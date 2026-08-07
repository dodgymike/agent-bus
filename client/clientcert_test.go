package client

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newMutualTLSBus starts an https bus that ASKS for a client certificate
// according to clientAuth, and records what the last completed request
// presented.
//
// It is a separate helper from newTLSBus rather than a parameter on it because
// the two model different things: newTLSBus exists to swap the certificate the
// bus SERVES, this one to observe what the client SENDS. The server-side
// ClientAuth setting has to go in the config returned by GetConfigForClient,
// which is what crypto/tls actually uses for the handshake — setting it on the
// outer config alone is silently ignored, which would make every assertion here
// vacuous.
type mutualTLSBus struct {
	srv *httptest.Server

	mu   sync.Mutex
	peer []byte // DER of the leaf the last request presented, nil if none
	seen bool   // a request completed (as opposed to a refused handshake)
}

func newMutualTLSBus(t *testing.T, cert tls.Certificate, clientAuth tls.ClientAuthType, body http.HandlerFunc) *mutualTLSBus {
	t.Helper()
	b := &mutualTLSBus{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.seen = true
		b.peer = nil
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			b.peer = r.TLS.PeerCertificates[0].Raw
		}
		b.mu.Unlock()
		body(w, r)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				Certificates: []tls.Certificate{cert},
				NextProtos:   []string{"http/1.1"},
				MinVersion:   tls.VersionTLS12,
				ClientAuth:   clientAuth,
			}, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	b.srv = srv
	return b
}

func (b *mutualTLSBus) URL() string { return b.srv.URL }

// presented returns the DER of the client leaf the last request carried, and
// whether a request completed at all.
func (b *mutualTLSBus) presented() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peer, b.seen
}

// fingerprintOfDER is the fingerprint spelling used throughout this system:
// SHA-256 over the certificate DER, lowercase hex.
func fingerprintOfDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// TestClientGeneratesClientCert is the primary proof: a fresh credential store
// yields a usable, self-signed, Ed25519 client certificate, and the properties
// asserted here are the ones another component will depend on.
func TestClientGeneratesClientCert(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	cc, err := loadOrCreateClientCertificate(dir, at)
	if err != nil {
		t.Fatalf("minting client TLS material in a fresh store: %v", err)
	}
	if !cc.Created {
		t.Error("Created = false on the call that minted the material; an agent scripting enrolment branches on this to know the fingerprint has never been seen by a bus")
	}

	// On disk, where the doc comment and the CLI say it is.
	wantDir := filepath.Join(dir, ClientTLSDirName)
	if cc.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", cc.Dir, wantDir)
	}
	for _, p := range []string{cc.CertPath, cc.KeyPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stat %s: %v", p, err)
		}
	}

	if cc.Leaf == nil {
		t.Fatal("Leaf is nil; nothing can read the dates or the fingerprint")
	}
	pub, ok := cc.Leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("certificate public key is %T, want ed25519.PublicKey", cc.Leaf.PublicKey)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}

	// SELF-SIGNED, and actually verified as such rather than assumed from the
	// subject matching the issuer — an unverified self-signature would let a
	// certificate signed by anything at all pass this test.
	//
	// CheckSignature, not CheckSignatureFrom: the latter additionally demands
	// that the signer be a CA with CertSign, which this certificate
	// deliberately is NOT (see the template). What is being asserted here is
	// that the key in the certificate produced the signature on it — nothing
	// about authority to sign anything else.
	if err := cc.Leaf.CheckSignature(cc.Leaf.SignatureAlgorithm, cc.Leaf.RawTBSCertificate, cc.Leaf.Signature); err != nil {
		t.Errorf("the certificate does not verify against its own key: %v", err)
	}

	// ClientAuth only. ServerAuth here would be a certificate claiming to be a
	// server, which an agent never is.
	if got := cc.Leaf.ExtKeyUsage; len(got) != 1 || got[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want exactly [ClientAuth]", got)
	}
	if cc.Leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v, want exactly DigitalSignature (%v) — CertSign in particular would make this certificate a certificate authority",
			cc.Leaf.KeyUsage, x509.KeyUsageDigitalSignature)
	}
	// No subject alternative names: nothing verifies a NAME against a client
	// certificate, and putting one here would invite something to start.
	if len(cc.Leaf.DNSNames) != 0 || len(cc.Leaf.IPAddresses) != 0 {
		t.Errorf("certificate carries SANs (%v / %v); a client certificate names no host", cc.Leaf.DNSNames, cc.Leaf.IPAddresses)
	}
	if cc.Leaf.SerialNumber == nil || cc.Leaf.SerialNumber.Sign() <= 0 {
		t.Errorf("serial number = %v, want a positive integer", cc.Leaf.SerialNumber)
	}

	// Validity: backdated by the skew allowance so a client whose clock runs
	// ahead of the bus's is not refused, and bounded at the same 365 days the
	// bus's own certificate uses.
	if want := at.Add(-clientCertClockSkewAllowance); !cc.Leaf.NotBefore.Equal(want) {
		t.Errorf("NotBefore = %s, want %s (backdated by the clock-skew allowance)", cc.Leaf.NotBefore, want)
	}
	if want := at.Add(ClientCertValidity); !cc.Leaf.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %s, want %s", cc.Leaf.NotAfter, want)
	}
	if cc.IsExpired(at) {
		t.Error("a freshly minted certificate reports itself expired")
	}
	if !cc.IsExpired(at.Add(ClientCertValidity + time.Hour)) {
		t.Error("IsExpired is false an hour past NotAfter; it would never report a certificate that needs replacing")
	}

	// The fingerprint is the value the bus will bind, so its spelling is a
	// contract: SHA-256 over the DER, lowercase hex, identical to the way a BUS
	// certificate's fingerprint is spelled.
	if got, want := cc.Fingerprint(), fingerprintOfDER(cc.Leaf.Raw); got != want {
		t.Errorf("Fingerprint() = %q, want the sha256 of the DER %q", got, want)
	}
	if len(cc.Fingerprint()) != 2*sha256.Size {
		t.Errorf("Fingerprint() is %d characters, want %d", len(cc.Fingerprint()), 2*sha256.Size)
	}
	if got := busFingerprintOfDER(cc.Leaf.Raw).String(); got != cc.Fingerprint() {
		t.Errorf("the client fingerprint (%s) is spelled differently from a bus fingerprint over the same DER (%s); one system, one spelling", cc.Fingerprint(), got)
	}

	// The private key is on disk and USABLE — a certificate whose key does not
	// match it proves nothing, and tls.X509KeyPair is what catches that.
	if len(cc.certificate().Certificate) == 0 || cc.certificate().PrivateKey == nil {
		t.Error("the loaded tls.Certificate has no certificate or no private key")
	}
}

