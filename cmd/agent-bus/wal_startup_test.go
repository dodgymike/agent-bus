package main

// Tests for DUR-9: run() must open AND replay the write-ahead log strictly
// after the data-dir lock and strictly before the listener binds, must come up
// and stay up even when the log on disk is unreadable (quarantining it loudly
// and starting a fresh one -- see DUR-11 below), and must close the log without
// damaging it on shutdown.
//
// These run the server in a SUBPROCESS -- os.Args[0] re-executed with
// envRunServer set, which TestMain routes into run() -- for two reasons that a
// same-process test cannot cover: only a real process proves the exit CODE
// (run() returning nil is half the claim; main() turning that into exit 0 is
// the other half), and the happy path has to exercise the real signal handler
// installed by run(), which a test that injects into waitAndShutdown
// deliberately bypasses (see TestShutdownReleasesLongPoll in main_test.go).
//
// The harness lives entirely in this file: no production code exists or is
// shaped to support it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// Environment contract between the parent test and the re-executed child. All
// four are read ONLY by TestMain below; nothing in main.go knows about them.
const (
	envRunServer = "AGENT_BUS_TEST_RUN_SERVER"
	envDataDir   = "AGENT_BUS_TEST_DATA_DIR"
	envListen    = "AGENT_BUS_TEST_LISTEN"
	envLogLevel  = "AGENT_BUS_TEST_LOG_LEVEL"
	// envExtraArgs carries SPACE-SEPARATED extra flags for the child, so a test
	// can exercise a flag the four fixed variables above do not cover (today:
	// -backfill-suffix-floors). Space-separated is enough because every flag it
	// carries is a bare boolean or a value with no spaces; if that ever stops
	// being true, encode it properly rather than quoting here.
	envExtraArgs = "AGENT_BUS_TEST_EXTRA_ARGS"
)

// Bounds. Every wait in this file is bounded so a regression fails the test
// instead of hanging the suite.
const (
	startupTimeout  = 20 * time.Second
	shutdownTimeout = 15 * time.Second
	pollInterval    = 10 * time.Millisecond
)

// TestMain doubles as the server entry point for the child process. With
// envRunServer set it calls main() itself, so the child is the real startup
// path and not a re-implementation of it.
func TestMain(m *testing.M) {
	if os.Getenv(envRunServer) == "1" {
		os.Exit(runServerChild())
	}
	os.Exit(m.Run())
}

// runServerChild IS main(): it builds argv from the harness environment and
// calls main() directly, so the child exercises parseFlags, Config.validate and
// -- crucially -- main()'s own error-to-exit-code mapping, instead of a copy of
// them that would keep passing while main() drifted. The corrupt-log test's
// "exit 1" and "agent-bus: " prefix are therefore assertions about main.go, not
// about this harness.
//
// main() calls os.Exit on every failure path, so reaching the return can only
// mean a clean run: 0.
func runServerChild() int {
	dataDir := os.Getenv(envDataDir)
	if dataDir == "" {
		// Never silently inherit main.go's ./data default: the tracked data dir
		// is not a test fixture and no test may ever write to it.
		fmt.Fprintf(os.Stderr, "agent-bus: test harness: %s must be set\n", envDataDir)
		return 2
	}
	listen := os.Getenv(envListen)
	if listen == "" {
		// An empty listen address must never become a bind on every interface.
		// The harness only ever wants an ephemeral loopback port.
		listen = "127.0.0.1:0"
	}
	level := os.Getenv(envLogLevel)
	if level == "" {
		level = defaultLogLevel
	}
	os.Args = []string{"agent-bus", "-listen", listen, "-data-dir", dataDir, "-log-level", level}
	os.Args = append(os.Args, strings.Fields(os.Getenv(envExtraArgs))...)
	main()
	return 0
}

