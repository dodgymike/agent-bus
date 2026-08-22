package auth_test

// # THE P0 THIS FILE DOCUMENTED IS CLOSED. WHAT REMAINS IS THE MECHANISM.
//
// Spec Server task 1c4d3dea-b4f6-4f68-b823-78bb76a6b5aa — "unauthenticated enrol
// permanently bricks the roster" — closed by INVITE-GATE-ENFORCE
// (8297d7e2-be64-4a52-a910-314b4be880cf), which turned invariant 3's gate on.
// This file was written as a CHARACTERIZATION of the vulnerable behaviour and
// was rewritten, not deleted, when the fix landed, exactly as its own
// instructions required.
//
// # BE PRECISE ABOUT WHAT CHANGED, BECAUSE MOST OF IT DID NOT
//
// The composition was four individually-reasonable facts. TWO ARE UNCHANGED
// and are still asserted below, because they are the design and not the bug:
//
//  2. The roster is capped and FAILS CLOSED at the cap (TestRosterDoSRosterCapacityFailsClosed).
//  3. That cap is DefaultMaxRosterEntries = 4096 (TestRosterDoSProductionCapIsFourZeroNineSix).
//
// Fact 4 MOVED with AUTH-4. It used to read "NOTHING reclaims a slot — no removal
// method on the Roster interface". A slot CAN now be freed: an agent can LEAVE
// (Roster.Remove, POST /v1/leave), and a departed agent's slot returns to the
// pool. What is UNCHANGED, and is what actually made the exhaustion permanent, is
// that NOTHING AUTOMATIC reclaims a slot — there is no TTL and no eviction
// (TestRosterDoSCapacityRefusalIsNotSelfHealing still advances the clock a
// century and the refusal stands), an attacker who fills the roster and does not
// leave keeps it full across a restart (TestRosterDoSCapacitySurvivesRestart),
// and a freed slot NEVER re-issues the departed id — the per-name suffix floor is
// not reclaimed on leave (invariant 1), which
// TestRosterLeaveDoesNotReclaimSuffixFloor pins.
//
// # THE SUFFIX-COUNTER GROWTH AUTH-4 HAD TO ADDRESS
//
// Before leave existed, the per-name suffix counters (ids.NameSuffixes, one entry
// per distinct name EVER enrolled, never reclaimed — point 5 of its doc) were
// bounded because the roster never shrank, so DefaultMaxRosterEntries capped the
// distinct names too. Leave breaks that coupling: an enrol/leave loop over
// distinct names keeps roster.Len() low while the suffix map grows one entry per
// NEW name. AUTH-4's answer is invariant 3's gate, NOT reclamation (reclamation
// would reuse ids): on a gated bus every enrolment costs one operator-minted,
// SINGLE-USE invite, and the invite gate sits ABOVE the mint (service.go), so a
// refused enrolment burns no suffix. Distinct-name growth is therefore bounded by
// invites redeemed, a controlled resource — not by an anonymous loop.
// TestRosterLeaveDoesNotReclaimSuffixFloor and the invite gate
// (invitegate_enforce_test.go) are the two halves of that bound.
//
// Fact 1 is the one that moved. It was "POST /v1/enroll is unauthenticated, so
// Service.Enrol accepts a request with NO invite, NO session and NO client
// certificate". On a GATED service that request is now refused with
// ErrInviteRequired before it reaches the roster, the idempotency table or the
// minter — TestRosterDoSUninvitedEnrolmentIsRefusedWhenGated, and the closure is
// covered in full in invitegate_enforce_test.go.
//
// So the exhaustion is no longer reachable ANONYMOUSLY, which is what made it a
// P0: it took no credential, no invite and no certificate. What it costs now is
// one operator-minted, single-use invite per slot.
//
// # WHAT IS NARROWED RATHER THAN ELIMINATED — stated so nobody reads this as "solved"
//
// The MECHANISM is untouched, and that is why the tests below still stand:
//
//   - 4096 is still the cap, slots are still never freed, and a restart still
//     does not help. An operator who mints 4096 invites, or whose invites leak,
//     reaches exactly the same permanent wall by exactly the same route.
//   - The gate is a CONFIGURATION BIT (auth.Options.RequireInvite). A service
//     built without it behaves as it always did —
//     TestRosterDoSUngatedServiceStillAcceptsUninvited pins that, because the
//     tests below depend on it and because an embedder can still build one.
//   - Nothing here says anything about the SHIPPED binary. That
//     cmd/agent-bus turns the gate on is asserted where it is decided, in
//     cmd/agent-bus/invitegate_enforce_test.go, and demonstrated end to end
//     against the compiled server.
//
// The structural guard USED TO assert the interface had no reclamation verb at
// all (TestRosterDoSRosterInterfaceHasNoReclamationMethod). AUTH-4 added exactly
// one — Remove — so that guard was rewritten, as its own instructions required,
// into TestRosterReclamationIsLeaveOnly: it now asserts the ONE sanctioned verb
// is present and that no OTHER, AUTOMATIC reclamation verb (evict, expire, prune,
// compact) has crept in beside it — because auto-reclamation, unlike a
// deliberate self-leave, would drop or reuse ids without the holder's action
// (invariant 1). There is precedent for a structural guard of this shape in
// client/guard_test.go.
//
// # Why this is filable rather than "INVITE-GATE is unfinished" restated
//
// internal/auth/session.go (~lines 244-260) carries a careful availability
// analysis of the unauthenticated session-table flood and concludes it is
// untargeted, unamplified and SELF-HEALING — "ChallengeTTL ... drains them in
// two minutes". That write-up is the canonical account of what an
// unauthenticated caller can cost this bus, and it stops one resource short. The
// roster version is untargeted and unamplified too, and is NOT self-healing, NOT
// TTL-bounded, and survives reboot. TestRosterDoSCapacityRefusalIsNotSelfHealing
// is the direct contrast: it advances the same injected clock the session tests
// use by a HUNDRED YEARS and the refusal still stands.
//
// Invariants read before writing this file: 1 (server-authoritative, never-reused
// ids — which is WHY a reclaimed slot is not simply "free the id"), 3 (enrolment
// is invite-only — the gate IS now on, which is what this file was rewritten
// for), 4 and 5 (durability — which is what makes
// the damage permanent rather than transient), 10 (idempotency; the replay path
// is deliberately checked BEFORE admission control and is exercised here).

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// uninvitedEnrol makes the exact call an unauthenticated attacker makes: a name,
// a well-formed Ed25519 public key, an idempotency key, and NOTHING ELSE.
//
// Invite is nil (no invite redemption), ClientCertFingerprint is nil (the
// connection presented no client certificate), and no session is established or
// consulted anywhere on this path. Every field left zero here is left zero
// because the attacker has nothing to put in it.
func uninvitedEnrol(t *testing.T, svc *auth.Service, name, idemKey string) (auth.EnrolResult, error) {
	t.Helper()
	pub, _ := newKeypair(t)
	return svc.Enrol(auth.EnrolRequest{
		Name:                  name,
		PublicKey:             pub,
		IdempotencyKey:        idemKey,
		Invite:                nil,
		ClientCertFingerprint: nil,
	})
}

