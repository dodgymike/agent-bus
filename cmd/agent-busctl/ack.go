package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/dodgymike/agent-bus/client"
)

func ackCommand() command {
	return command{
		name:    "ack",
		summary: "acknowledge a message you received (or refuse it)",
		help: `agent-busctl ack — acknowledge a message YOU received.

USAGE
  agent-busctl ack <message-id> [--refuse <class>] [--json]

WHAT IT DOES
  Tells the bus what your application did with one message it delivered to
  you, and settles that message's row for you as the recipient. The sender
  then sees it through ` + "`agent-busctl ack-status`" + `.

  The message id is the CORRELATION KEY, and the correlation key is the id the
  ORIGIN bus — the SENDER's bus — minted (invariant 1), fully bus-namespaced
  (invariant 2). For a message sent from YOUR OWN bus that is the
  ` + "`message_id`" + ` the message arrived with,
  ` + "`agent-busctl watch --json | jq -r .message_id`" + `. For a message RELAYED
  to you it is NOT: your bus minted a SECOND id for its own local copy, ` + "`watch`" + `
  prints that LOCAL id, and this route refuses it.

  CORRECTED 2026-08-21 (ACK-12). This paragraph used to end "...so it
  identifies the message across every hop it took to reach you." THAT IS NOT
  TRUE TODAY and it is the dangerous direction to be wrong in: it promised
  cross-hop correlation that the bus does not yet perform, so an agent acting
  on it would build a correlation scheme on a value that does not correlate.
  What actually works is a same-bus DIRECT message, and only that. Hub's
  recordAcceptance early-returns on relayed OR broadcast, so a relayed message
  and a same-bus BROADCAST both open no lifecycle row and both answer exit 8
  unknown. (This notice itself first said "the SAME-BUS case", which overclaimed
  in the very direction it exists to correct: broadcast is same-bus and still
  does not work.) On top of that, watch emits only the LOCAL message_id, never the origin id the
  correlation key is built from, so after a hop you cannot even name the
  message you are being asked to acknowledge. Tracked as P0 7d564118
  (destination row) and P0 f423959c (watch correlation key). When those land,
  delete this notice — do not leave it standing once it is stale.

  UPDATED 2026-08-21 (ACK-5). PART of the notice above has landed and part has
  not, so it stays — corrected beside itself rather than deleted, because a
  half-true notice reads as freshly checked.

  WHAT LANDED — the BEHAVIOUR P0 7d564118 names. (Its task record was still
  open in the Spec Server when this was written, 2026-08-21; do not read this
  as the task being closed.) A message RELAYED to this bus CAN now be
  acknowledged here, but ONLY under the ORIGIN bus's message id. This bus
  authorizes you from the relayed message it still retains, writes NO lifecycle
  row of its own, and carries your outcome one hop back along the path the
  message took, stopping at the ORIGIN bus — which holds the only
  sender-visible row. So "a relayed message ... opens no lifecycle row and
  answers exit 8 unknown" above — and with it "a same-bus DIRECT message, and
  only that" — is now FALSE under the origin id, and still TRUE under the
  local id.

  WHAT DID NOT LAND — P0 f423959c, and it is the trap. The id ` + "`watch`" + ` prints is
  still the LOCAL one, and this route REFUSES a correlation key whose bus half
  is this bus: the same uniform exit 8 unknown, with nothing recorded anywhere.
  There is still no way to obtain the origin id from ` + "`watch`" + `, so today it has
  to reach you OUT OF BAND — the sender captures it from
  ` + "`agent-busctl send --json | jq -r .message_id`" + ` and tells you.

  BROADCAST IS UNTOUCHED. recordAcceptance still early-returns on broadcast, so
  a same-bus BROADCAST still opens no lifecycle row and still answers exit 8
  unknown. That half is tracked separately as ACK-BROADCAST-NO-LIFECYCLE-ROW.

  TWO EXIT CODES CARRY MORE ON THE RELAYED PATH than the table below says on
  its own. Exit 6 is the backward hop failing: the bus answers 503 with
  Retry-After, this acknowledgement recorded nothing, and the identical retry
  is the right FIRST response — but it is NOT guaranteed to succeed, so back
  off and give up after a bounded number of attempts rather than looping.
  CORRECTED 2026-08-21 (ACK-5): this paragraph called the retry "the correct
  remedy" and set no bound. The same 503 is returned when the hop above
  FINALLY refused with a 409 — a swept row, a recipient the sender never
  addressed, or a conflicting terminal already standing at the origin — and
  none of those ever clear. Transient and permanent are byte-identical here
  BY DESIGN: no bus echoes another bus's verdict back down the chain, so the
  bus cannot tell you which one you are in. Exit 7 ALSO means this bus has no
  back-propagation wired at all (501: permanent, do not retry), as well as
  "already terminal with a different outcome". The message text tells those
  apart, so do not branch on the number alone.

  Delete this notice only once f423959c AND the broadcast gap have landed too.

  IT IS THE MESSAGE ID, NOT THE SEQUENCE. ` + "`seq`" + ` is identity and a delivery
  position is a position; this is correlation. They are three different
  numbers and passing the wrong one is refused locally, before anything is
  signed.

READING A MESSAGE IS NOT RECEIPT
  The bus does not acknowledge on your behalf and never will. Delivery to
  your inbox is a TRANSPORT fact; ` + "`delivered`" + ` is an APPLICATION fact, and only
  you can establish it. Run this when your application has actually taken
  responsibility for the message — not when it read it off the wire.

DELIVERED IS THE DEFAULT; REFUSING IS EXPLICIT
  agent-busctl ack bus-x-7                                    -> delivered
  agent-busctl ack bus-x-7 --refuse recipient_refused_policy  -> refused

  --refuse takes one of THREE classes, and only these three:
    recipient_refused_policy         your policy says no
    recipient_refused_undecodable    you cannot decode the body
    recipient_refused_not_addressed  it is not addressed to you as you
                                     understand your own addressing

  You say THAT you refused, never in your own words WHY. There is no
  free-text reason here, on the wire, or in the log: a reason string in an
  append-only trail is a message body by another name.

  ` + "`--refuse`" + ` WITH AN EMPTY VALUE IS AN ERROR (exit 2), NOT ` + "`delivered`" + `.
  If you write ` + "`agent-busctl ack \"$ID\" --refuse \"$CLASS\"`" + ` and $CLASS is
  unset, the command REFUSES rather than acknowledging delivery on your
  behalf — a terminal outcome is absorbing and cannot be taken back.

  There is NO ` + "`undeliverable`" + ` option and there must never be one. That is a
  claim a BUS makes about its own routing — "this will never be delivered" —
  and a recipient has no standing to sign it.

WHAT IS SIGNED, AND WHAT IT IS WORTH
  This command signs (context, message id, YOUR OWN fully-qualified id,
  outcome, class, your clock) with your MESSAGING key — the key that proves
  you to your PEERS, not the auth key that proves you to your bus — and sends
  the 64-byte detached signature with the frame.

  EVERY BUS CHECKS ITS SHAPE ONLY AND NO BUS VERIFIES IT. Nothing carries your
  messaging public key back to the sender either, so today this signature is
  END TO END UNVERIFIABLE BY ANYONE: the sender sees the label
  ` + "`recipient_signature_unverified`" + ` and that is exactly what it means. It is
  signed anyway so the binding is in the durable record from day one. Do not
  present it to a third party as proof that you received anything.

RE-ACKNOWLEDGING IS SAFE; CHANGING YOUR MIND IS NOT
  The first terminal outcome for a message stands forever — terminal is
  ABSORBING. Sending the SAME outcome again is a legitimate retry: it is
  accepted, ` + "`duplicate`" + ` is true, nothing is re-applied and nobody is
  disconnected. Sending a DIFFERENT outcome for a message you already settled
  is refused with exit 7 and NOTHING is written; retrying cannot change that.

  BOTH SENTENCES ABOVE DESCRIBE A MESSAGE YOUR OWN BUS ORIGINATED. Corrected
  in place 2026-08-21 (ACK-5): a RELAYED message differs on both counts,
  because the row lives at the ORIGIN bus and your bus keeps none — it only
  carries your outcome one hop back toward it.

    ` + "`duplicate`" + ` IS ALWAYS false for a relayed message, even when you
    re-acknowledge: there is no record here for a retry to be a duplicate OF,
    and the duplicate is absorbed at the origin, where the record is. Never
    read ` + "`duplicate:false`" + ` as proof this is your first acknowledgement.

    A DIFFERENT OUTCOME IS NOT exit 7 here. The origin answers 409 and no bus
    turns that into a 4xx for you, so what you get depends on how many buses
    the message crossed — and you cannot tell which case you are in:
      - your bus peers DIRECTLY with the origin: exit 6 (503), every time and
        for ever. It is NOT transient and no amount of retrying clears it.
      - an INTERMEDIATE bus sits in between: exit 0, accepted true, and the
        state printed is the outcome YOU just asserted — while the origin
        still holds the FIRST one. The intermediate absorbs the origin's 409
        deliberately (DECISIONS.md, 2026-08-21, ACK-5).
    So exit 0 on a relayed message means "the next hop back took it", not
    "the origin recorded exactly this".

"unknown" IS FOUR ANSWERS AT ONCE, AND CANNOT BE NARROWED
  Exit 8 with state ` + "`unknown`" + ` means the bus retains nothing for you and that
  message: it never existed, it was swept past the 24h retention window, you
  were not addressed in it, or the id is malformed. The bus answers all four
  identically on purpose — an answer that distinguished them would confirm to
  anyone who guessed an id that the message exists. Do not write a script
  that tries to tell them apart.

OUTPUT
  Human: the message, what you asserted, and the state that now stands.
  --json: exactly one object —
    {"correlation_key":"bus-x-7","recipient":"bus-x.me-1","outcome":"delivered",
     "accepted":true,"duplicate":false,"state":"delivered","ok":true}
  ` + "`outcome`" + ` is what YOU asserted; ` + "`state`" + ` is what now STANDS on the bus (on a
  duplicate, the original). ` + "`class`" + ` appears only on a negative terminal.

EXIT CODES
  0 the bus recorded it (including a duplicate of the same outcome)
  7 refused: already terminal with a DIFFERENT outcome — the first stands
  8 nothing to record: state unknown (see above)

  1 internal error              5 the bus is unreachable
  2 bad usage                   6 the bus reported an error of its own
  3 no usable identity          9 the bus has no route for this request:
  4 credential rejected           it is older than this client

  The result is printed BEFORE the exit code is decided, so exit 8 still
  tells you the state — under --json too, where the single result object is
  the only thing written to stdout.
`,
		run: runAck,
	}
}