// TestServerOpensWALOnStart is the DUR-9 proof: the name is load-bearing, the
// task's proof_cmd is `go test -race -run TestServerOpensWALOnStart ./...`.
func TestServerOpensWALOnStart(t *testing.T) {
	t.Run("fresh data dir", func(t *testing.T) {
		dir := t.TempDir()
		walPath := filepath.Join(dir, wal.WALFileName)

		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)

		// Invariant 6/DUR-9: the log FILE is created at startup, not lazily on
		// the first write. Non-empty because wal.Open writes the 16-byte file
		// header. A regression that drops the wal.Open call entirely leaves no
		// bus.wal here at all.
		fi, err := os.Stat(walPath)
		if err != nil {
			t.Fatalf("stat %q after startup: %v; run() must open the WAL on start", walPath, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("%q is empty after startup, want at least the file header", walPath)
		}

		// THE ORDERING ASSERTION (invariant 5: disk is the truth, memory is only
		// the serving copy). The three lines pin the whole startup sequence as an
		// observable order: the data dir is LOCKED, then the log is opened and
		// replayed inside that lock (wal.Open does not return until replay has
		// finished), and only then does the server announce itself started --
		// which run() logs after srv.Serve is running, so nothing can have been
		// ANSWERED from an unreplayed store.
		//
		// What this does NOT claim: where net.Listen sits. A listener bound early
		// but not yet served answers nothing, so hoisting the bind alone would
		// (correctly) not fail here; "served after replay" is what matters and is
		// what is asserted, reinforced by the mustGetHealthz below.
		lockedIdx := proc.lineIndex(t, msgDirLocked)
		openedIdx := proc.lineIndex(t, msgWALOpened)
		startedIdx := proc.lineIndex(t, msgServerStarted)
		if lockedIdx >= openedIdx || openedIdx >= startedIdx {
			t.Fatalf("stderr line order: %q at %d, %q at %d, %q at %d; want lock < open+replay < serve\n%s",
				msgDirLocked, lockedIdx, msgWALOpened, openedIdx, msgServerStarted, startedIdx, proc.stderr())
		}

		// A fresh log has nothing to replay, and the high-water mark starts at
		// 1: an empty log that reported anything else would mean recovery
		// invented history.
		fields := parseLogfmt(proc.line(t, msgWALOpened))
		wantFresh := map[string]string{
			"data_dir":         dir,
			"path":             walPath,
			"records_replayed": "0",
			"applied":          "0",
			"aborted":          "0",
			"dangling":         "0",
			"next_index":       "1",
			"repaired":         "false",
			"repaired_bytes":   "0",
		}
		for k, want := range wantFresh {
			if got := fields[k]; got != want {
				t.Errorf("%q field %s = %q, want %q (full line: %s)", msgWALOpened, k, got, want, proc.line(t, msgWALOpened))
			}
		}

		// The server really served: proves wal.Open did not merely succeed but
		// also returned in time for the listener to come up.
		mustGetHealthz(t, dir, addr)

		// Shutdown: SIGTERM must be a clean exit 0 (the signal handler run()
		// installs is only reachable in a real process), and Close must leave
		// the log on disk. Truncating or unlinking it on shutdown would be a
		// silent invariant-6 violation that nothing else catches.
		before := mustReadFile(t, walPath)
		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
		}

		// CLOSE ACTUALLY RAN, AND RAN BEFORE THE UNLOCK. The size check below
		// cannot see a missing Close: the kernel closes the fd at process exit,
		// so deleting run()'s deferred walLog.Close() leaves bus.wal
		// byte-identical and every other assertion here green. The "write-ahead
		// log closed" line is the only observable proof, and requiring it to
		// precede "data directory lock released" pins the defer-LIFO order
		// main.go treats as load-bearing: flush and close the log while the data
		// dir is still locked, and only then drop the lock.
		//
		// Both lines are written during process teardown, so they are read from
		// the FINAL drained snapshot -- awaitExit returns only after the stderr
		// pipe hit EOF and cmd.Wait() returned, so this cannot race the scanner.
		closedIdx := proc.lineIndex(t, msgWALClosed)
		releasedIdx := proc.lineIndex(t, msgLockReleased)
		if closedIdx >= releasedIdx {
			t.Fatalf("stderr line order on shutdown: %q at %d, %q at %d; the log must be closed while the data dir is still locked\n%s",
				msgWALClosed, closedIdx, msgLockReleased, releasedIdx, proc.stderr())
		}

		after := mustReadFile(t, walPath)
		if len(after) < len(before) {
			t.Fatalf("%q shrank across shutdown: %d bytes -> %d bytes; Close must never truncate the log",
				walPath, len(before), len(after))
		}
	})

	t.Run("replays an existing log", func(t *testing.T) {
		// REPLAY IS REAL. Without this subtest the suite only proves a file was
		// created; here the data dir is seeded with committed transactions
		// BEFORE the server starts, and the startup line has to account for
		// every one of them. A run() that opened the log with replay disabled,
		// or that created a second empty log somewhere else, fails here.
		dir := t.TempDir()
		walPath := filepath.Join(dir, wal.WALFileName)

		const seeded = 3
		seed, err := wal.Open(wal.LogOptions{Dir: dir})
		if err != nil {
			t.Fatalf("seeding wal.Open(%q): %v", dir, err)
		}
		var last wal.Committed
		for i := 0; i < seeded; i++ {
			body := json.RawMessage(fmt.Sprintf(`{"n":%d}`, i+1))
			last, err = seed.Write(wal.Entry{Kind: "test", Body: body})
			if err != nil {
				t.Fatalf("seeding write %d: %v", i, err)
			}
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("closing the seeded log: %v", err)
		}
		// This dir now has history, so it needs the floors file a real restart
		// would have. The subject here is REPLAY, and the shape being modelled is
		// an ordinary restart of a bus over its own data dir -- which always has
		// a floors file. Seeding one (rather than passing
		// -backfill-suffix-floors) keeps the identity guard armed and keeps this
		// test about the log. See seedSuffixFloorsFile.
		seedSuffixFloorsFile(t, dir)

		// Expectations are DERIVED from what was actually written, not guessed:
		// the two-phase write path emits one PREPARE and one COMMIT per Write
		// (2*seeded records), each Write reached commit (applied == seeded),
		// and the next append must land strictly above the last index handed
		// out, so next_index == last commit index + 1.
		wantRecords := 2 * seeded
		wantNext := last.CommitIndex + 1

		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)

		openedLine := proc.line(t, msgWALOpened)
		fields := parseLogfmt(openedLine)
		checks := []struct {
			key  string
			want string
			why  string
		}{
			{"path", walPath, "the server must replay the SEEDED log, not a fresh one elsewhere"},
			{"records_replayed", strconv.Itoa(wantRecords), "one prepare + one commit per seeded write must be read back"},
			{"applied", strconv.Itoa(seeded), "every seeded transaction reached commit and must be recovered"},
			{"aborted", "0", "nothing was aborted"},
			{"dangling", "0", "every prepare was committed before the seeding log was closed"},
			{"next_index", strconv.FormatUint(wantNext, 10), "the high-water mark must sit above every index ever written"},
			{"repaired", "false", "an intact log must never be truncated by recovery (invariant 6)"},
			{"repaired_bytes", "0", "an intact log must never lose a byte"},
		}
		for _, c := range checks {
			if got := fields[c.key]; got != c.want {
				t.Errorf("%q field %s = %q, want %q: %s\nline: %s", msgWALOpened, c.key, got, c.want, c.why, openedLine)
			}
		}

		// Ordering holds on the replay path too -- this is the case that
		// actually matters, since here there IS history to lose.
		if openedIdx, startedIdx := proc.lineIndex(t, msgWALOpened), proc.lineIndex(t, msgServerStarted); openedIdx >= startedIdx {
			t.Fatalf("replay finished at line %d but the server started at line %d; nothing may be served before replay completes\n%s",
				openedIdx, startedIdx, proc.stderr())
		}

		mustGetHealthz(t, dir, addr)

		beforeSize := mustFileSize(t, walPath)
		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
		}
		// Close ran, and ran before the unlock, on the path that has recovered
		// history to lose too (see the fresh-dir subtest for why the size check
		// alone cannot see a deleted Close).
		if closedIdx, releasedIdx := proc.lineIndex(t, msgWALClosed), proc.lineIndex(t, msgLockReleased); closedIdx >= releasedIdx {
			t.Fatalf("stderr line order on shutdown: %q at %d, %q at %d; the replayed log must be closed while the data dir is still locked\n%s",
				msgWALClosed, closedIdx, msgLockReleased, releasedIdx, proc.stderr())
		}
		if afterSize := mustFileSize(t, walPath); afterSize < beforeSize {
			t.Fatalf("seeded log shrank across shutdown: %d -> %d bytes; recovered history must survive a clean stop",
				beforeSize, afterSize)
		}
	})
}

