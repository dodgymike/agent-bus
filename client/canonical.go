package client

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file is a PINNED MIRROR of internal/signing/canonical.go and
// internal/signing/sign.go.
//
// # Why it is a copy and not an import
//
// client/doc.go forbids this package from importing anything under internal/:
// an agent EMBEDDING this client is a required audience (invariant 7), and Go
// forbids any other module from importing an internal/ path, so an import here
// would silently foreclose that. Every constant this package shares with the
// server — SessionSigningContext, AgentNamePattern, the route paths — is
// therefore pinned as a literal with a comment naming the server-side
// definition it mirrors. This is the same pattern, applied to a byte layout
// instead of a string.
//
// # Divergence FAILS CLOSED, which is the right direction for a duplicate
//
// If this file and internal/signing/canonical.go ever disagree about the
// context string, a field's order, a length prefix's width, the recipient sort,
// or the endianness of an integer, the two sides produce DIFFERENT bytes for
// the same message — so a signature made here simply does not verify there, and
// vice versa. Nothing is silently accepted; a message is refused. That is the
// only acceptable way for a duplicated definition to break, and it is why the
// duplication is tolerable at all.
//
// The dangerous direction — a divergence that still verifies but protects
// something other than what the reader thinks — is impossible here BECAUSE the
// layout is total: every covered field is length-prefixed, so no two distinct
// field values can produce the same byte sequence, and a field cannot be
// smuggled from one slot into the next.
//
// # This is FRAMING, not cryptography (invariant 9)
//
// Everything below is serialisation: length prefixes, big-endian integers, a
// sort and a domain-separation string. There is no padding scheme, no nonce, no
// IV, no KDF and no construction assembled out of primitives, and there must
// never be one. The signature itself is crypto/ed25519's high-level Sign/Verify
// and NOTHING else. If you find yourself wanting to hash before signing, or to
// add a "salt", stop: RFC 8032 Ed25519 signs the message, and a digest handed
// to ed25519.Sign produces a signature over 32 bytes that no conforming
// verifier will ever reproduce — it still "signs", it simply protects nothing.

// MessageSigningContext is the domain-separation prefix and the FIRST field of
// every canonical byte sequence. Mirrors internal/signing.Context.
//
// It exists so a signature over a message can never be mistaken for, or
// replayed as, a signature over some other agent-bus artefact that happens to
// serialise to the same bytes. The other input this same agent's keys sign is
// SessionSigningContext ("agent-bus:session-token:v1:" + token), and the two
// languages are disjoint in their FIRST BYTE: a canonical message always begins
// with the 0x00 of a uint32 length, a session challenge always begins with 'a'.
//
// The format version is spelled INSIDE this string rather than carried as a
// field of its own, so there is exactly one version indicator in the signed
// bytes and no way for two of them to disagree. Changing ANY of the layout below
// — adding a field, removing one, reordering, changing a width — is a new
// version with a new context string, never an in-place edit of this one.
const MessageSigningContext = "agent-bus/msg-sig/1"

// MessageSigningFormatVersion is the version the layout below implements.
// Mirrors internal/signing.FormatVersion, a RESERVED number allocated through
// the Spec Server `signing-format-version` namespace. Nobody picks one by
// looking at this constant.
//
// Version 1 is bound EXCLUSIVELY to Ed25519: the algorithm is not a negotiable
// field, so no verifier can be talked into a weaker one by anything on the wire.
const MessageSigningFormatVersion = 1

// Bounds of the canonical FORMAT, mirroring internal/signing.MaxBodyLen and
// MaxRecipients. They are properties of the format, not tuning knobs: raising
// one is a new MessageSigningFormatVersion.
//
// They are NOT the send path's limits. MaxBodyBytes (64 KiB) is far smaller and
// binds first on anything this client sends; these bound what the format can
// ENCODE, which is what matters when verifying a message someone else made.
const (
	// MaxSignedBodyLen is 1 MiB, matching the WAL's maximum payload.
	MaxSignedBodyLen = 1 << 20

	// MaxSignedRecipients is 4096. Without it the body is bounded and the
	// recipient list is not, so untrusted input could ask for canonical bytes of
	// unbounded size — which would defeat the point of bounding the body.
	MaxSignedRecipients = 4096
)

