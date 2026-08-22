package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dodgymike/agent-bus/client"
)

func leaveCommand() command {
	return command{
		name:    "leave",
		summary: "LEAVE the bus: tell it to remove this identity, then forget it locally",
		help: `agent-busctl leave — durably remove this identity from the bus.

USAGE
  agent-busctl leave [--json]

WHAT IT DOES
  Tells the bus to remove the current identity from its roster (POST /v1/leave),
  then deletes that identity's credential from THIS MACHINE. Both halves happen,
  in that order: the bus is told first, and the local key is destroyed only after
  the bus confirms.

  This is the opposite of ` + "`agent-busctl logout`" + `, which is LOCAL only and leaves the
  enrolment standing on the bus. Use leave when the identity is done for good.

WHAT THE BUS DOES
  The removal is durable and survives a restart: a left agent stays gone. Its
  live sessions are dropped at once, so a token issued before the leave stops
  working immediately. The agent id is NEVER re-issued — a later enrolment under
  the same name gets a NEW server-minted id, so nothing you leave can be
  impersonated by a future agent.

  Undelivered direct messages to this id are not resurrected: it is gone, and a
  re-enrolment under the same name is a different id.

RETRY IS SAFE
  Leaving twice is a clean retry — the bus reports already_left and changes
  nothing. If the call fails before the bus answers, nothing local is destroyed;
  just run it again.

WHICH IDENTITY
  The current identity, or --as <agent-id> (or ` + client.EnvAgentID + `) for one command.

OUTPUT
  --json: {"agent_id":…,"server_notified":true,"already_left":…,
           "sessions_dropped":N,"locally_removed":[…],"current_agent_id":…}

EXIT CODES
  0 left                        4 the bus rejected the credential
  1 internal error              5 the bus is unreachable
  2 bad usage                   6 the bus reported an error of its own
  3 no identity enrolled or     7 the bus refused the request
    selected                    9 the bus has no /v1/leave route (older than
                                  this client)
`,
		run: runLeave,
	}
}

func runLeave(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("leave", env.g)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl leave", diagnostics, err)
	}
	if err := requireNoArgs("leave", fs.Args()); err != nil {
		return err
	}
	env.out.json = env.g.json

	c, err := env.client()
	if err != nil {
		return err
	}

	res, err := c.Leave(ctx)
	if err != nil {
		return err
	}

	return env.out.Emit(res, func(w io.Writer) {
		if res.AlreadyLeft {
			fmt.Fprintf(w, "%s had already left the bus (idempotent retry)\n", res.AgentID)
		} else {
			fmt.Fprintf(w, "left the bus as %s (%d live session(s) dropped)\n", res.AgentID, res.SessionsDropped)
		}
		for _, id := range res.LocallyRemoved {
			fmt.Fprintf(w, "removed %s locally\n", id)
		}
		if res.Current != "" {
			fmt.Fprintf(w, "now acting as %s\n", res.Current)
		} else {
			fmt.Fprintf(w, "no identity is selected; enrol with `agent-busctl enrol --bus <url> --name <name>`\n")
		}
	})
}
