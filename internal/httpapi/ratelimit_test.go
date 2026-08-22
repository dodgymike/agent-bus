package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// rlClock is a manual clock so the token-bucket refill can be driven
// deterministically -- a rate-limit test that relied on wall-clock time would
// be either flaky or slow.
type rlClock struct {
	mu sync.Mutex
	t  time.Time
}

func newRLClock() *rlClock { return &rlClock{t: time.Unix(1_700_000_000, 0)} }

func (c *rlClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *rlClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newRateLimitedServer builds a Server whose three unauthenticated credential
// routes are throttled per source by the given bucket, on a manual clock. The
// auth service is real so the routes actually exist and register.
func newRateLimitedServer(t *testing.T, rl httpapi.AuthRateLimit, clock *rlClock) *httpapi.Server {
	t.Helper()
	minter, err := ids.NewAgentIDMinter(authTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}
	return httpapi.New(httpapi.Options{
		Identity:      testIdentity(authTestBusID),
		Logger:        logging.New(io.Discard, logging.LevelDebug),
		Auth:          svc,
		AuthRateLimit: rl,
		Now:           clock.now,
	})
}

// postFrom issues a POST to path from source remoteIP (an "ip:port" string), so
// a test controls the per-source key the limiter buckets on.
func postFrom(srv *httpapi.Server, path, remoteIP, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, r)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteIP
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestSessionBeginRateLimit is the named proof for AUTH-1-FU-RATELIMIT: a single
// source is throttled on POST /v1/session/begin once it exhausts its burst, and
// the refusal is a clean 429 with Retry-After -- never a disconnect (invariant
// 10). The final sub-test is the non-vacuity control: with the limiter disabled
// the identical flood produces NO 429, so the 429s asserted above come from the
// limiter and nothing else.
func TestSessionBeginRateLimit(t *testing.T) {
	const (
		burst    = 3
		perSec   = 1.0
		src      = "203.0.113.7:5555"
		beginReq = `{"agent_id":"` + authTestBusID + `.nobody"}`
	)

	t.Run("burst is admitted then the next request is throttled 429", func(t *testing.T) {
		clock := newRLClock()
		srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: perSec, Burst: burst}, clock)

		// The clock never advances inside this loop, so no tokens refill: exactly
		// `burst` requests are admitted, then the bucket is empty.
		for i := 0; i < burst; i++ {
			rec := postFrom(srv, httpapi.RouteSessionBegin, src, beginReq)
			if rec.Code == http.StatusTooManyRequests {
				t.Fatalf("request %d/%d was throttled while the burst should still have tokens: got 429", i+1, burst)
			}
		}

		rec := postFrom(srv, httpapi.RouteSessionBegin, src, beginReq)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the request past the burst should be throttled: got status %d, want 429", rec.Code)
		}
		ra := rec.Header().Get("Retry-After")
		if ra == "" {
			t.Fatalf("a 429 must carry Retry-After; it was absent")
		}
		secs, err := strconv.Atoi(ra)
		if err != nil || secs < 1 {
			t.Fatalf("Retry-After must be a positive integer number of seconds, got %q (err %v)", ra, err)
		}
		if got := decodeBody(t, rec)["error"]; got != "rate limit exceeded" {
			t.Fatalf("429 body error = %v, want %q", got, "rate limit exceeded")
		}
	})

	t.Run("a refilled token admits again after Retry-After", func(t *testing.T) {
		clock := newRLClock()
		srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: perSec, Burst: burst}, clock)

		for i := 0; i < burst; i++ {
			postFrom(srv, httpapi.RouteSessionBegin, src, beginReq)
		}
		if rec := postFrom(srv, httpapi.RouteSessionBegin, src, beginReq); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 with an empty bucket, got %d", rec.Code)
		}
		// One token refills after 1/perSec seconds.
		clock.advance(time.Second)
		if rec := postFrom(srv, httpapi.RouteSessionBegin, src, beginReq); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("after the bucket refilled a token the request should be admitted, got 429")
		}
		// And the single refilled token is spent, so the next is throttled again.
		if rec := postFrom(srv, httpapi.RouteSessionBegin, src, beginReq); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the refilled token should have been spent by the previous request, got %d", rec.Code)
		}
	})

	t.Run("NON-VACUITY CONTROL: with the limiter disabled the same flood is never 429", func(t *testing.T) {
		clock := newRLClock()
		// Burst 0 is the documented disabled state.
		srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: perSec, Burst: 0}, clock)
		for i := 0; i < burst*5; i++ {
			if rec := postFrom(srv, httpapi.RouteSessionBegin, src, beginReq); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("request %d was throttled although the limiter is disabled: the 429s in the sibling tests would then not be attributable to the limiter", i+1)
			}
		}
	})
}

