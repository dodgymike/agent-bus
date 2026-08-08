package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
)

// ---------------------------------------------------------------------------
// The test federation's keys.
// ---------------------------------------------------------------------------

// attestationContext domain-separates the FAKE attestation this test keyring
// builds, so it can never be confused with signing.Context's canonical message
// bytes. CRYPTO-4 owns the real bundle format; this is only enough of one to
// make the CrossBusTrust seam exercisable — an origin bus SIGNS its agent's
// messaging key with its BUS SIGNING key, and a verifier may check that
// signature against nothing but the peering-time pins it was handed.
const attestationContext = "agent-bus-test/attested-agent-key/1|"

// attestedAgent is one agent's messaging key plus the ORIGIN bus's attestation
// of it.
type attestedAgent struct {
	priv        ed25519.PrivateKey
	pub         ed25519.PublicKey
	attestation []byte // signed by the agent's OWN bus's signing key
}

// testKeyring mints, and then remembers, one Ed25519 BUS SIGNING key per bus id
// and one Ed25519 MESSAGING key per fully-qualified agent id.
//
// The two are deliberately different kinds of key held in different maps, for
// the reason CrossBusTrust's doc gives: a bus signing key attests AGENT KEY
// BUNDLES and is pinned by PEERS, while an agent's messaging key signs the
// MESSAGE. A keyring that conflated them would let a test pass that verified a
// message signature with a bus key, which is the category error VerifyRelayed's
// doc warns about.
//
// Keys are minted lazily and at random (ed25519.GenerateKey(rand.Reader)): no
// test depends on a key's VALUE, only on which key signed what, so there is
// nothing to pin to a fixture file.
type testKeyring struct {
	mu     sync.Mutex
	buses  map[string]ed25519.PrivateKey
	agents map[string]attestedAgent
}

// testKeys is the one keyring the whole package's tests share, so that a
// message signed by one test's fixture verifies under another's trust.
var testKeys = &testKeyring{
	buses:  map[string]ed25519.PrivateKey{},
	agents: map[string]attestedAgent{},
}

// busSigningKey returns busID's bus SIGNING key, minting it on first use.
func (k *testKeyring) busSigningKey(busID string) ed25519.PrivateKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.busSigningKeyLocked(busID)
}

func (k *testKeyring) busSigningKeyLocked(busID string) ed25519.PrivateKey {
	if priv, ok := k.buses[busID]; ok {
		return priv
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("relay test: generating a bus signing key: " + err.Error())
	}
	k.buses[busID] = priv
	return priv
}

// busPins returns busID's signing key as a peer would hold it after peering.
func (k *testKeyring) busPins(busID string) []ed25519.PublicKey {
	priv := k.busSigningKey(busID)
	return []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}
}

// agent returns the messaging key for a fully-qualified agent id together with
// the attestation its OWN bus made of it, minting both on first use.
func (k *testKeyring) agent(fqAgentID string) attestedAgent {
	busID, _, _, err := ids.ParseAgentID(fqAgentID)
	if err != nil {
		panic("relay test: not a fully-qualified agent id: " + err.Error())
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if a, ok := k.agents[fqAgentID]; ok {
		return a
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic("relay test: generating an agent messaging key: " + err.Error())
	}
	a := attestedAgent{
		priv:        priv,
		pub:         pub,
		attestation: ed25519.Sign(k.busSigningKeyLocked(busID), attestationBytes(fqAgentID, pub)),
	}
	k.agents[fqAgentID] = a
	return a
}

func attestationBytes(fqAgentID string, pub ed25519.PublicKey) []byte {
	return append([]byte(attestationContext+fqAgentID+"|"), pub...)
}

// ---------------------------------------------------------------------------
// The CrossBusTrust fake.
// ---------------------------------------------------------------------------

// testTrust is a CrossBusTrust over testKeys.
//
// It is a FAITHFUL fake rather than a stub: AttestedSignerKey actually verifies
// the origin bus's attestation of the agent's messaging key against the pins it
// was handed, AND NOTHING ELSE. That is what makes the two-method seam mean
// something in a test — a stub that simply looked a key up by name would pass
// identically whether the pins were consulted or ignored, which is exactly the
// space the old one-method SignerKeyResolver left open.
type testTrust struct {
	// t, when set, makes the seam assertion FAIL THE TEST rather than merely
	// refuse. Left nil for the shared federation-wide trust the non-SIGN-7
	// tests use, because those call it from goroutines a *testing.T must not be
	// touched from.
	t testing.TB

	// unpeered names bus ids we hold NO peering-time pin for. Everything else
	// in the keyring is treated as peered.
	unpeered map[string]bool

	// pinErr names bus ids whose pin lookup FAILS, which VerifyRelayed must
	// treat exactly as "no pins": ErrUnpeeredBus.
	pinErr map[string]bool

	// malformedPin names bus ids whose stored pin is the wrong length — a
	// broken PINNING STORE, which must be refused rather than skipped.
	malformedPin map[string]bool

	mu          sync.Mutex
	pinCalls    []string
	attestCalls []string
	seamBreaks  int
}

func (tr *testTrust) PinnedBusSigningKeys(busID string) ([]ed25519.PublicKey, error) {
	tr.mu.Lock()
	tr.pinCalls = append(tr.pinCalls, busID)
	tr.mu.Unlock()

	if tr.pinErr[busID] {
		return nil, fmt.Errorf("test: the pinning store is unreachable for %q", busID)
	}
	if tr.unpeered[busID] {
		// An empty slice and an error mean the same thing to VerifyRelayed.
		return nil, nil
	}
	if tr.malformedPin[busID] {
		return []ed25519.PublicKey{ed25519.PublicKey("too-short")}, nil
	}
	return testKeys.busPins(busID), nil
}

func (tr *testTrust) AttestedSignerKey(fqAgentID string, pinnedOriginBusSigningKeys []ed25519.PublicKey) (ed25519.PublicKey, error) {
	tr.mu.Lock()
	tr.attestCalls = append(tr.attestCalls, fqAgentID)
	tr.mu.Unlock()

	busID, _, _, err := ids.ParseAgentID(fqAgentID)
	if err != nil {
		return nil, fmt.Errorf("test: %q is not a fully-qualified agent id: %v", fqAgentID, err)
	}

	// THE SEAM ASSERTION. The pins handed in must be EXACTLY the pins
	// PinnedBusSigningKeys returned for this agent's OWN bus. If they are not,
	// then either the caller looked the pins up for some other bus, or it
	// obtained a key by a route that never consulted a pin at all — which is the
	// hostile-intermediate hole the two-method interface exists to close.
	want, err := tr.PinnedBusSigningKeys(busID)
	if err != nil || !samePins(want, pinnedOriginBusSigningKeys) {
		tr.mu.Lock()
		tr.seamBreaks++
		tr.mu.Unlock()
		if tr.t != nil {
			tr.t.Errorf("AttestedSignerKey(%q) was handed pins that are NOT this bus's peering-time pins; an attestation may be checked against the ORIGIN bus's pins and nothing else", fqAgentID)
		}
		return nil, errors.New("test: attestation offered against pins that are not the origin bus's")
	}
	if len(pinnedOriginBusSigningKeys) == 0 {
		return nil, errors.New("test: no pins were supplied, so no attestation can be checked")
	}

	a := testKeys.agent(fqAgentID)
	for _, pin := range pinnedOriginBusSigningKeys {
		if len(pin) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(pin, attestationBytes(fqAgentID, a.pub), a.attestation) {
			return a.pub, nil
		}
	}
	return nil, fmt.Errorf("test: no pin for bus %q attests a messaging key for %q", busID, fqAgentID)
}

func (tr *testTrust) calls() (pins, attests []string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.pinCalls...), append([]string(nil), tr.attestCalls...)
}

