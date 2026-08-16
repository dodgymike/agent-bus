package hub

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
)

// RelayedIngestRequest is one message arriving from a PEER BUS, as the relay
// ingress hands it over.
//
// EVERY FIELD IS PEER-SUPPLIED INPUT TO BE VALIDATED, never an identity or an
// assignment to be trusted (invariant 1). What this bus contributes is the
// sequence, the message id, the acceptance clock and the final hop of the bus
// path; nothing a peer says can choose any of them.
//
// It deliberately mirrors relay.RelayedMessage field for field where the two
// overlap, WITHOUT this package importing internal/relay: the dependency runs
// the other way (the wiring site imports both), and a hub that imported the
// relay would put the transport's vocabulary inside the durability layer.
type RelayedIngestRequest struct {
	// Sender is the ORIGIN agent's fully-qualified "<bus-id>.<agent-id>", and
	// its bus half is NOT this bus (invariant 2). See checkRelayedSender.
	Sender string

	// Recipients are the fully-qualified recipients of the message, exactly as
	// the origin addressed them. At least one is required.
	//
	// A relayed BROADCAST is not representable here, and that is deliberate
	// rather than an omission: signing format v1 has no canonical audience for
	// one, so relay.Acceptor refuses it at ingest, and a bool on this struct
	// would be a way to deliver to every agent on this bus with NO recipient
	// having been roster-checked.
	Recipients []string

	// Body is the opaque payload. The bounds are publish's, unchanged.
	Body []byte

	// OriginMessageID is the id the ORIGIN bus minted for this message, and it
	// does THREE jobs. It is ONE field rather than three because they must not be
	// able to disagree: two fields carrying one fact is two fields that can drift.
	//
	//  1. IT IS THE IDEMPOTENCY KEY. relay.RelayedMessage.Scope() keys on exactly
	//     this, which is what makes two copies of one message arriving by two
	//     disjoint routes land on ONE applied-key scope, so the second resolves as
	//     idem.OutcomeRetry rather than becoming a second message (invariant 10).
	//  2. IT CARRIES THE ORIGIN SEQUENCE, parsed out of it here.
	//  3. WITH THE SEQUENCE, IT IS THE ASSIGNMENT THE SIGNATURE WAS MADE OVER, and
	//     therefore the assignment the audit record's content hash must be
	//     computed under — see signedAs in audit.go.
	//
	// It is NEVER this message's identity here: this bus mints its own id and
	// never adopts a peer's (invariant 1).
	OriginMessageID string

	// OriginAttestation is the ORIGIN bus's signed binding of Sender to Sender's
	// messaging public key, exactly as it arrived. It is MANDATORY on this path
	// and is carried straight to the durable record (store.Message.OriginAttestation).
	//
	// # WHY THE DURABILITY LAYER NEEDS IT AT ALL (RELAY-48)
	//
	// Because the ONWARD hop is rebuilt from durable state after a restart, and
	// this is the one thing in that envelope this bus cannot regenerate: minting
	// an attestation for an agent in ANOTHER bus's namespace is precisely what
	// invariant 2 and the federation-trust design forbid. Without it a pending
	// onward job is settled ABANDONED at the next boot — after this bus already
	// answered the upstream peer 200.
	//
	// # IT IS NOT VERIFIED HERE, AND MUST NOT BE
	//
	// It has ALREADY been verified, against the origin bus's peering-time pin, by
	// relay.ValidateRelayRequest before a relay.RelayedMessage existed at all. The
	// pins live in the relay peer store and deliberately never reach this package.
	// What store.WithRelayOrigin re-checks on the way to disk is SHAPE and
	// BINDING-TO-SENDER — the same posture this package takes to Signature.
	//
	// An absent or malformed one FAILS THE INGEST, before anything is written.
	// That is the fail-closed direction on purpose: refusing a message the peer
	// will retry is a smaller failure than accepting an obligation we have
	// already made ourselves unable to keep.
	OriginAttestation attest.Attestation

	// BusPath is the path AS RECEIVED: origin-first, and NOT yet including this
	// bus. It is the value relay.RelayedMessage.BusPath carries, handed over
	// unchanged, and IngestRelayed appends this bus's own hop before the record
	// is built.
	//
	// # THE TWO PATHS, AND WHY THIS ONE IS THE RECEIVED ONE
	//
	// store.NewMessageWithBusPath documents the trap: an ingest holds the path a
	// peer ASSERTED and the path this bus is WILLING TO SWEAR TO, and they differ
	// by exactly one hop. Taking the received path here leaves the caller holding
	// ONE path — the same one it must hand to relay.Forward — so the two cannot
	// be transposed. Appending our hop is done in one place, below.
	//
	// It is the ONLY provenance this bus records, so an empty path is REFUSED
	// rather than defaulted: defaulting would durably claim the message
	// originated here (see relayedBusPath).
	BusPath []string

	// TimestampUnixMilli is the ORIGIN SENDER's signed clock reading. It is
	// PROVENANCE and is recorded as such — it is NOT this bus's acceptance time
	// and never becomes store.Message.SentAt, which publish stamps from the
	// bus's own clock. See relay.RelayedMessage.TimestampUnixMilli: SentAt
	// decides message VISIBILITY against an agent's enrolment instant, so a peer
	// able to choose it could backdate a message out of every local agent's view
	// or forward-date it into a later agent's.
	TimestampUnixMilli int64

	// Signature is the origin agent's detached Ed25519 signature over the
	// canonical bytes. This bus checks its LENGTH (store.NewMessageWithBusPath)
	// and never verifies it — verification belongs to the relay ingress, which
	// holds the origin bus's attested key. It is carried so the local record is
	// signed-by-construction like every other message.
	Signature []byte
}

