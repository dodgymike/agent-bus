package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingDoer fails the test if it is ever called — used to prove a code
// path that must fail LOCALLY never reaches the network.
type countingDoer struct{ calls int32 }

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&d.calls, 1)
	return nil, errors.New("countingDoer: unexpected HTTP request")
}

// flakyDoer fails the first `fail` calls with a network error, then forwards
// to inner. It records the decoded JSON body of every call it sees, so a test
// can compare what was sent on a failed attempt against what was sent on a
// later, successful one.
type flakyDoer struct {
	fail  int
	inner HTTPDoer

	mu     chan struct{} // 1-buffered mutex; avoids importing sync just for this
	calls  int
	bodies []map[string]interface{}
}

func newFlakyDoer(fail int, inner HTTPDoer) *flakyDoer {
	d := &flakyDoer{fail: fail, inner: inner, mu: make(chan struct{}, 1)}
	d.mu <- struct{}{}
	return d
}

func (d *flakyDoer) lock()   { <-d.mu }
func (d *flakyDoer) unlock() { d.mu <- struct{}{} }

func (d *flakyDoer) Do(req *http.Request) (*http.Response, error) {
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(raw))
	}
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)

	d.lock()
	d.calls++
	n := d.calls
	d.bodies = append(d.bodies, m)
	d.unlock()

	if n <= d.fail {
		return nil, errors.New("flakyDoer: simulated network failure")
	}
	return d.inner.Do(req)
}

