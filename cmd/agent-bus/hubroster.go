package main

import (
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/hub"
)

// hubRoster adapts the AUTHORITATIVE enrolment roster (internal/auth) to the
// read-only view the messaging core needs (hub.RosterSource).
//
// # Why the adapter lives HERE
//
// Because this is the composition root and neither package may depend on the
// other. internal/hub must not import internal/auth: the hub would then need the
// enrolment authority, the id minter and the session table to build a message
// store, and the dependency runs the wrong way round — auth issues the identity
// the hub consumes. internal/auth must not import internal/hub for the mirror
// reason. cmd/agent-bus is the one place that legitimately holds both, so the
// translation is fifteen lines here rather than a cycle anywhere else.
//
// # It is a VIEW, not a copy
//
// Every method reads through to the roster on the call. Nothing is cached and
// nothing is snapshotted, and that is the whole point of AUTH-7: the hub used to
// keep a second roster of its own, fed by the enrolment handler, which came back
// EMPTY after a restart while auth's durable one came back full — a bus that
// authenticated everyone and served nobody. One roster, read through, cannot
// diverge from itself. A snapshot taken at startup would reintroduce the same
// bug with a different cause: every agent enrolled after boot would authenticate
// and then be refused as an unknown sender.
//
// The wrapped value is the INTERFACE auth.Roster rather than *auth.WALRoster, so
// a bus wired with a different implementation (a test's MemoryRoster) needs no
// second adapter — and so that nothing here can reach a durable-write method.
type hubRoster struct{ roster auth.Roster }

// Lookup implements hub.RosterSource. It is on the per-send and per-read hot
// path: one map lookup and a small copy inside auth's roster, no allocation of
// the whole list.
func (v hubRoster) Lookup(agentID string) (hub.Agent, bool) {
	e, ok := v.roster.Get(agentID)
	if !ok {
		return hub.Agent{}, false
	}
	return agentOf(e), true
}

// List implements hub.RosterSource. auth's List already returns deep copies
// sorted by AgentID, so the slice built here is freshly allocated and shares no
// memory with the roster, as RosterSource requires.
func (v hubRoster) List() []hub.Agent {
	entries := v.roster.List()
	out := make([]hub.Agent, 0, len(entries))
	for _, e := range entries {
		out = append(out, agentOf(e))
	}
	return out
}

// agentOf projects one roster entry onto the agent-list shape.
//
// It carries THREE fields and deliberately drops the rest. The public keys, the
// invite provenance and the certificate bindings are credential material and
// enrolment audit; /v1/agents exists so agents can ADDRESS each other, and there
// is no reason to hand every enrolled agent a copy of every other agent's
// credentials to do that. Do not widen this without a decision recorded in
// DECISIONS.md.
//
// EnrolledAt is the load-bearing one. It is the agent's ORIGINAL enrolment
// instant, taken off the durable record rather than read from the clock at
// recovery, because it is the enrolment epoch every read path filters with
// (store.Message.VisibleTo). Substituting "now" here would silently delete every
// agent's history at each restart while every test that only checks membership
// stayed green.
func agentOf(e auth.RosterEntry) hub.Agent {
	return hub.Agent{
		AgentID:    e.AgentID,
		Name:       e.Name,
		EnrolledAt: e.EnrolledAt,
	}
}
