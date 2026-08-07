package client

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The tests in this file cover CLI-4 (send / broadcast) and CLI-5 (agents) at
// the CLIENT-PACKAGE level: the request the client actually puts on the wire,
// and what it makes of the answer.

// TestCLISendRejectsEmptyAndOversizeBody checks the two local body bounds are
// enforced BEFORE the round trip, for both write commands.
//
// Refusing locally is not merely an optimisation: the error names the actual
// size the caller exceeded, which a terse 400 from the bus cannot. The
// countingDoer proves the request never left — a check that fails after the
// call is not the check that was asked for.
func TestCLISendRejectsEmptyAndOversizeBody(t *testing.T) {
	oversize := make([]byte, MaxBodyBytes+1)
	for i := range oversize {
		oversize[i] = 'x'
	}

	cases := []struct {
		name string
		op   string
		send func(c *Client, body []byte) error
		body []byte
	}{
		{"send empty", "send", func(c *Client, b []byte) error {
			_, err := c.Send(context.Background(), SendOptions{To: "bus-x.other-1", Body: b})
			return err
		}, nil},
		{"send oversize", "send", func(c *Client, b []byte) error {
			_, err := c.Send(context.Background(), SendOptions{To: "bus-x.other-1", Body: b})
			return err
		}, oversize},
		{"broadcast empty", "broadcast", func(c *Client, b []byte) error {
			_, err := c.Broadcast(context.Background(), BroadcastOptions{Body: b})
			return err
		}, []byte{}},
		{"broadcast oversize", "broadcast", func(c *Client, b []byte) error {
			_, err := c.Broadcast(context.Background(), BroadcastOptions{Body: b})
			return err
		}, oversize},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			doer := &countingDoer{}
			c, err := New(Config{IdentityDir: t.TempDir(), HTTPClient: doer})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = tc.send(c, tc.body)
			if err == nil {
				t.Fatalf("%s with a %d-byte body = nil error, want one", tc.op, len(tc.body))
			}
			if KindOf(err) != KindUsage {
				t.Fatalf("KindOf(err) = %q, want %q (a local bound is the caller's mistake): %v", KindOf(err), KindUsage, err)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Fatalf("ExitCode(err) = %d, want %d", got, ExitUsage)
			}
			if got := atomic.LoadInt32(&doer.calls); got != 0 {
				t.Fatalf("HTTP calls = %d, want 0 — the body bound must be enforced before the round trip", got)
			}
		})
	}
}

// TestCLISendRequiresFullyQualifiedRecipient checks a BARE short name is
// refused with KindUsage and never sent (invariant 2: every agent id is
// `<bus-id>.<agent-id>`), and that a fully-qualified one reaches the wire
// verbatim.
func TestCLISendRequiresFullyQualifiedRecipient(t *testing.T) {
	bad := []struct {
		name string
		to   string
	}{
		{"empty", ""},
		{"bare short name", "planner"},
		{"bus prefix only", "bus-x."},
		{"leading dot", ".planner-1"},
		{"terminal escape", "bus-x.plan\x1b[2Kner"},
	}
	for _, tc := range bad {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			doer := &countingDoer{}
			c, err := New(Config{IdentityDir: t.TempDir(), HTTPClient: doer})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Send(context.Background(), SendOptions{To: tc.to, Body: []byte("hi")})
			if err == nil {
				t.Fatalf("Send to %q = nil error, want one", tc.to)
			}
			if KindOf(err) != KindUsage {
				t.Fatalf("KindOf(err) = %q, want %q: %v", KindOf(err), KindUsage, err)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("error is not a *client.Error: %v", err)
			}
			if !strings.Contains(e.Remedy, "busctl agents") {
				t.Fatalf("Remedy = %q, want it to name `busctl agents` as the way to find the real id", e.Remedy)
			}
			if got := atomic.LoadInt32(&doer.calls); got != 0 {
				t.Fatalf("HTTP calls = %d, want 0 — a malformed recipient must never be sent", got)
			}
		})
	}

	t.Run("fully qualified is accepted verbatim", func(t *testing.T) {
		const to = "bus-x.planner-1"
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			stubWriteJSON(w, http.StatusCreated, stubAccepted(7, "bus-x.agent-1", []string{to}, false, []byte("hi")))
		})
		c := bus.client(t, nil)
		res, err := c.Send(context.Background(), SendOptions{To: to, Body: []byte("hi")})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if res.MessageID != "bus-x-7" {
			t.Fatalf("MessageID = %q, want the server-minted %q", res.MessageID, "bus-x-7")
		}
		calls := bus.calls(routeSend)
		if len(calls) != 1 {
			t.Fatalf("the bus saw %d calls to %s, want 1", len(calls), routeSend)
		}
		if got, _ := calls[0].JSON()["to"].(string); got != to {
			t.Fatalf("request `to` = %q, want the fully-qualified id %q verbatim", got, to)
		}
	})
}

