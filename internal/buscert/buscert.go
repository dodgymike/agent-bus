package buscert

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The three file names inside the data directory. They sit alongside
// wal-mac.key, which internal/wal owns; between them they are every long-lived
// secret this bus has.
const (
	// CertFileName holds one PEM CERTIFICATE block: the DER of the self-signed
	// leaf whose fingerprint clients pin.
	CertFileName = "bus-tls.crt"

	// TLSKeyFileName holds one PEM PRIVATE KEY block: the PKCS#8 encoding of the
	// Ed25519 private key inside the certificate. SECRET.
	TLSKeyFileName = "bus-tls.key"

	// SigningKeyFileName holds one PEM PRIVATE KEY block: the PKCS#8 encoding of
	// the Ed25519 private key that attests agent key bundles. SECRET, and a
	// DIFFERENT key from the TLS key (DECISIONS.md 2026-08-07).
	SigningKeyFileName = "bus-signing.key"
)

// KeyFileMode is the permission of BOTH key files. Anything that can read one
// of these files is the bus, to a client (TLS key) or to a peer bus (signing
// key), so nothing outside the owner may see them.
const KeyFileMode os.FileMode = 0o600

// CertFileMode is the permission of the certificate file. It is deliberately
// NOT 0600: the certificate is PUBLIC by construction — it is sent to every
// client in every handshake, and its fingerprint is published in invite blobs —
// so its mode is not security-relevant. Insisting on 0600 would buy nothing and
// would collide with an operator-supplied certificate mounted 0644 (E7).
const CertFileMode os.FileMode = 0o644

// CertValidity is how long a freshly minted bus certificate is valid for: two
// years.
//
// The tension is worth stating plainly rather than hiding behind a constant.
// There is no rotation machinery yet (E3's two-certificates rollover is a
// separate, unwritten task), so this expiry is a SCHEDULED OUTAGE: the day it
// passes, the bus refuses to start and every client's pin is stale. Two years
// is chosen to be long enough that rotation certainly lands first, and short
// enough that a key does not live forever. The only mitigation available today
// is that Material.NotAfter is exported, so a startup log line can report the
// remaining life while there is still time to act on it.
const CertValidity = 730 * 24 * time.Hour

// clockSkewAllowance backdates NotBefore. A bus and its client disagreeing by a
// few seconds at first start must not turn into a "certificate is not yet
// valid" handshake failure, which is the single most confusing way for a fresh
// deployment to fail.
const clockSkewAllowance = 5 * time.Minute

// maxKeyFileSize caps how many bytes a key or certificate file may hold. A PEM
// Ed25519 key is about 120 bytes and a self-signed leaf a few hundred; 64 KiB
// is far beyond any legitimate file and stops a hostile or accidental
// enormous file being read into memory before it is rejected.
const maxKeyFileSize = 64 << 10

// Sentinel errors. All are checkable with errors.Is, and every concrete error
// naming one also names the offending PATH — the first question asked of a bus
// that will not start over its key material is always "which file".
//
// Every one of these is FATAL. Nothing in this package ever falls back,
// regenerates, repairs or downgrades: see LoadOrCreate for why.
var (
	// ErrIncomplete reports that SOME of the three files exist and some do not.
	// The bus generates material only on a virgin directory, so this is the
	// state that must be resolved by an operator.
	ErrIncomplete = errors.New("buscert: the bus key material is incomplete")

	// ErrPermissions reports a key file readable by group or other. It applies
	// to the two KEY files only; the certificate is public.
	ErrPermissions = errors.New("buscert: a bus key file has loose permissions")

	// ErrMalformed reports a file that exists but cannot be used: not a regular
	// file, unreadable, not PEM, the wrong PEM block type, not an Ed25519 key,
	// or a private key that does not match the certificate's public key.
	//
	// "not a regular file" deliberately does NOT get a sentinel of its own. A
	// directory or a device node where a key file belongs is the same class of
	// problem as a truncated PEM — something other than the key is at that path
	// — and the remedy is identical: put the real file there. A second sentinel
	// would be one more thing for a caller to have to remember to check.
	ErrMalformed = errors.New("buscert: a bus key or certificate file is malformed")

	// ErrExpired reports a certificate outside its validity window at
	// Options.Now — expired, or not yet valid.
	ErrExpired = errors.New("buscert: the bus certificate is outside its validity window")
)

