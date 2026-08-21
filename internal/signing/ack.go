package signing

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// THE CANONICAL RECIPIENT-ACKNOWLEDGEMENT BYTES (ACK-CONTRACT.md §6.3).
//
// This file adds an ENCODING and NOTHING ELSE. It introduces no primitive, no
// MAC, no KDF, no padding scheme, no nonce, and no construction assembled out
// of good primitives — invariant 9 forbids all of them, and §6.3 forbids them
// again by name, including any reuse of the WAL MAC key (`wal-mac.key`) for
// anything that travels on the wire. It produces bytes; crypto/ed25519 signs
// them.
//
// # Why this exists BEFORE `agent-busctl ack` does
//
// relay.ValidateAckAttestation REQUIRES a detached signature of exactly
// SignatureSize bytes on every recipient-sourced outcome, and checks SHAPE
// ONLY. A CLI written before this file existed would still have had to sign
// SOMETHING, and whatever it happened to serialise would have become the
// de-facto encoding by accident — 64 bytes over a byte string nobody specified,
// frozen onto the wire by the first agent that ran it. That is invariant 9's
// silent failure in miniature: it would sign, it would shape-check, and it
// would protect nothing. The encoding is therefore pinned here first.
//
// # THE SPELLINGS BELOW ARE FROZEN BY THE SIGNATURE ITSELF
//
// The outcome and class tokens are encoded as their WIRE SPELLINGS, so the
// moment a recipient signs "recipient_refused_policy" that byte string is
// permanent for this format version. This package therefore owns its own frozen
// copies of the five tokens a recipient signature can contain, and does NOT
// defer to ack's or relay's vocabulary types: deferring would mean that
// renaming a constant in either of those packages silently changed the signed
// bytes and invalidated every signature ever made.
// TestAckAlphabetMatchesTheDurableVocabulary — in ackvocab_external_test.go, an
// EXTERNAL test package, which is how it may import internal/ack at all — pins
// these against the DURABLE-RECORD spellings so the two cannot drift apart
// unnoticed. It pins against internal/ack and NOT internal/relay, for a reason
// recorded in full at the head of that file.
//
// That is deliberately NOT a third declaration of the twelve-class vocabulary
// ACK-13 is about. It is the FORMAT's alphabet — the two recipient-sourced
// outcomes and the three recipient-emitted classes, which are the only tokens
// a signature is ever made over. The nine bus-emitted classes are absent
// because a bus-sourced outcome carries no attestation at all
// (relay.ValidateAckAttestation), so no signature can exist over one.

// AckFormatVersion is the version of the canonical ACKNOWLEDGEMENT signing
// format implemented here. It is a RESERVED number, allocated through the Spec
// Server `signing-format-version` namespace (value 3, reserved 2026-08-21 for
// ACK-6-FU-CLI). Nobody picks one by looking at this constant.
//
// It is a SEPARATE constant from signing.FormatVersion (1, messages) and
// attest.AttestationFormatVersion (2, bus attestations), and must stay
// separate, for the reason attest already records: FormatVersion is consumed as
// THE format version in a peer-facing error string
// (internal/relay/message.go), so hanging a second layout off it would put two
// meanings on one key.
//
// As in the message and attestation formats, the version is not a field of its
// own — it is spelled out inside AckContext, so there is exactly ONE version
// indicator in the signed bytes and no way for two of them to disagree.
// Changing ANY of the layout below — adding a field, removing one, reordering,
// changing a width, respelling a token — is a NEW VERSION with a NEW context
// string, never an in-place edit of this one.
const AckFormatVersion = 3

