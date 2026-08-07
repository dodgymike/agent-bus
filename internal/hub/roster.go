package hub

import (
	"sort"
	"sync"
	"time"
)

// Agent is one entry of the bus's agent list, as GET /v1/agents returns it.
//
// It carries NO key material. The roster entry in internal/auth holds the
// agent's Ed25519 public key, and although a public key is by definition public,
// there is no reason for the agent list to hand every enrolled agent a copy of
// every other agent's credential material — the list exists so agents can
// address each other.
type Agent struct {
	// AgentID is the fully-qualified "<bus-id>.<agent-id>" (invariant 2). It is
	// the ONLY string /v1/send accepts as a recipient.
	AgentID string

	// Name is the short name the agent asked for at enrolment.
	Name string

	// EnrolledAt is when this bus accepted the enrolment.
	//
	// It must be the agent's ORIGINAL enrolment instant, not the instant a
	// restart recovered it. This is the enrolment epoch every read path filters
	// with (store.Message.VisibleTo), so a source that reported "now" for a
	// recovered agent would silently delete that agent's history at every
	// restart while looking perfectly healthy.
	EnrolledAt time.Time
}

// RosterSource is the hub's READ-ONLY view of the roster. It is injected, and
// whichever implementation is injected IS the single source of truth about who
// is enrolled on this bus.
//
// # Why the hub keeps no roster of its own
//
// It used to. Until AUTH-7 the hub held a private map fed by the HTTP enrolment
// handler calling NoteEnrolment, which was honest only while the two views had
// IDENTICAL LIFETIMES — true exactly as long as both were memory-only. Durable
// enrolment (internal/auth's WALRoster) ended that: auth's roster now survives a
// restart, so a hub with its own copy would come back EMPTY while every agent
// still authenticated perfectly. The result is a bus that authenticates everyone
// and serves nobody — 403 ErrUnknownSender on every send, 404 on every
// recipient, both read paths failing closed — and it fails in the direction that
// looks like the auth layer working, so it is slow to diagnose.
//
// The fix is not a second copy that is refreshed more carefully. It is having no
// copy: there is exactly one roster, it lives behind this interface, and the hub
// reads through to it on every check. Do NOT reintroduce a cache here, and do
// NOT add a "note this enrolment" method — either one recreates the divergence
// by construction.
//
// An implementation MUST be safe for concurrent use: Lookup is on the per-send
// and per-read hot path and is called from request goroutines.
type RosterSource interface {
	// Lookup returns the entry for agentID and whether it is enrolled.
	//
	// It exists ALONGSIDE List, rather than being expressed as a scan of it,
	// because Enrolled and enrolmentEpoch call it on every publish, every
	// history read and every long-poll wake — an O(n) sorted deep copy on those
	// paths would put the whole roster's allocation cost behind each one.
	Lookup(agentID string) (Agent, bool)

	// List returns every enrolled agent, sorted by AgentID, in a FRESHLY
	// ALLOCATED slice the caller may retain and mutate. Agents hands it
	// straight to the HTTP layer, so a source that returned its own backing
	// array would let a response handler edit the roster.
	List() []Agent
}

// StaticRoster is an in-memory RosterSource with an Add method: the roster of a
// bus that has no durable enrolment (a test, or a build wired without
// internal/auth). It is safe for concurrent use.
//
// USE IT ALONE OR NOT AT ALL. Whichever source is injected into Options.Roster
// is THE roster; this type is not a second view of one that lives elsewhere, and
// populating it alongside another source is precisely the divergence AUTH-7
// removed — see RosterSource. If a durable roster exists, adapt THAT and pass
// it; do not mirror it into one of these.
//
// The zero value is not usable; construct with NewStaticRoster.
type StaticRoster struct {
	// mu guards byID. An RWMutex because Lookup is on the per-send and per-read
	// hot path while Add happens once per enrolment.
	mu   sync.RWMutex
	byID map[string]Agent
}

// NewStaticRoster returns an empty roster.
func NewStaticRoster() *StaticRoster {
	return &StaticRoster{byID: make(map[string]Agent)}
}

// Add records that a is enrolled. FIRST WRITE WINS: an agent id is never
// rebound (invariant 1, and auth.Roster.Put's refuse-don't-overwrite contract),
// so a later entry for an id already present could only ever be wrong — an
// enrolment replayed from the idempotency table reports the ORIGINAL
// EnrolledAt, and anything else is an impostor. An empty AgentID is ignored: it
// is addressable by nobody and would sit in the agent list as a phantom.
func (r *StaticRoster) Add(a Agent) {
	if a.AgentID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[a.AgentID]; ok {
		return
	}
	r.byID[a.AgentID] = a
}

// Lookup implements RosterSource.
func (r *StaticRoster) Lookup(agentID string) (Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byID[agentID]
	return a, ok
}

// List implements RosterSource: a fresh slice, sorted by AgentID.
func (r *StaticRoster) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Agent, 0, len(r.byID))
	for _, a := range r.byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Agents returns the agent list, sorted by agent id so the response is stable
