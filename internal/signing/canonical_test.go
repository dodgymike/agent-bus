package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// The hand-computed vector.
//
// Everything else in this file compares the encoder against either its own
// recorded output (the vector file) or a second encoder written from the same
// specification — both of which would agree with each other if the LAYOUT ITSELF
// were wrong. This one expectation was written out by hand from PROTOCOL.md §8
// with an ASCII table, byte by byte, and is the anchor that makes the rest mean
// something. If it fails, do not regenerate it: read the layout table.
// ---------------------------------------------------------------------------

func TestCanonicalizeHandComputedLayout(t *testing.T) {
	m := Message{
		MessageID:          "bus-a-1",
		Sequence:           1,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-a.bob-2"},
		TimestampUnixMilli: 1,
		Body:               []byte("hi"),
	}

	const want = "" +
		// uint32(19) || "agent-bus/msg-sig/1"
		"00000013" + "6167656e742d6275732f6d73672d7369672f31" +
		// uint32(7) || "bus-a-1"
		"00000007" + "6275732d612d31" +
		// uint64(1) sequence
		"0000000000000001" +
		// uint32(13) || "bus-a.alice-1"
		"0000000d" + "6275732d612e616c6963652d31" +
		// uint32(1) recipient count
		"00000001" +
		// uint32(11) || "bus-a.bob-2"
		"0000000b" + "6275732d612e626f622d32" +
		// int64(1) timestamp, Unix milliseconds
		"0000000000000001" +
		// uint32(2) || "hi"
		"00000002" + "6869"

	got, err := Canonicalize(m)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Fatalf("canonical bytes do not match the hand-computed layout\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
}

// TestCanonicalizeHandComputedLayoutSortsRecipients is the second hand-written
// anchor, and it exists because the first one has a single recipient: with only
// one, the SORT ORDER of the recipient block is pinned by nothing but the
// vector file and the test file's own reference encoder, which could be flipped
// to descending together without any hand-computed expectation objecting. Here
// the recipients are supplied carol-then-bob and the expected bytes — written
// out by hand from PROTOCOL.md §8.3 — spell bob first.
func TestCanonicalizeHandComputedLayoutSortsRecipients(t *testing.T) {
	m := Message{
		MessageID:          "bus-a-2",
		Sequence:           2,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-a.carol-1", "bus-a.bob-2"},
		TimestampUnixMilli: 2,
		Body:               []byte("ok"),
	}

	const want = "" +
		"00000013" + "6167656e742d6275732f6d73672d7369672f31" + // context
		"00000007" + "6275732d612d32" + // "bus-a-2"
		"0000000000000002" + // sequence
		"0000000d" + "6275732d612e616c6963652d31" + // "bus-a.alice-1"
		"00000002" + // two recipients
		"0000000b" + "6275732d612e626f622d32" + // "bus-a.bob-2"   — sorted FIRST
		"0000000d" + "6275732d612e6361726f6c2d31" + // "bus-a.carol-1" — sorted SECOND
		"0000000000000002" + // timestamp
		"00000002" + "6f6b" // "ok"

	got, err := Canonicalize(m)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Fatalf("canonical bytes do not match the hand-computed layout\n got %s\nwant %s", hex.EncodeToString(got), want)
	}
}

// TestCanonicalizeContextSpellsTheFormatVersion pins the single version
// indicator: FormatVersion and the version inside Context can never disagree,
// because a mismatch here fails the build's tests rather than shipping two
// versions of one format.
func TestCanonicalizeContextSpellsTheFormatVersion(t *testing.T) {
	if want := fmt.Sprintf("agent-bus/msg-sig/%d", FormatVersion); Context != want {
		t.Fatalf("Context = %q, want %q — the format version is spelled ONCE, inside Context", Context, want)
	}
}

// ---------------------------------------------------------------------------
// The published test vectors.
// ---------------------------------------------------------------------------

