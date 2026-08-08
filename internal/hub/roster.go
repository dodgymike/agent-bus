package hub

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
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

// RemoteRouter answers ONE question, for a recipient this bus does NOT hold:
// is that fully-qualified id reachable through a peer bus, and which peer.
//
// It is the egress-admission seam (RELAY-16). It is OPTIONAL — Options.RemoteRouter
// may be nil, and a nil router means this bus is not federated, so every recipient
// it does not hold is refused with ErrUnknownRecipient exactly as it was before
// this interface existed. That equivalence is the point: the seam lands before
// anything is wired to it, and the non-federated bus cannot tell the difference.
//
// # What it is NOT
//
// It is not delivery. Route is a QUESTION asked BEFORE the durable write, so that
// a recipient nobody can reach costs nothing durable and gets the truthful 404 it
// got before. Answering true is an assertion that the message is DELIVERABLE, not
// a request to deliver it: the message becomes durable on this bus first
// (invariant 4), and the egress path that carries it to peerBusID is a separate
// mechanism with its own durable outbox. A router that says "yes" for an id no
// peer actually holds converts an honest refusal into an accepted-then-dropped
// message, which is the one outcome this task exists to avoid.
//
// # THE SEQUENCING CONSTRAINT — DO NOT INJECT A ROUTER EARLY
//
// For that same reason, no router may be wired until the egress path that carries
// an admitted message onward exists and is durable. Injecting one sooner does not
// make the bus federated: it makes it accept messages it has no way to deliver,
// silently, having removed the truthful 404 that was protecting the client. An
// unwired seam refuses honestly; a wired seam with nowhere to send is exactly the
// failure this seam exists to prevent, reached through the front door.
//
// # The contract on the answer
//
// peerBusID must be a valid bus id (ids.ValidateBusID) and must NOT be this bus:
// this bus's own agents are the ROSTER'S business, and a router that claims one
// would be answering a question it was never asked. Both are checked by the hub
// and a violation is refused and logged at ERROR rather than trusted — the router
// is in-process code, but it is the component that turns a 404 into an
// acceptance, so its answer is validated like any other input.
//
// An implementation MUST be safe for concurrent use: Route is called from request
// goroutines, once per non-local recipient, on the send path.
type RemoteRouter interface {
	// Route reports the peer bus that agentID is reachable through, and whether
	// it is reachable at all. It must not block: it sits on the send path,
	// before the durable write, and every caller is holding a client's request
	// open.
	Route(agentID string) (peerBusID string, ok bool)
}

