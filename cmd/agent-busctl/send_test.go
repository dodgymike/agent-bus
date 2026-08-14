package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// TestCLISendReusesIdempotencyKeyOnRetry is the load-bearing test for CLI-4.
//
// Generating a fresh key per attempt turns the retry that idempotency exists to
// make safe into a SECOND MESSAGE. Invariant 10's whole distinction rests on
// this: same key + same payload is a legitimate retry the bus answers from its
// applied-key table; same key + different payload is a protocol violation the
// bus rejects with a 409 and logs, KEEPING the connection (narrowed 2026-08-08
// — it disconnected until then, and must not again). A client that varies
// EITHER half across the attempts of one logical send has broken one of those
// two, and the send simply does not happen.
//
// So the assertion is not "a key was sent" but "every attempt sent the SAME
// key, in the SAME payload, byte for byte".
//
// The first attempt fails with 503 AND a Retry-After: that pairing is what the
// transport classifies as a live capacity bound and retries. A 503 WITHOUT the
// header means the hub cannot durably accept messages, is marked fatal, and is
// deliberately NOT retried — using it here would produce zero retries and a
// vacuous proof.
func TestCLISendReusesIdempotencyKeyOnRetry(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts [][]byte
	)
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteSend {
			http.NotFound(w, r)
			return
		}
		raw := bodyOf(t, r)
		mu.Lock()
		attempts = append(attempts, raw)
		n := len(attempts)
		mu.Unlock()

		if n == 1 {
			// Transient by construction: something in flight finishes and the
			// capacity comes back. Retry-After is what says so.
			w.Header().Set("Retry-After", "1")
			stubWriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the applied-key table is full"})
			return
		}
		stubWriteJSON(w, http.StatusCreated, stubAccepted(5, "bus-x.agent-1", []string{"bus-x.other-1"}, false, []byte("payload")))
	})

	res := bus.run(t, "", false, false, "send", "--json", "bus-x.other-1", "payload")
	if res.Code != client.ExitOK {
		t.Fatalf("send exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) < 2 {
		t.Fatalf("the bus saw %d send attempts, want at least 2 — the retry never happened, so this proof is vacuous", len(attempts))
	}

	first := attempts[0]
	var firstBody map[string]interface{}
	if err := json.Unmarshal(first, &firstBody); err != nil {
		t.Fatalf("attempt 0 body is not JSON: %v (%q)", err, first)
	}
	firstKey, _ := firstBody["idempotency_key"].(string)
	if firstKey == "" {
		t.Fatalf("attempt 0 carried no idempotency_key: %v — every mutating operation carries one (invariant 10)", firstBody)
	}

	for i, raw := range attempts {
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("attempt %d body is not JSON: %v (%q)", i, err, raw)
		}
		key, _ := body["idempotency_key"].(string)
		if key != firstKey {
			t.Fatalf("attempt %d used idempotency key %q, want %q — a key minted per attempt makes a retry a SECOND message", i, key, firstKey)
		}
		if string(raw) != string(first) {
			t.Fatalf("attempt %d sent a DIFFERENT payload:\n  attempt 0: %s\n  attempt %d: %s\nsame key + different payload is a protocol violation the bus rejects with a 409 and logs, so this send would never be applied", i, first, i, raw)
		}
	}

	// The key is REPORTED back: it is the only handle a LATER retry has to be
	// the same logical send rather than a second message.
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		t.Fatalf("send --json stdout is not JSON: %v (%q)", err, res.Stdout)
	}
	if got, _ := out["idempotency_key"].(string); got != firstKey {
		t.Fatalf("reported idempotency_key = %v, want the key the send was applied under (%q)", out["idempotency_key"], firstKey)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf(`stdout "ok" = %v, want true`, out["ok"])
	}
}

