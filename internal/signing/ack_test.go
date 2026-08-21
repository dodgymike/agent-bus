package signing

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// katAck is the fixture every test in this file starts from.
//
// The correlation key names bus-a and the recipient lives on bus-b ON PURPOSE.
// That is the RELAY case, which is the general case: the recipient of a message
// is routinely on a different bus from the one that minted the message id. A
// fixture where the two halves coincided would let someone add the "the bus
// halves must agree" check §6.2 warns about, and every test here would still
// pass while multi-hop acknowledgement was broken in production.
func katAck() Ack {
	return Ack{
		CorrelationKey:     "bus-a-7",
		Recipient:          "bus-b.bob-2",
		Outcome:            AckOutcomeRefused,
		Class:              AckClassRecipientRefusedPolicy,
		EmittedAtUnixMilli: 1,
	}
}

func mutateAck(a Ack, f func(*Ack)) Ack {
	f(&a)
	return a
}

// ---------------------------------------------------------------------------
// The hand-computed anchors.
//
// Everything else compares the encoder against a second encoder written from
// the same specification, and two encoders sharing one misreading would agree
// with each other. These two expectations were written out by hand from the
// layout table in CanonicalizeAck's doc, byte by byte. If one fails, do not
// regenerate it: read the layout table.
// ---------------------------------------------------------------------------

