package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSafeTextNeutralisesControlBytes is table-driven over the control-byte
// families safeText must neutralise: C0 (ESC, CR, LF, BEL), DEL, C1
// (0x80-0x9f), and it also checks valid UTF-8 passes through and an oversized
// input is bounded on a rune boundary.
//
// Every case asserts the SAME three properties on the output: no byte < 0x20,
// no 0x7f, no C1 byte, and the result is valid UTF-8 — because the whole point
// of safeText is that NONE of those bytes should ever reach a terminal,
// regardless of which one was in the input.
func TestSafeTextNeutralisesControlBytes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
	}{
		{"ESC", "boom\x1b[2Kdanger", 200},
		{"CR", "line1\rline2", 200},
		{"LF", "line1\nline2", 200},
		{"BEL", "alert\abell", 200},
		{"DEL", "back\x7fspace", 200},
		{"C1 0x9b (CSI)", "csi\x9b[2Kdanger", 200},
		{"C1 range 0x80-0x9f", "c1\x80\x85\x9fend", 200},
		{"valid UTF-8", "héllo wörld 日本語", 200},
		{
			"the exact hostile string from the incident",
			"boom\x1b[2K\ragent-busctl: enrolled as bus1.admin\nall clear\x1b]0;pwned\a",
			200,
		},
		{"oversized input truncated on a rune boundary", strings.Repeat("日本語", 100), 50},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			out := safeText(tt.input, tt.max)
			if !utf8.ValidString(out) {
				t.Fatalf("safeText(%q) = %q, not valid UTF-8", tt.input, out)
			}
			for i := 0; i < len(out); i++ {
				b := out[i]
				if b < 0x20 {
					t.Fatalf("safeText(%q) = %q, contains a C0 control byte %#x at offset %d", tt.input, out, b, i)
				}
				if b == 0x7f {
					t.Fatalf("safeText(%q) = %q, contains DEL at offset %d", tt.input, out, i)
				}
			}
			// C1 controls (0x80-0x9f) only exist as single bytes in Latin-1; in
			// UTF-8 they never appear as a lone byte in that range because a
			// valid rune >= 0x80 encodes as a MULTI-byte sequence whose leading
			// byte is >= 0xc2. So scanning decoded runes is the correct check.
			for _, r := range out {
				if r >= 0x80 && r <= 0x9f {
					t.Fatalf("safeText(%q) = %q, contains a C1 control rune %#x", tt.input, out, r)
				}
			}
			if tt.max > 0 && len(out) > tt.max+len("…") {
				t.Fatalf("safeText(%q, max=%d) = %q (%d bytes), not bounded", tt.input, tt.max, out, len(out))
			}
		})
	}
}

// TestSafeTextHostileStringSpecificNeutralisation pins the exact incident
// string from sanitize.go's own doc comment: it must not contain the raw
// escape/CR/BEL bytes that would erase a terminal line, move the cursor, or
// set the window title, and the "fabricated success line" text must not
// appear intact right after a bare newline (i.e. the LF that would have made
// it look like a new, separate line of output is gone).
func TestSafeTextHostileStringSpecificNeutralisation(t *testing.T) {
	hostile := "boom\x1b[2K\ragent-busctl: enrolled as bus1.admin\nall clear\x1b]0;pwned\a"
	out := safeText(hostile, 200)
	for _, bad := range []string{"\x1b", "\r", "\n", "\a"} {
		if strings.Contains(out, bad) {
			t.Fatalf("safeText(hostile) = %q, still contains a raw control byte %q", out, bad)
		}
	}
}