// seamBreaks reports how many times an attestation was offered against pins
// that are not the origin bus's. It is readable so a trust with no *testing.T
// (fakeCrossBusTrustForTest, driven from server goroutines) can still be
// asserted on from the test goroutine.
func (tr *testTrust) seamBreakCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.seamBreaks
}

func samePins(a, b []ed25519.PublicKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// fakeCrossBusTrustForTest is the trust every non-SIGN-7 test's handler is built
// with: it has pinned every bus in the keyring. It carries no *testing.T,
// because handlers built from it are driven from server goroutines.
//
// The name says FAKE and says FOR TEST on purpose. This value is a test double
// for CrossBusTrust — it mints its own keys and attests whatever it is asked
// about — and a skimmer who mistook it for a shippable trust would be reading a
// bus that verifies every peer against keys it generated itself. It lives in a
// _test.go file, so it cannot be linked into the binary; the name is the second
// line of defence, for the reader rather than the compiler.
var fakeCrossBusTrustForTest = &testTrust{}

// newTestTrust returns a trust bound to t, so the seam assertion fails the test
// rather than merely refusing the message.
func newTestTrust(t testing.TB) *testTrust {
	t.Helper()
	return &testTrust{t: t}
}

// ---------------------------------------------------------------------------
// Signing helpers.
// ---------------------------------------------------------------------------

// canonicalizeRelay returns the exact bytes an origin agent signs for req.
func canonicalizeRelay(req RelayRequest) ([]byte, error) {
	_, seq, err := ids.ParseMessageID(req.MessageID)
	if err != nil {
		return nil, err
	}
	return signing.Canonicalize(signing.Message{
		MessageID:          req.MessageID,
		Sequence:           seq,
		Sender:             req.Sender,
		Recipients:         req.Recipients,
		TimestampUnixMilli: req.TimestampUnixMilli,
		Body:               req.Body,
	})
}

// signRelay signs req IN PLACE with the messaging key the keyring holds for
// req.Sender — i.e. exactly as the origin agent on the origin bus would.
func signRelay(req *RelayRequest) error {
	b, err := canonicalizeRelay(*req)
	if err != nil {
		return err
	}
	req.Signature = ed25519.Sign(testKeys.agent(req.Sender).priv, b)
	return nil
}

// mustSignRelay is signRelay for a test that has already asserted the envelope
// is canonicalizable.
func mustSignRelay(t testing.TB, req *RelayRequest) {
	t.Helper()
	if err := signRelay(req); err != nil {
		t.Fatalf("signing the fixture: %v", err)
	}
}

// signRelayedMessage signs a RelayedMessage in place, for the fixtures that
// build one directly (an origin bus's own copy) rather than ingesting one.
func signRelayedMessage(m *RelayedMessage) error {
	b, err := m.CanonicalBytes()
	if err != nil {
		return err
	}
	m.Signature = ed25519.Sign(testKeys.agent(m.Sender).priv, b)
	return nil
}

// ---------------------------------------------------------------------------
// SIGN-7's four named behaviours.
// ---------------------------------------------------------------------------

// TestSign7SignedOnAVerifiesForARecipientOnB is SIGN-7's headline: a message
// signed by an agent on bus A, relayed to bus B, verifies at B against the key
// A attests for that agent — and the bytes B re-derives are BYTE-IDENTICAL to
// the bytes A signed.
//
// Byte-equality is the assertion that matters. "It verified" would also pass if
// both ends happened to agree on some OTHER serialisation; only equality with
// the sender's own bytes proves the wire form and the signed form did not drift,
// which is the failure the old sent_at_unix_ns field guaranteed.
func TestSign7SignedOnAVerifiesForARecipientOnB(t *testing.T) {
	const originSeq = 7
	busA, busB := peerBus, localBus

	messageID, err := ids.MessageID(busA, originSeq)
	if err != nil {
		t.Fatalf("ids.MessageID: %v", err)
	}
	sender := busA + ".beta-1"
	body := []byte("signed on A, read on B")

	// Bus A's agent signs the assignment its own bus minted.
	req := RelayRequest{
		OriginBus:          busA,
		MessageID:          messageID,
		Sender:             sender,
		Recipients:         []string{busB + ".alpha-1", busB + ".gamma-2"},
		BusPath:            []string{busA},
		TimestampUnixMilli: 1754500000000,
		Size:               len(body),
		ContentSHA256:      store.ContentHash(body),
		Body:               body,
	}
	signedBytes, err := canonicalizeRelay(req)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	req.Signature = ed25519.Sign(testKeys.agent(sender).priv, signedBytes)

	// Bus B ingests it, with A's bus signing key pinned from peering.
	trust := newTestTrust(t)
	m, err := ValidateRelayRequest(busB, req.MessageID, req, trust)
	if err != nil {
		t.Fatalf("a correctly signed relayed message was refused at bus B: %v", err)
	}

	got, err := m.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	if !bytes.Equal(got, signedBytes) {
		t.Fatalf("the bytes bus B re-derives are NOT the bytes bus A signed;\n"+
			"  signed by A: %x\n  derived at B: %x", signedBytes, got)
	}
	if !bytes.Equal(m.Signature, req.Signature) {
		t.Errorf("the ingested signature is not the one that arrived:\n got %x\nwant %x", m.Signature, req.Signature)
	}
	if m.OriginSeq != originSeq || m.OriginMessageID != messageID {
		t.Errorf("origin assignment = %q/%d, want %q/%d", m.OriginMessageID, m.OriginSeq, messageID, originSeq)
	}
	if m.TimestampUnixMilli != req.TimestampUnixMilli {
		t.Errorf("timestamp = %d, want the SIGNED integer %d carried through unconverted", m.TimestampUnixMilli, req.TimestampUnixMilli)
	}

	// And the verification really did run through both halves of the seam.
	pins, attests := trust.calls()
	if len(pins) == 0 || pins[0] != busA {
		t.Errorf("PinnedBusSigningKeys calls = %v, want the ORIGIN bus %q first", pins, busA)
	}
	if len(attests) == 0 || attests[0] != sender {
		t.Errorf("AttestedSignerKey calls = %v, want the sender %q", attests, sender)
	}
	if n := trust.seamBreakCount(); n != 0 {
		t.Errorf("the attestation was offered against non-pin keys %d times; VerifyRelayed must hand AttestedSignerKey the ORIGIN bus's peering-time pins and nothing else", n)
	}

	// Verifying the recovered record directly must agree with the ingest.
	if err := VerifyRelayed(m, trust); err != nil {
		t.Fatalf("VerifyRelayed on the recovered record: %v", err)
	}
}

// TestSign7StrippedSignatureIsRejectedAtIngest is the "strip it in transit"
// case. A signature that is absent, truncated or over-long is the same answer —
// ErrMissingSignature — and the caller gets the ZERO RelayedMessage, never a
// partially-trusted one with a body in it.
func TestSign7StrippedSignatureIsRejectedAtIngest(t *testing.T) {
	trust := newTestTrust(t)

	cases := []struct {
		name string
		sig  func(good []byte) []byte
	}{
		{"stripped entirely", func([]byte) []byte { return nil }},
		{"empty but present", func([]byte) []byte { return []byte{} }},
		{"truncated by one byte", func(good []byte) []byte { return good[:len(good)-1] }},
		{"one byte too long", func(good []byte) []byte { return append(append([]byte(nil), good...), 0) }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := relayFixture()
			req.Signature = tc.sig(req.Signature)

			m, err := ValidateRelayRequest(localBus, req.MessageID, req, trust)
			if !errors.Is(err, ErrMissingSignature) {
				t.Fatalf("error = %v, want one wrapping ErrMissingSignature", err)
			}
			if m.Body != nil {
				t.Errorf("the rejected ingest returned a message with a %d-byte body; a refusal must never hand a caller anything to act on", len(m.Body))
			}
			if !reflect.DeepEqual(m, RelayedMessage{}) {
				t.Errorf("the rejected ingest returned %+v, want the ZERO RelayedMessage: there is no partially-trusted result", m)
			}
		})
	}

	// AND THE AGENT-FACING SURFACE MUST REFUSE IT TOO. A library function that
	// says no while the handler says yes is the same hole with an extra step.
	t.Run("the relay ingress answers 400/unsigned and delivers nothing", func(t *testing.T) {
		remote := newRelayResponder(t, localBus, nil)
		req := relayFixture()
		req.Signature = nil

		status, code, _ := remote.postRelay(t, req)
		if status != http.StatusBadRequest || code != CodeUnsigned {
			t.Fatalf("an unsigned envelope gave %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeUnsigned)
		}
		if n := len(remote.acceptedMessages()); n != 0 {
			t.Fatalf("AcceptRelay was called %d times for an unsigned envelope, want 0", n)
		}
		if got := remote.h.Stats().Accepted; got != 0 {
			t.Errorf("Accepted = %d, want 0", got)
		}
	})
}

// TestSign7MutatedFieldNeverReachesDelivery walks EVERY field the signature
// covers, mutates it AFTER signing — an intermediate bus tampering in transit —
// and asserts two things: the ingest rejects it, and NOTHING DOWNSTREAM EVER
// SEES IT. "It returned an error" is the weaker claim; the AcceptRelay callback
// never being invoked is the one that matters, because that callback is where a
// wiring site makes the message durable and delivers it.
//
// bus_path is included as the CONTRAST case: it is outside the signature,
// permanently and by construction (it GROWS on every hop), so changing it must
// never be a signature failure.
func TestSign7MutatedFieldNeverReachesDelivery(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*RelayRequest)
		// want is the sentinel the mutation must be rejected with. A nil want
		// means the mutation is LEGAL and must be accepted.
		want error
		why  string
	}{
		{
			name: "message id",
			mut:  func(r *RelayRequest) { r.MessageID = peerBus + "-2" },
			want: ErrBadSignature,
			why:  "the signed bytes name the origin's assignment; re-pointing it is a forgery",
		},
		{
			name: "sender",
			mut:  func(r *RelayRequest) { r.Sender = peerBus + ".beta-2" },
			want: ErrBadSignature,
			why:  "the key attested for the substituted agent does not verify the original's signature",
		},
		{
			name: "a recipient is re-pointed",
			mut:  func(r *RelayRequest) { r.Recipients = []string{localBus + ".mallory-1"} },
			want: ErrBadSignature,
			why:  "the recipient set is signed, so a message cannot be re-addressed on the path",
		},
		{
			name: "a recipient is added",
			mut: func(r *RelayRequest) {
				r.Recipients = append(append([]string(nil), r.Recipients...), localBus+".mallory-1")
			},
			want: ErrBadSignature,
			why:  "widening the audience is a change to the signed recipient set",
		},
		{
			name: "timestamp",
			mut:  func(r *RelayRequest) { r.TimestampUnixMilli++ },
			want: ErrBadSignature,
			why:  "the signed clock reading is the exact integer inside the canonical bytes",
		},
		{
			name: "one byte of the body, with the declared hash left alone",
			mut:  func(r *RelayRequest) { r.Body[0] ^= 0x01 },
			want: ErrInvalidRelay,
			why:  "the SHAPE check fires first: the body no longer hashes to its declared digest (check 9), before any cryptography is spent",
		},
		{
			name: "one byte of the body, with the hash and size recomputed",
			mut: func(r *RelayRequest) {
				r.Body[0] ^= 0x01
				r.Size = len(r.Body)
				r.ContentSHA256 = store.ContentHash(r.Body)
			},
			want: ErrBadSignature,
			why:  "the content hash is NOT signed, so a tamperer updates it; the signature is what catches this",
		},
		{
			// THE CONTRAST CASE. bus_path is outside the signature FOREVER.
			name: "bus_path gains a hop",
			mut:  func(r *RelayRequest) { r.BusPath = []string{peerBus, thirdBus} },
			want: nil,
			why:  "the path grows on every hop, so it can never be inside the signature; changing it is normal routing, never a forgery",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// A responder, so "nothing downstream saw it" is a real assertion
			// about a real callback rather than about a return value.
			remote := newRelayResponder(t, localBus, nil)

			req := relayFixture()
			tc.mut(&req)

			_, err := ValidateRelayRequest(localBus, req.MessageID, req, newTestTrust(t))
			status, code, _ := remote.postRelay(t, req)

			if tc.want == nil {
				if err != nil {
					t.Fatalf("a mutation OUTSIDE the signature was refused (%s): %v", tc.why, err)
				}
				if status != http.StatusOK {
					t.Fatalf("the ingress answered %d/%q for a legal path change, want 200 (%s)", status, code, tc.why)
				}
				if n := len(remote.acceptedMessages()); n != 1 {
					t.Fatalf("AcceptRelay was called %d times, want 1: a bus_path change is routing metadata, not tampering", n)
				}
				return
			}

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want one wrapping %v (%s)", err, tc.want, tc.why)
			}
			if n := len(remote.acceptedMessages()); n != 0 {
				t.Fatalf("AcceptRelay was called %d times for a tampered envelope, want 0: a rejected message must never reach delivery (%s)", n, tc.why)
			}
			wantStatus := http.StatusBadRequest
			if errors.Is(err, ErrBadSignature) {
				wantStatus = http.StatusForbidden
			}
			if status != wantStatus {
				t.Errorf("the ingress answered %d/%q, want %d (%s)", status, code, wantStatus, tc.why)
			}
		})
	}
}

