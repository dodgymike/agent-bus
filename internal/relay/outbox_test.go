package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// RELAY-15's acceptance evidence: the outbox record, its monotonicity, its two
// bounds (capacity and age), and a REAL kill -9 proving that both halves of the
// lifecycle — "we owe this peer a message" and "we no longer do" — are WAL
// recovered state and not an in-memory cache.
//
// Every helper here is prefixed ob* so it cannot collide with the fixtures the
// rest of this package's tests own.

const (
	// obLocalBus is this bus; obPeerBus is the destination. They are distinct
	// because a job addressed to ourselves is a loop Enqueue must refuse.
	obLocalBus  = "bus-outbox-local"
	obPeerBus   = "bus-outbox-peer"
	obOriginBus = "bus-outbox-origin"

	// obHash is a well-formed lowercase hex SHA-256. Its VALUE is irrelevant;
	// its SHAPE is what the record validates.
	obHash      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	obOtherHash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// obMessageID builds a well-formed origin message id through the same minter
// the rest of the bus uses, so a change to the id grammar breaks this fixture
// loudly instead of leaving it asserting about a shape nothing produces.
func obMessageID(t testing.TB, seq uint64) string {
	t.Helper()
	id, err := ids.MessageID(obOriginBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%s, %d): %v", obOriginBus, seq, err)
	}
	return id
}

// obJob is the routing fixture for one delivery.
func obJob(t testing.TB, seq uint64) OutboxJob {
	t.Helper()
	return OutboxJob{
		PeerBusID:       obPeerBus,
		OriginMessageID: obMessageID(t, seq),
		Size:            11,
		ContentSHA256:   obHash,
	}
}

// obLogSink captures log output so a test can assert that a DISCARD WAS LOUD.
// Invariant 6 makes the silent discard the defect, so "the record went away" is
// only half an assertion — the other half is that an operator was told.
type obLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *obLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *obLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *obLogSink) mustContain(t *testing.T, ctx string, needles ...string) {
	t.Helper()
	got := s.String()
	for _, n := range needles {
		if !strings.Contains(got, n) {
			t.Errorf("%s: the log does not mention %q; a discard nobody is told about is the defect invariant 6 names, not the discard itself.\n--- log ---\n%s", ctx, n, got)
		}
	}
}

// mustLogAtLevel asserts a message was emitted AT A GIVEN LEVEL.
//
// IT EXISTS BECAUSE mustContain COULD NOT TELL. Invariant 6's requirement is that
// a discard be LOUD, and a substring check is satisfied by the same text logged
// at Debug — which is below the default level, so in a real deployment nobody
// would ever see it. Downgrading the merge branch's Error to Debug left the whole
// package green until this existed.
func (s *obLogSink) mustLogAtLevel(t *testing.T, ctx, level, needle string) {
	t.Helper()
	for _, line := range strings.Split(s.String(), "\n") {
		if strings.Contains(line, needle) {
			if !strings.Contains(line, "level="+level) {
				t.Errorf("%s: %q was logged, but not at level=%s:\n  %s", ctx, needle, level, line)
			}
			return
		}
	}
	t.Errorf("%s: nothing in the log mentions %q at all.\n--- log ---\n%s", ctx, needle, s.String())
}

// obClock is a hand-wound clock, so the age horizon can be crossed without a
// test sleeping for 24 hours.
type obClock struct {
	mu sync.Mutex
	t  time.Time
}

func newOBClock() *obClock {
	return &obClock{t: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *obClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *obClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// obNullDurable is a durable log that records what it was handed and nothing
// else. It is for the table-driven tests that are about the RECORD and the
// TABLE, where a real WAL would only slow them down; every test that makes a
// durability CLAIM uses a real *wal.Log (see obOpen and the crash tests).
type obNullDurable struct {
	mu      sync.Mutex
	entries []wal.Entry
	err     error
}

// obBlockingDurable holds writes after announcing that they reached the
// durable boundary. Tests use it to inspect capacity reservations while the
// writes are still absent from the in-memory table.
type obBlockingDurable struct {
	entered chan struct{}
	release chan struct{}
	count   atomic.Uint64
	once    sync.Once
}

func (d *obBlockingDurable) unblock() { d.once.Do(func() { close(d.release) }) }

func newOBBlockingDurable(writes int) *obBlockingDurable {
	return &obBlockingDurable{
		entered: make(chan struct{}, writes),
		release: make(chan struct{}),
	}
}

func (d *obBlockingDurable) Write(e wal.Entry) (wal.Committed, error) {
	d.entered <- struct{}{}
	<-d.release
	idx := d.count.Add(2)
	return wal.Committed{PrepareIndex: idx - 1, CommitIndex: idx, Entry: e}, nil
}

func (d *obNullDurable) Write(e wal.Entry) (wal.Committed, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return wal.Committed{}, d.err
	}
	d.entries = append(d.entries, e)
	return wal.Committed{PrepareIndex: uint64(len(d.entries)), CommitIndex: uint64(len(d.entries)) + 1, Entry: e}, nil
}

func (d *obNullDurable) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// obNewOutbox builds an outbox over a null durable log with a hand-wound clock.
func obNewOutbox(t *testing.T, tune func(*OutboxOptions)) (*Outbox, *obNullDurable, *obClock, *obLogSink) {
	t.Helper()
	d := &obNullDurable{}
	clk := newOBClock()
	sink := &obLogSink{}
	o := OutboxOptions{
		BusID:   obLocalBus,
		Durable: d,
		Logger:  logging.New(sink, logging.LevelDebug),
		Now:     clk.Now,
	}
	if tune != nil {
		tune(&o)
	}
	ob, err := NewOutbox(o)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	return ob, d, clk, sink
}

// obOpen opens a REAL wal.Log in dir with a fresh Outbox wired as its Applier,
// so recovery runs exactly the code path the server would run at startup.
func obOpen(t *testing.T, dir string, tune func(*OutboxOptions)) (*Outbox, *wal.Log, *obLogSink) {
	t.Helper()
	sink := &obLogSink{}
	o := OutboxOptions{
		BusID:  obLocalBus,
		Logger: logging.New(sink, logging.LevelDebug),
	}
	if tune != nil {
		tune(&o)
	}
	ob, err := NewOutbox(o)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ob})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	ob.durable = lg
	return ob, lg, sink
}

// obReplayCommitted reads the committed history straight off a log file,
// read-only, without opening a writer on it.
func obReplayCommitted(t *testing.T, dir string) []wal.Committed {
	t.Helper()
	var got []wal.Committed
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		got = append(got, c)
		return nil
	}); err != nil {
		t.Fatalf("replaying the log in %s: %v", dir, err)
	}
	return got
}

// obRecordsIn decodes every outbox record in a committed history.
func obRecordsIn(t *testing.T, committed []wal.Committed) []OutboxRecord {
	t.Helper()
	out := make([]OutboxRecord, 0, len(committed))
	for i, c := range committed {
		if c.Entry.Kind != OutboxRecordKind {
			continue
		}
		r, err := DecodeOutboxRecord(c.Entry.Body)
		if err != nil {
			t.Fatalf("committed entry %d does not decode as an outbox record: %v", i, err)
		}
		out = append(out, r)
	}
	return out
}

// obPendingIDs is the pending set as a comparable list.
func obPendingIDs(ob *Outbox) []string {
	pending := ob.Pending()
	out := make([]string, 0, len(pending))
	for _, r := range pending {
		out = append(out, r.JobID)
	}
	return out
}

// ---------------------------------------------------------------------------
// The bounds are BOUND BY REFERENCE, not copied
// ---------------------------------------------------------------------------

// TestOutboxHorizonIsBoundToThePeerOutageBudget pins the constraint RELAY-15
// inherits: the total retry horizon stays inside idem.PeerOutageBudget.
//
// The assertion is IDENTITY, not a value comparison against 24h alone, because
// the failure this guards against is DRIFT: a literal copied here would keep
// passing a "== 24h" check while idem's derivation moved underneath it, and the
// outbox would then retain jobs past the window in which the receiving bus
// still remembers the applied key — where a retry is applied as a NEW message
// (invariant 10).
func TestOutboxHorizonIsBoundToThePeerOutageBudget(t *testing.T) {
	if OutboxRetryHorizon != RetryHorizonCeiling {
		t.Errorf("OutboxRetryHorizon = %s, want RetryHorizonCeiling (%s): forward.go already binds the ceiling to idem.PeerOutageBudget BY REFERENCE, and the outbox must ride the same constant rather than a second copy of it",
			OutboxRetryHorizon, RetryHorizonCeiling)
	}
	if OutboxRetryHorizon != idem.PeerOutageBudget {
		t.Errorf("OutboxRetryHorizon = %s, want idem.PeerOutageBudget (%s)", OutboxRetryHorizon, idem.PeerOutageBudget)
	}
	if OutboxSettledRetention != OutboxRetryHorizon {
		t.Errorf("OutboxSettledRetention = %s, want the retry horizon (%s): a settled tombstone only has to outlive the longest pending record that could still turn up, and a pending record past the horizon is dropped by the same sweep",
			OutboxSettledRetention, OutboxRetryHorizon)
	}

	// And the source must not carry a hand-copied duration literal, which is
	// the exact mistake the binding exists to prevent. This scans only for a
	// spelled-out budget; the identity assertions above are what actually
	// enforce the binding.
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "outbox.go"))
	if err != nil {
		t.Fatalf("reading outbox.go: %v", err)
	}
	for _, banned := range []string{"24 * time.Hour", "24*time.Hour"} {
		if bytes.Contains(src, []byte(banned)) {
			t.Errorf("outbox.go contains the literal %q; the horizon is idem.PeerOutageBudget BY REFERENCE (through RetryHorizonCeiling), never a duplicated value", banned)
		}
	}
}

// TestOutboxCapacityIsDerivedFromTheQueuesItMakesDurable pins the cap to the
// two constants it is derived from, so it cannot quietly become a guess.
func TestOutboxCapacityIsDerivedFromTheQueuesItMakesDurable(t *testing.T) {
	if MaxOutboxJobs != MaxPeers*DefaultQueueDepth {
		t.Errorf("MaxOutboxJobs = %d, want MaxPeers*DefaultQueueDepth (%d*%d): the durable outbox undertakes to remember exactly what the in-memory per-peer queues could hold, and no more",
			MaxOutboxJobs, MaxPeers, DefaultQueueDepth)
	}
	if MaxOutboxJobIDLen != MaxPeerBusIDLen+1+ids.MaxMessageIDLen {
		t.Errorf("MaxOutboxJobIDLen = %d, want %d: the bound must be derived from the two halves DeriveJobID concatenates",
			MaxOutboxJobIDLen, MaxPeerBusIDLen+1+ids.MaxMessageIDLen)
	}
	ob, _, _, _ := obNewOutbox(t, nil)
	if ob.maxPendingPerPeer != DefaultQueueDepth {
		t.Errorf("default per-peer pending limit = %d, want DefaultQueueDepth %d", ob.maxPendingPerPeer, DefaultQueueDepth)
	}
}

// ---------------------------------------------------------------------------
// The record
// ---------------------------------------------------------------------------

// TestOutboxRecordRoundTrip proves Encode/DecodeOutboxRecord is lossless and
// canonical for every state.
func TestOutboxRecordRoundTrip(t *testing.T) {
	enq := time.Date(2026, 8, 8, 9, 30, 0, 123456789, time.UTC)
	base := OutboxRecord{
		PeerBusID:       obPeerBus,
		OriginMessageID: obMessageID(t, 7),
		Size:            42,
		ContentSHA256:   obHash,
		EnqueuedAt:      enq,
	}
	base.JobID = DeriveJobID(base.PeerBusID, base.OriginMessageID)

	pending := base
	pending.State = OutboxPending

	delivered := base
	delivered.State = OutboxDelivered
	delivered.SettledAt = enq.Add(2 * time.Second)

	abandoned := base
	abandoned.State = OutboxAbandoned
	abandoned.SettledAt = enq.Add(3 * time.Second)
	abandoned.Reason = "peer refused with 403 unpeered-bus"

	for _, tc := range []struct {
		name string
		rec  OutboxRecord
	}{
		{"pending", pending},
		{"delivered", delivered},
		{"abandoned", abandoned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.rec.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := DecodeOutboxRecord(body)
			if err != nil {
				t.Fatalf("DecodeOutboxRecord(%s): %v", body, err)
			}
			if got.JobID != tc.rec.JobID || got.PeerBusID != tc.rec.PeerBusID ||
				got.OriginMessageID != tc.rec.OriginMessageID || got.Size != tc.rec.Size ||
				got.ContentSHA256 != tc.rec.ContentSHA256 || got.State != tc.rec.State ||
				got.Reason != tc.rec.Reason {
				t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, tc.rec)
			}
			if !got.EnqueuedAt.Equal(tc.rec.EnqueuedAt) || !got.SettledAt.Equal(tc.rec.SettledAt) {
				t.Fatalf("round trip changed a timestamp: enqueued %s/%s settled %s/%s",
					got.EnqueuedAt, tc.rec.EnqueuedAt, got.SettledAt, tc.rec.SettledAt)
			}
			// Encoding the decoded record must produce the SAME bytes, or a
			// live fold and a replayed fold could hold different values.
			again, err := got.Encode()
			if err != nil {
				t.Fatalf("re-Encode: %v", err)
			}
			if !bytes.Equal(body, again) {
				t.Fatalf("encoding is not canonical:\n first %s\nsecond %s", body, again)
			}
			if got.OriginBus() != obOriginBus {
				t.Fatalf("OriginBus() = %q, want %q: the origin is the bus half of the message id, derived and never stored twice", got.OriginBus(), obOriginBus)
			}
		})
	}
}

