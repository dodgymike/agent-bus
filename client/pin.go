package client

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
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
// certificate fingerprint is pinned.
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
//     bytes.
//   - A zero pin is refused inside the callback, not merely refused by callers.
//     Every caller does check (see Client.transportFor), but a verifier that is
//     only safe because of what its callers do is one refactor from being
//     unsafe.
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
// certificates for ONE bus) is the opposite case and is safe.
//
// On the third, nothing substitutes, and this is a real gap rather than a
// non-issue: DECISIONS.md chose a 365-day certificate lifetime explicitly as "a
// leak-containment bound", and only the client can enforce that bound on the
// BUS's certificate. This code does not, so an expired bus certificate is
// accepted. MTLS-VERIFY owns it and must land with or before MTLS-LISTENER, or
// the lifetime decision is decoration. Recorded here, at the callback that
// would do the check, rather than only in a backlog nobody reads from here.
//
// Revocation is genuinely out of scope: there is no CA and therefore no CRL or
// OCSP to consult. Revoking a bus certificate means re-issuing invites.
func pinnedTLSConfig(pin BusFingerprint) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,

		// Default chain verification is off because there is no CA to chain to
		// (see this function's doc comment). It is replaced, never merely
		// removed, by the callback on the next line.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pinVerifier(pin),
	}
}

// pinVerifier adapts verifyPinnedBusCertificate to crypto/tls's callback shape.
//
// verifiedChains is ignored because it is ALWAYS nil here: crypto/tls documents
// that it passes nil when default verification is disabled. Naming it and
// ignoring it is deliberate — a future reader who assumes a chain is available
// would write a check that never runs.
func pinVerifier(pin BusFingerprint) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		return verifyPinnedBusCertificate(pin, rawCerts)
	}
}

// verifyPinnedBusCertificate is THE pin check. It returns nil only after
// comparing the leaf certificate's fingerprint against a non-zero pin.
//
// rawCerts[0] is the leaf, in the order the peer sent it (crypto/tls). Only the
// leaf is considered: the pin names one certificate, and a bus that presented
// the pinned certificate somewhere deeper in a chain it invented would not be
// the pinned bus.
func verifyPinnedBusCertificate(pin BusFingerprint, rawCerts [][]byte) error {
	if pin.IsZero() {
		// Unreachable through Client, which refuses to build a pinned transport
		// without a pin. Checked anyway: the zero pin means ABSENT, and an
		// absent pin must never be the thing that lets a handshake through.
		return ErrBusFingerprintMismatch
	}
	if len(rawCerts) == 0 || len(rawCerts[0]) == 0 {
		return ErrBusPresentedNoCertificate
	}
	got := busFingerprintOfDER(rawCerts[0])
	if !pin.Equal(got) {
		return &BusFingerprintError{Pinned: pin, Presented: got}
	}
	return nil
}

// BusFingerprintError reports a certificate that is not the pinned one, and
// carries both fingerprints so an operator can see which is which.
//
// BOTH values are safe to print: a certificate fingerprint is public. Printing
// only "mismatch" would leave an operator unable to tell a legitimate rotation
// (the presented value matches what the bus now logs) from an interception (it
// matches nothing they can account for) — which is the ONE judgement this
// error exists to support.
type BusFingerprintError struct {
	// Pinned is the fingerprint this client expected, from --bus-fingerprint,
	// AGENT_BUS_FINGERPRINT, or the stored identity.
	Pinned BusFingerprint

	// Presented is the fingerprint of the certificate the bus actually sent.
	Presented BusFingerprint
}

func (e *BusFingerprintError) Error() string {
	return fmt.Sprintf("the bus presented certificate %s, but %s is pinned",
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
// It names `logout` BEFORE `enrol`, and that ordering is not padding: the
// stored identity still pins the OLD certificate, so an enrol carrying the new
// fingerprint hits resolvePin's flag-vs-store conflict and is refused. An
// earlier draft omitted the logout and the reviewer gate reproduced the dead
// end — a remedy that does not work is worse than none, because the operator
// concludes the tool is broken and goes looking for the flag that turns the
// check off.
func pinError(op, busURL string, err error) *Error {
	var mismatch *BusFingerprintError
	if errors.As(err, &mismatch) {
		return wrapError(KindNetwork, op,
			fmt.Sprintf("REFUSING to talk to %s: it presented certificate %s, but this client pinned %s",
				busURL, mismatch.Presented, mismatch.Pinned),
			"the bus's certificate CHANGED. Either it was legitimately rotated, or you are not talking to the bus you think you are — and those look identical from here. "+
				"Confirm the new value OUT OF BAND (the bus logs `bus_cert_fingerprint=…` at startup). If it is genuine, re-pin in two steps, in this order: "+
				"`agent-busctl logout <agent-id>` to drop the old pin, then `agent-busctl enrol --bus <url> --bus-fingerprint <new> --name <name>`. "+
				"Enrolling without the logout is refused, because the stored identity still pins the old certificate. "+
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
func isPinError(err error) bool {
	return errors.Is(err, ErrBusFingerprintMismatch) || errors.Is(err, ErrBusPresentedNoCertificate)
}
