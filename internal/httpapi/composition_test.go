package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/client"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// THE COMPOSITION TEST — the one this wave was missing.
//
// # Why it exists, stated as bluntly as it deserves
//
// The defect that shipped in SIGN-2/SIGN-6 was in NEITHER HALF. internal/httpapi
// correctly demanded a signature; client/ correctly knew how to make one. Every
// unit test on both sides stayed green and `busctl send` failed with "a
// signature is required", because nothing anywhere exercised the two TOGETHER.
//
// A test that stubs either side cannot catch that class of bug — by
// construction, a stub agrees with the test author rather than with the other
// half of the system. So this file drives the REAL client package against the
// REAL httpapi server, over a REAL HTTP transport, with a REAL hub and a REAL
// durable log underneath. Nothing here is a double.
//
// # Why it lives in internal/httpapi's test package and not in client/
//
// client/ must not import internal/ (client/doc.go, invariant 7: an agent
// EMBEDDING this package is a required audience, and Go forbids another module
// from importing an internal/ path). This package's EXTERNAL test package is
// under no such restriction and can import both, which makes it the only place
// the two definitions of the canonical format can be compared against each
// other.
//
// # The load-bearing assertion is the third one
//
// It is not "the send returned 201" and it is not "a signature came back". It is
// that the signature the CLIENT produced, over the CLIENT's pinned canonical
// encoder (client/canonical.go), VERIFIES under internal/signing.Verify against
// bytes rebuilt from the WIRE FIELDS. That is the anti-drift check for the
// duplicated encoder: client/canonical.go is a hand-maintained mirror of
// internal/signing/canonical.go, and the two are only ever compared HERE.
//
// Two details of the reconstruction are exactly where a careless reader goes
// wrong, and both are asserted separately below:
//
//   - the sender is `from`, NOT the recipient;
//   - the timestamp is `timestamp_ms`, NOT `sent_at`. sent_at is the BUS's clock
//     and is deliberately not covered by the signature.

// composedBus is a real server on a real listener, plus the machinery to build
// real clients against it.
type composedBus struct {
	url string
}

// newComposedBus stands up newMessagingServer behind an httptest listener.
//
// The server is the same one every other test in this package uses — a real
// durable log under t.TempDir(), a real auth service, a real hub reading through
// to the shared roster — so a difference between this test and the handler tests
// beside it can only come from the client, which is the point.
func newComposedBus(t *testing.T) composedBus {
	t.Helper()
	srv, _ := newMessagingServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return composedBus{url: ts.URL}
}

// enrolClient builds a Client with its own credential store and enrols it,
// exactly as `busctl enrol` does.
func (b composedBus) enrolClient(t *testing.T, name string) (*client.Client, string) {
	t.Helper()
	c, err := client.New(client.Config{
		BusURL: b.url,
		// t.TempDir(), never the tracked data/ dir and never the developer's
		// real ~/.config/agent-bus: a test that wrote there would enrol against
		// a throwaway bus and leave the credential behind.
		IdentityDir: t.TempDir(),
		Timeout:     10 * time.Second,
	})
	if err != nil {
		t.Fatalf("building a client for %q: %v", name, err)
	}
	res, err := c.Enrol(context.Background(), client.EnrolOptions{
		Name:        name,
		Save:        true,
		MakeCurrent: true,
	})
	if err != nil {
		t.Fatalf("enrolling %q against the real bus: %v", name, err)
	}
	if res.AgentID == "" {
		t.Fatalf("enrolling %q returned no agent id", name)
	}
	return c, res.AgentID
}