// RelayedIngestResult is what the local bus reports about a relayed message it
// was asked to record.
type RelayedIngestResult struct {
	// MessageID is the id THIS bus minted in its OWN namespace (invariant 1) —
	// never the origin's, which is carried as the idempotency key. On a retry it
	// is the ORIGINAL local id, replayed verbatim.
	//
	// It is EMPTY when, and only when, the error is non-nil.
	MessageID string

	// Seq is the local sequence half of MessageID, and on a retry it is the
	// ORIGINAL one.
	Seq uint64

	// Outcome is what the applied-key table said, UNCOLLAPSED (invariant 10):
	// new, retry and violation are three different answers and the caller needs
	// all three — relay re-forwards on exactly ONE of them.
	//
	// # THE ZERO VALUE IS idem.OutcomeNew, SO CHECK THE ERROR FIRST
	//
	// "New" is the answer that RE-FORWARDS, and it is what a zero
	// RelayedIngestResult claims. This method therefore returns it only where it
	// is true:
	//
	//   - err == nil: Outcome is the real answer, idem.OutcomeNew or
	//     idem.OutcomeRetry, and MessageID names the message either way.
	//   - err wraps ErrIdempotencyKeyReused: Outcome is idem.OutcomeViolation.
	//     The violation is reported BOTH ways on purpose — invariant 10's third
	//     case must not collapse into either of the other two, and a caller may
	//     classify it by sentinel or by value.
	//   - any other err: NOTHING was applied, MessageID is empty and Outcome is
	//     not an answer. There is no "unknown" member of idem.Outcome to return
	//     instead, which is precisely why the error is the thing to check.
	Outcome idem.Outcome
}

