package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dodgymike/agent-bus/client"
)

// conversationCommand is the `conversation` command group. It has two
// subcommands: `create` (CONV-CREATE-CLI) and `send` (CONV-SEND-BY-ID);
// membership change is a separate task and lands as a further subcommand here.
func conversationCommand() command {
	return command{
		name:    "conversation",
		summary: "create and manage server-tracked conversations",
		help: `agent-busctl conversation — create and manage server-tracked conversations.

USAGE
  agent-busctl conversation create --recipient <id> [--recipient <id> ...] [flags]
  agent-busctl conversation send <conversation-id> --body <text> [flags]

SUBCOMMANDS
  create   mint a new conversation with a recipient list and an optional name
  send     send one message to a conversation by its id; the bus resolves who
           the members are, so you do not track the participants yourself

WHAT A CONVERSATION IS
  A server-minted, server-tracked, multi-party object. The bus assigns the id
  (` + "`<bus-id>.<uuid>`" + `, invariant 1 — the server is authoritative on every
  id, never the client) and records the CREATOR as the authenticated identity
  making the request. You address the conversation later by that id instead of
  tracking every participant yourself.

  Run ` + "`agent-busctl conversation create --help`" + ` or
  ` + "`agent-busctl conversation send --help`" + ` for each subcommand's flags.

EXIT CODES
  0 ok                          5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage                   7 the bus refused it
  3 no usable identity          9 the bus has no route: it is older than this client
  4 credential rejected
`,
		run: runConversation,
	}
}

func runConversation(ctx context.Context, env *cliEnv, args []string) error {
	// Parse the globals (so `conversation --json create ...` works), then read
	// the subcommand. This mirrors `pin`. The subcommand's OWN flags are parsed
	// by the subcommand handler below.
	fs, diagnostics := newCommandFlagSet("conversation", env.g)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl conversation", diagnostics, err)
	}
	env.out.json = env.g.json

	rest := fs.Args()
	if len(rest) == 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "conversation",
			Message: "no subcommand",
			Remedy:  "run `agent-busctl conversation create --recipient <bus-id>.<agent-id>`",
		}
	}

	action := rest[0]
	operands := rest[1:]
	switch action {
	case "create":
		return runConversationCreate(ctx, env, operands)
	case "send":
		return runConversationSend(ctx, env, operands)
	default:
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "conversation",
			Message: fmt.Sprintf("unknown subcommand %q", action),
			Remedy:  "known subcommands: create, send",
		}
	}
}

// printedHelp is returned by a subcommand that has ALREADY written its own help
// text to stdout. root.go honours the ExitCode() interface by returning the
// code and rendering nothing further, so this yields exit 0 with no duplicate
// or contradicting output — the mechanism `ack`/`ack-status` use for their own
// exit codes.
type printedHelp struct{}

func (printedHelp) Error() string { return "help already printed" }
func (printedHelp) ExitCode() int { return client.ExitOK }

// recipientList accumulates a repeated --recipient flag. A repeatable flag is
// unambiguous for an agent building argv programmatically — no delimiter to
// escape, no interleaving of flags and positionals to reason about — which is
// why recipients are named this way rather than as positional arguments.
type recipientList []string

func (r *recipientList) String() string { return strings.Join(*r, ",") }

func (r *recipientList) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func conversationCreateCommandHelp() string {
	return `agent-busctl conversation create — mint a new conversation.

USAGE
  agent-busctl conversation create --recipient <id> [--recipient <id> ...] [--name <label>] [flags]

WHAT IT DOES
  Asks the bus to mint a conversation with the given recipients and optional
  name, and returns only once the bus has made it DURABLE (invariant 4). A
  success here means the conversation record is on disk.

  The bus mints the id and records the creator as YOUR authenticated identity;
  neither is a value you supply (invariant 1). Each recipient is a
  FULLY-QUALIFIED ` + "`<bus-id>.<agent-id>`" + ` (invariant 2); a bare name is refused
  by the bus. List enrolled agents with ` + "`agent-busctl agents`" + `.

FLAGS
  --recipient <id>         a recipient's fully-qualified id; repeat for each one
                           (at least one; at most 64)
  --name <label>           optional single-line label, at most 128 bytes; a name
                           with a newline or control character is refused, not
                           truncated
  --idempotency-key <key>  retry a specific earlier create under its own key

IDEMPOTENCY — HOW TO RETRY SAFELY
  Every create carries an idempotency key. Omit --idempotency-key and one fresh
  random key is minted for the whole invocation and reused across every internal
  transport retry, so a create that is retried inside agent-busctl can never
  become two conversations. The key is always printed back.

  Same key + SAME recipients and name = a legitimate retry. The bus returns the
  ORIGINAL conversation from its applied-key table, mints nothing, and the
  output says "replayed"; the exit code is 0.

  Same key + DIFFERENT recipients or name = a protocol violation. The bus
  answers 409 and does NOT drop the connection (invariant 10). Use a fresh key
  for a different conversation.

OUTPUT
  Human: the conversation id, creator, name, recipients, timestamp and key.
  --json: one object with conversation_id, creator, name, recipients,
  created_at, replayed, idempotency_key.

EXIT CODES
  0 created and durable         5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage: no recipient     7 the bus refused it (409: this key was used for
  3 no usable identity            a different conversation)
  4 credential rejected         9 the bus has no route: it is older than this client
`
}