// TestClientCertIsNotACertificateAuthority is the standing guard for the trap
// the security gate caught on 2026-08-07, before it shipped.
//
// An earlier draft copied IsCA:true and KeyUsageCertSign from the BUS's
// certificate, where they are needed because client/pin.go verifies the bus
// leaf against a pool containing itself. On an AGENT's certificate they would
// have authorised something else entirely: the obvious way to write server-side
// client-certificate verification is "put every enrolled agent's certificate in
// one x509.CertPool and Verify against it", and a pool entry is a TRUSTED ROOT.
// With CertSign, any agent could then issue a certificate for any name it liked
// that chains to itself and validates — one agent, root on the whole bus. It
// would have looked like consistency, not like a mistake.
//
// This test asserts the two halves of the remedy:
//
//   - the fields are absent, and stated absent (CA:FALSE, not silence); and
//   - the FORBIDDEN DESIGN NOW FAILS LOUDLY. A CertPool built from these
//     certificates validates nothing, because x509 refuses a non-CA root. That
//     is the property worth having: the wrong implementation cannot be written
//     accidentally, because it does not work.
//
// If this test ever fails because someone re-added IsCA, the remedy is NOT to
// update the test. Fingerprint binding (MTLS-BIND) is the mechanism — exact
// match over the DER, no verifier, no pool, no CA.
func TestClientCertIsNotACertificateAuthority(t *testing.T) {
	dir := t.TempDir()
	cc, err := LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	if cc.Leaf.IsCA {
		t.Error("the client certificate is marked IsCA. If it is ever put in a CertPool — the obvious way to write server-side verification — every agent becomes a certificate authority for the whole bus")
	}
	if cc.Leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("the client certificate carries KeyUsageCertSign, which is permission to issue other certificates. An agent issues nothing")
	}
	if !cc.Leaf.BasicConstraintsValid {
		t.Error("BasicConstraintsValid is false, so the certificate says NOTHING about being a CA rather than saying no. Silence is what a lenient verifier interprets; a stated CA:FALSE is what a strict one refuses, and being refused is the outcome we want")
	}

	// # The escalation itself, attempted and refused
	//
	// This is the attack the removed fields would have permitted, written out
	// exactly. The attacker is an ENROLLED agent: it holds its own key and its
	// own certificate, and the bus has that certificate in a CertPool. It mints
	// a brand-new leaf — any subject it likes — and SIGNS IT WITH ITS OWN KEY.
	// If its certificate were a CA, that leaf would chain to a pool root and
	// validate, and the bus would accept a certificate the attacker minted for
	// a name of its choosing.
	//
	// Note what is NOT asserted, because getting this wrong once already cost a
	// test rewrite: a certificate placed in a Roots pool IS trusted as itself,
	// whatever its basic constraints — crypto/x509 short-circuits when the leaf
	// is a root, which is the very mechanism client/pin.go relies on. That is
	// harmless; it is an exact match wearing a verifier's clothes. The hazard is
	// ISSUANCE, and issuance is what must fail.
	forgedPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a key for the forged leaf: %v", err)
	}
	forgedTmpl := &x509.Certificate{
		SerialNumber: bigOne(),
		Subject:      pkix.Name{CommonName: "some other agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	forgedDER, err := x509.CreateCertificate(cryptorand.Reader, forgedTmpl, cc.Leaf, forgedPub, cc.certificate().PrivateKey)
	if err != nil {
		// crypto/x509 refusing to ISSUE from a non-CA parent is itself the
		// property under test, and is a pass.
		t.Logf("x509 refused to issue from the agent certificate at all: %v", err)
		return
	}
	forged, err := x509.ParseCertificate(forgedDER)
	if err != nil {
		t.Fatalf("parsing the forged leaf: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cc.Leaf)
	if _, err := forged.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err == nil {
		t.Fatal("a certificate MINTED BY AN AGENT, for a subject of its choosing, validated against a CertPool of that agent's own certificate. That agent is a certificate authority for the whole bus. Client certificates must be bound by FINGERPRINT (MTLS-BIND), never verified by chain building")
	}
}

// bigOne is a serial number for a throwaway test certificate. Its value is
// irrelevant; x509 only requires one to be present.
func bigOne() *big.Int { return big.NewInt(1) }

// TestClientTLSKeyIs0600 asserts the storage posture: 0600 files inside a 0700
// directory, so no other local user can read the private key.
//
// Every mode here is written as a LITERAL and never as the package constant it
// is checking. An earlier draft compared os.Stat against clientTLSFileMode,
// which is a tautology: changing that constant to 0644 left the test green
// while the key on disk became world-readable. A permission test that reads its
// expectation out of the code it is testing tests nothing.
func TestClientTLSKeyIs0600(t *testing.T) {
	const (
		wantDirMode  fs.FileMode = 0o700
		wantFileMode fs.FileMode = 0o600
	)
	// The constants are pinned to the literals separately, so that the rest of
	// the package cannot drift from what is asserted below.
	if clientTLSDirMode != wantDirMode || clientTLSFileMode != wantFileMode {
		t.Fatalf("clientTLSDirMode=%#o clientTLSFileMode=%#o, want %#o and %#o", clientTLSDirMode, clientTLSFileMode, wantDirMode, wantFileMode)
	}

	dir := t.TempDir()
	cc, err := LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateClientCertificate: %v", err)
	}

	// Freshly minted material must ALREADY be 0600, not written loose and
	// repaired on the next load. Without this the on-disk checks below cannot
	// tell the two apart, because the load that follows creation tightens
	// whatever it finds — and "written 0644, tightened a moment later" leaves a
	// window, however short, in which the key is another local user's to read.
	// A repair announces itself as a Warning, so an empty list is the assertion.
	if len(cc.Warnings) != 0 {
		t.Errorf("minting a fresh certificate produced repair warnings %v; the files must be created at their final permissions, not corrected afterwards", cc.Warnings)
	}

	info, err := os.Stat(cc.Dir)
	if err != nil {
		t.Fatalf("stat %s: %v", cc.Dir, err)
	}
	if got := info.Mode().Perm(); got != wantDirMode {
		t.Errorf("%s is mode %#o, want %#o — a world-traversable directory is how a private key gets read", cc.Dir, got, wantDirMode)
	}
	for _, p := range []string{cc.KeyPath, cc.CertPath} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != wantFileMode {
			t.Errorf("%s is mode %#o, want %#o", p, got, wantFileMode)
		}
		if got := fi.Mode().Perm(); got&0o077 != 0 {
			t.Errorf("%s is mode %#o: readable or writable by other local users", p, got)
		}
	}

	// A loose key is TIGHTENED and the operator is TOLD. Silently repairing it
	// would leave them believing a key was private when another local user may
	// already have read it.
	if err := os.Chmod(cc.KeyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	again, err := LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("reloading after loosening the key: %v", err)
	}
	if fi, _ := os.Stat(cc.KeyPath); fi.Mode().Perm() != wantFileMode {
		t.Errorf("a key found at mode 0644 was left at %#o instead of being tightened to %#o", fi.Mode().Perm(), wantFileMode)
	}
	if len(again.Warnings) == 0 {
		t.Error("a private key found readable by other local users was tightened SILENTLY; the operator must be told, because tightening it does not un-read it")
	}
}

// TestClientCertIsNotRegeneratedOnSecondCall is the idempotency proof.
//
// It matters more than it looks: a second mint would produce a different
// fingerprint, and the fingerprint is what the bus binds to the agent id. A
// regeneration that "just worked" locally would revoke the agent's TLS identity
// on the bus, and nothing local would look wrong.
func TestClientCertIsNotRegeneratedOnSecondCall(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	keyBefore, err := os.ReadFile(first.KeyPath)
	if err != nil {
		t.Fatalf("reading the key: %v", err)
	}

	second, err := LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.Created {
		t.Error("Created = true on the second call; the material was already there")
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("the certificate changed between two calls: %s then %s. A new fingerprint silently revokes whatever the bus already bound",
			first.Fingerprint(), second.Fingerprint())
	}
	keyAfter, err := os.ReadFile(first.KeyPath)
	if err != nil {
		t.Fatalf("re-reading the key: %v", err)
	}
	if string(keyBefore) != string(keyAfter) {
		t.Error("the private key file was rewritten by a second call; existing key material must never be replaced without a deliberate act")
	}

	// And through the Client, which is how every command reaches it.
	c, err := New(Config{IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	viaClient, err := c.ClientCertificate()
	if err != nil {
		t.Fatalf("Client.ClientCertificate: %v", err)
	}
	if viaClient.Fingerprint() != first.Fingerprint() {
		t.Errorf("Client.ClientCertificate returned %s, want the material already on disk %s", viaClient.Fingerprint(), first.Fingerprint())
	}
}

// TestClientCertConcurrentCreationYieldsOne covers the parallel-agent case:
// several callers reaching a FRESH store at the same moment must converge on
// one certificate, not race to overwrite each other's.
func TestClientCertConcurrentCreationYieldsOne(t *testing.T) {
	dir := t.TempDir()
	const workers = 8

	var wg sync.WaitGroup
	prints := make([]string, workers)
	created := make([]bool, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cc, err := LoadOrCreateClientCertificate(dir)
			if err != nil {
				errs[i] = err
				return
			}
			prints[i] = cc.Fingerprint()
			created[i] = cc.Created
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	for i, got := range prints {
		if got != prints[0] {
			t.Fatalf("worker %d holds certificate %s but worker 0 holds %s; concurrent creation produced two identities", i, got, prints[0])
		}
	}

	// EXACTLY ONE worker may report Created. The others reached the creation
	// path, minted a certificate, lost the installation race and loaded the
	// winner's — and for them "created" is false, because the fingerprint they
	// are holding is not new. This is not cosmetic: Created is documented as
	// "no bus has seen this certificate yet", and an agent scripting enrolment
	// is entitled to branch on it.
	var mints int
	for _, c := range created {
		if c {
			mints++
		}
	}
	if mints != 1 {
		t.Errorf("%d of %d concurrent callers reported Created=true, want exactly 1; every loser of the installation race is holding a certificate that is NOT new", mints, workers)
	}

	// Exactly one directory, and no staging directory left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.Name() != ClientTLSDirName && e.IsDir() {
			t.Errorf("%s left behind in the credential store; a losing racer must clean up after itself", e.Name())
		}
	}
}

// TestClientCertRefusesAnIncompleteDirectory proves the non-repair.
//
// Regenerating the missing half would change the fingerprint, which is the
// value the bus binds — so an automatic "repair" would look like a fix and
// would in fact revoke the agent's TLS identity. It must stop instead, and it
// must not touch the surviving half.
func TestClientCertRefusesAnIncompleteDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		remove  string
		survive string
	}{
		{name: "certificate missing", remove: ClientCertFileName, survive: ClientKeyFileName},
		{name: "key missing", remove: ClientKeyFileName, survive: ClientCertFileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cc, err := LoadOrCreateClientCertificate(dir)
			if err != nil {
				t.Fatalf("minting: %v", err)
			}
			survivor := filepath.Join(cc.Dir, tc.survive)
			before, err := os.ReadFile(survivor)
			if err != nil {
				t.Fatalf("reading %s: %v", survivor, err)
			}
			if err := os.Remove(filepath.Join(cc.Dir, tc.remove)); err != nil {
				t.Fatalf("removing %s: %v", tc.remove, err)
			}

			_, err = LoadOrCreateClientCertificate(dir)
			if err == nil {
				t.Fatal("a half-populated client TLS directory was silently repaired; the new fingerprint would revoke whatever the bus had bound")
			}
			if !errors.Is(err, ErrClientCertIncomplete) {
				t.Errorf("error does not wrap ErrClientCertIncomplete: %v", err)
			}
			if got := KindOf(err); got != KindConfig {
				t.Errorf("Kind = %q, want %q", got, KindConfig)
			}
			var cerr *Error
			if !errors.As(err, &cerr) || cerr.Remedy == "" {
				t.Errorf("the error names no remedy: %v", err)
			}
			after, rerr := os.ReadFile(survivor)
			if rerr != nil {
				t.Fatalf("re-reading %s: %v", survivor, rerr)
			}
			if string(before) != string(after) {
				t.Errorf("%s was modified by the refusal; a refusal must not touch surviving key material", survivor)
			}
		})
	}
}