// obOptionalTime renders a timestamp the way the encoder does, or "" for the
// zero time, so a hand-marshalled record matches the real wire shape.
func obOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// TestOutboxRecordRejectsMalformed is the way-IN half of validate: a record off
// disk is untrusted input even though this server wrote it.
func TestOutboxRecordRejectsMalformed(t *testing.T) {
	msgID := obMessageID(t, 9)
	good := func() OutboxRecord {
		r := OutboxRecord{
			PeerBusID:       obPeerBus,
			OriginMessageID: msgID,
			Size:            5,
			ContentSHA256:   obHash,
			EnqueuedAt:      time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC),
			State:           OutboxPending,
		}
		r.JobID = DeriveJobID(r.PeerBusID, r.OriginMessageID)
		return r
	}

	for _, tc := range []struct {
		name string
		mut  func(*OutboxRecord)
		why  string
	}{
		{"job id does not derive from its own components", func(r *OutboxRecord) { r.JobID = "somebody-elses-job" },
			"a record that names one job and describes another is how a splice settles a job it does not own"},
		{"unknown peer bus id", func(r *OutboxRecord) { r.PeerBusID = "not a bus id!" }, "the destination must be a well-formed bus id"},
		{"oversized peer bus id", func(r *OutboxRecord) { r.PeerBusID = strings.Repeat("p", MaxPeerBusIDLen+1) }, "bounded before anything quotes it"},
		{"malformed origin message id", func(r *OutboxRecord) {
			r.OriginMessageID = "no-seq-here-"
			r.JobID = DeriveJobID(r.PeerBusID, r.OriginMessageID)
		}, "the message id is the job's other half"},
		{"negative size", func(r *OutboxRecord) { r.Size = -1 }, "a size is 0..MaxBodyBytes"},
		{"oversized size", func(r *OutboxRecord) { r.Size = store.MaxBodyBytes + 1 }, "a size is 0..MaxBodyBytes"},
		{"short content hash", func(r *OutboxRecord) { r.ContentSHA256 = "abc" }, "a SHA-256 is 64 hex characters"},
		{"uppercase content hash", func(r *OutboxRecord) { r.ContentSHA256 = strings.ToUpper(obHash) },
			"two spellings of one hash would make the same-content comparison miss"},
		{"zero enqueued_at", func(r *OutboxRecord) { r.EnqueuedAt = time.Time{} }, "the age horizon is computed from it"},
		{"pending carrying a settlement", func(r *OutboxRecord) { r.SettledAt = r.EnqueuedAt.Add(time.Second) },
			"a pending record carrying a settlement is the shape a resurrection wants"},
		{"pending carrying a reason", func(r *OutboxRecord) { r.Reason = "why?" }, "only an abandoned job has a reason"},
		{"delivered without settled_at", func(r *OutboxRecord) { r.State = OutboxDelivered }, "tombstone retention is computed from it"},
		{"delivered carrying a reason", func(r *OutboxRecord) {
			r.State = OutboxDelivered
			r.SettledAt = r.EnqueuedAt.Add(time.Second)
			r.Reason = "delivered, but why?"
		}, "a reason on a delivered job is the one place the two terminal states could be confused"},
		{"abandoned without a reason", func(r *OutboxRecord) {
			r.State = OutboxAbandoned
			r.SettledAt = r.EnqueuedAt.Add(time.Second)
		}, "invariant 6: the discard must be recorded specifically, never silently"},
		{"abandoned with an oversized reason", func(r *OutboxRecord) {
			r.State = OutboxAbandoned
			r.SettledAt = r.EnqueuedAt.Add(time.Second)
			r.Reason = strings.Repeat("x", MaxOutboxReasonLen+1)
		}, "a peer must not choose the size of our durable record"},
		{"settled before it was enqueued", func(r *OutboxRecord) {
			r.State = OutboxDelivered
			r.SettledAt = r.EnqueuedAt.Add(-time.Second)
		}, "a job cannot settle before it exists"},
		{"unknown state", func(r *OutboxRecord) { r.State = OutboxState(99) }, "the state enum is closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := good()
			tc.mut(&r)
			if _, err := r.Encode(); err == nil {
				t.Fatalf("Encode accepted %+v; %s", r, tc.why)
			}

			// THE SAME ROW THROUGH THE DECODER, which is the half that matters
			// for invariant 1: a record off disk is untrusted input even though
			// this server wrote it. Going through Encode alone left
			// DecodeOutboxRecord's own validate() call unexercised — replacing
			// it with `if false` kept the whole package green until this
			// existed. Hand-marshalled, because Encode refuses to produce these
			// bytes, which is exactly why a hostile or damaged file is the only
			// way they arrive.
			raw, err := json.Marshal(outboxRecordJSON{
				JobID:           r.JobID,
				PeerBusID:       r.PeerBusID,
				OriginMessageID: r.OriginMessageID,
				Size:            r.Size,
				ContentSHA256:   r.ContentSHA256,
				EnqueuedAt:      r.EnqueuedAt.UTC().Format(time.RFC3339Nano),
				State:           r.State.String(),
				SettledAt:       obOptionalTime(r.SettledAt),
				Reason:          r.Reason,
			})
			if err != nil {
				t.Fatalf("marshalling the hostile record: %v", err)
			}
			if _, err := DecodeOutboxRecord(raw); err == nil {
				t.Fatalf("DecodeOutboxRecord accepted %s; %s. A record read off disk is untrusted input (invariant 1), so the decoder must re-validate rather than trust that this server wrote it", raw, tc.why)
			}
		})
	}

	// The way-IN half specifically: strictness of the decoder itself.
	body, err := good().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"unknown field", strings.TrimSuffix(string(body), "}") + `,"surprise":1}`},
		{"trailing data", string(body) + string(body)},
		{"unknown state spelling", strings.Replace(string(body), `"state":"pending"`, `"state":"maybe"`, 1)},
		{"non-RFC3339 timestamp", strings.Replace(string(body), `"enqueued_at":"2026-08-08T09:00:00Z"`, `"enqueued_at":"yesterday"`, 1)},
	} {
		t.Run("decode/"+tc.name, func(t *testing.T) {
			if _, err := DecodeOutboxRecord([]byte(tc.raw)); err == nil {
				t.Fatalf("DecodeOutboxRecord accepted %s; a lenient decoder reinstates a DELIVERED job as a pending one, which is a second delivery", tc.raw)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The live path
// ---------------------------------------------------------------------------

// TestOutboxEnqueueIsIdempotentOnTheJobID proves the derived job id does the
// work it exists to do: enqueueing the same message for the same peer twice
// names one job and writes once.
func TestOutboxEnqueueIsIdempotentOnTheJobID(t *testing.T) {
	ob, d, clk, _ := obNewOutbox(t, nil)

	first, err := ob.Enqueue(obJob(t, 1))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	clk.Advance(time.Minute)
	second, err := ob.Enqueue(obJob(t, 1))
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if second.JobID != first.JobID {
		t.Fatalf("the second enqueue named job %s, want %s", second.JobID, first.JobID)
	}
	if !second.EnqueuedAt.Equal(first.EnqueuedAt) {
		t.Fatalf("the second enqueue moved enqueued_at from %s to %s; the age horizon must not be pushed out by re-queueing",
			first.EnqueuedAt, second.EnqueuedAt)
	}
	if d.count() != 1 {
		t.Fatalf("%d durable writes for one job, want 1: a repeated enqueue must write nothing", d.count())
	}
	if got := obPendingIDs(ob); len(got) != 1 || got[0] != first.JobID {
		t.Fatalf("pending set = %v, want exactly [%s]", got, first.JobID)
	}

	// A RE-ENQUEUE NAMING DIFFERENT CONTENT IS REFUSED, so the live path is not
	// laxer than the replay path (which refuses the same substitution). Handing
	// back the ORIGINAL job silently would leave the caller believing its NEW
	// content was queued when the message actually sent is the old one.
	swapped := obJob(t, 1)
	swapped.ContentSHA256 = obOtherHash
	if _, err := ob.Enqueue(swapped); !errors.Is(err, ErrInvalidOutboxRecord) {
		t.Fatalf("re-enqueueing job %s with different content gave err = %v, want ErrInvalidOutboxRecord", first.JobID, err)
	}
	bigger := obJob(t, 1)
	bigger.Size = first.Size + 1
	if _, err := ob.Enqueue(bigger); !errors.Is(err, ErrInvalidOutboxRecord) {
		t.Fatalf("re-enqueueing job %s with a different size gave err = %v, want ErrInvalidOutboxRecord", first.JobID, err)
	}
	if d.count() != 1 {
		t.Fatalf("%d durable writes after two refused re-enqueues, want 1", d.count())
	}
}

// TestOutboxRefusesAJobAddressedToThisBus: a durable job pointed at ourselves is
// a loop that would survive a restart.
func TestOutboxRefusesAJobAddressedToThisBus(t *testing.T) {
	ob, d, _, _ := obNewOutbox(t, nil)
	job := obJob(t, 2)
	job.PeerBusID = obLocalBus
	if _, err := ob.Enqueue(job); err == nil {
		t.Fatal("Enqueue accepted a job addressed to this bus; that is a loop with a durable record attached")
	}
	if d.count() != 0 {
		t.Fatalf("%d durable writes for a refused job, want 0", d.count())
	}
}

// TestOutboxSettleIsMonotonic walks the whole lifecycle on the live path.
func TestOutboxSettleIsMonotonic(t *testing.T) {
	ob, d, clk, _ := obNewOutbox(t, nil)

	del, err := ob.Enqueue(obJob(t, 10))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	aba, err := ob.Enqueue(obJob(t, 11))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	clk.Advance(time.Second)

	if _, err := ob.Settle(del.JobID, OutboxDelivered, ""); err != nil {
		t.Fatalf("Settle delivered: %v", err)
	}
	if _, err := ob.Settle(aba.JobID, OutboxAbandoned, "horizon exhausted"); err != nil {
		t.Fatalf("Settle abandoned: %v", err)
	}
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("pending set = %v after both jobs settled, want empty", got)
	}
	if d.count() != 4 {
		t.Fatalf("%d durable writes, want 4 (two enqueues, two settlements)", d.count())
	}

	// Repeating a settlement verbatim is idempotent and writes nothing.
	if _, err := ob.Settle(del.JobID, OutboxDelivered, ""); err != nil {
		t.Fatalf("repeating the delivered settlement gave err = %v, want nil", err)
	}
	if d.count() != 4 {
		t.Fatalf("%d durable writes after a repeated settlement, want 4", d.count())
	}

	// Contradicting one is refused, and the first stands.
	if _, err := ob.Settle(del.JobID, OutboxAbandoned, "changed my mind"); !errors.Is(err, ErrOutboxSettled) {
		t.Fatalf("contradicting a settlement gave err = %v, want ErrOutboxSettled", err)
	}
	if got, _ := ob.Lookup(del.JobID); got.State != OutboxDelivered {
		t.Fatalf("job %s is %s after a refused contradiction, want delivered", del.JobID, got.State)
	}

	// RE-ENQUEUEING A SETTLED JOB IS THE RESURRECTION, and it is refused.
	if _, err := ob.Enqueue(obJob(t, 10)); !errors.Is(err, ErrOutboxSettled) {
		t.Fatalf("re-enqueueing a delivered job gave err = %v, want ErrOutboxSettled: accepting it sends the message a second time", err)
	}

	// A settle needs a terminal state and an abandonment needs a reason.
	if _, err := ob.Settle(del.JobID, OutboxPending, ""); !errors.Is(err, ErrInvalidOutboxRecord) {
		t.Fatalf("Settle to pending gave err = %v, want ErrInvalidOutboxRecord", err)
	}
	j, err := ob.Enqueue(obJob(t, 12))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// An abandonment with NO reason must still SETTLE — a failure here would
	// leave the job pending and retried forever — but it must not be silent
	// either, so a placeholder is stored (invariant 6).
	noReason, err := ob.Settle(j.JobID, OutboxAbandoned, "")
	if err != nil {
		t.Fatalf("an abandonment with no reason gave err = %v; a settle that fails leaves the job PENDING and retried forever", err)
	}
	if noReason.Reason != OutboxReasonUnspecified {
		t.Fatalf("an abandonment with no reason stored %q, want the placeholder %q", noReason.Reason, OutboxReasonUnspecified)
	}
	if _, err := ob.Settle("no-such-job", OutboxDelivered, ""); !errors.Is(err, ErrOutboxUnknownJob) {
		t.Fatalf("settling an unknown job gave err = %v, want ErrOutboxUnknownJob", err)
	}
}

// TestOutboxRefusesToWorkWithoutADurableLog: an outbox that forgets on restart
// is the thing this file replaces, so there is no in-memory mode.
func TestOutboxRefusesToWorkWithoutADurableLog(t *testing.T) {
	ob, err := NewOutbox(OutboxOptions{BusID: obLocalBus})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	if _, err := ob.Enqueue(obJob(t, 1)); !errors.Is(err, ErrOutboxNotDurable) {
		t.Fatalf("Enqueue without a durable log gave err = %v, want ErrOutboxNotDurable", err)
	}
	if _, err := ob.Settle("any", OutboxDelivered, ""); !errors.Is(err, ErrOutboxNotDurable) {
		t.Fatalf("Settle without a durable log gave err = %v, want ErrOutboxNotDurable", err)
	}
	if _, err := NewOutbox(OutboxOptions{BusID: "not a bus id!"}); err == nil {
		t.Fatal("NewOutbox accepted an invalid local bus id; without it no job can be checked for self-addressing")
	}
}

// ---------------------------------------------------------------------------
// REPLAY: the pending set, the bounds, and the ordering argument
// ---------------------------------------------------------------------------

// TestOutboxReplayRebuildsPendingSet is RELAY-15's headline claim: what this bus
// still owes its peers survives a restart, and what it no longer owes does not
// come back.
//
// It goes through a REAL wal.Log — write, close, reopen with a fresh Outbox as
// the Applier — because recovery is the code path being claimed, and a test
// that only replayed records through Apply by hand would not exercise it.
func TestOutboxReplayRebuildsPendingSet(t *testing.T) {
	dir := t.TempDir()

	ob, lg, _ := obOpen(t, dir, nil)
	owed1, err := ob.Enqueue(obJob(t, 100))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	owed2, err := ob.Enqueue(obJob(t, 101))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	done, err := ob.Enqueue(obJob(t, 102))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	gone, err := ob.Enqueue(obJob(t, 103))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := ob.Settle(done.JobID, OutboxDelivered, ""); err != nil {
		t.Fatalf("Settle delivered: %v", err)
	}
	if _, err := ob.Settle(gone.JobID, OutboxAbandoned, "peer gone"); err != nil {
		t.Fatalf("Settle abandoned: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// --- the restart -------------------------------------------------------
	ob2, lg2, _ := obOpen(t, dir, nil)
	defer func() { _ = lg2.Close() }()

	if got := lg2.Recovered().Applied; got != 6 {
		t.Fatalf("recovery applied %d committed entries, want 6 (four enqueues, two settlements)", got)
	}
	want := []string{owed1.JobID, owed2.JobID}
	got := obPendingIDs(ob2)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the rebuilt pending set is %v, want %v (oldest enqueue first): a restart must re-offer exactly what is still owed, in the order it would have sent it", got, want)
	}
	for _, id := range want {
		r, ok := ob2.Lookup(id)
		if !ok || r.State != OutboxPending {
			t.Fatalf("job %s recovered as (%+v, %v), want a pending record", id, r, ok)
		}
		if r.ContentSHA256 != obHash || r.Size != 11 {
			t.Fatalf("job %s recovered with content %s (%d bytes); the routing facts must survive intact or RELAY-19 cannot check the message it rebuilds against", id, r.ContentSHA256, r.Size)
		}
	}
	if r, ok := ob2.Lookup(done.JobID); !ok || r.State != OutboxDelivered {
		t.Fatalf("the delivered job recovered as (%+v, %v), want delivered: if it comes back pending the message is sent twice", r, ok)
	}
	if r, ok := ob2.Lookup(gone.JobID); !ok || r.State != OutboxAbandoned || r.Reason != "peer gone" {
		t.Fatalf("the abandoned job recovered as (%+v, %v), want abandoned with its reason", r, ok)
	}

	// A settled job stays settled across the restart: re-enqueueing it is still
	// the refused resurrection.
	if _, err := ob2.Enqueue(obJob(t, 102)); !errors.Is(err, ErrOutboxSettled) {
		t.Fatalf("re-enqueueing a job the recovered log says was delivered gave err = %v, want ErrOutboxSettled", err)
	}

	// Recovery is a FIXED POINT, not a one-off.
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the recovered log: %v", err)
	}
	ob3, lg3, _ := obOpen(t, dir, nil)
	defer func() { _ = lg3.Close() }()
	if got := obPendingIDs(ob3); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("after a second restart the pending set is %v, want %v", got, want)
	}
}

// TestOutboxRecordAndReplay is the task's recorded proof command. It asserts the
// two things the record has to get right on disk — the ENTRY KIND is the
// free-form "outbox" discriminator, and the two records for one job share a job
// id and carry a monotonic state — and then that replay rebuilds from exactly
// those bytes.
func TestOutboxRecordAndReplay(t *testing.T) {
	dir := t.TempDir()

	ob, lg, _ := obOpen(t, dir, nil)
	job, err := ob.Enqueue(obJob(t, 200))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := ob.Settle(job.JobID, OutboxDelivered, ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	committed := obReplayCommitted(t, dir)
	if len(committed) != 2 {
		t.Fatalf("the log holds %d committed entries, want 2", len(committed))
	}
	for i, c := range committed {
		if c.Entry.Kind != OutboxRecordKind {
			t.Fatalf("committed entry %d has kind %q, want %q", i, c.Entry.Kind, OutboxRecordKind)
		}
		if c.Entry.Kind != "outbox" {
			t.Fatalf("OutboxRecordKind is %q, want \"outbox\"; this is the on-disk discriminator, so changing it orphans every record already written. RELAY-15 did not own CONTRACTS-ONDISK.md, so the row documenting it is reported for a follow-up documentation pass rather than written by this task", c.Entry.Kind)
		}
	}
	recs := obRecordsIn(t, committed)
	if recs[0].JobID != recs[1].JobID {
		t.Fatalf("the two records name different jobs (%s and %s); a lifecycle is two records SHARING a job id", recs[0].JobID, recs[1].JobID)
	}
	if recs[0].State != OutboxPending || recs[1].State != OutboxDelivered {
		t.Fatalf("the durable states are %s then %s, want pending then delivered", recs[0].State, recs[1].State)
	}
	if !recs[1].EnqueuedAt.Equal(recs[0].EnqueuedAt) {
		t.Fatalf("the settlement record carries enqueued_at %s, want the pending record's %s: every durable entry carries the COMPLETE record, never a delta",
			recs[1].EnqueuedAt, recs[0].EnqueuedAt)
	}

	// The body is NOT in the log. The outbox holds routing information only —
	// the message body is already durable exactly once in this bus's own
	// message record, and a second copy per peer would be both waste and a
	// second truth.
	//
	// Asserted on the RECORD's own bytes, not on the whole file: wal's prepare
	// payload has a field of its own called "body" (it is the carrier for this
	// record), so scanning the file would match the framing and pass no matter
	// what the record contained.
	for i, c := range committed {
		if bytes.Contains(c.Entry.Body, []byte(`"body"`)) {
			t.Fatalf("outbox record %d carries a body field (%s); the outbox stores routing information only, and the message body is already durable exactly once in this bus's own message record", i, c.Entry.Body)
		}
	}

	// And replay rebuilds it.
	ob2, lg2, _ := obOpen(t, dir, nil)
	defer func() { _ = lg2.Close() }()
	if got := obPendingIDs(ob2); len(got) != 0 {
		t.Fatalf("the rebuilt pending set is %v, want empty: the job was delivered before the restart", got)
	}
	if r, ok := ob2.Lookup(job.JobID); !ok || r.State != OutboxDelivered {
		t.Fatalf("job %s rebuilt as (%+v, %v), want delivered", job.JobID, r, ok)
	}
}

// TestOutboxReplayIsOrderIndependent is the argument for why monotonicity keyed
// on the STATE RANK — and not on a sequence number or a timestamp — makes replay
// safe.
//
// Applying the same MULTISET of records in ANY order must converge on the same
// answer. The case that matters is a stale PENDING record arriving after the
// DELIVERED one: accepting it would resurrect a delivered message and turn
// at-least-once delivery into at-least-twice.
func TestOutboxReplayIsOrderIndependent(t *testing.T) {
	enq := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	msgID := obMessageID(t, 300)
	jobID := DeriveJobID(obPeerBus, msgID)

	pending := OutboxRecord{
		JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: msgID,
		Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: OutboxPending,
	}
	delivered := pending
	delivered.State = OutboxDelivered
	delivered.SettledAt = enq.Add(time.Second)

	commit := func(t *testing.T, r OutboxRecord, idx uint64) wal.Committed {
		t.Helper()
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return wal.Committed{PrepareIndex: idx, CommitIndex: idx + 1, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}
	}

	for _, tc := range []struct {
		name  string
		order []OutboxRecord
	}{
		{"in order", []OutboxRecord{pending, delivered}},
		{"reversed — the stale pending arrives last", []OutboxRecord{delivered, pending}},
		{"duplicated", []OutboxRecord{pending, pending, delivered, delivered, pending}},
		{"settlement first, then everything twice", []OutboxRecord{delivered, pending, delivered, pending}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ob, _, clk, sink := obNewOutbox(t, nil)
			clk.Advance(2 * time.Second)
			for i, r := range tc.order {
				if err := ob.Apply(commit(t, r, uint64(i*2+1))); err != nil {
					t.Fatalf("Apply must never return an error (it poisons the log on a live write and fails Open on recovery); got %v", err)
				}
			}
			got, ok := ob.Lookup(jobID)
			if !ok {
				t.Fatalf("job %s is not in the table after replay", jobID)
			}
			if got.State != OutboxDelivered {
				t.Fatalf("job %s converged on %s, want delivered: terminal is ABSORBING, so no interleaving of these records may leave the job pending — a pending job here is a message delivered twice",
					jobID, got.State)
			}
			if p := obPendingIDs(ob); len(p) != 0 {
				t.Fatalf("the pending set is %v after a delivered job was replayed, want empty", p)
			}
			// The refusal is never silent.
			if strings.Contains(tc.name, "reversed") || strings.Contains(tc.name, "first") {
				sink.mustContain(t, "a refused resurrection", "DISCARDING an outbox record", jobID)
			}
		})
	}
}

// TestOutboxReplayRefusesAContentSubstitution: one message id, two bodies. A
// message id is minted by the origin bus and never reused (invariant 1), so a
// second content hash under one id is corruption or substitution, and the first
// content wins.
func TestOutboxReplayRefusesAContentSubstitution(t *testing.T) {
	ob, _, _, sink := obNewOutbox(t, nil)
	msgID := obMessageID(t, 400)
	jobID := DeriveJobID(obPeerBus, msgID)
	enq := newOBClock().Now()

	base := OutboxRecord{
		JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: msgID,
		Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: OutboxPending,
	}
	swapped := base
	swapped.ContentSHA256 = obOtherHash

	apply := func(r OutboxRecord, idx uint64) {
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if err := ob.Apply(wal.Committed{PrepareIndex: idx, CommitIndex: idx + 1, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply returned %v", err)
		}
	}
	apply(base, 1)
	apply(swapped, 3)

	got, ok := ob.Lookup(jobID)
	if !ok || got.ContentSHA256 != obHash {
		t.Fatalf("job %s holds content %q, want the FIRST content %q", jobID, got.ContentSHA256, obHash)
	}
	sink.mustContain(t, "a refused content substitution", "DISCARDING an outbox record", jobID)
}

// TestOutboxApplySkipsForeignKinds: this log carries messages, roster entries,
// invites and outbox jobs. An applier that treated its neighbours' records as
// damage would fill the log with false alarms.
func TestOutboxApplySkipsForeignKinds(t *testing.T) {
	ob, _, _, sink := obNewOutbox(t, nil)
	if err := ob.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: wal.Entry{Kind: "message", Body: []byte(`{"anything":true}`)}}); err != nil {
		t.Fatalf("Apply of a foreign kind returned %v, want nil", err)
	}
	if ob.Len() != 0 {
		t.Fatalf("a foreign record entered the outbox table")
	}
	if s := sink.String(); s != "" {
		t.Fatalf("a foreign record produced log output:\n%s", s)
	}

	// An outbox record that does not DECODE is a different matter: it is
	// discarded and said so, loudly.
	if err := ob.Apply(wal.Committed{PrepareIndex: 3, CommitIndex: 4, Entry: wal.Entry{Kind: OutboxRecordKind, Body: []byte(`{"job_id":`)}}); err != nil {
		t.Fatalf("Apply of an undecodable outbox record returned %v, want nil (a non-nil error poisons the log)", err)
	}
	sink.mustContain(t, "an undecodable record", "DISCARDING an outbox record that could not be decoded", "relay hop is LOST")
}

// ---------------------------------------------------------------------------
// The two bounds, ON THE REPLAY PATH
// ---------------------------------------------------------------------------

// TestOutboxCapacityIsEnforcedOnTheReplayPath is the bound that matters most,
// and the one that is easiest to get wrong: a cap applied only when a job is
// ENQUEUED is not a cap, because a log written by a build with a larger cap — or
// simply a log longer than the current cap — replays straight past it.
func TestOutboxCapacityIsEnforcedOnTheReplayPath(t *testing.T) {
	const maxJobs = 2
	ob, _, _, sink := obNewOutbox(t, func(o *OutboxOptions) {
		o.MaxJobs = maxJobs
		o.MaxPendingPerPeer = maxJobs // isolate the global replay bound in this test
	})
	enq := newOBClock().Now()

	var overflow string
	for i := uint64(0); i < maxJobs+3; i++ {
		msgID := obMessageID(t, 500+i)
		r := OutboxRecord{
			JobID: DeriveJobID(obPeerBus, msgID), PeerBusID: obPeerBus, OriginMessageID: msgID,
			Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: OutboxPending,
		}
		if i == maxJobs {
			overflow = r.JobID
		}
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if err := ob.Apply(wal.Committed{PrepareIndex: i*2 + 1, CommitIndex: i*2 + 2, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply returned %v, want nil: a non-nil error from recovery makes Open fail, and invariant 6 settled that recovery ALWAYS reaches a running server", err)
		}
	}
	if got := ob.Len(); got != maxJobs {
		t.Fatalf("the outbox holds %d records after replaying %d, want the cap of %d: a bound one path can exceed is not a bound", got, maxJobs+3, maxJobs)
	}
	if _, ok := ob.Lookup(overflow); ok {
		t.Fatalf("job %s got in past the cap", overflow)
	}
	sink.mustContain(t, "a capacity discard on replay", "DISCARDING an outbox record", "at capacity")

	// The live path is bounded by the same number, and refuses rather than
	// silently overwriting.
	if _, err := ob.Enqueue(obJob(t, 999)); !errors.Is(err, ErrOutboxCapacity) {
		t.Fatalf("Enqueue at capacity gave err = %v, want ErrOutboxCapacity", err)
	}
}

// TestOutboxCapacityCountsPendingWorkNotTombstones is the regression for the
// 24-hour throughput ceiling. A settlement releases pending capacity, while its
// tombstone remains retained for the full anti-resurrection window.
func TestOutboxCapacityCountsPendingWorkNotTombstones(t *testing.T) {
	const maxJobs = 4
	const cycles = 3 * maxJobs
	ob, d, _, _ := obNewOutbox(t, func(o *OutboxOptions) {
		o.MaxJobs = maxJobs
		o.MaxPendingPerPeer = maxJobs
	})

	settled := make([]string, 0, cycles)
	for i := 0; i < cycles; i++ {
		r, err := ob.Enqueue(obJob(t, uint64(700+i)))
		if err != nil {
			t.Fatalf("cycle %d Enqueue: %v; settled tombstones must not turn a pending-work bound into a throughput-per-retention-window ceiling", i, err)
		}
		if _, err := ob.Settle(r.JobID, OutboxDelivered, ""); err != nil {
			t.Fatalf("cycle %d Settle: %v", i, err)
		}
		settled = append(settled, r.JobID)
	}
	if got := ob.Len(); got != cycles {
		t.Fatalf("retained records = %d, want %d tombstones; releasing pending capacity must not retire idempotency evidence early", got, cycles)
	}
	for _, jobID := range settled {
		if r, ok := ob.Lookup(jobID); !ok || !r.State.Terminal() {
			t.Fatalf("settled job %s lost its tombstone: %+v, %v", jobID, r, ok)
		}
	}
	if got := d.count(); got != 2*cycles {
		t.Fatalf("durable writes = %d, want %d enqueue+settle records", got, 2*cycles)
	}

	for i := 0; i < maxJobs; i++ {
		if _, err := ob.Enqueue(obJob(t, uint64(800+i))); err != nil {
			t.Fatalf("pending enqueue %d: %v", i, err)
		}
	}
	if _, err := ob.Enqueue(obJob(t, 899)); !errors.Is(err, ErrOutboxCapacity) {
		t.Fatalf("enqueue beyond %d simultaneously pending jobs gave %v, want ErrOutboxCapacity", maxJobs, err)
	}
}

// TestOutboxCapacityIsFairPerPeer proves that one dead or hostile peer cannot
// consume the global pending-work budget and prevent another peer's first job.
func TestOutboxCapacityIsFairPerPeer(t *testing.T) {
	const maxJobs = 4
	const perPeer = 2
	const otherPeer = "bus-outbox-peer-two"
	ob, _, _, _ := obNewOutbox(t, func(o *OutboxOptions) {
		o.MaxJobs = maxJobs
		o.MaxPendingPerPeer = perPeer
	})

	for i := 0; i < perPeer; i++ {
		if _, err := ob.Enqueue(obJob(t, uint64(900+i))); err != nil {
			t.Fatalf("first peer enqueue %d: %v", i, err)
		}
	}
	if _, err := ob.Enqueue(obJob(t, 999)); !errors.Is(err, ErrOutboxCapacity) {
		t.Fatalf("first peer exceeded its share with err %v, want ErrOutboxCapacity", err)
	}

	job := obJob(t, 1000)
	job.PeerBusID = otherPeer
	if _, err := ob.Enqueue(job); err != nil {
		t.Fatalf("another peer's first enqueue was denied after one peer filled its share: %v", err)
	}

}

// TestOutboxReplayPreservesPreQuotaSinglePeerBacklog is the upgrade-compatibility
// regression for adding MaxPendingPerPeer. An older build could durably
// acknowledge MaxJobs records for one peer. Replay must preserve all of them:
// the new quota constrains live admission, not already-acknowledged history.
// The global MaxJobs bound still applies and is pinned separately by
// TestOutboxCapacityIsEnforcedOnTheReplayPath.
func TestOutboxReplayPreservesPreQuotaSinglePeerBacklog(t *testing.T) {
	const maxJobs = 4
	const perPeer = 2
	replayed, _, clk, sink := obNewOutbox(t, func(o *OutboxOptions) {
		o.MaxJobs = maxJobs
		o.MaxPendingPerPeer = perPeer
	})

	for i := uint64(0); i < maxJobs; i++ {
		msgID := obMessageID(t, 1100+i)
		r := OutboxRecord{
			JobID: DeriveJobID(obPeerBus, msgID), PeerBusID: obPeerBus, OriginMessageID: msgID,
			Size: 11, ContentSHA256: obHash, EnqueuedAt: clk.Now(), State: OutboxPending,
		}
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode replay record: %v", err)
		}
		if err := replayed.Apply(wal.Committed{PrepareIndex: i*2 + 1, CommitIndex: i*2 + 2, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply replay record: %v", err)
		}
	}

	if got := len(replayed.Pending()); got != maxJobs {
		t.Fatalf("replayed pending jobs = %d, want all %d records acknowledged before the per-peer quota existed", got, maxJobs)
	}
	if strings.Contains(sink.String(), "DISCARDING an outbox record") {
		t.Fatalf("replay discarded acknowledged pre-quota records:\n%s", sink.String())
	}
}

// TestOutboxCapacityReservationsAreFairPerPeer pins the live-write
// window: work for one peer that is blocked in the durable write counts against
// that peer's share, but does not consume the share reserved for another peer.
func TestOutboxCapacityReservationsAreFairPerPeer(t *testing.T) {
	const (
		maxJobs  = 4
		perPeer  = 2
		attempts = 6
	)
	d := newOBBlockingDurable(maxJobs)
	t.Cleanup(d.unblock)
	ob, err := NewOutbox(OutboxOptions{
		BusID:             obLocalBus,
		Durable:           d,
		Now:               newOBClock().Now,
		MaxJobs:           maxJobs,
		MaxPendingPerPeer: perPeer,
		RetryHorizon:      time.Hour,
		SettledRetention:  time.Hour,
	})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}

	results := make(chan error, attempts+1)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		job := obJob(t, uint64(1200+i))
		go func() {
			<-start
			_, err := ob.Enqueue(job)
			results <- err
		}()
	}
	close(start)

	// Exactly perPeer same-peer calls may cross into the durable write. The
	// others must be refused while those writes remain blocked.
	for i := 0; i < perPeer; i++ {
		<-d.entered
	}
	for i := 0; i < attempts-perPeer; i++ {
		if err := <-results; !errors.Is(err, ErrOutboxCapacity) {
			t.Fatalf("same-peer overflow enqueue returned %v, want ErrOutboxCapacity", err)
		}
	}

	other := obJob(t, 1300)
	other.PeerBusID = "bus-outbox-peer-two"
	go func() {
		_, err := ob.Enqueue(other)
		results <- err
	}()
	<-d.entered // reaching Write proves the other peer retained capacity

	d.unblock()
	for i := 0; i < perPeer+1; i++ {
		if err := <-results; err != nil {
			t.Fatalf("reserved enqueue returned %v after the durable write was released", err)
		}
	}
	if got := ob.Pending(); len(got) != perPeer+1 {
		t.Fatalf("pending jobs = %d, want %d admitted reservations", len(got), perPeer+1)
	}
}

