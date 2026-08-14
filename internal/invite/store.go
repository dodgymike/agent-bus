package invite

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// DurableLog is the two-phase write path, injected.
//
// It is an interface for the same reason auth.Roster is one: the store must be
// constructible without a data directory (for tests and for a read-only
// rebuild) and must be able to record through internal/wal without this package
// depending on how the server assembled its log. It is satisfied by *wal.Log,
// and it is deliberately the SAME shape as httpapi.DurableLog so a server can
// pass one value to both.
//
// wal.Log.Write runs the whole prepare -> fsync -> commit -> fsync -> Apply
// cycle and returns only once the entry is on stable storage, so an operation
// that returns a nil error here is DURABLE. Nothing in this package
// acknowledges anything before that (invariant 4).
type DurableLog interface {
	Write(wal.Entry) (wal.Committed, error)
}

// StoreOptions configures NewStore. Every zero value means "the derived
// default" (retention.go), so a caller with no opinion gets the derivation
// rather than an accidental zero window.
type StoreOptions struct {
	// BusID is the bus minted invites admit to. It is required for Mint; a
	// store built without one can still Lookup and Apply.
	BusID string

	// Durable is the two-phase write path. A nil Durable makes every mutating
	// operation fail with ErrNotDurable — see that error for why this is a
	// refusal and not a degraded in-memory mode.
	Durable DurableLog

	// Logger receives every discard, every refused transition and every reaped
	// reservation. It may be nil.
	Logger *logging.Logger

	// Now supplies the clock every predicate here is evaluated against. nil
	// means time.Now.
	Now func() time.Time

	// MaxInvites is the hard cap on retained records. 0 means MaxInvites.
	MaxInvites int
	// DefaultTTL is the lifetime of an invite minted with no TTL. 0 means
	// DefaultTTL.
	DefaultTTL time.Duration
	// MaxTTL is the longest requestable lifetime. 0 means MaxTTL.
	MaxTTL time.Duration
	// SpentRetention is how long a redeemed record is kept. 0 means
	// SpentRetention.
	SpentRetention time.Duration
	// ReservationTTL bounds a reservation held before Consume. 0 means
	// ReservationTTL.
	ReservationTTL time.Duration
}

// Store is the durable, bounded, single-use invite table: mint, lookup, redeem,
// revoke, expire.
//
// See doc.go for the model, the idempotency scope and the fail-closed rule.
// What belongs here are the three properties that make the implementation safe:
//
// # 1. Apply is the ONLY writer to the table on the replay path, and the live
// # path re-applies the SAME canonical record
//
// Every mutating operation encodes a COMPLETE post-transition record, writes it
// through Durable, and then folds the identical record into memory. The record
// folded in is the one produced by DecodeRecord(Encode(rec)) — literally the
// bytes replay will read — so a live Apply and a replayed Apply cannot drift.
//
// # 2. THE STORE LOCK IS NEVER HELD ACROSS A DURABLE WRITE
//
// wal.Log.Write calls Applier.Apply synchronously, and this Store may itself be
// (or be reached from) that Applier. Holding s.mu across Durable.Write would
// therefore self-deadlock the moment the log is wired to apply live commits.
// Every mutating method here takes the lock, decides, RELEASES it, writes, and
// takes it again to fold the result in. What makes that safe is that no
// decision is left un-guarded: capacity is reserved with pendingMints, and a
// per-invite transition is reserved in inflight, so nothing can slip through
// the window.
//
// # 3. The upsert is MONOTONIC
//
// open -> redeemed and open -> revoked, and nothing else. A record that would
// move an invite BACKWARDS — redeemed -> open, revoked -> redeemed, a second
// and different redemption — is REFUSED and logged loudly, never applied. That
// is the defence against a reordered, replayed or forged record resurrecting a
// spent invite.
type Store struct {
	// mu guards every field below. A plain Mutex rather than an RWMutex: every
	// exported entry point sweeps first, and sweeping mutates.
	mu sync.Mutex

	busID          string
	durable        DurableLog
	log            *logging.Logger
	now            func() time.Time
	maxInvites     int
	defaultTTL     time.Duration
	maxTTL         time.Duration
	spentRetention time.Duration
	reservationTTL time.Duration

	// invites is the table, keyed by invite id.
	invites map[string]Record

	// inflight holds at most ONE lifecycle transition per invite: a redemption
	// between Begin and Commit/Abort, or a revocation across its durable write.
	// It is what makes concurrent double-redemption impossible rather than
	// merely unlikely.
	inflight map[string]*transition

	// pendingMints counts mints that have passed the capacity check but whose
	// record is not yet in the table (they are mid-fsync). It is counted against
	// the cap so that N concurrent mints cannot all pass a check that only one
	// of them had room for, and it guarantees the post-write fold always has a
	// slot — a record that is already durable must never be refused by the
	// in-memory bound.
	pendingMints int

	// dummyDigest is compared against when an invite id is UNKNOWN, so the
	// hash-and-compare work Begin does is identical either way and the id's
	// existence is not handed over by timing. It is per-store crypto/rand
	// output; the result of that comparison is discarded, so even a preimage of
	// it would grant nothing.
	dummyDigest [DigestSize]byte
}

// transition is one in-flight lifecycle change for a single invite.
type transition struct {
	// what names the operation, for the refusal message and the reap log.
	what string
	// since is when it was taken, the input to the ReservationTTL sweep.
	since time.Time
	// consumed marks that a DURABLE RECORD HAS BEEN BUILT for this transition.
	// From that moment the reservation is NO LONGER SWEEPABLE: the caller may
	// already have committed it, and reaping it back to open would admit a
	// second redemption while the durable log says the invite is spent. After
	// consumption only Commit or Abort resolves it, and an abandoned one stays
	// locked until restart — which is fail-closed.
	consumed bool
	// done marks it resolved, so a second Commit or Abort is a no-op rather
	// than a release of somebody else's reservation.
	done bool
}

