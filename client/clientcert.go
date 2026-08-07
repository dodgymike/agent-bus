package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// The agent's own TLS identity: the half of mutual TLS this end owns.
//
// # What this is for
//
// Invariant 11 makes TLS MUTUAL: both ends present a certificate and both
// verify. client/pin.go is the verifying half — it decides whether the BUS is
// the one the invite named. This file is the presenting half — it mints, keeps
// and offers the certificate the BUS will one day check.
//
// "One day" is the important word. The bus serves TLS with client certificates
// NOT REQUESTED today (MTLS-CLIENTAUTH is the task that starts asking). This
// lands first, and deliberately: the server-side requirement must never arrive
// before the client can satisfy it. A bus that demands a client certificate
// before any client can present one locks out every agent already enrolled,
// which is the same failure this project has already shipped once — signatures
// were required on send before the client could produce them, and every send
// returned exit 7. So the order is: capability, then enforcement.
//
// The consequence is that this certificate is, for now, offered and ignored.
// That is the intended state, and it is why nothing here fails when the bus
// does not ask.
//
// # What it is NOT
//
// It is not an identity in itself. Invariant 1 says the SERVER mints ids; a
// name inside a certificate is a string the holder chose. The bus will bind
// this certificate's FINGERPRINT to the server-minted agent id at enrolment
// (MTLS-BIND), and from then on the fingerprint is what identifies it. Nothing
// should ever read the Subject and conclude anything.
//
// # Why it exists before the agent id does
//
// The certificate has to be presentable on the ENROLMENT connection, and at
// that moment the agent has no id — the bus has not minted one yet. So the
// material cannot be named after an identity, and it is stored once per
// CREDENTIAL STORE rather than once per identity. Two identities enrolled from
// the same store therefore share one client certificate. That is a real
// limitation for MTLS-CROSSCHECK (which wants to reject a session token
// presented over a connection belonging to a different agent), and it is
// recorded as a follow-up rather than papered over here: solving it needs a
// second certificate minted AFTER enrolment and a rebind flow, which is not
// this task.

// ClientTLSDirName is the directory, inside the credential store directory,
// holding this agent's own TLS material. One directory rather than two loose
// files so that creation is a single atomic rename — see createClientTLS.
const ClientTLSDirName = "client-tls"

// The two files inside ClientTLSDirName. PEM, and the same encodings the bus
// writes for its own material (PKCS#8 "PRIVATE KEY", "CERTIFICATE"), so an
// operator debugging a handshake can point `openssl x509` at either end
// without learning a second format.
const (
	ClientCertFileName = "cert.pem"
	ClientKeyFileName  = "key.pem"
)

// Both files are 0600 inside a 0700 directory.
//
// The key is 0600 for the obvious reason. The CERTIFICATE is 0600 too, which
// differs from the bus's own certificate (0644) and the difference is
// deliberate rather than an oversight: the bus certificate is 0644 because it
// is MEANT to be read and copied — its fingerprint goes into every invite. This
// one is not published to anybody; it is handed to the bus during a handshake.
// Nothing is gained by making it world-readable, and the enclosing directory is
// 0700 regardless, so 0600 costs nothing and keeps one rule for the whole
// directory.
const (
	clientTLSDirMode  fs.FileMode = 0o700
	clientTLSFileMode fs.FileMode = 0o600
)

// ClientCertValidity is how long a freshly minted client certificate is valid,
// matching the bus's own certificate lifetime (DECISIONS.md, 2026-08-02: a
// leak-containment bound, not an expiry policy anybody wants to hit).
//
// Renewal is NOT automatic — see LoadOrCreateClientCertificate.
const ClientCertValidity = 365 * 24 * time.Hour

// clientCertClockSkewAllowance backdates NotBefore so a client whose clock is
// slightly ahead of the bus's does not present a certificate that is not yet
// valid. Same allowance the bus uses for its own.
const clientCertClockSkewAllowance = 5 * time.Minute

// maxClientCertFileBytes bounds a PEM file before it is read. A PEM certificate
// and an Ed25519 PKCS#8 key are together well under 2 KiB; 64 KiB is ample
// headroom and still finite, so a file that has been replaced with something
// enormous is refused rather than read into memory.
const maxClientCertFileBytes = 64 << 10

