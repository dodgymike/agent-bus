// INVITE-STORE's ACCEPTANCE EVIDENCE for single use: a real kill -9 proving
// that a spent invite is WAL-recovered state and not an in-memory cache.
//
// "The code is written" is not evidence for a durability claim, and neither is a
// graceful restart. TestInviteStoreRecovery closes the log politely first, so
// every deferred Close, buffer flush and runtime shutdown gets to run. A SIGKILL
// is the only thing that proves none of them was load-bearing: the parent opens
// the exact bytes the dying process had put on stable storage, with nobody
// having tidied the tail on the way out.
//
// The crash point is the one that matters for an invite: AFTER the consumption
// record's commit fsync and BEFORE anything acknowledges it. Redemption.Commit
// never runs, no caller is ever told the enrolment succeeded, and the client
// sees a dropped connection rather than a result — which is exactly the moment a
// well-behaved client RETRIES. What must hold on the other side:
//
//	(a) the consumption record is on stable storage;
//	(b) a FRESH store rebuilt from that log refuses a SECOND redemption
//	    presenting the CORRECT secret — single use survived the kill;
//	(c) the same-key + same-fingerprint retry replays the ORIGINAL result
//	    across the crash boundary (invariant 10's legitimate-retry carve-out).
//
// If (b) fails, one invite admits two agents across a crash and the enrolment
// gate is decorative. If (c) fails, the client doing the right thing is punished
// for a crash it did not cause.
//
// The pattern is internal/hub/idem_crash_test.go's, deliberately: an env-selected
// crash point, a killAfterCommit wrapper around the REAL *wal.Log, a parent that
// asserts the child DIED ON SIGKILL via syscall.WaitStatus (a child that merely
// failed its own assertions also exits non-zero, so the wait status is the
// assertion), and wal.Replay to read what the dying process actually got onto
// stable storage.
package invite_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// envInviteCrashPoint selects where the child kills itself. Unset means "not
	// a crash child", which is what makes TestInviteCrashChild a no-op skip in a
	// normal run of the suite.
	envInviteCrashPoint = "INVITE_CRASH_POINT"
	// envInviteCrashDir is the data directory the child writes into: a
	// t.TempDir() belonging to the parent, so no test shares a data directory
	// with another and the tracked data/ dir is never touched.
	envInviteCrashDir = "INVITE_CRASH_DIR"
	// envInviteCrashID and envInviteCrashSecret carry the fixture the PARENT
	// minted. The parent mints so that it knows the secret: the secret exists
	// exactly once, in Mint's return value, and is never stored — so a child that
	// minted it would take the only copy to the grave, and assertion (b) (a
	// second redemption presenting the CORRECT secret) could not be written.
	envInviteCrashID     = "INVITE_CRASH_ID"
	envInviteCrashSecret = "INVITE_CRASH_SECRET"

	// inviteCrashRedeemPostCommit: the child's redemption is committed and
	// fsynced, and the process dies before Redemption.Commit can fold it into
	// memory, before Redeem returns, and before any caller is acknowledged.
	inviteCrashRedeemPostCommit = "redeem-post-commit-pre-ack"
)

// The fixture the child and the parent must agree on byte for byte: the retry
// the parent sends has to be the SAME key under the SAME fingerprint, or the
// parent would be testing the key-reuse violation path by accident.
const (
	inviteCrashKey     = "k-invite-crash-post-commit"
	inviteCrashAgentID = testBusID + ".crashling"

	inviteCrashPayload      = "the enrolment acknowledgement this client never received"
	inviteCrashOtherPayload = "a DIFFERENT enrolment payload reusing the same key"
)

