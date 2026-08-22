package auth_test

// INVITE-GATE, part 3: the COMPOSITION inside auth.Service.Enrol.
//
// Enrol now orchestrates a two-phase participant it does not own:
//
//	Begin (the caller's)  ->  mint  ->  Consume  ->  PutWithInvite  ->  Commit
//	                                       \                            /
//	                                        \--- Abort, but ONLY when nothing became durable
//
// The whole of the risk is in the RESOLVE GUARD around that sequence. Two
// mistakes are available and they fail in opposite directions:
//
//   - Abort when the composite entry IS durable  -> the invite goes back to open
//     while the log says redeemed. The next attempt is a SECOND redemption of a
//     spent invite: ONE INVITE ADMITS TWO AGENTS.
//   - Fail to Abort when nothing became durable   -> the reservation is stranded
//     and the invite is locked out until a restart, for an enrolment that never
//     happened.
//
// Every path out of Enrol is therefore enumerated below against a fake
// InviteRedemption that COUNTS its calls. A fake rather than a real
// *invite.Redemption on purpose: the question here is what Enrol DOES to a
// participant, and internal/invite's own suite already proves what a real
// participant does in response. The two meet for real in
// invitegate_crash_test.go, over a SIGKILLed process.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// The doubles
// ---------------------------------------------------------------------------

// igFakeInvite is an auth.InviteRedemption that records every call. It is the
// instrument the assertions below read.
type igFakeInvite struct {
	id         string
	riderKind  string
	consumeErr error

	consumes int
	commits  int
	aborts   int
	sawByte  []auth.EnrolResult
}

func newIGFakeInvite(id string) *igFakeInvite {
	return &igFakeInvite{id: id, riderKind: igRiderKind}
}

func (f *igFakeInvite) InviteID() string  { return f.id }
func (f *igFakeInvite) RiderKind() string { return f.riderKind }

func (f *igFakeInvite) Consume(res auth.EnrolResult) (json.RawMessage, error) {
	f.consumes++
	f.sawByte = append(f.sawByte, res)
	if f.consumeErr != nil {
		return nil, f.consumeErr
	}
	return json.RawMessage(fmt.Sprintf(`{"id":%q,"state":"redeemed","redeemed_by":%q}`, f.id, res.AgentID)), nil
}

func (f *igFakeInvite) Commit() { f.commits++ }
func (f *igFakeInvite) Abort()  { f.aborts++ }

// igCompositeRoster is a Roster that CAN take the composite write, with the
// outcome of PutWithInvite under test control. It is the only way to reach
// Enrol's durable/non-durable branches without a real log and a real crash.
type igCompositeRoster struct {
	mu   sync.Mutex
	byID map[string]auth.RosterEntry

	// putWithInvite, when non-nil, replaces the default "store it and report
	// durable" behaviour.
	putWithInvite func(auth.RosterEntry, auth.InviteRider) (bool, error)

	composites []auth.InviteRider
	plainPuts  int
}

func newIGCompositeRoster() *igCompositeRoster {
	return &igCompositeRoster{byID: make(map[string]auth.RosterEntry)}
}

func (r *igCompositeRoster) Put(e auth.RosterEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plainPuts++
	if _, ok := r.byID[e.AgentID]; ok {
		return auth.ErrDuplicateAgentID
	}
	r.byID[e.AgentID] = e
	return nil
}

// AgentIDForCertFingerprint implements auth.Roster: the same fail-closed rule
// the shipped rosters have (see stubRoster's, in auth_test.go). This double is
// about the INVITE-composite write path and no test here binds a certificate, so
// in practice it answers ErrCertBindingUnknown — but it is written out rather
// than stubbed to panic or return nil, so that a future invite test that DOES
// carry a certificate gets the real answer instead of a lie.
func (r *igCompositeRoster) AgentIDForCertFingerprint(fp [32]byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var holders []string
	for agentID, e := range r.byID {
		for _, b := range e.CertBindings {
			if b.RetiredAt == nil && b.Fingerprint == fp {
				holders = append(holders, agentID)
				break
			}
		}
	}
	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		return "", auth.ErrCertBindingUnknown
	default:
		return "", auth.ErrCertBindingAmbiguous
	}
}

