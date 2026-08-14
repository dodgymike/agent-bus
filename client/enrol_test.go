package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// assertEnrolBodyFields asserts the enrol request body carries EXACTLY the
// named fields — no extras, none missing.
//
// It replaced a bare `len(body) != 3` plus a presence loop. The count was the
// reason RELAY-13's client half could not land inside its own file boundary:
// adding a field failed the count with "has 4 keys, want exactly 3", which
// names an arithmetic disagreement rather than the field that changed, and
// leaves the reader unable to tell an intended addition from an accidental one.
//
// Asserting the SET keeps the property that mattered — nothing undocumented
// reaches an UNAUTHENTICATED route, and no secret is smuggled onto the wire by
// a stray json tag on a struct that also holds seeds — while making the next
// addition fail by NAME. Extras and omissions are reported separately because
// they are different bugs: an extra field is something escaping, a missing one
// is something that stopped being sent.
func assertEnrolBodyFields(t *testing.T, body map[string]interface{}, want ...string) {
	t.Helper()
	expected := make(map[string]bool, len(want))
	for _, k := range want {
		expected[k] = true
	}
	var missing, unexpected []string
	for k := range expected {
		if _, ok := body[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range body {
		if !expected[k] {
			unexpected = append(unexpected, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)
	if len(missing) > 0 || len(unexpected) > 0 {
		t.Fatalf("enrol request body carries the wrong fields:\n  missing:    %v\n  unexpected: %v\n  want exactly: %v\n  got: %v",
			missing, unexpected, want, body)
	}
}

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
// the four documented fields, and an auth public key that decodes to 32 bytes
// — and that the server-minted result lands in a correctly permissioned store.
// The messaging key's own properties are pinned by
// TestEnrolRegistersMessagingPublicKey.
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
	assertEnrolBodyFields(t, body, "name", "public_key", "messaging_public_key", "idempotency_key")
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
		name string
		save bool
		// resumed marks the key material as belonging to an EARLIER attempt,
		// which is what forbids dropping it (INVITE-CLIENT-FU-PENDINGINVITE).
		resumed bool
		build   func() error
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
		{
			// The same permanent refusal, on a RESUMED attempt. The key
			// material belongs to an earlier request that may already have been
			// applied, so a refusal of THIS one is not evidence about it and
			// must not delete it.
			name: "a RESUMED attempt keeps its key material even on a permanent refusal",
			save: true, resumed: true,
			build: func() error {
				return fromStatus("enrol", http.StatusConflict, "", "idempotency key already used with a different payload")
			},
			wantKind:  KindRejected,
			wantFatal: false,
			wantRemedyKeeps: []string{
				"use a fresh key for new content",
			},
			wantRemedyAdds: []string{
				"--idempotency-key " + idemKey,
				"is KEPT and NOT dropped",
			},
			wantPendingDropped: false,
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

			got := c.enrolFailed("enrol", idemKey, busURL, EnrolOptions{Save: tc.save}, tc.resumed, built)

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

// idempotentEnrolBus is a stub /v1/enroll that enforces the SERVER's rule from
// invariant 10, which the plain 201-always stubs above do not: the same
// idempotency key with the SAME payload replays the original answer, and the
// same key with a DIFFERENT payload is a 409.
//
// That distinction is the whole point of the resumed-retry test. Against a stub
// that always answers 201, a client which regenerated its messaging key on
// retry would look perfectly healthy — the defect only shows up against a bus
// that compares payloads, which the real one does (internal/auth compares the
// messaging key too since RELAY-13).
type idempotentEnrolBus struct {
	mu       sync.Mutex
	applied  map[string]map[string]interface{} // idempotency key -> the payload it was applied with
	results  map[string]enrolResponseBody
	requests []map[string]interface{}
	minted   int
}

func newIdempotentEnrolBus() *idempotentEnrolBus {
	return &idempotentEnrolBus{
		applied: map[string]map[string]interface{}{},
		results: map[string]enrolResponseBody{},
	}
}

func (b *idempotentEnrolBus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != routeEnroll {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "not JSON", http.StatusBadRequest)
		return
	}
	key, _ := body["idempotency_key"].(string)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, body)

	if prev, seen := b.applied[key]; seen {
		if !reflect.DeepEqual(prev, body) {
			// Rejected and logged, connection preserved (invariant 10 as
			// narrowed 2026-08-08) — no disconnect.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"idempotency key reused with different content"}`))
			return
		}
		w.Header().Set(idempotencyReplayedHeader, "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(b.results[key])
		return
	}

	b.minted++
	name, _ := body["name"].(string)
	res := enrolResponseBody{
		AgentID:    fmt.Sprintf("bus-test01.%s-%d", name, b.minted),
		BusID:      "bus-test01",
		Name:       name,
		EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	b.applied[key] = body
	b.results[key] = res
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(res)
}

func (b *idempotentEnrolBus) seen() []map[string]interface{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]map[string]interface{}, len(b.requests))
	copy(out, b.requests)
	return out
}

// lossyDoer forwards the first `lose` calls to the bus and then THROWS AWAY the
// answer, reporting a network failure instead.
//
// It reproduces the one failure the idempotency machinery exists for and which
// flakyDoer does not: the request WAS applied and the acknowledgement was lost.
// flakyDoer fails before forwarding, so the bus never sees the first attempt and
// the retry is its first sight of the key — which cannot produce a 409 no matter
// what the client regenerates.
type lossyDoer struct {
	lose  int
	inner HTTPDoer

	mu    sync.Mutex
	calls int
}

func (d *lossyDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.inner.Do(req)
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	if n <= d.lose {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, errors.New("lossyDoer: the answer was lost in flight")
	}
	return resp, err
}

// TestEnrolRegistersMessagingPublicKey is the client half of RELAY-13.
//
// The bus attests `fq-agent-id -> messaging public key` to a peer bus, and it
// cannot attest a key it never recorded — so enrolment has to register one. This
// pins the three things that makes true: the key is on the wire, it is a second
// key rather than the auth key wearing a different name, and the PRIVATE half it
// was derived from is the one kept locally.
func TestEnrolRegistersMessagingPublicKey(t *testing.T) {
	bus := newIdempotentEnrolBus()
	srv := httptest.NewServer(bus)
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

	seen := bus.seen()
	if len(seen) != 1 {
		t.Fatalf("bus saw %d requests, want 1", len(seen))
	}
	body := seen[0]
	assertEnrolBodyFields(t, body, "name", "public_key", "messaging_public_key", "idempotency_key")

	msgPub, _ := body["messaging_public_key"].(string)
	if msgPub == "" {
		t.Fatalf("messaging_public_key is empty; the bus would have nothing to attest")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(msgPub)
	if err != nil {
		t.Fatalf("messaging_public_key is not standard, padded base64: %v", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		t.Fatalf("messaging_public_key decodes to %d bytes, want exactly %d — the bus refuses any other length",
			len(decoded), ed25519.PublicKeySize)
	}
	authPub, _ := body["public_key"].(string)
	if msgPub == authPub {
		t.Fatalf("messaging_public_key equals public_key; the bus refuses one key serving both roles, and the two must have independent lifetimes")
	}

	// The key the bus recorded must be the public half of the seed WE keep. If
	// these could drift, the bus would attest a key that verifies nothing this
	// agent signs — every peer rejecting every message, with a symptom nowhere
	// near the cause.
	cred, err := c.Store().Resolve(res.AgentID)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", res.AgentID, err)
	}
	if cred.MessagingKeySeed == "" {
		t.Fatalf("the stored credential has no messaging key seed; the private half of the key just registered was thrown away")
	}
	storedPub, err := cred.MessagingPublicKey()
	if err != nil {
		t.Fatalf("Credential.MessagingPublicKey: %v", err)
	}
	if storedPub != msgPub {
		t.Fatalf("the bus recorded messaging key %s but the credential holds the seed for %s", msgPub, storedPub)
	}

	// The SEED must never be on the wire. Check the whole serialised body, not
	// just the fields we know about: the point is to catch a field nobody
	// remembered to look for.
	rawBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-marshalling the captured body: %v", err)
	}
	if bytes.Contains(rawBody, []byte(cred.MessagingKeySeed)) {
		t.Fatalf("the messaging PRIVATE key seed appears in the enrol request: %s", rawBody)
	}
	if bytes.Contains(rawBody, []byte(cred.PrivateKeySeed)) {
		t.Fatalf("the auth PRIVATE key seed appears in the enrol request: %s", rawBody)
	}
}

// TestEnrolResumedRetryReusesTheMessagingKey is the test for the failure mode
// that makes the messaging seed part of the PENDING record rather than
// something minted at promotion time.
//
// The scenario is the ordinary one: the bus applied the enrolment and the
// acknowledgement was lost. The documented remedy is to re-run with the same
// --idempotency-key. That is only legitimate if the payload is byte-identical
// (invariant 10) — and the payload now contains the messaging public key. A
// client that regenerated it would present the same key with different content
// and be answered with a 409, turning the one correct response to an
// interrupted enrolment into a permanent failure.
//
// It is deliberately end to end rather than a field-comparison: the assertion
// that matters is that the RETRY SUCCEEDS against a bus which enforces the rule.
func TestEnrolResumedRetryReusesTheMessagingKey(t *testing.T) {
	bus := newIdempotentEnrolBus()
	srv := httptest.NewServer(bus)
	defer srv.Close()

	doer := &lossyDoer{lose: 1, inner: srv.Client()}
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
	opts := EnrolOptions{Name: "resumer", Save: true, IdempotencyKey: "resume-msgkey"}

	if _, err := c.Enrol(context.Background(), opts); err == nil {
		t.Fatalf("first Enrol = nil error, want the lost acknowledgement to surface as a network failure")
	} else if KindOf(err) != KindNetwork {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindNetwork)
	}

	pending, found, perr := c.Store().FindPending("resume-msgkey", srv.URL)
	if perr != nil {
		t.Fatalf("FindPending: %v", perr)
	}
	if !found {
		t.Fatalf("no pending record survived the lost acknowledgement")
	}
	if pending.MessagingKeySeed == "" {
		t.Fatalf("the pending record kept no messaging key seed, so the retry can only regenerate one and earn a 409")
	}

	// A SECOND client over the same store, because the realistic resume happens
	// in a new process: the first one was killed.
	c2, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: dir,
		HTTPClient:  srv.Client(),
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New (resuming process): %v", err)
	}
	res, err := c2.Enrol(context.Background(), opts)
	if err != nil {
		t.Fatalf("resumed Enrol: %v — a retry with the same key must recover the identity, not be refused", err)
	}
	if !res.Replayed {
		t.Fatalf("resumed Enrol Replayed = false, want true: the bus had already applied this enrolment")
	}

	seen := bus.seen()
	if len(seen) != 2 {
		t.Fatalf("bus saw %d requests, want 2 (the applied-but-unacknowledged one, then the retry)", len(seen))
	}
	if !reflect.DeepEqual(seen[0], seen[1]) {
		t.Fatalf("the retry payload differs from the original, which is a protocol violation rather than a retry:\n  first: %v\n  retry: %v", seen[0], seen[1])
	}
	first, _ := seen[0]["messaging_public_key"].(string)
	if first == "" {
		t.Fatalf("the original attempt carried no messaging_public_key, so this proves nothing about reusing it")
	}

	// And the credential finally stored holds the seed for the key the bus
	// recorded on the FIRST attempt — the one it will attest.
	cred, err := c2.Store().Resolve(res.AgentID)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", res.AgentID, err)
	}
	storedPub, err := cred.MessagingPublicKey()
	if err != nil {
		t.Fatalf("Credential.MessagingPublicKey: %v", err)
	}
	if storedPub != first {
		t.Fatalf("the bus recorded %s at enrolment but the stored credential holds the seed for %s", first, storedPub)
	}
}

