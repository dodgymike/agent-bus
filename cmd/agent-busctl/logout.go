package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dodgymike/agent-bus/client"
)

func logoutCommand() command {
	return command{
		name:    "logout",
		summary: "forget an identity LOCALLY (the bus is not told)",
		help: `agent-busctl logout — delete a stored credential from THIS MACHINE.

USAGE
  agent-busctl logout [<agent-id|name>]
  agent-busctl logout --all

READ THIS FIRST — THE BUS IS NOT TOLD
  This is a LOCAL operation only. It does NOT tell the bus: the enrolment stays
  on the roster and any session that is already live stays live until it expires
  (at most an hour). Nothing is revoked. To durably remove the identity from the
  bus AND forget it locally, use ` + "`agent-busctl leave`" + ` instead.

  What this DOES do is destroy the private key, so this machine can never
  authenticate as that identity again. There is no undo and no export: a
  credential you delete is gone, and the only way back onto the bus is to
  enrol afresh under a new server-minted id.

  In --json output the field "server_notified" reports this honestly. For logout
  it is ALWAYS false — logout never tells the bus; ` + "`agent-busctl leave`" + ` is the
  command whose server_notified is true.

FLAGS
  --all   remove every stored identity.

WITH NO ARGUMENT
  Removes the CURRENT identity. The selection then falls back to the
  lowest-sorting identity that remains, deterministically, so parallel
  agents end up agreeing.

EXIT CODES
  0 removed                     3 no such identity, or none is selected
  2 bad usage                   8 nothing to report (the store was empty)
`,
		run: runLogout,
	}
}

func runLogout(_ context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("logout", env.g)
	all := fs.Bool("all", false, "remove every stored identity")
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl logout", diagnostics, err)
	}
	env.out.json = env.g.json

	rest := fs.Args()
	if len(rest) > 1 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "logout",
			Message: fmt.Sprintf("expected at most one identity, got %d", len(rest)),
			Remedy:  "run `agent-busctl logout <agent-id>` or `agent-busctl logout --all`",
		}
	}
	if *all && len(rest) == 1 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "logout",
			Message: "--all takes no identity argument",
			Remedy:  "run `agent-busctl logout --all`, or name a single identity without --all",
		}
	}

	c, err := env.client()
	if err != nil {
		return err
	}

	var res client.LogoutResult
	if *all {
		res, err = c.LogoutAll()
	} else {
		ref := ""
		if len(rest) == 1 {
			ref = rest[0]
		}
		res, err = c.Logout(ref)
	}
	if err != nil {
		return err
	}
	if len(res.Removed) == 0 {
		return &client.Error{
			Kind:    client.KindEmpty,
			Op:      "logout",
			Message: "no stored identities to remove",
			Remedy:  "nothing to do",
		}
	}

	return env.out.Emit(res, func(w io.Writer) {
		for _, id := range res.Removed {
			fmt.Fprintf(w, "removed %s (locally; the bus was NOT told)\n", id)
		}
		if res.Current != "" {
			fmt.Fprintf(w, "now acting as %s\n", res.Current)
		} else {
			fmt.Fprintf(w, "no identity is selected; enrol with `agent-busctl enrol --bus <url> --name <name>`\n")
		}
	})
}
