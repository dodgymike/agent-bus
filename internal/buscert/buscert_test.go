package buscert_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// fixedNow returns a clock pinned to t, so a test can drive the certificate
// validity window without sleeping. Options.Now is the only hook this package
// offers, deliberately.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// mint generates material into a fresh directory and returns both.
func mint(t *testing.T, opts buscert.Options) (string, *buscert.Material) {
	t.Helper()
	dir := t.TempDir()
	m, err := buscert.LoadOrCreate(dir, opts)
	if err != nil {
		t.Fatalf("LoadOrCreate on a virgin directory: %v", err)
	}
	return dir, m
}

func TestBusCertGeneratedOnFirstStart(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	dir, m := mint(t, buscert.Options{BusID: "bus-1", Hosts: []string{"bus.example", "10.0.0.7"}, Now: fixedNow(now)})

	if !m.Generated() {
		t.Fatal("Generated() = false on a virgin directory, want true")
	}

	for _, name := range []string{buscert.CertFileName, buscert.TLSKeyFileName, buscert.SigningKeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}

	leaf := m.Certificate()
	if leaf.Subject.CommonName != "bus-1" {
		t.Errorf("CommonName = %q, want the bus id", leaf.Subject.CommonName)
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		t.Errorf("certificate public key is %T, want ed25519.PublicKey", leaf.PublicKey)
	}
	if !leaf.IsCA || !leaf.BasicConstraintsValid {
		t.Errorf("IsCA = %v, BasicConstraintsValid = %v, want both true (the certificate signs itself)", leaf.IsCA, leaf.BasicConstraintsValid)
	}
	if want := now.Add(buscert.CertValidity); !leaf.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %s, want %s", leaf.NotAfter, want)
	}
	if !leaf.NotBefore.Before(now) {
		t.Errorf("NotBefore = %s, want it backdated before %s for clock skew", leaf.NotBefore, now)
	}
	if !m.NotAfter().Equal(leaf.NotAfter) {
		t.Errorf("NotAfter() = %s, want the leaf's %s", m.NotAfter(), leaf.NotAfter)
	}

	// The certificate must verify as its own signer: it is self-signed, and
	// there is no CA anywhere in this design.
	if err := leaf.CheckSignatureFrom(leaf); err != nil {
		t.Errorf("the certificate is not validly self-signed: %v", err)
	}

	// SANs are required (Go dropped the CommonName fallback in 1.15). The
	// loopback set is always present, plus whatever the caller asked for.
	for _, want := range []string{"localhost", "bus.example"} {
		if !containsString(leaf.DNSNames, want) {
			t.Errorf("DNSNames = %v, want it to contain %q", leaf.DNSNames, want)
		}
	}
	for _, want := range []string{"127.0.0.1", "::1", "10.0.0.7"} {
		if !containsIP(leaf.IPAddresses, want) {
			t.Errorf("IPAddresses = %v, want it to contain %q", leaf.IPAddresses, want)
		}
	}

	if want := x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign; leaf.KeyUsage != want {
		t.Errorf("KeyUsage = %v, want %v", leaf.KeyUsage, want)
	}
	if len(leaf.ExtKeyUsage) != 2 ||
		leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth ||
		leaf.ExtKeyUsage[1] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want ServerAuth and ClientAuth (a bus dials peer buses with this same certificate)", leaf.ExtKeyUsage)
	}
	if leaf.SerialNumber.Sign() <= 0 {
		t.Errorf("SerialNumber = %s, want a positive integer", leaf.SerialNumber)
	}

	// The loaded pair is directly usable as a tls.Config certificate.
	tlsCert := m.TLSCertificate()
	if tlsCert.Leaf == nil {
		t.Error("TLSCertificate().Leaf is nil, want it populated so handshakes do not re-parse the DER")
	}
	if len(tlsCert.Certificate) != 1 || !bytes.Equal(tlsCert.Certificate[0], leaf.Raw) {
		t.Error("TLSCertificate() does not carry exactly the leaf DER")
	}
}