// TestEnrolResumedPreUpgradePendingSendsNoMessagingKey checks the ONE path on
// which the client deliberately sends no messaging key.
//
// A pending record written before the messaging seed existed belongs to an
// attempt that registered no messaging key. Minting one for its retry would be
// "same key + DIFFERENT payload" — a 409 — so the retry must reproduce the old,
// three-field payload exactly. The resulting identity keeps a locally-minted
// messaging key the bus cannot attest, which is the pre-RELAY-13 state and is
// the correct outcome: recovering the identity beats registering a key.
func TestEnrolResumedPreUpgradePendingSendsNoMessagingKey(t *testing.T) {
	bus := newIdempotentEnrolBus()
	srv := httptest.NewServer(bus)
	defer srv.Close()

	dir := t.TempDir()
	// flakyDoer, not lossyDoer: this attempt must NOT reach the bus, so that the
	// bus's first sight of the key is the retry. It stands in for an enrolment
	// made by the previous build, whose pending record is all that survives.
	doer := newFlakyDoer(1, srv.Client())
	c, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: dir,
		HTTPClient:  doer,
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := EnrolOptions{Name: "oldbuild", Save: true, IdempotencyKey: "pre-upgrade-key"}
	if _, err := c.Enrol(context.Background(), opts); err == nil {
		t.Fatalf("first Enrol = nil error, want a network failure that leaves a pending record")
	}

	// Rewrite the pending record the way the previous build would have written
	// it: no messaging_key_seed at all.
	path := filepath.Join(dir, storeFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the store is not JSON: %v", err)
	}
	pendings, ok := doc["pending"].([]interface{})
	if !ok || len(pendings) != 1 {
		t.Fatalf("want exactly 1 pending record in the store, got %v", doc["pending"])
	}
	rec, ok := pendings[0].(map[string]interface{})
	if !ok {
		t.Fatalf("the pending record is not an object: %v", pendings[0])
	}
	if _, present := rec["messaging_key_seed"]; !present {
		t.Fatalf("the pending record has no messaging_key_seed to remove, so this test would pass vacuously")
	}
	delete(rec, "messaging_key_seed")
	rewritten, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshalling the store: %v", err)
	}
	if err := os.WriteFile(path, rewritten, storeFileMode); err != nil {
		t.Fatalf("rewriting the store: %v", err)
	}

	c2, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: dir,
		HTTPClient:  srv.Client(),
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New (resuming process): %v", err)
	}
	res, err := c2.Enrol(context.Background(), opts)
	if err != nil {
		t.Fatalf("resuming a pre-upgrade pending record: %v", err)
	}

	seen := bus.seen()
	if len(seen) != 1 {
		t.Fatalf("bus saw %d requests, want 1", len(seen))
	}
	assertEnrolBodyFields(t, seen[0], "name", "public_key", "idempotency_key")

	cred, err := c2.Store().Resolve(res.AgentID)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", res.AgentID, err)
	}
	if cred.MessagingKeySeed != "" {
		t.Fatalf("the promoted credential invented a messaging key the bus never recorded (%q); it must stay empty so first-use minting still applies",
			cred.MessagingKeySeed)
	}
}

