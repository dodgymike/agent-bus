package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

// INTERNAL guards for MTLS-BIND, on the two unexported helpers directly.
//
// # WHY THESE ARE NOT IN THE EXTERNAL TEST FILE, and it is not a style choice
//
// Both rules below are UNREACHABLE through any exported roster API, so an
// external test of either is necessarily a test of something else. Both were
// caught by mutation testing, having first been written externally and observed
// to SURVIVE the mutation that should have killed them:
//
//   - THE AMBIGUOUS ARM. The external attempt built the ambiguous state in the
//     package's test-double roster, whose AgentIDForCertFingerprint is its own
//     copy of the logic. Mutating certFingerprintOwner's ambiguous arm to return
//     holders[0] left that test GREEN, because the test never called the mutated
//     function. It was measuring the double. (The one external test that DOES
//     reach the real ambiguous arm is
//     TestWALRosterRecoversAnAmbiguousBindingAndRefusesToResolveIt, through
//     recovery — the only path that reaches it in production. It is kept, and
//     this is the fast unit-level companion to it.)
//
//   - THE SELF-SKIP. checkCertFingerprintUnbound deliberately does not refuse an
//     agent for its OWN live binding. Through Roster.Put that branch is
//     unreachable: an entry whose id is already in the map is refused by the
//     DUPLICATE-ID check first, so deleting the self-skip changes no observable
//     behaviour and the external test stayed green under the mutation. Calling
//     the helper directly is the only way to make the rule falsifiable.
//
// The lesson generalises and is the reason both are written out at length: a
// guard that survives its own mutation is not a weak guard, it is not a guard.

func ibEntry(t *testing.T, agentID string, bindings ...CertBinding) RosterEntry {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	return RosterEntry{AgentID: agentID, AuthPublicKey: pub, Epoch: now, EnrolledAt: now, CertBindings: bindings}
}

func ibFP(n byte) [32]byte {
	var fp [32]byte
	for i := range fp {
		fp[i] = n
	}
	return fp
}

func ibLive(fp [32]byte) CertBinding {
	return CertBinding{Fingerprint: fp, BoundAt: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
}

// TestCertFingerprintOwnerAmbiguousArmRefuses calls the real resolver with a map
// holding TWO live holders of one fingerprint — the state recovery can produce
// and Put cannot.
//
// MUTATION THAT KILLS IT ALONE: make certFingerprintOwner's default arm return
// holders[0] instead of the error. (Verified: this test goes RED under exactly
// that mutation, where the external stub-based version did not.)
func TestCertFingerprintOwnerAmbiguousArmRefuses(t *testing.T) {
	fp := ibFP(0x33)
	byID := map[string]RosterEntry{
		"bus-under-test.alpha-1": ibEntry(t, "bus-under-test.alpha-1", ibLive(fp)),
		"bus-under-test.beta-1":  ibEntry(t, "bus-under-test.beta-1", ibLive(fp)),
	}

	got, err := certFingerprintOwner(byID, fp)
	if !errors.Is(err, ErrCertBindingAmbiguous) {
		t.Fatalf("err = %v, want ErrCertBindingAmbiguous", err)
	}
	if got != "" {
		t.Fatalf("resolved to %q; an ambiguous certificate must name NOBODY, never the first holder found", got)
	}
	// The error names BOTH holders, because an operator has to go and retire one
	// of them and cannot act on a count.
	for _, want := range []string{"bus-under-test.alpha-1", "bus-under-test.beta-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the ambiguity error does not name %q: %v", want, err)
		}
	}
}

