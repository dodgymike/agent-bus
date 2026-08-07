package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
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
	return newBusCertValidBetween(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

// newBusCertValidBetween mints the same shape of certificate over a CHOSEN
// validity window, which is what the expiry tests need.
//
// The window is the only thing that varies. Everything else — the Ed25519 key,
// the loopback SANs, the self-signature — is identical to a healthy bus
// certificate, so a test that rejects one of these rejects it for its DATES and
// for no other reason. A helper that also changed the key or the SANs would let
// a rejection be attributed to the wrong cause.
func newBusCertValidBetween(t *testing.T, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	return mintBusCert(t, notBefore, notAfter, nil)
}

// mintBusCert is the one place a test certificate is built. extra are additional
// X.509 extensions, which the unhandled-critical-extension test needs and
// nothing else uses.
func mintBusCert(t *testing.T, notBefore, notAfter time.Time, extra []pkix.Extension) tls.Certificate {
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
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
		ExtraExtensions:       extra,
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
			err := verifyPinnedBusCertificate(tc.pins, tc.rawCerts, time.Now())
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

// leafOf parses a minted certificate so a test can read the window as X.509
// ACTUALLY RECORDED IT.
//
// This matters for the boundary cases: ASN.1 stores times to the second, so the
// NotAfter that comes back is a truncation of the time.Time handed to the
// template. A boundary test written against the template value would be testing
// a moment that is not in the certificate.
func leafOf(t *testing.T, cert tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing the minted certificate: %v", err)
	}
	return leaf
}

// TestExpiredBusCertificateIsRejectedDespiteMatchingPin is the negative test
// that carries MTLS-EXPIRY, and the words "DESPITE MATCHING PIN" are the whole
// point.
//
// Before this task the client checked sha256-of-DER and nothing else, so a
// certificate whose NotAfter was a day in the past was pinned, accepted and
// enrolled against — demonstrated empirically by the MTLS-PIN security gate.
// DECISIONS.md chose the 365-day lifetime as a leak-containment bound, and a
// bound only the client can enforce, that the client does not enforce, is
// decoration.
//
// # Why a POSITIVE test would be worth almost nothing here
//
// The failure mode is silent in the exact way the pin callback's is: a
// verification path that returns nil still completes handshakes and still
// returns working connections. Every positive test in this file passes whether
// or not the validity window is checked at all. Only this test and its
// not-yet-valid twin can tell the two trees apart, which is why they are the
// registered proof command for the task.
//
// The assertions are layered deliberately: the connection is refused (the
// security property), the bus received NOTHING (refused at the handshake, not
// after the request went out), the error is distinguishable from a fingerprint
// mismatch (they demand opposite responses), and it is not retryable (a
// certificate does not un-expire).
func TestExpiredBusCertificateIsRejectedDespiteMatchingPin(t *testing.T) {
	cert := newBusCertValidBetween(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	pin := fingerprintOf(cert)
	var hits int32
	bus := newTLSBus(t, cert, enrolHandler(&hits))

	c, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Save:false, and the reason is a DEFECT this test found rather than a
	// preference. Client.enrolFailed OVERWRITES the Remedy of any KindNetwork
	// error with its idempotency-key hint whenever Save is set (enrol.go), so on
	// the saving path the certificate remedy — this one, and MTLS-PIN's mismatch
	// remedy too — never reaches the operator. That is in enrol.go, which this
	// task does not own; it is filed as a follow-up. The security property is
	// unaffected either way, and the Save:true path is exercised below.
	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "planner"})
	if err == nil {
		t.Fatal("ACCEPTED an EXPIRED bus certificate because its fingerprint matched the pin — the validity window is not being checked")
	}

	if !errors.Is(err, ErrBusCertificateExpired) {
		t.Fatalf("error does not match ErrBusCertificateExpired, so a caller cannot branch on it: %#v (%v)", err, err)
	}
	// It must NOT read as a substitution. A mismatch means "you may be talking
	// to the wrong bus"; this means "you are talking to the right bus and its
	// certificate is stale". Conflating them sends an operator hunting for an
	// attacker who is not there.
	if errors.Is(err, ErrBusFingerprintMismatch) {
		t.Errorf("an EXPIRED pinned certificate is reported as a fingerprint MISMATCH; these are different events with opposite remedies: %v", err)
	}
	if got := KindOf(err); got != KindNetwork {
		t.Errorf("Kind = %q, want %q (nothing was applied: the handshake never completed)", got, KindNetwork)
	}
	if got := ExitCode(err); got != ExitNetwork {
		t.Errorf("ExitCode = %d, want %d", got, ExitNetwork)
	}
	if !strings.Contains(err.Error(), pin.String()) {
		t.Errorf("the message does not name the certificate %s: %v", pin, err)
	}
	// The clock is named FIRST because a skewed local clock produces this exact
	// failure and no amount of re-pinning fixes it.
	assertRemedyNames(t, err, "clock", "pin add", "bus_cert_fingerprint")
	if isRetryable(err) {
		t.Error("an expired certificate is classified retryable; it will not un-expire, and retrying hides it behind what looks like a flaky connection")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("the bus received %d request(s) over an expired certificate; a refused handshake means the request never left this process", got)
	}

	// The SAVING path is refused too. Only the remedy differs there (see above);
	// nothing about the refusal, or about the request never being sent, does.
	if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true, MakeCurrent: true}); !errors.Is(err, ErrBusCertificateExpired) {
		t.Errorf("Enrol(Save:true) over an expired certificate: error = %v, want ErrBusCertificateExpired", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("the bus received %d request(s); it must receive none on either enrolment path", got)
	}

	// The boundary, driven through the verifier so the moment can be chosen
	// exactly. x509 owns the comparison; this pins WHICH SIDE of it we are on.
	leaf := leafOf(t, cert)
	pins := NewBusPinSet(pin)
	if err := verifyPinnedBusCertificate(pins, cert.Certificate, leaf.NotAfter); err != nil {
		t.Errorf("rejected at exactly NotAfter (%s); the window is inclusive of its final instant: %v", leaf.NotAfter.UTC().Format(time.RFC3339), err)
	}
	if err := verifyPinnedBusCertificate(pins, cert.Certificate, leaf.NotAfter.Add(time.Nanosecond)); !errors.Is(err, ErrBusCertificateExpired) {
		t.Errorf("one nanosecond after NotAfter, error = %v, want ErrBusCertificateExpired", err)
	}

	// ORDER: identity before validity. A certificate that is BOTH unpinned and
	// expired is reported as unpinned — the expiry of a stranger's certificate
	// is a detail, and leading with it would bury the substitution.
	other := newSelfSignedBusCert(t)
	err = verifyPinnedBusCertificate(NewBusPinSet(fingerprintOf(other)), cert.Certificate, time.Now())
	if !errors.Is(err, ErrBusFingerprintMismatch) {
		t.Errorf("an unpinned AND expired certificate reports %v; it must report the fingerprint mismatch, which is the more serious of the two", err)
	}
}