// TestServerQuarantinesACorruptLogAndStartsAnyway covers the damaged-log path.
// The name deliberately shares the TestServerOpensWAL... family's subject so it
// reads next to the happy-path test; it is matched by name, not by prefix.
//
// POLICY (DECISIONS.md 2026-08-02, "Availability over retention: the bus ALWAYS
// restarts" -- this REVERSES what this test used to assert). Faced with a log it
// cannot interpret, the server does NOT refuse to start. It moves the damaged
// file aside, says so loudly, starts a fresh log and serves. Invariant 4 is
// narrowed to match: acknowledged data may be discarded when it is found
// corrupt. This test therefore asserts the OPPOSITE of the "exit 1, nothing
// listens" contract it enforced before 2026-08-02 -- if a future change makes
// the server refuse again, that is a reverted decision, not a bug fix, and this
// test is where it must be argued.
//
// The seeded damage is a garbage FILE HEADER with no salvageable record behind
// it -- the one class recovery can make nothing at all of, and so the only class
// that reaches quarantine (a torn tail or an isolated bad record is repaired in
// place instead, and is covered in internal/wal).
//
// The load-bearing assertion here is NOT that the server survives; it is that
// the discard is OBSERVABLE. A SILENT discard is the P0 defect the decision
// calls out by name ("the defect was never that data was discarded; it is that
// the discard was SILENT"), and a server that quietly serves an empty bus after
// eating a log is indistinguishable, to an operator, from one that had nothing
// to serve. So: a loud level, the original path, the path it was moved to, and
// how many bytes went -- plus the bytes themselves still on disk.
func TestServerQuarantinesACorruptLogAndStartsAnyway(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, wal.WALFileName)

	corrupt := []byte(strings.Repeat("X", 64))
	if err := os.WriteFile(walPath, corrupt, 0o600); err != nil {
		t.Fatalf("writing the corrupt log %q: %v", walPath, err)
	}
	before := mustReadFile(t, walPath)
	// ISOLATE THE VARIABLE. The claim under test is that MEDIA DAMAGE TO THE LOG
	// does not stop the bus (DECISIONS.md 2026-08-02, availability over
	// retention). Writing a corrupt bus.wal also makes the dir non-empty, so
	// without a floors file the server would refuse for an unrelated reason --
	// lost identity authority -- and this test would be asserting the wrong
	// thing, or would have to disarm the identity guard with
	// -backfill-suffix-floors to see past it. Seeding the floors file leaves the
	// guard armed and makes the corrupt log the ONLY damage present, so a green
	// result means exactly what the test name says.
	seedSuffixFloorsFile(t, dir)

	proc := startServer(t, dir)

	// (1) IT STARTS. awaitServerStarted fails loudly if the child exits first,
	// so this alone catches a regression back to refuse-to-start -- and it
	// catches it in milliseconds rather than by waiting out a timeout.
	addr := proc.awaitServerStarted(t)

	// (2) THE DISCARD IS LOGGED, LOUDLY AND SPECIFICALLY.
	quarantineLine := proc.line(t, msgWALQuarantined)
	qf := parseLogfmt(quarantineLine)

	// Loud: an operator filtering at warn or above must still see it. info or
	// debug here would be the silent-discard defect wearing a log line.
	if lvl := qf["level"]; lvl != "error" {
		t.Errorf("quarantine line level = %q, want %q; a discarded log is not routine news\nline: %s", lvl, "error", quarantineLine)
	}
	// Specific: WHICH log, WHERE the bytes went, HOW MANY. All three are what
	// makes the line actionable instead of merely present.
	if got := qf["path"]; got != walPath {
		t.Errorf("quarantine line path = %q, want %q; the line must name the log that was discarded\nline: %s", got, walPath, quarantineLine)
	}
	movedTo := qf["moved_to"]
	if movedTo == "" {
		t.Errorf("quarantine line has no moved_to= field; an operator cannot find the evidence\nline: %s", quarantineLine)
	}
	if want := strconv.Itoa(len(corrupt)); qf["bytes"] != want {
		t.Errorf("quarantine line bytes = %q, want %q; the line must say how much was discarded\nline: %s", qf["bytes"], want, quarantineLine)
	}

	// (3) THE BYTES SURVIVE. Quarantine is a rename, never a delete: this code
	// failing to read a file does not prove nobody can, and an operator with a
	// hex editor is owed the original. Found by glob rather than by trusting
	// moved_to, then cross-checked against it, so a line that names a path
	// nothing was written to cannot pass.
	matches, err := filepath.Glob(walPath + ".corrupt-*")
	if err != nil {
		t.Fatalf("globbing for the quarantine file: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d files matching %q, want exactly 1; the damaged log must be preserved beside the fresh one: %v\n%s",
			len(matches), walPath+".corrupt-*", matches, proc.stderr())
	}
	if matches[0] != movedTo {
		t.Errorf("quarantine line says moved_to=%q but the file on disk is %q; the log must name the real destination", movedTo, matches[0])
	}
	if quarantined := mustReadFile(t, matches[0]); string(quarantined) != string(before) {
		t.Errorf("quarantined file %q is %d bytes and does not match the original %d bytes; the damaged data must be preserved verbatim",
			matches[0], len(quarantined), len(before))
	}

	// (4) IT CAME UP ON A FRESH LOG, and is not half-serving the corrupt one.
	// records_replayed=0 with next_index=1 is the signature of a log with no
	// history at all; anything else here would mean recovery kept, or invented,
	// something out of 64 bytes of "X".
	openedLine := proc.line(t, msgWALOpened)
	of := parseLogfmt(openedLine)
	for _, c := range []struct{ key, want, why string }{
		{"path", walPath, "the fresh log must take the canonical name back"},
		{"records_replayed", "0", "nothing in a quarantined log may be replayed"},
		{"applied", "0", "no transaction may be recovered from a log that could not be read"},
		{"next_index", "1", "a fresh log starts its indices at 1"},
	} {
		if got := of[c.key]; got != c.want {
			t.Errorf("%q field %s = %q, want %q: %s\nline: %s", msgWALOpened, c.key, got, c.want, c.why, openedLine)
		}
	}
	// The fresh log is a real, distinct file: created (wal.Open writes the file
	// header) and emphatically not the corrupt bytes left in place.
	fresh := mustReadFile(t, walPath)
	if len(fresh) == 0 {
		t.Errorf("%q is empty after the quarantine; a fresh log must still be created and headered", walPath)
	}
	if string(fresh) == string(before) {
		t.Errorf("%q still holds the corrupt bytes; the damaged log must be MOVED, not left in place", walPath)
	}

	// The discard is reported BEFORE anything is served, so the log record
	// exists even if the process dies a moment after coming up.
	if qIdx, sIdx := proc.lineIndex(t, msgWALQuarantined), proc.lineIndex(t, msgServerStarted); qIdx >= sIdx {
		t.Errorf("quarantine logged at line %d but the server started at line %d; the discard must be recorded before we serve\n%s",
			qIdx, sIdx, proc.stderr())
	}

	// (1, continued) IT STAYS UP AND SERVES. "Started" is not "serving": this
	// is the assertion that the always-restart policy actually delivers a
	// usable bus rather than a process that logs a banner and sits there.
	mustGetHealthz(t, dir, addr)

	// And it is a normal server in every other respect: SIGTERM is a clean
	// exit 0. Notably NOT the exit 1 this test demanded before the policy
	// changed -- a non-zero exit anywhere on this path is the regression.
	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0; a server that quarantined a log is still a healthy server\n%s", code, proc.stderr())
	}

	// Shutdown must not eat the evidence either.
	if q := mustReadFile(t, matches[0]); string(q) != string(before) {
		t.Errorf("quarantined file %q changed across shutdown; the preserved bytes must be immutable", matches[0])
	}
}

