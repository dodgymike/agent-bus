package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/client"
)

// TestCLIWatchStreamsNDJSONIncrementally is the load-bearing test for CLI-3.
//
// # Why it is written this way, and not the easy way
//
// The easy version collects stdout in a bytes.Buffer, waits for the command to
// finish, and asserts the result parses as NDJSON. That proves NOTHING about
// the property this command exists for: an implementation that buffered every
// record and flushed at exit would pass it identically — and `agent-busctl watch` is
// not meant to exit, so a stream that only became parseable at exit would be
// useless.
//
// So: stdout is an io.Pipe. A pipe write BLOCKS until somebody reads, and the
// stub bus parks after the first message, so the command provably has not
// finished. Asserting that a complete NDJSON line is READABLE at that moment is
// the only assertion that distinguishes incremental from buffered.
func TestCLIWatchStreamsNDJSONIncrementally(t *testing.T) {
	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteWait {
			http.NotFound(w, r)
			return
		}
		if atomic.AddInt32(&polls, 1) == 1 {
			stubWriteJSON(w, http.StatusOK, client.Batch{
				Messages: []client.Message{stubMessage(1, "bus-x.other-1", "bus-x.agent-1", "first record")},
				Cursor:   "cursor-1",
			})
			return
		}
		// Park until the client hangs up. The command therefore CANNOT have
		// finished while the assertions below run.
		<-r.Context().Done()
	})

	pr, pw := io.Pipe()
	var stderr strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		code := runWithTTY(ctx, bus.args("watch", "--poll-timeout", "5s"),
			strings.NewReader(""), pw, &stderr, emptyEnv, false, false)
		_ = pw.Close()
		done <- code
	}()

	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		br := bufio.NewReader(pr)
		line, err := br.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
		// Keep draining so a later write can never wedge the command.
		_, _ = io.Copy(io.Discard, br)
	}()

	var line string
	select {
	case line = <-lineCh:
	case err := <-errCh:
		t.Fatalf("reading the first NDJSON line: %v; stderr=%q", err, stderr.String())
	case code := <-done:
		t.Fatalf("the command exited (code %d) before a single line was readable; stderr=%q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no complete NDJSON line arrived while the command was still running — the stream is BUFFERED, not incremental; stderr=%q", stderr.String())
	}

	// The command must still be running: the stub is parked, so anything else
	// means the record was flushed at exit rather than as it arrived.
	select {
	case code := <-done:
		t.Fatalf("the command had already exited (code %d) when the first line was read; incrementality is unproven", code)
	default:
	}

	// One object per line, COMPACT: exactly one newline, and it is the last
	// byte. A record broken across lines would defeat every line-oriented
	// consumer this format exists for.
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("the record is not one compact line: %q", line)
	}
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		t.Fatalf("the record is indented (pretty-printed): %q", line)
	}
	trimmed := strings.TrimSuffix(line, "\n")
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		t.Fatalf("the record is not a bare JSON object — an envelope or array brackets would break a streaming consumer: %q", line)
	}

	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		t.Fatalf("the record is not JSON: %v (%q)", err, line)
	}
	if _, ok := rec["ok"]; ok {
		t.Fatalf(`the record carries an "ok" field: %v — Stream deliberately omits it; a stream is messages, not wrappers`, rec)
	}
	if _, ok := rec["messages"]; ok {
		t.Fatalf("the record is wrapped in an envelope: %v", rec)
	}
	if got, _ := rec["message_id"].(string); got != "bus-x-1" {
		t.Fatalf("message_id = %v, want %q", rec["message_id"], "bus-x-1")
	}
	if got, _ := rec["text"].(string); got != "first record" {
		t.Fatalf("text = %v, want %q for a plain-text body", rec["text"], "first record")
	}
	if got, _ := rec["body"].(string); got == "" {
		t.Fatalf("body is missing; it is the AUTHORITATIVE lossless form and is always present: %v", rec)
	}

	// A Ctrl-C is the successful end of a tail.
	cancel()
	select {
	case code := <-done:
		if code != client.ExitOK {
			t.Fatalf("exit = %d, want %d — an unbounded watch stopped by a signal is a success; stderr=%q", code, client.ExitOK, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the command did not exit after the context was cancelled")
	}
}

// TestCLIWatchHumanFeedNeutralisesTerminalEscapes is a SECURITY test.
//
// A message body is bytes chosen by ANOTHER AGENT. Rendered verbatim to a
// terminal, "\x1b[2K\r" erases the line just printed and repaints it with
// whatever the sender likes — so a message can forge the appearance of a
// message from someone else, or of a agent-busctl status line. A lone U+009B is CSI
// on some terminals, which is as dangerous as ESC-[.
//
// Nothing a terminal can act on may reach stdout on the human path. The other
// half matters too: a real newline in a body is CONTENT and must survive as a
// line break, with the continuation indented so a multi-line message can never
// be mistaken for several messages.
//
// Both halves are asserted over ONE body, because they genuinely have to hold
// together: the render path admits any body that is valid UTF-8 and neutralises
// it, so a message can carry an ESC, a CR, a C1, a tab AND a real newline at
// once. That combination is exactly what an attacker would send.
func TestCLIWatchHumanFeedNeutralisesTerminalEscapes(t *testing.T) {
	// One body carrying every dangerous form at once, plus a newline that is
	// legitimate CONTENT:
	//   \x1b[2K   erase-line, the forgery primitive
	//   \r        carriage return, repaints the line just printed
	//   \u009b    the C1 CSI — a single one of these is as dangerous as ESC-[
	//             on the terminals that honour it
	//   \t        a tab, which would break the continuation indent
	//   \n        a real newline, which MUST survive
	const hostile = "before\x1b[2K\ragent-busctl: enrolled as bus-x.admin\u009b2K\tafter\nsecond line"

	// A second body that is not valid UTF-8 at all (a LONE 0x9b byte, not the
	// two-byte encoding of U+009B). There is no honest rendering of that, so it
	// is withheld with a notice rather than turned into a screenful of
	// replacement characters.
	const notText = "csi\x9b!"

	// Guard the fixtures. Every assertion below is about bytes NOT appearing in
	// the output, so a fixture edited to drop the dangerous bytes would leave a
	// test that passes while proving nothing at all.
	for _, want := range []struct {
		name string
		b    byte
	}{{"ESC", 0x1b}, {"CR", '\r'}, {"tab", '\t'}, {"newline", '\n'}} {
		if !strings.ContainsRune(hostile, rune(want.b)) {
			t.Fatalf("the hostile fixture no longer contains a %s; this test would be vacuous", want.name)
		}
	}
	if !strings.ContainsRune(hostile, 0x9b) {
		t.Fatalf("the hostile fixture no longer contains U+009B; this test would be vacuous")
	}
	if utf8.ValidString(notText) {
		t.Fatalf("the not-text fixture is valid UTF-8; it would not exercise the withheld-body path")
	}

	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "" {
			<-r.Context().Done()
			return
		}
		stubWriteJSON(w, http.StatusOK, client.Batch{
			Messages: []client.Message{
				stubMessage(1, "bus-x.other-1", "bus-x.agent-1", hostile),
				stubMessage(2, "bus-x.other-1", "bus-x.agent-1", notText),
			},
			Cursor: "cursor-2",
		})
	})

	// stdoutIsTTY = true and no --json: the HUMAN feed.
	res := bus.run(t, "", true, false, "watch", "--poll-timeout", "5s", "--count", "2")
	if res.Code != client.ExitOK {
		t.Fatalf("watch exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}

	// Half one: nothing a terminal can act on survives. C0, DEL and a raw 0x9b
	// are checked per BYTE; the C1 RANGE is checked per RUNE, because a
	// byte-wise check would false-positive on any UTF-8 continuation byte (the
	// 0x86 inside "→", for one).
	for i := 0; i < len(res.Stdout); i++ {
		switch b := res.Stdout[i]; {
		case b == 0x1b:
			t.Fatalf("stdout byte %d is ESC (0x1b); a body can repaint the line above it: %q", i, res.Stdout)
		case b == '\r':
			t.Fatalf("stdout byte %d is CR; a body can overwrite the line it is printed on: %q", i, res.Stdout)
		case b == 0x07:
			t.Fatalf("stdout byte %d is BEL: %q", i, res.Stdout)
		case b == 0x7f:
			t.Fatalf("stdout byte %d is DEL: %q", i, res.Stdout)
		case b == 0x9b:
			t.Fatalf("stdout byte %d is a raw 0x9b (CSI): %q", i, res.Stdout)
		case b == '\t':
			t.Fatalf("stdout byte %d is a tab, which breaks the indent that marks a continuation line: %q", i, res.Stdout)
		}
	}
	for i, r := range res.Stdout {
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("stdout rune at %d is the C1 control %#x; a lone U+009B is CSI on some terminals: %q", i, r, res.Stdout)
		}
	}

	// Half two: a REAL newline is content and survives as an INDENTED
	// continuation line, so a multi-line body can never be mistaken for two
	// messages.
	if !strings.Contains(res.Stdout, "\n  second line") {
		t.Fatalf("the newline in the body did not survive as an indented continuation line: %q", res.Stdout)
	}
	// Controls are REPLACED in place, not dropped: dropping the ESC out of
	// "adm\x1bin" would splice it into the convincing token "admin".
	if !strings.Contains(res.Stdout, "before [2K agent-busctl: enrolled as bus-x.admin 2K after") {
		t.Fatalf("the controls were not replaced IN PLACE with spaces: %q", res.Stdout)
	}

	// The body that could not be rendered honestly must SAY so and name the
	// lossless route to it — a silently missing body would be worse than a
	// mangled one.
	if n := strings.Count(res.Stdout, "not text"); n != 1 {
		t.Fatalf("the feed announced %d withheld bodies, want exactly 1 (the invalid-UTF-8 one): %q", n, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "--json") {
		t.Fatalf("a withheld body does not name --json as the lossless route to it: %q", res.Stdout)
	}
	// The withheld body's own bytes must not leak into the notice.
	if strings.Contains(res.Stdout, "csi") {
		t.Fatalf("the withheld body leaked into the notice: %q", res.Stdout)
	}
}

// TestCLIWatchPipedDefaultsToNDJSON checks the output mode is chosen for the
// caller: a PIPE is a machine and gets NDJSON even without --json, while a
// terminal gets the readable feed.
func TestCLIWatchPipedDefaultsToNDJSON(t *testing.T) {
	newBus := func(t *testing.T) *stubBus {
		return newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") != "" {
				<-r.Context().Done()
				return
			}
			stubWriteJSON(w, http.StatusOK, client.Batch{
				Messages: []client.Message{stubMessage(1, "bus-x.other-1", "bus-x.agent-1", "hello")},
				Cursor:   "cursor-1",
			})
		})
	}

	t.Run("stdout is a pipe, no --json", func(t *testing.T) {
		bus := newBus(t)
		res := bus.run(t, "", false, false, "watch", "--poll-timeout", "5s", "--count", "1")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want %d; stderr=%q", res.Code, client.ExitOK, res.Stderr)
		}
		first := strings.SplitN(strings.TrimSuffix(res.Stdout, "\n"), "\n", 2)[0]
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(first), &rec); err != nil {
			t.Fatalf("a piped watch did not default to NDJSON: %v (%q)", err, res.Stdout)
		}
		if got, _ := rec["message_id"].(string); got != "bus-x-1" {
			t.Fatalf("message_id = %v, want %q", rec["message_id"], "bus-x-1")
		}
	})

	t.Run("stdout is a TTY, no --json", func(t *testing.T) {
		bus := newBus(t)
		res := bus.run(t, "", true, false, "watch", "--poll-timeout", "5s", "--count", "1")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want %d; stderr=%q", res.Code, client.ExitOK, res.Stderr)
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &rec); err == nil {
			t.Fatalf("a terminal got a JSON object rather than a readable feed: %q", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "hello") {
			t.Fatalf("the human feed does not contain the message body: %q", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "bus-x.other-1") {
			t.Fatalf("the human feed does not name the FULLY-QUALIFIED sender: %q", res.Stdout)
		}
	})

	t.Run("--json on a TTY is still NDJSON", func(t *testing.T) {
		bus := newBus(t)
		res := bus.run(t, "", true, false, "watch", "--json", "--poll-timeout", "5s", "--count", "1")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want %d; stderr=%q", res.Code, client.ExitOK, res.Stderr)
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &rec); err != nil {
			t.Fatalf("--json did not produce NDJSON on a TTY: %v (%q)", err, res.Stdout)
		}
	})
}

