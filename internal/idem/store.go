package idem

import (
	"fmt"
	"sync"
	"time"
)

// Outcome is what a Lookup found. THE THREE-WAY SPLIT IS THE LOAD-BEARING PART
// OF INVARIANT 10 AND MUST NOT BE COLLAPSED TO A BOOL.
//
// A boolean "have I seen this key?" answers two different questions with one
// value and therefore gets one of them wrong. The distinction:
//
//   - Same key + SAME payload is a legitimate RETRY. The acknowledgement was
//     probably lost in flight. Return the ORIGINAL result, re-apply nothing,
//     error nothing, disconnect nobody. This is the whole point of idempotency:
//     it exists so a well-behaved client can safely retry, and punishing that
//     breaks exactly the clients doing the right thing.
//   - Same key + DIFFERENT payload is a PROTOCOL VIOLATION. The client is
//     reusing a key for new content, which is either a serious bug or an
//     attack. Reject it, log it, and disconnect the offending client.
//
// Collapse the two and you must pick one behaviour for both: either every
// correct retry is punished, or every key-reuse-with-new-content is silently
// swallowed and answered with somebody else's result.
type Outcome int

const (
	// OutcomeNew: never seen inside the window. APPLY it.
	OutcomeNew Outcome = iota
	// OutcomeRetry: same key + SAME payload. Return the ORIGINAL result,
	// re-apply nothing, error nothing, DISCONNECT NOBODY.
	OutcomeRetry
	// OutcomeViolation: same key + DIFFERENT payload. Reject, log, and
	// DISCONNECT the client.
	OutcomeViolation
)

