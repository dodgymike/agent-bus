// Package attest mints and verifies BUS-SIGNED AGENT-KEY ATTESTATIONS: an
// origin bus's signed statement that a fully-qualified agent id belongs with a
// particular Ed25519 MESSAGING public key.
//
// It is the artefact the cross-bus trust model of DECISIONS.md (2026-08-07,
// "Cross-bus key trust: pin the origin bus key at peering, no TOFU") rests on,
// and it is specified in FEDERATION_TRUST_DEEPDIVE.md §4.2–§4.6, which is this
// package's normative reference. RELAY-14.
//
// # The topology this exists for, and the property it buys
//
// A laptop bus (A) relays through an internet-facing bus (B) to a third bus (C).
// C never peers with A, never dials A, and holds no roster of A's agents:
//
//	A ---> B ---> C
//
// A signs the binding "A.writer-7 -> <32-byte messaging key>" with its BUS
// SIGNING KEY (internal/buscert.Material.SigningPrivateKey). B forwards that
// attestation VERBATIM and MAY NEVER RE-ATTEST IT. C verifies it against its
// operator-pinned copy of A's bus signing key.
//
// B is therefore a COURIER, NOT A VOUCHER — which is the whole point, because B
// is the machine exposed to the internet. B can drop a message, delay it, or
// replace it with one forged from its OWN agents (which C attributes to B,
// correctly). What B cannot do is produce a message C attributes to A.writer-7,
// because that needs A's bus signing key.
//
// "Courier, not voucher" is scoped to CONTENT and ATTRIBUTION and NOT to
// provenance: bus_path stays unsigned and this package does not change that
// (internal/relay/doc.go — loop prevention is availability, never security).
//
// # What this package is NOT
//
// It is NOT the recipient's authenticity guarantee. A recipient agent verifies
// its correspondent end-to-end against its own client/keyring.DirKeyRing, which
// no bus can influence. Attestations are BUS-TO-BUS INGEST ADMISSION CONTROL:
// they stop a peer injecting forged traffic attributed to another bus's agents
// into our durable store, our audit trail, our idempotency table and our agents'
// cursors. The two layers are independent and both fail closed. See
// FEDERATION_TRUST_DEEPDIVE.md §2.3.
//
// # Invariant 9 — which side of the line this is on, and why
//
// PERMITTED SIDE. Not escalating. The accounting, so a reviewer can check the
// claim rather than take it:
//
//   - Signing is one call to crypto/ed25519.Sign over a byte string, UNHASHED.
//   - Verification is one call to crypto/ed25519.Verify over the same byte
//     string, re-derived at the verifier.
//   - Key generation is not ours: internal/buscert already generates, fsyncs
//     and permissions the bus signing key.
//
// Nothing here adds a cipher, hash, MAC, KDF, key exchange, ratchet, signature
// scheme, padding scheme, nonce scheme or IV scheme, and — the case invariant 9
// enumerates precisely because it does not FEEL like writing your own crypto —
// no bespoke construction assembled out of otherwise-good primitives. There is
// no composition of two primitives whose interaction anyone must reason about:
// it is one Sign, one Verify, over a byte string.
//
// Defining a canonical byte encoding is FRAMING, not a primitive. The security
// of Ed25519 does not depend on the structure of what it signs; what the
// encoding must supply is UNAMBIGUITY (two distinct field tuples must never
// encode to the same bytes) and DOMAIN SEPARATION (an artefact of one type must
// never be a valid encoding of another). Both are obtained by following
// internal/signing's existing, litigated rules rather than by inventing
// anything — see Canonicalize for the proof of each.
//
// THE THINGS THAT WOULD REQUIRE ESCALATION, named so they are recognisable:
// deriving the attestation key from the TLS key (a KDF); MAC-ing an attestation
// instead of signing it; composing the message signature and the attestation
// signature into one aggregate value. Each is a construction, not framing. None
// is needed and none is here.
//
// # Where the encoder lives, and the drift risk that creates
//
// FEDERATION_TRUST_DEEPDIVE.md §4.5 recommends the canonical ENCODER live in
// internal/signing (as signing.CanonicalizeAttestation, its task T4) with only
// the POLICY here. RELAY-14's file-ownership boundary is internal/attest only,
// so the encoder is here, written to internal/signing's normative rules
// (canonical.go:146-162) rather than to a parallel invention of them.
//
// THE CONSEQUENCE MUST NOT BE LOST: if T4 later lands
// signing.CanonicalizeAttestation, this package's Canonicalize MUST become a
// delegation to it, pinned by a byte-equality test. TWO independent encoders for
// one wire format is precisely the drift the split was meant to avoid, and the
// day they disagree, every attestation minted by one bus is refused by the
// other with an error that says "forgery".
//
// # The format version is RESERVED, never chosen
//
// AttestationFormatVersion is 2, allocated from the Spec Server
// `signing-format-version` namespace on 2026-08-08 for RELAY-14. Nobody picks
// one by looking at the constant. It is DELIBERATELY NOT signing.FormatVersion:
// two layouts behind one constant is the "two meanings on one key" defect, and
// signing.FormatVersion is already consumed as THE format version in a
// peer-facing error string.
package attest