// The FAILURE path is where the idempotency key earns its keep, and it is the
// path that dropped it.
//
// `agent-busctl send --help` promises "The key is always printed back, because it is
// the ONLY handle that makes a LATER retry the same logical send rather than a
// second message." That promise is only worth anything on a failure: on success
// the caller already knows the message landed. The genuinely AMBIGUOUS failures
// — a network error, or a 5xx — are exactly the ones where the send may or may
// not have been applied, so an operator who is not told the key has no way to
// retry the same logical send. Inventing a fresh key for the retry is not a
// retry at all: it is a SECOND message, which is precisely what invariant 10
// exists to prevent.
//
// The load-bearing assertion in every one of these is not "a key was printed"
// but "the key the OPERATOR is shown is the key the BUS was asked to apply".
// A key that is merely well-formed, or minted a second time for the error
// message, would be worse than none: it would look like a retry handle and
// produce a duplicate message when used.

// TestCLISendFailureReportsIdempotencyKey drives `agent-busctl send --json` against a
// bus that answers 500 on EVERY attempt — an ambiguous failure by construction,
// since a 500 says nothing about whether the write was applied.
func TestCLISendFailureReportsIdempotencyKey(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteSend {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "the write path fell over"})
	})

	res := bus.run(t, "", false, false, "send", "--json", "bus-x.other-1", "payload")
	// Pin the EXACT code, not merely "not 0". A 500 is the bus reporting a
	// failure of its own, so an agent branching on the exit code must see 6; a
	// test that accepts any non-zero code would pass if the failure were
	// misclassified as a usage error or an unreachable bus.
	if res.Code != client.ExitServer {
		t.Fatalf("send against a bus that answers 500 exited %d, want %d (client.ExitServer); stdout=%q stderr=%q",
			res.Code, client.ExitServer, res.Stdout, res.Stderr)
	}
	assertFailureReportsWireKey(t, res, bus.calls(stubRouteSend))
}

// TestCLIBroadcastFailureReportsIdempotencyKey is the same contract on the
// broadcast route. The two share a write path in the client, and a fix that
// covered only `send` would leave the noisier of the two commands unable to be
// retried safely.
func TestCLIBroadcastFailureReportsIdempotencyKey(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteBroadcast {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "the write path fell over"})
	})

	res := bus.run(t, "", false, false, "broadcast", "--json", "all hands")
	if res.Code != client.ExitServer {
		t.Fatalf("broadcast against a bus that answers 500 exited %d, want %d (client.ExitServer); stdout=%q stderr=%q",
			res.Code, client.ExitServer, res.Stdout, res.Stderr)
	}
	assertFailureReportsWireKey(t, res, bus.calls(stubRouteBroadcast))
}

// TestCLISendFailureNamesIdempotencyKeyInHumanMode is the human half of the
// same contract: without --json the failure goes to stderr, and it must tell a
// person what to DO, not merely what went wrong (invariant 7 — "errors that
// name the remedy rather than the stack").
//
// Two things are required and they are not the same: the KEY, so the retry is
// the same logical send, and the FLAG that takes it, so the reader does not
// have to go looking for it in `--help` at 3am.
func TestCLISendFailureNamesIdempotencyKeyInHumanMode(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteSend {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "the write path fell over"})
	})

	res := bus.run(t, "", false, false, "send", "bus-x.other-1", "payload")
	if res.Code == client.ExitOK {
		t.Fatalf("send against a bus that answers 500 exited %d, want a failure; stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
	}
	wire := idempotencyKeyOnWire(t, "send", bus.calls(stubRouteSend))

	if !strings.Contains(res.Stderr, wire) {
		t.Fatalf("human-mode stderr does not name the idempotency key the bus was asked to apply (%s):\n%s\n"+
			"without it an operator cannot retry the SAME logical send, and a fresh key would be a second message", wire, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "--idempotency-key") {
		t.Fatalf("human-mode stderr does not name the --idempotency-key flag:\n%s\n"+
			"a key with no flag to pass it to is a fact, not a remedy", res.Stderr)
	}
	if strings.Contains(res.Stdout, wire) {
		t.Fatalf("the key appeared on STDOUT in human mode: %q — diagnostics belong on stderr", res.Stdout)
	}
}

// TestCLISendFailureReportsExplicitIdempotencyKey guards the regression coming
// back in a NEW form: a fix that mints a fresh key for the error message would
// satisfy "a key is printed" while being actively harmful. When the caller
// named the key, the failure must report THAT key, byte for byte.
func TestCLISendFailureReportsExplicitIdempotencyKey(t *testing.T) {
	const explicit = "operator-chosen-key-1"

	cases := []struct {
		name  string
		route string
		args  []string
	}{
		{"send", stubRouteSend, []string{"send", "--json", "--idempotency-key", explicit, "bus-x.other-1", "payload"}},
		{"broadcast", stubRouteBroadcast, []string{"broadcast", "--json", "--idempotency-key", explicit, "all hands"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.route {
					http.NotFound(w, r)
					return
				}
				stubWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "the write path fell over"})
			})

			res := bus.run(t, "", false, false, tc.args...)
			if res.Code == client.ExitOK {
				t.Fatalf("%s against a bus that answers 500 exited %d, want a failure; stdout=%q stderr=%q", tc.name, res.Code, res.Stdout, res.Stderr)
			}
			if wire := idempotencyKeyOnWire(t, tc.name, bus.calls(tc.route)); wire != explicit {
				t.Fatalf("the bus was asked to apply key %q, want the one the caller passed (%q)", wire, explicit)
			}
			got := failureField(t, res, "idempotency_key")
			if got != explicit {
				t.Fatalf("the failure object reported idempotency_key = %q, want the caller's own key %q — a freshly minted key looks like a retry handle and produces a SECOND message when used", got, explicit)
			}
		})
	}
}

