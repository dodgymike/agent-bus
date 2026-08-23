package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// ONE BACKWARD HOP OF THE ACKNOWLEDGEMENT PLANE (ACK-5).
//
// ackframe.go owns the frame and ackhttp.go owns the route that RECEIVES one.
// This file owns the other direction: given a terminal outcome this bus has
// just accepted, WHO — if anybody — does it go to next, and the emission that
// sends it there.
//
// It is deliberately two halves that do not know about each other's inputs:
//
//   - DisposeAck / UpstreamHop are PURE DECISION over this bus's own id, the
//     correlation key and the path this bus itself stored. No I/O, no clock, no
//     registry, so a three-bus test can drive every refusal without a server.
//   - BackPropagator is EMISSION ONLY. It never decides where a frame goes; it
//     is TOLD the upstream bus id and resolves that id's ADDRESS through this
//     bus's own peer registry.
//
// # THE SHAPE OF THE WHOLE MECHANISM, SO NOBODY REINVENTS A BROADCAST
//
// A terminal outcome travels BACKWARDS ONE HOP AT A TIME along the traversed
// bus path and STOPS AT THE ORIGIN BUS. In A→B→C, C's terminal outcome goes to
// B, and B's copy of it goes to A, and A — which minted the correlation key —
// keeps it. Each hop is re-authenticated by the receiving side (ACK-CONTRACT.md
// §6.1, the TLS client certificate) and re-bound by it (§6.2, the outbox
// obligation). Nothing here fans out, and nothing here skips a hop to reach the
// origin directly: a bus contacts only the bus that handed it the message.
//
// # THIS FILE INVENTS NO IDENTIFIER (invariant 1, §3)
//
// Correlation is by ACK-CONTRACT.md §3's correlation key — the ORIGIN bus's
// server-minted message id, `<origin-bus-id>-<seq>`, parsed with
// ids.ParseMessageID. That value is already the relay wire idempotency key and
// already OutboxRecord.OriginMessageID; a fourth axis on the same object would
// have to be minted by some bus and learned by every other, which is exactly
// the job this one already does. The bus half of it is what tells this bus
// whether it is the origin, and that is the ONLY thing this file reads out of
// it — no ordering, no cursor, no retention decision (§3).
//
// # §8.4 AT THE CORRELATION LAYER: HOP RECEIPT NEVER CONVERTS TO DELIVERY
//
// The 200 a peer gives us for an ACK settles OUR HOP OBLIGATION to that peer
// and moves no sender-visible state anywhere. What travels here is the terminal
// frame itself — the recipient's own outcome, or a bus-emitted routing failure
// — forwarded VERBATIM, and it is the only thing that may move the origin's
// row. An implementation that let the last hop's 200 stand in for the
// recipient's terminal has re-created the precise conflation ACK-5 exists to
// prevent, at distance.
//
// # THE FRAME CARRIES NO BODY AND MUST NEVER GROW ONE (invariant 6)
//
// PeerAckRequest is ids, a closed-enum class, a clock and an opaque signature.
// The durable trail records metadata and routing only, so a "reason", a
// "detail" or an excerpt of the message would be a body by another name — and
// the back-propagation path is where such a field would look most useful and do
// the most damage, because it would be copied verbatim across every remaining
// hop.

// Back-propagation failures. Both are checkable with errors.Is, and callers
// MUST switch on identity rather than on text: these messages are written for
// an operator and will be reworded.
//
// They are built with newAckError so the ACK plane keeps ONE error family
// (ack.go's ErrAckNotBound, ErrInvalidAckFrame, ErrAckOutcomeConflict) rather
// than growing a second one for its egress half.
//
// NEITHER OF THESE EVER TRAVELS TO A PEER. They are decisions this bus makes
// about its OWN next contact, so there is no status code to map and no oracle
// to worry about — unlike ErrAckNotBound, whose uniformity IS its security
// property. ErrorCode deliberately does not name them.
var (
	// ErrNoUpstreamHop reports that no upstream bus could be derived, or that
	// the one derived is not a bus this ACK may be sent to.
	//
	// It is the "our own state does not support this hop" family: a path that
	// does not end at us, a path naming us twice, a hop that is not a valid bus
	// id, a one-hop path, or an upstream that IS us. Every one of those is a
	// fault in THIS bus's wiring or in a record it stored — never a statement
	// about the neighbour's availability, which is what keeps it distinct from
	// ErrUpstreamNotPeered below.
	ErrNoUpstreamHop = newAckError("relay: no upstream hop to propagate this acknowledgement to")

	// ErrUpstreamNotPeered reports that the upstream bus is real and correctly
	// derived, but this bus has no peer registry entry for it, so there is no
	// address to send to.
	//
	// IT IS A SEPARATE SENTINEL FROM ErrNoUpstreamHop ON PURPOSE. The two have
	// different operators and different remedies: this one means a neighbour
	// was de-peered (or never had its base URL configured) while messages it
	// relayed to us are still settling, which is an ordinary operational event
	// and is fixed by re-peering. Folding them together would send an operator
	// hunting a path-fabrication bug over a peer they removed on purpose.
	//
	// THERE IS NO FALLBACK BEHIND IT. A bus contacts only a bus it is peered
	// with (§9.4), so "not peered" is the end of the road for this hop — never
	// a reason to look for another route to the origin.
	ErrUpstreamNotPeered = newAckError("relay: the upstream bus on this message's stored path is not a peer of this bus")
)

// ---------------------------------------------------------------------------
// The decision
// ---------------------------------------------------------------------------

// AckDisposition is what THIS bus does with a terminal outcome it has accepted:
// keep it, or hand it one hop further back.
//
// It is a CLOSED enum with no zero member, for the reason ack.go gives for
// AckOutcome and AckClass: a zero value is what an unpopulated struct carries,
// and if zero meant "stop at the origin" then every uninitialised disposition
// would silently swallow a terminal outcome — the least detectable failure this
// plane could have, because nothing errors and the sender simply never learns.
type AckDisposition uint8

