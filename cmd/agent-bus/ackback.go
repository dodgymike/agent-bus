package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
)

// ACK-5 — THE BACK-PROPAGATION ADAPTER, AND THE ONLY PLACE THE THREE PIECES MEET.
//
// A terminal delivery outcome travels BACKWARDS one hop at a time along the
// traversed bus path and stops at the ORIGIN bus, which is the only bus holding
// a durable sender-visible lifecycle row for it (ACK-CONTRACT.md §9.4). Two
// surfaces raise or receive an outcome this bus does not own:
//
//   - POST /v1/ack, the AGENT surface on a terminal bus: a local recipient
//     acknowledges a message that was RELAYED here, so hub.AcknowledgeDelivery
//     writes nothing durable and reports a TRANSIT acknowledgement;
//   - POST /v1/peer/ack, the PEER surface on an intermediate bus: a downstream
//     peer propagates an outcome for a key this bus RELAYED but did not
//     originate, so ack.Store.Settle answers ErrNoRecord because no row exists
//     here.
//
// Both then owe exactly the same thing — one backward hop — and this file is the
// one implementation of it. It is COMPOSITION and nothing else: the decision
// (relay.DisposeAck), the emission (relay.BackPropagator) and the provenance
// lookup (store.RelayProvenanceByOriginMessageID) each live in the package that
// owns them, and this type is the only place that knows all three exist. That is
// why it is in the composition root rather than in internal/httpapi or
// internal/relay: putting it in either would make that package depend on the
// other two, and one of them would quietly become the owner of the other's
// wiring.
//
// # IT IS SYNCHRONOUS, AND THAT IS THE WHOLE DURABILITY ARGUMENT (INVARIANT 4)
//
// Nothing upstream of us is acknowledged until the ORIGIN has fsynced the
// outcome, because each hop answers only after the next hop answers. Invariant 4
// therefore holds END TO END through the chain rather than through a local
// write — which is what makes it correct for this path to add NO durable state
// of its own. It follows that there is NO retry queue here and there must not be
// one: every failure below is answered "not now" (503) by the caller, the party
// that raised the outcome re-offers the identical frame later, and nothing is
// lost because nothing was acknowledged. Retry, backoff and the bounce path are
// ACK-7 and ACK-14, and they belong THERE, once, beside the durable outbox that
// already survives a restart.
//
// # THE ABSOLUTE ABOVE HAS EXACTLY ONE EXCEPTION, AND HERE IT IS
//
// "Nothing upstream of us is acknowledged until the ORIGIN has fsynced" holds on
// every arm EXCEPT ONE: a 409 from the next hop back. A 409 is the one refusal
// that means the upstream UNDERSTOOD the frame and DECIDED about it — no
// obligation binds that recipient, or a conflicting terminal already stands
// there — and disposeUnrecordedAck (relaywiring.go) absorbs it and answers the
// downstream peer 200. In the "no obligation binds that recipient" case nothing
// durable exists anywhere, so a party one hop back is told `accepted` for an
// outcome the origin refused.
//
// That is a deliberate, RECORDED narrowing rather than an oversight
// (DECISIONS.md, 2026-08-21, ACK-5): re-offering a finally-refused frame is the
// retry amplification §9.3 exists to stop, and forwarding the origin's verdict
// verbatim would tell any bound peer whether the ORIGIN holds a row for a
// recipient it named. EVERY OTHER final status — 404, 403, 400 — means the
// upstream decided NOTHING, is answered "not now", and the sentence above is
// intact there.
//
// # NOTHING HERE DISCONNECTS ANYBODY (§12, invariant 10)
//
// Every failure is a refusal reported to a caller that answers a status code. A
// de-peered neighbour, an upstream bus that is down, a message swept by
// retention and a merely buggy client all reach these lines.

