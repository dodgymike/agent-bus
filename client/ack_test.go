package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ackKATSeedHex is RFC 8032 §7.1 TEST 1's seed — the key
// internal/signing/testdata/ack_vectors.json publishes its vectors under, and
// the same key the message vectors use, so the key derivation itself is
// checkable against a published vector. It is a TEST key and must never be used
// by a real agent.
const ackKATSeedHex = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"

// ackKAT is one published vector, transcribed from
// internal/signing/testdata/ack_vectors.json.
//
// # WHY IT IS TRANSCRIBED RATHER THAN READ FROM THAT FILE
//
// This package must not depend on anything under internal/, not even a path
// (doc.go, invariant 7) — an agent embedding it gets this package and nothing
// else. So the vectors are copied here, exactly as the byte LAYOUT above is
// copied, and they fail closed the same way: if internal/signing/ack.go ever
// changes what it produces, cmd/agent-busctl's TestAckRecipientCLI verifies a
// live CLI signature with signing.VerifyAck and goes red, and this file goes red
// against the published artefact. Two independent detections, no import.
//
// REGENERATING THESE BYTES TO MAKE A TEST PASS IS A WIRE-FORMAT CHANGE, NOT A
// FIX: it needs a new format version reserved from the signing-format-version
// namespace and a new context string.
type ackKAT struct {
	name         string
	why          string
	ack          ackSigned
	canonicalHex string
	signatureHex string
}

func ackVectors() []ackKAT {
	return []ackKAT{
		{
			name: "delivered-cross-bus",
			why:  "positive ACK across a relay hop; the two bus halves DIFFER and that is legitimate (§6.2)",
			ack: ackSigned{
				CorrelationKey:     "bus-a-7",
				Recipient:          "bus-b.bob-2",
				Outcome:            AckDelivered,
				Class:              "",
				EmittedAtUnixMilli: 1755777600000,
			},
			canonicalHex: "000000196167656e742d6275732f726563697069656e742d61636b2f33000000076275732d612d370000000b6275732d622e626f622d320000000964656c6976657265640000000000000198cc800a00",
			signatureHex: "16f8a71273ade969ec35af122a4e80ec9e718c19ac3b49efeb652dcaeae88170ca24f99884d7e1ede6980c621306aa8d8ee75947cfc3159ca683d50cd65c2805",
		},
		{
			name: "refused-not-addressed",
			why:  "the third and last recipient-emitted class, and a NON-empty class field",
			ack: ackSigned{
				CorrelationKey:     "bus-a-1",
				Recipient:          "bus-a.bob-2",
				Outcome:            AckRefused,
				Class:              AckClassRecipientRefusedNotAddressed,
				EmittedAtUnixMilli: 2,
			},
			canonicalHex: "000000196167656e742d6275732f726563697069656e742d61636b2f33000000076275732d612d310000000b6275732d612e626f622d3200000007726566757365640000001f726563697069656e745f726566757365645f6e6f745f6164647265737365640000000000000002",
			signatureHex: "6ce3968f651e3c9346ce497881950550bb34d51916c7e80b860d6437724ecbb8c9c29aa299f42a59cdcb5cc26366f9a512c617213b3889ff56013535dbb40e0c",
		},
	}
}