// ackOutput is the --json document and the human render's input.
//
// It carries THREE things the bus's own answer does not, because a caller that
// logs one of these objects should not have to correlate it with the command
// line to know what it means: which message, which recipient asserted it, and
// what was asserted. The json tags are a CONTRACT — an agent branches on them.
type ackOutput struct {
	// CorrelationKey is the message id acknowledged.
	CorrelationKey string `json:"correlation_key"`

	// Recipient is this agent's own fully-qualified id (invariant 2) — the id
	// inside the signed bytes, not one the caller chose.
	Recipient string `json:"recipient"`

	// Outcome is what THIS COMMAND asserted: "delivered" or "refused". It is
	// deliberately separate from State: one is the claim, the other is what the
	// bus now records.
	Outcome string `json:"outcome"`

	// Accepted reports that the bus recorded the outcome, or had already
	// recorded exactly this one.
	Accepted bool `json:"accepted"`

	// Duplicate reports that this message was ALREADY terminal with the SAME
	// outcome: the original stands and nothing was re-applied. It is a success.
	Duplicate bool `json:"duplicate"`

	// State is the state that now STANDS — on a duplicate, the ORIGINAL one —
	// or "unknown" when the bus retains nothing for this pair.
	State string `json:"state"`

	// Class is the class the BUS recorded, present only on a negative terminal.
	Class string `json:"class,omitempty"`
}