func runConversationCreate(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("conversation create", env.g)
	var recipients recipientList
	var name, key string
	fs.Var(&recipients, "recipient", "a recipient's fully-qualified <bus-id>.<agent-id> (repeat for each)")
	fs.StringVar(&name, "name", "", "optional single-line label for the conversation")
	fs.StringVar(&key, "idempotency-key", "", "retry a specific earlier create under its own key")
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			// `conversation create --help` prints the create help, not the group's.
			// root.go would otherwise print the conversation GROUP help for a bare
			// flag.ErrHelp; returning a printedHelp (which carries exit 0 and
			// renders nothing) after printing keeps the create-specific text.
			fmt.Fprint(env.stdout, conversationCreateCommandHelp())
			return printedHelp{}
		}
		return flagError("agent-busctl conversation create", diagnostics, err)
	}
	env.out.json = env.g.json

	if leftover := fs.Args(); len(leftover) > 0 {
		// Recipients are named with --recipient, never positional: a positional
		// argument here is almost always a recipient the caller forgot to flag, and
		// silently ignoring it would create a conversation missing a member.
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "agent-busctl conversation create",
			Message: fmt.Sprintf("unexpected argument %q", leftover[0]),
			Remedy:  "name every recipient with --recipient <bus-id>.<agent-id>; there are no positional arguments",
		}
	}
	if len(recipients) == 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "agent-busctl conversation create",
			Message: "no recipients",
			Remedy:  "pass at least one --recipient <bus-id>.<agent-id>; list enrolled agents with `agent-busctl agents`",
		}
	}

	c, err := env.client()
	if err != nil {
		return err
	}
	res, err := c.CreateConversation(ctx, client.CreateConversationOptions{
		Recipients:     recipients,
		Name:           name,
		IdempotencyKey: key,
	})
	if err != nil {
		return err
	}
	return emitConversationResult(env, res)
}

// emitConversationResult renders a created (or replayed) conversation. Every
// field of client.ConversationResult is in the --json object, including the two
// the bus does not send in its body: replayed (a header) and idempotency_key.
func emitConversationResult(env *cliEnv, res client.ConversationResult) error {
	return env.out.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "created %s\n", res.ConversationID)
		fmt.Fprintf(w, "  creator    %s\n", res.Creator)
		if res.Name != "" {
			fmt.Fprintf(w, "  name       %s\n", res.Name)
		}
		fmt.Fprintf(w, "  recipients %s\n", strings.Join(res.Recipients, ", "))
		fmt.Fprintf(w, "  created_at %s\n", res.CreatedAt)
		fmt.Fprintf(w, "  key        %s\n", res.IdempotencyKey)
		if res.Replayed {
			// Calm, not alarming: this is idempotency working as invariant 10
			// intends, and the exit code is 0.
			fmt.Fprintf(w, "  note       replayed — the bus had already created this conversation under this key, so nothing was minted and this is the ORIGINAL record\n")
		}
	})
}

func conversationSendCommandHelp() string {
	return `agent-busctl conversation send — send one message to a conversation by id.

USAGE
  agent-busctl conversation send <conversation-id> --body <text> [flags]
  agent-busctl conversation send <conversation-id> [body] [flags]
  echo 'hello' | agent-busctl conversation send <conversation-id>

WHAT IT DOES
  Sends ONE message to a conversation, addressing it by the id the bus returned
  from ` + "`agent-busctl conversation create`" + `. The BUS resolves who the members are
  at send time and delivers to each of them — you do NOT enumerate or track the
  participants. Returns only once the bus has made the message DURABLE
  (invariant 4).

  Only a MEMBER of the conversation may send to it (the creator or one of the
  recipients). A conversation you are not a member of, and one that does not
  exist, both answer the same way, so a non-member cannot tell them apart.

  You do NOT receive your own message back — the bus excludes a sender from its
  own copy, exactly as for a direct message. Every OTHER member receives it.

  A conversation has at most 64 members. A message to a larger conversation is
  refused (its membership exceeds the durable recipient bound).

WHERE THE BODY COMES FROM
  Exactly ONE source, and it is an error to give two:

    --body <text>           the body as a single string argument
    a positional argument   agent-busctl conversation send <id> 'hello' (quote it)
    --file <path>           read the body from a file; '-' means stdin
    --stdin                 read the body from stdin
    none of them            stdin is read anyway when it is a pipe or a redirect

  A body is sent VERBATIM — every byte, including a trailing newline. An EMPTY
  body is refused locally, exit 2. The limit is 65536 bytes DECODED.

FLAGS
  --body <text>            the message body as a single string
  --file <path>            read the body from a file ('-' is stdin)
  --stdin                  read the body from stdin
  --idempotency-key <key>  retry a specific earlier send under its own key

IDEMPOTENCY — HOW TO RETRY SAFELY
  Every send carries an idempotency key. Omit --idempotency-key and one fresh
  random key is minted for the whole invocation and reused across both legs of
  the handshake and every internal transport retry, so a send that is retried
  inside agent-busctl can never become two messages. The key is always printed
  back.

  Same key + byte-identical body = a legitimate retry: the bus returns the
  ORIGINAL result, re-applies nothing, and the output says "replayed"; exit 0.
  Same key + DIFFERENT body = a protocol violation: the bus answers 409, rejects
  and logs it, and does NOT drop the connection (invariant 10). Use a fresh key
  for new content.

OUTPUT
  Human: the message id, sequence, recipients, timestamp, content hash and key.
  --json: one object with message_id, seq, from, broadcast, to, sent_at,
  content_sha256, replayed, idempotency_key, ok.

EXIT CODES
  0 accepted and durable        5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage: no conversation   7 the bus refused it (you are not a member, the
    id, no body, two body sources   conversation does not exist, or a 409: this
  3 no usable identity             key was used with different content)
  4 credential rejected         9 the bus has no route: it is older than this client
`
}

