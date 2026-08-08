package ids

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

// TestNameSuffixesRefusesToIssueFromAnUnsealedFloor is the proof command for
// ID-2-WIRING-SEAL-FU-NAMESUFFIXES. It pins the two-state, one-way machine
// documented under "The floors must be SEALED before anything is issued":
//
//   - UNSEALED (ResumeNameSuffixes, and the zero value): NextSuffix issues
//     NOTHING for ANY name and returns (0, ErrFloorUnproven); RaiseFloor is
//     legal.
//   - SEALED, after exactly one Seal — or from birth, for NewNameSuffixes:
//     NextSuffix issues for EVERY name, including names absent from the floor
//     map; RaiseFloor returns ErrFloorSealed and changes nothing; a second Seal
//     likewise.
//
// The seal is GLOBAL, not per name, so "which names may issue" is never a
// question about a name — it is one question about the whole allocator.
//
// The load-bearing assertions are the ones about what a REFUSED call did to the
// allocator: a refusal that quietly burned a suffix would still look like a
// refusal to the caller, and would re-mint an AGENT ID one restart later —
// worse than a re-minted message id, because the agent id is the routing and
// authorization subject (invariants 2 and 3).
func TestNameSuffixesRefusesToIssueFromAnUnsealedFloor(t *testing.T) {
	// THE defect this task closes, built end to end: before the seal, a floor
	// that was still being derived — or one derived far too low — could go live
	// SILENTLY, because the only guard on RaiseFloor fired once something had
	// been issued for the name, and during the derivation window nothing had.
	t.Run("ResumeRefusesUntilSealedAndBurnsNothing", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 41, "dave": 7})

		// The assembly window is still open, and claims legitimately arrive in
		// any order from several sources (the WAL high-water mark, the audit
		// high-water mark, a peer). A stale, LOWER claim is a no-op here — the
		// floor is the maximum of the claims — and a higher one raises.
		for _, tc := range []struct {
			name    string
			atLeast uint64
			why     string
		}{
			{"bob", 20, "stale lower claim: no-op, the floor is a maximum"},
			{"bob", 41, "equal claim: no-op, nothing new was learned"},
			{"dave", 300, "higher claim: raises dave's floor"},
		} {
			if err := s.RaiseFloor(tc.name, tc.atLeast); err != nil {
				t.Fatalf("RaiseFloor(%q, %d) while UNSEALED: %v, want nil (%s)", tc.name, tc.atLeast, err, tc.why)
			}
		}

		// THE assertion this subtest exists for. Until the caller says out loud
		// that the floors are proven, NO suffix may be issued for ANY name — so
		// there is no value, correct or far too low, that can serve without an
		// explicit claim. The refusal repeats: the state machine has one
		// transition and NextSuffix is not it.
		for i := 0; i < 5; i++ {
			for _, name := range []string{"bob", "dave", "carol"} {
				n, err := s.NextSuffix(name)
				if n != 0 {
					t.Fatalf("unsealed NextSuffix(%q) call #%d = %d, want 0 (an unsealed allocator issues nothing, for any name)", name, i, n)
				}
				if !errors.Is(err, ErrFloorUnproven) {
					t.Fatalf("unsealed NextSuffix(%q) call #%d error = %v, want errors.Is(err, ErrFloorUnproven)", name, i, err)
				}
				if last := s.LastSuffix(name); last != 0 {
					t.Fatalf("LastSuffix(%q) after %d refused NextSuffix calls = %d, want 0 (nothing was issued)", name, i+1, last)
				}
			}
		}

		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil (first seal on a resumed allocator)", err)
		}

		// Fifteen refused calls burned NOTHING: bob still resumes at floor+1 ==
		// 42, dave at the raised floor+1 == 301, and carol — a name that was
		// never proven and is now asserted-absent by the seal — mints 1. Had a
		// refusal inched a floor forward, bob would start at 47 and the bus
		// would have a silent hole in its id space for every name.
		for _, tc := range []struct {
			name string
			want uint64
			why  string
		}{
			{"bob", 42, "floor 41 + 1; the two no-op RaiseFloor claims justified nothing"},
			{"dave", 301, "floor raised to 300 during assembly, + 1"},
			{"carol", 1, "absent from the floors: the seal asserted it was never written"},
		} {
			n, err := s.NextSuffix(tc.name)
			if err != nil {
				t.Fatalf("NextSuffix(%q) after Seal(): unexpected error %v", tc.name, err)
			}
			if n != tc.want {
				t.Fatalf("NextSuffix(%q) after Seal() = %d, want %d (%s) — the refused calls must have burned NOTHING", tc.name, n, tc.want, tc.why)
			}
			if last := s.LastSuffix(tc.name); last != tc.want {
				t.Fatalf("LastSuffix(%q) after the first issued suffix = %d, want %d", tc.name, last, tc.want)
			}
		}
	})

	// The other half of the defect: before the seal existed, a stale or far-too-
	// low claim arriving DURING the whole derivation window was accepted and
	// served. Once serving has begun the allocator now refuses every claim, and
	// keeps issuing from the SEALED floor rather than the rejected one.
	t.Run("StaleClaimAfterSealingIsRefusedAndTheFloorDoesNotMove", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 41})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		if n, err := s.NextSuffix("bob"); n != 42 || err != nil {
			t.Fatalf("NextSuffix(%q) after Seal() = (%d, %v), want (42, nil)", "bob", n, err)
		}

		// Every claim is refused, whether it is far too low, exactly equal to
		// what has been issued, higher, or the maximum. The sentinel does not
		// depend on how atLeast relates to the floor or to LastSuffix.
		for _, atLeast := range []uint64{0, 1, 41, 42, 100, math.MaxUint64} {
			err := s.RaiseFloor("bob", atLeast)
			if !errors.Is(err, ErrFloorSealed) {
				t.Fatalf("RaiseFloor(%q, %d) after Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", "bob", atLeast, err)
			}
			if last := s.LastSuffix("bob"); last != 42 {
				t.Fatalf("LastSuffix(%q) changed across a REJECTED RaiseFloor(%d): %d, want 42", "bob", atLeast, last)
			}
		}

		// The value, not just the error: issuing continues from the SEALED
		// floor. Had the 100 claim landed this would be 101; had MaxUint64
		// landed this would be ErrSuffixExhausted and the name would be dead.
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor calls: unexpected error %v", "bob", err)
		}
		if n != 43 {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor calls = %d, want 43 (the floor must NOT have moved to 101 or to MaxUint64)", "bob", n)
		}
	})

	// Refusals are not merely unsuccessful — they are INVISIBLE. An allocator
	// that was shouted at and one that was not must be indistinguishable.
	t.Run("RefusalsMutateNothing", func(t *testing.T) {
		floors := map[string]uint64{"bob": 41, "alice": 9}
		noisy := ResumeNameSuffixes(floors)
		quiet := ResumeNameSuffixes(floors)

		// N refused NextSuffix calls, while unsealed.
		for i := 0; i < 9; i++ {
			for _, name := range []string{"bob", "alice", "carol"} {
				if n, err := noisy.NextSuffix(name); n != 0 || !errors.Is(err, ErrFloorUnproven) {
					t.Fatalf("unsealed NextSuffix(%q) = (%d, %v), want (0, ErrFloorUnproven)", name, n, err)
				}
			}
		}
		for _, s := range []*NameSuffixes{noisy, quiet} {
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
		}
		// M refused RaiseFloor calls, now that it is sealed.
		for i := 0; i < 7; i++ {
			for _, name := range []string{"bob", "alice", "carol"} {
				if err := noisy.RaiseFloor(name, uint64(i)*1000); !errors.Is(err, ErrFloorSealed) {
					t.Fatalf("sealed RaiseFloor(%q, %d): err = %v, want errors.Is(err, ErrFloorSealed)", name, uint64(i)*1000, err)
				}
			}
		}

		for i := 0; i < 4; i++ {
			for _, name := range []string{"bob", "alice", "carol"} {
				wantN, wantErr := quiet.NextSuffix(name)
				if wantErr != nil {
					t.Fatalf("control NextSuffix(%q): unexpected error %v", name, wantErr)
				}
				gotN, gotErr := noisy.NextSuffix(name)
				if gotErr != nil {
					t.Fatalf("NextSuffix(%q) after refusals: unexpected error %v", name, gotErr)
				}
				if gotN != wantN {
					t.Fatalf("NextSuffix(%q) #%d after 27 refused NextSuffix and 21 refused RaiseFloor calls = %d, want %d (identical to an allocator that saw zero refusals)", name, i, gotN, wantN)
				}
				if got, want := noisy.LastSuffix(name), quiet.LastSuffix(name); got != want {
					t.Fatalf("LastSuffix(%q) after refusals = %d, want %d (identical to the control allocator)", name, got, want)
				}
			}
		}
	})

	// The wrap must not break the exported contract, and must not allocate.
	t.Run("UnsealedRefusalIsTheSharedSentinelAndIsNeverAllocatedPerCall", func(t *testing.T) {
		a := ResumeNameSuffixes(map[string]uint64{"bob": 41})
		b := ResumeNameSuffixes(nil)

		for i := 0; i < 3; i++ {
			for _, tc := range []struct {
				s    *NameSuffixes
				name string
			}{{a, "bob"}, {a, "alice"}, {b, "bob"}, {b, "zebra"}} {
				_, err := tc.s.NextSuffix(tc.name)
				// The EXPORTED contract: callers match the shared sentinel.
				if !errors.Is(err, ErrFloorUnproven) {
					t.Fatalf("unsealed NextSuffix(%q) error = %v, want errors.Is(err, ErrFloorUnproven)", tc.name, err)
				}
				// The unexported detail that makes it cheap and name-agnostic:
				// it is the SAME value every call, for every name and every
				// allocator, because an unproven floor is a whole-allocator
				// condition with nothing name-specific to interpolate.
				//
				// These two compare with == and not errors.Is on purpose:
				// VALUE IDENTITY is the property under test, and errors.Is
				// would be satisfied by a freshly allocated wrap every call.
				if err != errSuffixFloorUnproven {
					t.Fatalf("unsealed NextSuffix(%q) returned a different error value than the package-level errSuffixFloorUnproven; it must not allocate per call: %v", tc.name, err)
				}
				// It carries the AGENT-SUFFIX guidance, so it is a WRAP and not
				// the bare sentinel, and it is not any other sentinel.
				if err == ErrFloorUnproven {
					t.Fatalf("unsealed NextSuffix(%q) returned the bare ErrFloorUnproven; it must wrap it with the per-name suffix guidance", tc.name)
				}
				if errors.Is(err, ErrSuffixExhausted) || errors.Is(err, ErrFloorSealed) || errors.Is(err, ErrFloorBelowIssued) {
					t.Fatalf("unsealed NextSuffix(%q) error matched a sentinel other than ErrFloorUnproven: %v", tc.name, err)
				}
			}
		}
	})

	t.Run("ZeroValueFailsClosed", func(t *testing.T) {
		// An allocator reached by accident — a struct field nobody constructed —
		// must refuse rather than mint 1 for every name from floors nobody
		// derived, and must not panic on its nil maps on the way.
		//
		// NOT tested here, deliberately, and NOT an oversight: RaiseFloor on a
		// zero value is UNSEALED and therefore legal, and for ANY atLeast >= 1
		// it reaches the nil-map write and PANICS ("assignment to entry in nil
		// map"). Be exact about the boundary, because it is one call wide:
		// atLeast == 0 does NOT panic — the guard is `atLeast > s.floor[name]`,
		// 0 > 0 is false, no map write is attempted, and it returns nil. That
		// one case is pinned below; every larger value is left alone. The panic
		// is likewise reachable through NextSuffix AFTER a Seal on a zero value,
		// since the seal succeeds and the next call writes to the nil maps.
		// Fixing those nil-map panics is out of scope for this task; the
		// property it owns is that the UNSEALED refusal precedes every map
		// access, which is what these subtests pin.
		t.Run("var s NameSuffixes", func(t *testing.T) {
			var s NameSuffixes
			n, err := s.NextSuffix("bob")
			if n != 0 || !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("zero-value NameSuffixes.NextSuffix(%q) = (%d, %v), want (0, ErrFloorUnproven)", "bob", n, err)
			}
			if last := s.LastSuffix("bob"); last != 0 {
				t.Fatalf("zero-value NameSuffixes.LastSuffix(%q) = %d, want 0", "bob", last)
			}
		})

		t.Run("&NameSuffixes{}", func(t *testing.T) {
			s := &NameSuffixes{}
			for _, name := range []string{"bob", ""} {
				n, err := s.NextSuffix(name)
				if n != 0 || !errors.Is(err, ErrFloorUnproven) {
					t.Fatalf("&NameSuffixes{}.NextSuffix(%q) = (%d, %v), want (0, ErrFloorUnproven)", name, n, err)
				}
			}
		})

		// CHARACTERISATION of a documented-NOT-USABLE state, and emphatically
		// NOT an endorsement of using the zero value: construct with
		// NewNameSuffixes or ResumeNameSuffixes. It exists because the exact
		// fact it records is the one the prose got wrong — "RaiseFloor on a zero
		// value PANICS" was stated absolutely, and atLeast == 0 is the
		// counterexample. A nil-map READ is fine; only a WRITE panics, and
		// `atLeast > s.floor[name]` is false for 0, so no write is attempted.
		//
		// Only the NON-panicking half is asserted, on purpose. Pinning "atLeast
		// >= 1 panics" would CEMENT THE WART: a later nil-guard or lazy-map-init
		// fix would legitimately make that call succeed, and a test that failed
		// for that would be worse than no test. The assertion below SURVIVES
		// such a fix — a lazily-initialised zero value still returns nil here,
		// because 0 is still not greater than an absent floor of 0.
		t.Run("RaiseFloorAtLeastZeroOnAZeroValueReturnsNilWithoutPanicking", func(t *testing.T) {
			var value NameSuffixes
			for _, s := range []*NameSuffixes{&value, {}} {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("zero-value NameSuffixes.RaiseFloor(%q, 0) panicked: %v; atLeast 0 is never greater than the absent floor 0, so it must not reach the nil-map write", "bob", r)
						}
					}()
					if err := s.RaiseFloor("bob", 0); err != nil {
						t.Fatalf("zero-value NameSuffixes.RaiseFloor(%q, 0) = %v, want nil; the allocator is unsealed, nothing is issued, and no map write is attempted", "bob", err)
					}
				}()
			}
		})
	})

	// The seal is GLOBAL. While unsealed EVERY name is refused, whatever its
	// floor; one Seal unlocks ALL of them at once, including names the
	// derivation never saw.
	t.Run("SealIsGlobalNotPerName", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"high": 1_000_000})

		// "high" has a large DERIVED floor; "absent" has none at all. A
		// per-name seal would have to let "absent" mint from an unproven 0 —
		// exactly the collapse of "proven to be zero" into "never proven" that
		// this gate exists to prevent — so both must refuse identically.
		for _, name := range []string{"high", "absent"} {
			if n, err := s.NextSuffix(name); n != 0 || !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("unsealed NextSuffix(%q) = (%d, %v), want (0, ErrFloorUnproven); the seal is global, so a high derived floor is no more issuable than an absent one", name, n, err)
			}
		}

		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}

		// ONE Seal unlocked both. "absent" mints 1 only because the seal
		// asserted, out loud, that names absent from the floors were never
		// written to disk.
		if n, err := s.NextSuffix("high"); n != 1_000_001 || err != nil {
			t.Fatalf("NextSuffix(%q) after Seal() = (%d, %v), want (1000001, nil)", "high", n, err)
		}
		if n, err := s.NextSuffix("absent"); n != 1 || err != nil {
			t.Fatalf("NextSuffix(%q) after Seal() = (%d, %v), want (1, nil); a name absent from the floors is refused BEFORE the seal and mints 1 AFTER it", "absent", n, err)
		}
	})

	// The "did you break enrolment?" guard. A genuinely new agent enrolling on
	// a running bus is a name FIRST SEEN AFTER sealing, and it must still get an
	// id. If this test ever fails, the gate has stopped being about the
	// derivation window and started being about the name set.
	t.Run("NameFirstSeenAfterSealingStillEnrols", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 5})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for want := uint64(1); want <= 2; want++ {
			n, err := s.NextSuffix("carol")
			if err != nil {
				t.Fatalf("NextSuffix(%q) #%d on a name never seen during derivation: unexpected error %v", "carol", want, err)
			}
			if n != want {
				t.Fatalf("NextSuffix(%q) #%d = %d, want %d; enrolment of new names must keep working after the seal", "carol", want, n, want)
			}
		}
	})

	// GUARD ORDERING. Both new guards sit in front of an older check, and
	// swapping either pair back would still leave a test suite that only
	// asserted "some error" entirely green. These subtests name the exact
	// sentinel, and name the one that must NOT appear.
	t.Run("GuardOrdering", func(t *testing.T) {
		t.Run("UnsealedBeatsExhausted", func(t *testing.T) {
			// This allocator is exhausted for "bob" AND unsealed. The unsealed
			// check runs first: an allocator with no proven floor cannot
			// honestly claim a name is exhausted, because "exhausted" is a
			// statement about how far that floor has been carried. Reporting
			// exhaustion would tell an operator "this name is finished"
			// (unrecoverable) when the truth is "your derivation is broken"
			// (recoverable by fixing startup).
			s := ResumeNameSuffixes(map[string]uint64{"bob": math.MaxUint64})
			n, err := s.NextSuffix("bob")
			if n != 0 {
				t.Fatalf("NextSuffix(%q) on an unsealed MaxUint64 floor = %d, want 0", "bob", n)
			}
			if !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("NextSuffix(%q) on an unsealed MaxUint64 floor error = %v, want ErrFloorUnproven (the unsealed check runs BEFORE the exhaustion check)", "bob", err)
			}
			if errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) on an unsealed MaxUint64 floor reported ErrSuffixExhausted; the unsealed check must pre-empt it: %v", "bob", err)
			}

			// After the seal, the very same call reports exhaustion.
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			n, err = s.NextSuffix("bob")
			if n != 0 {
				t.Fatalf("NextSuffix(%q) on a sealed MaxUint64 floor = %d, want 0 (never wrap)", "bob", n)
			}
			if !errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) on a sealed MaxUint64 floor error = %v, want ErrSuffixExhausted", "bob", err)
			}
			if errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("NextSuffix(%q) on a SEALED allocator still reported ErrFloorUnproven: %v", "bob", err)
			}
		})

		t.Run("SealedBeatsBelowIssued", func(t *testing.T) {
			// A sealed allocator that has issued for a name satisfies BOTH
			// RaiseFloor guards. The seal check runs first because it is the
			// stronger statement: floor assembly is over, full stop — and it
			// holds for names this allocator has issued NOTHING for, which
			// would slip straight past the last != 0 guard.
			s := ResumeNameSuffixes(map[string]uint64{"bob": 10})
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			for i := 0; i < 3; i++ {
				if _, err := s.NextSuffix("bob"); err != nil {
					t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
				}
			}
			last := s.LastSuffix("bob")
			if last != 13 {
				t.Fatalf("LastSuffix(%q) = %d, want 13", "bob", last)
			}
			for _, atLeast := range []uint64{last, last - 1, 0} {
				err := s.RaiseFloor("bob", atLeast)
				if !errors.Is(err, ErrFloorSealed) {
					t.Fatalf("RaiseFloor(%q, %d) with LastSuffix()==%d on a sealed allocator: err = %v, want ErrFloorSealed", "bob", atLeast, last, err)
				}
				if errors.Is(err, ErrFloorBelowIssued) {
					t.Fatalf("RaiseFloor(%q, %d) reported ErrFloorBelowIssued; the seal check must pre-empt it: %v", "bob", atLeast, err)
				}
			}
			if got := s.LastSuffix("bob"); got != last {
				t.Fatalf("LastSuffix(%q) changed across rejected RaiseFloor calls: %d -> %d", "bob", last, got)
			}
		})
	})

	// NewNameSuffixes is born SEALED, and this subtest pins that.
	//
	// READ THIS BEFORE TREATING IT AS A PROPERTY TO DEFEND. The justification
	// that used to sit here is DEAD, and leaving it would have made this comment
	// the last surviving copy of a claim the rest of the package just corrected:
	// it said "NameSuffixes has a LIVE PRODUCTION CALLER — cmd/agent-bus/main.go
	// builds ids.NewNameSuffixes() on every start and every enrolment mints
	// through it". That caller is GONE. cmd/agent-bus constructs through
	// OpenNameSuffixes, and TestNoProductionCallerOfNewNameSuffixes now asserts
	// module-wide, by AST walk, that no production file references the fresh
	// constructor at all.
	//
	// So this subtest documents the CURRENT behaviour, not a desired one. It is
	// scheduled to be INVERTED — born-unsealed, for parity with NewSequence — by
	// MSG-FU-SUFFIXFLOOR-FU-UNSEAL (c), which could not land here because the
	// flip also needs internal/httpapi/{auth,authmw,authmw_internal,messages}_test.go
	// and internal/auth/auth_test.go, which mint through this constructor. When
	// that task runs, THIS subtest is one of the things it must change: it is
	// the in-package half of the flip, and a (c) scoped to only the other two
	// packages will fail here.
	t.Run("NewNameSuffixesIsBornSealed", func(t *testing.T) {
		s := NewNameSuffixes()

		// Mints immediately, with no Seal call anywhere.
		for want := uint64(1); want <= 3; want++ {
			n, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q) #%d on a fresh NewNameSuffixes(): unexpected error %v; this constructor is born SEALED because enrolment mints through it on a running bus", "bob", want, err)
			}
			if n != want {
				t.Fatalf("NextSuffix(%q) #%d = %d, want %d", "bob", want, n, want)
			}
		}

		// Sealing it is an ERROR, not a no-op: it means the caller derived
		// floors and handed them to the fresh-bus constructor, whose name
		// already claimed the disk was empty.
		if err := s.Seal(); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("Seal() on NewNameSuffixes(): err = %v, want errors.Is(err, ErrFloorSealed) (it is born sealed)", err)
		}

		// And this is the compensating property that makes the deviation safe:
		// a later floor derivation cannot be SILENTLY ABSORBED by a fresh-bus
		// allocator — it is refused loudly instead of quietly doing nothing.
		for _, name := range []string{"bob", "never-seen"} {
			if err := s.RaiseFloor(name, 9_000); !errors.Is(err, ErrFloorSealed) {
				t.Fatalf("RaiseFloor(%q, 9000) on NewNameSuffixes(): err = %v, want errors.Is(err, ErrFloorSealed)", name, err)
			}
		}
		if n, err := s.NextSuffix("bob"); n != 4 || err != nil {
			t.Fatalf("NextSuffix(%q) after refused RaiseFloor calls = (%d, %v), want (4, nil); the floor must NOT have moved to 9000", "bob", n, err)
		}
		if n, err := s.NextSuffix("never-seen"); n != 1 || err != nil {
			t.Fatalf("NextSuffix(%q) after a refused RaiseFloor = (%d, %v), want (1, nil)", "never-seen", n, err)
		}
	})

	t.Run("SecondSealIsRefusedAndChangesNothing", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 5})
		if err := s.Seal(); err != nil {
			t.Fatalf("first Seal(): %v, want nil", err)
		}
		if n, err := s.NextSuffix("bob"); n != 6 || err != nil {
			t.Fatalf("NextSuffix(%q) after first Seal() = (%d, %v), want (6, nil)", "bob", n, err)
		}

		// Two callers each believing they own the derivation is the bug, not a
		// duplicate no-op.
		if err := s.Seal(); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("second Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", err)
		}
		// A failed seal is not a state change: floors and issued values are
		// identical either side of it.
		if n, err := s.NextSuffix("bob"); n != 7 || err != nil {
			t.Fatalf("NextSuffix(%q) after a REJECTED second Seal() = (%d, %v), want (7, nil)", "bob", n, err)
		}
		if last := s.LastSuffix("bob"); last != 7 {
			t.Fatalf("LastSuffix(%q) = %d, want 7", "bob", last)
		}
		if n, err := s.NextSuffix("carol"); n != 1 || err != nil {
			t.Fatalf("NextSuffix(%q) after a REJECTED second Seal() = (%d, %v), want (1, nil); a refused Seal must not disturb the absent-name floors either", "carol", n, err)
		}
		// A third seal is refused identically — the state is one-way.
		if err := s.Seal(); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("third Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", err)
		}
	})

	// END-TO-END FAIL-CLOSED DIRECTION. A dropped or ignored gate must not be
	// able to become a live identity: this is the AgentIDMinter equivalent of
	// the Sequence -> MessageID link, one level up from the allocator.
	t.Run("MinterOnAnUnsealedAllocatorMintsNothing", func(t *testing.T) {
		alloc := ResumeNameSuffixes(map[string]uint64{"bob": 41})
		m, err := NewAgentIDMinter("bus-x", alloc)
		if err != nil {
			t.Fatalf("NewAgentIDMinter: unexpected error %v", err)
		}

		id, err := m.Mint("bob")
		if !errors.Is(err, ErrFloorUnproven) {
			t.Fatalf("Mint(%q) on an UNSEALED allocator: err = %v, want errors.Is(err, ErrFloorUnproven)", "bob", err)
		}
		if id != "" {
			t.Fatalf("Mint(%q) on an UNSEALED allocator = %q, want the empty id", "bob", id)
		}

		// And even a caller that IGNORED the error gets nothing usable: a
		// refused NextSuffix returns suffix 0, which AgentID (agentid.go:107)
		// and ParseAgentID (agentid.go:194) both reject outright, because 0 is
		// indistinguishable from an unset field. There is no path from a
		// refusal to a well-formed agent id.
		if got, aerr := AgentID("bus-x", "bob", 0); aerr == nil {
			t.Fatalf("AgentID(%q, %q, 0) = (%q, nil), want an error; suffix 0 is never allocated", "bus-x", "bob", got)
		}
		if _, _, _, perr := ParseAgentID("bus-x.bob-0"); perr == nil {
			t.Fatalf("ParseAgentID(%q) = nil error, want an error; suffix 0 is never allocated", "bus-x.bob-0")
		}

		// Sealed, the same minter works and resumes strictly above the floor.
		if err := alloc.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for want := uint64(42); want <= 43; want++ {
			id, err := m.Mint("bob")
			if err != nil {
				t.Fatalf("Mint(%q) after Seal(): unexpected error %v", "bob", err)
			}
			gotBus, gotName, gotN, perr := ParseAgentID(id)
			if perr != nil {
				t.Fatalf("ParseAgentID(%q): unexpected error %v", id, perr)
			}
			if gotBus != "bus-x" || gotName != "bob" || gotN != want {
				t.Fatalf("Mint(%q) after Seal() = %q, parsed as (%q, %q, %d), want (%q, %q, %d)", "bob", id, gotBus, gotName, gotN, "bus-x", "bob", want)
			}
		}
	})

	// CONCURRENCY. Goroutines hammer NextSuffix across several names on an
	// UNSEALED allocator while one goroutine seals it. Which calls land before
	// the seal and which after is pure scheduling, so NOTHING here asserts a
	// count or a ratio.
	//
	// Timing-INDEPENDENT (asserted): every error is exactly ErrFloorUnproven and
	// never a partial/torn result; no suffix is ever issued twice FOR A NAME;
	// every issued suffix is strictly greater than that name's floor;
	// LastSuffix(name) equals the maximum issued for that name, or 0 if none
	// were; and one final NextSuffix per name after the barrier continues from
	// max(floor, LastSuffix)+1 — i.e. the refused calls burned nothing.
	// Timing-DEPENDENT (deliberately NOT asserted): how many calls succeeded,
	// how many were refused, and which values landed. Zero successes and zero
	// failures are both legal outcomes of this test.
	t.Run("ConcurrentNextSuffixRacingSeal", func(t *testing.T) {
		const goroutines = 33
		const perGoroutine = 50

		// "carol" is deliberately absent from the floors: the seal unlocks it
		// at the same instant it unlocks the two derived names.
		floors := map[string]uint64{"bob": 0, "alice": 5_000}
		names := []string{"bob", "alice", "carol"}
		s := ResumeNameSuffixes(floors)

		var (
			mu        sync.Mutex
			issued    = map[string][]uint64{}
			refusals  int
			otherErrs []error
		)

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			name := names[g%len(names)]
			go func() {
				defer wg.Done()
				for i := 0; i < perGoroutine; i++ {
					n, err := s.NextSuffix(name)
					mu.Lock()
					switch {
					case err == nil:
						issued[name] = append(issued[name], n)
					case errors.Is(err, ErrFloorUnproven):
						refusals++
						if n != 0 {
							otherErrs = append(otherErrs, fmt.Errorf("refused NextSuffix(%q) returned n=%d, want 0", name, n))
						}
					default:
						otherErrs = append(otherErrs, err)
					}
					mu.Unlock()
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Seal(); err != nil {
				mu.Lock()
				otherErrs = append(otherErrs, fmt.Errorf("Seal(): %w, want nil (called exactly once)", err))
				mu.Unlock()
			}
		}()

		wg.Wait()

		if len(otherErrs) != 0 {
			t.Fatalf("unexpected errors during the race (only ErrFloorUnproven is legal here): %v", otherErrs)
		}

		total := 0
		maxFor := make(map[string]uint64, len(names))
		for _, name := range names {
			floor := floors[name]
			seen := make(map[uint64]bool, len(issued[name]))
			var max uint64
			for _, n := range issued[name] {
				if seen[n] {
					t.Fatalf("NextSuffix(%q) issued duplicate value %d while racing Seal()", name, n)
				}
				seen[n] = true
				if n <= floor {
					t.Fatalf("NextSuffix(%q) issued %d, which is not strictly greater than that name's floor %d", name, n, floor)
				}
				if n > max {
					max = n
				}
			}
			if got := s.LastSuffix(name); got != max {
				t.Fatalf("LastSuffix(%q) = %d, want %d (the maximum issued for that name, 0 if none were issued)", name, got, max)
			}
			maxFor[name] = max
			total += len(issued[name])
		}
		if refusals+total != goroutines*perGoroutine {
			t.Fatalf("accounted for %d+%d calls, want %d", total, refusals, goroutines*perGoroutine)
		}

		// The barrier has passed, so the allocator is definitely sealed: these
		// calls must succeed, and each must continue from its own name's floor —
		// proving that every refused call burned nothing, for every name.
		for _, name := range names {
			floor := floors[name]
			wantNext := floor + 1
			if maxFor[name] > floor {
				wantNext = maxFor[name] + 1
			}
			n, err := s.NextSuffix(name)
			if err != nil {
				t.Fatalf("NextSuffix(%q) after the seal barrier: unexpected error %v", name, err)
			}
			if n != wantNext {
				t.Fatalf("NextSuffix(%q) after the seal barrier = %d, want %d (%d refused calls across all names must have burned nothing)", name, n, wantNext, refusals)
			}
		}
	})
}

