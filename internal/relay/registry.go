package relay

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// MaxPeers bounds how many peer buses one Registry holds.
//
// The number that actually matters is the PRODUCT: total routing-table memory
// is bounded by MaxPeers * MaxRosterAgents fully-qualified ids — 64 * 1024 =
// 65,536 ids, a few megabytes at ids.MaxAgentIDLen. Each factor alone is
// generous for the "a handful of laptop buses relaying to each other" case this
// project is built for; together they are the ceiling an operator can reason
// about, which is why both are stated rather than only one.
const MaxPeers = 64

// MaxRosterUpdateEntries bounds one incremental roster update (Added plus
// Removed, together — not each). A peer with more churn than this to report has
// diverged far enough that a fresh handshake, which carries a full snapshot and
// resets the version, is the correct recovery.
const MaxRosterUpdateEntries = 256

// MaxRosterUpdateBytes bounds an encoded roster update read from the network.
//
// DERIVED, not guessed: MaxRosterUpdateEntries (256) ids of at most
// ids.MaxAgentIDLen (150) bytes, each costing two quotes and a comma, is
// 256 * 153 = 39,168 bytes, plus a bus id (64), a version (20) and the field
// names. 64 KiB leaves ~1.6x headroom, so a legal maximum-size update can
// always be encoded and can never be refused by this cap.
const MaxRosterUpdateBytes = 64 << 10

// Registry failures. All are checkable with errors.Is.
var (
	// ErrPeerBusIDCollision reports a peer whose bus id differs from one we
	// already know ONLY by ASCII case.
	ErrPeerBusIDCollision = errors.New("relay: peer bus id collides with a known peer")

	// ErrTooManyPeers reports the MaxPeers cap.
	ErrTooManyPeers = errors.New("relay: too many peer buses")

	// ErrUnknownPeer reports a roster update for a bus that has not handshaked.
	ErrUnknownPeer = errors.New("relay: roster update from an unknown peer")

	// ErrStaleRosterUpdate reports an update whose version is not strictly
	// greater than the version already applied for that peer.
	ErrStaleRosterUpdate = errors.New("relay: stale roster update")

	// ErrInvalidRosterUpdate reports a malformed or self-contradictory update.
	ErrInvalidRosterUpdate = errors.New("relay: invalid roster update")
)

