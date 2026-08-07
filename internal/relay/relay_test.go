package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
)

// testBody is the body every fixture relays unless it overrides it.
var testBody = []byte("hello from another bus")

// relayFixture builds a VALID, GENUINELY SIGNED relay envelope originating on
// peerBus and addressed to an agent on localBus, then applies each mod. Every
// rejection case in the table below is exactly one deviation from this baseline,
// so a failure names the deviation rather than the whole payload.
//
// THE SIGNATURE IS APPLIED AFTER THE MODS, over whatever the mods produced, so
// the baseline is a message a real origin agent could have sent (SIGN-7). A mod
// that makes the envelope UNCANONICALIZABLE — a malformed id, a duplicate
// recipient, an unset timestamp — gets a placeholder of the right LENGTH
// instead. That is not a bypass: every such deviation is refused by checks 1-10
// of ValidateRelayRequest, all of which run BEFORE the signature is looked at,
// so the case under test still fails for the reason it names. When the
// signature ITSELF is the subject, sign explicitly — see signed_test.go.
func relayFixture(mods ...func(*RelayRequest)) RelayRequest {
	req := RelayRequest{
		OriginBus:          peerBus,
		MessageID:          peerBus + "-1",
		Sender:             peerBus + ".beta-1",
		Broadcast:          false,
		Recipients:         []string{localBus + ".alpha-1"},
		BusPath:            []string{peerBus},
		TimestampUnixMilli: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC).UnixMilli(),
		Size:               len(testBody),
		ContentSHA256:      store.ContentHash(testBody),
		Body:               append([]byte(nil), testBody...),
	}
	for _, mod := range mods {
		mod(&req)
	}
	if err := signRelay(&req); err != nil {
		// Not canonicalizable, so no signature over it can exist for anyone.
		// A right-length placeholder keeps the deviation under test — not the
		// missing signature — the thing that decides the outcome.
		req.Signature = make([]byte, ed25519.SignatureSize)
	}
	return req
}

// relayResponder is a RelayHandler on a TLS test server plus the record of what
// its AcceptRelay callback was handed. Same harness shape as responder.
type relayResponder struct {
	srv *httptest.Server
	h   *RelayHandler

	mu       sync.Mutex
	accepted []RelayedMessage
	result   RelayAcceptance
	err      error
}

func newRelayResponder(t *testing.T, busID string, cfg func(*RelayConfig)) *relayResponder {
	t.Helper()
	r := &relayResponder{result: RelayAcceptance{LocalMessageID: busID + "-1"}}
	c := RelayConfig{
		BusID: busID,
		// The whole test federation is peered by default, so a test that is not
		// ABOUT trust gets the ordinary case. Override c.Trust to narrow it.
		Trust: fakeCrossBusTrustForTest,
		AcceptRelay: func(_ context.Context, m RelayedMessage) (RelayAcceptance, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.err != nil {
				return RelayAcceptance{}, r.err
			}
			r.accepted = append(r.accepted, m)
			return r.result, nil
		},
	}
	if cfg != nil {
		cfg(&c)
	}
	h, err := NewRelayHandler(c)
	if err != nil {
		t.Fatalf("NewRelayHandler: %v", err)
	}
	r.h = h
	r.srv = httptest.NewTLSServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *relayResponder) failWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *relayResponder) answerWith(acc RelayAcceptance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.result = acc
}

func (r *relayResponder) acceptedMessages() []RelayedMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RelayedMessage(nil), r.accepted...)
}

// postRelayRaw sends a body a Client would never construct, which is exactly
// what a hostile peer sends. It returns the status, the stable error code (for
// a non-200) and the decoded RelayResponse (for a 200).
func (r *relayResponder) postRelayRaw(t *testing.T, contentType, key string, body []byte) (int, string, RelayResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+PeerRelayPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set(idem.HeaderName, key)
	}
	return r.doRelay(t, req)
}

func (r *relayResponder) postRelay(t *testing.T, req RelayRequest) (int, string, RelayResponse) {
	t.Helper()
	return r.postRelayWithKey(t, req.MessageID, req)
}

func (r *relayResponder) postRelayWithKey(t *testing.T, key string, req RelayRequest) (int, string, RelayResponse) {
	t.Helper()
	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return r.postRelayRaw(t, "application/json", key, buf)
}

func (r *relayResponder) doRelay(t *testing.T, req *http.Request) (int, string, RelayResponse) {
	t.Helper()
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		var body ErrorBody
		if err := json.Unmarshal(buf, &body); err != nil {
			t.Fatalf("error body %q is not JSON: %v", string(buf), err)
		}
		return resp.StatusCode, body.Error, RelayResponse{}
	}
	var body RelayResponse
	if err := json.Unmarshal(buf, &body); err != nil {
		t.Fatalf("success body %q is not JSON: %v", string(buf), err)
	}
	return resp.StatusCode, "", body
}

