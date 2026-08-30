package store

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// MaxConversations is the hard cap on retained conversation records. It bounds
// the serving copy so a bus cannot be driven to unbounded memory by creating
// conversations without limit. It is enforced ONLY on the live Create path — a
// refused create never becomes durable, so the durable set never exceeds the cap
// and therefore neither does the replayed set. Replay itself never refuses a
// committed record for this cap: discarding durable history to satisfy an
// in-memory bound would violate invariant 5 (recover to a prefix of accepted
// history). Fail-closed, evicting nothing — the same posture ack.admitLocked and
// idem.ErrCapacity take.
const MaxConversations = 1 << 16

// ErrConversationCapacity is the hard cap refusal. It is fail-closed and evicts
// nothing: dropping a live conversation would be silent loss, not a bound.
var ErrConversationCapacity = errors.New("store: the conversation table is at its hard entry cap")

// ErrConversationNotDurable is returned by Create before Attach. It is a REFUSAL,
// not a degraded in-memory mode: a conversation table with no durable log would
// mint ids and report conversations that no restart could reproduce, and
// invariant 4 has no best-effort setting.
var ErrConversationNotDurable = errors.New("store: the conversation table has no durable log attached")

// ConversationDurableLog is the two-phase write path a conversation record is
// recorded through. It is the same one-method seam ack.DurableLog and
// relay.OutboxDurableLog use, and it is an INTERFACE rather than a *wal.Log so
// this package can be tested against a log that kills the process mid-write (see
// conversation_crash_test.go).
type ConversationDurableLog interface {
	Write(e wal.Entry) (wal.Committed, error)
}

// ConversationStore is the durable, bounded serving copy of this bus's
// conversation records (CONV-RECORD).
//
// It follows the build-before-Open / Attach-after three-step ack.Store,
// invite.Store and relay.Outbox use: it is a wal.Applier, so it must exist BEFORE
// wal.Open in order to be in the applier map that replay feeds, and it gets its
// durable log only afterwards through Attach. Replay therefore always finishes
// before the first live write, structurally rather than by remembering to.
//
// # THE LOCK IS NEVER HELD ACROSS A DURABLE WRITE
//
// wal.Log.Write calls Applier.Apply synchronously and this Store IS that applier,
// so holding mu across the write would self-deadlock. Create decides under the
// lock, releases it, writes, and folds the canonical record in — the same shape
// ack.Store's mutating methods use.
type ConversationStore struct {
	// mu guards every field below.
	mu sync.Mutex

	log   *logging.Logger
	now   func() time.Time
	busID string

	durable ConversationDurableLog

	// records maps a conversation id to its record. Keys are unique by invariant 1
	// — ids are server-minted and never reused — so an insert is unconditional.
	records map[string]ConversationRecord
}

// ConversationStoreOptions configures NewConversationStore.
type ConversationStoreOptions struct {
	// BusID is this bus's own server-minted id, used to mint conversation ids
	// (ruling 2). It is validated at construction.
	BusID string

	// Logger receives every discard. It may be nil, in which case a logger on
	// os.Stderr at LevelWarn is used rather than a discarding one: invariant 6
	// makes SILENT loss the defect, so a discard must never go unlogged because
	// the default logger threw it away.
	Logger *logging.Logger

	// Now supplies the created-at clock, overridable for tests. nil means
	// time.Now.
	Now func() time.Time
}