// TestStartupSummaryLogsQuarantineFields is the proof for the fix: the
// startup summary line (msgWALOpened) must itself say a quarantine happened,
// not only wal's own separate ERROR line (msgWALQuarantined, already asserted
// by TestServerQuarantinesACorruptLogAndStartsAnyway above).
//
// Before the fix, msgWALOpened carried only repaired/repaired_bytes -- fields
// that stay false/0 on the quarantine path, because wal.Repair.Truncated and
// .Removed are never set there (see wal.Repair's doc comment: "the quarantine
// path returns early with only Quarantined, DiscardCount and DiscardedBytes
// set"). So an operator reading ONLY the startup summary -- the common case,
// since it is the one line every start emits -- saw repaired=false
// repaired_bytes=0, indistinguishable from a clean start that replayed
// nothing. DECISIONS.md 2026-08-02 ("Availability over retention") is
// explicit that the defect is the SILENCE, not the discard.
//
// This test seeds the same "unsalvageable file header" damage the sibling
// test above uses (the one class of damage that reaches quarantine rather
// than a truncate/rewrite repair) and asserts the startup summary line names
// the quarantine destination and the exact discard totals.
func TestStartupSummaryLogsQuarantineFields(t *testing.T) {
	dir := t.TempDir() // throwaway data dir; never the tracked ./data
	walPath := filepath.Join(dir, wal.WALFileName)

	corrupt := []byte(strings.Repeat("X", 64))
	if err := os.WriteFile(walPath, corrupt, 0o600); err != nil {
		t.Fatalf("writing the corrupt log %q: %v", walPath, err)
	}
	// As in the sibling test above: the corrupt log is the damage under test, so
	// the dir gets the floors file a real one would have and the identity guard
	// stays armed. This test needs the server to REACH its startup summary line
	// at all, and a refusal over a missing floors file would stop it before then.
	seedSuffixFloorsFile(t, dir)

	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	// Cross-check against wal's own quarantine line so this test's expectation
	// is derived from what actually happened, not guessed: the startup summary
	// must agree with moved_to and bytes that msgWALQuarantined already reports.
	quarantineLine := proc.line(t, msgWALQuarantined)
	qf := parseLogfmt(quarantineLine)
	wantMovedTo := qf["moved_to"]
	wantBytes := qf["bytes"]
	if wantMovedTo == "" || wantBytes == "" {
		t.Fatalf("quarantine line missing moved_to/bytes, cannot derive expectations: %s", quarantineLine)
	}

	openedLine := proc.line(t, msgWALOpened)
	of := parseLogfmt(openedLine)

	if got := of["quarantined"]; got != wantMovedTo {
		t.Errorf("%q field quarantined = %q, want %q (the quarantine destination); the startup summary must say a whole log was eaten, not just wal's separate ERROR line\nline: %s",
			msgWALOpened, got, wantMovedTo, openedLine)
	}
	if got := of["discard_count"]; got != "1" {
		t.Errorf("%q field discard_count = %q, want %q (a whole-log quarantine is exactly one discard)\nline: %s",
			msgWALOpened, got, "1", openedLine)
	}
	if got := of["discarded_bytes"]; got != wantBytes {
		t.Errorf("%q field discarded_bytes = %q, want %q (must match wal's own byte count for the discarded file)\nline: %s",
			msgWALOpened, got, wantBytes, openedLine)
	}
	// repaired/repaired_bytes must NOT falsely claim a repair happened: the
	// quarantine path never sets Truncated/Removed (see wal.Repair doc), and a
	// regression that started reporting Truncated=true here would be a
	// different bug (misclassifying quarantine as a truncation repair).
	if got := of["repaired"]; got != "false" {
		t.Errorf("%q field repaired = %q, want %q; quarantine is not a truncation repair\nline: %s",
			msgWALOpened, got, "false", openedLine)
	}
	if got := of["repaired_bytes"]; got != "0" {
		t.Errorf("%q field repaired_bytes = %q, want %q; quarantine never sets Repair.Removed\nline: %s",
			msgWALOpened, got, "0", openedLine)
	}

	mustGetHealthz(t, dir, addr)

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
}

