package httpapi

// AUTH-4 — POST /v1/leave, the agent SELF-LEAVE route.
//
// # WHAT THIS ROUTE IS, IN ONE SENTENCE
//
// It lets an authenticated agent durably remove ITSELF from the roster: the
// removal is a tombstone through the two-phase write path (invariants 4, 6), so
// a left agent stays gone across a restart, and its live sessions are dropped so
// it stops authenticating at once on the running bus.
//
// # SELF-LEAVE ONLY
//
// The subject is ALWAYS the authenticated principal (PrincipalFromContext),
// never a request-body field. There is no way to name another agent here, by
// design: operator-initiated revocation of a DIFFERENT agent is a separate
// concern (AUTH-7 / AUTH-ROSTER-RECLAIM) with a different authority model. A
// body field naming a victim would be an authorization hole, so this route reads
// nothing from the body at all.
//
// # IT IS AUTHENTICATED, AND IS NOT ON unauthenticatedRoutes
//
// It is registered through s.route behind authMiddleware, which is default-deny,
// so it requires a bearer session like every other agent route. It must NEVER be
// added to unauthenticatedRoutes: leaving the bus is an authenticated action —
// an anonymous caller able to remove an agent id it names would be a denial-of-
// service and an authorization hole at once (invariant 3).
//
// # NOTHING HERE DISCONNECTS ANYBODY (invariant 10)
//
// Leaving twice is a legitimate idempotent retry: auth.Service.Leave returns
// success with already_left=true and writes nothing the second time. A retry
// whose first attempt succeeded loses its session (it was dropped) and then
// meets authMiddleware's ordinary 401 — a clean HTTP refusal, never a dropped
// socket. No `disconnect(w)` appears in this file.

import (
	"net/http"
)

// RouteLeave is the agent self-leave route (AUTH-4).
const RouteLeave = "/v1/leave"

// LeaveResponseBody is the 200 body of POST /v1/leave.
//
// A fresh leave and an idempotent retry both answer 200: the difference is
// carried by already_left, not by the status code, because a retry that could
// not tell success from failure would either re-drive the write or give up on a
// departure that in fact happened (invariant 10).
type LeaveResponseBody struct {
	// AgentID is the agent that left — the caller's own authenticated id.
	AgentID string `json:"agent_id"`

	// Left is always true on a 200: after this call the agent is gone from the
	// roster, whether this call removed it or a previous one already did.
	Left bool `json:"left"`

	// AlreadyLeft is true when the agent was already absent — an idempotent retry
	// or a departure recorded earlier. No new tombstone was written.
	AlreadyLeft bool `json:"already_left"`

	// SessionsDropped is how many of the agent's live sessions this call removed.
	// It is 0 on an already_left retry and when the agent held none.
	SessionsDropped int `json:"sessions_dropped"`
}

// handleLeave serves POST /v1/leave.
func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	if !s.requirePOST(w, r) {
		return
	}

	// The subject is the AUTHENTICATED principal, never the body. authMiddleware
	// attaches it before this handler runs (the route is not on
	// unauthenticatedRoutes), so its absence would be a wiring bug, not a client
	// error — fail closed with 401 rather than acting on an empty id.
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		s.log.Error("POST /v1/leave reached with no principal in context; authMiddleware must attach one on every authenticated route",
			"request_id", RequestIDFromContext(r.Context()),
		)
		s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "authentication required")
		return
	}

	res, err := s.auth.Leave(principal.AgentID)
	if err != nil {
		// principal.AgentID is SERVER-MINTED, so ErrUnknownAgent (a malformed id)
		// is unreachable here; a durable-write failure or ErrNotAttached maps to
		// 500 through writeAuthError's default. The agent id is safe to log — it
		// came from the roster, not the request.
		s.writeAuthError(w, r, "leave", err, "agent_id", principal.AgentID)
		return
	}

	if res.AlreadyLeft {
		s.log.Info("agent leave: already absent (idempotent retry); nothing written",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", res.AgentID,
		)
	} else {
		s.log.Info("agent LEFT the bus; roster tombstone written and live sessions dropped",
			"request_id", RequestIDFromContext(r.Context()),
			"agent_id", res.AgentID,
			"sessions_dropped", res.SessionsDropped,
		)
	}

	s.writeJSON(w, r, http.StatusOK, LeaveResponseBody{
		AgentID:         res.AgentID,
		Left:            true,
		AlreadyLeft:     res.AlreadyLeft,
		SessionsDropped: res.SessionsDropped,
	})
}
