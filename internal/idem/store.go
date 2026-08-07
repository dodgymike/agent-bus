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

	// byAgent counts how many retained records each agent currently holds. It is
	// the per-agent fair share's only input (see admitAgentLocked and the
	// derivation in retention.go).
	//
	// IT IS MAINTAINED IN EXACTLY THE SAME CRITICAL SECTIONS AS records AND
	// order — incremented in Remember beside the map insert, decremented in
	// expireLocked beside the map delete — and the two must never drift: a
	// counter that outlives its records would refuse an agent for keys that no
	// longer exist, and one that under-counts would hand an agent more than its
	// share. The map entry is DELETED at zero so the map does not accumulate one
	// entry per agent that has ever sent, which is the same discipline
	// hub.Wait's waitersByAgent defer applies.
	//
	// The BUS-WIDE ENROL scope (Scope.EnrolBusWide, agent == "") is NOT counted
	// here at all — see admitAgentLocked for why.
	byAgent map[string]int

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
		byAgent:    make(map[string]int),
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

// Remember records an applied operation ACCEPTED BY THE LIVE PATH. The replay
// path calls Recover instead, which is this function minus the per-agent share —
// see Recover for why re-adjudicating an already-accepted record is a lost key
// rather than a stricter bus.
//
// It returns a wrapped ErrCapacity when the table is at its cap, and a wrapped
// ErrAgentQuota when the table is under pressure and this record's agent is
// already at its fair share of it (retention.go). NOTHING IS EVICTED to make
// room in either case: evicting a live key turns the next retry of it into a
// second effect, which is the double-apply invariant 10 forbids. A refused
// operation is recoverable; a duplicated one is not.
//
// It enforces the SAME admission predicate Admit does, so nothing can bypass the
// share by calling Remember directly — Admit exists to move the refusal EARLIER,
// never to be the only place it happens.
//
// Remembering a scope that is ALREADY PRESENT is a no-op returning nil. That
// makes replay idempotent (a log replayed twice rebuilds the same table), and
// it makes a live double-remember — which would be a bug — harmless rather than
// corrupting: the FIRST record wins, so the stored result stays the one that
// was actually returned to the client.
func (s *Store) Remember(r Record) error { return s.remember(r, true) }

