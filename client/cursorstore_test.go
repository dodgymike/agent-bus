package client

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCLIWatchCursorStoreIgnoresDamagedFile checks the cursor file is the
// DELIBERATE OPPOSITE of identities.json when it cannot be read.
//
// identities.json refuses an unknown version (TestStoreUnknownVersionYields-
// KindConfig): a credential misread is unrecoverable and a private key
// misparsed as a public one fails silently, so refusing to guess is the only
// safe move. A cursor is a POSITION HINT. Losing it re-delivers from the start
// of the retained window, which at-least-once delivery already permits and any
// correct handler already tolerates — so refusing to start `busctl watch`
// because a hint is damaged would trade a harmless replay for an outage.
//
// Every damaged form must therefore yield "" plus a Store WARNING and NO error.
// The warning is the load-bearing half: a silent fallback would look identical
// to "you have never watched before".
func TestCLIWatchCursorStoreIgnoresDamagedFile(t *testing.T) {
	const (
		agentID = "bus-x.agent-1"
		busURL  = "http://127.0.0.1:9"
	)

	cases := []struct {
		name     string
		contents string
	}{
		{"not JSON at all", "{this is not json"},
		{"truncated mid-object", `{"version":1,"cursors":[{"agent_id":"bus-x.agent-1"`},
		{"unknown format version", `{"version":99,"cursors":[{"agent_id":"bus-x.agent-1","bus_url":"http://127.0.0.1:9","cursor":"c1"}]}`},
		{"version from the future as a string", `{"version":"1","cursors":[]}`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			if err := os.WriteFile(s.CursorPath(), []byte(tc.contents), storeFileMode); err != nil {
				t.Fatalf("writing the damaged cursor file: %v", err)
			}

			cursor, err := s.Cursor(agentID, busURL)
			if err != nil {
				t.Fatalf("Cursor on a damaged file = %v, want NO error — a position hint must never block a watch", err)
			}
			if cursor != "" {
				t.Fatalf("Cursor = %q, want %q — a damaged file is an empty set, not a guess", cursor, "")
			}
			if warnings := s.Warnings(); len(warnings) == 0 {
				t.Fatalf("Warnings() is empty; a silently ignored cursor file is indistinguishable from having never watched before")
			}
		})
	}

	t.Run("a valid file still resolves", func(t *testing.T) {
		// The negative cases above would all pass against a Cursor that always
		// returned "". This is the control that says they are testing damage.
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		if err := s.SetCursor(agentID, busURL, "cursor-42"); err != nil {
			t.Fatalf("SetCursor: %v", err)
		}
		got, err := s.Cursor(agentID, busURL)
		if err != nil {
			t.Fatalf("Cursor: %v", err)
		}
		if got != "cursor-42" {
			t.Fatalf("Cursor = %q, want %q", got, "cursor-42")
		}
	})

	t.Run("a cursor write never rewrites identities.json", func(t *testing.T) {
		// The whole reason cursors live in their own file: a cursor advances on
		// every batch, credentials change almost never. Routing the hot loop
		// through identities.json would rewrite a file of Ed25519 SEEDS
		// hundreds of times more often than it has any reason to change — each
		// rewrite another window in which a complete copy of every private key
		// exists in a temp file — and would put the watch loop in lock
		// contention with enrolment.
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		cred, _ := newTestCredential(t, agentID)
		cred.BusURL = busURL
		if err := s.PromotePending("", cred, true); err != nil {
			t.Fatalf("PromotePending: %v", err)
		}

		before, err := os.ReadFile(s.Path())
		if err != nil {
			t.Fatalf("reading identities.json: %v", err)
		}

		for i := 0; i < 5; i++ {
			if err := s.SetCursor(agentID, busURL, "cursor-"+string(rune('a'+i))); err != nil {
				t.Fatalf("SetCursor: %v", err)
			}
		}

		after, err := os.ReadFile(s.Path())
		if err != nil {
			t.Fatalf("re-reading identities.json: %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("identities.json changed across five cursor writes;\nbefore: %s\nafter:  %s", before, after)
		}

		info, err := os.Stat(s.CursorPath())
		if err != nil {
			t.Fatalf("stat %s: %v", s.CursorPath(), err)
		}
		if perm := info.Mode().Perm(); perm != storeFileMode {
			t.Fatalf("cursor file mode = %#o, want %#o", perm, storeFileMode)
		}
		if filepath.Dir(s.CursorPath()) != filepath.Dir(s.Path()) {
			t.Fatalf("the cursor file is not beside the credential file: %s vs %s", s.CursorPath(), s.Path())
		}
		if s.CursorPath() == s.Path() {
			t.Fatalf("the cursor file and the credential file are the same path: %s", s.CursorPath())
		}
	})
}