// The transit failures. All three are unexported, and deliberately: THE CALLER
// ONLY EVER NEEDS "DID IT WORK".
//
// Every one of these is answered upstream of here as "not now" — 503 on the
// agent surface, 503 CodeUnavailable on the peer surface — because nothing was
// written anywhere and an identical retry is both safe and the correct remedy.
// A caller that could tell them apart would be tempted to answer them
// differently, and the only ways to differ are wrong: a 4xx is FINAL and would
// make the acknowledgement be abandoned (§9.3), and a distinguishable answer
// would tell a remote party whether this bus retains a message under a key it
// named. Exporting them would invite exactly that.
//
// They exist as sentinels rather than as bare text so that THIS file's own tests
// and future maintenance can tell them apart without matching an operator
// message that will be reworded.
var (
	// errAckTransitUnresolved reports that this bus can no longer resolve which
	// hop handed it the message named by the correlation key, because the
	// message is no longer retained. There is no other source for that answer:
	// §9.4's rule is that the destination comes from THIS bus's own stored bus
	// path, never from the frame.
	errAckTransitUnresolved = errors.New("ack transit: this bus no longer retains the relayed message named by this correlation key, so the hop it arrived from cannot be resolved")

	// errAckTransitAtOrigin reports that the disposition came back
	// AckStopAtOrigin — this bus minted the correlation key — on a path that is
	// only ever reached for a key this bus did NOT originate. It is a
	// fail-closed assertion about the WIRING, never a normal outcome; see
	// TransitAck step 3.
	errAckTransitAtOrigin = errors.New("ack transit: the correlation key names THIS bus as its origin, so there is nothing to propagate and this path should never have been reached")

	// errAckTransitSaturated reports that this bus already holds
	// maxConcurrentAckTransitsPerUpstream backward hops IN FLIGHT toward the
	// upstream this settlement resolves to, so nothing was resolved, nothing was
	// dialled and nothing was recorded.
	//
	// It is a THIRD sentinel because neither of the two above can say "not now,
	// try again": errAckTransitUnresolved is a fact about retention that a retry
	// cannot change, and errAckTransitAtOrigin is a wiring fault. It needs no new
	// status code — both existing surfaces already answer any transit error "not
	// now", which is exactly right here: the agent surface answers 503 +
	// `Retry-After: 1` (internal/httpapi/ack.go) and the peer surface answers 503
	// CodeUnavailable (federation.disposeUnrecordedAck's final arm). A 4xx would
	// be FINAL and would make an outcome nothing recorded be ABANDONED (§9.3).
	//
	// NOBODY IS DISCONNECTED for it (§12, invariant 10). A merely EAGER client —
	// an agent acknowledging a batch of relayed messages at once, all of which
	// arrived over the same upstream — reaches this line while doing nothing
	// wrong at all.
	errAckTransitSaturated = errors.New("ack transit: this bus already has the maximum number of backward acknowledgement hops in flight toward that upstream bus, so this one was not sent and nothing was recorded anywhere")
)

