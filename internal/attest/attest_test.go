package attest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------------

// verifyNow is a fixed verification clock inside the worked example's validity
// window. Tests pass a clock explicitly; nothing here reads the wall clock, so
// no test can go red in a year's time.
var verifyNow = millis(katIssuedAt + 60_000)

func millis(ms int64) time.Time { return time.UnixMilli(ms) }

func mustGenerate(t *testing.T, what string) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a %s key: %v", what, err)
	}
	return pub, priv
}

// bus is a test origin bus: an id and a bus signing key.
type bus struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newBus(t *testing.T, id string) bus {
	t.Helper()
	pub, priv := mustGenerate(t, "bus signing")
	return bus{id: id, pub: pub, priv: priv}
}

// attest mints an attestation for one of this bus's agents, over the worked
// example's validity window.
func (b bus) attest(t *testing.T, agentID string, msgPub ed25519.PublicKey) Attestation {
	t.Helper()
	a, err := Sign(b.priv, b.id, agentID, msgPub, 1, millis(katIssuedAt), millis(katNotAfter))
	if err != nil {
		t.Fatalf("Sign(%s, %s) = error %v", b.id, agentID, err)
	}
	return a
}

// ---------------------------------------------------------------------------
// The proof_cmd test: an attestation is worth something ONLY against the pin.
// ---------------------------------------------------------------------------

// TestAttestationVerifiesOnlyAgainstPinnedBusKey is RELAY-14's proof.
//
// It asserts the whole point of the design: a bus-signed binding is accepted
// when — and only when — it verifies against a signing key the OPERATOR pinned
// out of band for the origin bus. Not a key that arrived with the message, not
// a key we have seen before, not "some bus we federate with signed it".
//
// The intermediate-bus case is the one the whole federation topology exists
// for: B is the internet-facing machine and is a COURIER, NOT A VOUCHER. B
// holds a perfectly good signing key of its own, and C pins it — for B. B
// re-attesting A's agent under B's key must be refused at C, or the entire
// A -> B -> C trust model collapses to "trust whoever handed it to you".
func TestAttestationVerifiesOnlyAgainstPinnedBusKey(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")   // A
	relayer := newBus(t, "gateway") // B, the internet-facing intermediate
	other := newBus(t, "stranger")  // a bus we do not pin at all

	msgPub, _ := mustGenerate(t, "agent messaging")
	genuine := origin.attest(t, katAgentID, msgPub)
	want := Subject{FQAgentID: katAgentID, OriginBus: origin.id}

	t.Run("verifies against the pinned origin key", func(t *testing.T) {
		got, err := Verify([]ed25519.PublicKey{origin.pub}, genuine, want, verifyNow)
		if err != nil {
			t.Fatalf("Verify(genuine attestation, correct pin) = error %v, want the attested key", err)
		}
		if !bytes.Equal(got, msgPub) {
			t.Fatal("Verify returned a key that is not the attested messaging key")
		}
	})

	t.Run("returns a COPY, not an alias of the blob", func(t *testing.T) {
		got, err := Verify([]ed25519.PublicKey{origin.pub}, genuine, want, verifyNow)
		if err != nil {
			t.Fatalf("Verify = error %v", err)
		}
		got[0] ^= 0xff
		if genuine.MessagingPublicKey[0] == got[0] {
			t.Fatal("Verify returned a slice aliasing the attestation; a caller mutating it would change the checked blob (time-of-check/time-of-use)")
		}
	})

	t.Run("refuses a DIFFERENT bus's pin", func(t *testing.T) {
		_, err := Verify([]ed25519.PublicKey{relayer.pub}, genuine, want, verifyNow)
		if !errors.Is(err, ErrVerify) {
			t.Fatalf("Verify(genuine attestation, wrong bus's pin) = %v, want ErrVerify", err)
		}
	})

	t.Run("refuses an INTERMEDIATE bus's re-attestation", func(t *testing.T) {
		// B forwards VERBATIM and may NEVER re-attest. Here B tries: it mints
		// its own attestation for A's agent, with A's real messaging key, and
		// C pins B's signing key legitimately (they are peers).
		reAttested, err := Sign(relayer.priv, relayer.id, katAgentID, msgPub, 1, millis(katIssuedAt), millis(katNotAfter))
		if err == nil {
			t.Fatalf("Sign let bus %q attest an agent of bus %q; a bus may only speak for its own agents", relayer.id, origin.id)
		}
		if !errors.Is(err, ErrOriginBusMismatch) {
			t.Fatalf("Sign(foreign namespace) = %v, want ErrOriginBusMismatch", err)
		}
		_ = reAttested

		// And even if B builds the blob by hand, bypassing Sign, C refuses it:
		// the pins C looks up are A's, and B's signature does not verify under
		// them.
		forged := genuine
		canonical, cerr := Canonicalize(forged)
		if cerr != nil {
			t.Fatalf("test setup: %v", cerr)
		}
		forged.Signature = ed25519.Sign(relayer.priv, canonical)
		if _, err := Verify([]ed25519.PublicKey{origin.pub}, forged, want, verifyNow); !errors.Is(err, ErrVerify) {
			t.Fatalf("Verify(hand-built re-attestation by the intermediate) = %v, want ErrVerify", err)
		}
	})

	t.Run("refuses an empty pin set — there is no trust-on-first-use", func(t *testing.T) {
		for _, pins := range [][]ed25519.PublicKey{nil, {}} {
			_, err := Verify(pins, genuine, want, verifyNow)
			if !errors.Is(err, ErrUnpinned) {
				t.Fatalf("Verify(no pins) = %v, want ErrUnpinned", err)
			}
		}
	})

	t.Run("refuses an unpinned third bus", func(t *testing.T) {
		strangerAttested := other.attest(t, "stranger.writer-7", msgPub)
		strangerWant := Subject{FQAgentID: "stranger.writer-7", OriginBus: other.id}
		_, err := Verify([]ed25519.PublicKey{origin.pub, relayer.pub}, strangerAttested, strangerWant, verifyNow)
		if !errors.Is(err, ErrVerify) {
			t.Fatalf("Verify(attestation from an unpinned bus) = %v, want ErrVerify", err)
		}
	})
}