// TestOutboxConcurrentEnqueuesNeverExceedTheCap proves the capacity RESERVATION,
// which is the part of the bound that is easy to get subtly wrong.
//
// The lock is released across the durable write, so N concurrent enqueues could
// otherwise all pass a check only one of them had room for. pendingWrites counts
// the in-flight writes against the cap, and — this is the ordering that matters
// — it is released AFTER the record is folded into the table, never before. A
// release before the fold reopens the window: a concurrent enqueue takes the
// freed slot and the ALREADY-DURABLE record is refused by the in-memory bound,
// leaving disk and memory disagreeing about what this bus owes.
//
// The "durable but refused" log line is therefore the load-bearing assertion
// here, not the count.
func TestOutboxConcurrentEnqueuesNeverExceedTheCap(t *testing.T) {
	const maxJobs = 8
	const attempts = 4 * maxJobs
	ob, d, _, sink := obNewOutbox(t, func(o *OutboxOptions) {
		o.MaxJobs = maxJobs
		o.MaxPendingPerPeer = maxJobs // isolate the global reservation in this test
	})

	jobs := make([]OutboxJob, attempts)
	for i := range jobs {
		jobs[i] = obJob(t, uint64(800+i))
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refused  int
	)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(job OutboxJob) {
			defer wg.Done()
			<-start
			_, err := ob.Enqueue(job)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrOutboxCapacity):
				refused++
			default:
				t.Errorf("Enqueue gave an unexpected error: %v", err)
			}
		}(jobs[i])
	}
	close(start)
	wg.Wait()

	if accepted > maxJobs {
		t.Fatalf("%d of %d concurrent enqueues were accepted against a cap of %d: the reservation did not hold", accepted, attempts, maxJobs)
	}
	if accepted+refused != attempts {
		t.Fatalf("%d accepted + %d refused != %d attempts", accepted, refused, attempts)
	}
	if got := ob.Len(); got != accepted {
		t.Fatalf("the table holds %d records but %d enqueues were acknowledged; every acknowledged enqueue is durable, so memory and disk now disagree about what this bus owes", got, accepted)
	}
	if got := d.count(); got != accepted {
		t.Fatalf("%d durable writes for %d acknowledged enqueues", got, accepted)
	}
	if strings.Contains(sink.String(), "DURABLE but was refused") {
		t.Fatalf("a record that was already on stable storage was refused by the in-memory bound; the capacity reservation must be released AFTER the fold, not before it.\n--- log ---\n%s", sink.String())
	}
}