// AckContext is the domain-separation prefix: the first field of every
// canonical acknowledgement. A fixed, documented, length-prefixed ASCII prefix
// is framing — it is not a cryptographic construction, and nothing about the
// signature scheme depends on its content (see Context).
//
// It is PINNED as a constant here and is NEVER learned from the wire. A client
// that took its domain-separation prefix from a value some server handed it
// would be a signing oracle: whoever chose the prefix would choose which
// language the agent's key spoke, and could point it at any other artefact's
// language. auth.SessionSigningContext carries the same rule for the same
// reason, and /v1/info deliberately refuses to serve it
// (internal/httpapi/discovery.go:336).
//
// # Disjointness, checked rather than asserted — and it MATTERS MORE HERE
//
// The first field of each artefact this codebase signs:
//
//	msg-sig    0x00000013 || "agent-bus/msg-sig/1"        (19 bytes)
//	attest     0x00000016 || "agent-bus/bus-attest/2"     (22 bytes)
//	ack-sig    0x00000019 || "agent-bus/recipient-ack/3"  (25 bytes)
//
// All three differ in the LENGTH WORD ITSELF (0x13, 0x16, 0x19), so no encoding
// is a prefix of another and no byte string is a valid encoding of two of them.
// The session languages are disjoint from all three by their first byte: a
// canonical artefact always begins with the 0x00 of a uint32 length, a session
// challenge always begins with 'a' (0x61).
//
// attest could lean on a SECOND, independent separation — a bus signing key and
// an agent messaging key are different keys, so cross-protocol confusion had no
// key that spoke both languages. THIS FORMAT CANNOT. An acknowledgement is
// signed by the recipient's MESSAGING key, the very same key that signs that
// agent's messages, so the context prefix is the ONLY thing standing between an
// ACK signature and a message signature. Without it, 64 bytes attesting "I
// refused this" could be presented as 64 bytes attesting to a message body, or
// the reverse. TestAckCanonicalIsDisjointFromMessageSigning and
// TestAckSignatureDoesNotVerifyAsMessage pin both directions by MUTATION.
const AckContext = "agent-bus/recipient-ack/3"

// The two RECIPIENT-SOURCED outcomes, and the only outcomes this format
// encodes. They are the wire spellings of relay.AckDelivered and
// relay.AckRefused.
//
// "undeliverable" is deliberately ABSENT and is REFUSED by CanonicalizeAck. It
// is a routing claim asserted by a BUS, it carries no attestation at all
// (relay.ValidateAckAttestation refuses a signature on it), and a recipient
// application has no standing to make it. Encoding it here would manufacture
// signable bytes for a claim that must never be signed — and an agent that
// could sign one would be a recipient asserting, in a durable record, a fact
// about the federation's routing.
//
// The two non-terminal states (accepted, in_flight) are absent for a different
// reason: they are facts about a BUS, never about the recipient, and never
// travel on a frame. So is "unknown", which is a reporting value of the status
// API and must never be signable — an "I don't know" that could be attested is
// how a real terminal outcome gets overwritten by ignorance.
const (
	AckOutcomeDelivered = "delivered"
	AckOutcomeRefused   = "refused"
)

// The three RECIPIENT-EMITTED classes, the wire spellings of
// relay.AckRecipientRefusedPolicy, relay.AckRecipientRefusedUndecodable and
// relay.AckRecipientRefusedNotAddressed.
//
// Each reveals THAT something failed and never WHAT — that is the invariant 6
// line, and it is why there is no adjacent free-text field in this layout and
// must never be one. A signed reason string would be a body in an append-only
// log, authenticated, which is strictly worse than an unsigned one.
const (
	AckClassRecipientRefusedPolicy       = "recipient_refused_policy"
	AckClassRecipientRefusedUndecodable  = "recipient_refused_undecodable"
	AckClassRecipientRefusedNotAddressed = "recipient_refused_not_addressed"
)

// ErrInvalidAck wraps every failure of CanonicalizeAck.
//
// It is a DISTINCT sentinel from ErrInvalid rather than a wrapper around it, so
// that a caller whose ingest policy fails closed on "this is not a
// canonicalizable MESSAGE" (SIGN-6) cannot silently swallow "this is not a
// canonicalizable ACKNOWLEDGEMENT" as the same event. They are different
// artefacts, arriving on different routes, with different remedies.
//
// CanonicalizeAck NEVER returns partial or best-effort bytes. Either the input
// is a well-formed acknowledgement and the bytes are exact, or there are no
// bytes at all.
var ErrInvalidAck = errors.New("signing: acknowledgement cannot be canonicalized")