// TestRosterDoSUninvitedEnrolmentIsRefusedWhenGated is the ANSWER to what was
// this finding's unauthenticated half.
//
// The call is byte-for-byte the attacker's: no invite, no session, no client
// certificate. On a gated service it is refused and the roster is left EMPTY, so
// the exhaustion below is simply not reachable by this route. This test replaces
// TestRosterDoSUninvitedEnrolmentIsAccepted, which asserted the opposite and was
// correct until the gate landed.
func TestRosterDoSUninvitedEnrolmentIsRefusedWhenGated(t *testing.T) {
	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster, RequireInvite: true})

	res, err := uninvitedEnrol(t, svc, "attacker", "rosterdos-uninvited-1")
	if !errors.Is(err, auth.ErrInviteRequired) {
		t.Fatalf("an enrolment with NO invite, NO session and NO client certificate returned (%+v, %v) on a GATED service; want auth.ErrInviteRequired (invariant 3)", res, err)
	}
	if got := roster.Len(); got != 0 {
		t.Fatalf("roster holds %d entries after a refused uninvited enrolment, want 0; the anonymous call must consume NOTHING or the exhaustion is still reachable", got)
	}
}

// TestRosterDoSUngatedServiceStillAcceptsUninvited pins the residual, and it is
// the load-bearing premise of every exhaustion test below.
//
// The gate is a configuration bit, not a property of the package: a service
// built WITHOUT RequireInvite behaves exactly as the pre-gate bus did. Stating
// that as its own test keeps two things honest at once — the tests below are not
// silently vacuous (they really do fill a roster, because this really is still
// accepted), and nobody reads "the P0 is closed" as "internal/auth cannot be
// built into a vulnerable bus".
func TestRosterDoSUngatedServiceStillAcceptsUninvited(t *testing.T) {
	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster}) // RequireInvite NOT set

	res, err := uninvitedEnrol(t, svc, "attacker", "rosterdos-ungated-1")
	if err != nil {
		t.Fatalf("an uninvited enrolment against an UNGATED service was refused: %v\n"+
			"If the gate has become unconditional, every exhaustion test in this file is now vacuous and this file must be rewritten again.", err)
	}
	if res.AgentID == "" {
		t.Fatal("an accepted uninvited enrolment returned an empty agent id")
	}
	if res.Replayed {
		t.Error("Replayed = true on a fresh uninvited enrolment")
	}

	// It CONSUMED A SLOT — the property the exhaustion rests on.
	if got := roster.Len(); got != 1 {
		t.Fatalf("roster holds %d entries after one uninvited enrolment, want 1", got)
	}
	entry, ok := roster.Get(res.AgentID)
	if !ok {
		t.Fatalf("no roster entry for the uninvited enrolment %q", res.AgentID)
	}
	// Provenance is EMPTY, which is the durable record of "nobody authorised
	// this agent onto the bus". RosterEntry.InviteID is populated only by an
	// invited enrolment — which, on a gated bus, is now every entry.
	if entry.InviteID != "" {
		t.Errorf("an UNINVITED enrolment recorded invite id %q; it authorised itself and must carry no provenance", entry.InviteID)
	}
	if len(entry.CertBindings) != 0 {
		t.Errorf("an enrolment over a connection with NO client certificate recorded %d certificate bindings, want 0", len(entry.CertBindings))
	}
}

