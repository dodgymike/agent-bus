// Package signing defines the ONE canonical byte sequence that agent-bus signs,
// verifies and hashes for a message (SIGN-1).
//
// It contains no cryptography. It produces bytes; `crypto/ed25519` signs them
// and `crypto/sha256` hashes them. That separation is deliberate and is what
// invariant 9 requires: this package must never grow a construction of its own.
//
// # Why a canonical format exists at all
//
// A detached signature is a claim about a byte string. If the sender and the
// verifier build that byte string even slightly differently — a different field
// order, a re-encoded JSON number, a trimmed space, a field one side includes
// and the other does not — then verification either fails at random or, far
// worse, succeeds over bytes that omit a field an attacker is free to change.
// The second failure is silent: signatures still verify, they simply stop
// protecting the omitted field. So the format is pinned here, in one place,
// with byte-exact test vectors (testdata/canonical_vectors.json) that SIGN-2,
// SIGN-5 and CRYPTO-10 check themselves against.
//
// # Encoding: length-prefixed binary, NOT canonical JSON
//
// Fixed field order, big-endian integers, every variable-length field preceded
// by its uint32 length. Canonical JSON was considered and rejected:
//
//   - JSON is re-encodable, and things on the path re-encode it. SIGN-7 relays
//     a signed message through an intermediate bus; any hop that unmarshals and
//     remarshals — reordering keys, changing number formatting, normalising
//     escapes or whitespace — breaks verification at the far end without
//     anybody having done anything wrong. Length-prefixed binary either arrives
//     byte-identical or does not arrive.
//   - "Canonical JSON" is a specification we would have to implement (key
//     ordering, string escaping, number formatting, Unicode normalisation), and
//     a subtly wrong implementation fails in exactly the silent way invariant 9
//     warns about.
//   - Bodies are opaque bytes. JSON cannot hold arbitrary bytes without a
//     second encoding layer (base64), which is one more place two sides can
//     disagree.
//
// Length prefixes are framing, not a construction. They exist so the encoding
// is INJECTIVE: distinct field tuples always produce distinct byte strings, so
// no attacker can shift bytes across a field boundary — move the tail of the
// sender id into the head of a recipient id, say — and present a different
// logical message under a signature that still verifies.
//
// # The design call: server-minted fields are INSIDE the signed bytes
//
// The message id and the sequence are minted by the server (invariant 1), yet
// they must be covered by the sender's signature, or an intermediate bus can
// reorder or misattribute messages undetectably. That is a real tension and it
// is resolved here as OPTION (a): the sender signs the ORIGIN server's
// assignment, obtained before signing.
//
//  1. the client asks the origin bus to mint a message id and sequence,
//     quoting its idempotency key (invariant 10, so a retry gets the SAME
//     assignment back rather than burning a second one);
//  2. the client canonicalizes with those values and signs (SIGN-2);
//  3. the client submits the signed envelope; the bus rejects it unless the
//     id and sequence are the ones it minted for that key, then runs the
//     ordinary durable two-phase write and only then acknowledges (invariant 4).
//
// The rejected alternative, OPTION (b) — sign only sender-known fields and bind
// the id/sequence separately in the durable record — costs no round trip, but
// leaves the ordering claim unauthenticated end to end: the id/sequence a
// recipient sees would be asserted by whichever bus handed the message over,
// and a malicious or compromised intermediate could renumber messages, replay
// one under a fresh sequence, or present them out of order, with every
// signature still verifying. The recipient could not tell. A one-round-trip
// cost is worth an authenticated ordering claim.
//
// What option (a) does NOT buy, stated so nobody over-reads it: it does not
// stop an intermediate DROPPING a message (a gap is visible; a truncation at
// the tail is not, until the next message arrives), it does not stop delay, and
// it does not authenticate the traversed bus path — the path grows on every hop
// and therefore cannot be inside the signed bytes at all (SIGN-7, IDEM-15), so
// loop prevention is an availability mechanism, never a security one.
//
// # Relay: the origin's numbers are signed, the receiving bus's are not
//
// The signed bytes carry the ORIGIN bus's message id and sequence. Both are
// already bus-namespaced ("<bus-id>-<seq>", invariants 1 and 2), so they are
// globally unambiguous and are not a peer's to mint. A receiving bus mints its
// own LOCAL delivery sequence for its own recipients' cursors (SIGN-4) OUTSIDE
// the signed bytes and binds it in its durable record. Neither bus cedes id
// authority, and no relayed signature breaks.
//
// Canonicalize enforces the origin binding: the bus id embedded in the message
// id must equal the bus id qualifying the sender. A message claiming id
// "bus-a-7" from "bus-b.eve-1" is refused rather than signed.
//
// # One byte sequence, two consumers — DUR-5 must not fork it
//
// The canonical bytes are the single shared input to BOTH:
//
//   - SIGN-2, which passes them to ed25519.Sign / ed25519.Verify UNHASHED.
//     Ed25519 signs the message, never a digest; crypto/ed25519 exposes no
//     mode that takes a pre-hash, and feeding it one is an API misuse rather
//     than a shortcut (RATCHET-7).
//   - DUR-5 / CRYPTO-11, whose audit-log content hash is SHA-256 over these
//     same bytes — see CanonicalDigest. Hashing a differently-serialised view
//     of "the same" message would silently decouple the audit trail from the
//     signature, and the pairing of the two is the whole non-repudiation claim:
//     the log proves WHICH sender produced WHICH content at WHICH sequence
//     without ever holding the content.
//
// If DUR-5's record needs fields this format does not cover — the traversed bus
// path, the local delivery sequence, the byte size — they are additional,
// clearly out-of-band columns of the audit record. They are never folded into
// the canonical bytes and never substituted for them.
//
// See PROTOCOL.md §8 for the normative byte-layout table.
package signing
