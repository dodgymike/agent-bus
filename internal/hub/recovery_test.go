package hub_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// MSG-5 — crash injection on the MESSAGING path.
//
// The property, stated the two ways it can fail:
//
//	EVERY PUBLISHED MESSAGE IS EITHER FULLY PRESENT OR FULLY ABSENT after
//	recovery — never partially visible, never a torn record served; and
//	WHAT SURVIVES IS A PREFIX of the accepted history (invariant 5).
//
// A crash is simulated the way internal/wal's own sweep does it: build the real
// file, close it, cut it at a chosen byte offset, and recover from the cut file.
// A torn tail is exactly what a crash between two fsyncs leaves behind.
//
// SCOPE: this proves SINGLE-WRITE atomicity. Retry-across-the-crash-boundary
// idempotency is IDEM-17 and is deliberately not built here. What IS asserted is
// narrower and in scope: the applied-key table is rebuilt from the records that
// SURVIVED, so retrying a survivor's key returns the original result rather than
// creating a second message.
// ---------------------------------------------------------------------------

// crashAgents are the names enrolled in every crash fixture and in every hub
// rebuilt from one. The roster is not durable yet (see hub.NoteEnrolment), so
// recovery re-enrols the same names — which is also what a restarted server
// does today.
var crashAgents = []string{"alpha", "beta", "gamma"}

// publishedMsg is one message the fixture accepted, remembered in full so
// recovery can be checked against it field by field.
type publishedMsg struct {
	id         string
	seq        uint64
	sender     string
	broadcast  bool
	recipients []string
	body       []byte
	key        string
}

