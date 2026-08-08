package client

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStoreConcurrentMutationsLoseNothing hammers ClaimEnrolment, PromotePending
// and SetCurrent from many goroutines under -race and checks every mutation
// landed: no lost update from the read-modify-write cycle, and no leftover
// ".tmp-" file from the atomic replace.
func TestStoreConcurrentMutationsLoseNothing(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	const n = 25
	now := time.Now()
	seed := func(i int) []byte {
		b := make([]byte, ed25519.SeedSize)
		for j := range b {
			b[j] = byte(i)
		}
		return b
	}
	pub := func(i int) []byte {
		b := make([]byte, ed25519.PublicKeySize)
		for j := range b {
			b[j] = byte(i + 1)
		}
		return b
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := pendingEnrolment{
				IdempotencyKey: fmt.Sprintf("key-%d", i),
				Name:           fmt.Sprintf("agent%d", i),
				BusURL:         "http://bus.example",
				PublicKey:      base64.StdEncoding.EncodeToString(pub(i)),
				PrivateKeySeed: base64.StdEncoding.EncodeToString(seed(i)),
				CreatedAt:      now.UTC().Format(time.RFC3339Nano),
			}
			if _, err := s.ClaimEnrolment(p, now); err != nil {
				t.Errorf("ClaimEnrolment(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if _, found, err := s.FindPending(fmt.Sprintf("key-%d", i), "http://bus.example"); err != nil || !found {
			t.Errorf("pending record %d missing after concurrent ClaimEnrolment: found=%v err=%v", i, found, err)
		}
	}

	wg = sync.WaitGroup{}
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i)
			cred := Credential{
				Identity: Identity{
					AgentID:    fmt.Sprintf("bus-x.agent%d-1", i),
					BusID:      "bus-x",
					Name:       fmt.Sprintf("agent%d", i),
					BusURL:     "http://bus.example",
					PublicKey:  base64.StdEncoding.EncodeToString(pub(i)),
					EnrolledAt: now.UTC().Format(time.RFC3339Nano),
				},
				PrivateKeySeed: base64.StdEncoding.EncodeToString(seed(i)),
				IdempotencyKey: key,
			}
			if err := s.PromotePending(key, cred, false); err != nil {
				t.Errorf("PromotePending(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	ids, _, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != n {
		t.Fatalf("List() returned %d identities, want %d — a concurrent PromotePending lost an update", len(ids), n)
	}

	wg = sync.WaitGroup{}
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SetCurrent(fmt.Sprintf("bus-x.agent%d-1", i)); err != nil {
				t.Errorf("SetCurrent(%d): %v", i, err)
			}
		}()
	}
	wg.Wait()

	_, current, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, id := range ids {
		if id.AgentID == current {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("final selection %q after concurrent SetCurrent is not one of the stored identities", current)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %q in store dir after concurrent saves", e.Name())
		}
	}
}

// TestStoreCorruptFileYieldsKindConfig checks a garbage store file is reported
// as KindConfig with a remedy, rather than a bare unclassified error or a
// panic.
func TestStoreCorruptFileYieldsKindConfig(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte("this is not json{{{"), storeFileMode); err != nil {
		t.Fatalf("writing garbage store file: %v", err)
	}

	_, _, err = s.List()
	if err == nil {
		t.Fatalf("List() on a corrupt store = nil error, want one")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not a *client.Error: %v", err)
	}
	if e.Kind != KindConfig {
		t.Fatalf("Kind = %q, want %q", e.Kind, KindConfig)
	}
	if e.Remedy == "" {
		t.Fatalf("want a non-empty remedy for a corrupt store")
	}
}

// TestStoreUnknownVersionYieldsKindConfig checks a store file written by a
// FUTURE format version is refused rather than guessed at — a private key
// misparsed as something else is exactly the silent failure invariant 1 and
// invariant 3 exist to prevent.
func TestStoreUnknownVersionYieldsKindConfig(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	doc := storeData{Version: storeFormatVersion + 1}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(s.Path(), body, storeFileMode); err != nil {
		t.Fatalf("writing store file: %v", err)
	}

	_, _, err = s.List()
	if err == nil {
		t.Fatalf("List() on an unknown store version = nil error, want one")
	}
	if KindOf(err) != KindConfig {
		t.Fatalf("KindOf(err) = %q, want %q", KindOf(err), KindConfig)
	}
}