// runConversationSend sends one message to a conversation by id. The membership
// is resolved by the bus; this command never enumerates participants.
func runConversationSend(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("conversation send", env.g)
	var bodyFlag, file, key string
	var stdin bool
	fs.StringVar(&bodyFlag, "body", "", "the message body as a single string")
	fs.StringVar(&file, "file", "", "read the message body from this file ('-' is stdin)")
	fs.BoolVar(&stdin, "stdin", false, "read the message body from stdin")
	fs.StringVar(&key, "idempotency-key", "", "retry a specific earlier send under its own key")

	rest, err := parseWithPositionals(fs, args)
	if err != nil {
		if err == flagErrHelp {
			// `conversation send --help` prints the send help, not the group's.
			fmt.Fprint(env.stdout, conversationSendCommandHelp())
			return printedHelp{}
		}
		return flagError("agent-busctl conversation send", diagnostics, err)
	}
	env.out.json = env.g.json

	if len(rest) == 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "agent-busctl conversation send",
			Message: "no conversation id",
			Remedy:  "pass the conversation id as the first argument: `agent-busctl conversation send <conversation-id> --body '…'`; create one with `agent-busctl conversation create`",
		}
	}
	conversationID := rest[0]
	positional := rest[1:]

	body, err := resolveConversationSendBody(ctx, env, bodyFlag, positional, file, stdin)
	if err != nil {
		return err
	}

	c, err := env.client()
	if err != nil {
		return err
	}
	res, err := c.SendToConversation(ctx, client.ConversationSendOptions{
		ConversationID: conversationID,
		Body:           body,
		IdempotencyKey: key,
	})
	if err != nil {
		return err
	}
	return emitSendResult(env, res)
}

// resolveConversationSendBody resolves the body from exactly one source, failing
// if two are named. It mirrors bodySource.read but adds the --body flag, which
// `send` and `broadcast` do not carry, so it is kept local rather than pushed
// into the shared bodySource.
func resolveConversationSendBody(ctx context.Context, env *cliEnv, bodyFlag string, positional []string, file string, stdin bool) ([]byte, error) {
	const op = "agent-busctl conversation send"

	if len(positional) > 1 {
		// A body is bytes; silently concatenating argv would send something the
		// caller did not write.
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: fmt.Sprintf("unexpected argument %q after the message body", positional[1]),
			Remedy:  "quote the whole body as ONE argument, or use --body '…', or pipe it",
		}
	}

	named := make([]string, 0, 4)
	if bodyFlag != "" {
		named = append(named, "--body")
	}
	if len(positional) == 1 {
		named = append(named, "a positional argument")
	}
	if file != "" {
		named = append(named, "--file")
	}
	if stdin {
		named = append(named, "--stdin")
	}
	if len(named) > 1 {
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: "the message body was given twice: " + strings.Join(named, " and "),
			Remedy:  "give exactly one of: --body '…', a quoted positional body, --file <path>, or --stdin",
		}
	}

	var (
		body []byte
		err  error
	)
	switch {
	case bodyFlag != "":
		body = []byte(bodyFlag)
	case len(positional) == 1:
		body = []byte(positional[0])
	case file != "" && file != "-":
		body, err = readBodyFile(ctx, op, file)
	default:
		// --stdin, --file -, or nothing at all. "Nothing at all" reads stdin too,
		// which is what makes `echo hi | agent-busctl conversation send <id>` compose;
		// a real terminal is announced first so the command does not appear to hang.
		if env.stdinIsTTY {
			fmt.Fprintf(env.stderr, "agent-busctl: reading the message body from the terminal; end with Ctrl-D\n")
		}
		body, err = readBodyStream(ctx, op, env.stdin, "stdin")
	}
	if err != nil {
		return nil, err
	}

	if len(body) == 0 {
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      op,
			Message: "the message body is empty",
			Remedy:  "pass a non-empty body with --body '…', as a quoted argument, with --file <path>, or on stdin; an empty send is refused rather than delivered",
		}
	}
	return body, nil
}
