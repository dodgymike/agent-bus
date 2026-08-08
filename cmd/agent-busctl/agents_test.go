package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// rosterEntry is one wire entry of GET /v1/agents. The route carries agent_id,
// name and enrolled_at and NOTHING else — no key material, and no bus_id (the
// client derives that from the qualified id).
type rosterEntry struct {
	AgentID    string `json:"agent_id"`
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

func rosterBus(t *testing.T, entries []rosterEntry, count int) *stubBus {
	t.Helper()
	return newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != stubRouteAgents {
			http.NotFound(w, r)
			return
		}
		stubWriteJSON(w, http.StatusOK, map[string]interface{}{"agents": entries, "count": count})
	})
}

// TestCLIAgentsHumanTableNeverTruncatesQualifiedID checks the id column is
// never cut, however long the id is.
//
// Invariant 2: every agent id is `<bus-id>.<agent-id>`, and the bus prefix is
// exactly what disambiguates two agents called "planner" on two different buses
// once relaying is in play. It is also the LEADING part, so any "…" truncation
// cuts the wrong end first. When a row will not fit, the columns carrying the
// least information go instead — ENROLLED, then BUS, which is only the id's own
// prefix restated.
func TestCLIAgentsHumanTableNeverTruncatesQualifiedID(t *testing.T) {
	const longID = "bus-averyverylongbusidentifierindeed0123." +
		"planner-with-an-unreasonably-long-descriptive-agent-name-0000000042"
	if len(longID) <= maxAgentTableWidth {
		t.Fatalf("the fixture id is only %d characters, which does not exceed the %d-column budget; this test would not exercise the drop path",
			len(longID), maxAgentTableWidth)
	}

	bus := rosterBus(t, []rosterEntry{
		{AgentID: longID, Name: "planner-with-an-unreasonably-long-descriptive-agent-name", EnrolledAt: "2026-08-02T09:00:00Z"},
		{AgentID: "bus-x.short-1", Name: "short", EnrolledAt: "2026-08-02T10:00:00Z"},
	}, 2)

	res := bus.run(t, "", true, false, "agents")
	if res.Code != client.ExitOK {
		t.Fatalf("agents exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, longID) {
		t.Fatalf("the FULL qualified id does not appear in the table; it was elided or truncated:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "…") || strings.Contains(res.Stdout, "...") {
		t.Fatalf("the table contains an ellipsis; an id is never abbreviated:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "bus-x.short-1") {
		t.Fatalf("the short id is missing from the table:\n%s", res.Stdout)
	}
	// The columns that carry the least information are what gets dropped.
	if strings.Contains(res.Stdout, "ENROLLED") {
		t.Fatalf("the ENROLLED column survived a row wider than the budget, so something else must have been cut:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "AGENT ID") {
		t.Fatalf("the table has no AGENT ID header:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "2 agent(s)") {
		t.Fatalf("the table does not report the count:\n%s", res.Stdout)
	}
}

// TestCLIAgentsRespectsColumns checks the table drops columns against the
// READER'S terminal width rather than a fixed 100.
//
// The fixture row is deliberately sized between the two: comfortably under the
// default budget, comfortably over a narrow COLUMNS. So the SAME roster must
// keep every column when COLUMNS is wide (or unset), and shed ENROLLED then BUS
// when COLUMNS is narrow. A fixed width cannot produce both, which is what makes
// this test RED before the fix.
//
// The fully-qualified id survives every case — that is CLI-5's load-bearing half
// (invariant 2) and narrowing the budget must never weaken it.
func TestCLIAgentsRespectsColumns(t *testing.T) {
	entries := []rosterEntry{
		{AgentID: "bus-x.planner-1", Name: "planner", EnrolledAt: "2026-08-02T09:00:00Z"},
		{AgentID: "bus-x.review-2", Name: "review", EnrolledAt: "2026-08-02T10:00:00Z"},
	}

	env := func(v string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k == "COLUMNS" && v != "" {
				return v, true
			}
			return "", false
		}
	}

	cases := []struct {
		name         string
		columns      string
		wantEnrolled bool
		wantBus      bool
	}{
		// Unset is the common non-interactive case: keep today's 100-column
		// default rather than guessing, because that is exactly when the output
		// is being piped and width does not matter.
		{name: "unset falls back to the default budget", columns: "", wantEnrolled: true, wantBus: true},
		{name: "wide keeps every column", columns: "200", wantEnrolled: true, wantBus: true},
		{name: "narrow drops ENROLLED first", columns: "40", wantEnrolled: false, wantBus: true},
		{name: "very narrow drops BUS too", columns: "26", wantEnrolled: false, wantBus: false},
		// A COLUMNS a shell never sets but a hostile or confused environment
		// might. None of these may be believed; all fall back to the default.
		{name: "zero is not believed", columns: "0", wantEnrolled: true, wantBus: true},
		{name: "negative is not believed", columns: "-80", wantEnrolled: true, wantBus: true},
		{name: "non-numeric is not believed", columns: "wide", wantEnrolled: true, wantBus: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			bus := rosterBus(t, entries, len(entries))
			res := bus.runEnv(t, context.Background(), env(tc.columns), "", true, false, "agents")
			if res.Code != client.ExitOK {
				t.Fatalf("agents exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
			}
			for _, id := range []string{"bus-x.planner-1", "bus-x.review-2"} {
				if !strings.Contains(res.Stdout, id) {
					t.Fatalf("COLUMNS=%q cut the qualified id %s out of the table:\n%s", tc.columns, id, res.Stdout)
				}
			}
			if strings.Contains(res.Stdout, "…") || strings.Contains(res.Stdout, "...") {
				t.Fatalf("COLUMNS=%q produced an ellipsis; an id is never abbreviated:\n%s", tc.columns, res.Stdout)
			}
			if got := strings.Contains(res.Stdout, "ENROLLED"); got != tc.wantEnrolled {
				t.Fatalf("COLUMNS=%q: ENROLLED present = %v, want %v:\n%s", tc.columns, got, tc.wantEnrolled, res.Stdout)
			}
			if got := strings.Contains(res.Stdout, "BUS"); got != tc.wantBus {
				t.Fatalf("COLUMNS=%q: BUS present = %v, want %v:\n%s", tc.columns, got, tc.wantBus, res.Stdout)
			}
		})
	}
}

// TestCLIAgentsEmptyRosterExitsEmpty checks an empty roster is "nothing to
// report" (exit 8) rather than a success a script cannot distinguish from a
// listing, and rather than a failure.
func TestCLIAgentsEmptyRosterExitsEmpty(t *testing.T) {
	bus := rosterBus(t, []rosterEntry{}, 0)

	res := bus.run(t, "", false, false, "agents")
	if res.Code != client.ExitEmpty {
		t.Fatalf("agents on an empty roster exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitEmpty, res.Stdout, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("stdout = %q, want nothing on the empty path", res.Stdout)
	}

	t.Run("--json still renders a parseable failure object", func(t *testing.T) {
		bus := rosterBus(t, []rosterEntry{}, 0)
		res := bus.run(t, "", false, false, "agents", "--json")
		if res.Code != client.ExitEmpty {
			t.Fatalf("exit = %d, want %d", res.Code, client.ExitEmpty)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &payload); err != nil {
			t.Fatalf("--json did not render the empty outcome as a parseable object: %v (%q)", err, res.Stdout)
		}
		if kind, _ := payload["kind"].(string); kind != string(client.KindEmpty) {
			t.Fatalf("kind = %v, want %q", payload["kind"], client.KindEmpty)
		}
		if code, _ := payload["exit_code"].(float64); int(code) != client.ExitEmpty {
			t.Fatalf("exit_code = %v, want %d", payload["exit_code"], client.ExitEmpty)
		}
	})
}

// TestCLIAgentsJSON checks the documented --json object: one object, sorted
// agents, a derived bus_id per entry, a LOCALLY recomputed count, and ok:true.
func TestCLIAgentsJSON(t *testing.T) {
	bus := rosterBus(t, []rosterEntry{
		{AgentID: "bus-x.zulu-9", Name: "zulu", EnrolledAt: "2026-08-02T10:00:00Z"},
		{AgentID: "bus-x.alpha-2", Name: "alpha", EnrolledAt: "2026-08-02T08:00:00Z"},
	}, 9999) // a lying count, which must not survive

	res := bus.run(t, "", false, false, "agents", "--json")
	if res.Code != client.ExitOK {
		t.Fatalf("agents --json exit = %d, want %d; stdout=%q stderr=%q", res.Code, client.ExitOK, res.Stdout, res.Stderr)
	}
	if lines := strings.Count(strings.TrimSuffix(res.Stdout, "\n"), "\n"); lines != 0 {
		t.Fatalf("--json emitted %d newlines inside the document; it must be exactly ONE object: %q", lines, res.Stdout)
	}

	var out struct {
		OK     bool `json:"ok"`
		Count  int  `json:"count"`
		Agents []struct {
			AgentID    string `json:"agent_id"`
			BusID      string `json:"bus_id"`
			Name       string `json:"name"`
			EnrolledAt string `json:"enrolled_at"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		t.Fatalf("agents --json stdout is not JSON: %v (%q)", err, res.Stdout)
	}
	if !out.OK {
		t.Fatalf(`"ok" = false, want true`)
	}
	if out.Count != len(out.Agents) {
		t.Fatalf("count = %d but len(agents) = %d — count is recomputed locally, never taken from the wire", out.Count, len(out.Agents))
	}
	if out.Count != 2 {
		t.Fatalf("count = %d, want 2 (the wire said 9999)", out.Count)
	}
	if out.Agents[0].AgentID != "bus-x.alpha-2" || out.Agents[1].AgentID != "bus-x.zulu-9" {
		t.Fatalf("agents are not sorted by agent_id: %+v", out.Agents)
	}
	for i, a := range out.Agents {
		if a.BusID != "bus-x" {
			t.Fatalf("agents[%d].bus_id = %q, want %q derived from the qualified id", i, a.BusID, "bus-x")
		}
		if a.Name == "" || a.EnrolledAt == "" {
			t.Fatalf("agents[%d] is missing name or enrolled_at: %+v", i, a)
		}
	}
	if strings.Contains(res.Stdout, "public_key") || strings.Contains(res.Stdout, "private") {
		t.Fatalf("the roster carries key material: %q", res.Stdout)
	}
}