// certErr is the concrete error for every problem here. errors.Is matches the
// sentinel; Unwrap still reaches the underlying I/O or parse cause.
type certErr struct {
	sentinel error
	msg      string
	cause    error
}

func (e *certErr) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *certErr) Is(target error) bool { return target == e.sentinel }

func (e *certErr) Unwrap() error { return e.cause }

// Options configures LoadOrCreate. Every field affects GENERATION only; loading
// existing material reads what is on disk and ignores BusID and Hosts entirely,
// because the certificate on disk is the one clients have already pinned and
// nothing here may quietly re-mint it to match new options.
type Options struct {
	// BusID becomes the certificate's Subject.CommonName when non-empty. It is
	// DESCRIPTIVE ONLY — see generateCertificate.
	BusID string

	// Hosts are additional subject alternative names: DNS names or IP literals.
	// The loopback set is always included.
	Hosts []string

	// Now supplies the current time. Nil means time.Now. It exists so a test can
	// drive the validity window, and it is the ONLY hook in this package: there
	// is no plaintext mode, no skip-verification switch and no
	// generate-anyway override, by decision (E7, 2026-08-02).
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// Material is the bus's loaded identity: the certificate it presents, the
// private key inside it, and the separate signing key. Fields are unexported so
// that the invariants established at load — the key matches the certificate,
// the certificate is in date, the fingerprint is over cert.Raw — cannot be
// broken by assignment afterwards.
type Material struct {
	tlsCert     tls.Certificate
	leaf        *x509.Certificate
	fingerprint Fingerprint
	signingKey  ed25519.PrivateKey

	certPath       string
	tlsKeyPath     string
	signingKeyPath string

	generated bool
}

// TLSCertificate returns the certificate/key pair, ready to drop into
// tls.Config.Certificates. Its Leaf is populated, so a handshake does not
// re-parse the DER on every connection.
func (m *Material) TLSCertificate() tls.Certificate { return m.tlsCert }

// Certificate returns the parsed leaf. Callers must treat it as read-only; it
// is shared with the tls.Certificate above.
func (m *Material) Certificate() *x509.Certificate { return m.leaf }

// Fingerprint returns sha256.Sum256(leaf.Raw) — the value a client pins, and
// the value that goes into an invite blob.
func (m *Material) Fingerprint() Fingerprint { return m.fingerprint }

// SigningPublicKey returns the PUBLIC half of the signing key: what a peer bus
// pins at peering time so it can verify this bus's attestations.
//
// The returned slice aliases the loaded key. Do not modify it.
func (m *Material) SigningPublicKey() ed25519.PublicKey {
	return m.signingKey.Public().(ed25519.PublicKey)
}

// SigningPrivateKey returns the signing key. It is SECRET: it must never be
// logged, serialised into a response, or written anywhere but its own file.
//
// The returned slice aliases the loaded key. Do not modify it.
func (m *Material) SigningPrivateKey() ed25519.PrivateKey { return m.signingKey }

// NotAfter is when the certificate stops being valid. Exported so startup can
// log the remaining life: with no rotation machinery yet, that log line is the
// only warning an operator gets before expiry becomes an outage.
func (m *Material) NotAfter() time.Time { return m.leaf.NotAfter }

// Generated reports whether THIS call minted fresh material. It is true exactly
// once in the life of a data directory, and the caller should log it loudly:
// generation means the fingerprint every client must pin has just changed, and
// on a directory that was supposed to already hold material it means the
// material was lost.
func (m *Material) Generated() bool { return m.generated }

// CertPath is the absolute path of the certificate file, for log lines.
func (m *Material) CertPath() string { return m.certPath }

// TLSKeyPath is the absolute path of the TLS key file, for log lines. The path
// is safe to log; the contents are not.
func (m *Material) TLSKeyPath() string { return m.tlsKeyPath }

// SigningKeyPath is the absolute path of the signing key file, for log lines.
// The path is safe to log; the contents are not.
func (m *Material) SigningKeyPath() string { return m.signingKeyPath }

// LoadOrCreate loads the bus's key material from dir, generating it only on a
// virgin directory.
//
// # THE GENERATION RULE
//
//	Material is generated when, and only when, ALL THREE files are ABSENT.
//	Any other partial state is FATAL (ErrIncomplete) and names which files are
//	present and which are missing. A missing file is NEVER regenerated beside
//	surviving ones.
//
// That rule is the load-bearing one here, and the reason is not tidiness.
// Minting a new TLS key silently breaks EVERY CLIENT that pinned the old
// fingerprint — and because there is no TOFU (E6), those clients do not
// "re-learn" it, they simply fail to connect, and the failure looks exactly
// like the attack the pinning exists to detect. Minting a new signing key is
// worse: it invalidates the pins held by every PEER BUS, which is a
// federation-wide event (DECISIONS.md 2026-08-07). Neither is a thing a process
// may decide to do because a file was not where it expected.
//
// A crash midway through a first generation lands in the same place, and the
// error says so: if this WAS a failed first start, the remedy is for an
// operator to remove the named files and restart. Removing them is a deliberate
// human act with a known consequence, and it is never something the bus does
// on anyone's behalf.
//
// # Everything else that is fatal
//
//   - A key file with any group or other permission bit set (ErrPermissions).
//     internal/wal's MAC key does NOT make this check; this package is
//     deliberately stricter, because these keys are the bus's identity to the
//     whole federation and a world-readable one is a compromise that has
//     already happened rather than a risk. The mode is read from the
//     ALREADY-OPEN file, so there is no window between the check and the read
//     in which the file could be swapped.
//   - A key file that is not a regular file — a directory, a device — caught by
//     the same stat, as ErrMalformed. Symlinks are NOT rejected: an operator
//     secret mount is legitimately a symlink, and the stat follows it to the
//     real file, whose mode is the one that matters.
//   - Malformed or truncated PEM, the wrong PEM block type, a key that is not
//     Ed25519, or a private key that does not match the certificate
//     (ErrMalformed). The key/certificate match is checked by tls.X509KeyPair,
//     which is the stdlib's own comparison; this package does not re-implement
//     it.
//   - A certificate that is expired or not yet valid at opts.Now (ErrExpired).
//     Refusing to start is the least bad of the three options: silently
//     regenerating would break every pin, and starting anyway would produce a
//     mystery handshake failure at every client with nothing in this bus's logs
//     to explain it. Refusing names the file and the date. Rotation (E3) is a
//     separate task that does not exist yet.
func LoadOrCreate(dir string, opts Options) (*Material, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("buscert: resolve the data directory %s: %w", dir, err)
	}
	certPath := filepath.Join(absDir, CertFileName)
	tlsKeyPath := filepath.Join(absDir, TLSKeyFileName)
	signingKeyPath := filepath.Join(absDir, SigningKeyFileName)
	paths := []string{certPath, tlsKeyPath, signingKeyPath}

	var present, missing []string
	for _, p := range paths {
		exists, err := fileExists(p)
		if err != nil {
			return nil, err
		}
		if exists {
			present = append(present, p)
		} else {
			missing = append(missing, p)
		}
	}

	switch len(present) {
	case len(paths):
		return load(certPath, tlsKeyPath, signingKeyPath, opts, false)
	case 0:
		if err := generate(absDir, certPath, tlsKeyPath, signingKeyPath, opts); err != nil {
			return nil, err
		}
		return load(certPath, tlsKeyPath, signingKeyPath, opts, true)
	default:
		return nil, &certErr{sentinel: ErrIncomplete,
			msg: fmt.Sprintf("buscert: the bus key material in %s is incomplete. MISSING: %s. PRESENT: %s. A missing file is NEVER regenerated beside surviving ones -- a new TLS key breaks every client that pinned the old certificate fingerprint, and a new signing key invalidates the pin held by every peer bus, which is a federation-wide event. Restore the missing file from backup; or, if this was a FAILED FIRST START and this bus has never served a client or been peered with, remove %s BY HAND and restart to mint a fresh set. The bus will not remove them for you.",
				absDir, strings.Join(missing, ", "), strings.Join(present, ", "), strings.Join(present, " and "))}
	}
}