// vectorFile mirrors testdata/canonical_vectors.json. That file is a PUBLISHED
// ARTIFACT: SIGN-2, SIGN-5 and CRYPTO-10 check their own implementations
// against it, so a change to it is a change to the wire format, never a way to
// make a red test go green. There is deliberately no -update flag.
type vectorFile struct {
	Format              string   `json:"format"`
	FormatVersion       int      `json:"format_version"`
	Context             string   `json:"context"`
	Note                string   `json:"note"`
	Ed25519SeedHex      string   `json:"ed25519_seed_hex"`
	Ed25519PublicKeyHex string   `json:"ed25519_public_key_hex"`
	Vectors             []vector `json:"vectors"`
}

type vector struct {
	Name                string     `json:"name"`
	Why                 string     `json:"why"`
	Message             vecMessage `json:"message"`
	CanonicalHex        string     `json:"canonical_hex"`
	CanonicalSHA256Hex  string     `json:"canonical_sha256_hex"`
	Ed25519SignatureHex string     `json:"ed25519_signature_hex"`
}

type vecMessage struct {
	MessageID          string   `json:"message_id"`
	Sequence           uint64   `json:"sequence"`
	Sender             string   `json:"sender"`
	Recipients         []string `json:"recipients"`
	TimestampUnixMilli int64    `json:"timestamp_unix_ms"`
	BodyHex            string   `json:"body_hex"`
}

func (v vecMessage) message(t *testing.T) Message {
	t.Helper()
	body, err := hex.DecodeString(v.BodyHex)
	if err != nil {
		t.Fatalf("body_hex %q: %v", v.BodyHex, err)
	}
	return Message{
		MessageID:          v.MessageID,
		Sequence:           v.Sequence,
		Sender:             v.Sender,
		Recipients:         v.Recipients,
		TimestampUnixMilli: v.TimestampUnixMilli,
		Body:               body,
	}
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "canonical_vectors.json"))
	if err != nil {
		t.Fatalf("reading test vectors: %v", err)
	}
	var vf vectorFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vf); err != nil {
		t.Fatalf("decoding test vectors: %v", err)
	}
	if len(vf.Vectors) == 0 {
		t.Fatal("test vector file contains no vectors")
	}
	return vf
}

// TestCanonicalizeVectors is the proof of record: every published vector is
// reproduced byte for byte, together with its SHA-256 and its Ed25519
// signature under a fixed key.
func TestCanonicalizeVectors(t *testing.T) {
	vf := loadVectors(t)

	if vf.Context != Context {
		t.Fatalf("vector file context %q != package Context %q", vf.Context, Context)
	}
	if vf.FormatVersion != FormatVersion {
		t.Fatalf("vector file format_version %d != package FormatVersion %d", vf.FormatVersion, FormatVersion)
	}

	priv, pub := vectorKey(t, vf)

	for _, v := range vf.Vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			m := v.Message.message(t)

			got, err := Canonicalize(m)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if gotHex := hex.EncodeToString(got); gotHex != v.CanonicalHex {
				t.Fatalf("canonical bytes differ\n got %s\nwant %s", gotHex, v.CanonicalHex)
			}

			sum, err := CanonicalDigest(m)
			if err != nil {
				t.Fatalf("CanonicalDigest: %v", err)
			}
			if gotHex := hex.EncodeToString(sum[:]); gotHex != v.CanonicalSHA256Hex {
				t.Fatalf("canonical sha256 differs\n got %s\nwant %s", gotHex, v.CanonicalSHA256Hex)
			}

			// Ed25519 is deterministic (RFC 8032), so the signature over a
			// pinned byte string under a pinned key is itself a vector. It
			// signs the canonical bytes UNHASHED — see the assertion below
			// that signing the digest instead produces something else.
			sig := ed25519.Sign(priv, got)
			if gotHex := hex.EncodeToString(sig); gotHex != v.Ed25519SignatureHex {
				t.Fatalf("ed25519 signature differs\n got %s\nwant %s", gotHex, v.Ed25519SignatureHex)
			}
			if !ed25519.Verify(pub, got, sig) {
				t.Fatal("ed25519.Verify rejected a signature over the canonical bytes")
			}
		})
	}
}

