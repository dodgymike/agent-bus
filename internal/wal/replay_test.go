package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// Hand-built logs.
//
// Replay is fed shapes that Log cannot serialise -- interleaved transactions, a
// commit that names a commit, a double commit -- so the tests below write the
// records THEMSELVES with a raw Writer and the package's own payload codecs.
// Going through the real encoders keeps these logs byte-compatible with what
// the write path produces: only the ORDER of records is synthetic, never the
// framing or the payloads.
//
// One walOp is exactly one record, so the ops are numbered 1..N and a commit or
// abort references a prepare by that number.
// ---------------------------------------------------------------------------

type walOp struct {
	typ    Type
	kind   string // prepare
	body   string // prepare; "" means a nil body, which encodes as null
	ref    uint64 // commit/abort: the record index of the prepare
	reason string // abort
	raw    []byte // when non-nil, appended verbatim as typ (for record types with no codec)
}

func opPrepare(kind, body string) walOp { return walOp{typ: TypePrepare, kind: kind, body: body} }

func opCommit(prepareIndex uint64) walOp { return walOp{typ: TypeCommit, ref: prepareIndex} }

func opAbort(prepareIndex uint64, reason string) walOp {
	return walOp{typ: TypeAbort, ref: prepareIndex, reason: reason}
}

// opRaw appends a record of an arbitrary type with an arbitrary payload. It is
// how a test puts a record in a WAL that the write path would never write.
func opRaw(t Type, payload string) walOp { return walOp{typ: t, raw: []byte(payload)} }

// buildWAL writes ops into a fresh temp dir as a well-framed WAL and returns
// the directory, the file (named WALFileName so wal.Open can be pointed at the
// directory too), and what the writer says the next index and end offset are.
func buildWAL(t *testing.T, ops ...walOp) (dir, path string, nextIndex uint64, endOffset int64) {
	t.Helper()
	return buildWALIn(t, t.TempDir(), ops...)
}

// shortTempDir is t.TempDir with a SHORT name.
//
// It exists for one reason and it is a real one: a discard REASON wraps the
// underlying error, and that error names the file, and the whole reason is then
// bounded to maxCauseChars (160) before it reaches an operator. t.TempDir()
// embeds the full test name, so a path here can run to 120 characters and push
// the actual diagnosis -- "prepare payload has an empty kind" -- past the bound,
// making the test a measurement of Go's temp-directory naming rather than of the
// code. A production path (/data/bus.wal) is short, so the short directory is
// the realistic fixture, not a convenience.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// buildWALIn is buildWAL into a caller-chosen directory.
func buildWALIn(t *testing.T, dir string, ops ...walOp) (_, path string, nextIndex uint64, endOffset int64) {
	t.Helper()
	path = filepath.Join(dir, WALFileName)

	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter(%s): %v", path, err)
	}
	base := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

	for i, op := range ops {
		var payload []byte
		var err error
		switch {
		case op.raw != nil:
			payload = op.raw
		case op.typ == TypePrepare:
			payload, err = encodePrepare(op.kind, jsonBody(op.body), base.Add(time.Duration(i)*time.Second))
		case op.typ == TypeCommit:
			payload, err = encodeCommit(op.ref)
		case op.typ == TypeAbort:
			payload, err = encodeAbort(op.ref, op.reason)
		default:
			t.Fatalf("buildWAL: op %d has type %s and no raw payload", i+1, op.typ)
		}
		if err != nil {
			_ = w.Close()
			t.Fatalf("buildWAL: encoding op %d (%s): %v", i+1, op.typ, err)
		}
		if _, err := w.Append(op.typ, payload); err != nil {
			_ = w.Close()
			t.Fatalf("buildWAL: appending op %d (%s): %v", i+1, op.typ, err)
		}
	}

	nextIndex, endOffset = w.NextIndex(), w.Size()
	if err := w.Close(); err != nil {
		t.Fatalf("buildWAL: Close: %v", err)
	}
	return dir, path, nextIndex, endOffset
}

// jsonBody turns a test's body literal into an Entry body; "" means nil.
func jsonBody(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// wantC builds an expected delivered entry. body "" means the nil body.
func wantC(prepareIndex, commitIndex uint64, kind, body string) Committed {
	return Committed{
		PrepareIndex: prepareIndex,
		CommitIndex:  commitIndex,
		Entry:        Entry{Kind: kind, Body: jsonBody(body)},
	}
}

// collector is the fn handed to Replay: it records the delivered stream in the
// order Replay produced it.
type collector struct {
	got []Committed
}

func (c *collector) fn(e Committed) error {
	c.got = append(c.got, e)
	return nil
}

// sameCommitted compares two delivered streams for the fields that are accepted
// history: the two indices, the kind, and the body BYTE FOR BYTE -- including
// the nil/empty distinction, because "no body" and "an empty body" are
// different entries.
func sameCommitted(a, b []Committed) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PrepareIndex != b[i].PrepareIndex || a[i].CommitIndex != b[i].CommitIndex {
			return false
		}
		if a[i].Entry.Kind != b[i].Entry.Kind {
			return false
		}
		if (a[i].Entry.Body == nil) != (b[i].Entry.Body == nil) {
			return false
		}
		if !bytes.Equal(a[i].Entry.Body, b[i].Entry.Body) {
			return false
		}
	}
	return true
}

// showCommitted renders a delivered stream so a mismatch prints something a
// human can read.
func showCommitted(cs []Committed) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		body := "nil"
		if c.Entry.Body != nil {
			body = string(c.Entry.Body)
		}
		parts = append(parts, fmt.Sprintf("{prepare:%d commit:%d kind:%q body:%s}",
			c.PrepareIndex, c.CommitIndex, c.Entry.Kind, body))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	return fi.Size()
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestWALReplayEmptyLog pins the two shapes that are an EMPTY log rather than
// corruption: a file that does not exist, and a zero-length file (the crash
// window between creating the file and writing its header). Neither can contain
// a record, so nothing that was ever acknowledged is lost by starting fresh --
// and OpenWriter heals the zero-length case the same way, so the two must
// agree.
func TestWALReplayEmptyLog(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"missing file", func(t *testing.T, path string) {}},
		{"zero-length file", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatalf("creating a zero-length WAL: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("closing the zero-length WAL: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, WALFileName)
			tc.setup(t, path)

			var c collector
			r, err := Replay(path, c.fn)
			if err != nil {
				t.Fatalf("Replay of an empty log: %v", err)
			}
			if len(c.got) != 0 {
				t.Errorf("Replay delivered %s, want nothing", showCommitted(c.got))
			}
			if r.NextIndex != 1 {
				t.Errorf("NextIndex = %d, want 1", r.NextIndex)
			}
			if r.EndOffset != 0 {
				t.Errorf("EndOffset = %d, want 0: an empty log has no header to sit after", r.EndOffset)
			}
			if r.Records != 0 || r.Applied != 0 || r.Aborted != 0 || len(r.Dangling) != 0 {
				t.Errorf("Recovered = %+v, want zero records/applied/aborted/dangling", r)
			}
			if r.Path != path {
				t.Errorf("Path = %q, want %q", r.Path, path)
			}

			// Open agrees: an empty log starts, and starts at index 1.
			l, err := Open(LogOptions{Dir: dir})
			if err != nil {
				t.Fatalf("Open on an empty log: %v", err)
			}
			defer l.Close()
			if got := l.Recovered().NextIndex; got != 1 {
				t.Errorf("Log.Recovered().NextIndex = %d, want 1", got)
			}
		})
	}
}

