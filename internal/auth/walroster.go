package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
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

	if e.LeftAt != nil {
		// A LEAVE/TOMBSTONE record (AUTH-4). Its effect is REMOVAL, not
		// insertion: the agent has left the bus, so it is deleted from the serving
		// copy. Replay of enrol-then-leave therefore ends with the agent ABSENT,
		// which is what makes a left agent stay gone across a restart (invariants
		// 4, 5). The id is not reissued — the suffix floor is durable in
		// ids.OpenNameSuffixes and is never reclaimed on leave (invariant 1).
		if _, ok := r.byID[e.AgentID]; ok {
			delete(r.byID, e.AgentID)
			r.log.Info("agent LEFT the bus and was removed from the serving roster; its id is never reused (invariant 1), so a re-enrolment under the same name gets a new suffix",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"agent_id", e.AgentID,
				"left_at", e.LeftAt.UTC().Format(time.RFC3339Nano),
			)
			return nil
		}
		// The agent is ALREADY ABSENT. This is a legitimate idempotent leave retry
		// (invariant 10) or a leave whose enrolment record was itself discarded
		// earlier — either way there is nothing to remove and the record is
		// benign. Logged at INFO for audit, NOT at ERROR: it is not damage, and
		// treating a harmless retry as an alarm trains an operator to ignore the
		// line. It does not return an error — that would poison the log / abort
		// recovery for a no-op.
		r.log.Info("a leave record names an agent already absent from the roster; nothing to remove (an idempotent leave retry, or a leave whose enrolment was discarded)",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"agent_id", e.AgentID,
			"left_at", e.LeftAt.UTC().Format(time.RFC3339Nano),
		)
		return nil
	}

	if prev, ok := r.byID[e.AgentID]; ok {
		if next, ok := certBindingRecordUpdate(prev, e); ok {
			r.byID[e.AgentID] = next
			r.log.Info("applied a client certificate binding update for an existing agent id; identity fields were unchanged and exactly one live certificate binding was appended",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"agent_id", e.AgentID,
			)
			return nil
		}
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
	_, err := r.put(e, RecordKind, func(e RosterEntry) (json.RawMessage, error) { return Encode(e) })
	return err
}

// PutWithInvite records an enrolment and the invite consumption that authorised
// it as ONE composite entry (EnrolInviteRecordKind), so the two are one
// transaction: one prepare, one commit, two fsyncs, and no window in which an
// agent is enrolled against an invite still marked open — or an invite is spent
// on an enrolment that never happened.
//
// It follows the SAME fixed order as Put (validate outside every lock, take
// writeMu, check attached, reject a duplicate id from memory, encode, write,
// confirm it reached memory); the shared tail is literally the same code, in
// put below, rather than a copy that can drift.
//
// # durable IS LOAD-BEARING — the caller decides an invite's fate by it
//
// A caller holding an invite reservation MUST NOT abort it when durable is
// true. That rule is inherited VERBATIM from internal/invite/store.go's Redeem:
// wal.Txn.Commit returns wal.ErrDiverged AFTER the commit record has been
// appended and fsynced, so on that error the entry — INCLUDING the invite
// consumption record inside it — is on stable storage and only a neighbouring
// applier failed. Releasing the reservation there would leave memory saying
// OPEN while disk says REDEEMED, and the next redemption attempt would be a
// SECOND redemption of a spent invite, which is the one outcome the invite
// package exists to prevent. Abandoning the reservation instead is fail-closed:
// the invite stays locked until a restart rebuilds the table from the log.
//
// Precisely:
//
//   - any failure BEFORE the durable Write   -> durable == false
//   - Write failed with wal.ErrDiverged      -> durable == TRUE (see above)
//   - Write failed any other way             -> durable == false
//   - Write succeeded                        -> durable == true, and the
//     "committed durably but ABSENT from the serving roster" mis-wiring check
//     still runs and still returns an error — with durable == true, because the
//     record IS on disk.
//
// # WALRoster.Apply is deliberately NOT taught about the composite kind
//
// MultiplexApplier expands a composite entry and dispatches the enrolment half
// to this roster as an ordinary RecordKind record, so there stays exactly ONE
// insertion path. A composite reaching Apply directly means the log was opened
// with this roster as its applier instead of the multiplexer — and that is
// caught LOUDLY on the first invited enrolment by the confirm-it-reached-memory
// check below, which is the same mis-wiring detector Put's doc describes.
func (r *WALRoster) PutWithInvite(e RosterEntry, rider InviteRider) (bool, error) {
	return r.put(e, EnrolInviteRecordKind, func(e RosterEntry) (json.RawMessage, error) {
		return EncodeEnrolWithInvite(e, rider)
	})
}

