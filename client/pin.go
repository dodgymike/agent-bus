package client

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// BusFingerprintSize is the length of a bus certificate fingerprint in bytes.
// It is sha256.Size and is not a tuning knob.
const BusFingerprintSize = sha256.Size

// BusFingerprint identifies ONE bus certificate: sha256 over the leaf's DER
// encoding.
//
// # This is a PINNED MIRROR of internal/buscert.Fingerprint
//
// The construction — sha256.Sum256(cert.Raw), rendered as lowercase hex with no
// prefix, no colons and no whitespace — is defined server-side in
// internal/buscert/fingerprint.go. It is duplicated here rather than imported
// because this package must not depend on internal/ (invariant 7: an agent has
// to be able to EMBED it, and Go forbids importing internal/ from another
// module). See client/doc.go and client/canonical.go, which carry the same
// constraint for the signing format.
//
// Divergence fails CLOSED, which is the right direction for a duplicate to fail
// in: if the two ever disagreed about how a certificate is hashed, no pin would
// ever match and every connection would be refused. Nothing is accepted by
// accident.
//
// There is deliberately ONE construction. A digest over the SPKI, or over the
// PEM rather than the DER, would be a different identity for the SAME
// certificate, and a system with two identities for one object eventually
// compares one against the other and concludes they are different buses.
//
// It is an array, not a slice, so it is comparable, copyable, and cannot be
// aliased or resized by a caller.
type BusFingerprint [BusFingerprintSize]byte

// ErrBusFingerprintMismatch is the sentinel for "the bus presented a
// certificate that is not the one we pinned".
//
// It is exported so an embedding agent can branch on it with errors.Is without
// parsing a message. It is NOT retryable: see isRetryable.
var ErrBusFingerprintMismatch = errors.New("the bus's certificate does not match the pinned fingerprint")

// ErrBusPresentedNoCertificate is the sentinel for a handshake that reached the
// pin check with an empty peer chain.
//
// This should be unreachable — a TLS server that sends no certificate cannot
// complete the handshakes we offer — which is exactly why it is checked. A
// verifier whose loop body never executes returns nil, and "returns nil" is
// "accepts everything"; the whole failure mode this task guards against is a
// verification callback that silently approves.
var ErrBusPresentedNoCertificate = errors.New("the bus presented no certificate")

// ErrBusCertificateExpired is the sentinel for "the certificate IS the pinned
// one, but it is outside its validity window" — expired, or not yet valid.
//
// It is a DIFFERENT event from ErrBusFingerprintMismatch and must never be
// folded into it. A mismatch says "this is not the bus you were told to expect",
// and the operator's question is whether they are being intercepted. This says
// "this is exactly the bus you expected, and its certificate is out of date" —
// nobody is being intercepted, and the remedy is a rotation or a clock, not an
// investigation. Reporting one as the other sends an operator hunting for an
// attacker that is not there, or worse, dismissing a real substitution as
// "probably just expiry".
//
// Both spellings share one sentinel because a caller's decision is the same for
// both (refuse, do not retry, tell a human) and because the two differ only in
// which end of the window was crossed. BusCertificateExpiredError carries the
// dates for anyone who needs to tell them apart.
var ErrBusCertificateExpired = errors.New("the bus's certificate is outside its validity window")

// ErrBusCertificateUnusable is the sentinel for a pinned certificate that
// crypto/x509 refuses for a reason that is NOT its validity window: DER that
// does not parse, or an unhandled critical extension.
//
// It exists so that "expired" stays a precise claim. The alternative — one
// sentinel for every possible refusal — would make an unhandled critical
// extension report itself as an expiry, and an error that names the wrong cause
// costs more operator time than one that says only "refused".
//
// It is deliberately a CATCH-ALL that fails CLOSED: any verdict from x509 other
// than "valid" refuses the connection. This is the direction a pin check must
// fail in — a default arm that returned nil would be the silent accept-anything
// hole the whole pinning design exists to prevent.
var ErrBusCertificateUnusable = errors.New("the bus's certificate cannot be used")