// TestWALReplayResolvesTransactions is the core semantics table: which records
// survive replay, and in what order. It deliberately includes INTERLEAVED
// transactions -- shapes Log's serialised write path cannot produce -- because
// the matching rule is "a commit names its prepare BY INDEX", and a replay that
// quietly relied on adjacency would pass every non-interleaved case and lose an
// acknowledged write the first time the write path ever changed.
func TestWALReplayResolvesTransactions(t *testing.T) {
	cases := []struct {
		name          string
		ops           []walOp
		want          []Committed
		wantRecords   uint64
		wantApplied   uint64
		wantAborted   uint64
		wantDangling  []uint64
		wantNextIndex uint64
	}{
		{
			name: "prepare then commit is accepted history",
			ops:  []walOp{opPrepare("message", `{"n":1}`), opCommit(1)},
			want: []Committed{wantC(1, 2, "message", `{"n":1}`)},
			// NextIndex is 3: two records were written, so the next append is 3.
			wantRecords: 2, wantApplied: 1, wantNextIndex: 3,
		},
		{
			name: "a nil body survives as nil",
			ops:  []walOp{opPrepare("agent", ""), opCommit(1)},
			want: []Committed{wantC(1, 2, "agent", "")},
			// A null body must come back nil, not []byte{}, so a replayed Apply
			// sees the same bytes a live Apply saw.
			wantRecords: 2, wantApplied: 1, wantNextIndex: 3,
		},
		{
			name: "prepare with no commit is discarded and reported",
			ops:  []walOp{opPrepare("message", `{"n":1}`)},
			want: nil,
			// The ordinary crash-between-fsyncs shape: durable, never
			// acknowledged, therefore never visible.
			wantRecords: 1, wantDangling: []uint64{1}, wantNextIndex: 2,
		},
		{
			name: "prepare then abort is discarded",
			ops:  []walOp{opPrepare("message", `{"n":1}`), opAbort(1, "recipient unknown")},
			want: nil,
			// The abort record is the durable statement that no commit is coming.
			wantRecords: 2, wantAborted: 1, wantNextIndex: 3,
		},
		{
			name: "interleaved transactions are delivered in COMMIT order",
			ops: []walOp{
				opPrepare("message", `{"n":1}`), // 1
				opPrepare("message", `{"n":2}`), // 2
				opCommit(2),                     // 3 -- the SECOND prepare commits first
				opCommit(1),                     // 4
			},
			// Commit order, not prepare order: entry 2 first.
			want: []Committed{
				wantC(2, 3, "message", `{"n":2}`),
				wantC(1, 4, "message", `{"n":1}`),
			},
			wantRecords: 4, wantApplied: 2, wantNextIndex: 5,
		},
		{
			name: "interleaved, only the inner transaction commits",
			ops: []walOp{
				opPrepare("message", `{"n":1}`), // 1 -- never resolved
				opPrepare("message", `{"n":2}`), // 2
				opCommit(2),                     // 3
			},
			want:        []Committed{wantC(2, 3, "message", `{"n":2}`)},
			wantRecords: 3, wantApplied: 1, wantDangling: []uint64{1}, wantNextIndex: 4,
		},
		{
			name: "an abort does not stop the next transaction committing",
			ops: []walOp{
				opPrepare("message", `{"n":1}`), // 1
				opAbort(1, "rejected"),          // 2
				opPrepare("message", `{"n":2}`), // 3
				opCommit(3),                     // 4
			},
			want:        []Committed{wantC(3, 4, "message", `{"n":2}`)},
			wantRecords: 4, wantApplied: 1, wantAborted: 1, wantNextIndex: 5,
		},
		{
			name: "a commit far from its prepare still matches by index",
			ops: []walOp{
				opPrepare("message", `{"n":1}`), // 1 -- committed LAST, five records later
				opPrepare("agent", `{"id":"a"}`),
				opCommit(2),
				opPrepare("message", `{"n":3}`),
				opAbort(4, "gone"),
				opCommit(1),
			},
			want: []Committed{
				wantC(2, 3, "agent", `{"id":"a"}`),
				wantC(1, 6, "message", `{"n":1}`),
			},
			wantRecords: 6, wantApplied: 2, wantAborted: 1, wantNextIndex: 7,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, path, builtNext, builtEnd := buildWAL(t, tc.ops...)

			var c collector
			r, err := Replay(path, c.fn)
			if err != nil {
				t.Fatalf("Replay: %v", err)
			}
			if !sameCommitted(c.got, tc.want) {
				t.Fatalf("delivered %s, want %s", showCommitted(c.got), showCommitted(tc.want))
			}
			if r.Records != tc.wantRecords {
				t.Errorf("Records = %d, want %d", r.Records, tc.wantRecords)
			}
			if r.Applied != tc.wantApplied {
				t.Errorf("Applied = %d, want %d", r.Applied, tc.wantApplied)
			}
			if r.Aborted != tc.wantAborted {
				t.Errorf("Aborted = %d, want %d", r.Aborted, tc.wantAborted)
			}
			if !reflect.DeepEqual(r.Dangling, tc.wantDangling) {
				t.Errorf("Dangling = %v, want %v", r.Dangling, tc.wantDangling)
			}
			if r.NextIndex != tc.wantNextIndex {
				t.Errorf("NextIndex = %d, want %d", r.NextIndex, tc.wantNextIndex)
			}
			// The high-water mark and the append offset must agree with what the
			// writer that BUILT the file thinks, or a reopen would either reuse an
			// index or write into the middle of the file.
			if r.NextIndex != builtNext {
				t.Errorf("NextIndex = %d, but the writer that built the file says %d", r.NextIndex, builtNext)
			}
			if r.EndOffset != builtEnd {
				t.Errorf("EndOffset = %d, but the writer that built the file ended at %d", r.EndOffset, builtEnd)
			}
			if got := fileSize(t, path); r.EndOffset != got {
				t.Errorf("EndOffset = %d, want the file size %d after a clean replay", r.EndOffset, got)
			}

			// The same log validated with fn == nil (the "cheap fsck" claim) must
			// report exactly the same thing.
			fsck, err := Replay(path, nil)
			if err != nil {
				t.Fatalf("Replay with a nil fn: %v", err)
			}
			if !reflect.DeepEqual(fsck, r) {
				t.Errorf("Replay(nil) = %+v, want the same Recovered as Replay(fn) = %+v", fsck, r)
			}
		})
	}
}

// TestWALReplayHighWaterMarkSurvivesDiscard is the id-authority proof for
// recovery (invariant 1): an index burned by a prepare that is DISCARDED is
// still burned. Reissuing it would let two different messages share an id, and
// the discarded one may already have been referred to somewhere.
func TestWALReplayHighWaterMarkSurvivesDiscard(t *testing.T) {
	const burned = 3 // the third record is a prepare that never commits
	dir, path, builtNext, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`),
		opCommit(1),
		opPrepare("message", `{"n":2}`),
	)

	r, err := Replay(path, nil)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !reflect.DeepEqual(r.Dangling, []uint64{burned}) {
		t.Fatalf("Dangling = %v, want [%d]", r.Dangling, burned)
	}
	if r.NextIndex != burned+1 {
		t.Fatalf("NextIndex = %d, want %d: a discarded prepare still burns its index", r.NextIndex, burned+1)
	}
	if r.NextIndex != builtNext {
		t.Fatalf("NextIndex = %d, but the writer that built the file says %d", r.NextIndex, builtNext)
	}

	// Now do it for real: reopen the Log and write. The new prepare must land
	// PAST the burned index, and the discarded entry must not be visible.
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("Open after a dangling prepare: %v", err)
	}
	defer l.Close()

	if app.count() != 1 {
		t.Fatalf("Apply called %d times on reopen, want 1: only the committed entry is accepted history", app.count())
	}
	if got := app.at(0); got.PrepareIndex != 1 || got.CommitIndex != 2 {
		t.Fatalf("replayed entry is {%d,%d}, want {1,2}", got.PrepareIndex, got.CommitIndex)
	}

	c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":3}`)})
	if err != nil {
		t.Fatalf("Write after recovery: %v", err)
	}
	if c.PrepareIndex <= burned {
		t.Fatalf("the write after recovery got prepare index %d, which reuses or precedes the burned index %d",
			c.PrepareIndex, burned)
	}
	if c.PrepareIndex != burned+1 {
		t.Errorf("the write after recovery got prepare index %d, want %d (no gap, no reuse)", c.PrepareIndex, burned+1)
	}

	// Nothing anywhere in the file repeats an index.
	recs, shape := scanTypes(t, path)
	seen := make(map[uint64]bool, len(recs))
	last := uint64(0)
	for _, rec := range recs {
		if seen[rec.Index] {
			t.Fatalf("index %d appears twice in %s", rec.Index, shape)
		}
		if rec.Index <= last {
			t.Fatalf("index %d follows %d in %s: indices must be strictly increasing", rec.Index, last, shape)
		}
		seen[rec.Index] = true
		last = rec.Index
	}
	if last != burned+2 { // burned+1 prepare, burned+2 commit
		t.Errorf("last index in %s is %d, want %d", shape, last, burned+2)
	}
}

// TestWALReplayDiscardsBadReferences pins what happens to a COMMIT or ABORT that
// does not name an OPEN prepare.
//
// WHAT THIS TEST USED TO ASSERT: a refusal to start, with nothing after the bad
// record delivered, on the reasoning that a delivered suffix past unexplained
// damage is how a state that is not a prefix of accepted history gets served.
// The user reversed that (DECISIONS.md, 2026-08-02, "Availability over
// retention"): "always be able to restart, prefer to discard messages and/or
// corruption, with logging". So the record is DISCARDED and the replay carries
// on, and the recovered state is accepted history MINUS the discarded records
// rather than a prefix of it.
//
// The honesty that replaced the refusal is asserted here in full:
//
//   - the loss is reported in Recovered.Discarded, with the record's offset,
//     index and type, and a reason that says an acknowledged write may be gone;
//   - Open LOGS it -- at ERROR for a commit, because a commit record means a
//     client was told the write was durable, and WARN for an abort, which
//     acknowledged nothing;
//   - and the records AFTER the damage, which are perfectly good, are recovered
//     rather than thrown away with it.
func TestWALReplayDiscardsBadReferences(t *testing.T) {
	// Every case ends with a well-formed prepare/commit pair (records 4 and 5).
	// Under the old policy they had to be withheld; now they are the anti-cascade
	// assertion -- one unusable record must not cost the transaction behind it.
	tail := []walOp{opPrepare("message", `{"n":9}`), opCommit(4)}
	tailEntry := wantC(4, 5, "message", `{"n":9}`)

	cases := []struct {
		name string
		ops  []walOp
		// badIdx is the 1-based record index of the record replay must discard.
		badIdx int
		// want is what survives, NOT counting the trailing transaction, which
		// every case recovers.
		want      []Committed
		wantType  Type
		wantLevel string
		wantMsg   string
	}{
		{
			name: "commit names a commit record",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opCommit(1),                     // 2
				opCommit(2),                     // 3 -- record 2 is not a prepare
			}, tail...),
			badIdx:    3,
			want:      []Committed{wantC(1, 2, "message", `{"n":1}`)},
			wantType:  TypeCommit,
			wantLevel: "ERROR",
			wantMsg:   "not an open prepare",
		},
		{
			name: "the same prepare is committed twice",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opCommit(1),                     // 2
				opCommit(1),                     // 3 -- already committed
			}, tail...),
			badIdx:    3,
			want:      []Committed{wantC(1, 2, "message", `{"n":1}`)},
			wantType:  TypeCommit,
			wantLevel: "ERROR",
			wantMsg:   "not an open prepare",
		},
		{
			name: "abort of an already committed prepare",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opCommit(1),                     // 2
				opAbort(1, "too late"),          // 3
			}, tail...),
			badIdx: 3,
			want:   []Committed{wantC(1, 2, "message", `{"n":1}`)},
			// An ABORT acknowledged nothing, so losing one is WARN, not ERROR.
			// The level is the whole difference between "we tidied something up"
			// and "a client was lied to", and it is part of the contract.
			wantType:  TypeAbort,
			wantLevel: "WARN",
			wantMsg:   "not an open prepare",
		},
		{
			name: "commit of an already aborted prepare",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opAbort(1, "rejected"),          // 2
				opCommit(1),                     // 3
			}, tail...),
			badIdx:    3,
			want:      nil,
			wantType:  TypeCommit,
			wantLevel: "ERROR",
			wantMsg:   "not an open prepare",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, path, _, _ := buildWAL(t, tc.ops...)

			// The FRAMING is fine -- this is a semantic failure -- so ScanAll
			// succeeds and gives the exact extent of the record that is discarded.
			recs, cleanEnd, err := ScanAll(path, KindWAL)
			if err != nil {
				t.Fatalf("the test log is not well framed: %v", err)
			}
			bad := recs[tc.badIdx-1]

			var c collector
			r, err := Replay(path, c.fn)
			if err != nil {
				t.Fatalf("Replay err = %v, want none: a record replay cannot interpret is discarded, not fatal", err)
			}

			want := append(append([]Committed{}, tc.want...), tailEntry)
			if !sameCommitted(c.got, want) {
				t.Fatalf("delivered %s, want %s: the trailing transaction is intact and must not be lost with the record in front of it",
					showCommitted(c.got), showCommitted(want))
			}
			if r.EndOffset != cleanEnd {
				t.Errorf("EndOffset = %d, want the end of the file %d: replay read the whole log", r.EndOffset, cleanEnd)
			}

			// EXACTLY what was thrown away, and why.
			if r.DiscardCount != 1 {
				t.Fatalf("Recovered.DiscardCount = %d, want exactly 1 (%+v)", r.DiscardCount, r.Discarded)
			}
			d := r.Discarded[0]
			if d.Stage != "replay" || d.Offset != bad.Offset || d.Index != uint64(tc.badIdx) || d.Type != tc.wantType || !d.TypeKnown {
				t.Errorf("Recovered.Discarded[0] = %+v, want the replay-stage loss of %s record %d at offset %d",
					d, tc.wantType, tc.badIdx, bad.Offset)
			}
			if d.Length != bad.frameSize() {
				t.Errorf("the discard reports %d bytes, want the record's frame size %d", d.Length, bad.frameSize())
			}
			if !strings.Contains(d.Reason, tc.wantMsg) {
				t.Errorf("the discard reason is %q, want it to contain %q", d.Reason, tc.wantMsg)
			}
			if tc.wantType == TypeCommit && !strings.Contains(d.Reason, "an acknowledged write is lost here") {
				t.Errorf("the discard reason for a lost COMMIT is %q, want it to say an acknowledged write is gone: "+
					"that is the fact the availability decision made it this code's job to report", d.Reason)
			}

			// And a Log STARTS on it, and says what it lost.
			got, rec, out, err := openCapturing(t, dir)
			if err != nil {
				t.Fatalf("Open on a log with one unusable record: %v: recovery must always reach a running server", err)
			}
			if !sameCommitted(got, want) {
				t.Fatalf("Open applied %s, want %s", showCommitted(got), showCommitted(want))
			}
			if rec.DiscardCount != 1 {
				t.Errorf("Recovered().DiscardCount = %d, want 1", rec.DiscardCount)
			}
			assertLogged(t, out, tc.wantLevel, "wal discarded a damaged record",
				"path="+path, "stage=replay",
				"record_index="+strconv.Itoa(tc.badIdx),
				"record_type="+tc.wantType.String(),
				tc.wantMsg)
		})
	}
}

