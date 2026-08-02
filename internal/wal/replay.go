package wal

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// Replay reconstructs accepted history from a write-ahead log.
//
// It walks the file ONCE from the beginning, pairs every COMMIT record with the
// PREPARE record it names, and hands the surviving entries to fn in the order
// their COMMIT records appear on disk. That is the whole of recovery's job at
// this layer: it produces the committed-only entry stream and the high-water
// index. Folding that stream into roster/store state is the caller's business
// (fn), so this package still knows nothing about what an entry MEANS.
//
// # What survives, and what does not
//
//   - PREPARE followed by COMMIT -- delivered to fn. It is accepted history.
//   - PREPARE with no COMMIT -- DISCARDED, and its index is reported in
//     Recovered.Dangling. This is the normal shape of a crash between the two
//     fsyncs: the prepare is durable but nothing was ever acknowledged, so
//     nothing may become visible (invariants 4 and 5).
//   - PREPARE followed by ABORT -- DISCARDED. The abort record is the durable
//     statement that the commit is never coming.
//
// A COMMIT is matched to its PREPARE BY INDEX, never by adjacency: nothing in
// the format requires a commit to be the record right after its prepare, and a
// replay that assumed so would mis-pair the moment the write path ever
// interleaves two transactions.
//
// # Ordering: commit order, not prepare order
//
// Entries are delivered in COMMIT-record order. A write becomes accepted
// history at the instant its COMMIT record is fsynced, which is also the
// instant Txn.Commit applies it to memory -- so replaying in commit order makes
// the sequence of Apply calls after a restart identical to the sequence the
// live process made before it. Prepare order would be a different, and
// sometimes wrong, story about what happened.
//
// # Errors
//
// Replay is STRICT. A record it cannot interpret -- an undecodable payload, a
// COMMIT or ABORT naming something that is not an open prepare, a record type
// that has no meaning in a WAL -- stops the replay and is reported as a
// CorruptError (errors.Is(err, ErrCorrupt)). It is never skipped: a skipped
// record is a silent guess about what history was accepted, and the safe answer
// to "I do not understand this record" is to refuse to start, not to serve a
// state that might be missing an acknowledged write.
//
// Every CorruptError Replay itself mints carries FrameIntact -- by the time a
// record reaches this layer its checksum has already verified, so a partial
// write cannot explain the damage and the record must never be treated as a
// truncatable tail. See Recovered.EndOffset.
//
// On ANY error the returned Recovered is DIAGNOSTIC ONLY -- fn may already have
// received entries from the good prefix, and the caller must discard whatever
// it built rather than serve from it. Recovered.EndOffset marks the end of the
// last record Replay accepted; read its doc before treating that as a place to
// truncate, because most replay failures are NOT torn tails and truncating at
// one would destroy committed records.
//
// A torn tail from a crash mid-write is likewise an error here, NOT a tolerated
// condition: this function reports precisely where the file stops making sense,
// and the policy question of whether that tail may be truncated belongs to
// RepairTail, which runs BEFORE this replay (see Open) and is the only thing in
// this package that ever shortens a file. Note the common case
// is not a torn tail at all -- Append fsyncs whole frames, so the usual crash
// artefact is a complete, uncommitted PREPARE record, which Replay handles by
// discarding it.
//
// A file that does not exist, and a zero-length file, are both reported as an
// empty log rather than as corruption: neither can contain a record, so
// nothing that was ever acknowledged is lost by treating them as fresh. (The
// zero-length case is the crash window between creating the file and writing
// its header; OpenWriter heals it the same way, and the two must agree.)
//
// fn may be nil, in which case Replay validates the log and computes the
// high-water mark without applying anything -- a cheap fsck.
//
// Memory is O(unresolved prepares), not O(file): an entry is held only between
// its PREPARE and its COMMIT or ABORT. Log serialises transactions, so a
// well-formed file has at most one unresolved prepare at a time.
func Replay(path string, fn func(Committed) error) (Recovered, error) {
	// An empty log: next index 1, and no offset, because there is not even a
	// file header to be positioned after.
	empty := Recovered{Path: path, NextIndex: 1}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil
		}
		return empty, fmt.Errorf("wal: open %s: %w", path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return empty, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	if fi.Size() == 0 {
		return empty, nil
	}

	r := Recovered{Path: path, NextIndex: 1}

	// open holds every PREPARE that has not yet been committed or aborted,
	// keyed by its index -- which is also its transaction id (see Log).
	// openBytes tracks what those entries are retaining, so the bound below is
	// on MEMORY and not merely on a count.
	open := make(map[uint64]Entry)
	openBytes := int64(0)

	end, err := scanFrom(f, path, KindWAL, func(rec Record) error {
		r.Records++
		// The high-water mark counts every index EVER WRITTEN, including one
		// burned by a prepare that is about to be discarded. An index is never
		// reused: reissuing one would let two different messages share an id.
		r.NextIndex = rec.Index + 1

		switch rec.Type {
		case TypePrepare:
			// Decoded eagerly, even though the entry may never commit: a
			// prepare payload that does not decode means the file no longer
			// says what it recorded, and that is worth failing on where it is
			// found rather than at some later restart.
			e, _, err := DecodePrepare(path, rec)
			if err != nil {
				return err
			}
			if _, dup := open[rec.Index]; dup {
				// Unreachable while indices are unique, which scanFrom
				// enforces; checked anyway so a future change to the sequence
				// rule cannot silently drop an entry.
				return frameCorruptf(path, rec, "record %d: a prepare with this index is already open", rec.Index)
			}
			// The open set is bounded because it is built from a file that
			// recovery has no reason to trust yet. Log serialises transactions,
			// so a file this code wrote never holds more than one unresolved
			// prepare; a file holding thousands is either damaged or was not
			// written by this server, and either way the answer is to fail the
			// start rather than to let recovery allocate until the kernel kills
			// it -- a boot-time OOM would survive every restart, which is the
			// worst failure mode available. The bounds are generous and may be
			// raised (with a test) if the write path ever batches prepares.
			openBytes += int64(len(e.Kind)) + int64(len(e.Body))
			if len(open) >= maxOpenPrepares || openBytes > maxOpenPrepareBytes {
				// Both figures are reported, and both limits with them, so the
				// message says which bound was hit instead of implying the
				// count one always was.
				return frameCorruptf(path, rec,
					"record %d: too many unresolved prepares: %d open, %d bytes retained, limits are %d prepares and %d bytes; the write path resolves one transaction at a time, so this file was not written by this server",
					rec.Index, len(open)+1, openBytes, maxOpenPrepares, maxOpenPrepareBytes)
			}
			open[rec.Index] = e
			return nil

		case TypeCommit:
			prepareIndex, err := DecodeCommit(path, rec)
			if err != nil {
				return err
			}
			e, ok := open[prepareIndex]
			if !ok {
				return danglingRefError(path, rec, prepareIndex)
			}
			delete(open, prepareIndex)
			openBytes -= int64(len(e.Kind)) + int64(len(e.Body))
			r.Applied++
			if fn == nil {
				return nil
			}
			c := Committed{PrepareIndex: prepareIndex, CommitIndex: rec.Index, Entry: e}
			if err := fn(c); err != nil {
				// Wrapped, not returned bare, so the failure is attributable to
				// a record -- and deliberately NOT a CorruptError: the log is
				// fine, the caller rejected an entry the log says was accepted.
				return fmt.Errorf("wal: replay %s: applying committed entry (prepare %d, commit %d, kind %q): %w",
					path, prepareIndex, rec.Index, elide(e.Kind, maxValueChars), err)
			}
			return nil

		case TypeAbort:
			prepareIndex, _, err := DecodeAbort(path, rec)
			if err != nil {
				return err
			}
			e, ok := open[prepareIndex]
			if !ok {
				return danglingRefError(path, rec, prepareIndex)
			}
			delete(open, prepareIndex)
			openBytes -= int64(len(e.Kind)) + int64(len(e.Body))
			r.Aborted++
			return nil

		default:
			// scanFrom accepts an unknown type on purpose (its checksum proves
			// some writer meant those exact bytes; see Type.Known). Replay is
			// where that forward compatibility ends: a record whose effect on
			// accepted history is unknown cannot be ignored, because ignoring
			// it is indistinguishable from losing whatever it recorded.
			// TypeAuditMessage lands here too -- audit records belong to the
			// audit file, and one in a WAL means these are not the bytes we
			// think they are.
			return frameCorruptf(path, rec,
				"record %d: %s records have no meaning in a write-ahead log, and replay will not guess whether one affects accepted history",
				rec.Index, rec.Type)
		}
	})

	r.EndOffset = end
	// Reported sorted so that two replays of the same bytes produce byte-equal
	// output: recovery has to be deterministic to be testable.
	for prepareIndex := range open {
		r.Dangling = append(r.Dangling, prepareIndex)
	}
	sort.Slice(r.Dangling, func(i, j int) bool { return r.Dangling[i] < r.Dangling[j] })

	if err != nil {
		return r, err
	}
	return r, nil
}

