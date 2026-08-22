package main

// ACK-5 — THE PEER SURFACE'S HALF OF THE BACKWARD HOP.
//
// federation.settleAck used to have exactly one answer for "no durable row
// here": the UNIFORM refusal. It now has TWO, and which one applies is not a
// choice — it depends on whether THIS bus is the origin of the correlation key.
// At the origin a missing row is §8.2's "(none)" row and the refusal must stay
// BYTE-IDENTICAL. At an intermediate it is the ordinary shape of a TRANSIT
// acknowledgement: this bus relayed the message on and never wrote a
// sender-visible row for it at all, so the outcome belongs one hop further back
// (§9.4).
//
// Getting that split wrong is silent in both directions. Widen it and the bus
// starts POSTing terminal outcomes about its own messages to peers that never
// owed it one, on the strength of an EXPIRED row. Narrow it and every
// acknowledgement crossing this bus is refused with an answer a peer cannot
// tell from a forgery, and the origin never learns anything.
//
// The fixtures come from ackwiring_ack3_test.go (ackNullDurable, ackFedSender,
// ackFedRecipient) and relaywiring_relay24_test.go (wiringLocalBus,
// wiringPeerBus). The seam under test is the REAL *ackTransit, built by the
// REAL newAckTransit over the REAL relay.BackPropagator — only the AckSender
// (the thing that dials) and the provenance lookup are doubles, which is the
// narrowest possible substitution: everything that DECIDES is production code.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
)

// atSender is the one thing that dials, doubled and COUNTED.
//
// The count is what makes "nothing was dialled" assertable: the origin arm and
// the no-seam arm must both refuse WITHOUT contacting anybody, and an
// implementation that dialled and then discarded the answer would pass every
// error assertion below.
type atSender struct {
	mu     sync.Mutex
	calls  int
	frames []relay.PeerAckRequest
	resp   relay.PeerAckResponse
	err    error
}

func (s *atSender) PeerAck(_ context.Context, _ string, req relay.PeerAckRequest) (relay.PeerAckResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.frames = append(s.frames, req)
	if s.err != nil {
		return relay.PeerAckResponse{}, s.err
	}
	return s.resp, nil
}

func (s *atSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *atSender) last() (relay.PeerAckRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return relay.PeerAckRequest{}, false
	}
	return s.frames[len(s.frames)-1], true
}

// atSink captures the operator log. Every drop on this path must be LOUD and
// SPECIFIC (invariant 6): the refusals are uniform ON THE WIRE by design, so
// the log is the only place an operator can see WHICH settlement stopped.
type atSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *atSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *atSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// atFed is a federation with a REAL attached lifecycle table and — optionally —
// a REAL *ackTransit over a real BackPropagator.
//
// It does not go through newFederation, for the reason newAckFed already gives:
// settleAck reads only f.acks, f.busID, f.ackTransit and f.log, and standing up
// the whole ingress would make a disposition test depend on a peer store, a
// registry and a certificate.
type atFed struct {
	fed      *federation
	acks     *ack.Store
	sender   *atSender
	logs     *atSink
	pathOK   bool
	busPath  []string
	pathCall int
	mu       sync.Mutex
}

// newATFed builds the federation. withTransit=false leaves f.ackTransit nil,
// which is a LEGITIMATE configuration — a bus that accepts acknowledgements for
// keys IT originated but carries none further is exactly right for a bus that
// never relays onward.
func newATFed(t *testing.T, withTransit bool) *atFed {
	t.Helper()
	st := ack.NewStore(ack.Options{})
	if err := st.Attach(ackNullDurable{}); err != nil {
		t.Fatalf("ack.Store.Attach: %v", err)
	}
	f := &atFed{
		acks:    st,
		sender:  &atSender{resp: relay.PeerAckResponse{Accepted: true}},
		logs:    &atSink{},
		pathOK:  true,
		busPath: []string{wiringPeerBus, wiringLocalBus},
	}
	log := logging.New(f.logs, logging.LevelDebug)

	fed := &federation{busID: wiringLocalBus, acks: st, log: log}
	if withTransit {
		prop, err := relay.NewBackPropagator(relay.BackPropagatorConfig{
			BusID:  wiringLocalBus,
			Sender: f.sender,
			// THE PEER REGISTRY, reduced to the one question it answers. It
			// resolves every bus but THIS one, so a self-directed hop is still
			// refused by relay's own guard rather than by this double.
			PeerBaseURL: func(busID string) (string, bool) {
				if strings.EqualFold(busID, wiringLocalBus) {
					return "", false
				}
				return "https://" + busID + ".invalid", true
			},
			Logger: log,
		})
		if err != nil {
			t.Fatalf("relay.NewBackPropagator: %v", err)
		}
		at, err := newAckTransit(wiringLocalBus, f.provenance, prop, log)
		if err != nil {
			t.Fatalf("newAckTransit: %v", err)
		}
		fed.ackTransit = at
	}
	f.fed = fed
	return f
}

