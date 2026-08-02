package wal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testApplier records every Apply call and can run an extra assertion INSIDE
// Apply, which is how the ordering claim is proven rather than assumed.
type testApplier struct {
	mu    sync.Mutex
	calls []Committed
	check func(Committed) error
}

func (a *testApplier) Apply(c Committed) error {
	a.mu.Lock()
	a.calls = append(a.calls, c)
	check := a.check
	a.mu.Unlock()
	if check != nil {
		return check(c)
	}
	return nil
}

func (a *testApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *testApplier) at(i int) Committed {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[i]
}

// openTestLog opens a Log in a fresh temp dir. It NEVER touches the tracked
// data/ directory.
func openTestLog(t *testing.T, a Applier) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(LogOptions{Dir: dir, Applier: a})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, filepath.Join(dir, WALFileName)
}

// scanTypes renders a WAL as a compact "type@index" list, so a mismatch prints
// something a human can read.
func scanTypes(t *testing.T, path string) ([]Record, string) {
	t.Helper()
	recs, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll(%s): %v", path, err)
	}
	s := ""
	for i, r := range recs {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%s@%d", r.Type, r.Index)
	}
	return recs, "[" + s + "]"
}

// TestPrepareCommit is the ordering proof for the two-phase write path: one
// Write must produce exactly PREPARE then COMMIT, the commit must reference the
// prepare, and the entry must NOT be applied until the commit record is already
// on stable storage.
func TestPrepareCommit(t *testing.T) {
	var path string
	var applyErr error

	app := &testApplier{}
	// THE LOAD-BEARING ASSERTION: Apply re-reads the WAL from disk and demands
	// that the COMMIT record is ALREADY there. If anyone ever reorders this
	// path to apply before the commit fsync -- the exact bug invariant 4
	// forbids -- this fails loudly instead of silently working.
	app.check = func(c Committed) error {
		recs, _, err := ScanAll(path, KindWAL)
		if err != nil {
			applyErr = fmt.Errorf("re-scanning the WAL inside Apply: %v", err)
			return applyErr
		}
		if len(recs) == 0 {
			applyErr = errors.New("Apply ran with an empty WAL: the commit record is not durable yet")
			return applyErr
		}
		last := recs[len(recs)-1]
		if last.Type != TypeCommit || last.Index != c.CommitIndex {
			applyErr = fmt.Errorf("Apply ran before the commit record was durable: last record on disk is %s@%d, want commit@%d",
				last.Type, last.Index, c.CommitIndex)
			return applyErr
		}
		prepIdx, err := DecodeCommit(path, last)
		if err != nil {
			applyErr = fmt.Errorf("decoding the durable commit record inside Apply: %v", err)
			return applyErr
		}
		if prepIdx != c.PrepareIndex {
			applyErr = fmt.Errorf("durable commit record names prepare %d, want %d", prepIdx, c.PrepareIndex)
			return applyErr
		}
		return nil
	}

	l, p := openTestLog(t, app)
	path = p

	body := json.RawMessage(`{"to": "bus-1.agent-a", "seq": 7}`)
	before := time.Now().Add(-time.Second)
	got, err := l.Write(Entry{Kind: "message", Body: body})
	if err != nil {
		t.Fatalf("Write: %v (applier assertion: %v)", err, applyErr)
	}
	if applyErr != nil {
		t.Fatalf("in-Apply assertion failed: %v", applyErr)
	}
	if app.count() != 1 {
		t.Fatalf("Apply called %d times, want 1", app.count())
	}

	// Write must not have returned before the commit was on disk: an
	// INDEPENDENT scan, after Write returned, must see both records.
	recs, shape := scanTypes(t, path)
	if len(recs) != 2 {
		t.Fatalf("WAL holds %d records %s, want exactly [prepare commit]", len(recs), shape)
	}
	if recs[0].Type != TypePrepare || recs[1].Type != TypeCommit {
		t.Fatalf("WAL shape is %s, want [prepare@N commit@N+1]", shape)
	}
	if recs[1].Index != recs[0].Index+1 {
		t.Fatalf("commit index %d is not prepare index %d + 1", recs[1].Index, recs[0].Index)
	}
	if got.PrepareIndex != recs[0].Index || got.CommitIndex != recs[1].Index {
		t.Fatalf("Committed{PrepareIndex:%d, CommitIndex:%d}, want {%d, %d}",
			got.PrepareIndex, got.CommitIndex, recs[0].Index, recs[1].Index)
	}

	// The commit payload names the prepare -- the transaction id IS the WAL
	// index of the prepare record.
	prepIdx, err := DecodeCommit(path, recs[1])
	if err != nil {
		t.Fatalf("DecodeCommit: %v", err)
	}
	if prepIdx != recs[0].Index {
		t.Fatalf("commit payload prepare_index = %d, want %d", prepIdx, recs[0].Index)
	}

	// The prepare payload round-trips Kind and Body (compacted).
	entry, ts, err := DecodePrepare(path, recs[0])
	if err != nil {
		t.Fatalf("DecodePrepare: %v", err)
	}
	if entry.Kind != "message" {
		t.Errorf("round-tripped Kind = %q, want %q", entry.Kind, "message")
	}
	const wantBody = `{"to":"bus-1.agent-a","seq":7}`
	if string(entry.Body) != wantBody {
		t.Errorf("round-tripped Body = %s, want %s", entry.Body, wantBody)
	}
	if string(got.Entry.Body) != wantBody {
		t.Errorf("Committed body = %s, want the canonical %s", got.Entry.Body, wantBody)
	}
	if ts.Before(before) || ts.After(time.Now().Add(time.Second)) {
		t.Errorf("round-tripped ts = %s, want a timestamp from this test run", ts)
	}

	// The raw payload is exactly the documented JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recs[0].Payload, &raw); err != nil {
		t.Fatalf("prepare payload is not a JSON object: %v (%s)", err, recs[0].Payload)
	}
	for _, k := range []string{"kind", "ts", "body"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("prepare payload has no %q field: %s", k, recs[0].Payload)
		}
	}
	if len(raw) != 3 {
		t.Errorf("prepare payload has %d fields, want exactly kind/ts/body: %s", len(raw), recs[0].Payload)
	}
	if want := fmt.Sprintf(`{"prepare_index":%d}`, recs[0].Index); string(recs[1].Payload) != want {
		t.Errorf("commit payload = %s, want %s", recs[1].Payload, want)
	}
}