// TestCLISendNetworkFailureReportsIdempotencyKey covers the genuinely ambiguous
// case: the request reached the bus and the connection died before an answer
// came back. Nobody — not the client, not the operator — can tell whether the
// message was applied, so the key is the only thing that makes the retry safe.
//
// The failure is injected by HIJACKING the connection on /v1/send and closing
// it, rather than by pointing agent-busctl at a dead address. That distinction is
// load-bearing: a bus that is not listening fails during the SESSION handshake,
// long before a key is minted or a body is marshalled, so it would prove
// nothing about the write path. Killing the connection on the send itself
// produces a real KindNetwork failure with the key already on the wire, which
// is what lets this test compare what the operator is shown against what the
// bus was asked to apply.
func TestCLISendNetworkFailureReportsIdempotencyKey(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteSend {
			http.NotFound(w, r)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("the test server does not support hijacking; this failure cannot be made to look like a dead connection")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijacking the connection: %v", err)
			return
		}
		// No response at all: the client sees the connection close mid-request,
		// which is exactly the "it may or may not have been applied" case.
		_ = conn.Close()
	})

	// --timeout bounds the whole operation including its retries, so a hung
	// dial can never make this test slow.
	res := bus.run(t, "", false, false, "send", "--json", "--timeout", "5s", "bus-x.other-1", "payload")
	if res.Code != client.ExitNetwork {
		t.Fatalf("send over a dropped connection exited %d, want %d (a transport failure); stdout=%q stderr=%q",
			res.Code, client.ExitNetwork, res.Stdout, res.Stderr)
	}
	assertFailureReportsWireKey(t, res, bus.calls(stubRouteSend))
}