// TestSign7LocalDeliverySequenceIsOutsideTheSignedBytes proves the
// origin-id-INSIDE / local-sequence-OUTSIDE split was answered correctly.
//
// PROTOCOL.md §8.5 has the receiving bus mint its OWN local delivery sequence,
// and invariant 1 says neither bus cedes id authority to the other. If the local
// number were anywhere in the signed bytes, then either the far bus could not
// mint one at all, or every hop would break verification — so this test asserts
// the byte-level version of the claim rather than the prose one: the local
// number does not appear in the canonical bytes in ANY of the forms the format
// could have encoded it.
func TestSign7LocalDeliverySequenceIsOutsideTheSignedBytes(t *testing.T) {
	const originSeq = 7
	const localSeq = 4001 // deliberately unrelated to the origin's

	originID, err := ids.MessageID(peerBus, originSeq)
	if err != nil {
		t.Fatalf("ids.MessageID: %v", err)
	}
	req := relayFixture(func(r *RelayRequest) { r.MessageID = originID })

	trust := newTestTrust(t)
	m, err := ValidateRelayRequest(localBus, req.MessageID, req, trust)
	if err != nil {
		t.Fatalf("ValidateRelayRequest: %v", err)
	}
	before, err := m.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}

	// The far bus mints its own local delivery sequence and binds it BESIDE the
	// relayed record — never inside it. This is the shape a wiring site takes.
	localID, err := ids.MessageID(localBus, localSeq)
	if err != nil {
		t.Fatalf("ids.MessageID: %v", err)
	}
	local := struct {
		relayed        RelayedMessage
		localSeq       uint64
		localMessageID string
	}{relayed: m, localSeq: localSeq, localMessageID: localID}

	// (a) The canonical bytes are UNCHANGED by the local assignment.
	after, err := local.relayed.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes after the local assignment: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("assigning a local delivery sequence changed the signed bytes:\n before %x\n  after %x", before, after)
	}

	// (b) Verification still passes.
	if err := VerifyRelayed(local.relayed, trust); err != nil {
		t.Fatalf("the message stopped verifying once the far bus minted its own sequence: %v", err)
	}

	// (c) The ORIGIN's id is what the record carries, and the local number is
	// nowhere in the signed bytes — in any encoding the format could have used.
	if local.relayed.OriginMessageID != originID || local.relayed.OriginSeq != originSeq {
		t.Errorf("origin assignment = %q/%d, want %q/%d", local.relayed.OriginMessageID, local.relayed.OriginSeq, originID, originSeq)
	}
	if local.localMessageID == local.relayed.OriginMessageID {
		t.Fatal("the local id equals the origin id; this test's premise needs two DIFFERENT numbers")
	}

	var be64, be32 [8]byte
	binary.BigEndian.PutUint64(be64[:], local.localSeq)
	binary.BigEndian.PutUint32(be32[:4], uint32(local.localSeq))
	forms := map[string][]byte{
		"decimal":              []byte(strconv.FormatUint(local.localSeq, 10)),
		"big-endian uint64":    be64[:],
		"big-endian uint32":    be32[:4],
		"the local message id": []byte(local.localMessageID),
	}
	for name, form := range forms {
		if bytes.Contains(before, form) {
			t.Errorf("the canonical bytes contain the far bus's LOCAL delivery sequence in its %s form (%x); the local sequence is outside the signature, or no bus could mint one without breaking every hop", name, form)
		}
	}
}