// TestClientCertFailsClosed is the negative the whole design turns on: a client
// that CANNOT produce its certificate must stop, not connect without one.
//
// The load-bearing assertion is the request count. "It returned an error" would
// also be satisfied by a client that connected unauthenticated, did the work
// and complained afterwards — which is exactly the failure that looks fine
// today and becomes a lockout the day the bus starts checking.
func TestClientCertFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so an unwritable directory cannot be simulated")
	}

	t.Run("the store cannot be written", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		_, err := LoadOrCreateClientCertificate(dir)
		if err == nil {
			t.Fatal("minting succeeded in a read-only credential store")
		}
		if got := KindOf(err); got != KindConfig {
			t.Errorf("Kind = %q, want %q", got, KindConfig)
		}
		var cerr *Error
		if !errors.As(err, &cerr) {
			t.Fatalf("not a *client.Error: %v", err)
		}
		if cerr.Remedy == "" {
			t.Errorf("the error names no remedy, only a cause: %v", err)
		}
		if cerr.Op != "client-cert" {
			t.Errorf("Op = %q, want %q so a caller can tell this from an enrolment failure", cerr.Op, "client-cert")
		}
	})

	t.Run("no request is sent without a certificate", func(t *testing.T) {
		busCert := newSelfSignedBusCert(t)
		var hits int32
		bus := newMutualTLSBus(t, busCert, tls.NoClientCert, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.WriteHeader(http.StatusOK)
		})

		dir := t.TempDir()
		c, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(busCert).String(), IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// Made unwritable AFTER New, so the store opens and the only thing that
		// can fail is the certificate.
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		_, err = c.Enrol(context.Background(), EnrolOptions{Name: "planner"})
		if err == nil {
			t.Fatal("enrolled without being able to produce a client certificate")
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Errorf("the bus saw %d requests; a client that cannot present its certificate must not connect at all, or the failure only appears the day the bus starts asking", got)
		}
	})
}