// TestAckCanonicalMirrorMatchesPublishedVectors is the guard that this
// package's copy of the recipient-acknowledgement layout has not drifted from
// internal/signing's, byte for byte.
//
// It checks the CANONICAL BYTES and not merely "a signature verifies", because
// a self-consistent mirror verifies its own signatures perfectly while agreeing
// with nobody. The bytes are the interoperable artefact.
func TestAckCanonicalMirrorMatchesPublishedVectors(t *testing.T) {
	seed, err := hex.DecodeString(ackKATSeedHex)
	if err != nil {
		t.Fatalf("decoding the published seed: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)

	for _, v := range ackVectors() {
		v := v
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			got, err := canonicalizeAck(v.ack)
			if err != nil {
				t.Fatalf("canonicalizeAck(%s): %v — the published vector must canonicalize", v.name, err)
			}
			want, err := hex.DecodeString(v.canonicalHex)
			if err != nil {
				t.Fatalf("decoding the published canonical bytes: %v", err)
			}
			if hex.EncodeToString(got) != v.canonicalHex {
				t.Fatalf("canonical bytes DIVERGED from the published format (%s):\n got %s\nwant %s\nThis mirror and internal/signing must agree byte for byte; regenerating the vector is a wire-format change, not a fix.",
					v.why, hex.EncodeToString(got), v.canonicalHex)
			}

			sig, err := signAck(priv, v.ack)
			if err != nil {
				t.Fatalf("signAck: %v", err)
			}
			if hex.EncodeToString(sig) != v.signatureHex {
				t.Fatalf("signature diverged from the published vector:\n got %s\nwant %s", hex.EncodeToString(sig), v.signatureHex)
			}
			// Signed UNHASHED over exactly those bytes: an implementation that
			// pre-hashed would still "sign" and would fail here.
			if !ed25519.Verify(priv.Public().(ed25519.PublicKey), want, sig) {
				t.Fatal("the published signature does not verify over the published bytes under the published key")
			}
		})
	}
}

// TestAckContextIsTheVersionIndicator pins the domain separator and its version.
// Changing either is a new reserved format version, never an in-place edit.
func TestAckContextIsTheVersionIndicator(t *testing.T) {
	if AckSigningContext != "agent-bus/recipient-ack/3" {
		t.Errorf("AckSigningContext = %q; it is the domain separator internal/signing.AckContext publishes and it carries the format version", AckSigningContext)
	}
	if AckSigningFormatVersion != 3 {
		t.Errorf("AckSigningFormatVersion = %d, want the RESERVED 3 (1 = messages, 2 = attestations)", AckSigningFormatVersion)
	}
	if !strings.HasSuffix(AckSigningContext, "/3") {
		t.Error("the version inside the context string and AckSigningFormatVersion must not be able to disagree")
	}
	// The two languages one messaging key now signs must be distinguishable
	// from their first bytes onward.
	if AckSigningContext == MessageSigningContext {
		t.Fatal("the acknowledgement and message contexts are identical; one key signs both languages and they must be separated")
	}
}

