package httpapi_test

// INVITE-GATE, part 4: POST /v1/enroll REDEEMS an invite when one is presented,
// and STILL ACCEPTS an enrolment carrying none.
//
// The route is exercised through the REAL handler over a REAL *auth.Service, a
// REAL *auth.WALRoster and a REAL *invite.Store on a real *wal.Log, because the
// composite two-phase write is the whole point and no double can stand in for
// it: auth.Service.Enrol REFUSES an invited enrolment on any roster that cannot
// co-commit the consumption record (ErrInviteNotAtomic), so a MemoryRoster here
// would test the refusal path and nothing else.
//
// Four properties get the most attention, because each of them is the kind of
// thing that looks fine in review and is wrong in production:
//
//  1. THE UN-INVITED PATH IS UNCHANGED. This build does not GATE enrolment; it
//     redeems an invite when one is offered. Nine agents are enrolled on a live
//     bus and the shipped client cannot present an invite at all, so a route
//     that started refusing them is a total onboarding outage.
//  2. THE REPLAY IS BYTE-IDENTICAL. A legitimate retry must parse a body it
//     cannot distinguish from the original's, and must not consume anything a
//     second time.
//  3. EVERY INVITE REFUSAL IS INDISTINGUISHABLE. Unknown, expired, revoked,
//     already-redeemed, malformed and wrong-secret all produce the SAME status
//     and the SAME bytes. The distinct sentinels exist for the OPERATOR, in the
//     log; the set of ANSWERS would otherwise be an oracle for "does invite X
//     exist" and "is invite X still live", which is precisely what somebody
//     enumerating invite ids wants.
//  4. THE SECRET NEVER ESCAPES. Not in a body, not in a header, not in a log
//     line, on any path — it is a bearer credential and whoever holds it can
//     enrol an agent onto this bus.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// igClock drives the invite store's expiry predicates without a test that
// sleeps. Mutex-guarded because the store calls Now from whatever goroutine is
// inside it and the suite runs under -race.
type igClock struct {
	mu sync.Mutex
	t  time.Time
}

