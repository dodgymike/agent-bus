package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/client"
)

func watchCommand() command {
	return command{
		name:    "watch",
		summary: "stream messages addressed to you as they arrive",
		help: `busctl watch — stream messages addressed to you, as they arrive.

USAGE
  busctl watch [--json] [--replay] [--cursor <c>] [--limit N]
               [--poll-timeout <dur>] [--count N] [--for <dur>] [--no-cursor]

WHAT IT DOES
  Long-polls the bus and prints every message addressed to you — direct
  messages, and broadcasts sent by other agents after you enrolled — until you
  stop it. It never gives up on a transient failure: a bus restart or a network
  blip is retried with backoff, reported on stderr, and the stream continues.

OUTPUT — THREE MODES, chosen for you
  --json                      NDJSON.
  no --json, stdout is a PIPE NDJSON. A pipe is a machine.
  no --json, stdout is a TTY  a readable live feed.

  NDJSON is one compact JSON object per line, FLUSHED as each message arrives,
  with no envelope, no "ok" field and no array brackets — so a consumer acts on
  each message as it lands. A stream that only became parseable when the
  command exited would be useless, because this command is not meant to exit.

  Each record carries message_id, seq, from, broadcast, to, bus_path, sent_at,
  size, content_sha256, body and sometimes text:

    body   ALWAYS present, standard base64 — the authoritative, lossless form.
    text   the body as a string, present ONLY when it is valid UTF-8 with no
           control characters other than tab, newline and carriage return.
           Absent otherwise; a body is never rewritten to make it printable.

  So: ` + "`jq -r .text`" + ` for text traffic, ` + "`jq -r .body | base64 -d`" + ` for anything.

  Diagnostics — retry notices, the closing summary — go to stderr and never
  appear inside the stream.

AT-LEAST-ONCE: YOUR HANDLER MUST BE IDEMPOTENT
  Delivery is AT-LEAST-ONCE. Duplicates are the NORMAL steady state, not an
  error and not a bug: relaying between buses in a cyclic topology guarantees
  them. Deduplicate on message_id.

  The cursor advances only AFTER a whole batch has been handed to you. A watch
  killed mid-batch RE-DELIVERS that batch on restart. It never skips — that is
  the only safe direction, because advancing first would convert at-least-once
  into at-most-once and silently drop messages on any crash.

  A poll that times out with nothing is NORMAL, not an error. On a quiet bus it
  is the steady state; nothing is printed and the next poll starts at once.

WHERE IT STARTS, AND WHAT IT REMEMBERS
  By default the read position (the "cursor") is persisted per identity and bus
  in the credential store, so a restarted watch resumes where it left off.

  --cursor <c>    start at an explicit position (one a previous run printed).
  --replay        start at position 0 and re-read the whole RETAINED window
                  (1 day, or 1 GiB of messages, whichever binds first). With
                  the cursor still persisted, the position is overwritten as
                  the replay catches up.
  --no-cursor     do not persist anything — a throwaway tail. The next run
                  starts wherever the STORED cursor left off, which this run
                  did not move.

  A cursor that has fallen out of the retained window resumes at the oldest
  retained message. The messages in between are gone; that is what a retention
  window means.

STOPPING
  Ctrl-C or SIGTERM stops cleanly, exit 0. An interrupted tail is a finished
  tail, not a failure.

  --count N    stop after N messages have been printed.
  --for <dur>  stop after this much wall-clock time (an in-flight poll is cut
               short rather than overrunning).

  A BOUNDED watch (--count or --for) that ends having printed NOTHING exits 8,
  so ` + "`busctl watch --for 30s --count 1`" + ` can be used as "wait for one message"
  and the caller can branch on the timeout without parsing text. An UNBOUNDED
  watch stopped by a signal always exits 0, however many messages it saw.

FLAGS
  --limit N            messages per batch, 1-256. Omit it and the bus chooses.
  --poll-timeout <dur> how long each poll parks, default ` + client.DefaultPollTimeout.String() + `, ceiling ` + client.MaxPollTimeout.String() + `.
                       A longer value is REFUSED, not clamped, exactly as the
                       bus refuses it. It does not need to be long: a poll that
                       times out costs one round trip and loses nothing.

  --timeout (the global flag) does NOT bound a watch or strangle a long poll.
  It bounds the individual request/response calls underneath.

EXIT CODES
  0 stopped cleanly             5 the bus is unreachable and will not recover
  1 internal error                (a fatal 503: the bus cannot durably accept)
  2 bad usage                   6 the bus reported an error of its own
  3 no usable identity          7 the bus refused the request
  4 credential rejected         8 a bounded watch delivered nothing
`,
		run: runWatch,
	}
}