// provenance is the STORED bus path lookup: origin-first and ending at THIS
// bus, which is the shape relay.UpstreamHop requires. A miss models the message
// having been pruned by retention, which is the accepted cost of holding no
// durable row for a relayed message.
func (f *atFed) provenance(string) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pathCall++
	if !f.pathOK {
		return nil, false
	}
	return append([]string(nil), f.busPath...), true
}

func (f *atFed) setPath(ok bool, path ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pathOK = ok
	f.busPath = path
}

// atOriginKey and atForeignKey are correlation keys whose ORIGIN half is,
// respectively, THIS bus and the peer. The bus half is the only thing
// DisposeAck reads out of the key, and it is what decides everything below.
var (
	atOriginKey  = wiringLocalBus + "-1"
	atForeignKey = wiringPeerBus + "-1"
)

// atSettled is the SettledAck the route hands to settleAck: a frame that has
// already passed relay's closed-set validation AND relay.AuthorizePeerAck's
// obligation binding. It is `settled` (ackwiring_ack3_test.go) with the
// correlation key made a parameter, because the key's BUS HALF is the whole
// subject of this file.
func atSettled(key string, outcome relay.AckOutcome, class relay.AckClass) relay.SettledAck {
	att := relay.AckAttestedPeerBus
	sig := []byte(nil)
	if outcome.RecipientSourced() {
		att = relay.AckAttestedRecipientSignatureUnverified
		sig = bytes.Repeat([]byte{0x5A}, 64)
	}
	return relay.SettledAck{
		PeerBusID: wiringPeerBus,
		Ack: relay.ValidatedPeerAck{
			ProtocolVersion:    relay.AckWireVersion,
			CorrelationKey:     key,
			Recipient:          ackFedRecipient,
			Outcome:            outcome,
			Class:              class,
			Attestation:        att,
			Signature:          sig,
			EmittedAtUnixMilli: 1_700_000_000_000,
		},
	}
}