// TestWALReplayDiscardsUnknownRecordType pins where forward compatibility ends.
//
// scanFrom accepts a record whose type it does not know, because the checksum
// proves some writer meant those exact bytes. Replay cannot: a record whose
// effect on accepted history is unknown cannot be INTERPRETED, and pretending it
// is a no-op would be indistinguishable from losing whatever it recorded. An
// audit record in a WAL is the same story -- audit records live in the audit
// file, so one here means these are not the bytes we think they are.
//
// Until 2026-08-02 that made Replay fail. Under "always be able to restart,
// prefer to discard messages and/or corruption, with logging" it is discarded
// instead -- and the two facts that make that acceptable are asserted here: the
// record's INDEX IS STILL BURNED (an id that was on stable storage is never
// handed out again, invariant 1), and the loss is on the operator's log with the
// unusable type named in it.
func TestWALReplayDiscardsUnknownRecordType(t *testing.T) {
	cases := []struct {
		name    string
		bad     walOp
		wantMsg string
	}{
		{"audit record in a WAL", opRaw(TypeAuditMessage, `{"message_id":"m1"}`), "audit_message"},
		{"a record type from the future", walOp{typ: Type(4242), raw: []byte(`{}`)}, "unknown(4242)"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ops := []walOp{opPrepare("message", `{"n":1}`), opCommit(1)}
			var dir, path string
			if tc.bad.typ.Known() {
				dir, path, _, _ = buildWAL(t, append(ops, tc.bad)...)
			} else {
				// Writer.Append refuses an unknown type on purpose, so the frame
				// is assembled and appended by hand. The framing is valid -- only
				// the type is unrecognised.
				dir, path, _, _ = buildWAL(t, ops...)
				appendRawFrame(t, path, 3, tc.bad.typ, tc.bad.raw)
			}

			// The frame is intact as far as the scanner is concerned.
			recs, _, err := ScanAll(path, KindWAL)
			if err != nil {
				t.Fatalf("ScanAll rejected the record before Replay could: %v", err)
			}

			var c collector
			r, err := Replay(path, c.fn)
			if err != nil {
				t.Fatalf("Replay err = %v, want none: an uninterpretable record is discarded, not fatal", err)
			}
			want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
			if !sameCommitted(c.got, want) {
				t.Fatalf("delivered %s, want %s", showCommitted(c.got), showCommitted(want))
			}
			if r.DiscardCount != 1 {
				t.Fatalf("Recovered.DiscardCount = %d, want 1 (%+v)", r.DiscardCount, r.Discarded)
			}
			d := r.Discarded[0]
			if d.Stage != "replay" || d.Index != 3 || d.Offset != recs[2].Offset {
				t.Errorf("Recovered.Discarded[0] = %+v, want the replay-stage loss of record 3 at offset %d", d, recs[2].Offset)
			}
			if !strings.Contains(d.Reason, tc.wantMsg) {
				t.Errorf("the discard reason is %q, want it to name the record type %q", d.Reason, tc.wantMsg)
			}
			if !strings.Contains(d.Reason, "have no meaning in a write-ahead log") {
				t.Errorf("the discard reason is %q, want it to say why the record could not be interpreted", d.Reason)
			}
			// THE ID IS STILL BURNED. Index 3 was on stable storage; reissuing it
			// would let two different records share an id (invariant 1), and that
			// rule does not bend for a record recovery could not read.
			if r.NextIndex != 4 {
				t.Errorf("NextIndex = %d, want 4: the unreadable record still burned index 3", r.NextIndex)
			}

			// A server starts on it and says what it could not read.
			_, rec, out, err := openCapturing(t, dir)
			if err != nil {
				t.Fatalf("Open: %v: recovery must always reach a running server", err)
			}
			if rec.NextIndex != 4 || rec.DiscardCount != 1 {
				t.Errorf("Recovered() = {NextIndex:%d DiscardCount:%d}, want {4 1}", rec.NextIndex, rec.DiscardCount)
			}
			assertLogged(t, out, "WARN", "wal discarded a damaged record",
				"path="+path, "stage=replay", "record_index=3", tc.wantMsg)
		})
	}
}

// appendRawFrame appends one hand-assembled frame to an existing log. It exists
// only to write records Writer.Append refuses, and it does no fsync because the
// test process is the only reader.
func appendRawFrame(t *testing.T, path string, index uint64, typ Type, payload []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatalf("OpenFile(%s) to append a raw frame: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(testCodec(t, path).encodeFrame(index, typ, payload)); err != nil {
		t.Fatalf("appending a raw frame: %v", err)
	}
}

// TestWALReplayTornTail documents the DUR-3/DUR-4 boundary. A tail cut mid-frame
// is an ERROR in Replay, not a tolerated condition: Replay says precisely where
// the file stops making sense (EndOffset) and never truncates anything. The
// policy question of whether that tail may be cut belongs to RepairTail (DUR-4),
// which Open runs FIRST -- so the same bytes that fail a bare Replay make a
// successful, repaired Open.
func TestWALReplayTornTail(t *testing.T) {
	dir, path, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4 -- this frame gets cut in half
	)

	recs, cleanEnd, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("the test log is not well framed before truncation: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("built %d records, want 4", len(recs))
	}
	// The offset just past the last INTACT record is where the final frame
	// starts. Computed, never hardcoded: the frame sizes are payload-dependent.
	lastFrameStart := recs[len(recs)-1].Offset
	lastFrameSize := cleanEnd - lastFrameStart
	if lastFrameSize < 2 {
		t.Fatalf("the last frame is %d bytes: nothing to cut in half", lastFrameSize)
	}
	truncate(t, path, lastFrameStart+lastFrameSize/2)

	var c collector
	r, err := Replay(path, c.fn)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay of a torn tail: err = %v, want a CorruptError", err)
	}
	if r.EndOffset != lastFrameStart {
		t.Errorf("EndOffset = %d, want %d (just past the last intact record, which is where DUR-4 would cut)",
			r.EndOffset, lastFrameStart)
	}
	// The torn frame was the COMMIT of prepare 3, so the entry it would have
	// made visible is not delivered -- and prepare 3 is still open.
	want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
	if !sameCommitted(c.got, want) {
		t.Fatalf("delivered %s, want %s", showCommitted(c.got), showCommitted(want))
	}
	if !reflect.DeepEqual(r.Dangling, []uint64{3}) {
		t.Errorf("Dangling = %v, want [3]: the prepare whose commit was torn away is unresolved", r.Dangling)
	}

	// DUR-4: Open now REPAIRS this, because it is the one shape of damage that
	// is provably a torn tail -- a single incomplete frame, nothing after it.
	// RepairTail cuts the file back to the end of the last good record before
	// the replay runs, so the Log starts on a clean prefix of accepted history.
	applied := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: applied})
	if err != nil {
		t.Fatalf("Open on a torn tail: %v, want a repaired start", err)
	}
	defer l.Close()

	if got := fileSize(t, path); got != lastFrameStart {
		t.Errorf("the file is now %d bytes, want %d: Open must truncate to the end of the last good record",
			got, lastFrameStart)
	}
	rep := l.Recovered().Repaired
	if !rep.Truncated {
		t.Fatalf("Recovered().Repaired = %+v, want Truncated true", rep)
	}
	if rep.At != lastFrameStart {
		t.Errorf("Repaired.At = %d, want %d", rep.At, lastFrameStart)
	}
	if wantRemoved := lastFrameSize / 2; rep.Removed != wantRemoved {
		t.Errorf("Repaired.Removed = %d, want %d (the half-frame that was cut)", rep.Removed, wantRemoved)
	}
	// Only the first committed pair survives: the torn frame was the COMMIT of
	// prepare 3, so that entry is not applied and prepare 3 stays dangling.
	got := make([]Committed, applied.count())
	for i := range got {
		got[i] = applied.at(i)
	}
	if !sameCommitted(got, want) {
		t.Errorf("Open applied %s, want %s", showCommitted(got), showCommitted(want))
	}
}

