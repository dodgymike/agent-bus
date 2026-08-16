package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newPersistTestClient builds a Client over a throwaway store, with a fixed
// clock so expiry is decided by the test rather than by wall time.
func newPersistTestClient(t *testing.T, now time.Time) (*Client, Credential) {
	t.Helper()
	dir := t.TempDir()
	c, err := New(Config{
		BusURL:         "https://127.0.0.1:8080",
		BusFingerprint: strings.Repeat("ab", 32),
		IdentityDir:    dir,
		PersistSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.nowFn = func() time.Time { return now }
	cred := Credential{Identity: Identity{
		AgentID: "bus-test.agent-1",
		BusID:   "bus-test",
		Name:    "agent",
		BusURL:  "https://127.0.0.1:8080",
	}}
	return c, cred
}

func liveSession(now time.Time) *session {
	return &session{
		agentID:   "bus-test.agent-1",
		token:     "TOKEN-DO-NOT-LEAK",
		expiresAt: now.Add(time.Hour),
		refreshAt: now.Add(45 * time.Minute),
		lifetime:  time.Hour,
	}
}

// TestPersistedSessionRoundTrip is the base case: what is saved is what comes
// back, token included.
func TestPersistedSessionRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)

	want := liveSession(now)
	c.savePersistedSession(cred, want)

	got, ok := c.loadPersistedSession(cred)
	if !ok {
		t.Fatalf("loadPersistedSession: not found after save")
	}
	if got.token != want.token {
		t.Errorf("token = %q, want %q", got.token, want.token)
	}
	if got.agentID != want.agentID {
		t.Errorf("agentID = %q, want %q", got.agentID, want.agentID)
	}
	if !got.expiresAt.Equal(want.expiresAt) {
		t.Errorf("expiresAt = %v, want %v", got.expiresAt, want.expiresAt)
	}
	if got.lifetime != want.lifetime {
		t.Errorf("lifetime = %v, want %v", got.lifetime, want.lifetime)
	}
}

// TestPersistedSessionFileIsOwnerOnly is the security base case. The file holds
// a bearer token; if it is ever created group- or world-readable, every other
// check in this file is decoration.
func TestPersistedSessionFileIsOwnerOnly(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.savePersistedSession(cred, liveSession(now))

	path := filepath.Join(c.store.Dir(), sessionFilePrefix+cred.AgentID+".json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("persisted session is mode %04o, want 0600 — it holds a bearer token", perm)
	}
}

// TestPersistedSessionRefusedWhenWorldReadable proves the READ side of the mode
// check, which is the half that protects a file someone else loosened. It must
// not be presented, and the caller must be TOLD rather than silently
// re-handshaking, because a readable token may already have been stolen.
func TestPersistedSessionRefusedWhenWorldReadable(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.savePersistedSession(cred, liveSession(now))

	path := filepath.Join(c.store.Dir(), sessionFilePrefix+cred.AgentID+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, ok := c.loadPersistedSession(cred); ok {
		t.Fatal("a 0644 session file was ACCEPTED; a token other local users can read must never be presented")
	}
	c.warnMu.Lock()
	warnings := strings.Join(c.warnings, "\n")
	c.warnMu.Unlock()
	if !strings.Contains(warnings, "0644") {
		t.Errorf("no warning naming the mode; got %q", warnings)
	}

	// The evidence must SURVIVE. It is moved aside rather than left at the
	// original path, because savePersistedSession would otherwise overwrite
	// that path 0600 later in the same command and destroy it — which also
	// made the warning's remedy name a file that no longer existed.
	if _, err := os.Stat(path + ".INSECURE"); err != nil {
		t.Errorf("the world-readable token was not preserved at %s.INSECURE: %v", path, err)
	}
	if !strings.Contains(warnings, ".INSECURE") {
		t.Errorf("the warning does not tell the operator where the evidence went; got %q", warnings)
	}

	// And a later save in the SAME process must not resurrect it at the live
	// path as if nothing happened.
	c.savePersistedSession(cred, liveSession(now))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after save: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the replacement file is mode %04o, want 0600", perm)
	}
}