// IngestRelayed durably records a message this bus accepted from a PEER BUS and
// reports what the applied-key table said.
//
// It is the local half of the relay ingress — relay.LocalIngest.AcceptRelayed —
// expressed in this package's own vocabulary so that internal/relay and
// internal/hub stay independent of each other.
//
// # IT RETURNS ONLY ONCE THE MESSAGE IS COMMITTED AND FSYNCED (invariant 4)
//
// The relay handler's 200 IS an acknowledgement to the peer, and nothing may be
// acknowledged before it is durable. This is the SAME two-phase write path a
// local send takes — publish, one wal.Entry, one fsync, carrying the message,
// its applied-key record and its audit record together — and not a second one.
// A separate durable path for relayed traffic would be a second answer to "have
// I applied this?", and two answers is how a duplicate becomes a second message.
//
// # WHAT IT DOES NOT REQUIRE, AND WHY THAT IS THE POINT
//
// The sender is NOT required to be on this bus's roster; it is required NOT to
// be ours. A relayed message's sender belongs to the origin bus by construction
// (invariant 2), so every exported write path on this hub — Send, Broadcast,
// Mint — refuses it, and there is no mint a peer bus could ever obtain. Those
// two facts are the whole reason this entry point exists.
//
// # WHAT THE CALLER STILL OWES
//
// This is the DURABLE half, not the whole ingress. The caller — relay.Acceptor —
// still owes: signature and attestation verification, relay.CheckIncomingPath on
// the arriving path, the roster check for local recipients (repeated here, since
// this is exported), and the decision to forward onward, which relay makes on
// idem.OutcomeNew alone. Nothing here forwards anything.
//
// # ONE OBLIGATION THIS PACKAGE STRUCTURALLY CANNOT MEET — read before wiring
//
// The applied-key scope built below is (SENDER, idem.OpRelay, origin message id),
// and that sender is PEER-ASSERTED. idem.Store's per-agent fair share is
// documented as safe precisely because its bucket key is a PROVEN identity and
// not an attacker-chosen label — which is true of every other caller and is NOT
// true here. A peer that asserts many distinct sender names takes a growing
// share of the bus-wide applied-key table, and that table fails CLOSED and
// evicts nothing by design, so the end state is local agents refused with
// ErrCapacity for as long as the retention window holds.
//
// THE FIX CANNOT LIVE IN THIS PACKAGE: the only proven identity in a relayed
// request is the AUTHENTICATED PEER, and this package never sees the connection.
// Metering the table by that peer belongs to the wiring site. internal/relay's
// package doc already carries this as a known gap; it is restated here because
// this method is where the unproven label actually enters the table.
//
// ctx is honoured ONLY before any durable work: a request the peer has already
// abandoned is refused with nothing written. It is deliberately NOT threaded
// into the write — a cancellation between the fsync and the return would
// abandon a message that is already this bus's responsibility.
func (h *Hub) IngestRelayed(ctx context.Context, req RelayedIngestRequest) (RelayedIngestResult, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return RelayedIngestResult{}, fmt.Errorf("hub: refusing to ingest a relayed message for a request that is already cancelled; nothing was written: %w", err)
		}
	}
	if err := validateRelayedShape(req); err != nil {
		return RelayedIngestResult{}, err
	}
	if err := h.relayedResultFits(req.Recipients); err != nil {
		return RelayedIngestResult{}, err
	}
	busPath, err := h.relayedBusPath(req.BusPath)
	if err != nil {
		return RelayedIngestResult{}, err
	}
	originSeq, err := h.relayedOrigin(req.Sender, req.OriginMessageID, busPath)
	if err != nil {
		return RelayedIngestResult{}, err
	}

	res, outcome, err := h.publish(publishRequest{
		sender:     req.Sender,
		broadcast:  false,
		recipients: req.Recipients,
		body:       req.Body,
		key:        req.OriginMessageID,
		// THE TIMESTAMP AND THE SIGNATURE ONLY. SignedMint is the shape publish
		// carries these two through for every message, and its other two fields —
		// the message id and the sequence — are a CLIENT PRESENTING BACK A
		// RESERVATION, which a peer bus does not have and must never be able to
		// fake. They are left zero, and publish REFUSES a relayed request that
		// sets either (invariant 1).
		signedMint: SignedMint{
			TimestampUnixMilli: req.TimestampUnixMilli,
			Signature:          req.Signature,
		},
		busPath: busPath,
		relayed: true,
		// The origin's assignment, for the audit content hash ONLY (signedAs).
		originMessageID: req.OriginMessageID,
		originSeq:       originSeq,
		// The origin's attestation, for the DURABLE RECORD (RELAY-48). Unlike the
		// two fields above it is not an audit input at all: it never reaches
		// wal.AuditRecord, which hub's auditRecordFor assembles field by field.
		originAttestation: req.OriginAttestation,
	})
	if err != nil {
		if outcome == idem.OutcomeViolation {
			// REJECTED AND LOGGED, AND NOBODY IS DISCONNECTED — invariant 10's
			// third case in full. The logging half is done HERE rather than left
			// to the caller: this method is exported, and a violation that only
			// the relay handler logs is a violation that goes unrecorded for any
			// other caller. It is a Warn, not an Error: one peer sent us two
			// different messages under one origin id, which is that peer's bug or
			// that peer's mischief, and this bus is unharmed either way.
			//
			// The origin message id is safe to name: it has parsed as a message
			// id, so it is bounded and charset-checked. The two bodies are not
			// named — the caller knows its own, and the other belongs to a
			// message it may have no business seeing quoted back.
			h.log.Warn("a relayed message was REFUSED as an idempotency violation: this origin message id was already applied here with DIFFERENT content. Nothing was written and nobody is disconnected (invariant 10)",
				"origin_message_id", req.OriginMessageID,
				"sender", req.Sender,
				"origin_bus", busPath[0],
			)
		}
		// The outcome is carried out on the error path too, because ONE error is
		// also an outcome: a key reused with a different payload is invariant
		// 10's violation, and it must not reach the caller looking like the zero
		// value, which claims idem.OutcomeNew and re-forwards.
		return RelayedIngestResult{Outcome: outcome}, err
	}
	return RelayedIngestResult{MessageID: res.MessageID, Seq: res.Seq, Outcome: outcome}, nil
}

