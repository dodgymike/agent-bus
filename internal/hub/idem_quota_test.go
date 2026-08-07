// IDEM-11-FU-FAIRSHARE, proved END TO END through the real durable write path.
//
// internal/idem/quota_test.go already pins the rule at the unit level, by
// calling Store.Remember directly. That is necessary and it is not sufficient:
// the property the bus actually promises is about hub.Send and hub.Broadcast —
// "one authenticated agent must never be able to deny another agent the bus" —
// and between Store.Remember and Send sit the admission ORDER in publish, the
// sequence mint, the two-phase fsynced write, the error wrapping the HTTP layer
// maps to a status code, and WAL replay. A counter that increments proves none
// of that.
//
// So every test in this file drives the hub's exported surface only, against a
// real wal.Log in t.TempDir(), and asserts on Result.Seq / Result.MessageID and
// on what a recipient can actually read back.
//
// # Why the bound is 16 here
//
// hub.Options.MaxIdempotencyEntries exists precisely so this file can exist:
// filling the real 65536-entry table means 65536 fsynced writes, i.e. a test
// nobody runs, i.e. a security property nobody ever checks. Sixteen entries
// gives a pressure line of 8 and a solo share of 8, so the whole property is
// demonstrable in a couple of dozen durable writes.
package hub_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// quotaMaxEntries is the applied-key bound these tests run the hub with.
	quotaMaxEntries = 16

	// quotaPressureLine is maxEntries/2 — the free/used crossover at which the
	// fair share starts being enforced (internal/idem/retention.go). It is ALSO
	// the solo share, maxEntries/(1+1), which is why a lone hog is refused at
	// exactly this many accepted keys: the two numbers coincide for one agent,
	// and that coincidence is the derivation, not an accident.
	quotaPressureLine = quotaMaxEntries / 2

	// quotaLoopBound is a generous ceiling on the "send until refused" loops. A
	// loop that runs to this bound without a refusal is a FAILURE, never a pass:
	// it means the fair share is not being enforced through the write path and
	// every assertion after it would be vacuous.
	quotaLoopBound = 1000
)

// openQuotaHub is newHubOver with an explicit applied-key bound.
//
// It is a SIBLING of newHubOver rather than a change to its signature: every
// other test in this package wants the default bound, and threading an extra
// argument through all of them to serve this file would be the wrong trade.
// Replay is wired exactly as newHubOver wires it — a closure over wal.Replay on
// the log's own path, which is a read-only pass — so a hub built here recovers
// through the same code main does.
func openQuotaHub(t *testing.T, lg *wal.Log, maxEntries int, agents ...string) *hub.Hub {
	t.Helper()
	return openQuotaHubClock(t, lg, maxEntries, nil, agents...)
}

// openQuotaHubClock is openQuotaHub with an injected clock, which
// TestReplayNeverRefusesWhatTheLivePathAccepted needs: the applied-key table
// expires from that clock, so driving it is the only way to make one run of the
// bus retain a DIFFERENT set of records from another run of the same log. A nil
// now means the real clock, which is what every other test here wants.
func openQuotaHubClock(t *testing.T, lg *wal.Log, maxEntries int, now func() time.Time, agents ...string) *hub.Hub {
	t.Helper()
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID:                 testBusID,
		DataDir:               filepath.Dir(path),
		Durable:               lg,
		Replay:                func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex:             lg.Recovered().NextIndex,
		MaxIdempotencyEntries: maxEntries,
		Roster:                roster,
		Now:                   now,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, agents...)
	return h
}

// publishShape lets one test body cover BOTH mutating message routes. Send and
// Broadcast funnel into the same publish, so the fair share must behave
// identically on both; running the table is how that stays true rather than
// being asserted about one of them and assumed about the other.
type publishShape struct {
	name      string
	broadcast bool
}

var publishShapes = []publishShape{{name: "send", broadcast: false}, {name: "broadcast", broadcast: true}}

// publishAs issues one message of the given shape, THROUGH THE MINT. `to` is
// ignored for a broadcast, which addresses the whole bus.
//
// It takes *testing.T only because the two-step publish helpers do (see
// mintedSend in hub_test.go); nothing here fails the test on its own.
func publishAs(t *testing.T, h *hub.Hub, sh publishShape, sender, to, key string, body []byte) (hub.Result, error) {
	t.Helper()
	if sh.broadcast {
		return mintedBroadcast(t, h, hub.BroadcastRequest{Sender: sender, Body: body, IdempotencyKey: key})
	}
	return mintedSend(t, h, hub.SendRequest{Sender: sender, To: to, Body: body, IdempotencyKey: key})
}

