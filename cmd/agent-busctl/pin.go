package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dodgymike/agent-bus/client"
)

// writePinnedCerts renders an identity's accept-set for a human.
//
// It is shared by `enrol`, `whoami` and `pin list` so the three cannot drift
// into three spellings of the same fact. label carries the leading indent and
// column so each caller keeps its own alignment.
//
// When more than one certificate is accepted it says SO, explicitly, and names
// the command that ends the rollover. A second pin is a temporary state by
// design — an accept-set that is never narrowed again becomes "accept every
// certificate this bus has ever had" — and an operator who cannot see that two
// are held has no reason to narrow it.
func writePinnedCerts(w io.Writer, label string, fingerprints []string) {
	if len(fingerprints) == 0 {
		return
	}
	blank := strings.Repeat(" ", len(label))
	for i, f := range fingerprints {
		lead := label
		if i > 0 {
			lead = blank
		}
		// Re-parsed before printing, not echoed. These strings come from the
		// credential FILE, and everything else this CLI prints from an
		// untrusted source goes through client's safeText — which is
		// unexported, so the equivalent here is the strict parser the value
		// should already satisfy. A hand-edited store holding an escape
		// sequence is the only way to reach the else branch, and quoting it
		// shows the operator what is actually in the file instead of letting
		// it drive their terminal.
		if parsed, err := client.ParseBusFingerprint(f); err == nil {
			fmt.Fprintf(w, "%s%s (pinned)\n", lead, parsed)
		} else {
			fmt.Fprintf(w, "%s%s (NOT A VALID FINGERPRINT — this identity's store is damaged)\n", lead, strconv.Quote(f))
		}
	}
	if len(fingerprints) > 1 {
		fmt.Fprintf(w, "%sROLLOVER: %d certificates are accepted. Retire the one the bus no longer\n", blank, len(fingerprints))
		fmt.Fprintf(w, "%sserves: agent-busctl pin remove <fingerprint>\n", blank)
	}
}

func pinCommand() command {
	return command{
		name:    "pin",
		summary: "list, add or retire the bus certificates an identity accepts",
		help: `agent-busctl pin — manage the bus certificates an identity will accept.

USAGE
  agent-busctl pin list
  agent-busctl pin add <fingerprint>
  agent-busctl pin remove <fingerprint>

WHY THIS EXISTS
  A bus certificate is PINNED: an identity accepts exactly the certificate(s)
  it was told to expect, and nothing else. That is what makes a substituted
  certificate visible, since bus certificates are self-signed and there is no
  certificate authority to fall back on.

  A bus rotating its certificate serves BOTH the outgoing and the incoming one
  for the duration of the rollover. This command is how an identity accepts
  both, so a routine rotation does not force you to re-enrol every agent — and
  so that having to re-enrol stays a signal that something is actually wrong.

  At most ` + fmt.Sprint(client.MaxBusPins) + ` certificates may be accepted at once: the one going out and
  the one coming in. A set that only ever grows would end up accepting every
  certificate the bus has ever had, including one that was compromised two
  rotations ago.

CONFIRM THE FINGERPRINT OUT OF BAND FIRST
  ` + "`pin add`" + ` is safe ONLY because you supply a value you have checked
  independently — the bus logs it as ` + "`bus_cert_fingerprint=…`" + ` at startup, and
  the invite carries it. NEVER paste in whatever a failing connection reported
  just to make the error go away: a legitimate rotation and an impostor look
  identical from here, and that is the entire point of the check.

  Nothing is ever pinned automatically. This client will not learn a
  certificate from a handshake, on any code path, by design.

WHICH IDENTITY
  The current one, or --as <agent-id> for a single command. --as must come
  BEFORE the action word: 'agent-busctl pin --as <id> add <hex>'.

IT DOES NOT REACH A PROCESS THAT IS ALREADY RUNNING
  Each agent-busctl process reads the accept-set once and keeps it. A
  long-running 'agent-busctl watch' therefore keeps the certificates it
  started with: 'pin add' will not un-wedge it, and — the half that matters —
  'pin remove' does not retire a certificate for it. Restart the watcher
  after either.

A ROLLOVER, START TO FINISH
  agent-busctl pin add <new>       # before or during the rollover
  ...                              # the bus finishes serving the old cert
  agent-busctl pin remove <old>    # back to one accepted certificate

EXIT CODES
  0 ok                          3 no identity enrolled or selected
  2 bad usage, an unknown fingerprint, or the maximum is already reached
`,
		run: runPin,
	}
}