func TestCanonicalizeAckHandComputedLayout(t *testing.T) {
	const want = "" +
		// uint32(25) || "agent-bus/recipient-ack/3"
		"00000019" + "6167656e742d6275732f726563697069656e742d61636b2f33" +
		// uint32(7) || "bus-a-7"                     (correlation key)
		"00000007" + "6275732d612d37" +
		// uint32(11) || "bus-b.bob-2"                (recipient — a DIFFERENT bus)
		"0000000b" + "6275732d622e626f622d32" +
		// uint32(7) || "refused"
		"00000007" + "72656675736564" +
		// uint32(24) || "recipient_refused_policy"
		"00000018" + "726563697069656e745f726566757365645f706f6c696379" +
		// int64(1) emitted-at, Unix milliseconds
		"0000000000000001"

	got, err := CanonicalizeAck(katAck())
	if err != nil {
		t.Fatalf("CanonicalizeAck: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Fatalf("canonical ACK bytes do not match the hand-computed layout\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
}

// TestCanonicalizeAckHandComputedLayoutDelivered is the SECOND hand-written
// anchor and it exists for one specific reason: a positive acknowledgement
// carries no class, and the empty class must still be encoded as a PRESENT
// zero-length field ("00000000"), never omitted.
//
// If the encoder were "optimised" to skip an empty field, the length word would
// disappear and the timestamp would slide up against the outcome. Every
// round-trip test would still pass — the encoder and its verifier would agree —
// and the format would have silently lost the property that makes it injective.
// Only a hand-computed expectation catches that.
func TestCanonicalizeAckHandComputedLayoutDelivered(t *testing.T) {
	a := mutateAck(katAck(), func(a *Ack) {
		a.Outcome = AckOutcomeDelivered
		a.Class = ""
		a.EmittedAtUnixMilli = 2
	})

	const want = "" +
		"00000019" + "6167656e742d6275732f726563697069656e742d61636b2f33" + // context
		"00000007" + "6275732d612d37" + // "bus-a-7"
		"0000000b" + "6275732d622e626f622d32" + // "bus-b.bob-2"
		"00000009" + "64656c697665726564" + // uint32(9) || "delivered"
		"00000000" + // the EMPTY class — present, zero length, never omitted
		"0000000000000002" // int64(2)

	got, err := CanonicalizeAck(a)
	if err != nil {
		t.Fatalf("CanonicalizeAck: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Fatalf("canonical ACK bytes do not match the hand-computed layout\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
}

// TestAckContextSpellsTheFormatVersion pins the single version indicator:
// AckFormatVersion and the version inside AckContext can never disagree,
// because a mismatch fails the build's tests rather than shipping two versions
// of one format.
func TestAckContextSpellsTheFormatVersion(t *testing.T) {
	if want := fmt.Sprintf("agent-bus/recipient-ack/%d", AckFormatVersion); AckContext != want {
		t.Fatalf("AckContext = %q, want %q — the format version is spelled ONCE, inside AckContext", AckContext, want)
	}
	// The three signing-format-version reservations are distinct values. If two
	// ever coincide, two different layouts hang off one key.
	if AckFormatVersion == FormatVersion {
		t.Fatalf("AckFormatVersion == FormatVersion == %d; each canonical format holds its OWN reservation from the signing-format-version namespace", AckFormatVersion)
	}
}

// ---------------------------------------------------------------------------
// Domain separation. THE central property, and it is proved by MUTATION.
//
// The recipient's MESSAGING key signs both messages and acknowledgements, so —
// unlike internal/attest, which is separated a second time by using a different
// key entirely — the context prefix is the ONLY thing keeping the two languages
// apart. Every test below must be able to go red.
// ---------------------------------------------------------------------------

// ackBytesUnder is a REFERENCE ENCODER: it builds the canonical ACK layout with
// an arbitrary context prefix, so a test can mutate the one value a constant
// otherwise puts out of reach.
func ackBytesUnder(context string, a Ack) []byte {
	var out []byte
	put := func(s string) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		out = append(out, l[:]...)
		out = append(out, s...)
	}
	put(context)
	put(a.CorrelationKey)
	put(a.Recipient)
	put(a.Outcome)
	put(a.Class)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(a.EmittedAtUnixMilli))
	return append(out, ts[:]...)
}

func TestAckDomainSeparationIsLoadBearing(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	a := katAck()
	sig, err := SignAck(priv, a)
	if err != nil {
		t.Fatalf("SignAck: %v", err)
	}

	// THE ANTI-VACUITY CONTROL. Without this, "the signature does not verify
	// over the mutated bytes" would pass even if ackBytesUnder produced garbage
	// for every input — a guard that cannot distinguish a real finding from its
	// own bug. Proving the reference encoder reproduces the REAL context first
	// is what makes the mutations below mean something.
	if !ed25519.Verify(pub, ackBytesUnder(AckContext, a), sig) {
		t.Fatal("the reference encoder does not reproduce the real canonical bytes; every mutation below would pass vacuously")
	}

	// Now mutate ONLY the domain-separation prefix. Each of these is a plausible
	// near-miss, including the other two contexts this codebase already signs.
	for _, ctx := range []string{
		Context,                         // "agent-bus/msg-sig/1"     — the message language
		"agent-bus/bus-attest/2",        // the bus attestation language
		"agent-bus/recipient-ack/2",     // a different format version
		"agent-bus/recipient-ack/4",     // a future format version
		"agent-bus/recipient-ack/3 ",    // one trailing space
		" agent-bus/recipient-ack/3",    // one leading space
		"agent-bus/recipient-ack/3\x00", // a NUL, which a length-prefixed field permits
		"Agent-bus/recipient-ack/3",     // one case change
		"",                              // no domain separation at all
	} {
		if ed25519.Verify(pub, ackBytesUnder(ctx, a), sig) {
			t.Fatalf("an ACK signature verified under context %q; the domain-separation prefix is NOT inside the signed bytes", ctx)
		}
	}
}

// TestAckCanonicalIsDisjointFromMessageSigning checks — rather than asserts in
// prose — that no byte string is a valid encoding of both an agent-signed
// message and that same agent's acknowledgement.
func TestAckCanonicalIsDisjointFromMessageSigning(t *testing.T) {
	framing := func(ctx string) []byte {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ctx)))
		return append(l[:], ctx...)
	}
	msgFirst := framing(Context)
	ackFirst := framing(AckContext)

	if bytes.HasPrefix(msgFirst, ackFirst) || bytes.HasPrefix(ackFirst, msgFirst) {
		t.Fatalf("one context framing is a prefix of the other:\n msg %s\n ack %s", hex.EncodeToString(msgFirst), hex.EncodeToString(ackFirst))
	}
	if len(Context) == len(AckContext) {
		t.Fatalf("the two contexts are the same length (%d), so the length word no longer separates them; check the byte content instead before relaxing this", len(AckContext))
	}

	ackBytes, err := CanonicalizeAck(katAck())
	if err != nil {
		t.Fatalf("CanonicalizeAck: %v", err)
	}
	if bytes.HasPrefix(ackBytes, msgFirst) {
		t.Fatal("a canonical acknowledgement begins with the canonical MESSAGE framing; the two languages are no longer disjoint")
	}
	if !bytes.HasPrefix(ackBytes, ackFirst) {
		t.Fatal("a canonical acknowledgement does not begin with its own context framing")
	}

	msgBytes, err := Canonicalize(Message{
		MessageID:          "bus-a-7",
		Sequence:           7,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-b.bob-2"},
		TimestampUnixMilli: 1,
		Body:               []byte("hi"),
	})
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if bytes.HasPrefix(msgBytes, ackFirst) {
		t.Fatal("a canonical message begins with the canonical ACKNOWLEDGEMENT framing; the two languages are no longer disjoint")
	}
}

