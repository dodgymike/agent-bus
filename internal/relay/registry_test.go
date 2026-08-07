package relay

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func newTestRegistry(t *testing.T, opts ...func(*RegistryOptions)) *Registry {
	t.Helper()
	o := RegistryOptions{BusID: localBus}
	for _, f := range opts {
		f(&o)
	}
	r, err := NewRegistry(o)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func mustUpsert(t *testing.T, r *Registry, busID string, agents ...string) {
	t.Helper()
	if err := r.UpsertPeer(PeerRoster{BusID: busID, Agents: agents}); err != nil {
		t.Fatalf("UpsertPeer(%s): %v", busID, err)
	}
}

// TestRegistryRoutesByBusHalfNotByRoster is the point of the whole design: a
// fully-qualified id NAMES ITS OWN OWNER (invariant 2), so a DM routes even
// when our copy of the peer's roster is stale. If routing depended on roster
// membership, every agent that enrolled remotely since our last sync would be
// undeliverable — turning an eventual-consistency mechanism into a delivery
// guarantee it cannot provide.
func TestRegistryRoutesByBusHalfNotByRoster(t *testing.T) {
	r := newTestRegistry(t)
	mustUpsert(t, r, peerBus, peerBus+".beta-1")

	// An agent that enrolled on the peer AFTER our last sync: not in our roster
	// copy, but unambiguously the peer's.
	stale := peerBus + ".just-enrolled-9"
	if got, ok := r.Route(stale); !ok || got != peerBus {
		t.Errorf("Route(%q) = %q/%v, want %q/true: routing must not depend on roster freshness", stale, got, ok, peerBus)
	}
	if r.Knows(stale) {
		t.Error("Knows() reported an agent that is not in our roster copy; it answers a DISCOVERY question and must be honest about staleness")
	}
	if !r.Knows(peerBus + ".beta-1") {
		t.Error("Knows() did not report an agent that IS in our roster copy")
	}

	t.Run("refuses to route our own bus", func(t *testing.T) {
		if got, ok := r.Route(localBus + ".alpha-1"); ok {
			t.Errorf("Route(%q) = %q/true; that is a LOCAL delivery, and relaying it would send the message out to be dropped as a loop", localBus+".alpha-1", got)
		}
		if _, ok := r.Route(strings.ToUpper(localBus) + ".alpha-1"); ok {
			t.Error("a case variant of our own bus id was routed off-bus")
		}
	})

	t.Run("refuses a malformed id", func(t *testing.T) {
		for _, id := range []string{"", "beta-1", "bus.x.beta-1", peerBus + ".beta", peerBus + ".Beta-1"} {
			if _, ok := r.Route(id); ok {
				t.Errorf("Route(%q) succeeded; a caller that has not been through ParseAgentID has not established that the string names anybody", id)
			}
		}
	})

	t.Run("refuses an unknown bus", func(t *testing.T) {
		if _, ok := r.Route(thirdBus + ".delta-1"); ok {
			t.Error("routed to a bus that has never handshaked")
		}
	})
}

// TestRegistryRefusesACaseCollidingPeer pays the debt doc.go's "What the gating
// tasks must not forget" recorded: ValidatePeerBusID folds a claim against OUR
// id alone, so two DIFFERENT peers whose ids differ only by case both validate.
func TestRegistryRefusesACaseCollidingPeer(t *testing.T) {
	r := newTestRegistry(t)
	mustUpsert(t, r, peerBus, peerBus+".beta-1")

	err := r.UpsertPeer(PeerRoster{BusID: strings.ToUpper(peerBus)})
	if !errors.Is(err, ErrPeerBusIDCollision) {
		t.Fatalf("error = %v, want one wrapping ErrPeerBusIDCollision", err)
	}
	// The original peer is untouched.
	if agents, _, ok := r.Roster(peerBus); !ok || len(agents) != 1 {
		t.Fatalf("the rejected upsert disturbed the known peer: %v/%v", agents, ok)
	}
	if got := ErrorCode(err); got != CodeBusIDCollision {
		t.Errorf("ErrorCode = %q, want %q", got, CodeBusIDCollision)
	}
}

func TestRegistryEnforcesMaxPeers(t *testing.T) {
	r := newTestRegistry(t, func(o *RegistryOptions) { o.MaxPeers = 2 })
	mustUpsert(t, r, "bus-one")
	mustUpsert(t, r, "bus-two")

	if err := r.UpsertPeer(PeerRoster{BusID: "bus-three"}); !errors.Is(err, ErrTooManyPeers) {
		t.Fatalf("error = %v, want one wrapping ErrTooManyPeers", err)
	}
	// Re-upserting a KNOWN peer is not a new peer and must still work at the cap.
	if err := r.UpsertPeer(PeerRoster{BusID: "bus-one", Agents: []string{"bus-one.x-1"}}); err != nil {
		t.Fatalf("re-upserting a known peer at the cap failed: %v", err)
	}
	if n := len(r.Peers()); n != 2 {
		t.Fatalf("registry holds %d peers, want 2", n)
	}
}

// TestRegistryUpsertReplacesTheRosterAndResetsTheVersion pins the two halves of
// the handshake's semantics: a full snapshot replaces (never merges), and the
// version resets so a restarted peer can recover from a regressed counter.
func TestRegistryUpsertReplacesTheRosterAndResetsTheVersion(t *testing.T) {
	r := newTestRegistry(t)
	mustUpsert(t, r, peerBus, peerBus+".beta-1", peerBus+".gamma-1")
	if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 5, Added: []string{peerBus + ".delta-1"}}); err != nil {
		t.Fatalf("ApplyRosterUpdate: %v", err)
	}

	mustUpsert(t, r, peerBus, peerBus+".epsilon-1")
	agents, version, ok := r.Roster(peerBus)
	if !ok {
		t.Fatal("the peer disappeared")
	}
	if len(agents) != 1 || agents[0] != peerBus+".epsilon-1" {
		t.Errorf("roster = %v, want exactly the handshake snapshot; merging would preserve entries the peer just told us it no longer has", agents)
	}
	if version != 0 {
		t.Errorf("version = %d, want 0: a re-handshake is the documented recovery from a regressed peer counter, and it only works if it resets", version)
	}
	// And a low version is now accepted again, which is the recovery working.
	if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + ".zeta-1"}}); err != nil {
		t.Fatalf("a post-handshake low version was refused: %v", err)
	}
}

