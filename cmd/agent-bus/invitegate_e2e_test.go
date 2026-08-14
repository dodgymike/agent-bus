package main

// INVITE-GATE, part 6: THE RUNNING BUS.
//
// Everything else about this feature can be green while the shipped binary does
// nothing at all, and that has happened in this repo before: auth.WALRoster was
// landed and unit-tested by AUTH-3 and every one of those tests stayed green
// while cmd/agent-bus built its Service with NO roster — the whole defect lived
// in one missing field in main.go and nothing in internal/auth could see it.
//
// The invite gate has the same shape and a worse failure mode. main.go must
//
//	build the invite store BEFORE wal.Open,
//	open the log with the MULTIPLEXER (not the roster) as its applier,
//	Attach both participants,
//	and pass the store to httpapi.Options.Invites,
//
// and if ANY of those four is missing the unit suites stay green while a
// running bus either refuses every redemption against an empty table or spends
// nothing at all. Only a test that starts the real process, mints through the
// real operator subcommand, redeems over the real HTTPS route and RESTARTS can
// tell "the code is written" from "a running bus does it".
//
// The sequence, and what each step would catch on its own:
//
//	1. start on a fresh dir, stop cleanly       -- a normal data directory
//	2. `agent-bus invite mint` (bus STOPPED)    -- the operator's real surface
//	3. start; POST /v1/enroll WITH the invite   -- Options.Invites is wired
//	4. an UN-INVITED enrolment still succeeds   -- the gate is NOT flipped
//	5. stop, START AGAIN                        -- the composite record replays
//	6. the agent authenticates on the SAME key  -- the enrolment half survived
//	7. the invite is REFUSED a second time      -- the invite half survived
//
// Step 7 is the one that makes the whole feature worth having. Without it a
// single-use invite is single-use only until the next restart, and one invite
// admits an unbounded number of agents.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// igeEnrolBody builds a /v1/enroll request body, including the invite fields
// only when they are non-empty — the wire shape a client without an invite
// actually sends.
func igeEnrolBody(name, pubB64, key, inviteID, secret string) map[string]string {
	body := map[string]string{
		"name":            name,
		"public_key":      pubB64,
		"idempotency_key": key,
	}
	if inviteID != "" {
		body["invite_id"] = inviteID
	}
	if secret != "" {
		body["invite_secret"] = secret
	}
	return body
}

// igeStopServer shuts a child bus down the way an operator does and insists on
// a clean exit: a bus that fell over on the way out would leave a data
// directory this test cannot reason about.
func igeStopServer(t *testing.T, p *serverProc) {
	t.Helper()
	p.signal(t, syscall.SIGTERM)
	if code := p.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("the bus exited with %d, want 0\n%s", code, p.stderr())
	}
}

