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
	// /v1/agents, /v1/broadcast, /v1/send, /v1/messages and /v1/wait — are
	// registered. Every one of them AUTHENTICATES: none is on
	// authMiddleware's allow-list, so they are protected by being registered.
	//
	// It may be nil, in which case those routes are NOT REGISTERED AT ALL and
	// 404 exactly like any other path this build does not serve — the same
	// choice, for the same reason, as Auth above: a route that exists and
	// refuses is a claim the surface is there.
	//
	// LEAVING IT NIL IS NOT THE SAME AS DISABLING MESSAGING, because New will
	// build one for itself when it can. See openHub for exactly when, and for
	// why that is a transitional arrangement rather than the design.
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

	if s.hub == nil {
		s.hub = s.openHub()
	}

	mux := http.NewServeMux()
	s.route(mux, "/healthz", s.handleHealthz)
	s.route(mux, "/v1/info", s.handleInfo)

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
		s.route(mux, RouteSend, s.handleSend)
		s.route(mux, RouteMessages, s.handleMessages)
		s.route(mux, RouteWait, s.handleWait)
	}

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

// recoverableLog is the RICHER view of a DurableLog that a hub needs in order
// to rebuild its serving copy: where the log lives, and what the replay at
// startup found. *wal.Log satisfies it.
//
// It is an OPTIONAL interface, discovered by assertion, rather than extra
// methods on DurableLog. DurableLog is deliberately one method — Write is the
// whole of invariant 4 as a handler needs it — and widening it would force
// every test fake and every future write path to implement two methods that
// only startup uses.
type recoverableLog interface {
	// Path is the durable log file, opened READ-ONLY for the rebuild pass.
	Path() string

	// Recovered is what the replay at wal.Open found: in particular NextIndex,
	// the high-water mark the sequence floor is derived from.
	Recovered() wal.Recovered
}

// openHub builds the hub when this server has everything one needs, and returns
// nil when it does not.
//
// # Why this is here and not in main
//
// The RIGHT wiring is for main to construct the hub, pass it as the WAL's
// Applier so the durable log is replayed exactly once, and hand it in as
// Options.Hub — a failure there is a startup error and the bus does not serve.
// That is a change to cmd/agent-bus, which this epic does not own, so the
// messaging surface would otherwise be code that no running server exposes.
//
// So this is a TRANSITIONAL arrangement with two honest costs, both worth
// naming rather than discovering later:
//
//   - The durable log is replayed TWICE at startup: once by wal.Open (with a
//     nil applier, an fsck) and once here (read-only, to rebuild the store).
//     Correct, since nothing writes in between, but wasteful.
//   - A rebuild FAILURE cannot be fatal, because New returns no error. It is
//     logged at ERROR and messaging is left unregistered, which is the least
//     bad of the options available from here: serving a store that disk does
//     not justify would break invariant 5, and panicking in a constructor is
//     not a contract this package has.
//
// Options.Hub takes precedence, so the moment main passes one this function is
// not called at all — which is exactly how it should be retired.
func (s *Server) openHub() *hub.Hub {
	if s.durable == nil {
		// No durable write path, so nothing may be acknowledged (invariant 4)
		// and there is no messaging to offer. Silent: this is the ordinary
		// shape of every test server in this package.
		return nil
	}
	rl, ok := s.durable.(recoverableLog)
	if !ok {
		s.log.Warn("messaging is NOT served: the durable write path cannot be replayed, so a message store rebuilt from it would not be justified by disk (invariant 5)",
			"bus_id", s.identity.BusID(),
		)
		return nil
	}

	path := rl.Path()
	rec := rl.Recovered()
	h, err := hub.Open(hub.Options{
		BusID:   s.identity.BusID(),
		Durable: s.durable,
		Replay: func(fn func(wal.Committed) error) (wal.Recovered, error) {
			return wal.Replay(path, fn)
		},
		NextIndex:   rec.NextIndex,
		Quarantined: rec.Repaired.Quarantined,
		Logger:      s.log,
		Now:         s.now,
		PollTimeout: s.pollTimeout,
	})
	if err != nil {
		s.log.Error("messaging is NOT served: rebuilding the message store from the durable log failed, and a store that disk does not justify must not be served (invariant 5)",
			"bus_id", s.identity.BusID(),
			"path", path,
			"err", err,
		)
		return nil
	}
	return h
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

// Hub returns the messaging hub this server serves from, or nil when there is
// none — in which case the messaging routes are not registered. See
// Options.Hub and openHub.
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
type InfoResponse struct {
	BusID         string  `json:"bus_id"`
	Version       string  `json:"version"`
	UptimeSeconds float64 `json:"uptime_seconds"`
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
	})
}

// ErrorResponse is the body of any non-2xx JSON reply.
type ErrorResponse struct {
	Error string `json:"error"`
}

// requireGET answers 405 with an Allow header for anything but GET, and
// reports whether the handler should continue.
func (s *Server) requireGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", http.MethodGet)
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
		_, _ = io.WriteString(w, `{"error":"internal error"}`+"\n")
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