// TestClientPresentsItsCertificate is the end-to-end proof over a LIVE
// handshake, in both directions that matter.
//
// The second subtest is the one that guards the deployment order: the bus
// TODAY does not ask for a client certificate, and this change must not break
// it. The first is what MTLS-CLIENTAUTH will turn on.
func TestClientPresentsItsCertificate(t *testing.T) {
	okBody := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agent_id":"bus-testbus.planner","bus_id":"bus-testbus","name":"planner","enrolled_at":"2026-08-07T12:00:00Z"}`))
	}

	t.Run("the bus asks, and gets exactly the stored certificate", func(t *testing.T) {
		busCert := newSelfSignedBusCert(t)
		bus := newMutualTLSBus(t, busCert, tls.RequireAnyClientCert, okBody)

		dir := t.TempDir()
		c, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(busCert).String(), IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true}); err != nil {
			t.Fatalf("enrolling against a bus that REQUIRES a client certificate: %v", err)
		}

		der, seen := bus.presented()
		if !seen {
			t.Fatal("the bus saw no request at all")
		}
		if len(der) == 0 {
			t.Fatal("the request arrived with NO client certificate. crypto/tls filters tls.Config.Certificates against the server's acceptable-CA list, and a self-signed certificate chains to no CA — which is why this is presented through GetClientCertificate instead")
		}

		mine, err := LoadOrCreateClientCertificate(dir)
		if err != nil {
			t.Fatalf("reading back the stored material: %v", err)
		}
		if mine.Created {
			t.Error("the material was minted by this read-back, so the connection above presented something else entirely")
		}
		if got, want := fingerprintOfDER(der), mine.Fingerprint(); got != want {
			t.Errorf("the bus was presented %s but the store holds %s", got, want)
		}

		// The PRIVATE key is not on the wire. What the server received is a
		// certificate — public by definition — and parsing it yields a public
		// key, never a private one.
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("the bus received something that is not a certificate: %v", err)
		}
		if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
			t.Errorf("the presented certificate carries a %T", leaf.PublicKey)
		}
		keyPEM, err := os.ReadFile(mine.KeyPath)
		if err != nil {
			t.Fatalf("reading the key: %v", err)
		}
		if len(keyPEM) == 0 {
			t.Fatal("the key file is empty")
		}
	})

	t.Run("the bus does not ask, and everything still works", func(t *testing.T) {
		busCert := newSelfSignedBusCert(t)
		bus := newMutualTLSBus(t, busCert, tls.NoClientCert, okBody)

		dir := t.TempDir()
		c, err := New(Config{BusURL: bus.URL(), BusFingerprint: fingerprintOf(busCert).String(), IdentityDir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner", Save: true}); err != nil {
			t.Fatalf("enrolling against a bus that does NOT ask for a client certificate — which is every bus today — failed: %v", err)
		}
		der, seen := bus.presented()
		if !seen {
			t.Fatal("the bus saw no request at all")
		}
		if len(der) != 0 {
			t.Error("a certificate was presented to a bus that did not ask; crypto/tls must only send one on request")
		}
	})
}