// maxConcurrentAckTransitsPerUpstream bounds how many backward acknowledgement
// hops this bus may have IN FLIGHT toward ONE upstream bus. It is the OUTBOUND
// twin of maxConcurrentRelayIngestsPerPeer (relaywiring.go), which bounds the
// same quantity in the INBOUND direction, and it is DERIVED from it rather than
// picked so the two cannot drift apart.
//
// # WHAT IT PROTECTS, AND IT IS THE NEIGHBOUR, NOT US
//
// TransitAck is AGENT-DRIVEN and infinitely repeatable: hub.AcknowledgeDelivery
// reports a transit acknowledgement statelessly, so an authenticated agent that
// is a named recipient of ONE retained relayed message can POST /v1/ack in a
// loop, and every one of those POSTs synchronously dials the upstream peer. The
// pinned peer transport sets MaxIdleConnsPerHost but no MaxConnsPerHost
// (relaydial.go), so concurrent calls do not queue — they become fresh mutual-TLS
// handshakes at a bus that did nothing wrong. That half crosses a TRUST
// BOUNDARY, which is what makes this a bound rather than a tuning knob.
//
// # THE NUMBER, DERIVED
//
// At the upstream, our acknowledgements land in ITS peerAdmission bucket for our
// principal — maxConcurrentRelayIngestsPerPeer (8) in-flight slots — and THAT
// BUCKET IS SHARED WITH RELAY MESSAGE INGEST. So the requirement is not "some
// limit" but "this bus must never be able, on its own, to fill a neighbour's
// per-peer admission budget". Half is the split that states it: at most 4
// acknowledgement hops in flight toward any one upstream leaves at least 4 slots
// there for the MESSAGES this bus is relaying to the same neighbour over the
// same principal. Anything at or above 8 would let acknowledgement traffic alone
// starve our own message forwarding at the far end.
//
// It is deliberately NOT the upstream's whole budget divided among our peers,
// and not a rate limit: an agent under the cap is never slowed, however fast it
// acknowledges, exactly as peerAdmission never slows a peer under its share.
const maxConcurrentAckTransitsPerUpstream = maxConcurrentRelayIngestsPerPeer / 2

// ackTransitTimeout bounds ONE backward acknowledgement hop, and it deliberately
// MIRRORS relay.DefaultForwardTimeout — the same constant the relay forwarder
// bounds ONE outbound peer attempt with (relay.ForwarderOptions.Timeout defaults
// to it, and cmd/agent-bus never overrides it). It is REFERENCED rather than
// copied so the two cannot drift: a backward ACK hop is an outbound peer request
// over the same pinned mutual-TLS client as a relayed message, so a different
// number here would be a second, undocumented answer to "how long may this bus
// wait on a peer".
//
// # WITHOUT IT THIS PATH IS BOUNDED BY NOTHING
//
// Every other candidate deadline is absent or too narrow. The pinned peer HTTP
// client has NO Timeout of its own — relaydial.go says so explicitly and points
// at ForwarderOptions.Timeout as the belt, which THIS path does not wear,
// because it emits through relay.BackPropagator rather than through the
// forwarder's queues. The server leaves ReadTimeout/WriteTimeout unset for the
// long-poll. peerDialTimeout covers connect and the TLS handshake only, so an
// upstream that accepts a connection and then never answers is unbounded.
//
// # AND AN UNBOUNDED HOP DENIES A NEIGHBOUR ITS MESSAGE INGEST, NOT MERELY ITS
// # ACKNOWLEDGEMENTS
//
// On the peer surface a stalled call holds one of only MaxConcurrentPerPeer (8)
// in-flight slots, and THAT BUCKET IS SHARED WITH RELAY MESSAGE INGEST. So a
// single unresponsive upstream would consume an honest downstream peer's
// admission budget and stop it delivering MESSAGES — a fault in one bus
// converted into an outage in a neighbour that did nothing wrong.
//
// # THE COST OF MIRRORING RATHER THAN SHORTENING IT, STATED
//
// 30 s is a FORWARD-attempt deadline being reused for a BACKWARD one, and the
// two are not identical in what they hold. A downstream peer's acknowledgements
// arriving here each occupy one of ITS 8 peerAdmission slots on this bus for as
// long as we wait on OUR upstream — so 8 acknowledgements stalled against an
// unresponsive upstream block that downstream peer's MESSAGE ingest here for up
// to 30 s, because the bucket is shared. A shorter transit-specific deadline
// would shrink that window, and it is a defensible change; it is NOT made here,
// because the value is a documented, referenced mirror and changing it is a
// separate decision from adding a bound. The OUTBOUND half of the same
// amplification — this bus filling a NEIGHBOUR's bucket — is what
// maxConcurrentAckTransitsPerUpstream bounds, and that one needed no new number
// to be invented either.
const ackTransitTimeout time.Duration = relay.DefaultForwardTimeout

