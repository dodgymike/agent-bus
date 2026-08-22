package auth_test

// AUTH-DUP-ENROL-KEY: Enrol must refuse a second enrolment presenting an AUTH
// public key already on the roster, so one keypair cannot hold two agent ids.
//
// The rule is the auth-key mirror of the certificate-fingerprint rule
// (certbind_mtlsbind_test.go): an authoritative refusal in Roster.Put, and an
// advisory pre-mint read in Service.Enrol so the common refusal burns no agent-id
// suffix. These tests pin both, plus the two ordering facts that make the rule
// safe: a legitimate idempotent RETRY still replays (it is decided before the
// auth-key read), and the durable roster refuses BEFORE the write.

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// authKeyEntry is a fully valid roster entry carrying a CALLER-CHOSEN auth key,
// so a test can enrol two ids under one keypair. Name is parsed from the id
// because the durable roster refuses an entry whose Name disagrees with its id.
func authKeyEntry(t *testing.T, agentID string, pub ed25519.PublicKey) auth.RosterEntry {
	t.Helper()
	_, name, _, err := ids.ParseAgentID(agentID)
	if err != nil {
		t.Fatalf("test fixture: agent id %q does not parse: %v", agentID, err)
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	return auth.RosterEntry{
		AgentID:       agentID,
		Name:          name,
		AuthPublicKey: pub,
		Epoch:         now,
		EnrolledAt:    now,
	}
}

// TestEnrolRejectsDuplicateAuthKey is the core behaviour: a second enrolment
// with the same auth public key — different name, different idempotency key — is
// refused with ErrAuthKeyBound, and no second agent id is minted.
//
// MUTATION THAT KILLS IT ALONE: delete the AgentIDForAuthKey pre-mint check from
// Service.Enrol (and the checkAuthKeyUnbound call from MemoryRoster.Put).
func TestEnrolRejectsDuplicateAuthKey(t *testing.T) {
	r := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: r})

	pub, _ := newKeypair(t)

	first, err := svc.Enrol(auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("first enrolment: %v", err)
	}

	_, err = svc.Enrol(auth.EnrolRequest{Name: "beta", PublicKey: pub, IdempotencyKey: "k2"})
	if !errors.Is(err, auth.ErrAuthKeyBound) {
		t.Fatalf("second enrolment with a duplicate auth key: err = %v, want ErrAuthKeyBound", err)
	}

	// Exactly one agent was minted: the duplicate never reached the roster.
	if n := r.Len(); n != 1 {
		t.Fatalf("roster holds %d agents after a refused duplicate, want 1", n)
	}
	if _, ok := r.Get(first.AgentID); !ok {
		t.Fatalf("the first agent %q is not in the roster", first.AgentID)
	}
}

// TestEnrolDuplicateAuthKeyRetryStillReplays proves the ORDER: a genuine
// idempotent retry — same idempotency key, same name, same key — replays the
// original result and is NOT caught by the auth-key check, because idempotency is
// decided first. Collapsing the two would punish exactly the clients that retry
// correctly (invariant 10).
//
// MUTATION THAT KILLS IT ALONE: move the AgentIDForAuthKey check above the
// idempotency replay block in Service.Enrol.
func TestEnrolDuplicateAuthKeyRetryStillReplays(t *testing.T) {
	svc, _ := newService(t, auth.Options{})

	pub, _ := newKeypair(t)
	req := auth.EnrolRequest{Name: "alpha", PublicKey: pub, IdempotencyKey: "k1"}

	first, err := svc.Enrol(req)
	if err != nil {
		t.Fatalf("first enrolment: %v", err)
	}
	if first.Replayed {
		t.Fatal("first enrolment reported Replayed = true")
	}

	retry, err := svc.Enrol(req)
	if err != nil {
		t.Fatalf("idempotent retry: err = %v, want nil (a retry must replay, not be refused as a duplicate key)", err)
	}
	if !retry.Replayed {
		t.Fatal("idempotent retry did not report Replayed = true")
	}
	if retry.AgentID != first.AgentID {
		t.Fatalf("retry minted a different id: got %q, want %q", retry.AgentID, first.AgentID)
	}
}

