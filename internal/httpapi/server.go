package httpapi

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// Identity supplies the bus's own id to the HTTP layer.
//
// It is an interface on purpose: invariant 1 makes the SERVER authoritative on
// every id, so the real implementation is ids.BusIdentity (internal/ids),
// which mints and persists the bus id. StaticIdentity below is retained for
// tests only; httpapi never has to know which of the two it is talking to.
type Identity interface {
	// BusID returns the id of this bus. It is stable for the process lifetime.
	BusID() string
}

// DurableLog is the HTTP layer's view of the two-phase durable write path
// (internal/wal). *wal.Log satisfies it.
//
// It is one method on purpose, in the same spirit as Identity above: Write is
// the whole of invariant 4 as a handler needs it -- hand over an entry, get
// back a Committed only once the change is prepared, committed and fsynced --
// and a handler has no business calling Begin, Close or Recovered, which belong
// to the process lifecycle that main owns. Narrowing it here also keeps the
// tests of this package free of a real log on disk.
type DurableLog interface {
	// Write durably records e and does not return until it is committed and
	// fsynced. Nothing may be acknowledged to a client before it returns.
	Write(wal.Entry) (wal.Committed, error)
}

// DefaultBusID is the placeholder bus id used when no identity is supplied.
const DefaultBusID = "bus-local"

// StaticIdentity is a fixed-id Identity, retained for tests only. Production
// code uses ids.BusIdentity (internal/ids), which mints and persists the real
// bus id; StaticIdentity is not id minting.
type StaticIdentity string

// BusID implements Identity.
func (s StaticIdentity) BusID() string { return string(s) }

// Options configures New. The zero value yields a usable server with a
// placeholder identity, a discarded log and an unknown version.
type Options struct {
	// Identity supplies the bus id served by /v1/info. Defaults to
	// StaticIdentity(DefaultBusID).
	Identity Identity

	// Logger receives the per-request records. Defaults to a discarding logger.
	Logger *logging.Logger

	// Version is the build version reported by /v1/info.
	Version string

	// StartedAt is the instant uptime is measured from. Defaults to time.Now
	// at construction.
	StartedAt time.Time

	// PollTimeout is the long-poll ceiling. Carried here so the POLL epic's
	// handlers can read it off the Server; nothing consumes it yet.
	PollTimeout time.Duration

	// Durable is the two-phase durable write path, opened and replayed by main
	// before the listener binds and held for the process lifetime. It is
	// carried here so the epics that add writing handlers have exactly one
	// write path to reach for; NO handler and NO route uses it in this task,
	// and neither /healthz nor /v1/info is affected by it.
	//
	// It may be nil -- the zero Options and every test that does not care about
	// durability leave it so -- and nothing here may panic on that. There is no
	// default: a no-op stand-in would be a write path that silently loses data,
	// which is worse than a nil the caller has to check.
	Durable DurableLog

	// Hub owns message fan-out, the applied-key table and the long-poll waiter
	// registry (internal/hub). When it is non-nil the messaging routes —
	// /v1/agents, /v1/broadcast, /v1/mint, /v1/send, /v1/messages and /v1/wait —
	// are registered. Every one of them AUTHENTICATES: none is on
	// authMiddleware's allow-list, so they are protected by being registered.
	//
	// It may be nil, in which case those routes are NOT REGISTERED AT ALL and
	// 404 exactly like any other path this build does not serve — the same
	// choice, for the same reason, as Auth above: a route that exists and
	// refuses is a claim the surface is there.
	//
	// THE CALLER BUILDS IT. This package used to construct one for itself when
	// Options.Hub was nil and Options.Durable was a replayable log (openHub,
	// deleted by AUTH-7), a transitional arrangement whose two costs it named
	// honestly: the durable log was replayed TWICE at startup, and a rebuild
	// FAILURE could not be fatal because New returns no error. The second is
	// what killed it. A hub now requires a roster (hub.Options.Roster) and a hub
	// without one must FAIL rather than serve nobody — and a constructor that
	// cannot return an error can only downgrade that failure to a log line and a
	// bus that starts with its messaging silently missing.
	Hub *hub.Hub

	// Auth is the enrolment and session authority (internal/auth). When it is
	// non-nil the three credential-issuing routes -- /v1/enroll,
	// /v1/session/begin and /v1/session/complete -- are registered.
	//
	// It may be nil -- the zero Options and every test that does not care about
	// enrolment leave it so -- and nothing here may panic on that. When it is
	// nil those routes are NOT REGISTERED AT ALL, so they 404 exactly like any
	// other path this build does not serve. That is deliberately preferred over
	// registering them and answering 503: a route that exists and refuses is a
	// claim that the surface is there, and a server built without an auth
	// service does not have it.
	//
	// A nil Auth also means nobody can be authenticated, and authMiddleware is
	// DEFAULT-DENY, so every path that is NOT on UnauthenticatedRoutes answers
	// 401. The three credential routes stay on the allow-list even then, so
	// they still reach the mux and still 404 -- the documented behaviour above
	// is preserved exactly, and is pinned by
	// TestEnrollRoutesAreAbsentWithoutAnAuthService.
	//
	// There is no default: an auth service needs an id minter, and inventing
	// one here would mint agent ids from a counter with nothing on disk behind
	// it (invariant 1 -- see auth.Options.Minter).
	Auth *auth.Service

	// Now is the clock, overridable so tests can assert on uptime.
	// Defaults to time.Now.
	Now func() time.Time
}