// Ed25519 sizes, taken from crypto/ed25519 rather than written as literals so
// the two can never disagree.
const (
	// SignatureSize is the length of a detached Ed25519 signature: 64 bytes,
	// always, for every message.
	SignatureSize = ed25519.SignatureSize

	// MessagingPublicKeySize is 32. It is exported because EVERY caller that
	// verifies must check it BEFORE calling ed25519.Verify — see
	// verifySignedMessage for why that is a remote-DoS trap and not a style
	// preference.
	MessagingPublicKeySize = ed25519.PublicKeySize

	// MessagingPrivateKeySize is 64: the RFC 8032 seed followed by the public
	// half, which is what crypto/ed25519 hands back from NewKeyFromSeed.
	MessagingPrivateKeySize = ed25519.PrivateKeySize
)

// BusIDPattern is the shape of a bus id, PINNED here to match
// internal/ids.BusIDPattern. '.' is excluded on purpose: invariant 2 qualifies
// agents as "<bus-id>.<agent-id>", so a bus id containing the separator would
// make the qualification ambiguous.
const BusIDPattern = `^[A-Za-z0-9_-]{1,64}$`

var busIDRegexp = regexp.MustCompile(BusIDPattern)

// errNotCanonical wraps every failure of canonicalize.
//
// canonicalize NEVER returns partial or best-effort bytes. Either the input is a
// well-formed message and the bytes are exact, or there are no bytes at all:
// signing a guess would produce a signature over something nobody checked, and
// VERIFYING a guess would accept a message nobody signed.
var errNotCanonical = errors.New("message cannot be canonicalized")

// signedMessage is the set of fields the signature covers — the whole set, in
// the order they are encoded. Mirrors internal/signing.Message.
//
// A field that is not here is NOT protected and may be changed by anyone on the
// path. bus_path and sent_at are the two deliberate examples: bus_path is
// rewritten by every relay by definition, and sent_at is the BUS's clock while
// TimestampUnixMilli is the SENDER's. Do not conflate the two — they are
// different facts, and only one of them is signed.
type signedMessage struct {
	// MessageID is the ORIGIN bus's message id, "<bus-id>-<seq>", minted by the
	// server (invariant 1). The sender does not choose it; it signs the
	// assignment the origin bus gave it at /v1/mint.
	MessageID string

	// Sequence is the origin bus's message sequence. It is encoded as a
	// fixed-width integer AS WELL AS appearing inside MessageID: the integer is
	// what a verifier cross-checks AGAINST THE SEQUENCE EMBEDDED IN MessageID,
	// and the redundancy is CHECKED rather than assumed.
	//
	// It is an IDENTITY, not an ordering or freshness token (corrected
	// 2026-08-14, SIGN-1-FU-REORDER-WATERMARK — this comment previously said
	// the integer is "what a verifier compares for ordering"). The sequence is
	// minted at RESERVATION time, so a lower sequence arriving after a higher
	// one is a normal, correct delivery. Do not order on it.
	Sequence uint64

	// Sender is the fully-qualified "<bus-id>.<agent-id>" (invariant 2).
	Sender string

	// Recipients are fully-qualified recipient ids. canonicalize sorts a COPY
	// bytewise ascending — the caller's slice is never modified — so two
	// independent implementations that disagree about ordering still produce
	// identical bytes. Duplicates are REJECTED rather than collapsed: collapsing
	// would change the recipient set the sender thought it signed.
	Recipients []string

	// TimestampUnixMilli is the sender's wall clock in Unix milliseconds UTC.
	//
	// Milliseconds rather than nanoseconds so the value is exactly representable
	// as a JSON number, and the wire carries THIS EXACT INTEGER rather than a
	// formatted timestamp a verifier has to re-parse. Every conversion between
	// the wire form and the signed form is a place the two sides can drift, so
	// there is none.
	//
	// It is NOT a freshness mechanism: clocks lie.
	//
	// NEITHER IS THE SEQUENCE (corrected 2026-08-14,
	// SIGN-1-FU-REORDER-WATERMARK — the previous wording here named "the
	// server-minted monotonic sequence plus the recipient's cursor", and an
	// embedder building a recipient from that sentence re-implements the very
	// message-suppression defect that task fixed, in their client rather than in
	// the bus). Replay protection is enforced SERVER-SIDE AT INGEST: the bus
	// refuses an already-accepted signed message before it is ever served
	// (invariant 10). Seq is minted when a client RESERVES, not when it sends,
	// so it is NOT monotone in delivery order — a lower Seq arriving after a
	// higher one is a normal, correct delivery, and dropping it loses a message
	// the bus has already acknowledged. Seq is an IDENTITY; the read cursor is
	// an opaque DELIVERY POSITION; they are different numbers and are not
	// comparable. Deduplicate on the message id.
	TimestampUnixMilli int64

	// Body is the payload, opaque bytes, length-prefixed and copied verbatim.
	// It is signed but NOT encrypted: bodies travel in cleartext with a detached
	// signature, and every bus on the path can read them.
	Body []byte
}