// NewStore builds an empty invite table.
//
// It returns an error only if crypto/rand fails while minting the per-store
// dummy digest. That is deliberately fatal rather than silently downgraded to a
// zero digest: there is no weaker fallback anywhere else in this package's
// randomness, and one here would be the kind of quiet exception that later gets
// copied to a place where it matters.
func NewStore(o StoreOptions) (*Store, error) {
	s := &Store{
		busID:          o.BusID,
		durable:        o.Durable,
		log:            o.Logger,
		now:            o.Now,
		maxInvites:     o.MaxInvites,
		defaultTTL:     o.DefaultTTL,
		maxTTL:         o.MaxTTL,
		spentRetention: o.SpentRetention,
		reservationTTL: o.ReservationTTL,
		invites:        make(map[string]Record),
		inflight:       make(map[string]*transition),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxInvites <= 0 {
		s.maxInvites = MaxInvites
	}
	if s.defaultTTL <= 0 {
		s.defaultTTL = DefaultTTL
	}
	if s.maxTTL <= 0 {
		s.maxTTL = MaxTTL
	}
	if s.spentRetention <= 0 {
		s.spentRetention = SpentRetention
	}
	if s.reservationTTL <= 0 {
		s.reservationTTL = ReservationTTL
	}
	if _, err := rand.Read(s.dummyDigest[:]); err != nil {
		return nil, fmt.Errorf("invite: reading %d bytes from crypto/rand for the unknown-invite dummy digest: %w", DigestSize, err)
	}
	return s, nil
}

// Attach binds the store to the durable log it writes through. It must be
// called EXACTLY ONCE, after wal.Open has returned.
//
// It exists for the same chicken-and-egg reason auth.WALRoster.Attach does:
// wal.Open needs the APPLIER before the *wal.Log exists (replay runs inside Open
// and hands every committed entry to the applier before Open returns), so this
// store must be constructible first and given its log afterwards. The ordering
// is three steps and is not optional:
//
//	s, err := invite.NewStore(invite.StoreOptions{BusID: id, Logger: lg})  // 1. applier first
//	log, err := wal.Open(...)                                             // 2. replay fills s
//	err = s.Attach(log)                                                   // 3. now it can write
//
// Between steps 1 and 3 the table can be READ and REBUILT but not WRITTEN:
// every mutating method returns ErrNotDurable. That is the correct order —
// recovery must finish before the first live mint or redemption.
//
// A nil log is an ERROR rather than a silent no-op: it would leave the store in
// the exact false-durability state ErrNotDurable exists to refuse. A SECOND call
// is an ERROR and changes nothing, for the reason WALRoster.Attach gives: two
// logs mean two distinct durable histories behind one in-memory table, and
// whichever won the race would silently own the redemptions the other had
// already acknowledged.
func (s *Store) Attach(d DurableLog) error {
	if d == nil {
		return fmt.Errorf("invite: attaching the durable log: it must not be nil; a store with no log would acknowledge mints and redemptions that never reached disk, and single use held only in memory is decorative (%v)", ErrNotDurable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.durable != nil {
		return errors.New("invite: attaching the durable log: already attached; a store is bound to exactly one durable log, and a second would give one in-memory invite table two durable histories")
	}
	s.durable = d
	return nil
}

// durableLog reads the attached log under the lock.
//
// It is a method rather than a bare field read because Attach can now WRITE
// s.durable after construction: an unsynchronised read from Mint, Revoke or
// Redeem would be a data race with it (and a race here is a P0 — concurrency is
// the product). Every one of those methods captures the value ONCE and uses the
// captured local for both its nil check and its later Write, so it cannot check
// one log and write to another.
func (s *Store) durableLog() DurableLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.durable
}

// Len reports how many invite records the table currently retains, after a
// sweep. It is the count a startup line uses to prove the table was rebuilt by
// replay.
//
// It is a RETENTION count, not a count of usable invites: a retired-but-retained
// record (see retiredAt) is refused exactly as hard as a dropped one.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	return len(s.invites)
}

// ---------------------------------------------------------------------------
// Mint
// ---------------------------------------------------------------------------

// MintRequest is an operator's request for a new invite.
type MintRequest struct {
	// Label is an optional operator note, at most MaxLabelLen bytes. It is
	// never echoed to a client.
	Label string

	// TTL is the requested lifetime. ZERO means "unset" and takes DefaultTTL —
	// the zero-value convention every options struct in this repo uses. A
	// NEGATIVE TTL is ErrInvalidTTL, and so is one over MaxTTL: an over-long
	// lifetime is REJECTED rather than silently clamped, because quietly
	// issuing a shorter-lived credential than the operator asked for is how an
	// invite mysteriously stops working.
	TTL time.Duration
}

// Minted is the result of a successful mint: the durable record and the
// plaintext secret.
type Minted struct {
	// Record is the invite as it was written to disk.
	Record

	// Secret is the PLAINTEXT BEARER CREDENTIAL, and this is the ONLY time it
	// exists outside the caller's hands. It is not stored, not logged and not in
	// the WAL — only HashSecret's digest is durable — so a lost secret cannot be
	// recovered, only replaced by minting a fresh invite.
	//
	// Whoever holds it can enrol an agent onto this bus (DECISIONS.md, E6: the
	// invite blob is the TRUST ANCHOR). Treat it with session-token discipline:
	// never log it, never include it in an error, never write it to a
	// world-readable file.
	Secret string
}

// String renders the minted invite with the SECRET REDACTED, so the one value
// in this package that must never be logged cannot be logged by accident.
//
// This matters more here than anywhere else: Minted embeds a Record, which
// makes it exactly the shape someone reaches for when they want to log or
// serialise "the invite that was just created" — and doing that wholesale would
// publish a live bearer credential. Callers that need the plaintext must read
// the field deliberately, which is the point.
func (m Minted) String() string {
	return fmt.Sprintf("invite.Minted{ID:%s Bus:%s State:%s ExpiresAt:%s Secret:[REDACTED %d bytes]}",
		m.ID, m.BusID, m.State, m.ExpiresAt.UTC().Format(time.RFC3339Nano), len(m.Secret))
}

// GoString is String, for the "%#v" verb.
func (m Minted) GoString() string { return m.String() }

// Mint issues a new invite and returns it with its one-time secret.
//
// Nothing is returned until the record is DURABLE (invariant 4): the capacity
// check refuses BEFORE anything is generated or written, and the record is
// folded into memory only after Durable.Write has fsynced both phases.
func (s *Store) Mint(req MintRequest) (Minted, error) {
	// Captured ONCE, under the lock, and used for both the check and the Write
	// below: Attach may set it concurrently. See durableLog.
	durable := s.durableLog()
	if durable == nil {
		return Minted{}, fmt.Errorf("%w: refusing to mint an invite whose single-use state would be lost on restart", ErrNotDurable)
	}
	if s.busID == "" {
		return Minted{}, fmt.Errorf("%w: this invite store was built without a bus id, so it cannot say which bus an invite admits to", ErrInvalidRecord)
	}
	if len(req.Label) > MaxLabelLen {
		return Minted{}, fmt.Errorf("%w: label is %d bytes, but a label is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(req.Label), MaxLabelLen)
	}
	ttl := req.TTL
	switch {
	case ttl == 0:
		ttl = s.defaultTTL
	case ttl < 0:
		return Minted{}, fmt.Errorf("%w: a lifetime of %s is negative; omit it entirely for the default of %s", ErrInvalidTTL, ttl, s.defaultTTL)
	case ttl > s.maxTTL:
		return Minted{}, fmt.Errorf("%w: %s exceeds the maximum invite lifetime of %s; it is REFUSED rather than clamped, so that an invite never silently outlives or under-lives what was asked for", ErrInvalidTTL, ttl, s.maxTTL)
	}

	id, err := GenerateInviteID()
	if err != nil {
		return Minted{}, err
	}
	secret, err := GenerateSecret()
	if err != nil {
		return Minted{}, err
	}

	// The capacity slot is RESERVED here and released after the fold, so that
	// concurrent mints cannot all pass a check only one of them had room for.
	now := s.now().UTC()
	s.mu.Lock()
	s.sweepLocked(now)
	if len(s.invites)+s.pendingMints >= s.maxInvites {
		// Both counts are read UNDER the lock and only then formatted: reading
		// s.pendingMints after the unlock would be a data race on the field this
		// whole reservation exists to make safe.
		held, pending := len(s.invites), s.pendingMints
		s.mu.Unlock()
		return Minted{}, fmt.Errorf("%w: %d invites are retained and %d more are being written, against a limit of %d; nothing is evicted to make room, because an evicted invite is an unknown one and an operator's live invite would silently stop working", ErrCapacity, held, pending, s.maxInvites)
	}
	if _, clash := s.invites[id]; clash {
		// Unreachable in practice (inviteIDRandBytes is 80 bits of crypto/rand)
		// and refused rather than assumed away: an id collision would REPLACE a
		// live invite's secret digest, which is the one edit this table must
		// never make.
		s.mu.Unlock()
		return Minted{}, fmt.Errorf("%w: generated invite id %q is already in use", ErrInvalidInviteID, id)
	}
	s.pendingMints++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.pendingMints--
		s.mu.Unlock()
	}()

	rec, body, err := canonical(Record{
		ID:           id,
		BusID:        s.busID,
		SecretDigest: HashSecret(secret),
		Label:        req.Label,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
		State:        StateOpen,
	})
	if err != nil {
		return Minted{}, err
	}
	if _, err := durable.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		// NOTHING was acknowledged and nothing is in memory. The secret dies
		// here, unlogged.
		return Minted{}, fmt.Errorf("invite: recording invite %s durably: %w", id, err)
	}
	s.foldIn(rec, "mint")
	return Minted{Record: rec, Secret: secret}, nil
}

