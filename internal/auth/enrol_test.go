package auth_test

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// TestEnrollMintsServerAuthoritativeIDs pins the shape and the source of an
// enrolled agent's identity (invariants 1 and 2): the client asks for a NAME,
// the server decides the ID, and the per-name suffix is monotonic.
func TestEnrollMintsServerAuthoritativeIDs(t *testing.T) {
	svc, clock := newService(t, auth.Options{})

	pub1, _ := newKeypair(t)
	res1, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub1, IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatalf("first enrolment: %v", err)
	}

	t.Run("the first enrolment of a name is suffix 1", func(t *testing.T) {
		if want := testBusID + ".alpha-1"; res1.AgentID != want {
			t.Errorf("agent id = %q, want %q", res1.AgentID, want)
		}
		if res1.BusID != testBusID {
			t.Errorf("bus id = %q, want %q", res1.BusID, testBusID)
		}
		if res1.Name != "alpha" {
			t.Errorf("name = %q, want %q", res1.Name, "alpha")
		}
		if !res1.EnrolledAt.Equal(epoch) {
			t.Errorf("enrolled at %s, want the injected clock's %s", res1.EnrolledAt, epoch)
		}
		if res1.Replayed {
			t.Error("Replayed = true on a fresh enrolment; it must describe THIS call, and nothing was replayed")
		}
	})

	t.Run("a second enrolment of the SAME name gets suffix 2, never the same id", func(t *testing.T) {
		clock.Advance(time.Minute)
		pub2, _ := newKeypair(t)
		res2, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub2, IdempotencyKey: "idem-2"})
		if err != nil {
			t.Fatalf("second enrolment: %v", err)
		}
		if want := testBusID + ".alpha-2"; res2.AgentID != want {
			t.Fatalf("agent id = %q, want %q; a reissued suffix is a reissued AGENT ID (invariant 1)", res2.AgentID, want)
		}
		if res2.AgentID == res1.AgentID {
			t.Fatal("the second enrolment of a name was given the first one's id")
		}
	})

	t.Run("a different name has its own counter starting at 1", func(t *testing.T) {
		pub, _ := newKeypair(t)
		res, err := svc.Enrol(auth.EnrolRequest{Name: "beta", PublicKey: pub, IdempotencyKey: "idem-3"})
		if err != nil {
			t.Fatalf("enrolling a second name: %v", err)
		}
		if want := testBusID + ".beta-1"; res.AgentID != want {
			t.Errorf("agent id = %q, want %q; distinct names have entirely independent counters", res.AgentID, want)
		}
	})
}

// TestEnrollRecordsThePresentedPublicKeyInTheRoster pins the binding invariant
// 3 rests on: the roster entry holds the key that was presented, byte for byte,
// so a later caller cannot authenticate as that id with a different keypair.
func TestEnrollRecordsThePresentedPublicKeyInTheRoster(t *testing.T) {
	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})

	pub, _ := newKeypair(t)
	res, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatalf("enrolling: %v", err)
	}

	entry, ok := roster.Get(res.AgentID)
	if !ok {
		t.Fatalf("roster has no entry for the freshly enrolled %q", res.AgentID)
	}
	if entry.AgentID != res.AgentID {
		t.Errorf("roster entry id = %q, want %q", entry.AgentID, res.AgentID)
	}
	if entry.Name != "alpha" {
		t.Errorf("roster entry name = %q, want %q; the stored name must be the name half of the id, byte for byte", entry.Name, "alpha")
	}
	if !entry.PublicKey.Equal(pub) {
		t.Error("the roster's public key is not the one presented at enrolment")
	}
	if !entry.EnrolledAt.Equal(epoch) {
		t.Errorf("roster entry enrolled at %s, want the injected clock's %s", entry.EnrolledAt, epoch)
	}
	if roster.Len() != 1 {
		t.Errorf("roster holds %d entries, want 1", roster.Len())
	}

	t.Run("the returned key is a copy the caller cannot use to rewrite the credential", func(t *testing.T) {
		entry.PublicKey[0] ^= 0xff
		again, ok := roster.Get(res.AgentID)
		if !ok {
			t.Fatal("roster entry vanished")
		}
		if !again.PublicKey.Equal(pub) {
			t.Fatal("mutating the returned slice changed the STORED credential; a caller must not be able to reach into the roster through the key it was handed")
		}
	})
}

