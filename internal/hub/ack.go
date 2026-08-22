package hub

// ACK-6 — THE RECIPIENT DELIVERY ACKNOWLEDGEMENT BOUNDARY (ACK-CONTRACT.md §4).
//
// # THE RULING THIS FILE IMPLEMENTS, AND THE THREE REASONS BEHIND IT
//
// DELIVERY TO AN INBOX OR A POLL IS NOT RECIPIENT RECEIPT. Plane C — "the
// addressed agent's application received and accepted it" — is reached ONLY by
// the recipient calling AcknowledgeDelivery, and is NEVER inferred from a cursor
// advancing. That is ACK-1's ruling and this package must not undo it:
//
//  1. THE BUS CANNOT KNOW WHAT THE RECIPIENT KNOWS. The bus carries the sender's
//     signature as opaque bytes and never verifies it — "the BUS enforces SHAPE
//     and the RECIPIENT enforces AUTHENTICITY" (internal/store/message.go:260-270).
//     A bus that auto-ACKed on poll would assert, on the recipient's behalf, a
//     fact only the recipient can establish.
//  2. THERE IS NO SERVER-SIDE PER-RECIPIENT DELIVERY STATE TO DERIVE IT FROM, and
//     adding one is strictly MORE state than an explicit ACK. The cursor is an
//     opaque, client-held delivery position (cursor.go; the store that persists it
//     is on the CLIENT, client/cursorstore.go).
//  3. A POLL IS REPLAYABLE. Delivery is at-least-once and an unrecognised cursor
//     version is accepted and remapped to position 0 — one full replay of the
//     retained window. One message would produce many "receipts".
//
// THE ENFORCEMENT OF ALL THREE IS THE ABSENCE OF A CALL. Nothing on the read path
// (Since, Wait, HasVisibleAfter, DecodeCursor) touches this file, and
// TestRecipientAcknowledgementBoundary's "delivery to a poll is NOT receipt"
// subtest is what keeps that true. There is deliberately NO `polled` state and
// one must not be added — it would require exactly the per-(agent, message)
// table reason 2 refuses.
//
// # WHAT AUTHORISES A RECIPIENT ACK: THE ROW ITSELF, AND NOTHING ELSE
//
// ack.Store.Settle's doc states the obligation it cannot discharge:
//
//	"ACK-6 (agent surface) MUST prove the authenticated principal EQUALS
//	 recipient before calling. Without that, agent B can mark agent A's message
//	 refused."
//
// This file discharges it STRUCTURALLY rather than with a comparison:
// RecipientAckRequest.Recipient IS the authenticated principal, and it is the
// SECOND HALF OF THE LOOKUP KEY. The lifecycle record is keyed on (correlation
// key, recipient) from day one (§3.2), so an agent can only ever reach the row
// that names it. There is no request field for a recipient, so there is nothing
// for a caller to put another agent's id into — the same shape /v1/send uses when
// it attributes a message to the context principal and discards body.Sender.
//
// A row exists for (key, principal) if and only if this bus committed a message
// addressed to that principal under that key. So "was I addressed?" and "am I
// entitled to settle this?" are ONE question with ONE answer, and it is answered
// by a map lookup that cannot be talked out of it.
//
// # THIS BOUNDARY CONSULTS THE MESSAGE STORE ON EXACTLY ONE ARM (ACK-5 NARROWED IT)
//
// AMENDED IN PLACE, 2026-08-21, BY ACK-5. This section was headed "THIS
// BOUNDARY NEVER CONSULTS THE MESSAGE STORE, AND THAT IS A DECISION" and the
// absolute is no longer true: transitAck asks store.Store one routing question.
// The original decision is reproduced FIRST and in full, because it still
// governs every arm but the new one and it is the reason the narrowing is one
// arm wide. A stale absolute is more dangerous here than no comment, because it
// reads as freshly checked.
//
// ## THE ORIGINAL DECISION (ACK-6), WHICH STILL GOVERNS EVERY ROW
//
// Wherever a lifecycle row exists, the row is the authority and store.Store is
// not consulted at all — no settle, no duplicate label and no refusal below is
// conditioned on whether the MESSAGE is still held. The two retention regimes
// are different and the difference matters:
//
//   - A MESSAGE is retained for 1 day OR 1 GiB, whichever bites first
//     (store.go's retention defaults; CONTRACTS-HTTP.md "Retention"), so a busy
//     bus prunes message bodies long before a day has passed.
//   - A LIFECYCLE ROW is retained for ack.Retention (24h) from accepted_at.
//
// Requiring the MESSAGE to still be held would therefore make an ACK fail for a
// message the recipient demonstrably received and is holding a copy of, and
// would strand its sender's row non-terminal for the rest of the window. It
// would also add a second, differently-expiring answer to "were you addressed?"
// — and §3's "two fields that must agree are two fields that can disagree"
// applies to two TABLES with far more force.
//
// So an EXPIRED MESSAGE has two distinct meanings and this boundary answers both
// without asking the message store anything:
//
//   - the message body was pruned but the row is retained -> the ACK is accepted
//     normally. The recipient may ACK at its leisure.
//   - the row itself was swept (§11) -> ErrAckNotRetained, the uniform answer,
//     and NO ROW IS CREATED. `unknown` is a REPORTING value and must never come
//     back to be written (§8.1); resurrecting a row here would also let any
//     authenticated agent mint durable rows for keys it invented, which is both
//     an unbounded write amplifier and the status oracle §13.3 exists to close.
//
// ## WHAT ACK-5 CHANGED, AND WHY NONE OF THE REASONS ABOVE REACH IT
//
// A RELAYED message has NO lifecycle row here and never gets one:
// hub.recordAcceptance returns early for relayed ingest, deliberately, because
// the sender-visible row is read by exactly one party — the ORIGINAL SENDER on
// the ORIGIN bus (§13.3) — so a row on an intermediate or terminal bus is
// readable by NOBODY. It would cost an fsync per recipient (peer-driven, up to
// store.MaxRecipients = 64 per relayed message, under the global write lock —
// the hazard recordAcceptance's own cost note tells the next task to re-check)
// and buy no observation. It would also put SEVERAL rows behind one correlation
// key, which is what ACK-4-FU-RECIPIENT-BINDING says must not happen before it
// lands.
//
// So without a second authorization path a terminal outcome could never
// ORIGINATE at the far end of a multi-hop route: the recipient on bus C would
// find no row and be told the uniform `unknown`, and §8.4's rule — that a hop
// receipt never converts to delivery — would leave plane C unreachable beyond
// one hop. That is the defect ACK-5 exists to fix.
//
// THE NARROW ARM: on ack.ErrNoRecord ONLY, transitAck asks store.Store whether
// this bus holds a RELAYED copy under this correlation key that NAMES THE
// AUTHENTICATED PRINCIPAL as a recipient. If it does, this bus AUTHORIZES the
// acknowledgement and reports it as TRANSIT (RecipientAckResult.Transit); the
// caller forwards it one hop back along the stored path and the ORIGIN makes it
// durable. NOTHING DURABLE IS WRITTEN HERE.
//
// INVARIANT 4 IS SATISFIED END TO END BY THE CHAIN RATHER THAN BY A LOCAL
// WRITE. The recipient is not told "accepted" until the origin has fsynced,
// because the forward is SYNCHRONOUS and the origin answers only after its own
// commit. That is the whole reason this design needs no retry queue and no
// local spool — and why adding one here would be building ACK-7/ACK-14 inside
// ACK-5.
//
// THAT SENTENCE IS NOT AN ABSOLUTE, AND HERE IS THE ONE ARM THAT FALSIFIES
// IT: an INTERMEDIATE bus absorbs a 409 from
// the hop above it and answers its downstream 200 (cmd/agent-bus/relaywiring.go,
// disposeUnrecordedAck). So on a chain of two or more backward hops a
// recipient can be told `accepted` for an outcome the ORIGIN refused with a
// 409 — in the "no obligation binds that recipient" case, with nothing durable
// anywhere. THIS BUS never does that itself: every TransitAck error, 409
// included, is answered 503 here. A 409 is absorbed because re-offering a
// finally-refused frame is the retry amplification §9.3 exists to stop and
// forwarding the verdict verbatim would make the hop an oracle; every OTHER
// final status (404, 403, 400) means the upstream decided nothing and is
// answered "not now". Recorded in DECISIONS.md, 2026-08-21 (ACK-5).
//
// WHY THE ORIGINAL REASONS DO NOT APPLY TO THIS ARM:
//
//   - THE TWO-TABLES-CAN-DISAGREE HAZARD CANNOT ARISE, because the two paths are
//     DISJOINT BY CONSTRUCTION: a message is either relayed or locally
//     originated, never both, and only the locally-originated one has a row.
//     transitAck refuses a locally-originated message outright — this bus IS
//     its origin, so forwarding would send a terminal outcome to a bus that
//     never owed us one, and a swept row would silently become a network event.
//     No row is ever settled, reopened or overridden on the strength of a
//     message lookup.
//   - THE RETENTION OBJECTION IS REAL HERE AND IS ACCEPTED AS A COST, not
//     argued away: a transit acknowledgement STOPS WORKING once the relayed
//     MESSAGE is pruned (1 day or 1 GiB, whichever bites first), which can
//     happen well before ack.Retention would have expired a row. The recipient
//     is then told the uniform `unknown`, exactly as it is for a swept row. The
//     alternative — writing unreadable rows on every bus — is the cost the
//     paragraph above refuses, and this bus genuinely has nothing else to bind
//     the frame to once the message is gone (§6.2).
//
// # AN OFFLINE RECIPIENT IS A NO-OP, ON PURPOSE
//
// There is no timer, no delivery deadline and no path by which not-ACKing
// becomes an outcome. A recipient that is offline, or online and silent, leaves
// its sender's row in `accepted`/`in_flight` until §11's window sweeps it, after
// which the status API answers `unknown`. §4 states that cost rather than hiding
// it: the sender genuinely does not know, and `unknown` is a first-class answer
// rather than an error. An ACK is also NOT bound to the session or the poll that
// delivered the message — the recipient may ACK later, after a restart, over a
// different session and a different connection, because the only thing checked is
// that the principal is the one the row names.
//
// # NO NEW DISCONNECT (§12, invariant 10)
//
// Every refusal below is reject-and-log. Invariant 10's two questions were
// answered on the record by ACK-1: a merely BUGGY client reaches every one of
// these lines (an agent that mistypes a key, an agent that re-ACKs after its own
// restart), and dropping a socket punishes every other request that client had
// pipelined on it. Nothing in this file, and nothing in the route that calls it,
// may close a connection.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// Sentinel errors for the recipient boundary. As with errors.go's set, the HTTP
// layer maps these BY SENTINEL and never by matching error text.
var (
	// ErrAckNotRetained is THE UNIFORM ANSWER (§13.3) and it deliberately
	// collapses four different facts into one:
	//
	//   - no such correlation key was ever accepted here;
	//   - the key exists but names a message this agent was not addressed in;
	//   - the row was swept by §11's retention;
	//   - the key is malformed enough that no row could match it.
	//
	// They MUST stay indistinguishable. A distinct answer for "that key exists
	// but is not yours" is a message-existence oracle: an authenticated agent
	// could enumerate keys and learn which ones this bus is carrying, and for
	// whom. handleBroadcast already applies the same reasoning to a 501, and
	// ack.Store.Lookup applies it to a malformed key by returning a MISS rather
	// than an error. The caller renders this as `unknown`.
	ErrAckNotRetained = errors.New("hub: no retained delivery lifecycle row for this (correlation key, recipient)")

	// ErrAckTerminalConflict reports invariant 10's SECOND case on this plane: a
	// DIFFERENT terminal outcome was offered for a pair that is already terminal.
	// The FIRST terminal stands, nothing is written, it is logged by ack.Store at
	// ERROR, and NOBODY IS DISCONNECTED. Terminal is absorbing (§8.1): a terminal
	// state is never revisited, never reopened and never downgraded.
	ErrAckTerminalConflict = errors.New("hub: this delivery outcome is already terminal with a different outcome")

	// ErrInvalidAck reports a recipient acknowledgement whose SHAPE this bus will
	// not record: an outcome outside {delivered, refused}, a class that does not
	// pair with the outcome, a class from the BUS-EMITTED half of the set, or an
	// attestation label a recipient cannot produce. Every one of them is refused
	// BEFORE the lifecycle table is touched, so a malformed acknowledgement costs
	// this bus nothing durable.
	ErrInvalidAck = errors.New("hub: invalid recipient acknowledgement")

	// ErrAckInFlight reports that another durable transition for the SAME
	// (correlation key, recipient) is being fsynced right now
	// (ack.ErrConcurrentTransition). It is TRANSIENT and the retry genuinely
	// succeeds — it lands on the row the in-flight transition wrote and is
	// absorbed as a duplicate.
	//
	// SECURITY FINDING, 2026-08-16 (ACK-6 security gate, MEDIUM): it had no arm
	// and fell through to the catch-all, so four concurrent BYTE-IDENTICAL
	// acknowledgements answered one 500 and one ERROR log line. Invariant 10's
	// first case says a same-key/same-payload retry returns the original result
	// and DOES NOT ERROR; a 500 is the loudest possible way to break that, and it
	// is reachable by a client doing nothing worse than retrying eagerly.
	ErrAckInFlight = errors.New("hub: another delivery outcome for this pair is being made durable; retry")

	// ErrNoAckTable reports a bus built without a delivery lifecycle table (or
	// with a recorder that records acceptance but cannot settle). The route
	// answers 501 — not 503, which would promise a retry that can never help,
	// and not 404, which would tell a caller the protocol lacks a route it has.
	// The capability is absent from this BUILD; see writeAckError in
	// internal/httpapi/ack.go, which is the authority on the code.
	ErrNoAckTable = errors.New("hub: no delivery lifecycle table is wired on this bus")
)

