package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// newAckStatusServer builds a server that serves GET /v1/ack/ and the credential
// routes, and nothing else.
//
// It deliberately wires NO HUB. That is the property being asserted as much as
// a convenience: the status route reads a durable table, not the messaging
// surface, so a bus whose hub is missing must still answer for messages it
// already accepted. If this ever starts 503-ing, the route has been coupled to
// the wrong dependency.
//
// The WAL lives under t.TempDir(): NEVER the tracked data/ dir.
func newAckStatusServer(t *testing.T) (*httpapi.Server, *ack.Store) {
	t.Helper()

	dir := t.TempDir()
	logger := logging.New(&bytes.Buffer{}, logging.LevelDebug)

	walLog, err := wal.Open(wal.LogOptions{Dir: dir, Logger: logger})
	if err != nil {
		t.Fatalf("opening the write-ahead log in %q: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := walLog.Close(); err != nil {
			t.Errorf("closing the write-ahead log: %v", err)
		}
	})

	minter, err := ids.NewAgentIDMinter(msgTestBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: auth.NewMemoryRoster()})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	acks := ack.NewStore(ack.Options{Logger: logger})
	if err := acks.Attach(walLog); err != nil {
		t.Fatalf("attaching the ack lifecycle table: %v", err)
	}

	srv := httpapi.New(httpapi.Options{
		Identity:  testIdentity(msgTestBusID),
		Logger:    logger,
		Durable:   walLog,
		Auth:      svc,
		AckStatus: acks,
	})
	return srv, acks
}

// ackStatusPath is the route under test with a key appended, spelled once so a
// test cannot drift from the constant the server registers.
func ackStatusPath(key string) string { return httpapi.RouteAckStatus + key }

// decodeAckRows reads the §13.2 rows out of a response.
func decodeAckRows(t *testing.T, rec *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	var body struct {
		Rows []map[string]interface{} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the status body is not JSON (%v): %s", err, rec.Body.String())
	}
	return body.Rows
}

