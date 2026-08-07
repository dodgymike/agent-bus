package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// SIGN-6's INGEST POLICY, proved on the wire and proved DURABLY.
//
// # Why every case asserts the WAL and not just the status code
//
// A 400 is easy to produce and easy to produce too late. The property SIGN-6
// actually claims is not "the bus answers 400" — it is that a rejected send
// leaves NO WAL RECORD, NO DELIVERY and NO ACK, because the checks run before
// hub.Send's first durable act. A test that only read the status would pass
// unchanged the day somebody "tidied" one of these checks into the hub, where it
// would sit AFTER the idempotency admission and a malformed request would start
// costing a remembered key.
//
// So each case here counts the durable message records on both sides of the
// request and insists the number did not move, and then asks the recipient what
// it can see. The two together are the claim; the status code is the detail.
//
// The reservation minted at step one is a SEPARATE, EARLIER act and IS spent
// whether or not a send follows — see internal/hub/mint.go, which documents the
// resulting gaps as correct. Nothing here asserts otherwise.

// durableMessageRecords counts the committed message-kind records in the
// server's write-ahead log.
//
// It replays the real file rather than asking the hub what it thinks it holds,
// because "the serving copy has no such message" and "nothing was ever written"
// are different claims and only the second one is SIGN-6's. Replay is a
// read-only pass over the path (internal/wal.Replay opens the file for reading
// and creates nothing), so it is safe against the live log beside it: invariant
// 4 fsyncs before any acknowledgement, so everything acknowledged is on disk by
// the time a handler has returned.
func durableMessageRecords(t *testing.T, srv *httpapi.Server) int {
	t.Helper()
	log, ok := srv.Durable().(*wal.Log)
	if !ok {
		t.Fatalf("the server's durable log is %T, not a *wal.Log; this helper cannot count durable records and every assertion built on it would be vacuous", srv.Durable())
	}
	n := 0
	if _, err := wal.Replay(log.Path(), func(c wal.Committed) error {
		if c.Entry.Kind == store.RecordKind {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("replaying %s to count durable message records: %v", log.Path(), err)
	}
	return n
}

// sendFields is one /v1/send request, field by field, so a case can make
// EXACTLY ONE of them wrong.
//
// It is spelled out rather than built from a mutating helper because the whole
// value of these tests is that the reader can see, per case, that the only thing
// different from a request the bus accepts is the field under test. A builder
// that "fixed up" anything would hide a case that is failing for the wrong
// reason.
type sendFields struct {
	to          string
	bodyB64     string
	key         string
	sender      string
	messageID   string
	seq         uint64
	timestampMs int64
	signature   string
}

func (f sendFields) json() string {
	return fmt.Sprintf(`{"to":%q,"body":%q,"idempotency_key":%q,"sender":%q,"message_id":%q,"seq":%d,"timestamp_ms":%d,"signature":%q}`,
		f.to, f.bodyB64, f.key, f.sender, f.messageID, f.seq, f.timestampMs, f.signature)
}

// wellFormedSend mints a reservation and returns the request that WOULD be
// accepted, so a case can spoil one field and nothing else.
func wellFormedSend(t *testing.T, srv *httpapi.Server, from testAgent, to, key string) sendFields {
	t.Helper()
	msgID, seq := mintOverHTTP(t, srv, from, "send", key)
	return sendFields{
		to:          to,
		bodyB64:     b64("payload for " + key),
		key:         key,
		sender:      from.id,
		messageID:   msgID,
		seq:         seq,
		timestampMs: msgTestTimestampMs,
		signature:   msgTestSignature(),
	}
}

// sigOfLen is a well-formed base64 string decoding to exactly n bytes.
func sigOfLen(n int) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xAB}, n))
}

