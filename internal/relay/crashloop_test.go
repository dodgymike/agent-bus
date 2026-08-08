package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
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
