package relay

// ACK-7 — RETRY, IDEMPOTENCY AND EXACTLY-ONCE TERMINAL HANDLING, AT THE RELAY LAYER.
//
// The proof command names one test: TestAckTerminalExactlyOnceUnderRetry.
//
// # WHAT THIS FILE IS FOR, AND WHAT IT DELIBERATELY DOES NOT REPEAT
//
// ACK-8 (d454ef7) already owns DURABLE RESTART idempotency inside internal/ack:
// a retry and a conflict staying distinct across a reopen, a settled row that a
// restart cannot resurrect, a log replayed twice being a no-op, and a crash at
// prepare not being recovered. None of that is re-tested here. Neither is
// ackhttp_test.go's status mapping for the UNRACED paths.
//
// The hole this file closes is CONCURRENCY. Before it there was no `go func`
// and no sync.WaitGroup anywhere in the relay ACK plane: every existing test
// drives one frame at a time, so the one mechanism that makes "exactly once"
// true — ack.Store's per-pair reservation held ACROSS THE FSYNC — was never
// exercised by two frames at once, and deleting it left every test green.
//
// # THE CENTRAL CLAIM, AND WHY THE OBVIOUS "FIX" WOULD BREAK INVARIANT 4
//
// A BYTE-IDENTICAL RETRY THAT LOSES THE IN-FLIGHT RESERVATION RACE IS ANSWERED
// 503 (RETRIABLE), NOT 200 {duplicate:true}. THAT IS CORRECT AND MUST NOT BE
// "FIXED".
//
// At the instant the loser is refused, the WINNER'S FSYNC HAS NOT COMPLETED —
// ack.Store.Settle reserves the pair, releases the mutex, and only then calls
// DurableLog.Write. Answering the loser 200 would be this bus telling a peer
// "your terminal outcome is recorded" about a record that is not yet on stable
// storage, and that is precisely INVARIANT 4: nothing is acknowledged before it
// is durable. Invariant 10's "a legitimate retry must not be punished" and
// invariant 4 meet at this one line, and INVARIANT 4 WINS — because the cost of
// the 503 is one more round trip on a retriable status, while the cost of the
// 200 is an acknowledgement that can be falsified by a power cut.
//
// Reporting the concurrent loser as a duplicate LOOKS like an improvement (it
// removes a "spurious" 503 from a peer doing the right thing) and is the exact
// change this file exists to keep red.
//
// # THE SECOND THING THAT MUST NEVER BE CONFLATED
//
// ack.ErrConcurrentTransition (TRANSIENT — 503, retriable, nothing written) and
// ack.ErrTerminal (PERMANENT — 409 CodeIdempotencyViolation, a protocol
// violation) are different facts about different failures. If a race were
// reported as a conflict, an honest peer retrying its own byte-identical
// acknowledgement would be told it had committed a protocol violation, and
// PeerRefusedError.Retriable would make it ABANDON a terminal outcome that
// nothing is wrong with. The mirror below therefore asserts, structurally, that
// cmd/agent-bus's settleAck does not branch on ErrConcurrentTransition at all.
//
// # INVARIANTS READ IN FULL BEFORE WRITING THIS: 4 and 10. SKIMMED: 1, 5, 6.
//
//	4  nothing acknowledged before durable  -> the 503-not-200 ruling above, and
//	   "exactly one WAL write" being COUNTED rather than inferred.
//	10 the three cases never collapsed      -> identical retry = 200/duplicate,
//	   conflicting terminal = 409 reject-and-log, and NO disconnect anywhere
//	   (ACK-CONTRACT.md §12's ruling).
//	1  ids are server-minted                -> every id here comes from ids.*.
//	5  disk is truth                        -> the durable row is re-read from
//	   ack.Store after every phase, never assumed from the HTTP status.
//	6  discards are loud                    -> the conflict must appear in the
//	   operator log, not merely on the wire.
//
// # NO DISCONNECT
//
// ACK-CONTRACT.md §12 rules that NO new disconnect exists anywhere in the ACK
// plane. ackhttp_test.go already asserts that for the malformed/unbound
// refusals over a real socket; this file EXTENDS the posture to the two paths it
// could not reach — the raced 503 and the terminal-conflict 409 — rather than
// repeating the cases it already covers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// The durable log under test: a REAL wal.Log, with every write COUNTED.
// ---------------------------------------------------------------------------

// a7Log sits between ack.Store and a real on-disk *wal.Log and remembers every
// record that was written.
//
// The count is the whole point of assertion 1. "Exactly once" is a claim about
// how many times something was made durable, and a test that infers it from
// `Lookup` returning one row cannot tell one write from five writes of the same
// value — the in-memory fold is idempotent by design (upsertLocked), so the
// table looks identical either way. Counting at the log is the only place the
// difference is observable.
//
// gate, when armed, BLOCKS THE FIRST WRITE before it reaches the real log. That
// is what makes the reservation race deterministic instead of hoped for: while
// one request is parked there it holds ack.Store's per-pair reservation and has
// NOT fsynced, which is exactly the window the central claim above is about.
type a7Log struct {
	real *wal.Log

	mu      sync.Mutex
	calls   int          // Write entered — including one that then fails
	records []ack.Record // Write returned successfully, decoded

	gate    chan struct{} // closed by the test to let the parked write proceed
	entered chan struct{} // closed by the log when a write parks
}

func (l *a7Log) Write(e wal.Entry) (wal.Committed, error) {
	l.mu.Lock()
	l.calls++
	gate, entered := l.gate, l.entered
	// ONE-SHOT: the gate catches the FIRST write only, so the release below
	// cannot accidentally park a later phase of the same test.
	l.gate, l.entered = nil, nil
	l.mu.Unlock()

	if gate != nil {
		close(entered)
		<-gate
	}

	c, err := l.real.Write(e)
	if err != nil {
		return c, err
	}
	rec, derr := ack.DecodeRecord(e.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	if derr != nil {
		// Recorded as a zero record rather than swallowed: the counts below
		// would otherwise silently under-report a write this test could not
		// decode, which is the shape of a vacuous assertion.
		l.records = append(l.records, ack.Record{})
		return c, nil
	}
	l.records = append(l.records, rec)
	return c, nil
}

// arm parks the next write until the returned release func is called, and hands
// back a channel closed once the write is actually parked.
func (l *a7Log) arm() (entered <-chan struct{}, release func()) {
	g := make(chan struct{})
	e := make(chan struct{})
	l.mu.Lock()
	l.gate, l.entered = g, e
	l.mu.Unlock()
	var once sync.Once
	return e, func() { once.Do(func() { close(g) }) }
}

// writes reports the successful durable writes for one pair, by state.
func (l *a7Log) writes(key, recipient string) []ack.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ack.Record
	for _, r := range l.records {
		if r.CorrelationKey == key && r.Recipient == recipient {
			out = append(out, r)
		}
	}
	return out
}