// ErrClientCertIncomplete reports that the client TLS directory holds some but
// not all of its files.
//
// It is FATAL rather than self-healing, and that is the whole point. The
// missing piece cannot be regenerated: a new certificate over the surviving key
// would have a different serial and different validity dates, and therefore a
// DIFFERENT FINGERPRINT — which is the value the bus binds to the agent id.
// Silently minting a replacement would look like a repair and would in fact
// revoke the agent's TLS identity. So it stops, and says which file is missing.
var ErrClientCertIncomplete = errors.New("client: the client TLS directory is incomplete")

// ClientCertificate is this agent's own TLS material: the certificate it
// presents, and the private key it proves possession of it with.
//
// The private key NEVER leaves this machine and is not exposed by this type.
// It exists only inside the tls.Certificate handed to crypto/tls, exactly as
// the enrolment auth key exists only inside the credential store.
type ClientCertificate struct {
	// Dir is the directory holding the material, CertPath and KeyPath the two
	// files in it. Reported so an operator can find, inspect and — deliberately
	// — retire them.
	Dir      string
	CertPath string
	KeyPath  string

	// Leaf is the parsed certificate. Public information by definition: this is
	// what is sent on the wire during a handshake.
	Leaf *x509.Certificate

	// Created reports whether this call MINTED the material, as opposed to
	// loading material that was already there. It is the honest answer to "did
	// running this change anything", which matters because minting a new
	// certificate invalidates any binding the bus already holds.
	Created bool

	// Warnings are conditions the operator should be told about but which do
	// not stop the client — a key file found with loose permissions and
	// tightened, for instance. Surfaced on stderr by the CLI, never on stdout.
	Warnings []string

	// cert is what crypto/tls is handed. Unexported: the private key inside it
	// is the one thing this package must not casually hand out, and no caller
	// outside this package has needed it. MTLS-CLIENTAUTH may export an
	// accessor if a genuine embedder case appears; adding one later is cheap,
	// un-exporting a key is not.
	cert tls.Certificate
}

// Fingerprint is the SHA-256 of the certificate's DER, lowercase hex.
//
// Deliberately the SAME construction as BusFingerprint, so that "the
// fingerprint of a certificate" has exactly one spelling in this system
// whichever end holds it. When the bus starts recording which certificate an
// agent enrolled with (MTLS-BIND), this is the string it records.
func (c *ClientCertificate) Fingerprint() string {
	if c == nil || c.Leaf == nil {
		return ""
	}
	sum := sha256.Sum256(c.Leaf.Raw)
	return hex.EncodeToString(sum[:])
}

// IsExpired reports whether the certificate's validity window excludes at.
//
// An expired certificate is REPORTED, not refused. Refusing to load one would
// brick every command on the day it expires — including the ones an operator
// would use to fix it — while the bus does not even ask for a client
// certificate yet. The honest behaviour is to keep working, present it, and
// let `agent-busctl client-cert` say plainly that it needs replacing.
func (c *ClientCertificate) IsExpired(at time.Time) bool {
	if c == nil || c.Leaf == nil {
		return false
	}
	return at.Before(c.Leaf.NotBefore) || at.After(c.Leaf.NotAfter)
}

// certificate returns the material in the shape crypto/tls wants.
func (c *ClientCertificate) certificate() *tls.Certificate {
	if c == nil {
		return nil
	}
	return &c.cert
}