// signingMessage returns the signedMessage whose canonicalization m's signature
// covers.
//
// It is the ONE place the mapping from a received wire message to its signed
// bytes is written down, mirroring store.Message.SigningMessage on the server
// for exactly the same reason: two call sites reconstructing "the signed bytes"
// by hand is two chances for one of them to omit a field, and a verifier that
// omits a covered field accepts messages whose omitted field was tampered with.
//
// Recipients come from To. A BROADCAST therefore has an EMPTY recipient set and
// will not canonicalize — which is correct and deliberate: under signing format
// v1 a broadcast has no canonical audience (the bus stores it as a flag, not as
// a roster snapshot), so it cannot be signed and cannot be verified. SIGN-3
// settles what a broadcast's audience is; until then a broadcast that somehow
// reaches a recipient is rejected rather than trusted.
func (m Message) signingMessage() signedMessage {
	return signedMessage{
		MessageID:          m.MessageID,
		Sequence:           m.Seq,
		Sender:             m.From,
		Recipients:         m.To,
		TimestampUnixMilli: m.TimestampMS,
		Body:               m.Body,
	}
}

// canonicalize returns the exact bytes to be signed and verified for m.
//
// The layout is NORMATIVE and is specified in PROTOCOL.md §8. All integers are
// big-endian; every variable-length field is preceded by its uint32 length:
//
//	uint32 len || MessageSigningContext     ("agent-bus/msg-sig/1")
//	uint32 len || MessageID
//	uint64        Sequence
//	uint32 len || Sender
//	uint32        recipient count
//	uint32 len || recipient                 (repeated, sorted, deduplicated)
//	int64         TimestampUnixMilli        (two's complement)
//	uint32 len || Body
//
// The output goes to ed25519.Sign / ed25519.Verify UNHASHED. Do not pre-hash it:
// Ed25519 signs the message itself.
func canonicalize(m signedMessage) ([]byte, error) {
	recipients, err := validateSignedMessage(m)
	if err != nil {
		return nil, err
	}

	size := 4 + len(MessageSigningContext) +
		4 + len(m.MessageID) +
		8 +
		4 + len(m.Sender) +
		4 +
		8 +
		4 + len(m.Body)
	for _, r := range recipients {
		size += 4 + len(r)
	}

	out := make([]byte, 0, size)
	out = appendLenPrefixed(out, []byte(MessageSigningContext))
	out = appendLenPrefixed(out, []byte(m.MessageID))
	out = appendUint64(out, m.Sequence)
	out = appendLenPrefixed(out, []byte(m.Sender))
	out = appendUint32(out, uint32(len(recipients)))
	for _, r := range recipients {
		out = appendLenPrefixed(out, []byte(r))
	}
	// Encoded as a two's-complement int64 — the field is signed in the LAYOUT so
	// the format never needs a version bump to admit a pre-1970 clock.
	// validateSignedMessage nevertheless rejects a non-positive value as an unset
	// field, so this branch is a property of the format rather than a reachable
	// case.
	out = appendUint64(out, uint64(m.TimestampUnixMilli))
	out = appendLenPrefixed(out, m.Body)
	return out, nil
}

