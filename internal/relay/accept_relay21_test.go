package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// RELAY-21: the AcceptRelay callback. Two orderings are the whole task, and both
// are properties of a SEQUENCE rather than of a return value, so every test here
// asserts on a recorded CALL LOG and not merely on what came back:
//
//  1. the roster is asked BEFORE the durable write (finding cca64afd);
//  2. the onward hop happens ONLY on idem.OutcomeNew.
//
// A test that checked only the error would pass against an implementation that
// wrote first and refused afterwards — which is the exact defect, since the
// injury is the durable record, not the answer.

// ---------------------------------------------------------------------------
// Harness.
// ---------------------------------------------------------------------------

// acceptRecorder is the ORDERED log of everything the acceptor did to its
// collaborators. Both fakes below write into one recorder, because the property
// under test is the order of calls ACROSS them.
type acceptRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *acceptRecorder) record(format string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *acceptRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// wrote reports whether the durable write was reached at all.
func (r *acceptRecorder) wrote() bool {
	for _, c := range r.snapshot() {
		if strings.HasPrefix(c, "durable-write:") {
			return true
		}
	}
	return false
}

// fakeLocalBus is a LocalIngest that records every call and answers from a
// script. It deliberately does NOT enforce the ordering itself: if the acceptor
// wrote before asking, this fake happily writes, and the assertion — not the
// fake — is what fails.
type fakeLocalBus struct {
	rec      *acceptRecorder
	enrolled map[string]bool

	acc LocalAcceptance
	err error

	mu       sync.Mutex
	accepted []RelayedMessage
}

func (f *fakeLocalBus) Enrolled(agentID string) bool {
	f.rec.record("roster:%s", agentID)
	return f.enrolled[agentID]
}

func (f *fakeLocalBus) AcceptRelayed(_ context.Context, m RelayedMessage) (LocalAcceptance, error) {
	f.rec.record("durable-write:%s", m.OriginMessageID)
	if f.err != nil {
		return LocalAcceptance{}, f.err
	}
	f.mu.Lock()
	f.accepted = append(f.accepted, m)
	f.mu.Unlock()
	return f.acc, nil
}

func (f *fakeLocalBus) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accepted)
}

// fakeOnward is an OnwardForwarder that records what it was handed.
type fakeOnward struct {
	rec    *acceptRecorder
	queued int
	err    error

	mu   sync.Mutex
	sent []RelayedMessage
}

func (f *fakeOnward) Enqueue(m RelayedMessage) (int, error) {
	f.rec.record("forward:%s", m.OriginMessageID)
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.queued, nil
}

func (f *fakeOnward) forwards() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// acceptHarness is one acceptor plus the collaborators it was built over.
type acceptHarness struct {
	a     *Acceptor
	local *fakeLocalBus
	fwd   *fakeOnward
	rec   *acceptRecorder
}

// newAcceptHarness builds an acceptor for localBus whose local roster holds
// exactly the ids given, and whose durable write reports a fresh acceptance.
func newAcceptHarness(t *testing.T, enrolled ...string) *acceptHarness {
	t.Helper()
	rec := &acceptRecorder{}
	roster := make(map[string]bool, len(enrolled))
	for _, id := range enrolled {
		roster[id] = true
	}
	local := &fakeLocalBus{
		rec:      rec,
		enrolled: roster,
		acc:      LocalAcceptance{LocalMessageID: localBus + "-1", Outcome: idem.OutcomeNew},
	}
	fwd := &fakeOnward{rec: rec, queued: 1}
	a, err := NewAcceptor(AcceptOptions{BusID: localBus, Local: local, Onward: fwd})
	if err != nil {
		t.Fatalf("NewAcceptor: %v", err)
	}
	return &acceptHarness{a: a, local: local, fwd: fwd, rec: rec}
}

