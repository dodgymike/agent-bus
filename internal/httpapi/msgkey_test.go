package httpapi_test

// RELAY-13: enrolment registers the agent's MESSAGING public key.
//
// The point of these tests is not that a JSON field is parsed. It is that the
// key an origin bus will later ATTEST to a peer bus (RELAY-17) actually reaches
// the roster and is still there after a restart — a bus cannot attest a key it
// does not have, and a field that is written but never replayed is WORSE than
// one that is absent, because it looks present until the process restarts.
//
// So the durability subtest drives a REAL wal.Log on a REAL directory through
// the REAL HTTP handler, closes it, and rebuilds the roster from disk with
// nothing carried over in memory. A fake durable log would prove the handler
// called Write; it would not prove the bytes come back.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// msgKeyBusID qualifies every agent id these tests mint (invariant 2).
const msgKeyBusID = "bus-msgkey-test"

// newMsgKeyServer builds a Server over a REAL auth.Service whose roster the
// caller can read back. newAuthServer (auth_test.go) hides its roster, and the
// whole claim under test here is what is IN the roster.
func newMsgKeyServer(t *testing.T, roster auth.Roster) *httpapi.Server {
	t.Helper()

	minter, err := ids.NewAgentIDMinter(msgKeyBusID, ids.NewNameSuffixes())
	if err != nil {
		t.Fatalf("building the agent id minter: %v", err)
	}
	svc, err := auth.NewService(auth.Options{Minter: minter, Roster: roster})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}
	var logBuf bytes.Buffer
	return httpapi.New(httpapi.Options{
		Identity: testIdentity(msgKeyBusID),
		Logger:   logging.New(&logBuf, logging.LevelDebug),
		Auth:     svc,
	})
}

// newMsgKeypair returns a fresh Ed25519 public key and its base64 spelling, as
// a client sends it. Separate from newAuthKeypair so a test can hold an auth
// key and a messaging key at once and assert they are NOT the same bytes.
func newMsgKeypair(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 keypair: %v", err)
	}
	return pub, base64.StdEncoding.EncodeToString(pub)
}

// enrolBody renders an enrolment request. msgPubB64 == "" omits the messaging
// key entirely, which is exactly the shape a client built before the field
// existed sends.
func enrolBody(name, pubB64, msgPubB64, idemKey string) string {
	b := `{"name":"` + name + `","public_key":"` + pubB64 + `"`
	if msgPubB64 != "" {
		b += `,"messaging_public_key":"` + msgPubB64 + `"`
	}
	return b + `,"idempotency_key":"` + idemKey + `"}`
}