// Log messages the assertions above key on. They are the operator-visible
// contract of run(); renaming one in main.go must fail these tests.
const (
	msgDirLocked     = `msg="data directory locked"`
	msgWALOpened     = `msg="write-ahead log opened"`
	msgServerStarted = `msg="server started"`
	msgWALClosed     = `msg="write-ahead log closed"`
	msgLockReleased  = `msg="data directory lock released"`
	// Emitted by internal/wal, not by run(), but it is startup-visible operator
	// contract all the same: it is the ONLY record that acknowledged data was
	// thrown away (DECISIONS.md 2026-08-02). Renaming it silently is the defect.
	msgWALQuarantined = `msg="wal quarantined an unreadable log and started a fresh one"`
)

// serverProc is a running (or finished) child server: its captured stderr, and
// a bounded way to wait for it.
type serverProc struct {
	cmd *exec.Cmd

	mu    sync.Mutex
	lines []string

	done    chan struct{} // closed once the process has exited AND stderr is drained
	waitErr error         // the Wait() result; read only after done is closed
}

// seedSuffixFloorsFile writes a valid, EMPTY agent-id floors file into dataDir,
// making the directory look like one a previous run of THIS binary left behind.
//
// # Why the tests below need it, and why it is the right fixture
//
// Since AUTH-3-FU-FAILOPEN, a data directory that has HISTORY (it was non-empty
// when the process started, or its log holds records) but has NO agent-suffixes
// file is a FATAL startup error rather than a silent backfill: that shape is
// indistinguishable from a floors file someone deleted, and starting would
// resume every agent name from suffix 1 over ids that are already live
// (invariant 1). Every fixture in this file builds exactly that shape -- it
// seeds a log, or writes a damaged one, into an otherwise empty dir -- so all of
// them tripped the new guard.
//
// The fix is this seed and NOT `-backfill-suffix-floors`, and the difference
// matters. The flag DISARMS the guard; a test that passes it is no longer
// running the code path a real restart runs, and would keep passing if the guard
// were deleted outright. Seeding the floors file instead leaves the guard fully
// ARMED and makes the directory a genuine steady-state restart, which is what
// these tests were always about: a bus coming back up over a data dir it already
// owns. The one test whose subject really IS the migration
// (TestLegacyDataDirDoesNotReMintAgentIDs) takes the flag, and the guard's own
// refusal is pinned by TestServerRefusesToStartWithHistoryAndNoSuffixFloors.
//
// It is written through ids.OpenNameSuffixes + Seal -- the production writer --
// rather than as literal bytes, so the fixture cannot drift out of the file
// format the way a hand-written header would (compare the deliberately corrupt
// literal in TestServerRefusesToStartWithCorruptSuffixFloors, which wants to be
// invalid). An EMPTY floors map is the honest content here: these directories
// have never enrolled anyone, so no name has a floor to record.
//
// # PRECONDITION, ENFORCED: dataDir's log must hold no agent id
//
// Sealing an empty floors map asserts "no suffix has ever been issued for any
// name on this dir". On the fixtures below that is simply TRUE -- their logs
// hold records of kind "test", or 64 bytes of garbage, and no agent id at all.
// Called on a dir whose log DOES name an agent id, the same two lines would
// seal a floor that is provably too low, and the resulting test would go green
// over exactly the re-mint this whole mechanism exists to prevent -- a false
// pass indistinguishable from a real one, which is the failure mode that
// produced this task.
//
// So the precondition is CHECKED rather than left as prose for a future fixture
// author to read. It is deliberately checked by RECORD KIND and not by deriving
// floors: the derivation needs a bus id this dir has not been given yet (the
// child process mints it on the start that follows), whereas the kinds are
// readable without one. A log that cannot be scanned at all is not a violation
// -- that is the corrupt-log fixture, whose unreadable bytes name no id anyone
// can recover -- so an unreadable log is accepted and only a READABLE log that
// carries an identity-bearing record is refused.
func seedSuffixFloorsFile(t *testing.T, dataDir string) {
	t.Helper()

	if recs, _, err := wal.ScanAll(filepath.Join(dataDir, wal.WALFileName), wal.KindWAL); err == nil {
		for _, rec := range recs {
			if rec.Type != wal.TypePrepare {
				continue
			}
			entry, _, err := wal.DecodePrepare(filepath.Join(dataDir, wal.WALFileName), rec)
			if err != nil {
				continue // undecodable: carries no id this fixture could be hiding
			}
			if entry.Kind == store.RecordKind || entry.Kind == enrolmentRecordKindOnDisk {
				t.Fatalf("seedSuffixFloorsFile(%q) was called on a dir whose log holds a %q record (index %d). Sealing an EMPTY floors map here would assert that no agent-id suffix was ever issued on this dir, which that record disproves -- the test would pass while a real bus in the same shape re-mints a live agent id (invariant 1). Use -backfill-suffix-floors for a dir whose log genuinely holds agent ids (see TestLegacyDataDirDoesNotReMintAgentIDs), or seed a log with no identity-bearing records.",
					dataDir, entry.Kind, rec.Index)
			}
		}
	}

	alloc, err := ids.OpenNameSuffixes(dataDir)
	if err != nil {
		t.Fatalf("seeding the agent-id floors file in %q: %v", dataDir, err)
	}
	if err := alloc.Seal(); err != nil {
		t.Fatalf("sealing the seeded agent-id floors file in %q: %v", dataDir, err)
	}
	// Prove the fixture actually produced the file the guard looks for. Without
	// this, a change to where the floors live would turn every test that calls
	// this helper into a test of the refusal path, failing far from the cause.
	if _, err := os.Stat(filepath.Join(dataDir, suffixFileInDataDir)); err != nil {
		t.Fatalf("seeding the floors file left no %q in %q: %v; the fixture proves nothing", suffixFileInDataDir, dataDir, err)
	}
}