// Ack is the set of fields covered by a recipient's acknowledgement signature —
// the whole set, in the order they are encoded. A field that is not here is NOT
// protected and may be changed by any bus on the path; the omissions are
// enumerated below and each one is a decision.
//
// Provenance of each field:
//
//	CorrelationKey      SERVER-minted by the ORIGIN bus ("<bus-id>-<seq>")
//	Recipient           SERVER-minted at enrolment ("<bus-id>.<agent-id>")
//	Outcome             RECIPIENT-chosen, from a closed pair
//	Class               RECIPIENT-chosen, from a closed triple, iff refused
//	EmittedAtUnixMilli  RECIPIENT-supplied
//
// # WHAT IS DELIBERATELY NOT COVERED, AND WHY
//
//   - The relay WIRE VERSION (§9.2's wire_version). It is a hop-transport
//     field, renegotiated per hop and due to be collapsed onto relay.WireVersion
//     (ACK-3-FU-COLLAPSE-WIREVERSION). Binding it into an END-TO-END signature
//     would make every relay wire bump a flag day that invalidated every
//     recipient signature in flight. This format's own version lives in
//     AckContext, which is the only version indicator that belongs in here.
//
//   - The TRAVERSED BUS PATH and the relaying bus's local delivery sequence.
//     They grow on every hop and therefore cannot be inside end-to-end signed
//     bytes at all — the same call, for the same reason, that the message
//     format makes (see the package doc).
//
//   - The IDEMPOTENCY KEY of the ACK request (§4). It is a transport-level
//     retry token, not a semantic claim. Covering it would break the legitimate
//     retry invariant 10 protects: re-presenting the same logical ACK under a
//     fresh key would need a fresh signature, so a retry would become
//     indistinguishable from a new statement.
//
//   - The SENDER. The correlation key is the ORIGIN bus's server-minted message
//     id, which already names exactly one message and therefore exactly one
//     sender; the origin bus resolves it. A recipient has no authority over the
//     sender's identity, so a Sender field would be an unverifiable claim that
//     could disagree with the correlation key — and unlike Message.Sequence,
//     which is redundant with MessageID and is CROSS-CHECKED against it, there
//     is nothing here to cross-check it against.
//
//   - A KEY EPOCH. attest.Attestation carries one even though nothing enforces
//     it yet, and that was right for a BUS-signed artefact because the bus
//     assigns the epoch and knows it. Nothing exposes an agent's messaging-key
//     epoch to the agent — no route, no client field, no CLI output — so a
//     KeyEpoch here would be structurally zero in every signature ever made. A
//     field that is always the constant looks like a binding and is not, which
//     is worse than its absence. Adding one is a new AckFormatVersion, and the
//     task that closes §16 Q1 (nothing distributes agents' messaging public
//     keys, so nobody can verify this signature end to end today) owns that
//     call.
type Ack struct {
	// CorrelationKey is the ORIGIN bus's message id, "<bus-id>-<seq>" — the
	// correlation key ACK-CONTRACT.md §3 rules on, obtained from
	// store.Message.OriginID(). It is the EXISTING third identifier and NOT a
	// fourth: Seq is identity, Pos is delivery position, and this is
	// correlation. Confusing the three has caused three defects here.
	CorrelationKey string

	// Recipient is the fully-qualified id of the acknowledging agent,
	// "<bus-id>.<agent-id>" (invariant 2) — i.e. the SIGNER's own identity,
	// inside the signed bytes.
	//
	// This is what binds the signature to the agent that made it. Without it,
	// a signature over (key, outcome, class, time) would be transplantable onto
	// any recipient of the same message, which is precisely the gap
	// ACK-4-FU-RECIPIENT-BINDING names at layer 2. Layer 3 closes it by
	// construction, on the day anything can verify.
	Recipient string

	// Outcome is the terminal outcome, "delivered" or "refused" — and nothing
	// else. See AckOutcomeDelivered for why "undeliverable" is refused here.
	Outcome string

	// Class is the recipient-emitted refusal class, and is EMPTY when Outcome
	// is "delivered": a success has nothing to explain, and an optional class
	// on a positive ACK would create a disclosure channel where none is needed
	// (§5.4).
	//
	// The empty string is the canonical encoding of "no class", which is
	// unambiguous only because "" is never a valid class token — CanonicalizeAck
	// enforces both halves of that, so an absent class and an empty-but-present
	// class can never encode to the same bytes as two different logical
	// statements.
	Class string

	// EmittedAtUnixMilli is the recipient's wall clock: Unix milliseconds UTC,
	// as a signed 64-bit integer, encoded fixed-width.
	//
	// Milliseconds rather than nanoseconds so the value is exactly
	// representable as a JSON number, and so the wire frame carries THIS EXACT
	// INTEGER rather than a formatted timestamp a verifier has to re-parse —
	// every conversion between the wire form and the signed form is a place the
	// two sides can drift, so there is none. This mirrors
	// Message.TimestampUnixMilli, and mirrors the call BEHIND it: the covered
	// timestamp is the SIGNER's claim, never a per-bus receipt time, which is
	// what keeps the signature portable across every bus on the path.
	//
	// It is NOT a freshness mechanism: clocks lie, and a recipient's clock is
	// no better than a sender's. Replay of an acknowledgement is handled where
	// replay is always handled here — server-side, by the absorbing terminal
	// state and the idempotency decision (§8.2, §12, invariant 10) — and never
	// by trusting this number.
	EmittedAtUnixMilli int64
}