func TestRegistryApplyRosterUpdate(t *testing.T) {
	t.Run("adds and removes incrementally", func(t *testing.T) {
		r := newTestRegistry(t)
		mustUpsert(t, r, peerBus, peerBus+".beta-1", peerBus+".gamma-1")

		if err := r.ApplyRosterUpdate(RosterUpdate{
			BusID:   peerBus,
			Version: 1,
			Added:   []string{peerBus + ".delta-1"},
			Removed: []string{peerBus + ".beta-1"},
		}); err != nil {
			t.Fatalf("ApplyRosterUpdate: %v", err)
		}
		agents, version, _ := r.Roster(peerBus)
		want := []string{peerBus + ".delta-1", peerBus + ".gamma-1"}
		if fmt.Sprint(agents) != fmt.Sprint(want) {
			t.Errorf("roster = %v, want %v", agents, want)
		}
		if version != 1 {
			t.Errorf("version = %d, want 1", version)
		}
	})

	t.Run("adding a present id or removing an absent one is a no-op", func(t *testing.T) {
		r := newTestRegistry(t)
		mustUpsert(t, r, peerBus, peerBus+".beta-1")
		if err := r.ApplyRosterUpdate(RosterUpdate{
			BusID: peerBus, Version: 1,
			Added:   []string{peerBus + ".beta-1"},
			Removed: []string{peerBus + ".nobody-1"},
		}); err != nil {
			t.Fatalf("an idempotent update was refused: %v", err)
		}
		if agents, _, _ := r.Roster(peerBus); len(agents) != 1 {
			t.Errorf("roster = %v, want unchanged", agents)
		}
	})

	// Each case is one deviation, and each MUST leave the roster exactly as it
	// was — a half-applied update is a routing table nobody validated.
	t.Run("refuses and changes nothing", func(t *testing.T) {
		tooMany := make([]string, MaxRosterUpdateEntries+1)
		for i := range tooMany {
			tooMany[i] = fmt.Sprintf("%s.a%d-1", peerBus, i)
		}

		cases := []struct {
			name    string
			update  RosterUpdate
			want    error
			because string
		}{
			{
				name:    "unknown peer",
				update:  RosterUpdate{BusID: thirdBus, Version: 1, Added: []string{thirdBus + ".x-1"}},
				want:    ErrUnknownPeer,
				because: "an update must never create a peer, or roster sync would be a second, ungated enrolment path",
			},
			{
				name:    "too many entries",
				update:  RosterUpdate{BusID: peerBus, Version: 1, Added: tooMany},
				want:    ErrInvalidRosterUpdate,
				because: "the count is refused before any id is parsed",
			},
			{
				name:    "stale version",
				update:  RosterUpdate{BusID: peerBus, Version: 3, Added: []string{peerBus + ".x-1"}},
				want:    ErrStaleRosterUpdate,
				because: "version 3 is already applied; only a STRICTLY greater version is accepted",
			},
			{
				name:    "out of order version",
				update:  RosterUpdate{BusID: peerBus, Version: 2, Added: []string{peerBus + ".x-1"}},
				want:    ErrStaleRosterUpdate,
				because: "an update that lost a race must not resurrect a departed agent",
			},
			{
				name:    "id in our namespace",
				update:  RosterUpdate{BusID: peerBus, Version: 4, Added: []string{localBus + ".alpha-1"}},
				want:    ErrBusIDCollision,
				because: "a peer adding an id in OUR namespace could make us route our own agent's mail off-bus",
			},
			{
				name:    "id in our namespace, different case",
				update:  RosterUpdate{BusID: peerBus, Version: 4, Added: []string{strings.ToUpper(localBus) + ".alpha-1"}},
				want:    ErrBusIDCollision,
				because: "case-folding the bus half must not open the namespace back up",
			},
			{
				name:    "id of a third bus",
				update:  RosterUpdate{BusID: peerBus, Version: 4, Added: []string{thirdBus + ".delta-1"}},
				want:    ErrInvalidRosterUpdate,
				because: "a peer speaks only for its own agents; transitive federation is not a sync side effect",
			},
			{
				name:    "malformed id",
				update:  RosterUpdate{BusID: peerBus, Version: 4, Removed: []string{"not-an-agent-id"}},
				want:    ErrInvalidRosterUpdate,
				because: "an id we cannot parse names nobody",
			},
			{
				name:    "duplicate within a list",
				update:  RosterUpdate{BusID: peerBus, Version: 4, Added: []string{peerBus + ".x-1", peerBus + ".x-1"}},
				want:    ErrInvalidRosterUpdate,
				because: "a delta list is a set",
			},
			{
				name: "added and removed overlap",
				update: RosterUpdate{BusID: peerBus, Version: 4,
					Added:   []string{peerBus + ".x-1"},
					Removed: []string{peerBus + ".x-1"}},
				want:    ErrInvalidRosterUpdate,
				because: "the intent is ambiguous and guessing would silently pick one",
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				r := newTestRegistry(t)
				mustUpsert(t, r, peerBus, peerBus+".beta-1", peerBus+".gamma-1")
				if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 3}); err != nil {
					t.Fatalf("seeding version 3: %v", err)
				}
				before, versionBefore, _ := r.Roster(peerBus)

				err := r.ApplyRosterUpdate(tc.update)
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want one wrapping %v (%s)", err, tc.want, tc.because)
				}
				after, versionAfter, _ := r.Roster(peerBus)
				if fmt.Sprint(before) != fmt.Sprint(after) {
					t.Errorf("a REJECTED update changed the roster: %v -> %v; validation must complete before anything is mutated", before, after)
				}
				if versionBefore != versionAfter {
					t.Errorf("a REJECTED update moved the version: %d -> %d", versionBefore, versionAfter)
				}
			})
		}
	})

	t.Run("refuses to grow the roster past the handshake cap", func(t *testing.T) {
		r := newTestRegistry(t)
		full := make([]string, MaxRosterAgents)
		for i := range full {
			full[i] = fmt.Sprintf("%s.a%d-1", peerBus, i)
		}
		mustUpsert(t, r, peerBus, full...)

		err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + ".onemore-1"}})
		if !errors.Is(err, ErrRosterTooLarge) {
			t.Fatalf("error = %v, want one wrapping ErrRosterTooLarge: a peer must not grow our routing table past the cap one increment at a time", err)
		}
		// Swapping one for another stays at the cap and is fine.
		if err := r.ApplyRosterUpdate(RosterUpdate{
			BusID: peerBus, Version: 1,
			Added:   []string{peerBus + ".onemore-1"},
			Removed: []string{full[0]},
		}); err != nil {
			t.Fatalf("a size-neutral update at the cap was refused: %v", err)
		}
	})
}