// RosterUpdate is one incremental change to a peer's roster, pushed by that
// peer as its own agents come and go.
type RosterUpdate struct {
	// BusID is the peer whose roster this describes. A peer describes ITS OWN
	// roster and nobody else's.
	BusID string `json:"bus_id"`

	// Version is the peer's own monotonic roster epoch. See ApplyRosterUpdate
	// for why a peer minting this does not breach invariant 1.
	Version uint64 `json:"version"`

	// Added and Removed are fully-qualified ids inside BusID's namespace. They
	// must be disjoint.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// RosterUpdateFingerprint computes the canonical fingerprint of a roster update,
// so a wiring site can tell invariant 10's legitimate retry (same key, same
// payload) from a protocol violation (same key, DIFFERENT payload).
//
// The field list is FIXED and ordered, as idem.ComputeFingerprint requires every
// call site to document: idem.OpPeerEnrol (domain separation — see the caveat
// below), the bus id, the version in decimal, a literal "added" marker, each
// added id in order, a literal "removed" marker, then each removed id in order.
//
// The two LITERAL MARKERS are not decoration. Without them, moving an id from
// Added to Removed while keeping the concatenation identical would produce the
// same fingerprint — {added:[x], removed:[]} and {added:[], removed:[x]} are
// opposite instructions, and a fingerprint that could not tell them apart would
// let a retry silently invert an update. Order within each list is significant
// for the same reason it is in peerFingerprint and relayFingerprint.
//
// # THE OPERATION IS WRONG AND internal/idem MUST FIX IT
//
// A roster push is its own mutating operation and deserves its own
// idem.Operation. internal/idem's Operation set is CLOSED and validated
// (OpEnrol, OpSend, OpBroadcast, OpLeave, OpPeerEnrol, OpRelay); there is no
// OpRosterSync, and adding one is a change to internal/idem, which is outside
// this task's file boundary. OpPeerEnrol is used as the nearest correct
// neighbour — a roster push IS the ongoing continuation of the peer-enrol
// exchange — and the consequence is bounded but real: a peer that reuses ONE
// key across a peer-enrol AND a roster push lands both in the same scope, so the
// second is reported as a violation. That is a peer bug either way, but it would
// be diagnosed as the wrong bug. Recorded as a blocker on internal/idem.
func RosterUpdateFingerprint(u RosterUpdate) idem.Fingerprint {
	fields := make([][]byte, 0, len(u.Added)+len(u.Removed)+5)
	fields = append(fields,
		[]byte(idem.OpPeerEnrol),
		[]byte(u.BusID),
		[]byte(strconv.FormatUint(u.Version, 10)),
		[]byte("added"),
	)
	for _, id := range u.Added {
		fields = append(fields, []byte(id))
	}
	fields = append(fields, []byte("removed"))
	for _, id := range u.Removed {
		fields = append(fields, []byte(id))
	}
	return idem.ComputeFingerprint(fields...)
}

// PeerView is a read-only snapshot of one peer's registry entry.
type PeerView struct {
	BusID         string
	BaseURL       string
	Agents        int
	RosterVersion uint64
	UpdatedAt     time.Time
}

// peerState is the mutable per-peer entry. The roster is a SET rather than a
// slice because the operations on it are membership, add and remove, and a
// slice would turn each of those into a scan of up to MaxRosterAgents entries
// while the registry lock is held.
type peerState struct {
	busID   string
	baseURL string
	roster  map[string]struct{}
	version uint64
	updated time.Time
}

// RegistryOptions configures NewRegistry.
type RegistryOptions struct {
	// BusID is THIS bus's server-minted id. It is what every peer claim is
	// measured against, and it is what Route refuses to route to.
	BusID string

	// MaxPeers overrides the MaxPeers cap. Zero means MaxPeers; negative is a
	// construction error.
	MaxPeers int

	// Logger is optional; nil discards.
	Logger *logging.Logger
}

// Registry is the routing table: which peer bus owns which agents, and how
// fresh our copy of each peer's roster is. It is safe for concurrent use.
//
// # Routing does NOT depend on roster freshness, and that is the whole design
//
// Route resolves by the BUS HALF of a fully-qualified id, never by roster
// membership. That is CLAUDE.md invariant 2 doing its job: "<bus-id>.<agent-id>"
// NAMES ITS OWN OWNER, so a DM to an agent that enrolled on a peer thirty
// seconds ago routes correctly even though our copy of that peer's roster
// predates it. Roster membership is a DISCOVERY and LISTING convenience —
// exposed separately as Knows — and never a routing precondition.
//
// This is why RELAY-2's "ongoing roster sync" is a CONVENIENCE rather than a
// correctness dependency, and it is worth being explicit about because the
// opposite design is the obvious one to reach for: a registry that routed by
// roster membership would drop every message to a newly-enrolled remote agent
// until the next sync landed, turning an eventual-consistency mechanism into a
// delivery guarantee it cannot provide.
type Registry struct {
	mu       sync.RWMutex
	busID    string
	maxPeers int
	log      *logging.Logger

	// peers is keyed on the LOWERCASED bus id, which is what makes the
	// case-collision refusal in UpsertPeer possible at all: two peers whose ids
	// differ only by case must land on one key so the second one can be seen
	// and refused. The canonical spelling is kept inside peerState.
	peers map[string]*peerState
}

// NewRegistry validates opts and returns an empty routing table.
func NewRegistry(opts RegistryOptions) (*Registry, error) {
	if err := ids.ValidateBusID(opts.BusID); err != nil {
		return nil, fmt.Errorf("relay: registry bus id: %w", err)
	}
	if opts.MaxPeers < 0 {
		return nil, fmt.Errorf("relay: RegistryOptions.MaxPeers is %d; it must be zero (meaning %d) or positive", opts.MaxPeers, MaxPeers)
	}
	max := opts.MaxPeers
	if max == 0 {
		max = MaxPeers
	}
	log := opts.Logger
	if log == nil {
		log = logging.New(io.Discard, logging.LevelError)
	}
	return &Registry{busID: opts.BusID, maxPeers: max, log: log, peers: make(map[string]*peerState)}, nil
}

// BusID reports this bus's own id.
func (r *Registry) BusID() string { return r.busID }

// UpsertPeer installs the FULL roster from a completed handshake.
//
// It REPLACES any existing roster for that peer and RESETS RosterVersion to 0.
// Both halves are deliberate: a handshake carries a complete snapshot, so
// merging it with what we already held would preserve entries the peer has just
// told us it no longer has; and the version is the PEER's counter, which a peer
// that restarted has itself reset — so keeping our old high-water mark would
// make every subsequent update from it look stale forever. A re-handshake is
// the documented recovery from a regressed version, and it only works if it
// resets.
//
// # A bus id that CASE-COLLIDES with a known peer is REFUSED
//
// doc.go's "What the gating tasks must not forget" names this as a debt this
// package owed, and this is where it is paid. ValidatePeerBusID case-folds a
// claim against OUR id alone, so two DIFFERENT peers calling themselves
// "bus-abc" and "BUS-ABC" both validate. Letting both into the routing table
// would put a confusable in the routing subject, which is the same
// social-engineering surface ids.ValidateAgentName refuses uppercase names to
// avoid — and worse here, because loop prevention compares hops
// case-insensitively (PathContains), so the two peers would be
// indistinguishable to the split horizon while being distinct routing targets.
func (r *Registry) UpsertPeer(peer PeerRoster) error {
	if err := ValidatePeerBusID(r.busID, peer.BusID); err != nil {
		return err
	}
	// The roster is re-validated rather than trusted to have come from
	// ValidatePeerEnrollRequest: a PeerRoster is an ordinary struct that any
	// caller can build, and this is the boundary of the routing table.
	agents, err := ValidateRoster(r.busID, peer.BusID, peer.Agents)
	if err != nil {
		return err
	}

	folded := strings.ToLower(peer.BusID)

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, known := r.peers[folded]
	if known && existing.busID != peer.BusID {
		return fmt.Errorf("%w: this registry already knows peer %q, and %q differs from it only by ASCII case; a confusable in the routing subject is refused at the door", ErrPeerBusIDCollision, existing.busID, peer.BusID)
	}
	if !known && len(r.peers) >= r.maxPeers {
		return fmt.Errorf("%w: %d peers are registered, the limit; total routing-table memory is bounded by %d peers x %d roster entries", ErrTooManyPeers, len(r.peers), r.maxPeers, MaxRosterAgents)
	}

	roster := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		roster[a] = struct{}{}
	}
	baseURL := ""
	if known {
		// The base URL is set out of band (SetPeerBaseURL) and is not part of a
		// handshake payload, so a re-handshake must not silently forget where
		// the peer lives.
		baseURL = existing.baseURL
	}
	r.peers[folded] = &peerState{
		busID:   peer.BusID,
		baseURL: baseURL,
		roster:  roster,
		version: 0,
		updated: time.Now().UTC(),
	}
	r.log.Info("peer registered",
		"local_bus", r.busID,
		"peer_bus", peer.BusID,
		"peer_agents", len(roster),
		"replaced", known,
	)
	return nil
}