// TestMessageRelay is RELAY-2's proof: a message crosses a bus boundary with
// its origin identity intact, every way a peer can send an incoherent envelope
// is refused, and the handler's status mapping says what each outcome means.
func TestMessageRelay(t *testing.T) {
	t.Run("carries a message across a bus boundary", func(t *testing.T) {
		remote := newRelayResponder(t, localBus, nil)
		remote.answerWith(RelayAcceptance{LocalMessageID: localBus + "-42"})

		local := newInitiator(t, peerBus, nil, remote.srv)
		req := relayFixture()
		resp, err := local.Relay(context.Background(), remote.srv.URL, req)
		if err != nil {
			t.Fatalf("Relay: %v", err)
		}
		if !resp.Accepted || resp.Duplicate || resp.DroppedReason != "" {
			t.Fatalf("response = %+v, want accepted with no drop", resp)
		}
		// THE RECEIVING BUS MINTS ITS OWN ID (invariant 1, PROTOCOL.md §8.5).
		if resp.MessageID != localBus+"-42" {
			t.Errorf("message id = %q, want the RECEIVING bus's own id %q; a relayed id is never carried in from a peer", resp.MessageID, localBus+"-42")
		}

		got := remote.acceptedMessages()
		if len(got) != 1 {
			t.Fatalf("AcceptRelay was called %d times, want 1", len(got))
		}
		m := got[0]
		if m.OriginBus != peerBus || m.OriginMessageID != peerBus+"-1" || m.OriginSeq != 1 {
			t.Errorf("origin = %q/%q/%d, want %q/%q/1", m.OriginBus, m.OriginMessageID, m.OriginSeq, peerBus, peerBus+"-1")
		}
		if m.Sender != peerBus+".beta-1" {
			t.Errorf("sender = %q", m.Sender)
		}
		if string(m.Body) != string(testBody) {
			t.Errorf("body = %q, want %q", m.Body, testBody)
		}
		// The path is as RECEIVED and does NOT yet include us.
		if len(m.BusPath) != 1 || m.BusPath[0] != peerBus {
			t.Errorf("bus path = %v, want [%s]: Forward appends our hop, ingress does not", m.BusPath, peerBus)
		}
		// The key IS the origin message id.
		if m.IdempotencyKey != m.OriginMessageID {
			t.Errorf("idempotency key = %q, want the origin message id %q", m.IdempotencyKey, m.OriginMessageID)
		}
	})

	t.Run("refuses an incoherent envelope", func(t *testing.T) {
		tooManyRecipients := make([]string, store.MaxRecipients+1)
		for i := range tooManyRecipients {
			tooManyRecipients[i] = fmt.Sprintf("%s.a%d-1", localBus, i)
		}
		bigBody := make([]byte, store.MaxBodyBytes+1)

		cases := []struct {
			name    string
			key     string // "" means "the origin message id"; "-" means no header
			mod     func(*RelayRequest)
			want    error
			because string
		}{
			{
				name:    "path does not name the origin",
				mod:     func(r *RelayRequest) { r.BusPath = []string{thirdBus} },
				want:    ErrInvalidRelay,
				because: "two claims about where a message came from is one claim too many",
			},
			{
				name:    "origin claims our bus id",
				mod:     func(r *RelayRequest) { r.OriginBus, r.BusPath = localBus, []string{localBus} },
				want:    ErrRelayLoop,
				because: "our own bus on the path is a loop, and the loop check runs first",
			},
			{
				name: "message id is minted by another bus",
				mod: func(r *RelayRequest) {
					r.MessageID = thirdBus + "-1"
				},
				key:     thirdBus + "-1",
				want:    ErrInvalidRelay,
				because: "a bus is the authority for its own message ids and for nobody else's",
			},
			{
				name:    "malformed message id",
				mod:     func(r *RelayRequest) { r.MessageID = peerBus + "-0" },
				key:     peerBus + "-0",
				want:    ErrInvalidRelay,
				because: "sequence 0 is never allocated",
			},
			{
				name:    "sender belongs to a third bus",
				mod:     func(r *RelayRequest) { r.Sender = thirdBus + ".delta-1" },
				want:    ErrInvalidRelay,
				because: "an origin bus speaks only for its own agents",
			},
			{
				name:    "unqualified sender",
				mod:     func(r *RelayRequest) { r.Sender = "beta-1" },
				want:    ErrInvalidRelay,
				because: "an unqualified id has no bus half and cannot be attributed (invariant 2)",
			},
			{
				name:    "too many recipients",
				mod:     func(r *RelayRequest) { r.Recipients = tooManyRecipients },
				want:    ErrInvalidRelay,
				because: "the count is refused before any recipient is parsed",
			},
			{
				name:    "malformed recipient",
				mod:     func(r *RelayRequest) { r.Recipients = []string{"not-an-agent-id"} },
				want:    ErrInvalidRelay,
				because: "a recipient we cannot parse is a recipient we cannot deliver to",
			},
			{
				name: "duplicate recipient",
				mod: func(r *RelayRequest) {
					r.Recipients = []string{localBus + ".alpha-1", localBus + ".alpha-1"}
				},
				want:    ErrInvalidRelay,
				because: "a recipient list is a set",
			},
			{
				name:    "broadcast with recipients",
				mod:     func(r *RelayRequest) { r.Broadcast = true },
				want:    ErrInvalidRelay,
				because: "store.Decode's rule: a broadcast carries no recipient list",
			},
			{
				name:    "directed with no recipients",
				mod:     func(r *RelayRequest) { r.Recipients = nil },
				want:    ErrInvalidRelay,
				because: "store.Decode's rule: a directed message names at least one recipient",
			},
			{
				name: "oversized body",
				mod: func(r *RelayRequest) {
					r.Body = bigBody
					r.Size = len(bigBody)
					r.ContentSHA256 = store.ContentHash(bigBody)
				},
				want:    ErrInvalidRelay,
				because: "the body cap is store.MaxBodyBytes, checked before anything reads it",
			},
			{
				name:    "declared size disagrees with the body",
				mod:     func(r *RelayRequest) { r.Size = len(testBody) + 1 },
				want:    ErrInvalidRelay,
				because: "a lying size is a rejection, never an allocation",
			},
			{
				name:    "content hash does not match",
				mod:     func(r *RelayRequest) { r.ContentSHA256 = store.ContentHash([]byte("something else")) },
				want:    ErrInvalidRelay,
				because: "a body that does not hash to its declared digest is not the body that was sent",
			},
			{
				name:    "unset timestamp",
				mod:     func(r *RelayRequest) { r.TimestampUnixMilli = 0 },
				want:    ErrInvalidRelay,
				because: "0 means the field was never set, not the epoch",
			},
			{
				name:    "empty bus path",
				mod:     func(r *RelayRequest) { r.BusPath = nil },
				want:    ErrInvalidBusPath,
				because: "every relayed message has traversed at least its origin",
			},
			{
				name:    "the key is not the origin message id",
				key:     "some-other-key",
				want:    ErrRelayKeyMismatch,
				because: "a per-hop key would make every copy look new and defeat dedupe silently",
			},
			{
				name:    "no idempotency key at all",
				key:     "-",
				want:    idem.ErrMissingKey,
				because: "relay is a mutating call and must be safe to retry (invariant 10)",
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				req := relayFixture()
				if tc.mod != nil {
					tc.mod(&req)
				}
				key := tc.key
				switch key {
				case "":
					key = req.MessageID
				case "-":
					key = ""
				}
				_, err := ValidateRelayRequest(localBus, key, req, fakeCrossBusTrustForTest)
				if !errors.Is(err, tc.want) {
					t.Fatalf("ValidateRelayRequest error = %v, want one wrapping %v (%s)", err, tc.want, tc.because)
				}
			})
		}
	})

	t.Run("maps every outcome to a status a peer can act on", func(t *testing.T) {
		t.Run("wrong method", func(t *testing.T) {
			remote := newRelayResponder(t, localBus, nil)
			req, err := http.NewRequest(http.MethodGet, remote.srv.URL+PeerRelayPath, nil)
			if err != nil {
				t.Fatalf("building request: %v", err)
			}
			if status, code, _ := remote.doRelay(t, req); status != http.StatusMethodNotAllowed || code != CodeMethodNotAllowed {
				t.Errorf("GET gave %d/%q, want %d/%q", status, code, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
			}
		})

		t.Run("wrong content type", func(t *testing.T) {
			remote := newRelayResponder(t, localBus, nil)
			if status, code, _ := remote.postRelayRaw(t, "text/plain", peerBus+"-1", []byte(`{}`)); status != http.StatusUnsupportedMediaType || code != CodeUnsupportedMediaType {
				t.Errorf("text/plain gave %d/%q, want %d/%q", status, code, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
			}
		})

		t.Run("oversized body", func(t *testing.T) {
			// The bound is enforced on BYTES READ, before any decoding, and
			// without believing Content-Length: the envelope below is
			// syntactically fine and simply too long.
			remote := newRelayResponder(t, localBus, func(c *RelayConfig) { c.MaxRequestBytes = 64 })
			if status, code, _ := remote.postRelay(t, relayFixture()); status != http.StatusRequestEntityTooLarge || code != CodePayloadTooLarge {
				t.Errorf("oversized body gave %d/%q, want %d/%q", status, code, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
			}
		})

		t.Run("unknown field", func(t *testing.T) {
			remote := newRelayResponder(t, localBus, nil)
			body := []byte(`{"origin_bus":"` + peerBus + `","trust_me":true}`)
			if status, code, _ := remote.postRelayRaw(t, "application/json", peerBus+"-1", body); status != http.StatusBadRequest || code != CodeInvalidRequest {
				t.Errorf("unknown field gave %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeInvalidRequest)
			}
		})

		t.Run("incoherent envelope", func(t *testing.T) {
			remote := newRelayResponder(t, localBus, nil)
			req := relayFixture(func(r *RelayRequest) { r.Size = 9999 })
			if status, code, _ := remote.postRelay(t, req); status != http.StatusBadRequest || code != CodeInvalidRelay {
				t.Errorf("got %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeInvalidRelay)
			}
			if n := len(remote.acceptedMessages()); n != 0 {
				t.Errorf("AcceptRelay was called %d times for a rejected envelope, want 0", n)
			}
		})

		t.Run("an idempotency violation is 409 and names the disconnect", func(t *testing.T) {
			remote := newRelayResponder(t, localBus, nil)
			remote.failWith(fmt.Errorf("the applied-key table disagrees: %w", ErrIdempotencyViolation))
			if status, code, _ := remote.postRelay(t, relayFixture()); status != http.StatusConflict || code != CodeIdempotencyViolation {
				t.Errorf("got %d/%q, want %d/%q: same key, different payload is a protocol violation (invariant 10)", status, code, http.StatusConflict, CodeIdempotencyViolation)
			}
		})

		t.Run("a broken bus is 503, not 400", func(t *testing.T) {
			remote := newRelayResponder(t, localBus, nil)
			remote.failWith(errors.New("the durable store is unavailable"))
			if status, code, _ := remote.postRelay(t, relayFixture()); status != http.StatusServiceUnavailable || code != CodeUnavailable {
				t.Errorf("got %d/%q, want %d/%q: a peer must be able to tell 'not now' from 'never'", status, code, http.StatusServiceUnavailable, CodeUnavailable)
			}
		})
	})

	// THE STATUS THAT MATTERS MOST. A 4xx/5xx here would make retry/backoff
	// re-deliver, forever, a message that can never be accepted.
	t.Run("a loop is 200 with a dropped reason, never an error status", func(t *testing.T) {
		remote := newRelayResponder(t, localBus, nil)
		req := relayFixture(func(r *RelayRequest) { r.BusPath = []string{peerBus, localBus, thirdBus} })

		status, code, body := remote.postRelay(t, req)
		if status != http.StatusOK {
			t.Fatalf("a loop drop gave status %d/%q, want 200: an error status would make a retrying sender re-deliver a message that can NEVER be accepted, which is the traffic amplification RELAY-3 exists to stop", status, code)
		}
		if body.Accepted {
			t.Error("a looping message was reported as accepted")
		}
		if body.DroppedReason != DropLoop {
			t.Errorf("dropped_reason = %q, want %q", body.DroppedReason, DropLoop)
		}
		if body.MessageID != "" {
			t.Errorf("message id = %q, want empty: nothing was minted", body.MessageID)
		}
		if n := len(remote.acceptedMessages()); n != 0 {
			t.Errorf("AcceptRelay was called %d times for a looping message, want 0: the drop must be cheap", n)
		}
		if got := remote.h.Stats().LoopDrops; got != 1 {
			t.Errorf("LoopDrops = %d, want 1: a loop drop is counted so an operator can see the mesh's shape", got)
		}
	})

	t.Run("a duplicate is 200 carrying the ORIGINAL local id", func(t *testing.T) {
		remote := newRelayResponder(t, localBus, nil)
		remote.answerWith(RelayAcceptance{LocalMessageID: localBus + "-7", Duplicate: true})

		status, _, body := remote.postRelay(t, relayFixture())
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: a legitimate retry must not be punished (invariant 10)", status)
		}
		if !body.Accepted || !body.Duplicate {
			t.Errorf("response = %+v, want accepted and duplicate", body)
		}
		if body.MessageID != localBus+"-7" {
			t.Errorf("message id = %q, want the ORIGINAL %q replayed verbatim", body.MessageID, localBus+"-7")
		}
		if got := remote.h.Stats().Duplicates; got != 1 {
			t.Errorf("Duplicates = %d, want 1", got)
		}
	})

	t.Run("error bodies never echo peer input", func(t *testing.T) {
		remote := newRelayResponder(t, localBus, nil)
		marker := "canary-relay-marker"
		req := relayFixture(func(r *RelayRequest) { r.Sender = marker + "-bus.Bad-1" })
		status, code, _ := remote.postRelay(t, req)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; this test only means something on a rejection", status)
		}
		if code != CodeInvalidRelay {
			t.Errorf("code = %q, want %q", code, CodeInvalidRelay)
		}

		// Re-read the raw body: the code check above already decoded it, so
		// assert on the bytes themselves.
		buf, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		httpReq, err := http.NewRequest(http.MethodPost, remote.srv.URL+PeerRelayPath, strings.NewReader(string(buf)))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set(idem.HeaderName, req.MessageID)
		resp, err := remote.srv.Client().Do(httpReq)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(string(raw), marker) {
			t.Fatalf("error body %q echoes peer-supplied input", string(raw))
		}
	})

	t.Run("NewRelayHandler refuses a config whose failure would be silent", func(t *testing.T) {
		accept := func(context.Context, RelayedMessage) (RelayAcceptance, error) {
			return RelayAcceptance{}, nil
		}
		trust := fakeCrossBusTrustForTest
		cases := []struct {
			name string
			cfg  RelayConfig
		}{
			{"no bus id", RelayConfig{AcceptRelay: accept, Trust: trust}},
			{"bus id with a dot", RelayConfig{BusID: "bus.x", AcceptRelay: accept, Trust: trust}},
			{"no accept callback", RelayConfig{BusID: localBus, Trust: trust}},
			// SIGN-7: a handler without a CrossBusTrust would answer 403 to every
			// well-formed message a correct peer sent — an outage that looks
			// exactly like a peer with the wrong keys. Failing at CONSTRUCTION is
			// what says which side is broken.
			{"no cross-bus trust", RelayConfig{BusID: localBus, AcceptRelay: accept}},
			{"negative byte cap", RelayConfig{BusID: localBus, AcceptRelay: accept, Trust: trust, MaxRequestBytes: -1}},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				if _, err := NewRelayHandler(tc.cfg); err == nil {
					t.Fatal("NewRelayHandler accepted an incomplete config")
				}
			})
		}
	})
}

