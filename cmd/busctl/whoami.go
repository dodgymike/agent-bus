package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dodgymike/agent-bus/client"
)

func whoamiCommand() command {
	return command{
		name:    "whoami",
		summary: "show the identity this shell acts as",
		help: `busctl whoami — show which identity commands will act as.

USAGE
  busctl whoami [--verify] [--all]

WHAT IT DOES
  Prints the current identity from the credential store. Nothing is sent to
  the bus unless you pass --verify.

FLAGS
  --all      list every enrolled identity, marking the current one with '*'.
  --verify   actually authenticate: ask the bus for a session token, sign it,
             and report when the session expires. This is the only way to
             tell a stored credential the bus still honours from one it has
             forgotten — sessions do not survive a bus restart, and neither
             does an enrolment if the bus was rebuilt with a fresh data
             directory.

WHICH IDENTITY
  --as <agent-id> (or ` + client.EnvAgentID + `) for one command, without changing
  anything; ` + "`busctl use`" + ` to change the stored selection. Parallel agents
  sharing a credential store should prefer --as.

EXIT CODES
  0 ok                          4 the bus rejected the credential (--verify)
  2 bad usage                   5 the bus is unreachable (--verify)
  3 no identity enrolled or selected
  8 nothing to report (--all found an empty store)
`,
		run: runWhoami,
	}
}

// whoamiResult is the JSON shape of `whoami`. It embeds Identity, so agent_id,
// bus_id, name, bus_url, public_key and enrolled_at appear at the top level.
type whoamiResult struct {
	client.Identity

	// IsCurrent reports whether this identity is the STORED selection, as
	// opposed to one chosen for this command alone by --as / AGENT_BUS_AGENT_ID.
	//
	// Named is_current rather than current because `whoami --all` and `logout`
	// carry a `current_agent_id` STRING; one key that is a bool in one
	// subcommand and a string in another makes `jq .current` unpredictable.
	IsCurrent bool `json:"is_current"`

	// Session is present only with --verify.
	Session *client.SessionInfo `json:"session,omitempty"`
}

// whoamiListResult is the JSON shape of `whoami --all`.
type whoamiListResult struct {
	Identities []client.Identity `json:"identities"`

	// CurrentAgentID is the stored selection, or "" when none is selected.
	CurrentAgentID string `json:"current_agent_id"`

	// Pending lists enrolments whose outcome is unknown — an attempt that was
	// interrupted before the bus answered. Each carries the idempotency key
	// that RESUMES it, which is the only handle to key material already on
	// disk. Omitted when there are none.
	Pending []client.PendingEnrolment `json:"pending,omitempty"`
}

func runWhoami(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("whoami", env.g)
	var (
		all    = fs.Bool("all", false, "list every enrolled identity")
		verify = fs.Bool("verify", false, "authenticate against the bus")
	)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("busctl whoami", diagnostics, err)
	}
	if err := requireNoArgs("whoami", fs.Args()); err != nil {
		return err
	}
	env.out.json = env.g.json

	c, err := env.client()
	if err != nil {
		return err
	}

	if *all {
		if *verify {
			return &client.Error{
				Kind:    client.KindUsage,
				Op:      "whoami",
				Message: "--all and --verify cannot be combined",
				Remedy:  "verify one identity at a time: `busctl whoami --verify --as <agent-id>`",
			}
		}
		return runWhoamiAll(env, c)
	}

	id, err := c.Identity()
	if err != nil {
		return err
	}
	// Ask the STORE which identity is selected. Deriving it from env.g.as was
	// wrong in both directions: that field holds the --as FLAG only, so
	// AGENT_BUS_AGENT_ID (which CONTRACTS-CLI.md tells parallel agents to
	// prefer) reported is_current=true for a non-current identity, and
	// `--as <the actual selection>` reported false.
	_, current, err := c.Identities()
	if err != nil {
		return err
	}
	res := whoamiResult{Identity: id, IsCurrent: id.AgentID == current}

	if *verify {
		info, verr := c.EnsureSession(ctx)
		if verr != nil {
			return verr
		}
		res.Session = &info
	}

	return env.out.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "%s\n", res.AgentID)
		fmt.Fprintf(w, "  bus      %s (%s)\n", res.BusID, res.BusURL)
		fmt.Fprintf(w, "  name     %s\n", res.Name)
		fmt.Fprintf(w, "  enrolled %s\n", res.EnrolledAt)
		if res.Session != nil {
			fmt.Fprintf(w, "  session  verified, expires %s (refresh at %s)\n",
				res.Session.ExpiresAt, res.Session.RefreshAt)
		}
	})
}

func runWhoamiAll(env *cliEnv, c *client.Client) error {
	ids, current, err := c.Identities()
	if err != nil {
		return err
	}
	pending, err := c.Store().ListPending()
	if err != nil {
		return err
	}
	if len(ids) == 0 && len(pending) == 0 {
		// "Nothing to report" is its own exit code so an agent can branch on
		// an empty store without parsing text.
		return &client.Error{
			Kind:    client.KindEmpty,
			Op:      "whoami",
			Message: "no identities are enrolled",
			Remedy:  "enrol with `busctl enrol --bus <url> --name <name>`",
		}
	}
	res := whoamiListResult{Identities: ids, CurrentAgentID: current, Pending: pending}
	return env.out.Emit(res, func(w io.Writer) {
		for _, id := range ids {
			marker := " "
			if id.AgentID == current {
				marker = "*"
			}
			fmt.Fprintf(w, "%s %-40s %s\n", marker, id.AgentID, id.BusURL)
		}
		if len(pending) > 0 {
			fmt.Fprintf(w, "\nunfinished enrolments — the bus may already have applied these:\n")
			for _, p := range pending {
				fmt.Fprintf(w, "  %s at %s, started %s\n", p.Name, p.BusURL, p.CreatedAt)
				fmt.Fprintf(w, "    resume: busctl enrol --bus %s --name %s --idempotency-key %s\n",
					p.BusURL, p.Name, p.IdempotencyKey)
			}
		}
	})
}