// TestPrepareCommitBeginThenCommit checks the explicit two-step form: after
// Begin the prepare is durable and NOTHING has been applied; only Commit makes
// the entry part of accepted history.
func TestPrepareCommitBeginThenCommit(t *testing.T) {
	app := &testApplier{}
	l, path := openTestLog(t, app)

	txn, err := l.Begin(Entry{Kind: "agent", Body: json.RawMessage(`{"id":"a"}`)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	recs, shape := scanTypes(t, path)
	if len(recs) != 1 || recs[0].Type != TypePrepare {
		t.Fatalf("after Begin the WAL is %s, want [prepare@1]", shape)
	}
	if txn.PrepareIndex() != recs[0].Index {
		t.Fatalf("PrepareIndex() = %d, want the durable prepare index %d", txn.PrepareIndex(), recs[0].Index)
	}
	if app.count() != 0 {
		t.Fatalf("Apply was called %d times after Begin, want 0: an uncommitted prepare is not accepted history", app.count())
	}

	c, err := txn.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	recs, shape = scanTypes(t, path)
	if len(recs) != 2 || recs[0].Type != TypePrepare || recs[1].Type != TypeCommit {
		t.Fatalf("after Commit the WAL is %s, want [prepare@1 commit@2]", shape)
	}
	if app.count() != 1 {
		t.Fatalf("Apply called %d times after Commit, want 1", app.count())
	}
	if applied := app.at(0); applied.PrepareIndex != c.PrepareIndex || applied.CommitIndex != c.CommitIndex {
		t.Fatalf("Apply saw {%d,%d}, want {%d,%d}",
			applied.PrepareIndex, applied.CommitIndex, c.PrepareIndex, c.CommitIndex)
	}
}

// TestPrepareCommitAbort checks that an aborted transaction leaves a durable
// ABORT record and applies nothing.
func TestPrepareCommitAbort(t *testing.T) {
	app := &testApplier{}
	l, path := openTestLog(t, app)

	txn, err := l.Begin(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := txn.Abort("recipient unknown"); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	recs, shape := scanTypes(t, path)
	if len(recs) != 2 || recs[0].Type != TypePrepare || recs[1].Type != TypeAbort {
		t.Fatalf("WAL is %s, want [prepare@1 abort@2]", shape)
	}
	if app.count() != 0 {
		t.Fatalf("Apply called %d times for an aborted transaction, want 0", app.count())
	}
	idx, reason, err := DecodeAbort(path, recs[1])
	if err != nil {
		t.Fatalf("DecodeAbort: %v", err)
	}
	if idx != recs[0].Index {
		t.Errorf("abort names prepare %d, want %d", idx, recs[0].Index)
	}
	if reason != "recipient unknown" {
		t.Errorf("abort reason = %q, want %q", reason, "recipient unknown")
	}
	if want := fmt.Sprintf(`{"prepare_index":%d,"reason":"recipient unknown"}`, recs[0].Index); string(recs[1].Payload) != want {
		t.Errorf("abort payload = %s, want %s", recs[1].Payload, want)
	}
}

// TestPrepareCommitTxnDone is the deadlock regression guard: resolving a Txn
// twice must report ErrTxnDone, write nothing, and -- critically -- not release
// the transaction lock a second time, so the Log stays usable.
func TestPrepareCommitTxnDone(t *testing.T) {
	cases := []struct {
		name  string
		first func(*Txn) error
		again func(*Txn) error
	}{
		{"commit twice",
			func(tx *Txn) error { _, err := tx.Commit(); return err },
			func(tx *Txn) error { _, err := tx.Commit(); return err }},
		{"abort twice",
			func(tx *Txn) error { return tx.Abort("first") },
			func(tx *Txn) error { return tx.Abort("second") }},
		{"commit after abort",
			func(tx *Txn) error { return tx.Abort("first") },
			func(tx *Txn) error { _, err := tx.Commit(); return err }},
		{"abort after commit",
			func(tx *Txn) error { _, err := tx.Commit(); return err },
			func(tx *Txn) error { return tx.Abort("too late") }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			app := &testApplier{}
			l, path := openTestLog(t, app)

			txn, err := l.Begin(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`)})
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if err := tc.first(txn); err != nil {
				t.Fatalf("first resolution: %v", err)
			}
			recs, _ := scanTypes(t, path)
			sizeAfterFirst := len(recs)

			err = tc.again(txn)
			if !errors.Is(err, ErrTxnDone) {
				t.Fatalf("second resolution error = %v, want ErrTxnDone", err)
			}
			if recs, shape := scanTypes(t, path); len(recs) != sizeAfterFirst {
				t.Fatalf("second resolution wrote a record: WAL is %s, want %d records", shape, sizeAfterFirst)
			}

			// If the second call had unlocked a second time, the Log's
			// transaction mutex would be released while free and the next
			// Write would either panic or wedge. Bound the wait so a
			// regression fails in seconds instead of hanging the suite.
			done := make(chan error, 1)
			go func() {
				_, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":2}`)})
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("follow-up Write after a double resolution: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("follow-up Write blocked for 10s: the transaction lock was double-unlocked or leaked")
			}
		})
	}
}