// Recover rebuilds one record that a PREVIOUS run of this bus already accepted
// — the replay path, and nothing else. It is Remember MINUS the per-agent fair
// share, and the omission is the whole reason it exists as a separate entry
// point rather than a flag.
//
// # Why replay must not adjudicate the share
//
// The fair share is a LIVE ADMISSION policy: a decision about whether to accept
// an operation that has not happened yet. It is NOT a property of a record. A
// record on disk is proof that admission ALREADY SUCCEEDED — the operation was
// accepted, acknowledged to the client and fsynced — and re-testing that
// decision at replay can only ever DISAGREE with a decision that has already
// been acted on. Disagreement here is not a stricter bus, it is a LOST key: the
// rebuilt table silently drops a key the pre-restart bus held, and the client's
// next retry of it is applied as a SECOND effect — the double-apply invariant 10
// exists to prevent, delivered by the very mechanism added to protect fairness.
// Refusing costs nothing at admission time (the client can retry) and costs
// correctness at replay time (there is nobody to refuse; the operation is
// already in the history).
//
// # Two concrete ways re-adjudication WOULD refuse an accepted record
//
// This is not a theoretical tidiness argument. Both of these are reachable, so
// do NOT "simplify" Recover back into Remember:
//
//  1. A BACKWARDS CLOCK. expireLocked deliberately tolerates one (see its
//     "steps BACKWARDS" note): retaining records LONGER than the window is the
//     safe direction FOR EXPIRY. It is the UNSAFE direction here. A larger
//     retained set raises the count (engaging the pressure gate), raises the
//     distinct-agent count (shrinking the share) and raises the agent's own
//     holding — all three move toward REFUSAL, so the replayed set can be a
//     SUPERSET of what was retained live and replay can refuse what the live
//     path admitted.
//  2. A LOG WRITTEN BEFORE THE FAIR SHARE EXISTED, which needs no clock anomaly
//     at all and fires exactly once, at the upgrade. The old bus-wide-only cap
//     let ONE agent hold up to the full MaxEntries inside the retention window.
//     Replaying such a log through the share would refuse everything past
//     maxEntries/(agents+1) — a durability improvement turned into a durability
//     regression, at the one moment it is hardest to notice. The same shape
//     appears whenever the bound is reconfigured downward between runs.
//
// # What Recover still does, and why each part is NOT optional
//
// It validates the record (a record read off disk is untrusted input, invariant
// 1 — "this server wrote it" is exactly what corruption disproves); it expires
// first (the window is a pure predicate re-derived identically on both paths);
// it keeps the already-present no-op (so replaying a log twice rebuilds the same
// table); it enforces the GLOBAL maxEntries cap (that is a MEMORY bound, not an
// admission policy, and a bound that held on only one path is not a bound); and
// it increments byAgent, so the rebuilt table's per-agent counters are correct
// for the LIVE traffic that follows the restart. Only the adjudication is
// dropped, because only the adjudication has already been made.
//
// # THE RESIDUAL THIS ACCEPTS, STATED RATHER THAN GLOSSED
//
// Because replay never refuses, a rebuilt table can hold one agent ABOVE its
// share — and, in the pathological case the backwards-clock test builds, holding
// the WHOLE table. Two consequences, and only the first is fairness working as
// intended:
//
//   - The over-share agent is FROZEN until its own keys age out. That is correct
//     and is exactly what the pre-restart bus would have done.
//   - A different agent's next send can then meet the GLOBAL cap (ErrCapacity)
//     rather than the fair share, so for that window the per-agent guarantee is
//     suspended for the victim as well. Fairness does not survive a table that
//     was already full when it was rebuilt.
//
// That is the deliberate trade and not an oversight: never dropping an accepted
// key outranks fairness, because a dropped key is an unrecoverable double-apply
// while an unfair window is a refusal the client can retry past. Narrowing it
// would mean evicting live keys at replay, which is the very thing invariant 10
// forbids.
func (s *Store) Recover(r Record) error { return s.remember(r, false) }

// remember is the ONE place anything is inserted into records, order and
// byAgent. Remember and Recover differ in exactly one boolean and share
// everything else, so the two paths cannot drift apart into disagreeing about
// validation, expiry, the duplicate no-op or the global cap — the only
// difference between them is the one difference that is intended.
func (s *Store) remember(r Record, enforceShare bool) error {
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
	// The already-present early return stays ABOVE the per-agent check below: a
	// re-Remember of a key the store already holds adds NOTHING to the table and
	// so cannot be the record that breaches a share. Refusing it would break
	// replay idempotency for a record that is already counted in its agent's
	// bucket.
	if _, ok := s.records[sc]; ok {
		return nil
	}
	// The GLOBAL cap is checked on BOTH paths: it is the memory bound, and a
	// memory bound that replay could exceed is not a bound.
	if len(s.records) >= s.maxEntries {
		return fmt.Errorf("%w: %d applied keys are remembered, the limit; nothing is evicted, because evicting a key turns the next retry of it into a second effect", ErrCapacity, s.maxEntries)
	}
	if enforceShare {
		if err := s.admitAgentLocked(sc); err != nil {
			return err
		}
	}
	s.records[sc] = r
	s.order = append(s.order, sc)
	if !sc.enrolBusWide {
		s.byAgent[sc.agent]++
	}
	return nil
}

// Admit answers "would this operation be accepted right now?" — expiring first,
// then applying the SAME predicate Remember applies: nil, a wrapped ErrCapacity
// (the bus-wide cap), or a wrapped ErrAgentQuota (this agent's fair share).
//
// IT IS THE PRE-MINT REFUSAL POINT, and the only one: it exists so a caller can
// refuse an operation BEFORE minting anything server-authoritative for it. A
// sequence spent on an operation that will be refused is a sequence burned for
// nothing, and invariant 1 forbids reusing it. (Full checks the bus-wide cap
// only and is deliberately NOT this — see its doc.)
//
// It is ADVISORY, not a reservation. Nothing is held between Admit and Remember,
// so a caller that does not serialise the two (hub.publish holds writeMu across
// both) can be admitted here and refused there. That is the safe direction: the
// authoritative check is the one inside Remember.
func (s *Store) Admit(sc Scope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	if len(s.records) >= s.maxEntries {
		return fmt.Errorf("%w: %d applied keys are remembered, the limit; nothing is evicted, because evicting a key turns the next retry of it into a second effect", ErrCapacity, s.maxEntries)
	}
	return s.admitAgentLocked(sc)
}