// Server is the HTTP surface of the bus. It implements http.Handler, so main
// only has to wire config -> Server -> net/http listener.
type Server struct {
	identity    Identity
	log         *logging.Logger
	version     string
	startedAt   time.Time
	pollTimeout time.Duration
	durable     DurableLog
	hub         *hub.Hub
	auth        *auth.Service
	now         func() time.Time
	handler     http.Handler

	// discovery is the STATIC protocol-discovery document served by
	// GET /v1/discovery, built once in New and never mutated afterwards.
	// Holding it here rather than assembling it per request is what makes
	// "the discovery response cannot grow with bus state" structurally true:
	// its only input is the bus id, which is stable for the process lifetime.
	// See discovery.go.
	discovery DiscoveryResponse

	// discoveryJSON is discovery already marshalled, so serving it costs a
	// write rather than a ~6 KiB marshal. That matters because /v1/discovery
	// is UNAUTHENTICATED and unrate-limited: a tiny anonymous request must not
	// be able to buy meaningful server CPU. Marshalled once in New, read-only
	// afterwards, and never handed out except as bytes written to a response.
	discoveryJSON []byte

	// routes is every pattern registered on the mux, in registration order.
	// Go 1.19's http.ServeMux cannot be enumerated, and an authentication
	// wrapper that is only claimed to cover the whole surface is worth
	// nothing -- this is how a test walks the real surface. Written once in
	// New, read-only afterwards; see (*Server).route.
	routes []string
}