// vectorKey returns the fixed Ed25519 key the vectors are signed with, and
// checks it against RFC 8032 §7.1 TEST 1 — an externally published pair, so a
// broken key derivation cannot hide behind our own recorded output.
func vectorKey(t *testing.T, vf vectorFile) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	const (
		rfc8032Test1Seed      = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
		rfc8032Test1PublicKey = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	)
	if vf.Ed25519SeedHex != rfc8032Test1Seed {
		t.Fatalf("vector seed %q is not the RFC 8032 TEST 1 seed", vf.Ed25519SeedHex)
	}
	seed, err := hex.DecodeString(vf.Ed25519SeedHex)
	if err != nil {
		t.Fatalf("ed25519_seed_hex: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("ed25519 private key did not yield an ed25519 public key")
	}
	if got := hex.EncodeToString(pub); got != rfc8032Test1PublicKey {
		t.Fatalf("derived public key %s != RFC 8032 TEST 1 public key %s", got, rfc8032Test1PublicKey)
	}
	if got := vf.Ed25519PublicKeyHex; got != rfc8032Test1PublicKey {
		t.Fatalf("vector file public key %s != RFC 8032 TEST 1 public key %s", got, rfc8032Test1PublicKey)
	}
	return priv, pub
}

// ---------------------------------------------------------------------------
// The differential test — the point of the whole task.
// ---------------------------------------------------------------------------

// referenceEncode is a SECOND, independent serialisation written straight from
// the PROTOCOL.md §8 layout table using a different mechanism (bytes.Buffer +
// binary.Write rather than append + PutUint*). It exists to catch the failure
// this task is about: two implementations of "the same" format that disagree
// about a byte, which shows up in production as signatures that verify on the
// sender and fail on the recipient.
func referenceEncode(t *testing.T, m Message) []byte {
	t.Helper()
	var buf bytes.Buffer

	put := func(v interface{}) {
		if err := binary.Write(&buf, binary.BigEndian, v); err != nil {
			t.Fatalf("reference encoder: %v", err)
		}
	}
	str := func(s string) {
		put(uint32(len(s)))
		if _, err := buf.WriteString(s); err != nil {
			t.Fatalf("reference encoder: %v", err)
		}
	}

	str(Context)
	str(m.MessageID)
	put(m.Sequence)
	str(m.Sender)

	recipients := append([]string(nil), m.Recipients...)
	sort.Slice(recipients, func(i, j int) bool { return recipients[i] < recipients[j] })
	put(uint32(len(recipients)))
	for _, r := range recipients {
		str(r)
	}

	put(m.TimestampUnixMilli)
	put(uint32(len(m.Body)))
	if _, err := buf.Write(m.Body); err != nil {
		t.Fatalf("reference encoder: %v", err)
	}
	return buf.Bytes()
}

func TestCanonicalizeDifferentialAgainstIndependentEncoder(t *testing.T) {
	for _, m := range sampleMessages() {
		m := m
		t.Run(m.name, func(t *testing.T) {
			got, err := Canonicalize(m.msg)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			want := referenceEncode(t, m.msg)
			if !bytes.Equal(got, want) {
				t.Fatalf("two independent serialisations of the same message differ\n got %s\nwant %s",
					hex.EncodeToString(got), hex.EncodeToString(want))
			}
		})
	}
}

// TestCanonicalizeRecipientOrderDoesNotMatter is the other half of the
// differential claim: two senders that list the same audience in different
// orders must produce IDENTICAL bytes, or a broadcast's signature would depend
// on an arbitrary iteration order.
func TestCanonicalizeRecipientOrderDoesNotMatter(t *testing.T) {
	base := Message{
		MessageID:          "bus-a-7",
		Sequence:           7,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-a.bob-2", "bus-a.carol-1", "bus-far.dave-9"},
		TimestampUnixMilli: 1_700_000_000_000,
		Body:               []byte("fan-out"),
	}
	want, err := Canonicalize(base)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}

	shuffles := [][]string{
		{"bus-far.dave-9", "bus-a.carol-1", "bus-a.bob-2"},
		{"bus-a.carol-1", "bus-far.dave-9", "bus-a.bob-2"},
		{"bus-a.bob-2", "bus-far.dave-9", "bus-a.carol-1"},
	}
	for i, order := range shuffles {
		m := base
		m.Recipients = order
		got, err := Canonicalize(m)
		if err != nil {
			t.Fatalf("shuffle %d: Canonicalize: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("shuffle %d produced different bytes:\n got %s\nwant %s", i, hex.EncodeToString(got), hex.EncodeToString(want))
		}
	}

	// The caller's slice must come back untouched: sorting in place would
	// reorder a recipient list the caller still needs for delivery.
	original := []string{"bus-a.carol-1", "bus-a.bob-2"}
	m := base
	m.Recipients = original
	if _, err := Canonicalize(m); err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if original[0] != "bus-a.carol-1" || original[1] != "bus-a.bob-2" {
		t.Fatalf("Canonicalize mutated the caller's recipient slice: %v", original)
	}
}