// TestOutboxConcurrentEnqueuesOfOneJobWriteOnce covers what the test above does
// NOT: N goroutines racing to enqueue the SAME job.
//
// Without a reservation the in-memory check is decorative — both callers see no
// existing record, both write a durable PENDING record, and the second is
// refused by the table for the rest of the log's life. The pending set still
// converges, so nothing is delivered twice; the cost is that EVERY future
// recovery logs "DISCARDING an outbox record that could not be applied" at
// ERROR for a record that was never wrong, on the exact line invariant 6
// reserves for a genuinely lost relay hop.
func TestOutboxConcurrentEnqueuesOfOneJobWriteOnce(t *testing.T) {
	for round := 0; round < 8; round++ {
		ob, d, _, sink := obNewOutbox(t, nil)
		job := obJob(t, uint64(1000+round))

		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]OutboxRecord, 32)
		errs := make([]error, 32)
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = ob.Enqueue(job)
			}(i)
		}
		close(start)
		wg.Wait()

		var accepted int
		for i, err := range errs {
			switch {
			case err == nil:
				accepted++
				if results[i].JobID != DeriveJobID(job.PeerBusID, job.OriginMessageID) {
					t.Fatalf("round %d: an accepted enqueue named job %s", round, results[i].JobID)
				}
			case errors.Is(err, ErrOutboxInFlight):
				// A caller that lost the race to the reservation. Acceptable:
				// it is told to retry and nothing durable was written for it.
			default:
				t.Fatalf("round %d: Enqueue gave an unexpected error: %v", round, err)
			}
		}
		if accepted == 0 {
			t.Fatalf("round %d: every concurrent enqueue of one job was refused; at least one must succeed", round)
		}
		if got := d.count(); got != 1 {
			t.Fatalf("round %d: %d durable records were written for ONE job, want exactly 1: the surplus records sit in the WAL forever and make every future recovery log a spurious ERROR discard", round, got)
		}
		if got := ob.Len(); got != 1 {
			t.Fatalf("round %d: the table holds %d records for one job, want 1", round, got)
		}
		if s := sink.String(); strings.Contains(s, "DURABLE but was refused") || strings.Contains(s, "two different pending records") {
			t.Fatalf("round %d: a durable record was refused by the table:\n--- log ---\n%s", round, s)
		}
	}
}

// TestOutboxRefusesATombstoneShorterThanTheHorizon: the anti-resurrection
// argument rests on SettledRetention >= RetryHorizon, so it is enforced
// STRUCTURALLY rather than documented — the treatment NewForwarder gives its own
// ceiling.
//
// The realistic trigger is a PARTIALLY filled options struct: a caller setting
// only RetryHorizon leaves SettledRetention on its 24h default and silently
// builds an outbox whose tombstones retire before the pending records they exist
// to refuse.
func TestOutboxRefusesATombstoneShorterThanTheHorizon(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tune    func(*OutboxOptions)
		wantErr bool
	}{
		{"defaults", func(*OutboxOptions) {}, false},
		{"retention shorter than the horizon", func(o *OutboxOptions) {
			o.RetryHorizon = time.Hour
			o.SettledRetention = time.Minute
		}, true},
		{"only the horizon set, so retention keeps its 24h default", func(o *OutboxOptions) {
			o.RetryHorizon = 72 * time.Hour
		}, true},
		{"retention equal to the horizon", func(o *OutboxOptions) {
			o.RetryHorizon = time.Hour
			o.SettledRetention = time.Hour
		}, false},
		{"retention longer than the horizon", func(o *OutboxOptions) {
			o.RetryHorizon = time.Hour
			o.SettledRetention = 2 * time.Hour
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := OutboxOptions{BusID: obLocalBus, Durable: &obNullDurable{}}
			tc.tune(&o)
			_, err := NewOutbox(o)
			if tc.wantErr && err == nil {
				t.Fatalf("NewOutbox accepted SettledRetention < RetryHorizon; the tombstone would then retire while a pending record for the same job is still live, and replaying that record puts a DELIVERED job back in the pending set")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("NewOutbox rejected a valid window pair: %v", err)
			}
		})
	}
}