// TestRelayEnvelopeBoundsAreLimitsNotOffByOnes walks each field cap in
// ValidateRelayRequest to its EXACT value and one past it.
//
// The rejection table above only ever approaches these caps from the far side,
// which proves the bound exists but not where it is. Both sides matter and they
// fail differently: one-too-permissive accepts a message store.Decode would
// then refuse, which is the acknowledged-but-not-persistable message invariant
// 5 forbids; one-too-strict silently refuses traffic the operator was told is
// legal, from a correct peer, with a 400 that blames the sender.
func TestRelayEnvelopeBoundsAreLimitsNotOffByOnes(t *testing.T) {
	recipients := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s.a%d-1", localBus, i)
		}
		return out
	}
	// A path whose first hop is the origin (check 3 requires it) and which does
	// NOT contain us, so the loop check is not what decides the outcome.
	path := func(n int) []string {
		out := make([]string, 0, n)
		out = append(out, peerBus)
		for i := 1; i < n; i++ {
			out = append(out, fmt.Sprintf("bus-h%d", i))
		}
		return out
	}
	body := func(n int) func(*RelayRequest) {
		buf := bytes.Repeat([]byte("b"), n)
		return func(r *RelayRequest) {
			r.Body = buf
			r.Size = len(buf)
			r.ContentSHA256 = store.ContentHash(buf)
		}
	}

	cases := []struct {
		name string
		mod  func(*RelayRequest)
		want error // nil means "must be accepted"
	}{
		{
			name: "recipients exactly at store.MaxRecipients",
			mod:  func(r *RelayRequest) { r.Recipients = recipients(store.MaxRecipients) },
		},
		{
			name: "one recipient over store.MaxRecipients",
			mod:  func(r *RelayRequest) { r.Recipients = recipients(store.MaxRecipients + 1) },
			want: ErrInvalidRelay,
		},
		{
			name: "a body of exactly store.MaxBodyBytes",
			mod:  body(store.MaxBodyBytes),
		},
		{
			name: "a body one byte over store.MaxBodyBytes",
			mod:  body(store.MaxBodyBytes + 1),
			want: ErrInvalidRelay,
		},
		{
			name: "a bus path of exactly MaxBusPath hops",
			mod:  func(r *RelayRequest) { r.BusPath = path(MaxBusPath) },
		},
		{
			name: "a bus path one hop over MaxBusPath",
			mod:  func(r *RelayRequest) { r.BusPath = path(MaxBusPath + 1) },
			want: ErrBusPathTooLong,
		},
		{
			name: "a single-hop path is the minimum, and is accepted",
			mod:  func(r *RelayRequest) { r.BusPath = path(1) },
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := relayFixture(tc.mod)
			m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("ValidateRelayRequest error = %v, want one wrapping %v", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("a message EXACTLY at the cap was refused: %v; the cap is a limit, not an off-by-one", err)
			}
			// And what we accepted is what we can still forward and persist: a
			// path at the cap cannot take our hop, which is a documented
			// ErrBusPathTooLong rather than a surprise at the store.
			if _, ferr := m.Forward(localBus); len(m.BusPath) == MaxBusPath && !errors.Is(ferr, ErrBusPathTooLong) {
				t.Fatalf("Forward on a path already at the cap gave %v, want one wrapping ErrBusPathTooLong", ferr)
			}
		})
	}
}