// TestEnrolHappyPath checks the request the client actually sends — exactly
// the three documented fields, and a public key that decodes to 32 bytes —
// and that the server-minted result lands in a correctly permissioned store.
func TestEnrolHappyPath(t *testing.T) {
	type seen struct {
		body map[string]interface{}
	}
	var requests []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routeEnroll {
			http.NotFound(w, r)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("request body is not JSON: %v (%q)", err, b)
		}
		requests = append(requests, seen{body: m})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID:    "bus-test01.myagent-1",
			BusID:      "bus-test01",
			Name:       "myagent",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := c.Enrol(context.Background(), EnrolOptions{Name: "myagent", Save: true, MakeCurrent: true})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if res.AgentID != "bus-test01.myagent-1" {
		t.Fatalf("AgentID = %q, want the SERVER-MINTED id", res.AgentID)
	}
	if !res.Stored {
		t.Fatalf("Stored = false, want true")
	}

	if len(requests) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(requests))
	}
	body := requests[0].body
	if len(body) != 3 {
		t.Fatalf("request body has %d keys, want exactly 3 (name, public_key, idempotency_key): %v", len(body), body)
	}
	for _, k := range []string{"name", "public_key", "idempotency_key"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("request body missing key %q: %v", k, body)
		}
	}
	pubStr, _ := body["public_key"].(string)
	pub, err := base64.StdEncoding.DecodeString(pubStr)
	if err != nil {
		t.Fatalf("public_key is not standard base64: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("public_key decodes to %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat store dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != storeDirMode {
		t.Fatalf("store directory mode = %#o, want %#o", perm, storeDirMode)
	}
	fileInfo, err := os.Stat(c.Store().Path())
	if err != nil {
		t.Fatalf("stat store file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != storeFileMode {
		t.Fatalf("store file mode = %#o, want %#o", perm, storeFileMode)
	}
}

// TestEnrolReplayedHeader checks the client surfaces Replayed when the bus
// sets the Idempotency-Replayed header.
func TestEnrolReplayedHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(idempotencyReplayedHeader, "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	c, err := New(Config{BusURL: srv.URL, IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if !res.Replayed {
		t.Fatalf("Replayed = false, want true when the bus sets %s: true", idempotencyReplayedHeader)
	}
}

// TestEnrolLocalReplayMakesNoRequest checks re-running Enrol with the SAME
// explicit idempotency key and the same name is answered entirely from the
// local store — the second call must not touch the network at all.
func TestEnrolLocalReplayMakesNoRequest(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	c, err := New(Config{BusURL: srv.URL, IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := EnrolOptions{Name: "a", Save: true, IdempotencyKey: "fixed-key-1"}

	res1, err := c.Enrol(context.Background(), opts)
	if err != nil {
		t.Fatalf("first Enrol: %v", err)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests after the first Enrol, want 1", got)
	}

	res2, err := c.Enrol(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Enrol (retry): %v", err)
	}
	if !res2.Replayed {
		t.Fatalf("second Enrol Replayed = false, want true (the local already-applied path)")
	}
	if res2.AgentID != res1.AgentID {
		t.Fatalf("second Enrol AgentID = %q, want the same as the first (%q)", res2.AgentID, res1.AgentID)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests after the retry, want still 1 — a local replay must send nothing", got)
	}
}

// TestEnrolIdempotencyKeyReusedWithDifferentNameFails checks reusing an
// already-applied key for a DIFFERENT name is refused LOCALLY (KindUsage) and
// never reaches the bus, which would otherwise disconnect the client
// (invariant 10).
func TestEnrolIdempotencyKeyReusedWithDifferentNameFails(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	c, err := New(Config{BusURL: srv.URL, IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := EnrolOptions{Name: "a", Save: true, IdempotencyKey: "fixed-key-2"}
	if _, err := c.Enrol(context.Background(), opts); err != nil {
		t.Fatalf("first Enrol: %v", err)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests after the first Enrol, want 1", got)
	}

	opts2 := opts
	opts2.Name = "b"
	_, err = c.Enrol(context.Background(), opts2)
	if err == nil {
		t.Fatalf("Enrol with a reused key and a different name = nil error, want one")
	}
	if KindOf(err) != KindUsage {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindUsage)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests after the conflicting retry, want still 1 — it must be refused locally", got)
	}
	// IDEM-14-FU-CLIENTTEXT applies here too (idempotencyConflict, the LOCAL
	// refusal, not just annotateIdempotencyConflict's bus-side 409 wording):
	// no false "disconnects the client" claim, on a call that in fact never
	// even reached the bus.
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if strings.Contains(e.Remedy, "disconnect") {
		t.Fatalf("Remedy = %q claims a disconnect on a request that was refused LOCALLY and never reached the bus", e.Remedy)
	}
	if !strings.Contains(e.Remedy, "rejects and logs") {
		t.Fatalf("Remedy = %q, want it to say the bus rejects and logs this rather than merely omitting the old disconnect claim", e.Remedy)
	}
}

// TestEnrolSameKeyDifferentBusIsFresh checks the idempotency scope is (key,
// bus URL): presenting the same key to a DIFFERENT bus is a fresh enrolment,
// not a conflict, and therefore DOES send a request.
func TestEnrolSameKeyDifferentBusIsFresh(t *testing.T) {
	var count1, count2 int32
	countingHandler := func(counter *int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(counter, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(enrolResponseBody{
				AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
				EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
	}
	srv1 := httptest.NewServer(countingHandler(&count1))
	defer srv1.Close()
	srv2 := httptest.NewServer(countingHandler(&count2))
	defer srv2.Close()

	dir := t.TempDir()
	const key = "shared-key"

	c1, err := New(Config{BusURL: srv1.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New(c1): %v", err)
	}
	if _, err := c1.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true, IdempotencyKey: key}); err != nil {
		t.Fatalf("Enrol against srv1: %v", err)
	}
	if got := atomic.LoadInt32(&count1); got != 1 {
		t.Fatalf("srv1 saw %d requests, want 1", got)
	}

	c2, err := New(Config{BusURL: srv2.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New(c2): %v", err)
	}
	if _, err := c2.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true, IdempotencyKey: key}); err != nil {
		t.Fatalf("Enrol against srv2 with the same key: %v", err)
	}
	if got := atomic.LoadInt32(&count2); got != 1 {
		t.Fatalf("srv2 saw %d requests, want 1 — the same key against a different bus must be a fresh enrolment", got)
	}
}

// TestEnrolNetworkFailureLeavesPendingAndRetryReusesKey checks the durability
// property enrol.go documents: a network failure must not lose the generated
// key pair, and a later retry with the same idempotency key must resend the
// SAME public key rather than generating a new one (which the bus would treat
// as a protocol violation under invariant 10).
func TestEnrolNetworkFailureLeavesPendingAndRetryReusesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	doer := newFlakyDoer(1, srv.Client())
	dir := t.TempDir()
	c, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: dir,
		HTTPClient:  doer,
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := EnrolOptions{Name: "a", Save: true, IdempotencyKey: "resume-key"}

	_, err = c.Enrol(context.Background(), opts)
	if err == nil {
		t.Fatalf("first Enrol over a flaky transport = nil error, want a network failure")
	}
	if KindOf(err) != KindNetwork {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindNetwork)
	}

	pending, found, perr := c.Store().FindPending("resume-key", srv.URL)
	if perr != nil {
		t.Fatalf("FindPending: %v", perr)
	}
	if !found {
		t.Fatalf("no pending record survived the network failure; the key material would be lost")
	}
	if pending.PrivateKeySeed == "" {
		t.Fatalf("pending record has no private key seed")
	}

	res, err := c.Enrol(context.Background(), opts)
	if err != nil {
		t.Fatalf("retry Enrol: %v", err)
	}
	if res.AgentID == "" {
		t.Fatalf("retry Enrol succeeded with no agent id")
	}

	doer.lock()
	defer doer.unlock()
	if len(doer.bodies) < 2 {
		t.Fatalf("flaky transport saw %d attempts, want at least 2", len(doer.bodies))
	}
	firstPub, _ := doer.bodies[0]["public_key"].(string)
	lastPub, _ := doer.bodies[len(doer.bodies)-1]["public_key"].(string)
	if firstPub == "" || lastPub == "" {
		t.Fatalf("missing public_key in a captured request body: first=%q last=%q", firstPub, lastPub)
	}
	if firstPub != lastPub {
		t.Fatalf("retry used a DIFFERENT public key: first attempt=%s, later attempt=%s", firstPub, lastPub)
	}
}

// TestEnrolInviteFailsFastLocally checks Invite fails immediately with
// KindUsage and never reaches the network — the wire shape is not settled yet
// (ENROL-SHAPE).
func TestEnrolInviteFailsFastLocally(t *testing.T) {
	doer := &countingDoer{}
	c, err := New(Config{IdentityDir: t.TempDir(), HTTPClient: doer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "a", Invite: "x", Save: true})
	if err == nil {
		t.Fatalf("Enrol with --invite = nil error, want one")
	}
	if KindOf(err) != KindUsage {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindUsage)
	}
	if got := atomic.LoadInt32(&doer.calls); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 (Invite must fail before any request)", got)
	}
}

// TestEnrolSameKeyCaseInsensitiveHostIsSameScope checks canonicalisation
// OBSERVABLY: two spellings of the same bus that differ only in host case
// ("LOCALHOST" vs "localhost") must resolve to the SAME idempotency scope. If
// they did not, a retry spelled differently would miss its own pending
// record, mint a fresh key pair, and resend the same idempotency key with a
// different payload — a protocol violation under invariant 10.
func TestEnrolSameKeyCaseInsensitiveHostIsSameScope(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing httptest URL: %v", err)
	}
	// srv.URL is host "127.0.0.1:port" — swap in "localhost" (loopback, so
	// http is still permitted) so the two spellings genuinely differ in case.
	lower := "http://localhost:" + u.Port()
	upper := "http://LOCALHOST:" + u.Port()

	dir := t.TempDir()
	const key = "case-scope-key"

	c1, err := New(Config{BusURL: upper, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New(c1): %v", err)
	}
	res1, err := c1.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("first Enrol (uppercase host): %v", err)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests after the first Enrol, want 1", got)
	}

	c2, err := New(Config{BusURL: lower, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New(c2): %v", err)
	}
	res2, err := c2.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("second Enrol (lowercase host): %v", err)
	}
	if !res2.Replayed {
		t.Fatalf("second Enrol (different host case) Replayed = false, want true — canonicalisation must make this the SAME scope as the first")
	}
	if res2.AgentID != res1.AgentID {
		t.Fatalf("second Enrol AgentID = %q, want the same as the first (%q)", res2.AgentID, res1.AgentID)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server saw %d requests after the case-differing retry, want still 1 — the two spellings must canonicalise to one scope", got)
	}
}