// rejectedSend issues req and asserts the WHOLE refusal contract: the status,
// the client-facing reason, the absence of a Retry-After, and — the load-bearing
// part — that nothing became durable and nothing was delivered.
func rejectedSend(t *testing.T, srv *httpapi.Server, from testAgent, recipient testAgent, req sendFields, wantStatus int, wantError string) {
	t.Helper()

	before := durableMessageRecords(t, srv)
	deliveredBefore := len(visibleMessages(t, srv, recipient))

	rec := authed(t, srv, from, http.MethodPost, httpapi.RouteSend, req.json())
	if rec.Code != wantStatus {
		t.Fatalf("POST %s = %d, want %d; body %s", httpapi.RouteSend, rec.Code, wantStatus, rec.Body.String())
	}
	if got, _ := decodeBody(t, rec)["error"].(string); got != wantError {
		t.Errorf("error = %q, want %q; the reason a send was refused is a contract surface, not free text", got, wantError)
	}
	// A rejection is TERMINAL for this idempotency key, not transient. A
	// Retry-After would put a well-behaved client into a loop that can never
	// succeed: the same key with the same malformed request is refused
	// identically for ever.
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on a %d; a SIGN-6 rejection is permanent and must not be dressed up as transient", got, wantStatus)
	}

	if after := durableMessageRecords(t, srv); after != before {
		t.Errorf("durable message records went %d -> %d; a SIGN-6 rejection must leave NO WAL RECORD — the check ran after something was written", before, after)
	}
	if after := len(visibleMessages(t, srv, recipient)); after != deliveredBefore {
		t.Errorf("the recipient can see %d messages, was %d; a rejected send must deliver nothing", after, deliveredBefore)
	}
}

// visibleMessages is everything an agent can read right now.
func visibleMessages(t *testing.T, srv *httpapi.Server, a testAgent) []interface{} {
	t.Helper()
	_, msgs := batchOf(t, authed(t, srv, a, http.MethodGet, httpapi.RouteMessages, ""))
	return msgs
}

// TestSendRejectsMalformedSignature covers SIGN-6 checks (a), (b) and (c).
//
// The 63/65 pair is the point of the table and is named explicitly by the task.
// The bound is what makes the check meaningful: written as `>=` or `<=` by
// accident it would admit a truncated or padded signature, and NOTHING
// downstream would notice, because this bus never verifies — it is not entitled
// to (it does not hold the sender's messaging key). Shape is the only thing the
// bus can enforce, so the shape check has to be exact.
func TestSendRejectsMalformedSignature(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	for _, tc := range []struct {
		name      string
		signature string
		wantError string
	}{
		{
			// (a) STRIPPED. Kept distinct from a length failure on purpose:
			// "no signature" must never read as "unsigned but fine", and an
			// attacker removing a signature is a different event from one
			// mangling it.
			name:      "absent",
			signature: "",
			wantError: "a signature is required",
		},
		{
			// An all-padding empty base64 string decodes to zero bytes, which is
			// the same fact spelled differently. It must land on the same answer,
			// or "absent" has two behaviours and only one of them is tested.
			name:      "decodes to zero bytes",
			signature: sigOfLen(0),
			wantError: "a signature is required",
		},
		{
			name:      "not base64",
			signature: "this is not base64!!",
			wantError: "signature is not valid base64",
		},
		{
			// Strict() base64: a signature has EXACTLY ONE spelling. The
			// permissive decoder accepts trailing bits no encoder produces, and
			// anything with several spellings is something an audit trail keyed
			// on the string form sees as several things.
			name:      "non-canonical base64 trailing bits",
			signature: strings.TrimSuffix(sigOfLen(64), "=") + "B=",
			wantError: "signature is not valid base64",
		},
		{
			// (c) ONE BYTE SHORT.
			name:      "63 bytes",
			signature: sigOfLen(ed25519.SignatureSize - 1),
			wantError: "signature must be exactly 64 bytes",
		},
		{
			// (c) ONE BYTE LONG. Both halves of the bound, because a check
			// written with the wrong comparison passes one of them.
			name:      "65 bytes",
			signature: sigOfLen(ed25519.SignatureSize + 1),
			wantError: "signature must be exactly 64 bytes",
		},
		{
			name:      "1 byte",
			signature: sigOfLen(1),
			wantError: "signature must be exactly 64 bytes",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := wellFormedSend(t, srv, alpha, beta.id, "sig-"+strings.ReplaceAll(tc.name, " ", "-"))
			req.signature = tc.signature
			rejectedSend(t, srv, alpha, beta, req, http.StatusBadRequest, tc.wantError)
		})
	}

	// The control. Without it every case above could be passing because the
	// helper builds a request the bus would refuse anyway, and the whole table
	// would be proving nothing about the signature at all.
	t.Run("control: the same request with a 64-byte signature is accepted", func(t *testing.T) {
		req := wellFormedSend(t, srv, alpha, beta.id, "sig-control")
		before := durableMessageRecords(t, srv)
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend, req.json())
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		if after := durableMessageRecords(t, srv); after != before+1 {
			t.Fatalf("durable message records went %d -> %d on an ACCEPTED send, want exactly one more; the counter this whole file rests on does not move", before, after)
		}
	})
}