// AckSettler is the RECIPIENT-SIDE half of the delivery lifecycle seam: the
// durable transition from a non-terminal row to a terminal one.
//
// # WHY IT IS A SEPARATE INTERFACE FROM AckRecorder, RATHER THAN A METHOD ON IT
//
// AckRecorder is documented, at length, as the SEND-PATH seam whose defining
// property is that IT MUST NEVER FAIL A SEND: its errors — including the
// deliberate capacity refusals — degrade an observation and are swallowed, by
// design (§11.3). Settling is the opposite kind of operation. It is a REQUEST in
// its own right, made by the recipient, and its failure IS the answer the
// recipient gets. Hanging it off the same interface would put two contradictory
// error contracts on one seam, and the next reader would have to know which
// method they were looking at to know whether an error mattered.
//
// Keeping them apart also means a build may wire an acceptance recorder without
// a settle path and still be coherent — the send path records, /v1/ack answers
// ErrNoAckTable, and nothing pretends otherwise.
//
// The production implementation is *ack.Store and the assertion below is a
// COMPILE-TIME one, so the optional type assertion in ackSettler can never
// silently stop matching after a signature change in internal/ack.
type AckSettler interface {
	// Settle durably moves (correlationKey, recipient) to a terminal state.
	//
	// It must return ack.ErrNoRecord when no row exists, ack.ErrTerminal when a
	// DIFFERENT terminal is already recorded, and nil both for a fresh
	// transition and for a byte-identical retry (invariant 10's first case).
	Settle(correlationKey, recipient string, state ack.State, class ack.Class, attestedBy ack.Attestation) error

	// Lookup returns the retained row for a pair, false meaning NOT RETAINED.
	//
	// It is here for ONE reason: labelling RecipientAckResult.Duplicate, which
	// Settle cannot report because it answers nil for both a fresh transition
	// and a byte-identical retry. It is NOT the authorization check and must
	// never become one — see AcknowledgeDelivery. It is part of the interface
	// rather than an optional type assertion at the call site because a seam
	// that silently loses a capability is a seam whose absence nothing goes red
	// for.
	Lookup(correlationKey, recipient string) (ack.Record, bool)
}