// SetPeerBaseURL records where a known peer lives, so a forwarder can dial it.
//
// It is separate from UpsertPeer because the address is OPERATOR configuration,
// not something a peer asserts about itself in a handshake: a peer-supplied
// base URL would be an unauthenticated instruction telling us where to send
// every message addressed to its agents.
func (r *Registry) SetPeerBaseURL(busID, baseURL string) error {
	if _, err := peerURL(baseURL, PeerRelayPath); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.peers[strings.ToLower(busID)]
	if !ok || st.busID != busID {
		return fmt.Errorf("%w: %q", ErrUnknownPeer, busID)
	}
	st.baseURL = baseURL
	return nil
}

// RemovePeer forgets a peer entirely. It reports whether one was removed.
func (r *Registry) RemovePeer(busID string) bool {
	folded := strings.ToLower(busID)
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.peers[folded]
	if !ok || st.busID != busID {
		return false
	}
	delete(r.peers, folded)
	r.log.Info("peer removed", "local_bus", r.busID, "peer_bus", busID)
	return true
}

// ApplyRosterUpdate applies one incremental change to a known peer's roster.
//
// # The validation order, and why each step is where it is
//
//  1. THE PEER MUST ALREADY BE KNOWN. An update can never CREATE a peer. If it
//     could, roster sync would be a SECOND, UNGATED ENROLMENT PATH — a stranger
//     could install itself in our routing table with one POST, bypassing the
//     handshake and, once they land, INVITE-PEERGUARD and MTLS-RELAYGUARD
//     entirely. An unknown peer must handshake first, full stop.
//
//  2. The ENTRY COUNT, before any id is parsed, so a hostile update cannot make
//     us parse MaxRosterUpdateEntries+N ids before we decline.
//
//  3. THE VERSION MUST BE STRICTLY GREATER than the one we hold. Updates cross
//     an unordered asynchronous channel — two pushes can overtake each other,
//     and a retry can arrive after a later update — so a monotonic per-peer
//     version is what stops a late "removed: alice" resurrecting an agent that
//     has since re-enrolled, or a late "added" reinstating one that departed.
//
//     The version is minted BY THE PEER FOR ITS OWN NAMESPACE, and that does
//     not breach invariant 1: invariant 1 is about ids WE mint, and a peer's
//     roster epoch is a fact about the peer's own state that only the peer can
//     know. A peer can inflate its own version, and the only thing that affects
//     is its own namespace.
//
//     THE RESIDUAL, STATED RATHER THAN GLOSSED: a peer that RESTARTS and loses
//     its version counter regresses to a low number, and every update it sends
//     is then refused as stale — permanently, because incremental updates alone
//     cannot recover from a regressed version. The recovery is a RE-HANDSHAKE,
//     which resets the version to 0 and carries a full snapshot.
//     ErrStaleRosterUpdate is how an operator sees that happening; it is a
//     signal, not noise.
//
//  4. Every id must parse and must be inside u.BusID's namespace —
//     ValidateRoster's rules, restated for the two delta lists. An id in OUR
//     namespace is ErrBusIDCollision (spoofing: a peer able to add
//     "<us>.alice-1" to its own roster could make us route our own agent's mail
//     off-bus). An id in a THIRD bus's namespace is ErrInvalidRosterUpdate: a
//     peer speaks only for its own agents, and learning about a third bus this
//     way would be transitive federation arriving as a side effect.
//
//  5. Added and Removed must be DISJOINT. An id in both is ambiguous intent,
//     and any resolution we picked would be a guess about what the peer meant.
//
//  6. The RESULTING roster must not exceed MaxRosterAgents. A peer must not be
//     able to grow our routing table past the cap the handshake enforces, one
//     increment at a time.
//
//  7. APPLY ATOMICALLY: everything above is checked first, then the mutation
//     happens under the lock with no further failure possible. A half-applied
//     update would leave a routing table that nobody validated, and there is
//     nothing to roll back to once the caller has been told it failed.
//     Removals run before additions.
//
// Adding an id already present, or removing one that is absent, is a NO-OP
// rather than an error: the update is a set operation, the version already
// suppresses replays, and erroring would make a correct peer's retry look like
// a fault.
func (r *Registry) ApplyRosterUpdate(u RosterUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 1. Known peers only. An update never creates one.
	st, ok := r.peers[strings.ToLower(u.BusID)]
	if !ok || st.busID != u.BusID {
		return fmt.Errorf("%w: bus %q has not completed a handshake; a roster update must never create a peer, or roster sync would be a second, ungated enrolment path", ErrUnknownPeer, u.BusID)
	}

	// 2. The count, before any parsing.
	if n := len(u.Added) + len(u.Removed); n > MaxRosterUpdateEntries {
		return fmt.Errorf("%w: %d entries (added plus removed), but one update carries at most %d; a peer with more churn than this should re-handshake with a full snapshot", ErrInvalidRosterUpdate, n, MaxRosterUpdateEntries)
	}

	// 3. Strictly monotonic.
	if u.Version <= st.version {
		return fmt.Errorf("%w: peer %q sent version %d, but version %d is already applied; updates cross an unordered channel, so only a strictly greater version is accepted — if this peer restarted and lost its counter, it must RE-HANDSHAKE, which resets the version and carries a full snapshot", ErrStaleRosterUpdate, u.BusID, u.Version, st.version)
	}

	// 4/5. Every id, in both lists, then disjointness.
	added, err := r.validateDelta(u.BusID, "added", u.Added)
	if err != nil {
		return err
	}
	removed, err := r.validateDelta(u.BusID, "removed", u.Removed)
	if err != nil {
		return err
	}
	for _, id := range added {
		for _, other := range removed {
			if id == other {
				return fmt.Errorf("%w: %q appears in both added and removed; the intent is ambiguous and guessing at it would silently pick one", ErrInvalidRosterUpdate, id)
			}
		}
	}

	// 6. The resulting size, computed before anything is mutated.
	size := len(st.roster)
	for _, id := range removed {
		if _, present := st.roster[id]; present {
			size--
		}
	}
	for _, id := range added {
		if _, present := st.roster[id]; !present {
			size++
		}
	}
	if size > MaxRosterAgents {
		return fmt.Errorf("%w: applying this update would leave peer %q with %d agents, over the %d cap the handshake enforces", ErrRosterTooLarge, u.BusID, size, MaxRosterAgents)
	}

	// 7. Mutate. Nothing below can fail.
	for _, id := range removed {
		delete(st.roster, id)
	}
	for _, id := range added {
		st.roster[id] = struct{}{}
	}
	st.version = u.Version
	st.updated = time.Now().UTC()
	r.log.Info("peer roster updated",
		"local_bus", r.busID,
		"peer_bus", u.BusID,
		"version", u.Version,
		"added", len(added),
		"removed", len(removed),
		"peer_agents", len(st.roster),
	)
	return nil
}

