package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// ErrUnknownLocalRecipient is the refusal a relayed message earns when it names
// an agent in THIS bus's namespace that THIS bus's roster does not hold.
//
// NOTHING IS WRITTEN WHEN IT IS RETURNED, and that is the entire point of the
// sentinel rather than a convenience. See Acceptor.Accept, "the roster is asked
// BEFORE the durable write".
var ErrUnknownLocalRecipient = errors.New("relay: relayed message names an agent in this bus's namespace that this bus's roster does not hold")

// LocalAcceptance is what the LOCAL BUS reports about a relayed message it was
// asked to record. It is not RelayAcceptance: that one is what we report back to
// the PEER, and the two differ precisely where this package's job is — the peer
// is told what it needs to retry safely, while this value carries the applied-key
// outcome that decides whether the message travels any further.
type LocalAcceptance struct {
	// LocalMessageID is the id THIS bus minted for its own copy (invariant 1 —
	// a bus mints ids in its own namespace and never adopts a peer's). On a
	// duplicate it is the ORIGINAL id, replayed verbatim (invariant 10).
	//
	// It is REQUIRED on a successful acceptance. An empty id is refused by
	// Accept rather than acknowledged, because a 200 naming no message is an
	// acknowledgement a peer cannot act on and cannot correlate.
	LocalMessageID string

	// Outcome is what the applied-key table said. THE THREE-WAY SPLIT IS
	// LOAD-BEARING and must reach here uncollapsed (invariant 10, and see
	// idem.Outcome's own doc): new, retry and violation are three different
	// answers with three different behaviours, and this package needs all
	// three — it re-forwards on exactly ONE of them.
	//
	// # THE ZERO VALUE IS idem.OutcomeNew, WHICH IS A TRAP WORTH NAMING
	//
	// A LocalIngest that forgets to set this field reports "new" by default, and
	// "new" is the answer that RE-FORWARDS. A duplicate misreported as new is
	// exactly the amplification loop the OutcomeNew gate exists to stop, so an
	// implementation must set it explicitly on every return path. Accept's
	// empty-LocalMessageID check catches the common shape of the mistake — a
	// zero LocalAcceptance returned by accident — but it cannot catch a seam
	// that fills in the id and forgets the outcome. Set it.
	Outcome idem.Outcome
}

// LocalIngest is the local bus, as the relay ingress needs it: the roster it
// asks BEFORE anything is written, and the durable write itself.
//
// It is an INTERFACE HERE rather than a concrete dependency because this package
// must not import the hub — relay is imported BY the wiring site, never the
// reverse — and because the two calls have to be separable in order to be
// ORDERED. A single "deliver this" method would put the roster check inside the
// durable write, where no test in this package could prove which happened first.
//
// THIS PACKAGE OWNS NO APPLIED-KEY TABLE AND NO ROSTER. internal/idem owns the
// first (its memory is recovered state, not a cache) and the local bus owns the
// second. A second copy of either here would be a second answer to a question
// that must have exactly one — see RelayConfig.AcceptRelay.
type LocalIngest interface {
	// Enrolled reports whether a fully-qualified "<bus-id>.<agent-id>" names an
	// agent enrolled on THIS bus.
	//
	// IT MUST CONSULT THE ROSTER AND NOTHING ELSE. The roster is the only
	// authority on this bus's own namespace; a "yes" derived from anything else
	// — a message the id once appeared in, a peer's advertisement, a cache — is
	// how a name gets admitted that nobody holds, which is the injury cca64afd
	// names. It must be safe for concurrent use: an HTTP handler serves peers in
	// parallel.
	Enrolled(agentID string) bool

	// AcceptRelayed durably records m on this bus and reports what the
	// applied-key table said (invariant 4: it returns only once the write is
	// committed and fsynced, because this handler's 200 IS an acknowledgement).
	//
	// It is called ONLY after the roster check above has passed, and it is the
	// ONLY thing on this path that writes. A non-nil error means NOTHING was
	// applied; an error wrapping ErrIdempotencyViolation is answered as invariant
	// 10's violation case — rejected and logged, and NOBODY IS DISCONNECTED.
	AcceptRelayed(ctx context.Context, m RelayedMessage) (LocalAcceptance, error)
}

