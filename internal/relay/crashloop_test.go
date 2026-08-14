package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// RELAY-5's evidence: a federation that is BOTH a cycle and a diamond, a real
// kill -9 in the middle of it, and the assertion that every bus still delivers
// exactly once afterwards.
//
// # The topology, and why it is not just a ring
//
//	         A                     A  originates
//	         |                     B  fan-out point AND the bus the cycle closes on
//	         v                     C  } two disjoint routes to E
//	         B <------+            D  }
//	        / \       |            E  the diamond JOIN, and the bus that is killed
//	       v   v      |
//	       C   D      |
//	        \ /       |
//	         v        |
//	         E -------+
//
// A ring alone would prove only RELAY-3. The task's own scope note says so:
// "a topology with only a CYCLE mostly exercises RELAY-3's traversed-bus-path
// loop prevention, which is a DIFFERENT mechanism from IDEM-15's key-based
// duplicate suppression". So this topology carries both shapes at once:
//
//   - THE DIAMOND (B->C->E and B->D->E). E receives the same message by two
//     paths, and NEITHER path revisits a bus. RELAY-3 does nothing here — there
//     is no loop to detect — and the second arrival is suppressed ONLY by the
//     applied-key check on the origin's message id.
//   - THE CYCLE (B->C->E->B). E's onward hop returns to a bus already on the
//     path. The egress split horizon stops it before a byte leaves E.
//
// Note the cycle deliberately closes on B and not on A. A message can never loop
// back to its ORIGIN undetected however much a peer lies, because
// ValidateBusPath enforces BusPath[0] == OriginBus — so a cycle through the
// origin would test the wrong thing and would look like loop prevention working
// when it was really the origin-binding check.
//
// # What "crash" means here, and what this test does NOT prove
//
// The crash is a REAL SIGKILL to a re-exec'd child of this test binary, and the
// parent asserts WaitStatus.Signaled() && Signal() == SIGKILL. That check is not
// ceremony: a child that merely failed its own assertions also exits non-zero,
// and without the wait-status check it would masquerade as a crash and leave the
// parent asserting over an empty directory — which passes. The harness follows
// internal/idem/crashinjection_test.go.
//
// WHAT IS STOOD IN FOR, SAID PLAINLY: internal/relay owns no durable store, by
// design (see RelayConfig.AcceptRelay — the applied-key table belongs to
// internal/idem and its durability to IDEM-11). So bus E's applied-key table is
// persisted here by a small append-and-fsync log using idem's OWN Record.Encode
// / DecodeRecord and rebuilt through Store.Recover. That is exactly the
// stand-in cycle_test.go already makes for the wiring site, extended across a
// process boundary.
//
// So this test proves: given a durable applied-key table, RELAY'S ingest,
// forwarding and suppression logic yield exactly-once across a crash and a
// retry. It does NOT prove the durability of the real table — that is IDEM-11's
// own crash evidence (internal/hub/idem_crash_test.go and
// internal/idem/crashinjection_test.go), and it must not be read as covered
// here.

const (
	// envCrashLoopChild selects the child role. Unset means "not a crash child",
	// which is what makes the child test a no-op skip in an ordinary package run.
	envCrashLoopChild = "RELAY_CRASHLOOP_CHILD"
	// envCrashLoopDir is the directory the child persists bus E's applied-key
	// log into. Always a t.TempDir() owned by the parent, so no run ever shares
	// a directory with another and the tracked data/ dir is never touched.
	envCrashLoopDir = "RELAY_CRASHLOOP_DIR"

	// crashAtJoinApplied: bus E has APPLIED the first arrival and its
	// applied-key record is fsynced; the process dies before E can answer, and
	// before the second disjoint-path copy arrives. This is the instant that
	// matters — the ack is lost, so RELAY-4's retry is now guaranteed, and the
	// second copy is still in flight.
	crashAtJoinApplied = "join-applied"
)

// The federation fixture, shared byte-for-byte between the child and the parent.
// They must agree exactly or the parent would be replaying a DIFFERENT payload
// and would be exercising the violation path by accident — and still go green.
const (
	crashLoopOriginBus = "bus-ca"
	crashLoopHubBus    = "bus-cb"
	crashLoopLeftBus   = "bus-cc"
	crashLoopRightBus  = "bus-cd"
	crashLoopJoinBus   = "bus-ce"

	crashLoopSender    = crashLoopOriginBus + ".alpha-1"
	crashLoopOriginSeq = 1

	// joinKeyLog is the durable applied-key log for the bus that gets killed.
	joinKeyLog = "bus-ce.appliedkeys"
)

var crashLoopBody = []byte("one message, one cycle, one diamond, one kill -9")

// crashLoopMessage is the message the origin bus sends. It is deterministic in
// every field the applied-key IDENTITY depends on — origin bus, message id,
// sender, recipients, timestamp, size, content hash — so the fingerprint the
// child stored and the fingerprint the parent recomputes are the same 32 bytes
// across two processes with two independently generated keyrings.
//
// The SIGNATURE is not one of those fields, and must not be: signing keys are
// minted per process here, and relayFingerprint deliberately excludes the
// signature (see its doc). A fingerprint that covered it would make this test
// impossible to write, which is a useful sanity check on that decision.
func crashLoopMessage() RelayedMessage {
	return originMessage(crashLoopOriginBus, crashLoopSender, crashLoopOriginSeq, crashLoopBody, func(m *RelayedMessage) {
		m.Recipients = []string{crashLoopJoinBus + ".target-1"}
	})
}

// ---------------------------------------------------------------------------
// The durable applied-key log: idem's own encoding, appended and fsynced.
// ---------------------------------------------------------------------------

