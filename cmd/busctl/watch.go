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

  Running diagnostics — retry notices, cursor-store warnings, the closing
  summary — go to STDERR and never appear inside the stream.

  ONE object does land on stdout: the FINAL failure object, when the command
  fails under --json. That includes the exit-8 "nothing arrived" outcome of a
  bounded watch, which is emitted as the LAST line of the stream. It is the
  same failure shape every other subcommand emits on stdout under --json, and
  keeping it there is what lets a consumer that captured only stdout still see
  why the stream ended. Branch on the presence of an "ok" field: a failure
  object always has one, and a message record never does.

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
  0 stopped cleanly             5 the bus could not be reached
  1 internal error              6 the bus reported an error of its own (incl. a
  2 bad usage                     fatal 503: it cannot durably accept messages)
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

	// --replay and --cursor are BOTH start positions, and client.Watch resolves
	// the explicit cursor first — so passing both silently makes --replay a
	// no-op. `send` already refuses two body sources loudly rather than picking
	// one; two start positions deserve the same treatment, for the same reason:
	// quietly honouring one of two contradictory instructions sends the caller
	// looking for the bug somewhere else entirely.
	if *replay && *cursor != "" {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "busctl watch",
			Message: "--replay and --cursor both name a start position",
			Remedy:  "pass --replay to re-read the whole retained window, or --cursor <c> to start at one position — not both",
		}
	}

	c, err := env.client()
	if err != nil {
		return err
	}

	// The cursor file is read INSIDE c.Watch, long after env.client() printed
	// the warnings the store had at open. Everything cursorstore.go records —
	// an unreadable cursors.json, an unparseable one, an unknown format
	// version, an oversize or malformed stored cursor — would therefore be
	// noted and never shown, and the operator would watch the whole retained
	// window replay with no explanation at all. Remember the high-water mark
	// now and drain whatever is added below, on EVERY return path.
	// A defer, so the ERROR path reports them too: a watch that stopped because
	// the store is broken is exactly when they matter most.
	warned := len(c.Store().Warnings())
	defer func() {
		// STDERR, never stdout: a warning inside the NDJSON stream would break
		// the consumer this command exists for.
		for _, w := range c.Store().Warnings()[warned:] {
			fmt.Fprintf(env.stderr, "busctl: WARNING: %s\n", w)
		}
	}()

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
		// The resume clause is omitted when there is no position to resume
		// FROM: a watch that never received anything leaves stats.Cursor empty,
		// and "resume with --cursor " (with nothing after it) is an instruction
		// that cannot be followed.
		resume := ""
		if stats.Cursor != "" {
			resume = "; resume with --cursor " + stats.Cursor
		}
		fmt.Fprintf(env.stderr, "busctl watch: %d message(s) over %d poll(s)%s\n",
			stats.Delivered, stats.Polls, resume)
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

	// TimestampMS is the SENDER's clock and Signature is the sender's detached
	// Ed25519 signature. They are carried HERE, on the stream, because they are
	// the only things a consumer can verify the message with — SIGN-6 requires
	// the receive path to hand the recipient the signature, and a stream that
	// dropped it would leave every consumer of this command structurally unable
	// to check anything, however good its key material.
	//
	// sent_at above is the BUS's clock and is NOT covered by the signature;
	// timestamp_ms is the sender's and IS. Do not conflate them: verifying
	// against the wrong one fails every time, and the reason is not obvious.
	TimestampMS int64  `json:"timestamp_ms"`
	Signature   string `json:"signature"`

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
		TimestampMS:   m.TimestampMS,
		Signature:     m.Signature,
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
		case isBidiOrInvisible(r):
			// Disqualifying rather than rewriting, for the same reason as any
			// other control: `jq -r .text` strips the JSON escaping and pipes
			// the result straight at a terminal, where a bidi override reorders
			// the line. See isBidiOrInvisible.
			return "", false
		}
	}
	return string(body), true
}

// isBidiOrInvisible reports whether r is a Unicode character that changes how
// text is DISPLAYED without being visible itself.
//
// None of these is a C0, DEL or C1 control, so the ordinary control checks miss
// every one of them — and each is chosen by another agent when it appears in a
// message body:
//
//	U+200B  zero-width space         invisible; splits a word that reads whole
//	U+200C  zero-width non-joiner    invisible
//	U+200D  zero-width joiner        invisible
//	U+200E  left-to-right mark       changes the direction of what follows
//	U+200F  right-to-left mark       changes the direction of what follows
//	U+202A..U+202E  the legacy bidi embedding/override controls — U+202E
//	                (RIGHT-TO-LEFT OVERRIDE) visually REVERSES the rest of the
//	                line, which is how a body or an id can be made to read as
//	                though it came from a different agent
//	U+2066..U+2069  the isolate forms of the same thing
//	U+FEFF  zero-width no-break space (BOM) — invisible mid-string
//
// A message body has no legitimate use for any of them. Real bidirectional text
// (Arabic, Hebrew) renders correctly from its own character properties; these
// codepoints exist to OVERRIDE that, which in a one-line feed is only ever a
// forgery primitive.
func isBidiOrInvisible(r rune) bool {
	switch {
	case r >= 0x200b && r <= 0x200f: // ZWSP, ZWNJ, ZWJ, LRM, RLM
		return true
	case r >= 0x202a && r <= 0x202e: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0xfeff: // BOM / zero-width no-break space
		return true
	}
	return false
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
// some terminals, which is as dangerous as ESC-[. So do the Unicode bidi and
// zero-width characters (isBidiOrInvisible), which are not controls at all and
// would otherwise pass through verbatim. Invalid UTF-8 becomes U+FFFD.
// Everything is REPLACED rather than dropped so a run of them cannot splice two
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
		case isBidiOrInvisible(r):
			// Not a control by any of the tests above, but it reorders or hides
			// what a human reads — the same forgery this function exists to
			// stop, spelled in Unicode instead of ANSI. Replaced with a space
			// for the same reason the controls are: dropping it would splice the
			// text either side of it into one convincing token.
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
