package signing

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// FormatVersion is the version of the canonical signing format implemented
// here. It is a RESERVED number, allocated through the Spec Server
// `signing-format-version` namespace (value 1, reserved 2026-08-02 for SIGN-1).
// Nobody picks one by looking at this constant.
//
// The version is not a field of its own: it is spelled out inside Context, so
// there is exactly ONE version indicator in the signed bytes and no way for two
// of them to disagree. Changing ANY of the layout below — adding a field,
// removing one, reordering, changing a width — is a new version with a new
// Context string, never an in-place edit of this one.
const FormatVersion = 1

// Context is the domain-separation prefix: the first field of every canonical
// byte sequence. It exists so that a signature over a message can never be
// mistaken for, or replayed as, a signature over some other agent-bus artefact
// (a session challenge, an invite, a peering record) that happens to serialise
// to the same bytes. A fixed, documented, length-prefixed ASCII prefix is
// framing — it is not a cryptographic construction, and nothing about the
// signature scheme depends on its content.
//
// Version 1 is bound EXCLUSIVELY to Ed25519 (RATCHET-7). The algorithm is not a
// negotiable field of the format: changing it is a new Context string, so no
// verifier can be talked into a weaker algorithm by anything on the wire.
//
// It is also disjoint from every other signing input in this codebase, which is
// what stops one signature being replayed as another. The only other input the
// same key signs today is auth.SessionSigningContext
// ("agent-bus:session-token:v1:" + token), and the two languages differ in
// their FIRST byte: a canonical message always begins with the 0x00 of a uint32
// length, a session challenge always begins with 'a' (0x61). Any future
// artefact signed with an agent's key must preserve that disjointness.
const Context = "agent-bus/msg-sig/1"

// MaxBodyLen bounds the message body the format will encode: 1 MiB, matching
// the WAL's MaxPayloadSize. It is a property OF THE FORMAT, so raising it is a
// format change (a new FormatVersion), not a tuning knob. The send path may of
// course impose a smaller limit of its own.
const MaxBodyLen = 1 << 20

// MaxRecipients bounds the recipient set at 4096.
//
// Without it the body is bounded and the recipient list is not, so untrusted
// input could ask for canonical bytes of unbounded size — the whole point of
// bounding the body. 4096 is derived rather than picked: a fully-qualified
// agent id is at most ids.MaxAgentIDLen (150) bytes and costs four more for its
// length prefix, so 4096 recipients cap the recipient block at about 616 KiB —
// the same order as MaxBodyLen, and comfortably inside the WAL's 1 MiB record.
//
// Like MaxBodyLen this is a property OF THE FORMAT: raising it is a new
// FormatVersion. A bus that outgrows it should fan a broadcast out over several
// messages rather than quietly widen the format.
const MaxRecipients = 4096

// ErrInvalid wraps every failure of Canonicalize. Callers that must fail closed
// on a malformed message — SIGN-6's ingest policy in particular — can match it
// with errors.Is and treat it as "this is not a canonicalizable message",
// without parsing error strings.
//
// Canonicalize NEVER returns partial or best-effort bytes. Either the input is
// a well-formed message and the bytes are exact, or there are no bytes at all:
// signing or hashing a guess would produce a signature over something nobody
// checked.
var ErrInvalid = errors.New("signing: message cannot be canonicalized")