// TestCLIWatchBoundedEmptyExitsEmpty checks the two "nothing arrived" outcomes,
// which are deliberately different.
//
// A BOUNDED watch (--count / --for) that delivered nothing exits 8, so
// `agent-busctl watch --for 30s --count 1` is a usable "wait for one message" and the
// caller branches on the code rather than parsing text. An UNBOUNDED watch was
// stopped by a signal, which is a success however many messages it saw.
func TestCLIWatchBoundedEmptyExitsEmpty(t *testing.T) {
	quiet := func(w http.ResponseWriter, r *http.Request) {
		// Park until the caller's own deadline or cancellation ends the request.
		<-r.Context().Done()
	}

	t.Run("bounded and empty exits 8", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", quiet)
		res := bus.run(t, "", false, false, "watch", "--poll-timeout", "5s", "--for", "250ms", "--count", "1")
		if res.Code != client.ExitEmpty {
			t.Fatalf("bounded empty watch exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitEmpty, res.Stdout, res.Stderr)
		}
		if strings.TrimSpace(res.Stdout) != "" {
			t.Fatalf("stdout = %q, want nothing — there was nothing to report", res.Stdout)
		}
	})

	t.Run("bounded and empty exits 8 under --json with a parseable object", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", quiet)
		res := bus.run(t, "", false, false, "watch", "--json", "--poll-timeout", "5s", "--for", "250ms", "--count", "1")
		if res.Code != client.ExitEmpty {
			t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitEmpty, res.Stdout, res.Stderr)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &payload); err != nil {
			t.Fatalf("--json did not render the empty outcome as a parseable object: %v (%q)", err, res.Stdout)
		}
		if kind, _ := payload["kind"].(string); kind != string(client.KindEmpty) {
			t.Fatalf("kind = %v, want %q", payload["kind"], client.KindEmpty)
		}
	})

	t.Run("unbounded, stopped by cancellation, exits 0", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", quiet)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan cliResult, 1)
		go func() { done <- bus.runCtx(t, ctx, "", false, false, "watch", "--poll-timeout", "5s") }()
		time.AfterFunc(250*time.Millisecond, cancel)
		select {
		case res := <-done:
			if res.Code != client.ExitOK {
				t.Fatalf("unbounded watch stopped by cancellation exit = %d, want %d; stderr=%q", res.Code, client.ExitOK, res.Stderr)
			}
		case <-time.After(15 * time.Second):
			cancel()
			t.Fatalf("the unbounded watch did not stop after cancellation")
		}
	})
}

