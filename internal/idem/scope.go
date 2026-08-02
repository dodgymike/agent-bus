package idem

import "fmt"

// Operation names the mutating route an idempotency key was presented to. It
// is the middle element of Scope (doc.go point 3): without it, one agent
// reusing the same key across two different routes would collide with
// itself.
//
// This is a closed set — the constants below are the only valid values, and
// every constructor in this file rejects anything else via
// ErrInvalidOperation — because Scope's whole job is to make a key-only, or
// even a key+agent-only, lookup impossible to construct by accident; an
// open string field here would let a typo silently mint a fresh, wrong scope
// instead of failing loudly.
type Operation string

// The exhaustive set of mutating operations that require an idempotency key
// (doc.go point 6). Every route NOT in this list is read-only and MUST NOT
// accept a key at all.
const (
	OpEnrol     Operation = "enrol"
	OpSend      Operation = "send"
	OpBroadcast Operation = "broadcast"
	OpLeave     Operation = "leave"
	OpPeerEnrol Operation = "peer-enrol"
	OpRelay     Operation = "relay"
)

// MutatingOperations lists every Operation constant, in the order doc.go
// point 6 enumerates them. It exists so a caller (a test, or a future
// httpapi route table) can range over "every mutating operation" without
// hand-copying the list and risking it drifting from the constants above.
var MutatingOperations = []Operation{OpEnrol, OpSend, OpBroadcast, OpLeave, OpPeerEnrol, OpRelay}

func (op Operation) valid() bool {
	switch op {
	case OpEnrol, OpSend, OpBroadcast, OpLeave, OpPeerEnrol, OpRelay:
		return true
	default:
		return false
	}
}

// Scope is the fully-qualified idempotency lookup key: (agent, operation,
// key), or the bus-wide enrol special case. Its fields are UNEXPORTED
// deliberately — see doc.go point 3. The only way to obtain a Scope is
// through NewAgentScope or NewEnrolScope, both of which validate every
// component and refuse to build a Scope missing its non-key discriminant. A
// caller (this package's own tests included) cannot construct a
// key-only Scope even by mistake: there is no exported field to set and no
// exported zero-argument-friendly constructor.
//
// Scope is comparable (every field is a plain string/Operation) so IDEM-11
// can use it directly as a map key.
type Scope struct {
	agent string // fully-qualified <bus-id>.<agent-id>, or "" only for the enrol bus-wide case (enrolBusWide is true)
	op    Operation
	key   string

	// enrolBusWide distinguishes the bus-wide enrol scope from a per-agent
	// scope with an (impossible, since NewAgentScope rejects it) empty agent.
	// Without this discriminant a zero-value Scope{} would silently equal a
	// legitimately-constructed enrol scope, which must never happen: a
	// zero-value Scope is exactly the key-only lookup this type exists to
	// make unconstructable.
	enrolBusWide bool
}

// Agent returns the scope's fully-qualified agent id, or "" for the bus-wide
// enrol scope (see EnrolBusWide).
func (s Scope) Agent() string { return s.agent }

// Operation returns the scope's operation.
func (s Scope) Operation() Operation { return s.op }

// EnrolBusWide reports whether this is the bus-wide enrol scope built by
// NewEnrolScope, as opposed to a per-agent scope built by NewAgentScope.
func (s Scope) EnrolBusWide() bool { return s.enrolBusWide }

// NewAgentScope builds a per-agent idempotency Scope. agentID must be the
// caller's proven, fully-qualified "<bus-id>.<agent-id>" (invariant 2) — this
// package does not itself re-validate the id's shape (that is
// ids.ValidateAgentName/ParseAgentID's job, and idem deliberately has no
// dependency on internal/ids so it stays a leaf package); it only refuses an
// EMPTY agent id, because an empty agent is indistinguishable from "no agent
// at all", i.e. exactly the key-only lookup this type exists to prevent.
//
// op must be one of the Operation constants and must not be OpEnrol: enrol
// has no authenticated caller yet (doc.go point 4), so its Scope is always
// built by NewEnrolScope, never this one.
func NewAgentScope(agentID string, op Operation, key string) (Scope, error) {
	if agentID == "" {
		return Scope{}, ErrInvalidAgent
	}
	if op == OpEnrol {
		return Scope{}, fmt.Errorf("%w: enrol has no authenticated agent id yet (doc.go point 4); use NewEnrolScope", ErrInvalidOperation)
	}
	if !op.valid() {
		return Scope{}, fmt.Errorf("%w: %q is not one of the fixed mutating operations", ErrInvalidOperation, op)
	}
	if err := ValidateKey(key); err != nil {
		return Scope{}, err
	}
	return Scope{agent: agentID, op: op}.withKey(key), nil
}

// NewEnrolScope builds the bus-wide idempotency Scope for enrolment — the one
// operation with no proven caller to scope by (doc.go point 4). Every enrol
// attempt against one bus shares this key space regardless of the
// (unauthenticated, unverified) name or public key presented; see doc.go
// point 4 for the documented squat risk this implies and the caveat IDEM-13
// must resolve before wiring a real store onto it.
func NewEnrolScope(key string) (Scope, error) {
	if err := ValidateKey(key); err != nil {
		return Scope{}, err
	}
	return Scope{op: OpEnrol, enrolBusWide: true}.withKey(key), nil
}

// withKey returns a copy of s with key set. Small unexported helper so
// NewAgentScope and NewEnrolScope share one place that assigns the field.
func (s Scope) withKey(key string) Scope {
	s.key = key
	return s
}

// ValidateIdempotencyKey is the single shared validator every mutating route
// handler calls, named to match the shape the task brief asked for
// ("validateIdempotencyKey(agentID, key)"), exported so it is usable from
// every package that owns a route (this package cannot own the routes
// themselves — see doc.go's file-ownership note). It is equivalent to
// NewAgentScope without an Operation component, for a caller that only needs
// a yes/no validation answer and will build its own Scope (with the correct
// Operation) separately — for example an HTTP middleware that validates the
// header before the specific route's operation is known.
//
// agentID must be non-empty; use ValidateKey directly for the (only)
// agentless case, enrolment.
func ValidateIdempotencyKey(agentID, key string) error {
	if agentID == "" {
		return ErrInvalidAgent
	}
	return ValidateKey(key)
}
