package httpapi

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"

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
	now         func() time.Time
	handler     http.Handler
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/info", s.handleInfo)

	s.handler = LoggingMiddleware(s.log, mux)
	return s
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