// Message is the set of fields covered by the signature — the whole set, in the
// order they are encoded. A field that is not here is NOT protected by the
// signature and may be changed by anyone on the path (the traversed bus path
// and a relaying bus's local delivery sequence are the two deliberate examples;
// see the package doc).
//
// Provenance of each field, which is the point of the design (invariant 1):
//
//	MessageID           SERVER-minted by the ORIGIN bus ("<bus-id>-<seq>")
//	Sequence            SERVER-minted by the ORIGIN bus, strictly monotonic
//	Sender              SERVER-minted at enrolment ("<bus-id>.<agent-id>")
//	Recipients          SENDER-chosen, but every id is server-minted
//	TimestampUnixMilli  SENDER-supplied
//	Body                SENDER-supplied, opaque bytes
//
// The sender does not choose the server-minted fields; it signs the assignment
// the origin bus gave it (option (a), package doc). A bus that receives a
// signed message therefore checks the assignment against what it minted before
// accepting — the signature proves the sender agreed to those numbers, not that
// the numbers are legitimate.
type Message struct {
	// MessageID is the origin bus's message id, "<bus-id>-<seq>". The bus half
	// must be the same bus that qualifies Sender, and the sequence half must
	// equal Sequence; Canonicalize enforces both.
	MessageID string

	// Sequence is the origin bus's message sequence. It is encoded as a
	// fixed-width integer as well as appearing inside MessageID: the integer is
	// what a verifier cross-checks against the sequence embedded in MessageID,
	// and the redundancy is checked rather than assumed.
	//
	// It is an IDENTITY, not an ordering or freshness token (corrected
	// 2026-08-14, SIGN-1-FU-REORDER-WATERMARK). The sequence is minted at
	// RESERVATION time, so it is not monotone in delivery order; a verifier
	// must not order on it.
	Sequence uint64

	// Sender is the fully-qualified sender id, "<bus-id>.<agent-id>"
	// (invariant 2).
	Sender string

	// Recipients are fully-qualified recipient ids. Canonicalize SORTS a copy
	// bytewise ascending — the caller's slice is never modified — so two
	// independent implementations that disagree about ordering still produce
	// identical bytes. Duplicates are rejected rather than silently collapsed:
	// collapsing would change the recipient set the sender thought it signed.
	//
	// The set is covered so a broadcast cannot be re-pointed at a different
	// audience, which is the foundation SIGN-3 builds its split-content check
	// on. Recipients may live on other buses; that is the relay case.
	Recipients []string

	// TimestampUnixMilli is the sender's wall clock: Unix milliseconds UTC, as
	// a signed 64-bit integer, encoded fixed-width.
	//
	// Milliseconds rather than nanoseconds so the value is exactly
	// representable as a JSON number (it stays well below 2^53 until the year
	// 287396) — the wire envelope SIGN-2 defines must carry THIS EXACT INTEGER,
	// not a formatted timestamp that a verifier has to re-parse. Every
	// conversion between the wire form and the signed form is a place the two
	// sides can drift, so there is none.
	//
	// It is not a freshness mechanism: clocks lie. Neither is the sequence
	// (corrected 2026-08-14, SIGN-1-FU-REORDER-WATERMARK): it is minted at
	// RESERVATION time, so it is not monotone in delivery order and is an
	// IDENTITY rather than a freshness token, and the recipient's cursor is a
	// delivery POSITION, a different number that is not comparable to it.
	// Replay protection is enforced SERVER-SIDE AT INGEST, by refusing an
	// already-accepted signed message (invariant 10). NOTE this supersedes
	// SIGN-4's original wording, which is being amended on the Spec Server.
	TimestampUnixMilli int64

	// Body is the message payload, opaque bytes, length-prefixed and copied
	// verbatim. It is signed but not encrypted: bodies travel in cleartext with
	// a detached signature, and every bus on the path can read them.
	Body []byte
}

// Canonicalize returns the exact bytes to be signed, verified and hashed for m.
//
// The layout is normative and specified in PROTOCOL.md §8. All integers are
// big-endian; every variable-length field is preceded by its uint32 length:
//
//	uint32 len || Context                    ("agent-bus/msg-sig/1")
//	uint32 len || MessageID
//	uint64        Sequence
//	uint32 len || Sender
//	uint32        recipient count
//	uint32 len || recipient                  (repeated, sorted, deduplicated)
//	int64         TimestampUnixMilli         (two's complement)
//	uint32 len || Body
//
// The output is handed to ed25519.Sign / ed25519.Verify UNHASHED (SIGN-2) and
// to sha256 for the audit-log content hash (DUR-5, via CanonicalDigest). Do not
// pre-hash it for signing: Ed25519 signs the message itself.
func Canonicalize(m Message) ([]byte, error) {
	recipients, err := validate(m)
	if err != nil {
		return nil, err
	}

	size := 4 + len(Context) +
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
	out = appendLenPrefixed(out, []byte(Context))
	out = appendLenPrefixed(out, []byte(m.MessageID))
	out = appendUint64(out, m.Sequence)
	out = appendLenPrefixed(out, []byte(m.Sender))
	out = appendUint32(out, uint32(len(recipients)))
	for _, r := range recipients {
		out = appendLenPrefixed(out, []byte(r))
	}
	// The timestamp is encoded as a two's-complement int64 — the field is signed
	// in the LAYOUT so the format never needs a version bump to admit a
	// pre-1970 clock. validate() nevertheless rejects a non-positive value as an
	// unset field, so this branch of the encoding is a property of the format
	// rather than a reachable case.
	out = appendUint64(out, uint64(m.TimestampUnixMilli))
	out = appendLenPrefixed(out, m.Body)
	return out, nil
}