// TestPrepareCommitApplyFailureDiverges pins the hard-stop rule: an Apply that
// fails AFTER its commit record is durable means memory and disk disagree, and
// no further write may be accepted.
func TestPrepareCommitApplyFailureDiverges(t *testing.T) {
	boom := errors.New("applier rejected the entry")
	app := &testApplier{check: func(Committed) error { return boom }}
	l, path := openTestLog(t, app)

	_, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`)})
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("Write with a failing applier: err = %v, want ErrDiverged", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Write error does not unwrap to the applier's cause: %v", err)
	}

	// The commit is durable -- that is precisely why divergence is fatal.
	recs, shape := scanTypes(t, path)
	if len(recs) != 2 || recs[1].Type != TypeCommit {
		t.Fatalf("WAL is %s, want the commit record to still be durable [prepare commit]", shape)
	}

	if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":2}`)}); !errors.Is(err, ErrDiverged) {
		t.Fatalf("Write after divergence: err = %v, want ErrDiverged", err)
	}
	if _, err := l.Begin(Entry{Kind: "message", Body: json.RawMessage(`{"n":3}`)}); !errors.Is(err, ErrDiverged) {
		t.Fatalf("Begin after divergence: err = %v, want ErrDiverged", err)
	}
	if recs, shape := scanTypes(t, path); len(recs) != 2 {
		t.Fatalf("a diverged Log wrote more records: WAL is %s, want 2 records", shape)
	}
	if app.count() != 1 {
		t.Errorf("Apply called %d times, want 1: a diverged Log must not apply anything else", app.count())
	}
}