// TestRosterDoSRosterCapacityFailsClosed is the EXHAUSTION half: every
// uninvited enrolment up to the cap succeeds, and the very next one is refused
// with ErrCapacity.
//
// The cap is INJECTED and SMALL (auth.Options.MaxRosterEntries) so this runs in
// milliseconds. The real deployed number is pinned separately and without a slow
// test by TestRosterDoSProductionCapIsFourZeroNineSix.
func TestRosterDoSRosterCapacityFailsClosed(t *testing.T) {
	const cap = 64

	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster, MaxRosterEntries: cap})

	// All enrolments ask for the SAME name: the server mints attacker-1 ..
	// attacker-64 from its own per-name counter (invariant 1), so one anonymous
	// caller needs no name variety at all to fill the roster.
	for i := 0; i < cap; i++ {
		if _, err := uninvitedEnrol(t, svc, "attacker", fmt.Sprintf("rosterdos-cap-%d", i)); err != nil {
			t.Fatalf("uninvited enrolment %d of %d was refused before the cap: %v", i+1, cap, err)
		}
		if got, want := roster.Len(), i+1; got != want {
			t.Fatalf("after %d enrolments the roster holds %d entries, want %d", i+1, got, want)
		}
	}

	_, err := uninvitedEnrol(t, svc, "victim", "rosterdos-cap-overflow")
	if !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("the enrolment past the cap returned %v, want auth.ErrCapacity; the roster bound must FAIL CLOSED and never evict, because an evicted roster entry is an agent id silently rebound (invariant 1)", err)
	}
	if got := roster.Len(); got != cap {
		t.Fatalf("the refused enrolment changed the roster to %d entries, want it left exactly at the cap of %d", got, cap)
	}

	// The refusal is INDISCRIMINATE. Once the attacker has filled the roster,
	// a legitimate agent asking for its own name is refused identically: there
	// is no reservation, no priority and no distinction between the caller that
	// filled it and the caller that is locked out.
	if _, err := uninvitedEnrol(t, svc, "legitimate", "rosterdos-cap-legit"); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("a LEGITIMATE agent's enrolment past the cap returned %v, want auth.ErrCapacity", err)
	}
}

