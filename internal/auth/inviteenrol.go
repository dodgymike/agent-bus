package auth

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// EnrolInviteRecordKind is the wal.Entry.Kind of a COMPOSITE enrolment: one
// entry carrying BOTH the enrolment record and the invite-consumption record
// that authorised it, so the two are one transaction (invariant 4 — nothing is
// acknowledged before it is durable, and "atomic" here means one prepare and
// one commit, not two entries written back to back).
//
// NO RESERVATION WAS TAKEN FOR IT, AND NONE IS NEEDED. wal.Entry.Kind is a
// free-form application STRING, not a reserved on-disk record-type NUMBER — the
// numbers (wal.Type) are owned by internal/wal and are reserved through the
// Spec Server; this is not one of them. This is the same statement RecordKind's
// doc makes (record.go), repeated here so the next reader does not go and
// reserve a number nothing requires.
//
// # THE FORWARD HAZARD — READ THIS BEFORE WIRING CHECKPOINTS
//
// internal/wal's MultiApplier (checkpoint.go) treats an UNOWNED kind as a HARD
// ERROR: it returns "no registered checkpoint participant", which POISONS the
// log on a live write (wal.ErrDiverged) and fails Open on recovery. Read its
// CODE and not its type comment, which still says "an unowned kind is ignored
// for forward compatibility" — that is stale, and MultiApplier.Apply a few
// lines below it does the opposite. Checkpoints
// are NOT wired into the server run path today — cmd/agent-bus/main.go passes
// `Applier:`, not `Checkpoints:` — so this kind reaches only MultiplexApplier
// below, which is deliberately silent about kinds it does not own. On the day
// checkpoints ARE wired in, THIS KIND MUST BE REGISTERED by a
// CheckpointParticipant or the bus stops starting.
const EnrolInviteRecordKind = "agent+invite"

// EnrolInviteRecordVersion is the schema version of the composite JSON payload
// (compositeJSON below). It versions ONLY the envelope: each half carries its
// own version inside its own bytes, so bumping auth.RecordVersion or the invite
// record's version does not move this number.
const EnrolInviteRecordVersion = 1

// InviteRider is the rider half of a composite entry: the durable record that
// must commit in the SAME transaction as the enrolment, plus the wal kind it
// replays as.
//
// The kind is CARRIED IN THE RECORD rather than hard-coded here so this package
// does not have to import internal/invite. That import would be a layering
// inversion — internal/invite is composed BY this package's caller
// (internal/httpapi), which adapts *invite.Redemption to InviteRedemption — and
// it would put a second copy of "invite" in a package that has no business
// knowing the name.
type InviteRider struct {
	// Kind is the wal.Entry.Kind the rider replays as (invite.RecordKind
	// today). It may not be empty, and it may not be RecordKind or
	// EnrolInviteRecordKind — see EncodeEnrolWithInvite.
	Kind string

	// Body is the rider's durable record, exactly as its own package encoded
	// it. It is opaque here: this package never decodes it, and the multiplexer
	// hands it straight back to the applier registered for Kind.
	Body json.RawMessage
}

// compositeJSON is the ON-DISK SHAPE of a composite enrol+invite record. THESE
// FIELD NAMES ARE FOREVER: they are written into an append-only log that later
// builds must still read.
//
//	{"v":1,
//	 "enrolment":{...the exact bytes auth.Encode produces...},
//	 "rider_kind":"invite",
//	 "rider":{...the exact bytes the rider's package produced...}}
//
// Both halves are embedded VERBATIM as raw JSON rather than re-modelled, which
// is what makes the expansion in MultiplexApplier.Apply exact: the bytes handed
// to each participant are byte-for-byte the bytes that participant would have
// received had the record been written on its own.
type compositeJSON struct {
	V         int             `json:"v"`
	Enrolment json.RawMessage `json:"enrolment"`
	RiderKind string          `json:"rider_kind"`
	Rider     json.RawMessage `json:"rider"`
}