var inviteCrashResult = json.RawMessage(`{"agent_id":"bus-test.crashling","token":"tok-crash"}`)

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestInviteCrashChild is the child half of the crash test. It does NOTHING in a
// normal run: without envInviteCrashPoint it skips immediately.
func TestInviteCrashChild(t *testing.T) {
	point := os.Getenv(envInviteCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envInviteCrashPoint + " is unset")
	}
	dir := os.Getenv(envInviteCrashDir)
	id := os.Getenv(envInviteCrashID)
	secret := os.Getenv(envInviteCrashSecret)
	if dir == "" || id == "" || secret == "" {
		t.Fatalf("child: %s=%q but the fixture is incomplete (dir=%q id=%q secret set=%v)", envInviteCrashPoint, point, dir, id, secret != "")
	}

	// NO deferred Close and NO t.Cleanup on the log: a Close that ran would be
	// exactly the graceful shutdown this test exists to rule out.
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("child: wal.Open(%s): %v", dir, err)
	}

	// The store writes through the killer and is rebuilt by a read-only replay of
	// the log's own path — the wiring internal/hub uses, and the one that keeps
	// the kill point unambiguous: nothing at all runs between the commit fsync
	// and the SIGKILL.
	st, err := invite.NewStore(invite.StoreOptions{BusID: testBusID, Durable: &killAfterCommit{l: lg}})
	if err != nil {
		t.Fatalf("child: invite.NewStore: %v", err)
	}
	if _, err := wal.Replay(lg.Path(), st.Apply); err != nil {
		t.Fatalf("child: replaying %s: %v", lg.Path(), err)
	}
	rec, ok := st.Lookup(id)
	if !ok {
		t.Fatalf("child: invite %s was not recovered from the log the parent wrote; there is nothing to redeem", id)
	}
	if rec.State != invite.StateOpen {
		t.Fatalf("child: invite %s recovered as %s, want open", id, rec.State)
	}

	switch point {
	case inviteCrashRedeemPostCommit:
		got, err := st.Redeem(invite.RedeemRequest{
			InviteID:    id,
			Secret:      secret,
			Key:         inviteCrashKey,
			Fingerprint: fingerprintOf(inviteCrashPayload),
		}, invite.Result{AgentID: inviteCrashAgentID, Response: inviteCrashResult})
		t.Fatalf("child: Redeem returned (%+v, %v) but the durable log kills this process the instant the commit is fsynced; the crash was never injected", got, err)

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
}

// killAfterCommit is the honest post-commit, pre-ack kill.
//
// It delegates to the REAL *wal.Log.Write — the whole prepare, commit and fsync
// cycle — and kills the process before returning, so Store.Redeem never reaches
// Redemption.Commit, never folds the spend into memory and never returns a
// record. The consumption record is on stable storage and nothing in this
// process, and no client, knows it.
type killAfterCommit struct{ l *wal.Log }

func (k *killAfterCommit) Write(e wal.Entry) (wal.Committed, error) {
	// Asserted HERE rather than in the parent because this is the only place the
	// entry the store built can be seen BEFORE it is written. If the store handed
	// the log anything other than a complete, redeemed record, the parent's
	// "the consumption record is durable" assertion would be examining bytes that
	// never meant what it thinks.
	if e.Kind != invite.RecordKind {
		return wal.Committed{}, fmt.Errorf("child: the invite store handed the durable log an entry of kind %q, want %q", e.Kind, invite.RecordKind)
	}
	rec, err := invite.DecodeRecord(e.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the entry the invite store handed the durable log does not decode as an invite record: %v", err)
	}
	if rec.State != invite.StateRedeemed {
		return wal.Committed{}, fmt.Errorf("child: the entry carries a %s record; this crash point exists to prove the CONSUMPTION record is durable, so it must be the consumption record that is written", rec.State)
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	killSelf()
	return c, nil
}

// killSelf kills this process with SIGKILL. SIGKILL cannot be caught, blocked or
// ignored, so nothing deferred, buffered or graceful runs afterwards — which is
// the entire evidentiary value of this test over a polite Close.
func killSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking rather
	// than looping means a platform where that is somehow untrue fails loudly
	// instead of hanging the suite.
	panic("invite crash test: SIGKILL to self did not kill the process")
}