// ---------------------------------------------------------------------------
// The rest of SIGN-7's surface.
// ---------------------------------------------------------------------------

// TestSign7RelayCapsFitTheCanonicalFormat makes the caps relationship
// EXECUTABLE, exactly as TestRelayIdempotencyKeyIsTheOriginMessageID does for
// the key rule. If relay's caps ever drifted past the format's, a legitimate
// maximum-size signed message would be rejected as UNSIGNED and the operator
// would be told the peer sent no signature.
func TestSign7RelayCapsFitTheCanonicalFormat(t *testing.T) {
	if err := relayFitsCanonicalFormat(); err != nil {
		t.Fatalf("relay's caps are outside the canonical signing format's: %v", err)
	}
	if store.MaxRecipients > signing.MaxRecipients {
		t.Errorf("store.MaxRecipients (%d) exceeds signing.MaxRecipients (%d)", store.MaxRecipients, signing.MaxRecipients)
	}
	if store.MaxBodyBytes > signing.MaxBodyLen {
		t.Errorf("store.MaxBodyBytes (%d) exceeds signing.MaxBodyLen (%d)", store.MaxBodyBytes, signing.MaxBodyLen)
	}

	// And the relationship is not merely numeric: a message at BOTH relay caps
	// must actually canonicalize, sign and verify.
	body := bytes.Repeat([]byte("b"), store.MaxBodyBytes)
	recipients := make([]string, store.MaxRecipients)
	for i := range recipients {
		recipients[i] = fmt.Sprintf("%s.a%d-1", localBus, i)
	}
	req := relayFixture(func(r *RelayRequest) {
		r.Recipients = recipients
		r.Body = body
		r.Size = len(body)
		r.ContentSHA256 = store.ContentHash(body)
	})
	if _, err := ValidateRelayRequest(localBus, req.MessageID, req, newTestTrust(t)); err != nil {
		t.Fatalf("a correctly signed message at BOTH relay caps was refused: %v", err)
	}
}

