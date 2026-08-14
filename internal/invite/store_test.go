// INVITE-STORE's acceptance evidence for the state machine and its durability.
//
// The suite is REJECTION-WEIGHTED on purpose. The one property everything in
// this package rests on is SINGLE USE, and single use is a property about the
// paths that must FAIL: expired, revoked, already-redeemed, wrong secret,
// unknown, dropped, replayed backwards, raced. A suite that only proved the
// happy path would pass just as happily against a store that never refused
// anything, which is exactly the store this package exists not to be.
//
// Three names here are pinned by the task's stored proof_cmd and must not be
// renamed: TestInviteStoreRecovery, TestInviteExpiredIsNotRedeemable, and
// TestInviteSingleUseSurvivesCrash (crash_test.go).
//
// Every data directory is a t.TempDir(). The tracked data/ dir is not a test
// fixture and no two tests ever share a directory.
package invite_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// Fixtures and helpers
// ---------------------------------------------------------------------------

const (
	testBusID = "bus-test"

	// testKey is the client idempotency key a redemption carries. It is scoped
	// to the invite (doc.go section 4), so the same literal is reused across
	// tests against different invites without collision.
	testKey = "k-enrol-1"
	// testOtherKey is a DIFFERENT key against an already-spent invite: the
	// ErrAlreadyRedeemed case, not the ErrKeyReuse one.
	testOtherKey = "k-enrol-2"

	// testAgentID is the fully-qualified id (invariant 2) a redemption mints.
	testAgentID = "bus-test.alpha"
)

var (
	// testResult is the response a fresh redemption stores and a legitimate
	// retry replays VERBATIM. It is already compact, so Encode's canonical form
	// is byte-identical to this.
	testResult = json.RawMessage(`{"agent_id":"bus-test.alpha","token":"tok-1"}`)

	// testCertFingerprint stands in for sha256.Sum256(cert.Raw). MTLS-BIND will
	// check it; this package only has to carry it through the durable round
	// trip, and a zero value here would prove nothing about that.
	testCertFingerprint = [invite.DigestSize]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
)

// fingerprintOf is the payload fingerprint a caller would compute. The exact
// field list is the CALLER's to define (RedeemRequest.Fingerprint); what this
// package cares about is only that two fingerprints compare equal or not.
func fingerprintOf(payload string) idem.Fingerprint {
	return idem.ComputeFingerprint([]byte(payload))
}

// testClock is the clock every predicate in this package is evaluated against.
// Expiry and retention are pure predicates over a time, so a fake clock tests
// the REAL derived windows (invite.SpentRetention is over 50 hours) without a
// test that sleeps.
//
// It is mutex-guarded because Store.Now is called from whatever goroutine is in
// the store, and the concurrency test runs under -race.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeLog is a DurableLog that records what it was asked to make durable and
// nothing else. It exists so the logic tests run in milliseconds; every claim
// that actually depends on bytes reaching stable storage goes through a REAL
// *wal.Log (TestInviteStoreRecovery, TestInviteMintNeverStoresTheSecret) or a
// real SIGKILL (crash_test.go).
//
// Counting its entries is how the tests prove a REFUSAL WROTE NOTHING, which is
// half of what "fails closed" means.
type fakeLog struct {
	mu      sync.Mutex
	entries []wal.Entry
	err     error
}

func (f *fakeLog) Write(e wal.Entry) (wal.Committed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return wal.Committed{}, f.err
	}
	f.entries = append(f.entries, e)
	n := uint64(len(f.entries))
	return wal.Committed{PrepareIndex: 2*n - 1, CommitIndex: 2 * n, Entry: e}, nil
}

func (f *fakeLog) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

func (f *fakeLog) records(t *testing.T) []invite.Record {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]invite.Record, 0, len(f.entries))
	for i, e := range f.entries {
		if e.Kind != invite.RecordKind {
			t.Fatalf("durable entry %d has kind %q, want %q", i, e.Kind, invite.RecordKind)
		}
		r, err := invite.DecodeRecord(e.Body)
		if err != nil {
			t.Fatalf("durable entry %d does not decode: %v", i, err)
		}
		out = append(out, r)
	}
	return out
}

func (f *fakeLog) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// newFakeStore builds a Store over fakeLog and a fake clock. mutate may adjust
// the options (a smaller cap, a shorter window) before construction.
func newFakeStore(t *testing.T, mutate func(*invite.StoreOptions)) (*invite.Store, *fakeLog, *testClock) {
	t.Helper()
	fl := &fakeLog{}
	clk := newTestClock()
	o := invite.StoreOptions{BusID: testBusID, Durable: fl, Now: clk.Now}
	if mutate != nil {
		mutate(&o)
	}
	st, err := invite.NewStore(o)
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	return st, fl, clk
}

// mustMint mints an invite and fails the test if it could not.
func mustMint(t *testing.T, st *invite.Store, req invite.MintRequest) invite.Minted {
	t.Helper()
	m, err := st.Mint(req)
	if err != nil {
		t.Fatalf("Mint(%+v): %v", req, err)
	}
	if m.Secret == "" {
		t.Fatalf("Mint returned an empty secret")
	}
	if m.State != invite.StateOpen {
		t.Fatalf("a freshly minted invite is %s, want open", m.State)
	}
	return m
}

// redeemReq is the standard redemption request against m.
func redeemReq(m invite.Minted, key, payload string) invite.RedeemRequest {
	return invite.RedeemRequest{
		InviteID:    m.ID,
		Secret:      m.Secret,
		Key:         key,
		Fingerprint: fingerprintOf(payload),
	}
}

// standardResult is what a fresh redemption produces.
func standardResult() invite.Result {
	return invite.Result{
		AgentID:         testAgentID,
		Response:        testResult,
		CertFingerprint: testCertFingerprint,
	}
}

// mustRedeem spends an invite through the standalone path and fails the test if
// it could not.
func mustRedeem(t *testing.T, st *invite.Store, req invite.RedeemRequest) invite.Record {
	t.Helper()
	rec, err := st.Redeem(req, standardResult())
	if err != nil {
		t.Fatalf("Redeem(%s): %v", req.InviteID, err)
	}
	if rec.State != invite.StateRedeemed {
		t.Fatalf("after Redeem the record is %s, want redeemed", rec.State)
	}
	return rec
}

// lateLog defers binding the *wal.Log until after wal.Open.
//
// A Store that is BOTH the log's Applier and the log's writer — which is how
// the server wires it — is a construction cycle: NewStore needs the log and
// wal.Open needs the store. Binding late closes it.
//
// Using this shape in every real-WAL test is deliberate: it is the wiring in
// which doc.go's "THE STORE LOCK IS NEVER HELD ACROSS A DURABLE WRITE" is
// load-bearing. wal.Log.Write calls Applier.Apply synchronously, so a store
// that held its own lock across Durable.Write would self-deadlock here on the
// first mint, and every one of these tests would hang instead of passing.
type lateLog struct{ l *wal.Log }

func (d *lateLog) Write(e wal.Entry) (wal.Committed, error) {
	if d.l == nil {
		return wal.Committed{}, errors.New("test: lateLog was used before its *wal.Log was bound")
	}
	return d.l.Write(e)
}

// openStore opens (or REOPENS) dir the way the server does: a FRESH Store wired
// as the log's Applier, so every committed entry in the durable log is replayed
// into it before wal.Open returns.
//
// The caller owns Close, because a test that reopens the same directory must
// close the first log before the second is opened — two writers on one WAL is
// not a scenario any of these tests are about.
func openStore(t *testing.T, dir string, clk *testClock) (*invite.Store, *wal.Log) {
	t.Helper()
	d := &lateLog{}
	o := invite.StoreOptions{BusID: testBusID, Durable: d}
	if clk != nil {
		o.Now = clk.Now
	}
	st, err := invite.NewStore(o)
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	d.l = lg
	return st, lg
}

// readWAL reads the durable log out of dir, byte for byte, the way an operator
// with `cat` would. It is how TestInviteMintNeverStoresTheSecret produces
// evidence rather than a restatement of the claim.
func readWAL(dir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, wal.WALFileName))
}

// decodeSecret returns the raw bytes behind a minted secret's
// base64.RawURLEncoding form.
func decodeSecret(secret string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(secret)
}

// assertRecordEqual compares every field of two records. Times compare with
// Equal (a decoded time carries no monotonic reading), and Result compares as
// bytes.
func assertRecordEqual(t *testing.T, ctx string, got, want invite.Record) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("%s: ID = %q, want %q", ctx, got.ID, want.ID)
	}
	if got.BusID != want.BusID {
		t.Errorf("%s: BusID = %q, want %q", ctx, got.BusID, want.BusID)
	}
	if got.SecretDigest != want.SecretDigest {
		t.Errorf("%s: SecretDigest = %x, want %x", ctx, got.SecretDigest, want.SecretDigest)
	}
	if got.Label != want.Label {
		t.Errorf("%s: Label = %q, want %q", ctx, got.Label, want.Label)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("%s: CreatedAt = %s, want %s", ctx, got.CreatedAt, want.CreatedAt)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("%s: ExpiresAt = %s, want %s", ctx, got.ExpiresAt, want.ExpiresAt)
	}
	if got.State != want.State {
		t.Errorf("%s: State = %s, want %s", ctx, got.State, want.State)
	}
	if !got.RedeemedAt.Equal(want.RedeemedAt) {
		t.Errorf("%s: RedeemedAt = %s, want %s", ctx, got.RedeemedAt, want.RedeemedAt)
	}
	if got.RedeemedBy != want.RedeemedBy {
		t.Errorf("%s: RedeemedBy = %q, want %q", ctx, got.RedeemedBy, want.RedeemedBy)
	}
	if got.RedeemKey != want.RedeemKey {
		t.Errorf("%s: RedeemKey = %q, want %q", ctx, got.RedeemKey, want.RedeemKey)
	}
	if got.RedeemFingerprint != want.RedeemFingerprint {
		t.Errorf("%s: RedeemFingerprint = %x, want %x", ctx, got.RedeemFingerprint, want.RedeemFingerprint)
	}
	if !bytes.Equal(got.Result, want.Result) {
		t.Errorf("%s: Result = %s, want %s", ctx, got.Result, want.Result)
	}
	if got.CertFingerprint != want.CertFingerprint {
		t.Errorf("%s: CertFingerprint = %x, want %x", ctx, got.CertFingerprint, want.CertFingerprint)
	}
	if !got.RevokedAt.Equal(want.RevokedAt) {
		t.Errorf("%s: RevokedAt = %s, want %s", ctx, got.RevokedAt, want.RevokedAt)
	}
	if got.RevokedReason != want.RevokedReason {
		t.Errorf("%s: RevokedReason = %q, want %q", ctx, got.RevokedReason, want.RevokedReason)
	}
}

// mustLookup fetches a retained record and fails if it has been dropped.
func mustLookup(t *testing.T, st *invite.Store, id, ctx string) invite.Record {
	t.Helper()
	r, ok := st.Lookup(id)
	if !ok {
		t.Fatalf("%s: Lookup(%s) = not found, want the retained record", ctx, id)
	}
	return r
}

// ---------------------------------------------------------------------------
// PINNED: recovery
// ---------------------------------------------------------------------------

