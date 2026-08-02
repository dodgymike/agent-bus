package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// Log is the two-phase (prepare -> commit) write path: the one way an
// application-level change becomes part of accepted history.
//
// Every durable change costs exactly two fsynced appends to the WAL:
//
//  1. a PREPARE record carrying the change itself. The change now EXISTS on
//     disk but is not yet accepted history, so a crash here loses nothing that
//     was ever acknowledged.
//  2. a COMMIT record naming that prepare. The change is now accepted history.
//
// Only once the COMMIT record is fsynced is the change applied to the in-memory
// serving copy, and only once THAT has happened does the write return to its
// caller. That order is the whole point of this type (invariants 4 and 5):
// disk is the truth, memory is the serving copy, and nothing is acknowledged
// before it is durable. A reviewer should be able to read Write, Begin and
// Commit as a straight line with nowhere to reorder.
//
// THERE IS NO SEPARATE TRANSACTION-ID SPACE: the transaction id IS the WAL
// index of the PREPARE record. That is a deliberate design choice, not an
// omission. The server mints WAL indices, so a transaction id is
// server-authoritative by construction (invariant 1), it is unique for the life
// of the file because indices are never reused, and there is no second counter
// that would itself have to be made durable and kept in step.
type Log struct {
	w       *Writer
	applier Applier
	logger  *logging.Logger
	now     func() time.Time

	// recovered is what the replay at Open found. It is immutable after Open.
	recovered Recovered

	// mu serialises TRANSACTIONS, not just appends. Begin acquires it and
	// Commit or Abort releases it, so at most one transaction is in flight and
	// the WAL can never interleave two prepares -- which is what lets a
	// recovery pass pair a COMMIT with its PREPARE by looking no further than
	// the record it names.
	//
	// Go mutexes are not owner-bound, so unlocking from a different goroutine
	// than the one that locked is legal and intended here: Begin may return a
	// Txn that another goroutine commits.
	//
	// The cost is a hard ceiling of one transaction per two fsyncs. That is the
	// correct trade for now (invariant 8: simple beats clever). The known
	// future optimisation is GROUP COMMIT -- batching several prepares behind
	// one commit fsync -- which is deliberately NOT implemented here: it needs
	// its own design and its own crash-injection tests.
	mu sync.Mutex

	// diverged is set, and never cleared, when an Applier rejects an entry
	// whose COMMIT record is already durable. It is read and written only with
	// mu held (a transaction holds mu from Begin through Commit).
	diverged error
}

// Entry is one application-level durable change. wal does not interpret Kind
// or Body; they are the application's business.
type Entry struct {
	// Kind is the application discriminator: "message", "agent", ... It must
	// not be empty: an empty discriminator is rejected on the way in AND on the
	// way out, so this package can never write a record it would refuse to
	// replay.
	Kind string
	// Body is the application payload. It MUST be valid JSON so the durable
	// record stays human-auditable with `head -c` and a JSON pretty-printer;
	// Write rejects invalid JSON. nil/empty is allowed and encodes as null.
	//
	// The body is stored COMPACTED (insignificant whitespace removed) and a
	// body of "null" is normalised to nil, so an entry handed to Apply by a
	// live write is byte-for-byte the entry a replay will hand to Apply.
	Body json.RawMessage
	// Audit, when non-nil, requests an audit-log record for this entry.
	// DUR-5 implements it; this task only carries the field. The audit record
	// is metadata and routing info ONLY -- never the message body (invariant 6,
	// corrected 2026-08-02: the bus is getting E2E encryption with forward
	// secrecy, so the audit trail is a provenance record, not a content
	// archive).
	Audit *AuditRecord
}

// AuditRecord requests an append-only audit-log record for an Entry.
//
// It is a PLACEHOLDER in this task: DUR-5 fills in its fields (message id,
// sequence, sender, recipients, bus path, timestamp, size, content hash) and
// writes the record. It exists now only so that the Entry shape does not have
// to change when DUR-5 lands.
type AuditRecord struct{}