// ingestForTest turns a fixture into the VALIDATED, SIGNATURE-VERIFIED message
// an acceptor is only ever handed. Going through ValidateRelayRequest rather
// than building a RelayedMessage literal keeps these tests honest about what
// reaches the callback in production.
func ingestForTest(t *testing.T, mods ...func(*RelayRequest)) RelayedMessage {
	t.Helper()
	req := relayFixture(mods...)
	m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest on the fixture: %v", err)
	}
	return m
}

func withRecipients(ids ...string) func(*RelayRequest) {
	return func(r *RelayRequest) { r.Recipients = append([]string(nil), ids...) }
}

// ---------------------------------------------------------------------------
// 1. The roster is asked BEFORE the durable write (cca64afd).
// ---------------------------------------------------------------------------

// TestAcceptRelayChecksRosterBeforeDurableWrite is RELAY-21's headline and the
// task's recorded proof.
//
// THE ASSERTION THAT MATTERS IS "NOTHING WAS WRITTEN", not "an error came back".
// A peer that can get a message durably recorded against
// "<local-bus>.alpha-<huge>" burns that short name FOR EVER: the suffix floor is
// derived from ids found in the recovered log (cmd/agent-bus/suffixfloors.go),
// invariant 1 forbids reissuing an id including across restarts, and the log is
// append-only so there is nothing to undo. Write-then-refuse would return the
// identical error and still inflict all of that.
func TestAcceptRelayChecksRosterBeforeDurableWrite(t *testing.T) {
	const (
		known   = localBus + ".alpha-1"
		unknown = localBus + ".ghost-9"
		foreign = thirdBus + ".delta-1"
	)
	// A confusable spelling of our OWN bus id. Registry.Route refuses to route
	// it (its bus half case-folds to ours), so admitting it would durably record
	// a message addressed to somebody nothing can ever deliver to.
	confusable := strings.ToUpper(localBus) + ".alpha-1"

	cases := []struct {
		name       string
		recipients []string
		wantWrite  bool
		why        string
	}{
		{
			name:       "local recipient on the roster is admitted",
			recipients: []string{known},
			wantWrite:  true,
			why:        "the roster holds it, so the message is ours to record",
		},
		{
			name:       "local recipient the roster does not hold is refused before any write",
			recipients: []string{unknown},
			wantWrite:  false,
			why:        "cca64afd: a name admitted by anything other than the roster is burned for ever",
		},
		{
			name:       "one known and one unknown local recipient refuses the whole message",
			recipients: []string{known, unknown},
			wantWrite:  false,
			why:        "a partial acceptance would be reported as an acceptance, and the write is what cannot be undone",
		},
		{
			name:       "a confusable spelling of our own bus id is ours to refuse",
			recipients: []string{confusable},
			wantWrite:  false,
			why:        "Registry.Route refuses to route it, so accepting it records a message nobody can ever be delivered",
		},
		{
			name:       "recipients on another bus are not roster-checked at all",
			recipients: []string{foreign},
			wantWrite:  true,
			why:        "pure transit: another bus's namespace is not ours to admit, and the message is still durably ours (RELAY-16)",
		},
		{
			name:       "an unknown local recipient beside a routable foreign one still refuses",
			recipients: []string{foreign, unknown},
			wantWrite:  false,
			why:        "the foreign hop must not buy admission for a local name nobody holds",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newAcceptHarness(t, known)
			m := ingestForTest(t, withRecipients(tc.recipients...))

			acc, err := h.a.Accept(context.Background(), m)
			calls := h.rec.snapshot()

			if tc.wantWrite {
				if err != nil {
					t.Fatalf("Accept: %v, want nil (%s); calls=%v", err, tc.why, calls)
				}
				if h.local.writes() != 1 {
					t.Fatalf("the local bus was asked to write %d times, want 1 (%s); calls=%v", h.local.writes(), tc.why, calls)
				}
				if acc.LocalMessageID == "" {
					t.Fatalf("Accept returned no local message id; calls=%v", calls)
				}
				return
			}

			if !errors.Is(err, ErrUnknownLocalRecipient) {
				t.Fatalf("Accept err = %v, want ErrUnknownLocalRecipient (%s)", err, tc.why)
			}
			if acc != (RelayAcceptance{}) {
				t.Errorf("Accept returned %+v alongside a refusal; a refused message must acknowledge nothing", acc)
			}
			if h.local.writes() != 0 || h.rec.wrote() {
				t.Fatalf("THE DURABLE WRITE HAPPENED FOR A RECIPIENT NOBODY HOLDS: %s. Calls in order: %v.\n"+
					"The roster must be asked BEFORE the write: invariant 1 forbids reusing an id including across "+
					"restarts, the suffix floor is derived from ids recovered from the append-only log, and there is "+
					"no cleanup path — so one accepted envelope burns that short name permanently.", tc.why, calls)
			}
			if h.fwd.forwards() != 0 {
				t.Errorf("a refused message was forwarded onward %d times, want 0", h.fwd.forwards())
			}
			if got := h.a.Stats().UnknownRecipient; got != 1 {
				t.Errorf("Stats().UnknownRecipient = %d, want 1: the refusal must be countable, since a stale peer roster and a peer probing our namespace differ only in rate", got)
			}
		})
	}

	// AND THE ORDER ITSELF, on the admitted case: the roster call must appear
	// before the write in the SAME log. The refusal cases above prove the write
	// never happens; this proves the check is not merely concurrent with it.
	t.Run("the roster call precedes the write in the call log", func(t *testing.T) {
		h := newAcceptHarness(t, known)
		if _, err := h.a.Accept(context.Background(), ingestForTest(t, withRecipients(known))); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		calls := h.rec.snapshot()
		if len(calls) < 2 || !strings.HasPrefix(calls[0], "roster:") || !strings.HasPrefix(calls[1], "durable-write:") {
			t.Fatalf("calls = %v, want the roster consulted first and the durable write second", calls)
		}
	})
}