// AgentIDForAuthKey implements auth.Roster: the same fail-closed rule the
// shipped rosters have (see authKeyOwner). Written out rather than stubbed so an
// enrolment test that reuses an auth key gets the real answer.
func (r *igCompositeRoster) AgentIDForAuthKey(key ed25519.PublicKey) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var holders []string
	for agentID, e := range r.byID {
		if e.AuthPublicKey.Equal(key) {
			holders = append(holders, agentID)
		}
	}
	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		return "", auth.ErrAuthKeyUnknown
	default:
		return "", auth.ErrAuthKeyAmbiguous
	}
}

func (r *igCompositeRoster) PutWithInvite(e auth.RosterEntry, rider auth.InviteRider) (bool, error) {
	r.mu.Lock()
	r.composites = append(r.composites, rider)
	r.mu.Unlock()
	if r.putWithInvite != nil {
		return r.putWithInvite(e, rider)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[e.AgentID]; ok {
		return false, auth.ErrDuplicateAgentID
	}
	r.byID[e.AgentID] = e
	return true, nil
}

func (r *igCompositeRoster) Get(agentID string) (auth.RosterEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[agentID]
	return e, ok
}

func (r *igCompositeRoster) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

func (r *igCompositeRoster) List() []auth.RosterEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]auth.RosterEntry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	return out
}

// igEnrolReq is a well-formed enrolment request carrying inv.
func igEnrolReq(name, key string, inv auth.InviteRedemption) auth.EnrolRequest {
	return auth.EnrolRequest{
		Name:               name,
		PublicKey:          fixedKey(0x51),
		MessagingPublicKey: fixedKey(0x52),
		IdempotencyKey:     key,
		Invite:             inv,
	}
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

// TestInviteGateEnrolConsumesThenCommitsAndNeverAborts is the sequence, and the
// provenance that comes with it.
func TestInviteGateEnrolConsumesThenCommitsAndNeverAborts(t *testing.T) {
	roster := newIGCompositeRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})
	inv := newIGFakeInvite("inv-0000000000000007")

	res, err := svc.Enrol(igEnrolReq("worker", "k-1", inv))
	if err != nil {
		t.Fatalf("Enrol with an invite: %v", err)
	}
	if res.AgentID != mustAgentID(t, "worker", 1) {
		t.Fatalf("Enrol minted %q, want %q", res.AgentID, mustAgentID(t, "worker", 1))
	}
	if inv.consumes != 1 || inv.commits != 1 || inv.aborts != 0 {
		t.Fatalf("the redemption saw consume=%d commit=%d abort=%d, want 1/1/0: a durable invited enrolment consumes once, commits once and NEVER aborts", inv.consumes, inv.commits, inv.aborts)
	}
	if roster.plainPuts != 0 {
		t.Fatalf("an INVITED enrolment took the plain Put path %d times; the consumption record would then be a SEPARATE transaction, which is the exact window the composite record exists to close", roster.plainPuts)
	}
	if len(roster.composites) != 1 {
		t.Fatalf("the roster received %d composite writes, want 1", len(roster.composites))
	}
	if got := roster.composites[0].Kind; got != igRiderKind {
		t.Errorf("the rider was written under kind %q, want the redemption's own RiderKind() %q", got, igRiderKind)
	}

	// Consume is handed the RESULT, so the consumption record can store the
	// response a legitimate retry will be replayed verbatim.
	if len(inv.sawByte) != 1 || inv.sawByte[0].AgentID != res.AgentID {
		t.Fatalf("Consume was handed %+v, want the enrolment result naming %q", inv.sawByte, res.AgentID)
	}
	if inv.sawByte[0].Replayed {
		t.Errorf("Consume was handed a result marked Replayed; the stored record must describe the ORIGINAL enrolment")
	}

	// PROVENANCE. The roster entry records WHICH invite admitted this agent.
	stored, ok := roster.Get(res.AgentID)
	if !ok {
		t.Fatalf("the agent is not in the roster after a successful invited enrolment")
	}
	if stored.InviteID != inv.id {
		t.Fatalf(`the stored enrolment records invite_id %q, want %q.

Provenance is the only durable answer to "which invite admitted this agent". An
operator revoking a leaked invite otherwise has no way to find what it let in.`, stored.InviteID, inv.id)
	}
}

