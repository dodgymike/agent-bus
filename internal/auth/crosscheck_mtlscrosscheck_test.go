package auth_test

import (
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
)

// Guards for MTLS-CROSSCHECK's AGENT-SIDE read: Service.LiveCertBindings, the
// question certbind.go cannot ask — "does THIS AGENT require a certificate, and
// if so which?".
//
// # EVERY GUARD HERE WAS WATCHED FAILING
//
// Reading a test tells you what it asserts, not whether it CAN fail. Each test
// below names, in a "MUTATION THAT KILLS IT ALONE" line, the single edit to the
// shipped source that turns it red — and every one of those mutations was
// applied on its own, run, and observed to fail EXACTLY the named test before
// being reverted. The transcript is in the task's report note.
//
// The arm that needed this most is the ZERO FINGERPRINT one. "Skip a binding
// whose fingerprint is all zeroes" looks like tidying up a damaged record and is
// in fact a fail-OPEN: it makes an agent whose durable record ROTTED look
// unbound, so the requirement recorded against it evaporates and it gets LESS
// enforcement than an agent whose record is intact. Nothing else in this package
// would notice.
//
// The fingerprints are synthetic [32]byte values, as in certbind_mtlsbind_test.
// go and for the same reason: this package stores a DIGEST and never parses a
// certificate. That the digest is computed the one true way is guarded in
// internal/httpapi, over real DER.

// TestCrossCheckLiveCertBindingsUnknownAgentIsNil: an agent the roster has never
// heard of states no requirement.
//
// nil is the right answer and NOT a refusal — refusing here is not this
// function's job. Its callers hold either a server-minted principal (so the
// agent exists) or a client-supplied id that BeginSession refuses for itself
// with the 404 that route already gives. What matters is that an unknown agent
// does not resolve to somebody else's requirement.
//
// MUTATION THAT KILLS IT ALONE: make the `if !ok` branch fall back to the first
// roster entry — `if l := s.roster.List(); len(l) > 0 { e = l[0] } else { return
// nil }` — the "look up the agent, fall back to something" shape this test exists
// to forbid. (Merely deleting the `if !ok` branch is NOT a mutation: the zero
// RosterEntry has no bindings, so the answer is still nil.)
func TestCrossCheckLiveCertBindingsUnknownAgentIsNil(t *testing.T) {
	r := auth.NewMemoryRoster()
	// A populated roster, deliberately: against an EMPTY one a function that
	// returned "the only agent's bindings" would also return nil, and the test
	// could not tell the two apart.
	if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", liveBinding(fpN(0x11)))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	svc, _ := newService(t, auth.Options{Roster: r})

	if got := svc.LiveCertBindings("bus-under-test.nobody-1"); got != nil {
		t.Fatalf("an unknown agent stated a requirement of %d binding(s) (%x); it must state none", len(got), got)
	}
	// A malformed id is the same answer, by the same route: it is not in the
	// roster. Stated because the /v1/session/begin call site passes an UNVALIDATED
	// client string straight through.
	if got := svc.LiveCertBindings("not-an-agent-id"); got != nil {
		t.Fatalf("a malformed agent id resolved to %d binding(s); it names nobody", len(got))
	}
}