// rosterSnapshot renders a peer's roster and version as one comparable string,
// so "unchanged" can be asserted BYTE FOR BYTE rather than by eyeballing a
// length. Roster() already sorts and copies, so the rendering is stable.
func rosterSnapshot(t *testing.T, r *Registry, busID string) string {
	t.Helper()
	agents, version, ok := r.Roster(busID)
	if !ok {
		t.Fatalf("Roster(%q) reports no such peer", busID)
	}
	return fmt.Sprintf("v%d:[%s]", version, strings.Join(agents, " "))
}

// TestRegistryApplyRosterUpdateIsAtomic pins step 7 of ApplyRosterUpdate's
// contract: EVERYTHING is validated before ANYTHING is mutated.
//
// The existing table proves that for updates that are wholly bad. These three
// are the harder shape — updates that are PARTLY GOOD, so a validator that
// applied as it walked would have committed real work before it noticed:
//
//   - a valid addition sitting next to one in OUR namespace;
//   - a valid Added list whose Removed list is malformed, so the addition has
//     already passed validateDelta when the failure lands;
//   - an update that survives every id check and dies at the SIZE cap, which is
//     the last gate before the mutation. Its removal would have made room, so
//     an implementation that removed first and added second would leave the
//     roster one agent short and the caller told the update failed — a routing
//     table nobody validated, with nothing to roll back to.
//
// "Unchanged" is asserted byte for byte, roster AND version, because a version
// that moved on a rejected update is just as corrosive as a roster that did: it
// would make the peer's NEXT, legitimate update look stale forever.
func TestRegistryApplyRosterUpdateIsAtomic(t *testing.T) {
	t.Run("a good id next to one in our namespace", func(t *testing.T) {
		r := newTestRegistry(t)
		mustUpsert(t, r, peerBus, peerBus+".beta-1", peerBus+".gamma-1")
		if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 3}); err != nil {
			t.Fatalf("seeding version 3: %v", err)
		}
		before := rosterSnapshot(t, r, peerBus)

		err := r.ApplyRosterUpdate(RosterUpdate{
			BusID:   peerBus,
			Version: 4,
			Added:   []string{peerBus + ".delta-1", localBus + ".alpha-1"},
		})
		if !errors.Is(err, ErrBusIDCollision) {
			t.Fatalf("error = %v, want one wrapping ErrBusIDCollision", err)
		}
		if after := rosterSnapshot(t, r, peerBus); after != before {
			t.Fatalf("a partly-valid update was partly applied: %s -> %s; the GOOD id must not land when the update as a whole is refused", before, after)
		}
	})

	t.Run("a valid addition and a malformed removal", func(t *testing.T) {
		r := newTestRegistry(t)
		mustUpsert(t, r, peerBus, peerBus+".beta-1", peerBus+".gamma-1")
		before := rosterSnapshot(t, r, peerBus)

		err := r.ApplyRosterUpdate(RosterUpdate{
			BusID:   peerBus,
			Version: 1,
			Added:   []string{peerBus + ".delta-1"},
			Removed: []string{"not-an-agent-id"},
		})
		if !errors.Is(err, ErrInvalidRosterUpdate) {
			t.Fatalf("error = %v, want one wrapping ErrInvalidRosterUpdate", err)
		}
		if after := rosterSnapshot(t, r, peerBus); after != before {
			t.Fatalf("the Added list was applied even though Removed was refused: %s -> %s", before, after)
		}
		if r.Knows(peerBus + ".delta-1") {
			t.Error("the addition from a REJECTED update is in the routing table")
		}
	})

	t.Run("a removal that would have made room for two additions", func(t *testing.T) {
		r := newTestRegistry(t)
		full := make([]string, MaxRosterAgents)
		for i := range full {
			full[i] = fmt.Sprintf("%s.a%d-1", peerBus, i)
		}
		mustUpsert(t, r, peerBus, full...)
		before := rosterSnapshot(t, r, peerBus)

		// Net +1 at the cap: every id is legal, both lists are disjoint, and the
		// ONLY thing wrong is the resulting size. The removal is the trap.
		err := r.ApplyRosterUpdate(RosterUpdate{
			BusID:   peerBus,
			Version: 1,
			Added:   []string{peerBus + ".new1-1", peerBus + ".new2-1"},
			Removed: []string{full[0]},
		})
		if !errors.Is(err, ErrRosterTooLarge) {
			t.Fatalf("error = %v, want one wrapping ErrRosterTooLarge", err)
		}
		if after := rosterSnapshot(t, r, peerBus); after != before {
			t.Fatalf("an update refused at the SIZE cap still mutated the roster; removals must not run before the size is known")
		}
		if !r.Knows(full[0]) {
			t.Fatalf("%q was removed by an update that was refused; the peer's agent is now unroutable and nothing said so", full[0])
		}
		if r.Knows(peerBus + ".new1-1") {
			t.Error("an addition from a refused update landed anyway")
		}
	})
}