// The production settler. This is a compile-time assertion and not a runtime
// one: ackSettler's type assertion is what makes the seam optional, and a type
// assertion that quietly began failing — because internal/ack changed a
// parameter — would turn every /v1/ack into a 503 with nothing red anywhere.
var _ AckSettler = (*ack.Store)(nil)

// RecipientAckRequest is one recipient declaring the fate of one message
// (ACK-CONTRACT.md §9.2, agent surface).
//
// EVERY FIELD IS EITHER THE AUTHENTICATED PRINCIPAL, A SERVER-MINTED ID, OR A
// MEMBER OF A CLOSED ENUM. There is no free-text field and one must not be added
// — invariant 6: a recipient-supplied reason string is a body by another name,
// and this record is written to an append-only trail nobody can rewrite. The
// recipient and the sender already have an end-to-end message channel for prose
// and it is the right place for it (§5.2).
type RecipientAckRequest struct {
	// Recipient is THE AUTHENTICATED PRINCIPAL, fully qualified (invariant 2).
	//
	// IT IS NEVER A BODY FIELD. The route takes it from the request context —
	// the same context principal /v1/send attributes a message to — and there is
	// no wire field it can be sourced from. It is also the second half of the
	// lookup key, which is what makes authorization structural: see the file
	// header.
	Recipient string

	// CorrelationKey is the ORIGIN bus's server-minted message id (§3), read on
	// the sending side through store.Message.OriginID(). For a message that
	// originated here it IS the message's own id.
	//
	// It is CLIENT-SUPPLIED and is therefore INPUT TO BE VALIDATED, NEVER AN
	// IDENTITY TO BE TRUSTED (invariant 1). It identifies nobody by itself; the
	// row it selects does.
	CorrelationKey string

	// Outcome is the terminal state the recipient is declaring: exactly
	// ack.StateDelivered or ack.StateRefused.
	//
	// ack.StateUndeliverable is NOT reachable from here and must never be. It is
	// a claim about the FEDERATION'S ROUTING, asserted by a bus about its own
	// failure to deliver; a recipient application has no standing to make it, and
	// accepting one would durably record a bus-emitted class as though a bus had
	// said it. The non-terminal states are not carriable either: a settle moves a
	// row OUT of them.
	Outcome ack.State

	// Class is the closed NACK class, set IFF Outcome is ack.StateRefused and
	// forbidden otherwise — validated in both directions.
	//
	// It must come from the RECIPIENT-EMITTED half of the set (exactly three
	// members). The half-set rule is anti-forgery and not tidiness: without it a
	// recipient sends outcome=refused with class=horizon_expired and this bus
	// records ITS OWN routing failure as THE RECIPIENT'S DECISION — a different
	// claim about a different party, in a durable trail a sender reads.
	Class ack.Class

	// AttestedBy is the label §6.3 requires the record to carry, and on this
	// surface there is exactly ONE legal value:
	// ack.AttestedByRecipientSignatureUnverified.
	//
	// It is a PARAMETER rather than a constant because the label is a fact about
	// WHICH GATE AUTHENTICATED THE FRAME, and only the mount site knows that
	// (relay.AckSurface says the same thing on the peer side). Defaulting it here
	// would let a future second call site record `peer_bus` — this bus telling a
	// sender, durably, that an adjacent BUS vouched for something an agent said.
	// ack.AttestedByPeerBus is refused below.
	AttestedBy ack.Attestation
}

