//go:build linux || darwin

// RELAY-48's ACCEPTANCE EVIDENCE: a pending ONWARD relay hop survives a real
// kill -9 and is RESUMED rather than durably abandoned.
//
// # WHAT WAS BROKEN
//
// Bus B accepts a relayed message from A addressed to an agent on C, answers A
// **200** — a durable acknowledgement — and writes an outbox job recording that
// it owes C a copy. B restarts. The obligation was DESTROYED: the job settled
// `abandoned`, C never heard about the message, and A did not retry because A
// was told 200. Invariant 6 held throughout (the discard was logged loudly); what
// did not hold was the promise implied by accepting the obligation durably in the
// first place, which is invariant 4's guarantee in spirit.
//
// The mechanism was two-layered, and fixing only the outer layer RELOCATES the
// bug rather than removing it:
//
//  1. nothing ever set store.Message.OriginMessageID, so Store.byOrigin was
//     permanently empty and the recovery lookup MISSED; and
//  2. even with it set, store.Message had no attestation field, while the next
//     hop REQUIRES an origin attestation this bus may not mint (invariant 2) —
//     so a relayed-in envelope was unbuildable from durable state by
//     construction, and the job was abandoned one line further down instead.
//
// # WHY THIS TEST HAS TO CRASH AND RESTART, AND WHAT THAT DISTINGUISHES
//
// The durable write happens at internal/hub/hub.go, between
// store.NewMessageWithBusPath and m.Encode(). Message.Record() is the ONLY thing
// that carries the two fields to disk and Encode()'s output IS the WAL entry
// body — but Store.Append populates the byOrigin index from the LIVE value
// regardless. So a writer placed one line too late:
//
//	m, _ := store.NewMessageWithBusPath(...)
//	payload, _ := m.Encode()          // <- the record is now FROZEN
//	m, _ = m.WithRelayOrigin(...)     // <- SILENT NO-OP as far as disk is concerned
//
// passes every in-process assertion — ByOriginMessageID resolves, the envelope
// rebuilds, the job is re-offered — and loses the message on the one path that
// matters. The two fields' ONLY reader (relay.Forwarder.Resume) runs ONLY after a
// restart.
//
// This test therefore does the write in a CHILD PROCESS that is SIGKILLed, and
// does the resume in the PARENT, which rebuilds the store from the durable log
// alone. Nothing in-memory survives that boundary, so the late-writer variant
// goes RED here: the replayed store's byOrigin is empty, RecoverMessage returns
// "no such message", and Resume re-offers 0. That was confirmed by MUTATION, not
// by argument — see TestOnwardRelayPendingJobRequeuesAfterRestart's own comment.
//
// A graceful Close would not do either: it lets every deferred flush and shutdown
// path run, so it proves only that the happy path tidies up. SIGKILL is the only
// thing that proves none of that was load-bearing.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

const (
	// envR48CrashDir is the data directory the child writes into: a t.TempDir()
	// belonging to the parent, so no run shares a data directory with another and
	// the tracked data/ dir is never touched.
	envR48CrashDir = "AGENT_BUS_RELAY48_CRASH_DIR"

	// r48OriginSeq is the ORIGIN bus's sequence for the transit message. The
	// origin message id is derived from it, and both processes derive it the same
	// way rather than passing a string between them.
	r48OriginSeq = 7

	// r48DeadPeerURL is where the CHILD's registry points the peer: port 9 is the
	// discard port and refuses the connection, which relay.Forwarder classifies as
	// RETRIABLE. That is what leaves the job PENDING for the crash to preserve —
	// a reachable peer would settle it `delivered` before the kill and this test
	// would be about nothing.
	r48DeadPeerURL = "https://127.0.0.1:9"
)

var r48Body = []byte("a transit message this bus owes a third bus a copy of")

