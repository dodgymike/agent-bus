package ids

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

// TestNameSuffixesAllocator is the per-name analogue of TestSequenceAllocator
// and pins the same headline contract, one level further in: NewNameSuffixes
// starts every name at 1, ResumeNameSuffixes(floor) starts a name strictly
// above its floor, a name absent from the resume map is floor 0 (equivalent
// to NewNameSuffixes for that name), and LastSuffix reports what has been
// ISSUED for a name, not its floor.
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

	t.Run("ResumeNilOrEmptyMapEquivalentToNew", func(t *testing.T) {
		for _, m := range []map[string]uint64{nil, {}} {
			fresh := NewNameSuffixes()
			resumed := ResumeNameSuffixes(m)
			for i := 0; i < 5; i++ {
				wantN, wantErr := fresh.NextSuffix("bob")
				gotN, gotErr := resumed.NextSuffix("bob")
				if gotN != wantN || !suffixErrorsEqual(gotErr, wantErr) {
					t.Fatalf("ResumeNameSuffixes(%v).NextSuffix(%q) #%d = (%d, %v), want (%d, %v) matching NewNameSuffixes()", m, "bob", i, gotN, gotErr, wantN, wantErr)
				}
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

// suffixErrorsEqual compares two NextSuffix errors for the Resume(nil/empty)
// == NewNameSuffixes() equivalence test: both nil, or both ErrSuffixExhausted.
func suffixErrorsEqual(a, b error) bool {
	if a == nil && b == nil {
		return true
	}
	return errors.Is(a, ErrSuffixExhausted) && errors.Is(b, ErrSuffixExhausted)
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
			var s *NameSuffixes
			if floor == 0 {
				s = NewNameSuffixes()
			} else {
				s = ResumeNameSuffixes(map[string]uint64{name: floor})
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
// call — while leaving every other name unaffected.
func TestNameSuffixesOverflow(t *testing.T) {
	t.Run("ResumeAtMaxIsImmediatelyExhausted", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": math.MaxUint64})
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
func TestNameSuffixesRaiseFloor(t *testing.T) {
	t.Run("NothingIssuedAtLeastAtOrBelowFloorIsNoOp", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 10})
		if err := s.RaiseFloor("bob", 10); err != nil {
			t.Fatalf("RaiseFloor(%q, 10) on floor 10, nothing issued: %v, want nil (no-op)", "bob", err)
		}
		if err := s.RaiseFloor("bob", 5); err != nil {
			t.Fatalf("RaiseFloor(%q, 5) on floor 10, nothing issued: %v, want nil (no-op)", "bob", err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after no-op RaiseFloor calls: unexpected error %v", "bob", err)
		}
		if n != 11 {
			t.Fatalf("NextSuffix(%q) after no-op RaiseFloor calls = %d, want 11 (floor of 10 unchanged)", "bob", n)
		}
	})

	t.Run("NothingIssuedAtLeastAboveFloorRaises", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"bob": 10})
		if err := s.RaiseFloor("bob", 20); err != nil {
			t.Fatalf("RaiseFloor(%q, 20) on floor 10, nothing issued: %v, want nil", "bob", err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(20): unexpected error %v", "bob", err)
		}
		if n != 21 {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(20) = %d, want 21", "bob", n)
		}
	})

	t.Run("SomethingIssuedAtLeastAtLastIsError", func(t *testing.T) {
		s := NewNameSuffixes()
		var last uint64
		for i := 0; i < 3; i++ {
			n, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
			last = n
		}
		// Equality case: atLeast == LastSuffix must be an error, not a no-op —
		// this is the "resumed one restart later" scenario invariant 1 forbids.
		if err := s.RaiseFloor("bob", last); !errors.Is(err, ErrFloorBelowIssued) {
			t.Fatalf("RaiseFloor(%q, %d) with LastSuffix()==%d: err = %v, want errors.Is(err, ErrFloorBelowIssued)", "bob", last, last, err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor: unexpected error %v", "bob", err)
		}
		if n != last+1 {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged) — a suffix at or below the floor must NEVER be reissued", "bob", last, n, last+1)
		}
	})

	t.Run("SomethingIssuedAtLeastBelowLastIsError", func(t *testing.T) {
		s := NewNameSuffixes()
		var last uint64
		for i := 0; i < 5; i++ {
			n, err := s.NextSuffix("bob")
			if err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
			last = n
		}
		if err := s.RaiseFloor("bob", last-2); !errors.Is(err, ErrFloorBelowIssued) {
			t.Fatalf("RaiseFloor(%q, %d) with LastSuffix()==%d: err = %v, want errors.Is(err, ErrFloorBelowIssued)", "bob", last-2, last, err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor: unexpected error %v", "bob", err)
		}
		if n != last+1 {
			t.Fatalf("NextSuffix(%q) after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged)", "bob", last-2, n, last+1)
		}
	})

	t.Run("SomethingIssuedAtLeastAboveLastRaises", func(t *testing.T) {
		s := NewNameSuffixes()
		for i := 0; i < 3; i++ {
			if _, err := s.NextSuffix("bob"); err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
		}
		if err := s.RaiseFloor("bob", 10); err != nil {
			t.Fatalf("RaiseFloor(%q, 10) with LastSuffix()==3: %v, want nil", "bob", err)
		}
		n, err := s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(10): unexpected error %v", "bob", err)
		}
		if n != 11 {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(10) = %d, want 11", "bob", n)
		}
	})

	t.Run("RaiseFloorTouchesOnlyTheOneName", func(t *testing.T) {
		s := NewNameSuffixes()
		if _, err := s.NextSuffix("alice"); err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "alice", err)
		}
		if err := s.RaiseFloor("bob", 500); err != nil {
			t.Fatalf("RaiseFloor(%q, 500): %v, want nil", "bob", err)
		}
		// alice's counter must be untouched by raising bob's floor.
		n, err := s.NextSuffix("alice")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "alice", err)
		}
		if n != 2 {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(%q, 500) = %d, want 2; RaiseFloor must touch only the named counter", "alice", "bob", n)
		}
		n, err = s.NextSuffix("bob")
		if err != nil {
			t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
		}
		if n != 501 {
			t.Fatalf("NextSuffix(%q) after RaiseFloor(%q, 500) = %d, want 501", "bob", "bob", n)
		}
	})

	t.Run("RaiseFloorToMaxSucceedsAndExhausts", func(t *testing.T) {
		t.Run("nothing issued yet", func(t *testing.T) {
			s := NewNameSuffixes()
			if err := s.RaiseFloor("bob", math.MaxUint64); err != nil {
				t.Fatalf("RaiseFloor(%q, MaxUint64): %v, want nil", "bob", err)
			}
			n, err := s.NextSuffix("bob")
			if n != 0 || !errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) after RaiseFloor(MaxUint64) = (%d, %v), want (0, ErrSuffixExhausted)", "bob", n, err)
			}
		})

		t.Run("something already issued", func(t *testing.T) {
			s := NewNameSuffixes()
			if _, err := s.NextSuffix("bob"); err != nil {
				t.Fatalf("NextSuffix(%q): unexpected error %v", "bob", err)
			}
			if err := s.RaiseFloor("bob", math.MaxUint64); err != nil {
				t.Fatalf("RaiseFloor(%q, MaxUint64) with LastSuffix()==1: %v, want nil", "bob", err)
			}
			n, err := s.NextSuffix("bob")
			if n != 0 || !errors.Is(err, ErrSuffixExhausted) {
				t.Fatalf("NextSuffix(%q) after RaiseFloor(MaxUint64) = (%d, %v), want (0, ErrSuffixExhausted)", "bob", n, err)
			}
		})
	})
}

// TestNameSuffixesRaiseFloorRace exercises RaiseFloor racing with NextSuffix
// across several names under -race: neither must ever produce a duplicate
// value for any name, and a floor (and therefore every value NextSuffix can
// subsequently return for that name) must never move backwards.
func TestNameSuffixesRaiseFloorRace(t *testing.T) {
	const nextGoroutines = 30
	const perGoroutine = 200
	const raiseGoroutines = 5
	names := []string{"bob", "alice"}

	s := NewNameSuffixes()

	results := make(chan struct {
		name string
		n    uint64
	}, nextGoroutines*perGoroutine)
	var wg sync.WaitGroup

	wg.Add(nextGoroutines)
	for g := 0; g < nextGoroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			name := names[g%len(names)]
			for i := 0; i < perGoroutine; i++ {
				n, err := s.NextSuffix(name)
				if err != nil {
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
					// Error just means LastSuffix has already passed atLeast
					// for that name; legal, and expected under concurrency.
					_ = s.RaiseFloor(name, atLeast)
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
	}
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