// TestCLISendFatalUnavailableTellsOperatorNotToRetryButKeepsTheKey is the
// end-to-end guard on the ONE case where "report the key" and "say what to do"
// pull in opposite directions.
//
// A 503 with NO Retry-After is the bus saying its write path cannot durably
// accept messages at all (client/transport.go, "the 503 split"). It is
// KindServer, so it is exit 6 — the bus reporting a failure of its OWN, not a
// bus that could not be reached — and client.IsFatalUnavailable reports that
// retrying will not clear it.
//
// The first fix for the lost-key defect replaced the remedy outright, which
// produced a failure that told the operator to retry a bus the same client had
// just classified as un-retryable, and threw away the actual diagnosis (a
// poisoned or non-durable write path) on the way. Both halves have to hold at
// once: the key survives, because per invariant 4 the bus is REFUSING rather
// than losing data and this send may still have been applied — and the
// instruction is to hold the key for later, not to hammer a dead write path.
func TestCLISendFatalUnavailableTellsOperatorNotToRetryButKeepsTheKey(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteSend {
			http.NotFound(w, r)
			return
		}
		// NO Retry-After header. Its absence is the whole signal.
		stubWriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the write path is poisoned"})
	})

	res := bus.run(t, "", false, false, "send", "bus-x.other-1", "payload")
	if res.Code != client.ExitServer {
		t.Fatalf("send against a fatal 503 exited %d, want %d (client.ExitServer): a fatal 503 is the bus reporting a failure of its own, not an unreachable bus; stdout=%q stderr=%q",
			res.Code, client.ExitServer, res.Stdout, res.Stderr)
	}

	wire := idempotencyKeyOnWire(t, "send", bus.calls(stubRouteSend))
	if !strings.Contains(res.Stderr, wire) {
		t.Fatalf("a fatal 503 dropped the idempotency key (%s) from the operator's output:\n%s\n"+
			"the bus is refusing rather than losing data (invariant 4), so this send may still have been applied and the key is the only handle that retries it as the SAME message", wire, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "do NOT retry") {
		t.Fatalf("a fatal 503 did not tell the operator to hold off:\n%s\n"+
			"client.IsFatalUnavailable reports this is not transient, so instructing a retry contradicts the client's own classification", res.Stderr)
	}
	// The anti-assertion: the non-fatal wording must not appear. This is the one
	// that actually catches a regression to "replace the remedy" — the key and
	// the flag are present in BOTH wordings, so only the absence of the plain
	// retry instruction distinguishes them.
	if strings.Contains(res.Stderr, "; retry with --idempotency-key") {
		t.Fatalf("a fatal 503 was given the NON-FATAL retry instruction:\n%s\n"+
			"retrying will not clear a poisoned write path; the key is a handle for after an operator has fixed the bus", res.Stderr)
	}
	// And the transport's own diagnosis must survive: it is what names the fault
	// an operator actually has to go and fix.
	if !strings.Contains(res.Stderr, "retrying will not clear it") {
		t.Fatalf("the fatal-503 diagnosis was destroyed by the idempotency-key annotation:\n%s", res.Stderr)
	}
}

// assertFailureReportsWireKey is the shared check: the command failed, its
// --json failure object is parseable, and the idempotency_key it reports is the
// one the bus was actually asked to apply.
func assertFailureReportsWireKey(t *testing.T, res cliResult, calls []stubRequest) {
	t.Helper()
	if res.Code == client.ExitOK {
		t.Fatalf("exit = %d, want a failure; stdout=%q stderr=%q", res.Code, res.Stdout, res.Stderr)
	}
	wire := idempotencyKeyOnWire(t, "write", calls)

	if ok, _ := failureObject(t, res)["ok"].(bool); ok {
		t.Fatalf(`the failure object has "ok": true; stdout=%q`, res.Stdout)
	}
	got := failureField(t, res, "idempotency_key")
	if got != wire {
		t.Fatalf("the failure object reported idempotency_key = %q, want %q — the key the bus was asked to apply.\n"+
			"stdout=%q\nAn operator who is not shown this key cannot retry the same logical send; a fresh key would be a SECOND message (invariant 10).",
			got, wire, res.Stdout)
	}
}

// failureObject parses the one-object --json failure document on stdout.
func failureObject(t *testing.T, res cliResult) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		t.Fatalf("--json stdout is not one parseable JSON object: %v (%q)", err, res.Stdout)
	}
	return out
}

// failureField reads one string field out of the --json failure object.
func failureField(t *testing.T, res cliResult, field string) string {
	t.Helper()
	out := failureObject(t, res)
	raw, present := out[field]
	if !present {
		t.Fatalf("the --json failure object carries no %q field: %v", field, out)
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("the --json failure object's %q is %T (%v), want a string", field, raw, raw)
	}
	return s
}