func TestBusCertGeneratedOnFirstStartUsesADescriptiveCommonNameWithoutABusID(t *testing.T) {
	_, m := mint(t, buscert.Options{})
	if m.Certificate().Subject.CommonName == "" {
		t.Error("CommonName is empty; want a fixed descriptive string when no bus id is supplied")
	}
}

func TestBusCertKeyIs0600(t *testing.T) {
	dir, _ := mint(t, buscert.Options{BusID: "bus-1"})

	cases := []struct {
		name string
		file string
		want os.FileMode
	}{
		{"tls key is secret", buscert.TLSKeyFileName, 0o600},
		{"signing key is secret", buscert.SigningKeyFileName, 0o600},
		// The certificate is public by construction -- it is sent to every
		// client in every handshake -- so 0644 is correct, not a slip.
		{"certificate is public", buscert.CertFileName, 0o644},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fi, err := os.Stat(filepath.Join(dir, tc.file))
			if err != nil {
				t.Fatalf("stat %s: %v", tc.file, err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("%s has mode %#o, want %#o", tc.file, got, tc.want)
			}
		})
	}

	if buscert.KeyFileMode.Perm() != 0o600 {
		t.Errorf("KeyFileMode = %#o, want 0600", buscert.KeyFileMode.Perm())
	}
	if buscert.CertFileMode.Perm() != 0o644 {
		t.Errorf("CertFileMode = %#o, want 0644", buscert.CertFileMode.Perm())
	}
}

// TestBusCertFingerprintIsSHA256OfDER recomputes the fingerprint independently,
// straight from the PEM on disk, and requires the package's answer to match. It
// is the anti-divergence test: the construction is fixed by the ENROL-SHAPE
// decision and mirrored in internal/auth and internal/invite, so a change here
// would silently invalidate every pin already issued.
func TestBusCertFingerprintIsSHA256OfDER(t *testing.T) {
	dir, m := mint(t, buscert.Options{BusID: "bus-1"})

	pemBytes, err := os.ReadFile(filepath.Join(dir, buscert.CertFileName))
	if err != nil {
		t.Fatalf("read the certificate: %v", err)
	}
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("the certificate file does not hold a single PEM CERTIFICATE block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("the certificate file holds trailing data: %q", rest)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the certificate: %v", err)
	}

	sum := sha256.Sum256(leaf.Raw)
	want := hex.EncodeToString(sum[:])

	if got := m.Fingerprint().String(); got != want {
		t.Errorf("Fingerprint() = %s, want sha256(leaf.Raw) = %s", got, want)
	}
	if got := buscert.FingerprintOf(leaf).String(); got != want {
		t.Errorf("FingerprintOf(leaf) = %s, want %s", got, want)
	}
	if got := buscert.FingerprintOfDER(block.Bytes).String(); got != want {
		t.Errorf("FingerprintOfDER(der) = %s, want %s", got, want)
	}
	if len(want) != hex.EncodedLen(buscert.DigestSize) {
		t.Errorf("the textual fingerprint is %d characters, want %d", len(want), hex.EncodedLen(buscert.DigestSize))
	}

	// The DER hashed must be the certificate's own bytes, not a re-encoding.
	if !bytes.Equal(leaf.Raw, block.Bytes) {
		t.Error("leaf.Raw differs from the DER in the PEM block")
	}

	parsed, err := buscert.ParseFingerprint(want)
	if err != nil {
		t.Fatalf("ParseFingerprint round trip: %v", err)
	}
	if !parsed.Equal(m.Fingerprint()) {
		t.Error("ParseFingerprint(Fingerprint().String()) did not round trip")
	}
}