// TestPrepareCommitRejectsBadEntry checks that validation happens BEFORE any
// I/O: a rejected entry leaves the WAL byte-for-byte unchanged.
func TestPrepareCommitRejectsBadEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry Entry
		want  error
	}{
		{"truncated object", Entry{Kind: "message", Body: json.RawMessage(`{"broken":`)}, ErrInvalidBody},
		{"bare word", Entry{Kind: "message", Body: json.RawMessage(`not json`)}, ErrInvalidBody},
		{"trailing garbage", Entry{Kind: "message", Body: json.RawMessage(`{"a":1} x`)}, ErrInvalidBody},
		{"whitespace only", Entry{Kind: "message", Body: json.RawMessage("  ")}, ErrInvalidBody},
		{"empty kind", Entry{Kind: "", Body: json.RawMessage(`{"a":1}`)}, ErrInvalidKind},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			app := &testApplier{}
			l, path := openTestLog(t, app)
			// One good write first, so "unchanged" means more than "empty".
			if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"ok":true}`)}); err != nil {
				t.Fatalf("setup Write: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read WAL: %v", err)
			}

			if _, err := l.Write(tc.entry); !errors.Is(err, tc.want) {
				t.Fatalf("Write(%v): err = %v, want %v", tc.entry, err, tc.want)
			}
			if _, err := l.Begin(tc.entry); !errors.Is(err, tc.want) {
				t.Fatalf("Begin(%v): err = %v, want %v", tc.entry, err, tc.want)
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("re-read WAL: %v", err)
			}
			if string(before) != string(after) {
				t.Fatalf("a rejected entry changed the WAL: %d bytes before, %d after", len(before), len(after))
			}
			if app.count() != 1 {
				t.Errorf("Apply called %d times, want 1 (only the setup write)", app.count())
			}
			// The Log is still usable: a rejected entry must not leak the lock.
			if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"ok":2}`)}); err != nil {
				t.Fatalf("Write after a rejected entry: %v", err)
			}
		})
	}
}

// TestPrepareCommitNilBody checks the documented nil/empty/null normalisation:
// all three encode as null on disk and come back as a nil body, so a live Apply
// and a replayed Apply agree byte for byte.
func TestPrepareCommitNilBody(t *testing.T) {
	for _, body := range []json.RawMessage{nil, {}, json.RawMessage("null"), json.RawMessage(" null ")} {
		app := &testApplier{}
		l, path := openTestLog(t, app)
		c, err := l.Write(Entry{Kind: "agent", Body: body})
		if err != nil {
			t.Fatalf("Write(body=%q): %v", body, err)
		}
		if c.Entry.Body != nil {
			t.Errorf("Write(body=%q): Committed body = %s, want nil", body, c.Entry.Body)
		}
		recs, _ := scanTypes(t, path)
		if want := `"body":null`; !contains(string(recs[0].Payload), want) {
			t.Errorf("Write(body=%q): payload %s does not contain %s", body, recs[0].Payload, want)
		}
		entry, _, err := DecodePrepare(path, recs[0])
		if err != nil {
			t.Fatalf("DecodePrepare: %v", err)
		}
		if entry.Body != nil {
			t.Errorf("Write(body=%q): round-tripped body = %s, want nil", body, entry.Body)
		}
		_ = l.Close()
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestPrepareCommitDecodersAreStrict pins the rule that a payload which does
// not decode is corruption, never a silently skipped record.
func TestPrepareCommitDecodersAreStrict(t *testing.T) {
	const path = "/tmp/does-not-need-to-exist.wal" // decoders work on records, not files
	cases := []struct {
		name string
		rec  Record
		call func(Record) error
	}{
		{"commit payload is garbage",
			Record{Index: 2, Type: TypeCommit, Payload: []byte("not json")},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"commit payload has an unknown field",
			Record{Index: 2, Type: TypeCommit, Payload: []byte(`{"prepare_index":1,"extra":true}`)},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"commit payload has trailing data",
			Record{Index: 2, Type: TypeCommit, Payload: []byte(`{"prepare_index":1} {"prepare_index":1}`)},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"commit prepare_index is zero",
			Record{Index: 2, Type: TypeCommit, Payload: []byte(`{"prepare_index":0}`)},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"commit prepare_index is missing",
			Record{Index: 2, Type: TypeCommit, Payload: []byte(`{}`)},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"commit prepare_index is a forward reference",
			Record{Index: 2, Type: TypeCommit, Payload: []byte(`{"prepare_index":9}`)},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"commit decoder given a prepare record",
			Record{Index: 1, Type: TypePrepare, Payload: []byte(`{"prepare_index":1}`)},
			func(r Record) error { _, err := DecodeCommit(path, r); return err }},
		{"prepare payload has an empty kind",
			Record{Index: 1, Type: TypePrepare, Payload: []byte(`{"kind":"","ts":"2026-08-02T09:00:00Z","body":null}`)},
			func(r Record) error { _, _, err := DecodePrepare(path, r); return err }},
		{"prepare timestamp is not RFC3339Nano",
			Record{Index: 1, Type: TypePrepare, Payload: []byte(`{"kind":"message","ts":"yesterday","body":null}`)},
			func(r Record) error { _, _, err := DecodePrepare(path, r); return err }},
		{"prepare payload has an unknown field",
			Record{Index: 1, Type: TypePrepare, Payload: []byte(`{"kind":"message","ts":"2026-08-02T09:00:00Z","body":null,"x":1}`)},
			func(r Record) error { _, _, err := DecodePrepare(path, r); return err }},
		{"abort prepare_index is zero",
			Record{Index: 2, Type: TypeAbort, Payload: []byte(`{"prepare_index":0,"reason":"x"}`)},
			func(r Record) error { _, _, err := DecodeAbort(path, r); return err }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(tc.rec)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want a CorruptError", err)
			}
			var ce *CorruptError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want it to unwrap to *CorruptError", err)
			}
		})
	}
}