// NewConversationStore builds an unattached conversation table. Create refuses
// with ErrConversationNotDurable until Attach.
func NewConversationStore(o ConversationStoreOptions) (*ConversationStore, error) {
	if err := ids.ValidateBusID(o.BusID); err != nil {
		return nil, fmt.Errorf("store: conversation table bus id: %w", err)
	}
	s := &ConversationStore{
		log:     o.Logger,
		now:     o.Now,
		busID:   o.BusID,
		records: make(map[string]ConversationRecord),
	}
	if s.log == nil {
		// os.Stderr, NOT io.Discard — see ConversationStoreOptions.Logger. A
		// discard on the replay path is invariant-6-relevant and must be visible.
		s.log = logging.New(os.Stderr, logging.LevelWarn)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Name and Kinds make this a wal.Applier for a kind-dispatching multiplexer.
func (s *ConversationStore) Name() string    { return "conversation" }
func (s *ConversationStore) Kinds() []string { return []string{ConversationRecordKind} }

// Attach binds the durable log, after wal.Open has replayed it.
func (s *ConversationStore) Attach(d ConversationDurableLog) error {
	if d == nil {
		return fmt.Errorf("store: attaching the conversation durable log: it must not be nil (%w)", ErrConversationNotDurable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durable != nil {
		return errors.New("store: attaching the conversation durable log: already attached; one in-memory table must not have two durable histories")
	}
	s.durable = d
	return nil
}

// durableLog reads the attached log under the lock. A method rather than a bare
// field read because Attach WRITES s.durable after construction: an
// unsynchronised read from Create would be a data race with it, and a race here
// is a P0.
func (s *ConversationStore) durableLog() ConversationDurableLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.durable
}

// Create mints a server-authoritative id, builds the conversation record,
// records it durably through the two-phase log, and returns the durable record.
//
// It returns only once the record is on stable storage (invariant 4): the write
// is wal.Log.Write's Begin(fsync) -> Commit(fsync), and a nil error means the
// record is durable AND visible in memory. Nothing is acknowledged before it is
// durable.
//
// creator and recipients are validated BEFORE anything is written, so a malformed
// request leaves the log byte-for-byte unchanged. There is deliberately NO
// idempotency here: create-idempotency is CONV-CREATE-CLI's, layered on top of
// this write path, and adding it now would pre-empt that task's ruling.
func (s *ConversationStore) Create(creator, name string, recipients []string) (ConversationRecord, error) {
	durable := s.durableLog()
	if durable == nil {
		return ConversationRecord{}, ErrConversationNotDurable
	}
	id, err := NewConversationID(s.busID)
	if err != nil {
		return ConversationRecord{}, err
	}
	// A defensive copy: the caller keeps ownership of its slice, and the record we
	// validate, encode and retain must not alias a slice the caller can mutate
	// after the durable write.
	rcpts := append([]string(nil), recipients...)
	rec := ConversationRecord{
		ID:         id,
		Creator:    creator,
		Name:       name,
		Recipients: rcpts,
		CreatedAt:  s.now().UTC(),
	}
	// Canonicalised (encoded then decoded) BEFORE the table is touched, so the
	// record folded into memory is byte-identical to the one replay will read
	// back, and so a malformed creator/name/recipient is refused here rather than
	// after a durable write.
	canon, body, err := canonicalConversation(rec)
	if err != nil {
		return ConversationRecord{}, err
	}

	s.mu.Lock()
	if len(s.records) >= MaxConversations {
		held := len(s.records)
		s.mu.Unlock()
		s.log.Error("REFUSING to create a conversation: the conversation table is at its hard entry cap and nothing is evicted to make room. Evicting a live conversation would be silent loss, not a bound",
			"held", held, "limit", MaxConversations, "creator", creator)
		return ConversationRecord{}, fmt.Errorf("%w: %d conversations retained against a limit of %d; the conversation is NOT created and nothing is evicted", ErrConversationCapacity, held, MaxConversations)
	}
	s.mu.Unlock()

	if _, err := durable.Write(wal.Entry{Kind: ConversationRecordKind, Body: body}); err != nil {
		// NOTHING was acknowledged and nothing is in memory.
		return ConversationRecord{}, fmt.Errorf("store: writing the conversation record %s: %w", id, err)
	}
	s.foldIn(canon)
	return canon, nil
}

// Apply folds one committed conversation record into the serving copy.
//
// It is the REPLAY path and it NEVER returns an error, because an error from an
// applier whose COMMIT record is already durable poisons the whole log with
// wal.ErrDiverged. A record this table cannot use is DISCARDED — and every
// discard is logged at ERROR, loudly and specifically, because invariant 6 rates
// the SILENT discard as the defect rather than the discard.
//
// It is also called for a LIVE write, since a kind-dispatching multiplexer routes
// this kind here; Create folds the identical canonical record in again
// afterwards, which is idempotent.
func (s *ConversationStore) Apply(c wal.Committed) error {
	if c.Entry.Kind != ConversationRecordKind {
		return nil
	}
	r, err := DecodeConversationRecord(c.Entry.Body)
	if err != nil {
		s.log.Error("DISCARDING a conversation record that could not be decoded off the log; the conversation will not be served. A record that fails the bounds Encode enforced was written by another version or the log was tampered with — either way it is refused, not trusted (invariant 6)",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex, "err", err)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.records[r.ID]; ok && !conversationsEqual(prev, r) {
		// Two DIFFERENT records under one id. Ids are server-minted and never
		// reused (invariant 1), so this cannot happen without a server bug or a
		// tampered log. Last-writer-wins in the map and it is logged loudly rather
		// than silently overwritten.
		s.log.Error("two DIFFERENT conversation records carry the same id; ids are server-minted and never reused (invariant 1), so this is a server bug or a tampered log. The later record replaces the earlier in the serving copy",
			"conversation_id", r.ID, "prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex)
	}
	s.records[r.ID] = r
	return nil
}

// foldIn applies a record produced by a LIVE, already-durable Create. A capacity
// refusal cannot occur here — Create checked the cap before the write — so this
// is an unconditional insert under the lock.
func (s *ConversationStore) foldIn(r ConversationRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = r
}

// Get returns the retained conversation record for id, and whether it exists.
// The returned record is a deep copy of the recipient slice, so a caller cannot
// reach into the serving copy.
func (s *ConversationStore) Get(id string) (ConversationRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return ConversationRecord{}, false
	}
	return copyConversation(r), true
}

// Len reports the number of retained conversations.
func (s *ConversationStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// copyConversation deep-copies the one slice a record carries out of the store,
// so a caller cannot mutate a slice the store still believes it holds.
func copyConversation(r ConversationRecord) ConversationRecord {
	out := r
	out.Recipients = append([]string(nil), r.Recipients...)
	return out
}

// conversationsEqual reports whether two records are field-for-field identical,
// used only to decide whether an id collision on replay is a benign duplicate or
// a genuine divergence worth logging.
func conversationsEqual(a, b ConversationRecord) bool {
	if a.ID != b.ID || a.Creator != b.Creator || a.Name != b.Name || !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	if len(a.Recipients) != len(b.Recipients) {
		return false
	}
	for i := range a.Recipients {
		if a.Recipients[i] != b.Recipients[i] {
			return false
		}
	}
	return true
}