// startServer re-executes the test binary as a server bound to 127.0.0.1:0 (an
// ephemeral port: never a fixed one, and never the tracked ./data dir) against
// dataDir, and registers a cleanup that kills it however the test ends.
func startServer(t *testing.T, dataDir string) *serverProc {
	t.Helper()
	return startServerArgs(t, dataDir)
}

// startServerArgs is startServer with extra flags appended to the child's argv.
// Only tests that need a flag outside the fixed four use it.
func startServerArgs(t *testing.T, dataDir string, extra ...string) *serverProc {
	t.Helper()
	return startServerListen(t, dataDir, "127.0.0.1:0", extra...)
}

// startServerListen is startServerArgs with an explicit -listen.
//
// Almost every test wants the ephemeral "127.0.0.1:0" the two helpers above
// pass, and MUST keep wanting it: a fixed port makes the suite fail when it is
// run twice at once. The one legitimate caller is a test whose subject IS the
// port -- proving a refused start left nothing listening on it
// (TestRunRefusesToStartWithoutUsableCert) is impossible against a port the
// kernel chose and the process never reported.
func startServerListen(t *testing.T, dataDir, listen string, extra ...string) *serverProc {
	t.Helper()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		envRunServer+"=1",
		envDataDir+"="+dataDir,
		envListen+"="+listen,
		envLogLevel+"=debug",
		envExtraArgs+"="+strings.Join(extra, " "),
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child server %q: %v", os.Args[0], err)
	}

	p := &serverProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			p.mu.Lock()
			p.lines = append(p.lines, sc.Text())
			p.mu.Unlock()
		}
		p.waitErr = cmd.Wait()
	}()

	t.Cleanup(func() {
		select {
		case <-p.done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(shutdownTimeout):
			t.Errorf("child server %d did not exit after Kill", cmd.Process.Pid)
		}
	})
	return p
}