// danglingRefError reports a COMMIT or ABORT whose prepare_index does not name
// an open prepare.
//
// DecodeCommit and DecodeAbort have already rejected index 0 and forward
// references, and scanFrom has already proven the index sequence has no holes,
// so the referenced record certainly EXISTS in this file. What is wrong is one
// of: it is not a PREPARE record at all, or it is a prepare that some earlier
// record already committed or aborted. Distinguishing those would cost a table
// of every index in the file, and the answer is the same either way -- the log
// does not describe a history this code can reconstruct -- so the error names
// all three possibilities rather than paying O(file) memory to pick one.
func danglingRefError(path string, rec Record, prepareIndex uint64) *CorruptError {
	return frameCorruptf(path, rec,
		"record %d: %s references prepare index %d, which is not an open prepare (it is not a prepare record, or it was already committed or aborted)",
		rec.Index, rec.Type, prepareIndex)
}

// Bounds on the unresolved-prepare set Replay retains while it walks a file.
// See the check in Replay for why recovery refuses to grow past them.
const (
	// maxOpenPrepares caps how many prepares may be open at once. The write
	// path holds one transaction at a time, so one is the real maximum; the
	// slack is for a future group-commit design, which would raise this.
	maxOpenPrepares = 1024

	// maxOpenPrepareBytes caps what those entries may retain, since a count
	// alone bounds nothing when a single body may be MaxPayloadSize. 8 MiB is
	// eight times the largest single entry the format allows and eight times
	// what the current write path can ever have open, which is enough slack to
	// be sure it only ever fires on a file this server did not write.
	maxOpenPrepareBytes = 8 << 20
)

