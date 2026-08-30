package httpapi_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/client"
)

// TestClientCreateConversationEndToEnd drives the REAL client package against
// the REAL httpapi server over a REAL HTTP transport — no doubles — the same
// composition discipline TestClientSendVerifiesEndToEnd applies to send. It is
// what catches the drift class the client/server split invites: the route
// constant, the request/response field names and the replayed-header contract
// are duplicated across client/ (which cannot import internal/) and
// internal/httpapi, and this is the only place the two are exercised together.
//
// It is CONV-CREATE-CLI's invariant-7 proof at the library level: an agent
// EMBEDDING client/ reaches the feature exactly this way. The compiled-CLI run
// is recorded separately in the task report.
func TestClientCreateConversationEndToEnd(t *testing.T) {
	srv, _ := newConversationServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	c, err := client.New(client.Config{
		BusURL:      ts.URL,
		IdentityDir: t.TempDir(),
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	enrol, err := c.Enrol(context.Background(), client.EnrolOptions{Name: "creator", Save: true, MakeCurrent: true})
	if err != nil {
		t.Fatalf("enrolling against the real bus: %v", err)
	}

	ctx := context.Background()
	res, err := c.CreateConversation(ctx, client.CreateConversationOptions{
		Recipients: []string{"bus-other.bob-1", "bus-other.carol-1"},
		Name:       "release",
	})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if !strings.HasPrefix(res.ConversationID, msgTestBusID+".") {
		t.Fatalf("conversation_id %q is not minted under the bus id %q (invariant 1)", res.ConversationID, msgTestBusID)
	}
	if res.Creator != enrol.AgentID {
		t.Fatalf("creator = %q, want the enrolled identity %q (creator comes from the session)", res.Creator, enrol.AgentID)
	}
	if res.Name != "release" {
		t.Fatalf("name = %q, want %q", res.Name, "release")
	}
	if len(res.Recipients) != 2 {
		t.Fatalf("recipients = %v, want two", res.Recipients)
	}
	if res.Replayed {
		t.Fatalf("a first create reported replayed=true")
	}
	if res.IdempotencyKey == "" {
		t.Fatalf("the client did not surface the minted idempotency key")
	}

	// A retry under the SAME key returns the ORIGINAL, replayed (invariant 10),
	// exercised through the real client/server round trip and its header.
	retry, err := c.CreateConversation(ctx, client.CreateConversationOptions{
		Recipients:     []string{"bus-other.bob-1", "bus-other.carol-1"},
		Name:           "release",
		IdempotencyKey: res.IdempotencyKey,
	})
	if err != nil {
		t.Fatalf("retry CreateConversation: %v", err)
	}
	if !retry.Replayed {
		t.Fatalf("retry did not report replayed=true; the client did not read the replay header")
	}
	if retry.ConversationID != res.ConversationID {
		t.Fatalf("retry minted a SECOND conversation %q, want the original %q", retry.ConversationID, res.ConversationID)
	}
}