func TestParseFingerprintRejects(t *testing.T) {
	valid := hex.EncodeToString(bytes.Repeat([]byte{0xab}, buscert.DigestSize))
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"too short", valid[:len(valid)-2]},
		{"too long", valid + "ab"},
		{"uppercase", "AB" + valid[2:]},
		{"not hex", "zz" + valid[2:]},
		{"colon separated", "ab:" + valid[3:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buscert.ParseFingerprint(tc.in); !errors.Is(err, buscert.ErrMalformed) {
				t.Errorf("ParseFingerprint(%q) error = %v, want ErrMalformed", tc.in, err)
			}
		})
	}
	if _, err := buscert.ParseFingerprint(valid); err != nil {
		t.Errorf("ParseFingerprint(%q) = %v, want it accepted", valid, err)
	}
}

// A second LoadOrCreate must return exactly the material the first minted, and
// must NOT report Generated: re-minting would break every client that pinned
// the first fingerprint.
func TestBusCertReloadIsIdempotent(t *testing.T) {
	dir, first := mint(t, buscert.Options{BusID: "bus-1"})

	// Different options on the reload: they must be ignored entirely, because
	// the certificate on disk is the one clients already pinned.
	second, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: "a-different-bus-id", Hosts: []string{"elsewhere.example"}})
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	if second.Generated() {
		t.Error("Generated() = true on a reload, want false")
	}
	if !second.Fingerprint().Equal(first.Fingerprint()) {
		t.Errorf("fingerprint changed across reload: %s then %s", first.Fingerprint(), second.Fingerprint())
	}
	if !bytes.Equal(second.SigningPublicKey(), first.SigningPublicKey()) {
		t.Error("the signing public key changed across reload")
	}
	if second.Certificate().Subject.CommonName != "bus-1" {
		t.Errorf("CommonName = %q on reload; the options must not re-mint the certificate", second.Certificate().Subject.CommonName)
	}
	for _, p := range []struct{ got, want string }{
		{second.CertPath(), filepath.Join(dir, buscert.CertFileName)},
		{second.TLSKeyPath(), filepath.Join(dir, buscert.TLSKeyFileName)},
		{second.SigningKeyPath(), filepath.Join(dir, buscert.SigningKeyFileName)},
	} {
		if p.got != p.want {
			t.Errorf("path = %s, want %s", p.got, p.want)
		}
	}
}

// The TLS key and the signing key must be genuinely independent material: one
// is pinned by clients, the other by peer buses, and they rotate on different
// schedules with different blast radii (DECISIONS.md 2026-08-07).
func TestBusCertTLSAndSigningKeysAreDistinct(t *testing.T) {
	dir, m := mint(t, buscert.Options{BusID: "bus-1"})

	tlsPriv := readKey(t, filepath.Join(dir, buscert.TLSKeyFileName))
	signingPriv := m.SigningPrivateKey()

	if bytes.Equal(tlsPriv, signingPriv) {
		t.Fatal("the TLS private key and the signing private key are the same bytes")
	}
	tlsPub := tlsPriv.Public().(ed25519.PublicKey)
	if bytes.Equal(tlsPub, m.SigningPublicKey()) {
		t.Fatal("the TLS public key and the signing public key are the same")
	}
	// The certificate must carry the TLS key, never the signing key.
	certPub, ok := m.Certificate().PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("certificate public key is %T", m.Certificate().PublicKey)
	}
	if !bytes.Equal(certPub, tlsPub) {
		t.Error("the certificate does not carry the TLS public key")
	}
	if bytes.Equal(certPub, m.SigningPublicKey()) {
		t.Error("the certificate carries the SIGNING key; the two keys must never be conflated")
	}

	// Two separate data directories must not share material either.
	_, other := mint(t, buscert.Options{BusID: "bus-1"})
	if other.Fingerprint().Equal(m.Fingerprint()) {
		t.Error("two independently generated buses share a certificate fingerprint")
	}
	if bytes.Equal(other.SigningPublicKey(), m.SigningPublicKey()) {
		t.Error("two independently generated buses share a signing key")
	}
}