// TestNameSuffixesAllocator is the per-name analogue of TestSequenceAllocator
// and pins the same headline contract, one level further in: NewNameSuffixes
// starts every name at 1, ResumeNameSuffixes(floor) starts a name strictly
// above its floor, a name absent from the resume map is floor 0 (the same floor
// NewNameSuffixes gives that name), and LastSuffix reports what has been
// ISSUED for a name, not its floor.
//
// Since ID-2-WIRING-SEAL-FU-NAMESUFFIXES, every RESUMED allocator here is
// SEALED first — issuing at all requires it (see
// TestNameSuffixesRefusesToIssueFromAnUnsealedFloor). NewNameSuffixes is born
// sealed and must NOT be sealed again; that deviation is pinned in
// TestNameSuffixesRefusesToIssueFromAnUnsealedFloor/NewNameSuffixesIsBornSealed.
func TestNameSuffixesAllocator(t *testing.T) {
	t.Run("NewNameSuffixesIssuesFromOnePerName", func(t *testing.T) {
		s := NewNameSuffixes()
		if got := s.LastSuffix("bob"); got != 0 {
			t.Fatalf("LastSuffix(%q) before any NextSuffix = %d, want 0", "bob", got)
		}
		for want := uint64(1); want <= 5; want++ {
			got, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q) #%d: unexpected error %v", "bob", want, err)
			}
			if got != want {
				t.Fatalf("NextSuffix(%q) #%d = %d, want %d", "bob", want, got, want)
			}
			if last := s.LastSuffix("bob"); last != want {
				t.Fatalf("LastSuffix(%q) after issuing %d = %d, want %d", "bob", want, last, want)
			}
		}
	})

	t.Run("DistinctNamesHaveIndependentCounters", func(t *testing.T) {
		s := NewNameSuffixes()
		// Interleave two names and confirm neither affects the other's
		// sequence: this is the property that distinguishes NameSuffixes from
		// Sequence and is the whole reason it exists.
		for i := 1; i <= 5; i++ {
			gotBob, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q) #%d: unexpected error %v", "bob", i, err)
			}
			if uint64(i) != gotBob {
				t.Fatalf("NextSuffix(%q) #%d = %d, want %d", "bob", i, gotBob, i)
			}

			// alice gets TWO issued per iteration, to prove bob's counter is
			// unaffected by alice running ahead.
			for j := 0; j < 2; j++ {
				if _, err := s.NextSuffix("alice"); err != nil {
					t.Fatalf("NextSuffix(%q): unexpected error %v", "alice", err)
				}
			}
		}
		if got := s.LastSuffix("bob"); got != 5 {
			t.Fatalf("LastSuffix(%q) = %d, want 5", "bob", got)
		}
		if got := s.LastSuffix("alice"); got != 10 {
			t.Fatalf("LastSuffix(%q) = %d, want 10", "alice", got)
		}
		// A name never touched has its own independent, untouched counter.
		if got := s.LastSuffix("carol"); got != 0 {
			t.Fatalf("LastSuffix(%q) for an untouched name = %d, want 0", "carol", got)
		}
		n, err := s.NextSuffix("carol")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "carol", err)
		}
		if n != 1 {
			t.Fatalf("NextSuffix(%q) first call = %d, want 1", "carol", n)
		}
	})

	t.Run("ResumeIssuesAboveFloorPerName", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 41, "alice": 9})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for i := uint64(1); i <= 5; i++ {
			wantBob := 41 + i
			gotBob, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q) #%d: unexpected error %v", "bob", i, err)
			}
			if gotBob != wantBob {
				t.Fatalf("NextSuffix(%q) #%d = %d, want %d (floor 41 + %d)", "bob", i, gotBob, wantBob, i)
			}
		}
		wantAlice := uint64(10)
		gotAlice, err := s.NextSuffix("alice")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "alice", err)
		}
		if gotAlice != wantAlice {
			t.Fatalf("NextSuffix(%q) = %d, want %d (floor 9 + 1)", "alice", gotAlice, wantAlice)
		}
	})

	t.Run("ResumeNameAbsentFromMapIsFloorZero", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 100})
		// The seal is what makes floor 0 for an absent name a CLAIM ("never
		// written") rather than a default; see SealIsGlobalNotPerName.
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		// "dave" never appears in the resume map: it must behave exactly as
		// NewNameSuffixes would for that name, i.e. floor 0.
		n, err := s.NextSuffix("dave")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "dave", err)
		}
		if n != 1 {
			t.Fatalf("NextSuffix(%q) on an absent name = %d, want 1 (floor 0)", "dave", n)
		}
	})

	t.Run("ResumeNilOrEmptyMapDiffersFromNewOnlyBySeal", func(t *testing.T) {
		// Was ResumeNilOrEmptyMapEquivalentToNew, whose premise was that
		// ResumeNameSuffixes(nil) is "exactly equivalent to NewNameSuffixes".
		// That equivalence is precisely what this task DELETED: collapsing the
		// two is what let a FAILED derivation returning an empty map mint every
		// name from 1 without anyone noticing. So the assertion is now the
		// distinction — "I derived floor 0 from an empty disk" (seal it and say
		// so) versus "I am a fresh bus" (the constructor's name is the claim) —
		// plus the part that still holds: once sealed, the FLOORS agree exactly.
		for _, m := range []map[string]uint64{nil, {}} {
			// They differ in the one way that matters. A probe allocator shows
			// NewNameSuffixes is born SEALED — it issues immediately and cannot
			// be sealed again — while the resumed one is born UNSEALED and
			// refuses, even for a nil or empty map.
			probe := NewNameSuffixes()
			if n, err := probe.NextSuffix("bob"); n != 1 || err != nil {
				t.Fatalf("NewNameSuffixes().NextSuffix(%q) = (%d, %v), want (1, nil); it is born SEALED", "bob", n, err)
			}
			if err := probe.Seal(); !errors.Is(err, ErrFloorSealed) {
				t.Fatalf("NewNameSuffixes().Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", err)
			}

			resumed := ResumeNameSuffixes(m)
			if n, err := resumed.NextSuffix("bob"); n != 0 || !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("ResumeNameSuffixes(%v).NextSuffix(%q) = (%d, %v), want (0, ErrFloorUnproven); it is born UNSEALED even for an empty map", m, "bob", n, err)
			}
			if err := resumed.Seal(); err != nil {
				t.Fatalf("ResumeNameSuffixes(%v).Seal(): %v, want nil", m, err)
			}

			// And they agree on the FLOORS: from a pristine fresh allocator,
			// step for step. The refused call above burned nothing, so the
			// resumed one still starts at 1.
			fresh := NewNameSuffixes()
			for i := 0; i < 5; i++ {
				wantN, wantErr := fresh.NextSuffix("bob")
				gotN, gotErr := resumed.NextSuffix("bob")
				if gotN != wantN || !suffixErrorsEqual(gotErr, wantErr) {
					t.Fatalf("sealed ResumeNameSuffixes(%v).NextSuffix(%q) #%d = (%d, %v), want (%d, %v) matching NewNameSuffixes()", m, "bob", i, gotN, gotErr, wantN, wantErr)
				}
			}
			if got, want := resumed.LastSuffix("bob"), fresh.LastSuffix("bob"); got != want {
				t.Fatalf("sealed ResumeNameSuffixes(%v).LastSuffix(%q) = %d, want %d (equal to NewNameSuffixes())", m, "bob", got, want)
			}
		}
	})

	t.Run("ResumeMapIsCopiedNotAliased", func(t *testing.T) {
		// The doc promises the map is copied: mutating the caller's map after
		// construction must not affect the allocator's floors.
		floors := map[string]uint64{"bob": 10}
		s := ResumeNameSuffixes(floors)
		floors["bob"] = 999
		floors["new-name"] = 500
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}

		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
		}
		if n != 11 {
			t.Fatalf("NextSuffix(%q) = %d, want 11; mutating the caller's map after ResumeNameSuffixes must not change the floor", "bob", n)
		}

		n, err = s.NextSuffix("new-name")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "new-name", err)
		}
		if n != 1 {
			t.Fatalf("NextSuffix(%q) = %d, want 1; a name added to the caller's map post-construction must not appear in the allocator", "new-name", n)
		}
	})

	t.Run("LastSuffixZeroBeforeFirstNextOnResume", func(t *testing.T) {
		// The subtle bit, mirrored from Sequence: a resumed name has a
		// non-zero floor but has ISSUED nothing yet, so LastSuffix must still
		// report 0 until the first NextSuffix for that name.
		s := ResumeNameSuffixes(map[string]uint64{"bob": 1000})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		if got := s.LastSuffix("bob"); got != 0 {
			t.Fatalf("LastSuffix(%q) on a fresh resume before any NextSuffix = %d, want 0 (floor is not \"issued\")", "bob", got)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
		}
		if n != 1001 {
			t.Fatalf("NextSuffix(%q) after resume(1000) = %d, want 1001", "bob", n)
		}
		if got := s.LastSuffix("bob"); got != 1001 {
			t.Fatalf("LastSuffix(%q) after first NextSuffix = %d, want 1001", "bob", got)
		}
	})
}

