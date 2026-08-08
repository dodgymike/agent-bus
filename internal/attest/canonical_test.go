package attest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ---------------------------------------------------------------------------
// The known-answer test.
// ---------------------------------------------------------------------------

// katAgentID / katKeyHex / kat* are FEDERATION_TRUST_DEEPDIVE.md §4.3's worked
// example, transcribed from the document. The key is a WORKED-EXAMPLE VALUE
// (0x01..0x20), not a real key, and no real key may ever be pasted here.
const (
	katAgentID  = "laptop.writer-7"
	katKeyHex   = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	katEpoch    = uint64(1)
	katIssuedAt = int64(1754650000000)
	katNotAfter = int64(1754653600000)
)

// katCanonicalHex is the expected canonical encoding, field by field, exactly as
// FEDERATION_TRUST_DEEPDIVE.md §4.3 tabulates it:
//
//	00000016                            uint32 len(Context) = 22
//	6167...2f32                         "agent-bus/bus-attest/2"
//	0000000f                            uint32 len(AgentID) = 15
//	6c61...2d37                         "laptop.writer-7"
//	00000020                            uint32 len(MessagingPublicKey) = 32
//	0102...1f20                         the worked-example key
//	0000000000000001                    uint64 KeyEpoch
//	00000198894a3a80                    int64  IssuedAtUnixMilli
//	0000019889812900                    int64  NotAfterUnixMilli
//
// 105 bytes total: 4+22 + 4+15 + 4+32 + 8 + 8 + 8. The encoder consumes exactly
// 105, so the layout has no slack and no padding.
const katCanonicalHex = "" +
	"000000166167656e742d6275732f6275732d6174746573742f32" +
	"0000000f6c6170746f702e7772697465722d37" +
	"000000200102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" +
	"0000000000000001" +
	"00000198894a3a80" +
	"0000019889812900"

func katAttestation(t *testing.T) Attestation {
	t.Helper()
	pub, err := hex.DecodeString(katKeyHex)
	if err != nil {
		t.Fatalf("decoding the worked-example key: %v", err)
	}
	return Attestation{
		AgentID:            katAgentID,
		MessagingPublicKey: ed25519.PublicKey(pub),
		KeyEpoch:           katEpoch,
		IssuedAtUnixMilli:  katIssuedAt,
		NotAfterUnixMilli:  katNotAfter,
	}
}