// fileExists reports whether path exists.
//
// A stat that fails for any reason OTHER than "not there" is an ERROR, never
// "absent". "I could not look at the file" must not be read as "the file is not
// there", because the difference between those two answers here is the
// difference between refusing to start and generating a new identity over the
// top of the old one.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, &certErr{sentinel: ErrMalformed,
		msg:   fmt.Sprintf("buscert: %s cannot be examined, and an unreadable file is NOT treated as an absent one", path),
		cause: err}
}

// load reads all three files and validates them against each other.
func load(certPath, tlsKeyPath, signingKeyPath string, opts Options, generated bool) (*Material, error) {
	certPEM, err := readFileStrict(certPath, false)
	if err != nil {
		return nil, err
	}
	tlsKeyPEM, err := readFileStrict(tlsKeyPath, true)
	if err != nil {
		return nil, err
	}
	signingKeyPEM, err := readFileStrict(signingKeyPath, true)
	if err != nil {
		return nil, err
	}

	// Decode both files ourselves first, so a malformed input is reported as
	// "this file, this problem" rather than as one of tls.X509KeyPair's generic
	// messages. X509KeyPair is still what performs the key/certificate MATCH.
	certDER, err := decodeSinglePEM(certPath, certPEM, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	if _, err := decodeEd25519Key(tlsKeyPath, tlsKeyPEM); err != nil {
		return nil, err
	}
	signingKey, err := decodeEd25519Key(signingKeyPath, signingKeyPEM)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg:   fmt.Sprintf("buscert: %s does not hold a parseable X.509 certificate", certPath),
			cause: err}
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: the certificate in %s carries a %T public key, want ed25519.PublicKey", certPath, leaf.PublicKey)}
	}

	// tls.X509KeyPair is the stdlib's own check that the private key belongs to
	// the certificate. Re-implementing that comparison would be a second, and
	// therefore possibly divergent, answer to a question the stdlib already
	// answers correctly.
	tlsCert, err := tls.X509KeyPair(certPEM, tlsKeyPEM)
	if err != nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg:   fmt.Sprintf("buscert: the private key in %s does not go with the certificate in %s", tlsKeyPath, certPath),
			cause: err}
	}
	tlsCert.Leaf = leaf

	now := opts.now()
	if now.Before(leaf.NotBefore) {
		return nil, &certErr{sentinel: ErrExpired,
			msg: fmt.Sprintf("buscert: the certificate in %s is not valid until %s, and it is now %s. The bus refuses to start rather than regenerate (which would break every client that pinned this certificate) or start anyway (which would fail every handshake with nothing here to explain it). Check the system clock.",
				certPath, leaf.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))}
	}
	if now.After(leaf.NotAfter) {
		return nil, &certErr{sentinel: ErrExpired,
			msg: fmt.Sprintf("buscert: the certificate in %s expired at %s, and it is now %s. The bus refuses to start rather than regenerate (which would break every client that pinned this certificate) or start anyway (which would fail every handshake with nothing here to explain it). Rotation is not implemented yet: replace %s and %s together, and re-issue invites carrying the new fingerprint.",
				certPath, leaf.NotAfter.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), certPath, tlsKeyPath)}
	}

	return &Material{
		tlsCert:        tlsCert,
		leaf:           leaf,
		fingerprint:    FingerprintOf(leaf),
		signingKey:     signingKey,
		certPath:       certPath,
		tlsKeyPath:     tlsKeyPath,
		signingKeyPath: signingKeyPath,
		generated:      generated,
	}, nil
}

