package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
)

var relay17Now = time.Date(2026, 8, 8, 22, 0, 0, 0, time.UTC)

func relay17OpenStore(t *testing.T, busID string) (*PeerStore, func()) {
	t.Helper()
	st, lg := psOpenStore(t, t.TempDir(), func(o *PeerStoreOptions) {
		o.BusID = busID
		o.Now = func() time.Time { return relay17Now }
	}, nil)
	return st, func() {
		if err := lg.Close(); err != nil {
			t.Errorf("closing peer-store WAL: %v", err)
		}
	}
}

// TestCrossBusTrustVerifiesAttestedEnvelope is RELAY-17's focused proof. It
// exercises the production *PeerStore implementation, not the package-wide
// test fake used by older relay tests.
func TestCrossBusTrustVerifiesAttestedEnvelope(t *testing.T) {
	originBus := peerBus
	originPins := testKeys.busPins(originBus)
	req := relayFixture()

	t.Run("A to B to C preserves and verifies the origin artefacts", func(t *testing.T) {
		atB, closeB := relay17OpenStore(t, localBus)
		defer closeB()
		if _, err := atB.PutTrust(BusTrust{BusID: originBus, SigningKeys: originPins}); err != nil {
			t.Fatalf("pinning A at B: %v", err)
		}
		mB, err := ValidateRelayRequest(localBus, req.MessageID, req, atB)
		if err != nil {
			t.Fatalf("B validating A's envelope: %v", err)
		}
		forwarded, err := mB.Forward(localBus)
		if err != nil {
			t.Fatalf("B forwarding A's envelope: %v", err)
		}

		atC, closeC := relay17OpenStore(t, thirdBus)
		defer closeC()
		if _, err := atC.PutTrust(BusTrust{BusID: originBus, SigningKeys: originPins}); err != nil {
			t.Fatalf("pinning non-adjacent A at C: %v", err)
		}
		mC, err := ValidateRelayRequest(thirdBus, forwarded.MessageID, forwarded, atC)
		if err != nil {
			t.Fatalf("C validating A's envelope carried by B: %v", err)
		}
		if _, routed := atC.Lookup(originBus); routed {
			t.Fatal("pinning non-adjacent A at C created a route to A")
		}
		if !bytes.Equal(mC.Signature, req.Signature) ||
			!bytes.Equal(mC.OriginAttestation.Signature, req.OriginAttestation.Signature) ||
			!bytes.Equal(mC.OriginAttestation.MessagingPublicKey, req.OriginAttestation.MessagingPublicKey) {
			t.Fatal("A's message signature or origin attestation changed on A -> B -> C")
		}
	})

	t.Run("rollover accepts either configured origin pin", func(t *testing.T) {
		st, closeStore := relay17OpenStore(t, thirdBus)
		defer closeStore()
		otherPub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("generating rollover key: %v", err)
		}
		if _, err := st.PutTrust(BusTrust{BusID: originBus, SigningKeys: []ed25519.PublicKey{otherPub, originPins[0]}}); err != nil {
			t.Fatalf("pinning rollover keys: %v", err)
		}
		if _, err := ValidateRelayRequest(thirdBus, req.MessageID, req, st); err != nil {
			t.Fatalf("the second rollover pin was not tried: %v", err)
		}
	})

	t.Run("pins are selected only by origin bus", func(t *testing.T) {
		st, closeStore := relay17OpenStore(t, thirdBus)
		defer closeStore()
		intermediatePins := testKeys.busPins(localBus)
		if _, err := st.PutTrust(BusTrust{BusID: originBus, SigningKeys: intermediatePins}); err != nil {
			t.Fatalf("pinning wrong A record: %v", err)
		}
		if _, err := st.PutTrust(BusTrust{BusID: localBus, SigningKeys: originPins}); err != nil {
			t.Fatalf("putting A's key under B's record: %v", err)
		}
		_, err := ValidateRelayRequest(thirdBus, req.MessageID, req, st)
		if !errors.Is(err, ErrNoSignerKey) || !errors.Is(err, attest.ErrVerify) {
			t.Fatalf("error = %v, want origin-specific attestation refusal; a key under another bus must never be searched", err)
		}
	})

	t.Run("unpeered no-Dir and typed-nil stores fail closed", func(t *testing.T) {
		unpeered, closeStore := relay17OpenStore(t, thirdBus)
		defer closeStore()
		if _, err := ValidateRelayRequest(thirdBus, req.MessageID, req, unpeered); !errors.Is(err, ErrUnpeeredBus) {
			t.Fatalf("unpeered error = %v, want ErrUnpeeredBus", err)
		}

		noDir, _ := psStore(t, func(o *PeerStoreOptions) { o.BusID = thirdBus })
		if err := noDir.Apply(psCommitted(t, psTrust(originBus, 1, relay17Now, originPins...), 1)); err != nil {
			t.Fatalf("applying active pin to no-Dir store: %v", err)
		}
		if len(noDir.PinnedKeys(originBus)) == 0 {
			t.Fatal("test premise failed: raw no-Dir PinnedKeys did not expose the replayed active pin")
		}
		if _, err := ValidateRelayRequest(thirdBus, req.MessageID, req, noDir); !errors.Is(err, ErrUnpeeredBus) {
			t.Fatalf("no-Dir verification error = %v, want ErrUnpeeredBus", err)
		}

		var nilStore *PeerStore
		var typedNil CrossBusTrust = nilStore
		if _, err := ValidateRelayRequest(thirdBus, req.MessageID, req, typedNil); !errors.Is(err, ErrUnpeeredBus) {
			t.Fatalf("typed-nil verification error = %v, want ErrUnpeeredBus without a panic", err)
		}
	})

	t.Run("foreign namespace and nearest-peer re-attestation are refused", func(t *testing.T) {
		st, closeStore := relay17OpenStore(t, thirdBus)
		defer closeStore()
		if _, err := st.PutTrust(BusTrust{BusID: originBus, SigningKeys: originPins}); err != nil {
			t.Fatalf("pinning A: %v", err)
		}
		if _, err := st.PutTrust(BusTrust{BusID: localBus, SigningKeys: testKeys.busPins(localBus)}); err != nil {
			t.Fatalf("pinning B: %v", err)
		}

		foreign := req
		foreign.Sender = localBus + ".mallory-1"
		foreign.OriginAttestation = cloneAttestation(testKeys.agent(foreign.Sender).attestation)
		canonical, err := attest.Canonicalize(foreign.OriginAttestation)
		if err != nil {
			t.Fatalf("canonicalizing foreign attestation: %v", err)
		}
		foreign.OriginAttestation.Signature = ed25519.Sign(testKeys.busSigningKey(originBus), canonical)
		m := RelayedMessage{
			OriginBus: originBus, Sender: foreign.Sender,
			OriginAttestation: foreign.OriginAttestation,
			Signature:         make([]byte, ed25519.SignatureSize),
		}
		if err := VerifyRelayed(m, st); !errors.Is(err, ErrInvalidRelay) {
			t.Fatalf("foreign-namespace standalone verification error = %v, want ErrInvalidRelay", err)
		}

		reAttested := req
		covered, err := attest.Canonicalize(reAttested.OriginAttestation)
		if err != nil {
			t.Fatalf("canonicalizing A's attestation: %v", err)
		}
		reAttested.OriginAttestation.Signature = ed25519.Sign(testKeys.busSigningKey(localBus), covered)
		_, err = ValidateRelayRequest(thirdBus, reAttested.MessageID, reAttested, st)
		if !errors.Is(err, ErrNoSignerKey) || !errors.Is(err, attest.ErrVerify) {
			t.Fatalf("nearest-peer re-attestation error = %v, want ErrNoSignerKey plus attest.ErrVerify", err)
		}
	})

	t.Run("rejection has no delivery trust or route side effects", func(t *testing.T) {
		st, closeStore := relay17OpenStore(t, thirdBus)
		defer closeStore()
		if _, err := st.PutTrust(BusTrust{BusID: originBus, SigningKeys: originPins}); err != nil {
			t.Fatalf("pinning A: %v", err)
		}
		beforeTrust := st.TrustedBuses()
		beforeRoutes := st.ActivePeers()
		bad := req
		bad.OriginAttestation = cloneAttestation(testKeys.agent(originBus + ".other-1").attestation)

		remote := newRelayResponder(t, thirdBus, func(c *RelayConfig) { c.Trust = st })
		status, code, _ := remote.postRelay(t, bad)
		if status != http.StatusForbidden || code != CodeBadSignature {
			t.Fatalf("mismatched attestation gave %d/%q, want %d/%q", status, code, http.StatusForbidden, CodeBadSignature)
		}
		if len(remote.acceptedMessages()) != 0 {
			t.Fatal("mismatched attestation reached the durable/delivery callback")
		}
		if !reflect.DeepEqual(st.TrustedBuses(), beforeTrust) || !reflect.DeepEqual(st.ActivePeers(), beforeRoutes) {
			t.Fatal("rejected attestation changed the peer store's trust or route tables")
		}
	})

	t.Run("attestation substitution does not change the relay fingerprint", func(t *testing.T) {
		st, closeStore := relay17OpenStore(t, thirdBus)
		defer closeStore()
		if _, err := st.PutTrust(BusTrust{BusID: originBus, SigningKeys: originPins}); err != nil {
			t.Fatalf("pinning A: %v", err)
		}
		one, err := ValidateRelayRequest(thirdBus, req.MessageID, req, st)
		if err != nil {
			t.Fatalf("validating first attestation: %v", err)
		}
		a := testKeys.agent(req.Sender)
		fresh, err := attest.Sign(testKeys.busSigningKey(originBus), originBus, req.Sender, a.pub, 2,
			relay17Now.Add(-time.Minute), time.Date(9998, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("minting a second valid attestation: %v", err)
		}
		twoReq := req
		twoReq.OriginAttestation = fresh
		two, err := ValidateRelayRequest(thirdBus, twoReq.MessageID, twoReq, st)
		if err != nil {
			t.Fatalf("validating second attestation: %v", err)
		}
		if one.Fingerprint != two.Fingerprint {
			t.Fatal("two valid attestations for identical signed content produced different idempotency fingerprints")
		}
	})
}

