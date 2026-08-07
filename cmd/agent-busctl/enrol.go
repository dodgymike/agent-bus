package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dodgymike/agent-bus/client"
)

func enrolCommand() command {
	return command{
		name:    "enrol",
		summary: "enrol with a bus and store the credential",
		help: `agent-busctl enrol — join a bus and store the credential.

USAGE
  agent-busctl enrol --bus <url> --name <name> [flags]

WHAT IT DOES
  Generates an Ed25519 key pair locally, sends ONLY the public half, and
  receives the fully-qualified agent id the BUS minted, ` + "`<bus-id>.<agent-id>`" + `.
  You do not choose your own id: two agents may ask for the name "planner"
  and the bus tells them apart.

  The private key is written to the credential store (a 0600 file in a 0700
  directory) BEFORE the request is sent, and never leaves this machine. The
  new identity becomes the current one unless --keep-current is given.

FLAGS
  --name <name>       the name to request. Lowercase [a-z0-9_-], 1-64 bytes,
                      starting with a letter or digit. Required.
  --invite <blob>     RESERVED, not yet implemented. Enrolment is becoming
                      invite-only; the wire shape is still being settled, so
                      passing this fails immediately rather than guessing it.
  --idempotency-key   RESUME an earlier attempt. Omit it and a fresh random
                      key is generated, which is what you want unless you are
                      deliberately retrying.
  --keep-current      do not switch the current identity to the new one.

RETRIES — WHAT TO DO WHEN AN ENROLMENT DOES NOT ANSWER
  Re-run it with --idempotency-key <the key the error named>. That resumes
  the SAME enrolment: the key pair generated for the first attempt was saved
  before the request went out, so the bus sees a byte-identical payload and
  replays its original answer instead of enrolling a second agent.
  "replayed" in the output says that is what happened.

  Do NOT reuse a key for a different name or a different bus. The bus treats
  that as a protocol violation and disconnects the client, so agent-busctl refuses
  it locally first.

EXIT CODES
  0 enrolled                    5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage, or --invite      7 the bus refused the request
  3 the credential store is unusable
`,
		run: runEnrol,
	}
}

func runEnrol(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("enrol", env.g)
	var (
		name        = fs.String("name", "", "name to request")
		invite      = fs.String("invite", "", "invite blob (reserved, not implemented)")
		idemKey     = fs.String("idempotency-key", "", "reuse a key to retry an earlier attempt")
		keepCurrent = fs.Bool("keep-current", false, "do not switch the current identity")
	)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl enrol", diagnostics, err)
	}
	if err := requireNoArgs("enrol", fs.Args()); err != nil {
		return err
	}
	env.out.json = env.g.json

	// No bus-URL guard here on purpose: client.Enrol raises it, so an agent
	// EMBEDDING the client gets the same KindUsage/exit-2 classification the
	// CLI does instead of a different one. Logic that lives only in cmd/ is
	// logic an embedder cannot reach (invariant 7).
	c, err := env.client()
	if err != nil {
		return err
	}

	res, err := c.Enrol(ctx, client.EnrolOptions{
		Name:           *name,
		Invite:         *invite,
		IdempotencyKey: *idemKey,
		Save:           true,
		MakeCurrent:    !*keepCurrent,
	})
	if err != nil {
		return err
	}

	return env.out.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "enrolled as %s\n", res.AgentID)
		fmt.Fprintf(w, "  bus        %s (%s)\n", res.BusID, res.BusURL)
		fmt.Fprintf(w, "  name       %s\n", res.Name)
		fmt.Fprintf(w, "  enrolled   %s\n", res.EnrolledAt)
		if res.Stored {
			fmt.Fprintf(w, "  credential %s\n", res.StorePath)
		}
		if res.Replayed {
			fmt.Fprintf(w, "  note       replayed: this idempotency key had already been applied, so this is the ORIGINAL enrolment, not a new one\n")
		}
	})
}
