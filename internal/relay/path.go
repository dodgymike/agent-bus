package relay

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
)

// Loop prevention over the traversed bus path — RELAY-3.
//
// # WHAT THIS IS FOR, AND WHAT IT IS NOT FOR
//
// PROTOCOL.md §8.5 settles the trust model and this file must never drift from
// it. Quoting it: "The traversed bus path is outside the signature and cannot
// ever be inside it — it grows on every hop, so covering it would invalidate
// the signature at the first relay. A lying peer can therefore rewrite the
// path: loop prevention (RELAY-3) is an AVAILABILITY mechanism, never a
// security one, and duplicate suppression rests on idempotency and the origin
// identity (IDEM-15)."
//
// Read that as an instruction rather than a caveat. Everything below is
// advisory in the face of a hostile peer, because a hostile peer can strip us
// out of the path it sends us and we cannot detect it. What stops the SAME
// message being delivered twice is internal/idem: the relay idempotency key is
// the ORIGIN's message id (see message.go), so every copy of one message —
// however it was routed and whatever path it claims — lands on ONE idem.Scope
// with ONE fingerprint. RELAY-3 COMPLEMENTS that and NEVER substitutes for it,
// which is exactly the relationship CLAUDE.md invariant 10 requires ("loop
// prevention via the traversed bus path is a complement to idempotency, never
// a substitute for it").
//
// What loop prevention DOES buy, and it is worth having: in a cyclic topology
// with at-least-once delivery, a message with nothing to stop it circulates
// forever, and the cost is paid by every correct bus on the ring. Dropping a
// message that has already visited us collapses that from unbounded traffic to
// one extra hop per cycle. It is a traffic-amplification control.

// MaxBusPath is the ingress cap on the traversed-bus list.
//
// It is HARD-LINKED to store.MaxBusPath rather than re-declared as a literal,
// because the two caps are not independent: a path we accept here is a path we
// go on to persist, and a path the durable record would refuse is a message we
// would ACKNOWLEDGE AND THEN FAIL TO PERSIST — the acknowledged-but-lost
// message CLAUDE.md invariant 5 forbids. The relay ingress cap must therefore
// never exceed the on-disk cap, and the cheapest way to guarantee that is to
// have exactly one number.
const MaxBusPath = store.MaxBusPath

// Bus path failures. All are checkable with errors.Is.
var (
	// ErrInvalidBusPath reports a path that is malformed, empty where it must
	// not be, or carries the same bus twice.
	ErrInvalidBusPath = errors.New("relay: invalid traversed bus path")

	// ErrBusPathTooLong reports a path longer than MaxBusPath.
	ErrBusPathTooLong = errors.New("relay: traversed bus path too long")

	// ErrRelayLoop reports that THIS bus is already on the traversed path, so
	// the message has come back round to us.
	//
	// THIS IS NOT AN ERROR CONDITION OF THE PEER. In a cyclic topology it is
	// the expected steady state, and the peer that sent it did nothing wrong —
	// it cannot know our federation graph. Everything downstream must treat it
	// as a SETTLED, non-retryable outcome: relayhttp.go answers it with HTTP
	// 200 and {"accepted":false,"dropped_reason":"loop"} precisely so a
	// retrying sender stops rather than re-delivering forever a message that
	// can never be accepted.
	ErrRelayLoop = errors.New("relay: message has already traversed this bus")
)

// PathContains reports whether busID appears anywhere on path, compared
// CASE-INSENSITIVELY.
//
// Case-insensitively, because ids.BusIDPattern admits both cases: "BUS-X" and
// "bus-x" are two spellings of one operator-visible identity, and a
// case-SENSITIVE membership test would let a peer flip a single character's
// case to slip past the drop and spin the cycle forever — turning the one
// mechanism that bounds cyclic traffic into a no-op for the price of a
// strings.ToUpper. This is the same posture ValidatePeerBusID already takes
// when it folds a peer's claimed bus id against ours, and the same posture
// ids.ValidateAgentName takes when it REJECTS uppercase rather than folding it.
func PathContains(path []string, busID string) bool {
	for _, hop := range path {
		if strings.EqualFold(hop, busID) {
			return true
		}
	}
	return false
}

// ValidateBusPath checks an untrusted traversed-bus path.
//
// The order of the checks is the load-bearing part, and it is exactly:
//
//  1. EMPTY is refused. Every RELAYED message has traversed at least the bus
//     that originated it, so an empty path is either a fabrication or a bus
//     that failed to stamp its own hop — and in both cases we cannot tell
//     where the message came from, which is the one thing the path exists to
//     say. (The origin's own EGRESS path is the single legitimate empty one;
//     see AppendHop, which accepts it. This validator is the INGRESS rule.)
//  2. The LENGTH is refused before any per-hop parsing, so a hostile peer
//     cannot make us parse MaxBusPath+N hops before we decline.
//  3. Each hop is bounded BEFORE ids.ValidateBusID sees it, because that
//     function quotes the id it rejects with %q and %q expands a control byte
//     to four characters — a peer sending a 200 KiB "hop" would otherwise
//     choose the size of the line we log about refusing it. Same reasoning,
//     same constant (MaxPeerBusIDLen) and same not-echoed wording as
//     ValidatePeerBusID.
//  4. A DUPLICATE hop (case-insensitively) is refused. A repeated hop is
//     either a loop that has already completed or a fabricated path; either
//     way the message is not routable and dropping it costs a correct peer
//     nothing, while accepting it would leave a path from which no further
//     routing decision can be trusted.
func ValidateBusPath(path []string) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: the path is empty, but every relayed message has traversed at least the bus that originated it", ErrInvalidBusPath)
	}
	return validateHops(path)
}