// validateSignedMessage checks every field and returns the sorted, deduplicated
// recipient list. It fails closed, exactly as internal/signing.validate does.
//
// # Where this mirror is deliberately more PERMISSIVE than the server, and why
//
// internal/signing calls ids.ParseAgentID, which enforces the full server-side
// agent-id grammar including the minted "<name>-<n>" suffix. This mirror does
// NOT re-derive that grammar, for the reason validateRecipient already states:
// invariant 1 keeps the SERVER authoritative on ids, and a client that
// re-implemented the grammar would start refusing legitimate ids the day the
// server's format grew — which is a self-inflicted outage, in a client that
// cannot be upgraded in lockstep with the bus.
//
// That asymmetry is safe in both directions, and it is worth spelling out
// because it looks like a hole:
//
//   - The BYTES are a pure function of the field VALUES. For any message both
//     sides accept, both produce identical bytes. A validator can only decide
//     whether bytes are produced at all — it can never make two accepting
//     implementations disagree about what the bytes are.
//   - Accepting something the server would refuse means we sign a message the
//     bus then rejects at ingest (SIGN-6's shape checks) — a refused send, not a
//     forged one.
//   - On the RECEIVE side it means we may canonicalize a message the server
//     considers malformed; the signature must still verify under the sender's
//     trusted key, so an attacker gains nothing but the ability to be rejected
//     slightly later.
//
// The three checks that are NOT relaxed are the ones whose absence would let a
// signature cover a mismatch: the sequence must agree with the message id's own
// sequence half, the message id must have exactly ONE spelling, and the sender
// must belong to the bus that minted the message id.
func validateSignedMessage(m signedMessage) ([]string, error) {
	originBus, seqFromID, err := parseSignedMessageID(m.MessageID)
	if err != nil {
		return nil, fmt.Errorf("%w: message id: %v", errNotCanonical, err)
	}
	if m.Sequence == 0 {
		// The server's allocator never issues 0 — the first sequence is 1 — so a
		// 0 here is an unset field rather than a real assignment.
		return nil, fmt.Errorf("%w: sequence 0 is never allocated and means \"unset\"; message sequences start at 1", errNotCanonical)
	}
	if m.Sequence != seqFromID {
		return nil, fmt.Errorf("%w: message id %q carries sequence %d but the message claims sequence %d; the two halves of one server assignment must agree", errNotCanonical, m.MessageID, seqFromID, m.Sequence)
	}

	senderBus, err := qualifyingBusID(m.Sender)
	if err != nil {
		return nil, fmt.Errorf("%w: sender: %v", errNotCanonical, err)
	}
	if senderBus != originBus {
		// The ORIGIN BINDING. Without it a peer could present its own agent as
		// the sender of a message id minted by another bus, and the signature
		// would cover the mismatch without objecting to it.
		return nil, fmt.Errorf("%w: message id %q was minted by bus %q but the sender %q belongs to bus %q; a message is signed by an agent of the bus that minted its id", errNotCanonical, m.MessageID, originBus, m.Sender, senderBus)
	}

	if len(m.Recipients) == 0 {
		return nil, fmt.Errorf("%w: a message has at least one recipient; an empty recipient set would sign an audience of nobody", errNotCanonical)
	}
	if len(m.Recipients) > MaxSignedRecipients {
		return nil, fmt.Errorf("%w: %d recipients, but the canonical format carries at most %d", errNotCanonical, len(m.Recipients), MaxSignedRecipients)
	}
	recipients := make([]string, len(m.Recipients))
	copy(recipients, m.Recipients)
	for _, r := range recipients {
		if _, rerr := qualifyingBusID(r); rerr != nil {
			return nil, fmt.Errorf("%w: recipient: %v", errNotCanonical, rerr)
		}
	}
	// Sorting here rather than requiring a sorted input is deliberate: it is the
	// difference between a format two implementations can accidentally disagree
	// about and one they cannot.
	sort.Strings(recipients)
	for i := 1; i < len(recipients); i++ {
		if recipients[i] == recipients[i-1] {
			return nil, fmt.Errorf("%w: recipient %q appears twice; duplicates are rejected rather than collapsed, because collapsing would change the recipient set the sender signed", errNotCanonical, recipients[i])
		}
	}

	if m.TimestampUnixMilli <= 0 {
		return nil, fmt.Errorf("%w: timestamp %d is not a positive Unix millisecond value; 0 means \"unset\"", errNotCanonical, m.TimestampUnixMilli)
	}

	if len(m.Body) > MaxSignedBodyLen {
		return nil, fmt.Errorf("%w: body is %d bytes, but the canonical format carries at most %d", errNotCanonical, len(m.Body), MaxSignedBodyLen)
	}
	return recipients, nil
}

// maxSignedMessageIDLen mirrors ids.MaxMessageIDLen: a 64-byte bus id, the
// separator, and the 20 decimal digits of math.MaxUint64.
const maxSignedMessageIDLen = 64 + 1 + 20