// readFileStrict opens path, checks what it actually is, and reads it.
//
// The mode and file-type checks are made with Stat on the ALREADY-OPEN
// descriptor rather than with os.Stat on the path, so there is no window in
// which the file that was checked and the file that is read could be different
// files.
func readFileStrict(path string, secret bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg:   fmt.Sprintf("buscert: %s cannot be opened", path),
			cause: err}
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg:   fmt.Sprintf("buscert: %s cannot be examined", path),
			cause: err}
	}
	// A symlink never reaches here as a symlink: Open follows it, and Stat on
	// the descriptor describes the real file. That is deliberate -- an operator
	// secret mount is legitimately a symlink, and the mode that matters is the
	// target's.
	//
	// The one shape this cannot report is a FIFO, because opening one for
	// reading blocks until a writer appears; that manifests as a hung start
	// rather than a wrong answer, and detecting it would need a non-blocking
	// open from package syscall.
	if !fi.Mode().IsRegular() {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s is not a regular file (mode %s)", path, fi.Mode())}
	}
	if secret && fi.Mode().Perm()&0o077 != 0 {
		return nil, &certErr{sentinel: ErrPermissions,
			msg: fmt.Sprintf("buscert: the key file %s has mode %#o, which is readable beyond its owner; want %#o. Anything that can read this file IS this bus to whoever pinned it. Fix it with: chmod %#o %s",
				path, fi.Mode().Perm(), KeyFileMode.Perm(), KeyFileMode.Perm(), path)}
	}
	if fi.Size() > maxKeyFileSize {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s is %d bytes, which is far larger than any certificate or key this package writes (limit %d)", path, fi.Size(), maxKeyFileSize)}
	}

	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg:   fmt.Sprintf("buscert: %s cannot be read", path),
			cause: err}
	}
	if len(b) > maxKeyFileSize {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s is larger than the %d byte limit", path, maxKeyFileSize)}
	}
	return b, nil
}

