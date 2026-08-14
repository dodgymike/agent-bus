package main

// Tests for `agent-bus peer add -peer-client-fingerprint`
// (RELAY-24-BLOCKER-PEERCERTFLAG).
//
// THE THING BEING PROVED, in one sentence: an operator running only the shipped
// binary can now produce a peer that `bindablePeerCount` counts, which before
// this flag was impossible for EVERY reachable configuration — no command wrote
// relay.BusTrustRecord.PeerClientTLSCertFingerprint, so the count was 0 always,
// and httpapi's mount refuses to register a peer surface that would answer 403
// to everyone. That is why the acceptance test below asserts a NUMBER GOING FROM
// 0 TO 1 rather than a field being set: the field is the mechanism, the count is
// the consequence, and only the count is what the mount guard reads.
//
// # Why the acceptance path goes through the COMPILED BINARY
//
// Invariant 7: the compiled CLI is THE client, and an operator or an agent
// reaches this flag through `agent-bus peer add`, never through
// runPeerCommand(). An in-process call would still pass if the flag were
// registered on a FlagSet the binary's dispatch never reaches. The guard tests
// below use the in-process helper deliberately — they are about validation
// decided before any I/O, where the extra fidelity buys nothing and the build
// cost would be paid many times over.
//
// # The direction-confusion guard
//
// -tls-fingerprint (OUTBOUND, on the ROUTE record, keyed to an ADDRESS) and
// -peer-client-fingerprint (INBOUND, on the TRUST record, keyed to a BUS
// PRINCIPAL) are different certificates in the general case. RELAY-41 and
// RELAY-45 both cost a task to the same confusion, so the acceptance test
// asserts not only that the value lands on the trust record but that NO route
// record carries it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// pccHopURL is an address that is never dialled; only its presence on a route
// record matters here.
const pccHopURL = "https://b.example:8443"

// buildAgentBusCLI compiles the REAL server binary into a temp dir and returns
// its path. Invariant 7: the acceptance path must be the artefact an operator
// runs, not a function an in-process test can reach.
func buildAgentBusCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agent-bus")
	// "." is this package: `go test` runs with the package directory as its
	// working directory, so no path juggling is needed and none is done.
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building the agent-bus binary failed: %v\n%s", err, out)
	}
	return bin
}

// runAgentBusCLI executes the compiled binary and returns its exit code and both
// streams. A failure to START the process is fatal; a non-zero EXIT is a result,
// because exit codes are the contract (invariant 7).
func runAgentBusCLI(t *testing.T, bin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %s %v: %v\nstderr: %s", bin, args, err, stderr.String())
		}
		return exitErr.ExitCode(), stdout.String(), stderr.String()
	}
	return 0, stdout.String(), stderr.String()
}

// pccOpenStore opens a replayed peer store over dir the way a starting server
// does — relay.NewPeerStore over the data directory, with the whole write-ahead
// log replayed into it before anything reads a table — by reusing the command's
// own read-only openPeerStore rather than re-implementing that sequence.
//
// The returned func RELEASES THE DIRECTORY LOCK and must be called before any
// further CLI invocation against the same directory: peer configuration is
// offline by design and the second writer would be refused at exit 3.
func pccOpenStore(t *testing.T, dir string) (*relay.PeerStore, func()) {
	t.Helper()
	store, release, _, cmdErr := openPeerStore(dir, false, logging.New(io.Discard, logging.LevelError))
	if cmdErr != nil {
		t.Fatalf("openPeerStore(%q): %v", dir, cmdErr)
	}
	return store, release
}

// pccTrustFor returns the LAST trust record on disk for busID, which is the
// generation that wins on replay, plus how many generations were written for it.
func pccTrustFor(t *testing.T, dir, busID string) (relay.BusTrustRecord, int) {
	t.Helper()
	_, trust := walPeerConfig(t, dir)
	var (
		last relay.BusTrustRecord
		n    int
	)
	for _, rec := range trust {
		if rec.BusID == busID {
			last = rec
			n++
		}
	}
	return last, n
}

// ---------------------------------------------------------------------------
// The acceptance test
// ---------------------------------------------------------------------------

