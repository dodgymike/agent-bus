package auth

import (
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"
)

// RosterEntry is one enrolled agent: the server-minted id, the short name the
// client asked for, and the PUBLIC half of the agent's Ed25519 AUTH keypair.
//
// The server never holds the private half — see the package doc. Everything in
// this struct is safe to hand to anyone who is already entitled to know the
// agent exists; a public key is public.
type RosterEntry struct {
	// AgentID is the fully-qualified "<bus-id>.<name>-<n>" (invariant 2),
	// minted by the server (invariant 1). It is the routing and authorization
	// subject and is never reused.
	AgentID string

	// Name is the short name the client requested, byte-identical to the name
	// half of AgentID. It is kept separately so a caller does not have to
	// re-parse the id, and it is byte-identical rather than normalised because
	// ids.ValidateAgentName rejects alternate spellings instead of folding
	// them — see that function for why the counter key, the name here and the
	// name inside the id must be the same bytes.
	Name string

	// PublicKey is the agent's AUTH public key, exactly ed25519.PublicKeySize
	// bytes. The AUTH keypair is DISTINCT from the messaging keypair and the
	// two are never conflated.
	PublicKey ed25519.PublicKey

	// EnrolledAt is when the server accepted the enrolment.
	EnrolledAt time.Time
}

// Roster is the set of enrolled agents, as this package needs it.
//
// It is an injected interface so AUTH-3 can supply a WAL-backed implementation
// — recording the enrolment through the two-phase write path and rebuilding
// the roster by replay — without reshaping Service or any caller. The method
// set is deliberately the minimum the enrolment and session paths use.
//
// An implementation MUST be safe for concurrent use.
//
// # This package ships only MemoryRoster, which is NOT durable
//
// The implementation below is IN MEMORY ONLY. It writes nothing, fsyncs
// nothing, and is lost entirely on restart, so enrolment is NOT crash-safe
// until AUTH-3 lands. Nothing in this package may be read as a durability
// claim, and an operator is told so at startup.
type Roster interface {
	// Put records a new enrolment. It MUST reject a duplicate AgentID with
	// ErrDuplicateAgentID rather than overwriting: an overwrite would silently
	// rebind a live identity to a different keypair, which is the worst outcome
	// available on this path (invariants 1 and 3).
	Put(RosterEntry) error

	// Get returns the entry for agentID and whether it was found.
	Get(agentID string) (RosterEntry, bool)

	// Len reports how many agents are enrolled. It backs admission control on
	// the unauthenticated enrolment route.
	Len() int
}

// MemoryRoster is the in-memory Roster used until AUTH-3 supplies a durable
// one. It is safe for concurrent use.
//
// The zero value is not usable; construct with NewMemoryRoster.
type MemoryRoster struct {
	// mu guards byID. A plain mutex rather than an RWMutex: reads are one map
	// lookup behind an fsync-free path at small scale, and a single mutex keeps
	// Put's check-then-insert trivially atomic (invariant 8).
	mu   sync.Mutex
	byID map[string]RosterEntry
}

// NewMemoryRoster returns an empty in-memory roster.
func NewMemoryRoster() *MemoryRoster {
	return &MemoryRoster{byID: make(map[string]RosterEntry)}
}

// Put implements Roster.
//
// The public key is COPIED in. The caller passed an untrusted slice it may
// still hold a reference to, and a caller that later mutated its backing array
// would be mutating a STORED CREDENTIAL — silently changing which private key
// authenticates as that agent. Copying costs 32 bytes per enrolment and removes
// the whole class.
func (r *MemoryRoster) Put(e RosterEntry) error {
	if len(e.PublicKey) != ed25519.PublicKeySize {
		// Checked here as well as in Service.Enrol, not instead of it: this is
		// the boundary that hands keys to ed25519.Verify, and a key of the
		// wrong length reaching Verify is a panic, not a false (see
		// ErrInvalidPublicKey).
		return fmt.Errorf("%w: agent %q presented a %d-byte public key, want exactly %d", ErrInvalidPublicKey, e.AgentID, len(e.PublicKey), ed25519.PublicKeySize)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byID[e.AgentID]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateAgentID, e.AgentID)
	}
	stored := e
	stored.PublicKey = append(ed25519.PublicKey(nil), e.PublicKey...)
	r.byID[e.AgentID] = stored
	return nil
}

// Get implements Roster. The returned entry carries a COPY of the public key,
// for the mirror of the reason Put copies on the way in: a caller must not be
// able to reach into the stored credential through the slice it was handed.
func (r *MemoryRoster) Get(agentID string) (RosterEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.byID[agentID]
	if !ok {
		return RosterEntry{}, false
	}
	e.PublicKey = append(ed25519.PublicKey(nil), e.PublicKey...)
	return e, true
}

// Len implements Roster.
func (r *MemoryRoster) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}
