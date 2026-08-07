package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// THE WEDGE — SIGN-6(5), and the reason fail-closed applies to the BODY and NOT
// to the CURSOR.
//
// The settled policy, which must not be re-decided here: on verification failure
// the CURSOR ADVANCES, the BODY IS DISCARDED and never handed to the calling
// agent, and the event is RECORDED with which check failed. Blocking the cursor
// on an unverifiable message would hand anyone who can inject a single bad
// message a PERMANENT denial of service against that agent — it would never read
// anything again, from anyone, for the price of one bad message. Discard the
// body, record the event, move on.
//
// # WHAT THIS FILE CAN AND CANNOT PROVE TODAY — read this before trusting it
//
// Recipient-side verification IS NOT WIRED INTO Client.Read. Read calls
// validateBatch (which checks that server-supplied strings are safe to print and
// store) and NOTHING ELSE: it never calls verifySignedMessage, so Batch.Rejected
// is never populated and an unverifiable body reaches the caller. That is a
// FINDING about the production code, not something this file papers over — see
// TestReadDoesNotYetVerifyReceivedMessages at the bottom, which pins the gap
// loudly so that wiring it up BREAKS a test rather than silently satisfying one.
//
// So the wedge policy is proved here at the seam that DOES exist —
// verifySignedMessage plus the KeyRing — driving exactly the loop the read path
// will have to run. When SIGN-5 wires it in, this file's drainVerified becomes
// redundant with production code and should be deleted in favour of asserting on
// Batch.Messages/Batch.Rejected directly.

// wedgeRecipient is the `to` every fixture below is addressed to. It is a
// constant because it is INSIDE the signed bytes: changing it changes the
// canonical encoding, so a fixture that used a different one would simply fail
// to verify for a reason that has nothing to do with the case under test.
const wedgeRecipient = "bus-x.beta-1"

// newWedgeKeypair mints a messaging keypair.
func newWedgeKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a messaging keypair: %v", err)
	}
	return pub, priv
}

// signedWireMessage builds one wire-shaped message and SIGNS it with priv,
// through the same encoder a real send uses.
//
// It goes through signSignedMessage rather than hand-assembling bytes for the
// obvious reason: a fixture built by a second encoder proves that the second
// encoder agrees with itself.
func signedWireMessage(t *testing.T, priv ed25519.PrivateKey, seq uint64, from, body string) Message {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	m := Message{
		MessageID:     fmt.Sprintf("bus-x-%d", seq),
		Seq:           seq,
		From:          from,
		To:            []string{wedgeRecipient},
		BusPath:       []string{"bus-x"},
		SentAt:        time.Now().UTC().Format(time.RFC3339Nano),
		TimestampMS:   1754130896789,
		Size:          len(body),
		ContentSHA256: hex.EncodeToString(sum[:]),
		Body:          []byte(body),
	}
	raw, err := signSignedMessage(priv, m.signingMessage())
	if err != nil {
		t.Fatalf("signing the fixture for seq %d: %v", seq, err)
	}
	m.Signature = base64.StdEncoding.EncodeToString(raw)
	return m
}