// TestAttestationKnownAnswer is the FIXED-INPUT, FIXED-EXPECTED-BYTES test.
//
// It is the one test that catches an encoding change which still round-trips.
// Every other test here signs and verifies with the same code, so a field
// reordering, a width change, a dropped length prefix or an altered Context
// would pass all of them: the encoder and the decoder would simply agree with
// each other on a format no other bus in the federation implements. The bytes
// below come from the design document, not from this implementation.
//
// If this test goes red, the correct response is almost never to update the
// constant. A change to this layout is a NEW Context string and a NEW reserved
// format version — a federation-wide flag day, not an in-place edit.
func TestAttestationKnownAnswer(t *testing.T) {
	want, err := hex.DecodeString(katCanonicalHex)
	if err != nil {
		t.Fatalf("test setup: decoding the expected bytes: %v", err)
	}
	if len(want) != 105 {
		t.Fatalf("test setup: the worked example is %d bytes, and the document says 105", len(want))
	}

	got, err := Canonicalize(katAttestation(t))
	if err != nil {
		t.Fatalf("Canonicalize(worked example) = error %v, want the document's bytes", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes do not match the worked example\n got %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}

	// The version number appears in exactly one place, inside Context, so there
	// is no way for two version indicators to disagree.
	if !strings.HasSuffix(Context, "/2") || AttestationFormatVersion != 2 {
		t.Fatalf("Context %q and AttestationFormatVersion %d disagree about the reserved version (2, reserved 2026-08-08)", Context, AttestationFormatVersion)
	}
}

// TestSignSignsExactlyTheCanonicalBytesUnhashed pins the OTHER half of the
// known answer: that Sign hands Canonicalize's output to ed25519.Sign verbatim
// and UNHASHED.
//
// The expected signature is derived from the DOCUMENT's bytes rather than from
// this package's encoder, so the two cannot drift together. The pre-hash case is
// asserted negatively because it is the exact silent failure invariant 9 warns
// about: signing a digest still produces a signature, it still verifies against
// itself, and no conforming verifier anywhere else will ever reproduce it.
func TestSignSignsExactlyTheCanonicalBytesUnhashed(t *testing.T) {
	// A FIXED, TEST-ONLY seed. Ed25519 signing is deterministic, so a fixed
	// seed yields a fixed signature. This is not, and must never be, a key
	// with any other use.
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)

	docBytes, err := hex.DecodeString(katCanonicalHex)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	wantSig := ed25519.Sign(priv, docBytes)

	a := katAttestation(t)
	got, err := Sign(priv, "laptop", a.AgentID, a.MessagingPublicKey, a.KeyEpoch,
		millis(a.IssuedAtUnixMilli), millis(a.NotAfterUnixMilli))
	if err != nil {
		t.Fatalf("Sign(worked example) = error %v", err)
	}
	if !bytes.Equal(got.Signature, wantSig) {
		t.Fatalf("Sign did not sign the document's canonical bytes\n got %s\nwant %s", hex.EncodeToString(got.Signature), hex.EncodeToString(wantSig))
	}

	// The pre-hash trap, asserted negatively.
	digest := sha256.Sum256(docBytes)
	if bytes.Equal(got.Signature, ed25519.Sign(priv, digest[:])) {
		t.Fatal("Sign produced a signature over SHA-256 of the canonical bytes; Ed25519 signs the message, not a digest of it (invariant 9)")
	}
}

// ---------------------------------------------------------------------------
// Unambiguity and domain separation.
// ---------------------------------------------------------------------------

// TestCanonicalEncodingIsUnambiguous is the property the security gate asks of
// any hand-rolled length-prefixed layout: can two DISTINCT inputs produce
// IDENTICAL signed bytes?
//
// It perturbs one covered field at a time from a fixed base and asserts every
// result is distinct from the base AND from every other perturbation. The
// AgentID cases are the interesting ones — with a separator-based encoding
// rather than a length-prefixed one, moving a byte across the boundary between
// two adjacent variable-length fields is exactly how a collision is built.
func TestCanonicalEncodingIsUnambiguous(t *testing.T) {
	base := katAttestation(t)

	altKey := bytes.Repeat([]byte{0x7f}, ed25519.PublicKeySize)

	cases := []struct {
		name   string
		mutate func(a Attestation) Attestation
	}{
		{"base", func(a Attestation) Attestation { return a }},
		{"different agent name", func(a Attestation) Attestation { a.AgentID = "laptop.writer-8"; return a }},
		{"different bus half", func(a Attestation) Attestation { a.AgentID = "laptops.writer-7"; return a }},
		// The boundary case: the same 15 characters split differently between
		// the bus half and the name half. A separator-based encoding that
		// merely concatenated the fields could not tell these apart.
		{"same bytes, different split", func(a Attestation) Attestation { a.AgentID = "lapto.pwriter-7"; return a }},
		{"different key", func(a Attestation) Attestation { a.MessagingPublicKey = altKey; return a }},
		{"different epoch", func(a Attestation) Attestation { a.KeyEpoch = 2; return a }},
		{"different issued-at", func(a Attestation) Attestation { a.IssuedAtUnixMilli = katIssuedAt + 1; return a }},
		{"different not-after", func(a Attestation) Attestation { a.NotAfterUnixMilli = katNotAfter + 1; return a }},
		// Signature is NOT covered — a signature cannot cover itself — so this
		// one MUST collide with the base. Asserted explicitly below rather than
		// left as an accident.
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			b, err := Canonicalize(tc.mutate(base))
			if err != nil {
				t.Fatalf("Canonicalize(%s) = error %v", tc.name, err)
			}
			h := hex.EncodeToString(b)
			if prev, dup := seen[h]; dup {
				t.Fatalf("%q and %q are DISTINCT attestations that encode to IDENTICAL bytes: %s", tc.name, prev, h)
			}
			seen[h] = tc.name
		})
	}
	if len(seen) != len(cases) {
		t.Fatalf("expected %d distinct encodings, got %d", len(cases), len(seen))
	}

	withSig := base
	withSig.Signature = bytes.Repeat([]byte{0xaa}, ed25519.SignatureSize)
	a, err := Canonicalize(base)
	if err != nil {
		t.Fatalf("Canonicalize(base) = error %v", err)
	}
	b, err := Canonicalize(withSig)
	if err != nil {
		t.Fatalf("Canonicalize(base+signature) = error %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Signature changed the canonical bytes; a signature cannot cover itself")
	}
}