// TestCredentialStringRedactsSeed pins the safety net: an accidental %v/%s of
// a Credential must never print the private key.
func TestCredentialStringRedactsSeed(t *testing.T) {
	c := Credential{
		Identity:       Identity{AgentID: "bus-x.agent-1", BusID: "bus-x", Name: "agent", BusURL: "http://bus.example"},
		PrivateKeySeed: "dGhpcy1pcy1hLXNlY3JldC1zZWVkLXZhbHVlLTEyMzQ1",
	}
	s := c.String()
	if strings.Contains(s, c.PrivateKeySeed) {
		t.Fatalf("Credential.String() leaks the private key seed: %q", s)
	}
	if strings.Contains(s, "bus-x.agent-1") {
		// AgentID is public and SHOULD appear; this assertion is here only to
		// prove the check above isn't vacuous by matching everything.
	} else {
		t.Fatalf("Credential.String() = %q, want it to still name the agent id", s)
	}
}

// TestPendingEnrolmentRedactsItsSeed is pendingEnrolment's half of
// TestCredentialStringRedactsSeed. pendingEnrolment carries the SAME class of
// secret (a raw Ed25519 seed) as Credential, and until this test existed it
// had no String() at all — so the default struct formatting would print every
// field, seed included, into the first %v or log line anyone added. No code
// path does that today (85da3164), which is exactly why the redaction has to
// be pinned before the leak, not after.
//
// It covers BOTH seeds since RELAY-13 put the messaging seed in this record.
// The redaction is enumerated rather than derived, so a second secret field is
// exactly the case that silently regresses it.
func TestPendingEnrolmentRedactsItsSeed(t *testing.T) {
	p := pendingEnrolment{
		IdempotencyKey:   "resume-key-1",
		Name:             "agent",
		BusURL:           "http://bus.example",
		PublicKey:        "cHVibGljLWtleS1iYXNlNjQ=",
		PrivateKeySeed:   "dGhpcy1pcy1hLXNlY3JldC1zZWVkLXZhbHVlLTEyMzQ1",
		MessagingKeySeed: "bWVzc2FnaW5nLXNlY3JldC1zZWVkLXZhbHVlLTk4NzY1",
		CreatedAt:        "2026-08-08T00:00:00Z",
	}
	s := p.String()
	if strings.Contains(s, p.PrivateKeySeed) {
		t.Fatalf("pendingEnrolment.String() leaks the private key seed: %q", s)
	}
	if strings.Contains(s, p.MessagingKeySeed) {
		t.Fatalf("pendingEnrolment.String() leaks the MESSAGING key seed: %q", s)
	}
	// Also check the %v path directly — fmt.Stringer is picked up automatically
	// by %v/%s, and a caller reaching for %v is exactly the accident this
	// exists to survive.
	if v := fmt.Sprintf("%v", p); strings.Contains(v, p.PrivateKeySeed) {
		t.Fatalf("fmt.Sprintf(%%v, pendingEnrolment) leaks the private key seed: %q", v)
	}
	if v := fmt.Sprintf("%v", p); strings.Contains(v, p.MessagingKeySeed) {
		t.Fatalf("fmt.Sprintf(%%v, pendingEnrolment) leaks the MESSAGING key seed: %q", v)
	}
	if !strings.Contains(s, "resume-key-1") {
		// The identity fields are not secret and SHOULD appear; this proves the
		// check above isn't vacuous by matching everything.
		t.Fatalf("pendingEnrolment.String() = %q, want it to still name the idempotency key", s)
	}
}

// TestLockReleaseOnlyRemovesOwnToken checks the ownership-token protocol
// described on Store.lock: a release must only remove the lock file if its
// contents still match the token that release closed over. Without that
// check, a release from a holder whose lock had already been broken as stale
// and re-acquired by someone else would delete the NEW holder's live lock —
// the exact lost-update race the token exists to prevent.
func TestLockReleaseOnlyRemovesOwnToken(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	release, err := s.lock()
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	lockPath := filepath.Join(dir, lockFileName)
	// Simulate another process having broken this (now considered stale) lock
	// and taken its own: overwrite the lock file with a DIFFERENT token.
	otherToken, err := newLockToken()
	if err != nil {
		t.Fatalf("newLockToken: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(otherToken), storeFileMode); err != nil {
		t.Fatalf("simulating a re-taken lock: %v", err)
	}

	// The ORIGINAL release must not remove the new holder's lock.
	release()

	held, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("lock file was removed by a release that did not own it: %v", err)
	}
	if string(held) != otherToken {
		t.Fatalf("lock file contents changed after a non-owning release: got %q, want the other holder's token %q", held, otherToken)
	}
}