// New builds the mux, wraps it in the shared middleware and returns the
// resulting handler.
func New(opts Options) *Server {
	s := &Server{
		identity:    opts.Identity,
		log:         opts.Logger,
		version:     opts.Version,
		startedAt:   opts.StartedAt,
		pollTimeout: opts.PollTimeout,
		durable:     opts.Durable,
		hub:         opts.Hub,
		auth:        opts.Auth,
		now:         opts.Now,
	}
	if s.identity == nil {
		s.identity = StaticIdentity(DefaultBusID)
	}
	if s.log == nil {
		s.log = logging.New(io.Discard, logging.LevelError)
	}
	if s.version == "" {
		s.version = "unknown"
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.startedAt.IsZero() {
		s.startedAt = s.now()
	}

	// Built AFTER the identity default is applied and BEFORE any request can
	// be served, so the handler only ever writes a finished value.
	s.discovery = newDiscoveryResponse(s.identity.BusID())

	// Marshalling CANNOT fail here: every field is a string, int, bool or a
	// slice/struct of those, with no channel, func, cycle or custom Marshaler
	// anywhere in the shape. If it somehow did, s.discoveryJSON stays nil and
	// handleDiscovery falls back to marshalling per request, which is slower
	// but still correct -- a discovery document is not worth failing New over.
	if b, err := json.Marshal(s.discovery); err == nil {
		s.discoveryJSON = append(b, '\n')
	}

	mux := http.NewServeMux()
	s.route(mux, "/healthz", s.handleHealthz)
	s.route(mux, "/v1/info", s.handleInfo)
	// Unauthenticated by necessity: it is how a caller holding only a URL
	// learns to enrol. It is static and carries no bus state; see discovery.go.
	s.route(mux, RouteDiscovery, s.handleDiscovery)

	// Registered only when there is an auth service to serve them; see
	// Options.Auth. These three ISSUE the credential every other route
	// requires, which is why they are on authMiddleware's allow-list -- see
	// unauthenticatedRoutes for why session/complete belongs there too.
	if s.auth != nil {
		s.route(mux, RouteEnroll, s.handleEnroll)
		s.route(mux, RouteSessionBegin, s.handleSessionBegin)
		s.route(mux, RouteSessionComplete, s.handleSessionComplete)
	}

	// The messaging surface. Registered only when there is a hub to serve it;
	// see Options.Hub. NONE of these is on unauthenticatedRoutes, so every one
	// of them requires a bearer token by default-deny — that is the whole point
	// of registering them through s.route behind authMiddleware rather than
	// protecting them one at a time.
	if s.hub != nil {
		s.route(mux, RouteAgents, s.handleAgents)
		s.route(mux, RouteBroadcast, s.handleBroadcast)
		// /v1/mint is the FIRST half of a send (SIGN-1's reserve-then-send), so
		// it is registered here with the rest of the messaging surface and
		// authenticates exactly like it: a reservation is minted for the
		// authenticated principal and is worthless to anybody else.
		s.route(mux, RouteMint, s.handleMint)
		s.route(mux, RouteSend, s.handleSend)
		s.route(mux, RouteMessages, s.handleMessages)
		s.route(mux, RouteWait, s.handleWait)
	}

	// The catch-all (CORE-8). Registered LAST for readability only --
	// http.ServeMux resolves by longest matching pattern, not by registration
	// order, so "/" wins only where nothing more specific matched.
	//
	// It is registered through s.route, and that is the SECURITY-LOAD-BEARING
	// part of this line. A catch-all hung on the raw mux, or wrapped outside
	// authMiddleware, would itself be an unauthenticated route and would turn
	// the whole server into a route oracle: an anonymous caller could probe any
	// path and read 404-vs-401 to learn exactly which surfaces this build
	// serves. Inside the wrapper, default-deny still answers FIRST, so:
	//
	//   anonymous  + any path, known or not -> 401, indistinguishable.
	//   authorised + an unknown path        -> 404, and now in the JSON error
	//                                          envelope every other route uses.
	//
	// So this changes NOTHING an anonymous caller can observe. What it fixes is
	// the authenticated case, where the answer used to be net/http's built-in
	// "404 page not found" as text/plain -- a client (or a wrapper piping
	// through a JSON parser) that trusts the documented contract got a parse
	// error instead of a structured one, i.e. the response was least usable
	// exactly when something was already wrong.
	s.route(mux, RouteCatchAll, s.handleNotFound)

	// The order is LOAD-BEARING. authMiddleware wraps the WHOLE mux, so
	// invariant 3 is enforced by default-deny rather than route by route: a
	// route added later is authenticated because it is registered, not because
	// someone remembered to protect it. LoggingMiddleware stays OUTSIDE it for
	// two reasons -- a 401 is a request an operator most wants in the log, and
	// it is what puts the request id in the context, so authMiddleware's own
	// refusal lines can be correlated with the response.
	s.handler = LoggingMiddleware(s.log, s.authMiddleware(mux))
	return s
}

// route registers h at pattern and records the pattern for Routes.
//
// EVERY route MUST be registered through this, not through mux.HandleFunc, so
// the enumeration test can walk the real surface -- Go 1.19's http.ServeMux
// offers no way to list its patterns.
//
// Forgetting is a testing gap, NOT a security hole: authMiddleware wraps the
// whole mux and is default-deny, so an unrecorded route is still
// authenticated. It is merely invisible to the test that proves it.
func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, pattern)
	mux.HandleFunc(pattern, h)
}

// Routes returns every pattern this server registered, sorted. It returns a
// copy so a caller cannot mutate the server's view of its own surface.
func (s *Server) Routes() []string {
	out := make([]string, len(s.routes))
	copy(out, s.routes)
	sort.Strings(out)
	return out
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// BusID returns the id of this bus, as reported by /v1/info.
func (s *Server) BusID() string { return s.identity.BusID() }

// PollTimeout returns the configured long-poll ceiling, for the POLL epic.
func (s *Server) PollTimeout() time.Duration { return s.pollTimeout }

// Durable returns the durable write path the server was built with, for the
// epics that add writing handlers. It is nil when none was supplied, so a
// caller must check before writing; no route consumes it yet.
func (s *Server) Durable() DurableLog { return s.durable }

// Auth returns the enrolment and session authority the server was built with,
// or nil when none was supplied -- in which case the credential-issuing routes
// are not registered -- and, since authMiddleware is default-deny, every path
// off the allow-list answers 401.
func (s *Server) Auth() *auth.Service { return s.auth }

// Hub returns the messaging hub this server serves from, or nil when the caller
// supplied none — in which case the messaging routes are not registered. This
// package never builds one; see Options.Hub.
func (s *Server) Hub() *hub.Hub { return s.hub }

// HealthResponse is the body of GET /healthz.
type HealthResponse struct {
	Status string `json:"status"`
}

// InfoResponse is the body of GET /v1/info.
//
// This endpoint is UNAUTHENTICATED (pre-enrolment discovery needs it), so the
// payload is deliberately minimal: bus id, version, uptime. Do not add data
// dirs, listen addresses, peer lists, agent rosters or anything else an
// unauthenticated caller has no business learning.
//
// Discovery is the one exception, and it is safe precisely because it adds no
// information: it is the COMPILE-TIME CONSTANT RouteDiscovery, identical in
// every build and independent of this bus's identity, state and configuration.
// It exists so a caller that knows only /v1/info can find the protocol
// document (GET /v1/discovery) instead of having to guess the path. This
// endpoint stays a liveness and version probe; the protocol guide lives at
// that other path so each can be pinned on its own. See discovery.go.
type InfoResponse struct {
	BusID         string  `json:"bus_id"`
	Version       string  `json:"version"`
	UptimeSeconds float64 `json:"uptime_seconds"`
	Discovery     string  `json:"discovery"`
}

// handleHealthz serves GET /healthz: liveness only, no auth, no state.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	s.writeJSON(w, r, http.StatusOK, HealthResponse{Status: "ok"})
}

