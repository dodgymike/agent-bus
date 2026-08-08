package relay

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// Signature failures on the relay ingress. All are checkable with errors.Is,
// alongside ErrInvalidRelay and ErrRelayKeyMismatch in message.go.
//
// They are SIX sentinels rather than one because they need two different
// answers on the wire and they blame three different parties.
// ErrMissingSignature, ErrMissingAttestation and ErrUnsignable are "this
// envelope can never be verified by anyone" — a malformed request, 400.
// ErrNoSignerKey and ErrBadSignature are "this envelope is not attributable to
// the agent it names" — a refusal, 403. ErrUnpeeredBus is a refusal too, but it
// blames a THIRD party — the operators, on both ends, who have not configured
// the origin bus's pins — and it is kept separate for exactly that reason. All
// are FINAL: a retrying peer must never be told to send the same bytes again.
//
// THESE ARE NOT THE ONLY SENTINELS A CALLER MAY TEST FOR (RELAY-27). A failure
// coming out of CrossBusTrust keeps its OWN sentinel as well — errors.Is finds
// it THROUGH the relay sentinel, because VerifyRelayed returns an
// *attributionError carrying both. That holds for any error a trust returns
// rather than for an enumerated set — attest exports SEVEN sentinels today and
// the wrapping is uniform, so it covers the two (ErrOriginBusMismatch,
// ErrNoClock) that the five RELAY-27 was filed about do not name — with ONE
// exception: an error that already carries a relay wire answer of its own is
// severed from the chain, so a trust cannot steer the ingress by wrapping one of
// this package's sentinels. See AttestedSignerKey's doc and the guard in
// VerifyRelayed.
//
// The five it does name are attest.ErrExpired, attest.ErrUnpinned,
// attest.ErrAgentIDMismatch, attest.ErrInvalid and attest.ErrVerify. Before
// RELAY-27 all of them collapsed into ErrNoSignerKey and every one answered a
// peer "bad_signature" — telling an operator to hunt a forgery on the two most
// common non-forgeries there are, an unfinished peering and a message that
// queued past its attestation's expiry.
var (
	// ErrMissingSignature reports an envelope with no signature, or with
	// something that cannot be one (a wrong length). The two are one error on
	// purpose: neither can be verified and neither becomes verifiable by being
	// resent, so distinguishing them only helps an attacker probe.
	ErrMissingSignature = errors.New("relay: relayed message carries no signature")

	// ErrMissingAttestation reports an absent or malformed origin-bus
	// attestation. Such an envelope can never be attributed, regardless of its
	// message signature, so it is a malformed request (400), not an attribution
	// refusal (403).
	ErrMissingAttestation = errors.New("relay: relayed message carries no usable origin-bus attestation")

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

// attributionError carries TWO answers at once: the RELAY sentinel that decides
// what the PEER is told, and the CAUSE, whose own sentinels must survive to
// local callers and to our logs.
//
// # WHY A TYPE AND NOT TWO %w VERBS — READ THIS BEFORE "SIMPLIFYING" IT
//
// The obvious spelling is fmt.Errorf("%w: …: %w", sentinel, cause), and it is
// WRONG ON THIS TOOLCHAIN. Multiple %w verbs are a go1.20 feature. go.mod pins
// go1.19 and the digest-pinned builder at Dockerfile:15 is golang:1.19.4, where
// fmt wraps NEITHER operand and renders the second literally as
// "%!w(*errors.errorString=&{…})". errors.Is then answers false for BOTH — so
// the naive fix would deliver no cause AND silently destroy
// errors.Is(err, ErrNoSignerKey), which ErrorCode depends on for the wire code.
//
// go vet DOES catch that exact spelling — "fmt.Errorf call has more than one
// error-wrapping directive %w" — and vet is mandated before every commit, so the
// literal two-%w edit is caught by the toolchain rather than by us. What vet
// does NOT catch is the same mistake reached any other way: a %v that should
// have been a %w, a helper that drops the chain, or an Unwrap that returns the
// wrong operand — and no positive test notices either, because a passing path
// never inspects an error. TestSignedRelayPreservesAttestSentinels therefore
// asserts errors.Is in BOTH directions on the SAME error value, and additionally
// fails on any "%!w(" in the text so the outcome is legible rather than
// mysterious if one ever slips through. errors.Join is unavailable for the same
// go1.20 reason.
//
// Unwrap exposes the CAUSE and Is answers for the RELAY sentinel: one value, two
// chains, no fmt verb involved in either.
type attributionError struct {
	// sentinel is the relay-level sentinel ErrorCode maps onto the wire. It is
	// ALWAYS one of this file's package-level sentinels — a leaf error — which
	// is what lets Is compare directly and terminate.
	sentinel error

	// cause is the CrossBusTrust implementation's own error, kept INTACT so its
	// sentinels remain reachable with errors.Is.
	//
	// It is NIL when that error already carried a relay wire answer of its own —
	// see the guard at the construction site, which refuses to let a trust
	// launder an attribution refusal into some other outcome. The diagnosis is
	// not lost when that happens; it is in msg.
	cause error

	// msg is rendered once at construction. It quotes only ids this package has
	// already validated and bounded, and it never leaves the process: the relay
	// handler answers a peer with the CODE alone (relayhttp.go, "the detailed
	// error stays local").
	msg string
}

func (e *attributionError) Error() string { return e.msg }

// Unwrap yields the CAUSE, which is what makes attest.ErrExpired,
// attest.ErrUnpinned, attest.ErrAgentIDMismatch, attest.ErrInvalid and
// attest.ErrVerify reachable through this error instead of stopping dead at the
// relay sentinel.
//
// It returns NIL when the cause was severed by the guard in VerifyRelayed — see
// the cause field above. errors.Is then stops after Is, which is the intent: the
// relay sentinel still answers, and only the hijack is refused.
func (e *attributionError) Unwrap() error { return e.cause }

// Is answers for the RELAY sentinel. errors.Is consults this method BEFORE it
// unwraps, so one value matches both its relay sentinel and everything in the
// cause chain.
//
// The comparison is a direct == and not a recursive errors.Is: sentinel is
// always a leaf from this file, so there is nothing beneath it to search, and a
// direct compare provably cannot re-enter this method. Comparing two interface
// values whose dynamic types differ yields false rather than panicking, so a
// non-comparable target is safe here.
func (e *attributionError) Is(target error) bool { return e.sentinel == target }

// relaySentinelForTrustError decides which relay sentinel — and therefore which
// wire code and status — a CrossBusTrust failure is answered with.
//
// It maps onto sentinels that ALREADY EXIST. No wire code is invented here:
// every code these reach is already in ErrorCode (peer.go) and already in the
// peerErrorCode allow-list RELAY-9 tightened (client.go), so a taxonomy fix does
// not become a protocol change and neither of those files is touched.
//
// The arms are MOST SPECIFIC FIRST and must stay that way, mirroring ErrorCode.
//
//   - attest.ErrUnpinned -> ErrUnpeeredBus (403 CodeUnpeeredBus). This is
//     attest's own documented instruction ("Callers map this onto relay's
//     ErrUnpeeredBus, whose remedy is an operator action, never a retry") and it
//     is the single most valuable arm: it is the ONLY diagnosis here with an
//     operator remedy — complete the peering — and it is the ordinary day-one
//     state of an unfinished federation. Answered "bad_signature", as it was
//     before RELAY-27, it sends an operator hunting a forgery that never
//     happened.
//
//   - attest.ErrInvalid -> ErrInvalidRelay (400 CodeInvalidRelay). Also attest's
//     own instruction: "a caller mapping this package's errors onto the wire
//     should answer it as a malformed request, not as a refusal to attribute."
//     The peer sent an attestation that cannot even be canonicalized, so nobody
//     could check it — a 400, not the 403 it used to get. NOT CodeUnsigned:
//     that tells a peer it did not sign its message, which is false and
//     misdirecting — the message may be signed perfectly well; what is malformed
//     is the ATTESTATION beside it, which is a bad field in the envelope, which
//     is exactly what ErrInvalidRelay means.
//
//   - everything else -> ErrNoSignerKey (403 CodeBadSignature). FAIL-CLOSED, and
//     it is the default rather than an enumeration on purpose: a CrossBusTrust
//     is an interface anyone may implement, so an error this function has never
//     seen — from a store, a future attest sentinel, a wiring bug — must land on
//     a REFUSAL. An unrecognised failure that fell through to "allow" would be
//     the unauthenticated relay ingress this whole file exists to prevent.
//
// WHAT TAKES THE DEFAULT ARM, AND THE TWO GAPS LEFT IN IT. attest.ErrVerify,
// attest.ErrAgentIDMismatch, attest.ErrOriginBusMismatch, attest.ErrExpired and
// attest.ErrNoClock all answer ErrNoSignerKey / 403 bad_signature. For the first
// three that is the right answer — they are genuine refusals to attribute. The
// other two are KNOWN GAPS, deliberately left (follow-up RELAY-27-FU-EXPIRED):
//
//   - attest.ErrExpired: a peer is told "bad_signature" for an EXPIRED
//     attestation, which attest itself warns is far more often an honest message
//     queued across a partition than a forgery.
//   - attest.ErrNoClock: a LOCAL wiring fault of OURS — attest gives it a
//     separate sentinel precisely so it "cannot be reported to a peer as its bad
//     request" — yet it answers a peer 403, i.e. non-retriable, so our bug makes
//     the peer permanently drop a message that was fine.
//
// Both are PRE-EXISTING, not RELAY-27 regressions: before this change everything
// here was bad_signature. Fixing either on the WIRE needs a stable code that is
// RESERVED, never chosen (CLAUDE.md), plus arms in handshake.go, peer.go and
// client.go — three files this task does not own. What RELAY-27 does deliver is
// that the SENTINEL now survives, so this bus's own logs and callers can tell all
// five apart even while the peer-facing code is still coarse.
func relaySentinelForTrustError(err error) error {
	switch {
	case errors.Is(err, attest.ErrUnpinned):
		return ErrUnpeeredBus
	case errors.Is(err, attest.ErrInvalid):
		return ErrInvalidRelay
	default:
		return ErrNoSignerKey
	}
}

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
// *PeerStore is the production implementation. It reads only durable,
// operator-configured origin-bus pins and refuses stores built without the
// RELAY-34 withdrawal floor. There is deliberately NO default and likewise no
// "verification disabled" mode.
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
	// originAttestation is the value carried beside the message from the ORIGIN
	// bus and forwarded verbatim by intermediates. An implementation verifies
	// that exact value; it must not fetch or substitute another binding.
	//
	// RETURNING AN ERROR IS ALWAYS SAFE, AND THE ERROR YOU RETURN IS KEPT
	// (RELAY-27). Every failure is a REFUSAL — there is no error this method can
	// return that becomes an allow, and a key of the wrong size is a refusal too.
	// What the error SELECTS is which refusal:
	//
	//	attest.ErrUnpinned  -> relay.ErrUnpeeredBus  (403 unpeered_bus)
	//	attest.ErrInvalid   -> relay.ErrInvalidRelay (400 invalid_relay)
	//	anything else       -> relay.ErrNoSignerKey  (403 bad_signature)
	//
	// and the error you return stays reachable with errors.Is through the one
	// VerifyRelayed returns. So an implementation that wraps attest's sentinels
	// gets attest's taxonomy on the wire and in the log for free, and one that
	// returns anything else still fails closed.
	//
	// WITH ONE EXCEPTION, AND IT IS AN INSTRUCTION: DO NOT WRAP AN ERROR THAT
	// ALREADY HAS A WIRE ANSWER — every relay sentinel, and idem's key errors
	// too. If your error carries a relay wire answer of its own — anything
	// ErrorCode recognises — it is SEVERED from the errors.Is chain and kept only
	// in the message text. Otherwise a trust could steer the ingress by wrapping,
	// say, ErrRelayLoop, which is answered 200 "settled, dropped" before
	// ErrorCode is ever consulted, turning your refusal into a routine loop drop.
	// A relay sentinel is relay's answer to give, not yours; return attest's
	// sentinels, or your own.
	//
	// This REPLACES the previous contract, under which VerifyRelayed flattened
	// every error here into ErrNoSignerKey. That flattening was deliberate but
	// wrong in effect: it answered a peer "bad_signature" — forgery — for an
	// unfinished peering and for an expired attestation alike. See
	// relaySentinelForTrustError.
	AttestedSignerKey(fqAgentID string, originAttestation attest.Attestation, pinnedOriginBusSigningKeys []ed25519.PublicKey) (ed25519.PublicKey, error)
}