// idempotencyKeyOnWire returns the key the bus SAW, having checked that every
// attempt carried the same one.
//
// It fails loudly on zero attempts, because "the bus saw no write at all" would
// otherwise let a caller's assertion pass against an empty comparison and turn
// the whole proof vacuous.
func idempotencyKeyOnWire(t *testing.T, op string, calls []stubRequest) string {
	t.Helper()
	if len(calls) == 0 {
		t.Fatalf("the bus saw no %s attempts at all — nothing reached the write path, so this proof would be vacuous", op)
	}
	var key string
	for i, c := range calls {
		body := c.JSON()
		if body == nil {
			t.Fatalf("%s attempt %d body is not a JSON object: %q", op, i, c.Body)
		}
		got, _ := body["idempotency_key"].(string)
		if got == "" {
			t.Fatalf("%s attempt %d carried no idempotency_key: %v — every mutating operation carries one (invariant 10)", op, i, body)
		}
		if i == 0 {
			key = got
			continue
		}
		if got != key {
			t.Fatalf("%s attempt %d used idempotency key %q, want %q — one logical send is one key", op, i, got, key)
		}
	}
	return key
}

// TestCLISendBodySourceIsUnambiguous checks the body comes from EXACTLY ONE
// source, and that naming two is refused rather than resolved by a precedence
// rule nobody would remember. Picking one silently would send the wrong bytes,
// and the caller would only find out by comparing content hashes much later.
func TestCLISendBodySourceIsUnambiguous(t *testing.T) {
	file := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(file, []byte("from the file"), 0o600); err != nil {
		t.Fatalf("writing the body file: %v", err)
	}

	cases := []struct {
		name     string
		args     []string
		stdin    string
		wantCode int
		wantErr  string
		wantBody string // when the send is expected to succeed
	}{
		{
			name:     "positional and --file",
			args:     []string{"send", "bus-x.other-1", "inline", "--file", file},
			wantCode: client.ExitUsage,
			wantErr:  "given twice",
		},
		{
			name:     "--file and --stdin",
			args:     []string{"send", "bus-x.other-1", "--file", file, "--stdin"},
			stdin:    "from stdin",
			wantCode: client.ExitUsage,
			wantErr:  "given twice",
		},
		{
			name:     "positional and --stdin",
			args:     []string{"send", "bus-x.other-1", "inline", "--stdin"},
			stdin:    "from stdin",
			wantCode: client.ExitUsage,
			wantErr:  "given twice",
		},
		{
			name:     "nothing named, a non-TTY stdin with bytes",
			args:     []string{"send", "bus-x.other-1"},
			stdin:    "piped body",
			wantCode: client.ExitOK,
			wantBody: "piped body",
		},
		{
			name:     "nothing named, an EMPTY non-TTY stdin",
			args:     []string{"send", "bus-x.other-1"},
			stdin:    "",
			wantCode: client.ExitUsage,
			wantErr:  "empty",
		},
		{
			name:     "no recipient at all",
			args:     []string{"send"},
			stdin:    "piped body",
			wantCode: client.ExitUsage,
			wantErr:  "no recipient",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
				stubWriteJSON(w, http.StatusCreated, stubAccepted(1, "bus-x.agent-1", []string{"bus-x.other-1"}, false, []byte(tc.wantBody)))
			})
			// stdinIsTTY is FALSE: a pipe, which is the shape an agent shelling
			// out always has. An empty pipe must fail fast, never hang.
			res := bus.run(t, tc.stdin, false, false, tc.args...)
			if res.Code != tc.wantCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", res.Code, tc.wantCode, res.Stdout, res.Stderr)
			}
			if tc.wantErr != "" {
				if !strings.Contains(res.Stderr, tc.wantErr) {
					t.Fatalf("stderr = %q, want it to contain %q", res.Stderr, tc.wantErr)
				}
				if len(bus.calls(stubRouteSend)) != 0 {
					t.Fatalf("an ambiguous or empty body reached the bus; it must be refused locally")
				}
				return
			}
			calls := bus.calls(stubRouteSend)
			if len(calls) != 1 {
				t.Fatalf("the bus saw %d sends, want 1", len(calls))
			}
			got := decodedBody(t, calls[0].Body)
			if string(got) != tc.wantBody {
				t.Fatalf("the bus received body %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// TestCLISendBodyIsVerbatim checks a body is sent BYTE FOR BYTE — including a
// trailing newline.
//
// This is what makes the content hash reproducible: `printf 'x\n' | agent-busctl
// send` and `sha256sum` must agree. Trimming a newline would silently change
// that hash and corrupt every binary and structured payload.
func TestCLISendBodyIsVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
	}{
		{"trailing newline", "hello world\n"},
		{"leading and trailing whitespace", "  padded  "},
		{"several trailing newlines", "a\n\n\n"},
		{"embedded NUL", "before\x00after\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			want := []byte(tc.stdin)
			sum := sha256.Sum256(want)
			bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
				stubWriteJSON(w, http.StatusCreated, stubAccepted(1, "bus-x.agent-1", []string{"bus-x.other-1"}, false, decodedBody(t, bodyOf(t, r))))
			})
			res := bus.run(t, tc.stdin, false, false, "send", "--json", "bus-x.other-1")
			if res.Code != client.ExitOK {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
			}
			calls := bus.calls(stubRouteSend)
			if len(calls) != 1 {
				t.Fatalf("the bus saw %d sends, want 1", len(calls))
			}
			got := decodedBody(t, calls[0].Body)
			if string(got) != string(want) {
				t.Fatalf("the bus received %q, want %q verbatim — nothing may be trimmed, added or re-encoded", got, want)
			}

			// The hash the bus computed over what it received must equal the
			// hash of what the caller handed us. That is the property a caller
			// checks end to end.
			var out map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
				t.Fatalf("send --json stdout is not JSON: %v (%q)", err, res.Stdout)
			}
			if got, _ := out["content_sha256"].(string); got != hex.EncodeToString(sum[:]) {
				t.Fatalf("content_sha256 = %v, want %s — the bytes on the wire were not the bytes on stdin",
					out["content_sha256"], hex.EncodeToString(sum[:]))
			}
		})
	}
}