func runAck(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("ack", env.g)
	refuse := fs.String("refuse", "",
		"refuse the message with one of: "+strings.Join(client.RecipientRefusalClasses(), ", "))

	// Parsed in passes so a flag may follow the positional argument: an agent
	// shelling out writes `ack <id> --json`, reading left to right — what, then
	// how — and a single fs.Parse would stop at the first non-flag argument and
	// reject the documented invocation.
	rest, err := parseWithPositionals(fs, args)
	if err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl ack", diagnostics, err)
	}
	if len(rest) != 1 {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "ack",
			Message: fmt.Sprintf("expected exactly one message id, got %d arguments", len(rest)),
			Remedy:  "run `agent-busctl ack <message-id>`; the id is the `message_id` the message arrived with",
		}
	}

	// ABSENT AND PRESENT-BUT-EMPTY ARE DIFFERENT, AND COLLAPSING THEM WOULD BE A
	// DEFECT WITH A DURABLE CONSEQUENCE.
	//
	// `ack "$ID" --refuse "$CLASS"` with $CLASS unset is the idiom an agent
	// shelling out writes, and the flag package hands that to us as an EMPTY
	// STRING — indistinguishable, without this, from not passing --refuse at
	// all. Treating it as the default would assert `delivered` for an agent
	// that was trying to REFUSE, and a terminal outcome is ABSORBING: it can
	// never afterwards be revisited, reopened or downgraded. client.Ack
	// deliberately refuses to default an outcome for exactly this reason (see
	// AckOptions.Outcome); reintroducing the default here through an unset
	// shell variable would undo that one layer up.
	refuseGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "refuse" {
			refuseGiven = true
		}
	})
	if refuseGiven && *refuse == "" {
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "ack",
			Message: "--refuse was given with an empty value; it is not the same as omitting it, and it is not `delivered`",
			Remedy:  "name the class you are refusing with — one of: " + strings.Join(client.RecipientRefusalClasses(), ", ") + " — or drop --refuse entirely to acknowledge delivery",
		}
	}

	// THE DEFAULT IS `delivered` AND THE REFUSAL IS EXPLICIT. There is no
	// --outcome flag, so "undeliverable" is not spellable here at all: the CLI
	// cannot express a routing claim, which is the cheapest place to make that
	// impossible. client.Ack refuses it again for an embedder that reaches the
	// package directly, and the bus refuses it a third time on the agent
	// surface.
	outcome := client.AckDelivered
	class := *refuse
	if class != "" {
		outcome = client.AckRefused
	}
	if class == client.AckUndeliverable {
		// Named on its own because it is the mistake somebody WILL make: it is
		// the one remaining member of the outcome vocabulary, so it reads like
		// a refusal reason. It is not one, and it is not a class at all — it is
		// a claim about a BUS's own routing, and a recipient that could assert
		// it would be settling a message, absorbingly and forever, on a fact it
		// has no way to know. The generic "not a class a recipient may emit"
		// below would be true and would leave the reader thinking they had
		// mistyped.
		return &client.Error{
			Kind:    client.KindUsage,
			Op:      "ack",
			Message: "`undeliverable` is a routing claim a BUS makes about its own federation, never something a recipient may assert",
			Remedy:  "if your application is refusing the message, use --refuse with one of: " + strings.Join(client.RecipientRefusalClasses(), ", "),
		}
	}
	env.out.json = env.g.json

	c, err := env.client()
	if err != nil {
		return err
	}
	// The recipient in the signed bytes is resolved by the client from the
	// credential; this is the same value, read locally, so the output can name
	// who acknowledged without a second round trip.
	id, err := c.Identity()
	if err != nil {
		return err
	}

	res, err := c.Ack(ctx, client.AckOptions{
		CorrelationKey: rest[0],
		Outcome:        outcome,
		Class:          class,
	})
	if err != nil {
		return err
	}
	out := ackOutput{
		CorrelationKey: rest[0],
		Recipient:      id.AgentID,
		Outcome:        outcome,
		Accepted:       res.Accepted,
		Duplicate:      res.Duplicate,
		State:          res.State,
		Class:          res.Class,
	}

	// THE RESULT IS PRINTED FIRST, ALWAYS — before any exit code is decided,
	// exactly as ack-status does it. Exit 8 is an OUTCOME of the message, not a
	// failure of the command, and expressing it as a returned error would print
	// the failure envelope INSTEAD of the object an agent needs.
	if err := env.out.Emit(out, func(w io.Writer) { writeAckResult(w, out) }); err != nil {
		return err
	}
	return ackExit(out)
}

