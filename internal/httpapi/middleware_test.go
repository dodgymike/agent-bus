package httpapi_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// logFieldRe tokenises one logfmt "key=value" pair, where value is either a
// strconv.Quote-quoted string or a bare (space-free) token. It mirrors the
// exact quoting rule in internal/logging.writeValue.
var logFieldRe = regexp.MustCompile(`([A-Za-z0-9_.\-]+)=("(?:\\.|[^"\\])*"|[^\s]*)`)

// parseLogLine tokenises a single logfmt record into its key/value pairs.
// It fails the test if any byte in the line is not accounted for by a
// well-formed field -- exactly the shape a torn/interleaved concurrent
// write, or an injected fragment, would produce.
func parseLogLine(t *testing.T, line string) map[string]string {
	t.Helper()
	fields := map[string]string{}
	matches := logFieldRe.FindAllStringSubmatchIndex(line, -1)
	consumed := 0
	for _, m := range matches {
		if m[0] < consumed {
			continue
		}
		gap := line[consumed:m[0]]
		if consumed > 0 && gap != " " {
			t.Fatalf("unexpected content %q between fields in line %q", gap, line)
		}
		key := line[m[2]:m[3]]
		raw := line[m[4]:m[5]]
		val := raw
		if strings.HasPrefix(raw, `"`) {
			uq, err := strconv.Unquote(raw)
			if err != nil {
				t.Fatalf("unquoting value %q in line %q: %v", raw, line, err)
			}
			val = uq
		}
		if _, dup := fields[key]; dup {
			t.Fatalf("duplicate key %q in line %q (possible injected field)", key, line)
		}
		fields[key] = val
		consumed = m[1]
	}
	if consumed != len(line) {
		t.Fatalf("trailing unparsed content %q in line %q", line[consumed:], line)
	}
	if len(fields) == 0 {
		t.Fatalf("no fields parsed from line %q", line)
	}
	return fields
}