// TestSign7UnpeeredOriginBusIsRefused pins the rule that an origin bus we hold
// no peering-time pin for is UNVERIFIABLE BY CONSTRUCTION — and that this is
// intended behaviour, not a gap to be closed with a trust-on-first-use fallback.
//
// The signature in every case below is PERFECTLY VALID. That is the point: the
// refusal does not depend on anything being wrong with the message.
func TestSign7UnpeeredOriginBusIsRefused(t *testing.T) {
	t.Run("no pin for the origin bus", func(t *testing.T) {
		trust := newTestTrust(t)
		trust.unpeered = map[string]bool{peerBus: true}

		req := relayFixture()
		// The signature is genuine: prove it verifies under a trust that HAS
		// peered, so the refusal below is attributable to the missing pin alone.
		if _, err := ValidateRelayRequest(localBus, req.MessageID, req, newTestTrust(t)); err != nil {
			t.Fatalf("the fixture's signature is not valid, so this test would prove nothing: %v", err)
		}

		m, err := ValidateRelayRequest(localBus, req.MessageID, req, trust)
		if !errors.Is(err, ErrUnpeeredBus) {
			t.Fatalf("error = %v, want one wrapping ErrUnpeeredBus", err)
		}
		if !reflect.DeepEqual(m, RelayedMessage{}) {
			t.Errorf("an unpeered origin still produced %+v", m)
		}
		// The pin is checked BEFORE any key lookup, so no attestation was even
		// attempted: there is no branch below the pin that could supply a key.
		if _, attests := trust.calls(); len(attests) != 0 {
			t.Errorf("AttestedSignerKey was called %d times for an UNPEERED bus (%v); the pin check must be unconditional and first, or a later step could be talked into supplying a key from somewhere else", len(attests), attests)
		}
	})

	t.Run("the pin lookup fails", func(t *testing.T) {
		trust := newTestTrust(t)
		trust.pinErr = map[string]bool{peerBus: true}
		req := relayFixture()
		if _, err := ValidateRelayRequest(localBus, req.MessageID, req, trust); !errors.Is(err, ErrUnpeeredBus) {
			t.Fatalf("error = %v, want one wrapping ErrUnpeeredBus: an error and an empty pin set mean the same thing", err)
		}
	})

	t.Run("a malformed pin is refused, not skipped", func(t *testing.T) {
		trust := newTestTrust(t)
		trust.malformedPin = map[string]bool{peerBus: true}
		req := relayFixture()
		if _, err := ValidateRelayRequest(localBus, req.MessageID, req, trust); !errors.Is(err, ErrUnpeeredBus) {
			t.Fatalf("error = %v, want one wrapping ErrUnpeeredBus: a malformed pin means the PINNING STORE is wrong, and skipping it would verify against less than the operator believes is pinned", err)
		}
	})

	t.Run("a nil trust is a refusal, never a skip", func(t *testing.T) {
		req := relayFixture()
		m, err := ValidateRelayRequest(localBus, req.MessageID, req, nil)
		if !errors.Is(err, ErrNoSignerKey) {
			t.Fatalf("error = %v, want one wrapping ErrNoSignerKey; a nil-means-allow branch is one missing argument away from an unauthenticated relay ingress", err)
		}
		if !reflect.DeepEqual(m, RelayedMessage{}) {
			t.Errorf("a nil trust still produced %+v", m)
		}
		if err := VerifyRelayed(RelayedMessage{}, nil); !errors.Is(err, ErrNoSignerKey) {
			t.Errorf("VerifyRelayed(_, nil) = %v, want one wrapping ErrNoSignerKey", err)
		}
	})

	t.Run("the ingress answers 403 unpeered_bus", func(t *testing.T) {
		trust := &testTrust{unpeered: map[string]bool{peerBus: true}}
		remote := newRelayResponder(t, localBus, func(c *RelayConfig) { c.Trust = trust })

		status, code, _ := remote.postRelay(t, relayFixture())
		if status != http.StatusForbidden || code != CodeUnpeeredBus {
			t.Fatalf("an unpeered origin gave %d/%q, want %d/%q: it is a distinct OPERATOR problem from a bad signature — peer the buses, do not hunt a forgery", status, code, http.StatusForbidden, CodeUnpeeredBus)
		}
		if n := len(remote.acceptedMessages()); n != 0 {
			t.Fatalf("AcceptRelay was called %d times for an unverifiable envelope, want 0", n)
		}
	})

	t.Run("a signer we will not attribute is 403 bad_signature", func(t *testing.T) {
		// Peered, but the sender's messaging key is not one this bus will
		// attribute: a DIFFERENT error, a different remedy, the same refusal.
		remote := newRelayResponder(t, localBus, nil)
		req := relayFixture()
		// Re-sign with a key that belongs to a different agent entirely.
		b, err := canonicalizeRelay(req)
		if err != nil {
			t.Fatalf("Canonicalize: %v", err)
		}
		req.Signature = ed25519.Sign(testKeys.agent(peerBus+".impostor-1").priv, b)

		status, code, _ := remote.postRelay(t, req)
		if status != http.StatusForbidden || code != CodeBadSignature {
			t.Fatalf("a message signed by another agent's key gave %d/%q, want %d/%q", status, code, http.StatusForbidden, CodeBadSignature)
		}
		if n := len(remote.acceptedMessages()); n != 0 {
			t.Fatalf("AcceptRelay was called %d times, want 0", n)
		}
	})
}

