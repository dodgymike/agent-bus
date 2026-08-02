package hub

import (
	"sort"
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
	EnrolledAt time.Time
}

// NoteEnrolment records that agentID is enrolled on this bus.
//
// # Why the hub keeps its own roster view
//
// The authoritative roster is internal/auth's, and it is NOT reachable from
// here: auth.Roster exposes Put/Get/Len and no listing, and internal/auth is
// outside this epic's ownership. Rather than reach across that boundary, the
// HTTP enrolment handler — which already sees every accepted enrolment —
// reports it here.
//
// That is a narrower claim than it looks, and it is honest because the two
// views have IDENTICAL LIFETIMES. auth's roster is in memory only and is lost on
// restart (see auth.Roster's doc: "NOT durable ... until AUTH-3 lands"), and so
// is this one. Neither survives a restart, so neither can be more or less
// correct than the other across one. Within a process they are updated from the
// same call site, in the same request, and NoteEnrolment is idempotent so a
// replayed enrolment does not disturb it.
//
// WHEN AUTH-3 LANDS DURABLE ENROLMENT this must be replaced by a read of the
// authoritative roster — auth.Roster needs a List method and the hub needs it
// injected — or the two views will diverge on the first restart, with this one
// empty and auth's populated. Filed as a follow-up; do not leave this in place
// past AUTH-3.
func (h *Hub) NoteEnrolment(a Agent) {
	if a.AgentID == "" {
		return
	}
	h.rosterMu.Lock()
	defer h.rosterMu.Unlock()
	if _, ok := h.roster[a.AgentID]; ok {
		// Idempotent: an enrolment replayed from the idempotency table reports
		// the ORIGINAL EnrolledAt, and even so, first write wins. An agent id is
		// never rebound (invariant 1 plus auth.Roster.Put's refuse-not-overwrite
		// contract), so an update here could only ever be wrong.
		return
	}
	h.roster[a.AgentID] = a

	if _, reused := h.recovered[a.AgentID]; reused {
		// An id that is already on disk has just been handed out AGAIN. That is
		// invariant 1 broken, and it is broken UPSTREAM of here: enrolment is
		// not durable yet (AUTH-3), so cmd/agent-bus starts every name's suffix
		// counter at 1 on every boot, and the justification comment there —
		// "nothing in this path writes an agent id to disk" — stopped being
		// true the moment message records began naming senders and recipients.
		//
		// The hub cannot fix it: it does not own the minter. What it can do is
		// refuse to be SILENT about it, which is the standing rule for a
		// discard or a reuse in this project.
		//
		// What stops this being exploitable meanwhile is the enrolment epoch:
		// store.Message.VisibleTo will not deliver a message sent before this
		// enrolment, so the new holder of the id reads none of the previous
		// holder's traffic. What remains, and is NOT fixed here, is that the
		// new agent's FUTURE messages are attributed to an id a different
		// keypair used to hold.
		h.log.Error("AGENT ID REUSED: this id already appears in messages recovered from disk, so a DIFFERENT keypair previously held it (invariant 1). Recovered traffic is NOT delivered to the new holder (the enrolment epoch blocks it), but its future messages are attributed to an id with a prior history. The fix is durable enrolment and resumed name-suffix counters",
			"agent_id", a.AgentID,
			"name", a.Name,
			"follow_up", "AUTH-3",
		)
	}
}

// Agents returns the agent list, sorted by agent id so the response is stable
// between calls and diffable by an operator.
func (h *Hub) Agents() []Agent {
	h.rosterMu.RLock()
	out := make([]Agent, 0, len(h.roster))
	for _, a := range h.roster {
		out = append(out, a)
	}
	h.rosterMu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Enrolled reports whether agentID is on this bus's roster.
//
// TOCTOU NOTE: publish calls this BEFORE taking writeMu, so an agent could in
// principle leave between the check and the write. There is no way to leave
// today — this roster has no removal path — so the window is empty. The day
// AUTH-4 adds leave or revocation it stops being empty, and the check must move
// inside writeMu (or the roster must be re-read there). Written down because
// the bug it would produce, a message durably accepted from a departed agent,
// is one nobody would look for here.
func (h *Hub) Enrolled(agentID string) bool {
	h.rosterMu.RLock()
	defer h.rosterMu.RUnlock()
	_, ok := h.roster[agentID]
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
	h.rosterMu.RLock()
	defer h.rosterMu.RUnlock()
	a, ok := h.roster[agentID]
	if !ok {
		return time.Time{}, false
	}
	return a.EnrolledAt, true
}