// logLines splits a captured log buffer into its non-empty lines.
func logLines(buf *bytes.Buffer) []string {
	s := strings.TrimRight(buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TestLoggingMiddleware covers CORE-4: internal/httpapi.LoggingMiddleware.
func TestLoggingMiddleware(t *testing.T) {
	t.Run("emits method path status latency, status captured through the wrapper", func(t *testing.T) {
		cases := []struct {
			name       string
			handler    http.HandlerFunc
			wantStatus string
		}{
			{
				name: "handler writes 201 explicitly",
				handler: func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte("created"))
				},
				wantStatus: "201",
			},
			{
				name: "handler writes nothing -> implicit 200",
				handler: func(w http.ResponseWriter, r *http.Request) {
					// deliberately no write: net/http implies 200.
				},
				wantStatus: "200",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				lg := logging.New(&buf, logging.LevelInfo)
				mw := httpapi.LoggingMiddleware(lg, tc.handler)

				req := httptest.NewRequest(http.MethodGet, "/widgets/42", nil)
				rec := httptest.NewRecorder()
				mw.ServeHTTP(rec, req)

				ls := logLines(&buf)
				if len(ls) != 1 {
					t.Fatalf("got %d log lines, want 1: %q", len(ls), buf.String())
				}
				f := parseLogLine(t, ls[0])
				if f["method"] != http.MethodGet {
					t.Fatalf("method = %q, want GET", f["method"])
				}
				if f["path"] != "/widgets/42" {
					t.Fatalf("path = %q, want /widgets/42", f["path"])
				}
				if f["status"] != tc.wantStatus {
					t.Fatalf("status = %q, want %q", f["status"], tc.wantStatus)
				}
				if _, ok := f["latency_ms"]; !ok {
					t.Fatalf("latency_ms field missing: %v", f)
				}
				if f["request_id"] == "" {
					t.Fatalf("request_id field missing/empty: %v", f)
				}
			})
		}
	})

	t.Run("inbound X-Request-Id injection is neutralised, exactly one log line", func(t *testing.T) {
		malicious := []string{
			"abc\ndef",
			"abc\nlevel=error msg=\"forged\" forged_field=1",
			"abc=def",
			"abc\x00def",
			"abc\x1bdef",
			"abc def",
			"key=value",
		}
		for _, id := range malicious {
			t.Run(strconv.Quote(id), func(t *testing.T) {
				var buf bytes.Buffer
				lg := logging.New(&buf, logging.LevelInfo)
				mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))

				req := httptest.NewRequest(http.MethodGet, "/x", nil)
				req.Header.Set(httpapi.RequestIDHeader, id)
				rec := httptest.NewRecorder()
				mw.ServeHTTP(rec, req)

				ls := logLines(&buf)
				if len(ls) != 1 {
					t.Fatalf("malicious id %q produced %d log lines, want exactly 1: %q", id, len(ls), buf.String())
				}
				f := parseLogLine(t, ls[0])
				if f["request_id"] == id {
					t.Fatalf("malicious id %q was reflected verbatim into the log instead of being rejected", id)
				}
				if v, ok := f["forged_field"]; ok {
					t.Fatalf("malicious id injected a forged field forged_field=%q into %v", v, f)
				}
				echoed := rec.Header().Get(httpapi.RequestIDHeader)
				if echoed == id {
					t.Fatalf("malicious id %q was echoed verbatim on the response header", id)
				}
				if httpapi.SanitizeRequestID(echoed) == "" {
					t.Fatalf("server-generated echoed request id %q is not itself a valid sanitized id", echoed)
				}
			})
		}
	})

	t.Run("over-length inbound request id is bounded by MaxRequestIDLen", func(t *testing.T) {
		long := strings.Repeat("a", httpapi.MaxRequestIDLen+1)
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(httpapi.RequestIDHeader, long)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		echoed := rec.Header().Get(httpapi.RequestIDHeader)
		if echoed == long {
			t.Fatalf("over-length request id (%d bytes) was accepted verbatim", len(long))
		}
		if len(echoed) > httpapi.MaxRequestIDLen {
			t.Fatalf("echoed request id is %d bytes, want <= MaxRequestIDLen=%d", len(echoed), httpapi.MaxRequestIDLen)
		}

		ls := logLines(&buf)
		if len(ls) != 1 {
			t.Fatalf("got %d log lines, want 1", len(ls))
		}
		f := parseLogLine(t, ls[0])
		if f["request_id"] != echoed {
			t.Fatalf("logged request_id %q != echoed response header %q", f["request_id"], echoed)
		}

		// Direct unit check on the boundary, independent of the middleware.
		atLimit := strings.Repeat("a", httpapi.MaxRequestIDLen)
		if got := httpapi.SanitizeRequestID(atLimit); got != atLimit {
			t.Fatalf("SanitizeRequestID at exactly MaxRequestIDLen = %q, want unchanged", got)
		}
		if got := httpapi.SanitizeRequestID(long); got != "" {
			t.Fatalf("SanitizeRequestID at MaxRequestIDLen+1 = %q, want \"\"", got)
		}
	})

	t.Run("level filtering honours the configured level", func(t *testing.T) {
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelError)
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if buf.Len() != 0 {
			t.Fatalf("request log line (Info) emitted while configured level is Error: %q", buf.String())
		}
	})

	t.Run("panic recovery: 500, logged error record, test process survives", func(t *testing.T) {
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}))

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}

		ls := logLines(&buf)
		if len(ls) != 2 {
			t.Fatalf("got %d log lines, want 2 (error record + request record): %q", len(ls), buf.String())
		}
		errFields := parseLogLine(t, ls[0])
		if errFields["level"] != "error" {
			t.Fatalf("first line level = %q, want error: %v", errFields["level"], errFields)
		}
		if errFields["msg"] != "panic serving request" {
			t.Fatalf("first line msg = %q, want %q", errFields["msg"], "panic serving request")
		}
		if errFields["panic"] != "boom" {
			t.Fatalf("panic field = %q, want boom", errFields["panic"])
		}
		if errFields["stack"] == "" {
			t.Fatalf("stack field missing/empty")
		}

		reqFields := parseLogLine(t, ls[1])
		if reqFields["status"] != "500" {
			t.Fatalf("request record status = %q, want 500", reqFields["status"])
		}
	})

	t.Run("http.ErrAbortHandler is re-panicked, not swallowed", func(t *testing.T) {
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic(http.ErrAbortHandler)
		}))

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()

		var recovered interface{}
		func() {
			defer func() { recovered = recover() }()
			mw.ServeHTTP(rec, req)
		}()

		if recovered == nil {
			t.Fatalf("expected http.ErrAbortHandler to propagate out of ServeHTTP, got no panic")
		}
		err, ok := recovered.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("recovered value = %v (%T), want http.ErrAbortHandler", recovered, recovered)
		}
		if buf.Len() != 0 {
			t.Fatalf("expected no log line for a re-panicked ErrAbortHandler, got: %q", buf.String())
		}
	})

	t.Run("concurrent requests produce no torn or interleaved lines", func(t *testing.T) {
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		mw := httpapi.LoggingMiddleware(lg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		const n = 200
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "/concurrent", nil)
				rec := httptest.NewRecorder()
				mw.ServeHTTP(rec, req)
			}()
		}
		wg.Wait()

		ls := logLines(&buf)
		if len(ls) != n {
			t.Fatalf("got %d log lines, want %d (a torn/interleaved write would change this count)", len(ls), n)
		}
		seen := make(map[string]bool, n)
		for _, l := range ls {
			f := parseLogLine(t, l)
			id := f["request_id"]
			if id == "" {
				t.Fatalf("empty request_id in line %q", l)
			}
			if seen[id] {
				t.Fatalf("duplicate request_id %q across concurrent requests", id)
			}
			seen[id] = true
			if f["path"] != "/concurrent" {
				t.Fatalf("path = %q, want /concurrent (line corrupted?): %q", f["path"], l)
			}
		}
	})
}
