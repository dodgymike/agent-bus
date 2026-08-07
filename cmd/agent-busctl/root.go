package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/client"
)

// flagErrHelp is flag.ErrHelp, aliased so a subcommand file does not have to
// import the flag package just to recognise -h.
var flagErrHelp = flag.ErrHelp

// globals are the flags every subcommand accepts.
//
// They are registered on the ROOT flag set and again on each subcommand's, so
// both `agent-busctl --json enrol …` and `agent-busctl enrol --json …` work. Agents write
// the second form and humans write either; refusing one of them would be a
// papercut with no upside.
type globals struct {
	bus string

	// busFingerprint is the certificate this client will accept from the bus.
	//
	// It is GLOBAL rather than a flag on `enrol` alone because it is needed
	// before an identity exists (enrol) AND by any command aimed at a bus the
	// store has no identity for. After a successful enrol it is stored with the
	// identity and does not need repeating — that is what makes the trusted
	// path the easy path (invariant 11).
	busFingerprint string

	identity string
	as       string
	json     bool
	timeout  time.Duration
}

// register adds the global flags to fs.
//
// Each default is the CURRENT value, not the zero value: flag.XxxVar assigns
// the default the moment it is registered, so registering with zero defaults
// on a subcommand's set would silently discard anything the root set had
// already parsed.
func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.bus, "bus", g.bus, "base URL of the bus (env "+client.EnvBusURL+")")
	fs.StringVar(&g.busFingerprint, "bus-fingerprint", g.busFingerprint,
		"SHA-256 of the bus's TLS certificate, 64 lowercase hex, from the invite (env "+client.EnvBusFingerprint+")")
	fs.StringVar(&g.identity, "identity", g.identity, "credential store DIRECTORY (env "+client.EnvIdentityDir+")")
	fs.StringVar(&g.as, "as", g.as, "act as this stored identity without changing the stored selection (env "+client.EnvAgentID+")")
	fs.BoolVar(&g.json, "json", g.json, "machine-readable JSON on stdout")
	fs.DurationVar(&g.timeout, "timeout", g.timeout, "bound one operation, e.g. 30s (env "+client.EnvTimeout+")")
}

// cliEnv is what a subcommand is handed: the resolved globals, the renderer,
// the standard streams, and the environment lookup (all injected so tests never
// mutate process state).
type cliEnv struct {
	g         *globals
	out       *output
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv func(string) (string, bool)

	// stdoutIsTTY and stdinIsTTY are the answers isTerminal gave for the real
	// process streams. They are carried rather than recomputed because the
	// streams above are ordinary io.Writers under test, and a subcommand that
	// reached for os.Stdout to ask the question would be asking about a
	// descriptor it is not writing to.
	//
	// Both are FALSE in every injected/test path, which is the safe default:
	// "not a terminal" means machine output and no interactive read, so nothing
	// can block waiting for a human who is not there.
	stdoutIsTTY bool
	stdinIsTTY  bool

	// lastClient is the most recent client built by client(), kept so run() can
	// drain its warnings after the command finishes. A subcommand that builds
	// no client leaves it nil.
	lastClient *client.Client
}

// client builds a configured client.Client from the globals and environment.
func (e *cliEnv) client() (*client.Client, error) {
	cfg := client.DefaultConfig()
	cfg.BusURL = e.g.bus
	cfg.BusFingerprint = e.g.busFingerprint
	cfg.IdentityDir = e.g.identity
	cfg.AgentID = e.g.as
	switch {
	case e.g.timeout > 0:
		cfg.Timeout = e.g.timeout
	case e.g.timeout < 0:
		// Rejected, not silently ignored. AGENT_BUS_TIMEOUT=-1s is already an
		// exit-2 usage error (client.Config.ApplyEnv), and a flag that means
		// the same thing must not quietly fall back to the default while the
		// env var refuses.
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      "agent-busctl",
			Message: "--timeout must be positive, got " + e.g.timeout.String(),
			Remedy:  "pass a positive duration such as --timeout 30s",
		}
	default:
		// Zero: leave it so ApplyEnv can supply AGENT_BUS_TIMEOUT, and
		// withDefaults fills in DefaultTimeout if the environment says nothing.
		cfg.Timeout = 0
	}
	cfg, err := cfg.ApplyEnv(e.lookupEnv)
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	// Surface anything the store had to repair. These go to STDERR, never
	// stdout, so a JSON consumer's document stays parseable — and they are
	// warnings rather than failures because refusing to run would not make an
	// already-exposed private key any less exposed.
	for _, w := range c.Store().Warnings() {
		fmt.Fprintf(e.stderr, "agent-busctl: WARNING: %s\n", w)
	}
	e.lastClient = c
	return c, nil
}

