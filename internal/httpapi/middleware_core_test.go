package httpapi_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// The log keys below are spelled as LITERALS, not as httpapi.PanickedField /
// httpapi.PanicAfterWriteField. A test that reads the constant it is checking
// asserts only that the constant equals itself; these strings are the contract
// an operator's log query and alert rule are written against. Spelling them
// out also means this file COMPILES against the pre-fix code, which is what
// let every case here be observed RED before it went green.

// ---------------------------------------------------------------------------
// CORE-13: the wrapper must advertise exactly the capabilities the writer it
// wraps actually has.
// ---------------------------------------------------------------------------

// capWriter is a bare http.ResponseWriter with NO optional interfaces. Every
// capability set below is built by embedding it and adding methods, so a set
// is exactly what it declares.
type capWriter struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer

	flushed  int
	hijacked int
	readFrom int
}

func newCapWriter() *capWriter { return &capWriter{hdr: http.Header{}} }

func (c *capWriter) Header() http.Header  { return c.hdr }
func (c *capWriter) WriteHeader(code int) { c.code = code }
func (c *capWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.buf.Write(b)
}

// errHijackUnsupported is what the fake connection layer returns; the test only
// needs to know the call ARRIVED, not that a real socket came back.
var errHijackUnsupported = errors.New("test hijacker: no real connection")

type capFlusher struct{ *capWriter }

func (c capFlusher) Flush() { c.flushed++ }

type capHijacker struct{ *capWriter }

func (c capHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked++
	return nil, nil, errHijackUnsupported
}

// succeedingHijacker is the one hijacker here whose Hijack SUCCEEDS, so a test
// can tell "the handler took the connection over" apart from "the handler
// asked and was refused" -- the distinction the hijacked/panic_after_write
// marker keys on. It hands back a nil net.Conn because no test writes to it;
// what matters is the nil error.
type succeedingHijacker struct{ *capWriter }

func (c succeedingHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked++
	return nil, nil, nil
}

type capReaderFrom struct{ *capWriter }

func (c capReaderFrom) ReadFrom(src io.Reader) (int64, error) {
	c.readFrom++
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return io.Copy(&c.buf, src)
}

type capFlushHijack struct{ *capWriter }

func (c capFlushHijack) Flush() { c.flushed++ }
func (c capFlushHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked++
	return nil, nil, errHijackUnsupported
}

type capFlushRead struct{ *capWriter }

func (c capFlushRead) Flush() { c.flushed++ }
func (c capFlushRead) ReadFrom(src io.Reader) (int64, error) {
	c.readFrom++
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return io.Copy(&c.buf, src)
}

type capHijackRead struct{ *capWriter }

func (c capHijackRead) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked++
	return nil, nil, errHijackUnsupported
}
func (c capHijackRead) ReadFrom(src io.Reader) (int64, error) {
	c.readFrom++
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return io.Copy(&c.buf, src)
}

type capAll struct{ *capWriter }

func (c capAll) Flush() { c.flushed++ }
func (c capAll) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c.hijacked++
	return nil, nil, errHijackUnsupported
}
func (c capAll) ReadFrom(src io.Reader) (int64, error) {
	c.readFrom++
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return io.Copy(&c.buf, src)
}