// A key file readable by group or other is FATAL. This package is deliberately
// stricter than internal/wal's MAC key, which does not make this check.
func TestBusCertRefusesLooseKeyPermissions(t *testing.T) {
	cases := []struct {
		name string
		file string
		mode os.FileMode
		want error
	}{
		{"tls key world readable", buscert.TLSKeyFileName, 0o644, buscert.ErrPermissions},
		{"tls key group readable", buscert.TLSKeyFileName, 0o640, buscert.ErrPermissions},
		{"signing key world readable", buscert.SigningKeyFileName, 0o644, buscert.ErrPermissions},
		{"signing key world writable", buscert.SigningKeyFileName, 0o606, buscert.ErrPermissions},
		{"tighter than required is fine", buscert.TLSKeyFileName, 0o400, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := mint(t, buscert.Options{BusID: "bus-1"})
			path := filepath.Join(dir, tc.file)
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			_, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: "bus-1"})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("LoadOrCreate = %v, want it accepted", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("LoadOrCreate error = %v, want ErrPermissions", err)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(path)) {
				t.Errorf("the error does not name the offending path %s: %v", path, err)
			}
		})
	}

	// The certificate is public: a 0644 certificate must be accepted (E7 --
	// an operator-supplied certificate is legitimately mounted 0644).
	dir, _ := mint(t, buscert.Options{BusID: "bus-1"})
	if err := os.Chmod(filepath.Join(dir, buscert.CertFileName), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := buscert.LoadOrCreate(dir, buscert.Options{}); err != nil {
		t.Errorf("a 0644 certificate was refused: %v", err)
	}
}

// Nothing here is repaired or regenerated: a file that exists but cannot be
// used is fatal, and the error names the file.
func TestBusCertRefusesCorruptMaterial(t *testing.T) {
	otherKeyPEM := func(t *testing.T) []byte {
		t.Helper()
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}

	cases := []struct {
		name    string
		file    string
		mutate  func(t *testing.T, original []byte) []byte
		wantErr error
	}{
		{
			name:    "truncated key",
			file:    buscert.TLSKeyFileName,
			mutate:  func(_ *testing.T, b []byte) []byte { return b[:len(b)/2] },
			wantErr: buscert.ErrMalformed,
		},
		{
			name:    "empty key",
			file:    buscert.SigningKeyFileName,
			mutate:  func(_ *testing.T, b []byte) []byte { return nil },
			wantErr: buscert.ErrMalformed,
		},
		{
			name:    "not pem at all",
			file:    buscert.SigningKeyFileName,
			mutate:  func(_ *testing.T, b []byte) []byte { return []byte("this is not a key\n") },
			wantErr: buscert.ErrMalformed,
		},
		{
			name: "wrong pem block type",
			file: buscert.TLSKeyFileName,
			mutate: func(_ *testing.T, b []byte) []byte {
				block, _ := pem.Decode(b)
				return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: block.Bytes})
			},
			wantErr: buscert.ErrMalformed,
		},
		{
			name:    "a second pem block",
			file:    buscert.CertFileName,
			mutate:  func(t *testing.T, b []byte) []byte { return append(append([]byte{}, b...), b...) },
			wantErr: buscert.ErrMalformed,
		},
		{
			name: "corrupt certificate der",
			file: buscert.CertFileName,
			mutate: func(_ *testing.T, b []byte) []byte {
				block, _ := pem.Decode(b)
				return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes[:20]})
			},
			wantErr: buscert.ErrMalformed,
		},
		{
			name:    "key that does not match the certificate",
			file:    buscert.TLSKeyFileName,
			mutate:  func(t *testing.T, _ []byte) []byte { return otherKeyPEM(t) },
			wantErr: buscert.ErrMalformed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := mint(t, buscert.Options{BusID: "bus-1"})
			path := filepath.Join(dir, tc.file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			mode := os.FileMode(0o600)
			if tc.file == buscert.CertFileName {
				mode = 0o644
			}
			if err := os.WriteFile(path, tc.mutate(t, original), mode); err != nil {
				t.Fatalf("write %s: %v", tc.file, err)
			}

			_, err = buscert.LoadOrCreate(dir, buscert.Options{BusID: "bus-1"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("LoadOrCreate error = %v, want %v", err, tc.wantErr)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(path)) {
				t.Errorf("the error does not name the offending path %s: %v", path, err)
			}
		})
	}
}

