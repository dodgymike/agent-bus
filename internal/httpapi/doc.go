// Package httpapi owns the HTTP surface of the bus: the mux, the shared
// middleware and the handlers.
//
// It is deliberately named httpapi rather than http so that files importing
// both it and stdlib net/http do not have to rename either.
//
// Handlers must treat the request context as the server-lifetime context:
// main wires it through http.Server.BaseContext, so a handler parked on a
// long-poll can select on r.Context().Done() and be released at shutdown
// instead of stalling the drain.
//
// Authentication is DEFAULT-DENY (see authmw.go). authMiddleware wraps the
// whole mux, so a route is authenticated by virtue of being registered, not
// because someone remembered to protect it. Register every route through
// (*Server).route, and if a new route genuinely must be anonymous, add it to
// unauthenticatedRoutes AND to the golden list in TestEveryRouteRequiresAuth --
// two deliberate, reviewable edits, which is the point.
//
// Currently implemented: GET /healthz and GET /v1/info (unauthenticated), plus
// POST /v1/enroll, /v1/session/begin and /v1/session/complete, which are
// unauthenticated by necessity -- they are how a credential is obtained. Send,
// poll and relay routes arrive with their own epics, authenticated by default.
package httpapi
