package signing

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

// TestVerifyRejectsSignatureOverDifferentBytes proves that Verify authenticates
// the exact canonical bytes it is given, not merely that sig is a valid
// Ed25519 signature made by pub. The signature below is cryptographically
// valid, but it covers a distinct byte string.
func TestVerifyRejectsSignatureOverDifferentBytes(t *testing.T) {
	pub, priv := genKey(t)
	m := validMessage()

	canonical, err := Canonicalize(m)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	different := append(append([]byte(nil), canonical...), 0x00)
	sig := ed25519.Sign(priv, different)
	if !ed25519.Verify(pub, different, sig) {
		t.Fatal("test setup produced a signature that is not valid over the different bytes")
	}

	if err := Verify(pub, m, sig); !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(signature over different bytes) = %v, want errors.Is(err, ErrVerify)", err)
	}
}

// TestVerifyRejectsAbsentSignature pins the absence-specific failure path.
// Absence must not be collapsed into a generic malformed-length error because
// stripping a signature and truncating one are distinct security events.
func TestVerifyRejectsAbsentSignature(t *testing.T) {
	pub, _ := genKey(t)
	m := validMessage()

	for _, tc := range []struct {
		name string
		sig  []byte
	}{
		{name: "nil", sig: nil},
		{name: "empty", sig: []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := safeVerify(t, pub, m, tc.sig); !errors.Is(err, ErrNoSignature) {
				t.Fatalf("Verify(absent signature) = %v, want errors.Is(err, ErrNoSignature)", err)
			}
		})
	}
}