// TestPeerAddBindsInboundClientCertFingerprint is the task's proof, and its name
// is referenced by the Spec Server's stored proof_cmd — do not rename it.
//
// It asserts, through the compiled binary and against the durable log:
//
//	(1) `peer add -peer-client-fingerprint` makes bindablePeerCount 1,
//	(2) the identical add WITHOUT the flag leaves it 0 (today's shipped
//	    behaviour, and the RED-before-fix half),
//	(3) the value lands on the TRUST record and on NO route record.
func TestPeerAddBindsInboundClientCertFingerprint(t *testing.T) {
	t.Parallel()

	const peerBus = "busB"
	bin := buildAgentBusCLI(t)
	fp, fpHex := peerTestCert(t, peerBus)
	_, keyB64 := newSigningKey(t)

	// --- (1) THE POSITIVE HALF -------------------------------------------
	boundDir, _ := initPeerDataDir(t)
	code, stdout, stderr := runAgentBusCLI(t, bin, "peer", "add",
		"-data-dir", boundDir,
		"-bus-id", peerBus,
		"-signing-key", keyB64,
		"-peer-client-fingerprint", fpHex,
		"-json")
	if code != exitPeerOK {
		t.Fatalf("peer add -peer-client-fingerprint exited %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
	}
	var res peerResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
	}

	boundStore, releaseBound := pccOpenStore(t, boundDir)
	if got := bindablePeerCount(boundStore); got != 1 {
		t.Fatalf("bindablePeerCount = %d after `peer add -peer-client-fingerprint`, want 1.\n"+
			"THIS NUMBER IS THE WHOLE TASK: httpapi refuses to mount /v1/peer/* while it is 0, because a registered surface that "+
			"authenticates nobody advertises federation while serving nobody. Trust records on disk: %+v",
			got, boundStore.TrustedBuses())
	}
	releaseBound()

	// --- (2) THE NEGATIVE HALF -------------------------------------------
	// The SAME command in a SEPARATE data directory with the flag removed and
	// nothing else changed. If this ever reports 1 the positive half above
	// proves nothing, because the count would not depend on the flag.
	unboundDir, _ := initPeerDataDir(t)
	code, stdout, stderr = runAgentBusCLI(t, bin, "peer", "add",
		"-data-dir", unboundDir,
		"-bus-id", peerBus,
		"-signing-key", keyB64,
		"-json")
	if code != exitPeerOK {
		t.Fatalf("peer add without -peer-client-fingerprint exited %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
	}
	unboundStore, releaseUnbound := pccOpenStore(t, unboundDir)
	if got := bindablePeerCount(unboundStore); got != 0 {
		t.Fatalf("bindablePeerCount = %d for a peer added WITHOUT -peer-client-fingerprint, want 0; "+
			"a trust record with pinned signing keys and no inbound binding can authenticate nobody", got)
	}
	releaseUnbound()

	// --- (3) THE DIRECTION-CONFUSION GUARD -------------------------------
	// A route record is added for the same bus so that the "no route carries
	// this value" assertion below has something to be false about. It is a
	// route-only add (no -signing-key), which writes no trust record and so
	// cannot disturb the binding.
	if code, stdout, stderr := runAgentBusCLI(t, bin, "peer", "add",
		"-data-dir", boundDir,
		"-bus-id", peerBus,
		"-url", pccHopURL); code != exitPeerOK {
		t.Fatalf("route-only add exited %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
	}

	routes, trust := walPeerConfig(t, boundDir)
	if len(trust) != 1 {
		t.Fatalf("the log holds %d trust records, want exactly 1: %+v", len(trust), trust)
	}
	if trust[0].BusID != peerBus {
		t.Fatalf("the trust record is about %q, want %q", trust[0].BusID, peerBus)
	}
	if trust[0].PeerClientTLSCertFingerprint != fp {
		t.Fatalf("the durable TRUST record binds %s, want the fingerprint passed on the command line (%s).\n"+
			"The INBOUND binding is what PeerStore.InboundPeerPrincipal resolves; nothing else on disk can stand in for it.",
			trust[0].PeerClientTLSCertFingerprint, fp)
	}
	if len(routes) != 1 {
		t.Fatalf("the log holds %d route records, want exactly 1: %+v", len(routes), routes)
	}
	for _, r := range routes {
		if r.NextHopTLSCertFingerprint == fp {
			t.Fatalf("route record %s carries the INBOUND client-certificate fingerprint in its OUTBOUND next-hop pin.\n"+
				"These are opposite directions and different certificates: next_hop_tls_cert_sha256 is what the bus at %q serves to US when WE dial IT; "+
				"the inbound binding is what %s presents AS A CLIENT when it dials us. Writing one into the other pins a credential that will never match.",
				r.BusID, r.BaseURL, peerBus)
		}
	}
}

// ---------------------------------------------------------------------------
// Guards — each fails ALONE
// ---------------------------------------------------------------------------

// TestPeerAddPeerClientFingerprintRefusesMalformedValues covers every spelling
// that must never reach disk. Each case gets its OWN data directory so that
// "nothing was written" can be asserted as "no write-ahead log exists at all",
// which is the strongest form of that claim available before the first write.
//
// The empty case is the important one and is not merely a completeness entry:
// `-peer-client-fingerprint "$PEER_FP"` with PEER_FP unset arrives here as an
// empty string, and reading it as "the flag was not given" would leave a peer
// unbound while reporting success.
//
// wantMsg, where set, pins the LENGTH refusal specifically. Without it the
// wrong-length cases are unfailable: relay.ParsePeerClientTLSFingerprint refuses
// a 63- or 65-character value on its own, so deleting the length pre-check
// changes the exit code not at all — mutation testing found exactly that. The
// length check earns its place by choosing the size of the diagnostic (an
// enormous argv must not get to decide how much we print), and the message is
// the only place that is observable.
func TestPeerAddPeerClientFingerprintRefusesMalformedValues(t *testing.T) {
	t.Parallel()

	const peerBus = "busB"
	_, validHex := peerTestCert(t, peerBus)

	cases := []struct {
		name    string
		hex     string
		wantMsg string
	}{
		{"empty — an unset shell variable, NOT an absent flag", "", "is 0 characters"},
		{"whitespace only", "   ", "is 0 characters"},
		{"uppercase — refused rather than normalised, so one certificate has one spelling", strings.ToUpper(validHex), ""},
		{"63 characters", validHex[:63], "is 63 characters"},
		{"65 characters", validHex + "0", "is 65 characters"},
		{"64 zeros — the record reads an all-zero digest as ABSENT, so accepting it would report a binding that is no binding", strings.Repeat("0", 64), ""},
		{"non-hex at the right length", strings.Repeat("z", 64), ""},
		{"a sha256: prefix", "sha256:" + validHex[7:], ""},
		{"colon-separated", strings.Repeat("ab:", 21) + "abc", "is 66 characters"},
		{"an enormous argv value, refused on LENGTH before anything decodes it", strings.Repeat("a", 100000), "is 100000 characters"},
	}

	_, keyB64 := newSigningKey(t)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, _ := initPeerDataDir(t)
			code, stdout, stderr := runPeer(t, "add",
				"-data-dir", dir,
				"-bus-id", peerBus,
				"-signing-key", keyB64,
				"-peer-client-fingerprint", tc.hex)
			if code != exitPeerUsage {
				t.Fatalf("-peer-client-fingerprint %q exited %d, want %d (usage)\nstdout: %s\nstderr: %s", tc.hex, code, exitPeerUsage, stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, wal.WALFileName)); !os.IsNotExist(err) {
				t.Fatalf("a refused add wrote a write-ahead log (stat err = %v); a value refused at exit 2 must be decided before anything is touched", err)
			}
			if tc.wantMsg != "" && !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("the refusal does not report the length (%q); a wrong-length value must be refused on its LENGTH, before its content is decoded, so an oversized argv cannot size the diagnostic:\n%s", tc.wantMsg, stderr)
			}
			// The refusal must not echo the argument back: it is argv on its way
			// to a terminal or a log, and its one relevant property is that it is
			// not a fingerprint.
			if strings.TrimSpace(tc.hex) != "" && strings.Contains(stderr, tc.hex) {
				t.Errorf("the refusal echoes the offending value (%d characters of it)", len(tc.hex))
			}
		})
	}
}

