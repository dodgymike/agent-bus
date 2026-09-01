package relay

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// PeerEnrollPath is the path the peer handshake is EXPECTED to occupy once it
// is gated by INVITE-PEERGUARD and MTLS-RELAYGUARD (see the package doc).
//
// It is a constant, not a registration: naming the path here lets the initiator
// build a URL and lets the guard tasks assert about a path that has exactly one
// spelling, without anything in this package attaching a handler to a mux.
const PeerEnrollPath = "/v1/peer/enroll"

// The caps on a handshake. Each is enforced BEFORE the allocation it bounds —
// a length field, a Content-Length header and a JSON array header are all
// claims made by the peer, and none of them is a fact.
const (
	// MaxRosterAgents bounds how many fully-qualified agent ids one roster may
	// carry, in either direction.
	//
	// 1024 is a deliberate, reviewable number rather than an unbounded slice:
	// the roster is exchanged in ONE shot, so the cap is also the largest bus
	// this handshake can federate. It is generous for the "several Claude Code
	// agents per laptop bus" case this project is built for, and small enough
	// that a hostile peer cannot make us materialise an unbounded []string. A
	// bus that outgrows it needs paginated roster exchange, not a bigger
	// number — the responder therefore FAILS LOUDLY rather than truncating its
	// own roster, because a silently partial roster misroutes messages, which
	// is worse than a refused handshake.
	MaxRosterAgents = 1024

	// MaxHandshakeBytes bounds the encoded payload read from the network in
	// either direction, before it is decoded.
	//
	// It is DERIVED, not guessed: MaxRosterAgents ids of at most
	// ids.MaxAgentIDLen bytes, each costing two quotes and a comma in JSON, is
	// 1024 * (150 + 3) = 156,672 bytes, plus a bus id and the field names.
	// 256 KiB leaves ~1.6x headroom, so a legal maximum-size
	// roster can always be encoded and can never be rejected by this cap —
	// while an unbounded stream still stops at a quarter of a megabyte.
	MaxHandshakeBytes = 256 << 10

	// MaxPeerBusIDLen bounds a CLAIMED bus id before it is validated.
	//
	// ids.ValidateBusID would reject an over-long id anyway — its pattern caps
	// at 64 — but its error message quotes the offending id with %q, and %q
	// expands a control byte to four characters. A peer sending a 200 KiB
	// "bus id" would therefore choose the size of the line we log about
	// rejecting it. This check refuses it first, without echoing it, exactly as
	// ids.ParseAgentID refuses an oversized agent id before quoting anything.
	// 64 is ids.BusIDPattern's own upper bound, so this rejects nothing that
	// could otherwise have been valid.
	MaxPeerBusIDLen = 64
)

// Handshake failures. These are the categories a caller — or the responder's
// HTTP layer — switches on; the wrapped text carries the diagnosable detail.
var (
	// ErrInvalidRequest reports a payload that is not a well-formed handshake:
	// undecodable, wrong shape, unknown fields, or trailing bytes.
	ErrInvalidRequest = errors.New("relay: invalid peer handshake payload")

	// ErrInvalidBusID reports a claimed bus id that is not a well-formed bus id.
	ErrInvalidBusID = errors.New("relay: invalid peer bus id")

	// ErrBusIDCollision reports a peer asserting OUR namespace — either
	// claiming our bus id as its own, or listing an agent id whose bus half is
	// ours. This is id spoofing (invariant 1), not a mistake to be tolerated.
	ErrBusIDCollision = errors.New("relay: peer asserts an id inside our own bus namespace")

	// ErrInvalidRoster reports a roster entry that is malformed, duplicated, or
	// outside the peer's own namespace.
	ErrInvalidRoster = errors.New("relay: invalid peer roster")

	// ErrRosterTooLarge reports a roster longer than MaxRosterAgents.
	ErrRosterTooLarge = errors.New("relay: peer roster too large")

	// ErrPayloadTooLarge reports a body that exceeded MaxHandshakeBytes.
	ErrPayloadTooLarge = errors.New("relay: handshake payload too large")
)