// A directory (or any non-regular file) where a key belongs is refused rather
// than read.
func TestBusCertRefusesANonRegularKeyFile(t *testing.T) {
	dir, _ := mint(t, buscert.Options{BusID: "bus-1"})
	path := filepath.Join(dir, buscert.SigningKeyFileName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := buscert.LoadOrCreate(dir, buscert.Options{})
	if !errors.Is(err, buscert.ErrMalformed) {
		t.Fatalf("LoadOrCreate error = %v, want ErrMalformed", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte(path)) {
		t.Errorf("the error does not name %s: %v", path, err)
	}
}

// A symlinked key file IS accepted -- an operator secret mount is legitimately
// a symlink, and the mode that matters is the target's.
func TestBusCertAcceptsASymlinkedKeyFile(t *testing.T) {
	dir, m := mint(t, buscert.Options{BusID: "bus-1"})
	path := filepath.Join(dir, buscert.SigningKeyFileName)
	elsewhere := filepath.Join(t.TempDir(), "mounted-secret")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(elsewhere, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	reloaded, err := buscert.LoadOrCreate(dir, buscert.Options{})
	if err != nil {
		t.Fatalf("a symlinked key file was refused: %v", err)
	}
	if !bytes.Equal(reloaded.SigningPublicKey(), m.SigningPublicKey()) {
		t.Error("the symlinked signing key loaded as different material")
	}
}

// THE GENERATION RULE: material is minted only when ALL THREE files are absent.
// Any partial state is fatal and is never repaired by regenerating the missing
// file, because a new TLS key breaks every client pin and a new signing key
// breaks every peer pin.
func TestBusCertRefusesIncompleteMaterial(t *testing.T) {
	all := []string{buscert.CertFileName, buscert.TLSKeyFileName, buscert.SigningKeyFileName}
	for _, missing := range all {
		t.Run("missing "+missing, func(t *testing.T) {
			dir, _ := mint(t, buscert.Options{BusID: "bus-1"})
			path := filepath.Join(dir, missing)
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove: %v", err)
			}

			_, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: "bus-1"})
			if !errors.Is(err, buscert.ErrIncomplete) {
				t.Fatalf("LoadOrCreate error = %v, want ErrIncomplete", err)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(path)) {
				t.Errorf("the error does not name the missing file %s: %v", path, err)
			}
			for _, other := range all {
				if other == missing {
					continue
				}
				otherPath := filepath.Join(dir, other)
				if !bytes.Contains([]byte(err.Error()), []byte(otherPath)) {
					t.Errorf("the error does not name the surviving file %s: %v", otherPath, err)
				}
			}

			// Nothing was regenerated: the missing file is still missing and
			// the survivors are untouched.
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("%s was recreated; a missing file must NEVER be regenerated beside survivors", path)
			}
			if _, statErr := os.Stat(filepath.Join(dir, buscert.CertFileName)); missing != buscert.CertFileName && statErr != nil {
				t.Errorf("the surviving certificate was disturbed: %v", statErr)
			}
		})
	}

	// Two of three missing is the same rule.
	dir, _ := mint(t, buscert.Options{})
	for _, name := range []string{buscert.TLSKeyFileName, buscert.SigningKeyFileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}
	if _, err := buscert.LoadOrCreate(dir, buscert.Options{}); !errors.Is(err, buscert.ErrIncomplete) {
		t.Fatalf("LoadOrCreate error = %v, want ErrIncomplete", err)
	}
}