// TestInviteGateARunningBusRedeemsAnInviteAndRemembersItAcrossARestart is the
// behavioural acceptance proof for INVITE-GATE.
func TestInviteGateARunningBusRedeemsAnInviteAndRemembersItAcrossARestart(t *testing.T) {
	dataDir := t.TempDir()

	// --- 1. A first, ordinary start, so the directory has its identity, its
	// certificate and its agent-id floors file exactly as a real deployment
	// would. Minting into a virgin directory is a different scenario and
	// `agent-bus invite mint` has its own tests for it.
	first := startServer(t, dataDir)
	firstAddr := first.awaitServerStarted(t)
	mustGetHealthz(t, dataDir, firstAddr)
	igeStopServer(t, first)

	// --- 2. Mint through the OPERATOR's real subcommand, with the bus STOPPED
	// (it takes the exclusive dirlock; two writers on one WAL destroy it).
	code, stdout, stderr := runMint(t,
		"-data-dir", dataDir,
		"-bus-address", "https://127.0.0.1:8443",
		"-ttl", "1h",
		"-label", "the invite-gate end-to-end fixture",
		"-json")
	if code != exitInviteOK {
		t.Fatalf("`invite mint` exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteOK, stdout, stderr)
	}
	var blob inviteBlob
	if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
		t.Fatalf("`invite mint --json` output is not one JSON object: %v\ngot: %s", err, stdout)
	}
	if blob.InviteID == "" || blob.InviteSecret == "" {
		t.Fatalf("the minted blob is missing the id or the secret: %s", stdout)
	}

	// --- 3. Restart and REDEEM the invite over the real route.
	second := startServer(t, dataDir)
	secondAddr := second.awaitServerStarted(t)

	// The startup line is the operator's evidence that the table was REBUILT by
	// replay rather than started empty — the failure that makes every redemption
	// fail closed against invites the operator can see on disk.
	recovered := parseLogfmt(second.line(t, "invite table recovered"))
	if got := recovered["invites_recovered"]; got != "1" {
		t.Fatalf(`the startup line reports invites_recovered=%q, want "1".

The invite `+blob.InviteID+` is on disk -- `+"`invite mint`"+` wrote it before this
process started. A zero here means the log was opened without the invite store
among its appliers, and every redemption would be refused against an empty
table: a 100%% enrolment-by-invite outage that looks exactly like a working
gate.`, got)
	}
	// And the same line must keep saying the gate is NOT on, because it is not.
	if got := recovered["enrolment_invite_required"]; got != "false" {
		t.Fatalf("the startup line reports enrolment_invite_required=%q, want \"false\"; this build redeems an invite, it does not require one", got)
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating an Ed25519 keypair: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	enrolKey := fmt.Sprintf("invited-%d", time.Now().UnixNano())

	body := mustPostJSON(t, dataDir, secondAddr, "/v1/enroll", "",
		igeEnrolBody("invited", pubB64, enrolKey, blob.InviteID, blob.InviteSecret),
		http.StatusCreated)
	var enrolled struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatalf("decoding the enrol response %s: %v", body, err)
	}
	if enrolled.AgentID == "" {
		t.Fatalf("the enrol response carries no agent_id: %s", body)
	}
	invitedAgent := &busAgent{id: enrolled.AgentID, priv: priv}

	// --- 4. THE GATE IS NOT FLIPPED. An enrolment presenting NO invite is still
	// accepted, on the same running bus, in the same breath. Nine agents are
	// enrolled on a live bus and the shipped client cannot present an invite at
	// all, so a refusal here is a total onboarding outage.
	uninvited := enrolNewAgent(t, dataDir, secondAddr, "uninvited")
	if uninvited.id == "" {
		t.Fatalf("the un-invited enrolment produced no agent id")
	}

	// The invite is spent NOW, before any restart: a second presentation with a
	// DIFFERENT idempotency key is refused, and refused with the collapsed body.
	igeAssertInviteRefused(t, dataDir, secondAddr, pubB64, blob, "already-spent-pre-restart")

	igeStopServer(t, second)

	// --- 5. THE RESTART. Everything below has to come off the durable log:
	// the composite entry is the only record of either half.
	third := startServer(t, dataDir)
	thirdAddr := third.awaitServerStarted(t)
	if got := parseLogfmt(third.line(t, "invite table recovered"))["invites_recovered"]; got != "1" {
		t.Fatalf("after the restart the startup line reports invites_recovered=%q, want \"1\"", got)
	}

	// --- 6. THE ENROLMENT HALF SURVIVED. The agent presents the SAME key
	// against the SAME id and completes the challenge/response handshake. A test
	// that generated a fresh key here would be re-enrolling in disguise.
	invitedAgent.authenticate(t, dataDir, thirdAddr)
	if invitedAgent.token == "" {
		t.Fatalf("agent %s could not authenticate after the restart; its enrolment half did not survive the composite record's replay", invitedAgent.id)
	}
	uninvited.authenticate(t, dataDir, thirdAddr)

	// --- 7. THE INVITE HALF SURVIVED, AND THIS IS THE POINT.
	igeAssertInviteRefused(t, dataDir, thirdAddr, pubB64, blob, "already-spent-post-restart")

	// Belt and braces, straight off the disk: a fresh replay of the data
	// directory says the invite is REDEEMED, by this agent. If the HTTP refusal
	// above ever came from somewhere other than the durable record, this is what
	// would catch it.
	replayed := igeReplayInvites(t, dataDir, blob.BusID)
	rec, ok := replayed.Lookup(blob.InviteID)
	if !ok {
		t.Fatalf("invite %s is not in the table rebuilt from the data directory at all", blob.InviteID)
	}
	if rec.State != invite.StateRedeemed {
		t.Fatalf(`the durable log says invite %s is %s, want redeemed.

The enrolment it authorised is on the roster and authenticating, so the invite
MUST be spent: the two are ONE transaction. If the log records the enrolment
without the consumption, this invite admits another agent on every restart, for
ever.`, blob.InviteID, rec.State)
	}
	if rec.RedeemedBy != invitedAgent.id {
		t.Errorf("the consumption record names agent %q, want %q", rec.RedeemedBy, invitedAgent.id)
	}

	// The secret is a bearer credential and the data directory outlives every
	// process here. It must not be in the log.
	raw := mustReadFile(t, walPathIn(dataDir))
	if strings.Contains(string(raw), blob.InviteSecret) {
		t.Fatalf("the plaintext invite secret is in the bus's write-ahead log")
	}
	// Nor in anything the two servers printed.
	for _, p := range []*serverProc{second, third} {
		if out := strings.Join(p.snapshot(), "\n"); strings.Contains(out, blob.InviteSecret) {
			t.Fatalf("the plaintext invite secret appears in a server's stderr")
		}
	}

	igeStopServer(t, third)
}

