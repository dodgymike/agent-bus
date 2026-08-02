package wal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// fileMode is the permission bits for a newly created log. 0600: the log is
// the audit trail and holds every message body, so it is readable only by the
// user the server runs as.
const fileMode os.FileMode = 0600

// Writer is an append-only file writer whose Append does not return until the
// bytes are on stable storage.
//
// It is safe for concurrent use. A single mutex is held across index
// allocation, frame assembly, the write and the fsync, so two appends can
// never interleave their bytes or claim the same index.
type Writer struct {
	path string
	kind Kind

	mu       sync.Mutex
	f        *os.File
	next     uint64 // index the next Append will use
	size     int64  // offset the next Append will write at
	closed   bool
	poisoned error // non-nil once a write or fsync has failed
}

// OpenWriter opens path for appending records of the given kind.
//
// If the file does not exist it is created 0600 with a fresh file header, and
// the PARENT DIRECTORY is fsynced so that the file's existence -- not just its
// contents -- survives a crash. If it does exist, its header is validated and
// its records are scanned strictly (see ScanAll) to establish the next index
// and the append offset; any malformed record is an error here. Deciding what
// damage may be removed is recovery policy and belongs to RepairLog, which does
// its work -- and verifies its own result -- before OpenWriter is called (see
// Open).
//
// A zero-length existing file is the one case treated as fresh rather than
// corrupt: it is the crash window between creating the file and writing its
// header, it provably contains no record, and so nothing that was ever
// acknowledged can be lost by initialising it.
func OpenWriter(path string, kind Kind) (*Writer, error) {
	if kind.magic() == "" {
		return nil, fmt.Errorf("wal: open %s: %w: %s", path, ErrUnknownKind, kind)
	}

	// O_EXCL rather than a stat-then-open: it makes "did I create this file?"
	// an atomic answer from the kernel instead of a guess with a race in it.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err == nil {
		if err := initFile(f, path, kind); err != nil {
			f.Close()
			return nil, err
		}
		return &Writer{path: path, kind: kind, f: f, next: 1, size: FileHeaderSize}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("wal: create %s: %w", path, err)
	}

	f, err = os.OpenFile(path, os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("wal: open %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	if fi.Size() == 0 {
		if err := initFile(f, path, kind); err != nil {
			f.Close()
			return nil, err
		}
		return &Writer{path: path, kind: kind, f: f, next: 1, size: FileHeaderSize}, nil
	}

	// Scan without retaining the records: the writer only needs the last index
	// and the end offset, and a log may be far larger than memory.
	last := uint64(0)
	end, err := scanFrom(f, path, kind, func(rec Record) error {
		last = rec.Index
		return nil
	})
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{path: path, kind: kind, f: f, next: last + 1, size: end}, nil
}

// initFile writes and fsyncs the file header, then fsyncs the parent
// directory. Both syncs are required: the first makes the header durable, the
// second makes the directory entry that names the file durable.
func initFile(f *os.File, path string, kind Kind) error {
	hdr := makeFileHeader(kind)
	if n, err := f.WriteAt(hdr, 0); err != nil || n != len(hdr) {
		return fmt.Errorf("wal: write file header to %s at offset 0: %w", path, shortWrite(err, n, len(hdr)))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("wal: fsync %s: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("wal: fsync directory of %s: %w", path, err)
	}
	return nil
}

// syncDir fsyncs a directory so that a file created in it is durably named.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// shortWrite turns a partial write into an error, so a write that returns
// (n < len, nil) is never mistaken for success.
func shortWrite(err error, n, want int) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: wrote %d of %d bytes", io.ErrShortWrite, n, want)
}