// An out-of-window certificate is refused, loudly and by name. The alternatives
// are a silent regeneration that breaks every pin, or a mystery handshake
// failure at every client.
func TestBusCertRefusesAnOutOfWindowCertificate(t *testing.T) {
	minted := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		now  time.Time
	}{
		{"expired", minted.Add(buscert.CertValidity).Add(time.Hour)},
		{"long expired", minted.Add(10 * 365 * 24 * time.Hour)},
		{"not yet valid", minted.Add(-24 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := mint(t, buscert.Options{BusID: "bus-1", Now: fixedNow(minted)})

			_, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: "bus-1", Now: fixedNow(tc.now)})
			if !errors.Is(err, buscert.ErrExpired) {
				t.Fatalf("LoadOrCreate at %s: error = %v, want ErrExpired", tc.now, err)
			}
			certPath := filepath.Join(dir, buscert.CertFileName)
			if !bytes.Contains([]byte(err.Error()), []byte(certPath)) {
				t.Errorf("the error does not name the certificate %s: %v", certPath, err)
			}
			// Refusing must not have re-minted anything.
			again, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: "bus-1", Now: fixedNow(minted.Add(time.Hour))})
			if err != nil {
				t.Fatalf("reload inside the window after a refusal: %v", err)
			}
			if again.Generated() {
				t.Error("the refused start regenerated material")
			}
		})
	}

	// Just inside each edge is accepted.
	dir, m := mint(t, buscert.Options{BusID: "bus-1", Now: fixedNow(minted)})
	for _, at := range []time.Time{m.Certificate().NotBefore, m.NotAfter()} {
		if _, err := buscert.LoadOrCreate(dir, buscert.Options{Now: fixedNow(at)}); err != nil {
			t.Errorf("LoadOrCreate at the boundary %s: %v", at, err)
		}
	}
}

// A generation that reaches the disk leaves no scratch behind.
func TestBusCertLeavesNoTemporaryFiles(t *testing.T) {
	dir, _ := mint(t, buscert.Options{BusID: "bus-1"})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the data directory: %v", err)
	}
	if len(entries) != 3 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the data directory holds %v, want exactly the three material files", names)
	}
}

// TestBusCertHandshakeVerifiesUnderStandardVerification is the highest-value
// gap: it proves the SANs and key usages the template sets are actually
// SUFFICIENT for a real Go client doing standard verification, not merely
// that the fields look plausible on the parsed struct.
//
// Verification is NEVER disabled here. tls.Config's skip-verification field is
// not set and must never appear anywhere in this tree, including in tests
// (DECISIONS.md, 2026-08-02, "E7: no plaintext escape hatch"); the client
// instead trusts the loaded leaf directly, exactly as a pinning client would.
// The field is not named in this comment on purpose -- client/transport.go
// makes the same choice for the same reason -- so that grepping the tree for
// its name finds only real uses rather than the rules forbidding them.
func TestBusCertHandshakeVerifiesUnderStandardVerification(t *testing.T) {
	_, m := mint(t, buscert.Options{})

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{m.TLSCertificate()},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	// httptest binds to 127.0.0.1, which is in the always-present loopback SAN
	// set, so the client's hostname check has something to verify against.
	pool := x509.NewCertPool()
	pool.AddCert(m.Certificate())
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("handshake against the bus certificate failed standard verification: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate observed by the client")
	}
	got := buscert.FingerprintOfDER(resp.TLS.PeerCertificates[0].Raw)
	if !got.Equal(m.Fingerprint()) {
		t.Errorf("the fingerprint the client observed (%s) does not equal Material.Fingerprint() (%s)", got, m.Fingerprint())
	}
}

// The signing key attests agent key bundles and must be a genuinely separate
// identity from the TLS key, not merely different bytes: a signature made
// with one must not verify under the other.
func TestBusCertSigningKeySignsAndTLSKeyCannotVerifyIt(t *testing.T) {
	_, m := mint(t, buscert.Options{})
	msg := []byte("an agent key bundle to attest")

	sig := ed25519.Sign(m.SigningPrivateKey(), msg)
	if !ed25519.Verify(m.SigningPublicKey(), msg, sig) {
		t.Fatal("a signature made with SigningPrivateKey does not verify under SigningPublicKey")
	}

	tlsPub, ok := m.Certificate().PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("certificate public key is %T, want ed25519.PublicKey", m.Certificate().PublicKey)
	}
	if ed25519.Verify(tlsPub, msg, sig) {
		t.Fatal("a signature made with the signing key verifies under the TLS certificate's public key -- the two identities are not independent")
	}
}

