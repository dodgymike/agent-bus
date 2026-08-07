// Package hub_test exercises the MSG/POLL wave through the EXPORTED surface
// only. Nothing here reaches into an unexported field: if a property cannot be
// observed from outside the package, a client cannot rely on it either, and a
// test that asserts it is testing the implementation rather than the contract.
package hub_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/signing"
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

// fixtureEnrolledAt is the enrolment instant every agent enrolled by enrolAll is
// given, for the whole test binary.
//
// It is ONE HOUR IN THE PAST, and that is load-bearing rather than arbitrary.
// store.Message.VisibleTo refuses to deliver a message sent BEFORE the reading
// agent enrolled (the enrolment epoch, added by the 2026-08-02 security audit),
// so a roster whose EnrolledAt sat after the fixture traffic would make every
// read in this package return nothing and every assertion below vacuous.
//
// It is also what makes the RECOVERY fixtures honest. The roster is not durable
// yet (AUTH-3), so a rebuilt hub has to be re-enrolled by hand; re-enrolling it
// at time.Now() would model a bus that has forgotten when its agents joined,
// which is not what AUTH-3 will deliver. Pinning one instant that precedes the
// fixture messages and using it in BOTH runs is the honest simulation of the
// durable roster that will restore each agent's ORIGINAL enrolment instant.
//
// A single package-level value, not a function, so the two runs of a recovery
// test cannot drift apart.
var fixtureEnrolledAt = time.Now().Add(-time.Hour)

// fixtureTimestampMs is the SENDER-clock reading (Unix milliseconds) every
// hand-built fixture message carries. store.NewMessage refuses 0 — "unset" — so
// a fixture needs a plausible positive value, and one shared constant keeps two
// fixtures from disagreeing about when the same fixture traffic happened.
const fixtureTimestampMs int64 = 1754130896789 // 2026-08-02T12:34:56.789Z

// fixtureSignature is a well-formed 64-byte placeholder. store.NewMessage
// enforces the LENGTH and nothing else — the bus never verifies a message
// signature, because it does not hold the sender's messaging key — so any
// signing.SignatureSize bytes are as good as a real signature here, and a
// constant one keeps a fixture reproducible byte for byte.
func fixtureSignature() []byte { return bytes.Repeat([]byte{0xAB}, signing.SignatureSize) }

// ---------------------------------------------------------------------------
// The two-step send: mint, then publish
// ---------------------------------------------------------------------------

// mintedSend and mintedBroadcast are how EVERY publish in this package is
// issued, and they exist because SIGN-1 made a send a TWO-STEP.
//
// Since SIGN-1 option (a) the SENDER signs the origin bus's minted message id
// and sequence, so a client must first reserve that assignment (Hub.Mint) and
// then present it back. A bare h.Send is no longer a send a real client can
// make: it is refused with ErrUnknownMint, because it names an idempotency key
// this bus holds no reservation for. Routing every test publish through these
// two helpers is what keeps the suite exercising the sequence a client actually
// performs rather than a shortcut only a test can take.
//
// # A MINT FAILURE IS NOT REPORTED, AND THAT IS DELIBERATE
//
// These helpers do NOT t.Fatal when Mint fails. Many tests here publish with
// arguments that are SUPPOSED to be refused — an empty idempotency key, a
// sender that is not on the roster, a poisoned or non-durable hub — and the
// property under test is the error the PUBLISH returns. Failing the test at the
// mint would hide that error behind a helper's own diagnosis, and worse, would
// hide it behind a DIFFERENT one: the mint and the publish reject an unenrolled
// sender with the same sentinel but an empty key with errors that are the same
// sentinel by design and not by accident. So a failed mint yields a zero
// SignedMint, the publish runs anyway, and the test sees exactly what a client
// would.
//
// # Why the mint is made from the request rather than passed in
//
// The scope of a reservation is the (agent, op, key) TUPLE, and it must be the
// SAME tuple the publish then presents. Deriving it here from the very request
// being issued makes the two impossible to get out of step; a caller-supplied
// mint would let a test drift into ErrMintMismatch by accident and spend an
// afternoon on it.
func mintedSend(t *testing.T, h *hub.Hub, req hub.SendRequest) (hub.Result, error) {
	t.Helper()
	req.SignedMint = mintFor(t, h, req.Sender, "send", req.IdempotencyKey)
	return h.Send(req)
}

// mintedBroadcast is mintedSend for a broadcast. The two are separate rather
// than one function with a flag because hub.Send and hub.Broadcast take
// different request types, and because the OP is part of the mint's scope —
// minting under "send" and spending under "broadcast" is a mismatch, not a
// detail.
func mintedBroadcast(t *testing.T, h *hub.Hub, req hub.BroadcastRequest) (hub.Result, error) {
	t.Helper()
	req.SignedMint = mintFor(t, h, req.Sender, "broadcast", req.IdempotencyKey)
	return h.Broadcast(req)
}