// put is the shared body of Put and PutWithInvite: the fixed order, the two
// locks, the one durable write and the mis-wiring check. Only the wal.Entry.Kind
// and the encoder differ between the two callers.
//
// It reports whether the entry is DURABLE — see PutWithInvite for why that
// answer, and specifically the wal.ErrDiverged case, is load-bearing.
func (r *WALRoster) put(e RosterEntry, kind string, encode func(RosterEntry) (json.RawMessage, error)) (bool, error) {
	if err := validateRosterEntry(e); err != nil {
		return false, err
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if r.w == nil {
		// NOT a silent success in memory. A Put that returned nil here would be
		// telling the caller the enrolment is durable when nothing was written,
		// which is the exact false claim this type exists to remove.
		return false, fmt.Errorf("%w: cannot record the enrolment of %q", ErrNotAttached, e.AgentID)
	}

	r.mu.Lock()
	_, dup := r.byID[e.AgentID]
	// The certificate axis of the duplicate rule, read under the SAME mu
	// acquisition as the id check so the two cannot see different rosters.
	// Serialisation against a concurrent Put is writeMu's job, already held for
	// the whole check-then-write; mu only guards the map itself.
	certErr := checkCertFingerprintUnbound(r.byID, e)
	// The auth-key axis (rule 3), read under the SAME mu acquisition for the same
	// reason — see checkAuthKeyUnbound.
	authKeyErr := checkAuthKeyUnbound(r.byID, e)
	r.mu.Unlock()
	if dup {
		return false, fmt.Errorf("%w: %q", ErrDuplicateAgentID, e.AgentID)
	}
	// BEFORE the encode and the write, exactly like the id check above and for
	// the same reason: a refusal here must never burn an fsync, and must never
	// reach Apply — where the record is already durable and the only available
	// handling is to discard it.
	if certErr != nil {
		return false, certErr
	}
	if authKeyErr != nil {
		return false, authKeyErr
	}

	body, err := encode(e)
	if err != nil {
		return false, err
	}
	if _, err := r.w.Write(wal.Entry{Kind: kind, Body: body}); err != nil {
		// ErrDiverged means the commit record was appended and FSYNCED before the
		// failure: the entry is durable and only a neighbouring applier failed.
		// Reported as durable so a caller holding an invite reservation does not
		// release an invite the log already records as spent.
		durable := errors.Is(err, wal.ErrDiverged)
		return durable, fmt.Errorf("auth: recording the enrolment of %q durably: %w", e.AgentID, err)
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
		// durable is TRUE here: the record IS on disk. What failed is the serving
		// copy, and a caller holding an invite reservation must not un-spend an
		// invite over it.
		return true, fmt.Errorf("auth: the enrolment of %q committed durably but is ABSENT from the serving roster; the record is on disk and will be replayed at the next start, but this roster is not the applier of the log it was attached to (check the wal.Open Applier wiring) or the record was discarded by Apply", e.AgentID)
	}
	return true, nil
}

// BindClientCertificate records the first live client-certificate binding for an
// already-enrolled agent DURABLY. It appends a full roster record whose identity
// fields match the original entry and whose CertBindings list has exactly one
// additional live binding; Apply accepts only that duplicate-agent-id shape.
func (r *WALRoster) BindClientCertificate(agentID string, fp [32]byte, idempotencyKey string, at time.Time) (RosterEntry, bool, error) {
	if _, _, _, err := ids.ParseAgentID(agentID); err != nil {
		return RosterEntry{}, false, fmt.Errorf("%w: %v", ErrUnknownAgent, err)
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if r.w == nil {
		return RosterEntry{}, false, fmt.Errorf("%w: cannot bind a client certificate for %q", ErrNotAttached, agentID)
	}

	r.mu.Lock()
	next, changed, err := appendFirstClientCertificateBinding(r.byID, agentID, fp, idempotencyKey, at)
	r.mu.Unlock()
	if err != nil {
		return RosterEntry{}, false, err
	}
	if !changed {
		return copyRosterEntry(next), false, nil
	}

	body, err := Encode(next)
	if err != nil {
		return RosterEntry{}, false, err
	}
	if _, err := r.w.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		return RosterEntry{}, false, fmt.Errorf("auth: binding a client certificate for %q durably: %w", agentID, err)
	}

	r.mu.Lock()
	applied, ok := r.byID[agentID]
	r.mu.Unlock()
	if !ok || !hasLiveCertBinding(applied, fp) {
		return RosterEntry{}, false, fmt.Errorf("auth: the client certificate binding for %q committed durably but is ABSENT from the serving roster; check wal.Open Applier wiring", agentID)
	}
	return applied, true, nil
}

// Remove implements Roster: it records that agentID has LEFT the bus DURABLY —
// a TOMBSTONE appended through the two-phase write path — and returns only once
// the departure is on stable storage and the agent is gone from the serving
// copy (invariants 4, 6). It is the durable half of AUTH-4's leave.
//
// The tombstone is an APPEND, never an in-place edit and never a truncation
// (invariant 6: the log is append-only in the strict sense). It reuses the
// enrolment record's wal kind (RecordKind) with left_at set, carrying the
// departing agent's own entry, so Apply — the single insertion/removal path —
// deletes it at replay. The id is never reused (invariant 1): the per-name
// suffix floor lives in ids.OpenNameSuffixes and is NOT reclaimed here.
//
// The order mirrors put and must not be rearranged:
//
//  1. parse the id, outside every lock;
//  2. take writeMu, which serialises the whole check-then-write;
//  3. reject an unattached roster (fail-closed, never a silent memory success);
//  4. if the agent is ALREADY ABSENT, return (false, nil) — the idempotent leave
//     retry (invariant 10) writes NOTHING and never reaches Apply;
//  5. encode the tombstone from the agent's current entry, then hand it to the
//     log, which fsyncs the prepare, fsyncs the commit and THEN calls Apply,
//     which does the map DELETE.
//
// A Write failure is returned wrapped and the agent is NOT removed from memory:
// Apply never ran, so memory still matches disk. ErrDiverged means the commit
// record was fsynced before a neighbouring applier failed, so the departure IS
// durable and removed=true is reported for it.
func (r *WALRoster) Remove(agentID string, at time.Time) (bool, error) {
	if _, _, _, err := ids.ParseAgentID(agentID); err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnknownAgent, err)
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if r.w == nil {
		// NOT a silent success in memory: that would claim the departure is
		// durable when nothing was written — the exact false claim this type
		// exists to remove.
		return false, fmt.Errorf("%w: cannot record the departure of %q", ErrNotAttached, agentID)
	}

	r.mu.Lock()
	prev, present := r.byID[agentID]
	r.mu.Unlock()
	if !present {
		// IDEMPOTENT LEAVE RETRY (invariant 10): the agent is already absent — a
		// retry whose first attempt succeeded, or a departure of an agent that was
		// never here. Nothing is written, no fsync is burned, no error is
		// returned, and Apply is never reached.
		return false, nil
	}

	// The tombstone is the agent's own entry with left_at set. Built from the
	// stored entry so it is self-describing and reuses Encode's whole validation.
	tomb := copyRosterEntry(prev)
	u := at.UTC()
	tomb.LeftAt = &u

	body, err := Encode(tomb)
	if err != nil {
		return false, err
	}
	if _, err := r.w.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		// ErrDiverged means the commit record was appended and FSYNCED before the
		// failure: the departure is durable and only a neighbouring applier
		// failed. Reported as removed so a caller does not treat a durable
		// departure as un-done.
		removed := errors.Is(err, wal.ErrDiverged)
		return removed, fmt.Errorf("auth: recording the departure of %q durably: %w", agentID, err)
	}

	// CONFIRM THE AGENT IS NOW ABSENT from the serving copy — the mirror of put's
	// presence check. Apply deletes on a left_at record, so after a successful
	// write the agent must be gone. If it is still present the log was opened
	// with a different applier (the mis-wiring put's doc describes), so the
	// removal committed durably but the serving copy did not reflect it.
	r.mu.Lock()
	_, still := r.byID[agentID]
	r.mu.Unlock()
	if still {
		return true, fmt.Errorf("auth: the departure of %q committed durably but the agent is STILL in the serving roster; the record is on disk and will remove it at the next start, but this roster is not the applier of the log it was attached to (check the wal.Open Applier wiring)", agentID)
	}
	return true, nil
}