// TestResponseWriterInterfaces pins CORE-13.
//
// The middleware's wrapper used to declare Flush() and Hijack()
// UNCONDITIONALLY, so `w.(http.Flusher)` and `w.(http.Hijacker)` always
// succeeded at the handler -- even when the writer underneath supported
// neither. Feature detection is the normal, correct pattern for optional
// interfaces, and an unconditional method turns it into a lie: the handler
// commits to the streaming or upgrade path and only discovers the truth at
// call time. Flush silently did nothing, which is the worse of the two,
// because a long-poll that believes it flushed reports no error at all.
//
// It also dropped io.ReaderFrom entirely, costing net/http's sendfile fast
// path for large responses.
//
// The assertion is EXACT SET EQUALITY in both directions -- advertised
// capabilities that are missing, and supported capabilities that are not
// advertised, both fail.
func TestResponseWriterInterfaces(t *testing.T) {
	cases := []struct {
		name         string
		writer       func(*capWriter) http.ResponseWriter
		wantFlush    bool
		wantHijack   bool
		wantReadFrom bool
	}{
		{"nothing", func(c *capWriter) http.ResponseWriter { return c }, false, false, false},
		{"flusher only", func(c *capWriter) http.ResponseWriter { return capFlusher{c} }, true, false, false},
		{"hijacker only", func(c *capWriter) http.ResponseWriter { return capHijacker{c} }, false, true, false},
		{"readerfrom only", func(c *capWriter) http.ResponseWriter { return capReaderFrom{c} }, false, false, true},
		{"flusher+hijacker", func(c *capWriter) http.ResponseWriter { return capFlushHijack{c} }, true, true, false},
		{"flusher+readerfrom", func(c *capWriter) http.ResponseWriter { return capFlushRead{c} }, true, false, true},
		{"hijacker+readerfrom", func(c *capWriter) http.ResponseWriter { return capHijackRead{c} }, false, true, true},
		{"all three", func(c *capWriter) http.ResponseWriter { return capAll{c} }, true, true, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			inner := newCapWriter()
			var buf bytes.Buffer
			lg := logging.New(&buf, logging.LevelError)

			var (
				sawFlusher    bool
				sawHijacker   bool
				sawReaderFrom bool
				ran           bool
			)
			mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = true
				_, sawFlusher = w.(http.Flusher)
				_, sawHijacker = w.(http.Hijacker)
				_, sawReaderFrom = w.(io.ReaderFrom)
			}))
			mw.ServeHTTP(tc.writer(inner), httptest.NewRequest(http.MethodGet, "/caps", nil))

			if !ran {
				t.Fatal("the handler never ran, so nothing below was actually asserted")
			}
			if sawFlusher != tc.wantFlush {
				t.Fatalf("handler sees http.Flusher = %v, want %v.\n"+
					"The wrapper must advertise a capability if and only if the writer it wraps has it; a handler that feature-detects is misled otherwise.",
					sawFlusher, tc.wantFlush)
			}
			if sawHijacker != tc.wantHijack {
				t.Fatalf("handler sees http.Hijacker = %v, want %v (same rule)", sawHijacker, tc.wantHijack)
			}
			if sawReaderFrom != tc.wantReadFrom {
				t.Fatalf("handler sees io.ReaderFrom = %v, want %v.\n"+
					"Dropping ReaderFrom costs net/http's sendfile fast path for large responses.",
					sawReaderFrom, tc.wantReadFrom)
			}
		})
	}

	t.Run("an advertised capability actually reaches the inner writer", func(t *testing.T) {
		// Advertising the right SET is half of it. A wrapper could advertise
		// correctly and still swallow the call, which is the precise failure
		// the old unconditional Flush had.
		inner := newCapWriter()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelError)

		var hijackErr error
		var readN int64
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			_, _, hijackErr = w.(http.Hijacker).Hijack()
			n, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("sendfile-payload"))
			if err != nil {
				t.Errorf("ReadFrom through the wrapper: %v", err)
			}
			readN = n
		}))
		mw.ServeHTTP(capAll{inner}, httptest.NewRequest(http.MethodGet, "/caps", nil))

		if inner.flushed != 1 {
			t.Fatalf("inner writer saw %d Flush calls, want 1 (the wrapper swallowed it)", inner.flushed)
		}
		if inner.hijacked != 1 {
			t.Fatalf("inner writer saw %d Hijack calls, want 1", inner.hijacked)
		}
		if !errors.Is(hijackErr, errHijackUnsupported) {
			t.Fatalf("Hijack returned %v, want the inner writer's own error; the wrapper must forward, not substitute", hijackErr)
		}
		if inner.readFrom != 1 {
			t.Fatalf("inner writer saw %d ReadFrom calls, want 1; a wrapper that falls back to io.Copy loses sendfile just as silently as one that omits ReaderFrom", inner.readFrom)
		}
		if readN != int64(len("sendfile-payload")) {
			t.Fatalf("ReadFrom copied %d bytes, want %d", readN, len("sendfile-payload"))
		}
		if inner.buf.String() != "sendfile-payload" {
			t.Fatalf("inner writer holds %q, want %q", inner.buf.String(), "sendfile-payload")
		}
	})

	t.Run("bytes written through ReadFrom are counted in the log record", func(t *testing.T) {
		// The recorder's whole job is accounting. A ReaderFrom pass-through
		// that bypassed it would silently report bytes=0 for exactly the large
		// responses the fast path exists for.
		inner := newCapWriter()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)

		const payload = "0123456789"
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader(payload)); err != nil {
				t.Errorf("ReadFrom: %v", err)
			}
		}))
		mw.ServeHTTP(capReaderFrom{inner}, httptest.NewRequest(http.MethodGet, "/caps", nil))

		ls := logLines(&buf)
		if len(ls) != 1 {
			t.Fatalf("got %d log lines, want 1: %q", len(ls), buf.String())
		}
		f := parseLogLine(t, ls[0])
		if f["bytes"] != "10" {
			t.Fatalf("bytes = %q, want 10; a ReadFrom that bypasses the recorder under-reports every large response", f["bytes"])
		}
		if f["status"] != "200" {
			t.Fatalf("status = %q, want 200; ReadFrom must imply WriteHeader(200) exactly as Write does", f["status"])
		}
	})
}