// ackTransit is the httpapi.AckTransit implementation: it resolves the upstream
// hop from this bus's own retained provenance and hands the frame to the
// back-propagator.
//
// Every COLLABORATOR field is set at construction and read-only afterwards, so
// it is safe for concurrent use to exactly the extent those collaborators are,
// and all are: RelayProvenanceByOriginMessageID takes the store's own lock, and
// relay.BackPropagator.Propagate documents itself as concurrent-safe. The ONE
// piece of mutable state is the outbound meter (mu/inFlight), which is guarded
// here; an earlier version of this comment said the type held none, and that was
// true right up until the meter was needed.
type ackTransit struct {
	// busID is THIS bus's server-minted id (invariant 1). It is what DisposeAck
	// measures the correlation key and the stored path against, and it never
	// travels on the frame — PeerAckRequest has no bus-id field at all.
	busID string

	// provenance answers "what path did the message under this correlation key
	// travel", and NOTHING ELSE. It is a func rather than the store so this
	// adapter cannot reach a body: the accessor behind it
	// (store.RelayProvenanceByOriginMessageID) returns routing metadata only,
	// which is invariant 6's line drawn at the seam rather than trusted to a
	// caller.
	//
	// The path it returns is ORIGIN-FIRST AND ENDS AT THIS BUS, because that is
	// what hub.relayedBusPath stored. relay.UpstreamHop requires exactly that
	// shape and refuses to search for this bus elsewhere in the path, so the only
	// hop we will ever contact is the one adjacent to a position WE wrote — and
	// relay.BackPropagator.Propagate then refuses any bus that is not in THIS
	// bus's peer registry with a base URL, so no frame ever names an address,
	// host or scheme. Handing it a WIRE path here would reopen that.
	//
	// CORRECTED 2026-08-21 (ACK-5). This paragraph used to end "precisely so a
	// peer-fabricated path cannot choose who this bus contacts", and THAT IS NOT
	// TRUE — it overclaims in the dangerous direction. hub.relayedBusPath
	// validates the arriving path's shape but NOTHING binds its last hop to the
	// peer that authenticated: hub.RelayedIngestRequest has no peer-principal
	// field at all. So an authenticated peer CAN place a different bus we peer
	// with immediately before us. What bounds the residual is §6.2's obligation
	// binding at the far end, which refuses an ACK that bus was never owed. The
	// unbound half is tracked as ACK-5-FU-BUSPATH-SENDER and is NOT closed here.
	provenance func(correlationKey string) (busPath []string, ok bool)

	// prop is the emitting half: it is TOLD the upstream bus id and resolves
	// that id's ADDRESS through this bus's own peer registry. It is the only
	// thing on this path that dials anything.
	prop *relay.BackPropagator

	// log receives the refusals, loudly and specifically (invariant 6). Never
	// nil after newAckTransit.
	log *logging.Logger

	// mu guards inFlight and NOTHING ELSE. It is never held across a dial: the
	// slot is taken, the lock is dropped, the hop is made, and the release
	// re-takes it. Holding it across Propagate would serialise every backward
	// hop this bus makes to every upstream behind one unresponsive neighbour,
	// which is a worse outage than the one being prevented.
	mu sync.Mutex

	// inFlight counts the backward hops currently in flight PER UPSTREAM BUS,
	// keyed lower-cased (bus ids compare case-insensitively everywhere on this
	// path — relay.UpstreamHop uses strings.EqualFold — so two spellings must
	// not buy two budgets).
	//
	// # WHY THIS MAP CANNOT GROW WITHOUT BOUND, WHICH IS THE WHOLE QUESTION
	//
	// Two independent bounds, and the argument is the shape federation.rosterMemo
	// makes for its own:
	//
	//  1. THE KEY IS SHAPE-CONSTRAINED, AND THAT IS ALL — CORRECTED 2026-08-21.
	//     A key reaches here only after relay.DisposeAck resolved it out of THIS
	//     bus's OWN stored bus path, validated hop by hop by relay.UpstreamHop and
	//     required to END at this bus. So it is always a syntactically valid bus
	//     id (ids.ValidateBusID) that is not our own.
	//
	//     THIS PARAGRAPH USED TO CLAIM MORE, AND THE CLAIM WAS FALSE. It said the
	//     resolvable key set was "the set of buses this bus has actually relayed
	//     FROM, which the peer registry bounds at relay.MaxPeers". Only the LAST
	//     hop of a stored path is written by this bus; the hop before it — which is
	//     the one metered here — comes from the PREFIX the sending peer supplied,
	//     and nothing binds that prefix to anything (ACK-5-FU-BUSPATH-SENDER).
	//     Worse for the old claim: the meter is taken BEFORE PeerBaseURL is ever
	//     consulted, deliberately, so that a refusal costs the neighbour nothing —
	//     which means a fabricated or de-peered id DOES occupy a transient entry
	//     here and never has to resolve through the registry at all. relay.MaxPeers
	//     bounds nothing on this path.
	//
	//     THE MAP IS STILL BOUNDED, BY (2) ALONE, and (2) is sufficient on its own
	//     — an entry exists only while a hop is in flight, and concurrency is what
	//     bounds that. The correction is recorded rather than quietly deleted
	//     because a reader reasoning about release semantics would otherwise
	//     inherit a registry-sized bound that does not exist.
	//  2. AN ENTRY EXISTS ONLY WHILE A HOP IS IN FLIGHT. The release DELETES the
	//     key at zero rather than leaving a zero-valued entry, so a bus that is
	//     de-peered, renamed or never spoken to again retains nothing here — the
	//     map is transient bookkeeping, not a registry, and it is empty whenever
	//     the bus is idle. That is also what keeps bound 1 honest if the stored
	//     path ever outlives the peering it names.
	inFlight map[string]int
}

