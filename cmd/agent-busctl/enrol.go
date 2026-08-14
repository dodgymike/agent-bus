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
  agent-busctl enrol --invite-file <path> --name <name> [flags]
  agent-busctl enrol --bus <url> --name <name> [flags]

WHAT IT DOES
  Generates TWO Ed25519 key pairs locally, sends ONLY their public halves,
  and receives the fully-qualified agent id the BUS minted,
  ` + "`<bus-id>.<agent-id>`" + `. You do not choose your own id: two agents may ask
  for the name "planner" and the bus tells them apart.

  The two keys do different jobs and are never the same key:
    - the AUTH key proves you TO THE BUS, in the session handshake;
    - the MESSAGING key proves you TO YOUR PEERS. Registering its public
      half here is what lets this bus attest your key to another bus, so
      agents on a peer bus can verify messages you signed. A bus cannot
      attest a key it never recorded.

  Registering it does not PROVE you hold the matching private key — nothing
  at enrolment does — so it establishes who a key is recorded against, not
  possession of it.

  Both private keys are written to the credential store (a 0600 file in a
  0700 directory) BEFORE the request is sent, and never leave this machine.
  The new identity becomes the current one unless --keep-current is given.

THE INVITE — THE NORMAL WAY IN
  An operator mints an invite with ` + "`agent-bus invite mint -json`" + ` and hands you
  the JSON blob. Save it to a file readable only by you and redeem it:

      chmod 0600 invite.json
      agent-busctl enrol --invite-file invite.json --name planner

  The blob carries the bus address AND the bus's certificate fingerprint, so
  --bus and --bus-fingerprint are UNNECESSARY with --invite-file. Passing one
  that DISAGREES with the invite is refused rather than silently preferred:
  one of the two is wrong about which bus this is, and that is worth stopping
  for. Passing the same value is merely redundant.

  It is a FILE, not a flag value, because the blob holds a bearer credential:
  anything on the command line is visible to every local user in the process
  list and lands in your shell history. ` + "`--invite-file -`" + ` reads it from stdin
  instead, which is refused when stdin is a terminal — there is no prompt.
  A file any other local user can read (any group or world permission bit) is
  refused too; the message names the chmod.

  An invite is SINGLE-USE, expiring and revocable. If the bus refuses it you
  cannot fix that by retrying — ask for a fresh one.

THE BUS'S CERTIFICATE — IT COMES FROM THE INVITE
  Bus certificates are SELF-SIGNED and there is no certificate authority, so
  this client refuses an https bus unless it is told which certificate to
  expect. --invite-file supplies it. Without an invite, pass it by hand:
  --bus-fingerprint <64 lowercase hex>, the value the invite carries
  (the bus also logs it at startup as bus_cert_fingerprint=…).

  There is deliberately NO trust-on-first-use. Accepting whatever certificate
  turns up on the first connection would mean the first connection is the one
  that cannot be checked, and that is the one an attacker picks.

  Enrol records the fingerprint with the identity, so every later command
  against this bus verifies it without being told again. If the bus ever
  presents a different certificate, those commands FAIL — either it was
  rotated or you are talking to something else, and they look the same from
  here.

  To re-pin after confirming a new fingerprint OUT OF BAND, do it in two
  steps, in this order:

      agent-busctl logout <agent-id>
      agent-busctl enrol --bus <url> --bus-fingerprint <new> --name <name>

  Enrolling without the logout first is REFUSED: the stored identity still
  pins the old certificate, and a flag that silently overrode it would turn a
  detected certificate substitution into an accepted one.

FLAGS
  --name <name>       the name to request. Lowercase [a-z0-9_-], 1-64 bytes,
                      starting with a letter or digit. Required.
  --invite-file       redeem the invite in this file. '-' reads it from stdin.
    <path>            The blob supplies the bus address and its certificate
                      fingerprint, so --bus and --bus-fingerprint are not
                      needed. The file must not be group- or world-readable.
                      There is deliberately no flag that takes the invite
                      itself: it holds a bearer credential, and argv is public.
  --bus-fingerprint   the bus's TLS certificate fingerprint. Not needed with
    <hex>             --invite-file. Required for an https bus otherwise;
                      refused for a plaintext one, which has no certificate to
                      check. Global flag, so it also works before the command
                      name.
  --idempotency-key   RESUME an earlier attempt. Omit it and a fresh random
                      key is generated, which is what you want unless you are
                      deliberately retrying.
  --keep-current      do not switch the current identity to the new one.