// LoadOrCreateClientCertificate returns this agent's TLS material from
// identityDir, minting it on first use.
//
// # Idempotent, and never destructive
//
// Called twice it returns the same certificate twice. Existing material is
// NEVER overwritten: there is no path through this function that replaces a key
// or a certificate that is already on disk. That is invariant 10's spirit
// applied to key material — the bus may already have bound this certificate's
// fingerprint to a server-minted agent id, and a silent regeneration would
// revoke that binding while looking like a no-op.
//
// It follows that RENEWAL IS A DELIBERATE MANUAL ACT today: an expired
// certificate is loaded, reported as expired, and left alone. Automatic renewal
// needs a rebind conversation with the bus that does not exist yet.
//
// # Concurrency and crash safety
//
// Two processes sharing a credential store may reach this at the same moment on
// a fresh machine — an agent running `watch` and `send` in parallel is the
// ordinary case, not an exotic one. Creation therefore writes both files into a
// TEMPORARY DIRECTORY and installs them with a single rename. A rename of a
// directory onto an existing non-empty directory fails, so exactly one process
// wins and the loser discards its material and loads the winner's. There is no
// lock file, and — because the unit of installation is the whole directory —
// no window in which a crash leaves one file without the other.
//
// # Failing closed
//
// Every failure here is returned, never swallowed. A client that cannot mint or
// read its certificate does not quietly continue without one: it stops, and the
// error names the directory and the remedy. The alternative — carrying on
// unauthenticated — is precisely the shape of failure that looks fine until the
// day the bus starts checking.
func LoadOrCreateClientCertificate(identityDir string) (*ClientCertificate, error) {
	return loadOrCreateClientCertificate(identityDir, time.Now())
}

// loadOrCreateClientCertificate is LoadOrCreateClientCertificate with the clock
// injected, so a test can mint a certificate at a chosen moment without a
// mutable package-level variable shared by every other test.
func loadOrCreateClientCertificate(identityDir string, now time.Time) (*ClientCertificate, error) {
	if identityDir == "" {
		return nil, newError(KindConfig, "client-cert",
			"no credential store directory",
			"pass --identity <dir> or set "+EnvIdentityDir)
	}
	dir := filepath.Join(identityDir, ClientTLSDirName)

	cc, err := loadClientCertificate(dir)
	if err == nil {
		return cc, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		// Includes ErrClientCertIncomplete and every parse failure. Both mean
		// "there is material here and it is not usable", which is never an
		// invitation to mint over the top of it.
		return nil, err
	}

	minted, err := createClientTLS(identityDir, dir, now)
	if err != nil {
		return nil, err
	}

	cc, err = loadClientCertificate(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The material was installed (or another process installed it) and
			// has vanished again between then and now. Vanishingly unlikely,
			// but it must not escape as the bare fs.ErrNotExist sentinel the
			// load path uses INTERNALLY to mean "nothing here yet, go mint": to
			// a caller that is not a *client.Error, so it carries no Kind, no
			// Remedy and exit code 1 rather than 3.
			return nil, wrapError(KindConfig, "client-cert",
				"the client TLS material at "+dir+" disappeared while it was being set up",
				"re-run the command; if it keeps happening, something else is deleting "+dir, err)
		}
		return nil, err
	}
	// Created reports whether THIS call installed the material, which is not
	// the same as "this call reached the creation path". A caller that loses
	// the installation race went through everything above and still ended up
	// loading somebody else's certificate; reporting Created=true there would
	// tell an agent scripting enrolment that a fingerprint is brand new when
	// another process has already been using it.
	cc.Created = minted
	return cc, nil
}