// TestInviteGateUninvitedEnrolmentIsUnchanged is the REGRESSION that must not
// break, and it is scoped to a service built WITHOUT Options.RequireInvite.
//
// It said "this build REDEEMS an invite; it does not REQUIRE one, and nine
// agents on a live bus depend on that". INVITE-GATE-ENFORCE made the shipped bus
// require one, so what this now pins is narrower and still worth pinning: with
// the gate OFF, a nil Invite takes the plain Put path exactly as it did before
// INVITE-GATE and leaves InviteID empty. The already-enrolled agents it was
// written to protect are unaffected either way — they do not re-enrol.
func TestInviteGateUninvitedEnrolmentIsUnchanged(t *testing.T) {
	roster := newIGCompositeRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})

	res, err := svc.Enrol(auth.EnrolRequest{
		Name:           "worker",
		PublicKey:      fixedKey(0x51),
		IdempotencyKey: "k-uninvited",
	})
	if err != nil {
		t.Fatalf("an UN-INVITED enrolment was refused: %v\n\nThis build accepts enrolment without an invite. Requiring one is a SEPARATE task, and making the flip here locks every already-enrolled agent's peers out of the bus.", err)
	}
	if len(roster.composites) != 0 {
		t.Fatalf("an un-invited enrolment took the COMPOSITE path %d times", len(roster.composites))
	}
	if roster.plainPuts != 1 {
		t.Fatalf("an un-invited enrolment took the plain Put path %d times, want 1", roster.plainPuts)
	}
	stored, _ := roster.Get(res.AgentID)
	if stored.InviteID != "" {
		t.Errorf("an un-invited enrolment recorded invite_id %q, want empty", stored.InviteID)
	}
}

// ---------------------------------------------------------------------------
// The resolve guard
// ---------------------------------------------------------------------------

// TestInviteGateEnrolAbortsWhenNothingBecameDurable enumerates the failures that
// happen BEFORE or WITHOUT a durable write. Every one of them must release the
// reservation exactly once — never zero times (the invite is locked out until a
// restart) and never twice.
func TestInviteGateEnrolAbortsWhenNothingBecameDurable(t *testing.T) {
	tests := []struct {
		name    string
		req     func(*igFakeInvite) auth.EnrolRequest
		roster  func(*igCompositeRoster)
		wantErr error
		why     string
	}{
		{
			name: "an invalid idempotency key",
			req: func(inv *igFakeInvite) auth.EnrolRequest {
				return igEnrolReq("worker", "not a valid key!", inv)
			},
			wantErr: auth.ErrInvalidIdempotencyKey,
			why:     "refused during validation, before anything is touched",
		},
		{
			name: "an invalid agent name",
			req: func(inv *igFakeInvite) auth.EnrolRequest {
				return igEnrolReq("Not A Name", "k-1", inv)
			},
			wantErr: auth.ErrInvalidName,
			why:     "refused before the mint",
		},
		{
			name: "a wrong-size public key",
			req: func(inv *igFakeInvite) auth.EnrolRequest {
				r := igEnrolReq("worker", "k-1", inv)
				r.PublicKey = []byte{1, 2, 3}
				return r
			},
			wantErr: auth.ErrInvalidPublicKey,
			why:     "refused before the mint, so a malformed key burns no suffix",
		},
		{
			name: "Consume itself fails",
			req: func(inv *igFakeInvite) auth.EnrolRequest {
				inv.consumeErr = errors.New("the reservation was already reaped")
				return igEnrolReq("worker", "k-1", inv)
			},
			why: "the record was never built, so nothing could have been written",
		},
		{
			name: "the composite write fails and is NOT durable",
			req: func(inv *igFakeInvite) auth.EnrolRequest {
				return igEnrolReq("worker", "k-1", inv)
			},
			roster: func(r *igCompositeRoster) {
				r.putWithInvite = func(auth.RosterEntry, auth.InviteRider) (bool, error) {
					return false, errors.New("disk on fire")
				}
			},
			why: "nothing reached stable storage, so the invite must go back to the operator",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			roster := newIGCompositeRoster()
			if tc.roster != nil {
				tc.roster(roster)
			}
			svc, _ := newService(t, auth.Options{Roster: roster})
			inv := newIGFakeInvite("inv-0000000000000007")

			_, err := svc.Enrol(tc.req(inv))
			if err == nil {
				t.Fatalf("Enrol succeeded, want a failure (%s)", tc.why)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Enrol = %v, want %v", err, tc.wantErr)
			}
			if inv.aborts != 1 {
				t.Fatalf(`the redemption was aborted %d times, want exactly 1 (%s).

Zero aborts strands the reservation: the invite is locked out of every future
redemption until the process restarts, over an enrolment that never happened.`, inv.aborts, tc.why)
			}
			if inv.commits != 0 {
				t.Fatalf("a FAILED enrolment committed the redemption %d times", inv.commits)
			}
		})
	}
}