// r48OriginMessageID is the id the ORIGIN bus minted. Both processes compute it,
// so they cannot disagree about the correlation key the outbox job is filed
// under.
func r48OriginMessageID(t *testing.T) string {
	t.Helper()
	id, err := ids.MessageID(egOriginBus, r48OriginSeq)
	if err != nil {
		t.Fatalf("ids.MessageID(%q, %d): %v", egOriginBus, r48OriginSeq, err)
	}
	return id
}

// r48Sender is the ORIGIN bus's agent. Its bus half is NOT ours (invariant 2),
// which is what makes this an ingest rather than a local send.
func r48Sender(t *testing.T) string {
	t.Helper()
	id, err := ids.AgentID(egOriginBus, "alpha", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	return id
}

// r48Attestation is the ORIGIN bus's attestation for the sender: the artefact
// this whole task exists to keep, and the one field of the onward envelope this
// bus can never regenerate.
//
// It is WELL-FORMED BUT NOT GENUINE, which is the right fidelity for this test:
// the durability layer validates SHAPE and BINDING-TO-SENDER and deliberately
// never verifies, because verification needs the origin bus's peering-time
// pinned signing key and that lives in the relay peer store. What this test
// proves is that the exact bytes handed to the ingest are the exact bytes the
// next hop is offered after a crash — which is a durability property, not a
// cryptographic one.
//
// The two byte patterns are distinctive so that an assertion failure shows at a
// glance whether the value was lost (zero) or altered (anything else).
func r48Attestation(t *testing.T) attest.Attestation {
	t.Helper()
	return attest.Attestation{
		AgentID:            r48Sender(t),
		MessagingPublicKey: bytes.Repeat([]byte{0x5A}, ed25519.PublicKeySize),
		KeyEpoch:           99,
		IssuedAtUnixMilli:  egTimestampMs,
		NotAfterUnixMilli:  egTimestampMs + 86_400_000,
		Signature:          bytes.Repeat([]byte{0xA5}, signing.SignatureSize),
	}
}

// r48Transit is the message as it ARRIVES: from the origin bus's alpha, for an
// agent on the PEER bus, having travelled one hop.
//
// It is TRANSIT and not local mail — no recipient is ours — which is exactly the
// case RELAY-47 made possible and RELAY-48 made survivable. BusPath is the path
// AS RECEIVED, WITHOUT this bus: the ingest appends our own hop.
func r48Transit(t *testing.T) relay.RelayedMessage {
	t.Helper()
	return relay.RelayedMessage{
		OriginBus:          egOriginBus,
		OriginMessageID:    r48OriginMessageID(t),
		OriginSeq:          r48OriginSeq,
		Sender:             r48Sender(t),
		Recipients:         []string{egAgentID(t, egPeerBusID, "gamma")},
		BusPath:            []string{egOriginBus},
		TimestampUnixMilli: egTimestampMs,
		OriginAttestation:  r48Attestation(t),
		Signature:          egSignature(),
		Body:               r48Body,
		ContentSHA256:      egSHA256(r48Body),
		IdempotencyKey:     r48OriginMessageID(t),
	}
}

// ---------------------------------------------------------------------------
// The bus, built identically either side of the crash
// ---------------------------------------------------------------------------

// r48Bus is the production trio over one data directory: the durable log, the
// hub that serves and records messages, and the outbox + forwarder that owe
// peers deliveries.
type r48Bus struct {
	log      *wal.Log
	hub      *hub.Hub
	outbox   *relay.Outbox
	registry *relay.Registry
	fwd      *relay.Forwarder
}

// openR48Bus assembles the bus over dir, in the SAME order main.go does and for
// the same reasons: the outbox applier must exist before wal.Open, because
// replay runs INSIDE Open and hands it every committed entry; the registry is
// seeded BEFORE Resume is ever called, because Resume resolves every recovered
// job through PeerBaseURL and would take the no-route arm for the whole backlog
// against an empty table.
//
// RecoverMessage is the PRODUCTION function, recoverRelayEnvelope, not a copy of
// it written here. A crash test that re-implements the recovery decision beside
// itself proves its own copy correct and says nothing about the server's — which
// is precisely why that decision was lifted out of main.go's closure.
//
// closeOnCleanup is FALSE in the crash child. A registered Close that ran would
// be the graceful shutdown this test exists to rule out.
func openR48Bus(t *testing.T, dir, peerBaseURL string, closeOnCleanup bool) *r48Bus {
	t.Helper()
	b := &r48Bus{}

	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: egLocalBus})
	if err != nil {
		t.Fatalf("relay.NewOutbox: %v", err)
	}
	lg, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ob})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if err := ob.Attach(lg); err != nil {
		t.Fatalf("Outbox.Attach: %v", err)
	}
	b.outbox, b.log = ob, lg

	path := lg.Path()
	h, err := hub.Open(hub.Options{
		BusID:   egLocalBus,
		DataDir: filepath.Dir(path),
		Durable: lg,
		// The hub rebuilds its serving copy from the durable log, which is what
		// makes the parent's view of this message a RECOVERED one rather than a
		// remembered one.
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg.Recovered().NextIndex,
		// No local agent is enrolled, deliberately: every recipient here belongs to
		// another bus, so this message is pure transit.
		Roster: hub.NewStaticRoster(),
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	b.hub = h

	reg, err := relay.NewRegistry(relay.RegistryOptions{BusID: egLocalBus})
	if err != nil {
		t.Fatalf("relay.NewRegistry: %v", err)
	}
	if err := reg.UpsertPeer(relay.PeerRoster{BusID: egPeerBusID}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	if err := reg.SetPeerBaseURL(egPeerBusID, peerBaseURL); err != nil {
		t.Fatalf("SetPeerBaseURL(%s): %v", peerBaseURL, err)
	}
	b.registry = reg

	if closeOnCleanup {
		t.Cleanup(func() { _ = lg.Close() })
	}
	return b
}

// ---------------------------------------------------------------------------
// The child
// ---------------------------------------------------------------------------

// TestRelay48CrashChild is the child half of the test below. It does NOTHING in
// a normal run: without envR48CrashDir it skips immediately.
//
// It performs the two durable writes a real onward relay performs, in the same
// order and through the same code:
//
//  1. hubIngest.AcceptRelayed — the production seam relay.Acceptor calls, which
//     takes the hub's ONE two-phase write path (invariant 4).
//  2. Forwarder.Enqueue — the production onward seam, which writes the outbox
//     record BEFORE it offers the job, so the obligation is durable the moment
//     Enqueue returns.
//
// Then it kills itself, with the job still PENDING because the peer address
// refuses connections.
func TestRelay48CrashChild(t *testing.T) {
	dir := os.Getenv(envR48CrashDir)
	if dir == "" {
		t.Skip("not a crash child: " + envR48CrashDir + " is unset")
	}

	// closeOnCleanup is false: see openR48Bus.
	b := openR48Bus(t, dir, r48DeadPeerURL, false)
	peer := newEgPeerBus(t)
	b.fwd = egForwarderLogging(t, b.registry, peer, b.outbox,
		func(originMessageID string) (relay.RelayedMessage, bool, error) {
			return recoverRelayEnvelope(egLocalBus, b.hub.Store(), func(store.Message) (relay.RelayedMessage, error) {
				return relay.RelayedMessage{}, errors.New("child: a locally-originated envelope must never be built here")
			}, originMessageID)
		}, io.Discard)

	m := r48Transit(t)
	acc, err := hubIngest{h: b.hub}.AcceptRelayed(context.Background(), m)
	if err != nil {
		t.Fatalf("child: AcceptRelayed: %v", err)
	}
	if acc.LocalMessageID == "" {
		t.Fatal("child: the ingest returned no local message id")
	}

	queued, err := b.fwd.Enqueue(m)
	if err != nil {
		t.Fatalf("child: Forwarder.Enqueue: %v", err)
	}
	if queued != 1 {
		t.Fatalf("child: Enqueue queued %d copies, want 1 (the peer %q must be routable, or this crash preserves nothing)", queued, egPeerBusID)
	}

	// The obligation must be DURABLE before the kill, or the parent is testing an
	// empty directory. Pending() reads the table the WAL replayed into, and
	// Outbox.Enqueue writes through the log synchronously, so a job visible here
	// is a job on disk.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(b.outbox.Pending()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child: the outbox holds %d pending jobs after 5s, want 1", len(b.outbox.Pending()))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// SIGKILL. Nothing after this line runs — no deferred Close, no flush, no
	// forwarder drain — which is the whole point.
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	t.Fatal("child: still alive after SIGKILL; the crash was never injected")
}

// runR48CrashChild re-execs this test binary as the crash child and asserts it
// really DIED ON SIGKILL rather than failing its own assertions — without that
// check a broken child would silently turn the parent into a test of an empty
// directory.
func runR48CrashChild(t *testing.T, dir string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestRelay48CrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), envR48CrashDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child did not finish in time: %v\n--- child output ---\n%s", ctx.Err(), out.String())
	}
	// A child that failed its OWN assertions also exits non-zero, so "err != nil"
	// is not the assertion. The wait status is.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child: Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child: wait status is %T, want syscall.WaitStatus", ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child exited with status %d instead of dying on SIGKILL; the crash was never injected\n--- child output ---\n%s", ws.ExitStatus(), out.String())
	}
}