// TestPeerAddPeerClientFingerprintRequiresASigningKey: the flag alone would be
// accepted, report success and bind NOTHING, because applyPeerAdd writes a trust
// record only when -signing-key was given and an active record requires at least
// one pinned key. Refused at exit 2, before the directory lock.
//
// -url IS PASSED, and that is what makes this test able to fail at all. Without
// it the invocation installs neither a route nor a pin and is caught by
// validatePeerAdd's generic "this add would do nothing" refusal — also exit 2 —
// so the assertion would hold with the coupling check deleted. That version of
// this test was written first and mutation testing found it: it stayed GREEN
// with the check disabled. With -url the add is otherwise LEGITIMATE (it writes
// a route), so exit 2 can only come from the coupling check itself, and the
// specific refusal is named below rather than the exit code alone.
func TestPeerAddPeerClientFingerprintRequiresASigningKey(t *testing.T) {
	t.Parallel()

	const peerBus = "busB"
	_, fpHex := peerTestCert(t, peerBus)

	t.Run("with an otherwise legitimate route add", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, stdout, stderr := runPeer(t, "add",
			"-data-dir", dir,
			"-bus-id", peerBus,
			"-url", pccHopURL,
			"-peer-client-fingerprint", fpHex)
		if code != exitPeerUsage {
			t.Fatalf("-peer-client-fingerprint without -signing-key exited %d, want %d (usage)\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}
		if !strings.Contains(stderr, "without -signing-key") {
			t.Errorf("the refusal is not the -signing-key coupling refusal; some other check caught this and the coupling is untested:\n%s", stderr)
		}
		// NOTHING is written — not even the route the invocation would
		// otherwise have installed, because the refusal precedes the lock.
		if _, err := os.Stat(filepath.Join(dir, wal.WALFileName)); !os.IsNotExist(err) {
			t.Fatalf("the refusal wrote a write-ahead log (stat err = %v)", err)
		}
	})

	t.Run("on its own", func(t *testing.T) {
		t.Parallel()
		dir, _ := initPeerDataDir(t)
		code, stdout, stderr := runPeer(t, "add",
			"-data-dir", dir,
			"-bus-id", peerBus,
			"-peer-client-fingerprint", fpHex)
		if code != exitPeerUsage {
			t.Fatalf("-peer-client-fingerprint alone exited %d, want %d (usage)\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
		}
		if !strings.Contains(stderr, "-signing-key") {
			t.Errorf("the refusal does not name the flag that is missing:\n%s", stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, wal.WALFileName)); !os.IsNotExist(err) {
			t.Fatalf("the refusal wrote a write-ahead log (stat err = %v)", err)
		}
	})
}

// TestPeerAddNeverSilentlyErasesAnInboundBinding is the RELAY-45-FU-CLI
// regression test.
//
// A TRUST RECORD IS WRITTEN WHOLE, NEVER AS A DELTA. So a plain key rotation —
// `peer add -bus-id busB -signing-key <new>` — does not leave the inbound
// binding alone: it ERASES it, taking with it this bus's only way to resolve
// that peer's connections to a principal, and the pre-fix code reported the
// result as a successful write.
//
// The binding is asserted STILL PRESENT by reading the record back, not by
// trusting the exit code: a refusal that had already written would be the same
// defect with an error message attached.
func TestPeerAddNeverSilentlyErasesAnInboundBinding(t *testing.T) {
	t.Parallel()

	const peerBus = "busB"
	dir, _ := initPeerDataDir(t)
	fp, fpHex := peerTestCert(t, peerBus)
	_, firstKey := newSigningKey(t)

	if code, stdout, stderr := runPeer(t, "add",
		"-data-dir", dir,
		"-bus-id", peerBus,
		"-signing-key", firstKey,
		"-peer-client-fingerprint", fpHex); code != exitPeerOK {
		t.Fatalf("the first bind exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	_, generations := pccTrustFor(t, dir, peerBus)
	if generations != 1 {
		t.Fatalf("the first bind wrote %d trust generations, want 1", generations)
	}

	// The realistic accident: a key rotation run from a runbook written before
	// the flag existed. Both the same key and a rotated one must be refused —
	// the destructive field is the one NOT mentioned.
	_, rotatedKey := newSigningKey(t)
	for _, key := range []struct{ name, b64 string }{
		{"the same signing key", firstKey},
		{"a rotated signing key", rotatedKey},
	} {
		key := key
		t.Run("re-stating the trust record without the flag is refused: "+key.name, func(t *testing.T) {
			code, stdout, stderr := runPeer(t, "add",
				"-data-dir", dir,
				"-bus-id", peerBus,
				"-signing-key", key.b64)
			if code != exitPeerUsage {
				t.Fatalf("an add that would erase the inbound binding exited %d, want %d (usage)\nstdout: %s\nstderr: %s", code, exitPeerUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, "-peer-client-fingerprint") {
				t.Errorf("the refusal does not name the flag to re-state:\n%s", stderr)
			}

			// THE BINDING IS STILL ON DISK — read back, not inferred.
			rec, gens := pccTrustFor(t, dir, peerBus)
			if gens != 1 {
				t.Fatalf("the refused add wrote a new trust generation (%d on disk, want 1)", gens)
			}
			if rec.PeerClientTLSCertFingerprint != fp {
				t.Fatalf("the refused add erased the inbound binding: record binds %s, want %s", rec.PeerClientTLSCertFingerprint, fp)
			}
			store, release := pccOpenStore(t, dir)
			got := bindablePeerCount(store)
			release()
			if got != 1 {
				t.Fatalf("bindablePeerCount = %d after a REFUSED add, want 1; the refusal unbound the peer it refused to unbind", got)
			}
		})
	}
}

// TestPeerAddPeerClientFingerprintUnchangedReporting: `unchanged` must describe
// what actually happened on disk, on the one field that decides which inbound
// connections this bus can authenticate.
//
// The second half is the trustAlreadyPinned fix. A no-op predicate comparing
// only the signing keys folds a REBOUND certificate in as "already configured;
// nothing written" — telling the operator the opposite of the truth while the
// OLD certificate stays bound.
func TestPeerAddPeerClientFingerprintUnchangedReporting(t *testing.T) {
	t.Parallel()

	const peerBus = "busB"
	dir, _ := initPeerDataDir(t)
	_, firstHex := peerTestCert(t, peerBus)
	_, keyB64 := newSigningKey(t)

	code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", peerBus, "-signing-key", keyB64, "-peer-client-fingerprint", firstHex, "-json")
	if code != exitPeerOK {
		t.Fatalf("the first bind exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var first peerResult
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatalf("--json: %v\ngot: %s", err, stdout)
	}
	if len(first.Changes) != 1 || first.Changes[0].Kind != "trust" {
		t.Fatalf("the first bind reported %+v, want exactly one trust change", first.Changes)
	}
	firstSeq := first.Changes[0].ConfigSeq

	// (a) THE IDENTICAL RE-STATE: same keys, same fingerprint. Reported
	// unchanged, and no new generation reaches the log.
	code, stdout, stderr = runPeer(t, "add", "-data-dir", dir, "-bus-id", peerBus, "-signing-key", keyB64, "-peer-client-fingerprint", firstHex, "-json")
	if code != exitPeerOK {
		t.Fatalf("the identical re-state exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var same peerResult
	if err := json.Unmarshal([]byte(stdout), &same); err != nil {
		t.Fatalf("--json: %v\ngot: %s", err, stdout)
	}
	if len(same.Changes) != 1 || !same.Changes[0].Unchanged {
		t.Fatalf("re-stating an identical binding was reported as a fresh write: %+v", same.Changes)
	}
	if _, gens := pccTrustFor(t, dir, peerBus); gens != 1 {
		t.Fatalf("an identical re-state wrote a new trust generation (%d on disk, want 1)", gens)
	}

	// (b) A ROTATED CERTIFICATE AT AN UNCHANGED KEY SET. This one MUST write.
	rotatedFP, rotatedHex := peerTestCert(t, peerBus)
	code, stdout, stderr = runPeer(t, "add", "-data-dir", dir, "-bus-id", peerBus, "-signing-key", keyB64, "-peer-client-fingerprint", rotatedHex, "-json")
	if code != exitPeerOK {
		t.Fatalf("re-binding a rotated certificate exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rotated peerResult
	if err := json.Unmarshal([]byte(stdout), &rotated); err != nil {
		t.Fatalf("--json: %v\ngot: %s", err, stdout)
	}
	if len(rotated.Changes) != 1 {
		t.Fatalf("the rotation reported %+v, want exactly one change", rotated.Changes)
	}
	if rotated.Changes[0].Unchanged {
		t.Fatalf("re-binding a ROTATED inbound certificate was reported as unchanged; the operator is told nothing happened while the OLD certificate stays bound")
	}
	if rotated.Changes[0].ConfigSeq <= firstSeq {
		t.Fatalf("the rotation reports config_seq %d, want more than the first generation's %d", rotated.Changes[0].ConfigSeq, firstSeq)
	}
	rec, gens := pccTrustFor(t, dir, peerBus)
	if gens != 2 {
		t.Fatalf("the log holds %d trust generations for %s, want 2 (the original and the rotation)", gens, peerBus)
	}
	if rec.PeerClientTLSCertFingerprint != rotatedFP {
		t.Fatalf("after rotation the durable record binds %s, want %s", rec.PeerClientTLSCertFingerprint, rotatedFP)
	}
	if rec.ConfigSeq <= firstSeq {
		t.Fatalf("the durable rotation carries config_seq %d, want more than %d", rec.ConfigSeq, firstSeq)
	}
}

// TestPeerAddPeerClientFingerprintIsUniqueAcrossBuses: one inbound certificate
// names exactly ONE bus principal. Binding it to a second bus is refused by the
// store under the same lock as the write, so nothing is written — and the
// refusal is exit 1, not exit 2, because it cannot be decided from the command
// line: it depends on what some OTHER bus's record already binds.
func TestPeerAddPeerClientFingerprintIsUniqueAcrossBuses(t *testing.T) {
	t.Parallel()

	const (
		busB = "busB"
		busC = "busC"
	)
	dir, _ := initPeerDataDir(t)
	_, fpHex := peerTestCert(t, busB)
	_, keyB := newSigningKey(t)
	_, keyC := newSigningKey(t)

	if code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", busB, "-signing-key", keyB, "-peer-client-fingerprint", fpHex); code != exitPeerOK {
		t.Fatalf("binding %s exited %d\nstdout: %s\nstderr: %s", busB, code, stdout, stderr)
	}

	code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", busC, "-signing-key", keyC, "-peer-client-fingerprint", fpHex)
	if code != exitPeerFailed {
		t.Fatalf("binding one certificate to a second bus exited %d, want %d (failed)\nstdout: %s\nstderr: %s", code, exitPeerFailed, stdout, stderr)
	}
	// The store's own refusal must reach the operator, not be flattened into a
	// generic write failure: it is the sentence that explains the constraint.
	if !strings.Contains(stderr, relay.ErrPeerClientCertAlreadyBound.Error()) {
		t.Errorf("the refusal does not surface relay.ErrPeerClientCertAlreadyBound (%q):\n%s", relay.ErrPeerClientCertAlreadyBound, stderr)
	}
	if !strings.Contains(stderr, busB) {
		t.Errorf("the refusal does not name the bus that already holds the certificate (%s):\n%s", busB, stderr)
	}

	// NOTHING WAS WRITTEN FOR busC — the check runs under the write lock.
	if _, gens := pccTrustFor(t, dir, busC); gens != 0 {
		t.Fatalf("the refused add wrote %d trust generations for %s, want 0", gens, busC)
	}
	for _, tr := range listPeers(t, dir).Trust {
		if tr.BusID == busC {
			t.Fatalf("`peer list` reports a trust record for %s after a refused add: %+v", busC, tr)
		}
	}
	store, release := pccOpenStore(t, dir)
	got := bindablePeerCount(store)
	release()
	if got != 1 {
		t.Fatalf("bindablePeerCount = %d, want 1 (only %s holds the certificate)", got, busB)
	}
}

// TestPeerListReportsTheInboundClientCertBinding round-trips the value through
// the CLI's OWN reader, which is how an operator and a provisioning script read
// it back.
//
// The key matters as much as the value: `peer_client_tls_cert_sha256` on the
// TRUST entry and NEVER `next_hop_tls_cert_sha256`, because a consumer reading
// the inbound binding out of the outbound key would conclude this bus had pinned
// a certificate it has not pinned, in a direction it does not apply to.
func TestPeerListReportsTheInboundClientCertBinding(t *testing.T) {
	t.Parallel()

	const (
		boundBus   = "busB"
		unboundBus = "busC"
	)
	dir, _ := initPeerDataDir(t)
	_, fpHex := peerTestCert(t, boundBus)
	_, hopFPHex := peerTestCert(t, boundBus)
	_, keyB := newSigningKey(t)
	_, keyC := newSigningKey(t)

	if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", boundBus,
		"-url", pccHopURL, "-tls-fingerprint", hopFPHex,
		"-signing-key", keyB, "-peer-client-fingerprint", fpHex); code != exitPeerOK {
		t.Fatalf("the bound add exited %d\nstderr: %s", code, stderr)
	}
	// A peer with a trust record and NO inbound binding: the absence must be
	// stated, not left blank.
	if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", unboundBus, "-signing-key", keyC); code != exitPeerOK {
		t.Fatalf("the unbound add exited %d\nstderr: %s", code, stderr)
	}

	out := listPeers(t, dir)
	var seenBound, seenUnbound bool
	for _, tr := range out.Trust {
		switch tr.BusID {
		case boundBus:
			seenBound = true
			if tr.PeerClientTLSCertFingerprint != fpHex {
				t.Errorf("`peer list --json` reports peer_client_tls_cert_sha256 = %q for %s, want %q", tr.PeerClientTLSCertFingerprint, boundBus, fpHex)
			}
			if tr.NextHopTLSCertFingerprint != "" {
				t.Errorf("the TRUST entry for %s carries next_hop_tls_cert_sha256 = %q; a trust record describes a bus principal, not an address", boundBus, tr.NextHopTLSCertFingerprint)
			}
		case unboundBus:
			seenUnbound = true
			if tr.PeerClientTLSCertFingerprint != "" {
				t.Errorf("an unbound peer (%s) reports a binding: %q", unboundBus, tr.PeerClientTLSCertFingerprint)
			}
		}
	}
	if !seenBound || !seenUnbound {
		t.Fatalf("`peer list --json` is missing a trust entry: %+v", out.Trust)
	}
	for _, r := range out.Routes {
		if r.PeerClientTLSCertFingerprint != "" {
			t.Errorf("ROUTE entry %s reports an inbound client-certificate binding (%q); that field belongs to the trust record only", r.BusID, r.PeerClientTLSCertFingerprint)
		}
		if r.NextHopTLSCertFingerprint == fpHex {
			t.Errorf("route %s pins the INBOUND client certificate as its OUTBOUND next-hop certificate; these are opposite directions", r.BusID)
		}
	}

	// The human rendering: the binding for the one that has it, and an explicit
	// statement of ABSENCE for the one that does not — an operator must see that
	// at a glance rather than infer it from a missing line.
	code, human, stderr := runPeer(t, "list", "-data-dir", dir)
	if code != exitPeerOK {
		t.Fatalf("`peer list` exited %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(human, "inbound client certificate bound: "+fpHex) {
		t.Errorf("human `peer list` does not show the inbound binding:\n%s", human)
	}
	if !strings.Contains(human, "no inbound client certificate bound") {
		t.Errorf("human `peer list` does not state the ABSENCE of a binding for the unbound peer:\n%s", human)
	}
}

// TestPeerAddRouteOnlyLeavesAnInboundBindingAlone proves the erase check's
// keysGiven gate is not over-broad.
//
// A route-only add writes no trust record and therefore erases no binding, so
// refusing it would be a refusal an operator could not act on: the remedy
// requires -signing-key, which such an invocation does not carry. It must
// SUCCEED, and the binding must survive it untouched.
func TestPeerAddRouteOnlyLeavesAnInboundBindingAlone(t *testing.T) {
	t.Parallel()

	const peerBus = "busB"
	dir, _ := initPeerDataDir(t)
	fp, fpHex := peerTestCert(t, peerBus)
	_, keyB64 := newSigningKey(t)

	if code, _, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", peerBus, "-signing-key", keyB64, "-peer-client-fingerprint", fpHex); code != exitPeerOK {
		t.Fatalf("the bind exited %d\nstderr: %s", code, stderr)
	}

	code, stdout, stderr := runPeer(t, "add", "-data-dir", dir, "-bus-id", peerBus, "-url", pccHopURL, "-json")
	if code != exitPeerOK {
		t.Fatalf("a route-only add against a bound peer exited %d, want %d\nstdout: %s\nstderr: %s", code, exitPeerOK, stdout, stderr)
	}

	rec, gens := pccTrustFor(t, dir, peerBus)
	if gens != 1 {
		t.Fatalf("the route-only add rewrote the trust record (%d generations on disk, want 1)", gens)
	}
	if rec.PeerClientTLSCertFingerprint != fp {
		t.Fatalf("the route-only add disturbed the inbound binding: record binds %s, want %s", rec.PeerClientTLSCertFingerprint, fp)
	}
	store, release := pccOpenStore(t, dir)
	got := bindablePeerCount(store)
	release()
	if got != 1 {
		t.Fatalf("bindablePeerCount = %d after a route-only add, want 1", got)
	}
}