// hogKey and hogBody are deterministic so a later restart can retry the EXACT
// original request — same key AND same payload, which is what makes it a
// legitimate retry rather than a key-reuse violation.
func hogKey(shape string, i int) string  { return fmt.Sprintf("hog-%s-%d", shape, i) }
func hogBody(shape string, i int) []byte { return []byte(fmt.Sprintf("hog %s message %d", shape, i)) }

// fillUntilRefused sends distinct messages (distinct keys, distinct bodies) as
// `sender` until one is refused, and returns everything the caller needs to
// assert on: the accepted results in order, and the refusal.
//
// A nil refusal means the loop ran out — the caller MUST treat that as a
// failure.
func fillUntilRefused(t *testing.T, h *hub.Hub, sh publishShape, sender, to string) ([]hub.Result, error) {
	t.Helper()
	var accepted []hub.Result
	for i := 0; i < quotaLoopBound; i++ {
		res, err := publishAs(t, h, sh, sender, to, hogKey(sh.name, i), hogBody(sh.name, i))
		if err != nil {
			return accepted, err
		}
		accepted = append(accepted, res)
	}
	return accepted, nil
}

// TestOneAgentCannotStarveAnotherThroughSend is THE test this task exists for.
//
// One authenticated agent floods the applied-key table through the real
// two-phase write path until it is refused. Then a SECOND agent — which has
// never sent anything, and therefore holds not one applied key of its own —
// sends, and must succeed. That second send is the security property: a
// bus-wide-only bound (what shipped before IDEM-11-FU-FAIRSHARE) refuses it,
// and refuses it for up to the full 50h retention window.
func TestOneAgentCannotStarveAnotherThroughSend(t *testing.T) {
	for _, sh := range publishShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			dir := t.TempDir()
			lg := openTestLog(t, dir, true)
			h := openQuotaHub(t, lg, quotaMaxEntries, "hog", "victim", "sink")
			hog := agentID(t, testBusID, "hog")
			victim := agentID(t, testBusID, "victim")
			sink := agentID(t, testBusID, "sink")

			// Pin the bound actually in force. If Options.MaxIdempotencyEntries
			// were ever silently ignored, every loop below would run against
			// 65536 entries and the test would fail late and confusingly.
			if got := h.IdempotencyStats().MaxEntries; got != quotaMaxEntries {
				t.Fatalf("IdempotencyStats().MaxEntries = %d, want %d: hub.Options.MaxIdempotencyEntries is not reaching the applied-key table, so this test cannot demonstrate the fair share at all", got, quotaMaxEntries)
			}

			accepted, refusal := fillUntilRefused(t, h, sh, hog, sink)

			// (a) IT MUST ACTUALLY BE REFUSED. A loop that is never refused is a
			// vacuous test, not a passing one.
			if refusal == nil {
				t.Fatalf("the hog was never refused after %d distinct %ss against an applied-key bound of %d; the per-agent fair share is not enforced through the durable write path at all, so everything below would prove nothing", quotaLoopBound, sh.name, quotaMaxEntries)
			}
			if len(accepted) != quotaPressureLine {
				t.Fatalf("the hog was accepted %d times before being refused, want %d: a lone agent's share is maxEntries/(1+1) = %d and the pressure line is maxEntries/2 = %d, so it must be refused at exactly that fill (internal/idem/retention.go)", len(accepted), quotaPressureLine, quotaPressureLine, quotaPressureLine)
			}

			// (b) The refusal must satisfy BOTH sentinels.
			//
			// ErrAgentQuota is the specific fact: this ONE agent is at its share,
			// the table is not full, nobody else is affected.
			//
			// ErrCapacity is the class, and that match is load-bearing OUTSIDE
			// this package: internal/httpapi/messages.go maps ErrCapacity to 503
			// with a Retry-After. A refusal that did not satisfy it would fall
			// through that switch and the route would silently degrade from a
			// considered "retry later" to a generic 500 for an entirely routine
			// condition. Asserting it here is what keeps that mapping intact.
			if !errors.Is(refusal, hub.ErrAgentQuota) {
				t.Fatalf("the hog's refusal is %v, which does not satisfy errors.Is(err, hub.ErrAgentQuota); an operator reading it cannot tell that ONE named client, not the bus, is at a limit", refusal)
			}
			if !errors.Is(refusal, hub.ErrCapacity) {
				t.Fatalf("the hog's refusal is %v, which does not satisfy errors.Is(err, hub.ErrCapacity); internal/httpapi/messages.go maps ErrCapacity to 503 + Retry-After, so this refusal would fall through to a generic 500", refusal)
			}

			// (c) The text must name the offending agent, so an operator can tell
			// abuse from a busy client without correlating logs.
			if !strings.Contains(refusal.Error(), hog) {
				t.Fatalf("the hog's refusal does not name the offending agent %q, so an operator cannot tell which client to look at: %v", hog, refusal)
			}

			// (d) THE SECURITY PROPERTY. A second agent that has sent NOTHING
			// must still be served.
			victimKey := "victim-" + sh.name + "-first"
			victimBody := []byte("the victim's very first message")
			got, err := publishAs(t, h, sh, victim, sink, victimKey, victimBody)
			if err != nil {
				t.Fatalf("the victim — an agent that has never sent a single message and holds not one applied key — was refused its FIRST %s with %v, because the hog filled the table. ONE AGENT MUST NEVER BE ABLE TO DENY ANOTHER THE BUS: that is the whole of IDEM-11-FU-FAIRSHARE and the reason invariant 10 exists (idempotency is there so a well-behaved client can retry safely, not so one client's volume can revoke that safety from everybody else)", sh.name, err)
			}

			// "No error" is not the same as "it landed". Prove the message is
			// server-authoritative and real.
			if got.MessageID == "" || got.Seq == 0 {
				t.Fatalf("the victim's %s returned %+v: an accepted message must carry a server-minted MessageID and a non-zero Seq (invariant 1)", sh.name, got)
			}
			if got.Replayed {
				t.Fatalf("the victim's %s returned Replayed = true (%+v); it is a brand-new key and must be a fresh send, not a replay of somebody's stored result", sh.name, got)
			}
			// And it is visible exactly where a normally-accepted message is: to
			// the recipient, off the serving copy.
			seen := historyIDs(t, h, sink)
			if !contains(seen, got.MessageID) {
				t.Fatalf("the victim's accepted %s %s is not in the recipient's history %v; the call returned success but the message did not land", sh.name, got.MessageID, seen)
			}

			// The bus is NOT full — that is the point of a per-agent share. The
			// victim's key was admitted on top of the hog's, so the table now
			// holds one more than the hog alone could reach.
			st := h.IdempotencyStats()
			if st.Count != quotaPressureLine+1 {
				t.Fatalf("IdempotencyStats().Count = %d after the victim was served, want %d (the hog's %d keys plus the victim's one)", st.Count, quotaPressureLine+1, quotaPressureLine)
			}
			if st.Count >= st.MaxEntries {
				t.Fatalf("IdempotencyStats() = %+v: the hog's refusal must be a per-agent share, not the bus-wide cap — the table is not supposed to be full", st)
			}
			if st.Agents != 2 {
				t.Fatalf("IdempotencyStats().Agents = %d, want 2 (the hog and the victim)", st.Agents)
			}
		})
	}
}