// ackTransit is what internal/httpapi's optional seam is satisfied by. The
// assertion is compile-time on purpose: this type is wired into an INTERFACE
// field, and a signature drift would otherwise surface as a nil seam at
// startup — which looks exactly like a build that deliberately does not
// federate acknowledgements.
var _ httpapi.AckTransit = (*ackTransit)(nil)

// newAckTransit validates every piece and returns the adapter, or says which one
// was missing.
//
// Each refusal names the absent dependency in the style of relay's own
// constructors, for the reason NewBackPropagator gives: on this path the silent
// version is the worse failure, because a mis-wired adapter loses terminal
// outcomes without producing an error a recipient or an operator would ever see.
func newAckTransit(busID string, provenance func(correlationKey string) ([]string, bool), prop *relay.BackPropagator, log *logging.Logger) (*ackTransit, error) {
	if err := ids.ValidateBusID(busID); err != nil {
		// OUR id, so this is a fault in this build rather than anything a remote
		// party did. DisposeAck fails closed on it too; failing here means the
		// bus does not start rather than answering 503 to every transit
		// acknowledgement for the rest of its life.
		return nil, fmt.Errorf("ack transit: this bus's own id is not valid: %w", err)
	}
	if provenance == nil {
		return nil, errors.New("ack transit: the provenance lookup is required; without it no stored bus path can be read, and §9.4 forbids taking the destination from anywhere else — an adapter with no lookup could only get one from the frame, which is the SSRF the peer surface exists to refuse")
	}
	if prop == nil {
		return nil, errors.New("ack transit: the back-propagator is required; without it every transit acknowledgement would be resolved and then dropped, which is indistinguishable from a federation with nothing to say")
	}
	if log == nil {
		log = logging.New(discardWriter{}, logging.LevelError)
	}
	return &ackTransit{
		busID:      busID,
		provenance: provenance,
		prop:       prop,
		log:        log,
		inFlight:   make(map[string]int),
	}, nil
}