// TestCanonicalizeEveryCoveredFieldChangesTheBytes is the complement: changing
// ANY covered field must change the output. A field that can change without
// changing the bytes is a field the signature does not protect.
func TestCanonicalizeEveryCoveredFieldChangesTheBytes(t *testing.T) {
	base := Message{
		MessageID:          "bus-a-7",
		Sequence:           7,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-a.bob-2", "bus-a.carol-1"},
		TimestampUnixMilli: 1_700_000_000_000,
		Body:               []byte("payload"),
	}
	baseBytes, err := Canonicalize(base)
	if err != nil {
		t.Fatalf("Canonicalize(base): %v", err)
	}

	mutations := []struct {
		field  string
		mutate func(m *Message)
	}{
		// The message id and the sequence move TOGETHER by construction: the
		// sequence is half of the id, and Canonicalize refuses a message where
		// the two disagree (see TestCanonicalizeRejects). This subtest proves
		// the server's assignment is inside the signed bytes — the central
		// design call of SIGN-1.
		{"message id + sequence (server-minted)", func(m *Message) { m.MessageID, m.Sequence = "bus-a-8", 8 }},
		{"sender", func(m *Message) { m.Sender = "bus-a.alice-2" }},
		{"recipient added", func(m *Message) { m.Recipients = append(append([]string(nil), m.Recipients...), "bus-a.dave-4") }},
		{"recipient replaced", func(m *Message) { m.Recipients = []string{"bus-a.bob-2", "bus-a.mallory-1"} }},
		{"recipient removed", func(m *Message) { m.Recipients = []string{"bus-a.bob-2"} }},
		{"recipient moved to another bus", func(m *Message) { m.Recipients = []string{"bus-a.bob-2", "bus-far.carol-1"} }},
		{"timestamp", func(m *Message) { m.TimestampUnixMilli++ }},
		{"body content", func(m *Message) { m.Body = []byte("payloaD") }},
		{"body length", func(m *Message) { m.Body = []byte("payload ") }},
		{"body emptied", func(m *Message) { m.Body = nil }},
	}

	for _, mut := range mutations {
		mut := mut
		t.Run(mut.field, func(t *testing.T) {
			m := base
			m.Recipients = append([]string(nil), base.Recipients...)
			m.Body = append([]byte(nil), base.Body...)
			mut.mutate(&m)

			got, err := Canonicalize(m)
			if err != nil {
				t.Fatalf("Canonicalize(mutated): %v", err)
			}
			if bytes.Equal(got, baseBytes) {
				t.Fatalf("changing %s did NOT change the canonical bytes — the signature would not protect it", mut.field)
			}
		})
	}
}

// TestCanonicalizeIsInjective checks the whole sample set pairwise: no two
// distinct messages share a byte string. Injectivity is what the uint32 length
// prefixes buy — without them an attacker could shift bytes across a field
// boundary and present a different logical message under a signature that
// still verifies.
func TestCanonicalizeIsInjective(t *testing.T) {
	seen := map[string]string{}
	for _, m := range sampleMessages() {
		b, err := Canonicalize(m.msg)
		if err != nil {
			t.Fatalf("%s: Canonicalize: %v", m.name, err)
		}
		key := string(b)
		if prev, dup := seen[key]; dup {
			t.Fatalf("%s and %s canonicalize to the same bytes", prev, m.name)
		}
		seen[key] = m.name
	}
}

// ---------------------------------------------------------------------------
// Fail-closed validation.
// ---------------------------------------------------------------------------

