package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/client"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ackRoute is the path the CLI must POST to. Spelled as a LITERAL rather than
// imported: this test is the thing that would notice the client silently
// starting to call a different route, and a constant shared with the code under
// test cannot notice that.
//
// NOTE THE ABSENT TRAILING SLASH. "/v1/ack/" is the SENDER's status route; a
// slash slipped onto this one would post an acknowledgement into the status
// route's subtree.
const ackRoute = "/v1/ack"

// ackFrame is the wire frame the bus receives, decoded for assertions. It
// mirrors internal/relay.PeerAckRequest — again as a literal shape, so a
// renamed json key shows up here as a zero value rather than as nothing.
type ackFrame struct {
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

// ackRecorder captures the frames one stub bus received.
type ackRecorder struct {
	mu     sync.Mutex
	frames []ackFrame
}

func (a *ackRecorder) record(t *testing.T, body []byte) ackFrame {
	t.Helper()
	var f ackFrame
	if err := json.Unmarshal(body, &f); err != nil {
		t.Errorf("stub bus: the ACK body is not JSON: %v (%s)", err, body)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frames = append(a.frames, f)
	return f
}

func (a *ackRecorder) all() []ackFrame {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ackFrame(nil), a.frames...)
}

// ackStubAnswer is what the stub bus replies with.
type ackStubAnswer struct {
	status int
	body   interface{}
}

// newAckBus stands up a stub bus that records every ACK frame and answers with
// answer. Every other route 404s, so a CLI that called the wrong one fails
// loudly rather than silently passing.
func newAckBus(t *testing.T, agentID string, rec *ackRecorder, answer func() ackStubAnswer) *stubBus {
	t.Helper()
	return newStubBus(t, agentID, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ackRoute || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body []byte
		if r.Body != nil {
			body = readAllForTest(t, r)
		}
		rec.record(t, body)
		a := answer()
		stubWriteJSON(w, a.status, a.body)
	})
}

// readAllForTest drains a request body the stub already re-attached.
func readAllForTest(t *testing.T, r *http.Request) []byte {
	t.Helper()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf
		}
	}
}

// ackKeys returns the identity's MESSAGING public key and its AUTH public key,
// read from the credential store the CLI just used.
//
// The two are different keys with different jobs (invariant 3), and telling
// them apart is the whole point of the signing assertion below.
func ackKeys(t *testing.T, dir string) (messaging, auth ed25519.PublicKey) {
	t.Helper()
	s, err := client.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred, err := s.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	encMsg, err := cred.MessagingPublicKey()
	if err != nil {
		t.Fatalf("the CLI did not mint a messaging key: %v", err)
	}
	rawMsg, err := base64.StdEncoding.DecodeString(encMsg)
	if err != nil {
		t.Fatalf("messaging public key is not base64: %v", err)
	}
	rawAuth, err := base64.StdEncoding.DecodeString(cred.Identity.PublicKey)
	if err != nil {
		t.Fatalf("auth public key is not base64: %v", err)
	}
	if string(rawMsg) == string(rawAuth) {
		t.Fatal("the messaging key and the auth key are the SAME key in this fixture; the assertion below could not tell them apart")
	}
	return ed25519.PublicKey(rawMsg), ed25519.PublicKey(rawAuth)
}