// TestRegistryBoundsAreLimitsNotOffByOnes walks each cap to its exact value and
// one past it. A cap tested only from the far side is a cap nobody has shown is
// not an off-by-one — and an off-by-one HERE refuses a peer, or a roster entry,
// that the operator was told is legal.
func TestRegistryBoundsAreLimitsNotOffByOnes(t *testing.T) {
	t.Run("exactly MaxPeers peers register, the next does not", func(t *testing.T) {
		// The DEFAULT cap, not an override: TestRegistryEnforcesMaxPeers proves
		// the mechanism with MaxPeers=2, and this proves the constant the product
		// actually ships with.
		r := newTestRegistry(t)
		for i := 0; i < MaxPeers; i++ {
			mustUpsert(t, r, fmt.Sprintf("bus-p%d", i))
		}
		if n := len(r.Peers()); n != MaxPeers {
			t.Fatalf("registry holds %d peers, want exactly %d", n, MaxPeers)
		}
		if err := r.UpsertPeer(PeerRoster{BusID: "bus-onemore"}); !errors.Is(err, ErrTooManyPeers) {
			t.Fatalf("error = %v, want one wrapping ErrTooManyPeers at MaxPeers+1", err)
		}
		if n := len(r.Peers()); n != MaxPeers {
			t.Fatalf("the refused peer changed the table: %d peers", n)
		}
	})

	t.Run("a roster reaches MaxRosterAgents by increments and stops there", func(t *testing.T) {
		// The handshake path is covered elsewhere; this is the INCREMENTAL path,
		// which is the one a peer controls one message at a time.
		r := newTestRegistry(t)
		nearly := make([]string, MaxRosterAgents-1)
		for i := range nearly {
			nearly[i] = fmt.Sprintf("%s.a%d-1", peerBus, i)
		}
		mustUpsert(t, r, peerBus, nearly...)

		if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 1, Added: []string{peerBus + ".last-1"}}); err != nil {
			t.Fatalf("the increment that lands EXACTLY on the cap was refused: %v", err)
		}
		agents, version, _ := r.Roster(peerBus)
		if len(agents) != MaxRosterAgents {
			t.Fatalf("roster holds %d agents, want exactly %d", len(agents), MaxRosterAgents)
		}
		if version != 1 {
			t.Fatalf("version = %d, want 1", version)
		}
		if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 2, Added: []string{peerBus + ".onemore-1"}}); !errors.Is(err, ErrRosterTooLarge) {
			t.Fatalf("error = %v, want one wrapping ErrRosterTooLarge at MaxRosterAgents+1", err)
		}
		if agents, _, _ := r.Roster(peerBus); len(agents) != MaxRosterAgents {
			t.Fatalf("the refused increment changed the roster to %d agents", len(agents))
		}
	})

	t.Run("an update of exactly MaxRosterUpdateEntries is accepted", func(t *testing.T) {
		// The count is Added PLUS Removed, together — not each — so the exact
		// boundary has to be walked with both lists populated or the test would
		// only ever exercise half the sum.
		const half = MaxRosterUpdateEntries / 2
		seed := make([]string, half)
		for i := range seed {
			seed[i] = fmt.Sprintf("%s.old%d-1", peerBus, i)
		}
		r := newTestRegistry(t)
		mustUpsert(t, r, peerBus, seed...)

		added := make([]string, half)
		for i := range added {
			added[i] = fmt.Sprintf("%s.new%d-1", peerBus, i)
		}
		if n := len(added) + len(seed); n != MaxRosterUpdateEntries {
			t.Fatalf("the test built a %d-entry update, want exactly %d", n, MaxRosterUpdateEntries)
		}
		if err := r.ApplyRosterUpdate(RosterUpdate{BusID: peerBus, Version: 1, Added: added, Removed: seed}); err != nil {
			t.Fatalf("an update of exactly MaxRosterUpdateEntries was refused: %v", err)
		}
		if agents, _, _ := r.Roster(peerBus); len(agents) != half {
			t.Fatalf("roster holds %d agents, want %d", len(agents), half)
		}

		if err := r.ApplyRosterUpdate(RosterUpdate{
			BusID: peerBus, Version: 2,
			Added:   append(append([]string(nil), added...), peerBus+".onemore-1"),
			Removed: seed,
		}); !errors.Is(err, ErrInvalidRosterUpdate) {
			t.Fatalf("error = %v, want one wrapping ErrInvalidRosterUpdate at MaxRosterUpdateEntries+1", err)
		}
	})
}