// RecipientAckResult is what the recipient is told. It reports the outcome that
// now STANDS, which on a duplicate is the ORIGINAL one.
//
// It carries no sender, no bus path, no peer identity and no timestamps beyond
// what the recipient already supplied: this is the recipient's side of the
// exchange, and everything about the message's routing belongs to §13's
// sender-visible status route (ACK-9) behind ITS OWN authorization.
type RecipientAckResult struct {
	// State is the terminal state now recorded for the pair.
	State ack.State

	// Class is the class now recorded, empty for a positive terminal.
	Class ack.Class

	// Duplicate reports invariant 10's FIRST case: this pair was ALREADY
	// terminal with exactly this outcome, so the original result stands and
	// nothing was re-applied. It is not an error and nobody is disconnected.
	//
	// # THE ONE CASE IT CAN MISLABEL, STATED RATHER THAN HIDDEN
	//
	// It is derived from a lookup taken BEFORE the settle, so two IDENTICAL
	// acknowledgements racing each other can both observe a non-terminal row and
	// both report Duplicate=false. Exactly one of them writes; the other is
	// absorbed by ack.Store under its own lock. The STATE reported is correct in
	// both, and the mislabelled field is the one describing a retry — the only
	// case where either label is defensible. Closing it properly means ack.Store
	// reporting which branch it took, which is a change to ACK-2's tested API and
	// belongs to the task that also needs it on the peer surface. relay's
	// AckSettlement.Duplicate is the SAME shape and IS wired
	// (cmd/agent-bus/relaywiring.go, SettleAck), so the two call sites share the
	// gap and a fix should close both — ACK-3-FU-SETTLEACK-RACE-ARM is the task
	// that owns it.
	Duplicate bool

	// Transit reports that this bus did NOT record the outcome because the
	// message was RELAYED here and the durable lifecycle row lives on the ORIGIN
	// bus (§13.3: the row is read by the original sender and by nobody else).
	// This bus AUTHORIZED the acknowledgement — the principal is a named
	// recipient of a relayed copy it holds — and wrote nothing.
	//
	// # WHAT THE CALLER OWES, AND IT IS NOT OPTIONAL
	//
	// The caller MUST propagate the outcome ONE HOP BACK along the stored bus
	// path (relay.DisposeAck / relay.UpstreamHop decide where; §9.4 forbids
	// contacting any bus this one is not peered with, and forbids skipping to
	// the origin) and MUST NOT answer the recipient "accepted" until that
	// forward succeeds. THAT SYNCHRONOUS ORDERING IS THE WHOLE OF INVARIANT 4 ON
	// THIS PATH: nothing durable happens on this bus, so the only thing standing
	// between the recipient and a lost terminal outcome is that it is not told
	// otherwise until the origin has fsynced. A caller that answers 200 and
	// forwards afterwards has silently converted the acknowledgement plane to
	// best-effort, and no test on this bus would notice.
	//
	// # "UNTIL THE ORIGIN HAS FSYNCED" HAS EXACTLY ONE EXCEPTION
	//
	// The obligation above is unconditional for THIS caller, and it is kept: a
	// successful forward is the only thing that lets it answer 200. What the
	// sentence cannot promise is what happened FURTHER back. An INTERMEDIATE
	// bus absorbs a 409 from the hop above it and answers its downstream 200
	// (cmd/agent-bus/relaywiring.go, disposeUnrecordedAck), so on a chain of two
	// or more backward hops a "successful forward" can end at an intermediate
	// whose own upstream refused — and the recipient is then told `accepted`
	// for an outcome the ORIGIN refused, with nothing durable anywhere in the
	// "no obligation binds that recipient" case.
	//
	// That is deliberate and recorded (DECISIONS.md, 2026-08-21, ACK-5): a 409
	// is the one refusal where the upstream UNDERSTOOD the frame and decided
	// about it, re-offering it is the retry amplification §9.3 exists to stop,
	// and forwarding the verdict verbatim would make the hop an oracle for
	// whether the origin holds a row for a named recipient. Every OTHER final
	// status (404, 403, 400) means the upstream decided nothing, is answered
	// "not now", and the sentence above holds.
	//
	// # State AND Class ARE STILL SET; Duplicate IS ALWAYS FALSE
	//
	// State and Class are the recipient's OWN declaration echoed back, not
	// anything this bus looked up — an intermediate re-classifies nothing and
	// re-attests nothing (§9.4, forwarded verbatim).
	//
	// Duplicate is always false on this path and that is HONEST rather than a
	// bug: this bus keeps no record for a relayed message, so there is nothing
	// here for a retry to be a duplicate OF. The duplicate is absorbed WHERE THE
	// RECORD IS — at the origin, under §8.2 note 2, which returns the original
	// result and re-applies nothing. The recipient's own retry is invariant 10's
	// first case and is likewise handled there: it must not error and it must
	// not disconnect. Labelling it here would mean this bus asserting something
	// about a table it does not hold.
	Transit bool
}