// TestOutboxRefusesAFutureDatedRecordOnReplay: Expired and Retired are
// subtractions against the clock, so a record dated AHEAD of now is never
// expired and never retired — the age horizon simply stops applying to it, and a
// job that outlives idem.PeerOutageBudget is retried onto a peer that has
// forgotten the applied key and takes it as a NEW message.
//
// The realistic cause is not corruption: an NTP step or a restored VM snapshot
// moves the clock backwards relative to records already on disk.
func TestOutboxRefusesAFutureDatedRecordOnReplay(t *testing.T) {
	ob, _, clk, sink := obNewOutbox(t, nil)
	now := clk.Now()

	mk := func(seq uint64, enq, settled time.Time, state OutboxState) ([]byte, string) {
		msgID := obMessageID(t, seq)
		r := OutboxRecord{
			JobID: DeriveJobID(obPeerBus, msgID), PeerBusID: obPeerBus, OriginMessageID: msgID,
			Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: state, SettledAt: settled,
		}
		if state == OutboxAbandoned {
			r.Reason = "whatever"
		}
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return body, r.JobID
	}

	farFuture, farFutureID := mk(1100, now.Add(10*365*24*time.Hour), time.Time{}, OutboxPending)
	withinSkew, withinSkewID := mk(1101, now.Add(OutboxClockSkewAllowance-time.Minute), time.Time{}, OutboxPending)
	futureTomb, futureTombID := mk(1102, now.Add(-time.Hour), now.Add(10*365*24*time.Hour), OutboxDelivered)

	for i, body := range [][]byte{farFuture, withinSkew, futureTomb} {
		if err := ob.Apply(wal.Committed{PrepareIndex: uint64(i*2 + 1), CommitIndex: uint64(i*2 + 2), Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply returned %v, want nil", err)
		}
	}

	if _, ok := ob.Lookup(farFutureID); ok {
		t.Fatalf("a pending record dated ten years ahead was admitted; it can never expire, so the retry horizon does not apply to it and the job is immortal")
	}
	// A FUTURE-DATED TOMBSTONE IS KEPT, DELIBERATELY. An earlier version of this
	// guard discarded it, and discarding a tombstone is the resurrection
	// direction: the pending record of the same job is admitted while its
	// settlement is thrown away, and the peer receives the message twice. The
	// cost of keeping it is one slot out of maxJobs until the clock catches up —
	// bounded and self-healing, which a duplicate delivery is not.
	if r, ok := ob.Lookup(futureTombID); !ok || r.State != OutboxDelivered {
		t.Fatalf("a future-dated TOMBSTONE was discarded (%+v, %v); dropping a settlement is the one direction that resurrects a delivered job", r, ok)
	}
	// A clock that stepped back by less than the allowance must NOT cost us
	// relay hops: discarding those would be losing messages to fix a
	// bookkeeping problem.
	if r, ok := ob.Lookup(withinSkewID); !ok || r.State != OutboxPending {
		t.Fatalf("a record inside the %s skew allowance was discarded (%+v, %v); a small backwards clock step must not drop pending jobs", OutboxClockSkewAllowance, r, ok)
	}
	sink.mustContain(t, "the future-dated discards", "DISCARDING an outbox record", "ahead of the clock")
}

// TestOutboxABackwardsClockStepDoesNotResurrectADeliveredJob is the regression
// test for a bug this task INTRODUCED and then removed, and it is the most
// important test in this file after the crash tests.
//
// The first version of the skew guard checked every record and ran before the
// state machine, so it refused TOMBSTONES. With a backwards clock step S and an
// enqueue-to-settle gap G, any job with 5m < S <= G+5m was resurrected: the
// pending record was inside the allowance and admitted, its delivered record was
// outside it and discarded, and the peer received the message twice. The trigger
// band for a job abandoned at the 24h horizon is (5m, 24h05m] — wide, and hit by
// exactly the scenario the guard's own comment cites, an NTP step or a restored
// VM snapshot.
func TestOutboxABackwardsClockStepDoesNotResurrectADeliveredJob(t *testing.T) {
	for _, gap := range []time.Duration{time.Minute, 6 * time.Hour, OutboxRetryHorizon} {
		t.Run(gap.String(), func(t *testing.T) {
			// The log was written before the step: enqueued at T, settled at
			// T+gap. The clock then steps BACK so that the settlement is in the
			// future but the enqueue is not.
			ob, _, clk, _ := obNewOutbox(t, nil)
			now := clk.Now()
			enq := now.Add(-time.Minute)

			msgID := obMessageID(t, 9000)
			jobID := DeriveJobID(obPeerBus, msgID)
			pending := OutboxRecord{
				JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: msgID,
				Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: OutboxPending,
			}
			delivered := pending
			delivered.State = OutboxDelivered
			delivered.SettledAt = enq.Add(gap)

			for i, r := range []OutboxRecord{pending, delivered} {
				body, err := r.Encode()
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				if err := ob.Apply(wal.Committed{PrepareIndex: uint64(i*2 + 1), CommitIndex: uint64(i*2 + 2), Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
					t.Fatalf("Apply returned %v, want nil", err)
				}
			}

			if got := obPendingIDs(ob); len(got) != 0 {
				t.Fatalf("after a backwards clock step the pending set is %v, want EMPTY. The settlement was discarded as future-dated while its pending record was kept, so a DELIVERED job is queued again and the peer receives the message twice", got)
			}
			if r, ok := ob.Lookup(jobID); !ok || r.State != OutboxDelivered {
				t.Fatalf("job %s is (%+v, %v), want delivered", jobID, r, ok)
			}
		})
	}
}

// obClockStepDurable is a durable log that steps the clock once, inside Write —
// i.e. between the record being stamped and its fold. It is the only way to
// exercise the window an NTP correction opens during an fsync.
type obClockStepDurable struct {
	inner *obNullDurable
	clk   *obClock
	step  time.Duration
	once  sync.Once
}

func (d *obClockStepDurable) Write(e wal.Entry) (wal.Committed, error) {
	c, err := d.inner.Write(e)
	if err != nil {
		return c, err
	}
	d.once.Do(func() { d.clk.Advance(d.step) })
	return c, nil
}

// TestOutboxABigBackwardsClockStepDropsAPendingJobLoudlyAndConsistently pins the
// behaviour chosen for a clock correction larger than the skew allowance, and —
// more importantly — pins that the LIVE TABLE and REPLAY reach the SAME verdict.
//
// The trade: the relay hop is LOST rather than kept. A pending record stamped
// ahead of the clock can never age out (Expired's subtraction stays negative),
// so keeping it means a job that outlives idem.PeerOutageBudget and is then
// retried onto a peer that has forgotten the applied key — a duplicate delivery,
// which invariant 10 ranks worse than the loss. Invariant 6 requires only that
// the discard be loud, and it is.
//
// THE CONSISTENCY HALF IS THE POINT. An earlier revision checked skew ONLY at
// admission, so replay discarded such a record while the live table kept it
// forever: a job enqueued under a clock a month fast was still pending ten days
// past its 24h horizon. One predicate (FutureDated) is now called by both.
func TestOutboxABigBackwardsClockStepDropsAPendingJobLoudlyAndConsistently(t *testing.T) {
	inner := &obNullDurable{}
	clk := newOBClock()
	sink := &obLogSink{}
	step := -(OutboxClockSkewAllowance + time.Hour)
	ob, err := NewOutbox(OutboxOptions{
		BusID:   obLocalBus,
		Durable: &obClockStepDurable{inner: inner, clk: clk, step: step},
		Logger:  logging.New(sink, logging.LevelDebug),
		Now:     clk.Now,
	})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}

	rec, err := ob.Enqueue(obJob(t, 7777))
	if err != nil {
		t.Fatalf("Enqueue across a backwards clock step gave err = %v", err)
	}
	if inner.count() != 1 {
		t.Fatalf("%d durable writes, want 1", inner.count())
	}
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("the live table still holds %v after a %s clock correction; a pending record stamped ahead of the clock can never age out, so the forwarder would retry it past idem.PeerOutageBudget where the peer applies it as a NEW message", got, step)
	}
	// Invariant 6 asks that the discard be LOUD AND SPECIFIC, not that it come
	// from one particular line. Depending on where the clock lands relative to
	// the fold, the same job is either refused at admission (an ERROR naming the
	// durable record) or admitted and swept (a WARN naming the drop) — so the
	// assertion is on the JOB and the REASON, which both forms carry.
	sink.mustContain(t, "the live-table drop", rec.JobID, "ahead of the clock")

	// THE CONSISTENCY ASSERTION: replaying the very record that was written must
	// reach the same verdict, or memory and disk disagree about which jobs are
	// live and a restart silently changes what this bus owes.
	ob2, _, clk2, sink2 := obNewOutbox(t, nil)
	clk2.Advance(step)
	body, err := rec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := ob2.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply returned %v, want nil", err)
	}
	if got := obPendingIDs(ob2); len(got) != 0 {
		t.Fatalf("replay ADMITTED %v while the live table dropped the same record; the two paths must agree about which jobs are live", got)
	}
	sink2.mustContain(t, "the replay discard", "DISCARDING an outbox record", "ahead of the clock")
}

// TestOutboxAClockCorrectionAfterEnqueueDropsTheImmortalJob is the scenario the
// admission guard CANNOT catch, and therefore the one the sweep exists for.
//
// A job enqueued while the clock is FAST is perfectly well-formed at admission —
// EnqueuedAt equals the clock at that instant, so nothing is future-dated yet.
// The correction arrives afterwards, and from then on the record is stamped a
// month ahead of the clock: Expired's subtraction is negative, the 24h horizon
// never fires, and the job is immortal. Measured before the fix at ten days past
// its horizon and still pending.
//
// Only sweepLocked can see this, because only it runs again after the
// correction. That is why FutureDated is a predicate shared by both paths rather
// than a check at the door.
func TestOutboxAClockCorrectionAfterEnqueueDropsTheImmortalJob(t *testing.T) {
	ob, _, clk, sink := obNewOutbox(t, nil)

	// Enqueued normally, under a clock nobody yet knows is wrong.
	rec, err := ob.Enqueue(obJob(t, 7778))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got := obPendingIDs(ob); len(got) != 1 {
		t.Fatalf("the job was not admitted at all: %v", got)
	}

	// NTP corrects the clock a month backwards, long after the write.
	clk.Advance(-(30 * 24 * time.Hour))

	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("the job is STILL pending %s after a clock correction left it stamped a month in the future: %v. Expired's subtraction stays negative, so the 24h horizon never fires and the forwarder would retry it past idem.PeerOutageBudget, where the peer applies it as a NEW message",
			OutboxRetryHorizon, got)
	}
	sink.mustContain(t, "the post-correction sweep", rec.JobID, "ahead of the clock")

	// And the drop is a fixed point: sweeping again is quiet and the job stays
	// gone rather than reappearing.
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("the job came back on a second sweep: %v", got)
	}
}

// TestOutboxSettleAcrossAClockStepIsBelievedInMemory pins the SettledAt
// forward-clamp, and nothing else — read the note at the end before adding to
// it.
//
// A backwards clock step SMALLER than the skew allowance (so the job survives
// the sweep) still makes SettledAt land before EnqueuedAt, which validate
// refuses. Without the clamp, Settle FAILS and the job stays PENDING and is
// retried forever — a duplicate delivery caused by a bookkeeping problem, which
// is the same failure class as an unstorable reason arriving through a different
// door.
//
// The step is deliberately UNDER the allowance: a larger one drops the job at
// the sweep before Settle can find it, which is
// TestOutboxABigBackwardsClockStepDropsAPendingJobLoudlyAndConsistently's
// territory, not this test's.
func TestOutboxSettleAcrossAClockStepIsBelievedInMemory(t *testing.T) {
	ob, _, clk, sink := obNewOutbox(t, nil)
	job, err := ob.Enqueue(obJob(t, 9300))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	clk.Advance(-time.Minute)

	settled, err := ob.Settle(job.JobID, OutboxDelivered, "")
	if err != nil {
		t.Fatalf("Settle across a backwards clock step gave err = %v; the settle failed on its own timestamp and the job stays PENDING, so the message is sent again", err)
	}
	if settled.State != OutboxDelivered {
		t.Fatalf("Settle returned state %s, want delivered", settled.State)
	}
	if !settled.SettledAt.Equal(settled.EnqueuedAt) {
		t.Fatalf("settled_at is %s and enqueued_at is %s; the clamp must move the settlement forward to the enqueue instant, never invent one", settled.SettledAt, settled.EnqueuedAt)
	}
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("Settle reported success but the job is still pending: %v", got)
	}
	if s := sink.String(); strings.Contains(s, "DURABLE but was refused") {
		t.Fatalf("an already-durable settlement was refused by the in-memory table:\n--- log ---\n%s", s)
	}
}

// TestOutboxRefusesASelfAddressedRecordOnReplay: Enqueue checks the destination
// through ValidatePeerBusID, but DecodeOutboxRecord cannot — it holds no local
// bus id. upsertLocked does, so the check belongs there as well. Reachable by
// reopening a data directory under a different -bus-id.
func TestOutboxRefusesASelfAddressedRecordOnReplay(t *testing.T) {
	ob, _, _, sink := obNewOutbox(t, nil)
	msgID := obMessageID(t, 1200)
	r := OutboxRecord{
		JobID: DeriveJobID(obLocalBus, msgID), PeerBusID: obLocalBus, OriginMessageID: msgID,
		Size: 11, ContentSHA256: obHash, EnqueuedAt: newOBClock().Now(), State: OutboxPending,
	}
	body, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := ob.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply returned %v, want nil", err)
	}
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("a job addressed to THIS bus was replayed into the pending set (%v); the forwarder would retry a loop until its horizon ran out", got)
	}
	sink.mustContain(t, "the self-addressed discard", "DISCARDING an outbox record", "addressed to this bus")
}

// TestOutboxSettleCannotBeMadeToFailByItsOwnReason.
//
// encoding/json rewrites every invalid UTF-8 byte as a three-byte U+FFFD, so a
// bound applied to the RAW Go string is not a bound on the record: a 200-byte
// reason of invalid UTF-8 encodes to 600 and Encode's own validate refuses it.
// That refusal lands on the SETTLE, so the job stays PENDING and is retried —
// and MaxOutboxReasonLen's own doc says a peer's error code can reach this
// field, so once RELAY-19 wires it a peer could choose bytes that make its own
// jobs unsettleable and hold slots against the cap for the full horizon.
func TestOutboxSettleCannotBeMadeToFailByItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"invalid UTF-8 at the length bound", strings.Repeat("\xff", 200)},
		{"invalid UTF-8 well over the bound", strings.Repeat("\xff", 4096)},
		{"valid but oversized", strings.Repeat("é", 4096)},
		{"a lone surrogate-ish tail byte", "peer said: \x80\x80\x80"},
		{"ordinary text", "peer refused with 403 unpeered-bus"},
		{"empty — a peer returned no body at all", ""},
		{"whitespace only", "   \t\n  "},
		// The row that catches the ordering trap: this is NOT blank, so an
		// emptiness check applied BEFORE truncation lets it through, and
		// truncating to MaxOutboxReasonLen then leaves nothing but spaces — a
		// reason that satisfies validate's non-empty rule and tells an operator
		// nothing.
		{"padding that only becomes blank once truncated", strings.Repeat(" ", MaxOutboxReasonLen+4) + "the real reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ob, _, clk, _ := obNewOutbox(t, nil)
			job, err := ob.Enqueue(obJob(t, 1300))
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			clk.Advance(time.Second)

			settled, err := ob.Settle(job.JobID, OutboxAbandoned, tc.reason)
			if err != nil {
				t.Fatalf("Settle failed on its own reason (%v); the job stays PENDING and is retried forever, which is the opposite of what the caller asked for", err)
			}
			if len(settled.Reason) > MaxOutboxReasonLen {
				t.Fatalf("the stored reason is %d bytes, over the %d bound", len(settled.Reason), MaxOutboxReasonLen)
			}
			if !utf8.ValidString(settled.Reason) {
				t.Fatalf("the stored reason is not valid UTF-8, so its encoded length is not the length that was bounded")
			}
			if strings.TrimSpace(settled.Reason) == "" {
				t.Fatalf("the stored reason is blank (%q); invariant 6 asks that a discard be recorded SPECIFICALLY, and a record of nothing but spaces meets the letter of the non-empty rule while missing its point", settled.Reason)
			}
			if got := obPendingIDs(ob); len(got) != 0 {
				t.Fatalf("the job is still pending after being abandoned: %v", got)
			}

			// AND the verbatim repeat is still recognised as the same
			// settlement. Comparing the caller's RAW reason against the stored
			// SANITISED one would report an identical retry as a contradiction.
			again, err := ob.Settle(job.JobID, OutboxAbandoned, tc.reason)
			if err != nil {
				t.Fatalf("repeating the identical settlement gave err = %v, want the original record: a verbatim retry must not be reported as a contradiction", err)
			}
			if again.Reason != settled.Reason || !again.SettledAt.Equal(settled.SettledAt) {
				t.Fatalf("the repeat returned a different record:\n got %+v\nwant %+v", again, settled)
			}
		})
	}
}

