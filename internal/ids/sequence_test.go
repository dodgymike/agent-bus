package ids

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

// TestSequenceAllocator is the proof command for the ID-2 task and the
// headline behavioral contract of Sequence: NewSequence starts at 1,
// Resume(n) starts strictly above n, Resume(0) is exactly NewSequence, and
// Last() reports what has been ISSUED, not the floor.
func TestSequenceAllocator(t *testing.T) {
	t.Run("NewSequenceIssuesFromOne", func(t *testing.T) {
		s := NewSequence()
		if got := s.Last(); got != 0 {
			t.Fatalf("Last() before any Next() = %d, want 0", got)
		}
		for want := uint64(1); want <= 5; want++ {
			got, err := s.Next()
			if err != nil {
				t.Fatalf("Next() #%d: unexpected error %v", want, err)
			}
			if got != want {
				t.Fatalf("Next() #%d = %d, want %d", want, got, want)
			}
			if last := s.Last(); last != want {
				t.Fatalf("Last() after issuing %d = %d, want %d", want, last, want)
			}
		}
	})

	t.Run("ResumeIssuesAboveFloor", func(t *testing.T) {
		const floor = uint64(41)
		s := Resume(floor)
		for i := uint64(1); i <= 5; i++ {
			want := floor + i
			got, err := s.Next()
			if err != nil {
				t.Fatalf("Next() #%d: unexpected error %v", i, err)
			}
			if got != want {
				t.Fatalf("Next() #%d = %d, want %d (floor %d + %d)", i, got, want, floor, i)
			}
		}
	})

	t.Run("ResumeZeroEquivalentToNew", func(t *testing.T) {
		fresh := NewSequence()
		resumed := Resume(0)
		for i := 0; i < 5; i++ {
			wantN, wantErr := fresh.Next()
			gotN, gotErr := resumed.Next()
			if gotN != wantN || !errorsEqual(gotErr, wantErr) {
				t.Fatalf("Resume(0).Next() #%d = (%d, %v), want (%d, %v) matching NewSequence()", i, gotN, gotErr, wantN, wantErr)
			}
		}
		if fresh.Last() != resumed.Last() {
			t.Fatalf("Resume(0).Last() = %d, want %d (equal to NewSequence().Last())", resumed.Last(), fresh.Last())
		}
	})

	t.Run("LastZeroBeforeFirstNextOnResume", func(t *testing.T) {
		// The subtle bit: a resumed allocator has a non-zero floor but has
		// ISSUED nothing yet, so Last() must still report 0 until the first
		// Next call, even though the floor itself is non-zero.
		s := Resume(1000)
		if got := s.Last(); got != 0 {
			t.Fatalf("Last() on a fresh Resume(1000) before any Next() = %d, want 0 (floor is not \"issued\")", got)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next(): unexpected error %v", err)
		}
		if n != 1001 {
			t.Fatalf("Next() after Resume(1000) = %d, want 1001", n)
		}
		if got := s.Last(); got != 1001 {
			t.Fatalf("Last() after first Next() = %d, want 1001", got)
		}
	})
}

// errorsEqual compares two errors for the purposes of the Resume(0) ==
// NewSequence() equivalence test: both nil, or both ErrSequenceExhausted.
func errorsEqual(a, b error) bool {
	if a == nil && b == nil {
		return true
	}
	return errors.Is(a, ErrSequenceExhausted) && errors.Is(b, ErrSequenceExhausted)
}