// command is one subcommand.
type command struct {
	name string

	// summary is the one-line description in `agent-busctl --help`.
	summary string

	// help is the full text for `agent-busctl help <name>` and `agent-busctl <name> -h`.
	// It is written to answer the question a reader actually has, not to
	// restate the flag list the flag package already prints.
	help string

	// run parses args (which exclude the command name) and does the work.
	run func(ctx context.Context, env *cliEnv, args []string) error
}

func commands() []command {
	return []command{
		enrolCommand(),
		whoamiCommand(),
		useCommand(),
		logoutCommand(),
		pinCommand(),
		clientCertCommand(),
		agentsCommand(),
		sendCommand(),
		broadcastCommand(),
		watchCommand(),
	}
}

func lookupCommand(name string) (command, bool) {
	for _, c := range commands() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// run is the whole CLI, injectable for tests: no globals, no os.Exit, no
// direct access to the process environment.
//
// It is runWithTTY with the three process-shaped inputs neutralised: an EMPTY
// stdin and "neither stream is a terminal". Empty rather than os.Stdin so that
// no test can ever block reading a terminal the test harness may or may not
// have handed it, and false/false because that is the machine-facing behaviour
// (NDJSON, no interactive read) which is the one worth having as the default.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	return runWithTTY(ctx, args, strings.NewReader(""), stdout, stderr, lookupEnv, false, false)
}

// runWithTTY is run with stdin and the terminal answers supplied explicitly.
// main uses it with the real process streams; a test uses it to drive the
// TTY-dependent behaviour of `watch` and `send` without owning a terminal.
func runWithTTY(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool), stdoutIsTTY, stdinIsTTY bool) int {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	g := &globals{}
	out := &output{stdout: stdout, stderr: stderr}

	rootFS := flag.NewFlagSet("agent-busctl", flag.ContinueOnError)
	var rootErrs bytes.Buffer
	rootFS.SetOutput(&rootErrs)
	rootFS.Usage = func() {}
	g.register(rootFS)

	if err := rootFS.Parse(args); err != nil {
		if err == flag.ErrHelp {
			writeRootHelp(stdout)
			return client.ExitOK
		}
		out.json = g.json
		return out.Fail(flagError("agent-busctl", &rootErrs, err))
	}
	// --json is honoured for the errors raised below. Note this only covers a
	// flag given BEFORE the command name; flag.Parse stops at the first
	// non-flag argument, so `agent-busctl bogus --json` never sees it here. The
	// error path after cmd.run re-reads it for the after-the-name case.
	out.json = g.json

	rest := rootFS.Args()
	if len(rest) == 0 {
		writeRootHelp(stdout)
		return client.ExitOK
	}

	name := rest[0]
	rest = rest[1:]

	if name == "help" {
		if len(rest) == 0 {
			writeRootHelp(stdout)
			return client.ExitOK
		}
		cmd, ok := lookupCommand(rest[0])
		if !ok {
			return out.Fail(unknownCommandError(rest[0]))
		}
		fmt.Fprint(stdout, cmd.help)
		return client.ExitOK
	}

	cmd, ok := lookupCommand(name)
	if !ok {
		return out.Fail(unknownCommandError(name))
	}

	env := &cliEnv{
		g: g, out: out,
		stdin: stdin, stdout: stdout, stderr: stderr,
		lookupEnv:   lookupEnv,
		stdoutIsTTY: stdoutIsTTY,
		stdinIsTTY:  stdinIsTTY,
	}
	// Drained AFTER the command, on both the success and the failure path.
	//
	// env.client() already prints the credential store's warnings, but those
	// are known at construction. The client's own warnings are not: the TLS
	// material is loaded lazily on the first pinned request, so "your private
	// key was world-readable and I tightened it" only exists once the command
	// has run. Draining only on success would hide it on exactly the runs where
	// something is already wrong.
	defer func() {
		if env.lastClient == nil {
			return
		}
		for _, w := range env.lastClient.Warnings() {
			fmt.Fprintf(stderr, "agent-busctl: WARNING: %s\n", w)
		}
	}()
	if err := cmd.run(ctx, env, rest); err != nil {
		if err == flag.ErrHelp {
			fmt.Fprint(stdout, cmd.help)
			return client.ExitOK
		}
		// Re-read --json here, not only inside the subcommand. A global flag
		// given AFTER the command name (`agent-busctl whoami --json --badflag`) is
		// parsed by the subcommand's flag set, which then fails, so the
		// subcommand never reached its own `out.json = g.json` line — and the
		// agent that asked for JSON got human text on the one path where it
		// most needs to parse the answer.
		out.json = g.json
		return out.Fail(err)
	}
	return client.ExitOK
}

// newCommandFlagSet builds a subcommand's flag set with the globals attached
// and its diagnostics captured rather than printed, so a parse failure becomes
// a classified *client.Error like every other failure.
func newCommandFlagSet(name string, g *globals) (*flag.FlagSet, *bytes.Buffer) {
	fs := flag.NewFlagSet("agent-busctl "+name, flag.ContinueOnError)
	buf := &bytes.Buffer{}
	fs.SetOutput(buf)
	fs.Usage = func() {}
	g.register(fs)
	return fs, buf
}

