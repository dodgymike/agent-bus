package hub_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ackEntriesIn counts the delivery lifecycle records in a log, read-only.
func ackEntriesIn(t *testing.T, dir string) []wal.Committed {
	t.Helper()
	var out []wal.Committed
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		if c.Entry.Kind == ack.RecordKind {
			out = append(out, c)
		}
		return nil
	}); err != nil {
		t.Fatalf("replaying %s: %v", dir, err)
	}
	return out
}

// TestNilAckRecorderIsTheOldBehaviourExactly is the claim hub.AckRecorder's doc
// comment makes, asserted rather than assumed: nil means no lifecycle table is
// wired and the bus behaves EXACTLY as it did before the seam existed.
//
// It matters because the seam sits on the send path, inside the global write
// lock, between the message's commit and the wake-up of local waiters. "Nil is a
// no-op" is the property that lets every other test in this package — and every
// embedder — keep passing unchanged, and it is one line away from being false.
func TestNilAckRecorderIsTheOldBehaviourExactly(t *testing.T) {
	h, _, dir := newTestHub(t, "alpha", "beta")
	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         agentID(t, testBusID, "alpha"),
		To:             agentID(t, testBusID, "beta"),
		Body:           []byte("no recorder is wired"),
		IdempotencyKey: "k-nil-acks",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID == "" {
		t.Fatal("Send returned no message id")
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 0 {
		t.Fatalf("a hub built with Acks nil wrote %d %q records; nil must be byte-for-byte the behaviour before the seam existed", len(rows), ack.RecordKind)
	}
}

// TestWiredAckRecorderWritesOneRowPerSend is the same assertion from the other
// side, on the ordinary (non-crash) path, so the seam is covered on every
// platform rather than only where the SIGKILL suite builds.
//
// It also pins the ORDER the whole design rests on: the message's own commit
// reaches the log FIRST. `accepted` asserts that this bus has committed and
// fsynced the message, so a row that landed first would be claiming a durability
// the message did not yet have.
func TestWiredAckRecorderWritesOneRowPerSend(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	acks := ack.NewStore(ack.Options{})
	if err := acks.Attach(lg); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	h := openAckHub(t, lg, lg, acks, "alpha", "beta")

	sender := agentID(t, testBusID, "alpha")
	recipient := agentID(t, testBusID, "beta")
	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           []byte("a recorder is wired"),
		IdempotencyKey: "k-wired-acks",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	rows := ackEntriesIn(t, dir)
	if len(rows) != 1 {
		t.Fatalf("one send wrote %d lifecycle records, want exactly 1 (one per recipient, and this send named one)", len(rows))
	}
	r, err := ack.DecodeRecord(rows[0].Entry.Body)
	if err != nil {
		t.Fatalf("decoding the row: %v", err)
	}
	if r.State != ack.StateAccepted {
		t.Errorf("the row is %s, want accepted", r.State)
	}
	if r.CorrelationKey != res.MessageID {
		t.Errorf("the row's correlation key is %q, want the message id %q: on the ORIGIN bus, store.Message.OriginID() IS the message's own id", r.CorrelationKey, res.MessageID)
	}
	if r.Sender != sender || r.Recipient != recipient {
		t.Errorf("the row names %s -> %s, want %s -> %s", r.Sender, r.Recipient, sender, recipient)
	}

	// The serving table the hub wrote through agrees with the log.
	got, ok := acks.Lookup(res.MessageID, recipient)
	if !ok || got.State != ack.StateAccepted {
		t.Fatalf("the live table holds (%+v, %v), want an accepted row", got, ok)
	}

	// A RETRY of the same key writes no second row: publish answers it from the
	// applied-key table and never reaches the seam at all.
	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             recipient,
		Body:           []byte("a recorder is wired"),
		IdempotencyKey: "k-wired-acks",
	}); err != nil {
		t.Fatalf("the retry returned %v, want the original result", err)
	}
	if rows := ackEntriesIn(t, dir); len(rows) != 1 {
		t.Fatalf("a retry brought the total to %d lifecycle records, want 1", len(rows))
	}
}

// openAckHub is openHubOverDurable plus the lifecycle seam, which that helper
// deliberately does not take: a hub with no AckRecorder must stay the default
// everywhere else, so wiring one is opt-in per test.
func openAckHub(t *testing.T, durable hub.DurableLog, lg *wal.Log, acks hub.AckRecorder, agents ...string) *hub.Hub {
	t.Helper()
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID:     testBusID,
		DataDir:   filepath.Dir(path),
		Durable:   durable,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
		Acks:      acks,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, agents...)
	return h
}