// igeAssertInviteRefused presents the already-spent invite again, under a fresh
// idempotency key, and requires the collapsed 403.
//
// A fresh key is what makes this a SECOND REDEMPTION rather than a legitimate
// retry: the same key with the same payload is invariant 10's carve-out and
// would correctly be answered with the ORIGINAL 201.
func igeAssertInviteRefused(t *testing.T, dataDir, addr, pubB64 string, blob inviteBlob, why string) {
	t.Helper()
	status, body := postJSONTo(t, dataDir, addr, "/v1/enroll", "",
		igeEnrolBody("interloper", pubB64, "k-"+why, blob.InviteID, blob.InviteSecret))
	if status != http.StatusForbidden {
		t.Fatalf(`presenting the SPENT invite again (%s) = %d, want 403; body %s

Single use is the property the whole invite mechanism rests on. A second
acceptance means one invite admits two agents.`, why, status, body)
	}
	if !strings.Contains(string(body), "invite not accepted") {
		t.Errorf("the refusal body is %s, want the collapsed \"invite not accepted\"; naming the reason is an oracle for whether an invite exists and what became of it", body)
	}
}

// walPathIn is the write-ahead log inside a data directory. Spelled out here
// rather than borrowed because this file must keep working if a helper of the
// same name is added elsewhere in the package.
func walPathIn(dataDir string) string {
	return dataDir + "/bus.wal"
}

// igeReplayInvites rebuilds the invite table from a data directory THROUGH THE
// MULTIPLEXER, which is the wiring cmd/agent-bus/main.go uses.
//
// It exists instead of the package's replayInvites helper, and the difference is
// the whole subtlety of this record shape rather than a style preference.
// replayInvites opens the log with a BARE *invite.Store as its applier, and
// invite.Store.Apply skips any entry whose Kind is not invite.RecordKind — so a
// COMPOSITE "agent+invite" entry is INVISIBLE to it and an invite spent by an
// invited enrolment reads back as OPEN. Only auth.MultiplexApplier expands the
// composite into its two halves and hands the consumption record to the store.
//
// Written down because the same shape is a live hazard in the tree, not merely
// in a test helper: `agent-bus invite mint` opens the log the bare way
// (cmd/agent-bus/invite.go), so its in-memory table under-counts spent invites.
// That is harmless for minting, which only appends new ids -- but any future
// `invite list` or `invite revoke` on that wiring would report a SPENT invite as
// still live, which is the direction that matters.
func igeReplayInvites(t *testing.T, dataDir, busID string) *invite.Store {
	t.Helper()

	lg := logging.New(io.Discard, logging.LevelError)
	roster := auth.NewWALRoster(lg)
	store, err := invite.NewStore(invite.StoreOptions{BusID: busID, Logger: lg})
	if err != nil {
		t.Fatalf("invite.NewStore: %v", err)
	}
	mux, err := auth.NewMultiplexApplier(lg, map[string]wal.Applier{
		auth.RecordKind:   roster,
		invite.RecordKind: store,
	})
	if err != nil {
		t.Fatalf("auth.NewMultiplexApplier: %v", err)
	}
	log, err := wal.Open(wal.LogOptions{Dir: dataDir, Logger: lg, Applier: mux})
	if err != nil {
		t.Fatalf("wal.Open for replay: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return store
}