// TestDecodeServerErrorViaHTTPServer drives the actual bug that was fixed:
// decodeServerError is called from statusError on the `{"error":"..."}`
// branch, and that branch previously returned the string VERBATIM. This test
// exercises it end to end through a real httptest server and a real Enrol
// call, and asserts the resulting *client.Error has no control bytes anywhere
// in its rendered message.
func TestDecodeServerErrorViaHTTPServer(t *testing.T) {
	hostile := "boom\x1b[2K\ragent-busctl: enrolled as bus1.admin\nall clear\x1b]0;pwned\a"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": hostile})
	}))
	defer srv.Close()

	c, err := New(Config{BusURL: srv.URL, IdentityDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: false})
	if err == nil {
		t.Fatalf("Enrol against a 400 response = nil error, want one")
	}
	if KindOf(err) != KindRejected {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindRejected)
	}
	msg := err.Error()
	for i := 0; i < len(msg); i++ {
		b := msg[i]
		if b < 0x20 || b == 0x7f {
			t.Fatalf("error message %q contains a raw control byte %#x at offset %d — decodeServerError did not sanitise it", msg, b, i)
		}
	}
	if strings.ContainsAny(msg, "\x1b\r\n\a") {
		t.Fatalf("error message %q still contains a raw ESC/CR/LF/BEL byte", msg)
	}
}