// TestWatchFatalUnavailableExitCodeMatchesDocumentedTable pins the ONE fact
// that a help text can get wrong without anything failing to build: the number
// `agent-busctl watch` documents for a fatal 503 must be the number it actually
// exits with.
//
// # Why both halves are observed rather than asserted
//
// The exit code is taken from a REAL run: the stub bus answers /v1/wait with 503
// and NO Retry-After, which is exactly the condition client/transport.go's "503
// split" classifies as fatal — the bus saying its write path cannot durably
// accept messages (invariant 4: it refuses rather than losing data). That is
// KindServer, and KindServer maps to client.ExitServer.
//
// The documented code is PARSED out of watchCommand().help, not written into
// this test. Hard-coding "the table says 6" would re-create the original defect
// one layer up: the table drifted from the code once already, and a test that
// carries its own copy of the answer cannot notice the next drift.
func TestWatchFatalUnavailableExitCodeMatchesDocumentedTable(t *testing.T) {
	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteWait {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&polls, 1)
		// NO Retry-After. Its absence is the whole signal: with the header this
		// is an ordinary capacity refusal the watch would retry for ever, and
		// the test would hang instead of observing an exit code.
		stubWriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the hub cannot durably accept messages"})
	})

	res := bus.run(t, "", false, false, "watch", "--json", "--poll-timeout", "5s")
	if got := atomic.LoadInt32(&polls); got == 0 {
		t.Fatalf("the bus was never polled; nothing exercised the fatal-503 path and this proof would be vacuous")
	}

	// Anchor: a fatal 503 is KindServer, so the client's own mapping says the
	// process exits with client.ExitServer. Taken from the constant, not from a
	// literal, so the anchor cannot drift either.
	if res.Code != client.ExitServer {
		t.Fatalf("a fatal 503 on /v1/wait exited %d, want client.ExitServer (%d) — a 503 with no Retry-After is KindServer; stdout=%q stderr=%q",
			res.Code, client.ExitServer, res.Stdout, res.Stderr)
	}

	entries := parseExitCodeTable(t, "watch", watchCommand().help)
	var documented []exitCodeEntry
	for _, e := range entries {
		if strings.Contains(e.Text, "503") {
			documented = append(documented, e)
		}
	}
	if len(documented) == 0 {
		t.Fatalf("`agent-busctl watch --help` no longer documents the fatal 503 anywhere in its EXIT CODES table; this check has nothing left to compare and would pass vacuously.\nentries: %v", entries)
	}
	for _, e := range documented {
		if e.Code != res.Code {
			t.Fatalf("`agent-busctl watch --help` documents the fatal 503 under exit %d (%q), but the command actually exits %d.\n"+
				"A fatal 503 is the bus reporting a failure of its own (client.ExitServer = %d), not a bus that could not be reached (client.ExitNetwork = %d). "+
				"An agent branching on the documented number would mis-handle the one failure it most needs to stop on.",
				e.Code, e.Text, res.Code, client.ExitServer, client.ExitNetwork)
		}
	}
}