func (o Outcome) String() string {
	switch o {
	case OutcomeNew:
		return "new"
	case OutcomeRetry:
		return "retry"
	case OutcomeViolation:
		return "violation"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// StoreOptions configures NewStore. Every zero value means "the derived
// default", so a caller that has no opinion gets the derivation in retention.go
// rather than an accidental zero window.
type StoreOptions struct {
	// Window is the retention window. 0 means RetentionWindow.
	Window time.Duration
	// MaxEntries is the hard cap on retained records. 0 means MaxEntries.
	MaxEntries int
	// Now supplies the clock retention is evaluated against. nil means
	// time.Now.
	Now func() time.Time
}

// Store is the applied-key table: the durable memory of which (agent,
// operation, key) tuples this bus has already applied, and what each one
// produced.
//
// # THE GUARANTEE, STATED HONESTLY: duplicates are suppressed WITHIN THE
// # RETENTION WINDOW
//
// NOT unconditional exactly-once. A retry arriving after its key expired is
// applied as a NEW operation and produces a SECOND effect. This is the resolved
// form of the contradiction IDEM-11 carried — "a retry past its window MUST
// FAIL CLOSED" versus "duplicates are suppressed within the retention window" —
// and it is written out here in full because the next implementer will
// otherwise re-open it.
//
// Why fail-closed is not available:
//
//   - Idempotency keys are OPAQUE client-supplied strings (IDEM-10). An EVICTED
//     key is byte-indistinguishable from a NEVER-SEEN key, and every legitimate
//     first attempt is a never-seen key. A fail-closed server would therefore
//     have to reject all of them or none of them; there is no third answer
//     available from the key alone. "Reject all of them" means refusing every
//     first attempt, i.e. refusing to serve.
//   - The only mechanisms that make expiry DETECTABLE require changing the key
//     format: a verifiable server-minted mint-time or nonce carried inside the
//     key. That makes the key server-MINTED, which adds a round trip before
//     every mutating call and a second durable table to remember issued tokens
//     — and it buys protection only for retries arriving AFTER
//     RetentionWindow, which the derivation in retention.go is built
//     specifically to exclude. IDEM-10 settled the key format and this task
//     does not reopen it.
//   - IDEM-16's past-the-window test and IDEM-18's PROTOCOL.md wording already
//     assume the bounded-window statement, so it is also the choice that needs
//     no downstream churn.
//
// What makes the boundary HONEST rather than merely accepted, and all three
// have to hold:
//
//   - the window is DERIVED to exceed every known retry horizon with a 2x
//     margin (retention.go), rather than picked;
//   - Stats.Expired counts evictions cumulatively, so they are observable
//     rather than silent;
//   - Stats.OldestAge lets an operator watch how much of the margin is actually
//     being used.
//
// # Eviction is a PURE PREDICATE, which is what keeps memory and disk agreeing
//
// Retention is now.Sub(record.CommittedAt) > window and nothing else. NOTHING
// is written to disk to record an eviction, and that is the point rather than
// an omission: eviction is DERIVED identically on the live path and on the
// replay path, so memory and disk can never disagree about which keys are live.
// The append-only log keeps the bytes; the rebuilt table applies the same
// predicate to them. Task point (f) — eviction must be consistent across memory
// and disk — is therefore satisfied BY CONSTRUCTION, not by a second mechanism
// that could drift out of step with the first.
//
// # The DUR-7 (snapshot/compaction) interaction, specified now
//
// A future snapshot MUST capture the applied-key table WITH each record's
// CommittedAt, and MUST re-apply this same expiry predicate on load. A snapshot
// that stores only the keys cannot expire them at all; a snapshot that resets
// CommittedAt to the snapshot time silently EXTENDS every key's life by the
// age of the snapshot. Both break the window, in opposite directions: the first
// reinstates keys that should be gone, the second keeps live keys alive past
// their derivation. Neither is detectable from the table's own contents
// afterwards.
//
// # Concurrency
//
// Store has its OWN RWMutex and is safe for concurrent use, even though the hub
// holds its writeMu around every mutating call. That is not redundant: Stats is
// read by CORE-5's inspect endpoint OFF the hub's write lock, so an inspect must
// never be able to observe a half-updated table and must never stall a send.
//
// Note that EVERY exported entry point takes the WRITE lock, including the
// nominally read-only Lookup, Full and Stats: all of them expire first, and
// expiry mutates. That is the correct trade — an answer computed from records
// that are no longer retained is wrong, and being wrong here means either
// refusing a send that had room or replaying a result the window no longer
// covers. The read lock is kept available for a future accessor that genuinely
// does not need to expire.
type Store struct {
	mu         sync.RWMutex
	window     time.Duration
	maxEntries int
	now        func() time.Time

	records map[Scope]Record

	// order is the same scopes in INSERTION order. Records are remembered in
	// COMMIT order on both the live path and the replay path, so this slice is
	// sorted by CommittedAt by construction, and expiry is a pop from the front
	// rather than a scan of the whole table.
	order []Scope

	// expired counts evictions cumulatively, for Stats. It is what makes the
	// bound observable instead of assumed.
	expired uint64
}

// NewStore builds an empty applied-key table.
func NewStore(o StoreOptions) *Store {
	s := &Store{
		window:     o.Window,
		maxEntries: o.MaxEntries,
		now:        o.Now,
		records:    make(map[Scope]Record),
	}
	if s.window <= 0 {
		s.window = RetentionWindow
	}
	if s.maxEntries <= 0 {
		s.maxEntries = MaxEntries
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Lookup answers the only question the write path asks: has this exact
// operation already been applied, and if so with what result?
//
// It EXPIRES FIRST, so a table full of keys past their window never refuses or
// mis-answers a live request on the strength of records that are no longer
// retained.
//
// The returned Record is meaningful only for OutcomeRetry (it is the original
// result to replay) and OutcomeViolation (it names what the key was previously
// applied to; the caller must NOT echo either payload back).
func (s *Store) Lookup(sc Scope, fp Fingerprint) (Record, Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	prev, ok := s.records[sc]
	if !ok {
		return Record{}, OutcomeNew
	}
	if prev.Fingerprint != fp {
		return prev, OutcomeViolation
	}
	return prev, OutcomeRetry
}

// Remember records an applied operation.
//
// It returns a wrapped ErrCapacity when the table is at its cap. NOTHING IS
// EVICTED to make room: evicting a live key turns the next retry of it into a
// second effect, which is the double-apply invariant 10 forbids. A refused
// operation is recoverable; a duplicated one is not.
//
// Remembering a scope that is ALREADY PRESENT is a no-op returning nil. That
// makes replay idempotent (a log replayed twice rebuilds the same table), and
// it makes a live double-remember — which would be a bug — harmless rather than
// corrupting: the FIRST record wins, so the stored result stays the one that
// was actually returned to the client.
func (s *Store) Remember(r Record) error {
	sc, err := r.Scope()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if err := r.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Expiry runs BEFORE the duplicate check, not after: a record already past
	// the window is not "already present", it is gone, and treating it as
	// present would silently drop the fresh record that is replacing it.
	s.expireLocked(s.now())
	if _, ok := s.records[sc]; ok {
		return nil
	}
	if len(s.records) >= s.maxEntries {
		return fmt.Errorf("%w: %d applied keys are remembered, the limit; nothing is evicted, because evicting a key turns the next retry of it into a second effect", ErrCapacity, s.maxEntries)
	}
	s.records[sc] = r
	s.order = append(s.order, sc)
	return nil
}

// Full reports whether the table is at its cap, EXPIRING FIRST so a table full
// of keys past their window is not reported as full.
//
// It exists so a caller can refuse an operation BEFORE minting anything
// server-authoritative for it — a sequence spent on an operation that will be
// refused is a sequence burned for nothing (invariant 1 forbids reusing it).
func (s *Store) Full() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	return len(s.records) >= s.maxEntries
}

// Expire drops every record past the retention window. Lookup, Remember and
// Full all expire internally, so a caller never has to call this to stay
// correct; it is exported for a periodic sweep on a quiet bus, where nothing
// else would trigger one.
func (s *Store) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
}

// expireLocked pops expired records off the FRONT of the order slice and stops
// at the first live one. The caller must hold mu for writing.
//
// # Why it stops at the first live record, and what a backwards clock does
//
// order is sorted by CommittedAt by construction (records are remembered in
// commit order on both paths), so the first live record proves every record
// behind it is live too. If the clock ever steps BACKWARDS, that assumption can
// hold a few records longer than the window — and that is the SAFE direction,
// deliberately chosen: a duplicate suppressed too long is correct behaviour
// (the client gets its original result back), while one suppressed too briefly
// is a DOUBLE-APPLY, which invariant 10 exists to prevent and which nothing
// downstream can undo.
func (s *Store) expireLocked(now time.Time) {
	drop := 0
	for drop < len(s.order) {
		r, ok := s.records[s.order[drop]]
		if !ok {
			// Defensive: an order entry with no record cannot happen (only this
			// function removes records, and it removes both together), but if it
			// ever did, leaving it would pin the front of the queue forever.
			drop++
			continue
		}
		if now.Sub(r.CommittedAt) <= s.window {
			break
		}
		delete(s.records, s.order[drop])
		s.expired++
		drop++
	}
	if drop == 0 {
		return
	}
	// COMPACT INTO A FRESH BACKING ARRAY rather than resliding s.order[drop:].
	// Resliding keeps the original array alive with every evicted Scope still
	// referenced by it, so the memory the eviction was supposed to release is
	// never released — the retain-forever bug internal/wal/replay.go's comment
	// describes.
	kept := make([]Scope, len(s.order)-drop)
	copy(kept, s.order[drop:])
	s.order = kept
}

// Stats is the observable state of the applied-key table (task point (g)). It
// is what CORE-5's inspect/metrics endpoint surfaces so the bound is VERIFIED
// in production rather than assumed.
type Stats struct {
	// Count is how many records are currently retained.
	Count int
	// Oldest is the commit time of the oldest retained record; the zero time
	// when the table is empty.
	Oldest time.Time
	// OldestAge is how long ago that was; zero when the table is empty.
	// Watching it approach Window is how an operator sees the derived margin
	// actually being consumed.
	OldestAge time.Duration
	// Expired is the CUMULATIVE count of evictions since this Store was built.
	// It is what makes the bounded-window guarantee observable rather than
	// silent: every increment is one key that a later retry would no longer
	// suppress.
	Expired uint64
	// Window and MaxEntries are the bounds in force, reported alongside the
	// measurements so a reading is interpretable without consulting the build.
	Window     time.Duration
	MaxEntries int
}

// Stats reports the table's observable state. It EXPIRES FIRST so the numbers
// describe what is actually retained, not what has merely not been swept yet —
// an inspect endpoint that reported stale counts would make the bound look
// breached when it was not, or intact when it was not.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	st := Stats{
		Count:      len(s.records),
		Expired:    s.expired,
		Window:     s.window,
		MaxEntries: s.maxEntries,
	}
	if len(s.order) > 0 {
		if r, ok := s.records[s.order[0]]; ok {
			st.Oldest = r.CommittedAt
			st.OldestAge = now.Sub(r.CommittedAt)
		}
	}
	return st
}
