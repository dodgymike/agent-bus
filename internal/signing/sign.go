package signing

import (
	"crypto/ed25519"
	"errors"
	"fmt"
)

// SignatureSize is the length of an Ed25519 detached signature: 64 bytes,
// always, for every message. It is taken from crypto/ed25519 rather than
// written as a literal so the two can never disagree.
//
// SIGN-6's ingest rule is a LENGTH check and nothing more, which is why it can
// live on a bus that cannot verify: a bus is not entitled to decide whether a
// signature is authentic (it does not hold, and must not be trusted to police,
// the sender's key), but it is entitled to insist the field is present and the
// right shape before it makes anything durable.
const SignatureSize = ed25519.SignatureSize

// PublicKeySize is the length of an Ed25519 messaging public key: 32 bytes.
//
// It is exported because EVERY caller that verifies must check it BEFORE
// calling ed25519.Verify — see Verify's doc for why that is a remote-DoS trap
// rather than a style preference.
const PublicKeySize = ed25519.PublicKeySize

// PrivateKeySize is the length of an Ed25519 messaging private key: 64 bytes
// (the RFC 8032 seed followed by the public half, which is what
// crypto/ed25519 hands back from GenerateKey).
const PrivateKeySize = ed25519.PrivateKeySize

// The failure taxonomy. Every one is matchable with errors.Is, and they are
// DISTINCT on purpose.
//
// SIGN-6 requires a verification failure to be LOUD and to name WHICH check
// failed, and SIGN-5 requires each rejection case to be provable by its own
// assertion rather than folded into a single "an error occurred". A verifier
// that returns one opaque error for "no signature", "wrong length", "garbage
// key" and "does not verify" makes both of those impossible, and it also hides
// the one case that is an OPERATOR fault (a roster holding a malformed key)
// among three that are attacker input.
//
// None of these is retryable. A signature that does not verify does not verify
// on the second attempt either; see the note on terminal rejection in Verify.
var (
	// ErrNoSignature reports an ABSENT signature: the field was empty or nil.
	// It is separate from ErrSignatureLength because it is the exact case
	// SIGN-6 exists to close — "no signature" must never be read as "unsigned
	// but fine", and an attacker STRIPPING a signature is a different event
	// from one mangling it.
	ErrNoSignature = errors.New("signing: a signature is required and none was supplied")

	// ErrSignatureLength reports a signature that is present but is not
	// exactly SignatureSize bytes.
	ErrSignatureLength = errors.New("signing: signature is not exactly 64 bytes")

	// ErrPublicKeyLength reports a messaging public key that is not exactly
	// PublicKeySize bytes. This is the panic trap: see Verify.
	ErrPublicKeyLength = errors.New("signing: messaging public key is not exactly 32 bytes")

	// ErrPrivateKeyLength reports a messaging private key that is not exactly
	// PrivateKeySize bytes. ed25519.Sign panics on one, exactly as Verify
	// panics on a malformed public key.
	ErrPrivateKeyLength = errors.New("signing: messaging private key is not exactly 64 bytes")

	// ErrVerify reports a well-formed signature, over well-formed canonical
	// bytes, under a well-formed key, that DOES NOT VERIFY.
	//
	// It deliberately says nothing about why, because there is nothing to say:
	// Ed25519 verification is a single boolean and the possible causes — a
	// tampered body, a re-labelled sender, a substituted key, a forged
	// signature — are indistinguishable from inside. The caller knows the
	// message id and the sender and logs those; this error is the verdict.
	ErrVerify = errors.New("signing: signature does not verify")
)

// ValidateSignature is the BUS-SIDE well-formedness check of SIGN-6's ingest
// policy, and the single place that check is spelled out.
//
// It is one function, exported, so that /v1/send, /v1/broadcast and the relay
// ingest path (SIGN-7) cannot drift apart. Three copies of "is it 64 bytes"
// are three chances for one of them to be written as >= or to be forgotten on
// the path that a peer bus reaches, and the relay path is precisely the
// backdoor SIGN-7 names.
//
// What it deliberately does NOT do is verify. A bus does not hold the sender's
// messaging key and must not be trusted to police messages on behalf of
// senders it does not control; trust decisions belong to the recipient. So the
// bus enforces SHAPE, the recipient enforces AUTHENTICITY, and neither is
// asked to do the other's job.
//
// A failure here is TERMINAL for the request, not transient — which matters
// under invariant 10, where a rejected send must not become a retry loop: the
// same idempotency key re-presented with the same malformed signature is
// rejected identically every time, and re-presented with a REPAIRED signature
// it is a different payload under a used key, which invariant 10 already
// handles as a protocol violation. What STATUS a caller answers with is
// SIGN-6's decision to record, not this package's to dictate.
func ValidateSignature(sig []byte) error {
	if len(sig) == 0 {
		return ErrNoSignature
	}
	if len(sig) != SignatureSize {
		return fmt.Errorf("%w: got %d bytes, want exactly %d", ErrSignatureLength, len(sig), SignatureSize)
	}
	return nil
}