// TestCanonicalizeAckRefusesWhatARecipientMayNotSay is the closed-vocabulary
// guard, in the layer that produces the bytes.
func TestCanonicalizeAckRefusesWhatARecipientMayNotSay(t *testing.T) {
	base := ackSigned{
		CorrelationKey:     "bus-a-7",
		Recipient:          "bus-b.bob-2",
		Outcome:            AckDelivered,
		EmittedAtUnixMilli: 1755777600000,
	}
	with := func(mutate func(a *ackSigned)) ackSigned {
		a := base
		mutate(&a)
		return a
	}
	cases := []struct {
		name string
		ack  ackSigned
		why  string
	}{
		{"undeliverable", with(func(a *ackSigned) { a.Outcome = AckUndeliverable }),
			"a recipient may not sign a bus's routing claim"},
		{"accepted is not terminal", with(func(a *ackSigned) { a.Outcome = AckAccepted }),
			"the two non-terminal states are a bus's own bookkeeping and must never travel"},
		{"unknown", with(func(a *ackSigned) { a.Outcome = AckUnknown }),
			"\"unknown\" is a REPORTING value; an \"I don't know\" on the wire could overwrite a real terminal"},
		{"empty outcome", with(func(a *ackSigned) { a.Outcome = "" }), "an outcome is never defaulted"},
		{"delivered with a class", with(func(a *ackSigned) { a.Class = AckClassRecipientRefusedPolicy }),
			"a positive acknowledgement has nothing to explain (§5.4)"},
		{"refused with no class", with(func(a *ackSigned) { a.Outcome = AckRefused }),
			"an unexplained refusal is not signable"},
		{"refused with a BUS class", with(func(a *ackSigned) { a.Outcome = AckRefused; a.Class = "no_route" }),
			"the nine bus-emitted classes are routing claims a recipient has no standing to sign"},
		{"refused with an invented class", with(func(a *ackSigned) { a.Outcome = AckRefused; a.Class = "recipient_refused_because_i_said_so" }),
			"the class set is closed; there is no free text anywhere on this surface"},
		{"correlation key is not a message id", with(func(a *ackSigned) { a.CorrelationKey = "7" }),
			"the correlation key is a server-minted message id, not any string"},
		{"correlation key is a bare sequence", with(func(a *ackSigned) { a.CorrelationKey = "bus-a-0" }),
			"sequence 0 is never allocated"},
		{"recipient is not fully qualified", with(func(a *ackSigned) { a.Recipient = "bob-2" }),
			"every agent id is <bus-id>.<agent-id> (invariant 2)"},
		{"emitted-at unset", with(func(a *ackSigned) { a.EmittedAtUnixMilli = 0 }), "0 means unset"},
		{"emitted-at negative", with(func(a *ackSigned) { a.EmittedAtUnixMilli = -1 }), "a clock reading is positive"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := canonicalizeAck(tc.ack)
			if err == nil {
				t.Fatalf("canonicalizeAck produced %d bytes for %q — %s", len(b), tc.name, tc.why)
			}
			if !errors.Is(err, errNotCanonicalAck) {
				t.Errorf("err = %v, want it wrapped in errNotCanonicalAck so a caller can classify it", err)
			}
			if b != nil {
				t.Error("bytes were returned alongside the error; canonicalization must never produce partial or best-effort bytes")
			}
		})
	}
}

// TestCanonicalizeAckDoesNotBindTheBusHalves goes RED the day somebody
// "hardens" this by requiring the correlation key's bus to equal the
// recipient's. A cross-bus acknowledgement is the NORMAL relay case (§6.2): in
// A -> B the key is A's and the recipient is B's, and a bus-half check would
// break every multi-hop delivery while every same-bus test still passed.
func TestCanonicalizeAckDoesNotBindTheBusHalves(t *testing.T) {
	crossBus := ackSigned{
		CorrelationKey:     "bus-a-7",
		Recipient:          "bus-b.bob-2",
		Outcome:            AckDelivered,
		EmittedAtUnixMilli: 1755777600000,
	}
	if _, err := canonicalizeAck(crossBus); err != nil {
		t.Fatalf("a cross-bus acknowledgement was refused (%v). The recipient being on a DIFFERENT bus from the origin is what the relay plane is FOR; do not add a bus-half check here.", err)
	}
}

// TestCanonicalAckEncodingIsInjective checks the property the length prefixes
// exist for: two DIFFERENT acknowledgements never produce the same bytes, so
// nothing can be shifted across a field boundary to present a different
// statement under a signature that still verifies.
func TestCanonicalAckEncodingIsInjective(t *testing.T) {
	// The two below are chosen so a naive concatenation WITHOUT length prefixes
	// would produce identical bytes: the boundary between the correlation key
	// and the recipient is the only difference.
	a := ackSigned{CorrelationKey: "bus-a-7", Recipient: "bus-b.bob-2", Outcome: AckDelivered, EmittedAtUnixMilli: 5}
	b := ackSigned{CorrelationKey: "bus-a-77", Recipient: "bus-b.bob-2", Outcome: AckDelivered, EmittedAtUnixMilli: 5}
	ab, err := canonicalizeAck(a)
	if err != nil {
		t.Fatalf("canonicalizeAck(a): %v", err)
	}
	bb, err := canonicalizeAck(b)
	if err != nil {
		t.Fatalf("canonicalizeAck(b): %v", err)
	}
	if hex.EncodeToString(ab) == hex.EncodeToString(bb) {
		t.Fatal("two different acknowledgements canonicalized to the same bytes; the encoding is not injective")
	}
}