const (
	// AckStopAtOrigin means this bus minted the correlation key, so the durable
	// sender-visible row lives HERE and nothing is forwarded.
	//
	// It is not "we could not find an upstream". It is the terminating
	// condition of the whole mechanism.
	AckStopAtOrigin AckDisposition = iota + 1

	// AckForwardUpstream means this bus is an intermediate (or the terminal bus
	// that produced the outcome) and the frame goes back exactly ONE hop, to
	// the bus named alongside this value.
	AckForwardUpstream

	// ackDispositionCount bounds the enum so a test can walk every member and
	// assert the set is closed, exactly as ackAttestationCount does in ack.go.
	// It is never a disposition.
	ackDispositionCount
)

// String renders the disposition for a log line. An out-of-range value renders
// as its number rather than as a plausible member, matching AckOutcome.String:
// a bogus value must look bogus in the log, not like a decision somebody made.
func (d AckDisposition) String() string {
	switch d {
	case AckStopAtOrigin:
		return "stop_at_origin"
	case AckForwardUpstream:
		return "forward_upstream"
	default:
		return fmt.Sprintf("AckDisposition(%d)", uint8(d))
	}
}

// DisposeAck decides what THIS bus does with a terminal outcome it has just
// accepted, returning the disposition and — for AckForwardUpstream — the bus id
// of the single hop it goes to next.
//
// storedBusPath is store.Message.BusPath: ORIGIN-FIRST and INCLUDING THIS BUS
// AS THE FINAL HOP, which is what hub.relayedBusPath writes (it appends the
// receiving bus after validating what arrived). It is NOT the path as it
// arrived on the wire. Handing the wire path here would make the last hop the
// bus that sent it to us, and this function would then propagate an ACK to
// whichever bus a peer named — the steering hole UpstreamHop's doc describes.
//
// # THE ORDER OF THE THREE STEPS IS THE DESIGN
//
//  1. OUR OWN BUS ID IS VALIDATED FIRST. An empty or malformed localBusID would
//     compare unequal to the origin half of every correlation key, so a bus
//     with a broken id would classify ITS OWN acknowledgements as "forward
//     upstream" and emit the origin's private settlements onto the network.
//     Failing closed on our own id costs one call and removes that entirely.
//
//  2. THE CORRELATION KEY IS PARSED, NEVER TRUSTED. It is peer- or
//     agent-supplied input to be validated (invariant 1). ids.ParseMessageID is
//     the single definition of "well-formed message id" — it checks the length
//     BEFORE it echoes anything, so a refusal here cannot be made large by the
//     party that chose the bytes — and the bus half it returns is the ORIGIN
//     bus by construction (`<origin-bus-id>-<seq>`, invariants 1 and 2).
//
//  3. IF WE ARE THE ORIGIN, THE PATH IS NOT CONSULTED AT ALL. This is §8.4's
//     rule at the correlation layer and it is what makes a terminal outcome
//     incapable of orbiting the federation: the one bus that could turn an
//     inbound ACK back into an outbound one is the bus that minted the key, and
//     it never does. Consulting the path first — say, "forward unless the
//     upstream is missing" — would leave the stop condition dependent on a
//     stored field, and a wrong or malicious path would restore the loop.
//     Idempotency (§12) would absorb the duplicate settlements, but the TRAFFIC
//     is unbounded, which is the same distinction path.go draws for message
//     loop prevention: an availability mechanism complementing idempotency,
//     never substituting for it (invariant 10).
//
// # BUS IDS ARE COMPARED CASE-INSENSITIVELY
//
// strings.EqualFold, because that is what this repository already does for bus
// ids everywhere the comparison is load-bearing: hub.relayedBusPath folds when
// testing whether the arriving path already names this bus, and relay's
// PathContains folds for the same reason its doc gives — ids.BusIDPattern
// admits both cases, so "BUS-X" and "bus-x" are two spellings of one operator-
// visible identity. A case-SENSITIVE test here would let a single flipped
// character make the origin fail to recognise its own key and forward the
// settlement back out, which is precisely the loop step 3 exists to forbid.
//
// The correlation key's bus half comes from ids.ParseMessageID and localBusID
// from this bus's configuration; folding is therefore strictly a widening of
// what counts as "us", which is the safe direction for a stop condition.
func DisposeAck(localBusID, correlationKey string, storedBusPath []string) (AckDisposition, string, error) {
	if err := ids.ValidateBusID(localBusID); err != nil {
		// OUR id, so this is a fault on THIS bus rather than anything a remote
		// party did — the same posture AppendHop takes on the same value, and
		// for the same reason: a bad local id makes every comparison below
		// vacuous rather than false.
		return 0, "", fmt.Errorf("relay: this bus's own id is not valid, so no acknowledgement can be disposed against it: %w", err)
	}

	originBusID, _, err := ids.ParseMessageID(correlationKey)
	if err != nil {
		// Wrapped as an invalid FRAME rather than as a missing hop: the fault
		// is in the value we were handed, it is decidable by whoever produced
		// it without asking us, and ack.go already owns that meaning.
		return 0, "", fmt.Errorf("%w: the correlation key is not a well-formed message id, so the origin bus it names cannot be established: %v", ErrInvalidAckFrame, err)
	}

	if strings.EqualFold(originBusID, localBusID) {
		// THE TERMINATING CONDITION. The path is deliberately not looked at.
		return AckStopAtOrigin, "", nil
	}

	hop, err := UpstreamHop(localBusID, storedBusPath)
	if err != nil {
		return 0, "", err
	}
	return AckForwardUpstream, hop, nil
}