// admitAgentLocked is the PER-AGENT FAIR SHARE (IDEM-11-FU-FAIRSHARE). The
// caller must hold mu for writing and must have expired first; the global cap is
// checked by the caller, before this.
//
// The rule and its full derivation — why the pressure line is the free/used
// crossover, why the divisor is agents+1, and what the halved solo ceiling
// costs — are in retention.go. What belongs HERE are the two properties that
// make the rule safe to apply at this point in the code:
//
// # 1. The cap is keyed on the AGENT ID, and that is safe here for ONE specific
// reason
//
// A Record only exists because an authenticated, server-minted, fully-qualified
// "<bus-id>.<agent-id>" (invariant 2) performed a mutating operation. The key of
// this bucket is therefore a PROVEN IDENTITY, not an attacker-chosen label: a
// flooder cannot make its keys land in a victim's bucket, it can only fill its
// own, so a refusal at the share is always SELF-INFLICTED.
//
// That distinction is not theoretical — this project got it wrong once already.
// auth.BeginSession's removed MaxPendingPerAgent cap was keyed on an agentID
// that an UNAUTHENTICATED caller supplies, which made it a targeted denial of
// service against any NAMED agent: anyone could burn a victim's quota just by
// naming it, and whichever way the bucket behaved at its limit (evict or refuse)
// the result was a lockout of the victim. See internal/auth/session.go's
// "There is deliberately NO per-agent cap" note. The two places this project got
// it RIGHT are the model for this one, and both argue from a proven key:
// hub.Wait's MaxWaitersPerAgent (internal/hub/wait.go) and the per-agent
// active-session cap in auth.CompleteSession (internal/auth/session.go), which
// is keyed on an id proven by an Ed25519 signature.
//
// # 2. The bus-wide ENROL scope is EXEMPT, and this is NOT an enrol defence
//
// Enrolment has no proven caller to key on (doc.go point 4), so its Scope is
// bus-wide with agent == "". Such records are exempt from the share and
// contribute to NO agent's bucket — bucketing them would either invent an
// "agent" out of an unauthenticated self-reported name (defect 1 above, again)
// or lump every enrol on the bus into one bucket keyed on the empty string,
// which is a bus-wide cap wearing a per-agent hat.
//
// SAY IT PLAINLY: this cap is therefore NOT a defence for the enrol path and
// must never be mistaken for one. The unauthenticated enrol-squat risk doc.go
// point 4 records is INVITE-GATE's problem, not this one. Bus-wide records do
// still count toward the GLOBAL cap and toward the pressure line, because they
// occupy the same table.
//
// # Restart consistency: replay does not run this rule AT ALL
//
// The reason replay can never refuse what the live path accepted is now
// STRUCTURAL, not an argument about clocks: the replay path calls Recover, which
// does not call this function. This rule is a LIVE ADMISSION policy and applies
// only to operations that have not happened yet. See Recover for why — and for
// the two concrete cases (a backwards clock; a log written before the fair share
// existed) that would otherwise make replay stricter than the run that accepted
// the record.
func (s *Store) admitAgentLocked(sc Scope) error {
	if sc.enrolBusWide {
		return nil
	}
	if len(s.records) < s.pressureLineLocked() {
		return nil
	}
	share := s.fairShareLocked()
	held := s.byAgent[sc.agent]
	if held < share {
		return nil
	}
	// The message names the agent, what it holds, the share in force and how
	// many agents that share is divided among, so an operator can tell an
	// abusive client from a merely busy one FROM THE LOG LINE ALONE. Modelled on
	// auth.CompleteSession's per-agent active-session refusal, down to spelling
	// out that nothing is evicted to make room.
	return newAgentQuotaError("agent %q holds %d of the %d applied keys retained by %d agents, at its fair share of %d while the table is under pressure (the share is the %d-entry limit divided by %d agents plus one, so an agent that has not yet arrived still has room); one of its OWN keys must age out of the retention window before another is admitted, and none is evicted to make room, because evicting a key turns the next retry of it into a second effect",
		sc.agent, held, len(s.records), len(s.byAgent), share, s.maxEntries, len(s.byAgent))
}