// suffixErrorsEqual compares two NextSuffix errors for the
// sealed-Resume(nil/empty) vs NewNameSuffixes() floor-agreement test: both nil,
// or both the same NameSuffixes sentinel. It deliberately compares by SENTINEL
// and not by text, because NextSuffix's unproven-floor error is a wrap carrying
// per-name suffix guidance while Sequence's is the bare shared sentinel.
func suffixErrorsEqual(a, b error) bool {
	if a == nil && b == nil {
		return true
	}
	for _, sentinel := range []error{ErrSuffixExhausted, ErrFloorUnproven, ErrFloorSealed} {
		if errors.Is(a, sentinel) && errors.Is(b, sentinel) {
			return true
		}
	}
	return false
}

// TestNameSuffixesConcurrentNextSameName reproduces the reviewer's measured
// concurrency proof for a single name: 8 goroutines issuing 5000 suffixes
// each must produce 40,000 distinct values with zero duplicates, and the
// union must be exactly the contiguous set floor+1..floor+40000. Run with
// -race — this is a plain sync.Mutex, so a race here would mean the lock
// itself is not doing its job.
func TestNameSuffixesConcurrentNextSameName(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 5000
	const total = goroutines * perGoroutine
	const name = "worker"

	for _, floor := range []uint64{0, 1_000_000} {
		floor := floor
		t.Run(fmt.Sprintf("floor=%d", floor), func(t *testing.T) {
			// Sealed BEFORE any goroutine starts: this test is about
			// NextSuffix racing NextSuffix, not about NextSuffix racing Seal
			// (that race is covered by
			// TestNameSuffixesRefusesToIssueFromAnUnsealedFloor/ConcurrentNextSuffixRacingSeal).
			// NewNameSuffixes is born sealed and must not be sealed again.
			var s *NameSuffixes
			if floor == 0 {
				s = NewNameSuffixes()
			} else {
				s = ResumeNameSuffixes(map[string]uint64{name: floor})
				if err := s.Seal(); err != nil {
					t.Fatalf("Seal(): %v, want nil", err)
				}
			}

			results := make(chan uint64, total)
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				go func() {
					defer wg.Done()
					for i := 0; i < perGoroutine; i++ {
						n, err := s.NextSuffix(name)
						if err != nil {
							t.Errorf("NextSuffix(%q): unexpected error %v", name, err)
							continue
						}
						results <- n
					}
				}()
			}
			wg.Wait()
			close(results)

			seen := make(map[uint64]bool, total)
			var max uint64
			for n := range results {
				if seen[n] {
					t.Fatalf("NextSuffix(%q) issued duplicate value %d", name, n)
				}
				seen[n] = true
				if n > max {
					max = n
				}
			}
			if len(seen) != total {
				t.Fatalf("got %d unique issued values for %q, want %d", len(seen), name, total)
			}
			for i := uint64(1); i <= uint64(total); i++ {
				want := floor + i
				if !seen[want] {
					t.Fatalf("issued set for %q is missing %d; union must be exactly the contiguous set %d..%d", name, want, floor+1, floor+total)
				}
			}
			if got := s.LastSuffix(name); got != max {
				t.Fatalf("LastSuffix(%q) = %d, want max issued value %d", name, got, max)
			}
		})
	}
}