// Options.Hosts must land in the certificate's SANs alongside the always-on
// loopback set, and duplicates supplied by the caller must be collapsed
// rather than appearing twice.
func TestBusCertOptionsHostsPopulateSANsAndDeduplicate(t *testing.T) {
	_, m := mint(t, buscert.Options{
		Hosts: []string{"extra.example", "203.0.113.5", "localhost", "127.0.0.1", "extra.example"},
	})
	leaf := m.Certificate()

	if !containsString(leaf.DNSNames, "extra.example") {
		t.Errorf("DNSNames = %v, want it to contain the extra host %q", leaf.DNSNames, "extra.example")
	}
	if !containsIP(leaf.IPAddresses, "203.0.113.5") {
		t.Errorf("IPAddresses = %v, want it to contain the extra IP %q", leaf.IPAddresses, "203.0.113.5")
	}
	// The loopback set is always present, whatever the caller passed.
	for _, want := range []string{"localhost"} {
		if !containsString(leaf.DNSNames, want) {
			t.Errorf("DNSNames = %v, want it to still contain the loopback name %q", leaf.DNSNames, want)
		}
	}
	for _, want := range []string{"127.0.0.1", "::1"} {
		if !containsIP(leaf.IPAddresses, want) {
			t.Errorf("IPAddresses = %v, want it to still contain the loopback IP %q", leaf.IPAddresses, want)
		}
	}

	dnsCount := func(name string) int {
		n := 0
		for _, d := range leaf.DNSNames {
			if d == name {
				n++
			}
		}
		return n
	}
	ipCount := func(ip string) int {
		want := net.ParseIP(ip)
		n := 0
		for _, got := range leaf.IPAddresses {
			if got.Equal(want) {
				n++
			}
		}
		return n
	}
	if n := dnsCount("localhost"); n != 1 {
		t.Errorf("localhost appears %d times in DNSNames, want exactly 1 (deduplicated)", n)
	}
	if n := dnsCount("extra.example"); n != 1 {
		t.Errorf("extra.example appears %d times in DNSNames, want exactly 1 (caller supplied it twice)", n)
	}
	if n := ipCount("127.0.0.1"); n != 1 {
		t.Errorf("127.0.0.1 appears %d times in IPAddresses, want exactly 1 (deduplicated)", n)
	}
}

// ParseFingerprint and String must round trip, and Equal must be true only
// for identical fingerprints -- a single flipped bit is a different
// fingerprint, never a near miss.
func TestFingerprintRoundTripAndEqual(t *testing.T) {
	var raw [buscert.DigestSize]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("draw random fingerprint bytes: %v", err)
	}
	f := buscert.Fingerprint(raw)

	s := f.String()
	parsed, err := buscert.ParseFingerprint(s)
	if err != nil {
		t.Fatalf("ParseFingerprint(%q): %v", s, err)
	}
	if !parsed.Equal(f) {
		t.Errorf("ParseFingerprint(f.String()) did not round trip to an equal fingerprint")
	}
	if parsed.String() != s {
		t.Errorf("parsed.String() = %q, want %q", parsed.String(), s)
	}
	if !f.Equal(f) {
		t.Error("Equal(f, f) = false, want true")
	}

	flipped := f
	flipped[0] ^= 0x01
	if f.Equal(flipped) {
		t.Error("Equal is true for two fingerprints differing by a single bit")
	}
}