// TestFairShareIsNotEnforcedBelowThePressureLine is the other half of the
// guarantee, and the one a later "simplification" is most likely to break: a
// bus that never approaches its cap must see NO behaviour change at all.
//
// Below maxEntries/2 the table's FREE space still exceeds its USED space, so
// whatever one agent has consumed is by construction still available to
// everyone else and there is nothing to protect. A single agent must therefore
// be able to send right up to the pressure line without ever meeting a quota
// error — and must meet one immediately at it, which is what stops this test
// passing on a hub that simply never enforces anything.
func TestFairShareIsNotEnforcedBelowThePressureLine(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	h := openQuotaHub(t, lg, quotaMaxEntries, "solo", "sink")
	solo := agentID(t, testBusID, "solo")
	sink := agentID(t, testBusID, "sink")

	for i := 0; i < quotaPressureLine; i++ {
		st := h.IdempotencyStats()
		// Every send in this loop starts from a table BELOW the line.
		if st.UnderPressure {
			t.Fatalf("before send %d the table already reports UnderPressure with %d of %d entries retained; the line is maxEntries/2 = %d", i, st.Count, st.MaxEntries, quotaPressureLine)
		}
		res, err := mintedSend(t, h, hub.SendRequest{
			Sender:         solo,
			To:             sink,
			Body:           hogBody("solo", i),
			IdempotencyKey: hogKey("solo", i),
		})
		if err != nil {
			t.Fatalf("send %d was refused with %v, but the table held only %d of %d applied keys — below the pressure line of %d the fair share must stay ENTIRELY out of the way, or a bus that never approaches its cap silently changes behaviour", i, err, st.Count, st.MaxEntries, quotaPressureLine)
		}
		if res.Seq == 0 {
			t.Fatalf("send %d returned Seq 0: %+v", i, res)
		}
	}

	st := h.IdempotencyStats()
	if st.Count != quotaPressureLine || !st.UnderPressure {
		t.Fatalf("after %d accepted sends IdempotencyStats() = %+v, want Count = %d and UnderPressure = true", quotaPressureLine, st, quotaPressureLine)
	}
	if st.Share != quotaPressureLine {
		t.Fatalf("IdempotencyStats().Share = %d, want %d: a lone agent's share is maxEntries/(agents+1) = %d/2", st.Share, quotaPressureLine, quotaMaxEntries)
	}

	// The boundary, asserted from the other side. Without this the test above
	// would also pass against a hub that enforces NOTHING, which would make the
	// "no behaviour change below the line" claim unfalsifiable.
	_, err := mintedSend(t, h, hub.SendRequest{
		Sender:         solo,
		To:             sink,
		Body:           hogBody("solo", quotaPressureLine),
		IdempotencyKey: hogKey("solo", quotaPressureLine),
	})
	if !errors.Is(err, hub.ErrAgentQuota) {
		t.Fatalf("the first send AT the pressure line gave err = %v, want hub.ErrAgentQuota; if the line never engages, the preceding loop proves nothing", err)
	}
}