// UpstreamHop returns the bus that handed this message to us: the hop
// immediately before ours in the STORED path.
//
// The stored path is origin-first and ends with THIS bus (hub.relayedBusPath),
// so the answer is always index len-2 and never a search.
//
// # WHY IT IS AN INDEX AND NOT A SEARCH — READ THIS BEFORE "MAKING IT ROBUST"
//
// The obvious-looking generalisation is to find our own id ANYWHERE in the path
// and take the hop before it. That would let ANY position in a peer-supplied
// path decide who we POST a terminal outcome to: the stored path is derived from
// bytes a peer sent (hub.relayedBusPath validates and appends, but the prefix is
// the peer's), so a peer that puts our id in the middle of a path it fabricates
// would pick a bus that was never on the route, on its own say-so. Requiring the
// path to END at us means the only hop we will ever contact is the one adjacent
// to a position WE wrote — and BackPropagator.Propagate then contacts it ONLY if
// it is in THIS bus's own peer registry with a base URL, so a fabricated prefix
// can never name an address, a host or a scheme, and an unpeered id means
// nothing is dialled at all. When the shape is not what we wrote, the honest
// answer is to refuse, not to guess: refusing costs one settlement that an
// operator can see in the log, and guessing costs a contact the graph never
// authorised.
//
// # WHAT THIS DOES NOT DO — CORRECTED 2026-08-21 (ACK-5), READ BEFORE CITING IT
//
// This comment used to say the end-at-us rule is "what would let a peer-supplied
// path steer this bus's onward contact" and stop there. IT DOES NOT PROVE THE
// LAST HOP OF THE ARRIVING PATH IS THE PEER THAT AUTHENTICATED.
// hub.relayedBusPath checks that the arriving path is non-empty, is within
// store.MaxReceivedBusPath, that every hop passes ids.ValidateBusID and that the
// receiving bus is not already on it — then appends that bus.
// hub.RelayedIngestRequest has NO peer-principal field at all, so NOTHING binds
// received[len-1] to the authenticated peer: an authenticated peer can still
// place a DIFFERENT bus it knows we peer with immediately before us and have the
// settlement delivered there. That gap is tracked as ACK-5-FU-BUSPATH-SENDER and
// is NOT closed by the rule above.
//
// The residual is bounded at the FAR end rather than here, and this complements
// — never replaces — the receiving side's own checks. §6.2's obligation binding
// means the upstream bus will independently refuse an ACK it was never owed, and
// §12's idempotency absorbs a repeat. Loop prevention over the path is a
// COMPLEMENT to idempotency, never a substitute (invariant 10).
//
// # EVERY REFUSAL WRAPS ErrNoUpstreamHop AND NAMES AN INDEX, NEVER A VALUE
//
// A hop that failed validation is unbounded input, and ids.ValidateBusID quotes
// what it rejects with %q, which expands a control byte to four characters. So
// the refusals below report the INDEX and the LENGTH — the convention
// hub.relayedBusPath and validateHops already follow — and the oversize case is
// caught BEFORE ids.ValidateBusID ever sees the value.
func UpstreamHop(localBusID string, storedBusPath []string) (string, error) {
	if err := ids.ValidateBusID(localBusID); err != nil {
		// Re-checked here as well as in DisposeAck because this function is
		// exported and callable on its own; every comparison below measures a
		// hop against this string, so a bad one makes all of them meaningless.
		return "", fmt.Errorf("%w: this bus's own id is not valid, so no hop on the stored path can be measured against it: %v", ErrNoUpstreamHop, err)
	}

	// 1. FEWER THAN TWO HOPS. A one-hop stored path is `[us]`, which says the
	//    message ORIGINATED here — and an origin never forwards a terminal
	//    outcome (§8.4), so DisposeAck's origin test should already have
	//    returned AckStopAtOrigin. Reaching here means the correlation key and
	//    the stored path disagree about where the message came from, and the
	//    safe reading of that disagreement is "nowhere to send it".
	if len(storedBusPath) < 2 {
		return "", fmt.Errorf("%w: the stored path has %d hop(s), so there is no hop before ours; a path this short belongs to a message that originated on this bus, which the correlation key did not say", ErrNoUpstreamHop, len(storedBusPath))
	}
	last := len(storedBusPath) - 1

	// 2. EVERY HOP MUST BE A VALID BUS ID, checked before any of them is
	//    compared. A path holding one malformed entry is not a path we can make
	//    a routing decision from, wherever the malformed entry sits.
	for i, hop := range storedBusPath {
		if len(hop) > MaxPeerBusIDLen {
			return "", fmt.Errorf("%w: hop %d of the stored path is %d bytes, but a bus id is at most %d; the hop is not echoed here because it is oversized", ErrNoUpstreamHop, i, len(hop), MaxPeerBusIDLen)
		}
		if err := ids.ValidateBusID(hop); err != nil {
			return "", fmt.Errorf("%w: hop %d of the stored path is not a valid bus id: %v", ErrNoUpstreamHop, i, err)
		}
	}

	// 3. THE PATH MUST END AT US. See the doc above: this is the check that
	//    stops a peer-supplied path from choosing who we contact.
	if !strings.EqualFold(storedBusPath[last], localBusID) {
		return "", fmt.Errorf("%w: the %d-hop stored path does not end at this bus — its final hop is some other bus (hop %d, not echoed) — so the hop before it is not the bus that handed this message to us; refused rather than searched for our id elsewhere on the path, because that search is what would let a peer-supplied path choose who this bus contacts", ErrNoUpstreamHop, len(storedBusPath), last)
	}

	// 4. WE MUST APPEAR EXACTLY ONCE. A second occurrence is a fabricated
	//    second visit — the identical rule hub.relayedBusPath enforces on
	//    ingest and AppendHop enforces on egress, applied here to the record
	//    that survived them, and it is invariant 10's loop-prevention
	//    complement rather than a duplicate of either.
	for i, hop := range storedBusPath[:last] {
		if strings.EqualFold(hop, localBusID) {
			return "", fmt.Errorf("%w: this bus appears at hop %d of the %d-hop stored path as well as at its end, so the path claims two visits; a path from which no further routing decision can be trusted is refused rather than resolved", ErrNoUpstreamHop, i, len(storedBusPath))
		}
	}

	upstream := storedBusPath[last-1]

	// 5. THE RESOLVED HOP MUST NOT BE US. UNREACHABLE after step 4, which has
	//    already refused our id at every index below `last` — including
	//    `last-1`. It is kept because it is the ONE assertion that states the
	//    post-condition this function exists to guarantee ("never ourselves"),
	//    and a future edit that relaxes step 4 into a search would otherwise
	//    turn a fabricated adjacent pair into a self-POST with nothing left to
	//    catch it. It costs one comparison.
	if strings.EqualFold(upstream, localBusID) {
		return "", fmt.Errorf("%w: the hop before ours on the %d-hop stored path is this bus again, so the path names us twice adjacently and there is no upstream to propagate to", ErrNoUpstreamHop, len(storedBusPath))
	}
	return upstream, nil
}