// TestPinsAreAListForRolloverOnly pins the reason PinnedBusSigningKeys is a
// list: a signing-key ROLLOVER window, mirroring the two-certificate TLS
// rollover, so a federation-wide rotation need not be simultaneous.
//
// Both the outgoing and the incoming key are pinned for the overlap, and an
// attestation minted under either must verify — in either pin order.
func TestPinsAreAListForRolloverOnly(t *testing.T) {
	t.Parallel()

	outgoing := newBus(t, "laptop")
	incoming := bus{id: "laptop"}
	incoming.pub, incoming.priv = mustGenerate(t, "rolled bus signing")

	msgPub, _ := mustGenerate(t, "agent messaging")
	want := Subject{FQAgentID: katAgentID, OriginBus: "laptop"}

	window := []ed25519.PublicKey{outgoing.pub, incoming.pub}
	reversed := []ed25519.PublicKey{incoming.pub, outgoing.pub}

	for _, minter := range []bus{outgoing, incoming} {
		a := minter.attest(t, katAgentID, msgPub)
		for name, pins := range map[string][]ed25519.PublicKey{"outgoing first": window, "incoming first": reversed} {
			if _, err := Verify(pins, a, want, verifyNow); err != nil {
				t.Fatalf("during a rollover window (%s), an attestation minted under one of the two pinned keys was refused: %v", name, err)
			}
		}
	}
}