// TestSendRejectsSenderThatIsNotTheAuthenticatedCaller covers SIGN-6 check (d).
//
// 403, not 400: the request is well formed and re-sending it will not help. The
// check exists because the canonical format binds a signature to a sender, so a
// client signing as somebody else produces a message whose signed content
// contradicts the identity the bus recorded — a recipient would resolve a
// verification key for the NAMED sender and either fail confusingly or, worse,
// succeed against a key the real sender never used.
func TestSendRejectsSenderThatIsNotTheAuthenticatedCaller(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")
	gamma := enrolAndAuthenticate(t, srv, "gamma")

	for _, tc := range []struct {
		name   string
		sender string
	}{
		// The one that matters: a REAL, ENROLLED agent of this bus. If the bus
		// ever took body.Sender as the identity, this is the request that would
		// attribute alpha's message to gamma.
		{name: "another enrolled agent", sender: gamma.id},
		{name: "an unenrolled but well-formed id", sender: msgTestBusID + ".ghost-9"},
		{name: "an agent of another bus", sender: "bus-elsewhere.alpha-1"},
		{name: "absent", sender: ""},
		{name: "the bare name without the bus qualifier", sender: "alpha"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := wellFormedSend(t, srv, alpha, beta.id, "sender-"+strings.ReplaceAll(tc.name, " ", "-"))
			req.sender = tc.sender
			rejectedSend(t, srv, alpha, beta, req, http.StatusForbidden, "sender does not match the authenticated caller")
		})
	}

	// The claimed name is NOT echoed back to the client — it chose it, so
	// repeating it teaches the client nothing and hands an attacker a reflector.
	t.Run("the claimed sender is not reflected to the client", func(t *testing.T) {
		req := wellFormedSend(t, srv, alpha, beta.id, "sender-not-reflected")
		req.sender = gamma.id
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend, req.json())
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if strings.Contains(rec.Body.String(), gamma.id) {
			t.Errorf("the 403 body echoes the claimed sender %q: %s", gamma.id, rec.Body.String())
		}
	})
}

