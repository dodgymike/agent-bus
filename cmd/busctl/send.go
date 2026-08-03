package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dodgymike/agent-bus/client"
)

// The two write commands live in one file because they differ in exactly one
// thing — whether there is a recipient — and share everything that is actually
// hard: where the body comes from, refusing an ambiguous body, the local size
// bound, and the idempotency contract. Splitting them would duplicate all of
// that and let the two copies drift, which is how `send` and `broadcast` end up
// disagreeing about what an empty pipe means.

// idempotencyHelp is the paragraph both commands carry verbatim.
//
// It is spelled out in full on both because invariant 10's distinction is the
// one thing a caller MUST get right, and "see busctl send --help" is not a
// thing anyone reads before retrying a failed broadcast at 3am.
const idempotencyHelp = `IDEMPOTENCY — HOW TO RETRY SAFELY
  Every send carries an idempotency key. Omit --idempotency-key and one fresh
  random key is minted for the whole invocation and reused across every
  internal transport retry, so a send that is retried inside busctl can never
  become two messages. The key is always printed back, because it is the ONLY
  handle that makes a LATER retry the same logical send rather than a second
  message.

  Same key + byte-identical body = a legitimate retry. The bus answers from
  its applied-key table, re-applies nothing, and returns the ORIGINAL result;
  the output says "replayed" and the exit code is 0. This is the whole point:
  a client that lost the acknowledgement is meant to retry.

  Same key + DIFFERENT content = a protocol violation. The bus answers 409 AND
  DISCONNECTS. Retrying will not help; use a fresh key for new content.

  A key is remembered only for as long as the message it produced is retained
  (1 day, or until 1 GiB of messages push it out). A "retry" that arrives after
  that produces a SECOND message rather than being rejected — so a key is a
  retry handle for minutes and hours, not for days.`

// bodyHelp is the body-source paragraph both commands carry.
const bodyHelp = `WHERE THE BODY COMES FROM
  Exactly ONE source, and it is an error to give two:

    a positional argument   busctl send <to> 'hello'   (quote it — one word
                            per argument, and busctl will not join them)
    --file <path>           read the body from a file; '-' means stdin
    --stdin                 read the body from stdin
    none of them            stdin is read anyway when it is a pipe or a
                            redirect, so ` + "`echo hi | busctl send <to>`" + ` composes.
                            When stdin is a TERMINAL, busctl says so on stderr
                            and reads until Ctrl-D.

  Flags may appear before or after the positional arguments. A body that
  begins with a dash needs a "--" first, so it is not read as a flag:
  ` + "`busctl send <to> -- --this-is-the-body`" + `.

  A body is sent VERBATIM — every byte, including a trailing newline, whether
  it came from an argument, a file or a pipe. Nothing is trimmed, added or
  re-encoded. That is why the content_sha256 the bus reports matches the hash
  of the bytes you handed it; stripping a newline would corrupt binary and
  structured payloads and silently change that hash.

  An EMPTY body is refused locally, exit 2, rather than sent: an empty send is
  almost always an empty variable, an empty file or an empty pipe, the bus
  rejects it anyway, and failing here names the real cause.

  The limit is 65536 bytes DECODED. A larger body is refused locally, with its
  actual size, instead of being uploaded to earn a 413.`

func sendCommand() command {
	return command{
		name:    "send",
		summary: "send a direct message to one agent",
		help: `busctl send — send a direct message to one agent.

USAGE
  busctl send <to-agent-id> [body] [flags]
  echo 'hello' | busctl send <to-agent-id>

WHAT IT DOES
  Delivers one message to one agent and returns only once the bus has made it
  DURABLE (invariant 4: the bus acknowledges nothing it has not committed and
  fsynced). A success here means the message is on disk, not merely received.

  <to-agent-id> is the FULLY-QUALIFIED ` + "`<bus-id>.<agent-id>`" + `; a bare name is
  refused. List them with ` + "`busctl agents`" + `.

  A direct message is visible to the named recipient ONLY — not to you, not to
  anyone else. You will not see it come back on ` + "`busctl watch`" + `.

FLAGS
  --file <path>            read the body from a file; '-' means stdin
  --stdin                  read the body from stdin
  --idempotency-key <key>  retry a specific earlier send (see below)

` + bodyHelp + `

` + idempotencyHelp + `

OUTPUT
  Human: the message id, sequence, recipient, timestamp, content hash and the
  idempotency key.
  --json: one object with message_id, seq, from, broadcast, to, sent_at,
  content_sha256, replayed, idempotency_key, ok.

EXIT CODES
  0 accepted and durable        5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage: no recipient, no body, two body sources, body too large
  3 no usable identity          7 the bus refused it (unknown recipient, or a
  4 credential rejected           409: this key was used with other content)
`,
		run: runSend,
	}
}

