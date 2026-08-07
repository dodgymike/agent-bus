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
// # What it does with a record it cannot interpret: DISCARD AND CONTINUE
//
// Until 2026-08-02 this function was STRICT: an undecodable payload, a COMMIT
// naming no open prepare, or a record type with no meaning in a WAL stopped the
// replay and refused the start. The user reversed that policy (DECISIONS.md,
// "Availability over retention"): "always be able to restart, prefer to discard
// messages and/or corruption, with logging".
//
// So each of those is now DISCARDED and RECORDED in Recovered.Discarded, and
// the replay continues. Nothing is discarded silently: Open logs the discards in
// Recovered.Discarded, with offset, record index and type, at ERROR when what
// was lost had been acknowledged (a commit record) and WARN otherwise. A discard
// that does not reach the log is a bug, and there are tests that fail when one
// does not.
//
// Two CAPS apply to the detail and to neither of the totals, which is the
// precise claim: Recovered.Discarded retains at most maxDiscardsRetained
// entries and Open names at most maxDiscardsLogged of them individually, so a
// file that is damage from end to end cannot be held in memory as error text or
// turn one restart into a hundred thousand log lines. Recovered.DiscardCount and
// the emitted total are EXACT regardless. So "how much was lost" is never
// capped; only "which ones are described one by one" is.
//
// The honest consequence, stated rather than buried: a COMMIT record whose
// prepare was discarded is an ACKNOWLEDGED WRITE THAT IS NOW LOST. It is
// reported at ERROR with the record index. The alternative -- the previous
// behaviour -- was a bus that would not start at all, and the user chose the
// restart.
//
// # Gaps in the index sequence
//
// A repaired log has HOLES: recovery discards damaged records and deliberately
// does not renumber the survivors, because renumbering would reuse ids
// (invariant 1). Replay counts every hole into Recovered.MissingRecords and
// records it in Discarded, on EVERY start rather than only the one that made
// it, so a record lost to a bad sector cannot become a clean, quiet startup.
//
// # Errors
//
// What remains an error here is FRAMING damage -- a file header or a frame that
// does not parse -- reported as a CorruptError (errors.Is(err, ErrCorrupt)).
// Through Open that is unreachable: RepairLog runs first and hands Replay a
// file it has verified scans end to end. It is still reported when Replay is
// called directly on a damaged file, which is what makes Replay usable as a
// read-only fsck.
//
// On such an error the returned Recovered is DIAGNOSTIC ONLY -- fn may already
// have received entries from the good prefix, and the caller must discard
// whatever it built rather than serve from it.
//
// Every CorruptError Replay itself mints carries FrameIntact -- by the time a
// record reaches this layer its checksum has already verified, so a partial
// write cannot explain the damage. See Recovered.EndOffset.
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

	// The codec is resolved AFTER the empty cases inside replay, so replaying a
	// log that is not there reports an empty log rather than anything about a
	// key -- and does not create one as a side effect of a read.
	if empty, err := logIsEmpty(path); err != nil || empty {
		return Recovered{Path: path, NextIndex: 1}, err
	}
	c, err := resolveCodec(path, KindWAL, nil)
	if err != nil {
		return empty, err
	}
	return replay(path, c, fn)
}

