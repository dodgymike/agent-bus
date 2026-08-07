// These tests pin IDEM-11-FU-FAIRSHARE: the applied-key table (Store) is bounded
// BOTH bus-wide by MaxEntries AND per agent by a fair share of it. Once the
// table is under pressure (len(records) >= maxEntries/2, the free/used
// crossover), an agent already holding maxEntries/(agents+1) records is refused
// with ErrAgentQuota — and nothing is evicted to make room.
//
// The defect that rule closes: with a bus-wide bound ALONE, one agent could fill
// the whole table, after which every other agent's mutating operations were
// refused with ErrCapacity for up to the full RetentionWindow (50h10m22s) even
// though that other agent had never stored a single key of its own. That is a
// denial of service one misbehaving (or merely busy) agent could inflict on
// every peer on the bus, and it violated invariant 10 ("Duplicate detection and
// idempotency, everywhere"): idempotency exists so a well-behaved client can
// retry safely, not so one client's volume can revoke that safety from everyone
// else.
//
// See internal/hub/idem_quota_test.go for the same property proved end to end
// through the durable write path.
package idem_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// agentRecord builds a valid Record for an arbitrary agent+key pair, at a
// fixed instant, following the same construction mustRecord uses in
// store_test.go (CommittedAt non-zero, a real Operation, a key that satisfies
// ValidateKey).
func agentRecord(agent, key string, b byte, at time.Time) idem.Record {
	return idem.Record{
		Agent:       agent,
		Op:          idem.OpSend,
		Key:         key,
		Fingerprint: fp(b),
		Result:      json.RawMessage(`{"message_id":"bus-a-1"}`),
		Seq:         1,
		CommittedAt: at,
	}
}

// TestOneAgentCannotStarveAnother is the property invariant 10's fair-share
// requirement turns on, at the unit level: one agent (bus-a.hog) sends until it
// is refused, and a second agent (bus-a.victim) that has never stored a single
// key of its own is then admitted on its very first Remember.
//
// The hog's refusal must be ErrAgentQuota — its OWN fair share, reached while
// the table still has room — and not ErrCapacity-the-bus-wide-cap, which is what
// a bus-wide-only bound would have produced and which would have refused the
// victim too. One agent filling its share must never be able to deny another
// agent its own first applied key.
func TestOneAgentCannotStarveAnother(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	// Injected clock: nothing expires during the test, so a pass here can
	// only be explained by fair-share admission, not by the window rolling
	// forward and reclaiming room.
	s := idem.NewStore(idem.StoreOptions{MaxEntries: 16, Now: func() time.Time { return at }})

	const hog = "bus-a.hog"
	var hogWasRefused bool
	// A generous loop bound: if the hog is never refused, the bound is not
	// being enforced at all and the test must fail rather than pass
	// vacuously.
	const loopBound = 1000
	for i := 0; i < loopBound; i++ {
		key := fmt.Sprintf("hogkey-%d", i)
		rec := agentRecord(hog, key, byte(i), at)
		if err := s.Remember(rec); err != nil {
			// The SPECIFIC sentinel is the assertion that matters: the hog is at
			// its own fair share, not at the bus-wide cap. ErrCapacity is asserted
			// too because the refusal deliberately satisfies both (the class match
			// is what internal/httpapi maps to 503 + Retry-After), but a refusal
			// that were ONLY ErrCapacity would mean the table is full — and the
			// victim below could then never be admitted.
			if !errors.Is(err, idem.ErrAgentQuota) {
				t.Fatalf("hog Remember %d: err = %v, want ErrAgentQuota (its own fair share), not a bus-wide refusal", i, err)
			}
			if !errors.Is(err, idem.ErrCapacity) {
				t.Fatalf("hog Remember %d: err = %v does not also satisfy errors.Is(err, ErrCapacity); the class match is what the HTTP layer maps to 503 + Retry-After", i, err)
			}
			hogWasRefused = true
			break
		}
	}
	if !hogWasRefused {
		t.Fatalf("the hog was never refused after %d distinct keys against MaxEntries=16; "+
			"the capacity bound is not being enforced at all", loopBound)
	}

	// The victim has never stored a single key. Its very first Remember must
	// succeed: one agent filling the table must never be able to deny another
	// agent its own first applied key (invariant 10's fair-share requirement).
	// A bus-wide-only MaxEntries bound refused this, and that was the
	// IDEM-11-FU-FAIRSHARE defect.
	const victim = "bus-a.victim"
	victimRec := agentRecord(victim, "victimkey-1", 0xff, at)
	if err := s.Remember(victimRec); err != nil {
		t.Fatalf("victim (an agent that has never stored a key of its own) was refused its "+
			"first Remember with %v because the hog filled the bus-wide table; one agent "+
			"filling the table must never be able to deny another agent its own first applied "+
			"key (invariant 10, fair-share requirement)", err)
	}
}

