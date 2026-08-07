package relay

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/signing"
)

// Signature failures on the relay ingress. All are checkable with errors.Is,
// alongside ErrInvalidRelay and ErrRelayKeyMismatch in message.go.
//
// They are FIVE sentinels rather than one because they need two different
// answers on the wire and they blame three different parties. ErrMissingSignature
// and ErrUnsignable are "this envelope can never be verified by anyone" — a
// malformed request, 400. ErrNoSignerKey and ErrBadSignature are "this envelope
// is not attributable to the agent it names" — a refusal, 403. ErrUnpeeredBus is
// a refusal too, but it blames a THIRD party — the operators, on both ends, who
// have not peered these buses — and it is kept separate for exactly that reason.
// All are FINAL: a retrying peer must never be told to send the same bytes again.
var (
	// ErrMissingSignature reports an envelope with no signature, or with
	// something that cannot be one (a wrong length). The two are one error on
	// purpose: neither can be verified and neither becomes verifiable by being
	// resent, so distinguishing them only helps an attacker probe.
	ErrMissingSignature = errors.New("relay: relayed message carries no signature")

	// ErrUnsignable reports a message that signing.Canonicalize refuses, so no
	// signature over it can exist to be checked. A relayed BROADCAST is the case
	// that actually occurs today — see ValidateRelayRequest's check 11a and
	// SIGN-3.
	ErrUnsignable = errors.New("relay: relayed message cannot be canonicalized, so no signature over it can exist")

	// ErrNoSignerKey reports that this bus has no signing key it is willing to
	// attribute to the sending agent — including the case where no CrossBusTrust
	// was supplied at all. FAIL-CLOSED: "we do not know who this is" is a refusal,
	// never a skip.
	ErrNoSignerKey = errors.New("relay: no trusted signing key for the sending agent")

	// ErrUnpeeredBus reports that we hold NO PEERING-TIME PIN for the origin
	// bus's signing key, so nothing that bus's agents sign can be verified here.
	//
	// It is a SEPARATE sentinel from ErrNoSignerKey on purpose, and the reason is
	// operational rather than cryptographic: "we have never peered with your bus"
	// and "we peered with you but will not attribute this message to that agent"
	// are two completely different problems with two completely different
	// remedies — the first is fixed by an operator completing a peering, the
	// second by finding out who forged what. Collapsing them would send an
	// operator hunting a forgery that never happened, on the single most common
	// day-one failure of a federation.
	//
	// It is NOT a "not yet" and must never be answered as one: nothing the
	// sending peer can do on a retry establishes a pin. See VerifyRelayed for why
	// the absence of a pin is checked FIRST and unconditionally.
	ErrUnpeeredBus = errors.New("relay: the origin bus has no signing key pinned from peering, so its agents' signatures cannot be verified")

	// ErrBadSignature reports a signature that did not verify against the key
	// this bus attributes to the sender. The envelope is a forgery, a corruption
	// or a message signed by a key we do not associate with that agent, and
	// nothing distinguishes those three from here.
	ErrBadSignature = errors.New("relay: relayed message signature does not verify")
)

// CanonicalBytes re-derives the exact bytes the sender signed, from the fields
// THIS bus will route, deliver, attribute and log.
//
// PROTOCOL.md §8.5 is normative here: "Verify the bytes you are about to ACT on
// — never a blob that arrived beside them." The envelope carries no
// pre-serialised canonical byte string, and if it did, this method would still
// ignore it: a transported blob would have to be trusted to correspond to the
// fields next to it, and a hostile peer would simply send a blob describing an
// innocuous message alongside fields describing a different one. The signature
// would verify over the blob and the bus would route the fields.
//
// Re-derivation is only DETERMINISTIC because the canonical format was built to
// be (see internal/signing): fixed field order, fixed-width big-endian
// integers, every variable-length field length-prefixed, and the recipient set
// sorted by Canonicalize itself on a copy. JSON transports the field VALUES and
// never the signed bytes, so key order, number formatting, base64 padding and
// whitespace may all change on any hop without affecting anything.
//
// OriginSeq is NOT a second wire field: it is parsed out of OriginMessageID by
// ValidateRelayRequest, so the envelope makes ONE claim about the sequence and
// not two that could disagree. signing.Canonicalize nevertheless re-checks that
// the integer and the id's sequence half agree, which is why passing both here
// is a cross-check rather than a duplication.
//
// Any failure is wrapped in ErrUnsignable: if the message cannot be
// canonicalized then there are no bytes anybody could have signed, and there is
// nothing to verify rather than something that failed to verify.
func (m RelayedMessage) CanonicalBytes() ([]byte, error) {
	b, err := signing.Canonicalize(signing.Message{
		MessageID:          m.OriginMessageID,
		Sequence:           m.OriginSeq,
		Sender:             m.Sender,
		Recipients:         m.Recipients,
		TimestampUnixMilli: m.TimestampUnixMilli,
		Body:               m.Body,
	})
	if err != nil {
		// The error text from signing is bounded and quotes only ids it has
		// itself validated; the body is never in it.
		return nil, fmt.Errorf("%w: %v", ErrUnsignable, err)
	}
	return b, nil
}