// TestSign7RelayedBroadcastIsUnsignable is the TRIPWIRE for SIGN-3.
//
// signing.Canonicalize refuses an empty recipient set, and a relayed broadcast
// carries no recipient list by construction, so under canonical format v1 there
// are NO canonical bytes for a relayed broadcast and therefore no signature over
// one can exist. The honest answer is a refusal.
//
// SIGN-3 ("Broadcast signature covers the recipient set") is the task that must
// resolve this, by defining what a broadcast's signed audience IS. This test
// fires if anyone instead "fixes" broadcasts by exempting them from the
// signature requirement — which would be an unauthenticated downgrade selectable
// from the wire, on the surface with the largest blast radius.
func TestSign7RelayedBroadcastIsUnsignable(t *testing.T) {
	req := relayFixture(func(r *RelayRequest) {
		r.Broadcast = true
		r.Recipients = nil
	})

	// The premise, stated rather than assumed: there are no bytes to sign.
	if _, err := canonicalizeRelay(req); !errors.Is(err, signing.ErrInvalid) {
		t.Fatalf("Canonicalize of a broadcast gave %v, want one wrapping signing.ErrInvalid; if a broadcast became canonicalizable, SIGN-3 has landed and this test must be revisited deliberately", err)
	}

	m, err := ValidateRelayRequest(localBus, req.MessageID, req, newTestTrust(t))
	if !errors.Is(err, ErrUnsignable) {
		t.Fatalf("error = %v, want one wrapping ErrUnsignable (SIGN-3)", err)
	}
	if !reflect.DeepEqual(m, RelayedMessage{}) {
		t.Errorf("a relayed broadcast still produced %+v", m)
	}

	remote := newRelayResponder(t, localBus, nil)
	status, code, _ := remote.postRelay(t, req)
	if status != http.StatusBadRequest || code != CodeUnsigned {
		t.Fatalf("a relayed broadcast gave %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeUnsigned)
	}
	if n := len(remote.acceptedMessages()); n != 0 {
		t.Fatalf("AcceptRelay was called %d times for a relayed broadcast, want 0", n)
	}
}

// TestSign7ForwardIsVerbatimAcrossTwoHops is the real "no mutation on the path"
// proof: A signs, B ingests and forwards, C ingests — and what C verifies is
// A's ORIGINAL signature over A's ORIGINAL bytes.
//
// One hop cannot show this, because the ingest and the forward are the same
// values. Two hops is where a normalisation — a re-hash, a re-encode, a
// re-order, a re-sign — would show up.
func TestSign7ForwardIsVerbatimAcrossTwoHops(t *testing.T) {
	busA, busB, busC := peerBus, localBus, thirdBus
	trust := newTestTrust(t)

	atA := relayFixture()
	signedBytes, err := canonicalizeRelay(atA)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}

	// Hop 1: A -> B.
	atB, err := ValidateRelayRequest(busB, atA.MessageID, atA, trust)
	if err != nil {
		t.Fatalf("bus B refused A's message: %v", err)
	}

	// B forwards. The ONLY difference must be one extra hop on the path.
	onward, err := atB.Forward(busB)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if len(onward.BusPath) != 2 || onward.BusPath[0] != busA || onward.BusPath[1] != busB {
		t.Fatalf("bus path = %v, want [%s %s]", onward.BusPath, busA, busB)
	}
	if !bytes.Equal(onward.Signature, atA.Signature) {
		t.Fatalf("Forward changed the signature:\n got %x\nwant %x", onward.Signature, atA.Signature)
	}
	for _, f := range []struct {
		name      string
		got, want interface{}
	}{
		{"origin_bus", onward.OriginBus, atA.OriginBus},
		{"message_id", onward.MessageID, atA.MessageID},
		{"sender", onward.Sender, atA.Sender},
		{"broadcast", onward.Broadcast, atA.Broadcast},
		{"timestamp_unix_ms", onward.TimestampUnixMilli, atA.TimestampUnixMilli},
		{"size", onward.Size, atA.Size},
		{"content_sha256", onward.ContentSHA256, atA.ContentSHA256},
	} {
		if f.got != f.want {
			t.Errorf("Forward changed %s: got %v, want %v", f.name, f.got, f.want)
		}
	}
	if !bytes.Equal(onward.Body, atA.Body) {
		t.Errorf("Forward changed the body")
	}
	if !reflect.DeepEqual(onward.Recipients, atA.Recipients) {
		t.Errorf("Forward changed the recipients: got %v, want %v", onward.Recipients, atA.Recipients)
	}

	// The forwarded envelope must still re-derive A's EXACT signed bytes...
	if got, err := canonicalizeRelay(onward); err != nil || !bytes.Equal(got, signedBytes) {
		t.Fatalf("the forwarded envelope no longer canonicalizes to A's signed bytes (err %v):\n got %x\nwant %x", err, got, signedBytes)
	}

	// Hop 2: B -> C. C has never spoken to A's agent, only pinned A's bus key.
	atC, err := ValidateRelayRequest(busC, onward.MessageID, onward, trust)
	if err != nil {
		t.Fatalf("bus C refused the twice-relayed message: %v; PROTOCOL.md §8.5 requires the signed values to survive every hop", err)
	}
	if !bytes.Equal(atC.Signature, atA.Signature) {
		t.Fatalf("the signature C verified is not the one A produced")
	}
	got, err := atC.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes at C: %v", err)
	}
	if !bytes.Equal(got, signedBytes) {
		t.Fatalf("two hops changed the signed bytes:\n signed by A: %x\n derived at C: %x", signedBytes, got)
	}
	if err := VerifyRelayed(atC, trust); err != nil {
		t.Fatalf("A's original signature no longer verifies at C: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SIGN-7 follow-up (P1): the FINGERPRINT and the SIGNATURE must agree about
// what "the same message" is.
// ---------------------------------------------------------------------------

// permutedRecipients is the recipient set the permutation regression tests use.
//
// The three ids are deliberately chosen so that SORTED order differs from BOTH
// wire orders below — a fixture whose wire order happened to be sorted would
// pass against the order-sensitive code and prove nothing.
var (
	permutedRecipientsSorted = []string{localBus + ".alpha-1", localBus + ".beta-2", localBus + ".gamma-3"}
	permutedRecipientsWireA  = []string{localBus + ".gamma-3", localBus + ".alpha-1", localBus + ".beta-2"}
	permutedRecipientsWireB  = []string{localBus + ".beta-2", localBus + ".gamma-3", localBus + ".alpha-1"}
)

// TestSign7RecipientPermutationCannotGetAnHonestPeerDisconnected is the
// regression test for the P1 the SIGN-7 review found. It FAILS against the
// order-sensitive relayFingerprint that shipped first.
//
// # THE ATTACK IT CLOSES
//
// signing.Canonicalize SORTS A COPY of the recipient set before encoding it
// (canonical.go), so recipient ORDER IS NOT PART OF THE SIGNED PAYLOAD: one
// signature covers every permutation of one recipient set. relayFingerprint
// used to fold the recipients in WIRE order, so it treated a permutation as a
// DIFFERENT payload.
//
// Invariant 10 turns that disagreement into a weapon. Same idempotency key with
// a different fingerprint is idem.OutcomeViolation, which is a 409 refusal plus
// a protocol-violation log line. So a hostile peer could take an honest peer's
// legitimately signed message, reorder nothing but the recipient array, forward
// it, and have this bus REFUSE the honest peer's own copy as a protocol
// violation — a copy that still carried a perfectly valid signature over the
// very same audience. The victim has done nothing wrong, its message does not
// arrive, and it is logged as the offender; the attacker spends one forwarded
// copy. (Before invariant 10 was narrowed on 2026-08-08 the same refusal also
// closed the honest peer's connection. The narrowing shrank the blast radius; it
// did not make the misattribution acceptable, which is why this test stands.)
//
// The fix is that the fingerprint defines "same payload" exactly as the
// signature does: sorted here, sorted there, one meaning. This test asserts the
// two halves of that — the signature really is indifferent to order (both wire
// orders carry the SAME signature bytes and both verify), so the fingerprint
// must be indifferent too.
func TestSign7RecipientPermutationCannotGetAnHonestPeerDisconnected(t *testing.T) {
	// The premise, asserted rather than assumed.
	if sort.StringsAreSorted(permutedRecipientsWireA) || sort.StringsAreSorted(permutedRecipientsWireB) {
		t.Fatalf("this fixture's wire orders (%v, %v) must BOTH differ from sorted order (%v), or the test would pass against order-sensitive code",
			permutedRecipientsWireA, permutedRecipientsWireB, permutedRecipientsSorted)
	}
	if reflect.DeepEqual(permutedRecipientsWireA, permutedRecipientsWireB) {
		t.Fatal("the two wire orders are identical; this test needs two DIFFERENT permutations of one set")
	}

	// The honest peer's copy: signed ONCE, over wire order A.
	honest := relayFixture(func(r *RelayRequest) {
		r.Recipients = append([]string(nil), permutedRecipientsWireA...)
	})
	// The reordering peer's copy: byte-for-byte the same message with the
	// recipient array permuted, and THE SAME SIGNATURE — nothing is re-signed,
	// because a reordering peer has no key to re-sign with. That is the whole
	// point: this envelope is what an attacker can build from a message it
	// merely forwarded.
	reordered := honest
	reordered.Recipients = append([]string(nil), permutedRecipientsWireB...)
	if !bytes.Equal(reordered.Signature, honest.Signature) {
		t.Fatal("the reordered copy must carry the ORIGINAL signature bytes; an attacker cannot re-sign")
	}

	t.Run("both permutations verify under one signature", func(t *testing.T) {
		// If the signature were order-sensitive there would be no vulnerability
		// to regress against — the reordered copy would simply be a forgery. So
		// prove it is not, at the byte level, before asserting anything else.
		bytesA, err := canonicalizeRelay(honest)
		if err != nil {
			t.Fatalf("Canonicalize(wire order A): %v", err)
		}
		bytesB, err := canonicalizeRelay(reordered)
		if err != nil {
			t.Fatalf("Canonicalize(wire order B): %v", err)
		}
		if !bytes.Equal(bytesA, bytesB) {
			t.Fatalf("two permutations of ONE recipient set canonicalize differently:\n A: %x\n B: %x\nsigning.Canonicalize sorts a copy, so recipient order is outside the signed bytes", bytesA, bytesB)
		}
	})

	t.Run("a permutation produces an IDENTICAL fingerprint", func(t *testing.T) {
		trust := newTestTrust(t)

		one, err := ValidateRelayRequest(localBus, honest.MessageID, honest, trust)
		if err != nil {
			t.Fatalf("the honest peer's copy was refused: %v", err)
		}
		two, err := ValidateRelayRequest(localBus, reordered.MessageID, reordered, trust)
		if err != nil {
			t.Fatalf("the reordered copy was refused: %v; a permutation is inside the signature's notion of one message, so it must ingest", err)
		}

		if one.Fingerprint != two.Fingerprint {
			t.Fatalf("two permutations of ONE recipient set have DIFFERENT fingerprints (%x vs %x).\n"+
				"The signature says these are the same message — both copies carry the SAME signature bytes and both verify — so the fingerprint must too.\n"+
				"With them disagreeing, invariant 10 classifies the pair as idem.OutcomeViolation, so a hostile peer that reorders a recipient array gets an HONEST peer's own signed message refused with a 409 and logged as a protocol violation.",
				one.Fingerprint, two.Fingerprint)
		}
		// The scopes must agree too, or the second copy would never be looked up
		// against the first and the fingerprint would never be compared at all.
		sc1, err := one.Scope()
		if err != nil {
			t.Fatalf("Scope: %v", err)
		}
		sc2, err := two.Scope()
		if err != nil {
			t.Fatalf("Scope: %v", err)
		}
		if sc1 != sc2 {
			t.Fatal("the two copies resolve to different idem.Scopes, so nothing would ever compare their fingerprints")
		}

		// Both carry the ORIGINAL signature, and both verify. This is what makes
		// the equality above load-bearing rather than a coincidence.
		if !bytes.Equal(one.Signature, honest.Signature) || !bytes.Equal(two.Signature, honest.Signature) {
			t.Fatalf("the ingested signatures are not the one signature that arrived:\n A: %x\n B: %x\n want: %x", one.Signature, two.Signature, honest.Signature)
		}
		if err := VerifyRelayed(one, trust); err != nil {
			t.Errorf("wire order A stopped verifying: %v", err)
		}
		if err := VerifyRelayed(two, trust); err != nil {
			t.Errorf("wire order B stopped verifying: %v", err)
		}
		if n := trust.seamBreakCount(); n != 0 {
			t.Errorf("the attestation was offered against non-pin keys %d times", n)
		}
	})

	// THE ATTACK, DRIVEN THROUGH THE ACTUAL INGRESS. The AcceptRelay callback is
	// wired to a REAL idem.Store, exactly the shape RelayConfig.AcceptRelay's doc
	// describes a wiring site must have, so the outcome asserted here is the one
	// invariant 10 actually produces rather than a fingerprint comparison
	// standing in for it.
	t.Run("the ingress answers 200/duplicate, NOT 409 idempotency_violation", func(t *testing.T) {
		remote, applied := newIdempotentRelayResponder(t, localBus)

		status, code, resp := remote.postRelay(t, honest)
		if status != http.StatusOK || !resp.Accepted || resp.Duplicate {
			t.Fatalf("the honest peer's copy gave %d/%q %+v, want 200 accepted, not duplicate", status, code, resp)
		}

		status, code, resp = remote.postRelay(t, reordered)
		if status == http.StatusConflict || code == CodeIdempotencyViolation {
			t.Fatalf("a REORDERED recipient array gave %d/%q: the bus has classified an honest peer's own signed message as an idempotency VIOLATION, so the honest copy is refused and the honest peer is the one logged as the offender. Reordering is free for an attacker and the victim's copy is perfectly valid.", status, code)
		}
		if status != http.StatusOK {
			t.Fatalf("the reordered copy gave %d/%q, want 200", status, code)
		}
		if !resp.Accepted || !resp.Duplicate {
			t.Fatalf("the reordered copy gave %+v, want accepted+duplicate: same key, same payload, so it is a RETRY — replay the original result, apply nothing, disconnect nobody", resp)
		}
		if resp.MessageID != localBus+"-1" {
			t.Errorf("the duplicate replayed message id %q, want the ORIGINAL result %q", resp.MessageID, localBus+"-1")
		}
		if n := len(remote.acceptedMessages()); n != 1 {
			t.Fatalf("the message was applied %d times, want 1: a retry must not be re-applied", n)
		}
		if n := remote.h.Stats().Duplicates; n != 1 {
			t.Errorf("Duplicates = %d, want 1", n)
		}
		if n := applied(); n != 1 {
			t.Errorf("the idem.Store remembered %d records, want 1", n)
		}
	})

	// THE GUARD AGAINST OVER-CORRECTING. Sorting must collapse a PERMUTATION and
	// nothing else: re-ADDRESSING a message changes the recipient SET, which
	// changes the sorted list, which changes the canonical bytes AND the
	// fingerprint. If this half broke, a retry could quietly widen, narrow or
	// re-point the audience of an already-accepted message and be waved through
	// as "the same payload".
	t.Run("a genuinely different recipient SET still changes the fingerprint", func(t *testing.T) {
		base := relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			permutedRecipientsWireA, 100, len(testBody), store.ContentHash(testBody))

		for name, set := range map[string][]string{
			"one recipient ADDED":       {localBus + ".gamma-3", localBus + ".alpha-1", localBus + ".beta-2", localBus + ".mallory-4"},
			"one recipient REMOVED":     {localBus + ".gamma-3", localBus + ".alpha-1"},
			"one recipient SUBSTITUTED": {localBus + ".gamma-3", localBus + ".mallory-4", localBus + ".beta-2"},
			"the whole set replaced":    {localBus + ".mallory-4"},
		} {
			if other := relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
				set, 100, len(testBody), store.ContentHash(testBody)); base == other {
				t.Errorf("%s produced the SAME fingerprint as the original set; sorting must collapse a PERMUTATION and nothing else, or a retry could re-address an already-accepted message", name)
			}
		}

		// And end-to-end, through the real ingest path with a real signature over
		// the widened set.
		widened := relayFixture(func(r *RelayRequest) {
			r.Recipients = append(append([]string(nil), permutedRecipientsWireA...), localBus+".mallory-4")
		})
		trust := newTestTrust(t)
		one, err := ValidateRelayRequest(localBus, honest.MessageID, honest, trust)
		if err != nil {
			t.Fatalf("ValidateRelayRequest(original): %v", err)
		}
		two, err := ValidateRelayRequest(localBus, widened.MessageID, widened, trust)
		if err != nil {
			t.Fatalf("ValidateRelayRequest(widened): %v", err)
		}
		if one.Fingerprint == two.Fingerprint {
			t.Fatal("adding a recipient did not change the fingerprint; re-addressing a message must never be indistinguishable from retrying it")
		}
	})

	// NO SIDE EFFECTS. The fingerprint sorts a COPY. The caller's slice — and the
	// RelayedMessage the caller is about to route, deliver, log and forward —
	// must still carry the WIRE order it arrived in, because Forward re-emits
	// these exact values and PROTOCOL.md §8.5 forbids normalising anything on the
	// path.
	t.Run("fingerprinting does not reorder anything the caller holds", func(t *testing.T) {
		in := append([]string(nil), permutedRecipientsWireA...)
		want := append([]string(nil), permutedRecipientsWireA...)
		relayFingerprint(peerBus, peerBus+"-1", peerBus+".beta-1", false,
			in, 100, len(testBody), store.ContentHash(testBody))
		if !reflect.DeepEqual(in, want) {
			t.Errorf("relayFingerprint reordered its caller's slice: got %v, want %v", in, want)
		}

		req := relayFixture(func(r *RelayRequest) {
			r.Recipients = append([]string(nil), permutedRecipientsWireA...)
		})
		before := append([]string(nil), req.Recipients...)
		m, err := ValidateRelayRequest(localBus, req.MessageID, req, newTestTrust(t))
		if err != nil {
			t.Fatalf("ValidateRelayRequest: %v", err)
		}
		if !reflect.DeepEqual(req.Recipients, before) {
			t.Errorf("ValidateRelayRequest reordered the decoded envelope's recipients: got %v, want %v", req.Recipients, before)
		}
		if !reflect.DeepEqual(m.Recipients, before) {
			t.Errorf("RelayedMessage.Recipients = %v, want the WIRE order %v; Forward re-emits these values verbatim, so a sorted copy here would normalise the envelope on the path", m.Recipients, before)
		}
		onward, err := m.Forward(localBus)
		if err != nil {
			t.Fatalf("Forward: %v", err)
		}
		if !reflect.DeepEqual(onward.Recipients, before) {
			t.Errorf("Forward emitted recipients %v, want the WIRE order %v", onward.Recipients, before)
		}
	})
}

// newIdempotentRelayResponder returns a relayResponder whose AcceptRelay is
// backed by a REAL idem.Store, plus a function reporting how many records that
// store has remembered.
//
// It exists so the permutation regression can be asserted where invariant 10
// actually bites — 200/duplicate versus 409/idempotency_violation on the wire —
// rather than only at the fingerprint. The callback is deliberately the shape
// RelayConfig.AcceptRelay's doc prescribes for a wiring site: look the message
// up by m.Scope() and m.Fingerprint, answer OutcomeViolation with
// ErrIdempotencyViolation, answer OutcomeRetry by REPLAYING the original result
// without re-applying, and remember a newly applied one.
func newIdempotentRelayResponder(t *testing.T, busID string) (*relayResponder, func() int) {
	t.Helper()
	// Named appliedKeys, not "store": internal/store is imported by this file
	// and shadowing a package name inside a callback is how a later edit ends up
	// calling a method on the wrong thing.
	appliedKeys := idem.NewStore(idem.StoreOptions{})
	var mu sync.Mutex
	remembered := 0

	r := newRelayResponder(t, busID, func(c *RelayConfig) {
		apply := c.AcceptRelay
		c.AcceptRelay = func(ctx context.Context, m RelayedMessage) (RelayAcceptance, error) {
			sc, err := m.Scope()
			if err != nil {
				return RelayAcceptance{}, err
			}
			prev, outcome := appliedKeys.Lookup(sc, m.Fingerprint)
			switch outcome {
			case idem.OutcomeViolation:
				return RelayAcceptance{}, ErrIdempotencyViolation
			case idem.OutcomeRetry:
				var id string
				if err := json.Unmarshal(prev.Result, &id); err != nil {
					return RelayAcceptance{}, err
				}
				return RelayAcceptance{LocalMessageID: id, Duplicate: true}, nil
			}
			acc, err := apply(ctx, m)
			if err != nil {
				return RelayAcceptance{}, err
			}
			result, err := json.Marshal(acc.LocalMessageID)
			if err != nil {
				return RelayAcceptance{}, err
			}
			if err := appliedKeys.Remember(idem.Record{
				Agent:       m.Sender,
				Op:          idem.OpRelay,
				Key:         m.IdempotencyKey,
				Fingerprint: m.Fingerprint,
				Result:      result,
				Seq:         m.OriginSeq,
				CommittedAt: time.Now().UTC(),
			}); err != nil {
				return RelayAcceptance{}, err
			}
			mu.Lock()
			remembered++
			mu.Unlock()
			return acc, nil
		}
	})
	return r, func() int {
		mu.Lock()
		defer mu.Unlock()
		return remembered
	}
}
