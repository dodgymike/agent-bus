package wal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriterIndexContinuityAcrossReopen is the restart invariant: a reopened
// writer continues the sequence. An index is never reused, so a record written
// before a restart can never be confused with one written after it.
func TestWriterIndexContinuityAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")

	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if got := w.NextIndex(); got != 1 {
		t.Errorf("NextIndex on a fresh file = %d, want 1", got)
	}
	if got := w.Size(); got != FileHeaderSize {
		t.Errorf("Size on a fresh file = %d, want %d", got, FileHeaderSize)
	}
	if got := w.Path(); got != path {
		t.Errorf("Path() = %q, want %q", got, path)
	}
	if got := w.Kind(); got != KindWAL {
		t.Errorf("Kind() = %v, want wal", got)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Append(TypePrepare, []byte(fmt.Sprintf("before-%d", i))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	sizeBefore := w.Size()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w2, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	if got := w2.NextIndex(); got != 4 {
		t.Fatalf("NextIndex after reopen = %d, want 4 (indices must never restart or repeat)", got)
	}
	if got := w2.Size(); got != sizeBefore {
		t.Errorf("Size after reopen = %d, want %d", got, sizeBefore)
	}
	for i := 0; i < 2; i++ {
		rec, err := w2.Append(TypeCommit, []byte(fmt.Sprintf("after-%d", i)))
		if err != nil {
			t.Fatalf("Append after reopen: %v", err)
		}
		if want := uint64(4 + i); rec.Index != want {
			t.Errorf("index after reopen = %d, want %d", rec.Index, want)
		}
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	recs, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("ScanAll returned %d records, want 5", len(recs))
	}
	for i, rec := range recs {
		if rec.Index != uint64(i+1) {
			t.Errorf("record %d has index %d, want %d", i, rec.Index, i+1)
		}
	}
}

// TestWriterConcurrentAppend runs many appends at once. Every index in 1..N
// must be handed out exactly once and the file must parse cleanly, which is
// only true if index allocation, the write and the fsync are all inside one
// critical section. Run with -race.
func TestWriterConcurrentAppend(t *testing.T) {
	const n = 64
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	var (
		mu   sync.Mutex
		seen = make(map[uint64][]byte, n)
		wg   sync.WaitGroup
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("goroutine-%02d", i))
			rec, err := w.Append(TypeAuditMessage, payload)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Append from goroutine %d: %v", i, err)
				return
			}
			if prev, dup := seen[rec.Index]; dup {
				t.Errorf("index %d handed out twice: %q and %q", rec.Index, prev, payload)
				return
			}
			seen[rec.Index] = payload
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(seen) != n {
		t.Fatalf("got %d distinct indices, want %d", len(seen), n)
	}
	for i := uint64(1); i <= n; i++ {
		if _, ok := seen[i]; !ok {
			t.Errorf("index %d was never handed out", i)
		}
	}

	recs, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll after concurrent appends: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("ScanAll returned %d records, want %d", len(recs), n)
	}
	for i, rec := range recs {
		if rec.Index != uint64(i+1) {
			t.Fatalf("record %d has index %d, want %d", i, rec.Index, i+1)
		}
		if want := seen[rec.Index]; !bytes.Equal(rec.Payload, want) {
			t.Errorf("record %d payload = %q, want %q", rec.Index, rec.Payload, want)
		}
	}
}