// TestFairShareRefusalBurnsNoSequenceBeyondItsReservation: a refused send must
// consume NOTHING server-authoritative of its own.
//
// # What this test used to assert, and why it could not stay that way
//
// It was TestFairShareRefusalMintsNoSequence, and it asserted the stronger
// claim that a refused send burns NO sequence at all — publish called
// h.idem.Admit before h.seq.Next(), so the admission decision was made while a
// refusal was still free.
//
// SIGN-1 retired that ordering, deliberately and with its eyes open. The SENDER
// now signs the origin bus's minted id, so the number must leave the bus BEFORE
// the message arrives, which puts the allocation in Hub.Mint — one step and one
// network round trip earlier than any admission decision could possibly be
// made. A refused send therefore leaves behind the ONE number its own mint
// reserved. internal/ids/sequence.go already documents that outcome as CORRECT
// rather than as damage: consumers must treat the sequence as strictly
// increasing, NEVER as dense.
//
// # What is still worth pinning, and is
//
// Exactly one number per reservation, and not one more. A refusal must not
// allocate on top of the client's mint, and it must not allocate again on a
// retry of the same reservation — either would turn every refused send into a
// widening hole in the bus's total order, and the fair share is a condition a
// busy client meets ROUTINELY. So the arithmetic below counts reservations
// rather than accepted messages, and it is still falsified by a publish path
// that reaches for h.seq.Next() on the refusal path.
func TestFairShareRefusalBurnsNoSequenceBeyondItsReservation(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	h := openQuotaHub(t, lg, quotaMaxEntries, "hog", "victim", "sink")
	hog := agentID(t, testBusID, "hog")
	victim := agentID(t, testBusID, "victim")
	sink := agentID(t, testBusID, "sink")

	accepted, refusal := fillUntilRefused(t, h, publishShapes[0], hog, sink)
	if refusal == nil {
		t.Fatalf("the hog was never refused after %d distinct sends; nothing about sequence burning can be proved from a loop that was always admitted", quotaLoopBound)
	}
	if len(accepted) == 0 {
		t.Fatal("the hog was refused on its very first send; there is no accepted sequence to compare against")
	}
	last := accepted[len(accepted)-1]

	// The accepted prefix must itself be gapless, so "one past the last" below
	// is measured against a total order that never had a hole in it.
	for i, res := range accepted {
		if want := accepted[0].Seq + uint64(i); res.Seq != want {
			t.Fatalf("accepted send %d took Seq %d, want %d: the accepted prefix must be contiguous", i, res.Seq, want)
		}
	}

	// Every refused attempt so far, INCLUDING the one that ended the fill loop.
	// Each one reserved a number before it was refused, and each is entitled to
	// exactly that one.
	reservations := 1

	// Burn some more refusals, each under a FRESH reservation. If a refusal
	// allocated on top of its mint, these would open a hole wider than one slot
	// apiece and the arithmetic below catches it.
	const extraRefusals = 3
	for i := 0; i < extraRefusals; i++ {
		_, err := mintedSend(t, h, hub.SendRequest{
			Sender:         hog,
			To:             sink,
			Body:           hogBody("burn", i),
			IdempotencyKey: hogKey("burn", i),
		})
		if !errors.Is(err, hub.ErrAgentQuota) {
			t.Fatalf("extra refusal %d gave err = %v, want hub.ErrAgentQuota", i, err)
		}
		reservations++
	}

	// And retry ONE of those refusals under its EXISTING reservation, without
	// minting again. A re-mint under the same (agent, op, key) returns the SAME
	// assignment and allocates nothing (invariant 10), so this must not move the
	// sequence at all — it is the case that would go unnoticed if the helper
	// above were the only way a send were ever issued.
	retryMint := mintFor(t, h, hog, "send", hogKey("burn", 0))
	if _, err := h.Send(hub.SendRequest{
		Sender:         hog,
		To:             sink,
		Body:           hogBody("burn", 0),
		IdempotencyKey: hogKey("burn", 0),
		SignedMint:     retryMint,
	}); !errors.Is(err, hub.ErrAgentQuota) {
		t.Fatalf("the retry of a refused send under its ORIGINAL reservation gave err = %v, want hub.ErrAgentQuota", err)
	}

	// The next ACCEPTED message must sit exactly one past the numbers those
	// reservations account for — no wider.
	next, err := mintedSend(t, h, hub.SendRequest{
		Sender:         victim,
		To:             sink,
		Body:           []byte("the next accepted message"),
		IdempotencyKey: "victim-after-refusals",
	})
	if err != nil {
		t.Fatalf("the victim's send after the hog's refusals: %v", err)
	}
	if want := last.Seq + uint64(reservations) + 1; next.Seq != want {
		t.Fatalf("the next accepted message took Seq %d, want %d: the last accepted message took %d and %d refused send(s) each hold ONE reservation of their own, so the hole must be exactly %d wide. A wider one means the refusal path allocated a sequence on top of the client's reservation, and every refused send would then widen the bus's total order (invariant 1 forbids ever reusing the numbers it skips)",
			next.Seq, want, last.Seq, reservations, reservations)
	}
	if _, head := shapeOf(h).count, shapeOf(h).head; head != next.Seq {
		t.Fatalf("the serving copy's head sequence is %d but the last accepted message took %d", head, next.Seq)
	}
}