// TestMalformedPinIsRefusedNotSkipped: a malformed pin means the PINNING STORE
// is wrong, not that this peer did anything. Verifying against the well-formed
// remainder would silently check against LESS than the operator believes is
// pinned.
func TestMalformedPinIsRefusedNotSkipped(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)
	want := Subject{FQAgentID: katAgentID, OriginBus: origin.id}

	cases := map[string][]ed25519.PublicKey{
		"short pin alongside the good one": {make([]byte, 31), origin.pub},
		"good pin alongside a short one":   {origin.pub, make([]byte, 31)},
		"nil pin":                          {nil, origin.pub},
		"over-long pin":                    {origin.pub, make([]byte, 33)},
	}
	for name, pins := range cases {
		pins := pins
		t.Run(name, func(t *testing.T) {
			// The attestation is GENUINE and one pin is CORRECT; the refusal is
			// about the store, not about the peer.
			if _, err := Verify(pins, a, want, verifyNow); !errors.Is(err, ErrUnpinned) {
				t.Fatalf("Verify(%s) = %v, want ErrUnpinned", name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BINDING CHECK 1 — the load-bearing one. THE MANDATORY NEGATIVE TEST.
// ---------------------------------------------------------------------------

// TestAttestationDoesNotAuthoriseADifferentAgentOnTheSameBus is the negative
// test FEDERATION_TRUST_DEEPDIVE.md §4.2 binding check 1 mandates.
//
// THE CHECK IT PINS IS LOAD-BEARING, NOT DEFENCE IN DEPTH. An early draft of
// the design said the opposite; the security gate caught it. Without
// `a.AgentID == want.FQAgentID`, ANYONE HOLDING ONE A-AGENT'S MESSAGING PRIVATE
// KEY CAN SIGN AS EVERY OTHER AGENT ON A, and every remaining check still
// passes:
//
//   - the envelope's sender and origin bus agree (both "laptop"),
//   - the message id's bus and the sender's bus agree (both "laptop"),
//   - the attached attestation is GENUINE and UNMODIFIED — attestations travel
//     in the clear on every relayed message, so observing one yields one,
//   - it verifies against the pin, because it really was minted by A,
//   - and the message signature verifies, because the key this function
//     RETURNED is the very key that signed it.
//
// The message signature does not save you: it covers the sender, but the KEY IT
// IS CHECKED AGAINST is what this function selects. This test is the only thing
// standing between that attack and a receiving bus attributing the message to
// the wrong agent, undetectably.
//
// VERIFIED LOAD-BEARING (RELAY-14, 2026-08-08): with the AgentID comparison
// removed from Verify in a scratch copy of the tree, this test FAILS —
// "Verify RETURNED A KEY for laptop.reader-9 on the strength of an attestation
// for laptop.writer-7". It is not a test that passes either way.
func TestAttestationDoesNotAuthoriseADifferentAgentOnTheSameBus(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")

	// alice is the compromised agent: the attacker holds her messaging key.
	const alice = "laptop.writer-7"
	alicePub, alicePriv := mustGenerate(t, "alice messaging")

	// bob is the victim: a different agent on the SAME bus. The attacker does
	// not hold bob's key and never needs to.
	const bob = "laptop.reader-9"
	bobPub, _ := mustGenerate(t, "bob messaging")

	// The genuine, unmodified attestation for alice, minted by the origin bus.
	aliceAttestation := origin.attest(t, alice, alicePub)
	pins := []ed25519.PublicKey{origin.pub}

	// Sanity: it is genuine, and it authorises ALICE.
	got, err := Verify(pins, aliceAttestation, Subject{FQAgentID: alice, OriginBus: origin.id}, verifyNow)
	if err != nil {
		t.Fatalf("test setup: alice's own attestation must verify for alice: %v", err)
	}
	if !bytes.Equal(got, alicePub) {
		t.Fatal("test setup: alice's attestation returned a key that is not alice's")
	}

	// THE ATTACK: present alice's genuine attestation while claiming to be bob.
	key, err := Verify(pins, aliceAttestation, Subject{FQAgentID: bob, OriginBus: origin.id}, verifyNow)
	if err == nil {
		t.Fatalf("Verify RETURNED A KEY for %s on the strength of an attestation for %s; one compromised agent messaging key now impersonates every agent on bus %q", bob, alice, origin.id)
	}
	if key != nil {
		t.Fatalf("Verify returned a %d-byte key alongside an error; it must return nil on every refusal", len(key))
	}
	if !errors.Is(err, ErrAgentIDMismatch) {
		t.Fatalf("Verify(alice's attestation, attributed to bob) = %v, want ErrAgentIDMismatch", err)
	}

	// And the consequence the check prevents, spelled out: the key that would
	// have been returned is ALICE's, so a message the attacker signed with
	// alice's key, naming bob as its sender, would have verified.
	if bytes.Equal(alicePub, bobPub) {
		t.Fatal("test setup: alice and bob must hold different messaging keys")
	}
	canonical, cerr := Canonicalize(aliceAttestation)
	if cerr != nil {
		t.Fatalf("test setup: %v", cerr)
	}
	forgedAsBob := ed25519.Sign(alicePriv, canonical)
	if !ed25519.Verify(alicePub, canonical, forgedAsBob) {
		t.Fatal("test setup: alice's key must sign and verify normally")
	}
}

// TestAttestationDoesNotCrossBusBoundaries pins BINDING CHECK 2: an attestation
// whose subject lives on a different bus than the one whose pins were looked up
// is refused, even when it is genuine and even when we pin that other bus.
//
// This is what ties the pin set to the subject. The caller looks pins up BY
// origin bus, so without it a peer could present a validly-signed attestation
// minted by a different bus we also federate with.
func TestAttestationDoesNotCrossBusBoundaries(t *testing.T) {
	t.Parallel()

	a := newBus(t, "laptop")
	c := newBus(t, "workstation")
	msgPub, _ := mustGenerate(t, "agent messaging")

	// A genuine attestation minted by "workstation" for its own agent.
	genuine := c.attest(t, "workstation.writer-7", msgPub)

	// Presented as though it came from "laptop", with BOTH buses pinned — the
	// signature verifies under one of the pins, so only check 2 stops it.
	pins := []ed25519.PublicKey{a.pub, c.pub}
	_, err := Verify(pins, genuine, Subject{FQAgentID: "workstation.writer-7", OriginBus: a.id}, verifyNow)
	if !errors.Is(err, ErrOriginBusMismatch) {
		t.Fatalf("Verify(attestation for a workstation agent, presented as origin bus %q) = %v, want ErrOriginBusMismatch", a.id, err)
	}
}

// TestTamperedAttestationDoesNotVerify: every covered field is covered. A field
// changed after minting breaks the signature, which is what makes "forwarded
// VERBATIM" enforceable rather than a request.
func TestTamperedAttestationDoesNotVerify(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	attackerPub, _ := mustGenerate(t, "attacker messaging")
	pins := []ed25519.PublicKey{origin.pub}

	cases := map[string]func(a Attestation) (Attestation, Subject){
		// The substitution an intermediate would actually attempt: keep the
		// subject, swap in a key it controls.
		"messaging key swapped": func(a Attestation) (Attestation, Subject) {
			a.MessagingPublicKey = attackerPub
			return a, Subject{FQAgentID: a.AgentID, OriginBus: origin.id}
		},
		"epoch bumped": func(a Attestation) (Attestation, Subject) {
			a.KeyEpoch++
			return a, Subject{FQAgentID: a.AgentID, OriginBus: origin.id}
		},
		"issued-at moved": func(a Attestation) (Attestation, Subject) {
			a.IssuedAtUnixMilli -= 1000
			return a, Subject{FQAgentID: a.AgentID, OriginBus: origin.id}
		},
		// The lifetime extension: an intermediate cannot re-mint, so its only
		// route to keeping a stale attestation alive is to push NotAfter out.
		"expiry extended": func(a Attestation) (Attestation, Subject) {
			a.NotAfterUnixMilli += 365 * 24 * 60 * 60 * 1000
			return a, Subject{FQAgentID: a.AgentID, OriginBus: origin.id}
		},
		"signature flipped": func(a Attestation) (Attestation, Subject) {
			sig := append([]byte{}, a.Signature...)
			sig[0] ^= 0x01
			a.Signature = sig
			return a, Subject{FQAgentID: a.AgentID, OriginBus: origin.id}
		},
	}

	for name, mangle := range cases {
		mangle := mangle
		t.Run(name, func(t *testing.T) {
			a, want := mangle(origin.attest(t, katAgentID, msgPub))
			if _, err := Verify(pins, a, want, verifyNow); !errors.Is(err, ErrVerify) {
				t.Fatalf("Verify(%s) = %v, want ErrVerify", name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// BINDING CHECK 4 — expiry is a MUST.
// ---------------------------------------------------------------------------

// TestExpiryIsEnforcedWithItsOwnSentinel. Revocation across a non-adjacent link
// is UNSOLVED, so NotAfter is the ONLY bound on a compromised agent messaging
// key. An implementer who treats it as advisory makes every attestation
// eternal.
//
// The sentinel is separate on purpose: an expired attestation is very often an
// honest message that sat in an intermediate's queue across a partition,
// because an intermediate forwards verbatim and cannot re-mint. Reporting it as
// a signature failure sends an operator hunting an attacker who does not exist.
func TestExpiryIsEnforcedWithItsOwnSentinel(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)
	pins := []ed25519.PublicKey{origin.pub}
	want := Subject{FQAgentID: katAgentID, OriginBus: origin.id}

	notAfter := millis(katNotAfter)

	cases := []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"inside the window", millis(katIssuedAt + 1), false},
		{"exactly at not-after", notAfter, false},
		{"just past, inside the skew allowance", notAfter.Add(ClockSkewAllowance - time.Millisecond), false},
		{"exactly at the skew boundary", notAfter.Add(ClockSkewAllowance), false},
		{"one millisecond past the skew allowance", notAfter.Add(ClockSkewAllowance + time.Millisecond), true},
		{"a year late", notAfter.Add(365 * 24 * time.Hour), true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(pins, a, want, tc.now)
			switch {
			case tc.expired && !errors.Is(err, ErrExpired):
				t.Fatalf("Verify(%s) = %v, want ErrExpired", tc.name, err)
			case tc.expired && errors.Is(err, ErrVerify):
				t.Fatalf("Verify(%s) reported expiry as a signature failure; that sends an operator hunting a forgery that never happened", tc.name)
			case !tc.expired && err != nil:
				t.Fatalf("Verify(%s) = %v, want acceptance", tc.name, err)
			}
		})
	}
}

// TestExpiryIsCheckedAfterTheSignature. If expiry came first, arbitrary
// unsigned garbage carrying an old timestamp would be reported as "expired",
// and ErrExpired would stop meaning "a genuinely bus-signed attestation grew
// old" — which is the entire reason it is a separate sentinel.
func TestExpiryIsCheckedAfterTheSignature(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")

	a := origin.attest(t, katAgentID, msgPub)
	a.Signature = bytes.Repeat([]byte{0xab}, ed25519.SignatureSize) // not a signature

	longAfter := millis(katNotAfter).Add(48 * time.Hour)
	_, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: katAgentID, OriginBus: origin.id}, longAfter)
	if errors.Is(err, ErrExpired) {
		t.Fatal("an unsigned blob with an old timestamp was reported as EXPIRED; expiry must be checked after the signature so ErrExpired only ever names a genuinely bus-signed attestation")
	}
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("Verify(garbage signature) = %v, want ErrVerify", err)
	}
}

// TestVerifyRefusesAZeroClock: a caller that forgot to supply a clock would
// otherwise get an attestation with an unbounded lifetime and no indication
// anything was wrong. Expiry is a MUST, so a missing clock is a refusal.
//
// It is ErrNoClock and NOT ErrInvalid: this is OUR wiring bug, and ErrInvalid
// is the sentinel a caller answers to a peer as a 400. Reporting our own
// missing argument to a remote bus as its malformed request is the
// misattribution this package refuses everywhere else.
func TestVerifyRefusesAZeroClock(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)

	_, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: katAgentID, OriginBus: origin.id}, time.Time{})
	if !errors.Is(err, ErrNoClock) {
		t.Fatalf("Verify(zero clock) = %v, want ErrNoClock", err)
	}
	if errors.Is(err, ErrInvalid) {
		t.Fatal("a missing clock is OUR fault, and ErrInvalid is what a caller answers to a PEER as a 400")
	}
}

// TestVerifyBoundsBothSidesOfTheSubjectComparison. ids.ParseAgentID bounds the
// ATTESTATION's agent id before anything quotes it; without a matching bound on
// the caller's half, the discipline would hold on one side of a two-sided
// comparison only, and %q expands a control byte to four characters.
func TestVerifyBoundsBothSidesOfTheSubjectComparison(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)

	huge := strings.Repeat("\x00", 64<<10)
	_, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: huge, OriginBus: origin.id}, verifyNow)
	if err == nil {
		t.Fatal("Verify(64 KiB subject) = nil error, want a refusal")
	}
	if len(err.Error()) > 512 {
		t.Fatalf("the refusal is %d bytes long; an oversized subject must not be echoed", len(err.Error()))
	}
}