// drainVerified is the read-side policy, written once: verify each message in
// order, hand on the ones that verify, DISCARD the bodies of the ones that do
// not, record why, and ADVANCE PAST BOTH.
//
// The key is resolved from m.From through the ring and from NOTHING ELSE. That
// is the single most abusable property of a detached-signature API: a verifier
// that took the key from beside the signature, or from the bus, would be
// self-signed and worth nothing while every test still passed.
//
// cursor is the sequence the caller would resume after — the thing a wedge would
// freeze.
func drainVerified(ring KeyRing, batch []Message) (delivered []Message, rejected []RejectedMessage, cursor uint64) {
	for _, m := range batch {
		// THE CURSOR MOVES FIRST, unconditionally. Writing it here rather than
		// on each success path is the whole policy in one line: there is no
		// branch below that can leave the cursor behind.
		cursor = m.Seq

		reject := func(reason RejectionReason, detail string) {
			rejected = append(rejected, RejectedMessage{
				MessageID: m.MessageID,
				Seq:       m.Seq,
				// UNPROVEN by definition — the message did not verify, so this
				// is the id the key was looked up under, a lead and never an
				// attribution.
				From:   m.From,
				Reason: reason,
				Detail: detail,
			})
		}

		var pub ed25519.PublicKey
		if ring != nil {
			var err error
			pub, err = ring.MessagingKey(m.From)
			if err != nil {
				reject(RejectedNoTrustedKey, err.Error())
				continue
			}
		} else {
			// A nil ring means "this agent trusts nobody". Fail closed; do not
			// invent a permissive default.
			reject(RejectedNoTrustedKey, "no trust store is configured")
			continue
		}

		sig, err := base64.StdEncoding.Strict().DecodeString(m.Signature)
		if err != nil {
			reject(RejectedSignatureEncoded, "the signature is not valid standard base64")
			continue
		}
		reason, verr := verifySignedMessage(pub, m.signingMessage(), sig)
		if reason != "" {
			reject(reason, verr.Error())
			continue
		}
		delivered = append(delivered, m)
	}
	return delivered, rejected, cursor
}