// TestAcceptRelayRefusesAnUnknownLocalRecipientThroughTheRealIngress drives the
// same property through the ACTUAL handler, over HTTP, with a genuinely signed
// envelope — the shape a peer bus produces.
//
// It also pins the answer the peer gets: 404 with the stable code, which is
// FINAL. A 503 would have the peer's retry machinery re-send a message that
// cannot be accepted until an enrolment happens, turning our own control into
// the amplifier — the failure mode relayhttp's status argument exists to avoid.
func TestAcceptRelayRefusesAnUnknownLocalRecipientThroughTheRealIngress(t *testing.T) {
	rec := &acceptRecorder{}
	local := &fakeLocalBus{
		rec:      rec,
		enrolled: map[string]bool{},
		acc:      LocalAcceptance{LocalMessageID: localBus + "-1", Outcome: idem.OutcomeNew},
	}
	acceptor, err := NewAcceptor(AcceptOptions{BusID: localBus, Local: local})
	if err != nil {
		t.Fatalf("NewAcceptor: %v", err)
	}
	r := newRelayResponder(t, localBus, func(c *RelayConfig) { c.AcceptRelay = acceptor.Accept })

	cli := newInitiator(t, peerBus, nil, r.srv)
	req := relayFixture(withRecipients(localBus + ".ghost-9"))
	resp, err := cli.Relay(context.Background(), r.srv.URL, req)
	if err == nil {
		t.Fatalf("Relay = %+v, want a refusal: this bus holds no such agent", resp)
	}

	var refused *PeerRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Relay err = %v, want a *PeerRefusedError", err)
	}
	if refused.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d: the envelope is well formed and correctly signed, and the remedy is the sending bus's roster", refused.StatusCode, http.StatusNotFound)
	}
	if refused.Code != CodeUnknownRecipient {
		t.Errorf("code = %q, want %q", refused.Code, CodeUnknownRecipient)
	}
	if refused.Retriable() {
		t.Errorf("a 404 must not be retriable: re-sending the identical bytes cannot make this bus hold an agent it never enrolled, and retrying it would make our own retry control the amplifier")
	}
	if local.writes() != 0 || rec.wrote() {
		t.Fatalf("THE DURABLE WRITE HAPPENED over the real ingress for an agent this bus does not hold; calls=%v", rec.snapshot())
	}
	if got := ErrorCode(fmt.Errorf("wrapped: %w", ErrUnknownLocalRecipient)); got != CodeUnknownRecipient {
		t.Errorf("ErrorCode(ErrUnknownLocalRecipient) = %q, want %q", got, CodeUnknownRecipient)
	}
}