// CanonicalizeAck returns the exact bytes to be signed and verified for a.
//
// The layout is normative. All integers are big-endian; every variable-length
// field is preceded by its uint32 length, exactly as the message format does:
//
//	uint32 len || AckContext              ("agent-bus/recipient-ack/3")
//	uint32 len || CorrelationKey          ("<origin-bus-id>-<seq>")
//	uint32 len || Recipient               ("<bus-id>.<agent-id>")
//	uint32 len || Outcome                 ("delivered" | "refused")
//	uint32 len || Class                   ("" iff Outcome is "delivered")
//	int64         EmittedAtUnixMilli      (two's complement)
//
// The field COUNT is fixed and every field is length-prefixed, so the encoding
// is INJECTIVE: distinct acknowledgements always produce distinct byte strings,
// and no attacker can shift bytes across a field boundary — move the tail of a
// correlation key into the head of a recipient id, say — to present a different
// logical acknowledgement under a signature that still verifies.
//
// Every field is bounded by validation and not by a remote party's choice: the
// correlation key and the recipient by ids, the outcome and class by closed
// sets. So the canonical bytes have a fixed maximum length, which is what lets
// §9.2's request cap be derived exactly rather than guessed.
//
// The output is handed to ed25519.Sign / ed25519.Verify UNHASHED. Do not
// pre-hash it: Ed25519 signs the message itself, crypto/ed25519 exposes no
// pre-hash mode, and handing it a digest produces a signature no conforming
// verifier will ever reproduce (see CanonicalDigest's warning).
func CanonicalizeAck(a Ack) ([]byte, error) {
	if err := validateAck(a); err != nil {
		return nil, err
	}

	size := 4 + len(AckContext) +
		4 + len(a.CorrelationKey) +
		4 + len(a.Recipient) +
		4 + len(a.Outcome) +
		4 + len(a.Class) +
		8

	out := make([]byte, 0, size)
	out = appendLenPrefixed(out, []byte(AckContext))
	out = appendLenPrefixed(out, []byte(a.CorrelationKey))
	out = appendLenPrefixed(out, []byte(a.Recipient))
	out = appendLenPrefixed(out, []byte(a.Outcome))
	out = appendLenPrefixed(out, []byte(a.Class))
	// Two's-complement int64, so the LAYOUT admits a pre-1970 clock without a
	// version bump. validateAck nevertheless rejects a non-positive value as an
	// unset field, so this branch is a property of the format rather than a
	// reachable case — the same call the message format makes.
	out = appendUint64(out, uint64(a.EmittedAtUnixMilli))
	return out, nil
}

