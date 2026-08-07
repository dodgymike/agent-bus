package relay

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
)

// PeerRelayPath is the path the relay ingress is EXPECTED to occupy once it is
// gated by INVITE-PEERGUARD and MTLS-RELAYGUARD (see the package doc).
//
// It is a CONSTANT, NOT A REGISTRATION, exactly as PeerEnrollPath is: naming
// the path here lets the initiator build a URL and lets the guard tasks assert
// about a path with exactly one spelling, without anything in this package
// attaching a handler to a mux.
const PeerRelayPath = "/v1/peer/relay"

// The relay envelope deliberately carries NO wire-protocol version field, for
// the reason set out in doc.go's "No wire protocol version field, on purpose"
// section: versions are RESERVED through the Spec Server reservations API and
// never chosen by the agent writing the code, none has been reserved, and
// because nothing serves this handler the format is not yet on any wire, so
// there is nothing to stay compatible with. The task that first REGISTERS this
// handler reserves a version and adds the field to BOTH surfaces at once.

// DropLoop is the RelayResponse.DroppedReason for a message dropped because it
// had already traversed this bus (RELAY-3).
//
// It is a stable string on the wire because a sender has to be able to tell
// "settled, stop sending this" apart from "try again later" WITHOUT parsing
// prose, and because the drop arrives with HTTP 200 — see relayhttp.go for why
// a 4xx/5xx there would make retry/backoff amplify exactly the traffic
// loop prevention exists to stop.
const DropLoop = "loop"

// MaxRelayBytes bounds the encoded relay payload read from the network, before
// it is decoded.
//
// It is DERIVED, not guessed, exactly as MaxHandshakeBytes is:
//
//	body        store.MaxBodyBytes (65,536) base64-expanded by 4/3 = 87,384
//	recipients  store.MaxRecipients (64) x (ids.MaxAgentIDLen 150 + 3 for two
//	            quotes and a comma)                                  =  9,792
//	bus_path    MaxBusPath (64) x (64-byte bus id + 3)               =  4,288
//	fixed       origin_bus (64) + message_id (85) + sender (150) +
//	            content_sha256 (64) + sent_at (20) + size (10) +
//	            the flags and every field name                       = ~1,024
//	                                                         total  ~ 102,488
//
// 256 KiB leaves ~2.5x headroom, so a legal maximum-size relayed message can
// always be encoded and can never be rejected by this cap — while an unbounded
// stream still stops at a quarter of a megabyte. TestMaxRelayBytesFitsAMaximumMessage
// pins the derivation, because a bound nothing checks is a description.
const MaxRelayBytes = 256 << 10

// Relay failures. All are checkable with errors.Is; the path sentinels in
// path.go complete the set.
var (
	// ErrInvalidRelay reports a relay envelope that is well-formed JSON but not
	// a coherent message: a mis-parented id, a bad recipient list, a body that
	// does not match its declared size or hash.
	ErrInvalidRelay = errors.New("relay: invalid relay envelope")

	// ErrRelayKeyMismatch reports an idempotency key that is not the origin's
	// message id. See ValidateRelayRequest for why that identity is a PROTOCOL
	// RULE rather than a convention.
	ErrRelayKeyMismatch = errors.New("relay: idempotency key is not the origin message id")
)

