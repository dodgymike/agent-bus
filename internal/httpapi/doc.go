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
// Currently implemented: GET /healthz and GET /v1/info, both unauthenticated.
// Enrolment, send, poll and relay routes arrive with their own epics.
package httpapi
