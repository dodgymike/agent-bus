package relay

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/attest"
)

// attestSentinelTrust is a CrossBusTrust that PINS NORMALLY and then fails
// AttestedSignerKey with a chosen error.
//
// It pins for real — a freshly generated, correctly sized Ed25519 public key —
// because VerifyRelayed checks the peering-time pin FIRST and returns
// ErrUnpeeredBus if it is missing or malformed (steps 3 and 4). A stub that
// returned no pins would never reach step 5, and the whole test would pass
// while proving nothing about the attestation seam.
type attestSentinelTrust struct {
	pin     ed25519.PublicKey
	failure error
}

func (tr *attestSentinelTrust) PinnedBusSigningKeys(string) ([]ed25519.PublicKey, error) {
	return []ed25519.PublicKey{tr.pin}, nil
}

func (tr *attestSentinelTrust) AttestedSignerKey(string, []ed25519.PublicKey) (ed25519.PublicKey, error) {
	return nil, tr.failure
}

func newAttestSentinelTrust(t *testing.T, failure error) *attestSentinelTrust {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a pin: %v", err)
	}
	return &attestSentinelTrust{pin: pub, failure: failure}
}

// attestSentinelMessage is an envelope that reaches step 5 of VerifyRelayed.
//
// Only the fields the first four steps read have to be real: the signature must
// be exactly Ed25519's length (step 2), and the ids are quoted into the error
// text. Verification never gets past AttestedSignerKey in any case here, so the
// body and the signature bytes are irrelevant and are deliberately not signed —
// signing them would suggest the outcome depended on them.
func attestSentinelMessage() RelayedMessage {
	return RelayedMessage{
		OriginBus: peerBus,
		Sender:    peerBus + ".beta-1",
		Signature: make([]byte, ed25519.SignatureSize),
	}
}