// TestCLISendSurfaces409AsIdempotencyViolation checks a 409 is re-worded into
// the one error it actually is.
//
// The transport's generic 409 text ("the bus refused the request") invites
// exactly the wrong reaction — another retry — when the bus has in fact
// answered `Connection: close` and disconnected because the SAME idempotency
// key arrived with a DIFFERENT payload (invariant 10). The message must name
// the key and say the payload differed.
func TestCLISendSurfaces409AsIdempotencyViolation(t *testing.T) {
	const key = "reused-key-abc"
	var calls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Connection", "close")
		stubWriteJSON(w, http.StatusConflict, map[string]string{"error": "idempotency key reused with different content"})
	})
	c := bus.client(t, nil)

	_, err := c.Send(context.Background(), SendOptions{
		To:             "bus-x.other-1",
		Body:           []byte("new content under an old key"),
		IdempotencyKey: key,
	})
	if err == nil {
		t.Fatalf("Send answered 409 = nil error, want one")
	}
	if KindOf(err) != KindRejected {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindRejected)
	}
	if got := ExitCode(err); got != ExitRejected {
		t.Fatalf("ExitCode(err) = %d, want %d", got, ExitRejected)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if !strings.Contains(e.Message, key) {
		t.Fatalf("Message = %q, want it to name the idempotency key %q", e.Message, key)
	}
	if !strings.Contains(e.Message, "DIFFERENT payload") {
		t.Fatalf("Message = %q, want it to say the payload DIFFERED", e.Message)
	}
	if strings.Contains(e.Message, "the bus refused the request") {
		t.Fatalf("Message = %q is still the transport's generic 409 wording; a key conflict needs its own", e.Message)
	}
	if !strings.Contains(e.Remedy, "FRESH") {
		t.Fatalf("Remedy = %q, want it to say a FRESH key is the fix", e.Remedy)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the bus saw %d send attempts, want 1 — a 409 must not be retried", got)
	}
}

// TestCLISendReportsReplayed checks the Idempotency-Replayed header sets
// Replayed and is NOT an error. Same key + same payload is a legitimate retry:
// the bus answers from its applied-key table, and punishing the client for
// doing the right thing would defeat invariant 10 entirely.
func TestCLISendReportsReplayed(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(idempotencyReplayedHeader, "true")
		stubWriteJSON(w, http.StatusCreated, stubAccepted(3, "bus-x.agent-1", []string{"bus-x.other-1"}, false, []byte("hi")))
	})
	c := bus.client(t, nil)

	res, err := c.Send(context.Background(), SendOptions{
		To:             "bus-x.other-1",
		Body:           []byte("hi"),
		IdempotencyKey: "retry-key-1",
	})
	if err != nil {
		t.Fatalf("Send with %s: true = %v, want no error — a replay is idempotency working", idempotencyReplayedHeader, err)
	}
	if !res.Replayed {
		t.Fatalf("Replayed = false, want true when the bus sets %s: true", idempotencyReplayedHeader)
	}
	if res.IdempotencyKey != "retry-key-1" {
		t.Fatalf("IdempotencyKey = %q, want the key the send was applied under", res.IdempotencyKey)
	}
}