// TestOriginBusComparisonIsExact pins binding check 2's case sensitivity, the
// mirror of TestSignRefusesBadInput's "bus id differing only by case".
// internal/relay already rejects ids differing only by ASCII case as id
// spoofing, and this check must not be the one place that quietly accepts them.
func TestOriginBusComparisonIsExact(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)
	pins := []ed25519.PublicKey{origin.pub}

	for _, claimed := range []string{"LAPTOP", "Laptop", "laptop ", "lapto", "laptopp"} {
		claimed := claimed
		t.Run(claimed, func(t *testing.T) {
			if _, err := Verify(pins, a, Subject{FQAgentID: katAgentID, OriginBus: claimed}, verifyNow); !errors.Is(err, ErrOriginBusMismatch) {
				t.Fatalf("Verify(subject on bus %q, pins looked up by %q) = %v, want ErrOriginBusMismatch", "laptop", claimed, err)
			}
		})
	}
}

// TestVerifyReturnsTheKEY_THE_SIGNATURE_COVERED pins the snapshot property.
//
// a.MessagingPublicKey is a SLICE, so it may be a window onto a decoded wire
// payload another goroutine still holds. Canonicalizing from the live array and
// then RE-READING it to build the return value leaves a gap in which the bytes
// verified and the bytes returned are not the same bytes. The security gate
// demonstrated exactly that against an earlier draft (RELAY-14, 2026-08-08
// P2-1) — a write landing in that gap made Verify return an attacker-chosen key
// with a nil error.
//
// This test simulates the post-verification half of that window: the returned
// key must not change when the blob's backing array is overwritten afterwards.
func TestVerifyReturnsTheKeyTheSignatureCovered(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	attackerPub, _ := mustGenerate(t, "attacker messaging")
	a := origin.attest(t, katAgentID, msgPub)

	got, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: katAgentID, OriginBus: origin.id}, verifyNow)
	if err != nil {
		t.Fatalf("Verify = error %v", err)
	}

	// Whoever else holds the decoded payload scribbles on it.
	copy(a.MessagingPublicKey, attackerPub)

	if bytes.Equal(got, attackerPub) {
		t.Fatal("the returned key followed a later write to the attestation's backing array; Verify must return the key material it canonicalized and verified")
	}
	if !bytes.Equal(got, msgPub) {
		t.Fatal("the returned key is neither the attested key nor the attacker's; Verify must return the key it verified")
	}
}