// TestInviteGateEnrolNeverAbortsADurableWrite is the direction that costs single
// use if it is wrong, and it is inherited VERBATIM from
// internal/invite/store.go's Redeem: on a write that failed AFTER its commit
// record was fsynced, the consumption record IS on stable storage.
//
// Aborting there would release the reservation, memory would say OPEN while disk
// says REDEEMED, and the next redemption would be a SECOND redemption of a spent
// invite. Abandoning it instead is fail-closed: the invite stays locked until a
// restart rebuilds the table from the log.
func TestInviteGateEnrolNeverAbortsADurableWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"wal.ErrDiverged", fmt.Errorf("wal: applying committed entry 9: %w", wal.ErrDiverged)},
		{"durable but absent from the serving roster", errors.New("auth: the enrolment committed durably but is ABSENT from the serving roster")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roster := newIGCompositeRoster()
			roster.putWithInvite = func(auth.RosterEntry, auth.InviteRider) (bool, error) {
				return true, tc.err // durable == TRUE
			}
			svc, _ := newService(t, auth.Options{Roster: roster})
			inv := newIGFakeInvite("inv-0000000000000007")

			if _, err := svc.Enrol(igEnrolReq("worker", "k-1", inv)); err == nil {
				t.Fatalf("Enrol returned nil although the write failed")
			}
			if inv.aborts != 0 {
				t.Fatalf(`the redemption was ABORTED %d times after a write that reported durable=true.

The composite entry -- INCLUDING the invite consumption record -- is already on
stable storage. Releasing the reservation puts memory (OPEN) at odds with disk
(REDEEMED), and the next attempt is a SECOND redemption of a spent invite. ONE
INVITE THEN ADMITS TWO AGENTS.`, inv.aborts)
			}
			if inv.commits != 0 {
				t.Fatalf("a FAILED enrolment committed the redemption %d times; the reservation is abandoned, not committed", inv.commits)
			}
		})
	}
}