// TestInviteStoreRecovery is the invariant-5 claim, proved end to end: memory is
// the serving copy, DISK IS THE TRUTH, and a fresh process rebuilds the whole
// invite table — including which invites are SPENT — by replaying the durable
// log.
//
// If this fails, an invite that was redeemed before a restart is redeemable
// again after one, and the enrolment gate INVITE-GATE builds on this is
// decorative.
func TestInviteStoreRecovery(t *testing.T) {
	dir := t.TempDir()
	clk := newTestClock()

	// --- Before the restart -------------------------------------------------
	st, lg := openStore(t, dir, clk)

	spent := mustMint(t, st, invite.MintRequest{Label: "for the agent that enrols", TTL: 6 * time.Hour})
	clk.Advance(time.Minute)
	spentRec := mustRedeem(t, st, redeemReq(spent, testKey, "payload-a"))

	revoked := mustMint(t, st, invite.MintRequest{Label: "issued in error", TTL: 6 * time.Hour})
	clk.Advance(time.Minute)
	revokedRec, err := st.Revoke(revoked.ID, "minted for the wrong team")
	if err != nil {
		t.Fatalf("Revoke(%s): %v", revoked.ID, err)
	}

	open := mustMint(t, st, invite.MintRequest{Label: "still unused", TTL: 6 * time.Hour})
	openRec := mustLookup(t, st, open.ID, "before the restart")

	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log before the restart: %v", err)
	}

	// --- THE RESTART: a brand-new Store, rebuilt only from the durable log ---
	st2, lg2 := openStore(t, dir, clk)
	defer func() { _ = lg2.Close() }()

	if got := lg2.Recovered().Applied; got != 5 {
		t.Fatalf("recovery applied %d committed entries, want 5 (three mints, one redemption, one revocation): the table must be REBUILT FROM THE DURABLE LOG, and a store that recovered nothing would pass the rest of this test only by accident", got)
	}

	// (a) EVERY FIELD is rebuilt, for each of the three terminal shapes.
	assertRecordEqual(t, "the spent invite after recovery", mustLookup(t, st2, spent.ID, "after the restart"), spentRec)
	assertRecordEqual(t, "the revoked invite after recovery", mustLookup(t, st2, revoked.ID, "after the restart"), revokedRec)
	assertRecordEqual(t, "the open invite after recovery", mustLookup(t, st2, open.ID, "after the restart"), openRec)

	// (b) THE SPENT ONE IS STILL SPENT. A fresh redemption presenting the
	// CORRECT secret under a DIFFERENT key is refused: single use survived the
	// restart.
	if _, err := st2.Begin(redeemReq(spent, testOtherKey, "payload-a")); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("redeeming the already-spent invite after a restart gave err = %v, want ErrAlreadyRedeemed: the restart FORGOT that this invite was spent, so one invite admitted two agents", err)
	}

	// ... and neither does the standalone path let it through.
	if _, err := st2.Redeem(redeemReq(spent, testOtherKey, "payload-a"), standardResult()); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("Redeem of the already-spent invite after a restart gave err = %v, want ErrAlreadyRedeemed", err)
	}

	// (c) The legitimate retry still replays the ORIGINAL result across the
	// restart, so recovery preserved the key AND the fingerprint, not merely
	// "this invite is spent".
	r, err := st2.Begin(redeemReq(spent, testKey, "payload-a"))
	if err != nil {
		t.Fatalf("the legitimate retry after a restart gave err = %v, want the original result replayed", err)
	}
	if r.Outcome() != invite.OutcomeReplay {
		t.Fatalf("the retry after a restart has outcome %s, want replay", r.Outcome())
	}
	if !bytes.Equal(r.Result(), testResult) {
		t.Fatalf("the retry after a restart replayed %s, want the original %s", r.Result(), testResult)
	}

	// (d) THE REVOKED ONE IS STILL REVOKED.
	if _, err := st2.Begin(redeemReq(revoked, testKey, "payload-b")); !errors.Is(err, invite.ErrRevoked) {
		t.Fatalf("redeeming the revoked invite after a restart gave err = %v, want ErrRevoked", err)
	}

	// (e) The OPEN one still works — recovery must not have broken the invite
	// that had no terminal event, or the gate would be closed to everybody.
	if _, err := st2.Redeem(redeemReq(open, testKey, "payload-c"), standardResult()); err != nil {
		t.Fatalf("redeeming the still-open invite after a restart gave err = %v, want success", err)
	}

	// (f) And that redemption is itself durable: a SECOND restart still says
	// spent. Recovery has to be a fixed point, not a one-off.
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the log before the second restart: %v", err)
	}
	st3, lg3 := openStore(t, dir, clk)
	defer func() { _ = lg3.Close() }()
	for _, id := range []string{spent.ID, open.ID} {
		if got := mustLookup(t, st3, id, "after the second restart").State; got != invite.StateRedeemed {
			t.Errorf("after a second restart invite %s is %s, want redeemed", id, got)
		}
	}
	if got := mustLookup(t, st3, revoked.ID, "after the second restart").State; got != invite.StateRevoked {
		t.Errorf("after a second restart invite %s is %s, want revoked", revoked.ID, got)
	}
}

// ---------------------------------------------------------------------------
// PINNED: expiry
// ---------------------------------------------------------------------------

// TestInviteExpiredIsNotRedeemable pins the expiry predicate.
//
// Expiry is deliberately NOT a stored state (record.go): it is re-derived from
// ExpiresAt and the clock on every call. So the test that matters is the
// BOUNDARY — an invite is live at exactly ExpiresAt and dead one nanosecond
// later — plus the guarantee that a refused redemption writes NOTHING and
// leaves the invite unspent.
func TestInviteExpiredIsNotRedeemable(t *testing.T) {
	t.Run("the boundary", func(t *testing.T) {
		st, fl, clk := newFakeStore(t, nil)
		m := mustMint(t, st, invite.MintRequest{TTL: time.Hour})

		// AT ExpiresAt the invite is still live: Expired is now.After(ExpiresAt).
		clk.Advance(time.Hour)
		if m.Expired(clk.Now()) {
			t.Fatalf("Expired(ExpiresAt) = true, want false: the window closes AFTER ExpiresAt, not at it")
		}
		r, err := st.Begin(redeemReq(m, testKey, "payload"))
		if err != nil {
			t.Fatalf("Begin exactly at ExpiresAt gave err = %v, want a reserved redemption", err)
		}
		r.Abort()

		// ONE NANOSECOND LATER it is not.
		clk.Advance(time.Nanosecond)
		if !m.Expired(clk.Now()) {
			t.Fatalf("Expired(ExpiresAt+1ns) = false, want true")
		}
		if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrExpired) {
			t.Fatalf("Begin one nanosecond past ExpiresAt gave err = %v, want ErrExpired", err)
		}
		if n := fl.len(); n != 1 {
			t.Fatalf("the durable log holds %d entries, want 1 (the mint alone): a refused redemption must write nothing", n)
		}
	})

	t.Run("well past expiry, through every path", func(t *testing.T) {
		st, fl, clk := newFakeStore(t, nil)
		m := mustMint(t, st, invite.MintRequest{Label: "expires", TTL: time.Hour})
		clk.Advance(2 * time.Hour)

		if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrExpired) {
			t.Errorf("Begin on an expired invite gave err = %v, want ErrExpired", err)
		}
		if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); !errors.Is(err, invite.ErrExpired) {
			t.Errorf("Redeem on an expired invite gave err = %v, want ErrExpired", err)
		}

		// NOTHING was spent and NOTHING was written. An expired invite that
		// quietly flipped to redeemed would be a state change produced by a
		// refusal.
		if got := mustLookup(t, st, m.ID, "after the refused redemptions").State; got != invite.StateOpen {
			t.Errorf("after two refused redemptions the invite is %s, want open", got)
		}
		if n := fl.len(); n != 1 {
			t.Errorf("the durable log holds %d entries, want 1 (the mint alone)", n)
		}

		// An expired-but-retained record is still diagnosable as EXPIRED rather
		// than unknown — that is the whole reason retention outlives ExpiresAt
		// (retention.go, SpentRetention). It buys diagnosis, never usability.
		if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrExpired) {
			t.Errorf("a retained expired invite reports err = %v, want ErrExpired to stay reachable", err)
		}

		// An operator may still REVOKE an expired invite: revocation records a
		// decision, and the clock having got there first must not erase it.
		if _, err := st.Revoke(m.ID, "expired and withdrawn"); err != nil {
			t.Errorf("Revoke on an expired but open invite gave err = %v, want success", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The other refusals
// ---------------------------------------------------------------------------

// TestInviteRevokedIsNotRedeemable covers revocation in both directions: a
// revoked invite is dead, and revocation itself refuses to lie about its reach.
func TestInviteRevokedIsNotRedeemable(t *testing.T) {
	st, fl, clk := newFakeStore(t, nil)

	m := mustMint(t, st, invite.MintRequest{Label: "to be withdrawn"})
	rec, err := st.Revoke(m.ID, "the operator changed their mind")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if rec.State != invite.StateRevoked || rec.RevokedReason != "the operator changed their mind" || rec.RevokedAt.IsZero() {
		t.Fatalf("Revoke produced %+v, want a revoked record carrying the reason and a timestamp", rec)
	}

	if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrRevoked) {
		t.Fatalf("Begin on a revoked invite gave err = %v, want ErrRevoked", err)
	}
	if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); !errors.Is(err, invite.ErrRevoked) {
		t.Fatalf("Redeem on a revoked invite gave err = %v, want ErrRevoked", err)
	}

	// Re-revoking is IDEMPOTENT and writes NOTHING: it is the same decision.
	before := fl.len()
	again, err := st.Revoke(m.ID, "a different reason on a second attempt")
	if err != nil {
		t.Fatalf("re-revoking gave err = %v, want the existing record and no error", err)
	}
	if !again.RevokedAt.Equal(rec.RevokedAt) || again.RevokedReason != rec.RevokedReason {
		t.Errorf("re-revoking returned %+v, want the ORIGINAL revocation unchanged", again)
	}
	if n := fl.len(); n != before {
		t.Errorf("re-revoking wrote %d new durable entries, want 0", n-before)
	}

	// Revoking a REDEEMED invite is refused, and that refusal is the honest
	// answer: revocation does not reach an agent that already enrolled
	// (DECISIONS.md E5). Reporting success would promise reach it does not have.
	spent := mustMint(t, st, invite.MintRequest{})
	clk.Advance(time.Minute)
	mustRedeem(t, st, redeemReq(spent, testKey, "payload"))
	before = fl.len()
	if _, err := st.Revoke(spent.ID, "shut it down"); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("revoking a redeemed invite gave err = %v, want ErrAlreadyRedeemed", err)
	}
	if n := fl.len(); n != before {
		t.Errorf("the refused revocation wrote %d durable entries, want 0", n-before)
	}
	if got := mustLookup(t, st, spent.ID, "after the refused revocation").State; got != invite.StateRedeemed {
		t.Errorf("after a refused revocation the invite is %s, want redeemed (unchanged)", got)
	}

	// Revoking an invite that does not exist is ErrUnknownInvite, and an
	// oversized reason is refused without echoing itself back.
	if _, err := st.Revoke("inv-zzzzzzzzzzzzzzzz", "gone"); !errors.Is(err, invite.ErrUnknownInvite) {
		t.Errorf("revoking an unknown invite gave err = %v, want ErrUnknownInvite", err)
	}
	huge := strings.Repeat("R", invite.MaxReasonLen+1)
	_, err = st.Revoke(m.ID, huge)
	if !errors.Is(err, invite.ErrInvalidRecord) {
		t.Fatalf("revoking with an oversized reason gave err = %v, want ErrInvalidRecord", err)
	}
	if strings.Contains(err.Error(), huge) {
		t.Errorf("the oversized reason was echoed back in the error, which lets a caller size an operator's log")
	}
}

// TestInviteWrongSecretIsRejected pins the bearer-credential check.
func TestInviteWrongSecretIsRejected(t *testing.T) {
	st, fl, _ := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})

	// A well-formed secret that is simply not this invite's.
	other, err := invite.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	cases := []struct {
		name   string
		secret string
	}{
		{"a different, well-formed secret", other},
		{"the correct secret with one character changed", flipLast(m.Secret)},
		{"the correct secret truncated by one byte", m.Secret[:len(m.Secret)-1]},
		{"the correct secret with a byte appended", m.Secret + "A"},
		{"an empty secret", ""},
		{"a secret over MaxSecretLen", strings.Repeat("s", invite.MaxSecretLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := invite.RedeemRequest{InviteID: m.ID, Secret: tc.secret, Key: testKey, Fingerprint: fingerprintOf("payload")}
			r, err := st.Begin(req)
			if !errors.Is(err, invite.ErrUnknownInvite) {
				t.Fatalf("Begin with %s gave err = %v, want ErrUnknownInvite: a wrong secret and an unknown id are deliberately the SAME answer, so a caller holding no secret learns nothing", tc.name, err)
			}
			if r != nil {
				t.Fatalf("Begin returned a non-nil Redemption alongside its error")
			}
			if strings.Contains(err.Error(), tc.secret) && tc.secret != "" {
				t.Fatalf("the presented secret appears in the error string; a secret must never be echoed, at any length")
			}
		})
	}

	// The invite is untouched and still redeemable by whoever really holds the
	// secret: a wrong guess must not burn a live invite.
	if n := fl.len(); n != 1 {
		t.Errorf("the durable log holds %d entries, want 1 (the mint alone): a wrong secret wrote something", n)
	}
	if got := mustLookup(t, st, m.ID, "after the wrong-secret attempts").State; got != invite.StateOpen {
		t.Errorf("after six wrong-secret attempts the invite is %s, want open", got)
	}
	if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); err != nil {
		t.Errorf("redeeming with the CORRECT secret after six wrong guesses gave err = %v, want success", err)
	}
}

// flipLast returns s with its final character changed, keeping the length.
func flipLast(s string) string {
	if s == "" {
		return "x"
	}
	last := s[len(s)-1]
	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}
	return s[:len(s)-1] + string(repl)
}