// CrossBusTrust is the ONLY way a relayed signature becomes verifiable.
//
// # THE TRUST MODEL IS DECIDED, AND THIS INTERFACE IS ITS SHAPE
//
// DECISIONS.md (2026-08-07, "Cross-bus key trust: pin the origin bus key at
// peering, no TOFU"): a relayed message keeps the ORIGIN bus's own attestation
// of its agent's messaging key INTACT, signed by the ORIGIN BUS'S SIGNING KEY,
// and that signing key is PINNED AT PEERING TIME. An intermediate bus may
// FORWARD an attestation; it may never RE-ATTEST one. A bus we have not peered
// with cannot have its agents' signatures trusted, ever.
//
// THERE IS NO TRUST-ON-FIRST-USE ANYWHERE — not as a mode, not as a fallback,
// not as a hook for one. The relay-specific reason, which is stronger than the
// consistency argument with the invite blob: TOFU's exposure window is the
// moment of FIRST CONTACT, which is exactly the moment a hostile intermediate is
// best placed to act. And a TOFU FALLBACK is not a narrowing of the hole but the
// whole of it, because it applies to every peer we have not yet seen — which is
// every peer, once.
//
// # Why TWO methods, and why the pins are a PARAMETER of the second
//
// This replaced a single-method SignerKeyResolver — SignerKey(fqAgentID) — and
// the replacement is the entire point rather than a tidy-up. A one-method key
// oracle is indistinguishable, from the caller's side, between an implementation
// that checks the origin bus's attestation and one that simply believes whatever
// the NEAREST bus re-attested. Both satisfy the signature. So the hostile-
// intermediate attack lived in the space the interface left open: a compromised
// hop substitutes ITS OWN key for the sender's, signs whatever it likes, and
// every signature verifies — proving only that the nearest bus asserted
// something, which is exactly what the unsigned envelope already asserted.
//
// Passing the peering-time pins INTO AttestedSignerKey closes that space
// STRUCTURALLY: an implementation is handed the only keys it is permitted to
// check an attestation against, and has nothing else to check one against. The
// rule "verify against the ORIGIN bus and nothing else" stops being a comment an
// implementor may overlook and becomes the only thing the signature admits.
//
// This package ships NO implementation of this interface and NO default. That
// omission is deliberate and is not an oversight to be helpfully corrected: a
// default is the one thing every wiring site would reach for, and there is no
// default that is safe. There is likewise no "verification disabled" mode.
type CrossBusTrust interface {
	// PinnedBusSigningKeys returns the ORIGIN bus's Ed25519 BUS SIGNING keys as
	// pinned at PEERING time — the keys with which that bus attests its own
	// agents' messaging keys.
	//
	// NEVER THE TLS KEY. DECISIONS.md (2026-08-07, "Bus TLS key and bus signing
	// key are separate") splits the two: the TLS key authenticates the
	// CONNECTION and its certificate fingerprint travels in the invite blob to be
	// pinned by CLIENTS; the SIGNING key attests AGENT KEY BUNDLES and is pinned
	// by PEERS at peering time. A peer pins TWO things, obtained at DIFFERENT
	// MOMENTS, and only the second one is what this method returns.
	//
	// They are separate because their rotations have incompatible blast radii:
	// rotating the TLS key affects one bus's clients and the two-certificate
	// rollover makes it non-disruptive, while rotating the SIGNING key
	// invalidates pins held by EVERY PEER BUS — a federation-wide event. Fused
	// into one key, every routine TLS rotation would inherit the federation-wide
	// cost, and the predictable outcome is that neither key is ever rotated. They
	// also keep the failure domains apart: a compromised TLS key lets an attacker
	// impersonate the bus to CLIENTS, while a compromised signing key lets it
	// forge attestations for ANY AGENT ON THE BUS. One key would make the lesser
	// compromise automatically become the greater one.
	//
	// An empty slice and an error mean the same thing to VerifyRelayed —
	// ErrUnpeeredBus — so an implementation is free to use whichever is natural.
	// Returning either is always SAFE; there is no "unknown, so allow" outcome to
	// reach by accident.
	//
	// MORE THAN ONE KEY IS RETURNED ONLY DURING A SIGNING-KEY ROLLOVER WINDOW,
	// mirroring the two-certificate TLS rollover: both the outgoing and the
	// incoming key are pinned for the overlap, so a federation-wide rotation does
	// not have to be simultaneous. It is NOT a general-purpose key list to be
	// stuffed with anything we might like to accept.
	PinnedBusSigningKeys(busID string) ([]ed25519.PublicKey, error)

	// AttestedSignerKey returns the messaging (signing) public key that the
	// ORIGIN bus attests for fqAgentID, having verified that attestation against
	// one of pinnedOriginBusSigningKeys AND NOTHING ELSE.
	//
	// "And nothing else" is the whole contract. An implementation MUST NOT accept
	// a key because a peer presented it alongside the message (presentation is
	// not attestation), MUST NOT accept an intermediate bus's re-attestation, and
	// MUST NOT fall back to trusting a key it has merely seen before. If the
	// attestation does not verify against one of the pins it was handed, the
	// answer is an error.
	//
	// THIS METHOD DOES NOT DEFINE CRYPTO-4'S KEY BUNDLE. The bundle's BYTES, its
	// TRANSPORT (carried inside the relay envelope versus fetched from the origin
	// bus), and `key_epoch` are all CRYPTO-4's to settle, and none of those wire
	// numbers is reserved yet — nobody picks one by eyeballing a list (CLAUDE.md,
	// "Parallel-agent coordination"). What this interface fixes is only the SEAM:
	// whatever bundle format CRYPTO-4 ships, it is verified against a
	// PEERING-TIME PIN of the origin bus's signing key, because that is the only
	// input this method is given.
	//
	// Returning an error is always safe: VerifyRelayed turns any error, and any
	// key of the wrong size, into ErrNoSignerKey, which is a refusal.
	AttestedSignerKey(fqAgentID string, pinnedOriginBusSigningKeys []ed25519.PublicKey) (ed25519.PublicKey, error)
}