// terminalWrites reports only the SETTLEMENT writes for one pair. The
// acceptance write (E1) is not one of them, and counting it would make the
// exactly-once assertion off-by-one in the direction that hides a defect.
func (l *a7Log) terminalWrites(key, recipient string) []ack.Record {
	var out []ack.Record
	for _, r := range l.writes(key, recipient) {
		if r.State.Terminal() {
			out = append(out, r)
		}
	}
	return out
}

func (l *a7Log) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// ---------------------------------------------------------------------------
// The SettleAck callback. HARNESS OPTION (b): it MIRRORS production.
// ---------------------------------------------------------------------------

// a7Mirror is a deliberate re-implementation of
// cmd/agent-bus/relaywiring.go's (*federation).settleAck.
//
// # WHY A MIRROR AT ALL, AND WHAT IT COSTS
//
// internal/relay cannot import cmd/agent-bus, so the production callback is
// unreachable from this package. The honest options were:
//
//	(a) a THIN callback that only calls ack.Store.Settle, asserting only what
//	    holds regardless of the wiring; or
//	(b) this mirror, which reproduces settleAck's read-decide-settle sequence.
//
// (b) was chosen because ACK-7's subject is the DECISION, not the store: the
// `duplicate` flag on the wire is produced by relay.DecideAck, and a thin
// callback would never call it, so the whole of invariant 10's three-case
// discrimination would be untested at this layer and a mutation making
// DecideAck answer AckApply where it answers AckReplay would leave this file
// green.
//
// THE COST IS REAL AND IS STATED RATHER THAN HIDDEN: THIS COPY WILL DRIFT FROM
// THE PRODUCTION ONE. Proving a copy correct proves nothing about the original.
// a7AssertMirrorMatchesProduction is the tie-back, and this comment states
// EXACTLY what it proves and what it does not — because a guard described as
// stronger than it is, is worse than no guard.
//
// WHAT IT PROVES. It parses the real (*federation).settleAck with go/ast (so a
// comment cannot satisfy it) and asserts (a) the identifiers the mirror depends
// on are still present, and (b) settleAck still does NOT branch on
// ack.ErrConcurrentTransition. (b) is the load-bearing one: the entire 503
// ruling (DECISIONS.md 2026-08-21) rests on that sentinel falling through to the
// "not now" arm, so if somebody adds a case for it — the plausible-looking
// "fix" that would acknowledge before durability — this goes RED. Mutation-
// proven: adding `case errors.Is(err, ack.ErrConcurrentTransition): return
// AckSettlement{Duplicate: true}` to settleAck reddens phase 0.
//
// WHAT IT DOES NOT PROVE. It is NOT a proof that the mirror performs the same
// SEQUENCE, and an earlier version of this comment claimed it was. Identifier
// presence plus one structural negative admits real semantic drift: the
// AckReplay arm could start reporting Duplicate:false, or the replay
// short-circuit could be neutered so Settle runs on every retry, and this guard
// stays green.
//
// A SECOND LIMIT, measured rather than assumed: the positive checks are
// PRESENCE checks over the whole path, so they catch an identifier disappearing
// ENTIRELY, not one call site out of several. Removing ONE of the two
// `return relay.AckSettlement{}, relay.ErrAckNotBound` sites leaves this green;
// removing BOTH goes red. That is the right sensitivity for a drift detector —
// it answers "does production still speak this vocabulary", not "is every arm
// still wired" — but it is not the stronger claim, so it is not stated as one.
//
// THAT IS NOT A COVERAGE HOLE, because those drifts are owned elsewhere, in the
// package that owns settleAck. Both owners were verified by mutating each arm
// separately rather than assumed:
//
//   - cmd/agent-bus/ackwiring_ack3_test.go, TestSettleAckCorrelatesToTheDurableRecord
//     — the SEQUENCE-AND-SEMANTICS owner for settleAck.
//   - cmd/agent-bus/acktransit_test.go, TestSettleAckDisposition — WHICH DOES NOT
//     EXIST YET. It arrives with ACK-5, and it is named here BECAUSE of what ACK-5
//     changes, not in anticipation of it generally.
//
// THE CITATION ABOVE IS TIME-DEPENDENT, AND THAT IS THE POINT OF SPELLING IT OUT.
// Today ackwiring_ack3_test.go's fixture reaches settleAck's ORIGIN arm and goes
// red when that arm's uniform refusal is broken. ACK-5 splits that arm three ways
// (origin / transit / transit-with-no-seam), and that same fixture — keyed
// ackFedKey = wiringPeerBus + "-1" against busID: wiringLocalBus — then lands on
// the NO-SEAM arm instead. The ORIGIN arm's only owner after ACK-5 is
// TestSettleAckDisposition/AT_THE_ORIGIN. So a reader who checks this citation
// after ACK-5 lands and finds only the ack3 test would be reading a claim that has
// quietly stopped being true. Measured by the ACK-7 security gate, not inferred.
type a7Mirror struct {
	acks *ack.Store

	applied  atomic.Int64
	replayed atomic.Int64

	// beforeSettle runs between the ADVISORY read and ack.Store.Settle, for the
	// FIRST request to reach that point and for no other. It is the only way to
	// reach Settle's own absorbing arm deterministically: every other route to
	// it is decided earlier by DecideAck, which the advisory read feeds.
	//
	// It is NOT a sync.Once — Once.Do BLOCKS every later caller until the first
	// returns, which is precisely the caller this hook needs to let through.
	beforeSettle func()
	hookCalls    atomic.Int64
}

// settle mirrors (*federation).settleAck. Keep the two in step.
func (m *a7Mirror) settle(_ context.Context, s SettledAck) (AckSettlement, error) {
	state, class, attested, err := a7Vocabulary(s.Ack)
	if err != nil {
		return AckSettlement{}, err
	}

	// READ-THEN-SETTLE, and the read is ADVISORY. ack.Store.Settle re-decides
	// under its own lock and is the authority; this read only tells an APPLY
	// from a REPLAY for the `duplicate` field.
	incoming := s.Ack.Terminal()
	prior, hasPrior := m.priorTerminal(s.Ack.CorrelationKey, s.Ack.Recipient)
	decision, err := DecideAck(prior, hasPrior, incoming)
	if err != nil {
		return AckSettlement{}, err
	}
	if decision == AckReplay {
		m.replayed.Add(1)
		// Invariant 10's FIRST case: return the ORIGINAL result, re-apply
		// nothing. No durable write is even attempted.
		return AckSettlement{Duplicate: true}, nil
	}

	if m.beforeSettle != nil && m.hookCalls.Add(1) == 1 {
		m.beforeSettle()
	}

	switch err := m.acks.Settle(s.Ack.CorrelationKey, s.Ack.Recipient, state, class, attested); {
	case err == nil:
		m.applied.Add(1)
		return AckSettlement{Duplicate: false}, nil
	case errors.Is(err, ack.ErrNoRecord):
		// MIRRORS THE ORIGIN ARM ONLY. In production this arm now delegates to
		// (*federation).disposeUnrecordedAck, which tells three cases apart: we
		// are the ORIGIN (return the uniform refusal, what this mirror does),
		// this bus is in TRANSIT and can carry the outcome one hop back, or it
		// is in transit with no back-propagation seam wired. This rig is always
		// the origin — newA7Rig calls Accept for the pair before anything else —
		// so the origin arm is the only one reachable here, and the transit arms
		// belong to ACK-5's own tests, not to this file.
		return AckSettlement{}, ErrAckNotBound
	case errors.Is(err, ack.ErrTerminal):
		return AckSettlement{}, fmt.Errorf("%w: %v", ErrAckOutcomeConflict, err)
	default:
		// ErrNotDurable, a WAL failure, and ErrConcurrentTransition. "Not now"
		// (503), NOTHING WAS WRITTEN. Deliberately NOT an arm of its own: see
		// the file header on why a race must never be spelled as a conflict.
		return AckSettlement{}, err
	}
}

