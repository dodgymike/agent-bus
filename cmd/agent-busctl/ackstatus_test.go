package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// ackStatusRoute is the path the CLI must call. Spelled here as a LITERAL
// rather than imported, on purpose: this test is the thing that would notice
// the client silently starting to call a different route, and a constant shared
// with the code under test cannot notice that.
const ackStatusRoute = "/v1/ack/"

// ackStubRows is a §13.2 body the bus would send.
func ackStubRows(rows ...map[string]interface{}) map[string]interface{} {
	if len(rows) == 0 {
		rows = []map[string]interface{}{{"state": "unknown"}}
	}
	return map[string]interface{}{"rows": rows}
}

// TestAckStatusAPIAndCLI is ACK-9's recorded proof: the compiled CLI, driven the
// way an agent drives it, against a bus that answers the §13 wire shapes.
//
// It exercises the THREE audiences invariant 7 names — a human reading a block,
// an agent reading --json, and an agent branching on an exit code — and it
// covers the whole §13.4 exit-code table. It NEVER hand-writes an HTTP call:
// the request the stub records is the one client.AckStatus built.
func TestAckStatusAPIAndCLI(t *testing.T) {
	t.Run("json shape and the request the client builds", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) || r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			stubWriteJSON(w, http.StatusOK, ackStubRows(map[string]interface{}{
				"correlation_key": "bus-x-7",
				"recipient":       "bus-y.beta-1",
				"state":           "delivered",
				"attested_by":     "recipient_signature_unverified",
				"accepted_at":     "2026-08-16T09:00:00Z",
				"settled_at":      "2026-08-16T09:00:02Z",
			}))
		})

		res := bus.run(t, "", false, false, "ack-status", "bus-x-7", "--json")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want 0; stdout %s stderr %s", res.Code, res.Stdout, res.Stderr)
		}

		// EXACTLY ONE object on stdout: an agent piping this into a parser must
		// not have to know how many lines to expect.
		lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
		if len(lines) != 1 {
			t.Fatalf("--json wrote %d lines, want exactly 1:\n%s", len(lines), res.Stdout)
		}
		var out struct {
			OK   bool `json:"ok"`
			Rows []struct {
				CorrelationKey string `json:"correlation_key"`
				Recipient      string `json:"recipient"`
				State          string `json:"state"`
				Class          string `json:"class"`
				AttestedBy     string `json:"attested_by"`
			} `json:"rows"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &out); err != nil {
			t.Fatalf("stdout is not one JSON object (%v): %s", err, res.Stdout)
		}
		if !out.OK || len(out.Rows) != 1 {
			t.Fatalf("json = %s, want ok:true and one row", lines[0])
		}
		row := out.Rows[0]
		if row.State != "delivered" || row.Recipient != "bus-y.beta-1" || row.CorrelationKey != "bus-x-7" {
			t.Errorf("row = %+v, want the delivered row the bus sent", row)
		}
		if row.Class != "" {
			t.Errorf("a POSITIVE terminal carries class %q; §5.4 gives it no class at all", row.Class)
		}
		if row.AttestedBy != "recipient_signature_unverified" {
			t.Errorf("attested_by = %q, want the unverified label preserved verbatim — shortening it would read as a verification claim", row.AttestedBy)
		}

		// The CLI called the documented route with the key in the PATH, and
		// sent no ?wait when none was asked for.
		calls := bus.calls(ackStatusRoute + "bus-x-7")
		if len(calls) != 1 {
			t.Fatalf("the CLI made %d calls to %s, want 1", len(calls), ackStatusRoute+"bus-x-7")
		}
		if got := calls[0].Query.Get("wait"); got != "" {
			t.Errorf("a snapshot request sent ?wait=%q, want none", got)
		}
	})

	t.Run("unknown is exit 0 without --wait and exit 8 with it", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			stubWriteJSON(w, http.StatusOK, ackStubRows())
		})

		// §13.4 row 1: reported a state successfully — any state, unknown
		// included. Without --wait you asked for a snapshot and you got one.
		snap := bus.run(t, "", false, false, "ack-status", "bus-x-9", "--json")
		if snap.Code != client.ExitOK {
			t.Errorf("unknown without --wait exited %d, want 0; %s", snap.Code, snap.Stdout)
		}
		if !strings.Contains(snap.Stdout, `"state":"unknown"`) {
			t.Errorf("the unknown answer did not reach stdout: %s", snap.Stdout)
		}

		// §13.4 row 4: --wait and the state is unknown -> ExitEmpty.
		waited := bus.run(t, "", false, false, "ack-status", "bus-x-9", "--wait", "1s", "--json")
		if waited.Code != client.ExitEmpty {
			t.Errorf("unknown with --wait exited %d, want %d (ExitEmpty)", waited.Code, client.ExitEmpty)
		}
		// The ROW still reached stdout, as exactly one object: an agent that
		// branched on exit 8 must still be able to read what it was told.
		if lines := strings.Split(strings.TrimSpace(waited.Stdout), "\n"); len(lines) != 1 {
			t.Fatalf("--json with a non-zero exit wrote %d lines, want 1:\n%s", len(lines), waited.Stdout)
		}
		if !strings.Contains(waited.Stdout, `"state":"unknown"`) || !strings.Contains(waited.Stdout, `"ok":true`) {
			t.Errorf("exit 8 replaced the result object with a failure envelope: %s", waited.Stdout)
		}
		if got := bus.calls(ackStatusRoute + "bus-x-9")[1].Query.Get("wait"); got != "1" {
			t.Errorf("--wait 1s sent ?wait=%q, want \"1\" (whole seconds)", got)
		}
	})

	t.Run("a negative terminal is exit 7 and still prints the class", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			stubWriteJSON(w, http.StatusOK, ackStubRows(map[string]interface{}{
				"correlation_key": "bus-x-8",
				"recipient":       "bus-y.beta-1",
				"state":           "refused",
				"class":           "recipient_refused_policy",
				"attested_by":     "recipient_signature_unverified",
				"accepted_at":     "2026-08-16T09:00:00Z",
				"settled_at":      "2026-08-16T09:00:02Z",
			}))
		})

		// Without --wait it is a snapshot: exit 0, per §13.4 row 1.
		if snap := bus.run(t, "", false, false, "ack-status", "bus-x-8"); snap.Code != client.ExitOK {
			t.Errorf("refused WITHOUT --wait exited %d, want 0 — the outcome only becomes the exit status when you asked to wait for it", snap.Code)
		}

		res := bus.run(t, "", false, false, "ack-status", "bus-x-8", "--wait", "5s", "--json")
		if res.Code != client.ExitRejected {
			t.Fatalf("refused with --wait exited %d, want %d (ExitRejected)", res.Code, client.ExitRejected)
		}
		if !strings.Contains(res.Stdout, `"class":"recipient_refused_policy"`) {
			t.Fatalf("exit 7 did not carry the class, which is the one field the caller needs: %s", res.Stdout)
		}
		if lines := strings.Split(strings.TrimSpace(res.Stdout), "\n"); len(lines) != 1 {
			t.Fatalf("--json wrote %d lines on the exit-7 path, want 1:\n%s", len(lines), res.Stdout)
		}
	})

	t.Run("delivered with --wait is exit 0", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			stubWriteJSON(w, http.StatusOK, ackStubRows(map[string]interface{}{
				"correlation_key": "bus-x-7", "recipient": "bus-y.beta-1",
				"state": "delivered", "attested_by": "peer_bus",
				"accepted_at": "2026-08-16T09:00:00Z", "settled_at": "2026-08-16T09:00:02Z",
			}))
		})
		if res := bus.run(t, "", false, false, "ack-status", "bus-x-7", "--wait", "5s"); res.Code != client.ExitOK {
			t.Errorf("delivered with --wait exited %d, want 0; %s", res.Code, res.Stderr)
		}
	})

	t.Run("the human rendering explains what unknown means", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			stubWriteJSON(w, http.StatusOK, ackStubRows())
		})
		res := bus.run(t, "", true, false, "ack-status", "bus-x-9")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want 0", res.Code)
		}
		// A human told only "unknown" would read it as "your message vanished".
		// The four cases it collapses are what the reader actually needs.
		for _, want := range []string{"unknown", "another sender", "swept"} {
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("the human rendering never mentions %q:\n%s", want, res.Stdout)
			}
		}
	})

	// The unknown branch above is the SHORT one — it returns before the row
	// loop ever runs. Every per-row line in writeAckStatus (recipient, state,
	// class, attested, accepted, settled, and the blank line between rows) was
	// therefore unasserted in the human rendering: deleting any one of those
	// Fprintf calls left this file GREEN, because --json reads the client's
	// struct and never the text a human is shown.
	//
	// MUTATION (each run, each RED): deleting any single Fprintf in the row
	// loop, and deleting the `if i > 0` separator.
	t.Run("the human rendering carries every field of every row", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			stubWriteJSON(w, http.StatusOK, ackStubRows(
				map[string]interface{}{
					"correlation_key": "bus-x-7", "recipient": "bus-y.beta-1",
					"state": "refused", "class": "recipient_refused_policy",
					"attested_by": "peer_bus",
					"accepted_at": "2026-08-16T09:00:00Z", "settled_at": "2026-08-16T09:00:02Z",
				},
				map[string]interface{}{
					"correlation_key": "bus-x-7", "recipient": "bus-y.gamma-1",
					"state": "delivered", "attested_by": "recipient_signature_unverified",
					"accepted_at": "2026-08-16T09:00:00Z", "settled_at": "2026-08-16T09:00:03Z",
				},
			))
		})

		// No --wait, so the exit code is 0 and the assertions below are about
		// the RENDERING and nothing else (§13.4 row 1).
		res := bus.run(t, "", true, false, "ack-status", "bus-x-7")
		if res.Code != client.ExitOK {
			t.Fatalf("exit = %d, want 0; stderr %s", res.Code, res.Stderr)
		}
		out := res.Stdout

		// Matched as whole LINES with their labels, not as bare substrings: a
		// value that survived while its label was dropped would still be
		// unreadable, and a substring check would not notice.
		for _, want := range []string{
			`(?m)^to:\s+bus-y\.beta-1$`,
			`(?m)^state:\s+refused$`,
			`(?m)^class:\s+recipient_refused_policy$`,
			`(?m)^attested:\s+peer_bus$`,
			`(?m)^to:\s+bus-y\.gamma-1$`,
			`(?m)^state:\s+delivered$`,
			`(?m)^attested:\s+recipient_signature_unverified$`,
		} {
			if !regexp.MustCompile(want).MatchString(out) {
				t.Errorf("the human rendering has no line matching %s:\n%s", want, out)
			}
		}

		// Both timestamps are rendered for both rows. The VALUE is not pinned:
		// shortTimestamp formats in the local zone, so an exact string would
		// assert the test box's TZ rather than the CLI's behaviour.
		if n := len(regexp.MustCompile(`(?m)^accepted:\s+\S`).FindAllString(out, -1)); n != 2 {
			t.Errorf("%d accepted: lines, want 2:\n%s", n, out)
		}
		if n := len(regexp.MustCompile(`(?m)^settled:\s+\S`).FindAllString(out, -1)); n != 2 {
			t.Errorf("%d settled: lines, want 2:\n%s", n, out)
		}

		// EXACTLY ONE class line. §5.4 gives a positive terminal no class at
		// all, so rendering one for `delivered` would invent a field the bus
		// never sent — and an unconditional Fprintf would print an empty one.
		if n := len(regexp.MustCompile(`(?m)^class:`).FindAllString(out, -1)); n != 1 {
			t.Errorf("%d class: lines, want exactly 1 (only the refused row has a class):\n%s", n, out)
		}

		// The rows are SEPARATED by a blank line. Two recipients run together
		// read as one block, and a human scanning a fan-out would attribute the
		// second row's outcome to the first recipient.
		if !strings.Contains(out, "\n\nto:") {
			t.Errorf("the two rows are not separated by a blank line:\n%s", out)
		}
	})

	t.Run("usage failures never reach the bus", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the CLI sent a request for a malformed invocation: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"no key", []string{"ack-status"}},
			{"two keys", []string{"ack-status", "bus-x-1", "bus-x-2"}},
			{"a whitespace-padded key", []string{"ack-status", "bus-x-1 "}},
			{"a negative wait", []string{"ack-status", "bus-x-1", "--wait", "-5s"}},
			{"a wait above the ceiling", []string{"ack-status", "bus-x-1", "--wait", "10m"}},
		} {
			res := bus.run(t, "", false, false, tc.args...)
			if res.Code != client.ExitUsage {
				t.Errorf("%s exited %d, want %d (ExitUsage); stdout %s stderr %s",
					tc.name, res.Code, client.ExitUsage, res.Stdout, res.Stderr)
			}
		}
	})

	t.Run("a bus with no such route is version skew, not a refusal", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
		res := bus.run(t, "", false, false, "ack-status", "bus-x-7", "--json")
		if res.Code != client.ExitVersionSkew {
			t.Errorf("a 404 on the status route exited %d, want %d (ExitVersionSkew) — an older bus never understood the request, which is not the same as refusing it",
				res.Code, client.ExitVersionSkew)
		}
	})

	t.Run("the parked-wait cap is transient: retried, then exit 6", func(t *testing.T) {
		// PINS A DOCUMENTED CLAIM. AGENT_PROTOCOL.md and CONTRACTS-CLI.md both
		// tell an agent what a 429 from this route does, and an earlier draft of
		// that text said exit 7. It is exit 6: statusError classifies 429 as
		// KindServer (client/transport.go), not KindRejected, because being at
		// capacity is not the bus judging the request on its merits — and
		// KindServer + 429 is RETRYABLE, so the client absorbs a cap breach that
		// clears inside the retry window.
		//
		// Both halves are asserted, because both are documented and either could
		// drift: the retry (an agent would otherwise see spurious failures under
		// its own fan-out) and the code (an agent branches on it).
		var attempts int32
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			atomic.AddInt32(&attempts, 1)
			w.Header().Set("Retry-After", "1")
			stubWriteJSON(w, http.StatusTooManyRequests, map[string]interface{}{
				"error": "too many delivery-status waits parked for this agent",
			})
		})

		res := bus.run(t, "", false, false, "ack-status", "bus-x-7", "--wait", "5s", "--json")
		if res.Code != client.ExitServer {
			t.Errorf("a 429 exited %d, want %d (ExitServer) — it is a capacity failure of the BUS, not a refusal of the request on its merits, so it must not be ExitRejected(%d)",
				res.Code, client.ExitServer, client.ExitRejected)
		}
		if n := atomic.LoadInt32(&attempts); n < 2 {
			t.Errorf("the client made %d attempt(s); a 429 is transient and must be retried, or an agent at its own fan-out limit sees a hard failure for a condition that clears in a second", n)
		}
		if !strings.Contains(res.Stdout, `"status":429`) {
			t.Errorf("the failure object does not carry the status the caller needs to recognise the cap: %s", res.Stdout)
		}
	})

	t.Run("a state outside the closed set is refused, not passed through", func(t *testing.T) {
		bus := newStubBus(t, "bus-x.agent-1", func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, ackStatusRoute) {
				http.NotFound(w, r)
				return
			}
			// "polled" is the exact state ack.State refuses to have: delivery to
			// an inbox is NOT recipient receipt (§4).
			stubWriteJSON(w, http.StatusOK, ackStubRows(map[string]interface{}{
				"correlation_key": "bus-x-7", "recipient": "bus-y.beta-1", "state": "polled",
			}))
		})
		res := bus.run(t, "", false, false, "ack-status", "bus-x-7", "--json")
		if res.Code == client.ExitOK {
			t.Fatalf("a state outside the closed set was reported as a success: %s", res.Stdout)
		}
		if !strings.Contains(res.Stdout, `"ok":false`) {
			t.Errorf("the failure was not rendered as the documented failure object: %s", res.Stdout)
		}
	})
}