// RelayRequest is the body one bus POSTs to another at PeerRelayPath.
//
// EVERY FIELD IS UNTRUSTED, including the ones that name the origin. Nothing
// here proves the sending bus is entitled to speak for OriginBus; proving that
// is INVITE-PEERGUARD's and MTLS-RELAYGUARD's job, and neither exists yet.
//
// The idempotency key is deliberately NOT a field: it travels in the
// idem.HeaderName header, internal/idem's one canonical carrier — and for
// relay it must equal MessageID, which ValidateRelayRequest enforces.
type RelayRequest struct {
	// OriginBus is the bus that ACCEPTED the message from its own agent. It is
	// the authority for MessageID and for Sender's namespace.
	OriginBus string `json:"origin_bus"`

	// MessageID is the ORIGIN's message id "<bus-id>-<seq>" (invariant 1). It
	// is NOT this bus's id for the message: a receiving bus mints its own local
	// delivery sequence outside the relayed envelope (PROTOCOL.md §8.5).
	MessageID string `json:"message_id"`

	// Sender is the fully-qualified "<bus-id>.<agent-id>" of the originating
	// agent (invariant 2), always inside OriginBus's namespace.
	Sender string `json:"sender"`

	// Broadcast reports a message addressed to every agent rather than to named
	// recipients. Exactly one of Broadcast and a non-empty Recipients is set,
	// mirroring store.Decode's two rules.
	Broadcast bool `json:"broadcast"`

	// Recipients are the fully-qualified ids a directed message is addressed
	// to. Empty for a broadcast.
	Recipients []string `json:"recipients,omitempty"`

	// BusPath is the ordered list of bus ids this message has traversed,
	// starting with OriginBus and ending with the bus that is sending it to us.
	// It is loop-prevention and provenance metadata and is NOT covered by any
	// signature — see path.go and PROTOCOL.md §8.5.
	BusPath []string `json:"bus_path"`

	// SentAtUnixNs is the ORIGIN bus's clock reading for the message. It is
	// PROVENANCE, not authorization input — see RelayedMessage.OriginSentAt.
	SentAtUnixNs int64 `json:"sent_at_unix_ns"`

	// Size is the declared body length. It is cross-checked against the actual
	// body, so a lying Size is a rejection rather than an allocation.
	Size int `json:"size"`

	// ContentSHA256 is the hex SHA-256 of Body (store.ContentHash).
	ContentSHA256 string `json:"content_sha256"`

	// Body is the opaque payload, carried verbatim. The bus never interprets
	// it, so the CRYPTO epic can put ciphertext here with nothing on this path
	// changing.
	Body []byte `json:"body"`
}

// RelayResponse is the answer to a relay POST.
//
// It carries a SETTLED outcome even when nothing was accepted, which is the
// whole point: Accepted=false with DroppedReason set means "stop, this can
// never be accepted", and it arrives with HTTP 200 so that a retrying sender
// treats it as final rather than as a transient failure.
type RelayResponse struct {
	// Accepted reports that this bus took responsibility for the message —
	// either by applying it now, or (with Duplicate) by having applied it
	// before.
	Accepted bool `json:"accepted"`

	// Duplicate reports idem.OutcomeRetry: the same key with the SAME payload
	// had already been applied, so the ORIGINAL result is being replayed.
	// Nothing was re-applied and nobody is disconnected (invariant 10).
	Duplicate bool `json:"duplicate"`

	// DroppedReason is "" or one of the Drop* constants. It is set only when
	// Accepted is false and the outcome is final.
	DroppedReason string `json:"dropped_reason,omitempty"`

	// MessageID is the id THIS bus minted for its local copy (invariant 1) —
	// never the origin's. It is empty when nothing was accepted.
	MessageID string `json:"message_id,omitempty"`
}

// RelayedMessage is the VALIDATED outcome of a relay ingress: what this bus is
// entitled to believe about a message a peer handed it.
//
// "Entitled to believe" is bounded exactly as it is for PeerRoster: the ids are
// well-formed, the origin speaks only for its own namespace, the body matches
// its declared size and hash, and the path is well-formed and does not include
// us. Nothing here asserts the sending bus was allowed to send it.
type RelayedMessage struct {
	// OriginBus is the validated bus id of the bus that accepted the message
	// from its own agent. It is not ours and it is BusPath[0].
	OriginBus string

	// OriginMessageID is the origin's "<bus-id>-<seq>" id, whose bus half is
	// OriginBus. It is also the relay idempotency key — see IdempotencyKey.
	OriginMessageID string

	// OriginSeq is the sequence half of OriginMessageID, parsed once here so no
	// consumer re-parses an id we have already validated.
	OriginSeq uint64

	// Sender is the validated fully-qualified sender, inside OriginBus.
	Sender string

	// Broadcast mirrors RelayRequest.Broadcast.
	Broadcast bool

	// Recipients are the validated fully-qualified recipients. Freshly
	// allocated; never aliases the decoded payload.
	Recipients []string

	// BusPath is the validated path AS RECEIVED. It does NOT yet include this
	// bus: appending our hop is Forward's job, and doing it here would make an
	// ingress record disagree with the egress envelope built from it.
	BusPath []string

	// OriginSentAt is the ORIGIN BUS's clock reading, and the field is named
	// OriginSentAt rather than SentAt DELIBERATELY.
	//
	// IT IS UNTRUSTED PEER INPUT AND MUST NEVER BECOME THE LOCAL
	// store.Message.SentAt. store.Message.VisibleTo compares SentAt against an
	// agent's ENROLMENT INSTANT to enforce the enrolment epoch ("you do not
	// receive mail sent before you existed"), which is an AUTHORIZATION
	// boundary. A wiring task that copied this value into the local record
	// would hand a peer the ability to choose a message's visibility: backdate
	// it out of every local agent's view, or forward-date it so it is delivered
	// to agents that enrol later.
	//
	// The local bus MUST stamp its own acceptance time. This value is
	// PROVENANCE — worth recording in the audit trail, worth showing an
	// operator, never an input to a visibility or ordering decision.
	OriginSentAt time.Time

	// Body is the opaque payload, freshly allocated.
	Body []byte

	// ContentSHA256 is the body's hex SHA-256, re-derived and checked against
	// what the peer declared.
	ContentSHA256 string

	// IdempotencyKey is the key the sending bus supplied in idem.HeaderName. It
	// EQUALS OriginMessageID — ValidateRelayRequest refuses the envelope
	// otherwise — which is what makes two copies of one message arriving by two
	// disjoint paths land on one idem.Scope.
	IdempotencyKey string

	// Fingerprint is the canonical fingerprint of the message's
	// IDENTITY-DEFINING content, computed by relayFingerprint. It deliberately
	// EXCLUDES BusPath; read relayFingerprint's comment before changing that.
	Fingerprint idem.Fingerprint
}