// phantomLenRoster is a real MemoryRoster that reports `phantom` extra entries in
// Len.
//
// It exists to observe the DEFAULT roster cap without performing 4096 fsync-free
// but still pointless enrolments. Len is the ONE thing admission control reads
// (service.go: `if s.roster.Len() >= s.maxRosterEntries`), so a roster that
// claims to hold 4095 agents puts the service exactly one enrolment away from
// the real, defaulted bound — and the pass/fail boundary of the next two calls
// measures that bound to a resolution of one.
//
// Everything else is delegated to the embedded MemoryRoster, INCLUDING both
// uniqueness rules the Roster interface requires (see Roster.Put's note on test
// doubles): this double overrides exactly one method and stubs nothing.
type phantomLenRoster struct {
	*auth.MemoryRoster
	phantom int
}

// Len implements auth.Roster, over-reporting by phantom.
func (r *phantomLenRoster) Len() int { return r.MemoryRoster.Len() + r.phantom }

// TestRosterDoSProductionCapIsFourZeroNineSix ties the small-cap test above to
// the number a deployed bus actually enforces, without a slow test.
//
// Two separate claims, deliberately not collapsed into one: the CONSTANT is
// 4096, and a service built with MaxRosterEntries unset actually RESOLVES to it.
// A test that asserted only the constant would still pass if the defaulting in
// NewService were deleted; a test that measured the default against the constant
// would still pass if the constant were changed. Both are needed.
func TestRosterDoSProductionCapIsFourZeroNineSix(t *testing.T) {
	// 4096 is written as a LITERAL in both subtests, never as
	// auth.DefaultMaxRosterEntries, so that editing the constant makes this test
	// RED rather than making it agree with itself.
	const productionCap = 4096

	t.Run("the constant is 4096", func(t *testing.T) {
		if auth.DefaultMaxRosterEntries != productionCap {
			t.Fatalf("auth.DefaultMaxRosterEntries = %d, want %d; the finding's arithmetic (that many unauthenticated calls brick the bus) is stated against %d and must be restated if the number moves", auth.DefaultMaxRosterEntries, productionCap, productionCap)
		}
	})

	// OBSERVED, not read off the source: MaxRosterEntries is unexported and
	// there is no accessor, so the only honest way to measure the effective cap
	// from an external test is to stand the service one enrolment away from it
	// and watch where acceptance turns into ErrCapacity.
	t.Run("a service built with MaxRosterEntries 0 enforces exactly 4096", func(t *testing.T) {
		roster := &phantomLenRoster{MemoryRoster: auth.NewMemoryRoster(), phantom: productionCap - 1}
		svc, _ := newService(t, auth.Options{Roster: roster, MaxRosterEntries: 0})

		// Len() == 4095: one slot left under a 4096 cap.
		if _, err := uninvitedEnrol(t, svc, "attacker", "rosterdos-default-lastslot"); err != nil {
			t.Fatalf("the enrolment at an apparent roster size of %d was refused with %v; a DEFAULTED service enforces a cap of %d or lower, not %d", productionCap-1, err, productionCap-1, productionCap)
		}

		// Len() == 4096: at the cap.
		_, err := uninvitedEnrol(t, svc, "attacker", "rosterdos-default-overflow")
		if !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("the enrolment at an apparent roster size of %d returned %v, want auth.ErrCapacity; a DEFAULTED service is enforcing a cap ABOVE %d", productionCap, err, productionCap)
		}
		// errors.Is(err, ErrCapacity) ALONE would not pin this to the roster.
		// Service.Enrol raises the SAME sentinel six lines further down for a
		// full IDEMPOTENCY table, so a service that defaulted the roster bound
		// to something enormous but capped idempotency keys low would satisfy
		// the check above while proving nothing about 4096. Only the roster
		// bound's message names the roster; keep them distinguishable.
		if !strings.Contains(err.Error(), "roster holds") {
			t.Fatalf("the refusal at an apparent roster size of %d was %q, which is ErrCapacity but NOT the roster bound; this subtest must observe the ROSTER cap, never the idempotency-table cap", productionCap, err)
		}
	})
}