// TestSettleAckDisposition walks federation.settleAck's ErrNoRecord arm — which
// is now federation.disposeUnrecordedAck — in every shape it can take.
func TestSettleAckDisposition(t *testing.T) {
	ctx := context.Background()

	// -----------------------------------------------------------------------
	t.Run("AT THE ORIGIN the refusal is UNCHANGED and nothing is dialled", func(t *testing.T) {
		// A missing row on the bus that MINTED the key means the row never
		// existed, was swept, or names a recipient the sender did not address.
		// A peer must not be able to tell those apart — ErrAckNotBound's doc has
		// the argument, and it is the deliberate analogue of the 409
		// no-matching-reservation indistinguishability invariant 10 preserves.
		//
		// The seam IS wired here. That is the point: having somewhere to
		// forward to must not change the answer.
		f := newATFed(t, true)
		got, err := f.fed.settleAck(ctx, atSettled(atOriginKey, relay.AckDelivered, ""))
		if !errors.Is(err, relay.ErrAckNotBound) {
			t.Fatalf("err = %v, want relay.ErrAckNotBound (the ONE uniform refusal)", err)
		}
		// BYTE-IDENTICAL, not merely of the same family: a wrapper would add
		// text the handler could put on the wire, and the whole property is that
		// there is nothing to distinguish.
		if err.Error() != relay.ErrAckNotBound.Error() {
			t.Errorf("the refusal reads %q, want the sentinel's own %q; a peer must not be able to tell \"no such key\" from \"not addressed to that recipient\" from \"swept\"",
				err.Error(), relay.ErrAckNotBound.Error())
		}
		if got.Duplicate {
			t.Error("a refusal reported duplicate:true")
		}
		if errors.Is(err, ack.ErrNoRecord) {
			t.Error("ack.ErrNoRecord leaked to the route; the durable layer's distinguishable error must be translated, not wrapped")
		}
		// NOTHING WAS DIALLED, AND NO PATH WAS EVEN LOOKED UP. The origin test
		// is DisposeAck's own, with a NIL path, so it cannot depend on a stored
		// field that a wrong or malicious value could move.
		if n := f.sender.count(); n != 0 {
			t.Fatalf("the origin dialled %d times. This bus MINTED the key: forwarding would send its own settlement to a bus that never owed it one, and §8.4's stop rule cannot see that shape because the correlation key never changes", n)
		}
		f.mu.Lock()
		pathCalls := f.pathCall
		f.mu.Unlock()
		if pathCalls != 0 {
			t.Errorf("the origin arm consulted the stored path %d times; the path is deliberately NOT looked at, so that the stop condition cannot be moved by a stored field", pathCalls)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("AT AN INTERMEDIATE the outcome is carried one hop further back", func(t *testing.T) {
		f := newATFed(t, true)
		got, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
		if err != nil {
			t.Fatalf("settleAck at an intermediate = %v, want nil — a missing row here is not a refusal at all: this bus relayed the message on and never wrote a sender-visible row for it", err)
		}
		if got.Duplicate {
			t.Error("a forwarded outcome reported duplicate:true; this bus holds no record for it to be a duplicate OF")
		}
		if n := f.sender.count(); n != 1 {
			t.Fatalf("the intermediate dialled %d times, want 1", n)
		}
		// FORWARDED VERBATIM: the recipient's own outcome, class, clock and
		// signature, reproduced exactly (§9.4, invariant 2).
		frame, _ := f.sender.last()
		if frame.CorrelationKey != atForeignKey || frame.Recipient != ackFedRecipient {
			t.Errorf("the forwarded frame is (%q,%q), want (%q,%q)", frame.CorrelationKey, frame.Recipient, atForeignKey, ackFedRecipient)
		}
		if frame.Outcome != relay.AckDelivered.String() || frame.Class != "" {
			t.Errorf("the forwarded frame is %s/%q, want delivered and no class", frame.Outcome, frame.Class)
		}
		if frame.Attestation == nil || len(frame.Attestation.Signature) != 64 {
			t.Errorf("the forwarded attestation is %+v; an intermediate re-signs nothing", frame.Attestation)
		}
		// AND NOTHING DURABLE CHANGED HERE.
		if _, ok := f.acks.Lookup(atForeignKey, ackFedRecipient); ok {
			t.Error("the transit arm wrote a lifecycle row; invariant 4 is satisfied at the ORIGIN, by the synchronous chain, and this bus must add no durable state of its own")
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a RETRIABLE upstream failure is an error, so the peer is told NOT NOW", func(t *testing.T) {
		// Nothing was written on this bus or on any bus upstream of it, so the
		// downstream peer's identical retry is safe and is the correct remedy.
		// The route answers 503 CodeUnavailable.
		for _, status := range []int{
			http.StatusServiceUnavailable,
			http.StatusInternalServerError,
			http.StatusTooManyRequests,
			http.StatusRequestTimeout,
		} {
			f := newATFed(t, true)
			f.sender.err = &relay.PeerRefusedError{Endpoint: "https://upstream.invalid", StatusCode: status}
			got, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
			if err == nil {
				t.Fatalf("upstream %d: settleAck succeeded; the downstream peer would be told the outcome was carried when it was not", status)
			}
			if errors.Is(err, relay.ErrAckNotBound) || errors.Is(err, relay.ErrAckOutcomeConflict) {
				t.Errorf("upstream %d: reported as %v — a FINAL 409. The peer would abandon a real terminal outcome instead of retrying it", status, err)
			}
			// *PeerRefusedError survives UNWRAPPED-THROUGH, so the caller can
			// still ask Retriable() rather than re-deriving it from text.
			var refused *relay.PeerRefusedError
			if !errors.As(err, &refused) {
				t.Errorf("upstream %d: err = %v (%T), want a *relay.PeerRefusedError to survive", status, err, err)
			} else if !refused.Retriable() {
				t.Errorf("upstream %d: Retriable() = false", status)
			}
			if got.Duplicate {
				t.Errorf("upstream %d: a failure reported duplicate:true", status)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a 409 from upstream is answered DOWNSTREAM AS SUCCESS", func(t *testing.T) {
		// THIS IS THE DELIBERATE ANTI-ORACLE CHOICE, AND IT LOOKS WRONG UNTIL
		// BOTH REASONS ARE READ. A 409 is the ONE refusal that means the
		// upstream UNDERSTOOD the frame and DECIDED about it: no obligation
		// binds that recipient, or a conflicting terminal already stands there.
		//
		//  1. Re-offering a frame the origin has FINALLY refused is exactly the
		//     retry amplification §9.3 exists to stop, so answering the
		//     downstream peer 503 would make it re-send for its whole horizon.
		//  2. Forwarding the origin's 409 verbatim would turn this hop into an
		//     ORACLE: any bound peer could ask whether the ORIGIN holds a row
		//     for a recipient it names — which is precisely the uniform-answer
		//     property ErrAckNotBound protects, leaked one hop back.
		//
		// So: the settlement IS dropped, the downstream peer is told 200, and
		// the drop is logged LOUDLY (invariant 6) because that log line is the
		// only place an operator can see which one.
		f := newATFed(t, true)
		f.sender.err = &relay.PeerRefusedError{Endpoint: "https://upstream.invalid", StatusCode: http.StatusConflict, Code: relay.CodeIdempotencyViolation}
		got, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
		if err != nil {
			t.Fatalf("upstream 409: settleAck = %v, want nil (200 downstream)", err)
		}
		if got.Duplicate {
			t.Error("upstream 409: duplicate:true; this bus knows of no prior settlement")
		}
		logs := f.logs.String()
		if !strings.Contains(logs, "FINALLY REFUSED") {
			t.Errorf("the dropped settlement was not logged loudly; log was:\n%s", logs)
		}
		if !strings.Contains(logs, atForeignKey) {
			t.Errorf("the log does not name WHICH settlement stopped:\n%s", logs)
		}
		// AND THE BUS PATH IS NOT IN IT. §13.3 forbids disclosing the traversed
		// path, and a log line is the one place a routing detail leaks into an
		// operator's export without anybody deciding to disclose it.
		if strings.Contains(logs, fmt.Sprintf("%v", f.busPath)) {
			t.Errorf("the operator log echoed the traversed bus path:\n%s", logs)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a FINAL status that DECIDED NOTHING is still NOT NOW", func(t *testing.T) {
		// THE ABSORBING ARM IS 409 AND NOTHING ELSE, and the test in production
		// is the STATUS rather than Retriable(). Retriable() calls every 4xx
		// except 408/429 FINAL, so switching on it here would sweep 404 (an
		// upstream running a pre-ACK-5 binary that does not serve the route),
		// 403 (the peering was removed at the upstream) and 400 (the frame
		// encoding has drifted between the two builds) into a 200 the
		// downstream peer reads as "accepted" — for an outcome NOBODY recorded.
		//
		// All three are OPERATOR-recoverable rather than self-healing, so they
		// are answered "not now" like any other failure and logged SEPARATELY,
		// with the remedy named.
		for _, status := range []int{
			http.StatusNotFound,
			http.StatusForbidden,
			http.StatusBadRequest,
		} {
			f := newATFed(t, true)
			f.sender.err = &relay.PeerRefusedError{Endpoint: "https://upstream.invalid", StatusCode: status}
			got, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
			if err == nil {
				t.Fatalf("upstream %d: settleAck succeeded. Telling a downstream peer 'accepted' for an outcome nobody recorded is the one sentence this contract exists to prevent", status)
			}
			if errors.Is(err, relay.ErrAckNotBound) || errors.Is(err, relay.ErrAckOutcomeConflict) {
				t.Errorf("upstream %d: reported as %v — a FINAL 409 downstream, which would make the peer abandon a real terminal outcome", status, err)
			}
			if got.Duplicate {
				t.Errorf("upstream %d: a failure reported duplicate:true", status)
			}
			logs := f.logs.String()
			if !strings.Contains(logs, "WITHOUT A DECISION") {
				t.Errorf("upstream %d: the case was not logged apart from a retriable failure; nothing here improves on its own and an operator is the remedy. Log was:\n%s", status, logs)
			}
			if strings.Contains(logs, "FINALLY REFUSED") {
				t.Errorf("upstream %d: logged as an absorbed 409; only a 409 is a DECISION about the frame", status)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("with NO seam the answer is the SAME uniform refusal, and it is AUDIBLE", func(t *testing.T) {
		// A peer must not learn from the wire whether this bus federates
		// onward, so the refusal is byte-identical to the origin's. But from an
		// operator's side it is not a refusal at all — a terminal outcome
		// reached this bus and STOPS, and the origin never learns it — so the
		// case is made audible in the log.
		f := newATFed(t, false)
		if f.fed.ackTransit != nil {
			t.Fatal("the fixture wired a seam; this subtest would prove nothing")
		}
		_, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
		if !errors.Is(err, relay.ErrAckNotBound) {
			t.Fatalf("err = %v, want relay.ErrAckNotBound", err)
		}
		if err.Error() != relay.ErrAckNotBound.Error() {
			t.Errorf("the refusal reads %q, want the sentinel's own %q", err.Error(), relay.ErrAckNotBound.Error())
		}
		if n := f.sender.count(); n != 0 {
			t.Errorf("a build with no seam dialled %d times", n)
		}
		logs := f.logs.String()
		if !strings.Contains(logs, "STOPS HERE") {
			t.Errorf("the case was not made audible; log was:\n%s", logs)
		}
		if !strings.Contains(logs, atForeignKey) {
			t.Errorf("the log does not name which settlement stopped:\n%s", logs)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("an unresolvable path stops the outcome, loudly, and dials nothing", func(t *testing.T) {
		// The message is no longer retained, so this bus cannot answer "which
		// hop handed it to me". §9.4's rule is that the destination comes from
		// THIS bus's own stored path and NEVER from the frame, so there is no
		// fallback: the terminal outcome stops here.
		f := newATFed(t, true)
		f.setPath(false)
		_, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
		if err == nil {
			t.Fatal("settleAck succeeded with no resolvable path; the peer would be told the outcome was carried when nothing was sent")
		}
		if !errors.Is(err, errAckTransitUnresolved) {
			t.Errorf("err = %v, want errAckTransitUnresolved", err)
		}
		if n := f.sender.count(); n != 0 {
			t.Errorf("%d dials were made with no resolved destination", n)
		}
		if logs := f.logs.String(); !strings.Contains(logs, "NOTHING was dialled") {
			t.Errorf("the drop was not logged loudly; log was:\n%s", logs)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a stored path that does not end at us is REFUSED, never searched", func(t *testing.T) {
		// The check that stops a peer-supplied path from choosing who this bus
		// contacts. A path naming us in the MIDDLE would, under a search, let a
		// peer pick the bus we POST a terminal outcome to.
		for _, tc := range []struct {
			name string
			path []string
		}{
			{"ends at some other bus", []string{wiringLocalBus, wiringPeerBus}},
			{"names us twice", []string{wiringLocalBus, wiringPeerBus, wiringLocalBus}},
			{"one hop", []string{wiringLocalBus}},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				f := newATFed(t, true)
				f.setPath(true, tc.path...)
				_, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
				if err == nil {
					t.Fatal("settleAck succeeded on a path this bus did not write the end of")
				}
				if !errors.Is(err, relay.ErrNoUpstreamHop) {
					t.Errorf("err = %v, want relay.ErrNoUpstreamHop", err)
				}
				if n := f.sender.count(); n != 0 {
					t.Errorf("%d dials were made from an untrusted path", n)
				}
			})
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a NEGATIVE terminal transits with its class intact", func(t *testing.T) {
		// The class is the RECIPIENT's own declaration and an intermediate
		// re-classifies nothing. A class dropped here would reach the origin as
		// a `refused` with no explanation — or, worse, be rebuilt from the
		// bus-emitted half of the closed set, which is a different claim about a
		// different party.
		f := newATFed(t, true)
		if _, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckRefused, relay.AckRecipientRefusedPolicy)); err != nil {
			t.Fatalf("settleAck: %v", err)
		}
		frame, ok := f.sender.last()
		if !ok {
			t.Fatal("nothing was forwarded")
		}
		if frame.Class != relay.AckRecipientRefusedPolicy.String() {
			t.Errorf("class = %q, want %q", frame.Class, relay.AckRecipientRefusedPolicy.String())
		}
		if frame.Outcome != relay.AckRefused.String() {
			t.Errorf("outcome = %q, want %q", frame.Outcome, relay.AckRefused.String())
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a row that DOES exist still settles here, seam or no seam", func(t *testing.T) {
		// The transit arm is reachable from ONE place — the ErrNoRecord arm —
		// and a row, when one exists, remains the only authority for settling
		// it. This is the assertion that goes red if the disposition is ever
		// moved above the settle.
		f := newATFed(t, true)
		if err := f.acks.Accept(atForeignKey, ackFedSender, ackFedRecipient); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		got, err := f.fed.settleAck(ctx, atSettled(atForeignKey, relay.AckDelivered, ""))
		if err != nil {
			t.Fatalf("settleAck with a real row = %v, want nil", err)
		}
		if got.Duplicate {
			t.Error("the FIRST terminal reported duplicate:true")
		}
		if n := f.sender.count(); n != 0 {
			t.Fatalf("a settled row was ALSO forwarded upstream (%d dials). The row is the authority; forwarding as well would make the origin's own outcome travel back out", n)
		}
		rec, ok := f.acks.Lookup(atForeignKey, ackFedRecipient)
		if !ok || rec.State != ack.StateDelivered {
			t.Errorf("durable row = (%+v,%v), want delivered", rec, ok)
		}
	})
}

// ---------------------------------------------------------------------------
// The OUTBOUND meter
// ---------------------------------------------------------------------------

// atMeterBus and atMeterKey name a SECOND upstream, so "the meter is per-peer"
// can be told from "the meter is global". The key's bus half is the ORIGIN bus
// (ids.ParseMessageID reads it out of `<origin-bus-id>-<seq>`), and on a two-hop
// stored path the origin IS the upstream.
const (
	atMeterBus = "otherbus"

	// atUpperKey selects a stored path whose upstream hop is wiringPeerBus in
	// UPPERCASE — the same neighbour as atForeignKey, differently spelt.
	atUpperKey = wiringPeerBus + "-9"
	atMeterKey = atMeterBus + "-1"
)

// atMeterSender is the one thing that dials, doubled, COUNTED PER UPSTREAM and
// — crucially — BLOCKING.
//
// A cap on IN-FLIGHT work cannot be tested with a sender that returns
// immediately: every call would have finished and released its slot before the
// next began, so a meter and no meter would look identical. This one parks each
// call until hold is closed, announcing its arrival first, which is what makes
// "the cap is reached" an observable state rather than a race.
type atMeterSender struct {
	mu      sync.Mutex
	total   int
	byURL   map[string]int
	err     error
	arrived chan string
	hold    chan struct{}
}

func newATMeterSender() *atMeterSender {
	return &atMeterSender{
		byURL: make(map[string]int),
		// Buffered well past anything this test starts, so announcing an arrival
		// can never itself block a call and turn a missing assertion into a
		// deadlock the harness reports as a timeout instead of a failure.
		arrived: make(chan string, 64),
		hold:    make(chan struct{}),
	}
}

func (s *atMeterSender) PeerAck(ctx context.Context, baseURL string, _ relay.PeerAckRequest) (relay.PeerAckResponse, error) {
	s.mu.Lock()
	s.total++
	s.byURL[baseURL]++
	err := s.err
	s.mu.Unlock()

	s.arrived <- baseURL
	select {
	case <-s.hold:
	case <-ctx.Done():
		return relay.PeerAckResponse{}, ctx.Err()
	}
	if err != nil {
		return relay.PeerAckResponse{}, err
	}
	return relay.PeerAckResponse{Accepted: true}, nil
}

func (s *atMeterSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func (s *atMeterSender) countFor(busID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byURL["https://"+busID+".invalid"]
}

func (s *atMeterSender) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// release lets every parked call — and every later one — return.
func (s *atMeterSender) release() { close(s.hold) }

// newATMeter builds the REAL *ackTransit over the REAL relay.BackPropagator.
// Only the sender and the provenance lookup are doubles; everything that
// DECIDES, including the meter under test, is production code.
func newATMeter(t *testing.T) (*ackTransit, *atMeterSender, *atSink) {
	t.Helper()
	sender := newATMeterSender()
	sink := &atSink{}
	log := logging.New(sink, logging.LevelDebug)

	prop, err := relay.NewBackPropagator(relay.BackPropagatorConfig{
		BusID:  wiringLocalBus,
		Sender: sender,
		PeerBaseURL: func(busID string) (string, bool) {
			if strings.EqualFold(busID, wiringLocalBus) {
				return "", false
			}
			return "https://" + busID + ".invalid", true
		},
		Logger: log,
	})
	if err != nil {
		t.Fatalf("relay.NewBackPropagator: %v", err)
	}

	// The STORED path: origin-first and ending at THIS bus, which is the shape
	// relay.UpstreamHop requires. Two keys, two upstreams.
	paths := map[string][]string{
		atForeignKey: {wiringPeerBus, wiringLocalBus},
		atMeterKey:   {atMeterBus, wiringLocalBus},
		// THE SAME NEIGHBOUR AS atForeignKey, SPELT IN A DIFFERENT CASE. It is a
		// legal bus id — ids.BusIDPattern is ^[A-Za-z0-9_-]{1,64}$, so uppercase
		// passes ids.ValidateBusID — and hub.relayedBusPath stores hops VERBATIM
		// with no normalisation, so a stored path really can carry this spelling.
		atUpperKey: {strings.ToUpper(wiringPeerBus), wiringLocalBus},
	}
	at, err := newAckTransit(wiringLocalBus, func(key string) ([]string, bool) {
		p, ok := paths[key]
		if !ok {
			return nil, false
		}
		return append([]string(nil), p...), true
	}, prop, log)
	if err != nil {
		t.Fatalf("newAckTransit: %v", err)
	}
	return at, sender, sink
}

// atMeterFrame is the frame TransitAck is handed. It is already past relay's
// closed-set validation; only the correlation key matters here, because it is
// what selects the stored path and therefore the upstream.
func atMeterFrame(key string) relay.PeerAckRequest {
	return relay.AckFrameFrom(atSettled(key, relay.AckDelivered, "").Ack)
}

// waitArrival blocks until a call reaches the sender, or fails the test.
func waitArrival(t *testing.T, s *atMeterSender) string {
	t.Helper()
	select {
	case url := <-s.arrived:
		return url
	case <-time.After(10 * time.Second):
		t.Fatal("no call reached the sender within 10s; a transit that should have been ADMITTED was refused, so the meter is bounding more than it may")
		return ""
	}
}

// TestAckTransitOutboundMeter pins the OUTBOUND in-flight cap.
//
// # THE DEFECT IT CLOSES
//
// hub.AcknowledgeDelivery reports a transit acknowledgement STATELESSLY, so an
// authenticated agent that is a named recipient of ONE retained relayed message
// can POST /v1/ack in an unbounded loop, and every POST synchronously dials the
// upstream peer. Unmetered, that is (a) this bus's own cross-bus delivery denied
// for the duration and (b) unbounded mutual-TLS handshakes driven at a
// NEIGHBOURING bus — the half that crosses a trust boundary — where they consume
// an 8-slot admission bucket that neighbour SHARES with relay message ingest.
//
// Every assertion below is about work NOT DONE, which is why the sender is
// counted: an implementation that dialled and then discarded the answer would
// satisfy every error assertion and none of the count ones.
func TestAckTransitOutboundMeter(t *testing.T) {
	ctx := context.Background()
	limit := maxConcurrentAckTransitsPerUpstream
	if limit < 1 {
		t.Fatalf("maxConcurrentAckTransitsPerUpstream = %d; a cap below 1 is a permanent outage, not a bound", limit)
	}

	// -----------------------------------------------------------------------
	t.Run("two CASE SPELLINGS of one upstream share ONE budget", func(t *testing.T) {
		// THE GUARD THIS PINS IS enterUpstream's strings.ToLower ON THE KEY, and
		// before this subtest existed deleting that call left the whole of
		// TestAckTransitOutboundMeter green. That is the eighth guard-that-cannot-fire
		// this project has hunted, and it sits in the one function that meters a
		// security boundary.
		//
		// # WHY THE SPELLING IS REACHABLE AT ALL, WHICH IS THE WHOLE POINT
		//
		// Three facts compose. ids.BusIDPattern is ^[A-Za-z0-9_-]{1,64}$, so an
		// uppercase bus id is LEGAL. hub.relayedBusPath (internal/hub/relayingest.go)
		// stores the hops it received VERBATIM — it validates each one and refuses a
		// self-loop, but it normalises nothing. And nothing binds an arriving path's
		// PREFIX to the peer that authenticated (ACK-5-FU-BUSPATH-SENDER). So a peer
		// that influences the prefix can name one real neighbour in several cases.
		//
		// Without the fold each spelling would open its OWN bucket of
		// maxConcurrentAckTransitsPerUpstream, and the cap — whose entire job is to
		// leave room in a NEIGHBOUR's shared admission bucket — would dissolve by
		// however many spellings the attacker cares to enumerate.
		//
		// # WHAT IS ASSERTED, AND WHY IT DOES NOT DEPEND ON THE ADDRESS RESOLVING
		//
		// enterUpstream is taken BEFORE relay.BackPropagator.Propagate consults
		// PeerBaseURL — deliberately, so that a refusal costs the neighbour nothing.
		// So the KEYING is observable on its own: with the cap already full under one
		// spelling, the other spelling must be refused by the METER, before any
		// address is looked up. That keeps this subtest a test of the meter and not
		// of the registry's own (exact-match) resolution rules.
		at, sender, _ := newATMeter(t)

		// Fill the cap under the LOWERCASE spelling and park every call inside the
		// sender, so the slots are genuinely held rather than raced.
		for i := 0; i < limit; i++ {
			go func() { _ = at.TransitAck(ctx, atMeterFrame(atForeignKey)) }()
		}
		for i := 0; i < limit; i++ {
			if got := waitArrival(t, sender); got != "https://"+wiringPeerBus+".invalid" {
				t.Fatalf("call %d arrived at %q, want the lowercase upstream's address", i, got)
			}
		}
		if n := sender.count(); n != limit {
			t.Fatalf("%d dials with the cap %d filled; the meter admitted more than it may", n, limit)
		}

		// NOW the UPPERCASE spelling of the SAME neighbour. It must be refused by
		// the meter, and it must not dial.
		err := at.TransitAck(ctx, atMeterFrame(atUpperKey))
		if !errors.Is(err, errAckTransitSaturated) {
			t.Fatalf("a transit toward %q (the same neighbour as %q, differently spelt) returned %v, want errAckTransitSaturated. Two spellings of one bus id must not buy two budgets: the cap exists to leave room in that neighbour's admission bucket, which it SHARES with relay message ingest, and a per-spelling bucket dissolves it",
				strings.ToUpper(wiringPeerBus), wiringPeerBus, err)
		}
		if n := sender.count(); n != limit {
			t.Fatalf("the differently-cased transit DIALLED: %d calls, want the cap %d. A refusal must cost the neighbour nothing at all", n, limit)
		}
	})

	t.Run("past the cap the excess is refused and NOTHING is dialled", func(t *testing.T) {
		at, sender, sink := newATMeter(t)

		// Fill the cap and WAIT until every one of them is parked inside the
		// sender. Without that wait this asserts a race rather than a bound.
		done := make(chan error, limit)
		for i := 0; i < limit; i++ {
			go func() { done <- at.TransitAck(ctx, atMeterFrame(atForeignKey)) }()
		}
		for i := 0; i < limit; i++ {
			if got := waitArrival(t, sender); got != "https://"+wiringPeerBus+".invalid" {
				t.Fatalf("call %d arrived at %q, want the upstream's own address", i, got)
			}
		}
		if n := sender.count(); n != limit {
			t.Fatalf("%d dials with the cap %d filled; the meter admitted more than it may", n, limit)
		}

		// NOW the excess, serially, so each refusal is deterministic.
		for i := 0; i < 3; i++ {
			err := at.TransitAck(ctx, atMeterFrame(atForeignKey))
			if !errors.Is(err, errAckTransitSaturated) {
				t.Fatalf("excess call %d: err = %v, want errAckTransitSaturated — an unmetered transit is an agent-driven, infinitely repeatable outbound peer request", i, err)
			}
			// NOT dressed as an upstream refusal. Nothing was refused by any
			// upstream, because nothing reached one, and the peer surface
			// classifies *relay.PeerRefusedError by its STATUS.
			var refused *relay.PeerRefusedError
			if errors.As(err, &refused) {
				t.Errorf("excess call %d: the local refusal carries an upstream status (%d) no upstream ever sent", i, refused.StatusCode)
			}
			if n := sender.count(); n != limit {
				t.Fatalf("excess call %d DIALLED: %d calls, want the cap %d. The refusal must cost the neighbour nothing at all", i, n, limit)
			}
		}

		// LOUD AND SPECIFIC (invariant 6): an operator must be able to tell this
		// from a real failure at the neighbour, because on the wire they are the
		// same 503.
		logs := sink.String()
		if !strings.Contains(logs, "NOTHING was dialled") {
			t.Errorf("the refusal was not logged loudly; log was:\n%s", logs)
		}
		if !strings.Contains(logs, wiringPeerBus) {
			t.Errorf("the log does not name WHICH upstream is saturated:\n%s", logs)
		}

		sender.release()
		for i := 0; i < limit; i++ {
			if err := <-done; err != nil {
				t.Errorf("an ADMITTED transit failed: %v", err)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the meter is PER UPSTREAM, so a saturated peer does not refuse another", func(t *testing.T) {
		// A GLOBAL cap would be a denial of service dressed as a bound: one
		// unresponsive neighbour would stop this bus acknowledging to every
		// OTHER neighbour, none of which did anything.
		at, sender, _ := newATMeter(t)

		done := make(chan error, limit+1)
		for i := 0; i < limit; i++ {
			go func() { done <- at.TransitAck(ctx, atMeterFrame(atForeignKey)) }()
		}
		for i := 0; i < limit; i++ {
			waitArrival(t, sender)
		}
		if err := at.TransitAck(ctx, atMeterFrame(atForeignKey)); !errors.Is(err, errAckTransitSaturated) {
			t.Fatalf("the first upstream is not saturated (err = %v); this subtest would prove nothing", err)
		}

		// The SECOND upstream, while the first is at its cap.
		go func() { done <- at.TransitAck(ctx, atMeterFrame(atMeterKey)) }()
		if got := waitArrival(t, sender); got != "https://"+atMeterBus+".invalid" {
			t.Fatalf("the admitted call arrived at %q, want the SECOND upstream", got)
		}
		if n := sender.countFor(atMeterBus); n != 1 {
			t.Fatalf("the second upstream was dialled %d times, want 1 — the cap is being shared across peers", n)
		}

		sender.release()
		for i := 0; i < limit+1; i++ {
			if err := <-done; err != nil {
				t.Errorf("an ADMITTED transit failed: %v", err)
			}
		}
	})

	// -----------------------------------------------------------------------
	t.Run("slots are RELEASED, and the map retains no entry at zero", func(t *testing.T) {
		// A leaked slot is a PERMANENT, SILENT loss of transit capacity toward
		// that upstream, and from the outside it is indistinguishable from the
		// neighbour being slow — which is exactly why it is asserted rather than
		// reasoned about.
		at, sender, _ := newATMeter(t)

		done := make(chan error, limit)
		for i := 0; i < limit; i++ {
			go func() { done <- at.TransitAck(ctx, atMeterFrame(atForeignKey)) }()
		}
		for i := 0; i < limit; i++ {
			waitArrival(t, sender)
		}
		if n := at.meteredUpstreams(); n != 1 {
			t.Fatalf("meteredUpstreams() = %d with one upstream in flight, want 1", n)
		}
		sender.release()
		for i := 0; i < limit; i++ {
			if err := <-done; err != nil {
				t.Fatalf("an ADMITTED transit failed: %v", err)
			}
		}

		// EVERY slot back: a further limit+1 transits all succeed, which they
		// cannot if even one was leaked.
		for i := 0; i < limit+1; i++ {
			if err := at.TransitAck(ctx, atMeterFrame(atForeignKey)); err != nil {
				t.Fatalf("transit %d after the in-flight calls returned = %v, want nil; a slot was leaked", i, err)
			}
		}
		// AND THE MAP IS EMPTY. An entry left at zero would make this a registry
		// of every bus this one ever relayed from, retained for the process's
		// life; deleting at zero is what bounds it.
		if n := at.meteredUpstreams(); n != 0 {
			t.Errorf("meteredUpstreams() = %d with nothing in flight, want 0; a de-peered bus would be retained for ever", n)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("a slot is released on the ERROR path too", func(t *testing.T) {
		// The failure paths are the ones that matter: an upstream that is DOWN
		// is precisely when this bus must not ALSO lose its capacity to reach it
		// once it comes back.
		at, sender, _ := newATMeter(t)
		sender.release() // nothing blocks; every call returns at once
		sender.setErr(&relay.PeerRefusedError{Endpoint: "https://upstream.invalid", StatusCode: http.StatusServiceUnavailable})

		for i := 0; i < limit+2; i++ {
			err := at.TransitAck(ctx, atMeterFrame(atForeignKey))
			if errors.Is(err, errAckTransitSaturated) {
				t.Fatalf("failing transit %d was refused by the METER: a failed hop did not release its slot, so an upstream that is merely down becomes an upstream this bus can never reach again", i)
			}
			var refused *relay.PeerRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("failing transit %d: err = %v, want the upstream's own refusal to survive", i, err)
			}
		}

		// And once the upstream recovers, so does this bus.
		sender.setErr(nil)
		if err := at.TransitAck(ctx, atMeterFrame(atForeignKey)); err != nil {
			t.Fatalf("transit after the upstream recovered = %v, want nil", err)
		}
		if n := at.meteredUpstreams(); n != 0 {
			t.Errorf("meteredUpstreams() = %d after the error path, want 0", n)
		}
	})

	// -----------------------------------------------------------------------
	t.Run("the cap leaves room in the upstream's SHARED admission bucket", func(t *testing.T) {
		// The number is DERIVED, not picked: at the upstream our hops land in
		// its peerAdmission bucket for our principal — maxConcurrentRelayIngestsPerPeer
		// slots — and that bucket is SHARED with the relay MESSAGE ingest it
		// accepts from us. A cap at or above it would let acknowledgement
		// traffic alone starve this bus's own message forwarding at the far end.
		if maxConcurrentAckTransitsPerUpstream >= maxConcurrentRelayIngestsPerPeer {
			t.Fatalf("the outbound cap (%d) is not strictly below the upstream's shared inbound bucket (%d)", maxConcurrentAckTransitsPerUpstream, maxConcurrentRelayIngestsPerPeer)
		}
	})
}