func certBindingRecordUpdate(prev, next RosterEntry) (RosterEntry, bool) {
	if prev.AgentID != next.AgentID || prev.Name != next.Name || prev.InviteID != next.InviteID {
		return RosterEntry{}, false
	}
	if prev.ClientCertBootstrapIdempotencyKey != "" || next.ClientCertBootstrapIdempotencyKey == "" {
		return RosterEntry{}, false
	}
	if !bytes.Equal(prev.AuthPublicKey, next.AuthPublicKey) || !bytes.Equal(prev.MessagingPublicKey, next.MessagingPublicKey) {
		return RosterEntry{}, false
	}
	if !prev.Epoch.Equal(next.Epoch) || !prev.EnrolledAt.Equal(next.EnrolledAt) || next.LeftAt != nil {
		return RosterEntry{}, false
	}
	if len(next.CertBindings) != len(prev.CertBindings)+1 {
		return RosterEntry{}, false
	}
	for i := range prev.CertBindings {
		if !certBindingEqual(prev.CertBindings[i], next.CertBindings[i]) {
			return RosterEntry{}, false
		}
	}
	added := next.CertBindings[len(prev.CertBindings)]
	if added.Fingerprint == ([32]byte{}) || added.RetiredAt != nil || hasLiveCertBinding(prev, added.Fingerprint) {
		return RosterEntry{}, false
	}
	if err := validateRosterEntry(next); err != nil {
		return RosterEntry{}, false
	}
	return copyRosterEntry(next), true
}

