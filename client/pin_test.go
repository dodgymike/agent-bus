package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newSelfSignedBusCert mints a self-signed certificate of the shape a bus
// serves: an Ed25519 key, a 127.0.0.1 IP SAN, valid now.
//
// Tests mint REAL certificates rather than asking the client to skip
// verification — that is the point of the guard in guard_test.go, and it is
// also what makes these tests exercise the real handshake rather than a
// stubbed-out one.
//
// Every call produces a DIFFERENT certificate, which is what the negative tests
// need: httptest's own StartTLS certificate is a single shared value, so two
// httptest servers started the default way have the SAME fingerprint and could
// not tell a pin mismatch from a match.
func newSelfSignedBusCert(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generating a serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "agent-bus test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// tlsBus is an https server that can SWAP its certificate without changing its
// address — which is exactly the event a pin exists to detect, and which cannot
// be modelled by starting a second server on a second port.
type tlsBus struct {
	srv *httptest.Server

	// current is the certificate served to the next handshake.
	current atomic.Value // tls.Certificate
}

// newTLSBus starts an https bus serving cert, with handler.
func newTLSBus(t *testing.T, cert tls.Certificate, handler http.Handler) *tlsBus {
	t.Helper()
	b := &tlsBus{}
	b.current.Store(cert)
	srv := httptest.NewUnstartedServer(handler)
	// A refused handshake is the EXPECTED outcome of half these tests, and
	// net/http logs it. Discard it so a passing run is quiet and a real failure
	// is the only thing on stderr.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{
		// Certificates is set so httptest does not substitute its own shared
		// test certificate; GetConfigForClient is what actually decides, and
		// crypto/tls consults it first on every handshake.
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			served := b.current.Load().(tls.Certificate)
			return &tls.Config{
				Certificates: []tls.Certificate{served},
				NextProtos:   []string{"http/1.1"},
				MinVersion:   tls.VersionTLS12,
			}, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	b.srv = srv
	return b
}

func (b *tlsBus) URL() string { return b.srv.URL }

// serve swaps the certificate presented to subsequent handshakes.
func (b *tlsBus) serve(cert tls.Certificate) { b.current.Store(cert) }

// fingerprintOf is the fingerprint of a minted certificate's leaf.
func fingerprintOf(cert tls.Certificate) BusFingerprint {
	return busFingerprintOfDER(cert.Certificate[0])
}

// enrolHandler answers POST /v1/enroll like a bus that minted an id, and counts
// how many requests actually ARRIVED.
//
// The count is the load-bearing assertion of the negative tests: a refused
// handshake means the request never left this process, so a bus with a wrong
// certificate must see ZERO requests. "The call returned an error" alone would
// also be satisfied by a client that connected, sent the enrolment and then
// complained.
func enrolHandler(hits *int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID:    "bus-testbus.planner",
			BusID:      "bus-testbus",
			Name:       "planner",
			EnrolledAt: "2026-08-07T12:00:00Z",
		})
	})
}