func TestEnrolRegistersMessagingPublicKey(t *testing.T) {
	t.Run("the wire field reaches the roster, distinct from the auth key", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		authPub, _, authB64 := newAuthKeypair(t)
		msgPub, msgB64 := newMsgKeypair(t)

		rec := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("alpha", authB64, msgB64, "idem-1"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		agentID, _ := decodeBody(t, rec)["agent_id"].(string)
		if agentID == "" {
			t.Fatalf("no agent_id in %s", rec.Body.String())
		}

		entry, ok := roster.Get(agentID)
		if !ok {
			t.Fatalf("agent %q is not in the roster after a 201", agentID)
		}
		if !bytes.Equal(entry.MessagingPublicKey, msgPub) {
			// This is the RELAY-17 dependency in one line: the attestation signs
			// over this key, so an entry without it cannot be attested at all.
			t.Fatalf("roster MessagingPublicKey = %x, want the key the client sent %x", entry.MessagingPublicKey, msgPub)
		}
		if !bytes.Equal(entry.AuthPublicKey, authPub) {
			t.Fatalf("roster AuthPublicKey = %x, want %x", entry.AuthPublicKey, authPub)
		}
		if bytes.Equal(entry.MessagingPublicKey, entry.AuthPublicKey) {
			// A wiring bug that stored ONE of the two keys in BOTH fields would
			// pass every other assertion here — the messaging key would be
			// "present", it would round-trip, it would replay — while quietly
			// collapsing the auth/messaging separation. The client sent two
			// different keys, so the roster must hold two.
			t.Fatal("the roster stored the same bytes in AuthPublicKey and MessagingPublicKey, but the client sent two DIFFERENT keys; one of the two fields is being written from the wrong source")
		}
	})

	t.Run("it survives a restart, replayed from the durable log", func(t *testing.T) {
		dir := t.TempDir()
		_, _, authB64 := newAuthKeypair(t)
		msgPub, msgB64 := newMsgKeypair(t)

		// --- first boot: real WALRoster over a real log on disk ---
		r1 := auth.NewWALRoster(nil)
		l1, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r1})
		if err != nil {
			t.Fatalf("opening the log: %v", err)
		}
		if err := r1.Attach(l1); err != nil {
			t.Fatalf("attaching the roster: %v", err)
		}
		srv := newMsgKeyServer(t, r1)

		rec := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("worker", authB64, msgB64, "idem-restart"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		agentID, _ := decodeBody(t, rec)["agent_id"].(string)
		if agentID == "" {
			t.Fatalf("no agent_id in %s", rec.Body.String())
		}
		if err := l1.Close(); err != nil {
			t.Fatalf("closing the first log: %v", err)
		}

		// --- the restart: a BRAND NEW roster, rebuilt only by replay ---
		r2 := auth.NewWALRoster(nil)
		l2, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r2})
		if err != nil {
			t.Fatalf("reopening the log: %v", err)
		}
		defer l2.Close()

		entry, ok := r2.Get(agentID)
		if !ok {
			t.Fatalf("agent %q is absent from the recovered roster; the enrolment was acknowledged, so it must replay", agentID)
		}
		if !bytes.Equal(entry.MessagingPublicKey, msgPub) {
			t.Fatalf("recovered MessagingPublicKey = %x, want %x. The key was acknowledged as registered and did not survive the restart — which is worse than never having stored it, because it looked present right up until the process died", entry.MessagingPublicKey, msgPub)
		}
	})

	t.Run("an enrolment with no messaging key is still accepted", func(t *testing.T) {
		// The compatibility half, and a deliberate record of TODAY's behaviour:
		// every agent enrolled before this field existed has none, and refusing
		// them would brick every current identity. Requiring it on NEW
		// enrolments is the intended end state; when that lands this subtest
		// flips to asserting a 400, and it is here so that flip is a visible
		// contract change rather than a silent one.
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		_, _, authB64 := newAuthKeypair(t)

		rec := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("legacy", authB64, "", "idem-legacy"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (the field is optional today); body %s", rec.Code, rec.Body.String())
		}
		agentID, _ := decodeBody(t, rec)["agent_id"].(string)
		entry, ok := roster.Get(agentID)
		if !ok {
			t.Fatalf("agent %q is not in the roster after a 201", agentID)
		}
		if len(entry.MessagingPublicKey) != 0 {
			t.Fatalf("MessagingPublicKey = %x for an enrolment that sent none; empty is the reserved state and nothing may invent a value for it", entry.MessagingPublicKey)
		}
	})

	t.Run("a malformed messaging key is refused and enrols nobody", func(t *testing.T) {
		cases := []struct {
			name  string
			value string
		}{
			{"not base64", "!!!!not base64!!!!"},
			{"31 bytes", base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize-1))},
			{"33 bytes", base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize+1))},
			{"empty-but-present padding only", "="},
			{
				// NON-CANONICAL base64: 44 characters, the RIGHT LENGTH, with
				// non-zero trailing bits in the final quantum. The non-strict
				// decoder accepts it and yields a 32-byte key; Strict() refuses
				// it. This case therefore exercises Strict() SPECIFICALLY, which
				// a wrong-length string cannot — it would be refused on length
				// by either decoder and would prove nothing about the strictness
				// the field's contract claims.
				//
				// It matters because it is what gives one key exactly one
				// spelling. Two spellings of the same key would make a genuine
				// idempotent retry look like a key reused for different content
				// (the comparison is over bytes, but the client would have had
				// to re-encode) and would give an attacker two distinct 32-byte
				// strings decoding to one identity.
				"non-canonical base64",
				base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))[:42] + "B=",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				roster := auth.NewMemoryRoster()
				srv := newMsgKeyServer(t, roster)
				_, _, authB64 := newAuthKeypair(t)

				rec := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("bad", authB64, tc.value, "idem-bad"))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
				}
				if roster.Len() != 0 {
					t.Fatalf("roster holds %d agents after a rejected enrolment, want 0", roster.Len())
				}
				// The rejected VALUE is never echoed: this route is
				// unauthenticated and the field carries key material.
				if strings.Contains(rec.Body.String(), tc.value) {
					t.Fatalf("the 400 body echoes the offending key material: %s", rec.Body.String())
				}
			})
		}
	})

	t.Run("a refused messaging key burns no agent id suffix", func(t *testing.T) {
		// Invariant 1: a suffix the minter allocates is SPENT and never handed
		// back, so validation of the new field has to run BEFORE the mint or an
		// anonymous caller burns one number per malformed request on this
		// unauthenticated route. That ordering is invisible in the code — the
		// check sits a few lines above minter.Mint and nothing stops a later
		// edit moving it below — so it is pinned here: the FIRST successful
		// enrolment of a name must still get suffix 1 after a rejected attempt
		// at the same name.
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		_, _, authB64 := newAuthKeypair(t)
		short := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize-1))

		rejected := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("solo", authB64, short, "idem-burn-1"))
		if rejected.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", rejected.Code, rejected.Body.String())
		}

		_, _, goodAuthB64 := newAuthKeypair(t)
		_, goodMsgB64 := newMsgKeypair(t)
		accepted := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("solo", goodAuthB64, goodMsgB64, "idem-burn-2"))
		if accepted.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", accepted.Code, accepted.Body.String())
		}
		agentID, _ := decodeBody(t, accepted)["agent_id"].(string)
		if want := msgKeyBusID + ".solo-1"; agentID != want {
			t.Fatalf("agent id = %q, want %q. A suffix was burned by an enrolment that was REFUSED, which means the messaging key is being validated after the mint rather than before it", agentID, want)
		}
	})

	t.Run("one key may not serve as BOTH the auth and the messaging key", func(t *testing.T) {
		// The session handshake has the SERVER choose the bytes the auth key
		// signs (invariant 3), so an agent that reused that key for messaging
		// would be putting a server-chosen input under the key its PEERS verify.
		//
		// This refusal is NOT what stops a session signature being read as a
		// message signature — DOMAIN SEPARATION does that today, and the client
		// PINS the session prefix, so the two byte languages differ in their
		// first byte (see internal/signing/canonical.go). What the refusal buys
		// is that the separation stops depending on every future signing domain
		// staying disjoint, and that one compromised key is confined to one role.
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		_, _, authB64 := newAuthKeypair(t)

		rec := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("samekey", authB64, authB64, "idem-samekey"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for one key presented in both roles; body %s", rec.Code, rec.Body.String())
		}
		if roster.Len() != 0 {
			t.Fatalf("roster holds %d agents after a rejected enrolment, want 0", roster.Len())
		}
	})

	t.Run("a byte-identical retry replays and applies nothing", func(t *testing.T) {
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		_, _, authB64 := newAuthKeypair(t)
		_, msgB64 := newMsgKeypair(t)
		body := enrolBody("retry", authB64, msgB64, "idem-retry")

		first := postJSON(t, srv, httpapi.RouteEnroll, body)
		if first.Code != http.StatusCreated {
			t.Fatalf("first status = %d, want 201; body %s", first.Code, first.Body.String())
		}
		second := postJSON(t, srv, httpapi.RouteEnroll, body)
		if second.Code != http.StatusCreated {
			t.Fatalf("retry status = %d, want 201; body %s", second.Code, second.Body.String())
		}
		if second.Body.String() != first.Body.String() {
			t.Fatalf("the retry body %s differs from the original %s; a replay is the original result byte for byte", second.Body.String(), first.Body.String())
		}
		if got := second.Result().Header.Get(httpapi.IdempotencyReplayedHeader); got != "true" {
			t.Fatalf("%s = %q on the retry, want \"true\"", httpapi.IdempotencyReplayedHeader, got)
		}
		if roster.Len() != 1 {
			t.Fatalf("roster holds %d agents after one enrolment and one retry, want 1", roster.Len())
		}
	})

	t.Run("the same idempotency key with a DIFFERENT messaging key is a conflict", func(t *testing.T) {
		// Invariant 10: same key + different payload is a protocol violation.
		// The messaging key is part of the payload, and it has to be — if it
		// were not, this second call would be answered as a replay, the roster
		// would keep the FIRST messaging key, and the client would walk away
		// believing the second one was registered. Every message it signed
		// would then fail to verify, for every peer, with nothing pointing at
		// the cause.
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		_, _, authB64 := newAuthKeypair(t)
		msgPub1, msgB64 := newMsgKeypair(t)
		_, otherMsgB64 := newMsgKeypair(t)

		first := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("conflict", authB64, msgB64, "idem-conflict"))
		if first.Code != http.StatusCreated {
			t.Fatalf("first status = %d, want 201; body %s", first.Code, first.Body.String())
		}
		agentID, _ := decodeBody(t, first)["agent_id"].(string)

		second := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("conflict", authB64, otherMsgB64, "idem-conflict"))
		if second.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 for one key carrying two different messaging keys; body %s", second.Code, second.Body.String())
		}
		// The connection is KEPT (narrowed 2026-08-08): this is a client that
		// lost track of its keys, on an unauthenticated route where the socket
		// identifies no principal to punish.
		if got := second.Result().Header.Get("Connection"); strings.EqualFold(got, "close") {
			t.Fatal("the 409 sent Connection: close; enrolment key reuse rejects and logs, it does not disconnect")
		}
		entry, _ := roster.Get(agentID)
		if !bytes.Equal(entry.MessagingPublicKey, msgPub1) {
			t.Fatalf("the rejected call changed the stored messaging key to %x, want the original %x", entry.MessagingPublicKey, msgPub1)
		}
	})

	t.Run("the messaging key does not influence the minted id", func(t *testing.T) {
		// Invariant 1: the server is authoritative on ids and no client-supplied
		// material may reach one. Two agents asking for the SAME name with
		// different messaging keys must be told apart by the SERVER's suffix,
		// and neither id may carry any of the key material.
		roster := auth.NewMemoryRoster()
		srv := newMsgKeyServer(t, roster)
		_, _, authB64A := newAuthKeypair(t)
		_, _, authB64B := newAuthKeypair(t)
		_, msgB64A := newMsgKeypair(t)
		_, msgB64B := newMsgKeypair(t)

		recA := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("twin", authB64A, msgB64A, "idem-twin-a"))
		recB := postJSON(t, srv, httpapi.RouteEnroll, enrolBody("twin", authB64B, msgB64B, "idem-twin-b"))
		if recA.Code != http.StatusCreated || recB.Code != http.StatusCreated {
			t.Fatalf("statuses = %d and %d, want 201 and 201", recA.Code, recB.Code)
		}
		idA, _ := decodeBody(t, recA)["agent_id"].(string)
		idB, _ := decodeBody(t, recB)["agent_id"].(string)
		if idA == idB {
			t.Fatalf("both enrolments minted %q; ids are never reused", idA)
		}
		for _, id := range []string{idA, idB} {
			if !strings.HasPrefix(id, msgKeyBusID+".") {
				t.Fatalf("agent id %q is not fully qualified with the bus id (invariant 2)", id)
			}
			for _, key := range []string{msgB64A, msgB64B, authB64A, authB64B} {
				if strings.Contains(id, strings.TrimRight(key, "=")) {
					t.Fatalf("agent id %q contains client-supplied key material; the id is the SERVER's (invariant 1)", id)
				}
			}
		}
	})
}