func (m *a7Mirror) priorTerminal(correlationKey, recipient string) (AckTerminal, bool) {
	rec, ok := m.acks.Lookup(correlationKey, recipient)
	if !ok || !rec.State.Terminal() {
		return AckTerminal{}, false
	}
	outcome := rec.State
	var class AckClass
	if rec.Class != "" {
		if !rec.Class.Valid() {
			return AckTerminal{Outcome: outcome}, true
		}
		class = rec.Class
	}
	return AckTerminal{Outcome: outcome, Class: class}, true
}

// a7Vocabulary mirrors cmd/agent-bus/relaywiring.go's ackVocabulary.
func a7Vocabulary(v ValidatedPeerAck) (ack.State, ack.Class, ack.Attestation, error) {
	if !v.Outcome.Terminal() {
		return ack.StateInvalid, "", "", fmt.Errorf("acknowledgement outcome %s is not a terminal durable state", v.Outcome)
	}
	if v.Class != "" && !v.Class.Valid() {
		return ack.StateInvalid, "", "", fmt.Errorf("acknowledgement class %s is outside the closed durable set", v.Class)
	}
	if !v.Attestation.Valid() {
		return ack.StateInvalid, "", "", fmt.Errorf("acknowledgement attestation %s is outside the closed durable set", v.Attestation)
	}
	return v.Outcome, v.Class, v.Attestation, nil
}

// ---------------------------------------------------------------------------
// The rig: a real AckHandler, over a real socket, over a real WAL.
// ---------------------------------------------------------------------------

// a7ClientTimeout bounds every request this file makes. See newA7Rig.
const a7ClientTimeout = 30 * time.Second

type a7Rig struct {
	t      *testing.T
	rig    *ackRig
	store  *ack.Store
	dlog   *a7Log
	mirror *a7Mirror
	srv    *httptest.Server
	client *http.Client

	key       string
	recipient string
	sender    string

	// traceMu guards the connection counters, which the concurrent phase writes
	// from many goroutines.
	traceMu sync.Mutex
	conns   int
	reused  int
}

func newA7Rig(t *testing.T) *a7Rig {
	t.Helper()
	dir := t.TempDir()
	sink := &ackSink{}
	lg := logging.New(sink, logging.LevelDebug)

	store := ack.NewStore(ack.Options{Logger: lg})
	// Opened exactly as cmd/agent-bus does: build the store, Open (which
	// REPLAYS into it), then Attach the write path.
	wl, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: store})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = wl.Close() })
	dlog := &a7Log{real: wl}
	if err := store.Attach(dlog); err != nil {
		t.Fatalf("ack.Store.Attach: %v", err)
	}

	r := &a7Rig{
		t:      t,
		store:  store,
		dlog:   dlog,
		mirror: &a7Mirror{acks: store},
		key:    ackMessageID(t, 7),
		// The recipient lives on the ACKING PEER's bus (ackPeerBus): this bus owes
		// ackPeerBus a copy and ackPeerBus is the peer that settles it, so since
		// ACK-4-FU-RECIPIENT-BINDING the recipient's home bus must equal that peer
		// for the direct arm to bind. A recipient on ackLocalBus (the acking never
		// happens for a recipient on the bus RECEIVING the ack) would now be
		// refused, and the retry loop would spin against a permanent 409.
		recipient: ackAgentID(t, ackPeerBus, "bravo"),
		sender:    ackAgentID(t, ackOriginBus, "alpha"),
	}

	// E1: the sender's bus durably accepted the message. Without this row every
	// settlement would be ErrNoRecord and every assertion below would be about
	// the refusal path instead of the retry path.
	if err := store.Accept(r.key, r.sender, r.recipient); err != nil {
		t.Fatalf("ack.Store.Accept: %v", err)
	}

	r.rig = newAckRig(t, func(cfg *AckConfig) { cfg.SettleAck = r.mirror.settle })
	r.rig.table.owe(ackPeerBus, r.key)

	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// AckHandler is not an http.Handler on purpose; the mount supplies the
		// AUTHENTICATED peer bus id, never a frame field.
		r.rig.h.ServeAuthenticated(w, req, ackPeerBus)
	}))
	t.Cleanup(r.srv.Close)

	// MaxConnsPerHost is deliberately NOT 1 here: the concurrent phase needs
	// several sockets, and the connection-reuse assertion that proves nothing
	// disconnected runs on its own single-connection client.
	// TIMEOUT IS NOT DECORATION HERE. This file deliberately PARKS a write
	// inside the real wal.Log to force the reservation race, so "a request that
	// never returns" is a shape this test manufactures on purpose. Without a
	// client timeout a genuine server hang is indistinguishable from that park
	// and degrades into a package-wide `go test` timeout — a failure with no
	// name, in a different test, minutes later. a7ClientTimeout is far above any
	// legitimate loopback settle and far below the package timeout.
	r.client = &http.Client{
		Timeout:   a7ClientTimeout,
		Transport: &http.Transport{MaxIdleConnsPerHost: 16},
	}
	return r
}

// a7Resp is one complete HTTP answer. Every field is read from the wire; none
// is inferred.
type a7Resp struct {
	status    int
	accepted  bool
	duplicate bool
	code      string
	raw       string
}

func (r a7Resp) String() string {
	return fmt.Sprintf("status=%d accepted=%v duplicate=%v code=%q", r.status, r.accepted, r.duplicate, r.code)
}