// ---------------------------------------------------------------------------
// Fail-closed shape checks.
// ---------------------------------------------------------------------------

// TestVerifyRefusesAZeroAttestationWithoutPanicking. Attestation is a VALUE
// type, not a pointer, precisely so that a path reaching Verify without a prior
// shape check is a REFUSAL rather than a nil dereference — a remote panic, not
// a remote refusal, is the failure shape that matters on an ingress.
func TestVerifyRefusesAZeroAttestationWithoutPanicking(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	pins := []ed25519.PublicKey{origin.pub}

	cases := map[string]Attestation{
		"zero value":       {},
		"no signature":     {AgentID: katAgentID, MessagingPublicKey: origin.pub, IssuedAtUnixMilli: katIssuedAt, NotAfterUnixMilli: katNotAfter},
		"short signature":  {AgentID: katAgentID, MessagingPublicKey: origin.pub, IssuedAtUnixMilli: katIssuedAt, NotAfterUnixMilli: katNotAfter, Signature: make([]byte, 63)},
		"long signature":   {AgentID: katAgentID, MessagingPublicKey: origin.pub, IssuedAtUnixMilli: katIssuedAt, NotAfterUnixMilli: katNotAfter, Signature: make([]byte, 65)},
		"no messaging key": {AgentID: katAgentID, IssuedAtUnixMilli: katIssuedAt, NotAfterUnixMilli: katNotAfter, Signature: make([]byte, ed25519.SignatureSize)},
	}
	for name, a := range cases {
		a := a
		t.Run(name, func(t *testing.T) {
			key, err := Verify(pins, a, Subject{FQAgentID: katAgentID, OriginBus: origin.id}, verifyNow)
			if err == nil {
				t.Fatalf("Verify(%s) = nil error; every malformed attestation is a refusal", name)
			}
			if key != nil {
				t.Fatalf("Verify(%s) returned a key alongside an error", name)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Verify(%s) = %v, want ErrInvalid", name, err)
			}
		})
	}
}