// ParseBusFingerprint decodes the textual form: exactly BusFingerprintSize*2
// LOWERCASE hexadecimal characters, with no prefix, no colons and no
// whitespace.
//
// Uppercase is REJECTED rather than accepted-and-normalised, matching
// internal/buscert.ParseFingerprint. The textual form travels in the invite
// blob, in the bus's startup log and on the --bus-fingerprint flag, and is
// compared by eye and by naive tooling as often as by this function; permitting
// two spellings of one fingerprint invites a string comparison somewhere else
// that says two equal fingerprints differ.
//
// It does NOT trim whitespace. A fingerprint with a stray space is a
// transcription error, and quietly repairing it teaches a caller that sloppy
// copies are fine — on the one value where an exact copy is the whole point.
func ParseBusFingerprint(s string) (BusFingerprint, error) {
	var f BusFingerprint
	want := hex.EncodedLen(BusFingerprintSize)
	if len(s) != want {
		return BusFingerprint{}, usagef("fingerprint",
			"pass the fingerprint exactly as the invite (or the bus's startup log line `bus_cert_fingerprint=…`) gives it: "+
				fmt.Sprint(want)+" lowercase hex characters, no colons",
			"a bus certificate fingerprint is %d lowercase hexadecimal characters, got %d", want, len(s))
	}
	if _, err := hex.Decode(f[:], []byte(s)); err != nil {
		return BusFingerprint{}, wrapError(KindUsage, "fingerprint",
			"a bus certificate fingerprint must be hexadecimal",
			"copy the value from the invite, or from the bus's `bus_cert_fingerprint=…` startup log line",
			err)
	}
	if f.String() != s {
		// hex.Decode accepts uppercase; the round trip is what rejects it, and
		// it also rejects any other spelling that decodes to the same bytes.
		return BusFingerprint{}, usagef("fingerprint",
			"lowercase it: "+f.String(),
			"a bus certificate fingerprint must be LOWERCASE hexadecimal")
	}
	return f, nil
}

// busFingerprintOfDER returns the fingerprint of a certificate's DER bytes.
//
// It hashes the DER exactly as it arrived on the wire, NOT a re-encoding of the
// parsed fields: re-marshalling a certificate is not guaranteed to reproduce
// the original bytes, and a fingerprint that changed on a round trip would fail
// to match the pin the invite carried.
func busFingerprintOfDER(der []byte) BusFingerprint { return sha256.Sum256(der) }

// String renders the fingerprint as lowercase hex — the one textual form.
func (f BusFingerprint) String() string { return hex.EncodeToString(f[:]) }

// Equal reports whether two fingerprints are the same.
//
// subtle.ConstantTimeCompare, and the honest reason is NOT that timing matters:
// a certificate fingerprint is PUBLIC — it is in the invite blob, in the bus's
// log, and derivable by anyone who completes a handshake — so constant time
// buys nothing against an attacker here. It is used because it costs nothing at
// 32 bytes and because the alternative is that the next caller who needs a
// comparison writes a hand-rolled byte loop, and the one after that copies it
// somewhere it IS security-relevant.
func (f BusFingerprint) Equal(other BusFingerprint) bool {
	return subtle.ConstantTimeCompare(f[:], other[:]) == 1
}

// IsZero reports whether f is the zero value, i.e. "no pin was configured".
//
// The zero fingerprint is not a wildcard and never matches a real certificate:
// no DER hashes to 32 zero bytes. It means ABSENT, and every caller treats
// absent as "refuse to speak TLS to this bus" rather than "accept anything".
func (f BusFingerprint) IsZero() bool { return f == BusFingerprint{} }