// ackSettler resolves the optional settle seam, or explains its absence.
func (h *Hub) ackSettler() (AckSettler, error) {
	if h.acks == nil {
		return nil, ErrNoAckTable
	}
	s, ok := h.acks.(AckSettler)
	if !ok {
		// A recorder that records acceptance but cannot settle. Legal, and it
		// must be LOUD rather than a mysterious 503: an operator seeing /v1/ack
		// fail needs to know it is a wiring fact about this build and not a
		// transient one.
		h.log.Error("a recipient acknowledgement arrived but the wired delivery lifecycle table cannot SETTLE, only ACCEPT; POST /v1/ack cannot work on this build and every sender-visible row will stay non-terminal until it is swept",
			"recorder_type", fmt.Sprintf("%T", h.acks))
		return nil, ErrNoAckTable
	}
	return s, nil
}

// AcknowledgeDelivery is THE recipient delivery acknowledgement boundary: the
// one and only way a message becomes `delivered` or `refused` on this bus.
//
// req.Recipient MUST already be the authenticated principal — the caller is the
// route, and there is no field a client can put it in. See the file header for
// why that makes the authorization structural rather than a comparison, and read
// it before adding any parameter to this method.
//
// # THE ORDER OF THE CHECKS IS THE DESIGN
//
//  1. The SEAM, because a bus with no lifecycle table cannot answer at all and
//     saying so is more honest than validating input it will then discard.
//  2. The SHAPE — outcome, class, attestation — before the table is touched, so
//     a malformed acknowledgement writes nothing, logs no attacker-chosen
//     string, and cannot be used to probe which rows exist by timing.
//  3. The LOOKUP, whose miss IS the authorization refusal and the retention
//     refusal and the never-addressed refusal, all wearing the same face.
//  4. The SETTLE, which is durable (invariant 4: the transition is committed and
//     fsynced before this method returns, and therefore before the recipient is
//     told anything).
//  5. THE TRANSIT CHECK (ACK-5) — A SECOND AUTHORIZATION PATH, USED ONLY WHEN
//     THERE IS NO ROW, AND ONLY FOR A RELAYED COPY. It is fifth because it is
//     reachable from exactly one place: the ack.ErrNoRecord arm of step 4. A
//     row, when one exists, remains the only authority for settling it, and this
//     step never settles anything — it authorizes an outcome this bus will not
//     record and reports Transit so the caller propagates it one hop back
//     toward the bus that will. "Toward", not "to": the hop this bus makes is
//     to its own upstream neighbour, and RecipientAckResult.Transit names the
//     one case (an intermediate absorbing a 409) in which a successful hop does
//     NOT mean the origin recorded anything. Every miss that is not a transit answers ErrAckNotRetained
//     byte-identically to before, so the uniform answer (§13.3) is unchanged for
//     every non-transit case. Moving this check ABOVE the settle would make a
//     store lookup precede the row lookup on every acknowledgement and would
//     hand the message store a say in settling rows — read the file header's
//     amendment before considering it.
//
// Nothing here disconnects anybody (§12).
func (h *Hub) AcknowledgeDelivery(req RecipientAckRequest) (RecipientAckResult, error) {
	settler, err := h.ackSettler()
	if err != nil {
		return RecipientAckResult{}, err
	}
	if err := validateRecipientAck(req); err != nil {
		return RecipientAckResult{}, err
	}

	// The pre-lookup serves ONE purpose: labelling a duplicate. It is NOT the
	// authorization check and must not be turned into one — ack.Store.Settle
	// re-reads the row under its own lock and is the only authority on whether
	// the transition may happen. A check here that gated the call would be
	// advisory by construction, which is the defect Settle's own reservation
	// comment describes.
	duplicate := false
	if r, found := settler.Lookup(req.CorrelationKey, req.Recipient); found {
		duplicate = r.State.Terminal() && r.State == req.Outcome && r.Class == req.Class
	}

	err = settler.Settle(req.CorrelationKey, req.Recipient, req.Outcome, req.Class, req.AttestedBy)
	switch {
	case err == nil:
		h.log.Debug("recipient acknowledgement recorded",
			"correlation_key", elideAckField(req.CorrelationKey),
			"recipient", req.Recipient,
			"state", req.Outcome.String(),
			"class", string(req.Class),
			"attested_by", string(req.AttestedBy),
			"duplicate", duplicate,
		)
		return RecipientAckResult{State: req.Outcome, Class: req.Class, Duplicate: duplicate}, nil

	case errors.Is(err, ack.ErrNoRecord):
		// ACK-5: NO ROW HERE MAY MEAN THE ROW IS SOMEWHERE ELSE. A message
		// relayed to this bus never gets a lifecycle row (see the file header's
		// amendment), so its recipient would otherwise be told the uniform
		// `unknown` and a terminal outcome could never originate at the far end
		// of a multi-hop path. This is the ONLY place transitAck is consulted,
		// and it settles nothing.
		if h.transitAck(req) {
			h.log.Debug("recipient acknowledgement is a TRANSIT acknowledgement: the message was RELAYED to this bus, so no lifecycle row exists here and NOTHING WAS WRITTEN. The caller must forward this outcome one hop back along the stored path and must not answer the recipient until the origin has made it durable",
				"correlation_key", elideAckField(req.CorrelationKey),
				"recipient", req.Recipient,
				"state", req.Outcome.String(),
				"class", string(req.Class),
			)
			// The bus path is DELIBERATELY NOT LOGGED: §13.3 forbids disclosing
			// the traversed path to the parties on this surface, and an operator
			// log line is not the place to start.
			return RecipientAckResult{State: req.Outcome, Class: req.Class, Transit: true}, nil
		}

		// DEBUG, not WARN, and the four causes are NOT distinguished in the log
		// either — an operator hunting a probe wants the request id and the
		// principal, both of which the route's own line carries, and this bus
		// genuinely cannot tell "swept" from "never yours" without the extra
		// state §4 refuses to keep. It is routine: an agent that ACKs after the
		// window, or after its own restart, lands here honestly.
		h.log.Debug("recipient acknowledgement refused: no retained lifecycle row for this (correlation key, recipient). The four causes -- never accepted, not addressed to this agent, swept by retention, or malformed -- are deliberately indistinguishable (§13.3), and NOBODY IS DISCONNECTED",
			"correlation_key", elideAckField(req.CorrelationKey),
			"recipient", req.Recipient,
			"state", req.Outcome.String(),
			"class", string(req.Class),
			"attested_by", string(req.AttestedBy),
		)
		return RecipientAckResult{}, ErrAckNotRetained

	case errors.Is(err, ack.ErrTerminal):
		// ack.Store already logged this at ERROR with both outcomes named, so it
		// is NOT logged a second time here — the same rule recordAcceptance
		// applies to the capacity refusals, and for the same reason: a second
		// unthrottled copy on a route an authenticated agent can drive is how an
		// observability table becomes a disk outage.
		return RecipientAckResult{}, ErrAckTerminalConflict

	case errors.Is(err, ack.ErrInvalidRecord):
		// DEFENCE IN DEPTH for the HIGH finding validateRecipientAck now closes at
		// the top of this method. ack.Store's own validatePair is the second place
		// a malformed correlation key or recipient can be caught, and it must
		// answer the SAME uniform refusal rather than the catch-all's 500 — the
		// four facts §13.3 collapses do not become five because the check that
		// caught it was the inner one. Kept as a mapping and not deleted as
		// unreachable: "unreachable" here depends on two packages agreeing, and
		// the day they stop agreeing this arm is the difference between a uniform
		// answer and a remotely-drivable 5xx.
		h.log.Debug("recipient acknowledgement refused by the lifecycle table's own id validation; answering the uniform refusal",
			"recipient", req.Recipient, "err", err)
		return RecipientAckResult{}, ErrAckNotRetained

	case errors.Is(err, ack.ErrConcurrentTransition):
		// TRANSIENT, and DEBUG rather than ERROR: two acknowledgements of one pair
		// racing is ordinary client behaviour, not a fault, and it is exactly the
		// shape an eager retry produces. The caller answers 503 with a
		// Retry-After; the retry lands on the row the winner wrote and is absorbed
		// as a duplicate.
		h.log.Debug("recipient acknowledgement deferred: another transition for this pair is being made durable",
			"correlation_key", elideAckField(req.CorrelationKey),
			"recipient", req.Recipient,
		)
		return RecipientAckResult{}, ErrAckInFlight

	case errors.Is(err, ack.ErrNotDurable):
		// Invariant 4 has no best-effort setting: a bus that cannot write cannot
		// acknowledge. Mapped to the hub's own sentinel so the HTTP layer's
		// existing 503 arm answers it.
		return RecipientAckResult{}, ErrNotDurable

	default:
		// Everything left is a validation refusal from ack.Store (a malformed
		// correlation key or recipient) or a durable-write failure. Both are
		// rare by construction and neither may be quiet.
		h.log.Error("recipient acknowledgement failed",
			"correlation_key", elideAckField(req.CorrelationKey),
			"recipient", req.Recipient,
			"err", err,
		)
		return RecipientAckResult{}, fmt.Errorf("hub: recording the recipient acknowledgement: %w", err)
	}
}