// TestOutboxDurableWriteFailureIsNotAcknowledged: invariant 4 in the negative
// direction. A failed durable write must leave NOTHING in memory, on either
// path.
func TestOutboxDurableWriteFailureIsNotAcknowledged(t *testing.T) {
	boom := errors.New("the disk said no")

	t.Run("enqueue", func(t *testing.T) {
		ob, d, _, _ := obNewOutbox(t, nil)
		d.err = boom
		if _, err := ob.Enqueue(obJob(t, 1400)); !errors.Is(err, boom) {
			t.Fatalf("Enqueue gave err = %v, want the durable log's error", err)
		}
		if ob.Len() != 0 {
			t.Fatalf("a job whose durable write FAILED is in the table; nothing may be acknowledged before it is durable")
		}
	})

	t.Run("settle", func(t *testing.T) {
		ob, d, clk, _ := obNewOutbox(t, nil)
		job, err := ob.Enqueue(obJob(t, 1401))
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		clk.Advance(time.Second)
		d.err = boom
		if _, err := ob.Settle(job.JobID, OutboxDelivered, ""); !errors.Is(err, boom) {
			t.Fatalf("Settle gave err = %v, want the durable log's error", err)
		}
		r, ok := ob.Lookup(job.JobID)
		if !ok || r.State != OutboxPending {
			t.Fatalf("after a FAILED settle the job is (%+v, %v), want still pending: a settlement that never reached disk must not be believed in memory", r, ok)
		}
		// And the reservation was released, so a later settle can still succeed.
		d.err = nil
		if _, err := ob.Settle(job.JobID, OutboxDelivered, ""); err != nil {
			t.Fatalf("settling after a failed attempt gave err = %v; the in-flight reservation leaked", err)
		}
	})
}

// TestOutboxAgeHorizonIsEnforcedOnTheReplayPath: a pending record older than
// idem.PeerOutageBudget is DROPPED rather than re-offered, because the receiving
// bus has by then forgotten the applied key and would take the retry as a NEW
// message (invariant 10) — a duplicate delivery, which is worse than the loss it
// was avoiding.
func TestOutboxAgeHorizonIsEnforcedOnTheReplayPath(t *testing.T) {
	ob, _, clk, sink := obNewOutbox(t, nil)

	fresh := obMessageID(t, 600)
	stale := obMessageID(t, 601)

	build := func(msgID string, enq time.Time) []byte {
		r := OutboxRecord{
			JobID: DeriveJobID(obPeerBus, msgID), PeerBusID: obPeerBus, OriginMessageID: msgID,
			Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: OutboxPending,
		}
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return body
	}
	// Written before a long outage...
	staleBody := build(stale, clk.Now())

	// ...and replayed after it. The clock jumps past the horizon; a job
	// enqueued at the NEW time is still inside it.
	clk.Advance(OutboxRetryHorizon + time.Minute)
	freshBody := build(fresh, clk.Now())

	for i, body := range [][]byte{staleBody, freshBody} {
		if err := ob.Apply(wal.Committed{PrepareIndex: uint64(i*2 + 1), CommitIndex: uint64(i*2 + 2), Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply returned %v, want nil", err)
		}
	}

	staleID := DeriveJobID(obPeerBus, stale)
	if _, ok := ob.Lookup(staleID); ok {
		t.Fatalf("job %s replayed back into the table %s after it was enqueued; retrying past the horizon is applied by the peer as a NEW message",
			staleID, OutboxRetryHorizon+time.Minute)
	}
	freshID := DeriveJobID(obPeerBus, fresh)
	if r, ok := ob.Lookup(freshID); !ok || r.State != OutboxPending {
		t.Fatalf("the in-horizon job %s recovered as (%+v, %v), want pending", freshID, r, ok)
	}
	sink.mustContain(t, "a horizon discard on replay", "DISCARDING an outbox record", staleID, "past the retry horizon")

	// A job already in the table is swept when the clock crosses the horizon,
	// and the drop is LOUD: it is a message this bus will never deliver.
	clk.Advance(OutboxRetryHorizon + time.Minute)
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("pending set = %v after the clock crossed the horizon again, want empty", got)
	}
	sink.mustContain(t, "the sweep", "DROPPING an outbox job that has passed the retry horizon", freshID)
}