// TestCLIBroadcastSendsNoRecipient checks `agent-busctl broadcast` goes to the
// broadcast route with NO `to` field: the bus fans it out, it is not addressed,
// and the recipient list comes back empty rather than enumerated.
func TestCLIBroadcastSendsNoRecipient(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteBroadcast {
			t.Errorf("broadcast went to %s, want %s", r.URL.Path, stubRouteBroadcast)
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusCreated, stubAccepted(9, "bus-x.agent-1", []string{}, true, []byte("all hands")))
	})

	res := bus.run(t, "", false, false, "broadcast", "--json", "all hands")
	if res.Code != client.ExitOK {
		t.Fatalf("broadcast exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}

	calls := bus.calls(stubRouteBroadcast)
	if len(calls) != 1 {
		t.Fatalf("the bus saw %d broadcasts, want 1", len(calls))
	}
	body := calls[0].JSON()
	if body == nil {
		t.Fatalf("broadcast request body is not a JSON object: %q", calls[0].Body)
	}
	if _, ok := body["to"]; ok {
		t.Fatalf("the broadcast request carries a `to` field: %v — a broadcast has no recipient", body)
	}
	if _, ok := body["idempotency_key"]; !ok {
		t.Fatalf("the broadcast request carries no idempotency_key: %v", body)
	}
	if got := decodedBody(t, calls[0].Body); string(got) != "all hands" {
		t.Fatalf("the bus received body %q, want %q", got, "all hands")
	}
	if len(bus.calls(stubRouteSend)) != 0 {
		t.Fatalf("a broadcast also hit %s", stubRouteSend)
	}

	var out map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		t.Fatalf("broadcast --json stdout is not JSON: %v (%q)", err, res.Stdout)
	}
	if b, _ := out["broadcast"].(bool); !b {
		t.Fatalf(`stdout "broadcast" = %v, want true`, out["broadcast"])
	}
	if to, ok := out["to"].([]interface{}); !ok || len(to) != 0 {
		t.Fatalf(`stdout "to" = %v, want an empty list for a broadcast`, out["to"])
	}
}

