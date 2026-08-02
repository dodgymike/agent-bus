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
// is an ERROR here, not a tolerated condition: Replay says precisely where the
// file stops making sense (EndOffset), and the policy question of whether that
// tail may be truncated belongs to DUR-4. Until DUR-4 lands, a Log must refuse
// to start -- so this test also pins that nobody truncates anything yet.
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

	// DUR-4 boundary: no one truncates a corrupt tail yet, so Open fails.
	l, err := Open(LogOptions{Dir: dir})
	if err == nil {
		_ = l.Close()
		t.Fatal("Open succeeded on a torn tail: truncation policy is DUR-4 and must not have been implemented here")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open on a torn tail: err = %v, want ErrCorrupt", err)
	}
	if got := fileSize(t, path); got != lastFrameStart+lastFrameSize/2 {
		t.Errorf("the file is now %d bytes, want %d: a failed Open must not have modified it",
			got, lastFrameStart+lastFrameSize/2)
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