// TestSendRejectsMalformedMintedMessageID covers SIGN-6 check (e).
//
// The bus-half case is the one worth naming: an id minted by ANOTHER bus that
// happened to match a local reservation's sequence would be a message attributed
// to this bus's total order while carrying a foreign origin, and origin is
// exactly what SIGN-1 made the signature cover.
//
// The seq-disagreement case is the other: both halves travel on the wire because
// the canonical format encodes the sequence separately AS WELL AS inside the id,
// so the two must agree before anything is signed over them. A mismatch is
// either a client splicing two operations together or an attempt to have one
// number signed and a different one recorded.
func TestSendRejectsMalformedMintedMessageID(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	for _, tc := range []struct {
		name string
		// spoil receives the reservation this bus really minted and returns the
		// id and seq to present instead.
		spoil func(msgID string, seq uint64) (string, uint64)
	}{
		{
			name: "the bus half is another bus",
			spoil: func(_ string, seq uint64) (string, uint64) {
				return fmt.Sprintf("bus-elsewhere-%d", seq), seq
			},
		},
		{
			name: "the bus half is a prefix of this bus",
			spoil: func(_ string, seq uint64) (string, uint64) {
				return fmt.Sprintf("%s-%d", msgTestBusID[:len(msgTestBusID)-1], seq), seq
			},
		},
		{
			name: "seq disagrees with the sequence inside the id",
			spoil: func(msgID string, seq uint64) (string, uint64) {
				return msgID, seq + 1
			},
		},
		{
			name: "seq is 0 while the id carries a real sequence",
			spoil: func(msgID string, _ uint64) (string, uint64) {
				return msgID, 0
			},
		},
		{
			name: "the id carries sequence 0",
			spoil: func(_ string, _ uint64) (string, uint64) {
				return msgTestBusID + "-0", 0
			},
		},
		{
			// One id has ONE spelling. "007" and "7" are different byte
			// sequences that canonicalize to different signed messages while a
			// consumer keyed on the parsed pair sees one, and an attacker
			// chooses which.
			name: "a leading-zero spelling of a real sequence",
			spoil: func(_ string, seq uint64) (string, uint64) {
				return fmt.Sprintf("%s-0%d", msgTestBusID, seq), seq
			},
		},
		{
			name: "no separator at all",
			spoil: func(_ string, seq uint64) (string, uint64) {
				return msgTestBusID, seq
			},
		},
		{
			name: "empty",
			spoil: func(_ string, seq uint64) (string, uint64) {
				return "", seq
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := wellFormedSend(t, srv, alpha, beta.id, "mid-"+strings.ReplaceAll(tc.name, " ", "-"))
			req.messageID, req.seq = tc.spoil(req.messageID, req.seq)
			rejectedSend(t, srv, alpha, beta, req, http.StatusBadRequest, "invalid message id")
		})
	}
}

// TestSendRejectsNonPositiveTimestamp covers SIGN-6 check (f).
//
// 0 means "unset" and a negative value is a pre-1970 instant. Both are refused
// rather than DEFAULTED, and the defaulting is the trap: the timestamp is
// covered by the signature, so a bus that substituted its own clock would store
// a message whose recorded content no recipient can reproduce, and every
// signature over it would fail to verify for a reason no client could diagnose.
func TestSendRejectsNonPositiveTimestamp(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	for _, tc := range []struct {
		name string
		ts   int64
	}{
		{name: "absent", ts: 0},
		{name: "minus one", ts: -1},
		{name: "a pre-1970 instant", ts: -1754130896789},
		{name: "the most negative int64", ts: -1 << 63},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := wellFormedSend(t, srv, alpha, beta.id, "ts-"+strings.ReplaceAll(tc.name, " ", "-"))
			req.timestampMs = tc.ts
			rejectedSend(t, srv, alpha, beta, req, http.StatusBadRequest, "timestamp_ms is required")
		})
	}
}