// validateRelayedShape refuses a request that store.NewMessageWithBusPath would
// refuse, BEFORE the write path allocates anything.
//
// # Why the bounds are re-checked here rather than left to the constructor
//
// They are not re-checked for safety — the constructor still applies them and is
// still the authority — but for WHERE THE REFUSAL LANDS. On the relayed path the
// local sequence is minted INSIDE publish, a few lines above the constructor, so
// a request that dies in the constructor has already burned a number. A burned
// number is not damage (a gap is correct, and invariant 1 would rather have a gap
// than a reissue), but it is a number chosen by a PEER: without this, a peer could
// burn one per malformed request, and one fsync of the sequence-floor file per
// 256 of them, at no cost to itself. A refusal must cost this bus nothing durable.
//
// Each bound is taken from the SAME constant the constructor uses, so the two
// cannot drift into disagreeing about what is legal.
func validateRelayedShape(req RelayedIngestRequest) error {
	if len(req.Recipients) == 0 {
		// The same refusal relay.Acceptor makes, repeated because this method is
		// exported: a directed message naming nobody would be durable, invisible
		// and undeliverable. (A relayed BROADCAST is not representable at all —
		// RelayedIngestRequest has no flag for one.)
		return fmt.Errorf("%w: a relayed message must name at least one recipient", ErrInvalidRecipient)
	}
	if len(req.Recipients) > store.MaxRecipients {
		return fmt.Errorf("%w: %d recipients, the limit is %d", ErrInvalidRecipient, len(req.Recipients), store.MaxRecipients)
	}
	// DUPLICATES ARE REFUSED, NOT COLLAPSED — the same answer signing.Canonicalize
	// gives ("collapsing would change the recipient set the sender signed"), made
	// here so the refusal lands before the sequence is minted rather than inside
	// auditRecordFor afterwards. internal/relay dedupes upstream, so this is the
	// direct-caller belt to that braces, exactly like the broadcast and sender
	// rules this method repeats.
	seen := make(map[string]struct{}, len(req.Recipients))
	for i, r := range req.Recipients {
		// THE LENGTH BOUND COMES FIRST, and it is the reason this loop indexes.
		//
		// publish parses every recipient with ids.ParseAgentID — but publish runs
		// AFTER this function, so nothing here may assume a recipient is bounded.
		// Both of the checks below consume the raw string: the duplicate refusal
		// would echo it with %q (measured at 4x amplification on a megabyte of
		// control bytes) and relayedResultFits JSON-marshals it (6x on invalid
		// UTF-8). ids.ParseAgentID refuses to produce exactly that amplification
		// in its own oversized case, and this is the same rule at the one place
		// that reaches these strings before it does.
		//
		// Unreachable through the wired relay path, which parses every recipient
		// at request validation — but this method is EXPORTED, and every other
		// rule repeated in this function is repeated for that same reason.
		if len(r) > ids.MaxAgentIDLen {
			// NOT echoed, by index instead: it is oversized, and no such string
			// is a valid agent id whatever it contains.
			return fmt.Errorf("%w: recipient %d is %d bytes, but an agent id is at most %d; it is not echoed here because it is oversized", ErrInvalidRecipient, i, len(r), ids.MaxAgentIDLen)
		}
		if _, dup := seen[r]; dup {
			// Safe to echo now: the bound above caps it at one agent id's worth,
			// and it is quoted rather than concatenated.
			return fmt.Errorf("%w: recipient %q appears twice; duplicates are rejected rather than collapsed, because collapsing would change the recipient set the sender signed", ErrInvalidRecipient, r)
		}
		seen[r] = struct{}{}
	}
	if req.TimestampUnixMilli <= 0 {
		// 0 means "unset" and a negative value is a pre-1970 clock the canonical
		// format refuses to validate. The origin's clock is PROVENANCE — it never
		// becomes this bus's SentAt — but a record carrying an unusable one could
		// never be canonicalized and so could never be audited.
		return fmt.Errorf("%w: the origin timestamp %d is not a positive Unix millisecond value", ErrInvalidRelayedMessage, req.TimestampUnixMilli)
	}
	if len(req.Signature) != signing.SignatureSize {
		// The signature is NOT echoed: it is peer-chosen bytes headed for a log
		// line, and its LENGTH is the whole of what was wrong. This bus checks the
		// length and never verifies — verification needs the origin bus's attested
		// key and belongs to internal/relay, which does it before calling here.
		return fmt.Errorf("%w: the signature is %d bytes, but every message carries a detached Ed25519 signature of exactly %d", ErrInvalidRelayedMessage, len(req.Signature), signing.SignatureSize)
	}
	return nil
}