func broadcastCommand() command {
	return command{
		name:    "broadcast",
		summary: "send a message to every other agent on the bus",
		help: `busctl broadcast — send one message to every other agent on the bus.

USAGE
  busctl broadcast [body] [flags]
  echo 'starting build' | busctl broadcast

WHAT IT DOES
  Delivers one message to every agent enrolled on the bus, and returns only
  once the bus has made it DURABLE (invariant 4).

WHO ACTUALLY RECEIVES IT — two surprises, both deliberate
  YOU DO NOT. The sender is excluded from its own broadcast. An agent polling
  its own bus does not want its own traffic echoed back into its loop, and it
  already holds the message id from this command's output. Do not use a
  broadcast to check that your own watcher works — it will never arrive.

  NEITHER DOES ANYONE WHO ENROLS LATER. A message sent before an agent's own
  enrolment is never delivered to it, whatever it was addressed to. So the
  order is JOIN, THEN LISTEN, THEN ANNOUNCE: an agent that broadcasts "I am
  here" before its peers have enrolled has told nobody.

FLAGS
  --file <path>            read the body from a file; '-' means stdin
  --stdin                  read the body from stdin
  --idempotency-key <key>  retry a specific earlier broadcast (see below)

` + bodyHelp + `

` + idempotencyHelp + `

OUTPUT
  Human: the message id, sequence, the scope (broadcast), timestamp, content
  hash and the idempotency key. The recipient list is EMPTY for a broadcast —
  the bus fans it out, it does not enumerate it back to you.
  --json: one object with message_id, seq, from, broadcast, to, sent_at,
  content_sha256, replayed, idempotency_key, ok.

EXIT CODES
  0 accepted and durable        5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage: no body, two body sources, body too large
  3 no usable identity          7 the bus refused it (or a 409: this key was
  4 credential rejected           already used with different content)
`,
		run: runBroadcast,
	}
}

func runSend(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics, src := newSendFlagSet("send", env.g)
	rest, err := parseWithPositionals(fs, args)
	if err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("busctl send", diagnostics, err)
	}
	env.out.json = env.g.json

	if len(rest) == 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "send",
			Message: "no recipient",
			Remedy:  "pass the fully-qualified `<bus-id>.<agent-id>`: `busctl send <to> 'message'`; list them with `busctl agents`",
		}
	}
	to := rest[0]
	if err := src.adoptPositional(rest[1:], "send"); err != nil {
		return err
	}

	body, err := src.read(env, "send")
	if err != nil {
		return err
	}

	c, err := env.client()
	if err != nil {
		return err
	}
	res, err := c.Send(ctx, client.SendOptions{To: to, Body: body, IdempotencyKey: src.idempotencyKey()})
	if err != nil {
		return err
	}
	return emitSendResult(env, res)
}

func runBroadcast(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics, src := newSendFlagSet("broadcast", env.g)
	rest, err := parseWithPositionals(fs, args)
	if err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("busctl broadcast", diagnostics, err)
	}
	env.out.json = env.g.json

	if err := src.adoptPositional(rest, "broadcast"); err != nil {
		return err
	}

	body, err := src.read(env, "broadcast")
	if err != nil {
		return err
	}

	c, err := env.client()
	if err != nil {
		return err
	}
	res, err := c.Broadcast(ctx, client.BroadcastOptions{Body: body, IdempotencyKey: src.idempotencyKey()})
	if err != nil {
		return err
	}
	return emitSendResult(env, res)
}