// TestInviteUnknownInviteIsRejected covers every shape of "no such invite",
// including the ones that must be refused before any lookup happens at all.
func TestInviteUnknownInviteIsRejected(t *testing.T) {
	st, _, _ := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})

	oversizedID := "inv-" + strings.Repeat("a", invite.MaxInviteIDLen)

	cases := []struct {
		name string
		req  invite.RedeemRequest
		want error
	}{
		{
			name: "a well-formed id that was never minted",
			req:  invite.RedeemRequest{InviteID: "inv-zzzzzzzzzzzzzzzz", Secret: m.Secret, Key: testKey, Fingerprint: fingerprintOf("p")},
			want: invite.ErrUnknownInvite,
		},
		{
			name: "an empty id",
			req:  invite.RedeemRequest{InviteID: "", Secret: m.Secret, Key: testKey, Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidInviteID,
		},
		{
			name: "an id that is not an invite id at all",
			req:  invite.RedeemRequest{InviteID: "not-an-invite", Secret: m.Secret, Key: testKey, Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidInviteID,
		},
		{
			name: "an id carrying the qualifier separator",
			req:  invite.RedeemRequest{InviteID: "inv-aaaaaaaa.aaaaaaa", Secret: m.Secret, Key: testKey, Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidInviteID,
		},
		{
			name: "an oversized id, rejected before the regexp engine ever sees it",
			req:  invite.RedeemRequest{InviteID: oversizedID, Secret: m.Secret, Key: testKey, Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidInviteID,
		},
		{
			name: "a missing idempotency key",
			req:  invite.RedeemRequest{InviteID: m.ID, Secret: m.Secret, Key: "", Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidRecord,
		},
		{
			name: "an idempotency key outside the charset",
			req:  invite.RedeemRequest{InviteID: m.ID, Secret: m.Secret, Key: "key with spaces", Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidRecord,
		},
		{
			name: "an oversized idempotency key",
			req:  invite.RedeemRequest{InviteID: m.ID, Secret: m.Secret, Key: strings.Repeat("k", idem.MaxKeyLen+1), Fingerprint: fingerprintOf("p")},
			want: invite.ErrInvalidRecord,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := st.Begin(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Begin gave (%v, %v), want %v", r, err, tc.want)
			}
			if r != nil {
				t.Fatalf("Begin returned a non-nil Redemption alongside its error")
			}
			if _, err := st.Redeem(tc.req, standardResult()); !errors.Is(err, tc.want) {
				t.Fatalf("Redeem gave err = %v, want %v", err, tc.want)
			}
		})
	}

	if strings.Contains(mustBeginErr(t, st, invite.RedeemRequest{InviteID: oversizedID, Secret: m.Secret, Key: testKey}), oversizedID) {
		t.Errorf("the oversized invite id was echoed back in the error; attacker-chosen input must not be quoted into an operator's log")
	}

	if _, ok := st.Lookup("inv-zzzzzzzzzzzzzzzz"); ok {
		t.Errorf("Lookup of an unminted id reported found")
	}
}

// mustBeginErr returns the error text of a Begin that is expected to fail.
func mustBeginErr(t *testing.T, st *invite.Store, req invite.RedeemRequest) string {
	t.Helper()
	_, err := st.Begin(req)
	if err == nil {
		t.Fatalf("Begin(%s) succeeded, want an error", req.InviteID)
	}
	return err.Error()
}

// TestInviteAlreadyRedeemedIsRejected is the single-use property itself, inside
// one process lifetime.
func TestInviteAlreadyRedeemedIsRejected(t *testing.T) {
	st, fl, clk := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})
	clk.Advance(time.Minute)
	first := mustRedeem(t, st, redeemReq(m, testKey, "payload-a"))

	afterFirst := fl.len()

	// A DIFFERENT key is a second, genuine attempt to spend the invite.
	if _, err := st.Begin(redeemReq(m, testOtherKey, "payload-b")); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("a second redemption under a different key gave err = %v, want ErrAlreadyRedeemed", err)
	}
	if _, err := st.Redeem(redeemReq(m, testOtherKey, "payload-b"), invite.Result{AgentID: "bus-test.beta", Response: json.RawMessage(`{"agent_id":"bus-test.beta"}`)}); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("a second Redeem under a different key gave err = %v, want ErrAlreadyRedeemed", err)
	}

	if n := fl.len(); n != afterFirst {
		t.Errorf("the refused second redemption wrote %d durable entries, want 0", n-afterFirst)
	}
	after := mustLookup(t, st, m.ID, "after the refused second redemption")
	assertRecordEqual(t, "the invite after a refused second redemption", after, first)
}

// TestInviteKeyReuseDifferentPayloadIsRejected pins invariant 10's distinction,
// which must NOT be collapsed: same key + same payload is a legitimate retry to
// be ANSWERED; same key + different payload is a protocol violation that gets
// the client disconnected.
func TestInviteKeyReuseDifferentPayloadIsRejected(t *testing.T) {
	st, fl, clk := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})
	clk.Advance(time.Minute)
	mustRedeem(t, st, redeemReq(m, testKey, "the original payload"))
	afterFirst := fl.len()

	_, err := st.Begin(redeemReq(m, testKey, "a DIFFERENT payload under the same key"))
	if !errors.Is(err, invite.ErrKeyReuse) {
		t.Fatalf("the same key with a different payload gave err = %v, want ErrKeyReuse", err)
	}
	// The two sentinels demand OPPOSITE reactions — "you are too late" versus
	// "you are misbehaving" — so a caller that matched only ErrAlreadyRedeemed
	// must not be able to swallow a violation as an ordinary loser.
	if errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Errorf("ErrKeyReuse also matches ErrAlreadyRedeemed; the two must stay distinct, or a protocol violation is indistinguishable from a client that simply arrived second")
	}
	if _, err := st.Redeem(redeemReq(m, testKey, "a DIFFERENT payload under the same key"), standardResult()); !errors.Is(err, invite.ErrKeyReuse) {
		t.Fatalf("Redeem with a reused key and a different payload gave err = %v, want ErrKeyReuse", err)
	}
	if n := fl.len(); n != afterFirst {
		t.Errorf("the refused key reuse wrote %d durable entries, want 0", n-afterFirst)
	}

	// And the legitimate retry still works afterwards: detecting the violation
	// must not have poisoned the invite for the honest client.
	r, err := st.Begin(redeemReq(m, testKey, "the original payload"))
	if err != nil || r.Outcome() != invite.OutcomeReplay {
		t.Fatalf("after a refused key reuse the legitimate retry gave (%v, %v), want a replay", r, err)
	}
}

// TestInviteRetryReplaysTheOriginalResult is the legitimate-retry carve-out: the
// ack was lost in flight, the client retries, and it gets the ORIGINAL result
// back with nothing re-applied and no disconnection.
func TestInviteRetryReplaysTheOriginalResult(t *testing.T) {
	st, fl, clk := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})
	clk.Advance(time.Minute)
	first := mustRedeem(t, st, redeemReq(m, testKey, "payload"))
	afterFirst := fl.len()

	// Through Begin: a replay reserves NOTHING and answers with the original.
	r, err := st.Begin(redeemReq(m, testKey, "payload"))
	if err != nil {
		t.Fatalf("the legitimate retry gave err = %v, want the original result: punishing a correct retry breaks exactly the clients doing the right thing", err)
	}
	if r.Outcome() != invite.OutcomeReplay {
		t.Fatalf("the retry has outcome %s, want replay", r.Outcome())
	}
	if !bytes.Equal(r.Result(), testResult) {
		t.Fatalf("the retry replayed %s, want the original %s verbatim", r.Result(), testResult)
	}

	// Result() hands back a COPY: a caller that scribbles on the response it was
	// given must not be able to rewrite the stored record of a credential.
	got := r.Result()
	if len(got) > 0 {
		got[0] = 'X'
	}
	if !bytes.Equal(mustLookup(t, st, m.ID, "after mutating a replayed result").Result, testResult) {
		t.Fatalf("mutating the slice Result() returned changed the STORED result")
	}

	// A replay may not be consumed: there is nothing to spend.
	if _, err := r.Consume(standardResult()); err == nil {
		t.Fatalf("Consume on a replay succeeded, want a refusal: a replay reserved nothing and a second consumption record would be a second redemption")
	}
	// Commit and Abort on a replay are no-ops, not errors and not an un-spend.
	if err := r.Commit(); err != nil {
		t.Errorf("Commit on a replay gave err = %v, want nil", err)
	}
	r.Abort()
	if got := mustLookup(t, st, m.ID, "after Abort on a replay").State; got != invite.StateRedeemed {
		t.Fatalf("after Abort on a replay the invite is %s, want redeemed: Abort must never un-spend", got)
	}

	// Through the standalone path: same answer, still nothing written.
	rec, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult())
	if err != nil {
		t.Fatalf("Redeem as a retry gave err = %v, want the original record", err)
	}
	assertRecordEqual(t, "the record a retry returns", rec, first)
	if n := fl.len(); n != afterFirst {
		t.Errorf("the retries wrote %d durable entries, want 0: a replay must re-apply nothing", n-afterFirst)
	}

	// The retry horizon deliberately OUTLIVES the invite: a retry arriving after
	// ExpiresAt still replays, because expiry gates a FRESH redemption only.
	clk.Advance(invite.DefaultTTL + time.Hour)
	r2, err := st.Begin(redeemReq(m, testKey, "payload"))
	if err != nil {
		t.Fatalf("a retry arriving after the invite's own expiry gave err = %v, want the original result: SpentRetention is longer than the TTL precisely so this works", err)
	}
	if r2.Outcome() != invite.OutcomeReplay || !bytes.Equal(r2.Result(), testResult) {
		t.Fatalf("the post-expiry retry gave outcome %s / result %s, want a replay of the original", r2.Outcome(), r2.Result())
	}
}

// TestInviteConcurrentRedeemYieldsExactlyOneSuccess is the property that makes
// concurrent double-redemption impossible rather than merely unlikely.
//
// It runs under -race, so it is also the data-race check on the store lock,
// the inflight map and the pending-mint counter.
func TestInviteConcurrentRedeemYieldsExactlyOneSuccess(t *testing.T) {
	const racers = 24

	st, fl, _ := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})

	type attempt struct {
		key string
		rec invite.Record
		err error
	}
	results := make([]attempt, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		i := i
		key := "k-racer-" + string(rune('a'+i%26)) + "-" + string(rune('a'+i/26))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec, err := st.Redeem(invite.RedeemRequest{
				InviteID:    m.ID,
				Secret:      m.Secret,
				Key:         key,
				Fingerprint: fingerprintOf(key),
			}, invite.Result{AgentID: testBusID + "." + key, Response: json.RawMessage(`{"k":"` + key + `"}`)})
			results[i] = attempt{key: key, rec: rec, err: err}
		}()
	}
	close(start)
	wg.Wait()

	var winners []attempt
	for _, a := range results {
		if a.err == nil {
			winners = append(winners, a)
			continue
		}
		// Every loser must lose for one of the two legitimate reasons.
		if !errors.Is(a.err, invite.ErrRedemptionInFlight) && !errors.Is(a.err, invite.ErrAlreadyRedeemed) {
			t.Errorf("racer %s failed with err = %v, want ErrRedemptionInFlight or ErrAlreadyRedeemed", a.key, a.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d of %d concurrent redemptions succeeded, want EXACTLY 1: a single-use invite admitted %d agents", len(winners), racers, len(winners))
	}

	// Exactly one consumption record reached the durable log, and the serving
	// copy names the same winner.
	recs := fl.records(t)
	var redeemed int
	for _, r := range recs {
		if r.State == invite.StateRedeemed {
			redeemed++
		}
	}
	if redeemed != 1 {
		t.Fatalf("the durable log holds %d redemption records, want exactly 1", redeemed)
	}
	final := mustLookup(t, st, m.ID, "after the race")
	if final.State != invite.StateRedeemed {
		t.Fatalf("after the race the invite is %s, want redeemed", final.State)
	}
	if final.RedeemKey != winners[0].key || final.RedeemedBy != winners[0].rec.RedeemedBy {
		t.Fatalf("the stored record names key %q / agent %q, but the winner was %q / %q",
			final.RedeemKey, final.RedeemedBy, winners[0].key, winners[0].rec.RedeemedBy)
	}

	// And after the dust settles it is simply spent.
	if _, err := st.Begin(redeemReq(m, "k-after-the-race", "payload")); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("a redemption after the race gave err = %v, want ErrAlreadyRedeemed", err)
	}
}