// decodeSinglePEM returns the DER inside a file holding EXACTLY ONE PEM block
// of the expected type.
//
// A second block is refused rather than ignored. For the certificate that would
// be a chain, and there is no CA in this design (E6) -- so a chain here means
// somebody expected a trust model this bus does not implement, and silently
// using only the first block would make "the bus certificate", and therefore
// the fingerprint clients pin, ambiguous.
func decodeSinglePEM(path string, data []byte, want string) ([]byte, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s does not hold a PEM %s block (it may be truncated, or not PEM at all)", path, want)}
	}
	if block.Type != want {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s holds a PEM %q block, want %q", path, block.Type, want)}
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s holds more than one PEM block; exactly one %s is expected (this design has no certificate chain and no CA)", path, want)}
	}
	return block.Bytes, nil
}

// decodeEd25519Key parses a PKCS#8 PEM private key file and insists it is
// Ed25519. An RSA or ECDSA key is refused rather than accommodated: invariant 9
// says use the audited high-level primitive, and here that is crypto/ed25519 --
// one algorithm, no negotiation, nothing to downgrade.
func decodeEd25519Key(path string, data []byte) (ed25519.PrivateKey, error) {
	der, err := decodeSinglePEM(path, data, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, &certErr{sentinel: ErrMalformed,
			msg:   fmt.Sprintf("buscert: %s does not hold a parseable PKCS#8 private key", path),
			cause: err}
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s holds a %T private key, want an Ed25519 key", path, key)}
	}
	if len(ed) != ed25519.PrivateKeySize {
		return nil, &certErr{sentinel: ErrMalformed,
			msg: fmt.Sprintf("buscert: %s holds a %d byte Ed25519 private key, want %d", path, len(ed), ed25519.PrivateKeySize)}
	}
	return ed, nil
}