// EncodeEnrolWithInvite renders a composite enrol+invite record as the JSON body
// of a wal.Entry.
//
// It VALIDATES BEFORE IT RETURNS, exactly as Encode does and for the same
// reason: this runs BEFORE the durable write, so a record that cannot be stored
// fails the whole operation with NOTHING written — rather than being discovered
// as broken at replay, when the enrolment is durable, the invite is spent, and
// every remaining option is bad.
func EncodeEnrolWithInvite(e RosterEntry, rider InviteRider) (json.RawMessage, error) {
	// The enrolment half goes through Encode, so the composite's inner bytes are
	// LITERALLY the bytes a plain enrolment record would carry — and Encode
	// validates the entry on the way.
	enrolment, err := Encode(e)
	if err != nil {
		return nil, err
	}
	if err := validateRiderKind(rider.Kind); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(rider.Body)) == 0 {
		return nil, fmt.Errorf("%w: the composite record for %q carries an empty rider body; a rider that records nothing has no business sharing the enrolment's transaction", ErrInvalidRecord, e.AgentID)
	}
	if !json.Valid(rider.Body) {
		// Checked here rather than trusted: the rider is embedded as raw JSON, so
		// an invalid body would produce a composite record that this build writes
		// and no build can ever read back.
		return nil, fmt.Errorf("%w: the composite record for %q carries a rider body that is not valid JSON", ErrInvalidRecord, e.AgentID)
	}

	rec := compositeJSON{
		V:         EnrolInviteRecordVersion,
		Enrolment: enrolment,
		RiderKind: rider.Kind,
		Rider:     rider.Body,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// DecodeEnrolWithInvite parses a composite enrol+invite record read back off
// disk.
//
// It is STRICT for the reason Decode is strict: a record read off disk is
// UNTRUSTED INPUT (invariant 1) even though this server wrote it, because "this
// server wrote it" is exactly the claim corruption disproves. Unknown fields
// and trailing data are refused, the version must be this build's, the
// enrolment half goes through Decode, and the rider kind is re-checked with the
// same predicate the encoder applied.
func DecodeEnrolWithInvite(raw json.RawMessage) (RosterEntry, InviteRider, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j compositeJSON
	if err := dec.Decode(&j); err != nil {
		return RosterEntry{}, InviteRider{}, fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	if dec.More() {
		return RosterEntry{}, InviteRider{}, fmt.Errorf("%w: trailing data after the composite record", ErrInvalidRecord)
	}
	if j.V != EnrolInviteRecordVersion {
		return RosterEntry{}, InviteRider{}, fmt.Errorf("%w: composite record schema version %d, this build understands %d", ErrInvalidRecord, j.V, EnrolInviteRecordVersion)
	}
	if err := validateRiderKind(j.RiderKind); err != nil {
		return RosterEntry{}, InviteRider{}, err
	}
	if len(bytes.TrimSpace(j.Rider)) == 0 {
		return RosterEntry{}, InviteRider{}, fmt.Errorf("%w: the composite record carries an empty rider body", ErrInvalidRecord)
	}
	e, err := Decode(j.Enrolment)
	if err != nil {
		return RosterEntry{}, InviteRider{}, err
	}
	return e, InviteRider{Kind: j.RiderKind, Body: j.Rider}, nil
}

// validateRiderKind enforces the one structural rule about a rider's kind: it
// names ANOTHER applier, never this package's own record and never the
// composite envelope itself.
//
// RecordKind would make the multiplexer hand the enrolment applier a body it
// cannot decode; EnrolInviteRecordKind would make it re-enter the expansion and
// apply the enrolment twice.
func validateRiderKind(kind string) error {
	switch kind {
	case "":
		return fmt.Errorf("%w: a composite record's rider kind is empty, so nothing could apply it at replay", ErrInvalidRecord)
	case RecordKind:
		return fmt.Errorf("%w: a composite record's rider kind is %q, which is the enrolment kind itself; the rider must name a DIFFERENT applier", ErrInvalidRecord, RecordKind)
	case EnrolInviteRecordKind:
		return fmt.Errorf("%w: a composite record's rider kind is %q, which is the composite envelope itself; that would apply the enrolment twice", ErrInvalidRecord, EnrolInviteRecordKind)
	}
	return nil
}

// MultiplexApplier is the log's ONE applier: it dispatches each committed entry
// to the participant registered for its kind, and EXPANDS a composite
// enrol+invite entry into the two records it carries.
//
// It exists because a wal.Log has exactly one Applier and this bus now has two
// live participants — the enrolment roster and the invite store — plus a
// composite record that belongs to both. Composing them here keeps exactly one
// insertion path per participant: a replayed composite and a live composite go
// through the same expansion, in the same order, so memory and disk cannot
// drift (invariant 5).
//
// It is NOT internal/wal's MultiApplier and must not be confused with it: that
// one is the checkpoint dispatcher and treats an unowned kind as a HARD ERROR.
// This one is deliberately silent about kinds it does not own — see Apply.
//
// The zero value is not usable; construct with NewMultiplexApplier.
type MultiplexApplier struct {
	log    *logging.Logger
	byKind map[string]wal.Applier
}

// NewMultiplexApplier builds a multiplexer over the per-kind appliers in byKind.
//
// The map is COPIED, so a caller cannot mutate the dispatch table after the log
// has been opened with it — a mid-replay change of applier would silently split
// one record kind's history across two serving copies.
//
// EnrolInviteRecordKind may NOT be registered. A composite entry is EXPANDED,
// never dispatched whole: registering an applier for it would apply the
// enrolment through the expansion AND hand the whole envelope to that applier,
// which is a double-apply of an identity record (invariant 1).
func NewMultiplexApplier(logger *logging.Logger, byKind map[string]wal.Applier) (*MultiplexApplier, error) {
	if len(byKind) == 0 {
		return nil, fmt.Errorf("auth: creating the WAL applier multiplexer: at least one per-kind applier is required; a multiplexer over nothing would silently discard every record in the log")
	}
	m := &MultiplexApplier{log: logger, byKind: make(map[string]wal.Applier, len(byKind))}
	for kind, a := range byKind {
		if kind == "" {
			return nil, fmt.Errorf("auth: creating the WAL applier multiplexer: a per-kind applier is registered under the empty kind, which no record carries")
		}
		if kind == EnrolInviteRecordKind {
			return nil, fmt.Errorf("auth: creating the WAL applier multiplexer: %q may not be registered; a composite enrol+invite entry is EXPANDED into its two halves and dispatched to their own appliers, so registering it would apply the enrolment twice", EnrolInviteRecordKind)
		}
		if a == nil {
			return nil, fmt.Errorf("auth: creating the WAL applier multiplexer: the applier registered for kind %q is nil; records of that kind would be silently dropped rather than applied", kind)
		}
		m.byKind[kind] = a
	}
	return m, nil
}

// Apply implements wal.Applier. It runs BOTH during recovery (inside wal.Open)
// and on every LIVE commit (inside Txn.Commit, after the commit fsync), and it
// cannot tell the two apart.
//
// # A composite entry is EXPANDED, enrolment half FIRST
//
// The enrolment record is dispatched to byKind[RecordKind] and then the rider to
// byKind[rider.Kind]. The order is fixed so that a rider applier which reads the
// roster (none does today) sees the agent the invite was spent on.
//
// # AN UNDECODABLE COMPOSITE DISCARDS BOTH HALVES, AND SAYS SO
//
// It returns nil — never an error. A non-nil error POISONS the log on a live
// write (wal.ErrDiverged) and makes Open fail on recovery, and invariant 6
// settled that recovery ALWAYS reaches a running server: damaged records are
// discarded and the bus starts, provided every discard is LOGGED loudly and
// specifically. So the discard is reported at ERROR with both indices and the
// exact reason.
//
// BE HONEST ABOUT WHAT IT COSTS, in both directions:
//
//   - the AGENT is not in the roster. It was acknowledged as enrolled, holds an
//     id this bus minted and told it, and must re-enrol under a NEW id (the old
//     suffix is burned, and that is correct — invariant 1).
//   - the INVITE is NOT marked spent, so it stays redeemable. That direction is
//     FAIL-OPEN and is exactly the hazard internal/invite/doc.go section 5
//     documents. An operator seeing this line should REVOKE the invite if they
//     can identify it, rather than assume the discard was safe.
//
// # An unregistered kind is skipped SILENTLY, and that is NOT MultiApplier's policy
//
// internal/wal's MultiApplier makes an unowned kind a hard error, because a
// checkpoint that cannot account for a record cannot claim to be complete. This
// log is different: it also carries store.RecordKind ("message") records and
// hub.SeqFloorRecordKind ("seqfloor") records, and the hub applies those through
// its OWN read-only replay pass (see the hub.Open comment in
// cmd/agent-bus/main.go — the hub deliberately is not the log's applier). Those
// records are a NEIGHBOUR's, not damage, and treating them as damage would fill
// the log with false alarms. This is exactly the behaviour WALRoster.Apply had
// when it was the sole applier, preserved unchanged.
//
// # A participant's error is RETURNED UNCHANGED
//
// Both participants today (WALRoster.Apply and invite.Store.Apply) always return
// nil by design, so this path is unreachable in this build. It is not swallowed:
// a participant that decides a failure is fatal enough to return has made that
// judgement deliberately, and this multiplexer is not the place to overrule it.
func (m *MultiplexApplier) Apply(c wal.Committed) error {
	if c.Entry.Kind != EnrolInviteRecordKind {
		if a := m.byKind[c.Entry.Kind]; a != nil {
			return a.Apply(c)
		}
		return nil
	}

	e, rider, err := DecodeEnrolWithInvite(c.Entry.Body)
	if err != nil {
		m.log.Error("DISCARDING a composite enrolment+invite record that could not be decoded; BOTH HALVES ARE LOST. The agent it named is NOT in this bus's roster and must re-enrol under a NEW id, and the invite it spent is NOT marked spent, so it remains REDEEMABLE — that half is FAIL-OPEN. If you can identify the invite, REVOKE it rather than assume this discard was safe",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"err", err,
		)
		return nil
	}

	// Re-encoded rather than sliced back out of the raw envelope so that the
	// bytes handed to the enrolment applier are produced by the ONE encoder this
	// package has (Encode), whatever the composite's inner formatting was.
	body, err := Encode(e)
	if err != nil {
		m.log.Error("DISCARDING a composite enrolment+invite record whose enrolment half decoded but could not be re-encoded; BOTH HALVES ARE LOST and the invite is NOT marked spent, so it remains REDEEMABLE — revoke it if you can identify it",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"agent_id", e.AgentID,
			"err", err,
		)
		return nil
	}

	if a := m.byKind[RecordKind]; a != nil {
		if err := a.Apply(wal.Committed{
			PrepareIndex: c.PrepareIndex,
			CommitIndex:  c.CommitIndex,
			Entry:        wal.Entry{Kind: RecordKind, Body: body},
		}); err != nil {
			return err
		}
	} else {
		m.log.Error("a composite enrolment+invite record was applied with NO enrolment applier registered; the agent is NOT in the serving roster",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex, "agent_id", e.AgentID)
	}

	a := m.byKind[rider.Kind]
	if a == nil {
		m.log.Error("a composite enrolment+invite record names a rider kind with NO registered applier; the rider is UNAPPLIED, so the invite is not marked spent in the serving copy and may be REDEEMABLE AGAIN until a restart with the applier wired",
			"prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
			"agent_id", e.AgentID, "rider_kind", rider.Kind)
		return nil
	}
	return a.Apply(wal.Committed{
		PrepareIndex: c.PrepareIndex,
		CommitIndex:  c.CommitIndex,
		Entry:        wal.Entry{Kind: rider.Kind, Body: rider.Body},
	})
}