// Scope builds the idem.Scope this message must be looked up and remembered
// under: the ORIGIN's sender, idem.OpRelay, and the origin message id as the
// key.
//
// Scoping on the origin's SENDER rather than on the forwarding peer is what
// makes the dedupe work across routes: the same message relayed to us by two
// different peers has the same sender and the same key, so it resolves to one
// scope and the second arrival is an idem.OutcomeRetry.
func (m RelayedMessage) Scope() (idem.Scope, error) {
	return idem.NewAgentScope(m.Sender, idem.OpRelay, m.IdempotencyKey)
}

// relayFingerprint computes the canonical fingerprint of a relayed message.
//
// The field list is FIXED and ordered, as idem.ComputeFingerprint requires
// every call site to document: idem.OpRelay (domain separation, so a relay
// fingerprint can never equal a send's or a peer-enrol's), the origin bus, the
// origin message id, the sender, the broadcast flag as "1"/"0", the size in
// decimal, the content hash, the origin timestamp in decimal, then each
// recipient in order. Recipient ORDER is part of the payload for the same
// reason roster order is in peerFingerprint: two envelopes differing only in
// order are different envelopes, and treating them as equal would let a retry
// quietly re-address a message.
//
// # BusPath MUST NOT BE IN THE FINGERPRINT. THIS IS NOT AN OVERSIGHT.
//
// In a cyclic or meshed topology the SAME message reaches us by more than one
// route, and each copy carries a DIFFERENT bus_path — that is the normal
// steady state, not an edge case. If the path were covered by the fingerprint,
// the second copy would be the same idempotency key with a DIFFERENT
// fingerprint, which is idem.OutcomeViolation. CLAUDE.md invariant 10 mandates
// that a violation is rejected, logged AND THE OFFENDING CLIENT DISCONNECTED.
// So covering the path would make correct peers disconnect each other as the
// ordinary behaviour of a correct mesh — a self-inflicted partition, produced
// by the very mechanism meant to make retries safe, and one that no test of
// the two-node case would ever reveal.
//
// The rule that keeps this coherent: the fingerprint covers the message's
// IDENTITY-DEFINING CONTENT — who sent it, to whom, when, and what — while the
// bus path is PER-COPY ROUTING METADATA that says how this particular copy got
// here. Changing the content is a violation; arriving by another route is not.
func relayFingerprint(originBus, messageID, sender string, broadcast bool, recipients []string, sentAtUnixNs int64, size int, contentSHA256 string) idem.Fingerprint {
	broadcastField := "0"
	if broadcast {
		broadcastField = "1"
	}
	fields := make([][]byte, 0, len(recipients)+8)
	fields = append(fields,
		[]byte(idem.OpRelay),
		[]byte(originBus),
		[]byte(messageID),
		[]byte(sender),
		[]byte(broadcastField),
		[]byte(strconv.Itoa(size)),
		[]byte(contentSHA256),
		[]byte(strconv.FormatInt(sentAtUnixNs, 10)),
	)
	for _, r := range recipients {
		fields = append(fields, []byte(r))
	}
	return idem.ComputeFingerprint(fields...)
}

