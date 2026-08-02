// Package hub_test exercises the MSG/POLL wave through the EXPORTED surface
// only. Nothing here reaches into an unexported field: if a property cannot be
// observed from outside the package, a client cannot rely on it either, and a
// test that asserts it is testing the implementation rather than the contract.
package hub_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// testBusID is a literal that satisfies ids.BusIDPattern (`^[A-Za-z0-9_-]{1,64}$`).
// A literal rather than ids.GenerateBusID() so a failure message names the same
// bus every run and the fixtures a crash sweep produces are byte-reproducible.
const testBusID = "testbus"

// agentID builds the fully-qualified "<bus-id>.<name>-1" every enrolled test
// agent uses (invariant 2). Suffix 1 because ids.AgentID rejects 0.
func agentID(t *testing.T, busID, name string) string {
	t.Helper()
	id, err := ids.AgentID(busID, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%q, %q, 1): %v", busID, name, err)
	}
	return id
}

// newTestHub opens a real wal.Log in t.TempDir() and a Hub over it, with one
// enrolled agent per name in agents.
//
// It returns the hub, the log (so a test can reach Path() for a read-only
// replay, or Close() it to simulate a crash) and the data directory.
func newTestHub(t *testing.T, agents ...string) (*hub.Hub, *wal.Log, string) {
	t.Helper()
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	h := newHubOver(t, lg, testBusID, agents...)
	return h, lg, dir
}

// openTestLog opens the durable log in dir. When closeOnCleanup is false the
// caller owns Close — the crash sweep closes the log itself before truncating
// the file underneath it.
func openTestLog(t *testing.T, dir string, closeOnCleanup bool) *wal.Log {
	t.Helper()
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if closeOnCleanup {
		t.Cleanup(func() { _ = lg.Close() })
	}
	return lg
}

// newHubOver builds a Hub over an already-open log, wiring replay the way main
// does: a closure over wal.Replay on the log's own path (a read-only pass — a
// second wal.Open on the same directory would be a second writer).
func newHubOver(t *testing.T, lg *wal.Log, busID string, agents ...string) *hub.Hub {
	t.Helper()
	return newHubOverDurable(t, lg, lg, busID, agents...)
}