RETRIES — WHAT TO DO WHEN AN ENROLMENT DOES NOT ANSWER
  Re-run it with --idempotency-key <the key the error named>. That resumes
  the SAME enrolment: BOTH key pairs generated for the first attempt were
  saved before the request went out, so the bus sees a byte-identical payload
  and replays its original answer instead of enrolling a second agent.
  "replayed" in the output says that is what happened.

  RESUME WITH THE SAME INVITE. The bus's idempotency fingerprint covers the
  invite id, so a resume that presents a DIFFERENT invite file — or none — is
  a different payload, not a retry. agent-busctl refuses that here (exit 2)
  and KEEPS the stored key material: it is the only copy of the first
  attempt's private keys, and the bus may already hold their public halves.
  agent-busctl whoami --all names the invite each unfinished enrolment
  belongs to.

  Do NOT reuse a key for a different NAME either: agent-busctl refuses that
  locally too, because the bus treats it as a protocol violation — one it
  rejects and logs rather than disconnecting over — and the round trip teaches
  you nothing this refusal does not.

  A key reused against a DIFFERENT BUS is not refused, and is not a violation:
  keys are remembered per bus, so that is simply a new enrolment there.

EXIT CODES
  0 enrolled                    5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage                   7 the bus refused the request
  3 the credential store is unusable, or the invite file cannot be used
  4 the bus rejected the credential, invite included
  9 the bus has no route for this request: it is older than this client
`,
		run: runEnrol,
	}
}

// loadInvite resolves --invite-file into a validated invite, or nil when the
// flag was not given.
//
// '-' reads stdin, and is REFUSED when stdin is a terminal. That refusal is
// invariant 7: an agent shelling out must never meet a prompt, and a command
// that silently parked reading a terminal would hang a supervisor with no
// output to explain it. The same reasoning as `send`, which announces on stderr
// that it is reading a TTY — but an invite is a credential rather than a message
// body, so this refuses outright instead of inviting a human to paste one in.
func loadInvite(env *cliEnv, path string) (*client.Invite, error) {
	if path == "" {
		return nil, nil
	}
	if path != "-" {
		return client.LoadInviteFile(path)
	}
	if env.stdinIsTTY {
		return nil, &client.Error{
			Kind:    client.KindUsage,
			Op:      "enrol",
			Message: "--invite-file - reads the invite from stdin, but stdin is a terminal",
			Remedy:  "pipe the blob in (`agent-bus invite mint -json ... | agent-busctl enrol --invite-file - --name <name>`) or save it to a 0600 file and pass that path",
		}
	}
	return client.ParseInvite(env.stdin)
}

func runEnrol(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("enrol", env.g)
	var (
		name = fs.String("name", "", "name to request")
		// --invite-file, never --invite. There is deliberately NO flag that
		// takes the blob or the secret ITSELF: argv is world-readable in the
		// process list and is recorded in shell history, and the invite secret
		// is a bearer credential that enrols an agent onto the bus. The old
		// --invite flag is REMOVED rather than deprecated — it never did
		// anything but return exit 2, so nothing can depend on it, and an
		// unknown flag is still exit 2.
		inviteFile  = fs.String("invite-file", "", "redeem the invite in this file ('-' is stdin)")
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

	invite, err := loadInvite(env, *inviteFile)
	if err != nil {
		return err
	}

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
		Invite:         invite,
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
		// Echoed so the operator can compare it against the invite they were
		// given, at the one moment they still have it to hand.
		writePinnedCerts(w, "  cert       ", res.BusFingerprints)
		fmt.Fprintf(w, "  name       %s\n", res.Name)
		if res.InviteID != "" {
			// The ID, which is a name and safe to print. NEVER the secret.
			//
			// Through TerminalSafe even so. The invite is attacker-influenceable
			// input (invariant 11: whoever can substitute the blob points this
			// agent at a bus of their choosing), and a raw id of
			// "\x1b[2K\ragent-busctl: verified OK" would erase this line and
			// print a forged success line in its place. client.Invite.Validate
			// now refuses that charset outright, so this is the second of two
			// independent checks rather than the only one — which is what it has
			// to be for a line that renders untrusted text to a terminal.
			fmt.Fprintf(w, "  invite     %s\n", client.TerminalSafe(res.InviteID, false))
		}
		fmt.Fprintf(w, "  enrolled   %s\n", res.EnrolledAt)
		if res.Stored {
			fmt.Fprintf(w, "  credential %s\n", res.StorePath)
		}
		if res.Replayed {
			fmt.Fprintf(w, "  note       replayed: this idempotency key had already been applied, so this is the ORIGINAL enrolment, not a new one\n")
		}
	})
}