// post sends one frame and reads the WHOLE response.
//
// A transport error is returned rather than swallowed: on this route it IS the
// disconnect assertion. Every refusal must arrive as a complete HTTP response.
func (r *a7Rig) post(frame PeerAckRequest) (a7Resp, error) {
	buf, err := json.Marshal(frame)
	if err != nil {
		return a7Resp{}, err
	}
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+PeerAckPath, bytes.NewReader(buf))
	if err != nil {
		return a7Resp{}, err
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			r.traceMu.Lock()
			r.conns++
			if info.Reused {
				r.reused++
			}
			r.traceMu.Unlock()
		},
	}))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return a7Resp{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// A body that cannot be read to completion is a TRUNCATED response,
		// which is the shape a mid-response hang-up takes.
		return a7Resp{}, fmt.Errorf("reading the response body: %w", err)
	}
	out := a7Resp{status: resp.StatusCode, raw: string(body)}
	if resp.StatusCode == http.StatusOK {
		var ok PeerAckResponse
		if err := json.Unmarshal(body, &ok); err != nil {
			return out, fmt.Errorf("decoding the 200 body %q: %w", body, err)
		}
		out.accepted, out.duplicate = ok.Accepted, ok.Duplicate
		return out, nil
	}
	var eb ErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		return out, fmt.Errorf("decoding the error body %q: %w", body, err)
	}
	out.code = eb.Error
	return out, nil
}

// row re-reads the DURABLE row. Invariant 5: the assertions are about what the
// table holds, never about what the status code implied.
func (r *a7Rig) row() (ack.Record, bool) { return r.store.Lookup(r.key, r.recipient) }

// a7Delivered is the byte-identical frame every retry in this file re-sends: a
// recipient-sourced positive terminal, which carries no class.
func (r *a7Rig) a7Delivered() PeerAckRequest { return ackFrame(r.key, r.recipient) }

// a7Conflicting is the SAME (key, recipient) with a DIFFERENT terminal outcome —
// invariant 10's second case. `refused` is recipient-sourced too, so it carries
// a signature and a recipient-emitted class.
func (r *a7Rig) a7Conflicting() PeerAckRequest {
	f := ackFrame(r.key, r.recipient)
	f.Outcome = AckRefused.String()
	f.Class = string(AckRecipientRefusedPolicy)
	f.Attestation = &AckAttestationEnvelope{Signature: make([]byte, signing.SignatureSize)}
	return f
}

// ---------------------------------------------------------------------------
// THE TEST
// ---------------------------------------------------------------------------