func newIGClock() *igClock {
	return &igClock{t: time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *igClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *igClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// igBus is a server wired the way cmd/agent-bus wires one: a durable roster and
// a durable invite store behind ONE multiplexing applier over a real log.
type igBus struct {
	srv    *httpapi.Server
	store  *invite.Store
	roster *auth.WALRoster
	log    *bytes.Buffer
	clock  *igClock
}

// newIGBus builds the bus. withInvites=false leaves Options.Invites nil, which
// is the "this build does not redeem invites" configuration.
func newIGBus(t *testing.T, withInvites bool) *igBus {
	t.Helper()

	dir := t.TempDir()
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, logging.LevelDebug)
	clock := newIGClock()

	roster := auth.NewWALRoster(logger)
	store, err := invite.NewStore(invite.StoreOptions{BusID: authTestBusID, Logger: logger, Now: clock.Now})
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
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	if err := roster.Attach(lg); err != nil {
		t.Fatalf("roster.Attach: %v", err)
	}
	if err := store.Attach(lg); err != nil {
		t.Fatalf("store.Attach: %v", err)
	}

	minter, err := ids.NewAgentIDMinter(authTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	opts := httpapi.Options{
		Identity: testIdentity(authTestBusID),
		Logger:   logger,
		Auth:     svc,
	}
	if withInvites {
		opts.Invites = store
	}
	return &igBus{srv: httpapi.New(opts), store: store, roster: roster, log: &logBuf, clock: clock}
}

// mint mints an invite the way `agent-bus invite mint` does.
func (b *igBus) mint(t *testing.T, label string) invite.Minted {
	t.Helper()
	m, err := b.store.Mint(invite.MintRequest{Label: label, TTL: time.Hour})
	if err != nil {
		t.Fatalf("minting an invite: %v", err)
	}
	return m
}

// igEnrolBody renders an enrolment request body. An empty inviteID/inviteSecret
// is OMITTED rather than sent as "", so the no-invite case is the wire shape a
// pre-INVITE-GATE client actually produces.
func igEnrolBody(name, pubB64, key, inviteID, inviteSecret string) string {
	fields := []string{
		fmt.Sprintf("%q:%q", "name", name),
		fmt.Sprintf("%q:%q", "public_key", pubB64),
		fmt.Sprintf("%q:%q", "idempotency_key", key),
	}
	if inviteID != "" {
		fields = append(fields, fmt.Sprintf("%q:%q", "invite_id", inviteID))
	}
	if inviteSecret != "" {
		fields = append(fields, fmt.Sprintf("%q:%q", "invite_secret", inviteSecret))
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// ---------------------------------------------------------------------------
// 1. The un-invited path is unchanged
// ---------------------------------------------------------------------------

// TestInviteGateEnrolWithoutAnInviteIsStillAccepted is THE regression of this
// task. InviteRequired is false and must stay false until a separate flip lands
// alongside a client that can present an invite; until then a refusal here is a
// 100% onboarding outage that looks exactly like a working gate.
func TestInviteGateEnrolWithoutAnInviteIsStillAccepted(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	minted := b.mint(t, "an invite nobody presents")

	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-no-invite", "", ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf(`enrol WITHOUT an invite = %d, want 201; body %s

This build REDEEMS an invite when one is presented; it does not REQUIRE one.`, rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	wantKeys(t, body, "agent_id", "bus_id", "name", "enrolled_at")
	if got := body["agent_id"]; got != authTestBusID+".worker-1" {
		t.Errorf("agent_id = %v, want %s.worker-1", got, authTestBusID)
	}

	// Nothing about the un-invited path may touch the invite table.
	if got, _ := b.store.Lookup(minted.ID); got.State != invite.StateOpen {
		t.Fatalf("an un-invited enrolment left invite %s in state %s, want open", minted.ID, got.State)
	}
	// And the roster entry carries NO provenance, because there is none.
	entry, ok := b.roster.Get(authTestBusID + ".worker-1")
	if !ok {
		t.Fatalf("the agent is not on the roster after a 201")
	}
	if entry.InviteID != "" {
		t.Errorf("an un-invited enrolment recorded invite_id %q, want empty", entry.InviteID)
	}
}

// ---------------------------------------------------------------------------
// 2. A presented invite is redeemed, atomically
// ---------------------------------------------------------------------------

// TestInviteGateEnrolRedeemsAPresentedInvite is the positive claim: 201, the
// agent is on the roster with the invite recorded as provenance, and the invite
// is SPENT — in ONE composite entry.
func TestInviteGateEnrolRedeemsAPresentedInvite(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	minted := b.mint(t, "for the deploy runner")

	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-invited", minted.ID, minted.Secret))
	if rec.Code != http.StatusCreated {
		t.Fatalf("enrol WITH a valid invite = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	wantKeys(t, body, "agent_id", "bus_id", "name", "enrolled_at")
	agentID, _ := body["agent_id"].(string)
	if agentID != authTestBusID+".worker-1" {
		t.Fatalf("agent_id = %q, want %s.worker-1", agentID, authTestBusID)
	}
	if rec.Header().Get(httpapi.IdempotencyReplayedHeader) != "" {
		t.Errorf("a FIRST enrolment carries %s; that header means nothing was applied", httpapi.IdempotencyReplayedHeader)
	}

	// THE INVITE IS SPENT.
	got, ok := b.store.Lookup(minted.ID)
	if !ok {
		t.Fatalf("invite %s vanished from the table", minted.ID)
	}
	if got.State != invite.StateRedeemed {
		t.Fatalf("invite %s is %s after a successful invited enrolment, want redeemed: a gate that does not spend the invite is decorative", minted.ID, got.State)
	}
	if got.RedeemedBy != agentID {
		t.Errorf("the consumption record names agent %q, want %q", got.RedeemedBy, agentID)
	}
	if !bytes.Equal(got.Result, bytes.TrimRight(rec.Body.Bytes(), "\n")) {
		t.Errorf(`the stored result is not the 201 body.
  stored %s
  sent   %s
A retry can only be answered with the ORIGINAL response if the ORIGINAL response
is what was stored.`, got.Result, bytes.TrimRight(rec.Body.Bytes(), "\n"))
	}

	// PROVENANCE on the roster entry.
	entry, ok := b.roster.Get(agentID)
	if !ok {
		t.Fatalf("the agent is not on the roster")
	}
	if entry.InviteID != minted.ID {
		t.Errorf("the roster entry records invite_id %q, want %q", entry.InviteID, minted.ID)
	}
}

// ---------------------------------------------------------------------------
// 3. The replay
// ---------------------------------------------------------------------------

// TestInviteGateReplayIsByteIdenticalAndConsumesNothing is invariant 10's
// legitimate-retry carve-out on this route.
//
// BYTE for byte, because a client must not be able to tell a retry's answer from
// the original's by parsing it — the fact that it was a replay is carried out of
// band, in a header, for operators and clients that care.
func TestInviteGateReplayIsByteIdenticalAndConsumesNothing(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	minted := b.mint(t, "for the retrying client")
	reqBody := igEnrolBody("worker", pubB64, "k-retry", minted.ID, minted.Secret)

	first := postJSON(t, b.srv, httpapi.RouteEnroll, reqBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("the original enrol = %d, want 201; body %s", first.Code, first.Body.String())
	}
	spent, _ := b.store.Lookup(minted.ID)

	second := postJSON(t, b.srv, httpapi.RouteEnroll, reqBody)
	if second.Code != http.StatusCreated {
		t.Fatalf("the RETRY = %d, want 201: the response to a retry is the response to the original, status included; body %s", second.Code, second.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf(`the retry's body differs from the original's.
  original %q
  retry    %q
A retry must not be able to tell the difference in the payload it parses.`, first.Body.String(), second.Body.String())
	}
	if got := second.Header().Get(httpapi.IdempotencyReplayedHeader); got != "true" {
		t.Errorf("the retry carries %s=%q, want \"true\": the out-of-band signal is the ONLY way a client learns nothing was re-applied", httpapi.IdempotencyReplayedHeader, got)
	}
	if got := second.Header().Get("Connection"); strings.EqualFold(got, "close") {
		t.Errorf("the retry was answered with Connection: close; a LEGITIMATE retry is never punished (invariant 10)")
	}

	// NOTHING WAS RE-APPLIED. Same consumption record, same agent, one agent on
	// the roster.
	after, _ := b.store.Lookup(minted.ID)
	if after.RedeemedBy != spent.RedeemedBy || !after.RedeemedAt.Equal(spent.RedeemedAt) {
		t.Fatalf("the retry rewrote the consumption record: was redeemed_by=%q at %s, now %q at %s", spent.RedeemedBy, spent.RedeemedAt, after.RedeemedBy, after.RedeemedAt)
	}
	if n := b.roster.Len(); n != 1 {
		t.Fatalf("the roster holds %d agents after one enrolment and one retry, want 1: the retry minted a SECOND id for one agent", n)
	}
}

// TestInviteGateSameKeyDifferentPayloadIs409AndKeepsTheConnection: same key +
// DIFFERENT payload is a protocol violation, not a retry.
//
// THE CONNECTION IS KEPT (invariant 10, NARROWED 2026-08-08). /v1/enroll is
// UNAUTHENTICATED, so the socket identifies NO principal to punish — whoever
// owns it need not be whoever sent the request — and dropping it destroys every
// other request pipelined there, hitting an honest client part-way through
// obtaining a credential. A merely BUGGY client reaches this line easily.
func TestInviteGateSameKeyDifferentPayloadIs409AndKeepsTheConnection(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	_, _, otherPubB64 := newAuthKeypair(t)
	minted := b.mint(t, "for the buggy client")

	if rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-reused", minted.ID, minted.Secret)); rec.Code != http.StatusCreated {
		t.Fatalf("the original enrol = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"a different public key", igEnrolBody("worker", otherPubB64, "k-reused", minted.ID, minted.Secret)},
		{"a different name", igEnrolBody("other", pubB64, "k-reused", minted.ID, minted.Secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, b.srv, httpapi.RouteEnroll, tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("reusing the key with %s = %d, want 409; body %s", tc.name, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Connection"); strings.EqualFold(got, "close") {
				t.Fatalf(`the 409 carried "Connection: close".

Invariant 10 was NARROWED on 2026-08-08: same key + different payload is
REJECTED AND LOGGED with the connection KEPT. Only replay of an already-accepted
SIGNED message disconnects. This route is unauthenticated, so the socket
identifies no principal to punish and the party dropped is simply whoever owns
it.`)
			}
			if n := b.roster.Len(); n != 1 {
				t.Fatalf("the refused call enrolled somebody (%d agents on the roster, want 1)", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Indistinguishability — the anti-enumeration property
// ---------------------------------------------------------------------------

// TestInviteGateEveryInviteRefusalIsIndistinguishable asserts the collapse
// DIRECTLY: every reason an invite can be refused produces the same status AND
// the same bytes.
//
// This is not tidiness. The set of ANSWERS is otherwise an oracle: "unknown"
// versus "expired" tells a caller holding no valid credential that invite X
// EXISTS, and "revoked" versus "already redeemed" tells it what happened to it.
// An enumerator wants exactly those two bits.
//
// The SENTINELS still differ, and must: they go to the operator's log, which is
// where the difference is safe and useful.
func TestInviteGateEveryInviteRefusalIsIndistinguishable(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)

	// Each case gets its OWN invite, so no case can perturb another's.
	expired := b.mint(t, "expired")
	revoked := b.mint(t, "revoked")
	spent := b.mint(t, "spent")

	if _, err := b.store.Revoke(revoked.ID, "leaked in a paste bin"); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	if rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("first", pubB64, "k-spend", spent.ID, spent.Secret)); rec.Code != http.StatusCreated {
		t.Fatalf("spending an invite for the already-redeemed case = %d; body %s", rec.Code, rec.Body.String())
	}
	// Past the first three invites' TTL, but well inside SpentRetention, so
	// every record is still in the table and each refusal below is a STATE
	// decision rather than a lookup miss dressed up as one.
	b.clock.Advance(2 * time.Hour)

	// Minted AFTER the jump, so it is genuinely OPEN and UNSPENT: the two
	// wrong-secret rows must be refused for the secret and not for the state, or
	// they would be testing the expired row twice.
	live := b.mint(t, "live")
	if got, _ := b.store.Lookup(live.ID); got.State != invite.StateOpen || got.Expired(b.clock.Now()) {
		t.Fatalf("the live fixture is %s / expired=%v; the wrong-secret rows need an invite that would otherwise be accepted", got.State, got.Expired(b.clock.Now()))
	}

	cases := []struct {
		name   string
		id     string
		secret string
	}{
		{"an unknown invite id", "inv-aaaaaaaaaaaaaaaa", live.Secret},
		{"a malformed invite id", "inv-NOT-BASE32!!", live.Secret},
		{"an oversized invite id", "inv-" + strings.Repeat("a", 200), live.Secret},
		{"an expired invite", expired.ID, expired.Secret},
		{"a revoked invite", revoked.ID, revoked.Secret},
		{"an already-redeemed invite", spent.ID, spent.Secret},
		{"the wrong secret for a live invite", live.ID, flipIGSecret(live.Secret)},
		{"an empty-ish secret for a live invite", live.ID, "x"},
	}

	var wantStatus int
	var wantBody string
	for i, tc := range cases {
		rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, fmt.Sprintf("k-refuse-%d", i), tc.id, tc.secret))
		if i == 0 {
			wantStatus, wantBody = rec.Code, rec.Body.String()
			if wantStatus != http.StatusForbidden {
				t.Fatalf("a refused invite = %d, want 403", wantStatus)
			}
			var errBody map[string]interface{}
			if err := json.Unmarshal([]byte(wantBody), &errBody); err != nil {
				t.Fatalf("the refusal body is not JSON: %v (%s)", err, wantBody)
			}
			if errBody["error"] != "invite not accepted" {
				t.Fatalf("the refusal says %q; the collapsed answer must not name the reason", errBody["error"])
			}
			continue
		}
		if rec.Code != wantStatus || rec.Body.String() != wantBody {
			t.Errorf(`%s answers differently from %q.
  got  %d %q
  want %d %q
The set of answers is an ORACLE for "does invite X exist" and "is invite X still
live". The distinguishing detail belongs in the operator's log, never on the
wire.`, tc.name, cases[0].name, rec.Code, rec.Body.String(), wantStatus, wantBody)
		}
	}

	// Nothing above enrolled anybody beyond the one deliberate spend.
	if n := b.roster.Len(); n != 1 {
		t.Fatalf("the roster holds %d agents after eight refusals and one success, want 1", n)
	}
}

// flipIGSecret returns a secret of the same length that is not the real one.
func flipIGSecret(s string) string {
	if s == "" {
		return "x"
	}
	last := s[len(s)-1]
	if last == 'A' {
		return s[:len(s)-1] + "B"
	}
	return s[:len(s)-1] + "A"
}

// ---------------------------------------------------------------------------
// 5. Malformed presentations
// ---------------------------------------------------------------------------

// TestInviteGateHalfAnInviteIs400: an id without a secret, or a secret without
// an id, is REFUSED rather than treated as no invite at all.
//
// Quietly enrolling such a client WITHOUT the invite would leave it believing
// its single-use credential had been spent.
func TestInviteGateHalfAnInviteIs400(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	minted := b.mint(t, "half presented")

	for _, tc := range []struct {
		name   string
		id     string
		secret string
	}{
		{"an invite id with no secret", minted.ID, ""},
		{"an invite secret with no id", "", minted.Secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-half", tc.id, tc.secret))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400; body %s", tc.name, rec.Code, rec.Body.String())
			}
			if b.roster.Len() != 0 {
				t.Fatalf("%s enrolled an agent anyway", tc.name)
			}
			if got, _ := b.store.Lookup(minted.ID); got.State != invite.StateOpen {
				t.Fatalf("%s changed the invite's state to %s", tc.name, got.State)
			}
		})
	}
}

// TestInviteGateAnInvalidIdempotencyKeyWithAnInviteIs400 pins the pre-check that
// keeps a CLIENT error from being reported as a SERVER one.
//
// invite.Begin rejects a malformed key as ErrInvalidRecord, which the invite
// error mapping sends to 500. Validating the key before Begin keeps the two
// paths — invited and un-invited — agreeing about which keys are acceptable.
func TestInviteGateAnInvalidIdempotencyKeyWithAnInviteIs400(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	minted := b.mint(t, "with a bad key")

	withInvite := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "not a valid key!", minted.ID, minted.Secret))
	if withInvite.Code != http.StatusBadRequest {
		t.Fatalf("an invalid idempotency key WITH an invite = %d, want 400 (a client error must never be reported as a 500); body %s", withInvite.Code, withInvite.Body.String())
	}
	withoutInvite := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "not a valid key!", "", ""))
	if withoutInvite.Code != withInvite.Code {
		t.Errorf("the same invalid key is %d with an invite and %d without; the two paths must agree about which keys are acceptable", withInvite.Code, withoutInvite.Code)
	}
	if got, _ := b.store.Lookup(minted.ID); got.State != invite.StateOpen {
		t.Fatalf("the refused call moved the invite to %s", got.State)
	}
}

// TestInviteGateAPresentedInviteIs501WhenThisBuildHasNoStore: a bus built with
// Options.Invites nil answers 501 rather than silently enrolling without the
// invite.
//
// A silent ignore is the dangerous answer: the client walks away believing its
// single-use credential was spent, and the operator believes the invite was
// used. 501 says exactly what happened.
func TestInviteGateAPresentedInviteIs501WhenThisBuildHasNoStore(t *testing.T) {
	b := newIGBus(t, false) // Options.Invites == nil
	_, _, pubB64 := newAuthKeypair(t)

	rec := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-501", "inv-aaaaaaaaaaaaaaaa", "some-secret"))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("presenting an invite to a build with no invite store = %d, want 501; body %s", rec.Code, rec.Body.String())
	}
	if b.roster.Len() != 0 {
		t.Fatalf("the 501 enrolled the agent anyway; a client must never walk away believing its invite was spent when it was not")
	}

	// And the same build still accepts an UN-INVITED enrolment: 501 is about the
	// invite, not about enrolment.
	if ok := postJSON(t, b.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-no-invite", "", "")); ok.Code != http.StatusCreated {
		t.Fatalf("a build with no invite store refused an UN-INVITED enrolment: %d; body %s", ok.Code, ok.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 6. The secret never escapes
// ---------------------------------------------------------------------------

// TestInviteGateTheInviteSecretNeverEscapes sweeps EVERY path — success,
// replay, key reuse, each refusal, the 400s and the 501 — and greps the response
// body, every response header and the whole debug-level server log for the
// plaintext secret.
//
// The secret is a bearer credential: whoever holds it can enrol an agent onto
// this bus. It gets the same discipline a session token gets.
func TestInviteGateTheInviteSecretNeverEscapes(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)

	live := b.mint(t, "live")
	revoked := b.mint(t, "revoked")
	if _, err := b.store.Revoke(revoked.ID, "leaked"); err != nil {
		t.Fatalf("revoking: %v", err)
	}
	secrets := map[string]string{"live": live.Secret, "revoked": revoked.Secret}

	reqs := []struct {
		name string
		body string
	}{
		{"a successful redemption", igEnrolBody("worker", pubB64, "k-ok", live.ID, live.Secret)},
		{"its legitimate retry", igEnrolBody("worker", pubB64, "k-ok", live.ID, live.Secret)},
		{"the same key with a different payload", igEnrolBody("other", pubB64, "k-ok", live.ID, live.Secret)},
		{"an already-redeemed invite", igEnrolBody("worker", pubB64, "k-again", live.ID, live.Secret)},
		{"a revoked invite", igEnrolBody("worker", pubB64, "k-revoked", revoked.ID, revoked.Secret)},
		{"an unknown invite", igEnrolBody("worker", pubB64, "k-unknown", "inv-aaaaaaaaaaaaaaaa", live.Secret)},
		{"half an invite", igEnrolBody("worker", pubB64, "k-half", "", live.Secret)},
		{"an invalid idempotency key", igEnrolBody("worker", pubB64, "not valid!", live.ID, live.Secret)},
	}

	for _, req := range reqs {
		rec := postJSON(t, b.srv, httpapi.RouteEnroll, req.body)
		igAssertNoSecret(t, req.name+" (response body)", rec.Body.String(), secrets)
		for k, vs := range rec.Header() {
			igAssertNoSecret(t, req.name+" (header "+k+")", strings.Join(vs, " "), secrets)
		}
	}

	igAssertNoSecret(t, "the server log", b.log.String(), secrets)

	// A 501 build gets the same sweep: it is the one path that logs the invite
	// id at INFO, so it is the most likely place for the secret to follow it.
	nb := newIGBus(t, false)
	rec := postJSON(t, nb.srv, httpapi.RouteEnroll, igEnrolBody("worker", pubB64, "k-501", live.ID, live.Secret))
	igAssertNoSecret(t, "the 501 body", rec.Body.String(), secrets)
	igAssertNoSecret(t, "the 501 build's log", nb.log.String(), secrets)
}

// igAssertNoSecret fails if any plaintext secret appears in text.
func igAssertNoSecret(t *testing.T, where, text string, secrets map[string]string) {
	t.Helper()
	for name, s := range secrets {
		if s == "" {
			t.Fatalf("the %s fixture has an empty secret, so this check proves nothing", name)
		}
		if strings.Contains(text, s) {
			t.Errorf(`the %s invite SECRET appears in %s.

It is a bearer credential: whoever holds it can enrol an agent onto this bus. It
belongs nowhere but the operator's hand.`, name, where)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. Method and shape, on the invite fields specifically
// ---------------------------------------------------------------------------

// TestInviteGateInviteFieldsAreStrictlyDecoded: the invite fields ride the same
// strict decoder as the rest of the body, so a misspelling is an error rather
// than a silent un-invited enrolment.
func TestInviteGateInviteFieldsAreStrictlyDecoded(t *testing.T) {
	b := newIGBus(t, true)
	_, _, pubB64 := newAuthKeypair(t)
	minted := b.mint(t, "misspelled")

	body := fmt.Sprintf(`{"name":"worker","public_key":%q,"idempotency_key":"k-typo","invite":%q,"invite_secret":%q}`, pubB64, minted.ID, minted.Secret)
	rec := postJSON(t, b.srv, httpapi.RouteEnroll, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf(`a MISSPELLED invite field = %d, want 400; body %s

Unknown fields are rejected precisely so a client that misspells "invite_id"
gets an error rather than silently enrolling with no invite at all.`, rec.Code, rec.Body.String())
	}
	if got, _ := b.store.Lookup(minted.ID); got.State != invite.StateOpen {
		t.Fatalf("the rejected request moved the invite to %s", got.State)
	}
}

// TestInviteGateEnrolStillRefusesNonPOST is a guard against the invite work
// having disturbed the route's method handling: the checks run BEFORE any of the
// invite logic, and a regression there would be reachable without a credential.
func TestInviteGateEnrolStillRefusesNonPOST(t *testing.T) {
	b := newIGBus(t, true)
	rec := doRequest(t, b.srv, http.MethodGet, httpapi.RouteEnroll, "", "application/json")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/enroll = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

// ---------------------------------------------------------------------------
// 7. Log volume on the unauthenticated route
// ---------------------------------------------------------------------------

// TestInviteGateAMalformedInviteIDIsNeverEchoedIntoTheLog pins the property
// inviteIDLogFields exists for, and it is deliberately BEHAVIOURAL rather than
// a unit test of that helper.
//
// The helper is unexported and covers only the "invite_id" FIELD. The hole both
// gates found independently was the OTHER field on the same line: every
// writeInviteError record also carries "err", and invite.ValidateInviteID's
// malformed-id branch used to quote the offending id straight into it — so the
// raw value reached the log through a path the helper never touched. A unit
// test of the helper would have passed throughout and proved nothing.
//
// So this drives the REAL route and greps the WHOLE debug-level log for the
// attacker-chosen bytes. It goes red if either half regresses: the helper
// logging a raw invalid id, or any sentinel on this path starting to echo one.
//
// It is about VOLUME, not escaping: logging.writeValue already quotes every
// value, so these bytes are safe to write. /v1/enroll needs no credential, this
// server rate-limits nothing, and MaxAuthRequestBytes lets an anonymous caller
// choose ~1 KiB of them per cheap request.
func TestInviteGateAMalformedInviteIDIsNeverEchoedIntoTheLog(t *testing.T) {
	_, _, pubB64 := newAuthKeypair(t)

	// A needle that cannot occur by accident and is a legal JSON string, so it
	// reaches validation as itself rather than being rejected by the decoder.
	const needle = "ZZQQXXmalformedinviteidneedleXXQQZZ"

	for _, tc := range []struct {
		name        string
		withInvites bool
	}{
		{"a build that redeems invites", true},
		{"a build with no invite store (the 501 path)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newIGBus(t, tc.withInvites)
			live := b.mint(t, "live")

			// Every shape of INVALID id, all short enough to dodge the oversized
			// branch that already refused to echo. Each must be refused, and none
			// may put its bytes in the log.
			ids := []string{
				needle,                 // not the invite id shape at all
				"inv-" + needle,        // right prefix, illegal body
				"inv-" + needle + "!!", // illegal characters
			}
			for i, id := range ids {
				rec := postJSON(t, b.srv, httpapi.RouteEnroll,
					igEnrolBody("worker", pubB64, fmt.Sprintf("k-echo-%d", i), id, live.Secret))
				if rec.Code == http.StatusCreated {
					t.Fatalf("a malformed invite id %q was ACCEPTED (%d); this test proves nothing if the id is not refused", id, rec.Code)
				}
				if strings.Contains(rec.Body.String(), needle) {
					t.Errorf("the malformed invite id is echoed in the RESPONSE BODY: %s", rec.Body.String())
				}
			}

			if got := b.log.String(); strings.Contains(got, needle) {
				t.Errorf(`a client-supplied MALFORMED invite id was echoed into the server log.

/v1/enroll is UNAUTHENTICATED and nothing here is rate-limited, so an anonymous
caller choosing these bytes chooses the server's log volume with them. A valid
id is logged in full on purpose (an operator correlating a refusal needs it, and
it is bounded by invite.MaxInviteIDLen); an INVALID one must appear only as
invite_id_len.

Check BOTH halves: httpapi.inviteIDLogFields for the "invite_id" field, and
whichever sentinel is being wrapped into "err" — invite.ValidateInviteID's
malformed-id branch is the one that regressed before.

log follows:
%s`, got)
			}

			// The bound the fix is worth: a valid id IS still logged, so the
			// refusal stays diagnosable. Without this the whole property could be
			// satisfied by logging nothing at all.
			rec := postJSON(t, b.srv, httpapi.RouteEnroll,
				igEnrolBody("worker", pubB64, "k-valid-id", live.ID, flipIGSecret(live.Secret)))
			if rec.Code == http.StatusCreated {
				t.Fatalf("a wrong secret was ACCEPTED (%d)", rec.Code)
			}
			if tc.withInvites && !strings.Contains(b.log.String(), live.ID) {
				t.Errorf("a VALID invite id is not in the log; the refusal is no longer diagnosable, which is not what the volume fix was for")
			}
		})
	}
}