// PeerEnrollRequest is the body an initiating bus POSTs to PeerEnrollPath.
//
// Every field is UNTRUSTED. BusID is what the caller CLAIMS to be; nothing in
// this package makes that claim true — proving it is the job of the invite
// redemption (INVITE-PEERGUARD) and the TLS client certificate
// (MTLS-RELAYGUARD), both of which gate this handler and neither of which
// exists yet.
//
// The idempotency key that makes peer-enrol safe to retry (invariant 10) is
// deliberately NOT a field here: it travels in the idem.HeaderName request
// header, which internal/idem (IDEM-10) defines as the one canonical carrier —
// no body field, no fallback, because a key able to arrive by two routes
// eventually disagrees with itself on a retry. The header also lets an
// oversized key be refused before a byte of body is read. Strict decoding means
// a peer that puts the key in the body here gets a 400 rather than having it
// silently ignored.
type PeerEnrollRequest struct {
	// BusID is the initiator's claimed bus id.
	BusID string `json:"bus_id"`

	// Agents is the initiator's roster: fully-qualified "<bus-id>.<agent-id>"
	// ids (invariant 2), all inside the initiator's OWN namespace. May be
	// empty: a freshly started bus has no agents and must still be able to
	// federate.
	Agents []string `json:"agents"`
}

// PeerEnrollResponse is the responder's 200 body. It is validated by the
// initiator with the same rules the responder applied to the request: a reply
// is no more trusted than a request.
type PeerEnrollResponse struct {
	// BusID is the responder's own server-minted bus id (invariant 1).
	BusID string `json:"bus_id"`

	// Agents is the responder's roster of fully-qualified ids.
	Agents []string `json:"agents"`

	// Count is len(Agents), sent so a client can spot a truncated response
	// without counting and because a bare empty array reads ambiguously in a
	// log. It is NOT trusted: the validator ignores it in favour of the actual
	// length, so a lying Count cannot drive an allocation.
	Count int `json:"count"`
}

// PeerRoster is the VALIDATED outcome of one direction of the handshake: what
// this bus is now entitled to believe about the other end, having checked it.
//
// "Entitled to believe" is bounded: the ids are well-formed and confined to the
// peer's own namespace. Nothing here asserts the peer is who it says it is.
type PeerRoster struct {
	// BusID is the peer's validated bus id — well-formed, and not ours.
	BusID string

	// Agents are the peer's validated fully-qualified agent ids: parseable,
	// unique, and every one of them inside BusID's namespace. The slice is
	// freshly allocated and never aliases the decoded payload.
	Agents []string

	// IdempotencyKey is the key the initiator supplied in the idem.HeaderName
	// header, carried through so the registration site can build an
	// idem.Scope with idem.OpPeerEnrol and hand it to the durable applied-key
	// table (invariant 10) when this handler is finally gated and wired. It is
	// empty on a roster derived from a RESPONSE, which carries no key.
	IdempotencyKey string

	// Fingerprint is the canonical fingerprint of the VALIDATED payload this
	// roster came from, computed with idem.ComputeFingerprint over the field
	// list documented at peerFingerprint.
	//
	// It exists because invariant 10's central distinction cannot be made
	// without it: same key + same payload is a legitimate retry that must
	// return the original result, while same key + DIFFERENT payload is a
	// protocol violation that must be rejected and logged — and NOT
	// disconnected (narrowed 2026-08-08). A
	// registration site handed only the key could not tell those apart, and
	// would have to re-derive the fingerprint from a payload it no longer has.
	// Zero on a roster derived from a RESPONSE.
	Fingerprint idem.Fingerprint
}

// peerFingerprint computes the canonical fingerprint of a peer-enrol payload.
//
// The field list is FIXED and ordered, as idem.ComputeFingerprint requires
// every call site to document: the operation name (domain separation, so a
// peer-enrol fingerprint can never equal a send's), then the claimed bus id,
// then each validated agent id in the order the peer sent them. Order is part
// of the payload: two rosters differing only in order are different requests,
// and treating them as equal would let a retry quietly carry new content.
func peerFingerprint(busID string, agents []string) idem.Fingerprint {
	fields := make([][]byte, 0, len(agents)+2)
	fields = append(fields, []byte(idem.OpPeerEnrol), []byte(busID))
	for _, a := range agents {
		fields = append(fields, []byte(a))
	}
	return idem.ComputeFingerprint(fields...)
}

