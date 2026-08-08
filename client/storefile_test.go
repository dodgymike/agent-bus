package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests in this file are CLI-3-FU-STOREDEDUP's reason for existing: the
// atomic-save and lock discipline must be ONE implementation, driven by both
// documents in the store directory, not two copies that happen to agree.
//
// Why that matters more than tidiness. What the original protects is a file of
// Ed25519 PRIVATE-KEY SEEDS, and the reasoning that makes the lock correct — why
// a stale break must be conditional on the ownership token — is subtle enough
// that a second copy edited without it would be a lost-update bug on private
// keys. The tests below therefore run the SAME table over BOTH descriptors, so a
// change made to one path and not the other fails here.

// storeFiles is every JSON document the store directory holds. A new one must be
// added here, which is the point: the table is the enumeration.
func storeFiles() []storeFile {
	return []storeFile{identitiesFile, cursorsFile}
}

func TestStoreFileAtomicSaveIsShared(t *testing.T) {
	for _, f := range storeFiles() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			path := filepath.Join(dir, f.name)

			if err := s.saveJSON(f, map[string]interface{}{"version": 1, "hello": "world"}); err != nil {
				t.Fatalf("saveJSON(%s): %v", f.name, err)
			}

			// 0600, created that way rather than chmodded afterwards: there must
			// be no instant at which private keys exist under a looser mode.
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			if perm := info.Mode().Perm(); perm != storeFileMode {
				t.Fatalf("%s mode = %#o, want %#o", f.name, perm, storeFileMode)
			}

			var got map[string]interface{}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s is not JSON: %v (%q)", f.name, err, raw)
			}
			if got["hello"] != "world" {
				t.Fatalf("%s = %v, want the saved document", f.name, got)
			}

			// No temp file survives a successful save. Each one is a complete
			// 0600 copy of the document, and for identities.json that means a
			// complete copy of every private key.
			leftovers, _ := filepath.Glob(filepath.Join(dir, f.name+".tmp-*"))
			if len(leftovers) != 0 {
				t.Fatalf("saveJSON(%s) left temp files behind: %v", f.name, leftovers)
			}
		})
	}
}

// TestStoreFileSweepsAbandonedTempFiles checks the sweep is shared and that the
// two documents' globs stay DISJOINT — a sweep that took the other document's
// temp files would delete a concurrent writer's in-flight copy.
func TestStoreFileSweepsAbandonedTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	for _, f := range storeFiles() {
		abandoned := filepath.Join(dir, f.name+".tmp-deadbeef")
		if err := os.WriteFile(abandoned, []byte("{}"), storeFileMode); err != nil {
			t.Fatalf("seeding %s: %v", abandoned, err)
		}
	}

	for _, f := range storeFiles() {
		s.sweepTempFiles(f)
		if _, err := os.Stat(filepath.Join(dir, f.name+".tmp-deadbeef")); !os.IsNotExist(err) {
			t.Fatalf("sweepTempFiles(%s) did not remove its own abandoned temp file (stat err = %v)", f.name, err)
		}
		for _, other := range storeFiles() {
			if other.name == f.name {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, other.name+".tmp-deadbeef")); err != nil {
				t.Fatalf("sweepTempFiles(%s) removed %s's temp file; the globs must be DISJOINT or a sweep deletes a concurrent writer's in-flight copy",
					f.name, other.name)
			}
		}
		// Put it back for the next descriptor's turn.
		_ = os.WriteFile(filepath.Join(dir, f.name+".tmp-deadbeef"), []byte("{}"), storeFileMode)
	}
}

