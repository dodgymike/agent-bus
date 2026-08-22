package auth_test

// AUTH-10: the OPERATOR/ADMIN PRINCIPAL — a bus-scoped, non-agent identity that
// can authenticate to the running bus.
//
// THE LOAD-BEARING PROPERTY, and the reason this file exists at all:
//
//	If an admin route reused AGENT authentication, an AGENT credential would
//	authorise minting the credentials that CREATE AGENTS: any enrolled agent
//	could mint itself an unlimited supply of new identities, which collapses
//	invariant 3 completely. So the principal must be distinct in KIND, not
//	merely in permission.
//
// TestOperatorPrincipalEnrolledAgentCannotAuthenticateAsOperator is the test
// that measures that sentence, and it is written to go RED if the distinction is
// removed by ANY of the three routes available: merging the two session tables,
// overlapping the two id namespaces, or accepting an agent's credential on the
// operator path. It uses a REAL enrolment through auth.Service — invite-gated,
// with a real client-certificate fingerprint — and a REAL Ed25519 handshake on
// both planes, because a stub on either side would prove only that two stubs
// disagree.
//
// Every test here is prefixed TestOperatorPrincipal so one -run regex covers the
// set. Nothing sleeps: expiry is driven through the injected clock.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// opFakeLog is an auth.DurableWriter that applies what it is handed, in the
// order it is handed it — the observable half of wal.Log.Write's contract
// ("durable AND visible in memory") without a data directory and two fsyncs per
// case.
//
// It calls the applier from inside Write, exactly as wal.Txn.Commit does, which
// is what makes it able to catch the deadlock a single-mutex registry would
// have.
type opFakeLog struct {
	mu      sync.Mutex
	applier wal.Applier
	entries []wal.Entry
	index   uint64

	// writeErr, when non-nil, fails every Write BEFORE applying anything, so a
	// test can assert that a failed durable write leaves memory untouched.
	writeErr error
}

func (l *opFakeLog) Write(e wal.Entry) (wal.Committed, error) {
	l.mu.Lock()
	if l.writeErr != nil {
		err := l.writeErr
		l.mu.Unlock()
		return wal.Committed{}, err
	}
	l.index += 2
	c := wal.Committed{PrepareIndex: l.index - 1, CommitIndex: l.index, Entry: e}
	l.entries = append(l.entries, e)
	applier := l.applier
	l.mu.Unlock()

	if applier != nil {
		if err := applier.Apply(c); err != nil {
			return wal.Committed{}, err
		}
	}
	return c, nil
}

func (l *opFakeLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// opCompositeRoster is a Roster that CAN take the composite enrol+invite write,
// which auth.Service requires when RequireInvite is on. It is the smallest thing
// that lets this file enrol a REAL agent through the invite-gated path.
type opCompositeRoster struct {
	mu   sync.Mutex
	byID map[string]auth.RosterEntry
}

func newOpCompositeRoster() *opCompositeRoster {
	return &opCompositeRoster{byID: make(map[string]auth.RosterEntry)}
}

func (r *opCompositeRoster) Put(e auth.RosterEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[e.AgentID]; ok {
		return auth.ErrDuplicateAgentID
	}
	r.byID[e.AgentID] = e
	return nil
}

func (r *opCompositeRoster) PutWithInvite(e auth.RosterEntry, _ auth.InviteRider) (bool, error) {
	return true, r.Put(e)
}

func (r *opCompositeRoster) Get(agentID string) (auth.RosterEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[agentID]
	return e, ok
}

func (r *opCompositeRoster) Remove(agentID string, _ time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[agentID]; !ok {
		return false, nil
	}
	delete(r.byID, agentID)
	return true, nil
}

func (r *opCompositeRoster) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

func (r *opCompositeRoster) List() []auth.RosterEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]auth.RosterEntry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	return out
}