// TestInviteCapacityFailsClosed pins the bus-wide cap: it REFUSES and evicts
// NOTHING. An eviction would be safe in the single-use sense (an evicted invite
// is unknown and therefore unredeemable) but it would silently break an
// operator's live invite, and a refused mint is loud and recoverable.
func TestInviteCapacityFailsClosed(t *testing.T) {
	const limit = 3
	st, fl, _ := newFakeStore(t, func(o *invite.StoreOptions) { o.MaxInvites = limit })

	live := make([]invite.Minted, 0, limit)
	for i := 0; i < limit; i++ {
		live = append(live, mustMint(t, st, invite.MintRequest{Label: "live"}))
	}

	if _, err := st.Mint(invite.MintRequest{Label: "one too many"}); !errors.Is(err, invite.ErrCapacity) {
		t.Fatalf("minting past the cap gave err = %v, want ErrCapacity", err)
	}
	if n := fl.len(); n != limit {
		t.Errorf("the durable log holds %d entries, want %d: the refused mint wrote a record", n, limit)
	}

	// NOTHING was evicted, and every live invite still works.
	for _, m := range live {
		if _, ok := st.Lookup(m.ID); !ok {
			t.Fatalf("invite %s was EVICTED to make room; the cap must refuse, never evict", m.ID)
		}
	}
	if _, err := st.Redeem(redeemReq(live[0], testKey, "payload"), standardResult()); err != nil {
		t.Errorf("redeeming a live invite after a capacity refusal gave err = %v, want success", err)
	}

	// The cap is enforced on the REPLAY path too — it is a memory bound, and a
	// bound one path could exceed is not a bound. The over-cap record is
	// DISCARDED, which makes that invite UNKNOWN and therefore unredeemable.
	extra := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	over := invite.Record{
		ID:           "inv-qqqqqqqqqqqqqqqq",
		BusID:        testBusID,
		SecretDigest: invite.HashSecret("a secret for the over-cap record"),
		CreatedAt:    extra,
		ExpiresAt:    extra.Add(time.Hour),
		State:        invite.StateOpen,
	}
	if err := st.Apply(committedRecord(t, over, 101, 102)); err != nil {
		t.Fatalf("Apply of an over-capacity record returned err = %v; Apply must NEVER return non-nil (it would poison the log or refuse to boot)", err)
	}
	if _, ok := st.Lookup(over.ID); ok {
		t.Fatalf("the over-capacity record was applied anyway; the cap is not enforced on the replay path")
	}
	if _, err := st.Begin(invite.RedeemRequest{
		InviteID:    over.ID,
		Secret:      "a secret for the over-cap record",
		Key:         testKey,
		Fingerprint: fingerprintOf("payload"),
	}); !errors.Is(err, invite.ErrUnknownInvite) {
		t.Fatalf("the discarded over-capacity invite answers err = %v, want ErrUnknownInvite: every drop must be fail-closed", err)
	}
}

// TestInviteDropIsFailClosed is the rule doc.go section 5 states and this test
// exists to prove: EVERY DROP MAKES THE INVITE UNKNOWN, AND AN UNKNOWN INVITE IS
// REJECTED.
//
// The dangerous direction is the SPENT one. If a forgotten redemption came back
// as an open invite, retention — a memory optimisation — would have manufactured
// a second admission to the bus. This is the exact opposite of idem's
// applied-key table, where forgetting a key fails OPEN.
func TestInviteDropIsFailClosed(t *testing.T) {
	t.Run("a forgotten SPENT invite is unknown, never open", func(t *testing.T) {
		st, fl, clk := newFakeStore(t, nil)
		m := mustMint(t, st, invite.MintRequest{TTL: time.Hour})
		clk.Advance(time.Minute)
		mustRedeem(t, st, redeemReq(m, testKey, "payload"))
		afterFirst := fl.len()

		// Past RedeemedAt + SpentRetention the record is dropped.
		clk.Advance(invite.SpentRetention + time.Hour)
		if _, ok := st.Lookup(m.ID); ok {
			t.Fatalf("the spent record survived %s past its redemption; the retention predicate did not fire", invite.SpentRetention)
		}

		// THE ASSERTION THAT MATTERS: presenting the CORRECT secret, with the
		// original key, gets UNKNOWN — not a fresh reservation.
		for _, key := range []string{testKey, testOtherKey} {
			r, err := st.Begin(redeemReq(m, key, "payload"))
			if !errors.Is(err, invite.ErrUnknownInvite) {
				t.Fatalf("redeeming a DROPPED spent invite under key %q gave (%v, %v), want ErrUnknownInvite; anything else is a SECOND redemption produced by retention", key, r, err)
			}
			if r != nil {
				t.Fatalf("Begin on a dropped spent invite returned a Redemption")
			}
		}
		if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); !errors.Is(err, invite.ErrUnknownInvite) {
			t.Fatalf("Redeem of a dropped spent invite gave err = %v, want ErrUnknownInvite", err)
		}
		if n := fl.len(); n != afterFirst {
			t.Fatalf("redeeming a dropped invite wrote %d durable entries, want 0", n-afterFirst)
		}
	})

	t.Run("a forgotten OPEN invite is unknown", func(t *testing.T) {
		st, _, clk := newFakeStore(t, nil)
		m := mustMint(t, st, invite.MintRequest{TTL: time.Hour})

		// Still diagnosable as EXPIRED inside the retention window ...
		clk.Advance(2 * time.Hour)
		if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrExpired) {
			t.Fatalf("inside the retention window an expired invite answers %v, want ErrExpired", err)
		}
		// ... and simply UNKNOWN once it is dropped. It loses diagnosability,
		// never safety: both answers are refusals.
		clk.Advance(invite.SpentRetention + time.Hour)
		if _, ok := st.Lookup(m.ID); ok {
			t.Fatalf("the expired record survived ExpiresAt + %s", invite.SpentRetention)
		}
		if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrUnknownInvite) {
			t.Fatalf("a dropped expired invite answers %v, want ErrUnknownInvite", err)
		}
	})

	t.Run("a forgotten REVOKED invite is unknown", func(t *testing.T) {
		st, _, clk := newFakeStore(t, nil)
		m := mustMint(t, st, invite.MintRequest{TTL: time.Hour})
		if _, err := st.Revoke(m.ID, "withdrawn"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		clk.Advance(time.Hour + invite.SpentRetention + time.Hour)
		if _, ok := st.Lookup(m.ID); ok {
			t.Fatalf("the revoked record survived its retention")
		}
		if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrUnknownInvite) {
			t.Fatalf("a dropped revoked invite answers %v, want ErrUnknownInvite", err)
		}
	})

	t.Run("a record discarded at replay is unknown", func(t *testing.T) {
		st, _, _ := newFakeStore(t, nil)
		// A body that cannot be decoded: the invite it described never enters the
		// table at all, so nothing can ever redeem it.
		bad := wal.Committed{
			PrepareIndex: 7, CommitIndex: 8,
			Entry: wal.Entry{Kind: invite.RecordKind, Body: json.RawMessage(`{"id":"inv-dddddddddddddddd","bus":"bus-test"`)},
		}
		if err := st.Apply(bad); err != nil {
			t.Fatalf("Apply of an undecodable record returned err = %v, want nil", err)
		}
		if _, ok := st.Lookup("inv-dddddddddddddddd"); ok {
			t.Fatalf("an undecodable record was applied")
		}
		if _, err := st.Begin(invite.RedeemRequest{InviteID: "inv-dddddddddddddddd", Secret: "anything at all", Key: testKey, Fingerprint: fingerprintOf("p")}); !errors.Is(err, invite.ErrUnknownInvite) {
			t.Fatalf("an invite discarded at replay answers %v, want ErrUnknownInvite", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The replay half
// ---------------------------------------------------------------------------

// committedRecord wraps a Record as the wal.Committed a replay would hand to
// Apply. It encodes through the REAL Encode, so the bytes are the bytes.
func committedRecord(t *testing.T, r invite.Record, prepare, commit uint64) wal.Committed {
	t.Helper()
	body, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode(%s): %v", r.ID, err)
	}
	return wal.Committed{PrepareIndex: prepare, CommitIndex: commit, Entry: wal.Entry{Kind: invite.RecordKind, Body: body}}
}

// replayFixture is one invite in three states, sharing an id and a digest, for
// the monotonicity table.
type replayFixture struct {
	open     invite.Record
	redeemed invite.Record
	revoked  invite.Record
	secret   string
}

func newReplayFixture(id string) replayFixture {
	secret := "the secret behind " + id
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	open := invite.Record{
		ID:           id,
		BusID:        testBusID,
		SecretDigest: invite.HashSecret(secret),
		Label:        "fixture",
		CreatedAt:    base,
		ExpiresAt:    base.Add(24 * time.Hour),
		State:        invite.StateOpen,
	}
	redeemed := open
	redeemed.State = invite.StateRedeemed
	redeemed.RedeemedAt = base.Add(time.Minute)
	redeemed.RedeemedBy = testAgentID
	redeemed.RedeemKey = testKey
	redeemed.RedeemFingerprint = fingerprintOf("payload")
	redeemed.Result = testResult
	redeemed.CertFingerprint = testCertFingerprint

	revoked := open
	revoked.State = invite.StateRevoked
	revoked.RevokedAt = base.Add(2 * time.Minute)
	revoked.RevokedReason = "withdrawn"

	return replayFixture{open: open, redeemed: redeemed, revoked: revoked, secret: secret}
}

// TestInviteMonotonicTransitionsOnly pins the upsert rule that stops a
// reordered, replayed or forged record from resurrecting a spent invite.
//
// Allowed: an insert, a re-apply of the same record, open -> redeemed,
// open -> revoked. Everything else is refused and the invite keeps the state it
// already had.
func TestInviteMonotonicTransitionsOnly(t *testing.T) {
	f := newReplayFixture("inv-mmmmmmmmmmmmmmmm")

	// A second, DIFFERENT redemption of the same invite.
	otherRedemption := f.redeemed
	otherRedemption.RedeemedAt = f.redeemed.RedeemedAt.Add(time.Hour)
	otherRedemption.RedeemedBy = "bus-test.intruder"
	otherRedemption.RedeemKey = testOtherKey
	otherRedemption.RedeemFingerprint = fingerprintOf("another payload")
	otherRedemption.Result = json.RawMessage(`{"agent_id":"bus-test.intruder"}`)

	// The same id rebound to a different secret, and to a different bus.
	rebound := f.open
	rebound.SecretDigest = invite.HashSecret("an attacker's secret")
	otherBus := f.open
	otherBus.BusID = "bus-elsewhere"

	cases := []struct {
		name string
		// seed is applied first and must all succeed.
		seed []invite.Record
		// then is the record whose fate the case is about.
		then invite.Record
		// want is the state the invite must be in afterwards.
		want invite.State
		// applied says whether `then` was expected to take effect.
		applied bool
	}{
		{"an insert", nil, f.open, invite.StateOpen, true},
		{"open -> redeemed", []invite.Record{f.open}, f.redeemed, invite.StateRedeemed, true},
		{"open -> revoked", []invite.Record{f.open}, f.revoked, invite.StateRevoked, true},
		{"re-applying the same open record", []invite.Record{f.open}, f.open, invite.StateOpen, true},
		{"re-applying the same redemption", []invite.Record{f.open, f.redeemed}, f.redeemed, invite.StateRedeemed, true},
		{"re-applying the same revocation", []invite.Record{f.open, f.revoked}, f.revoked, invite.StateRevoked, true},

		{"redeemed -> open (the resurrection)", []invite.Record{f.open, f.redeemed}, f.open, invite.StateRedeemed, false},
		{"revoked -> open (the resurrection)", []invite.Record{f.open, f.revoked}, f.open, invite.StateRevoked, false},
		{"redeemed -> revoked", []invite.Record{f.open, f.redeemed}, f.revoked, invite.StateRedeemed, false},
		{"revoked -> redeemed", []invite.Record{f.open, f.revoked}, f.redeemed, invite.StateRevoked, false},
		{"a SECOND, different redemption", []invite.Record{f.open, f.redeemed}, otherRedemption, invite.StateRedeemed, false},
		{"the same id with a different secret digest", []invite.Record{f.open}, rebound, invite.StateOpen, false},
		{"the same id bound to a different bus", []invite.Record{f.open}, otherBus, invite.StateOpen, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _, _ := newFakeStore(t, nil)
			idx := uint64(1)
			for _, r := range tc.seed {
				if err := st.Apply(committedRecord(t, r, idx, idx+1)); err != nil {
					t.Fatalf("seeding: Apply returned err = %v, want nil", err)
				}
				idx += 2
			}
			if err := st.Apply(committedRecord(t, tc.then, idx, idx+1)); err != nil {
				t.Fatalf("Apply returned err = %v; Apply must NEVER return non-nil", err)
			}
			got := mustLookup(t, st, f.open.ID, tc.name)
			if got.State != tc.want {
				t.Fatalf("after %q the invite is %s, want %s", tc.name, got.State, tc.want)
			}
			// A refused record must not have leaked any of its fields in.
			if !tc.applied {
				switch tc.want {
				case invite.StateRedeemed:
					assertRecordEqual(t, "the invite after a refused transition", got, f.redeemed)
				case invite.StateRevoked:
					assertRecordEqual(t, "the invite after a refused transition", got, f.revoked)
				default:
					assertRecordEqual(t, "the invite after a refused transition", got, f.open)
				}
			}
		})
	}

	// And the property the whole rule exists for: after a refused resurrection
	// the invite is STILL SPENT to a live redemption attempt holding the correct
	// secret.
	st, _, _ := newFakeStore(t, nil)
	for i, r := range []invite.Record{f.open, f.redeemed, f.open} {
		if err := st.Apply(committedRecord(t, r, uint64(2*i+1), uint64(2*i+2))); err != nil {
			t.Fatalf("Apply %d returned err = %v, want nil", i, err)
		}
	}
	if _, err := st.Begin(invite.RedeemRequest{
		InviteID:    f.open.ID,
		Secret:      f.secret,
		Key:         testOtherKey,
		Fingerprint: fingerprintOf("payload"),
	}); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("after a replayed OPEN record followed the redemption, the invite answers %v, want ErrAlreadyRedeemed: a spent invite was resurrected by replay order", err)
	}
}

// TestInviteApplyNeverReturnsError is a hard requirement, not a preference. A
// non-nil return from Apply POISONS the log on a live write (wal.ErrDiverged)
// and makes Open FAIL during recovery — and invariant 6 settled that recovery
// always reaches a running server.
//
// So every malformed, over-cap and backwards input must return nil AND log
// loudly. Silent discard is the defect, not discard itself, so the test asserts
// an ERROR line was emitted too.
func TestInviteApplyNeverReturnsError(t *testing.T) {
	f := newReplayFixture("inv-nnnnnnnnnnnnnnnn")
	oversizedResult := json.RawMessage(`"` + strings.Repeat("x", idem.MaxResultBytes) + `"`)
	bigResult := f.redeemed
	bigResult.Result = oversizedResult

	cases := []struct {
		name string
		// body is the raw entry body. Non-empty bodies bypass Encode, so they can
		// be shapes Encode would never produce.
		body string
		// rec is encoded through the real Encode when body is empty.
		rec *invite.Record
		// kind overrides the entry kind.
		kind string
		// seed is applied before the case.
		seed []invite.Record
		// wantLogged says whether a loud ERROR line is required.
		wantLogged bool
	}{
		{name: "a body that is not JSON at all", body: `not json`, wantLogged: true},
		{name: "a truncated record", body: `{"id":"inv-aaaaaaaaaaaaaaaa","bus":"bus-test"`, wantLogged: true},
		{name: "an empty body", body: ``, wantLogged: true},
		{name: "a JSON null body", body: `null`, wantLogged: true},
		{name: "a JSON array body", body: `[]`, wantLogged: true},
		{name: "an unknown field", body: `{"id":"inv-aaaaaaaaaaaaaaaa","bus":"bus-test","secret_sha256":"` + hexDigest("s") + `","created_at":"2026-08-07T12:00:00Z","expires_at":"2026-08-08T12:00:00Z","state":"open","surprise":1}`, wantLogged: true},
		{name: "trailing data after the record", body: `{"id":"inv-aaaaaaaaaaaaaaaa","bus":"bus-test","secret_sha256":"` + hexDigest("s") + `","created_at":"2026-08-07T12:00:00Z","expires_at":"2026-08-08T12:00:00Z","state":"open"} {"id":"inv-bbbbbbbbbbbbbbbb"}`, wantLogged: true},
		{name: "a state that is not one of the three", body: `{"id":"inv-aaaaaaaaaaaaaaaa","bus":"bus-test","secret_sha256":"` + hexDigest("s") + `","created_at":"2026-08-07T12:00:00Z","expires_at":"2026-08-08T12:00:00Z","state":"expired"}`, wantLogged: true},
		{name: "an open record carrying a redemption", body: `{"id":"inv-aaaaaaaaaaaaaaaa","bus":"bus-test","secret_sha256":"` + hexDigest("s") + `","created_at":"2026-08-07T12:00:00Z","expires_at":"2026-08-08T12:00:00Z","state":"open","redeemed_by":"bus-test.alpha"}`, wantLogged: true},
		{name: "an all-zero secret digest", body: `{"id":"inv-aaaaaaaaaaaaaaaa","bus":"bus-test","secret_sha256":"` + strings.Repeat("0", 2*invite.DigestSize) + `","created_at":"2026-08-07T12:00:00Z","expires_at":"2026-08-08T12:00:00Z","state":"open"}`, wantLogged: true},
		{name: "an oversized stored result", rec: &bigResult, seed: []invite.Record{f.open}, wantLogged: true},
		{name: "a backwards transition", rec: &f.open, seed: []invite.Record{f.open, f.redeemed}, wantLogged: true},
		{name: "a second, different redemption", rec: &f.redeemed, seed: []invite.Record{f.open, f.revoked}, wantLogged: true},

		// A neighbour's record is skipped SILENTLY: this log carries messages,
		// roster entries and invites, and a store that treated its neighbours as
		// damage would fill the log with false alarms.
		{name: "an entry belonging to another package", kind: "message", body: `{"anything":"at all"}`, wantLogged: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			st, _, _ := newFakeStore(t, func(o *invite.StoreOptions) {
				o.Logger = logging.New(&logBuf, logging.LevelDebug)
			})
			idx := uint64(1)
			for _, r := range tc.seed {
				if err := st.Apply(committedRecord(t, r, idx, idx+1)); err != nil {
					t.Fatalf("seeding: Apply returned err = %v, want nil", err)
				}
				idx += 2
			}
			logBuf.Reset()

			kind := tc.kind
			if kind == "" {
				kind = invite.RecordKind
			}
			var body json.RawMessage
			if tc.rec != nil {
				enc, err := tc.rec.Encode()
				if err != nil {
					// Encode refusing is itself correct behaviour for the
					// oversized-result case: the record never becomes durable. Fall
					// back to a hand-built body so Apply still sees the shape.
					enc = json.RawMessage(mustJSON(t, tc.rec))
				}
				body = enc
			} else {
				body = json.RawMessage(tc.body)
			}

			err := st.Apply(wal.Committed{PrepareIndex: idx, CommitIndex: idx + 1, Entry: wal.Entry{Kind: kind, Body: body}})
			if err != nil {
				t.Fatalf("Apply returned err = %v, want nil ALWAYS: a non-nil return poisons the log on a live write and refuses to boot on recovery", err)
			}

			logged := strings.Contains(logBuf.String(), "level=error")
			if logged != tc.wantLogged {
				t.Fatalf("Apply logged an ERROR line = %v, want %v; silent discard is the defect, not discard itself\n--- log ---\n%s", logged, tc.wantLogged, logBuf.String())
			}
			if tc.wantLogged && !strings.Contains(logBuf.String(), "DISCARDING") {
				t.Errorf("the discard line does not say what happened:\n%s", logBuf.String())
			}
		})
	}
}