// TestNameSuffixesConcurrentNextAcrossNames runs several goroutines each
// hammering a DIFFERENT name concurrently, so a race that leaked state
// between names (a shared counter instead of per-name ones, or a map write
// racing a map write for another key) would show up under -race even though
// TestNameSuffixesConcurrentNextSameName only ever touches one key.
func TestNameSuffixesConcurrentNextAcrossNames(t *testing.T) {
	names := []string{"alpha", "bravo", "charlie", "delta"}
	const perName = 2000

	s := NewNameSuffixes()
	var wg sync.WaitGroup
	results := make([][]uint64, len(names))
	var mu sync.Mutex

	wg.Add(len(names))
	for idx, name := range names {
		idx, name := idx, name
		go func() {
			defer wg.Done()
			local := make([]uint64, 0, perName)
			for i := 0; i < perName; i++ {
				n, err := s.NextSuffix(name)
				if err != nil {
					t.Errorf("NextSuffix(%q): unexpected error %v", name, err)
					continue
				}
				local = append(local, n)
			}
			mu.Lock()
			results[idx] = local
			mu.Unlock()
		}()
	}
	wg.Wait()

	for idx, name := range names {
		seen := make(map[uint64]bool, perName)
		for _, n := range results[idx] {
			if seen[n] {
				t.Fatalf("NextSuffix(%q) issued duplicate value %d", name, n)
			}
			seen[n] = true
		}
		if len(seen) != perName {
			t.Fatalf("name %q got %d unique values, want %d", name, len(seen), perName)
		}
		for i := uint64(1); i <= perName; i++ {
			if !seen[i] {
				t.Fatalf("name %q issued set is missing %d; each name's counter must be independent and contiguous from 1", name, i)
			}
		}
		if got := s.LastSuffix(name); got != perName {
			t.Fatalf("LastSuffix(%q) = %d, want %d", name, got, perName)
		}
	}
}