// TestPrepareCommitPoisonSurfaces checks requirement 5: a poisoned Writer is
// reported by the Log, not swallowed. The file descriptor is closed underneath
// the Writer to force a failed write.
func TestPrepareCommitPoisonSurfaces(t *testing.T) {
	app := &testApplier{}
	l, _ := openTestLog(t, app)

	if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`)}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := l.w.f.Close(); err != nil { // simulate the descriptor failing
		t.Fatalf("closing the underlying file: %v", err)
	}

	if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":2}`)}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Write on a failing file: err = %v, want ErrPoisoned", err)
	}
	if _, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":3}`)}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("second Write after poisoning: err = %v, want ErrPoisoned", err)
	}
	if app.count() != 1 {
		t.Errorf("Apply called %d times, want 1: nothing may be applied once the writer is poisoned", app.count())
	}
}

// TestPrepareCommitConcurrent runs N concurrent writers. Transactions are
// serialised, so the WAL must hold exactly N strictly alternating
// prepare/commit pairs with no interleaving, and Apply must run exactly N
// times. Run with -race.
func TestPrepareCommitConcurrent(t *testing.T) {
	const n = 16

	app := &testApplier{}
	l, path := openTestLog(t, app)

	var wg sync.WaitGroup
	results := make([]Committed, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = l.Write(Entry{
				Kind: "message",
				Body: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Write %d: %v", i, err)
		}
	}
	if app.count() != n {
		t.Fatalf("Apply called %d times, want %d", app.count(), n)
	}

	recs, shape := scanTypes(t, path)
	if len(recs) != 2*n {
		t.Fatalf("WAL holds %d records, want %d: %s", len(recs), 2*n, shape)
	}
	for i := 0; i < n; i++ {
		prep, commit := recs[2*i], recs[2*i+1]
		if prep.Type != TypePrepare || commit.Type != TypeCommit {
			t.Fatalf("records %d/%d are %s/%s, want prepare/commit -- transactions interleaved: %s",
				2*i, 2*i+1, prep.Type, commit.Type, shape)
		}
		idx, err := DecodeCommit(path, commit)
		if err != nil {
			t.Fatalf("DecodeCommit(record %d): %v", commit.Index, err)
		}
		if idx != prep.Index {
			t.Fatalf("commit %d names prepare %d, want the prepare that precedes it, %d",
				commit.Index, idx, prep.Index)
		}
	}

	// Every write got a distinct, real transaction id.
	seen := make(map[uint64]bool, n)
	for i, c := range results {
		if c.CommitIndex != c.PrepareIndex+1 {
			t.Errorf("write %d: commit index %d is not prepare index %d + 1", i, c.CommitIndex, c.PrepareIndex)
		}
		if seen[c.PrepareIndex] {
			t.Fatalf("write %d reused prepare index %d", i, c.PrepareIndex)
		}
		seen[c.PrepareIndex] = true
	}
}