// parseSignedMessageID splits an untrusted message id into its bus id and
// sequence, mirroring ids.ParseMessageID.
//
// It splits on the LAST '-', which is exact rather than heuristic: a bus id may
// contain '-' (the minted form is literally "bus-<base32>"), but the sequence
// half is decimal digits only and can never contain one.
//
// The one-spelling rule is mirrored in full and is NOT a nicety. "bus-x-007",
// "bus-x-+7" and "bus-x-7" must not all mean sequence 7: they are three
// DIFFERENT byte sequences that would canonicalize to three different signed
// messages while a consumer keyed on the parsed pair sees one, and an attacker
// chooses which. Rejecting the alternate spellings collapses that gap.
func parseSignedMessageID(id string) (busID string, seq uint64, err error) {
	// Length first, and this error deliberately does not echo its input: %q
	// escapes a control byte to four characters, so an oversized hostile id
	// would be echoed back several times its own size through the wrapping
	// errors. Past the bound the content cannot matter — no such string is valid.
	if len(id) > maxSignedMessageIDLen {
		return "", 0, fmt.Errorf("%d bytes, but a message id is at most %d (\"<bus-id>-<seq>\"); the id is not echoed here because it is oversized", len(id), maxSignedMessageIDLen)
	}
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return "", 0, fmt.Errorf("message id %q has no %q separator; the form is \"<bus-id>-<seq>\"", id, "-")
	}
	busID, seqPart := id[:i], id[i+1:]
	if !busIDRegexp.MatchString(busID) {
		return "", 0, fmt.Errorf("message id %q: bus id %q must match %s", id, busID, BusIDPattern)
	}
	if seqPart == "" {
		return "", 0, fmt.Errorf("message id %q: the sequence after the final %q is empty", id, "-")
	}
	for j := 0; j < len(seqPart); j++ {
		if c := seqPart[j]; c < '0' || c > '9' {
			return "", 0, fmt.Errorf("message id %q: sequence %q must be decimal digits only (no sign, no whitespace, no underscores)", id, seqPart)
		}
	}
	if len(seqPart) > 1 && seqPart[0] == '0' {
		return "", 0, fmt.Errorf("message id %q: sequence %q has a leading zero; a message id has exactly one spelling", id, seqPart)
	}
	// Digits only by the loop above, so the only failure ParseUint can report is
	// overflow past 64 bits — which must fail rather than silently truncate and
	// alias a sequence the bus really did issue.
	seq, perr := strconv.ParseUint(seqPart, 10, 64)
	if perr != nil {
		return "", 0, fmt.Errorf("message id %q: sequence %q is not a 64-bit decimal number: %v", id, seqPart, perr)
	}
	if seq == 0 {
		return "", 0, fmt.Errorf("message id %q: sequence 0 is never allocated; message sequences start at 1", id)
	}
	return busID, seq, nil
}

// qualifyingBusID returns the bus half of a fully-qualified agent id, checking
// only what invariant 2 makes structural: a legal bus id, a '.' separator, and a
// non-empty agent half within the bound every server-supplied id already obeys.
//
// It is deliberately permissive about the agent half — see
// validateSignedMessage's comment on why re-deriving the server's grammar here
// would be a self-inflicted outage rather than a safety measure.
func qualifyingBusID(id string) (string, error) {
	if id == "" {
		return "", errors.New("an agent id is required")
	}
	if !serverIDPattern.MatchString(id) {
		return "", fmt.Errorf("agent id %q is not 1-256 bytes of [A-Za-z0-9._-]", safeText(id, 60))
	}
	busID, agentPart, ok := splitQualifiedID(id)
	if !ok {
		return "", fmt.Errorf("agent id %q is not fully qualified; every agent id is \"<bus-id>.<agent-id>\" (invariant 2)", safeText(id, 60))
	}
	if !busIDRegexp.MatchString(busID) {
		return "", fmt.Errorf("agent id %q: bus id %q must match %s", safeText(id, 60), busID, BusIDPattern)
	}
	if agentPart == "" {
		return "", fmt.Errorf("agent id %q has an empty agent half", safeText(id, 60))
	}
	return busID, nil
}

