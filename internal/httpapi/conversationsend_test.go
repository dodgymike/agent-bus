package httpapi_test

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// newConversationSendServer builds a server that serves BOTH the messaging
// surface (a real hub) AND the conversation surface (create + send), over ONE
// durable log and ONE roster — exactly as cmd/agent-bus wires it. The send
// routes are what CONV-SEND-BY-ID adds, and they register only when a
// conversation lookup AND a hub are both present.
func newConversationSendServer(t *testing.T) (*httpapi.Server, *store.ConversationStore) {
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
	roster := auth.NewMemoryRoster()
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	h, err := hub.Open(hub.Options{
		BusID:   msgTestBusID,
		DataDir: filepath.Dir(walLog.Path()),
		Durable: walLog,
		Replay: func(fn func(wal.Committed) error) (wal.Recovered, error) {
			return wal.Replay(walLog.Path(), fn)
		},
		NextIndex: walLog.Recovered().NextIndex,
		Roster:    authRosterView{roster},
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("opening the hub: %v", err)
	}

	conv, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: msgTestBusID, Logger: logger})
	if err != nil {
		t.Fatalf("building the conversation store: %v", err)
	}
	if err := conv.Attach(walLog); err != nil {
		t.Fatalf("attaching the conversation store: %v", err)
	}

	srv := httpapi.New(httpapi.Options{
		Identity:           testIdentity(msgTestBusID),
		Logger:             logger,
		Durable:            walLog,
		Auth:               svc,
		Hub:                h,
		Conversations:      conv,
		ConversationLookup: conv,
	})
	return srv, conv
}

