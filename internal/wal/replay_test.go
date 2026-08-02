package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	dir = t.TempDir()
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

// TestWALReplayRejectsBadReferences pins the strictness rule for a COMMIT or
// ABORT that does not name an OPEN prepare. Each of these says the file no
// longer describes a history this code can reconstruct, so replay refuses to
// start rather than guessing -- and, critically, nothing after the bad record is
// delivered, because a delivered suffix past unexplained damage is exactly how a
// state that is not a prefix of accepted history gets served.
func TestWALReplayRejectsBadReferences(t *testing.T) {
	// Every case ends with a well-formed prepare/commit pair (records 4 and 5)
	// that must NOT be delivered.
	tail := []walOp{opPrepare("message", `{"n":9}`), opCommit(4)}

	cases := []struct {
		name    string
		ops     []walOp
		badIdx  int // 1-based record index of the record replay must stop on
		want    []Committed
		wantMsg string
	}{
		{
			name: "commit names a commit record",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opCommit(1),                     // 2
				opCommit(2),                     // 3 -- record 2 is not a prepare
			}, tail...),
			badIdx:  3,
			want:    []Committed{wantC(1, 2, "message", `{"n":1}`)},
			wantMsg: "not an open prepare",
		},
		{
			name: "the same prepare is committed twice",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opCommit(1),                     // 2
				opCommit(1),                     // 3 -- already committed
			}, tail...),
			badIdx:  3,
			want:    []Committed{wantC(1, 2, "message", `{"n":1}`)},
			wantMsg: "not an open prepare",
		},
		{
			name: "abort of an already committed prepare",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opCommit(1),                     // 2
				opAbort(1, "too late"),          // 3
			}, tail...),
			badIdx:  3,
			want:    []Committed{wantC(1, 2, "message", `{"n":1}`)},
			wantMsg: "not an open prepare",
		},
		{
			name: "commit of an already aborted prepare",
			ops: append([]walOp{
				opPrepare("message", `{"n":1}`), // 1
				opAbort(1, "rejected"),          // 2
				opCommit(1),                     // 3
			}, tail...),
			badIdx:  3,
			want:    nil,
			wantMsg: "not an open prepare",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, path, _, _ := buildWAL(t, tc.ops...)

			// The FRAMING is fine -- this is a semantic failure -- so ScanAll
			// succeeds and gives the offset replay must stop at.
			recs, _, err := ScanAll(path, KindWAL)
			if err != nil {
				t.Fatalf("the test log is not well framed: %v", err)
			}
			bad := recs[tc.badIdx-1]

			var c collector
			r, err := Replay(path, c.fn)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Replay err = %v, want a CorruptError", err)
			}
			var ce *CorruptError
			if !errors.As(err, &ce) {
				t.Fatalf("Replay err = %v, want it to unwrap to *CorruptError", err)
			}
			// An operator's first move is a hex dump, so the error has to name
			// the record and the offset.
			if want := fmt.Sprintf("record %d", tc.badIdx); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err, tc.wantMsg)
			}
			if ce.Offset != bad.Offset {
				t.Errorf("CorruptError.Offset = %d, want the bad record's offset %d", ce.Offset, bad.Offset)
			}
			if !sameCommitted(c.got, tc.want) {
				t.Fatalf("delivered %s, want %s: nothing after the bad record may be delivered",
					showCommitted(c.got), showCommitted(tc.want))
			}
			if r.EndOffset != bad.Offset {
				t.Errorf("EndOffset = %d, want %d: the file stops being trustworthy at the bad record",
					r.EndOffset, bad.Offset)
			}

			// A Log must refuse to start on it.
			if l, err := Open(LogOptions{Dir: dir}); err == nil {
				_ = l.Close()
				t.Fatal("Open succeeded on a log replay rejects; recovery must be a refusal to start")
			} else if !errors.Is(err, ErrCorrupt) {
				t.Errorf("Open err = %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestWALReplayRejectsUnknownRecordType pins where forward compatibility ends.
// scanFrom accepts a record whose type it does not know, because the checksum
// proves some writer meant those exact bytes. Replay must NOT: a record whose
// effect on accepted history is unknown cannot be ignored, since ignoring it is
// indistinguishable from losing whatever it recorded. An audit record in a WAL
// is the same story -- audit records live in the audit file, so one here means
// these are not the bytes we think they are.
func TestWALReplayRejectsUnknownRecordType(t *testing.T) {
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
			var path string
			if tc.bad.typ.Known() {
				_, path, _, _ = buildWAL(t, append(ops, tc.bad)...)
			} else {
				// Writer.Append refuses an unknown type on purpose, so the frame
				// is assembled and appended by hand. The framing is valid -- only
				// the type is unrecognised.
				_, path, _, _ = buildWAL(t, ops...)
				appendRawFrame(t, path, 3, tc.bad.typ, tc.bad.raw)
			}

			// The frame is intact as far as the scanner is concerned.
			if _, _, err := ScanAll(path, KindWAL); err != nil {
				t.Fatalf("ScanAll rejected the record before Replay could: %v", err)
			}

			var c collector
			r, err := Replay(path, c.fn)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Replay err = %v, want a CorruptError", err)
			}
			if !strings.Contains(err.Error(), "record 3") {
				t.Errorf("error %q does not name record 3", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not name the record type %q", err, tc.wantMsg)
			}
			want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
			if !sameCommitted(c.got, want) {
				t.Fatalf("delivered %s, want %s (the good prefix only)", showCommitted(c.got), showCommitted(want))
			}
			if r.NextIndex != 4 {
				t.Errorf("NextIndex = %d, want 4: the unreadable record still burned index 3", r.NextIndex)
			}
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
	if _, err := f.Write(encodeFrame(index, typ, payload)); err != nil {
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

// TestWALReplayApplierErrorFailsOpen: an Applier that rejects an entry the log
// says was accepted means memory cannot be rebuilt from disk, so Open must fail
// rather than serve a state disk does not justify. The failure is deliberately
// NOT corruption -- the log is fine; the caller is the one refusing -- and
// classifying it as ErrCorrupt would send an operator hunting a disk problem
// that does not exist (and, worse, invite a DUR-4 tail truncation of a perfectly
// good log).
func TestWALReplayApplierErrorFailsOpen(t *testing.T) {
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
	before := readFile(t, filepath.Join(dir, WALFileName))

	boom := errors.New("the roster rejected a replayed entry")
	app := &testApplier{check: func(c Committed) error {
		if c.PrepareIndex == 3 { // fail on the second entry, not the first
			return boom
		}
		return nil
	}}

	l, err := Open(LogOptions{Dir: dir, Applier: app})
	if err == nil {
		_ = l.Close()
		t.Fatal("Open succeeded with an Applier that rejected a replayed entry")
	}
	if errors.Is(err, ErrCorrupt) {
		t.Errorf("Open err = %v, want an error that is NOT ErrCorrupt: the log is fine, the applier refused", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Open err = %v, want it to unwrap to the applier's cause", err)
	}
	if !strings.Contains(err.Error(), "prepare 3") {
		t.Errorf("Open err = %q, does not name the entry the applier rejected", err)
	}
	if app.count() != 2 {
		t.Errorf("Apply called %d times, want 2: replay must stop at the rejection", app.count())
	}
	if after := readFile(t, filepath.Join(dir, WALFileName)); !bytes.Equal(before, after) {
		t.Errorf("a failed Open changed the WAL: %d bytes before, %d after", len(before), len(after))
	}
}

// TestWALReplayFrameIntactAtSemanticDamage is the ANTI-DATA-LOSS test for
// DUR-4, and the most load-bearing test in this file.
//
// Every way a replay fails ABOVE the framing layer -- a commit that names no
// open prepare, a double commit, an abort of something already committed, a
// record type with no meaning in a WAL, a prepare payload that will not decode
// -- happens in a frame WHOSE CHECKSUM ALREADY VERIFIED. A partial write cannot
// produce that: a torn frame does not checksum. So each of these errors must
// carry FrameIntact, and must declare where the frame ended.
//
// Why that matters, concretely: recovery policy (DUR-4) is allowed to truncate a
// torn TAIL. Every case below is built with well-formed records AFTER the
// damage, including a COMMIT record -- accepted history, already acknowledged to
// a client. If DUR-4 ever mistook one of these for a tail and cut the file at
// Recovered.EndOffset, it would DELETE that committed record to tidy up a file
// that is fully readable, and the loss would be silent and permanent. The
// assertions below are the evidence that lets DUR-4 tell the two apart:
// FrameIntact says a torn write cannot explain this, and FrameEnd < file size
// proves the file continues past the damage.
func TestWALReplayFrameIntactAtSemanticDamage(t *testing.T) {
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

			var c collector
			r, err := Replay(path, c.fn)
			var ce *CorruptError
			if !errors.As(err, &ce) {
				t.Fatalf("Replay err = %v, want a *CorruptError", err)
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Replay err = %v, want errors.Is(err, ErrCorrupt)", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err, tc.wantMsg)
			}
			if ce.Offset != bad.Offset {
				t.Errorf("CorruptError.Offset = %d, want the damaged record's offset %d", ce.Offset, bad.Offset)
			}

			// (1) The checksum verified, so a torn write cannot explain this.
			if !ce.FrameIntact {
				t.Errorf("CorruptError.FrameIntact = false, want true: this frame's checksum verified, "+
					"so a partial write cannot explain the damage and recovery must never treat it as a truncatable tail (%v)", err)
			}
			// (2) The frame's extent is declared, so recovery can look past it.
			if want := bad.Offset + bad.frameSize(); ce.FrameEnd != want {
				t.Errorf("CorruptError.FrameEnd = %d, want %d (just past the damaged frame)", ce.FrameEnd, want)
			}
			// (3) THE PROOF: the file continues past the damage, so this is not a
			// tail, so DUR-4 must not truncate here.
			if ce.FrameEnd >= size {
				t.Fatalf("FrameEnd = %d is at or past the %d-byte file: this case is meant to have records AFTER the damage",
					ce.FrameEnd, size)
			}
			// (4) And what follows is not filler: it is a COMMIT record, i.e.
			// accepted history that a truncation would destroy.
			committedAfter := false
			for _, rec := range recs {
				if rec.Offset >= ce.FrameEnd && rec.Type == TypeCommit {
					committedAfter = true
				}
			}
			if !committedAfter {
				t.Fatalf("no COMMIT record sits after FrameEnd %d: the test no longer demonstrates the data loss it is guarding against",
					ce.FrameEnd)
			}

			// The usual strictness guarantees still hold: only the good prefix
			// was delivered, and replay stopped ON the damaged record.
			want := []Committed{wantC(1, 2, "message", `{"n":1}`)}
			if !sameCommitted(c.got, want) {
				t.Fatalf("delivered %s, want %s (the good prefix only)", showCommitted(c.got), showCommitted(want))
			}
			if r.EndOffset != bad.Offset {
				t.Errorf("EndOffset = %d, want %d (the start of the damaged record)", r.EndOffset, bad.Offset)
			}
			if r.EndOffset >= size {
				t.Errorf("EndOffset = %d is at the end of the %d-byte file, so the damage looks like a tail when it is not",
					r.EndOffset, size)
			}
		})
	}
}

// TestWALReplayRejectsMalformedPreparePayload covers the EAGER DECODE branch: a
// PREPARE record is decoded the moment it is read, before anything knows whether
// it will ever commit.
//
// Each variant is run twice, and the second run is the interesting one: the bad
// prepare NEVER COMMITS, so nothing about it could ever become visible and a
// lenient replay would be tempted to shrug and carry on. It must still fail. A
// payload that will not decode means the file no longer says what it recorded,
// and the honest report of that is at the record where it is found -- not at some
// later restart, and never by guessing.
func TestWALReplayRejectsMalformedPreparePayload(t *testing.T) {
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
					dir, path, _, _ := buildWAL(t, ops...)

					// The frame itself is impeccable; only the payload is not.
					if _, _, err := ScanAll(path, KindWAL); err != nil {
						t.Fatalf("the test log is not well framed: %v", err)
					}

					var c collector
					r, err := Replay(path, c.fn)
					if !errors.Is(err, ErrCorrupt) {
						t.Fatalf("Replay err = %v, want errors.Is(err, ErrCorrupt)", err)
					}
					var ce *CorruptError
					if !errors.As(err, &ce) {
						t.Fatalf("Replay err = %v, want a *CorruptError", err)
					}
					if !strings.Contains(err.Error(), "record 1") {
						t.Errorf("error %q does not name record 1", err)
					}
					if !strings.Contains(err.Error(), tc.wantMsg) {
						t.Errorf("error %q does not contain %q", err, tc.wantMsg)
					}
					if len(c.got) != 0 {
						t.Fatalf("delivered %s, want nothing: a record replay could not read must make nothing visible",
							showCommitted(c.got))
					}
					// A verified frame with an unreadable payload: fatal where it
					// sits, never a truncatable tail (see the FrameIntact test).
					if !ce.FrameIntact {
						t.Errorf("CorruptError.FrameIntact = false, want true: the frame checksummed, only its payload is bad")
					}
					recs, _, _ := ScanAll(path, KindWAL)
					if want := recs[0].Offset + recs[0].frameSize(); ce.FrameEnd != want {
						t.Errorf("CorruptError.FrameEnd = %d, want %d", ce.FrameEnd, want)
					}
					if r.EndOffset != FileHeaderSize {
						t.Errorf("EndOffset = %d, want %d (replay stopped on the first record)", r.EndOffset, FileHeaderSize)
					}
					if r.NextIndex != 2 {
						t.Errorf("NextIndex = %d, want 2: the unreadable record still burned index 1", r.NextIndex)
					}

					// And a Log refuses to start on it.
					app := &testApplier{}
					if l, err := Open(LogOptions{Dir: dir, Applier: app}); err == nil {
						_ = l.Close()
						t.Fatal("Open succeeded on a WAL whose prepare payload does not decode")
					} else if !errors.Is(err, ErrCorrupt) {
						t.Errorf("Open err = %v, want ErrCorrupt", err)
					}
					if app.count() != 0 {
						t.Errorf("Apply called %d times, want 0", app.count())
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

// TestWALReplayOpenRefusesSemanticDamage is the same strictness seen from where
// an OPERATOR sees it. Replay's verdict only matters if Open honours it: a
// server must refuse to start on a log it cannot interpret rather than serve a
// state that may be missing an acknowledged write.
//
// It also pins the documented consequence of that refusal: replay applies as it
// walks, so a FAILED Open leaves the Applier holding the good prefix. That
// fragment is not a prefix of anything the caller may serve, and the contract is
// that the caller throws the Applier away with the failed Log -- which is only
// testable by observing that Apply really was called before the failure.
func TestWALReplayOpenRefusesSemanticDamage(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T) (dir, path string)
		wantMsg string
	}{
		{
			name: "an audit record in a WAL",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t,
					opPrepare("message", `{"n":1}`), opCommit(1),
					opRaw(TypeAuditMessage, `{"message_id":"m1"}`))
				return dir, path
			},
			wantMsg: "audit_message",
		},
		{
			name: "a record type from the future",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t, opPrepare("message", `{"n":1}`), opCommit(1))
				appendRawFrame(t, path, 3, Type(4242), []byte(`{}`))
				return dir, path
			},
			wantMsg: "unknown(4242)",
		},
		{
			name: "a commit that names a commit record",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t,
					opPrepare("message", `{"n":1}`), opCommit(1), opCommit(2))
				return dir, path
			},
			wantMsg: "not an open prepare",
		},
		{
			name: "a commit that names a record after itself",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t,
					opPrepare("message", `{"n":1}`), opCommit(1), opCommit(9))
				return dir, path
			},
			wantMsg: "not earlier in the file",
		},
		{
			name: "a commit that names index 0",
			build: func(t *testing.T) (string, string) {
				dir, path, _, _ := buildWAL(t,
					opPrepare("message", `{"n":1}`), opCommit(1), opCommit(0))
				return dir, path
			},
			wantMsg: "indices start at 1",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, path := tc.build(t)
			before := readFile(t, path)

			app := &testApplier{}
			l, err := Open(LogOptions{Dir: dir, Applier: app})
			if err == nil {
				_ = l.Close()
				t.Fatal("Open succeeded on a log replay rejects; recovery must be a refusal to start")
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open err = %v, want ErrCorrupt", err)
			}
			var ce *CorruptError
			if !errors.As(err, &ce) {
				t.Fatalf("Open err = %v, want a *CorruptError", err)
			}
			if !ce.FrameIntact {
				t.Errorf("Open err has FrameIntact = false, want true: every one of these frames checksummed")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Open err %q does not contain %q", err, tc.wantMsg)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("Open err %q does not name the file", err)
			}
			// A refusal to start must not have EDITED anything: no truncation,
			// no repair, no fresh header. The bytes are evidence.
			if after := readFile(t, path); !bytes.Equal(before, after) {
				t.Errorf("a failed Open changed the WAL: %d bytes before, %d after", len(before), len(after))
			}
			// The documented partial-rebuild hazard: the good prefix reached the
			// Applier before the failure, so the caller must discard it.
			if app.count() != 1 {
				t.Errorf("Apply called %d times, want 1 (the good prefix before the damage): "+
					"Open's contract is that a failed Open may leave the Applier partially rebuilt", app.count())
			}
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
// failure can have. The bound turns it into a refusal to start with a diagnosis.
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

	t.Run("a log of nothing but prepares is refused", func(t *testing.T) {
		ops := make([]walOp, 0, n)
		for i := 0; i < n; i++ {
			ops = append(ops, opPrepare(kind, body))
		}
		_, path, _, _ := buildWAL(t, ops...)

		var c collector
		_, err := Replay(path, c.fn)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Replay of %d unresolved %d-byte prepares: err = %v, want errors.Is(err, ErrCorrupt)",
				n, perRecord, err)
		}
		var ce *CorruptError
		if !errors.As(err, &ce) {
			t.Fatalf("Replay err = %v, want a *CorruptError", err)
		}
		// The diagnosis has to say what limit was hit, or an operator has no way
		// to tell this from ordinary corruption.
		for _, want := range []string{"unresolved prepares", "bytes retained"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name the bound (%q)", err, want)
			}
		}
		if !ce.FrameIntact {
			t.Errorf("CorruptError.FrameIntact = false, want true: every frame here checksummed, " +
				"so this must never be mistaken for a truncatable tail")
		}
		if len(c.got) != 0 {
			t.Errorf("delivered %s, want nothing", showCommitted(c.got))
		}
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

	t.Run("a timestamp from disk is elided", func(t *testing.T) {
		payload := `{"kind":"message","ts":"` + strings.Repeat("t", long) + `","body":` + body + `}`
		_, path, _, _ := buildWAL(t, opRaw(TypePrepare, payload))

		_, err := Replay(path, nil)
		var ce *CorruptError
		if !errors.As(err, &ce) {
			t.Fatalf("Replay err = %v, want a *CorruptError", err)
		}
		// time.ParseError repeats the offending value twice; the rendered
		// message must be bounded regardless.
		assertElided(t, err.Error(), marker)
	})

	t.Run("a field name from disk is elided", func(t *testing.T) {
		payload := `{"kind":"message","ts":"2026-08-02T09:00:00Z","` +
			strings.Repeat("f", long) + `":1,"body":` + body + `}`
		_, path, _, _ := buildWAL(t, opRaw(TypePrepare, payload))

		_, err := Replay(path, nil)
		var ce *CorruptError
		if !errors.As(err, &ce) {
			t.Fatalf("Replay err = %v, want a *CorruptError", err)
		}
		// encoding/json's "unknown field" message carries the field name, which
		// came off disk: the bound has to survive that.
		assertElided(t, err.Error(), marker)
		// The cause is still available in full to a caller that unwraps.
		if ce.Err == nil {
			t.Error("the decoder's error was not kept as the cause")
		}
	})

	t.Run("an entry kind is elided when the applier rejects it", func(t *testing.T) {
		kind := strings.Repeat("k", long)
		dir, _, _, _ := buildWAL(t, opPrepare(kind, body), opCommit(1))

		boom := errors.New("the roster rejected a replayed entry")
		app := &testApplier{check: func(Committed) error { return boom }}
		l, err := Open(LogOptions{Dir: dir, Applier: app})
		if err == nil {
			_ = l.Close()
			t.Fatal("Open succeeded with an Applier that rejected a replayed entry")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("Open err = %v, want it to unwrap to the applier's cause", err)
		}
		assertElided(t, err.Error(), marker)
		if strings.Contains(err.Error(), kind) {
			t.Errorf("the full %d-byte kind was pasted into the error", len(kind))
		}
	})
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