// ---------------------------------------------------------------------------
// CORE-14: a handler that writes and THEN panics must not log as a success.
// ---------------------------------------------------------------------------

// TestPanicAfterWrite pins CORE-14.
//
// Once bytes are on the wire the status cannot be retracted -- that is HTTP,
// and the RESPONSE is unfixable. The LOG is not: before this change a handler
// that wrote 200 and then panicked produced `status=200` and nothing else, so
// an operator reading the logs, or an error-rate metric built from them, saw a
// success for a request that failed. That is the same defect class as a
// control that reports green while the thing it controls is broken.
func TestPanicAfterWrite(t *testing.T) {
	// run drives one handler through the middleware and returns the parsed
	// error record (or nil) and the parsed request record.
	run := func(t *testing.T, h http.HandlerFunc) (errRec, reqRec map[string]string, status int) {
		t.Helper()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		rec := httptest.NewRecorder()
		httpapi.LoggingMiddleware(lg, h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

		ls := logLines(&buf)
		if len(ls) != 2 {
			t.Fatalf("got %d log lines, want 2 (the panic record + the request record): %q", len(ls), buf.String())
		}
		return parseLogLine(t, ls[0]), parseLogLine(t, ls[1]), rec.Code
	}

	t.Run("a handler that writes 200, flushes, then panics is marked", func(t *testing.T) {
		errRec, reqRec, status := run(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"partial":`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic("boom after write")
		})

		// The response itself: 200 with a truncated body, and that is correct.
		// HTTP gives us no way to take it back.
		if status != http.StatusOK {
			t.Fatalf("response status = %d, want 200; the bytes were already committed and cannot be retracted", status)
		}
		// ...which is exactly why the log line must say so.
		if reqRec["status"] != "200" {
			t.Fatalf("request record status = %q, want 200 (it must report what the CLIENT received)", reqRec["status"])
		}
		if reqRec["panicked"] != "true" {
			t.Fatalf("request record has panicked=%q, want \"true\".\n"+
				"THE CORE-14 DEFECT: this record reads status=200 with no other marker, so every log reader and every error-rate metric counts a failed request as a success. Fields present: %v",
				reqRec["panicked"], reqRec)
		}
		if reqRec["panic_after_write"] != "true" {
			t.Fatalf("request record has panic_after_write=%q, want \"true\".\n"+
				"This is the field that says WHY status cannot be trusted: the response had already begun. Fields present: %v",
				reqRec["panic_after_write"], reqRec)
		}

		// The panic must still be logged with its stack (CORE-6's companion).
		if errRec["level"] != "error" || errRec["msg"] != "panic serving request" {
			t.Fatalf("first record = %v, want the error-level panic record", errRec)
		}
		if errRec["panic"] != "boom after write" {
			t.Fatalf("panic field = %q, want %q", errRec["panic"], "boom after write")
		}
		if errRec["stack"] == "" {
			t.Fatal("the panic record carries no stack")
		}
		if errRec["panic_after_write"] != "true" {
			t.Fatalf("the panic record has panic_after_write=%q, want \"true\"; the error line an operator reads first must carry it too", errRec["panic_after_write"])
		}
	})

	t.Run("a handler that writes only the header then panics is still marked", func(t *testing.T) {
		// The subtler shape: no body at all, but WriteHeader has been called,
		// so the status line is already on the wire and recovery's 500 is
		// ignored by net/http.
		_, reqRec, status := run(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			panic("boom after header")
		})
		if status != http.StatusCreated {
			t.Fatalf("response status = %d, want 201 (already sent)", status)
		}
		if reqRec["status"] != "201" {
			t.Fatalf("request record status = %q, want 201", reqRec["status"])
		}
		if reqRec["panicked"] != "true" || reqRec["panic_after_write"] != "true" {
			t.Fatalf("panicked=%q panic_after_write=%q, want both \"true\": %v",
				reqRec["panicked"], reqRec["panic_after_write"], reqRec)
		}
	})

	t.Run("a panic BEFORE any write is 500 and panic_after_write is false", func(t *testing.T) {
		// The distinction has to survive in both directions. Recovery calls
		// WriteHeader(500) itself, so a naive implementation that read `wrote`
		// after that would report true for every panic and the marker would be
		// worthless.
		_, reqRec, status := run(t, func(w http.ResponseWriter, r *http.Request) {
			panic("boom before write")
		})
		if status != http.StatusInternalServerError {
			t.Fatalf("response status = %d, want 500", status)
		}
		if reqRec["status"] != "500" {
			t.Fatalf("request record status = %q, want 500", reqRec["status"])
		}
		if reqRec["panicked"] != "true" {
			t.Fatalf("panicked = %q, want \"true\": %v", reqRec["panicked"], reqRec)
		}
		if reqRec["panic_after_write"] != "false" {
			t.Fatalf("panic_after_write = %q, want \"false\"; recovery's own WriteHeader(500) must not be mistaken for the handler having written: %v",
				reqRec["panic_after_write"], reqRec)
		}
	})

	t.Run("a HIJACKED connection that panics is still marked", func(t *testing.T) {
		// FOUND BY THE SECURITY GATE, 2026-08-08, and demonstrated live: a
		// handler that hijacks, writes a response on the raw socket and then
		// panics logged `panicked=true panic_after_write=false status=500`.
		// That claims recovery answered cleanly while the client had already
		// been sent something else -- a FALSE NEGATIVE in the very control
		// CORE-14 added, reachable exactly because CORE-13 made Hijack work
		// through this wrapper. `wrote` stays false across a hijack because
		// the handler bypasses the recorder entirely, so the flag has to come
		// from the hijack itself.
		inner := newCapWriter()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)

		httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
				t.Errorf("Hijack failed: %v; this case needs it to SUCCEED", err)
			}
			// A real handler would now write its own status line and body to
			// the returned net.Conn. The point is that the recorder cannot see
			// it either way, which is why the flag must come from the hijack.
			panic("boom after hijack")
		})).ServeHTTP(succeedingHijacker{inner}, httptest.NewRequest(http.MethodGet, "/hijack", nil))

		ls := logLines(&buf)
		if len(ls) != 2 {
			t.Fatalf("got %d log lines, want 2: %q", len(ls), buf.String())
		}
		reqRec := parseLogLine(t, ls[1])
		if reqRec["panicked"] != "true" {
			t.Fatalf("panicked = %q, want \"true\": %v", reqRec["panicked"], reqRec)
		}
		if reqRec["panic_after_write"] != "true" {
			t.Fatalf("panic_after_write = %q, want \"true\".\n"+
				"After a hijack the handler owns the socket, so the response HAS begun even though the recorder never saw a Write. Reporting false here is the false negative the security gate demonstrated.",
				reqRec["panic_after_write"])
		}
		if reqRec["hijacked"] != "true" {
			t.Fatalf("hijacked = %q, want \"true\": %v", reqRec["hijacked"], reqRec)
		}
		if reqRec["status"] != "0" {
			t.Fatalf("status = %q, want \"0\".\n"+
				"The handler wrote the status line itself on the raw socket; this middleware never saw it, and inventing 200 (or 500) would be exactly the kind of made-up success CORE-14 exists to stop.",
				reqRec["status"])
		}
		// Recovery must NOT have tried to write 500 onto a socket that no
		// longer speaks HTTP.
		if inner.code == http.StatusInternalServerError {
			t.Fatal("recovery called WriteHeader(500) on a hijacked connection")
		}
	})

	t.Run("a Write AFTER a hijack cannot fabricate a status", func(t *testing.T) {
		// SECOND security-gate finding, 2026-08-08, also measured live: a stray
		// Write after a successful hijack routed through the recorder's
		// WriteHeader, flipped `wrote`, and replaced the honest status=0 with a
		// fabricated 200 -- while the client had been sent something else
		// entirely on the raw socket. Smaller than the panic case, same class.
		inner := newCapWriter()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)

		httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
				t.Errorf("Hijack failed: %v", err)
			}
			// A real net/http writer answers ErrHijacked here; the point is
			// what the RECORDER does with the attempt.
			_, _ = w.Write([]byte("stray"))
		})).ServeHTTP(succeedingHijacker{inner}, httptest.NewRequest(http.MethodGet, "/hijack-write", nil))

		f := parseLogLine(t, logLines(&buf)[0])
		if f["status"] != "0" {
			t.Fatalf("status = %q, want \"0\"; a write after a hijack must not invent a status this middleware never saw", f["status"])
		}
		if f["hijacked"] != "true" {
			t.Fatalf("hijacked = %q, want \"true\": %v", f["hijacked"], f)
		}
	})

	t.Run("a real status written BEFORE a hijack is kept", func(t *testing.T) {
		// The other direction: status=0 means "unknown", not "hijacked". If the
		// handler did send a status through this middleware before taking the
		// connection over, that status is genuinely what went out first and
		// must survive.
		inner := newCapWriter()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)

		httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusSwitchingProtocols)
			if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
				t.Errorf("Hijack failed: %v", err)
			}
		})).ServeHTTP(succeedingHijacker{inner}, httptest.NewRequest(http.MethodGet, "/upgrade", nil))

		f := parseLogLine(t, logLines(&buf)[0])
		if f["status"] != "101" {
			t.Fatalf("status = %q, want 101; a status actually written before the hijack is known and must be reported", f["status"])
		}
		if f["hijacked"] != "true" {
			t.Fatalf("hijacked = %q, want \"true\": %v", f["hijacked"], f)
		}
	})

	t.Run("a FAILED hijack changes nothing", func(t *testing.T) {
		// The flag must key on success. A hijack that errored did not take the
		// connection over, so the recorder's view is still complete and
		// recovery must still be able to answer 500.
		inner := newCapWriter()
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)

		httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, err := w.(http.Hijacker).Hijack(); err == nil {
				t.Error("Hijack succeeded; this case needs it to FAIL")
			}
			panic("boom after a failed hijack")
		})).ServeHTTP(capHijacker{inner}, httptest.NewRequest(http.MethodGet, "/hijack-fail", nil))

		ls := logLines(&buf)
		if len(ls) != 2 {
			t.Fatalf("got %d log lines, want 2: %q", len(ls), buf.String())
		}
		reqRec := parseLogLine(t, ls[1])
		if _, ok := reqRec["hijacked"]; ok {
			t.Fatalf("hijacked marker present after a FAILED hijack: %v", reqRec)
		}
		if reqRec["status"] != "500" {
			t.Fatalf("status = %q, want 500; a failed hijack must not stop recovery answering", reqRec["status"])
		}
		if reqRec["panic_after_write"] != "false" {
			t.Fatalf("panic_after_write = %q, want \"false\"; nothing was ever written", reqRec["panic_after_write"])
		}
	})

	t.Run("an ordinary request carries neither marker", func(t *testing.T) {
		// If the markers appeared on every line they would carry no signal, and
		// a log query could not select on presence.
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		rec := httptest.NewRecorder()
		httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fine", nil))

		ls := logLines(&buf)
		if len(ls) != 1 {
			t.Fatalf("got %d log lines, want 1: %q", len(ls), buf.String())
		}
		f := parseLogLine(t, ls[0])
		if _, ok := f["panicked"]; ok {
			t.Fatalf("a successful request carries panicked=%q; the marker must be absent so its presence means something", f["panicked"])
		}
		if _, ok := f["panic_after_write"]; ok {
			t.Fatalf("a successful request carries panic_after_write=%q; it must be absent", f["panic_after_write"])
		}
	})
}

// ---------------------------------------------------------------------------
// CORE-6: the panic stack must survive the log's value cap.
// ---------------------------------------------------------------------------

// deepPanic recurses depth times and then panics, so debug.Stack() has to
// render depth+ frames.
//
// This is the part of CORE-6 that matters as much as the fix. The ORIGINAL
// test passed while production truncated, because httptest's call stack is
// shorter than a real net/http request path: 962 bytes measured under the
// test, 1238 in production, with the cap at 1024. Forcing the depth here
// removes that dependency on how the test happens to be driven, so the blind
// spot cannot come back the next time the constant is tuned.
func deepPanic(depth int) {
	if depth <= 0 {
		panic("deep boom")
	}
	deepPanic(depth - 1)
}

// deepStack is deepPanic's non-panicking twin, used only to CALIBRATE the
// depth. A frame's rendered size depends on the absolute path of the source
// file -- the same depth yields ~1 KiB in a short checkout path and 14 KiB
// under a long temp dir -- so a hardcoded depth either never reaches the old
// 1024-byte cap (the test passes having proved nothing, which is the CORE-6
// blind spot itself) or sails past the new 8192-byte one (the test then
// measures the new limit rather than the exemption).
func deepStack(depth int) []byte {
	if depth <= 0 {
		return debug.Stack()
	}
	return deepStack(depth - 1)
}

// panicDepth returns a recursion depth whose stack is longer than the old
// 1024-byte cap with room to spare, and short enough that the extra frames the
// panic path adds (gopanic, the deferred recovery, the ServeHTTP chain) cannot
// push it near the new 8192-byte one.
func panicDepth(t *testing.T) int {
	t.Helper()
	for depth := 0; depth <= 256; depth++ {
		n := len(deepStack(depth))
		if n > 1300 {
			if n >= 4096 {
				t.Fatalf("the shortest stack exceeding 1300 bytes is already %d; one frame renders too large here to measure the exemption", n)
			}
			return depth
		}
	}
	t.Fatal("no recursion depth up to 256 produced a stack longer than 1300 bytes")
	return 0
}

// TestPanicStackNotTruncated pins CORE-6 at the HTTP layer: the stack logged
// for a panicking handler reaches its DEEPEST frame -- the one naming where
// the panic actually happened, which is the half a tail truncation destroys.
func TestPanicStackNotTruncated(t *testing.T) {
	depth := panicDepth(t)

	var buf bytes.Buffer
	lg := logging.New(&buf, logging.LevelInfo)
	rec := httptest.NewRecorder()
	httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deepPanic(depth)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/deep", nil))

	ls := logLines(&buf)
	if len(ls) != 2 {
		t.Fatalf("got %d log lines, want 2: %q", len(ls), buf.String())
	}
	f := parseLogLine(t, ls[0])
	stack := f["stack"]

	// GUARD: if the stack were somehow shorter than the old 1024 cap, this
	// test would pass without ever exercising the truncation path -- which is
	// precisely the blind spot CORE-6 was filed about.
	if len(stack) <= 1024 {
		t.Fatalf("the logged stack is only %d bytes, at or under the OLD 1024-byte cap, so this test never exercised truncation and proves nothing. Increase the recursion depth.", len(stack))
	}
	if strings.Contains(stack, "...(truncated)") {
		t.Fatalf("the panic stack was truncated at %d bytes.\n"+
			"A stack's TAIL is its useful half -- the deepest frames say where the panic happened -- so capping it discards exactly what an operator needs.",
			len(stack))
	}
	// The deepest frame must be present, named specifically rather than
	// inferred from the absence of a truncation marker.
	if !strings.Contains(stack, "deepPanic") {
		t.Fatalf("the logged stack does not name deepPanic, the frame that panicked:\n%s", stack)
	}
	if !strings.Contains(stack, "TestPanicStackNotTruncated") {
		t.Fatalf("the logged stack does not reach the test frame; it was cut short:\n%s", stack)
	}
}