// checkRelayedSender enforces invariant 2 INVERTED: the sender of a relayed
// message must be a well-formed fully-qualified id whose bus half is NOT ours.
//
// # Why the inverse, and why the fold
//
// A peer may assert ids in ITS OWN namespace and nowhere else. An id claiming
// OUR namespace, admitted by anything other than our roster, is a PERMANENT
// id-space injury and not merely a wrong record: cmd/agent-bus/suffixfloors.go
// derives each short name's suffix floor from the sender and recipients of every
// recovered message, so one durable record whose sender is
// "<local-bus>.alpha-18446744073709551615" burns the name "alpha" on this bus
// for ever, across every future restart, with nothing to roll back because the
// log is append-only (invariant 6).
//
// The comparison is a CASE-FOLD, which is deliberately WIDER than the exact
// comparison the floor derivation uses: a confusable spelling of our own bus id
// is not routable (relay.Registry.Route refuses any bus half that folds to
// ours), so admitting one would durably record a message from somebody no part
// of this system will ever be able to name. Each side errs towards its own safe
// answer, and the safe answer here is to refuse. It is the identical rule
// relay.Acceptor.Accept applies to the sender, made again here because this
// method is EXPORTED and a wiring site could call it directly.
//
// The sender is NOT echoed on either arm: on the parse failure it is unbounded
// caller input on its way to a log line, and on the other the diagnosis does not
// need it.
func (h *Hub) checkRelayedSender(sender string) error {
	senderBus, _, _, err := ids.ParseAgentID(sender)
	if err != nil {
		return fmt.Errorf("%w: a relayed message's sender must be a well-formed fully-qualified id (invariant 2): %s", ErrRelayedSender, err)
	}
	if strings.EqualFold(senderBus, h.busID) {
		return fmt.Errorf("%w: it claims this bus's namespace, but a relayed message's sender belongs to the ORIGIN bus and only this bus's roster may admit an id of ours (invariant 2)", ErrRelayedSender)
	}
	return nil
}