// validateHops applies steps 2-4 of ValidateBusPath. It is shared with
// AppendHop, which must apply the identical per-hop rules but must NOT reject
// an empty path — see AppendHop.
func validateHops(path []string) error {
	if len(path) > MaxBusPath {
		return fmt.Errorf("%w: %d hops, but a traversed path carries at most %d; a longer path is a routing pathology, not a topology", ErrBusPathTooLong, len(path), MaxBusPath)
	}
	seen := make(map[string]struct{}, len(path))
	for i, hop := range path {
		if len(hop) > MaxPeerBusIDLen {
			// Refused BEFORE ids.ValidateBusID, whose message quotes the id.
			return fmt.Errorf("%w: hop %d is %d bytes, but a bus id is at most %d; the hop is not echoed here because it is oversized", ErrInvalidBusPath, i, len(hop), MaxPeerBusIDLen)
		}
		if err := ids.ValidateBusID(hop); err != nil {
			return fmt.Errorf("%w: hop %d: %v", ErrInvalidBusPath, i, err)
		}
		folded := strings.ToLower(hop)
		if _, dup := seen[folded]; dup {
			return fmt.Errorf("%w: hop %d is %q, which already appears earlier on the path (compared case-insensitively); a repeated hop is either a completed loop or a fabrication, and in neither case is the message routable", ErrInvalidBusPath, i, hop)
		}
		seen[folded] = struct{}{}
	}
	return nil
}

// CheckIncomingPath is RELAY-3's rule, applied at ingress: validate the path,
// then refuse it if THIS bus is already on it.
//
// The order matters in the cheap direction as well as the correct one — a
// malformed path is refused as malformed (ErrInvalidBusPath / ErrBusPathTooLong,
// which ARE the sender's fault) rather than as a loop (ErrRelayLoop, which is
// nobody's fault and is answered with a 200).
func CheckIncomingPath(localBusID string, path []string) error {
	if err := ValidateBusPath(path); err != nil {
		return err
	}
	if PathContains(path, localBusID) {
		return fmt.Errorf("%w: bus %q appears on the %d-hop path this message arrived with, so it has been here before", ErrRelayLoop, localBusID, len(path))
	}
	return nil
}

// AppendHop returns path with localBusID appended — the egress stamp, applied
// when this bus re-relays a message onward.
//
// The result is ALWAYS A FRESH SLICE. It never appends into the caller's
// backing array, because the caller's slice may be one decoded from a PEER's
// payload with spare capacity, and writing through it would let one outbound
// forward silently rewrite the path another outbound forward is about to read.
//
// An EMPTY input path is accepted, and it is the one place an empty path is
// legal: it means "this bus is the ORIGIN and the message has traversed
// nothing yet", so the result is exactly [localBusID]. ValidateBusPath refuses
// an empty path because that is the INGRESS rule — a message that ARRIVED from
// a peer has necessarily traversed its origin — and the two rules are
// different on purpose.
func AppendHop(path []string, localBusID string) ([]string, error) {
	if err := ids.ValidateBusID(localBusID); err != nil {
		// Our own id, so this is a bug on THIS bus rather than a peer problem;
		// it is checked anyway so a misconfigured local id cannot make the loop
		// test below vacuous.
		return nil, fmt.Errorf("relay: local bus id is invalid, so no hop can be appended for it: %w", err)
	}
	if err := validateHops(path); err != nil {
		return nil, err
	}
	if PathContains(path, localBusID) {
		return nil, fmt.Errorf("%w: bus %q is already on the path, so appending it would fabricate a second visit", ErrRelayLoop, localBusID)
	}
	if len(path)+1 > MaxBusPath {
		return nil, fmt.Errorf("%w: appending a hop to a %d-hop path would exceed the %d-hop limit", ErrBusPathTooLong, len(path), MaxBusPath)
	}
	out := make([]string, 0, len(path)+1)
	out = append(out, path...)
	out = append(out, localBusID)
	return out, nil
}

// NextHopAllowed reports whether a message carrying path may be forwarded to
// peerBusID — the EGRESS split horizon.
//
// # The division of labour, which matters because neither half is sufficient
//
// This function is the OPTIMISATION: it stops cycle traffic at the FIRST hop,
// before a byte leaves this process, so a correct mesh never puts the message
// on the wire towards a bus that has demonstrably already seen it.
//
// CheckIncomingPath is the BACKSTOP: it still works when the peer LIES — when
// it strips itself, or us, out of the path it forwards. A peer's path is
// untrusted input (PROTOCOL.md §8.5), so the sending side's split horizon can
// be defeated by anyone who wants to defeat it, and the receiving side must
// therefore re-derive the decision from its OWN identity, which is the one
// thing on the path it does know.
//
// Dropping either half leaves a real gap: without the split horizon, a correct
// mesh sends every message once round every cycle before anything stops it;
// without the ingress check, one lying peer restores unbounded circulation.
func NextHopAllowed(path []string, peerBusID string) bool {
	return !PathContains(path, peerBusID)
}