// pinnedTLSConfig builds the client TLS configuration for a bus whose
// certificate fingerprints are pinned.
//
// pins is a SET (MTLS-ROTATE, 2026-08-07), normally of size one and of size two
// for the duration of a rollover. See BusPinSet for why the set is bounded and
// why membership is granted rather than learned; nothing below treats a larger
// set as weaker, because every member got there by the same explicit operator
// act that the single pin used to require.
//
// # Read this before changing anything below
//
// agent-bus certificates are SELF-SIGNED and there is NO certificate authority
// anywhere in the design (invariant 11). The stdlib's default chain
// verification therefore CANNOT succeed: there is no root to chain to, and
// there is deliberately no trust-on-first-use path that would create one. The
// only supported way to substitute a different verification policy in
// crypto/tls is to turn the default chain check off and supply
// VerifyPeerCertificate — the field's own documentation describes exactly this
// arrangement, and it is why the two appear TOGETHER, in one composite literal,
// in this one function.
//
// The distinction that matters, and the reason this function is small enough to
// read in one sitting: "we skip the CA check and verify the pin instead" and
// "we skip verification" differ by ONE callback, and the second fails silently
// — it still completes a handshake, it still returns a working connection, it
// simply proves nothing. So:
//
//   - The pin check is NOT optional and NOT conditional. There is no branch in
//     verifyPinnedBusCertificate that returns nil without having compared 32
//     bytes against a member of the set.
//   - An EMPTY set is refused inside the callback, not merely refused by
//     callers. Every caller does check (see Client.doer), but a verifier that is
//     only safe because of what its callers do is one refactor from being
//     unsafe. Empty means ABSENT; it is never a wildcard.
//   - guard_test.go asserts, by AST walk, that this file is the ONLY place in
//     client/ or cmd/agent-busctl/ that disables default verification, that it
//     does so at most once, and that the same tls.Config literal also sets
//     VerifyPeerCertificate. Deleting the callback is a test failure, not a
//     review question.
//
// What is given up by turning off the default check, precisely: chain building
// to a trusted root (there is none, by design), HOSTNAME verification, and the
// VALIDITY PERIOD.
//
// On the first two, the pin substitutes: a name check asks "does this
// certificate claim this address", while the pin asks "is this the exact
// certificate the invite named". That is stronger — but stronger UNDER AN
// ASSUMPTION worth stating rather than glossing: one certificate per bus. The
// security gate was right to push back on an earlier draft that called it
// strictly stronger without qualification. If one certificate were ever served
// by two buses, a name check would distinguish them and the pin would not.
// Nothing in this design does that today, and rotation (which serves two
// certificates for ONE bus) is the opposite case and is safe — which is
// precisely what the accept-set models.
//
// On the third, nothing substituted until MTLS-EXPIRY (2026-08-07), and the gap
// was real rather than theoretical: DECISIONS.md chose a 365-day certificate
// lifetime explicitly as "a leak-containment bound", and a bound nothing
// enforces is decoration. checkBusCertificateValidity now RESTORES that one
// check — and only that one — by handing the leaf back to crypto/x509. It is
// restored SEPARATELY rather than by re-enabling the default chain check,
// because the default check would fail for the CA reason long before it reached
// the dates.
//
// # DO NOT ADD A ClientSessionCache HERE
//
// Both gates on MTLS-EXPIRY independently flagged this as the next occurrence of
// the same silent failure, so it is written at the literal rather than left to
// be rediscovered. crypto/tls DOES NOT CALL VerifyPeerCertificate ON A RESUMED
// HANDSHAKE — its own source says so ("Resumptions currently don't reverify
// certificates"). Setting ClientSessionCache for latency would therefore bypass
// the pin check AND the expiry check on every resumed connection, silently, with
// every positive test still passing. It is absent today, which disables
// resumption entirely and is why the callback runs on every connection.
//
// If session resumption is ever genuinely wanted, the checks must move to or be
// duplicated in VerifyConnection, which IS called on resumption (crypto/tls
// invokes it inside the resumption branch for exactly this reason). Adding the
// cache ALONE is a regression that looks like a performance win.
//
// TestPinnedSkipIsAlwaysPairedWithAPinCheck enforces precisely that and no more:
// it rejects a session cache set in this literal, or assigned to any tls.Config
// field anywhere this guard scans, UNLESS VerifyConnection is set in the same
// literal. It does not and cannot check that the callback actually re-runs these
// checks — that part is on the reader.
//
// Revocation is genuinely out of scope: there is no CA and therefore no CRL or
// OCSP to consult. Revoking a bus certificate means re-issuing invites.
func pinnedTLSConfig(pins BusPinSet, clientCert *tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,

		// Default chain verification is off because there is no CA to chain to
		// (see this function's doc comment). It is replaced, never merely
		// removed, by the callback on the next line.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pinVerifier(pins, time.Now),

		// The OTHER half of mutual TLS (invariant 11): what this end PRESENTS.
		// It belongs in this literal and not in newHTTPClient's unpinned one,
		// which this function replaces wholesale — a client certificate set
		// there would be silently dropped on every pinned, i.e. every real,
		// connection.
		//
		// A callback rather than Certificates, and the reason is a lockout
		// rather than a preference: see clientCertificateProvider. nil when
		// there is no material, which is exactly "present nothing" — and is
		// what happens today anyway, because the bus does not ask.
		GetClientCertificate: clientCertificateProvider(clientCert),
	}
}

