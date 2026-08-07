package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/dodgymike/agent-bus/client"
)

func agentsCommand() command {
	return command{
		name:    "agents",
		summary: "list the agents enrolled on this bus",
		help: `agent-busctl agents — list the agents enrolled on this bus.

USAGE
  agent-busctl agents [--json]

WHAT IT DOES
  Asks the bus for its roster and prints every agent's FULLY-QUALIFIED id,
  ` + "`<bus-id>.<agent-id>`" + `. That id — all of it — is what ` + "`agent-busctl send`" + ` takes: the
  bus prefix is what makes an id unambiguous once buses relay to each other,
  so it is never shortened, elided or truncated here. If a row would be too
  wide, this command drops a COLUMN (ENROLLED first, then BUS, which is only
  the id's own prefix restated) rather than cutting the id.

  The roster carries no key material.

WHAT IT DOES NOT REPORT
  There is no "last seen" column. The bus does not track one: ` + "`GET /v1/agents`" + `
  returns agent_id, name and enrolled_at and nothing else, and a liveness
  figure invented on this side would be a guess presented as a fact. To find
  out whether an agent is alive, send to it and see whether it answers.

OUTPUT
  Human: an aligned table, one agent per row.
  --json: one object — {"agents":[…],"count":N,"ok":true} — where each entry
  carries agent_id, bus_id, name and enrolled_at. bus_id is DERIVED from the
  qualified id (the part before the first '.'), not sent separately.

EXIT CODES
  0 ok                          5 the bus is unreachable
  1 internal error              6 the bus reported an error of its own
  2 bad usage                   7 the bus refused the request
  3 no usable identity          8 the roster is empty
  4 credential rejected

  8 is rare in practice: you are normally on the roster yourself, so an empty
  answer means the bus has forgotten your enrolment (it was rebuilt, or the
  data directory was replaced). It is its own code so a script can tell that
  apart from a successful listing without parsing text.
`,
		run: runAgents,
	}
}

func runAgents(ctx context.Context, env *cliEnv, args []string) error {
	fs, diagnostics := newCommandFlagSet("agents", env.g)
	if err := fs.Parse(args); err != nil {
		if err == flagErrHelp {
			return err
		}
		return flagError("agent-busctl agents", diagnostics, err)
	}
	if err := requireNoArgs("agents", fs.Args()); err != nil {
		return err
	}
	env.out.json = env.g.json

	c, err := env.client()
	if err != nil {
		return err
	}

	list, err := c.Agents(ctx)
	if err != nil {
		return err
	}
	if len(list.Agents) == 0 {
		// "Nothing to report" is its own exit code so a script can branch on an
		// empty roster without parsing text, exactly as `whoami --all` does.
		return &client.Error{
			Kind:    client.KindEmpty,
			Op:      "agents",
			Message: "the bus reports no enrolled agents",
			Remedy:  "check --bus points at the bus you enrolled with; if it was rebuilt, re-enrol with `agent-busctl enrol`",
		}
	}

	return env.out.Emit(list, func(w io.Writer) { writeAgentTable(w, list.Agents) })
}

// maxAgentTableWidth is the width at which columns start being dropped.
//
// 100 rather than 80: a terminal is usually at least 80 wide and an id is
// commonly 20-30 characters, so this only bites on genuinely long ids — which
// is precisely the case where the id must survive and something else must go.
const maxAgentTableWidth = 100

// writeAgentTable renders the roster as an aligned table.
//
// # The id column is never truncated. That is the whole design of this function
//
// Invariant 2: every agent id is `<bus-id>.<agent-id>`, and the bus prefix is
// exactly what disambiguates two agents called "planner" on two different buses
// once relaying is in play. Eliding the middle of an id, or cutting it to a
// fixed width, would therefore destroy the one property the id exists to carry —
// and the bus prefix is the LEADING part, so any "…" truncation cuts the wrong
// end first. So the id column is sized to the longest id present, and when that
// makes a row wider than maxAgentTableWidth the columns that carry the least
// information are dropped instead:
//
//	ENROLLED first — it is a timestamp nobody routes on;
//	then BUS — it is literally the id's own prefix, already on the row.
//
// Nothing here sanitises the strings: client.Agents has already REJECTED the
// whole response if any id, name or timestamp contained anything a terminal
// could act on (client/sanitize.go). Do not add a second, weaker check here;
// add it there if it is ever missing.
func writeAgentTable(w io.Writer, agents []client.AgentSummary) {
	const (
		idHead       = "AGENT ID"
		nameHead     = "NAME"
		busHead      = "BUS"
		enrolledHead = "ENROLLED"
		gap          = "  "
	)

	enrolled := make([]string, len(agents))
	idW, nameW, busW, enrolledW := len(idHead), len(nameHead), len(busHead), len(enrolledHead)
	for i, a := range agents {
		enrolled[i] = shortTimestamp(a.EnrolledAt)
		idW = maxInt(idW, len(a.AgentID))
		nameW = maxInt(nameW, len(a.Name))
		busW = maxInt(busW, len(a.BusID))
		enrolledW = maxInt(enrolledW, len(enrolled[i]))
	}

	showBus, showEnrolled := true, true
	width := func() int {
		n := idW + len(gap) + nameW
		if showBus {
			n += len(gap) + busW
		}
		if showEnrolled {
			n += len(gap) + enrolledW
		}
		return n
	}
	if width() > maxAgentTableWidth {
		showEnrolled = false
	}
	if width() > maxAgentTableWidth {
		showBus = false
	}

	row := func(id, name, bus, when string) {
		line := pad(id, idW) + gap + pad(name, nameW)
		if showBus {
			line += gap + pad(bus, busW)
		}
		if showEnrolled {
			line += gap + when
		}
		fmt.Fprintln(w, trimTrailingSpace(line))
	}

	row(idHead, nameHead, busHead, enrolledHead)
	for i, a := range agents {
		row(a.AgentID, a.Name, a.BusID, enrolled[i])
	}
	fmt.Fprintf(w, "\n%s agent(s)\n", strconv.Itoa(len(agents)))
}

// shortTimestamp renders an RFC3339 instant as a local "2006-01-02 15:04".
//
// A timestamp the bus formatted some other way is passed through UNCHANGED
// rather than replaced with a placeholder: it is already known not to contain
// control characters (client.validateServerTimestamp), and showing what the bus
// actually said is more useful than hiding it. It only ever costs width, which
// is the column dropped first anyway.
func shortTimestamp(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

func pad(s string, w int) string {
	for len(s) < w {
		s += " "
	}
	return s
}

func trimTrailingSpace(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