// TestLockBreaksOnlyGenuinelyStaleLock checks the other half: lock() DOES
// break and take over a lock file that is genuinely abandoned (older than
// lockStaleAfter), and the freshly acquired token differs from the old one.
func TestLockBreaksOnlyGenuinelyStaleLock(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	lockPath := filepath.Join(dir, lockFileName)
	staleToken := "999999 deadbeefdeadbeefdeadbeefdead\n"
	if err := os.WriteFile(lockPath, []byte(staleToken), storeFileMode); err != nil {
		t.Fatalf("writing a fake abandoned lock: %v", err)
	}
	staleTime := time.Now().Add(-(lockStaleAfter + time.Second))
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("backdating the lock file's mtime: %v", err)
	}

	done := make(chan struct{})
	var release func()
	var lockErr error
	go func() {
		release, lockErr = s.lock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lockAcquireTimeout):
		t.Fatalf("lock() did not return within the acquire timeout; a genuinely stale lock should be broken quickly")
	}
	if lockErr != nil {
		t.Fatalf("lock() on a stale lock file: %v", lockErr)
	}
	defer release()

	held, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading the newly acquired lock: %v", err)
	}
	if string(held) == staleToken {
		t.Fatalf("the lock file still holds the OLD stale token; it was not actually replaced")
	}
}