// TestVerifyRefusesAnEmptySubject. Subject's fields are the CALLER's own
// validated values and are never copied out of the attestation; a zero Subject
// is a caller that has not said what it is attributing, and it must not verify.
func TestVerifyRefusesAnEmptySubject(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)

	if _, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{}, verifyNow); !errors.Is(err, ErrAgentIDMismatch) {
		t.Fatalf("Verify(zero Subject) = %v, want ErrAgentIDMismatch", err)
	}
	if _, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: katAgentID}, verifyNow); !errors.Is(err, ErrOriginBusMismatch) {
		t.Fatalf("Verify(Subject with no origin bus) = %v, want ErrOriginBusMismatch", err)
	}
}

// TestAgentIDComparisonIsExact: no case folding, no trimming, no prefix match.
// A "helpful" normalisation here is a re-attribution hole — internal/relay
// already rejects ids differing only by ASCII case as id spoofing, and this
// check must not be the one place that quietly accepts them.
func TestAgentIDComparisonIsExact(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	a := origin.attest(t, katAgentID, msgPub)
	pins := []ed25519.PublicKey{origin.pub}

	for _, claimed := range []string{
		"LAPTOP.writer-7",
		"laptop.WRITER-7",
		"laptop.writer-7 ",
		" laptop.writer-7",
		"laptop.writer-70",
		"laptop.writer-",
		katAgentID + "\x00",
	} {
		claimed := claimed
		t.Run(strings.ReplaceAll(claimed, "\x00", "<NUL>"), func(t *testing.T) {
			if _, err := Verify(pins, a, Subject{FQAgentID: claimed, OriginBus: origin.id}, verifyNow); !errors.Is(err, ErrAgentIDMismatch) {
				t.Fatalf("Verify(attestation for %q, attributed to %q) = %v, want ErrAgentIDMismatch", katAgentID, claimed, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Sign's own refusals.
// ---------------------------------------------------------------------------

func TestSignRefusesBadInput(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	issued, expires := millis(katIssuedAt), millis(katNotAfter)

	cases := []struct {
		name string
		run  func() (Attestation, error)
		want error
	}{
		{"short private key", func() (Attestation, error) {
			return Sign(make([]byte, 32), origin.id, katAgentID, msgPub, 1, issued, expires)
		}, ErrInvalid},
		{"nil private key", func() (Attestation, error) {
			return Sign(nil, origin.id, katAgentID, msgPub, 1, issued, expires)
		}, ErrInvalid},
		{"unqualified agent id", func() (Attestation, error) {
			return Sign(origin.priv, origin.id, "writer-7", msgPub, 1, issued, expires)
		}, ErrInvalid},
		{"malformed bus id", func() (Attestation, error) {
			return Sign(origin.priv, "not a bus id", katAgentID, msgPub, 1, issued, expires)
		}, ErrInvalid},
		{"agent on another bus", func() (Attestation, error) {
			return Sign(origin.priv, origin.id, "workstation.writer-7", msgPub, 1, issued, expires)
		}, ErrOriginBusMismatch},
		{"bus id differing only by case", func() (Attestation, error) {
			return Sign(origin.priv, "LAPTOP", katAgentID, msgPub, 1, issued, expires)
		}, ErrOriginBusMismatch},
		{"short messaging key", func() (Attestation, error) {
			return Sign(origin.priv, origin.id, katAgentID, make([]byte, 31), 1, issued, expires)
		}, ErrInvalid},
		{"nil messaging key", func() (Attestation, error) {
			return Sign(origin.priv, origin.id, katAgentID, nil, 1, issued, expires)
		}, ErrInvalid},
		{"expiry before issue", func() (Attestation, error) {
			return Sign(origin.priv, origin.id, katAgentID, msgPub, 1, expires, issued)
		}, ErrInvalid},
		{"unset timestamps", func() (Attestation, error) {
			return Sign(origin.priv, origin.id, katAgentID, msgPub, 1, time.Unix(0, 0), time.Unix(0, 0))
		}, ErrInvalid},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a, err := tc.run()
			if err == nil {
				t.Fatalf("Sign(%s) = nil error; want a refusal", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Sign(%s) = %v, want %v", tc.name, err, tc.want)
			}
			if a.Signature != nil || a.AgentID != "" {
				t.Fatalf("Sign(%s) returned a partially populated attestation alongside an error", tc.name)
			}
			if strings.Contains(err.Error(), string(origin.priv)) {
				t.Fatalf("Sign(%s) echoed the private key in its error", tc.name)
			}
		})
	}
}

// TestSignCopiesTheMessagingKey: the caller's key may be a slice into a buffer
// it goes on to reuse. An attestation whose covered bytes change after it was
// minted is the time-of-check/time-of-use shape this codebase already guards
// against on relay signatures.
func TestSignCopiesTheMessagingKey(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")
	scratch := append(ed25519.PublicKey{}, msgPub...)

	a, err := Sign(origin.priv, origin.id, katAgentID, scratch, 1, millis(katIssuedAt), millis(katNotAfter))
	if err != nil {
		t.Fatalf("Sign = error %v", err)
	}
	scratch[0] ^= 0xff // the caller reuses its buffer

	if !bytes.Equal(a.MessagingPublicKey, msgPub) {
		t.Fatal("Sign aliased the caller's key buffer; mutating it changed the minted attestation")
	}
	if _, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: katAgentID, OriginBus: origin.id}, verifyNow); err != nil {
		t.Fatalf("the minted attestation stopped verifying after the caller reused its buffer: %v", err)
	}
}

// TestSignThenVerifyRoundTrips is the happy path across a range of inputs,
// including the boundary values the layout admits.
func TestSignThenVerifyRoundTrips(t *testing.T) {
	t.Parallel()

	origin := newBus(t, "laptop")
	msgPub, _ := mustGenerate(t, "agent messaging")

	cases := []struct {
		name    string
		agentID string
		epoch   uint64
	}{
		{"ordinary", "laptop.writer-7", 1},
		{"epoch zero — nothing assigns epochs yet, so 0 is honest", "laptop.writer-7", 0},
		{"maximum epoch", "laptop.writer-7", ^uint64(0)},
		{"maximum agent suffix", "laptop.writer-18446744073709551615", 3},
		{"single-character name", "laptop.w-1", 3},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a, err := Sign(origin.priv, origin.id, tc.agentID, msgPub, tc.epoch, millis(katIssuedAt), millis(katNotAfter))
			if err != nil {
				t.Fatalf("Sign(%s) = error %v", tc.name, err)
			}
			got, err := Verify([]ed25519.PublicKey{origin.pub}, a, Subject{FQAgentID: tc.agentID, OriginBus: origin.id}, verifyNow)
			if err != nil {
				t.Fatalf("Verify(%s) = error %v", tc.name, err)
			}
			if !bytes.Equal(got, msgPub) {
				t.Fatalf("Verify(%s) returned the wrong messaging key", tc.name)
			}
			if len(a.Signature) != ed25519.SignatureSize {
				t.Fatalf("Sign(%s) produced a %d-byte signature", tc.name, len(a.Signature))
			}
		})
	}
}