// Committed describes an entry that reached commit and is durable.
type Committed struct {
	// PrepareIndex is the WAL index of the PREPARE record, which is also the
	// transaction id.
	PrepareIndex uint64
	// CommitIndex is the WAL index of the COMMIT record.
	CommitIndex uint64
	// Entry is the change, canonicalised exactly as it was written to disk.
	Entry Entry
}

// Applier applies a committed entry to the in-memory serving copy.
//
// Apply is called in exactly two situations, and it cannot tell them apart --
// which is the point, because memory must end up the same either way:
//
//   - during a live write, with the Log's transaction lock held and ONLY after
//     the entry's COMMIT record has been fsynced;
//   - during recovery at Open, once per committed entry in the durable log, in
//     commit order, before Open returns.
//
// It must therefore be quick and it must not call back into the Log. Returning
// an error is a hard failure: from a live write it poisons the Log (see
// ErrDiverged), and from recovery it makes Open fail, because a memory state
// that cannot be rebuilt from disk must not be served.
type Applier interface {
	Apply(Committed) error
}

// LogOptions configures Open.
type LogOptions struct {
	// Dir is the data directory; the WAL is Dir/bus.wal. It is created 0700 if
	// it does not exist.
	Dir string
	// Applier receives every committed entry: first every entry already in the
	// durable log, replayed in commit order at Open, then every entry written
	// afterwards. It may be nil, in which case changes are recorded durably and
	// applied nowhere -- useful for tests and for a server that only wants the
	// durable record.
	Applier Applier
	// Logger receives divergence and lifecycle events. It may be nil.
	Logger *logging.Logger
	// Now supplies the prepare-record timestamp. It defaults to time.Now.
	Now func() time.Time
}

// WALFileName is the name of the write-ahead log inside the data directory.
const WALFileName = "bus.wal"

// dirMode is the permission bits for a data directory created by Open. 0700
// for the same reason the log file itself is 0600: it is the durable record of
// everything the bus has accepted.
const dirMode os.FileMode = 0700

// Log-level sentinel errors. All are checkable with errors.Is. The framing
// sentinels (ErrPoisoned, ErrCorrupt, ErrClosed, ...) live in format.go and are
// surfaced unchanged by this layer -- a poisoned Writer is never swallowed.
var (
	// ErrDiverged reports a Log whose in-memory state no longer matches its
	// durable state, because an Applier rejected an entry AFTER that entry's
	// COMMIT record was already fsynced.
	//
	// This is deliberately a HARD STOP rather than a retry. The commit record
	// is durable, so on the next start recovery WILL replay that entry: disk
	// says the change happened, memory says it did not, and every subsequent
	// write would be computed from the wrong memory and compound the
	// divergence. There is no safe way to reconcile that from inside the
	// process, so the Log poisons itself: every later Write and Begin returns
	// ErrDiverged, the operator sees an ERROR log line, and the fix is to
	// restart and rebuild memory from disk.
	ErrDiverged = errors.New("wal: in-memory state has diverged from the durable log")

	// ErrTxnDone reports a Txn that has already been committed or aborted. A
	// second Commit or Abort is a no-op that reports this: it must never write
	// a second record and must never release the transaction lock twice.
	ErrTxnDone = errors.New("wal: transaction is already committed or aborted")

	// ErrInvalidBody reports an Entry.Body that is not valid JSON. It is
	// detected BEFORE anything is written, so a rejected entry leaves the WAL
	// byte-for-byte unchanged.
	ErrInvalidBody = errors.New("wal: entry body is not valid JSON")

	// ErrInvalidKind reports an empty Entry.Kind.
	ErrInvalidKind = errors.New("wal: entry kind is empty")
)