// TestSequenceAllocatorConcurrentNext is the point of this task: Next() must
// never issue the same number twice under concurrent load, the union of
// everything issued must be exactly the contiguous set floor+1..floor+N, and
// Last() must equal the maximum issued value. Run with -race.
func TestSequenceAllocatorConcurrentNext(t *testing.T) {
	const goroutines = 50
	const perGoroutine = 200
	const total = goroutines * perGoroutine

	for _, floor := range []uint64{0, 1_000_000} {
		floor := floor
		t.Run(fmt.Sprintf("floor=%d", floor), func(t *testing.T) {
			var s *Sequence
			if floor == 0 {
				s = NewSequence()
			} else {
				s = Resume(floor)
			}

			results := make(chan uint64, total)
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				go func() {
					defer wg.Done()
					for i := 0; i < perGoroutine; i++ {
						n, err := s.Next()
						if err != nil {
							t.Errorf("Next(): unexpected error %v", err)
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
					t.Fatalf("Next() issued duplicate value %d", n)
				}
				seen[n] = true
				if n > max {
					max = n
				}
			}
			if len(seen) != total {
				t.Fatalf("got %d unique issued values, want %d", len(seen), total)
			}
			for i := uint64(1); i <= uint64(total); i++ {
				want := floor + i
				if !seen[want] {
					t.Fatalf("issued set is missing %d; union must be exactly the contiguous set %d..%d", want, floor+1, floor+total)
				}
			}
			if got := s.Last(); got != max {
				t.Fatalf("Last() = %d, want max issued value %d", got, max)
			}
		})
	}
}

// TestSequenceAllocatorOverflow covers the documented overflow contract: at
// math.MaxUint64 Next returns (0, ErrSequenceExhausted), never wraps to 0,
// and stays exhausted on every subsequent call.
func TestSequenceAllocatorOverflow(t *testing.T) {
	t.Run("ResumeAtMaxIsImmediatelyExhausted", func(t *testing.T) {
		s := Resume(math.MaxUint64)
		for i := 0; i < 3; i++ {
			n, err := s.Next()
			if n != 0 {
				t.Fatalf("Next() call #%d = %d, want 0 (must never wrap)", i, n)
			}
			if !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() call #%d error = %v, want errors.Is(err, ErrSequenceExhausted)", i, err)
			}
		}
	})

	t.Run("ResumeOneBelowMaxIssuesMaxThenExhausts", func(t *testing.T) {
		s := Resume(math.MaxUint64 - 1)
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() at MaxUint64-1: unexpected error %v", err)
		}
		if n != math.MaxUint64 {
			t.Fatalf("Next() = %d, want math.MaxUint64 (%d)", n, uint64(math.MaxUint64))
		}
		if last := s.Last(); last != math.MaxUint64 {
			t.Fatalf("Last() = %d, want math.MaxUint64", last)
		}

		for i := 0; i < 3; i++ {
			n, err := s.Next()
			if n != 0 {
				t.Fatalf("Next() after exhaustion call #%d = %d, want 0", i, n)
			}
			if !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() after exhaustion call #%d error = %v, want errors.Is(err, ErrSequenceExhausted)", i, err)
			}
		}
	})
}

