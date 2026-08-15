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
// The composition was four individually-reasonable facts. THREE ARE UNCHANGED
// and are still asserted below, because they are the design and not the bug:
//
//  2. The roster is capped and FAILS CLOSED at the cap (TestRosterDoSRosterCapacityFailsClosed).
//  3. That cap is DefaultMaxRosterEntries = 4096 (TestRosterDoSProductionCapIsFourZeroNineSix).
//  4. NOTHING reclaims a slot — no removal method on the Roster interface
//     (TestRosterDoSRosterInterfaceHasNoReclamationMethod), the refusal is not
//     TTL-bounded (TestRosterDoSCapacityRefusalIsNotSelfHealing), and the
//     entries survive a restart (TestRosterDoSCapacitySurvivesRestart).
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
// The structural guard in TestRosterDoSRosterInterfaceHasNoReclamationMethod is
// deliberately KEPT and still passes: the gate did NOT add reclamation, and
// reclamation is still the wrong fix (invariant 1 — freeing a slot by reusing an
// id is precisely what must never happen). It remains designed to fail the day
// someone adds a reclamation verb. There is precedent for a structural guard of
// this shape in client/guard_test.go.
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

// reclamationVerbs are the names a method that FREES A ROSTER SLOT would
// plausibly carry. Matched case-insensitively as substrings, so Remove,
// RemoveAgent, Unenrol... anything of the shape trips it.
var reclamationVerbs = []string{
	"remove", "delete", "evict", "leave", "revoke", "expire", "prune", "compact",
}

// TestRosterDoSRosterInterfaceHasNoReclamationMethod is the PERMANENT half,
// structurally: the auth.Roster interface offers no way to give a slot back.
//
// This is the assertion that makes "permanent" a property of the TYPE rather
// than of a code path someone might have missed. A capacity refusal is a
// transient outage if anything anywhere can free a slot; it is a permanent one
// if the abstraction the service holds has no such verb at all — and it does
// not. Both shipped rosters (MemoryRoster, WALRoster) implement exactly this
// method set, and Service holds the interface, not a concrete type.
//
// # IT IS DESIGNED TO GO RED WHEN RECLAMATION IS ADDED
//
// That is the whole value. The day someone adds Remove, Evict or Revoke to this
// interface, this test names it and this file must be rewritten to characterise
// the new, better behaviour. Do not "fix" it by widening the allow-list.
//
// # WHAT THIS GUARD DOES NOT COVER — stated so it is not mistaken for total
//
// It reflects over the auth.Roster INTERFACE, which is the abstraction Service
// actually holds, and that is the right locus for the claim above. It is NOT a
// module-wide proof that nothing can free a slot. Reclamation would slip past it
// in at least two shapes: a method added only to a CONCRETE roster
// (*WALRoster, *MemoryRoster) and never promoted to the interface, and an
// OFFLINE operator tool that edits the data directory without going through this
// package at all — which is exactly the shape AUTH-ROSTER-RECLAIM
// (b418638c-e9bc-4666-9998-6806f110e357) proposes, in cmd/. Whoever lands that
// must update this file deliberately; it will not be caught here.
func TestRosterDoSRosterInterfaceHasNoReclamationMethod(t *testing.T) {
	rt := reflect.TypeOf((*auth.Roster)(nil)).Elem()
	if rt.Kind() != reflect.Interface {
		t.Fatalf("auth.Roster is a %s, not an interface; this guard reflects over the interface method set and cannot say anything useful about a %s", rt.Kind(), rt.Kind())
	}

	methods := make([]string, 0, rt.NumMethod())
	for i := 0; i < rt.NumMethod(); i++ {
		methods = append(methods, rt.Method(i).Name)
	}
	// A method set of zero would make every assertion below vacuously true.
	if len(methods) == 0 {
		t.Fatal("auth.Roster has an EMPTY method set; this guard would pass vacuously, so it fails instead")
	}

	for _, name := range methods {
		lower := strings.ToLower(name)
		for _, verb := range reclamationVerbs {
			if strings.Contains(lower, verb) {
				t.Errorf("auth.Roster now has method %q, which looks like roster-slot RECLAMATION (matched %q).\n"+
					"  full method set: %v\n"+
					"If a slot can now be freed, the capacity exhaustion recorded in this file is no longer PERMANENT and every assertion here must be rewritten to characterise the new behaviour. Do NOT silence this by editing reclamationVerbs.",
					name, verb, methods)
			}
		}
	}

	// Reported unconditionally so a reader of a passing run still sees exactly
	// what was inspected, and so a method set that shrank to something
	// meaningless is visible rather than silently green. It states what was
	// inspected, NOT the verdict — the assertions above own the verdict.
	t.Logf("auth.Roster method set inspected against %v: %v", reclamationVerbs, methods)
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
