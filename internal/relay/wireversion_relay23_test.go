package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestRelayEnvelopeCarriesDistinctWireVersionKey is RELAY-23's proof: the relay
// envelope carries a wire-protocol version under the key "protocol_version",
// value 1, and that key is DISTINCT from "version" (the roster epoch on
// RosterUpdateResponse). Two meanings on one key is how a peer applies a roster
// epoch as a format number, so the two must never collide on the wire.
func TestRelayEnvelopeCarriesDistinctWireVersionKey(t *testing.T) {
	// The value on the wire is the reserved relay-wire-version = 1, spelled as the
	// constant so a future bump moves this in lockstep with the resolver.
	if WireVersion != 1 {
		t.Fatalf("WireVersion = %d, want 1 (the reserved relay-wire-version); a change here needs a fresh reservation, not a code edit", WireVersion)
	}

	req := relayFixture(func(r *RelayRequest) { r.ProtocolVersion = WireVersion })
	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	// Inspect the RAW keys, not a re-decode into RelayRequest: the point is what
	// spelling actually travels, and a re-decode would hide a wrong key behind
	// Go's field mapping.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf, &raw); err != nil {
		t.Fatalf("unmarshalling to raw map: %v", err)
	}

	pv, ok := raw["protocol_version"]
	if !ok {
		t.Fatalf("the relay envelope put NO protocol_version key on the wire (body %s); every envelope this bus emits must declare the version it speaks", buf)
	}
	if string(pv) != "1" {
		t.Errorf("protocol_version = %s on the wire, want 1", pv)
	}

	// THE KEY MUST NOT BE "version". RosterUpdateResponse.Version owns "version"
	// as a roster EPOCH, a different quantity entirely; if the relay envelope used
	// it too, a peer could read a format number as an epoch or the reverse.
	if _, collides := raw["version"]; collides {
		t.Errorf("the relay envelope carries a bare \"version\" key (body %s); that spelling belongs to the roster epoch and must never name a format version", buf)
	}

	// Round-trips back to the same resolved value.
	var back RelayRequest
	if err := json.Unmarshal(buf, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.ProtocolVersion != 1 {
		t.Errorf("round-tripped ProtocolVersion = %d, want 1", back.ProtocolVersion)
	}

	// An UNSET version is OMITTED, not encoded as 0: 0 is not a version anyone may
	// transmit, so its only meaning is "written before the field existed", which
	// an explicit 0 on the wire would destroy.
	zeroBuf, err := json.Marshal(relayFixture())
	if err != nil {
		t.Fatalf("marshalling versionless fixture: %v", err)
	}
	var zeroRaw map[string]json.RawMessage
	if err := json.Unmarshal(zeroBuf, &zeroRaw); err != nil {
		t.Fatalf("unmarshalling versionless fixture: %v", err)
	}
	if _, present := zeroRaw["protocol_version"]; present {
		t.Errorf("a versionless envelope encoded a protocol_version key; an unset version must be omitted entirely: %s", zeroBuf)
	}
}

// TestWireVersionResolveRelayEnvelope pins resolveWireVersion: absent reads as 1,
// 1 reads as 1, and anything else is REFUSED with ErrUnsupportedRelayVersion and
// never defaulted (invariant 10). Named to match RELAY-53's TestWireVersion
// proof regex.
func TestWireVersionResolveRelayEnvelope(t *testing.T) {
	t.Run("absent and 1 resolve to 1", func(t *testing.T) {
		for _, declared := range []int{relayWireVersionAbsent, 1} {
			got, err := resolveWireVersion(declared)
			if err != nil {
				t.Errorf("resolveWireVersion(%d) errored: %v", declared, err)
			}
			if got != 1 {
				t.Errorf("resolveWireVersion(%d) = %d, want 1", declared, got)
			}
		}
	})

	t.Run("an unrecognised version is refused, not defaulted", func(t *testing.T) {
		for _, declared := range []int{2, 42, -1} {
			got, err := resolveWireVersion(declared)
			if !errors.Is(err, ErrUnsupportedRelayVersion) {
				t.Errorf("resolveWireVersion(%d) error = %v, want one wrapping ErrUnsupportedRelayVersion", declared, err)
			}
			// The returned int is 0, never a silent fall-back to 1: a caller that
			// ignored the error must not find a plausible version waiting.
			if got != 0 {
				t.Errorf("resolveWireVersion(%d) returned version %d alongside its error; an unrecognised version must resolve to 0, never a defaulted 1", declared, got)
			}
		}
	})
}