// ---------------------------------------------------------------------------
// The proof
// ---------------------------------------------------------------------------

// TestOnwardRelayPendingJobRequeuesAfterRestart is RELAY-48's proof.
//
// A child process accepts a transit message from bus A for an agent on bus C,
// durably records the obligation to carry it onward, and is SIGKILLed. This
// process then opens the exact bytes the dying process left behind, with nobody
// having tidied the tail on the way out, and asserts the obligation is RESUMED.
//
// # WHAT EACH ASSERTION RULES OUT
//
//   - the job is back in the outbox: the durable record survived and replay put
//     it back.
//   - the RECOVERED message carries the origin id AND the origin attestation:
//     the durable record carried them, which is the assertion that fails if the
//     ingest-side writer sits after m.Encode(). This is the one that distinguishes
//     a correct fix from a silent no-op, and it is checked BEFORE Resume so a
//     failure names the cause rather than the symptom.
//   - Resume re-offers 1, not 0: the recovery seam rebuilt an envelope instead of
//     abandoning the job. Before this task it was 0 on every run.
//   - the peer actually RECEIVES the envelope, and it carries the ORIGIN's
//     attestation byte for byte, the ORIGIN's message id, the ORIGIN's sender and
//     a bus path of [origin, us]. Asserting only `re_offered=1` would be a
//     VACUOUS pass for this task: an envelope missing its attestation is refused
//     by the next hop's ValidateRelayRequest, so a job re-offered with one
//     missing is a job that will never be delivered.
//   - the job settles DELIVERED: the obligation is discharged rather than
//     re-offered for ever.
//
// # MUTATION EVIDENCE — this test has been watched to FAIL
//
// Moving the WithRelayOrigin call in internal/hub/hub.go from BEFORE m.Encode()
// to AFTER it — the silent-no-op placement the file header describes — turns this
// test RED on the recovered-message assertion (`the recovered message carries no
// origin message id`) and, with that assertion removed, red again on
// `Resume re-offered 0 jobs`. Every non-restart test in the tree stays GREEN
// under the same mutation. A durability test nobody has watched fail is not
// evidence.
func TestOnwardRelayPendingJobRequeuesAfterRestart(t *testing.T) {
	dir := t.TempDir()

	// --- THE CRASH ---
	runR48CrashChild(t, dir)

	// --- THE RESTART ---
	//
	// A LIVE peer this time: the child's registry pointed at a dead port so the
	// job would still be owed, and this one must actually be delivered to.
	peer := newEgPeerBus(t)
	b := openR48Bus(t, dir, peer.srv.URL, true)
	b.fwd = egForwarderLogging(t, b.registry, peer, b.outbox,
		func(originMessageID string) (relay.RelayedMessage, bool, error) {
			return recoverRelayEnvelope(egLocalBus, b.hub.Store(), func(store.Message) (relay.RelayedMessage, error) {
				// A relay-ingested message must NEVER be routed through the
				// originated-here builder: it would claim this bus as the origin and
				// try to attest another bus's agent (invariant 2). Failing loudly
				// here is what makes "the transit branch was taken" an assertion
				// rather than an assumption.
				return relay.RelayedMessage{}, errors.New("the locally-originated envelope builder was called for a relay-ingested message")
			}, originMessageID)
		}, io.Discard)

	originID := r48OriginMessageID(t)

	// STAGE 1. The durable outbox record survived the kill and replay put it back.
	pending := b.outbox.Pending()
	if len(pending) != 1 {
		t.Fatalf("after the crash the outbox holds %d pending jobs, want exactly 1 for %s owed to %s.\n\nThe obligation this bus accepted on the peer's behalf is gone from disk, which is a different (and worse) defect than the one this test is about", len(pending), originID, egPeerBusID)
	}
	if want := relay.DeriveJobID(egPeerBusID, originID); pending[0].JobID != want {
		t.Fatalf("the recovered job id is %q, want %q", pending[0].JobID, want)
	}

	// STAGE 2 — THE ASSERTION THAT SEPARATES A REAL FIX FROM A SILENT NO-OP.
	//
	// This process has never seen the message: its store was rebuilt from the
	// durable log alone. So both fields below are being read off disk, at the one
	// moment they are ever read. A writer placed after m.Encode() populates the
	// LIVE store's index and writes NEITHER, and only a restart can tell.
	recovered, ok := b.hub.Store().ByOriginMessageID(originID)
	if !ok {
		t.Fatalf(`the recovered store cannot resolve origin message id %s.

Store.byOrigin is rebuilt from the DURABLE record on replay, so this means the
record carries no origin_message_id. The ingest-side writer must sit BETWEEN
store.NewMessageWithBusPath and m.Encode() in internal/hub/hub.go: Encode's
output IS the WAL entry body, so a writer placed after it stamps only the serving
copy — which passes every test that does not restart.`, originID)
	}
	if recovered.OriginMessageID != originID {
		t.Fatalf("the recovered message carries origin message id %q, want %q", recovered.OriginMessageID, originID)
	}
	if recovered.ID == originID {
		t.Fatalf("the recovered message's OWN id is %q, the origin's id; this bus mints its own and never adopts a peer's (invariant 1)", recovered.ID)
	}
	want := r48Attestation(t)
	if !recovered.HasOriginAttestation() {
		t.Fatalf(`the recovered message carries NO origin attestation.

It is the one field of the onward envelope this bus cannot regenerate --
attest.Sign refuses a subject in another bus's namespace (invariant 2) -- so
without it on disk the envelope is unbuildable and the job is abandoned. Like the
origin id above, it reaches disk only via Message.Record(), i.e. only if it is set
BEFORE m.Encode().`)
	}
	if got := recovered.OriginAttestation; got.AgentID != want.AgentID ||
		got.KeyEpoch != want.KeyEpoch ||
		got.IssuedAtUnixMilli != want.IssuedAtUnixMilli ||
		got.NotAfterUnixMilli != want.NotAfterUnixMilli ||
		!bytes.Equal(got.MessagingPublicKey, want.MessagingPublicKey) ||
		!bytes.Equal(got.Signature, want.Signature) {
		t.Fatalf("the recovered origin attestation is not the one the origin issued:\n got %+v\nwant %+v\n\nIt must be carried VERBATIM: no hop may re-mint or alter it, and one that does produces a blob the next hop refuses as a forgery", got, want)
	}

	// STAGE 3. The obligation is RESUMED, not abandoned. This was 0 before
	// RELAY-48, on every run, with the job settling `abandoned`.
	requeued, err := b.fwd.Resume()
	if err != nil {
		t.Fatalf("Forwarder.Resume: %v", err)
	}
	if requeued != 1 {
		t.Fatalf(`Resume re-offered %d jobs, want 1.

The registry was seeded before this call and the message is in the recovered
store, so zero here means RecoverMessage refused to rebuild the envelope and the
job was settled ABANDONED -- after this bus had already answered the upstream peer
200. That peer will not resend.`, requeued)
	}

	// STAGE 4. It actually reaches the peer, carrying what the ORIGIN said.
	got := peer.await(t, 1, "a resumed onward hop must actually reach the next bus; re_offered=1 alone proves only that a job went back on a queue")
	peer.quiet(t, 1, "one recovered job must produce exactly one delivery")
	req := got[0]
	if req.MessageID != originID {
		t.Errorf("the peer received message_id %q, want the ORIGIN's %q; an intermediate must not renumber a message it is carrying", req.MessageID, originID)
	}
	if req.OriginBus != egOriginBus {
		t.Errorf("the peer received origin_bus %q, want %q", req.OriginBus, egOriginBus)
	}
	if req.Sender != r48Sender(t) {
		t.Errorf("the peer received sender %q, want the ORIGIN's %q", req.Sender, r48Sender(t))
	}
	if len(req.BusPath) != 2 || req.BusPath[0] != egOriginBus || req.BusPath[1] != egLocalBus {
		t.Errorf(`the peer received bus_path %v, want [%s %s].

The stored path already ends with our hop, and relay.AppendHop adds it again
inside Forward -- so the recovery builder must STRIP the final hop. Handing
AppendHop a path it is already on comes back ErrRelayLoop, and the bus logs a
loop about a message that never left.`, req.BusPath, egOriginBus, egLocalBus)
	}
	if !bytes.Equal(req.Body, r48Body) {
		t.Errorf("the peer received a %d-byte body, want the original %d bytes", len(req.Body), len(r48Body))
	}
	if got := req.OriginAttestation; got.AgentID != want.AgentID ||
		!bytes.Equal(got.MessagingPublicKey, want.MessagingPublicKey) ||
		!bytes.Equal(got.Signature, want.Signature) ||
		got.KeyEpoch != want.KeyEpoch ||
		got.IssuedAtUnixMilli != want.IssuedAtUnixMilli ||
		got.NotAfterUnixMilli != want.NotAfterUnixMilli {
		t.Fatalf(`the envelope the peer received does not carry the ORIGIN's attestation:
 got %+v
want %+v

relay.ValidateRelayRequest REQUIRES one and verifies it against the origin bus's
pinned signing key, so an envelope re-offered without it is a job that will be
refused at every attempt until its retry horizon expires. Asserting only that the
job was re-offered would call that a pass.`, got, want)
	}

	// STAGE 4b — THE NEXT HOP'S OWN VALIDATOR, RUN OVER THE REBUILT ENVELOPE.
	//
	// The fake peer above accepts any JSON and answers 200, so every assertion so
	// far checks fields THIS TEST THOUGHT OF. A field ValidateRelayRequest starts
	// requiring tomorrow could be dropped by relayedOriginEnvelope with all of the
	// above still green. So the real receiving-side validator is run here, over
	// the exact bytes the peer was handed.
	//
	// A NIL TRUST IS THE POINT, not a shortcut. VerifyRelayed's first check is
	// "trust == nil is ErrNoSignerKey, never a skip", so a nil trust makes every
	// STRUCTURAL check run to completion and stops at exactly the one step this
	// test cannot perform — the origin bus's peering-time pinned signing key,
	// which lives in a peer store no crash test seeds. Landing on ErrNoSignerKey
	// therefore means checks 1-7, the attestation's shape, the path, the size, the
	// content hash and the idempotency-key equality ALL PASSED. Landing anywhere
	// else means the rebuilt envelope is structurally wrong and would be refused
	// by a real peer.
	if _, err := relay.ValidateRelayRequest(egPeerBusID, req.MessageID, req, nil); !errors.Is(err, relay.ErrNoSignerKey) {
		t.Fatalf(`the rebuilt envelope does not survive the NEXT HOP's own validator: ValidateRelayRequest = %v

want relay.ErrNoSignerKey, which is where a nil CrossBusTrust stops AFTER every
structural check has passed. Any other error -- ErrInvalidRelay,
ErrMissingAttestation, ErrRelayKeyMismatch, a path or hash refusal -- means
relayedOriginEnvelope built something a real peer would reject, and the fake peer
in this test accepts anything so nothing else here would notice.`, err)
	}

	// STAGE 5. The obligation is discharged durably, or the next start re-offers
	// it for ever.
	egAwaitState(t, b.outbox, pending[0].JobID, relay.OutboxDelivered,
		"a resumed job that reached the peer must be settled DELIVERED")
}