// TestVerificationSeamCannotBeWedged is the wedge proof.
//
// Each case puts ONE unverifiable message in front of a good one and insists the
// reader still reaches the good one. That ordering is the point: a fail-closed
// implementation that stopped at the first failure would pass every "bad message
// is rejected" test ever written and still be permanently DoS-able.
func TestVerificationSeamCannotBeWedged(t *testing.T) {
	goodPub, goodPriv := newWedgeKeypair(t)
	strangerPub, strangerPriv := newWedgeKeypair(t)

	const (
		trusted   = "bus-x.alpha-1"
		untrusted = "bus-x.stranger-1"
		poison    = "POISON: this body must never reach the caller"
		wanted    = "the good one, which must still arrive"
	)

	for _, tc := range []struct {
		name string
		// bad is built fresh per case so a mutation cannot leak between them.
		bad func(t *testing.T) Message
		// trustStranger records the stranger's key too, for the cases where the
		// failure must NOT be "no trusted key".
		trustStranger bool
		wantReason    RejectionReason
	}{
		{
			// The ORDINARY case today, and it is not an attack: no messaging key
			// is registered at enrolment and CRYPTO-4 does not exist, so on a bus
			// where nobody has exchanged keys EVERY message lands here. If this
			// wedged the cursor, the bus would be unusable out of the box.
			name:       "no trusted key for the sender",
			bad:        func(t *testing.T) Message { return signedWireMessage(t, strangerPriv, 1, untrusted, poison) },
			wantReason: RejectedNoTrustedKey,
		},
		{
			name:          "the signature was made by a different key",
			bad:           func(t *testing.T) Message { return signedWireMessage(t, strangerPriv, 1, trusted, poison) },
			trustStranger: true,
			wantReason:    RejectedSignatureInvalid,
		},
		{
			name: "the signature is corrupted in flight",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				raw, err := base64.StdEncoding.DecodeString(m.Signature)
				if err != nil {
					t.Fatalf("decoding the fixture signature: %v", err)
				}
				raw[0] ^= 0xFF
				m.Signature = base64.StdEncoding.EncodeToString(raw)
				return m
			},
			wantReason: RejectedSignatureInvalid,
		},
		{
			name: "the body was tampered with after signing",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, "innocuous")
				m.Body = []byte(poison)
				return m
			},
			wantReason: RejectedSignatureInvalid,
		},
		{
			// The exact case SIGN-6 exists to close. "Unsigned" must never read
			// as "unsigned but fine", and it must not wedge either.
			name: "the signature was stripped",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				m.Signature = ""
				return m
			},
			wantReason: RejectedNoSignature,
		},
		{
			name: "the signature is 63 bytes",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				raw, err := base64.StdEncoding.DecodeString(m.Signature)
				if err != nil {
					t.Fatalf("decoding the fixture signature: %v", err)
				}
				m.Signature = base64.StdEncoding.EncodeToString(raw[:SignatureSize-1])
				return m
			},
			wantReason: RejectedSignatureLength,
		},
		{
			name: "the signature is 65 bytes",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				raw, err := base64.StdEncoding.DecodeString(m.Signature)
				if err != nil {
					t.Fatalf("decoding the fixture signature: %v", err)
				}
				m.Signature = base64.StdEncoding.EncodeToString(append(raw, 0x00))
				return m
			},
			wantReason: RejectedSignatureLength,
		},
		{
			name: "the signature is not base64 at all",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				m.Signature = "not base64 !!!"
				return m
			},
			wantReason: RejectedSignatureEncoded,
		},
		{
			// A BROADCAST. It has an empty recipient set, so it has no canonical
			// audience under signing format v1 and cannot be canonicalized —
			// which means it can never be verified, which means it must never be
			// delivered, and it must not wedge the cursor either. SIGN-3 settles
			// what a broadcast's audience is.
			name: "a broadcast, which has no canonical audience under format v1",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				m.Broadcast = true
				m.To = nil
				return m
			},
			wantReason: RejectedNotCanonical,
		},
		{
			name: "the seq disagrees with the sequence inside the message id",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				m.Seq = 99
				return m
			},
			wantReason: RejectedNotCanonical,
		},
		{
			name: "the sender belongs to a different bus than minted the id",
			bad: func(t *testing.T) Message {
				m := signedWireMessage(t, goodPriv, 1, trusted, poison)
				m.From = "bus-elsewhere.alpha-1"
				return m
			},
			// The key is looked up under the CLAIMED sender, and no key is held
			// for a stranger on another bus — so this fails at the trust-store
			// step, before the origin binding is ever reached. That ordering is
			// itself worth pinning: the lookup must use the id in the message and
			// nothing else.
			wantReason: RejectedNoTrustedKey,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ring := NewDirKeyRing(t.TempDir())
			if err := ring.Trust(trusted, goodPub); err != nil {
				t.Fatalf("seeding the trust store: %v", err)
			}
			if tc.trustStranger {
				if err := ring.Trust(untrusted, strangerPub); err != nil {
					t.Fatalf("seeding the trust store: %v", err)
				}
			}

			bad := tc.bad(t)
			good := signedWireMessage(t, goodPriv, 2, trusted, wanted)

			delivered, rejected, cursor := drainVerified(ring, []Message{bad, good})

			// (1) PROGRESS. The cursor is past BOTH messages. A reader that
			// stopped at the bad one would report cursor 1 and never advance
			// again — a permanent DoS for the price of one injected message.
			if cursor != good.Seq {
				t.Fatalf("cursor = %d, want %d: the reader WEDGED on an unverifiable message and will never make progress again", cursor, good.Seq)
			}

			// (2) THE GOOD MESSAGE ARRIVED.
			if len(delivered) != 1 {
				t.Fatalf("delivered %d messages, want exactly the good one; %+v", len(delivered), delivered)
			}
			if string(delivered[0].Body) != wanted {
				t.Fatalf("delivered body = %q, want %q", delivered[0].Body, wanted)
			}
			if delivered[0].Seq != good.Seq {
				t.Fatalf("delivered seq %d, want %d", delivered[0].Seq, good.Seq)
			}

			// (3) THE BAD BODY WAS NEVER HANDED OVER. Not flagged, not
			// accompanied by a warning — ABSENT. A body that reaches an agent has
			// been acted on whatever the warning beside it said.
			for _, m := range delivered {
				if strings.Contains(string(m.Body), poison) {
					t.Fatalf("the unverifiable body reached the caller: %q", m.Body)
				}
			}

			// (4) THE EVENT WAS RECORDED, naming WHICH check failed — and
			// carrying no body.
			if len(rejected) != 1 {
				t.Fatalf("recorded %d rejections, want exactly 1: a discard that is not recorded is a silent discard", len(rejected))
			}
			r := rejected[0]
			if r.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q (detail: %s)", r.Reason, tc.wantReason, r.Detail)
			}
			if r.MessageID != bad.MessageID || r.Seq != bad.Seq {
				t.Errorf("the rejection names %q/%d, want the offending %q/%d", r.MessageID, r.Seq, bad.MessageID, bad.Seq)
			}
			if r.Detail == "" {
				t.Error("the rejection carries no detail; an operator cannot investigate a bare reason code")
			}
			if strings.Contains(r.Detail, poison) {
				t.Errorf("the rejection's detail leaks the discarded body: %q", r.Detail)
			}
		})
	}
}