// between calls and diffable by an operator.
//
// It re-sorts what the source returned rather than trusting it. RosterSource.List
// requires sorted output, but the ORDER IS THIS PACKAGE'S CONTRACT — /v1/agents
// promises it — and Go randomises map iteration, so a source that forgot would
// turn a stable listing into one that shuffles between calls: a flake nobody
// would trace back to an injected implementation. One sort of a roster-sized
// slice is not a cost worth arguing about.
func (h *Hub) Agents() []Agent {
	out := h.roster.List()
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Enrolled reports whether agentID is on this bus's roster.
//
// TOCTOU NOTE: publish calls this BEFORE taking writeMu, so an agent could in
// principle leave between the check and the write. There is no way to leave
// today — no roster source has a removal path — so the window is empty. The day
// AUTH-4 adds leave or revocation it stops being empty, and the check must move
// inside writeMu (or the roster must be re-read there). Written down because
// the bug it would produce, a message durably accepted from a departed agent,
// is one nobody would look for here.
func (h *Hub) Enrolled(agentID string) bool {
	_, ok := h.roster.Lookup(agentID)
	return ok
}

// enrolmentEpoch returns when agentID enrolled, and whether it is on the
// roster at all.
//
// This is the value every read path filters with (store.Message.VisibleTo).
// A caller that cannot find the agent MUST NOT fall back to the zero time —
// that disables the epoch check — which is why this returns the two results
// separately rather than a zero Time meaning both "not enrolled" and "no
// restriction".
func (h *Hub) enrolmentEpoch(agentID string) (time.Time, bool) {
	a, ok := h.roster.Lookup(agentID)
	if !ok {
		return time.Time{}, false
	}
	return a.EnrolledAt, true
}

// noteRecoveredIdentities reports, at ERROR, every agent id that a message
// replayed from disk names but that the roster does NOT hold.
//
// # What this is, and why it survived the deletion of NoteEnrolment
//
// It is the id-reuse detector. NoteEnrolment used to make the check on the live
// path — "this id already appears in messages recovered from disk, so a
// DIFFERENT keypair previously held it" — and deleting that method without
// re-siting the check would have deleted the alarm along with the map.
//
// The startup form is the stronger one anyway, because it does not have to wait
// for the reuse to happen. An id in the recovered traffic and absent from the
// durable roster is an id whose HOLDER IS GONE: nothing on this bus can
// authenticate as it, nothing will ever be delivered to it, and the name it was
// minted from is free to be enrolled again. Whether that re-mint produces the
// SAME id is a question about the suffix floors, not about this map, and the
// answer is supposed to be no (ids.DurableNameSuffixes persists each name's
// floor BEFORE issuing it) — which is exactly why a mismatch here is worth
// shouting about: it is the shape a floors file that was lost, restored from a
// backup, or never written leaves behind.
//
// It is a LOG LINE AND NOT AN ERROR, deliberately. By the time this runs both
// facts are already durable, so refusing to start would not un-write either one;
// it would turn a diagnosable inconsistency into an outage. The standing rule
// for this project is that a discard or a reuse is never SILENT, not that it is
// always fatal (invariant 6, 2026-08-02).
//
// A pre-AUTH-7 data directory trips this on its first start, and that is
// correct rather than a false alarm: those agents really were enrolled into a
// memory-only roster that really was lost, and they really must re-enrol.
func (h *Hub) noteRecoveredIdentities() {
	// # AN EMPTY MAP IS NOT PROOF OF ANYTHING IF RECORDS WERE DISCARDED
	//
	// This check runs on ids harvested from message records that DECODED. A
	// record that did not decode contributed no ids, so a discard makes the map
	// a strict undercount — and "no ids recovered" then looks exactly like a
	// fresh data directory, which is the one case where returning silently is
	// correct. Reported BEFORE the early return, because the case that matters
	// most is the one where the map is empty precisely BECAUSE everything was
	// discarded.
	//
	// That is not hypothetical: it is the FIRST START AFTER THIS UPGRADE. Every
	// message record written before SIGN-6 is store.RecordVersion 1, this build
	// understands 2, so every one of them is refused by store.Decode — the exact
	// start on which this detector is supposed to fire, and on which it would
	// otherwise report success having examined nothing.
	if h.undecodableMessages > 0 {
		h.log.Error("THE AGENT-ID REUSE CHECK RAN ON INCOMPLETE INPUT: message records were discarded during recovery, so the ids they named were never harvested and an id being reused could not be detected. Treat a clean result below as UNPROVEN, not as an all-clear. On the first start after a store record-schema change this is expected and every message record is discarded; at any other time it means message records were damaged",
			"undecodable_message_records", h.undecodableMessages,
			"ids_recovered", len(h.recovered),
		)
	}
	if len(h.recovered) == 0 {
		return
	}
	missing := make([]string, 0, len(h.recovered))
	for id := range h.recovered {
		if _, ok := h.roster.Lookup(id); !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	// Sorted so two starts over the same data dir produce the same line, and
	// so an operator can diff them.
	sort.Strings(missing)
	h.log.Error("AGENT IDS RECOVERED FROM MESSAGES ARE ABSENT FROM THE ROSTER: a different keypair once held each of these ids, nothing can authenticate as them now, and the names they were minted from are free to be enrolled again. Their traffic is NOT delivered to a new holder (the enrolment epoch blocks it), but a re-minted id would carry a prior history (invariant 1). Expected once when upgrading a data directory that predates durable enrolment; at any other time it means enrolment records were lost while message records survived",
		"count", len(missing),
		"agent_ids", missing,
		"roster_size", len(h.roster.List()),
	)
}