// mintFor reserves an assignment and dresses it as the SignedMint a client
// would present, or returns the zero value if the reservation was refused.
//
// The signature is fixtureSignature(): a well-formed 64-byte placeholder. The
// bus checks the LENGTH and never verifies — it does not hold the sender's
// messaging key — so a real signature would prove nothing here that a constant
// one does not, and a constant keeps a durable fixture reproducible byte for
// byte. The negative suite that proves the LENGTH check is enforced belongs to
// internal/httpapi, where the check actually lives.
func mintFor(t *testing.T, h *hub.Hub, sender, op, key string) hub.SignedMint {
	t.Helper()
	m, err := h.Mint(hub.MintRequest{Sender: sender, Op: op, IdempotencyKey: key})
	if err != nil {
		return hub.SignedMint{}
	}
	return hub.SignedMint{
		MessageID:          m.MessageID,
		Seq:                m.Seq,
		TimestampUnixMilli: fixtureTimestampMs,
		Signature:          fixtureSignature(),
	}
}

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
	h, roster := openHubOverDurable(t, durable, lg, busID, nil)
	enrolAll(t, roster, busID, agents...)
	return h
}

// openHubOverDurable opens a Hub and enrols NOBODY, returning the roster the hub
// serves from so a caller that needs to choose each agent's enrolment epoch
// (TestEnrolmentEpoch) can do so itself. now may be nil, in which case the hub
// uses the real clock.
//
// The roster is a hub.StaticRoster and is the ONLY one this hub has: since
// AUTH-7 the hub keeps no roster of its own and reads through to whatever is
// injected, so adding an agent here is adding it to the bus. In production the
// injected source is a view of internal/auth's durable roster; a test that wants
// to model a restart therefore re-populates this one with each agent's ORIGINAL
// EnrolledAt, exactly as recovery does.
func openHubOverDurable(t *testing.T, durable hub.DurableLog, lg *wal.Log, busID string, now func() time.Time) (*hub.Hub, *hub.StaticRoster) {
	t.Helper()
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID: busID,
		// The log's own directory, which is where main puts every durable file
		// this bus owns — including the message sequence floor the hub writes
		// ahead of every minted number (hub.SeqFloorFileName). Derived from the
		// log rather than taken as a parameter so the two can never point at
		// different directories in a test, which would silently give the hub a
		// floor file no restart of that log would ever read.
		DataDir:   filepath.Dir(path),
		Durable:   durable,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	return h, roster
}