// ---------------------------------------------------------------------------
// Lookup
// ---------------------------------------------------------------------------

// Lookup returns the invite with this id, if it is still retained.
//
// IT IS NOT A REDEMPTION PATH and must never be used as one: it takes no
// secret, performs no constant-time comparison and reports the invite to
// anybody who can name it. Its callers are operator tooling (INVITE-REVOKE's
// listing, an inspect endpoint) and tests. Redemption goes through Begin.
//
// The returned record is a deep copy, so a caller cannot reach into the stored
// record through its Result slice.
func (s *Store) Lookup(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	r, ok := s.invites[id]
	if !ok {
		return Record{}, false
	}
	return copyRecord(r), true
}

// ---------------------------------------------------------------------------
// Revoke
// ---------------------------------------------------------------------------

// Revoke withdraws an unredeemed invite, durably.
//
// Already-REVOKED is IDEMPOTENT: the existing record is returned and NOTHING is
// written. Already-REDEEMED is ErrAlreadyRedeemed and nothing is written —
// revocation does NOT reach an agent that already redeemed (DECISIONS.md, E5),
// and reporting success would give an operator a false expectation of reach at
// exactly the moment they are trying to shut something down. Cascading
// revocation of an enrolled agent's credential is AUTH-4.
//
// An EXPIRED but open invite is still revocable, deliberately: an operator
// revoking an invite is recording a decision, and refusing because the clock
// already handled it would leave no durable trace of that decision.
func (s *Store) Revoke(id, reason string) (Record, error) {
	if err := ValidateInviteID(id); err != nil {
		return Record{}, err
	}
	if len(reason) > MaxReasonLen {
		return Record{}, fmt.Errorf("%w: reason is %d bytes, but a reason is at most %d; it is not echoed here because it is oversized", ErrInvalidRecord, len(reason), MaxReasonLen)
	}
	// Captured ONCE, under the lock; see durableLog.
	durable := s.durableLog()
	if durable == nil {
		return Record{}, fmt.Errorf("%w: refusing to revoke, because a revocation held only in memory is undone by the next restart", ErrNotDurable)
	}

	now := s.now().UTC()
	s.mu.Lock()
	s.sweepLocked(now)
	cur, ok := s.invites[id]
	if !ok {
		s.mu.Unlock()
		return Record{}, fmt.Errorf("%w: %s", ErrUnknownInvite, id)
	}
	switch cur.State {
	case StateRevoked:
		s.mu.Unlock()
		return copyRecord(cur), nil
	case StateRedeemed:
		s.mu.Unlock()
		return Record{}, fmt.Errorf("%w: invite %s was redeemed at %s by %s; revoking it now would NOT reach that agent, so this is refused rather than reported as a revocation that took effect (DECISIONS.md E5 — cascading revocation of an enrolled agent is AUTH-4)",
			ErrAlreadyRedeemed, id, cur.RedeemedAt.UTC().Format(time.RFC3339Nano), cur.RedeemedBy)
	}
	// The transition is reserved for the same reason a redemption is: two
	// concurrent revocations would each write a durable record, and a revocation
	// racing a redemption would let both believe they had the invite.
	t, err := s.beginTransitionLocked(id, "revocation", now)
	if err != nil {
		s.mu.Unlock()
		return Record{}, err
	}
	s.mu.Unlock()

	// keepHold suppresses the release below when the durable record may already
	// exist. See the ErrDiverged branch.
	keepHold := false
	defer func() {
		if keepHold {
			return
		}
		s.mu.Lock()
		s.releaseLocked(id, t)
		s.mu.Unlock()
	}()

	next := cur
	next.State = StateRevoked
	next.RevokedAt = now
	next.RevokedReason = reason
	rec, body, err := canonical(next)
	if err != nil {
		return Record{}, err
	}

	// Marked BEFORE the write, exactly as Redemption.Consume marks its own
	// reservation: from here a durable record may exist, so the TTL sweep must
	// not reap this hold and hand the invite to somebody else while the log
	// says it is revoked. Only this function resolves it.
	s.mu.Lock()
	t.consumed = true
	s.mu.Unlock()

	if _, err := durable.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		// THE SAME RULE Store.Redeem FOLLOWS, and it must be applied here too:
		// wal.Txn.Commit returns ErrDiverged AFTER the commit record is fsynced,
		// so the revocation is durably recorded and only a neighbouring
		// applier failed. Releasing the hold there would leave memory saying
		// OPEN while disk says REVOKED — and a revoked invite that memory still
		// thinks is open is redeemable, which is the outcome revocation exists
		// to prevent. Keeping the hold is fail-closed: the invite is frozen
		// until a restart rebuilds the table from the durable log.
		//
		// That the poisoned log would refuse a subsequent write is a
		// NEIGHBOURING mechanism's doing, not this package's, and correctness
		// here must not rest on it.
		if errors.Is(err, wal.ErrDiverged) {
			keepHold = true
		}
		return Record{}, fmt.Errorf("invite: recording the revocation of %s durably: %w", id, err)
	}
	s.foldIn(rec, "revoke")
	return rec, nil
}