// TestRemoveAllClearsPending checks RemoveAll — the guts of `logout --all` —
// clears in-flight Pending records, not just applied Credentials. A pending
// record holds a private key seed, and `logout --all` promises the keys are
// destroyed; leaving pending records behind would be a live private key that
// survives a wipe the operator believes was complete.
func TestRemoveAllClearsPending(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	now := time.Now()
	p := pendingEnrolment{
		IdempotencyKey: "pending-key-1",
		Name:           "a",
		BusURL:         "http://bus.example",
		PublicKey:      "cHVi",
		PrivateKeySeed: "c2VlZA==",
		CreatedAt:      now.UTC().Format(time.RFC3339Nano),
	}
	if _, err := s.ClaimEnrolment(p, now); err != nil {
		t.Fatalf("ClaimEnrolment: %v", err)
	}
	if pending, err := s.ListPending(); err != nil || len(pending) != 1 {
		t.Fatalf("setup: ListPending() = %v, %v; want exactly 1 pending record before RemoveAll", pending, err)
	}

	if _, err := s.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	pending, err := s.ListPending()
	if err != nil {
		t.Fatalf("ListPending after RemoveAll: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("ListPending() after RemoveAll = %v, want empty — RemoveAll must clear Pending too", pending)
	}
}

// TestRemoveDropsMatchingPending checks Store.Remove drops the pending record
// that matches the removed credential's (idempotency key, bus URL), not just
// the credential itself.
func TestRemoveDropsMatchingPending(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	const key = "remove-pending-key"
	const busURL = "http://bus.example"

	// Construct a store document directly with a credential AND a pending
	// record that share (key, busURL) — the state Remove is documented to
	// clean up together.
	doc := storeData{
		Version: storeFormatVersion,
		Current: "bus-x.agent-1",
		Credentials: []Credential{{
			Identity: Identity{
				AgentID: "bus-x.agent-1", BusID: "bus-x", Name: "agent", BusURL: busURL,
				PublicKey: "cHVi", EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
			PrivateKeySeed: "c2VlZA==",
			IdempotencyKey: key,
		}},
		Pending: []pendingEnrolment{{
			IdempotencyKey: key,
			Name:           "agent",
			BusURL:         busURL,
			PublicKey:      "cHVi",
			PrivateKeySeed: "c2VlZA==",
			CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		}},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(s.Path(), body, storeFileMode); err != nil {
		t.Fatalf("writing fixture store: %v", err)
	}

	removed, _, err := s.Remove("bus-x.agent-1")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(removed) != 1 || removed[0] != "bus-x.agent-1" {
		t.Fatalf("Remove removed = %v, want [bus-x.agent-1]", removed)
	}

	if _, found, err := s.FindPending(key, busURL); err != nil {
		t.Fatalf("FindPending: %v", err)
	} else if found {
		t.Fatalf("FindPending(%q, %q) still found a record after Remove — the matching pending record must be dropped with the credential", key, busURL)
	}
}

// TestUpdateSweepsAbandonedTempFiles checks that a leftover
// "identities.json.tmp-XXXX" file — the signature of a save() killed between
// creating the temp file and renaming it over the real store, and a COMPLETE
// copy of every private key in the store — is removed by the NEXT mutation.
func TestUpdateSweepsAbandonedTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	abandoned := filepath.Join(dir, storeFileName+".tmp-deadbeef")
	if err := os.WriteFile(abandoned, []byte("{}"), storeFileMode); err != nil {
		t.Fatalf("writing an abandoned temp file: %v", err)
	}

	// Any mutation sweeps temp files, per Store.update's doc comment. Use a
	// no-op DropPending so this test exercises exactly that path.
	if err := s.DropPending("does-not-exist", "http://bus.example"); err != nil {
		t.Fatalf("DropPending: %v", err)
	}

	if _, err := os.Stat(abandoned); err == nil {
		t.Fatalf("abandoned temp file %q still exists after a mutation; it should have been swept", abandoned)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %q: unexpected error: %v", abandoned, err)
	}
}

// TestUpdatePrunesExpiredPending checks a Pending record whose CreatedAt is
// older than pendingTTL (24h) is dropped by the NEXT mutation, so the
// documented TTL is real for any store that is written to at all — not only
// for one that happens to enrol again.
func TestUpdatePrunesExpiredPending(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	expired := time.Now().Add(-(pendingTTL + time.Hour))
	doc := storeData{
		Version: storeFormatVersion,
		Pending: []pendingEnrolment{{
			IdempotencyKey: "old-key",
			Name:           "agent",
			BusURL:         "http://bus.example",
			PublicKey:      "cHVi",
			PrivateKeySeed: "c2VlZA==",
			CreatedAt:      expired.UTC().Format(time.RFC3339Nano),
		}},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(s.Path(), body, storeFileMode); err != nil {
		t.Fatalf("writing fixture store: %v", err)
	}

	if err := s.DropPending("does-not-exist", "http://bus.example"); err != nil {
		t.Fatalf("DropPending (triggers the housekeeping sweep): %v", err)
	}

	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("reading store after mutation: %v", err)
	}
	var reloaded storeData
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal reloaded store: %v", err)
	}
	if len(reloaded.Pending) != 0 {
		t.Fatalf("Pending on disk after a mutation = %v, want empty — the expired record should have been pruned", reloaded.Pending)
	}
}

// TestOpenStoreTightensLooseCredentialFileMode checks OpenStore tightens an
// existing 0644 identities.json to 0600 AND records a non-empty warning — the
// warning is the point, not just the chmod, because a file that was EVER
// readable by another local user must be treated as compromised rather than
// silently repaired.
func TestOpenStoreTightensLooseCredentialFileMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, storeDirMode); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	loose := storeData{Version: storeFormatVersion}
	body, err := json.Marshal(loose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	storePath := filepath.Join(dir, storeFileName)
	if err := os.WriteFile(storePath, body, 0o644); err != nil {
		t.Fatalf("writing a loosely-permissioned store file: %v", err)
	}

	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != storeFileMode {
		t.Fatalf("credential file mode after OpenStore = %#o, want %#o", perm, storeFileMode)
	}
	if warnings := s.Warnings(); len(warnings) == 0 {
		t.Fatalf("Warnings() is empty after tightening a loose credential file mode, want at least one warning")
	}
}