// TestSignRejectionIsTerminalForItsIdempotencyKey is the fourth named case, and
// the one that is about STATE rather than about a status code.
//
// A rejected send must be terminal in BOTH directions:
//
//   - Forwards: replaying the same malformed request must be refused
//     identically, for ever, with no Retry-After and no escalation. Anything
//     else is a client spinning on a request that can never succeed.
//   - Backwards: it must not leave the key HALF-APPLIED. The refusal happens
//     before hub.Send, so no applied-key record exists — which means the client
//     that repairs its request and re-presents the SAME key must get exactly one
//     message out of it, and a further retry must replay that one rather than
//     writing a second.
//
// The second half is the one a status-code test cannot see, and it is where a
// check moved "for tidiness" into the hub would show up: there the rejection
// would land after the idempotency admission and the repaired send would come
// back 409 for a message that was never written.
func TestSignRejectionIsTerminalForItsIdempotencyKey(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")
	beta := enrolAndAuthenticate(t, srv, "beta")

	const key = "terminal-1"
	req := wellFormedSend(t, srv, alpha, beta.id, key)
	req.signature = sigOfLen(ed25519.SignatureSize - 1)

	before := durableMessageRecords(t, srv)

	var bodies []string
	for attempt := 1; attempt <= 3; attempt++ {
		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend, req.json())
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d = %d, want 400 every time; body %s", attempt, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("attempt %d carried Retry-After %q; a terminal refusal must never invite a retry loop", attempt, got)
		}
		if got := rec.Header().Get("Connection"); strings.EqualFold(got, "close") {
			t.Errorf("attempt %d disconnected the client; a malformed signature is a refusal, not invariant 10's key-reuse violation", attempt)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("attempt %d answered\n  %s\nbut attempt 1 answered\n  %s\na terminal refusal is the SAME refusal every time", i+1, bodies[i], bodies[0])
		}
	}
	if after := durableMessageRecords(t, srv); after != before {
		t.Fatalf("durable message records went %d -> %d across three REFUSED sends", before, after)
	}

	t.Run("the key is not left half-applied: a repaired send under it is admitted exactly once", func(t *testing.T) {
		// The client repairs its signature and re-presents the SAME key. The
		// reservation is still outstanding — a mint is spent at /v1/mint, not at
		// /v1/send — so this is one message, not a second one.
		repaired := req
		repaired.signature = msgTestSignature()

		rec := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend, repaired.json())
		if rec.Code != http.StatusCreated {
			t.Fatalf("the repaired send = %d, want 201; a refusal that ran before hub.Send must not have consumed the key. body %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(httpapi.IdempotencyReplayedHeader); got != "" {
			t.Errorf("%s = %q on the FIRST accepted send under this key; the refusals must not have recorded anything", httpapi.IdempotencyReplayedHeader, got)
		}
		if after := durableMessageRecords(t, srv); after != before+1 {
			t.Fatalf("durable message records went %d -> %d, want exactly one more", before, after)
		}

		// And now it IS applied, so the same request again is a legitimate retry
		// and replays rather than writing a second message.
		again := authed(t, srv, alpha, http.MethodPost, httpapi.RouteSend, repaired.json())
		if again.Code != http.StatusCreated {
			t.Fatalf("the retry = %d, want 201; a legitimate retry is never punished (invariant 10). body %s", again.Code, again.Body.String())
		}
		if got := again.Header().Get(httpapi.IdempotencyReplayedHeader); got != "true" {
			t.Errorf("%s = %q on a retry, want \"true\"", httpapi.IdempotencyReplayedHeader, got)
		}
		if after := durableMessageRecords(t, srv); after != before+1 {
			t.Fatalf("durable message records went %d -> %d across a retry; the retry was re-applied", before, after)
		}
		if got := len(visibleMessages(t, srv, beta)); got != 1 {
			t.Fatalf("beta can see %d messages, want exactly 1 out of one rejected, one accepted and one replayed send", got)
		}
	})
}