// TestNotYetValidBusCertificateIsRejected is the other end of the window, and it
// is not a symmetry exercise.
//
// A check written as "reject if now is past NotAfter" passes the expiry test
// above completely and accepts a certificate dated ten years into the future —
// which is what an attacker with a stolen key and a machine whose clock they can
// influence would present, and what a freshly generated certificate looks like
// on a client whose clock is behind. Both ends are enforced because crypto/x509
// enforces both ends; that is the argument for handing the decision to it rather
// than writing the comparison here.
func TestNotYetValidBusCertificateIsRejected(t *testing.T) {
	cert := newBusCertValidBetween(t, time.Now().Add(time.Hour), time.Now().Add(48*time.Hour))
	pin := fingerprintOf(cert)
	var hits int32
	bus := newTLSBus(t, cert, enrolHandler(&hits))

	c, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Save:false so the remedy survives to be asserted — see the note in
	// TestExpiredBusCertificateIsRejectedDespiteMatchingPin.
	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "planner"})
	if err == nil {
		t.Fatal("ACCEPTED a NOT-YET-VALID bus certificate because its fingerprint matched the pin")
	}

	if !errors.Is(err, ErrBusCertificateExpired) {
		t.Fatalf("error does not match ErrBusCertificateExpired: %#v (%v)", err, err)
	}
	if got := KindOf(err); got != KindNetwork {
		t.Errorf("Kind = %q, want %q", got, KindNetwork)
	}
	// The message must name the END OF THE WINDOW THAT WAS CROSSED. Telling an
	// operator a certificate "expired" when it has not started yet is a wrong
	// answer that survives every other assertion here.
	if !strings.Contains(err.Error(), "NOT VALID UNTIL") {
		t.Errorf("a not-yet-valid certificate is not described as such: %v", err)
	}
	if strings.Contains(err.Error(), "EXPIRED at") {
		t.Errorf("a not-yet-valid certificate is described as EXPIRED: %v", err)
	}
	assertRemedyNames(t, err, "clock")
	if isRetryable(err) {
		t.Error("a not-yet-valid certificate is classified retryable")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("the bus received %d request(s) over a not-yet-valid certificate; it must receive none", got)
	}

	leaf := leafOf(t, cert)
	pins := NewBusPinSet(pin)
	if err := verifyPinnedBusCertificate(pins, cert.Certificate, leaf.NotBefore); err != nil {
		t.Errorf("rejected at exactly NotBefore (%s); the window is inclusive of its first instant: %v", leaf.NotBefore.UTC().Format(time.RFC3339), err)
	}
	if err := verifyPinnedBusCertificate(pins, cert.Certificate, leaf.NotBefore.Add(-time.Nanosecond)); !errors.Is(err, ErrBusCertificateExpired) {
		t.Errorf("one nanosecond before NotBefore, error = %v, want ErrBusCertificateExpired", err)
	}

	// A healthy certificate still passes. Without this the two negative tests
	// above would also be satisfied by a verifier that rejects EVERYTHING —
	// which would be secure, useless, and would not be caught until the first
	// real bus refused to talk to anybody.
	healthy := newSelfSignedBusCert(t)
	if err := verifyPinnedBusCertificate(NewBusPinSet(fingerprintOf(healthy)), healthy.Certificate, time.Now()); err != nil {
		t.Fatalf("an in-date pinned certificate was rejected: %v", err)
	}
}