// TestEnrollRejectsWrongSizePublicKeysWithoutPanicking is the RATCHET-7
// acceptance criterion for this task.
//
// ed25519.Verify PANICS when len(publicKey) != ed25519.PublicKeySize — it does
// NOT return false, which is what a wrong-size SIGNATURE gets. The asymmetry is
// the trap: a call site that only guards the signature looks correct and is a
// remote denial of service, because the public key on this route is
// unauthenticated, client-supplied input. Every wrong size must be a plain
// validation error, and nothing may reach Verify.
func TestEnrollRejectsWrongSizePublicKeysWithoutPanicking(t *testing.T) {
	cases := []struct {
		name string
		key  ed25519.PublicKey
	}{
		{"nil key", nil},
		{"empty key", ed25519.PublicKey{}},
		{"four bytes", make([]byte, 4)},
		{"one byte short", make([]byte, ed25519.PublicKeySize-1)},
		{"one byte long", make([]byte, ed25519.PublicKeySize+1)},
		{"a whole private key presented as a public one", make([]byte, ed25519.PrivateKeySize)},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			roster := auth.NewMemoryRoster()
			svc, _ := newService(t, auth.Options{Roster: roster})

			var (
				err    error
				called bool
			)
			mustNotPanic(t, "Enrol with a "+tc.name, func() {
				_, err = svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: tc.key, IdempotencyKey: "idem-1"})
				called = true
			})
			if !called {
				t.Fatal("Enrol did not return at all")
			}
			if !errors.Is(err, auth.ErrInvalidPublicKey) {
				t.Fatalf("err = %v, want one wrapping ErrInvalidPublicKey", err)
			}
			if roster.Len() != 0 {
				t.Errorf("roster holds %d entries after a rejected enrolment, want 0", roster.Len())
			}
		})
	}

	t.Run("MemoryRoster refuses a wrong-size key even when handed one directly", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		err := roster.Put(auth.RosterEntry{AgentID: testBusID + ".alpha-1", Name: "alpha", PublicKey: make([]byte, 8)})
		if !errors.Is(err, auth.ErrInvalidPublicKey) {
			t.Fatalf("Put err = %v, want one wrapping ErrInvalidPublicKey; this is the boundary that hands keys to ed25519.Verify", err)
		}
	})

	t.Run("a wrong-size key already in the roster cannot reach ed25519.Verify", func(t *testing.T) {
		// The state AUTH-3 makes reachable: the roster is rebuilt from disk and
		// a truncated record yields a short key. CompleteSession's own length
		// check must catch it, on an unauthenticated route, without panicking.
		roster := newStubRoster()
		svc, _ := newService(t, auth.Options{Roster: roster})

		agentID := testBusID + ".alpha-1"
		if err := roster.Put(auth.RosterEntry{AgentID: agentID, Name: "alpha", PublicKey: make([]byte, ed25519.PublicKeySize-1)}); err != nil {
			t.Fatalf("seeding the stub roster: %v", err)
		}

		var ch auth.Challenge
		mustNotPanic(t, "BeginSession for an agent whose roster key is short", func() {
			var err error
			ch, err = svc.BeginSession(agentID)
			if err != nil {
				t.Fatalf("BeginSession: %v", err)
			}
		})

		var err error
		mustNotPanic(t, "CompleteSession against a short roster key", func() {
			_, err = svc.CompleteSession(ch.Token, make([]byte, ed25519.SignatureSize))
		})
		if !errors.Is(err, auth.ErrInvalidPublicKey) {
			t.Fatalf("err = %v, want one wrapping ErrInvalidPublicKey", err)
		}
	})
}

// TestEnrollRejectsInvalidNames checks that name validation is enforced and is
// delegated to ids.ValidateAgentName — one case per rejection class, not an
// exhaustive re-test of that package.
func TestEnrollRejectsInvalidNames(t *testing.T) {
	cases := []struct {
		name    string
		agent   string
		because string
	}{
		{"empty", "", "a name is required"},
		{"uppercase", "Alpha", "uppercase is REJECTED, not case-folded, so the input bytes are the counter key and the id bytes"},
		{"contains a dot", "al.pha", "'.' is the <bus-id>.<agent-id> separator (invariant 2)"},
		{"over long", strings.Repeat("a", 65), "the pattern admits at most 64 bytes"},
		{"leading hyphen", "-alpha", "a name must never be mistakable for a flag"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			roster := auth.NewMemoryRoster()
			svc, _ := newService(t, auth.Options{Roster: roster})
			pub, _ := newKeypair(t)

			_, err := svc.Enrol(auth.EnrolRequest{Name: tc.agent, PublicKey: pub, IdempotencyKey: "idem-1"})
			if !errors.Is(err, auth.ErrInvalidName) {
				t.Fatalf("err = %v, want one wrapping ErrInvalidName (%s)", err, tc.because)
			}
			if roster.Len() != 0 {
				t.Errorf("roster holds %d entries after a rejected enrolment, want 0", roster.Len())
			}
		})
	}
}