// ---------------------------------------------------------------------------
// 2. Onward only on idem.OutcomeNew.
// ---------------------------------------------------------------------------

// TestAcceptRelayForwardsOnlyOnOutcomeNew pins the other half of the task.
//
// Re-forwarding a DUPLICATE is what turns idempotent redelivery into an
// amplification loop: the split horizon alone admits one copy per simple path,
// which is factorial in a full mesh, and it is the applied-key table answering
// the second arrival AND SENDING IT NO FURTHER that terminates the traffic.
func TestAcceptRelayForwardsOnlyOnOutcomeNew(t *testing.T) {
	const known = localBus + ".alpha-1"
	original := localBus + "-1"

	cases := []struct {
		name         string
		acc          LocalAcceptance
		localErr     error
		wantForwards int
		wantDup      bool
		wantErr      error
		wantID       string
		why          string
	}{
		{
			name:         "a new acceptance is forwarded onward",
			acc:          LocalAcceptance{LocalMessageID: original, Outcome: idem.OutcomeNew},
			wantForwards: 1,
			wantID:       original,
			why:          "the first copy is the one that travels",
		},
		{
			name:         "a duplicate is answered and goes no further",
			acc:          LocalAcceptance{LocalMessageID: original, Outcome: idem.OutcomeRetry},
			wantForwards: 0,
			wantDup:      true,
			wantID:       original,
			why:          "invariant 10: return the ORIGINAL result, re-apply nothing, forward nothing, disconnect nobody",
		},
		{
			name:         "a violation reported by value is a 409 and is not forwarded",
			acc:          LocalAcceptance{LocalMessageID: original, Outcome: idem.OutcomeViolation},
			wantForwards: 0,
			wantErr:      ErrIdempotencyViolation,
			why:          "same key, different payload: rejected and logged, and that is the WHOLE remedy",
		},
		{
			name:         "a violation reported as an error passes through unchanged",
			localErr:     fmt.Errorf("hub: %w", ErrIdempotencyViolation),
			wantForwards: 0,
			wantErr:      ErrIdempotencyViolation,
			why:          "the handler classifies it either way, and neither disconnects the peer",
		},
		{
			name:         "a failed durable write forwards nothing",
			localErr:     errors.New("disk is on fire"),
			wantForwards: 0,
			wantErr:      nil, // any error; asserted as non-nil below
			why:          "nothing was written, so there is nothing to hand onward",
		},
		{
			name:         "an outcome this build does not know is refused rather than guessed",
			acc:          LocalAcceptance{LocalMessageID: original, Outcome: idem.Outcome(99)},
			wantForwards: 0,
			why:          "guessing new re-forwards and guessing retry silently drops the hop",
		},
		{
			name:         "an acceptance with no local message id is refused rather than acknowledged",
			acc:          LocalAcceptance{Outcome: idem.OutcomeNew},
			wantForwards: 0,
			why:          "a 200 naming no message cannot be correlated, and the zero LocalAcceptance also claims OutcomeNew",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newAcceptHarness(t, known)
			h.local.acc = tc.acc
			h.local.err = tc.localErr

			acc, err := h.a.Accept(context.Background(), ingestForTest(t, withRecipients(known)))

			if h.fwd.forwards() != tc.wantForwards {
				t.Fatalf("the message was queued onward %d times, want %d (%s); calls=%v", h.fwd.forwards(), tc.wantForwards, tc.why, h.rec.snapshot())
			}
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Accept err = %v, want %v (%s)", err, tc.wantErr, tc.why)
				}
				return
			case tc.wantID == "":
				if err == nil {
					t.Fatalf("Accept = %+v, want an error (%s)", acc, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("Accept: %v, want nil (%s)", err, tc.why)
			}
			if acc.LocalMessageID != tc.wantID {
				t.Errorf("LocalMessageID = %q, want %q (%s)", acc.LocalMessageID, tc.wantID, tc.why)
			}
			if acc.Duplicate != tc.wantDup {
				t.Errorf("Duplicate = %v, want %v (%s)", acc.Duplicate, tc.wantDup, tc.why)
			}
		})
	}

	// The order of the two steps that DO happen: durable first, onward second.
	// Forwarding before the write would put a message on the wire that a crash
	// could leave this bus with no record of ever having accepted.
	t.Run("the durable write precedes the onward hop", func(t *testing.T) {
		h := newAcceptHarness(t, known)
		if _, err := h.a.Accept(context.Background(), ingestForTest(t, withRecipients(known))); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		calls := h.rec.snapshot()
		want := []string{"roster:" + known, "durable-write:" + peerBus + "-1", "forward:" + peerBus + "-1"}
		if len(calls) != len(want) {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
		for i := range want {
			if calls[i] != want[i] {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
		}
	})

	// A duplicate must not be counted as applied, and the counters are how an
	// operator sees the gate working in a cyclic topology.
	t.Run("the counters separate applied from suppressed", func(t *testing.T) {
		h := newAcceptHarness(t, known)
		m := ingestForTest(t, withRecipients(known))
		if _, err := h.a.Accept(context.Background(), m); err != nil {
			t.Fatalf("Accept (new): %v", err)
		}
		h.local.acc = LocalAcceptance{LocalMessageID: original, Outcome: idem.OutcomeRetry}
		if _, err := h.a.Accept(context.Background(), m); err != nil {
			t.Fatalf("Accept (duplicate): %v", err)
		}
		got := h.a.Stats()
		if got.Applied != 1 || got.Duplicates != 1 || got.ForwardedCopies != 1 {
			t.Errorf("Stats() = %+v, want Applied=1 Duplicates=1 ForwardedCopies=1: the second arrival was answered and must not have travelled", got)
		}
	})
}

// TestAcceptRelayHandsTheForwarderTheMessageAsIngested pins what is forwarded:
// the message EXACTLY as it arrived, path included. Appending our own hop is
// Forward's job — doing it here would make the ingress record disagree with the
// egress envelope built from it, and the far end re-derives the signed bytes
// from these values.
//
// The fixture is MULTI-HOP on purpose. RELAY-11 made store.NewMessageWithBusPath
// validate a full path, and this callback is one of the first things able to
// produce one.
func TestAcceptRelayHandsTheForwarderTheMessageAsIngested(t *testing.T) {
	const known = localBus + ".alpha-1"
	h := newAcceptHarness(t, known)

	// origin -> peerBus -> us. The path is routing metadata outside the
	// signature, so the fixture stays genuinely signed.
	m := ingestForTest(t, withRecipients(known, thirdBus+".delta-1"), func(r *RelayRequest) {
		r.BusPath = []string{r.OriginBus, thirdBus}
	})
	if len(m.BusPath) != 2 {
		t.Fatalf("fixture BusPath = %v, want two hops", m.BusPath)
	}

	if _, err := h.a.Accept(context.Background(), m); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	h.fwd.mu.Lock()
	defer h.fwd.mu.Unlock()
	if len(h.fwd.sent) != 1 {
		t.Fatalf("forwarded %d messages, want 1", len(h.fwd.sent))
	}
	got := h.fwd.sent[0]
	if strings.Join(got.BusPath, ",") != strings.Join(m.BusPath, ",") {
		t.Errorf("forwarded BusPath = %v, want the path AS INGESTED %v: our hop is appended by Forward, not here", got.BusPath, m.BusPath)
	}
	if got.OriginMessageID != m.OriginMessageID || got.IdempotencyKey != m.IdempotencyKey {
		t.Errorf("forwarded (%s,%s), want (%s,%s): the origin id IS the idempotency key on every hop", got.OriginMessageID, got.IdempotencyKey, m.OriginMessageID, m.IdempotencyKey)
	}
}

// TestAcceptRelayStillAcknowledgesWhenTheOnwardHopFails: the message is already
// durable and is therefore already this bus's responsibility. Refusing here
// would ask the peer to retry something we have recorded — a guaranteed
// duplicate — over a condition of OUR bus (Enqueue's only error is
// ErrForwarderClosed, a shutdown).
func TestAcceptRelayStillAcknowledgesWhenTheOnwardHopFails(t *testing.T) {
	const known = localBus + ".alpha-1"
	h := newAcceptHarness(t, known)
	h.fwd.err = ErrForwarderClosed

	acc, err := h.a.Accept(context.Background(), ingestForTest(t, withRecipients(known)))
	if err != nil {
		t.Fatalf("Accept: %v, want nil: the message is durable, so the peer is owed its acknowledgement", err)
	}
	if acc.LocalMessageID == "" || acc.Duplicate {
		t.Errorf("Accept = %+v, want the freshly minted local id and Duplicate=false", acc)
	}
	if got := h.a.Stats().ForwardedCopies; got != 0 {
		t.Errorf("Stats().ForwardedCopies = %d, want 0: nothing was queued", got)
	}
}

// TestAcceptRelayWithNoForwarderIsALeafBus: nil Onward is a configuration, not a
// mistake. A leaf bus accepts messages for its own agents and carries nothing
// onward, and a foreign recipient it cannot route is still durably ours.
func TestAcceptRelayWithNoForwarderIsALeafBus(t *testing.T) {
	rec := &acceptRecorder{}
	local := &fakeLocalBus{
		rec:      rec,
		enrolled: map[string]bool{localBus + ".alpha-1": true},
		acc:      LocalAcceptance{LocalMessageID: localBus + "-1", Outcome: idem.OutcomeNew},
	}
	a, err := NewAcceptor(AcceptOptions{BusID: localBus, Local: local})
	if err != nil {
		t.Fatalf("NewAcceptor: %v", err)
	}
	acc, err := a.Accept(context.Background(), ingestForTest(t, withRecipients(thirdBus+".delta-1")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if acc.LocalMessageID == "" {
		t.Fatalf("Accept = %+v, want an acknowledged local id: a message with no route is still durably ours (RELAY-16)", acc)
	}
	if local.writes() != 1 {
		t.Errorf("the local bus wrote %d times, want 1", local.writes())
	}
}

// TestNewAcceptorRefusesAnIncompleteConfiguration: every omission here produces
// a bus that looks healthy and silently does the wrong thing, so it is refused
// at construction where the message can name which side is broken.
func TestNewAcceptorRefusesAnIncompleteConfiguration(t *testing.T) {
	rec := &acceptRecorder{}
	local := &fakeLocalBus{rec: rec, enrolled: map[string]bool{}}

	cases := []struct {
		name string
		opts AcceptOptions
		want bool // want an error
	}{
		{"no bus id", AcceptOptions{Local: local}, true},
		{"bus id with a dot", AcceptOptions{BusID: "bus.x", Local: local}, true},
		{"no local bus", AcceptOptions{BusID: localBus}, true},
		{"no forwarder is a LEAF BUS, not an error", AcceptOptions{BusID: localBus, Local: local}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAcceptor(tc.opts)
			if tc.want != (err != nil) {
				t.Fatalf("NewAcceptor(%+v) err = %v, want error: %v", tc.opts, err, tc.want)
			}
		})
	}
}

// TestAcceptRelayRefusesADirectCallerClaimingOurNamespace covers the two
// direct-caller guards that both gates asked for, and the branch behind them.
//
// The SENDER is the second field the suffix-floor derivation folds into a
// name's floor (cmd/agent-bus/suffixfloors.go reads sender AND recipients of
// every recovered message), so a durable record whose sender claims
// "<local-bus>.<name>-<huge>" burns that short name exactly as permanently as a
// recipient would. ValidateRelayRequest makes it unreachable over the wire;
// Accept is exported, so it is refused here too — and, as everywhere else in
// this file, the assertion is that NOTHING WAS WRITTEN.
func TestAcceptRelayRefusesADirectCallerClaimingOurNamespace(t *testing.T) {
	const known = localBus + ".alpha-1"

	cases := []struct {
		name string
		mut  func(*RelayedMessage)
		want error
		why  string
	}{
		{
			name: "a sender inside our own namespace",
			mut:  func(m *RelayedMessage) { m.Sender = localBus + ".alpha-18446744073709551615" },
			want: ErrInvalidRelay,
			why:  "the sender belongs to the origin bus, never to this one, and it feeds the same suffix floor",
		},
		{
			name: "a sender spelled as a confusable of our bus id",
			mut:  func(m *RelayedMessage) { m.Sender = strings.ToUpper(localBus) + ".alpha-9" },
			want: ErrInvalidRelay,
			why:  "a confusable claims our namespace just as effectively",
		},
		{
			name: "a sender that does not parse at all",
			mut:  func(m *RelayedMessage) { m.Sender = "not an id" },
			want: ErrInvalidRelay,
			why:  "an id this bus cannot attribute to anybody is not one it will record",
		},
		{
			name: "a recipient that does not parse at all",
			mut:  func(m *RelayedMessage) { m.Recipients = []string{"not an id"} },
			want: ErrUnknownLocalRecipient,
			why:  "it names nobody, and the unparseable arm must refuse rather than fall through to the write",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newAcceptHarness(t, known)
			m := ingestForTest(t, withRecipients(known))
			tc.mut(&m)

			acc, err := h.a.Accept(context.Background(), m)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Accept err = %v, want %v (%s)", err, tc.want, tc.why)
			}
			if acc != (RelayAcceptance{}) {
				t.Errorf("Accept returned %+v alongside a refusal; a refused message must acknowledge nothing", acc)
			}
			if tc.want == ErrInvalidRelay && len(h.rec.snapshot()) != 0 {
				// The sender guard is a DIRECT-CALLER guard and must sit ahead of
				// every collaborator, not merely ahead of the write: an id that
				// burns a name must cost us not even a roster lookup. Moving the
				// guard below the roster read has to fail here.
				t.Errorf("the refusal touched a collaborator before refusing: %v", h.rec.snapshot())
			}
			if h.local.writes() != 0 || h.rec.wrote() {
				t.Fatalf("THE DURABLE WRITE HAPPENED for %s: %s; calls=%v", tc.name, tc.why, h.rec.snapshot())
			}
			if h.fwd.forwards() != 0 {
				t.Errorf("a refused message was forwarded %d times, want 0", h.fwd.forwards())
			}
		})
	}
}

