// IDEM-17's ACCEPTANCE EVIDENCE: a real kill -9 lands INSIDE the retry window,
// and the operation still has exactly ONE effect on the far side of it.
//
// # What "the retry window" means, and why it is a different axis to IDEM-11's
//
// internal/hub/idem_crash_test.go (IDEM-11's evidence) indexes its crash points
// by DURABLE STATE: nothing committed, a prepare with no commit, a commit. Those
// are the three states recovery can find, and it proves recovery reads each one
// correctly.
//
// This file indexes them by where the crash falls relative to the CLIENT's
// retry — which is the axis invariant 10 is actually about, because a duplicate
// is something a client does, not something a disk does:
//
//	|--- send issued ---|--- durable ---|--- ack sent ---|--- client retries ---|
//	         ^A                 ^B              ^C                   ^D
//
//	A  pre-commit           the prepare is fsynced, the commit never is.
//	B  post-commit-pre-ack  the commit is fsynced; nobody was ever told.
//	C  post-ack             the client HAS its result, retried once in-process,
//	                        and the bus died with the window still open.
//	D  mid-retry            the bus died while ANSWERING a post-restart retry.
//
// Each is a real SIGKILL to a re-exec'd child of this test binary, following
// internal/wal/crash_injection_test.go's established harness — never a Close()
// standing in for a crash, because a Close runs every deferred flush and so
// proves nothing about what was on stable storage at the instant of death.
//
// # THE PROPERTY THIS FILE EXISTS TO PROTECT, WHICH IS NOT "REJECT THE ATTACKER"
//
// Our abuse defence has aimed at the wrong party three times in this repo. So
// the load-bearing tests here are the ones that FAIL IF A WELL-BEHAVED CLIENT IS
// PUNISHED, not the ones that catch a malicious one:
//
//   - TestIdemCrashInjectionRestartHonestRetryIsNeverPunished replays the
//     client's request BYTE-FOR-BYTE — the ORIGINAL SignedMint included, with NO
//     re-mint — because that is what a real client holds after a dropped
//     connection. The mint table is in memory and does NOT survive a restart, so
//     if the reservation lookup ever moved ahead of the applied-key lookup, this
//     honest client would be answered ErrUnknownMint for a message that IS
//     durable. It replays three times: a "consume once" bug would pass a single
//     replay.
//   - TestIdemCrashInjectionRestartRetryStormIsAnsweredOnce runs that same
//     verbatim replay from 32 goroutines at once, under -race.
//   - TestIdemCrashInjectionRestartKeyReuseIsStillTheViolation is the other
//     side: after the same crash, the SAME key with a DIFFERENT payload must be
//     ErrIdempotencyKeyReused (reject+log only, narrowed 2026-08-08 — no
//     disconnect) and must NOT be reported as
//     the routine, retriable ErrUnknownMint — misfiling a violation as routine
//     aims the defence at nobody at all.
//
// # Why this lives in internal/idem and not next to IDEM-11's file
//
// File-ownership boundary: this task owns internal/idem/** and must not edit
// internal/hub/**. package idem_test is an EXTERNAL test package, so it may
// import internal/hub even though internal/hub imports internal/idem. The
// helpers below are therefore deliberate re-implementations of internal/hub's
// unexported test harness, not an attempt to improve on it.
package idem_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// The fixture. The child and the parent must agree on it byte-for-byte, or the
// parent would be exercising the VIOLATION path by accident and would still go
// green.
// ---------------------------------------------------------------------------

const (
	// envRestartCrashPoint selects where the child kills itself. Unset means
	// "not a crash child", which is what makes the child test a no-op skip in an
	// ordinary run of the package.
	envRestartCrashPoint = "IDEM_RESTART_CRASH_POINT"
	// envRestartCrashDir is the data directory the child works in: always a
	// t.TempDir() owned by the parent, so no run ever shares a bus data
	// directory with another and the tracked data/ dir is never touched.
	envRestartCrashDir = "IDEM_RESTART_CRASH_DIR"

	// crashPreCommit: the PREPARE carrying the applied-key record is fsynced and
	// the process dies with no COMMIT ever written. Nothing was accepted.
	crashPreCommit = "pre-commit"

	// crashPostCommitPreAck: the commit fsync returns and the process dies
	// before publish can apply anything to memory or return. The message is on
	// stable storage and no client knows it.
	crashPostCommitPreAck = "post-commit-pre-ack"

	// crashPostAck: the send SUCCEEDS, the client has its result, the client
	// then retries once IN-PROCESS (a retry answered from the live table, which
	// must write nothing), and only then does the process die. This is the
	// literal "mid-retry-window" crash: the window is open on both sides of it.
	crashPostAck = "post-ack"

	// crashBroadcastPostCommit: crashPostCommitPreAck for a BROADCAST rather
	// than a directed send. The op is part of the idempotency scope AND part of
	// the fingerprint, so it is a genuinely separate path through recovery, and
	// the task text asks for "send/broadcast at minimum".
	crashBroadcastPostCommit = "broadcast-post-commit-pre-ack"

	// crashMidRetry: the child opens a directory ANOTHER child already crashed,
	// recovers, is answered a replayed result for the original key, and dies the
	// instant it has that answer. It proves a replay is itself crash-safe — an
	// answer that quietly wrote something would leave a second effect behind.
	crashMidRetry = "mid-retry"
)

const (
	crashBusID         = "testbus"
	crashSenderName    = "alpha"
	crashRecipientName = "beta"
	crashKey           = "k-idem-17-retry-window"

	// crashTimestampMs is the SENDER-clock reading every fixture message
	// carries. store.NewMessage refuses 0 ("unset"), and one shared constant
	// keeps the child's message and the parent's replay from disagreeing.
	crashTimestampMs int64 = 1754130896789 // 2026-08-02T12:34:56.789Z
)

var (
	crashBody      = []byte("the acknowledgement that died in the retry window")
	crashOtherBody = []byte("a DIFFERENT payload reusing the same idempotency key")
)

// crashSignature is a well-formed 64-byte placeholder. The bus checks the LENGTH
// of a signature and never verifies it — it does not hold the sender's messaging
// key — so a constant is as good as a real one here and keeps a durable fixture
// reproducible byte for byte.
func crashSignature() []byte { return bytes.Repeat([]byte{0xAB}, signing.SignatureSize) }

// crashEnrolledAt is an hour in the past.
//
// It is NOT load-bearing for the assertions in this file as they stand — every
// query here is Store().Stats(), which is visibility-independent, so nothing
// would currently go vacuous if the epoch were wrong. It is set correctly
// anyway, and named, because store.Message.VisibleTo filters on the enrolment
// epoch: the moment a test here reads a message rather than counting one, a
// roster re-populated at time.Now() after a restart would silently return
// nothing and take the assertion with it. The child and the parent each compute
// it as "an hour ago", which is the honest simulation of the durable roster
// restoring each agent's ORIGINAL instant rather than the instant recovery ran.
func crashEnrolledAt() time.Time { return time.Now().Add(-time.Hour) }