// TestSignedRelayPreservesAttestSentinels is RELAY-27's regression test.
//
// # What was broken
//
// VerifyRelayed wrapped AttestedSignerKey's error with %v, not %w, so ALL FIVE
// attest sentinels collapsed: errors.Is found none of them, every one answered
// errors.Is(err, ErrNoSignerKey) instead, and every one went out on the wire as
// 403 bad_signature. A peer was told "forgery" for an unfinished peering and for
// a message that merely queued past its attestation's expiry.
//
// # Why it is asserted THROUGH VerifyRelayed and through the HTTP ingress
//
// The bug lived in the seam, not in either package. attest's own tests proved
// its sentinels were distinct and relay's proved its codes were stable; both
// were green throughout, because neither ran the path where one becomes the
// other. So the table below calls the REAL VerifyRelayed, and the wire subtest
// posts to the REAL RelayHandler.
//
// # THE go1.19 TRAP THIS TEST EXISTS TO CATCH
//
// The obvious fix — a second %w in that one Errorf — does not work on go1.19
// (go.mod's pin, and golang:1.19.4 in the digest-pinned builder at
// Dockerfile:15). Multiple %w verbs are go1.20+; go1.19 wraps NEITHER operand
// and renders the second as "%!w(*errors.errorString=&{…})", so errors.Is fails
// for BOTH — the attest sentinel AND ErrNoSignerKey, which ErrorCode depends on.
// No positive test anywhere would notice, because a passing path never inspects
// an error.
//
// go vet DOES reject that literal spelling on go1.19 ("fmt.Errorf call has more
// than one error-wrapping directive %w"), and vet is mandated before every
// commit — so the tripwire here is not for the obvious edit. It is for the same
// mistake reached any other way: a %v that should have been a %w (which is
// exactly how RELAY-27 arose), a helper that drops the chain, an Unwrap that
// returns the wrong operand, or a non-constant format string, which vet does not
// analyse. Hence every case asserts errors.Is in BOTH directions on the SAME
// error value, and asserts the rendered text contains no "%!w(" so that if one
// ever does slip through, the failure names the cause instead of being
// mysterious.
func TestSignedRelayPreservesAttestSentinels(t *testing.T) {
	// Each attest sentinel, as internal/attest actually returns it: wrapped with
	// detail behind a single %w. A bare sentinel would be a weaker test, since it
	// would pass even for an implementation that only compared errors directly.
	cases := []struct {
		name string
		// cause is what the CrossBusTrust returns.
		cause error
		// attestSentinel must remain reachable with errors.Is. THIS is the
		// regression: before RELAY-27 every one of these was false.
		attestSentinel error
		// relaySentinel is the relay-level sentinel that must also match, so
		// ErrorCode still has something to map.
		relaySentinel error
		// notRelaySentinel must NOT match, pinning that exactly one relay
		// sentinel answers for this cause rather than several.
		notRelaySentinel error
		wantCode         string
		// wantStatus is what the relay ingress must answer a peer.
		wantStatus int
		why        string
	}{
		{
			name:             "unpinned origin bus is the operator-actionable one",
			cause:            fmt.Errorf("%w: no pinned signing key was supplied for the origin bus", attest.ErrUnpinned),
			attestSentinel:   attest.ErrUnpinned,
			relaySentinel:    ErrUnpeeredBus,
			notRelaySentinel: ErrNoSignerKey,
			wantCode:         CodeUnpeeredBus,
			wantStatus:       http.StatusForbidden,
			why:              "this is the ONLY diagnosis here with an operator remedy — complete the peering. Answered bad_signature it sends an operator hunting a forgery on the ordinary day-one state of an unfinished federation",
		},
		{
			name:             "a malformed attestation is a 400, not a 403",
			cause:            fmt.Errorf("%w: agent id: bad", attest.ErrInvalid),
			attestSentinel:   attest.ErrInvalid,
			relaySentinel:    ErrInvalidRelay,
			notRelaySentinel: ErrNoSignerKey,
			wantCode:         CodeInvalidRelay,
			wantStatus:       http.StatusBadRequest,
			why:              "attest.ErrInvalid means nobody could ever check this attestation, which is a malformed request and not a refusal to attribute; attest's own doc directs callers to answer it 400",
		},
		{
			name:             "an expired attestation keeps its sentinel",
			cause:            fmt.Errorf("%w: subject %q", attest.ErrExpired, peerBus+".beta-1"),
			attestSentinel:   attest.ErrExpired,
			relaySentinel:    ErrNoSignerKey,
			notRelaySentinel: ErrUnpeeredBus,
			wantCode:         CodeBadSignature,
			wantStatus:       http.StatusForbidden,
			why:              "expiry is far more often an honest message queued across a partition than a forgery — the wire code is still coarse (RELAY-27-FU-EXPIRED) but this bus's own logs and callers can now tell it apart",
		},
		{
			name:             "an attestation naming a different agent keeps its sentinel",
			cause:            fmt.Errorf("%w: names %q", attest.ErrAgentIDMismatch, peerBus+".gamma-1"),
			attestSentinel:   attest.ErrAgentIDMismatch,
			relaySentinel:    ErrNoSignerKey,
			notRelaySentinel: ErrUnpeeredBus,
			wantCode:         CodeBadSignature,
			wantStatus:       http.StatusForbidden,
			why:              "this is attest's load-bearing check; a refusal is right, but the reason must survive to the audit trail",
		},
		{
			name:             "an attestation that does not verify keeps its sentinel",
			cause:            fmt.Errorf("%w: 1 pin(s) tried", attest.ErrVerify),
			attestSentinel:   attest.ErrVerify,
			relaySentinel:    ErrNoSignerKey,
			notRelaySentinel: ErrUnpeeredBus,
			wantCode:         CodeBadSignature,
			wantStatus:       http.StatusForbidden,
			why:              "the one case where bad_signature is genuinely the right answer — it must still be distinguishable from the four that are not",
		},

		// The two attest sentinels RELAY-27's title does not name. attest exports
		// SEVEN; a taxonomy task that pinned only the five it was filed about
		// would leave the other two free to be re-mapped by a future arm without
		// anything going red (security gate, RELAY-27 P2-2).
		{
			name:             "an attestation from another bus we also pin keeps its sentinel",
			cause:            fmt.Errorf("%w: subject is on a different bus", attest.ErrOriginBusMismatch),
			attestSentinel:   attest.ErrOriginBusMismatch,
			relaySentinel:    ErrNoSignerKey,
			notRelaySentinel: ErrUnpeeredBus,
			wantCode:         CodeBadSignature,
			wantStatus:       http.StatusForbidden,
			why:              "this is the most attack-shaped error attest has — a peer presenting a validly-signed attestation from a DIFFERENT bus we also pin. bad_signature is the correct answer, and this case exists to keep it that way",
		},
		{
			name:             "our own missing clock is still a refusal",
			cause:            fmt.Errorf("%w", attest.ErrNoClock),
			attestSentinel:   attest.ErrNoClock,
			relaySentinel:    ErrNoSignerKey,
			notRelaySentinel: ErrUnpeeredBus,
			wantCode:         CodeBadSignature,
			wantStatus:       http.StatusForbidden,
			why:              "PINS A KNOWN GAP, NOT AN ENDORSEMENT (RELAY-27-FU-EXPIRED): ErrNoClock is OUR wiring fault, and attest gives it a separate sentinel precisely so it is not reported to a peer as the peer's bad request — yet we answer 403, so the peer permanently drops a message that was fine. Pre-existing, not a RELAY-27 regression. When the follow-up gives it its own code, CHANGE this case deliberately rather than discovering it",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			trust := newAttestSentinelTrust(t, tc.cause)
			err := VerifyRelayed(attestSentinelMessage(), trust)
			if err == nil {
				t.Fatalf("VerifyRelayed returned nil for a trust that refused; every trust failure is a refusal, never an allow")
			}

			// THE REGRESSION. Before RELAY-27 this was false for all five.
			if !errors.Is(err, tc.attestSentinel) {
				t.Errorf("errors.Is(err, %v) = false, want true\n  err  = %v\n  why  = %s", tc.attestSentinel, err, tc.why)
			}

			// AND, on the SAME value, the relay sentinel. This pair is the
			// go1.19 two-%w proof: with two %w verbs BOTH of these go false.
			if !errors.Is(err, tc.relaySentinel) {
				t.Errorf("errors.Is(err, %v) = false, want true — the relay sentinel must survive alongside the attest one, or ErrorCode has nothing to map\n  err = %v", tc.relaySentinel, err)
			}
			if errors.Is(err, tc.notRelaySentinel) {
				t.Errorf("errors.Is(err, %v) = true, want false — exactly ONE relay sentinel may answer for a given cause, or the wire code depends on the order ErrorCode happens to test them in", tc.notRelaySentinel)
			}

			// The go1.19 tripwire proper: fmt renders an unsupported second %w
			// verb as "%!w(…)" and does not wrap. Text, not errors.Is, is what
			// makes that failure legible rather than mysterious.
			if strings.Contains(err.Error(), "%!w(") {
				t.Errorf("the error text contains a %%!w( badverb, so an fmt.Errorf here used two %%w verbs — go1.19 (go.mod, and golang:1.19.4 at Dockerfile:15) wraps NEITHER operand in that case and errors.Is fails for both\n  err = %v", err)
			}

			if got := ErrorCode(err); got != tc.wantCode {
				t.Errorf("ErrorCode = %q, want %q\n  why = %s", got, tc.wantCode, tc.why)
			}
		})
	}

	// The wire is what the peer actually sees, and it is the thing RELAY-27 was
	// filed about: a code and a status, not an errors.Is result. Asserting it
	// through the real RelayHandler is what makes the 400/403 split real rather
	// than a property of a mapping function nobody has connected.
	t.Run("the ingress answers each one on the wire", func(t *testing.T) {
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				trust := newAttestSentinelTrust(t, tc.cause)
				remote := newRelayResponder(t, localBus, func(c *RelayConfig) { c.Trust = trust })

				status, code, _ := remote.postRelay(t, relayFixture())
				if status != tc.wantStatus || code != tc.wantCode {
					t.Fatalf("the ingress answered %d/%q, want %d/%q\n  why = %s", status, code, tc.wantStatus, tc.wantCode, tc.why)
				}
				if n := len(remote.acceptedMessages()); n != 0 {
					t.Fatalf("AcceptRelay was called %d times for an envelope we refused to attribute, want 0", n)
				}
			})
		}
	})

	// A TRUST MAY NOT LAUNDER ITS REFUSAL INTO SOME OTHER OUTCOME.
	//
	// Preserving the cause is the whole point of RELAY-27, but it also makes the
	// cause reachable by every errors.Is in the ingress — including ErrRelayLoop,
	// which relayhttp answers 200 "settled, dropped" BEFORE ErrorCode is
	// consulted. Without the guard at the construction site, a CrossBusTrust
	// returning an error that wrapped ErrRelayLoop turns an ATTRIBUTION REFUSAL
	// into a routine loop drop: counted as a loop, logged Info instead of Warn,
	// and the peer told not to retry. Found by the RELAY-27 security gate (P2-1)
	// by probing it, not by reading it.
	//
	// internal/attest cannot reach this today — it imports only ids and signing,
	// so it cannot name a relay sentinel — which is exactly why it is tested:
	// CrossBusTrust is an INTERFACE, and the next implementation is under no such
	// constraint.
	t.Run("a trust error carrying a relay code of its own cannot hijack the outcome", func(t *testing.T) {
		for _, hijack := range []struct {
			name string
			with error
		}{
			{"a loop, which is answered 200 and is not a refusal at all", ErrRelayLoop},
			{"an oversized payload, which is a different code", ErrPayloadTooLarge},
		} {
			hijack := hijack
			t.Run(hijack.name, func(t *testing.T) {
				cause := fmt.Errorf("the trust store said: %w", hijack.with)
				trust := newAttestSentinelTrust(t, cause)

				err := VerifyRelayed(attestSentinelMessage(), trust)
				if errors.Is(err, hijack.with) {
					t.Errorf("errors.Is(err, %v) = true: a CrossBusTrust can steer the ingress by wrapping a relay sentinel, and an attribution refusal becomes some other outcome entirely", hijack.with)
				}
				if !errors.Is(err, ErrNoSignerKey) {
					t.Errorf("errors.Is(err, ErrNoSignerKey) = false; the refusal must stand whatever the trust wrapped\n  err = %v", err)
				}
				if got := ErrorCode(err); got != CodeBadSignature {
					t.Errorf("ErrorCode = %q, want %q — the relay sentinel, not the trust's, decides the wire answer", got, CodeBadSignature)
				}
				// The diagnosis is dropped from the CHAIN, never from the TEXT:
				// severing it must not cost an operator the reason.
				if !strings.Contains(err.Error(), "the trust store said") {
					t.Errorf("the cause left the error TEXT as well as the chain; the guard drops it from errors.Is only\n  err = %v", err)
				}
			})
		}

		// And the wire, since that is where the laundering would have shown:
		// 403 bad_signature, NOT 200 accepted:false dropped_reason=loop.
		trust := newAttestSentinelTrust(t, fmt.Errorf("store: %w", ErrRelayLoop))
		remote := newRelayResponder(t, localBus, func(c *RelayConfig) { c.Trust = trust })

		status, code, body := remote.postRelay(t, relayFixture())
		if status != http.StatusForbidden || code != CodeBadSignature {
			t.Fatalf("the ingress answered %d/%q for a refusal whose cause wrapped ErrRelayLoop, want %d/%q; a refusal reported as a settled loop drop is invariant 6's \"every discard is LOGGED, loudly and specifically\" failing silently", status, code, http.StatusForbidden, CodeBadSignature)
		}
		if body.DroppedReason == DropLoop {
			t.Fatalf("the refusal was reported to the peer as a loop drop")
		}
		if n := len(remote.acceptedMessages()); n != 0 {
			t.Fatalf("AcceptRelay was called %d times, want 0", n)
		}
	})

	// RELAY-27's other half: the taxonomy got FINER, it did not get LEAKIER.
	// Everything that answered ErrNoSignerKey for a reason unrelated to attest
	// must still answer it, or the fix has quietly opened the fail-closed path.
	t.Run("ErrNoSignerKey still answers for the cases that always meant it", func(t *testing.T) {
		t.Run("a nil trust", func(t *testing.T) {
			err := VerifyRelayed(attestSentinelMessage(), nil)
			if !errors.Is(err, ErrNoSignerKey) {
				t.Fatalf("VerifyRelayed(_, nil) = %v, want one wrapping ErrNoSignerKey; a nil-means-allow branch is one missing argument away from an unauthenticated relay ingress", err)
			}
			if got := ErrorCode(err); got != CodeBadSignature {
				t.Errorf("ErrorCode = %q, want %q", got, CodeBadSignature)
			}
		})

		t.Run("a trust error that is not an attest sentinel at all", func(t *testing.T) {
			// A CrossBusTrust is an interface anyone may implement, so the
			// default arm carries the fail-closed guarantee for every error the
			// mapping has never seen — a store outage, a future sentinel, a
			// wiring bug. It must be a REFUSAL, never an allow.
			cause := errors.New("relay-27: the key store is unreachable")
			trust := newAttestSentinelTrust(t, cause)

			err := VerifyRelayed(attestSentinelMessage(), trust)
			if !errors.Is(err, ErrNoSignerKey) {
				t.Fatalf("errors.Is(err, ErrNoSignerKey) = false for an unrecognised trust error (%v); an error this mapping has never seen MUST fall closed onto a refusal", err)
			}
			if !errors.Is(err, cause) {
				t.Errorf("the cause did not survive for a non-attest error either; the wrapping must be uniform, not special-cased for attest")
			}
			for _, other := range []error{ErrUnpeeredBus, ErrInvalidRelay, ErrMissingSignature, ErrUnsignable, ErrBadSignature} {
				if errors.Is(err, other) {
					t.Errorf("errors.Is(err, %v) = true for an unrecognised trust error, want false", other)
				}
			}
			if got := ErrorCode(err); got != CodeBadSignature {
				t.Errorf("ErrorCode = %q, want %q", got, CodeBadSignature)
			}
		})
	})
}