// viewers are the agents entitled to see this message: everyone but the sender
// for a broadcast, the named recipients otherwise. It is the same rule
// store.Message.VisibleTo applies, restated from the SEND side so the test does
// not prove the implementation against itself.
func (p publishedMsg) viewers(all []string) []string {
	var out []string
	for _, a := range all {
		if a == p.sender {
			continue
		}
		if p.broadcast {
			out = append(out, a)
			continue
		}
		for _, r := range p.recipients {
			if r == a {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// crashFixture is one accepted history, plus the bytes it produced.
type crashFixture struct {
	dir       string
	agents    []string
	published []publishedMsg
	// sizes[i] is the file size after message i was accepted, so the records of
	// message i occupy [sizes[i-1], sizes[i]).
	sizes []int64
	size  int64
}

func (f crashFixture) maxSeq() uint64 {
	var m uint64
	for _, p := range f.published {
		if p.seq > m {
			m = p.seq
		}
	}
	return m
}

// buildCrashFixture publishes a MIX of broadcasts and directed sends through a
// real hub over a real WAL, then closes the log so the bytes can be cut.
func buildCrashFixture(t *testing.T) crashFixture {
	t.Helper()

	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	h := newHubOver(t, lg, testBusID, crashAgents...)

	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")
	g := agentID(t, testBusID, "gamma")

	f := crashFixture{dir: dir, agents: []string{a, b, g}}

	type step struct {
		sender string
		to     string // "" for a broadcast
		body   string
		key    string
	}
	steps := []step{
		{sender: a, body: "b0 broadcast from alpha", key: "ck-0"},
		{sender: a, to: b, body: "d1 alpha to beta", key: "ck-1"},
		{sender: b, body: "b2 broadcast from beta", key: "ck-2"},
		{sender: g, to: a, body: "d3 gamma to alpha", key: "ck-3"},
		{sender: g, body: "b4 broadcast from gamma", key: "ck-4"},
	}
	if len(steps) == 0 {
		t.Fatal("the crash fixture publishes nothing")
	}

	for i, s := range steps {
		var res hub.Result
		var err error
		if s.to == "" {
			res, err = h.Broadcast(hub.BroadcastRequest{Sender: s.sender, Body: []byte(s.body), IdempotencyKey: s.key})
		} else {
			res, err = h.Send(hub.SendRequest{Sender: s.sender, To: s.to, Body: []byte(s.body), IdempotencyKey: s.key})
		}
		if err != nil {
			t.Fatalf("fixture step %d: %v", i, err)
		}
		p := publishedMsg{
			id:         res.MessageID,
			seq:        res.Seq,
			sender:     s.sender,
			broadcast:  s.to == "",
			recipients: res.Recipients,
			body:       []byte(s.body),
			key:        s.key,
		}
		f.published = append(f.published, p)

		st, err := os.Stat(filepath.Join(dir, wal.WALFileName))
		if err != nil {
			t.Fatalf("stat after fixture step %d: %v", i, err)
		}
		f.sizes = append(f.sizes, st.Size())
	}

	if err := lg.Close(); err != nil {
		t.Fatalf("closing the fixture log: %v", err)
	}
	f.size = f.sizes[len(f.sizes)-1]
	if f.size <= 0 {
		t.Fatalf("the fixture WAL is %d bytes", f.size)
	}
	return f
}

// crashAt copies the fixture data directory into a fresh temp dir and truncates
// the WAL copy to n bytes. Everything else in the directory (the MAC key) is
// copied verbatim, because a crash does not lose it.
func crashAt(t *testing.T, f crashFixture, n int64) string {
	t.Helper()
	dst := t.TempDir()

	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatalf("reading the fixture dir: %v", err)
	}
	copied := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if e.Name() == wal.WALFileName {
			if n > int64(len(b)) {
				t.Fatalf("truncation length %d is past the %d-byte WAL", n, len(b))
			}
			b = b[:n]
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0600); err != nil {
			t.Fatalf("writing %s: %v", e.Name(), err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatal("the fixture dir held no files to crash-copy")
	}
	return dst
}

// historyOf drains everything agent can see, in sequence order.
func historyOf(h *hub.Hub, agent string) []store.Message {
	var out []store.Message
	after := uint64(0)
	for {
		b := h.History(agent, after, hub.MaxBatchLimit)
		out = append(out, b.Messages...)
		if !b.More {
			return out
		}
		after = b.Cursor
	}
}

// crashOffsets is the table of byte lengths the WAL is cut to.
//
// Byte-by-byte over the LAST message's records (the genuine crash window: the
// only records a crash between fsyncs can leave torn), plus a stride across
// everything before it and every inter-message boundary, so the sweep also
// covers the "several records lost" shapes that media damage produces.
func crashOffsets(f crashFixture) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(n int64) {
		if n < 0 || n > f.size || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}

	lastStart := f.sizes[len(f.sizes)-2]
	for n := lastStart; n <= f.size; n++ {
		add(n)
	}
	for _, s := range f.sizes {
		add(s)
		add(s - 1)
		add(s + 1)
	}
	const stride = 23
	for n := int64(0); n < lastStart; n += stride {
		add(n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestMessagingCrashRecovery(t *testing.T) {
	f := buildCrashFixture(t)
	offsets := crashOffsets(f)
	if len(offsets) == 0 {
		t.Fatal("the crash-offset table is empty")
	}
	t.Logf("fixture: %d messages, %d-byte WAL, message boundaries %v, %d truncation offsets",
		len(f.published), f.size, f.sizes, len(offsets))

	// The genuine crash window: at or past the start of the LAST message's
	// records, so every earlier record is intact. A crash between two fsyncs can
	// only ever tear the record being written, so this is the whole of what a
	// crash can produce, and it is where the STRONG sequence claim is asserted —
	// "strictly above every sequence ever issued, including one burned by a
	// message that did not survive".
	//
	// DEEPER CUTS ARE SWEPT TOO, but only for the all-or-nothing, prefix and
	// above-every-SURVIVOR properties. The strong claim is not asserted there,
	// and deliberately: the sequence floor is derived from the durable log's
	// high-water INDEX, and a cut that removes more records than the bus issued
	// sequences removes the very bytes that carry it. (Measured on this fixture:
	// every cut below ~1449 bytes of a 2523-byte, five-message log — i.e. losing
	// more than half the records — resumes at a sequence at or below one already
	// issued.) That is media damage, not a crash, and it is the same
	// unreconstructable-high-water-mark hazard hub.Open already logs at ERROR for
	// a QUARANTINED log. Asserting the strong claim here would assert something
	// no information on disk can support.
	crashWindowStart := f.sizes[len(f.sizes)-2]

	swept := 0
	survivorsSeen := 0
	for _, n := range offsets {
		n := n
		t.Run(fmt.Sprintf("TruncatedTo%d", n), func(t *testing.T) {
			dir := crashAt(t, f, n)

			lg, err := wal.Open(wal.LogOptions{Dir: dir})
			if err != nil {
				t.Fatalf("wal.Open after truncating to %d: %v — recovery must always reach a running server", n, err)
			}
			defer func() { _ = lg.Close() }()

			h := newHubOver(t, lg, testBusID, crashAgents...)

			// What each agent can see after recovery.
			views := map[string][]store.Message{}
			for _, who := range f.agents {
				views[who] = historyOf(h, who)
			}

			var survivingSeqs []uint64
			var survivors []publishedMsg
			for _, p := range f.published {
				viewers := p.viewers(f.agents)
				if len(viewers) == 0 {
					t.Fatalf("message %s has no eligible viewer, so its presence is unobservable", p.id)
				}

				presentIn := 0
				for _, who := range viewers {
					m, ok := findByID(views[who], p.id)
					if !ok {
						continue
					}
					presentIn++
					// FULLY present means the whole record, not just the id.
					if m.Seq != p.seq {
						t.Fatalf("%s: %s has sequence %d, want %d", who, p.id, m.Seq, p.seq)
					}
					if m.Sender != p.sender {
						t.Fatalf("%s: %s has sender %q, want %q", who, p.id, m.Sender, p.sender)
					}
					if m.Broadcast != p.broadcast {
						t.Fatalf("%s: %s has Broadcast=%v, want %v", who, p.id, m.Broadcast, p.broadcast)
					}
					if len(m.Recipients) != len(p.recipients) {
						t.Fatalf("%s: %s has recipients %v, want %v", who, p.id, m.Recipients, p.recipients)
					}
					for i := range p.recipients {
						if m.Recipients[i] != p.recipients[i] {
							t.Fatalf("%s: %s has recipients %v, want %v", who, p.id, m.Recipients, p.recipients)
						}
					}
					if !bytes.Equal(m.Body, p.body) {
						t.Fatalf("%s: %s has body %q, want %q", who, p.id, m.Body, p.body)
					}
					if want := store.ContentHash(p.body); m.ContentSHA256 != want {
						t.Fatalf("%s: %s asserts content hash %q but its body hashes to %q — a torn record was served", who, p.id, m.ContentSHA256, want)
					}
				}

				// ALL OR NOTHING. A message visible to some of its viewers and
				// not others is a half-applied record.
				if presentIn != 0 && presentIn != len(viewers) {
					t.Fatalf("%s is visible to %d of its %d entitled viewers — partially applied", p.id, presentIn, len(viewers))
				}

				// And never visible to anyone who was not entitled to it, even
				// after a crash.
				for _, who := range f.agents {
					entitled := false
					for _, v := range viewers {
						if v == who {
							entitled = true
						}
					}
					if entitled {
						continue
					}
					if _, ok := findByID(views[who], p.id); ok {
						t.Fatalf("after recovery %s can see %s, which it was never entitled to", who, p.id)
					}
				}

				if presentIn > 0 {
					survivingSeqs = append(survivingSeqs, p.seq)
					survivors = append(survivors, p)
				}
			}

			// PREFIX (invariant 5): the survivors are published[0..k], never a
			// hole followed by a survivor from after the cut.
			for i, seq := range survivingSeqs {
				if seq != f.published[i].seq {
					t.Fatalf("recovered history is not a prefix: survivor %d has sequence %d, but published[%d] is %d (survivors %v)",
						i, seq, i, f.published[i].seq, survivingSeqs)
				}
			}

			// The applied-key table is part of RECOVERED STATE: a survivor's key,
			// retried with the same payload, returns the ORIGINAL result and
			// creates nothing. (Scope: survivors only. Retry of a message that
			// did NOT survive is IDEM-17.)
			if len(survivors) > 0 {
				survivorsSeen++
				p := survivors[len(survivors)-1]
				before := shapeOf(h)
				var res hub.Result
				var err error
				if p.broadcast {
					res, err = h.Broadcast(hub.BroadcastRequest{Sender: p.sender, Body: p.body, IdempotencyKey: p.key})
				} else {
					res, err = h.Send(hub.SendRequest{Sender: p.sender, To: p.recipients[0], Body: p.body, IdempotencyKey: p.key})
				}
				if err != nil {
					t.Fatalf("retrying survivor %s's key after recovery: %v", p.id, err)
				}
				if !res.Replayed {
					t.Fatalf("retrying survivor %s's key after recovery produced a FRESH message %s; the applied-key table was not rebuilt from disk", p.id, res.MessageID)
				}
				if res.MessageID != p.id || res.Seq != p.seq {
					t.Fatalf("retry of %s returned %s/%d, want the original %s/%d", p.id, res.MessageID, res.Seq, p.id, p.seq)
				}
				if after := shapeOf(h); after != before {
					t.Fatalf("retrying a survivor's key changed the serving copy: %+v -> %+v", before, after)
				}
			}

			// The sequence must never regress (invariant 1).
			fresh, err := h.Broadcast(hub.BroadcastRequest{
				Sender:         f.agents[0],
				Body:           []byte("post-recovery"),
				IdempotencyKey: fmt.Sprintf("k-post-%d", n),
			})
			if err != nil {
				t.Fatalf("publishing after recovery from a %d-byte truncation: %v", n, err)
			}
			for _, seq := range survivingSeqs {
				if fresh.Seq <= seq {
					t.Fatalf("the first post-recovery message took sequence %d, which is not above the surviving sequence %d", fresh.Seq, seq)
				}
			}
			if n >= crashWindowStart {
				// Inside the crash window every record before the last message is
				// intact, so the log's high-water index still bounds every
				// sequence ever issued — including one burned by a message that
				// did NOT survive.
				if fresh.Seq <= f.maxSeq() {
					t.Fatalf("after a crash-window truncation to %d, the first post-recovery message took sequence %d, which is not above every sequence issued before the crash (%d) — a restart would reissue a message id (invariant 1)",
						n, fresh.Seq, f.maxSeq())
				}
			}
			if err := h.Poisoned(); err != nil {
				t.Fatalf("the hub rebuilt from a %d-byte truncation is poisoned: %v", n, err)
			}
		})
		swept++
	}
	if swept != len(offsets) {
		t.Fatalf("swept %d truncation offsets, want %d", swept, len(offsets))
	}
	if survivorsSeen == 0 {
		t.Fatal("no truncation offset in the sweep left a single surviving message, so the all-or-nothing assertions never ran")
	}
	t.Logf("sweep: %d offsets, %d of them left at least one surviving message", swept, survivorsSeen)
}

// TestMessagingCrashRecoveryLeavesAWorkingHub pins the other half of "recovery
// always reaches a RUNNING server": the rebuilt hub is not merely readable, it
// still parks and wakes long-polls.
func TestMessagingCrashRecoveryLeavesAWorkingHub(t *testing.T) {
	f := buildCrashFixture(t)

	// A cut inside the LAST message's records: the ordinary crash — a prefix of
	// accepted history plus one torn write.
	cut := (f.sizes[len(f.sizes)-2] + f.size) / 2
	dir := crashAt(t, f, cut)

	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open after truncating to %d: %v", cut, err)
	}
	defer func() { _ = lg.Close() }()
	h := newHubOver(t, lg, testBusID, crashAgents...)

	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")

	_, _, _, head, _ := h.Store().Stats()
	if head == 0 {
		t.Fatalf("recovery from a %d-byte truncation kept nothing, so the wake-up check would be vacuous", cut)
	}

	type outcome struct {
		batch hub.Batch
		err   error
	}
	out := make(chan outcome, 1)
	go func() {
		batch, err := h.Wait(context.Background(), b, head, 16, 10*time.Second)
		out <- outcome{batch, err}
	}()

	waitForWaiters(t, h, 1, "the post-recovery waiter must park")

	res, err := h.Broadcast(hub.BroadcastRequest{Sender: a, Body: []byte("alive after the crash"), IdempotencyKey: "k-alive"})
	if err != nil {
		t.Fatalf("publishing after recovery: %v", err)
	}

	select {
	case got := <-out:
		if got.err != nil {
			t.Fatalf("Wait after recovery: %v", got.err)
		}
		if got.batch.TimedOut {
			t.Fatal("a waiter parked on a recovered hub was never woken; recovery left the hub wedged")
		}
		if len(got.batch.Messages) != 1 || got.batch.Messages[0].ID != res.MessageID {
			t.Fatalf("the woken waiter got %v, want %s", got.batch.Messages, res.MessageID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the post-recovery waiter never returned")
	}
}
