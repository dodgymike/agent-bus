package relay

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// modulePath is this package's import path, as it would appear in another
// package's import block.
const modulePath = "github.com/dodgymike/agent-bus/internal/relay"

// repoRoot locates the repository root from this test file's own location, and
// fails rather than skips if it cannot: a guard that quietly does nothing is
// worse than no guard, because it reads as green.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file, so the guard cannot run")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/relay -> internal -> repo root
	// Anchor on two files that must exist, so a wrong root fails loudly here
	// instead of silently walking an empty tree and passing.
	for _, anchor := range []string{"CLAUDE.md", filepath.Join("internal", "httpapi")} {
		if _, err := os.Stat(filepath.Join(root, anchor)); err != nil {
			t.Fatalf("repo root %q does not contain %s (%v); the guard would scan the wrong tree", root, anchor, err)
		}
	}
	return root
}

// TestHandshakeHandlerIsNotWiredIntoAnyMux is the guard the package doc
// promises: NOTHING outside internal/relay may import internal/relay.
//
// The handler authenticates no peer, so serving it would create exactly the
// ungated federation-enrolment path INVITE-PEERGUARD (f5d91dbe) exists to
// forbid, on a link MTLS-RELAYGUARD (8192c3c7) has not yet made mutually
// authenticated. Both must land before this package is reachable from a
// listener — and when they do, THIS TEST is the thing to change deliberately,
// which is the entire point of it failing loudly first.
func TestHandshakeHandlerIsNotWiredIntoAnyMux(t *testing.T) {
	root := repoRoot(t)
	selfDir := filepath.Join(root, "internal", "relay")

	var importers []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch {
			case path == selfDir:
				return filepath.SkipDir
			case info.Name() == ".git", info.Name() == "vendor", info.Name() == "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), `"`+modulePath+`"`) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			importers = append(importers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(importers) != 0 {
		t.Fatalf("these files import %s: %v\n"+
			"The peer handshake handler authenticates NOTHING and must not be reachable from any mux "+
			"until INVITE-PEERGUARD (f5d91dbe, invite redemption is the only route onto the bus, including "+
			"for peer buses) and MTLS-RELAYGUARD (8192c3c7, bus-to-bus links are mutually authenticated) "+
			"have landed. If you are one of those tasks, change this test deliberately as part of that work.",
			modulePath, importers)
	}
}

// TestPackageDocNamesTheGatingTasks pins the warning in doc.go.
//
// The doc comment IS the control here: it is what the next agent reads before
// deciding whether wiring this up is safe. If the warning is softened or the
// task keys are dropped, the next agent gets a handler that looks ready to
// serve, so the text is asserted rather than trusted.
func TestPackageDocNamesTheGatingTasks(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file")
	}
	docPath := filepath.Join(filepath.Dir(thisFile), "doc.go")
	src, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}
	doc := string(src)

	for _, must := range []string{
		"NOT REGISTERED ON ANY MUX",
		"INVITE-PEERGUARD",
		"f5d91dbe",
		"MTLS-RELAYGUARD",
		"8192c3c7",
	} {
		if !strings.Contains(doc, must) {
			t.Errorf("doc.go no longer contains %q; the package doc is the warning the next agent reads before wiring this handler up", must)
		}
	}
}

// TestIdempotencyKeyCapMatchesTheRestOfTheServer pins the duplicated constant.
//
// relay deliberately re-states the key rule instead of importing hub, so one
// key can travel from a peer handshake into the durable applied-key table
// unchanged (invariant 10). Duplication that is allowed to drift is worse than
// coupling, so the value is asserted here; hub.MaxIdempotencyKeyLen,
// store.MaxIdempotencyKeyLen and auth.MaxIdempotencyKeyLen are all 128.
func TestIdempotencyKeyCapMatchesTheRestOfTheServer(t *testing.T) {
	if MaxIdempotencyKeyLen != 128 {
		t.Fatalf("MaxIdempotencyKeyLen = %d, want 128 to match hub/store/auth", MaxIdempotencyKeyLen)
	}
}