// TestListPendingJSONHasNoPrivateKeyMaterial checks the exported
// PendingEnrolment type — surfaced by `whoami --all` — never carries the
// private key seed under any field, by asserting the marshalled JSON does not
// contain the seed string anywhere.
func TestListPendingJSONHasNoPrivateKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	const secretSeed = "dGhpcy1pcy1hLXNlY3JldC1zZWVkLXZhbHVlLTEyMzQ1"
	p := pendingEnrolment{
		IdempotencyKey: "list-pending-key",
		Name:           "agent",
		BusURL:         "http://bus.example",
		PublicKey:      "cHVi",
		PrivateKeySeed: secretSeed,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := s.ClaimEnrolment(p, time.Now()); err != nil {
		t.Fatalf("ClaimEnrolment: %v", err)
	}

	pending, err := s.ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("ListPending() = %d records, want 1", len(pending))
	}
	body, err := json.Marshal(pending)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte(secretSeed)) {
		t.Fatalf("ListPending JSON contains the private key seed: %s", body)
	}
	if bytes.Contains(body, []byte("private_key")) {
		t.Fatalf("ListPending JSON mentions a private key field at all: %s", body)
	}
}

// TestIdentityJSONHasNoPrivateKeyField checks the PUBLIC struct never carries
// a private-key field under any json tag, so no rendering path — --json
// output, a log line, a debugger dump serialized to JSON — can leak one by
// forgetting a redaction step.
func TestIdentityJSONHasNoPrivateKeyField(t *testing.T) {
	id := Identity{
		AgentID:    "bus-x.agent-1",
		BusID:      "bus-x",
		Name:       "agent",
		BusURL:     "http://bus.example",
		PublicKey:  "cHVibGljLWtleQ==",
		EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) == 0 {
		t.Fatalf("Identity marshalled to no fields at all: %s", body)
	}
	if _, ok := fields["private_key_seed"]; ok {
		t.Fatalf("Identity JSON has a private_key_seed field: %s", body)
	}
	if bytes.Contains(body, []byte("private_key")) {
		t.Fatalf("Identity JSON mentions a private key field at all: %s", body)
	}
}

// TestPendingEnrolmentMessagingSeedSurvivesDisk checks the messaging seed a
// pending enrolment carries is DURABLE — written to the file and read back by a
// separate Store, the way a resumed `enrol --idempotency-key` in a NEW process
// reads it.
//
// It is not a JSON-tag tautology. The whole point of putting the messaging seed
// in the pending record (RELAY-13) is that the process which resumes the
// enrolment is usually not the process that started it: the first one was
// killed. A field that round-tripped only in memory would leave that second
// process regenerating the messaging key, presenting the original idempotency
// key with different content, and being answered with a 409 — a correct retry
// turned into a permanent failure.
//
// It also pins that the seed does NOT escape through ListPending, which is the
// public, printable view of the same record.
func TestPendingEnrolmentMessagingSeedSurvivesDisk(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	const (
		key    = "durable-msgseed-key"
		busURL = "http://bus.example"
	)
	want := pendingEnrolment{
		IdempotencyKey:   key,
		Name:             "agent",
		BusURL:           busURL,
		PublicKey:        base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)),
		PrivateKeySeed:   base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, ed25519.SeedSize)),
		MessagingKeySeed: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, ed25519.SeedSize)),
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := s.ClaimEnrolment(want, time.Now()); err != nil {
		t.Fatalf("ClaimEnrolment: %v", err)
	}

	// A SECOND Store over the same directory — no shared memory with the first,
	// so this reads the file rather than a cached struct.
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore (second): %v", err)
	}
	got, found, err := reopened.FindPending(key, busURL)
	if err != nil {
		t.Fatalf("FindPending: %v", err)
	}
	if !found {
		t.Fatalf("no pending record found after reopening the store")
	}
	if got.MessagingKeySeed != want.MessagingKeySeed {
		t.Fatalf("pending MessagingKeySeed after a reopen = %q, want %q — a resumed enrolment in a new process would mint a DIFFERENT messaging key and earn a 409",
			got.MessagingKeySeed, want.MessagingKeySeed)
	}
	if got.PrivateKeySeed != want.PrivateKeySeed {
		t.Fatalf("pending PrivateKeySeed after a reopen = %q, want %q", got.PrivateKeySeed, want.PrivateKeySeed)
	}

	// The PUBLIC view must not carry either secret.
	listed, err := reopened.ListPending()
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("ListPending() returned %d records, want 1", len(listed))
	}
	rendered := fmt.Sprintf("%+v", listed[0])
	if strings.Contains(rendered, want.MessagingKeySeed) {
		t.Fatalf("ListPending() leaks the messaging key seed: %q", rendered)
	}
	if strings.Contains(rendered, want.PrivateKeySeed) {
		t.Fatalf("ListPending() leaks the private key seed: %q", rendered)
	}
}