// TestWriterPoisonedAfterWriteFailure is the safety property this task exists
// to protect: once a write has failed, the writer must never append again,
// because a second append would bury a torn record in the MIDDLE of the file
// and turn a truncatable tail into an unrecoverable log.
//
// The failure is injected by closing the descriptor out from under the writer.
// That is the in-package equivalent of the disk refusing further writes, and
// it needs no test hook in production code.
func TestWriterPoisonedAfterWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	good, err := w.Append(TypePrepare, []byte("durable"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	w.mu.Lock()
	if err := w.f.Close(); err != nil {
		w.mu.Unlock()
		t.Fatalf("closing the descriptor to inject a write failure: %v", err)
	}
	w.mu.Unlock()

	_, err = w.Append(TypeCommit, []byte("this write fails"))
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Append after a failed write: err = %v, want one matching ErrPoisoned", err)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("poison error should still unwrap to the underlying cause, got %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("poison error should name the file, got %v", err)
	}

	// Every later call refuses, too -- including ones that would otherwise
	// look harmless.
	if _, err := w.Append(TypeCommit, []byte("nor this")); !errors.Is(err, ErrPoisoned) {
		t.Errorf("second Append after poisoning: err = %v, want ErrPoisoned", err)
	}
	if err := w.Sync(); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Sync after poisoning: err = %v, want ErrPoisoned", err)
	}
	if err := w.Close(); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Close after poisoning: err = %v, want ErrPoisoned", err)
	}
	if got := w.NextIndex(); got != good.Index+1 {
		t.Errorf("NextIndex = %d, want %d: a failed append must not consume an index", got, good.Index+1)
	}

	// The record that WAS acknowledged is still there, and the file is still
	// a clean prefix of the accepted history.
	recs, end, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll after poisoning: %v", err)
	}
	if len(recs) != 1 || recs[0].Index != 1 || !bytes.Equal(recs[0].Payload, []byte("durable")) {
		t.Fatalf("ScanAll = %+v, want exactly the one acknowledged record", recs)
	}
	if want := good.Offset + good.frameSize(); end != want {
		t.Errorf("end offset = %d, want %d", end, want)
	}
}

func TestWriterRejectsBadAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Append(TypeCommit, make([]byte, MaxPayloadSize+1)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("Append(MaxPayloadSize+1) = %v, want ErrPayloadTooLarge", err)
	}
	for _, typ := range []Type{Type(0), Type(5), Type(65535)} {
		if _, err := w.Append(typ, nil); !errors.Is(err, ErrUnknownType) {
			t.Errorf("Append(Type(%d)) = %v, want ErrUnknownType", uint16(typ), err)
		}
	}
	// A rejected append consumes nothing: no index, no bytes.
	if got := w.NextIndex(); got != 1 {
		t.Errorf("NextIndex = %d, want 1", got)
	}
	if got := w.Size(); got != FileHeaderSize {
		t.Errorf("Size = %d, want %d", got, FileHeaderSize)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close = %v, want nil (Close is idempotent)", err)
	}
	if _, err := w.Append(TypeCommit, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Append after Close = %v, want ErrClosed", err)
	}
	if err := w.Sync(); !errors.Is(err, ErrClosed) {
		t.Errorf("Sync after Close = %v, want ErrClosed", err)
	}
}

func TestOpenWriterRejectsBadKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	if _, err := OpenWriter(path, Kind(0)); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("OpenWriter(Kind(0)) = %v, want ErrUnknownKind", err)
	}
	if _, err := OpenWriter(path, Kind(9)); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("OpenWriter(Kind(9)) = %v, want ErrUnknownKind", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a rejected kind must not create the file, Stat err = %v", err)
	}
	if _, _, err := ScanAll(path, Kind(9)); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("ScanAll(Kind(9)) = %v, want ErrUnknownKind", err)
	}
}

// TestOpenWriterRejectsCorruptFile: the corrupt-tail POLICY belongs to
// recovery, so the writer itself refuses to open a file it cannot fully parse
// rather than quietly appending after the damage.
func TestOpenWriterRejectsCorruptFile(t *testing.T) {
	path, recs := writeGoodLog(t)
	flipByte(t, path, recs[1].Offset+FrameHeaderSize)

	w, err := OpenWriter(path, KindWAL)
	if err == nil {
		w.Close()
		t.Fatalf("OpenWriter succeeded on a corrupt file")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenWriter error = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("offset %d", recs[1].Offset)) {
		t.Errorf("error does not name offset %d: %v", recs[1].Offset, err)
	}
}

// TestOpenWriterHealsZeroLengthFile covers the crash window between creating
// the file and writing its header. A zero-length file provably holds no
// acknowledged record, so it is initialised rather than declared corrupt --
// while ScanAll, which makes no such repairs, still calls it corrupt.
func TestOpenWriterHealsZeroLengthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := ScanAll(path, KindWAL); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ScanAll on a headerless file = %v, want ErrCorrupt", err)
	}

	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter on a zero-length file: %v", err)
	}
	if _, err := w.Append(TypePrepare, []byte("first")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recs, _, err := ScanAll(path, KindWAL)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(recs) != 1 || recs[0].Index != 1 {
		t.Fatalf("ScanAll = %+v, want one record at index 1", recs)
	}
}

// TestWriterFilePermissions: the log carries every message body, so it must
// not be world- or group-readable.
func TestWriterFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("file mode = %#o, want 0600", got)
	}
}