// TestEnrolDoesNotFollowRedirect checks the transport-level rule (invariant
// 11): a bus that answers with a redirect must NOT be followed, because Go's
// default redirect policy would copy the Authorization header across a
// same-port https->http downgrade. Enrol carries no bearer yet, but this
// proves the mechanism at the layer every authenticated call shares.
func TestEnrolDoesNotFollowRedirect(t *testing.T) {
	var targetCount int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.a-1", BusID: "bus-x", Name: "a",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+routeEnroll, http.StatusFound)
	}))
	defer redirector.Close()

	c, err := New(Config{
		BusURL:      redirector.URL,
		IdentityDir: t.TempDir(),
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: false})
	if err == nil {
		t.Fatalf("Enrol against a redirecting bus = nil error, want the 302 surfaced as a failure")
	}
	if got := atomic.LoadInt32(&targetCount); got != 0 {
		t.Fatalf("the redirect target saw %d requests, want 0 — the client must never follow a redirect", got)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if e.Status != http.StatusFound {
		t.Fatalf("error Status = %d, want %d (the 302 itself, not whatever the redirect target would have answered)", e.Status, http.StatusFound)
	}
}

// TestEnrolConcurrentSameIdempotencyKeyUsesOneKeyPair drives N goroutines
// calling Client.Enrol with the SAME explicit idempotency key against one
// httptest server. This is the race ClaimEnrolment's insert-if-absent closes:
// under the OLD check-then-act AddPending, two concurrent callers could each
// see "no existing record", each generate a DIFFERENT key pair, and each POST
// the same idempotency key with a different public_key — a protocol
// violation (invariant 10) that would also silently drop one of the two
// private keys from the store.
func TestEnrolConcurrentSameIdempotencyKeyUsesOneKeyPair(t *testing.T) {
	var (
		mu          sync.Mutex
		requestPubs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body enrolRequestBody
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requestPubs = append(requestPubs, body.PublicKey)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID: "bus-x.concurrent-1", BusID: "bus-x", Name: "concurrent",
			EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	const n = 20
	const key = "concurrent-enrol-key"

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
			if err != nil {
				errs[i] = err
				return
			}
			_, err = c.Enrol(context.Background(), EnrolOptions{Name: "concurrent", Save: true, IdempotencyKey: key})
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Enrol: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requestPubs) == 0 {
		t.Fatalf("the server saw 0 requests; this proof is vacuous")
	}
	first := requestPubs[0]
	for i, pub := range requestPubs {
		if pub != first {
			t.Fatalf("request %d carried public_key %q, want the SAME as request 0 (%q) — a concurrent enrol generated a second key pair for one idempotency key", i, pub, first)
		}
	}

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	creds, _, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("store has %d credentials after %d concurrent Enrol calls with one idempotency key, want exactly 1: %+v", len(creds), n, creds)
	}
}