// ValidateRelayRequest validates a decoded relay envelope against our own bus
// id and returns what we may believe about the message.
//
// # The order of the checks IS the design; each position is security-relevant
//
//  1. The idempotency key's SHAPE, first, because everything after it depends
//     on the key being a legal key.
//
//  2. The PATH — validate and loop-drop — BEFORE any of the per-field work
//     below. A looping message is the expected steady state of a cycle and
//     must cost us as close to nothing as possible: no id parsing, no
//     recipient walk, no hashing of a 64 KiB body. Cheap-drop is the entire
//     value of RELAY-3.
//
//  3. OriginBus must be a well-formed bus id and must not be OURS
//     (ValidatePeerBusID). A peer claiming our namespace is id spoofing. This
//     runs BEFORE the agreement check below because that check QUOTES
//     origin_bus, and an unbounded value must never be quoted — see the check's
//     own comment.
//
//  4. OriginBus must equal BusPath[0]. Both halves are peer-supplied, so this
//     adds no trust — it REMOVES AN AMBIGUITY. With two independent claims
//     about where the message came from, every downstream consumer would have
//     to choose one, and different consumers choosing differently is how a
//     message gets attributed to one bus in the audit log and routed as if it
//     came from another.
//
//  5. MessageID must parse and its bus half must be OriginBus. An origin bus
//     is the authority for its own message ids and for nobody else's
//     (invariant 1).
//
//  6. THE IDEMPOTENCY KEY MUST BE THE ORIGIN MESSAGE ID. See below.
//
//  7. Sender must parse and its bus half must be OriginBus — ValidateRoster's
//     rule ("a bus speaks only for its own agents") applied to a sender.
//
//  8. Recipients: the COUNT before any parsing, then each id, then duplicates,
//     then the broadcast/recipients exclusivity — mirroring store.Decode's two
//     rules exactly, because a message we accept here is a message we go on to
//     persist, and a shape store.Decode would refuse is one we would
//     acknowledge and then fail to recover.
//
//  9. Body: the LENGTH before anything reads it, then the declared size, then
//     the content hash. The body is NEVER echoed in an error: it may be 64 KiB
//     and, once the CRYPTO epic lands, it is ciphertext nobody here can read.
//
//  10. SentAtUnixNs must be positive; 0 is an unset field, not the epoch.
//
// # Why check 6 is a PROTOCOL RULE and not a convention
//
// THE RELAY IDEMPOTENCY KEY IS THE ORIGIN MESSAGE ID. That identity is the
// only reason dedupe works at all in a cycle: it is what makes the same message
// arriving by two disjoint paths — from two different peers, at two different
// times — resolve to the SAME idem.Scope, which is what lets the second arrival
// be recognised as idem.OutcomeRetry instead of being applied a second time. A
// peer free to mint a fresh key per hop would defeat CLAUDE.md invariant 10
// SILENTLY: every copy would look new, every copy would be delivered, and
// nothing in the system would report an error.
//
// The shapes fit, and the fit is asserted by a test rather than assumed:
// ids.MaxMessageIDLen (85) is under idem.MaxKeyLen (128), and a message id's
// charset — a bus id's [A-Za-z0-9_-] plus '-' plus decimal digits — is a subset
// of idem.KeyCharset ([A-Za-z0-9._-]). A future widening of a bus id could
// break that relationship, which is why TestRelayIdempotencyKeyIsTheOriginMessageID
// pins it.
func ValidateRelayRequest(localBusID, idempotencyKey string, req RelayRequest) (RelayedMessage, error) {
	// 1. The key's shape.
	if err := idem.ValidateKey(idempotencyKey); err != nil {
		return RelayedMessage{}, err
	}

	// 2. The path, and the loop drop, before any per-field work.
	if err := CheckIncomingPath(localBusID, req.BusPath); err != nil {
		return RelayedMessage{}, err
	}

	// 3. The origin is a well-formed bus, and is not us.
	//
	// THIS RUNS BEFORE THE PATH-AGREEMENT CHECK BELOW, AND THE ORDER IS THE
	// POINT. The agreement check echoes origin_bus with %q to make the mismatch
	// diagnosable, and %q expands a control byte to four characters — so if it
	// ran first, a peer sending a 200 KiB "origin_bus" would choose the size of
	// the line we log about refusing it. ValidatePeerBusID bounds the value
	// (MaxPeerBusIDLen) before anything quotes it, which is the same discipline
	// ValidateBusPath applies per hop and ids.ParseMessageID applies to an
	// oversized id. Only once origin_bus is known-bounded is it safe to print.
	if err := ValidatePeerBusID(localBusID, req.OriginBus); err != nil {
		return RelayedMessage{}, err
	}

	// 4. One claim about the origin, not two. Both operands are bounded by now:
	// origin_bus by check 3, and every path hop by CheckIncomingPath.
	if req.OriginBus != req.BusPath[0] {
		return RelayedMessage{}, fmt.Errorf("%w: origin_bus is %q but the path starts at %q; the two must agree, so that no consumer has to choose which claim to believe", ErrInvalidRelay, req.OriginBus, req.BusPath[0])
	}

	// 5. The origin's message id belongs to the origin.
	idBus, seq, err := ids.ParseMessageID(req.MessageID)
	if err != nil {
		return RelayedMessage{}, fmt.Errorf("%w: message id: %v", ErrInvalidRelay, err)
	}
	if idBus != req.OriginBus {
		return RelayedMessage{}, fmt.Errorf("%w: message id %q is minted by bus %q, but the envelope claims origin %q; a bus is the authority for its own message ids and for nobody else's (invariant 1)", ErrInvalidRelay, req.MessageID, idBus, req.OriginBus)
	}

	// 6. The key IS the origin message id.
	if idempotencyKey != req.MessageID {
		// Neither string is echoed in full: the key is bounded and validated,
		// but there is no diagnostic value in quoting two ids that differ, and
		// the pair is what an attacker chooses.
		return RelayedMessage{}, fmt.Errorf("%w: the %s header must carry the origin message id verbatim, so that every copy of one message — however it was routed — resolves to ONE idempotency scope; a per-hop key would make each copy look new and would defeat duplicate suppression silently (invariant 10)", ErrRelayKeyMismatch, idem.HeaderName)
	}

	// 7. The sender is one of the origin's own agents.
	senderBus, _, _, err := ids.ParseAgentID(req.Sender)
	if err != nil {
		return RelayedMessage{}, fmt.Errorf("%w: sender: %v", ErrInvalidRelay, err)
	}
	if senderBus != req.OriginBus {
		return RelayedMessage{}, fmt.Errorf("%w: sender %q belongs to bus %q, but bus %q may only speak for its own agents", ErrInvalidRelay, req.Sender, senderBus, req.OriginBus)
	}

	// 8. Recipients: the count first, so a hostile envelope cannot make us
	// parse MaxRecipients+N ids before we decline.
	if len(req.Recipients) > store.MaxRecipients {
		return RelayedMessage{}, fmt.Errorf("%w: %d recipients, but a message carries at most %d", ErrInvalidRelay, len(req.Recipients), store.MaxRecipients)
	}
	seen := make(map[string]struct{}, len(req.Recipients))
	for i, r := range req.Recipients {
		if _, _, _, err := ids.ParseAgentID(r); err != nil {
			return RelayedMessage{}, fmt.Errorf("%w: recipient %d: %v", ErrInvalidRelay, i, err)
		}
		if _, dup := seen[r]; dup {
			return RelayedMessage{}, fmt.Errorf("%w: recipient %d is %q, which appears more than once; a recipient list is a set", ErrInvalidRelay, i, r)
		}
		seen[r] = struct{}{}
	}
	if req.Broadcast && len(req.Recipients) != 0 {
		return RelayedMessage{}, fmt.Errorf("%w: a broadcast carries no recipient list, but this envelope has %d", ErrInvalidRelay, len(req.Recipients))
	}
	if !req.Broadcast && len(req.Recipients) == 0 {
		return RelayedMessage{}, fmt.Errorf("%w: a directed message must name at least one recipient", ErrInvalidRelay)
	}

	// 9. The body, its declared size, and its hash.
	if len(req.Body) > store.MaxBodyBytes {
		return RelayedMessage{}, fmt.Errorf("%w: body is %d bytes, the limit is %d", ErrInvalidRelay, len(req.Body), store.MaxBodyBytes)
	}
	if req.Size != len(req.Body) {
		return RelayedMessage{}, fmt.Errorf("%w: the envelope declares a %d-byte body but carries %d", ErrInvalidRelay, req.Size, len(req.Body))
	}
	if got := store.ContentHash(req.Body); got != req.ContentSHA256 {
		// The BODY is not echoed — see the doc above. The two hashes are fixed
		// length and are the whole diagnosis.
		return RelayedMessage{}, fmt.Errorf("%w: content hash mismatch: the envelope asserts %q, the body hashes to %q", ErrInvalidRelay, req.ContentSHA256, got)
	}

	// 10. A timestamp of 0 is an unset field, not the epoch.
	if req.SentAtUnixNs <= 0 {
		return RelayedMessage{}, fmt.Errorf("%w: sent_at_unix_ns is %d; a message always carries the origin's clock reading, and 0 means the field was never set", ErrInvalidRelay, req.SentAtUnixNs)
	}

	// 11. Build the record. EVERY SLICE IS COPIED: nothing may alias the
	// decoded payload, because the decoder's buffers are the sending peer's
	// bytes and a consumer that outlives the request must not be reading
	// memory somebody else may still hold.
	return RelayedMessage{
		OriginBus:       req.OriginBus,
		OriginMessageID: req.MessageID,
		OriginSeq:       seq,
		Sender:          req.Sender,
		Broadcast:       req.Broadcast,
		Recipients:      append([]string(nil), req.Recipients...),
		BusPath:         append([]string(nil), req.BusPath...),
		OriginSentAt:    time.Unix(0, req.SentAtUnixNs).UTC(),
		Body:            append([]byte(nil), req.Body...),
		ContentSHA256:   req.ContentSHA256,
		IdempotencyKey:  idempotencyKey,
		Fingerprint: relayFingerprint(req.OriginBus, req.MessageID, req.Sender, req.Broadcast,
			req.Recipients, req.SentAtUnixNs, req.Size, req.ContentSHA256),
	}, nil
}