// TestWALReplayIsDeterministic: recovery has to be reproducible to be testable,
// and an operator comparing two replays of the same bytes must get the same
// answer. Dangling in particular is collected from a map, so it is sorted on the
// way out; this is the test that would catch that regressing to map order.
func TestWALReplayIsDeterministic(t *testing.T) {
	_, path, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`),  // 1 -- dangling
		opPrepare("agent", `{"id":"a"}`), // 2
		opPrepare("message", `{"n":3}`),  // 3 -- dangling
		opCommit(2),                      // 4
		opPrepare("message", `{"n":5}`),  // 5
		opCommit(5),                      // 6
	)

	var first, second collector
	r1, err := Replay(path, first.fn)
	if err != nil {
		t.Fatalf("first Replay: %v", err)
	}
	r2, err := Replay(path, second.fn)
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}

	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("two replays of the same file disagree:\n first: %+v\nsecond: %+v", r1, r2)
	}
	if !sameCommitted(first.got, second.got) {
		t.Fatalf("two replays delivered different streams:\n first: %s\nsecond: %s",
			showCommitted(first.got), showCommitted(second.got))
	}
	if !reflect.DeepEqual(r1.Dangling, []uint64{1, 3}) {
		t.Errorf("Dangling = %v, want [1 3] in ascending order", r1.Dangling)
	}
	want := []Committed{
		wantC(2, 4, "agent", `{"id":"a"}`),
		wantC(5, 6, "message", `{"n":5}`),
	}
	if !sameCommitted(first.got, want) {
		t.Errorf("delivered %s, want %s", showCommitted(first.got), showCommitted(want))
	}
}

// TestWALReplayThroughOpen is the PREFIX-OF-ACCEPTED-HISTORY proof at the Log
// level (invariant 5): the Apply sequence a process sees when it rebuilds from
// disk must be identical -- same order, same indices, same body BYTES -- to the
// sequence the previous process saw live. If those two ever differ, memory after
// a restart is not the memory that was serving before it.
func TestWALReplayThroughOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WALFileName)

	entries := []Entry{
		{Kind: "message", Body: json.RawMessage(`{"to":"bus-1.agent-a","seq":1}`)},
		{Kind: "agent", Body: nil}, // the nil body must round-trip as nil
		{Kind: "message", Body: json.RawMessage(`{"to": "bus-1.agent-b", "seq": 2}`)}, // compacted on the way in
		{Kind: "bus", Body: json.RawMessage(`[1,2,3]`)},
		{Kind: "message", Body: json.RawMessage(`{"unicode":"héllo \"q\"","nested":{"a":[true,null]}}`)},
	}

	live := &testApplier{}
	l1, err := Open(LogOptions{Dir: dir, Applier: live})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if got := l1.Recovered(); got.NextIndex != 1 || got.Records != 0 || got.EndOffset != 0 {
		t.Errorf("first Open Recovered = %+v, want an empty log", got)
	}
	for i, e := range entries {
		if _, err := l1.Write(e); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	n := len(entries)
	if live.count() != n {
		t.Fatalf("the live Applier saw %d entries, want %d", live.count(), n)
	}
	liveCalls := make([]Committed, n)
	for i := range liveCalls {
		liveCalls[i] = live.at(i)
	}

	// Reopen with a FRESH applier: this is a cold start reading nothing but the
	// file.
	replayed := &testApplier{}
	l2, err := Open(LogOptions{Dir: dir, Applier: replayed})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()

	if replayed.count() != n {
		t.Fatalf("the replay Applier saw %d entries, want %d", replayed.count(), n)
	}
	replayCalls := make([]Committed, n)
	for i := range replayCalls {
		replayCalls[i] = replayed.at(i)
	}
	if !sameCommitted(replayCalls, liveCalls) {
		t.Fatalf("the replayed Apply sequence differs from the live one:\n  live: %s\nreplay: %s",
			showCommitted(liveCalls), showCommitted(replayCalls))
	}

	rec := l2.Recovered()
	if rec.Path != path {
		t.Errorf("Recovered().Path = %q, want %q", rec.Path, path)
	}
	if rec.Applied != uint64(n) {
		t.Errorf("Recovered().Applied = %d, want %d", rec.Applied, n)
	}
	if rec.Records != uint64(2*n) {
		t.Errorf("Recovered().Records = %d, want %d (a prepare and a commit each)", rec.Records, 2*n)
	}
	if rec.Aborted != 0 || len(rec.Dangling) != 0 {
		t.Errorf("Recovered() = %+v, want no aborts and no dangling prepares after a clean close", rec)
	}
	if rec.NextIndex != uint64(2*n)+1 {
		t.Errorf("Recovered().NextIndex = %d, want %d", rec.NextIndex, 2*n+1)
	}
	if got := fileSize(t, path); rec.EndOffset != got {
		t.Errorf("Recovered().EndOffset = %d, want the file size %d", rec.EndOffset, got)
	}

	// And the recovered Log keeps going from the high-water mark.
	c, err := l2.Write(Entry{Kind: "message", Body: json.RawMessage(`{"seq":6}`)})
	if err != nil {
		t.Fatalf("Write after recovery: %v", err)
	}
	if c.PrepareIndex != rec.NextIndex {
		t.Errorf("the write after recovery used prepare index %d, want NextIndex %d", c.PrepareIndex, rec.NextIndex)
	}
}

// TestWALReplayApplierErrorIsDiscardedNotFatal: an Applier that rejects an entry
// the log says was accepted.
//
// WHAT THIS USED TO ASSERT: Open failed, because memory could not be rebuilt
// from disk and serving a state disk does not justify is worse than not starting.
// The availability decision (DECISIONS.md, 2026-08-02) reversed it: the entry is
// dropped from the rebuilt memory state, the replay continues, and the server
// starts. This is the sharpest edge of that decision and it is asserted, not
// glossed -- the entry is DURABLE ON DISK AND ABSENT FROM MEMORY, which is a real
// divergence, so the whole weight rests on it being reported at ERROR with the
// prepare index and the entry kind.
//
// Two things that did NOT change, and would be bugs if they had:
//
//   - the rejection is not corruption. The log is fine; the caller refused. The
//     bytes are not touched, and nothing here truncates or rewrites the file.
//   - the entries AFTER the rejected one are still applied. Stopping at the
//     rejection would turn one caller-side failure into the loss of everything
//     behind it.
func TestWALReplayApplierErrorIsDiscardedNotFatal(t *testing.T) {
	dir := t.TempDir()

	seed, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := seed.Write(Entry{Kind: "message", Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))}); err != nil {
			t.Fatalf("seed Write %d: %v", i, err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	path := filepath.Join(dir, WALFileName)
	before := readFile(t, path)

	boom := errors.New("the roster rejected a replayed entry")
	rejects := &testApplier{check: func(c Committed) error {
		if c.PrepareIndex == 3 { // reject the second entry, not the first or third
			return boom
		}
		return nil
	}}

	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Applier: rejects, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open with an Applier that rejected a replayed entry: %v: "+
			"a caller-side rejection is discarded and logged, not a refusal to start", err)
	}
	rec := l.Recovered()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out := buf.String()

	// The rejected entry is gone from the rebuilt state; the ones on either side
	// of it are not. Stopping at the rejection would have cost entry 5 as well,
	// which is the cascade this asserts against. (testApplier records a call
	// before it runs its check, so all three OFFERS show up here -- what matters
	// is that the entry AFTER the rejection was offered at all.)
	if rejects.count() != 3 {
		t.Fatalf("Apply was offered %d entries, want 3: the replay must carry on past a rejected entry", rejects.count())
	}
	for i, want := range []uint64{1, 3, 5} {
		if got := rejects.at(i).PrepareIndex; got != want {
			t.Errorf("Apply call %d was prepare %d, want %d (in commit order, including the one it rejected)", i, got, want)
		}
	}
	if rec.Applied != 2 {
		t.Errorf("Recovered.Applied = %d, want 2: a rejected entry is not applied history", rec.Applied)
	}
	if rec.DiscardCount != 1 {
		t.Fatalf("Recovered.DiscardCount = %d, want 1 (%+v)", rec.DiscardCount, rec.Discarded)
	}
	d := rec.Discarded[0]
	if d.Stage != "replay" || d.Index != 4 || d.Type != TypeCommit {
		t.Errorf("Recovered.Discarded[0] = %+v, want the replay-stage loss of COMMIT record 4 (the commit of prepare 3)", d)
	}
	// THE DIVERGENCE IS NAMED. "durable on disk but absent from the rebuilt
	// memory state" is the whole of what an operator needs to know here.
	for _, want := range []string{"the applier rejected this committed entry", "prepare 3", "durable on disk but absent from the rebuilt memory state"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("the discard reason is %q, want it to contain %q", d.Reason, want)
		}
	}
	if !strings.Contains(d.Reason, boom.Error()) {
		t.Errorf("the discard reason is %q, want it to carry the applier's own cause %q", d.Reason, boom)
	}
	// AT ERROR. A committed entry that disk has and memory does not is the most
	// serious thing this package reports.
	assertLogged(t, out, "ERROR", "wal discarded a damaged record",
		"path="+path, "stage=replay", "record_index=4", "record_type=commit",
		"the applier rejected this committed entry")

	// The log is FINE -- the caller refused, not the disk -- so nothing was
	// truncated, rewritten or repaired.
	if after := readFile(t, path); !bytes.Equal(before, after) {
		t.Fatalf("Open changed the WAL: %d bytes before, %d after: an applier's rejection says nothing about the file",
			len(before), len(after))
	}
	if rep := rec.Repaired; rep.Truncated || rep.Rewritten || rep.Quarantined != "" {
		t.Errorf("Recovered.Repaired = %+v, want no repair at all", rep)
	}

	// And a healthy Applier over the same bytes still gets all three: the entry
	// was never removed from the durable log.
	healthy := &testApplier{}
	l2, err := Open(LogOptions{Dir: dir, Applier: healthy})
	if err != nil {
		t.Fatalf("reopening with a healthy Applier: %v", err)
	}
	defer l2.Close()
	if healthy.count() != 3 {
		t.Errorf("a healthy Applier saw %d entries, want 3: the rejected entry is still on disk", healthy.count())
	}
}

// TestWALSemanticDamageDoesNotCostTheRecordsBehindIt is the ANTI-DATA-LOSS test
// for DUR-4, and the most load-bearing test in this file. Its SUBJECT changed on
// 2026-08-02; its point did not.
//
// Every way a replay fails ABOVE the framing layer -- a commit that names no
// open prepare, a double commit, an abort of something already committed, a
// record type with no meaning in a WAL, a prepare payload that will not decode
// -- happens in a frame WHOSE CHECKSUM ALREADY VERIFIED. A partial write cannot
// produce that: a torn frame does not checksum.
//
// It used to prove that by asserting each error carried CorruptError.FrameIntact
// and a FrameEnd short of the end of the file -- the evidence that let the repair
// pass tell "damage in the middle of a readable log" from "a torn tail". Replay
// no longer returns an error for any of these (they are discarded and reported
// instead), so the FrameIntact flag is not where the evidence lives any more. The
// property is now asserted DIRECTLY, which is strictly better than asserting a
// flag that stood in for it:
//
//   - every case is built with well-formed records AFTER the damage, INCLUDING A
//     COMMIT -- accepted history, already acknowledged to a client;
//   - recovery must discard EXACTLY the one unusable record and deliver the
//     transaction behind it;
//   - and the FILE must not change by one byte. A semantic failure says nothing
//     about the framing, so nothing here may truncate, rewrite or repair.
//
// If recovery ever mistook one of these for a tail and cut at the damaged record,
// it would DELETE that committed record to tidy up a file that is fully readable,
// and the loss would be silent and permanent. That is the failure this test
// exists to make loud.
func TestWALSemanticDamageDoesNotCostTheRecordsBehindIt(t *testing.T) {
	// Records 1 and 2 are a clean transaction, record 3 is the damage, and
	// records 4 and 5 are a second clean transaction that must survive on disk.
	prefix := []walOp{opPrepare("message", `{"n":1}`), opCommit(1)}
	tail := []walOp{opPrepare("message", `{"n":9}`), opCommit(4)}
	const badIdx = 3

	// build lays the case out on disk. A record type Writer.Append refuses (an
	// unknown one) forces records 3..5 to be framed by hand; the framing is
	// still valid, so only the TYPE is unrecognisable.
	build := func(t *testing.T, bad walOp) string {
		t.Helper()
		if bad.typ.Known() {
			ops := make([]walOp, 0, len(prefix)+1+len(tail))
			ops = append(ops, prefix...)
			ops = append(ops, bad)
			ops = append(ops, tail...)
			_, path, _, _ := buildWAL(t, ops...)
			return path
		}
		_, path, _, _ := buildWAL(t, prefix...)
		base := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
		prep, err := encodePrepare("message", jsonBody(`{"n":9}`), base)
		if err != nil {
			t.Fatalf("encodePrepare: %v", err)
		}
		com, err := encodeCommit(4)
		if err != nil {
			t.Fatalf("encodeCommit: %v", err)
		}
		appendRawFrame(t, path, 3, bad.typ, bad.raw)
		appendRawFrame(t, path, 4, TypePrepare, prep)
		appendRawFrame(t, path, 5, TypeCommit, com)
		return path
	}

	cases := []struct {
		name    string
		bad     walOp
		wantMsg string
	}{
		{"a commit that names a commit record", opCommit(2), "not an open prepare"},
		{"the same prepare committed twice", opCommit(1), "not an open prepare"},
		{"an abort of an already committed prepare", opAbort(1, "too late"), "not an open prepare"},
		{"an audit record in a WAL", opRaw(TypeAuditMessage, `{"message_id":"m1"}`), "audit_message"},
		{"a record type from the future", walOp{typ: Type(4242), raw: []byte(`{}`)}, "unknown(4242)"},
		{"a prepare whose payload does not decode", opRaw(TypePrepare, `{"kind":"message"`), "does not decode"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			path := build(t, tc.bad)
			dir := filepath.Dir(path)

			// The FRAMING is fine everywhere in this file -- that is the whole
			// premise -- so ScanAll succeeds and hands over the exact extent of
			// the damaged record.
			recs, cleanEnd, err := ScanAll(path, KindWAL)
			if err != nil {
				t.Fatalf("the test log is not well framed: %v", err)
			}
			if len(recs) != 5 {
				t.Fatalf("built %d records, want 5", len(recs))
			}
			size := fileSize(t, path)
			if cleanEnd != size {
				t.Fatalf("ScanAll ended at %d but the file is %d bytes", cleanEnd, size)
			}
			bad := recs[badIdx-1]
			before := readFile(t, path)

			// The premise, asserted rather than assumed: there really is a COMMIT
			// record -- accepted history -- sitting after the damage. Without it
			// this case would no longer demonstrate the loss it guards against.
			committedAfter := false
			for _, rec := range recs {
				if rec.Offset >= bad.Offset+bad.frameSize() && rec.Type == TypeCommit {
					committedAfter = true
				}
			}
			if !committedAfter {
				t.Fatalf("no COMMIT record sits after the damaged frame at %d: the test no longer demonstrates the data loss it is guarding against",
					bad.Offset)
			}

			var c collector
			r, err := Replay(path, c.fn)
			if err != nil {
				t.Fatalf("Replay err = %v, want none: a record replay cannot interpret is discarded, not fatal", err)
			}

			// THE ASSERTION THIS TEST EXISTS FOR: the transaction behind the
			// damage is recovered. One unusable record must cost one record.
			want := []Committed{
				wantC(1, 2, "message", `{"n":1}`),
				wantC(4, 5, "message", `{"n":9}`),
			}
			if !sameCommitted(c.got, want) {
				t.Fatalf("delivered %s, want %s: records AFTER semantic damage are intact, acknowledged history and must survive it",
					showCommitted(c.got), showCommitted(want))
			}
			if r.EndOffset != size {
				t.Errorf("EndOffset = %d, want the end of the %d-byte file: replay reads past the damage", r.EndOffset, size)
			}
			if r.DiscardCount != 1 {
				t.Fatalf("Recovered.DiscardCount = %d, want exactly 1 (%+v)", r.DiscardCount, r.Discarded)
			}
			d := r.Discarded[0]
			if d.Stage != "replay" || d.Offset != bad.Offset || d.Index != badIdx {
				t.Errorf("Recovered.Discarded[0] = %+v, want the replay-stage loss of record %d at offset %d", d, badIdx, bad.Offset)
			}
			if !strings.Contains(d.Reason, tc.wantMsg) {
				t.Errorf("the discard reason is %q, want it to contain %q", d.Reason, tc.wantMsg)
			}

			// AND THE FILE IS UNTOUCHED. A semantic failure says nothing about the
			// framing, so recovery must not truncate, rewrite or repair on the
			// strength of one. This is the assertion that catches a future change
			// wiring the replay stage into the repair stage.
			_, rec, out, err := openCapturing(t, dir)
			if err != nil {
				t.Fatalf("Open: %v: recovery must always reach a running server", err)
			}
			if after := readFile(t, path); !bytes.Equal(before, after) {
				t.Fatalf("Open CHANGED the WAL: %d bytes before, %d after: semantic damage must never reach the truncation path",
					len(before), len(after))
			}
			if rep := rec.Repaired; rep.Truncated || rep.Rewritten || rep.HeaderRepaired || rep.Quarantined != "" {
				t.Errorf("Recovered.Repaired = %+v, want no repair: the framing of this file is perfect", rep)
			}
			assertLogged(t, out, "", "wal discarded a damaged record",
				"path="+path, "stage=replay", "record_index="+strconv.FormatUint(badIdx, 10))
		})
	}
}

// TestWALReplayDiscardsMalformedPreparePayload covers the EAGER DECODE branch: a
// PREPARE record is decoded the moment it is read, before anything knows whether
// it will ever commit.
//
// Each variant is run twice, and the second run is the interesting one: the bad
// prepare NEVER COMMITS in the first, so nothing about it could ever have become
// visible, while in the second a COMMIT names it -- and that commit is then an
// ACKNOWLEDGED WRITE WHOSE CONTENT IS UNRECOVERABLE. Both are discarded and the
// replay continues (DECISIONS.md 2026-08-02), and the difference between them is
// visible where it should be: the "then commits" run loses TWO records and the
// second loss is reported at ERROR, because a client was told that write was
// durable.
//
// The eagerness of the decode is still what is being pinned. A payload that will
// not decode means the file no longer says what it recorded, and that has to be
// reported at the record where it is found rather than deferred to whatever
// happens to reference it later.
func TestWALReplayDiscardsMalformedPreparePayload(t *testing.T) {
	const ts = `2026-08-02T09:00:00Z`

	cases := []struct {
		name    string
		payload string
		wantMsg string
	}{
		{"not JSON at all", `this is not json`, "does not decode"},
		{"an empty kind", `{"kind":"","ts":"` + ts + `","body":null}`, "empty kind"},
		{"a timestamp that will not parse", `{"kind":"message","ts":"yesterday","body":null}`, "not RFC3339Nano"},
		{"an empty timestamp", `{"kind":"message","ts":"","body":null}`, "not RFC3339Nano"},
		{"an unknown field", `{"kind":"message","ts":"` + ts + `","body":null,"seq":7}`, "does not decode"},
		{"trailing data after the object", `{"kind":"message","ts":"` + ts + `","body":null}{"kind":"x"}`, "trailing data"},
		{"a commit payload in a prepare record", `{"prepare_index":1}`, "does not decode"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// "never commits" first: it is the shape that proves the decode is
			// eager rather than deferred to the commit that pairs with it.
			for _, commits := range []bool{false, true} {
				name := "never commits"
				ops := []walOp{opRaw(TypePrepare, tc.payload)}
				if commits {
					name = "then commits"
					ops = append(ops, opCommit(1))
				}
				t.Run(name, func(t *testing.T) {
					// A SHORT directory: the discard reason wraps an error that
					// names the file and is then bounded, so a 120-character temp
					// path would elide away the very diagnosis being asserted.
					dir := shortTempDir(t)
					_, path, _, _ := buildWALIn(t, dir, ops...)

					// The frame itself is impeccable; only the payload is not.
					recs, cleanEnd, err := ScanAll(path, KindWAL)
					if err != nil {
						t.Fatalf("the test log is not well framed: %v", err)
					}

					var c collector
					r, err := Replay(path, c.fn)
					if err != nil {
						t.Fatalf("Replay err = %v, want none: a payload that does not decode is discarded, not fatal", err)
					}
					if len(c.got) != 0 {
						t.Fatalf("delivered %s, want nothing: a record replay could not read must never become visible",
							showCommitted(c.got))
					}
					if r.EndOffset != cleanEnd {
						t.Errorf("EndOffset = %d, want the end of the file %d: the framing is perfect, so replay reads it all",
							r.EndOffset, cleanEnd)
					}
					// THE ID IS STILL BURNED, even though nothing about the record
					// could be read. Index 1 reached stable storage; reissuing it
					// would let two records share an id (invariant 1).
					if r.NextIndex != uint64(len(recs))+1 {
						t.Errorf("NextIndex = %d, want %d: the unreadable record still burned its index", r.NextIndex, len(recs)+1)
					}

					// The prepare is discarded where it is FOUND, not where it is
					// referenced -- that is what makes the decode eager.
					if r.DiscardCount != len(ops) {
						t.Fatalf("Recovered.DiscardCount = %d, want %d (%+v)", r.DiscardCount, len(ops), r.Discarded)
					}
					d := r.Discarded[0]
					if d.Stage != "replay" || d.Index != 1 || d.Type != TypePrepare || d.Offset != recs[0].Offset {
						t.Errorf("Recovered.Discarded[0] = %+v, want the replay-stage loss of PREPARE record 1 at offset %d", d, recs[0].Offset)
					}
					if !strings.Contains(d.Reason, tc.wantMsg) {
						t.Errorf("the discard reason is %q, want it to contain %q", d.Reason, tc.wantMsg)
					}
					if !strings.Contains(d.Reason, "what this record reserved cannot be known") {
						t.Errorf("the discard reason is %q, want it to say what was lost", d.Reason)
					}

					// And a Log STARTS on it, applying nothing, and says so.
					got, rec, out, err := openCapturing(t, dir)
					if err != nil {
						t.Fatalf("Open on a WAL whose prepare payload does not decode: %v: recovery must always reach a running server", err)
					}
					if len(got) != 0 {
						t.Fatalf("Open applied %s, want nothing", showCommitted(got))
					}
					if rec.DiscardCount != len(ops) {
						t.Errorf("Recovered().DiscardCount = %d, want %d", rec.DiscardCount, len(ops))
					}
					// The undecodable PREPARE is WARN: nothing was acknowledged, so
					// nothing was promised.
					assertLogged(t, out, "WARN", "wal discarded a damaged record",
						"path="+path, "stage=replay", "record_index=1", "record_type=prepare")
					if commits {
						// ... but the COMMIT that named it is ERROR. A client was
						// told this write was durable and its content is now
						// unrecoverable, which is the single most serious thing
						// this package has to report.
						line := assertLogged(t, out, "ERROR", "wal discarded a damaged record",
							"stage=replay", "record_index=2", "record_type=commit")
						if !strings.Contains(line, "an acknowledged write is lost here") {
							t.Errorf("the lost-commit line does not say an acknowledged write is gone:\n%s", line)
						}
					}
				})
			}
		})
	}
}

// TestWALReplayHeaderOnlyLog pins the third "nothing happened here" shape, which
// is distinct from the two in TestWALReplayEmptyLog: a file that HAS its 16-byte
// header and no records at all. That is what a crash right after OpenWriter
// creates the file leaves behind, and it is a perfectly valid log of zero
// records -- not corruption, and not the same as a zero-length file, because it
// reports EndOffset FileHeaderSize (there IS a header to sit after) rather than
// 0.
func TestWALReplayHeaderOnlyLog(t *testing.T) {
	dir, path, builtNext, builtEnd := buildWAL(t) // no ops: header only

	if got := fileSize(t, path); got != FileHeaderSize {
		t.Fatalf("the built file is %d bytes, want exactly the %d-byte header", got, FileHeaderSize)
	}
	if builtNext != 1 || builtEnd != FileHeaderSize {
		t.Fatalf("the writer that built the file says next index %d end offset %d, want 1 and %d",
			builtNext, builtEnd, FileHeaderSize)
	}

	var c collector
	r, err := Replay(path, c.fn)
	if err != nil {
		t.Fatalf("Replay of a header-only log: %v, want no error", err)
	}
	if len(c.got) != 0 {
		t.Errorf("Replay delivered %s, want nothing", showCommitted(c.got))
	}
	if r.NextIndex != 1 {
		t.Errorf("NextIndex = %d, want 1", r.NextIndex)
	}
	if r.EndOffset != FileHeaderSize {
		t.Errorf("EndOffset = %d, want %d: the next append goes just after the file header",
			r.EndOffset, FileHeaderSize)
	}
	if r.Records != 0 || r.Applied != 0 || r.Aborted != 0 || len(r.Dangling) != 0 {
		t.Errorf("Recovered = %+v, want zero records/applied/aborted/dangling", r)
	}

	// Open agrees, and the first write starts the sequence at 1.
	app := &testApplier{}
	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("Open on a header-only log: %v", err)
	}
	defer l.Close()
	if got := l.Recovered(); got.NextIndex != 1 || got.EndOffset != FileHeaderSize || got.Records != 0 {
		t.Errorf("Log.Recovered() = %+v, want next index 1, end offset %d, no records", got, FileHeaderSize)
	}
	if app.count() != 0 {
		t.Errorf("Apply called %d times on a header-only log, want 0", app.count())
	}

	c1, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`)})
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if c1.PrepareIndex != 1 || c1.CommitIndex != 2 {
		t.Errorf("the first write got {prepare:%d commit:%d}, want {1,2}", c1.PrepareIndex, c1.CommitIndex)
	}
}