// TestClientCertificateProviderIgnoresTheAcceptableCAList pins the reason this
// is a callback and not tls.Config.Certificates.
//
// crypto/tls FILTERS Certificates against the CertificateRequest's
// acceptable-CA list. This certificate is self-signed, so it chains to no CA
// any bus could name, and under that filter the client would send an EMPTY
// certificate message — which reads on the server as "this client has none".
// The lockout would be silent and would log no decision anywhere.
func TestClientCertificateProviderIgnoresTheAcceptableCAList(t *testing.T) {
	dir := t.TempDir()
	cc, err := LoadOrCreateClientCertificate(dir)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	cfg := pinnedTLSConfig(NewBusPinSet(BusFingerprint{1}), cc.certificate())
	if cfg.GetClientCertificate == nil {
		t.Fatal("the pinned TLS config offers no client certificate; the presenting half of mutual TLS is missing")
	}
	// A request naming CAs this certificate has never heard of, plus a
	// signature-algorithm list it does not appear in. Both are shapes under
	// which a filtering implementation would return nothing.
	got, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{
		AcceptableCAs:    [][]byte{[]byte("some other certificate authority")},
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		Version:          tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if got == nil || len(got.Certificate) == 0 {
		t.Fatal("no certificate was offered for a request naming an unrelated CA. Sending nothing produces 'unauthenticated', which is a mystery; sending ours produces a handshake error that names the disagreement, which is a bug report")
	}
	if fingerprintOfDER(got.Certificate[0]) != cc.Fingerprint() {
		t.Errorf("offered %s, want the stored %s", fingerprintOfDER(got.Certificate[0]), cc.Fingerprint())
	}

	// And the verifying half is untouched by any of this.
	if cfg.VerifyPeerCertificate == nil {
		t.Error("the pinned config lost its bus-certificate check while gaining a client certificate")
	}

	// With no material, the field is left unset — which is exactly "present
	// nothing", and is what the plaintext-loopback branch needs.
	if pinnedTLSConfig(NewBusPinSet(BusFingerprint{1}), nil).GetClientCertificate != nil {
		t.Error("a nil client certificate produced a callback; it must leave the field unset rather than offer an empty certificate")
	}
}

// TestClientCertRejectsGarbage covers the damaged-store paths: material that is
// present but unusable is refused, never silently replaced.
func TestClientCertRejectsGarbage(t *testing.T) {
	t.Run("not PEM", func(t *testing.T) {
		dir := t.TempDir()
		cc, err := LoadOrCreateClientCertificate(dir)
		if err != nil {
			t.Fatalf("minting: %v", err)
		}
		if err := os.WriteFile(cc.CertPath, []byte("this is not a certificate\n"), clientTLSFileMode); err != nil {
			t.Fatalf("corrupting: %v", err)
		}
		_, err = LoadOrCreateClientCertificate(dir)
		if err == nil {
			t.Fatal("a damaged certificate was accepted or silently replaced")
		}
		if got := KindOf(err); got != KindConfig {
			t.Errorf("Kind = %q, want %q", got, KindConfig)
		}
	})

	// The size bound, asserted BY THE BOUND rather than by "an error happened".
	//
	// `err != nil` alone was not discriminating and the reviewer was right to
	// call it: delete maxClientCertFileBytes entirely and an oversized
	// zero-filled key still fails, one layer later, inside tls.X509KeyPair. The
	// subtest would have stayed green over a removed control. So it asserts the
	// bound's OWN message, naming the actual size — which only the bounded path
	// can produce.
	t.Run("larger than the bound is refused BY the bound", func(t *testing.T) {
		dir := t.TempDir()
		cc, err := LoadOrCreateClientCertificate(dir)
		if err != nil {
			t.Fatalf("minting: %v", err)
		}
		oversized := make([]byte, maxClientCertFileBytes+1)
		if err := os.WriteFile(cc.KeyPath, oversized, clientTLSFileMode); err != nil {
			t.Fatalf("corrupting: %v", err)
		}
		_, err = LoadOrCreateClientCertificate(dir)
		if err == nil {
			t.Fatal("a key file larger than the bound was read anyway")
		}
		wantSize := strconv.Itoa(len(oversized))
		if !strings.Contains(err.Error(), wantSize) {
			t.Errorf("error %q does not name the offending size %s, so it did not come from the size bound — it came from something downstream, and the bound could be deleted with this test still green", err, wantSize)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(maxClientCertFileBytes)) {
			t.Errorf("error %q does not name the bound %d", err, maxClientCertFileBytes)
		}
	})

	// A stat-size bound does not cover a file whose stat size is a lie. A
	// character device reports 0 and reads forever; a FIFO blocks. Neither is a
	// plausible accident inside a 0700 directory, but "the directory is 0700"
	// is a claim about the attacker rather than about the code.
	t.Run("not an ordinary file", func(t *testing.T) {
		dir := t.TempDir()
		cc, err := LoadOrCreateClientCertificate(dir)
		if err != nil {
			t.Fatalf("minting: %v", err)
		}
		if err := os.Remove(cc.CertPath); err != nil {
			t.Fatalf("removing: %v", err)
		}
		if err := os.Symlink(os.DevNull, cc.CertPath); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		// A deadline, because the failure mode being guarded against is a read
		// that never returns — and a test that hangs reports nothing.
		done := make(chan error, 1)
		go func() {
			_, lerr := LoadOrCreateClientCertificate(dir)
			done <- lerr
		}()
		select {
		case lerr := <-done:
			if lerr == nil {
				t.Fatal("a character device was accepted as a certificate")
			}
			if !strings.Contains(lerr.Error(), "ordinary file") {
				t.Errorf("error %q does not say the file is not an ordinary file; it was refused for some other reason and a /dev/zero symlink would still be read unbounded", lerr)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("loading a character device did not return within 10s: the stat-size bound does not cover it and the read is unbounded")
		}
	})

	t.Run("no identity directory", func(t *testing.T) {
		_, err := LoadOrCreateClientCertificate("")
		if err == nil {
			t.Fatal("an empty credential store directory was accepted")
		}
		var cerr *Error
		if !errors.As(err, &cerr) || cerr.Remedy == "" {
			t.Errorf("the error names no remedy: %v", err)
		}
	})
}

// TestClientCertRefusesAMateriallessDirectory covers the wedge all three
// reviews found independently, and which no test caught.
//
// os.Rename Lstats its target and refuses ANY pre-existing directory, empty or
// not — so an EMPTY or junk-filled client-tls was classified as "another
// process won the race". The reload then found nothing and returned the bare
// fs.ErrNotExist sentinel the load path uses INTERNALLY to mean "go mint". That
// is not a *client.Error: no Kind, no Remedy, exit code 1 instead of 3. And
// because the certificate is resolved on EVERY pinned request, the state wedged
// every command against a real bus, permanently, with a message naming no fix.
//
// Reachable by an operator who reads "move the whole directory aside" and moves
// its CONTENTS, by a partially-failed RemoveAll, or by an rsync that recreated
// the directory entry.
func TestClientCertRefusesAMateriallessDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "empty",
			setup: func(t *testing.T, dir string) {},
		},
		{
			name: "junk only",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "README"), []byte("not a certificate\n"), 0o600); err != nil {
					t.Fatalf("seeding junk: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityDir := t.TempDir()
			dir := filepath.Join(identityDir, ClientTLSDirName)
			if err := os.MkdirAll(dir, clientTLSDirMode); err != nil {
				t.Fatalf("creating the directory: %v", err)
			}
			tc.setup(t, dir)

			_, err := LoadOrCreateClientCertificate(identityDir)
			if err == nil {
				t.Fatal("a materialless client-tls directory was accepted")
			}
			// The whole point: a CLASSIFIED error with a remedy, not a bare
			// sentinel. Exit code 3 (config), never 1 (internal).
			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("error is not a *client.Error, so it carries no Kind and no Remedy and exits 1: %v", err)
			}
			if cerr.Kind != KindConfig {
				t.Errorf("Kind = %q, want %q", cerr.Kind, KindConfig)
			}
			if got := ExitCode(err); got != ExitConfig {
				t.Errorf("ExitCode = %d, want %d", got, ExitConfig)
			}
			if cerr.Remedy == "" {
				t.Error("the error names no remedy, so an operator is told the client is broken and not how to fix it")
			}
			if !strings.Contains(cerr.Remedy+cerr.Message, dir) {
				t.Errorf("neither the message nor the remedy names %s: %v / %v", dir, cerr.Message, cerr.Remedy)
			}
			// The remedy must be the one that WORKS. This state is permanent —
			// re-running changes nothing — so a remedy of "try again" is worse
			// than none: it sends the operator round a loop. The only thing
			// that fixes it is moving the directory out of the way, and the
			// error has to say so. This assertion is what distinguishes the
			// real fix from merely wrapping the bare sentinel into a
			// classified-but-useless error.
			if !strings.Contains(strings.ToLower(cerr.Remedy), "move") {
				t.Errorf("remedy %q does not tell the operator to move %s aside. This state does not clear by itself, so any remedy that amounts to 'run it again' is a loop", cerr.Remedy, dir)
			}
			if strings.Contains(strings.ToLower(cerr.Remedy), "re-run the command") {
				t.Errorf("remedy %q tells the operator to re-run, but this failure is permanent: %s exists and holds no material, and every subsequent run takes the same path", cerr.Remedy, dir)
			}
			// It must be repeatable rather than half-applied: the same call
			// again gives the same refusal, and nothing was minted into the
			// broken directory.
			if _, again := LoadOrCreateClientCertificate(identityDir); again == nil {
				t.Error("the second call succeeded, so the first left the directory in a different state")
			}
			if _, serr := os.Stat(filepath.Join(dir, ClientKeyFileName)); !errors.Is(serr, fs.ErrNotExist) {
				t.Error("a key was minted into a directory the code refused to use")
			}
		})
	}
}

