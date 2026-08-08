package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
//
// PanicAfterWriteField (CORE-14) is why the request record is assembled rather
// than written in one literal: when a handler has already put bytes on the
// wire, the status it sent CANNOT be retracted, so `status` stays what the
// client actually received -- but the record must not therefore read as a
// success. See the field constants below.
func LoggingMiddleware(lg *logging.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		id := SanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id))

		// rec records; rw is what the HANDLER sees. They differ because rw
		// advertises exactly the optional interfaces w itself supports -- see
		// wrapResponseWriter (CORE-13).
		rec, rw := wrapResponseWriter(w)

		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					// Documented "abort quietly" signal: let net/http see it.
					panic(p)
				}
				// Captured BEFORE the 500 below, which would otherwise set
				// wrote itself and make every panic look like one that
				// happened after the handler wrote. responseBegun() also
				// counts a hijack, where the handler has been writing to the
				// raw socket behind this recorder's back.
				rec.panicked = true
				rec.panicAfterWrite = rec.responseBegun()

				lg.Error("panic serving request",
					"request_id", id,
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(p),
					PanicAfterWriteField, rec.panicAfterWrite,
					// On the error line too: this is the first line an
					// operator reads, and "the connection had been taken over"
					// changes how every other field on it should be read.
					HijackedField, rec.hijacked,
					logging.StackKey, string(debug.Stack()),
				)
				// Never on a hijacked connection: the socket no longer speaks
				// HTTP, so net/http would only log a complaint about a write
				// it cannot make.
				if !rec.responseBegun() {
					rec.WriteHeader(http.StatusInternalServerError)
				}
			}

			fields := []interface{}{
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status(),
				"bytes", rec.bytes,
				"latency_ms", strconv.FormatFloat(float64(time.Since(start).Microseconds())/1000.0, 'f', 3, 64),
				"remote", remoteHost(r),
			}
			if rec.hijacked {
				// Says the record describes only the part of the request this
				// middleware could still see. status is 0 unless the handler
				// had already written a real one before hijacking -- see
				// HijackedField; do not read this as "status is always 0".
				fields = append(fields, HijackedField, true)
			}
			if rec.panicked {
				// CORE-14. Both markers, and both only on a request that
				// actually panicked, so an ordinary line is unchanged and a
				// log query can select on presence alone.
				//
				// panicked=true is the honest answer to "did this request
				// fail?" -- and it is NOT redundant with status, precisely
				// because status can be 200. panic_after_write says WHY the
				// status cannot be trusted: the response had already begun
				// when the handler panicked, so nothing here could retract it.
				// Measured against a real net/http server, what the client
				// gets is not visibly broken -- a correctly framed 200 with no
				// read error -- so the LOG is the only place the failure is
				// visible at all. An error-rate metric must key on panicked,
				// never on status alone.
				fields = append(fields, PanickedField, true, PanicAfterWriteField, rec.panicAfterWrite)
			}
			lg.Info("request", fields...)
		}()

		next.ServeHTTP(rw, r)
	})
}

// PanickedField and PanicAfterWriteField are the log keys CORE-14 adds. They
// are exported so a test, a log query or an alert rule names the same string
// the middleware writes rather than a copy that can drift.
//
//   - PanickedField is present, and true, on the `request` record of any
//     request whose handler panicked. It is absent otherwise.
//   - PanicAfterWriteField distinguishes the two panics that look identical in
//     the log and are not: false means recovery answered 500 and the recorded
//     status is the truth; true means the response had already begun, so what
//     the client received is a well-formed reply under an unrelated status
//     (very often 200) that HTTP does not allow us to retract. Note it reads
//     as a clean success at the client -- net/http still frames it correctly,
//     Content-Length and all -- which is exactly why the LOG has to say
//     otherwise.
//   - HijackedField is present, and true, when the handler took the raw
//     connection over. `status` is then 0 -- "not known here" -- UNLESS the
//     handler had already written a real status before hijacking, in which
//     case that status is kept because it is genuinely what went out first.
const (
	PanickedField        = "panicked"
	PanicAfterWriteField = "panic_after_write"
	HijackedField        = "hijacked"
)

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
//
// It deliberately does NOT define Flush, Hijack or ReadFrom itself. Those live
// on the small wrapper types below so the capability set a handler sees is the
// capability set the underlying writer actually has; see wrapResponseWriter.
type responseRecorder struct {
	http.ResponseWriter
	code  int
	bytes int
	wrote bool

	// hijacked records that the handler took the raw connection over. From
	// that moment this recorder observes NOTHING: the handler writes its own
	// status line and body straight to the socket. See hijack().
	hijacked bool

	// panicked / panicAfterWrite are set by LoggingMiddleware's recover, not
	// by anything on this type. See PanicAfterWriteField (CORE-14).
	panicked        bool
	panicAfterWrite bool
}

