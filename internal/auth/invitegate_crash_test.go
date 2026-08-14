package auth_test

// INVITE-GATE, part 1: CRASH INJECTION over the COMPOSITE enrol+invite write.
//
// The claim under test, in one line: AN INVITE IS SPENT IF AND ONLY IF THE
// ENROLMENT IT AUTHORISED IS DURABLE. There is no in-between, because the two
// records are ONE transaction — one prepare, one commit, one wal.Entry of kind
// auth.EnrolInviteRecordKind.
//
// "The code looks right" is not evidence for a durability claim, so each point
// in the composite write path is exercised by a real process that is really
// SIGKILLed:
//
//	A  after the PREPARE fsync, before COMMIT  -> NEITHER half applied: the agent
//	                                              is not on the roster AND the
//	                                              invite is still open and still
//	                                              REDEEMABLE with its correct
//	                                              secret. The agent-id suffix is
//	                                              BURNED all the same.
//	B  after the COMMIT fsync, before any ack  -> BOTH halves: the agent is on the
//	                                              roster with every field intact
//	                                              (including InviteID provenance)
//	                                              AND the invite is SPENT — a
//	                                              fresh store rebuilt from that
//	                                              log REFUSES a second redemption
//	                                              presenting the CORRECT secret.
//	C  a TORN COMMIT frame                     -> repaired tail, state as at A.
//	D  a TORN PREPARE frame                    -> repaired tail, state as at A
//	                                              except that the log is
//	                                              PROVABLY unable to name the
//	                                              burned suffix (see point D).
//
// Point A is the half that makes the gate safe to fail: an invite burned with no
// agent behind it is an operator handing out a credential that admits nobody.
// Point B is the half that makes the gate WORTH HAVING: without it one invite
// admits two agents across a crash and the whole mechanism is decorative.
//
// # THE RECOVERY SIDE GOES THROUGH THE SHIPPED MULTIPLEXER
//
// Every reopen here wires auth.NewMultiplexApplier over the roster AND the
// invite store, because that is what EXPANDS a composite entry into its two
// halves. A test that replayed with a bare *auth.WALRoster would prove nothing
// about the shipped path: the roster skips a kind it does not own, so the
// enrolment half would vanish and every "the agent is absent" assertion here
// would pass for entirely the wrong reason.
//
// The pattern is internal/auth/crash_test.go's and internal/invite/
// crash_test.go's, deliberately: an env-selected crash point, a killer wrapped
// around the REAL *wal.Log, a parent that asserts the child DIED ON SIGKILL via
// syscall.WaitStatus (a child that merely failed its own assertions also exits
// non-zero, so the wait status is the assertion), and the PARENT minting the
// invite so that it knows the secret — Mint returns it exactly once and nothing
// stores it, so a child that minted would take the only copy to the grave and
// point A's "still redeemable with the CORRECT secret" could not be written.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	envIGCrashPoint  = "INVITE_GATE_CRASH_POINT"
	envIGCrashDir    = "INVITE_GATE_CRASH_DIR"
	envIGCrashID     = "INVITE_GATE_CRASH_ID"
	envIGCrashSecret = "INVITE_GATE_CRASH_SECRET"

	// igCrashAfterPrepare: the composite entry's PREPARE is fsynced and the
	// transaction is open. Death.
	igCrashAfterPrepare = "after-prepare"

	// igCrashAfterCommit: the composite entry's prepare AND commit are fsynced
	// and both halves have been applied by the multiplexer — and then death,
	// before PutWithInvite returns, before Redemption.Commit runs, before Enrol
	// returns and before any client is acknowledged.
	igCrashAfterCommit = "after-commit"

	// igCrashTornCommit: the composite prepare is fsynced and a PARTIAL commit
	// frame sits on the end of the file.
	igCrashTornCommit = "torn-commit"

	// igCrashTornPrepare: the composite entry never got a whole PREPARE. The
	// partial frame IS the record carrying both halves, so the only bytes that
	// ever named this agent id are the bytes that did not survive.
	igCrashTornPrepare = "torn-prepare"
)

// The fixture the parent and the child must agree on byte for byte.
const (
	// igControlName is an UN-INVITED enrolment the PARENT records before handing
	// the directory over. It is the "damage does not cascade backwards" control:
	// every point below asserts it came back unchanged.
	igControlName = "control"

	// igInvitedName is the name the child's INVITED enrolment asks for. A fresh
	// ids.NewNameSuffixes in the child mints suffix 1 for it, and it is a
	// DIFFERENT name from the control so that "the floor rose because of the
	// invited enrolment" can never be confused with "the floor rose because of
	// the control".
	igInvitedName = "invited"

	// igEnrolKey is the idempotency key the child's enrolment carries, on both
	// the auth service and the invite redemption (the two must agree, or the
	// invite's own idempotency scope would be testing a different request).
	igEnrolKey = "k-invite-gate-crash"

	igEnrolPayload = "the enrolment acknowledgement this client never received"
)

// igEnrolTime is the fixed instant the child's auth.Service stamps its
// enrolment with, so the parent can assert exact values across a process
// boundary. The INVITE store deliberately keeps the real clock: its TTL and
// retention windows are what make a minted invite live for the duration of the
// test.
var igEnrolTime = time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)