// TestMemoryRosterPutRefusesDuplicateAuthKey pins the authoritative refusal and
// the read seam directly on the in-memory roster.
func TestMemoryRosterPutRefusesDuplicateAuthKey(t *testing.T) {
	r := auth.NewMemoryRoster()
	pub, _ := newKeypair(t)

	if err := r.Put(authKeyEntry(t, "bus-under-test.alpha-1", pub)); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}

	err := r.Put(authKeyEntry(t, "bus-under-test.beta-1", pub))
	if !errors.Is(err, auth.ErrAuthKeyBound) {
		t.Fatalf("Put beta with a duplicate auth key: err = %v, want ErrAuthKeyBound", err)
	}
	if _, ok := r.Get("bus-under-test.beta-1"); ok {
		t.Fatal("the refused agent is in the roster")
	}

	// The read seam resolves the one holder, and refuses a key nobody holds.
	if got, err := r.AgentIDForAuthKey(pub); err != nil || got != "bus-under-test.alpha-1" {
		t.Fatalf("AgentIDForAuthKey(bound) = (%q, %v), want (bus-under-test.alpha-1, nil)", got, err)
	}
	other, _ := newKeypair(t)
	if _, err := r.AgentIDForAuthKey(other); !errors.Is(err, auth.ErrAuthKeyUnknown) {
		t.Fatalf("AgentIDForAuthKey(unbound) err = %v, want ErrAuthKeyUnknown", err)
	}
}

// TestAuthKeyOwnerFailsClosedWhenAmbiguous drives the recovery-only case: a
// roster that already holds two agents under one key (which the write path
// refuses to create, but a damaged log can) must resolve to NOBODY, never a pick.
// The stubRoster does not enforce uniqueness in Put, so it can build the state.
func TestAuthKeyOwnerFailsClosedWhenAmbiguous(t *testing.T) {
	r := newStubRoster()
	pub, _ := newKeypair(t)

	for _, id := range []string{"bus-under-test.alpha-1", "bus-under-test.beta-1"} {
		if err := r.Put(authKeyEntry(t, id, pub)); err != nil {
			t.Fatalf("stub Put %q: %v", id, err)
		}
	}
	if _, err := r.AgentIDForAuthKey(pub); !errors.Is(err, auth.ErrAuthKeyAmbiguous) {
		t.Fatalf("AgentIDForAuthKey over a duplicated key: err = %v, want ErrAuthKeyAmbiguous", err)
	}
}

// TestWALRosterRefusesDuplicateAuthKeyBeforeWriting is the durable-roster half:
// the refusal must land BEFORE the write, proved by restarting and rebuilding
// from the log alone — if the refused entry is absent after a restart it was
// never on disk, so no fsync was burned and Apply never had to discard it.
//
// MUTATION THAT KILLS IT ALONE: delete the checkAuthKeyUnbound call from
// WALRoster.put. Moving it to AFTER r.w.Write kills only the restart half.
func TestWALRosterRefusesDuplicateAuthKeyBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	r, l := openRoster(t, dir)
	if err := r.Put(authKeyEntry(t, "bus-under-test.alpha-1", pub)); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}
	err = r.Put(authKeyEntry(t, "bus-under-test.beta-1", pub))
	if !errors.Is(err, auth.ErrAuthKeyBound) {
		t.Fatalf("durable Put with a duplicate auth key: err = %v, want ErrAuthKeyBound", err)
	}
	if _, ok := r.Get("bus-under-test.beta-1"); ok {
		t.Fatal("the refused agent is in the serving roster")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// RESTART: rebuild from the log alone. The refused entry must not appear.
	r2, l2 := openRoster(t, dir)
	defer func() {
		if err := l2.Close(); err != nil {
			t.Fatalf("closing the reopened log: %v", err)
		}
	}()
	if _, ok := r2.Get("bus-under-test.beta-1"); ok {
		t.Fatal("the refused agent reappeared after a restart, so it WAS written to the log")
	}
	if _, ok := r2.Get("bus-under-test.alpha-1"); !ok {
		t.Fatal("the first agent did not survive the restart")
	}
}