// crashAgentID builds the fully-qualified "<bus-id>.<name>-1" (invariant 2).
func crashAgentID(t *testing.T, name string) string {
	t.Helper()
	id, err := ids.AgentID(crashBusID, name, 1)
	if err != nil {
		t.Fatalf("ids.AgentID(%q, %q, 1): %v", crashBusID, name, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// openCrashLog opens the durable log in dir. closeOnCleanup is ALWAYS false in a
// crash child: a deferred Close that ran would be exactly the graceful shutdown
// these tests exist to rule out.
func openCrashLog(t *testing.T, dir string, closeOnCleanup bool) *wal.Log {
	t.Helper()
	lg, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if closeOnCleanup {
		t.Cleanup(func() { _ = lg.Close() })
	}
	return lg
}

// openCrashHub builds a Hub over an already-open log, wiring replay the way main
// does — a read-only wal.Replay over the log's own path, because a second
// wal.Open on the same directory would be a second writer — and enrols both
// fixture agents at their original instant.
//
// durable is separated from lg so a crash point can wrap the write path while
// recovery still comes off the real file.
func openCrashHub(t *testing.T, durable hub.DurableLog, lg *wal.Log) *hub.Hub {
	t.Helper()
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID:     crashBusID,
		DataDir:   filepath.Dir(path),
		Durable:   durable,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		Roster:    roster,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	at := crashEnrolledAt()
	for _, name := range []string{crashSenderName, crashRecipientName} {
		roster.Add(hub.Agent{AgentID: crashAgentID(t, name), Name: name, EnrolledAt: at})
	}
	return h
}

// freshSendRequest is the client's FIRST attempt from the default fixture agent
// to the default fixture recipient under crashKey: a reservation is taken and
// the assignment it returns is what the client signs (SIGN-1's
// reserve-then-send).
//
// The mint-failure behaviour is freshSendRequestFrom's and is documented there.
func freshSendRequest(t *testing.T, h *hub.Hub, body []byte) hub.SendRequest {
	t.Helper()
	return freshSendRequestFrom(t, h, crashSenderName, crashRecipientName, crashKey, body)
}

// freshSendRequestFrom is freshSendRequest with the SENDER, the recipient and
// the key all chosen by the caller.
//
// It exists for the cross-agent isolation test, which needs a SECOND agent to
// present the FIRST agent's key. The scope is the (agent, op, key) tuple, so
// "the same key from a different agent" is only expressible if the sender is a
// parameter — and holding the key constant while varying the agent is exactly
// what makes a cross-agent oracle detectable.
//
// The mint is taken under the given sender, not merely claimed: a reservation
// belongs to the agent that took it, so borrowing another agent's assignment
// would be refused for that reason and would prove nothing about the
// applied-key scope.
//
// A mint failure is deliberately not fatal — a caller may be exercising a path
// that is supposed to refuse — so a refused reservation yields the zero
// SignedMint and lets the publish return the error the client would see.
func freshSendRequestFrom(t *testing.T, h *hub.Hub, fromName, toName, key string, body []byte) hub.SendRequest {
	t.Helper()
	sender := crashAgentID(t, fromName)
	req := hub.SendRequest{
		Sender:         sender,
		To:             crashAgentID(t, toName),
		Body:           body,
		IdempotencyKey: key,
	}
	if m, err := h.Mint(hub.MintRequest{Sender: sender, Op: "send", IdempotencyKey: key}); err == nil {
		req.SignedMint = hub.SignedMint{
			MessageID:          m.MessageID,
			Seq:                m.Seq,
			TimestampUnixMilli: crashTimestampMs,
			Signature:          crashSignature(),
		}
	}
	return req
}

// freshBroadcastRequest is freshSendRequest for a broadcast. The two are
// separate rather than one function with a flag because Send and Broadcast take
// different request types, and because the OP is part of the reservation's
// scope — minting under "send" and spending under "broadcast" is a mismatch,
// not a detail.
//
// It reuses crashKey DELIBERATELY: the scope is the (agent, op, key) tuple, so
// the same key under a different op must be a different operation, and proving
// that survives a crash is part of what this path is for.
func freshBroadcastRequest(t *testing.T, h *hub.Hub, body []byte) hub.BroadcastRequest {
	t.Helper()
	sender := crashAgentID(t, crashSenderName)
	req := hub.BroadcastRequest{
		Sender:         sender,
		Body:           body,
		IdempotencyKey: crashKey,
	}
	if m, err := h.Mint(hub.MintRequest{Sender: sender, Op: "broadcast", IdempotencyKey: crashKey}); err == nil {
		req.SignedMint = hub.SignedMint{
			MessageID:          m.MessageID,
			Seq:                m.Seq,
			TimestampUnixMilli: crashTimestampMs,
			Signature:          crashSignature(),
		}
	}
	return req
}

// verbatimBroadcastReplayOf is verbatimReplayOf for a broadcast. A broadcast is
// stored as a FLAG rather than an expanded roster snapshot, so it has no
// recipient list to rebuild the request from — which is exactly why it needs its
// own reconstruction rather than sharing the directed one.
func verbatimBroadcastReplayOf(m store.Message, body []byte) hub.BroadcastRequest {
	return hub.BroadcastRequest{
		Sender:         m.Sender,
		Body:           body,
		IdempotencyKey: m.IdempotencyKey,
		SignedMint: hub.SignedMint{
			MessageID:          m.ID,
			Seq:                m.Seq,
			TimestampUnixMilli: m.TimestampUnixMilli,
			Signature:          m.Signature,
		},
	}
}

// verbatimReplayOf reconstructs the EXACT request the client was holding when
// the bus died, from the message the dying process left on stable storage.
//
// This is the whole point of the honest-client tests: a real client that saw a
// dropped connection re-sends the bytes it already has — the same key, the same
// body and THE SAME SIGNED ASSIGNMENT — it does not go and ask for a new
// reservation first. body is a parameter only so the violation test can keep the
// mint identical while changing the payload.
func verbatimReplayOf(m store.Message, body []byte) hub.SendRequest {
	return hub.SendRequest{
		Sender:         m.Sender,
		To:             m.Recipients[0],
		Body:           body,
		IdempotencyKey: m.IdempotencyKey,
		SignedMint: hub.SignedMint{
			MessageID:          m.ID,
			Seq:                m.Seq,
			TimestampUnixMilli: m.TimestampUnixMilli,
			Signature:          m.Signature,
		},
	}
}

// ---------------------------------------------------------------------------
// The kill points
// ---------------------------------------------------------------------------

// killAfterCommit delegates to the REAL *wal.Log.Write — the whole prepare,
// commit and fsync cycle — and kills the process before returning, so publish
// never applies the message to memory, never remembers the key, never wakes a
// waiter and never returns a Result.
type killAfterCommit struct{ l *wal.Log }

func (k *killAfterCommit) Write(e wal.Entry) (wal.Committed, error) {
	if passThroughSeqFloorRecord(e) {
		return k.l.Write(e)
	}
	if len(e.Idem) == 0 {
		return wal.Committed{}, fmt.Errorf("child: the entry publish handed to the durable log carries NO applied-key record; invariant 10 requires it to ride in the SAME two-phase transaction as the effect, and this crash point is meaningless without it")
	}
	c, err := k.l.Write(e)
	if err != nil {
		return wal.Committed{}, err
	}
	killSelfNow()
	return c, nil
}

// killAfterPrepare stops the two-phase write between its two fsyncs: the PREPARE
// — carrying the applied-key record publish built — is durable and complete, and
// no COMMIT record is ever written. The transaction is deliberately never
// resolved; nothing runs after the kill, so there is nothing to resolve it for.
type killAfterPrepare struct{ l *wal.Log }

func (k *killAfterPrepare) Write(e wal.Entry) (wal.Committed, error) {
	if passThroughSeqFloorRecord(e) {
		return k.l.Write(e)
	}
	if len(e.Idem) == 0 {
		return wal.Committed{}, fmt.Errorf("child: the entry publish handed to the durable log carries NO applied-key record; this crash point exists to prove an UNCOMMITTED one is not recovered, so it has to be there to begin with")
	}
	if _, err := k.l.Begin(e); err != nil {
		return wal.Committed{}, err
	}
	// Begin returns only once the PREPARE record is fsynced.
	killSelfNow()
	return wal.Committed{}, nil
}

// passThroughSeqFloorRecord reports that e is a SEQUENCE-FLOOR record and must
// be written for real, without the assertion or the kill.
//
// Since SIGN-1's reserve-then-send a send is TWO durable writes and only the
// second is the message: Hub.Mint burns a batch of sequence numbers ahead before
// it hands one to the client, and that record legitimately carries no
// applied-key record — it is not an operation a client performed. Dropping this
// guard would be a silent way to stop injecting the intended crash at all: the
// "no applied-key record" assertion would fire on the floor record and turn the
// child into a FAILED MINT (so the send would be refused with ErrUnknownMint and
// the crash would never happen), and killing on it would crash at the MINT
// rather than at the message write.
func passThroughSeqFloorRecord(e wal.Entry) bool { return e.Kind == hub.SeqFloorRecordKind }

// exitBroadcastUnsignable is the child's exit code meaning "a broadcast was
// refused because signing format v1 cannot canonicalize one" (SIGN-3).
//
// It exists because this harness's signal is a WAIT STATUS, not a message: the
// parent asserts the child died on SIGKILL, and every ordinary child failure is
// exit status 1. A refused broadcast is neither — it is a test that CANNOT RUN
// yet — and status 1 is indistinguishable from the crash genuinely failing to
// inject, which is a real defect this harness must keep catching.
//
// 42 is chosen simply because `go test` uses 1 and the shell reserves 2 and
// 126-165; nothing else in this tree exits 42.
const exitBroadcastUnsignable = 42

// exitIfBroadcastHasNoSigningDigest exits the CHILD with exitBroadcastUnsignable
// when err is the SIGN-3 refusal, so the parent can skip.
//
// # Why the child exits instead of the parent skipping up front
//
// This is the same mechanism internal/hub uses (skipIfBroadcastHasNoSigningDigest
// in hub_test.go) reached across a process boundary, and it is chosen for the
// same two reasons. It is EXACT: it fires only on signing.ErrInvalid, so a
// broadcast crash-injection failing for any other reason — including the crash
// genuinely not injecting — still fails loudly. And it is SELF-HEALING: the day
// SIGN-3 lands, the broadcast reaches the durable write, the kill wrapper fires,
// the child dies on SIGKILL and this test runs again WITH NO EDIT.
//
// A literal t.Skip at the top of the parent test would need a human to find and
// remove it, and a skip nobody removes silently un-covers a path everyone
// believes is tested.
//
// os.Exit is safe here precisely because of what this harness is: the child is
// built to be SIGKILLed, so it already runs no deferred cleanup on its intended
// path, and openCrashLog is opened with closeOnCleanup=false throughout.
func exitIfBroadcastHasNoSigningDigest(t *testing.T, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, signing.ErrInvalid) {
		return
	}
	// Written to the child's captured output so the parent's skip message can
	// carry the real refusal rather than only an exit code.
	fmt.Printf("child: broadcast refused, no canonical digest (SIGN-3): %v\n", err)
	os.Exit(exitBroadcastUnsignable)
}

// killSelfNow kills this process with SIGKILL. SIGKILL cannot be caught, blocked
// or ignored, so nothing deferred, buffered or graceful runs afterwards — which
// is the entire evidentiary value of these tests over a polite Close.
func killSelfNow() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable: the signal is delivered before Kill returns. Panicking rather
	// than looping means a platform where that is somehow untrue fails loudly
	// instead of hanging the suite.
	panic("idem crash test: SIGKILL to self did not kill the process")
}

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestIdemCrashInjectionRestartChild is the child half of every test below. It
// does NOTHING in an ordinary run: without envRestartCrashPoint it skips
// immediately.
func TestIdemCrashInjectionRestartChild(t *testing.T) {
	point := os.Getenv(envRestartCrashPoint)
	if point == "" {
		t.Skip("not a crash child: " + envRestartCrashPoint + " is unset")
	}
	dir := os.Getenv(envRestartCrashDir)
	if dir == "" {
		t.Fatalf("child: %s=%q but %s is empty", envRestartCrashPoint, point, envRestartCrashDir)
	}

	// closeOnCleanup is false throughout: see openCrashLog.
	lg := openCrashLog(t, dir, false)

	switch point {
	case crashPreCommit:
		h := openCrashHub(t, &killAfterPrepare{l: lg}, lg)
		res, err := h.Send(freshSendRequest(t, h, crashBody))
		t.Fatalf("child: Send returned (%+v, %v) but the durable log kills this process the instant the PREPARE is fsynced; the crash was never injected", res, err)

	case crashPostCommitPreAck:
		h := openCrashHub(t, &killAfterCommit{l: lg}, lg)
		res, err := h.Send(freshSendRequest(t, h, crashBody))
		t.Fatalf("child: Send returned (%+v, %v) but the durable log kills this process the instant the COMMIT is fsynced; the crash was never injected", res, err)

	case crashBroadcastPostCommit:
		h := openCrashHub(t, &killAfterCommit{l: lg}, lg)
		res, err := h.Broadcast(freshBroadcastRequest(t, h, crashBody))
		// SIGN-3: the broadcast never reached the durable write, so the kill
		// wrapper was never armed and this child cannot die as designed. Report
		// it to the PARENT as a distinguished exit code rather than as a test
		// failure — see exitBroadcastUnsignable.
		exitIfBroadcastHasNoSigningDigest(t, err)
		t.Fatalf("child: Broadcast returned (%+v, %v) but the durable log kills this process the instant the COMMIT is fsynced; the crash was never injected", res, err)

	case crashPostAck:
		// No kill wrapper: this child completes an honest, fully acknowledged
		// send over the real log, and dies later.
		h := openCrashHub(t, lg, lg)
		req := freshSendRequest(t, h, crashBody)
		res, err := h.Send(req)
		if err != nil {
			t.Fatalf("child: the first send failed: %v", err)
		}
		if res.Replayed {
			t.Fatalf("child: the FIRST send reported Replayed = true; the applied-key table was not empty on a fresh log")
		}
		// The client has its ack, and retries anyway — a relay redelivering, or
		// a wrapper that retries on a connection reset after the response was
		// already on the wire. Replayed VERBATIM: same key, same body, same
		// signed assignment, no new reservation.
		again, err := h.Send(verbatimReplayOf(mustDecodeOnly(t, dir), crashBody))
		if err != nil {
			t.Fatalf("child: the in-process retry of an ACKNOWLEDGED send returned %v, want the original result; a legitimate retry must never error", err)
		}
		if !again.Replayed || again.MessageID != res.MessageID || again.Seq != res.Seq {
			t.Fatalf("child: the in-process retry returned %+v, want the original %s / seq %d replayed", again, res.MessageID, res.Seq)
		}
		// The retry window is open and the bus dies inside it.
		killSelfNow()

	case crashMidRetry:
		// dir already holds a log ANOTHER child crashed with exactly one
		// committed message. Recover it and answer the retry.
		h := openCrashHub(t, lg, lg)
		again, err := h.Send(verbatimReplayOf(mustDecodeOnly(t, dir), crashBody))
		if err != nil {
			t.Fatalf("child: the post-restart retry returned %v, want the original result replayed", err)
		}
		if !again.Replayed {
			t.Fatalf("child: the post-restart retry returned Replayed = false with message %s; the recovered applied-key table did not recognise the key", again.MessageID)
		}
		// Dies holding an answer nobody received. The NEXT recovery must still
		// find exactly one effect.
		killSelfNow()

	default:
		t.Fatalf("child: unknown crash point %q", point)
	}
	t.Fatalf("child: still running after SIGKILL")
}

// mustDecodeOnly reads the single committed message off the log in dir. It is
// how the child (and the parent) learn the exact assignment the original client
// was holding, without either of them taking the other's word for it.
func mustDecodeOnly(t *testing.T, dir string) store.Message {
	t.Helper()
	msgs := committedMessagesIn(t, dir)
	if len(msgs) != 1 {
		t.Fatalf("the durable log in %s holds %d committed MESSAGE entries, want exactly 1", dir, len(msgs))
	}
	return msgs[0]
}

// ---------------------------------------------------------------------------
// Reading what the dying process actually left behind
// ---------------------------------------------------------------------------

// replayRaw reads the committed history straight off the log, read-only, without
// opening a writer on it. It is how the parent learns what reached stable
// storage rather than what the process believed it had written.
func replayRaw(t *testing.T, dir string) []wal.Committed {
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

// messageEntries narrows a replayed history to the MESSAGE transactions.
//
// Since SIGN-1 a send costs TWO durable transactions and only one of them is the
// message: Hub.Mint commits a sequence-floor record burning a batch of numbers
// ahead. A raw count of committed entries therefore does not answer "how many
// effects are there", and counting a floor record as an effect would let a crash
// at the wrong moment look like a successful send.
func messageEntries(committed []wal.Committed) []wal.Committed {
	var out []wal.Committed
	for _, c := range committed {
		if c.Entry.Kind == store.RecordKind {
			out = append(out, c)
		}
	}
	return out
}

// seqFloorEntries is messageEntries' counterpart: the sequence-floor records a
// mint wrote.
func seqFloorEntries(committed []wal.Committed) []wal.Committed {
	var out []wal.Committed
	for _, c := range committed {
		if c.Entry.Kind == hub.SeqFloorRecordKind {
			out = append(out, c)
		}
	}
	return out
}

// committedMessagesIn decodes every committed message record in dir.
func committedMessagesIn(t *testing.T, dir string) []store.Message {
	t.Helper()
	var out []store.Message
	for _, c := range messageEntries(replayRaw(t, dir)) {
		m, err := store.Decode(c.Entry.Body)
		if err != nil {
			t.Fatalf("decoding a committed message record in %s: %v", dir, err)
		}
		out = append(out, m)
	}
	return out
}

// storedCount reports what the hub's serving copy holds and where its head sits.
func storedCount(t *testing.T, h *hub.Hub) (int, uint64) {
	t.Helper()
	count, _, _, head, _ := h.Store().Stats()
	return count, head
}

// runRestartCrashChild re-execs this test binary at the given crash point and
// asserts the child really DIED ON SIGKILL rather than failing its own
// assertions. Without that check a broken child would silently turn the parent
// into a test of an empty directory — which passes.
func runRestartCrashChild(t *testing.T, point, dir string) {
	t.Helper()

	// No os.Args[0] fallback. If os.Args[0] carried no path separator,
	// exec.Command would resolve it through PATH and could re-exec a DIFFERENT
	// binary — which would then be handed a SIGKILL crash point and a data
	// directory. Failing is the only safe answer, and it is unreachable on Linux
	// where os.Executable reads /proc/self/exe.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v; refusing to fall back to os.Args[0], which exec.Command would resolve through PATH", err)
	}

	// Bounded, so a wedged child fails this test in a minute rather than hanging
	// the suite until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestIdemCrashInjectionRestartChild$", "-test.v")
	cmd.Env = append(os.Environ(), envRestartCrashPoint+"="+point, envRestartCrashDir+"="+dir)
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
	// SIGN-3, checked BEFORE the SIGKILL assertion: the child refused the
	// broadcast before the durable write, so the kill wrapper was never armed
	// and there is no crash to assert on. This is a test that cannot run yet,
	// not a harness that stopped working — and the distinguished exit code is
	// what keeps the two apart (see exitBroadcastUnsignable).
	if !ws.Signaled() && ws.ExitStatus() == exitBroadcastUnsignable {
		t.Skipf("SKIPPED pending SIGN-3: signing format v1 has not defined a canonical broadcast audience, "+
			"so a broadcast has no signing digest and cannot produce the audit-log content hash DUR-5 requires "+
			"(PROTOCOL.md 8.6); internal/hub fails closed rather than inventing one, so the broadcast never "+
			"reaches the durable write and the crash cannot be injected. /v1/broadcast answers 501 today, so "+
			"nothing in production is affected. UN-SKIP WHEN SIGN-3 LANDS -- this test needs no change, it will "+
			"simply start running again.\n--- child output ---\n%s", out.String())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child %q exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s",
			point, ws.ExitStatus(), out.String())
	}
}