// loadClientCertificate reads existing material from dir.
//
// It returns an error wrapping fs.ErrNotExist — and ONLY then — when neither
// file is present, which is the single condition under which the caller is
// allowed to mint. Every other failure, including a half-populated directory,
// is terminal.
func loadClientCertificate(dir string) (*ClientCertificate, error) {
	certPath := filepath.Join(dir, ClientCertFileName)
	keyPath := filepath.Join(dir, ClientKeyFileName)

	certInfo, certErr := os.Stat(certPath)
	keyInfo, keyErr := os.Stat(keyPath)
	certMissing := errors.Is(certErr, fs.ErrNotExist)
	keyMissing := errors.Is(keyErr, fs.ErrNotExist)

	switch {
	case certMissing && keyMissing:
		return nil, fmt.Errorf("%w: %s", fs.ErrNotExist, dir)
	case certMissing:
		return nil, wrapError(KindConfig, "client-cert",
			fmt.Sprintf("%s holds a private key but no certificate (%s is missing)", dir, ClientCertFileName),
			"the certificate cannot be regenerated from the key — a new one would have a different fingerprint, "+
				"which is the value the bus binds to your agent id. Move the whole "+dir+" directory aside to mint a fresh identity, "+
				"then re-enrol; or restore "+ClientCertFileName+" from wherever it went",
			ErrClientCertIncomplete)
	case keyMissing:
		return nil, wrapError(KindConfig, "client-cert",
			fmt.Sprintf("%s holds a certificate but no private key (%s is missing)", dir, ClientKeyFileName),
			"a certificate without its key proves nothing and cannot be used. Move the whole "+dir+" directory aside "+
				"to mint a fresh identity, then re-enrol; or restore "+ClientKeyFileName+" from wherever it went",
			ErrClientCertIncomplete)
	}
	if certErr != nil {
		return nil, wrapError(KindConfig, "client-cert", "cannot read "+certPath, "check the file is readable by this user", certErr)
	}
	if keyErr != nil {
		return nil, wrapError(KindConfig, "client-cert", "cannot read "+keyPath, "check the file is readable by this user", keyErr)
	}
	// BOTH must be ordinary files. The size bound below is taken from stat, and
	// stat reports 0 for a character device or a FIFO — so a cert.pem symlinked
	// to /dev/zero sails through the bound and then reads forever, and a FIFO
	// simply blocks. Neither is a plausible accident, but "the identity dir is
	// 0700 so nobody can do that" is an argument about the ATTACKER, not about
	// the code, and the check costs one line each.
	for _, f := range []struct {
		path string
		info os.FileInfo
	}{{certPath, certInfo}, {keyPath, keyInfo}} {
		if !f.info.Mode().IsRegular() {
			return nil, newError(KindConfig, "client-cert",
				f.path+" is not an ordinary file (mode "+f.info.Mode().String()+")",
				"client TLS material is two ordinary PEM files; move "+dir+" aside in its entirety and re-run to mint fresh material")
		}
	}

	cc := &ClientCertificate{Dir: dir, CertPath: certPath, KeyPath: keyPath}

	// The directory and the key are tightened if they were found loose, and the
	// operator is TOLD. Following Store.OpenStore exactly: silently chmodding
	// makes the evidence disappear and leaves the operator believing a key was
	// private when another local user may have read it. Tightening without
	// saying so is the part that would be wrong, not the tightening.
	if err := cc.tightenPermissions(dir, keyInfo, certInfo); err != nil {
		return nil, err
	}

	certPEM, err := readBoundedFile(certPath, certInfo.Size())
	if err != nil {
		return nil, err
	}
	keyPEM, err := readBoundedFile(keyPath, keyInfo.Size())
	if err != nil {
		return nil, err
	}

	// tls.X509KeyPair parses both halves AND checks that they belong together —
	// a certificate paired with somebody else's key is caught here rather than
	// mid-handshake, where it would surface as an opaque failure from the bus.
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, wrapError(KindConfig, "client-cert",
			"the client TLS material in "+dir+" is not a usable certificate and key",
			"move the whole "+dir+" directory aside and re-run to mint fresh material, then re-enrol",
			err)
	}
	if len(pair.Certificate) == 0 {
		return nil, newError(KindConfig, "client-cert",
			certPath+" contains no certificate",
			"move the whole "+dir+" directory aside and re-run to mint fresh material, then re-enrol")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, wrapError(KindConfig, "client-cert",
			"cannot parse the certificate in "+certPath,
			"move the whole "+dir+" directory aside and re-run to mint fresh material, then re-enrol",
			err)
	}
	// Leaf is populated so crypto/tls does not re-parse the DER on every
	// handshake, and so callers have the dates without parsing it themselves.
	pair.Leaf = leaf
	cc.cert = pair
	cc.Leaf = leaf
	return cc, nil
}