// TestCLIBroadcastSendsNoRecipient checks a broadcast carries NO `to` field at
// all — the bus fans it out, it is not addressed — and that the result reports
// broadcast:true with an empty recipient list.
func TestCLIBroadcastSendsNoRecipient(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routeBroadcast {
			t.Errorf("broadcast went to %s, want %s", r.URL.Path, routeBroadcast)
		}
		stubWriteJSON(w, http.StatusCreated, stubAccepted(11, "bus-x.agent-1", []string{}, true, []byte("all hands")))
	})
	c := bus.client(t, nil)

	res, err := c.Broadcast(context.Background(), BroadcastOptions{Body: []byte("all hands")})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if !res.Broadcast {
		t.Fatalf("Broadcast = false, want true")
	}
	if len(res.To) != 0 {
		t.Fatalf("To = %v, want empty for a broadcast", res.To)
	}
	if res.IdempotencyKey == "" {
		t.Fatalf("IdempotencyKey = %q, want a minted key reported back — it is the only handle a later retry has", res.IdempotencyKey)
	}

	calls := bus.calls(routeBroadcast)
	if len(calls) != 1 {
		t.Fatalf("the bus saw %d calls to %s, want 1", len(calls), routeBroadcast)
	}
	body := calls[0].JSON()
	if body == nil {
		t.Fatalf("broadcast request body is not a JSON object: %q", calls[0].Body)
	}
	if _, ok := body["to"]; ok {
		t.Fatalf("broadcast request body carries a `to` field: %v — a broadcast has no recipient", body)
	}
	if _, ok := body["body"]; !ok {
		t.Fatalf("broadcast request body has no `body` field: %v", body)
	}
	if _, ok := body["idempotency_key"]; !ok {
		t.Fatalf("broadcast request body has no `idempotency_key`: %v — every mutating operation carries one (invariant 10)", body)
	}
	raw, _ := body["body"].(string)
	decoded, derr := base64.StdEncoding.DecodeString(raw)
	if derr != nil {
		t.Fatalf("broadcast body is not standard base64: %v (%q)", derr, raw)
	}
	if string(decoded) != "all hands" {
		t.Fatalf("broadcast body decoded to %q, want %q", decoded, "all hands")
	}
}

// TestCLIAgentsDerivesBusIDAndSorts checks the three things Agents adds on top
// of the wire:
//
//   - BusID is DERIVED as the prefix up to the FIRST '.', because an agent id
//     may itself contain dots while a bus id may not;
//   - the list is sorted here, so the order is a property of this package
//     rather than of whatever the remote service happens to do today;
//   - Count is len(Agents), RECOMPUTED — a hostile bus claiming 9999 agents
//     while sending three must not be able to say so through this type.
func TestCLIAgentsDerivesBusIDAndSorts(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("agents used %s, want GET", r.Method)
		}
		stubWriteJSON(w, http.StatusOK, map[string]interface{}{
			// Deliberately UNSORTED, and with a lying count.
			"agents": []map[string]string{
				{"agent_id": "bus-x.zulu-9", "name": "zulu", "enrolled_at": "2026-08-02T10:00:00Z"},
				{"agent_id": "bus-x.team.planner-1", "name": "planner", "enrolled_at": "2026-08-02T09:00:00Z"},
				{"agent_id": "bus-x.alpha-2", "name": "alpha", "enrolled_at": "2026-08-02T08:00:00Z"},
			},
			"count": 9999,
		})
	})
	c := bus.client(t, nil)

	list, err := c.Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if list.Count != len(list.Agents) {
		t.Fatalf("Count = %d but len(Agents) = %d — Count must be recomputed locally, never taken from the wire", list.Count, len(list.Agents))
	}
	if list.Count != 3 {
		t.Fatalf("Count = %d, want 3 (the wire said 9999)", list.Count)
	}

	wantIDs := []string{"bus-x.alpha-2", "bus-x.team.planner-1", "bus-x.zulu-9"}
	for i, want := range wantIDs {
		if list.Agents[i].AgentID != want {
			t.Fatalf("Agents[%d].AgentID = %q, want %q — the roster must be sorted by agent id", i, list.Agents[i].AgentID, want)
		}
		if list.Agents[i].BusID != "bus-x" {
			t.Fatalf("Agents[%d].BusID = %q, want %q — the bus id is the prefix up to the FIRST '.'", i, list.Agents[i].BusID, "bus-x")
		}
	}
}