func TestCanonicalizeRejects(t *testing.T) {
	valid := Message{
		MessageID:          "bus-a-7",
		Sequence:           7,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-a.bob-2"},
		TimestampUnixMilli: 1_700_000_000_000,
		Body:               []byte("payload"),
	}

	cases := []struct {
		name   string
		mutate func(m *Message)
	}{
		{"empty message id", func(m *Message) { m.MessageID = "" }},
		{"message id with no sequence", func(m *Message) { m.MessageID = "bus-a" }},
		{"message id sequence 0", func(m *Message) { m.MessageID, m.Sequence = "bus-a-0", 0 }},
		{"message id sequence with a leading zero", func(m *Message) { m.MessageID = "bus-a-07" }},
		{"sequence 0", func(m *Message) { m.Sequence = 0 }},
		{"sequence disagrees with the message id", func(m *Message) { m.Sequence = 8 }},
		{"sender not fully qualified", func(m *Message) { m.Sender = "alice-1" }},
		{"sender has no minted suffix", func(m *Message) { m.Sender = "bus-a.alice" }},
		{"sender belongs to another bus", func(m *Message) { m.Sender = "bus-b.alice-1" }},
		{"no recipients", func(m *Message) { m.Recipients = nil }},
		{"empty recipient", func(m *Message) { m.Recipients = []string{""} }},
		{"recipient not fully qualified", func(m *Message) { m.Recipients = []string{"bob-2"} }},
		{"duplicate recipient", func(m *Message) { m.Recipients = []string{"bus-a.bob-2", "bus-a.bob-2"} }},
		{"timestamp unset", func(m *Message) { m.TimestampUnixMilli = 0 }},
		{"timestamp negative", func(m *Message) { m.TimestampUnixMilli = -1 }},
		{"body over the format limit", func(m *Message) { m.Body = make([]byte, MaxBodyLen+1) }},
		{"recipients over the format limit", func(m *Message) { m.Recipients = manyRecipients(MaxRecipients + 1) }},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m := valid
			c.mutate(&m)
			b, err := Canonicalize(m)
			if err == nil {
				t.Fatalf("Canonicalize accepted %s and produced %d bytes; it must fail closed", c.name, len(b))
			}
			if b != nil {
				t.Fatalf("Canonicalize returned %d bytes alongside an error; it must never emit partial bytes", len(b))
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error does not wrap ErrInvalid, so SIGN-6 cannot classify it: %v", err)
			}
		})
	}

	// The boundaries themselves are accepted: exactly MaxBodyLen and exactly
	// MaxRecipients are legal, one more of either is not. Without these the
	// limit cases above would also pass if the encoder rejected every body or
	// every recipient list.
	m := valid
	m.Body = make([]byte, MaxBodyLen)
	if _, err := Canonicalize(m); err != nil {
		t.Fatalf("a body of exactly MaxBodyLen must be accepted: %v", err)
	}

	m = valid
	m.Recipients = manyRecipients(MaxRecipients)
	if _, err := Canonicalize(m); err != nil {
		t.Fatalf("exactly MaxRecipients recipients must be accepted: %v", err)
	}
}

// manyRecipients builds n distinct, well-formed fully-qualified recipient ids.
func manyRecipients(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("bus-a.r%d-1", i)
	}
	return out
}

// ---------------------------------------------------------------------------
// The two consumers of the canonical bytes: ed25519 (unhashed) and sha256.
// ---------------------------------------------------------------------------

// TestCanonicalizeDigestIsSHA256OverTheCanonicalBytes pins the DUR-5 /
// CRYPTO-11 binding: the audit-log content hash is a hash of EXACTLY the bytes
// the signature covers. If DUR-5 ever hashes a different serialisation, this
// is the test that should have stopped it.
func TestCanonicalizeDigestIsSHA256OverTheCanonicalBytes(t *testing.T) {
	for _, m := range sampleMessages() {
		b, err := Canonicalize(m.msg)
		if err != nil {
			t.Fatalf("%s: Canonicalize: %v", m.name, err)
		}
		want := sha256.Sum256(b)
		got, err := CanonicalDigest(m.msg)
		if err != nil {
			t.Fatalf("%s: CanonicalDigest: %v", m.name, err)
		}
		if got != want {
			t.Fatalf("%s: CanonicalDigest is not sha256 over the canonical bytes", m.name)
		}
	}
}