// AgentIDForCertFingerprint implements the fail-closed rule the shipped rosters
// have. It is written out rather than stubbed so that the agent-plane
// cross-check in this file gets the REAL answer instead of a lie.
func (r *opCompositeRoster) AgentIDForCertFingerprint(fp [32]byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fp == ([32]byte{}) {
		return "", auth.ErrCertBindingUnknown
	}
	var holders []string
	for id, e := range r.byID {
		for _, b := range e.CertBindings {
			if b.RetiredAt == nil && b.Fingerprint == fp {
				holders = append(holders, id)
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
// shipped rosters have (see authKeyOwner). Written out so the agent-plane
// enrolment in this file gets the real answer instead of a lie.
func (r *opCompositeRoster) AgentIDForAuthKey(key ed25519.PublicKey) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var holders []string
	for id, e := range r.byID {
		if e.AuthPublicKey.Equal(key) {
			holders = append(holders, id)
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

// opFakeInvite is the minimal auth.InviteRedemption the invite-gated enrolment
// path needs. internal/invite's own suite proves what a real participant does;
// what matters here is only that the enrolment is genuinely gated.
type opFakeInvite struct{ id string }

func (f *opFakeInvite) InviteID() string  { return f.id }
func (f *opFakeInvite) RiderKind() string { return "invite" }
func (f *opFakeInvite) Consume(res auth.EnrolResult) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"` + f.id + `","redeemed_by":"` + res.AgentID + `"}`), nil
}
func (f *opFakeInvite) Commit() {}
func (f *opFakeInvite) Abort()  {}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// opFP builds a distinct, NON-ZERO fingerprint. Non-zero matters everywhere: the
// zero [32]byte is the ABSENCE of a certificate, so a test using it could pass
// on a code path that compared nothing at all.
func opFP(n byte) [32]byte {
	var fp [32]byte
	for i := range fp {
		fp[i] = n
	}
	return fp
}

// opRegistry builds an attached OperatorRegistry over an opFakeLog, plus the
// OperatorService over it and the fake clock both share.
func opRegistry(t *testing.T) (*auth.OperatorRegistry, *auth.OperatorService, *fakeClock, *opFakeLog) {
	t.Helper()
	return opRegistryWithLogger(t, logging.New(&bytes.Buffer{}, logging.LevelError))
}

func opRegistryWithLogger(t *testing.T, lg *logging.Logger) (*auth.OperatorRegistry, *auth.OperatorService, *fakeClock, *opFakeLog) {
	t.Helper()
	reg := auth.NewOperatorRegistry(lg)
	log := &opFakeLog{applier: reg}
	if err := reg.Attach(log); err != nil {
		t.Fatalf("attaching the operator registry: %v", err)
	}
	clock := newFakeClock()
	svc, err := auth.NewOperatorService(reg, auth.OperatorOptions{Now: clock.Now})
	if err != nil {
		t.Fatalf("building the operator service: %v", err)
	}
	return reg, svc, clock, log
}

// opAdd registers an operator with a fresh keypair and returns its id and the
// PRIVATE half, which only the operator ever holds.
func opAdd(t *testing.T, reg *auth.OperatorRegistry, name string, fp [32]byte) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv := newKeypair(t)
	id, err := auth.MintOperatorID(testBusID, name)
	if err != nil {
		t.Fatalf("minting an operator id for %q: %v", name, err)
	}
	if err := reg.Add(auth.Operator{
		OperatorID:      id,
		Name:            name,
		AuthPublicKey:   pub,
		CertFingerprint: fp,
		CreatedAt:       epoch,
	}); err != nil {
		t.Fatalf("adding operator %q: %v", name, err)
	}
	return id, priv
}

// opActiveSession drives a REAL challenge/response to an ACTIVE session and
// returns the token. A hand-built token would prove nothing about the signature
// path, which is the half of invariant 3 this plane inherits.
func opActiveSession(t *testing.T, svc *auth.OperatorService, operatorID string, priv ed25519.PrivateKey, fp [32]byte) string {
	t.Helper()
	ch, err := svc.BeginSession(operatorID, fp)
	if err != nil {
		t.Fatalf("beginning an operator session for %q: %v", operatorID, err)
	}
	sig := ed25519.Sign(priv, []byte(auth.OperatorSessionSigningContext+ch.Token))
	sess, err := svc.CompleteSession(ch.Token, sig, fp)
	if err != nil {
		t.Fatalf("completing the operator session for %q: %v", operatorID, err)
	}
	if sess.State != auth.SessionActive {
		t.Fatalf("operator session state = %v, want active", sess.State)
	}
	return ch.Token
}

// ---------------------------------------------------------------------------
// THE test
// ---------------------------------------------------------------------------

// TestOperatorPrincipalEnrolledAgentCannotAuthenticateAsOperator is the guard
// for the whole task.
//
// # THE MUTATIONS IT IS WRITTEN TO CATCH
//
//   - MERGE THE SESSION TABLES (make OperatorService.Authenticate look in
//     Service.sessions, or vice versa): the first two subtests go red, because a
//     genuine agent token would then resolve on the operator plane.
//   - OVERLAP THE ID NAMESPACES (drop the "op:" prefix check in ParseOperatorID,
//     or teach ids.ParseAgentID to accept one): the last two subtests go red.
//   - ACCEPT AN AGENT ON THE OPERATOR PATH (have BeginSession consult the
//     roster, or Authenticate fall back to it): subtest three goes red.
//
// Everything in it is REAL: an invite-gated enrolment through auth.Service with
// a client-certificate fingerprint, a real Ed25519 signature over the agent
// signing context, a real operator record and a real Ed25519 signature over the
// operator signing context.
func TestOperatorPrincipalEnrolledAgentCannotAuthenticateAsOperator(t *testing.T) {
	t.Parallel()

	agentFP := opFP(0x11)
	operatorFP := opFP(0x22)

	// --- the AGENT plane: a real, invite-gated enrolment and a real session ---
	roster := newOpCompositeRoster()
	agentSvc, agentClock := newService(t, auth.Options{Roster: roster, RequireInvite: true})

	agentPub, agentPriv := newKeypair(t)
	enrolled, err := agentSvc.Enrol(auth.EnrolRequest{
		Name:                  "worker",
		PublicKey:             agentPub,
		IdempotencyKey:        "enrol-1",
		Invite:                &opFakeInvite{id: "inv-abc"},
		ClientCertFingerprint: &agentFP,
	})
	if err != nil {
		t.Fatalf("enrolling the agent through the invite gate: %v", err)
	}
	agentID := enrolled.AgentID

	agentChallenge, err := agentSvc.BeginSession(agentID)
	if err != nil {
		t.Fatalf("beginning the agent session: %v", err)
	}
	agentToken := agentChallenge.Token
	if _, err := agentSvc.CompleteSession(agentToken, ed25519.Sign(agentPriv, []byte(auth.SessionSigningContext+agentToken))); err != nil {
		t.Fatalf("completing the agent session: %v", err)
	}
	// The agent's own token authenticates the agent — otherwise the negative
	// results below would be worthless.
	if _, err := agentSvc.Authenticate(agentToken); err != nil {
		t.Fatalf("the agent's own token must authenticate on the agent plane, got: %v", err)
	}
	_ = agentClock

	// --- the OPERATOR plane: a real operator and a real session ---
	reg, opSvc, _, _ := opRegistry(t)
	operatorID, operatorPriv := opAdd(t, reg, "ops", operatorFP)
	operatorToken := opActiveSession(t, opSvc, operatorID, operatorPriv, operatorFP)
	if _, err := opSvc.Authenticate(operatorToken, operatorFP); err != nil {
		t.Fatalf("the operator's own token must authenticate on the operator plane, got: %v", err)
	}

	t.Run("agent token is refused by the operator authorization check", func(t *testing.T) {
		// Presented with the AGENT'S OWN certificate fingerprint, which is the
		// strongest form of the attack: everything the agent legitimately holds,
		// offered to the admin plane.
		if _, err := opSvc.Authenticate(agentToken, agentFP); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("OperatorService.Authenticate(agent token) error = %v, want ErrUnknownSession", err)
		}
		// And with the operator's fingerprint, in case a merged table were
		// reached only through a matching certificate.
		if _, err := opSvc.Authenticate(agentToken, operatorFP); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("OperatorService.Authenticate(agent token, operator cert) error = %v, want ErrUnknownSession", err)
		}
	})

	t.Run("operator token is refused by the agent authentication check", func(t *testing.T) {
		if _, err := agentSvc.Authenticate(operatorToken); !errors.Is(err, auth.ErrUnknownSession) {
			t.Fatalf("Service.Authenticate(operator token) error = %v, want ErrUnknownSession", err)
		}
	})

	t.Run("an agent id cannot begin an operator session", func(t *testing.T) {
		// Refused with the UNIFORM refusal (ErrOperatorCertMismatch): the agent's
		// own certificate does not resolve to any live operator, so the caller is
		// told nothing about whether "agentID" names anything on this plane. The
		// property under test is unchanged — an agent id can never begin an
		// operator session — only the refusal is now indistinguishable from every
		// other one, which is what stops BeginSession being an enumeration oracle.
		if _, err := opSvc.BeginSession(agentID, agentFP); !errors.Is(err, auth.ErrOperatorCertMismatch) {
			t.Fatalf("OperatorService.BeginSession(agent id) error = %v, want the uniform ErrOperatorCertMismatch refusal", err)
		}
		// And the same id presented over a REAL OPERATOR's certificate is refused
		// too: holding an operator certificate does not let you name an agent.
		if _, err := opSvc.BeginSession(agentID, operatorFP); !errors.Is(err, auth.ErrOperatorCertMismatch) {
			t.Fatalf("OperatorService.BeginSession(agent id, operator cert) error = %v, want the uniform ErrOperatorCertMismatch refusal", err)
		}
	})

	t.Run("an operator id cannot begin an agent session", func(t *testing.T) {
		if _, err := agentSvc.BeginSession(operatorID); !errors.Is(err, auth.ErrUnknownAgent) {
			t.Fatalf("Service.BeginSession(operator id) error = %v, want ErrUnknownAgent", err)
		}
	})

	t.Run("the id namespaces are structurally disjoint", func(t *testing.T) {
		operatorIDs := []string{
			operatorID,
			"op:" + testBusID + ".ops-aaaaaaaaaaaaaaaa",
			"op:" + testBusID + ".release-ops-2222222222222222",
			"op:bus-x.a-abcdefghijklmnop",
		}
		for _, id := range operatorIDs {
			if !auth.IsOperatorID(id) {
				t.Fatalf("auth.IsOperatorID(%q) = false, want true — the fixture is wrong, not the code", id)
			}
			if _, _, _, err := ids.ParseAgentID(id); err == nil {
				t.Fatalf("ids.ParseAgentID(%q) accepted an OPERATOR id; the namespaces have overlapped and an operator id could name an agent", id)
			}
		}

		agentIDs := []string{
			agentID,
			testBusID + ".worker-1",
			testBusID + ".code-reviewer-42",
			"bus-x.a-1",
			// THE ADVERSARIAL SHAPE, and the reason this list is not just the
			// obvious three: an agent suffix is decimal digits, and the digits
			// 2-7 are ALSO in the base32 alphabet an operator suffix uses. So
			// this is a perfectly valid AGENT id whose every component would
			// satisfy the operator grammar except the "op:" prefix. If that
			// prefix check is ever relaxed, THIS is the entry that goes red —
			// the other three would keep passing on the suffix-length rule and
			// hide the overlap.
			testBusID + ".ops-2222222222222222",
		}
		for _, id := range agentIDs {
			if _, _, _, err := ids.ParseAgentID(id); err != nil {
				t.Fatalf("ids.ParseAgentID(%q) = %v — the fixture is wrong, not the code", id, err)
			}
			if _, _, _, err := auth.ParseOperatorID(id); err == nil {
				t.Fatalf("auth.ParseOperatorID(%q) accepted an AGENT id; the namespaces have overlapped and an agent id could name an operator", id)
			}
			if auth.IsOperatorID(id) {
				t.Fatalf("auth.IsOperatorID(%q) = true for an agent id", id)
			}
		}
	})

	t.Run("the signing contexts are domain-separated", func(t *testing.T) {
		// A signature produced for the AGENT context must not complete an
		// OPERATOR challenge and vice versa. Without distinct contexts an
		// operator's signature could be harvested and replayed on the other
		// plane whenever the token bytes happened to match.
		if auth.SessionSigningContext == auth.OperatorSessionSigningContext {
			t.Fatal("the agent and operator session signing contexts are identical; a signature on one plane would verify on the other")
		}
		ch, err := opSvc.BeginSession(operatorID, operatorFP)
		if err != nil {
			t.Fatalf("beginning an operator session: %v", err)
		}
		wrongContext := ed25519.Sign(operatorPriv, []byte(auth.SessionSigningContext+ch.Token))
		if _, err := opSvc.CompleteSession(ch.Token, wrongContext, operatorFP); !errors.Is(err, auth.ErrBadSignature) {
			t.Fatalf("CompleteSession with an AGENT-context signature error = %v, want ErrBadSignature", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Revocation
// ---------------------------------------------------------------------------

// TestOperatorPrincipalRevokedOperatorIsRefusedOnTheVeryNextCall proves the
// property invariant 3 chose opaque server-side handles for: a revoked principal
// stops authenticating IMMEDIATELY, with no restart and no waiting for expiry.
//
// The second subtest is the SHARP one and the reason this test is not two lines.
// Revocation is protected by TWO mechanisms — the session sweep and the
// per-request registry re-read — and a test that only exercised
// OperatorRegistry.Revoke would pass with the re-read deleted, because the sweep
// alone would have emptied the table. That is exactly the "two independent
// mechanisms, so no single mutation goes red" trap this repo has been bitten by
// before. So the second subtest revokes through Apply — the RECOVERY path, which
// does NOT sweep sessions — leaving a genuinely live session in the table, and
// asserts that Authenticate refuses it anyway. Delete the re-read (cache
// "is revoked" on the session, or trust it from BeginSession) and it goes red on
// its own.
func TestOperatorPrincipalRevokedOperatorIsRefusedOnTheVeryNextCall(t *testing.T) {
	t.Parallel()

	t.Run("revoked through the registry", func(t *testing.T) {
		fp := opFP(0x31)
		reg, svc, clock, _ := opRegistry(t)
		id, priv := opAdd(t, reg, "ops", fp)
		token := opActiveSession(t, svc, id, priv, fp)

		if _, err := svc.Authenticate(token, fp); err != nil {
			t.Fatalf("before revocation the operator must authenticate, got: %v", err)
		}
		if _, err := reg.Revoke(id, "laptop stolen", clock.Now()); err != nil {
			t.Fatalf("revoking %q: %v", id, err)
		}
		if _, err := svc.Authenticate(token, fp); !errors.Is(err, auth.ErrUnknownSession) && !errors.Is(err, auth.ErrOperatorRevoked) {
			t.Fatalf("after revocation Authenticate error = %v, want ErrUnknownSession (session dropped) or ErrOperatorRevoked", err)
		}
		// And a NEW session cannot be established either — the begin path
		// refuses a revoked operator before it allocates anything.
		//
		// The sentinel is ErrOperatorCertMismatch rather than ErrOperatorRevoked
		// and that is the FIX, not a regression: a revoked operator's fingerprint
		// is no longer a live binding, so the certificate does not resolve and the
		// caller gets BeginSession's ONE uniform refusal. Naming the revocation
		// here used to leak the exact revocation instant to anybody who could
		// complete a TLS handshake — see
		// TestOperatorPrincipalBeginSessionIsNotAnEnumerationOracle.
		if _, err := svc.BeginSession(id, fp); !errors.Is(err, auth.ErrOperatorCertMismatch) {
			t.Fatalf("BeginSession for a revoked operator error = %v, want the uniform ErrOperatorCertMismatch refusal", err)
		}
	})

	t.Run("revoked with the session still in the table", func(t *testing.T) {
		fp := opFP(0x34)
		reg, svc, clock, _ := opRegistry(t)
		id, priv := opAdd(t, reg, "ops", fp)
		token := opActiveSession(t, svc, id, priv, fp)

		live, ok := reg.Get(id)
		if !ok {
			t.Fatal("the operator vanished")
		}
		revokedAt := clock.Now().Add(time.Minute)
		live.RevokedAt = &revokedAt
		live.RevokedReason = "revoked on another node, replayed here"
		body, err := auth.EncodeOperator(live)
		if err != nil {
			t.Fatalf("encoding the revocation: %v", err)
		}
		// Apply, not Revoke: the recovery path folds the record in WITHOUT
		// touching the session table.
		if err := reg.Apply(wal.Committed{PrepareIndex: 10, CommitIndex: 11, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := svc.SessionCount(); got != 1 {
			t.Fatalf("session count = %d, want 1 — this subtest is worthless unless the session is still live in the table", got)
		}
		if _, err := svc.Authenticate(token, fp); !errors.Is(err, auth.ErrOperatorRevoked) {
			t.Fatalf("Authenticate over a LIVE session whose operator is revoked = %v, want ErrOperatorRevoked; the check must RE-READ the registry on every call, with no cache", err)
		}
	})
}

// TestOperatorPrincipalRevokeSessionsKillsALiveTokenSynchronously pins the OTHER
// half of revocation: the table entry is gone the moment Revoke returns, not at
// its natural expiry up to SessionLifetime later.
func TestOperatorPrincipalRevokeSessionsKillsALiveTokenSynchronously(t *testing.T) {
	t.Parallel()

	fp := opFP(0x32)
	reg, svc, clock, _ := opRegistry(t)
	id, priv := opAdd(t, reg, "ops", fp)
	_ = opActiveSession(t, svc, id, priv, fp)
	_ = opActiveSession(t, svc, id, priv, fp)

	if got := svc.SessionCount(); got != 2 {
		t.Fatalf("session count before revocation = %d, want 2", got)
	}
	if _, err := reg.Revoke(id, "left the team", clock.Now()); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if got := svc.SessionCount(); got != 0 {
		t.Fatalf("session count after revocation = %d, want 0; OperatorRegistry.Revoke must drop the operator's live sessions synchronously", got)
	}
}

// TestOperatorPrincipalRevokeIsAnAppendAndReRevokingWritesNothing covers
// invariant 6 (revocation is an append, never an in-place edit or a deletion)
// and invariant 10 (same key + same payload is a legitimate retry: return the
// original result, do not re-apply).
func TestOperatorPrincipalRevokeIsAnAppendAndReRevokingWritesNothing(t *testing.T) {
	t.Parallel()

	fp := opFP(0x33)
	reg, _, clock, log := opRegistry(t)
	id, _ := opAdd(t, reg, "ops", fp)

	if got := log.count(); got != 1 {
		t.Fatalf("records after add = %d, want 1", got)
	}
	first, err := reg.Revoke(id, "rotating credentials", clock.Now())
	if err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if got := log.count(); got != 2 {
		t.Fatalf("records after revoke = %d, want 2 — revocation must APPEND a record, never edit the first one in place", got)
	}
	if !first.Revoked() {
		t.Fatal("the returned record is not marked revoked")
	}
	// The registry still HOLDS the operator: revocation is not deletion.
	if _, ok := reg.Get(id); !ok {
		t.Fatal("the revoked operator disappeared from the registry; revocation is an append, not a deletion, and its id stays spent forever (invariant 1)")
	}
	if reg.Len() != 1 || reg.LiveLen() != 0 {
		t.Fatalf("Len=%d LiveLen=%d, want 1 and 0", reg.Len(), reg.LiveLen())
	}

	clock.Advance(time.Hour)
	second, err := reg.Revoke(id, "a different reason entirely", clock.Now())
	if err != nil {
		t.Fatalf("re-revoke must be a legitimate retry, got: %v", err)
	}
	if got := log.count(); got != 2 {
		t.Fatalf("records after RE-revoke = %d, want 2 — re-revoking must write nothing (invariant 10)", got)
	}
	if !second.RevokedAt.Equal(*first.RevokedAt) || second.RevokedReason != first.RevokedReason {
		t.Fatalf("re-revoke returned %v/%q, want the ORIGINAL %v/%q; a retry returns the original result and must not rewrite history",
			second.RevokedAt, second.RevokedReason, first.RevokedAt, first.RevokedReason)
	}
}

// ---------------------------------------------------------------------------
// Invariant 11's cross-check
// ---------------------------------------------------------------------------

// TestOperatorPrincipalCrossCheckRefusesAnotherOperatorsCertificate is invariant
// 11 applied UNNARROWED: a session token presented over a connection whose
// client certificate belongs to a DIFFERENT principal is rejected, even though
// both the token and the certificate are individually genuine.
func TestOperatorPrincipalCrossCheckRefusesAnotherOperatorsCertificate(t *testing.T) {
	t.Parallel()

	fpA, fpB := opFP(0x41), opFP(0x42)
	reg, svc, _, _ := opRegistry(t)
	idA, privA := opAdd(t, reg, "alice", fpA)
	_, privB := opAdd(t, reg, "bob", fpB)

	tokenA := opActiveSession(t, svc, idA, privA, fpA)
	_ = privB

	if _, err := svc.Authenticate(tokenA, fpB); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("Authenticate(alice's token, bob's certificate) error = %v, want ErrOperatorCertMismatch", err)
	}
	// An unknown-but-non-zero certificate is refused too: "not bound to anybody"
	// is not "no constraint applies".
	if _, err := svc.Authenticate(tokenA, opFP(0x43)); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("Authenticate(alice's token, an unenrolled certificate) error = %v, want ErrOperatorCertMismatch", err)
	}
}

// TestOperatorPrincipalZeroFingerprintIsRefusedEverywhere pins the fail-closed
// treatment of the value a caller holds when there was NO certificate. Resolving
// it would turn "this connection presented nothing" into "this connection is an
// ADMIN".
func TestOperatorPrincipalZeroFingerprintIsRefusedEverywhere(t *testing.T) {
	t.Parallel()

	var zero [32]byte
	fp := opFP(0x51)
	reg, svc, _, _ := opRegistry(t)
	id, priv := opAdd(t, reg, "ops", fp)
	token := opActiveSession(t, svc, id, priv, fp)

	if _, err := svc.Authenticate(token, zero); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("Authenticate with the zero fingerprint error = %v, want ErrOperatorCertMismatch", err)
	}
	if _, err := svc.BeginSession(id, zero); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("BeginSession with the zero fingerprint error = %v, want ErrOperatorCertMismatch", err)
	}
	if _, err := reg.LiveOperatorForCertFingerprint(zero); !errors.Is(err, auth.ErrOperatorCertUnknown) {
		t.Fatalf("LiveOperatorForCertFingerprint(zero) error = %v, want ErrOperatorCertUnknown", err)
	}
	// And it can never be STORED, which is the other direction of the same rule.
	if err := reg.Add(auth.Operator{
		OperatorID:    mustMintOperatorID(t, "zero"),
		Name:          "zero",
		AuthPublicKey: mustPub(t),
		CreatedAt:     epoch,
	}); !errors.Is(err, auth.ErrInvalidOperatorRecord) {
		t.Fatalf("Add with the zero fingerprint error = %v, want ErrInvalidOperatorRecord", err)
	}
}

// TestOperatorPrincipalAmbiguousFingerprintRefuses builds the state Add refuses
// to create — two LIVE operators holding one certificate — through Apply, the
// RECOVERY path, which replays records that are already durable and must not
// refuse them (invariant 6). The READ is the thing that has to decline to guess.
func TestOperatorPrincipalAmbiguousFingerprintRefuses(t *testing.T) {
	t.Parallel()

	fp := opFP(0x61)
	var logBuf bytes.Buffer
	reg, svc, _, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))

	idA, privA := opAdd(t, reg, "alice", fp)
	tokenA := opActiveSession(t, svc, idA, privA, fp)
	if _, err := svc.Authenticate(tokenA, fp); err != nil {
		t.Fatalf("before the duplicate, alice must authenticate: %v", err)
	}

	// Add REFUSES to create the second holder...
	dupPub, _ := newKeypair(t)
	idB := mustMintOperatorID(t, "bob")
	dup := auth.Operator{OperatorID: idB, Name: "bob", AuthPublicKey: dupPub, CertFingerprint: fp, CreatedAt: epoch}
	if err := reg.Add(dup); !errors.Is(err, auth.ErrOperatorCertBound) {
		t.Fatalf("Add of a second live holder error = %v, want ErrOperatorCertBound", err)
	}

	// ...but a log that already holds one recovers into exactly that state.
	body, err := auth.EncodeOperator(dup)
	if err != nil {
		t.Fatalf("encoding the duplicate: %v", err)
	}
	if err := reg.Apply(wal.Committed{PrepareIndex: 90, CommitIndex: 91, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply must never fail recovery: %v", err)
	}

	if _, err := reg.LiveOperatorForCertFingerprint(fp); !errors.Is(err, auth.ErrOperatorCertAmbiguous) {
		t.Fatalf("LiveOperatorForCertFingerprint over two holders error = %v, want ErrOperatorCertAmbiguous — it must NOT pick one", err)
	}
	if _, err := svc.Authenticate(tokenA, fp); err == nil {
		t.Fatal("Authenticate succeeded over an AMBIGUOUS certificate; a certificate held by two operators must resolve to nobody")
	}
}

// ---------------------------------------------------------------------------
// Session mechanics
// ---------------------------------------------------------------------------

// TestOperatorPrincipalExpiredSessionIsRefusedWithNoSkewGrace pins that expiry is
// measured against the server's own clock and that the boundary instant counts
// as expired. A grace window would be a longer lifetime with a less honest name.
func TestOperatorPrincipalExpiredSessionIsRefusedWithNoSkewGrace(t *testing.T) {
	t.Parallel()

	fp := opFP(0x71)
	reg, svc, clock, _ := opRegistry(t)
	id, priv := opAdd(t, reg, "ops", fp)
	token := opActiveSession(t, svc, id, priv, fp)

	clock.Advance(auth.SessionLifetime - time.Nanosecond)
	if _, err := svc.Authenticate(token, fp); err != nil {
		t.Fatalf("one nanosecond before expiry the session must still authenticate: %v", err)
	}
	clock.Advance(time.Nanosecond)
	if _, err := svc.Authenticate(token, fp); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("at exactly ExpiresAt error = %v, want ErrUnknownSession", err)
	}
}

// TestOperatorPrincipalPendingSessionIsNotACredential: an unsigned challenge is
// rejected exactly like an unknown token.
func TestOperatorPrincipalPendingSessionIsNotACredential(t *testing.T) {
	t.Parallel()

	fp := opFP(0x72)
	reg, svc, _, _ := opRegistry(t)
	id, _ := opAdd(t, reg, "ops", fp)

	ch, err := svc.BeginSession(id, fp)
	if err != nil {
		t.Fatalf("beginning a session: %v", err)
	}
	if _, err := svc.Authenticate(ch.Token, fp); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("Authenticate with a PENDING token error = %v, want ErrUnknownSession", err)
	}
}

// TestOperatorPrincipalCompleteSessionNeverExtendsExpiry: re-completing an
// already-active session returns it UNCHANGED (invariant 10), so one signature
// can never be turned into an unbounded session.
func TestOperatorPrincipalCompleteSessionNeverExtendsExpiry(t *testing.T) {
	t.Parallel()

	fp := opFP(0x73)
	reg, svc, clock, _ := opRegistry(t)
	id, priv := opAdd(t, reg, "ops", fp)

	ch, err := svc.BeginSession(id, fp)
	if err != nil {
		t.Fatalf("beginning a session: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(auth.OperatorSessionSigningContext+ch.Token))
	first, err := svc.CompleteSession(ch.Token, sig, fp)
	if err != nil {
		t.Fatalf("completing: %v", err)
	}
	clock.Advance(30 * time.Minute)
	second, err := svc.CompleteSession(ch.Token, sig, fp)
	if err != nil {
		t.Fatalf("re-completing must be a legitimate retry, got: %v", err)
	}
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("re-completion moved ExpiresAt from %s to %s; the expiry is set ONCE and never extended", first.ExpiresAt, second.ExpiresAt)
	}
}

// TestOperatorPrincipalWrongSizePublicKeyDoesNotPanic: ed25519.Verify PANICS on
// a public key that is not exactly ed25519.PublicKeySize, so the length check
// must precede it. The registry refuses to store one in both directions, which
// is what keeps a truncated record off the verification path.
func TestOperatorPrincipalWrongSizePublicKeyDoesNotPanic(t *testing.T) {
	t.Parallel()

	fp := opFP(0x74)
	reg, _, _, _ := opRegistry(t)

	for _, tc := range []struct {
		name string
		key  ed25519.PublicKey
	}{
		{"nil", nil},
		{"empty", ed25519.PublicKey{}},
		{"one byte short", make([]byte, ed25519.PublicKeySize-1)},
		{"one byte long", make([]byte, ed25519.PublicKeySize+1)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := reg.Add(auth.Operator{
				OperatorID:      mustMintOperatorID(t, "ops"),
				Name:            "ops",
				AuthPublicKey:   tc.key,
				CertFingerprint: fp,
				CreatedAt:       epoch,
			})
			if !errors.Is(err, auth.ErrInvalidPublicKey) {
				t.Fatalf("Add with a %s key error = %v, want ErrInvalidPublicKey", tc.name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The durable record
// ---------------------------------------------------------------------------

// TestOperatorPrincipalRecordRoundTrip pins the on-disk shape: the field names
// are FOREVER, and a record must survive encode/decode byte-identically.
func TestOperatorPrincipalRecordRoundTrip(t *testing.T) {
	t.Parallel()

	pub, _ := newKeypair(t)
	revokedAt := epoch.Add(time.Hour)
	for _, tc := range []struct {
		name string
		op   auth.Operator
	}{
		{"live", auth.Operator{
			OperatorID: mustMintOperatorID(t, "ops"), Name: "ops",
			AuthPublicKey: pub, CertFingerprint: opFP(0x81), CreatedAt: epoch,
		}},
		{"live with a label", auth.Operator{
			OperatorID: mustMintOperatorID(t, "ops"), Name: "ops",
			AuthPublicKey: pub, CertFingerprint: opFP(0x82), Label: "mike, laptop", CreatedAt: epoch,
		}},
		{"revoked", auth.Operator{
			OperatorID: mustMintOperatorID(t, "ops"), Name: "ops",
			AuthPublicKey: pub, CertFingerprint: opFP(0x83), CreatedAt: epoch,
			RevokedAt: &revokedAt, RevokedReason: "laptop stolen",
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			raw, err := auth.EncodeOperator(tc.op)
			if err != nil {
				t.Fatalf("EncodeOperator: %v", err)
			}
			back, err := auth.DecodeOperator(raw)
			if err != nil {
				t.Fatalf("DecodeOperator: %v", err)
			}
			if back.OperatorID != tc.op.OperatorID || back.Name != tc.op.Name ||
				!back.AuthPublicKey.Equal(tc.op.AuthPublicKey) ||
				back.CertFingerprint != tc.op.CertFingerprint ||
				back.Label != tc.op.Label ||
				!back.CreatedAt.Equal(tc.op.CreatedAt) ||
				back.Revoked() != tc.op.Revoked() ||
				back.RevokedReason != tc.op.RevokedReason {
				t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", back, tc.op)
			}
			if tc.op.RevokedAt != nil && !back.RevokedAt.Equal(*tc.op.RevokedAt) {
				t.Fatalf("revoked_at round trip: got %v, want %v", back.RevokedAt, tc.op.RevokedAt)
			}

			// The FOREVER field names, asserted by name so a rename is a test
			// failure rather than an unreadable log six months from now.
			var raw2 map[string]json.RawMessage
			if err := json.Unmarshal(raw, &raw2); err != nil {
				t.Fatalf("the encoded record is not a JSON object: %v", err)
			}
			for _, key := range []string{"v", "operator_id", "name", "auth_pub", "cert_fp", "created_at"} {
				if _, ok := raw2[key]; !ok {
					t.Fatalf("the encoded record is missing the %q field; these field names are FOREVER", key)
				}
			}
			if _, ok := raw2["revoked_at"]; ok != tc.op.Revoked() {
				t.Fatalf("revoked_at present = %v, want %v; it is OMITTED ENTIRELY while live, so \"live\" and \"revoked at the zero time\" cannot be confused", ok, tc.op.Revoked())
			}
		})
	}
}

// TestOperatorPrincipalRecordRejectsInvalidFields: every validation rule, in
// both directions. Each case is a record that must NOT be storable and must not
// be trustable if it somehow reached disk.
func TestOperatorPrincipalRecordRejectsInvalidFields(t *testing.T) {
	t.Parallel()

	pub, _ := newKeypair(t)
	id := mustMintOperatorID(t, "ops")
	zeroTime := time.Time{}
	revokedAt := epoch.Add(time.Hour)

	base := auth.Operator{
		OperatorID: id, Name: "ops", AuthPublicKey: pub,
		CertFingerprint: opFP(0x91), CreatedAt: epoch,
	}
	mutate := func(f func(*auth.Operator)) auth.Operator {
		o := base
		o.AuthPublicKey = append(ed25519.PublicKey(nil), base.AuthPublicKey...)
		f(&o)
		return o
	}

	for _, tc := range []struct {
		name string
		op   auth.Operator
		want error
	}{
		{"empty id", mutate(func(o *auth.Operator) { o.OperatorID = "" }), auth.ErrInvalidOperatorRecord},
		{"an AGENT id", mutate(func(o *auth.Operator) { o.OperatorID = testBusID + ".ops-1" }), auth.ErrInvalidOperatorRecord},
		{"no op: prefix", mutate(func(o *auth.Operator) { o.OperatorID = testBusID + ".ops-aaaaaaaaaaaaaaaa" }), auth.ErrInvalidOperatorRecord},
		{"name disagrees with the id", mutate(func(o *auth.Operator) { o.Name = "somebodyelse" }), auth.ErrInvalidOperatorRecord},
		{"zero fingerprint", mutate(func(o *auth.Operator) { o.CertFingerprint = [32]byte{} }), auth.ErrInvalidOperatorRecord},
		{"wrong-size key", mutate(func(o *auth.Operator) { o.AuthPublicKey = make([]byte, 8) }), auth.ErrInvalidPublicKey},
		{"zero created_at", mutate(func(o *auth.Operator) { o.CreatedAt = zeroTime }), auth.ErrInvalidOperatorRecord},
		{"oversized label", mutate(func(o *auth.Operator) { o.Label = strings.Repeat("x", auth.MaxOperatorLabelLen+1) }), auth.ErrInvalidOperatorRecord},
		{"revoked at the zero time", mutate(func(o *auth.Operator) { o.RevokedAt = &zeroTime; o.RevokedReason = "why" }), auth.ErrInvalidOperatorRecord},
		{"revoked with no reason", mutate(func(o *auth.Operator) { o.RevokedAt = &revokedAt }), auth.ErrInvalidOperatorRecord},
		{"a reason with no revocation", mutate(func(o *auth.Operator) { o.RevokedReason = "why" }), auth.ErrInvalidOperatorRecord},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.EncodeOperator(tc.op); !errors.Is(err, tc.want) {
				t.Fatalf("EncodeOperator error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestOperatorPrincipalDecodeRejectsUnknownFieldsAndVersions: a record off disk
// is UNTRUSTED INPUT even though this server wrote it.
func TestOperatorPrincipalDecodeRejectsUnknownFieldsAndVersions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"unknown field", `{"v":1,"operator_id":"op:bus-x.ops-aaaaaaaaaaaaaaaa","name":"ops","auth_pub":"","cert_fp":"","created_at":"2026-08-02T12:00:00Z","surprise":1}`},
		{"future version", `{"v":2,"operator_id":"op:bus-x.ops-aaaaaaaaaaaaaaaa","name":"ops","auth_pub":"","cert_fp":"","created_at":"2026-08-02T12:00:00Z"}`},
		{"not json", `{`},
		{"trailing data", `{"v":1,"operator_id":"op:bus-x.ops-aaaaaaaaaaaaaaaa","name":"ops","auth_pub":"","cert_fp":"","created_at":"2026-08-02T12:00:00Z"} {}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.DecodeOperator(json.RawMessage(tc.raw)); !errors.Is(err, auth.ErrInvalidOperatorRecord) {
				t.Fatalf("DecodeOperator error = %v, want ErrInvalidOperatorRecord", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Recovery
// ---------------------------------------------------------------------------

// TestOperatorPrincipalApplySurvivesADamagedRecordAndLogsIt is invariant 6:
// recovery ALWAYS reaches a running server, damaged records are discarded, and
// EVERY discard is logged LOUDLY AND SPECIFICALLY — silent discard is the
// defect.
func TestOperatorPrincipalApplySurvivesADamagedRecordAndLogsIt(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	reg, _, _, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))

	damaged := wal.Committed{
		PrepareIndex: 40, CommitIndex: 41,
		Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: json.RawMessage(`{"v":1,"operator_id":"nonsense"}`)},
	}
	if err := reg.Apply(damaged); err != nil {
		t.Fatalf("Apply returned %v; a damaged record must be DISCARDED, not turned into an outage (invariant 6)", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("the damaged record was stored: Len = %d", reg.Len())
	}
	out := logBuf.String()
	if !strings.Contains(out, "DISCARDING") {
		t.Fatalf("the discard was not logged loudly; log was:\n%s", out)
	}
	if !strings.Contains(out, "41") {
		t.Fatalf("the discard log does not name the commit index, so an operator cannot find the record; log was:\n%s", out)
	}

	// A neighbour's record is skipped SILENTLY — treating it as damage would
	// fill the log with false alarms.
	logBuf.Reset()
	if err := reg.Apply(wal.Committed{PrepareIndex: 42, CommitIndex: 43, Entry: wal.Entry{Kind: "message", Body: json.RawMessage(`{"anything":true}`)}}); err != nil {
		t.Fatalf("Apply of another kind: %v", err)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("a neighbour's record produced log output:\n%s", logBuf.String())
	}
}

// TestOperatorPrincipalApplyKeepsTheFirstRevocation: nothing supersedes a
// revocation, and a duplicate live record never overwrites.
func TestOperatorPrincipalApplyKeepsTheFirstRevocation(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	reg, _, clock, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))
	fp := opFP(0xA1)
	id, _ := opAdd(t, reg, "ops", fp)
	if _, err := reg.Revoke(id, "first", clock.Now()); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	// A LIVE record for the same id, replayed after the revocation, must not
	// resurrect the principal.
	live, ok := reg.Get(id)
	if !ok {
		t.Fatal("the operator vanished")
	}
	live.RevokedAt = nil
	live.RevokedReason = ""
	body, err := auth.EncodeOperator(live)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if err := reg.Apply(wal.Committed{PrepareIndex: 70, CommitIndex: 71, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := reg.Get(id)
	if !got.Revoked() {
		t.Fatal("a replayed LIVE record un-revoked a revoked operator; revocation is permanent (invariant 1: the id is never reused)")
	}
	if !strings.Contains(logBuf.String(), "UN-REVOKE") {
		t.Fatalf("the un-revoke attempt was not logged; log was:\n%s", logBuf.String())
	}
}

// TestOperatorPrincipalAddBeforeAttachIsRefused: a registry with no log must
// never acknowledge an operator that never reached disk (invariant 4).
func TestOperatorPrincipalAddBeforeAttachIsRefused(t *testing.T) {
	t.Parallel()

	reg := auth.NewOperatorRegistry(nil)
	pub, _ := newKeypair(t)
	err := reg.Add(auth.Operator{
		OperatorID: mustMintOperatorID(t, "ops"), Name: "ops",
		AuthPublicKey: pub, CertFingerprint: opFP(0xB1), CreatedAt: epoch,
	})
	if !errors.Is(err, auth.ErrNotAttached) {
		t.Fatalf("Add before Attach error = %v, want ErrNotAttached", err)
	}
	if _, err := reg.Revoke("op:"+testBusID+".ops-aaaaaaaaaaaaaaaa", "why", epoch); !errors.Is(err, auth.ErrNotAttached) {
		t.Fatalf("Revoke before Attach error = %v, want ErrNotAttached", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("something reached memory with no log behind it: Len = %d", reg.Len())
	}
}

// TestOperatorPrincipalDuplicateIDIsRefused: an operator id is never reused
// (invariant 1) and an overwrite would rebind a live ADMIN identity to a
// different keypair.
func TestOperatorPrincipalDuplicateIDIsRefused(t *testing.T) {
	t.Parallel()

	reg, _, _, _ := opRegistry(t)
	id, _ := opAdd(t, reg, "ops", opFP(0xC1))

	pub, _ := newKeypair(t)
	err := reg.Add(auth.Operator{
		OperatorID: id, Name: "ops", AuthPublicKey: pub,
		CertFingerprint: opFP(0xC2), CreatedAt: epoch,
	})
	if !errors.Is(err, auth.ErrDuplicateOperatorID) {
		t.Fatalf("Add of a duplicate id error = %v, want ErrDuplicateOperatorID", err)
	}
	if _, err := reg.Revoke("op:"+testBusID+".nobody-aaaaaaaaaaaaaaaa", "why", epoch); !errors.Is(err, auth.ErrUnknownOperator) {
		t.Fatalf("Revoke of an unknown operator error = %v, want ErrUnknownOperator", err)
	}
}

// ---------------------------------------------------------------------------
// Ids
// ---------------------------------------------------------------------------

// TestOperatorPrincipalMintAndParse covers the id grammar itself: what is
// accepted, what is refused, and that a mint is never predictable.
func TestOperatorPrincipalMintAndParse(t *testing.T) {
	t.Parallel()

	t.Run("mint produces a parseable, unpredictable id", func(t *testing.T) {
		seen := make(map[string]bool)
		for i := 0; i < 32; i++ {
			id, err := auth.MintOperatorID(testBusID, "ops")
			if err != nil {
				t.Fatalf("MintOperatorID: %v", err)
			}
			if seen[id] {
				t.Fatalf("MintOperatorID returned the same id twice (%q); ids are never reused (invariant 1)", id)
			}
			seen[id] = true
			busID, name, suffix, err := auth.ParseOperatorID(id)
			if err != nil {
				t.Fatalf("ParseOperatorID(%q): %v", id, err)
			}
			if busID != testBusID || name != "ops" || len(suffix) != 16 {
				t.Fatalf("ParseOperatorID(%q) = %q/%q/%q", id, busID, name, suffix)
			}
		}
	})

	t.Run("names", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ok   bool
		}{
			{"ops", true},
			{"release-ops", true},
			{"a", true},
			{"0ps", true},
			{"", false},
			{"Ops", false},
			{"-ops", false},
			{"o.ps", false},
			{"o:ps", false},
			{strings.Repeat("a", 65), false},
		} {
			err := auth.ValidateOperatorName(tc.name)
			if tc.ok && err != nil {
				t.Fatalf("ValidateOperatorName(%q) = %v, want nil", tc.name, err)
			}
			if !tc.ok && !errors.Is(err, auth.ErrInvalidOperatorName) {
				t.Fatalf("ValidateOperatorName(%q) = %v, want ErrInvalidOperatorName", tc.name, err)
			}
		}
	})

	t.Run("malformed ids are refused", func(t *testing.T) {
		for _, id := range []string{
			"",
			"ops",
			"op:",
			"op:" + testBusID,
			"op:" + testBusID + ".ops",
			"op:" + testBusID + ".ops-",
			"op:" + testBusID + ".ops-short",
			"op:" + testBusID + ".ops-AAAAAAAAAAAAAAAA",  // uppercase base32
			"op:" + testBusID + ".ops-0000000000000000",  // 0 and 1 are outside the alphabet
			"op:" + testBusID + ".Ops-aaaaaaaaaaaaaaaa",  // uppercase name
			"op:.ops-aaaaaaaaaaaaaaaa",                   // empty bus id
			"OP:" + testBusID + ".ops-aaaaaaaaaaaaaaaa",  // uppercase prefix
			" op:" + testBusID + ".ops-aaaaaaaaaaaaaaaa", // leading space
			strings.Repeat("x", auth.MaxOperatorIDLen+1),
		} {
			if _, _, _, err := auth.ParseOperatorID(id); err == nil {
				t.Fatalf("ParseOperatorID(%q) accepted a malformed id", id)
			}
		}
	})

	t.Run("the name pattern matches the agent one today", func(t *testing.T) {
		// Declared separately on purpose (see OperatorNamePattern), so a
		// divergence must be a decision somebody makes rather than one that
		// happens by accident.
		if auth.OperatorNamePattern != ids.AgentNamePattern {
			t.Fatalf("OperatorNamePattern = %q, ids.AgentNamePattern = %q; if this divergence is deliberate, record it in DECISIONS.md and update this test",
				auth.OperatorNamePattern, ids.AgentNamePattern)
		}
	})
}

// ---------------------------------------------------------------------------
// The MUTATIONS that survived the first round
//
// Each test below was written against a SPECIFIC mutation that a reviewer
// applied to a clean overlay and watched stay GREEN. The mutation each one
// catches is named in its doc comment, in the form it was applied, so that a
// later reader can re-run the experiment rather than trust this sentence.
// ---------------------------------------------------------------------------

// opServiceWith builds an attached registry plus a service with EXPLICIT caps,
// which the shared opRegistry fixture cannot express.
func opServiceWith(t *testing.T, opts auth.OperatorOptions) (*auth.OperatorRegistry, *auth.OperatorService, *fakeClock) {
	t.Helper()
	reg := auth.NewOperatorRegistry(logging.New(&bytes.Buffer{}, logging.LevelError))
	if err := reg.Attach(&opFakeLog{applier: reg}); err != nil {
		t.Fatalf("attaching the operator registry: %v", err)
	}
	clock := newFakeClock()
	opts.Now = clock.Now
	svc, err := auth.NewOperatorService(reg, opts)
	if err != nil {
		t.Fatalf("building the operator service: %v", err)
	}
	return reg, svc, clock
}

// TestOperatorPrincipalBeginSessionRequiresThatOperatorsCertificate is MUTATION
// 1: delete the certificate resolution from BeginSession (keeping only the
// zero-fingerprint refusal) and this goes RED.
//
// It is the property BeginSession's doc derives BOTH of its security claims
// from — not an enumeration oracle, and safe to key a per-operator cap on — so
// nothing else in this file is sound if it does not hold. Holding operator X's
// certificate must not let X begin a session as operator Y, even though X is a
// perfectly legitimate operator with a perfectly valid certificate.
func TestOperatorPrincipalBeginSessionRequiresThatOperatorsCertificate(t *testing.T) {
	t.Parallel()

	fpA, fpB := opFP(0x81), opFP(0x82)
	reg, svc, _, _ := opRegistry(t)
	idA, _ := opAdd(t, reg, "alice", fpA)
	idB, privB := opAdd(t, reg, "bob", fpB)

	// Alice's certificate, Bob's id.
	if _, err := svc.BeginSession(idB, fpA); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("BeginSession(bob, ALICE's certificate) error = %v, want ErrOperatorCertMismatch — one operator's certificate must never begin another operator's session", err)
	}
	// And the other direction, so the test cannot pass on an accident of ordering.
	if _, err := svc.BeginSession(idA, fpB); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("BeginSession(alice, BOB's certificate) error = %v, want ErrOperatorCertMismatch", err)
	}
	// A certificate no operator holds is refused too.
	if _, err := svc.BeginSession(idA, opFP(0x83)); !errors.Is(err, auth.ErrOperatorCertMismatch) {
		t.Fatalf("BeginSession(alice, an unbound certificate) error = %v, want ErrOperatorCertMismatch", err)
	}
	// NOTHING was allocated by any of the three refusals: a refusal that still
	// took a table slot would be the lockout primitive the caps exist to prevent.
	if got := svc.SessionCount(); got != 0 {
		t.Fatalf("session count after three refused begins = %d, want 0; a refusal must leave the table exactly as it found it", got)
	}
	// The legitimate case still works, so the test is not passing by refusing
	// everything.
	if _, err := svc.BeginSession(idB, fpB); err != nil {
		t.Fatalf("BeginSession(bob, bob's certificate) must succeed: %v", err)
	}
	_ = privB
}

// TestOperatorPrincipalBeginSessionIsNotAnEnumerationOracle is the test for the
// A1 fix: the three refusals an unauthenticated caller can reach are now ONE
// sentinel with ONE message.
//
// Before the fix a caller who could merely complete a TLS handshake got:
//
//	LIVE     -> ErrOperatorCertMismatch naming the operator id
//	REVOKED  -> ErrOperatorRevoked, naming the id AND the exact revocation instant
//	UNKNOWN  -> ErrUnknownOperator, naming the id
//
// which is a roster enumerator plus a timestamp leak.
func TestOperatorPrincipalBeginSessionIsNotAnEnumerationOracle(t *testing.T) {
	t.Parallel()

	reg, svc, clock, _ := opRegistry(t)
	liveID, _ := opAdd(t, reg, "alice", opFP(0x91))
	revokedID, _ := opAdd(t, reg, "bob", opFP(0x92))
	if _, err := reg.Revoke(revokedID, "laptop stolen", clock.Now()); err != nil {
		t.Fatalf("revoking bob: %v", err)
	}
	unknownID := mustMintOperatorID(t, "carol")
	malformedID := "not-an-operator-id"
	// The ATTACKER's certificate: well-formed, non-zero, bound to nobody.
	attackerFP := opFP(0x99)

	var messages []string
	for _, tc := range []struct {
		what string
		id   string
	}{
		{"live", liveID},
		{"revoked", revokedID},
		{"unknown", unknownID},
		{"malformed", malformedID},
	} {
		_, err := svc.BeginSession(tc.id, attackerFP)
		if err == nil {
			t.Fatalf("BeginSession(%s id, an unbound certificate) succeeded", tc.what)
		}
		if !errors.Is(err, auth.ErrOperatorCertMismatch) {
			t.Fatalf("BeginSession(%s id) sentinel = %v, want ErrOperatorCertMismatch for ALL of live/revoked/unknown/malformed; a distinguishable sentinel IS the oracle", tc.what, err)
		}
		msg := err.Error()
		// No id echoed: an id in the refusal confirms the caller's guess.
		for _, leak := range []string{liveID, revokedID, unknownID, "alice", "bob", "carol"} {
			if strings.Contains(msg, leak) {
				t.Fatalf("BeginSession(%s id) refusal echoes %q back to an unauthenticated caller: %s", tc.what, leak, msg)
			}
		}
		// No revocation instant: it used to be formatted straight into the error.
		if strings.Contains(msg, clock.Now().UTC().Format(time.RFC3339Nano)) || strings.Contains(msg, "20") {
			t.Fatalf("BeginSession(%s id) refusal appears to carry a timestamp, which leaks WHEN a principal was revoked: %s", tc.what, msg)
		}
		messages = append(messages, msg)
	}
	for i := 1; i < len(messages); i++ {
		if messages[i] != messages[0] {
			t.Fatalf("the refusals are DISTINGUISHABLE:\n  [0] %s\n  [%d] %s", messages[0], i, messages[i])
		}
	}
	// And a THIRD PARTY's live certificate produces the same sentence: holding
	// SOME operator certificate must not turn the refusal into an answer.
	otherID, _ := opAdd(t, reg, "dave", opFP(0x94))
	_ = otherID
	_, err := svc.BeginSession(liveID, opFP(0x94))
	if err == nil || err.Error() != messages[0] {
		t.Fatalf("BeginSession(alice, DAVE's live certificate) = %v, want the identical uniform refusal %q", err, messages[0])
	}
}

// TestOperatorPrincipalApplyNeverOverwritesALiveOperator is MUTATION 2, and the
// reviewer rated it the SHARPEST of the five: change Apply's duplicate-live arm
// to store the later record and it rebinds a LIVE ADMIN IDENTITY to a different
// keypair and a different certificate.
//
// TestOperatorPrincipalDuplicateIDIsRefused covers only Add. Apply is the
// security-critical path precisely BECAUSE it deliberately does not re-run the
// admission checks (invariant 6: a record reaching it is already durable), so
// the refusal has to be tested where it actually has to hold.
func TestOperatorPrincipalApplyNeverOverwritesALiveOperator(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	reg, svc, _, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))
	fp := opFP(0xD1)
	id, priv := opAdd(t, reg, "ops", fp)

	original, ok := reg.Get(id)
	if !ok {
		t.Fatal("the operator vanished")
	}

	// The impostor: SAME id, different keypair, different certificate.
	impostorPub, impostorPriv := newKeypair(t)
	impostorFP := opFP(0xD2)
	body, err := auth.EncodeOperator(auth.Operator{
		OperatorID: id, Name: "ops", AuthPublicKey: impostorPub,
		CertFingerprint: impostorFP, CreatedAt: epoch,
	})
	if err != nil {
		t.Fatalf("encoding the impostor record: %v", err)
	}
	if err := reg.Apply(wal.Committed{PrepareIndex: 200, CommitIndex: 201, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply must never fail recovery: %v", err)
	}

	got, _ := reg.Get(id)
	if !got.AuthPublicKey.Equal(original.AuthPublicKey) {
		t.Fatal("a replayed DUPLICATE record REBOUND a live admin identity to a different signing key; the FIRST record must be kept (invariants 1 and 3)")
	}
	if got.CertFingerprint != original.CertFingerprint {
		t.Fatal("a replayed DUPLICATE record REBOUND a live admin identity to a different client certificate")
	}
	if !strings.Contains(logBuf.String(), "DISCARDING") {
		t.Fatalf("the duplicate discard was not logged loudly (invariant 6); log was:\n%s", logBuf.String())
	}

	// The behavioural half: the ORIGINAL key holder still authenticates and the
	// impostor never does. A field comparison alone would pass on a registry that
	// kept the fields but authenticated the other key.
	if _, err := svc.BeginSession(id, impostorFP); err == nil {
		t.Fatal("the impostor's certificate began a session for an operator it never held")
	}
	token := opActiveSession(t, svc, id, priv, fp)
	if _, err := svc.Authenticate(token, fp); err != nil {
		t.Fatalf("the ORIGINAL operator must still authenticate after the duplicate was discarded: %v", err)
	}
	_ = impostorPriv
}

// TestOperatorPrincipalApplyRevocationCannotRebindCredentials is MUTATION 5:
// make Apply's revocation arm take the WHOLE record (r.byID[id] =
// copyOperator(o)) instead of only the revocation fields, and a record that
// SAYS "revoke" can swap the key and the certificate on its way past.
//
// Fail-closed on authority (the revocation IS applied) and fail-closed on
// identity (the credential is NOT rebound) are two separate promises, and only
// this test measures the second.
func TestOperatorPrincipalApplyRevocationCannotRebindCredentials(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	reg, _, clock, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))
	fp := opFP(0xE1)
	id, _ := opAdd(t, reg, "ops", fp)
	original, _ := reg.Get(id)

	attackerPub, _ := newKeypair(t)
	attackerFP := opFP(0xE2)
	revokedAt := clock.Now().Add(time.Minute)
	body, err := auth.EncodeOperator(auth.Operator{
		OperatorID: id, Name: "ops",
		AuthPublicKey:   attackerPub,
		CertFingerprint: attackerFP,
		CreatedAt:       epoch.Add(time.Hour),
		RevokedAt:       &revokedAt,
		RevokedReason:   "a revocation that also swaps the credential",
	})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if err := reg.Apply(wal.Committed{PrepareIndex: 210, CommitIndex: 211, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
		t.Fatalf("Apply must never fail recovery: %v", err)
	}

	got, _ := reg.Get(id)
	if !got.Revoked() {
		t.Fatal("the revocation was not applied; revocation is fail-CLOSED on authority")
	}
	if !got.AuthPublicKey.Equal(original.AuthPublicKey) {
		t.Fatal("a REVOCATION record rebound the operator's signing key; only the revocation fields may be taken")
	}
	if got.CertFingerprint != original.CertFingerprint {
		t.Fatal("a REVOCATION record rebound the operator's client certificate; only the revocation fields may be taken")
	}
	if !got.CreatedAt.Equal(original.CreatedAt) {
		t.Fatal("a REVOCATION record rewrote the operator's creation instant")
	}
	if !strings.Contains(logBuf.String(), "disagrees") {
		t.Fatalf("the disagreement was not reported; it can only be corruption or tampering (invariant 6). Log was:\n%s", logBuf.String())
	}
}

// TestOperatorPrincipalApplyLogsEverySilentDiscard is the A4 fix: two folds that
// used to change the serving copy, or refuse to, WITHOUT SAYING SO.
//
// Invariant 6's absolute half is that every discard is logged loudly and
// specifically — silent discard is the defect, not discard. Invariant 10 draws
// the other edge: a BYTE-IDENTICAL retry is legitimate and must stay silent, or
// the operator learns to ignore this exact message.
func TestOperatorPrincipalApplyLogsEverySilentDiscard(t *testing.T) {
	t.Parallel()

	t.Run("a second revocation that DISAGREES is reported", func(t *testing.T) {
		var logBuf bytes.Buffer
		reg, _, clock, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))
		id, _ := opAdd(t, reg, "ops", opFP(0xF1))
		if _, err := reg.Revoke(id, "first", clock.Now()); err != nil {
			t.Fatalf("revoking: %v", err)
		}
		stored, _ := reg.Get(id)

		// EXACTLY the stored record: a legitimate retry (invariant 10). SILENT.
		logBuf.Reset()
		same, err := auth.EncodeOperator(stored)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		if err := reg.Apply(wal.Committed{PrepareIndex: 220, CommitIndex: 221, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: same}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if logBuf.Len() != 0 {
			t.Fatalf("a byte-identical re-revocation was logged; it is a legitimate retry (invariant 10) and noise here trains an operator to ignore the real line:\n%s", logBuf.String())
		}

		// A DIFFERENT instant and reason for the same id: a contradiction.
		logBuf.Reset()
		other := stored
		otherAt := clock.Now().Add(time.Hour)
		other.RevokedAt = &otherAt
		other.RevokedReason = "a different story"
		body, err := auth.EncodeOperator(other)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		if err := reg.Apply(wal.Committed{PrepareIndex: 222, CommitIndex: 223, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		out := logBuf.String()
		if !strings.Contains(out, "DISCARDING") {
			t.Fatalf("a contradicting second revocation was discarded SILENTLY, which is the invariant 6 defect; log was:\n%s", out)
		}
		if !strings.Contains(out, "223") {
			t.Fatalf("the discard log does not name the commit index, so nobody can find the record; log was:\n%s", out)
		}
		kept, _ := reg.Get(id)
		if !kept.RevokedAt.Equal(*stored.RevokedAt) {
			t.Fatal("the second revocation overwrote the first; nothing supersedes a revocation")
		}
	})

	t.Run("an ORPHAN revocation is reported", func(t *testing.T) {
		var logBuf bytes.Buffer
		reg, _, clock, _ := opRegistryWithLogger(t, logging.New(&logBuf, logging.LevelDebug))

		// A revocation for an id whose ADD never arrived: the log was truncated, or
		// the creating record was damaged and discarded.
		pub, _ := newKeypair(t)
		revokedAt := clock.Now()
		body, err := auth.EncodeOperator(auth.Operator{
			OperatorID: mustMintOperatorID(t, "ghost"), Name: "ghost",
			AuthPublicKey: pub, CertFingerprint: opFP(0xF2), CreatedAt: epoch,
			RevokedAt: &revokedAt, RevokedReason: "revoked elsewhere",
		})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		if err := reg.Apply(wal.Committed{PrepareIndex: 230, CommitIndex: 231, Entry: wal.Entry{Kind: auth.OperatorRecordKind, Body: body}}); err != nil {
			t.Fatalf("Apply must never fail recovery: %v", err)
		}
		out := logBuf.String()
		if !strings.Contains(out, "NO preceding ADD") {
			t.Fatalf("an orphan revocation was folded in SILENTLY; the record that CREATED the operator is missing and nobody was told. Log was:\n%s", out)
		}
		if !strings.Contains(out, "231") {
			t.Fatalf("the orphan log does not name the commit index; log was:\n%s", out)
		}
		if reg.LiveLen() != 0 {
			t.Fatalf("an orphan REVOCATION produced a LIVE operator: LiveLen = %d", reg.LiveLen())
		}
	})
}

// TestOperatorPrincipalDivergedWriteIsReportedAsDURABLE pins the A5 fix:
// wal.ErrDiverged means the commit record was appended and FSYNCED before the
// failure, so the record IS on disk and only a neighbouring applier failed.
//
// WALRoster.put already reports that condition as durable. Collapsing it into a
// generic failure here is worse than it is there: a failed-looking `operator add`
// whose record is in fact durable becomes a LIVE ADMIN at the next start while
// the operator retries and is given a second one under a fresh id (invariant 1:
// the id is never reused), so one intended add leaves two admin identities.
func TestOperatorPrincipalDivergedWriteIsReportedAsDURABLE(t *testing.T) {
	t.Parallel()

	reg := auth.NewOperatorRegistry(logging.New(&bytes.Buffer{}, logging.LevelError))
	log := &opFakeLog{applier: reg, writeErr: fmt.Errorf("applying entry: %w", wal.ErrDiverged)}
	if err := reg.Attach(log); err != nil {
		t.Fatalf("attaching: %v", err)
	}

	pub, _ := newKeypair(t)
	err := reg.Add(auth.Operator{
		OperatorID: mustMintOperatorID(t, "ops"), Name: "ops",
		AuthPublicKey: pub, CertFingerprint: opFP(0x51), CreatedAt: epoch,
	})
	if err == nil {
		t.Fatal("Add over a diverged write returned nil")
	}
	if !errors.Is(err, wal.ErrDiverged) {
		t.Fatalf("Add error = %v, want it to still wrap wal.ErrDiverged so a caller can match on it", err)
	}
	if !strings.Contains(err.Error(), "DURABLE") {
		t.Fatalf("the diverged write was reported as an ordinary failure, so an operator would RETRY and end up with TWO admin identities from one intended add: %v", err)
	}
	if !strings.Contains(err.Error(), "DO NOT RETRY") {
		t.Fatalf("the error does not tell the operator what to do instead of retrying: %v", err)
	}
}

// TestOperatorPrincipalGlobalSessionCapFailsClosed is MUTATION 3: delete the
// global cap and the operator session table grows without bound.
//
// The table is deliberately small (DefaultMaxOperatorSessions = 64) and separate
// from the agent one; a cap nobody enforces makes the separation worthless.
func TestOperatorPrincipalGlobalSessionCapFailsClosed(t *testing.T) {
	t.Parallel()

	reg, svc, _ := opServiceWith(t, auth.OperatorOptions{MaxSessions: 3})
	fpA, fpB := opFP(0x21), opFP(0x22)
	idA, _ := opAdd(t, reg, "alice", fpA)
	idB, _ := opAdd(t, reg, "bob", fpB)

	for i := 0; i < 3; i++ {
		if _, err := svc.BeginSession(idA, fpA); err != nil {
			t.Fatalf("begin %d of 3 within the cap: %v", i+1, err)
		}
	}
	if _, err := svc.BeginSession(idB, fpB); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("the 4th begin over a table of 3 error = %v, want ErrCapacity", err)
	}
	if got := svc.SessionCount(); got != 3 {
		t.Fatalf("session count = %d, want 3; the refusal must leave the table exactly as it found it and must NEVER evict", got)
	}
}

// TestOperatorPrincipalPerOperatorPendingCapIsNotALockout is the A3 fix: one
// operator can no longer park the whole table and lock every other operator out
// of the admin plane. It is also the test for MUTATION 3's sibling — remove the
// per-operator PENDING cap and the first subtest goes red.
//
// The second subtest is the half that matters more: the cap must bound the
// FLOODER and nobody else. A per-operator bucket keyed on an ATTACKER-SUPPLIED
// name would itself be a lockout primitive (AUTH-1-FU-PENDINGCAP, which is why
// Service.BeginSession has no such cap); it is safe here only because
// BeginSession has already proved the caller holds that operator's certificate.
func TestOperatorPrincipalPerOperatorPendingCapIsNotALockout(t *testing.T) {
	t.Parallel()

	reg, svc, _ := opServiceWith(t, auth.OperatorOptions{MaxSessions: 64, MaxPendingSessionsPerOperator: 2})
	fpA, fpB := opFP(0x31), opFP(0x32)
	idA, _ := opAdd(t, reg, "alice", fpA)
	idB, _ := opAdd(t, reg, "bob", fpB)

	t.Run("one operator is bounded", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if _, err := svc.BeginSession(idA, fpA); err != nil {
				t.Fatalf("pending challenge %d of 2 within the cap: %v", i+1, err)
			}
		}
		if _, err := svc.BeginSession(idA, fpA); !errors.Is(err, auth.ErrCapacity) {
			t.Fatalf("the 3rd PENDING challenge for one operator error = %v, want ErrCapacity; without this cap one certificate holder fills the whole table and denies the admin plane", err)
		}
	})

	t.Run("every other operator is unaffected", func(t *testing.T) {
		if _, err := svc.BeginSession(idB, fpB); err != nil {
			t.Fatalf("a SECOND operator must still be able to begin a session while the first is at its cap: %v", err)
		}
	})

	if got := svc.SessionCount(); got != 3 {
		t.Fatalf("session count = %d, want 3 (2 for alice, 1 for bob); the refusal must never evict", got)
	}
}

