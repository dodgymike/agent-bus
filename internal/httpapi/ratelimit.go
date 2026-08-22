package httpapi

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// AuthRateLimit configures the per-source token bucket that fronts the three
// UNAUTHENTICATED credential routes -- /v1/enroll, /v1/session/begin and
// /v1/session/complete (AUTH-1-FU-RATELIMIT). It is a VALUE, not a pointer, so
// there is no typed-nil trap: the zero value (Burst <= 0) simply disables the
// limiter.
//
// The limiter exists because those three routes are anonymous by necessity
// (invariant 3 -- they are how a credential is obtained, so none can require
// one) and every admission-control cap behind them is GLOBAL: without a
// per-source cap a single anonymous caller can exhaust MaxRosterEntries with
// enrol requests or MaxSessions with session/begin requests and deny the whole
// bus. Rate limiting sits IN FRONT of the allow-list; it does not change the
// allow-list's membership (invariant 3 -- the allow-list is the security
// boundary and is not widened here).
type AuthRateLimit struct {
	// PerSecond is the sustained refill rate of each source's bucket, in tokens
	// per second. One token is spent per request. It must be > 0 for the limiter
	// to be enabled.
	PerSecond float64

	// Burst is the bucket capacity: the largest instantaneous run of requests a
	// single source may make before it is throttled to PerSecond. It must be > 0
	// for the limiter to be enabled; a zero or negative Burst DISABLES the
	// limiter entirely, which is the zero-value default and preserves the
	// historical unlimited behaviour.
	Burst int
}

// enabled reports whether this configuration turns the limiter on. Both fields
// must be positive: a bucket that can never hold a token, or that never
// refills, would refuse every request forever, which is a worse failure than
// no limiter at all.
func (a AuthRateLimit) enabled() bool { return a.Burst > 0 && a.PerSecond > 0 }

// rateLimitedRoutes is the set of paths the per-source limiter guards. It is
// DERIVED FROM the three credential route constants, not from
// unauthenticatedRoutes: the limiter guards exactly the three routes named in
// AUTH-1-FU-RATELIMIT, and must not silently start or stop guarding a route
// because the allow-list changed for an unrelated reason. /healthz, /v1/info
// and /v1/discovery are anonymous too but carry no bus state and mutate
// nothing, so they are deliberately NOT limited.
//
// Matching is EXACT string equality against r.URL.Path, the same fail-closed
// rule authMiddleware uses: a non-canonical spelling is simply not in this set,
// so it is not rate-limited -- and it is not served either, because it is not a
// registered route.
var rateLimitedRoutes = map[string]struct{}{
	RouteEnroll:          {},
	RouteSessionBegin:    {},
	RouteSessionComplete: {},
}

// isRateLimitedRoute reports whether path is one of the three credential routes
// the per-source limiter guards.
func isRateLimitedRoute(path string) bool {
	_, ok := rateLimitedRoutes[path]
	return ok
}

// tokenBucket is one source's bucket. It is guarded by rateLimiter.mu and holds
// no lock of its own.
type tokenBucket struct {
	tokens float64   // available tokens, in [0, burst]
	last   time.Time // when tokens was last refilled
}

// rateLimiter is a per-source token-bucket limiter, stdlib only (invariant 8 --
// no golang.org/x/time/rate). One bucket per source key; each request spends
// one token, and a source with an empty bucket is refused a 429 carrying
// Retry-After rather than being disconnected (invariant 10 -- a merely-buggy
// or merely-busy client must never lose its socket).
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens per second
	burst   float64 // bucket capacity

	// lastGC and gcInterval bound the memory the map can hold. A source that has
	// refilled to full capacity is indistinguishable from one never seen, so its
	// bucket is dropped on the next sweep -- which is why an attacker rotating
	// source IPs cannot grow this map without bound past the sources seen within
	// one gcInterval that are STILL being throttled.
	lastGC     time.Time
	gcInterval time.Duration
}

// defaultRateLimitGCInterval is how often the limiter sweeps out fully-refilled
// buckets. It is a memory-hygiene knob, not a security parameter: removing a
// full bucket loses no throttling state because a full bucket admits exactly
// what a fresh one would.
const defaultRateLimitGCInterval = time.Minute