func TestAckTerminalExactlyOnceUnderRetry(t *testing.T) {
	// --------------------------------------------------------------------
	// 0. The mirror is tied back to production, or every phase below is a
	//    proof about a copy.
	// --------------------------------------------------------------------
	t.Run("the mirrored settle callback is tied back to cmd/agent-bus", func(t *testing.T) {
		a7AssertMirrorMatchesProduction(t)
	})

	// --------------------------------------------------------------------
	// 1. N CONCURRENT BYTE-IDENTICAL FRAMES -> EXACTLY ONE DURABLE TERMINAL,
	//    AND EXACTLY ONE WAL WRITE. Counted, never inferred.
	// --------------------------------------------------------------------
	t.Run("N concurrent identical frames settle exactly once", func(t *testing.T) {
		r := newA7Rig(t)
		const n = 12

		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		results := make([]a7Resp, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			done.Add(1)
			go func(i int) {
				defer done.Done()
				start.Wait()
				results[i], errs[i] = r.post(r.a7Delivered())
			}(i)
		}
		start.Done()
		done.Wait()

		var got200, gotDup, got503 int
		for i, resp := range results {
			if errs[i] != nil {
				// NO DISCONNECT: every answer on this route is a complete HTTP
				// response. A transport error here means a socket was dropped,
				// and a peer link carries every agent behind that peer (§12).
				t.Fatalf("frame %d did not receive a complete HTTP response: %v. NO ACK-plane refusal may disconnect (ACK-CONTRACT.md §12, invariant 10)", i, errs[i])
			}
			switch {
			case resp.status == http.StatusOK && !resp.duplicate:
				got200++
			case resp.status == http.StatusOK && resp.duplicate:
				gotDup++
			case resp.status == http.StatusServiceUnavailable:
				got503++
				if resp.code != CodeUnavailable {
					t.Errorf("frame %d: 503 carried code %q, want %q", i, resp.code, CodeUnavailable)
				}
			default:
				// THE ASSERTION THAT MATTERS MOST HERE. A 409 for a
				// BYTE-IDENTICAL payload would tell an honest peer retrying a
				// lost acknowledgement that it had committed a protocol
				// violation, and PeerRefusedError.Retriable makes a 4xx FINAL —
				// so the terminal outcome would be abandoned, not retried.
				t.Errorf("frame %d answered %s, want one of 200{duplicate:false}, 200{duplicate:true} or 503. A 409 for a byte-identical retry is invariant 10's first case answered as its second", i, resp)
			}
			if resp.status == http.StatusOK && !resp.accepted {
				t.Errorf("frame %d: a 200 reported accepted=false", i)
			}
		}
		if got200+gotDup == 0 {
			t.Fatalf("not one of the %d identical frames was accepted (200s=%d dup=%d 503s=%d); the phase below would be vacuous", n, got200, gotDup, got503)
		}

		// EXACTLY ONE DURABLE TERMINAL RECORD, COUNTED AT THE LOG.
		terminal := r.dlog.terminalWrites(r.key, r.recipient)
		if len(terminal) != 1 {
			t.Fatalf("%d terminal records were written to the WAL for one pair under %d identical frames, want exactly 1: %+v. The in-memory table is idempotent, so this count is the ONLY place a second write is observable (invariant 4)", len(terminal), n, terminal)
		}
		if got, want := terminal[0].State, AckDelivered; got != want {
			t.Errorf("the one durable terminal is %s, want %s", got, want)
		}
		if terminal[0].Class != "" {
			t.Errorf("the durable terminal carries class %q; a positive outcome carries none (§5.4)", terminal[0].Class)
		}
		if got, want := terminal[0].AttestedBy, AckAttestedRecipientSignatureUnverified; got != want {
			t.Errorf("attested_by = %q, want %q", got, want)
		}

		// AND THE SERVING COPY AGREES WITH DISK (invariant 5).
		rec, ok := r.row()
		if !ok {
			t.Fatal("the pair has no row after settlement")
		}
		if rec.State != AckDelivered {
			t.Errorf("the durable row is %s, want %s", rec.State, AckDelivered)
		}
	})

	// --------------------------------------------------------------------
	// 2. THE CENTRAL CLAIM, MADE DETERMINISTIC: a byte-identical retry that
	//    loses the in-flight reservation race is 503, NEVER 200 and NEVER 409.
	// --------------------------------------------------------------------
	t.Run("a retry that loses the reservation race is 503 and nothing is written", func(t *testing.T) {
		r := newA7Rig(t)

		// The winner is parked INSIDE the durable write: it holds the pair's
		// reservation and its fsync has NOT completed.
		entered, release := r.dlog.arm()
		// RELEASED WHATEVER HAPPENS. A t.Fatalf below aborts this goroutine, and
		// a parked write that is never released holds an in-flight request open
		// for ever — httptest.Server.Close then blocks and the FAILURE becomes a
		// package-wide TIMEOUT instead of a named assertion. That difference is
		// what a mutation run reads, so it has to be a defer.
		defer release()

		type outcome struct {
			resp a7Resp
			err  error
		}
		winner := make(chan outcome, 1)
		go func() {
			resp, err := r.post(r.a7Delivered())
			winner <- outcome{resp, err}
		}()
		<-entered

		// While it is parked, an honest peer retries the IDENTICAL frame
		// because our 200 has not arrived. It must be told "not now".
		loser, err := r.post(r.a7Delivered())
		if err != nil {
			t.Fatalf("the losing retry did not receive a complete HTTP response: %v — a refusal must not disconnect (§12)", err)
		}
		if loser.status != http.StatusServiceUnavailable {
			t.Fatalf("the racing byte-identical retry answered %s, want 503 %s.\n\n"+
				"IF THIS IS NOW 200 {duplicate:true}: that is not an improvement, it is an INVARIANT 4 VIOLATION. At this instant the winner's fsync has NOT completed, so a 200 acknowledges a terminal outcome that is not yet durable — a power cut here loses data this bus said it had.\n"+
				"IF THIS IS NOW 409: a transient race has been spelled as a PERMANENT protocol violation. PeerRefusedError treats a 4xx as FINAL, so an honest peer would abandon a terminal outcome nothing is wrong with.",
				loser, CodeUnavailable)
		}
		if loser.code != CodeUnavailable {
			t.Errorf("the raced retry carried code %q, want %q — 503 is the retriable \"not now\" this route already uses", loser.code, CodeUnavailable)
		}
		if loser.status == http.StatusConflict || loser.code == CodeIdempotencyViolation {
			t.Error("a TRANSIENT ack.ErrConcurrentTransition was reported as a PERMANENT idempotency violation; the two must never be conflated")
		}

		// NOTHING WAS WRITTEN FOR THE LOSER. Only the parked write exists, and
		// it has not committed yet.
		if got := len(r.dlog.terminalWrites(r.key, r.recipient)); got != 0 {
			t.Errorf("%d terminal records were already committed while the winner was still parked in its fsync, want 0", got)
		}

		release()
		w := <-winner
		if w.err != nil {
			t.Fatalf("the winning frame did not receive a complete HTTP response: %v", w.err)
		}
		if w.resp.status != http.StatusOK || w.resp.duplicate {
			t.Errorf("the winner answered %s, want 200{duplicate:false}", w.resp)
		}

		// EXACTLY ONE WRITE ACROSS THE WHOLE RACE.
		if terminal := r.dlog.terminalWrites(r.key, r.recipient); len(terminal) != 1 {
			t.Fatalf("%d terminal records were written for one raced pair, want exactly 1: %+v", len(terminal), terminal)
		}

		// AND THE LOSER'S RETRY, RE-OFFERED AFTER THE WINNER SETTLED, IS THE
		// LEGITIMATE RETRY IT ALWAYS WAS. This is what makes the 503 honest
		// rather than data loss: the same frame now succeeds.
		again, err := r.post(r.a7Delivered())
		if err != nil {
			t.Fatalf("re-offering after the race: %v", err)
		}
		if again.status != http.StatusOK || !again.duplicate {
			t.Errorf("the re-offered identical frame answered %s, want 200{duplicate:true}", again)
		}
	})

	// --------------------------------------------------------------------
	// 2b. THE OTHER HALF OF EXACTLY-ONCE: an APPLY that is overtaken between
	//     the advisory decision and the settle is ABSORBED, not re-written.
	//
	//     This is ack.Store.Settle's own byte-identical arm, and it is the arm
	//     nothing else in this file can reach: every other identical retry is
	//     decided as AckReplay by DecideAck and never calls Settle at all. Two
	//     callers both decide APPLY here — which is legal, because the advisory
	//     read is explicitly allowed to race — and exactly ONE record must
	//     become durable.
	// --------------------------------------------------------------------
	t.Run("an apply overtaken before the settle is absorbed, not re-written", func(t *testing.T) {
		r := newA7Rig(t)

		atDecision := make(chan struct{})
		proceed := make(chan struct{})
		var release sync.Once
		letThrough := func() { release.Do(func() { close(proceed) }) }
		// Released whatever happens, for the reason the phase above gives: a
		// request parked here for ever turns a named assertion failure into a
		// package timeout.
		defer letThrough()
		r.mirror.beforeSettle = func() {
			close(atDecision)
			<-proceed
		}

		type outcome struct {
			resp a7Resp
			err  error
		}
		slow := make(chan outcome, 1)
		go func() {
			resp, err := r.post(r.a7Delivered())
			slow <- outcome{resp, err}
		}()
		<-atDecision // the slow request has DECIDED APPLY and has not settled

		// A second, byte-identical frame overtakes it completely: it decides
		// apply on the same non-terminal row, settles, and fsyncs.
		fast, err := r.post(r.a7Delivered())
		if err != nil {
			t.Fatalf("the overtaking frame did not receive a complete HTTP response: %v", err)
		}
		if fast.status != http.StatusOK || fast.duplicate {
			t.Fatalf("the overtaking frame answered %s, want 200{duplicate:false}", fast)
		}
		if got := len(r.dlog.terminalWrites(r.key, r.recipient)); got != 1 {
			t.Fatalf("%d terminal records after the overtaking frame, want 1", got)
		}

		letThrough() // the slow request now settles onto an ALREADY-TERMINAL row
		s := <-slow
		if s.err != nil {
			t.Fatalf("the overtaken frame did not receive a complete HTTP response: %v", s.err)
		}
		if s.resp.status != http.StatusOK {
			t.Fatalf("the overtaken byte-identical frame answered %s, want 200. It is invariant 10's legitimate retry however it raced, and a 409 here would tell an honest peer it violated the protocol", s.resp)
		}

		// THE ASSERTION MUTATION 2 BREAKS: the absorbing arm must WRITE NOTHING.
		// A second, byte-identical record in an append-only log is not
		// harmless — it is a second answer to "when did this settle", and the
		// in-memory fold is idempotent so nothing else would ever notice.
		if terminal := r.dlog.terminalWrites(r.key, r.recipient); len(terminal) != 1 {
			t.Errorf("%d terminal records were written for one pair when TWO callers both decided apply, want exactly 1: %+v. ack.Store.Settle's byte-identical arm must absorb the second silently and write NOTHING", len(terminal), terminal)
		}
		if got, want := r.mirror.applied.Load(), int64(2); got != want {
			t.Errorf("the mirror recorded %d successful settles, want %d; if it is fewer, the two callers did not both decide APPLY and this phase never reached Settle's absorbing arm", got, want)
		}
	})

	// --------------------------------------------------------------------
	// 3. SEQUENTIAL RETRIES AFTER SETTLEMENT: 200 {duplicate:true}, for ever,
	//    and NOT ONE further durable write.
	// --------------------------------------------------------------------
	t.Run("retries after settlement stay duplicates and write nothing", func(t *testing.T) {
		r := newA7Rig(t)

		first, err := r.post(r.a7Delivered())
		if err != nil {
			t.Fatalf("the first acknowledgement: %v", err)
		}
		if first.status != http.StatusOK || first.duplicate {
			t.Fatalf("the first acknowledgement answered %s, want 200{duplicate:false}", first)
		}
		callsAfterFirst := r.dlog.callCount()

		const retries = 7
		for i := 0; i < retries; i++ {
			resp, err := r.post(r.a7Delivered())
			if err != nil {
				t.Fatalf("retry %d did not receive a complete HTTP response: %v — invariant 10 says a legitimate retry is not punished, least of all with a dropped socket", i, err)
			}
			if resp.status != http.StatusOK {
				t.Fatalf("retry %d answered %s, want 200. A settled, byte-identical retry is invariant 10's FIRST case: return the original result", i, resp)
			}
			if !resp.duplicate {
				t.Errorf("retry %d answered 200{duplicate:false}; the caller cannot tell a fresh apply from a replay, and relay.DecideAck's AckReplay arm is what makes them distinguishable", i)
			}
			if !resp.accepted {
				t.Errorf("retry %d answered accepted=false", i)
			}
		}

		// RE-APPLY NOTHING. Not one further write was even ATTEMPTED — the
		// replay arm returns before ack.Store.Settle is called at all, which is
		// what makes "re-apply nothing" structural rather than a promise the
		// store keeps.
		if got := r.dlog.callCount(); got != callsAfterFirst {
			t.Errorf("%d durable writes were attempted after settlement across %d identical retries, want 0 (call count %d -> %d)", got-callsAfterFirst, retries, callsAfterFirst, got)
		}
		if terminal := r.dlog.terminalWrites(r.key, r.recipient); len(terminal) != 1 {
			t.Errorf("%d terminal records exist after %d retries, want exactly 1: %+v", len(terminal), retries, terminal)
		}
		if got := r.mirror.replayed.Load(); got != retries {
			t.Errorf("relay.DecideAck answered AckReplay %d times for %d identical retries, want %d", got, retries, retries)
		}
		if got := r.mirror.applied.Load(); got != 1 {
			t.Errorf("%d settlements were applied, want exactly 1", got)
		}
	})

	// --------------------------------------------------------------------
	// 4. A CONFLICTING PAYLOAD: 409 CodeIdempotencyViolation, the FIRST
	//    terminal stands, nothing is written, and it is LOGGED (invariant 6).
	// --------------------------------------------------------------------
	t.Run("a conflicting terminal is 409 and the first one stands", func(t *testing.T) {
		r := newA7Rig(t)

		if first, err := r.post(r.a7Delivered()); err != nil || first.status != http.StatusOK {
			t.Fatalf("the first acknowledgement: %v / %s", err, first)
		}
		writesBefore := r.dlog.callCount()

		// A transient 503 is tolerated ONLY if it resolves; the eventual answer
		// must be the permanent 409.
		var resp a7Resp
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			resp, err = r.post(r.a7Conflicting())
			if err != nil {
				t.Fatalf("the conflicting frame did not receive a complete HTTP response: %v. Invariant 10's protocol-violation case is REJECT AND LOG, never a disconnect — this link carries every agent behind the peer (§12)", err)
			}
			if resp.status != http.StatusServiceUnavailable {
				break
			}
		}
		if resp.status != http.StatusConflict {
			t.Fatalf("a conflicting terminal answered %s, want 409 %s", resp, CodeIdempotencyViolation)
		}
		if resp.code != CodeIdempotencyViolation {
			t.Errorf("the conflict carried code %q, want %q", resp.code, CodeIdempotencyViolation)
		}

		// THE FIRST TERMINAL STANDS. Terminal is absorbing.
		rec, ok := r.row()
		if !ok {
			t.Fatal("the pair lost its row to a refused conflicting frame")
		}
		if rec.State != AckDelivered {
			t.Errorf("the durable row is %s after a refused `refused` frame, want %s — terminal is ABSORBING and the FIRST terminal stands", rec.State, AckDelivered)
		}
		if rec.Class != "" {
			t.Errorf("the durable row grew class %q from the refused frame", rec.Class)
		}

		// NOTHING WAS WRITTEN.
		if got := r.dlog.callCount(); got != writesBefore {
			t.Errorf("%d durable writes were attempted for a refused conflicting terminal, want 0", got-writesBefore)
		}
		if terminal := r.dlog.terminalWrites(r.key, r.recipient); len(terminal) != 1 {
			t.Errorf("%d terminal records exist after the conflict, want exactly 1: %+v", len(terminal), terminal)
		}

		// AND IT IS LOUD (invariant 6): the remedy for a conflict is reject AND
		// LOG, and a silent rejection is the actual defect.
		// ONE LINE, not merely somewhere in the log. A whole-log substring match
		// passes if "conflict" is logged by one call and "NOT disconnected" by an
		// unrelated other — so it would keep passing after a refactor that split
		// the reasoning across two lines, or that logged the not-disconnected
		// reassurance on a path that is not this one. Invariant 6's requirement
		// is that THIS discard is logged loudly and SPECIFICALLY.
		if line := a7LineContainingAll(r.rig.logs.String(), "NOT disconnected", "conflict"); line == "" {
			t.Errorf("no SINGLE log line carries both the conflict and the not-disconnected reasoning; log:\n%s", r.rig.logs.String())
		}
	})

	// --------------------------------------------------------------------
	// THE THIRD CASE — SIGNED REPLAY — AND WHY IT IS NOT PINNED SEPARATELY.
	//
	// ACK-7 asks for three cases to be told apart: same-payload retry (phases
	// 1-3), conflicting correlation (phase 4), and SIGNED REPLAY. The third is
	// deliberately not a distinct arm here, and that is a ruling rather than an
	// omission: ACK-CONTRACT.md §12 and invariant 10 place the replay-of-a-
	// signed-message disconnect on the MESSAGE plane, and state that "an ACK
	// frame is not a message and must never reach that path". A re-sent signed
	// ACK frame is therefore invariant 10's FIRST case — a legitimate retry —
	// and it is covered by phases 1-3, which re-send the identical frame
	// (signature and all) and require it be absorbed without error and without
	// disconnect. Phase 5 below pins the no-disconnect half structurally.
	//
	// Adding a replay arm HERE would mean minting a new disconnect on the ACK
	// plane, which §12 rules out absolutely.
	// --------------------------------------------------------------------

	// --------------------------------------------------------------------
	// 5. NO PATH ON THIS ROUTE DISCONNECTS — extended to the two paths
	//    ackhttp_test.go could not reach: the duplicate and the CONFLICT.
	// --------------------------------------------------------------------
	t.Run("neither a duplicate nor a conflict drops the connection", func(t *testing.T) {
		r := newA7Rig(t)
		// ONE CONNECTION, AND REUSE IS ASSERTED RATHER THAN ASSUMED: Go's
		// transport transparently redials, so "the next request worked" is not
		// evidence that nothing hung up. httptrace's GotConn.Reused is.
		r.client = &http.Client{
			Timeout:   a7ClientTimeout,
			Transport: &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1},
		}

		// Bound to len(frames) rather than a literal, so adding a frame cannot
		// silently loosen the connection assertion below.
		frames := []PeerAckRequest{
			r.a7Delivered(),   // apply
			r.a7Delivered(),   // duplicate
			r.a7Conflicting(), // conflict, 409
			r.a7Delivered(),   // still a duplicate afterwards
			r.a7Conflicting(), // conflict again
			r.a7Delivered(),   // and the honest peer is still served
		}
		for _, frame := range frames {
			resp, err := r.post(frame)
			if err != nil {
				t.Fatalf("a request failed at the transport: %v. NO ACK-plane refusal may disconnect (§12) — a peer link carries every agent behind that peer, including their parked long-polls", err)
			}
			if resp.status != http.StatusOK && resp.status != http.StatusConflict {
				t.Fatalf("unexpected answer %s in the disconnect sequence", resp)
			}
		}

		r.traceMu.Lock()
		conns, reused := r.conns, r.reused
		r.traceMu.Unlock()
		// PINNED EXACTLY, not ">= 2". A loose floor cannot tell "six requests
		// shared one dial" from "two did and four were never sent", so it would
		// stay green if a future change silently stopped issuing requests. It is
		// bound to len(frames) so it cannot drift from what is actually sent.
		if conns != len(frames) {
			t.Fatalf("%d connections were dialled for %d requests, want exactly 1 dial reused throughout; the reuse assertion below cannot be trusted at any other count", conns, len(frames))
		}
		if reused != conns-1 {
			t.Errorf("%d of %d requests reused the connection, want %d. Something on the duplicate or conflict path DROPPED THE SOCKET", reused, conns, conns-1)
		}
	})

	// --------------------------------------------------------------------
	// 7. ACK-CONTRACT.md §16 Q2 — A TERMINAL NEGATIVE DOES NOT CANCEL THE
	//    OUTSTANDING HOP. Ruled by ACK-7; reasoning in DECISIONS.md.
	//
	// The contract deferred Q2 here with a DEFAULT of "do not cancel" and no
	// test, which is how a default quietly becomes a behaviour nobody chose.
	// This pins it.
	//
	// WHY NOT CANCELLING IS THE SECURE ANSWER, not merely the lazy one: §16 Q1
	// is still open, so NOBODY can verify a recipient attestation — there is no
	// endpoint distributing agents' messaging public keys, which is why
	// ack.Attestation has no value meaning "verified". If a terminal negative
	// cancelled outstanding hops, then in an A->B->C chain a legitimately
	// obligation-bound intermediate could refuse its OWN copy and thereby
	// suppress the origin's other routes to the same recipient. That is a
	// denial-of-delivery vector, and the obligation binding rule (§6.2) was
	// never designed to carry it: it answers "may this peer speak about this
	// key", not "may this peer decide the fate of every other route".
	//
	// What is given up is stated plainly rather than hidden: a message the
	// recipient refused on one route may still arrive on another. That is
	// at-least-once delivery behaving as designed, and the answer is
	// recipient-side idempotency (invariant 10), not cancellation.
	// --------------------------------------------------------------------
	t.Run("a terminal negative does not cancel the outstanding hop", func(t *testing.T) {
		r := newA7Rig(t)

		// A REAL outbox holding a REAL pending job for this correlation key —
		// the obligation a cancel would have to reach. The fake table the other
		// phases use has nothing to cancel, so it could not fail this.
		ob, err := NewOutbox(OutboxOptions{BusID: ackLocalBus, Durable: &obNullDurable{}})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		if _, err := ob.Enqueue(OutboxJob{PeerBusID: ackPeerBus, OriginMessageID: r.key, Size: 11, ContentSHA256: akHash}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		// FIXTURE GUARD. Without a pending job the assertion below is vacuous —
		// "still 1 pending" and "never had one" are indistinguishable.
		if got := len(ob.Jobs(OutboxPending)); got != 1 {
			t.Fatalf("fixture: %d pending jobs before the NACK, want exactly 1; the assertion below could not fail", got)
		}

		rig := newAckRig(t, func(cfg *AckConfig) {
			cfg.Obligations = ob
			cfg.SettleAck = r.mirror.settle
		})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rig.h.ServeAuthenticated(w, req, ackPeerBus)
		}))
		defer srv.Close()

		body, err := json.Marshal(r.a7Conflicting())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		q2Client := &http.Client{Timeout: a7ClientTimeout}
		resp, err := q2Client.Post(srv.URL+PeerAckPath, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("posting the terminal negative: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the terminal negative answered %d, want 200; this phase is about what a SETTLED negative does to the hop, so it must actually settle", resp.StatusCode)
		}
		if row, ok := r.store.Lookup(r.key, r.recipient); !ok || row.State != ack.StateRefused {
			t.Fatalf("row = %+v ok=%v, want a durable `refused`; without it nothing negative was recorded and the hop assertion proves nothing", row, ok)
		}

		// THE RULING. The hop is untouched: still pending, still owed, still
		// retriable. Asserted on the RECORD's state as well as the count, so a
		// cancel that settled the job in place could not hide behind the length.
		pending := ob.Jobs(OutboxPending)
		if len(pending) != 1 {
			t.Fatalf("after a terminal negative the outbox holds %d pending jobs, want 1: the terminal negative CANCELLED an outstanding hop, which ACK-CONTRACT.md §16 Q2 rules it must not (see DECISIONS.md)", len(pending))
		}
		if pending[0].State != OutboxPending {
			t.Errorf("the outstanding hop is %s, want %s", pending[0].State, OutboxPending)
		}
		if got := len(ob.Jobs(OutboxAbandoned, OutboxDelivered)); got != 0 {
			t.Errorf("%d hop(s) were settled by a terminal negative, want 0", got)
		}
	})
}