// checkRelayedRecipient adjudicates ONE recipient of a relayed message. It is
// the relay path's replacement for the local send's roster-or-router rule, and
// it differs from it in exactly one direction.
//
// TWO RULES, AND THE SPLIT IS THE BUS-ID FOLD:
//
//   - A recipient CLAIMING OUR NAMESPACE must be spelled exactly like our bus id
//     AND be on our roster. The roster is the only authority on our own
//     namespace, it is asked HERE — before the durable write, which is the whole
//     of finding cca64afd — and a confusable spelling is refused rather than
//     treated as somebody else's to route.
//
//   - A FOREIGN recipient is accepted WITHOUT asking whether anyone can route
//     it. This is where the relay path deliberately differs from Send: a local
//     client sending to an unroutable id gets ErrUnknownRecipient because
//     nothing could ever carry it, but a relayed message is ALREADY this bus's
//     responsibility — a leaf bus with no onward peer still records it durably,
//     and whether it travels further is relay.Acceptor's separate decision. Both
//     packages already say so; refusing here would make a transit ingest fail on
//     a bus configured with no route.
//
// Nothing durable is written for a refusal, which is what makes the refusal
// cheap and the injury it prevents impossible.
func (h *Hub) checkRelayedRecipient(recipient string) error {
	bus, _, _, err := ids.ParseAgentID(recipient)
	if err != nil {
		// Not echoed: unvalidated input headed for a log line.
		return fmt.Errorf("%w: every recipient of a relayed message must be a well-formed fully-qualified id (invariant 2): %s", ErrInvalidRecipient, err)
	}
	if !strings.EqualFold(bus, h.busID) {
		return nil
	}
	if bus != h.busID || !h.Enrolled(recipient) {
		// The id IS echoed: it has passed ids.ParseAgentID, so it is bounded and
		// charset-checked, and it is what makes the line actionable — this is the
		// moment a peer with a stale roster and a peer probing our namespace look
		// identical.
		return fmt.Errorf("%w: %q claims this bus's namespace but this bus's roster does not hold it; nothing was written", ErrUnknownRecipient, recipient)
	}
	return nil
}

// relayedBusPath validates the path a relayed message arrived with and returns
// the path to RECORD: the received hops, origin-first, with this bus appended as
// the final hop.
//
// The result is ALWAYS A FRESH SLICE. It never appends into the caller's backing
// array, because that array may be one decoded from a peer's payload and may
// still be read by the onward forward, which needs the path AS RECEIVED.
//
// Three refusals, and each one is a durable-record problem rather than a
// tidiness rule:
//
//   - EMPTY. A message that arrived from a peer has necessarily traversed its
//     origin, so an empty path is a path that was LOST. Defaulting it to this bus
//     would durably assert the message originated here — a provenance claim
//     nobody made, indistinguishable afterwards from a genuine local send.
//   - TOO LONG once our hop is appended. store.MaxBusPath is the durable bound
//     and the constructor enforces it; checking it here means the number is never
//     allocated for a record that cannot be built.
//   - ALREADY CONTAINS US. Appending our hop to a path we are already on would
//     fabricate a SECOND visit in an append-only trail. relay.CheckIncomingPath
//     is the authority on the loop — it refuses the request outright, before this
//     is ever reached — and relay.AppendHop refuses the same fabrication at
//     egress; this is the same rule at the moment the record is built, so an
//     exported entry point cannot be used to write a path that never happened.
//     The fold is used for the same reason it is in checkRelayedSender.
func (h *Hub) relayedBusPath(received []string) ([]string, error) {
	if len(received) == 0 {
		return nil, fmt.Errorf("%w: a relayed message carries the path it travelled, so an empty one is a path that was lost; recording it as this bus alone would claim the message originated here", ErrInvalidBusPath)
	}
	if len(received) > store.MaxReceivedBusPath {
		return nil, fmt.Errorf("%w: the message arrived with %d hops, but a received path carries at most %d so this bus can append itself within the %d-hop durable limit", ErrInvalidBusPath, len(received), store.MaxReceivedBusPath, store.MaxBusPath)
	}
	for i, hop := range received {
		if err := ids.ValidateBusID(hop); err != nil {
			// The hop is NOT echoed — it has just failed validation, so it is
			// unbounded peer-supplied input — but its INDEX is, which is what
			// makes the refusal diagnosable.
			return nil, fmt.Errorf("%w: hop %d of the arriving path is not a valid bus id: %s", ErrInvalidBusPath, i, err)
		}
		if strings.EqualFold(hop, h.busID) {
			return nil, fmt.Errorf("%w: this bus already appears at hop %d of the %d-hop path the message arrived with, so appending our own hop would fabricate a second visit", ErrBusPathLoop, i, len(received))
		}
	}
	out := make([]string, 0, len(received)+1)
	out = append(out, received...)
	out = append(out, h.busID)
	return out, nil
}