// TestCanonicalIsDisjointFromMessageSigning checks — rather than asserts in
// prose — that no byte string is a valid encoding of both an agent-signed
// message and a bus-signed attestation.
//
// The two first fields differ in the LENGTH WORD ITSELF, so neither can be a
// prefix of the other. That matters because the two artefacts would otherwise
// have to be told apart by something outside the signed bytes.
func TestCanonicalIsDisjointFromMessageSigning(t *testing.T) {
	msgFirst := append(append([]byte{}, 0x00, 0x00, 0x00, byte(len(signing.Context))), []byte(signing.Context)...)
	attFirst := append(append([]byte{}, 0x00, 0x00, 0x00, byte(len(Context))), []byte(Context)...)

	if bytes.HasPrefix(msgFirst, attFirst) || bytes.HasPrefix(attFirst, msgFirst) {
		t.Fatalf("one context framing is a prefix of the other:\n msg %s\n att %s", hex.EncodeToString(msgFirst), hex.EncodeToString(attFirst))
	}
	if len(signing.Context) == len(Context) {
		t.Fatalf("the two contexts are the same length (%d), so the length word no longer separates them; check the byte content instead before relaxing this", len(Context))
	}

	// A real canonical attestation must not begin with the message framing.
	b, err := Canonicalize(katAttestation(t))
	if err != nil {
		t.Fatalf("Canonicalize = error %v", err)
	}
	if bytes.HasPrefix(b, msgFirst) {
		t.Fatal("a canonical attestation begins with the canonical MESSAGE framing; the two languages are no longer disjoint")
	}
	if !bytes.HasPrefix(b, attFirst) {
		t.Fatal("a canonical attestation does not begin with its own context framing")
	}
}

// ---------------------------------------------------------------------------
// validate(): every rejection has its own case.
// ---------------------------------------------------------------------------