// Forward re-encodes the message for onward relay, with THIS bus's hop appended
// to the path.
//
// Every other field is carried VERBATIM. PROTOCOL.md §8.5: "a relay must
// forward the signed bytes verbatim — any normalisation on the path breaks
// verification at the far end". Nothing here re-derives, re-orders, re-hashes
// or otherwise touches a field that a signature covers or will cover; the ONLY
// thing that changes on a hop is the bus path, which is the one field that is
// outside the signature and can never be inside it.
//
// An EMPTY m.BusPath means this bus is the ORIGIN — the message has traversed
// nothing yet — and the result carries exactly our own hop. See AppendHop.
func (m RelayedMessage) Forward(localBusID string) (RelayRequest, error) {
	path, err := AppendHop(m.BusPath, localBusID)
	if err != nil {
		return RelayRequest{}, err
	}
	return RelayRequest{
		OriginBus:     m.OriginBus,
		MessageID:     m.OriginMessageID,
		Sender:        m.Sender,
		Broadcast:     m.Broadcast,
		Recipients:    append([]string(nil), m.Recipients...),
		BusPath:       path,
		SentAtUnixNs:  m.OriginSentAt.UTC().UnixNano(),
		Size:          len(m.Body),
		ContentSHA256: m.ContentSHA256,
		Body:          append([]byte(nil), m.Body...),
	}, nil
}