// TestRegistryIsSafeUnderConcurrentAccess drives every mutator and every reader
// at once under -race. The Registry doc claims "safe for concurrent use", and
// concurrency is the product here: a data race in the routing table is a P0.
//
// The two peer families are DISJOINT on purpose. Roster versions are per-peer
// and strictly monotonic, so one writer per peer is what the protocol itself
// implies (a peer pushes its own updates in order); letting two goroutines
// update ONE peer would make ErrStaleRosterUpdate the expected outcome and the
// final state unassertable. Splitting the families keeps the end state exactly
// predictable, so this test proves CORRECTNESS under concurrency and not merely
// the absence of a detector report.
func TestRegistryIsSafeUnderConcurrentAccess(t *testing.T) {
	const (
		families = 4
		updates  = 60
		upserts  = 20
		readers  = 6
		reads    = 200
	)

	r := newTestRegistry(t)

	synced := make([]string, families)  // driven by ApplyRosterUpdate
	created := make([]string, families) // driven by UpsertPeer
	knownTarget := make(map[string]bool, 2*families)
	for i := 0; i < families; i++ {
		synced[i] = fmt.Sprintf("bus-sync-%d", i)
		created[i] = fmt.Sprintf("bus-new-%d", i)
		mustUpsert(t, r, synced[i], synced[i]+".seed-1")
		knownTarget[synced[i]] = true
		knownTarget[created[i]] = true
	}

	var wg sync.WaitGroup
	problems := make(chan error, 64)

	for i := 0; i < families; i++ {
		bus := synced[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := 1; v <= updates; v++ {
				u := RosterUpdate{BusID: bus, Version: uint64(v), Added: []string{fmt.Sprintf("%s.a%d-1", bus, v)}}
				if v > 1 {
					u.Removed = []string{fmt.Sprintf("%s.a%d-1", bus, v-1)}
				}
				if err := r.ApplyRosterUpdate(u); err != nil {
					problems <- fmt.Errorf("ApplyRosterUpdate(%s, v%d): %v", bus, v, err)
					return
				}
			}
		}()
	}

	for i := 0; i < families; i++ {
		bus := created[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < upserts; n++ {
				// A RE-handshake is an ordinary event, so the same peer is
				// upserted repeatedly rather than once.
				if err := r.UpsertPeer(PeerRoster{BusID: bus, Agents: []string{bus + ".beta-1"}}); err != nil {
					problems <- fmt.Errorf("UpsertPeer(%s): %v", bus, err)
					return
				}
			}
		}()
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < reads; n++ {
				for _, bus := range synced {
					if got, ok := r.Route(bus + ".anyone-1"); ok && got != bus {
						problems <- fmt.Errorf("Route(%s.anyone-1) resolved to %q", bus, got)
						return
					}
					// The snapshot must never be one another goroutine is
					// mutating: Roster copies out under the lock.
					if agents, _, ok := r.Roster(bus); ok {
						for _, id := range agents {
							if !strings.HasPrefix(id, bus+".") {
								problems <- fmt.Errorf("Roster(%s) contained %q, which is not one of that peer's agents", bus, id)
								return
							}
						}
					}
					_ = r.Knows(bus + ".seed-1")
				}
				for _, target := range r.BroadcastTargets([]string{"bus-elsewhere"}) {
					if !knownTarget[target] {
						problems <- fmt.Errorf("BroadcastTargets returned %q, which is not a registered peer", target)
						return
					}
				}
				_ = r.Peers()
			}
		}()
	}

	wg.Wait()
	close(problems)
	for err := range problems {
		t.Fatal(err)
	}

	// The end state is exactly what a serial run would have produced.
	for _, bus := range synced {
		want := fmt.Sprintf("v%d:[%s.a%d-1 %s.seed-1]", updates, bus, updates, bus)
		if got := rosterSnapshot(t, r, bus); got != want {
			t.Errorf("peer %s ended at %s, want %s", bus, got, want)
		}
	}
	for _, bus := range created {
		if got, want := rosterSnapshot(t, r, bus), fmt.Sprintf("v0:[%s.beta-1]", bus); got != want {
			t.Errorf("peer %s ended at %s, want %s", bus, got, want)
		}
	}
	if n := len(r.Peers()); n != 2*families {
		t.Errorf("registry holds %d peers, want %d", n, 2*families)
	}
}