// TestClientPinsBusFingerprintAtEnrol is the POSITIVE half: the pin the invite
// carried is honoured, the enrolment succeeds against the certificate it names,
// and the fingerprint is RECORDED with the identity so no later command has to
// be told again ("the trusted path must be the easy path", invariant 11).
//
// It also asserts the no-TOFU refusal, because the positive case alone would
// pass just as well on a client that ignored the pin entirely.
func TestClientPinsBusFingerprintAtEnrol(t *testing.T) {
	cert := newSelfSignedBusCert(t)
	var hits int32
	bus := newTLSBus(t, cert, enrolHandler(&hits))
	pin := fingerprintOf(cert)

	t.Run("the pinned certificate is accepted and stored", func(t *testing.T) {
		dir := t.TempDir()
		c, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true})
		if err != nil {
			t.Fatalf("Enrol over a pinned TLS bus: %v", err)
		}
		if res.AgentID != "bus-testbus.planner" {
			t.Errorf("agent id = %q, want the server-minted bus-testbus.planner", res.AgentID)
		}
		if got := res.BusFingerprints; len(got) != 1 || got[0] != pin.String() {
			t.Errorf("result records fingerprints %q, want exactly [%s]", got, pin)
		}
		if atomic.LoadInt32(&hits) != 1 {
			t.Errorf("the bus saw %d enrol requests, want 1", atomic.LoadInt32(&hits))
		}

		// Stored, not merely returned: the next command must find it without
		// the flag.
		ids, _, err := c.Identities()
		if err != nil {
			t.Fatalf("Identities: %v", err)
		}
		if len(ids) != 1 {
			t.Fatalf("stored %d identities, want 1", len(ids))
		}
		if got := ids[0].BusFingerprints; len(got) != 1 || got[0] != pin.String() {
			t.Errorf("stored identity pins %q, want exactly [%s] — without this, every later command would need --bus-fingerprint again", got, pin)
		}

		// And the stored pin is enough on its own: a fresh client with NO
		// --bus-fingerprint and NO --bus reaches the same bus and verifies it.
		c2, err := New(Config{IdentityDir: dir})
		if err != nil {
			t.Fatalf("New (second client): %v", err)
		}
		u, gotPins, err := c2.endpoint()
		if err != nil {
			t.Fatalf("resolving the endpoint from the stored identity alone: %v", err)
		}
		if u.String() != bus.URL() {
			t.Errorf("resolved bus %q, want %q", u.String(), bus.URL())
		}
		if !gotPins.Equal(NewBusPinSet(pin)) {
			t.Errorf("resolved accept-set %s, want exactly the stored %s", gotPins, pin)
		}
	})

	t.Run("an https bus with NO pin is refused: there is no trust-on-first-use", func(t *testing.T) {
		before := atomic.LoadInt32(&hits)
		c, err := New(Config{BusURL: bus.URL(), IdentityDir: t.TempDir()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true})
		if err == nil {
			t.Fatal("enrolled against an https bus with no pinned fingerprint; that is trust-on-first-use, which invariant 11 rules out by name")
		}
		if got := KindOf(err); got != KindConfig {
			t.Errorf("Kind = %q, want %q", got, KindConfig)
		}
		assertRemedyNames(t, err, "--bus-fingerprint")
		if got := atomic.LoadInt32(&hits); got != before {
			t.Errorf("the bus saw %d new requests; an unpinned https bus must never be contacted at all", got-before)
		}
	})

	t.Run("a pin on a PLAINTEXT url is refused rather than ignored", func(t *testing.T) {
		c, err := New(Config{BusURL: "http://127.0.0.1:8080", BusFingerprint: pin.String(), IdentityDir: t.TempDir()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true})
		if err == nil {
			t.Fatal("accepted a pinned fingerprint on a plaintext URL; there is no certificate there, so the caller would believe in a check that never runs")
		}
		if got := KindOf(err); got != KindUsage {
			t.Errorf("Kind = %q, want %q", got, KindUsage)
		}
	})

	t.Run("a malformed fingerprint fails at construction, naming the flag", func(t *testing.T) {
		for _, bad := range []string{
			strings.ToUpper(pin.String()),        // uppercase is rejected, not folded
			pin.String()[:63],                    // short
			pin.String() + "0",                   // long
			strings.Repeat("g", 64),              // not hex
			" " + pin.String(),                   // stray whitespace on a flag
			insertEvery(pin.String(), 2, ":")[:], // the colon-separated spelling
		} {
			if _, err := New(Config{BusURL: "https://127.0.0.1:8080", BusFingerprint: bad, IdentityDir: t.TempDir()}); err == nil {
				t.Errorf("New accepted the malformed fingerprint %q", bad)
			} else if KindOf(err) != KindUsage {
				t.Errorf("fingerprint %q gave Kind %q, want %q", bad, KindOf(err), KindUsage)
			}
		}
	})
}