// autoReclamationVerbs are the names a method that frees a slot AUTOMATICALLY —
// on a timer, on capacity pressure, or by garbage-collecting history — would
// plausibly carry. Matched case-insensitively as substrings. They are STILL
// forbidden on the Roster interface after AUTH-4: a deliberate self-leave
// (Remove) returns a slot with the holder's action and never reuses the id,
// whereas an evict/expire/prune/compact drops or reclaims an agent WITHOUT its
// action, which is where id reuse and silent identity loss come from (invariant
// 1). "delete" and "revoke" are also here: operator-initiated revocation of
// ANOTHER agent is AUTH-7's concern with a different authority model, and must
// not arrive on this interface as an unremarked verb.
var autoReclamationVerbs = []string{
	"delete", "evict", "revoke", "expire", "prune", "compact",
}

// TestRosterReclamationIsLeaveOnly is the rewrite of the pre-AUTH-4 guard
// TestRosterDoSRosterInterfaceHasNoReclamationMethod, which asserted the
// interface had NO reclamation verb at all and was designed to go RED the day one
// was added. AUTH-4 added exactly one — Remove (self-leave) — so this now
// characterises the new, narrower rule:
//
//   - EXACTLY ONE reclamation verb is present, and it is Remove. Its absence
//     would mean leave is gone; a second one would mean some other reclamation
//     path crept onto the interface unreviewed.
//   - NONE of the AUTOMATIC reclamation verbs (autoReclamationVerbs) is present.
//     Those are the dangerous shape: they free a slot without the holder acting,
//     which is how a slot gets re-issued under a live id (invariant 1). Remove is
//     safe precisely because it is a deliberate act by the agent leaving and it
//     does NOT reclaim the suffix floor — TestRosterLeaveDoesNotReclaimSuffixFloor
//     pins that behavioural half.
//
// It is DESIGNED TO GO RED again if a SECOND reclamation method (an Evict, a
// Prune, an operator Revoke) is added to the interface: at that point the
// exhaustion and id-reuse reasoning in this file must be re-characterised.
//
// # WHAT THIS GUARD DOES NOT COVER — stated so it is not mistaken for total
//
// It reflects over the auth.Roster INTERFACE, the abstraction Service holds. It
// is NOT a module-wide proof: a method on a CONCRETE roster never promoted to the
// interface, or an OFFLINE operator tool that edits the data directory directly
// (the shape AUTH-ROSTER-RECLAIM proposes, in cmd/), would slip past it. Whoever
// lands those must update this file deliberately.
func TestRosterReclamationIsLeaveOnly(t *testing.T) {
	rt := reflect.TypeOf((*auth.Roster)(nil)).Elem()
	if rt.Kind() != reflect.Interface {
		t.Fatalf("auth.Roster is a %s, not an interface; this guard reflects over the interface method set and cannot say anything useful about a %s", rt.Kind(), rt.Kind())
	}

	methods := make([]string, 0, rt.NumMethod())
	for i := 0; i < rt.NumMethod(); i++ {
		methods = append(methods, rt.Method(i).Name)
	}
	if len(methods) == 0 {
		t.Fatal("auth.Roster has an EMPTY method set; this guard would pass vacuously, so it fails instead")
	}

	// EXACTLY ONE sanctioned reclamation verb, named Remove. "leave" is the
	// concept; "Remove" is the method that implements it (Roster.Remove, called by
	// Service.Leave). A reclamation method under any other name has bypassed this
	// guard's expectation and must be reviewed here.
	reclaimers := make([]string, 0, 1)
	for _, name := range methods {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "remove") || strings.Contains(lower, "leave") {
			reclaimers = append(reclaimers, name)
		}
	}
	if len(reclaimers) != 1 || reclaimers[0] != "Remove" {
		t.Errorf("auth.Roster reclamation methods = %v, want exactly [Remove] (AUTH-4 self-leave).\n"+
			"  full method set: %v\n"+
			"If leave was removed or a second reclamation verb was added, the exhaustion and id-reuse reasoning in this file must be re-characterised. Do NOT silence this by editing the match set.",
			reclaimers, methods)
	}

	// NO automatic reclamation verb. These free a slot without the holder acting,
	// which is the id-reuse hazard invariant 1 forbids; Remove is exempt because it
	// is deliberate and floor-preserving.
	for _, name := range methods {
		lower := strings.ToLower(name)
		for _, verb := range autoReclamationVerbs {
			if strings.Contains(lower, verb) {
				t.Errorf("auth.Roster now has method %q, which looks like AUTOMATIC roster-slot reclamation (matched %q).\n"+
					"  full method set: %v\n"+
					"Automatic reclamation frees a slot without the holder's action and is exactly how an id gets re-issued under a live identity (invariant 1). A deliberate self-leave (Remove) is the ONLY sanctioned reclamation. Do NOT silence this by editing autoReclamationVerbs.",
					name, verb, methods)
			}
		}
	}

	t.Logf("auth.Roster method set inspected: %v (sanctioned reclaimer: Remove; forbidden auto-reclamation verbs: %v)", methods, autoReclamationVerbs)
}