// TestEnrolUppercaseNameNamesLowercaseRemedy checks the client's local name
// validation catches the single most likely mistake — uppercase — and its
// Remedy names the fix rather than restating the rule abstractly.
func TestEnrolUppercaseNameNamesLowercaseRemedy(t *testing.T) {
	doer := &countingDoer{}
	c, err := New(Config{IdentityDir: t.TempDir(), HTTPClient: doer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "MyAgent", Save: true})
	if err == nil {
		t.Fatalf("Enrol with an uppercase name = nil error, want one")
	}
	if KindOf(err) != KindUsage {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindUsage)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if !strings.Contains(e.Remedy, "myagent") {
		t.Fatalf("Remedy = %q, want it to name the lowercase form %q", e.Remedy, "myagent")
	}
	if got := atomic.LoadInt32(&doer.calls); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 (name validation must fail before any request)", got)
	}
}

// TestEnrolFailedComposesRemedyAndStampsKey mirrors
// TestWriteFailedComposesRemedyAndNeverTellsAFatalBusToRetry (messages_test.go)
// for enrolFailed, which was the whole point of 45b2e17a / 799aea40: the two
// near-duplicate reports of the same three defects (enrolFailed REPLACED the
// remedy instead of composing it, ignored e.fatal, and never stamped
// Error.IdempotencyKey) are fixed together here, on the same shape writeFailed
// already used.
//
// It calls enrolFailed directly and builds every *Error through statusError /
// networkError, for the same reason the messages.go table does: what is under
// test is the REAL classification, not a fixture that agrees with the test by
// construction.
func TestEnrolFailedComposesRemedyAndStampsKey(t *testing.T) {
	const idemKey = "enrol-fail-key-1"
	const busURL = "https://bus.example"

	fromStatus := func(op string, status int, retryAfter, detail string) error {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		resp := &http.Response{StatusCode: status, Header: h}
		return statusError(op, routeEnroll, resp, []byte(`{"error":`+strconvQuote(detail)+`}`))
	}

	cases := []struct {
		name  string
		save  bool
		build func() error
		// wantKind is the Kind after enrolFailed. Closed vocabulary, same
		// reasoning as the writeFailed table.
		wantKind  Kind
		wantFatal bool
		// wantRemedyKeeps are substrings of the ORIGINAL remedy that must
		// survive composition.
		wantRemedyKeeps []string
		// wantRemedyAdds are substrings the appended clause must contribute.
		wantRemedyAdds []string
		// wantRemedyLacks are substrings that must NOT appear.
		wantRemedyLacks []string
		// wantRemedyUnchanged asserts the remedy is byte-for-byte unchanged —
		// no clause at all (the default/KindRejected branch).
		wantRemedyUnchanged bool
		// wantPendingDropped asserts DropPending ran: no pending record
		// survives for this (key, busURL) after enrolFailed returns.
		wantPendingDropped bool
	}{
		{
			name: "fatal 503 keeps its diagnosis and is told NOT to retry",
			save: true,
			build: func() error {
				return fromStatus("enrol", http.StatusServiceUnavailable, "", "the write path is poisoned")
			},
			wantKind:  KindServer,
			wantFatal: true,
			wantRemedyKeeps: []string{
				fatalRemedyFragment,
				"invariant 4",
			},
			wantRemedyAdds: []string{
				"this enrol may or may not have been applied",
				"do NOT retry until the bus can durably accept again",
				"--idempotency-key " + idemKey,
				"invariant 10",
			},
		},
		{
			name: "network failure keeps `check --bus` and gains the retry clause",
			save: true,
			build: func() error {
				return networkError("enrol", busURL, errors.New("dial tcp: connection refused"))
			},
			wantKind:  KindNetwork,
			wantFatal: false,
			wantRemedyKeeps: []string{
				networkRemedyFragment,
			},
			wantRemedyAdds: []string{
				"this enrol may or may not have been applied",
				"--idempotency-key " + idemKey,
				"the SAME enrolment rather than a second one",
				"invariant 10",
			},
			wantRemedyLacks: []string{"do NOT retry"},
		},
		{
			name: "network failure with Save=false still stamps the key but composes nothing",
			save: false,
			build: func() error {
				return networkError("enrol", busURL, errors.New("dial tcp: connection refused"))
			},
			wantKind:            KindNetwork,
			wantFatal:           false,
			wantRemedyUnchanged: true,
		},
		{
			name: "404 (unknown route / version skew) is refused for good and drops the pending record",
			save: true,
			build: func() error {
				return fromStatus("enrol", http.StatusNotFound, "", "no such route")
			},
			wantKind:            KindVersionSkew,
			wantFatal:           false,
			wantRemedyUnchanged: true,
			wantPendingDropped:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c, err := New(Config{IdentityDir: dir})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.Store().ClaimEnrolment(pendingEnrolment{
				IdempotencyKey: idemKey,
				Name:           "a",
				BusURL:         busURL,
				PublicKey:      "cHViLWtleQ==",
				PrivateKeySeed: "c2VlZC1ieXRlcy1oZXJlLWZvci10ZXN0aW5nLW9ubHk=",
				CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
			}, time.Now()); err != nil {
				t.Fatalf("seeding a pending record: %v", err)
			}

			built := tc.build()
			var before *Error
			if !errors.As(built, &before) {
				t.Fatalf("the fixture is not a *client.Error, so this case proves nothing: %v", built)
			}
			beforeRemedy := before.Remedy

			got := c.enrolFailed("enrol", idemKey, busURL, EnrolOptions{Save: tc.save}, built)

			var e *Error
			if !errors.As(got, &e) {
				t.Fatalf("enrolFailed returned a non-*Error: %v", got)
			}
			if e.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q — annotating a failure must not move it between categories", e.Kind, tc.wantKind)
			}
			if got := IsFatalUnavailable(got); got != tc.wantFatal {
				t.Fatalf("IsFatalUnavailable = %v, want %v", got, tc.wantFatal)
			}
			if e.IdempotencyKey != idemKey {
				t.Fatalf("IdempotencyKey = %q, want %q — this was previously NEVER stamped on a failed enrol (799aea40)", e.IdempotencyKey, idemKey)
			}
			if got := IdempotencyKeyOf(got); got != idemKey {
				t.Fatalf("IdempotencyKeyOf = %q, want %q", got, idemKey)
			}

			if tc.wantRemedyUnchanged {
				if e.Remedy != beforeRemedy {
					t.Fatalf("the remedy was modified:\n  before: %q\n   after: %q", beforeRemedy, e.Remedy)
				}
			} else {
				if !strings.HasPrefix(e.Remedy, beforeRemedy) {
					t.Fatalf("the composed remedy does not START with the original diagnosis:\n  before: %q\n   after: %q\n"+
						"this was exactly the REPLACES-not-composes bug (45b2e17a): `check --bus` and similar must survive", beforeRemedy, e.Remedy)
				}
			}
			for _, want := range tc.wantRemedyKeeps {
				if !strings.Contains(e.Remedy, want) {
					t.Fatalf("the ORIGINAL diagnosis was destroyed: remedy no longer contains %q.\nremedy: %q", want, e.Remedy)
				}
			}
			for _, want := range tc.wantRemedyAdds {
				if !strings.Contains(e.Remedy, want) {
					t.Fatalf("remedy = %q, want it to contain the appended clause %q", e.Remedy, want)
				}
			}
			for _, unwanted := range tc.wantRemedyLacks {
				if strings.Contains(e.Remedy, unwanted) {
					t.Fatalf("remedy = %q, want it NOT to contain %q", e.Remedy, unwanted)
				}
			}

			_, found, ferr := c.Store().FindPending(idemKey, busURL)
			if ferr != nil {
				t.Fatalf("FindPending: %v", ferr)
			}
			if tc.wantPendingDropped && found {
				t.Fatalf("a pending record survives for a failure that will be refused identically forever — it is dead weight and should have been dropped")
			}
			if !tc.wantPendingDropped && !found {
				t.Fatalf("the pending record was dropped for an AMBIGUOUS failure — it may still have been applied, so the key material must be kept for a retry")
			}
		})
	}
}