// igFingerprint is the payload fingerprint the redemption carries. The child and
// the parent must compute the same one or the parent's retry assertions would be
// testing the key-reuse violation path by accident.
func igFingerprint() idem.Fingerprint {
	return idem.ComputeFingerprint([]byte(igEnrolPayload))
}

// igControlEntry is the un-invited enrolment the parent records first. Every
// reserved field is populated so "came back unchanged" is a claim with content.
func igControlEntry(t *testing.T) auth.RosterEntry {
	t.Helper()
	return auth.RosterEntry{
		AgentID:            mustAgentID(t, igControlName, 1),
		Name:               igControlName,
		AuthPublicKey:      fixedKey(0xC0),
		MessagingPublicKey: fixedKey(0xC1),
		Epoch:              igEnrolTime,
		EnrolledAt:         igEnrolTime,
	}
}

// ---------------------------------------------------------------------------
// The wiring under test: roster + invite store behind THE multiplexer
// ---------------------------------------------------------------------------

// igOpen performs the wiring cmd/agent-bus/main.go performs, and the order of
// which is not optional: both appliers first, then the multiplexer over them,
// then wal.Open (which REPLAYS through the multiplexer), then Attach on each.
//
// writer is what the ROSTER writes through — the real log on a plain open, a
// killer on a crash child. A nil writer means "the log itself".
func igOpen(t *testing.T, dir string, logger *logging.Logger, writer func(*wal.Log) auth.DurableWriter) (*auth.WALRoster, *invite.Store, *wal.Log) {
	t.Helper()

	roster := auth.NewWALRoster(logger)
	store, err := invite.NewStore(invite.StoreOptions{BusID: testBusID, Logger: logger})
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	mux, err := auth.NewMultiplexApplier(logger, map[string]wal.Applier{
		auth.RecordKind:   roster,
		invite.RecordKind: store,
	})
	if err != nil {
		t.Fatalf("auth.NewMultiplexApplier: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: mux})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	var w auth.DurableWriter = lg
	if writer != nil {
		w = writer(lg)
	}
	if err := roster.Attach(w); err != nil {
		lg.Close()
		t.Fatalf("roster.Attach: %v", err)
	}
	if err := store.Attach(lg); err != nil {
		lg.Close()
		t.Fatalf("store.Attach: %v", err)
	}
	return roster, store, lg
}

// igRedemption adapts *invite.Redemption to auth.InviteRedemption, exactly as
// internal/httpapi's inviteRedemption does. It is written out here rather than
// imported because internal/httpapi imports internal/auth, and because this
// package's crash evidence must not depend on the HTTP layer being wired
// correctly.
type igRedemption struct {
	red *invite.Redemption
	id  string
}

func (a *igRedemption) InviteID() string  { return a.id }
func (a *igRedemption) RiderKind() string { return invite.RecordKind }

func (a *igRedemption) Consume(res auth.EnrolResult) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{
		"agent_id":    res.AgentID,
		"bus_id":      res.BusID,
		"name":        res.Name,
		"enrolled_at": res.EnrolledAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	return a.red.Consume(invite.Result{AgentID: res.AgentID, Response: body})
}

func (a *igRedemption) Commit() { _ = a.red.Commit() }
func (a *igRedemption) Abort()  { a.red.Abort() }

// ---------------------------------------------------------------------------
// The killers
// ---------------------------------------------------------------------------

// igSuicide SIGKILLs this process. SIGKILL cannot be caught, blocked or
// ignored, so nothing deferred, buffered or graceful runs afterwards — which is
// the whole evidentiary value of this file over a polite Close.
func igSuicide() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking rather
	// than looping means a platform where that is somehow untrue fails loudly
	// instead of hanging the suite.
	panic("invite-gate crash test: SIGKILL to self did not kill the process")
}

// igKiller is the auth.DurableWriter the roster writes through in a crash
// child. It sits exactly where PutWithInvite hands the composite entry to the
// log, which is the only place the two halves are visible as ONE thing before
// they reach the platter.
type igKiller struct {
	l     *wal.Log
	point string
}

func (k *igKiller) Write(e wal.Entry) (wal.Committed, error) {
	// Asserted HERE because this is the only place the entry the roster built
	// can be seen BEFORE it is written. If PutWithInvite handed the log anything
	// other than ONE composite entry carrying BOTH halves, every assertion the
	// parent makes about atomicity would be examining bytes that never meant
	// what it thinks.
	if e.Kind != auth.EnrolInviteRecordKind {
		return wal.Committed{}, fmt.Errorf("child: the roster handed the durable log an entry of kind %q, want %q: an invited enrolment that is not ONE composite entry is not one transaction", e.Kind, auth.EnrolInviteRecordKind)
	}
	entry, rider, err := auth.DecodeEnrolWithInvite(e.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the composite entry does not decode: %v", err)
	}
	if rider.Kind != invite.RecordKind {
		return wal.Committed{}, fmt.Errorf("child: the composite entry's rider kind is %q, want %q", rider.Kind, invite.RecordKind)
	}
	rec, err := invite.DecodeRecord(rider.Body)
	if err != nil {
		return wal.Committed{}, fmt.Errorf("child: the composite entry's rider is not an invite record: %v", err)
	}
	if rec.State != invite.StateRedeemed {
		return wal.Committed{}, fmt.Errorf("child: the rider carries a %s record; the whole point is that the CONSUMPTION record rides with the enrolment", rec.State)
	}
	if entry.InviteID != rec.ID {
		return wal.Committed{}, fmt.Errorf("child: the enrolment half records invite %q but the rider is invite %q", entry.InviteID, rec.ID)
	}

	switch k.point {
	case igCrashAfterCommit:
		// The whole real path: prepare fsync, commit fsync, Apply through the
		// multiplexer. Then death, before this function returns.
		if _, err := k.l.Write(e); err != nil {
			return wal.Committed{}, err
		}
		igSuicide()

	case igCrashAfterPrepare:
		// The prepare is fsynced and the transaction is left OPEN.
		if _, err := k.l.Begin(e); err != nil {
			return wal.Committed{}, err
		}
		igSuicide()

	case igCrashTornCommit:
		txn, err := k.l.Begin(e)
		if err != nil {
			return wal.Committed{}, err
		}
		// ---------------------------------------------------------------
		// HONEST ACCOUNT, inherited verbatim in substance from
		// internal/auth/crash_test.go: a SIGKILL cannot by itself tear a
		// write. os.File.Write is one syscall and the bytes land in the PAGE
		// CACHE, which outlives the process, so killing between two appends
		// leaves WHOLE frames — that is point A, not this one. The torn bytes
		// are written deliberately; what the kill contributes, and cannot be
		// faked, is that nothing graceful runs afterwards.
		//
		// The 32 MAC bytes are ZEROS and that is SAID OUT LOUD: the frame tag
		// is keyed per file and the keying is unexported, so this package
		// cannot compute it. The difference is UNOBSERVABLE here — the length
		// header promises more payload than the file holds, so the reader hits
		// EOF mid-payload and never reaches tag verification. The tear is
		// genuine; only the bytes the tear makes unreadable are fake.
		// ---------------------------------------------------------------
		igAppendTornFrame(k.l.Path(), wal.TypeCommit, txn.PrepareIndex()+1,
			[]byte(fmt.Sprintf(`{"prepare_index":%d}`, txn.PrepareIndex())))
		igSuicide()

	case igCrashTornPrepare:
		// The log is never touched through its own API: the frame carrying BOTH
		// halves is appended by hand and cut in the middle of its payload, so
		// the bytes that named this agent id and this invite consumption are
		// precisely the bytes that did not reach the platter.
		payload, err := json.Marshal(struct {
			Kind string          `json:"kind"`
			TS   string          `json:"ts"`
			Body json.RawMessage `json:"body"`
		}{Kind: e.Kind, TS: igEnrolTime.Format(time.RFC3339Nano), Body: e.Body})
		if err != nil {
			return wal.Committed{}, fmt.Errorf("child: marshalling the prepare payload: %v", err)
		}
		// The child has written NOTHING through the log since Open, so the next
		// index is exactly the one recovery reported. The parent asserts the
		// value the repair report names, which is what stops a wrong guess here
		// from quietly turning point D into a test of a frame nobody looks at.
		igAppendTornFrame(k.l.Path(), wal.TypePrepare, k.l.Recovered().NextIndex, payload)
		igSuicide()
	}
	return wal.Committed{}, fmt.Errorf("child: unknown crash point %q", k.point)
}

// igAppendTornFrame appends a version-2 frame
//
//	payloadLen[4] ++ index[8] ++ type[2] ++ reserved[2] ++ mac[32] ++ payload
//
// cut short in the MIDDLE OF ITS PAYLOAD. It never returns: the caller kills the
// process on the next statement, and there is deliberately no Close, no Sync and
// no defer between the two.
func igAppendTornFrame(path string, typ wal.Type, index uint64, payload []byte) {
	frame := make([]byte, wal.FrameHeaderSize+len(payload))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint64(frame[4:12], index)
	binary.BigEndian.PutUint16(frame[12:14], uint16(typ))
	binary.BigEndian.PutUint16(frame[14:16], 0) // reserved
	// frame[16:48] is the MAC and is left zero -- see the caller's comment.
	copy(frame[wal.FrameHeaderSize:], payload)

	partial := wal.FrameHeaderSize + len(payload)/2
	if partial <= wal.FrameHeaderSize || partial >= len(frame) {
		panic(fmt.Sprintf("child: a %d-byte cut of a %d-byte frame is not a torn payload", partial, len(frame)))
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		panic(fmt.Sprintf("child: OpenFile to append the torn frame: %v", err))
	}
	if _, err := f.Write(frame[:partial]); err != nil {
		panic(fmt.Sprintf("child: writing the torn frame: %v", err))
	}
	// No Close, no Sync, no defer.
}

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestInviteGateCrashChild is the child half. It does NOTHING in a normal run:
// without envIGCrashPoint it skips immediately, so its presence costs the suite
// nothing.
func TestInviteGateCrashChild(t *testing.T) {
	point := os.Getenv(envIGCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envIGCrashPoint + " is unset")
	}
	dir := os.Getenv(envIGCrashDir)
	inviteID := os.Getenv(envIGCrashID)
	secret := os.Getenv(envIGCrashSecret)
	if dir == "" || inviteID == "" || secret == "" {
		t.Fatalf("child: %s=%q but the fixture is incomplete (dir=%q id=%q secret set=%v)", envIGCrashPoint, point, dir, inviteID, secret != "")
	}

	// NO deferred Close and NO t.Cleanup on the log: a Close that ran would be
	// exactly the graceful shutdown this test exists to rule out.
	roster, store, _ := igOpen(t, dir, nil, func(l *wal.Log) auth.DurableWriter {
		return &igKiller{l: l, point: point}
	})

	// The parent's control enrolment and the parent's mint must both have come
	// back, or the crash below would be injected into a directory that does not
	// hold what every parent assertion assumes.
	if _, ok := roster.Get(mustAgentID(t, igControlName, 1)); !ok {
		t.Fatalf("child: the control enrolment was not recovered from the log the parent wrote")
	}
	rec, ok := store.Lookup(inviteID)
	if !ok {
		t.Fatalf("child: invite %s was not recovered; there is nothing to redeem", inviteID)
	}
	if rec.State != invite.StateOpen {
		t.Fatalf("child: invite %s recovered as %s, want open", inviteID, rec.State)
	}

	minter, err := ids.NewAgentIDMinter(testBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("child: building the minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{
		Minter: minter,
		Roster: roster,
		Now:    func() time.Time { return igEnrolTime },
	})
	if err != nil {
		t.Fatalf("child: auth.NewService: %v", err)
	}

	red, err := store.Begin(invite.RedeemRequest{
		InviteID:    inviteID,
		Secret:      secret,
		Key:         igEnrolKey,
		Fingerprint: igFingerprint(),
	})
	if err != nil {
		t.Fatalf("child: invite.Begin: %v", err)
	}
	if red.Outcome() != invite.OutcomeReserved {
		t.Fatalf("child: Begin outcome = %s, want reserved", red.Outcome())
	}

	res, err := svc.Enrol(auth.EnrolRequest{
		Name:               igInvitedName,
		PublicKey:          fixedKey(0xE1),
		MessagingPublicKey: fixedKey(0xE2),
		IdempotencyKey:     igEnrolKey,
		Invite:             &igRedemption{red: red, id: inviteID},
	})
	t.Fatalf("child: Enrol returned (%+v, %v) but the durable writer kills this process inside PutWithInvite; the crash was never injected", res, err)
}