// TestAckRecipientCLI is ACK-15's recorded proof: the compiled CLI, driven the
// way an agent drives it, against a bus answering the §9.2 / §13 wire shapes.
//
// It NEVER hand-writes an HTTP call — the frame the stub records is the one
// client.Ack built — and it exercises invariant 7's three audiences: a human
// reading a block, an agent reading --json, and an agent branching on an exit
// code.
func TestAckRecipientCLI(t *testing.T) {
	t.Run("delivered: the frame, the signature, and the key that made it", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusOK, map[string]interface{}{
				"accepted": true, "duplicate": false, "state": "delivered",
			}}
		})

		// A flag AFTER the positional: the form an agent writes, reading left
		// to right — what, then how.
		res := bus.run(t, "", false, false, "ack", "bus-y-7", "--json")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want 0; stdout %s stderr %s", res.Code, res.Stdout, res.Stderr)
		}

		// EXACTLY ONE object on stdout: an agent piping this into a parser must
		// not have to know how many lines to expect.
		lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
		if len(lines) != 1 {
			t.Fatalf("--json wrote %d lines, want exactly 1:\n%s", len(lines), res.Stdout)
		}
		var out struct {
			OK             bool   `json:"ok"`
			CorrelationKey string `json:"correlation_key"`
			Recipient      string `json:"recipient"`
			Outcome        string `json:"outcome"`
			Accepted       bool   `json:"accepted"`
			Duplicate      bool   `json:"duplicate"`
			State          string `json:"state"`
			Class          string `json:"class"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
			t.Fatalf("stdout is not one JSON object (%v): %s", err, res.Stdout)
		}
		if !out.OK || !out.Accepted || out.State != "delivered" || out.Duplicate {
			t.Fatalf("json = %s, want ok/accepted true, state delivered, duplicate false", lines[0])
		}
		if out.CorrelationKey != "bus-y-7" || out.Recipient != bus.AgentID || out.Outcome != "delivered" {
			t.Errorf("json = %s, want the message, this agent's own fully-qualified id, and the outcome asserted", lines[0])
		}
		if out.Class != "" {
			t.Errorf("class = %q; a POSITIVE terminal has nothing to explain and carries no class (§5.4)", out.Class)
		}

		frames := rec.all()
		if len(frames) != 1 {
			t.Fatalf("the bus saw %d ACK frames, want exactly 1", len(frames))
		}
		f := frames[0]
		if f.ProtocolVersion != 1 {
			t.Errorf("protocol_version = %d, want 1 (the ACK WIRE version, not the signing format version)", f.ProtocolVersion)
		}
		if f.CorrelationKey != "bus-y-7" {
			t.Errorf("correlation_key = %q, want the MESSAGE ID — not a seq, not a delivery position", f.CorrelationKey)
		}
		if f.Recipient != bus.AgentID {
			t.Errorf("recipient = %q, want this agent's own fully-qualified id %q (invariant 2)", f.Recipient, bus.AgentID)
		}
		if f.Outcome != "delivered" || f.Class != "" {
			t.Errorf("outcome/class = %q/%q, want delivered with no class", f.Outcome, f.Class)
		}
		if f.EmittedAtUnixMilli <= 0 {
			t.Errorf("emitted_at = %d, want a positive Unix-millisecond clock reading", f.EmittedAtUnixMilli)
		}
		if f.Attestation == nil || len(f.Attestation.Signature) != signing.SignatureSize {
			t.Fatalf("attestation = %+v, want exactly %d signature bytes", f.Attestation, signing.SignatureSize)
		}

		// THE GUARD THIS TEST EXISTS FOR.
		//
		// The signature is verified with internal/signing — the AUTHORITATIVE
		// implementation — against the fields the frame carries. That checks two
		// things at once that no bus checks at all: that client/ack.go's mirror
		// of the byte layout still agrees with internal/signing/ack.go, and that
		// the CLI signed with the MESSAGING key.
		//
		// Every bus checks SHAPE ONLY, so a CLI signing with the auth key would
		// produce 64 perfectly acceptable bytes and nothing on the wire would
		// ever report the mistake. Only this assertion would.
		messaging, auth := ackKeys(t, bus.Dir)
		signed := signing.Ack{
			CorrelationKey:     f.CorrelationKey,
			Recipient:          f.Recipient,
			Outcome:            f.Outcome,
			Class:              f.Class,
			EmittedAtUnixMilli: f.EmittedAtUnixMilli,
		}
		if err := signing.VerifyAck(messaging, signed, f.Attestation.Signature); err != nil {
			t.Fatalf("the signature does not verify under this agent's MESSAGING key (%v).\nEither the CLI signed with the wrong key, or client/ack.go's canonical layout has diverged from internal/signing's.", err)
		}
		if err := signing.VerifyAck(auth, signed, f.Attestation.Signature); err == nil {
			t.Fatal("the signature ALSO verifies under the AUTH key: the CLI signed an acknowledgement with the key that proves it to its BUS rather than the one that proves it to its PEERS (invariant 3)")
		}
	})

	t.Run("refused: each of the three recipient classes, signed with the class inside the bytes", func(t *testing.T) {
		for _, class := range client.RecipientRefusalClasses() {
			class := class
			t.Run(class, func(t *testing.T) {
				rec := &ackRecorder{}
				bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
					return ackStubAnswer{http.StatusOK, map[string]interface{}{
						"accepted": true, "duplicate": false, "state": "refused", "class": class,
					}}
				})
				res := bus.run(t, "", false, false, "ack", "bus-y-7", "--refuse", class, "--json")
				if res.Code != client.ExitOK {
					t.Fatalf("exit = %d, want 0; stdout %s stderr %s", res.Code, res.Stdout, res.Stderr)
				}
				frames := rec.all()
				if len(frames) != 1 {
					t.Fatalf("the bus saw %d frames, want 1", len(frames))
				}
				f := frames[0]
				if f.Outcome != "refused" || f.Class != class {
					t.Fatalf("outcome/class = %q/%q, want refused/%s", f.Outcome, f.Class, class)
				}
				messaging, _ := ackKeys(t, bus.Dir)
				if err := signing.VerifyAck(messaging, signing.Ack{
					CorrelationKey:     f.CorrelationKey,
					Recipient:          f.Recipient,
					Outcome:            f.Outcome,
					Class:              f.Class,
					EmittedAtUnixMilli: f.EmittedAtUnixMilli,
				}, f.Attestation.Signature); err != nil {
					t.Fatalf("the refusal's signature does not verify with the class INSIDE the signed bytes: %v", err)
				}
				if !strings.Contains(res.Stdout, class) {
					t.Errorf("--json output does not carry the class: %s", res.Stdout)
				}
			})
		}
	})

	// A RECIPIENT MAY NOT ASSERT A ROUTING CLAIM. `undeliverable` says a BUS
	// will never deliver the message — a statement about a federation the
	// recipient cannot see. There is no --outcome flag, so it is unspellable
	// through the CLI; --refuse must not become a back door for it, and neither
	// may any of the NINE bus-emitted classes.
	t.Run("a recipient cannot assert a routing claim", func(t *testing.T) {
		for _, class := range []string{
			"undeliverable",
			"no_route", "no_such_recipient", "hop_refused", "hop_unauthenticated",
			"loop_dropped", "fanout_exceeded", "horizon_expired", "local_capacity", "obligation_lost",
			"recipient_refused_because_i_said_so", "",
		} {
			class := class
			t.Run("refuse="+class, func(t *testing.T) {
				rec := &ackRecorder{}
				bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
					return ackStubAnswer{http.StatusOK, map[string]interface{}{"accepted": true, "state": "refused"}}
				})
				args := []string{"ack", "bus-y-7", "--json"}
				if class != "" {
					args = append(args, "--refuse", class)
				}
				res := bus.run(t, "", false, false, args...)
				if class == "" {
					// The control: no --refuse at all is the DEFAULT, and it
					// must succeed as `delivered`. Without this the loop could
					// pass by refusing everything.
					if res.Code != client.ExitOK {
						t.Fatalf("exit = %d with no --refuse, want 0: delivered is the default", res.Code)
					}
					if got := rec.all(); len(got) != 1 || got[0].Outcome != "delivered" {
						t.Fatalf("frames = %+v, want exactly one `delivered`", got)
					}
					return
				}
				if res.Code != client.ExitUsage {
					t.Fatalf("`--refuse %s` exited %d, want 2 (usage); a recipient may emit only %s",
						class, res.Code, strings.Join(client.RecipientRefusalClasses(), ", "))
				}
				if got := rec.all(); len(got) != 0 {
					t.Fatalf("the CLI sent %d frames for `--refuse %s`; it must be refused LOCALLY, before anything is signed or sent", len(got), class)
				}
				// --json puts the failure envelope on STDOUT, so read both.
				said := res.Stdout + res.Stderr
				if class == "undeliverable" && !strings.Contains(said, "routing claim") {
					t.Errorf("the refusal does not say WHY undeliverable is refused, so an agent reading it cannot tell this from a typo: %s", said)
				}
			})
		}
	})

	t.Run("unknown is exit 8 with the object still printed", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusOK, map[string]interface{}{"accepted": false, "state": "unknown"}}
		})
		res := bus.run(t, "", false, false, "ack", "bus-y-7", "--json")
		if res.Code != client.ExitEmpty {
			t.Fatalf("exit = %d, want 8 (ExitEmpty) for the uniform `unknown` answer; stdout %s stderr %s", res.Code, res.Stdout, res.Stderr)
		}
		lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
		if len(lines) != 1 {
			t.Fatalf("--json wrote %d lines on the exit-8 path, want exactly 1:\n%s", len(lines), res.Stdout)
		}
		var out struct {
			OK       bool   `json:"ok"`
			Accepted bool   `json:"accepted"`
			State    string `json:"state"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
			t.Fatalf("stdout is not one JSON object (%v): %s", err, res.Stdout)
		}
		if !out.OK || out.Accepted || out.State != "unknown" {
			t.Fatalf("json = %s, want the result object with accepted:false, state:unknown — NOT a failure envelope", lines[0])
		}
	})

	t.Run("a duplicate of the same outcome is a success", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusOK, map[string]interface{}{
				"accepted": true, "duplicate": true, "state": "delivered",
			}}
		})
		res := bus.run(t, "", false, false, "ack", "bus-y-7")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want 0: re-acknowledging the SAME outcome is a legitimate retry (invariant 10), not an error", res.Code)
		}
		if !strings.Contains(res.Stdout, "Already acknowledged") {
			t.Errorf("the human output does not say it was already acknowledged: %s", res.Stdout)
		}
	})

	t.Run("a different terminal outcome is exit 7 and says the first stands", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusConflict, map[string]string{
				"error": "this delivery outcome is already terminal with a different outcome; the first terminal stands",
			}}
		})
		res := bus.run(t, "", false, false, "ack", "bus-y-7")
		if res.Code != client.ExitRejected {
			t.Fatalf("exit = %d, want 7 (ExitRejected) for invariant 10's second case; stderr %s", res.Code, res.Stderr)
		}
		// The transport's GENERIC 409 remedy talks about idempotency keys. This
		// frame carries none, so that advice names a header the caller cannot
		// find — the annotation must have replaced it.
		if strings.Contains(res.Stderr, "idempotency key") {
			t.Errorf("the 409 remedy still talks about idempotency keys, which this frame does not carry: %s", res.Stderr)
		}
		if !strings.Contains(res.Stderr, "ack-status") {
			t.Errorf("the remedy does not point at the one command that says what WAS recorded: %s", res.Stderr)
		}
	})

	t.Run("accepted:false with a state other than unknown is a bus fault, not a silent success", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusOK, map[string]interface{}{"accepted": false, "state": "delivered"}}
		})
		res := bus.run(t, "", false, false, "ack", "bus-y-7", "--json")
		if res.Code != client.ExitServer {
			t.Fatalf("exit = %d, want 6: `accepted:false` has exactly one meaning on this route and exiting 0 would report a settled message to a script that never looks again", res.Code)
		}
	})

	t.Run("a state outside the closed set is refused rather than passed through", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusOK, map[string]interface{}{"accepted": true, "state": "delivered!"}}
		})
		res := bus.run(t, "", false, false, "ack", "bus-y-7", "--json")
		if res.Code != client.ExitServer {
			t.Fatalf("exit = %d, want 6: an unrecognised state spelling must not reach a caller branching on == \"delivered\"", res.Code)
		}
	})

	t.Run("local usage errors never reach the bus", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
		}{
			{"no message id", []string{"ack"}},
			{"two message ids", []string{"ack", "bus-y-7", "bus-y-8"}},
			{"whitespace in the key", []string{"ack", "bus-y 7"}},
			{"not a message id at all", []string{"ack", "7"}},
			{"a bare sequence, not the message id", []string{"ack", "42"}},
			{"sequence zero is never allocated", []string{"ack", "bus-y-0"}},
			{"unknown flag", []string{"ack", "bus-y-7", "--outcome", "undeliverable"}},
			// PRESENT-BUT-EMPTY, the shape `--refuse "$CLASS"` takes when
			// $CLASS is unset. It must NOT fall back to `delivered`: that
			// would assert receipt for an agent that was trying to refuse,
			// and a terminal outcome is absorbing.
			{"--refuse with an empty value", []string{"ack", "bus-y-7", "--refuse", ""}},
			{"--refuse= with an empty value", []string{"ack", "bus-y-7", "--refuse="}},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := &ackRecorder{}
				bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
					return ackStubAnswer{http.StatusOK, map[string]interface{}{"accepted": true, "state": "delivered"}}
				})
				// stdinIsTTY true and NOTHING on stdin: a command that prompted
				// would block here forever rather than exiting 2.
				res := bus.run(t, "", false, true, tc.args...)
				if res.Code != client.ExitUsage {
					t.Fatalf("exit = %d, want 2 (usage); stdout %s stderr %s", res.Code, res.Stdout, res.Stderr)
				}
				if got := rec.all(); len(got) != 0 {
					t.Fatalf("the CLI sent %d frames for a locally-invalid invocation; nothing must be signed or sent", len(got))
				}
			})
		}
	})

	// Invariant 7, audience three: `agent-busctl ack -h` must describe the
	// command without touching a bus or a credential.
	t.Run("help is self-contained and names what it will not do", func(t *testing.T) {
		rec := &ackRecorder{}
		bus := newAckBus(t, "bus-x.agent-1", rec, func() ackStubAnswer {
			return ackStubAnswer{http.StatusOK, nil}
		})
		res := bus.run(t, "", false, false, "ack", "-h")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want 0 for -h", res.Code)
		}
		for _, want := range []string{
			"recipient_refused_policy", "recipient_refused_undecodable", "recipient_refused_not_addressed",
			"undeliverable", "MESSAGING key", "unverifiable by anyone",
		} {
			if !strings.Contains(strings.ToLower(res.Stdout), strings.ToLower(want)) {
				t.Errorf("`ack -h` does not mention %q", want)
			}
		}
		if got := rec.all(); len(got) != 0 {
			t.Fatal("`ack -h` reached the bus")
		}
	})
}

// TestAckIsRegistered checks the subcommand is reachable at all: invariant 7 is
// unmet while a capability has no subcommand, and a command that exists in a
// file but not in the registry is exactly that.
func TestAckIsRegistered(t *testing.T) {
	c, ok := lookupCommand("ack")
	if !ok {
		t.Fatal("`ack` is not registered in cmd/agent-busctl/root.go; POST /v1/ack would have no CLI")
	}
	if c.summary == "" || c.help == "" || c.run == nil {
		t.Fatal("`ack` is registered without a summary, help text or run function")
	}
	if !strings.Contains(c.help, "--json") {
		t.Error("`ack` help does not document --json, which every agent-facing command must offer")
	}
}