// TestCLIWatchRejectsPollTimeoutAboveCeiling checks a poll timeout above the
// bus's ceiling is REFUSED, not silently clamped — exactly as the bus refuses
// it. A caller who asked for ten minutes and was quietly given five would
// conclude its request had been dropped.
func TestCLIWatchRejectsPollTimeoutAboveCeiling(t *testing.T) {
	cases := []struct {
		name string
		flag string
		want string
	}{
		{"above the ceiling", "10m", client.MaxPollTimeout.String()},
		{"zero", "0s", "positive"},
		{"negative", "-5s", "positive"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				stubWriteJSON(w, http.StatusOK, client.Batch{Messages: []client.Message{}, TimedOut: true})
			})
			res := bus.run(t, "", false, false, "watch", "--poll-timeout", tc.flag)
			if res.Code != client.ExitUsage {
				t.Fatalf("--poll-timeout %s exit = %d, want %d; stdout=%q stderr=%q", tc.flag, res.Code, client.ExitUsage, res.Stdout, res.Stderr)
			}
			if !strings.Contains(res.Stderr, tc.want) {
				t.Fatalf("--poll-timeout %s stderr = %q, want it to name %q", tc.flag, res.Stderr, tc.want)
			}
			if got := atomic.LoadInt32(&calls); got != 0 {
				t.Fatalf("the bus saw %d calls, want 0 — a refused poll timeout must NOT be clamped and sent", got)
			}
		})
	}
}

