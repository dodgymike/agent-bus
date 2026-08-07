package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Shared helpers — same style as canonical_test.go's sampleMessages/valid.
// ---------------------------------------------------------------------------

// validMessage returns a Message that satisfies canonical.go's validate():
// MessageID "<busID>-<seq>", Sequence equal to that seq, Sender
// "<busID>.<name>-<n>" on the same bus, at least one recipient, and a
// positive timestamp.
func validMessage() Message {
	return Message{
		MessageID:          "bus-a-7",
		Sequence:           7,
		Sender:             "bus-a.alice-1",
		Recipients:         []string{"bus-a.bob-2"},
		TimestampUnixMilli: 1_700_000_000_000,
		Body:               []byte("payload"),
	}
}

// genKey generates a fresh, genuine Ed25519 key pair.
func genKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// safeVerify calls Verify with a recover wrapper so a regression that
// reintroduces the crypto/ed25519 panic trap is reported as a test failure
// naming the panic, not as a crashed test binary.
func safeVerify(t *testing.T, pub ed25519.PublicKey, m Message, sig []byte) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Verify panicked: %v", r)
		}
	}()
	return Verify(pub, m, sig)
}

// safeSign calls Sign with the same panic-to-failure wrapper as safeVerify,
// for the private-key side of the same trap (ed25519.Sign panics on a
// wrong-size private key exactly as ed25519.Verify panics on a wrong-size
// public key).
func safeSign(t *testing.T, priv ed25519.PrivateKey, m Message) (sig []byte, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Sign panicked: %v", r)
		}
	}()
	return Sign(priv, m)
}

// ---------------------------------------------------------------------------
// 1. The happy path.
// ---------------------------------------------------------------------------

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()

	sig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != SignatureSize {
		t.Fatalf("signature is %d bytes, want exactly %d", len(sig), SignatureSize)
	}
	if err := Verify(pub, m, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// THE ANCHOR TO THE NORMATIVE FORMAT, and the reason this is not just a
	// round trip. Everything above round-trips through this package's OWN
	// Sign/Verify pair, so an implementation that quietly signed something
	// other than Canonicalize's output — a byte appended, a field dropped, a
	// digest substituted — would still agree with ITSELF and stay green. That
	// is precisely the silent failure invariant 9 and PROTOCOL.md §8.1 exist to
	// prevent: the signature would verify and would simply stop covering what
	// §8 says it covers, and every cross-implementation verifier would break.
	//
	// So the signature is pinned to raw crypto/ed25519 over the canonical bytes
	// in BOTH directions: our Sign must produce exactly what stdlib produces
	// over Canonicalize(m), and stdlib must accept what our Sign produced.
	canonical, err := Canonicalize(m)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if want := ed25519.Sign(priv, canonical); !bytes.Equal(sig, want) {
		t.Fatalf("Sign did not sign Canonicalize(m):\n got %x\nwant %x\nSign must pass the canonical bytes to ed25519.Sign UNHASHED and unmodified (PROTOCOL.md §8)", sig, want)
	}
	if !ed25519.Verify(pub, canonical, sig) {
		t.Fatal("raw ed25519.Verify rejected a signature our Sign produced over the canonical bytes; the two have drifted apart")
	}
}

// ---------------------------------------------------------------------------
// 2-6. The tamper matrix: every covered field, plus the key itself, must
// break verification. Each is its own named test asserting the DISTINCT
// ErrVerify sentinel, per SIGN-5.
// ---------------------------------------------------------------------------

func TestVerifyRejectsTamperedBody(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()

	sig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tampered := m
	tampered.Body = append([]byte(nil), m.Body...)
	tampered.Body[0] ^= 0x01

	if err := Verify(pub, tampered, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(tampered body) = %v, want errors.Is(err, ErrVerify)", err)
	}
}

// TestVerifyRejectsSwappedSender proves the sender id is INSIDE the signed
// bytes: signing as alice and re-labelling the message as bob — a DIFFERENT
// valid agent on the SAME bus, so canonicalization still succeeds and the
// only thing that can fail is the signature — must not verify.
func TestVerifyRejectsSwappedSender(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()
	m.Sender = "bus-a.alice-1"
	// bob-2 is already the sole recipient; make the swapped sender a THIRD,
	// distinct agent on the same bus so the message stays well-formed
	// (a sender may also be a recipient, but keeping them distinct avoids
	// any ambiguity about what changed).
	m.Recipients = []string{"bus-a.carol-3"}

	sig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	relabelled := m
	relabelled.Sender = "bus-a.bob-2"

	if err := Verify(pub, relabelled, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(swapped sender) = %v, want errors.Is(err, ErrVerify)", err)
	}
}