// pinVerifier adapts verifyPinnedBusCertificate to crypto/tls's callback shape.
//
// pins is captured BY VALUE, and BusPinSet's slice is never mutated in place
// (every method returns a fresh set), so the policy a live handshake is checked
// against cannot change under it. A transport built for one set is discarded
// wholesale when the set changes — see Client.doer.
//
// verifiedChains is ignored because it is ALWAYS nil here: crypto/tls documents
// that it passes nil when default verification is disabled. Naming it and
// ignoring it is deliberate — a future reader who assumes a chain is available
// would write a check that never runs.
//
// now is READ PER HANDSHAKE, not captured once. A long-lived agent holds one
// transport for days; a clock sampled when the transport was built would go on
// approving a certificate that expired hours ago, and the longer the process
// ran the wronger it would get. It is a parameter rather than a package-level
// variable so that the clock a verifier uses is visible at its construction
// site, and so a test can drive the window without a mutable global that every
// other test in the package shares.
//
// PER HANDSHAKE IS NOT PER REQUEST, and the difference is worth stating rather
// than leaving to be assumed: an established connection is reused WITHOUT a new
// handshake, so the real bound on an expired certificate's usable life is how
// long the pooled connection survives. For an agent continuously long-polling
// /v1/wait the connection never goes idle, so a certificate that expires
// mid-poll keeps being used until that connection drops. That is ordinary TLS
// client behaviour rather than a defect here — closing it would mean tearing
// down live connections on a timer — but the claim is "every handshake", not
// "every request".
func pinVerifier(pins BusPinSet, now func() time.Time) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		return verifyPinnedBusCertificate(pins, rawCerts, now())
	}
}

// verifyPinnedBusCertificate is THE pin check. It returns nil only after the
// leaf certificate's fingerprint has matched a member of a NON-EMPTY set.
//
// rawCerts[0] is the leaf, in the order the peer sent it (crypto/tls). Only the
// leaf is considered: the set names certificates, and a bus that presented a
// pinned certificate somewhere deeper in a chain it invented would not be the
// pinned bus.
//
// Matching ANY member is the rotation property (MTLS-ROTATE) and it is NOT a
// relaxation of the check: each member was placed there by the same explicit,
// out-of-band operator act the single pin required, the set is bounded at
// MaxBusPins, and this function still never returns nil without a 32-byte
// comparison having succeeded. Nothing here can add to the set — this is a pure
// read — which is what makes "a pin is never learned from a handshake" a
// structural fact rather than a convention.
//
// # Two checks, in this order, and the order is deliberate (MTLS-EXPIRY)
//
// Since 2026-08-07 a matching fingerprint is NECESSARY BUT NOT SUFFICIENT: the
// certificate must also be inside its validity window. The pin answers "which
// bus"; it never answered "is this certificate still fit to use", and until
// this task nothing did.
//
// The IDENTITY check runs FIRST and the VALIDITY check second, because the two
// failures demand opposite responses and the first question an operator must be
// able to answer is "am I talking to the right bus at all". A certificate that
// is both unpinned and expired is reported as UNPINNED — the expiry of a
// certificate we were never going to accept is a detail about a stranger, and
// leading with it would bury the substitution.
//
// It also means the x509 parser is only handed DER that already matched a
// pinned 32-byte digest. That is not the reason for the ordering (crypto/tls has
// parsed the whole peer chain before this callback is ever invoked, so no
// exposure is actually avoided), and it is recorded here only so nobody
// "optimises" the order believing it buys something.
//
// at is the time to judge the window against, passed in rather than read here
// so this function stays a pure predicate over (pins, bytes, time) — which is
// what makes the boundary cases testable without waiting for a clock.
func verifyPinnedBusCertificate(pins BusPinSet, rawCerts [][]byte, at time.Time) error {
	if pins.IsEmpty() {
		// Unreachable through Client, which refuses to build a pinned transport
		// without a pin. Checked anyway: an empty set means ABSENT, and an
		// absent pin must never be the thing that lets a handshake through.
		return ErrBusFingerprintMismatch
	}
	if len(rawCerts) == 0 || len(rawCerts[0]) == 0 {
		return ErrBusPresentedNoCertificate
	}
	got := busFingerprintOfDER(rawCerts[0])
	if !pins.Contains(got) {
		return &BusFingerprintError{Pinned: pins, Presented: got}
	}
	return checkBusCertificateValidity(rawCerts[0], at)
}