// enterUpstream takes one of this bus's outbound in-flight slots for
// upstreamBusID, or refuses with errAckTransitSaturated. The returned release is
// safe to call exactly once and MUST be deferred by the caller on EVERY path,
// including the error paths: a leaked slot is a permanent, silent loss of
// transit capacity toward that upstream, and it would look from the outside
// exactly like the neighbour being slow.
//
// It is the peerAdmission.enter shape (relaywiring.go) in the other direction,
// and DELIBERATELY NOT peerAdmission itself — see TransitAck step 4.
func (a *ackTransit) enterUpstream(upstreamBusID string) (func(), error) {
	limit := maxConcurrentAckTransitsPerUpstream
	if limit < 1 {
		// A DERIVED constant must never evaluate to "refuse everything". If
		// maxConcurrentRelayIngestsPerPeer is ever lowered below 2 the integer
		// halving above reaches 0, and a zero cap here would not be a tight bound
		// — it would be a permanent, total outage of cross-bus acknowledgement
		// with no error naming the cause. One is the smallest bound that still
		// works.
		limit = 1
	}

	key := strings.ToLower(upstreamBusID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := a.inFlight[key]; n >= limit {
		return nil, fmt.Errorf("%w: %d hops are already in flight toward it, the limit is %d", errAckTransitSaturated, n, limit)
	}
	a.inFlight[key]++
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		// DELETE AT ZERO, never leave a zero-valued entry: an idle upstream must
		// hold no room in this map at all. See the inFlight field doc.
		if n := a.inFlight[key]; n > 1 {
			a.inFlight[key] = n - 1
			return
		}
		delete(a.inFlight, key)
	}, nil
}

// meteredUpstreams reports how many upstream buses currently hold at least one
// in-flight slot.
//
// IT EXISTS FOR THE TEST — TestAckTransitOutboundMeter's "the map does not
// retain an entry once a peer's count reaches zero" case — and for nothing else.
// The alternative was for the test to read a.inFlight directly, which under
// -race would need it to take a.mu by hand at every assertion; one accessor that
// locks correctly is safer than five open-coded ones. It is not called from
// production code and must not become so: it is a snapshot that is stale the
// instant it returns.
func (a *ackTransit) meteredUpstreams() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inFlight)
}