// TestFairShareSurvivesRestart is the restart-consistency requirement the task
// carries, and it is the one place the fair share could quietly cost the bus
// something.
//
// The rule must never be STRICTER on replay than it was live. If it were, two
// runs of the same log would DISAGREE about what was accepted: the rebuilt
// table would silently drop keys the pre-restart bus held, and a retry of one of
// them would be applied as a SECOND message — the double-apply invariant 10
// exists to prevent, delivered by the very mechanism added to protect fairness.
func TestFairShareSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	// closeOnCleanup = false: this test owns Close, because it closes the log
	// itself to simulate the restart.
	lg := openTestLog(t, dir, false)
	h := openQuotaHub(t, lg, quotaMaxEntries, "hog", "victim", "sink")
	hog := agentID(t, testBusID, "hog")
	victim := agentID(t, testBusID, "victim")
	sink := agentID(t, testBusID, "sink")

	accepted, refusal := fillUntilRefused(t, h, publishShapes[0], hog, sink)
	if refusal == nil {
		t.Fatalf("the hog was never refused after %d distinct sends; the pre-restart table was never driven under pressure, so the restart proves nothing", quotaLoopBound)
	}
	if !errors.Is(refusal, hub.ErrAgentQuota) {
		t.Fatalf("the hog's refusal is %v, want hub.ErrAgentQuota", refusal)
	}
	// The victim sends too, so the replayed table has TWO distinct agents in it
	// and the share on the replay path is divided the same way it was live.
	victimFirst, err := mintedSend(t, h, hub.SendRequest{
		Sender:         victim,
		To:             sink,
		Body:           []byte("victim before the restart"),
		IdempotencyKey: "victim-pre-restart",
	})
	if err != nil {
		t.Fatalf("the victim's pre-restart send: %v", err)
	}

	before := h.IdempotencyStats()
	if before.Count != len(accepted)+1 {
		t.Fatalf("before the restart IdempotencyStats().Count = %d, want %d", before.Count, len(accepted)+1)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- RESTART: a fresh log and a fresh hub over the SAME directory, with the
	// SAME bound. The table is rebuilt purely by WAL replay. ---
	lg2 := openTestLog(t, dir, true)
	h2 := openQuotaHub(t, lg2, quotaMaxEntries, "hog", "victim", "sink")
	after := h2.IdempotencyStats()

	// (a) Replay ACCEPTED every record the live path accepted. Not one dropped.
	if after.Count != before.Count {
		t.Fatalf("after the restart IdempotencyStats().Count = %d but the live table held %d: replay REFUSED a record the live path accepted, so two runs of the same log disagree about what was accepted and a retry of a dropped key will be applied as a SECOND message. Replay must not adjudicate the per-agent share AT ALL — hub.Apply inserts through idem.Store.Recover, which skips it (internal/idem/store.go, Recover)", after.Count, before.Count)
	}
	if after.Agents != before.Agents {
		t.Fatalf("after the restart IdempotencyStats().Agents = %d, want %d", after.Agents, before.Agents)
	}
	if after.MaxEntries != quotaMaxEntries {
		t.Fatalf("after the restart IdempotencyStats().MaxEntries = %d, want %d", after.MaxEntries, quotaMaxEntries)
	}

	// (b) The quota did not cost the bus its duplicate suppression: a retry of
	// one of the hog's ORIGINAL keys still replays the ORIGINAL result. Note it
	// is answered from the applied-key table BEFORE admission is consulted, so
	// an agent at its share can still retry safely — which is the whole point of
	// invariant 10 and would be defeated if the share refused retries too.
	for _, i := range []int{0, len(accepted) - 1} {
		want := accepted[i]
		again, err := mintedSend(t, h2, hub.SendRequest{
			Sender:         hog,
			To:             sink,
			Body:           hogBody(publishShapes[0].name, i),
			IdempotencyKey: hogKey(publishShapes[0].name, i),
		})
		if err != nil {
			t.Fatalf("retrying the hog's original key %q after the restart gave err = %v; an agent at its fair share must still be able to RETRY, or the share has turned a lost acknowledgement into a lost message", hogKey(publishShapes[0].name, i), err)
		}
		if !again.Replayed || again.MessageID != want.MessageID || again.Seq != want.Seq {
			t.Fatalf("retrying the hog's original key %q after the restart returned %+v, want the original %+v with Replayed set", hogKey(publishShapes[0].name, i), again, want)
		}
	}
	// The victim's pre-restart key replays too — the table is recovered state
	// for every agent in it, not just the one that filled it.
	victimAgain, err := mintedSend(t, h2, hub.SendRequest{
		Sender:         victim,
		To:             sink,
		Body:           []byte("victim before the restart"),
		IdempotencyKey: "victim-pre-restart",
	})
	if err != nil {
		t.Fatalf("retrying the victim's pre-restart key: %v", err)
	}
	if !victimAgain.Replayed || victimAgain.MessageID != victimFirst.MessageID {
		t.Fatalf("retrying the victim's pre-restart key returned %+v, want the original %+v with Replayed set", victimAgain, victimFirst)
	}

	// (c) And the victim can still be SERVED after the restart, with a new key.
	fresh, err := mintedSend(t, h2, hub.SendRequest{
		Sender:         victim,
		To:             sink,
		Body:           []byte("victim after the restart"),
		IdempotencyKey: "victim-post-restart",
	})
	if err != nil {
		t.Fatalf("the victim was refused a NEW send after the restart with %v; the hog's pre-restart flood must not be able to deny another agent the bus across a restart either", err)
	}
	if fresh.MessageID == "" || fresh.Seq == 0 || fresh.Replayed {
		t.Fatalf("the victim's post-restart send returned %+v, want a fresh server-minted message", fresh)
	}
	if seen := historyIDs(t, h2, sink); !contains(seen, fresh.MessageID) {
		t.Fatalf("the victim's post-restart message %s is not in the recipient's history %v", fresh.MessageID, seen)
	}
}

