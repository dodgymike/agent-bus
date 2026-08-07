package main

// The BEHAVIOURAL acceptance proof for MSG-FU-SUFFIXFLOOR: a REAL server,
// against a REAL data dir, restarted, must mint a STRICTLY GREATER agent id
// suffix for a re-enrolled name.
//
// This is deliberately not a unit test of the allocator. The defect being
// guarded was never in the allocator -- ids.DurableNameSuffixes was landed and
// tested first -- it was that cmd/agent-bus/main.go never CONSTRUCTED it, so
// every unit test in internal/ids stayed green while a restarted bus re-minted
// live agent ids. Only a test that starts the process, enrols through
// POST /v1/enroll and restarts can tell those two states apart.
//
// The harness (startServer, awaitServerStarted, signal, awaitExit) is shared
// with wal_startup_test.go; nothing in main.go is shaped to support it.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// suffixFileInDataDir is the floors file ids.DurableNameSuffixes persists to.
// Spelled out here rather than imported because ids does not export the name,
// and a test that asserts on the wrong path would silently prove nothing.
const suffixFileInDataDir = "agent-suffixes"

// enrolAgent enrols name through the real HTTP route and returns the
// server-minted, fully-qualified agent id.
//
// A FRESH keypair every call, on purpose: that is the whole hazard. A reused
// suffix hands a DIFFERENT keypair the previous holder's identity, so the test
// must not accidentally present the same key twice and make a reuse look benign.
//
// dataDir names the bus: since MTLS-LISTENER the enrol route is https, verified
// against that directory's certificate (busTestClient, tlsclient_test.go).
func enrolAgent(t *testing.T, dataDir, addr, name string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating an Ed25519 keypair: %v", err)
	}
	reqBody, err := json.Marshal(map[string]string{
		"name":       name,
		"public_key": base64.StdEncoding.EncodeToString(pub),
		// Unique per call: a repeated key would be an idempotent REPLAY and
		// would return the ORIGINAL id, which would make this test pass for
		// entirely the wrong reason.
		"idempotency_key": fmt.Sprintf("enrol-%s-%d", name, time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("marshalling the enrol request: %v", err)
	}

	resp, err := busTestClient(t, dataDir).Post(busURL(addr, "/v1/enroll"), "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST %s: %v", busURL(addr, "/v1/enroll"), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading the enrol response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/enroll status = %d, want 201; body: %s", resp.StatusCode, body)
	}
	var out struct {
		AgentID string `json:"agent_id"`
		BusID   string `json:"bus_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding the enrol response %s: %v", body, err)
	}
	if out.AgentID == "" {
		t.Fatalf("enrol response carries no agent_id: %s", body)
	}
	return out.AgentID
}

// suffixOf parses the suffix out of a fully-qualified agent id.
func suffixOf(t *testing.T, agentID string) uint64 {
	t.Helper()
	_, _, n, err := ids.ParseAgentID(agentID)
	if err != nil {
		t.Fatalf("parsing the minted agent id %q: %v", agentID, err)
	}
	return n
}

// busIDOf reads the bus id out of the "server started" line.
func busIDOf(t *testing.T, p *serverProc) string {
	t.Helper()
	busID := parseLogfmt(p.line(t, msgServerStarted))["bus_id"]
	if busID == "" {
		t.Fatalf("the %q line carries no bus_id", msgServerStarted)
	}
	return busID
}

// TestRestartMintsStrictlyGreaterAgentIDSuffix is the acceptance bar named in
// the task: "do not close this task until a running server, restarted, provably
// mints a strictly greater suffix for a re-enrolled name."
//
// It covers BOTH restart shapes. A clean SIGTERM is the easy one; the kill -9 is
// the one that matters, because it is what proves the floor was durable BEFORE
// the suffix was issued rather than flushed on the way out.
func TestRestartMintsStrictlyGreaterAgentIDSuffix(t *testing.T) {
	dir := t.TempDir()
	const name = "alpha"

	// --- start 1: a fresh data dir ---
	p1 := startServer(t, dir)
	addr1 := p1.awaitServerStarted(t)
	id1 := enrolAgent(t, dir, addr1, name)
	n1 := suffixOf(t, id1)
	if n1 != 1 {
		t.Fatalf("first enrolment of %q on a FRESH data dir minted %q (suffix %d), want suffix 1", name, id1, n1)
	}
	// The floor was written before the id was issued, so the file exists now --
	// not at shutdown.
	floorsPath := filepath.Join(dir, suffixFileInDataDir)
	if _, err := os.Stat(floorsPath); err != nil {
		t.Fatalf("stat %q after the first enrolment: %v; the floor must be persisted BEFORE the suffix is issued", floorsPath, err)
	}

	p1.signal(t, syscall.SIGTERM)
	if code := p1.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("clean shutdown exited %d, want 0\n%s", code, p1.stderr())
	}

	// --- start 2: same data dir, clean restart ---
	p2 := startServer(t, dir)
	addr2 := p2.awaitServerStarted(t)
	id2 := enrolAgent(t, dir, addr2, name)
	n2 := suffixOf(t, id2)
	if n2 <= n1 {
		t.Fatalf("after a CLEAN restart, re-enrolling %q minted %q (suffix %d), want STRICTLY GREATER than %d (%q).\nAn agent id is never reused, including across restarts (invariant 1): a reused id hands a new agent holding a different keypair the previous holder's routing and authorization identity.\n%s",
			name, id2, n2, n1, id1, p2.stderr())
	}

	// --- start 3: same data dir, after kill -9 ---
	//
	// No graceful shutdown, no deferred flush, no chance to write anything on
	// the way out. Whatever the next start resumes above must already have been
	// on disk at the moment the id was issued.
	p2.signal(t, syscall.SIGKILL)
	p2.awaitExit(t, shutdownTimeout)

	p3 := startServer(t, dir)
	addr3 := p3.awaitServerStarted(t)
	id3 := enrolAgent(t, dir, addr3, name)
	n3 := suffixOf(t, id3)
	if n3 <= n2 {
		t.Fatalf("after a KILL -9 restart, re-enrolling %q minted %q (suffix %d), want STRICTLY GREATER than %d (%q).\nThe floor must be fsynced BEFORE the suffix is issued, so a process that dies without warning still cannot reissue it.\n%s",
			name, id3, n3, n2, id2, p3.stderr())
	}

	// A DIFFERENT name is unaffected: the counters are per name, and the seal
	// asserts that names absent from the floors were never written.
	if n := suffixOf(t, enrolAgent(t, dir, addr3, "beta")); n != 1 {
		t.Fatalf("first enrolment of \"beta\" minted suffix %d, want 1: per-name counters are independent", n)
	}
}

// TestLegacyDataDirDoesNotReMintAgentIDs is the migration proof: a data dir
// written by the SHIPPED binary has agent ids inside WAL message records and NO
// agent-suffixes file. Sealing the empty floor map that dir yields would assert
// "no suffix was ever written", which is exactly the false claim that re-mints
// live ids.
func TestLegacyDataDirDoesNotReMintAgentIDs(t *testing.T) {
	dir := t.TempDir()
	const name = "alpha"

	// Start once so the dir has the bus id (and the WAL's MAC key) a real dir
	// has, and so the test learns the bus id the ids must be qualified with.
	p1 := startServer(t, dir)
	p1.awaitServerStarted(t)
	busID := busIDOf(t, p1)
	p1.signal(t, syscall.SIGTERM)
	if code := p1.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("shutdown exited %d, want 0\n%s", code, p1.stderr())
	}

	// Now make it LEGACY: agent ids durable in message records, no floors file.
	l := openTestWAL(t, dir)
	writeMessageRecord(t, l, busID, busID+"."+name+"-5", []string{busID + ".beta-3"}, 1)
	if err := l.Close(); err != nil {
		t.Fatalf("closing the fixture WAL: %v", err)
	}
	floorsPath := filepath.Join(dir, suffixFileInDataDir)
	if err := os.Remove(floorsPath); err != nil {
		t.Fatalf("removing %q to simulate a data dir that predates the floors file: %v", floorsPath, err)
	}

	// --- start against the legacy dir ---
	//
	// -backfill-suffix-floors is the operator's one-time migration opt-in, and it
	// is REQUIRED here: since AUTH-3-FU-FAILOPEN a data dir with history and no
	// floors file is refused rather than backfilled silently, precisely because
	// the same shape is what a DELETED floors file looks like. This test is the
	// legacy MIGRATION, so it opts in; that the same dir without the flag refuses
	// is a separate assertion.
	p2 := startServerArgs(t, dir, "-backfill-suffix-floors")
	addr2 := p2.awaitServerStarted(t)

	// The migration is LOUD, in the real operator log, at ERROR: this dir has
	// history and had no floors file, so any id it issued that no record names
	// can still be re-minted on this one start. A silent backfill was the
	// security gate's M2.
	floorsLine := p2.line(t, "WITHOUT a persisted floors file")
	if lvl := parseLogfmt(floorsLine)["level"]; lvl != "error" {
		t.Fatalf("the backfill line is level=%q, want %q; a data dir with history and no floors file is not routine news\nline: %s", lvl, "error", floorsLine)
	}

	if n := suffixOf(t, enrolAgent(t, dir, addr2, name)); n <= 5 {
		t.Fatalf("on a LEGACY data dir, enrolling %q minted suffix %d, want strictly greater than 5: %s.%s-5 is already durable in a WAL message record, and re-minting it hands a new keypair that agent's identity.\n%s",
			name, n, busID, name, p2.stderr())
	}
	if n := suffixOf(t, enrolAgent(t, dir, addr2, "beta")); n <= 3 {
		t.Fatalf("on a LEGACY data dir, enrolling \"beta\" minted suffix %d, want strictly greater than 3: a RECIPIENT id is as durable as a sender id.\n%s", n, p2.stderr())
	}
	// A name with no history on that dir still starts at 1.
	if n := suffixOf(t, enrolAgent(t, dir, addr2, "gamma")); n != 1 {
		t.Fatalf("enrolling \"gamma\" on the legacy dir minted suffix %d, want 1: the seal asserts that names absent from the derivation were never written", n)
	}
}

// TestServerRefusesToStartWithHistoryAndNoSuffixFloors is the PIN ON THE GUARD
// ITSELF (AUTH-3-FU-FAILOPEN), and it is the assertion
// TestLegacyDataDirDoesNotReMintAgentIDs above defers to when it says "that the
// same dir without the flag refuses is a separate assertion".
//
// It matters more than its size suggests. Every other test in this package
// arranges NOT to trip the guard -- by seeding a floors file, or by taking the
// -backfill-suffix-floors opt-in -- so without this test the guard could be
// deleted outright and the whole suite would stay green. That is precisely how
// a superseded decision gets restored by someone "making a failing test pass":
// the four tests this fix broke all looked like the guard was the bug.
//
// The shape being refused: a dir that HAS history and has NO agent-suffixes
// file. It is what a legacy dir looks like, and it is equally what a DELETED
// floors file looks like, and nothing on disk tells those apart -- so the server
// asks rather than guesses. The information lives in the operator's head.
func TestServerRefusesToStartWithHistoryAndNoSuffixFloors(t *testing.T) {
	dir := t.TempDir()

	// A real, ordinary data dir: started once, so it holds a bus id, a WAL, a
	// MAC key and a correct floors file.
	p1 := startServer(t, dir)
	p1.awaitServerStarted(t)
	p1.signal(t, syscall.SIGTERM)
	if code := p1.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("first shutdown exited %d, want 0\n%s", code, p1.stderr())
	}

	floorsPath := filepath.Join(dir, suffixFileInDataDir)
	if _, err := os.Stat(floorsPath); err != nil {
		t.Fatalf("a completed first start left no %q: %v; the rest of this test would be vacuous", suffixFileInDataDir, err)
	}
	// THE LOSS. Only the floors file goes -- bus id, log and MAC key all stay
	// intact, which is what makes this indistinguishable from a legacy dir and
	// is exactly the case the guard exists for.
	if err := os.Remove(floorsPath); err != nil {
		t.Fatalf("removing %q: %v", floorsPath, err)
	}

	// --- restart with NO opt-in: must REFUSE ---
	p2 := startServer(t, dir)
	if code := p2.awaitExit(t, startupTimeout); code != 1 {
		t.Fatalf("server exited %d on a data dir with history and no %q, want 1: resuming every agent name from suffix 1 would re-mint agent ids that are live (invariant 1)\n%s",
			code, suffixFileInDataDir, p2.stderr())
	}
	out := p2.stderr()
	// It must NOT have served. A server that refuses after binding has already
	// answered from an allocator it could not prove.
	for _, line := range p2.snapshot() {
		if strings.Contains(line, msgServerStarted) {
			t.Fatalf("the server logged %q despite an unprovable agent-id floors file\n%s", msgServerStarted, out)
		}
	}
	// The refusal has to be ACTIONABLE, not merely correct: an operator woken by
	// it needs to know what is wrong and what the two ways out are. This is the
	// only thing standing between a lost floors file and a silent identity
	// takeover, so the wording is contract, not decoration.
	for _, want := range []string{
		"has HISTORY",             // what is wrong
		suffixFileInDataDir,       // which file
		"restore",                 // remedy 1: put it back
		"-backfill-suffix-floors", // remedy 2: the one-time opt-in
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q; an operator cannot act on it\n%s", want, out)
		}
	}

	// --- and the outage is RECOVERABLE IN ONE RESTART ---
	//
	// Load-bearing for the whole fail-closed argument: refusing to boot is only
	// defensible because the operator has a way forward that costs one restart.
	// A guard that could not be cleared would be a permanent outage, and this is
	// what makes the difference observable rather than asserted in a comment.
	p3 := startServerArgs(t, dir, "-backfill-suffix-floors")
	p3.awaitServerStarted(t)
	p3.signal(t, syscall.SIGTERM)
	if code := p3.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("the -backfill-suffix-floors restart exited %d, want 0; the refusal must be clearable in a single restart\n%s", code, p3.stderr())
	}
}

// TestFreshDataDirStartsWithoutTheBackfillOptIn pins the OTHER side of the guard
// above: it must NOT fire on a genuinely fresh data directory. That is the
// common case -- every bus's first start -- and a guard that caught it would
// make a new bus unstartable without an opt-in flag whose own documentation says
// it is "never needed in normal operation".
//
// The discriminator is DIRECTORY EMPTINESS (run() reads it before dirlock
// writes bus.lock, which is the last instant the answer is knowable), so this
// test is also the pin on that ordering: hoist the emptiness probe below the
// lock, or below wal.Open, and every fresh start starts refusing. No unit test
// sees that, because the flag is a parameter there and a real one only at
// process level.
//
// The second start is as load-bearing as the first. It proves the migration
// window really does close on its own: the first start WRITES the floors file,
// so the ordinary restart that follows is the steady state and needs no opt-in
// either. If it did, the guard would have made every bus permanently dependent
// on a migration flag.
func TestFreshDataDirStartsWithoutTheBackfillOptIn(t *testing.T) {
	dir := t.TempDir() // empty: no bus id, no log, no floors file

	p1 := startServer(t, dir) // deliberately NO -backfill-suffix-floors
	addr1 := p1.awaitServerStarted(t)

	// It is a usable bus, not just a process that logged a banner: enrolling
	// requires a SEALED allocator (an unsealed one refuses every NextSuffix with
	// ErrFloorUnproven), so this proves the floors were proven, not bypassed.
	if n := suffixOf(t, enrolAgent(t, dir, addr1, "alpha")); n != 1 {
		t.Fatalf("first enrolment on a fresh dir minted suffix %d, want 1", n)
	}

	p1.signal(t, syscall.SIGTERM)
	if code := p1.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("first shutdown exited %d, want 0\n%s", code, p1.stderr())
	}

	// The first start must have PERSISTED the floors, or the restart below would
	// be the refusal case rather than the steady state.
	if _, err := os.Stat(filepath.Join(dir, suffixFileInDataDir)); err != nil {
		t.Fatalf("a fresh start left no %q: %v; without it every subsequent start of this dir refuses", suffixFileInDataDir, err)
	}

	// --- the ordinary restart: still no opt-in ---
	p2 := startServer(t, dir)
	addr2 := p2.awaitServerStarted(t)
	// And the floors are REAL, not merely present: alpha-1 is durable, so the
	// restart must mint strictly above it.
	if n := suffixOf(t, enrolAgent(t, dir, addr2, "alpha")); n <= 1 {
		t.Fatalf("after a restart, enrolling \"alpha\" minted suffix %d, want strictly greater than 1\n%s", n, p2.stderr())
	}
	p2.signal(t, syscall.SIGTERM)
	if code := p2.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("restart shutdown exited %d, want 0\n%s", code, p2.stderr())
	}
}

// TestServerRefusesToStartWithCorruptSuffixFloors proves the no-fallback rule at
// the PROCESS level: a floors file that does not verify must stop the server,
// not be regenerated and not be replaced by a fresh counter. Regenerating means
// resuming every name from 1, which is the failure this whole task exists to
// prevent -- so the correct outcome is a loud, recoverable outage.
func TestServerRefusesToStartWithCorruptSuffixFloors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, suffixFileInDataDir), []byte("agent-bus-agent-suffixes v3 sha256=00\nalpha 1\n"), 0o600); err != nil {
		t.Fatalf("writing the corrupt floors file: %v", err)
	}

	p := startServer(t, dir)
	code := p.awaitExit(t, startupTimeout)
	if code != 1 {
		t.Fatalf("server exited %d on a corrupt floors file, want 1\n%s", code, p.stderr())
	}
	out := p.stderr()
	if !strings.Contains(out, "agent-bus: ") {
		t.Fatalf("the refusal must be reported on stderr with main()'s prefix\n%s", out)
	}
	if !strings.Contains(out, "suffix floors") {
		t.Fatalf("the refusal must name the suffix floors so an operator knows what to fix\n%s", out)
	}
	// It must not have started serving.
	for _, line := range p.snapshot() {
		if strings.Contains(line, msgServerStarted) {
			t.Fatalf("the server logged %q despite unprovable suffix floors\n%s", msgServerStarted, out)
		}
	}
}