// TestCLIAgentsRejectsMalformedRosterEntry checks a bad entry fails the WHOLE
// call rather than being silently dropped.
//
// Dropping it would hide the only thing it actually tells us: a roster entry
// that cannot be a legitimate agent id does not mean "one bad agent", it means
// the thing on the other end is not the bus we think it is. Each case pairs the
// bad entry with a perfectly good one, so a silent-drop implementation would
// return a non-empty list and fail here.
func TestCLIAgentsRejectsMalformedRosterEntry(t *testing.T) {
	good := map[string]string{"agent_id": "bus-x.good-1", "name": "good", "enrolled_at": "2026-08-02T08:00:00Z"}

	cases := []struct {
		name string
		bad  map[string]string
		want string
	}{
		{
			name: "control character in agent_id",
			bad:  map[string]string{"agent_id": "bus-x.evil\x1b[2K\rbusctl: ok-1", "name": "evil", "enrolled_at": "2026-08-02T08:00:00Z"},
			want: "characters an agent id cannot contain",
		},
		{
			name: "no bus prefix",
			bad:  map[string]string{"agent_id": "unqualified", "name": "unqualified", "enrolled_at": "2026-08-02T08:00:00Z"},
			want: "not fully qualified",
		},
		{
			name: "control character in name",
			bad:  map[string]string{"agent_id": "bus-x.evil-2", "name": "ev\x07il", "enrolled_at": "2026-08-02T08:00:00Z"},
			want: "characters an agent id cannot contain",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
				stubWriteJSON(w, http.StatusOK, map[string]interface{}{
					"agents": []map[string]string{good, tc.bad},
					"count":  2,
				})
			})
			c := bus.client(t, nil)

			list, err := c.Agents(context.Background())
			if err == nil {
				t.Fatalf("Agents with a malformed entry = nil error, want the WHOLE call to fail; got %+v", list)
			}
			if KindOf(err) != KindServer {
				t.Fatalf("KindOf(err) = %q, want %q: %v", KindOf(err), KindServer, err)
			}
			if len(list.Agents) != 0 {
				t.Fatalf("Agents = %+v, want an EMPTY list — a partial roster would hide the malformed entry", list.Agents)
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("error is not a *client.Error: %v", err)
			}
			if !strings.Contains(e.Message, tc.want) {
				t.Fatalf("Message = %q, want it to contain %q", e.Message, tc.want)
			}
			if strings.ContainsRune(e.Message, 0x1b) || strings.ContainsRune(e.Message, '\r') {
				t.Fatalf("the error message carries the hostile bytes through to a terminal: %q", e.Message)
			}
		})
	}
}

// nonFatalRetryClause is the EXACT text writeFailed appends to an ambiguous but
// genuinely retryable write failure.
//
// It is spelled out as a constant because two of the assertions below are its
// mirror images: a non-fatal failure MUST carry it, and a FATAL one must never.
// The leading "; " is part of it on purpose — it is what proves the clause was
// JOINED to an existing remedy rather than substituted for one.
const nonFatalRetryClause = "; retry with --idempotency-key"

// fatalRemedyFragment is the load-bearing half of the fatal-503 diagnosis
// statusError produces. It is the sentence that says WHY this failure is
// different, and it is the sentence a remedy-replacing writeFailed destroyed.
const fatalRemedyFragment = "retrying will not clear it"

// networkRemedyFragment is the first thing to check when the bus is unreachable
// — and the second casualty of a remedy-replacing writeFailed.
const networkRemedyFragment = "check --bus / " + EnvBusURL + " and that the bus is running"

