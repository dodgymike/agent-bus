package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// RequestIDHeader is the header carrying a caller-supplied correlation id. It
// is echoed on the response and included in every log record for the request.
const RequestIDHeader = "X-Request-Id"

// MaxRequestIDLen bounds an inbound request id. Anything longer is rejected
// and replaced by a server-generated id.
const MaxRequestIDLen = 64

// requestIDCounter is the fallback source of uniqueness if crypto/rand fails.
var requestIDCounter uint64

type ctxKey int

const ctxKeyRequestID ctxKey = 0

// RequestIDFromContext returns the request id assigned by LoggingMiddleware,
// or "" if the request did not pass through it.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// SanitizeRequestID validates a caller-supplied request id. An inbound id is
// untrusted input: it is reflected into the response and, more importantly,
// into the log stream, so anything that is not a short run of [A-Za-z0-9._-]
// is rejected outright rather than escaped-and-kept. Returns "" when the id is
// unusable, in which case the caller mints a fresh one.
func SanitizeRequestID(v string) string {
	if v == "" || len(v) > MaxRequestIDLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '-':
		default:
			return ""
		}
	}
	return v
}

// NewRequestID mints a server-side request id: 16 hex characters from
// crypto/rand, with a monotonic counter as the fallback if the entropy source
// fails so that ids stay unique even then.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := atomic.AddUint64(&requestIDCounter, 1)
		return "seq-" + strconv.FormatUint(n, 16)
	}
	return hex.EncodeToString(b[:])
}

// LoggingMiddleware wraps next with per-request structured logging. Every
// request gets a request id (inbound one if it survives SanitizeRequestID,
// otherwise a fresh one), and one record is emitted on completion carrying
// method, path, status, response size, latency and the request id.
//
// It also recovers panics: a panicking handler is logged at error level with
// its stack and answered with 500 rather than dropping the connection with no
// trace. http.ErrAbortHandler keeps its documented meaning and is re-panicked.
func LoggingMiddleware(lg *logging.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		id := SanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id))

		rec := &responseRecorder{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					// Documented "abort quietly" signal: let net/http see it.
					panic(p)
				}
				lg.Error("panic serving request",
					"request_id", id,
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(p),
					"stack", string(debug.Stack()),
				)
				if !rec.wrote {
					rec.WriteHeader(http.StatusInternalServerError)
				}
			}
			lg.Info("request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status(),
				"bytes", rec.bytes,
				"latency_ms", strconv.FormatFloat(float64(time.Since(start).Microseconds())/1000.0, 'f', 3, 64),
				"remote", remoteHost(r),
			)
		}()

		next.ServeHTTP(rec, r)
	})
}

// remoteHost returns the peer address without its port. Proxy headers are
// deliberately ignored: they are trivially forged and this server does not
// know whether it sits behind a trusted proxy.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// responseRecorder wraps an http.ResponseWriter to capture the status code and
// the number of bytes written. It is used by a single goroutine per request
// (the handler's), so it needs no locking.
type responseRecorder struct {
	http.ResponseWriter
	code  int
	bytes int
	wrote bool
}

func (r *responseRecorder) status() int {
	if !r.wrote {
		// Handler returned without writing: net/http sends 200 with no body.
		return http.StatusOK
	}
	return r.code
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wrote {
		// Duplicate WriteHeader: net/http logs and ignores it, and so do we --
		// the first status is the one on the wire.
		return
	}
	r.wrote = true
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush keeps streaming and long-poll handlers working through the wrapper.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		if !r.wrote {
			r.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Hijack keeps connection-upgrade paths available through the wrapper.
func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpapi: ResponseWriter does not support Hijack")
	}
	return h.Hijack()
}
