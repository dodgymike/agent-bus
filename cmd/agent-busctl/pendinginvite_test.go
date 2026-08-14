package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// INVITE-CLIENT-FU-PENDINGINVITE, the CLI half.
//
// Invariant 7: if the subcommand does not do it, the feature does not exist. The
// package half is asserted in client/pendinginvite_test.go; what is asserted
// here is the two things only the CLI can get wrong — the exit code an agent
// branches on, and the fact that `whoami --all` prints a resume command that
// actually WORKS for an invited attempt. It used to print
// `enrol --bus <url> --name <name> --idempotency-key <key>`, which since this
// change is refused (exit 2) for a record that redeemed an invite: the bus
// fingerprints the invite id, so a resume that presents none is a different
// payload. A printed remedy that fails is worse than none.

// unreachableBus is a loopback port nothing listens on. Enrolling against it
// fails as KindNetwork, which is exactly the "may or may not have been applied"
// case that KEEPS the key material — the state these tests need.
const unreachableBus = "http://127.0.0.1:1"

// storeEnv is a lookupEnv that points the CLI at dir as its credential store.
func storeEnv(dir string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == client.EnvIdentityDir {
			return dir, true
		}
		return "", false
	}
}

// runCLIIn drives the CLI with a credential store at dir.
func runCLIIn(t *testing.T, dir string, argv ...string) cliResult {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(context.Background(), argv, &stdout, &stderr, storeEnv(dir))
	return cliResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

// TestPendingInviteResumeThroughTheCLI is the CLI-level proof for
// INVITE-CLIENT-FU-PENDINGINVITE.
func TestPendingInviteResumeThroughTheCLI(t *testing.T) {
	const key = "busctl-cli-pendinginvite"

	// An interrupted invited enrolment: the bus is unreachable, so the pending
	// record — the only copy of both private key seeds — is deliberately kept.
	seed := func(t *testing.T) (dir, inviteFile string) {
		t.Helper()
		dir = t.TempDir()
		inviteFile = writeInviteFile(t, cliInviteBlob(t, unreachableBus), 0o600)
		res := runCLIIn(t, dir, "enrol", "--invite-file", inviteFile, "--name", "planner", "--idempotency-key", key)
		if res.Code != client.ExitNetwork {
			t.Fatalf("the setup enrol exited %d, want %d (unreachable bus); stderr=%q", res.Code, client.ExitNetwork, res.Stderr)
		}
		return dir, inviteFile
	}

	t.Run("whoami --all names the invite and prints a resume command that works", func(t *testing.T) {
		dir, _ := seed(t)

		res := runCLIIn(t, dir, "whoami", "--all")
		if res.Code != client.ExitOK {
			t.Fatalf("whoami --all exited %d, want %d; stderr=%q", res.Code, client.ExitOK, res.Stderr)
		}
		if !strings.Contains(res.Stdout, cliInviteID) {
			t.Errorf("whoami --all does not name the invite the unfinished enrolment redeems; the resume depends on it:\n%s", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "--invite-file") {
			t.Errorf("the resume line does not mention --invite-file, so following it verbatim is refused (exit 2):\n%s", res.Stdout)
		}
		if !strings.Contains(res.Stdout, "--idempotency-key "+key) {
			t.Errorf("the resume line does not carry the idempotency key, which is the only handle that reaches the stored key material:\n%s", res.Stdout)
		}
		// The OLD line, which is now a command that fails. Its absence is the
		// regression this subtest exists for.
		if strings.Contains(res.Stdout, "--bus "+unreachableBus+" --name") {
			t.Errorf("whoami --all still prints the --bus resume form for an INVITED attempt; that command is now refused locally:\n%s", res.Stdout)
		}
		if strings.Contains(res.Stdout, cliInviteSecret) {
			t.Fatalf("whoami --all printed the invite SECRET:\n%s", res.Stdout)
		}
	})

	t.Run("--json reports invite_id on the pending entry and never the secret", func(t *testing.T) {
		dir, _ := seed(t)

		res := runCLIIn(t, dir, "--json", "whoami", "--all")
		if res.Code != client.ExitOK {
			t.Fatalf("whoami --all --json exited %d; stderr=%q", res.Code, res.Stderr)
		}
		var parsed struct {
			Pending []struct {
				IdempotencyKey string `json:"idempotency_key"`
				InviteID       string `json:"invite_id"`
			} `json:"pending"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &parsed); err != nil {
			t.Fatalf("parsing whoami --json: %v\n%s", err, res.Stdout)
		}
		if len(parsed.Pending) != 1 {
			t.Fatalf("whoami --json reported %d pending enrolments, want 1: %s", len(parsed.Pending), res.Stdout)
		}
		if parsed.Pending[0].InviteID != cliInviteID {
			t.Errorf("pending[0].invite_id = %q, want %q — an agent has to be able to find the invite its resume needs without parsing prose", parsed.Pending[0].InviteID, cliInviteID)
		}
		if strings.Contains(res.Stdout, cliInviteSecret) {
			t.Fatalf("whoami --json printed the invite SECRET: %s", res.Stdout)
		}
	})

	t.Run("a hostile STORED invite id cannot forge a line on the terminal", func(t *testing.T) {
		// Defence in depth, and the second of two independent checks. The id
		// was charset-checked by client.Invite.Validate before it was written,
		// so the ONLY way one of these reaches the store is a hand-edited file
		// — which is exactly what this does, because a guard nothing exercises
		// is a guard that gets deleted as dead code.
		dir, _ := seed(t)

		path := filepath.Join(dir, "identities.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the store: %v", err)
		}
		var store map[string]interface{}
		if err := json.Unmarshal(raw, &store); err != nil {
			t.Fatalf("decoding the store: %v", err)
		}
		pending, _ := store["pending"].([]interface{})
		if len(pending) != 1 {
			t.Fatalf("the store holds %d pending records, want 1", len(pending))
		}
		rec, _ := pending[0].(map[string]interface{})
		rec["invite_id"] = "\x1b[2K\ragent-busctl: verified OK"
		edited, err := json.Marshal(store)
		if err != nil {
			t.Fatalf("re-encoding the store: %v", err)
		}
		if err := os.WriteFile(path, edited, 0o600); err != nil {
			t.Fatalf("writing the store: %v", err)
		}

		res := runCLIIn(t, dir, "whoami", "--all")
		if strings.Contains(res.Stdout, "\x1b") {
			t.Errorf("whoami --all wrote a raw ESC, which can erase and rewrite the line it prints on:\n%q", res.Stdout)
		}
		if strings.Contains(res.Stdout, "\r") {
			t.Errorf("whoami --all wrote a raw CR, which returns the cursor to the start of the line:\n%q", res.Stdout)
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "agent-busctl: verified OK") {
				t.Errorf("whoami --all produced a line beginning with the forged success text, which is indistinguishable from a real one: %q", line)
			}
		}
	})

	t.Run("resuming with a different invite exits 2 and keeps the key material", func(t *testing.T) {
		dir, _ := seed(t)

		// A second, well-formed invite for the same bus — the wrong file, taken
		// from the same directory by an operator who has minted two.
		other := client.Invite{}
		if err := json.Unmarshal(cliInviteBlob(t, unreachableBus), &other); err != nil {
			t.Fatalf("decoding the blob: %v", err)
		}
		other.InviteID = "inv-01H8XCLIOTHERINVITE"
		otherBlob, err := json.Marshal(other)
		if err != nil {
			t.Fatalf("marshalling the second invite: %v", err)
		}
		otherFile := writeInviteFile(t, otherBlob, 0o600)

		res := runCLIIn(t, dir, "enrol", "--invite-file", otherFile, "--name", "planner", "--idempotency-key", key)
		if res.Code != client.ExitUsage {
			t.Fatalf("resuming with a different invite exited %d, want %d (a local refusal, not a bus refusal); stderr=%q", res.Code, client.ExitUsage, res.Stderr)
		}
		for _, want := range []string{cliInviteID, other.InviteID, "KEPT"} {
			if !strings.Contains(res.Stderr, want) {
				t.Errorf("the refusal does not mention %q; it must name both invites and say the key material is kept:\n%s", want, res.Stderr)
			}
		}

		// And the record is still listed, which is what makes it recoverable.
		after := runCLIIn(t, dir, "whoami", "--all")
		if !strings.Contains(after.Stdout, "--idempotency-key "+key) {
			t.Fatalf("the unfinished enrolment is GONE after the refusal; its private key seeds were the only copy:\n%s", after.Stdout)
		}
	})
}
