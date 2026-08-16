package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dodgymike/agent-bus/client"
)

func sessionCommand() command {
	return command{
		name:    "session",
		summary: "show or discard the persisted session token",
		help: `agent-busctl session — manage the session token cached on THIS MACHINE.

USAGE
  agent-busctl session logout
  agent-busctl session logout --as <agent-id>

WHAT A PERSISTED SESSION IS
  With --persist-session (env ` + client.EnvPersistSession + `) the session token
  is written to the credential store, 0600, so the NEXT agent-busctl process
  reuses it instead of running a fresh handshake. Without that flag nothing is
  written and there is nothing here to remove.

  The flag exists because the bus caps ONE agent at 32 concurrent sessions,
  holds each for an hour, and evicts nothing. An agent that shells out per
  command burns one session per command, so a work rate above roughly one
  command every two minutes exhausts its OWN cap and is refused for up to an
  hour. Persisting the token collapses that to one session.

READ THIS FIRST — THE BUS IS NOT TOLD, AND THIS DOES NOT FREE A SESSION SLOT
  ` + "`session logout`" + ` deletes this machine's COPY of the token. It does NOT
  tell the bus. There is no server-side session-end route to call, so the bus
  keeps the session — and its slot against the per-agent cap — until it expires
  on its own, up to an hour away.

  So this command reduces the exposure of a bearer token at rest. It does
  NOTHING for the session count, and it is not a way out of a cap refusal.
  Adding a real server-side end-session route is filed as its own task; when it
  lands, the "server_notified" field below becomes true.

  What DOES clear the count today: waiting for the sessions to expire, or an
  operator restarting the bus (sessions are in-memory and do not survive it).

WHEN TO USE IT
  - the machine is shared, or was, and the token may have been read
  - you are handing the host to someone else
  - agent-busctl warned that the file's mode let other local users read it

FLAGS
  --as <agent-id>  discard that identity's session instead of the current one.

EXIT CODES
  0 a persisted session was removed
  2 bad usage
  3 no usable identity
  8 nothing to report — there was no persisted session to remove
`,
		run: runSessionLogout,
	}
}

func runSessionLogout(_ context.Context, env *cliEnv, args []string) error {
	// Pull the sub-subcommand out BEFORE flag parsing. Go's flag package stops
	// at the first non-flag operand, so leaving "logout" in place pushed every
	// flag after it into fs.Args() — which made the documented
	// `session logout --as <id>` and `session logout --json` both exit 2, while
	// the error's own remedy recommended the flag it had just rejected. Flags
	// may appear on either side of the word, as they do for every other
	// command here.
	var (
		sub  string
		rest = make([]string, 0, len(args))
	)
	for _, a := range args {
		if sub == "" && a != "" && !strings.HasPrefix(a, "-") {
			sub = a
			continue
		}
		rest = append(rest, a)
	}

	fs, diagnostics := newCommandFlagSet("session", env.g)
	if err := fs.Parse(rest); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl session", diagnostics, err)
	}
	env.out.json = env.g.json

	if sub == "" {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "session",
			Message: "session needs a subcommand",
			Remedy:  "run `agent-busctl session logout`",
		}
	}
	if sub != "logout" {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "session",
			Message: "unknown session subcommand " + sub,
			Remedy:  "the only subcommand is `logout`",
		}
	}
	if extra := fs.Args(); len(extra) > 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "session logout",
			Message: "session logout takes no arguments, got " + extra[0],
			Remedy:  "use --as <agent-id> to name a different identity",
		}
	}

	c, err := env.client()
	if err != nil {
		return err
	}

	// Resolve WHICH identity through the same path every other command uses,
	// so --as and the stored selection behave identically here. It also makes
	// exit 3 (no usable identity) fire before we touch the disk.
	id, err := c.Identity()
	if err != nil {
		return err
	}

	removed, err := c.ForgetPersistedSession(id.AgentID)
	if err != nil {
		return err
	}

	if !removed {
		// Return KindEmpty WITHOUT emitting first. Emitting then returning the
		// error rendered TWO json documents on stdout — {"ok":true...} followed
		// by {"ok":false...,"exit_code":8} — so json.load(stdout) failed for any
		// agent scripting a cleanup step. logout.go does it this way too.
		return &client.Error{
			Kind:    client.KindEmpty,
			Op:      "session logout",
			Message: "there was no persisted session for " + id.AgentID + " to remove",
			Remedy:  "sessions are only written when --persist-session (env " + client.EnvPersistSession + ") is set",
		}
	}

	return env.out.Emit(sessionLogoutResult{
		AgentID:        id.AgentID,
		Removed:        true,
		ServerNotified: false,
	}, func(w io.Writer) {
		fmt.Fprintf(w, "removed the persisted session for %s\n", id.AgentID)
		fmt.Fprintf(w, "  the bus was NOT told: it keeps this session, and its slot against\n")
		fmt.Fprintf(w, "  the per-agent cap, until it expires (up to an hour)\n")
	})
}

// sessionLogoutResult is the --json document. Its field names are a documented
// contract surface (CONTRACTS-CLI.md).
type sessionLogoutResult struct {
	AgentID string `json:"agent_id"`
	Removed bool   `json:"removed"`

	// ServerNotified reports, honestly, that the bus was not told. It mirrors
	// the same field on `logout` and is false until a real server-side
	// end-session route exists. An agent deciding whether it has freed a
	// session slot must read that here rather than infer it from prose.
	ServerNotified bool `json:"server_notified"`
}