// TestClientSendVerifiesEndToEnd is the composition test.
func TestClientSendVerifiesEndToEnd(t *testing.T) {
	bus := newComposedBus(t)
	sender, senderID := bus.enrolClient(t, "alpha")
	recipient, recipientID := bus.enrolClient(t, "beta")

	ctx := context.Background()
	payload := []byte("composition: the halves have to agree")

	// (1) THE SEND SUCCEEDS. This is the assertion that was red when the wave
	// shipped: the client drove /v1/mint, signed the reservation and presented
	// it to /v1/send, and the bus refused with "a signature is required".
	res, err := sender.Send(ctx, client.SendOptions{To: recipientID, Body: payload})
	if err != nil {
		t.Fatalf("client.Send against a REAL bus failed: %v\n"+
			"this is the exact failure the wave shipped: each half was individually green and their COMPOSITION was broken", err)
	}
	if res.MessageID == "" || res.Seq == 0 {
		t.Fatalf("the send returned message id %q and seq %d; the bus is authoritative on both", res.MessageID, res.Seq)
	}
	if res.From != senderID {
		t.Errorf("from = %q, want the AUTHENTICATED sender %q; a client never chooses it", res.From, senderID)
	}
	if res.Replayed {
		t.Error("the first send reports replayed; nothing had been applied under this key")
	}

	// (2) THE MESSAGE COMES BACK CARRYING WHAT A RECIPIENT NEEDS. Without both
	// of these a recipient cannot reconstruct the signed bytes at all, and the
	// signature on the wire is decoration.
	batch, err := recipient.Read(ctx, client.ReadOptions{})
	if err != nil {
		t.Fatalf("client.Read: %v", err)
	}
	if len(batch.Messages) != 1 {
		t.Fatalf("the recipient sees %d messages, want exactly 1", len(batch.Messages))
	}
	m := batch.Messages[0]
	if m.MessageID != res.MessageID || m.Seq != res.Seq {
		t.Fatalf("read back %q/%d, want the sent %q/%d", m.MessageID, m.Seq, res.MessageID, res.Seq)
	}
	if m.From != senderID {
		t.Fatalf("from = %q, want %q", m.From, senderID)
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(m.Signature)
	if err != nil {
		t.Fatalf("the signature on the read path is not strict standard base64: %v", err)
	}
	if len(sig) != signing.SignatureSize {
		t.Fatalf("the signature is %d bytes, want exactly %d", len(sig), signing.SignatureSize)
	}
	if m.TimestampMS <= 0 {
		t.Fatalf("timestamp_ms = %d; it is the SENDER's clock, it is covered by the signature, and a recipient cannot verify without it", m.TimestampMS)
	}
	if m.SentAt == "" {
		t.Error("sent_at is empty; it is the BUS's clock and is a separate fact from timestamp_ms")
	}
	if string(m.Body) != string(payload) {
		t.Fatalf("body = %q, want %q", m.Body, payload)
	}

	// The sender's messaging public key, which is what a recipient must obtain
	// OUT OF BAND today: nothing registers a messaging key at enrolment and
	// CRYPTO-4 does not exist, so there is no route that publishes it. This test
	// takes it from the sending client directly — which is exactly the out-of-band
	// exchange, performed in-process.
	_, pubB64, err := sender.MessagingPublicKey()
	if err != nil {
		t.Fatalf("reading the sender's messaging public key: %v", err)
	}
	pubRaw, err := base64.StdEncoding.Strict().DecodeString(pubB64)
	if err != nil {
		t.Fatalf("the client's messaging public key is not strict standard base64: %v", err)
	}
	if len(pubRaw) != signing.PublicKeySize {
		t.Fatalf("the messaging public key is %d bytes, want %d", len(pubRaw), signing.PublicKeySize)
	}
	pub := ed25519.PublicKey(pubRaw)

	// (3) THE LOAD-BEARING ASSERTION. Rebuilt from the WIRE FIELDS —
	// message_id, seq, from, to, timestamp_ms, body — and verified by the
	// SERVER-SIDE encoder against a signature produced by the CLIENT-SIDE one.
	//
	// bus_path is deliberately absent: it is rewritten by every relay, so
	// signing it would make every relayed message unverifiable (settled in
	// SIGN-1).
	fromWire := signing.Message{
		MessageID:          m.MessageID,
		Sequence:           m.Seq,
		Sender:             m.From, // `from`, NOT the recipient.
		Recipients:         m.To,
		TimestampUnixMilli: m.TimestampMS, // `timestamp_ms`, NOT `sent_at`.
		Body:               m.Body,
	}
	if err := signing.Verify(pub, fromWire, sig); err != nil {
		t.Fatalf("the signature the CLIENT made does not verify under internal/signing: %v\n"+
			"client/canonical.go has drifted from internal/signing/canonical.go — the two produce different bytes for one message, "+
			"and every message this client sends is unverifiable by every recipient", err)
	}

	// The negative controls. Without them the assertion above could be passing
	// for a reason that has nothing to do with the canonical format agreeing —
	// a Verify that returned nil unconditionally would satisfy it.
	t.Run("the same bytes do NOT verify under a different key", func(t *testing.T) {
		otherPub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("generating a decoy keypair: %v", err)
		}
		if err := signing.Verify(otherPub, fromWire, sig); err == nil {
			t.Fatal("a signature verified under a key that never signed it; the verification above proves nothing")
		}
	})

	t.Run("substituting the recipient for the sender breaks verification", func(t *testing.T) {
		// The mistake this guards against is the obvious one when rebuilding a
		// received message: `to` is right there beside `from`.
		swapped := fromWire
		swapped.Sender = recipientID
		if err := signing.Verify(pub, swapped, sig); err == nil {
			t.Fatal("the signature verified with the RECIPIENT in the sender slot; the sender is inside the signed bytes and must be")
		}
	})

	t.Run("substituting the bus's clock for the sender's breaks verification", func(t *testing.T) {
		// sent_at is the BUS's clock and is NOT covered. A recipient that
		// reconstructed from it would fail every verification for a reason no
		// client could diagnose — so it must be visibly different bytes here.
		busClock := fromWire
		parsed, err := time.Parse(time.RFC3339Nano, m.SentAt)
		if err != nil {
			t.Fatalf("sent_at %q is not RFC3339Nano: %v", m.SentAt, err)
		}
		busClock.TimestampUnixMilli = parsed.UnixNano() / int64(time.Millisecond)
		if busClock.TimestampUnixMilli == fromWire.TimestampUnixMilli {
			t.Skip("the bus's clock and the sender's clock happen to agree to the millisecond in this run; nothing to distinguish")
		}
		if err := signing.Verify(pub, busClock, sig); err == nil {
			t.Fatal("the signature verified against the BUS's clock; timestamp_ms and sent_at are two different facts and only one is signed")
		}
	})

	t.Run("a tampered body breaks verification", func(t *testing.T) {
		tampered := fromWire
		tampered.Body = append(append([]byte(nil), fromWire.Body...), '!')
		if err := signing.Verify(pub, tampered, sig); err == nil {
			t.Fatal("the signature verified over a body it does not cover")
		}
	})

	// The SAME message read through the long poll must carry the SAME signed
	// fields. Two read routes rendering one message two ways is exactly the kind
	// of drift a recipient discovers as an unexplainable verification failure.
	t.Run("the long poll renders the identical signed fields", func(t *testing.T) {
		polled, err := recipient.Read(ctx, client.ReadOptions{Wait: time.Second})
		if err != nil {
			t.Fatalf("client.Read (long poll): %v", err)
		}
		if len(polled.Messages) != 1 {
			t.Fatalf("the long poll returned %d messages from cursor 0, want 1", len(polled.Messages))
		}
		w := polled.Messages[0]
		if w.Signature != m.Signature || w.TimestampMS != m.TimestampMS || w.From != m.From || w.MessageID != m.MessageID || w.Seq != m.Seq {
			t.Fatalf("/v1/wait rendered the signed fields differently from /v1/messages:\n  wait: %+v\n  messages: %+v", w, m)
		}
		wSig, err := base64.StdEncoding.Strict().DecodeString(w.Signature)
		if err != nil {
			t.Fatalf("the long poll's signature is not strict standard base64: %v", err)
		}
		if err := signing.Verify(pub, signing.Message{
			MessageID:          w.MessageID,
			Sequence:           w.Seq,
			Sender:             w.From,
			Recipients:         w.To,
			TimestampUnixMilli: w.TimestampMS,
			Body:               w.Body,
		}, wSig); err != nil {
			t.Fatalf("the message read through /v1/wait does not verify: %v", err)
		}
	})
}