// newHubOverDurable is newHubOver with the DurableLog decoupled from the
// *wal.Log, so a test double (probeLog, in wait_test.go) can observe the write
// path while replay still comes off the real file.
func newHubOverDurable(t *testing.T, durable hub.DurableLog, lg *wal.Log, busID string, agents ...string) *hub.Hub {
	t.Helper()
	path := lg.Path()
	h, err := hub.Open(hub.Options{
		BusID:     busID,
		Durable:   durable,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, h, busID, agents...)
	return h
}

// enrolAll registers one agent per name on the hub's roster view.
func enrolAll(t *testing.T, h *hub.Hub, busID string, agents ...string) {
	t.Helper()
	for i, name := range agents {
		h.NoteEnrolment(hub.Agent{
			AgentID:    agentID(t, busID, name),
			Name:       name,
			EnrolledAt: time.Date(2026, 8, 2, 9, 0, i, 0, time.UTC),
		})
	}
}

// replayMessages reads every committed message record off the durable log
// WITHOUT going through the hub. It is how "durable before the call returned"
// is proven: the bytes are on disk and a fresh reader can see them.
//
// Decode failures are fatal here: this helper is used only against files no
// test has damaged.
func replayMessages(t *testing.T, path string) []store.Message {
	t.Helper()
	var out []store.Message
	_, err := wal.Replay(path, func(c wal.Committed) error {
		if c.Entry.Kind != store.RecordKind {
			return nil
		}
		m, err := store.Decode(c.Entry.Body)
		if err != nil {
			return fmt.Errorf("decoding a committed message record: %w", err)
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		t.Fatalf("wal.Replay(%s): %v", path, err)
	}
	return out
}

// findByID reports the message with the given id, and whether it was found.
func findByID(msgs []store.Message, id string) (store.Message, bool) {
	for _, m := range msgs {
		if m.ID == id {
			return m, true
		}
	}
	return store.Message{}, false
}

// historyIDs is the ordered list of message ids agentID can see from position 0.
func historyIDs(h *hub.Hub, agent string) []string {
	var out []string
	after := uint64(0)
	for {
		b := h.History(agent, after, hub.MaxBatchLimit)
		for _, m := range b.Messages {
			out = append(out, m.ID)
		}
		if !b.More {
			return out
		}
		after = b.Cursor
	}
}

// contains reports membership, so a failure message can say which list.
func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// storeShape is the pair of numbers every "nothing was written" assertion
// checks: how many messages are retained and where the head sequence sits.
type storeShape struct {
	count int
	head  uint64
}

func shapeOf(h *hub.Hub) storeShape {
	count, _, _, head, _ := h.Store().Stats()
	return storeShape{count: count, head: head}
}

// ---------------------------------------------------------------------------
// MSG-1 — the agent list
// ---------------------------------------------------------------------------

func TestListAgents(t *testing.T) {
	// Enrolled deliberately OUT of id order, so a passing sort assertion is
	// evidence of sorting rather than of insertion order.
	h, _, _ := newTestHub(t)
	busID := h.BusID()

	zeta := agentID(t, busID, "zeta")
	alpha := agentID(t, busID, "alpha")
	mid := agentID(t, busID, "mid")

	tZeta := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	tAlpha := time.Date(2026, 8, 2, 10, 0, 1, 0, time.UTC)
	tMid := time.Date(2026, 8, 2, 10, 0, 2, 0, time.UTC)

	h.NoteEnrolment(hub.Agent{AgentID: zeta, Name: "zeta", EnrolledAt: tZeta})
	h.NoteEnrolment(hub.Agent{AgentID: alpha, Name: "alpha", EnrolledAt: tAlpha})
	h.NoteEnrolment(hub.Agent{AgentID: mid, Name: "mid", EnrolledAt: tMid})

	t.Run("SortedByAgentID", func(t *testing.T) {
		got := h.Agents()
		if len(got) != 3 {
			t.Fatalf("Agents() returned %d entries, want 3: %+v", len(got), got)
		}
		want := []string{alpha, mid, zeta}
		for i, id := range want {
			if got[i].AgentID != id {
				t.Fatalf("Agents()[%d].AgentID = %q, want %q (full list %+v)", i, got[i].AgentID, id, got)
			}
		}
	})

	t.Run("CarriesNameAndEnrolledAt", func(t *testing.T) {
		wantName := map[string]string{alpha: "alpha", mid: "mid", zeta: "zeta"}
		wantAt := map[string]time.Time{alpha: tAlpha, mid: tMid, zeta: tZeta}
		checked := 0
		for _, a := range h.Agents() {
			name, ok := wantName[a.AgentID]
			if !ok {
				t.Fatalf("Agents() returned an agent nobody enrolled: %q", a.AgentID)
			}
			if a.Name != name {
				t.Fatalf("agent %q has Name %q, want %q", a.AgentID, a.Name, name)
			}
			if !a.EnrolledAt.Equal(wantAt[a.AgentID]) {
				t.Fatalf("agent %q has EnrolledAt %v, want %v", a.AgentID, a.EnrolledAt, wantAt[a.AgentID])
			}
			checked++
		}
		if checked != len(wantName) {
			t.Fatalf("checked %d agents, want %d — the assertion loop ran too few times to prove anything", checked, len(wantName))
		}
	})

	t.Run("ReturnsACopy", func(t *testing.T) {
		got := h.Agents()
		got[0].AgentID = "clobbered"
		got[0].Name = "clobbered"
		got = append(got, hub.Agent{AgentID: "injected", Name: "injected"})
		_ = got

		again := h.Agents()
		if len(again) != 3 {
			t.Fatalf("after mutating the returned slice, Agents() has %d entries, want 3: %+v", len(again), again)
		}
		if again[0].AgentID != alpha || again[0].Name != "alpha" {
			t.Fatalf("mutating the returned slice changed the hub's roster: got %+v", again[0])
		}
	})

	t.Run("NoteEnrolmentIsIdempotentFirstWriteWins", func(t *testing.T) {
		later := tAlpha.Add(72 * time.Hour)
		h.NoteEnrolment(hub.Agent{AgentID: alpha, Name: "impostor", EnrolledAt: later})

		got := h.Agents()
		if len(got) != 3 {
			t.Fatalf("re-enrolling an existing id created a second entry: %d entries, want 3: %+v", len(got), got)
		}
		if got[0].AgentID != alpha {
			t.Fatalf("Agents()[0] = %q, want %q", got[0].AgentID, alpha)
		}
		if got[0].Name != "alpha" {
			t.Fatalf("re-enrolment overwrote Name: got %q, want the FIRST value %q", got[0].Name, "alpha")
		}
		if !got[0].EnrolledAt.Equal(tAlpha) {
			t.Fatalf("re-enrolment overwrote EnrolledAt: got %v, want the FIRST value %v", got[0].EnrolledAt, tAlpha)
		}
	})

	t.Run("EmptyAgentIDIsIgnored", func(t *testing.T) {
		h.NoteEnrolment(hub.Agent{AgentID: "", Name: "nobody"})
		if got := h.Agents(); len(got) != 3 {
			t.Fatalf("an empty agent id was admitted to the roster: %+v", got)
		}
	})

	t.Run("Enrolled", func(t *testing.T) {
		cases := []struct {
			id   string
			want bool
		}{
			{alpha, true},
			{mid, true},
			{zeta, true},
			{agentID(t, busID, "ghost"), false},
			{"", false},
			{"not-an-agent-id", false},
		}
		if len(cases) == 0 {
			t.Fatal("the Enrolled table is empty")
		}
		for _, c := range cases {
			if got := h.Enrolled(c.id); got != c.want {
				t.Fatalf("Enrolled(%q) = %v, want %v", c.id, got, c.want)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// MSG-2 — broadcast
// ---------------------------------------------------------------------------

func TestBroadcastSend(t *testing.T) {
	t.Run("MintsServerSideIDAndSequence", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		res, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("hello"), IdempotencyKey: "k-mint"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		if res.Seq == 0 {
			t.Fatal("Broadcast returned sequence 0; sequence 0 is never allocated")
		}
		if res.Replayed {
			t.Fatal("a first send came back with Replayed set")
		}
		if !res.Broadcast {
			t.Fatal("Result.Broadcast is false for a broadcast")
		}
		if len(res.Recipients) != 0 {
			t.Fatalf("a broadcast carries no recipient list, got %v", res.Recipients)
		}
		if res.Sender != a {
			t.Fatalf("Result.Sender = %q, want %q", res.Sender, a)
		}
		busID, seq, err := ids.ParseMessageID(res.MessageID)
		if err != nil {
			t.Fatalf("ids.ParseMessageID(%q): %v", res.MessageID, err)
		}
		if busID != h.BusID() {
			t.Fatalf("message id %q carries bus %q, want %q", res.MessageID, busID, h.BusID())
		}
		if seq != res.Seq {
			t.Fatalf("message id %q carries sequence %d, but Result.Seq is %d", res.MessageID, seq, res.Seq)
		}
	})

	t.Run("DurableBeforeReturn", func(t *testing.T) {
		// Invariant 4: a send returns success only once the message is committed
		// and fsynced. Proven by reading the FILE — a second, read-only replay
		// that shares nothing with the hub's memory — the instant Broadcast
		// returns.
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		body := []byte("durable before the ack")

		res, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-durable"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		onDisk := replayMessages(t, lg.Path())
		m, ok := findByID(onDisk, res.MessageID)
		if !ok {
			t.Fatalf("Broadcast returned %s but the durable log holds %d message records and none of them is it", res.MessageID, len(onDisk))
		}
		if !m.Broadcast {
			t.Fatalf("the durable record for %s is not marked broadcast", res.MessageID)
		}
		if m.Sender != a {
			t.Fatalf("the durable record for %s names sender %q, want %q", res.MessageID, m.Sender, a)
		}
		if !bytes.Equal(m.Body, body) {
			t.Fatalf("the durable record for %s carries body %q, want %q", res.MessageID, m.Body, body)
		}
		if m.Seq != res.Seq {
			t.Fatalf("the durable record for %s carries sequence %d, want %d", res.MessageID, m.Seq, res.Seq)
		}
	})

	t.Run("VisibleToOthersNotSender", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		g := agentID(t, h.BusID(), "gamma")

		res, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("to everyone"), IdempotencyKey: "k-fanout"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		// The recipients. The counter makes an empty loop impossible to pass.
		seen := 0
		for _, recipient := range []string{b, g} {
			seenIDs := historyIDs(h, recipient)
			if !contains(seenIDs, res.MessageID) {
				t.Fatalf("%s cannot see broadcast %s; its history is %v", recipient, res.MessageID, seenIDs)
			}
			seen++
		}
		if seen != 2 {
			t.Fatalf("checked %d recipients, want 2", seen)
		}

		// The sender. store.Message.VisibleTo excludes it, and that is a stated
		// contract rather than an accident — pin it.
		if got := historyIDs(h, a); contains(got, res.MessageID) {
			t.Fatalf("the SENDER %s sees its own broadcast %s; VisibleTo excludes the sender by contract. History: %v", a, res.MessageID, got)
		}
	})

	t.Run("SequencesStrictlyIncrease", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		var prev uint64
		const n = 5
		for i := 0; i < n; i++ {
			res, err := h.Broadcast(hub.BroadcastRequest{
				Sender:         a,
				Body:           []byte(fmt.Sprintf("msg-%d", i)),
				IdempotencyKey: fmt.Sprintf("k-seq-%d", i),
			})
			if err != nil {
				t.Fatalf("Broadcast %d: %v", i, err)
			}
			if res.Seq <= prev {
				t.Fatalf("send %d took sequence %d, which is not strictly greater than the previous %d", i, res.Seq, prev)
			}
			prev = res.Seq
		}
		if prev == 0 {
			t.Fatal("no sequence was ever issued")
		}
	})

	t.Run("BodyAndContentHashCarriedExactly", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		bodies := [][]byte{
			[]byte("plain ascii"),
			{0x00, 0x01, 0xff, 0xfe, 0x7f, 0x80},
			[]byte(strings.Repeat("x", store.MaxBodyBytes)),
			[]byte("\xed\xa0\x80 lone surrogate bytes, not valid UTF-8"),
		}
		if len(bodies) == 0 {
			t.Fatal("the body table is empty")
		}
		checked := 0
		for i, body := range bodies {
			res, err := h.Broadcast(hub.BroadcastRequest{
				Sender:         a,
				Body:           body,
				IdempotencyKey: fmt.Sprintf("k-body-%d", i),
			})
			if err != nil {
				t.Fatalf("Broadcast body %d (%d bytes): %v", i, len(body), err)
			}
			batch := h.History(b, res.Seq-1, 1)
			if len(batch.Messages) != 1 {
				t.Fatalf("body %d: History returned %d messages, want 1", i, len(batch.Messages))
			}
			m := batch.Messages[0]
			if m.ID != res.MessageID {
				t.Fatalf("body %d: History returned %s, want %s", i, m.ID, res.MessageID)
			}
			if !bytes.Equal(m.Body, body) {
				t.Fatalf("body %d: round-tripped body differs (got %d bytes, want %d)", i, len(m.Body), len(body))
			}
			if want := store.ContentHash(body); m.ContentSHA256 != want {
				t.Fatalf("body %d: ContentSHA256 = %q, want the sha256 of the body %q", i, m.ContentSHA256, want)
			}
			if m.Size() != len(body) {
				t.Fatalf("body %d: Size() = %d, want %d", i, m.Size(), len(body))
			}
			checked++
		}
		if checked != len(bodies) {
			t.Fatalf("checked %d bodies, want %d", checked, len(bodies))
		}
	})

	t.Run("IdempotentRetryReturnsOriginal", func(t *testing.T) {
		// Invariant 10: same key + same payload is a LEGITIMATE retry. Return
		// the original result, apply nothing, error nothing.
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		body := []byte("the ack was lost in flight")

		first, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-retry"})
		if err != nil {
			t.Fatalf("first Broadcast: %v", err)
		}
		before := shapeOf(h)

		for attempt := 0; attempt < 3; attempt++ {
			again, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-retry"})
			if err != nil {
				t.Fatalf("retry %d: %v — a legitimate retry must not error", attempt, err)
			}
			if !again.Replayed {
				t.Fatalf("retry %d: Replayed is false; the caller cannot tell a replay from a fresh send", attempt)
			}
			if again.MessageID != first.MessageID || again.Seq != first.Seq {
				t.Fatalf("retry %d returned %s/%d, want the ORIGINAL %s/%d", attempt, again.MessageID, again.Seq, first.MessageID, first.Seq)
			}
			if !again.SentAt.Equal(first.SentAt) {
				t.Fatalf("retry %d returned SentAt %v, want the original %v", attempt, again.SentAt, first.SentAt)
			}
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("retrying created work: store went from %+v to %+v", before, after)
		}
	})

	t.Run("KeyReusedWithDifferentPayloadIsRejectedAndWritesNothing", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		if _, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("first"), IdempotencyKey: "k-reuse"}); err != nil {
			t.Fatalf("first Broadcast: %v", err)
		}
		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))

		_, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("SECOND, different"), IdempotencyKey: "k-reuse"})
		if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
			t.Fatalf("same key + different payload gave err = %v, want ErrIdempotencyKeyReused", err)
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("a rejected key-reuse changed the serving copy: %+v -> %+v", before, after)
		}
		if onDiskAfter := len(replayMessages(t, lg.Path())); onDiskAfter != onDiskBefore {
			t.Fatalf("a rejected key-reuse wrote to the durable log: %d records -> %d", onDiskBefore, onDiskAfter)
		}
	})

	t.Run("IdempotencyKeyIsScopedToTheSender", func(t *testing.T) {
		// The same key from a DIFFERENT agent is a different key: one agent can
		// neither collide with nor probe for another's.
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")

		first, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("mine"), IdempotencyKey: "shared-key"})
		if err != nil {
			t.Fatalf("Broadcast from %s: %v", a, err)
		}
		second, err := h.Broadcast(hub.BroadcastRequest{Sender: b, Body: []byte("also mine"), IdempotencyKey: "shared-key"})
		if err != nil {
			t.Fatalf("Broadcast from %s with the same key: %v — keys are scoped per sender", b, err)
		}
		if second.Replayed {
			t.Fatal("a different sender's identical key came back as a replay")
		}
		if second.Seq <= first.Seq {
			t.Fatalf("second send took sequence %d, not above %d", second.Seq, first.Seq)
		}
	})

	t.Run("InvalidIdempotencyKey", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		cases := []struct {
			name string
			key  string
		}{
			{"Missing", ""},
			{"Oversized", strings.Repeat("k", hub.MaxIdempotencyKeyLen+1)},
			{"Space", "bad key"},
			{"Slash", "bad/key"},
			{"NUL", "bad\x00key"},
			{"Newline", "bad\nkey"},
			{"NonASCII", "kéy"},
			{"Colon", "bad:key"},
		}
		if len(cases) == 0 {
			t.Fatal("the invalid-key table is empty")
		}
		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				_, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("x"), IdempotencyKey: c.key})
				if !errors.Is(err, hub.ErrInvalidIdempotencyKey) {
					t.Fatalf("key %q gave err = %v, want ErrInvalidIdempotencyKey", c.key, err)
				}
			})
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("a rejected idempotency key changed the serving copy: %+v -> %+v", before, after)
		}
		if onDiskAfter := len(replayMessages(t, lg.Path())); onDiskAfter != onDiskBefore {
			t.Fatalf("a rejected idempotency key wrote to the durable log: %d records -> %d", onDiskBefore, onDiskAfter)
		}
		// A key at exactly the limit, over the whole permitted alphabet, is VALID
		// — otherwise the rejections above would prove nothing about the boundary.
		okKey := strings.Repeat("aZ9._-", 21) // 126 bytes
		if len(okKey) > hub.MaxIdempotencyKeyLen {
			t.Fatalf("test bug: %d-byte key is over the %d limit", len(okKey), hub.MaxIdempotencyKeyLen)
		}
		if _, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("x"), IdempotencyKey: okKey}); err != nil {
			t.Fatalf("a %d-byte key over [A-Za-z0-9._-] was rejected: %v", len(okKey), err)
		}
	})

	t.Run("InvalidBody", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		cases := []struct {
			name string
			body []byte
		}{
			{"Nil", nil},
			{"Empty", []byte{}},
			{"OneOverTheLimit", bytes.Repeat([]byte("y"), store.MaxBodyBytes+1)},
			{"FarOverTheLimit", bytes.Repeat([]byte("y"), store.MaxBodyBytes*2)},
		}
		if len(cases) == 0 {
			t.Fatal("the invalid-body table is empty")
		}
		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))
		for i, c := range cases {
			c, i := c, i
			t.Run(c.name, func(t *testing.T) {
				_, err := h.Broadcast(hub.BroadcastRequest{
					Sender:         a,
					Body:           c.body,
					IdempotencyKey: fmt.Sprintf("k-badbody-%d", i),
				})
				if !errors.Is(err, hub.ErrInvalidBody) {
					t.Fatalf("body of %d bytes gave err = %v, want ErrInvalidBody", len(c.body), err)
				}
			})
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("a rejected body changed the serving copy: %+v -> %+v", before, after)
		}
		if onDiskAfter := len(replayMessages(t, lg.Path())); onDiskAfter != onDiskBefore {
			t.Fatalf("a rejected body wrote to the durable log: %d records -> %d", onDiskBefore, onDiskAfter)
		}
		// Exactly at the limit is accepted, so the rejection above is a boundary
		// and not a blanket refusal.
		if _, err := h.Broadcast(hub.BroadcastRequest{
			Sender:         a,
			Body:           bytes.Repeat([]byte("y"), store.MaxBodyBytes),
			IdempotencyKey: "k-body-at-limit",
		}); err != nil {
			t.Fatalf("a body of exactly store.MaxBodyBytes was rejected: %v", err)
		}
	})

	t.Run("UnknownSenderWritesNothing", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		ghost := agentID(t, h.BusID(), "ghost")

		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))

		_, err := h.Broadcast(hub.BroadcastRequest{Sender: ghost, Body: []byte("x"), IdempotencyKey: "k-ghost"})
		if !errors.Is(err, hub.ErrUnknownSender) {
			t.Fatalf("broadcast from an unenrolled agent gave err = %v, want ErrUnknownSender", err)
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("an unknown sender changed the serving copy: %+v -> %+v", before, after)
		}
		if onDiskAfter := len(replayMessages(t, lg.Path())); onDiskAfter != onDiskBefore {
			t.Fatalf("an unknown sender wrote to the durable log: %d records -> %d", onDiskBefore, onDiskAfter)
		}
	})
}

