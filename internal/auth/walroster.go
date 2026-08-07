package auth

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// DurableWriter is the narrow slice of *wal.Log this roster writes through.
//
// One method, deliberately, and an INTERFACE rather than the concrete *wal.Log
// — the same shape and the same reasoning as hub.DurableLog: Write is the whole
// of invariant 4 as this package needs it (hand over an entry, get back a
// Committed only once it is prepared, committed and fsynced), Begin/Commit/
// Abort/Close belong to the process lifecycle that main owns, and a roster that
// could reach them could reorder the one guarantee it exists to preserve. It
// also lets a test drive the write path without a data directory.
type DurableWriter interface {
	Write(wal.Entry) (wal.Committed, error)
}

// WALRoster is the durable Roster: every enrolment is recorded through
// internal/wal's two-phase write path, fsynced at prepare and again at commit,
// and only then applied to the in-memory serving copy (invariants 4 and 5). On
// restart the roster is rebuilt by replay, because WALRoster is also the
// wal.Applier for its own record kind.
//
// # Construction order — the chicken-and-egg, and how it is resolved
//
// wal.Open needs the Applier BEFORE the *wal.Log exists (replay runs inside
// Open and hands every committed entry to the applier before Open returns). So
// the wiring is three steps and the order is not optional:
//
//	r := auth.NewWALRoster(logger)                                  // 1. applier first
//	log, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r, ...})  // 2. replay fills r
//	if err := r.Attach(log); err != nil { ... }                      // 3. now it can write
//
// Between steps 1 and 3 the roster can be READ and REBUILT but not WRITTEN:
// Put returns ErrNotAttached. That is the correct order — recovery must finish
// before the first live enrolment, and a roster that accepted a Put before its
// log existed would be claiming durability it does not have.
//
// # Sessions are NOT persisted, and neither is anything else
//
// This type writes EXACTLY ONE record kind (RecordKind) and nothing else. No
// session, no pending challenge, no idempotency key is written here. Sessions
// are a deliberate memory-only exception (see doc.go): they are short-lived
// credentials with a one-hour ceiling, losing one costs an agent a single
// challenge/response round trip, and persisting live credentials would put
// replayable material on disk for no benefit.
//
// The zero value is not usable; construct with NewWALRoster. It is safe for
// concurrent use.
type WALRoster struct {
	log *logging.Logger

	// writeMu serialises the WHOLE check-then-write of Put: the duplicate check,
	// the encode and the durable Write, including the Apply that Write performs
	// at the end of Commit. It is NOT the map lock.
	//
	// Two mutexes rather than one because Apply is called FROM INSIDE Write —
	// wal.Txn.Commit calls the applier after the commit fsync, on the same
	// goroutine — so a single mutex held across Write would deadlock the moment
	// Apply tried to take it. Put therefore never holds mu across Write.
	writeMu sync.Mutex

	// mu guards byID and is held only for the duration of a map operation. It is
	// the lock Apply, Get and Len take.
	mu   sync.Mutex
	byID map[string]RosterEntry

	// w is the durable log, set once by Attach. It is read under writeMu (Put)
	// and written under writeMu (Attach), so no separate lock is needed.
	w DurableWriter
}

// NewWALRoster returns a roster that is not yet attached to a durable log. It
// is ready to receive replayed entries through Apply; it cannot accept a Put
// until Attach has been called. logger may be nil (logging.Logger's methods are
// nil-safe), in which case discards are still refused-and-skipped but nobody
// hears about them — pass a logger in production, because "every discard is
// LOGGED" is the absolute half of invariant 6.
func NewWALRoster(logger *logging.Logger) *WALRoster {
	return &WALRoster{log: logger, byID: make(map[string]RosterEntry)}
}