// OnwardForwarder is the egress half: the queue a message joins when this bus is
// an intermediate hop rather than (or as well as) the destination.
//
// *Forwarder implements it — the assertion below is compile-time — and it is an
// interface only so that the gating decision ("did anything get queued?") is
// observable in a test without standing up peers, queues and a network.
type OnwardForwarder interface {
	// Enqueue offers m to every peer that should receive it and returns how many
	// outbound copies were accepted onto a queue. It never blocks. See
	// Forwarder.Enqueue.
	Enqueue(m RelayedMessage) (int, error)
}

// The real forwarder must satisfy the seam, or the seam is fiction.
var _ OnwardForwarder = (*Forwarder)(nil)

// AcceptOptions configures NewAcceptor.
type AcceptOptions struct {
	// BusID is THIS bus's server-minted id (invariant 1). It is what decides
	// which recipients are OURS to admit and which are somebody else's to route,
	// so it is validated once at construction rather than trusted per message.
	BusID string

	// Local is the local bus. REQUIRED: a nil one would make every relayed
	// message look accepted while nothing was ever written or delivered, which
	// is indistinguishable from a working relay right up until someone waits for
	// a message that never arrives.
	Local LocalIngest

	// Onward is the egress queue. OPTIONAL, and nil is a legitimate
	// configuration rather than a mistake: a LEAF bus peers with one neighbour,
	// accepts messages for its own agents and forwards nothing. With nil, a
	// message whose recipients are all foreign is still made durable and is then
	// carried no further — the honest behaviour for a bus with no route, and the
	// same one Forwarder.targets produces for a recipient it cannot route.
	Onward OnwardForwarder

	// Logger receives the refusals. Optional; nil discards.
	Logger *logging.Logger
}

// AcceptStats is the observable state of one Acceptor.
type AcceptStats struct {
	// UnknownRecipient counts messages REFUSED because they named a local
	// recipient the roster does not hold — nothing durable was written for any
	// of them. It is the security-interesting number: a rising count is a peer
	// with a stale roster, or a peer probing this bus's namespace, and those two
	// are indistinguishable from any single request.
	UnknownRecipient uint64

	// Applied counts messages the local bus recorded as NEW.
	Applied uint64

	// Duplicates counts messages the applied-key table already had. None of them
	// was re-applied and NONE OF THEM WAS RE-FORWARDED — that is the number that
	// shows the amplification gate doing its job in a cyclic topology.
	Duplicates uint64

	// ForwardedCopies counts outbound copies queued for onward peers, summed
	// across messages: a message forwarded to three peers adds three.
	ForwardedCopies uint64
}

// Acceptor is the AcceptRelay callback: what THIS bus does with a relayed
// message that has already been validated, verified and found not to be a loop.
//
// Accept is the callback; wire it as RelayConfig.AcceptRelay.
type Acceptor struct {
	busID  string
	local  LocalIngest
	onward OnwardForwarder
	log    *logging.Logger

	unknownRecipient atomic.Uint64
	applied          atomic.Uint64
	duplicates       atomic.Uint64
	forwardedCopies  atomic.Uint64
}

// NewAcceptor validates opts and returns the acceptor.
//
// The validation is at CONSTRUCTION, following NewRelayHandler: every omission
// here produces a bus that looks healthy and silently does the wrong thing, and
// a startup failure names which side is broken while a runtime one does not.
func NewAcceptor(opts AcceptOptions) (*Acceptor, error) {
	if err := ids.ValidateBusID(opts.BusID); err != nil {
		return nil, fmt.Errorf("relay: acceptor bus id: %w", err)
	}
	if opts.Local == nil {
		return nil, errors.New("relay: AcceptOptions.Local is required; without it the acceptor would answer a peer with an acknowledgement while nothing was written, delivered or remembered, which is indistinguishable from a working relay until an agent waits for a message that never arrives")
	}
	log := opts.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &Acceptor{busID: opts.BusID, local: opts.Local, onward: opts.Onward, log: log}, nil
}