// bodySource collects the ways a body can be named and resolves exactly one.
type bodySource struct {
	file   string
	stdin  bool
	key    string
	arg    string
	hasArg bool
}

// parseWithPositionals parses fs allowing flags and positional arguments to be
// INTERLEAVED, and returns the positionals in the order they were given.
//
// # Why this exists, and what went wrong without it
//
// flag.FlagSet.Parse STOPS at the first non-flag argument and hands everything
// after it back as positionals. Every other busctl subcommand takes no
// positionals, so this never mattered — but `send` takes a recipient, and a
// plain Parse therefore made `busctl send <to> --json` treat "--json" as THE
// MESSAGE BODY and send it. Silently. A smoke test caught exactly that: a
// piped body was discarded and the literal text "--json" was delivered instead.
// Refusing ambiguity is this command's stated job, so quietly sending a flag as
// content is precisely the failure it must not have.
//
// The loop is the usual permutation: parse, take one positional, parse the
// rest, repeat. A literal "--" is honoured first and everything after it is
// positional VERBATIM, so a body that begins with a dash can still be sent as
// an argument: `busctl send <to> -- --not-a-flag`.
func parseWithPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	return append(positional, tail...), nil
}

// newSendFlagSet builds the flag set `send` and `broadcast` share.
func newSendFlagSet(name string, g *globals) (*flag.FlagSet, *bytes.Buffer, *bodySource) {
	fs, diagnostics := newCommandFlagSet(name, g)
	src := &bodySource{}
	fs.StringVar(&src.file, "file", "", "read the message body from this file ('-' is stdin)")
	fs.BoolVar(&src.stdin, "stdin", false, "read the message body from stdin")
	fs.StringVar(&src.key, "idempotency-key", "", "retry a specific earlier send under its own key")
	return fs, diagnostics, src
}

// idempotencyKey returns the caller's key, or "" to let the client mint one.
//
// NOTHING in this package generates a key. Minting it here would put the one
// value that makes a retry safe outside the importable package, where an agent
// EMBEDDING the client could not reach it — and, worse, a key minted per
// attempt is a second message rather than a retry. client.Send mints it once,
// before the payload is marshalled, and reuses it across every transport retry.
func (b *bodySource) idempotencyKey() string { return b.key }

// adoptPositional takes the leftover positional arguments as the body.
func (b *bodySource) adoptPositional(rest []string, op string) error {
	if len(rest) == 0 {
		return nil
	}
	if len(rest) > 1 {
		// Not joined with spaces. A body is bytes, and silently concatenating
		// argv would send something the caller did not write.
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "busctl " + op,
			Message: fmt.Sprintf("unexpected argument %q after the message body", rest[1]),
			Remedy:  "quote the whole body as ONE argument, or pipe it: `busctl " + op + " ... 'a b c'`",
		}
	}
	b.arg, b.hasArg = rest[0], true
	return nil
}

