package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ids"
)

func TestValidateRoster(t *testing.T) {
	cases := []struct {
		name    string
		peerBus string
		agents  []string
		want    error
	}{
		{name: "empty roster", peerBus: peerBus},
		{name: "one agent", peerBus: peerBus, agents: []string{peerBus + ".beta-1"}},
		{name: "names may contain hyphens", peerBus: peerBus, agents: []string{peerBus + ".code-reviewer-3"}},
		{name: "our bus id", peerBus: localBus, want: ErrBusIDCollision},
		{name: "our bus id, other case", peerBus: strings.ToUpper(localBus), want: ErrBusIDCollision},
		{name: "agent in our namespace", peerBus: peerBus, agents: []string{localBus + ".alpha-1"}, want: ErrBusIDCollision},
		{name: "agent of a third bus", peerBus: peerBus, agents: []string{thirdBus + ".delta-1"}, want: ErrInvalidRoster},
		{name: "unqualified name", peerBus: peerBus, agents: []string{"beta-1"}, want: ErrInvalidRoster},
		{name: "no minted suffix", peerBus: peerBus, agents: []string{peerBus + ".beta"}, want: ErrInvalidRoster},
		{name: "suffix zero", peerBus: peerBus, agents: []string{peerBus + ".beta-0"}, want: ErrInvalidRoster},
		{name: "leading-zero suffix", peerBus: peerBus, agents: []string{peerBus + ".beta-01"}, want: ErrInvalidRoster},
		{name: "uppercase name", peerBus: peerBus, agents: []string{peerBus + ".Beta-1"}, want: ErrInvalidRoster},
		{name: "duplicate", peerBus: peerBus, agents: []string{peerBus + ".beta-1", peerBus + ".beta-1"}, want: ErrInvalidRoster},
		{name: "oversized id", peerBus: peerBus, agents: []string{peerBus + "." + strings.Repeat("a", ids.MaxAgentIDLen) + "-1"}, want: ErrInvalidRoster},
		{name: "empty bus id", peerBus: "", want: ErrInvalidBusID},
		{name: "bus id with a dot", peerBus: "bus.x", want: ErrInvalidBusID},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateRoster(localBus, tc.peerBus, tc.agents)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("ValidateRoster error = %v, want one wrapping %v", err, tc.want)
				}
				if got != nil {
					t.Errorf("ValidateRoster returned %v alongside an error, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRoster: %v", err)
			}
			if len(got) != len(tc.agents) {
				t.Fatalf("ValidateRoster returned %d ids, want %d", len(got), len(tc.agents))
			}
		})
	}
}

// TestValidateRosterCopiesTheInput proves the validated slice does not alias
// the decoded payload: a caller that keeps a PeerRoster must not have its view
// of a peer rewritten by whoever still holds the input slice.
func TestValidateRosterCopiesTheInput(t *testing.T) {
	input := []string{peerBus + ".beta-1"}
	got, err := ValidateRoster(localBus, peerBus, input)
	if err != nil {
		t.Fatalf("ValidateRoster: %v", err)
	}
	input[0] = localBus + ".alpha-1"
	if got[0] != peerBus+".beta-1" {
		t.Fatalf("validated roster changed to %q when the input slice was mutated; it aliases the caller's memory", got[0])
	}
}

func TestValidateRosterRefusesOversizedRosterBeforeParsing(t *testing.T) {
	// Every entry is individually malformed. If the length check did not run
	// first, the failure would be ErrInvalidRoster from the parser; the point
	// of this test is that we never got that far.
	agents := make([]string, MaxRosterAgents+1)
	for i := range agents {
		agents[i] = "not-an-agent-id"
	}
	_, err := ValidateRoster(localBus, peerBus, agents)
	if !errors.Is(err, ErrRosterTooLarge) {
		t.Fatalf("error = %v, want one wrapping ErrRosterTooLarge (the length must be checked before any entry is parsed)", err)
	}

	atCap := make([]string, MaxRosterAgents)
	for i := range atCap {
		atCap[i] = fmt.Sprintf("%s.a%d-1", peerBus, i)
	}
	if _, err := ValidateRoster(localBus, peerBus, atCap); err != nil {
		t.Fatalf("a roster of exactly MaxRosterAgents was refused: %v", err)
	}
}