// generate mints a fresh set of material and writes all three files durably.
//
// It is called only when all three files are absent (see LoadOrCreate).
func generate(dir, certPath, tlsKeyPath, signingKeyPath string, opts Options) error {
	// TWO independent keypairs. The signing key is NEVER derived from the TLS
	// key, and vice versa; that independence is the whole point of the decision
	// to keep them separate, and a derivation would quietly re-couple the two
	// rotations it exists to decouple.
	tlsPub, tlsPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("buscert: generate the bus TLS key for %s: %w", tlsKeyPath, err)
	}
	_, signingPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("buscert: generate the bus signing key for %s: %w", signingKeyPath, err)
	}

	certDER, err := generateCertificate(tlsPub, tlsPriv, opts)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	tlsKeyPEM, err := marshalKey(tlsPriv, tlsKeyPath)
	if err != nil {
		return err
	}
	signingKeyPEM, err := marshalKey(signingPriv, signingKeyPath)
	if err != nil {
		return err
	}

	return writeAll(dir, []fileSpec{
		{path: certPath, data: certPEM, mode: CertFileMode},
		{path: tlsKeyPath, data: tlsKeyPEM, mode: KeyFileMode},
		{path: signingKeyPath, data: signingKeyPEM, mode: KeyFileMode},
	})
}

// marshalKey PEM-encodes a private key as PKCS#8, the one encoding this package
// writes and reads.
func marshalKey(key ed25519.PrivateKey, path string) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("buscert: encode the private key for %s: %w", path, err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// generateCertificate builds the self-signed leaf.
//
// SELF-SIGNED, with the template as its own parent: there is no CA in this
// design (E6) and there is not going to be one. IsCA is set because the
// certificate signs ITSELF and Go's verifier wants the basic constraint to
// agree with that; it does not mean this certificate issues others, and nothing
// in this system will ever ask it to.
//
// The CommonName is DESCRIPTIVE ONLY. Nothing authenticates on it. Clients pin
// the fingerprint, and the bus id is minted by the server (invariant 1) -- a
// name written into a certificate is not an identity, and treating it as one is
// exactly the mistake the fingerprint pin exists to avoid.
//
// SANs are REQUIRED, not optional garnish: Go removed the CommonName fallback
// in 1.15, so a certificate without subject alternative names is unusable by
// any Go client doing standard verification. The loopback set is always
// present, because the default bind is loopback (invariant 11, E8).
func generateCertificate(pub ed25519.PublicKey, priv ed25519.PrivateKey, opts Options) ([]byte, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}

	cn := opts.BusID
	if cn == "" {
		cn = "agent-bus (self-signed, descriptive only)"
	}

	dnsNames, ipAddresses := subjectAltNames(opts.Hosts)
	now := opts.now()

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-clockSkewAllowance),
		NotAfter:              now.Add(CertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}
	// ClientAuth is present because this same certificate is what the bus
	// PRESENTS when it dials a PEER bus. One identity, both directions.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("buscert: create the self-signed bus certificate: %w", err)
	}
	return der, nil
}

// newSerial draws a random positive 128-bit serial number -- the standard
// construction for a certificate with no issuing CA to allocate one.
//
// A zero serial is drawn again rather than shipped: zero is not a positive
// integer, and some verifiers reject it.
func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for attempt := 0; attempt < 8; attempt++ {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, fmt.Errorf("buscert: draw a certificate serial number: %w", err)
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
	// Eight consecutive zeroes out of 2^128 is not luck, it is a broken
	// random source, and continuing on a broken random source is how keys get
	// generated that anybody can reproduce.
	return nil, errors.New("buscert: crypto/rand returned a zero certificate serial eight times; the random source is not working")
}