// TestOutboxSettledRecordsAreRetiredEventually: the tombstone window is bounded
// too, or the table would grow without limit on a healthy bus.
func TestOutboxSettledRecordsAreRetiredEventually(t *testing.T) {
	ob, _, clk, _ := obNewOutbox(t, nil)
	job, err := ob.Enqueue(obJob(t, 700))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := ob.Settle(job.JobID, OutboxDelivered, ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if ob.Len() != 1 {
		t.Fatalf("the settled record is not retained; a tombstone is what refuses a stale pending record")
	}
	clk.Advance(OutboxSettledRetention + time.Minute)
	if got := ob.Len(); got != 0 {
		t.Fatalf("the outbox retains %d records past the tombstone window, want 0", got)
	}
}

// TestOutboxARetiredTombstoneCannotBeFollowedByAStalePending is the direct test
// of the ONE way a delivered job could still be resurrected here, and it is the
// analogue of the P0 found in the sibling peer-store task.
//
// THE DEFECT CLASS: if the ordering evidence for a job can itself LEAVE the
// table, a later record arrives at a table that has forgotten the job and is
// admitted as new. The peer-store hit this because it keyed monotonicity on a
// per-peer counter derived from an entry that could be swept, so the counter
// restarted at 1 and a superseded generation won on replay.
//
// THIS OUTBOX HAS NO COUNTER. Monotonicity is keyed on the STATE RANK carried by
// each record itself (pending < terminal), so there is nothing derived to
// restart. What remains is the table-forgetting case: the tombstone is swept by
// OutboxSettledRetention, and then the job's original PENDING record replays.
//
// It cannot resurrect, and the reason is an inequality rather than an accident.
// For the tombstone to have been retired, now-SettledAt > settledRetention.
// Every record validates SettledAt >= EnqueuedAt, so
//
//	now-EnqueuedAt >= now-SettledAt > settledRetention >= retryHorizon
//
// and the pending record is therefore EXPIRED — refused by the guard above the
// insert. The last step is exactly the inequality NewOutbox refuses to be built
// without, which is what turns this from a happy accident into a guarantee.
func TestOutboxARetiredTombstoneCannotBeFollowedByAStalePending(t *testing.T) {
	ob, _, clk, sink := obNewOutbox(t, nil)

	// A job enqueued, delivered, and then left until its tombstone retires.
	job, err := ob.Enqueue(obJob(t, 5100))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	pendingRec, ok := ob.Lookup(job.JobID)
	if !ok {
		t.Fatalf("the pending record is not in the table")
	}
	pendingBody, err := pendingRec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	clk.Advance(time.Second)
	if _, err := ob.Settle(job.JobID, OutboxDelivered, ""); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	// Long enough that the tombstone is gone from the table entirely.
	clk.Advance(OutboxSettledRetention + time.Hour)
	if _, ok := ob.Lookup(job.JobID); ok {
		t.Fatalf("the tombstone is still retained; this test needs it GONE to mean anything")
	}
	if ob.Len() != 0 {
		t.Fatalf("the table holds %d records, want 0", ob.Len())
	}

	// NOW the stale pending record replays into a table that has forgotten the
	// job. This is the resurrection attempt.
	if err := ob.Apply(wal.Committed{PrepareIndex: 1, CommitIndex: 2, Entry: wal.Entry{Kind: OutboxRecordKind, Body: pendingBody}}); err != nil {
		t.Fatalf("Apply returned %v, want nil", err)
	}
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("a stale PENDING record was admitted after its tombstone had been retired: %v. The job was DELIVERED, so the peer now receives the message a second time and at-least-once has become at-least-twice", got)
	}
	if _, ok := ob.Lookup(job.JobID); ok {
		t.Fatalf("job %s is back in the table after its tombstone retired", job.JobID)
	}
	sink.mustContain(t, "the refused resurrection", "DISCARDING an outbox record", job.JobID)

	// The same holds for a FRESH replay of the whole surviving history, which is
	// what a restart actually does: pending then delivered, both long stale.
	ob2, _, clk2, _ := obNewOutbox(t, nil)
	clk2.Advance(OutboxSettledRetention + time.Hour)
	settledRec := pendingRec
	settledRec.State = OutboxDelivered
	settledRec.SettledAt = pendingRec.EnqueuedAt.Add(time.Second)
	settledBody, err := settledRec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for i, body := range [][]byte{pendingBody, settledBody} {
		if err := ob2.Apply(wal.Committed{PrepareIndex: uint64(i*2 + 1), CommitIndex: uint64(i*2 + 2), Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply returned %v, want nil", err)
		}
	}
	if got := obPendingIDs(ob2); len(got) != 0 {
		t.Fatalf("replaying a fully-stale history left %v pending; nothing that old may be re-offered", got)
	}
}

// TestOutboxAStaleSiblingCannotOutliveASettlement is the regression test for a
// REPRODUCED P0 in this task's own earlier revision, and it is the case my
// original correctness proof got wrong.
//
// That proof said a retired tombstone can never strand a job as pending: retiring
// it needs now-SettledAt > settledRetention, and SettledAt >= EnqueuedAt, so
// now-EnqueuedAt > settledRetention >= retryHorizon and the pending record must
// already be expired.
//
// SettledAt >= EnqueuedAt IS A WITHIN-ONE-RECORD RULE, AND THE CONCLUSION IS
// CROSS-RECORD. Two pending records can exist for one job with DIFFERENT
// anchors, and the tombstone is anchored on only ONE of them — so the OTHER can
// still be inside the horizon when the tombstone retires. sweepLocked is what
// creates the pair: a record written under a fast clock is swept when the clock
// is corrected, the caller re-offers the message, and the durable log ends up
// holding two pending records and a settlement anchored on the second.
//
// This is the same defect CLASS as the sibling peer-store P0 — the ordering
// evidence leaving the table — arriving through retention rather than a counter.
func TestOutboxAStaleSiblingCannotOutliveASettlement(t *testing.T) {
	msgID := obMessageID(t, 6100)
	jobID := DeriveJobID(obPeerBus, msgID)

	build := func(t *testing.T, enq, settled time.Time, state OutboxState) []byte {
		t.Helper()
		r := OutboxRecord{
			JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: msgID,
			Size: 11, ContentSHA256: obHash, EnqueuedAt: enq, State: state, SettledAt: settled,
		}
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return body
	}

	// The replay clock is swept across the whole band in which the tombstone is
	// retired but the fast-clock pending record is not yet expired. A single
	// point would look like a lucky boundary; the band is ~15 minutes wide.
	for _, at := range []time.Duration{
		24*time.Hour + 1*time.Minute,
		24*time.Hour + 8*time.Minute,
		24*time.Hour + 14*time.Minute,
	} {
		t.Run(at.String(), func(t *testing.T) {
			ob, _, clk, _ := obNewOutbox(t, nil)
			base := clk.Now()

			// P1 was stamped under a clock running fifteen minutes fast; P2 is
			// the re-offer after the correction; T settles P2.
			p1 := build(t, base.Add(15*time.Minute), time.Time{}, OutboxPending)
			p2 := build(t, base, time.Time{}, OutboxPending)
			tomb := build(t, base, base.Add(10*time.Second), OutboxDelivered)

			clk.Advance(at)
			for i, body := range [][]byte{p1, p2, tomb} {
				if err := ob.Apply(wal.Committed{PrepareIndex: uint64(i*2 + 1), CommitIndex: uint64(i*2 + 2), Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
					t.Fatalf("Apply returned %v, want nil", err)
				}
			}

			if got := obPendingIDs(ob); len(got) != 0 {
				t.Fatalf("a DELIVERED job is back in the pending set: %v. The settlement was discarded as a retired tombstone while a stale sibling pending record — anchored fifteen minutes later and therefore not yet expired — was admitted. The peer receives the message a second time",
					got)
			}
			if r, ok := ob.Lookup(jobID); ok && r.State == OutboxPending {
				t.Fatalf("job %s is pending after being delivered", jobID)
			}
		})
	}
}

// TestOutboxASettlementIsNeverDiscardedForCapacity is the other half of the same
// P0.
//
// Refusing a tombstone for want of a slot refuses the one record that prevents a
// duplicate delivery. It is reachable in STRICT COMMIT ORDER at default
// settings: a job enqueued at T and settled at T+23h58m has its PENDING record
// EXPIRE before its tombstone RETIRES, so on replay the settlement arrives at a
// table that holds no sibling for it and takes the insert path, where the cap
// applies. Drop it, let a neighbour age out, and a later pending record for that
// job is admitted into the freed slot.
func TestOutboxASettlementIsNeverDiscardedForCapacity(t *testing.T) {
	const maxJobs = 4
	ob, _, clk, sink := obNewOutbox(t, func(o *OutboxOptions) {
		o.MaxJobs = maxJobs
		o.MaxPendingPerPeer = maxJobs // fill the global bound with one peer
	})
	base := clk.Now()

	apply := func(t *testing.T, r OutboxRecord, idx uint64) {
		t.Helper()
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if err := ob.Apply(wal.Committed{PrepareIndex: idx, CommitIndex: idx + 1, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply returned %v, want nil", err)
		}
	}

	// Fill the table to the cap with unrelated live jobs.
	for i := 0; i < maxJobs; i++ {
		m := obMessageID(t, uint64(6200+i))
		apply(t, OutboxRecord{
			JobID: DeriveJobID(obPeerBus, m), PeerBusID: obPeerBus, OriginMessageID: m,
			Size: 11, ContentSHA256: obHash, EnqueuedAt: base, State: OutboxPending,
		}, uint64(i*2+1))
	}
	if ob.Len() != maxJobs {
		t.Fatalf("the table holds %d, want the cap of %d before the settlement arrives", ob.Len(), maxJobs)
	}

	// The settlement of a job whose pending record is long gone, arriving while
	// the table is full.
	m := obMessageID(t, 6299)
	jobID := DeriveJobID(obPeerBus, m)
	apply(t, OutboxRecord{
		JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: m,
		Size: 11, ContentSHA256: obHash,
		EnqueuedAt: base.Add(-23 * time.Hour), State: OutboxDelivered, SettledAt: base.Add(-time.Minute),
	}, 999)

	r, ok := ob.Lookup(jobID)
	if !ok || r.State != OutboxDelivered {
		t.Fatalf("the settlement was DISCARDED for capacity (%+v, %v). A tombstone is the record that stops a delivered message being sent again; drop it and a later pending record for the same job resurrects the job", r, ok)
	}
	if strings.Contains(sink.String(), "DISCARDING an outbox record") {
		t.Fatalf("the settlement produced a discard while pending capacity was full; tombstones do not consume pending capacity:\n%s", sink.String())
	}

	// A PENDING record at capacity is still refused — the exemption is for
	// tombstones only, or the cap would mean nothing.
	if _, err := ob.Enqueue(obJob(t, 6300)); !errors.Is(err, ErrOutboxCapacity) {
		t.Fatalf("Enqueue of a PENDING job past the cap gave err = %v, want ErrOutboxCapacity: only settlements are exempt", err)
	}
}

// TestOutboxNoSettledJobIsEverPendingAfterReplay is a systematic sweep over the
// resurrection plane, written after TWO hand-built proofs of this property
// turned out to be wrong (see TestOutboxAStaleSiblingCannotOutliveASettlement).
//
// THE INVARIANT: replay a durable history that contains a SETTLEMENT for a job,
// and that job must not end up PENDING. That is the whole safety property of
// this file — a job left pending after being delivered is a message the peer
// receives twice.
//
// It preserves PER-JOB COMMIT ORDER, because that is what wal guarantees and
// what the argument is entitled to assume: a job's pending records precede its
// settlement. It varies everything the two broken proofs turned on — how far
// apart two pending records' anchors are (the fast-clock sibling), where the
// replay clock falls relative to BOTH windows and their boundaries, the cap, and
// how much capacity pressure unrelated jobs apply.
//
// THE FILLER COUNT IS LOAD-BEARING AND THE FIRST VERSION GOT IT WRONG: it filled
// the table to exactly maxJobs every time, so the job's own pending records were
// always refused by the cap and the sweep never reached the plane it was written
// to explore. It passed with the fix REVERTED, which is how the mistake was
// caught. Fractions of the cap, including none, are what make the capacity axis
// real rather than decorative.
func TestOutboxNoSettledJobIsEverPendingAfterReplay(t *testing.T) {
	base := newOBClock().Now()

	spreads := []time.Duration{0, 10 * time.Second, 15 * time.Minute, 2 * time.Hour}
	clocks := []time.Duration{
		time.Minute,
		12 * time.Hour,
		OutboxRetryHorizon - time.Minute,
		OutboxRetryHorizon,
		OutboxRetryHorizon + time.Minute,
		OutboxRetryHorizon + 8*time.Minute,
		OutboxRetryHorizon + 20*time.Minute,
		2 * OutboxRetryHorizon,
	}
	caps := []int{4, 64}

	atomic.StoreInt64(&obPlaneTotal, 0)
	atomic.StoreInt64(&obPlaneTerminal, 0)

	seq := uint64(20000)
	for _, spread := range spreads {
		for _, at := range clocks {
			for _, maxJobs := range caps {
				for _, fillers := range []int{0, maxJobs / 2, maxJobs - 1, maxJobs} {
					seq++
					name := fmt.Sprintf("spread=%s/clock=%s/cap=%d/fill=%d", spread, at, maxJobs, fillers)
					t.Run(name, func(t *testing.T) {
						ob, _, clk, _ := obNewOutbox(t, func(o *OutboxOptions) {
							o.MaxJobs = maxJobs
							o.MaxPendingPerPeer = maxJobs // this sweep varies global pressure, not peer fairness
						})
						msgID := obMessageID(t, seq)
						jobID := DeriveJobID(obPeerBus, msgID)

						clk.Advance(at)
						idx := uint64(1)
						apply := func(r OutboxRecord) {
							body, err := r.Encode()
							if err != nil {
								t.Fatalf("Encode %s: %v", r.State, err)
							}
							if err := ob.Apply(wal.Committed{PrepareIndex: idx, CommitIndex: idx + 1, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
								t.Fatalf("Apply returned %v, want nil", err)
							}
							idx += 2
						}

						for f := 0; f < fillers; f++ {
							m := obMessageID(t, seq*100+uint64(f))
							apply(OutboxRecord{
								JobID: DeriveJobID(obPeerBus, m), PeerBusID: obPeerBus, OriginMessageID: m,
								Size: 11, ContentSHA256: obHash, EnqueuedAt: clk.Now(), State: OutboxPending,
							})
						}

						// Per-job commit order: two pending records, then the
						// settlement. The FIRST carries the spread — the
						// fast-clock sibling, anchored LATER than the record the
						// settlement is anchored on.
						mk := func(enq, settled time.Time, st OutboxState) OutboxRecord {
							return OutboxRecord{
								JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: msgID,
								Size: 11, ContentSHA256: obHash,
								EnqueuedAt: enq, State: st, SettledAt: settled,
							}
						}
						apply(mk(base.Add(spread), time.Time{}, OutboxPending))
						apply(mk(base, time.Time{}, OutboxPending))
						apply(mk(base, base.Add(10*time.Second), OutboxDelivered))

						for _, p := range ob.Pending() {
							if p.JobID == jobID {
								t.Fatalf("job %s is PENDING after a history containing its DELIVERED record (%s). The peer receives this message a second time", jobID, name)
							}
						}
						r, ok := ob.Lookup(jobID)
						if ok && r.State == OutboxPending {
							t.Fatalf("job %s is pending in the table after being delivered (%s)", jobID, name)
						}
						// HALF THIS SWEEP USED TO PROVE NOTHING. Where the job
						// ends up ABSENT — swept, or refused as too old — the
						// "not pending" assertion is trivially true, and 128 of
						// 256 sub-cases were in that state. So the admissible
						// cases now assert the STRONGER property: the job is
						// present AND terminal. Absence is only accepted where
						// the records are genuinely past their windows.
						// Only the replay clock decides admissibility here: the
						// spreads above are all far under the horizon, so a
						// `spread <= OutboxRetryHorizon` conjunct would be dead
						// while reading as load-bearing. If a spread past the
						// horizon is ever added, this needs it back.
						admissible := at <= OutboxRetryHorizon
						if admissible && fillers < maxJobs {
							if !ok {
								t.Fatalf("job %s is ABSENT (%s) though every record in its history is inside its window; the sub-case proves nothing about resurrection", jobID, name)
							}
							if !r.State.Terminal() {
								t.Fatalf("job %s is %s (%s), want a terminal state", jobID, r.State, name)
							}
							atomic.AddInt64(&obPlaneTerminal, 1)
						}
						atomic.AddInt64(&obPlaneTotal, 1)
					})
				}
			}
		}
	}

	// A FLOOR ON THE STRONG ASSERTION, so this sweep can never quietly become
	// half-vacuous again. It did exactly that once already: an earlier version
	// filled the table to the cap in every sub-case, so the job's own records
	// were always refused and the whole sweep passed with the fix REVERTED.
	total := atomic.LoadInt64(&obPlaneTotal)
	terminal := atomic.LoadInt64(&obPlaneTerminal)
	if total == 0 {
		t.Fatal("the sweep ran no sub-cases at all")
	}
	if terminal*4 < total {
		t.Fatalf("only %d of %d sub-cases reached the strong present-and-terminal assertion; the rest end with the job absent, where \"not pending\" is trivially true and proves nothing about resurrection", terminal, total)
	}
	t.Logf("resurrection plane: %d sub-cases, %d asserted present-and-terminal", total, terminal)
}

// obPlaneTotal and obPlaneTerminal count how much of the sweep reached the
// STRONG assertion, so a generator change that quietly stops exercising the
// plane fails the floor instead of passing silently.
var obPlaneTotal, obPlaneTerminal int64

// TestOutboxASettlementIsNeverRefusedByTheContentCheck is the THIRD predicate
// that was dropping settlements, found independently by both gates after the
// first two were fixed.
//
// The pattern is worth naming, because it is what this task kept getting wrong:
// EVERY guard that runs before the state machine is a place a TOMBSTONE can be
// thrown away, and throwing away a tombstone leaves a delivered job pending —
// the peer receives the message twice. Retention was the first, capacity the
// second, and the content/size mismatch the third.
//
// The precondition here is two pending records for one job id carrying different
// content, which is an invariant-1 violation or a spliced log rather than
// something a peer can arrange (the WAL is MAC-keyed). It is closed anyway,
// because "our own code wrote it" is exactly the claim corruption disproves —
// and because the fix costs one branch.
func TestOutboxASettlementIsNeverRefusedByTheContentCheck(t *testing.T) {
	msgID := obMessageID(t, 8100)
	jobID := DeriveJobID(obPeerBus, msgID)

	// The pending record is anchored TWENTY MINUTES AFTER base, so a settlement
	// stamped at base lands BEFORE it and the forward-clamp actually fires.
	// Without that offset the clamp is dead under test — deleting it left the
	// whole suite green, which both gates caught independently. The offset is
	// applied by advancing the clock rather than by future-dating the record,
	// because a future-dated pending record is refused outright.
	newFixture := func(t *testing.T) (*Outbox, *obClock, *obLogSink, time.Time, func(OutboxRecord)) {
		t.Helper()
		ob, _, clk, sink := obNewOutbox(t, nil)
		base := clk.Now()
		clk.Advance(20 * time.Minute)
		idx := uint64(1)
		apply := func(r OutboxRecord) {
			body, err := r.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if err := ob.Apply(wal.Committed{PrepareIndex: idx, CommitIndex: idx + 1, Entry: wal.Entry{Kind: OutboxRecordKind, Body: body}}); err != nil {
				t.Fatalf("Apply returned %v, want nil", err)
			}
			idx += 2
		}
		return ob, clk, sink, base, apply
	}
	mk := func(enq time.Time, size int, hash string, st OutboxState, settled time.Time, reason string) OutboxRecord {
		return OutboxRecord{
			JobID: jobID, PeerBusID: obPeerBus, OriginMessageID: msgID,
			Size: size, ContentSHA256: hash,
			EnqueuedAt: enq, State: st, SettledAt: settled, Reason: reason,
		}
	}

	t.Run("delivered settlement is applied and clamps its timestamp", func(t *testing.T) {
		ob, clk, sink, base, apply := newFixture(t)
		pendingAt := clk.Now()

		apply(mk(pendingAt, 11, obHash, OutboxPending, time.Time{}, ""))
		apply(mk(pendingAt, 22, obOtherHash, OutboxPending, time.Time{}, ""))
		// Settled at BASE — twenty minutes before the pending record it settles.
		apply(mk(base, 22, obOtherHash, OutboxDelivered, base.Add(time.Second), ""))

		if got := obPendingIDs(ob); len(got) != 0 {
			t.Fatalf("job %s is PENDING after its DELIVERED record was refused by the content check: %v. A settlement must never lose to a pending record — the peer receives this message a second time", jobID, got)
		}
		r, ok := ob.Lookup(jobID)
		if !ok || r.State != OutboxDelivered {
			t.Fatalf("job %s is (%+v, %v), want delivered", jobID, r, ok)
		}
		if r.ContentSHA256 != obHash || r.Size != 11 {
			t.Fatalf("the settled record carries content %s (%d bytes), want the FIRST content %s (11)", r.ContentSHA256, r.Size, obHash)
		}
		// THE CLAMP. The settlement's own timestamp is before the enqueue it is
		// attached to, so the merged entry would be one this package refuses to
		// write. It must be moved FORWARD to the enqueue instant, never back.
		if r.SettledAt.Before(r.EnqueuedAt) {
			t.Fatalf("settled_at %s is before enqueued_at %s; the merged record is one this package would refuse to write", r.SettledAt, r.EnqueuedAt)
		}
		if !r.SettledAt.Equal(r.EnqueuedAt) {
			t.Fatalf("settled_at is %s, want it clamped forward to enqueued_at %s", r.SettledAt, r.EnqueuedAt)
		}
		if _, err := r.Encode(); err != nil {
			t.Fatalf("the merged record does not Encode (%v); an entry the table holds must be one this package would write", err)
		}
		sink.mustLogAtLevel(t, "the contradicting settlement", "error", "names different content from the pending record it settles")
		// The clamp moved the stored timestamp away from the one in the durable
		// record, so the line has to say so — otherwise an operator reconciling
		// the table against the WAL finds two values and no explanation.
		sink.mustContain(t, "the clamp signal", "settled_at_clamped=true")
	})

	t.Run("abandoned settlement carries its reason into the log", func(t *testing.T) {
		ob, clk, sink, _, apply := newFixture(t)
		pendingAt := clk.Now()
		const why = "PEER-REFUSED-BEYOND-RETRY"

		apply(mk(pendingAt, 11, obHash, OutboxPending, time.Time{}, ""))
		apply(mk(pendingAt, 22, obOtherHash, OutboxPending, time.Time{}, ""))
		apply(mk(pendingAt, 22, obOtherHash, OutboxAbandoned, pendingAt.Add(time.Second), why))

		r, ok := ob.Lookup(jobID)
		if !ok || r.State != OutboxAbandoned {
			t.Fatalf("job %s is (%+v, %v), want abandoned", jobID, r, ok)
		}
		if r.Reason != why {
			t.Fatalf("the merged record carries reason %q, want %q: the settlement's own reason must survive the merge", r.Reason, why)
		}
		// The merge branch returns before the switch, so the ABANDONED warning
		// never fires for it. Invariant 6 wants the discard recorded
		// SPECIFICALLY, so the reason and the message id have to be on THIS line.
		// ASSERT THE KEYS, NOT JUST THE VALUES. msgID is a substring of jobID
		// (DeriveJobID concatenates them), so matching the bare value proved
		// nothing about origin_message_id being present — removing that key
		// entirely left this green.
		sink.mustContain(t, "the abandoned merge", why, jobID,
			"origin_message_id="+msgID, "reason=", "state=abandoned")
	})

	t.Run("a contradicting settlement cannot overwrite an existing tombstone", func(t *testing.T) {
		ob, clk, _, _, apply := newFixture(t)
		pendingAt := clk.Now()

		apply(mk(pendingAt, 11, obHash, OutboxPending, time.Time{}, ""))
		apply(mk(pendingAt, 11, obHash, OutboxDelivered, pendingAt.Add(time.Second), ""))
		// Now a mismatched ABANDONED record for a job that is already DELIVERED.
		// The merge branch must NOT take it: the first terminal record wins, and
		// without the existing-is-pending half of the guard this silently
		// rewrote a delivered job as abandoned.
		apply(mk(pendingAt, 22, obOtherHash, OutboxAbandoned, pendingAt.Add(2*time.Second), "too late"))

		r, ok := ob.Lookup(jobID)
		if !ok || r.State != OutboxDelivered {
			t.Fatalf("job %s is (%+v, %v), want the FIRST terminal record (delivered) to stand", jobID, r, ok)
		}
		if r.Reason != "" {
			t.Fatalf("the delivered record picked up reason %q from a contradicting abandonment", r.Reason)
		}
	})

	t.Run("two pending records still contradict", func(t *testing.T) {
		ob, clk, sink, _, apply := newFixture(t)
		pendingAt := clk.Now()
		apply(mk(pendingAt, 11, obHash, OutboxPending, time.Time{}, ""))
		apply(mk(pendingAt, 22, obOtherHash, OutboxPending, time.Time{}, ""))
		if r, ok := ob.Lookup(jobID); !ok || r.ContentSHA256 != obHash {
			t.Fatalf("the first content did not win between two pending records: %+v", r)
		}
		sink.mustContain(t, "the refused pending substitution", "DISCARDING an outbox record", jobID)
	})
}

// ---------------------------------------------------------------------------
// CRASH INJECTION — a real SIGKILL, twice
// ---------------------------------------------------------------------------
//
// "The code looks right" is not evidence for a durability claim, and neither is
// a graceful restart: the tests above close the log politely, so every deferred
// Close, buffer flush and runtime shutdown gets to run. A SIGKILL is the only
// thing that proves none of them was load-bearing.
//
// TWO crash points, because the outbox makes two promises and they fail in
// opposite directions:
//
//	enqueue-post-commit  the PENDING record is fsynced and the process dies
//	                     before Enqueue returns. Recovery must show the job
//	                     PENDING — otherwise "durable outbox" means nothing and
//	                     the relay hop is lost exactly as it is today.
//	settle-post-commit   the DELIVERED record is fsynced and the process dies
//	                     before Settle returns. Recovery must show the job
//	                     DELIVERED and the pending set EMPTY — otherwise the
//	                     crash resurrects a delivered message and the peer gets
//	                     it twice.

const (
	// envOutboxCrashPoint selects where the child kills itself. Unset means
	// "not a crash child", which is what makes the child test a no-op skip in a
	// normal run of the suite.
	envOutboxCrashPoint = "RELAY_OUTBOX_CRASH_POINT"
	// envOutboxCrashDir is the data directory the child writes into: a
	// t.TempDir() belonging to the parent, so no test shares a data directory
	// with another and the tracked data/ dir is never touched.
	envOutboxCrashDir = "RELAY_OUTBOX_CRASH_DIR"

	obCrashEnqueue = "enqueue-post-commit-pre-ack"
	obCrashSettle  = "settle-post-commit-pre-ack"

	// obCrashSeq is the origin sequence the parent and child must agree on.
	obCrashSeq = 4242
)

// obKillOnState delegates to the REAL *wal.Log.Write — the whole prepare,
// commit and fsync cycle — and kills the process before returning, but only for
// the record whose state the crash point is about. Everything before that is
// written normally, so the crash point is unambiguous: nothing at all runs
// between the commit fsync and the SIGKILL.
type obKillOnState struct {
	l    *wal.Log
	want OutboxState
}

func (k *obKillOnState) Write(e wal.Entry) (wal.Committed, error) {
	// Asserted HERE rather than in the parent because this is the only place
	// the entry the outbox built can be seen BEFORE it is written. If the
	// outbox handed the log anything else, the parent's "the record is durable"
	// assertion would be examining bytes that never meant what it thinks.
	if e.Kind != OutboxRecordKind {
		return wal.Committed{}, fmt.Errorf("child: the outbox handed the durable log an entry of kind %q, want %q", e.Kind, OutboxRecordKind)
	}
	rec, err := DecodeOutboxRecord(e.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the entry the outbox handed the durable log does not decode as an outbox record: %v", err)
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	if rec.State == k.want {
		obKillSelf()
	}
	return c, nil
}

// obKillSelf kills this process with SIGKILL. SIGKILL cannot be caught, blocked
// or ignored, so nothing deferred, buffered or graceful runs afterwards — which
// is the entire evidentiary value of this test over a polite Close.
func obKillSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking
	// rather than looping means a platform where that is somehow untrue fails
	// loudly instead of hanging the suite.
	panic("relay outbox crash test: SIGKILL to self did not kill the process")
}

// TestOutboxCrashChild is the child half. It does NOTHING in a normal run.
func TestOutboxCrashChild(t *testing.T) {
	point := os.Getenv(envOutboxCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envOutboxCrashPoint + " is unset")
	}
	dir := os.Getenv(envOutboxCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is unset", envOutboxCrashPoint, point, envOutboxCrashDir)
	}

	// NO deferred Close and NO t.Cleanup on the log: a Close that ran would be
	// exactly the graceful shutdown this test exists to rule out.
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("child: wal.Open(%s): %v", dir, err)
	}

	var killOn OutboxState
	switch point {
	case obCrashEnqueue:
		killOn = OutboxPending
	case obCrashSettle:
		killOn = OutboxDelivered
	default:
		t.Fatalf("child: unknown crash point %q", point)
	}

	ob, err := NewOutbox(OutboxOptions{BusID: obLocalBus, Durable: &obKillOnState{l: lg, want: killOn}})
	if err != nil {
		t.Fatalf("child: NewOutbox: %v", err)
	}
	// Rebuilt by a read-only replay of the log's own path — the wiring the
	// server uses, and the one that keeps the kill point unambiguous.
	if _, err := wal.Replay(lg.Path(), ob.Apply); err != nil {
		t.Fatalf("child: replaying %s: %v", lg.Path(), err)
	}

	job, err := ob.Enqueue(obJob(t, obCrashSeq))
	if point == obCrashEnqueue {
		t.Fatalf("child: Enqueue returned (%+v, %v) but the durable log kills this process the instant the PENDING commit is fsynced; the crash was never injected", job, err)
	}
	if err != nil {
		t.Fatalf("child: Enqueue: %v", err)
	}
	got, err := ob.Settle(job.JobID, OutboxDelivered, "")
	t.Fatalf("child: Settle returned (%+v, %v) but the durable log kills this process the instant the DELIVERED commit is fsynced; the crash was never injected", got, err)
}

// obRunCrashChild re-execs this test binary at the given crash point and
// asserts the child really DIED ON SIGKILL rather than failing its own
// assertions — without that check a broken child would silently turn the parent
// into a test of a directory nothing was ever written to.
func obRunCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}

	// Bounded, so a wedged child fails this test in a minute rather than
	// hanging the suite until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestOutboxCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envOutboxCrashPoint+"="+point,
		envOutboxCrashDir+"="+dir,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so
	// "err != nil" is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// TestOutboxSurvivesACrashMidEnqueue: the promise "this bus owes that peer a
// message" is on stable storage before anything is told the enqueue succeeded.
func TestOutboxSurvivesACrashMidEnqueue(t *testing.T) {
	dir := t.TempDir()
	obRunCrashChild(t, obCrashEnqueue, dir)

	committed := obReplayCommitted(t, dir)
	if len(committed) != 1 {
		t.Fatalf("the crashed log holds %d committed entries, want exactly 1 (the pending record): the child died before its enqueue was durable, so there is no post-commit crash to recover from", len(committed))
	}
	recs := obRecordsIn(t, committed)
	if recs[0].State != OutboxPending {
		t.Fatalf("the committed record is %s, want pending", recs[0].State)
	}
	wantID := DeriveJobID(obPeerBus, obMessageID(t, obCrashSeq))
	if recs[0].JobID != wantID {
		t.Fatalf("the committed record names job %s, want %s", recs[0].JobID, wantID)
	}

	ob, lg, _ := obOpen(t, dir, nil)
	defer func() { _ = lg.Close() }()
	if got := lg.Recovered().Applied; got != 1 {
		t.Fatalf("recovery applied %d committed entries, want 1", got)
	}
	pending := ob.Pending()
	if len(pending) != 1 || pending[0].JobID != wantID {
		t.Fatalf("after a SIGKILL mid-enqueue the pending set is %v, want exactly [%s]; a process killed with -9 flushes nothing, so this state has to come off the durable log",
			obPendingIDs(ob), wantID)
	}
	if pending[0].ContentSHA256 != obHash || pending[0].Size != 11 {
		t.Fatalf("the recovered job describes content %s (%d bytes), want %s (11)", pending[0].ContentSHA256, pending[0].Size, obHash)
	}

	// The client's retry — the same job, enqueued again — is ANSWERED with the
	// original record and writes nothing. The crash is exactly the moment a
	// well-behaved caller retries, and punishing that would break the callers
	// doing the right thing (invariant 10).
	again, err := ob.Enqueue(obJob(t, obCrashSeq))
	if err != nil {
		t.Fatalf("re-enqueueing after the crash gave err = %v, want the original record", err)
	}
	if again.JobID != wantID || !again.EnqueuedAt.Equal(pending[0].EnqueuedAt) {
		t.Fatalf("the retry produced a DIFFERENT job (%+v); it must find the one the crash left behind", again)
	}
	if after := obReplayCommitted(t, dir); len(after) != 1 {
		t.Fatalf("the retry wrote a second record; the log now holds %d committed entries, want 1", len(after))
	}
}

// TestOutboxSettlementSurvivesACrash is the more important half: a crash the
// instant a delivery is recorded must NOT resurrect the job. If it does, the
// peer receives the message a second time, and the durable outbox has turned
// at-least-once delivery into at-least-twice.
func TestOutboxSettlementSurvivesACrash(t *testing.T) {
	dir := t.TempDir()
	obRunCrashChild(t, obCrashSettle, dir)

	committed := obReplayCommitted(t, dir)
	if len(committed) != 2 {
		t.Fatalf("the crashed log holds %d committed entries, want exactly 2 (pending then delivered): the child died before its settlement was durable", len(committed))
	}
	recs := obRecordsIn(t, committed)
	wantID := DeriveJobID(obPeerBus, obMessageID(t, obCrashSeq))
	if recs[0].State != OutboxPending || recs[1].State != OutboxDelivered {
		t.Fatalf("the committed states are %s then %s, want pending then delivered", recs[0].State, recs[1].State)
	}
	if recs[1].JobID != wantID || recs[1].JobID != recs[0].JobID {
		t.Fatalf("the settlement names job %s and the enqueue names %s, want both to be %s", recs[1].JobID, recs[0].JobID, wantID)
	}
	if recs[1].SettledAt.IsZero() {
		t.Fatalf("the settlement has no settled_at, so tombstone retention could never fire on it")
	}

	ob, lg, _ := obOpen(t, dir, nil)
	defer func() { _ = lg.Close() }()
	if got := lg.Recovered().Applied; got != 2 {
		t.Fatalf("recovery applied %d committed entries, want 2", got)
	}
	if got := obPendingIDs(ob); len(got) != 0 {
		t.Fatalf("after a SIGKILL the instant a delivery was recorded, the pending set is %v, want EMPTY: the crash resurrected a delivered job and the peer will receive the message twice", got)
	}
	r, ok := ob.Lookup(wantID)
	if !ok || r.State != OutboxDelivered {
		t.Fatalf("job %s recovered as (%+v, %v), want delivered", wantID, r, ok)
	}
	if _, err := ob.Enqueue(obJob(t, obCrashSeq)); !errors.Is(err, ErrOutboxSettled) {
		t.Fatalf("re-enqueueing the delivered job after the crash gave err = %v, want ErrOutboxSettled", err)
	}

	// And it is still delivered after ANOTHER restart: recovery from a crashed
	// log has to be a fixed point, not a one-off.
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the recovered log: %v", err)
	}
	ob2, lg2, _ := obOpen(t, dir, nil)
	defer func() { _ = lg2.Close() }()
	if r, ok := ob2.Lookup(wantID); !ok || r.State != OutboxDelivered {
		t.Fatalf("after a clean restart following the crash, job %s is (%+v, %v), want delivered", wantID, r, ok)
	}
}