// hexDigest is the hex form of a secret's digest, for hand-built record bodies.
func hexDigest(secret string) string {
	d := invite.HashSecret(secret)
	return hex.EncodeToString(d[:])
}

// mustJSON marshals a value for a hand-built body.
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// The secret
// ---------------------------------------------------------------------------

// TestInviteMintNeverStoresTheSecret is concrete evidence for the claim
// secret.go makes, not a restatement of it: mint through a REAL WAL, then read
// bus.wal off the disk and look for the plaintext.
//
// The secret is a BEARER CREDENTIAL and the WAL is the one file that outlives
// the process. A secret that leaked into it would be an admission ticket
// readable by anybody who could read the data directory, permanently, because
// the log is append-only and nothing may edit it in place.
func TestInviteMintNeverStoresTheSecret(t *testing.T) {
	dir := t.TempDir()
	clk := newTestClock()
	st, lg := openStore(t, dir, clk)
	defer func() { _ = lg.Close() }()

	minted := make([]invite.Minted, 0, 3)
	for i := 0; i < 3; i++ {
		minted = append(minted, mustMint(t, st, invite.MintRequest{Label: "leak check"}))
	}
	// Spend one and revoke another, so the check covers every record shape a
	// redemption or revocation could rewrite.
	clk.Advance(time.Minute)
	mustRedeem(t, st, redeemReq(minted[0], testKey, "payload"))
	if _, err := st.Revoke(minted[1].ID, "withdrawn"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	raw, err := readWAL(dir)
	if err != nil {
		t.Fatalf("reading the durable log: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("bus.wal is empty; this test would pass vacuously against a log nothing was ever written to")
	}

	for i, m := range minted {
		// The digest IS there. This is the control: without it the test could
		// pass just as happily against the wrong file or an unwritten record.
		d := m.SecretDigest
		if !bytes.Contains(raw, []byte(hex.EncodeToString(d[:]))) {
			t.Fatalf("invite %d's secret DIGEST is not in bus.wal; the test is not looking at the record it thinks it is", i)
		}
		// The plaintext is NOT — in its encoded form ...
		if bytes.Contains(raw, []byte(m.Secret)) {
			t.Fatalf("invite %d's PLAINTEXT SECRET is in bus.wal; the one file that outlives the process holds a live bearer credential", i)
		}
		// ... nor as the raw bytes behind that encoding, in case some other
		// encoding of the same value slipped in.
		rawSecret, err := decodeSecret(m.Secret)
		if err != nil {
			t.Fatalf("invite %d: the minted secret is not base64.RawURLEncoding: %v", i, err)
		}
		if bytes.Contains(raw, rawSecret) {
			t.Fatalf("invite %d's secret appears in bus.wal as raw bytes", i)
		}
		// ... and neither does any 16-character run of it, which would be enough
		// to make the rest brute-forceable and is the shape a "just a prefix for
		// debugging" log line takes.
		if len(m.Secret) >= 16 && bytes.Contains(raw, []byte(m.Secret[:16])) {
			t.Fatalf("invite %d: a 16-character PREFIX of the secret is in bus.wal", i)
		}
	}

	// The label is operator text and does reach the log — asserted so the test
	// above cannot be passing because nothing at all was written.
	if !bytes.Contains(raw, []byte("leak check")) {
		t.Fatalf("the invite label is not in bus.wal, so the records under test are not there either")
	}
}

// ---------------------------------------------------------------------------
// Mint and durability preconditions
// ---------------------------------------------------------------------------

// TestInviteMintValidatesItsRequest pins the mint-side refusals, in particular
// that an over-long TTL is REJECTED and never silently clamped: quietly issuing
// a shorter-lived credential than the operator asked for is how an invite
// mysteriously stops working an hour before it is needed.
func TestInviteMintValidatesItsRequest(t *testing.T) {
	st, fl, clk := newFakeStore(t, nil)

	// The zero TTL means "unset" and takes DefaultTTL.
	m := mustMint(t, st, invite.MintRequest{})
	if want := clk.Now().Add(invite.DefaultTTL); !m.ExpiresAt.Equal(want) {
		t.Errorf("a mint with no TTL expires at %s, want %s (DefaultTTL)", m.ExpiresAt, want)
	}

	oversizedLabel := strings.Repeat("L", invite.MaxLabelLen+1)
	cases := []struct {
		name string
		req  invite.MintRequest
		want error
	}{
		{"a negative TTL", invite.MintRequest{TTL: -time.Second}, invite.ErrInvalidTTL},
		{"a TTL over MaxTTL", invite.MintRequest{TTL: invite.MaxTTL + time.Second}, invite.ErrInvalidTTL},
		{"an oversized label", invite.MintRequest{Label: oversizedLabel}, invite.ErrInvalidRecord},
	}
	before := fl.len()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.Mint(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Mint(%+v) gave err = %v, want %v", tc.req, err, tc.want)
			}
			if tc.req.Label != "" && strings.Contains(err.Error(), oversizedLabel) {
				t.Errorf("the oversized label was echoed back in the error")
			}
		})
	}
	if n := fl.len(); n != before {
		t.Errorf("the refused mints wrote %d durable entries, want 0", n-before)
	}

	// Exactly MaxTTL is accepted: the bound is inclusive, and an operator asking
	// for the documented maximum must not be refused by an off-by-one.
	if _, err := st.Mint(invite.MintRequest{TTL: invite.MaxTTL}); err != nil {
		t.Errorf("Mint with exactly MaxTTL gave err = %v, want success", err)
	}
}

// TestInviteNotDurableIsRefused pins ErrNotDurable as a REFUSAL rather than a
// degraded in-memory mode: single-use held only in memory is decorative,
// because a restart would forget every spent invite.
func TestInviteNotDurableIsRefused(t *testing.T) {
	// The clock is injected and pinned to newReplayFixture's base time rather
	// than left as the default (real time.Now): the replayed record below
	// retires SpentRetention after its ExpiresAt, and a store evaluating that
	// window against the WALL clock would eventually — and did — start
	// dropping the fixture out from under this test as the calendar moved
	// past that retirement instant, long after the fixture was written.
	clk := newTestClock()
	st, err := invite.NewStore(invite.StoreOptions{BusID: testBusID, Now: clk.Now})
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}

	if _, err := st.Mint(invite.MintRequest{}); !errors.Is(err, invite.ErrNotDurable) {
		t.Errorf("Mint on a store with no durable log gave err = %v, want ErrNotDurable", err)
	}
	if _, err := st.Revoke("inv-aaaaaaaaaaaaaaaa", "x"); !errors.Is(err, invite.ErrNotDurable) {
		t.Errorf("Revoke on a store with no durable log gave err = %v, want ErrNotDurable", err)
	}
	if _, err := st.Redeem(invite.RedeemRequest{InviteID: "inv-aaaaaaaaaaaaaaaa", Secret: "s", Key: testKey}, standardResult()); !errors.Is(err, invite.ErrNotDurable) {
		t.Errorf("Redeem on a store with no durable log gave err = %v, want ErrNotDurable", err)
	}

	// A store with no log may still be READ and REBUILT: neither claims
	// durability, and recovery has to work before a writer exists.
	f := newReplayFixture("inv-pppppppppppppppp")
	if err := st.Apply(committedRecord(t, f.open, 1, 2)); err != nil {
		t.Fatalf("Apply on a store with no durable log gave err = %v, want nil", err)
	}
	if _, ok := st.Lookup(f.open.ID); !ok {
		t.Fatalf("Lookup on a store with no durable log did not find the replayed record")
	}

	// A store built without a bus id cannot say which bus an invite admits to.
	st2, err := invite.NewStore(invite.StoreOptions{Durable: &fakeLog{}})
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	if _, err := st2.Mint(invite.MintRequest{}); !errors.Is(err, invite.ErrInvalidRecord) {
		t.Errorf("Mint on a store with no bus id gave err = %v, want ErrInvalidRecord", err)
	}
}