// TestEnrollRejectsInvalidIdempotencyKeys pins the shape of the key that makes
// enrolment safe to retry. The key is reflected into the server log, so it is
// bounded and character-restricted rather than escaped and kept.
func TestEnrollRejectsInvalidIdempotencyKeys(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"one byte over the limit", strings.Repeat("k", auth.MaxIdempotencyKeyLen+1)},
		{"contains a slash", "idem/1"},
		{"contains a space", "idem 1"},
		{"contains a newline", "idem\n1"},
		{"contains a NUL", "idem\x001"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			roster := auth.NewMemoryRoster()
			svc, _ := newService(t, auth.Options{Roster: roster})
			pub, _ := newKeypair(t)

			_, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: tc.key})
			if !errors.Is(err, auth.ErrInvalidIdempotencyKey) {
				t.Fatalf("err = %v, want one wrapping ErrInvalidIdempotencyKey", err)
			}
			if roster.Len() != 0 {
				t.Errorf("roster holds %d entries after a rejected enrolment, want 0", roster.Len())
			}
		})
	}

	t.Run("a key of exactly the maximum length is accepted", func(t *testing.T) {
		svc, _ := newService(t, auth.Options{})
		pub, _ := newKeypair(t)
		if _, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: strings.Repeat("k", auth.MaxIdempotencyKeyLen)}); err != nil {
			t.Fatalf("a key of exactly MaxIdempotencyKeyLen bytes was rejected: %v", err)
		}
	})
}

// TestEnrollSameKeySamePayloadReplaysTheOriginal is the half of invariant 10
// that must not be punished: a retry after a lost acknowledgement returns the
// ORIGINAL result, applies nothing, and mints no second id.
func TestEnrollSameKeySamePayloadReplaysTheOriginal(t *testing.T) {
	roster := auth.NewMemoryRoster()
	svc, clock := newService(t, auth.Options{Roster: roster})

	pub, _ := newKeypair(t)
	req := auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: "idem-1"}

	first, err := svc.Enrol(req)
	if err != nil {
		t.Fatalf("first enrolment: %v", err)
	}

	// Time moves between the call and the retry, as it would in life. The
	// replayed result must carry the ORIGINAL instant.
	clock.Advance(90 * time.Second)

	second, err := svc.Enrol(req)
	if err != nil {
		t.Fatalf("retrying with the same key and the same payload must succeed, not error: %v", err)
	}

	if second.AgentID != first.AgentID {
		t.Fatalf("retry minted a SECOND id %q for the same agent (original %q); the client has no way to tell, which is exactly the double-apply invariant 10 forbids", second.AgentID, first.AgentID)
	}
	if second.BusID != first.BusID || second.Name != first.Name {
		t.Errorf("replayed result = %+v, want the original %+v", second, first)
	}
	if !second.EnrolledAt.Equal(first.EnrolledAt) {
		t.Errorf("replayed EnrolledAt = %s, want the ORIGINAL %s: the result of an idempotent retry is the original result", second.EnrolledAt, first.EnrolledAt)
	}
	if !second.Replayed {
		t.Error("Replayed = false on a retry; the handler needs it to set the Idempotency-Replayed header")
	}
	if roster.Len() != 1 {
		t.Errorf("roster holds %d entries after one enrolment and one retry, want 1", roster.Len())
	}

	t.Run("the retry burned no suffix", func(t *testing.T) {
		pub2, _ := newKeypair(t)
		next, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub2, IdempotencyKey: "idem-2"})
		if err != nil {
			t.Fatalf("enrolling alpha again: %v", err)
		}
		if want := testBusID + ".alpha-2"; next.AgentID != want {
			t.Fatalf("next id = %q, want %q; a replay must not reach the minter at all", next.AgentID, want)
		}
	})
}