// TestPersistedSessionBindingIsEnforced covers the three ways a file can be the
// wrong file. Each must be a silent miss, never a presented token: handing a
// token to a bus it was not issued by LEAKS it to that bus.
func TestPersistedSessionBindingIsEnforced(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		mutta func(*persistedSession)
	}{
		{"wrong agent id", func(d *persistedSession) { d.AgentID = "bus-test.someone-else-1" }},
		{"wrong bus url", func(d *persistedSession) { d.BusURL = "https://evil.example:8080" }},
		{"unknown version", func(d *persistedSession) { d.Version = sessionFileVersion + 1 }},
		{"empty token", func(d *persistedSession) { d.Token = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, cred := newPersistTestClient(t, now)
			path := filepath.Join(c.store.Dir(), sessionFilePrefix+cred.AgentID+".json")

			s := liveSession(now)
			doc := persistedSession{
				Version:         sessionFileVersion,
				AgentID:         s.agentID,
				BusURL:          cred.BusURL,
				Token:           s.token,
				ExpiresAt:       s.expiresAt,
				RefreshAt:       s.refreshAt,
				LifetimeSeconds: s.lifetime.Seconds(),
			}
			tc.mutta(&doc)
			if err := writeSessionFile(path, doc); err != nil {
				t.Fatalf("writeSessionFile: %v", err)
			}

			if _, ok := c.loadPersistedSession(cred); ok {
				t.Fatalf("%s was ACCEPTED; a mis-bound session file must never be presented", tc.name)
			}
		})
	}
}

// TestPersistedSessionExpiryUsesTheSameRule guards against the laxer-path bug:
// a token off disk must face exactly the rule a token in memory faces, not a
// weaker one. Past the refresh point it is unusable from either source.
func TestPersistedSessionExpiryUsesTheSameRule(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.savePersistedSession(cred, liveSession(now))

	// Move past the refresh point (45m) but still inside expiry (60m). The
	// in-memory rule refuses here; disk must refuse identically.
	c.nowFn = func() time.Time { return now.Add(50 * time.Minute) }
	if _, ok := c.loadPersistedSession(cred); ok {
		t.Fatal("a session past its refresh point was loaded from disk; the disk path must not be laxer than the memory path")
	}
}

// TestSessionFileNameRefusesTraversal is the path-injection guard. The agent id
// reaches sessionFileName from --as and from a store document, so an id
// carrying a separator would write outside the store.
func TestSessionFileNameRefusesTraversal(t *testing.T) {
	bad := []string{
		"../../etc/passwd",
		"bus/../../x",
		"bus-test.agent\x00",
		"..",
		".",
		"",
		"bus test.agent-1",
		"bus-test.agent-1/",
	}
	for _, id := range bad {
		if name, err := sessionFileName(id); err == nil {
			t.Errorf("sessionFileName(%q) = %q, want an error — this becomes a file path", id, name)
		}
	}
	if _, err := sessionFileName("bus-test.agent-1"); err != nil {
		t.Errorf("sessionFileName rejected a legitimate id: %v", err)
	}
}