// ValidatePublicKey reports whether pub is a usable Ed25519 messaging public
// key, WITHOUT calling into crypto/ed25519.
//
// It exists so that a caller holding a key from an untrusted or merely stale
// source — a roster record off disk, a peer's key bundle, a TOFU pin — can find
// out before it is standing in front of ed25519.Verify with no way to back out.
func ValidatePublicKey(pub ed25519.PublicKey) error {
	if len(pub) != PublicKeySize {
		return fmt.Errorf("%w: got %d bytes, want exactly %d", ErrPublicKeyLength, len(pub), PublicKeySize)
	}
	return nil
}

// Sign canonicalizes m and returns the detached Ed25519 signature over those
// exact bytes, made with the sender's MESSAGING private key.
//
// The messaging key is NOT the auth key (invariant 3). The auth key proves the
// agent to the BUS and the bus holds its public half; the messaging key proves
// the agent to its PEERS and is the one a compromised bus cannot forge. Two
// keys, two lifetimes, two purposes — see SIGN-8.
//
// The canonical bytes are handed to ed25519.Sign UNHASHED. Ed25519 signs the
// message, not a digest of it: crypto/ed25519 exposes no pre-hash mode, and
// passing it CanonicalDigest's output would produce a valid signature over 32
// bytes that no conforming verifier will ever reproduce (RATCHET-7). That is
// the silent-failure shape invariant 9 warns about — it would still "sign", it
// would simply protect nothing anyone checks.
//
// Nothing about the construction is ours. Canonicalize produces bytes;
// crypto/ed25519 — the audited, high-level, misuse-resistant RFC 8032 sign API
// — produces the signature. There is no padding scheme, no nonce, no framing
// beyond the documented field order, and there must never be one.
func Sign(priv ed25519.PrivateKey, m Message) ([]byte, error) {
	// Checked BEFORE ed25519.Sign, which PANICS on a wrong-size private key
	// exactly as Verify panics on a wrong-size public key. A private key
	// arrives from a file on disk that an operator may have truncated, copied
	// half of, or replaced with a public key by mistake, so this is a reachable
	// input and not a theoretical one.
	//
	// The key's LENGTH is reported; the key is not, and must never be. A
	// private key that reaches a log line or an error string returned to a
	// caller has left the machine.
	if len(priv) != PrivateKeySize {
		return nil, fmt.Errorf("%w: got %d bytes, want exactly %d", ErrPrivateKeyLength, len(priv), PrivateKeySize)
	}
	b, err := Canonicalize(m)
	if err != nil {
		// Canonicalize already failed closed: there are no bytes, so there is
		// nothing to sign. Signing a best-effort serialisation would produce a
		// signature over something nobody specified.
		return nil, err
	}
	return ed25519.Sign(priv, b), nil
}

// Verify checks sig against m under pub, FAIL-CLOSED: a nil return is the ONLY
// outcome on which a caller may hand the body to the calling agent.
//
// # WHICH KEY — the most abusable property of a detached-signature API
//
// pub is a free parameter here, and this function CANNOT check that the caller
// chose it correctly. PROTOCOL.md §8.3 makes the rule normative and it is the
// caller's to obey: the verification key MUST be resolved from the roster using
// the fully-qualified SENDER FIELD INSIDE THE SIGNED BYTES (m.Sender), and from
// NOTHING ELSE — never a key, key id, hint or sender name carried beside the
// signature in the envelope, and never a key supplied by whichever bus handed
// the message over.
//
// Get this wrong and verification is self-signed and worth nothing, while every
// test in this package still passes: an attacker that can choose the key can
// satisfy Verify for any body it likes. That is the silent failure invariant 9
// exists for, and it is why the canonical layout deliberately carries NO key
// identifier — a self-describing signature is one an attacker can point at a
// key of its choosing. Cross-bus key trust is a known open hole and is SIGN-7's
// to settle.
//
// # The panic trap, which is why the key is checked first
//
// crypto/ed25519.Verify PANICS when len(publicKey) != ed25519.PublicKeySize. It
// does not return false. That asymmetry is a remote denial of service the
// moment a key reaches this path from anywhere an attacker influences — a peer
// bus's key bundle, a roster record off damaged media, a TOFU pin file — because
// a MALFORMED SIGNATURE is handled safely and returns false while a MALFORMED
// KEY takes the process down. Verified first-hand against this box's stdlib
// source (crypto/ed25519/ed25519.go under GOROOT) and pinned by test.
//
// So the order below is load-bearing and must not be rearranged:
//
//  1. the public key's length      (else ed25519.Verify panics)
//  2. the signature's length       (so "absent" and "mangled" stay distinct)
//  3. canonicalization of m        (a message that will not canonicalize has
//     no signable bytes, so nothing can verify)
//  4. ed25519.Verify
//
// # A failure is TERMINAL, and the body is discarded, not delivered
//
// SIGN-6's poison-message rule: a message that fails here was still durably
// delivered by the bus and cannot be un-sent, so a recipient must DISCARD the
// body, RECORD the event, and ADVANCE ITS CURSOR PAST IT. Blocking the cursor
// on an unverifiable message hands any bus — or any peer that can inject one —
// a permanent denial of service against that agent for the price of a single
// bad message. Callers implement that; this function's contribution is that it
// always returns, always fails closed, and never panics.
func Verify(pub ed25519.PublicKey, m Message, sig []byte) error {
	if err := ValidatePublicKey(pub); err != nil {
		return err
	}
	if err := ValidateSignature(sig); err != nil {
		return err
	}
	b, err := Canonicalize(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, b, sig) {
		return ErrVerify
	}
	return nil
}