// pinListResult is the JSON shape of every `pin` subcommand. All three answer
// with the identity's FULL accept-set rather than with a diff, so an agent
// scripting a rollover reads the resulting state instead of reconstructing it.
type pinListResult struct {
	// AgentID is the identity whose accept-set this is.
	AgentID string `json:"agent_id"`

	// BusURL is the bus those certificates belong to.
	BusURL string `json:"bus_url"`

	// BusFingerprints is the accept-set, in the order the pins were added.
	// Always present, and never null, so a consumer can range over it without a
	// nil check.
	BusFingerprints []string `json:"bus_fingerprints"`

	// MaxBusFingerprints is the cap. Reported so a caller can tell "one slot
	// free" from "add will be refused" without hard-coding the number.
	MaxBusFingerprints int `json:"max_bus_fingerprints"`
}

func newPinListResult(id client.Identity) pinListResult {
	fps := id.BusFingerprints
	if fps == nil {
		fps = []string{}
	}
	return pinListResult{
		AgentID:            id.AgentID,
		BusURL:             id.BusURL,
		BusFingerprints:    fps,
		MaxBusFingerprints: client.MaxBusPins,
	}
}

func runPin(_ context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("pin", env.g)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl pin", diagnostics, err)
	}
	env.out.json = env.g.json

	rest := fs.Args()
	if len(rest) == 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "pin",
			Message: "no subcommand",
			Remedy:  "run `agent-busctl pin list`, `agent-busctl pin add <fingerprint>` or `agent-busctl pin remove <fingerprint>`",
		}
	}

	action := rest[0]
	operands := rest[1:]

	switch action {
	case "list":
		if len(operands) != 0 {
			return pinUsage(fmt.Sprintf("unexpected argument %q", operands[0]), "run `agent-busctl pin list`")
		}
	case "add", "remove":
		if len(operands) != 1 {
			return pinUsage(
				fmt.Sprintf("`pin %s` takes exactly one fingerprint, got %d", action, len(operands)),
				"run `agent-busctl pin "+action+" <fingerprint>` with the 64 lowercase hex characters the bus logs as `bus_cert_fingerprint=…`")
		}
	default:
		return pinUsage(fmt.Sprintf("unknown subcommand %q", action), "known subcommands: add, list, remove")
	}

	c, err := env.client()
	if err != nil {
		return err
	}

	// c.Config().AgentID, not env.g.as: the flag is only half the selection —
	// AGENT_BUS_AGENT_ID is the other half, and CONTRACTS-CLI.md tells parallel
	// agents to prefer it. Reading the flag alone would silently pin against the
	// STORED current identity while the caller believed it had selected another.
	ref := c.Config().AgentID

	var id client.Identity
	switch action {
	case "list":
		id, err = c.Identity()
	case "add":
		id, err = c.AddBusPin(ref, operands[0])
	case "remove":
		id, err = c.RemoveBusPin(ref, operands[0])
	}
	if err != nil {
		return err
	}
	res := newPinListResult(id)

	// The CANONICAL fingerprint, re-derived from the parsed value, never the raw
	// operand. ParseBusFingerprint trims surrounding whitespace before it
	// validates, so a stray CR on argv survives validation and would be echoed
	// straight into the terminal — while every other string this CLI prints goes
	// through safeText. Printing the parsed value costs nothing and means the
	// operator is shown exactly what was stored.
	echo := ""
	if len(operands) == 1 {
		// It parsed already — AddBusPin/RemoveBusPin got this far — so the error
		// is unreachable and the canonical spelling is what String returns.
		if f, perr := client.ParseBusFingerprint(strings.TrimSpace(operands[0])); perr == nil {
			echo = f.String()
		}
	}

	return env.out.Emit(res, func(w io.Writer) {
		switch action {
		case "add":
			fmt.Fprintf(w, "%s now accepts %s\n", res.AgentID, echo)
		case "remove":
			fmt.Fprintf(w, "%s no longer accepts %s\n", res.AgentID, echo)
		}
		fmt.Fprintf(w, "%s\n", res.AgentID)
		fmt.Fprintf(w, "  bus      %s\n", res.BusURL)
		if len(res.BusFingerprints) == 0 {
			fmt.Fprintf(w, "  cert     (none — this identity enrolled over a plaintext bus)\n")
			return
		}
		writePinnedCerts(w, "  cert     ", res.BusFingerprints)
	})
}

func pinUsage(message, remedy string) error {
	return &client.Error{Kind: client.KindUsage, Op: "pin", Message: message, Remedy: remedy}
}