// TestAckStatusUniformAnswer is the §13.3 guard, and it is the reason this route
// exists in its own file with its own server.
//
//	"Only the ORIGINAL SENDER may read a row. Every other case — key never
//	 existed, key swept, key belongs to someone else — returns the SAME
//	 answer: 200 with state: unknown."
//
// It asserts BYTE EQUALITY of the whole response body across the four cases,
// not merely "all of them are 200". A body that differed by a field, by field
// ORDER, or by echoing the caller's input back would be an existence oracle
// written in a different alphabet, and a status-code-only assertion would pass
// over every one of those.
//
// # MUTATION PROOF
//
// Each of these makes it FAIL, and each was run:
//   - answering 403 when the row belongs to another sender;
//   - answering 404 when the key never existed;
//   - answering 400 on a malformed key;
//   - echoing the caller's key into the unknown row.
func TestAckStatusUniformAnswer(t *testing.T) {
	srv, acks := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")
	stranger := enrolAndAuthenticate(t, srv, "stranger")

	const key = "bus-msg-test-41"
	if err := acks.Accept(key, owner.id, msgTestBusID+".recipient-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// The sender CAN see it — otherwise every assertion below would be about a
	// route that shows nobody anything, which is trivially uniform and useless.
	own := authed(t, srv, owner, http.MethodGet, ackStatusPath(key), "")
	if own.Code != http.StatusOK {
		t.Fatalf("the sender's own status read = %d, want 200; body %s", own.Code, own.Body.String())
	}
	rows := decodeAckRows(t, own)
	if len(rows) != 1 || rows[0]["state"] != "accepted" {
		t.Fatalf("the sender's own read returned %v, want one accepted row", rows)
	}

	cases := []struct {
		name string
		key  string
	}{
		{"a key that never existed", "bus-msg-test-999"},
		{"a malformed key", "not-a-message-id-at-all"},
		{"an empty key", ""},
		{"a key that is somebody else's", key},
	}
	var want string
	for i, tc := range cases {
		rec := authed(t, srv, stranger, http.MethodGet, ackStatusPath(tc.key), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200; a status that is not 200 confirms which keys exist", tc.name, rec.Code)
		}
		got := rec.Body.String()
		if i == 0 {
			want = got
			if len(decodeAckRows(t, rec)) != 1 {
				t.Fatalf("the unknown answer carries %d rows, want exactly 1", len(decodeAckRows(t, rec)))
			}
			if decodeAckRows(t, rec)[0]["state"] != "unknown" {
				t.Fatalf("the unknown answer is %s, want state \"unknown\"", got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s answered\n  %s\nbut a key that never existed answered\n  %s\n§13.3 requires them to be the SAME answer; a difference is an existence oracle", tc.name, got, want)
		}
	}
}

// TestAckStatusRequiresAuthentication: the route is protected by being
// REGISTERED, because authMiddleware is default-deny and this pattern is not on
// the allow-list. An anonymous caller able to read delivery status would be an
// existence oracle over every message on the bus.
//
// MUTATION: adding RouteAckStatus to unauthenticatedRoutes makes this FAIL with
// 200 where 401 was wanted.
func TestAckStatusRequiresAuthentication(t *testing.T) {
	srv, _ := newAckStatusServer(t)
	// The BARE subtree path is included deliberately. unauthenticatedRoutes is
	// matched EXACTLY (authmw.go), so "/v1/ack/" is the one spelling an
	// allow-list entry for this route could ever reach — a test that probed
	// only "/v1/ack/<key>" would still pass with the route allow-listed, and
	// would be a guard that could not fire.
	for _, path := range []string{ackStatusPath("bus-msg-test-1"), httpapi.RouteAckStatus} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("an anonymous status read of %s = %d, want 401; body %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestAckStatusNotRegisteredWithoutATable: a route that exists and refuses is a
// claim the surface is there. With no lifecycle table the path must 404 through
// the catch-all like any other path this build does not serve — the same posture
// Options.Hub and Options.Auth take.
func TestAckStatusNotRegisteredWithoutATable(t *testing.T) {
	srv := httpapi.New(httpapi.Options{Identity: testIdentity(msgTestBusID)})
	for _, r := range srv.Routes() {
		if r == httpapi.RouteAckStatus {
			t.Fatalf("a server built with no ack lifecycle table registered %s anyway", r)
		}
	}
}

// TestAckStatusRendersTheTerminalRow pins the §13.2 shape of a settled row, and
// pins what it MUST NOT carry.
//
// The negative half is the load-bearing half: §13.3 forbids disclosing the
// traversed bus_path, the peer bus that refused, the recipient's poll activity
// or the roster. It also carries NO sender field — the sender already knows who
// it is — and NO free-text reason, because a reason string in a durable trail is
// a message body by another name (invariant 6).
func TestAckStatusRendersTheTerminalRow(t *testing.T) {
	srv, acks := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")

	const key = "bus-msg-test-77"
	recipient := msgTestBusID + ".recipient-1"
	if err := acks.Accept(key, owner.id, recipient); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := acks.Settle(key, recipient, ack.StateUndeliverable, ack.ClassNoRoute, ack.AttestedByPeerBus); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	rec := authed(t, srv, owner, http.MethodGet, ackStatusPath(key), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status read = %d, want 200", rec.Code)
	}
	rows := decodeAckRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	for field, want := range map[string]interface{}{
		"correlation_key": key,
		"recipient":       recipient,
		"state":           "undeliverable",
		"class":           "no_route",
		"attested_by":     "peer_bus",
	} {
		if row[field] != want {
			t.Errorf("row[%q] = %v, want %v", field, row[field], want)
		}
	}
	if row["settled_at"] == nil || row["accepted_at"] == nil {
		t.Errorf("a terminal row must carry accepted_at and settled_at; got %v", row)
	}
	for _, forbidden := range []string{"sender", "bus_path", "peer", "peer_bus", "reason", "body", "message", "polled_at", "hops"} {
		if _, present := row[forbidden]; present {
			t.Errorf("the status row carries %q; §13.3 forbids disclosing anything about the federation, the recipient's activity or the payload", forbidden)
		}
	}
}

// TestAckStatusWaitParksOnUnknown is the TIMING half of §13.3, and it is the one
// an implementation is most likely to get wrong by trying to be helpful.
//
// The obvious optimisation — return at once when there is nothing to show —
// rebuilds the oracle out of TIME: an immediate answer would mean "no such row"
// and a parked one would mean "a row exists and has not settled", so a prober
// would read existence straight off the latency. A wait on an unknown key must
// therefore take as long as a wait on a live non-terminal row.
//
// MUTATION: making parkForAckSettlement return early on an empty row set makes
// this FAIL — the unknown request comes back in microseconds.
func TestAckStatusWaitParksOnUnknown(t *testing.T) {
	srv, acks := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")

	// A live, NON-terminal row: the case a wait is genuinely for.
	const live = "bus-msg-test-11"
	if err := acks.Accept(live, owner.id, msgTestBusID+".recipient-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	timed := func(key string) time.Duration {
		start := time.Now()
		rec := authed(t, srv, owner, http.MethodGet, ackStatusPath(key)+"?wait=1", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("waiting status read of %q = %d, want 200", key, rec.Code)
		}
		return time.Since(start)
	}

	// 1s is the smallest the route accepts (whole seconds, like every other
	// parked poll). Both must approach it; a floor of 700ms leaves room for a
	// slow CI box without letting an immediate return through.
	const floor = 700 * time.Millisecond
	if d := timed(live); d < floor {
		t.Fatalf("a wait on a live non-terminal row returned after %s, want at least %s — it did not park at all", d, floor)
	}
	if d := timed("bus-msg-test-4242"); d < floor {
		t.Fatalf("a wait on an UNKNOWN key returned after %s, want at least %s; returning early leaks existence through timing, which is the oracle §13.3 closes", d, floor)
	}
}

// TestAckStatusWaitReturnsOnSettlement: a parked request must be released by a
// terminal outcome, not only by its deadline. Otherwise --wait is a sleep.
func TestAckStatusWaitReturnsOnSettlement(t *testing.T) {
	srv, acks := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")

	const key = "bus-msg-test-22"
	recipient := msgTestBusID + ".recipient-1"
	if err := acks.Accept(key, owner.id, recipient); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := acks.Settle(key, recipient, ack.StateDelivered, "", ack.AttestedByRecipientSignatureUnverified); err != nil {
			t.Errorf("Settle: %v", err)
		}
	}()

	start := time.Now()
	rec := authed(t, srv, owner, http.MethodGet, ackStatusPath(key)+"?wait=30", "")
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("waiting status read = %d, want 200", rec.Code)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the parked request took %s to notice a terminal outcome; it is not being released by the settlement", elapsed)
	}
	rows := decodeAckRows(t, rec)
	if len(rows) != 1 || rows[0]["state"] != "delivered" {
		t.Fatalf("after settlement the wait returned %v, want one delivered row", rows)
	}
}

// TestAckStatusRefusesAnOutOfRangeWait: an out-of-range wait is REFUSED rather
// than silently clamped, exactly as /v1/wait refuses one. A caller that asked
// for an hour and was quietly given five minutes would conclude the server had
// dropped its request.
//
// A 400 here discloses nothing about any message: it judges the caller's own
// parameter, which is why it is the one refusal this route may make.
func TestAckStatusRefusesAnOutOfRangeWait(t *testing.T) {
	srv, _ := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")
	for _, raw := range []string{"0", "-1", "notanumber", "301"} {
		rec := authed(t, srv, owner, http.MethodGet, ackStatusPath("bus-msg-test-1")+"?wait="+raw, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?wait=%s = %d, want 400", raw, rec.Code)
		}
	}
	// The ceiling ITSELF is accepted, so the refusal above is a bound and not
	// an off-by-one that refuses everything. The request is abandoned after
	// 200ms rather than parked for the full five minutes: a 400 is written
	// IMMEDIATELY when the parameter is refused, so an unwritten status is
	// proof it was accepted. The handler notices the cancelled context and
	// writes nothing, which leaves the recorder at its default 200.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, ackStatusPath("bus-msg-test-1")+"?wait=300", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+owner.token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Errorf("?wait=300 (the 5-minute ceiling) was refused; the bound is off by one")
	}
}

// TestAckStatusCapsParkedWaitsPerAgent: this route parks even on an unknown key
// (it must — see TestAckStatusWaitParksOnUnknown), so a probe costs the caller
// nothing and is guaranteed to hold a connection, a goroutine and a share of
// ack.Store's global mutex for the full ceiling. Without a cap, one authenticated
// agent can hold arbitrarily many, and the mutex it wakes onto every tick is the
// same one Accept takes inside Hub.publish — so the cost lands on every writer
// on the bus. /v1/wait bounds the equivalent at hub.MaxWaitersPerAgent; this is
// the same bound for the same reason.
//
// The refusal must NOT depend on the key: it is decided from the principal's own
// parked count, so it says something about the caller's concurrency and nothing
// about any message.
//
// MUTATION: deleting the s.ackWaiters.acquire block from handleAckStatus makes
// this FAIL — the 33rd concurrent wait is admitted and parks.
//
// # WHAT THIS TEST CANNOT SEE, AND WHY ITS NAME OVERSTATES IT
//
// Every request here — all 32 fillers AND the prober — is issued by the SINGLE
// principal `owner`. One principal cannot tell a per-principal bucket from a
// global one: both refuse the 33rd request from the same agent, so this test is
// GREEN when handleAckStatus keys the bucket on a constant instead of on the
// authenticated sender, which is a cross-agent denial of service. The word
// "PerAgent" in the name is a claim this FIXTURE is structurally unable to make.
// TestAckStatusParkedWaitCapIsPerPrincipal is the one that makes it; do not
// delete it on the grounds that this one already covers the cap.
func TestAckStatusCapsParkedWaitsPerAgent(t *testing.T) {
	srv, _ := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")

	// The fillers and the convergence loop live in parkAckWaitsUntilCapped, so
	// this test and TestAckStatusParkedWaitCapIsPerPrincipal cannot drift apart
	// on the one part of the fixture that is genuinely delicate.
	over, stop := parkAckWaitsUntilCapped(t, srv, owner)
	defer stop()

	if over.Header().Get("Retry-After") == "" {
		t.Error("the refusal carries no Retry-After; a client cannot tell when to try again")
	}
	// THE REFUSAL MUST NOT NAME THE KEY, or the cap becomes a channel for the
	// oracle the rest of this file closes.
	if strings.Contains(over.Body.String(), ackProbeKey) {
		t.Errorf("the refusal echoes the correlation key: %s", over.Body.String())
	}

	// A NON-waiting read is unaffected: the cap bounds parking, not reading.
	if snap := authed(t, srv, owner, http.MethodGet, ackStatusPath(ackProbeKey), ""); snap.Code != http.StatusOK {
		t.Errorf("a snapshot read while at the wait cap = %d, want 200", snap.Code)
	}

	stop()

	// And the slots are RELEASED when the parked requests end, so the cap is a
	// concurrency bound and not a lifetime quota.
	if again := authed(t, srv, owner, http.MethodGet, ackStatusPath("bus-msg-test-2001")+"?wait=1", ""); again.Code != http.StatusOK {
		t.Errorf("after the parked requests ended, a wait = %d, want 200; slots are not being released", again.Code)
	}
}

// TestAckStatusKeyLengthBoundIsInvisible: maxAckKeyBytes stops the work, it does
// not change the answer. An over-long key must be byte-identical to any other
// unknown one, or the bound itself becomes a signal.
func TestAckStatusKeyLengthBoundIsInvisible(t *testing.T) {
	srv, _ := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")

	short := authed(t, srv, owner, http.MethodGet, ackStatusPath("bus-msg-test-4242"), "")
	long := authed(t, srv, owner, http.MethodGet, ackStatusPath(strings.Repeat("a", 4096)), "")
	if short.Code != http.StatusOK || long.Code != http.StatusOK {
		t.Fatalf("statuses = %d and %d, want 200 and 200", short.Code, long.Code)
	}
	if short.Body.String() != long.Body.String() {
		t.Errorf("an over-long key answered\n  %s\nbut an ordinary unknown key answered\n  %s\nthe length bound must stop the WORK, never change the ANSWER",
			long.Body.String(), short.Body.String())
	}
}

// maxParkedAckStatusPerAgentForTest mirrors the unexported cap for the
// external test package. It is a SEPARATE declaration rather than an export of
// the real one: the cap is not part of the HTTP contract, and exporting it
// would invite a caller to depend on a number this bus may re-tune.
const maxParkedAckStatusPerAgentForTest = 32

// TestAckStatusParkedWaitCapIsPerPrincipal is the CROSS-AGENT half of the bound,
// and it is the assertion the whole cap exists to earn.
//
// # WHY IT NEEDED A SECOND PRINCIPAL
//
// maxParkedAckStatusPerAgent is documented as a self-harm bound: a flooder
// "can only fill its own bucket" (ackstatus.go), and CONTRACTS-HTTP.md publishes
// the same promise. Both statements are about what happens to OTHER agents, and
// no fixture driven by one principal can observe them — TestAckStatusCapsParked-
// WaitsPerAgent and TestAckWaiterCountAdmitsExactlyTheCap between them assert
// that the counter is per-key and that the handler consults it, but neither
// notices when the handler passes a CONSTANT key. Keyed on a constant, 32
// requests from any one agent lock every other agent out of ?wait= entirely:
// an authenticated cross-agent DoS on a route whose own doc comment says it
// cannot happen.
//
// # IT ASSERTS BEHAVIOUR, NOT BOOKKEEPING
//
// The claim is "the neighbour's request is SERVED while the flooder sits at its
// cap" — a 200 carrying the ordinary §13.3 answer, obtained through the same
// public ServeHTTP path a real agent uses. It does not reach into ackWaiterCount
// or count map entries, so a refactor that moves the bookkeeping cannot satisfy
// it accidentally, and it stays true of any implementation that keeps the
// promise.
//
// MUTATION (run, RED): replacing s.ackWaiters.acquire(sender) in
// handleAckStatus with a constant global bucket — acquire("") — leaves every
// other test in this package GREEN and fails this one, because the neighbour is
// answered 429 while the flooder holds all 32 slots.
func TestAckStatusParkedWaitCapIsPerPrincipal(t *testing.T) {
	srv, _ := newAckStatusServer(t)
	flooder := enrolAndAuthenticate(t, srv, "flooder")
	neighbour := enrolAndAuthenticate(t, srv, "neighbour")

	// The flooder holds every slot its own bucket allows, and keeps holding them
	// for the rest of the test: the refusal returned here is proof that it is AT
	// the cap, not merely near it.
	_, stop := parkAckWaitsUntilCapped(t, srv, flooder)
	defer stop()

	// A DIFFERENT principal, parking on the same route at the same moment, must
	// be admitted. Three times, on three keys: once could in principle catch a
	// slot a global bucket had just released, three consecutive successes could
	// not, and each of these requests is itself parked for its full second while
	// the flooder's 32 are still parked.
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("bus-msg-test-90%02d", i)
		rec := authed(t, srv, neighbour, http.MethodGet, ackStatusPath(key)+"?wait=1", "")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("probe %d: a SECOND agent was refused 429 while the first sat at its parked-wait cap.\n"+
				"The bucket is not keyed on the authenticated principal, so any one agent can lock every other agent out of ?wait= "+
				"by parking %d requests — a cross-agent denial of service on an authenticated route, and the exact opposite of the "+
				"self-harm bound ackstatus.go and CONTRACTS-HTTP.md both promise.\nbody: %s",
				i+1, maxParkedAckStatusPerAgentForTest, rec.Body.String())
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("probe %d: the neighbour's wait = %d, want 200; body %s", i+1, rec.Code, rec.Body.String())
		}
		// SERVED, not merely not-refused: the neighbour got the ordinary §13.3
		// answer, which is what a request that actually parked returns.
		rows := decodeAckRows(t, rec)
		if len(rows) != 1 || rows[0]["state"] != "unknown" {
			t.Fatalf("probe %d: the neighbour was answered 200 with %v, want the single unknown row", i+1, rows)
		}
	}

	// And the flooder is STILL capped, so the neighbour's admissions were not a
	// side effect of the flooder's slots draining mid-test.
	if still := authed(t, srv, flooder, http.MethodGet, ackStatusPath(ackProbeKey)+"?wait=1", ""); still.Code != http.StatusTooManyRequests {
		t.Errorf("the flooder was answered %d, want 429; it stopped being at its cap during the run, so the probes above prove nothing", still.Code)
	}
}