// TestReplayNeverRefusesWhatTheLivePathAccepted is the regression test for the
// rule that the fair share is a LIVE ADMISSION policy and must never be
// adjudicated on the REPLAY path (internal/idem/store.go, Store.Recover).
//
// TestFairShareSurvivesRestart above restarts with the SAME bound and the SAME
// clock, and that case is safe for a boring reason: replay re-runs the identical
// records in the identical order against the identical numbers, so it reaches
// the identical decisions. It therefore CANNOT detect a replay path that
// adjudicates — which is exactly why this test exists, and why both cases below
// deliberately break one of those "identicals".
//
// What is at stake if replay refuses a record the live path accepted: the
// rebuilt table silently DROPS an applied key the pre-restart bus held, so the
// client's next retry of that key finds nothing to replay and is applied as a
// SECOND message — the double-apply invariant 10 exists to prevent, delivered by
// the very mechanism added to protect fairness. The refusal is not a stricter
// bus; there is nobody left to refuse, the operation is already in the history.
func TestReplayNeverRefusesWhatTheLivePathAccepted(t *testing.T) {
	// CASE 1 — THE UPGRADE, and it needs no clock anomaly at all.
	//
	// A log written BEFORE the fair share existed can legitimately contain ONE
	// agent holding far more than maxEntries/(agents+1) applied keys inside the
	// retention window: the old bus-wide cap allowed up to the full MaxEntries.
	// Replaying such a log through the share would refuse everything past it,
	// exactly once, at the upgrade, where it is hardest to notice.
	//
	// A pre-fair-share log cannot be produced by today's write path, so this
	// reproduces the SAME SHAPE the honest way — a bound reconfigured downward
	// between runs, which is also a real operational move: the hog is admitted
	// liveMax/2 keys under the large bound, then replayed against a smaller one
	// whose share is narrower than the set the log already contains.
	t.Run("a log holding more keys than the bound in force would now admit", func(t *testing.T) {
		const (
			liveMax   = 64
			replayMax = 48
		)
		// The three inequalities that make this case NON-VACUOUS, asserted rather
		// than assumed. Without them the test could pass because nothing was ever
		// stricter, or fail for the entirely legitimate reason that the bus-wide
		// memory bound was exceeded.
		wantAccepted := liveMax / 2
		if replayMax/2 >= wantAccepted {
			t.Fatalf("replayMax/2 = %d is not below the %d keys the live path accepts, so replay's share is not stricter and this test would prove nothing", replayMax/2, wantAccepted)
		}
		if wantAccepted >= replayMax {
			t.Fatalf("the %d live keys reach the replay bound's BUS-WIDE cap of %d, so a drop could be the memory bound rather than the share", wantAccepted, replayMax)
		}

		dir := t.TempDir()
		// closeOnCleanup = false: this test closes the log itself to restart.
		lg := openTestLog(t, dir, false)
		h := openQuotaHub(t, lg, liveMax, "hog", "sink")
		hog := agentID(t, testBusID, "hog")
		sink := agentID(t, testBusID, "sink")

		accepted, refusal := fillUntilRefused(t, h, publishShapes[0], hog, sink)
		if refusal == nil {
			t.Fatalf("the hog was never refused after %d distinct sends against a bound of %d; the live table was never driven under pressure, so the restart proves nothing", quotaLoopBound, liveMax)
		}
		if len(accepted) != wantAccepted {
			t.Fatalf("the hog was accepted %d times, want %d (a lone agent's share of %d)", len(accepted), wantAccepted, liveMax)
		}
		if err := lg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		originals := make([]acceptedSend, len(accepted))
		for i, res := range accepted {
			originals[i] = acceptedSend{
				key:  hogKey(publishShapes[0].name, i),
				body: hogBody(publishShapes[0].name, i),
				res:  res,
			}
		}

		// --- RESTART over the SAME directory, under the NARROWER bound. ---
		lg2 := openTestLog(t, dir, true)
		h2 := openQuotaHubClock(t, lg2, replayMax, nil, "hog", "sink")
		assertReplayKeptEverything(t, h2, hog, sink, originals)
	})

	// CASE 2 — A BACKWARDS CLOCK, same bound, same records.
	//
	// expireLocked deliberately TOLERATES a clock that steps backwards, because
	// retaining records LONGER than the window is the safe direction for expiry
	// (a duplicate suppressed too long is correct behaviour). It is the UNSAFE
	// direction for admission: a larger retained set raises the count, raises the
	// distinct-agent count and raises the agent's own holding, and all three move
	// toward REFUSAL. So the replayed set can be a SUPERSET of what was ever
	// retained live at one instant, and a replay that adjudicates would refuse
	// records this very bus accepted and acknowledged.
	//
	// Driven here by letting the live bus roll PAST the retention window between
	// two batches — legitimately freeing the hog's share — and then replaying at
	// an earlier instant, at which both batches are still inside their windows.
	t.Run("a backwards clock makes replay retain a superset", func(t *testing.T) {
		const share = quotaMaxEntries / 2
		base := time.Now().UTC()
		clock := base
		nowFn := func() time.Time { return clock }

		dir := t.TempDir()
		lg := openTestLog(t, dir, false)
		h := openQuotaHubClock(t, lg, quotaMaxEntries, nowFn, "hog", "sink")
		hog := agentID(t, testBusID, "hog")
		sink := agentID(t, testBusID, "sink")

		var accepted []acceptedSend
		send := func(key string, body []byte) {
			t.Helper()
			res, err := mintedSend(t, h, hub.SendRequest{Sender: hog, To: sink, Body: body, IdempotencyKey: key})
			if err != nil {
				t.Fatalf("send %q: %v", key, err)
			}
			accepted = append(accepted, acceptedSend{key: key, body: body, res: res})
		}
		// Batch one fills the hog's whole share at `base`.
		for i := 0; i < share; i++ {
			send(fmt.Sprintf("early-%d", i), []byte(fmt.Sprintf("early %d", i)))
		}
		if _, err := mintedSend(t, h, hub.SendRequest{Sender: hog, To: sink, Body: []byte("over"), IdempotencyKey: "early-over"}); !errors.Is(err, hub.ErrAgentQuota) {
			t.Fatalf("the hog's send past its share gave err = %v, want hub.ErrAgentQuota; batch one did not fill the share, so batch two proves nothing", err)
		}

		// The clock rolls past the retention window: batch one expires, the hog's
		// bucket empties, and batch two is admitted entirely legitimately.
		clock = base.Add(idem.RetentionWindow + time.Minute)
		for i := 0; i < share; i++ {
			send(fmt.Sprintf("late-%d", i), []byte(fmt.Sprintf("late %d", i)))
		}
		if st := h.IdempotencyStats(); st.Count != share {
			t.Fatalf("the live table holds %d applied keys, want %d: batch one should have expired before batch two was sent", st.Count, share)
		}
		if err := lg.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// --- RESTART with the clock stepped BACK to half a window after `base`,
		// at which BOTH batches are inside their retention windows (batch two's
		// records are, from this clock's point of view, in the future — and a
		// negative age is emphatically not "expired"). The replayed set is
		// therefore a superset of anything the live bus ever held at once. ---
		clock = base.Add(idem.RetentionWindow / 2)
		lg2 := openTestLog(t, dir, true)
		h2 := openQuotaHubClock(t, lg2, quotaMaxEntries, nowFn, "hog", "sink")
		assertReplayKeptEverything(t, h2, hog, sink, accepted)
	})
}