// TestRecoverIgnoresTheFairShareThatRememberEnforces is the one-line proof that
// the LIVE path and the REPLAY path are genuinely different functions, and it
// exists to stop a later refactor collapsing them back together.
//
// Same store bound, same agent, same records, opposite outcomes: Remember
// adjudicates the per-agent fair share because it is deciding whether to ACCEPT
// an operation; Recover does not, because the record it is handed is proof that
// the decision was already made, durably, by the run that accepted it.
// Re-adjudicating it at replay can only make two runs of the same log disagree —
// and a record dropped by that disagreement turns its owner's next retry into a
// SECOND effect (see Store.Recover for the two concrete triggers: a backwards
// clock, and a log written before the fair share existed).
func TestRecoverIgnoresTheFairShareThatRememberEnforces(t *testing.T) {
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const (
		maxEntries = 16
		hog        = "bus-a.hog"
	)
	// A lone agent's share is maxEntries/(1+1) and the pressure line is
	// maxEntries/2 — the same number, 8 — so the 9th record is the first one the
	// live path refuses.
	const share = maxEntries / 2

	newStore := func() *idem.Store {
		return idem.NewStore(idem.StoreOptions{MaxEntries: maxEntries, Now: func() time.Time { return at }})
	}
	records := make([]idem.Record, maxEntries)
	for i := range records {
		records[i] = agentRecord(hog, fmt.Sprintf("hogkey-%d", i), byte(i), at)
	}

	// The LIVE path refuses at the share, with room still left in the table.
	live := newStore()
	for i, r := range records {
		err := live.Remember(r)
		if i < share {
			if err != nil {
				t.Fatalf("Remember %d: %v, want nil (below the share of %d)", i, err, share)
			}
			continue
		}
		if !errors.Is(err, idem.ErrAgentQuota) {
			t.Fatalf("Remember %d: err = %v, want ErrAgentQuota; a lone agent's share is %d and the live path must enforce it", i, err, share)
		}
	}
	if got := live.Stats().Count; got != share {
		t.Fatalf("after the live path, Count = %d, want %d", got, share)
	}

	// The REPLAY path admits every one of them, on an identical store.
	replay := newStore()
	for i, r := range records {
		if err := replay.Recover(r); err != nil {
			t.Fatalf("Recover %d: %v. Recover must NOT adjudicate the per-agent fair share: every record it is handed was already admitted, acknowledged and fsynced by the run that accepted it, so refusing one here does not make the bus stricter — it DROPS a key, and the next retry of that key is applied as a SECOND effect (invariant 10)", i, err)
		}
	}
	if got := replay.Stats().Count; got != len(records) {
		t.Fatalf("after the replay path, Count = %d, want all %d records; %d accepted key(s) were dropped by replay", got, len(records), len(records)-got)
	}

	// The GLOBAL cap still binds on the replay path — it is a MEMORY bound, not
	// an admission policy, and a bound that held on only one path is not a bound.
	over := agentRecord(hog, "hogkey-over-the-cap", 0xfe, at)
	err := replay.Recover(over)
	if !errors.Is(err, idem.ErrCapacity) {
		t.Fatalf("Recover past the bus-wide cap: err = %v, want ErrCapacity; the global cap must hold on BOTH paths", err)
	}
	if errors.Is(err, idem.ErrAgentQuota) {
		t.Fatalf("Recover past the bus-wide cap: err = %v also satisfies ErrAgentQuota, so Recover is adjudicating the per-agent share after all", err)
	}

	// And the counters the rebuilt table hands to the LIVE traffic that follows
	// the restart are correct: the replayed records are attributed to their
	// agent, not silently uncounted.
	st := replay.Stats()
	if st.Agents != 1 {
		t.Fatalf("after replay, Stats().Agents = %d, want 1: Recover must still count each record against its agent, or the fair share is blind to everything held before the restart", st.Agents)
	}
	if !st.UnderPressure {
		t.Fatalf("after replay, Stats() = %+v, want UnderPressure = true with %d of %d entries retained", st, st.Count, maxEntries)
	}
}