// transitAck reports whether this acknowledgement is for a message that was
// RELAYED to this bus and names the authenticated principal as a recipient —
// ACK-5's second authorization path (ACK-CONTRACT.md §9.4).
//
// It is consulted from ONE place, the ack.ErrNoRecord arm of
// AcknowledgeDelivery, and it authorizes only. It writes nothing, settles
// nothing, sends nothing and never touches the lifecycle table.
//
// THREE CONDITIONS, ALL REQUIRED, AND EACH ONE REFUSES INTO THE SAME UNIFORM
// ANSWER (§13.3): the correlation key names ANOTHER bus, the message it names is
// a RELAYED copy this bus still retains, and the authenticated principal is one
// of its named recipients. The first is the one that looks redundant and is not
// — it is the difference between the uniform `unknown` and a 503 no client can
// ever clear; the block at the check itself has the whole argument.
//
// # THE MEMBERSHIP TEST IS THE ENTIRE AUTHORIZATION, AND IT IS STRUCTURAL
//
// The same argument the file header makes for the row lookup applies here
// unchanged: req.Recipient IS the authenticated principal, taken from the
// request context, and there is NO request field a caller could put another
// agent's id into. So an agent can only ever ask whether IT is a recipient, and
// a hit means this bus is holding a message it handed — or would hand — to that
// agent. There is nothing to compare against a claim, because no claim is made.
//
// A NON-MEMBER AND A MISS ARE INDISTINGUISHABLE. Both return false and both
// land on the uniform refusal (§13.3), so this adds no message-existence oracle:
// the only new fact an agent can obtain is about mail addressed to itself.
//
// # IDS ARE COMPARED EXACTLY, NOT FOLDED — AND THAT IS THE MATCHING RULE
//
// The comparison below is `==`, which is the rule store.Message.VisibleTo uses
// for recipient membership — and VisibleTo is what decided whether this agent
// could ever have been HANDED this message. Matching more loosely here than the
// delivery path matched would authorize an agent to acknowledge a message it was
// never shown. hub.checkRelayedRecipient folds only the BUS HALF, and only to
// catch a CONFUSABLE claim on this bus's own namespace, after which it too
// requires an exact match plus a roster hit; so exact comparison is the rule at
// both ends and folding here would be the odd one out. Both ids are
// fully-qualified `<bus-id>.<agent-id>` (invariant 2) and req.Recipient has
// already passed ids.ParseAgentID in validateRecipientAck.
//
// # THE RELAYED TEST IS A POST-CONDITION, NOT THE DISCRIMINATOR — AMENDED 2026-08-21
//
// WHAT IT IS FOR. A LOCALLY-ORIGINATED message ALWAYS had a row written for it
// (hub.recordAcceptance), so reaching ack.ErrNoRecord for one means the row was
// swept or never created — and that must keep the uniform refusal. Letting a
// local message take the transit path would make this bus forward a terminal
// outcome for a message it is the ORIGIN of, to a bus that never owed it one,
// turning an expired row into an unsolicited network contact.
//
// WHAT ACTUALLY ENFORCES IT IS THE BUS-HALF CHECK ABOVE, AND THIS COMMENT SAID
// OTHERWISE UNTIL A MUTATION PROVED IT. Deleting `!prov.Relayed` leaves the
// WHOLE internal/hub package green, because the two checks are not independent:
// store.ByOriginMessageID resolves through byOrigin — every entry of which is a
// relayed copy — or else falls back to treating the key as a LOCAL id, and a
// local id's bus half is OURS, which the bus-half check has already refused. So
// by the time control reaches here, the message came from byOrigin and Relayed
// is true by construction.
//
// IT IS KEPT, AND KEPT HONESTLY LABELLED. It is a stated post-condition of the
// same kind relay.UpstreamHop's final self-hop check openly claims for itself:
// it costs one branch, it is the one line that says "a locally-originated
// message must never reach the transit path" at the place that would forward
// one, and it is what catches a future edit that relaxes the bus-half check —
// which is otherwise the single point of failure for that rule. What it is NOT
// is a second, independent discriminator, and a reader who believed this
// paragraph's earlier claim would have concluded the rule was doubly enforced
// when it is enforced once.
func (h *Hub) transitAck(req RecipientAckRequest) bool {
	if h.store == nil {
		// A hub with no message store cannot answer the routing question at
		// all, and "cannot answer" is not "yes". Fail closed to the uniform
		// refusal; Open always wires one, so this is a guard rather than an
		// expectation.
		return false
	}

	// THE CORRELATION KEY MUST NAME ANOTHER BUS (§3), AND CHECKING IT HERE IS
	// NOT BELT-AND-BRACES — WITHOUT IT THIS ROUTE ANSWERS 503 FOREVER TO A
	// PLAUSIBLE MISTAKE.
	//
	// §3 rules that the correlation key is the ORIGIN bus's server-minted
	// message id. For a message this bus RELAYED, the origin is by construction
	// some OTHER bus, so a key whose bus half is ours can never be the key of a
	// transit acknowledgement. It is a LOCAL id.
	//
	// The trap is that a local id nevertheless RESOLVES. store.ByOriginMessageID
	// falls back to treating an unmatched key as a local id — a documented and
	// correct property of that method — so the id THIS bus minted and served to
	// the recipient resolves to the very same relayed message. Relayed is true,
	// the recipient is a member, and without this check the answer below would
	// be "yes, transit". The caller would then ask relay.DisposeAck where to
	// send it, be told AckStopAtOrigin (the bus half is ours), and answer a
	// RETRIABLE 503 to an acknowledgement that can never succeed at any point in
	// the future — the shape ACK-CONTRACT.md §9.3 names as the worst kind of
	// wrong status, because a client can do nothing with it but retry.
	//
	// IT IS REACHED BY DOING THE OBVIOUS THING. `agent-busctl watch` prints the
	// LOCAL message id and does not expose the origin id at all, so the id a
	// recipient has in its hand is precisely the one that must be refused here.
	// (That gap is its own task; this function's job is to answer it correctly,
	// not to make it look like it worked.)
	//
	// The answer is the UNIFORM refusal (§13.3) — identical to a key that was
	// never accepted, a key that was swept, and a key naming somebody else's
	// mail — which is what a key this bus holds no row for has always meant.
	// Folded comparison, matching hub.relayedBusPath's rule for the same reason
	// it uses one: two spellings of one bus id must not be two different buses.
	originBus, _, err := ids.ParseMessageID(req.CorrelationKey)
	if err != nil || strings.EqualFold(originBus, h.busID) {
		return false
	}

	prov, ok := h.store.RelayProvenanceByOriginMessageID(req.CorrelationKey)
	if !ok {
		// Never accepted here, or the MESSAGE has been pruned by retention. The
		// second is the accepted cost the file header names: a transit
		// acknowledgement stops working once the relayed message ages out, and
		// the recipient is then told `unknown`.
		return false
	}
	if !prov.Relayed {
		return false
	}

	for _, r := range prov.Recipients {
		if r == req.Recipient {
			return true
		}
	}
	// A broadcast reaches here with an empty recipient list and is therefore
	// refused, which is the right answer: a broadcast has no canonical audience
	// under signing format v1, so there is no (message, recipient) pair for any
	// party to acknowledge.
	return false
}