func TestRelay17AttestationWireAndShape(t *testing.T) {
	t.Run("nested JSON keys are stable snake_case and strict", func(t *testing.T) {
		req := relayFixture()
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshalling relay envelope: %v", err)
		}
		var outer map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &outer); err != nil {
			t.Fatalf("decoding outer JSON: %v", err)
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(outer["origin_attestation"], &nested); err != nil {
			t.Fatalf("decoding origin_attestation JSON: %v", err)
		}
		got := make([]string, 0, len(nested))
		for k := range nested {
			got = append(got, k)
		}
		sort.Strings(got)
		want := []string{"agent_id", "issued_at_unix_ms", "key_epoch", "messaging_public_key", "not_after_unix_ms", "signature"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("origin_attestation keys = %v, want %v", got, want)
		}

		withUnknown := bytes.Replace(encoded, []byte(`"agent_id":`), []byte(`"unknown":true,"agent_id":`), 1)
		remote := newRelayResponder(t, localBus, nil)
		status, code, _ := remote.postRelayRaw(t, "application/json", req.MessageID, withUnknown)
		if status != http.StatusBadRequest || code != CodeInvalidRequest {
			t.Fatalf("unknown nested attestation field gave %d/%q, want %d/%q", status, code, http.StatusBadRequest, CodeInvalidRequest)
		}
		if len(remote.acceptedMessages()) != 0 {
			t.Fatal("unknown nested attestation field reached delivery")
		}
	})

	t.Run("missing and malformed attestations stop before delivery", func(t *testing.T) {
		cases := []struct {
			name string
			mut  func(*attest.Attestation)
		}{
			{"zero", func(a *attest.Attestation) { *a = attest.Attestation{} }},
			{"short key", func(a *attest.Attestation) { a.MessagingPublicKey = []byte("short") }},
			{"short signature", func(a *attest.Attestation) { a.Signature = []byte("short") }},
			{"unset expiry", func(a *attest.Attestation) { a.NotAfterUnixMilli = 0 }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := relayFixture()
				tc.mut(&req.OriginAttestation)
				m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
				if !errors.Is(err, ErrMissingAttestation) || ErrorCode(err) != CodeInvalidRelay {
					t.Fatalf("error = %v/code %q, want ErrMissingAttestation/%q", err, ErrorCode(err), CodeInvalidRelay)
				}
				if !reflect.DeepEqual(m, RelayedMessage{}) {
					t.Fatalf("malformed attestation returned a usable message: %+v", m)
				}
			})
		}

		good := relayFixture()
		m := RelayedMessage{
			OriginBus: good.OriginBus, Sender: good.Sender,
			Signature: make([]byte, ed25519.SignatureSize),
		}
		if err := VerifyRelayed(m, fakeCrossBusTrustForTest); !errors.Is(err, ErrMissingAttestation) {
			t.Fatalf("standalone zero-attestation error = %v, want ErrMissingAttestation", err)
		}
	})
}