// TestWALReplayOpenDiscardsSemanticDamage is the same policy seen from where an
// OPERATOR sees it. Replay's verdict only matters if Open honours it.
//
// It used to assert a REFUSAL TO START -- a server must not serve a state that
// may be missing an acknowledged write -- plus the documented consequence that a
// failed Open leaves the Applier partially rebuilt. The user chose the restart
// instead (DECISIONS.md, 2026-08-02): the unusable record is discarded, the
// server starts, and the partial-rebuild hazard is gone with the failure that
// caused it.
//
// What this now pins, and each of these would be a real bug if it broke:
//
//   - Open SUCCEEDS on every one of these shapes;
//   - it applies the good transaction and nothing else -- a discarded record must
//     never be guessed at and half-applied;
//   - the FILE IS NOT EDITED. None of this is framing damage, so nothing may be
//     truncated, rewritten or repaired on the strength of it;
//   - and the loss reaches the operator log, at ERROR when what went was a
//     COMMIT (a client was told that write was durable) and WARN when it was a
//     record type that acknowledged nothing.
func TestWALReplayOpenDiscardsSemanticDamage(t *testing.T) {
	cases := []struct {
		name      string
		build     func(t *testing.T) (dir, path string)
		wantType  string
		wantLevel string
		wantMsg   string
	}{
		{
			name: "an audit record in a WAL",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t,
					opPrepare("message", `{"n":1}`), opCommit(1),
					opRaw(TypeAuditMessage, `{"message_id":"m1"}`))
				return dir, path
			},
			wantType:  "audit_message",
			wantLevel: "WARN",
			wantMsg:   "audit_message",
		},
		{
			name: "a record type from the future",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t, opPrepare("message", `{"n":1}`), opCommit(1))
				appendRawFrame(t, path, 3, Type(4242), []byte(`{}`))
				return dir, path
			},
			wantType:  "unknown(4242)",
			wantLevel: "WARN",
			wantMsg:   "unknown(4242)",
		},
		{
			name: "a commit that names a commit record",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t,
					opPrepare("message", `{"n":1}`), opCommit(1), opCommit(2))
				return dir, path
			},
			wantType:  "commit",
			wantLevel: "ERROR",
			wantMsg:   "not an open prepare",
		},
		{
			// A SHORT directory here and below: these two reasons wrap a decoder
			// error that names the file and is then bounded to maxCauseChars, so
			// a long temp path would elide the diagnosis being asserted.
			name: "a commit that names a record after itself",
			build: func(t *testing.T) (string, string) {
				dir := shortTempDir(t)
				_, path, _, _ := buildWALIn(t, dir,
					opPrepare("message", `{"n":1}`), opCommit(1), opCommit(9))
				return dir, path
			},
			wantType:  "commit",
			wantLevel: "ERROR",
			wantMsg:   "not earlier in the file",
		},
		{
			name: "a commit that names index 0",
			build: func(t *testing.T) (string, string) {
				dir := shortTempDir(t)
				_, path, _, _ := buildWALIn(t, dir,
					opPrepare("message", `{"n":1}`), opCommit(1), opCommit(0))
				return dir, path
			},
			wantType:  "commit",
			wantLevel: "ERROR",
			wantMsg:   "indices start at 1",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, path := tc.build(t)
			before := readFile(t, path)

			got, rec, out, err := openCapturing(t, dir)
			if err != nil {
				t.Fatalf("Open err = %v, want none: recovery must always reach a running server (DECISIONS.md 2026-08-02)", err)
			}

			// The good transaction, and only it. A discarded record is never
			// guessed at.
			want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
			if !sameCommitted(got, want) {
				t.Fatalf("Open applied %s, want %s", showCommitted(got), showCommitted(want))
			}
			if rec.DiscardCount != 1 {
				t.Fatalf("Recovered().DiscardCount = %d, want exactly 1 (%+v)", rec.DiscardCount, rec.Discarded)
			}
			d := rec.Discarded[0]
			if d.Stage != "replay" || d.Index != 3 {
				t.Errorf("Recovered().Discarded[0] = %+v, want the replay-stage loss of record 3", d)
			}
			if !strings.Contains(d.Reason, tc.wantMsg) {
				t.Errorf("the discard reason is %q, want it to contain %q", d.Reason, tc.wantMsg)
			}
			// Nothing here is FRAMING damage, so nothing may be EDITED: no
			// truncation, no repair, no fresh header. The bytes are evidence.
			if after := readFile(t, path); !bytes.Equal(before, after) {
				t.Errorf("Open changed the WAL: %d bytes before, %d after: a semantic failure must never reach the repair path",
					len(before), len(after))
			}
			if rep := rec.Repaired; rep.Truncated || rep.Rewritten || rep.HeaderRepaired || rep.Quarantined != "" {
				t.Errorf("Recovered().Repaired = %+v, want no repair at all", rep)
			}
			assertLogged(t, out, tc.wantLevel, "wal discarded a damaged record",
				"path="+path, "stage=replay", "record_index=3", "record_type="+tc.wantType, tc.wantMsg)
		})
	}
}