// TestMaxHandshakeBytesFitsAMaximumRoster pins the derivation in peer.go: the
// byte cap must never be the thing that rejects a roster the roster cap allows,
// or the two limits would contradict each other and the failure would look like
// a peer bug.
func TestMaxHandshakeBytesFitsAMaximumRoster(t *testing.T) {
	agents := make([]string, MaxRosterAgents)
	longName := strings.Repeat("a", 63) // 1 + 63 = the longest name ids allows
	for i := range agents {
		agents[i] = fmt.Sprintf("%s.b%s-%d", peerBus, longName, i+1)
	}
	for _, id := range agents[:1] {
		if _, _, _, err := ids.ParseAgentID(id); err != nil {
			t.Fatalf("test built an invalid id %q: %v", id, err)
		}
	}
	body, err := json.Marshal(PeerEnrollRequest{
		BusID:          peerBus,
		IdempotencyKey: strings.Repeat("k", MaxIdempotencyKeyLen),
		Agents:         agents,
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if len(body) > MaxHandshakeBytes {
		t.Fatalf("a maximum-size roster encodes to %d bytes, over the %d byte cap; the caps contradict each other", len(body), MaxHandshakeBytes)
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{name: "typical", key: "relay-1.handshake_2", ok: true},
		{name: "at the cap", key: strings.Repeat("k", MaxIdempotencyKeyLen), ok: true},
		{name: "empty", key: ""},
		{name: "over the cap", key: strings.Repeat("k", MaxIdempotencyKeyLen+1)},
		{name: "space", key: "a b"},
		{name: "newline", key: "a\nb"},
		{name: "slash", key: "a/b"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIdempotencyKey(tc.key)
			if tc.ok && err != nil {
				t.Fatalf("ValidateIdempotencyKey(%q) = %v, want nil", tc.key, err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidIdempotencyKey) {
				t.Fatalf("ValidateIdempotencyKey(%q) = %v, want one wrapping ErrInvalidIdempotencyKey", tc.key, err)
			}
		})
	}
}

func TestPeerEnrollURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{name: "origin", base: "https://peer.example:8443", want: "https://peer.example:8443" + PeerEnrollPath},
		{name: "trailing slash", base: "https://peer.example/", want: "https://peer.example" + PeerEnrollPath},
		{name: "uppercase scheme", base: "HTTPS://peer.example", want: "https://peer.example" + PeerEnrollPath},
		{name: "plaintext", base: "http://peer.example"},
		{name: "no scheme", base: "peer.example:8443"},
		{name: "no host", base: "https://"},
		{name: "query", base: "https://peer.example?a=1"},
		{name: "fragment", base: "https://peer.example#f"},
		{name: "userinfo", base: "https://user:pw@peer.example"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := peerEnrollURL(tc.base)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("peerEnrollURL(%q) = %q, want an error", tc.base, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("peerEnrollURL(%q): %v", tc.base, err)
			}
			if got != tc.want {
				t.Fatalf("peerEnrollURL(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

func TestErrorCodeIsStable(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("wrapped: %w", ErrPayloadTooLarge), CodePayloadTooLarge},
		{fmt.Errorf("wrapped: %w", ErrBusIDCollision), CodeBusIDCollision},
		{fmt.Errorf("wrapped: %w", ErrInvalidBusID), CodeInvalidBusID},
		{fmt.Errorf("wrapped: %w", ErrRosterTooLarge), CodeRosterTooLarge},
		{fmt.Errorf("wrapped: %w", ErrInvalidRoster), CodeInvalidRoster},
		{fmt.Errorf("wrapped: %w", ErrInvalidIdempotencyKey), CodeInvalidIdempotencyKey},
		{fmt.Errorf("wrapped: %w", ErrInvalidRequest), CodeInvalidRequest},
		{errors.New("something else"), CodeInternal},
	}
	for _, tc := range cases {
		if got := ErrorCode(tc.err); got != tc.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestErrorBodiesNeverEchoPeerInput pins the responder's posture: the code goes
// on the wire, the detail stays in our log. An error body that quoted the
// peer's own bytes back would describe our parser to a stranger.
func TestErrorBodiesNeverEchoPeerInput(t *testing.T) {
	marker := "canary-namespace-marker"
	remote := newResponder(t, localBus, nil, nil)
	// An uppercase agent name is rejected by ids.ValidateAgentName, and the
	// resulting error quotes the offending id — so the marker IS in the error
	// we log, and the question is only whether it reaches the wire.
	req := PeerEnrollRequest{BusID: marker + "-bus", IdempotencyKey: "k", Agents: []string{marker + "-bus.Bad-1"}}

	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	resp, err := remote.srv.Client().Post(remote.srv.URL+PeerEnrollPath, "application/json", strings.NewReader(string(buf)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400; this test only means something on a rejection", resp.StatusCode)
	}
	var body [4096]byte
	n, _ := resp.Body.Read(body[:])
	if strings.Contains(string(body[:n]), marker) {
		t.Fatalf("error body %q echoes peer-supplied input", string(body[:n]))
	}
	if !strings.Contains(string(body[:n]), CodeInvalidRoster) {
		t.Fatalf("error body %q does not carry the stable %q code", string(body[:n]), CodeInvalidRoster)
	}
}