// TestRecoverRelayEnvelopeRefusesARecordWithNoOriginAttestation pins the ONE
// case that is still abandoned, and pins it as a NAMED refusal rather than a
// silent one.
//
// A message relay-ingested by a build that had nowhere durable to keep the
// origin's attestation decodes fine, serves fine and is delivered to local
// recipients fine — only its ONWARD hop is unrecoverable, because this bus may
// not mint an attestation for another bus's agent (invariant 2). That is a
// genuine, bounded, historical loss, and the requirement on it is invariant 6's:
// the discard must be loud and specific, never silent and never a guess at an
// envelope the next hop would refuse.
//
// It also fixes the SHAPE of the half-fix this task's record warns about. Setting
// only store.Message.OriginMessageID makes ByOriginMessageID hit and lands
// exactly here — so this test is what keeps that state a named error instead of
// quietly becoming an unattested envelope on the wire.
func TestRecoverRelayEnvelopeRefusesARecordWithNoOriginAttestation(t *testing.T) {
	sender := r48Sender(t)
	m, err := store.NewMessageWithBusPath(
		egLocalBus, sender, false, []string{egAgentID(t, egPeerBusID, "gamma")},
		3, time.Now().UTC(), r48Body, "r48-no-attestation", egTimestampMs, egSignature(),
		[]string{egOriginBus, egLocalBus},
	)
	if err != nil {
		t.Fatalf("store.NewMessageWithBusPath: %v", err)
	}
	// WithOriginMessageID, deliberately: it is the ID-ONLY setter, and this is the
	// exact state it can still produce.
	m, err = m.WithOriginMessageID(r48OriginMessageID(t))
	if err != nil {
		t.Fatalf("WithOriginMessageID: %v", err)
	}
	if m.HasOriginAttestation() {
		t.Fatal("WithOriginMessageID set an origin attestation; it must set the correlation id and nothing else")
	}

	_, err = relayedOriginEnvelope(egLocalBus, m)
	if err == nil {
		t.Fatal(`relayedOriginEnvelope BUILT an envelope for a message with no origin attestation.

relay.ValidateRelayRequest requires one at the next hop, so what was built cannot
be delivered -- and the peer's refusal would be indistinguishable from an attack.
This must be a named refusal, which the forwarder turns into one loudly-logged
abandoned job.`)
	}
	for _, want := range []string{"origin attestation", "invariant 2"} {
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("the refusal does not mention %q, so an operator cannot tell this from any other rebuild failure: %v", want, err)
		}
	}
}