// TestInviteGateEnrolFailsClosedOnANonAtomicRoster: an invited enrolment against
// a roster that cannot write both records in one transaction is REFUSED, not
// downgraded to two writes.
//
// Splitting them reopens exactly the window the participant API exists to close.
// Failing closed costs one refused enrolment; the alternative costs single use.
func TestInviteGateEnrolFailsClosedOnANonAtomicRoster(t *testing.T) {
	// MemoryRoster is the roster in question: it satisfies auth.Roster and has
	// no PutWithInvite.
	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})
	inv := newIGFakeInvite("inv-0000000000000007")

	_, err := svc.Enrol(igEnrolReq("worker", "k-1", inv))
	if !errors.Is(err, auth.ErrInviteNotAtomic) {
		t.Fatalf("Enrol against a MemoryRoster = %v, want ErrInviteNotAtomic: a roster that cannot co-commit the consumption record must REFUSE, never split the transaction", err)
	}
	if roster.Len() != 0 {
		t.Fatalf("the refused enrolment reached the roster anyway (%d entries)", roster.Len())
	}
	if inv.consumes != 0 {
		t.Fatalf("Consume ran %d times on a roster that could never have written the record", inv.consumes)
	}
	if inv.aborts != 1 {
		t.Fatalf("the redemption was aborted %d times, want 1: the refusal must hand the invite back", inv.aborts)
	}
	// The error names the roster type, because the remedy is a wiring change.
	if !strings.Contains(err.Error(), "PutWithInvite") {
		t.Errorf("the error does not say what the roster is missing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Idempotency (invariant 10) with an invite in the payload
// ---------------------------------------------------------------------------

// TestInviteGateEnrolReplayAppliesNothingAndReleasesTheFreshReservation is
// invariant 10's legitimate-retry carve-out, with the invite half spelled out.
//
// The retry's OWN reservation must be released: this enrolment spends no invite
// (the original already spent whichever one it carried), so holding the fresh
// reservation would lock the invite out for a client doing exactly the right
// thing.
func TestInviteGateEnrolReplayAppliesNothingAndReleasesTheFreshReservation(t *testing.T) {
	roster := newIGCompositeRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})

	first := newIGFakeInvite("inv-0000000000000007")
	original, err := svc.Enrol(igEnrolReq("worker", "k-retry", first))
	if err != nil {
		t.Fatalf("the original enrolment: %v", err)
	}

	// The retry arrives with a FRESH reservation for the SAME invite — which is
	// what a client that never saw the 201 would present.
	second := newIGFakeInvite("inv-0000000000000007")
	replay, err := svc.Enrol(igEnrolReq("worker", "k-retry", second))
	if err != nil {
		t.Fatalf("the retry was refused: %v; a retry of an enrolment this server already accepted must be ANSWERED, not punished", err)
	}
	if !replay.Replayed {
		t.Errorf("the retry's result is not marked Replayed; the caller cannot tell that NOTHING was re-applied")
	}
	if replay.AgentID != original.AgentID || !replay.EnrolledAt.Equal(original.EnrolledAt) {
		t.Fatalf("the retry returned %+v, want the ORIGINAL result %+v byte for byte", replay, original)
	}
	if second.consumes != 0 {
		t.Fatalf("the retry CONSUMED the invite %d times; a replay applies nothing, and a second consumption record would be a second redemption", second.consumes)
	}
	if second.commits != 0 {
		t.Fatalf("the retry committed the redemption %d times", second.commits)
	}
	if second.aborts != 1 {
		t.Fatalf(`the retry's fresh reservation was aborted %d times, want 1.

A replay spends no invite, so the reservation it took on the way in must go
back. Holding it locks the invite out of every later redemption until a restart
-- and the party punished is the client that retried correctly.`, second.aborts)
	}
	if len(roster.composites) != 1 {
		t.Fatalf("the roster took %d composite writes across the original and its retry, want exactly 1", len(roster.composites))
	}
	if roster.Len() != 1 {
		t.Fatalf("the roster holds %d agents after one enrolment and one retry, want 1", roster.Len())
	}
}