// ackExit maps the recorded answer onto the §13.4 exit-code table. NO NEW EXIT
// CODE IS MINTED: an agent's branch table is a contract, and a new number in it
// is a breaking change.
//
//	the bus recorded it (duplicate included) | ExitOK       (0)
//	nothing retained, state "unknown"        | ExitEmpty    (8)
//	already terminal, different outcome      | ExitRejected (7) — a 409, so it
//	                                           arrives as a KindRejected error
//	                                           and never reaches this function
func ackExit(out ackOutput) error {
	switch {
	case out.Accepted:
		return nil
	case out.State == client.AckUnknown:
		return ackStatusExitCode{code: client.ExitEmpty}
	default:
		// UNREACHABLE against a correct bus: the route answers accepted:false
		// for exactly one condition, and it names that condition "unknown".
		// It is checked rather than assumed because the alternative is worse
		// than an error — silently exiting 0 on `accepted:false` would report a
		// settled message to a script that would never look again.
		return &client.Error{
			Kind:    client.KindServer,
			Op:      "ack",
			Message: "the bus did not accept the acknowledgement but reported state " + out.State + " rather than \"unknown\"",
			Remedy:  "report this to the bus operator: `accepted:false` has exactly one meaning on this route, and it is that nothing is retained for this message",
		}
	}
}

// writeAckResult renders the answer for a human.
//
// Nothing here sanitises the strings — client.Ack has already REJECTED the
// whole response if any field carried anything a terminal could act on
// (client/sanitize.go), and refused a state spelling outside the closed set. Do
// not add a second, weaker check here; add it there if it is ever missing.
func writeAckResult(w io.Writer, out ackOutput) {
	fmt.Fprintf(w, "message:  %s\n", out.CorrelationKey)
	fmt.Fprintf(w, "as:       %s\n", out.Recipient)
	fmt.Fprintf(w, "asserted: %s\n", out.Outcome)
	fmt.Fprintf(w, "state:    %s\n", out.State)
	if out.Class != "" {
		fmt.Fprintf(w, "class:    %s\n", out.Class)
	}
	switch {
	case !out.Accepted:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "The bus has nothing to record for that message. It never existed, it was swept")
		fmt.Fprintln(w, "past the 24h retention window, you were not addressed in it, or the id is")
		fmt.Fprintln(w, "malformed — the bus answers all four the same way on purpose.")
	case out.Duplicate:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Already acknowledged with this same outcome; the original stands and nothing")
		fmt.Fprintln(w, "was re-applied. Re-acknowledging is a legitimate retry, not an error.")
	}
}