// signSignedMessage canonicalizes m and returns the detached Ed25519 signature
// over those exact bytes, made with this agent's MESSAGING private key.
//
// The messaging key is NOT the auth key. The auth key proves this agent to the
// BUS and the bus holds its public half; the messaging key proves this agent to
// its PEERS and is the one a compromised bus cannot forge. Two keys, two
// purposes — see store.go's Credential.MessagingKeySeed.
func signSignedMessage(priv ed25519.PrivateKey, m signedMessage) ([]byte, error) {
	// Checked BEFORE ed25519.Sign, which PANICS on a wrong-size private key
	// exactly as Verify panics on a wrong-size public key. A private key arrives
	// from a file an operator may have truncated or half-copied, so this is a
	// reachable input and not a theoretical one.
	//
	// The key's LENGTH is reported; the key is not, and must never be. A private
	// key that reaches a log line or an error string has left the machine.
	if len(priv) != MessagingPrivateKeySize {
		return nil, fmt.Errorf("messaging private key is %d bytes, want exactly %d", len(priv), MessagingPrivateKeySize)
	}
	b, err := canonicalize(m)
	if err != nil {
		// canonicalize already failed closed: there are no bytes, so there is
		// nothing to sign. Signing a best-effort serialisation would produce a
		// signature over something nobody specified.
		return nil, err
	}
	return ed25519.Sign(priv, b), nil
}

// verifySignedMessage checks sig against m under pub, FAIL-CLOSED: a "" reason
// with a nil error is the ONLY outcome on which a caller may hand the body to
// the calling agent.
//
// # WHICH KEY — the most abusable property of a detached-signature API
//
// pub is a free parameter and this function CANNOT check that the caller chose
// it correctly. The rule is the caller's to obey and it is absolute: the
// verification key MUST be resolved from the local trust store using the
// fully-qualified SENDER FIELD, m.Sender, and from NOTHING ELSE — never a key,
// key id or hint carried beside the signature, and never a key the BUS supplied.
//
// Get that wrong and verification is SELF-SIGNED and worth nothing, while every
// test still passes: an attacker who can choose the key can satisfy this
// function for any body it likes. That is exactly the silent failure invariant 9
// exists for, and it is why the canonical layout carries NO key identifier — a
// self-describing signature is one an attacker can point at a key of its
// choosing. verifyReceivedMessage below is the only caller, and it resolves the
// key from m.Sender; keep it that way.
//
// # The panic trap, which is why the key is checked first
//
// crypto/ed25519.Verify PANICS when len(publicKey) != ed25519.PublicKeySize. It
// does not return false. That asymmetry is a remote denial of service the moment
// a key reaches this path from anywhere an attacker influences — a trust-store
// file on damaged media, a key pasted by hand into `agent-busctl trust` — because a
// MALFORMED SIGNATURE is handled safely and returns false while a MALFORMED KEY
// takes the process down.
//
// So the order below is load-bearing and must not be rearranged:
//
//  1. the public key's length   (else ed25519.Verify panics)
//  2. the signature's length    (so "absent" and "mangled" stay distinct)
//  3. canonicalization of m     (a message that will not canonicalize has no
//     signable bytes, so nothing can verify)
//  4. ed25519.Verify
func verifySignedMessage(pub ed25519.PublicKey, m signedMessage, sig []byte) (RejectionReason, error) {
	if len(pub) != MessagingPublicKeySize {
		return RejectedMalformedKey, fmt.Errorf("the trusted messaging key for %s is %d bytes, want exactly %d", safeText(m.Sender, 60), len(pub), MessagingPublicKeySize)
	}
	if len(sig) == 0 {
		return RejectedNoSignature, errors.New("the message carries no signature")
	}
	if len(sig) != SignatureSize {
		return RejectedSignatureLength, fmt.Errorf("the signature is %d bytes, want exactly %d", len(sig), SignatureSize)
	}
	b, err := canonicalize(m)
	if err != nil {
		return RejectedNotCanonical, err
	}
	if !ed25519.Verify(pub, b, sig) {
		// Deliberately says nothing about why, because there is nothing to say:
		// Ed25519 verification is a single boolean, and a tampered body, a
		// re-labelled sender, a substituted key and a forged signature are
		// indistinguishable from inside. The caller logs the message id and the
		// sender; this is the verdict.
		return RejectedSignatureInvalid, errors.New("the signature does not verify against the trusted messaging key for this sender")
	}
	return "", nil
}

// appendLenPrefixed appends a uint32 big-endian length followed by b. Callers
// have already bounded every field, so a length that does not fit in a uint32
// cannot reach here; the check stays because a silently truncated length would
// produce bytes that encode a DIFFERENT message.
func appendLenPrefixed(dst, b []byte) []byte {
	if uint64(len(b)) > math.MaxUint32 {
		panic("client: field longer than uint32 reached the canonical encoder; validateSignedMessage must reject it first")
	}
	dst = appendUint32(dst, uint32(len(b)))
	return append(dst, b...)
}

func appendUint32(dst []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(dst, buf[:]...)
}

func appendUint64(dst []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(dst, buf[:]...)
}