// TestInviteGateEnrolSameKeyDifferentInviteIsAViolation: the invite id is part
// of the REMEMBERED PAYLOAD, so re-presenting one key with a DIFFERENT invite is
// a key reused for different content, not a retry.
//
// Leaving the invite id out of the comparison would be WORSE than a missing
// check: the call would be answered with the ORIGINAL result and apply nothing,
// so the SECOND invite would be left UNSPENT while the caller walked away
// believing it had been redeemed.
func TestInviteGateEnrolSameKeyDifferentInviteIsAViolation(t *testing.T) {
	roster := newIGCompositeRoster()
	svc, _ := newService(t, auth.Options{Roster: roster})

	first := newIGFakeInvite("inv-0000000000000007")
	if _, err := svc.Enrol(igEnrolReq("worker", "k-shared", first)); err != nil {
		t.Fatalf("the original enrolment: %v", err)
	}

	other := newIGFakeInvite("inv-0000000000000008")
	_, err := svc.Enrol(igEnrolReq("worker", "k-shared", other))
	if !errors.Is(err, auth.ErrIdempotencyKeyReused) {
		t.Fatalf(`Enrol with the same key and a DIFFERENT invite = %v, want ErrIdempotencyKeyReused.

Answered as a replay instead, this call returns the original agent id and applies
nothing -- so invite %s is left UNSPENT while the caller believes it was
redeemed.`, err, other.id)
	}
	if other.consumes != 0 {
		t.Fatalf("the refused call consumed invite %s", other.id)
	}
	if other.aborts != 1 {
		t.Fatalf("the refused call aborted its reservation %d times, want 1", other.aborts)
	}

	// And the mirror: a key first used WITHOUT an invite, then re-presented WITH
	// one. The un-invited enrolment remembers the empty string, so this is
	// caught by the same comparison. It uses a DISTINCT auth key (0x71) from the
	// first enrolment above, so it is a fresh identity and not refused by the
	// auth-key uniqueness rule (AUTH-DUP-ENROL-KEY) before the idempotency check
	// this case is about can be exercised.
	if _, err := svc.Enrol(auth.EnrolRequest{Name: "worker", PublicKey: fixedKey(0x71), MessagingPublicKey: fixedKey(0x72), IdempotencyKey: "k-was-uninvited"}); err != nil {
		t.Fatalf("the un-invited enrolment: %v", err)
	}
	late := newIGFakeInvite("inv-0000000000000009")
	// The SAME 0x71 key as the un-invited enrolment above, so ONLY the invite
	// differs from the remembered payload — which is the difference this case is
	// about (the invite id is part of the remembered payload).
	relate := auth.EnrolRequest{Name: "worker", PublicKey: fixedKey(0x71), MessagingPublicKey: fixedKey(0x72), IdempotencyKey: "k-was-uninvited", Invite: late}
	if _, err := svc.Enrol(relate); !errors.Is(err, auth.ErrIdempotencyKeyReused) {
		t.Fatalf("re-presenting an un-invited key WITH an invite = %v, want ErrIdempotencyKeyReused", err)
	}
	if late.aborts != 1 {
		t.Fatalf("that refusal aborted its reservation %d times, want 1", late.aborts)
	}
}

// TestInviteGateEnrolCapacityRefusalStillReleasesTheInvite: admission control
// runs AFTER the idempotency check and BEFORE the mint, and it is on the path an
// invited enrolment takes too. A refusal there must still hand the invite back.
func TestInviteGateEnrolCapacityRefusalStillReleasesTheInvite(t *testing.T) {
	roster := newIGCompositeRoster()
	svc, _ := newService(t, auth.Options{Roster: roster, MaxRosterEntries: 1})

	if _, err := svc.Enrol(auth.EnrolRequest{Name: "first", PublicKey: fixedKey(0x51), IdempotencyKey: "k-0"}); err != nil {
		t.Fatalf("filling the roster: %v", err)
	}

	inv := newIGFakeInvite("inv-0000000000000007")
	if _, err := svc.Enrol(igEnrolReq("worker", "k-1", inv)); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("Enrol against a full roster = %v, want ErrCapacity", err)
	}
	if inv.consumes != 0 {
		t.Fatalf("a capacity refusal consumed the invite")
	}
	if inv.aborts != 1 {
		t.Fatalf("a capacity refusal aborted the reservation %d times, want 1: a bus that is temporarily full must not permanently strand an operator's invite", inv.aborts)
	}
}
