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
