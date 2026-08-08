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
// correct handler already tolerates — so refusing to start `agent-busctl watch`
// because a hint is damaged would trade a harmless replay for an outage.
//
// Every damaged form must therefore yield "" plus a Store WARNING and NO error.
// The warning is the load-bearing half: a silent fallback would look identical
// to "you have never watched before".
func TestCLIWatchCursorStoreIgnoresDamagedFile(t *testing.T) {
	const (
		agentID = "bus-x.agent-1"
		// The bus half of the key is the SERVER-MINTED BUS ID, not a URL
		// (CLI-3-FU-URLKEY). busURL below is only the credential's dial address.
		busID  = "bus-x"
		busURL = "http://127.0.0.1:9"
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

			cursor, err := s.Cursor(agentID, busID)
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
		if err := s.SetCursor(agentID, busID, "cursor-42"); err != nil {
			t.Fatalf("SetCursor: %v", err)
		}
		got, err := s.Cursor(agentID, busID)
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
			if err := s.SetCursor(agentID, busID, "cursor-"+string(rune('a'+i))); err != nil {
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

// TestCursorStore_KeyedByBusID_NotURL is CLI-3-FU-URLKEY.
//
// # The field event this comes from
//
// A real agent migrated across a plaintext -> TLS switch and its cursors.json
// ended up holding TWO entries for ONE agent id:
//
//	{agent_id: …mic-array-1, bus_url: http://127.0.0.1:18080,  cursor: …|266}
//	{agent_id: …mic-array-1, bus_url: https://127.0.0.1:18080, cursor: …|266}
//
// The https entry started EMPTY, so the first watch after the flip re-received
// the agent's entire history. At-least-once delivery permits that and message_id
// dedup absorbs it — the problem is the SCOPE. It fires for every agent on the
// bus simultaneously, the moment TLS is turned on, and any handler that reacts
// per-message rather than deduping on message_id re-acts on its whole history at
// once.
//
// # Why the bus id is the right key
//
// The server-minted bus id is the durable answer to "is this the same bus"
// (invariant 2, `<bus-id>.<agent-id>`). A URL is not: the SAME bus is reachable
// at http:// and https:// during a migration window, and also after a port move,
// a DNS change or a reverse proxy appearing — all of which are one bus that
// should share one cursor.
func TestCursorStore_KeyedByBusID_NotURL(t *testing.T) {
	const (
		agentID = "bus-x.mic-array-1"
		busID   = "bus-x"
	)

	t.Run("a scheme flip reuses the same entry", func(t *testing.T) {
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}

		// Watched over plaintext, cursor persisted.
		if err := s.SetCursor(agentID, busID, "cursor-266"); err != nil {
			t.Fatalf("SetCursor: %v", err)
		}

		// The bus flips to TLS. Same bus, same server-minted bus id, different
		// URL scheme — and the cursor must survive it.
		got, err := s.Cursor(agentID, busID)
		if err != nil {
			t.Fatalf("Cursor after the flip: %v", err)
		}
		if got != "cursor-266" {
			t.Fatalf("Cursor after a plaintext->TLS flip = %q, want %q — a fresh empty entry replays the agent's whole history, for EVERY agent at once",
				got, "cursor-266")
		}

		// And exactly ONE record exists, not one per scheme.
		d, ok := s.loadCursors()
		if !ok {
			t.Fatalf("loadCursors reported the file unusable")
		}
		if len(d.Cursors) != 1 {
			t.Fatalf("cursors.json holds %d records for one (agent, bus), want 1: %+v", len(d.Cursors), d.Cursors)
		}
		if d.Cursors[0].BusID != busID {
			t.Fatalf("record bus_id = %q, want %q", d.Cursors[0].BusID, busID)
		}
	})

	t.Run("different buses still keep different cursors", func(t *testing.T) {
		// The control. Keying by bus id must not collapse two genuinely
		// different buses onto one position: a cursor is bound to ONE bus's
		// sequence space, and applying bus A's position to bus B would SKIP
		// messages B never delivered — far worse than replaying.
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		if err := s.SetCursor("bus-a.agent-1", "bus-a", "cursor-a"); err != nil {
			t.Fatalf("SetCursor(bus-a): %v", err)
		}
		if err := s.SetCursor("bus-b.agent-1", "bus-b", "cursor-b"); err != nil {
			t.Fatalf("SetCursor(bus-b): %v", err)
		}
		for _, tc := range []struct{ agent, bus, want string }{
			{"bus-a.agent-1", "bus-a", "cursor-a"},
			{"bus-b.agent-1", "bus-b", "cursor-b"},
		} {
			got, err := s.Cursor(tc.agent, tc.bus)
			if err != nil {
				t.Fatalf("Cursor(%s,%s): %v", tc.agent, tc.bus, err)
			}
			if got != tc.want {
				t.Fatalf("Cursor(%s,%s) = %q, want %q", tc.agent, tc.bus, got, tc.want)
			}
		}
	})

	t.Run("an existing url-keyed file migrates instead of replaying", func(t *testing.T) {
		// The upgrade path, and the whole point of doing this rather than just
		// changing the key: an agent that upgrades agent-busctl must NOT replay
		// its history as the price of the fix. The two url-keyed records below
		// are the field report's, verbatim in shape.
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		legacy := `{"version":1,"cursors":[
			{"agent_id":"bus-x.mic-array-1","bus_url":"http://127.0.0.1:18080","cursor":"cursor-200","updated_at":"2026-08-07T10:00:00Z"},
			{"agent_id":"bus-x.mic-array-1","bus_url":"https://127.0.0.1:18080","cursor":"cursor-266","updated_at":"2026-08-07T11:00:00Z"}
		]}`
		if err := os.WriteFile(s.CursorPath(), []byte(legacy), storeFileMode); err != nil {
			t.Fatalf("seeding the legacy cursor file: %v", err)
		}

		got, err := s.Cursor(agentID, busID)
		if err != nil {
			t.Fatalf("Cursor over a legacy file: %v", err)
		}
		// The most recently updated of the two colliding records wins. A cursor
		// is opaque, so "furthest along" is not a question this client can ask;
		// the timestamp is the only ordering available, and choosing the newest
		// replays at most the gap between them rather than the whole history.
		if got != "cursor-266" {
			t.Fatalf("Cursor over a legacy url-keyed file = %q, want %q — an upgrade must not replay the agent's history", got, "cursor-266")
		}

		// The collision is collapsed on the next write, so the file does not
		// carry the duplicate forever.
		if err := s.SetCursor(agentID, busID, "cursor-300"); err != nil {
			t.Fatalf("SetCursor: %v", err)
		}
		d, ok := s.loadCursors()
		if !ok {
			t.Fatalf("loadCursors reported the file unusable")
		}
		if len(d.Cursors) != 1 {
			t.Fatalf("after migration cursors.json holds %d records, want 1: %+v", len(d.Cursors), d.Cursors)
		}
		if d.Cursors[0].Cursor != "cursor-300" {
			t.Fatalf("migrated record cursor = %q, want %q", d.Cursors[0].Cursor, "cursor-300")
		}
	})

	t.Run("ClearCursor is keyed the same way", func(t *testing.T) {
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		if err := s.SetCursor(agentID, busID, "cursor-1"); err != nil {
			t.Fatalf("SetCursor: %v", err)
		}
		if err := s.ClearCursor(agentID, busID); err != nil {
			t.Fatalf("ClearCursor: %v", err)
		}
		got, err := s.Cursor(agentID, busID)
		if err != nil {
			t.Fatalf("Cursor: %v", err)
		}
		if got != "" {
			t.Fatalf("Cursor after ClearCursor = %q, want empty", got)
		}
	})
}

// TestCursorBusIDPrefersTheAgentIDPrefix pins which of the credential's two
// bus-id-shaped values the cursor key is built from, and what happens when they
// disagree.
//
// They are not validated to the same standard. cred.BusID is checked only by
// validateServerField (up to 256 bytes of [A-Za-z0-9._-], DOTS INCLUDED), while
// the prefix of cred.AgentID is checked against busIDRegexp, the actual bus-id
// grammar, which excludes '.' precisely because '.' is what qualifies an agent
// id (invariant 2). The stricter value wins, and a disagreement is refused
// rather than resolved — the two would be claiming different sequence spaces for
// the same cursor.
func TestCursorBusIDPrefersTheAgentIDPrefix(t *testing.T) {
	cases := []struct {
		name    string
		agentID string
		busID   string
		want    string
		wantErr bool
	}{
		{name: "agreeing", agentID: "bus-x.agent-1", busID: "bus-x", want: "bus-x"},
		{name: "absent bus id is derived", agentID: "bus-x.agent-1", busID: "", want: "bus-x"},
		{name: "disagreeing is refused", agentID: "bus-x.agent-1", busID: "bus-y", wantErr: true},
		// The exact shape validateServerField admits and busIDRegexp does not.
		{name: "dotted bus id is refused", agentID: "bus-x.agent-1", busID: "bus-x.evil", wantErr: true},
		{name: "unqualified agent id is refused", agentID: "agent-1", busID: "bus-x", wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := cursorBusID(Credential{Identity: Identity{AgentID: tc.agentID, BusID: tc.busID}})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cursorBusID(agent=%q, bus=%q) = %q, want an error", tc.agentID, tc.busID, got)
				}
				if KindOf(err) != KindConfig {
					t.Fatalf("Kind = %q, want %q — a self-contradictory stored identity is a local config fault", KindOf(err), KindConfig)
				}
				return
			}
			if err != nil {
				t.Fatalf("cursorBusID(agent=%q, bus=%q) = %v", tc.agentID, tc.busID, err)
			}
			if got != tc.want {
				t.Fatalf("cursorBusID(agent=%q, bus=%q) = %q, want %q", tc.agentID, tc.busID, got, tc.want)
			}
		})
	}
}