// TestCLIBroadcastRefused501ExitsRejectedNotServer is the end-to-end version of
// the SIGN-6 client fix: `agent-busctl broadcast` against a bus that answers 501
// (the deliberate, permanent refusal — a broadcast cannot be signed under
// signing format v1) must exit 7 (client.ExitRejected), not 6, and the message
// an agent sees must say plainly that this is a refusal, not a fault, that
// NOTHING was applied, and it must not tell the agent to retry.
//
// Before this fix `agent-busctl broadcast` reported this as "the bus reported an
// INTERNAL ERROR", exit 6, with "may or may not have been APPLIED" and a
// `retry with --idempotency-key` instruction — which is exactly the retry loop
// SIGN-6(6) forbids for a TERMINAL rejection.
func TestCLIBroadcastRefused501ExitsRejectedNotServer(t *testing.T) {
	var calls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteBroadcast {
			http.NotFound(w, r)
			return
		}
		calls++
		stubWriteJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "a broadcast cannot be signed under signing format v1: the canonical format requires a non-empty recipient set and the canonical audience of a broadcast is SIGN-3's undecided question; SIGN-6 admits no unsigned message type, so this route is refused rather than accepting unsigned traffic",
		})
	})

	res := bus.run(t, "", false, false, "broadcast", "all hands")
	if res.Code != client.ExitRejected {
		t.Fatalf("broadcast against a 501 exited %d, want %d (client.ExitRejected); stdout=%q stderr=%q",
			res.Code, client.ExitRejected, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stderr, "INTERNAL ERROR") || strings.Contains(res.Stderr, "internal error") {
		t.Fatalf("stderr still reads as an internal error, not a deliberate refusal: %q", res.Stderr)
	}
	if strings.Contains(res.Stderr, "may or may not have been APPLIED") || strings.Contains(res.Stderr, "may or may not have been applied") {
		t.Fatalf("stderr falsely claims the outcome is ambiguous — a 501 here is certain: nothing was applied: %q", res.Stderr)
	}
	if strings.Contains(res.Stderr, "--idempotency-key") {
		t.Fatalf("stderr still advises a retry with --idempotency-key — SIGN-6(6) forbids this for a TERMINAL rejection: %q", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "SIGN-3") {
		t.Fatalf("stderr does not name SIGN-3 as the task that reopens broadcast: %q", res.Stderr)
	}
	if calls != 1 {
		t.Fatalf("the bus saw %d broadcast attempts, want 1 — a 501 must not be retried", calls)
	}
}

// TestCLISendSurfaces409MintLostDoesNotAdviseAFreshKey is the end-to-end
// version of the mint-vs-conflict split: after a bus restart the (memory-only)
// mint table is empty, so /v1/send answers 409 for a perfectly good
// idempotency key. Telling the operator to use a FRESH key there is harmful —
// if the original send had landed, a fresh key applies it a SECOND time
// (invariant 10) — so the human-mode remedy must say to redo the same
// reserve-then-send under the SAME key instead.
func TestCLISendSurfaces409MintLostDoesNotAdviseAFreshKey(t *testing.T) {
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteSend {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusConflict, map[string]string{
			"error": "no matching sequence reservation: mint a fresh message id with POST /v1/mint, re-sign it and re-send",
		})
	})

	res := bus.run(t, "", false, false, "send", "bus-x.other-1", "payload")
	if res.Code != client.ExitRejected {
		t.Fatalf("send against a mint-lost 409 exited %d, want %d (client.ExitRejected); stdout=%q stderr=%q",
			res.Code, client.ExitRejected, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stderr, "FRESH") {
		t.Fatalf("stderr advises a FRESH idempotency key for a lost reservation — harmful, since a fresh key double-applies an already-landed send (invariant 10): %q", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "SAME idempotency key") {
		t.Fatalf("stderr does not tell the operator to reuse the SAME idempotency key: %q", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "/v1/mint") {
		t.Fatalf("stderr does not point at /v1/mint to re-mint: %q", res.Stderr)
	}
}

// bodyOf reads a handler's request body. The stub bus has already replaced
// r.Body with a re-readable buffer, so this is safe inside a route handler.
func bodyOf(t *testing.T, r *http.Request) []byte {
	t.Helper()
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading the request body: %v", err)
	}
	return raw
}

// decodedBody pulls the standard-base64 `body` field out of a send or broadcast
// request and decodes it.
func decodedBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("request body is not JSON: %v (%q)", err, raw)
	}
	encoded, _ := m["body"].(string)
	if encoded == "" {
		t.Fatalf("request body has no `body` field: %v", m)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("`body` is not standard base64: %v (%q)", err, encoded)
	}
	return decoded
}