// TestWALReplayBoundsUnresolvedPrepares pins the boot-time OOM defence.
//
// Replay retains every prepare it has not yet paired with a commit or an abort,
// and it builds that set from a FILE IT HAS NO REASON TO TRUST YET. Without a
// bound, a damaged (or hostile) log of nothing but prepares makes recovery
// allocate until the kernel kills the process -- and because it happens during
// recovery, that failure survives every restart, which is the worst shape a
// failure can have.
//
// WHAT CHANGED ON 2026-08-02. Hitting the bound used to REFUSE THE START with a
// diagnosis, on the reasoning that a boot-time OOM would survive every restart
// so failing was better than allocating. Under "always be able to restart" that
// is no longer an available answer, and neither is allocating -- so the OLDEST
// unresolved prepares are EVICTED instead. That is a safe trade and the reason is
// worth stating: AN UNRESOLVED PREPARE NEVER COMMITTED, so nothing about it was
// ever acknowledged and evicting one loses nothing a client was promised. The
// memory bound stays exactly as tight as it was.
//
// Only the BYTE bound is exercised. maxOpenPrepares (1024) would need 1024
// fsynced appends to reach and is deliberately skipped as too slow for the
// narrow suite; the byte bound trips after a few dozen ~1 MiB records and
// exercises the same check.
func TestWALReplayBoundsUnresolvedPrepares(t *testing.T) {
	// A body just under MaxPayloadSize once the prepare envelope
	// ({"kind":..,"ts":..,"body":..}) is added around it.
	body := `"` + strings.Repeat("a", MaxPayloadSize-512) + `"`
	const kind = "m"
	// What one open prepare retains, exactly as Replay accounts for it.
	perRecord := int64(len(kind) + len(body))
	// One more record than the bound can hold, plus one for slack.
	n := int(maxOpenPrepareBytes/perRecord) + 2

	t.Run("a log of nothing but prepares evicts the oldest", func(t *testing.T) {
		ops := make([]walOp, 0, n)
		for i := 0; i < n; i++ {
			ops = append(ops, opPrepare(kind, body))
		}
		dir, path, _, _ := buildWAL(t, ops...)

		var c collector
		r, err := Replay(path, c.fn)
		if err != nil {
			t.Fatalf("Replay of %d unresolved %d-byte prepares: err = %v, want none: the bound evicts now, it does not refuse",
				n, perRecord, err)
		}
		if len(c.got) != 0 {
			t.Fatalf("delivered %s, want nothing: not one of these prepares ever committed", showCommitted(c.got))
		}
		// The eviction happened, it is reported, and it is the OLDEST prepare
		// that went -- record 1, the one furthest from ever being resolved.
		if r.DiscardCount == 0 {
			t.Fatalf("Recovered = %+v, want at least one eviction reported: a bound that evicts silently is a bound "+
				"that turns a hostile file into quiet data loss", r)
		}
		d := r.Discarded[0]
		if d.Stage != "replay" || d.Type != TypePrepare || d.Index != 1 {
			t.Errorf("Recovered.Discarded[0] = %+v, want the eviction of PREPARE record 1 (the oldest)", d)
		}
		// The diagnosis has to say what limit was hit, or an operator has no way
		// to tell this from ordinary damage.
		for _, want := range []string{"evicted the oldest unresolved prepare", "memory bounds"} {
			if !strings.Contains(d.Reason, want) {
				t.Errorf("the eviction reason %q does not name the bound (%q)", d.Reason, want)
			}
		}
		// EVERY index is still burned: eviction frees memory, never ids.
		if r.NextIndex != uint64(n)+1 {
			t.Errorf("NextIndex = %d, want %d: an evicted prepare still burned its index", r.NextIndex, n+1)
		}
		// And a server starts on it, and says what it evicted. An eviction is
		// WARN, not ERROR: nothing here was ever acknowledged.
		_, rec, out, err := openCapturing(t, dir)
		if err != nil {
			t.Fatalf("Open: %v: a file this server did not write must not stop it starting", err)
		}
		if rec.DiscardCount == 0 {
			t.Errorf("Recovered().DiscardCount = 0, want the evictions reported through Open too")
		}
		assertLogged(t, out, "WARN", "wal discarded a damaged record",
			"path="+path, "stage=replay", "record_type=prepare", "evicted the oldest unresolved prepare")
	})

	t.Run("a legitimate log is never bounded out", func(t *testing.T) {
		// THE NEGATIVE, and the reason the bound is on RETAINED bytes and not on
		// bytes seen: this log moves more than maxOpenPrepareBytes through the
		// open set, but resolves each transaction before starting the next, so
		// the set never holds more than one entry. A bound that accumulated
		// instead of releasing would refuse to start on a perfectly good log --
		// a far worse bug than the one it guards against, because it would take
		// down a healthy server.
		var ops []walOp
		aborts := 0
		for i := 0; i < n; i++ {
			prepareIndex := uint64(2*i + 1)
			ops = append(ops, opPrepare(kind, body))
			if i%5 == 4 { // aborts must release their bytes too, not just commits
				ops = append(ops, opAbort(prepareIndex, "released"))
				aborts++
			} else {
				ops = append(ops, opCommit(prepareIndex))
			}
		}
		_, path, _, _ := buildWAL(t, ops...)

		// Counted rather than collected: holding n ~1 MiB bodies would make the
		// test itself the memory hog.
		applied := 0
		r, err := Replay(path, func(Committed) error { applied++; return nil })
		if err != nil {
			t.Fatalf("Replay of %d serialised transactions carrying %d bytes total: %v, want no error",
				n, int64(n)*perRecord, err)
		}
		if want := n - aborts; applied != want {
			t.Errorf("delivered %d entries, want %d", applied, want)
		}
		if r.Applied != uint64(n-aborts) || r.Aborted != uint64(aborts) {
			t.Errorf("Recovered Applied/Aborted = %d/%d, want %d/%d", r.Applied, r.Aborted, n-aborts, aborts)
		}
		if len(r.Dangling) != 0 {
			t.Errorf("Dangling = %v, want none", r.Dangling)
		}
	})
}

