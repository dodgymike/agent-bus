package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	w *Writer
	// audit is the append-only message audit log (DUR-5, invariant 6): a
	// SEPARATE file in the same data directory, under the same MAC key, holding
	// metadata and routing info only and never a message body. It is non-nil for
	// every Log built by Open. See audit.go.
	audit   *Writer
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
	// Idem, when non-nil, is the APPLIED-KEY RECORD for the operation this
	// entry effects (IDEM-11, invariant 10). It is opaque JSON: wal does not
	// interpret it, exactly as it does not interpret Kind or Body.
	//
	// IT RIDES IN THE SAME PREPARE PAYLOAD AS THE EFFECT, and that is the whole
	// point rather than an implementation detail. A transaction carries exactly
	// one Entry, so an applied-key record written here commits when -- and only
	// when -- the effect commits, in one fsync. A second, separately ordered
	// write would leave a window where the effect is durable and the key is
	// not; a crash there plus a client retry produces exactly the duplicate
	// invariant 10 exists to prevent, and the window is small enough to be
	// invisible in ordinary testing.
	//
	// It is canonicalised (compacted, "null" normalised to nil) exactly like
	// Body, so a live Apply and a replayed Apply see byte-identical bytes.
	//
	// # FORWARD-COMPATIBILITY HAZARD (downgrade is not supported, and this is
	// # why)
	//
	// decodePayload uses DisallowUnknownFields. A binary built BEFORE this
	// field existed, reading a log written AFTER it, treats EVERY prepare
	// carrying an idem record as an undecodable payload -- which recovery
	// DISCARDS. That is an acknowledged write lost on downgrade, not a
	// degraded-but-correct read. Downgrade is not a supported operation here
	// (one binary, one container, forward-only), so this is not a defect to be
	// fixed by loosening the decoder -- a lenient decoder is how a file that no
	// longer says what history was accepted gets served as if it did. It is
	// written down so it is known rather than discovered.
	Idem json.RawMessage
	// Audit, when non-nil, requests a record in the append-only MESSAGE AUDIT
	// LOG -- a second file, distinct from this WAL (DUR-5, invariant 6). The
	// audit record is metadata and routing info ONLY -- never the message body
	// (corrected 2026-08-02: the bus is getting E2E encryption with forward
	// secrecy, so the audit trail is a provenance record, not a content
	// archive). See audit.go for the record, the exclusion and the ordering.
	//
	// A nil Audit means this entry gets no audit record, which is the right
	// answer for every entry that is not a message: roster, invite and session
	// records share this WAL and are not part of the message trail.
	//
	// A NON-NIL Audit THAT DOES NOT VALIDATE FAILS THE WRITE, before anything is
	// appended to either file (see AuditRecord.validate and ErrInvalidAudit).
	// A message that cannot be audited is not accepted.
	Audit *AuditRecord
}

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
// THE RECORD INDEX IS NOT DERIVED FROM THE LOG ALONE. Open also reads the
// DURABLE INDEX FLOOR in <data-dir>/wal-index-floor (see indexfloor.go) and
// resumes above the maximum of the replayed high-water mark, what the repair
// pass observed, and that floor. This is what makes invariant 1 hold through
// recovery policy that is allowed to throw bytes away: a discarded tail record's
// index is jumped over rather than reissued, and a QUARANTINE -- which starts a
// completely fresh log, so the file's own answer is "index 1" -- still resumes
// above every index this data directory ever authorised. The floor lives outside
// the log precisely so that no repair, truncation or quarantine can lower it. A
// MISSING floor file is benign (a data directory that predates it) and logged; a
// CORRUPT one is fatal and never regenerated -- see ErrIndexFloorCorrupt, which
// also explains why that is not a violation of "recovery always restarts".
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

	// DOES THIS DATA DIRECTORY ALREADY HAVE A MAC KEY? Probed here, read-only,
	// before recovery can create one, and used for exactly one judgement further
	// down: whether a floor file that fails its keyed tag is DAMAGE (the key is
	// this directory's own, so the bytes are wrong) or a RE-FOUNDED DIRECTORY (the
	// key was lost and recovery minted a new one, under which nothing the old
	// identity wrote can possibly verify). Those two states call for opposite
	// responses and are indistinguishable after the fact, so the question is asked
	// while the answer still exists.
	//
	// A malformed or unreadable key is fatal here exactly as it is below; a
	// MISSING one is not diagnosed here at all, because macKeyFor owns that
	// judgement and duplicating it would let the two drift.
	hadKeyAtOpen := true
	if _, kerr := loadMACKey(macKeyPath(opts.Dir)); kerr != nil {
		if !errors.Is(kerr, ErrMACKeyMissing) {
			return nil, kerr
		}
		hadKeyAtOpen = false
	}

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
	// RECOVERY ALWAYS REACHES A RUNNING SERVER (DECISIONS.md, 2026-08-02,
	// "Availability over retention": "always be able to restart, prefer to
	// discard messages and/or corruption, with logging"). Damage is discarded
	// and LOGGED -- never silently, and never by deleting intact records that
	// happen to sit behind the damage. What still refuses the start is being
	// unable to READ the file at all: permission denied, an I/O error from the
	// device, an audit file where a WAL was expected, a format version this
	// binary does not implement, or a data directory another process is writing
	// to. None of those are damage.
	//
	// RepairLog runs FIRST, before a single entry is handed to the Applier. It
	// is a framing-only pass: it truncates damage at the end of the file,
	// rewrites the file to drop damage in the middle while keeping every intact
	// record behind it, rebuilds a damaged file header, and -- if the file
	// cannot be interpreted at all -- moves it aside so startup can continue
	// with a fresh log. It then PROVES its own result by re-scanning, so Replay
	// below always sees a file that parses end to end.
	//
	// THE ON-DISK FORMAT AND THE MAC KEY ARE RESOLVED ONCE, HERE, and threaded
	// through every pass, so recovery cannot change its mind about which format
	// it is reading half way and does not load the key four times.
	//
	// A FORMAT VERSION 1 LOG IS UPGRADED BEFORE ANYTHING ELSE HAPPENS TO IT, in
	// two steps that have to be in this order: it is repaired IN VERSION 1 (the
	// upgrade's strict scan cannot read a damaged log), then converted once to
	// version 2 (see upgradeV1). After that the file is version 2 and the rest of
	// recovery is the ordinary path. Nothing appends to a version 1 file --
	// OpenWriter refuses one outright.
	// ---------------------------------------------------------------------
	version, err := detectFormat(path, KindWAL)
	if err != nil {
		return nil, err // detectFormat already names the path
	}
	var legacyRepair Repair
	if version == formatVersionV1 {
		legacyRepair, err = repairLog(path, KindWAL, codec{version: formatVersionV1}, opts.Logger)
		if err != nil {
			return nil, err
		}
		to, err := currentCodec(path, KindWAL, opts.Logger)
		if err != nil {
			return nil, err
		}
		if err := upgradeV1(path, KindWAL, to, opts.Logger); err != nil {
			return nil, err
		}
	}

	c, err := resolveCodec(path, KindWAL, opts.Logger)
	if err != nil {
		return nil, err
	}

	repair, err := repairLog(path, KindWAL, c, opts.Logger)
	if err != nil {
		return nil, err // repairLog already names the path and the offset
	}
	// A legacy log that needed repairing was repaired BEFORE the upgrade, so the
	// pass above found the converted file clean. Report the repair that actually
	// happened rather than the no-op that followed it.
	// Its NextIndex is still right: the upgrade carries indices across byte for
	// byte and never renumbers.
	if legacyRepair.Truncated || legacyRepair.Rewritten || legacyRepair.Quarantined != "" || legacyRepair.DiscardCount > 0 {
		repair = legacyRepair
	}

	// ---------------------------------------------------------------------
	// THE DURABLE INDEX FLOOR (invariant 1: ids are never reused, INCLUDING
	// across restarts). It is read HERE -- after the MAC key has been settled,
	// before the start index is derived, and before a single byte is appended.
	//
	// # Why not earlier, which is where it used to be
	//
	// The floor is AUTHENTICATED WITH HMAC-SHA256 UNDER THIS DATA DIRECTORY'S
	// wal-mac.key (invariant 6: integrity is a keyed MAC, never an unkeyed
	// checksum). YOU CANNOT JUDGE A MAC'D FILE BEFORE YOU KNOW WHICH KEY IS THE
	// RIGHT ONE, and settling that is precisely what the passes above do:
	// macKeyFor decides whether an absent key may be generated or is fatal, and
	// repairLog raises ErrMACKeyMismatch for a key that is present but wrong.
	//
	// Reading the floor first put those two verdicts in the wrong order and
	// produced a DANGEROUS misdiagnosis: a merely WRONG key made the floor fail
	// its tag, so the operator was told the floor was corrupt and pointed at a
	// remedy (delete it) that FORFEITS INVARIANT 1 -- when the actual fix was to
	// restore the key and nothing was wrong with the floor at all. Two failures
	// with opposite remedies must not be collapsed into the more destructive one.
	//
	// THE MOVE NARROWS THAT MISDIAGNOSIS, IT DOES NOT ELIMINATE IT, and saying so
	// is the point of this paragraph. ErrMACKeyMismatch is raised only for a log
	// that identifies itself as version 2 AND is longer than its own file header.
	// A wrong key over a log with NO READABLE RECORD -- a bare 48-byte header,
	// which is what a fresh clean run and a POST-QUARANTINE directory both leave
	// -- still reaches the floor first and still reports a corrupt floor. Both
	// gates measured it. What closes the remaining gap is not ordering but the
	// error TEXT: indexFloorCorrupt names wal-mac.key as the first thing to check,
	// ahead of the remedy. See there.
	//
	// THE COST OF THE MOVE, stated rather than hidden: a corrupt floor is now
	// refused AFTER repairLog has run, and repairLog DESTROYS BYTES -- truncateAt
	// truncates permanently and the mid-file rewrite renames a temp over the
	// original; only a QUARANTINE preserves the file by renaming it aside. An
	// earlier draft of this comment claimed repairLog "never deletes bytes without
	// moving them aside", and a reviewer measured that false (839 bytes before a
	// refused Open, 789 after, nothing moved aside). The move is still right, for
	// a narrower reason: the bytes repairLog removes are damage it has already
	// LOGGED before touching the file, and they are bytes any successful start
	// would have discarded anyway -- whereas the misdiagnosis above cost an id
	// space. Note also that this still runs BEFORE replay, so a refusal here can
	// never leave the caller holding a partially rebuilt Applier.
	//
	// hadKeyAtOpen distinguishes the two ways a keyed tag can fail to verify. See
	// openIndexFloor.
	floor, err := openIndexFloor(opts.Dir, c.key, hadKeyAtOpen)
	if err != nil {
		return nil, err // openIndexFloor already names the path and the remedy
	}
	// AN UNAUTHENTICATED FLOOR IS ANNOUNCED, LOUDLY, EVERY TIME. Its `sealed` bit
	// has been DISCARDED as untrustworthy (readIndexFloorFile), so this start
	// takes the ceiling and burns at most one reservation block. That hole is
	// legal and correct, but an operator who sees it in the index sequence
	// deserves to know why, and these are the only lines that say so.
	switch {
	case floor.legacyUnauthenticated():
		opts.Logger.Warn("wal upgraded the durable record index floor to a keyed MAC",
			"path", floor.Path(), "key", macKeyPath(opts.Dir),
			"why", "the file carried an UNKEYED sha256 digest, which anyone able to write the data directory can recompute; a forged `sealed 1` would reissue WAL record indices, and therefore message ids, silently. Its sealed bit has been discarded as untrustworthy, so this start resumes above the durable ceiling and burns at most one reservation block; the file is rewritten with an HMAC-SHA256 tag below")
	case floor.unverified():
		opts.Logger.Error("wal could not verify the durable record index floor and read it WITHOUT authentication",
			"path", floor.Path(), "key", macKeyPath(opts.Dir),
			"why", "this data directory had no MAC key when it was opened, so recovery minted a new one and nothing the previous identity wrote can verify under it. The floor's numbers are still used, but ONLY TO RAISE the start index, and its `sealed` bit has been discarded. If this key was not meant to be lost, STOP THE BUS AND RESTORE IT: an unverified floor is no better protection than no floor at all")
	}

	var apply func(Committed) error
	if opts.Applier != nil {
		apply = opts.Applier.Apply
	}
	rec, err := replay(path, c, apply)
	if err != nil {
		return nil, err // replay already names the path and the offset
	}
	rec.Repaired = repair

	w, err := openWriter(path, KindWAL, c)
	if err != nil {
		return nil, err // openWriter already names the path
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

	// ---------------------------------------------------------------------
	// DERIVING THE START INDEX (invariant 1: ids are never reused, INCLUDING
	// across restarts -- reaffirmed WITHOUT narrowing on 2026-08-02).
	//
	// THE PROPERTY, in one sentence a reviewer can check: THE START INDEX IS
	// ALWAYS STRICTLY GREATER THAN EVERY INDEX THIS DATA DIRECTORY HAS EVER
	// WRITTEN TO A LOG, NO MATTER WHAT HAPPENED TO THE LOG -- truncated at any
	// offset, rewritten, quarantined, or deleted outright.
	//
	// The index the next append uses is the MAXIMUM of three sources, and each
	// one covers a hole the others leave:
	//
	//	rec.NextIndex      one past the highest index still IN the file
	//	repair.NextIndex   one past the highest index the repair pass OBSERVED,
	//	                   discarded records included
	//	floor.burned()+1   one past everything durably BURNED -- written, or
	//	                   permanently skipped by an earlier recovery
	//
	// plus, WHENEVER THE PREVIOUS RUN DID NOT CLOSE CLEANLY, floor.ceiling()+1:
	// one past everything this data directory has EVER AUTHORISED.
	//
	// # Why the trigger is "did the previous run close cleanly", not "did this
	// # recovery find damage"
	//
	// Until 2026-08-07 the ceiling was taken only when repair.LostUnidentified
	// was set -- only when recovery could PROVE the log had lost something. THAT
	// CAN NEVER BE SOUND, and a reviewer proved it with a probe rather than an
	// argument: a truncation at a CLEAN FRAME BOUNDARY is byte-indistinguishable
	// from a log that was simply shorter, so salvage sees no damage, nothing is
	// proved, and the indices past the cut are handed straight back out. 25 of
	// 2289 truncation offsets over a 12-message log reissued an index, and they
	// were exactly the frame boundaries. The degenerate case is worse still:
	// crash without a Close and then delete bus.wal, and recovery reports an
	// empty log with no damage at all and resumes at index 1, while `reserved`
	// sits on disk unread.
	//
	// "Did the previous run close cleanly" IS knowable -- see indexfloor.go's
	// `sealed` bit, which begin clears (fsynced) before a byte can be appended
	// and only Writer.Close's seal ever sets. So that is the trigger now.
	//
	// # The induction that actually holds
	//
	//  1. A RUN THAT CRASHES LEAVES sealed 0. The next start therefore takes
	//     ceiling+1, which is >= every index that run could possibly have
	//     authorised -- because nothing is ever stamped into a frame until the
	//     ceiling covering it has been fsynced (Writer.Append reserves first).
	//     The price is a hole of at most indexReserveBlock-1.
	//  2. A RUN THAT CLOSES CLEANLY LEAVES sealed 1 AND AN EXACT `written`. Then
	//     written+1 already dominates every index ever put in a frame, the
	//     ceiling adds nothing but a hole, and NO indices are burned. This is the
	//     ordinary restart, and it is why the seal bit buys back the property
	//     that a clean cycle leaves the index sequence dense.
	//  3. `begin` DURABLY RAISES `written` TO start-1 AT EVERY Open (below). So a
	//     run that jumped the index -- because a quarantine or an unidentifiable
	//     discard forced it to -- and then wrote nothing at all is not forgotten
	//     even though no record carries the jump.
	//
	// repair.LostUnidentified IS NO LONGER A CORRECTNESS TRIGGER. It is kept
	// because it is exported, documented, tested, consumed by cmd/agent-bus, and
	// still good DIAGNOSTICS -- "recovery threw away bytes it could not
	// enumerate" is worth an operator knowing. But it is not needed here, and
	// making the ceiling conditional on it is precisely the defect above: when
	// sealed 1, written+1 already dominates and the ceiling would only burn a
	// hole; when sealed 0, the ceiling is taken regardless of what the repair
	// pass did or did not manage to prove.
	// ---------------------------------------------------------------------
	fileNext := rec.NextIndex
	if repair.NextIndex > fileNext {
		fileNext = repair.NextIndex
	}
	start := fileNext
	quarantined := repair.Quarantined != ""
	if floor.burned() == math.MaxUint64 || floor.ceiling() == math.MaxUint64 {
		// Nothing can be appended without reusing an index, and reusing one is
		// the thing this may never do. Refusing is the only honest answer.
		wErr := w.Close()
		return nil, fmt.Errorf("wal: open log %s: the durable index floor %s has reached the end of the 64-bit record index space (burned %d, reserved %d), so no index can be issued without reusing one, which invariant 1 forbids (close: %v)",
			path, floor.Path(), floor.burned(), floor.ceiling(), wErr)
	}
	if b := floor.burned() + 1; b > start {
		start = b
	}
	if !floor.sealedClean() {
		if c := floor.ceiling() + 1; c > start {
			start = c
		}
	}

	w.advanceIndexTo(start)
	// Report the index the next append will ACTUALLY use. internal/hub derives
	// the message-sequence floor from this field (Recovered.NextIndex - 1), so
	// leaving it at the file's own arithmetic would reissue message ids even
	// though the WAL itself had jumped -- which is half of what defects e120153b
	// and db350e39 actually were.
	rec.NextIndex = start

	// Read BEFORE begin. This is BELT-AND-BRACES now rather than load-bearing:
	// existedAtOpen() answers "was the file there when this data directory was
	// OPENED", and persistLocked no longer flips it, so asking after begin would
	// give the same answer. It used to flip it, which made the migration warning
	// below dead code no test could ever see -- caught by a test written to
	// expect the warning rather than the silence. The snapshot stays so that this
	// site does not depend on that accessor staying honest.
	migrating := !floor.existedAtOpen()

	// Captured BEFORE begin raises it, for the head-loss check further down. Once
	// begin has run, floor.burned() is start-1 and the comparison would be
	// tautological.
	burnedBeforeThisRun := floor.burned()

	// The floor is written and FSYNCED BEFORE the Log is returned, and therefore
	// before anything can append. Until this succeeds, the jump above exists only
	// in memory and a crash would forget it.
	if err := floor.begin(start); err != nil {
		wErr := w.Close()
		return nil, fmt.Errorf("wal: open log %s: recording the start index %d in the durable index floor: %w (close: %v)", path, start, err, wErr)
	}
	w.setIndexFloor(floor)

	// SKIPPING INDEX SPACE SILENTLY IS THE SAME FAILURE AS DISCARDING SILENTLY,
	// applied to the id space instead of the message space -- and silent discard
	// is the defect invariant 6 actually names. So it is logged loudly and
	// specifically whenever the start is above what the file alone would have
	// given, at ERROR when a quarantine caused it and WARN otherwise.
	if start > fileNext {
		kv := []interface{}{
			"path", path, "from", fileNext, "to", start, "indices_skipped", start - fileNext,
			"index_floor", floor.Path(),
			"why", "an index this data directory has already authorised is never authorised again (invariant 1, reaffirmed without narrowing 2026-08-02); the skipped indices are permanently burned and will appear as a hole in the log's index sequence",
		}
		if quarantined {
			opts.Logger.Error("wal resumed the record index above the durable floor after a quarantine", kv...)
		} else {
			opts.Logger.Warn("wal resumed the record index above what the log file alone would have given", kv...)
		}
	}
	// THE MIGRATION WINDOW, stated honestly rather than left to be discovered: a
	// data directory that already holds WAL records but has no floor file was
	// written by a binary that predates it. Until the file exists -- which it
	// will, from the begin above -- a quarantine could still have reissued ids.
	if migrating && (rec.Records > 0 || repair.DiscardCount > 0 || quarantined) {
		opts.Logger.Warn("wal created the durable record index floor for an existing data directory",
			"path", floor.Path(), "start_index", start,
			"note", "this data directory predates the durable index floor; until this file existed, a quarantine or an unidentifiable discard could still have reissued record indices and the message ids derived from them")
	}
	// HEAD LOSS: records that were BURNED by an earlier run but that this file
	// does not start above.
	//
	// replay.go reports INTERIOR holes only. It deliberately stopped reporting a
	// gap before the FIRST record, because a log legitimately starts high -- a
	// fresh log after a quarantine begins above the durable floor -- and claiming
	// "records 1..756 are missing" on every start is a loss channel that cries
	// wolf. But that removed a real signal, and replay cannot restore it: replay
	// has no access to the floor and must not gain one, because being
	// floor-independent is exactly what keeps it usable as a read-only fsck on a
	// file with no data directory around it.
	//
	// So the check lives HERE, where the floor IS available, and it is the honest
	// version: an index at or below burnedBeforeThisRun was written or
	// permanently skipped by some earlier run, so if the file now starts ABOVE
	// that mark plus one, the records in between are in no file this recovery can
	// see.
	//
	// IT IS A WEAKER SIGNAL THAN THE INTERIOR-HOLE CHECK, and the difference is
	// worth stating so nobody reads more into it than it says:
	//
	//   - It cannot distinguish LOST from BURNED. A range here may have been
	//     skipped by a reservation a crash never used, discarded by an earlier
	//     recovery, or genuinely lost. It is an upper bound, exactly like
	//     Recovered.MissingRecords.
	//   - It only sees the HEAD. Records lost from the middle or the end of a
	//     file whose surviving head still starts at the right index are invisible
	//     to it; the interior-hole check catches the middle, and nothing catches
	//     a truncated tail whose indices were never recorded anywhere but the
	//     ceiling.
	//   - It says nothing at all when the floor is missing (a migrated data
	//     directory reports burned 0), which is the case with the least
	//     information and the one most likely to have lost something.
	if rec.FirstIndex > 0 && rec.FirstIndex > burnedBeforeThisRun+1 {
		opts.Logger.Warn("wal recovered a log that starts above the durable index floor",
			"path", path, "first_index", rec.FirstIndex,
			"burned_before_this_run", burnedBeforeThisRun,
			"missing_from", burnedBeforeThisRun+1, "missing_to", rec.FirstIndex-1,
			"index_floor", floor.Path(),
			"note", "records in that range were burned by an earlier run but are in no file this recovery can see: lost from the media, discarded by an earlier recovery, or an index range a crash reserved and never used. It is an UPPER BOUND on loss, not a count of it")
	}

	if rec.Records > 0 {
		opts.Logger.Info("wal replayed",
			"path", path, "records", rec.Records, "applied", rec.Applied,
			"aborted", rec.Aborted, "dangling", len(rec.Dangling),
			"discarded", rec.DiscardCount, "missing_records", rec.MissingRecords,
			"first_index", rec.FirstIndex, "next_index", rec.NextIndex)
	}
	// EVERY replay-stage discard reaches the operator log. Replay has no logger
	// of its own -- that is what keeps it usable as a read-only fsck -- so this
	// loop is where the "nothing is discarded silently" contract is actually
	// kept, and there are tests that fail if a discard does not appear here.
	logDiscards(opts.Logger, path, rec.Discarded, rec.DiscardCount)
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

	// ---------------------------------------------------------------------
	// THE APPEND-ONLY MESSAGE AUDIT LOG (DUR-5, invariant 6). A SECOND file in
	// the same data directory, recovered by the same rules as the WAL and
	// authenticated by the same per-directory key.
	//
	// It is opened LAST, deliberately: everything above it can still fail, and
	// opening it first would mean unwinding a descriptor on every one of those
	// paths. From here there is exactly one failure to unwind, and it closes the
	// WAL writer before returning.
	//
	// It gets NO INDEX FLOOR. The floor protects the WAL's index because message
	// SEQUENCES are derived from it; nothing is derived from an audit record's
	// index. See audit.go.
	// ---------------------------------------------------------------------
	auditW, auditRepair, err := openAuditLog(opts.Dir, c, rec.Records, opts.Logger)
	if err != nil {
		wErr := w.Close()
		return nil, fmt.Errorf("wal: open log %s: opening the append-only message audit log: %w (wal close: %v)", path, err, wErr)
	}
	rec.AuditRepaired = auditRepair

	return &Log{w: w, audit: auditW, applier: opts.Applier, logger: opts.Logger, now: now, recovered: rec}, nil
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

// AuditPath returns the append-only message audit log this Log writes to
// (<data-dir>/bus.audit), or "" if there is none. It is for operator messages,
// fsck tools and tests.
func (l *Log) AuditPath() string {
	if l.audit == nil {
		return ""
	}
	return l.audit.Path()
}

// IndexFloorPath returns the durable record-index floor file backing this Log
// (<data-dir>/wal-index-floor), or "" if there is none.
//
// It is for operator messages and tests. It is NOT a hook for writing to that
// file: lowering it by hand reissues indices this data directory has already
// authorised, which is exactly what the file exists to prevent.
func (l *Log) IndexFloorPath() string {
	if l.w == nil || l.w.floor == nil {
		return ""
	}
	return l.w.floor.Path()
}

// Close closes the underlying WAL and the append-only message audit log.
//
// It blocks until any in-flight transaction has committed or aborted, because
// closing the file underneath an open prepare would leave a caller holding a
// Txn that can no longer be resolved. It is idempotent.
//
// BOTH files are closed even if the first close fails, and the FIRST error is
// the one returned: leaving the audit log's descriptor open because the WAL
// failed to close would leak it on every restart of an already-unhappy bus.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.w.Close()
	if l.audit != nil {
		if aErr := l.audit.Close(); aErr != nil && err == nil {
			err = aErr
		}
	}
	return err
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
	// The applied-key record is canonicalised with the SAME helper and the same
	// validation as the body, so the two can never disagree about what "the
	// bytes that were written" means. The error context names it separately so
	// an operator can tell which of the two failed.
	idemRec, err := canonicalBody(e.Idem)
	if err != nil {
		return nil, fmt.Errorf("wal: prepare in %s: %w: entry idem record: %v", l.Path(), ErrInvalidBody, err)
	}
	// THE AUDIT RECORD IS VALIDATED HERE, with the body and the idem record, and
	// for the same reason: an entry that cannot be written completely must leave
	// BOTH files byte-for-byte unchanged. Validating in Commit instead would mean
	// discovering the problem with a durable prepare already on disk, which turns
	// a rejected write into a dangling transaction for recovery to clean up.
	//
	// It FAILS THE WRITE rather than skipping the audit record. Invariant 6 says
	// every message is written to the audit log; a message the trail cannot
	// describe is one this bus declines to accept.
	//
	// THE RECORD IS COPIED BEFORE IT IS VALIDATED, and the COPY is what gets
	// written. Commit encodes it later, and between here and there the caller's
	// memory is its own: without the copy, a caller that reused its AuditRecord
	// or its Recipients slice would have its LATER value written to the trail,
	// unvalidated, and "validated in Begin" would be a claim about bytes nobody
	// ever wrote. Body and Idem are canonicalised into fresh bytes just above
	// for the same reason.
	auditRec := e.Audit.clone()
	if auditRec != nil {
		if err := auditRec.validate(); err != nil {
			return nil, fmt.Errorf("wal: prepare in %s: %w", l.Path(), err)
		}
		// AN ALREADY-POISONED AUDIT LOG IS REFUSED HERE, NOT IN Commit.
		//
		// The audit writer's poison LATCHES until the process restarts, so once
		// a write or fsync on bus.audit has failed, every later message is going
		// to fail. Discovering that in Commit costs a PREPARE and an ABORT record
		// -- two fsynced WAL appends and an ERROR line -- per attempt, and a
		// client doing the right thing and retrying would grow the WAL and the
		// operator log without limit while never getting a different answer. The
		// security gate measured 4714 WAL bytes and 40 fsyncs over 20 retries.
		//
		// So the answer is given before anything is written. This is a
		// FAST PATH, not the guarantee: the writer can poison between this check
		// and the append, which is why Commit still routes an audit failure
		// through failBeforeCommit.
		if l.audit != nil {
			if err := l.audit.poisonErr(); err != nil {
				return nil, fmt.Errorf("wal: prepare in %s: the message audit log %s cannot be written, so this message cannot be accepted (invariant 6: every message is written to the audit log): %w",
					l.Path(), l.audit.Path(), err)
			}
		}
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

	payload, err := encodePrepareWithIdem(e.Kind, body, idemRec, l.now())
	if err != nil {
		return nil, fmt.Errorf("wal: prepare in %s: encode payload: %w", l.Path(), err)
	}
	rec, err := l.w.Append(TypePrepare, payload)
	if err != nil {
		// Includes ErrPoisoned from a torn write: surfaced, never swallowed.
		return nil, fmt.Errorf("wal: prepare in %s: %w", l.Path(), err)
	}

	handedOver = true
	return &Txn{l: l, prepareIndex: rec.Index, entry: Entry{Kind: e.Kind, Body: body, Idem: idemRec, Audit: auditRec}}, nil
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

	// -----------------------------------------------------------------------
	// THE AUDIT RECORD (DUR-5, invariant 6), written and fsynced HERE: AFTER the
	// prepare fsync and BEFORE the commit fsync.
	//
	// That position is the whole design and must not be moved. It makes the
	// audit log a SUPERSET of committed history -- a crash in this window leaves
	// an audit record for a message that never committed, and recovery discards
	// the dangling prepare while the audit record stays. The trail may therefore
	// OVER-report, never under-report. Writing it after the commit fsync would
	// invert that: a crash would leave an acknowledged message with no trace in
	// the trail, which is the failure this file exists to prevent.
	//
	// It is fsynced before the commit for the same reason (Writer.Append does not
	// return until the bytes are on stable storage). No "write the trail lazily"
	// optimisation is admissible here; it would trade the property above for
	// nothing but latency.
	// -----------------------------------------------------------------------
	if t.entry.Audit != nil {
		if l.audit == nil {
			// Unreachable today -- only Open builds a Log, and Open always opens
			// the audit file -- but it goes through failBeforeCommit like every
			// other failure in this block. A bare return here would be the ONE
			// path that leaves a durable prepare with no abort record and no
			// operator line, which is precisely the state recovery should never
			// have to infer.
			return Committed{}, t.failBeforeCommit(fmt.Errorf("wal: commit prepare %d in %s: the entry requests an audit record but this Log has no audit log open",
				t.prepareIndex, l.Path()))
		}
		auditPayload, err := encodeAudit(t.entry.Audit, t.prepareIndex)
		if err != nil {
			return Committed{}, t.failBeforeCommit(fmt.Errorf("wal: commit prepare %d in %s: encode audit payload: %w",
				t.prepareIndex, l.Path(), err))
		}
		if _, err := l.audit.Append(TypeAuditMessage, auditPayload); err != nil {
			return Committed{}, t.failBeforeCommit(fmt.Errorf("wal: commit prepare %d in %s: appending to the message audit log %s: %w",
				t.prepareIndex, l.Path(), l.audit.Path(), err))
		}
	}

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

// failBeforeCommit resolves a transaction that failed AFTER its prepare was
// fsynced but BEFORE its commit record was written -- today, only a failure to
// write the audit record.
//
// It writes a durable ABORT so recovery is told, in the file, that this prepare
// will never commit, rather than leaving a dangling prepare for replay to infer.
// The entry is NOT accepted history and was never acknowledged, so nothing is
// lost by abandoning it; what would be lost is the operator's ability to tell
// "the bus was killed mid-transaction" from "the bus could not write the audit
// trail", and those call for different responses.
//
// THE ABORT IS BEST-EFFORT AND ITS FAILURE NEVER MASKS THE ORIGINAL CAUSE. If
// the audit log could not be written, the WAL is quite likely unwritable too
// (same disk, probably full); a prepare left dangling is a state recovery already
// handles correctly -- it is discarded and its index burned. The caller is
// always told about the FIRST failure, which is the one that explains the rest.
//
// The caller must hold the transaction lock, which Commit does for its whole
// body, and must not call Commit's remaining steps afterwards.
func (t *Txn) failBeforeCommit(cause error) error {
	l := t.l
	l.logger.Error("wal could not complete a durable write and is abandoning the transaction",
		"path", l.Path(), "prepare_index", t.prepareIndex, "kind", t.entry.Kind, "err", cause,
		"effect", "the entry is NOT accepted history and was never acknowledged; the prepare is abandoned and its record index is burned")
	payload, err := encodeAbort(t.prepareIndex, "the audit record could not be written")
	if err != nil {
		l.logger.Error("wal could not encode the abort record for an abandoned transaction",
			"path", l.Path(), "prepare_index", t.prepareIndex, "err", err,
			"effect", "the prepare is left dangling; recovery discards it and burns its index, which is the same outcome")
		return cause
	}
	if _, err := l.w.Append(TypeAbort, payload); err != nil {
		l.logger.Error("wal could not record the abort for an abandoned transaction",
			"path", l.Path(), "prepare_index", t.prepareIndex, "err", err,
			"effect", "the prepare is left dangling; recovery discards it and burns its index, which is the same outcome")
	}
	return cause
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
//	PREPARE  {"kind":"<Entry.Kind>","ts":"<RFC3339Nano>","body":<Entry.Body>,"idem":<opaque JSON, omitted when absent>}
//	COMMIT   {"prepare_index":<uint64>}
//	ABORT    {"prepare_index":<uint64>,"reason":"<string>"}
//
// "idem" is IDEM-11's applied-key record (see Entry.Idem). It is OMITTED
// ENTIRELY when the entry carries none, so a prepare written for an entry with
// no applied-key record is BYTE-IDENTICAL to one written before the field
// existed -- an existing log and a new one are the same file for every entry
// that does not use the field. See TestPrepareWithoutIdemIsByteIdentical.
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
	// Idem is omitempty so an entry with no applied-key record encodes to the
	// exact bytes this codec produced before the field existed. See the block
	// comment above and Entry.Idem.
	Idem json.RawMessage `json:"idem,omitempty"`
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

// encodePrepare encodes a prepare payload for an entry that carries NO
// applied-key record. It is the pre-IDEM-11 form, kept as its own name so that
// every existing call site -- and every byte-identity proof written against it
// -- continues to describe exactly the bytes it always did.
func encodePrepare(kind string, body json.RawMessage, ts time.Time) ([]byte, error) {
	return encodePrepareWithIdem(kind, body, nil, ts)
}

// encodePrepareWithIdem encodes a prepare payload, optionally carrying
// IDEM-11's applied-key record (Entry.Idem).
//
// A nil idem is OMITTED from the JSON entirely rather than written as null:
// with omitempty on the field, encodePrepareWithIdem(k, b, nil, ts) and the
// pre-IDEM-11 encoder produce identical bytes. That is what keeps this change
// additive on disk instead of a silent format change.
func encodePrepareWithIdem(kind string, body, idem json.RawMessage, ts time.Time) ([]byte, error) {
	if body == nil {
		body = json.RawMessage("null")
	}
	return encodeJSON(preparePayload{Kind: kind, TS: ts.UTC().Format(time.RFC3339Nano), Body: body, Idem: idem})
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
	// The applied-key record is normalised the same way, so a record written
	// with no idem field, one written with an explicit null, and one written by
	// a pre-IDEM-11 binary all decode to the same nil.
	idemRec := p.Idem
	if len(idemRec) == 0 || string(idemRec) == "null" {
		idemRec = nil
	}
	return Entry{Kind: p.Kind, Body: body, Idem: idemRec}, ts, nil
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