// A generation that fails partway must leave the data directory exactly as
// virgin as it found it: no partial file set, and no leftover *.tmp-* file,
// because a partial set left behind converts a transient write error into a
// permanent, operator-only ErrIncomplete.
func TestBusCertFailedGenerationLeavesDirectoryVirgin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not stop a write, so this test cannot force the failure it needs")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // let t.TempDir() clean up even if the test fails early

	if _, err := buscert.LoadOrCreate(dir, buscert.Options{}); err == nil {
		t.Fatal("LoadOrCreate succeeded against a read-only data directory, want an error")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the data directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a failed generation left %v behind, want a virgin directory", names)
	}
}

func readKey(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("%s does not hold a PEM PRIVATE KEY block", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("%s holds a %T, want an Ed25519 key", path, key)
	}
	return ed
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func containsIP(haystack []net.IP, needle string) bool {
	want := net.ParseIP(needle)
	for _, ip := range haystack {
		if ip.Equal(want) {
			return true
		}
	}
	return false
}

// TestBusCertRefusesASigningKeyIdenticalToTheTLSKey pins the one collapse of
// the two-key separation that generation cannot cause and tls.X509KeyPair
// cannot catch.
//
// X509KeyPair proves only that the TLS key matches the CERTIFICATE; it never
// looks at the signing key. So a restore or a copy that put one key under both
// names would load, serve, sign and peer perfectly happily with a single key
// doing both jobs -- silently turning a stolen TLS key (impersonates the bus to
// its clients) into a stolen signing key (forges attestations for every agent
// on the bus, and every peer believes them). That is precisely the outcome the
// separate-keys decision exists to prevent, so it is refused at load.
func TestBusCertRefusesASigningKeyIdenticalToTheTLSKey(t *testing.T) {
	dir, _ := mint(t, buscert.Options{})

	tlsKey, err := os.ReadFile(filepath.Join(dir, buscert.TLSKeyFileName))
	if err != nil {
		t.Fatalf("read the TLS key: %v", err)
	}
	// The operator mistake, exactly: one key written under both names.
	signingPath := filepath.Join(dir, buscert.SigningKeyFileName)
	if err := os.WriteFile(signingPath, tlsKey, buscert.KeyFileMode); err != nil {
		t.Fatalf("overwrite the signing key: %v", err)
	}

	_, err = buscert.LoadOrCreate(dir, buscert.Options{})
	if !errors.Is(err, buscert.ErrMalformed) {
		t.Fatalf("LoadOrCreate with the TLS key copied over the signing key: got %v, want ErrMalformed", err)
	}
	// The message must name BOTH files: an operator reading it has to know which
	// two paths are the same key, not merely that something is wrong.
	msg := err.Error()
	for _, want := range []string{buscert.SigningKeyFileName, buscert.TLSKeyFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not name %s: %s", want, msg)
		}
	}
}

// TestBusCertValidityIsTheDecidedPeriod pins the validity period to the value
// DECISIONS.md settled (MTLS-DESIGN: both the bus and the client certificate
// "default to 365 days when self-generated"), not to a number this package
// picked for itself. It shipped at 730 days once; this test is what stops that
// drifting back.
func TestBusCertValidityIsTheDecidedPeriod(t *testing.T) {
	const decided = 365 * 24 * time.Hour
	if buscert.CertValidity != decided {
		t.Fatalf("CertValidity = %v, want %v (DECISIONS.md, MTLS-DESIGN)", buscert.CertValidity, decided)
	}

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	_, m := mint(t, buscert.Options{Now: fixedNow(now)})

	if got, want := m.NotAfter().UTC(), now.Add(decided).UTC(); !got.Equal(want) {
		t.Errorf("NotAfter = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// NotBefore is backdated so a few seconds of clock skew between a fresh bus
	// and its first client is not a "certificate is not yet valid" handshake
	// failure, which is the most confusing way for a new deployment to fail.
	if !m.Certificate().NotBefore.Before(now) {
		t.Errorf("NotBefore = %s, want it backdated before %s", m.Certificate().NotBefore, now)
	}
}