// TestRelayClientCannotBeMadeToEmitAnInjectedOrUnboundedLogLine covers the
// "the response steers our behaviour" class, of which the redirect-following
// HIGH a previous review found was one instance.
//
// Client.Relay writes the PEER's dropped_reason into our log line. A peer
// answering 200 chooses that string, and it reaches us AFTER every validation
// the request path performs — nothing in ValidateRelayRequest looks at a
// response. So the only thing standing between a hostile peer and our log is
// the logger, and this test states that dependency out loud rather than leaving
// it to be rediscovered: a value must not be able to terminate the record and
// forge a second one, and must not be able to choose how many bytes we write.
//
// It is a REGRESSION test for internal/logging as much as for relay: if someone
// ever "optimises" writeValue's quoting or its maxValueLen truncation away, the
// break shows up here, at the surface where the bytes are attacker-chosen.
func TestRelayClientCannotBeMadeToEmitAnInjectedOrUnboundedLogLine(t *testing.T) {
	// A forged record, repeated until it dwarfs any sane log line. It carries a
	// newline (the record terminator), an '=' and a '"' — every byte a logfmt
	// parser gives meaning to.
	const forged = "\nts=2026-01-01T00:00:00.000Z level=error msg=\"forged record\" local_bus=bus-evil"
	reason := strings.Repeat(forged, 800) // ~62 KiB, comfortably under MaxRelayBytes

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RelayResponse{
			Accepted:      false,
			DroppedReason: reason,
			// The peer chooses this too. It is not logged today; the length
			// assertion below is what would catch it if that ever changed.
			MessageID: strings.Repeat("m", 30_000),
		})
	}))
	defer srv.Close()

	var logged bytes.Buffer
	cli, err := NewClient(ClientConfig{
		BusID:       peerBus,
		LocalRoster: func() []string { return nil },
		HTTPClient:  srv.Client(),
		// Debug, because that is the level Relay logs the peer's answer at: a
		// test at Info would pass by emitting nothing at all.
		Logger: logging.New(&logged, logging.LevelDebug),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := cli.Relay(context.Background(), srv.URL, relayFixture())
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if resp.DroppedReason != reason {
		t.Errorf("Relay changed the peer's dropped_reason; it is returned verbatim and judged by the caller (deliver only treats %q as settled)", DropLoop)
	}

	out := logged.String()
	if out == "" {
		t.Fatal("Relay logged nothing at LevelDebug, so this test would prove nothing about what it logs")
	}
	if !strings.HasPrefix(out, "ts=") || !strings.Contains(out, " level=debug ") {
		t.Fatalf("the emitted record is not the one under test: %.200q", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("the peer's dropped_reason produced %d newlines, want exactly 1 (the record terminator): a peer that can end our record can forge the next one", n)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("the record does not end with its terminator: %.200q", out)
	}
	const budget = 8 << 10
	if len(out) > budget {
		t.Fatalf("a %d-byte peer-chosen dropped_reason produced a %d-byte log line (budget %d); the peer must not choose how much we write", len(reason), len(out), budget)
	}
	for i := 0; i < len(out)-1; i++ { // -1: the trailing terminator
		if c := out[i]; c < 0x20 || c >= 0x7f {
			t.Fatalf("byte %d of the record is 0x%02x; an emitted record must be a single line of printable ASCII, or a log shipper can be fed a second record", i, c)
		}
	}
}

// TestRelayFingerprintExcludesBusPath is the test that proves the single most
// consequential decision in RELAY-2.
//
// In a cyclic or meshed topology one message reaches a bus by more than one
// route, and each copy carries a DIFFERENT bus_path. If the path were covered
// by the fingerprint, the second copy would be the same key with a different
// fingerprint — idem.OutcomeViolation — and invariant 10 requires a violation
// to be rejected AND THE PEER DISCONNECTED. Correct peers would therefore
// disconnect each other as the steady state of a correct mesh.
func TestRelayFingerprintExcludesBusPath(t *testing.T) {
	viaB := relayFixture(func(r *RelayRequest) { r.BusPath = []string{peerBus, "bus-b"} })
	viaC := relayFixture(func(r *RelayRequest) { r.BusPath = []string{peerBus, "bus-c", "bus-d"} })

	one, err := ValidateRelayRequest(localBus, viaB.MessageID, viaB, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest(viaB): %v", err)
	}
	two, err := ValidateRelayRequest(localBus, viaC.MessageID, viaC, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest(viaC): %v", err)
	}
	if one.Fingerprint != two.Fingerprint {
		t.Fatal("two copies of ONE message arriving by different routes have different fingerprints; in a cyclic topology every legitimate duplicate would be an idem.OutcomeViolation, and invariant 10 would have correct peers disconnect each other as the normal steady state")
	}
	// The scopes must agree too, or the second copy would never be looked up
	// against the first.
	sc1, err := one.Scope()
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	sc2, err := two.Scope()
	if err != nil {
		t.Fatalf("Scope: %v", err)
	}
	if sc1 != sc2 {
		t.Fatal("two copies of one message resolve to different idem.Scopes")
	}

	// Everything that IS identity-defining must change the fingerprint.
	base := relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
		[]string{localBus + ".alpha-1"}, 100, len(testBody), store.ContentHash(testBody))
	for name, other := range map[string]idem.Fingerprint{
		"different body": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".alpha-1"}, 100, len(testBody), store.ContentHash([]byte("other"))),
		"different size": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".alpha-1"}, 100, len(testBody)+1, store.ContentHash(testBody)),
		"different recipient": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".gamma-1"}, 100, len(testBody), store.ContentHash(testBody)),
		"extra recipient": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".alpha-1", localBus + ".gamma-1"}, 100, len(testBody), store.ContentHash(testBody)),
		// NOT "recipient order" — that case used to live here and it asserted the
		// OPPOSITE of what the fingerprint now guarantees. It passed only by
		// accident (it compared a TWO-recipient list against a ONE-recipient
		// base, so the SET differed too). Recipient ORDER is deliberately
		// invisible to the fingerprint, because signing.Canonicalize sorts a copy
		// and the fingerprint must define "same payload" exactly as the signature
		// does; see TestSign7RecipientPermutationCannotGetAnHonestPeerDisconnected.
		// What must still differ is a different recipient SET:
		"extra recipient, listed first": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".gamma-1", localBus + ".alpha-1"}, 100, len(testBody), store.ContentHash(testBody)),
		"different sender": relayFingerprint(peerBus, peerBus+"-1", peerBus+".gamma-1", false,
			[]string{localBus + ".alpha-1"}, 100, len(testBody), store.ContentHash(testBody)),
		"different timestamp": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".alpha-1"}, 101, len(testBody), store.ContentHash(testBody)),
		"different origin bus": relayFingerprint(thirdBus, peerBus+"-1", peerBus+".beta-1", false,
			[]string{localBus + ".alpha-1"}, 100, len(testBody), store.ContentHash(testBody)),
		"different message id": relayFingerprint(peerBus, peerBus+"-2", peerBus+".beta-1", false,
			[]string{localBus + ".alpha-1"}, 100, len(testBody), store.ContentHash(testBody)),
		"broadcast instead of directed": relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", true,
			nil, 100, len(testBody), store.ContentHash(testBody)),
	} {
		if base == other {
			t.Errorf("%s produced the SAME fingerprint; a changed retry would be accepted as identical", name)
		}
	}

	// Domain separation: a relay fingerprint can never collide with another
	// operation's over the same bytes.
	if base == idem.ComputeFingerprint([]byte(idem.OpSend), []byte(peerBus), []byte(peerBus+"-1")) {
		t.Error("the relay fingerprint is not domain-separated from a send's")
	}
}