// createConversation creates a conversation as `by` with the given recipients
// and returns the server-minted id.
func createConversation(t *testing.T, srv *httpapi.Server, by testAgent, recipients []string, key string) string {
	t.Helper()
	quoted := make([]string, 0, len(recipients))
	for _, r := range recipients {
		quoted = append(quoted, fmt.Sprintf("%q", r))
	}
	body := fmt.Sprintf(`{"recipients":[%s],"idempotency_key":%q}`, join(quoted, ","), key)
	rec := authed(t, srv, by, http.MethodPost, httpapi.RouteConversations, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create conversation = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	id, _ := decodeBody(t, rec)["conversation_id"].(string)
	if id == "" {
		t.Fatalf("create conversation returned no conversation_id; body %s", rec.Body.String())
	}
	return id
}

func join(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}

// convMint runs the first leg — POST /v1/conversations/mint — and returns the
// reservation plus the resolved membership the bus handed back to sign.
func convMint(t *testing.T, srv *httpapi.Server, from testAgent, convID, key string) (messageID string, seq uint64, recipients []string) {
	t.Helper()
	rec := authed(t, srv, from, http.MethodPost, httpapi.RouteConversationMint,
		fmt.Sprintf(`{"conversation_id":%q,"idempotency_key":%q}`, convID, key))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s = %d, want 201; body %s", httpapi.RouteConversationMint, rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	messageID, _ = body["message_id"].(string)
	rawSeq, _ := body["seq"].(float64)
	for _, r := range toStringSlice(body["recipients"]) {
		recipients = append(recipients, r)
	}
	if messageID == "" || rawSeq == 0 || len(recipients) == 0 {
		t.Fatalf("%s returned message_id %q seq %v recipients %v; all are required", httpapi.RouteConversationMint, messageID, body["seq"], recipients)
	}
	return messageID, uint64(rawSeq), recipients
}

func toStringSlice(v interface{}) []string {
	raw, _ := v.([]interface{})
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// convSendBody is the POST /v1/conversations/send request a client builds once it
// holds a reservation. The signature is the same well-formed placeholder the
// messaging tests use — the bus checks SHAPE and never verifies (invariant: the
// bus does not hold the sender's key).
func convSendBody(convID, payloadB64, key, sender, messageID string, seq uint64) string {
	return fmt.Sprintf(`{"conversation_id":%q,"body":%q,"idempotency_key":%q,"sender":%q,"message_id":%q,"seq":%d,"timestamp_ms":%d,"signature":%q}`,
		convID, payloadB64, key, sender, messageID, seq, msgTestTimestampMs, msgTestSignature())
}

// TestConversationSendByID is CONV-SEND-BY-ID's proof for the HTTP surface: a
// message addressed by conversation id is delivered to the resolved membership,
// only a participant may send, an unknown id and a non-member both 404, and a
// retry returns the original (invariant 10).
func TestConversationSendByID(t *testing.T) {
	t.Run("delivers to every member except the sender, resolving membership server-side", func(t *testing.T) {
		srv, _ := newConversationSendServer(t)
		alice := enrolAndAuthenticate(t, srv, "alice")
		bob := enrolAndAuthenticate(t, srv, "bob")
		carol := enrolAndAuthenticate(t, srv, "carol")

		// alice creates a conversation with bob and carol; membership is
		// {alice, bob, carol}.
		convID := createConversation(t, srv, alice, []string{bob.id, carol.id}, "conv-create-1")

		msgID, seq, members := convMint(t, srv, alice, convID, "conv-send-1")
		// The mint hands back the membership to sign — creator first, deduped.
		if len(members) != 3 || !containsID(members, alice.id) || !containsID(members, bob.id) || !containsID(members, carol.id) {
			t.Fatalf("mint returned members %v, want {alice,bob,carol}", members)
		}

		rec := authed(t, srv, alice, http.MethodPost, httpapi.RouteConversationSend,
			convSendBody(convID, b64("hello conversation"), "conv-send-1", alice.id, msgID, seq))
		if rec.Code != http.StatusCreated {
			t.Fatalf("conversation send = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		sent := decodeBody(t, rec)
		gotID, _ := sent["message_id"].(string)
		if gotID != msgID {
			t.Fatalf("send stored message id %q, want the minted %q", gotID, msgID)
		}
		// The stored recipient set is the whole membership (the durable record
		// freezes the send-time roster; DECISIONS.md CONV-SEND-BY-ID).
		if to := toStringSlice(sent["to"]); len(to) != 3 {
			t.Fatalf("send recorded to=%v, want the 3-member set", to)
		}

		// bob and carol receive it; alice (the sender) does not.
		if !receives(t, srv, bob, msgID) {
			t.Fatalf("bob did not receive the conversation message")
		}
		if !receives(t, srv, carol, msgID) {
			t.Fatalf("carol did not receive the conversation message")
		}
		if receives(t, srv, alice, msgID) {
			t.Fatalf("alice (the sender) received her own conversation message; the sender must be excluded")
		}
	})

	t.Run("a non-member is refused with 404, indistinguishable from a nonexistent conversation", func(t *testing.T) {
		srv, _ := newConversationSendServer(t)
		alice := enrolAndAuthenticate(t, srv, "alice")
		bob := enrolAndAuthenticate(t, srv, "bob")
		dave := enrolAndAuthenticate(t, srv, "dave") // not a member

		convID := createConversation(t, srv, alice, []string{bob.id}, "conv-create-2")

		// The mint leg refuses a non-member with 404.
		rec := authed(t, srv, dave, http.MethodPost, httpapi.RouteConversationMint,
			fmt.Sprintf(`{"conversation_id":%q,"idempotency_key":%q}`, convID, "dave-key"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("non-member mint = %d, want 404 (leak-less: a non-member must not be told the conversation exists); body %s", rec.Code, rec.Body.String())
		}

		// So does the send leg, even if the caller fabricates a reservation.
		sendRec := authed(t, srv, dave, http.MethodPost, httpapi.RouteConversationSend,
			convSendBody(convID, b64("intrusion"), "dave-send", dave.id, msgTestBusID+"-999", 999))
		if sendRec.Code != http.StatusNotFound {
			t.Fatalf("non-member send = %d, want 404; body %s", sendRec.Code, sendRec.Body.String())
		}
	})

	t.Run("an unknown conversation id is a clean 404, never a panic", func(t *testing.T) {
		srv, _ := newConversationSendServer(t)
		alice := enrolAndAuthenticate(t, srv, "alice")

		unknown := msgTestBusID + ".00000000-0000-4000-8000-000000000000"
		rec := authed(t, srv, alice, http.MethodPost, httpapi.RouteConversationMint,
			fmt.Sprintf(`{"conversation_id":%q,"idempotency_key":%q}`, unknown, "u-key"))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("mint against an unknown conversation = %d, want 404; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("same key + same payload is a legitimate retry: the ORIGINAL result, replayed", func(t *testing.T) {
		srv, _ := newConversationSendServer(t)
		alice := enrolAndAuthenticate(t, srv, "alice")
		bob := enrolAndAuthenticate(t, srv, "bob")

		convID := createConversation(t, srv, alice, []string{bob.id}, "conv-create-3")

		msgID, seq, _ := convMint(t, srv, alice, convID, "conv-send-3")
		payload := b64("retry me")

		first := authed(t, srv, alice, http.MethodPost, httpapi.RouteConversationSend,
			convSendBody(convID, payload, "conv-send-3", alice.id, msgID, seq))
		if first.Code != http.StatusCreated {
			t.Fatalf("first send = %d, want 201; body %s", first.Code, first.Body.String())
		}
		firstID, _ := decodeBody(t, first)["message_id"].(string)

		second := authed(t, srv, alice, http.MethodPost, httpapi.RouteConversationSend,
			convSendBody(convID, payload, "conv-send-3", alice.id, msgID, seq))
		if second.Code != http.StatusCreated {
			t.Fatalf("retry send = %d, want 201; body %s", second.Code, second.Body.String())
		}
		if second.Header().Get(httpapi.IdempotencyReplayedHeader) != "true" {
			t.Fatalf("retry did not carry %s: true; headers %v", httpapi.IdempotencyReplayedHeader, second.Header())
		}
		secondID, _ := decodeBody(t, second)["message_id"].(string)
		if secondID != firstID {
			t.Fatalf("retry produced a SECOND message %q, want the original %q (invariant 10)", secondID, firstID)
		}
	})

	t.Run("the send routes require authentication (invariant 3)", func(t *testing.T) {
		srv, _ := newConversationSendServer(t)
		for _, p := range []string{httpapi.RouteConversationMint, httpapi.RouteConversationSend} {
			if httpapi.IsUnauthenticatedRoute(p) {
				t.Fatalf("%s is on the anonymous allow-list; a conversation send reads and writes agents' traffic (invariant 3)", p)
			}
			rec := doRequest(t, srv, http.MethodPost, p, "", "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s with NO credential = %d, want 401", p, rec.Code)
			}
		}
	})
}

// receives reports whether agent a can see message id in its history.
func receives(t *testing.T, srv *httpapi.Server, a testAgent, id string) bool {
	t.Helper()
	rec := authed(t, srv, a, http.MethodGet, httpapi.RouteMessages, "")
	_, msgs := batchOf(t, rec)
	for _, m := range msgs {
		mm, _ := m.(map[string]interface{})
		if got, _ := mm["message_id"].(string); got == id {
			return true
		}
	}
	return false
}

func containsID(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