// TestCrossCheckLiveCertBindingsReportsTheLiveSet is the shape of the answer, in
// the four states an agent's history can be in.
//
// The ROTATION row is the one with teeth: invariant 11 has a rotation serve two
// certificates at once, so BOTH live bindings must be reported and the caller
// must be free to accept either. A "newest wins" read here would refuse the
// outgoing certificate for the whole rollover — silently, because the incoming
// one keeps working.
//
// MUTATION THAT KILLS IT ALONE: delete the `if b.RetiredAt != nil { continue }`
// arm (the "retired is excluded" and "retired only" rows go red); or return only
// the last element (the rotation row goes red).
func TestCrossCheckLiveCertBindingsReportsTheLiveSet(t *testing.T) {
	live1, live2, dead := fpN(0x21), fpN(0x22), fpN(0x23)

	for _, tc := range []struct {
		name     string
		bindings []auth.CertBinding
		want     [][32]byte
	}{
		{"no bindings at all", nil, nil},
		{"one live binding", []auth.CertBinding{liveBinding(live1)}, [][32]byte{live1}},
		{"a retired binding is excluded", []auth.CertBinding{liveBinding(live1), retiredBinding(dead)}, [][32]byte{live1}},
		{"retired only states no requirement", []auth.CertBinding{retiredBinding(dead)}, nil},
		{"a rotation reports BOTH live bindings, in order", []auth.CertBinding{liveBinding(live1), liveBinding(live2)}, [][32]byte{live1, live2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := auth.NewMemoryRoster()
			if err := r.Put(mtlsEntry(t, "bus-under-test.alpha-1", tc.bindings...)); err != nil {
				t.Fatalf("Put: %v", err)
			}
			svc, _ := newService(t, auth.Options{Roster: r})

			got := svc.LiveCertBindings("bus-under-test.alpha-1")
			if len(got) != len(tc.want) {
				t.Fatalf("LiveCertBindings returned %d binding(s), want %d: got %x, want %x", len(got), len(tc.want), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("binding %d is %x, want %x (the order they are stored in is the order they are returned)", i, got[i][:4], tc.want[i][:4])
				}
			}
		})
	}
}

// TestCrossCheckLiveCertBindingsIncludesAZeroFingerprint is the FAIL-CLOSED
// choice, and it is the reason this file exists as a separate guard rather than
// one more row in the table above.
//
// A live binding carrying the zero fingerprint can only come from a damaged or
// hand-edited durable record: Enrol binds only a non-nil digest taken from a real
// parsed certificate, but validateRosterEntry checks BoundAt and RetiredAt and
// does NOT reject a zero Fingerprint, so such a record decodes cleanly and is
// stored LIVE.
//
// It must be INCLUDED. No certificate a client can present has an all-zero
// sha256 of its DER, so including it makes the agent UNSATISFIABLE — every
// request naming it is refused until an operator repairs the record. Loud,
// contained, fixable. FILTERING it would make the agent look UNBOUND, silently
// deleting the requirement recorded against it: the agent whose record ROTTED
// would end up with LESS enforcement than one whose record is intact, which is
// precisely backwards, and nothing would notice.
//
// The httpapi half of this guard is
// TestCrossCheckAZeroFingerprintBindingRefusesEvenARealCertificate, which proves
// the consequence — such an agent is refused at the HTTP gate even when it
// presents a perfectly good certificate. Two tests, one mutation, asserting
// different consequences of the same decision; that is the safe direction.
//
// MUTATION THAT KILLS IT ALONE: skip the zero fingerprint in LiveCertBindings —
// `if b.Fingerprint == ([32]byte{}) { continue }`, the fail-OPEN version.
func TestCrossCheckLiveCertBindingsIncludesAZeroFingerprint(t *testing.T) {
	var zero [32]byte
	r := auth.NewMemoryRoster()
	if err := r.Put(mtlsEntry(t, "bus-under-test.rotted-1", liveBinding(zero))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	svc, _ := newService(t, auth.Options{Roster: r})

	got := svc.LiveCertBindings("bus-under-test.rotted-1")
	if len(got) != 1 || got[0] != zero {
		t.Fatalf("LiveCertBindings = %x, want exactly one zero fingerprint: an agent whose durable record carries a zero binding is UNSATISFIABLE, never unbound — filtering it would delete the requirement that record was written to state", got)
	}

	// AND THE OTHER SIDE OF IT, which is what makes the inclusion safe rather
	// than merely strict: the zero fingerprint still resolves to NOBODY. So the
	// agent cannot be satisfied by a caller that presented no certificate and
	// ignored the ok — the case certFingerprintOwner refuses in terms.
	if owner, err := svc.AgentIDForClientCertificate(zero); err == nil {
		t.Fatalf("the zero fingerprint resolved to %q; it is the ABSENCE of a certificate and must name nobody", owner)
	}
}
