package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// seedTwoIdentities builds a credential store with two identities, cred1
// selected as the STORED current one. It uses only the exported client API so
// this test exercises exactly what an embedding agent could build too.
func seedTwoIdentities(t *testing.T) (dir string, currentAgentID, otherAgentID string) {
	t.Helper()
	dir = t.TempDir()
	s, err := client.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	cred1 := client.Credential{
		Identity: client.Identity{
			AgentID: "bus-x.current-1", BusID: "bus-x", Name: "current", BusURL: "http://127.0.0.1:1",
			PublicKey: "cHVi", EnrolledAt: "2026-08-02T00:00:00Z",
		},
		PrivateKeySeed: "c2VlZA==",
	}
	cred2 := client.Credential{
		Identity: client.Identity{
			AgentID: "bus-x.other-1", BusID: "bus-x", Name: "other", BusURL: "http://127.0.0.1:1",
			PublicKey: "cHVi", EnrolledAt: "2026-08-02T00:00:00Z",
		},
		PrivateKeySeed: "c2VlZA==",
	}
	if err := s.PromotePending("", cred1, true); err != nil {
		t.Fatalf("PromotePending(cred1): %v", err)
	}
	if err := s.PromotePending("", cred2, false); err != nil {
		t.Fatalf("PromotePending(cred2): %v", err)
	}
	return dir, cred1.AgentID, cred2.AgentID
}

// TestWhoamiIsCurrentComesFromStoreNotAsFlag checks fix 12: `whoami`'s
// is_current comes from the STORE's selection, not from the --as flag. It
// drives the selection via the injected lookupEnv (AGENT_BUS_AGENT_ID),
// exactly as a parallel agent sharing a credential store is documented to do,
// and checks both directions: selecting the non-current identity reports
// false, and selecting the actual current identity reports true.
func TestWhoamiIsCurrentComesFromStoreNotAsFlag(t *testing.T) {
	dir, currentID, otherID := seedTwoIdentities(t)

	envFor := func(agentID string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			if key == client.EnvAgentID {
				return agentID, true
			}
			return "", false
		}
	}

	t.Run("AGENT_BUS_AGENT_ID selects the non-current identity", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"--identity", dir, "whoami", "--json"}, &stdout, &stderr, envFor(otherID))
		if code != client.ExitOK {
			t.Fatalf("run(whoami) = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
			t.Fatalf("stdout not JSON: %v (%q)", err, stdout.String())
		}
		if got, _ := parsed["agent_id"].(string); got != otherID {
			t.Fatalf("agent_id = %q, want %q", got, otherID)
		}
		if isCurrent, _ := parsed["is_current"].(bool); isCurrent {
			t.Fatalf("is_current = true for the NON-current identity %q, want false", otherID)
		}
	})

	t.Run("AGENT_BUS_AGENT_ID selects the current identity", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"--identity", dir, "whoami", "--json"}, &stdout, &stderr, envFor(currentID))
		if code != client.ExitOK {
			t.Fatalf("run(whoami) = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
			t.Fatalf("stdout not JSON: %v (%q)", err, stdout.String())
		}
		if got, _ := parsed["agent_id"].(string); got != currentID {
			t.Fatalf("agent_id = %q, want %q", got, currentID)
		}
		if isCurrent, _ := parsed["is_current"].(bool); !isCurrent {
			t.Fatalf("is_current = false for the STORED current identity %q, want true", currentID)
		}
	})
}

// TestCLINegativeTimeoutIsUsageError checks --timeout -1s is rejected with
// ExitUsage (2), not silently ignored or falling back to the default. The
// flag path must refuse it the same way AGENT_BUS_TIMEOUT=-1s already does.
func TestCLINegativeTimeoutIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "--timeout", "-1s", "whoami"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(--timeout -1s whoami) = %d, want %d; stdout=%q stderr=%q", code, client.ExitUsage, stdout.String(), stderr.String())
	}
}

// TestCLIJSONHonouredOnFlagParseErrorAfterSubcommand checks fix 12:
// `busctl whoami --json --badflag` still renders a parseable JSON error
// object, because --json appears (and is parsed successfully) BEFORE the
// flag.FlagSet hits the unknown flag and fails.
func TestCLIJSONHonouredOnFlagParseErrorAfterSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "whoami", "--json", "--badflag"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(whoami --json --badflag) = %d, want %d; stdout=%q stderr=%q", code, client.ExitUsage, stdout.String(), stderr.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("stdout not parseable JSON, so --json was not honoured for a flag error after the subcommand name: %v (%q)", err, stdout.String())
	}
	if ok, _ := parsed["ok"].(bool); ok {
		t.Fatalf(`parsed["ok"] = %v, want false`, parsed["ok"])
	}
}