// appendAppliedKey writes one record and FSYNCS before returning.
//
// The fsync is here because the PRODUCTION path requires it (invariant 4:
// nothing is acknowledged before it is durable), and the stand-in must not model
// a weaker write than the thing it stands in for.
//
// Stated precisely, because it is easy to overclaim: a SIGKILL does NOT discard
// the page cache — the kernel outlives the process, so an unsynced write would
// still be readable by the parent here and REMOVING THIS CALL LEAVES THE TEST
// GREEN. What the fsync defends against is power loss or a kernel panic, which
// no in-process harness can inject. So this line is fidelity to the real write
// path, not the mechanism this test exercises.
func appendAppliedKey(path string, r idem.Record) error {
	enc, err := r.Encode()
	if err != nil {
		return fmt.Errorf("encoding applied-key record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(append([]byte(nil), enc...), '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// recoverAppliedKeys rebuilds a Store from the log, exactly as a restart would.
//
// Store.Recover rather than Store.Remember: recovery reinstates records that
// were already admitted, and re-running admission over them could refuse a
// record the bus has ALREADY applied — which would resurrect the duplicate the
// record exists to suppress.
func recoverAppliedKeys(t *testing.T, path string) (*idem.Store, []idem.Record) {
	t.Helper()
	store := idem.NewStore(idem.StoreOptions{})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the applied-key log the crashed child left at %s: %v", path, err)
	}
	var out []idem.Record
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		rec, err := idem.DecodeRecord(line)
		if err != nil {
			t.Fatalf("decoding a record from the crashed child's applied-key log: %v", err)
		}
		if err := store.Recover(rec); err != nil {
			t.Fatalf("recovering %s into the applied-key table: %v", rec.Key, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the applied-key log: %v", err)
	}
	return store, out
}

// ---------------------------------------------------------------------------
// The topology
// ---------------------------------------------------------------------------

// crashLoopTopology builds the five-bus federation. tune runs on each node
// before any traffic, so a caller can switch a defence off.
func crashLoopTopology(t *testing.T, fab *fabric, tune func(*node)) (origin, hub, left, right, join *node) {
	t.Helper()
	origin = newNode(t, fab, crashLoopOriginBus)
	hub = newNode(t, fab, crashLoopHubBus)
	left = newNode(t, fab, crashLoopLeftBus)
	right = newNode(t, fab, crashLoopRightBus)
	join = newNode(t, fab, crashLoopJoinBus)

	origin.peers = []*node{hub}
	hub.peers = []*node{left, right}
	left.peers = []*node{join}
	right.peers = []*node{join}
	join.peers = []*node{hub} // closes the cycle on hub, NOT on the origin

	if tune != nil {
		for _, n := range []*node{origin, hub, left, right, join} {
			tune(n)
		}
	}
	return origin, hub, left, right, join
}

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestRelayCrashLoopIntegrationChild is the child half. It does NOTHING in an
// ordinary run: without envCrashLoopChild it skips immediately.
func TestRelayCrashLoopIntegrationChild(t *testing.T) {
	point := os.Getenv(envCrashLoopChild)
	if point == "" {
		t.Skip("not a crash child: " + envCrashLoopChild + " is unset")
	}
	dir := os.Getenv(envCrashLoopDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envCrashLoopChild, point, envCrashLoopDir)
	}
	if point != crashAtJoinApplied {
		t.Fatalf("child: unknown crash point %q", point)
	}

	fab := &fabric{t: t}
	origin, _, _, _, join := crashLoopTopology(t, fab, nil)

	logPath := filepath.Join(dir, joinKeyLog)
	var once sync.Once
	join.persist = func(r idem.Record) error {
		if err := appendAppliedKey(logPath, r); err != nil {
			return err
		}
		// THE KILL, at the exact instant the record is on stable storage and
		// before this bus can answer. Everything after this point in the
		// process — the HTTP response, the onward hop, the second disjoint-path
		// arrival — never happens. SIGKILL cannot be caught, so nothing
		// deferred, buffered or graceful runs either.
		once.Do(killCrashLoopChildNow)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	origin.schedule(crashLoopMessage())
	fab.run(ctx, 64)

	t.Fatal("child: the run completed without the join bus ever applying the message, so no crash was injected")
}

// killCrashLoopChildNow kills this process with SIGKILL.
func killCrashLoopChildNow() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	panic("relay crash test: SIGKILL to self did not kill the process")
}

// runCrashLoopChild re-execs this test binary and asserts the child really DIED
// ON SIGKILL rather than failing its own assertions.
func runCrashLoopChild(t *testing.T, point, dir string) {
	t.Helper()

	// No os.Args[0] fallback: if it carried no path separator, exec.Command
	// would resolve it through PATH and could re-exec a DIFFERENT binary, which
	// would then be handed a crash point and a directory.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v; refusing to fall back to os.Args[0], which exec.Command would resolve through PATH", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestRelayCrashLoopIntegrationChild$", "-test.v")
	cmd.Env = append(os.Environ(), envCrashLoopChild+"="+point, envCrashLoopDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so "err != nil"
	// is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// ---------------------------------------------------------------------------
// The parent
// ---------------------------------------------------------------------------

// TestRelayCrashLoopIntegration is RELAY-5.
func TestRelayCrashLoopIntegration(t *testing.T) {
	// -----------------------------------------------------------------------
	// 1. The clean run: cycle + diamond, exactly once, and it terminates.
	// -----------------------------------------------------------------------
	t.Run("a cycle plus a diamond delivers exactly once and terminates", func(t *testing.T) {
		fab := &fabric{t: t}
		origin, hub, left, right, join := crashLoopTopology(t, fab, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		origin.schedule(crashLoopMessage())
		fab.run(ctx, 64)

		wantID := crashLoopOriginBus + "-1"
		for _, n := range []*node{hub, left, right, join} {
			if got := n.deliveredIDs(); len(got) != 1 || got[0] != wantID {
				t.Errorf("%s delivered %v, want exactly [%s]", n.busID, got, wantID)
			}
			if _, violations := n.counters(); violations != 0 {
				t.Errorf("%s reported %d idempotency violations; every arrival here is either new or a byte-identical duplicate, and neither is a protocol violation", n.busID, violations)
			}
		}
		if got := origin.deliveredIDs(); len(got) != 0 {
			t.Errorf("the origin delivered its own message back to itself: %v", got)
		}

		// THE DIAMOND: the join bus received the message TWICE, by two routes
		// that share no bus after the fan-out, and suppressed the second.
		// RELAY-3 cannot see this — neither path revisits anything.
		if dup, _ := join.counters(); dup != 1 {
			t.Errorf("the join bus suppressed %d duplicates, want exactly 1: it is reached by two DISJOINT paths, so the second arrival is the case only the applied-key check catches", dup)
		}

		// THE CYCLE: the join bus's onward hop returns to a bus already on the
		// path, and the egress split horizon stops it before a byte leaves. The
		// hub therefore never sees a second arrival at all.
		if dup, _ := hub.counters(); dup != 0 {
			t.Errorf("the hub bus absorbed %d duplicate arrivals, want 0: the split horizon should stop the returning copy at the join bus, so nothing reaches the hub's ingress", dup)
		}
		if got := hub.h.Stats(); got.Duplicates != 0 || got.LoopDrops != 0 {
			t.Errorf("hub handler stats = %+v, want no duplicates and no loop drops: with the split horizon on, the cycle costs ZERO wire traffic", got)
		}
	})

	// -----------------------------------------------------------------------
	// 2. Both defences are independently necessary.
	// -----------------------------------------------------------------------
	//
	// Invariant 10: "loop-prevention via the traversed bus path is a COMPLEMENT
	// to idempotency, never a substitute for it." That sentence is only worth
	// having if removing either one demonstrably breaks something, so each is
	// removed in turn and the specific damage is asserted.
	t.Run("removing the applied-key check double-DELIVERS on the diamond", func(t *testing.T) {
		fab := &fabric{t: t}
		origin, _, _, _, join := crashLoopTopology(t, fab, func(n *node) { n.noIdem = true })

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		origin.schedule(crashLoopMessage())
		fab.run(ctx, 64)

		if got := join.deliveredIDs(); len(got) != 2 {
			t.Fatalf("with the applied-key check removed the join bus delivered %v, want TWO copies. "+
				"If this ever reads 1, the premise of the whole epic is wrong: it would mean RELAY-3 alone "+
				"suppresses a message arriving by two DISJOINT paths, which it cannot — there is no repeated "+
				"hop for it to see. IDEM-15 is not optional, and this assertion is what proves it.", got)
		}
	})

	t.Run("removing loop prevention puts cycle traffic on the wire that idempotency must absorb", func(t *testing.T) {
		clean := &fabric{t: t}
		cleanOrigin, _, _, _, _ := crashLoopTopology(t, clean, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleanOrigin.schedule(crashLoopMessage())
		cleanSteps, ok := clean.runBounded(ctx, 64)
		if !ok {
			t.Fatal("the intact federation did not terminate")
		}

		lying := &fabric{t: t}
		origin, hub, _, _, join := crashLoopTopology(t, lying, func(n *node) { n.lying = true })
		origin.schedule(crashLoopMessage())
		lyingSteps, ok := lying.runBounded(ctx, 64)
		if !ok {
			t.Fatal("with loop prevention removed but the applied-key check intact, the traffic did not terminate; forwarding only on a NEW acceptance should still settle it")
		}

		// Correctness SURVIVES — that is the honest finding, and it is exactly
		// what doc.go claims: loop prevention is an AVAILABILITY mechanism.
		if got := join.deliveredIDs(); len(got) != 1 {
			t.Errorf("the join bus delivered %v with loop prevention off, want exactly one: idempotency is what protects DELIVERY", got)
		}
		// But the cost is real and measurable: the returning copy now reaches
		// the hub's ingress, where it must be absorbed, and the mesh does
		// strictly more work.
		if got := hub.h.Stats().Duplicates; got == 0 {
			t.Errorf("with loop prevention removed the hub absorbed %d duplicate arrivals, want at least 1: if the cycle costs nothing extra even with RELAY-3 off, then RELAY-3 is buying nothing and this test is measuring the wrong thing", got)
		}
		if lyingSteps <= cleanSteps {
			t.Errorf("the federation took %d relay steps with loop prevention OFF and %d with it ON; RELAY-3's whole contribution is bounding the traffic a cycle produces, so the first number must be the larger one", lyingSteps, cleanSteps)
		}
	})

	t.Run("removing BOTH defences does not terminate at all", func(t *testing.T) {
		fab := &fabric{t: t}
		origin, _, _, _, _ := crashLoopTopology(t, fab, func(n *node) {
			n.lying = true
			n.noIdem = true
		})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		origin.schedule(crashLoopMessage())
		if steps, terminated := fab.runBounded(ctx, 64); terminated {
			t.Fatalf("with BOTH loop prevention and the applied-key check removed the traffic settled after %d steps. "+
				"It must NOT: a cycle with no path check and no duplicate suppression circulates forever. "+
				"If this passes, one of the two switches is not actually switching its defence off, and every "+
				"independent-necessity claim above is vacuous.", steps)
		}
	})

	// -----------------------------------------------------------------------
	// 3. The crash: kill -9 mid-relay, restart, still exactly once.
	// -----------------------------------------------------------------------
	t.Run("a bus killed mid-relay recovers and still delivers exactly once", func(t *testing.T) {
		dir := t.TempDir()
		runCrashLoopChild(t, crashAtJoinApplied, dir)

		logPath := filepath.Join(dir, joinKeyLog)
		store, records := recoverAppliedKeys(t, logPath)
		if len(records) != 1 {
			t.Fatalf("the crashed bus left %d applied-key records, want exactly 1: it was killed the instant the FIRST record was fsynced, so more would mean the crash landed somewhere other than where it was aimed", len(records))
		}
		var originalLocalID string
		if err := json.Unmarshal(records[0].Result, &originalLocalID); err != nil {
			t.Fatalf("the stored result is not a local message id: %v", err)
		}
		if originalLocalID != crashLoopJoinBus+"-1" {
			t.Fatalf("the recovered result is %q, want %q: the receiving bus mints its OWN local id (invariant 1), and a retry must be answered with that same id", originalLocalID, crashLoopJoinBus+"-1")
		}

		// THE RESTART. A fresh bus E, with the recovered table and nothing else
		// — no memory of the delivery, exactly as a restarted process has none.
		//
		// seq is restored from the highest recovered record, which is invariant 1
		// ("ids are never reused, including across restarts" — the one reaffirmed
		// WITHOUT narrowing). It is asserted below by sending a genuinely NEW
		// message after recovery and requiring the id to be bus-ce-2; without
		// that assertion the restore would be inert, because every other arrival
		// here is a duplicate that returns before a mint.
		fab := &fabric{t: t}
		restarted := newNode(t, fab, crashLoopJoinBus)
		restarted.keys = store
		restarted.seq = records[0].Seq
		restarted.persist = func(r idem.Record) error { return appendAppliedKey(logPath, r) }

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Two arrivals now converge on the restarted bus, and they are the two
		// this task cares about:
		//
		//  (a) RELAY-4's RETRY of the copy that was in flight when the bus died.
		//      Its ack was lost with the process, so a retrying forwarder WILL
		//      resend it. IDEM-15 point (d) names this as the duplicate source
		//      relay-side durability actually exists to defend against.
		//  (b) the SECOND DISJOINT-PATH copy, which was still on its way from the
		//      right-hand branch when the bus died.
		//
		// Both must be answered with the ORIGINAL local id and must apply
		// nothing. Different bus paths, one message.
		m := crashLoopMessage()
		viaLeft, err := m.Forward(crashLoopOriginBus)
		if err != nil {
			t.Fatalf("building the origin hop: %v", err)
		}
		viaLeft.BusPath = []string{crashLoopOriginBus, crashLoopHubBus, crashLoopLeftBus}
		viaRight := viaLeft
		viaRight.BusPath = []string{crashLoopOriginBus, crashLoopHubBus, crashLoopRightBus}

		// The arrivals go over a REAL HTTPS request to the restarted bus's REAL
		// RelayHandler, not straight into the accept callback. Calling the
		// callback would skip the handler entirely — the ingest validation, the
		// status mapping, and the 200-with-duplicate:true answer are exactly the
		// surface a retrying peer sees after a restart, so they are the surface
		// under test.
		for _, tc := range []struct {
			name string
			req  RelayRequest
		}{
			{"RELAY-4's retry of the copy whose ack died with the bus", viaLeft},
			{"the second copy, still in flight by the disjoint path", viaRight},
		} {
			resp, err := restarted.cli.Relay(ctx, restarted.srv.URL, tc.req)
			if err != nil {
				t.Fatalf("%s: relaying to the restarted bus: %v", tc.name, err)
			}
			if !resp.Accepted || !resp.Duplicate {
				t.Fatalf("%s gave %+v, want accepted+duplicate. The applied-key record was fsynced before the process died, so recovery must recognise it; anything else delivers the message twice to the local agents and consumes a second local sequence. A 409 here would be worse still — it would punish the peer that retried correctly.", tc.name, resp)
			}
			if resp.MessageID != originalLocalID {
				t.Errorf("%s was answered with %q, want the ORIGINAL result %q replayed verbatim (invariant 10)", tc.name, resp.MessageID, originalLocalID)
			}
		}
		if got := restarted.h.Stats(); got.Duplicates != 2 || got.Rejected != 0 || got.Accepted != 0 {
			t.Errorf("restarted handler stats = %+v, want Duplicates=2, Rejected=0, Accepted=0", got)
		}

		// INVARIANT 1 ACROSS THE RESTART, and the reason the recovered sequence
		// is restored at all: a genuinely NEW message must mint bus-ce-2. If the
		// counter had restarted at zero this would be bus-ce-1 — the id the
		// pre-crash delivery already holds — and two different messages would
		// share one local id. That invariant was reaffirmed WITHOUT narrowing.
		fresh := originMessage(crashLoopOriginBus, crashLoopSender, crashLoopOriginSeq+1, []byte("a different message, after the restart"), func(m *RelayedMessage) {
			m.Recipients = []string{crashLoopJoinBus + ".target-1"}
		})
		freshReq, err := fresh.Forward(crashLoopOriginBus)
		if err != nil {
			t.Fatalf("building the fresh message's origin hop: %v", err)
		}
		freshReq.BusPath = []string{crashLoopOriginBus, crashLoopHubBus, crashLoopLeftBus}
		freshResp, err := restarted.cli.Relay(ctx, restarted.srv.URL, freshReq)
		if err != nil {
			t.Fatalf("relaying a fresh message to the restarted bus: %v", err)
		}
		if freshResp.Duplicate || !freshResp.Accepted {
			t.Fatalf("a genuinely new message gave %+v, want accepted and NOT duplicate", freshResp)
		}
		if want := crashLoopJoinBus + "-2"; freshResp.MessageID != want {
			t.Fatalf("the first post-restart message minted %q, want %q. Ids are NEVER reused, including across restarts (invariant 1, reaffirmed without narrowing) — %q is the id the pre-crash delivery already holds.", freshResp.MessageID, want, originalLocalID)
		}

		// Exactly ONE delivery after the restart, and it is the FRESH message —
		// never the one that was already applied before the crash.
		if got := restarted.deliveredIDs(); len(got) != 1 || got[0] != crashLoopOriginBus+"-2" {
			t.Fatalf("the restarted bus delivered %v, want only the fresh message [%s-2]; the two duplicate arrivals were work already applied before the crash and must deliver nothing", got, crashLoopOriginBus)
		}
		if dup, violations := restarted.counters(); dup != 2 || violations != 0 {
			t.Fatalf("the restarted bus counted %d duplicates and %d violations, want 2 and 0: a retry and a disjoint-path copy are both RETRIES, and punishing either would break exactly the peers behaving correctly", dup, violations)
		}

		// The durable log grew by exactly ONE — the fresh message. A suppressed
		// duplicate writes nothing, or a later recovery would see two
		// applications of one message.
		_, after := recoverAppliedKeys(t, logPath)
		if len(after) != 2 {
			t.Fatalf("the applied-key log holds %d records, want 2 (the pre-crash record plus the one fresh message); two duplicate arrivals must have written nothing at all", len(after))
		}
	})
}

// ---------------------------------------------------------------------------
// RELAY-19: the FORWARDER writes and settles durable outbox records
// ---------------------------------------------------------------------------
//
// RELAY-15's own crash evidence (TestOutboxSurvivesACrashMidEnqueue, in
// outbox_test.go) proves that a RECORD survives a SIGKILL. It says nothing
// about the forwarder, because part 1 deliberately did not touch it: the outbox
// was a library nothing called.
//
// What is proved here is the property that library was built for, and it is a
// property of the WHOLE PATH rather than of the record:
//
//	a message handed to Forwarder.Enqueue and then lost to a kill -9 is
//	re-enqueued on recovery, delivered, and delivered EXACTLY ONCE.
//
// Three things make that claim non-trivial, and each is asserted separately
// below rather than inferred from the others:
//
//  1. THE ORDER. The pending record is fsynced BEFORE the job is offered to any
//     queue. The child dies inside the durable write, so if the order were the
//     other way round the peer stand-in would already have a delivery on disk.
//     The parent asserts that file does NOT exist — which is the only way to
//     observe an ordering from outside a process that no longer exists, and is
//     the shape RELAY-15's own child uses for the same reason.
//  2. THE RE-ENQUEUE. The parent replays the crashed log into a fresh Outbox,
//     builds a fresh Forwarder, and calls Resume — the startup sequence a
//     restarted server performs — and the peer receives the message.
//  3. EXACTLY ONCE, in both directions it can fail. Within the process, a
//     second Resume is refused. Across restarts, a THIRD open of the same
//     directory must find an EMPTY pending set: the delivered tombstone is what
//     stops a recovered job being re-sent forever, and without assertion 3 a
//     forwarder that re-sent on every boot would pass 1 and 2 perfectly.

const (
	// envFwdCrashChild selects the child role. Unset means "not a crash child",
	// so the child test is a no-op skip in an ordinary package run. The names
	// are distinct from outbox_test.go's RELAY_OUTBOX_CRASH_* pair because the
	// two harnesses re-exec DIFFERENT child tests, and a shared variable would
	// let one harness's child be started by the other's parent.
	envFwdCrashChild = "RELAY_FORWARD_OUTBOX_CRASH_CHILD"
	// envFwdCrashDir is the data directory the child writes into: always a
	// parent-owned t.TempDir(), never the tracked data/ dir.
	envFwdCrashDir = "RELAY_FORWARD_OUTBOX_CRASH_DIR"

	fwdCrashLocalBus = "bus-fwd-local"
	fwdCrashPeerBus  = "bus-fwd-peer"
	fwdCrashSeq      = 9001

	// fwdCrashDeliveries is the peer stand-in's on-disk record of what actually
	// reached it. It is a FILE rather than a counter because the two processes
	// that write to it do not share memory, and because "nothing arrived before
	// the crash" is a claim about the child that only the parent can check.
	fwdCrashDeliveries = "peer-deliveries.log"
)

// fwdCrashMessage is the message under test. Every field the outbox record
// derives from — origin message id, size, content hash — is deterministic, so
// the job id the child wrote and the job id the parent recomputes are the same
// string in two processes.
func fwdCrashMessage() RelayedMessage {
	return originMessage(fwdCrashLocalBus, fwdCrashLocalBus+".alpha-1", fwdCrashSeq, []byte("a relay hop that must outlive a kill -9"), func(m *RelayedMessage) {
		m.Recipients = []string{fwdCrashPeerBus + ".target-1"}
	})
}

// fwdCrashPeerServer is a peer bus's relay ingress that RECORDS what it
// received, appending one line per accepted envelope and fsyncing before it
// answers. The fsync is fidelity to the real path: the parent reads this file
// after the child has been SIGKILLed, and a buffered write would make "nothing
// arrived" indistinguishable from "something arrived and was never flushed".
func fwdCrashPeerServer(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	path := filepath.Join(dir, fwdCrashDeliveries)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, MaxRelayBytes))
		var req RelayRequest
		line := "undecodable\n"
		if err := json.Unmarshal(body, &req); err == nil {
			line = req.MessageID + "\n"
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := f.WriteString(line); err != nil {
			_ = f.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = f.Close()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RelayResponse{Accepted: true, MessageID: fwdCrashPeerBus + "-1"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fwdCrashDeliveryLines is what the peer stand-in actually received, or nil if
// it was never called at all.
func fwdCrashDeliveryLines(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, fwdCrashDeliveries))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the peer stand-in's delivery log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// fwdCrashForwarder wires a Registry, a Client and a durable Forwarder around
// the peer stand-in. The child and the parent build it IDENTICALLY apart from
// the outbox, so a difference between them cannot be the reason the parent
// succeeds where the child died.
func fwdCrashForwarder(t *testing.T, ob *Outbox, srv *httptest.Server) *Forwarder {
	t.Helper()
	reg, err := NewRegistry(RegistryOptions{BusID: fwdCrashLocalBus})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.UpsertPeer(PeerRoster{BusID: fwdCrashPeerBus, Agents: []string{fwdCrashPeerBus + ".target-1"}}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	cli, err := NewClient(ClientConfig{
		BusID:       fwdCrashLocalBus,
		LocalRoster: func() []string { return nil },
		HTTPClient:  srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	fwd, err := NewForwarder(ForwarderOptions{
		BusID:    fwdCrashLocalBus,
		Registry: reg,
		Client:   cli,
		Timeout:  5 * time.Second,
		PeerBaseURL: func(busID string) (string, bool) {
			if busID != fwdCrashPeerBus {
				return "", false
			}
			return srv.URL, true
		},
		Outbox: ob,
		// The stand-in for the durable message store. The real wiring reads
		// store.Message by origin id; what matters to this test is that the
		// forwarder can rebuild an envelope it did NOT carry across the crash,
		// because the record deliberately holds routing facts and not a body.
		RecoverMessage: func(originMessageID string) (RelayedMessage, bool, error) {
			m := fwdCrashMessage()
			if originMessageID != m.OriginMessageID {
				return RelayedMessage{}, false, nil
			}
			return m, true, nil
		},
	})
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}
	return fwd
}

// fwdKillOnPending delegates to the REAL *wal.Log.Write — prepare, commit and
// both fsyncs — and kills the process the instant a PENDING outbox record is on
// stable storage, before Outbox.Enqueue can return and therefore before
// Forwarder.Enqueue can offer anything to a queue.
type fwdKillOnPending struct{ l *wal.Log }

func (k *fwdKillOnPending) Write(e wal.Entry) (wal.Committed, error) {
	// Checked HERE because this is the only place the entry can be seen before
	// it is written; the parent would otherwise be asserting over bytes that
	// never meant what it assumed.
	if e.Kind != OutboxRecordKind {
		return wal.Committed{}, fmt.Errorf("child: the outbox handed the log an entry of kind %q, want %q", e.Kind, OutboxRecordKind)
	}
	rec, err := DecodeOutboxRecord(e.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the entry does not decode as an outbox record: %v", err)
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	if rec.State == OutboxPending {
		obKillSelf()
	}
	return c, nil
}

// TestRelayForwardOutboxCrashChild is the child half. It does NOTHING in an
// ordinary run.
func TestRelayForwardOutboxCrashChild(t *testing.T) {
	if os.Getenv(envFwdCrashChild) == "" {
		t.Skip("not a crash child: " + envFwdCrashChild + " is unset")
	}
	dir := os.Getenv(envFwdCrashDir)
	if dir == "" {
		t.Fatalf("child: %s is set but %s is empty", envFwdCrashChild, envFwdCrashDir)
	}

	srv := fwdCrashPeerServer(t, dir)

	// NO deferred Close on the log: a Close that ran would be exactly the
	// graceful shutdown this test exists to rule out.
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("child: wal.Open(%s): %v", dir, err)
	}
	ob, err := NewOutbox(OutboxOptions{BusID: fwdCrashLocalBus, Durable: &fwdKillOnPending{l: lg}})
	if err != nil {
		t.Fatalf("child: NewOutbox: %v", err)
	}

	fwd := fwdCrashForwarder(t, ob, srv)
	queued, err := fwd.Enqueue(fwdCrashMessage())
	t.Fatalf("child: Enqueue returned (%d, %v), but the durable log kills this process the instant the PENDING record is fsynced. "+
		"Either the record was never written — in which case the forwarder is not recording deliveries at all — or it was written AFTER the offer, "+
		"which is the ordering RELAY-19 exists to establish", queued, err)
}

// fwdRunCrashChild re-execs this test binary as the crash child and asserts it
// really DIED ON SIGKILL. Without the wait-status check a child that merely
// failed its own assertions would masquerade as a crash and leave the parent
// asserting over an empty directory — which passes.
func fwdRunCrashChild(t *testing.T, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v; refusing to fall back to os.Args[0], which exec.Command would resolve through PATH", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestRelayForwardOutboxCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envFwdCrashChild+"=1", envFwdCrashDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the crash child did not finish in time: %v\n--- child output ---\n%s", ctx.Err(), out.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child: wait status is %T, want syscall.WaitStatus", ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			ws.ExitStatus(), out.String())
	}
}

// fwdWaitFor polls until cond holds, and fails with why if it never does. Used
// instead of a sleep because the settle happens on a peer worker goroutine.
func fwdWaitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// fwdOpenOutbox reopens dir the way a restarting server does: a fresh Outbox
// wired as the log's Applier, so recovery runs the real replay path.
func fwdOpenOutbox(t *testing.T, dir string) (*Outbox, *wal.Log) {
	t.Helper()
	ob, err := NewOutbox(OutboxOptions{BusID: fwdCrashLocalBus, Logger: logging.New(&obLogSink{}, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ob})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	ob.durable = lg
	return ob, lg
}

// TestRelayOutboxSurvivesCrash is RELAY-19's crash evidence.
func TestRelayOutboxSurvivesCrash(t *testing.T) {
	dir := t.TempDir()
	wantJob := DeriveJobID(fwdCrashPeerBus, fwdCrashMessage().OriginMessageID)

	fwdRunCrashChild(t, dir)

	// -----------------------------------------------------------------------
	// 1. THE ORDER: durable first, wire second.
	// -----------------------------------------------------------------------
	if got := fwdCrashDeliveryLines(t, dir); len(got) != 0 {
		t.Fatalf("the peer received %v BEFORE the crash. The child dies inside the durable write of the PENDING record, so anything on the wire at that point means the job was offered to the queue before it was recorded — the exact ordering RELAY-19 is about", got)
	}

	// -----------------------------------------------------------------------
	// 2. THE RECORD survived, and it is PENDING.
	// -----------------------------------------------------------------------
	recs := obRecordsIn(t, obReplayCommitted(t, dir))
	if len(recs) != 1 {
		t.Fatalf("the crashed log holds %d outbox records, want exactly 1: the child was killed the instant the first PENDING commit was fsynced", len(recs))
	}
	if recs[0].State != OutboxPending || recs[0].JobID != wantJob {
		t.Fatalf("the recovered record is %+v, want a PENDING record for job %s", recs[0], wantJob)
	}

	// -----------------------------------------------------------------------
	// 3. THE RESTART re-enqueues it, and the peer gets it.
	// -----------------------------------------------------------------------
	ob, lg := fwdOpenOutbox(t, dir)
	if got := obPendingIDs(ob); len(got) != 1 || got[0] != wantJob {
		t.Fatalf("replay rebuilt the pending set as %v, want exactly [%s]", got, wantJob)
	}

	srv := fwdCrashPeerServer(t, dir)
	fwd := fwdCrashForwarder(t, ob, srv)

	requeued, err := fwd.Resume()
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("Resume re-enqueued %d jobs, want 1: the message was durably owed and nothing had settled it", requeued)
	}

	fwdWaitFor(t, "the recovered job to settle DELIVERED", func() bool {
		r, ok := ob.Lookup(wantJob)
		return ok && r.State == OutboxDelivered
	})
	if got := fwdCrashDeliveryLines(t, dir); len(got) != 1 || got[0] != fwdCrashMessage().OriginMessageID {
		t.Fatalf("the peer received %v after the restart, want exactly one copy of %s", got, fwdCrashMessage().OriginMessageID)
	}

	// -----------------------------------------------------------------------
	// 4. EXACTLY ONCE, within the process.
	// -----------------------------------------------------------------------
	if n, err := fwd.Resume(); !errors.Is(err, ErrForwarderResumed) || n != 0 {
		t.Fatalf("a second Resume returned (%d, %v), want (0, ErrForwarderResumed). A repeated Resume that re-offered the pending set would deliver every recovered job twice", n, err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fwd.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// -----------------------------------------------------------------------
	// 5. EXACTLY ONCE, across restarts — the tombstone is what proves it.
	// -----------------------------------------------------------------------
	//
	// Without this a forwarder that re-sent every recovered job on EVERY boot
	// would pass all four assertions above and still deliver the message once
	// per restart, forever.
	again, lg2 := fwdOpenOutbox(t, dir)
	defer func() { _ = lg2.Close() }()
	if got := obPendingIDs(again); len(got) != 0 {
		t.Fatalf("a second restart found %v still pending; the delivered settlement must retire the job, or every boot re-sends a message the peer already took", got)
	}
	r, ok := again.Lookup(wantJob)
	if !ok || r.State != OutboxDelivered {
		t.Fatalf("after a second restart job %s is %+v (present=%v), want a DELIVERED tombstone", wantJob, r, ok)
	}
	if got := fwdCrashDeliveryLines(t, dir); len(got) != 1 {
		t.Fatalf("the peer received %d copies in total, want exactly 1", len(got))
	}
}

// ---------------------------------------------------------------------------
// RELAY-19: the settle paths, without a crash
// ---------------------------------------------------------------------------

// fwdCountingPeer answers every relay attempt with `status`, counting arrivals.
// It is separate from fwdCrashPeerServer because these tests never cross a
// process boundary and want the count in memory rather than on disk.
func fwdCountingPeer(t *testing.T, status int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var got atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got.Add(1)
		if status != http.StatusOK {
			http.Error(w, "the peer refuses this envelope", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RelayResponse{Accepted: true, MessageID: fwdCrashPeerBus + "-1"})
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// fwdSettleFixture is a durable forwarder over an in-memory log.
func fwdSettleFixture(t *testing.T, srv *httptest.Server) (*Forwarder, *Outbox, *obLogSink) {
	t.Helper()
	sink := &obLogSink{}
	ob, err := NewOutbox(OutboxOptions{
		BusID:   fwdCrashLocalBus,
		Durable: &obNullDurable{},
		Logger:  logging.New(sink, logging.LevelDebug),
	})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	return fwdCrashForwarder(t, ob, srv), ob, sink
}

// fwdBlockingDurable holds the PENDING write open so a test can look at the
// world while the record is mid-fsync — the window in which the wrong ordering
// would already have put the message on the wire.
type fwdBlockingDurable struct {
	inner   obNullDurable
	entered chan struct{}
	release chan struct{}
}

func (d *fwdBlockingDurable) Write(e wal.Entry) (wal.Committed, error) {
	if rec, err := DecodeOutboxRecord(e.Body); err == nil && rec.State == OutboxPending {
		d.entered <- struct{}{}
		<-d.release
	}
	return d.inner.Write(e)
}

// TestForwarderSettlesOutboxRecords is RELAY-19's proof: the forwarder WRITES
// the pending record before it offers, and SETTLES it — one way or the other —
// for every outcome that is not a shutdown.
//
// The four cases below are the four different answers the code must give, and
// they are asserted separately because getting one right says nothing about the
// others: a settle-everything forwarder passes 1 and 2 and destroys 4, and a
// settle-nothing forwarder passes 4 and loses every guarantee in 1 and 2.
func TestForwarderSettlesOutboxRecords(t *testing.T) {
	msg := fwdCrashMessage()
	wantJob := DeriveJobID(fwdCrashPeerBus, msg.OriginMessageID)

	t.Run("the pending record is durable BEFORE anything is offered to a queue", func(t *testing.T) {
		srv, received := fwdCountingPeer(t, http.StatusOK)
		blocking := &fwdBlockingDurable{entered: make(chan struct{}, 1), release: make(chan struct{})}
		ob, err := NewOutbox(OutboxOptions{BusID: fwdCrashLocalBus, Durable: blocking, Logger: logging.New(&obLogSink{}, logging.LevelDebug)})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		fwd := fwdCrashForwarder(t, ob, srv)
		defer closeForwarder(t, fwd)

		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := fwd.Enqueue(msg); err != nil {
				t.Errorf("Enqueue: %v", err)
			}
		}()

		select {
		case <-blocking.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the forwarder never attempted a durable write; it is not recording deliveries at all")
		}
		// THE WINDOW. The pending write is in flight, so nothing may have been
		// offered yet — and therefore nothing may be on the wire. This is the
		// in-process half of the crash test's assertion 1; it is cheap, it is
		// deterministic, and it fails the moment the two are reordered.
		time.Sleep(50 * time.Millisecond)
		if n := received.Load(); n != 0 {
			t.Fatalf("the peer received %d copies while the PENDING record was still being written; the record must be durable BEFORE the job is offered", n)
		}
		close(blocking.release)
		<-done
		fwdWaitFor(t, "the delivered settlement", func() bool {
			r, ok := ob.Lookup(wantJob)
			return ok && r.State == OutboxDelivered
		})
	})

	t.Run("a peer that accepts settles the job DELIVERED", func(t *testing.T) {
		srv, received := fwdCountingPeer(t, http.StatusOK)
		fwd, ob, _ := fwdSettleFixture(t, srv)
		defer closeForwarder(t, fwd)

		if n, err := fwd.Enqueue(msg); err != nil || n != 1 {
			t.Fatalf("Enqueue = (%d, %v), want (1, nil)", n, err)
		}
		if r, ok := ob.Lookup(wantJob); !ok || r.State != OutboxPending {
			t.Fatalf("immediately after Enqueue job %s is %+v (present=%v), want PENDING: the record is written before the offer", wantJob, r, ok)
		}
		fwdWaitFor(t, "the delivered settlement", func() bool {
			r, ok := ob.Lookup(wantJob)
			return ok && r.State == OutboxDelivered
		})
		if got := received.Load(); got != 1 {
			t.Fatalf("the peer received %d copies, want 1", got)
		}
		if got := ob.Pending(); len(got) != 0 {
			t.Fatalf("the pending set is %v after a delivery, want empty: a settled job must not be re-offered by a later Resume", got)
		}
		if r, _ := ob.Lookup(wantJob); r.Reason != "" {
			t.Fatalf("the delivered record carries reason %q; only an abandoned job has one", r.Reason)
		}
	})

	t.Run("a permanent refusal settles the job ABANDONED, with a reason", func(t *testing.T) {
		srv, received := fwdCountingPeer(t, http.StatusBadRequest)
		fwd, ob, _ := fwdSettleFixture(t, srv)
		defer closeForwarder(t, fwd)

		if n, err := fwd.Enqueue(msg); err != nil || n != 1 {
			t.Fatalf("Enqueue = (%d, %v), want (1, nil)", n, err)
		}
		fwdWaitFor(t, "the abandonment", func() bool {
			r, ok := ob.Lookup(wantJob)
			return ok && r.State == OutboxAbandoned
		})
		r, _ := ob.Lookup(wantJob)
		// Invariant 6: a message this bus will never deliver is recorded
		// SPECIFICALLY, not merely recorded. An abandonment that said "abandoned"
		// and nothing else would satisfy the record's own validate and tell an
		// operator nothing.
		if !strings.Contains(r.Reason, "permanently") {
			t.Fatalf("the abandonment reason is %q; it must say WHY this message will never reach the peer", r.Reason)
		}
		if got := received.Load(); got != 1 {
			t.Fatalf("the peer was called %d times, want exactly 1: a 400 is final and must not be retried", got)
		}
		if got := fwd.Stats().Dropped.Permanent; got != 1 {
			t.Fatalf("Dropped.Permanent = %d, want 1", got)
		}
	})

	t.Run("a SHUTDOWN leaves the job PENDING so the next start re-offers it", func(t *testing.T) {
		// The whole point of the durable outbox: the four shutdown paths in
		// forward.go used to be uncounted, silent loss. None of them may settle
		// anything — an abandonment here would convert a recoverable shutdown
		// back into the permanent loss it used to be.
		hanging := newPeerServer(t, true)
		fwd, ob, _ := fwdSettleFixture(t, hanging.srv)

		if n, err := fwd.Enqueue(msg); err != nil || n != 1 {
			t.Fatalf("Enqueue = (%d, %v), want (1, nil)", n, err)
		}
		fwdWaitFor(t, "the attempt to reach the hanging peer", func() bool { return hanging.hanging.Load() > 0 })

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := fwd.Close(ctx); err == nil {
			t.Fatal("Close returned nil against a hanging peer, so the shutdown path under test never ran")
		}
		r, ok := ob.Lookup(wantJob)
		if !ok || r.State != OutboxPending {
			t.Fatalf("after a shutdown mid-attempt job %s is %+v (present=%v), want PENDING. A shutdown is not a verdict on the message, and settling it here is exactly the silent loss RELAY-19 removes", wantJob, r, ok)
		}
		if got := obPendingIDs(ob); len(got) != 1 || got[0] != wantJob {
			t.Fatalf("the pending set is %v, want [%s]: the job must still be owed so Resume re-offers it", got, wantJob)
		}
	})
}

// closeForwarder shuts a forwarder down and fails on anything but a clean stop.
func closeForwarder(t *testing.T, f *Forwarder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestForwarderResumeDoesNotAbandonWhatItCannotRoute covers the two guards the
// RELAY-19 review added, both of which turn a WIRING mistake into something
// recoverable instead of something durable.
func TestForwarderResumeDoesNotAbandonWhatItCannotRoute(t *testing.T) {
	msg := fwdCrashMessage()
	wantJob := DeriveJobID(fwdCrashPeerBus, msg.OriginMessageID)

	t.Run("a recovered job with no route stays OWED rather than being abandoned", func(t *testing.T) {
		// The shape this defends against: Resume running before the peer roster
		// is restored. Every peer then looks unknown at once, so abandoning would
		// destroy the WHOLE recovered backlog, durably, at boot.
		srv, received := fwdCountingPeer(t, http.StatusOK)
		ob, err := NewOutbox(OutboxOptions{BusID: fwdCrashLocalBus, Durable: &obNullDurable{}, Logger: logging.New(&obLogSink{}, logging.LevelDebug)})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		if _, err := ob.Enqueue(OutboxJob{
			PeerBusID:       fwdCrashPeerBus,
			OriginMessageID: msg.OriginMessageID,
			Size:            len(msg.Body),
			ContentSHA256:   msg.ContentSHA256,
		}); err != nil {
			t.Fatalf("seeding the pending job: %v", err)
		}

		reg, err := NewRegistry(RegistryOptions{BusID: fwdCrashLocalBus})
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		cli, err := NewClient(ClientConfig{BusID: fwdCrashLocalBus, LocalRoster: func() []string { return nil }, HTTPClient: srv.Client()})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		fwd, err := NewForwarder(ForwarderOptions{
			BusID:    fwdCrashLocalBus,
			Registry: reg,
			Client:   cli,
			// THE EMPTY ROSTER: nothing resolves, exactly as it would before the
			// peers were loaded.
			PeerBaseURL:    func(string) (string, bool) { return "", false },
			Outbox:         ob,
			RecoverMessage: func(string) (RelayedMessage, bool, error) { return msg, true, nil },
		})
		if err != nil {
			t.Fatalf("NewForwarder: %v", err)
		}
		defer closeForwarder(t, fwd)

		n, err := fwd.Resume()
		if err != nil || n != 0 {
			t.Fatalf("Resume = (%d, %v), want (0, nil): nothing can be routed", n, err)
		}
		r, ok := ob.Lookup(wantJob)
		if !ok || r.State != OutboxPending {
			t.Fatalf("job %s is %+v (present=%v) after an unroutable Resume, want it still PENDING. Abandoning here would destroy the whole recovered backlog on a startup-ordering bug", wantJob, r, ok)
		}
		if got := received.Load(); got != 0 {
			t.Fatalf("the peer received %d copies, want 0", got)
		}
	})

	t.Run("a forwarder that would out-retry its own outbox is refused at construction", func(t *testing.T) {
		// A shorter outbox horizon sweeps a job while the forwarder still holds it
		// live: the message goes out and its settlement is then refused as an
		// unknown job — sent, with no durable record that it was.
		ob, err := NewOutbox(OutboxOptions{
			BusID:            fwdCrashLocalBus,
			Durable:          &obNullDurable{},
			RetryHorizon:     time.Hour,
			SettledRetention: time.Hour,
		})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		reg, err := NewRegistry(RegistryOptions{BusID: fwdCrashLocalBus})
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		srv, _ := fwdCountingPeer(t, http.StatusOK)
		cli, err := NewClient(ClientConfig{BusID: fwdCrashLocalBus, LocalRoster: func() []string { return nil }, HTTPClient: srv.Client()})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		_, err = NewForwarder(ForwarderOptions{
			BusID:          fwdCrashLocalBus,
			Registry:       reg,
			Client:         cli,
			PeerBaseURL:    func(string) (string, bool) { return srv.URL, true },
			Outbox:         ob,
			RecoverMessage: func(string) (RelayedMessage, bool, error) { return msg, true, nil },
		})
		if err == nil {
			t.Fatal("NewForwarder accepted a retry horizon longer than the outbox retains a pending record; the settle would later fail as an unknown job")
		}
		if !strings.Contains(err.Error(), "outbox") {
			t.Fatalf("NewForwarder error is %q; it must name the outbox horizon it disagrees with", err)
		}
	})
}