// subjectAltNames splits hosts into DNS names and IP addresses, always
// including the loopback set, and de-duplicates both lists while preserving
// order.
func subjectAltNames(hosts []string) ([]string, []net.IP) {
	var dnsNames []string
	var ipAddresses []net.IP
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}

	// The loopback defaults come first so a certificate is always usable by a
	// bus on its default (loopback) bind, whatever the operator passed.
	for _, host := range append([]string{"localhost", "127.0.0.1", "::1"}, hosts...) {
		if host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			if key := ip.String(); !seenIP[key] {
				seenIP[key] = true
				ipAddresses = append(ipAddresses, ip)
			}
			continue
		}
		if !seenDNS[host] {
			seenDNS[host] = true
			dnsNames = append(dnsNames, host)
		}
	}
	return dnsNames, ipAddresses
}

// fileSpec is one file to write atomically.
type fileSpec struct {
	path string
	data []byte
	mode os.FileMode
}

// writeAll writes every file via a same-directory temp file and a rename, then
// fsyncs the directory ONCE.
//
// The discipline is internal/wal's, for internal/wal's reason: a partial write
// must never be observable as a valid file. Each temp is created 0600 and
// chmodded to its final mode before the rename, so a key file is never briefly
// world-readable and a rename never publishes a half-written file.
//
// On ANY failure every temp AND every file already renamed into place by THIS
// call is removed, so a failed generation leaves the directory exactly as
// virgin as it found it. That is the one deletion this package performs, and it
// is safe precisely because these are files nothing has ever read: leaving a
// partial set behind would instead convert a transient write error into a
// permanent ErrIncomplete that needs an operator's hands.
func writeAll(dir string, files []fileSpec) error {
	var temps, renamed []string
	cleanup := func() {
		for _, p := range temps {
			os.Remove(p)
		}
		for _, p := range renamed {
			os.Remove(p)
		}
		// Best effort: the caller is already returning an error, and a failed
		// fsync of the directory cannot make the removals less true.
		_ = syncDir(dir)
	}

	for _, spec := range files {
		tmp, err := os.CreateTemp(dir, filepath.Base(spec.path)+".tmp-*")
		if err != nil {
			cleanup()
			return fmt.Errorf("buscert: create a temporary file for %s: %w", spec.path, err)
		}
		tmpPath := tmp.Name()
		temps = append(temps, tmpPath)

		if err := tmp.Chmod(spec.mode); err != nil {
			tmp.Close()
			cleanup()
			return fmt.Errorf("buscert: set mode %#o on the temporary file for %s: %w", spec.mode.Perm(), spec.path, err)
		}
		if n, err := tmp.Write(spec.data); err != nil || n != len(spec.data) {
			tmp.Close()
			cleanup()
			return fmt.Errorf("buscert: write %s: %w", spec.path, shortWrite(err, n, len(spec.data)))
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			cleanup()
			return fmt.Errorf("buscert: fsync %s: %w", spec.path, err)
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return fmt.Errorf("buscert: close %s: %w", spec.path, err)
		}
		if err := os.Rename(tmpPath, spec.path); err != nil {
			cleanup()
			return fmt.Errorf("buscert: rename into place %s: %w", spec.path, err)
		}
		temps = temps[:len(temps)-1] // the temp is gone; the final name owns it now
		renamed = append(renamed, spec.path)
	}

	// One fsync of the directory, AFTER every rename: the contents were made
	// durable file by file, and this is what makes the NAMES durable. Without
	// it a crash here could leave a key whose bytes survived but whose directory
	// entry did not -- which is the ErrIncomplete state, arrived at by accident.
	if err := syncDir(dir); err != nil {
		cleanup()
		return fmt.Errorf("buscert: fsync the data directory %s: %w", dir, err)
	}
	return nil
}

// syncDir fsyncs a directory so that a file created in it is durably named.
// It is a copy of internal/wal's helper, which is unexported there; the
// duplication is three lines and is preferable to widening that package's API.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// shortWrite turns a partial write into an error, so a write that returns
// (n < len, nil) is never mistaken for success.
func shortWrite(err error, n, want int) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: wrote %d of %d bytes", io.ErrShortWrite, n, want)
}