// TestForgetPersistedSessionRemovesTokenAndTemp proves logout actually removes
// the token — including the temp file an interrupted write leaves behind, which
// holds the same token and would otherwise survive the command.
func TestForgetPersistedSessionRemovesTokenAndTemp(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.savePersistedSession(cred, liveSession(now))

	path := filepath.Join(c.store.Dir(), sessionFilePrefix+cred.AgentID+".json")
	// Temp files carry a RANDOM suffix (as Store.saveJSON does), so seed the
	// shape that a SIGKILL mid-write actually leaves behind. A fixed ".tmp"
	// name was a cross-process race: two concurrent writers unlinked each
	// other's in-flight file.
	seeded := []string{path + ".tmp-deadbeef", path + ".tmp-cafebabe", path + ".INSECURE"}
	for _, p := range seeded {
		if err := os.WriteFile(p, []byte(`{"token":"TOKEN-DO-NOT-LEAK"}`), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	removed, err := c.ForgetPersistedSession(cred.AgentID)
	if err != nil {
		t.Fatalf("ForgetPersistedSession: %v", err)
	}
	if !removed {
		t.Fatal("removed = false, want true — a session file existed")
	}
	mustBeGone := append([]string{path}, seeded...)
	for _, p := range mustBeGone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after logout (err=%v); it holds a bearer token", p, err)
		}
	}

	// Second call: nothing to remove, and that is NOT an error.
	removed, err = c.ForgetPersistedSession(cred.AgentID)
	if err != nil {
		t.Errorf("second ForgetPersistedSession returned an error: %v", err)
	}
	if removed {
		t.Error("removed = true on the second call, want false")
	}
}

// TestForgetPersistedSessionDropsMemoryCopy — deleting the file while the
// process keeps presenting the same token in memory would make logout a lie for
// any long-lived embedding client.
func TestForgetPersistedSessionDropsMemoryCopy(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.session = liveSession(now)

	if _, err := c.ForgetPersistedSession(cred.AgentID); err != nil {
		t.Fatalf("ForgetPersistedSession: %v", err)
	}
	c.mu.Lock()
	held := c.session
	c.mu.Unlock()
	if held != nil {
		t.Fatal("the in-memory session survived `session logout`; the token is still presentable")
	}
}

// TestPersistSessionOffWritesNothing is the default-safety case. With the
// option off, no bearer token may reach the disk at all.
func TestPersistSessionOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{
		BusURL:         "https://127.0.0.1:8080",
		BusFingerprint: strings.Repeat("ab", 32),
		IdentityDir:    dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.cfg.PersistSession {
		t.Fatal("PersistSession defaulted to TRUE; the safe default is off")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), sessionFilePrefix) {
			t.Errorf("a session file %s exists with PersistSession off", e.Name())
		}
	}
}

// TestEnvPersistSessionIsAClosedSet proves AGENT_BUS_PERSIST_SESSION=0 and
// =false do NOT enable persistence. "non-empty means true" would turn an
// operator's attempt to DISABLE this into an instruction to write tokens to
// disk — the failure direction that matters.
func TestEnvPersistSessionIsAClosedSet(t *testing.T) {
	on := []string{"1", "true", "TRUE", "yes", "on", " true "}
	off := []string{"0", "false", "no", "off", "", "maybe", "2", "-1"}

	for _, v := range on {
		if !envTruthy(v) {
			t.Errorf("envTruthy(%q) = false, want true", v)
		}
	}
	for _, v := range off {
		if envTruthy(v) {
			t.Errorf("envTruthy(%q) = TRUE, want false — this enables writing a bearer token to disk", v)
		}
	}
}

// TestPersistedSessionSurvivesAsJSONDocument pins the on-disk field names,
// which CONTRACTS-CLI.md documents as a contract surface.
func TestPersistedSessionSurvivesAsJSONDocument(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.savePersistedSession(cred, liveSession(now))

	body, err := os.ReadFile(filepath.Join(c.store.Dir(), sessionFilePrefix+cred.AgentID+".json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("the persisted session is not valid JSON: %v", err)
	}
	for _, field := range []string{"version", "agent_id", "bus_url", "token", "expires_at", "refresh_at", "lifetime_seconds"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("field %q is missing from the persisted session document", field)
		}
	}
}

// --- Wiring tests -----------------------------------------------------------
//
// Everything above exercises the helpers DIRECTLY. The review gate on
// 2026-08-16 pointed out that nothing exercised `ensureSession`, which is where
// the feature actually lives — so the headline claim ("N commands, one
// session") rested only on a manual run. These cover the wiring.