// runInviteCrashChild re-execs this test binary at the given crash point and
// asserts the child really DIED ON SIGKILL rather than failing its own
// assertions — without that check a broken child would silently turn the parent
// into a test of a directory nothing was ever written to.
func runInviteCrashChild(t *testing.T, point, dir, id, secret string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}

	// Bounded, so a wedged child fails this test in a minute rather than hanging
	// the suite until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestInviteCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envInviteCrashPoint+"="+point,
		envInviteCrashDir+"="+dir,
		envInviteCrashID+"="+id,
		envInviteCrashSecret+"="+secret,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so "err != nil"
	// is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s",
			point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// replayCommitted reads the committed history straight off the crashed log,
// read-only, without opening a writer on it. It is how the parent learns what
// the dying child actually got onto stable storage.
func replayCommitted(t *testing.T, dir string) []wal.Committed {
	t.Helper()
	var got []wal.Committed
	if _, err := wal.Replay(filepath.Join(dir, wal.WALFileName), func(c wal.Committed) error {
		got = append(got, c)
		return nil
	}); err != nil {
		t.Fatalf("replaying the crashed log in %s: %v", dir, err)
	}
	return got
}

// inviteRecordsIn decodes every invite record in a committed history.
func inviteRecordsIn(t *testing.T, committed []wal.Committed) []invite.Record {
	t.Helper()
	out := make([]invite.Record, 0, len(committed))
	for i, c := range committed {
		if c.Entry.Kind != invite.RecordKind {
			t.Fatalf("committed entry %d has kind %q, want %q", i, c.Entry.Kind, invite.RecordKind)
		}
		r, err := invite.DecodeRecord(c.Entry.Body)
		if err != nil {
			t.Fatalf("committed entry %d does not decode as an invite record: %v", i, err)
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// PINNED: the crash
// ---------------------------------------------------------------------------

// TestInviteSingleUseSurvivesCrash injects a real SIGKILL the instant a
// redemption's commit record is fsynced, and proves the three things that make
// the invite gate worth having. See the file comment for why each one matters.
func TestInviteSingleUseSurvivesCrash(t *testing.T) {
	dir := t.TempDir()

	// --- Phase 1, in THIS process: mint the invite ---------------------------
	//
	// The parent mints so that it holds the plaintext secret. Mint returns it
	// exactly once and nothing stores it, so a child that minted would take the
	// only copy with it when it died — and assertion (b), which turns on
	// presenting the CORRECT secret after the crash, could not be written at all.
	st, lg := openStore(t, dir, nil)
	minted := mustMint(t, st, invite.MintRequest{Label: "the crash fixture", TTL: time.Hour})
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log before handing the directory to the child: %v", err)
	}

	// --- Phase 2: the child redeems it and is SIGKILLed at the commit fsync ---
	runInviteCrashChild(t, inviteCrashRedeemPostCommit, dir, minted.ID, minted.Secret)

	// --- (a) THE CONSUMPTION RECORD IS ON STABLE STORAGE ---------------------
	//
	// Without this the rest could pass just as happily against a directory where
	// the redemption never happened, and would prove nothing.
	committed := replayCommitted(t, dir)
	if len(committed) != 2 {
		t.Fatalf("the crashed log holds %d committed entries, want exactly 2 (the parent's mint and the child's redemption): the child died before its redemption was durable, so there is no post-commit crash to recover from", len(committed))
	}
	recs := inviteRecordsIn(t, committed)
	if recs[0].State != invite.StateOpen || recs[0].ID != minted.ID {
		t.Fatalf("the first committed entry is %s for %s, want the parent's open mint of %s", recs[0].State, recs[0].ID, minted.ID)
	}
	spent := recs[1]
	if spent.ID != minted.ID {
		t.Fatalf("the consumption record names invite %s, want %s", spent.ID, minted.ID)
	}
	if spent.State != invite.StateRedeemed {
		t.Fatalf("the second committed entry is %s, want redeemed: the record that reached stable storage is not a consumption record", spent.State)
	}
	if spent.RedeemKey != inviteCrashKey || spent.RedeemedBy != inviteCrashAgentID {
		t.Fatalf("the consumption record names key %q / agent %q, want %q / %q", spent.RedeemKey, spent.RedeemedBy, inviteCrashKey, inviteCrashAgentID)
	}
	if spent.RedeemFingerprint != fingerprintOf(inviteCrashPayload) {
		t.Fatalf("the consumption record carries a different payload fingerprint; without the one the child computed, a legitimate retry would be reported as a key-reuse violation")
	}
	if !bytes.Equal(spent.Result, inviteCrashResult) {
		t.Fatalf("the consumption record stores result %s, want %s: without the stored result a retry could only be refused, never answered", spent.Result, inviteCrashResult)
	}
	if spent.RedeemedAt.IsZero() {
		t.Fatalf("the consumption record has no redeemed_at, so spent-record retention could never fire on it")
	}

	// The secret is a bearer credential and the crashed log is the file that
	// outlives the process. It must not be in there, even now.
	raw, err := readWAL(dir)
	if err != nil {
		t.Fatalf("reading the crashed log: %v", err)
	}
	if bytes.Contains(raw, []byte(minted.Secret)) {
		t.Fatalf("the plaintext invite secret is in the crashed bus.wal")
	}

	// --- RECOVERY: a FRESH store, rebuilt only from the crashed log ----------
	st2, lg2 := openStore(t, dir, nil)
	defer func() { _ = lg2.Close() }()

	if got := lg2.Recovered().Applied; got != 2 {
		t.Fatalf("recovery applied %d committed entries, want 2", got)
	}
	recovered, ok := st2.Lookup(minted.ID)
	if !ok {
		t.Fatalf("after recovering from a SIGKILL the invite is not in the table at all; a process killed with -9 flushes nothing, so this state has to come off the durable log")
	}
	if recovered.State != invite.StateRedeemed {
		t.Fatalf("after recovering from a SIGKILL the invite is %s, want redeemed: the crash FORGOT that this invite was spent, and one invite now admits two agents", recovered.State)
	}
	assertRecordEqual(t, "the invite recovered from the crashed log", recovered, spent)

	// --- (b) A SECOND REDEMPTION WITH THE CORRECT SECRET IS REFUSED ----------
	//
	// This is the whole point. The caller here holds the genuine bearer
	// credential; the ONLY thing standing between it and a second agent on the
	// bus is the consumption record recovered above.
	for _, key := range []string{"k-second-attempt", "k-third-attempt"} {
		r, err := st2.Begin(invite.RedeemRequest{
			InviteID:    minted.ID,
			Secret:      minted.Secret,
			Key:         key,
			Fingerprint: fingerprintOf("a fresh enrolment payload"),
		})
		if !errors.Is(err, invite.ErrAlreadyRedeemed) {
			t.Fatalf("a SECOND redemption presenting the CORRECT secret under key %q gave (%v, %v) after the crash, want ErrAlreadyRedeemed: single use did not survive the kill", key, r, err)
		}
		if r != nil {
			t.Fatalf("Begin returned a Redemption for an invite the durable log says is spent")
		}
	}
	if _, err := st2.Redeem(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         "k-second-attempt",
		Fingerprint: fingerprintOf("a fresh enrolment payload"),
	}, invite.Result{AgentID: testBusID + ".intruder", Response: json.RawMessage(`{"agent_id":"bus-test.intruder"}`)}); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("Redeem of the spent invite after the crash gave err = %v, want ErrAlreadyRedeemed", err)
	}

	// --- (c) THE LEGITIMATE RETRY REPLAYS THE ORIGINAL RESULT ---------------
	//
	// The client saw a dropped connection, not a result. Its retry — same key,
	// same fingerprint — must be ANSWERED, not punished: this is invariant 10's
	// carve-out, and it has to work across the crash boundary because the crash
	// is precisely what made the client retry.
	retry, err := st2.Begin(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         inviteCrashKey,
		Fingerprint: fingerprintOf(inviteCrashPayload),
	})
	if err != nil {
		t.Fatalf("the legitimate retry after the crash gave err = %v, want the ORIGINAL result and no error: this is a retry of an acknowledgement the crash destroyed, so it must not error and must not disconnect the client", err)
	}
	if retry.Outcome() != invite.OutcomeReplay {
		t.Fatalf("the retry after the crash has outcome %s, want replay: the caller must be able to tell that NOTHING was re-applied", retry.Outcome())
	}
	if !bytes.Equal(retry.Result(), inviteCrashResult) {
		t.Fatalf("the retry after the crash replayed %s, want the original %s verbatim", retry.Result(), inviteCrashResult)
	}
	if _, err := retry.Consume(invite.Result{AgentID: inviteCrashAgentID}); err == nil {
		t.Fatalf("Consume on the post-crash replay succeeded; a replay reserved nothing and a second consumption record would be a second redemption")
	}
	if err := retry.Commit(); err != nil {
		t.Errorf("Commit on the post-crash replay gave err = %v, want nil", err)
	}
	// Through the standalone path too: the same answer, and the ORIGINAL record.
	again, err := st2.Redeem(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         inviteCrashKey,
		Fingerprint: fingerprintOf(inviteCrashPayload),
	}, invite.Result{AgentID: "bus-test.somebody-else", Response: json.RawMessage(`{"agent_id":"bus-test.somebody-else"}`)})
	if err != nil {
		t.Fatalf("Redeem as a retry after the crash gave err = %v, want the original record", err)
	}
	assertRecordEqual(t, "the record the post-crash retry returns", again, spent)

	// --- The violation, ACROSS the crash boundary ---------------------------
	//
	// The fingerprint that separates a retry from key reuse has to have survived
	// on disk, or a key reused for new content is silently answered with somebody
	// else's result.
	if _, err := st2.Begin(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         inviteCrashKey,
		Fingerprint: fingerprintOf(inviteCrashOtherPayload),
	}); !errors.Is(err, invite.ErrKeyReuse) {
		t.Fatalf("reusing the key for a DIFFERENT payload after the crash gave err = %v, want ErrKeyReuse", err)
	}

	// --- Nothing above wrote a thing ----------------------------------------
	if after := replayCommitted(t, dir); len(after) != 2 {
		t.Fatalf("after the refused redemptions and the retries the durable log holds %d committed entries, want the same 2: a refusal or a replay wrote a SECOND effect to disk", len(after))
	}
	if got := mustLookup(t, st2, minted.ID, "at the end of the crash test"); got.RedeemedBy != inviteCrashAgentID {
		t.Fatalf("the invite ended up redeemed by %q, want the child's %q: something after the crash overwrote the consumption record", got.RedeemedBy, inviteCrashAgentID)
	}

	// And it is still spent after ANOTHER restart: recovery from a crashed log
	// has to be a fixed point, not a one-off.
	if err := lg2.Close(); err != nil {
		t.Fatalf("closing the recovered log: %v", err)
	}
	st3, lg3 := openStore(t, dir, nil)
	defer func() { _ = lg3.Close() }()
	if got := mustLookup(t, st3, minted.ID, "after a second restart").State; got != invite.StateRedeemed {
		t.Fatalf("after a clean restart following the crash the invite is %s, want redeemed", got)
	}
	if _, err := st3.Begin(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         "k-yet-another-attempt",
		Fingerprint: fingerprintOf("payload"),
	}); !errors.Is(err, invite.ErrAlreadyRedeemed) {
		t.Fatalf("after a clean restart following the crash a second redemption gave err = %v, want ErrAlreadyRedeemed", err)
	}

	// A guard against this file drifting away from the fingerprint helper it
	// shares with the child: if the two ever computed different digests the
	// retry above would look like a key-reuse violation and this test would fail
	// for a reason nobody could read.
	if fingerprintOf(inviteCrashPayload) == (idem.Fingerprint{}) {
		t.Fatalf("the crash fixture fingerprint is the zero value")
	}
}