// TestWriteFailedComposesRemedyAndNeverTellsAFatalBusToRetry pins the two
// regressions that stamping the idempotency key onto a failed write introduced,
// and that no end-to-end test caught.
//
// writeFailed originally REPLACED e.Remedy with the retry clause. That is a
// remedy-shaped hole in two directions:
//
//  1. A 503 with no Retry-After is classified KindServer + fatal, because the
//     bus's write path cannot durably accept and, per invariant 4, refusing is
//     what it SHOULD do. Its remedy says so in as many words. Replacing that
//     with "retry with --idempotency-key X" left IsFatalUnavailable reporting
//     true while the human beside it was told to retry — the API and the prose
//     saying opposite things about the same error, with the real diagnosis
//     deleted. An operator following the prose hammers a dead write path; a
//     supervisor following the API stops. Only one of them is right.
//  2. A transport failure's remedy is "check --bus / AGENT_BUS_URL and that the
//     bus is running", which is the ACTUAL first move when nothing can reach the
//     bus. Replacing it with retry mechanics leaves the reader holding a key and
//     no idea the bus is unreachable.
//
// So the contract asserted here is COMPOSITION: the original remedy survives
// verbatim, the clause is joined after "; ", and the clause's WORDING is chosen
// by e.fatal. The anti-assertion — a fatal error must not match
// nonFatalRetryClause — is the one that actually catches regression 1, because
// every positive assertion in the fatal case (key present, "--idempotency-key"
// named) passes just as happily against the broken version.
//
// It calls writeFailed and statusError DIRECTLY, and builds every *Error through
// statusError rather than by hand, so what is under test is the REAL
// classification — fatal bit included — rather than a fixture that agrees with
// the test by construction.
func TestWriteFailedComposesRemedyAndNeverTellsAFatalBusToRetry(t *testing.T) {
	const key = "write-key-42"

	// fromStatus builds the error the transport would build for this response.
	fromStatus := func(op string, status int, retryAfter, detail string) error {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		resp := &http.Response{StatusCode: status, Header: h}
		return statusError(op, resp, []byte(`{"error":`+strconvQuote(detail)+`}`))
	}

	cases := []struct {
		name string
		op   string
		// build returns the error as it reaches writeFailed, i.e. straight off
		// the transport (and, for the 409, through the annotation submit applies
		// first).
		build func() error
		// wantKind is the Kind after writeFailed. The Kind vocabulary is closed
		// and the CLI maps it to an exit code, so annotating an error must never
		// move it.
		wantKind Kind
		// wantFatal is IsFatalUnavailable after writeFailed.
		wantFatal bool
		// wantRemedyKeeps are substrings of the ORIGINAL remedy that must
		// survive composition.
		wantRemedyKeeps []string
		// wantRemedyAdds are substrings the appended clause must contribute.
		wantRemedyAdds []string
		// wantRemedyLacks are substrings that must NOT appear.
		wantRemedyLacks []string
		// wantRemedyUnchanged asserts the remedy is byte-for-byte what it was
		// before writeFailed — no clause at all.
		wantRemedyUnchanged bool
		// wantKeyStamped asserts e.IdempotencyKey is the key we passed.
		wantKeyStamped bool
	}{
		{
			// Regression 1, in its exact original shape.
			name: "fatal 503 send keeps its diagnosis and is told NOT to retry",
			op:   "send",
			build: func() error {
				return fromStatus("send", http.StatusServiceUnavailable, "", "the write path is poisoned")
			},
			wantKind:  KindServer,
			wantFatal: true,
			wantRemedyKeeps: []string{
				fatalRemedyFragment,
				"invariant 4",
			},
			wantRemedyAdds: []string{
				"this send may or may not have been applied",
				"do NOT retry until the bus can durably accept again",
				"--idempotency-key " + key,
				"invariant 10",
			},
			wantKeyStamped: true,
		},
		{
			// The OTHER 503. Retry-After present = a live capacity bound, which
			// is transient by construction, so the retry clause belongs here.
			name: "non-fatal 503 with Retry-After keeps its remedy and gains the retry clause",
			op:   "send",
			build: func() error {
				return fromStatus("send", http.StatusServiceUnavailable, "1", "the applied-key table is full")
			},
			wantKind:  KindServer,
			wantFatal: false,
			wantRemedyKeeps: []string{
				"retry in a few seconds",
			},
			wantRemedyAdds: []string{
				"this send may or may not have been applied",
				nonFatalRetryClause + " " + key,
			},
			wantRemedyLacks: []string{"do NOT retry"},
			wantKeyStamped:  true,
		},
		{
			name: "500 keeps the log pointer and gains the retry clause",
			op:   "send",
			build: func() error {
				return fromStatus("send", http.StatusInternalServerError, "", "the write path fell over")
			},
			wantKind:  KindServer,
			wantFatal: false,
			wantRemedyKeeps: []string{
				"check the bus's logs",
			},
			wantRemedyAdds:  []string{nonFatalRetryClause + " " + key},
			wantRemedyLacks: []string{"do NOT retry"},
			wantKeyStamped:  true,
		},
		{
			// Same shape on the broadcast route, and the clause must name the
			// operation the caller actually ran. "this send may or may not have
			// been applied" after a broadcast is a small lie that costs a reader
			// real time.
			name: "500 on a broadcast names broadcast, not send",
			op:   "broadcast",
			build: func() error {
				return fromStatus("broadcast", http.StatusInternalServerError, "", "the write path fell over")
			},
			wantKind:  KindServer,
			wantFatal: false,
			wantRemedyKeeps: []string{
				"check the bus's logs",
			},
			wantRemedyAdds: []string{
				"this broadcast may or may not have been applied",
				nonFatalRetryClause + " " + key,
			},
			wantRemedyLacks: []string{"this send may or may not"},
			wantKeyStamped:  true,
		},
		{
			// Regression 2. The bus is unreachable; the remedy that says so must
			// still be the FIRST thing the reader sees.
			name: "network failure keeps `check --bus` and gains the retry clause",
			op:   "send",
			build: func() error {
				return networkError("send", "http://bus.invalid:9999", errors.New("dial tcp 127.0.0.1:9999: connect: connection refused"))
			},
			wantKind:  KindNetwork,
			wantFatal: false,
			wantRemedyKeeps: []string{
				networkRemedyFragment,
			},
			wantRemedyAdds:  []string{nonFatalRetryClause + " " + key},
			wantRemedyLacks: []string{"do NOT retry"},
			wantKeyStamped:  true,
		},
		{
			// A 409 is the one failure where retrying under this key is the
			// VIOLATION, not the fix. It still carries the key — an embedder may
			// want it — but gains no clause at all.
			name: "409 conflict carries the key but gains no retry clause",
			op:   "send",
			build: func() error {
				return annotateIdempotencyConflict("send", key,
					fromStatus("send", http.StatusConflict, "", "idempotency key reused with different content"))
			},
			wantKind:            KindRejected,
			wantFatal:           false,
			wantRemedyUnchanged: true,
			wantRemedyLacks: []string{
				nonFatalRetryClause,
				"do NOT retry",
				"may or may not have been applied",
			},
			wantKeyStamped: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build()
			var before *Error
			if !errors.As(err, &before) {
				t.Fatalf("the fixture is not a *client.Error, so this case proves nothing: %v", err)
			}
			beforeMessage, beforeRemedy := before.Message, before.Remedy
			if beforeRemedy == "" {
				t.Fatalf("the fixture has an EMPTY remedy, so `the original remedy survives` is vacuous here: %+v", before)
			}

			got := writeFailed(tc.op, key, err)

			var e *Error
			if !errors.As(got, &e) {
				t.Fatalf("writeFailed returned a non-*Error: %v", got)
			}
			if e.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q — annotating a failure must not move it between categories (the CLI maps Kind to an exit code)", e.Kind, tc.wantKind)
			}
			if got := IsFatalUnavailable(got); got != tc.wantFatal {
				t.Fatalf("IsFatalUnavailable = %v, want %v — writeFailed must not disturb the fatal bit", got, tc.wantFatal)
			}
			if e.Message != beforeMessage {
				t.Fatalf("Message was rewritten:\n  before: %q\n   after: %q\nwriteFailed adds a remedy; WHAT WENT WRONG is not its to change", beforeMessage, e.Message)
			}
			if tc.wantKeyStamped && e.IdempotencyKey != key {
				t.Fatalf("IdempotencyKey = %q, want %q — the key is the only handle a later retry has (invariant 10)", e.IdempotencyKey, key)
			}
			if got := IdempotencyKeyOf(got); tc.wantKeyStamped && got != key {
				t.Fatalf("IdempotencyKeyOf = %q, want %q", got, key)
			}

			if tc.wantRemedyUnchanged && e.Remedy != beforeRemedy {
				t.Fatalf("the remedy was modified:\n  before: %q\n   after: %q\nthis failure must gain no retry clause — retrying under this key is the protocol violation, not the fix", beforeRemedy, e.Remedy)
			}
			for _, want := range tc.wantRemedyKeeps {
				if !strings.Contains(e.Remedy, want) {
					t.Fatalf("the ORIGINAL diagnosis was destroyed: remedy no longer contains %q.\n  before: %q\n   after: %q\n"+
						"The transport's remedy says what is WRONG; the key clause is only the mechanics of retrying. "+
						"The clause must be APPENDED after \"; \", never substituted for the diagnosis.", want, beforeRemedy, e.Remedy)
				}
			}
			for _, want := range tc.wantRemedyAdds {
				if !strings.Contains(e.Remedy, want) {
					t.Fatalf("remedy = %q, want it to contain the appended clause %q", e.Remedy, want)
				}
			}
			for _, unwanted := range tc.wantRemedyLacks {
				if strings.Contains(e.Remedy, unwanted) {
					t.Fatalf("remedy = %q, want it NOT to contain %q", e.Remedy, unwanted)
				}
			}

			// THE anti-assertion. Everything above passes against the broken,
			// remedy-REPLACING writeFailed except this and the wantRemedyKeeps
			// check — and this one is the only thing standing between an
			// operator and being told to retry a bus that has told us, in the
			// one way it can, that retrying is futile.
			if tc.wantFatal && strings.Contains(e.Remedy, nonFatalRetryClause) {
				t.Fatalf("REGRESSION: a FATAL failure was given the NON-FATAL retry clause %q.\n"+
					"  remedy = %q\n"+
					"IsFatalUnavailable(err) is TRUE for this error, so the API tells a supervisor to STOP while the "+
					"prose tells the human to retry now. This bus answered 503 with no Retry-After, which it does only "+
					"when its write path cannot durably accept (invariant 4); nothing on the client side clears that, so "+
					"the clause must be the `do NOT retry until the bus can durably accept again` wording and the key must "+
					"be offered as a handle for LATER.", nonFatalRetryClause, e.Remedy)
			}
			// Composition, structurally: the joined remedy is strictly longer
			// than the diagnosis it was built from, and still starts with it.
			if !tc.wantRemedyUnchanged && !strings.HasPrefix(e.Remedy, beforeRemedy) {
				t.Fatalf("the composed remedy does not START with the original:\n  before: %q\n   after: %q\n"+
					"the clause is appended to the diagnosis, not prepended to it or swapped for it", beforeRemedy, e.Remedy)
			}
		})
	}

	// An error that is not an *Error must pass through untouched rather than
	// panic or be swallowed. writeFailed is called on EVERY failure path of
	// submit, including ones that may one day return something else.
	t.Run("a plain error passes through unchanged", func(t *testing.T) {
		plain := errors.New("something entirely else")
		got := writeFailed("send", key, plain)
		if got != plain {
			t.Fatalf("writeFailed returned %#v, want the original error unchanged", got)
		}
		if k := IdempotencyKeyOf(got); k != "" {
			t.Fatalf("IdempotencyKeyOf = %q, want \"\" — there is nowhere to stamp a key on a non-*Error", k)
		}
	})
}

// strconvQuote JSON-quotes a detail string for the stub error bodies above. It
// is strconv.Quote in all the cases used here and exists only so this file does
// not grow an import for one call.
func strconvQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }
