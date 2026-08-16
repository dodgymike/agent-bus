// RELAY-48: the ORIGIN ATTESTATION on the durable message record.
//
// The field exists for one consumer — relay.Forwarder.Resume — which runs ONLY
// after a restart, so its whole value is that it survives one. The end-to-end
// proof of that is a real SIGKILL in cmd/agent-bus
// (TestOnwardRelayPendingJobRequeuesAfterRestart). What is proved HERE is the
// half that lives in this package and that a crash test cannot reach: the
// encoding, the write-path refusals, and — the reason the security gate asked
// for this file — the two new arms of Decode, which is the boundary for bytes
// THIS PROCESS DID NOT VALIDATE.
//
// # WHY Decode'S ARMS NEED THEIR OWN TESTS
//
// Decode reads a file that may have been written by another build, damaged by
// media, or handed over by a peer. A refusal there DISCARDS A DURABLE MESSAGE,
// so the blast radius of each refusal is a design decision rather than an
// implementation detail, and it is asserted rather than argued:
//
//   - an attestation that does not validate    -> REFUSED (that record only)
//   - an attestation with NO origin message id -> REFUSED (incoherent: this bus
//     mints none for its own traffic)
//   - an origin message id with NO attestation -> ACCEPTED, deliberately. That is
//     every record written before this field existed. Refusing it would throw
//     away a whole message to protect one hop of it; what is lost is the ONWARD
//     hop alone, and the recovery seam refuses THAT by name.
package store_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
)

// r48Attestation is a WELL-FORMED attestation for sender.
//
// It is not GENUINE — no bus signing key ever signed these bytes — and that is
// the right fidelity for this package, which validates SHAPE and
// BINDING-TO-SENDER and deliberately never verifies: verification needs the
// origin bus's peering-time pinned signing key, which lives in internal/relay's
// peer store and never comes near the durability layer.
func r48Attestation(sender string) attest.Attestation {
	return attest.Attestation{
		AgentID:            sender,
		MessagingPublicKey: bytes.Repeat([]byte{0x5A}, ed25519.PublicKeySize),
		KeyEpoch:           42,
		IssuedAtUnixMilli:  testTimestampMs,
		NotAfterUnixMilli:  testTimestampMs + 86_400_000,
		Signature:          bytes.Repeat([]byte{0xA5}, signing.SignatureSize),
	}
}

// r48Ingested is the message a RELAY INGEST produces, built through the SAME two
// calls production makes so a fixture can never carry a combination the write
// path would have refused.
func r48Ingested(t *testing.T) store.Message {
	t.Helper()
	sender := originAgentIDOn(t, originPeerBusID, "alpha")
	m, err := store.NewMessageWithBusPath(
		testBusID, sender, false, []string{agentIDFor(t, "beta")}, 5,
		time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC), []byte("carried from another bus"),
		"r48-key", testTimestampMs, testSignature(t),
		[]string{originPeerBusID, testBusID},
	)
	if err != nil {
		t.Fatalf("store.NewMessageWithBusPath: %v", err)
	}
	m, err = m.WithRelayOrigin(originPeerBusID+"-7", r48Attestation(sender))
	if err != nil {
		t.Fatalf("WithRelayOrigin: %v", err)
	}
	return m
}

// TestOriginAttestationRoundTripsThroughTheDurableRecord is the encoding half:
// what WithRelayOrigin sets reaches the JSON, and comes back byte-identical.
//
// The bytes are asserted directly, not just the round-tripped Go value: a Record
// field that never reached JSON would still round-trip through a struct, and the
// fact a restart depends on is what is IN THE FILE.
func TestOriginAttestationRoundTripsThroughTheDurableRecord(t *testing.T) {
	m := r48Ingested(t)
	if !m.HasOriginAttestation() {
		t.Fatal("WithRelayOrigin did not set an origin attestation")
	}

	raw, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"origin_attestation":`)) {
		t.Fatalf(`the durable record carries no "origin_attestation".

Its only consumer is relay.Forwarder.Resume, which runs ONLY after a restart, so
a field that is not written here is gone at the one moment it is read
(invariant 5). Record() is the only thing that carries it to disk.

%s`, raw)
	}

	got, err := store.Decode(raw)
	if err != nil {
		t.Fatalf("Decode of a record carrying an origin attestation: %v", err)
	}
	want := m.OriginAttestation
	if a := got.OriginAttestation; a.AgentID != want.AgentID ||
		a.KeyEpoch != want.KeyEpoch ||
		a.IssuedAtUnixMilli != want.IssuedAtUnixMilli ||
		a.NotAfterUnixMilli != want.NotAfterUnixMilli ||
		!bytes.Equal(a.MessagingPublicKey, want.MessagingPublicKey) ||
		!bytes.Equal(a.Signature, want.Signature) {
		t.Fatalf("after Encode/Decode the origin attestation is:\n got %+v\nwant %+v\n\nIt must survive VERBATIM: no hop may re-mint or alter it, and one that does produces a blob the next hop refuses as a forgery", a, want)
	}
	// Decoding must not hand back a value aliasing the decoded buffer.
	got.OriginAttestation.Signature[0] ^= 0xff
	if got.OriginAttestation.Signature[0] == want.Signature[0] {
		t.Error("the decoded attestation's signature aliases the record's bytes")
	}
}

// TestLocalMessageWritesNoOriginAttestation: absence is the ORDINARY case and
// costs nothing on disk.
//
// This bus mints no attestation for its own traffic, so a locally-originated
// message has none — and `omitempty` must keep the key out of the JSON entirely
// rather than writing a skeleton of nulls onto every message the bus ever sends.
func TestLocalMessageWritesNoOriginAttestation(t *testing.T) {
	m, err := store.NewMessage(testBusID, agentIDFor(t, "alpha"), false, []string{agentIDFor(t, "beta")},
		3, time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC), []byte("local"), "k", testTimestampMs, testSignature(t))
	if err != nil {
		t.Fatalf("store.NewMessage: %v", err)
	}
	if m.HasOriginAttestation() {
		t.Fatal("a locally-originated message reports an origin attestation")
	}
	raw, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if bytes.Contains(raw, []byte("origin_attestation")) {
		t.Fatalf("a locally-originated message's record mentions origin_attestation; omitempty must keep it off disk entirely:\n%s", raw)
	}
	got, err := store.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.HasOriginAttestation() {
		t.Fatal("Decode invented an origin attestation for a record that carries none")
	}
}

// TestWithRelayOriginRefusals — one case per refusal on the write path.
//
// Every refusal wraps store.ErrInvalidMessage so a caller can classify it, and
// each is checked for the REASON as well as the fact, because a refusal for the
// wrong reason is a test that would stay green through the bug it exists to
// catch.
func TestWithRelayOriginRefusals(t *testing.T) {
	sender := originAgentIDOn(t, originPeerBusID, "alpha")
	base, err := store.NewMessageWithBusPath(
		testBusID, sender, false, []string{agentIDFor(t, "beta")}, 5,
		time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC), []byte("payload"),
		"k", testTimestampMs, testSignature(t), []string{originPeerBusID, testBusID},
	)
	if err != nil {
		t.Fatalf("store.NewMessageWithBusPath: %v", err)
	}
	originID := originPeerBusID + "-7"

	shortSig := r48Attestation(sender)
	shortSig.Signature = shortSig.Signature[:signing.SignatureSize-1]

	wrongSubject := r48Attestation(originAgentIDOn(t, originPeerBusID, "mallory"))

	expired := r48Attestation(sender)
	expired.NotAfterUnixMilli = expired.IssuedAtUnixMilli - 1

	for _, c := range []struct {
		name   string
		origin string
		att    attest.Attestation
		wantIn string
	}{
		{
			// The whole point of the single setter: the half-set state is
			// unrepresentable through it.
			name:   "a ZERO attestation",
			origin: originID,
			att:    attest.Attestation{},
			wantIn: "cannot be canonicalized",
		},
		{
			name:   "a signature of the wrong length",
			origin: originID,
			att:    shortSig,
			wantIn: "origin attestation signature",
		},
		{
			// The next hop verifies the attestation AGAINST the sender, so a
			// mismatched pair could never be delivered anywhere.
			name:   "an attestation for a DIFFERENT agent",
			origin: originID,
			att:    wrongSubject,
			wantIn: "the message's sender is",
		},
		{
			name:   "an attestation that expires before it is issued",
			origin: originID,
			att:    expired,
			wantIn: "not after issued-at",
		},
		{
			// The id half's refusals are WithOriginMessageID's, reused rather
			// than restated. One case is enough to prove they are still applied.
			name:   "an origin id naming THIS bus",
			origin: testBusID + "-9",
			att:    r48Attestation(sender),
			wantIn: "names THIS bus",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := base.WithRelayOrigin(c.origin, c.att)
			if err == nil {
				t.Fatalf("WithRelayOrigin SUCCEEDED and returned %+v, want a refusal wrapping store.ErrInvalidMessage", got)
			}
			if !errors.Is(err, store.ErrInvalidMessage) {
				t.Fatalf("WithRelayOrigin = %v, which is not errors.Is(store.ErrInvalidMessage)", err)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("refused, but for the wrong reason: %v\nwant an error mentioning %q", err, c.wantIn)
			}
			if got.ID != "" || got.HasOriginAttestation() {
				t.Fatalf("a refused WithRelayOrigin returned a non-zero Message %+v", got)
			}
			if base.HasOriginAttestation() {
				t.Fatal("WithRelayOrigin mutated its RECEIVER; it returns a COPY")
			}
		})
	}
}

// TestWithRelayOriginDoesNotAliasTheCallersAttestation: the durable message must
// not share bytes with the transient envelope it came from.
//
// This is the same time-of-check/time-of-use fence internal/relay's own
// cloneAttestation puts up on the wire side, applied at the boundary where the
// value stops being transient and starts being durable.
func TestWithRelayOriginDoesNotAliasTheCallersAttestation(t *testing.T) {
	sender := originAgentIDOn(t, originPeerBusID, "alpha")
	att := r48Attestation(sender)
	m := r48Ingested(t)
	if !bytes.Equal(m.OriginAttestation.Signature, att.Signature) {
		t.Fatalf("fixture mismatch: %x vs %x", m.OriginAttestation.Signature, att.Signature)
	}

	stored := append([]byte(nil), m.OriginAttestation.Signature...)
	att.Signature[0] ^= 0xff
	att.MessagingPublicKey[0] ^= 0xff
	if !bytes.Equal(m.OriginAttestation.Signature, stored) {
		t.Error("mutating the caller's attestation changed the stored one; the durable value aliases the caller's slices")
	}
}

// TestDecodeRefusesAnUnusableOriginAttestation is the recovery-path half, and it
// is the one the security gate asked for.
//
// Decode applies EXACTLY the rule WithRelayOrigin applies — one function,
// validateOriginAttestation, called from both — so a restart can never load
// state the write path would have refused to create. Each case here is a record
// this build would never write, arriving off media it did not control.
//
// WHAT IS ASSERTED ABOUT THE BLAST RADIUS: the refusal is per-RECORD. Decode
// returns an error for that record and nothing else; hub.Apply logs it loudly
// and specifically and carries on, so recovery still reaches a running server
// (invariant 6). Nothing here can stop a boot.
func TestDecodeRefusesAnUnusableOriginAttestation(t *testing.T) {
	good := r48Ingested(t)
	raw, err := good.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Re-decoding into a generic map is how a record "written by another build"
	// is forged here: it edits the JSON rather than the Go value, which is the
	// only way to produce a shape this build's own types cannot express.
	mutate := func(t *testing.T, fn func(rec map[string]json.RawMessage, att map[string]json.RawMessage)) json.RawMessage {
		t.Helper()
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var att map[string]json.RawMessage
		if err := json.Unmarshal(rec["origin_attestation"], &att); err != nil {
			t.Fatalf("unmarshal attestation: %v", err)
		}
		fn(rec, att)
		if _, still := rec["origin_attestation"]; still {
			b, err := json.Marshal(att)
			if err != nil {
				t.Fatalf("marshal attestation: %v", err)
			}
			rec["origin_attestation"] = b
		}
		out, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return out
	}

	for _, c := range []struct {
		name   string
		build  func(t *testing.T) json.RawMessage
		wantIn string
	}{
		{
			name: "a subject that is not the record's sender",
			build: func(t *testing.T) json.RawMessage {
				return mutate(t, func(_, att map[string]json.RawMessage) {
					att["agent_id"] = json.RawMessage(`"` + originAgentIDOn(t, originPeerBusID, "mallory") + `"`)
				})
			},
			wantIn: "the message's sender is",
		},
		{
			name: "a subject that is not a fully-qualified agent id",
			build: func(t *testing.T) json.RawMessage {
				return mutate(t, func(_, att map[string]json.RawMessage) {
					att["agent_id"] = json.RawMessage(`"not-qualified"`)
				})
			},
			wantIn: "cannot be canonicalized",
		},
		{
			name: "a messaging public key of the wrong size",
			build: func(t *testing.T) json.RawMessage {
				return mutate(t, func(_, att map[string]json.RawMessage) {
					att["messaging_public_key"] = json.RawMessage(`"YWJj"`) // 3 bytes
				})
			},
			wantIn: "messaging public key",
		},
		{
			name: "a signature of the wrong size",
			build: func(t *testing.T) json.RawMessage {
				return mutate(t, func(_, att map[string]json.RawMessage) {
					att["signature"] = json.RawMessage(`"YWJj"`)
				})
			},
			wantIn: "origin attestation signature",
		},
		{
			// Incoherent: this bus mints no attestation for its own traffic, so a
			// record claiming one WITHOUT an origin says another bus vouches for a
			// message we originated.
			name: "an attestation with NO origin message id",
			build: func(t *testing.T) json.RawMessage {
				return mutate(t, func(rec, _ map[string]json.RawMessage) {
					delete(rec, "origin_message_id")
				})
			},
			wantIn: "but no origin message id",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := c.build(t)
			got, err := store.Decode(bad)
			if err == nil {
				t.Fatalf(`Decode ACCEPTED a record this build could never have written and returned %+v.

Decode is the boundary for bytes this process did not validate, and it must apply
exactly the rule Message.WithRelayOrigin applies on the way in -- otherwise a
restart reloads state the write path refuses to create, and the rebuilt envelope
is refused by the next hop as a forgery.`, got)
			}
			if !errors.Is(err, store.ErrInvalidMessage) {
				t.Fatalf("Decode = %v, which is not errors.Is(store.ErrInvalidMessage)", err)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("refused, but for the wrong reason: %v\nwant an error mentioning %q", err, c.wantIn)
			}
			if !strings.Contains(err.Error(), good.ID) {
				t.Errorf("the refusal does not name the message it discarded (%s), so the operator log cannot say WHICH record went: %v", good.ID, err)
			}
		})
	}
}

// TestDecodeAcceptsAnOriginIDWithNoAttestation pins the LENIENT direction, and it
// is lenient on purpose.
//
// Every record written before this field existed looks exactly like this, as does
// anything WithOriginMessageID alone still produces. Such a message decodes,
// serves and is delivered to its local recipients unchanged; what is lost is its
// ONWARD hop alone, and cmd/agent-bus's recovery seam refuses THAT by name with
// the reason logged.
//
// The alternative — refusing the record — would discard a whole durable MESSAGE
// to protect one hop of it. Invariant 6 sanctions a discard; it does not ask for
// a wider one than the fault.
func TestDecodeAcceptsAnOriginIDWithNoAttestation(t *testing.T) {
	sender := originAgentIDOn(t, originPeerBusID, "alpha")
	m, err := store.NewMessageWithBusPath(
		testBusID, sender, false, []string{agentIDFor(t, "beta")}, 5,
		time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC), []byte("pre-RELAY-48 shape"),
		"k", testTimestampMs, testSignature(t), []string{originPeerBusID, testBusID},
	)
	if err != nil {
		t.Fatalf("store.NewMessageWithBusPath: %v", err)
	}
	m, err = m.WithOriginMessageID(originPeerBusID + "-7")
	if err != nil {
		t.Fatalf("WithOriginMessageID: %v", err)
	}
	raw, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := store.Decode(raw)
	if err != nil {
		t.Fatalf(`Decode REFUSED a record carrying an origin message id and no attestation: %v

That is the shape of every record written before RELAY-48. Refusing it discards a
whole durable message to protect its onward hop, which is a wider blast radius
than the fault.`, err)
	}
	if got.OriginMessageID != originPeerBusID+"-7" {
		t.Fatalf("the recovered origin message id is %q", got.OriginMessageID)
	}
	if got.HasOriginAttestation() {
		t.Fatal("Decode invented an attestation for a record that carries none")
	}
}