// flagError turns a flag-parsing failure into a usage error that carries the
// flag package's own diagnostic, which is more specific than anything this
// code could reconstruct.
func flagError(name string, diagnostics *bytes.Buffer, err error) error {
	msg := strings.TrimSpace(diagnostics.String())
	if msg == "" {
		msg = err.Error()
	}
	// The flag package repeats its message on several lines; the first is the
	// one that names the problem.
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return &client.Error{
		Kind:    client.KindUsage,
		Op:      name,
		Message: msg,
		Remedy:  "run `" + name + " --help`",
	}
}

func unknownCommandError(name string) error {
	known := make([]string, 0, 8)
	for _, c := range commands() {
		known = append(known, c.name)
	}
	sort.Strings(known)
	return &client.Error{
		Kind:    client.KindUsage,
		Op:      "agent-busctl",
		Message: fmt.Sprintf("unknown command %q", name),
		Remedy:  "known commands: " + strings.Join(known, ", ") + " — run `agent-busctl --help`",
	}
}

// requireNoArgs rejects positional arguments a command does not take, rather
// than ignoring them. A silently-ignored argument is how a caller comes to
// believe a command did something it did not.
func requireNoArgs(name string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return &client.Error{
		Kind:    client.KindUsage,
		Op:      "agent-busctl " + name,
		Message: fmt.Sprintf("unexpected argument %q", args[0]),
		Remedy:  "run `agent-busctl " + name + " --help`",
	}
}

func writeRootHelp(w io.Writer) {
	var b strings.Builder
	b.WriteString(`agent-busctl — the agent-bus client.

USAGE
  agent-busctl [flags] <command> [flags]

COMMANDS
`)
	cmds := commands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].name < cmds[j].name })
	for _, c := range cmds {
		// Widened from 10 to 12 when `client-cert` (11 characters) landed, for
		// the reason it was widened from 9 to 10 when `broadcast` did: at the
		// old width the longest name touched its own summary.
		fmt.Fprintf(&b, "  %-12s %s\n", c.name, c.summary)
	}
	b.WriteString(`  help         show help for a command

FLAGS (accepted before or after the command)
  --bus <url>       base URL of the bus                       (env ` + client.EnvBusURL + `)
  --bus-fingerprint <hex>
                    the bus's TLS certificate, as 64 lowercase
                    hex characters, from the invite           (env ` + client.EnvBusFingerprint + `)
  --identity <dir>  credential store DIRECTORY, not an agent  (env ` + client.EnvIdentityDir + `)
  --as <agent-id>   act as a stored identity for this command
                    only, without changing the selection      (env ` + client.EnvAgentID + `)
  --json            machine-readable JSON on stdout
  --timeout <dur>   bound one operation, default ` + client.DefaultTimeout.String() + `           (env ` + client.EnvTimeout + `)

EXIT CODES
  0  ok                        5  the bus could not be reached
  1  internal error            6  the bus reported an error of its own
  2  usage error               7  the bus refused the request
  3  no usable identity        8  nothing to report
  4  credential rejected

NOTES
  No agent-busctl command is ever interactive. Credentials come from the store or
  the environment, never from a prompt, because an agent shelling out has no
  terminal to answer one.

  ` + "`agent-busctl watch`" + ` streams NDJSON whenever stdout is not a terminal, one
  message per line, flushed as it arrives. Delivery is AT-LEAST-ONCE, so a
  handler must be idempotent on message_id: run ` + "`agent-busctl watch --help`" + ` for the
  cursor and re-delivery contract before you write one.

  Credentials live in a 0600 file under the credential store directory
  (default: the user's config directory + /agent-bus). Never in a repository.

  THE BUS'S CERTIFICATE IS PINNED. Bus certificates are self-signed and there
  is no certificate authority, so an https bus is refused unless you say which
  certificate to expect: --bus-fingerprint, from the invite. Pass it once at
  enrol and it is stored with the identity. If the bus later presents a
  different certificate the command FAILS and says so — that is either a
  rotation or an impostor, and they look identical from here. There is no
  flag that turns the check off.

  A ROTATION does not mean re-enrolling. A bus serves both certificates during
  a rollover; ` + "`agent-busctl pin add <new>`" + ` accepts the incoming one alongside the
  outgoing one, and ` + "`agent-busctl pin remove <old>`" + ` ends the rollover. Confirm the
  new fingerprint OUT OF BAND first — nothing is ever pinned automatically.

  Enrolment is becoming invite-only and the bus is becoming TLS-only; both
  are in flight. See CONTRACTS-CLI.md for what is stable today.
`)
	fmt.Fprint(w, b.String())
}