// validateOriginAttestation checks the wire shape without trusting the
// attestation. Canonicalize owns the field bounds and timestamp rules;
// ValidateSignature owns the panic-preventing Ed25519 length check.
func validateOriginAttestation(a attest.Attestation) error {
	if _, err := attest.Canonicalize(a); err != nil {
		return fmt.Errorf("%w: %v", ErrMissingAttestation, err)
	}
	if err := signing.ValidateSignature(a.Signature); err != nil {
		return fmt.Errorf("%w: %v", ErrMissingAttestation, err)
	}
	return nil
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
// Fail-closed is about the OUTCOME, not the diagnosis. Every trust failure is
// still a refusal; RELAY-27 only stopped them all being the SAME refusal. An
// error this file has never seen still lands on ErrNoSignerKey — the default arm
// of relaySentinelForTrustError is a refusal precisely so an unrecognised
// failure can never become an allow.
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
//     nothing else. A failure here is a refusal WHOSE SENTINEL IS PRESERVED
//     (RELAY-27): relaySentinelForTrustError picks the relay sentinel that fixes
//     the wire code, and the trust's own error — attest.ErrExpired,
//     attest.ErrUnpinned, attest.ErrAgentIDMismatch, attest.ErrInvalid,
//     attest.ErrVerify — stays reachable with errors.Is on the SAME returned
//     value. It is no longer flattened into ErrNoSignerKey.
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

	// 2b. A zero or malformed attestation is refused independently of
	// ValidateRelayRequest. VerifyRelayed is exported and must remain fail-closed
	// when a caller constructs a RelayedMessage directly.
	if err := validateOriginAttestation(m.OriginAttestation); err != nil {
		return err
	}

	// 2c. Repeat the origin/sender namespace binding here for the same standalone
	// reason. Without it a bus whose signing key is pinned for origin A could
	// attest a foreign B.agent and have a directly-constructed message attributed
	// across namespaces. Deriving attest.Subject.OriginBus from Sender alone would
	// make that check tautological.
	if len(m.OriginBus) > MaxPeerBusIDLen {
		return fmt.Errorf("%w: origin bus is %d bytes, over the %d-byte bound", ErrInvalidRelay, len(m.OriginBus), MaxPeerBusIDLen)
	}
	if err := ids.ValidateBusID(m.OriginBus); err != nil {
		return fmt.Errorf("%w: origin bus: %v", ErrInvalidRelay, err)
	}
	senderBus, _, _, err := ids.ParseAgentID(m.Sender)
	if err != nil {
		return fmt.Errorf("%w: sender: %v", ErrInvalidRelay, err)
	}
	if senderBus != m.OriginBus {
		return fmt.Errorf("%w: sender %q belongs to bus %q, not origin bus %q", ErrInvalidRelay, m.Sender, senderBus, m.OriginBus)
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
	pub, err := trust.AttestedSignerKey(m.Sender, cloneAttestation(m.OriginAttestation), pins)
	if err != nil {
		// RELAY-27: the trust's error keeps its OWN sentinel. relaySentinelFor-
		// TrustError picks the relay sentinel that fixes the wire answer, and
		// attributionError carries BOTH, because go1.19 cannot wrap two errors
		// with fmt (see attributionError's doc — the two-%w spelling wraps
		// neither and would break errors.Is for both).
		sentinel := relaySentinelForTrustError(err)

		// THE CAUSE MAY NOT CARRY A RELAY WIRE ANSWER OF ITS OWN (security gate,
		// RELAY-27 P2-1). Keeping the cause reachable is the point of this
		// change, but it makes it reachable by EVERY errors.Is in the ingress,
		// not only the ones looking for attest — and one of those is not a
		// refusal at all: relayhttp.go tests ErrRelayLoop and answers 200
		// "settled, dropped" BEFORE ErrorCode is ever consulted. A trust whose
		// error wrapped ErrRelayLoop would therefore have an ATTRIBUTION REFUSAL
		// laundered into a routine loop drop — counted as loopDrops, logged Info
		// rather than Warn, and the peer told not to retry a message we in fact
		// refused. That is invariant 6's "every discard is LOGGED, loudly and
		// specifically" failing silently.
		//
		// ErrorCode is the SELF-MAINTAINING test for "this error already means
		// something to relay": it answers CodeInternal for everything it does not
		// recognise, so a cause that already has a code is dropped from the
		// CHAIN while remaining in the TEXT above — where the diagnosis is wanted
		// and where it decides nothing. A hand-written list of relay sentinels
		// here would go stale the first time one was added elsewhere.
		//
		// internal/attest cannot trip this (it imports only ids and signing, so
		// it cannot name a relay sentinel), and that is exactly why the guard is
		// code rather than a sentence in the interface doc: CrossBusTrust is an
		// INTERFACE, and a future in-tree implementation is under no such
		// constraint.
		//
		// ACCEPTED RESIDUAL (security gate re-verification, LOW): an error whose
		// Is() method is STATEFUL — false on this one classifying probe, true
		// when the ingress asks — still slips through. No single classification
		// can be immune to an Is() that lies, and reaching it requires a hostile
		// in-tree CrossBusTrust, which is already trusted code inside the trust
		// boundary. Not worth a second probe or a defensive copy.
		//
		// The other half of this rule is stated as an INSTRUCTION to implementors
		// on AttestedSignerKey ("do not wrap a relay sentinel"), because a cause
		// wrapping BOTH an attest sentinel and a relay-coded error would lose the
		// attest one here. That is unreachable through fmt on go1.19 — it needs a
		// hand-written multi-Is type — so it is documented rather than coded
		// against.
		cause := err
		if ErrorCode(err) != CodeInternal {
			cause = nil
		}
		return &attributionError{
			sentinel: sentinel,
			cause:    cause,
			msg:      fmt.Sprintf("%s: sender %q: %v", sentinel, m.Sender, err),
		}
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