func certBindingEqual(a, b CertBinding) bool {
	if a.Fingerprint != b.Fingerprint || !a.BoundAt.Equal(b.BoundAt) {
		return false
	}
	if a.RetiredAt == nil || b.RetiredAt == nil {
		return a.RetiredAt == nil && b.RetiredAt == nil
	}
	return a.RetiredAt.Equal(*b.RetiredAt)
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

// AgentIDForCertFingerprint implements Roster over the RECOVERED and live
// roster. See certFingerprintOwner.
//
// This is the implementation for which the ambiguous answer is actually
// reachable: Apply replays whatever the log holds and does not run the
// write-side uniqueness check (it cannot usefully refuse a record that is
// already durable — invariant 6), so a log carrying two live holders of one
// fingerprint recovers into exactly that state and this method must decline to
// pick one.
func (r *WALRoster) AgentIDForCertFingerprint(fp [32]byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return certFingerprintOwner(r.byID, fp)
}

// AgentIDForAuthKey implements Roster over the RECOVERED and live roster. See
// authKeyOwner.
//
// This is the implementation for which the ambiguous answer is actually
// reachable: Apply replays whatever the log holds and does not run the
// write-side uniqueness check (it cannot usefully refuse a record that is
// already durable — invariant 6), so a log carrying two agents with one auth key
// recovers into exactly that state and this method must decline to pick one.
func (r *WALRoster) AgentIDForAuthKey(key ed25519.PublicKey) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return authKeyOwner(r.byID, key)
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