// ValidatePeerBusID checks a peer's claimed bus id against our own.
//
// Two rules, and the second is the load-bearing one:
//
//   - it must be a well-formed bus id (ids.ValidateBusID — one definition of
//     legal, shared with the rest of the server);
//   - it must not be OURS, compared case-insensitively.
//
// Case-insensitively, because ids.BusIDPattern admits both cases: a peer
// claiming "BUS-ABC" when we are "bus-abc" is not a different namespace in any
// sense an operator reading a roster would recognise, and the fully-qualified
// agent id is the routing and authorization subject. This is the same posture
// ids.ValidateAgentName takes when it REJECTS uppercase rather than folding it:
// remove the confusable at the door instead of resolving it later.
func ValidatePeerBusID(localBusID, peerBusID string) error {
	if err := ids.ValidateBusID(localBusID); err != nil {
		// A programming error on our side, not the peer's fault. It is checked
		// anyway so a misconfigured local bus id can never make the collision
		// test below vacuous.
		return fmt.Errorf("relay: local bus id is invalid, so no peer claim can be judged against it: %w", err)
	}
	if len(peerBusID) > MaxPeerBusIDLen {
		// Refused BEFORE ids.ValidateBusID, whose message quotes the id: see
		// MaxPeerBusIDLen. The peer chooses this string; it must not get to
		// choose the size of the line we log about rejecting it.
		return fmt.Errorf("%w: %d bytes, but a bus id is at most %d; the claimed id is not echoed here because it is oversized", ErrInvalidBusID, len(peerBusID), MaxPeerBusIDLen)
	}
	if err := ids.ValidateBusID(peerBusID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBusID, err)
	}
	if strings.EqualFold(peerBusID, localBusID) {
		return fmt.Errorf("%w: peer claims bus id %q, but this bus is %q (compared case-insensitively); a peer may never assert our namespace", ErrBusIDCollision, peerBusID, localBusID)
	}
	return nil
}