// SignAck canonicalizes a and returns the detached Ed25519 signature over those
// exact bytes, made with the RECIPIENT's MESSAGING private key.
//
// It is the messaging key — the same key Sign uses, the one attest binds to an
// agent id — and NOT the auth key (invariant 3). The auth key proves the agent
// to its BUS; the messaging key proves the agent to its PEERS, and an
// acknowledgement is a statement to the SENDER, not to any bus on the path.
// Because that one key now signs two languages, AckContext is load-bearing; see
// its doc.
//
// Nothing about the construction is ours: CanonicalizeAck produces bytes and
// crypto/ed25519 — the audited, misuse-resistant RFC 8032 sign API — produces
// the signature. There is no padding, no nonce, and no framing beyond the
// documented field order, and there must never be one.
func SignAck(priv ed25519.PrivateKey, a Ack) ([]byte, error) {
	// Checked BEFORE ed25519.Sign, which PANICS on a wrong-size private key.
	// The key's LENGTH is reported; the key is not, and must never be.
	if len(priv) != PrivateKeySize {
		return nil, fmt.Errorf("%w: got %d bytes, want exactly %d", ErrPrivateKeyLength, len(priv), PrivateKeySize)
	}
	b, err := CanonicalizeAck(a)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, b), nil
}

// VerifyAck checks sig against a under pub, FAIL-CLOSED.
//
// # NO BUS MAY CALL THIS TO GATE AN ACKNOWLEDGEMENT
//
// A bus checks SHAPE ONLY — present, exactly SignatureSize bytes — via
// relay.ValidateAckAttestation, and that is not an oversight to be tidied up by
// calling this function on the ingest path. A bus does not hold the recipient's
// messaging key, must not be trusted to police statements on behalf of senders
// it does not control, and MUST NOT claim to have verified: §6.3 gives the
// status API exactly two attestation labels, peer_bus and
// recipient_signature_unverified, and deliberately provides no value meaning
// "verified" because nothing can produce one.
//
// This function exists for the SENDER — the only party entitled to verify — and
// for this package's own round-trip and mutation tests.
//
// It is NOT REACHABLE IN PRODUCTION TODAY, and the reason is directional rather
// than a simple absence. internal/attest does bind an agent id to a messaging
// public key, and that attestation travels VERBATIM in the relay envelope — but
// it carries the SENDER's key DOWNSTREAM, which is what lets a recipient verify
// a message. Nothing carries the RECIPIENT's key back UPSTREAM to the sender:
// no route distributes it (internal/relay/rosterhttp.go's PeerRosterPath is a
// reserved path, not an implemented key service), and an acknowledgement frame
// deliberately carries no key, key id or hint — see the WHICH KEY note below.
// So a sender cannot resolve pub, and a recipient acknowledgement is today
// attributable to a bus and END TO END UNVERIFIABLE BY ANYONE (§16 Q1). That is
// a real limitation of this epic, stated here rather than papered over, and the
// task that closes Q1 is the one that makes this function reachable.
//
// # WHICH KEY — the most abusable property of a detached-signature API
//
// pub is a free parameter and this function CANNOT check the caller chose it
// correctly. The rule is the caller's to obey and it is the same rule Verify
// states: resolve the key from the fully-qualified RECIPIENT FIELD INSIDE THE
// SIGNED BYTES (a.Recipient) and from NOTHING ELSE — never a key, key id or
// hint carried beside the signature in the frame, and never a key supplied by
// whichever bus handed the acknowledgement over. Get it wrong and verification
// is self-signed and worth nothing, while every test here still passes. That is
// why the layout carries no key identifier.
//
// The check order below is load-bearing and must not be rearranged: the public
// key's length first, because crypto/ed25519.Verify PANICS rather than
// returning false on a malformed key, which is a remote denial of service the
// moment a key reaches this path from anywhere an attacker influences.
func VerifyAck(pub ed25519.PublicKey, a Ack, sig []byte) error {
	if err := ValidatePublicKey(pub); err != nil {
		return err
	}
	if err := ValidateSignature(sig); err != nil {
		return err
	}
	b, err := CanonicalizeAck(a)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, b, sig) {
		return ErrVerify
	}
	return nil
}