// TestCheckCertFingerprintUnboundSkipsTheSameAgent is the self-skip rule, called
// directly because nothing reaches it through Put (see the file comment).
//
// The rule is "already live on a DIFFERENT agent", not "already live anywhere".
// It is written that way so a future re-bind/rotate route (task 7a197025) is
// correct by construction rather than having to RELAX this check — relaxing a
// uniqueness rule is how one gets deleted.
//
// MUTATION THAT KILLS IT ALONE: drop the `agentID == e.AgentID` skip.
func TestCheckCertFingerprintUnboundSkipsTheSameAgent(t *testing.T) {
	fp := ibFP(0x88)
	e := ibEntry(t, "bus-under-test.alpha-1", ibLive(fp))
	byID := map[string]RosterEntry{"bus-under-test.alpha-1": e}

	if err := checkCertFingerprintUnbound(byID, e); err != nil {
		t.Fatalf("an agent was refused for its OWN live binding: %v", err)
	}

	// And the rule still bites for a DIFFERENT agent holding the same
	// fingerprint — asserted here so the test cannot pass by the check having
	// been removed altogether, which is the obvious wrong way to make the first
	// half green.
	other := ibEntry(t, "bus-under-test.beta-1", ibLive(fp))
	if err := checkCertFingerprintUnbound(byID, other); !errors.Is(err, ErrCertFingerprintBound) {
		t.Fatalf("a DIFFERENT agent was allowed the same live fingerprint: err = %v, want ErrCertFingerprintBound", err)
	}
}

// TestCheckCertFingerprintUnboundIgnoresRetiredBindings: a retired binding
// constrains nothing and is constrained by nothing. It is history, not a
// credential this bus will accept.
//
// MUTATION THAT KILLS IT ALONE: drop the `b.RetiredAt != nil { continue }` skip
// from checkCertFingerprintUnbound's outer loop.
func TestCheckCertFingerprintUnboundIgnoresRetiredBindings(t *testing.T) {
	fp := ibFP(0x99)
	at := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	retiredAt := at.Add(time.Hour)

	// alpha holds it LIVE; beta wants it only as a RETIRED entry, which asks
	// this bus to accept nothing and must therefore not be refused.
	byID := map[string]RosterEntry{
		"bus-under-test.alpha-1": ibEntry(t, "bus-under-test.alpha-1", ibLive(fp)),
	}
	beta := ibEntry(t, "bus-under-test.beta-1", CertBinding{Fingerprint: fp, BoundAt: at, RetiredAt: &retiredAt})
	if err := checkCertFingerprintUnbound(byID, beta); err != nil {
		t.Fatalf("a RETIRED binding was refused: %v; a retired binding authorises nothing and cannot collide", err)
	}
}

// TestCertFingerprintOwnerRefusesTheZeroFingerprint closes a FAIL-OPEN.
//
// The zero [32]byte is what a caller holds when there was NO certificate. If it
// could resolve, "this connection presented nothing" would become "this
// connection is agent X" — and the state is reachable, because
// validateRosterEntry checks a binding's BoundAt and RetiredAt but NOT its
// Fingerprint, so a record carrying a zero fingerprint decodes cleanly and is
// stored LIVE.
//
// The map here is exactly that record: an agent holding a zero-fingerprint
// binding, as recovery would produce it. It must still name nobody.
//
// MUTATION THAT KILLS IT ALONE: delete the zero-fingerprint guard from
// certFingerprintOwner. (Without the guard this test fails and reports the agent
// the zero fingerprint resolved to.)
func TestCertFingerprintOwnerRefusesTheZeroFingerprint(t *testing.T) {
	var zero [32]byte
	byID := map[string]RosterEntry{
		"bus-under-test.alpha-1": ibEntry(t, "bus-under-test.alpha-1", ibLive(zero)),
	}

	got, err := certFingerprintOwner(byID, zero)
	if !errors.Is(err, ErrCertBindingUnknown) {
		t.Fatalf("err = %v, want ErrCertBindingUnknown", err)
	}
	if got != "" {
		t.Fatalf("the ZERO fingerprint resolved to %q; absence of a certificate must never authenticate as an agent", got)
	}

	// And the guard is not a blanket refusal: a real digest in the same map
	// still resolves, so the fix cannot be "always return unknown".
	real := ibFP(0x7C)
	byID["bus-under-test.beta-1"] = ibEntry(t, "bus-under-test.beta-1", ibLive(real))
	if owner, err := certFingerprintOwner(byID, real); err != nil || owner != "bus-under-test.beta-1" {
		t.Fatalf("a real fingerprint resolved to (%q, %v), want (bus-under-test.beta-1, nil)", owner, err)
	}
}
