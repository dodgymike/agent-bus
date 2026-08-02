package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// emptyEnv is a lookupEnv that never finds anything, mirroring an agent
// shelling out with no relevant environment set. Shared by every CLI test in
// this package.
func emptyEnv(string) (string, bool) { return "", false }

// TestCLIHelpExitsZeroAndMentionsEnrol checks --help is a success (exit 0)
// and that the root help text names every command, in particular enrol.
func TestCLIHelpExitsZeroAndMentionsEnrol(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitOK {
		t.Fatalf("run(--help) = %d, want %d; stderr=%q", code, client.ExitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "enrol") {
		t.Fatalf("--help output does not mention enrol: %q", stdout.String())
	}
}

// TestCLIUnknownCommandJSON checks an unrecognised subcommand fails with
// ExitUsage and, under --json, renders a parseable error object on stdout.
func TestCLIUnknownCommandJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--json", "bogus"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(--json bogus) = %d, want %d; stdout=%q stderr=%q", code, client.ExitUsage, stdout.String(), stderr.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("stdout not parseable JSON: %v (%q)", err, stdout.String())
	}
	if ok, _ := parsed["ok"].(bool); ok {
		t.Fatalf("parsed[\"ok\"] = %v, want false", parsed["ok"])
	}
}

// TestCLIGlobalFlagsBeforeAndAfterSubcommand checks --json (and --identity)
// are honoured whether they appear before or after the subcommand name.
func TestCLIGlobalFlagsBeforeAndAfterSubcommand(t *testing.T) {
	dir := t.TempDir()
	cases := [][]string{
		{"--json", "--identity", dir, "whoami"},
		{"--identity", dir, "whoami", "--json"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr, emptyEnv)
			if code != client.ExitConfig {
				t.Fatalf("run(%v) = %d, want %d (empty store); stdout=%q stderr=%q", args, code, client.ExitConfig, stdout.String(), stderr.String())
			}
			var parsed map[string]interface{}
			if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
				t.Fatalf("run(%v): stdout not parseable JSON, so --json was not honoured in this flag position: %v (%q)", args, err, stdout.String())
			}
		})
	}
}

// TestCLIWhoamiEmptyStoreExitsConfig checks whoami with nothing enrolled
// yields ExitConfig (3).
func TestCLIWhoamiEmptyStoreExitsConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "whoami"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitConfig {
		t.Fatalf("run(whoami) on an empty store = %d, want %d; stderr=%q", code, client.ExitConfig, stderr.String())
	}
}

// TestCLIWhoamiAllEmptyStoreExitsEmpty checks whoami --all with nothing
// enrolled yields ExitEmpty (8), the "nothing to report" code.
func TestCLIWhoamiAllEmptyStoreExitsEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "whoami", "--all"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitEmpty {
		t.Fatalf("run(whoami --all) on an empty store = %d, want %d; stderr=%q", code, client.ExitEmpty, stderr.String())
	}
}

// TestCLIUseNoArgExitsUsage checks `use` with no positional argument is
// ExitUsage (2), not a silent no-op or a panic.
func TestCLIUseNoArgExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "use"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(use) with no argument = %d, want %d; stderr=%q", code, client.ExitUsage, stderr.String())
	}
}

// TestCLILogoutAllWithPositionalExitsUsage checks `logout --all <name>` is
// rejected (ExitUsage) rather than silently ignoring one of the two.
func TestCLILogoutAllWithPositionalExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "logout", "--all", "someone"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(logout --all someone) = %d, want %d; stderr=%q", code, client.ExitUsage, stderr.String())
	}
}