// TestClientRefusesChangedBusFingerprint is the NEGATIVE half, and it is the
// one that means anything: a verifier that accepts everything passes every
// positive test ever written.
//
// The bus keeps its ADDRESS and changes its CERTIFICATE — the exact event that
// motivated doing this task before the TLS listener. Substituting a bus's key
// material on an established data directory makes it restart cleanly with a
// different fingerprint and no warning at all; key LOSS is loud, key
// SUBSTITUTION is silent, and this is the only place it becomes loud again.
func TestClientRefusesChangedBusFingerprint(t *testing.T) {
	original := newSelfSignedBusCert(t)
	imposter := newSelfSignedBusCert(t)
	if fingerprintOf(original).Equal(fingerprintOf(imposter)) {
		t.Fatal("two freshly minted certificates share a fingerprint; this test would prove nothing")
	}

	var hits int32
	bus := newTLSBus(t, original, enrolHandler(&hits))
	pin := fingerprintOf(original)

	dir := t.TempDir()
	c, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true}); err != nil {
		t.Fatalf("Enrol against the original certificate: %v", err)
	}
	enrolHits := atomic.LoadInt32(&hits)

	// The certificate under the same address changes. Nothing else does.
	bus.serve(imposter)

	// A FRESH client, so nothing is served from a connection pooled while the
	// old certificate was in force — this must fail on its own merits.
	c2, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (after the swap): %v", err)
	}
	_, err = c2.EnsureSession(context.Background())
	if err == nil {
		t.Fatal("the client accepted a DIFFERENT certificate from the pinned bus; the pin is not being checked")
	}

	if !errors.Is(err, ErrBusFingerprintMismatch) {
		t.Fatalf("error does not match ErrBusFingerprintMismatch, so a caller cannot branch on it: %#v (%v)", err, err)
	}
	if got := KindOf(err); got != KindNetwork {
		t.Errorf("Kind = %q, want %q (nothing was applied: the handshake never completed)", got, KindNetwork)
	}
	if got := ExitCode(err); got != ExitNetwork {
		t.Errorf("ExitCode = %d, want %d", got, ExitNetwork)
	}
	// Both fingerprints are reported. Without them an operator cannot tell a
	// rotation they can account for from an interception they cannot.
	for _, want := range []string{pin.String(), fingerprintOf(imposter).String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name the fingerprint %s: %v", want, err)
		}
	}
	assertRemedyNames(t, err, "out of band", "bus_cert_fingerprint", "logout", "enrol")

	// It is NOT retried: retrying a bus we have decided is the wrong bus burns
	// the caller's budget and delays the moment a human sees it.
	if isRetryable(err) {
		t.Error("a fingerprint mismatch is classified retryable; it will never succeed and must not be repeated")
	}

	// And nothing was sent. A handshake refused before the request is the
	// difference between a pin and a complaint.
	if got := atomic.LoadInt32(&hits); got != enrolHits {
		t.Errorf("the bus received %d request(s) after presenting the wrong certificate; it must receive none", got-enrolHits)
	}
}

// TestPinnedRemedyActuallyWorks FOLLOWS the remedy the mismatch error prints
// and asserts it reaches a working client.
//
// It exists because an earlier draft's remedy said "re-enrol against the new
// fingerprint" and omitted the logout — and the reviewer gate reproduced the
// dead end it led to: the stored identity still pinned the OLD certificate, so
// the enrol was refused by the flag-vs-store conflict rule. Both messages were
// individually correct and the sequence was impossible.
//
// A remedy that does not work is worse than no remedy at all. An operator who
// follows the instructions and hits a second refusal concludes the tool is
// broken, and then goes looking for the flag that turns the check off. So the
// remedy is a TESTED path, not a sentence.
func TestPinnedRemedyActuallyWorks(t *testing.T) {
	original := newSelfSignedBusCert(t)
	rotated := newSelfSignedBusCert(t)
	var hits int32
	bus := newTLSBus(t, original, enrolHandler(&hits))

	dir := t.TempDir()
	c, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(original).String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	// The bus rotates its certificate, keeping its address.
	bus.serve(rotated)

	stale, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (stale pin): %v", err)
	}
	if _, err := stale.EnsureSession(context.Background()); err == nil {
		t.Fatal("the rotated certificate was accepted against the old pin")
	}

	// Now do exactly what the remedy says, in the order it says it: logout,
	// then enrol with the confirmed new fingerprint.
	recovering, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (recovery): %v", err)
	}
	if _, err := recovering.Logout(res.AgentID); err != nil {
		t.Fatalf("step 1 of the remedy (`logout %s`) failed: %v", res.AgentID, err)
	}
	reenrolled, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(rotated).String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (re-enrol): %v", err)
	}
	out, err := reenrolled.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true})
	if err != nil {
		t.Fatalf("step 2 of the remedy (enrol with the new fingerprint) failed: %v — the printed remedy is a dead end", err)
	}
	if got := out.BusFingerprints; len(got) != 1 || got[0] != fingerprintOf(rotated).String() {
		t.Errorf("the recovered identity pins %q, want exactly the rotated [%s]", got, fingerprintOf(rotated))
	}

	// And the ENROL-WITHOUT-LOGOUT shortcut is still refused, which is why the
	// remedy has to name both steps.
	shortcutDir := t.TempDir()
	c2, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(original).String(), IdentityDir: shortcutDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bus.serve(original)
	if _, err := c2.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true}); err != nil {
		t.Fatalf("Enrol (shortcut setup): %v", err)
	}
	bus.serve(rotated)
	c3, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(rotated).String(), IdentityDir: shortcutDir})
	if err != nil {
		t.Fatalf("New (shortcut): %v", err)
	}
	if _, err := c3.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true}); err == nil {
		t.Fatal("enrolling with a new fingerprint over a stored conflicting pin succeeded; if this is ever allowed, the remedy text must change with it")
	}
}