// enrolAll registers one agent per name on the roster the hub serves from, all
// at fixtureEnrolledAt. See that variable for why the instant is in the past.
func enrolAll(t *testing.T, roster *hub.StaticRoster, busID string, agents ...string) {
	t.Helper()
	for _, name := range agents {
		roster.Add(hub.Agent{
			AgentID:    agentID(t, busID, name),
			Name:       name,
			EnrolledAt: fixtureEnrolledAt,
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
//
// A read error is FATAL here: every caller passes an enrolled agent, and
// History fails closed with ErrUnknownSender for anyone else — swallowing that
// would turn a roster bug into an empty list that reads like "saw nothing".
func historyIDs(t *testing.T, h *hub.Hub, agent string) []string {
	t.Helper()
	var out []string
	after := uint64(0)
	for {
		b, err := h.History(agent, after, hub.MaxBatchLimit)
		if err != nil {
			t.Fatalf("History(%q, %d): %v", agent, after, err)
		}
		for _, m := range b.Messages {
			out = append(out, m.ID)
		}
		if !b.More {
			return out
		}
		after = b.Cursor
	}
}

// mustHistory is History for a caller that expects it to succeed.
func mustHistory(t *testing.T, h *hub.Hub, agent string, after uint64, limit int) hub.Batch {
	t.Helper()
	b, err := h.History(agent, after, limit)
	if err != nil {
		t.Fatalf("History(%q, %d, %d): %v", agent, after, limit, err)
	}
	return b
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
// AUTH-7 — the roster is INJECTED, and its absence is a hard failure
// ---------------------------------------------------------------------------

// TestOpenRefusesAHubWithNoRoster pins the one thing that must never be
// reachable by omission.
//
// Every roster check in this package fails CLOSED, so a hub with an empty or
// missing roster does not break loudly: it starts, serves an empty agent list,
// answers 403 to every send and 404 to every recipient, and passes a health
// check while doing it. That is the exact shape of the AUTH-7 bug — a bus that
// authenticates everyone and serves nobody — so a defaulted empty roster is not
// a convenience, it is the defect with a different cause. Open must refuse.
func TestOpenRefusesAHubWithNoRoster(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	path := lg.Path()

	h, err := hub.Open(hub.Options{
		BusID:     testBusID,
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		// Roster deliberately omitted.
	})
	if err == nil {
		t.Fatalf("hub.Open with no Roster returned a usable hub (%p) instead of an error; a hub that cannot see the roster refuses every send and lists no agents, and it must not be possible to build one by forgetting a field", h)
	}
	if h != nil {
		t.Fatalf("hub.Open failed but still returned a hub: %p", h)
	}
	// The message has to name the thing that is missing: this failure is a
	// wiring mistake in a composition root, and "invalid options" would send the
	// reader looking in the wrong package.
	if !strings.Contains(err.Error(), "roster") {
		t.Fatalf("hub.Open error = %q, want it to name the missing roster", err)
	}
}

// TestRecoveredAgentIDsAbsentFromTheRosterAreReported covers the id-reuse
// detector, which was re-sited into Open when NoteEnrolment was deleted.
//
// An agent id that a REPLAYED MESSAGE names and that the roster does NOT hold is
// an id whose holder has vanished: nothing can authenticate as it and the name it
// was minted from is free to be enrolled again. The suffix floors are what stop
// the re-mint producing the same id, so a mismatch here is the shape a lost or
// restored-from-backup floors file leaves behind — and the standing rule in this
// project is that a reuse is never SILENT.
//
// The negative half is asserted too, and it is the half that would rot first: a
// check that fires for every recovered agent is a check an operator learns to
// ignore, which is the same as not having one.
func TestRecoveredAgentIDsAbsentFromTheRosterAreReported(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	a := agentID(t, testBusID, "alpha")
	b := agentID(t, testBusID, "beta")

	m, err := store.NewMessage(testBusID, a, false, []string{b}, 1, time.Now().UTC().Add(-time.Minute), []byte("recovered traffic"), "k-recovered", fixtureTimestampMs, fixtureSignature())
	if err != nil {
		t.Fatalf("store.NewMessage: %v", err)
	}
	payload, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := lg.Write(wal.Entry{Kind: store.RecordKind, Body: payload}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	openWithRoster := func(t *testing.T, roster *hub.StaticRoster) string {
		t.Helper()
		l := openTestLog(t, dir, true)
		buf := &bytes.Buffer{}
		path := l.Path()
		if _, err := hub.Open(hub.Options{
			BusID:     testBusID,
			DataDir:   dir,
			Durable:   l,
			Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
			NextIndex: l.Recovered().NextIndex,
			Roster:    roster,
			Logger:    logging.New(buf, logging.LevelError),
		}); err != nil {
			t.Fatalf("hub.Open: %v", err)
		}
		return buf.String()
	}

	t.Run("an empty roster reports both the sender and the recipient", func(t *testing.T) {
		out := openWithRoster(t, hub.NewStaticRoster())
		if !strings.Contains(out, "ABSENT FROM THE ROSTER") {
			t.Fatalf("recovering a message naming agents that are NOT enrolled logged nothing at ERROR; the reuse must never be silent.\nlog: %s", out)
		}
		for _, id := range []string{a, b} {
			if !strings.Contains(out, id) {
				t.Fatalf("the report does not name %q, so an operator cannot act on it.\nlog: %s", id, out)
			}
		}
	})

	t.Run("a roster holding them says nothing", func(t *testing.T) {
		roster := hub.NewStaticRoster()
		roster.Add(hub.Agent{AgentID: a, Name: "alpha", EnrolledAt: fixtureEnrolledAt})
		roster.Add(hub.Agent{AgentID: b, Name: "beta", EnrolledAt: fixtureEnrolledAt})
		if out := openWithRoster(t, roster); strings.Contains(out, "ABSENT FROM THE ROSTER") {
			t.Fatalf("an ordinary recovery — every recovered id present in the roster — logged an id-reuse ERROR; a check that fires on healthy startups is one nobody reads.\nlog: %s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// MSG-1 — the agent list
// ---------------------------------------------------------------------------

func TestListAgents(t *testing.T) {
	// Enrolled deliberately OUT of id order, so a passing sort assertion is
	// evidence of sorting rather than of insertion order.
	lg := openTestLog(t, t.TempDir(), true)
	h, roster := openHubOverDurable(t, lg, lg, testBusID, nil)
	busID := h.BusID()

	zeta := agentID(t, busID, "zeta")
	alpha := agentID(t, busID, "alpha")
	mid := agentID(t, busID, "mid")

	tZeta := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	tAlpha := time.Date(2026, 8, 2, 10, 0, 1, 0, time.UTC)
	tMid := time.Date(2026, 8, 2, 10, 0, 2, 0, time.UTC)

	roster.Add(hub.Agent{AgentID: zeta, Name: "zeta", EnrolledAt: tZeta})
	roster.Add(hub.Agent{AgentID: alpha, Name: "alpha", EnrolledAt: tAlpha})
	roster.Add(hub.Agent{AgentID: mid, Name: "mid", EnrolledAt: tMid})

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

	t.Run("AddingAnExistingAgentIDIsIdempotentFirstWriteWins", func(t *testing.T) {
		later := tAlpha.Add(72 * time.Hour)
		roster.Add(hub.Agent{AgentID: alpha, Name: "impostor", EnrolledAt: later})

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
		roster.Add(hub.Agent{AgentID: "", Name: "nobody"})
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

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("hello"), IdempotencyKey: "k-mint"})
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

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-durable"})
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

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("to everyone"), IdempotencyKey: "k-fanout"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		// The recipients. The counter makes an empty loop impossible to pass.
		seen := 0
		for _, recipient := range []string{b, g} {
			seenIDs := historyIDs(t, h, recipient)
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
		if got := historyIDs(t, h, a); contains(got, res.MessageID) {
			t.Fatalf("the SENDER %s sees its own broadcast %s; VisibleTo excludes the sender by contract. History: %v", a, res.MessageID, got)
		}
	})

	t.Run("SequencesStrictlyIncrease", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")

		var prev uint64
		const n = 5
		for i := 0; i < n; i++ {
			res, err := mintedBroadcast(t, h, hub.BroadcastRequest{
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
			res, err := mintedBroadcast(t, h, hub.BroadcastRequest{
				Sender:         a,
				Body:           body,
				IdempotencyKey: fmt.Sprintf("k-body-%d", i),
			})
			if err != nil {
				t.Fatalf("Broadcast body %d (%d bytes): %v", i, len(body), err)
			}
			batch := mustHistory(t, h, b, res.Seq-1, 1)
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

		first, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-retry"})
		if err != nil {
			t.Fatalf("first Broadcast: %v", err)
		}
		before := shapeOf(h)

		for attempt := 0; attempt < 3; attempt++ {
			again, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-retry"})
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

		if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("first"), IdempotencyKey: "k-reuse"}); err != nil {
			t.Fatalf("first Broadcast: %v", err)
		}
		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))

		_, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("SECOND, different"), IdempotencyKey: "k-reuse"})
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

		first, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("mine"), IdempotencyKey: "shared-key"})
		if err != nil {
			t.Fatalf("Broadcast from %s: %v", a, err)
		}
		second, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: b, Body: []byte("also mine"), IdempotencyKey: "shared-key"})
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
				_, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("x"), IdempotencyKey: c.key})
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
		if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("x"), IdempotencyKey: okKey}); err != nil {
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
				_, err := mintedBroadcast(t, h, hub.BroadcastRequest{
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
		if _, err := mintedBroadcast(t, h, hub.BroadcastRequest{
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

		_, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: ghost, Body: []byte("x"), IdempotencyKey: "k-ghost"})
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

		res, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: []byte("for beta only"), IdempotencyKey: "k-dm"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if res.Broadcast {
			t.Fatal("Result.Broadcast is true for a directed send")
		}
		if len(res.Recipients) != 1 || res.Recipients[0] != b {
			t.Fatalf("Result.Recipients = %v, want [%s]", res.Recipients, b)
		}

		if got := historyIDs(t, h, b); !contains(got, res.MessageID) {
			t.Fatalf("the recipient %s cannot see DM %s; history %v", b, res.MessageID, got)
		}

		// The third party and the sender. Counted so the loop cannot be empty.
		excluded := 0
		for _, who := range []string{g, a} {
			got := historyIDs(t, h, who)
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

		res, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-dm-durable"})
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
		// Read back with the SAME enrolment epoch the roster holds, never the
		// zero time: a zero epoch disables the enrolment-epoch filter, so
		// asserting visibility with it would assert less than the read path does.
		if !m.VisibleTo(b, fixtureEnrolledAt) {
			t.Fatalf("the durable record for %s is not visible to its recipient %s", res.MessageID, b)
		}
		if m.VisibleTo(a, fixtureEnrolledAt) {
			t.Fatalf("the durable record for %s is visible to its own sender %s", res.MessageID, a)
		}
		if m.VisibleTo(b, m.SentAt.Add(time.Nanosecond)) {
			t.Fatalf("the durable record for %s is visible to a %s that enrolled AFTER it was sent", res.MessageID, b)
		}
	})

	t.Run("UnknownRecipientWritesNothing", func(t *testing.T) {
		h, lg, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		ghost := agentID(t, h.BusID(), "ghost")

		before := shapeOf(h)
		onDiskBefore := len(replayMessages(t, lg.Path()))

		_, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: ghost, Body: []byte("x"), IdempotencyKey: "k-unknown-to"})
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
		res, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: []byte("x"), IdempotencyKey: "k-unknown-to"})
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
				_, err := mintedSend(t, h, hub.SendRequest{
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

		res, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: a, Body: []byte("note to self"), IdempotencyKey: "k-self"})
		if err != nil {
			t.Fatalf("Send to self: %v", err)
		}
		if got := historyIDs(t, h, a); contains(got, res.MessageID) {
			t.Fatalf("a self-addressed message %s is visible to its sender; history %v", res.MessageID, got)
		}
	})

	t.Run("IdempotentRetryReturnsOriginal", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta", "gamma")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		g := agentID(t, h.BusID(), "gamma")
		body := []byte("retried DM")

		first, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-dm-retry"})
		if err != nil {
			t.Fatalf("first Send: %v", err)
		}
		before := shapeOf(h)

		again, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-dm-retry"})
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
		_, err = mintedSend(t, h, hub.SendRequest{Sender: a, To: g, Body: body, IdempotencyKey: "k-dm-retry"})
		if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
			t.Fatalf("same key, same body, different recipient gave err = %v, want ErrIdempotencyKeyReused", err)
		}
		if after := shapeOf(h); after != before {
			t.Fatalf("a rejected re-address changed the serving copy: %+v -> %+v", before, after)
		}
	})

	// CHANGED BY IDEM-11 (2026-08-02), deliberately, and this is the same
	// property stated more strongly rather than a weakened one.
	//
	// The applied-key table is now scoped by idem.Scope = (agent, OPERATION,
	// key), which is IDEM-10's settled key format. A broadcast and a directed
	// send that happen to reuse one key are therefore in DIFFERENT KEY SPACES,
	// not one key space distinguished by fingerprint.
	//
	// Before, "they do not share an identity" was expressed as "the second one
	// is rejected as a reused key" — which punished a client using a per-route
	// counter for doing nothing wrong. Now it is expressed directly: each is a
	// distinct operation with a distinct result, and each replays ITS OWN
	// original result on retry. That is a stronger assertion — it pins what
	// each key resolves to, not merely that the second call failed — and the
	// cross-payload violation check for a key reused WITHIN one operation is
	// still asserted above (same key, same body, different recipient).
	t.Run("BroadcastAndDirectDoNotShareAnIdempotencyIdentity", func(t *testing.T) {
		h, _, _ := newTestHub(t, "alpha", "beta")
		a := agentID(t, h.BusID(), "alpha")
		b := agentID(t, h.BusID(), "beta")
		body := []byte("same bytes, different shape")

		bc, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-shape"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		dm, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-shape"})
		if err != nil {
			t.Fatalf("a directed send reusing a broadcast's key is a DIFFERENT scope and must be accepted, got err = %v", err)
		}
		if dm.MessageID == bc.MessageID || dm.Replayed {
			t.Fatalf("the directed send was answered from the broadcast's applied-key entry: broadcast %+v, send %+v", bc, dm)
		}

		// And each key still replays its OWN original result, which is what
		// "do not share an identity" actually means.
		bcAgain, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: body, IdempotencyKey: "k-shape"})
		if err != nil {
			t.Fatalf("broadcast retry: %v", err)
		}
		if !bcAgain.Replayed || bcAgain.MessageID != bc.MessageID {
			t.Fatalf("broadcast retry returned %+v, want the original %s with Replayed set", bcAgain, bc.MessageID)
		}
		dmAgain, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: b, Body: body, IdempotencyKey: "k-shape"})
		if err != nil {
			t.Fatalf("send retry: %v", err)
		}
		if !dmAgain.Replayed || dmAgain.MessageID != dm.MessageID {
			t.Fatalf("send retry returned %+v, want the original %s with Replayed set", dmAgain, dm.MessageID)
		}
	})
}

