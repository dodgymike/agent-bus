package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dodgymike/agent-bus/client"
)

func useCommand() command {
	return command{
		name:    "use",
		summary: "switch to another enrolled identity",
		help: `busctl use — choose which enrolled identity commands act as.

USAGE
  busctl use <agent-id|name>

WHAT IT DOES
  Records the selection in the credential store, so later commands act as
  that identity on the bus it was enrolled with. Switching identity switches
  bus too: each identity remembers where it enrolled.

  <agent-id|name> may be the fully-qualified '<bus-id>.<agent-id>', or a
  short name when exactly one enrolled identity has it. An ambiguous name is
  refused with the candidates listed rather than resolved by guessing — the
  wrong guess means acting as the wrong agent on the wrong bus.

BEFORE YOU USE THIS IN A PARALLEL AGENT
  This MUTATES SHARED STATE. Several agents sharing one credential store
  will fight over the selection. Use --as <agent-id> (or ` + client.EnvAgentID + `)
  instead: it selects for a single command and changes nothing on disk.

EXIT CODES
  0 switched                    3 no such identity, or the name is ambiguous
  2 bad usage
`,
		run: runUse,
	}
}

// useResult is the JSON shape of `use`. is_current is always true here — the
// command's whole job is to make it so — and is named to match `whoami`
// rather than the `current_agent_id` string that `whoami --all` and `logout`
// carry. See whoamiResult for why the two must not share a key name.
type useResult struct {
	client.Identity
	IsCurrent bool `json:"is_current"`
}

func runUse(_ context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("use", env.g)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("busctl use", diagnostics, err)
	}
	env.out.json = env.g.json

	rest := fs.Args()
	switch len(rest) {
	case 1:
	case 0:
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "use",
			Message: "no identity named",
			Remedy:  "run `busctl use <agent-id>`; list the enrolled identities with `busctl whoami --all`",
		}
	default:
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "use",
			Message: fmt.Sprintf("expected one identity, got %d", len(rest)),
			Remedy:  "run `busctl use <agent-id>`",
		}
	}

	c, err := env.client()
	if err != nil {
		return err
	}
	id, err := c.Use(rest[0])
	if err != nil {
		return err
	}

	res := useResult{Identity: id, IsCurrent: true}
	return env.out.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "now acting as %s\n", id.AgentID)
		fmt.Fprintf(w, "  bus %s (%s)\n", id.BusID, id.BusURL)
	})
}