// runIGCrashChild re-execs this test binary at the given crash point and PROVES
// the child died on SIGKILL rather than failing its own assertions. Without that
// check a child that t.Fatalf'd on its first line would exit 1, leave the
// parent's directory untouched, and every "neither half is applied" assertion
// below would pass for the wrong reason.
func runIGCrashChild(t *testing.T, point, dir, inviteID, secret string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestInviteGateCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envIGCrashPoint+"="+point,
		envIGCrashDir+"="+dir,
		envIGCrashID+"="+inviteID,
		envIGCrashSecret+"="+secret,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child %q did not finish in time: %v\n--- child output ---\n%s", point, ctx.Err(), out.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child %q: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", point, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child %q: wait status is %T, want syscall.WaitStatus", point, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected, so nothing below is being tested\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// igSeed builds the directory the child is handed: one un-invited control
// enrolment and one OPEN invite, both durable, with the log closed cleanly
// afterwards.
//
// The PARENT mints, because Mint returns the plaintext secret exactly once and
// nothing stores it. A child that minted would take the only copy with it when
// it died, and the assertions that turn on presenting the CORRECT secret after
// the crash could not be written at all.
func igSeed(t *testing.T, dir string) invite.Minted {
	t.Helper()
	roster, store, lg := igOpen(t, dir, nil, nil)
	if err := roster.Put(igControlEntry(t)); err != nil {
		t.Fatalf("recording the control enrolment: %v", err)
	}
	minted, err := store.Mint(invite.MintRequest{Label: "the invite-gate crash fixture", TTL: time.Hour})
	if err != nil {
		t.Fatalf("minting the crash fixture invite: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log before handing the directory to the child: %v", err)
	}
	return minted
}

// igAssertControlSurvived is the "damage does not cascade BACKWARDS" check every
// point makes: the control enrolment committed before the damage and must come
// back byte for byte.
func igAssertControlSurvived(t *testing.T, r *auth.WALRoster) {
	t.Helper()
	want := normaliseEntry(igControlEntry(t))
	got, ok := r.Get(want.AgentID)
	if !ok {
		t.Fatalf("the control agent %q is NOT on the roster after the crash; its own commit fsynced before the damage, which is BEHIND it and must not cascade", want.AgentID)
	}
	if got.AgentID != want.AgentID || got.Name != want.Name || !bytes.Equal(got.AuthPublicKey, want.AuthPublicKey) ||
		!bytes.Equal(got.MessagingPublicKey, want.MessagingPublicKey) || !got.EnrolledAt.Equal(want.EnrolledAt) {
		t.Fatalf("the control agent came back changed.\n  got  %+v\n  want %+v", got, want)
	}
}

// igAssertNeitherHalfApplied is points A, C and D's shared claim, and it is the
// whole of the "if and only if" in one direction: the enrolment did not commit,
// so the invite MUST still be spendable, and the agent MUST be absent.
//
// The invite is proved redeemable BY REDEEMING IT, not by reading its state. A
// record that says "open" while some in-flight reservation, retention sweep or
// digest mismatch would refuse the next Begin is an invite that admits nobody,
// which is exactly the failure this half exists to rule out.
func igAssertNeitherHalfApplied(t *testing.T, r *auth.WALRoster, st *invite.Store, minted invite.Minted, point string) {
	t.Helper()

	invitedID := mustAgentID(t, igInvitedName, 1)
	if _, ok := r.Get(invitedID); ok {
		t.Fatalf("[%s] agent %q IS on the roster, but the composite entry's COMMIT never reached disk; recovery surfaced an enrolment no caller was ever told about (invariant 4)", point, invitedID)
	}
	if n := r.Len(); n != 1 {
		t.Fatalf("[%s] the roster holds %d agents, want exactly 1 (the control)", point, n)
	}

	rec, ok := st.Lookup(minted.ID)
	if !ok {
		t.Fatalf("[%s] invite %s is not in the rebuilt table at all; the parent's mint committed long before the damage", point, minted.ID)
	}
	if rec.State != invite.StateOpen {
		t.Fatalf(`[%s] invite %s recovered as %s, want open.

The enrolment it authorised is NOT durable, so the invite MUST NOT be spent:
the two are ONE transaction. An invite burned with no agent behind it is an
operator handing out a single-use credential that admits nobody, with nothing
telling them why.`, point, minted.ID, rec.State)
	}

	// REDEEMED FOR REAL, through the standalone path, with the CORRECT secret.
	spent, err := st.Redeem(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         "k-after-the-crash",
		Fingerprint: idem.ComputeFingerprint([]byte("a fresh enrolment after the crash")),
	}, invite.Result{
		AgentID:  testBusID + ".retry-1",
		Response: json.RawMessage(`{"agent_id":"` + testBusID + `.retry-1"}`),
	})
	if err != nil {
		t.Fatalf(`[%s] redeeming the invite after the crash gave err = %v, want success.

Nothing the child did became durable, so the invite is still the operator's to
spend. If this refuses, a crash between the prepare and the commit has silently
consumed a single-use credential.`, point, err)
	}
	if spent.State != invite.StateRedeemed {
		t.Fatalf("[%s] the post-crash redemption left the invite %s, want redeemed", point, spent.State)
	}
}