// handleInfo serves GET /v1/info: bus id, version and uptime, no auth.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	uptime := s.now().Sub(s.startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	s.writeJSON(w, r, http.StatusOK, InfoResponse{
		BusID:         s.identity.BusID(),
		Version:       s.version,
		UptimeSeconds: math.Round(uptime*1000) / 1000,
		Discovery:     RouteDiscovery,
	})
}

// RouteCatchAll is the pattern the JSON 404 handler is registered at. In
// http.ServeMux "/" is a subtree pattern that matches every request no more
// specific pattern claims, which is exactly "no such route here".
const RouteCatchAll = "/"

// handleNotFound answers any path this build does not serve, in the same JSON
// error envelope as every other failure (CORE-8). Its registration in New
// explains why it must be registered through (*Server).route; two properties
// belong here, at the handler:
//
//   - EVERY METHOD gets 404, never 405. 405 means "this resource exists but
//     not via that method" -- false here, and a disclosure: it would let a
//     caller separate "path exists, wrong method" from "path does not exist".
//   - The body is the fixed string "not found" and never echoes r.URL.Path,
//     which is attacker-controlled and, on this route by definition, has had
//     no other validation applied to it.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusNotFound, ErrorResponse{Error: "not found"})
}

// ErrorResponse is the body of any non-2xx JSON reply.
type ErrorResponse struct {
	Error string `json:"error"`
}

// AllowGET is the Allow header a GET route sends with its 405. It names HEAD
// as well because requireGET accepts HEAD -- an Allow header that omitted a
// method the route serves would be a second, quieter version of the same
// inconsistency CORE-7 fixed.
const AllowGET = "GET, HEAD"

// requireGET answers 405 with an Allow header for anything but GET or HEAD,
// and reports whether the handler should continue.
//
// HEAD IS ACCEPTED (CORE-7, decided 2026-08-08); it previously was not, while
// writeJSON carried a body-suppression guard for HEAD that could therefore
// never run. Two things about the decision that are not obvious from the code:
//
//   - It is SAFE because every requireGET route is a pure READ. The message
//     cursor is the client-supplied `after`/`cursor` parameter, so a HEAD
//     consumes and advances nothing a later GET needed. A HEAD on a route with
//     side effects would be a different question; there is no such route.
//   - Authentication is UNAFFECTED. HEAD reaches these handlers through the
//     same default-deny authMiddleware as GET, so an anonymous HEAD to a
//     protected route is 401 like any other method.
//
// The rest of the rationale (probes issue HEAD; RFC 9110 makes it a GET
// without a body) is in CONTRACTS-HTTP.md.
func (s *Server) requireGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", AllowGET)
	s.writeJSON(w, r, http.StatusMethodNotAllowed, ErrorResponse{Error: "method not allowed"})
	return false
}

// writeJSON marshals v before touching the response so that a marshalling
// failure still produces a clean 500 instead of a truncated 200 body.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Error("marshalling response failed",
			"request_id", RequestIDFromContext(r.Context()),
			"path", r.URL.Path,
			"err", err,
		)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusInternalServerError)
		// Same HEAD suppression as the success path below. net/http would
		// discard the body anyway, but writing it still miscounts the response
		// size in the request log -- and a failure path that handles HEAD
		// differently from the success path is how the two drift.
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `{"error":"internal error"}`+"\n")
		}
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		if _, err := w.Write(body); err != nil {
			s.log.Debug("writing response failed",
				"request_id", RequestIDFromContext(r.Context()),
				"path", r.URL.Path,
				"err", err,
			)
		}
	}
}

// writePreformattedJSON writes an ALREADY-MARSHALLED body, for responses that
// are computed once at construction rather than per request (see
// Server.discoveryJSON). It sets exactly the headers writeJSON sets and
// handles HEAD the same way -- deliberately, so the two paths cannot drift.
//
// body must include its trailing newline, and must NEVER be derived from
// request input: the whole point is that it is a fixed, server-owned value.
// Callers must not retain or mutate it afterwards.
func (s *Server) writePreformattedJSON(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		if _, err := w.Write(body); err != nil {
			s.log.Debug("writing response failed",
				"request_id", RequestIDFromContext(r.Context()),
				"path", r.URL.Path,
				"err", err,
			)
		}
	}
}