// TestPendingEnrolmentWithoutMessagingSeedLoads checks a pending record written
// by a build that predates the messaging seed still loads, with an EMPTY seed
// rather than an error.
//
// The empty value is MEANINGFUL, not merely absent: Enrol reads it as "that
// attempt registered no messaging key, so this retry must register none
// either". A record that failed to load, or that was repaired by minting a
// seed here, would break the retry it exists to make possible.
func TestPendingEnrolmentWithoutMessagingSeedLoads(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "version": 1,
  "current": "",
  "identities": [],
  "pending": [
    {
      "idempotency_key": "legacy-key",
      "name": "agent",
      "bus_url": "http://bus.example",
      "public_key": "cHVibGljLWtleS1iYXNlNjQ=",
      "private_key_seed": "c2VlZA==",
      "created_at": "` + time.Now().UTC().Format(time.RFC3339Nano) + `"
    }
  ]
}
`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(legacy), storeFileMode); err != nil {
		t.Fatalf("writing a legacy store: %v", err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	got, found, err := s.FindPending("legacy-key", "http://bus.example")
	if err != nil {
		t.Fatalf("FindPending: %v", err)
	}
	if !found {
		t.Fatalf("a pending record written before messaging_key_seed existed did not load")
	}
	if got.MessagingKeySeed != "" {
		t.Fatalf("MessagingKeySeed = %q, want empty — nothing may invent key material for an attempt that registered none", got.MessagingKeySeed)
	}
}

// TestDamagedMessagingSeedRemedyDoesNotAdviseLocalReminting pins the ADVICE in
// damagedMessagingSeedRemedy, which nothing else does.
//
// It exists because that string was already wrong once. It told the operator to
// delete the messaging_key_seed field and mint a replacement locally — correct
// while the messaging key never left the machine, and actively harmful once
// RELAY-13 made the bus record the public half at enrolment. The field is
// write-once server side, so a locally minted replacement leaves the agent
// signing with a key its own bus still attests the predecessor of: every peer
// that verifies through an attestation rejects every message, and no test went
// red. Prose that only a human re-reading it can falsify is prose that decays.
//
// It asserts the SHAPE of the advice — no local re-mint, re-enrolment named —
// rather than the exact sentence, so wording can improve without a test edit
// while the substance cannot silently revert.
func TestDamagedMessagingSeedRemedyDoesNotAdviseLocalReminting(t *testing.T) {
	remedy := damagedMessagingSeedRemedy

	// The specific reverted-to advice, and the command that would carry it out.
	for _, forbidden := range []string{"keygen", "re-distribute"} {
		if strings.Contains(remedy, forbidden) {
			t.Fatalf("the damaged-messaging-seed remedy mentions %q: %q\n"+
				"minting a replacement key locally leaves the agent signing with a key the bus does not attest, because the bus recorded the key at enrolment and the field is write-once",
				forbidden, remedy)
		}
	}
	if !strings.Contains(remedy, "enrol") {
		t.Fatalf("the damaged-messaging-seed remedy = %q, want it to name re-enrolment, which is the only route that puts the bus and the agent back in agreement", remedy)
	}

	// And it must actually be the remedy BOTH damaged-seed branches carry, which
	// is the drift the constant exists to prevent.
	for _, tc := range []struct {
		name string
		cred Credential
	}{
		{"not base64", Credential{MessagingKeySeed: "!!! not base64 !!!"}},
		{"wrong length", Credential{MessagingKeySeed: base64.StdEncoding.EncodeToString([]byte("short"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cred.MessagingPrivateKey()
			if err == nil {
				t.Fatalf("MessagingPrivateKey() = nil error, want one")
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("error is not a *client.Error: %v", err)
			}
			if e.Remedy != remedy {
				t.Fatalf("Remedy = %q, want the shared damagedMessagingSeedRemedy %q", e.Remedy, remedy)
			}
		})
	}
}