// TestEnrollSameKeyDifferentPayloadIsAProtocolViolation is the other half of
// invariant 10, the one that must not be collapsed into the first: reusing a
// key for NEW content is a serious client bug or an attack, and nothing is
// applied.
func TestEnrollSameKeyDifferentPayloadIsAProtocolViolation(t *testing.T) {
	pubA, _ := newKeypair(t)
	pubB, _ := newKeypair(t)

	cases := []struct {
		name    string
		mutate  func(auth.EnrolRequest) auth.EnrolRequest
		because string
	}{
		{
			name:    "same key with a different name",
			mutate:  func(r auth.EnrolRequest) auth.EnrolRequest { r.Name = "beta"; return r },
			because: "the key was applied for a different agent name",
		},
		{
			name:    "same key with a different public key",
			mutate:  func(r auth.EnrolRequest) auth.EnrolRequest { r.PublicKey = pubB; return r },
			because: "accepting this would rebind an identity to a different keypair",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			roster := auth.NewMemoryRoster()
			svc, _ := newService(t, auth.Options{Roster: roster})

			orig := auth.EnrolRequest{Name: "alpha", PublicKey: pubA, IdempotencyKey: "idem-1"}
			first, err := svc.Enrol(orig)
			if err != nil {
				t.Fatalf("first enrolment: %v", err)
			}

			res, err := svc.Enrol(tc.mutate(orig))
			if !errors.Is(err, auth.ErrIdempotencyKeyReused) {
				t.Fatalf("err = %v, want one wrapping ErrIdempotencyKeyReused (%s)", err, tc.because)
			}
			if res.AgentID != "" {
				t.Errorf("a rejected reuse returned agent id %q, want the zero result", res.AgentID)
			}
			if roster.Len() != 1 {
				t.Errorf("roster holds %d entries, want 1: NOTHING may be applied by a rejected reuse", roster.Len())
			}

			// And the original is untouched: a violation by a broken client must
			// not destroy the record of what was legitimately applied.
			replay, err := svc.Enrol(orig)
			if err != nil {
				t.Fatalf("replaying the ORIGINAL payload after a rejected reuse: %v", err)
			}
			if replay.AgentID != first.AgentID || !replay.Replayed {
				t.Fatalf("replay = %+v, want the original %q replayed", replay, first.AgentID)
			}
		})
	}
}

// TestEnrollCapacityFailsClosed pins that every bound REFUSES rather than
// evicts. Evicting a remembered idempotency key silently converts the next
// replay into a fresh application — a second agent id for one agent — so a
// refused enrolment (recoverable) is always preferred to a silently duplicated
// one (not).
func TestEnrollCapacityFailsClosed(t *testing.T) {
	t.Run("a full roster refuses a new enrolment", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		svc, _ := newService(t, auth.Options{Roster: roster, MaxRosterEntries: 2})

		for i := 1; i <= 2; i++ {
			pub, _ := newKeypair(t)
			if _, err := svc.Enrol(auth.EnrolRequest{Name: fmt.Sprintf("agent%d", i), PublicKey: pub, IdempotencyKey: fmt.Sprintf("idem-%d", i)}); err != nil {
				t.Fatalf("enrolment %d: %v", i, err)
			}
		}

		pub, _ := newKeypair(t)
		_, err := svc.Enrol(auth.EnrolRequest{Name: "agent3", PublicKey: pub, IdempotencyKey: "idem-3"})
		if !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("err = %v, want one wrapping ErrCapacity", err)
		}
		if roster.Len() != 2 {
			t.Errorf("roster holds %d entries, want 2: the cap must refuse, never evict", roster.Len())
		}
	})

	t.Run("a full roster still replays an enrolment it already accepted", func(t *testing.T) {
		// Idempotency is checked BEFORE admission control, deliberately: the
		// agent is already in that roster, so replaying its result admits
		// nobody new, and a retry must not start failing because the bus filled
		// up afterwards.
		roster := auth.NewMemoryRoster()
		svc, _ := newService(t, auth.Options{Roster: roster, MaxRosterEntries: 1})

		pub, _ := newKeypair(t)
		req := auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: "idem-1"}
		first, err := svc.Enrol(req)
		if err != nil {
			t.Fatalf("first enrolment: %v", err)
		}

		other, _ := newKeypair(t)
		if _, err := svc.Enrol(auth.EnrolRequest{Name: "beta", PublicKey: other, IdempotencyKey: "idem-2"}); !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("second enrolment err = %v, want one wrapping ErrCapacity", err)
		}

		replay, err := svc.Enrol(req)
		if err != nil {
			t.Fatalf("replaying a recorded key at capacity: %v", err)
		}
		if replay.AgentID != first.AgentID || !replay.Replayed {
			t.Fatalf("replay = %+v, want the original %q replayed; nothing may have been evicted", replay, first.AgentID)
		}
	})

	t.Run("a full idempotency table refuses rather than forgetting a key", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		svc, _ := newService(t, auth.Options{Roster: roster, MaxIdempotencyEntries: 1})

		pub, _ := newKeypair(t)
		req := auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: "idem-1"}
		first, err := svc.Enrol(req)
		if err != nil {
			t.Fatalf("first enrolment: %v", err)
		}

		other, _ := newKeypair(t)
		_, err = svc.Enrol(auth.EnrolRequest{Name: "beta", PublicKey: other, IdempotencyKey: "idem-2"})
		if !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("err = %v, want one wrapping ErrCapacity", err)
		}
		if roster.Len() != 1 {
			t.Errorf("roster holds %d entries, want 1: the idempotency cap is checked BEFORE the mint so a full table cannot burn suffixes", roster.Len())
		}

		replay, err := svc.Enrol(req)
		if err != nil {
			t.Fatalf("replaying the recorded key after the table filled: %v", err)
		}
		if replay.AgentID != first.AgentID || !replay.Replayed {
			t.Fatalf("replay = %+v, want the original %q replayed; an evicted key would silently re-apply", replay, first.AgentID)
		}
	})
}