// ---------------------------------------------------------------------------
// The table: a crash ANYWHERE in the retry window yields exactly one effect
// ---------------------------------------------------------------------------

// TestIdemCrashInjectionRestartYieldsExactlyOneEffect kills the bus at three
// points across the retry window and asserts the same thing after every one of
// them: when the dust settles the client's operation has happened EXACTLY ONCE —
// one committed message on disk, one message in the serving copy, one applied
// key in the recovered table.
//
// The rows differ only in what the honest client's next move is, and that
// difference is itself the property being pinned:
//
//   - After a PRE-COMMIT crash nothing was accepted, so a verbatim replay is
//     answered ErrUnknownMint — which is documented as ROUTINE and retriable,
//     with the remedy "re-mint under the same key and re-send". It must NOT be
//     ErrIdempotencyKeyReused: the client did nothing wrong and must not be
//     told it committed a protocol violation for a crash on our side.
//   - After a POST-COMMIT or POST-ACK crash the operation IS part of accepted
//     history, so the same verbatim replay must return the ORIGINAL result with
//     no error and no re-mint.
func TestIdemCrashInjectionRestartYieldsExactlyOneEffect(t *testing.T) {
	tests := []struct {
		name string
		// point is the crash injected into the child.
		point string
		// wantCommittedAfterCrash is how many MESSAGE transactions the dying
		// child left on stable storage.
		wantCommittedAfterCrash int
		// wantKeysAfterRecovery is how many applied-key records replay rebuilds.
		wantKeysAfterRecovery int
		// replayIsAnswered says whether a VERBATIM replay (no re-mint) is
		// answered from the recovered applied-key table.
		replayIsAnswered bool
	}{
		{
			name:                    "killed between the prepare and the commit: nothing was accepted",
			point:                   crashPreCommit,
			wantCommittedAfterCrash: 0,
			wantKeysAfterRecovery:   0,
			replayIsAnswered:        false,
		},
		{
			name:                    "killed after the commit fsync, before the ack: accepted, never acknowledged",
			point:                   crashPostCommitPreAck,
			wantCommittedAfterCrash: 1,
			wantKeysAfterRecovery:   1,
			replayIsAnswered:        true,
		},
		{
			name:                    "killed after the ack, with an in-process retry already answered",
			point:                   crashPostAck,
			wantCommittedAfterCrash: 1,
			wantKeysAfterRecovery:   1,
			replayIsAnswered:        true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runRestartCrashChild(t, tc.point, dir)

			// (0) What the dying process ACTUALLY left on stable storage.
			// Without this the rest could pass just as happily against an empty
			// directory and would prove nothing.
			all := replayRaw(t, dir)
			if len(seqFloorEntries(all)) == 0 {
				t.Fatalf("the crashed log holds no sequence-floor record among its %d committed entries: the child's mint handed out a sequence without first durably burning it, so a restart could reissue that message id (invariant 1)", len(all))
			}
			msgs := messageEntries(all)
			if len(msgs) != tc.wantCommittedAfterCrash {
				t.Fatalf("the crashed log holds %d committed MESSAGE entries, want %d: the crash did not land where this row says it did, so nothing below is testing what it claims", len(msgs), tc.wantCommittedAfterCrash)
			}
			for i, c := range msgs {
				if len(c.Entry.Idem) == 0 {
					t.Fatalf("committed message transaction %d carries NO applied-key record (Entry.Idem is empty): invariant 10's load-bearing requirement is that the record commits in the SAME two-phase transaction as the effect, not in a second write ordered after it", i)
				}
			}

			// --- RESTART on the SAME directory. ---
			lg := openCrashLog(t, dir, true)
			h := openCrashHub(t, lg, lg)

			if got := h.IdempotencyStats().Count; got != tc.wantKeysAfterRecovery {
				t.Fatalf("after recovering from a SIGKILL the applied-key table holds %d records, want %d: the table must be REBUILT FROM THE DURABLE LOG and not held in memory — a process killed with -9 flushes nothing", got, tc.wantKeysAfterRecovery)
			}
			if count, _ := storedCount(t, h); count != tc.wantCommittedAfterCrash {
				t.Fatalf("the recovered serving copy holds %d messages, want %d: memory must be rebuilt to exactly what disk accepted (invariant 5)", count, tc.wantCommittedAfterCrash)
			}

			// (1) THE CLIENT'S NEXT MOVE: it replays the bytes it is holding.
			if tc.replayIsAnswered {
				original := mustDecodeOnly(t, dir)
				again, err := h.Send(verbatimReplayOf(original, crashBody))
				if err != nil {
					t.Fatalf("the verbatim replay after the crash returned err = %v, want the ORIGINAL result and no error: the message IS durable, this is a legitimate retry of a lost acknowledgement, and it must neither error nor disconnect (invariant 10's legitimate-retry carve-out)", err)
				}
				if !again.Replayed {
					t.Errorf("the replay returned Replayed = false, want true: the caller must be able to tell that NOTHING was re-applied")
				}
				if again.MessageID != original.ID || again.Seq != original.Seq {
					t.Errorf("the replay returned message %s / seq %d, want the original %s / seq %d", again.MessageID, again.Seq, original.ID, original.Seq)
				}
				if !again.SentAt.Equal(original.SentAt) {
					t.Errorf("the replay returned sent_at %s, want the original %s: the stored result must be returned verbatim, not re-minted", again.SentAt.Format(time.RFC3339Nano), original.SentAt.Format(time.RFC3339Nano))
				}
			} else {
				// Nothing was accepted, so there is nothing to replay from and
				// no message to reconstruct the assignment from. The client's
				// held reservation died with the in-memory mint table.
				_, err := h.Send(hub.SendRequest{
					Sender:         crashAgentID(t, crashSenderName),
					To:             crashAgentID(t, crashRecipientName),
					Body:           crashBody,
					IdempotencyKey: crashKey,
					SignedMint: hub.SignedMint{
						MessageID:          "testbus-1",
						Seq:                1,
						TimestampUnixMilli: crashTimestampMs,
						Signature:          crashSignature(),
					},
				})
				if !errors.Is(err, hub.ErrUnknownMint) {
					t.Fatalf("replaying a reservation that died with the process returned err = %v, want ErrUnknownMint: the reservation table is in memory and does not survive a restart, and the documented remedy is to re-mint under the SAME key", err)
				}
				if errors.Is(err, hub.ErrIdempotencyKeyReused) {
					t.Fatalf("a client whose send NEVER COMMITTED was told its idempotency key was reused (%v); that is the protocol-violation path, aimed at a client that did nothing wrong", err)
				}
				// The remedy the error names must actually work, first time.
				// The reservation is checked explicitly because freshSendRequest
				// deliberately swallows a refused Mint (it yields the zero
				// SignedMint so a caller can observe the PUBLISH's error) — and
				// without this check a refused re-mint would surface below as a
				// misleading "want success" against the send.
				remedy := freshSendRequest(t, h, crashBody)
				if remedy.SignedMint.MessageID == "" {
					t.Fatalf("re-minting under the same key after a pre-commit crash was REFUSED; ErrUnknownMint documents that remedy as routine, so it has to be available")
				}
				res, err := h.Send(remedy)
				if err != nil {
					t.Fatalf("re-minting under the same key after a pre-commit crash returned err = %v, want success: nothing was ever committed under this key", err)
				}
				if res.Replayed {
					t.Fatalf("the genuine first attempt after a pre-commit crash returned Replayed = true with message %s: a key whose effect NEVER COMMITTED was remembered as applied, so a real first attempt was silently swallowed (invariant 5)", res.MessageID)
				}
			}

			// (2) EXACTLY ONE EFFECT, whichever crash point was hit. The durable
			// log, the serving copy and the applied-key table all have to agree.
			if got := len(committedMessagesIn(t, dir)); got != 1 {
				t.Fatalf("the durable log holds %d committed MESSAGE entries once the retry window has closed, want exactly 1: the client's single operation left %d effects on stable storage", got, got)
			}
			count, head := storedCount(t, h)
			if count != 1 {
				t.Fatalf("the serving copy holds %d messages once the retry window has closed, want exactly 1", count)
			}
			if head != committedMessagesIn(t, dir)[0].Seq {
				t.Fatalf("the serving copy's head is %d, want %d: memory and disk disagree about the accepted history", head, committedMessagesIn(t, dir)[0].Seq)
			}
			if got := h.IdempotencyStats().Count; got != 1 {
				t.Fatalf("the applied-key table holds %d records once the retry window has closed, want exactly 1", got)
			}
			if err := h.Poisoned(); err != nil {
				t.Fatalf("the hub is poisoned after the retry window closed: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The honest client must not be punished
// ---------------------------------------------------------------------------

// TestIdemCrashInjectionRestartHonestRetryIsNeverPunished is the test this task
// exists to add, and it is deliberately written to FAIL IF WE PUNISH THE CLIENT
// DOING THE RIGHT THING rather than to catch one doing the wrong thing.
//
// The scenario is the ordinary one: the message committed, the process died
// before the ack, and the client re-sends the bytes it already has — the same
// key, the same body and THE SAME SIGNED ASSIGNMENT. It does NOT go and take a
// new reservation first, because a real client has no reason to think it needs
// one and, under SIGN-1, taking one would give it a different message id to sign
// and invalidate the signature it already computed.
//
// The reservation table is IN MEMORY and does not survive a restart. So if the
// mint lookup ever moved ahead of the applied-key lookup in publish, this honest
// client would be told ErrUnknownMint about a message that is sitting durable on
// disk — the third time in this repo that a defence would have been pointed at
// the wrong party.
//
// It replays THREE times. A single replay passes even if the answer is consumed
// on first use; three do not.
func TestIdemCrashInjectionRestartHonestRetryIsNeverPunished(t *testing.T) {
	dir := t.TempDir()
	runRestartCrashChild(t, crashPostCommitPreAck, dir)

	original := mustDecodeOnly(t, dir)

	lg := openCrashLog(t, dir, true)
	h := openCrashHub(t, lg, lg)

	for attempt := 1; attempt <= 3; attempt++ {
		again, err := h.Send(verbatimReplayOf(original, crashBody))
		// The two named misdiagnoses are checked BEFORE the general case, so a
		// failure says which wrong party the defence was aimed at rather than
		// just printing an error string.
		if errors.Is(err, hub.ErrUnknownMint) {
			t.Fatalf("replay %d was refused with ErrUnknownMint: the applied-key lookup must run BEFORE the reservation lookup, or every honest retry across a restart is refused for a message that IS durable", attempt)
		}
		if errors.Is(err, hub.ErrIdempotencyKeyReused) {
			t.Fatalf("replay %d of an IDENTICAL payload was treated as a key-reuse violation: that is the reject-and-log path and it is aimed at the wrong party", attempt)
		}
		if err != nil {
			t.Fatalf("replay %d of %d returned err = %v, want the original result: a well-behaved client retrying a lost acknowledgement must be answered, not refused", attempt, 3, err)
		}
		if !again.Replayed {
			t.Fatalf("replay %d returned Replayed = false: the caller cannot tell that nothing was re-applied", attempt)
		}
		if again.MessageID != original.ID || again.Seq != original.Seq || !again.SentAt.Equal(original.SentAt) {
			t.Fatalf("replay %d returned %s / seq %d / sent_at %s, want the original %s / seq %d / %s returned verbatim",
				attempt, again.MessageID, again.Seq, again.SentAt.Format(time.RFC3339Nano),
				original.ID, original.Seq, original.SentAt.Format(time.RFC3339Nano))
		}
		if again.Sender != original.Sender || again.Broadcast != original.Broadcast {
			t.Fatalf("replay %d returned sender %q / broadcast %v, want %q / %v: the scope-derived fields did not survive recovery",
				attempt, again.Sender, again.Broadcast, original.Sender, original.Broadcast)
		}

		if got := len(committedMessagesIn(t, dir)); got != 1 {
			t.Fatalf("after replay %d the durable log holds %d committed MESSAGE entries, want 1: a replay wrote a second effect", attempt, got)
		}
		if count, _ := storedCount(t, h); count != 1 {
			t.Fatalf("after replay %d the serving copy holds %d messages, want 1", attempt, count)
		}
		if got := h.IdempotencyStats().Count; got != 1 {
			t.Fatalf("after replay %d the applied-key table holds %d records, want 1: the record must not be consumed by being read", attempt, got)
		}
	}
}

// TestIdemCrashInjectionRestartRetryStormIsAnsweredOnce is the honest-retry
// property under concurrency, which is where a data race would live: 32
// goroutines replay the SAME lost acknowledgement at once, immediately after
// recovery, with no re-mint.
//
// Every one must get the original result. Not one may error, and not one may
// produce an effect. Run with -race, this is also the concurrency check for the
// recovered applied-key table itself.
func TestIdemCrashInjectionRestartRetryStormIsAnsweredOnce(t *testing.T) {
	dir := t.TempDir()
	runRestartCrashChild(t, crashPostCommitPreAck, dir)

	original := mustDecodeOnly(t, dir)

	lg := openCrashLog(t, dir, true)
	h := openCrashHub(t, lg, lg)

	const retries = 32
	type outcome struct {
		res hub.Result
		err error
	}
	results := make([]outcome, retries)

	// A start barrier, so the retries genuinely overlap instead of trickling
	// through the write lock one at a time while the earlier ones have finished.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < retries; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := h.Send(verbatimReplayOf(original, crashBody))
			results[i] = outcome{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, got := range results {
		if got.err != nil {
			t.Fatalf("retry %d of %d in the storm returned err = %v, want the original result: every one of these is the SAME legitimate retry of a lost acknowledgement", i, retries, got.err)
		}
		if !got.res.Replayed {
			t.Fatalf("retry %d returned Replayed = false: exactly one of these could have been a fresh apply, and none of them may be — the message was already durable before the process died", i)
		}
		if got.res.MessageID != original.ID || got.res.Seq != original.Seq {
			t.Fatalf("retry %d returned message %s / seq %d, want the original %s / seq %d", i, got.res.MessageID, got.res.Seq, original.ID, original.Seq)
		}
	}

	if got := len(committedMessagesIn(t, dir)); got != 1 {
		t.Fatalf("after %d concurrent retries the durable log holds %d committed MESSAGE entries, want exactly 1", retries, got)
	}
	if count, head := storedCount(t, h); count != 1 || head != original.Seq {
		t.Fatalf("after %d concurrent retries the serving copy holds %d messages with head %d, want 1 at seq %d", retries, count, head, original.Seq)
	}
	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after %d concurrent retries the applied-key table holds %d records, want exactly 1", retries, got)
	}
	if err := h.Poisoned(); err != nil {
		t.Fatalf("the hub is poisoned after a retry storm of legitimate retries: %v", err)
	}
}

// TestIdemCrashInjectionRestartRemintingClientStillGetsOneEffect is the test
// that catches the DOUBLE-APPLY directly, and it exists because the obvious
// tests do not.
//
// Under SIGN-1's reserve-then-send the mint table is also in memory, so if the
// applied-key table stopped being recovered the visible symptom would be a
// verbatim replay refused with ErrUnknownMint — a refusal, not a duplicate. The
// duplicate only appears one step later, when the client does exactly what that
// error tells it to do: re-mint under the SAME key and re-send. A suite that
// only ever replays verbatim would report the wrong failure and would never
// observe the second message at all.
//
// So this models the client that ALWAYS re-mints on reconnect, which is a
// perfectly reasonable wrapper to write. It must still get ONE effect, and it
// must be answered with the ORIGINAL assignment — not with the fresh one it just
// reserved, which would hand it a message id that no message on this bus bears.
func TestIdemCrashInjectionRestartRemintingClientStillGetsOneEffect(t *testing.T) {
	dir := t.TempDir()
	runRestartCrashChild(t, crashPostCommitPreAck, dir)

	original := mustDecodeOnly(t, dir)

	lg := openCrashLog(t, dir, true)
	h := openCrashHub(t, lg, lg)

	// The client reconnects, takes a FRESH reservation under the same key, signs
	// it, and re-sends. freshSendRequest does exactly that.
	req := freshSendRequest(t, h, crashBody)
	if req.SignedMint.MessageID == "" {
		t.Fatalf("re-minting under the same key after a restart was refused; the documented remedy for ErrUnknownMint has to work")
	}
	if req.SignedMint.Seq == original.Seq {
		t.Fatalf("the re-mint returned the ORIGINAL sequence %d; a sequence durably burned before the crash must never be handed out again (invariant 1)", original.Seq)
	}

	again, err := h.Send(req)
	if err != nil {
		t.Fatalf("re-sending under the same key with a fresh reservation returned err = %v, want the ORIGINAL result: the key was already applied, so this is still a legitimate retry", err)
	}
	if !again.Replayed {
		t.Fatalf("re-sending under the same key with a fresh reservation returned Replayed = false and minted message %s: the operation has now been APPLIED TWICE, which is the exact duplicate invariant 10 forbids", again.MessageID)
	}
	if again.MessageID != original.ID || again.Seq != original.Seq {
		t.Fatalf("the answer was message %s / seq %d, want the ORIGINAL %s / seq %d: answering with the freshly-minted assignment names a message that does not exist on this bus",
			again.MessageID, again.Seq, original.ID, original.Seq)
	}

	if got := len(committedMessagesIn(t, dir)); got != 1 {
		t.Fatalf("the durable log holds %d committed MESSAGE entries after a re-minting retry, want exactly 1: the operation was applied twice", got)
	}
	if count, head := storedCount(t, h); count != 1 || head != original.Seq {
		t.Fatalf("the serving copy holds %d messages with head %d after a re-minting retry, want 1 at seq %d", count, head, original.Seq)
	}
	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("the applied-key table holds %d records after a re-minting retry, want exactly 1", got)
	}
}

// TestIdemCrashInjectionRestartBroadcastRetryIsAnsweredOnce is the same lost
// acknowledgement on the BROADCAST path.
//
// It is not a formality. The operation is part of the idempotency scope AND part
// of publishFingerprint, and a broadcast is stored as a FLAG with no recipient
// list rather than as an expanded roster snapshot — so the record recovery
// rebuilds for a broadcast is a different shape to the one it rebuilds for a
// send, and the fingerprint it recomputes covers a different input. IDEM-17's
// own text asks for "send/broadcast at minimum", and no broadcast had been
// exercised across a crash anywhere in the tree.
//
// It also pins the OP-SCOPING across recovery: the broadcast is retried under
// the very same key string, so an implementation that scoped on the key alone
// would answer the directed-send test's key here and be caught.
func TestIdemCrashInjectionRestartBroadcastRetryIsAnsweredOnce(t *testing.T) {
	dir := t.TempDir()
	runRestartCrashChild(t, crashBroadcastPostCommit, dir)

	original := mustDecodeOnly(t, dir)
	if !original.Broadcast {
		t.Fatalf("the committed message has Broadcast = false; this child was supposed to crash on a BROADCAST, so nothing below is testing the broadcast path")
	}
	if len(original.Recipients) != 0 {
		t.Fatalf("the committed broadcast carries %d recipients, want none: a broadcast is stored as a flag, and a recipient list here would mean the fixture is a directed send wearing a flag", len(original.Recipients))
	}

	lg := openCrashLog(t, dir, true)
	h := openCrashHub(t, lg, lg)

	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after recovering a crashed BROADCAST the applied-key table holds %d records, want 1: the broadcast's applied-key record must be rebuilt from the durable log exactly as a send's is", got)
	}

	again, err := h.Broadcast(verbatimBroadcastReplayOf(original, crashBody))
	if errors.Is(err, hub.ErrUnknownMint) {
		t.Fatalf("the verbatim broadcast replay was refused with ErrUnknownMint: the applied-key lookup must run BEFORE the reservation lookup on the broadcast path too")
	}
	if err != nil {
		t.Fatalf("the verbatim broadcast replay returned err = %v, want the ORIGINAL result: the broadcast IS durable and this is a legitimate retry of a lost acknowledgement", err)
	}
	if !again.Replayed || again.MessageID != original.ID || again.Seq != original.Seq {
		t.Fatalf("the broadcast replay returned %+v, want the original %s / seq %d replayed", again, original.ID, original.Seq)
	}
	if !again.Broadcast {
		t.Fatalf("the broadcast replay returned Broadcast = false: the op did not survive recovery, so the replayed result describes a different operation to the one that was applied")
	}

	// THE OP IS PART OF THE SCOPE. The same key under "send" is a DIFFERENT
	// operation and must be applied as new, not answered with the broadcast's
	// result — and it must not be mistaken for a key-reuse violation either.
	sendUnderSameKey := freshSendRequest(t, h, crashBody)
	if sendUnderSameKey.SignedMint.MessageID == "" {
		t.Fatalf("minting a SEND under the recovered broadcast's key string was REFUSED; the reservation scope is the (agent, op, key) tuple too, so this must be available")
	}
	res, err := h.Send(sendUnderSameKey)
	if errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("a SEND reusing the BROADCAST's key string was rejected as a key-reuse violation (%v): the idempotency scope is the (agent, op, key) tuple, so one key across two routes is two operations and this honest client was told it committed a protocol violation for nothing", err)
	}
	if err != nil {
		t.Fatalf("a send under the same key string as the recovered broadcast returned err = %v, want success: different op, different scope", err)
	}
	if res.Replayed {
		t.Fatalf("a send under the same key string as the recovered broadcast was answered with a REPLAY of message %s: the op is not part of the scope after recovery, so one agent's broadcast result is being handed back for its send", res.MessageID)
	}
	if res.MessageID == original.ID {
		t.Fatalf("the send was answered with the broadcast's own message id %s", original.ID)
	}

	// Exactly TWO effects now, because there were exactly TWO operations.
	if got := len(committedMessagesIn(t, dir)); got != 2 {
		t.Fatalf("the durable log holds %d committed MESSAGE entries, want exactly 2 (one broadcast, one send under the same key string)", got)
	}
	if got := h.IdempotencyStats().Count; got != 2 {
		t.Fatalf("the applied-key table holds %d records, want exactly 2: one per (agent, op, key) scope", got)
	}
	if err := h.Poisoned(); err != nil {
		t.Fatalf("the hub is poisoned after a broadcast retry and a same-key send: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The violation is still a violation, and is still reported as one
// ---------------------------------------------------------------------------

// TestIdemCrashInjectionRestartKeyReuseIsStillTheViolation is the other side of
// the honest-retry test, and it is about WHICH error comes back rather than
// merely that one does.
//
// After the same crash, the same key with a DIFFERENT payload is a protocol
// violation: reject and log (narrowed 2026-08-08 — no disconnect; see
// internal/idem/store.go's Outcome doc comment). The payload fingerprint that
// detects it has to have survived the crash on disk, and the check has to run
// ahead of the reservation lookup — because the reservation table is empty
// after a restart, so a violation checked second would come back as the
// ROUTINE, retriable ErrUnknownMint and the offender would be invited to try
// again rather than told it violated the protocol.
func TestIdemCrashInjectionRestartKeyReuseIsStillTheViolation(t *testing.T) {
	dir := t.TempDir()
	runRestartCrashChild(t, crashPostCommitPreAck, dir)

	original := mustDecodeOnly(t, dir)

	lg := openCrashLog(t, dir, true)
	h := openCrashHub(t, lg, lg)

	_, err := h.Send(verbatimReplayOf(original, crashOtherBody))
	// The SPECIFIC misdiagnosis is checked FIRST. Checking the general case
	// first would make this branch unreachable and cost the failure its
	// diagnosis: a violation misfiled as ErrUnknownMint is not merely "the wrong
	// error", it is the difference between telling the offender it violated the
	// protocol and inviting it to try again.
	if errors.Is(err, hub.ErrUnknownMint) {
		t.Fatalf("a key-reuse violation after a restart was reported as ErrUnknownMint (%v), which is documented as ROUTINE and retriable: the violation check must run BEFORE the reservation lookup, or every post-restart violation is misfiled as a routine re-mint and the offending client is never told it violated the protocol", err)
	}
	if !errors.Is(err, hub.ErrIdempotencyKeyReused) {
		t.Fatalf("reusing the key for DIFFERENT content after the crash gave err = %v, want ErrIdempotencyKeyReused: the payload fingerprint must survive the crash on disk, or a key reused for new content is silently answered with somebody else's result", err)
	}

	// A refused violation writes NOTHING.
	if got := len(committedMessagesIn(t, dir)); got != 1 {
		t.Fatalf("the refused key-reuse left %d committed MESSAGE entries on disk, want 1", got)
	}
	if count, _ := storedCount(t, h); count != 1 {
		t.Fatalf("the refused key-reuse left %d messages in the serving copy, want 1", count)
	}
	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("the refused key-reuse left %d records in the applied-key table, want 1", got)
	}

	// And the honest retry still works AFTERWARDS. A violation must not poison
	// the key for the client that legitimately owns it.
	again, err := h.Send(verbatimReplayOf(original, crashBody))
	if err != nil {
		t.Fatalf("the honest retry AFTER a refused key-reuse returned err = %v, want the original result: rejecting a violation must not invalidate the record the legitimate client depends on", err)
	}
	if !again.Replayed || again.MessageID != original.ID {
		t.Fatalf("the honest retry after a refused key-reuse returned %+v, want the original %s replayed", again, original.ID)
	}
}

// ---------------------------------------------------------------------------
// Cross-agent isolation survives recovery: no oracle in the rebuilt table
// ---------------------------------------------------------------------------

// TestIdemCrashInjectionRestartCrossAgentKeyIsolation proves the applied-key
// scope's AGENT component survives a crash — that the table rebuilt from disk
// still answers "have I applied this?" per (agent, op, key) and not per (op,
// key).
//
// The in-memory half of this is already covered (idem_test.go, store_test.go).
// What was NOT covered is the half that only a restart can reach: the scope is
// rebuilt by DECODING each record and re-running its constructor
// (Record.Scope), so a recovery that dropped, defaulted or transposed the agent
// field would collapse every agent's keys into one namespace — and every
// in-memory test would keep passing, because none of them ever decodes a
// record.
//
// # WHAT A COLLAPSED SCOPE WOULD ACTUALLY DO, AND WHY EITHER OUTCOME IS A LEAK
//
// Agent A's key is durable and A never got its acknowledgement. Agent B — a
// different, legitimate agent that has never used this key — now presents it.
// If the recovered scope had lost its agent component, B meets one of two
// answers, and B's PAYLOAD FINGERPRINT decides which:
//
//   - Fingerprint EQUAL to A's: B is answered A's result. B is handed A's
//     message id and sequence for a message it did not send, and its own send
//     never happens — a cross-agent oracle AND a silently dropped message.
//   - Fingerprint DIFFERENT from A's: B is told ErrIdempotencyKeyReused. B is
//     accused of a protocol violation over a key it has never used, and the
//     refusal itself discloses that somebody else holds that key.
//
// Both rows are asserted because they are DIFFERENT BRANCHES, not two spellings
// of one: only the first can reach the "answered with A's result" assertions,
// and only the second can reach the violation assertion. Verified by mutation —
// collapsing the scope's agent component turns row 1 into a Replayed answer
// carrying A's message id and row 2 into ErrIdempotencyKeyReused.
//
// # WHY B SENDS TO ITSELF IN THE FIRST ROW, WHICH IS OTHERWISE ODD
//
// publishFingerprint (internal/hub) hashes the OP, the RECIPIENT LIST and the
// BODY — and NOT the sender. So making B's fingerprint equal A's requires B to
// send to A's recipient with A's body, and B IS that recipient. The self-send
// is not the point of the row; it is the only way to hold every hashed field
// equal while changing the one field under test, the sending agent. Without it
// the recipient list differs, the fingerprints differ, and row 1 silently
// degenerates into a second copy of row 2 — which is exactly what the first
// draft of this test did, and the mutation run is what exposed it.
//
// The two rows therefore differ in EXACTLY ONE input: the body.
//
// B's send must simply be APPLIED AS NEW in both cases: a new id, a new
// sequence, Replayed false, no error. And A's own honest retry must still be
// answered afterwards — B's insertion must not evict or overwrite the record A
// is still depending on, which is the same fail-closed property the key-reuse
// test pins from the other side.
func TestIdemCrashInjectionRestartCrossAgentKeyIsolation(t *testing.T) {
	tests := []struct {
		name string
		// body is what the SECOND agent sends under the first agent's key. It is
		// the ONLY field that varies between the rows: every other input to the
		// payload fingerprint (the op and the recipient list) is held equal to
		// the crashed agent's, so the row name describes the whole difference.
		body []byte
	}{
		{
			name: "fingerprint identical to the crashed agent's: must not be answered with its result",
			body: crashBody,
		},
		{
			name: "a different payload: must not be accused of reusing a key it has never used",
			body: crashOtherBody,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			// Agent A's send is committed and fsynced, and the process dies before
			// the ack. Its applied-key record is on stable storage and A is still
			// holding an unanswered request.
			runRestartCrashChild(t, crashPostCommitPreAck, dir)
			original := mustDecodeOnly(t, dir)

			lg := openCrashLog(t, dir, true)
			h := openCrashHub(t, lg, lg)

			// The table really was rebuilt from disk — otherwise every assertion
			// below would pass against an empty table, which is the vacuous way
			// this test could look green while proving nothing.
			if got := h.IdempotencyStats().Count; got != 1 {
				t.Fatalf("after recovery the applied-key table holds %d records, want exactly 1 (agent A's): the rest of this test asserts B is NOT answered from that record, so an empty table here would make it vacuous", got)
			}
			if original.Sender != crashAgentID(t, crashSenderName) {
				t.Fatalf("the recovered message was sent by %q, want %q: this test's whole premise is that the durable record belongs to a DIFFERENT agent than the one that sends next", original.Sender, crashAgentID(t, crashSenderName))
			}

			// Agent B presents A's key. B is enrolled, takes its OWN reservation,
			// and has never used this key. B sends to A's RECIPIENT (which is B
			// itself) so that the recipient list — a hashed field — is held equal
			// to A's; see the self-send note in this test's doc comment.
			req := freshSendRequestFrom(t, h, crashRecipientName, crashRecipientName, original.IdempotencyKey, tc.body)

			// The row's premise, asserted rather than assumed: every hashed field
			// except the body is equal to the crashed record's. If this drifts,
			// row 1 stops being the "identical fingerprint" branch and quietly
			// becomes a duplicate of row 2 — a degeneration that leaves the
			// oracle assertions below unreachable while the test still passes.
			if len(original.Recipients) != 1 || req.To != original.Recipients[0] {
				t.Fatalf("agent B is sending to %q but the crashed message's recipients are %v: the payload fingerprint hashes the recipient list, so the rows only differ by BODY if this holds", req.To, original.Recipients)
			}

			res, err := h.Send(req)
			if errors.Is(err, hub.ErrIdempotencyKeyReused) {
				t.Fatalf("agent B presenting agent A's key after recovery was refused with ErrIdempotencyKeyReused (%v): the applied-key scope is the (agent, op, key) tuple, so B has never used this key and has violated nothing; a violation here means the recovered scope lost its AGENT component and B is being adjudicated against A's fingerprint", err)
			}
			if err != nil {
				t.Fatalf("agent B presenting agent A's key after recovery returned err = %v, want the send to be applied as a NEW operation: B is a different agent and this is its first use of the key", err)
			}
			if res.Replayed {
				t.Fatalf("agent B's send was reported Replayed = true: B was answered from agent A's recovered record, which hands B a message id and sequence for a message it never sent AND silently drops the message it did send")
			}
			if res.MessageID == original.ID {
				t.Fatalf("agent B's send returned agent A's message id %s: the recovered applied-key table answered across agents", res.MessageID)
			}
			if res.Seq == original.Seq {
				t.Fatalf("agent B's send returned agent A's sequence %d: two different messages must never share a sequence (invariant 1)", res.Seq)
			}

			// Two distinct effects, both durable.
			if got := len(committedMessagesIn(t, dir)); got != 2 {
				t.Fatalf("the durable log holds %d committed MESSAGE entries, want 2: agent A's recovered message and agent B's new one", got)
			}
			if got := h.IdempotencyStats().Count; got != 2 {
				t.Fatalf("the applied-key table holds %d records, want 2: the same key under two agents is two scopes, not one", got)
			}
			// The DENOMINATOR is a separate plane from the record set, and a
			// recovery can keep the two scopes distinct while still collapsing the
			// per-agent counters they feed. Count alone would not notice: it reads
			// the records map, and the fair share reads byAgent. A collapsed
			// denominator is not cosmetic — it is what decides every later
			// admission, so it is asserted in its own right.
			if got := h.IdempotencyStats().Agents; got != 2 {
				t.Fatalf("the applied-key table's per-agent counters cover %d agents, want 2: the two records survived recovery as distinct scopes but the fair-share DENOMINATOR they feed was collapsed, so the share every later admission is judged against is wrong even though the table looks correct", got)
			}

			// A's record survived B's insertion. A is STILL retrying the
			// acknowledgement it lost in the crash, and must still be answered.
			again, err := h.Send(verbatimReplayOf(original, crashBody))
			if err != nil {
				t.Fatalf("agent A's honest retry AFTER agent B used the same key returned err = %v, want A's original result: B's insertion must not disturb the record A depends on", err)
			}
			if !again.Replayed || again.MessageID != original.ID || again.Seq != original.Seq {
				t.Fatalf("agent A's retry returned %+v, want the original %s / seq %d replayed", again, original.ID, original.Seq)
			}
			if err := h.Poisoned(); err != nil {
				t.Fatalf("the hub is poisoned after a cross-agent key collision across recovery: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Two crashes, both inside the retry window
// ---------------------------------------------------------------------------

// TestIdemCrashInjectionRestartTwiceStillOneEffect kills the bus TWICE inside a
// single retry window — once after the ack with an in-process retry already
// answered, and again while a post-restart retry was being answered — and
// asserts the operation still happened exactly once.
//
// One crash proves recovery reads the log. Two prove recovery is IDEMPOTENT in
// its own right: the second recovery re-reads a log the first one already
// rebuilt from, and a replay answered from the recovered table must not itself
// write anything that a third recovery would count as a second effect.
func TestIdemCrashInjectionRestartTwiceStillOneEffect(t *testing.T) {
	dir := t.TempDir()

	// CRASH 1: acknowledged, retried in-process, killed with the window open.
	runRestartCrashChild(t, crashPostAck, dir)
	afterFirst := mustDecodeOnly(t, dir)

	// CRASH 2: a fresh process recovers that log, is answered the replay, and
	// dies holding an answer nobody received.
	runRestartCrashChild(t, crashMidRetry, dir)
	afterSecond := mustDecodeOnly(t, dir)

	if afterSecond.ID != afterFirst.ID || afterSecond.Seq != afterFirst.Seq {
		t.Fatalf("after the second crash the durable log holds message %s / seq %d, want the same %s / seq %d the first crash left: recovery must not re-mint an id it has already handed out (invariant 1)",
			afterSecond.ID, afterSecond.Seq, afterFirst.ID, afterFirst.Seq)
	}

	// THIRD recovery, and the client finally gets its answer.
	lg := openCrashLog(t, dir, true)
	h := openCrashHub(t, lg, lg)

	if got := h.IdempotencyStats().Count; got != 1 {
		t.Fatalf("after TWO SIGKILLs the applied-key table holds %d records, want exactly 1: the key must be rebuilt identically at every recovery, not accumulated and not lost", got)
	}

	again, err := h.Send(verbatimReplayOf(afterSecond, crashBody))
	if err != nil {
		t.Fatalf("the retry after two crashes returned err = %v, want the original result: the client has been retrying the same lost acknowledgement across two restarts and has still done nothing wrong", err)
	}
	if !again.Replayed || again.MessageID != afterFirst.ID || again.Seq != afterFirst.Seq {
		t.Fatalf("the retry after two crashes returned %+v, want the original %s / seq %d replayed", again, afterFirst.ID, afterFirst.Seq)
	}

	if got := len(committedMessagesIn(t, dir)); got != 1 {
		t.Fatalf("after two crashes, three recoveries and three retries the durable log holds %d committed MESSAGE entries, want exactly 1", got)
	}
	if count, head := storedCount(t, h); count != 1 || head != afterFirst.Seq {
		t.Fatalf("the serving copy holds %d messages with head %d, want 1 at seq %d", count, head, afterFirst.Seq)
	}
	if err := h.Poisoned(); err != nil {
		t.Fatalf("the hub is poisoned after two crashes inside one retry window: %v", err)
	}
}