// TestSignAckRefusesAWrongSizeKey checks the order that matters:
// ed25519.Sign PANICS on a malformed private key, so the length is checked
// before it is reached.
func TestSignAckRefusesAWrongSizeKey(t *testing.T) {
	a := ackSigned{CorrelationKey: "bus-a-7", Recipient: "bus-b.bob-2", Outcome: AckDelivered, EmittedAtUnixMilli: 5}
	for _, n := range []int{0, 32, 63, 65} {
		sig, err := signAck(make(ed25519.PrivateKey, n), a)
		if err == nil {
			t.Fatalf("signAck accepted a %d-byte private key and produced %d bytes", n, len(sig))
		}
		if strings.Contains(err.Error(), string(make([]byte, n))) && n > 0 {
			t.Error("the error echoed key material; report the LENGTH, never the key")
		}
	}
}

// TestRecipientRefusalClassesAreClosedAndUnwritable pins the three-member set
// and that a caller cannot edit it through the returned slice.
func TestRecipientRefusalClassesAreClosedAndUnwritable(t *testing.T) {
	want := []string{"recipient_refused_policy", "recipient_refused_undecodable", "recipient_refused_not_addressed"}
	got := RecipientRefusalClasses()
	if len(got) != len(want) {
		t.Fatalf("RecipientRefusalClasses() = %v, want exactly the three a recipient may emit: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("class[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	got[0] = "no_route"
	if RecipientRefusalClasses()[0] != want[0] {
		t.Fatal("writing through the returned slice edited the closed set; return a fresh slice each call")
	}
}

// TestAckOptionValidationRefusesBeforeItSigns is the guard at the API layer an
// EMBEDDER reaches directly (invariant 7's third audience). The CLI cannot even
// spell some of these — it has no --outcome flag — so without this test those
// branches would be unreachable code that looks like a check.
//
// Every case must fail with NO network call: the client here has no usable
// identity at all, so anything that got as far as resolving a credential would
// report a config error instead of the usage error asserted.
func TestAckOptionValidationRefusesBeforeItSigns(t *testing.T) {
	c, err := New(Config{IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name  string
		opts  AckOptions
		wants string
	}{
		{"undeliverable is a routing claim", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckUndeliverable}, "routing claim"},
		{"accepted is not terminal", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckAccepted}, "not one a recipient may acknowledge with"},
		{"in_flight is not terminal", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckInFlight}, "not one a recipient may acknowledge with"},
		{"unknown never travels", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckUnknown}, "not one a recipient may acknowledge with"},
		{"an outcome is never defaulted", AckOptions{CorrelationKey: "bus-a-7"}, "required and is never defaulted"},
		{"delivered carries no class", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckDelivered, Class: AckClassRecipientRefusedPolicy}, "carries no class"},
		{"refused needs a class", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckRefused}, "requires a class"},
		{"refused rejects a BUS class", AckOptions{CorrelationKey: "bus-a-7", Outcome: AckRefused, Class: "no_route"}, "not one a recipient may emit"},
		{"no correlation key", AckOptions{Outcome: AckDelivered}, "correlation key is required"},
		{"whitespace in the key", AckOptions{CorrelationKey: "bus-a 7", Outcome: AckDelivered}, "no whitespace"},
		{"an oversized key", AckOptions{CorrelationKey: strings.Repeat("a", MaxCorrelationKeyLen+1), Outcome: AckDelivered}, "the limit is"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := c.Ack(nil, tc.opts)
			if err == nil {
				t.Fatalf("Ack accepted %+v and returned %+v", tc.opts, res)
			}
			if KindOf(err) != KindUsage {
				t.Fatalf("Kind = %q, want KindUsage (exit 2) — this is the caller's own input, judged locally: %v", KindOf(err), err)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not say %q, so a caller cannot tell this from a different refusal", err, tc.wants)
			}
		})
	}
}

