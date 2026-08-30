package httpapi_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// newConversationServer builds a server that serves POST /v1/conversations and
// the credential routes. It deliberately wires NO hub: a conversation is a
// durable object of its own plane, and the create route must not depend on the
// messaging surface. The WAL lives under t.TempDir(): NEVER the tracked data/.
func newConversationServer(t *testing.T) (*httpapi.Server, *store.ConversationStore) {
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

	conv, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: msgTestBusID, Logger: logger})
	if err != nil {
		t.Fatalf("building the conversation store: %v", err)
	}
	if err := conv.Attach(walLog); err != nil {
		t.Fatalf("attaching the conversation store: %v", err)
	}

	srv := httpapi.New(httpapi.Options{
		Identity:      testIdentity(msgTestBusID),
		Logger:        logger,
		Durable:       walLog,
		Auth:          svc,
		Conversations: conv,
	})
	return srv, conv
}

// TestConversationCreate is CONV-CREATE-CLI's proof for the HTTP surface: the
// server mints the id, records the creator from the authenticated session (never
// a request field), writes the durable record, and is idempotent across the
// three invariant-10 cases. It also pins the auth boundary (401 without a
// session) and that the route is absent when no store is wired.
func TestConversationCreate(t *testing.T) {
	t.Run("mints an id, records the creator from the session, and returns the record", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")

		rec := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":["bus-other.bob-1","bus-other.carol-1"],"name":"launch","idempotency_key":"conv-key-1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)

		id, _ := body["conversation_id"].(string)
		// The id is SERVER-MINTED: this bus's own id, a '.', then a uuid. Never a
		// client value (invariant 1). The request supplied no id at all.
		if !strings.HasPrefix(id, msgTestBusID+".") {
			t.Fatalf("conversation_id %q is not minted under this bus's id %q (invariant 1)", id, msgTestBusID)
		}
		// The CREATOR is the authenticated agent, taken from the session — not a
		// request field (there is none). It must be this agent's fully-qualified id.
		if got, _ := body["creator"].(string); got != a.id {
			t.Fatalf("creator = %q, want the authenticated agent %q (creator must come from the session, not the request)", got, a.id)
		}
		if got, _ := body["name"].(string); got != "launch" {
			t.Fatalf("name = %q, want %q", got, "launch")
		}
		if got := recipientStrings(t, body); len(got) != 2 || got[0] != "bus-other.bob-1" || got[1] != "bus-other.carol-1" {
			t.Fatalf("recipients = %v, want [bus-other.bob-1 bus-other.carol-1]", got)
		}
		if body["created_at"] == nil || body["created_at"] == "" {
			t.Fatalf("created_at missing from %s", rec.Body.String())
		}
	})

	t.Run("same key + same payload is a legitimate retry: the ORIGINAL conversation, replayed", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")

		payload := `{"recipients":["bus-other.bob-1"],"name":"chat","idempotency_key":"retry-key"}`
		first := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations, payload)
		if first.Code != http.StatusCreated {
			t.Fatalf("first create = %d, want 201; body %s", first.Code, first.Body.String())
		}
		firstID, _ := decodeBody(t, first)["conversation_id"].(string)

		second := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations, payload)
		if second.Code != http.StatusCreated {
			t.Fatalf("retry = %d, want 201; body %s", second.Code, second.Body.String())
		}
		if second.Header().Get(httpapi.IdempotencyReplayedHeader) != "true" {
			t.Fatalf("retry did not carry %s: true; headers %v", httpapi.IdempotencyReplayedHeader, second.Header())
		}
		secondID, _ := decodeBody(t, second)["conversation_id"].(string)
		if secondID != firstID {
			t.Fatalf("retry minted a SECOND conversation %q, want the original %q (invariant 10)", secondID, firstID)
		}
	})

	t.Run("same key + different payload is a protocol violation: 409, and no second conversation", func(t *testing.T) {
		srv, conv := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")

		first := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":["bus-other.bob-1"],"idempotency_key":"reuse-key"}`)
		if first.Code != http.StatusCreated {
			t.Fatalf("first create = %d, want 201; body %s", first.Code, first.Body.String())
		}
		before := conv.Len()

		clash := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":["bus-other.carol-1"],"idempotency_key":"reuse-key"}`)
		if clash.Code != http.StatusConflict {
			t.Fatalf("key reuse with a different payload = %d, want 409; body %s", clash.Code, clash.Body.String())
		}
		// It did NOT disconnect (invariant 10, narrowed): the response carries no
		// Connection: close.
		if strings.EqualFold(clash.Header().Get("Connection"), "close") {
			t.Fatalf("key reuse dropped the connection; invariant 10 keeps it (the key is the caller's own)")
		}
		if after := conv.Len(); after != before {
			t.Fatalf("a violation created a conversation: table went from %d to %d", before, after)
		}
	})

	t.Run("a recipient that is not fully-qualified is 400", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")
		rec := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":["bob-1"],"idempotency_key":"bad-recip"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unqualified recipient = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an empty recipient list is 400", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")
		rec := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":[],"idempotency_key":"empty-recip"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("empty recipients = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a missing idempotency key is 400", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")
		rec := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":["bus-other.bob-1"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("missing idempotency key = %d, want 400; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a creator field in the body is rejected as an unknown field, so it can never be injected", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		a := enrolAndAuthenticate(t, srv, "creator")
		// A client trying to create a conversation "as" someone else. The decoder
		// refuses unknown fields, so this is a 400 rather than a silently-honoured
		// creator (invariant 1).
		rec := authed(t, srv, a, http.MethodPost, httpapi.RouteConversations,
			`{"recipients":["bus-other.bob-1"],"creator":"bus-other.mallory-1","idempotency_key":"inject-key"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("a body carrying a creator field = %d, want 400 (unknown field); body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("without a session the route is 401 (invariant 3, default-deny)", func(t *testing.T) {
		srv, _ := newConversationServer(t)
		// No Authorization header at all.
		rec := postJSON(t, srv, httpapi.RouteConversations,
			`{"recipients":["bus-other.bob-1"],"idempotency_key":"no-auth"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated create = %d, want 401; body %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the route is absent when no conversation store is wired", func(t *testing.T) {
		srv := httpapi.New(httpapi.Options{Identity: testIdentity(msgTestBusID)})
		rec := postJSON(t, srv, httpapi.RouteConversations, `{"recipients":["bus-other.bob-1"],"idempotency_key":"k"}`)
		// Off the allow-list and unregistered: default-deny answers 401 to the
		// anonymous caller (it cannot tell a missing route from a protected one),
		// which is the same indistinguishability the catch-all preserves.
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("create with no store wired = %d, want 401 (default-deny hides the missing route); body %s", rec.Code, rec.Body.String())
		}
	})
}

// recipientStrings pulls the recipients array out of a decoded response body.
func recipientStrings(t *testing.T, body map[string]interface{}) []string {
	t.Helper()
	raw, ok := body["recipients"].([]interface{})
	if !ok {
		t.Fatalf("recipients is not an array: %v", body["recipients"])
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s, _ := r.(string)
		out = append(out, s)
	}
	return out
}