// TestPersistedSessionBindsToTheBusActuallyDialled is the regression for the
// HIGH finding of 2026-08-16.
//
// The binding check compared `doc.BusURL` against `cred.BusURL` — BOTH read off
// the stored credential, so it was a tautology that could never fire. Meanwhile
// `resolveBusURL` prefers `--bus` / AGENT_BUS_URL over the credential, so the
// flag moved the CONNECTION without moving the CHECK: a token saved for the
// honest bus was loaded and presented to whatever `--bus` named. Demonstrated
// leaking to a rogue loopback listener.
func TestPersistedSessionBindsToTheBusActuallyDialled(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()

	honest := "https://127.0.0.1:8080"
	cred := Credential{Identity: Identity{
		AgentID: "bus-test.agent-1", BusID: "bus-test", Name: "agent", BusURL: honest,
	}}

	// Save while pointed at the honest bus.
	saver, err := New(Config{BusURL: honest, BusFingerprint: strings.Repeat("ab", 32), IdentityDir: dir, PersistSession: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	saver.nowFn = func() time.Time { return now }
	saver.savePersistedSession(cred, liveSession(now))

	// A DIFFERENT client, same store and same credential, but --bus points
	// somewhere else. It must NOT hand over the token.
	attacker, err := New(Config{BusURL: "http://127.0.0.1:44193", IdentityDir: dir, PersistSession: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	attacker.nowFn = func() time.Time { return now }
	if s, ok := attacker.loadPersistedSession(cred); ok {
		t.Fatalf("the token was loaded while --bus pointed at a DIFFERENT bus (token %q would have been sent there)", s.token)
	}

	// Control: the honest bus still gets a hit, or the check is useless.
	if _, ok := saver.loadPersistedSession(cred); !ok {
		t.Fatal("the honest bus got a cache MISS; the binding is too strict to be useful")
	}
}

// TestPersistedSessionRecordsTheBusActuallyDialled is the save-side half: the
// file must record the bus talked to, not the one the credential remembers.
func TestPersistedSessionRecordsTheBusActuallyDialled(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	cred := Credential{Identity: Identity{
		AgentID: "bus-test.agent-1", BusID: "bus-test", Name: "agent",
		BusURL: "https://127.0.0.1:8080",
	}}

	other := "https://127.0.0.1:9999"
	c, err := New(Config{BusURL: other, BusFingerprint: strings.Repeat("ab", 32), IdentityDir: dir, PersistSession: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.nowFn = func() time.Time { return now }
	c.savePersistedSession(cred, liveSession(now))

	body, err := os.ReadFile(filepath.Join(dir, sessionFilePrefix+cred.AgentID+".json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var doc persistedSession
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if doc.BusURL == cred.BusURL {
		t.Fatalf("bus_url recorded the CREDENTIAL's bus %q, not the one dialled %q — the binding would mislabel the file", doc.BusURL, other)
	}
	if doc.BusURL != other {
		t.Errorf("bus_url = %q, want the dialled bus %q", doc.BusURL, other)
	}
}

// TestForcedRefreshSkipsTheDiskCache proves the `force` contract: after a 401
// the caller forces a refresh, and the file holds the token that just failed.
// Reading it would hand back exactly what the bus refused.
func TestForcedRefreshSkipsTheDiskCache(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	c, cred := newPersistTestClient(t, now)
	c.savePersistedSession(cred, liveSession(now))

	// Not forced: the disk answers, no network needed.
	if _, ok := c.loadPersistedSession(cred); !ok {
		t.Fatal("precondition: the disk cache should hold a usable session")
	}

	// Forced: ensureSession must go past the file. With no reachable bus and no
	// real credential the handshake FAILS — and that failure is the proof it
	// tried the network instead of returning the cached token.
	c.session = nil
	_, err := c.ensureSession(contextForTest(), true)
	if err == nil {
		t.Fatal("a FORCED refresh returned a session without reaching the bus; it served the dead token from disk")
	}
}

// contextForTest is a plain background context; these tests never need a
// deadline because every path they take fails before any network wait.
func contextForTest() context.Context { return context.Background() }