// TestNameSuffixesOverflow covers the documented per-name overflow contract:
// at math.MaxUint64 NextSuffix returns (0, ErrSuffixExhausted) for THAT NAME,
// never wraps to 0, and stays exhausted for that name on every subsequent
// call — while leaving every other name unaffected. Reaching that check at all
// requires a Seal — the unsealed guard runs first, which is asserted in
// TestNameSuffixesRefusesToIssueFromAnUnsealedFloor/GuardOrdering/UnsealedBeatsExhausted.
func TestNameSuffixesOverflow(t *testing.T) {
	t.Run("ResumeAtMaxIsImmediatelyExhausted", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": math.MaxUint64})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for i := 0; i < 3; i++ {
			n, err := s.NextSuffix("bob")
			if n != 0 {
				t.Fatalf("NextSuffix(%q) call #%d = %d, want 0 (must never wrap)", "bob", i, n)
			}
			if !errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) call #%d error = %v, want errors.Is(err, ErrSuffixExhausted)", "bob", i, err)
			}
		}
		// A different name must be entirely unaffected by bob's exhaustion.
		n, err := s.NextSuffix("alice")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v; another name's exhaustion must not leak", "alice", err)
		}
		if n != 1 {
			t.Fatalf("NextSuffix(%q) = %d, want 1", "alice", n)
		}
	})

	t.Run("ResumeOneBelowMaxIssuesMaxThenExhausts", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": math.MaxUint64 - 1})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) at MaxUint64-1: unexpected error %v", "bob", err)
		}
		if n != math.MaxUint64 {
			t.Fatalf("NextSuffix(%q) = %d, want math.MaxUint64 (%d)", "bob", n, uint64(math.MaxUint64))
		}
		if last := s.LastSuffix("bob"); last != math.MaxUint64 {
			t.Fatalf("LastSuffix(%q) = %d, want math.MaxUint64", "bob", last)
		}

		for i := 0; i < 3; i++ {
			n, err := s.NextSuffix("bob")
			if n != 0 {
				t.Fatalf("NextSuffix(%q) after exhaustion call #%d = %d, want 0", "bob", i, n)
			}
			if !errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) after exhaustion call #%d error = %v, want errors.Is(err, ErrSuffixExhausted)", "bob", i, err)
			}
		}
	})
}