// tightenPermissions repairs a loose directory or key file and records a
// warning for each repair.
func (c *ClientCertificate) tightenPermissions(dir string, keyInfo, certInfo os.FileInfo) error {
	if info, err := os.Stat(dir); err == nil {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			if cerr := os.Chmod(dir, clientTLSDirMode); cerr != nil {
				return wrapError(KindConfig, "client-cert",
					fmt.Sprintf("the client TLS directory %s is mode %#o and could not be tightened to %#o", dir, perm, clientTLSDirMode),
					"run: chmod 700 "+dir, cerr)
			}
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"client TLS directory %s was mode %#o (others could traverse it); tightened to %#o", dir, perm, clientTLSDirMode))
		}
	}
	// Only the KEY earns the "assume it is compromised" wording. A certificate
	// read by another local user has leaked nothing — it is sent in the clear
	// during every handshake.
	if perm := keyInfo.Mode().Perm(); perm&0o077 != 0 {
		if cerr := os.Chmod(c.KeyPath, clientTLSFileMode); cerr != nil {
			return wrapError(KindConfig, "client-cert",
				fmt.Sprintf("the client TLS private key %s is mode %#o and could not be tightened to %#o", c.KeyPath, perm, clientTLSFileMode),
				"run: chmod 600 "+c.KeyPath, cerr)
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"client TLS private key %s was mode %#o (readable by other local users); tightened to %#o — treat it as compromised and mint fresh material",
			c.KeyPath, perm, clientTLSFileMode))
	}
	if perm := certInfo.Mode().Perm(); perm&0o077 != 0 {
		// Tightened for tidiness, and NOT warned about: the certificate is
		// public. A warning here would train the operator to ignore the one
		// above it, which is not.
		_ = os.Chmod(c.CertPath, clientTLSFileMode)
	}
	return nil
}

// readBoundedFile reads path, refusing a file whose stat size already exceeds
// the bound rather than reading it into memory first.
func readBoundedFile(path string, size int64) ([]byte, error) {
	if size > maxClientCertFileBytes {
		return nil, newError(KindConfig, "client-cert",
			fmt.Sprintf("%s is %d bytes; a PEM certificate or key is under %d", path, size, maxClientCertFileBytes),
			"this file is not client TLS material — move it aside and re-run to mint fresh material")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, wrapError(KindConfig, "client-cert", "cannot read "+path, "check the file is readable by this user", err)
	}
	return b, nil
}