// ---------------------------------------------------------------------------
// THE ENROLMENT EPOCH — the P0 closed by the 2026-08-02 security audit.
//
// A message sent BEFORE an agent enrolled is never delivered to it, whatever it
// is addressed to. The hole it closes is specific and is worth restating here,
// because a future reader will otherwise be tempted to relax it:
//
//	Message records are durable and they NAME agent ids. Enrolment is not
//	durable yet (AUTH-3), so the per-name suffix counter restarts at 1 on every
//	boot — after a restart, whoever enrols the name "alpha" is minted the id
//	"<bus>.alpha-1" the PREVIOUS alpha held, and without the epoch could read a
//	full retention window of that agent's direct messages. The bus cannot tell
//	the two apart by ID, because an id is exactly what is being reused. It CAN
//	tell them apart by TIME.
//
// Both halves are asserted below, and both are needed: the recovered messages
// are NOT DELIVERED, and they ARE STILL THERE. Durability is intact; delivery
// is refused. A test that checked only the first half would pass just as well
// against a bus that had lost them.
// ---------------------------------------------------------------------------

func TestEnrolmentEpoch(t *testing.T) {
	// A clock pinned in the past, so a message can be published with a SentAt
	// that PRECEDES an enrolment the test performs afterwards. Without it there
	// is no way to publish "before" an agent that already exists.
	pinned := time.Now().Add(-time.Hour)
	pinnedNow := func() time.Time { return pinned }

	t.Run("HistoryRefusesTrafficThatPredatesTheReader", func(t *testing.T) {
		dir := t.TempDir()
		lg := openTestLog(t, dir, true)
		h, roster := openHubOverDurable(t, lg, lg, testBusID, pinnedNow)

		a := agentID(t, testBusID, "alpha")
		early := agentID(t, testBusID, "early")
		late := agentID(t, testBusID, "late")

		roster.Add(hub.Agent{AgentID: a, Name: "alpha", EnrolledAt: pinned.Add(-time.Minute)})
		roster.Add(hub.Agent{AgentID: early, Name: "early", EnrolledAt: pinned.Add(-time.Minute)})
		// LATE enrols one minute after the messages below are sent.
		roster.Add(hub.Agent{AgentID: late, Name: "late", EnrolledAt: pinned.Add(time.Minute)})

		bc, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("before late existed"), IdempotencyKey: "k-epoch-bc"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		// Addressed to LATE BY NAME, and still not deliverable: the epoch is not
		// a broadcast-only rule.
		dm, err := mintedSend(t, h, hub.SendRequest{Sender: a, To: late, Body: []byte("a DM late must not read"), IdempotencyKey: "k-epoch-dm"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}

		// An agent enrolled BEFORE the traffic receives it, so the refusal below
		// is discrimination on the epoch and not an empty bus.
		earlySees := historyIDs(t, h, early)
		if !contains(earlySees, bc.MessageID) {
			t.Fatalf("%s enrolled before the broadcast but cannot see %s; history %v", early, bc.MessageID, earlySees)
		}

		lateSees := historyIDs(t, h, late)
		if len(lateSees) != 0 {
			t.Fatalf("%s enrolled AFTER every message on the bus but its history is %v; a message sent before an agent existed is never delivered to it", late, lateSees)
		}
		if contains(lateSees, dm.MessageID) {
			t.Fatalf("%s read DM %s, which was sent before it enrolled", late, dm.MessageID)
		}

		// Both messages are still RETAINED. The epoch refuses delivery; it does
		// not delete, and it does not make the bus lose data.
		count, _, _, head, dropped := h.Store().Stats()
		if count != 2 || head != dm.Seq || dropped != 0 {
			t.Fatalf("Stats = (count %d, head %d, dropped %d), want (2, %d, 0) — the epoch must refuse delivery, not drop messages", count, head, dropped, dm.Seq)
		}
	})

	t.Run("AParkedPollIsNotWokenByTrafficThatPredatesTheReader", func(t *testing.T) {
		// This is the half that proves NOTIFY filters on the epoch and not only
		// the batch read. A waiter woken and then handed nothing would still
		// "pass" a history-only test, while burning a wake-up on every send.
		dir := t.TempDir()
		lg := openTestLog(t, dir, true)
		h, roster := openHubOverDurable(t, lg, lg, testBusID, pinnedNow)

		a := agentID(t, testBusID, "alpha")
		early := agentID(t, testBusID, "early")
		late := agentID(t, testBusID, "late")

		roster.Add(hub.Agent{AgentID: a, Name: "alpha", EnrolledAt: pinned.Add(-time.Minute)})
		roster.Add(hub.Agent{AgentID: early, Name: "early", EnrolledAt: pinned.Add(-time.Minute)})
		roster.Add(hub.Agent{AgentID: late, Name: "late", EnrolledAt: pinned.Add(time.Minute)})

		type outcome struct {
			batch hub.Batch
			err   error
		}
		const lateTimeout = 700 * time.Millisecond
		lateOut := make(chan outcome, 1)
		earlyOut := make(chan outcome, 1)
		go func() {
			batch, err := h.Wait(context.Background(), late, 0, 10, lateTimeout)
			lateOut <- outcome{batch, err}
		}()
		go func() {
			batch, err := h.Wait(context.Background(), early, 0, 10, 20*time.Second)
			earlyOut <- outcome{batch, err}
		}()

		waitForWaiters(t, h, 2, "both readers must park before the broadcast")

		res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("wake only the eligible"), IdempotencyKey: "k-epoch-wake"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}

		// The eligible reader IS woken. Without this the timeout below could be
		// explained by a broadcast that never reached the waiter registry at all.
		select {
		case got := <-earlyOut:
			if got.err != nil {
				t.Fatalf("%s: Wait: %v", early, got.err)
			}
			if got.batch.TimedOut {
				t.Fatalf("%s enrolled before the broadcast and was not woken by it", early)
			}
			if len(got.batch.Messages) != 1 || got.batch.Messages[0].ID != res.MessageID {
				t.Fatalf("%s got %v, want exactly %s", early, got.batch.Messages, res.MessageID)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s never returned", early)
		}

		select {
		case got := <-lateOut:
			if got.err != nil {
				t.Fatalf("%s: Wait: %v", late, got.err)
			}
			if !got.batch.TimedOut {
				t.Fatalf("%s's poll returned before its deadline: it was WOKEN by %s, which was sent before %s enrolled — notify is not filtering on the enrolment epoch", late, res.MessageID, late)
			}
			if len(got.batch.Messages) != 0 {
				t.Fatalf("%s received %v, all of it sent before it enrolled", late, got.batch.Messages)
			}
			if got.batch.Cursor != 0 {
				t.Fatalf("%s's cursor moved to %d without a delivery", late, got.batch.Cursor)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s never returned", late)
		}
	})

	t.Run("AReusedAgentIDAfterARestartInheritsNoTraffic", func(t *testing.T) {
		// THE ATTACK THE EPOCH EXISTS FOR. The suffix counter restarts at 1 on
		// every boot, so a stranger who enrols the name "beta" is minted the id
		// the previous beta held. It must read none of that agent's traffic — and
		// the traffic must still be on the bus, or this would be proving that
		// recovery lost it.
		dir := t.TempDir()

		// --- Run 1: the ORIGINAL beta receives a DM and a broadcast.
		lg1 := openTestLog(t, dir, false)
		h1 := newHubOver(t, lg1, testBusID, "alpha", "beta")
		a := agentID(t, testBusID, "alpha")
		b := agentID(t, testBusID, "beta")

		dm, err := mintedSend(t, h1, hub.SendRequest{Sender: a, To: b, Body: []byte("the previous beta's private mail"), IdempotencyKey: "k-reuse-dm"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		bc, err := mintedBroadcast(t, h1, hub.BroadcastRequest{Sender: a, Body: []byte("the previous beta's bus traffic"), IdempotencyKey: "k-reuse-bc"})
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		if got := historyIDs(t, h1, b); len(got) != 2 {
			t.Fatalf("before the restart the original %s sees %v, want both %s and %s", b, got, dm.MessageID, bc.MessageID)
		}
		if err := lg1.Close(); err != nil {
			t.Fatalf("closing the first log: %v", err)
		}

		// --- Run 2: a DIFFERENT keypair enrols the same name and is minted the
		// same id. Its enrolment instant is NOW, after everything on disk.
		lg2 := openTestLog(t, dir, false)
		h2, roster2 := openHubOverDurable(t, lg2, lg2, testBusID, nil)
		roster2.Add(hub.Agent{AgentID: a, Name: "alpha", EnrolledAt: time.Now()})
		roster2.Add(hub.Agent{AgentID: b, Name: "beta", EnrolledAt: time.Now()})

		// NOT DELIVERED — via history…
		if got := historyIDs(t, h2, b); len(got) != 0 {
			t.Fatalf("the NEW holder of %s read %v after a restart; that is the previous holder's traffic", b, got)
		}
		// …and via the long poll, which must TIME OUT rather than be woken.
		batch, err := h2.Wait(context.Background(), b, 0, 10, 300*time.Millisecond)
		if err != nil {
			t.Fatalf("Wait on the recovered hub: %v", err)
		}
		if !batch.TimedOut || len(batch.Messages) != 0 {
			t.Fatalf("the NEW holder of %s was handed %+v by a long poll", b, batch)
		}

		// STILL PRESENT. Read through the store directly with the ZERO epoch —
		// the roster-less, audit-tool read — which is the one caller allowed to
		// disable the filter. This is what distinguishes "not delivered" from
		// "lost".
		count, _, _, head, dropped := h2.Store().Stats()
		if count != 2 || head != bc.Seq || dropped != 0 {
			t.Fatalf("the recovered store holds (count %d, head %d, dropped %d), want (2, %d, 0) — the messages must survive the restart, the epoch only refuses to DELIVER them", count, head, dropped, bc.Seq)
		}
		audit, _, _ := h2.Store().Since(b, time.Time{}, 0, 100)
		if len(audit) != 2 {
			t.Fatalf("a roster-less audit read of the recovered store returned %d messages, want the 2 that were written", len(audit))
		}
		found := map[string]bool{}
		for _, m := range audit {
			found[m.ID] = true
		}
		if !found[dm.MessageID] || !found[bc.MessageID] {
			t.Fatalf("the recovered store is missing %s or %s: it holds %v", dm.MessageID, bc.MessageID, found)
		}
		if err := lg2.Close(); err != nil {
			t.Fatalf("closing the second log: %v", err)
		}

		// --- Run 3: the SAME id re-enrolled with its ORIGINAL instant — what the
		// durable roster of AUTH-3 will restore — reads its history back in full.
		// Without this the refusal above is consistent with a recovered hub that
		// simply serves nobody anything.
		lg3 := openTestLog(t, dir, true)
		h3, roster3 := openHubOverDurable(t, lg3, lg3, testBusID, nil)
		roster3.Add(hub.Agent{AgentID: a, Name: "alpha", EnrolledAt: fixtureEnrolledAt})
		roster3.Add(hub.Agent{AgentID: b, Name: "beta", EnrolledAt: fixtureEnrolledAt})
		got := historyIDs(t, h3, b)
		if len(got) != 2 || !contains(got, dm.MessageID) || !contains(got, bc.MessageID) {
			t.Fatalf("a genuinely continuous %s (re-enrolled at its ORIGINAL instant) sees %v, want both %s and %s", b, got, dm.MessageID, bc.MessageID)
		}
	})
}

// ---------------------------------------------------------------------------
// Both read paths FAIL CLOSED for an agent that is not on the roster.
//
// The alternative — reading with a zero enrolment epoch — disables the epoch
// filter (see store.Message.VisibleTo), so an empty roster would serve
// EVERYTHING to ANYONE rather than nothing to nobody. The dangerous shape is a
// nil error with an empty batch, which every caller would read as "you are up to
// date"; it is asserted against explicitly below.
// ---------------------------------------------------------------------------

func TestReadPathsFailClosedForAnUnknownAgent(t *testing.T) {
	h, _, _ := newTestHub(t, "alpha", "beta")
	a := agentID(t, h.BusID(), "alpha")
	b := agentID(t, h.BusID(), "beta")

	res, err := mintedBroadcast(t, h, hub.BroadcastRequest{Sender: a, Body: []byte("visible to the enrolled"), IdempotencyKey: "k-failclosed"})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	// There IS something to read, so an empty result below is a refusal and not
	// an empty bus.
	if got := historyIDs(t, h, b); !contains(got, res.MessageID) {
		t.Fatalf("the enrolled %s cannot see %s; history %v", b, res.MessageID, got)
	}

	strangers := []struct {
		name string
		id   string
	}{
		{"NeverEnrolled", agentID(t, h.BusID(), "ghost")},
		{"EmptyAgentID", ""},
		{"MalformedAgentID", "not-an-agent-id"},
		{"ForeignBusQualifier", "otherbus.alpha-1"},
		{"RightNameWrongSuffix", testBusID + ".alpha-2"},
	}
	if len(strangers) == 0 {
		t.Fatal("the unknown-agent table is empty")
	}
	checked := 0
	const after = uint64(7)
	for _, s := range strangers {
		s := s
		t.Run(s.name, func(t *testing.T) {
			batch, err := h.History(s.id, after, 10)
			if !errors.Is(err, hub.ErrUnknownSender) {
				t.Fatalf("History(%q) = (%+v, %v), want ErrUnknownSender — a silent empty batch reads to every caller as \"you are up to date\"", s.id, batch, err)
			}
			if len(batch.Messages) != 0 {
				t.Fatalf("History(%q) refused AND returned %d messages", s.id, len(batch.Messages))
			}
			if batch.Cursor != after {
				t.Fatalf("History(%q) moved the cursor from %d to %d", s.id, after, batch.Cursor)
			}

			start := time.Now()
			wbatch, werr := h.Wait(context.Background(), s.id, after, 10, 20*time.Second)
			elapsed := time.Since(start)
			if !errors.Is(werr, hub.ErrUnknownSender) {
				t.Fatalf("Wait(%q) = (%+v, %v), want ErrUnknownSender", s.id, wbatch, werr)
			}
			if elapsed > 5*time.Second {
				t.Fatalf("Wait(%q) took %v; an unknown agent must be refused before it parks", s.id, elapsed)
			}
			if len(wbatch.Messages) != 0 || wbatch.TimedOut {
				t.Fatalf("Wait(%q) refused but returned %+v", s.id, wbatch)
			}
			if wbatch.Cursor != after {
				t.Fatalf("Wait(%q) moved the cursor from %d to %d", s.id, after, wbatch.Cursor)
			}
			if got := h.WaiterCount(); got != 0 {
				t.Fatalf("a refused Wait(%q) left %d waiters registered", s.id, got)
			}
		})
		checked++
	}
	if checked != len(strangers) {
		t.Fatalf("checked %d unknown agents, want %d", checked, len(strangers))
	}
}