// Open opens (or creates) the write-ahead log in opts.Dir, REPLAYING it first:
// every entry that reached commit is handed to opts.Applier, in commit order,
// before Open returns, so a Log is never serving from a memory state that disk
// does not justify. Prepares that never committed are discarded and their
// indices are never reissued. See Replay, and Log.Recovered for what the
// replay found.
//
// IF OPEN RETURNS AN ERROR, THE APPLIER MAY HAVE BEEN PARTIALLY REBUILT: replay
// applies entries as it walks the file, so a failure part-way leaves the caller
// holding a fragment of the durable state. That fragment is not a prefix of
// anything and must be thrown away -- discard the Applier along with the failed
// Log, and never retry Open onto the same one, or the surviving entries will be
// applied twice.
func Open(opts LogOptions) (*Log, error) {
	if opts.Dir == "" {
		return nil, errors.New("wal: open log: Dir is empty")
	}
	if err := os.MkdirAll(opts.Dir, dirMode); err != nil {
		return nil, fmt.Errorf("wal: create data directory %s: %w", opts.Dir, err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	path := filepath.Join(opts.Dir, WALFileName)

	// ---------------------------------------------------------------------
	// RECOVERY (invariant 5: disk is the truth, memory is the serving copy).
	// Replay runs BEFORE the writer is opened and before anything can be
	// appended: it walks the existing file, pairs every COMMIT with the PREPARE
	// it names, discards prepares that never committed and those explicitly
	// aborted, and hands each surviving entry to opts.Applier in commit order,
	// so the Apply sequence after a restart is the one the previous process
	// made. Nothing else in this package makes an entry visible, so an
	// uncommitted prepare cannot survive a restart.
	//
	// A replay failure is a REFUSAL TO START, deliberately. Either the log
	// cannot be interpreted, or the Applier rejected an entry the log says was
	// accepted; in both cases memory cannot be rebuilt from disk, and serving
	// anyway would mean serving a state that is not a prefix of accepted
	// history.
	//
	// Corrupt-tail policy runs FIRST, in RepairTail, before a single entry is
	// handed to the Applier. It is a framing-only pass: if the file ends in a
	// torn frame -- the signature of a crash mid-append -- it cuts the file back
	// to the end of the last verified-good record and fsyncs that, so the replay
	// below sees a file that is a clean prefix of accepted history. Nothing that
	// was ever acknowledged is inside the discarded region, because Append only
	// returns after its fsync. Damage anywhere but the tail, and damage in a
	// frame whose checksum verified, is NOT repaired: RepairTail returns the
	// error and Open refuses to start. That is the only truncation this package
	// performs (invariant 6).
	// ---------------------------------------------------------------------
	repair, err := RepairTail(path, KindWAL, opts.Logger)
	if err != nil {
		return nil, err // RepairTail already names the path and the offset
	}

	var apply func(Committed) error
	if opts.Applier != nil {
		apply = opts.Applier.Apply
	}
	rec, err := Replay(path, apply)
	if err != nil {
		return nil, err // Replay already names the path and the offset
	}
	rec.Repaired = repair

	w, err := OpenWriter(path, KindWAL)
	if err != nil {
		return nil, err // OpenWriter already names the path
	}
	// Replay and OpenWriter read the same file in two passes, so they must agree
	// about where it ends. If they do not, the file changed between the passes
	// and the writer is about to append at an offset computed from a file that
	// no longer exists as it was read -- so this is fatal rather than a warning.
	// (EndOffset 0 means the file did not exist at replay time and OpenWriter
	// created it, which is not a disagreement.)
	//
	// THIS IS NOT A LOCK, and must not be mistaken for one. It only catches a
	// change inside the window between the two passes; two servers started on
	// the same data directory can both replay the same bytes, both agree, and
	// then both append at the same offsets, which destroys the log. Excluding a
	// second process needs a real lock on the data directory (an flock held for
	// the Log's lifetime) and is a follow-up, not something this check does.
	if w.NextIndex() != rec.NextIndex || (rec.EndOffset != 0 && w.Size() != rec.EndOffset) {
		wErr := w.Close()
		return nil, fmt.Errorf("wal: open log %s: the file changed between replay and open: replay ended at index %d offset %d, the writer sees index %d offset %d; another process may be writing to this data directory (close: %v)",
			path, rec.NextIndex, rec.EndOffset, w.NextIndex(), w.Size(), wErr)
	}

	if rec.Records > 0 {
		opts.Logger.Info("wal replayed",
			"path", path, "records", rec.Records, "applied", rec.Applied,
			"aborted", rec.Aborted, "dangling", len(rec.Dangling), "next_index", rec.NextIndex)
	}
	// Worth an operator's attention: a dangling prepare is what a crash between
	// the prepare fsync and the commit fsync looks like, and the client that was
	// waiting on that write never got an answer. Only the first few are named --
	// the count above is the complete figure, and a damaged file must not be
	// able to turn one restart into thousands of log lines.
	for i, prepareIndex := range rec.Dangling {
		if i == maxDanglingLogged {
			opts.Logger.Warn("wal replay discarded further uncommitted prepares",
				"path", path, "not_logged", len(rec.Dangling)-maxDanglingLogged)
			break
		}
		opts.Logger.Warn("wal replay discarded an uncommitted prepare",
			"path", path, "prepare_index", prepareIndex)
	}

	return &Log{w: w, applier: opts.Applier, logger: opts.Logger, now: now, recovered: rec}, nil
}

// maxDanglingLogged bounds how many discarded prepares Open names individually.
const maxDanglingLogged = 8

// Recovered reports what the replay at Open found. It is set once, before Open
// returns, and never changes: the Dangling slice is copied out, so a caller
// cannot reach back through it and edit the Log's record of its own recovery.
func (l *Log) Recovered() Recovered {
	r := l.recovered
	if r.Dangling != nil {
		r.Dangling = append([]uint64(nil), r.Dangling...)
	}
	return r
}

// Path returns the WAL file the Log appends to.
func (l *Log) Path() string { return l.w.Path() }

// Close closes the underlying WAL.
//
// It blocks until any in-flight transaction has committed or aborted, because
// closing the file underneath an open prepare would leave a caller holding a
// Txn that can no longer be resolved. It is idempotent.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Close()
}

// Write records one entry durably and applies it. It is the normal path: the
// whole two-phase cycle in one call.
//
// In order, and with no opportunity to reorder:
//
//	Begin  -- encode the prepare, append it, FSYNC
//	Commit -- append the commit record, FSYNC, then Apply, then return
//
// A nil error means the entry is on stable storage AND visible in memory. Only
// then may the caller acknowledge it (invariant 4).
func (l *Log) Write(e Entry) (Committed, error) {
	t, err := l.Begin(e)
	if err != nil {
		return Committed{}, err
	}
	// Begin holds the transaction lock; Commit always releases it, on every
	// path, via defer. Write therefore cannot leak the lock.
	return t.Commit()
}

// Begin runs phase one: it appends the PREPARE record and RETURNS ONLY AFTER
// THAT RECORD IS FSYNCED.
//
// On success the caller holds the Log's transaction lock and must resolve the
// transaction with exactly one Commit or Abort. On any error nothing is left
// locked and, for a validation error, nothing at all has been written.
func (l *Log) Begin(e Entry) (*Txn, error) {
	// Validation happens BEFORE the lock and before any I/O, so a bad entry
	// leaves the WAL byte-for-byte unchanged and does not stall other writers.
	if e.Kind == "" {
		return nil, fmt.Errorf("wal: prepare in %s: %w", l.Path(), ErrInvalidKind)
	}
	body, err := canonicalBody(e.Body)
	if err != nil {
		return nil, fmt.Errorf("wal: prepare in %s: %w: %v", l.Path(), ErrInvalidBody, err)
	}

	l.mu.Lock()
	// The lock is released here on every FAILURE path and kept on the success
	// path, where it becomes the Txn's to release.
	handedOver := false
	defer func() {
		if !handedOver {
			l.mu.Unlock()
		}
	}()

	if l.diverged != nil {
		return nil, l.diverged
	}

	payload, err := encodePrepare(e.Kind, body, l.now())
	if err != nil {
		return nil, fmt.Errorf("wal: prepare in %s: encode payload: %w", l.Path(), err)
	}
	rec, err := l.w.Append(TypePrepare, payload)
	if err != nil {
		// Includes ErrPoisoned from a torn write: surfaced, never swallowed.
		return nil, fmt.Errorf("wal: prepare in %s: %w", l.Path(), err)
	}

	handedOver = true
	return &Txn{l: l, prepareIndex: rec.Index, entry: Entry{Kind: e.Kind, Body: body, Audit: e.Audit}}, nil
}

// Txn is one in-flight two-phase write, between prepare and commit. Its
// transaction id is the WAL index of its PREPARE record.
//
// A Txn holds the Log's transaction lock. It must be resolved with exactly one
// call to Commit or Abort; the second and later calls return ErrTxnDone and do
// nothing at all, in particular they do not unlock a second time.
type Txn struct {
	l            *Log
	prepareIndex uint64
	entry        Entry

	// done is claimed with an atomic compare-and-swap rather than guarded by
	// l.mu, because l.mu is HELD for the lifetime of the transaction and the
	// resolving call is the one that releases it. The CAS is what guarantees
	// exactly one of Commit/Abort ever proceeds, and therefore exactly one
	// Unlock, even if two goroutines race to resolve the same Txn.
	done int32
}

// PrepareIndex returns the WAL index of the PREPARE record, which is this
// transaction's id.
func (t *Txn) PrepareIndex() uint64 { return t.prepareIndex }

// Commit runs phase two. It appends the COMMIT record, RETURNS ONLY AFTER THAT
// RECORD IS FSYNCED and only after Applier.Apply has run.
//
// The order below is load-bearing and must not be rearranged: append+fsync the
// commit record FIRST, apply to memory SECOND, return THIRD. Applying first
// would make the change visible -- and possibly acknowledged -- before it was
// durable, which is exactly what invariant 4 forbids.
func (t *Txn) Commit() (Committed, error) {
	if !atomic.CompareAndSwapInt32(&t.done, 0, 1) {
		return Committed{}, fmt.Errorf("wal: commit prepare %d in %s: %w",
			t.prepareIndex, t.l.Path(), ErrTxnDone)
	}
	l := t.l
	defer l.mu.Unlock()

	// DUR-5 AUDIT SEAM. When t.entry.Audit is non-nil the audit record is
	// written and fsynced HERE, between prepare-fsync and commit-fsync, so the
	// audit log is a SUPERSET of committed history: an entry can appear in the
	// audit trail without having committed, but never the reverse.

	payload, err := encodeCommit(t.prepareIndex)
	if err != nil {
		return Committed{}, fmt.Errorf("wal: commit prepare %d in %s: encode payload: %w",
			t.prepareIndex, l.Path(), err)
	}
	rec, err := l.w.Append(TypeCommit, payload)
	if err != nil {
		return Committed{}, fmt.Errorf("wal: commit prepare %d in %s: %w", t.prepareIndex, l.Path(), err)
	}

	// --- the entry is accepted history from this line onwards ---

	c := Committed{PrepareIndex: t.prepareIndex, CommitIndex: rec.Index, Entry: t.entry}
	if l.applier != nil {
		if err := l.applier.Apply(c); err != nil {
			l.diverged = &divergedError{
				path:         l.Path(),
				prepareIndex: t.prepareIndex,
				commitIndex:  rec.Index,
				cause:        err,
			}
			l.logger.Error("wal log diverged from its durable state",
				"path", l.Path(), "prepare_index", t.prepareIndex, "commit_index", rec.Index,
				"kind", t.entry.Kind, "err", err)
			return Committed{}, l.diverged
		}
	}
	return c, nil
}

// Abort resolves the transaction by writing a durable ABORT record, so that a
// recovery pass knows the prepared entry will never commit and need not wait
// for a commit that is not coming. The prepared entry is never applied.
//
// The abort record is fsynced like every other append: it is a fact about
// accepted history, not a hint.
func (t *Txn) Abort(reason string) error {
	if !atomic.CompareAndSwapInt32(&t.done, 0, 1) {
		return fmt.Errorf("wal: abort prepare %d in %s: %w", t.prepareIndex, t.l.Path(), ErrTxnDone)
	}
	l := t.l
	defer l.mu.Unlock()

	payload, err := encodeAbort(t.prepareIndex, reason)
	if err != nil {
		return fmt.Errorf("wal: abort prepare %d in %s: encode payload: %w", t.prepareIndex, l.Path(), err)
	}
	if _, err := l.w.Append(TypeAbort, payload); err != nil {
		return fmt.Errorf("wal: abort prepare %d in %s: %w", t.prepareIndex, l.Path(), err)
	}
	l.logger.Debug("wal transaction aborted",
		"path", l.Path(), "prepare_index", t.prepareIndex, "reason", reason)
	return nil
}

// divergedError reports the exact commit at which memory and disk parted ways.
type divergedError struct {
	path         string
	prepareIndex uint64
	commitIndex  uint64
	cause        error
}

func (e *divergedError) Error() string {
	return fmt.Sprintf("wal: %s: commit record %d (prepare %d) is durable but the applier rejected it, so memory no longer matches disk; the log accepts no further writes and the process must restart and replay: %v",
		e.path, e.commitIndex, e.prepareIndex, e.cause)
}

// Is reports a match for ErrDiverged; Unwrap still reaches the applier's error.
func (e *divergedError) Is(target error) bool { return target == ErrDiverged }

func (e *divergedError) Unwrap() error { return e.cause }

// ---------------------------------------------------------------------------
// Payload codecs.
//
// The frame is binary (see format.go); the PAYLOAD is JSON, so a later epic can
// add a field without a format-version bump and so an operator can read a
// record with `head -c` and a pretty-printer.
//
//	PREPARE  {"kind":"<Entry.Kind>","ts":"<RFC3339Nano>","body":<Entry.Body>}
//	COMMIT   {"prepare_index":<uint64>}
//	ABORT    {"prepare_index":<uint64>,"reason":"<string>"}
//
// The decoders are STRICT in both directions: an unknown field, trailing
// garbage, a wrong record type, a zero prepare_index or a forward reference is
// a CorruptError, never a silently ignored record. A payload that will not
// decode means the file no longer says what history was accepted, and guessing
// is how acknowledged writes get lost.
// ---------------------------------------------------------------------------

type preparePayload struct {
	Kind string          `json:"kind"`
	TS   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type commitPayload struct {
	PrepareIndex uint64 `json:"prepare_index"`
}

type abortPayload struct {
	PrepareIndex uint64 `json:"prepare_index"`
	Reason       string `json:"reason"`
}

// canonicalBody validates and compacts an entry body. An empty body, and a body
// that is literally null, both canonicalise to nil so that a live Apply and a
// replayed Apply see identical bytes.
func canonicalBody(body json.RawMessage) (json.RawMessage, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	// json.Compact both validates and canonicalises, and does not HTML-escape.
	if err := json.Compact(&buf, body); err != nil {
		return nil, err
	}
	if buf.String() == "null" {
		return nil, nil
	}
	return json.RawMessage(buf.Bytes()), nil
}

func encodePrepare(kind string, body json.RawMessage, ts time.Time) ([]byte, error) {
	if body == nil {
		body = json.RawMessage("null")
	}
	return encodeJSON(preparePayload{Kind: kind, TS: ts.UTC().Format(time.RFC3339Nano), Body: body})
}

func encodeCommit(prepareIndex uint64) ([]byte, error) {
	return encodeJSON(commitPayload{PrepareIndex: prepareIndex})
}

func encodeAbort(prepareIndex uint64, reason string) ([]byte, error) {
	return encodeJSON(abortPayload{PrepareIndex: prepareIndex, Reason: reason})
}

// encodeJSON renders a payload without HTML escaping, so what an operator reads
// on disk is what the application handed over.
func encodeJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode terminates with a newline; the frame is length-prefixed
	// and needs no terminator.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// DecodePrepare decodes a PREPARE record into its entry and timestamp. It is
// the reader half of the write path and the decoder DUR-3's replay will use.
//
// The returned Entry never carries an Audit request: the audit log is a
// separate file (DUR-5) and a replay reads it from there, not from here.
func DecodePrepare(path string, rec Record) (Entry, time.Time, error) {
	var p preparePayload
	if err := decodePayload(path, rec, TypePrepare, &p); err != nil {
		return Entry{}, time.Time{}, err
	}
	if p.Kind == "" {
		return Entry{}, time.Time{}, frameCorruptf(path, rec,
			"record %d: prepare payload has an empty kind", rec.Index)
	}
	ts, err := time.Parse(time.RFC3339Nano, p.TS)
	if err != nil {
		e := frameCorruptf(path, rec, "record %d: prepare payload timestamp %q is not RFC3339Nano",
			rec.Index, elide(p.TS, maxValueChars))
		e.Err = err
		return Entry{}, time.Time{}, e
	}
	body := p.Body
	if len(body) == 0 || string(body) == "null" {
		body = nil
	}
	return Entry{Kind: p.Kind, Body: body}, ts, nil
}

// DecodeCommit decodes a COMMIT record and returns the index of the PREPARE it
// commits.
func DecodeCommit(path string, rec Record) (uint64, error) {
	var p commitPayload
	if err := decodePayload(path, rec, TypeCommit, &p); err != nil {
		return 0, err
	}
	if err := checkPrepareRef(path, rec, p.PrepareIndex); err != nil {
		return 0, err
	}
	return p.PrepareIndex, nil
}

// DecodeAbort decodes an ABORT record and returns the index of the PREPARE it
// abandons, plus the recorded reason.
func DecodeAbort(path string, rec Record) (uint64, string, error) {
	var p abortPayload
	if err := decodePayload(path, rec, TypeAbort, &p); err != nil {
		return 0, "", err
	}
	if err := checkPrepareRef(path, rec, p.PrepareIndex); err != nil {
		return 0, "", err
	}
	return p.PrepareIndex, p.Reason, nil
}

// checkPrepareRef rejects a prepare_index that cannot name a real earlier
// record. Index 0 never exists (indices start at 1) and a reference at or after
// the referring record is a forward reference, which the write path cannot
// produce.
func checkPrepareRef(path string, rec Record, prepareIndex uint64) error {
	if prepareIndex == 0 {
		return frameCorruptf(path, rec,
			"record %d: %s payload has prepare_index 0, but indices start at 1", rec.Index, rec.Type)
	}
	if prepareIndex >= rec.Index {
		return frameCorruptf(path, rec,
			"record %d: %s payload references prepare index %d, which is not earlier in the file",
			rec.Index, rec.Type, prepareIndex)
	}
	return nil
}

// decodePayload strictly decodes a record payload of an expected type into v.
func decodePayload(path string, rec Record, want Type, v interface{}) error {
	if rec.Type != want {
		return frameCorruptf(path, rec, "record %d is a %s record, want %s", rec.Index, rec.Type, want)
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// The decoder's own message quotes file-derived text (an unknown field
		// NAME, for instance). It is carried as the cause, which CorruptError
		// renders through a length bound; the payload itself is never included.
		e := frameCorruptf(path, rec, "record %d: %s payload does not decode", rec.Index, want)
		e.Err = err
		return e
	}
	if dec.More() {
		return frameCorruptf(path, rec, "record %d: %s payload has trailing data after the JSON object",
			rec.Index, want)
	}
	return nil
}