// TestInviteFailedDurableWriteSpendsNothing pins invariant 4 from the other
// side: if the write path fails, NOTHING is acknowledged and nothing changed.
func TestInviteFailedDurableWriteSpendsNothing(t *testing.T) {
	st, fl, clk := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{})
	clk.Advance(time.Minute)

	fl.failWith(errors.New("the disk is on fire"))
	if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); err == nil {
		t.Fatalf("Redeem over a failing durable log succeeded; nothing may be acknowledged before it is durable")
	}
	if got := mustLookup(t, st, m.ID, "after a failed durable write").State; got != invite.StateOpen {
		t.Fatalf("after a failed durable write the invite is %s, want open: memory recorded a spend the disk never accepted", got)
	}

	// The reservation was released, so the invite is usable once the disk
	// recovers — a failed write must not strand a live invite forever.
	fl.failWith(nil)
	if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); err != nil {
		t.Fatalf("redeeming after the durable log recovered gave err = %v, want success", err)
	}
}

// ---------------------------------------------------------------------------
// The TWO-PHASE PARTICIPANT API — the door INVITE-GATE actually uses
// ---------------------------------------------------------------------------
//
// Everything above this line that spends an invite goes through Store.Redeem,
// which is the STANDALONE path INVITE-GATE is explicitly forbidden to use: it
// writes a transaction of its OWN, so it can never be atomic with the roster
// write that creates the agent. The participant sequence — Begin, Consume, the
// CALLER's write, Commit — is the one the gate will run, and it is the one that
// has to be proved.
//
// These tests therefore never call Redeem. The test composes and writes the
// wal.Entry itself, exactly as INVITE-GATE will.

// enrolKind is the wal.Entry.Kind of the COMPOSITE entry INVITE-GATE will write:
// ONE transaction carrying BOTH the roster record that creates the agent and the
// invite consumption record that spends the invite.
//
// Its shape is a stand-in — AUTH-3 owns the real one — but its STRUCTURE is the
// load-bearing part and is not a stand-in at all: a wal.Entry is exactly one
// transaction, so putting both records in one Body is what makes "the invite is
// spent" and "the agent exists" commit or fail together. A test that wrote the
// consumption record in its own entry would be re-testing Store.Redeem under a
// different name and would prove nothing about the window the participant API
// exists to close.
const enrolKind = "test-enrol"

// enrolBody is that composite payload.
type enrolBody struct {
	Roster json.RawMessage `json:"roster"`
	Invite json.RawMessage `json:"invite"`
}

// enrolEntry composes the entry the CALLER writes: the roster half it minted,
// plus the consumption record Redemption.Consume handed back, verbatim.
func enrolEntry(t *testing.T, agentID string, consumption json.RawMessage) wal.Entry {
	t.Helper()
	if len(consumption) == 0 {
		t.Fatalf("Consume returned an empty body; there is nothing to make durable")
	}
	roster := json.RawMessage(mustJSON(t, map[string]string{"agent_id": agentID}))
	body := enrolBody{Roster: roster, Invite: consumption}
	return wal.Entry{Kind: enrolKind, Body: json.RawMessage(mustJSON(t, body))}
}

// enrolApplier is the wal.Applier a server composing enrolments installs: it
// routes a plain invite entry straight to the store and DECOMPOSES a composite
// enrolment entry, handing the invite half to the same store.
//
// It is what makes recovery meaningful here. Without it the invite half of a
// composite entry would be invisible to Store.Apply — a fresh process would
// replay the log and conclude the invite was never spent, which is precisely the
// second redemption these tests exist to rule out.
//
// Like Store.Apply it NEVER returns a non-nil error: a non-nil return poisons
// the log on a live write and refuses to boot on recovery.
type enrolApplier struct {
	st *invite.Store
	// agents is what the roster half created, in commit order. It is the
	// evidence that both halves rode the SAME transaction.
	agents []string
}

func (a *enrolApplier) Apply(c wal.Committed) error {
	if c.Entry.Kind != enrolKind {
		return a.st.Apply(c)
	}
	var b enrolBody
	if err := json.Unmarshal(c.Entry.Body, &b); err != nil {
		return nil
	}
	var roster struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(b.Roster, &roster); err == nil && roster.AgentID != "" {
		a.agents = append(a.agents, roster.AgentID)
	}
	return a.st.Apply(wal.Committed{
		PrepareIndex: c.PrepareIndex,
		CommitIndex:  c.CommitIndex,
		Entry:        wal.Entry{Kind: invite.RecordKind, Body: b.Invite},
	})
}

// openEnrolStore is openStore wired the way a server composing enrolments wires
// it: the same late-bound *wal.Log, but with the decomposing applier in front of
// the store. The caller owns Close.
func openEnrolStore(t *testing.T, dir string, clk *testClock) (*invite.Store, *wal.Log, *enrolApplier) {
	t.Helper()
	d := &lateLog{}
	o := invite.StoreOptions{BusID: testBusID, Durable: d}
	if clk != nil {
		o.Now = clk.Now
	}
	st, err := invite.NewStore(o)
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	ap := &enrolApplier{st: st}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ap})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	d.l = lg
	return st, lg, ap
}

// TestInviteParticipantTwoPhaseRedemption drives the REAL participant sequence
// against a REAL *wal.Log, start to finish, and pins every refusal that guards
// it.
//
// The sequence, and why each step is where it is:
//
//	r, err := store.Begin(req)   // validates + RESERVES; no other transition may start
//	body, err := r.Consume(res)  // builds the consumption record; writes NOTHING
//	_, err = log.Write(entry)    // the CALLER's one transaction: roster + invite
//	err = r.Commit()             // folds into memory, AFTER the write committed
//
// Nothing about the invite is durable until the caller's entry commits, and
// nothing is in memory until Commit. A crash before the write leaves the invite
// open (nothing was acknowledged); a crash after it leaves the invite spent and
// the agent enrolled. There is no window in between because the two records are
// one transaction — which is the whole reason this API exists and Store.Redeem
// cannot be used in its place.
func TestInviteParticipantTwoPhaseRedemption(t *testing.T) {
	dir := t.TempDir()
	clk := newTestClock()
	st, lg, ap := openEnrolStore(t, dir, clk)
	defer func() { _ = lg.Close() }()

	m := mustMint(t, st, invite.MintRequest{Label: "for the participant path"})
	clk.Advance(time.Minute)
	req := redeemReq(m, testKey, "payload")

	// --- Begin: the reservation ---------------------------------------------
	r, err := st.Begin(req)
	if err != nil {
		t.Fatalf("Begin on a fresh, open invite: %v", err)
	}
	if r.Outcome() != invite.OutcomeReserved {
		t.Fatalf("Begin on a fresh, open invite gave outcome %s, want %s", r.Outcome(), invite.OutcomeReserved)
	}
	if got := r.Result(); got != nil {
		t.Errorf("a RESERVED redemption has no earlier result to replay, got %s", got)
	}

	// While the reservation is held NO other lifecycle transition may start —
	// including a retry presenting the SAME key, which cannot be answered until
	// the original resolves.
	if _, err := st.Begin(redeemReq(m, testOtherKey, "another payload")); !errors.Is(err, invite.ErrRedemptionInFlight) {
		t.Fatalf("a second Begin under a DIFFERENT key while a reservation is held gave err = %v, want ErrRedemptionInFlight; two reservations on one invite is a double redemption", err)
	}
	if _, err := st.Begin(req); !errors.Is(err, invite.ErrRedemptionInFlight) {
		t.Fatalf("a second Begin under the SAME key while a reservation is held gave err = %v, want ErrRedemptionInFlight; answering a retry that races its own original means inventing a result or starting a second redemption", err)
	}
	if _, err := st.Revoke(m.ID, "racing the redemption"); !errors.Is(err, invite.ErrRedemptionInFlight) {
		t.Fatalf("Revoke while a redemption is reserved gave err = %v, want ErrRedemptionInFlight", err)
	}

	// Commit BEFORE Consume is MISUSE: there is no durable record to fold in.
	if err := r.Commit(); err == nil {
		t.Fatalf("Commit without Consume succeeded; it must refuse, because folding a spend into memory that no record backs is the in-memory half of a double redemption")
	}
	// ... and that refusal did NOT resolve the reservation.
	if _, err := st.Begin(redeemReq(m, testOtherKey, "another payload")); !errors.Is(err, invite.ErrRedemptionInFlight) {
		t.Fatalf("a refused Commit released the reservation: err = %v, want ErrRedemptionInFlight", err)
	}

	// --- Consume: the record is BUILT, and nothing at all is written ---------
	body, err := r.Consume(standardResult())
	if err != nil {
		t.Fatalf("Consume on a reserved redemption: %v", err)
	}
	if got := mustLookup(t, st, m.ID, "after Consume, before the caller's write").State; got != invite.StateOpen {
		t.Fatalf("after Consume the in-memory invite is %s, want open: nothing may be in memory before the CALLER's write commits", got)
	}
	if _, err := r.Consume(standardResult()); err == nil {
		t.Fatalf("a second Consume succeeded; a second consumption record is a second redemption")
	}

	// --- the CALLER's entry: roster record + consumption record, ONE txn -----
	if _, err := lg.Write(enrolEntry(t, testAgentID, body)); err != nil {
		t.Fatalf("writing the composed enrolment entry: %v", err)
	}

	// --- Commit: fold into memory -------------------------------------------
	if err := r.Commit(); err != nil {
		t.Fatalf("Commit after the caller's write committed: %v", err)
	}
	if err := r.Commit(); err != nil {
		t.Fatalf("Commit is idempotent, second call gave err = %v", err)
	}
	// ABORT AFTER A SUCCESSFUL COMMIT IS A NO-OP, never an un-spend.
	r.Abort()

	rec := mustLookup(t, st, m.ID, "after the participant commit")
	if rec.State != invite.StateRedeemed {
		t.Fatalf("after the participant sequence the invite is %s, want redeemed (Abort after Commit un-spent it?)", rec.State)
	}
	if rec.RedeemedBy != testAgentID {
		t.Errorf("RedeemedBy = %q, want %q", rec.RedeemedBy, testAgentID)
	}
	if rec.RedeemKey != testKey {
		t.Errorf("RedeemKey = %q, want %q", rec.RedeemKey, testKey)
	}
	if !bytes.Equal(rec.Result, testResult) {
		t.Errorf("Result = %s, want %s", rec.Result, testResult)
	}

	// SINGLE USE: the invite is spent to everybody else.
	if _, err := st.Begin(redeemReq(m, testOtherKey, "another payload")); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("after the participant sequence a second redemption gave err = %v, want ErrAlreadyRedeemed", err)
	}

	// ... and the legitimate retry still gets the ORIGINAL result verbatim.
	replay, err := st.Begin(req)
	if err != nil {
		t.Fatalf("the same-key, same-fingerprint retry gave err = %v, want a replay", err)
	}
	if replay.Outcome() != invite.OutcomeReplay {
		t.Fatalf("the retry gave outcome %s, want %s", replay.Outcome(), invite.OutcomeReplay)
	}
	if !bytes.Equal(replay.Result(), testResult) {
		t.Fatalf("the replayed result is %s, want %s verbatim", replay.Result(), testResult)
	}
	// A REPLAY RESERVED NOTHING and may not be consumed: applying anything here
	// would be a second redemption dressed as a retry.
	if _, err := replay.Consume(standardResult()); err == nil {
		t.Fatalf("Consume on an OutcomeReplay redemption succeeded; a replay must return Result() and apply nothing")
	}
	// Commit and Abort on a replay are no-ops and must not disturb the record.
	if err := replay.Commit(); err != nil {
		t.Errorf("Commit on a replay gave err = %v, want nil", err)
	}
	replay.Abort()
	if got := mustLookup(t, st, m.ID, "after a replay was committed and aborted").State; got != invite.StateRedeemed {
		t.Fatalf("a replay's Commit/Abort moved the invite to %s, want redeemed", got)
	}

	// The roster half rode the SAME transaction — that is what the composite
	// entry buys, and the assertion that this test is not quietly re-testing the
	// standalone path.
	if len(ap.agents) != 1 || ap.agents[0] != testAgentID {
		t.Fatalf("the applier saw roster halves %v, want exactly [%s]: the enrolment and the consumption record must commit together", ap.agents, testAgentID)
	}

	// --- and a FRESH store rebuilt from that same log AGREES -----------------
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}
	st2, lg2, _ := openEnrolStore(t, dir, clk)
	defer func() { _ = lg2.Close() }()

	assertRecordEqual(t, "the record recovered from the durable log", mustLookup(t, st2, m.ID, "after recovery"), rec)
	if _, err := st2.Begin(redeemReq(m, testOtherKey, "another payload")); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("after a restart the invite answers %v, want ErrAlreadyRedeemed: the participant path's spend did not survive recovery, so one invite admits two agents across a restart", err)
	}
	retry, err := st2.Begin(req)
	if err != nil {
		t.Fatalf("the retry after recovery gave err = %v, want a replay", err)
	}
	if retry.Outcome() != invite.OutcomeReplay || !bytes.Equal(retry.Result(), testResult) {
		t.Fatalf("after recovery the retry gave outcome %s / result %s, want replay of %s", retry.Outcome(), retry.Result(), testResult)
	}
}