// checkBusCertificateValidity enforces the pinned certificate's VALIDITY WINDOW
// — the one thing the default chain check would have done that the pin does not
// stand in for.
//
// # It AUTHENTICATES NOTHING on its own, and must never be called as if it did
//
// An attacker-minted, in-date, self-signed certificate passes this function
// cleanly — the security gate confirmed it by construction. That is correct: it
// judges dates, and only dates. The identity comes entirely from the fingerprint
// comparison in verifyPinnedBusCertificate, which is unconditional and runs
// first. A future caller that invoked this function without that comparison
// would have checked that a stranger's certificate is in date and nothing else.
//
// For the same reason "hands the leaf back to crypto/x509" should not be read as
// "x509 validated the certificate". Chain building is skipped, so the
// SELF-SIGNATURE is never verified here either. It does not need to be: the pin
// covers the entire DER including the signature bytes, and the TLS handshake
// separately proves the peer holds the private key for that certificate's public
// key.
//
// # crypto/x509 decides; this function only reports
//
// The verdict comes from x509.Certificate.Verify and from nowhere else.
// Invariant 9 is the reason, and it applies more literally than it first looks:
// the temptation here is two lines of `at.Before(leaf.NotBefore) ||
// at.After(leaf.NotAfter)`, which is not obviously "writing crypto" and is
// exactly the kind of certificate-handling detail a library exists to get right
// — half-open intervals, the zero time, a NotAfter before NotBefore. x509
// already implements it, has been audited implementing it, and is the same code
// every other Go TLS client is judged by. Reimplementing it would create a
// SECOND answer to a question that must have one.
//
// # Why Verify with the leaf as its own root, rather than the default check
//
// agent-bus certificates are self-signed with no CA anywhere (invariant 11), so
// an ordinary Verify fails with UnknownAuthorityError before it ever considers
// the dates — which is precisely why turning the default check off took the
// validity check away with it. Putting the LEAF ITSELF in the root pool is the
// stdlib's own supported way to say "trust is already established, apply the
// remaining checks": Verify runs its validity check, finds the certificate in
// the pool, and returns without building a chain.
//
// The pool must be x509.NewCertPool() specifically, and this is load-bearing for
// an EMBEDDABLE package (invariant 7): on darwin, windows and ios, Verify hands
// off to the PLATFORM verifier when Roots is nil or is the system pool. A fresh
// pool is neither, so this code path is the same on every operating system. A
// system pool here would also drag in the CA trust this design does not have.
//
// Three options are set and each is load-bearing:
//
//   - Roots is the leaf, for the reason above. It is a fresh pool per call, so
//     nothing accumulates and no other certificate can be in it.
//   - CurrentTime is the caller's, so the boundary cases are testable. A ZERO
//     time never reaches it — see the guard at the top of this function.
//   - KeyUsages is ExtKeyUsageAny, which SKIPS extended-key-usage filtering. It
//     is not laxity: the pin already decided this is the bus's certificate, and
//     the default (ServerAuth) would make this function quietly reject a valid
//     pinned certificate over an EKU bit, reporting it as a validity problem it
//     is not. EKU policy, if it is ever wanted, belongs in its own check with
//     its own error.
//
// DNSName is deliberately left empty: there is no hostname verification in this
// design and the pin substitutes for it (see pinnedTLSConfig). Setting it here
// would resurrect a name check the invite blob was designed to replace, and a
// bus reachable at an address its certificate does not list is normal.
//
// # There is NO client-side clock-skew allowance, on purpose
//
// internal/buscert backdates NotBefore by five minutes when it MINTS a
// certificate, which is the right place for an allowance: it is applied once,
// by the party that knows the certificate is fresh, and it is visible in the
// certificate itself. A second, invisible allowance here would extend every
// certificate's usable life beyond the NotAfter it states — silently weakening
// the leak-containment bound this task exists to enforce, in a way no operator
// reading the certificate could see. A client whose clock is wrong gets a
// refusal that names the clock as the first thing to check.
func checkBusCertificateValidity(der []byte, at time.Time) error {
	if at.IsZero() {
		// REFUSED rather than repaired, and the security gate is why. x509
		// substitutes time.Now() for a zero CurrentTime, so the VERDICT would
		// have been right — but BusCertificateExpiredError.Error() would then
		// compare the zero At and name the wrong end of the window, which the
		// gate observed: an EXPIRED certificate described as "NOT VALID UNTIL …
		// it is now 0001-01-01". Refusing removes the divergence instead of
		// documenting it, and a caller with no clock has not judged anything.
		return fmt.Errorf("%w: it was checked against the zero time, which is not a clock", ErrBusCertificateUnusable)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		// Unreachable from a live handshake — crypto/tls parses the peer chain
		// before it calls VerifyPeerCertificate, so unparseable DER never gets
		// this far. Checked because a verifier must have no path that returns
		// nil without having judged something.
		return fmt.Errorf("%w: its leaf is not a parseable X.509 certificate: %s", ErrBusCertificateUnusable, err)
	}

	selfSigned := x509.NewCertPool()
	selfSigned.AddCert(leaf)
	if _, verr := leaf.Verify(x509.VerifyOptions{
		Roots:       selfSigned,
		CurrentTime: at,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); verr != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(verr, &invalid) && invalid.Reason == x509.Expired {
			return &BusCertificateExpiredError{
				Fingerprint: busFingerprintOfDER(der),
				NotBefore:   leaf.NotBefore,
				NotAfter:    leaf.NotAfter,
				At:          at,
			}
		}
		// Any OTHER verdict from x509 refuses the connection too. On this build
		// only an unhandled critical extension can reach here; under a
		// boringcrypto build it would ALSO catch every Ed25519 certificate,
		// because x509's FIPS gate accepts only RSA and NIST-curve ECDSA keys —
		// i.e. every healthy bus certificate would be refused as unusable. That
		// is a build we do not produce (no GOEXPERIMENT in go.mod or the
		// Dockerfile) and is recorded so the "today only" claim stays checkable.
		//
		// A certificate that is BOTH expired AND carries an unhandled critical
		// extension lands here rather than in the expiry branch, because x509
		// checks extensions before dates. It fails closed either way; only the
		// remedy is the generic one.
		//
		// The default arm FAILS CLOSED: a verifier whose unrecognised case
		// returns nil accepts everything it did not think of.
		return fmt.Errorf("%w: %s", ErrBusCertificateUnusable, verr)
	}
	return nil
}