// newRateLimiter builds a limiter from a validated AuthRateLimit. The caller
// must have checked cfg.enabled(); this does not, and a disabled cfg would
// build a limiter that refuses everything.
func newRateLimiter(cfg AuthRateLimit) *rateLimiter {
	return &rateLimiter{
		buckets:    make(map[string]*tokenBucket),
		rate:       cfg.PerSecond,
		burst:      float64(cfg.Burst),
		gcInterval: defaultRateLimitGCInterval,
	}
}

// allow spends one token for key at time now. It returns whether the request is
// admitted and, when it is not, how long the caller should wait before the
// bucket holds a whole token again -- the value for the Retry-After header.
//
// The refusal is a REFUSAL, never a disconnect: allow returns false and the
// middleware answers 429. That is invariant 10's rule for a client that has
// done nothing worse than send too fast.
func (rl *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.maybeGCLocked(now)

	b := rl.buckets[key]
	if b == nil {
		// A never-seen source starts with a full bucket, so a legitimate first
		// contact is never throttled.
		b = &tokenBucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	} else {
		// Refill for the elapsed time, capped at burst. A clock that went
		// backwards (now < last) must not MANUFACTURE tokens, so a negative
		// elapsed adds nothing.
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens = math.Min(rl.burst, b.tokens+elapsed.Seconds()*rl.rate)
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Not enough for a whole token. Tell the caller how long until one refills,
	// rounded UP to whole seconds by the middleware (Retry-After is expressed in
	// seconds), and never less than the time for a single token.
	need := 1 - b.tokens
	wait := time.Duration(need / rl.rate * float64(time.Second))
	if wait <= 0 {
		wait = time.Second
	}
	return false, wait
}

// maybeGCLocked drops fully-refilled buckets when at least gcInterval has passed
// since the last sweep. Caller holds rl.mu.
//
// A bucket at capacity holds no throttling state -- it would admit the next
// request exactly as a fresh bucket would -- so deleting it is free and keeps
// the map proportional to the number of sources CURRENTLY being throttled plus
// whatever arrived since the last sweep, rather than to every source ever seen.
func (rl *rateLimiter) maybeGCLocked(now time.Time) {
	if rl.lastGC.IsZero() {
		rl.lastGC = now
		return
	}
	if now.Sub(rl.lastGC) < rl.gcInterval {
		return
	}
	rl.lastGC = now
	for key, b := range rl.buckets {
		refilled := b.tokens
		if elapsed := now.Sub(b.last); elapsed > 0 {
			refilled = math.Min(rl.burst, b.tokens+elapsed.Seconds()*rl.rate)
		}
		if refilled >= rl.burst {
			delete(rl.buckets, key)
		}
	}
}

// rateLimitMiddleware throttles the three credential routes per source. It is
// always in the chain; when the limiter is disabled (s.authRateLimiter == nil)
// or the path is not one of the three guarded routes, it is a pass-through.
//
// The source key is remoteHost(r): the TCP peer address with its port stripped,
// with proxy headers deliberately ignored. HONEST LIMITATION: behind a shared
// NAT, a reverse proxy, an SSH tunnel or the Docker bridge (where every
// container appears as e.g. 172.17.0.1), many distinct clients collapse to ONE
// key and share ONE bucket, so they throttle each other. This is a known
// weakness of IP-based limiting and is not solved here -- but the alternative,
// trusting an X-Forwarded-For header, is trivially forged by the very attacker
// this guards against, so the peer address is the only source identity this
// server can attest. remoteHost is the same value LoggingMiddleware already
// records as "remote", so a 429 in the log and the request line agree on who
// was throttled.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authRateLimiter == nil || !isRateLimitedRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		key := remoteHost(r)
		ok, retryAfter := s.authRateLimiter.allow(key, s.now())
		if ok {
			next.ServeHTTP(w, r)
			return
		}

		secs := int(math.Ceil(retryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		// Info, not Warn: a throttled source is expected operation of the guard,
		// not an error, but an operator watching for an attack wants it by
		// default. The token is never involved here -- this runs before any
		// credential is read -- so there is nothing sensitive to leak.
		s.log.Info("request rate-limited: per-source cap on an unauthenticated credential route",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"remote", key,
			"retry_after_s", secs,
		)
		s.writeJSON(w, r, http.StatusTooManyRequests, ErrorResponse{Error: "rate limit exceeded"})
	})
}