// status reports what the client received, as far as this recorder can honestly
// tell.
func (r *responseRecorder) status() int {
	if r.hijacked && !r.wrote {
		// 0 means "not known here", and it is always paired with hijacked=true
		// on the same record. The handler owns the socket and wrote the status
		// line itself; reporting 200 would be inventing a success we never saw
		// -- the same lie CORE-14 exists to stop, one layer down.
		return 0
	}
	if !r.wrote {
		// Handler returned without writing: net/http sends 200 with no body.
		return http.StatusOK
	}
	return r.code
}

// responseBegun reports whether anything the client can see has already been
// committed -- either through this recorder or, after a hijack, straight to the
// socket behind its back. It is what panic_after_write is computed from.
func (r *responseRecorder) responseBegun() bool { return r.wrote || r.hijacked }

func (r *responseRecorder) WriteHeader(code int) {
	if r.responseBegun() {
		// Duplicate WriteHeader: net/http logs and ignores it, and so do we --
		// the first status is the one on the wire.
		//
		// The hijacked half of responseBegun() is the same rule for the same
		// reason (found by the security gate, 2026-08-08). After a hijack the
		// ResponseWriter is dead -- net/http answers ErrHijacked -- so a stray
		// Write on it would otherwise route through here, flip `wrote`, and
		// replace the honest status=0 with a fabricated 200 while the client
		// had been sent a 418 on the raw socket. A status this middleware
		// never saw must stay unknown; that is the whole of CORE-14.
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

// flush, hijack and readFrom are the capability implementations. Each one
// assumes the inner writer supports the interface in question; that is
// guaranteed by wrapResponseWriter, which is the ONLY thing that may expose
// them, and only on a writer whose inner half type-asserted successfully.
func (r *responseRecorder) flush() {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	r.ResponseWriter.(http.Flusher).Flush()
}

// hijack forwards to the inner writer and RECORDS that it succeeded.
//
// Recording it is not bookkeeping, it is the correctness of CORE-14's audit
// marker. A hijacking handler writes its status line and body directly to the
// socket, so `wrote` stays false and this recorder sees nothing. Without the
// flag, a handler that hijacked, wrote a response and then panicked logged
// `panicked=true panic_after_write=false status=500` -- claiming recovery had
// answered cleanly, while the client had already been sent something else
// entirely. That is a FALSE NEGATIVE in the very control CORE-14 added, and it
// is reachable precisely because CORE-13 made Hijack work through this
// wrapper. Demonstrated live by the security gate, 2026-08-08.
//
// A FAILED hijack changes nothing: the connection was not taken over, so the
// recorder's view of the response is still complete.
func (r *responseRecorder) hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := r.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		r.hijacked = true
	}
	return c, rw, err
}

// readFrom forwards to the inner writer's ReadFrom, which is what preserves
// net/http's sendfile fast path for large/file responses. A wrapper that only
// implements Write silently costs that; a wrapper that implements ReadFrom by
// falling back to io.Copy costs it just as silently.
func (r *responseRecorder) readFrom(src io.Reader) (int64, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
	r.bytes += int(n)
	return n, err
}

// The capability wrappers (CORE-13).
//
// THE BUG THESE FIX: responseRecorder used to declare Flush() and Hijack()
// unconditionally, so `w.(http.Flusher)` and `w.(http.Hijacker)` ALWAYS
// succeeded at the handler -- even when the writer underneath supported
// neither. Feature detection is the normal, correct pattern for optional
// interfaces, and an unconditional method turns it into a lie: the handler
// takes the streaming path, or the upgrade path, and only finds out at call
// time. Hijack returned a plain error, which a handler that had already
// decided to hijack has no fallback for; Flush silently did nothing, which is
// worse, because a long-poll that believes it flushed reports no error at all.
//
// Go has no way to build a method set at run time, so the eight combinations
// are spelled out. Ugly, but exhaustive and checked by the compiler -- and the
// alternative (a reflect-built proxy) is the kind of clever this project's
// invariant 8 exists to refuse.
//
// SCOPE, stated so the next reader does not assume otherwise: exactly three
// optional interfaces are forwarded -- http.Flusher, http.Hijacker and
// io.ReaderFrom. http.CloseNotifier and http.Pusher are NOT, deliberately:
// CloseNotifier is deprecated in favour of Request.Context, and Pusher is
// HTTP/2 server push, which this bus does not serve and browsers no longer
// implement. Neither was advertised before this change either, so nothing
// regresses; adding one means adding eight more cases below, which is the
// honest cost of a capability and should be paid only for a capability
// something actually uses.
type (
	recFlush           struct{ *responseRecorder }
	recHijack          struct{ *responseRecorder }
	recReadFrom        struct{ *responseRecorder }
	recFlushHijack     struct{ *responseRecorder }
	recFlushRead       struct{ *responseRecorder }
	recHijackRead      struct{ *responseRecorder }
	recFlushHijackRead struct{ *responseRecorder }
)