// TestCLIWatchDiagnosticsNeverReachStdout checks the property that makes this
// command pipeable at all: a retry notice goes to STDERR, and stdout stays a
// clean, parseable NDJSON stream.
//
// A one-line "bus unreachable, retrying" landing in the middle of the stream
// would break the very consumer the stream exists for.
func TestCLIWatchDiagnosticsNeverReachStdout(t *testing.T) {
	var polls int32
	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		// A 500 is a KindServer failure the transport does NOT absorb itself
		// (only 429/503 are retried down there), so the WATCH loop is the thing
		// that retries — which is what puts a notice on stderr.
		if atomic.AddInt32(&polls, 1) == 1 {
			stubWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "the bus fell over"})
			return
		}
		if r.URL.Query().Get("cursor") != "" {
			<-r.Context().Done()
			return
		}
		stubWriteJSON(w, http.StatusOK, client.Batch{
			Messages: []client.Message{stubMessage(1, "bus-x.other-1", "bus-x.agent-1", "after the fault")},
			Cursor:   "cursor-1",
		})
	})

	res := bus.run(t, "", false, false, "watch", "--poll-timeout", "5s", "--count", "1")
	if res.Code != client.ExitOK {
		t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Fatalf("the bus saw %d polls, want at least 2 — the retry never happened, so this proof is vacuous", polls)
	}
	if !strings.Contains(res.Stderr, "retrying in") {
		t.Fatalf("stderr = %q, want the retry notice — a bus outage must not be silent", res.Stderr)
	}

	// stdout must still be pure NDJSON: every line a bare JSON object.
	lines := strings.Split(strings.TrimSuffix(res.Stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want exactly 1 (the single message): %q", len(lines), res.Stdout)
	}
	for i, line := range lines {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("stdout line %d is not a JSON object — a diagnostic leaked into the stream: %v (%q)", i, err, line)
		}
	}
	if strings.Contains(res.Stdout, "retrying") || strings.Contains(res.Stdout, "agent-busctl watch:") {
		t.Fatalf("a diagnostic reached stdout: %q", res.Stdout)
	}
}