// TestVerificationSeamSurvivesAnAllBadBatch is the degenerate case of the same
// property: EVERY message unverifiable.
//
// This is not hypothetical, it is the state of the world today — no messaging
// key is registered at enrolment, so before any out-of-band exchange every
// sender is unverifiable. The cursor must still reach the end of the batch, or a
// fresh agent's first poll wedges it for ever and it never sees the message that
// arrives after somebody finally runs `agent-busctl trust`.
func TestVerificationSeamSurvivesAnAllBadBatch(t *testing.T) {
	_, strangerPriv := newWedgeKeypair(t)
	ring := NewDirKeyRing(t.TempDir()) // trusts NOBODY

	batch := []Message{
		signedWireMessage(t, strangerPriv, 1, "bus-x.stranger-1", "one"),
		signedWireMessage(t, strangerPriv, 2, "bus-x.stranger-1", "two"),
		signedWireMessage(t, strangerPriv, 3, "bus-x.stranger-1", "three"),
	}
	delivered, rejected, cursor := drainVerified(ring, batch)

	if cursor != 3 {
		t.Fatalf("cursor = %d, want 3: an agent that trusts nobody must still make progress, or its very first poll wedges it permanently", cursor)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered %d messages from a trust store holding no keys; every one of them is unverifiable", len(delivered))
	}
	if len(rejected) != 3 {
		t.Fatalf("recorded %d rejections, want 3: every discard is recorded", len(rejected))
	}
	for _, r := range rejected {
		if r.Reason != RejectedNoTrustedKey {
			t.Errorf("reason = %q for %s, want %q", r.Reason, r.MessageID, RejectedNoTrustedKey)
		}
	}

	t.Run("a nil KeyRing trusts nobody and still does not wedge", func(t *testing.T) {
		delivered, rejected, cursor := drainVerified(nil, batch)
		if cursor != 3 {
			t.Fatalf("cursor = %d, want 3", cursor)
		}
		if len(delivered) != 0 || len(rejected) != 3 {
			t.Fatalf("delivered %d / rejected %d, want 0 / 3: a nil ring means \"trusts nobody\", never \"trusts everybody\"", len(delivered), len(rejected))
		}
	})
}

// TestVerifySignedMessageDoesNotPanicOnMalformedPublicKey is SIGN-2's named
// acceptance criterion, on the CLIENT's mirrored verifier.
//
// crypto/ed25519.Verify PANICS when len(publicKey) != 32. It does not return
// false. That asymmetry is a remote denial of service the moment a key reaches
// this path from anywhere an attacker or a damaged disk influences: a MALFORMED
// SIGNATURE is handled safely and returns false, while a MALFORMED KEY takes the
// process down. internal/signing.Verify pins this on the server side; the
// mirrored client-side verifier needs its own, because the mirror is where a
// re-ordering would go unnoticed.
//
// The order inside verifySignedMessage is load-bearing and this test is what
// keeps it: KEY LENGTH FIRST, before the signature length, before
// canonicalization, before ed25519.Verify.
func TestVerifySignedMessageDoesNotPanicOnMalformedPublicKey(t *testing.T) {
	_, priv := newWedgeKeypair(t)
	m := signedWireMessage(t, priv, 1, "bus-x.alpha-1", "hello")
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		t.Fatalf("decoding the fixture signature: %v", err)
	}

	for _, tc := range []struct {
		name string
		pub  ed25519.PublicKey
	}{
		{name: "nil", pub: nil},
		{name: "empty", pub: ed25519.PublicKey{}},
		{name: "one byte short", pub: make([]byte, MessagingPublicKeySize-1)},
		{name: "one byte long", pub: make([]byte, MessagingPublicKeySize+1)},
		{name: "a single byte", pub: ed25519.PublicKey{0x01}},
		// The shape an operator produces by pasting a PRIVATE key, or by
		// pasting a signature, into `agent-busctl trust`.
		{name: "a 64-byte value", pub: make([]byte, 64)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("verifySignedMessage PANICKED on a %d-byte public key: %v\n"+
						"ed25519.Verify panics rather than returning false on a wrong-size key, so this is a remote DoS: "+
						"the key must be length-checked BEFORE it reaches crypto/ed25519", len(tc.pub), r)
				}
			}()
			reason, err := verifySignedMessage(tc.pub, m.signingMessage(), sig)
			if reason != RejectedMalformedKey {
				t.Fatalf("reason = %q, want %q: a damaged trust store is an OPERATOR fault and must not be reported as an attack", reason, RejectedMalformedKey)
			}
			if err == nil {
				t.Fatal("a malformed key was rejected with a nil error; a nil error is the ONLY outcome on which a caller may deliver the body")
			}
		})
	}

	// The control: with the RIGHT key the same fixture verifies. Without it the
	// table above would pass against a verifier that rejected everything.
	t.Run("control: the correct key verifies", func(t *testing.T) {
		pub := priv.Public().(ed25519.PublicKey)
		reason, err := verifySignedMessage(pub, m.signingMessage(), sig)
		if reason != "" || err != nil {
			t.Fatalf("the fixture does not verify under its own key: reason %q, err %v", reason, err)
		}
	})
}