// TestRateLimitIsPerSource proves the bucket is keyed on the source address: one
// source exhausting its burst does not throttle a different source.
func TestRateLimitIsPerSource(t *testing.T) {
	const burst = 2
	clock := newRLClock()
	srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: 1, Burst: burst}, clock)
	body := `{"agent_id":"` + authTestBusID + `.nobody"}`

	// Exhaust source A.
	for i := 0; i < burst; i++ {
		postFrom(srv, httpapi.RouteSessionBegin, "198.51.100.1:1000", body)
	}
	if rec := postFrom(srv, httpapi.RouteSessionBegin, "198.51.100.1:1000", body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("source A should be throttled after its burst, got %d", rec.Code)
	}
	// Source B is unaffected: a different IP, and the PORT must be ignored (the
	// key is the host only, so the same host on a new port shares the bucket --
	// checked separately below).
	if rec := postFrom(srv, httpapi.RouteSessionBegin, "198.51.100.2:2000", body); rec.Code == http.StatusTooManyRequests {
		t.Fatalf("source B shares no bucket with A and should be admitted, got 429")
	}
}

// TestRateLimitKeyIgnoresPort proves the source key is the host without the
// port: the same client reconnecting on a new ephemeral port must not reset its
// bucket, or the limit would be trivially bypassed.
func TestRateLimitKeyIgnoresPort(t *testing.T) {
	const burst = 2
	clock := newRLClock()
	srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: 1, Burst: burst}, clock)
	body := `{"agent_id":"` + authTestBusID + `.nobody"}`

	postFrom(srv, httpapi.RouteSessionBegin, "192.0.2.50:1111", body)
	postFrom(srv, httpapi.RouteSessionBegin, "192.0.2.50:2222", body)
	// Two requests from the same host on different ports have spent the burst.
	if rec := postFrom(srv, httpapi.RouteSessionBegin, "192.0.2.50:3333", body); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the same host on a new port must share the bucket and be throttled, got %d", rec.Code)
	}
}

// TestRateLimitCoversAllThreeCredentialRoutes proves the guard is on each of the
// three unauthenticated credential routes, and on NO OTHER route -- /healthz is
// anonymous too but carries no bus state, so it is deliberately unthrottled.
func TestRateLimitCoversAllThreeCredentialRoutes(t *testing.T) {
	limited := []string{
		httpapi.RouteEnroll,
		httpapi.RouteSessionBegin,
		httpapi.RouteSessionComplete,
	}
	for _, path := range limited {
		path := path
		t.Run("throttled: "+path, func(t *testing.T) {
			clock := newRLClock()
			srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: 1, Burst: 2}, clock)
			src := "192.0.2.77:9000"
			var got429 bool
			for i := 0; i < 5; i++ {
				if rec := postFrom(srv, path, src, `{}`); rec.Code == http.StatusTooManyRequests {
					got429 = true
					break
				}
			}
			if !got429 {
				t.Fatalf("route %s was never throttled within 5 requests from one source", path)
			}
		})
	}

	t.Run("NOT throttled: /healthz", func(t *testing.T) {
		clock := newRLClock()
		srv := newRateLimitedServer(t, httpapi.AuthRateLimit{PerSecond: 1, Burst: 2}, clock)
		src := "192.0.2.88:9000"
		for i := 0; i < 20; i++ {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.RemoteAddr = src
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code == http.StatusTooManyRequests {
				t.Fatalf("/healthz must not be rate-limited, got 429 on request %d", i+1)
			}
		}
	})
}

// TestRateLimitDisabledByDefault proves a Server built with the zero-value
// Options (no AuthRateLimit) serves the credential routes unthrottled, so the
// whole existing test suite and any embedder that does not opt in is unchanged.
func TestRateLimitDisabledByDefault(t *testing.T) {
	srv, _ := newAuthServer(t) // no AuthRateLimit set
	src := "192.0.2.99:1234"
	for i := 0; i < 50; i++ {
		if rec := postFrom(srv, httpapi.RouteSessionBegin, src, `{}`); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("with no AuthRateLimit configured the route must be unthrottled, got 429 on request %d", i+1)
		}
	}
}