func runWatch(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("watch", env.g)
	var (
		replay      = fs.Bool("replay", false, "start at position 0 and re-read the retained window")
		cursor      = fs.String("cursor", "", "start at an explicit position")
		limit       = fs.Int("limit", 0, fmt.Sprintf("messages per batch, 1-%d (0 lets the bus choose)", client.MaxBatchLimit))
		pollTimeout = fs.Duration("poll-timeout", client.DefaultPollTimeout, "how long each poll parks")
		count       = fs.Int("count", 0, "stop after this many messages")
		forDur      = fs.Duration("for", 0, "stop after this much wall-clock time")
		noCursor    = fs.Bool("no-cursor", false, "do not persist the read position")
	)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("busctl watch", diagnostics, err)
	}
	if err := requireNoArgs("watch", fs.Args()); err != nil {
		return err
	}
	env.out.json = env.g.json

	// client.Watch refuses both of these too, and its message is good. This one
	// exists only to name the FLAG the caller typed rather than the option field
	// it maps to — and to do it before an identity and a session are paid for.
	if *pollTimeout <= 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "busctl watch",
			Message: "--poll-timeout must be positive, got " + pollTimeout.String(),
			Remedy:  "pass a positive duration, e.g. --poll-timeout " + client.DefaultPollTimeout.String(),
		}
	}
	if *pollTimeout > client.MaxPollTimeout {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "busctl watch",
			Message: "--poll-timeout " + pollTimeout.String() + " is above the bus's ceiling of " + client.MaxPollTimeout.String(),
			Remedy:  "use at most --poll-timeout " + client.MaxPollTimeout.String() + "; the bus refuses a longer poll rather than clamping it, and a poll that times out loses nothing",
		}
	}

	c, err := env.client()
	if err != nil {
		return err
	}

	// A pipe is a machine. Someone who redirected this into a file or a
	// consumer, and did not think to pass --json, wants records — not a live
	// feed frozen into a text file.
	ndjson := env.g.json || !env.stdoutIsTTY

	handle := func(m client.Message) error {
		if ndjson {
			if werr := env.out.Stream(newWatchRecord(m)); werr != nil {
				// Returning it stops the watch WITHOUT advancing the cursor, so
				// the batch is re-delivered next time. That is right: a message
				// that could not be written was not delivered.
				return &client.Error{
					Kind:    client.KindInternal,
					Op:      "watch",
					Message: "could not write a message to stdout: " + werr.Error(),
					Remedy:  "the consumer of this stream closed it or the destination is full",
					Err:     werr,
				}
			}
			return nil
		}
		writeHumanMessage(env.stdout, m)
		return nil
	}

	// ctx is passed through UNBOUNDED on purpose. It is the process's
	// signal-cancelled context; --timeout bounds individual calls inside the
	// client (and client.Read gives a long poll its own, longer deadline), so
	// wrapping a deadline around the whole watch here would kill a perfectly
	// healthy tail after 30 seconds.
	stats, werr := c.Watch(ctx, client.WatchOptions{
		Cursor:      *cursor,
		Replay:      *replay,
		Limit:       *limit,
		PollTimeout: *pollTimeout,
		Persist:     !*noCursor,
		Max:         *count,
		For:         *forDur,
		OnRetry: func(err error, delay time.Duration) {
			// STDERR, always. A retry notice on stdout would land in the middle
			// of a JSON stream and break the consumer this command exists for.
			fmt.Fprintf(env.stderr, "busctl watch: %s; retrying in %s\n", err, delay.Round(time.Millisecond))
		},
		// No OnPoll. A heartbeat printed on every timed-out poll would be noise
		// in a human feed and a foreign record in a machine stream.
	}, handle)
	if werr != nil {
		return werr
	}

	if !ndjson {
		fmt.Fprintf(env.stderr, "busctl watch: %d message(s) over %d poll(s); resume with --cursor %s\n",
			stats.Delivered, stats.Polls, stats.Cursor)
	}

	// A bounded watch that saw nothing is "nothing to report", not a failure —
	// it is how `--for 30s --count 1` says "no message arrived in time" to a
	// script that must not have to parse text. An unbounded watch is never
	// empty in this sense: it was stopped by a signal, and that is a success.
	if (*count > 0 || *forDur > 0) && stats.Delivered == 0 {
		return &client.Error{
			Kind:    client.KindEmpty,
			Op:      "watch",
			Message: "no messages arrived before the watch finished",
			Remedy:  "wait longer (--for), or check the sender is using your fully-qualified id from `busctl agents`",
		}
	}
	return nil
}

// watchRecord is the NDJSON shape of one message.
//
// It is defined HERE rather than in client/ because it is a RENDERING: the
// wire-faithful type is client.Message, and this adds one derived, presentation
// field (text) that an embedding agent does not need — it already has the
// bytes. Every other field is client.Message's, key for key, so the two shapes
// do not drift.
type watchRecord struct {
	MessageID     string   `json:"message_id"`
	Seq           uint64   `json:"seq"`
	From          string   `json:"from"`
	Broadcast     bool     `json:"broadcast"`
	To            []string `json:"to"`
	BusPath       []string `json:"bus_path"`
	SentAt        string   `json:"sent_at"`
	Size          int      `json:"size"`
	ContentSHA256 string   `json:"content_sha256"`

	// Body is the AUTHORITATIVE form: encoding/json renders []byte as standard
	// base64, so it is lossless for any bytes at all and is always present.
	Body []byte `json:"body"`

	// Text is a convenience for the common case, and is OMITTED rather than
	// mangled when the body is not plain text. A lossily-rewritten body would
	// be worse than no field: a consumer would have no way to tell that what it
	// read is not what was sent.
	Text string `json:"text,omitempty"`
}