// replay is Replay with the codec already resolved, so that Open works out the
// format and loads the MAC key exactly once for the whole of recovery.
func replay(path string, c codec, fn func(Committed) error) (Recovered, error) {
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
	//
	// There is deliberately NO side list of indices in file order. An earlier
	// version kept one to find the oldest prepare cheaply, and security measured
	// what that cost: the list was appended to for EVERY prepare in the file and
	// compacted only from inside the eviction path, which never runs on a
	// healthy log -- so a 23.7 MB WAL with zero unresolved prepares retained
	// 1.76 MB of index list, growing linearly with the FILE. That is the
	// O(unresolved prepares) bound this function documents, broken, and at
	// 10 GiB it is the boot-time OOM the eviction was written to avoid.
	// Eviction only ever runs on a file this server did not write, so it can
	// afford to scan the (at most maxOpenPrepares) live entries instead.
	open := make(map[uint64]Entry)
	openBytes := int64(0)

	// expectIndex is 0 until the FIRST record is seen, meaning "no expectation
	// yet". It deliberately does NOT start at 1.
	//
	// A WAL no longer necessarily begins at index 1: a fresh log started after a
	// quarantine begins ABOVE the durable index floor, and recovery may advance
	// the index past a hole rather than reissue it. Starting the expectation at 1
	// made a log whose first record is index 757 report "records 1..756 are
	// missing" on EVERY start, for ever -- records that were never in this file
	// at all. Claiming them as missing turns the loss channel into noise, and a
	// loss channel that cries wolf is the mirror image of the silent-discard
	// defect invariant 6 names as the actual P0. So only INTERIOR holes are
	// reported; where the file STARTS is reported separately, as FirstIndex.
	expectIndex := uint64(0)

	end, err := scanFrom(c, f, path, KindWAL, func(rec Record) error {
		if r.FirstIndex == 0 {
			r.FirstIndex = rec.Index
		}
		// A hole in the index sequence is a record that is not in the file:
		// discarded by an earlier recovery, lost from the media, or an index
		// recovery deliberately skipped so that nothing was reissued. It is not
		// an error -- a repaired log has holes by design, because survivors are
		// never renumbered -- but it IS a loss (or at worst a burned number), and
		// it is reported on every start for as long as it exists. Silence here is
		// what let a lost sector look like a clean boot.
		//
		// MissingRecords therefore counts SKIPPED-BUT-NEVER-USED indices too, and
		// is an UPPER BOUND on loss rather than an exact count. That is the right
		// direction to be wrong in: over-reporting a hole invites an operator to
		// look, under-reporting one hides a lost sector.
		if expectIndex != 0 && rec.Index > expectIndex {
			r.MissingRecords += rec.Index - expectIndex
			r.addDiscard(Discard{Stage: "replay", Offset: rec.Offset, Length: 0,
				Index: expectIndex, TypeKnown: false,
				Reason: fmt.Sprintf("records %d..%d are missing from the index sequence: lost from the file, discarded by an earlier recovery which -- correctly -- did not renumber the survivors, or skipped by recovery advancing the index past a hole so that no index is reissued (see the durable index floor, <data-dir>/%s)",
					expectIndex, rec.Index-1, IndexFloorFileName)})
		}
		expectIndex = rec.Index + 1

		r.Records++
		// The high-water mark counts every index EVER SEEN, including one burned
		// by a prepare that is about to be discarded. An index is never reused:
		// reissuing one would let two different messages share an id.
		r.NextIndex = rec.Index + 1

		switch rec.Type {
		case TypePrepare:
			// Decoded eagerly, even though the entry may never commit: a
			// prepare payload that does not decode means the file no longer
			// says what it recorded.
			e, _, err := DecodePrepare(path, rec)
			if err != nil {
				r.discardRecord(rec, "the prepare payload does not decode, so what this record reserved cannot be known: "+reasonOf(err))
				return nil
			}
			if _, dup := open[rec.Index]; dup {
				// Unreachable while indices rise, which scanFrom enforces;
				// handled anyway so a future change to the sequence rule cannot
				// silently drop an entry.
				r.discardRecord(rec, "a prepare with this index is already open")
				return nil
			}
			// The open set is bounded because it is built from a file recovery
			// has no reason to trust yet. Log serialises transactions, so a file
			// this code wrote never holds more than one unresolved prepare; a
			// file holding thousands is either damaged or was not written by
			// this server. Before 2026-08-02 hitting the bound refused the
			// start; a boot-time OOM would have survived every restart, so
			// failing was better than allocating. Under the always-restart
			// policy neither is acceptable, so the OLDEST unresolved prepares
			// are EVICTED instead. That loses nothing that was acknowledged --
			// an unresolved prepare never committed -- and it keeps the memory
			// bound exactly as tight as it was.
			openBytes += int64(len(e.Kind)) + int64(len(e.Body))
			open[rec.Index] = e
			for len(open) > maxOpenPrepares || openBytes > maxOpenPrepareBytes {
				// The record just read is never the victim: evicting it would
				// leave the loop unable to make progress when one entry alone
				// exceeds the byte bound. len(open) > 1 guarantees a different
				// victim exists.
				if len(open) < 2 {
					break
				}
				victim := oldestOpen(open)
				ev := open[victim]
				delete(open, victim)
				openBytes -= int64(len(ev.Kind)) + int64(len(ev.Body))
				r.addDiscard(Discard{Stage: "replay", Offset: -1, Length: 0,
					Index: victim, Type: TypePrepare, TypeKnown: true,
					Reason: fmt.Sprintf("evicted the oldest unresolved prepare to stay inside recovery's memory bounds (%d prepares, %d bytes): this file holds more open transactions than the write path can produce, so it was not written by this server",
						maxOpenPrepares, maxOpenPrepareBytes)})
			}
			return nil

		case TypeCommit:
			prepareIndex, err := DecodeCommit(path, rec)
			if err != nil {
				r.discardRecord(rec, "the commit payload does not decode, so the prepare it accepted cannot be identified: "+reasonOf(err))
				return nil
			}
			e, ok := open[prepareIndex]
			if !ok {
				// An ACKNOWLEDGED WRITE IS LOST HERE. The commit record is
				// durable, so a client was told this entry was accepted, but
				// the prepare carrying the entry is gone -- discarded as damage
				// earlier in this recovery, or already resolved.
				r.discardRecord(rec, danglingRefReason(rec, prepareIndex))
				return nil
			}
			delete(open, prepareIndex)
			openBytes -= int64(len(e.Kind)) + int64(len(e.Body))
			r.Applied++
			if fn == nil {
				return nil
			}
			c := Committed{PrepareIndex: prepareIndex, CommitIndex: rec.Index, Entry: e}
			if err := fn(c); err != nil {
				// The log is fine; the caller rejected an entry the log says was
				// accepted. Refusing the start here was the old policy; now the
				// entry is dropped from the rebuilt memory state and reported as
				// the acknowledged loss it is.
				r.Applied--
				r.discardRecord(rec, fmt.Sprintf("the applier rejected this committed entry (prepare %d, kind %q), so it is durable on disk but absent from the rebuilt memory state: %s",
					prepareIndex, elide(e.Kind, maxValueChars), reasonOf(err)))
				return nil
			}
			return nil

		case TypeAbort:
			prepareIndex, _, err := DecodeAbort(path, rec)
			if err != nil {
				r.discardRecord(rec, "the abort payload does not decode: "+reasonOf(err))
				return nil
			}
			e, ok := open[prepareIndex]
			if !ok {
				r.discardRecord(rec, danglingRefReason(rec, prepareIndex))
				return nil
			}
			delete(open, prepareIndex)
			openBytes -= int64(len(e.Kind)) + int64(len(e.Body))
			r.Aborted++
			return nil

		default:
			// scanFrom accepts an unknown type on purpose (its tag proves
			// some writer meant those exact bytes; see Type.Known). Replay
			// cannot know what such a record did to accepted history, so it
			// discards it and says so. TypeAuditMessage lands here too -- audit
			// records belong to the audit file, and one in a WAL means these are
			// not the bytes we think they are.
			r.discardRecord(rec, fmt.Sprintf("%s records have no meaning in a write-ahead log, and replay will not guess whether one affects accepted history", rec.Type))
			return nil
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

// danglingRefReason describes a COMMIT or ABORT whose prepare_index does not
// name an open prepare.
//
// DecodeCommit and DecodeAbort have already rejected index 0 and forward
// references. What is wrong is one of: the referenced record was DISCARDED as
// damage earlier in this same recovery, or it was lost from the file, or it is
// not a PREPARE record at all, or it is a prepare that some earlier record
// already resolved. Distinguishing those would cost a table of every index in
// the file, and the consequence is the same either way, so the reason names
// them rather than paying O(file) memory to pick one.
func danglingRefReason(rec Record, prepareIndex uint64) string {
	return fmt.Sprintf("%s references prepare index %d, which is not an open prepare (it was discarded as damage, lost from the file, is not a prepare record, or was already committed or aborted); if this is a commit, an acknowledged write is lost here",
		rec.Type, prepareIndex)
}

// reasonOf renders an underlying error for a Discard reason, bounded like every
// other file-derived text in this package (see elide).
//
// For a CorruptError it takes the REASON alone, not the rendered error: the
// rendered form re-embeds the path and offset, which the log line already
// carries as its own fields, and on a long data-directory path that prefix ate
// the whole of the length bound and elided the actual diagnosis away.
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	var ce *CorruptError
	if errors.As(err, &ce) {
		s := ce.Reason
		if ce.Err != nil {
			s += ": " + ce.Err.Error()
		}
		return elide(s, maxCauseChars)
	}
	return elide(err.Error(), maxCauseChars)
}

// oldestOpen returns the earliest prepare still unresolved: indices rise
// through the file, so the smallest live key is the oldest record.
//
// It is O(open), not O(file), and open is capped at maxOpenPrepares. That is
// the whole point -- see the note in Replay about the index list this replaced.
// It runs only from the eviction path, which only fires on a file this server
// did not write. The caller must not call it on an empty map.
func oldestOpen(open map[uint64]Entry) uint64 {
	oldest := uint64(0)
	for idx := range open {
		if oldest == 0 || idx < oldest {
			oldest = idx
		}
	}
	return oldest
}

// addDiscard records one loss, keeping the counts exact while capping the
// detail list -- a file that is damage from end to end must not be able to make
// recovery hold it all in memory as error text.
func (r *Recovered) addDiscard(d Discard) {
	r.DiscardCount++
	if len(r.Discarded) < maxDiscardsRetained {
		r.Discarded = append(r.Discarded, d)
	}
}

// discardRecord records a whole record thrown away at the replay stage.
func (r *Recovered) discardRecord(rec Record, reason string) {
	r.addDiscard(Discard{
		Stage:     "replay",
		Offset:    rec.Offset,
		Length:    rec.frameSize(),
		Index:     rec.Index,
		Type:      rec.Type,
		TypeKnown: true,
		Reason:    reason,
	})
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

	// NextIndex is the high-water mark: strictly greater than EVERY index in the
	// file, including indices burned by prepares that were discarded, so a
	// discarded transaction can never have its index handed out again. An empty
	// log reports 1.
	//
	// FROM Replay it is the FILE's high-water mark. FROM Log.Recovered it is the
	// index the next append will ACTUALLY use: Open raises it to the maximum of
	// this, what the repair pass observed, and the durable index floor in
	// <data-dir>/wal-index-floor. That distinction matters because internal/hub
	// derives the message-sequence floor from this field, so it must report the
	// number that will be used, not the number the file's arithmetic suggests.
	NextIndex uint64

	// FirstIndex is the index of the FIRST record this replay saw, or 0 for an
	// empty log.
	//
	// A WAL does not necessarily begin at index 1 any more. A fresh log started
	// after a quarantine begins above the durable index floor, so an operator
	// looking at a log whose first record is index 758 needs to be able to see
	// that this is where the file starts rather than where the loss ends --
	// which is also why replay does not report the indices below it as missing.
	FirstIndex uint64

	// EndOffset is the byte offset just past the last record Replay ACCEPTED.
	// After a successful replay that is the end of the file and the offset the
	// next append writes at. An empty log reports 0, since it has no header to
	// be positioned after.
	//
	// After a FAILURE it is only where replay stopped, and it is NOT on its own
	// a licence to truncate: damage can sit anywhere in the file with committed
	// records after it, and cutting there would delete accepted history to tidy
	// up a file that is mostly readable. Deciding what to remove is RepairLog's
	// job, and RepairLog searches forward for the next intact record rather than
	// treating the first damage as the end of the log.
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

	// Discarded is what this replay THREW AWAY: records whose frames were
	// intact but whose content could not be turned into history, prepares
	// evicted to stay inside recovery's memory bounds, and holes in the index
	// sequence. It is capped at maxDiscardsRetained entries; DiscardCount is
	// exact.
	//
	// Replay does not log these itself -- it has no logger, which keeps it
	// usable as a pure fsck -- but Open DOES, one line each, and that logging is
	// part of the contract rather than a nicety. Discarding is sanctioned
	// behaviour now; discarding without a log record is the bug.
	Discarded    []Discard
	DiscardCount int

	// MissingRecords is how many record indices are absent from the file: the
	// size of the holes a previous repair (or a bad sector) left behind. It is
	// reported on every start, not only the one that made the hole.
	MissingRecords uint64

	// Repaired describes what the recovery pass removed or rebuilt BEFORE this
	// replay ran. It is zero when nothing was repaired. Replay itself never
	// changes a file and never sets this; Open fills it in from RepairLog.
	Repaired Repair
}