// TestWALReplayRecoveredIsACopy: Recovered() hands out the Log's own record of
// its recovery, and Dangling is a slice. Without a copy, a caller that sorted,
// truncated or otherwise edited that slice would silently rewrite the Log's
// account of what the durable file said -- and the next caller (an operator
// endpoint, a metric) would read the edited version as fact.
func TestWALReplayRecoveredIsACopy(t *testing.T) {
	dir, _, _, _ := buildWAL(t,
		opPrepare("message", `{"n":1}`),  // 1 -- dangling
		opPrepare("agent", `{"id":"a"}`), // 2 -- dangling
		opPrepare("message", `{"n":3}`),  // 3 -- dangling
	)
	want := []uint64{1, 2, 3}

	l, err := Open(LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	first := l.Recovered()
	if !reflect.DeepEqual(first.Dangling, want) {
		t.Fatalf("Recovered().Dangling = %v, want %v", first.Dangling, want)
	}

	first.Dangling[0] = 999
	first.Dangling = append(first.Dangling, 4242)

	second := l.Recovered()
	if !reflect.DeepEqual(second.Dangling, want) {
		t.Fatalf("after a caller edited the first Recovered().Dangling, a second call returned %v, want %v: "+
			"Recovered must hand out a copy", second.Dangling, want)
	}
	// And the copy is fresh every time, not one shared copy handed to everybody.
	second.Dangling[2] = 777
	if third := l.Recovered(); !reflect.DeepEqual(third.Dangling, want) {
		t.Fatalf("a third Recovered().Dangling = %v, want %v", third.Dangling, want)
	}
}

// TestWALReplayCapsDanglingLogLines: a dangling prepare is worth an operator's
// attention -- it is what a crash between the two fsyncs looks like, and a client
// may still be waiting on that write -- but a damaged file must not be able to
// turn one restart into thousands of log lines. Open names the first few and
// then reports a count.
func TestWALReplayCapsDanglingLogLines(t *testing.T) {
	const dangling = maxDanglingLogged + 5
	ops := make([]walOp, 0, dangling)
	for i := 0; i < dangling; i++ {
		ops = append(ops, opPrepare("message", fmt.Sprintf(`{"n":%d}`, i)))
	}
	dir, _, _, _ := buildWAL(t, ops...)

	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open with %d dangling prepares: %v (dangling prepares are not an error)", dangling, err)
	}
	defer l.Close()

	if got := len(l.Recovered().Dangling); got != dangling {
		t.Fatalf("Recovered().Dangling has %d entries, want %d", got, dangling)
	}

	out := buf.String()
	named := strings.Count(out, "wal replay discarded an uncommitted prepare")
	if named != maxDanglingLogged {
		t.Errorf("Open logged %d individual dangling prepares, want at most %d", named, maxDanglingLogged)
	}
	if !strings.Contains(out, "not_logged="+fmt.Sprint(dangling-maxDanglingLogged)) {
		t.Errorf("the summary line does not report the %d prepares it did not name; log was:\n%s",
			dangling-maxDanglingLogged, out)
	}
	// The complete figure is still reported once, on the replay line.
	if !strings.Contains(out, "dangling="+fmt.Sprint(dangling)) {
		t.Errorf("the replay line does not report the full dangling count %d; log was:\n%s", dangling, out)
	}
}