// Append writes one framed record and FSYNCS BEFORE RETURNING. A caller that
// has seen Append return a nil error may treat the record as durable
// (invariant 4: nothing is acknowledged before it is durable).
//
// The frame is assembled in a buffer and issued as a single write, so the
// smallest unit the kernel ever sees is one whole record.
//
// If the write is short or fails, or the fsync fails, the file may now carry a
// torn tail and the Writer is POISONED: this and every subsequent Append or
// Sync returns an error matching ErrPoisoned. Appending after a torn write
// would bury the damage in the MIDDLE of the file, and while recovery can now
// salvage that -- it discards the damaged record and keeps the ones behind it
// -- salvaging is a lossy answer to a problem the writer can simply decline to
// create. So the writer still refuses to keep going.
func (w *Writer) Append(t Type, payload []byte) (Record, error) {
	if !t.Known() {
		return Record{}, fmt.Errorf("wal: append to %s: %w: %s", w.path, ErrUnknownType, t)
	}
	if len(payload) > MaxPayloadSize {
		return Record{}, fmt.Errorf("wal: append to %s: %w: payload is %d bytes, maximum is %d",
			w.path, ErrPayloadTooLarge, len(payload), MaxPayloadSize)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.poisoned != nil {
		return Record{}, w.poisoned
	}
	if w.closed {
		return Record{}, fmt.Errorf("wal: append to %s: %w", w.path, ErrClosed)
	}

	off, index := w.size, w.next
	frame := encodeFrame(index, t, payload)

	if n, err := w.f.WriteAt(frame, off); err != nil || n != len(frame) {
		return Record{}, w.poison(fmt.Errorf("wal: append to %s at offset %d: %w",
			w.path, off, shortWrite(err, n, len(frame))))
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, w.poison(fmt.Errorf("wal: fsync %s after appending at offset %d: %w", w.path, off, err))
	}

	w.next++
	w.size += int64(len(frame))
	// The payload is the writer's own copy inside frame, never the caller's
	// slice, so a caller that reuses its buffer cannot mutate a returned
	// Record out from under the reader that later replays it.
	return Record{Index: index, Type: t, Payload: frame[FrameHeaderSize:], Offset: off}, nil
}

// poison latches the first failure and returns the error to report. The caller
// must hold w.mu.
func (w *Writer) poison(cause error) error {
	if w.poisoned == nil {
		w.poisoned = &poisonError{path: w.path, cause: cause}
	}
	return w.poisoned
}

// poisonError reports a Writer that failed mid-write. errors.Is matches
// ErrPoisoned, and Unwrap still reaches the underlying I/O error so a caller
// can also ask whether the disk was full or the descriptor was closed.
type poisonError struct {
	path  string
	cause error
}

func (e *poisonError) Error() string {
	return fmt.Sprintf("wal: %s: writer poisoned by an earlier failed write or fsync; the file may have a torn tail and must not be appended to: %v",
		e.path, e.cause)
}

func (e *poisonError) Is(target error) bool { return target == ErrPoisoned }

func (e *poisonError) Unwrap() error { return e.cause }

// NextIndex reports the index the next Append will use.
func (w *Writer) NextIndex() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.next
}

// Size reports the current end offset of the file, which is where the next
// Append will write.
func (w *Writer) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// Path returns the file the Writer appends to.
func (w *Writer) Path() string { return w.path }

// Kind returns the file kind the Writer was opened for.
func (w *Writer) Kind() Kind { return w.kind }

// Sync fsyncs the file. Every Append already syncs, so this is only needed by
// a caller that wants to re-assert durability; a failure poisons the Writer.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.poisoned != nil {
		return w.poisoned
	}
	if w.closed {
		return fmt.Errorf("wal: sync %s: %w", w.path, ErrClosed)
	}
	if err := w.f.Sync(); err != nil {
		return w.poison(fmt.Errorf("wal: fsync %s: %w", w.path, err))
	}
	return nil
}

// Close syncs and closes the file. It is idempotent. If the Writer is
// poisoned, Close still closes the descriptor but reports the poison error, so
// a caller that only checks Close still learns the file is suspect.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true

	first := w.poisoned
	if first == nil {
		if err := w.f.Sync(); err != nil {
			first = w.poison(fmt.Errorf("wal: fsync %s on close: %w", w.path, err))
		}
	}
	if err := w.f.Close(); err != nil && first == nil {
		first = fmt.Errorf("wal: close %s: %w", w.path, err)
	}
	return first
}