// TestEnrollServiceRequiresAnInjectedMinter pins that a missing minter is an
// ERROR and never a convenience default. A defaulted minter would start every
// name at suffix 1 with nothing on disk behind it and would re-mint live agent
// ids the first time it ran on a bus with history.
func TestEnrollServiceRequiresAnInjectedMinter(t *testing.T) {
	svc, err := auth.NewService(auth.Options{})
	if err == nil {
		t.Fatal("NewService with a nil Minter returned no error; a defaulted minter re-mints live agent ids (invariant 1)")
	}
	if svc != nil {
		t.Errorf("NewService returned a non-nil service alongside an error: %#v", svc)
	}
}

// TestEnrollConcurrentSameNameMintsDistinctIDs is the invariant-1 test that
// matters. N clients racing for one name must each receive a DIFFERENT id: an
// id handed out twice means two principals with different keypairs share one
// routing and authorization subject.
func TestEnrollConcurrentSameNameMintsDistinctIDs(t *testing.T) {
	const n = 64

	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  = make(map[string]int)
		errs []error
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			pub, _, err := ed25519.GenerateKey(nil) // nil reader = crypto/rand
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			res, err := svc.Enrol(auth.EnrolRequest{
				Name:           "swarm",
				PublicKey:      pub,
				IdempotencyKey: fmt.Sprintf("idem-%d", i),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			got[res.AgentID]++
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent enrolment failed: %v", err)
	}
	if len(got) != n {
		t.Fatalf("%d concurrent enrolments of one name produced %d DISTINCT ids, want %d", n, len(got), n)
	}

	// Every id is well formed, carries this bus, this name, and a suffix in
	// 1..n used exactly once.
	seen := make(map[uint64]bool, n)
	for id, count := range got {
		if count != 1 {
			t.Errorf("agent id %q was handed out %d times; an id is never reused (invariant 1)", id, count)
		}
		bus, name, suffix, err := ids.ParseAgentID(id)
		if err != nil {
			t.Errorf("minted id %q does not parse: %v", id, err)
			continue
		}
		if bus != testBusID || name != "swarm" {
			t.Errorf("id %q parsed as bus %q name %q, want %q / %q", id, bus, name, testBusID, "swarm")
		}
		if suffix < 1 || suffix > n {
			t.Errorf("id %q has suffix %d, outside 1..%d", id, suffix, n)
		}
		if seen[suffix] {
			t.Errorf("suffix %d was issued twice", suffix)
		}
		seen[suffix] = true
	}
	if roster.Len() != n {
		t.Errorf("roster holds %d entries, want %d", roster.Len(), n)
	}
}