// TestVerifyRejectsTamperedMessageIDAndSequence proves the server-minted
// ordering fields are inside the signed bytes. MessageID and Sequence must
// move TOGETHER to stay self-consistent (Canonicalize refuses a message
// where the two disagree), so this re-labels both to a different, still
// internally-consistent pair.
func TestVerifyRejectsTamperedMessageIDAndSequence(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()

	sig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	relabelled := m
	relabelled.MessageID, relabelled.Sequence = "bus-a-8", 8

	if err := Verify(pub, relabelled, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(re-labelled message id + sequence) = %v, want errors.Is(err, ErrVerify)", err)
	}
}

func TestVerifyRejectsTamperedRecipients(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()

	sig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	tampered := m
	tampered.Recipients = []string{"bus-a.mallory-9"}

	if err := Verify(pub, tampered, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(tampered recipients) = %v, want errors.Is(err, ErrVerify)", err)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv := genKey(t)
	wrongPub, _ := genKey(t)
	m := validMessage()

	sig, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if err := Verify(wrongPub, m, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(wrong public key) = %v, want errors.Is(err, ErrVerify)", err)
	}
}

// ---------------------------------------------------------------------------
// 7. Signature shape: absent vs. mangled are DISTINCT sentinels.
// ---------------------------------------------------------------------------

func TestVerifyRejectsTruncatedSignature(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()

	valid, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	cases := []struct {
		name string
		sig  []byte
		want error
	}{
		{"nil", nil, ErrNoSignature},
		{"0 bytes", []byte{}, ErrNoSignature},
		{"63 bytes", valid[:63], ErrSignatureLength},
		{"65 bytes", append(append([]byte(nil), valid...), 0x00), ErrSignatureLength},
		{"1 byte", valid[:1], ErrSignatureLength},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := safeVerify(t, pub, m, c.sig)
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify(sig len %d) = %v, want errors.Is(err, %v)", len(c.sig), err, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 8. THE critical one: SIGN-2's named acceptance criterion. crypto/ed25519
// PANICS on a malformed public key; Verify must never let that reach the
// caller.
// ---------------------------------------------------------------------------

func TestVerifyDoesNotPanicOnMalformedPublicKey(t *testing.T) {
	m := validMessage()
	// A well-formed-length signature (its content does not matter — the
	// public key check must fire before the signature is ever examined
	// cryptographically).
	validLenSig := make([]byte, SignatureSize)

	cases := []struct {
		name string
		pub  ed25519.PublicKey
	}{
		{"nil", nil},
		{"empty", ed25519.PublicKey{}},
		{"31 bytes", make(ed25519.PublicKey, 31)},
		{"33 bytes", make(ed25519.PublicKey, 33)},
		{"64 bytes (a private key's length)", make(ed25519.PublicKey, 64)},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := safeVerify(t, c.pub, m, validLenSig)
			if !errors.Is(err, ErrPublicKeyLength) {
				t.Fatalf("Verify(pub len %d) = %v, want errors.Is(err, ErrPublicKeyLength)", len(c.pub), err)
			}
		})
	}

	// Ordering matters: a malformed public key together with a malformed
	// signature must STILL report ErrPublicKeyLength, because the key is
	// checked FIRST — that is precisely what averts the panic (see Verify's
	// doc comment on the load-bearing check order).
	t.Run("malformed key AND malformed signature", func(t *testing.T) {
		malformedPub := make(ed25519.PublicKey, 10)
		malformedSig := make([]byte, 5)
		err := safeVerify(t, malformedPub, m, malformedSig)
		if !errors.Is(err, ErrPublicKeyLength) {
			t.Fatalf("Verify(malformed key + malformed sig) = %v, want errors.Is(err, ErrPublicKeyLength) — the key must be checked before the signature", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 9. The private-key mirror of case 8: ed25519.Sign panics on a wrong-size
// private key.
// ---------------------------------------------------------------------------

func TestSignRejectsMalformedPrivateKey(t *testing.T) {
	m := validMessage()

	cases := []struct {
		name string
		priv ed25519.PrivateKey
	}{
		{"nil", nil},
		{"empty", ed25519.PrivateKey{}},
		{"63 bytes", make(ed25519.PrivateKey, 63)},
		{"65 bytes", make(ed25519.PrivateKey, 65)},
		{"32 bytes (a public key's length)", make(ed25519.PrivateKey, 32)},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sig, err := safeSign(t, c.priv, m)
			if !errors.Is(err, ErrPrivateKeyLength) {
				t.Fatalf("Sign(priv len %d) = %v, want errors.Is(err, ErrPrivateKeyLength)", len(c.priv), err)
			}
			if sig != nil {
				t.Fatalf("Sign(priv len %d) returned a non-nil signature alongside an error", len(c.priv))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 10. Sign/Verify must fail closed on a Message validate() itself refuses,
// and must say ErrInvalid, not ErrVerify.
// ---------------------------------------------------------------------------

func TestSignRejectsUncanonicalizableMessage(t *testing.T) {
	pub, priv := genKey(t)

	cases := []struct {
		name   string
		mutate func(m *Message)
	}{
		{"sender on a different bus from the message id", func(m *Message) { m.Sender = "bus-b.alice-1" }},
		{"sequence zero", func(m *Message) { m.MessageID, m.Sequence = "bus-a-0", 0 }},
		{"empty recipients", func(m *Message) { m.Recipients = nil }},
		{"non-positive timestamp", func(m *Message) { m.TimestampUnixMilli = 0 }},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m := validMessage()
			c.mutate(&m)

			sig, err := Sign(priv, m)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Sign(%s) = %v, want errors.Is(err, ErrInvalid)", c.name, err)
			}
			if sig != nil {
				t.Fatalf("Sign(%s) returned a non-nil signature alongside an error", c.name)
			}

			// Verify must reach the SAME classification, not ErrVerify: a
			// message that will not canonicalize has no signable bytes, so
			// there is nothing for ed25519 to have an opinion about. Feed it
			// a well-formed-length (but otherwise meaningless) signature so
			// the earlier length checks do not short-circuit before
			// canonicalization is reached.
			verifyErr := Verify(pub, m, make([]byte, SignatureSize))
			if !errors.Is(verifyErr, ErrInvalid) {
				t.Fatalf("Verify(%s) = %v, want errors.Is(err, ErrInvalid)", c.name, verifyErr)
			}
			if errors.Is(verifyErr, ErrVerify) {
				t.Fatalf("Verify(%s) matched ErrVerify; an uncanonicalizable message must be classified ErrInvalid, not a failed cryptographic check", c.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 11. The standalone bus-side shape checks.
// ---------------------------------------------------------------------------

func TestValidateSignature(t *testing.T) {
	cases := []struct {
		name    string
		sig     []byte
		wantErr error
	}{
		{"nil", nil, ErrNoSignature},
		{"empty", []byte{}, ErrNoSignature},
		{"63 bytes", make([]byte, 63), ErrSignatureLength},
		{"65 bytes", make([]byte, 65), ErrSignatureLength},
		{"exactly SignatureSize", make([]byte, SignatureSize), nil},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSignature(c.sig)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateSignature(%d bytes) = %v, want nil", len(c.sig), err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ValidateSignature(%d bytes) = %v, want errors.Is(err, %v)", len(c.sig), err, c.wantErr)
			}
		})
	}
}

func TestValidatePublicKey(t *testing.T) {
	cases := []struct {
		name    string
		pub     ed25519.PublicKey
		wantErr error
	}{
		{"nil", nil, ErrPublicKeyLength},
		{"empty", ed25519.PublicKey{}, ErrPublicKeyLength},
		{"31 bytes", make(ed25519.PublicKey, 31), ErrPublicKeyLength},
		{"33 bytes", make(ed25519.PublicKey, 33), ErrPublicKeyLength},
		{"exactly PublicKeySize", make(ed25519.PublicKey, PublicKeySize), nil},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePublicKey(c.pub)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidatePublicKey(%d bytes) = %v, want nil", len(c.pub), err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ValidatePublicKey(%d bytes) = %v, want errors.Is(err, %v)", len(c.pub), err, c.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 12. Determinism — guards against anyone "improving" this with randomness.
// ---------------------------------------------------------------------------

func TestSignIsDeterministic(t *testing.T) {
	_, priv := genKey(t)
	m := validMessage()

	sig1, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign (1st): %v", err)
	}
	sig2, err := Sign(priv, m)
	if err != nil {
		t.Fatalf("Sign (2nd): %v", err)
	}
	if !bytes.Equal(sig1, sig2) {
		t.Fatalf("Sign is not deterministic: signing the same message twice under the same key produced different signatures\n1st: %x\n2nd: %x", sig1, sig2)
	}
}

// ---------------------------------------------------------------------------
// 13. A malformed-length private key must never leak its bytes through an
// error string.
// ---------------------------------------------------------------------------

func TestSignDoesNotLeakPrivateKeyBytes(t *testing.T) {
	m := validMessage()

	// Distinctive, recognisable byte patterns per wrong length so a leak is
	// unambiguous if the error text ever contains them.
	cases := []struct {
		name string
		priv ed25519.PrivateKey
	}{
		{"63 bytes", bytes.Repeat([]byte{0xAB}, 63)},
		{"65 bytes", bytes.Repeat([]byte{0xCD}, 65)},
		{"32 bytes", bytes.Repeat([]byte{0xEF}, 32)},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := safeSign(t, c.priv, m)
			if err == nil {
				t.Fatalf("Sign(priv len %d) unexpectedly succeeded", len(c.priv))
			}
			errText := err.Error()

			rawKey := string(c.priv)
			if strings.Contains(errText, rawKey) {
				t.Fatalf("Sign error text contains the raw private key bytes: %q", errText)
			}

			hexKey := hex.EncodeToString(c.priv)
			if strings.Contains(errText, hexKey) {
				t.Fatalf("Sign error text contains the hex-encoded private key: %q", errText)
			}
		})
	}
}