// pressureLineLocked is the fill level at which the fair share starts being
// enforced: the point where FREE space stops exceeding USED space. Derived from
// this store's own maxEntries, not from the package constant, so a store built
// with StoreOptions.MaxEntries is testable (PressureLine is the same number for
// the default bound).
func (s *Store) pressureLineLocked() int { return s.maxEntries / 2 }

// fairShareLocked is maxEntries/(agents+1) — see retention.go for why the
// divisor carries the +1.
//
// It cannot return 0 at a point where it matters: a zero share needs
// agents >= maxEntries, and every counted agent holds at least one record, so
// count >= agents >= maxEntries would already have been refused by the global
// cap that both callers check first.
func (s *Store) fairShareLocked() int { return s.maxEntries / (len(s.byAgent) + 1) }

// Full reports whether the table is at its BUS-WIDE cap, EXPIRING FIRST so a
// table full of keys past their window is not reported as full.
//
// IT IS NOT THE ADMISSION PREDICATE — Admit is. Full answers ONE of the two
// questions admission asks (is the table at maxEntries?) and is blind to the
// other (is this agent at its fair share of it?), so a caller that used it to
// decide whether to serve an operation would admit sends the write path then
// refuses. Admit owns the pre-mint refusal; this is the bus-wide-cap predicate
// alone, retained as documented surface for tests and inspection, and it has no
// production caller.
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
		// The per-agent counter is decremented in the SAME critical section as
		// the map delete, from the SAME Scope that keyed the increment in
		// Remember, and the entry is removed at zero so the map does not
		// accumulate one entry per agent that has ever sent. records and byAgent
		// must never drift; this is the only place either shrinks.
		if sc := s.order[drop]; !sc.enrolBusWide {
			if n := s.byAgent[sc.agent] - 1; n > 0 {
				s.byAgent[sc.agent] = n
			} else {
				delete(s.byAgent, sc.agent)
			}
		}
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

	// Agents is how many DISTINCT agents currently hold at least one retained
	// record. It is the divisor input to the fair share (minus the +1 phantom
	// slot — see retention.go).
	//
	// IT IS A COUNT AND NOTHING MORE. There is deliberately no per-agent
	// breakdown in Stats: this struct is read by CORE-5's inspect endpoint, and
	// a map of agent id -> keys held there would be a cross-agent information
	// leak of exactly the kind Scope exists to prevent (doc.go point 3's PROBING
	// case — who is sending, how much, and when they started). The agent id
	// appears only in the refusal handed to the agent it names, and in the
	// operator's log.
	Agents int

	// Share is the per-agent fair share currently in force,
	// MaxEntries/(Agents+1). It is reported whether or not the table is under
	// pressure, so an operator can see how much headroom one agent has BEFORE
	// the rule starts to bite.
	Share int

	// UnderPressure reports whether the table has reached the fill level at
	// which Share is enforced (Count >= MaxEntries/2; PressureLine for the
	// default bound). False means the fair share is having NO effect on
	// admission at all.
	UnderPressure bool
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
		Count:         len(s.records),
		Expired:       s.expired,
		Window:        s.window,
		MaxEntries:    s.maxEntries,
		Agents:        len(s.byAgent),
		Share:         s.fairShareLocked(),
		UnderPressure: len(s.records) >= s.pressureLineLocked(),
	}
	if len(s.order) > 0 {
		if r, ok := s.records[s.order[0]]; ok {
			st.Oldest = r.CommittedAt
			st.OldestAge = now.Sub(r.CommittedAt)
		}
	}
	return st
}