// TestCLIWatchSurfacesBodyHashMismatch exercises CLI-3-FU-HASHVERIFY THE WAY AN
// AGENT WOULD: through `agent-busctl watch`, not through the client package.
//
// The client-level test (client.TestCLIWatchRejectsBodyHashMismatch) proves the
// check fires. This one proves the agent on the other end can act on it: a
// documented exit code, and a message on stderr that names which side is at
// fault. That second half is the entire point of the task — an agent saw short
// bodies, blamed the bus, and the bus was innocent.
func TestCLIWatchSurfacesBodyHashMismatch(t *testing.T) {
	damaged := stubMessage(1, "bus-x.other-1", "bus-x.agent-1", "the original body")
	damaged.Body = damaged.Body[:3] // truncated after the bus sized and hashed it

	bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteWait {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusOK, map[string]interface{}{
			"messages": []client.Message{damaged},
			"cursor":   "cursor-1",
		})
	})

	res := bus.run(t, "", true, false, "watch", "--for", "2s")

	// KindServer -> exit 6. NOT exit 7: the bus did not understand and refuse
	// this request, it answered with a body inconsistent with its own metadata.
	if res.Code != client.ExitServer {
		t.Fatalf("watch over a damaged message exit = %d, want %d; stdout=%q stderr=%q",
			res.Code, client.ExitServer, res.Stdout, res.Stderr)
	}
	if strings.Contains(res.Stdout, "the original body"[:3]) && strings.Contains(res.Stdout, "bus-x.other-1") {
		t.Fatalf("the damaged body was rendered to stdout; it must never reach the consumer:\n%s", res.Stdout)
	}
	// NOTE the phrases below are deliberately SPECIFIC. An earlier version of
	// this test asserted the bare substring "bus", which CANNOT FAIL: every
	// diagnostic line is prefixed "agent-busctl: " and "bus" sits inside
	// "busctl". It passed against a remedy naming no side at all — a vacuous
	// assertion guarding the one thing the task exists to prove.
	for _, want := range []string{
		"size 17 but a body of 3 bytes",  // which field disagreed, and by how much
		"on the BUS side of this client", // WHICH SIDE is at fault
		"not in your handler",            // and which side is not
	} {
		if !strings.Contains(res.Stderr, want) {
			t.Fatalf("stderr does not contain %q — an agent must be able to tell WHICH SIDE is wrong:\n%s", want, res.Stderr)
		}
	}
	if !strings.Contains(res.Stderr, "truncated by the consumer, not by the bus") {
		t.Fatalf("stderr does not say what a PASSING check would have meant (that a short body is the consumer's doing); that is the diagnosis this check exists to make self-evident:\n%s", res.Stderr)
	}
	// The remedy must not advise a retry: the failure is fatal precisely because
	// re-reading the same position returns the same message.
	if strings.Contains(res.Stderr, "retry the read") {
		t.Fatalf("stderr advises retrying a failure that is marked fatal and will never clear:\n%s", res.Stderr)
	}
}