// TestRelayIdempotencyKeyIsTheOriginMessageID pins the protocol rule that makes
// cross-route dedupe possible at all, and asserts the shape relationship it
// depends on so a future widening of a bus id cannot break it silently.
func TestRelayIdempotencyKeyIsTheOriginMessageID(t *testing.T) {
	t.Run("a mismatched key is refused", func(t *testing.T) {
		req := relayFixture()
		if _, err := ValidateRelayRequest(localBus, "a-different-key", req, fakeCrossBusTrustForTest); !errors.Is(err, ErrRelayKeyMismatch) {
			t.Fatalf("error = %v, want one wrapping ErrRelayKeyMismatch", err)
		}
		if _, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest); err != nil {
			t.Fatalf("the matching key was refused: %v", err)
		}
	})

	t.Run("the client sends the origin message id as the key", func(t *testing.T) {
		var gotKey string
		var mu sync.Mutex
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotKey = r.Header.Get(idem.HeaderName)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RelayResponse{Accepted: true, MessageID: localBus + "-1"})
		}))
		defer srv.Close()

		local := newInitiator(t, peerBus, nil, srv)
		req := relayFixture()
		if _, err := local.Relay(context.Background(), srv.URL, req); err != nil {
			t.Fatalf("Relay: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if gotKey != req.MessageID {
			t.Fatalf("the %s header carried %q, want the origin message id %q", idem.HeaderName, gotKey, req.MessageID)
		}
	})

	// The shapes have to fit, and the fit is EXECUTED rather than asserted in a
	// comment: a message id must always be a legal idempotency key.
	t.Run("every message id is a legal idempotency key", func(t *testing.T) {
		if ids.MaxMessageIDLen > idem.MaxKeyLen {
			t.Fatalf("ids.MaxMessageIDLen (%d) exceeds idem.MaxKeyLen (%d); the relay key rule cannot hold", ids.MaxMessageIDLen, idem.MaxKeyLen)
		}
		if err := relayKeyFitsIdemKey(); err != nil {
			t.Fatalf("a message id byte is not a legal idempotency key byte: %v", err)
		}
		// A maximal, real message id must pass idem.ValidateKey.
		maximal := strings.Repeat("b", 64) + "-18446744073709551615"
		if len(maximal) != ids.MaxMessageIDLen {
			t.Fatalf("the test built a %d-byte id, want %d", len(maximal), ids.MaxMessageIDLen)
		}
		if _, _, err := ids.ParseMessageID(maximal); err != nil {
			t.Fatalf("the test built an invalid message id: %v", err)
		}
		if err := idem.ValidateKey(maximal); err != nil {
			t.Fatalf("a maximal message id is not a legal idempotency key: %v", err)
		}
	})
}