// igFloors derives the enrolment suffix floors from a repaired log.
func igFloors(t *testing.T, path string) map[string]uint64 {
	t.Helper()
	floors, err := auth.EnrolmentSuffixesInWAL(path, testBusID)
	if err != nil {
		t.Fatalf("EnrolmentSuffixesInWAL(%s): %v", path, err)
	}
	return floors
}

// ---------------------------------------------------------------------------
// PINNED: point A
// ---------------------------------------------------------------------------

// TestInviteGateCrashAfterPrepare is POINT A: the process died between the
// composite entry's prepare fsync and its commit.
//
// NEITHER half is applied — and the pairing that makes the design correct is
// that the agent-id SUFFIX IS BURNED ANYWAY. The composite prepare reached the
// platter, so the number it names is a number this bus issued; a floor that did
// not see it would hand the next "invited" enrolment an id this bus has already
// written down (invariant 1). A fold that looked only at auth.RecordKind would
// miss it entirely, which is precisely why floors.go unwraps the composite.
func TestInviteGateCrashAfterPrepare(t *testing.T) {
	dir := t.TempDir()
	minted := igSeed(t, dir)
	runIGCrashChild(t, igCrashAfterPrepare, dir, minted.ID, minted.Secret)

	r, st, lg := igOpen(t, dir, nil, nil)
	defer lg.Close()

	igAssertControlSurvived(t, r)

	// The floors are read BEFORE the post-crash redemption below writes anything
	// further, so the scan is over exactly the bytes the dying child left.
	floors := igFloors(t, lg.Path())
	if got := floors[igControlName]; got != 1 {
		t.Fatalf("the floor for %q is %d, want 1: the control enrolment is a plain agent record and must still be counted", igControlName, got)
	}
	if got := floors[igInvitedName]; got != 1 {
		t.Fatalf(`the floor for %q is %d, want 1.

The invited enrolment's PREPARE was fsynced before the kill, so its number
reached the platter. It is NOT enrolled and never will be -- and it must still
never be re-issued. The suffix lives inside the COMPOSITE record's enrolment
half, so a derivation that folds only %q records reports %d here and hands the
next %q enrolment an id this bus has already written down (invariant 1).`,
			igInvitedName, got, auth.RecordKind, got, igInvitedName)
	}
	if got, want := mintOverFloors(t, floors, igInvitedName), mustAgentID(t, igInvitedName, 2); got != want {
		t.Fatalf("the next %q enrolment minted %q, want %q", igInvitedName, got, want)
	}

	igAssertNeitherHalfApplied(t, r, st, minted, igCrashAfterPrepare)
}

