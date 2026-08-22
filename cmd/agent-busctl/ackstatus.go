package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dodgymike/agent-bus/client"
)

func ackStatusCommand() command {
	return command{
		name:    "ack-status",
		summary: "report what happened to a message you sent",
		help: `agent-busctl ack-status — report what happened to a message YOU sent.

USAGE
  agent-busctl ack-status <correlation-key> [--wait <dur>] [--json]

WHAT IT DOES
  Reads the bus's durable delivery record for one message and prints one row
  per recipient: the state, the reason it failed if it failed, and what
  authenticated that outcome.

  The correlation key is the message id the bus returned when you sent it —
  ` + "`agent-busctl send --json | jq -r .message_id`" + `. It is minted by the bus that
  accepted the message (invariant 1) and is bus-namespaced.

  CORRECTED 2026-08-21 (ACK-12). This used to end "...so it identifies the
  message across every hop it takes." It does not, today. Hub's
  recordAcceptance early-returns on relayed OR broadcast, so no lifecycle row
  exists for a relayed message or for a same-bus broadcast, and this command
  answers unknown for both. Only a same-bus DIRECT message is tracked.

  Mind the EXIT CODE difference from the ack subcommand, which carries the
  same notice: ack exits 8 on unknown, but THIS command exits 0 without
  --wait, because an unknown state is still a state it successfully reported.
  It exits 8 only when --wait ends with nothing to report. So do not treat a
  0 from this command as evidence the message is tracked -- read the state.
  Tracked as P0 7d564118 and P0 f423959c.
  When those land, delete this notice rather than leaving it to rot.

  UPDATED 2026-08-21 (ACK-5). THE RELAYED HALF ABOVE IS NOW FALSE, and it is
  the more harmful half to leave standing: it tells you a cross-bus message is
  untracked when it is now tracked end to end. Mind the subtlety — YOUR bus,
  the one that minted this key, ALWAYS held the row: recordAcceptance's
  early-return is about a message a bus merely CARRIED, and your own send is
  not that. What was missing was anything to SETTLE the row. A terminal outcome
  raised by a recipient on another bus now travels backwards one hop at a time
  along the path the message took and stops here, at the origin — the only bus
  holding a sender-visible row. A three-bus end-to-end run over the compiled
  CLI settles such a row to state ` + "`delivered`" + `, attested_by
  ` + "`recipient_signature_unverified`" + `, with settled_at stamped.

  THE BROADCAST HALF IS STILL TRUE: recordAcceptance still early-returns on
  broadcast, so a same-bus broadcast still has no row here and this command
  still answers unknown for it. That gap is tracked separately as
  ACK-BROADCAST-NO-LIFECYCLE-ROW.

  UPDATED 2026-08-22 (f423959c). Two sentences here are retracted, quoted so
  they are not read as current: "P0 7d564118 is CLOSED. P0 f423959c is STILL
  OPEN: ` + "`watch`" + ` shows a recipient only the LOCAL message id its own bus
  minted, so a recipient on another bus cannot learn this correlation key from
  the bus. Until that lands, send it to them out of band."

  DO NOT SEND IT OUT OF BAND. Every ` + "`watch`" + ` record now carries
  ` + "`correlation_key`" + `, and it is THE SAME STRING you pass here — the ORIGIN
  bus's id — however many buses the message crossed. That is what makes this key
  the one id both ends can name: you read it from
  ` + "`agent-busctl send --json | jq -r .message_id`" + ` on this bus, the recipient
  reads the identical value from
  ` + "`agent-busctl watch --json | jq -r .correlation_key`" + ` on theirs, and passes
  it to ` + "`agent-busctl ack`" + `.

  AND 7d564118 IS NOT "CLOSED". Its BEHAVIOUR landed with ACK-5, but its Spec
  Server record still read ` + "`todo`" + ` on 2026-08-22 when this was checked. The
  two are different facts and this notice conflated them.

  The exit-code paragraph above is unchanged and still applies. Delete this
  notice only once the broadcast gap has landed too — that is now the sole
  remaining trigger.

THE FIVE STATES
  accepted       committed and fsynced on your bus. It is durable; nobody has
                 acknowledged it yet.
  in_flight      at least one onward hop is owed to another bus.
  delivered      TERMINAL. The recipient APPLICATION acknowledged it.
  refused        TERMINAL. The recipient refused it; ` + "`class`" + ` says which of the
                 three recipient reasons.
  undeliverable  TERMINAL. This bus will never deliver it; ` + "`class`" + ` says why.

  Terminal is ABSORBING: a terminal state is never revisited, never reopened
  and never downgraded.

  A HOP ACK IS NOT A DELIVERY. Another bus taking responsibility for the next
  hop does NOT advance this state and is not reported here. "Another bus has
  it" and "an agent got it" are different facts, and this command reports only
  the second.

"unknown" IS FOUR ANSWERS AT ONCE, AND CANNOT BE NARROWED
  You will see state "unknown" when the key never existed, when the record was
  swept past its 24-hour retention window, when the key belongs to a DIFFERENT
  sender, and when the key is malformed. The bus answers all four identically
  and on purpose: an answer that distinguished them would confirm to anyone
  who guessed a key that the message exists. Do not write a script that tries
  to tell them apart.

  Only the original sender ever sees a row. There is no way to ask about
  somebody else's message, and asking is not an error — it is "unknown".

WAITING
  --wait <dur>   park on the bus until every row is TERMINAL, or until the
                 duration elapses (ceiling 5m, the same ceiling as any poll).
                 A wait that ends without settling is a SUCCESS reporting the
                 current state, not a failure.

  A wait on a key with nothing to report waits the full duration. That is
  deliberate: returning early would leak existence through timing.

REASON CLASSES ARE A CLOSED SET
  ` + "`class`" + ` is one of twelve fixed values and there is NO free-text reason
  anywhere — not here, not on the wire, not in the log. Nine are emitted by a
  bus about routing (no_route, no_such_recipient, hop_refused,
  hop_unauthenticated, loop_dropped, fanout_exceeded, horizon_expired,
  local_capacity, obligation_lost) and three by the recipient application
  (recipient_refused_policy, recipient_refused_undecodable,
  recipient_refused_not_addressed). A recipient can say THAT it refused, never
  in its own words WHY: a reason string in an append-only trail is a message
  body by another name.

attested_by IS A LABEL, NOT A PROOF
  peer_bus                        authenticated as a BUS by its client
                                  certificate. It says which bus spoke and
                                  nothing about any agent.
  recipient_signature_unverified  the recipient supplied a signature whose
                                  SHAPE was checked and whose authenticity was
                                  not, by anybody.

  There is no value meaning "verified" and this system cannot produce one. Do
  not present either label as proof of receipt to a third party.

OUTPUT
  Human: one block per recipient.
  --json: exactly one object — {"rows":[…],"ok":true} — where each row carries
  state and, when they apply, correlation_key, recipient, class, attested_by,
  accepted_at and settled_at. The unknown answer is {"rows":[{"state":
  "unknown"}],"ok":true}.

EXIT CODES
  0 reported a state: any state without --wait, and delivered or
    still-in-progress with it
  7 --wait settled on refused or undeliverable
  8 --wait ended and there is nothing to report (state unknown)

  1 internal error              5 the bus is unreachable
  2 bad usage                   6 the bus reported an error of its own
  3 no usable identity          9 the bus has no route for this request:
  4 credential rejected           it is older than this client

  The row data is printed BEFORE the exit code is decided, so exit 7 still
  tells you the class — under --json too, where the single result object is
  the only thing written to stdout.
`,
		run: runAckStatus,
	}
}