// validateAck checks every field. It fails closed: an id that is not
// well-formed, a token outside the closed set, or a class that disagrees with
// the outcome all stop the encoding rather than producing bytes.
func validateAck(a Ack) error {
	// The correlation key must be a server-minted ORIGIN message id. Parsing it
	// is what makes it the correlation key of §3 rather than any string a
	// caller fancied.
	if _, _, err := ids.ParseMessageID(a.CorrelationKey); err != nil {
		return fmt.Errorf("%w: correlation key: %v", ErrInvalidAck, err)
	}

	if _, _, _, err := ids.ParseAgentID(a.Recipient); err != nil {
		return fmt.Errorf("%w: recipient: %v", ErrInvalidAck, err)
	}

	// THERE IS DELIBERATELY NO CHECK THAT THE TWO BUS HALVES AGREE, and adding
	// one would be a defect rather than a hardening.
	//
	// The message format DOES bind them: Canonicalize requires the bus that
	// minted the message id to be the bus that qualifies the sender, because a
	// message is signed by an agent of its origin bus. An ACKNOWLEDGEMENT is
	// the opposite case by construction — the whole point of the relay plane is
	// that the recipient is on a DIFFERENT bus from the origin, so in A -> B the
	// correlation key's bus half is A's and the recipient's is B's. §6.2 names
	// this exact trap: "a 'the bus half must equal the ACKing peer' rule would
	// be wrong and would break multi-hop". The local-delivery case, where the
	// halves do coincide, is a special case of the general rule and not the
	// rule.
	switch a.Outcome {
	case AckOutcomeDelivered:
		if a.Class != "" {
			// §5.4. Refused rather than silently dropped: dropping would let
			// two different logical statements share one signature.
			return fmt.Errorf("%w: outcome %q carries class %q, but a positive acknowledgement has nothing to explain and carries no class at all", ErrInvalidAck, a.Outcome, elideAckToken(a.Class))
		}
	case AckOutcomeRefused:
		switch a.Class {
		case AckClassRecipientRefusedPolicy, AckClassRecipientRefusedUndecodable, AckClassRecipientRefusedNotAddressed:
		case "":
			return fmt.Errorf("%w: outcome %q requires a recipient-emitted class; an unexplained refusal is not signable", ErrInvalidAck, a.Outcome)
		default:
			// A bus-emitted class reaching here is the specific case worth
			// naming: it would be a recipient signing a ROUTING claim.
			return fmt.Errorf("%w: class %q is not one of the three a recipient may emit (%s, %s, %s); the nine bus-emitted classes are routing claims a recipient has no standing to sign", ErrInvalidAck, elideAckToken(a.Class),
				AckClassRecipientRefusedPolicy, AckClassRecipientRefusedUndecodable, AckClassRecipientRefusedNotAddressed)
		}
	case "":
		return fmt.Errorf("%w: outcome is empty; the closed pair this format signs is %s, %s", ErrInvalidAck, AckOutcomeDelivered, AckOutcomeRefused)
	default:
		return fmt.Errorf("%w: outcome %q is not one of %s, %s; in particular a recipient may not sign \"undeliverable\", which is a routing claim asserted by a bus and carries no attestation at all", ErrInvalidAck, elideAckToken(a.Outcome), AckOutcomeDelivered, AckOutcomeRefused)
	}

	if a.EmittedAtUnixMilli <= 0 {
		return fmt.Errorf("%w: emitted-at %d is not a positive Unix millisecond value; 0 means \"unset\"", ErrInvalidAck, a.EmittedAtUnixMilli)
	}
	return nil
}

// maxElidedAckToken bounds an unrecognised token echoed in an error.
const maxElidedAckToken = 32

// elideAckToken bounds a rejected token before it reaches an error string.
//
// A caller — ultimately a remote party, once a frame's fields are fed through
// here — chooses these bytes, and it must not also get to choose the size of
// the line we log about refusing them. relay.elideAck and relay.elideOutbox
// exist for the same reason; this is a local copy because internal/signing must
// not import internal/relay, which imports internal/signing.
func elideAckToken(s string) string {
	if len(s) <= maxElidedAckToken {
		return s
	}
	// Truncated on a RUNE boundary, the same way relay.elideOutbox
	// (internal/relay/outbox.go) does it, and for the reason recorded there:
	// cutting mid-rune would put an invalid UTF-8 fragment in an operator's
	// log, which the %q the callers format this with then renders as an escape
	// nobody can read back to the original bytes. A remote party chooses these
	// bytes, so a multi-byte rune at the cut is reachable input, not a
	// theoretical one.
	cut := maxElidedAckToken
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