// TestRegistryBroadcastTargets pins the egress split horizon applied to fan-out.
func TestRegistryBroadcastTargets(t *testing.T) {
	r := newTestRegistry(t)
	mustUpsert(t, r, "bus-one")
	mustUpsert(t, r, "bus-two")
	mustUpsert(t, r, "bus-three")

	got := r.BroadcastTargets([]string{"bus-two"})
	want := []string{"bus-one", "bus-three"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("BroadcastTargets = %v, want %v", got, want)
	}
	// Case-insensitively, for the reason PathContains folds.
	if got := r.BroadcastTargets([]string{"BUS-TWO"}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("BroadcastTargets with a case variant = %v, want %v", got, want)
	}
	// Our own bus is never a target: it is not a peer.
	for _, target := range r.BroadcastTargets(nil) {
		if strings.EqualFold(target, localBus) {
			t.Fatalf("BroadcastTargets returned our own bus %q", target)
		}
	}
	if n := len(r.BroadcastTargets(nil)); n != 3 {
		t.Errorf("BroadcastTargets(nil) returned %d targets, want 3", n)
	}
}

func TestRegistryRemovePeerAndBaseURL(t *testing.T) {
	r := newTestRegistry(t)
	mustUpsert(t, r, peerBus, peerBus+".beta-1")

	if err := r.SetPeerBaseURL(peerBus, "http://peer.example"); err == nil {
		t.Error("SetPeerBaseURL accepted a plaintext origin; there is no plaintext listener (invariant 11)")
	}
	if err := r.SetPeerBaseURL(thirdBus, "https://peer.example"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("SetPeerBaseURL for an unknown peer gave %v, want ErrUnknownPeer", err)
	}
	if err := r.SetPeerBaseURL(peerBus, "https://peer.example:8443"); err != nil {
		t.Fatalf("SetPeerBaseURL: %v", err)
	}
	peers := r.Peers()
	if len(peers) != 1 || peers[0].BaseURL != "https://peer.example:8443" || peers[0].Agents != 1 {
		t.Fatalf("Peers() = %+v", peers)
	}

	// A re-handshake must not forget where the peer lives.
	mustUpsert(t, r, peerBus, peerBus+".beta-1", peerBus+".gamma-1")
	if peers := r.Peers(); peers[0].BaseURL != "https://peer.example:8443" {
		t.Errorf("a re-handshake forgot the peer's base URL: %+v", peers[0])
	}

	if !r.RemovePeer(peerBus) {
		t.Error("RemovePeer reported no peer removed")
	}
	if r.RemovePeer(peerBus) {
		t.Error("RemovePeer removed the same peer twice")
	}
	if _, ok := r.Route(peerBus + ".beta-1"); ok {
		t.Error("a removed peer still routes")
	}
}

func TestNewRegistryRejectsIncompleteOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts RegistryOptions
	}{
		{"no bus id", RegistryOptions{}},
		{"bus id with a dot", RegistryOptions{BusID: "bus.x"}},
		{"negative peer cap", RegistryOptions{BusID: localBus, MaxPeers: -1}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegistry(tc.opts); err == nil {
				t.Fatal("NewRegistry accepted an invalid config")
			}
		})
	}
}

// TestRegistryUpsertRevalidatesTheRoster proves the registry does not trust a
// PeerRoster to have come from ValidatePeerEnrollRequest: it is an ordinary
// struct any caller can build, and this is the boundary of the routing table.
func TestRegistryUpsertRevalidatesTheRoster(t *testing.T) {
	r := newTestRegistry(t)
	cases := []struct {
		name  string
		peer  PeerRoster
		want  error
		about string
	}{
		{"our own bus id", PeerRoster{BusID: localBus}, ErrBusIDCollision, "a peer may never assert our namespace"},
		{"malformed bus id", PeerRoster{BusID: "bus.x"}, ErrInvalidBusID, "'.' is the qualification separator"},
		{"agent in our namespace", PeerRoster{BusID: peerBus, Agents: []string{localBus + ".alpha-1"}}, ErrBusIDCollision, "a peer minting ids in our namespace could impersonate our agents"},
		{"agent of a third bus", PeerRoster{BusID: peerBus, Agents: []string{thirdBus + ".delta-1"}}, ErrInvalidRoster, "a peer speaks for its own agents only"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := r.UpsertPeer(tc.peer); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want one wrapping %v (%s)", err, tc.want, tc.about)
			}
			if n := len(r.Peers()); n != 0 {
				t.Errorf("a rejected peer was registered anyway (%d peers)", n)
			}
		})
	}
}