func (r recFlush) Flush()           { r.flush() }
func (r recFlushHijack) Flush()     { r.flush() }
func (r recFlushRead) Flush()       { r.flush() }
func (r recFlushHijackRead) Flush() { r.flush() }

func (r recHijack) Hijack() (net.Conn, *bufio.ReadWriter, error)          { return r.hijack() }
func (r recFlushHijack) Hijack() (net.Conn, *bufio.ReadWriter, error)     { return r.hijack() }
func (r recHijackRead) Hijack() (net.Conn, *bufio.ReadWriter, error)      { return r.hijack() }
func (r recFlushHijackRead) Hijack() (net.Conn, *bufio.ReadWriter, error) { return r.hijack() }

func (r recReadFrom) ReadFrom(src io.Reader) (int64, error)        { return r.readFrom(src) }
func (r recFlushRead) ReadFrom(src io.Reader) (int64, error)       { return r.readFrom(src) }
func (r recHijackRead) ReadFrom(src io.Reader) (int64, error)      { return r.readFrom(src) }
func (r recFlushHijackRead) ReadFrom(src io.Reader) (int64, error) { return r.readFrom(src) }

// Compile-time proof that each wrapper is still an http.ResponseWriter. The
// embedded *responseRecorder promotes Header/Write/WriteHeader; if a future
// edit shadowed one of them wrongly, this fails at build time rather than at
// the first request.
var (
	_ http.ResponseWriter = recFlush{}
	_ http.ResponseWriter = recHijack{}
	_ http.ResponseWriter = recReadFrom{}
	_ http.ResponseWriter = recFlushHijack{}
	_ http.ResponseWriter = recFlushRead{}
	_ http.ResponseWriter = recHijackRead{}
	_ http.ResponseWriter = recFlushHijackRead{}

	// ...and that each ADVERTISES what its name claims. A method whose
	// signature drifted would otherwise merely stop satisfying the interface,
	// silently, and only the runtime table in
	// TestResponseWriterInterfaces would notice.
	_ http.Flusher  = recFlush{}
	_ http.Hijacker = recHijack{}
	_ io.ReaderFrom = recReadFrom{}

	_ interface {
		http.Flusher
		http.Hijacker
	} = recFlushHijack{}
	_ interface {
		http.Flusher
		io.ReaderFrom
	} = recFlushRead{}
	_ interface {
		http.Hijacker
		io.ReaderFrom
	} = recHijackRead{}
	_ interface {
		http.Flusher
		http.Hijacker
		io.ReaderFrom
	} = recFlushHijackRead{}
)

// wrapResponseWriter returns the recorder that observes the response and the
// writer to hand the HANDLER. The second value advertises http.Flusher,
// http.Hijacker and io.ReaderFrom if and only if w itself does, so a handler's
// feature detection reaches the same answer through the wrapper as it would
// have reached against w directly.
func wrapResponseWriter(w http.ResponseWriter) (*responseRecorder, http.ResponseWriter) {
	rec := &responseRecorder{ResponseWriter: w}
	_, canFlush := w.(http.Flusher)
	_, canHijack := w.(http.Hijacker)
	_, canReadFrom := w.(io.ReaderFrom)

	switch {
	case canFlush && canHijack && canReadFrom:
		return rec, recFlushHijackRead{rec}
	case canFlush && canHijack:
		return rec, recFlushHijack{rec}
	case canFlush && canReadFrom:
		return rec, recFlushRead{rec}
	case canHijack && canReadFrom:
		return rec, recHijackRead{rec}
	case canFlush:
		return rec, recFlush{rec}
	case canHijack:
		return rec, recHijack{rec}
	case canReadFrom:
		return rec, recReadFrom{rec}
	default:
		return rec, rec
	}
}