// TestVerifyPinnedBusCertificateRejects drives the verifier directly, including
// the cases a live handshake cannot easily produce.
//
// The "accepts everything" failure mode has a specific shape — a callback that
// returns nil down some path without having compared anything — so every path
// is enumerated here rather than inferred from the two end-to-end tests.
func TestVerifyPinnedBusCertificateRejects(t *testing.T) {
	a := newSelfSignedBusCert(t)
	b := newSelfSignedBusCert(t)
	pinA := fingerprintOf(a)

	pinB := fingerprintOf(b)
	c := newSelfSignedBusCert(t)

	tests := []struct {
		name     string
		pins     BusPinSet
		rawCerts [][]byte
		wantErr  error
	}{
		{"the pinned certificate", NewBusPinSet(pinA), a.Certificate, nil},
		{"a different certificate", NewBusPinSet(pinA), b.Certificate, ErrBusFingerprintMismatch},
		{"no certificate at all", NewBusPinSet(pinA), nil, ErrBusPresentedNoCertificate},
		{"an empty leaf", NewBusPinSet(pinA), [][]byte{{}}, ErrBusPresentedNoCertificate},
		{"an EMPTY set never matches", BusPinSet{}, a.Certificate, ErrBusFingerprintMismatch},
		{"an empty set with no certificate", BusPinSet{}, nil, ErrBusFingerprintMismatch},
		{"a set built from only the zero fingerprint is empty and matches nothing", NewBusPinSet(BusFingerprint{}), a.Certificate, ErrBusFingerprintMismatch},
		// The rollover set: either member is accepted, and ONLY those two.
		{"the first member of a rollover set", NewBusPinSet(pinA, pinB), a.Certificate, nil},
		{"the second member of a rollover set", NewBusPinSet(pinA, pinB), b.Certificate, nil},
		{"a third certificate against a rollover set", NewBusPinSet(pinA, pinB), c.Certificate, ErrBusFingerprintMismatch},
		{
			// The pinned certificate presented as an ISSUER rather than as the
			// leaf. rawCerts[0] is the leaf by definition, and a bus that put
			// the pinned certificate deeper in a chain it invented is not the
			// pinned bus.
			"the pinned certificate below the leaf",
			NewBusPinSet(pinA),
			[][]byte{b.Certificate[0], a.Certificate[0]},
			ErrBusFingerprintMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyPinnedBusCertificate(tc.pins, tc.rawCerts)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("rejected the pinned certificate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ACCEPTED — this is the silent failure the whole task is about")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want one matching %v", err, tc.wantErr)
			}
		})
	}
}