// TransitAck carries ONE terminal outcome ONE hop back toward the origin bus.
//
// # THE FIVE STEPS, IN THIS ORDER
//
//  1. RESOLVE THE PATH from this bus's OWN retained provenance. Nothing in
//     frame names an address, a host, a scheme or a destination bus, and nothing
//     here may ever pass one (§9.4).
//  2. DECIDE, through relay.DisposeAck — the single spelling of "who, if
//     anybody, does this go to next".
//  3. ASSERT that the answer is not "stop at the origin". See below.
//  4. METER, per upstream bus, BEFORE any address is resolved and before
//     anything is dialled. This path is agent-driven and repeatable; without a
//     bound one ordinary agent drives unbounded TLS handshakes at a neighbour.
//  5. EMIT, through relay.BackPropagator, which resolves the upstream bus id's
//     address in this bus's own peer registry and dials that and nothing else.
//
// The error is returned so the CALLER can classify it — *relay.PeerRefusedError
// survives unwrapped-through, so `Retriable()` is still answerable one layer up
// — but no caller is expected to act on WHICH failure it was: see the sentinels
// above. Nothing here is retried, and nothing here writes anything durable.
func (a *ackTransit) TransitAck(ctx context.Context, frame relay.PeerAckRequest) error {
	// 1. THE STORED PATH, FROM OUR OWN RECORD.
	busPath, ok := a.provenance(frame.CorrelationKey)
	if !ok {
		// LOUD AND SPECIFIC (invariant 6): a terminal outcome is being dropped,
		// and a discard nobody can diagnose is the actual defect. The line names
		// WHICH settlement stopped and WHY, and says what the remedy is.
		//
		// THE BUS PATH IS NOT LOGGED, here or anywhere on this path: §13.3
		// forbids disclosing the traversed path to a sender, and a log line is
		// the one place a routing detail leaks into an operator's export without
		// anybody deciding to disclose it. The correlation key and the recipient
		// are enough to identify the settlement.
		a.log.Warn("acknowledgement NOT propagated: this bus no longer retains the RELAYED message this correlation key names, so the hop it arrived from cannot be resolved and NOTHING was dialled. The terminal outcome STOPS HERE and the origin bus will not learn it from this hop. The destination is derived from this bus's own stored bus path and never from the frame (ACK-CONTRACT.md §9.4), so there is no fallback — the remedy is message retention long enough to cover the acknowledgement window",
			"local_bus", a.busID,
			"correlation_key", elideAckTransit(frame.CorrelationKey, ids.MaxMessageIDLen),
			"recipient", elideAckTransit(frame.Recipient, ids.MaxAgentIDLen),
			"outcome", frame.Outcome,
		)
		return fmt.Errorf("%w: the terminal outcome stops at this bus", errAckTransitUnresolved)
	}

	// 2. THE DECISION. DisposeAck validates our own bus id, parses the
	//    correlation key rather than trusting it, and refuses a stored path that
	//    does not end at us — the check that stops a peer-supplied path from
	//    choosing who this bus contacts.
	disp, upstream, err := relay.DisposeAck(a.busID, frame.CorrelationKey, busPath)
	if err != nil {
		return fmt.Errorf("ack transit: no upstream hop could be derived for this acknowledgement: %w", err)
	}

	// 3. THE FAIL-CLOSED ASSERTION. UNREACHABLE from both call sites: the agent
	//    surface reaches transit only for a message that was RELAYED here (so
	//    the origin is some other bus), and the peer surface tests for the
	//    origin itself before it ever calls this. Reaching it means one of those
	//    two gates has been changed and this bus is about to treat its OWN
	//    settlement as somebody else's — the one shape §8.4's stop rule cannot
	//    see, because the correlation key never changes. It is an ERROR-level
	//    wiring fault, never a normal path, and it is refused rather than
	//    "handled": there is nowhere for an origin's own outcome to go.
	if disp == relay.AckStopAtOrigin {
		a.log.Error("acknowledgement NOT propagated: the correlation key names THIS bus as the ORIGIN, on a path that is only ever reached for a key this bus did not originate. This is a WIRING fault — the caller's own origin test has been changed or removed — and nothing was sent",
			"local_bus", a.busID,
			"correlation_key", elideAckTransit(frame.CorrelationKey, ids.MaxMessageIDLen),
			"recipient", elideAckTransit(frame.Recipient, ids.MaxAgentIDLen),
			"disposition", disp.String(),
		)
		return errAckTransitAtOrigin
	}

	// 4. THE METER, TAKEN BEFORE ANY ADDRESS IS RESOLVED AND BEFORE ANYTHING IS
	//    DIALLED. relay.BackPropagator.Propagate resolves the upstream's address
	//    and opens the connection, so a check placed after it would be a counter
	//    rather than a bound — the same discipline peerAdmission follows on the
	//    inbound side ("it refuses before the write, never after").
	//
	//    IT IS DELIBERATELY *NOT* peerAdmission, AND THAT IS THE WHOLE POINT:
	//    peerAdmission is this bus's INBOUND admission bucket, SHARED between the
	//    peer ACK route and relay MESSAGE ingest, so spending it on an OUTBOUND
	//    operation would convert an outbound amplification problem into an
	//    INBOUND denial — a burst of local agent acknowledgements would start
	//    refusing legitimate peer traffic ARRIVING here, which is strictly worse
	//    than the problem being fixed and would be very hard to diagnose from
	//    either side. The two directions are metered separately, on purpose.
	//
	//    RELEASED BY defer ON EVERY PATH, including the error ones. See
	//    enterUpstream.
	release, err := a.enterUpstream(upstream)
	if err != nil {
		// LOUD AND SPECIFIC (invariant 6), ONCE per refusal, and bounded: every
		// remote-chosen string goes through elideAckTransit, so the party that
		// chose the bytes does not choose the size of the line written about it.
		// It says WHICH upstream is saturated and that NOTHING was recorded, so
		// an operator can tell this from a real failure at the neighbour — the
		// two are indistinguishable on the wire, because both are answered "not
		// now".
		//
		// NOTHING DISCONNECTS (§12, invariant 10): an agent acknowledging a batch
		// of relayed messages that all arrived over one upstream is merely eager.
		a.log.Warn("acknowledgement NOT propagated: this bus is already at its limit of backward acknowledgement hops IN FLIGHT toward that upstream bus, so NOTHING was resolved, NOTHING was dialled and NOTHING was recorded. The caller is answered 'not now' and an identical retry is safe and is the correct remedy. This bound exists so that acknowledgements this bus emits cannot fill the upstream's per-peer admission budget, which that bus SHARES with the relay message ingest it accepts from us",
			"local_bus", a.busID,
			"upstream_bus", elideAckTransit(upstream, relay.MaxPeerBusIDLen),
			"correlation_key", elideAckTransit(frame.CorrelationKey, ids.MaxMessageIDLen),
			"recipient", elideAckTransit(frame.Recipient, ids.MaxAgentIDLen),
			"outcome", frame.Outcome,
			"limit", maxConcurrentAckTransitsPerUpstream,
		)
		return err
	}
	defer release()

	// 5. THE EMISSION. The frame is passed through UNTOUCHED — an intermediate
	//    re-signs nothing, re-classifies nothing and re-times nothing (§9.4) —
	//    and the ADDRESS comes from the peer registry, which is the only source
	//    of one on this path.
	//
	//    RETURNED UNWRAPPED-THROUGH so *relay.PeerRefusedError survives as
	//    itself and a caller can still ask Retriable(). The peer-surface caller
	//    needs exactly that to tell "the origin is busy" (re-offer later) from
	//    "the origin has FINALLY refused" (stop), and re-wrapping it here would
	//    make that answer come from error text instead of from a status code.
	//
	//    IT IS BOUNDED, AND THE DEADLINE IS DERIVED FROM THE INBOUND CONTEXT.
	//    WithTimeout over ctx KEEPS the caller's cancellation: a recipient or a
	//    downstream peer that goes away still cancels the outbound hop at once,
	//    and ackTransitTimeout is only the CEILING on how long this bus waits
	//    when nobody goes away — it is not a floor and it never extends an
	//    inbound deadline that is already shorter. See ackTransitTimeout for why
	//    an unbounded hop here would deny a neighbour its MESSAGE ingest.
	ctx, cancel := context.WithTimeout(ctx, ackTransitTimeout)
	defer cancel()

	_, err = a.prop.Propagate(ctx, upstream, frame)
	return err
}

// elideAckTransit renders an id for a log line without letting the party that
// chose the bytes choose the size of the line written about it.
//
// It is elidePeerClaim's discipline (relaywiring.go) with the maximum passed in,
// because this path carries TWO different ids with two different maxima. It
// CLAMPS NOTHING LEGITIMATE: a value within its own id's maximum is printed
// whole, and only an oversized one is replaced by its length — the
// AckRefusalLogFields rule, which exists because eliding at one shared width
// silently truncated correlation keys that were perfectly legal.
//
// Both call sites hand it values that are already bounded — the agent surface's
// key matched a retained message and its recipient IS the authenticated
// principal, and the peer surface's ids have both been through
// AuthorizePeerAck's validation — so this is a belt to those braces, on a method
// whose signature cannot enforce either.
func elideAckTransit(s string, max int) string {
	if len(s) > max {
		return fmt.Sprintf("a %d-byte value, which is not echoed here because it exceeds the %d-byte maximum for this id", len(s), max)
	}
	return s
}