// TestCanonicalizeIsSignedUnhashed proves the RATCHET-7 constraint holds in
// practice: a signature is made over the canonical bytes themselves, and a
// verifier handed the SHA-256 digest instead rejects it. Anybody tempted to
// "optimise" SIGN-2 by signing the digest will fail here.
func TestCanonicalizeIsSignedUnhashed(t *testing.T) {
	vf := loadVectors(t)
	priv, pub := vectorKey(t, vf)

	m := sampleMessages()[0].msg
	canonical, err := Canonicalize(m)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	digest, err := CanonicalDigest(m)
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}

	sig := ed25519.Sign(priv, canonical)
	if !ed25519.Verify(pub, canonical, sig) {
		t.Fatal("a signature over the canonical bytes did not verify over the canonical bytes")
	}
	if ed25519.Verify(pub, digest[:], sig) {
		t.Fatal("a signature over the canonical bytes verified over their SHA-256 digest; Ed25519 signs the message, never a pre-hash")
	}

	// And the reverse mistake: signing the digest yields something that does
	// not verify over the message a recipient will actually hold.
	digestSig := ed25519.Sign(priv, digest[:])
	if ed25519.Verify(pub, canonical, digestSig) {
		t.Fatal("a signature over the digest verified over the canonical bytes")
	}

	// One flipped bit anywhere in the canonical bytes must break the
	// signature — including inside the server-minted id and sequence fields.
	for _, i := range []int{0, 24, 30, len(canonical) - 1} {
		tampered := append([]byte(nil), canonical...)
		tampered[i] ^= 0x01
		if ed25519.Verify(pub, tampered, sig) {
			t.Fatalf("signature still verified after flipping a bit at offset %d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared sample set.
// ---------------------------------------------------------------------------

type namedMessage struct {
	name string
	msg  Message
}

// sampleMessages covers the shapes the format has to survive: one recipient and
// many, an empty body, a body that is not valid UTF-8, recipients on another
// bus (the relay case), and the extremes of the fixed-width fields.
func sampleMessages() []namedMessage {
	return []namedMessage{
		{"minimal", Message{
			MessageID: "bus-a-1", Sequence: 1,
			Sender: "bus-a.alice-1", Recipients: []string{"bus-a.bob-2"},
			TimestampUnixMilli: 1, Body: []byte("hi"),
		}},
		{"multi-recipient-unsorted", Message{
			MessageID: "bus-a-9", Sequence: 9,
			Sender:             "bus-a.alice-1",
			Recipients:         []string{"bus-a.carol-1", "bus-a.bob-2", "bus-a.alice-1"},
			TimestampUnixMilli: 1_700_000_000_000, Body: []byte("ordering must not matter"),
		}},
		{"empty-body", Message{
			MessageID: "bus-a-2", Sequence: 2,
			Sender: "bus-a.alice-1", Recipients: []string{"bus-a.bob-2"},
			TimestampUnixMilli: 1_700_000_000_001, Body: nil,
		}},
		{"binary-body", Message{
			MessageID: "bus-a-3", Sequence: 3,
			Sender: "bus-a.alice-1", Recipients: []string{"bus-a.bob-2"},
			TimestampUnixMilli: 1_700_000_000_002,
			Body:               []byte{0x00, 0xff, 0xfe, 0x0a, 0x0d, 0x00},
		}},
		{"cross-bus-recipients", Message{
			MessageID: "bus-origin-42", Sequence: 42,
			Sender:             "bus-origin.relay-agent-1",
			Recipients:         []string{"bus-far.dave-3", "bus-origin.eve-2"},
			TimestampUnixMilli: 1_700_000_000_003, Body: []byte("relayed"),
		}},
		{"extremes", Message{
			MessageID: "bus-a-18446744073709551615", Sequence: math.MaxUint64,
			Sender: "bus-a.alice-1", Recipients: []string{"bus-a.bob-2"},
			TimestampUnixMilli: math.MaxInt64, Body: []byte("last message this bus can mint"),
		}},
	}
}