// TestMaxRelayBytesFitsAMaximumMessage pins the derivation in message.go: the
// byte cap must never be the thing that rejects a message the field caps allow,
// or the two limits would contradict each other and the failure would look like
// a peer bug. Mirrors TestMaxHandshakeBytesFitsAMaximumRoster.
func TestMaxRelayBytesFitsAMaximumMessage(t *testing.T) {
	body := make([]byte, store.MaxBodyBytes)
	for i := range body {
		body[i] = byte(i % 251)
	}
	longName := strings.Repeat("a", 63)
	recipients := make([]string, store.MaxRecipients)
	for i := range recipients {
		recipients[i] = fmt.Sprintf("%s.b%s-%d", localBus, longName, i+1)
	}
	path := make([]string, MaxBusPath)
	for i := range path {
		path[i] = strings.Repeat("p", 58) + fmt.Sprintf("-%d", i)
	}
	for _, id := range recipients[:1] {
		if _, _, _, err := ids.ParseAgentID(id); err != nil {
			t.Fatalf("test built an invalid recipient %q: %v", id, err)
		}
	}
	if err := validateHops(path); err != nil {
		t.Fatalf("test built an invalid path: %v", err)
	}

	buf, err := json.Marshal(RelayRequest{
		OriginBus:          strings.Repeat("o", 64),
		MessageID:          strings.Repeat("o", 64) + "-18446744073709551615",
		Sender:             strings.Repeat("o", 64) + ".b" + longName + "-18446744073709551615",
		Recipients:         recipients,
		BusPath:            path,
		TimestampUnixMilli: 1 << 42,
		Signature:          make([]byte, ed25519.SignatureSize),
		Size:               len(body),
		ContentSHA256:      store.ContentHash(body),
		Body:               body,
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if len(buf) > MaxRelayBytes {
		t.Fatalf("a maximum-size relayed message encodes to %d bytes, over the %d byte cap; the caps contradict each other", len(buf), MaxRelayBytes)
	}
}

// TestRelayedMessageCopiesEverything proves nothing in a RelayedMessage aliases
// the decoded payload. A consumer that outlives the request must not be reading
// memory the sending peer's decoder still owns.
func TestRelayedMessageCopiesEverything(t *testing.T) {
	req := relayFixture()
	m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest: %v", err)
	}
	req.Recipients[0] = localBus + ".impostor-1"
	req.BusPath[0] = thirdBus
	req.Body[0] = 'X'

	if m.Recipients[0] != localBus+".alpha-1" {
		t.Error("Recipients aliases the decoded payload")
	}
	if m.BusPath[0] != peerBus {
		t.Error("BusPath aliases the decoded payload")
	}
	if m.Body[0] != testBody[0] {
		t.Error("Body aliases the decoded payload")
	}
}

// TestForwardCarriesEveryFieldVerbatim pins PROTOCOL.md §8.5: "a relay must
// forward the signed bytes verbatim". The ONLY field that may change on a hop
// is the bus path, which is the one field a signature can never cover.
func TestForwardCarriesEveryFieldVerbatim(t *testing.T) {
	req := relayFixture()
	m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest: %v", err)
	}
	out, err := m.Forward(localBus)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if len(out.BusPath) != 2 || out.BusPath[0] != peerBus || out.BusPath[1] != localBus {
		t.Fatalf("bus path = %v, want [%s %s]", out.BusPath, peerBus, localBus)
	}
	want := req
	want.BusPath = out.BusPath
	if out.OriginBus != want.OriginBus || out.MessageID != want.MessageID ||
		out.Sender != want.Sender || out.Broadcast != want.Broadcast ||
		out.TimestampUnixMilli != want.TimestampUnixMilli || out.Size != want.Size ||
		out.ContentSHA256 != want.ContentSHA256 {
		t.Fatalf("Forward changed a covered field:\n got %+v\nwant %+v", out, want)
	}
	if string(out.Body) != string(testBody) {
		t.Fatalf("Forward changed the body")
	}
	if len(out.Recipients) != 1 || out.Recipients[0] != localBus+".alpha-1" {
		t.Fatalf("Forward changed the recipients: %v", out.Recipients)
	}

	// The forwarded envelope must be acceptable to the NEXT bus.
	if _, err := ValidateRelayRequest(thirdBus, out.MessageID, out, fakeCrossBusTrustForTest); err != nil {
		t.Fatalf("the forwarded envelope was refused by the next bus: %v", err)
	}
	// And it must be refused by a bus already on the path.
	if _, err := ValidateRelayRequest(localBus, out.MessageID, out, fakeCrossBusTrustForTest); !errors.Is(err, ErrRelayLoop) {
		t.Fatalf("error = %v, want one wrapping ErrRelayLoop", err)
	}
}