// TestUnrecognisedCertificateDefectFailsClosed covers the CATCH-ALL arm of
// checkBusCertificateValidity — the one that must never be the arm that lets
// something through.
//
// The reviewer gate flagged this arm as live-reachable and asserted only in
// prose. It is reachable: crypto/tls PARSES the peer chain but, with default
// verification replaced, never calls Verify itself, so a certificate carrying a
// critical extension Go does not understand arrives here intact. It is in date
// and its fingerprint matches, so the only thing standing between it and an
// accepted connection is the default arm returning non-nil.
//
// A default arm that returns nil accepts everything the author did not think
// of, which is the same silent-accept shape as a verification callback that
// returns nil — and it would be invisible, because the certificate is otherwise
// perfectly healthy.
func TestUnrecognisedCertificateDefectFailsClosed(t *testing.T) {
	cert := mintBusCert(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), []pkix.Extension{{
		// A private-arc OID nothing implements, marked CRITICAL — which is
		// precisely the instruction "refuse this certificate if you do not
		// understand this extension".
		Id:       asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 9, 9},
		Critical: true,
		Value:    []byte{0x05, 0x00}, // ASN.1 NULL
	}})
	pin := fingerprintOf(cert)

	err := verifyPinnedBusCertificate(NewBusPinSet(pin), cert.Certificate, time.Now())
	if err == nil {
		t.Fatal("ACCEPTED a certificate carrying an unhandled CRITICAL extension; the catch-all arm is not failing closed")
	}
	if !errors.Is(err, ErrBusCertificateUnusable) {
		t.Fatalf("error = %v, want ErrBusCertificateUnusable", err)
	}
	// It is IN DATE, so it must not be mislabelled as an expiry. "Expired" stays
	// a precise claim; that is the whole reason there are two sentinels.
	if errors.Is(err, ErrBusCertificateExpired) {
		t.Errorf("an in-date certificate refused for a non-date reason reports as EXPIRED: %v", err)
	}
	// And it must be classified as a certificate problem, so it reaches
	// pinError's message rather than a generic "cannot reach the bus", and is
	// never retried.
	if !isPinError(err) {
		t.Error("not classified as a pin error; it would be reported as a transient network fault AND retried")
	}

	// End to end, over a real handshake, because that is the path the reviewer
	// showed is reachable.
	var hits int32
	bus := newTLSBus(t, cert, enrolHandler(&hits))
	c, err := New(Config{BusURL: bus.URL(), BusFingerprint: pin.String(), IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner"}); !errors.Is(err, ErrBusCertificateUnusable) {
		t.Fatalf("Enrol over an undecipherable certificate: error = %v, want ErrBusCertificateUnusable", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("the bus received %d request(s); the handshake must be refused before anything is sent", got)
	}
}

// TestPinVerifierReadsTheClockOnEveryHandshake guards a DESIGNED property that
// nothing else tests: pinVerifier calls now() per handshake rather than
// capturing a time when the transport is built.
//
// The reviewer gate found that mutating the verifier to sample the clock once,
// at construction, reddened nothing — despite the property having a paragraph of
// rationale. The consequence of getting it wrong is slow and silent: a
// long-lived agent holds one transport for days, so a captured clock would go on
// approving a certificate that expired hours ago, and the longer the process ran
// the wronger it would get. Nothing would look broken.
//
// The test drives ONE verifier across a clock that crosses NotAfter between the
// two calls. A captured clock passes the first and, wrongly, the second.
func TestPinVerifierReadsTheClockOnEveryHandshake(t *testing.T) {
	cert := newSelfSignedBusCert(t)
	leaf := leafOf(t, cert)

	// Sequential, single goroutine: the verifier is invoked directly rather than
	// through a handshake, so there is nothing to race with.
	at := leaf.NotAfter
	verify := pinVerifier(NewBusPinSet(fingerprintOf(cert)), func() time.Time { return at })

	if err := verify(cert.Certificate, nil); err != nil {
		t.Fatalf("the first handshake, inside the validity window, was refused: %v", err)
	}
	at = leaf.NotAfter.Add(time.Nanosecond)
	if err := verify(cert.Certificate, nil); !errors.Is(err, ErrBusCertificateExpired) {
		t.Fatalf("the SAME verifier accepted the certificate after it expired: error = %v, want ErrBusCertificateExpired. "+
			"The clock is being captured once instead of read per handshake, so a long-running agent would keep trusting an expired certificate", err)
	}
}

// TestZeroClockIsRefusedRatherThanRepaired covers the guard at the top of
// checkBusCertificateValidity.
//
// It is here because the security gate pointed out that the guard was itself an
// untested fail-closed path — deleting the whole `if at.IsZero()` block reddened
// nothing — which is the same shape as the finding it was written to answer. A
// fail-closed arm nobody tests is a fail-closed arm until someone edits it.
//
// What it protects: x509 substitutes time.Now() for a zero CurrentTime, so the
// VERDICT would be right, but BusCertificateExpiredError.Error() would compare
// the zero At and name the WRONG END of the window — the gate observed an
// expired certificate reported as "NOT VALID UNTIL … it is now 0001-01-01".
func TestZeroClockIsRefusedRatherThanRepaired(t *testing.T) {
	healthy := newSelfSignedBusCert(t)
	err := verifyPinnedBusCertificate(NewBusPinSet(fingerprintOf(healthy)), healthy.Certificate, time.Time{})
	if err == nil {
		t.Fatal("ACCEPTED a certificate judged against the ZERO time; a caller with no clock has not judged anything")
	}
	if !errors.Is(err, ErrBusCertificateUnusable) {
		t.Errorf("error = %v, want ErrBusCertificateUnusable", err)
	}

	// The case that motivated refusing rather than letting x509 repair it: an
	// EXPIRED certificate must not come back described as not-yet-valid.
	expired := newBusCertValidBetween(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	err = verifyPinnedBusCertificate(NewBusPinSet(fingerprintOf(expired)), expired.Certificate, time.Time{})
	if err == nil {
		t.Fatal("ACCEPTED an expired certificate judged against the zero time")
	}
	if strings.Contains(err.Error(), "NOT VALID UNTIL") {
		t.Errorf("an EXPIRED certificate is described as not-yet-valid when the clock is the zero time: %v", err)
	}
}

// TestPinnedTLSConfigUsesALiveClock is the WIRING test, and it exists because
// every other clock assertion in this file stops one level too early.
//
// TestPinVerifierReadsTheClockOnEveryHandshake proves pinVerifier reads its
// clock per call — but it constructs the verifier itself, so it says nothing
// about what pinnedTLSConfig HANDS it. The security gate found that mutating
// that single line to capture a time at construction left the entire suite
// green. This is the test that fails.
//
// It has to let real time pass, because the seam it is testing is precisely the
// absence of an injectable clock on the production path. The window is short and
// the wait is a poll rather than a fixed sleep, so the cost is a few seconds and
// the outcome does not depend on how fast the machine is.
//
// # It RETRIES a stalled machine; it does NOT skip
//
// An earlier draft called t.Skipf when the window closed before the first check
// could run. The security gate showed that hole is reachable UNDER THE MUTATION
// rather than only on the honest path: with the clock frozen at construction and
// a multi-second stall, the first check fails, the skip condition holds, and a
// REAL REGRESSION reports green — and proof-check.sh reports VACUOUS only when
// EVERY test skipped, so it would have said PASS. A test that can silently stop
// proving anything is the same failure shape as the verifier that silently
// returns nil, which is the thing this whole task is about. So a stall re-mints
// and retries, and exhausting the attempts is a FAILURE, never a skip.
//
// margin cannot usefully go below ~2s: ASN.1 records NotAfter to the second, so
// a shorter window risks a certificate that is already expired when it is built.
func TestPinnedTLSConfigUsesALiveClock(t *testing.T) {
	const (
		margin   = 3 * time.Second
		attempts = 3
	)
	for attempt := 1; ; attempt++ {
		cert := newBusCertValidBetween(t, time.Now().Add(-time.Hour), time.Now().Add(margin))
		leaf := leafOf(t, cert)

		// The config is built ONCE, now, while the certificate is still valid.
		cfg := pinnedTLSConfig(NewBusPinSet(fingerprintOf(cert)))
		if cfg.VerifyPeerCertificate == nil {
			t.Fatal("pinnedTLSConfig produced a config with no verification callback")
		}

		if err := cfg.VerifyPeerCertificate(cert.Certificate, nil); err != nil {
			if attempt < attempts && time.Now().After(leaf.NotAfter) {
				// The machine stalled past the window between minting and
				// checking. Re-mint and try again rather than concluding
				// anything — but never more than `attempts` times, so a genuine
				// refusal cannot hide behind an infinite retry.
				continue
			}
			t.Fatalf("the pinned config refused an in-date certificate on attempt %d of %d: %v", attempt, attempts, err)
		}

		// Let the certificate expire UNDER the already-built config.
		for time.Now().Before(leaf.NotAfter.Add(50 * time.Millisecond)) {
			time.Sleep(10 * time.Millisecond)
		}

		if err := cfg.VerifyPeerCertificate(cert.Certificate, nil); !errors.Is(err, ErrBusCertificateExpired) {
			t.Fatalf("a config built BEFORE the certificate expired still accepts it: error = %v, want ErrBusCertificateExpired. "+
				"pinnedTLSConfig is handing pinVerifier a clock captured at construction instead of time.Now, so a long-lived agent would trust an expired certificate for as long as its process runs", err)
		}
		return
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