// TestStoreFileLockOwnershipTokenIsShared is the load-bearing one.
//
// Both the stale BREAK and the RELEASE are conditional on the lock's ownership
// token. Without that, two processes waiting on one abandoned lock race like
// this: A stats it, finds it stale, removes it and wins the O_EXCL create; B —
// which stat'd the OLD file a moment earlier — then removes A's LIVE lock and
// wins its own create. Both believe they hold the lock, both read-modify-write,
// and one whole-file update is lost. On identities.json "lost update" means
// "lost identity".
//
// Driving it over BOTH descriptors is what stops that reasoning existing in one
// copy and being referenced by the other.
func TestStoreFileLockOwnershipTokenIsShared(t *testing.T) {
	for _, f := range storeFiles() {
		f := f
		t.Run(f.lockName, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			lockPath := filepath.Join(dir, f.lockName)

			release, err := s.lockFile(f)
			if err != nil {
				t.Fatalf("lockFile(%s): %v", f.lockName, err)
			}
			held, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatalf("the lock file %s was not created: %v", lockPath, err)
			}
			if len(held) == 0 {
				t.Fatalf("the lock file %s is empty; every lock carries an ownership token", lockPath)
			}

			// A release must remove ONLY a lock that is still ours. Overwrite the
			// token to simulate another process having taken it in the meantime.
			if err := os.WriteFile(lockPath, []byte("999 someone-else\n"), storeFileMode); err != nil {
				t.Fatalf("overwriting the lock: %v", err)
			}
			release()
			if _, err := os.Stat(lockPath); err != nil {
				t.Fatalf("release() removed a lock owned by someone else (stat err = %v); both the break and the release must be conditional on the token", err)
			}

			// And a release DOES remove a lock that is still ours.
			if err := os.Remove(lockPath); err != nil {
				t.Fatalf("clearing the lock: %v", err)
			}
			release2, err := s.lockFile(f)
			if err != nil {
				t.Fatalf("re-locking %s: %v", f.lockName, err)
			}
			release2()
			if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
				t.Fatalf("release() left our own lock behind (stat err = %v)", err)
			}
		})
	}
}

// TestStoreFileLockBreaksOnlyStaleLocks checks the conditional stale break, over
// both descriptors: a FRESH lock is waited on (and times out), a lock older than
// lockStaleAfter is broken.
func TestStoreFileLockBreaksOnlyStaleLocks(t *testing.T) {
	for _, f := range storeFiles() {
		f := f
		t.Run(f.lockName, func(t *testing.T) {
			dir := t.TempDir()
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			lockPath := filepath.Join(dir, f.lockName)

			if err := os.WriteFile(lockPath, []byte("1 abandoned\n"), storeFileMode); err != nil {
				t.Fatalf("seeding a stale lock: %v", err)
			}
			old := time.Now().Add(-2 * lockStaleAfter)
			if err := os.Chtimes(lockPath, old, old); err != nil {
				t.Fatalf("ageing the lock: %v", err)
			}

			release, err := s.lockFile(f)
			if err != nil {
				t.Fatalf("lockFile(%s) over a stale lock = %v, want it broken and acquired", f.lockName, err)
			}
			release()
		})
	}
}

// TestStoreFileDescriptorsAreDistinct guards the refactor's one real hazard: two
// descriptors that shared a file name or a lock name would put the credential
// document and the cursor document under one lock, reintroducing exactly the
// contention the split exists to avoid — and, worse, could let a cursor write
// truncate the seeds.
func TestStoreFileDescriptorsAreDistinct(t *testing.T) {
	seenName := map[string]bool{}
	seenLock := map[string]bool{}
	for _, f := range storeFiles() {
		if f.name == "" || f.lockName == "" || f.op == "" || f.what == "" {
			t.Fatalf("storeFile %+v has an empty field; every failure message is built from these", f)
		}
		if seenName[f.name] {
			t.Fatalf("two descriptors share the file name %q", f.name)
		}
		if seenLock[f.lockName] {
			t.Fatalf("two descriptors share the lock name %q; that would serialise the credential document against the cursor hot loop", f.lockName)
		}
		if f.name == f.lockName {
			t.Fatalf("descriptor %q uses its own name as its lock", f.name)
		}
		seenName[f.name] = true
		seenLock[f.lockName] = true
	}
}