// ---------------------------------------------------------------------------
// MSG-3 — the directed send
// ---------------------------------------------------------------------------

func TestDirectMessageSend(t *testing.T) {
	t.Run("DeliveredOnlyToTheNamedRecipient", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		g := agentID(t, h.BusID(), "gamma")

		res, err := h.Send(hub.SendRequest{Sender: a, To: b, Body: []byte("for beta only"), IdempotencyKey: "k-dm"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if res.Broadcast {
			t.Fatal("Result.Broadcast is true for a directed send")
		}
		if len(res.Recipients) != 1 || res.Recipients[0] != b {
			t.Fatalf("Result.Recipients = %v, want [%s]", res.Recipients, b)
		}

		if got := historyIDs(h, b); !contains(got, res.MessageID) {
			t.Fatalf("the recipient %s cannot see DM %s; history %v", b, res.MessageID, got)
		}

		// The third party and the sender. Counted so the loop cannot be empty.
		excluded := 0
		for _, who := range []string{g, a} {
			got := historyIDs(h, who)
			if contains(got, res.MessageID) {
				t.Fatalf("%s can see DM %s addressed to %s; history %v", who, res.MessageID, b, got)
			}
			excluded++
		}
		if excluded != 2 {
			t.Fatalf("checked %d non-recipients, want 2", excluded)
		}
	})

	t.Run("DurableBeforeReturn", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		body := []byte("directed and durable")

		res, err := h.Send(hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-dm-durable"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		m, ok := findByID(replayMessages(t, lg.Path()), res.MessageID)
		if !ok {
			t.Fatalf("Send returned %s but it is not in the durable log", res.MessageID)
		}
		if m.Broadcast {
			t.Fatalf("the durable record for %s is marked broadcast", res.MessageID)
		}
		if len(m.Recipients) != 1 || m.Recipients[0] != b {
			t.Fatalf("the durable record for %s has recipients %v, want [%s]", res.MessageID, m.Recipients, b)
		}
		if !bytes.Equal(m.Body, body) {
			t.Fatalf("the durable record for %s carries body %q, want %q", res.MessageID, m.Body, body)
		}
		if !m.VisibleTo(b) {
			t.Fatalf("the durable record for %s is not visible to its recipient %s", res.MessageID, b)
		}
		if m.VisibleTo(a) {
			t.Fatalf("the durable record for %s is visible to its own sender %s", res.MessageID, a)
		}
	})

	t.Run("UnknownRecipientWritesNothing", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		ghost := agentID(t, h.BusID(), "ghost")

		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))

		_, err := h.Send(hub.SendRequest{Sender: a, To: ghost, Body: []byte("x"), IdempotencyKey: "k-unknown-to"})
		if !errors.Is(err, hub.ErrUnknownRecipient) {
			t.Fatalf("Send to an unenrolled agent gave err = %v, want ErrUnknownRecipient", err)
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("an unknown recipient changed the serving copy: %+v -> %+v", before, after)
		}
		if onDiskAfter := len(replayMessages(t, lg.Path())); onDiskAfter != onDiskBefore {
			t.Fatalf("an unknown recipient wrote to the durable log: %d records -> %d", onDiskBefore, onDiskAfter)
		}
		// And the key it presented was not consumed either: the same key with the
		// same payload to a REAL recipient must still be accepted as a first send.
		b := agentID(t, h.BusID(), "beta")
		res, err := h.Send(hub.SendRequest{Sender: a, To: b, Body: []byte("x"), IdempotencyKey: "k-unknown-to"})
		if err != nil {
			t.Fatalf("re-using the key of a REJECTED send: %v", err)
		}
		if res.Replayed {
			t.Fatal("the key of a rejected send was remembered as applied")
		}
	})

	t.Run("MalformedRecipient", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		cases := []struct {
			name string
			to   string
		}{
			{"Empty", ""},
			{"NoBusQualifier", "beta-1"},
			{"NoSuffix", testBusID + ".beta"},
			{"ZeroSuffix", testBusID + ".beta-0"},
			{"TwoDots", testBusID + ".beta.extra-1"},
			{"UppercaseName", testBusID + ".BETA-1"},
			{"LeadingZeroSuffix", testBusID + ".beta-01"},
			{"Oversized", testBusID + "." + strings.Repeat("b", 300) + "-1"},
		}
		if len(cases) == 0 {
			t.Fatal("the malformed-recipient table is empty")
		}
		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))
		for i, c := range cases {
			c, i := c, i
			t.Run(c.name, func(t *testing.T) {
				_, err := h.Send(hub.SendRequest{
					Sender:         a,
					To:             c.to,
					Body:           []byte("x"),
					IdempotencyKey: fmt.Sprintf("k-badto-%d", i),
				})
				if !errors.Is(err, hub.ErrInvalidRecipient) {
					t.Fatalf("Send to %q gave err = %v, want ErrInvalidRecipient", c.to, err)
				}
			})
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("a malformed recipient changed the serving copy: %+v -> %+v", before, after)
		}
		if onDiskAfter := len(replayMessages(t, lg.Path())); onDiskAfter != onDiskBefore {
			t.Fatalf("a malformed recipient wrote to the durable log: %d records -> %d", onDiskBefore, onDiskAfter)
		}
	})

	t.Run("SelfAddressedIsAcceptedButNeverVisible", func(t *testing.T) {
		// An agent may address itself; VisibleTo still excludes the sender, so
		// the message is durable and unread. Pinned because it is the one case
		// where "delivered only to the named recipient" and "the sender never
		// sees its own message" have to be reconciled.
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		res, err := h.Send(hub.SendRequest{Sender: a, To: a, Body: []byte("note to self"), IdempotencyKey: "k-self"})
		if err != nil {
			t.Fatalf("Send to self: %v", err)
		}
		if got := historyIDs(h, a); contains(got, res.MessageID) {
			t.Fatalf("a self-addressed message %s is visible to its sender; history %v", res.MessageID, got)
		}
	})

	t.Run("IdempotentRetryReturnsOriginal", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		g := agentID(t, h.BusID(), "gamma")
		body := []byte("retried DM")

		first, err := h.Send(hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-dm-retry"})
		if err != nil {
			t.Fatalf("first Send: %v", err)
		}
		before := shapeOf(h)

		again, err := h.Send(hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-dm-retry"})
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if !again.Replayed || again.MessageID != first.MessageID {
			t.Fatalf("retry returned %+v, want the original %s with Replayed set", again, first.MessageID)
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("retrying a DM created work: %+v -> %+v", before, after)
		}

		// Same key, same body, DIFFERENT recipient is a different request:
		// addressing is part of the fingerprint.
		_, err = h.Send(hub.SendRequest{Sender: a, To: g, Body: body, IdempotencyKey: "k-dm-retry"})
		if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
			t.Fatalf("same key, same body, different recipient gave err = %v, want ErrIdempotencyKeyReused", err)
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("a rejected re-address changed the serving copy: %+v -> %+v", before, after)
		}
	})

	t.Run("BroadcastAndDirectDoNotShareAnIdempotencyIdentity", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		body := []byte("same bytes, different shape")

		if _, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-shape"}); err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		_, err := h.Send(hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-shape"})
		if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
			t.Fatalf("a directed send reusing a broadcast's key gave err = %v, want ErrIdempotencyKeyReused", err)
		}
	})
}