// routeRemote reports whether recipient is admissible as a REMOTE recipient, and
// through which peer.
//
// The order of the checks is the whole of its correctness:
//
//  1. NO ROUTER => not routable. This is what makes a nil router behaviourally
//     identical to the bus before RELAY-16: publish's recipient loop falls
//     straight through to the same ErrUnknownRecipient it always returned.
//  2. MALFORMED => not routable. A recipient that is not a fully-qualified
//     "<bus-id>.<agent-id>" names nobody (invariant 2), so there is nothing to
//     route and no router is consulted about it.
//  3. THIS BUS'S OWN NAMESPACE => not routable, WITHOUT asking the router. The
//     local roster is the ONLY authority on ids qualified with this bus, and this
//     line is what keeps that true: were a router able to admit
//     "<local-bus>.alpha-18446744073709551615", relay ingest could make the bus
//     accept a local id that was never minted here and permanently exhaust the
//     name "alpha" across every future restart (cca64afd's precondition; see
//     cmd/agent-bus/suffixfloors.go). The roster check therefore both PRECEDES
//     the durable write and remains the last word for local ids.
//  4. Only then is the router asked, and its answer is validated.
//
// BOTH COMPARISONS AGAINST THIS BUS'S OWN ID ARE CASE-INSENSITIVE, and that is
// deliberate rather than sloppy. ids.BusIDPattern admits both cases, but every
// comparison in internal/relay is folded (registry.go, peer.go, path.go), so the
// layer that will IMPLEMENT this interface already treats "PeerBus" and "peerbus"
// as one bus. A guard whose failure is PERMANENT — the exhausted agent name — must
// not be the looser of two comparisons, and folding here can only ever produce an
// additional REFUSAL, never an additional acceptance.
func (h *Hub) routeRemote(recipient string) (string, bool) {
	if h.router == nil {
		return "", false
	}
	busID, _, _, err := ids.ParseAgentID(recipient)
	if err != nil {
		return "", false
	}
	if strings.EqualFold(busID, h.busID) {
		return "", false
	}
	peer, ok := h.router.Route(recipient)
	if !ok {
		return "", false
	}
	// The answer is validated, not trusted. A router that returns ok with an
	// empty or malformed peer — or with THIS bus — has admitted a message to
	// nowhere, and admitting it would replace an honest 404 with silent loss.
	// Refusing here keeps the refusal truthful; logging it at ERROR keeps the
	// misconfiguration from being invisible (invariant 6's rule: never silent).
	//
	// THE PEER STRING ITSELF IS NOT ECHOED, only its LENGTH. ids.ValidateBusID
	// formats the offending id with %q and applies no length cap of its own, so
	// logging the error would put an arbitrarily long value into a log line — the
	// same echo ids.ParseAgentID checks its length first specifically to avoid.
	// The router is in-process code an operator wrote, and "your router returned a
	// peer bus id of N bytes that is not a legal bus id" locates the bug without
	// making the log a channel for whatever it returned.
	if err := ids.ValidateBusID(peer); err != nil {
		h.log.Error("REMOTE ROUTER RETURNED AN UNUSABLE PEER BUS ID: the recipient is being refused as unroutable rather than accepted for delivery to a bus that cannot be named; a message accepted here would be durably stored and never deliverable. The value is reported by LENGTH only, because it is unvalidated and headed for a log line",
			"recipient", recipient,
			"peer_bytes", len(peer),
		)
		return "", false
	}
	if strings.EqualFold(peer, h.busID) {
		h.log.Error("REMOTE ROUTER CLAIMED A RECIPIENT IS REACHABLE VIA THIS BUS ITSELF: only the roster may admit an id in this bus's namespace, and the recipient is not on it, so the send is refused",
			"recipient", recipient,
			"bus_id", h.busID,
		)
		return "", false
	}
	return peer, true
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

// noteRecoveredIdentities reports, at ERROR, every agent id IN THIS BUS'S OWN
// NAMESPACE that a message replayed from disk names but that the roster does NOT
// hold. Ids qualified with another bus are skipped — see the filter in the loop.
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
		// ONLY IDS IN THIS BUS'S OWN NAMESPACE ARE CANDIDATES (RELAY-16).
		//
		// This detector's claim is a strong one — "a different keypair once held
		// this id, and the name it was minted from is free again" — and it is a
		// claim about THIS bus's id space, which this bus's roster is the
		// authority on. Before egress admission every recovered id was local by
		// construction (a send could name no other kind), so no filter was needed;
		// once a message may durably name "<peer>.<agent>", every foreign
		// recipient is by definition absent from the local roster and would be
		// reported here on EVERY start, for ever, as a reuse that never happened.
		//
		// That is not merely noise, it is two defects. It makes the false half
		// drown the true half, which is the shape of a lost floors file and the
		// only reason this line exists. And the ids are chosen by whoever sent the
		// messages, so an unfiltered list is a client-influenced, uncapped value
		// re-emitted at every startup of a log that never compacts.
		//
		// cmd/agent-bus/suffixfloors.go and internal/auth/floors.go already filter
		// the same SHAPE for the same reason — a foreign id burned no LOCAL suffix
		// — though both compare exactly rather than folding. The difference is
		// deliberate and each is on its own safe side: an exact match there SKIPS
		// fewer ids and so derives a HIGHER floor, while folding here KEEPS more
		// ids and so reports MORE. Both err towards the loud answer.
		//
		// A malformed id is kept — it is qualified with nothing, it should never
		// have reached disk (store.Decode parses every sender and recipient), and
		// on the day one does it is worth shouting about.
		if bus, _, _, err := ids.ParseAgentID(id); err == nil && !strings.EqualFold(bus, h.busID) {
			continue
		}
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