// validateDelta applies ValidateRoster's per-entry rules to one delta list.
// The caller holds the lock; nothing here mutates.
func (r *Registry) validateDelta(peerBusID, list string, ids0 []string) ([]string, error) {
	out := make([]string, 0, len(ids0))
	seen := make(map[string]struct{}, len(ids0))
	for i, id := range ids0 {
		busPart, _, _, err := ids.ParseAgentID(id)
		if err != nil {
			return nil, fmt.Errorf("%w: %s entry %d: %v", ErrInvalidRosterUpdate, list, i, err)
		}
		if strings.EqualFold(busPart, r.busID) {
			return nil, fmt.Errorf("%w: %s entry %d is %q, whose bus half is this bus (%q, compared case-insensitively); a peer may not assert ids inside our namespace", ErrBusIDCollision, list, i, id, r.busID)
		}
		if busPart != peerBusID {
			return nil, fmt.Errorf("%w: %s entry %d is %q, but bus %q may only describe its own agents", ErrInvalidRosterUpdate, list, i, id, peerBusID)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: %s entry %d is %q, which appears more than once; a delta list is a set", ErrInvalidRosterUpdate, list, i, id)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// Route resolves a fully-qualified agent id to the peer bus that owns it.
//
// IT RESOLVES BY THE BUS HALF, NOT BY ROSTER MEMBERSHIP — see the Registry doc
// for why that is the design and not a shortcut. Two refusals are deliberate:
//
//   - OUR OWN bus id is never routed. That is a LOCAL delivery, and relaying it
//     would send an agent's message out to the network and (via the split
//     horizon and the ingress loop check) get it dropped, i.e. lose it.
//   - A malformed id is never routed. ParseAgentID is the one definition of a
//     legal id; a caller that has not been through it has not established that
//     the string names anybody.
//
// The bus half must match a known peer's id EXACTLY, not just case-insensitively.
// The registry is keyed case-insensitively so a confusable peer can be REFUSED
// at UpsertPeer, but once a peer is admitted its canonical spelling is the only
// one that routes — matching ValidateRoster, which requires a roster entry's bus
// half to equal the peer's id byte for byte.
func (r *Registry) Route(agentID string) (string, bool) {
	busPart, _, _, err := ids.ParseAgentID(agentID)
	if err != nil {
		return "", false
	}
	if strings.EqualFold(busPart, r.busID) {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.peers[strings.ToLower(busPart)]
	if !ok || st.busID != busPart {
		return "", false
	}
	return st.busID, true
}

// Knows reports whether agentID appears in our copy of its owner's roster.
//
// IT IS NOT THE ROUTING PREDICATE — Route is. Knows answers a DISCOVERY
// question ("should this agent appear in a federated agent list?"), and its
// answer is only as fresh as the last roster sync. A caller that used it to
// decide whether to route would drop every message to an agent that enrolled
// remotely since our last update.
func (r *Registry) Knows(agentID string) bool {
	busPart, _, _, err := ids.ParseAgentID(agentID)
	if err != nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.peers[strings.ToLower(busPart)]
	if !ok || st.busID != busPart {
		return false
	}
	_, present := st.roster[agentID]
	return present
}

// BroadcastTargets returns every known peer that is NOT already on busPath —
// the egress split horizon (NextHopAllowed) applied to fan-out.
//
// It never returns our own bus: our bus is not a peer, and a broadcast reaching
// our own agents is a local delivery that has already happened by the time
// anything is being forwarded.
//
// The result is sorted, so a caller's fan-out order is deterministic and a test
// asserting on it is not flaky.
func (r *Registry) BroadcastTargets(busPath []string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.peers))
	for _, st := range r.peers {
		if !NextHopAllowed(busPath, st.busID) {
			continue
		}
		out = append(out, st.busID)
	}
	sort.Strings(out)
	return out
}

// Peers returns a snapshot of every known peer, sorted by bus id.
func (r *Registry) Peers() []PeerView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PeerView, 0, len(r.peers))
	for _, st := range r.peers {
		out = append(out, PeerView{
			BusID:         st.busID,
			BaseURL:       st.baseURL,
			Agents:        len(st.roster),
			RosterVersion: st.version,
			UpdatedAt:     st.updated,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusID < out[j].BusID })
	return out
}

// Roster returns a peer's known agents (sorted, freshly allocated) and the
// version they are current as of.
//
// The slice is COPIED out under the lock: handing a caller the registry's own
// map keys would let it observe a partially-applied update, and a snapshot the
// caller may hold indefinitely must not be one another goroutine is mutating.
func (r *Registry) Roster(busID string) ([]string, uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.peers[strings.ToLower(busID)]
	if !ok || st.busID != busID {
		return nil, 0, false
	}
	out := make([]string, 0, len(st.roster))
	for id := range st.roster {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, st.version, true
}

// PeerBaseURL reports where a known peer lives, and whether one is known.
//
// Its signature is exactly ForwarderOptions.PeerBaseURL's, so a wiring site
// can pass `registry.PeerBaseURL` directly instead of hand-writing its own
// closure over the Registry's internals — which is the defect this method
// fixes: every existing wiring site was doing that, and each one is a fresh
// chance to get the locking wrong.
//
// It is SAFE FOR CONCURRENT USE, the same guarantee LocalRoster's doc states
// on its two call sites (ClientConfig.LocalRoster, handshake.Config's
// LocalRoster): this method takes the same RLock as Route and Knows, and it
// is called concurrently by Enqueue and by every per-peer worker goroutine.
// That concurrency is also what makes a RemovePeer or SetPeerBaseURL
// observable to a job already sitting in a peer's retry queue: forward.go's
// attempt re-resolves the address on every attempt rather than freezing it at
// enqueue time precisely so a de-peering or an address move takes effect on
// the NEXT attempt instead of being invisible for the rest of the retry
// horizon (see "THE ADDRESS IS RE-RESOLVED ON EVERY ATTEMPT" in forward.go).
//
// An empty base URL is folded into "not found": SetPeerBaseURL is the only
// way to give a peer a non-empty address, so a peer that has handshaked but
// never had its address configured reports (_, false) rather than ("", true),
// which would look like a routable, contactless peer to a caller checking
// only the second return value.
func (r *Registry) PeerBaseURL(busID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.peers[strings.ToLower(busID)]
	if !ok || st.busID != busID || st.baseURL == "" {
		return "", false
	}
	return st.baseURL, true
}
