package main

// The BEHAVIOURAL proof for the forged-message-seq-floor finding: a data
// directory holding a `message-seq-floor` that claims 2^64-1 must be REFUSED at
// startup, loudly and with a remedy, instead of being adopted into a bus that
// boots perfectly healthy and then 500s every single send forever.
//
// # The exploit this pins, as it was reported and reproduced
//
// The floor file's digest is an UNKEYED SHA-256 over its one-line body, so
// anyone who can write the data directory can compute it. Note what that
// requires: DIRECTORY write, not file write. Replacing a file is unlink+create
// or rename, both of which are permissions on the containing directory, so the
// 0600 on `message-seq-floor` itself protects nothing here.
//
// Write `floor 18446744073709551615` with a matching digest and the bus starts
// CLEAN -- /healthz ok, the roster intact, the log replayed, not one warning --
// and then every POST /v1/mint returns 500 "ids: sequence exhausted", forever,
// across every restart, because the file persists. A bus that enrols, issues
// sessions and cannot deliver a single message is a worse outcome than one that
// refuses to start and says why in one line.
//
// It is a WHOLE-PROCESS test on purpose. The unit tests in internal/hub prove
// readSeqFloorFile rejects the value; only starting the real binary over a real
// forged directory proves the refusal is actually WIRED -- that hub.Open's error
// reaches main() and becomes exit 1, rather than being logged and shrugged off.
// That distinction is exactly the one AUTH-7 was landed to close, where every
// unit test stayed green while cmd/agent-bus built its Service with no roster.
//
// The fixture writes the file as LITERAL BYTES rather than through the
// production encoder. That is deliberate: this is an attacker's file, not ours,
// and the whole claim is that bytes an attacker can trivially produce are
// refused. Reusing the encoder would make the test agree with the code by
// construction, and would go green if the format silently changed underneath it.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
)

// seedSeqFloorFile writes a LEGITIMATE, honest floor file into dataDir, making
// it look like a directory a previous run of THIS binary left behind.
//
// # Why fixtures need it, and why this is the right shape of fixture
//
// A data directory with NO floor file whose log recovery had to REPAIR is
// refused at startup: the log is the only remaining source for the floor and it
// has just been proven incomplete, so rebuilding from it would reissue sequence
// numbers already handed out (see hub.ErrSeqFloorUnprovable). Every
// corrupt-log fixture in this package builds exactly that shape -- damage a log
// inside an otherwise-fresh directory -- so all of them trip the new guard.
//
// The fix is this seed and NOT a weakening of the guard, for precisely the
// reason seedSuffixFloorsFile gives about the identity guard it mirrors: a test
// that disarmed the guard would no longer be running the code path a real
// restart runs, and would stay green if the guard were deleted outright.
// Seeding leaves it fully ARMED and makes the corrupt LOG the only damage
// present, which is what those tests were always about. The guard's own refusal
// is pinned separately by TestMissingSeqFloorWithADamagedLogRefusesToStart.
//
// A floor of ZERO is the honest content, and the precondition is CHECKED below
// rather than left as prose: these directories' logs hold 64 bytes of "X" and no
// message or mint has ever happened on them, so "no sequence has ever been
// burned" is simply true. Seeding a zero floor onto a directory that HAS minted
// would assert something false and let a test go green over the very reissue
// this mechanism prevents.
func seedSeqFloorFile(t *testing.T, dataDir string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dataDir, hub.SeqFloorFileName)); err == nil {
		t.Fatalf("seedSeqFloorFile(%q) was called on a dir that already has a %s; seeding over it would hide whatever it says", dataDir, hub.SeqFloorFileName)
	}
	return writeSeqFloorFileBytes(t, dataDir, 0)
}

// forgeSeqFloorFile writes a message-seq-floor claiming floor, with a VALID
// unkeyed digest, into dataDir -- the one-line Python an attacker writes.
func forgeSeqFloorFile(t *testing.T, dataDir string, floor uint64) string {
	t.Helper()
	return writeSeqFloorFileBytes(t, dataDir, floor)
}

// writeSeqFloorFileBytes builds the file as LITERAL BYTES rather than through
// the production encoder, which is unexported anyway.
//
// That is deliberate for the FORGERY -- the claim is that bytes an attacker can
// trivially produce are refused, and reusing the encoder would make the test
// agree with the code by construction. The risk it carries for the SEED is that
// this literal drifts out of the real format; that is covered, because
// TestForgedSeqFloorIsRefusedRatherThanBrickingEverySend reads back the file a
// genuinely working bus wrote and asserts it has exactly this shape.
func writeSeqFloorFileBytes(t *testing.T, dataDir string, floor uint64) string {
	t.Helper()
	body := fmt.Sprintf("floor %d\n", floor)
	sum := sha256.Sum256([]byte(body))
	// v5 is the on-disk format version the current binary accepts. If this ever
	// stops matching, the test starts proving "an unknown version is refused",
	// which is a DIFFERENT and much weaker claim -- so the caller asserts the
	// shape against a file a genuinely working bus wrote.
	data := fmt.Sprintf("agent-bus-message-seq-floor v5 sha256=%s\n%s", hex.EncodeToString(sum[:]), body)
	path := filepath.Join(dataDir, hub.SeqFloorFileName)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("forging %s: %v", path, err)
	}
	return path
}