// ---------------------------------------------------------------------------
// The emission
// ---------------------------------------------------------------------------

// AckSender is the one method BackPropagator needs from *Client.
//
// It is an interface rather than a *Client so a three-bus test can drive every
// path in this file — the forward, the de-peered neighbour, the refusal — with
// no listener, no certificates and no clock. *Client satisfies it as written;
// this declaration adds no behaviour and must not grow any, because the mutual
// TLS that authenticates the far end (invariant 11) lives in *Client's
// transport and a second implementation in production would be a second, weaker
// answer to "who did we just talk to".
type AckSender interface {
	PeerAck(ctx context.Context, peerBaseURL string, req PeerAckRequest) (PeerAckResponse, error)
}

// BackPropagatorStats is the observable state of one BackPropagator.
//
// Failed counts EVERY emission that did not forward; NotPeered counts the
// de-peered-neighbour subset of that. They overlap DELIBERATELY, the same way
// AckStats.Refused and AckStats.Conflicts do and for the same reason: Failed is
// "how many terminal outcomes did not move" and NotPeered is "how much of that
// was topology rather than a broken peer". Subtracting gives the rest, whereas
// making them disjoint would mean an operator watching Failed alone misses the
// case with the clearest remedy.
type BackPropagatorStats struct {
	// Forwarded counts terminal outcomes this bus successfully handed one hop
	// back — the peer answered 200 — including its idempotent replays.
	Forwarded uint64

	// NotPeered counts refusals because the upstream bus has no registry entry.
	NotPeered uint64

	// Failed counts every emission that did not forward, this subset included.
	Failed uint64
}

// BackPropagatorConfig configures the emitting half.
type BackPropagatorConfig struct {
	// BusID is THIS bus's server-minted id (invariant 1). It is used to refuse
	// a self-directed hop and to label the operator log; it never travels on
	// the frame, which has no bus-id field at all (ackframe.go).
	//
	// Required and validated at construction.
	BusID string

	// Sender performs the POST. In production it is *Client, whose transport
	// carries this bus's mutual-TLS configuration.
	//
	// Required. A nil sender would make a BackPropagator that silently
	// discarded every terminal outcome, which from the outside is
	// indistinguishable from a federation that is simply quiet.
	Sender AckSender

	// PeerBaseURL resolves an upstream BUS ID to its ADDRESS. Pass
	// Registry.PeerBaseURL as the METHOD VALUE — its signature is exactly this
	// — rather than hand-writing a closure over the registry's internals, which
	// is the defect that method was added to fix (see its doc: every wiring
	// site was doing that, and each was a fresh chance to get the locking
	// wrong). It is safe for concurrent use.
	//
	// THIS FUNCTION IS THE ONLY SOURCE OF AN ADDRESS ON THIS PATH. See
	// Propagate.
	//
	// Required.
	PeerBaseURL func(busID string) (string, bool)

	// Logger receives the refusals. Optional; nil discards.
	Logger *logging.Logger
}

// BackPropagator sends ONE terminal outcome ONE hop back, to a bus it has been
// told the ID of and looks the ADDRESS of up itself.
//
// It makes no routing decision — DisposeAck does that — and it retries nothing;
// see Propagate.
type BackPropagator struct {
	busID       string
	sender      AckSender
	peerBaseURL func(string) (string, bool)
	log         *logging.Logger

	forwarded atomic.Uint64
	notPeered atomic.Uint64
	failed    atomic.Uint64
}

// NewBackPropagator validates cfg and returns the emitting half.
//
// Every field is required and each refusal names the missing piece, in the
// style of NewAckHandler and NewForwarder: a startup failure that says which
// dependency is absent beats a peer's unexplained silence hours later, and on
// this path the silent version is the worse failure — a mis-wired
// BackPropagator loses terminal outcomes without ever producing an error a
// sender or an operator would see.
func NewBackPropagator(cfg BackPropagatorConfig) (*BackPropagator, error) {
	if err := ids.ValidateBusID(cfg.BusID); err != nil {
		return nil, fmt.Errorf("relay: back-propagator bus id: %w", err)
	}
	if cfg.Sender == nil {
		return nil, errors.New("relay: BackPropagatorConfig.Sender is required; without it every terminal outcome this bus accepted would be dropped on the way back to the origin, which is indistinguishable from a federation with nothing to say")
	}
	if cfg.PeerBaseURL == nil {
		return nil, errors.New("relay: BackPropagatorConfig.PeerBaseURL is required; it is the ONLY source of a peer address on this path, and a back-propagator without one could only get an address from the frame — which is the SSRF the peer surface exists to refuse")
	}
	log := cfg.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &BackPropagator{
		busID:       cfg.BusID,
		sender:      cfg.Sender,
		peerBaseURL: cfg.PeerBaseURL,
		log:         log,
	}, nil
}

// Stats reports this back-propagator's counters. Its shape mirrors
// AckHandler.Stats so the two halves of the ACK plane read the same way.
func (p *BackPropagator) Stats() BackPropagatorStats {
	return BackPropagatorStats{
		Forwarded: p.forwarded.Load(),
		NotPeered: p.notPeered.Load(),
		Failed:    p.failed.Load(),
	}
}