// ---------------------------------------------------------------------------
// PINNED: point B — the assertion that makes the gate non-decorative
// ---------------------------------------------------------------------------

// TestInviteGateCrashAfterCommit is POINT B, and it is invariant 4's actual
// claim applied to BOTH halves at once: the composite entry's commit record was
// fsynced and then the process died with no Close, no Sync, no defer, no runtime
// shutdown, and nothing acknowledged to any client.
//
// What must come back is BOTH halves, from ONE record:
//
//	the agent is on the roster, every field intact, including the InviteID
//	provenance that records WHICH invite admitted it;
//
//	AND the invite is SPENT — a fresh store rebuilt from that log REFUSES a
//	second redemption presenting the CORRECT secret. That is the assertion that
//	makes the gate worth having: without it one invite admits two agents across
//	a crash and single use is decorative.
//
// Note what the child did NOT get to do: Redemption.Commit never ran, so the
// invite was never folded into the dying process's in-memory table. Everything
// asserted below therefore has to come off the durable log.
func TestInviteGateCrashAfterCommit(t *testing.T) {
	dir := t.TempDir()
	minted := igSeed(t, dir)
	runIGCrashChild(t, igCrashAfterCommit, dir, minted.ID, minted.Secret)

	// (1) ONE composite entry carries both halves. Without this the rest could
	// pass against a log holding two separate records, which is exactly the
	// shape this design exists not to be.
	var composites int
	var gotEntry auth.RosterEntry
	var gotRider invite.Record
	if _, err := wal.Replay(walPath(dir), func(c wal.Committed) error {
		if c.Entry.Kind != auth.EnrolInviteRecordKind {
			return nil
		}
		composites++
		e, rider, err := auth.DecodeEnrolWithInvite(c.Entry.Body)
		if err != nil {
			t.Fatalf("the committed composite entry does not decode: %v", err)
		}
		rec, err := invite.DecodeRecord(rider.Body)
		if err != nil {
			t.Fatalf("the composite entry's rider is not an invite record: %v", err)
		}
		gotEntry, gotRider = e, rec
		return nil
	}); err != nil {
		t.Fatalf("replaying the crashed log: %v", err)
	}
	if composites != 1 {
		t.Fatalf("the crashed log holds %d committed composite entries, want exactly 1: the invite consumption and the enrolment must be ONE transaction", composites)
	}
	if gotEntry.AgentID != mustAgentID(t, igInvitedName, 1) {
		t.Fatalf("the composite's enrolment half names %q, want %q", gotEntry.AgentID, mustAgentID(t, igInvitedName, 1))
	}
	if gotRider.ID != minted.ID || gotRider.State != invite.StateRedeemed {
		t.Fatalf("the composite's rider is invite %s in state %s, want %s redeemed", gotRider.ID, gotRider.State, minted.ID)
	}
	if gotRider.RedeemedBy != gotEntry.AgentID {
		t.Fatalf("the consumption record says the invite was redeemed by %q, but the enrolment half in the SAME record mints %q; the two halves disagree about what happened", gotRider.RedeemedBy, gotEntry.AgentID)
	}

	// The secret is a bearer credential and the crashed log is the file that
	// outlives the process. It must not be in there, even now.
	raw, err := os.ReadFile(walPath(dir))
	if err != nil {
		t.Fatalf("reading the crashed log: %v", err)
	}
	if bytes.Contains(raw, []byte(minted.Secret)) {
		t.Fatalf("the plaintext invite secret is in the crashed bus.wal")
	}

	// (2) RECOVERY, through the shipped multiplexer.
	r, st, lg := igOpen(t, dir, nil, nil)
	defer lg.Close()

	igAssertControlSurvived(t, r)

	invitedID := mustAgentID(t, igInvitedName, 1)
	got, ok := r.Get(invitedID)
	if !ok {
		t.Fatalf(`agent %q is NOT on the roster.

Its composite entry's prepare AND commit were both fsynced before the kill.
Nothing about invariant 4 may depend on a clean shutdown -- and nothing about
the EXPANSION of a composite record may depend on one either: the enrolment half
has to be dispatched to the roster by the multiplexer on the recovery path
exactly as it was on the live one.`, invitedID)
	}
	if got.Name != igInvitedName {
		t.Errorf("the recovered name is %q, want %q", got.Name, igInvitedName)
	}
	if !bytes.Equal(got.AuthPublicKey, fixedKey(0xE1)) {
		t.Errorf("the auth public key did not survive the crash intact")
	}
	if !bytes.Equal(got.MessagingPublicKey, fixedKey(0xE2)) {
		t.Errorf("the messaging public key did not survive the crash intact")
	}
	if !got.EnrolledAt.Equal(igEnrolTime) || !got.Epoch.Equal(igEnrolTime) {
		t.Errorf("the enrolment timestamps came back as epoch=%s enrolled_at=%s, want %s", got.Epoch, got.EnrolledAt, igEnrolTime)
	}
	if got.InviteID != minted.ID {
		t.Errorf(`the recovered enrolment records invite_id %q, want %q.

The provenance is the only durable answer to "which invite admitted this agent".
Losing it does not un-enrol anybody, which is why it needs an assertion: an
operator revoking a leaked invite has no way to find what it let in.`, got.InviteID, minted.ID)
	}
	if n := r.Len(); n != 2 {
		t.Fatalf("the roster holds %d agents, want 2 (the control and the invited one)", n)
	}

	// (3) THE ASSERTION THAT MAKES THE GATE NON-DECORATIVE.
	rec, ok := st.Lookup(minted.ID)
	if !ok {
		t.Fatalf("invite %s is not in the rebuilt table at all", minted.ID)
	}
	if rec.State != invite.StateRedeemed {
		t.Fatalf(`after recovering from a SIGKILL the invite is %s, want redeemed.

Redemption.Commit NEVER RAN in the dying process, so this can only come off the
durable log -- which is the point. If the crash FORGOT that this invite was
spent, one invite admits two agents.`, rec.State)
	}
	if rec.RedeemedBy != invitedID {
		t.Errorf("the recovered consumption record names agent %q, want %q", rec.RedeemedBy, invitedID)
	}
	if rec.RedeemedAt.IsZero() {
		t.Errorf("the recovered consumption record has no redeemed_at, so spent-record retention could never fire on it")
	}

	for _, key := range []string{"k-second-attempt", "k-third-attempt"} {
		red, err := st.Begin(invite.RedeemRequest{
			InviteID:    minted.ID,
			Secret:      minted.Secret,
			Key:         key,
			Fingerprint: idem.ComputeFingerprint([]byte("a second agent's enrolment")),
		})
		if !errors.Is(err, invite.ErrAlreadyRedeemed) {
			t.Fatalf("a SECOND redemption presenting the CORRECT secret under key %q gave (%v, %v), want ErrAlreadyRedeemed: single use did not survive the kill", key, red, err)
		}
	}

	// (4) The legitimate RETRY still replays the original result across the
	// crash boundary. The client saw a dropped connection, not a 201, so this is
	// exactly the moment it retries — and invariant 10's carve-out says it must
	// be ANSWERED rather than punished.
	retry, err := st.Begin(invite.RedeemRequest{
		InviteID:    minted.ID,
		Secret:      minted.Secret,
		Key:         igEnrolKey,
		Fingerprint: igFingerprint(),
	})
	if err != nil {
		t.Fatalf("the legitimate retry after the crash gave err = %v, want the ORIGINAL result: the crash is precisely what made the client retry", err)
	}
	if retry.Outcome() != invite.OutcomeReplay {
		t.Fatalf("the retry after the crash has outcome %s, want replay", retry.Outcome())
	}
	var replayed map[string]string
	if err := json.Unmarshal(retry.Result(), &replayed); err != nil {
		t.Fatalf("the replayed result is not the JSON the enrolment stored: %v (%s)", err, retry.Result())
	}
	if replayed["agent_id"] != invitedID {
		t.Fatalf("the replayed result names agent %q, want %q", replayed["agent_id"], invitedID)
	}
	retry.Abort()

	// (5) And the floor sees the composite record's suffix.
	if got := igFloors(t, lg.Path())[igInvitedName]; got != 1 {
		t.Fatalf("the floor for %q is %d, want 1: a COMMITTED composite enrolment must be as visible to the suffix derivation as a plain one", igInvitedName, got)
	}
}