// ackProbeKey is the unknown key the cap fixtures probe with. It is never
// accepted into the table, so every answer about it is the uniform §13.3 one and
// nothing about the cap can be attributed to a row.
const ackProbeKey = "bus-msg-test-2000"

// parkAckWaitsUntilCapped fills a's parked-wait bucket and blocks until the bus
// refuses a's own probe with 429. It returns that refusal and a stop func that
// releases every parked request; stop is safe to call more than once.
//
// EACH FILLER RETRIES ON 429, and that is what makes this converge rather than
// flake. The probe competes for the same slots and holds one for a second at a
// time; a filler that gave up on its first refusal would permanently shrink the
// parked population, and the run could then sit below the cap forever. A filler
// that retries takes a slot the moment one frees and holds it until the context
// is cancelled, so the fillers monotonically converge on owning all 32 — after
// which the probe can only ever be refused.
func parkAckWaitsUntilCapped(t *testing.T, srv *httpapi.Server, a testAgent) (*httptest.ResponseRecorder, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	stop := func() {
		cancel()
		wg.Wait()
	}

	// Each filler parks on a DIFFERENT unknown key, so nothing about the cap can
	// be attributed to a shared row.
	for i := 0; i < maxParkedAckStatusPerAgentForTest; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for ctx.Err() == nil {
				req := httptest.NewRequest(http.MethodGet,
					ackStatusPath(fmt.Sprintf("bus-msg-test-%d", 1000+i))+"?wait=60", nil).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer "+a.token)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != http.StatusTooManyRequests {
					return // parked until the context was cancelled
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(i)
	}

	// 60s, not 20: this is a CONVERGENCE loop, and convergence is the thing a
	// loaded box slows down. The bound exists to fail the test if the cap never
	// engages at all, not to measure how fast it engages.
	deadline := time.Now().Add(60 * time.Second)
	for {
		// ?wait=1, not 60: an ADMITTED probe must return quickly or the poll
		// loop would itself become one of the parked requests it is waiting on.
		// A REFUSED probe returns immediately either way.
		over := authed(t, srv, a, http.MethodGet, ackStatusPath(ackProbeKey)+"?wait=1", "")
		if over.Code == http.StatusTooManyRequests {
			return over, stop
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("the over-limit wait was answered %d, want 429; the parked-wait cap did not engage for %s", over.Code, a.id)
		}
		// YIELD BETWEEN PROBES, and this is not padding.
		//
		// An ADMITTED probe holds a slot for its own second and then releases
		// it. Looping straight back into the next probe lets this goroutine
		// re-take the slot it just freed before any filler is scheduled — on a
		// single-CPU box that starves the fillers indefinitely and the cap never
		// fills, which is a TEST livelock and not a product defect. Reproduced
		// with GOMAXPROCS=1 before this sleep existed.
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAckStatusOmitsSettledAtBeforeSettlement pins the ZERO-CHECK on the
// optional timestamp (renderAckRows).
//
// formatInstant on a zero time renders "0001-01-01T00:00:00Z", which is a
// perfectly parseable RFC 3339 instant — so a client reading a row that has not
// settled would be told it settled in the year one, and would branch on it.
// Omitting the field says "absent", which is what it is.
//
// TestAckStatusRendersTheTerminalRow asserts settled_at is PRESENT once a row is
// terminal; it cannot notice a missing zero-check, because its row has a real
// settled_at either way. Both directions are needed.
//
// MUTATION (run, RED): dropping the `if !r.SettledAt.IsZero()` guard in
// renderAckRows makes this FAIL — the accepted row grows
// "settled_at":"0001-01-01T00:00:00Z".
func TestAckStatusOmitsSettledAtBeforeSettlement(t *testing.T) {
	srv, acks := newAckStatusServer(t)
	owner := enrolAndAuthenticate(t, srv, "owner")

	const key = "bus-msg-test-77"
	if err := acks.Accept(key, owner.id, msgTestBusID+".recipient-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	rows := decodeAckRows(t, authed(t, srv, owner, http.MethodGet, ackStatusPath(key), ""))
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want exactly the accepted row", rows)
	}
	row := rows[0]
	if row["state"] != "accepted" {
		t.Fatalf("state = %v, want accepted; the fixture is not testing a non-terminal row", row["state"])
	}
	if got, present := row["settled_at"]; present {
		t.Errorf("an accepted, UNSETTLED row carries settled_at=%v — a zero time rendered as an instant from the year one, which a client will parse and believe. It must be omitted entirely.", got)
	}
	// accepted_at is the timestamp every admitted record has, and it must still
	// be there: the fix for the above is not to omit both.
	if row["accepted_at"] == nil {
		t.Errorf("the accepted row carries no accepted_at: %v", row)
	}
}
