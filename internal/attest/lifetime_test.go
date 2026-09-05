package attest

import (
	"crypto/ed25519"
	"errors"
	"math"
	"testing"
	"time"
)

// TestVerifierSideLifetimeCeiling is RELAY-28's proof.
//
// Before this control, Verify trusted NotAfter entirely at the minter's
// discretion: an attestation minted valid until year 292278994 was accepted, and
// with revocation across a non-adjacent link unsolved, NotAfter is the ONLY bound
// on a compromised agent messaging key — so an absurd NotAfter made that key
// eternal. Verify now caps the remaining validity (NotAfter - now) at
// MaxAttestationLifetime regardless of the minted NotAfter, refusing the excess
// with ErrLifetimeExceeded.
//
// The measure is NotAfter - now from the verifier's own clock, so the boundary is
// expressed against a fixed now. Everything up to and including the ceiling
// verifies; anything past it is refused; and the year-292278994 case that names
// the defect is exercised directly, proving the time.Time.Sub saturation path
// refuses rather than wrapping.
func TestVerifierSideLifetimeCeiling(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	pins := []ed25519.PublicKey{origin.pub}
	want := Subject{FQAgentID: katAgentID, OriginBus: origin.id}

	// A fixed verification clock. The mint's issued-at is this same instant, so
	// remaining validity is exactly NotAfter - now and the boundary is exact.
	now := millis(katIssuedAt)
	issuedAt := now

	cases := []struct {
		name     string
		notAfter time.Time
		refused  bool
	}{
		{"well within the ceiling (1h window)", now.Add(time.Hour), false},
		{"one ms below the ceiling", now.Add(MaxAttestationLifetime - time.Millisecond), false},
		{"exactly at the ceiling", now.Add(MaxAttestationLifetime), false},
		{"one ms over the ceiling", now.Add(MaxAttestationLifetime + time.Millisecond), true},
		{"an hour over the ceiling", now.Add(MaxAttestationLifetime + time.Hour), true},
		{"the RELAY-28 far-future not-after (max int64 millis, ~year 292278994)", time.UnixMilli(math.MaxInt64), true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a, err := Sign(origin.priv, origin.id, katAgentID, msgPub, 1, issuedAt, tc.notAfter)
			if err != nil {
				t.Fatalf("Sign(not-after %s) = error %v", tc.notAfter.UTC(), err)
			}
			key, err := Verify(pins, a, want, now)
			switch {
			case tc.refused && !errors.Is(err, ErrLifetimeExceeded):
				t.Fatalf("Verify(%s) = %v, want ErrLifetimeExceeded", tc.name, err)
			case tc.refused && errors.Is(err, ErrExpired):
				t.Fatalf("Verify(%s) reported an over-ceiling attestation as expired; the two are distinct sentinels", tc.name)
			case tc.refused && errors.Is(err, ErrVerify):
				t.Fatalf("Verify(%s) reported the ceiling refusal as a signature failure; that sends an operator hunting a forgery that never happened", tc.name)
			case tc.refused && key != nil:
				t.Fatalf("Verify(%s) refused but returned a non-nil key; the ceiling must fail closed", tc.name)
			case !tc.refused && err != nil:
				t.Fatalf("Verify(%s) = %v, want acceptance (remaining validity is within the ceiling)", tc.name, err)
			case !tc.refused && key == nil:
				t.Fatalf("Verify(%s) accepted but returned a nil key", tc.name)
			}
		})
	}
}

// TestHonestMaximumWindowVerifiesUnderAdverseSkew proves the ceiling does not
// refuse the WORST honest case. A real origin bus mints the maximum window as
// issuedAt + relay.RetryHorizonCeiling, and relay.RetryHorizonCeiling IS
// idem.PeerOutageBudget, which is MaxAttestationLifetime - ClockSkewAllowance.
// Observed at the earliest a verifier possibly could — its own clock lagging the
// minter's by the full ClockSkewAllowance, so now = issuedAt - skew — the
// remaining validity is exactly MaxAttestationLifetime. Because the ceiling
// comparison is strict, this boundary case verifies; this is why the derivation
// adds one ClockSkewAllowance rather than none.
//
// The maximum honest window is expressed as MaxAttestationLifetime -
// ClockSkewAllowance (= idem.PeerOutageBudget) rather than by importing
// internal/relay: internal/relay imports internal/attest and a guard forbids the
// reverse (internal/relay/guards_test.go, the relay → attest → signing
// direction). idem.PeerOutageBudget is the same constant relay.RetryHorizonCeiling
// is defined as, and attest.go already imports it, so MaxAttestationLifetime
// tracks the minter's maximum by construction.
func TestHonestMaximumWindowVerifiesUnderAdverseSkew(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	pins := []ed25519.PublicKey{origin.pub}
	want := Subject{FQAgentID: katAgentID, OriginBus: origin.id}

	issuedAt := millis(katIssuedAt)
	honestMaxWindow := MaxAttestationLifetime - ClockSkewAllowance // == idem.PeerOutageBudget
	notAfter := issuedAt.Add(honestMaxWindow)

	a, err := Sign(origin.priv, origin.id, katAgentID, msgPub, 1, issuedAt, notAfter)
	if err != nil {
		t.Fatalf("Sign(honest max window) = %v", err)
	}

	// Verifier clock lags the minter by the full skew allowance: the earliest a
	// verifier could observe this attestation, and the largest NotAfter - now an
	// honest attestation can present (exactly MaxAttestationLifetime).
	now := issuedAt.Add(-ClockSkewAllowance)
	if _, err := Verify(pins, a, want, now); err != nil {
		t.Fatalf("Verify(honest max window, adverse skew) = %v, want acceptance; the ceiling must not refuse the worst honest case", err)
	}
}

// TestLifetimeCeilingIsCheckedAfterTheSignature. Like ErrExpired, the ceiling
// refusal must only ever name a genuinely bus-signed attestation. If it ran
// before the signature, a peer could present arbitrary unsigned garbage carrying
// a far-future NotAfter and be told "lifetime exceeded", so ErrLifetimeExceeded
// would stop meaning "a genuinely bus-signed attestation claims an absurd
// lifetime". A tampered attestation with a far-future NotAfter must therefore
// fail as ErrVerify, not ErrLifetimeExceeded.
func TestLifetimeCeilingIsCheckedAfterTheSignature(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	pins := []ed25519.PublicKey{origin.pub}
	want := Subject{FQAgentID: katAgentID, OriginBus: origin.id}

	now := millis(katIssuedAt)
	// A far-future NotAfter, well past the ceiling, but the signature is corrupted
	// so it never legitimately verifies.
	a, err := Sign(origin.priv, origin.id, katAgentID, msgPub, 1, now, now.Add(MaxAttestationLifetime+time.Hour))
	if err != nil {
		t.Fatalf("Sign = %v", err)
	}
	a.Signature[0] ^= 0xff

	_, err = Verify(pins, a, want, now)
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(tampered, far-future not-after) = %v, want ErrVerify; the ceiling must not pre-empt the signature check", err)
	}
	if errors.Is(err, ErrLifetimeExceeded) {
		t.Fatalf("Verify reported ErrLifetimeExceeded for an attestation that never verified; the ceiling must only name genuinely bus-signed attestations")
	}
}