func newWatchRecord(m client.Message) watchRecord {
	r := watchRecord{
		MessageID:     m.MessageID,
		Seq:           m.Seq,
		From:          m.From,
		Broadcast:     m.Broadcast,
		To:            m.To,
		BusPath:       m.BusPath,
		SentAt:        m.SentAt,
		Size:          m.Size,
		ContentSHA256: m.ContentSHA256,
		Body:          m.Body,
	}
	if s, ok := plainText(m.Body); ok {
		r.Text = s
	}
	return r
}

// plainText reports whether body is safe to expose as a JSON string, and
// returns it if so.
//
// "Safe" is: valid UTF-8, and no control characters other than tab, newline and
// carriage return — the three that legitimately appear in text. Everything else
// in C0, DEL and C1 disqualifies the whole body, because the point of `text` is
// that a consumer can print it, and one ESC in it makes that untrue.
//
// JSON itself would escape the bytes below 0x20, so this is not about the
// stream's validity; it is about what happens after `jq -r .text` strips the
// escaping and pipes the result at a terminal.
func plainText(body []byte) (string, bool) {
	if !utf8.Valid(body) {
		return "", false
	}
	for _, r := range string(body) {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return "", false
		}
	}
	return string(body), true
}

// writeHumanMessage renders one message for a terminal.
//
// # Everything printed here is neutralised first, and that is not optional
//
// The body is attacker-controlled bytes chosen by ANOTHER AGENT, and the sender
// id and timestamp are chosen by the bus. Rendered verbatim to a terminal, a
// body containing "\x1b[2K\r" erases the line just printed and repaints it with
// whatever the sender likes — so a message can forge the appearance of a
// message from someone else, or of a busctl status line. client/sanitize.go
// documents exactly this attack for server-supplied text.
//
// This duplicates client.safeText because that function is UNEXPORTED and this
// package must not reach into client/'s internals; exporting it would widen the
// client package's API for a purely presentational need. The duplication is
// confined to the HUMAN path — NDJSON needs none of it, since encoding/json
// escapes every byte below 0x20 already.
//
// It differs from safeText in one deliberate way: NEWLINES SURVIVE. A body
// legitimately contains them, and turning them into spaces would mangle every
// multi-line message. They are rendered as real line breaks with the
// continuation lines INDENTED, so a multi-line body can never be mistaken for
// several messages.
func writeHumanMessage(w io.Writer, m client.Message) {
	scope := "broadcast"
	if !m.Broadcast {
		scope = "→you"
	}
	header := fmt.Sprintf("%s %s %s", humanTime(m.SentAt), terminalSafe(m.From, false), scope)

	if !utf8.Valid(m.Body) {
		// Not text at all. A screenful of replacement characters helps nobody,
		// and the lossless form is one flag away. Note the test is UTF-8
		// VALIDITY, not the stricter plainText: a body that is real text with a
		// stray control byte in it is still worth reading, and terminalSafe
		// below is what makes reading it safe.
		fmt.Fprintf(w, "%s  <%d bytes, not text — re-run with --json and use `jq -r .body | base64 -d`>\n", header, len(m.Body))
		return
	}

	safe := terminalSafe(string(m.Body), true)
	lines := strings.Split(safe, "\n")
	if len(lines) == 1 {
		fmt.Fprintf(w, "%s  %s\n", header, lines[0])
		return
	}
	fmt.Fprintln(w, header)
	for _, line := range lines {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// terminalSafe replaces everything a terminal can act on with a space.
//
// C0 controls, DEL and C1 controls all go — C1 because a lone 0x9b is CSI on
// some terminals, which is as dangerous as ESC-[. Invalid UTF-8 becomes U+FFFD.
// Controls are REPLACED rather than dropped so a run of them cannot splice two
// words into one convincing token (a dropped ESC would turn "adm\x1bin" into
// "admin").
//
// keepNewlines is set only for a message BODY, where a newline is content. It
// never applies to an id or a timestamp: a line break in one of those is an
// attempt to forge a second line of output.
func terminalSafe(s string, keepNewlines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' && keepNewlines:
			b.WriteByte('\n')
		case r == utf8.RuneError:
			b.WriteRune('�')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// Tabs included: a tab in a body would break the indentation this
			// renderer uses to show that a continuation line is not a new
			// message.
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// humanTime renders the bus's timestamp as a local wall-clock time.
//
// A live feed is read by someone watching it now, so the second matters and the
// date does not. A timestamp the bus formatted some other way is passed through
// neutralised rather than dropped — it is information, and it is already known
// to be free of control characters (client.validateServerTimestamp).
func humanTime(sentAt string) string {
	t, err := time.Parse(time.RFC3339, sentAt)
	if err != nil {
		return terminalSafe(sentAt, false)
	}
	return t.Local().Format("15:04:05")
}
