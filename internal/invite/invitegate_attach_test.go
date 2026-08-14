package invite_test

// INVITE-GATE, part 5: Store.Attach and Store.Len.
//
// Attach exists for the chicken-and-egg the whole durability layer has:
// wal.Open needs the APPLIER before the *wal.Log exists, because replay runs
// INSIDE Open and hands every committed entry to the applier before Open
// returns. So the store must be constructible first and given its log
// afterwards, and the ordering is not optional:
//
//	store := invite.NewStore(...)               // 1. applier first
//	log   := wal.Open({Applier: store})         // 2. replay fills it
//	store.Attach(log)                           // 3. NOW it can write
//
// Between steps 1 and 3 the table can be READ and REBUILT but not WRITTEN. That
// is the correct order — recovery must finish before the first live mint or
// redemption — and it is what these tests pin, along with the two refusals that
// keep a mis-wiring loud: a nil log, and a SECOND log.
//
// Before Attach existed, cmd/agent-bus could only build the store with its log
// already in hand, which it cannot do; without the refusals, a store could end
// up with two durable histories behind one in-memory table and whichever won
// the race would silently own the redemptions the other had acknowledged.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// igNewDetachedStore builds a store with NO durable log, which is the state
// wal.Open needs it in.
func igNewDetachedStore(t *testing.T, clk *testClock) *invite.Store {
	t.Helper()
	o := invite.StoreOptions{BusID: testBusID}
	if clk != nil {
		o.Now = clk.Now
	}
	st, err := invite.NewStore(o)
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	return st
}

// TestInviteGateAttachRefusesNilAndSecondCalls pins both refusals.
func TestInviteGateAttachRefusesNilAndSecondCalls(t *testing.T) {
	t.Run("a nil log", func(t *testing.T) {
		st := igNewDetachedStore(t, nil)
		err := st.Attach(nil)
		if err == nil {
			t.Fatalf("Attach(nil) succeeded; a store with no log would acknowledge mints and redemptions that never reached disk, and single use held only in memory is decorative")
		}
		// The message names ErrNotDurable, which is the state a nil log would
		// leave the store in. It is rendered with %v rather than %w, so the
		// sentinel is deliberately NOT matchable with errors.Is here — asserted
		// on the TEXT to record that on purpose rather than by accident. Nothing
		// classifies this error: Attach is wiring, it is called once at startup,
		// and its only correct handling is to refuse to boot.
		if !strings.Contains(err.Error(), "must not be nil") {
			t.Errorf("Attach(nil) = %v, want a refusal naming the nil log", err)
		}
		// And the store is still UNATTACHED, not half-bound: a mutating call
		// must still fail closed.
		if _, err := st.Mint(invite.MintRequest{TTL: time.Hour}); !errors.Is(err, invite.ErrNotDurable) {
			t.Errorf("Mint after a refused Attach(nil) = %v, want ErrNotDurable", err)
		}
	})

	t.Run("a second log", func(t *testing.T) {
		st := igNewDetachedStore(t, nil)
		first := &fakeLog{}
		if err := st.Attach(first); err != nil {
			t.Fatalf("the first Attach: %v", err)
		}
		second := &fakeLog{}
		err := st.Attach(second)
		if err == nil {
			t.Fatalf(`a SECOND Attach succeeded.

Two logs mean two distinct durable histories behind one in-memory table, and
whichever won the race would silently own the redemptions the other had already
acknowledged.`)
		}
		if !strings.Contains(err.Error(), "already attached") {
			t.Errorf("the second Attach = %v, want an already-attached error", err)
		}

		// It CHANGED NOTHING: the first log is still the one writes go to.
		if _, err := st.Mint(invite.MintRequest{TTL: time.Hour}); err != nil {
			t.Fatalf("Mint after a refused second Attach: %v", err)
		}
		if first.len() != 1 {
			t.Errorf("the FIRST log received %d entries, want 1", first.len())
		}
		if second.len() != 0 {
			t.Errorf("the refused SECOND log received %d entries, want 0", second.len())
		}
	})
}

