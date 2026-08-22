package httpapi

import (
	"testing"
	"time"
)

// TestTokenBucketRefillMath checks the bucket admits exactly a burst, then
// refills at the configured rate and never past capacity.
func TestTokenBucketRefillMath(t *testing.T) {
	rl := newRateLimiter(AuthRateLimit{PerSecond: 2, Burst: 4})
	now := time.Unix(0, 0)
	key := "k"

	// A fresh source starts full: 4 admits, no time passing.
	for i := 0; i < 4; i++ {
		if ok, _ := rl.allow(key, now); !ok {
			t.Fatalf("admit %d of the burst should succeed", i+1)
		}
	}
	// Bucket empty now.
	ok, wait := rl.allow(key, now)
	if ok {
		t.Fatalf("the 5th request should be refused with an empty bucket")
	}
	// At 2 tokens/sec, one token needs 0.5s.
	if want := 500 * time.Millisecond; wait != want {
		t.Fatalf("Retry-After wait = %v, want %v", wait, want)
	}

	// Advance 0.5s -> exactly one token.
	now = now.Add(500 * time.Millisecond)
	if ok, _ := rl.allow(key, now); !ok {
		t.Fatalf("one refilled token should admit one request")
	}
	if ok, _ := rl.allow(key, now); ok {
		t.Fatalf("the single refilled token was already spent")
	}

	// Advance well past capacity; the bucket must cap at burst, not overflow.
	now = now.Add(10 * time.Second)
	for i := 0; i < 4; i++ {
		if ok, _ := rl.allow(key, now); !ok {
			t.Fatalf("refill should have capped at burst=4; admit %d failed", i+1)
		}
	}
	if ok, _ := rl.allow(key, now); ok {
		t.Fatalf("bucket must not exceed its capacity of 4")
	}
}

// TestTokenBucketClockRewindMakesNoTokens proves a backwards clock cannot mint
// tokens: elapsed <= 0 adds nothing.
func TestTokenBucketClockRewindMakesNoTokens(t *testing.T) {
	rl := newRateLimiter(AuthRateLimit{PerSecond: 100, Burst: 1})
	now := time.Unix(100, 0)
	if ok, _ := rl.allow("k", now); !ok {
		t.Fatalf("first request should be admitted")
	}
	// Clock jumps backwards; must not refill.
	if ok, _ := rl.allow("k", now.Add(-50*time.Second)); ok {
		t.Fatalf("a backwards clock must not manufacture a token")
	}
}

// TestRateLimiterGCDropsFullBuckets proves the sweep removes buckets that have
// refilled to capacity (they hold no throttling state and a fresh bucket would
// admit identically) while retaining ones still below capacity, so the map
// cannot grow without bound past the sources being actively throttled.
//
// gcInterval is overridden to a short window so the timing is deterministic:
// with the 60s production default, any idle bucket has fully refilled long
// before a sweep, which is correct but makes the "retained" case unreachable in
// a fast test.
func TestRateLimiterGCDropsFullBuckets(t *testing.T) {
	rl := newRateLimiter(AuthRateLimit{PerSecond: 1, Burst: 10})
	rl.gcInterval = 2 * time.Second
	now := time.Unix(0, 0)

	// "idle": spent one token, left alone -> will refill to capacity by sweep.
	rl.allow("idle", now) // 10 -> 9
	// "throttled": drained to zero -> still below capacity by sweep.
	for i := 0; i < 10; i++ {
		rl.allow("throttled", now) // 10 -> 0
	}
	if got := len(rl.buckets); got != 2 {
		t.Fatalf("expected 2 tracked sources before GC, got %d", got)
	}

	// Advance exactly the GC interval. At 1 tok/s over 2s:
	//   "idle":      9 + 2 = 11, capped at 10  -> AT capacity  -> swept
	//   "throttled": 0 + 2 = 2  (< 10)         -> below cap     -> retained
	now = now.Add(rl.gcInterval)
	rl.allow("current", now) // triggers the sweep, then is created fresh

	if _, ok := rl.buckets["idle"]; ok {
		t.Fatalf("a bucket refilled to capacity should have been swept")
	}
	if _, ok := rl.buckets["throttled"]; !ok {
		t.Fatalf("a bucket still below capacity must be retained")
	}
	if _, ok := rl.buckets["current"]; !ok {
		t.Fatalf("a source touched at sweep time must be present")
	}
}