// TestClientSendRetryIsOneMessageEndToEnd proves the two-step handshake is
// retryable as a UNIT, against a real bus.
//
// This is where reserve-then-send could quietly become two messages: a client
// that re-minted under a fresh key on the retry would get a fresh sequence, sign
// a different assignment, and the bus would accept a SECOND message that is
// perfectly valid and completely wrong. The single idempotency key spanning both
// calls is what prevents it, and only an end-to-end run can show that it does.
func TestClientSendRetryIsOneMessageEndToEnd(t *testing.T) {
	bus := newComposedBus(t)
	sender, _ := bus.enrolClient(t, "alpha")
	recipient, recipientID := bus.enrolClient(t, "beta")

	ctx := context.Background()
	payload := []byte("exactly once, please")

	first, err := sender.Send(ctx, client.SendOptions{To: recipientID, Body: payload})
	if err != nil {
		t.Fatalf("the first send: %v", err)
	}
	if first.IdempotencyKey == "" {
		t.Fatal("the send reported no idempotency key; it is the only handle that can retry this exact logical send")
	}

	// The retry a client makes after a lost acknowledgement: the SAME key, the
	// SAME body, byte for byte.
	retry, err := sender.Send(ctx, client.SendOptions{
		To:             recipientID,
		Body:           payload,
		IdempotencyKey: first.IdempotencyKey,
	})
	if err != nil {
		t.Fatalf("the retry under the same key: %v; a legitimate retry must never be punished (invariant 10)", err)
	}
	if !retry.Replayed {
		t.Error("the retry does not report replayed; the bus wrote it again rather than answering from its applied-key table")
	}
	if retry.MessageID != first.MessageID || retry.Seq != first.Seq {
		t.Fatalf("the retry produced %q/%d, want the ORIGINAL %q/%d — this is a SECOND message, not a retry",
			retry.MessageID, retry.Seq, first.MessageID, first.Seq)
	}

	batch, err := recipient.Read(ctx, client.ReadOptions{})
	if err != nil {
		t.Fatalf("client.Read: %v", err)
	}
	if len(batch.Messages) != 1 {
		t.Fatalf("the recipient sees %d messages after one send and one retry, want exactly 1", len(batch.Messages))
	}
}