// snapshot returns a copy of the stderr lines seen so far.
func (p *serverProc) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.lines))
	copy(out, p.lines)
	return out
}

// stderr renders everything captured so far, for failure messages.
func (p *serverProc) stderr() string {
	return "--- child stderr ---\n" + strings.Join(p.snapshot(), "\n") + "\n--- end ---"
}

// awaitLine blocks until a stderr line contains want, the process exits, or the
// bound expires.
func (p *serverProc) awaitLine(t *testing.T, want string, bound time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		for _, l := range p.snapshot() {
			if strings.Contains(l, want) {
				return l
			}
		}
		select {
		case <-p.done:
			// One last look: the line may have arrived with the final flush.
			for _, l := range p.snapshot() {
				if strings.Contains(l, want) {
					return l
				}
			}
			t.Fatalf("child server exited (%v) before logging %q\n%s", p.waitErr, want, p.stderr())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %q on the child's stderr\n%s", bound, want, p.stderr())
		}
		time.Sleep(pollInterval)
	}
}

// awaitServerStarted waits for the startup line and returns the address it
// reports, so the test talks to the port the kernel actually assigned.
func (p *serverProc) awaitServerStarted(t *testing.T) string {
	t.Helper()
	line := p.awaitLine(t, msgServerStarted, startupTimeout)
	addr := parseLogfmt(line)["addr"]
	if addr == "" {
		t.Fatalf("%q line has no addr= field: %s", msgServerStarted, line)
	}
	return addr
}