// TestInviteParticipantAbortLeavesTheInviteRedeemable pins the other half of the
// participant contract: an Abort — the caller's write failed, or never happened
// — leaves the invite REDEEMABLE, and leaves NOTHING on disk.
//
// Both directions matter. If Abort failed to release, one failed enrolment would
// strand an operator's invite until a restart. If Abort left something durable,
// the invite would be spent on an enrolment that never happened.
func TestInviteParticipantAbortLeavesTheInviteRedeemable(t *testing.T) {
	cases := []struct {
		name string
		// consume says whether the consumption record was built before the abort.
		// It is the interesting case: a record exists in memory, but the caller
		// demonstrably never wrote it, so releasing is correct.
		consume bool
	}{
		{"abort BEFORE Consume", false},
		{"abort AFTER Consume, the caller's write demonstrably not committed", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clk := newTestClock()
			st, lg, _ := openEnrolStore(t, dir, clk)

			m := mustMint(t, st, invite.MintRequest{})
			clk.Advance(time.Minute)

			r, err := st.Begin(redeemReq(m, testKey, "payload"))
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if tc.consume {
				if _, err := r.Consume(standardResult()); err != nil {
					t.Fatalf("Consume: %v", err)
				}
			}
			r.Abort()
			// Abort is idempotent, and a second one must not release a reservation
			// somebody else took in the meantime.
			r.Abort()

			if got := mustLookup(t, st, m.ID, "after Abort").State; got != invite.StateOpen {
				t.Fatalf("after Abort the invite is %s, want open", got)
			}
			// The invite is immediately reservable again by a DIFFERENT caller.
			second, err := st.Begin(redeemReq(m, testOtherKey, "second attempt"))
			if err != nil {
				t.Fatalf("Begin after Abort gave err = %v; an aborted redemption stranded the invite", err)
			}
			if second.Outcome() != invite.OutcomeReserved {
				t.Fatalf("Begin after Abort gave outcome %s, want %s", second.Outcome(), invite.OutcomeReserved)
			}
			second.Abort()

			// NOTHING reached the durable log: a fresh process replaying it sees an
			// invite that was never spent.
			if err := lg.Close(); err != nil {
				t.Fatalf("closing the log: %v", err)
			}
			st2, lg2, _ := openEnrolStore(t, dir, clk)
			defer func() { _ = lg2.Close() }()
			if got := mustLookup(t, st2, m.ID, "after recovery").State; got != invite.StateOpen {
				t.Fatalf("after a restart an ABORTED redemption left the invite %s, want open: the invite was spent on an enrolment that never happened", got)
			}

			// And it can still be spent for real, through the whole sequence.
			r2, err := st2.Begin(redeemReq(m, testKey, "payload"))
			if err != nil {
				t.Fatalf("Begin after recovery: %v", err)
			}
			body, err := r2.Consume(standardResult())
			if err != nil {
				t.Fatalf("Consume after recovery: %v", err)
			}
			if _, err := lg2.Write(enrolEntry(t, testAgentID, body)); err != nil {
				t.Fatalf("writing the composed enrolment entry: %v", err)
			}
			if err := r2.Commit(); err != nil {
				t.Fatalf("Commit after recovery: %v", err)
			}
			if got := mustLookup(t, st2, m.ID, "after the real redemption").State; got != invite.StateRedeemed {
				t.Fatalf("after the participant sequence the invite is %s, want redeemed", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ReservationTTL — and the half of it that is NEGATIVE
// ---------------------------------------------------------------------------

// TestInviteReservationTTL pins BOTH directions of the reservation sweep, which
// is the rule doc.go names as easy to get wrong.
//
//   - An UNCONSUMED reservation held past ReservationTTL IS reaped. Without
//     that, one caller that died between Begin and Consume locks an operator's
//     invite until the next restart.
//   - A CONSUMED reservation is NEVER reaped, however long it is held. This is
//     the load-bearing half: after Consume the caller may already have committed
//     the consumption record, so reaping the hold back to open would admit a
//     SECOND redemption while the durable log says the invite is spent — a
//     double redemption inside one process lifetime, with no crash involved.
//
// The clock is injected (StoreOptions.Now) and the TTL is shortened
// (StoreOptions.ReservationTTL), so the real derived windows are exercised
// without a test that sleeps.
func TestInviteReservationTTL(t *testing.T) {
	const ttl = time.Second

	t.Run("an UNCONSUMED reservation held past the TTL is reaped", func(t *testing.T) {
		st, fl, clk := newFakeStore(t, func(o *invite.StoreOptions) { o.ReservationTTL = ttl })
		m := mustMint(t, st, invite.MintRequest{})
		clk.Advance(time.Minute)
		mintedEntries := fl.len()

		abandoned, err := st.Begin(redeemReq(m, testKey, "payload"))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}

		// EXACTLY at the TTL the hold survives: the sweep reaps strictly past it,
		// and a caller that took the whole budget must not be robbed at the buzzer.
		clk.Advance(ttl)
		if _, err := st.Begin(redeemReq(m, testOtherKey, "second")); !errors.Is(err, invite.ErrRedemptionInFlight) {
			t.Fatalf("at exactly the reservation TTL the hold gave err = %v, want ErrRedemptionInFlight", err)
		}

		// One tick past it, the sweep hands the invite to somebody else.
		clk.Advance(time.Nanosecond)
		fresh, err := st.Begin(redeemReq(m, testOtherKey, "second"))
		if err != nil {
			t.Fatalf("past the reservation TTL an ABANDONED, unconsumed hold was not reaped: err = %v; one caller that died between Begin and Consume would lock the invite until restart", err)
		}
		if fresh.Outcome() != invite.OutcomeReserved {
			t.Fatalf("after the reap, Begin gave outcome %s, want %s", fresh.Outcome(), invite.OutcomeReserved)
		}

		// The abandoned holder can no longer build a durable record. Silently
		// succeeding here is the failure that matters: it would produce a
		// consumption record for a reservation somebody else now holds.
		if _, err := abandoned.Consume(standardResult()); err == nil {
			t.Fatalf("Consume on a REAPED reservation succeeded; the reservation is gone and the holder no longer holds the invite")
		} else if !strings.Contains(err.Error(), "reaped") {
			t.Errorf("the refusal does not tell the operator the reservation was reaped: %v", err)
		}

		// Its late Commit folds nothing in — there is no record to fold — and its
		// late Commit/Abort must not release the NEW reservation. (Two mechanisms
		// enforce that: the resolved flag the reap set, and the identity check in
		// releaseLocked. The property is pinned here; either alone is enough.)
		if err := abandoned.Commit(); err != nil {
			t.Errorf("Commit on a reaped reservation gave err = %v, want a silent no-op", err)
		}
		abandoned.Abort()
		if got := mustLookup(t, st, m.ID, "after a reaped holder committed and aborted").State; got != invite.StateOpen {
			t.Fatalf("a reaped holder's late Commit spent the invite (%s); it never had a durable record", got)
		}
		if n := fl.len() - mintedEntries; n != 0 {
			t.Fatalf("the reaped holder made %d durable writes, want 0", n)
		}
		if _, err := st.Begin(redeemReq(m, "k-third", "third")); !errors.Is(err, invite.ErrRedemptionInFlight) {
			t.Fatalf("a reaped holder's late Commit/Abort released the NEW reservation: err = %v, want ErrRedemptionInFlight", err)
		}

		// The new holder still resolves normally: the reap cost availability for
		// one caller, not correctness for the next.
		body, err := fresh.Consume(standardResult())
		if err != nil {
			t.Fatalf("Consume on the reservation taken after the reap: %v", err)
		}
		if _, err := fl.Write(wal.Entry{Kind: invite.RecordKind, Body: body}); err != nil {
			t.Fatalf("the caller's durable write: %v", err)
		}
		if err := fresh.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if got := mustLookup(t, st, m.ID, "after the post-reap redemption").State; got != invite.StateRedeemed {
			t.Fatalf("after the post-reap redemption the invite is %s, want redeemed", got)
		}
	})

	t.Run("a CONSUMED reservation is NEVER reaped", func(t *testing.T) {
		st, fl, clk := newFakeStore(t, func(o *invite.StoreOptions) { o.ReservationTTL = ttl })
		m := mustMint(t, st, invite.MintRequest{})
		clk.Advance(time.Minute)

		r, err := st.Begin(redeemReq(m, testKey, "payload"))
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		body, err := r.Consume(standardResult())
		if err != nil {
			t.Fatalf("Consume: %v", err)
		}

		// Hold it for four hours — 14400 times the reservation TTL — sweeping the
		// whole way. Every exported entry point sweeps, so each of these calls is
		// another chance for the sweep to get it wrong.
		for i := 1; i <= 4; i++ {
			clk.Advance(time.Hour)
			if _, ok := st.Lookup(m.ID); !ok {
				t.Fatalf("after %dh the invite RECORD was dropped while a consumption was in flight; the holder is working from a snapshot of exactly that record", i)
			}
			if _, err := st.Mint(invite.MintRequest{}); err != nil {
				t.Fatalf("after %dh, Mint (which sweeps): %v", i, err)
			}
			if _, err := st.Begin(redeemReq(m, testOtherKey, "second")); !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("after %dh — %d times the reservation TTL — a CONSUMED hold was released: err = %v, want ErrRedemptionInFlight. The consumption record may already be durable, so this is a SECOND redemption of an invite the log says is spent", i, int(time.Duration(i)*time.Hour/ttl), err)
			}
			if _, err := st.Revoke(m.ID, "operator tries to withdraw it"); !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("after %dh a CONSUMED hold did not block a revocation: err = %v, want ErrRedemptionInFlight", i, err)
			}
		}

		// Nothing was folded into memory the whole time — only the CALLER's write
		// and Commit may do that.
		if got := mustLookup(t, st, m.ID, "while a consumption is in flight").State; got != invite.StateOpen {
			t.Fatalf("while the consumption was in flight the in-memory invite became %s, want open", got)
		}

		// And the original holder still resolves it, four hours later.
		if _, err := fl.Write(wal.Entry{Kind: invite.RecordKind, Body: body}); err != nil {
			t.Fatalf("the caller's durable write: %v", err)
		}
		if err := r.Commit(); err != nil {
			t.Fatalf("Commit on a long-held CONSUMED reservation: %v", err)
		}
		if got := mustLookup(t, st, m.ID, "after the long-held commit").State; got != invite.StateRedeemed {
			t.Fatalf("after Commit the invite is %s, want redeemed", got)
		}
	})
}

// ---------------------------------------------------------------------------
// wal.ErrDiverged — the one write error that must NOT release the hold
// ---------------------------------------------------------------------------

// TestInviteDivergedDurableWriteKeepsTheHold pins the asymmetry that both
// Store.Redeem and Store.Revoke implement, and that nothing else in this suite
// would notice the loss of.
//
// wal.Txn.Commit returns wal.ErrDiverged AFTER the commit record has been
// appended and FSYNCED (internal/wal/log.go): the failure belongs to a
// neighbouring applier, not to the write, and the record is by then on stable
// storage. So:
//
//   - ErrDiverged  -> the hold is KEPT. Releasing it would leave memory saying
//     OPEN while disk says REDEEMED/REVOKED, and the next Begin would admit a
//     second redemption of an invite the log already spent. Keeping it is
//     fail-closed: the invite is frozen until a restart rebuilds from the log.
//   - any other error -> the hold is RELEASED. Nothing committed, so stranding a
//     live invite until restart would be a self-inflicted outage.
//
// The hold that is kept must also survive the reservation TTL, because both
// paths mark the transition consumed BEFORE the durable write for exactly that
// reason.
func TestInviteDivergedDurableWriteKeepsTheHold(t *testing.T) {
	const ttl = time.Second
	diverged := fmt.Errorf("wal: applying committed entry 7: %w", wal.ErrDiverged)
	onFire := errors.New("the disk is on fire")

	cases := []struct {
		name     string
		writeErr error
		// wantHeld says whether the invite must still be frozen afterwards.
		wantHeld bool
	}{
		{"a DIVERGED write keeps the hold", diverged, true},
		{"any OTHER write error releases it", onFire, false},
	}

	for _, tc := range cases {
		t.Run("Redeem/"+tc.name, func(t *testing.T) {
			st, fl, clk := newFakeStore(t, func(o *invite.StoreOptions) { o.ReservationTTL = ttl })
			m := mustMint(t, st, invite.MintRequest{})
			clk.Advance(time.Minute)

			fl.failWith(tc.writeErr)
			if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); !errors.Is(err, tc.writeErr) {
				t.Fatalf("Redeem over a failing durable log gave err = %v, want one wrapping %v", err, tc.writeErr)
			}
			// Either way memory never recorded the spend: only the durable log may
			// say that, and this write did not tell us it did.
			if got := mustLookup(t, st, m.ID, "after the failed durable write").State; got != invite.StateOpen {
				t.Fatalf("after a FAILED durable write the in-memory invite is %s, want open", got)
			}

			// The log recovers. On the diverged path the HOLD must not.
			fl.failWith(nil)
			// Well past the reservation TTL: the transition was marked consumed
			// before the write, so the sweep may not reap it either.
			clk.Advance(100 * ttl)

			_, err := st.Begin(redeemReq(m, testOtherKey, "second"))
			if !tc.wantHeld {
				if err != nil {
					t.Fatalf("after an ordinary failed write the invite must be usable once the disk recovers, got err = %v", err)
				}
				if _, err := st.Redeem(redeemReq(m, testOtherKey, "second"), standardResult()); !errors.Is(err, invite.ErrRedemptionInFlight) {
					// The Begin above still holds the reservation; that it is HELD,
					// not stranded, is the point.
					t.Fatalf("expected the reservation just taken to be held, got err = %v", err)
				}
				return
			}
			if !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("after a write that returned wal.ErrDiverged AFTER its commit record was fsynced, the invite is available again: err = %v, want ErrRedemptionInFlight. The consumption record is durably on disk, so this is a SECOND redemption of a spent invite", err)
			}
			if _, err := st.Redeem(redeemReq(m, testKey, "payload"), standardResult()); !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("a retry of the diverged redemption gave err = %v, want ErrRedemptionInFlight", err)
			}
			if _, err := st.Revoke(m.ID, "withdrawn"); !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("Revoke of an invite frozen by a diverged redemption gave err = %v, want ErrRedemptionInFlight", err)
			}
		})

		t.Run("Revoke/"+tc.name, func(t *testing.T) {
			st, fl, clk := newFakeStore(t, func(o *invite.StoreOptions) { o.ReservationTTL = ttl })
			m := mustMint(t, st, invite.MintRequest{})
			clk.Advance(time.Minute)

			fl.failWith(tc.writeErr)
			if _, err := st.Revoke(m.ID, "withdrawn"); !errors.Is(err, tc.writeErr) {
				t.Fatalf("Revoke over a failing durable log gave err = %v, want one wrapping %v", err, tc.writeErr)
			}
			if got := mustLookup(t, st, m.ID, "after the failed revocation").State; got != invite.StateOpen {
				t.Fatalf("after a FAILED durable write the in-memory invite is %s, want open", got)
			}

			fl.failWith(nil)
			clk.Advance(100 * ttl)

			_, err := st.Revoke(m.ID, "second attempt")
			if !tc.wantHeld {
				if err != nil {
					t.Fatalf("after an ordinary failed revocation the invite must be revocable once the disk recovers, got err = %v", err)
				}
				if got := mustLookup(t, st, m.ID, "after the retried revocation").State; got != invite.StateRevoked {
					t.Fatalf("after the retried revocation the invite is %s, want revoked", got)
				}
				return
			}
			if !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("after a revocation write that returned wal.ErrDiverged AFTER its commit record was fsynced, the hold was released: err = %v, want ErrRedemptionInFlight. Memory would say OPEN while disk says REVOKED, and a revoked invite memory still thinks is open is REDEEMABLE — the one outcome revocation exists to prevent", err)
			}
			// FROZEN, not merely un-revoked: nobody may redeem it either.
			if _, err := st.Begin(redeemReq(m, testKey, "payload")); !errors.Is(err, invite.ErrRedemptionInFlight) {
				t.Fatalf("an invite frozen by a diverged revocation is redeemable: err = %v, want ErrRedemptionInFlight", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Redaction: the plaintext secret and Go's formatting verbs
// ---------------------------------------------------------------------------

// TestInviteSecretIsRedactedByEveryFormatVerb pins Minted.String/GoString and
// RedeemRequest.String/GoString.
//
// The hazard they exist for is not a deliberate log line — it is "%+v" in a
// panic, a debug print, or a structured log field added months from now by
// someone who did not read the field comment. Go reaches for String and GoString
// automatically in all of those, so redaction there is the only measure that
// holds without everyone remembering. A test that only checked one verb would
// leave the other three as the leak.
//
// KNOWN GAP, deliberately NOT asserted here: encoding/json still marshals the
// plaintext Secret field of Minted (and of RedeemRequest). MarshalJSON is not a
// formatting verb and json.Marshal never consults String, so these methods
// cannot close it. It is reported rather than pinned, because pinning the
// current behaviour would turn the eventual fix into a test failure.
func TestInviteSecretIsRedactedByEveryFormatVerb(t *testing.T) {
	st, _, _ := newFakeStore(t, nil)
	m := mustMint(t, st, invite.MintRequest{Label: "redaction"})
	req := redeemReq(m, testKey, "payload")

	// The control: without a secret long enough to look for, every assertion
	// below would pass for the wrong reason.
	const prefixLen = 16
	if len(m.Secret) < prefixLen {
		t.Fatalf("the minted secret is %d characters, too short for this test to mean anything", len(m.Secret))
	}
	if req.Secret != m.Secret {
		t.Fatalf("the request under test does not carry the secret, so nothing here is being checked")
	}

	values := []struct {
		name string
		v    interface{}
	}{
		{"invite.Minted", m},
		{"*invite.Minted", &m},
		{"invite.RedeemRequest", req},
		{"*invite.RedeemRequest", &req},
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, val := range values {
			t.Run(val.name+" "+verb, func(t *testing.T) {
				out := fmt.Sprintf(verb, val.v)
				if strings.Contains(out, m.Secret) {
					t.Fatalf("fmt.Sprintf(%q, %s) printed the PLAINTEXT SECRET; a live bearer credential reaches any log, panic or debug print that formats this value", verb, val.name)
				}
				// A prefix is enough to make the rest brute-forceable, and is the
				// shape a "just the first few characters for debugging" line takes.
				if strings.Contains(out, m.Secret[:prefixLen]) {
					t.Fatalf("fmt.Sprintf(%q, %s) printed a %d-character PREFIX of the secret", verb, val.name, prefixLen)
				}
				if !strings.Contains(out, "REDACTED") {
					t.Fatalf("fmt.Sprintf(%q, %s) does not say the secret was redacted, so the redaction is not the reason the secret is absent:\n%s", verb, val.name, out)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// A redeemed record with no payload fingerprint is DISCARDED
// ---------------------------------------------------------------------------

// TestInviteApplyDiscardsARedeemedRecordWithoutItsFingerprint pins record.go's
// refusal at the level that matters — Store.Apply, the replay path — rather than
// only at DecodeRecord.
//
// The zero fingerprint is EXPLOITABLE, not untidy. Store.Begin's triage treats a
// MATCHING fingerprint as a legitimate retry and hands back the ORIGINAL RESULT,
// which for enrolment is an agent identity and its token. A stored all-zero
// fingerprint therefore matches a request that carries no fingerprint at all, so
// anybody who learned the invite id, the secret and the idempotency key from a
// record whose "redeem_fp" was dropped or zeroed on disk would be replayed the
// original enrolment instead of being refused with ErrKeyReuse.
//
// Encode always writes the full 64 hex characters, so no record this package
// produces can reach here: the input this test uses is exactly the untrusted,
// damaged-log shape validate exists to catch.
func TestInviteApplyDiscardsARedeemedRecordWithoutItsFingerprint(t *testing.T) {
	const id = "inv-ffffffffffffffff"
	const secret = "the fingerprint fixture secret"
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	open := invite.Record{
		ID:           id,
		BusID:        testBusID,
		SecretDigest: invite.HashSecret(secret),
		CreatedAt:    base,
		ExpiresAt:    base.Add(24 * time.Hour),
		State:        invite.StateOpen,
	}

	// redeemedBody is a well-formed redemption record in every respect EXCEPT its
	// fingerprint. It is hand-built rather than encoded, because Encode would
	// refuse to produce it — which is the point.
	redeemedBody := func(t *testing.T, fp interface{}) json.RawMessage {
		t.Helper()
		m := map[string]interface{}{
			"id":            id,
			"bus":           testBusID,
			"secret_sha256": hexDigest(secret),
			"created_at":    "2026-08-07T12:00:00Z",
			"expires_at":    "2026-08-08T12:00:00Z",
			"state":         "redeemed",
			"redeemed_at":   "2026-08-07T12:05:00Z",
			"redeemed_by":   testAgentID,
			"redeem_key":    testKey,
			"result":        testResult,
		}
		if fp != nil {
			m["redeem_fp"] = fp
		}
		return json.RawMessage(mustJSON(t, m))
	}

	cases := []struct {
		name string
		// fp is the "redeem_fp" value; nil omits the key entirely.
		fp interface{}
		// seedOpen seeds the open record first, so the case shows the record is
		// refused rather than merely undecodable in isolation.
		seedOpen bool
	}{
		{"redeem_fp absent, against a known open invite", nil, true},
		{"redeem_fp all zero, against a known open invite", strings.Repeat("0", 2*idem.FingerprintSize), true},
		{"redeem_fp absent, with no earlier record", nil, false},
		{"redeem_fp all zero, with no earlier record", strings.Repeat("0", 2*idem.FingerprintSize), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			st, _, _ := newFakeStore(t, func(o *invite.StoreOptions) {
				o.Logger = logging.New(&logBuf, logging.LevelDebug)
			})
			if tc.seedOpen {
				if err := st.Apply(committedRecord(t, open, 1, 2)); err != nil {
					t.Fatalf("seeding the open record: %v", err)
				}
			}
			logBuf.Reset()

			c := wal.Committed{
				PrepareIndex: 9, CommitIndex: 10,
				Entry: wal.Entry{Kind: invite.RecordKind, Body: redeemedBody(t, tc.fp)},
			}
			if err := st.Apply(c); err != nil {
				t.Fatalf("Apply returned err = %v, want nil ALWAYS", err)
			}

			// THE EFFECT FIRST, the log line second: if the refusal is ever
			// removed, the failure an operator should read is "the invite was
			// spent", not "nothing was logged".
			rec, found := st.Lookup(id)
			if tc.seedOpen {
				if !found {
					t.Fatalf("the seeded open record disappeared; a refused record must leave the state already in memory")
				}
				if rec.State != invite.StateOpen {
					t.Fatalf("a redeemed record with no payload fingerprint SPENT the invite (state %s); Store.Begin would then replay the original enrolment result to a request carrying no fingerprint at all", rec.State)
				}
				// The exploit shape, refused at the source: a request carrying the
				// ZERO fingerprint is not handed a stored result, because there is
				// no stored result to hand it.
				r, err := st.Begin(invite.RedeemRequest{InviteID: id, Secret: secret, Key: testKey})
				if err != nil {
					t.Fatalf("Begin against the surviving open record: %v", err)
				}
				if r.Outcome() != invite.OutcomeReserved {
					t.Fatalf("a request carrying an ALL-ZERO fingerprint got outcome %s, want %s: it was answered as a legitimate retry", r.Outcome(), invite.OutcomeReserved)
				}
				if got := r.Result(); got != nil {
					t.Fatalf("a request carrying an ALL-ZERO fingerprint was replayed a stored result: %s", got)
				}
				r.Abort()
			} else {
				if found {
					t.Fatalf("a redeemed record with no payload fingerprint was APPLIED: %+v", rec)
				}
				// Fail-closed: the invite is UNKNOWN and therefore unredeemable.
				if _, err := st.Begin(invite.RedeemRequest{InviteID: id, Secret: secret, Key: testKey, Fingerprint: fingerprintOf("payload")}); !errors.Is(err, invite.ErrUnknownInvite) {
					t.Fatalf("an invite discarded at replay answers %v, want ErrUnknownInvite", err)
				}
			}

			// Silent discard is the defect, not discard itself.
			if !strings.Contains(logBuf.String(), "DISCARDING") || !strings.Contains(logBuf.String(), "level=error") {
				t.Fatalf("the discard was SILENT; an operator must see why an invite went missing:\n%s", logBuf.String())
			}
		})
	}
}