// TestWALReplayElidesFileDerivedStrings: a WAL payload is attacker-influenced
// (message bodies are client-supplied) and may be up to MaxPayloadSize, so no
// string lifted out of one may be pasted whole into an error or a log line. Two
// things are pinned here: the elision itself, and the rule that a record's BODY
// never appears in an error at all.
//
// The assertion is on the FULLY RENDERED message in every case, which is the
// only bound that means anything: a log line gets Error(), not Reason. That
// matters most where a CorruptError carries a cause, because encoding/json and
// time quote the offending field or value back IN FULL, so an unbounded cause
// would defeat an elided Reason completely. CorruptError.Error bounds the cause
// too; Unwrap still exposes it whole for a caller that asks for it.
func TestWALReplayElidesFileDerivedStrings(t *testing.T) {
	const marker = "SECRET-BODY-DO-NOT-LOG"
	const long = 2000 // comfortably longer than elide's 64-byte budget
	body := `{"secret":"` + marker + `","pad":"` + strings.Repeat("p", long) + `"}`

	// The bound moved with the policy: these failures are no longer errors, they
	// are DISCARD REASONS, and a discard reason goes straight into a log line. So
	// the string that has to be bounded is the same string, reached through
	// Recovered.Discarded rather than through err.Error() -- and the log line
	// itself is checked too, because that is where an unelided megabyte would
	// actually land.
	t.Run("a timestamp from disk is elided", func(t *testing.T) {
		payload := `{"kind":"message","ts":"` + strings.Repeat("t", long) + `","body":` + body + `}`
		dir, path, _, _ := buildWAL(t, opRaw(TypePrepare, payload))

		r, err := Replay(path, nil)
		if err != nil {
			t.Fatalf("Replay err = %v, want none", err)
		}
		if r.DiscardCount != 1 {
			t.Fatalf("Recovered.DiscardCount = %d, want 1 (%+v)", r.DiscardCount, r.Discarded)
		}
		// time.ParseError repeats the offending value twice; the rendered
		// reason must be bounded regardless.
		assertElided(t, r.Discarded[0].Reason, marker)
		assertLoggedReasonElided(t, dir, marker)
	})

	t.Run("a field name from disk is elided", func(t *testing.T) {
		payload := `{"kind":"message","ts":"2026-08-02T09:00:00Z","` +
			strings.Repeat("f", long) + `":1,"body":` + body + `}`
		dir, path, _, _ := buildWAL(t, opRaw(TypePrepare, payload))

		r, err := Replay(path, nil)
		if err != nil {
			t.Fatalf("Replay err = %v, want none", err)
		}
		if r.DiscardCount != 1 {
			t.Fatalf("Recovered.DiscardCount = %d, want 1 (%+v)", r.DiscardCount, r.Discarded)
		}
		// encoding/json's "unknown field" message carries the field name, which
		// came off disk: the bound has to survive that.
		assertElided(t, r.Discarded[0].Reason, marker)
		assertLoggedReasonElided(t, dir, marker)
	})

	t.Run("an entry kind is elided when the applier rejects it", func(t *testing.T) {
		kind := strings.Repeat("k", long)
		dir, _, _, _ := buildWAL(t, opPrepare(kind, body), opCommit(1))

		boom := errors.New("the roster rejected a replayed entry")
		app := &testApplier{check: func(Committed) error { return boom }}
		var buf bytes.Buffer
		l, err := Open(LogOptions{Dir: dir, Applier: app, Logger: logging.New(&buf, logging.LevelDebug)})
		if err != nil {
			t.Fatalf("Open err = %v, want none: an applier's rejection is discarded and logged, not fatal", err)
		}
		rec := l.Recovered()
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if rec.DiscardCount != 1 {
			t.Fatalf("Recovered.DiscardCount = %d, want 1 (%+v)", rec.DiscardCount, rec.Discarded)
		}
		reason := rec.Discarded[0].Reason
		if !strings.Contains(reason, boom.Error()) {
			t.Errorf("the discard reason %q does not carry the applier's own cause", reason)
		}
		assertElided(t, reason, marker)
		if strings.Contains(reason, kind) {
			t.Errorf("the full %d-byte kind was pasted into the discard reason", len(kind))
		}
		if strings.Contains(buf.String(), kind) {
			t.Errorf("the full %d-byte kind was pasted into a LOG LINE, which is where an unbounded file-derived "+
				"string actually costs something", len(kind))
		}
	})
}

// assertLoggedReasonElided starts a Log on dir and checks that the discard's
// reason reached the operator log WITHOUT dragging a record body into it. A
// bounded Reason that is then logged unbounded would be no bound at all.
func assertLoggedReasonElided(t *testing.T, dir, bodyMarker string) {
	t.Helper()
	var buf bytes.Buffer
	l, err := Open(LogOptions{Dir: dir, Logger: logging.New(&buf, logging.LevelDebug)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	line, ok := findLogLine(buf.String(), "wal discarded a damaged record")
	if !ok {
		t.Fatalf("the discard was not logged at all:\n%s", buf.String())
	}
	if strings.Contains(line, bodyMarker) {
		t.Errorf("the record BODY reached the operator log:\n%s", line)
	}
	const maxLine = 1024
	if len(line) > maxLine {
		t.Errorf("the discard log line is %d bytes, want at most %d: a file-derived string was not elided:\n%s",
			len(line), maxLine, line)
	}
}

// assertElided checks one message that quoted a string lifted off disk: it is
// bounded, it says so, and it does not contain the record body.
func assertElided(t *testing.T, msg, bodyMarker string) {
	t.Helper()
	// Generous, and still orders of magnitude below MaxPayloadSize: what is
	// being ruled out is a megabyte of payload in a log line, not a long path.
	const maxLen = 512
	if len(msg) > maxLen {
		t.Errorf("message is %d bytes, want at most %d: a file-derived string was not elided:\n%s",
			len(msg), maxLen, msg)
	}
	if !strings.Contains(msg, "[elided]") {
		t.Errorf("message %q does not mark the truncation with [elided]", msg)
	}
	if strings.Contains(msg, bodyMarker) {
		t.Errorf("the record body leaked into the message: %q", msg)
	}
}

// TestWALReplayReportsIndexHoles pins the reporting half of "survivors are never
// renumbered": a repaired log has permanent HOLES, and every one of them is
// counted and logged on EVERY start rather than only the one that made it.
//
// Silence here is exactly what let a lost sector look like a clean boot, so this
// is asserted across a RESTART: the hole is still reported the second time, and
// the third.
func TestWALReplayReportsIndexHoles(t *testing.T) {
	dir := t.TempDir()
	_, path, _, _ := buildWALIn(t, dir,
		opPrepare("message", `{"n":1}`), // 1
		opCommit(1),                     // 2
		opPrepare("message", `{"n":2}`), // 3
		opCommit(3),                     // 4
		opPrepare("message", `{"n":3}`), // 5
		opCommit(5),                     // 6
	)
	recs, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// Cut records 3 and 4 out of the file entirely, leaving 1, 2, 5, 6: every
	// remaining frame checksums, and the sequence rises with a two-record hole.
	b := readFile(t, path)
	cut := append(append([]byte{}, b[:recs[2].Offset]...), b[recs[4].Offset:]...)
	if err := os.WriteFile(path, cut, fileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for start := 1; start <= 3; start++ {
		got, rec, out, err := openCapturing(t, dir)
		if err != nil {
			t.Fatalf("start %d: Open: %v: a hole is not corruption, it is what a repair LEAVES", start, err)
		}
		if rec.MissingRecords != 2 {
			t.Fatalf("start %d: MissingRecords = %d, want 2: records 3 and 4 are absent, and that must be reported on "+
				"EVERY start -- silence here is what lets a lost sector look like a clean boot", start, rec.MissingRecords)
		}
		if rec.Repaired.Truncated || rec.Repaired.Rewritten {
			t.Errorf("start %d: Repaired = %+v, want no repair: a hole is legal framing", start, rec.Repaired)
		}
		want := []Committed{
			wantC(1, 2, "message", `{"n":1}`),
			wantC(5, 6, "message", `{"n":3}`),
		}
		if !sameCommitted(got, want) {
			t.Fatalf("start %d: recovery served %s, want %s", start, showCommitted(got), showCommitted(want))
		}
		// WARN, not ERROR, and the level is the assertion. A hole has NO BYTES:
		// nothing was read and nothing was deleted, and the range may even be
		// indices a crash reserved and never used. ERROR here is how one flipped
		// bit turned into a permanent, alarming line on every start for ever --
		// a loss channel that cries wolf. A discard that actually removed bytes
		// it could not identify is still ERROR (see Discard.severe).
		assertLogged(t, out, "WARN", "wal discarded a damaged record",
			"stage=replay", "records 3..4 are absent from the index sequence",
			"UPPER BOUND on loss")
		assertLogged(t, out, "", "wal replayed", "missing_records=2")
	}
}
