package ids

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

// TestSequenceRefusesToIssueFromAnUnsealedFloor is the proof command for
// ID-2-WIRING-SEAL. It pins the two-state, one-way machine documented under
// "The floor must be SEALED before anything is issued":
//
//   - UNSEALED (both constructors, and the zero value): Next issues NOTHING and
//     returns (0, ErrFloorUnproven); RaiseFloor is legal.
//   - SEALED, after exactly one Seal: Next issues; RaiseFloor returns
//     ErrFloorSealed and changes nothing; a second Seal likewise.
//
// The load-bearing assertions are the ones about what a REFUSED call did to the
// allocator: a refusal that quietly burned a number would still look like a
// refusal to the caller, and would break invariant 1 one restart later.
func TestSequenceRefusesToIssueFromAnUnsealedFloor(t *testing.T) {
	t.Run("ResumeRefusesUntilSealedAndBurnsNothing", func(t *testing.T) {
		s := Resume(99)

		// Refused repeatedly: the state machine has one transition and Next is
		// not it, so calling Next again must not inch the floor forward.
		for i := 0; i < 5; i++ {
			n, err := s.Next()
			if n != 0 {
				t.Fatalf("unsealed Next() call #%d = %d, want 0 (an unsealed allocator issues nothing)", i, n)
			}
			if !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("unsealed Next() call #%d error = %v, want errors.Is(err, ErrFloorUnproven)", i, err)
			}
			if last := s.Last(); last != 0 {
				t.Fatalf("Last() after %d refused Next() calls = %d, want 0 (nothing was issued)", i+1, last)
			}
		}

		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil (first seal)", err)
		}

		// THE assertion this subtest exists for: five refused Next calls burned
		// nothing, so the first issued number is still floor+1 == 100. If a
		// refusal had incremented the floor this would be 105 and the bus would
		// silently have a five-number hole in its id space.
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after Seal(): unexpected error %v", err)
		}
		if n != 100 {
			t.Fatalf("Next() after Seal() on Resume(99) = %d, want 100 (the refused calls must have burned NOTHING)", n)
		}
		if last := s.Last(); last != 100 {
			t.Fatalf("Last() after first issued number = %d, want 100", last)
		}
	})

	t.Run("NewSequenceIsBornUnsealedToo", func(t *testing.T) {
		// This is the whole reason BOTH constructors are born unsealed. A fresh
		// bus is floor 0 because there is nothing on disk; a bus whose recovery
		// scan FAILED is also floor 0. The type cannot tell those two apart, so
		// floor-0-because-empty must not be distinguishable-by-behaviour from
		// floor-0-because-the-scan-failed: neither may issue until a caller says
		// out loud, by sealing, that the floor is proven. If NewSequence were
		// born sealed, the failed-scan case would silently inherit the empty-bus
		// default and reissue every id from 1.
		s := NewSequence()
		for i := 0; i < 3; i++ {
			n, err := s.Next()
			if n != 0 || !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("unsealed NewSequence().Next() call #%d = (%d, %v), want (0, ErrFloorUnproven)", i, n, err)
			}
		}
		if last := s.Last(); last != 0 {
			t.Fatalf("Last() on a refused fresh allocator = %d, want 0", last)
		}

		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after Seal(): unexpected error %v", err)
		}
		if n != 1 {
			t.Fatalf("Next() after Seal() on NewSequence() = %d, want 1", n)
		}
	})

	t.Run("ZeroValueFailsClosed", func(t *testing.T) {
		// An allocator reached by accident — a struct field nobody constructed —
		// must refuse rather than mint 1 from a floor nobody derived.
		t.Run("var s Sequence", func(t *testing.T) {
			var s Sequence
			n, err := s.Next()
			if n != 0 || !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("zero-value Sequence.Next() = (%d, %v), want (0, ErrFloorUnproven)", n, err)
			}
			if last := s.Last(); last != 0 {
				t.Fatalf("zero-value Sequence.Last() = %d, want 0", last)
			}
		})

		t.Run("&Sequence{}", func(t *testing.T) {
			s := &Sequence{}
			n, err := s.Next()
			if n != 0 || !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("&Sequence{}.Next() = (%d, %v), want (0, ErrFloorUnproven)", n, err)
			}
			// It is still a legal (if pointless) allocator once sealed — the
			// zero value fails closed, it is not permanently poisoned.
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal() on &Sequence{}: %v, want nil", err)
			}
			if n, err := s.Next(); n != 1 || err != nil {
				t.Fatalf("&Sequence{}.Next() after Seal() = (%d, %v), want (1, nil)", n, err)
			}
		})
	})

	t.Run("AssembleThenSealThenRaiseFloorIsRefused", func(t *testing.T) {
		s := Resume(100)

		// The assembly window is still open while unsealed: the floor may be
		// raised from a second source (the audit high-water mark, a peer).
		if err := s.RaiseFloor(150); err != nil {
			t.Fatalf("RaiseFloor(150) while UNSEALED: %v, want nil (assembly is still open)", err)
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}

		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after Seal(): unexpected error %v", err)
		}
		if n != 151 {
			t.Fatalf("Next() after RaiseFloor(150)+Seal() = %d, want 151", n)
		}

		// Assembly is over. A later claim about the high-water mark is refused
		// and must change nothing at all.
		lastBefore := s.Last()
		err = s.RaiseFloor(200)
		if !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(200) after Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", err)
		}
		if lastAfter := s.Last(); lastAfter != lastBefore {
			t.Fatalf("Last() changed across a REJECTED RaiseFloor: %d -> %d", lastBefore, lastAfter)
		}

		n, err = s.Next()
		if err != nil {
			t.Fatalf("Next() after rejected RaiseFloor: unexpected error %v", err)
		}
		if n != 152 {
			t.Fatalf("Next() after rejected RaiseFloor(200) = %d, want 152 (the floor must NOT have moved to 201)", n)
		}
	})

	t.Run("SecondSealIsRefusedAndChangesNothing", func(t *testing.T) {
		s := Resume(5)
		if err := s.Seal(); err != nil {
			t.Fatalf("first Seal(): %v, want nil", err)
		}
		if n, err := s.Next(); n != 6 || err != nil {
			t.Fatalf("Next() after first Seal() = (%d, %v), want (6, nil)", n, err)
		}

		err := s.Seal()
		if !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("second Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", err)
		}
		// A failed seal is not a state change: the allocator keeps allocating
		// from exactly where it was.
		if n, err := s.Next(); n != 7 || err != nil {
			t.Fatalf("Next() after a REJECTED second Seal() = (%d, %v), want (7, nil)", n, err)
		}
		if last := s.Last(); last != 7 {
			t.Fatalf("Last() = %d, want 7", last)
		}
		// And a third seal is refused identically — the state is one-way.
		if err := s.Seal(); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("third Seal(): err = %v, want errors.Is(err, ErrFloorSealed)", err)
		}
	})

	// GUARD ORDERING. Both guards sit in front of an older check, and swapping
	// either pair back would still leave a test suite that only asserted "some
	// error" entirely green. These two subtests name the exact sentinel.
	t.Run("GuardOrdering", func(t *testing.T) {
		t.Run("UnsealedBeatsExhausted", func(t *testing.T) {
			// Resume(MaxUint64) is exhausted AND unsealed. The unsealed check
			// runs first: an allocator with no proven floor cannot honestly
			// claim to be exhausted, because "exhausted" is a statement about
			// how far the floor has been carried.
			s := Resume(math.MaxUint64)
			n, err := s.Next()
			if n != 0 {
				t.Fatalf("Next() on unsealed Resume(MaxUint64) = %d, want 0", n)
			}
			if !errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("Next() on unsealed Resume(MaxUint64) error = %v, want ErrFloorUnproven (the unsealed check runs BEFORE the exhaustion check)", err)
			}
			if errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() on unsealed Resume(MaxUint64) reported ErrSequenceExhausted; the unsealed check must pre-empt it: %v", err)
			}

			// After the seal, the very same call reports exhaustion.
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			n, err = s.Next()
			if n != 0 {
				t.Fatalf("Next() on sealed Resume(MaxUint64) = %d, want 0 (never wrap)", n)
			}
			if !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() on sealed Resume(MaxUint64) error = %v, want ErrSequenceExhausted", err)
			}
			if errors.Is(err, ErrFloorUnproven) {
				t.Fatalf("Next() on a SEALED allocator still reported ErrFloorUnproven: %v", err)
			}
		})

		t.Run("SealedBeatsBelowIssued", func(t *testing.T) {
			// A sealed allocator that has issued something satisfies BOTH
			// RaiseFloor guards. The seal check runs first because it is the
			// stronger statement: floor assembly is over, full stop.
			s := NewSequence()
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			for i := 0; i < 3; i++ {
				if _, err := s.Next(); err != nil {
					t.Fatalf("Next(): unexpected error %v", err)
				}
			}
			last := s.Last()
			for _, atLeast := range []uint64{last, last - 1, 0} {
				err := s.RaiseFloor(atLeast)
				if !errors.Is(err, ErrFloorSealed) {
					t.Fatalf("RaiseFloor(%d) with Last()==%d on a sealed allocator: err = %v, want ErrFloorSealed", atLeast, last, err)
				}
				if errors.Is(err, ErrFloorBelowIssued) {
					t.Fatalf("RaiseFloor(%d) reported ErrFloorBelowIssued; the seal check must pre-empt it: %v", atLeast, err)
				}
			}
			if got := s.Last(); got != last {
				t.Fatalf("Last() changed across rejected RaiseFloor calls: %d -> %d", last, got)
			}
		})
	})

	t.Run("SentinelsAreDistinct", func(t *testing.T) {
		// errors.Is is the whole API callers have for telling these apart, so a
		// wrapped error must match its OWN sentinel and no other. Every error
		// here comes from a live code path, not from a hand-built fmt.Errorf.
		unproven := func() error {
			_, err := Resume(1).Next() // unsealed
			return err
		}()
		exhausted := func() error {
			s := Resume(math.MaxUint64)
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v", err)
			}
			_, err := s.Next()
			return err
		}()
		sealed := func() error {
			s := NewSequence()
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v", err)
			}
			return s.Seal()
		}()
		// ErrFloorBelowIssued is unreachable on Sequence under the seal gate, so
		// its live producer is NameSuffixes.RaiseFloor, which has no seal and
		// enforces the same contract per name.
		belowIssued := func() error {
			ns := NewNameSuffixes()
			if _, err := ns.NextSuffix("alice"); err != nil {
				t.Fatalf("NextSuffix: %v", err)
			}
			return ns.RaiseFloor("alice", 1)
		}()

		sentinels := []struct {
			name string
			err  error
		}{
			{"ErrFloorUnproven", ErrFloorUnproven},
			{"ErrFloorSealed", ErrFloorSealed},
			{"ErrSequenceExhausted", ErrSequenceExhausted},
			{"ErrFloorBelowIssued", ErrFloorBelowIssued},
		}

		// Each sentinel matches only itself.
		for _, a := range sentinels {
			for _, b := range sentinels {
				if a.name == b.name {
					if !errors.Is(a.err, b.err) {
						t.Fatalf("errors.Is(%s, %s) = false, want true", a.name, b.name)
					}
					continue
				}
				if errors.Is(a.err, b.err) {
					t.Fatalf("errors.Is(%s, %s) = true; the sentinels must be distinct", a.name, b.name)
				}
			}
		}

		// Each live, wrapped error matches only its own sentinel.
		produced := []struct {
			name string
			err  error
			want error
		}{
			{"unsealed Next()", unproven, ErrFloorUnproven},
			{"sealed Next() at MaxUint64", exhausted, ErrSequenceExhausted},
			{"second Seal()", sealed, ErrFloorSealed},
			{"NameSuffixes.RaiseFloor below issued", belowIssued, ErrFloorBelowIssued},
		}
		for _, p := range produced {
			if p.err == nil {
				t.Fatalf("%s returned nil, want an error", p.name)
			}
			for _, s := range sentinels {
				got := errors.Is(p.err, s.err)
				want := errors.Is(s.err, p.want)
				if got != want {
					t.Fatalf("errors.Is(%s error, %s) = %v, want %v (it must match %v and nothing else); err = %v",
						p.name, s.name, got, want, p.want, p.err)
				}
			}
		}
	})

	// CONCURRENCY. Goroutines hammer Next on an UNSEALED allocator while one
	// goroutine seals it. Which calls land before the seal and which after is
	// pure scheduling, so NOTHING here asserts a count or a ratio.
	//
	// Timing-INDEPENDENT (asserted): every error is exactly ErrFloorUnproven and
	// never a partial/torn result; no value is ever issued twice; every issued
	// value is strictly greater than the floor; Last() equals the maximum issued
	// value, or 0 if nothing was issued; and one final Next after the barrier
	// returns max(floor, Last())+1 — i.e. the refused calls burned nothing.
	// Timing-DEPENDENT (deliberately NOT asserted): how many calls succeeded,
	// how many were refused, and which values landed. Zero successes and zero
	// failures are both legal outcomes of this test.
	t.Run("ConcurrentNextRacingSeal", func(t *testing.T) {
		const goroutines = 32
		const perGoroutine = 50

		for _, floor := range []uint64{0, 5_000} {
			floor := floor
			t.Run(fmt.Sprintf("floor=%d", floor), func(t *testing.T) {
				s := Resume(floor)

				var (
					mu        sync.Mutex
					issued    []uint64
					refusals  int
					otherErrs []error
				)

				var wg sync.WaitGroup
				wg.Add(goroutines)
				for g := 0; g < goroutines; g++ {
					go func() {
						defer wg.Done()
						for i := 0; i < perGoroutine; i++ {
							n, err := s.Next()
							mu.Lock()
							switch {
							case err == nil:
								issued = append(issued, n)
							case errors.Is(err, ErrFloorUnproven):
								refusals++
								if n != 0 {
									otherErrs = append(otherErrs, fmt.Errorf("refused Next() returned n=%d, want 0", n))
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

				seen := make(map[uint64]bool, len(issued))
				var max uint64
				for _, n := range issued {
					if seen[n] {
						t.Fatalf("Next() issued duplicate value %d while racing Seal()", n)
					}
					seen[n] = true
					if n <= floor {
						t.Fatalf("Next() issued %d, which is not strictly greater than the floor %d", n, floor)
					}
					if n > max {
						max = n
					}
				}
				if got := s.Last(); got != max {
					t.Fatalf("Last() = %d, want %d (the maximum issued value, 0 if none were issued)", got, max)
				}
				if refusals+len(issued) != goroutines*perGoroutine {
					t.Fatalf("accounted for %d+%d calls, want %d", len(issued), refusals, goroutines*perGoroutine)
				}

				// The barrier has passed, so the allocator is definitely sealed:
				// this Next must succeed, and it must continue from the floor —
				// proving that every refused call burned nothing.
				wantNext := floor + 1
				if max > floor {
					wantNext = max + 1
				}
				n, err := s.Next()
				if err != nil {
					t.Fatalf("Next() after the seal barrier: unexpected error %v", err)
				}
				if n != wantNext {
					t.Fatalf("Next() after the seal barrier = %d, want %d (%d refused calls must have burned nothing)", n, wantNext, refusals)
				}
			})
		}
	})
}

// TestSequenceAllocator is the headline behavioral contract of Sequence:
// NewSequence starts at 1, Resume(n) starts strictly above n, Resume(0) is
// exactly NewSequence, and Last() reports what has been ISSUED, not the floor.
// Every allocator here is SEALED first — since ID-2-WIRING-SEAL, issuing at all
// requires it (see TestSequenceRefusesToIssueFromAnUnsealedFloor).
func TestSequenceAllocator(t *testing.T) {
	t.Run("NewSequenceIssuesFromOne", func(t *testing.T) {
		s := NewSequence()
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
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
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
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

		// The equivalence holds in the UNSEALED state too: both refuse
		// identically, so Resume(0) is not a back door around the seal gate.
		freshN, freshErr := fresh.Next()
		resumedN, resumedErr := resumed.Next()
		if freshN != 0 || !errors.Is(freshErr, ErrFloorUnproven) {
			t.Fatalf("unsealed NewSequence().Next() = (%d, %v), want (0, ErrFloorUnproven)", freshN, freshErr)
		}
		if resumedN != freshN || !errorsEqual(resumedErr, freshErr) {
			t.Fatalf("unsealed Resume(0).Next() = (%d, %v), want (%d, %v) matching NewSequence()", resumedN, resumedErr, freshN, freshErr)
		}

		if err := fresh.Seal(); err != nil {
			t.Fatalf("fresh.Seal(): %v, want nil", err)
		}
		if err := resumed.Seal(); err != nil {
			t.Fatalf("resumed.Seal(): %v, want nil", err)
		}
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
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
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
// NewSequence() equivalence test: both nil, or both the same Sequence sentinel.
func errorsEqual(a, b error) bool {
	if a == nil && b == nil {
		return true
	}
	for _, sentinel := range []error{ErrSequenceExhausted, ErrFloorUnproven, ErrFloorSealed} {
		if errors.Is(a, sentinel) && errors.Is(b, sentinel) {
			return true
		}
	}
	return false
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
			// Sealed BEFORE any goroutine starts: this test is about Next
			// racing Next, not about Next racing Seal (that race is covered by
			// TestSequenceRefusesToIssueFromAnUnsealedFloor/ConcurrentNextRacingSeal).
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
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
// and stays exhausted on every subsequent call. Reaching that check at all
// requires a Seal — the unsealed guard runs first, which is asserted in
// TestSequenceRefusesToIssueFromAnUnsealedFloor/GuardOrdering/UnsealedBeatsExhausted.
func TestSequenceAllocatorOverflow(t *testing.T) {
	t.Run("ResumeAtMaxIsImmediatelyExhausted", func(t *testing.T) {
		s := Resume(math.MaxUint64)
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
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
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
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
// RaiseFloor's no-op, success and error cases exactly as specified in the doc
// comment on Sequence.RaiseFloor.
//
// Since ID-2-WIRING-SEAL that boundary is drawn by the SEAL, not by whether
// anything has been issued: RaiseFloor is legal only while unsealed, and
// "something has been issued" implies "sealed", so ErrFloorBelowIssued is
// unreachable on a Sequence (it is still live per name on
// NameSuffixes.RaiseFloor, which has no seal).
func TestSequenceAllocatorRaiseFloor(t *testing.T) {
	t.Run("NothingIssuedAtLeastAtOrBelowFloorIsNoOp", func(t *testing.T) {
		s := Resume(10)
		if err := s.RaiseFloor(10); err != nil {
			t.Fatalf("RaiseFloor(10) on floor 10, nothing issued: %v, want nil (no-op)", err)
		}
		if err := s.RaiseFloor(5); err != nil {
			t.Fatalf("RaiseFloor(5) on floor 10, nothing issued: %v, want nil (no-op)", err)
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
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
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after RaiseFloor(20): unexpected error %v", err)
		}
		if n != 21 {
			t.Fatalf("Next() after RaiseFloor(20) = %d, want 21", n)
		}
	})

	t.Run("UnsealedRaisesFromSeveralSourcesInAnyOrder", func(t *testing.T) {
		// The "raises" behaviour where it is still legal: the assembly window,
		// before Seal. The floor is the MAXIMUM of the claims, and the order
		// they arrive in does not matter.
		s := NewSequence()
		for _, atLeast := range []uint64{7, 3, 42, 1, 42} {
			if err := s.RaiseFloor(atLeast); err != nil {
				t.Fatalf("RaiseFloor(%d) while unsealed: %v, want nil", atLeast, err)
			}
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next(): unexpected error %v", err)
		}
		if n != 43 {
			t.Fatalf("Next() after raising to a maximum of 42 = %d, want 43", n)
		}
	})

	t.Run("SealedRaiseFloorAtLastIsErrFloorSealed", func(t *testing.T) {
		// Was SomethingIssuedAtLeastAtLastIsError, expecting ErrFloorBelowIssued.
		// To have issued anything the allocator must be sealed, and the seal
		// check runs FIRST, so the equality case now reports ErrFloorSealed.
		// ErrFloorBelowIssued is unreachable on Sequence.
		s := NewSequence()
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		var last uint64
		for i := 0; i < 3; i++ {
			n, err := s.Next()
			if err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
			last = n
		}
		if err := s.RaiseFloor(last); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(%d) with Last()==%d on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", last, last, err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after rejected RaiseFloor: unexpected error %v", err)
		}
		if n != last+1 {
			t.Fatalf("Next() after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged)", last, n, last+1)
		}
	})

	t.Run("SealedRaiseFloorBelowLastIsErrFloorSealed", func(t *testing.T) {
		// Was SomethingIssuedAtLeastBelowLastIsError, expecting
		// ErrFloorBelowIssued — same inversion as above: the seal pre-empts it.
		s := NewSequence()
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		var last uint64
		for i := 0; i < 5; i++ {
			n, err := s.Next()
			if err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
			last = n
		}
		if err := s.RaiseFloor(last - 2); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(%d) with Last()==%d on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", last-2, last, err)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after rejected RaiseFloor: unexpected error %v", err)
		}
		if n != last+1 {
			t.Fatalf("Next() after rejected RaiseFloor(%d) = %d, want %d (allocator left unchanged)", last-2, n, last+1)
		}
	})

	t.Run("SealedRaiseFloorAboveLastIsRefusedAndFloorDoesNotMove", func(t *testing.T) {
		// Was SomethingIssuedAtLeastAboveLastRaises, which asserted the floor
		// MOVED to 10 and the next value was 11. That premise inverts under the
		// seal gate: assembly ended at Seal, so a later claim about the
		// high-water mark — even a higher one — is refused and the floor stays
		// exactly where it was.
		s := NewSequence()
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}
		for i := 0; i < 3; i++ {
			if _, err := s.Next(); err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
		}
		// Last() == 3, floor == 3 here.
		if err := s.RaiseFloor(10); !errors.Is(err, ErrFloorSealed) {
			t.Fatalf("RaiseFloor(10) with Last()==3 on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", err)
		}
		if got := s.Last(); got != 3 {
			t.Fatalf("Last() after rejected RaiseFloor(10) = %d, want 3", got)
		}
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after rejected RaiseFloor(10): unexpected error %v", err)
		}
		if n != 4 {
			t.Fatalf("Next() after rejected RaiseFloor(10) = %d, want 4 (the floor must NOT have moved to 10)", n)
		}
	})

	t.Run("RaiseFloorToMaxSucceedsAndExhausts", func(t *testing.T) {
		t.Run("nothing issued yet", func(t *testing.T) {
			s := NewSequence()
			if err := s.RaiseFloor(math.MaxUint64); err != nil {
				t.Fatalf("RaiseFloor(MaxUint64) while unsealed: %v, want nil", err)
			}
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			n, err := s.Next()
			if n != 0 || !errors.Is(err, ErrSequenceExhausted) {
				t.Fatalf("Next() after RaiseFloor(MaxUint64) = (%d, %v), want (0, ErrSequenceExhausted)", n, err)
			}
		})

		t.Run("something already issued (now refused)", func(t *testing.T) {
			// Was: RaiseFloor(MaxUint64) succeeded after a Next and left the
			// allocator exhausted. Inverted by the seal gate — issuing requires
			// a seal, so this RaiseFloor is refused and the allocator carries on
			// from where it was.
			s := NewSequence()
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal(): %v, want nil", err)
			}
			if _, err := s.Next(); err != nil {
				t.Fatalf("Next(): unexpected error %v", err)
			}
			if err := s.RaiseFloor(math.MaxUint64); !errors.Is(err, ErrFloorSealed) {
				t.Fatalf("RaiseFloor(MaxUint64) with Last()==1 on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", err)
			}
			n, err := s.Next()
			if err != nil {
				t.Fatalf("Next() after rejected RaiseFloor(MaxUint64): unexpected error %v", err)
			}
			if n != 2 {
				t.Fatalf("Next() after rejected RaiseFloor(MaxUint64) = %d, want 2 (not exhausted: the floor never moved)", n)
			}
		})
	})
}

// TestSequenceAllocatorRaiseFloorRace covers RaiseFloor racing under -race, in
// the two halves the seal gate leaves.
//
// Before ID-2-WIRING-SEAL this test proved "RaiseFloor racing with Next never
// duplicates". That race no longer exists on a live allocator: Next requires a
// seal and RaiseFloor is refused after one, so the two can never both succeed.
// Rather than let the test decay into asserting nothing, it is split:
//
//	SEALED  — refused RaiseFloor calls racing with Next must corrupt nothing.
//	UNSEALED — concurrent RaiseFloor calls racing the single Seal, which is the
//	           one RaiseFloor race that is still legal.
func TestSequenceAllocatorRaiseFloorRace(t *testing.T) {
	t.Run("SealedRefusedRaiseFloorNeverCorruptsNext", func(t *testing.T) {
		const nextGoroutines = 30
		const perGoroutine = 200
		const raiseGoroutines = 5
		const total = nextGoroutines * perGoroutine

		s := NewSequence()
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal(): %v, want nil", err)
		}

		results := make(chan uint64, total)
		var wg sync.WaitGroup

		wg.Add(nextGoroutines)
		for g := 0; g < nextGoroutines; g++ {
			go func() {
				defer wg.Done()
				for i := 0; i < perGoroutine; i++ {
					n, err := s.Next()
					if err != nil {
						// Sealed and bounded well below math.MaxUint64, so
						// there is no legal error here at all.
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
				for i := 1; i <= 20; i++ {
					atLeast := uint64(r*1000 + i*10)
					// Every one of these is refused: assembly ended at Seal.
					// The error is deterministic — it does not depend on how
					// far concurrent Next calls have got.
					if err := s.RaiseFloor(atLeast); !errors.Is(err, ErrFloorSealed) {
						t.Errorf("RaiseFloor(%d) on a sealed allocator: err = %v, want errors.Is(err, ErrFloorSealed)", atLeast, err)
					}
				}
			}()
		}

		wg.Wait()
		close(results)

		seen := make(map[uint64]bool, total)
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
		// The floor never moved, so the issued set is exactly 1..total. This is
		// the strong form of "a refused RaiseFloor changed nothing": had any of
		// the 100 refused calls leaked through, there would be a hole here.
		if len(seen) != total {
			t.Fatalf("got %d unique issued values, want %d", len(seen), total)
		}
		for i := uint64(1); i <= uint64(total); i++ {
			if !seen[i] {
				t.Fatalf("issued set is missing %d; a refused RaiseFloor must not move the floor, so the set must be exactly 1..%d", i, total)
			}
		}
	})

	t.Run("UnsealedConcurrentRaiseFloorRacingSeal", func(t *testing.T) {
		// The race that IS still legal: several sources push claims into the
		// assembly window while one goroutine closes it. Which claims land
		// before the seal is pure scheduling, so the exact final floor is NOT
		// asserted. The invariant that matters is timing-independent: every
		// RaiseFloor that returned nil made a promise that the floor is at least
		// atLeast, and the sealed floor must honour every one of them.
		const raiseGoroutines = 8
		const perGoroutine = 40

		s := NewSequence()

		var (
			mu      sync.Mutex
			claimed []uint64 // atLeast values whose RaiseFloor returned nil
		)

		var wg sync.WaitGroup
		wg.Add(raiseGoroutines)
		for r := 0; r < raiseGoroutines; r++ {
			r := r
			go func() {
				defer wg.Done()
				for i := 1; i <= perGoroutine; i++ {
					atLeast := uint64(r*100 + i)
					err := s.RaiseFloor(atLeast)
					switch {
					case err == nil:
						mu.Lock()
						claimed = append(claimed, atLeast)
						mu.Unlock()
					case errors.Is(err, ErrFloorSealed):
						// Legal: this call lost the race to Seal.
					default:
						t.Errorf("RaiseFloor(%d) while assembling: err = %v, want nil or ErrFloorSealed", atLeast, err)
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

		// Nothing was issued during assembly, so Last() must still be 0.
		if got := s.Last(); got != 0 {
			t.Fatalf("Last() after an assembly-only race = %d, want 0 (nothing was issued)", got)
		}

		// The seal has happened (it is inside the WaitGroup), so this Next
		// succeeds and reveals the sealed floor as n-1.
		n, err := s.Next()
		if err != nil {
			t.Fatalf("Next() after the seal barrier: unexpected error %v", err)
		}
		floor := n - 1
		for _, atLeast := range claimed {
			if floor < atLeast {
				t.Fatalf("sealed floor = %d, but RaiseFloor(%d) returned nil; the sealed floor must be >= every accepted claim", floor, atLeast)
			}
		}
	})
}