// Stats reports this acceptor's counters.
func (a *Acceptor) Stats() AcceptStats {
	return AcceptStats{
		UnknownRecipient: a.unknownRecipient.Load(),
		Applied:          a.applied.Load(),
		Duplicates:       a.duplicates.Load(),
		ForwardedCopies:  a.forwardedCopies.Load(),
	}
}

// Accept is the AcceptRelay callback (RelayConfig.AcceptRelay).
//
// THE ORDER OF THE THREE STEPS IS THE WHOLE OF THIS FUNCTION, and neither
// boundary may be moved:
//
//  1. ASK THE ROSTER about every recipient in OUR namespace — BEFORE anything
//     is written.
//  2. WRITE, DURABLY, through the local bus (invariant 4: the 200 this produces
//     is an acknowledgement, so the write is fsynced before it is sent).
//  3. RE-FORWARD, and ONLY on idem.OutcomeNew.
//
// # 1. THE ROSTER IS ASKED BEFORE THE DURABLE WRITE (finding cca64afd)
//
// Write-then-check would let a peer PERMANENTLY EXHAUST AN AGENT NAME on this
// bus, and permanently means permanently: invariant 1 forbids reusing an id,
// INCLUDING ACROSS RESTARTS, and there is no cleanup path that could undo it.
// The mechanism is not hypothetical. cmd/agent-bus/suffixfloors.go derives each
// short name's suffix FLOOR by scanning the recovered log for agent ids in this
// bus's namespace, so a relayed message durably naming
// "<local-bus>.alpha-18446744073709551615" pushes "alpha"'s floor to the top of
// the range and the name can never be minted again, on this start or any later
// one. One accepted envelope, one name burned for ever, and nothing to roll back
// because the log is append-only (invariant 6).
//
// So the roster is the ONLY thing that admits an id into this bus's namespace,
// it is asked FIRST, and its "no" costs a peer exactly one refused request and
// costs this bus nothing durable. hub.publish enforces the identical rule on the
// LOCAL send path and cites the same finding; this is that rule applied to the
// one ingress that the local path does not cover.
//
// A CONFUSABLE SPELLING OF OUR OWN BUS ID IS TREATED AS OURS AND REFUSED.
// "<LOCAL-BUS>.alpha-1" is not routable — Registry.Route refuses any bus half
// that case-folds to ours, precisely so a local delivery is never sent out onto
// the network — so admitting it would durably record a message addressed to
// somebody no part of this system will ever deliver to. The fold is
// deliberately WIDER than the exact comparison the suffix-floor derivation uses:
// each side errs towards its own safe answer, and the safe answer here is to
// refuse.
//
// THE WHOLE MESSAGE IS REFUSED, not just the unknown recipient. A partial
// acceptance would have to be reported as an acceptance, leaving the peer
// believing a recipient was reached that never existed; and it is exactly what
// hub.publish does with a mixed recipient list on the local path. The cost is
// that one bad recipient denies the others their copy — which the sending bus
// can fix, and which is the direction that writes nothing.
//
// # 2. WHAT IS ACKNOWLEDGED, AND WHEN (invariant 4)
//
// A refusal in step 1 acknowledges NOTHING: no id is minted, no record is
// written, no key is remembered, and the peer is free to send the identical
// bytes again once its roster is right. Only step 2's return means "durable",
// and only then does this function report an acceptance. Step 3 happens after
// the acknowledgement is earned and can never be the reason one is given: a
// message this bus has recorded is this bus's responsibility whether or not the
// next hop ever takes it.
//
// # 3. RE-FORWARD ONLY ON idem.OutcomeNew
//
// This is the rule that BOUNDS FAN-OUT, and the package doc calls it out as the
// half that lives in the wiring site. The egress split horizon alone admits one
// copy per simple path, which in a full mesh is factorial rather than linear; it
// is the applied-key table that terminates the traffic, by answering the second
// arrival and sending it NO FURTHER.
//
// A duplicate is therefore answered with the ORIGINAL local message id,
// re-applied nowhere, forwarded nowhere, AND NOBODY IS DISCONNECTED (invariant
// 10): a relayed duplicate is the NORMAL steady state of a cyclic topology and
// of any peer retrying after a lost ack — it is the behaviour of a peer doing
// the right thing. Two further reasons a disconnect would be wrong here, which
// are invariant 10's own two questions: a merely BUGGY peer reaches this line
// constantly, and this connection MULTIPLEXES AN ENTIRE PEER BUS'S ROSTER, so
// dropping it would punish every agent behind that peer for one message.
//
// A violation (same key, DIFFERENT payload) is rejected and logged and nothing
// else — see ErrIdempotencyViolation, whose doc records that the 409 plus the log
// line is the COMPLETE remedy and that the withdrawn disconnect must not be
// reinstated.
func (a *Acceptor) Accept(ctx context.Context, m RelayedMessage) (RelayAcceptance, error) {
	// A RELAYED BROADCAST NEVER REACHES HERE THROUGH THE HANDLER — check 11a of
	// ValidateRelayRequest refuses it, because the canonical signing format has
	// no bytes for an empty audience — and it is refused here too rather than
	// assumed away. This method is exported and a future wiring site could call
	// it directly; a broadcast admitted at this point would be delivered to every
	// agent on this bus WITHOUT any recipient having been roster-checked, which
	// is the check above turned off by one boolean.
	if m.Broadcast {
		return RelayAcceptance{}, fmt.Errorf("%w: a relayed broadcast has no roster-checkable audience and is refused at ingest; SIGN-3 must define a broadcast's signed audience first", ErrInvalidRelay)
	}
	if len(m.Recipients) == 0 {
		return RelayAcceptance{}, fmt.Errorf("%w: a directed relayed message must name at least one recipient", ErrInvalidRelay)
	}
	// A SENDER CLAIMING OUR OWN NAMESPACE IS THE SAME INJURY AS A RECIPIENT
	// CLAIMING IT, and both gates found it: the suffix floors are derived from
	// the SENDER as well as the recipients of every recovered message
	// (cmd/agent-bus/suffixfloors.go), so a durable record whose sender is
	// "<local-bus>.alpha-<huge>" burns that short name exactly as permanently.
	//
	// Unreachable through the handler — ValidateRelayRequest checks 3 and 7 pin
	// the sender's bus half to the ORIGIN bus, and the origin bus is refused if
	// it case-folds to ours — so this is the direct-caller belt to that braces,
	// beside the broadcast one above and refused for the identical reason.
	if senderBus, _, _, err := ids.ParseAgentID(m.Sender); err != nil || strings.EqualFold(senderBus, a.busID) {
		// The sender is NOT echoed: on the arm where it failed to parse it is
		// unbounded input from a direct caller, and the diagnosis does not need
		// it.
		return RelayAcceptance{}, fmt.Errorf("%w: the sender of a relayed message belongs to the origin bus, never to this one, and must be a parseable fully-qualified id (invariant 2)", ErrInvalidRelay)
	}

	// STEP 1. THE ROSTER, BEFORE THE DURABLE WRITE. Do not move this below the
	// write — see the doc comment above for what that costs, permanently.
	if unknown := a.unknownLocalRecipients(m); len(unknown) > 0 {
		a.unknownRecipient.Add(1)
		// LOUD AND SPECIFIC, because this is the moment a stale peer roster and a
		// peer probing our namespace look identical, and only the rate tells them
		// apart. The ids make the line actionable, so they are named — but
		// SUMMARISED rather than dumped: this is the cheapest refusal on the
		// surface to provoke, so the line it produces is bounded independently of
		// how many recipients a caller names (see summariseIDs).
		a.log.Warn("relayed message REFUSED and NOTHING was written: it names agents in this bus's namespace that this bus's roster does not hold. The roster is asked BEFORE the durable write on purpose — an id admitted by anything other than the roster burns that name for ever (invariant 1: ids are never reused, including across restarts)",
			"local_bus", a.busID,
			"origin_bus", m.OriginBus,
			"origin_message_id", m.OriginMessageID,
			"sender", m.Sender,
			"unknown_recipient_count", len(unknown),
			"unknown_recipients", summariseIDs(unknown),
			"unknown_recipient_refusals", a.unknownRecipient.Load(),
		)
		return RelayAcceptance{}, fmt.Errorf("%w: %d of %d recipients", ErrUnknownLocalRecipient, len(unknown), len(m.Recipients))
	}

	// STEP 2. THE DURABLE WRITE. Everything above this line is free to fail
	// without leaving a trace; nothing below it is.
	acc, err := a.local.AcceptRelayed(ctx, m)
	if err != nil {
		// Returned verbatim so the handler can classify it: an error wrapping
		// ErrIdempotencyViolation becomes 409 (rejected, logged, NOT
		// disconnected), and anything else becomes 503 — "not now" rather than
		// "never", which is the honest answer for a local write that failed.
		return RelayAcceptance{}, err
	}
	if acc.LocalMessageID == "" {
		// FAIL CLOSED. Acknowledging a message with no id would hand the peer a
		// success it cannot correlate, cannot retry safely and cannot audit — and
		// the shape that produces it, a zero LocalAcceptance, would also claim
		// idem.OutcomeNew and re-forward. See LocalAcceptance.Outcome.
		return RelayAcceptance{}, fmt.Errorf("relay: the local bus accepted relayed message %s but minted no local message id; refusing to acknowledge a message this bus cannot name (invariant 1)", m.OriginMessageID)
	}

	switch acc.Outcome {
	case idem.OutcomeNew:
		a.applied.Add(1)
	case idem.OutcomeRetry:
		a.duplicates.Add(1)
		// THE ORIGINAL RESULT, REPLAYED. Nothing re-applied, nothing forwarded,
		// nobody disconnected (invariant 10). The onward hop is skipped for the
		// duplicate specifically: the copy that WAS new was already forwarded, so
		// forwarding again would multiply the very traffic the applied-key table
		// exists to terminate.
		a.log.Debug("relayed message was a duplicate: the original result is replayed and it is NOT forwarded onward",
			"local_bus", a.busID,
			"origin_bus", m.OriginBus,
			"origin_message_id", m.OriginMessageID,
			"local_message_id", acc.LocalMessageID,
			"path_hops", len(m.BusPath),
		)
		return RelayAcceptance{LocalMessageID: acc.LocalMessageID, Duplicate: true}, nil
	case idem.OutcomeViolation:
		// The local bus reported the violation by VALUE rather than as an error.
		// It is the same answer either way and it gets the same treatment: 409,
		// a Warn line in the handler, and NO DISCONNECT.
		return RelayAcceptance{}, fmt.Errorf("%w: relayed message %s", ErrIdempotencyViolation, m.OriginMessageID)
	default:
		// An outcome this build does not know is not a licence to guess: guessing
		// "new" re-forwards and guessing "retry" silently drops the onward hop.
		return RelayAcceptance{}, fmt.Errorf("relay: the local bus reported idempotency outcome %s for relayed message %s, which this build cannot act on", acc.Outcome, m.OriginMessageID)
	}

	// STEP 3. ONWARD, AND ONLY FOR A NEW ACCEPTANCE.
	//
	// m is passed AS INGESTED, with the path exactly as it arrived: appending
	// our own hop is Forward's job and doing it here would make the ingress
	// record disagree with the egress envelope (see RelayedMessage.BusPath).
	//
	// A FAILURE HERE NEVER UNDOES THE ACCEPTANCE. The message is already durable
	// on this bus, so the peer is owed its 200 whatever the next hop does;
	// Enqueue's only error is ErrForwarderClosed, a lifecycle condition of THIS
	// bus during shutdown, and turning that into a refusal would ask the peer to
	// retry a message we have already recorded — a guaranteed duplicate.
	if a.onward != nil {
		queued, ferr := a.onward.Enqueue(m)
		if ferr != nil {
			a.log.Warn("relayed message was accepted durably but could not be queued for onward relay; it is NOT refused, because it is already this bus's responsibility",
				"local_bus", a.busID,
				"origin_bus", m.OriginBus,
				"origin_message_id", m.OriginMessageID,
				"local_message_id", acc.LocalMessageID,
				"err", ferr.Error(),
			)
		} else if queued > 0 {
			a.forwardedCopies.Add(uint64(queued))
			a.log.Debug("relayed message queued for onward relay",
				"local_bus", a.busID,
				"origin_message_id", m.OriginMessageID,
				"copies", queued,
			)
		}
	}
	return RelayAcceptance{LocalMessageID: acc.LocalMessageID}, nil
}