// Propagate forwards ONE terminal outcome ONE hop back to upstreamBusID.
//
// It is SAFE FOR CONCURRENT USE: it holds no mutable state beyond its atomic
// counters, re-resolves the address on every call, and mutates nothing in req.
//
// # NOTHING IN THE FRAME NAMES A DESTINATION — THIS IS THE SECURITY PROPERTY
//
// upstreamBusID comes from THIS bus's OWN stored bus path (UpstreamHop), and
// its ADDRESS comes from THIS bus's OWN peer registry (PeerBaseURL). A frame
// field, a header or a query parameter must NEVER reach either decision, and
// there is deliberately nothing in PeerAckRequest one could reach them from —
// the same structural argument ackframe.go makes about the absent peer-bus
// field, and the same rule httpapi.servePeerAck states for the authenticated
// principal: IT MUST NEVER COME FROM THE REQUEST.
//
// §9.4 puts it as a rule: "No bus contacts a bus it is not peered with, and
// nothing in a peer-supplied frame ever names an address, host or scheme." This
// is the identical SSRF class the relay envelope already refuses. If a future
// task wants an ACK to reach somewhere the registry does not name, the answer
// is a peering change by an operator, never a field on the frame.
//
// # THE CLASS AND THE ATTESTATION ARE FORWARDED VERBATIM (§9.4)
//
// req is PASSED THROUGH, NOT REBUILT. An intermediate re-signs nothing,
// re-classifies nothing and re-times nothing — exactly as it re-attests nothing
// when forwarding a message. Constructing a frame here would mean choosing an
// outcome, a class, a timestamp and an attestation, and every one of those
// choices would be this bus asserting something the RECIPIENT said. In
// particular the attestation is opaque bytes over the ORIGINAL canonical ACK
// bytes: touching any covered field would silently invalidate a signature
// nobody in this federation can verify yet (§6.3, §16 Q1), so the corruption
// would be undetectable end to end.
//
// THE ONE FIELD THAT IS NOT PRESERVED IS ProtocolVersion, AND IT IS NOT SET
// HERE EITHER. Client.PeerAck overwrites it with AckWireVersion by design —
// "every frame this bus emits declares the version this bus speaks", which is
// what makes an ABSENT version unambiguously mean "written before the field
// existed". A second assignment here would be a second place to update at the
// next version bump, and the two would eventually disagree.
//
// # THIS TASK DOES NOT RETRY, AND MUST NOT LEARN TO
//
// On failure the error is returned as it came, so the caller can classify it —
// *PeerRefusedError already answers Retriable(), splitting "not now" (5xx, 429,
// 408) from "never" (every other 4xx). Retry, backoff and the bounce path are
// ACK-7 and ACK-14 and belong THERE, once, beside the existing outbox retry
// horizon. A loop added here would be a second retry policy with no durable
// record behind it: it would survive neither a restart nor a crash, and it
// would race the real one the moment ACK-7 lands.
//
// Note the rollout ordering Client.PeerAck documents: a peer running a binary
// from before ACK-3 does not serve PeerAckPath and answers 404, which is FINAL.
// Receivers must be upgraded before senders.
func (p *BackPropagator) Propagate(ctx context.Context, upstreamBusID string, req PeerAckRequest) (PeerAckResponse, error) {
	// A malformed upstream id is a WIRING fault — the caller derived it from
	// somewhere other than UpstreamHop, which validates every hop — so it wraps
	// ErrNoUpstreamHop rather than ErrUpstreamNotPeered. The distinction is the
	// operator's: ErrUpstreamNotPeered means "re-peer that bus", and answering
	// it here would send somebody to the registry over a bug in the caller.
	// The id is not echoed; it has just failed validation.
	if err := ids.ValidateBusID(upstreamBusID); err != nil {
		wrapped := fmt.Errorf("%w: the upstream bus id handed to Propagate is not a valid bus id (%d bytes, not echoed): %v", ErrNoUpstreamHop, len(upstreamBusID), err)
		p.fail("acknowledgement NOT propagated: the upstream bus id is not a valid bus id, so no peer could be resolved for it. This is a fault in the caller that derived it, not a de-peered neighbour",
			"local_bus", p.busID,
			"upstream_bus_bytes", len(upstreamBusID),
			"correlation_key", elideAck(req.CorrelationKey),
			"error", wrapped.Error(),
		)
		return PeerAckResponse{}, wrapped
	}

	// NEVER POST AN ACKNOWLEDGEMENT TO OURSELVES. UpstreamHop already refuses
	// it, so reaching here means the id came from somewhere else; it wraps
	// ErrNoUpstreamHop for the same reason as above. Left in because the
	// failure it prevents is nasty and quiet: this bus would authenticate as
	// its own peer, settle its own obligation, and — depending on wiring —
	// dispose the result again, which is the one shape §8.4's stop rule cannot
	// see, because the correlation key never changes.
	if strings.EqualFold(upstreamBusID, p.busID) {
		wrapped := fmt.Errorf("%w: the upstream bus is this bus, and a bus never acknowledges to itself", ErrNoUpstreamHop)
		p.fail("acknowledgement NOT propagated: the upstream hop resolved to THIS bus. A bus never POSTs an acknowledgement to itself; this is a wiring fault, not a topology one",
			"local_bus", p.busID,
			"upstream_bus", elideAck(upstreamBusID),
			"correlation_key", elideAck(req.CorrelationKey),
		)
		return PeerAckResponse{}, wrapped
	}

	// THE ONE PLACE AN ADDRESS COMES FROM. Re-resolved on every call rather
	// than frozen, exactly as forward.go re-resolves per attempt, so a
	// de-peering or an address move takes effect on the NEXT emission instead
	// of being invisible for the rest of the settlement's life.
	baseURL, ok := p.peerBaseURL(upstreamBusID)
	if !ok {
		p.notPeered.Add(1)
		// LOUD AND SPECIFIC (invariant 6's posture): a terminal outcome is
		// being dropped, and a drop nobody can diagnose is the actual defect.
		// The line names the local bus, the upstream bus and the correlation
		// key so an operator can tell WHICH settlement stopped and WHERE.
		// NOTHING IS DIALLED — there is no fallback address to try, because a
		// bus contacts only a bus it is peered with (§9.4).
		p.fail("acknowledgement NOT propagated: the upstream bus on this message's stored path is NOT a peer of this bus, so there is no address to send to and NOTHING was dialled. The terminal outcome stops here and the origin will not learn it from this hop — re-peer that bus (or configure its base URL) if this is not intended",
			"local_bus", p.busID,
			"upstream_bus", elideAck(upstreamBusID),
			"correlation_key", elideAck(req.CorrelationKey),
			"recipient", elideAck(req.Recipient),
			"outcome", elideAck(req.Outcome),
			"class", elideAck(req.Class),
		)
		return PeerAckResponse{}, fmt.Errorf("%w: bus %q is not in this bus's peer registry, or has no configured base URL", ErrUpstreamNotPeered, elideAck(upstreamBusID))
	}

	// req IS PASSED THROUGH UNTOUCHED. See "forwarded verbatim" above.
	resp, err := p.sender.PeerAck(ctx, baseURL, req)
	if err != nil {
		// The ENDPOINT is not logged as a separate field: it is inside
		// *PeerRefusedError's message when the far end answered at all, and the
		// upstream bus id is the field an operator correlates on. The error
		// text may quote the URL WE resolved, which is ours, never a peer's.
		p.fail("acknowledgement NOT propagated: the upstream peer did not accept it. NOT retried here — retry and bounce are ACK-7/ACK-14 — and nothing durable changed on this bus as a result",
			"local_bus", p.busID,
			"upstream_bus", elideAck(upstreamBusID),
			"correlation_key", elideAck(req.CorrelationKey),
			"recipient", elideAck(req.Recipient),
			"outcome", elideAck(req.Outcome),
			"class", elideAck(req.Class),
			"error", err.Error(),
		)
		// UNWRAPPED-THROUGH, so *PeerRefusedError survives as itself and the
		// caller can ask Retriable() rather than re-deriving it from text.
		return PeerAckResponse{}, err
	}

	p.forwarded.Add(1)
	// A DUPLICATE IS A SUCCESS, not a separate counter: invariant 10's first
	// case says the original result stands and nothing is re-applied, so the
	// terminal outcome IS at the upstream bus either way — which is the only
	// thing Forwarded claims.
	p.log.Info("acknowledgement propagated one hop back",
		"local_bus", p.busID,
		"upstream_bus", elideAck(upstreamBusID),
		"correlation_key", elideAck(req.CorrelationKey),
		"recipient", elideAck(req.Recipient),
		"outcome", elideAck(req.Outcome),
		"class", elideAck(req.Class),
		"accepted", resp.Accepted,
		"duplicate", resp.Duplicate,
	)
	return resp, nil
}