// TestInviteGateAttachedAfterReplayCanWrite is the wiring the server performs,
// end to end over a real log: the store is the log's APPLIER at Open (so replay
// rebuilds it) and becomes the log's WRITER only afterwards.
//
// The two halves are asserted separately because they fail differently: without
// replay-first the table is empty and every redemption fails closed against an
// invite the operator can see on disk; without attach-after the store cannot
// write at all.
func TestInviteGateAttachedAfterReplayCanWrite(t *testing.T) {
	dir := t.TempDir()

	// First boot: mint one invite, close cleanly.
	st1, lg1 := openStore(t, dir, nil)
	minted := mustMint(t, st1, invite.MintRequest{Label: "before the restart", TTL: time.Hour})
	if err := lg1.Close(); err != nil {
		t.Fatalf("closing the first log: %v", err)
	}

	// Second boot, done the server's way and in the server's order.
	st2 := igNewDetachedStore(t, nil)

	// BEFORE Attach: the table is READABLE and WRITES FAIL CLOSED.
	lg2, err := wal.Open(wal.LogOptions{Dir: dir, Applier: st2})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = lg2.Close() }()

	rec, ok := st2.Lookup(minted.ID)
	if !ok {
		t.Fatalf("the invite was not replayed into the store before wal.Open returned; recovery must finish before the first live operation")
	}
	if rec.State != invite.StateOpen {
		t.Fatalf("the replayed invite is %s, want open", rec.State)
	}
	if _, err := st2.Mint(invite.MintRequest{TTL: time.Hour}); !errors.Is(err, invite.ErrNotDurable) {
		t.Fatalf(`Mint BEFORE Attach = %v, want ErrNotDurable.

A store that accepted a write before its log existed would be claiming a
durability it does not have.`, err)
	}

	// AFTER Attach: it can write, and the write is durable.
	if err := st2.Attach(lg2); err != nil {
		t.Fatalf("Attach after wal.Open: %v", err)
	}
	fresh := mustMint(t, st2, invite.MintRequest{Label: "after the restart", TTL: time.Hour})
	if _, ok := st2.Lookup(fresh.ID); !ok {
		t.Fatalf("the invite minted after Attach is not in the table")
	}
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the second log: %v", err)
	}

	// Third boot proves the post-Attach mint really reached the log.
	st3, lg3 := openStore(t, dir, nil)
	defer func() { _ = lg3.Close() }()
	for _, id := range []string{minted.ID, fresh.ID} {
		if _, ok := st3.Lookup(id); !ok {
			t.Fatalf("invite %s did not survive to the third boot; the post-Attach write was not durable", id)
		}
	}
}

// TestInviteGateLenCountsTheRebuiltTable pins Store.Len, which is what the
// server's startup line reads to prove the table was rebuilt by replay rather
// than started empty.
//
// It is a RETENTION count and says so: a retired-but-retained record is refused
// exactly as hard as a dropped one, so Len is evidence about REPLAY, never about
// how many invites an operator can still spend.
func TestInviteGateLenCountsTheRebuiltTable(t *testing.T) {
	dir := t.TempDir()
	clk := newTestClock()

	st1, lg1 := openStore(t, dir, clk)
	if got := st1.Len(); got != 0 {
		t.Fatalf("a fresh store reports Len = %d, want 0", got)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustMint(t, st1, invite.MintRequest{TTL: time.Hour}).ID)
	}
	if got := st1.Len(); got != 3 {
		t.Fatalf("after three mints Len = %d, want 3", got)
	}
	if err := lg1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A restart on the SAME clock rebuilds all three.
	st2, lg2 := openStore(t, dir, clk)
	if got := st2.Len(); got != 3 {
		t.Fatalf(`after a restart Len = %d, want 3.

This is the number the startup line reports. A zero here is what "the invite
store was constructed but nothing was replayed into it" looks like, and it makes
every redemption fail closed against invites the operator can see on disk.`, got)
	}
	if err := lg2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Past every retention window, the sweep retires them and Len says so. This
	// is the RETENTION half of the contract, and it is why Len must not be read
	// as "spendable invites".
	clk.Advance(invite.SpentRetention + invite.MaxTTL + 48*time.Hour)
	st3, lg3 := openStore(t, dir, clk)
	defer func() { _ = lg3.Close() }()
	if got := st3.Len(); got != 0 {
		t.Fatalf("after the retention window Len = %d, want 0 (the records are retired by the sweep)", got)
	}
	for _, id := range ids {
		if _, ok := st3.Lookup(id); ok {
			t.Errorf("invite %s is still retained past its retention window", id)
		}
	}
}