// CanonicalDigest returns SHA-256 over Canonicalize(m) — the audit-log content
// hash of DUR-5 and CRYPTO-11, and NOTHING ELSE.
//
// # This value is NEVER signed
//
// It must not be passed to ed25519.Sign or ed25519.Verify. Ed25519 signs the
// message, not a digest of it (RATCHET-7); crypto/ed25519 exposes no pre-hash
// mode, and handing it a digest produces a signature over 32 bytes that no
// conforming verifier will ever reproduce. Sign Canonicalize(m).
//
// # Why it hashes the canonical bytes and not the bare body
//
// DUR-5 calls this a "content hash of the body", and hashing the body alone
// would be the obvious reading — but it would fingerprint content while
// proving nothing about who sent it, to whom, or in what order. Hashing the
// canonical bytes binds the hash and the signature to the SAME serialisation:
// the audit record and the signature are then provably statements about one
// message, which is what makes the pair non-repudiable. A hash over a
// different serialisation than the signature covers is a silent correctness
// hole (CRYPTO-11 says so in as many words).
//
// The body is still recoverable-in-principle by anyone who has both the body
// and this hash, exactly as before: the extra covered fields are all published
// metadata the audit record already carries.
func CanonicalDigest(m Message) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	b, err := Canonicalize(m)
	if err != nil {
		return sum, err
	}
	return sha256.Sum256(b), nil
}

// validate checks every field and returns the sorted, deduplicated recipient
// list. It fails closed: an unset server-minted field, an id that is not
// well-formed, or a sender that does not belong to the bus that minted the
// message id all stop the encoding rather than producing bytes.
func validate(m Message) ([]string, error) {
	originBus, seqFromID, err := ids.ParseMessageID(m.MessageID)
	if err != nil {
		return nil, fmt.Errorf("%w: message id: %v", ErrInvalid, err)
	}
	if m.Sequence == 0 {
		// ids.Sequence never issues 0, so a 0 here is an unset field rather
		// than a real assignment — the same posture ids.MessageID takes.
		return nil, fmt.Errorf("%w: sequence 0 is never allocated and means \"unset\"; message sequences start at 1", ErrInvalid)
	}
	if m.Sequence != seqFromID {
		return nil, fmt.Errorf("%w: message id %q carries sequence %d but the message claims sequence %d; the two halves of one server assignment must agree", ErrInvalid, m.MessageID, seqFromID, m.Sequence)
	}

	senderBus, _, _, err := ids.ParseAgentID(m.Sender)
	if err != nil {
		return nil, fmt.Errorf("%w: sender: %v", ErrInvalid, err)
	}
	if senderBus != originBus {
		// The origin binding. Without it a peer could present its own agent as
		// the sender of a message id minted by another bus, and the signature
		// would cover the mismatch without objecting to it.
		return nil, fmt.Errorf("%w: message id %q was minted by bus %q but the sender %q belongs to bus %q; a message is signed by an agent of the bus that minted its id", ErrInvalid, m.MessageID, originBus, m.Sender, senderBus)
	}

	if len(m.Recipients) == 0 {
		return nil, fmt.Errorf("%w: a message has at least one recipient; an empty recipient set would sign an audience of nobody", ErrInvalid)
	}
	if len(m.Recipients) > MaxRecipients {
		return nil, fmt.Errorf("%w: %d recipients, but the canonical format carries at most %d", ErrInvalid, len(m.Recipients), MaxRecipients)
	}
	recipients := make([]string, len(m.Recipients))
	copy(recipients, m.Recipients)
	for _, r := range recipients {
		if _, _, _, err := ids.ParseAgentID(r); err != nil {
			return nil, fmt.Errorf("%w: recipient: %v", ErrInvalid, err)
		}
	}
	// Sorting here rather than requiring a sorted input is deliberate: it is
	// the difference between a format two implementations can accidentally
	// disagree about and one they cannot.
	sort.Strings(recipients)
	for i := 1; i < len(recipients); i++ {
		if recipients[i] == recipients[i-1] {
			return nil, fmt.Errorf("%w: recipient %q appears twice; duplicates are rejected rather than collapsed, because collapsing would change the recipient set the sender signed", ErrInvalid, recipients[i])
		}
	}

	if m.TimestampUnixMilli <= 0 {
		return nil, fmt.Errorf("%w: timestamp %d is not a positive Unix millisecond value; 0 means \"unset\"", ErrInvalid, m.TimestampUnixMilli)
	}

	if len(m.Body) > MaxBodyLen {
		return nil, fmt.Errorf("%w: body is %d bytes, but the canonical format carries at most %d", ErrInvalid, len(m.Body), MaxBodyLen)
	}
	return recipients, nil
}

// appendLenPrefixed appends a uint32 big-endian length followed by b. Callers
// have already bounded every field, so a length that does not fit in a uint32
// cannot reach here; the check stays because a silently truncated length would
// produce bytes that encode a DIFFERENT message.
func appendLenPrefixed(dst, b []byte) []byte {
	if uint64(len(b)) > math.MaxUint32 {
		panic("signing: field longer than uint32 reached the encoder; validate() must reject it first")
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