// fail is the ONE place Failed is incremented, so a path that increments its own
// specific counter and then fails cannot double-count. It is AckHandler.fail's
// discipline, minus the wire response: nothing here answers a request.
//
// Every refusal on this path is a Warn rather than an Error, and deliberately:
// a terminal outcome stopping one hop early is a real loss of information to
// the sender, but this bus is intact and every other settlement is unaffected —
// the same judgement hub.IngestRelayed records for an idempotency violation.
//
// EVERY UNTRUSTED VALUE ITS CALLERS PASS GOES THROUGH elideAck. This function
// forwards a frame VERBATIM and therefore does not re-validate it, so as far as
// it knows every string in req is bytes a remote party chose; elideAck is this
// package's single truncation point, the one AckRefusalLogFields already uses.
// The known cost is that a legitimate correlation key longer than
// maxElidedAckChars is shown truncated — accepted there for the same reason, and
// preferable to letting a remote party size our log line.
func (p *BackPropagator) fail(msg string, kv ...interface{}) {
	p.failed.Add(1)
	p.log.Warn(msg, kv...)
}

// ---------------------------------------------------------------------------
// The frame, rebuilt for the next hop
// ---------------------------------------------------------------------------

// AckFrameFrom rebuilds the peer ACK wire frame from an ALREADY-VALIDATED
// acknowledgement, so a bus that owes one more backward hop can hand it to
// Propagate.
//
// It belongs to neither half of this file: it is pure, does no I/O and makes no
// routing decision. It sits BETWEEN them, because both call sites of the
// back-propagation path — the agent surface (a local recipient acknowledging a
// message that was RELAYED here) and the peer surface (a downstream peer
// propagating an outcome for a key this bus did not originate) — hold a
// ValidatedPeerAck and need the frame that goes onward.
//
// # THIS IS THE ONE SPELLING OF "FORWARD VERBATIM" (§9.4, invariant 2)
//
// The recipient's fields are reproduced EXACTLY and the signature is passed
// through untouched: an intermediate re-signs nothing, re-classifies nothing
// and re-times nothing, exactly as it re-attests nothing when forwarding a
// message. The attestation is opaque bytes over the ORIGINAL canonical ACK
// bytes, so touching any covered field would silently invalidate a signature
// nobody in this federation can verify yet (§6.3, §16 Q1) — the corruption
// would be undetectable end to end. EmittedAtUnixMilli in particular is the
// EMITTER's clock and stays the emitter's clock; restamping it here would make
// every hop look like the origin of the outcome in an operator's log.
//
// # WHY IT REBUILDS FROM THE VALIDATED STRUCT RATHER THAN PASSING THE RAW FRAME
//
// Deliberate, and it is a containment property rather than a style choice. A
// PeerAckRequest as decoded is bytes a remote party chose; a ValidatedPeerAck is
// what survived ValidatePeerAckRequest's closed-set validation — the version is
// one we speak, the outcome and class are members of the closed sets and agree
// with each other, and the attestation has the shape the outcome requires. By
// building the onward frame from the VALIDATED value, only fields that passed
// that gate can ever be forwarded, so this bus cannot launder an unvalidated
// byte string onward under its own TLS identity. Threading the decoded frame
// through instead would compile, behave identically on every legal input, and
// quietly make this bus a relay for whatever a peer sent.
//
// # ProtocolVersion IS DELIBERATELY LEFT ZERO
//
// Client.PeerAck overwrites it with AckWireVersion, because "every frame this
// bus emits declares the version this bus speaks" — which is what makes an
// ABSENT version unambiguously mean "written before the field existed". A second
// assignment here would be a second place to update at the next version bump,
// and the two would eventually disagree. Do not add one; do not copy
// v.ProtocolVersion, which is what the PREVIOUS hop spoke.
//
// # THE TWO ABSENCES THAT MUST STAY ABSENT
//
//   - Class is carried by a DIRECT CONVERSION, string(v.Class), and it must
//     never become v.Class.String(). CORRECTED 2026-08-21 (ACK-5): this bullet
//     described a uint8 enum stringifying to "AckClass(0)" and the guard
//     `if v.Class != 0` that used to sit at the assignment, and ACK-13 removed
//     both — AckClass is now a true ALIAS for the STRING type ack.Class, whose
//     "no class" value is the empty string (relay's ackNoClass). The hazard the
//     bullet named is LIVE in the new shape and is worse: ack.Class.String
//     REPORTS on a non-member rather than echoing it, so Class("").String()
//     returns "invalid-class(0 bytes)" — measured, not assumed. That is
//     NON-EMPTY, so `omitempty` would NOT drop it, and every POSITIVE terminal
//     (§5.4: it carries no class at all) would go onward carrying a class the
//     next hop answers 400 to, which is FINAL, and the outcome would be lost
//     rather than retried. See the note at the assignment below.
//   - Attestation is set only when there are signature bytes. A bus-sourced
//     `undeliverable` carries none, and ValidateAckAttestation refuses one that
//     does; `omitempty` on the POINTER is what makes absent mean absent rather
//     than "present and empty".
//
// The signature is COPIED, never aliased, so a forwarded frame cannot be mutated
// through a ValidatedPeerAck the caller still holds — the same no-aliasing
// discipline store.RelayProvenance keeps on its slices, and for the same reason:
// the failure is silent right up to the moment something mutates it.
func AckFrameFrom(v ValidatedPeerAck) PeerAckRequest {
	frame := PeerAckRequest{
		CorrelationKey:     v.CorrelationKey,
		Recipient:          v.Recipient,
		Outcome:            v.Outcome.String(),
		EmittedAtUnixMilli: v.EmittedAtUnixMilli,
	}
	// A DIRECT CONVERSION, AND THE BRANCH THAT USED TO GUARD IT IS GONE (ACK-13,
	// 2026-08-21). AckClass was a uint8 enum whose zero value meant "no class",
	// so this read `if v.Class != 0 { frame.Class = v.Class.String() }`. ACK-13
	// collapsed the twice-declared vocabulary onto internal/ack and AckClass is
	// now a TRUE ALIAS for ack.Class, which is a STRING whose "no class" value is
	// the empty string (relay's ackNoClass). The conversion therefore carries the
	// absent case correctly on its own, and `omitempty` on the frame field is
	// what keeps an absent class absent on the wire (§5.4: a positive terminal
	// has nothing to explain and carries no class at all).
	//
	// The old branch is described rather than deleted silently because it did not
	// merely become redundant — it stopped COMPILING, which is the useful kind of
	// breakage. A version of this that kept a `!= ""` guard would look like it
	// was still enforcing something; it would not be.
	frame.Class = string(v.Class)
	if len(v.Signature) > 0 {
		frame.Attestation = &AckAttestationEnvelope{Signature: append([]byte(nil), v.Signature...)}
	}
	return frame
}