// acceptedSend is one send the live path accepted, kept with the EXACT request
// that produced it: a retry is only a retry if the key AND the payload match
// (same key + different payload is a protocol violation, invariant 10), so the
// original body has to be carried alongside the result.
type acceptedSend struct {
	key  string
	body []byte
	res  hub.Result
}

// assertReplayKeptEverything is the assertion both cases above turn on: the
// rebuilt table holds EVERY record the live path accepted, and a retry of the
// FIRST and the LAST of them still replays the original server-minted result.
//
// The count is checked as well as the retries because the two failures look
// different from the outside: a dropped key in the middle of the log would still
// let the endpoints replay, and a table that is merely short is the same defect
// one retry away from becoming a second message.
func assertReplayKeptEverything(t *testing.T, h *hub.Hub, sender, to string, accepted []acceptedSend) {
	t.Helper()
	want := len(accepted)
	st := h.IdempotencyStats()
	if st.Count != want {
		t.Fatalf("after the restart the rebuilt applied-key table holds %d of the %d keys the live path ACCEPTED, acknowledged and fsynced: replay refused %d of them. Replay is not admitting anything — every record it sees was already admitted by the run that accepted it — so a refusal here does not make the bus stricter, it DROPS a key, and the next retry of a dropped key is applied as a SECOND MESSAGE (invariant 10). The per-agent fair share must not be adjudicated on the replay path: see idem.Store.Recover. Stats: %+v", st.Count, want, want-st.Count, st)
	}
	// The endpoints of the accepted run, which is where a share-shaped drop
	// bites: a share refuses the TAIL, so the last key is the one that vanishes.
	for _, i := range []int{0, want - 1} {
		orig := accepted[i]
		again, err := mintedSend(t, h, hub.SendRequest{
			Sender:         sender,
			To:             to,
			Body:           orig.body,
			IdempotencyKey: orig.key,
		})
		if err != nil {
			t.Fatalf("retrying accepted key %q after the restart: %v", orig.key, err)
		}
		if !again.Replayed || again.MessageID != orig.res.MessageID || again.Seq != orig.res.Seq {
			t.Fatalf("retrying accepted key %q after the restart returned %+v, want the ORIGINAL %+v with Replayed set. The applied key was dropped by replay, so this retry was applied as a SECOND MESSAGE — the exact double-apply invariant 10 exists to prevent", orig.key, again, orig.res)
		}
	}
}