// TestEnrolResumedDamagedMessagingSeedIsRefusedLocally covers the one new
// branch in Enrol that both gates found untested: a resumed attempt whose
// stored messaging seed cannot be read back.
//
// Two properties, and the second is the security-relevant one:
//
//   - It is refused LOCALLY, as KindConfig, and never reaches the bus. Sending
//     a key derived from unreadable material is not possible, and sending NONE
//     would change the payload of an attempt that had already sent one — a 409.
//   - The message carries NO fragment of the damaged material. It names the
//     idempotency key, which is charset-bounded caller input, and nothing else.
func TestEnrolResumedDamagedMessagingSeedIsRefusedLocally(t *testing.T) {
	bus := newIdempotentEnrolBus()
	srv := httptest.NewServer(bus)
	defer srv.Close()

	dir := t.TempDir()
	doer := newFlakyDoer(1, srv.Client())
	c, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: dir,
		HTTPClient:  doer,
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	opts := EnrolOptions{Name: "damaged", Save: true, IdempotencyKey: "damaged-msgseed-key"}
	if _, err := c.Enrol(context.Background(), opts); err == nil {
		t.Fatalf("first Enrol = nil error, want a network failure that leaves a pending record")
	}

	// Corrupt ONLY the messaging seed, the way a hand-edited or partially
	// restored store would. The auth seed stays valid, so a failure here is
	// unambiguously the new branch and not the pre-existing auth-seed one.
	// Deliberately distinctive AND unambiguously not base64, so that the
	// sliding-window leak check below cannot collide by accident with ordinary
	// English in the remedy text.
	const damaged = "!!!!DAMAGEDSEEDMATERIALMARKER!!!!"
	path := filepath.Join(dir, storeFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the store is not JSON: %v", err)
	}
	pendings, ok := doc["pending"].([]interface{})
	if !ok || len(pendings) != 1 {
		t.Fatalf("want exactly 1 pending record in the store, got %v", doc["pending"])
	}
	rec, ok := pendings[0].(map[string]interface{})
	if !ok {
		t.Fatalf("the pending record is not an object: %v", pendings[0])
	}
	if _, present := rec["messaging_key_seed"]; !present {
		t.Fatalf("the pending record has no messaging_key_seed to damage, so this test would pass vacuously")
	}
	rec["messaging_key_seed"] = damaged
	rewritten, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshalling the store: %v", err)
	}
	if err := os.WriteFile(path, rewritten, storeFileMode); err != nil {
		t.Fatalf("rewriting the store: %v", err)
	}

	c2, err := New(Config{
		BusURL:      srv.URL,
		IdentityDir: dir,
		HTTPClient:  srv.Client(),
		Retry:       RetryPolicy{Attempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New (resuming process): %v", err)
	}
	_, err = c2.Enrol(context.Background(), opts)
	if err == nil {
		t.Fatalf("resuming with a damaged messaging seed = nil error, want a refusal")
	}
	if KindOf(err) != KindConfig {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
	}
	if seen := bus.seen(); len(seen) != 0 {
		t.Fatalf("bus saw %d requests, want 0 — this must be refused before anything is sent: %v", len(seen), seen)
	}

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	rendered := e.Error() + " " + e.Remedy
	// Every window of the material, not just the whole string. The security
	// gate measured the weaker check: an error echoing only a PREFIX of the
	// damaged value — which is how key material usually leaks, truncated into a
	// "context" clause — slipped past `strings.Contains(rendered, damaged)`
	// while the test still reported the property as pinned.
	const leakWindow = 8
	for i := 0; i+leakWindow <= len(damaged); i++ {
		if frag := damaged[i : i+leakWindow]; strings.Contains(rendered, frag) {
			t.Fatalf("the error echoes %d bytes of the damaged key material (%q) back: %q", leakWindow, frag, rendered)
		}
	}
	if !strings.Contains(rendered, "damaged-msgseed-key") {
		// The idempotency key IS safe to name — it is charset-bounded caller
		// input and it is the handle that resumes the attempt. Asserting it
		// appears also proves the check above is not vacuous by matching an
		// empty string.
		t.Fatalf("the error = %q, want it to name the idempotency key so the attempt can be identified", rendered)
	}
}