// Recovered is the outcome of a Replay: what the durable log says happened, and
// where the next append goes.
type Recovered struct {
	// Path is the file that was replayed.
	Path string

	// NextIndex is the high-water mark: the index the next append will use. It
	// is strictly greater than EVERY index in the file, including indices
	// burned by prepares that were discarded, so a discarded transaction can
	// never have its index handed out again. An empty log reports 1.
	NextIndex uint64

	// EndOffset is the byte offset just past the last record Replay ACCEPTED.
	// After a successful replay that is the end of the file and the offset the
	// next append writes at. An empty log reports 0, since it has no header to
	// be positioned after.
	//
	// After a FAILURE it is only where replay stopped, and it is NOT on its own
	// a licence to truncate. Most of the ways a replay fails are damage in a
	// frame whose checksum verified -- a payload that will not decode, a commit
	// naming no open prepare, a record type with no meaning here -- and those
	// can sit anywhere in the file, with committed records after them. Cutting
	// at EndOffset there would delete accepted history to tidy up a file that
	// is fully readable.
	//
	// A corrupt-tail truncation qualifies the error first, and RepairTail is
	// where that happens: only a *CorruptError with FrameIntact false, whose
	// declared FrameEnd reaches or passes the end of the file (or is 0, meaning
	// the frame header itself was short), can be a torn tail. Anything else is
	// fatal where it sits.
	EndOffset int64

	// Records is the number of records read, of every type.
	Records uint64

	// Applied is the number of entries that reached commit, and so were
	// delivered to fn -- or would have been, when fn is nil and the replay is
	// only an fsck. It counts recovered history, not callbacks made, so the two
	// modes report the same numbers for the same bytes.
	Applied uint64

	// Aborted is the number of prepares resolved by an explicit ABORT record.
	Aborted uint64

	// Dangling holds, in ascending order, the indices of prepares that reached
	// neither commit nor abort. They were DISCARDED: nothing about them is
	// visible after recovery. A non-empty Dangling is the ordinary signature of
	// a crash between the prepare fsync and the commit fsync, and is not an
	// error -- but it is worth an operator seeing, because it is also the
	// signature of a write that a client may have been waiting on.
	Dangling []uint64

	// Repaired describes a corrupt tail that was truncated by the recovery pass
	// BEFORE this replay ran. It is zero (Truncated false) when nothing was
	// repaired. Replay itself never truncates anything and never sets this;
	// Open fills it in from RepairTail.
	Repaired TailRepair
}