// ---------------------------------------------------------------------------
// The binding rule, widened by ONE case for the transit hop (§6.2, ACK-5)
// ---------------------------------------------------------------------------

// NextHopAddress resolves the base URL this bus would dial to reach busID, and
// reports whether it knows one at all.
//
// It is satisfied by Registry.PeerBaseURL, whose signature is exactly this, and
// it must be passed as the METHOD VALUE rather than as a hand-written closure
// over the registry's internals — the same requirement
// BackPropagatorConfig.PeerBaseURL and ForwarderOptions.PeerBaseURL state, for
// the same reason: the method takes the registry's RLock and is safe for
// concurrent use, and every site that rewrote it was a fresh chance to get the
// locking wrong.
//
// IT IS THIS BUS'S OWN CONFIGURATION AND NOTHING ELSE. Nothing a peer sends may
// reach it as an ADDRESS; the only peer-supplied value AuthorizePeerAckVia
// passes to it is a BUS ID it parsed out of an already-validated agent id, and
// the answer is our own operator's routing table (`peer add -url`, `-route-for`).
// It is a lookup, never a dial.
type NextHopAddress func(busID string) (string, bool)

// AuthorizePeerAckVia is AuthorizePeerAck plus the INDIRECT arm that multi-hop
// back-propagation needs.
//
// # THE DEFECT IT FIXES, MEASURED RATHER THAN HYPOTHESISED
//
// In A -> B -> C with a recipient on C and a sender on A, A writes its outbox
// obligation as DeriveJobID(C, K): Forwarder.targets keys the job on
// Registry.Route(recipient), which returns the RECIPIENT'S HOME BUS, not the
// next hop A actually dials. But the acknowledgement comes BACK over A's mutual-
// TLS link with B, so the authenticated peer is B and AuthorizePeerAck looks up
// DeriveJobID(B, K) — a job id nothing ever wrote. The two spellings COINCIDE
// ONLY ON A DIRECT PEER LINK, which is why every single-hop test was green while
// the whole multi-hop ACK path silently failed: A answered 409, B's 409-absorbing
// arm turned that into a 200 for C, and the recipient read "accepted" for an
// outcome nothing recorded anywhere — the one sentence ACK-CONTRACT.md §1.1
// exists to stop, reached on the happy path.
//
// The outbox's job id is NOT re-keyed to fix this. It is a DURABLE id: changing
// its shape would orphan every pending job across an upgrade and move the split-
// horizon and recovery logic with it. The binding rule is widened instead, by one
// case, computed entirely from state this bus already holds.
//
// # THE RULE, IN FULL
//
// A peer-hop ACK/NACK from AUTHENTICATED peer P for correlation key K naming
// recipient R is authoritative if EITHER:
//
//  1. DIRECT (§6.2 unchanged, and tried FIRST): DeriveJobID(P, K) names an
//     outbox job this bus durably wrote.
//  2. INDIRECT (new): let D be the BUS HALF of R — invariant 2 is what makes
//     this readable at all, since a fully-qualified agent id names its home bus,
//     and that is precisely why the id is namespaced. Then ALL of:
//     - D is not P (otherwise case 1 already applied) and D is not US;
//     - the address this bus would dial to reach D is the SAME address it would
//     dial to reach P, both resolved and both non-empty; AND
//     - DeriveJobID(D, K) names an outbox job this bus durably wrote.
//
// # THE THIRD CLAUSE IS THE SECURITY CORE
//
// IT IS COMPUTED FROM OUR OWN PEER CONFIGURATION, NEVER FROM ANYTHING THE FRAME
// SAID. R is peer-supplied, so it selects WHICH job we look for — but it cannot
// conjure one, and it cannot make an unrelated peer the next hop for a
// destination we route somewhere else. On a `-route-for` topology the busD route
// record literally carries the NEXT HOP's address (cmd/agent-bus/peer.go: "a
// route record's bus id is the DESTINATION bus, and its base URL is the address
// to DIAL to reach that destination"), so "PeerBaseURL(D) == PeerBaseURL(P)" is
// exactly the question "is P the hop we route D through", asked of the only party
// entitled to answer it: us. A peer naming a recipient on a bus we do NOT route
// through it finds nothing and gets the SAME uniform refusal.
//
// Both addresses must RESOLVE and both must be NON-EMPTY. Registry.PeerBaseURL
// already folds an empty base URL into "not found" for its own reasons, but this
// function re-checks it rather than relying on that: two buses that have
// handshaked and never had an address configured must not become each other's
// next hop by both being blank.
//
// # WHAT THIS BUYS
//
// On BOTH arms the recipient is now bound to the acknowledging peer. The INDIRECT
// arm binds it by routing — P must be the hop we route R's bus through. The
// DIRECT arm binds it by home bus — AuthorizePeerAck requires EqualFold(homeBus(R),
// P) (ACK-4-FU-RECIPIENT-BINDING, CLOSED 2026-08-23). Together they mean a peer
// bound for K may settle only recipients whose home bus is the one its obligation
// names, never a sibling recipient of K on another bus, which matters the moment a
// key gains a second recipient row (ACK-12-FU-DESTINATION-ROW).
//
// The still-separate second conjunct — that a row exists for a recipient the
// SENDER named — remains ACK-CONTRACT.md §8.2's "(none)" row, applied by the
// caller's SettleAck. The two are conjunctive and neither is sufficient alone.
//
// # UNIFORMITY, ORDER AND COST
//
// EVERY indirect refusal returns ErrAckNotBound, by identity, byte-identical to
// the direct arm's. The uniform-answer property that error's doc protects must
// not acquire a new distinguishable case: the widening adds a way to be BOUND,
// never a new way to be told why you were not.
//
// Any error from the direct arm that is NOT ErrAckNotBound — the nil-table wiring
// fault, and every ErrInvalidAckFrame from its id validation — is returned
// UNCHANGED and the indirect arm is never reached. AuthorizePeerAck owns that
// validation, the uniform refusal and the ErrInvalidAckFrame distinction, and a
// second copy of those rules here is exactly the drift this repo keeps paying
// for; that is why this function CALLS it instead of re-implementing it.
//
// A nil nextHop means "this build has no routing table to consult" and FAILS
// CLOSED to the direct arm's answer — which is correct for a bus with no static
// routes, and is byte-for-byte the pre-ACK-5 behaviour.
//
// The two routing lookups are done BEFORE the second obligation lookup, and the
// order is load-bearing for §16 Q3's denial-of-service note rather than for
// correctness (the answer is identical either way): NextHopAddress takes the
// registry's RLock, while AckObligations.Lookup takes the outbox's EXCLUSIVE
// mutex and runs an O(n) sweep. Checking routing first means a peer can only
// provoke a SECOND sweep for a destination this bus already routes through it.
func AuthorizePeerAckVia(obligations AckObligations, nextHop NextHopAddress, localBusID, peerBusID, correlationKey, recipient string) (OutboxRecord, error) {
	// THE DIRECT ARM FIRST, AND IT IS THE ONLY VALIDATION. Everything below
	// relies on AuthorizePeerAck having already rejected a malformed peer id,
	// correlation key or recipient with ErrInvalidAckFrame.
	rec, err := AuthorizePeerAck(obligations, localBusID, peerBusID, correlationKey, recipient)
	if err == nil || !errors.Is(err, ErrAckNotBound) {
		return rec, err
	}
	if nextHop == nil {
		return OutboxRecord{}, err
	}

	// Invariant 2: the recipient is fully qualified, so its bus half names the
	// DESTINATION bus. AuthorizePeerAck has already parsed it; a failure here is
	// unreachable, and it answers the uniform refusal rather than inventing a
	// second spelling of "malformed".
	destBusID, _, _, perr := ids.ParseAgentID(recipient)
	if perr != nil {
		return OutboxRecord{}, ErrAckNotBound
	}
	// D == P means the direct arm already asked this exact question. The
	// comparison FOLDS CASE deliberately: DeriveJobID is case-sensitive, so a D
	// that differs from P only in case would derive a SECOND job-id spelling for
	// ONE bus, which is the silent case-mismatch hazard AuthorizePeerAck's doc
	// warns about, arriving from the other direction. D == US is refused for the
	// same reason Registry.Route excludes our own bus: a recipient at home here
	// was never routed anywhere, so no obligation to any peer can bind it.
	if strings.EqualFold(destBusID, peerBusID) || strings.EqualFold(destBusID, localBusID) {
		return OutboxRecord{}, ErrAckNotBound
	}

	// THE THIRD CLAUSE. Our routing table, twice, and nothing from the frame.
	destAddr, ok := nextHop(destBusID)
	if !ok || destAddr == "" {
		return OutboxRecord{}, ErrAckNotBound
	}
	peerAddr, ok := nextHop(peerBusID)
	if !ok || peerAddr == "" {
		return OutboxRecord{}, ErrAckNotBound
	}
	if destAddr != peerAddr {
		return OutboxRecord{}, ErrAckNotBound
	}

	rec, ok = obligations.Lookup(DeriveJobID(destBusID, correlationKey))
	if !ok {
		return OutboxRecord{}, ErrAckNotBound
	}
	// The same defence in depth AuthorizePeerAck applies to its own hit: the
	// record we found must describe the obligation we asked for, or the table has
	// been spliced. Uniform refusal, so it cannot be used to probe the table.
	if rec.PeerBusID != destBusID || rec.OriginMessageID != correlationKey {
		return OutboxRecord{}, ErrAckNotBound
	}
	return rec, nil
}