// TestRosterLeaveDoesNotReclaimSuffixFloor is the behavioural half of AUTH-4's
// answer to "how does leave bound suffix-counter growth without reusing ids".
//
// It proves the two facts that make leave safe under invariant 1:
//
//  1. Leaving FREES A ROSTER SLOT: a roster filled to its cap admits a new
//     enrolment once one agent leaves. This is the reclamation the pre-AUTH-4
//     guard said could never exist.
//  2. Leaving does NOT reclaim the per-name SUFFIX FLOOR: re-enrolling the SAME
//     name after a leave mints a STRICTLY HIGHER suffix, never the departed id.
//     That is invariant 1 — the id is never reused, including after leave — and it
//     is what stops an enrol/leave loop from ever handing a new agent a departed
//     agent's routing and authorization identity.
//
// The suffix map growing by one per distinct name is the accepted, invite-bounded
// cost documented in the file header: eviction is forbidden (point 5 of the
// ids.NameSuffixes doc), so the map is bounded by invites redeemed, not reclaimed.
func TestRosterLeaveDoesNotReclaimSuffixFloor(t *testing.T) {
	const cap = 2

	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster, MaxRosterEntries: cap})

	// Fill the roster to its cap with one name; the server mints alice-1, alice-2.
	first, err := uninvitedEnrol(t, svc, "alice", "leave-floor-1")
	if err != nil {
		t.Fatalf("first enrolment of alice: %v", err)
	}
	if _, err := uninvitedEnrol(t, svc, "bob", "leave-floor-2"); err != nil {
		t.Fatalf("enrolment of bob to reach the cap: %v", err)
	}
	if got := roster.Len(); got != cap {
		t.Fatalf("roster holds %d after filling, want %d", got, cap)
	}

	// At the cap, a new enrolment is refused.
	if _, err := uninvitedEnrol(t, svc, "carol", "leave-floor-atcap"); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("enrolment at the cap returned %v, want auth.ErrCapacity", err)
	}

	// alice-1 LEAVES. Its slot must come back.
	res, err := svc.Leave(first.AgentID)
	if err != nil {
		t.Fatalf("leave of %q: %v", first.AgentID, err)
	}
	if res.AlreadyLeft {
		t.Fatalf("a fresh leave of %q reported AlreadyLeft; it was enrolled", first.AgentID)
	}
	if _, ok := roster.Get(first.AgentID); ok {
		t.Fatalf("agent %q is still on the roster after leaving", first.AgentID)
	}
	if got := roster.Len(); got != cap-1 {
		t.Fatalf("roster holds %d after one leave, want %d; the slot was not freed", got, cap-1)
	}

	// FACT 1: the freed slot admits a new enrolment.
	reenrol, err := uninvitedEnrol(t, svc, "alice", "leave-floor-reenrol")
	if err != nil {
		t.Fatalf("re-enrolment of alice after a leave was refused: %v; leaving did not free a slot", err)
	}

	// FACT 2: the re-enrolment did NOT get alice-1 back. The suffix floor was not
	// reclaimed, so alice's next id is strictly higher than the departed one.
	if reenrol.AgentID == first.AgentID {
		t.Fatalf("re-enrolling alice after a leave re-issued the DEPARTED id %q; an agent id is never reused, including after leave (invariant 1)", first.AgentID)
	}
	_, _, firstN, err := ids.ParseAgentID(first.AgentID)
	if err != nil {
		t.Fatalf("parsing the first alice id %q: %v", first.AgentID, err)
	}
	_, _, reN, err := ids.ParseAgentID(reenrol.AgentID)
	if err != nil {
		t.Fatalf("parsing the re-enrolled alice id %q: %v", reenrol.AgentID, err)
	}
	if reN <= firstN {
		t.Fatalf("re-enrolled alice suffix %d is not strictly greater than the departed %d; the suffix floor was reclaimed on leave, which reuses an id (invariant 1)", reN, firstN)
	}
}