// TestRelayClientRefusesRedirect is a REGRESSION test for a HIGH a previous
// relay review found on the handshake path, restated for the relay path where
// the payload is a message body rather than a roster.
//
// Go's default redirect policy would replay the POST at whatever host a 3xx
// names, over whatever scheme it names, on a connection nobody validated. The
// https check only ever sees the URL we were handed, so it cannot catch this,
// and mutual TLS would not either: the redirect target gets a fresh connection.
func TestRelayClientRefusesRedirect(t *testing.T) {
	var leaked int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&leaked, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RelayResponse{Accepted: true, MessageID: "bus-evil-1"})
	}))
	defer sink.Close()

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+PeerRelayPath, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	local := newInitiator(t, peerBus, nil, redirector)
	_, err := local.Relay(context.Background(), redirector.URL, relayFixture())
	if !errors.Is(err, ErrPeerRefused) {
		t.Fatalf("Relay error = %v, want one wrapping ErrPeerRefused: a 3xx is a refusal, never an instruction", err)
	}
	if n := atomic.LoadInt32(&leaked); n != 0 {
		t.Fatalf("the redirect target received %d relayed message(s); the message body was exfiltrated to an attacker-chosen host", n)
	}

	// The same posture on the roster surface.
	atomic.StoreInt32(&leaked, 0)
	err = local.PushRoster(context.Background(), redirector.URL, RosterUpdate{
		BusID: peerBus, Version: 1, Added: []string{peerBus + ".beta-1"},
	}, "roster-redirect-bait")
	if !errors.Is(err, ErrPeerRefused) {
		t.Fatalf("PushRoster error = %v, want one wrapping ErrPeerRefused", err)
	}
	if n := atomic.LoadInt32(&leaked); n != 0 {
		t.Fatalf("the redirect target received %d roster update(s)", n)
	}
}

// TestRelayClientRefusesAPlaintextPeer keeps the https-only rule on the new
// surfaces (invariant 11) — peerURL is shared, and this proves the sharing.
func TestRelayClientRefusesAPlaintextPeer(t *testing.T) {
	remote := newRelayResponder(t, localBus, nil)
	local := newInitiator(t, peerBus, nil, remote.srv)
	plaintext := strings.Replace(remote.srv.URL, "https://", "http://", 1)

	if _, err := local.Relay(context.Background(), plaintext, relayFixture()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("Relay over http:// gave %v, want an error naming https", err)
	}
	if err := local.PushRoster(context.Background(), plaintext, RosterUpdate{BusID: peerBus, Version: 1}, "k"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("PushRoster over http:// gave %v, want an error naming https", err)
	}
	if n := len(remote.acceptedMessages()); n != 0 {
		t.Errorf("the peer was reached %d times, want 0: nothing should have been sent", n)
	}
}
