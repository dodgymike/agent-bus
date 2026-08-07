// Package auth_test exercises AUTH-1 through the package's EXPORTED surface
// only. Enrolment and session establishment are the routes that ISSUE a
// credential, so what matters is the contract a client and the HTTP layer see;
// an internal test could assert on the session map and would pin the
// implementation instead of the promise.
//
// Two rules hold throughout this package's tests:
//
//   - Failures are matched with errors.Is against the exported sentinels, never
//     against error text. The text is diagnostic detail for an operator and is
//     documented as free to change.
//   - Nothing sleeps. Every expiry assertion drives the injected Options.Now,
//     so the tests are deterministic and the whole package runs in well under a
//     second.
package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// testBusID is the bus every minted id in these tests is qualified with
// (invariant 2). It is spelled out rather than generated so an assertion can
// name the exact expected id.
const testBusID = "bus-under-test"

// epoch is the fixed instant fakeClock starts from. A fixed, non-zero, UTC
// instant keeps expiry assertions readable and keeps a zero time.Time (which
// several helpers treat as "unset") out of the results.
var epoch = time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)

// fakeClock is the injected Options.Now. It is mutex-guarded because the
// concurrency test reads it from many goroutines under -race, where an
// unguarded time.Time field would be a genuine data race in the TEST rather
// than in the code under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: epoch} }

// Now is the func passed as auth.Options.Now.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward. It never moves backwards: a rewinding clock
// would be testing a scenario the server does not promise anything about.
func (c *fakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("fakeClock.Advance: the clock never goes backwards")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newMinter returns a real ids.AgentIDMinter over a fresh per-name suffix
// allocator. The real minter is used rather than a stub precisely because the
// "<bus-id>.<name>-<n>" shape and the monotonic per-name suffix are what
// invariant 1 promises about enrolment.
func newMinter(t *testing.T) *ids.AgentIDMinter {
	t.Helper()
	m, err := ids.NewAgentIDMinter(testBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	return m
}

// newService builds a Service, filling in a real minter and the fake clock when
// the caller did not supply them. It fails the test rather than returning an
// error: a service that will not build is never the thing under test here.
func newService(t *testing.T, opts auth.Options) (*auth.Service, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	if opts.Minter == nil {
		opts.Minter = newMinter(t)
	}
	if opts.Now == nil {
		opts.Now = clock.Now
	}
	svc, err := auth.NewService(opts)
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}
	return svc, clock
}

// newKeypair returns a real Ed25519 keypair. Real keys, not fixtures: the whole
// point of the session tests is that a signature produced by the documented
// procedure verifies, and a hand-built byte slice would prove nothing about
// that.
func newKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 keypair: %v", err)
	}
	return pub, priv
}

// enrolAgent enrols name with a fresh keypair and returns the minted id and the
// PRIVATE half, which only the client ever holds.
func enrolAgent(t *testing.T, svc *auth.Service, name, idemKey string) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv := newKeypair(t)
	res, err := svc.Enrol(auth.EnrolRequest{Name: name, PublicKey: pub, IdempotencyKey: idemKey})
	if err != nil {
		t.Fatalf("enrolling %q: %v", name, err)
	}
	return res.AgentID, priv
}

// signToken produces the signature CompleteSession expects: over
// SessionSigningContext + token, exactly as a client must.
func signToken(priv ed25519.PrivateKey, token string) []byte {
	return ed25519.Sign(priv, []byte(auth.SessionSigningContext+token))
}

// mustNotPanic runs fn and turns a panic into a named test failure instead of a
// crashed binary.
//
// This is not decoration. ed25519.Verify PANICS on a public key whose length is
// not ed25519.PublicKeySize — it returns false only for a bad SIGNATURE — so
// every one of these paths takes untrusted, attacker-chosen bytes towards a
// call that will abort the process if the length check in front of it is ever
// removed. A recovered panic here says exactly which input reopened that hole.
func mustNotPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s PANICKED (%v); a wrong-size key or signature must be a clean error, never a panic: ed25519.Verify aborts the process on a wrong-size public key, so this is a remote denial of service on an unauthenticated route", what, r)
		}
	}()
	fn()
}

// stubRoster is a Roster that stores whatever it is handed, INCLUDING a public
// key of the wrong length.
//
// It exists to reach a path MemoryRoster cannot produce: the roster feeding
// ed25519.Verify a corrupt key. That is not hypothetical — AUTH-3 rebuilds this
// roster FROM DISK, where a truncated record turns into exactly this state, and
// CompleteSession's own length check is what stands between it and a panic on
// an unauthenticated route. A test double that validated on the way in could
// never exercise that check.
type stubRoster struct {
	mu   sync.Mutex
	byID map[string]auth.RosterEntry
}

func newStubRoster() *stubRoster {
	return &stubRoster{byID: make(map[string]auth.RosterEntry)}
}

// Put implements auth.Roster. It deliberately does NOT length-check the key —
// see the type doc — but it does keep the duplicate-id refusal, because that
// one is a real invariant (ids are never reused) rather than a validation the
// double is standing in for.
func (r *stubRoster) Put(e auth.RosterEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[e.AgentID]; ok {
		return auth.ErrDuplicateAgentID
	}
	r.byID[e.AgentID] = e
	return nil
}

// Get implements auth.Roster.
func (r *stubRoster) Get(agentID string) (auth.RosterEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[agentID]
	return e, ok
}

// Len implements auth.Roster.
func (r *stubRoster) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// List implements auth.Roster. Like Put, it hands back exactly what it was
// given — no copying and no validation — so a test can still observe the
// corrupt state this double exists to produce.
func (r *stubRoster) List() []auth.RosterEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]auth.RosterEntry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}