// TestReadDoesNotYetVerifyReceivedMessages PINS A KNOWN GAP. It is NOT a
// contract.
//
// Client.Read does not call verifySignedMessage. It calls validateBatch, which
// only checks that server-supplied strings are safe to print and store, and then
// hands every message straight to the caller. Consequently:
//
//   - Batch.Rejected is NEVER populated, on any input;
//   - a message with NO signature at all is delivered, body and all;
//   - the trust store is not consulted, so a KeyRing holding no keys changes
//     nothing.
//
// This test exists so that WIRING VERIFICATION IN BREAKS A TEST rather than
// silently satisfying one. When SIGN-5 lands, this test must be INVERTED — the
// unsigned message below must appear in Batch.Rejected with the body withheld,
// and TestVerificationSeamCannotBeWedged's drainVerified helper should be
// deleted in favour of asserting on Read's own output.
//
// Do NOT "fix" this test by deleting it. Deleting it removes the only record
// that the read path is unverified.
func TestReadDoesNotYetVerifyReceivedMessages(t *testing.T) {
	unsigned := stubMessage(1, "bus-x.stranger-1", "nobody signed this")
	if unsigned.Signature != "" || unsigned.TimestampMS != 0 {
		t.Fatalf("the fixture is not unsigned: signature %q, timestamp %d", unsigned.Signature, unsigned.TimestampMS)
	}

	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		stubWriteJSON(w, http.StatusOK, Batch{Messages: []Message{unsigned}, Cursor: "cursor-1"})
	})
	// An EMPTY trust store: this client holds nobody's key, so under SIGN-5's
	// policy every message here is unverifiable and every body is discarded.
	c := bus.client(t, func(cfg *Config) { cfg.KeyRing = NewDirKeyRing(t.TempDir()) })

	batch, err := c.Read(context.Background(), ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(batch.Rejected) != 0 {
		t.Fatalf("KNOWN GAP CLOSED: Read now populates Batch.Rejected (%+v).\n"+
			"That is the desired behaviour — INVERT this test: assert the unsigned message is REJECTED with reason %q and that its body is NOT in Batch.Messages.",
			batch.Rejected, RejectedNoSignature)
	}
	if len(batch.Messages) != 1 {
		t.Fatalf("Read returned %d messages, want 1", len(batch.Messages))
	}
	if string(batch.Messages[0].Body) != "nobody signed this" {
		t.Fatalf("body = %q, want the unsigned fixture's", batch.Messages[0].Body)
	}
	t.Log("KNOWN GAP (SIGN-5, not yet implemented): Client.Read delivered an UNSIGNED message from a sender this client holds no key for. " +
		"The verification seam (verifySignedMessage) exists and is proved by TestVerificationSeamCannotBeWedged, but nothing on the read path calls it.")
}