// TestClientCertInstallFailureNamesItsCause covers the branch where the rename
// fails and the target is NOT there — no space, read-only filesystem, a path
// too long, a permission the process lacks.
//
// The point is that the error must not assert something it did not check. An
// earlier version of this branch told every such operator that the directory
// "already exists", which was false and sent them to move a directory that was
// not there, while the real cause was invisible: *Error.Error() prints Message
// and never the wrapped cause, so a bare wrap loses it entirely.
func TestClientCertInstallFailureNamesItsCause(t *testing.T) {
	identityDir := t.TempDir()
	// A target name far past NAME_MAX. MkdirTemp and the writes all succeed —
	// only the final rename fails — so this exercises the intended branch
	// rather than failing early.
	longName := strings.Repeat("n", 400)
	dir := filepath.Join(identityDir, longName)

	minted, err := createClientTLS(identityDir, dir, time.Now())
	if err == nil {
		t.Skipf("this filesystem accepted a %d-character directory name; nothing to assert", len(longName))
	}
	if minted {
		t.Error("createClientTLS reported success alongside an error")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("not a *client.Error: %v", err)
	}
	if strings.Contains(cerr.Message, "already exists") {
		t.Errorf("message %q claims the target already exists, which was never checked and is false here", cerr.Message)
	}
	// The cause must survive into what an operator actually sees, which is
	// Error() — and Error() prints Message.
	if !strings.Contains(err.Error(), "long") && !strings.Contains(err.Error(), "name") {
		t.Errorf("Error() = %q does not carry the underlying cause, so the operator is told it failed and never why", err)
	}
	if cerr.Remedy == "" {
		t.Error("no remedy")
	}
	// And nothing was left behind holding a private key.
	entries, rerr := os.ReadDir(identityDir)
	if rerr != nil {
		t.Fatalf("reading %s: %v", identityDir, rerr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".client-tls-tmp-") {
			t.Errorf("%s left behind after a failed install, holding a live private key", e.Name())
		}
	}
}