// a7LineContainingAll returns the first line of s containing every needle, or
// "" if no single line does. It exists because a whole-log match is a weaker
// claim than it looks: two needles can be satisfied by two unrelated lines.
func a7LineContainingAll(s string, needles ...string) string {
	for _, line := range strings.Split(s, "\n") {
		if a7ContainsAll(line, needles...) {
			return line
		}
	}
	return ""
}

// a7ContainsAll reports whether s contains every needle.
func a7ContainsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !bytes.Contains([]byte(s), []byte(n)) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The tie-back: the mirror above is only evidence while production still has
// this shape.
// ---------------------------------------------------------------------------

// a7AssertMirrorMatchesProduction parses cmd/agent-bus/relaywiring.go and
// asserts that (*federation).settleAck still performs the sequence a7Mirror
// reproduces, and still does NOT branch on ack.ErrConcurrentTransition.
//
// IT IS AST-BASED, NOT A GREP, AND THAT IS LOAD-BEARING IN BOTH DIRECTIONS.
// settleAck's own comments DISCUSS ErrConcurrentTransition at length — that
// prose is the reasoning for why the race falls through to the default arm and
// it must stay — so a textual search would fire on the very explanation it
// exists to protect and the guard would be deleted. Conversely, a textual
// search for "DecideAck" would be satisfied by a comment mentioning it, so a
// production change that DELETED the call would leave this green. go/parser
// discards comments, so only real code can satisfy or trip this.
func a7AssertMirrorMatchesProduction(t *testing.T) {
	t.Helper()
	path := filepath.Join("..", "..", "cmd", "agent-bus", "relaywiring.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0) // 0 = comments DISCARDED
	if err != nil {
		t.Fatalf("parsing %s: %v — the mirror in this file cannot be tied back to production, so every assertion below would be a proof about a copy", path, err)
	}

	names, visited, hasDefaultArm := a7SettlePath(t, file, "settleAck")

	// Non-vacuity, in three parts. Each one would silently turn every check
	// below into a free pass.
	if len(names) == 0 {
		t.Fatal("the AST walk over the settle path found no identifiers at all; every check below would be vacuous")
	}
	if names["a7ThisIdentifierIsNotInProduction"] != 0 {
		t.Fatal("the AST walk reported an identifier that cannot exist; the scanner is not reading what it claims to")
	}
	// The walk MUST have followed at least one delegate. settleAck has called
	// f.priorTerminal since it was written, so a visited-set of size 1 means the
	// delegate-following broke and the scan silently narrowed to one function —
	// which is exactly the failure this rewrite exists to fix.
	if len(visited) < 2 {
		t.Fatalf("the settle-path walk visited only %v; it is supposed to follow same-receiver delegates, so this means the follower is broken and the scan has silently narrowed to a single function", visited)
	}

	for _, want := range []struct {
		name string
		why  string
	}{
		{"DecideAck", "the duplicate/conflict decision. a7Mirror calls it, and mutating its AckReplay arm is one of this file's mutation proofs"},
		{"AckReplay", "invariant 10's first case: return the original result and re-apply nothing"},
		{"Settle", "the durable transition itself"},
		{"ErrNoRecord", "§8.2's \"(none)\" row"},
		{"ErrAckNotBound", "the uniform refusal the \"(none)\" row is answered with at the ORIGIN"},
		{"ErrTerminal", "invariant 10's SECOND case, the permanent conflict"},
		{"ErrAckOutcomeConflict", "the sentinel the handler maps to 409 CodeIdempotencyViolation"},
	} {
		if names[want.name] == 0 {
			t.Errorf("the settle path in cmd/agent-bus no longer references %s (%s). a7Mirror in this file still does, so the mirror has DRIFTED and its evidence no longer describes production. Path walked: %v", want.name, want.why, visited)
		}
	}

	// THE DEFAULT ARM MUST STILL EXIST, and this is checked BECAUSE of the
	// negative below rather than beside it.
	//
	// The negative asserts that nothing on the settle path BRANCHES on
	// ErrConcurrentTransition. That sentence only means anything while there is
	// a default arm for it to fall through TO. Delete the default and the
	// negative still passes — happily, over code that no longer answers a
	// concurrent transition at all. A negative assertion whose premise has been
	// removed is a guard that cannot fire, so the premise is asserted too.
	if !hasDefaultArm {
		t.Error("the settle path in cmd/agent-bus no longer has a switch with a DEFAULT arm. ErrConcurrentTransition is answered 503 by FALLING THROUGH to that arm — with no default there is nothing to fall through to, and the negative assertion below would pass while the behaviour it protects had gone.")
	}

	// THE NEGATIVE, AND IT IS THE POINT OF THIS WHOLE FUNCTION.
	//
	// Scoped to the WHOLE settle path, not to settleAck alone: an arm extracted
	// into a delegate is still an arm.
	if names["ErrConcurrentTransition"] != 0 {
		t.Errorf("the settle path in cmd/agent-bus now BRANCHES ON ack.ErrConcurrentTransition in code (walked: %v). A concurrent transition is TRANSIENT — it must fall through to the default arm and be answered 503 (retriable, nothing written). Giving it an arm of its own is how it acquires a 4xx: a 200{duplicate:true} would acknowledge an outcome whose fsync has not completed (INVARIANT 4), and a 409 would tell an honest retrying peer it committed a protocol violation it did not commit (INVARIANT 10). See this file's header.", visited)
	}
}

// a7SettlePath walks the SETTLE PATH in cmd/agent-bus/relaywiring.go: the named
// method, plus every method it calls on its OWN RECEIVER that is declared in the
// same file, transitively.
//
// # WHY IT FOLLOWS DELEGATES INSTEAD OF SCANNING ONE FUNCTION
//
// The first version scanned settleAck alone. ACK-5 then extracted the
// not-bound arm into (*federation).disposeUnrecordedAck — production behaviour
// unchanged, ErrAckNotBound still returned, still the uniform refusal — and the
// function-scoped scan stopped finding the identifier and failed. That is a
// guard constraining the SHAPE of production rather than its BEHAVIOUR, which is
// backwards: a refactor that preserves behaviour must not break a test whose
// subject is behaviour. Following the receiver's own delegates makes the guard
// survive extraction, which is the commonest refactor this code will see.
//
// It returns the identifier counts, the functions actually visited (for
// non-vacuity and for an actionable failure message), and whether any visited
// function contains a switch with a default arm.
func a7SettlePath(t *testing.T, file *ast.File, root string) (map[string]int, []string, bool) {
	t.Helper()

	methods := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Recv != nil && d.Body != nil {
			methods[d.Name.Name] = d
		}
	}
	if methods[root] == nil {
		t.Fatalf("cmd/agent-bus/relaywiring.go no longer declares a method named %s. a7Mirror in this file MIRRORS it; production has moved and the mirror is now a proof about a copy of nothing. Re-derive both.", root)
	}

	names := map[string]int{}
	seen := map[string]bool{}
	var visited []string
	hasDefault := false

	var walk func(string)
	walk = func(name string) {
		fn := methods[name]
		if fn == nil || seen[name] {
			return
		}
		seen[name] = true
		visited = append(visited, name)

		recv := ""
		if len(fn.Recv.List) > 0 && len(fn.Recv.List[0].Names) > 0 {
			recv = fn.Recv.List[0].Names[0].Name
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				names[v.Name]++
			case *ast.SelectorExpr:
				names[v.Sel.Name]++
				// Follow ONLY `recv.method(...)`. `f.acks.Settle` has a
				// SelectorExpr for X, not an Ident, so it is not followed —
				// correct, since that leaves this file.
				if id, ok := v.X.(*ast.Ident); ok && recv != "" && id.Name == recv {
					walk(v.Sel.Name)
				}
			case *ast.SwitchStmt:
				for _, c := range v.Body.List {
					if cc, ok := c.(*ast.CaseClause); ok && cc.List == nil {
						hasDefault = true
					}
				}
			}
			return true
		})
	}
	walk(root)
	return names, visited, hasDefault
}