// TestNameSuffixesRaiseFloor encodes the documented boundary between
// RaiseFloor's no-op and error cases exactly as specified in the doc comment,
// per name — this is invariant 1's single most important pin: a resumed floor
// must NEVER re-issue a suffix at or below it.
//
// Since ID-2-WIRING-SEAL-FU-NAMESUFFIXES that boundary is drawn by the SEAL —
// which is GLOBAL, not per name — and no longer by whether anything has been
// issued for the name: RaiseFloor is legal only while unsealed, and "something
// has been issued" implies "sealed", so ErrFloorBelowIssued is now unreachable
// through the exported API of NameSuffixes as well as of Sequence. It survives
// in the code as defence-in-depth, and the last subtest here reaches it
// WHITE-BOX so that the backstop is at least executed once — see
// WhiteBoxOnlyErrFloorBelowIssuedBackstopUnreachableThroughTheExportedAPI.
func TestNameSuffixesRaiseFloor(t *testing.T) {
	t.Run("UnsealedAtLeastAtOrBelowFloorIsNoOp", func(t *testing.T) {
		// Was NothingIssuedAtLeastAtOrBelowFloorIsNoOp. The behaviour is
		// unchanged; only the predicate that names the window is — "nothing
		// issued" became "unsealed", which is the stronger statement.
		s := ResumeNameSuffixes(map[string]uint64{"bob": 10})
		if err := s.RaiseFloor("bob", 10); err != nil {
			t.Fatalf("RaiseFloor(%q, 10) on floor 10 while UNSEALED: %v, want nil (no-op)", "bob", err)
		}
		if err := s.RaiseFloor("bob", 5); err != nil {
			t.Fatalf("RaiseFloor(%q, 5) on floor 10 while UNSEALED: %v, want nil (no-op)", "bob", err)
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after no-op RaiseFloor calls: unexpected error %v", "bob", err)
		}
		if n != 11 {
			t.Fatalf("NextSuffix(%q) after no-op RaiseFloor calls = %d, want 11 (floor of 10 unchanged)", "bob", n)
		}
	})

	t.Run("UnsealedAtLeastAboveFloorRaises", func(t *testing.T) {
		// Was NothingIssuedAtLeastAboveFloorRaises; same inversion of predicate.
		s := ResumeNameSuffixes(map[string]uint64{"bob": 10})
		if err := s.RaiseFloor("bob", 20); err != nil {
			t.Fatalf("RaiseFloor(%q, 20) on floor 10 while UNSEALED: %v, want nil", "bob", err)
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(20): unexpected error %v", "bob", err)
		}
		if n != 21 {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(20) = %d, want 21", "bob", n)
		}
	})

	t.Run("UnsealedRaisesFromSeveralSourcesInAnyOrderPerName", func(t *testing.T) {
		// The "raises" behaviour where it is still legal: the assembly window,
		// before Seal. Each name's floor is the MAXIMUM of the claims made
		// about THAT name, and the order they arrive in does not matter.
		s := ResumeNameSuffixes(nil)
		for _, tc := range []struct {
			name    string
			atLeast uint64
		}{
			{"bob", 7}, {"alice", 3}, {"bob", 42}, {"alice", 1},
			{"bob", 1}, {"alice", 3}, {"bob", 42},
		} {
			if err := s.RaiseFloor(tc.name, tc.atLeast); err != nil {
				t.Fatalf("RaiseFloor(%q, %d) while unsealed: %v, want nil", tc.name, tc.atLeast, err)
			}
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for _, tc := range []struct {
			name string
			want uint64
		}{{"bob", 43}, {"alice", 4}} {
			n, err := s.NextSuffix(tc.name)
			if err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", tc.name, err)
			}
			if n != tc.want {
				t.Fatalf("NextSuffix(%q) after raising to a per-name maximum = %d, want %d", tc.name, n, tc.want)
			}
		}
	})

	t.Run("SealedRaiseFloorAtLastSuffixIsErrFloorSealed", func(t *testing.T) {
		// Was SomethingIssuedAtLeastAtLastIsError, expecting ErrFloorBelowIssued
		// for the equality case. To have issued anything the allocator must be
		// sealed, and the seal check runs FIRST, so the equality case now
		// reports ErrFloorSealed. ErrFloorBelowIssued is unreachable here.
		s := ResumeNameSuffixes(map[string]uint64{"bob": 0})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		var last uint64
		for i := 0; i < 3; i++ {
			n, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
			last = n
		}
		if err := s.RaiseFloor("bob", last); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(%q, %d) with LastSuffix()==%d on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", "bob", last, last, err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor: unexpected error %v", "bob", err)
		}
		if n != last+1 {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged) — a suffix at or below the floor must NEVER be reissued", "bob", last, n, last+1)
		}
	})

	t.Run("SealedRaiseFloorBelowLastSuffixIsErrFloorSealed", func(t *testing.T) {
		// Was SomethingIssuedAtLeastBelowLastIsError, expecting
		// ErrFloorBelowIssued — same inversion as above: the seal pre-empts it.
		s := ResumeNameSuffixes(map[string]uint64{"bob": 0})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		var last uint64
		for i := 0; i < 5; i++ {
			n, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
			last = n
		}
		if err := s.RaiseFloor("bob", last-2); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(%q, %d) with LastSuffix()==%d on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", "bob", last-2, last, err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor: unexpected error %v", "bob", err)
		}
		if n != last+1 {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged)", "bob", last-2, n, last+1)
		}
	})

	t.Run("SealedRaiseFloorAboveLastSuffixIsRefusedAndFloorDoesNotMove", func(t *testing.T) {
		// Was SomethingIssuedAtLeastAboveLastRaises, which asserted the floor
		// MOVED to 10 and the next suffix was 11. That premise inverts under the
		// seal gate: assembly ended at Seal, so a later claim about a name's
		// high-water mark — even a higher one — is refused and the floor stays
		// exactly where it was.
		s := ResumeNameSuffixes(map[string]uint64{"bob": 0})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for i := 0; i < 3; i++ {
			if _, err := s.NextSuffix("bob"); err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
		}
		// LastSuffix("bob") == 3 and its floor == 3 here.
		if err := s.RaiseFloor("bob", 10); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(%q, 10) with LastSuffix()==3 on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", "bob", err)
		}
		if got := s.LastSuffix("bob"); got != 3 {
			t.Fatalf("LastSuffix(%q) after rejected RaiseFloor(10) = %d, want 3", "bob", got)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(10): unexpected error %v", "bob", err)
		}
		if n != 4 {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(10) = %d, want 4 (the floor must NOT have moved to 10)", "bob", n)
		}
	})

	t.Run("RaiseFloorTouchesOnlyTheOneName", func(t *testing.T) {
		// Unchanged in substance; it just has to happen inside the assembly
		// window now, since RaiseFloor is legal only while unsealed.
		s := ResumeNameSuffixes(nil)
		if err := s.RaiseFloor("bob", 500); err != nil {
			t.Fatalf("RaiseFloor(%q, 500) while unsealed: %v, want nil", "bob", err)
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		// alice's counter must be untouched by raising bob's floor.
		for want := uint64(1); want <= 2; want++ {
			n, err := s.NextSuffix("alice")
			if err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "alice", err)
			}
			if n != want {
				t.Fatalf("NextSuffix(%q) #%d after RaiseFloor(%q, 500) = %d, want %d; RaiseFloor must touch only the named counter", "alice", want, "bob", n, want)
			}
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
		}
		if n != 501 {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(%q, 500) = %d, want 501", "bob", "bob", n)
		}
	})

	t.Run("RaiseFloorToMaxSucceedsAndExhausts", func(t *testing.T) {
		t.Run("unsealed, nothing issued yet", func(t *testing.T) {
			// Raising a name to MaxUint64 is still legal during assembly, and
			// leaves that name exhausted. Note this is also the DoS noted in
			// the RaiseFloor doc: a peer's claim is untrusted input and must be
			// bounded before it reaches here.
			s := ResumeNameSuffixes(nil)
			if err := s.RaiseFloor("bob", math.MaxUint64); err != nil {
				t.Fatalf("RaiseFloor(%q, MaxUint64) while unsealed: %v, want nil", "bob", err)
			}
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			n, err := s.NextSuffix("bob")
			if n != 0 || !errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) after RaiseFloor(MaxUint64) = (%d, %v), want (0, ErrSuffixExhausted)", "bob", n, err)
			}
			// Only that name: the exhaustion must not leak.
			if n, err := s.NextSuffix("alice"); n != 1 || err != nil {
				t.Fatalf("NextSuffix(%q) = (%d, %v), want (1, nil)", "alice", n, err)
			}
		})

		t.Run("something already issued (now refused)", func(t *testing.T) {
			// Was: RaiseFloor(MaxUint64) succeeded after a NextSuffix and left
			// the name exhausted. Inverted by the seal gate — issuing requires a
			// seal, so this RaiseFloor is refused with ErrFloorSealed (NOT
			// ErrFloorBelowIssued) and the name carries on from where it was.
			s := ResumeNameSuffixes(map[string]uint64{"bob": 0})
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			if _, err := s.NextSuffix("bob"); err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
			if err := s.RaiseFloor("bob", math.MaxUint64); !errors.Is(err, ErrFloorSealed) {
				t.Fatalf("RaiseFloor(%q, MaxUint64) with LastSuffix()==1 on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", "bob", err)
			}
			n, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(MaxUint64): unexpected error %v", "bob", err)
			}
			if n != 2 {
				t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(MaxUint64) = %d, want 2 (not exhausted: the floor never moved)", "bob", n)
			}
		})
	})

	// THE STATE BUILT HERE CANNOT ARISE THROUGH THE EXPORTED API, and that is
	// the entire point of the subtest. last[name] != 0 requires a successful
	// NextSuffix, NextSuffix requires the allocator to be SEALED, and
	// RaiseFloor's sealed check returns ErrFloorSealed before the below-issued
	// check is ever consulted — so "unsealed, but something recorded as issued"
	// is unreachable from outside the package, which makes the
	// ErrFloorBelowIssued branch structurally dead through the exported API.
	//
	// It is kept as DEFENCE IN DEPTH: the whole one-way state machine rests on a
	// single bool, and this is the check that stops a stale floor landing on top
	// of already-issued suffixes if that bool ever stops being one-way. A
	// backstop nobody has ever executed is a weak backstop, so this test — which
	// is IN-PACKAGE and can therefore reach the unexported fields — constructs
	// the impossible state deliberately and runs the branch.
	//
	// IF THIS BRANCH EVER BECOMES REACHABLE THROUGH THE EXPORTED API — if any
	// sequence of ResumeNameSuffixes/NewNameSuffixes/Seal/NextSuffix/RaiseFloor
	// calls can produce ErrFloorBelowIssued — then the one-way seal has been
	// BROKEN, and that is the bug to chase; this test is not the thing to
	// "fix". It is complementary to sequence_test.go, which instead SYNTHESISES
	// the sentinel with fmt.Errorf for its distinctness matrix and does not
	// execute this code at all.
	t.Run("WhiteBoxOnlyErrFloorBelowIssuedBackstopUnreachableThroughTheExportedAPI", func(t *testing.T) {
		// atLeast far below, just below, and exactly at what was issued — the
		// equality case included deliberately: a caller whose view merely
		// matches ours has learned nothing new, and accepting it would be the
		// silent no-op that hides an off-by-one.
		for _, atLeast := range []uint64{0, 1, 4, 5} {
			s := &NameSuffixes{
				floor: map[string]uint64{"bob": 5},
				last:  map[string]uint64{"bob": 5},
			}
			err := s.RaiseFloor("bob", atLeast)
			if !errors.Is(err, ErrFloorBelowIssued) {
				t.Fatalf("white-box RaiseFloor(%q, %d) on an UNSEALED allocator with last==5: err = %v, want errors.Is(err, ErrFloorBelowIssued); the defence-in-depth branch must actually fire", "bob", atLeast, err)
			}
			// It is that sentinel and NO other. The two conditions must stay
			// distinguishable: "assembly is over" (ErrFloorSealed) is a
			// different diagnosis from "the seal is gone AND a stale floor
			// arrived on top of issued suffixes", and this allocator is
			// deliberately unsealed.
			if errors.Is(err, ErrFloorSealed) {
				t.Fatalf("white-box RaiseFloor(%q, %d) also matched ErrFloorSealed on an UNSEALED allocator: %v", "bob", atLeast, err)
			}
			if errors.Is(err, ErrFloorUnproven) || errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("white-box RaiseFloor(%q, %d) matched a sentinel other than ErrFloorBelowIssued: %v", "bob", atLeast, err)
			}
			// The error reports a broken CALLER; it must not damage the
			// counter. This is the assertion that makes the backstop worth
			// keeping: a rejection that had already moved the floor would be no
			// backstop at all.
			if got := s.floor["bob"]; got != 5 {
				t.Fatalf("white-box RaiseFloor(%q, %d) moved the floor to %d, want 5 (a rejected claim changes nothing)", "bob", atLeast, got)
			}
			if got := s.LastSuffix("bob"); got != 5 {
				t.Fatalf("white-box RaiseFloor(%q, %d) changed LastSuffix to %d, want 5", "bob", atLeast, got)
			}
		}

		// The boundary, from the same impossible state: a claim STRICTLY ABOVE
		// what was issued is not what this branch is for, so it is accepted and
		// the floor moves. That pins the comparison as `atLeast <= last` rather
		// than "any claim at all once something is issued".
		s := &NameSuffixes{
			floor: map[string]uint64{"bob": 5},
			last:  map[string]uint64{"bob": 5},
		}
		if err := s.RaiseFloor("bob", 6); err != nil {
			t.Fatalf("white-box RaiseFloor(%q, 6) with last==5 on an UNSEALED allocator: %v, want nil (6 is strictly above what was issued)", "bob", err)
		}
		if got := s.floor["bob"]; got != 6 {
			t.Fatalf("white-box RaiseFloor(%q, 6) left the floor at %d, want 6", "bob", got)
		}
	})
}