// TestClientCertLostRaceIsDeterministic exercises the lost-installation-race
// path WITHOUT depending on the scheduler.
//
// TestClientCertConcurrentCreationYieldsOne covers the same path with real
// goroutines, but a reviewer showed it stops DETECTING a regression under
// GOMAXPROCS=1: worker 0 finishes installing before the others reach the
// creation path, so they never race and report Created=false correctly by
// accident. This one pre-installs the winner's material and then calls the
// creation path directly, so it exercises the branch on any machine.
func TestClientCertLostRaceIsDeterministic(t *testing.T) {
	identityDir := t.TempDir()
	winner, err := LoadOrCreateClientCertificate(identityDir)
	if err != nil {
		t.Fatalf("installing the winner's material: %v", err)
	}

	dir := filepath.Join(identityDir, ClientTLSDirName)
	minted, err := createClientTLS(identityDir, dir, time.Now())
	if err != nil {
		t.Fatalf("createClientTLS against an already-populated directory: %v; losing the race is a success, not an error", err)
	}
	if minted {
		t.Error("createClientTLS reported that it installed material into a directory that was already populated")
	}

	// The winner's certificate is untouched, and the loser's staging directory
	// — which contains a live private key — is gone.
	after, err := LoadOrCreateClientCertificate(identityDir)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if after.Fingerprint() != winner.Fingerprint() {
		t.Errorf("the certificate changed to %s; the loser overwrote the winner's material", after.Fingerprint())
	}
	if after.Created {
		t.Error("Created = true for material that was already on disk")
	}
	entries, err := os.ReadDir(identityDir)
	if err != nil {
		t.Fatalf("reading %s: %v", identityDir, err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".client-tls-tmp-") {
			t.Errorf("%s left behind, holding a live private key nothing will ever clean up", e.Name())
		}
	}
}

// TestClientCertNotCreatedForAPlaintextBus asserts the material is minted
// lazily and only where there is a handshake to present it in.
//
// A plaintext loopback bus performs no handshake, so requiring a writable
// credential store to reach one would be a new failure for no gain.
func TestClientCertNotCreatedForAPlaintextBus(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent_id":"bus-testbus.planner","bus_id":"bus-testbus","name":"planner","enrolled_at":"2026-08-07T12:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Enrol(context.Background(), EnrolOptions{Name: "planner"}); err != nil {
		t.Fatalf("enrolling over plaintext loopback: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("the bus saw %d requests, want 1", atomic.LoadInt32(&hits))
	}
	if _, err := os.Stat(filepath.Join(dir, ClientTLSDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("client TLS material was minted for a plaintext connection that has no handshake to present it in (stat: %v)", err)
	}
}