// relayKeyFitsIdemKey reports whether every well-formed message id is also a
// legal idempotency key, which check 6 of ValidateRelayRequest depends on.
//
// It exists so the dependency is EXECUTABLE rather than a claim in a comment:
// TestRelayIdempotencyKeyIsTheOriginMessageID calls it, so a future widening of
// ids.BusIDPattern that let a message id contain a byte outside
// idem.KeyCharset fails a test here instead of silently making relay
// undeliverable.
func relayKeyFitsIdemKey() error {
	if ids.MaxMessageIDLen > idem.MaxKeyLen {
		return fmt.Errorf("a message id is up to %d bytes but an idempotency key is at most %d, so the relay key rule cannot hold", ids.MaxMessageIDLen, idem.MaxKeyLen)
	}
	// The bus id charset (ids.BusIDPattern), plus the '-' separator and the
	// decimal sequence, is the exact byte set a message id can contain. Each
	// one is put through idem.ValidateKey itself rather than compared against
	// idem.KeyCharset's textual form, so this tracks the VALIDATOR and not a
	// documentation string that could drift from it.
	const messageIDBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	for i := 0; i < len(messageIDBytes); i++ {
		b := messageIDBytes[i : i+1]
		if err := idem.ValidateKey(b); err != nil {
			return fmt.Errorf("byte %q may appear in a message id but is not a legal idempotency key byte (%s): %v", b, idem.KeyCharset, err)
		}
	}
	return nil
}