// TestAckSignatureDoesNotVerifyAsMessage is the cross-protocol confusion proof,
// in BOTH directions, under ONE key — which is the situation that actually
// obtains, because an agent's messaging key signs both languages.
//
// The two artefacts are deliberately about the SAME message id and the SAME
// pair of agents, so nothing but the domain separation is doing the work.
func TestAckSignatureDoesNotVerifyAsMessage(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	m := Message{
		MessageID:          "bus-a-7",
		Sequence:           7,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-b.bob-2"},
		TimestampUnixMilli: 1,
		Body:               []byte("hi"),
	}
	a := katAck()

	msgSig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	ackSig, err := SignAck(priv, a)
	if err != nil {
		t.Fatalf("SignAck: %v", err)
	}

	// The controls: each signature verifies in its OWN language. Without these
	// the two negative assertions below would pass even if Sign and SignAck
	// were both broken.
	if err := Verify(pub, m, msgSig); err != nil {
		t.Fatalf("a message signature does not verify as a message: %v", err)
	}
	if err := VerifyAck(pub, a, ackSig); err != nil {
		t.Fatalf("an ACK signature does not verify as an ACK: %v", err)
	}

	if err := Verify(pub, m, ackSig); !errors.Is(err, ErrVerify) {
		t.Fatalf("an ACKNOWLEDGEMENT signature was accepted as a MESSAGE signature (err = %v); a refusal replayed as a message body is exactly what the domain separation exists to stop", err)
	}
	if err := VerifyAck(pub, a, msgSig); !errors.Is(err, ErrVerify) {
		t.Fatalf("a MESSAGE signature was accepted as an ACKNOWLEDGEMENT signature (err = %v); a message replayed as an attested refusal is the other direction of the same hole", err)
	}
}

// ---------------------------------------------------------------------------
// Field coverage, proved by MUTATION.
//
// This is the guard against the failure this package cannot survive: a field
// that is in the struct, is documented as covered, and is not actually in the
// encoder. A round-trip test cannot see it — signer and verifier omit it
// together and agree perfectly. Mutating each field in turn and requiring the
// ORIGINAL signature to stop verifying is the only assertion that can.
// ---------------------------------------------------------------------------

func TestAckSignatureCoversEveryField(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	base := katAck()
	sig, err := SignAck(priv, base)
	if err != nil {
		t.Fatalf("SignAck: %v", err)
	}
	if err := VerifyAck(pub, base, sig); err != nil {
		t.Fatalf("control: the unmutated acknowledgement does not verify: %v", err)
	}

	cases := []struct {
		field string
		why   string
		a     Ack
	}{
		{
			"CorrelationKey", "a signature that did not cover it could be moved onto another message",
			mutateAck(base, func(a *Ack) { a.CorrelationKey = "bus-a-8" }),
		},
		{
			"CorrelationKey (bus half)", "a signature that did not cover it could be re-attributed to another origin bus",
			mutateAck(base, func(a *Ack) { a.CorrelationKey = "bus-c-7" }),
		},
		{
			"Recipient", "a signature that did not cover it could be transplanted onto a different recipient of the same message",
			mutateAck(base, func(a *Ack) { a.Recipient = "bus-b.carol-3" }),
		},
		{
			"Outcome", "a bus on the path could flip refused to delivered, or the reverse",
			mutateAck(base, func(a *Ack) { a.Outcome = AckOutcomeDelivered; a.Class = "" }),
		},
		{
			"Class", "a bus on the path could change WHY the recipient refused",
			mutateAck(base, func(a *Ack) { a.Class = AckClassRecipientRefusedNotAddressed }),
		},
		{
			"EmittedAtUnixMilli", "a bus on the path could restate when the recipient spoke",
			mutateAck(base, func(a *Ack) { a.EmittedAtUnixMilli = 2 }),
		},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			if err := VerifyAck(pub, tc.a, sig); !errors.Is(err, ErrVerify) {
				t.Fatalf("mutating %s left the ORIGINAL signature verifying (err = %v) — the field is NOT covered by the signature: %s", tc.field, err, tc.why)
			}
			b1, err := CanonicalizeAck(base)
			if err != nil {
				t.Fatalf("CanonicalizeAck(base): %v", err)
			}
			b2, err := CanonicalizeAck(tc.a)
			if err != nil {
				t.Fatalf("CanonicalizeAck(mutated): %v", err)
			}
			if bytes.Equal(b1, b2) {
				t.Fatalf("mutating %s produced IDENTICAL canonical bytes; the encoding is not injective over that field", tc.field)
			}
		})
	}
}

