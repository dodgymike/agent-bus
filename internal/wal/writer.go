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
	// c is the on-disk format this Writer appends in. It is ALWAYS the current
	// version: OpenWriter refuses a format version 1 file outright, so a Writer
	// can never emit a legacy frame.
	c codec

	mu       sync.Mutex
	f        *os.File
	next     uint64 // index the next Append will use
	size     int64  // offset the next Append will write at
	closed   bool
	poisoned error // non-nil once a write or fsync has failed

	// floor is the DURABLE index floor for the data directory, or nil.
	//
	// It is nil for the AUDIT log, for a Writer built by the exported
	// OpenWriter, and in tests; nil means no reservation is made and the
	// behaviour is exactly what it was before the floor existed. Only wal.Open
	// attaches one, because only Open knows the data directory and only the WAL
	// feeds internal/hub's message-sequence floor.
	floor *indexFloor
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
	c, err := resolveCodec(path, kind, nil)
	if err != nil {
		return nil, err
	}
	// THERE IS NO DOWNGRADE WRITE. A version 1 log is READ so it can be
	// upgraded, never appended to: mixing versions inside one file is
	// impossible by construction (the version lives in the file header), so
	// appending here would mean writing a legacy frame for ever. Open performs
	// the upgrade at startup and therefore never reaches this.
	if c.isV1() {
		return nil, fmt.Errorf("wal: open %s for appending: the file is in on-disk format version %d; it must be upgraded to version %d before it can be appended to -- wal.Open performs that upgrade at startup",
			path, formatVersionV1, FormatVersion)
	}
	return openWriter(path, kind, c)
}

// openWriter is OpenWriter with the codec already resolved, so that Open loads
// the MAC key exactly once for the whole of recovery.
func openWriter(path string, kind Kind, c codec) (*Writer, error) {
	// O_EXCL rather than a stat-then-open: it makes "did I create this file?"
	// an atomic answer from the kernel instead of a guess with a race in it.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	if err == nil {
		if err := initFile(f, path, kind, c); err != nil {
			f.Close()
			return nil, err
		}
		return &Writer{path: path, kind: kind, c: c, f: f, next: 1, size: c.fileHeaderSize()}, nil
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
		if err := initFile(f, path, kind, c); err != nil {
			f.Close()
			return nil, err
		}
		return &Writer{path: path, kind: kind, c: c, f: f, next: 1, size: c.fileHeaderSize()}, nil
	}

	// Scan without retaining the records: the writer only needs the last index
	// and the end offset, and a log may be far larger than memory.
	last := uint64(0)
	end, err := scanFrom(c, f, path, kind, func(rec Record) error {
		last = rec.Index
		return nil
	})
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{path: path, kind: kind, c: c, f: f, next: last + 1, size: end}, nil
}

// initFile writes and fsyncs the file header, then fsyncs the parent
// directory. Both syncs are required: the first makes the header durable, the
// second makes the directory entry that names the file durable.
func initFile(f *os.File, path string, kind Kind, c codec) error {
	hdr := c.makeFileHeader(kind)
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

	// THE INDEX IS MADE DURABLE BEFORE IT IS USED, never after. That ordering is
	// the whole of the guarantee: a number that reached a frame was authorised on
	// stable storage first, so no crash, no tail repair and no quarantine can
	// leave the next start believing it is free. It is invariant 4's ordering
	// ("nothing is acknowledged before it is durable") applied one layer down, to
	// the id rather than the record.
	//
	// A failure to reserve POISONS the Writer. A writer that cannot durably
	// reserve must not write: appending anyway would stamp an index nothing has
	// authorised into the log, which is precisely the state the floor exists to
	// make impossible. Fail-closed, in the same voice as the existing poison
	// discipline -- the first failure latches and every later Append reports it.
	//
	// The cost is amortised: reserve only touches the disk when the index passes
	// the durable ceiling, which is once per indexReserveBlock appends.
	if w.floor != nil {
		if err := w.floor.reserve(w.next); err != nil {
			return Record{}, w.poison(fmt.Errorf("wal: append to %s: reserving record index %d in the durable index floor %s: %w",
				w.path, w.next, w.floor.Path(), err))
		}
	}

	off, index := w.size, w.next
	frame := w.c.encodeFrame(index, t, payload)

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
	return Record{Index: index, Type: t, Payload: frame[w.c.frameHeaderSize():], Offset: off}, nil
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

// advanceIndexTo raises the index the next Append will use to n. It NEVER
// lowers it: an n at or below the current value is a no-op.
//
// Lowering would reissue an index, which invariant 1 forbids, so a caller
// passing a lower n is a PROGRAMMING ERROR rather than a request -- it is
// ignored here rather than honoured, because the one thing this must not do is
// obey. Open is the only caller: it raises the writer to the maximum of the
// file's arithmetic and the durable floor.
func (w *Writer) advanceIndexTo(n uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n > w.next {
		w.next = n
	}
}

// setIndexFloor attaches the durable index floor. It is called by Open, once,
// before the Writer is reachable by anything that could append.
func (w *Writer) setIndexFloor(f *indexFloor) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.floor = f
}

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
//
// On a CLEAN close it also SEALS the durable index floor, recording that every
// index up to the last one written is burned. The seal happens AFTER the file's
// own fsync and only if that fsync succeeded: the floor's claim is "these
// indices are in the log", so it must not be made until the log's bytes are
// durable. If the writer is POISONED the floor is deliberately NOT sealed -- we
// do not know what reached the file, and leaving the reservation standing is the
// conservative side (the next start resumes higher, burning at most a block,
// which is free; resuming lower is the defect).
//
// A seal failure is folded into the returned error exactly as a failed sync is,
// so a caller that only checks Close still learns the floor is suspect.
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
	if first == nil && w.floor != nil {
		// w.next-1 is the highest index actually written; w.next is 1 and this is
		// 0 when nothing was appended, which seal treats as "raise nothing".
		if err := w.floor.seal(w.next - 1); err != nil {
			first = fmt.Errorf("wal: close %s: sealing the durable index floor %s: %w", w.path, w.floor.Path(), err)
		}
	}
	if err := w.f.Close(); err != nil && first == nil {
		first = fmt.Errorf("wal: close %s: %w", w.path, err)
	}
	return first
}
