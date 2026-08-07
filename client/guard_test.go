package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoInsecureSkipVerifyAnywhere is the standing-rule regression guard
// (DECISIONS.md, 2026-08-02, "E7: no plaintext escape hatch"): the string
// InsecureSkipVerify must appear NOWHERE under client/ or cmd/agent-busctl/,
// including in tests. A test that needs TLS uses httptest.NewTLSServer with
// srv.Client(), which verifies correctly without this flag.
func TestNoInsecureSkipVerifyAnywhere(t *testing.T) {
	roots := []string{".", filepath.Join("..", "cmd", "agent-busctl")}
	var scanned int
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// This file itself necessarily names the forbidden string in order
			// to check for it; excluding it from the scan is not a loophole —
			// every OTHER .go file, including every other test file, is still
			// scanned and still counted.
			if filepath.Base(path) == "guard_test.go" {
				return nil
			}
			scanned++
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(b), "InsecureSkipVerify") {
				t.Errorf("%s contains InsecureSkipVerify; this string must appear nowhere under client/ or cmd/agent-busctl/ (DECISIONS.md 2026-08-02 E7)", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 .go files under %v; this guard is vacuous", roots)
	}
}