func TestCanonicalizeRejectsMalformedAttestations(t *testing.T) {
	good := katAttestation(t)

	cases := []struct {
		name string
		a    Attestation
		want string
	}{
		{"zero value", Attestation{}, "agent id"},
		{"unqualified agent id", mutate(good, func(a *Attestation) { a.AgentID = "writer-7" }), "agent id"},
		{"agent id with no minted suffix", mutate(good, func(a *Attestation) { a.AgentID = "laptop.writer" }), "agent id"},
		{"empty agent id", mutate(good, func(a *Attestation) { a.AgentID = "" }), "agent id"},
		{"oversized agent id", mutate(good, func(a *Attestation) { a.AgentID = strings.Repeat("a", ids.MaxAgentIDLen+1) }), "agent id"},
		{"nil messaging key", mutate(good, func(a *Attestation) { a.MessagingPublicKey = nil }), "messaging public key"},
		{"short messaging key", mutate(good, func(a *Attestation) { a.MessagingPublicKey = make([]byte, 31) }), "messaging public key"},
		{"long messaging key", mutate(good, func(a *Attestation) { a.MessagingPublicKey = make([]byte, 33) }), "messaging public key"},
		{"unset issued-at", mutate(good, func(a *Attestation) { a.IssuedAtUnixMilli = 0 }), "issued-at"},
		{"negative issued-at", mutate(good, func(a *Attestation) { a.IssuedAtUnixMilli = -1 }), "issued-at"},
		{"unset not-after", mutate(good, func(a *Attestation) { a.NotAfterUnixMilli = 0 }), "not-after"},
		{"expiry before issue", mutate(good, func(a *Attestation) { a.NotAfterUnixMilli = a.IssuedAtUnixMilli - 1 }), "not after"},
		{"expiry equal to issue", mutate(good, func(a *Attestation) { a.NotAfterUnixMilli = a.IssuedAtUnixMilli }), "not after"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			b, err := Canonicalize(tc.a)
			if err == nil {
				t.Fatalf("Canonicalize(%s) = %d bytes, nil error; want a refusal", tc.name, len(b))
			}
			if b != nil {
				t.Fatalf("Canonicalize(%s) returned %d bytes alongside an error; it must never return partial bytes", tc.name, len(b))
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Canonicalize(%s) = %v, want an error matching ErrInvalid", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Canonicalize(%s) = %v, want the message to name %q so the operator learns WHICH field", tc.name, err, tc.want)
			}
		})
	}
}

// TestCanonicalizeDoesNotEchoAnOversizedAgentID pins the bound-before-quote
// discipline: an attestation arrives from a peer, so the agent id is
// attacker-chosen, and %q expands a control byte to four characters. An
// unbounded value would let a peer choose the size of the line we log about
// refusing it.
func TestCanonicalizeDoesNotEchoAnOversizedAgentID(t *testing.T) {
	a := katAttestation(t)
	a.AgentID = strings.Repeat("\x00", 64<<10)

	_, err := Canonicalize(a)
	if err == nil {
		t.Fatal("Canonicalize(64 KiB of NULs as an agent id) = nil error, want a refusal")
	}
	if len(err.Error()) > 512 {
		t.Fatalf("the refusal is %d bytes long; an oversized id must not be echoed", len(err.Error()))
	}
}

// TestCanonicalizeAcceptsTheLongestValidAgentID is the anti-drift guard: this
// format's bound on an agent id IS ids.MaxAgentIDLen, because it is ids that
// enforces it. If ids ever admits a longer id and this format cannot carry it,
// an operator would be told a peer sent no valid attestation when in fact our
// own limits drifted — the worst kind of misattribution.
func TestCanonicalizeAcceptsTheLongestValidAgentID(t *testing.T) {
	longest := strings.Repeat("b", 64) + "." + strings.Repeat("n", 64) + "-18446744073709551615"
	if len(longest) != ids.MaxAgentIDLen {
		t.Fatalf("test setup: built a %d-byte id, want ids.MaxAgentIDLen = %d", len(longest), ids.MaxAgentIDLen)
	}
	if _, _, _, err := ids.ParseAgentID(longest); err != nil {
		t.Fatalf("test setup: %q is not a valid agent id: %v", longest, err)
	}

	a := katAttestation(t)
	a.AgentID = longest
	b, err := Canonicalize(a)
	if err != nil {
		t.Fatalf("Canonicalize(longest valid agent id) = error %v; the format must carry every id ids admits", err)
	}
	if want := 4 + len(Context) + 4 + len(longest) + 4 + ed25519.PublicKeySize + 24; len(b) != want {
		t.Fatalf("canonical length %d, want %d", len(b), want)
	}
}

func mutate(a Attestation, f func(*Attestation)) Attestation {
	f(&a)
	return a
}