// ValidateRoster validates an untrusted roster claimed to belong to peerBusID,
// returning a fresh slice of the validated ids.
//
// The order of the checks is the security-relevant part:
//
//  1. the roster LENGTH is checked first, so a hostile peer cannot make us do
//     MaxRosterAgents+N parses before we refuse;
//  2. each entry is parsed by ids.ParseAgentID, which caps the id length,
//     rejects malformed ids and enforces one spelling per id;
//  3. an entry whose bus half is OURS is rejected as spoofing (ErrBusIDCollision)
//     before the generic namespace check, so the loudest failure gets the
//     specific error;
//  4. an entry whose bus half is neither ours nor the peer's is rejected too: a
//     peer speaks for its own agents only. Learning about a third bus's agents
//     is transitive federation, which is RELAY's later business and must not
//     arrive as a side effect of a handshake.
//
// Duplicates are rejected rather than silently deduplicated: a roster is a set,
// and a repeated id is either a bug on the peer or an attempt to inflate our
// view of it. Rejecting keeps len(result) == len(input), so no caller can be
// surprised by a shorter answer than it was given.
func ValidateRoster(localBusID, peerBusID string, agents []string) ([]string, error) {
	if err := ValidatePeerBusID(localBusID, peerBusID); err != nil {
		return nil, err
	}
	if len(agents) > MaxRosterAgents {
		return nil, fmt.Errorf("%w: %d agents, but a roster carries at most %d; a bus this large needs paginated roster exchange, not a larger cap", ErrRosterTooLarge, len(agents), MaxRosterAgents)
	}

	out := make([]string, 0, len(agents))
	seen := make(map[string]struct{}, len(agents))
	for i, id := range agents {
		busPart, _, _, err := ids.ParseAgentID(id)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", ErrInvalidRoster, i, err)
		}
		if strings.EqualFold(busPart, localBusID) {
			return nil, fmt.Errorf("%w: roster entry %d is %q, whose bus half is this bus (%q, compared case-insensitively); a peer may not assert ids inside our namespace", ErrBusIDCollision, i, id, localBusID)
		}
		if busPart != peerBusID {
			return nil, fmt.Errorf("%w: roster entry %d is %q, but bus %q may only list its own agents", ErrInvalidRoster, i, id, peerBusID)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: roster entry %d is %q, which appears more than once; a roster is a set", ErrInvalidRoster, i, id)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// validateLocalRoster checks OUR OWN roster before we publish it to a peer, in
// either direction of the handshake.
//
// Validating our own output is not paranoia about the peer; it is about us. A
// malformed local id teaches a peer to route to something that cannot exist,
// and an id from the wrong namespace means a roster provider is handing out
// somebody else's agents. Both are bugs on THIS bus, so both are reported as
// errors rather than filtered out: dropping the bad entry would federate a
// roster quietly missing an agent, and misroute that agent's messages for as
// long as nobody looks.
func validateLocalRoster(localBusID string, roster []string) ([]string, error) {
	if len(roster) > MaxRosterAgents {
		return nil, fmt.Errorf("%w: this bus has %d agents, more than the %d a single-shot roster exchange carries; refusing to federate a TRUNCATED roster, which would silently misroute messages for the agents left out — paginated exchange is required first", ErrRosterTooLarge, len(roster), MaxRosterAgents)
	}
	out := make([]string, 0, len(roster))
	seen := make(map[string]struct{}, len(roster))
	for i, id := range roster {
		busPart, _, _, err := ids.ParseAgentID(id)
		if err != nil {
			return nil, fmt.Errorf("relay: local roster entry %d is not a valid agent id (%v); this is a bug on THIS bus, not a peer problem", i, err)
		}
		if busPart != localBusID {
			return nil, fmt.Errorf("relay: local roster entry %d is %q, which belongs to bus %q and not to this bus (%q); this is a bug on THIS bus", i, id, busPart, localBusID)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("relay: local roster entry %d is %q, which appears more than once; this is a bug on THIS bus", i, id)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// ValidatePeerEnrollRequest validates a decoded request against our own bus id
// and returns what we may believe about the initiator.
func ValidatePeerEnrollRequest(localBusID, idempotencyKey string, req PeerEnrollRequest) (PeerRoster, error) {
	if err := idem.ValidateKey(idempotencyKey); err != nil {
		return PeerRoster{}, err
	}
	agents, err := ValidateRoster(localBusID, req.BusID, req.Agents)
	if err != nil {
		return PeerRoster{}, err
	}
	return PeerRoster{
		BusID:          req.BusID,
		Agents:         agents,
		IdempotencyKey: idempotencyKey,
		Fingerprint:    peerFingerprint(req.BusID, agents),
	}, nil
}

// ValidatePeerEnrollResponse validates a decoded response with exactly the
// rules ValidatePeerEnrollRequest applies to a request, minus the idempotency
// key the responder does not send.
//
// The initiator validating the reply is not belt-and-braces: the responder is
// as untrusted as the initiator, it is equally able to claim our bus id or to
// list agents in our namespace, and it is the side we DIALLED — which proves
// only that we know its address, never who answered.
func ValidatePeerEnrollResponse(localBusID string, resp PeerEnrollResponse) (PeerRoster, error) {
	agents, err := ValidateRoster(localBusID, resp.BusID, resp.Agents)
	if err != nil {
		return PeerRoster{}, err
	}
	return PeerRoster{BusID: resp.BusID, Agents: agents}, nil
}

// ErrorCode maps a handshake failure to the stable, non-echoing code sent to
// the other end. The detailed error stays local — it quotes peer-supplied
// bytes, and while those are bounded and validated, there is no reason to hand
// a stranger a description of our parser. The code is enough for a peer
// operator to act on and enough for us to grep for.
//
// The arms are ordered MOST SPECIFIC FIRST and must stay that way: a sentinel
// that another one wraps has to be tested before its wrapper, or the more
// useful code is shadowed by the vaguer one. TestErrorCodeIsStable pins every
// mapping, so a reordering that changes an answer fails there rather than in a
// peer operator's logs — EXCEPT the three acknowledgement-plane arms below,
// which are pinned in ack_test.go beside the privacy reasoning that decides
// them, because two of them share a code deliberately and a reader changing that
// needs the argument in front of them, not a bare expectation in another file.
func ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrPayloadTooLarge):
		return CodePayloadTooLarge

	// Relay ingress (RELAY-2/RELAY-3). ErrRelayLoop is FIRST among these
	// because it is the one outcome that is not a fault at all: the relay
	// handler answers it with 200 and never reaches this function, and any
	// other caller that does reach it must get the loop-specific code rather
	// than a generic "invalid" that would invite a retry.
	case errors.Is(err, ErrRelayLoop):
		return CodeRelayLoop
	case errors.Is(err, ErrBusPathTooLong), errors.Is(err, ErrInvalidBusPath):
		return CodeInvalidBusPath
	case errors.Is(err, ErrRelayKeyMismatch):
		// The offending item IS the idempotency key, so a peer operator is
		// pointed at the header rather than at the envelope.
		return CodeInvalidIdempotencyKey
	case errors.Is(err, ErrIdempotencyViolation):
		return CodeIdempotencyViolation
	case errors.Is(err, ErrUnknownLocalRecipient):
		// RELAY-21. Above the generic relay codes below because the envelope is
		// NOT invalid: it is well formed, correctly signed, and addressed to
		// somebody we do not have. The remedy lives in the sending bus's roster,
		// not in its encoder.
		return CodeUnknownRecipient

	// The acknowledgement plane (ACK-4). It EXTENDS THIS VOCABULARY rather than
	// starting a second one — a peer operator reads one list.
	//
	// ErrAckNotBound and ErrAckOutcomeConflict deliberately share ONE code, and
	// that is a privacy decision rather than laziness. "No obligation binds you
	// to this key" and "this key already settled differently" are both answered
	// 409/idempotency_violation, so a peer cannot tell an unknown key from one
	// it is not entitled to settle from one that is already terminal. Splitting
	// them would hand any peered bus an oracle for "did bus A send message K to
	// bus B" — see ErrAckNotBound's doc, and the 409 no-matching-reservation
	// indistinguishability invariant 10 preserves for the same reason.
	//
	// ErrInvalidAckFrame is NOT folded in with them: a malformed field is
	// decidable by the sender from its own bytes without asking us, so 400
	// leaks nothing and collapsing it would only make an honest peer's encoder
	// bug undebuggable.
	case errors.Is(err, ErrAckNotBound), errors.Is(err, ErrAckOutcomeConflict):
		return CodeIdempotencyViolation
	case errors.Is(err, ErrInvalidAckFrame):
		return CodeInvalidRequest

	// The ACK frame's wire version (ACK-3) is checked before anything else in
	// ValidatePeerAckRequest, and it is mapped BESIDE the ACK codes here for the
	// same reason RELAY-23 maps the envelope's first: if the two buses do not
	// agree on the format, every other diagnosis is an answer to a question we
	// could not read. It must not fall through to CodeInvalidRequest, which
	// would send the far-end operator looking for a malformed field instead of
	// at the older of the two binaries.
	case errors.Is(err, ErrUnsupportedAckVersion):
		return CodeUnsupportedAckVersion

	// The relay envelope's wire version (RELAY-23), the sibling of the ACK frame's
	// above. It is check 0 of ValidateRelayRequest, and it is mapped BESIDE the ACK
	// version and ABOVE the SIGN-7 and ErrInvalidRelay arms for the same reason: if
	// the two buses do not agree on the envelope format, every other diagnosis is
	// an answer to a question we could not read. It must not fall through to
	// CodeInvalidRelay, which would send the far-end operator hunting a malformed
	// field instead of at the older of the two binaries. RELAY-53: this is a
	// distinct code from CodeUnsupportedAckVersion, not a collapse of the two.
	case errors.Is(err, ErrUnsupportedRelayVersion):
		return CodeUnsupportedRelayVersion

	// Signed relay ingest (SIGN-7). These sit ABOVE ErrInvalidRelay because a
	// signature failure is the more specific and the more serious diagnosis: a
	// peer told "invalid_relay" would go looking for a malformed field, when the
	// actual answer is that we will not attribute this message to the agent it
	// names. They are also never retryable, and the generic code invites a retry.
	case errors.Is(err, ErrMissingSignature), errors.Is(err, ErrUnsignable):
		return CodeUnsigned
	case errors.Is(err, ErrUnpeeredBus):
		// FIRST among the attribution failures, because it is the most specific
		// diagnosis and the only one with an operator remedy: we hold no
		// peering-time pin for the origin bus's signing key, so nothing it sends
		// is verifiable. A peer told "bad_signature" here would go looking for a
		// forgery when the actual answer is that the two buses were never peered.
		return CodeUnpeeredBus
	case errors.Is(err, ErrNoSignerKey), errors.Is(err, ErrBadSignature):
		return CodeBadSignature
	case errors.Is(err, ErrMissingAttestation), errors.Is(err, ErrInvalidRelay):
		return CodeInvalidRelay

	// Roster sync (RELAY-2). ErrPeerBusIDCollision is folded into the existing
	// bus-id-collision code below, since a peer reading it needs the same
	// remedy — pick a bus id that is not confusable with one we already know.
	case errors.Is(err, ErrUnknownPeer):
		return CodeUnknownPeer
	case errors.Is(err, ErrStaleRosterUpdate):
		return CodeStaleRoster
	case errors.Is(err, ErrInvalidRosterUpdate):
		return CodeInvalidRosterUpdate
	case errors.Is(err, ErrTooManyPeers):
		// Capacity, not the peer's fault and not permanent: it is answered as
		// "not now" (503/CodeUnavailable), never as "never".
		return CodeUnavailable

	case errors.Is(err, ErrPeerBusIDCollision), errors.Is(err, ErrBusIDCollision):
		return CodeBusIDCollision
	case errors.Is(err, ErrInvalidBusID):
		return CodeInvalidBusID
	case errors.Is(err, ErrRosterTooLarge):
		return CodeRosterTooLarge
	case errors.Is(err, ErrInvalidRoster):
		return CodeInvalidRoster
	case errors.Is(err, idem.ErrMissingKey), errors.Is(err, idem.ErrInvalidKey):
		return CodeInvalidIdempotencyKey
	case errors.Is(err, ErrInvalidRequest):
		return CodeInvalidRequest
	default:
		return CodeInternal
	}
}