// TestCanonicalizeAckIsInjectiveAcrossFieldBoundaries proves the length
// prefixes do the job they are there for: no attacker can shift bytes across a
// field boundary and present a different logical acknowledgement under bytes
// that are identical.
//
// The pairs below are chosen so that a naive concatenating encoder — one that
// joined the fields with no length prefix, or with a separator character — would
// produce the same bytes for both members.
//
// # Which member goes through production, and why it matters
//
// The WELL-FORMED member is encoded by CanonicalizeAck itself, so a regression
// in the PRODUCTION encoder fails this test. Its shifted partner cannot be:
// "bus-a-7bus" is not a message id and "-b.bob-2" is not an agent id, so
// validateAck refuses them — correctly, and that refusal is a second layer of
// defence rather than a reason to skip the layout check. The partner is
// therefore built by the reference encoder, which TestAckDomainSeparationIsLoadBearing
// has already proved reproduces CanonicalizeAck byte for byte on a well-formed
// input.
func TestCanonicalizeAckIsInjectiveAcrossFieldBoundaries(t *testing.T) {
	pairs := [][2]Ack{
		{
			// The boundary between the correlation key and the recipient.
			mutateAck(katAck(), func(a *Ack) { a.CorrelationKey = "bus-a-7"; a.Recipient = "bus-b.bob-2" }),
			mutateAck(katAck(), func(a *Ack) { a.CorrelationKey = "bus-a-7bus"; a.Recipient = "-b.bob-2" }),
		},
		{
			// The boundary between the outcome and the class.
			mutateAck(katAck(), func(a *Ack) { a.Outcome = "refused"; a.Class = "recipient_refused_policy" }),
			mutateAck(katAck(), func(a *Ack) { a.Outcome = "refusedrecipient"; a.Class = "_refused_policy" }),
		},
	}
	for i, p := range pairs {
		b1, err := CanonicalizeAck(p[0])
		if err != nil {
			t.Fatalf("pair %d: the well-formed member must go through the PRODUCTION encoder, but it was refused: %v", i, err)
		}
		// The control: the reference encoder agrees with production on this
		// input, so using it for the shifted partner is sound.
		if !bytes.Equal(b1, ackBytesUnder(AckContext, p[0])) {
			t.Fatalf("pair %d: the reference encoder disagrees with CanonicalizeAck; the comparison below would be meaningless", i)
		}
		b2 := ackBytesUnder(AckContext, p[1])
		if bytes.Equal(b1, b2) {
			t.Fatalf("pair %d encodes to identical bytes; bytes can be shifted across a field boundary, so the encoding is not injective", i)
		}
		// The shifted partner must ALSO be refused outright — the layout is the
		// first defence, the field rules the second.
		if _, err := CanonicalizeAck(p[1]); err == nil {
			t.Fatalf("pair %d: the shifted partner was accepted as a well-formed acknowledgement", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Round trip.
// ---------------------------------------------------------------------------

func TestSignAckVerifyAckRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	for _, a := range []Ack{
		katAck(),
		mutateAck(katAck(), func(a *Ack) { a.Outcome = AckOutcomeDelivered; a.Class = "" }),
		mutateAck(katAck(), func(a *Ack) { a.Class = AckClassRecipientRefusedUndecodable }),
		mutateAck(katAck(), func(a *Ack) { a.Class = AckClassRecipientRefusedNotAddressed }),
		// The LOCAL case: origin bus and recipient bus coincide. It must work
		// too — it is a special case of the general rule, not the rule.
		mutateAck(katAck(), func(a *Ack) { a.Recipient = "bus-a.bob-2" }),
		mutateAck(katAck(), func(a *Ack) { a.EmittedAtUnixMilli = 1 << 42 }),
	} {
		sig, err := SignAck(priv, a)
		if err != nil {
			t.Fatalf("SignAck(%+v): %v", a, err)
		}
		if len(sig) != SignatureSize {
			t.Fatalf("SignAck produced %d bytes, want %d — relay.ValidateAckAttestation shape-checks exactly this", len(sig), SignatureSize)
		}
		if err := VerifyAck(pub, a, sig); err != nil {
			t.Fatalf("VerifyAck(%+v): %v", a, err)
		}
	}
}

// TestCanonicalizeAckDoesNotBindTheBusHalves is a POSITIVE guard against a
// plausible future "hardening" that would be a defect.
//
// The message format requires the bus that minted the message id to be the bus
// that qualifies the sender. Copying that rule here — requiring the correlation
// key's bus half to equal the recipient's — would refuse every cross-bus
// acknowledgement, i.e. exactly the ones the relay plane exists for. §6.2 names
// the trap. This test goes red the day someone adds the check.
func TestCanonicalizeAckDoesNotBindTheBusHalves(t *testing.T) {
	for _, a := range []Ack{
		mutateAck(katAck(), func(a *Ack) { a.CorrelationKey = "bus-a-7"; a.Recipient = "bus-b.bob-2" }),
		mutateAck(katAck(), func(a *Ack) { a.CorrelationKey = "bus-a-7"; a.Recipient = "bus-z.bob-2" }),
	} {
		if _, err := CanonicalizeAck(a); err != nil {
			t.Fatalf("CanonicalizeAck refused a CROSS-BUS acknowledgement (%q acknowledging %q): %v\nA multi-hop ACK legitimately crosses buses; requiring the halves to agree breaks the relay plane (ACK-CONTRACT.md §6.2).", a.Recipient, a.CorrelationKey, err)
		}
	}
}

// ---------------------------------------------------------------------------
// validateAck(): every rejection has its own case.
// ---------------------------------------------------------------------------

func TestCanonicalizeAckRejectsMalformed(t *testing.T) {
	good := katAck()

	cases := []struct {
		name string
		a    Ack
		want string
	}{
		{"zero value", Ack{}, "correlation key"},
		{"empty correlation key", mutateAck(good, func(a *Ack) { a.CorrelationKey = "" }), "correlation key"},
		{"correlation key is not a message id", mutateAck(good, func(a *Ack) { a.CorrelationKey = "bus-a" }), "correlation key"},
		{"correlation key is an agent id", mutateAck(good, func(a *Ack) { a.CorrelationKey = "bus-a.alice-1" }), "correlation key"},
		{"oversized correlation key", mutateAck(good, func(a *Ack) { a.CorrelationKey = strings.Repeat("a", 4096) }), "correlation key"},

		{"empty recipient", mutateAck(good, func(a *Ack) { a.Recipient = "" }), "recipient"},
		{"unqualified recipient", mutateAck(good, func(a *Ack) { a.Recipient = "bob-2" }), "recipient"},
		{"recipient with no minted suffix", mutateAck(good, func(a *Ack) { a.Recipient = "bus-b.bob" }), "recipient"},
		{"oversized recipient", mutateAck(good, func(a *Ack) { a.Recipient = strings.Repeat("a", 4096) }), "recipient"},

		{"empty outcome", mutateAck(good, func(a *Ack) { a.Outcome = "" }), "outcome is empty"},
		{"unknown outcome", mutateAck(good, func(a *Ack) { a.Outcome = "acked" }), "not one of"},
		{"outcome undeliverable", mutateAck(good, func(a *Ack) { a.Outcome = "undeliverable" }), "undeliverable"},
		{"outcome accepted", mutateAck(good, func(a *Ack) { a.Outcome = "accepted" }), "not one of"},
		{"outcome in_flight", mutateAck(good, func(a *Ack) { a.Outcome = "in_flight" }), "not one of"},
		{"outcome unknown", mutateAck(good, func(a *Ack) { a.Outcome = "unknown" }), "not one of"},
		{"outcome case change", mutateAck(good, func(a *Ack) { a.Outcome = "Refused" }), "not one of"},
		{"outcome with padding", mutateAck(good, func(a *Ack) { a.Outcome = " refused" }), "not one of"},

		{"delivered with a class", mutateAck(good, func(a *Ack) { a.Outcome = AckOutcomeDelivered }), "carries no class at all"},
		{"refused with no class", mutateAck(good, func(a *Ack) { a.Class = "" }), "requires a recipient-emitted class"},
		{"refused with an unknown class", mutateAck(good, func(a *Ack) { a.Class = "recipient_refused_because" }), "not one of the three"},
		{"refused with a bus-emitted class", mutateAck(good, func(a *Ack) { a.Class = "no_route" }), "not one of the three"},
		{"refused with horizon_expired", mutateAck(good, func(a *Ack) { a.Class = "horizon_expired" }), "not one of the three"},
		{"refused with obligation_lost", mutateAck(good, func(a *Ack) { a.Class = "obligation_lost" }), "not one of the three"},
		{"class case change", mutateAck(good, func(a *Ack) { a.Class = "Recipient_Refused_Policy" }), "not one of the three"},
		{"class with padding", mutateAck(good, func(a *Ack) { a.Class = "recipient_refused_policy " }), "not one of the three"},

		{"unset emitted-at", mutateAck(good, func(a *Ack) { a.EmittedAtUnixMilli = 0 }), "not a positive Unix millisecond"},
		{"negative emitted-at", mutateAck(good, func(a *Ack) { a.EmittedAtUnixMilli = -1 }), "not a positive Unix millisecond"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := CanonicalizeAck(tc.a)
			if err == nil {
				t.Fatalf("CanonicalizeAck accepted a malformed acknowledgement and produced %d bytes", len(b))
			}
			if b != nil {
				t.Fatalf("CanonicalizeAck returned %d bytes ALONGSIDE an error; it must never return partial or best-effort bytes", len(b))
			}
			if !errors.Is(err, ErrInvalidAck) {
				t.Fatalf("error does not wrap ErrInvalidAck: %v", err)
			}
			// ErrInvalid is the MESSAGE sentinel. A caller failing closed on
			// "not a canonicalizable message" must not silently swallow an
			// acknowledgement failure as the same event.
			if errors.Is(err, ErrInvalid) {
				t.Fatalf("an ACK failure also matches ErrInvalid, the MESSAGE sentinel: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestSignAckRejectsMalformedPrivateKey pins the panic trap: ed25519.Sign PANICS
// on a wrong-size private key, so the length is checked first — and the key is
// never echoed.
func TestSignAckRejectsMalformedPrivateKey(t *testing.T) {
	for _, priv := range []ed25519.PrivateKey{nil, make([]byte, 0), make([]byte, 32), make([]byte, 63), make([]byte, 65)} {
		sig, err := SignAck(priv, katAck())
		if !errors.Is(err, ErrPrivateKeyLength) {
			t.Fatalf("SignAck(%d-byte key) = %v, want ErrPrivateKeyLength", len(priv), err)
		}
		if sig != nil {
			t.Fatal("SignAck returned a signature alongside an error")
		}
	}
}

// TestSignAckDoesNotLeakPrivateKeyBytes is the ACK twin of
// TestSignDoesNotLeakPrivateKeyBytes.
//
// It uses DISTINCTIVE fill bytes rather than the zero-filled keys the test
// above uses, and that is the entire point: a private key that reached an error
// string would be invisible against an all-zero key, because the error already
// contains "0" characters and a run of NULs is not distinguishable from
// ordinary formatting. A leak is only detectable if the key is recognisable.
func TestSignAckDoesNotLeakPrivateKeyBytes(t *testing.T) {
	for _, c := range []struct {
		name string
		priv ed25519.PrivateKey
	}{
		{"63 bytes", bytes.Repeat([]byte{0xAB}, 63)},
		{"65 bytes", bytes.Repeat([]byte{0xCD}, 65)},
		{"32 bytes", bytes.Repeat([]byte{0xEF}, 32)},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := SignAck(c.priv, katAck())
			if err == nil {
				t.Fatalf("SignAck(priv len %d) unexpectedly succeeded", len(c.priv))
			}
			errText := err.Error()
			if strings.Contains(errText, string(c.priv)) {
				t.Fatalf("SignAck error text contains the raw private key bytes: %q", errText)
			}
			if strings.Contains(errText, hex.EncodeToString(c.priv)) {
				t.Fatalf("SignAck error text contains the hex-encoded private key: %q", errText)
			}
			// The LENGTH is reported, and must be — it is what tells an
			// operator their key file is truncated.
			if !strings.Contains(errText, fmt.Sprintf("%d bytes", len(c.priv))) {
				t.Fatalf("SignAck error does not report the key LENGTH, which is the one thing it should say: %q", errText)
			}
		})
	}
}

// TestVerifyAckChecksTheKeyBeforeTheSignature pins the ORDER, which is
// load-bearing: crypto/ed25519.Verify PANICS rather than returning false on a
// malformed public key, so a key from anywhere an attacker influences would be
// a remote denial of service if the signature were checked first.
func TestVerifyAckChecksTheKeyBeforeTheSignature(t *testing.T) {
	// A malformed key AND a malformed signature: the key error must win, which
	// can only happen if the key is checked first.
	err := VerifyAck(make([]byte, 31), katAck(), []byte{1, 2, 3})
	if !errors.Is(err, ErrPublicKeyLength) {
		t.Fatalf("VerifyAck(short key, short sig) = %v, want ErrPublicKeyLength — the key must be checked BEFORE the signature or ed25519.Verify panics", err)
	}
}

func TestVerifyAckRejectsAbsentAndMisshapenSignatures(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := VerifyAck(pub, katAck(), nil); !errors.Is(err, ErrNoSignature) {
		t.Fatalf("VerifyAck(nil sig) = %v, want ErrNoSignature", err)
	}
	if err := VerifyAck(pub, katAck(), make([]byte, 63)); !errors.Is(err, ErrSignatureLength) {
		t.Fatalf("VerifyAck(63-byte sig) = %v, want ErrSignatureLength", err)
	}
	// A malformed acknowledgement has no signable bytes, so nothing can verify.
	if err := VerifyAck(pub, Ack{}, make([]byte, SignatureSize)); !errors.Is(err, ErrInvalidAck) {
		t.Fatalf("VerifyAck(malformed ack) = %v, want ErrInvalidAck", err)
	}
}

// TestElideAckTokenBoundsARemoteChosenToken proves the elision can actually
// fire: a rejected token is echoed into an error, and a remote party chooses
// those bytes.
func TestElideAckTokenBoundsARemoteChosenToken(t *testing.T) {
	long := strings.Repeat("z", 4096)
	_, err := CanonicalizeAck(mutateAck(katAck(), func(a *Ack) { a.Class = long }))
	if err == nil {
		t.Fatal("CanonicalizeAck accepted a 4096-byte class")
	}
	if strings.Contains(err.Error(), long) {
		t.Fatal("the full remote-chosen token reached the error string; a remote party must not choose the size of the line we log about refusing it")
	}
	if !strings.Contains(err.Error(), "…") {
		t.Fatalf("the oversized token was not elided: %v", err)
	}
}

// TestElideAckTokenCutsOnARuneBoundary is the case the ASCII fixture above
// STRUCTURALLY CANNOT reach: with every rune one byte wide, a naive cut at a
// raw byte offset is indistinguishable from a correct one, so that test would
// pass over a mid-rune truncation.
//
// A remote party chooses these bytes, so a multi-byte rune straddling the cut
// is reachable input. Cutting through it would put an invalid UTF-8 fragment
// into an operator's log, which the %q the callers format with then renders as
// an escape nobody can read back to the original bytes — relay.elideOutbox
// records the same reasoning.
//
// The offsets are chosen so the rune straddles maxElidedAckToken: a 3-byte rune
// is placed to start one byte before the cut, so a raw cut lands inside it.
func TestElideAckTokenCutsOnARuneBoundary(t *testing.T) {
	for _, pad := range []int{maxElidedAckToken - 2, maxElidedAckToken - 1, maxElidedAckToken} {
		token := strings.Repeat("z", pad) + strings.Repeat("→", 8) // '→' is 3 bytes
		got := elideAckToken(token)
		if got == token {
			t.Fatalf("pad %d: token was not elided at all", pad)
		}
		trimmed := strings.TrimSuffix(got, "…")
		if !utf8.ValidString(trimmed) {
			t.Fatalf("pad %d: elided token %q is not valid UTF-8; the cut landed mid-rune", pad, trimmed)
		}
		if len(trimmed) > maxElidedAckToken {
			t.Fatalf("pad %d: elided token is %d bytes, over the %d-byte bound", pad, len(trimmed), maxElidedAckToken)
		}
	}

	// And the whole path end to end: a multi-byte token rejected by
	// CanonicalizeAck must leave a valid-UTF-8 error string.
	_, err := CanonicalizeAck(mutateAck(katAck(), func(a *Ack) { a.Class = strings.Repeat("→", 64) }))
	if err == nil {
		t.Fatal("CanonicalizeAck accepted a 192-byte class")
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("the error string is not valid UTF-8: %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// The published test vector.
//
// testdata/ack_vectors.json is a PUBLISHED ARTIFACT: the follow-on CLI task and
// any second implementation check themselves against it. Regenerating it to
// make a test pass is a wire-format change, not a fix.
// ---------------------------------------------------------------------------

type ackVectorFile struct {
	FormatVersion int    `json:"format_version"`
	Context       string `json:"context"`
	SeedHex       string `json:"ed25519_seed_hex"`
	PublicKeyHex  string `json:"ed25519_public_key_hex"`
	Vectors       []struct {
		Name               string `json:"name"`
		CorrelationKey     string `json:"correlation_key"`
		Recipient          string `json:"recipient"`
		Outcome            string `json:"outcome"`
		Class              string `json:"class"`
		EmittedAtUnixMilli int64  `json:"emitted_at_unix_milli"`
		CanonicalHex       string `json:"canonical_hex"`
		SignatureHex       string `json:"signature_hex"`
	} `json:"vectors"`
}

func TestAckPublishedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ack_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var f ackVectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if f.FormatVersion != AckFormatVersion {
		t.Fatalf("vector file format_version = %d, want %d", f.FormatVersion, AckFormatVersion)
	}
	if f.Context != AckContext {
		t.Fatalf("vector file context = %q, want %q", f.Context, AckContext)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("the vector file holds no vectors; a file that asserts nothing is worse than none")
	}

	seed, err := hex.DecodeString(f.SeedHex)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if got := hex.EncodeToString(pub); got != f.PublicKeyHex {
		t.Fatalf("the seed derives public key %s, but the file says %s", got, f.PublicKeyHex)
	}

	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			a := Ack{
				CorrelationKey:     v.CorrelationKey,
				Recipient:          v.Recipient,
				Outcome:            v.Outcome,
				Class:              v.Class,
				EmittedAtUnixMilli: v.EmittedAtUnixMilli,
			}
			b, err := CanonicalizeAck(a)
			if err != nil {
				t.Fatalf("CanonicalizeAck: %v", err)
			}
			if got := hex.EncodeToString(b); got != v.CanonicalHex {
				t.Fatalf("canonical bytes differ from the published vector\n got %s\nwant %s", got, v.CanonicalHex)
			}
			sig, err := SignAck(priv, a)
			if err != nil {
				t.Fatalf("SignAck: %v", err)
			}
			if got := hex.EncodeToString(sig); got != v.SignatureHex {
				t.Fatalf("signature differs from the published vector\n got %s\nwant %s", got, v.SignatureHex)
			}
			if err := VerifyAck(pub, a, sig); err != nil {
				t.Fatalf("VerifyAck: %v", err)
			}
		})
	}
}
