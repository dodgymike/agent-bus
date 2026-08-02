package relay

import (
	"errors"
	"fmt"
	"strings"

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
	// 1024 * (150 + 3) = 156,672 bytes, plus a bus id, an idempotency key and
	// the field names. 256 KiB leaves ~1.6x headroom, so a legal maximum-size
	// roster can always be encoded and can never be rejected by this cap —
	// while an unbounded stream still stops at a quarter of a megabyte.
	MaxHandshakeBytes = 256 << 10

	// MaxIdempotencyKeyLen bounds the peer-supplied idempotency key that makes
	// peer-enrol safe to retry (invariant 10).
	//
	// 128 is pinned to hub.MaxIdempotencyKeyLen / store.MaxIdempotencyKeyLen /
	// auth.MaxIdempotencyKeyLen. This package deliberately re-states the rule
	// instead of importing hub: one shape of idempotency key across the whole
	// server is the point, and a test pins this constant to that value so the
	// duplication cannot drift silently.
	MaxIdempotencyKeyLen = 128
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

	// ErrInvalidIdempotencyKey reports a missing or malformed idempotency key.
	ErrInvalidIdempotencyKey = errors.New("relay: invalid idempotency key")

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
type PeerEnrollRequest struct {
	// BusID is the initiator's claimed bus id.
	BusID string `json:"bus_id"`

	// IdempotencyKey makes peer-enrol safe to retry (invariant 10). Peer
	// topologies are cyclic and delivery is at-least-once, so a repeated
	// handshake is the steady state, not an edge case.
	IdempotencyKey string `json:"idempotency_key"`

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

	// IdempotencyKey is the key the initiator supplied, carried through so the
	// registration site can hand it to the durable applied-key table
	// (invariant 10) when this handler is finally gated and wired. It is empty
	// on a roster derived from a RESPONSE, which carries no key.
	IdempotencyKey string
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

// ValidateIdempotencyKey enforces the shape of a peer-supplied idempotency key:
// non-empty, at most MaxIdempotencyKeyLen bytes, [A-Za-z0-9._-] only.
//
// The alphabet is pinned to hub's so one key can be carried unchanged from the
// peer handshake into the durable applied-key table when this handler is gated
// and wired. It excludes everything that would need escaping in a log line or a
// storage key, which is why the error may quote the offending byte safely.
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: an idempotency key is required on every mutating call — including peer-enrol — so a retry after a lost acknowledgement cannot be applied twice (invariant 10)", ErrInvalidIdempotencyKey)
	}
	if len(key) > MaxIdempotencyKeyLen {
		return fmt.Errorf("%w: %d bytes, but an idempotency key is at most %d; the key is not echoed here because it is oversized", ErrInvalidIdempotencyKey, len(key), MaxIdempotencyKeyLen)
	}
	for i := 0; i < len(key); i++ {
		if !isIdempotencyKeyByte(key[i]) {
			return fmt.Errorf("%w: byte %d is %q, but an idempotency key must contain only [A-Za-z0-9._-]", ErrInvalidIdempotencyKey, i, key[i:i+1])
		}
	}
	return nil
}

func isIdempotencyKeyByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-':
		return true
	default:
		return false
	}
}

// ValidatePeerEnrollRequest validates a decoded request against our own bus id
// and returns what we may believe about the initiator.
func ValidatePeerEnrollRequest(localBusID string, req PeerEnrollRequest) (PeerRoster, error) {
	if err := ValidateIdempotencyKey(req.IdempotencyKey); err != nil {
		return PeerRoster{}, err
	}
	agents, err := ValidateRoster(localBusID, req.BusID, req.Agents)
	if err != nil {
		return PeerRoster{}, err
	}
	return PeerRoster{BusID: req.BusID, Agents: agents, IdempotencyKey: req.IdempotencyKey}, nil
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
func ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrPayloadTooLarge):
		return CodePayloadTooLarge
	case errors.Is(err, ErrBusIDCollision):
		return CodeBusIDCollision
	case errors.Is(err, ErrInvalidBusID):
		return CodeInvalidBusID
	case errors.Is(err, ErrRosterTooLarge):
		return CodeRosterTooLarge
	case errors.Is(err, ErrInvalidRoster):
		return CodeInvalidRoster
	case errors.Is(err, ErrInvalidIdempotencyKey):
		return CodeInvalidIdempotencyKey
	case errors.Is(err, ErrInvalidRequest):
		return CodeInvalidRequest
	default:
		return CodeInternal
	}
}