// TestPinConflictBetweenFlagAndStoreIsRefused: when --bus-fingerprint and the
// stored identity name DIFFERENT certificates for the SAME bus, neither wins.
//
// Preferring the command line here would be the documented resolution order and
// the wrong answer: it is the exact move an operator is talked into making
// ("it stopped working, so I used the fingerprint the other end sent me"), and
// it would turn a detected substitution into a successful one.
func TestPinConflictBetweenFlagAndStoreIsRefused(t *testing.T) {
	cert := newSelfSignedBusCert(t)
	other := newSelfSignedBusCert(t)
	var hits int32
	bus := newTLSBus(t, cert, enrolHandler(&hits))
	pin := fingerprintOf(cert)

	dir := t.TempDir()
	c, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true}); err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	conflicted, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(other).String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (conflicting pin): %v", err)
	}
	if _, _, err := conflicted.endpoint(); err == nil {
		t.Fatal("a flag fingerprint silently overrode the stored one; a disagreement about which certificate a bus has must be surfaced, not resolved by precedence")
	} else {
		if KindOf(err) != KindUsage {
			t.Errorf("Kind = %q, want %q", KindOf(err), KindUsage)
		}
		assertRemedyNames(t, err, "out of band")
	}

	// Agreeing with the stored pin is not a conflict.
	agreeing, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: dir})
	if err != nil {
		t.Fatalf("New (agreeing pin): %v", err)
	}
	if _, got, err := agreeing.endpoint(); err != nil {
		t.Fatalf("repeating the stored fingerprint was refused: %v", err)
	} else if !got.Equal(NewBusPinSet(pin)) {
		t.Errorf("resolved accept-set %s, want %s", got, pin)
	}
}

// TestParseBusFingerprintRoundTrip pins the ONE textual form.
func TestParseBusFingerprintRoundTrip(t *testing.T) {
	cert := newSelfSignedBusCert(t)
	f := fingerprintOf(cert)
	parsed, err := ParseBusFingerprint(f.String())
	if err != nil {
		t.Fatalf("ParseBusFingerprint(%s): %v", f, err)
	}
	if !parsed.Equal(f) {
		t.Errorf("round trip changed the fingerprint: %s -> %s", f, parsed)
	}
	if f.IsZero() {
		t.Error("a real certificate's fingerprint reported IsZero")
	}
	if !(BusFingerprint{}).IsZero() {
		t.Error("the zero fingerprint does not report IsZero")
	}
	if (BusFingerprint{}).Equal(f) {
		t.Error("the zero fingerprint matched a real certificate; ABSENT must never mean ANY")
	}
}

// TestBusFingerprintEnvIsRead checks the environment carrier, including the
// trailing newline a shell pipeline leaves behind.
func TestBusFingerprintEnvIsRead(t *testing.T) {
	cert := newSelfSignedBusCert(t)
	pin := fingerprintOf(cert)
	cfg, err := Config{}.ApplyEnv(func(k string) (string, bool) {
		if k == EnvBusFingerprint {
			return pin.String() + "\n", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.BusFingerprint != pin.String() {
		t.Errorf("%s produced %q, want %q", EnvBusFingerprint, cfg.BusFingerprint, pin.String())
	}

	// An explicit value is not overridden by the environment.
	cfg, err = Config{BusFingerprint: pin.String()}.ApplyEnv(func(k string) (string, bool) {
		if k == EnvBusFingerprint {
			return strings.Repeat("a", 64), true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.BusFingerprint != pin.String() {
		t.Errorf("the environment overrode an explicit fingerprint: %q", cfg.BusFingerprint)
	}
}

// assertRemedyNames checks that a failure carries a Remedy naming each of want.
//
// A pin failure whose remedy is "check your configuration" is a pin failure an
// operator resolves by turning the check off. Invariant 7: errors name the
// remedy, not the stack.
func assertRemedyNames(t *testing.T, err error, want ...string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("not a *client.Error: %v", err)
	}
	if strings.TrimSpace(e.Remedy) == "" {
		t.Fatalf("no remedy on %v", err)
	}
	for _, w := range want {
		if !strings.Contains(strings.ToLower(e.Remedy), strings.ToLower(w)) {
			t.Errorf("remedy does not mention %q: %s", w, e.Remedy)
		}
	}
}

// insertEvery is a test helper that produces the colon-separated fingerprint
// spelling other tools print, which this client deliberately does not accept.
func insertEvery(s string, n int, sep string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%n == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(r)
	}
	return b.String()
}
