package auth

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// OperatorRegistry is the DURABLE set of operator/admin principals (AUTH-10):
// every add and every revocation is recorded through internal/wal's two-phase
// write path, fsynced at prepare and again at commit, and only then applied to
// the in-memory serving copy (invariants 4 and 5). On restart the registry is
// rebuilt by replay, because OperatorRegistry is also the wal.Applier for its
// own record kind.
//
// It is deliberately a NEAR-MIRROR of WALRoster, down to the two mutexes and the
// fixed order inside put — and it is a separate type rather than a
// generalisation of it, because the two guard different kinds of principal and a
// shared implementation is one refactor away from letting a roster write produce
// an operator or vice versa. Read WALRoster's type doc for the construction
// order; it applies here unchanged:
//
//	r := auth.NewOperatorRegistry(logger)                            // 1. applier first
//	log, err := wal.Open(wal.LogOptions{Dir: dir, Applier: mux, ...}) // 2. replay fills r
//	if err := r.Attach(log); err != nil { ... }                       // 3. now it can write
//
// # THE SERVER REGISTERS THIS APPLIER (AUTH-10-WIRING, 2026-08-21)
//
// THIS BLOCK ASSERTED THE OPPOSITE, in the present tense, until AUTH-10-WIRING:
// "!! THE SERVER DOES NOT YET REGISTER THIS APPLIER — READ THIS BEFORE ASSUMING
// IT DOES !!", cmd/agent-bus/main.go "builds its applier map WITHOUT
// auth.OperatorRecordKind", an operator record "is currently PASSED OVER AT
// SERVER REPLAY WITHOUT A WORD". EVERY CLAUSE OF THAT IS NOW FALSE. It was true
// when it was written, and it is the silent discard invariant 6 rates as the
// defect.
//
// READ THE EVIDENCE CAREFULLY, because the obvious reading of it is wrong.
// Verified 2026-08-21 by running BOTH builds over one data directory holding two
// adds and one revoke: the PRE-WIRING binary logged
// `msg="wal replayed" records=6 applied=3 … discarded=0` and NOTHING ELSE about
// operators, and the WIRED binary logs THAT SAME LINE, same counters, plus
// `operators_recovered=2 live_operators=1`. So `applied=3` is NOT three records
// skipped — Replay.Applied counts entries DELIVERED to the applier (records=6 is
// three prepare+commit pairs), and the multiplexer then returned nil for a kind
// it did not own. That is what made the old state so dangerous: the replay line
// read perfectly healthy, `discarded=0` included, over records nothing had
// rebuilt. The ONLY signal is the operator line below and its two counts.
//
// main.go now takes all three steps in the order above: it builds the registry
// beside authRoster, registers auth.OperatorRecordKind in the applier map it
// hands wal.Open, and calls Attach once wal.Open has returned. It reports the
// outcome at INFO on every start — a log holding two adds and one revoke replays
// as `operators_recovered=2 live_operators=1` — and those counts reading 0 over a
// data directory that holds operator records is how this defect would be seen if
// it ever came back.
//
// `agent-bus operator add|list|revoke` was never affected either way: it opens
// the log with its own applier map (cmd/agent-bus/operator.go) and has always
// seen every record.
//
// The registration belongs IN main's applier map and must stay there: a shim in
// another cmd/agent-bus file that reached into main's wiring would put it where
// nobody reading the applier map would find it.
//
// # STILL TRUE, and a different claim entirely: nothing CONSUMES this principal
//
// auth.NewOperatorService has no non-test caller, no HTTP route authenticates an
// operator, and admitting or revoking one remains an OFFLINE action taken under
// the data directory's exclusive lock — so a revocation is in effect from the
// next start, NOT immediately on a running bus. The principal now EXISTS and
// REPLAYS; nothing USES it (AUTH-7, INVMINT and CONV-AUTHZ-ADMIN are the
// consumers, each unstarted).
//
// The zero value is not usable; construct with NewOperatorRegistry. It is safe
// for concurrent use.
type OperatorRegistry struct {
	log *logging.Logger

	// writeMu serialises the WHOLE check-then-write of Add and Revoke: the
	// duplicate checks, the encode and the durable Write, including the Apply
	// that Write performs at the end of Commit. It is NOT the map lock.
	//
	// Two mutexes rather than one for WALRoster's reason: Apply is called FROM
	// INSIDE Write — wal.Txn.Commit calls the applier after the commit fsync, on
	// the same goroutine — so a single mutex held across Write would deadlock
	// the moment Apply tried to take it.
	writeMu sync.Mutex

	// mu guards byID and sessions, and is held only for the duration of a map
	// or field operation. It is the lock Apply, Get, List and Len take.
	mu   sync.Mutex
	byID map[string]Operator

	// sessions is the live session table to drop sessions from when an operator
	// is revoked, or nil when none is attached. See AttachSessions and Revoke.
	sessions OperatorSessionRevoker

	// w is the durable log, set once by Attach. It is read and written under
	// writeMu, so no separate lock is needed.
	w DurableWriter
}