// read resolves the body from exactly one source, or fails saying which two it
// was given.
//
// The ambiguity check is the point of this function: picking one source when
// two were named would silently send the wrong bytes, and the caller would
// discover it by comparing content hashes long afterwards.
func (b *bodySource) read(env *cliEnv, op string) ([]byte, error) {
	op = "busctl " + op

	named := make([]string, 0, 3)
	if b.hasArg {
		named = append(named, "a positional argument")
	}
	if b.file != "" {
		named = append(named, "--file")
	}
	if b.stdin {
		named = append(named, "--stdin")
	}
	if len(named) > 1 {
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: "the message body was given twice: " + strings.Join(named, " and "),
			Remedy:  "give exactly one of: a quoted positional body, --file <path>, or --stdin",
		}
	}

	var (
		body []byte
		err  error
	)
	switch {
	case b.hasArg:
		body = []byte(b.arg)
	case b.file != "" && b.file != "-":
		body, err = readBodyFile(op, b.file)
	default:
		// --stdin, --file -, or nothing at all. "Nothing at all" reads stdin
		// too: that is what makes `echo hi | busctl send <to>` compose, and it
		// is safe because the only case where a human could be left waiting is
		// a real terminal, which is announced first.
		//
		// The notice covers the explicit --stdin case as well, not just the
		// implicit one. A command that appears to hang is the same papercut
		// whichever flag got you there, and the line costs a machine nothing:
		// a pipe is never a terminal, so it is never printed to one.
		if env.stdinIsTTY {
			fmt.Fprintf(env.stderr, "busctl: reading the message body from the terminal; end with Ctrl-D\n")
		}
		body, err = readBodyStream(op, env.stdin, "stdin")
	}
	if err != nil {
		return nil, err
	}

	if len(body) == 0 {
		// Refusing an empty send rather than delivering nothing. client.Send
		// refuses it too — this earlier check exists to name the SOURCE, which
		// is the part the caller has to fix.
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: "the message body is empty",
			Remedy:  "pass a non-empty body as a quoted argument, with --file <path>, or on stdin; an empty send is refused rather than delivered",
		}
	}
	return body, nil
}

// readBodyFile reads a file, refusing an oversized one by its real size.
func readBodyFile(op, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: "cannot read the message body from " + path,
			Remedy:  "check the path exists and is readable",
			Err:     err,
		}
	}
	defer f.Close()

	// Stat first so an oversized file is reported with its ACTUAL size rather
	// than "more than the limit". A regular file's size is known for free; a
	// pipe or a device named with --file is not, and falls through to the
	// stream bound below.
	if info, serr := f.Stat(); serr == nil && info.Mode().IsRegular() && info.Size() > client.MaxBodyBytes {
		return nil, oversizeError(op, fmt.Sprintf("%d bytes", info.Size()))
	}
	return readBodyStream(op, f, path)
}

// readBodyStream reads at most MaxBodyBytes+1 and refuses at +1.
//
// The extra byte is the whole trick: it is enough to KNOW the input is too
// large without reading a 64 MiB paste into memory to be told 413 by the bus.
func readBodyStream(op string, r io.Reader, what string) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r, client.MaxBodyBytes+1))
	if err != nil {
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: "cannot read the message body from " + what,
			Remedy:  "check the input is readable",
			Err:     err,
		}
	}
	if len(body) > client.MaxBodyBytes {
		return nil, oversizeError(op, fmt.Sprintf("more than %d bytes", client.MaxBodyBytes))
	}
	return body, nil
}

func oversizeError(op, size string) error {
	return &client.Error{
		Kind:    client.KindUsage,
		Op:      op,
		Message: fmt.Sprintf("the message body is %s; the bus accepts at most %d", size, client.MaxBodyBytes),
		Remedy:  "split the payload, compress it, or send a reference to it rather than its contents",
	}
}

// emitSendResult renders an accepted send.
//
// Every field of client.SendResult is in the --json object, including the two
// the bus does not send in its body: replayed (which arrives as a header) and
// idempotency_key (which is the key this send was applied under).
func emitSendResult(env *cliEnv, res client.SendResult) error {
	return env.out.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "sent %s\n", res.MessageID)
		fmt.Fprintf(w, "  seq        %d\n", res.Seq)
		if res.Broadcast {
			fmt.Fprintf(w, "  scope      broadcast — every agent enrolled before now, except you\n")
		} else {
			fmt.Fprintf(w, "  to         %s\n", strings.Join(res.To, ", "))
		}
		fmt.Fprintf(w, "  sent_at    %s\n", res.SentAt)
		if res.ContentSHA256 != "" {
			fmt.Fprintf(w, "  sha256     %s\n", res.ContentSHA256)
		}
		fmt.Fprintf(w, "  key        %s\n", res.IdempotencyKey)
		if res.Replayed {
			// Calm, not alarming: this is idempotency working exactly as
			// invariant 10 intends, and the exit code is 0.
			fmt.Fprintf(w, "  note       replayed — the bus had already accepted this message under this key, so nothing was re-applied and this is the ORIGINAL result\n")
		}
	})
}