// ---------------------------------------------------------------------------
// PINNED: point C
// ---------------------------------------------------------------------------

// TestInviteGateCrashTornCommit is POINT C: the composite entry's commit frame
// was half written when the machine died.
//
// The prepare is on disk, whole and fsynced; the commit is not. PutWithInvite
// never returned, so no client was ever told, and recovery must repair the torn
// tail and leave BOTH halves unapplied — while the suffix, which did reach the
// platter, stays burned.
func TestInviteGateCrashTornCommit(t *testing.T) {
	dir := t.TempDir()
	minted := igSeed(t, dir)
	runIGCrashChild(t, igCrashTornCommit, dir, minted.ID, minted.Secret)

	// (1) The tail really IS torn. Without this the rest would pass just as
	// happily against a healthy file and would prove nothing.
	if _, err := wal.Replay(walPath(dir), nil); !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("Replay of the crashed log = %v, want ErrCorrupt: the child did not leave a torn frame, so this test is not exercising a torn commit", err)
	}

	// (2) Recovery repairs it and the bus starts (invariant 6).
	r, st, lg := igOpen(t, dir, nil, nil)
	defer lg.Close()

	igAssertControlSurvived(t, r)

	floors := igFloors(t, lg.Path())
	if got := floors[igInvitedName]; got != 1 {
		t.Fatalf("the floor for %q is %d, want 1: the composite PREPARE survived the repair, so its suffix is burned even though its commit was torn", igInvitedName, got)
	}

	igAssertNeitherHalfApplied(t, r, st, minted, igCrashTornCommit)
}