// exitedWithin reports whether the child has exited inside bound, and its code
// if it has.
//
// serverProc.awaitExit cannot be used here: it t.Fatalf's when the process is
// still running, and "still running" is precisely the outcome this test has to
// INSPECT rather than abort on -- a bus that came up on the forged floor is the
// finding, and the test needs to reach it to report the 500.
func exitedWithin(p *serverProc, bound time.Duration) (bool, int) {
	select {
	case <-p.done:
	case <-time.After(bound):
		return false, 0
	}
	if p.waitErr == nil {
		return true, 0
	}
	var ee *exec.ExitError
	if errors.As(p.waitErr, &ee) {
		return true, ee.ExitCode()
	}
	return true, -1
}

// TestForgedSeqFloorIsRefusedRatherThanBrickingEverySend runs the exploit
// end-to-end: a working bus, a forged floor, a restart.
func TestForgedSeqFloorIsRefusedRatherThanBrickingEverySend(t *testing.T) {
	dir := t.TempDir()

	// ---- 1. A bus that demonstrably WORKS, so the brick below is attributable
	// to the forgery and not to a broken fixture.
	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	agent := enrolNewAgent(t, dir, addr, "forge-victim")
	agent.authenticate(t, dir, addr)

	mustPostJSON(t, dir, addr, "/v1/mint", agent.token, map[string]string{
		"op":              "send",
		"idempotency_key": fmt.Sprintf("mint-before-forgery-%d", time.Now().UnixNano()),
	}, http.StatusCreated)

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}

	// The legitimate file the working bus just wrote. Reading it pins the
	// premise of the forgery: the format is v5 and the digest is unkeyed, so
	// the attacker's bytes below are indistinguishable from a genuine file.
	floorPath := filepath.Join(dir, hub.SeqFloorFileName)
	genuine, err := os.ReadFile(floorPath)
	if err != nil {
		t.Fatalf("reading the floor file a working bus wrote (%s): %v", floorPath, err)
	}
	if !strings.HasPrefix(string(genuine), "agent-bus-message-seq-floor v5 sha256=") {
		t.Fatalf("the floor file a working bus wrote is not the v5/sha256 shape this test forges:\n%s", genuine)
	}

	// ---- 2. THE FORGERY. No key, no privilege beyond write on the directory.
	forgeSeqFloorFile(t, dir, math.MaxUint64)

	// ---- 3. The restart. It must REFUSE.
	proc2 := startServer(t, dir)
	exited, code := exitedWithin(proc2, startupTimeout)
	if !exited {
		// The forgery was ADOPTED. Spell out the consequence rather than just
		// failing, because the consequence IS the finding: a healthy-looking bus
		// that cannot send.
		addr2 := proc2.awaitServerStarted(t)
		agent.authenticate(t, dir, addr2)
		status, body := postJSONTo(t, dir, addr2, "/v1/mint", agent.token, map[string]string{
			"op":              "send",
			"idempotency_key": fmt.Sprintf("mint-after-forgery-%d", time.Now().UnixNano()),
		})
		t.Fatalf("a message-seq-floor forged to %d (valid unkeyed digest, no key needed) was ACCEPTED: "+
			"the bus started HEALTHY and POST /v1/mint returned %d: %s\n"+
			"Every send on this data directory now fails forever, across every restart, because the file persists.\n"+
			"Startup must refuse an implausibly high floor as corrupt-or-tampered and name the remedy.\nstderr:\n%s",
			uint64(math.MaxUint64), status, body, proc2.stderr())
	}
	if code != 1 {
		t.Fatalf("exit code on a forged floor = %d, want 1\n%s", code, proc2.stderr())
	}

	// ---- 4. The refusal has to be USABLE: it must name the file, say the value
	// is implausible rather than merely "corrupt", and carry the remedy. An
	// operator who gets exit 1 and no diagnosis is only marginally better off
	// than one who gets a silently bricked bus.
	stderr := proc2.stderr()
	for _, want := range []string{
		hub.SeqFloorFileName,   // WHICH file
		"18446744073709551615", // the value it claimed
		"implausibly high",     // WHY -- not a checksum failure, not bit-rot
		"TAMPERED WITH",        // that this is an attack shape
		"move " + floorPath,    // the one-step remedy, with the real path
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the refusal does not mention %q; an operator cannot act on it.\nstderr:\n%s", want, stderr)
		}
	}
}