// createClientTLS mints material and installs it at dir with ONE rename.
//
// It reports whether THIS call installed what is now on disk. Losing the
// installation race to another process is a success, not an error — the caller
// simply loads what the winner wrote — but it is reported as (false, nil) so
// the caller does not go on to describe somebody else's certificate as freshly
// minted.
func createClientTLS(identityDir, dir string, now time.Time) (bool, error) {
	if err := os.MkdirAll(identityDir, clientTLSDirMode); err != nil {
		return false, wrapError(KindConfig, "client-cert",
			"cannot create the credential store directory "+identityDir,
			"check the path is writable, or point --identity somewhere else", err)
	}

	certPEM, keyPEM, err := mintClientCertificate(now)
	if err != nil {
		return false, err
	}

	// The staging directory is created INSIDE identityDir so the rename that
	// installs it is same-filesystem, and so material never sits, even
	// briefly, anywhere less protected than the credential store. MkdirTemp
	// creates it 0700.
	tmp, err := os.MkdirTemp(identityDir, ".client-tls-tmp-")
	if err != nil {
		return false, wrapError(KindConfig, "client-cert",
			"cannot stage new client TLS material in "+identityDir,
			"check the path is writable, or point --identity somewhere else", err)
	}
	// A no-op once the rename has moved it: RemoveAll on a path that no longer
	// exists returns nil, and it cannot reach the INSTALLED directory because
	// after a successful rename tmp names nothing.
	defer os.RemoveAll(tmp)

	if err := writeSyncedFile(filepath.Join(tmp, ClientCertFileName), certPEM); err != nil {
		return false, err
	}
	if err := writeSyncedFile(filepath.Join(tmp, ClientKeyFileName), keyPEM); err != nil {
		return false, err
	}
	// The files are fsynced above; fsyncing the staging directory makes their
	// NAMES durable too, so the rename cannot publish a directory whose entries
	// a crash could still lose.
	if err := syncDir(tmp); err != nil {
		return false, err
	}

	if err := os.Rename(tmp, dir); err != nil {
		// Either another process installed material between the load attempt
		// and now — the ordinary parallel-agent case, and not a failure — or
		// something is genuinely wrong. Distinguished by asking the filesystem
		// rather than by decoding an errno, which varies by platform.
		//
		// "The directory exists" IS NOT ENOUGH, and an earlier draft that
		// stopped there had a bad failure mode all three reviews found
		// independently. os.Rename Lstats its target and refuses ANY existing
		// directory, empty or not — so an EMPTY or junk-filled client-tls (an
		// operator who read "move the whole directory aside" and moved its
		// CONTENTS; a partially-failed RemoveAll; an rsync that recreated the
		// entry) was classified as a lost race. The reload then found no files
		// and returned a bare fs.ErrNotExist, which is not a *client.Error at
		// all: no Kind, no Remedy, exit code 1 instead of 3. And because doer
		// resolves the certificate on EVERY pinned request, that state wedged
		// every command against a real bus, permanently, with a message naming
		// no fix.
		//
		// So the winner has to have actually WON: both files present, or this
		// is not a race and the operator is told what to do.
		if _, ferr := os.Stat(dir); ferr == nil {
			if complete, cerr := clientTLSIsComplete(dir); cerr == nil && complete {
				return false, nil
			}
			return false, wrapError(KindConfig, "client-cert",
				dir+" already exists but does not hold usable client TLS material, so new material could not be installed there",
				"move "+dir+" aside in its entirety (not just its contents) and re-run; if an identity was already enrolled with the certificate that used to be there, enrol again",
				err)
		}
		// The target is not there at all, so the rename failed for its OWN
		// reason — no space, a read-only or full filesystem, a path that is too
		// long, a permission the process does not have. Saying "it already
		// exists" here would be the same defect this branch was just fixed for,
		// one case over: an assertion about the filesystem that was never
		// checked, plus a remedy that cannot work.
		//
		// The cause is spliced into the MESSAGE rather than left to the wrapped
		// error, because *Error.Error() prints Message and never the cause — so
		// an operator would otherwise be told "cannot install" with no hint of
		// why, which is the least actionable error this file could produce.
		return false, wrapError(KindConfig, "client-cert",
			"cannot install the new client TLS material at "+dir+": "+err.Error(),
			"check the credential store directory is writable and has free space, or point --identity somewhere else",
			err)
	}
	// Best effort: the material is already durable in the files themselves, and
	// a client that cannot open its own config directory has larger problems
	// than a missing directory fsync.
	_ = syncDir(identityDir)
	return true, nil
}

// clientTLSIsComplete reports whether dir holds BOTH files.
//
// It is the "did the other process actually finish" question, and it is asked
// only on the install-race path. It deliberately does not parse anything: a
// present-but-corrupt pair is a different failure with a different message, and
// loadClientCertificate is where that is decided.
func clientTLSIsComplete(dir string) (bool, error) {
	for _, name := range []string{ClientCertFileName, ClientKeyFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, nil
		}
	}
	return true, nil
}

// writeSyncedFile writes data to path with O_EXCL at 0600 and fsyncs it.
//
// O_EXCL rather than O_TRUNC because this function only ever writes into a
// freshly created staging directory: if the file is somehow already there, the
// right answer is to stop, not to overwrite key material.
func writeSyncedFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, clientTLSFileMode)
	if err != nil {
		return wrapError(KindConfig, "client-cert", "cannot write "+path,
			"check for a full or read-only filesystem", err)
	}
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		return wrapError(KindConfig, "client-cert", "cannot write "+path,
			"check for a full or read-only filesystem", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return wrapError(KindConfig, "client-cert", "cannot flush "+path,
			"check for a full or read-only filesystem", serr)
	}
	if cerr := f.Close(); cerr != nil {
		return wrapError(KindConfig, "client-cert", "cannot close "+path,
			"check for a full or read-only filesystem", cerr)
	}
	return nil
}

// syncDir fsyncs a directory so that entries created in it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return wrapError(KindConfig, "client-cert", "cannot open "+dir+" to flush it",
			"check the directory is readable by this user", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return wrapError(KindConfig, "client-cert", "cannot flush the directory "+dir,
			"check for a full or read-only filesystem", serr)
	}
	if cerr != nil {
		return wrapError(KindConfig, "client-cert", "cannot close "+dir,
			"check for a full or read-only filesystem", cerr)
	}
	return nil
}