// TestAckSignsWithTheMessagingKeyNotTheAuthKey drives Client.Ack against a stub
// bus and checks WHICH KEY it chose — not merely that two different keys do not
// verify each other, which is a tautology about Ed25519 and would stay green if
// Ack signed with the auth key. (An earlier version of this test was exactly
// that tautology; the security gate caught it, mutation would have too.)
//
// It is here as well as in cmd/agent-busctl because this package is importable
// on its own, so key selection must be guarded in the layer that makes the
// choice. And the mistake it catches is INVISIBLE ON THE WIRE: every bus checks
// the signature's SHAPE only, so an acknowledgement signed with the key that
// proves this agent to its BUS is accepted and recorded by every bus in the
// federation while being attributable to nobody.
func TestAckSignsWithTheMessagingKeyNotTheAuthKey(t *testing.T) {
	var frame struct {
		ProtocolVersion    int    `json:"protocol_version"`
		CorrelationKey     string `json:"correlation_key"`
		Recipient          string `json:"recipient"`
		Outcome            string `json:"outcome"`
		Class              string `json:"class"`
		EmittedAtUnixMilli int64  `json:"emitted_at"`
		Attestation        *struct {
			Signature []byte `json:"signature"`
		} `json:"attestation"`
	}
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routeAck || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &frame); err != nil {
			t.Errorf("the ACK frame is not JSON: %v", err)
		}
		stubWriteJSON(w, http.StatusOK, AckResult{Accepted: true, State: AckDelivered})
	})
	c := bus.client(t, nil)

	res, err := c.Ack(context.Background(), AckOptions{CorrelationKey: "bus-y-7", Outcome: AckDelivered})
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if !res.Accepted || res.State != AckDelivered {
		t.Fatalf("result = %+v, want accepted with state delivered", res)
	}
	if frame.Attestation == nil || len(frame.Attestation.Signature) != SignatureSize {
		t.Fatalf("attestation = %+v, want exactly %d signature bytes", frame.Attestation, SignatureSize)
	}
	if frame.Recipient != bus.AgentID {
		t.Fatalf("recipient = %q, want this agent's OWN id %q — a recipient speaks only for itself", frame.Recipient, bus.AgentID)
	}

	// The two keys as the STORE holds them, after Ack minted the messaging one.
	s, err := OpenStore(bus.Dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, err := s.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	encMsg, err := cred.MessagingPublicKey()
	if err != nil {
		t.Fatalf("Ack did not mint a messaging key: %v", err)
	}
	msgPub, err := base64.StdEncoding.DecodeString(encMsg)
	if err != nil {
		t.Fatalf("messaging public key is not base64: %v", err)
	}
	authPub, err := base64.StdEncoding.DecodeString(cred.Identity.PublicKey)
	if err != nil {
		t.Fatalf("auth public key is not base64: %v", err)
	}
	if string(msgPub) == string(authPub) {
		t.Fatal("the messaging key and the auth key are the SAME key in this fixture; this test could not tell them apart and would pass either way")
	}

	canonical, err := canonicalizeAck(ackSigned{
		CorrelationKey:     frame.CorrelationKey,
		Recipient:          frame.Recipient,
		Outcome:            frame.Outcome,
		Class:              frame.Class,
		EmittedAtUnixMilli: frame.EmittedAtUnixMilli,
	})
	if err != nil {
		t.Fatalf("the frame Ack sent does not canonicalize: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(msgPub), canonical, frame.Attestation.Signature) {
		t.Fatal("the signature Ack sent does not verify under this agent's MESSAGING key")
	}
	if ed25519.Verify(ed25519.PublicKey(authPub), canonical, frame.Attestation.Signature) {
		t.Fatal("the signature Ack sent verifies under the AUTH key: it signed an acknowledgement with the key that proves this agent to its BUS rather than to its PEERS (invariant 3)")
	}
}