// Attach binds the roster to the durable log it writes through. It must be
// called EXACTLY ONCE, after wal.Open has returned (see the type doc for the
// ordering).
//
// A second call is an ERROR and changes nothing. Two logs would mean two
// distinct durable histories behind one in-memory roster, and whichever won the
// race would silently own the enrolments the other had already acknowledged.
// A nil writer is likewise an error rather than a silent no-op: it would leave
// Put succeeding in memory with nothing on disk, which is the exact false
// durability claim this type exists to remove.
func (r *WALRoster) Attach(w DurableWriter) error {
	if w == nil {
		return errors.New("auth: attaching durable roster: the durable writer must not be nil; a roster with no log would acknowledge enrolments that never reached disk (invariant 4)")
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.w != nil {
		return errors.New("auth: attaching durable roster: already attached; a roster is bound to exactly one durable log, and a second would give one in-memory roster two durable histories")
	}
	r.w = w
	return nil
}

// Apply implements wal.Applier: it folds one committed entry into the serving
// copy. It runs BOTH during recovery (inside wal.Open, before anything can
// reach this roster) and on every LIVE commit (inside Txn.Commit, after the
// commit record is fsynced) — and, per wal.Applier's contract, it cannot tell
// the two apart. It does not need to: the map insert is the same either way,
// and there is no admission control here to exempt.
//
// # Entries of another Kind are skipped SILENTLY
//
// The log is shared with store.RecordKind messages and with whatever kinds
// later epics add. A roster that treated them as damage would fill the log with
// false alarms — the same words, and the same reasoning, as Hub.Apply.
//
// # A record it cannot understand is DISCARDED, LOUDLY
//
// Returning an error here would abort recovery and refuse to start the bus (or,
// on a live commit, poison the log). Invariant 6 settled that trade on
// 2026-08-02: recovery ALWAYS reaches a running server, damaged records are
// discarded, and the absolute requirement is that every discard is LOGGED,
// specifically. So an enrolment record that does not decode is reported at
// ERROR with its prepare and commit indices, skipped, and the bus starts.
//
// BE HONEST ABOUT WHAT THAT COSTS. The discarded agent was ACKNOWLEDGED as
// enrolled — it holds an agent id this bus minted and told it — and it is now
// silently absent from the roster. Its next authenticated call fails with
// ErrUnknownAgent and it must re-enrol, under a NEW id, because the old suffix
// is burned. Nothing tells that agent why.
//
// Its AGENT ID is NOT handed back out, and that is the one part of the damage
// that does not compound — but the reason is nothing in this package. The
// suffix floor lives in ids.OpenNameSuffixes, which persists each name's floor
// BEFORE issuing it, so a number burned by an enrolment stays burned however
// badly the enrolment record itself is later damaged. Do NOT read
// EnrolmentSuffixesInWAL as the thing protecting this: it reports only what the
// surviving enrolment records say, so a record too damaged to decode is exactly
// a record it cannot count either.
//
// # A DUPLICATE agent id KEEPS THE FIRST record
//
// Never an overwrite. An overwrite rebinds a live identity to a different
// keypair, which is the worst outcome available on this path (invariants 1 and
// 3): every DM addressed to that id would route to the new key holder and every
// ACL naming it would name someone else. It is logged at ERROR and skipped.
//
// It does NOT return an error, even though a duplicate id in the log is a
// serious invariant breach. Returning one aborts recovery / poisons the log,
// and by the time this runs the duplicate is already durable — refusing to
// start does not un-write it, it only turns one damaged record into an outage.
func (r *WALRoster) Apply(c wal.Committed) error {
	if c.Entry.Kind != RecordKind {
		return nil
	}
	e, err := Decode(c.Entry.Body)
	if err != nil {
		r.log.Error("DISCARDING an enrolment record that could not be decoded; the agent it named is NOT in this bus's roster and must re-enrol under a new id",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"err", err,
		)
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.byID[e.AgentID]; ok {
		r.log.Error("DISCARDING a DUPLICATE enrolment record: this agent id is already in the roster, so the later record is dropped and the FIRST is kept; an agent id is never reused (invariant 1) and overwriting one would rebind a live identity to a different keypair",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"agent_id", e.AgentID,
			"kept_enrolled_at", prev.EnrolledAt,
		)
		return nil
	}
	r.byID[e.AgentID] = copyRosterEntry(e)
	return nil
}

// Put implements Roster: it records a new enrolment DURABLY and returns only
// once the entry is on stable storage and visible in memory (invariant 4).
//
// The order is fixed and must not be rearranged:
//
//  1. validate the entry — outside every lock, touching no shared state;
//  2. take writeMu, which serialises the whole check-then-write;
//  3. REJECT A DUPLICATE agent id from the in-memory map, BEFORE writing. Doing
//     it here means a duplicate never burns an fsync and never reaches Apply,
//     where the only available handling is to discard it — after it is durable;
//  4. encode the record, so a record that cannot be stored fails with NOTHING
//     written;
//  5. hand it to the log, which fsyncs the prepare, fsyncs the commit, and THEN
//     calls Apply — and it is Apply, not this method, that does the map insert.
//     There is exactly one insertion path, so a replayed enrolment and a live
//     one cannot diverge.
//
// A Write failure is returned UNCHANGED (wrapped only for context) and the
// entry is NOT in memory: Apply never ran, so memory still matches disk.
//
// Put must never hold mu across the Write — Apply takes mu from inside it. See
// the field comments.
func (r *WALRoster) Put(e RosterEntry) error {
	if err := validateRosterEntry(e); err != nil {
		return err
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if r.w == nil {
		// NOT a silent success in memory. A Put that returned nil here would be
		// telling the caller the enrolment is durable when nothing was written,
		// which is the exact false claim this type exists to remove.
		return fmt.Errorf("%w: cannot record the enrolment of %q", ErrNotAttached, e.AgentID)
	}

	r.mu.Lock()
	_, dup := r.byID[e.AgentID]
	r.mu.Unlock()
	if dup {
		return fmt.Errorf("%w: %q", ErrDuplicateAgentID, e.AgentID)
	}

	body, err := Encode(e)
	if err != nil {
		return err
	}
	if _, err := r.w.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		return fmt.Errorf("auth: recording the enrolment of %q durably: %w", e.AgentID, err)
	}

	// CONFIRM THE ENTRY REACHED MEMORY. Write's contract is "durable AND visible
	// in memory" (wal.Log.Write), but this roster's Apply deliberately returns
	// nil on a record it cannot use — it DISCARDS, so recovery of a damaged log
	// still reaches a running bus. That policy is right for replay and wrong to
	// inherit silently here: on a LIVE commit it would let Put return success for
	// an enrolment that is durable but absent from the serving copy, so the agent
	// is told its id and can then never authenticate, with nothing failing.
	//
	// The reachable trigger is not a round-trip bug. It is MIS-WIRING: Attach
	// takes any DurableWriter and cannot check that the log was opened with THIS
	// roster as its Applier. Hand it a log wired to a different applier — easy to
	// do, since the hub is an applier too and the startup path needs to multiplex
	// them — and every Put writes durably, returns nil, and the roster stays
	// permanently empty. This check turns that from a silent, total, invisible
	// failure into a loud one on the first enrolment.
	//
	// It reports failure even though the record IS durable, and that is the
	// correct direction: the write is not lost, it is on disk and will be
	// replayed at the next start. What must not happen is ACKNOWLEDGING an
	// enrolment the serving copy does not have.
	r.mu.Lock()
	_, applied := r.byID[e.AgentID]
	r.mu.Unlock()
	if !applied {
		return fmt.Errorf("auth: the enrolment of %q committed durably but is ABSENT from the serving roster; the record is on disk and will be replayed at the next start, but this roster is not the applier of the log it was attached to (check the wal.Open Applier wiring) or the record was discarded by Apply", e.AgentID)
	}
	return nil
}

// Get implements Roster. The returned entry is a DEEP COPY, exactly as
// MemoryRoster.Get returns one: a caller must not be able to reach into a
// stored credential through the slices it was handed.
func (r *WALRoster) Get(agentID string) (RosterEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[agentID]
	if !ok {
		return RosterEntry{}, false
	}
	return copyRosterEntry(e), true
}

// Len implements Roster.
func (r *WALRoster) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// List implements Roster: every RECOVERED and live agent, deep-copied and
// sorted by AgentID.
//
// This is the method that makes a restart survivable end to end rather than
// only inside this package. See Roster.List: without it the hub rebuilds no
// roster after a restart and refuses every send and every delivery for agents
// that authenticate perfectly well.
func (r *WALRoster) List() []RosterEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedCopies(r.byID)
}