// validateRecipientAck applies every rule decidable without the table.
//
// It duplicates checks relay.ValidatePeerAckRequest already makes on the wire
// frame, DELIBERATELY. This is the last gate before a durable write and the hub
// is reachable by an embedder that never went through an HTTP frame at all; the
// same reasoning ErrUnknownSender records ("checked anyway, fail-closed: this is
// the last gate before a durable write"). The costs are three map lookups and
// the failure mode of NOT doing it is a forged claim in an append-only trail.
func validateRecipientAck(req RecipientAckRequest) error {
	switch req.Outcome {
	case ack.StateDelivered:
		if req.Class != "" {
			// §5.4: a positive terminal has nothing to explain, and an optional
			// class on it would create a channel where none is needed.
			return fmt.Errorf("%w: %s carries no class, and %q was supplied", ErrInvalidAck, req.Outcome, elideAckField(string(req.Class)))
		}
	case ack.StateRefused:
		if !req.Class.RecipientEmitted() {
			// Covers three refusals at once, and all three matter: an empty class
			// (a NACK that explains nothing), a class outside the closed set (a
			// future or corrupt spelling, REJECTED rather than defaulted), and a
			// BUS-EMITTED class (the forgery — a recipient asserting a routing
			// failure it has no standing to assert).
			return fmt.Errorf("%w: %s requires one of the three recipient-emitted classes (%s, %s, %s); %q is not one of them",
				ErrInvalidAck, req.Outcome,
				ack.ClassRecipientRefusedPolicy, ack.ClassRecipientRefusedUndecodable, ack.ClassRecipientRefusedNotAddressed,
				elideAckField(string(req.Class)))
		}
	default:
		// ack.StateUndeliverable lands here with everything else: a recipient may
		// not assert a routing outcome, and the non-terminal states are not
		// declarable at all.
		return fmt.Errorf("%w: a recipient may declare only %s or %s; %s is not a recipient outcome",
			ErrInvalidAck, ack.StateDelivered, ack.StateRefused, req.Outcome)
	}

	// THE IDS ARE VALIDATED HERE, AND A FAILURE IS THE UNIFORM ANSWER, NOT A 400.
	//
	// SECURITY FINDING, 2026-08-16 (ACK-6 security gate, HIGH): without this the
	// malformed case fell through to ack.Store's own validatePair, whose
	// ErrInvalidRecord landed in AcknowledgeDelivery's default arm — so a
	// malformed or ABSENT correlation key answered 500 with two unthrottled ERROR
	// log lines, while a well-formed unknown key answered the uniform `unknown`.
	// That is the FOURTH of the four facts this file promises are
	// indistinguishable, reachable by a merely buggy client that omits the field,
	// and it handed any authenticated agent a zero-prerequisite remote 5xx and
	// ERROR-log driver.
	//
	// The frame validator does NOT cover it and must not be blamed for that:
	// relay.ValidatePeerAckRequest deliberately validates no ids, because the PEER
	// route validates them inside AuthorizePeerAck, in the same call that binds
	// them. This route has no AuthorizePeerAck, so the substitute belongs here.
	//
	// ErrAckNotRetained rather than ErrInvalidAck is deliberate and copies
	// ack.Store.Lookup's posture exactly: a malformed key is a MISS, not a fourth
	// distinguishable case, because a caller that could tell "malformed" from
	// "unknown" would learn which of its guesses were even well-formed. It costs
	// nothing to the honest client — a malformed key names no row on any bus.
	if err := ack.ValidateCorrelationKey(req.CorrelationKey); err != nil {
		return fmt.Errorf("%w: the correlation key is not one this bus could have minted: %v", ErrAckNotRetained, err)
	}
	if _, _, _, err := ids.ParseAgentID(req.Recipient); err != nil {
		// The principal is server-minted, so this is a should-not-happen — and it
		// is checked anyway, fail-closed, for the reason ErrUnknownSender records:
		// this is the last gate before a durable write. It answers the SAME
		// uniform refusal rather than a distinct error, so an embedder passing a
		// malformed principal learns nothing a caller could not already deduce.
		return fmt.Errorf("%w: the recipient is not a well-formed fully-qualified agent id: %v", ErrAckNotRetained, err)
	}

	if req.AttestedBy != ack.AttestedByRecipientSignatureUnverified {
		// The only label a recipient's own acknowledgement can carry. `peer_bus`
		// means RequirePeerPrincipal's certificate check ran, which it did not,
		// and there is deliberately no value meaning "verified" because nothing
		// in this system can produce one (§6.3, §16 Q1).
		return fmt.Errorf("%w: a recipient acknowledgement is attested %s and nothing else; %q was offered",
			ErrInvalidAck, ack.AttestedByRecipientSignatureUnverified, elideAckField(string(req.AttestedBy)))
	}
	return nil
}

// maxAckFieldChars bounds what an error string and an operator log line may
// carry back from a caller-chosen value. It matches internal/ack's own elide
// bound; the two are the same rule applied at two layers.
const maxAckFieldChars = 64

// elideAckField bounds an untrusted value on its way into an error message.
// Nothing here is persisted, but an error string reaches a log line and a
// response body, and an unbounded echo of attacker-chosen input into either is
// the expansion internal/ack's elide exists to prevent.
func elideAckField(s string) string {
	if len(s) <= maxAckFieldChars {
		return s
	}
	return s[:maxAckFieldChars] + "...(elided)"
}