// mintClientCertificate generates an Ed25519 keypair and a self-signed
// certificate over it, returning both PEM-encoded.
//
// Ed25519 to match every other key in this system, and because crypto/tls
// supports it natively at TLS 1.2 and 1.3 — the floor both ends set.
//
// Nothing here is hand-rolled (invariant 9): the key comes from
// crypto/ed25519.GenerateKey over crypto/rand, the certificate from
// crypto/x509.CreateCertificate, the encodings from crypto/x509's PKCS#8
// marshaller and encoding/pem. The only "scheme" chosen locally is the serial
// number, and x509 requires one — see newClientCertSerial.
func mintClientCertificate(now time.Time) (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, wrapError(KindInternal, "client-cert",
			"cannot generate a client TLS key",
			"this is a failure of the system random source; do not retry until it is understood", err)
	}

	serial, err := newClientCertSerial()
	if err != nil {
		return nil, nil, err
	}

	// SELF-SIGNED, with the template as its own parent. There is no CA in this
	// design and there is not going to be one (invariant 11).
	//
	// The CommonName is DESCRIPTIVE ONLY and carries no authority whatsoever.
	// It cannot carry the agent id even in principle: this certificate has to
	// exist before enrolment, and the id does not exist until the bus mints it
	// (invariant 1). Identity comes from the fingerprint the bus binds, never
	// from a string the holder wrote into its own certificate.
	//
	// # THIS CERTIFICATE IS NOT A CA, AND THAT IS LOAD-BEARING
	//
	// An earlier draft of this template set IsCA:true and KeyUsageCertSign,
	// copying the bus's own certificate (internal/buscert), where they ARE
	// needed: crypto/x509 will only treat a self-signed certificate as its own
	// root if the basic constraint agrees, and client/pin.go's
	// checkBusCertificateValidity verifies the bus leaf against a pool
	// containing itself. The local reasoning was correct. The DOWNSTREAM
	// consequence was not, and the security gate caught it before it shipped.
	//
	// The hazard is in what those two fields would AUTHORISE once the bus
	// starts verifying client certificates (MTLS-CLIENTAUTH / MTLS-BIND). The
	// obvious generalisation of the single-certificate trick above is "collect
	// every enrolled agent's certificate into one x509.CertPool and Verify
	// against it" — and it would look like consistency, not like a mistake. But
	// a pool entry is a TRUSTED ROOT: with CertSign, every agent would become a
	// certificate authority for the whole bus, free to issue a certificate for
	// any name it liked that chains to itself and validates. One agent, root on
	// everybody.
	//
	// That is the same shape as the ClientSessionCache hole two gates found on
	// 2026-08-07: a property that is safe because of what today's CALLER does,
	// one reasonable refactor from being unsafe.
	//
	// So the fields are GONE. Nothing on this side needed them — the presenting
	// client never verifies its own certificate, and nothing in this package
	// builds a pool from it.
	//
	// BE PRECISE ABOUT WHAT THAT DOES AND DOES NOT BUY, because an earlier
	// draft of this comment overclaimed and the security gate disproved it over
	// a live handshake. What the removal closes is ISSUANCE: an agent can no
	// longer mint a certificate for a name of its choosing that chains to its
	// own, so the escalation is gone. What it does NOT do is make the forbidden
	// pool-based design fail. crypto/x509 SHORT-CIRCUITS when the leaf is
	// itself in the Roots pool — it returns that one-element chain without ever
	// checking basic constraints — so a CertPool of agent certificates still
	// validates those agents, and a TLS server with RequireAndVerifyClientCert
	// plus that pool completes the handshake. It was tried; it works.
	//
	// The consequence for whoever reads this next: the ordinary case passing is
	// NOT evidence that a pool-based design is safe, and no test in this suite
	// will tell you otherwise. The instruction below is therefore not a
	// preference, and it is the only thing standing between here and the
	// escalation.
	//
	// # IF YOU ARE HERE TO WRITE SERVER-SIDE CLIENT-CERTIFICATE VERIFICATION
	//
	// DO NOT BUILD A CertPool OF AGENT CERTIFICATES. There is no CA in this
	// design (invariant 11) and chain verification is not the mechanism.
	// MTLS-BIND binds the presented certificate's FINGERPRINT — SHA-256 over
	// the DER, exact match — to the server-minted agent id, exactly as the
	// client pins the bus's. An exact-match comparison runs no verifier and
	// needs neither IsCA nor CertSign from either end.
	//
	// Note also that the BUS's certificate carries both ServerAuth and
	// ClientAuth (a bus dials peers as well as listening), so a pool-based
	// scheme would additionally have to reason about a bus certificate arriving
	// on a client-auth connection. Fingerprint binding does not have that
	// problem either.
	//
	// BasicConstraintsValid stays TRUE with IsCA false, so the certificate
	// carries an explicit CA:FALSE rather than saying nothing. Silence is what
	// a lenient verifier interprets; a stated "no" is what a strict one
	// refuses, and being refused is the outcome we want.
	//
	// KeyUsage is DigitalSignature alone — what a TLS client needs to sign the
	// CertificateVerify message, and nothing more.
	//
	// ExtKeyUsage is ClientAuth ONLY — narrower than the bus's certificate,
	// which also carries ServerAuth because a bus relays to peers and therefore
	// dials as well as listens. An agent only ever dials.
	//
	// NO SUBJECT ALTERNATIVE NAMES, deliberately. SANs answer "which host is
	// this", and this certificate is never presented BY a host: nothing
	// verifies a name against it, and a name here would be an invitation to
	// start.
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "agent-bus client (self-signed, descriptive only)"},
		NotBefore:             now.Add(-clientCertClockSkewAllowance),
		NotAfter:              now.Add(ClientCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, wrapError(KindInternal, "client-cert",
			"cannot create the self-signed client certificate", "this is a bug; report it", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, wrapError(KindInternal, "client-cert",
			"cannot encode the client TLS private key", "this is a bug; report it", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// newClientCertSerial draws a random positive 128-bit serial number — the
// standard construction for a certificate with no issuing CA to allocate one.
//
// A zero draw is retried rather than shipped: zero is not a positive integer
// and some verifiers reject it. Eight consecutive zeroes out of 2^128 is not
// luck, it is a broken random source, and minting a key on a broken random
// source is how a private key ends up being one somebody else can reproduce.
func newClientCertSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for attempt := 0; attempt < 8; attempt++ {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, wrapError(KindInternal, "client-cert",
				"cannot draw a certificate serial number",
				"this is a failure of the system random source; do not retry until it is understood", err)
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
	return nil, newError(KindInternal, "client-cert",
		"the system random source returned a zero certificate serial eight times",
		"the random source is not working; do not generate keys on this machine until it is fixed")
}

// clientCertificateProvider adapts a loaded certificate to crypto/tls's
// GetClientCertificate callback, returning nil when there is none.
//
// # Why GetClientCertificate rather than tls.Config.Certificates
//
// Not style. crypto/tls FILTERS Certificates against the acceptable-CA list in
// the server's CertificateRequest, and this certificate is self-signed — it
// chains to no CA any bus could name. Against a bus that sends a CA list, the
// filter would drop it and the handshake would send an EMPTY certificate
// message, which reads on the server as "this client has no certificate". The
// agent would be locked out by a mechanism that never logged a decision.
// GetClientCertificate is not filtered: what it returns is what is sent.
//
// The request info is ignored ON PURPOSE. The obvious-looking
// cri.SupportsCertificate check would, when the bus and this client disagree
// about signature algorithms, cause us to send nothing at all — the same silent
// lockout in a different costume. Sending the one certificate we have makes the
// disagreement a loud handshake failure naming the algorithms, which is a bug
// report; sending nothing produces "unauthenticated", which is a mystery.
//
// A nil return here would mean "I have no certificate", which never applies:
// this provider is only built once material has been loaded.
func clientCertificateProvider(cert *tls.Certificate) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if cert == nil {
		// Left unset, which is exactly "present no client certificate". The bus
		// does not ask for one today, so this is not a failure path — it is the
		// plaintext-loopback branch, where there is no handshake at all.
		return nil
	}
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
}