// OperatorSessionRevoker is the narrow slice of *OperatorService the registry
// calls when an operator is revoked.
//
// An INTERFACE and one method, for DurableWriter's reason: revocation needs
// exactly one thing from the session table — drop this principal's live sessions
// NOW — and a registry that could reach the rest of OperatorService could issue
// a session while revoking one.
type OperatorSessionRevoker interface {
	// RevokeSessions drops every live session belonging to operatorID and
	// reports how many were dropped.
	RevokeSessions(operatorID string) int
}

// NewOperatorRegistry returns a registry that is not yet attached to a durable
// log. It is ready to receive replayed entries through Apply; it cannot accept
// an Add or a Revoke until Attach has been called.
//
// logger may be nil (logging.Logger's methods are nil-safe), in which case
// discards are still refused-and-skipped but nobody hears about them — pass a
// logger in production, because "every discard is LOGGED" is the absolute half
// of invariant 6.
func NewOperatorRegistry(logger *logging.Logger) *OperatorRegistry {
	return &OperatorRegistry{log: logger, byID: make(map[string]Operator)}
}

// Attach binds the registry to the durable log it writes through. It must be
// called EXACTLY ONCE, after wal.Open has returned.
//
// A second call is an ERROR and changes nothing, and a nil writer is likewise an
// error rather than a silent no-op — WALRoster.Attach's two reasons, the second
// of them CORRECTED (AUTH-10-WIRING, 2026-08-21): two logs would mean two
// durable histories behind one in-memory registry, and a nil writer would spend
// the once-only Attach on nothing while leaving the registry exactly as
// unattached as it already was.
//
// THE SECOND REASON USED TO READ "a nil writer would leave Add succeeding in
// memory with nothing on disk, which is the exact false durability claim this
// type exists to remove", and it is INVERTED. Add and Revoke both check r.w and
// refuse with ErrNotAttached before writing anything, so an unattached
// registry is FAIL-CLOSED, not silently-succeeding. Attach therefore SPENDS that
// guard rather than adding one — it is not a hardening step and must not be read
// as one.
func (r *OperatorRegistry) Attach(w DurableWriter) error {
	if w == nil {
		return errors.New("auth: attaching the operator registry: the durable writer must not be nil; a nil writer would spend the once-only Attach and leave the registry unattached, where Add and Revoke refuse with ErrNotAttached")
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.w != nil {
		return errors.New("auth: attaching the operator registry: already attached; a registry is bound to exactly one durable log, and a second would give one in-memory registry two durable histories")
	}
	r.w = w
	return nil
}

// AttachSessions binds the registry to the live operator session table, so that
// Revoke drops a revoked operator's already-issued tokens SYNCHRONOUSLY rather
// than leaving them to be refused at next use.
//
// It is called by NewOperatorService, which passes itself — so an
// OperatorService built over a registry is ALWAYS wired for revocation and there
// is no order for a caller to get wrong. It is exported anyway because a test
// (and only a test) has reason to attach a double.
//
// Both halves of revocation matter and neither replaces the other: Authenticate
// re-reads the registry on EVERY call, so a revoked operator is refused even if
// this wiring is absent; and this wiring frees the table entries immediately
// rather than at their natural expiry, which is what stops a revoked principal
// holding session slots for up to SessionLifetime.
func (r *OperatorRegistry) AttachSessions(s OperatorSessionRevoker) error {
	if s == nil {
		return errors.New("auth: attaching the operator session table: the revoker must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions != nil {
		return errors.New("auth: attaching the operator session table: already attached; one registry backs exactly one session table, and a second would leave the first holding live sessions for operators this registry has revoked")
	}
	r.sessions = s
	return nil
}

// Apply implements wal.Applier: it folds one committed operator record into the
// serving copy. It runs BOTH during recovery (inside wal.Open, before anything
// can reach this registry) and on every LIVE commit (inside Txn.Commit, after
// the commit record is fsynced) — and, per wal.Applier's contract, it cannot
// tell the two apart.
//
// # IT DOES NOT RE-RUN THE LIVE-WRITE ADMISSION CHECKS, DELIBERATELY
//
// Add refuses a duplicate id and a certificate fingerprint already live on
// another operator. Apply does NOT: a record reaching here is ALREADY DURABLE,
// so refusing it would not un-write it — it would only turn a damaged log into
// an outage (invariant 6, and the same reasoning WALRoster.Apply records). The
// consequence is that a log CAN present two live operators holding one
// fingerprint, and it is the READ side that declines to guess: see
// LiveOperatorForCertFingerprint, whose ambiguous arm exists precisely for the
// state this method is allowed to build.
//
// # A record it cannot understand is DISCARDED, LOUDLY
//
// Returning an error here would abort recovery and refuse to start the bus (or,
// on a live commit, poison the log). Invariant 6 settled that trade: recovery
// ALWAYS reaches a running server, damaged records are discarded, and the
// absolute requirement is that every discard is LOGGED, specifically. BE HONEST
// ABOUT WHAT THAT COSTS on this plane: a discarded ADD means an operator who
// holds a keypair this bus told them about cannot authenticate; a discarded
// REVOCATION means an operator the bus was told to revoke is still LIVE, which
// is the fail-OPEN direction and is why the log line for it says so in terms.
func (r *OperatorRegistry) Apply(c wal.Committed) error {
	if c.Entry.Kind != OperatorRecordKind {
		// Another participant's record (messages, enrolments, invites, acks,
		// peer configuration). Skipped SILENTLY, exactly as WALRoster.Apply and
		// Hub.Apply skip kinds they do not own — treating a neighbour's record
		// as damage would fill the log with false alarms.
		return nil
	}
	o, err := DecodeOperator(c.Entry.Body)
	if err != nil {
		r.log.Error("DISCARDING an operator record that could not be decoded; if it was an ADD the operator it named CANNOT authenticate to this bus, and if it was a REVOCATION the operator it named is STILL LIVE — that direction is FAIL-OPEN, so re-run `agent-bus operator list -all` and revoke again if the principal should be dead",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"err", err,
		)
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	prev, ok := r.byID[o.OperatorID]
	switch {
	case !ok:
		// AN ORPHAN REVOCATION IS NOT AN ORDINARY ADD, AND MUST NOT LAND IN
		// SILENCE. A revocation record whose ADD never arrived means the record
		// that CREATED the principal was damaged, or the log was truncated between
		// them — the credential fields being inserted here are therefore the only
		// copy of a key and a fingerprint that nothing else in the log corroborates.
		// It is stored anyway (fail-CLOSED: the principal is dead either way, and
		// refusing it would resurrect nobody), but invariant 6's absolute half is
		// that every such discrepancy is reported loudly and specifically.
		if o.Revoked() {
			r.log.Error("an operator REVOCATION record arrived with NO preceding ADD for that id; it is stored as a revoked principal, but the record that CREATED this operator is MISSING — the log was truncated or the add record was damaged, so the key and fingerprint recorded here are uncorroborated. This can only be corruption or tampering — inspect the log",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"operator_id", o.OperatorID,
				"revoked_at", o.RevokedAt,
			)
		}
		r.byID[o.OperatorID] = copyOperator(o)

	case prev.Revoked():
		// NOTHING SUPERSEDES A REVOCATION. Keeping the FIRST one is the
		// fail-closed direction on this plane and it is not the same choice
		// WALRoster.Apply makes for a duplicate agent id — there the rule is
		// "keep the first" because an overwrite would rebind an identity to a
		// different keypair; here the rule is "keep the first REVOCATION"
		// because letting a later record un-revoke a principal would make the
		// log's most security-critical operation reversible by a duplicated or
		// replayed record. An operator id is never reused (invariant 1), so a
		// later record for a revoked id is either a re-revocation (harmless,
		// nothing changes) or damage.
		switch {
		case !o.Revoked():
			r.log.Error("DISCARDING an operator record that would UN-REVOKE a revoked operator; the first revocation is kept and this record is dropped. An operator id is never reused (invariant 1), so revocation is permanent and a live record for a revoked id is either a duplicate or damage",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"operator_id", o.OperatorID,
				"kept_revoked_at", prev.RevokedAt,
			)

		case operatorRecordsAgree(prev, o):
			// AN AGREEING RE-REVOCATION IS A LEGITIMATE RETRY AND STAYS SILENT.
			// Invariant 10: same operation, same payload — the caller's
			// acknowledgement was probably lost, nothing changes, and logging it at
			// ERROR would train an operator to ignore this exact message.
			//
			// "AGREEING" IS NOT "BYTE-IDENTICAL", and this comment said
			// BYTE-IDENTICAL until a reviewer caught it. operatorRecordsAgree
			// compares only the fields that BIND the identity or the revocation —
			// AuthPublicKey, CertFingerprint, CreatedAt, RevokedAt, RevokedReason —
			// and deliberately ignores two:
			//
			//   - Label is the operator's own note. It authorises nothing and binds
			//     nothing, so two revocations differing only in their label ARE the
			//     same event; reporting that as tampering would be a false alarm.
			//   - Name cannot differ for one OperatorID: it is re-derived from the
			//     id and re-checked on encode AND decode, so a record whose name
			//     disagreed never reaches this switch — it fails to decode.
			//
			// Stated at this length because the overclaiming version is the exact
			// species of defect this package keeps finding: a comment that describes
			// a STRICTER rule than the code enforces reads as freshly checked and is
			// more dangerous than no comment.

		default:
			// A SECOND REVOCATION THAT DISAGREES WITH THE STORED ONE. It is
			// DISCARDED (nothing supersedes the first revocation) — and until now it
			// was discarded in SILENCE, which is precisely the defect invariant 6
			// rates above the discard itself. The evidence reported is the same the
			// live-revocation arm below reports, because it means the same thing:
			// two records claim different revocation facts, or different credential
			// fields, for one id that can only have been revoked once.
			r.log.Error("DISCARDING a SECOND operator revocation record that DISAGREES with the stored one (instant, reason, key, certificate fingerprint or creation instant); the FIRST revocation is kept and this record is dropped, so nothing was rebound and the principal stays dead. A re-revocation AGREEING on all of those fields is a legitimate retry and is silent — note that is AGREEING, not byte-identical: the label is ignored deliberately, so a second revocation differing only in its label is also silent — and reaching this line can only be corruption or tampering, so inspect the log",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"operator_id", o.OperatorID,
				"kept_revoked_at", prev.RevokedAt,
				"discarded_revoked_at", o.RevokedAt,
			)
		}

	case !o.Revoked():
		// A second LIVE record for an id already live: a duplicate add. Never an
		// overwrite — an overwrite would rebind a live ADMIN identity to a
		// different keypair, which is the worst outcome available on this path
		// (invariants 1 and 3).
		r.log.Error("DISCARDING a DUPLICATE operator record: this operator id is already registered, so the later record is dropped and the FIRST is kept; an operator id is never reused (invariant 1) and overwriting one would rebind a live admin identity to a different keypair",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"operator_id", o.OperatorID,
			"kept_created_at", prev.CreatedAt,
		)

	default:
		// THE REVOCATION of a live operator: the one case where a later record
		// legitimately supersedes an earlier one.
		//
		// Only the REVOCATION FIELDS are taken. Everything else — the key, the
		// fingerprint, the creation instant — is kept from the record that
		// created the principal, so a record that claims to revoke while also
		// swapping the credential cannot rebind a live identity through the
		// revocation path. Revocation is honoured (fail-closed on authority) and
		// the rebinding is refused (fail-closed on identity); a disagreement is
		// reported because it can only be corruption or tampering.
		next := copyOperator(prev)
		next.RevokedAt = o.RevokedAt
		next.RevokedReason = o.RevokedReason
		if !prev.AuthPublicKey.Equal(o.AuthPublicKey) || prev.CertFingerprint != o.CertFingerprint || !prev.CreatedAt.Equal(o.CreatedAt) {
			r.log.Error("an operator REVOCATION record disagrees with the record that created the operator (key, certificate fingerprint or creation instant); the REVOCATION IS APPLIED and the original credential fields are KEPT, so the principal is dead and nothing was rebound. This can only be corruption or tampering — inspect the log",
				"prepare_index", c.PrepareIndex,
				"commit_index", c.CommitIndex,
				"operator_id", o.OperatorID,
			)
		}
		r.byID[o.OperatorID] = next
	}
	return nil
}

// operatorRecordsAgree reports whether two records for ONE operator id state the
// same facts, so that Apply can tell a LEGITIMATE RETRY from a CONTRADICTION.
//
// It is the line between invariant 10 and invariant 6 on this path: same
// operation with the same payload is a retry and must be silent, while the same
// operation with a DIFFERENT payload is a protocol violation that must be
// reported. Getting it backwards is expensive in both directions — a noisy retry
// teaches operators to ignore the message, and a silent contradiction is the
// discard invariant 6 rates as the defect.
//
// The compared fields are the ones that DECIDE something: the credential (key,
// certificate fingerprint), the creation instant, and the revocation facts
// (instant and reason). Name is not compared because validateOperator already
// requires it to equal the name half of the id, and Label is not compared
// because nothing authorises on it and an operator note is not a fact about the
// principal.
func operatorRecordsAgree(a, b Operator) bool {
	if !a.AuthPublicKey.Equal(b.AuthPublicKey) || a.CertFingerprint != b.CertFingerprint || !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	if a.RevokedReason != b.RevokedReason {
		return false
	}
	switch {
	case a.RevokedAt == nil && b.RevokedAt == nil:
		return true
	case a.RevokedAt == nil || b.RevokedAt == nil:
		return false
	default:
		return a.RevokedAt.Equal(*b.RevokedAt)
	}
}

// Add records a new operator DURABLY and returns only once the record is on
// stable storage and visible in memory (invariant 4).
//
// The order is fixed and must not be rearranged — it is put's, which is
// WALRoster.put's:
//
//  1. validate, outside every lock, touching no shared state;
//  2. take writeMu, which serialises the whole check-then-write;
//  3. REJECT a duplicate id and a bound certificate from memory, BEFORE writing,
//     so a refusal never burns an fsync and never reaches Apply — where the
//     record is already durable and the only available handling is to discard;
//  4. encode, so a record that cannot be stored fails with NOTHING written;
//  5. hand it to the log, which fsyncs the prepare, fsyncs the commit and THEN
//     calls Apply — and it is Apply, not this method, that does the map insert.
//     There is exactly one insertion path, so a replayed record and a live one
//     cannot diverge.
//
// It NEVER overwrites. The two refusals guard the same identity from opposite
// directions and both are mandatory: ErrDuplicateOperatorID keeps one operator
// id from naming two keypairs, ErrOperatorCertBound keeps one certificate from
// naming two operators (the mirror of ErrCertFingerprintBound on the agent
// plane; without it one key holder could authenticate as either and the
// fingerprint would stop naming anybody, invariant 11).
func (r *OperatorRegistry) Add(o Operator) error {
	if err := validateOperator(o); err != nil {
		return err
	}
	if o.Revoked() {
		// An operator is added LIVE. Adding one pre-revoked would create a
		// principal that never existed as a credential, and — because nothing
		// supersedes a revocation — could never be made live afterwards, so it
		// would burn the name's id for nothing.
		return fmt.Errorf("%w: operator %q is already marked revoked; an operator is added LIVE and is revoked by a later record, never in one step", ErrInvalidOperatorRecord, o.OperatorID)
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if r.w == nil {
		// NOT a silent success in memory: that would tell the caller the
		// operator is durable when nothing was written.
		return fmt.Errorf("%w: cannot record the operator %q", ErrNotAttached, o.OperatorID)
	}

	r.mu.Lock()
	_, dup := r.byID[o.OperatorID]
	certHolder, certErr := liveOperatorForFingerprint(r.byID, o.CertFingerprint)
	r.mu.Unlock()

	if dup {
		return fmt.Errorf("%w: %q", ErrDuplicateOperatorID, o.OperatorID)
	}
	// certErr is the ORDINARY case here: ErrOperatorCertUnknown means nobody
	// holds this fingerprint, which is what an add requires. Anything else — a
	// live holder, or an ambiguity recovered off disk — is a refusal.
	switch {
	case certErr == nil:
		return fmt.Errorf("%w: operator %q already holds a live binding for this client certificate, so it cannot also be bound to %q", ErrOperatorCertBound, certHolder, o.OperatorID)
	case errors.Is(certErr, ErrOperatorCertUnknown):
		// Good: unbound.
	default:
		return fmt.Errorf("%w: refusing to add %q over an unresolvable certificate binding: %v", ErrOperatorCertBound, o.OperatorID, certErr)
	}

	return r.write(o, "adding")
}

// Revoke revokes operatorID by APPENDING a new record carrying the whole
// operator with RevokedAt and RevokedReason set.
//
// REVOCATION IS AN APPEND, NEVER AN IN-PLACE EDIT AND NEVER A DELETION
// (invariant 6: the log is append-only in the strict sense). The id stays spent
// forever (invariant 1) and the history stays readable, which is what makes
// `operator list -all` able to answer "who was revoked, when, and why".
//
// # RE-REVOKING IS A LEGITIMATE RETRY, AND WRITES NOTHING
//
// Invariant 10: same key + SAME payload is a retry — return the original result,
// do not re-apply. Re-revoking an already-revoked operator returns the EXISTING
// record unchanged, with its original RevokedAt and reason, and appends nothing.
// Overwriting the instant would rewrite history for a caller doing exactly the
// right thing after a lost acknowledgement.
//
// It also drops the operator's live sessions synchronously when a session table
// is attached (AttachSessions), so a revoked operator's already-issued token
// dies at once rather than at next use.
func (r *OperatorRegistry) Revoke(operatorID, reason string, at time.Time) (Operator, error) {
	if reason == "" {
		// Attribution is required, invariant 6. Checked here as well as in
		// validateOperator so the caller gets the specific message rather than a
		// record-shape one.
		return Operator{}, fmt.Errorf("%w: revoking operator %q requires a reason; an operator action must be loudly attributable", ErrInvalidOperatorRecord, operatorID)
	}
	if at.IsZero() {
		return Operator{}, fmt.Errorf("%w: revoking operator %q requires the instant it happened", ErrInvalidOperatorRecord, operatorID)
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if r.w == nil {
		return Operator{}, fmt.Errorf("%w: cannot revoke the operator %q", ErrNotAttached, operatorID)
	}

	r.mu.Lock()
	prev, ok := r.byID[operatorID]
	r.mu.Unlock()
	if !ok {
		return Operator{}, fmt.Errorf("%w: %q is not registered on this bus", ErrUnknownOperator, operatorID)
	}
	if prev.Revoked() {
		// The idempotent retry. Nothing is written and the ORIGINAL record is
		// returned — see the doc comment. The sessions are still swept, because
		// this call may be the retry of one whose acknowledgement was lost
		// BEFORE it got that far, and dropping nothing is free.
		r.revokeSessions(operatorID)
		return copyOperator(prev), nil
	}

	next := copyOperator(prev)
	u := at.UTC()
	next.RevokedAt = &u
	next.RevokedReason = reason
	if err := r.write(next, "revoking"); err != nil {
		return Operator{}, err
	}

	// AFTER the durable write, never before. A session dropped for a revocation
	// that then failed to commit would be an outage with no record behind it;
	// dropping afterwards can at worst leave a live session for a few
	// microseconds, and Authenticate re-reads the registry on every call anyway.
	dropped := r.revokeSessions(operatorID)
	r.log.Info("operator REVOKED",
		"operator_id", operatorID,
		"revoked_at", u.Format(time.RFC3339Nano),
		"sessions_dropped", dropped,
	)

	r.mu.Lock()
	out, ok := r.byID[operatorID]
	r.mu.Unlock()
	if !ok {
		return copyOperator(next), nil
	}
	return copyOperator(out), nil
}

// write is the shared durable tail of Add and Revoke: encode, hand to the log,
// and CONFIRM THE RECORD REACHED MEMORY. The caller must hold writeMu and must
// already have checked r.w and run the admission rules that apply to it.
//
// The confirm step is WALRoster.put's mis-wiring detector, and it is needed here
// for the identical reason: Apply deliberately returns nil on a record it cannot
// use (it DISCARDS, so recovery of a damaged log still reaches a running bus),
// and inheriting that policy silently on a LIVE write would let Add return
// success for an operator that is durable but absent from the serving copy. The
// reachable trigger is not a round-trip bug, it is MIS-WIRING: Attach takes any
// DurableWriter and cannot check that the log was opened with THIS registry in
// its applier map. Hand it a log wired to a multiplexer that does not know
// OperatorRecordKind and every write would succeed durably, return nil, and
// leave the registry permanently empty.
//
// WHICH IS NO LONGER THE STATE cmd/agent-bus/main.go IS IN (AUTH-10-WIRING,
// 2026-08-21). The sentence above carried "— which is EXACTLY the state
// cmd/agent-bus/main.go is in today, see the type doc —" in the middle of it, and
// that is now false: main.go registers OperatorRecordKind. The check stays
// because the trigger is merely hypothetical FOR MAIN while remaining live for
// any other embedder that opens a log without this registry in its applier map.
// Note what the check is, though: it reads back AFTER the durable commit, so it
// DETECTS that mis-wiring and cannot PREVENT the write it detects.
func (r *OperatorRegistry) write(o Operator, what string) error {
	body, err := EncodeOperator(o)
	if err != nil {
		return err
	}
	if _, err := r.w.Write(wal.Entry{Kind: OperatorRecordKind, Body: body}); err != nil {
		if errors.Is(err, wal.ErrDiverged) {
			// ErrDiverged means the commit record was appended and FSYNCED before
			// the failure: THE ENTRY IS DURABLE and only a neighbouring applier
			// failed. WALRoster.put (walroster.go) reports the same condition as
			// durable, and this path must not collapse it into a generic failure —
			// on THIS plane the consequence is worse than a released invite. A
			// failed-looking `operator add` whose record is in fact on disk becomes
			// a LIVE ADMIN at the next start, while the operator retries and gets a
			// second one under a fresh id (invariant 1: the id is never reused), so
			// one intended add leaves two admin identities and only one of them is
			// in anybody's notes.
			return fmt.Errorf("auth: %s the operator %q: THE RECORD IS DURABLE — the commit was fsynced and a NEIGHBOURING APPLIER then failed, so this operator WILL be present after the next start. DO NOT RETRY the command; run `agent-bus operator list -all` and reconcile: %w", what, o.OperatorID, err)
		}
		return fmt.Errorf("auth: %s the operator %q durably: %w", what, o.OperatorID, err)
	}

	r.mu.Lock()
	stored, applied := r.byID[o.OperatorID]
	r.mu.Unlock()
	if !applied || stored.Revoked() != o.Revoked() {
		return fmt.Errorf("auth: %s the operator %q committed durably but the serving registry does not reflect it; the record is on disk and will be replayed at the next start, but this registry is not in the applier map of the log it was attached to (check the wal.Open Applier wiring — auth.MultiplexApplier is SILENT about a kind nobody registered) or the record was discarded by Apply", what, o.OperatorID)
	}
	return nil
}

// revokeSessions drops the operator's live sessions through the attached table,
// if any. It must NOT be called with mu held: the session table takes its own
// lock and calls nothing back into this registry, but keeping the two lock
// domains disjoint is what guarantees that stays true.
func (r *OperatorRegistry) revokeSessions(operatorID string) int {
	r.mu.Lock()
	s := r.sessions
	r.mu.Unlock()
	if s == nil {
		return 0
	}
	return s.RevokeSessions(operatorID)
}

// Get returns the operator registered under operatorID and whether it was found.
// The returned value is a DEEP COPY (copyOperator): a caller must not be able to
// reach into a stored credential — or into the RevokedAt pointer that decides
// whether it is still a credential at all — through the value it was handed.
//
// A REVOKED operator IS returned, with Revoked() true. Callers on the
// authorisation path must check it; OperatorService.Authenticate does, on every
// call. Hiding revoked entries here would make revocation look like deletion,
// which it deliberately is not.
func (r *OperatorRegistry) Get(operatorID string) (Operator, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.byID[operatorID]
	if !ok {
		return Operator{}, false
	}
	return copyOperator(o), true
}

// List returns every registered operator, live and revoked, sorted by
// OperatorID, as deep copies.
//
// Sorted because the consumer is `agent-bus operator list` and Go randomises map
// iteration: an order that varies run to run turns a stable listing into a flaky
// one and makes two readings of one registry look like two registries.
func (r *OperatorRegistry) List() []Operator {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Operator, 0, len(r.byID))
	for _, o := range r.byID {
		out = append(out, copyOperator(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperatorID < out[j].OperatorID })
	return out
}

// Len reports how many operators are registered, live and revoked together.
func (r *OperatorRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// LiveLen reports how many operators are registered and NOT revoked.
//
// It is a distinct number from Len and both are worth having: Len says how much
// history the registry holds, LiveLen says how many principals can authenticate
// right now. A bus whose LiveLen has reached zero has no operator at all, which
// is a state an operator should be told about rather than discover.
func (r *OperatorRegistry) LiveLen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, o := range r.byID {
		if !o.Revoked() {
			n++
		}
	}
	return n
}

// LiveOperatorForCertFingerprint resolves a client-certificate fingerprint to
// the single operator that holds it LIVE, or refuses.
//
// It is the operator-plane twin of certFingerprintOwner and inherits its rules
// verbatim; the comments there are the full reasoning and are not repeated:
//
//   - THE COMPARISON IS EXACT-MATCH ON THE FINGERPRINT and may NEVER become
//     chain verification. A certificate placed in an x509.CertPool is a TRUSTED
//     ROOT, operator certificates are self-signed, so a pool would make every
//     operator a CA able to mint a certificate for any name that chains to its
//     own — one operator becomes a CA for the whole bus.
//   - THE THREE ANSWERS ARE DISTINCT AND TWO OF THEM ARE REFUSALS:
//     exactly one live holder -> that id, nil; none -> ErrOperatorCertUnknown;
//     more than one -> ErrOperatorCertAmbiguous, and NOT a pick. Returning "the
//     first" or "the newest" would resolve a duplicated certificate to a
//     DEFINITE admin, which is precisely the credential confusion invariant 11
//     exists to prevent.
//   - AMBIGUITY IS REACHABLE even though Add refuses to create it, because Apply
//     replays records that are already durable and does not re-run the
//     write-side check (invariant 6). The read is the thing that must decline to
//     guess.
func (r *OperatorRegistry) LiveOperatorForCertFingerprint(fp [32]byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return liveOperatorForFingerprint(r.byID, fp)
}

// liveOperatorForFingerprint is the free function over the map, in the same
// shape and for the same reason as certFingerprintOwner and sortedCopies: the
// rule is fail-closed and must have exactly one implementation, since two copies
// of a fail-closed rule are two chances for one of them to stop failing closed.
//
// THE CALLER MUST HOLD THE LOCK GUARDING byID.
func liveOperatorForFingerprint(byID map[string]Operator, fp [32]byte) (string, error) {
	// THE ZERO FINGERPRINT NAMES NOBODY AND IS REFUSED BEFORE ANYTHING IS
	// COMPARED — certFingerprintOwner's first rule, and the one that matters
	// most here. It is the value a caller holds when there was NO certificate,
	// so resolving it would turn "this connection presented nothing" into "this
	// connection is an ADMIN": a fail-OPEN, and the worst one available in this
	// package. validateOperator refuses to STORE a zero, so this guards the
	// other direction — a caller that ignored the ok from an accessor and passed
	// the zero value, which is the idiomatic slip
	// `fp, _ := ClientCertFingerprintFromContext(ctx)`.
	if fp == ([32]byte{}) {
		return "", fmt.Errorf("%w: the zero fingerprint is the ABSENCE of a certificate, never a digest, and names nobody", ErrOperatorCertUnknown)
	}

	var holders []string
	for id, o := range byID {
		if !o.Revoked() && o.CertFingerprint == fp {
			holders = append(holders, id)
		}
	}
	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		return "", fmt.Errorf("%w: no live operator holds this client certificate", ErrOperatorCertUnknown)
	default:
		// Sorted, because the operator reading this has to go and revoke all but
		// one, and an unsorted list of map keys makes two reports of one incident
		// look like two incidents.
		sort.Strings(holders)
		return "", fmt.Errorf("%w: %d operators hold one client certificate (%v); it resolves to nobody until all but one are revoked", ErrOperatorCertAmbiguous, len(holders), holders)
	}
}
