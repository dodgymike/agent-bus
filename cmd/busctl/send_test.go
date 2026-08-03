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
// applied-key table; same key + different payload is a protocol violation that
// gets the client disconnected. A client that varies EITHER half across the
// attempts of one logical send has broken one of those two.
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
			t.Fatalf("attempt %d sent a DIFFERENT payload:\n  attempt 0: %s\n  attempt %d: %s\nsame key + different payload is a protocol violation that disconnects the client", i, first, i, raw)
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
// This is what makes the content hash reproducible: `printf 'x\n' | busctl
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

// TestCLIBroadcastSendsNoRecipient checks `busctl broadcast` goes to the
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