// TestMintReplayBurnsNoSequence is the durable half of invariant 10 on
// /v1/mint.
//
// A re-mint returning the same numbers is easy to get right in the response and
// easy to get wrong underneath: an implementation that allocated first and then
// noticed the key was already held would answer correctly and still have burned
// a number. That is not cosmetic — a sequence handed out is burned for ever, and
// a client that retries a lost mint a hundred times would consume a hundred
// numbers while being told it consumed one.
//
// The proof is arithmetic and needs a witness: mint under a FRESH key afterwards
// and insist the allocator moved by EXACTLY ONE.
func TestMintReplayBurnsNoSequence(t *testing.T) {
	srv, _ := newMessagingServer(t)
	alpha := enrolAndAuthenticate(t, srv, "alpha")

	first := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"send","idempotency_key":"burn-1"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", first.Code, first.Body.String())
	}
	body := decodeBody(t, first)
	msgID, _ := body["message_id"].(string)
	rawSeq, _ := body["seq"].(float64)
	seq := uint64(rawSeq)
	if seq == 0 {
		t.Fatalf("the first mint returned seq %v; sequence 0 is never allocated", body["seq"])
	}

	for attempt := 2; attempt <= 5; attempt++ {
		again := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"send","idempotency_key":"burn-1"}`)
		if again.Code != http.StatusCreated {
			t.Fatalf("re-mint %d = %d, want 201; body %s", attempt, again.Code, again.Body.String())
		}
		if got := again.Header().Get(httpapi.IdempotencyReplayedHeader); got != "true" {
			t.Errorf("re-mint %d: %s = %q, want \"true\"", attempt, httpapi.IdempotencyReplayedHeader, got)
		}
		if again.Body.String() != first.Body.String() {
			t.Fatalf("re-mint %d answered\n  %s\nwant the ORIGINAL byte for byte\n  %s", attempt, again.Body.String(), first.Body.String())
		}
	}

	// THE WITNESS. Four replays burned nothing, so the very next fresh key must
	// receive seq+1. Anything higher is a number handed to nobody.
	next := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"send","idempotency_key":"burn-2"}`)
	if next.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", next.Code, next.Body.String())
	}
	nextBody := decodeBody(t, next)
	nextRaw, _ := nextBody["seq"].(float64)
	if uint64(nextRaw) != seq+1 {
		t.Fatalf("the next fresh mint took seq %v, want %d: four replays of %q burned %d sequence numbers between them",
			nextBody["seq"], seq+1, msgID, uint64(nextRaw)-seq-1)
	}

	t.Run("a different op under the same key is a DIFFERENT reservation", func(t *testing.T) {
		// The mint is scoped by (agent, op, key). Minting under "send" and
		// spending under "broadcast" with one key must not be one reservation,
		// or one route's idempotency key would shadow the other's.
		other := authed(t, srv, alpha, http.MethodPost, httpapi.RouteMint, `{"op":"broadcast","idempotency_key":"burn-1"}`)
		if other.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", other.Code, other.Body.String())
		}
		if got := other.Header().Get(httpapi.IdempotencyReplayedHeader); got != "" {
			t.Errorf("%s = %q: a different op under the same key was answered as a replay of the send reservation", httpapi.IdempotencyReplayedHeader, got)
		}
		if got, _ := decodeBody(t, other)["message_id"].(string); got == msgID {
			t.Errorf("message_id = %q, the same one \"send\" holds; the op is part of the mint's scope, not decoration", got)
		}
	})

	t.Run("another agent's key is its own reservation", func(t *testing.T) {
		// The scope names the AGENT too, so one agent cannot collide with — or
		// probe for — another's keys.
		beta := enrolAndAuthenticate(t, srv, "beta")
		rec := authed(t, srv, beta, http.MethodPost, httpapi.RouteMint, `{"op":"send","idempotency_key":"burn-1"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(httpapi.IdempotencyReplayedHeader); got != "" {
			t.Errorf("%s = %q: beta was handed alpha's reservation", httpapi.IdempotencyReplayedHeader, got)
		}
		got := decodeBody(t, rec)
		if got["message_id"] == msgID {
			t.Errorf("beta was minted alpha's message id %q", msgID)
		}
		if got["sender"] != beta.id {
			t.Errorf("sender = %v, want the AUTHENTICATED principal %q", got["sender"], beta.id)
		}
	})
}