// widestStoredResultSentAt is the longest RFC3339Nano rendering this bus can
// produce: nanosecond precision with no trailing zero to trim. It is a WORST
// CASE for a size probe, never a value anything records.
var widestStoredResultSentAt = time.Unix(0, math.MaxInt64).UTC()

// relayedResultFits refuses a relayed message whose APPLIED-KEY RESULT would not
// fit in idem.MaxResultBytes — BEFORE the sequence is minted.
//
// # The bound this is standing in front of, and why it only bites here
//
// The applied-key record stores the result a retry is replayed with, and that
// includes THE RECIPIENT LIST (storedResult). idem.MaxResultBytes caps it at 512
// bytes. On the LOCAL paths that cap is unreachable: Send carries exactly one
// recipient and Broadcast carries none. A RELAYED message is the first thing on
// this bus that can carry many — up to store.MaxRecipients (64) — so this is the
// first path on which the two bounds disagree, and they do: with
// production-length ids the record stops fitting at roughly a dozen recipients,
// far below 64.
//
// Without this check the refusal arrives from idem.Record.Encode, INSIDE publish
// and AFTER mintRelayedSeqLocked. The consequences are what make it worth a
// separate check rather than a comment: it is not an idempotency violation, so
// relay.Acceptor maps it to 503 — "not now" — and an HONEST peer retries a
// message that can never be accepted, deterministically, for ever, burning one
// sequence number per attempt and one sequence-floor fsync per 256.
//
// THIS IS A WORKAROUND AND IS LABELLED AS ONE. Refusing early makes the answer
// immediate and free instead of slow and expensive — but it does NOT make it
// honest, and the difference is worth stating: relay.Acceptor maps everything
// that is not an idempotency violation to 503 "not now", so the peer is still
// told to retry a message that can never be accepted. It merely stops costing us
// a sequence number each time. The message is LEGITIMATE and this bus still will
// not deliver it; a permanent-refusal class in the relay error mapping belongs
// with the real fix below.
//
// THE REAL FIX is to reconcile idem.MaxResultBytes with store.MaxRecipients —
// raise the cap with the retention-budget maths redone, or stop storing the
// recipient list in the result — which is internal/idem's to make. Tracked as
// IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS. Do not read this function as the
// multi-recipient case being handled.
//
// It probes with the REAL encoder rather than an estimate, under the WIDEST id,
// sequence and timestamp this bus could later produce, so the check can never
// admit something the real encoding then refuses.
func (h *Hub) relayedResultFits(recipients []string) error {
	widestID, err := ids.MessageID(h.busID, math.MaxUint64)
	if err != nil {
		return fmt.Errorf("hub: sizing the applied-key result for a relayed message: %w", err)
	}
	probe, err := encodeStoredResult(widestID, math.MaxUint64, recipients, widestStoredResultSentAt)
	if err != nil {
		return fmt.Errorf("hub: sizing the applied-key result for a relayed message: %w", err)
	}
	if len(probe) > idem.MaxResultBytes {
		// The recipients are NOT echoed — there may be 64 of them and this is a
		// log line — but the counts are, because they are the whole diagnosis.
		return fmt.Errorf("%w: its %d recipients need %d bytes of applied-key result, but a stored result is at most %d; the message is well-formed and this is a limit of THIS bus, so retrying it unchanged cannot succeed",
			ErrInvalidRelayedMessage, len(recipients), len(probe), idem.MaxResultBytes)
	}
	return nil
}