// BusCertificateExpiredError reports the pinned certificate presented outside
// its validity window, and carries the whole window plus the time it was judged
// against.
//
// All four values are safe to print and all four are needed. "Expired" alone
// leaves an operator unable to distinguish the two causes, which have nothing in
// common: a bus genuinely serving a stale certificate, or THIS MACHINE'S CLOCK
// being wrong. Printing the window and the observed time makes a skewed clock
// self-evident — a NotAfter years away with an "it is now 1970" beside it needs
// no further diagnosis.
type BusCertificateExpiredError struct {
	// Fingerprint is the certificate's fingerprint. It MATCHED the pin — that
	// is what makes this error different from a BusFingerprintError — so naming
	// it tells the operator the identity check passed.
	Fingerprint BusFingerprint

	// NotBefore and NotAfter are the certificate's stated validity window.
	NotBefore time.Time
	NotAfter  time.Time

	// At is the time the window was judged against: this client's clock.
	At time.Time
}

// Error names which END of the window was crossed.
//
// The comparison below chooses WORDING ONLY. The verdict was already made by
// crypto/x509 in checkBusCertificateValidity; if this comparison and x509 ever
// disagreed, the connection would still be refused and only the sentence would
// be wrong. It is written this way round on purpose — a message that decides
// nothing cannot become a second, divergent implementation of the check.
func (e *BusCertificateExpiredError) Error() string {
	if e.At.Before(e.NotBefore) {
		return fmt.Sprintf("the bus presented certificate %s, which is NOT VALID UNTIL %s (this client's clock says it is now %s)",
			e.Fingerprint, e.NotBefore.UTC().Format(time.RFC3339), e.At.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("the bus presented certificate %s, which EXPIRED at %s (this client's clock says it is now %s)",
		e.Fingerprint, e.NotAfter.UTC().Format(time.RFC3339), e.At.UTC().Format(time.RFC3339))
}

// Unwrap makes errors.Is(err, ErrBusCertificateExpired) true, so a caller can
// branch on the condition without knowing the concrete type.
func (e *BusCertificateExpiredError) Unwrap() error { return ErrBusCertificateExpired }

// BusFingerprintError reports a certificate that is not the pinned one, and
// carries both fingerprints so an operator can see which is which.
//
// BOTH values are safe to print: a certificate fingerprint is public. Printing
// only "mismatch" would leave an operator unable to tell a legitimate rotation
// (the presented value matches what the bus now logs) from an interception (it
// matches nothing they can account for) — which is the ONE judgement this
// error exists to support.
type BusFingerprintError struct {
	// Pinned is the SET of certificates this client would have accepted, from
	// --bus-fingerprint, AGENT_BUS_FINGERPRINT, or the stored identity.
	//
	// It is a set rather than one value because a bus mid-rollover legitimately
	// has two certificates (MTLS-ROTATE), and an error that named only one of
	// them would send an operator hunting for a mismatch that is not there.
	Pinned BusPinSet

	// Presented is the fingerprint of the certificate the bus actually sent.
	Presented BusFingerprint
}

func (e *BusFingerprintError) Error() string {
	if e.Pinned.Len() == 1 {
		return fmt.Sprintf("the bus presented certificate %s, but %s is pinned",
			e.Presented, e.Pinned)
	}
	return fmt.Sprintf("the bus presented certificate %s, but this client accepts only %s",
		e.Presented, e.Pinned)
}

// Unwrap makes errors.Is(err, ErrBusFingerprintMismatch) true for this type, so
// a caller can branch on the condition without knowing the concrete type.
func (e *BusFingerprintError) Unwrap() error { return ErrBusFingerprintMismatch }

// pinError classifies a pin failure that surfaced out of a handshake.
//
// KindNetwork, matching the Kind's own documentation ("a failure to reach the
// bus at all: … or a certificate that does not match the pinned fingerprint")
// and therefore exit code 5. Nothing was applied: the request never left this
// process, because the handshake did not complete.
//
// The remedy is the load-bearing part. A fingerprint mismatch has exactly two
// causes and they demand opposite responses, so the remedy names both rather
// than guessing which one the operator is looking at — and it forecloses the
// third response, which is to disable the check.
//
// # Since MTLS-ROTATE the FIRST remedy is `pin add`, and that ordering matters
//
// A rollover is the common cause, and until this task the only recovery was
// `logout` + re-enrol — which is a fleet-wide re-enrolment for a routine key
// hygiene event, and DECISIONS.md E3 says that must never be required. Worse,
// it made routine rotation indistinguishable from an incident, and a wedged
// fleet is exactly the pressure under which somebody proposes letting
// --bus-fingerprint override the stored pin (which would turn a DETECTED
// substitution into an ACCEPTED one). `pin add` removes the pressure without
// touching the check.
//
// The confirmation clause is not decoration. `pin add` is safe only because the
// value is confirmed OUT OF BAND first; adding whatever the far end just
// presented would be trust-on-first-use, so the remedy says where the genuine
// value comes from before it says which command to run.
//
// The logout/enrol path is still named, second, because it is the right answer
// when the operator wants the OLD certificate gone rather than accepted
// alongside the new one. It names `logout` BEFORE `enrol`, and that ordering is
// not padding: the stored identity still pins the old certificate, so an enrol
// carrying the new fingerprint hits resolvePin's flag-vs-store conflict and is
// refused. An earlier draft omitted the logout and the reviewer gate reproduced
// the dead end — a remedy that does not work is worse than none, because the
// operator concludes the tool is broken and goes looking for the flag that
// turns the check off.
//
// # The expiry case gets its own remedy, and it must not read like a mismatch
//
// A pinned certificate that is out of date is NOT a substitution, and the
// remedy says so first. The two causes are a bus serving a stale certificate
// and a wrong LOCAL CLOCK, and the clock is named first because it is the one
// an operator can check in five seconds, because re-pinning cannot fix it, and
// because a skewed clock is the failure most likely to be misread as an attack.
func pinError(op, busURL string, err error) *Error {
	var stale *BusCertificateExpiredError
	if errors.As(err, &stale) {
		return wrapError(KindNetwork, op,
			fmt.Sprintf("REFUSING to talk to %s: %s", busURL, stale.Error()),
			"the certificate IS the one you pinned, so this is not a substitution — it is out of date. "+
				"Check THIS MACHINE'S CLOCK first: a skewed clock rejects a perfectly good certificate, and no amount of re-pinning will fix it. "+
				"If the clock is right, the bus is serving a certificate past its validity window and must rotate it. "+
				"Once it has, confirm the new value OUT OF BAND (the bus logs `bus_cert_fingerprint=…` at startup), then "+
				"`agent-busctl pin add <new>` and `agent-busctl pin remove "+stale.Fingerprint.String()+"`. "+
				"There is no flag that accepts an out-of-date certificate, on purpose.",
			err)
	}
	var mismatch *BusFingerprintError
	if errors.As(err, &mismatch) {
		return wrapError(KindNetwork, op,
			fmt.Sprintf("REFUSING to talk to %s: it presented certificate %s, but this client accepts %s",
				busURL, mismatch.Presented, mismatch.Pinned),
			"the bus's certificate CHANGED. Either it was legitimately rotated, or you are not talking to the bus you think you are — and those look identical from here. "+
				"Confirm the presented value OUT OF BAND (the bus logs `bus_cert_fingerprint=…` at startup). If it is genuine, accept it ALONGSIDE the old one for the rollover: "+
				"`agent-busctl pin add "+mismatch.Presented.String()+"`, then `agent-busctl pin remove <old>` once the bus has stopped serving the old certificate. "+
				"To replace the pin outright instead, `agent-busctl logout <agent-id>` then `agent-busctl enrol --bus <url> --bus-fingerprint <new> --name <name>` — "+
				"enrolling without the logout is refused, because the stored identity still pins the old certificate. "+
				"Do NOT work around this by turning verification off: there is no flag that does, on purpose.",
			err)
	}
	return wrapError(KindNetwork, op,
		"REFUSING to talk to "+busURL+": its TLS certificate could not be checked against the pinned fingerprint",
		"check that --bus points at the agent-bus server the invite named; a bus always presents its certificate",
		err)
}

// isPinError reports whether err is a pinned-certificate failure, following the
// Unwrap chain out of *url.Error and *tls.Error.
//
// Every sentinel this file can produce is listed, and that completeness is what
// makes the classification correct rather than approximate: networkError routes
// on it (so the message and remedy come from pinError rather than from a generic
// "cannot reach the bus"), and isRetryable routes on it (so none of these is
// ever repeated). A sentinel added to this file and forgotten here would be
// reported as a transient network fault AND retried — a certificate problem
// dressed up as a flaky connection, which is the single most effective way to
// stop an operator noticing it.
func isPinError(err error) bool {
	return errors.Is(err, ErrBusFingerprintMismatch) ||
		errors.Is(err, ErrBusPresentedNoCertificate) ||
		errors.Is(err, ErrBusCertificateExpired) ||
		errors.Is(err, ErrBusCertificateUnusable)
}