// ---------------------------------------------------------------------------
// PINNED: point D — and the LIMITATION it exists to pin
// ---------------------------------------------------------------------------

// TestInviteGateCrashTornPrepare is POINT D: the machine died while the
// COMPOSITE RECORD ITSELF was being written.
//
// Points A and C leave the composite prepare WHOLE on disk, so the log still
// carries both the agent id and the invite consumption and a scan can find them.
// This point removes that last copy: the torn frame IS the prepare.
//
// # What this pins, in order of importance
//
//  1. The bus STARTS (invariant 6). A half-written composite at the tail is
//     repaired and recovery reaches a running server; it never refuses to boot.
//  2. The discard is SPECIFIC, not silent: the repair report names the index and
//     the record TYPE. Silent discard is the P0, not discard.
//  3. Damage at the tail does not cascade BACKWARDS.
//  4. NEITHER half is applied, and the invite is STILL REDEEMABLE — the
//     fail-open direction, which is correct here because nothing was
//     acknowledged.
//  5. The index the torn record consumed is NOT reissued (invariant 1).
//  6. AND THE ONE THIS POINT EXISTS FOR: the suffix derivation comes back with
//     NO TRACE of the invited enrolment. READ THAT AS A LIMITATION BEING PINNED,
//     NEVER AS A CORRECTNESS PROPERTY — it is exactly the hole
//     internal/auth/crash_test.go's point D pins for a PLAIN enrolment, and the
//     composite record inherits it unchanged. What keeps the suffix burned on a
//     real bus is ids.OpenNameSuffixes, which persists each name's floor BEFORE
//     issuing it, in a file this tear cannot reach; this test builds no
//     allocator and its data directory holds no agent-suffixes file at all.
func TestInviteGateCrashTornPrepare(t *testing.T) {
	dir := t.TempDir()
	minted := igSeed(t, dir)
	runIGCrashChild(t, igCrashTornPrepare, dir, minted.ID, minted.Secret)

	// (1) The tail really IS torn, and torn in the PAYLOAD rather than merely
	// short: any other damage means the frame header did not survive, which is a
	// different failure from the one this point injects.
	_, err := wal.Replay(walPath(dir), nil)
	if !errors.Is(err, wal.ErrCorrupt) {
		t.Fatalf("Replay of the crashed log = %v, want ErrCorrupt: the child did not leave a torn frame", err)
	}
	if !strings.Contains(err.Error(), "truncated payload") {
		t.Fatalf("Replay reported %v; want a TRUNCATED PAYLOAD", err)
	}

	// (2) Before the repair the derivation FAILS TOTALLY rather than handing
	// back the half of the log it could read. A partial map here would be the
	// worst possible answer: it looks like an answer.
	if floors, err := auth.EnrolmentSuffixesInWAL(walPath(dir), testBusID); err == nil {
		t.Fatalf("EnrolmentSuffixesInWAL over the UNREPAIRED log returned %v with a nil error; a caller cannot tell an incomplete scan from a complete one", floors)
	} else if len(floors) != 0 {
		t.Fatalf("EnrolmentSuffixesInWAL returned a %d-entry map alongside its error, want none; failure is TOTAL", len(floors))
	}

	// (3) Recovery repairs it and the bus starts.
	var logBuf bytes.Buffer
	r, st, lg := igOpen(t, dir, logging.New(&logBuf, logging.LevelDebug), nil)
	defer lg.Close()

	rec := lg.Recovered()
	if !rec.Repaired.Truncated {
		t.Fatalf("recovery did not report truncating anything: %+v", rec.Repaired)
	}
	if n := len(rec.Repaired.Discards); n != 1 {
		t.Fatalf("recovery recorded %d discards, want exactly 1: %+v", n, rec.Repaired.Discards)
	}
	d := rec.Repaired.Discards[0]
	if !d.TypeKnown || d.Type != wal.TypePrepare {
		t.Fatalf("the discard is reported as type %v (known=%v), want a PREPARE: %+v", d.Type, d.TypeKnown, d)
	}
	if d.Reason == "" {
		t.Fatalf("the discard carries no reason: %+v\nA discard an operator cannot act on is the silent-discard defect wearing a struct", d)
	}
	// The control Put is one prepare and one commit; the mint is another pair;
	// so the composite prepare is the FIFTH record. The child computes it from
	// wal.Recovered().NextIndex and this asserts the two agree — a wrong guess
	// on either side would turn point D into a test of a frame nobody looks at.
	if d.Index != 5 {
		t.Fatalf("the discarded record carried index %d, want 5 (control prepare+commit, mint prepare+commit, then the composite prepare): %+v", d.Index, d)
	}

	// (4) The index the torn record consumed is never handed out again.
	if next := rec.NextIndex; next <= d.Index {
		t.Fatalf("recovery resumes at index %d, but index %d was already written to this file and then discarded; when recovery discards a record the sequence advances PAST the hole, it never rewinds into it (invariant 1)", next, d.Index)
	}

	// (5) Damage at the tail does not cascade backwards, and neither half of the
	// torn composite is applied.
	igAssertControlSurvived(t, r)

	// (6) THE POINT. See the doc comment: this asserts a LIMITATION.
	floors := igFloors(t, lg.Path())
	if got := floors[igControlName]; got != 1 {
		t.Fatalf("the floor for %q is %d, want 1: the scan must still work, or the absence below would prove nothing", igControlName, got)
	}
	if got, ok := floors[igInvitedName]; ok {
		t.Fatalf(`the scan reports a floor of %d for %q, want NO ENTRY AT ALL.

Want absent -- and that is not an aspiration, it is the limitation this point
exists to pin. The suffix WAS issued by this bus and the only record of it went
down with the torn prepare; nothing left in the log can name it. If this ever
reports a value, the scan has started reading bytes the repair discarded and the
whole file's corruption story needs re-reading.

What keeps that suffix burned is NOT this map. It is ids.OpenNameSuffixes, which
writes each name's floor BEFORE issuing it, in a file the tear never touched.
This test builds no allocator, so it pins the LIMITATION and leaves the
mitigation to internal/ids' own suite.`, got, igInvitedName)
	}

	igAssertNeitherHalfApplied(t, r, st, minted, igCrashTornPrepare)
}