// unknownLocalRecipients returns every recipient that CLAIMS THIS BUS and that
// the roster does not hold. An empty result means the message names nobody of
// ours that we do not have — which includes the pure-transit case, where every
// recipient belongs to another bus and the roster is not consulted at all.
//
// The claim is tested by CASE-FOLD, and membership by the EXACT id. A bus half
// that folds to ours but is not spelled like ours is claiming our namespace with
// a confusable, so it is ours to refuse rather than somebody else's to route —
// Registry.Route would refuse to route it too, and admitting it would durably
// record a message nothing can ever deliver.
func (a *Acceptor) unknownLocalRecipients(m RelayedMessage) []string {
	var unknown []string
	for _, r := range m.Recipients {
		bus, _, _, err := ids.ParseAgentID(r)
		if err != nil {
			// Unreachable through the handler — ValidateRelayRequest parses every
			// recipient before a RelayedMessage exists — so this is the
			// direct-caller path, and an id that names no bus at all is not one
			// this bus can attribute to anybody. Refuse it with the others, but
			// under a PLACEHOLDER rather than its own bytes: this is the one
			// entry in this slice that was never length- or charset-checked, and
			// the slice's only consumers are a count and a log line.
			unknown = append(unknown, "<unparseable-recipient>")
			continue
		}
		if !strings.EqualFold(bus, a.busID) {
			// Another bus's namespace: not ours to admit, and not ours to refuse.
			// Whether anyone can route it is step 3's question, and a message with
			// no route is still durably ours (RELAY-16).
			continue
		}
		if bus != a.busID || !a.local.Enrolled(r) {
			unknown = append(unknown, r)
		}
	}
	return unknown
}

// maxLoggedIDs is how many ids one refusal line names before it summarises. The
// COUNT is logged separately and is never truncated, so nothing is hidden: what
// is bounded is the line's size, which is otherwise chosen by the peer.
const maxLoggedIDs = 8

// summariseIDs joins ids for a log line, naming at most maxLoggedIDs of them.
//
// Every id here has been through ids.ParseAgentID (or has been replaced by a
// placeholder), so each one is bounded; what this bounds is their NUMBER. A
// refusal is the cheapest answer on this surface to provoke — it costs the peer
// one request and costs us no write — so the line it emits must not scale with
// a value the peer chooses.
func summariseIDs(ids0 []string) string {
	if len(ids0) <= maxLoggedIDs {
		return strings.Join(ids0, ",")
	}
	return strings.Join(ids0[:maxLoggedIDs], ",") + fmt.Sprintf(",… and %d more", len(ids0)-maxLoggedIDs)
}