// TestWireVersionResolversCoexistIndependently is RELAY-53's proof: the relay
// envelope's resolveWireVersion and the ACK frame's resolveAckWireVersion live in
// ONE package under DISTINCT names, with DISTINCT sentinels and DISTINCT wire
// codes, and each refuses an unknown version through its OWN sentinel/code — a
// guard proven on one frame says nothing about the other. This is the deliberate
// coexistence (two codes let a peer read WHICH frame it could not parse), not the
// collapse the task originally sketched.
func TestWireVersionResolversCoexistIndependently(t *testing.T) {
	// Both accept absent/1 -> 1, independently.
	for _, tc := range []struct {
		name    string
		resolve func(int) (int, error)
	}{
		{"relay", resolveWireVersion},
		{"ack", resolveAckWireVersion},
	} {
		for _, declared := range []int{0, 1} {
			got, err := tc.resolve(declared)
			if err != nil || got != 1 {
				t.Errorf("%s resolver(%d) = %d, %v; want 1, nil", tc.name, declared, got, err)
			}
		}
	}

	// An unknown version on the RELAY frame blames the RELAY sentinel and NOT the
	// ACK one; on the ACK frame, the reverse. Conflating them would misdiagnose a
	// partial rollout of one surface as the other.
	_, relayErr := resolveWireVersion(2)
	if !errors.Is(relayErr, ErrUnsupportedRelayVersion) {
		t.Errorf("resolveWireVersion(2) = %v, want ErrUnsupportedRelayVersion", relayErr)
	}
	if errors.Is(relayErr, ErrUnsupportedAckVersion) {
		t.Errorf("resolveWireVersion(2) wrapped ErrUnsupportedAckVersion; the two frames must fail through distinct sentinels")
	}

	_, ackErr := resolveAckWireVersion(2)
	if !errors.Is(ackErr, ErrUnsupportedAckVersion) {
		t.Errorf("resolveAckWireVersion(2) = %v, want ErrUnsupportedAckVersion", ackErr)
	}
	if errors.Is(ackErr, ErrUnsupportedRelayVersion) {
		t.Errorf("resolveAckWireVersion(2) wrapped ErrUnsupportedRelayVersion; the two frames must fail through distinct sentinels")
	}

	// The two sentinels are distinct values.
	if errors.Is(ErrUnsupportedRelayVersion, ErrUnsupportedAckVersion) || errors.Is(ErrUnsupportedAckVersion, ErrUnsupportedRelayVersion) {
		t.Errorf("ErrUnsupportedRelayVersion and ErrUnsupportedAckVersion are not distinct sentinels")
	}

	// The two wire codes are distinct strings, and ErrorCode maps each sentinel to
	// its own — so a peer operator reads WHICH frame the far end could not parse.
	if CodeUnsupportedRelayVersion == CodeUnsupportedAckVersion {
		t.Errorf("CodeUnsupportedRelayVersion == CodeUnsupportedAckVersion (%q); the two frames must carry distinct codes", CodeUnsupportedRelayVersion)
	}
	if got := ErrorCode(ErrUnsupportedRelayVersion); got != CodeUnsupportedRelayVersion {
		t.Errorf("ErrorCode(ErrUnsupportedRelayVersion) = %q, want %q", got, CodeUnsupportedRelayVersion)
	}
	if got := ErrorCode(ErrUnsupportedAckVersion); got != CodeUnsupportedAckVersion {
		t.Errorf("ErrorCode(ErrUnsupportedAckVersion) = %q, want %q", got, CodeUnsupportedAckVersion)
	}
}