// TestUnknownRecipientLogLineIsBoundedByUsAndNotByThePeer: a refusal is the
// cheapest answer on this surface to provoke — one request, no write — so the
// line it emits must not scale with a number the peer chooses.
func TestUnknownRecipientLogLineIsBoundedByUsAndNotByThePeer(t *testing.T) {
	many := make([]string, 0, maxLoggedIDs+5)
	for i := 0; i < maxLoggedIDs+5; i++ {
		many = append(many, fmt.Sprintf("%s.ghost-%d", localBus, i+1))
	}
	got := summariseIDs(many)
	if strings.Count(got, ",") != maxLoggedIDs {
		t.Errorf("summariseIDs named %d separators for %d ids, want %d: the line is bounded by us", strings.Count(got, ","), len(many), maxLoggedIDs)
	}
	if !strings.Contains(got, "and 5 more") {
		t.Errorf("summariseIDs = %q, want it to say how many it omitted; the count itself is never truncated", got)
	}
	if short := summariseIDs(many[:2]); short != many[0]+","+many[1] {
		t.Errorf("summariseIDs(2 ids) = %q, want both named verbatim", short)
	}

	// AND THE BOUND MUST BE SELF-ENFORCING: asserting on the helper alone would
	// stay green if Accept went back to joining the whole slice, which is the
	// only way the bound can actually be lost. So drive the real refusal and
	// read the line it emitted (security gate, LOW-3).
	var logged bytes.Buffer
	local := &fakeLocalBus{rec: &acceptRecorder{}, enrolled: map[string]bool{}}
	a, err := NewAcceptor(AcceptOptions{
		BusID:  localBus,
		Local:  local,
		Logger: logging.New(&logged, logging.LevelWarn),
	})
	if err != nil {
		t.Fatalf("NewAcceptor: %v", err)
	}
	// store.MaxRecipients caps a WIRE envelope; a direct caller is bounded only
	// by this line, so the fixture uses more than the cap on purpose.
	if _, err := a.Accept(context.Background(), ingestForTest(t, withRecipients(many[:2]...))); !errors.Is(err, ErrUnknownLocalRecipient) {
		t.Fatalf("Accept err = %v, want ErrUnknownLocalRecipient", err)
	}
	line := logged.String()
	if !strings.Contains(line, "unknown_recipient_count=2") {
		t.Errorf("the refusal line does not carry the exact count, which is the field that is never truncated: %q", line)
	}
	logged.Reset()
	m := ingestForTest(t, withRecipients(many[:2]...))
	m.Recipients = append([]string(nil), many...)
	if _, err := a.Accept(context.Background(), m); !errors.Is(err, ErrUnknownLocalRecipient) {
		t.Fatalf("Accept err = %v, want ErrUnknownLocalRecipient", err)
	}
	line = logged.String()
	if !strings.Contains(line, fmt.Sprintf("unknown_recipient_count=%d", len(many))) {
		t.Errorf("the refusal line must state the true count even when the id list is summarised: %q", line)
	}
	if !strings.Contains(line, "and 5 more") {
		t.Errorf("the refusal line named every id a caller supplied instead of summarising them; the line's size must be ours to bound, not the caller's: %q", line)
	}
	if strings.Contains(line, many[len(many)-1]) {
		t.Errorf("the refusal line named the last of %d ids, so nothing bounded it: %q", len(many), line)
	}
}

// TestAcceptRelayRefusesARelayedBroadcast: ValidateRelayRequest already refuses
// one (the canonical signing format has no bytes for an empty audience), and
// this is the belt to that braces — Accept is exported, and a broadcast admitted
// here would be delivered to every agent on this bus with NO recipient having
// been roster-checked, which is the whole of check 1 turned off by one boolean.
func TestAcceptRelayRefusesARelayedBroadcast(t *testing.T) {
	h := newAcceptHarness(t, localBus+".alpha-1")
	m := ingestForTest(t, withRecipients(localBus+".alpha-1"))
	m.Broadcast = true
	m.Recipients = nil

	if _, err := h.a.Accept(context.Background(), m); !errors.Is(err, ErrInvalidRelay) {
		t.Fatalf("Accept err = %v, want ErrInvalidRelay", err)
	}
	if h.local.writes() != 0 || h.rec.wrote() {
		t.Fatalf("a relayed broadcast reached the durable write; calls=%v", h.rec.snapshot())
	}
}