// TestRosterDoSCapacityRefusalIsNotSelfHealing is the PERMANENT half,
// behaviourally, and it is the direct contrast with internal/auth/session.go's
// availability analysis at ~lines 244-260.
//
// That comment concludes the unauthenticated SESSION-table flood is
// "UNTARGETED, unamplified, self-healing", because ChallengeTTL drains pending
// entries in two minutes and SessionLifetime reclaims active ones within the
// hour. The roster flood is untargeted and unamplified too — and there is no TTL
// on it whatsoever. This test advances the SAME injected clock those session
// tests use by a hundred years and the refusal is byte-for-byte the same.
func TestRosterDoSCapacityRefusalIsNotSelfHealing(t *testing.T) {
	const cap = 8

	roster := auth.NewMemoryRoster()
	svc, clock := newService(t, auth.Options{Roster: roster, MaxRosterEntries: cap})

	for i := 0; i < cap; i++ {
		if _, err := uninvitedEnrol(t, svc, "attacker", fmt.Sprintf("rosterdos-heal-%d", i)); err != nil {
			t.Fatalf("uninvited enrolment %d of %d was refused before the cap: %v", i+1, cap, err)
		}
	}
	if got := roster.Len(); got != cap {
		t.Fatalf("roster holds %d entries after filling to the cap, want %d", got, cap)
	}

	// The refusal, at t = epoch.
	if _, err := uninvitedEnrol(t, svc, "victim", "rosterdos-heal-t0"); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("at the cap the enrolment returned %v, want auth.ErrCapacity", err)
	}

	// Every interval a self-healing resource could plausibly drain over, and
	// then some. auth.ChallengeTTL drains the pending session table; a session
	// lifetime drains the active one; a hundred years drains nothing here.
	for i, step := range []struct {
		label string
		by    time.Duration
	}{
		{"two minutes (the session ChallengeTTL horizon)", 2 * time.Minute},
		{"one hour (the SessionLifetime horizon)", time.Hour},
		{"one day", 24 * time.Hour},
		{"one hundred years", 100 * 365 * 24 * time.Hour},
	} {
		clock.Advance(step.by)

		if got := roster.Len(); got != cap {
			t.Fatalf("after advancing the clock by %s the roster holds %d entries, want it UNCHANGED at %d; if entries now age out, the exhaustion is TTL-bounded and this file must be rewritten", step.label, got, cap)
		}
		// A FRESH idempotency key each time: a reused one would be replayed
		// from the idempotency table (which is checked BEFORE admission
		// control, invariant 10) and would prove nothing about capacity.
		_, err := uninvitedEnrol(t, svc, "victim", fmt.Sprintf("rosterdos-heal-step-%d", i))
		if !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("after advancing the clock by %s the enrolment returned %v, want auth.ErrCapacity STILL; the refusal is not self-healing and must not become so silently", step.label, err)
		}
	}

	// And the roster is not merely refusing — it is refusing while HOLDING the
	// attacker's entries, none of which can be identified as an attacker's:
	// every one is a well-formed enrolment with a real key.
	if got := roster.Len(); got != cap {
		t.Fatalf("roster holds %d entries at the end, want %d", got, cap)
	}
	if n := len(roster.List()); n != cap {
		t.Fatalf("roster lists %d entries, want %d", n, cap)
	}
}