// relayedOrigin validates the ORIGIN's assignment against the rest of the
// message and returns the origin sequence.
//
// # Why this is checked HERE and not left to the constructor
//
// The origin message id is the only thing that ties this bus's record to the
// bytes the origin agent SIGNED, and it goes into an append-only trail. Three
// facts have to agree, and each disagreement produces a durable record that is
// permanently self-contradictory rather than merely wrong:
//
//   - it must PARSE as a message id. It is also the idempotency key, so a
//     malformed one is reported as ErrInvalidIdempotencyKey — a relayed message's
//     key IS an origin message id, and there is no second thing it could be.
//   - its bus half must be THE SENDER'S BUS. signing.Canonicalize enforces this
//     too ("a message is signed by an agent of the bus that minted its id") and
//     would refuse to produce the content hash, failing the write with nothing
//     written; checked here so the refusal names the mismatch rather than
//     surfacing as an unsignable message.
//   - it must be the FIRST HOP OF THE PATH. The path is origin-first, so
//     BusPath[0] is where the message was accepted from its own agent, and that
//     is the bus that minted this id. A record whose provenance and whose signed
//     assignment name different origins can never be reconciled afterwards.
//
// busPath is the path as it will be RECORDED (this bus appended), so its first
// element is still the origin hop.
func (h *Hub) relayedOrigin(sender, originMessageID string, busPath []string) (uint64, error) {
	originBus, originSeq, err := ids.ParseMessageID(originMessageID)
	if err != nil {
		// NOT echoed: it failed to parse, so it is unbounded caller input.
		return 0, fmt.Errorf("%w: a relayed message's idempotency key IS the id the origin bus minted, and must parse as one: %s", ErrInvalidIdempotencyKey, err)
	}
	senderBus, _, _, err := ids.ParseAgentID(sender)
	if err != nil {
		// checkRelayedSender says this properly, inside publish, with nothing
		// written. Reaching it here first would only duplicate that refusal, so
		// this defers rather than competing with it.
		return 0, fmt.Errorf("%w: %s", ErrRelayedSender, err)
	}
	if originBus != senderBus {
		// EXACT, NOT A FOLD, and this is the one comparison on this path that must
		// be exact. Every other bus-id comparison here folds because it is asking
		// "is this OURS?", where the wider answer is the safe one. This one asks
		// "will signing.Canonicalize accept the pair?", and that check
		// (internal/signing/canonical.go, `senderBus != originBus`) is EXACT and
		// unconditional. A fold here is therefore strictly more permissive than the
		// thing it is standing in for: "BUSORIGIN-100" with sender
		// "busorigin.alpha-1" would pass, mint a sequence, and then fail in
		// auditRecordFor with the number already burned — the exact cost this
		// function's own doc promises to avoid.
		//
		// Both halves have passed their own validators, so both are bounded and
		// safe to name — and naming them is the whole diagnosis.
		return 0, fmt.Errorf("%w: the origin message id was minted by bus %q but the sender belongs to bus %q; a message is signed by an agent of the bus that minted its id", ErrRelayedSender, originBus, senderBus)
	}
	if !strings.EqualFold(busPath[0], originBus) {
		return 0, fmt.Errorf("%w: the path says the message originated on bus %q but its message id was minted by bus %q; the two name different origins and the record would be permanently self-contradictory", ErrInvalidBusPath, busPath[0], originBus)
	}
	return originSeq, nil
}

// mintRelayedSeqLocked allocates THIS bus's sequence and message id for a
// relayed message. Caller holds writeMu.
//
// It is Hub.Mint's allocation half, minus the reservation: the number is burned
// durably BEFORE it is used (ensureSeqFloorLocked), allocated, and then asserted
// to sit at or below the proven floor — the same three steps in the same order,
// for the same reason. A number handed out before it is durably burned is a
// number a restart can hand out again (invariant 1).
//
// It does NOT enter the number into h.mints and does NOT count against any
// agent's outstanding-mint share: there is no client waiting to sign it and
// nothing to spend it later. The number leaves this function and is written into
// the record microseconds later, under the same lock, or it is not used at all.
func (h *Hub) mintRelayedSeqLocked() (uint64, string, error) {
	if err := h.ensureSeqFloorLocked(); err != nil {
		return 0, "", err
	}
	seq, err := h.seq.Next()
	if err != nil {
		return 0, "", fmt.Errorf("hub: allocating a message sequence for a relayed message: %w", err)
	}
	// Asserted at the moment of issue, exactly as Mint does, and again before the
	// durable write in publish. The two assertions are separated by everything
	// between them, which is the entire value of making both.
	if err := h.assertSeqFloorLocked(string(idem.OpRelay), "", seq); err != nil {
		return 0, "", err
	}
	id, err := ids.MessageID(h.busID, seq)
	if err != nil {
		// The sequence is spent either way — invariant 1 forbids reusing it — so
		// this leaves a gap and nothing else. It cannot happen: the bus id was
		// validated in Open and Next never returns 0.
		return 0, "", fmt.Errorf("hub: building the message id for relayed sequence %d: %w", seq, err)
	}
	return seq, id, nil
}