// TestPromotePendingClearsAReplacedIdentitysCursor is the P1 the security gate
// found against CLI-3-FU-URLKEY, and it is the one failure mode in this client
// that SKIPS rather than replays.
//
// findCredential matches on AgentID alone, so re-enrolling under an agent id
// already in the store OVERWRITES it. Since the cursor is keyed by (agent id,
// bus id) — both derivable from that same agent id — the incoming credential
// would otherwise inherit the read position the previous holder reached.
//
// Why that is worse than it sounds: the bus's cursor is deliberately unsigned
// and NOT bus-scoped (internal/hub's DecodeCursor binds only the agent half), so
// a position minted elsewhere is ACCEPTED and only messages after it come back.
// Under the old bus_url key the two records sat under different URLs and could
// not collide; re-keying removed that accidental separation, so it now has to be
// deliberate. Everything else in this client fails towards replay; this is the
// one place that could fail towards silent loss.
func TestPromotePendingClearsAReplacedIdentitysCursor(t *testing.T) {
	const agentID = "bus-x.agent-1"

	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	first, _ := newTestCredential(t, agentID)
	first.BusURL = "https://bus-one.example:8080"
	if err := s.PromotePending("", first, true); err != nil {
		t.Fatalf("PromotePending(first): %v", err)
	}

	busID, err := cursorBusID(first)
	if err != nil {
		t.Fatalf("cursorBusID: %v", err)
	}
	if err := s.SetCursor(agentID, busID, "cursor-far-ahead"); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	// Control: the position really is stored, so the assertion below is about
	// it being CLEARED and not about it never having been there.
	if got, err := s.Cursor(agentID, busID); err != nil || got != "cursor-far-ahead" {
		t.Fatalf("Cursor = %q, %v before re-enrolment; want the position to be stored, or this test proves nothing", got, err)
	}

	// Re-enrol the SAME agent id. This overwrites the credential in place.
	second, _ := newTestCredential(t, agentID)
	second.BusURL = "https://bus-two.example:8080"
	if err := s.PromotePending("", second, true); err != nil {
		t.Fatalf("PromotePending(second): %v", err)
	}

	got, err := s.Cursor(agentID, busID)
	if err != nil {
		t.Fatalf("Cursor after re-enrolment: %v", err)
	}
	if got != "" {
		t.Fatalf("a replaced credential inherited the previous holder's read position %q; the bus accepts a cursor it did not mint, so the next watch SKIPS everything before it — silent message loss, where every other failure here replays", got)
	}

	// The route the first version of this fix left open, reproduced by the
	// security gate: `logout` REMOVES the credential but does not touch
	// cursors.json, so a logout followed by an enrolment lands on the APPEND
	// path — nothing is "replaced" — with the previous holder's position still
	// in the file under a key the new credential derives identically.
	t.Run("the logout then re-enrol append path does not inherit either", func(t *testing.T) {
		dir := t.TempDir()
		s, err := OpenStore(dir)
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}
		before, _ := newTestCredential(t, agentID)
		if err := s.PromotePending("", before, true); err != nil {
			t.Fatalf("PromotePending(before): %v", err)
		}
		busID, err := cursorBusID(before)
		if err != nil {
			t.Fatalf("cursorBusID: %v", err)
		}
		if err := s.SetCursor(agentID, busID, "cursor-far-ahead"); err != nil {
			t.Fatalf("SetCursor: %v", err)
		}

		// logout: the credential goes, the cursor file is untouched.
		if _, _, err := s.Remove(agentID); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if got, err := s.Cursor(agentID, busID); err != nil || got != "cursor-far-ahead" {
			t.Fatalf("Cursor = %q, %v after logout; the fixture requires the position to SURVIVE removal, or this test proves nothing", got, err)
		}

		// Enrol again. findCredential finds nothing, so this is the append path.
		after, _ := newTestCredential(t, agentID)
		if err := s.PromotePending("", after, true); err != nil {
			t.Fatalf("PromotePending(after): %v", err)
		}
		if got, err := s.Cursor(agentID, busID); err != nil || got != "" {
			t.Fatalf("a freshly appended credential inherited %q from a previous holder of the same id; the clear must be UNCONDITIONAL, not scoped to the overwrite path", got)
		}
	})

	t.Run("a first enrolment does not disturb an unrelated cursor", func(t *testing.T) {
		// The clear must be scoped to the identity actually replaced. A blanket
		// clear on every enrolment would replay every other agent's history.
		const other = "bus-x.other-9"
		if err := s.SetCursor(other, "bus-x", "cursor-other"); err != nil {
			t.Fatalf("SetCursor(other): %v", err)
		}
		fresh, _ := newTestCredential(t, "bus-x.brand-new-3")
		if err := s.PromotePending("", fresh, false); err != nil {
			t.Fatalf("PromotePending(fresh): %v", err)
		}
		if got, err := s.Cursor(other, "bus-x"); err != nil || got != "cursor-other" {
			t.Fatalf("Cursor(other) = %q, %v after an unrelated first enrolment; want %q untouched", got, err, "cursor-other")
		}
	})
}