func runAckStatus(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("ack-status", env.g)
	wait := fs.Duration("wait", 0,
		"park until every row is terminal, up to this long (max "+client.MaxPollTimeout.String()+"); 0 answers immediately")
	// PARSED IN PASSES so a flag may follow the positional argument.
	//
	// The flag package stops at the first non-flag argument, so a single
	// fs.Parse would leave `ack-status <key> --json` with TWO leftover
	// arguments and reject the documented invocation — which is the one an
	// agent shelling out writes, because it reads left to right: what, then
	// how. Each pass consumes the flags it can, takes ONE positional, and
	// re-parses the remainder.
	rest := make([]string, 0, 1)
	for {
		if err := fs.Parse(args); err != nil {
			if err == flagErrHelp {
				return err
			}
			return flagError("agent-busctl ack-status", diagnostics, err)
		}
		tail := fs.Args()
		if len(tail) == 0 {
			break
		}
		rest = append(rest, tail[0])
		args = tail[1:]
	}
	if len(rest) != 1 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "ack-status",
			Message: fmt.Sprintf("expected exactly one correlation key, got %d arguments", len(rest)),
			Remedy:  "run `agent-busctl ack-status <correlation-key>`; the key is the message_id `agent-busctl send` returned",
		}
	}
	if *wait < 0 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "ack-status",
			Message: "--wait must not be negative, got " + wait.String(),
			Remedy:  "pass a positive duration such as --wait 30s, or omit --wait for an immediate answer",
		}
	}
	env.out.json = env.g.json

	c, err := env.client()
	if err != nil {
		return err
	}

	status, err := c.AckStatus(ctx, client.AckStatusOptions{
		CorrelationKey: rest[0],
		Wait:           *wait,
	})
	if err != nil {
		return err
	}

	// THE RESULT IS PRINTED FIRST, ALWAYS — before any exit code is decided.
	//
	// The §13.4 table asks for a NON-ZERO exit on outcomes that are not
	// failures of this command: a message that was refused was reported
	// perfectly well. If that were expressed as a returned error, the JSON mode
	// would print the failure envelope INSTEAD of the rows, and an agent that
	// branched on exit 7 would have been denied the one field it needs — the
	// class. So the object goes out here and ackStatusExit below carries only
	// the number.
	if err := env.out.Emit(status, func(w io.Writer) { writeAckStatus(w, status) }); err != nil {
		return err
	}
	return ackStatusExit(status, *wait > 0)
}