// ---------------------------------------------------------------------------
// Redemption — the two-phase participant
// ---------------------------------------------------------------------------

// Outcome is what Begin found.
type Outcome int

const (
	// OutcomeReserved is a FRESH redemption. The invite is now reserved and the
	// caller MUST resolve it: Consume then Commit once its own durable write
	// has committed, or Abort.
	OutcomeReserved Outcome = iota + 1

	// OutcomeReplay is a LEGITIMATE RETRY — same key, same payload fingerprint,
	// against an invite this key already redeemed. NOTHING is reserved and
	// nothing may be applied: return Result verbatim. Do not error, and do NOT
	// disconnect the client; punishing a correct retry breaks exactly the
	// clients doing the right thing (invariant 10).
	OutcomeReplay
)

func (o Outcome) String() string {
	switch o {
	case OutcomeReserved:
		return "reserved"
	case OutcomeReplay:
		return "replay"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// RedeemRequest is one attempt to spend an invite.
type RedeemRequest struct {
	// InviteID is the invite being redeemed. Untrusted input.
	InviteID string

	// Secret is the presented bearer secret. It is compared in constant time
	// against the stored digest and is NEVER logged, echoed or stored.
	//
	// Begin DROPS it the moment it has been verified — the RedeemRequest a
	// Redemption keeps carries an empty Secret — so an in-flight redemption
	// holds no live credential for a heap dump, a panic or a stray %+v to
	// print. See this type's String method, which redacts it in the window
	// before that.
	Secret string

	// Key is the client idempotency key, scoped to THIS invite (doc.go).
	Key string

	// Fingerprint is the request payload's fingerprint
	// (idem.ComputeFingerprint). Comparing it is what separates a legitimate
	// retry from a key reused for different content. The caller defines and
	// documents the exact field list it hashes.
	Fingerprint idem.Fingerprint
}

// String renders the request with the SECRET REDACTED.
//
// It exists because the alternative is silent: a RedeemRequest is exactly the
// kind of value that ends up in a "%+v" during debugging, in a panic message,
// or in a structured log line added months from now by someone who did not read
// the field comment above. Go reaches for String automatically in every one of
// those places, so redacting here is the only measure that holds without
// everyone remembering. GoString covers "%#v" for the same reason.
func (r RedeemRequest) String() string {
	return fmt.Sprintf("invite.RedeemRequest{InviteID:%s Secret:[REDACTED %d bytes] Key:%s Fingerprint:%x}",
		r.InviteID, len(r.Secret), r.Key, r.Fingerprint[:4])
}

// GoString is String, for the "%#v" verb.
func (r RedeemRequest) GoString() string { return r.String() }

// Result is what a fresh redemption produced: the identity it minted and the
// response to replay to a later retry.
type Result struct {
	// AgentID is the fully-qualified "<bus-id>.<agent-id>" the redemption
	// created. It is the provenance stored on the invite.
	AgentID string

	// Response is the minted result, verbatim, as the route will return it. It
	// is opaque here and capped at idem.MaxResultBytes.
	Response json.RawMessage

	// CertFingerprint is sha256.Sum256(cert.Raw) of the client certificate, or
	// the zero value for "none bound". RESERVED — MTLS-BIND populates it; see
	// Record.CertFingerprint.
	CertFingerprint [DigestSize]byte
}

// Redemption is one in-flight redemption: a PARTICIPANT in the caller's
// transaction, not a one-shot.
//
// # Why a participant
//
// Redemption must be ATOMIC with the effect it authorises — the roster write
// that creates the agent. A wal.Entry is exactly one transaction, so "atomic"
// means the consumption record and the roster record ride in the SAME entry.
// THAT ENTRY IS COMPOSED BY auth.Service.Enrol + auth.WALRoster.PutWithInvite
// (kind auth.EnrolInviteRecordKind, INVITE-GATE 2026-08-14); this package
// cannot and must not compose it, which is why the flow below hands the caller
// a body and waits.
//
// Note what INVITE-GATE did and did not do: an invite PRESENTED at enrolment is
// now genuinely redeemed, and enrolment is still NOT gated — an enrolment
// carrying no invite is accepted.
//
// The flow:
//
//	r, err := store.Begin(req)          // validates + reserves
//	if r.Outcome() == OutcomeReplay {   // legitimate retry
//	    return r.Result()               // the ORIGINAL result, verbatim
//	}
//	body, err := r.Consume(result)      // the durable consumption record
//	entry := wal.Entry{...roster record + body...}
//	_, err = log.Write(entry)           // ONE transaction, two fsyncs
//	r.Commit()                          // fold into memory
//
// and on ANY failure between Begin and the durable commit: r.Abort().
//
// # Crash safety of that sequence
//
//   - Crash BEFORE the caller's WAL commit: nothing durable, the invite is
//     still open. Correct — nothing was acknowledged.
//   - Crash AFTER it: the consumption record is on stable storage, replay marks
//     the invite spent. Correct — the enrolment is durable and the invite is
//     not redeemable a second time.
//
// There is no window in between, because the two records are one transaction.
type Redemption struct {
	s   *Store
	req RedeemRequest

	// invite is the snapshot Begin took, under the lock.
	invite Record

	outcome Outcome

	// t is the reservation. nil for OutcomeReplay, which reserves nothing.
	t *transition

	// consumed is the record Consume built, folded in by Commit.
	consumed *Record
}

// Begin validates a redemption attempt and, if it is fresh, RESERVES the
// invite.
//
// The order of checks is load-bearing. The SECRET IS VERIFIED FIRST, before any
// state is reported, so a caller holding no secret learns nothing about whether
// an id exists or what state it is in. An unknown id is compared against a
// per-store dummy digest so the work is identical either way.
//
// The triage, per invariant 10 (these must not be collapsed):
//
//	correct secret, same key, same fingerprint      -> OutcomeReplay: the ORIGINAL result
//	correct secret, same key, DIFFERENT fingerprint -> ErrKeyReuse: reject and log, CONNECTION KEPT
//	correct secret, different key                   -> ErrAlreadyRedeemed: single use is spent
//	wrong secret or unknown id                      -> ErrUnknownInvite
//
// THE CONNECTION IS KEPT on ErrKeyReuse (invariant 10, NARROWED 2026-08-08):
// same key + different payload is REJECTED AND LOGGED, and only replay of an
// already-accepted SIGNED MESSAGE disconnects. See ErrKeyReuse for the argument,
// which is at its strongest on the enrolment route.
//
// While a reservation is held, any other Begin on that invite returns
// ErrRedemptionInFlight — INCLUDING one presenting the same key. See that
// error for why refusing a retry that races its own original is what makes
// concurrent double-redemption impossible.
func (s *Store) Begin(req RedeemRequest) (*Redemption, error) {
	if err := ValidateInviteID(req.InviteID); err != nil {
		return nil, err
	}
	if err := idem.ValidateKey(req.Key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if req.Secret == "" || len(req.Secret) > MaxSecretLen {
		// Reported as ErrUnknownInvite, never as a distinct "malformed secret":
		// a secret of an impossible length is not one this bus minted, and
		// saying so in a different way is one more bit an enumerator can read.
		// The secret itself is NEVER echoed, at any length.
		return nil, fmt.Errorf("%w: the presented secret is %d bytes, which is not a secret this bus minted", ErrUnknownInvite, len(req.Secret))
	}

	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)

	cur, known := s.invites[req.InviteID]
	digest := s.dummyDigest
	if known {
		digest = cur.SecretDigest
	}
	// ALWAYS compared, known or not, so the timing of the two paths matches.
	// The result for an unknown id is discarded: even a preimage of the dummy
	// digest grants nothing.
	if !VerifySecret(req.Secret, digest) || !known {
		return nil, fmt.Errorf("%w: %s", ErrUnknownInvite, req.InviteID)
	}

	// From here the caller has PROVED it holds the invite's secret, so it is
	// entitled to know the invite's state.
	if t, held := s.inflight[req.InviteID]; held {
		return nil, fmt.Errorf("%w: invite %s is held by a %s that began %s ago", ErrRedemptionInFlight, req.InviteID, t.what, now.Sub(t.since).Round(time.Millisecond))
	}

	switch cur.State {
	case StateRevoked:
		return nil, fmt.Errorf("%w: invite %s was revoked at %s", ErrRevoked, req.InviteID, cur.RevokedAt.UTC().Format(time.RFC3339Nano))
	case StateRedeemed:
		// Expiry is deliberately NOT consulted here: the retry horizon
		// (SpentRetention) is longer than an invite's lifetime, so a legitimate
		// retry routinely arrives after ExpiresAt has passed. Expiry gates a
		// FRESH redemption only.
		// Both comparisons are CONSTANT TIME. Neither is the credential check —
		// the secret already proved that — but both decide whether a caller is
		// handed the ORIGINAL RESULT, which for enrolment is an agent identity.
		// A byte-at-a-time compare would let a holder of the secret recover the
		// original key and fingerprint by timing, and constant time costs
		// nothing here. The agent id and time are still NOT echoed to the wire:
		// errors.go requires the HTTP layer to collapse these.
		if subtle.ConstantTimeCompare([]byte(cur.RedeemKey), []byte(req.Key)) != 1 {
			return nil, fmt.Errorf("%w: invite %s was redeemed at %s by %s under a different idempotency key",
				ErrAlreadyRedeemed, req.InviteID, cur.RedeemedAt.UTC().Format(time.RFC3339Nano), cur.RedeemedBy)
		}
		if subtle.ConstantTimeCompare(cur.RedeemFingerprint[:], req.Fingerprint[:]) != 1 {
			// Neither payload is echoed: one is attacker-chosen and the other
			// belongs to the earlier request.
			return nil, fmt.Errorf("%w: idempotency key %q already redeemed invite %s with a DIFFERENT payload; this is a protocol violation, not a retry — it is REFUSED AND LOGGED, and the connection is KEPT (invariant 10, narrowed 2026-08-08)",
				ErrKeyReuse, req.Key, req.InviteID)
		}
		return &Redemption{s: s, req: withoutSecret(req), invite: copyRecord(cur), outcome: OutcomeReplay}, nil
	}

	if cur.Expired(now) {
		return nil, fmt.Errorf("%w: invite %s expired at %s", ErrExpired, req.InviteID, cur.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}

	t, err := s.beginTransitionLocked(req.InviteID, "redemption", now)
	if err != nil {
		return nil, err
	}
	return &Redemption{s: s, req: withoutSecret(req), invite: copyRecord(cur), outcome: OutcomeReserved, t: t}, nil
}

// withoutSecret is the copy a Redemption keeps: everything the later phases
// need (the id, the key, the fingerprint) and NOT the bearer secret.
//
// The secret's only job is finished the instant VerifySecret returns, so a
// Redemption that kept it would hold a live admission credential for the whole
// duration of the caller's durable write for no purpose at all. Dropping it
// bounds the window in which the plaintext exists to Begin's own stack frame.
func withoutSecret(r RedeemRequest) RedeemRequest {
	r.Secret = ""
	return r
}

// Outcome reports what Begin found.
func (r *Redemption) Outcome() Outcome { return r.outcome }

// Result is the ORIGINAL stored result of the earlier redemption. It is
// meaningful only for OutcomeReplay, where it is what the caller must return
// verbatim; for OutcomeReserved it is nil. The returned slice is a copy.
func (r *Redemption) Result() json.RawMessage {
	if r.outcome != OutcomeReplay {
		return nil
	}
	return append(json.RawMessage(nil), r.invite.Result...)
}

// Consume builds the DURABLE CONSUMPTION RECORD for this redemption and returns
// its encoded form, for the caller to place in the SAME wal.Entry as the effect
// the redemption authorises.
//
// It writes nothing itself. Nothing about this invite is durable until the
// CALLER's entry commits, and nothing is in memory until Commit.
//
// AFTER A SUCCESSFUL Consume THE RESERVATION IS NO LONGER SWEEPABLE. From this
// point the caller may already have committed the record, so reaping the
// reservation back to open would admit a second redemption while the log says
// the invite is spent. Only Commit or Abort resolves it now.
func (r *Redemption) Consume(res Result) (json.RawMessage, error) {
	if r.outcome != OutcomeReserved {
		return nil, fmt.Errorf("invite: Consume on a %s redemption of %s: only a reserved redemption may be consumed; a replay must return Result() and apply nothing", r.outcome, r.req.InviteID)
	}
	if res.AgentID == "" {
		return nil, fmt.Errorf("%w: a redemption must name the agent id it created", ErrInvalidRecord)
	}

	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.t.done {
		// Either the caller already resolved this redemption, or the sweep
		// reaped it for being held past ReservationTTL without a durable record.
		// Both are refusals: the reservation is gone, so this redemption no
		// longer holds the invite and must not build a record as if it did.
		return nil, fmt.Errorf("invite: Consume on the redemption of %s: the reservation is already resolved — committed, aborted, or reaped after being held longer than %s", r.req.InviteID, r.s.reservationTTL)
	}
	if r.t.consumed {
		return nil, fmt.Errorf("invite: Consume on the redemption of %s: it has already been consumed; a second consumption record would be a second redemption", r.req.InviteID)
	}

	next := r.invite
	next.State = StateRedeemed
	next.RedeemedAt = r.s.now().UTC()
	next.RedeemedBy = res.AgentID
	next.RedeemKey = r.req.Key
	next.RedeemFingerprint = r.req.Fingerprint
	next.Result = res.Response
	next.CertFingerprint = res.CertFingerprint
	rec, body, err := canonical(next)
	if err != nil {
		// The reservation is deliberately left UNconsumed: no durable record
		// exists, so the caller may still Abort it and the TTL sweep may still
		// reap it. Failing here must not strand the invite.
		return nil, err
	}
	r.t.consumed = true
	r.consumed = &rec
	return body, nil
}

// Commit folds the consumption into memory. It is called AFTER the caller's own
// durable write has committed, and never before.
//
// It is idempotent, and on a replay it is a no-op (a replay reserved nothing).
//
// It returns an error only for MISUSE — commit without consume. A refused
// UPSERT is logged at ERROR and does NOT fail the call: by this point the
// consumption record is already durable, so the durable log is the truth and
// telling the caller its committed enrolment failed would be worse than the
// discrepancy. The operator gets a specific line and a restart rebuilds memory
// from disk.
func (r *Redemption) Commit() error {
	if r.outcome == OutcomeReplay {
		return nil
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.t.done {
		return nil
	}
	if !r.t.consumed || r.consumed == nil {
		return fmt.Errorf("invite: Commit on the redemption of %s: Consume was never called, so there is no durable record to fold in", r.req.InviteID)
	}
	// The upsert runs BEFORE the release, so the sweep inside it still sees this
	// invite as held and cannot drop the record the fold is about to replace.
	if err := r.s.upsertLocked(*r.consumed, "commit"); err != nil {
		r.s.log.Error("an invite consumption record is DURABLE but was refused by the in-memory table; the durable log is the truth and a restart will rebuild from it",
			"invite_id", r.req.InviteID, "err", err)
	}
	r.s.releaseLocked(r.req.InviteID, r.t)
	return nil
}

// Abort releases the reservation. The caller's durable write failed, or never
// happened.
//
// ABORT AFTER A SUCCESSFUL Commit IS A NO-OP, never an un-spend.
//
// CALL IT ONLY WHEN THE DURABLE WRITE DEMONSTRABLY DID NOT COMMIT. If the
// caller cannot tell — a write that returned an error after its commit record
// was fsynced — it must NOT abort: abandoning the Redemption leaves the invite
// locked until restart, which is fail-closed, whereas releasing it would admit
// a second redemption of an invite the log already says is spent.
func (r *Redemption) Abort() {
	if r.outcome == OutcomeReplay {
		return
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.t.done {
		return
	}
	r.s.releaseLocked(r.req.InviteID, r.t)
	r.s.log.Debug("invite redemption aborted; the reservation is released and the invite is open again",
		"invite_id", r.req.InviteID, "consumed", r.t.consumed)
}

// Redeem is the STANDALONE redemption path: Begin, Consume, write its OWN
// invite entry, Commit.
//
// USE IT ONLY WHEN THE REDEMPTION HAS NO OTHER DURABLE EFFECT TO BE ATOMIC
// WITH. INVITE-GATE MUST NOT USE IT: enrolment's roster write has to share the
// transaction with the consumption record, and this function writes a
// transaction of its own. Splitting them reopens exactly the window the
// participant API exists to close — a crash between the two leaves an agent
// enrolled against an invite that is still open, or an invite spent on an
// enrolment that never happened.
//
// It exists so this package is complete and provable on its own, today, before
// anything composes it.
func (s *Store) Redeem(req RedeemRequest, res Result) (Record, error) {
	// Captured ONCE, under the lock; see durableLog.
	durable := s.durableLog()
	if durable == nil {
		return Record{}, fmt.Errorf("%w: refusing to redeem, because a spent invite remembered only in memory is redeemable again after the next restart", ErrNotDurable)
	}
	r, err := s.Begin(req)
	if err != nil {
		return Record{}, err
	}
	if r.Outcome() == OutcomeReplay {
		return copyRecord(r.invite), nil
	}
	body, err := r.Consume(res)
	if err != nil {
		r.Abort()
		return Record{}, err
	}
	if _, err := durable.Write(wal.Entry{Kind: RecordKind, Body: body}); err != nil {
		// ABORT ONLY WHEN THE WRITE DEMONSTRABLY DID NOT COMMIT.
		//
		// wal.Txn.Commit returns ErrDiverged AFTER the commit record has been
		// appended and fsynced (internal/wal/log.go): the failure is a
		// neighbouring applier's, not the write's, and the invite is by then
		// durably SPENT. Releasing the reservation there would leave memory
		// saying OPEN while disk says REDEEMED, and the next Begin would admit a
		// SECOND redemption of a spent invite — the one outcome this package
		// exists to prevent. That the poisoned log would refuse the second
		// durable write is a neighbouring mechanism's doing, not this one's, and
		// single use must not rest on it.
		//
		// So on ErrDiverged the Redemption is ABANDONED, still holding the
		// invite. That is fail-closed: the invite stays locked until a restart
		// rebuilds the table from the durable log, which records it as spent.
		// INVITE-GATE must inherit this rule verbatim when it composes its own
		// entry — see Redemption.Abort.
		if !errors.Is(err, wal.ErrDiverged) {
			r.Abort()
		}
		return Record{}, fmt.Errorf("invite: recording the redemption of %s durably: %w", req.InviteID, err)
	}
	if err := r.Commit(); err != nil {
		return Record{}, err
	}
	return *r.consumed, nil
}

// ---------------------------------------------------------------------------
// Replay
// ---------------------------------------------------------------------------

// Apply implements wal.Applier: it folds one committed invite entry into the
// serving copy, on replay at Open and — if the server wires this store as the
// log's Applier — on live commits too. It cannot tell the two apart, and must
// not need to: the record is complete in both cases.
//
// Entries of any other Kind are skipped SILENTLY: this log carries messages,
// roster entries and invites, and a store that treated its neighbours' records
// as damage would fill the log with false alarms.
//
// # APPLY MUST NEVER RETURN A NON-NIL ERROR
//
// From a live write a non-nil error POISONS the log (wal.ErrDiverged); from
// recovery it makes Open fail, and invariant 6 settled that recovery ALWAYS
// reaches a running server. So every failure path here — an undecodable record,
// an invalid one, the capacity bound, a non-monotonic transition — LOGS LOUDLY
// AND SPECIFICALLY at ERROR and returns nil, exactly as hub.Apply does. SILENT
// discard is the defect, not discard itself: each line names the invite, the
// prepare/commit index and the reason.
//
// A discard here is FAIL-CLOSED: the invite becomes UNKNOWN, and an unknown
// invite is rejected. It can cost availability (a live invite stops working)
// and the ability to answer a retry with its original result. It can never
// produce a second redemption.
func (s *Store) Apply(c wal.Committed) error {
	if c.Entry.Kind != RecordKind {
		return nil
	}
	rec, err := DecodeRecord(c.Entry.Body)
	if err != nil {
		s.log.Error("DISCARDING an invite record that could not be decoded; the invite is therefore UNKNOWN and cannot be redeemed",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex, "err", err)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.upsertLocked(rec, "replay"); err != nil {
		s.log.Error("DISCARDING an invite record that could not be applied; the invite keeps the state already in memory",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
			"invite_id", rec.ID, "state", rec.State.String(), "err", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The table itself
// ---------------------------------------------------------------------------

// foldIn applies a record produced by a LIVE, already-durable operation.
//
// A refusal is logged at ERROR and swallowed for the same reason
// Redemption.Commit swallows one: the record is on stable storage, so the
// durable log is the truth and a restart rebuilds memory from it. Returning an
// error would tell the caller an operation failed that demonstrably did not.
func (s *Store) foldIn(r Record, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.upsertLocked(r, source); err != nil {
		s.log.Error("an invite record is DURABLE but was refused by the in-memory table; the durable log is the truth and a restart will rebuild from it",
			"invite_id", r.ID, "state", r.State.String(), "source", source, "err", err)
	}
}

// upsertLocked is the ONE place anything enters or changes in the table, and it
// is MONOTONIC.
//
// Allowed: an insert; a re-apply of the same record; open -> redeemed;
// open -> revoked. Refused: everything else, in particular anything that would
// move an invite BACKWARDS out of a terminal state, and any record that claims
// an existing invite's id with a different secret digest or a different bus.
//
// The caller must hold mu.
func (s *Store) upsertLocked(r Record, source string) error {
	s.sweepLocked(s.now())

	existing, ok := s.invites[r.ID]
	if !ok {
		// The cap is enforced on EVERY path, including replay: it is a MEMORY
		// bound, and a bound one path could exceed is not a bound. A live mint
		// cannot be refused here because Mint reserved its slot in pendingMints
		// before writing.
		if len(s.invites) >= s.maxInvites {
			return fmt.Errorf("%w: %d invites are retained, the limit; this record is DISCARDED, which makes the invite UNKNOWN and therefore unredeemable", ErrCapacity, s.maxInvites)
		}
		s.invites[r.ID] = copyRecord(r)
		return nil
	}

	// An invite's identity NEVER changes. A record that shares an id but not
	// these is corruption, an id collision, or an attempt to rebind an invite to
	// a different bus or a different secret — the one edit this table must never
	// make.
	if existing.SecretDigest != r.SecretDigest {
		return fmt.Errorf("%w: the record carries a different secret digest for invite %s, so it is not the same invite", ErrInvalidRecord, r.ID)
	}
	if existing.BusID != r.BusID {
		return fmt.Errorf("%w: the record binds invite %s to a different bus", ErrInvalidRecord, r.ID)
	}

	switch {
	case existing.State == StateOpen && r.State == StateOpen:
		// A re-applied mint: the same log replayed twice, or a live fold that
		// Apply already performed. Idempotent.
		if !existing.CreatedAt.Equal(r.CreatedAt) {
			return fmt.Errorf("%w: two different open records claim invite %s", ErrInvalidRecord, r.ID)
		}
		return nil

	case existing.State == StateOpen:
		// THE SPEND: open -> redeemed or open -> revoked. The only transition
		// this table performs.
		s.invites[r.ID] = copyRecord(r)
		return nil

	case r.State == existing.State:
		if sameEvent(existing, r) {
			return nil
		}
		if r.State == StateRedeemed {
			// A SECOND, DIFFERENT redemption record for an already-spent invite.
			// The first one wins and this is refused: whichever is dropped, the
			// invite stays spent, and keeping the FIRST keeps the record that
			// matches the result already returned to a client.
			return fmt.Errorf("%w: invite %s is already redeemed by %s at %s; a second, different redemption record is refused and the first is kept",
				ErrAlreadyRedeemed, r.ID, existing.RedeemedBy, existing.RedeemedAt.UTC().Format(time.RFC3339Nano))
		}
		// A redundant revocation: benign (both records say revoked), so it is
		// not an ERROR, but it is worth an operator seeing.
		s.log.Warn("a redundant invite revocation record; the first revocation is kept",
			"invite_id", r.ID, "source", source,
			"kept", existing.RevokedAt.UTC().Format(time.RFC3339Nano),
			"discarded", r.RevokedAt.UTC().Format(time.RFC3339Nano))
		return nil

	default:
		return fmt.Errorf("%w: refusing a NON-MONOTONIC transition %s -> %s for invite %s; a spent invite is never resurrected",
			ErrInvalidRecord, existing.State, r.State, r.ID)
	}
}

// sameEvent reports whether two records in the same terminal state describe the
// SAME event — the test for "this is the record I already have", used to keep a
// re-applied record silent instead of alarming.
func sameEvent(a, b Record) bool {
	switch a.State {
	case StateRedeemed:
		return a.RedeemKey == b.RedeemKey && a.RedeemedBy == b.RedeemedBy && a.RedeemedAt.Equal(b.RedeemedAt)
	case StateRevoked:
		return a.RevokedAt.Equal(b.RevokedAt)
	default:
		return false
	}
}

// sweepLocked drops records past their retention and reaps abandoned
// reservations. The caller must hold mu.
//
// The predicate is retiredAt, and it is a PURE PREDICATE re-derived identically
// on the live path and the replay path: a record is dropped once the event that
// ended its usefulness is more than SpentRetention old.
//
// It is called by every mutating entry point rather than by a background
// goroutine: the paths that grow the table are the paths that shrink it, the
// same discipline auth's sweepLocked uses. There is no ordered queue as in
// idem's table because there could not be a useful one — each invite carries
// its OWN TTL, so drop order is not insertion order, and the table is bounded
// at MaxInvites with an operator-rate arrival, which makes an O(n) scan of at
// most 8192 entries the simpler correct choice (invariant 8).
//
// EVERY DROP IS FAIL-CLOSED: the invite becomes UNKNOWN, and an unknown invite
// is REJECTED. See doc.go for why that is the opposite of idem's applied-key
// table, where forgetting a key fails OPEN.
func (s *Store) sweepLocked(now time.Time) {
	for id, r := range s.invites {
		if _, held := s.inflight[id]; held {
			// Never drop an invite with a transition in flight: the holder is
			// working from a snapshot of exactly this record, and dropping it
			// would let the fold that follows re-insert it as a fresh entry.
			continue
		}
		if !now.After(s.retiredAt(r)) {
			continue
		}
		delete(s.invites, id)
		s.log.Debug("dropping an invite record past its retention; the invite is now UNKNOWN and can never be redeemed",
			"invite_id", id, "state", r.State.String())
	}
	for id, t := range s.inflight {
		// ONLY an UNCONSUMED reservation is reapable. After Consume the caller
		// may already have committed a durable consumption record, so sweeping
		// it back to open would admit a second redemption of an invite the log
		// says is spent — a double redemption inside one process lifetime.
		if t.consumed || now.Sub(t.since) <= s.reservationTTL {
			continue
		}
		t.done = true
		delete(s.inflight, id)
		s.log.Warn("reaping an abandoned invite reservation; it was held past the reservation TTL without building a durable record",
			"invite_id", id, "what", t.what, "held", now.Sub(t.since).Round(time.Millisecond).String(), "ttl", s.reservationTTL.String())
	}
}

// retiredAt is when a record stops being retained: SpentRetention after the
// event that ended its usefulness.
//
//   - redeemed: RedeemedAt + SpentRetention — the legitimate-retry window.
//   - revoked:  the LATER of RevokedAt and ExpiresAt, + SpentRetention. The
//     later of the two because an invite may be revoked after it has already
//     expired, and because an operator asking "why did this fail" deserves
//     "revoked" for the invite's whole natural life, not just until the
//     revocation itself ages out.
//   - open:     ExpiresAt + SpentRetention — the diagnosis window that keeps
//     ErrExpired reachable at all. See SpentRetention.
//
// NOTHING here is a usability window: a retired-but-retained record is refused
// exactly as hard as a dropped one, and the ONLY thing it changes is which
// sentinel the refusal carries.
func (s *Store) retiredAt(r Record) time.Time {
	switch r.State {
	case StateRedeemed:
		return r.RedeemedAt.Add(s.spentRetention)
	case StateRevoked:
		end := r.RevokedAt
		if r.ExpiresAt.After(end) {
			end = r.ExpiresAt
		}
		return end.Add(s.spentRetention)
	default:
		return r.ExpiresAt.Add(s.spentRetention)
	}
}

// beginTransitionLocked reserves the one in-flight lifecycle transition an
// invite may have. The caller must hold mu and must have swept first (the sweep
// is what reaps an abandoned, unconsumed reservation).
func (s *Store) beginTransitionLocked(id, what string, now time.Time) (*transition, error) {
	if t, held := s.inflight[id]; held {
		return nil, fmt.Errorf("%w: invite %s is held by a %s that began %s ago", ErrRedemptionInFlight, id, t.what, now.Sub(t.since).Round(time.Millisecond))
	}
	t := &transition{what: what, since: now}
	s.inflight[id] = t
	return t, nil
}

// releaseLocked resolves a transition exactly once. The caller must hold mu.
//
// The identity check matters: a reservation the TTL sweep already reaped may
// have been replaced by a NEW one for the same invite, and a late Commit or
// Abort from the abandoned holder must not release somebody else's.
func (s *Store) releaseLocked(id string, t *transition) {
	if t.done {
		return
	}
	t.done = true
	if cur, ok := s.inflight[id]; ok && cur == t {
		delete(s.inflight, id)
	}
}

// canonical encodes a record and decodes it straight back.
//
// The round trip is the point, not a redundancy: the record folded into memory
// is then LITERALLY THE ONE REPLAY WILL PRODUCE from the same bytes — same
// compaction of Result, same UTC times, same normalisation — so a live Apply
// and a replayed Apply can never hold records that differ. It also proves,
// before anything is written, that the record this bus is about to make durable
// is one it can read back and will accept (Encode and DecodeRecord both
// validate).
func canonical(r Record) (Record, json.RawMessage, error) {
	body, err := r.Encode()
	if err != nil {
		return Record{}, nil, err
	}
	out, err := DecodeRecord(body)
	if err != nil {
		// Unreachable unless Encode and DecodeRecord disagree, which would be a
		// bug in this package rather than bad input. Surfaced rather than
		// ignored: it would otherwise appear later as a record that cannot be
		// replayed.
		return Record{}, nil, fmt.Errorf("invite: encoded record for %s does not decode back: %w", r.ID, err)
	}
	return out, body, nil
}