// TestRosterDoSCapacitySurvivesRestart is what makes the damage PERMANENT rather
// than a reboot away from fixed, and it drives the REAL durable path: wal.Open,
// prepare fsync, commit fsync, Apply, then a genuine reopen from the same
// directory (the pattern openRoster in walroster_test.go documents).
//
// Restart is the operator's instinctive remedy and it is exactly the wrong one
// here: recovery REPLAYS the roster (invariants 4 and 5), so the attacker's
// entries come back and the bus is refusing enrolments again before it has
// finished starting. The only remedy left is deleting the data directory, which
// destroys every legitimate agent id and the message history with it.
func TestRosterDoSCapacitySurvivesRestart(t *testing.T) {
	// Small: each enrolment costs two real fsyncs through the WAL.
	const cap = 8

	dir := t.TempDir()

	// --- first boot: the attacker fills the durable roster ---
	r1, l1 := openRoster(t, dir)
	svc1, _ := newService(t, auth.Options{Roster: r1, MaxRosterEntries: cap})

	filled := make([]string, 0, cap)
	for i := 0; i < cap; i++ {
		res, err := uninvitedEnrol(t, svc1, "attacker", fmt.Sprintf("rosterdos-restart-%d", i))
		if err != nil {
			l1.Close()
			t.Fatalf("uninvited enrolment %d of %d was refused before the cap: %v", i+1, cap, err)
		}
		filled = append(filled, res.AgentID)
	}
	if _, err := uninvitedEnrol(t, svc1, "victim", "rosterdos-restart-overflow"); !errors.Is(err, auth.ErrCapacity) {
		l1.Close()
		t.Fatalf("before the restart the enrolment past the cap returned %v, want auth.ErrCapacity", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("closing the first log: %v", err)
	}

	// --- the restart: same directory, brand new roster, log and service ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()
	// A FRESH service: fresh minter, fresh (empty) idempotency table, fresh
	// session table. Everything the process held in memory is gone. The roster
	// is what survives, and it is what this test is about.
	//
	// NOT A FAITHFUL RESTART IN ONE RESPECT, and the inaccuracy is called out
	// here rather than glossed because this file is meant to be the canonical
	// record of the finding. The per-name agent-id SUFFIX FLOORS are ALSO
	// durable in production — floors.go's EnrolmentSuffixesInWAL feeds ids.Seal,
	// driven from cmd/agent-bus — whereas newService here builds a minter over a
	// FRESH, EMPTY ids.NewNameSuffixes(): the fresh-bus constructor, born SEALED
	// with every floor at 0 (ids/agentmint.go NewNameSuffixes, and the contrast
	// with the born-UNSEALED ResumeNameSuffixes drawn in ids/doc.go). A real
	// restarted bus therefore resumes minting ABOVE the floors it recovered;
	// this one would restart "attacker-1", which is precisely the id reuse
	// invariant 1 forbids.
	//
	// It does not weaken the assertions below, and cannot manufacture a false
	// PASS: the roster is already AT the cap, so admission control refuses every
	// post-restart enrolment before a name is ever minted. The minter is not an
	// input to anything asserted here. Do not read this as evidence about
	// suffix-floor recovery either way — that is floors_test.go's subject.
	svc2, _ := newService(t, auth.Options{Roster: r2, MaxRosterEntries: cap})

	if got := r2.Len(); got != cap {
		t.Fatalf("after the restart the roster holds %d entries, want %d; if the attacker's entries did NOT survive, the exhaustion is transient and this file must be rewritten", got, cap)
	}
	for _, agentID := range filled {
		if _, ok := r2.Get(agentID); !ok {
			t.Fatalf("attacker entry %q did not survive the restart", agentID)
		}
	}

	// The refusal survives with them. A brand-new idempotency key, a brand-new
	// name, a brand-new keypair — and the bus is still full.
	if _, err := uninvitedEnrol(t, svc2, "victim", "rosterdos-restart-after"); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("after the restart the enrolment returned %v, want auth.ErrCapacity; a restart is the operator's instinctive remedy and it must be recorded here that it does NOT work", err)
	}
	if got := r2.Len(); got != cap {
		t.Fatalf("after the post-restart refusal the roster holds %d entries, want it left at %d", got, cap)
	}
}