// ackStatusExit maps a reported status onto the exit code table of
// ACK-CONTRACT.md §13.4. NO NEW EXIT CODE IS MINTED: every value here is one
// client/errors.go already documents, because an agent's branch table is a
// contract and a new number in it is a breaking change.
//
//	Reported a state successfully (any state) | ExitOK (0)
//	--wait reached delivered                  | ExitOK (0)
//	--wait reached a negative terminal        | ExitRejected (7)
//	--wait and the state is unknown           | ExitEmpty (8)
//
// # WHY THE SAME STATE EXITS DIFFERENTLY WITH AND WITHOUT --wait
//
// It looks inconsistent and it is deliberate. WITHOUT --wait you asked for a
// snapshot, and the command succeeded by telling you what it is — including
// "unknown", which is a legitimate, final-shaped answer about a swept or
// unknown key. WITH --wait you asked to be told the OUTCOME, so the outcome
// becomes the exit status: a script can then `agent-busctl ack-status K --wait 60s ||
// handle-failure` and be right.
//
// A --wait that ends WITHOUT settling (still accepted or in_flight) is exit 0:
// nothing failed, the answer is simply "not yet", and a script that treated
// slowness as refusal would retry sends that are still perfectly alive.
func ackStatusExit(status client.AckStatus, waited bool) error {
	if !waited {
		return nil
	}
	switch {
	case status.Unknown():
		return ackStatusExitCode{code: client.ExitEmpty}
	case status.AnyNegative():
		return ackStatusExitCode{code: client.ExitRejected}
	default:
		return nil
	}
}

// ackStatusExitCode is an error that carries ONLY a process exit code: the
// command has already written its one result object, and root's error path must
// set the status without printing a second one.
//
// It is deliberately not a *client.Error. A client.Error is rendered — as a
// failure envelope on stdout under --json, or as two lines on stderr for a human
// — and neither is right here, because nothing failed: the command reported a
// state, and the state was a refusal. Rendering it would either replace the rows
// the caller needs or duplicate them.
type ackStatusExitCode struct{ code int }

func (e ackStatusExitCode) Error() string {
	// Reached only if some future caller logs it instead of honouring it, which
	// is why the text says what it is rather than pretending to be a failure.
	return "the delivery outcome sets exit code " + fmt.Sprint(e.code)
}

// ExitCode makes the carried code visible to root without a type assertion on a
// concrete type, so a second command can use the same mechanism later.
func (e ackStatusExitCode) ExitCode() int { return e.code }

// writeAckStatus renders the rows for a human: one block per recipient.
//
// Nothing here sanitises the strings — client.AckStatus has already REJECTED the
// whole response if any field carried anything a terminal could act on
// (client/sanitize.go), and refused a state spelling outside the closed set. Do
// not add a second, weaker check here; add it there if it is ever missing.
func writeAckStatus(w io.Writer, status client.AckStatus) {
	if status.Unknown() {
		fmt.Fprintln(w, "state:    unknown")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The bus has nothing to report for that key. It never existed, it was swept")
		fmt.Fprintln(w, "past the 24h retention window, it belongs to another sender, or it is")
		fmt.Fprintln(w, "malformed — the bus answers all four the same way on purpose.")
		return
	}
	for i, r := range status.Rows {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if r.Recipient != "" {
			fmt.Fprintf(w, "to:       %s\n", r.Recipient)
		}
		fmt.Fprintf(w, "state:    %s\n", r.State)
		if r.Class != "" {
			fmt.Fprintf(w, "class:    %s\n", r.Class)
		}
		if r.AttestedBy != "" {
			// Labelled, never presented as proof: see the help text and §6.3.
			fmt.Fprintf(w, "attested: %s\n", r.AttestedBy)
		}
		if r.AcceptedAt != "" {
			fmt.Fprintf(w, "accepted: %s\n", shortTimestamp(r.AcceptedAt))
		}
		if r.SettledAt != "" {
			fmt.Fprintf(w, "settled:  %s\n", shortTimestamp(r.SettledAt))
		}
	}
}