// TestWireVersionAbsentReadsAsOneThroughValidate proves the backward-compatible
// read end to end: a versionless envelope (every fixture, and every legacy peer)
// validates and resolves to version 1, and an explicit 1 does the same. Named to
// match RELAY-53's TestWireVersion regex.
func TestWireVersionAbsentReadsAsOneThroughValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*RelayRequest)
	}{
		{"absent", nil},
		{"explicit 1", func(r *RelayRequest) { r.ProtocolVersion = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mods []func(*RelayRequest)
			if tc.mod != nil {
				mods = append(mods, tc.mod)
			}
			req := relayFixture(mods...)
			m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
			if err != nil {
				t.Fatalf("ValidateRelayRequest: %v", err)
			}
			if m.ProtocolVersion != 1 {
				t.Errorf("resolved ProtocolVersion = %d, want 1", m.ProtocolVersion)
			}
		})
	}
}

// TestWireVersionUnrecognisedRefusedAtIngest proves an unknown relay wire version
// is refused at ingest — through ValidateRelayRequest AND over HTTP — with
// CodeUnsupportedRelayVersion and a 400, never defaulted to 1, never a 503, and
// WITHOUT disconnecting: the response is an ordinary JSON refusal the peer reads
// back on the same connection (invariant 10). Named to match RELAY-53's
// TestWireVersion regex.
func TestWireVersionUnrecognisedRefusedAtIngest(t *testing.T) {
	t.Run("ValidateRelayRequest refuses it and returns the zero message", func(t *testing.T) {
		req := relayFixture(func(r *RelayRequest) { r.ProtocolVersion = 2 })
		m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
		if !errors.Is(err, ErrUnsupportedRelayVersion) {
			t.Fatalf("ValidateRelayRequest error = %v, want one wrapping ErrUnsupportedRelayVersion", err)
		}
		// FAIL CLOSED: no partially-validated message escapes, and in particular
		// the version did not silently default to 1.
		if m.ProtocolVersion != 0 {
			t.Errorf("a refused envelope produced a RelayedMessage with ProtocolVersion %d; it must be the zero value, not a defaulted 1", m.ProtocolVersion)
		}
	})

	t.Run("HTTP answers 400 unsupported_relay_version and does not disconnect", func(t *testing.T) {
		remote := newRelayResponder(t, localBus, nil)
		req := relayFixture(func(r *RelayRequest) { r.ProtocolVersion = 2 })
		// A successful read of the status and error body is itself the proof the
		// connection was NOT dropped — doRelay reads a full JSON error body back.
		status, code, _ := remote.postRelay(t, req)
		if status != http.StatusBadRequest {
			t.Errorf("status = %d, want %d (a version we do not speak is a 400, never a 503: no retry installs a new binary)", status, http.StatusBadRequest)
		}
		if code != CodeUnsupportedRelayVersion {
			t.Errorf("code = %q, want %q", code, CodeUnsupportedRelayVersion)
		}
		// Nothing was accepted: an unreadable format reaches no AcceptRelay.
		if got := remote.acceptedMessages(); len(got) != 0 {
			t.Errorf("AcceptRelay was called %d times for a refused version; it must be called 0 times", len(got))
		}
	})
}

// TestWireVersionForwardStampsWireVersion proves egress is server-authoritative:
// Forward stamps THIS bus's WireVersion on the outbound envelope rather than
// echoing whatever version the ingested message resolved to (invariant 1). Named
// to match RELAY-53's TestWireVersion regex.
func TestWireVersionForwardStampsWireVersion(t *testing.T) {
	req := relayFixture()
	m, err := ValidateRelayRequest(localBus, req.MessageID, req, fakeCrossBusTrustForTest)
	if err != nil {
		t.Fatalf("ValidateRelayRequest: %v", err)
	}
	// Even if the ingested record somehow carried a different resolved version,
	// Forward must stamp the constant this binary speaks.
	m.ProtocolVersion = 99
	out, err := m.Forward(localBus)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if out.ProtocolVersion != WireVersion {
		t.Errorf("Forward stamped ProtocolVersion %d, want WireVersion %d; egress speaks this binary's version, never a peer's", out.ProtocolVersion, WireVersion)
	}
}