// TestSequenceAllocatorRaiseFloor encodes the documented boundary between
// RaiseFloor's no-op and error cases exactly as specified in the doc comment
// on Sequence.RaiseFloor.
func TestSequenceAllocatorRaiseFloor(t *testing.T) {
	t.Run("NothingIssuedAtLeastAtOrBelowFloorIsNoOp", func(t *testing.T) {
		s := Resume(10)
		if err := s.RaiseFloor(10); err != nil {
			t.Fatalf("RaiseFloor(10) on floor 10, nothing issued: %v, want nil (no-op)", err)
		}
		if err := s.RaiseFloor(5); err != nil {
			t.Fatalf("RaiseFloor(5) on floor 10, nothing issued: %v, want nil (no-op)", err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after no-op RaiseFloor calls: unexpected error %v", err)
		}
		if n != 11 {
			t.Fatalf("Next() after no-op RaiseFloor calls = %d, want 11 (floor of 10 unchanged)", n)
		}
	})

	t.Run("NothingIssuedAtLeastAboveFloorRaises", func(t *testing.T) {
		s := Resume(10)
		if err := s.RaiseFloor(20); err != nil {
			t.Fatalf("RaiseFloor(20) on floor 10, nothing issued: %v, want nil", err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after RaiseFloor(20): unexpected error %v", err)
		}
		if n != 21 {
			t.Fatalf("Next() after RaiseFloor(20) = %d, want 21", n)
		}
	})

	t.Run("SomethingIssuedAtLeastAtLastIsError", func(t *testing.T) {
		s := NewSequence()
		var last uint64
		for i := 0; i < 3; i++ {
			n, err := s.Next()
			if err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
			last = n
		}
		// Equality case: atLeast == Last() must be an error, not a no-op.
		if err := s.RaiseFloor(last); !errors.Is(err, ErrFloorBelowIssued) {
			t.Fatalf("RaiseFloor(%d) with Last()==%d: err = %v, want errors.Is(err, ErrFloorBelowIssued)", last, last, err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after rejected RaiseFloor: unexpected error %v", err)
		}
		if n != last+1 {
			t.Fatalf("Next() after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged)", last, n, last+1)
		}
	})

	t.Run("SomethingIssuedAtLeastBelowLastIsError", func(t *testing.T) {
		s := NewSequence()
		var last uint64
		for i := 0; i < 5; i++ {
			n, err := s.Next()
			if err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
			last = n
		}
		if err := s.RaiseFloor(last - 2); !errors.Is(err, ErrFloorBelowIssued) {
			t.Fatalf("RaiseFloor(%d) with Last()==%d: err = %v, want errors.Is(err, ErrFloorBelowIssued)", last-2, last, err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after rejected RaiseFloor: unexpected error %v", err)
		}
		if n != last+1 {
			t.Fatalf("Next() after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged)", last-2, n, last+1)
		}
	})

	t.Run("SomethingIssuedAtLeastAboveLastRaises", func(t *testing.T) {
		s := NewSequence()
		for i := 0; i < 3; i++ {
			if _, err := s.Next(); err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
		}
		// Last() == 3, floor == 3 here.
		if err := s.RaiseFloor(10); err != nil {
			t.Fatalf("RaiseFloor(10) with Last()==3: %v, want nil", err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after RaiseFloor(10): unexpected error %v", err)
		}
		if n != 11 {
			t.Fatalf("Next() after RaiseFloor(10) = %d, want 11", n)
		}
	})

	t.Run("RaiseFloorToMaxSucceedsAndExhausts", func(t *testing.T) {
		t.Run("nothing issued yet", func(t *testing.T) {
			s := NewSequence()
			if err := s.RaiseFloor(math.MaxUint64); err != nil {
				t.Fatalf("RaiseFloor(MaxUint64): %v, want nil", err)
			}
			n, err := s.Next()
			if n != 0 || !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() after RaiseFloor(MaxUint64) = (%d, %v), want (0, ErrSequenceExhausted)", n, err)
			}
		})

		t.Run("something already issued", func(t *testing.T) {
			s := NewSequence()
			if _, err := s.Next(); err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
			if err := s.RaiseFloor(math.MaxUint64); err != nil {
				t.Fatalf("RaiseFloor(MaxUint64) with Last()==1: %v, want nil", err)
			}
			n, err := s.Next()
			if n != 0 || !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() after RaiseFloor(MaxUint64) = (%d, %v), want (0, ErrSequenceExhausted)", n, err)
			}
		})
	})
}

// TestSequenceAllocatorRaiseFloorRace exercises RaiseFloor racing with Next
// under -race: neither must ever produce a duplicate value, and the floor
// (and therefore every value Next can subsequently return) must never move
// backwards.
func TestSequenceAllocatorRaiseFloorRace(t *testing.T) {
	const nextGoroutines = 30
	const perGoroutine = 200
	const raiseGoroutines = 5

	s := NewSequence()

	results := make(chan uint64, nextGoroutines*perGoroutine)
	var wg sync.WaitGroup

	wg.Add(nextGoroutines)
	for g := 0; g < nextGoroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				n, err := s.Next()
				if err != nil {
					// Bounded well below math.MaxUint64, so exhaustion is
					// not expected; report but do not fail the goroutine.
					t.Errorf("Next(): unexpected error %v", err)
					continue
				}
				results <- n
			}
		}()
	}

	wg.Add(raiseGoroutines)
	for r := 0; r < raiseGoroutines; r++ {
		r := r
		go func() {
			defer wg.Done()
			// Small, well-bounded targets: RaiseFloor is a no-op whenever
			// concurrent Next() calls have already passed the target, and
			// that is a legal outcome, not a failure.
			for i := 1; i <= 20; i++ {
				atLeast := uint64(r*1000 + i*10)
				_ = s.RaiseFloor(atLeast) // error here just means Last() has already passed atLeast; legal.
			}
		}()
	}

	wg.Wait()
	close(results)

	seen := make(map[uint64]bool, nextGoroutines*perGoroutine)
	var max uint64
	for n := range results {
		if seen[n] {
			t.Fatalf("Next() issued duplicate value %d under RaiseFloor race", n)
		}
		seen[n] = true
		if n > max {
			max = n
		}
	}
	if got := s.Last(); got != max {
		t.Fatalf("Last() = %d, want max issued value %d", got, max)
	}
}