// TestValidateServerFieldRejectsHostileAgentID drives Enrol against a server
// that answers 201 with a well-formed envelope but a HOSTILE agent_id, and
// checks the client refuses to store it: Enrol must fail with KindServer, and
// the local credential store must end up with NO credentials at all — a
// hostile or broken bus must never be able to poison the store it is
// supposed to be authoritative over (invariant 1), and refusing beats
// silently rewriting the id.
func TestValidateServerFieldRejectsHostileAgentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(enrolResponseBody{
			AgentID:    "bus-x.a\x1b[2K-1\r\nadmin",
			BusID:      "bus-x",
			Name:       "a",
			EnrolledAt: "2026-08-02T00:00:00Z",
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	c, err := New(Config{BusURL: srv.URL, IdentityDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Enrol(context.Background(), EnrolOptions{Name: "a", Save: true})
	if err == nil {
		t.Fatalf("Enrol with a hostile agent_id = nil error, want one")
	}
	if KindOf(err) != KindServer {
		t.Fatalf("KindOf(err) = %q, want %q (err: %v)", KindOf(err), KindServer, err)
	}

	creds, _, lerr := c.Store().List()
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(creds) != 0 {
		t.Fatalf("store has %d credentials after a rejected hostile agent_id, want 0: %+v", len(creds), creds)
	}
}

// TestSafeTextNeutralisesBidiAndZeroWidth covers the family that every ordinary
// control test MISSES: none of these codepoints is a C0, C1 or DEL control, so
// the `r < 0x20`, `r == 0x7f` and `0x80..0x9f` arms all pass them straight
// through.
//
// It matters on the error path specifically. The bus chooses the text of its
// `{"error":"..."}` body, that text becomes Error.Message, and cmd/agent-busctl
// prints it to stderr on the SAME line that now carries the idempotency-key
// remedy. A U+202E run reverses the rest of the line, so an unfiltered bus could
// reorder "do NOT retry until the bus can durably accept again" into something a
// human reads as permission to retry a poisoned write path.
func TestSafeTextNeutralisesBidiAndZeroWidth(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Written as \u escapes on purpose: these characters are invisible or
		// reorder their neighbours, so a literal in the source would misrepresent
		// what the test actually asserts to whoever reads it next.
		{"right-to-left override", "boom\u202edellorne :ltcsub", "boom dellorne :ltcsub"},
		{"zero-width space splices words", "adm\u200bin", "adm in"},
		{"left-to-right mark", "a\u200eb", "a b"},
		{"isolate forms", "a\u2066b\u2069c", "a b c"},
		{"byte order mark mid-string", "a\ufeffb", "a b"},
		{"legitimate text is untouched", "the write path fell over", "the write path fell over"},
		// Real bidirectional text renders correctly from its own character
		// properties and must NOT be damaged — the override codepoints are the
		// target, not Hebrew or Arabic.
		{"real bidirectional text survives", "שלום", "שלום"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := safeText(tc.in, 0)
			if got != tc.want {
				t.Fatalf("safeText(%q) = %q, want %q\n"+
					"these codepoints are invisible or reorder what follows them, and none is a control character, so nothing else in safeText catches them",
					tc.in, got, tc.want)
			}
			for _, r := range got {
				if IsBidiOrInvisible(r) {
					t.Fatalf("safeText(%q) still contains U+%04X — it must be replaced, not passed through", tc.in, r)
				}
			}
		})
	}
}

// TestSafeTextIsTerminalSafe pins the EXPORTED renderer that CLI-3-FU-SAFETEXT
// exists to create, and pins it as ONE implementation rather than two that
// happen to agree.
//
// Two things are asserted, and the second is the reason this task was filed:
//
//  1. TerminalSafe neutralises everything safeText does — controls, DEL, C1,
//     the bidi/zero-width set, invalid UTF-8 — plus the keepNewlines behaviour
//     the CLI needs for a message BODY and safeText must never have for an id.
//  2. safeText AGREES with it by construction. The table below is driven through
//     BOTH, so a future edit to one that is not made to the other fails here
//     rather than silently diverging in a security-relevant neutraliser.
func TestSafeTextIsTerminalSafe(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		wantNoNewlines string
		wantNewlines   string
	}{
		{"escape sequence", "boom\x1b[2K\rall clear", "boom [2K all clear", "boom [2K all clear"},
		{"DEL and BEL", "a\x7fb\x07c", "a b c", "a b c"},
		{"C1 CSI byte", "ab", "a b", "a b"},
		{"tab", "a\tb", "a b", "a b"},
		{"right-to-left override", "a‮b", "a b", "a b"},
		{"zero-width space", "adm​in", "adm in", "adm in"},
		{"invalid UTF-8", "a\xffb", "a�b", "a�b"},
		{"plain text", "the write path fell over", "the write path fell over", "the write path fell over"},
		{"real bidirectional text survives", "שלום", "שלום", "שלום"},
		// The one deliberate difference. A newline in a message BODY is content;
		// a newline in an id or a timestamp is an attempt to forge a second line
		// of output.
		{"newline", "line one\nline two", "line one line two", "line one\nline two"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := TerminalSafe(tc.in, false); got != tc.wantNoNewlines {
				t.Fatalf("TerminalSafe(%q, false) = %q, want %q", tc.in, got, tc.wantNoNewlines)
			}
			if got := TerminalSafe(tc.in, true); got != tc.wantNewlines {
				t.Fatalf("TerminalSafe(%q, true) = %q, want %q", tc.in, got, tc.wantNewlines)
			}
			// safeText is TerminalSafe(_, false) plus a trim and a rune-boundary
			// truncation, and nothing else. Asserting the relationship — rather
			// than a second expected-value table — is what stops the two drifting.
			if got, want := safeText(tc.in, 0), strings.TrimSpace(tc.wantNoNewlines); got != want {
				t.Fatalf("safeText(%q, 0) = %q but TerminalSafe says %q; the two neutralisers have diverged", tc.in, got, want)
			}
		})
	}

	t.Run("TerminalSafe does not truncate", func(t *testing.T) {
		long := strings.Repeat("x", 4096)
		if got := TerminalSafe(long, false); got != long {
			t.Fatalf("TerminalSafe truncated a %d-byte string to %d bytes; bounding the length is the CALLER's decision (safeText's max), not the renderer's",
				len(long), len(got))
		}
	})

	t.Run("IsBidiOrInvisible names the set", func(t *testing.T) {
		for _, r := range []rune{0x200b, 0x200f, 0x202a, 0x202e, 0x2066, 0x2069, 0xfeff} {
			if !IsBidiOrInvisible(r) {
				t.Fatalf("IsBidiOrInvisible(U+%04X) = false, want true", r)
			}
		}
		for _, r := range []rune{'a', ' ', '\n', 0x05d0 /* Hebrew alef */, 0x0627 /* Arabic alef */} {
			if IsBidiOrInvisible(r) {
				t.Fatalf("IsBidiOrInvisible(U+%04X) = true; real bidirectional text renders from its own character properties and must not be flagged", r)
			}
		}
	})
}