// TestNameSuffixesRaiseFloorRace covers RaiseFloor racing under -race, in the
// two halves the seal gate leaves.
//
// Before ID-2-WIRING-SEAL-FU-NAMESUFFIXES this test ran RaiseFloor against
// NextSuffix and DISCARDED every RaiseFloor error with a bare `_ =`, on the
// theory that a failure "just means LastSuffix has already passed atLeast". That
// race no longer exists on a live allocator — NextSuffix requires a seal and
// RaiseFloor is refused after one, so the two can never both succeed — and an
// ignored error would have hidden the gate entirely. Rather than let the test
// decay into asserting nothing, it is split, and both halves now CHECK the
// error:
//
//	SEALED  — refused RaiseFloor calls racing with NextSuffix across several
//	          names must corrupt nothing, and the issued set per name must be
//	          exactly the expected contiguous range.
//	UNSEALED — concurrent per-name RaiseFloor calls racing the single Seal, which
//	          is the one RaiseFloor race that is still legal.
func TestNameSuffixesRaiseFloorRace(t *testing.T) {
	names := []string{"bob", "alice"}

	t.Run("SealedRefusedRaiseFloorNeverCorruptsNextSuffix", func(t *testing.T) {
		const nextGoroutines = 30
		const perGoroutine = 200
		const raiseGoroutines = 5
		const perName = nextGoroutines / 2 * perGoroutine

		s := ResumeNameSuffixes(nil)
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}

		results := make(chan struct {
			name string
			n    uint64
		}, nextGoroutines*perGoroutine)
		var wg sync.WaitGroup

		wg.Add(nextGoroutines)
		for g := 0; g < nextGoroutines; g++ {
			name := names[g%len(names)]
			go func() {
				defer wg.Done()
				for i := 0; i < perGoroutine; i++ {
					n, err := s.NextSuffix(name)
					if err != nil {
						// Sealed, and bounded well below math.MaxUint64, so
						// there is no legal error here at all.
						t.Errorf("NextSuffix(%q): unexpected error %v", name, err)
						continue
					}
					results <- struct {
						name string
						n    uint64
					}{name, n}
				}
			}()
		}

		wg.Add(raiseGoroutines)
		for r := 0; r < raiseGoroutines; r++ {
			r := r
			go func() {
				defer wg.Done()
				for i := 1; i <= 20; i++ {
					atLeast := uint64(r*1000 + i*10)
					for _, name := range names {
						// Every one of these is refused: assembly ended at
						// Seal. The error is deterministic — it does not
						// depend on how far concurrent NextSuffix calls have
						// got for that name.
						if err := s.RaiseFloor(name, atLeast); !errors.Is(err, ErrFloorSealed) {
							t.Errorf("RaiseFloor(%q, %d) on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", name, atLeast, err)
						}
					}
				}
			}()
		}

		wg.Wait()
		close(results)

		seen := make(map[string]map[uint64]bool, len(names))
		max := make(map[string]uint64, len(names))
		for _, name := range names {
			seen[name] = make(map[uint64]bool)
		}
		for r := range results {
			if seen[r.name][r.n] {
				t.Fatalf("NextSuffix(%q) issued duplicate value %d under RaiseFloor race", r.name, r.n)
			}
			seen[r.name][r.n] = true
			if r.n > max[r.name] {
				max[r.name] = r.n
			}
		}
		for _, name := range names {
			if got := s.LastSuffix(name); got != max[name] {
				t.Fatalf("LastSuffix(%q) = %d, want max issued value %d", name, got, max[name])
			}
			// No floor ever moved, so each name's issued set is exactly
			// 1..perName. This is the strong form of "a refused RaiseFloor
			// changed nothing": had any of the 200 refused calls leaked
			// through, there would be a hole here.
			if len(seen[name]) != perName {
				t.Fatalf("name %q got %d unique issued values, want %d", name, len(seen[name]), perName)
			}
			for i := uint64(1); i <= perName; i++ {
				if !seen[name][i] {
					t.Fatalf("issued set for %q is missing %d; a refused RaiseFloor must not move that name's floor, so the set must be exactly 1..%d", name, i, perName)
				}
			}
		}
	})

	t.Run("UnsealedConcurrentRaiseFloorRacingSeal", func(t *testing.T) {
		// The race that IS still legal: several sources push per-name claims
		// into the assembly window while one goroutine closes it. Which claims
		// land before the seal is pure scheduling, so the exact final floors are
		// NOT asserted. The invariant that matters is timing-independent: every
		// RaiseFloor that returned nil promised that name's floor is at least
		// atLeast, and the sealed floors must honour every one of them.
		const raiseGoroutines = 8
		const perGoroutine = 40

		s := ResumeNameSuffixes(nil)

		var (
			mu      sync.Mutex
			claimed = map[string][]uint64{} // atLeast values whose RaiseFloor returned nil
		)

		var wg sync.WaitGroup
		wg.Add(raiseGoroutines)
		for r := 0; r < raiseGoroutines; r++ {
			r := r
			go func() {
				defer wg.Done()
				for i := 1; i <= perGoroutine; i++ {
					atLeast := uint64(r*100 + i)
					for _, name := range names {
						err := s.RaiseFloor(name, atLeast)
						switch {
						case err == nil:
							mu.Lock()
							claimed[name] = append(claimed[name], atLeast)
							mu.Unlock()
						case errors.Is(err, ErrFloorSealed):
							// Legal: this call lost the race to Seal.
						default:
							t.Errorf("RaiseFloor(%q, %d) while assembling: err = %v, want nil or ErrFloorSealed", name, atLeast, err)
						}
					}
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Seal(); err != nil {
				t.Errorf("Seal(): %v, want nil (called exactly once)", err)
			}
		}()

		wg.Wait()

		for _, name := range names {
			// Nothing was issued during assembly, so LastSuffix is still 0.
			if got := s.LastSuffix(name); got != 0 {
				t.Fatalf("LastSuffix(%q) after an assembly-only race = %d, want 0 (nothing was issued)", name, got)
			}
			// The seal has happened (it is inside the WaitGroup), so this
			// NextSuffix succeeds and reveals that name's sealed floor as n-1.
			n, err := s.NextSuffix(name)
			if err != nil {
				t.Fatalf("NextSuffix(%q) after the seal barrier: unexpected error %v", name, err)
			}
			floor := n - 1
			for _, atLeast := range claimed[name] {
				if floor < atLeast {
					t.Fatalf("sealed floor for %q = %d, but RaiseFloor(%q, %d) returned nil; the sealed floor must be >= every accepted claim", name, floor, name, atLeast)
				}
			}
		}
	})
}

// fakeSuffixAllocator is a SuffixAllocator test double that lets Mint's
// argument-passing and error-propagation be tested independently of a real
// NameSuffixes: it records every name it was asked for and returns
// caller-controlled results.
type fakeSuffixAllocator struct {
	mu      sync.Mutex
	calls   []string
	next    uint64
	nextErr error
}

func (f *fakeSuffixAllocator) NextSuffix(name string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.next, f.nextErr
}

func (f *fakeSuffixAllocator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestNewAgentIDMinter covers the two documented construction failures: a
// malformed bus id, and a nil allocator. Both must fail at construction
// rather than at the first Mint, and a nil allocator must be rejected rather
// than silently defaulted to a fresh in-memory counter (the doc is explicit
// that a defaulted counter would re-mint ids already on disk).
func TestNewAgentIDMinter(t *testing.T) {
	t.Run("valid bus id and allocator succeeds", func(t *testing.T) {
		m, err := NewAgentIDMinter("bus-x", NewNameSuffixes())
		if err != nil {
			t.Fatalf("NewAgentIDMinter(%q, <valid alloc>): unexpected error %v", "bus-x", err)
		}
		if got := m.BusID(); got != "bus-x" {
			t.Fatalf("BusID() = %q, want %q", got, "bus-x")
		}
	})

	t.Run("invalid bus id is rejected", func(t *testing.T) {
		_, err := NewAgentIDMinter("", NewNameSuffixes())
		if err == nil {
			t.Fatalf("NewAgentIDMinter(%q, <valid alloc>) = nil error, want an error for an invalid bus id", "")
		}
	})

	t.Run("nil allocator is rejected, not defaulted", func(t *testing.T) {
		_, err := NewAgentIDMinter("bus-x", nil)
		if err == nil {
			t.Fatalf("NewAgentIDMinter(%q, nil) = nil error, want an error; a minter must never invent its own counter", "bus-x")
		}
	})
}

// TestAgentIDMinterMint covers the production path end to end: Mint must
// produce a fully-qualified "<bus-id>.<name>-<n>" id (invariant 2), BusID
// must round-trip into every minted id, and repeated Mint calls for the same
// name must draw strictly increasing suffixes from the underlying allocator.
func TestAgentIDMinterMint(t *testing.T) {
	busID, err := GenerateBusID()
	if err != nil {
		t.Fatalf("GenerateBusID(): %v", err)
	}
	m, err := NewAgentIDMinter(busID, NewNameSuffixes())
	if err != nil {
		t.Fatalf("NewAgentIDMinter(%q, ...): unexpected error %v", busID, err)
	}
	if got := m.BusID(); got != busID {
		t.Fatalf("BusID() = %q, want %q", got, busID)
	}

	for want := uint64(1); want <= 3; want++ {
		id, err := m.Mint("bob")
		if err != nil {
			t.Fatalf("Mint(%q) #%d: unexpected error %v", "bob", want, err)
		}
		gotBus, gotName, gotN, err := ParseAgentID(id)
		if err != nil {
			t.Fatalf("ParseAgentID(%q): unexpected error %v", id, err)
		}
		if gotBus != busID || gotName != "bob" || gotN != want {
			t.Fatalf("Mint(%q) #%d = %q, parsed as (%q, %q, %d), want (%q, %q, %d)", "bob", want, id, gotBus, gotName, gotN, busID, "bob", want)
		}
	}

	// A second name starts its own counter at 1, independent of "bob".
	id, err := m.Mint("alice")
	if err != nil {
		t.Fatalf("Mint(%q): unexpected error %v", "alice", err)
	}
	gotBus, gotName, gotN, err := ParseAgentID(id)
	if err != nil {
		t.Fatalf("ParseAgentID(%q): unexpected error %v", id, err)
	}
	if gotBus != busID || gotName != "alice" || gotN != 1 {
		t.Fatalf("Mint(%q) = %q, parsed as (%q, %q, %d), want (%q, %q, 1)", "alice", id, gotBus, gotName, gotN, busID, "alice")
	}
}

// TestAgentIDMinterMintRejectsInvalidNameWithoutCallingAllocator confirms Mint
// validates requestedName BEFORE it ever reaches the allocator: an
// unvalidated name must never become a counter key (see the NameSuffixes and
// Mint docs), so a rejected name must never show up in the allocator's call
// log at all.
func TestAgentIDMinterMintRejectsInvalidNameWithoutCallingAllocator(t *testing.T) {
	fake := &fakeSuffixAllocator{next: 1}
	m, err := NewAgentIDMinter("bus-x", fake)
	if err != nil {
		t.Fatalf("NewAgentIDMinter: unexpected error %v", err)
	}

	for _, name := range []string{"", "Bob", "bo b", "bo.b", "-bob"} {
		if _, err := m.Mint(name); err == nil {
			t.Fatalf("Mint(%q) = nil error, want an error (invalid name)", name)
		}
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("allocator was called %d times for invalid names, want 0; a name that fails ValidateAgentName must never reach the allocator", got)
	}
}

// TestAgentIDMinterMintPropagatesAllocatorError confirms an allocator failure
// (e.g. ErrSuffixExhausted for a name that has burned every suffix) surfaces
// through Mint as an error rather than being swallowed or masked.
func TestAgentIDMinterMintPropagatesAllocatorError(t *testing.T) {
	wantErr := fmt.Errorf("%w: name %q", ErrSuffixExhausted, "bob")
	fake := &fakeSuffixAllocator{nextErr: wantErr}
	m, err := NewAgentIDMinter("bus-x", fake)
	if err != nil {
		t.Fatalf("NewAgentIDMinter: unexpected error %v", err)
	}

	_, err = m.Mint("bob")
	if err == nil {
		t.Fatalf("Mint(%q) = nil error, want the allocator's error to propagate", "bob")
	}
	if !errors.Is(err, ErrSuffixExhausted) {
		t.Fatalf("Mint(%q) error = %v, want errors.Is(err, ErrSuffixExhausted)", "bob", err)
	}
}

// TestAgentIDMinterMintRejectsSuffixZeroFromAllocator pins a defensive
// property: if a (necessarily buggy, since NameSuffixes never does this)
// allocator hands back suffix 0 with a nil error, Mint must still fail rather
// than mint a real-looking id for an agent with no allocated suffix at all —
// AgentID itself rejects n == 0, and Mint must not swallow that.
func TestAgentIDMinterMintRejectsSuffixZeroFromAllocator(t *testing.T) {
	fake := &fakeSuffixAllocator{next: 0, nextErr: nil}
	m, err := NewAgentIDMinter("bus-x", fake)
	if err != nil {
		t.Fatalf("NewAgentIDMinter: unexpected error %v", err)
	}

	id, err := m.Mint("bob")
	if err == nil {
		t.Fatalf("Mint(%q) = (%q, nil), want an error when the allocator returns suffix 0", "bob", id)
	}
}

// TestAgentIDMinterBusIDRoundTrips confirms that BusID() always equals the
// bus id half ParseAgentID recovers from a minted id, across several
// distinct valid bus ids — the round trip invariant 2 depends on.
func TestAgentIDMinterBusIDRoundTrips(t *testing.T) {
	for _, busID := range []string{"a", "bus-x", "0", "bus-with-many-dashes-9"} {
		busID := busID
		t.Run(busID, func(t *testing.T) {
			m, err := NewAgentIDMinter(busID, NewNameSuffixes())
			if err != nil {
				t.Fatalf("NewAgentIDMinter(%q, ...): unexpected error %v", busID, err)
			}
			if got := m.BusID(); got != busID {
				t.Fatalf("BusID() = %q, want %q", got, busID)
			}
			id, err := m.Mint("bob")
			if err != nil {
				t.Fatalf("Mint(%q): unexpected error %v", "bob", err)
			}
			gotBus, _, _, err := ParseAgentID(id)
			if err != nil {
				t.Fatalf("ParseAgentID(%q): unexpected error %v", id, err)
			}
			if gotBus != m.BusID() {
				t.Fatalf("ParseAgentID(%q) bus id = %q, want %q (must match BusID())", id, gotBus, m.BusID())
			}
		})
	}
}