// TestOperatorPrincipalPerOperatorActiveCapFailsClosed is MUTATION 4: delete the
// per-operator ACTIVE cap in CompleteSession and one operator can hold the whole
// table in live credentials.
//
// The cap is keyed on the operator id, which is safe THERE for a different
// reason than on the begin path: an entry can only be counted into a bucket by
// someone who produced a valid Ed25519 signature with that operator's PRIVATE
// key, so a flooder can only fill its own.
func TestOperatorPrincipalPerOperatorActiveCapFailsClosed(t *testing.T) {
	t.Parallel()

	reg, svc, _ := opServiceWith(t, auth.OperatorOptions{MaxActiveSessionsPerOperator: 2})
	fp := opFP(0x41)
	id, priv := opAdd(t, reg, "ops", fp)

	for i := 0; i < 2; i++ {
		opActiveSession(t, svc, id, priv, fp)
	}

	ch, err := svc.BeginSession(id, fp)
	if err != nil {
		t.Fatalf("the BEGIN must still succeed — the cap is on ACTIVE sessions: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(auth.OperatorSessionSigningContext+ch.Token))
	if _, err := svc.CompleteSession(ch.Token, sig, fp); !errors.Is(err, auth.ErrCapacity) {
		t.Fatalf("completing a 3rd ACTIVE session over a cap of 2 error = %v, want ErrCapacity", err)
	}
	// FAILS CLOSED WITHOUT EVICTING, and without burning the pending challenge:
	// the single-attempt rule deletes a pending session on a FAILED SIGNATURE, and
	// this signature verified.
	if got := svc.SessionCount(); got != 3 {
		t.Fatalf("session count = %d, want 3 (2 active + the untouched pending); a capacity refusal must never evict somebody's live session", got)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func mustMintOperatorID(t *testing.T, name string) string {
	t.Helper()
	id, err := auth.MintOperatorID(testBusID, name)
	if err != nil {
		t.Fatalf("MintOperatorID(%q): %v", name, err)
	}
	return id
}

func mustPub(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _ := newKeypair(t)
	return pub
}