// VerifyRelayed verifies m's signature against the messaging key the ORIGIN bus
// attests for m.Sender, under a signing key pinned when we peered with that bus.
//
// # FAIL-CLOSED, INCLUDING ON A NIL TRUST
//
// trust == nil is ErrNoSignerKey, never a skip. There is no "verification
// disabled" mode and no flag that produces one: a nil-means-allow branch is a
// single missing argument away from an unauthenticated relay ingress, and it
// would be invisible in every test that happens to pass a CrossBusTrust. The
// same posture covers a trust that errors and one that returns a key of the
// wrong size — both are "we do not know who this is", which is a refusal.
//
// # THE ORDER OF THE CHECKS IS THE SECURITY PROPERTY, NOT AN OPTIMISATION
//
//  1. A nil trust.
//  2. The signature's presence and length — the cheapest thing that can be
//     wrong, and it is wrong independently of who sent it.
//  3. THE PEERING-TIME PIN FOR m.OriginBus, UNCONDITIONALLY AND BEFORE ANY KEY
//     LOOKUP. No pins, or an error fetching them, is ErrUnpeeredBus and the
//     function returns. This is what makes "an unpeered bus's messages cannot be
//     verified, BY CONSTRUCTION" a property of the CODE rather than a claim in
//     prose: there is no branch below this one that can be reached without a pin,
//     so no later step can be talked into supplying a key from somewhere else.
//     THIS IS INTENDED BEHAVIOUR AND IT IS NOT A GAP. If a message from a bus we
//     have never peered with is being refused, the remedy is to PEER WITH THAT
//     BUS — never to add a trust-on-first-use fallback here.
//  4. Every pin is a real Ed25519 public key. A short or over-long pin is
//     ErrUnpeeredBus, not a best-effort attempt with the rest: a malformed pin
//     means the pinning store is wrong, and proceeding would verify against a
//     subset of what the operator believes is pinned.
//  5. The origin bus's attestation for m.Sender, checked against those pins and
//     nothing else.
//  6. The canonical bytes, re-derived from the fields THIS bus will act on.
//  7. ed25519.Verify.
//
// Steps 3 and 5 are ordered that way for cost as well: an envelope from a bus we
// do not federate with costs one pin lookup rather than an attestation check and
// a re-encoding of a 64 KiB body.
//
// # THE PINS DO NOT VERIFY THE MESSAGE, AND MUST NEVER BE TRIED AGAINST IT
//
// The obvious wrong reading of "there may be several pins" is to loop step 7
// over them until one verifies. That would be verifying the AGENT's message
// signature with the BUS's signing key, which is a category error: the pins
// attest the agent's key BUNDLE (step 5), and the agent's own messaging key
// signs the MESSAGE (step 7). Multiple pins exist for ONE reason — a
// signing-key ROLLOVER window, mirroring the two-certificate TLS rollover — and
// they are consumed entirely by step 5. Exactly one messaging key comes out of
// step 5, and exactly one Verify runs.
//
// The verification itself is crypto/ed25519's high-level Verify and nothing
// else: no primitive assembly, no pre-hashing (Ed25519 signs the message, and
// signing.Canonicalize's output is handed over UNHASHED), no partial or
// best-effort result. CLAUDE.md invariant 9.
func VerifyRelayed(m RelayedMessage, trust CrossBusTrust) error {
	// 1. A nil trust is a refusal, not a skipped check.
	if trust == nil {
		return fmt.Errorf("%w: no cross-bus trust was supplied, and an absent trust is a refusal rather than a skipped check", ErrNoSignerKey)
	}

	// 2. The signature is present and is the right length.
	if len(m.Signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: the message carries %d signature bytes, and an Ed25519 signature is exactly %d", ErrMissingSignature, len(m.Signature), ed25519.SignatureSize)
	}

	// 3. THE PEERING-TIME PIN, UNCONDITIONALLY AND FIRST. An origin bus we hold
	// no pin for is refused HERE, before anything can look a key up by any other
	// route — which is what makes the "unpeered means unverifiable" rule
	// structural. Do not add a fallback branch to this step; see the doc above.
	//
	// m.OriginBus is a validated, bounded bus id by the time a RelayedMessage
	// exists (ValidateRelayRequest check 3, which bounds it BEFORE anything
	// quotes it), so quoting it here is safe.
	pins, err := trust.PinnedBusSigningKeys(m.OriginBus)
	if err != nil {
		return fmt.Errorf("%w: origin bus %q: %v", ErrUnpeeredBus, m.OriginBus, err)
	}
	if len(pins) == 0 {
		return fmt.Errorf("%w: origin bus %q; peer with that bus to pin its signing key — there is deliberately no trust-on-first-use fallback", ErrUnpeeredBus, m.OriginBus)
	}

	// 4. Every pin is a real Ed25519 public key. A malformed pin means our
	// PINNING STORE is wrong, not that this peer did anything, so we refuse
	// rather than quietly verify against whichever pins happen to be well-formed.
	for i, pin := range pins {
		if len(pin) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: pin %d for origin bus %q is %d bytes, and an Ed25519 public key is exactly %d; a malformed pin is refused rather than skipped, because skipping it would verify against less than the operator believes is pinned", ErrUnpeeredBus, i, m.OriginBus, len(pin), ed25519.PublicKeySize)
		}
	}

	// 5. The ORIGIN bus's attestation for this sender, checked against those pins
	// and nothing else. The pins go no further than this call.
	//
	// m.Sender is likewise a validated, bounded, fully-qualified agent id.
	pub, err := trust.AttestedSignerKey(m.Sender, pins)
	if err != nil {
		return fmt.Errorf("%w: sender %q: %v", ErrNoSignerKey, m.Sender, err)
	}
	// The length check is signing.ValidatePublicKey's and not a second copy of
	// it. ed25519.Verify PANICS on a wrong-sized public key, so the guard is a
	// remote-DoS control rather than a tidiness one — and a control that exists
	// in two places is a control that drifts in one of them. internal/signing
	// owns it and has the tests that pin it (security gate, 2026-08-07 P2).
	if err := signing.ValidatePublicKey(pub); err != nil {
		return fmt.Errorf("%w: the attested key for sender %q is unusable: %v", ErrNoSignerKey, m.Sender, err)
	}

	// 6. The bytes we are about to ACT on, re-derived — never a blob that arrived
	// beside them (PROTOCOL.md §8.5).
	canonical, err := m.CanonicalBytes()
	if err != nil {
		return err
	}

	// 7. ONE Verify, with the ONE messaging key step 5 attested. The pins are not
	// tried here; see "THE PINS DO NOT VERIFY THE MESSAGE" above.
	if !ed25519.Verify(pub, canonical, m.Signature) {
		// Neither the signature nor the key is echoed: the pair is what an
		// attacker chooses, and there is no diagnosis in either. The ids are
		// enough to find the message in the audit trail.
		return fmt.Errorf("%w: origin message %s from sender %q", ErrBadSignature, m.OriginMessageID, m.Sender)
	}
	return nil
}