// stubRecorder is an AckRecorder that does exactly what a test tells it to.
type stubRecorder struct {
	err    error
	panics bool
	calls  int
}

func (s *stubRecorder) Accept(correlationKey, sender, recipient string) error {
	s.calls++
	if s.panics {
		panic("the recorder exploded")
	}
	return s.err
}

// openHubWithRecorderAndLog is openAckHub plus a capturing logger, so a test can
// assert on what the send path DID and DID NOT write to the operator log.
func openHubWithRecorderAndLog(t *testing.T, acks hub.AckRecorder, out *bytes.Buffer, agents ...string) *hub.Hub {
	t.Helper()
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID:     testBusID,
		DataDir:   filepath.Dir(path),
		Durable:   lg,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
		Logger:    logging.New(out, logging.LevelDebug),
		Acks:      acks,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, agents...)
	return h
}

func sendOne(t *testing.T, h *hub.Hub, key string) error {
	t.Helper()
	_, err := mintedSend(t, h, hub.SendRequest{
		Sender:         agentID(t, testBusID, "alpha"),
		To:             agentID(t, testBusID, "beta"),
		Body:           []byte("body"),
		IdempotencyKey: key,
	})
	return err
}

// TestCapacityRefusalIsNotLoggedTwiceOnTheSendPath is a REGRESSION GUARD for a
// finding, not a behaviour anyone would think to test.
//
// internal/ack throttles its "table is full" ERROR to one line per minute
// precisely because a full table refuses on EVERY send and an unthrottled line
// would emit thousands per second. A second, unthrottled copy on the hub's send
// path defeats that throttle completely — and turns an OBSERVABILITY table into
// a DISK outage, which is the exact failure ACK-CONTRACT.md §11.3 exists to
// prevent. The hub must therefore stay silent for the two errors the recorder
// already logs and counts, and MUST still shout for anything else.
func TestCapacityRefusalIsNotLoggedTwiceOnTheSendPath(t *testing.T) {
	const marker = "NO SENDER-VISIBLE DELIVERY STATUS"

	for i, tc := range []struct {
		name    string
		key     string
		err     error
		wantLog bool
	}{
		{"ErrCapacity is the recorder's to log", "k-cap", ack.ErrCapacity, false},
		{"ErrAgentQuota is the recorder's to log", "k-quota", ack.ErrAgentQuota, false},
		{"an unexpected error must still shout", "k-other", errors.New("the disk is on fire"), true},
	} {
		i, tc := i, tc
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			rec := &stubRecorder{err: tc.err}
			h := openHubWithRecorderAndLog(t, rec, &out, "alpha", "beta")
			// The key must match the hub's idempotency-key alphabet
			// ([A-Za-z0-9._-]), so it is built from the case's own key rather
			// than from its prose name.
			if err := sendOne(t, h, fmt.Sprintf("%s-%d", tc.key, i)); err != nil {
				t.Fatalf("Send returned %v; a recorder refusal must NEVER fail a send (§11.3)", err)
			}
			if rec.calls != 1 {
				t.Fatalf("the recorder was called %d times, want 1", rec.calls)
			}
			if got := strings.Contains(out.String(), marker); got != tc.wantLog {
				t.Fatalf("the send path logged the refusal = %v, want %v\n--- log ---\n%s", got, tc.wantLog, out.String())
			}
		})
	}
}

// TestRecorderPanicCannotTakeTheSendDown is the other missing guard.
//
// recordAcceptance runs inside publish with the GLOBAL WRITE LOCK held, on a
// message that is already committed and already in the serving copy. An
// unrecovered panic there would unwind through publish and kill the process for
// a send that demonstrably succeeded — so it is recovered, PER RECIPIENT, and
// reported as a lost observation.
func TestRecorderPanicCannotTakeTheSendDown(t *testing.T) {
	var out bytes.Buffer
	rec := &stubRecorder{panics: true}
	h := openHubWithRecorderAndLog(t, rec, &out, "alpha", "beta")

	if err := sendOne(t, h, "k-panic"); err != nil {
		t.Fatalf("Send returned %v; a panicking recorder must not fail a send that is already durable", err)
	}
	if !strings.Contains(out.String(), "PANIC in the delivery lifecycle seam") {
		t.Fatalf("the recovered panic was not reported; a silent loss of status is the defect invariant 6 names\n--- log ---\n%s", out.String())
	}
	// And the bus still works afterwards: the recovery must not have left the
	// write lock or any hub state wedged.
	if err := sendOne(t, h, "k-panic-2"); err != nil {
		t.Fatalf("the NEXT send returned %v; the recovery left the hub unusable", err)
	}
}