// line returns the (already captured) stderr line containing want.
func (p *serverProc) line(t *testing.T, want string) string {
	t.Helper()
	for _, l := range p.snapshot() {
		if strings.Contains(l, want) {
			return l
		}
	}
	t.Fatalf("no stderr line contains %q\n%s", want, p.stderr())
	return ""
}

// lineIndex returns the position of the first stderr line containing want.
func (p *serverProc) lineIndex(t *testing.T, want string) int {
	t.Helper()
	for i, l := range p.snapshot() {
		if strings.Contains(l, want) {
			return i
		}
	}
	t.Fatalf("no stderr line contains %q\n%s", want, p.stderr())
	return -1
}

// signal delivers sig to the child.
func (p *serverProc) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signalling the child with %v: %v", sig, err)
	}
}

// awaitExit waits for the child to exit within bound and returns its exit code.
func (p *serverProc) awaitExit(t *testing.T, bound time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(bound):
		t.Fatalf("child server did not exit within %s\n%s", bound, p.stderr())
	}
	if p.waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(p.waitErr, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for the child server: %v\n%s", p.waitErr, p.stderr())
	return -1
}

// mustGetHealthz asserts GET /healthz answers 200 at addr, i.e. the process is
// really serving and not merely alive.
//
// It takes dataDir because the bus serves TLS AND ONLY TLS (MTLS-LISTENER,
// invariant 11): the certificate in that directory is the trust anchor, so a
// caller must say WHICH bus it expects to be talking to. That is not
// bookkeeping -- it is the assertion. A probe that trusted any certificate would
// keep passing against a bus that had silently re-minted its key material, which
// is the one failure buscert_test.go exists to catch.
func mustGetHealthz(t *testing.T, dataDir, addr string) {
	t.Helper()
	resp, err := busTestClient(t, dataDir).Get(busURL(addr, "/healthz"))
	if err != nil {
		t.Fatalf("GET %s: %v", busURL(addr, "/healthz"), err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	return b
}

func mustFileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Size()
}

// parseLogfmt splits one logfmt record ("k=v k=\"quoted v\"") into its fields.
// internal/logging quotes a value whenever it is not bare-printable, so quoted
// values are unquoted here with strconv.Unquote, the same function that wrote
// them.
func parseLogfmt(line string) map[string]string {
	fields := make(map[string]string)
	for i := 0; i < len(line); {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		key := line[i : i+eq]
		i += eq + 1
		if i < len(line) && line[i] == '"' {
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					j++
					break
				}
				j++
			}
			raw := line[i:j]
			if v, err := strconv.Unquote(raw); err == nil {
				fields[key] = v
			} else {
				fields[key] = raw
			}
			i = j
			continue
		}
		if sp := strings.IndexByte(line[i:], ' '); sp >= 0 {
			fields[key] = line[i : i+sp]
			i += sp
		} else {
			fields[key] = line[i:]
			i = len(line)
		}
	}
	return fields
}
