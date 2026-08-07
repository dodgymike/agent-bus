package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Cursor persistence: a SEPARATE file from identities.json, on purpose.
//
// A cursor and a credential have opposite write profiles, and putting them in
// one file makes the rare one pay the cost of the frequent one:
//
//   - A cursor advances on EVERY batch — potentially several times a second on
//     a busy bus, and once per poll on a quiet one. Credentials change almost
//     never: an enrolment, a `use`, a `logout`.
//   - Routing the hot loop through identities.json would therefore rewrite a
//     file of Ed25519 SEEDS hundreds of times more often than that file has any
//     reason to change. Each rewrite is another window in which a complete copy
//     of every private key exists in a temp file (save() writes 0600 with
//     O_EXCL, then renames — the copy is short-lived, but multiplying the number
//     of them by three orders of magnitude is not free).
//   - It would also put the watch loop in LOCK CONTENTION with enrolment: the
//     store lock is held across a read-modify-write, so a `busctl watch` and a
//     `busctl enrol` in another terminal would serialise against each other for
//     no reason at all.
//
// So: cursors.json, mode 0600, in the same 0700 directory, with its OWN lock
// file. The atomic-save and locking discipline below is DUPLICATED from
// store.go's save() and lock() rather than shared, because those two are
// hard-coded to the identities file names and this task does not own store.go.
// The originals are `(*Store).save` and `(*Store).lock`; keep them in step. Two
// helpers ARE shared, because they were already file-name-agnostic:
// newLockToken and removeIfToken (store.go), which is where the ownership-token
// reasoning lives.

const (
	// cursorsFileName holds one persisted read position per (agent, bus).
	cursorsFileName = "cursors.json"

	// cursorsLockFileName serialises read-modify-write cycles on it. It is a
	// DIFFERENT lock from identities.lock — see the file comment.
	cursorsLockFileName = "cursors.lock"
)

// cursorFormatVersion is the schema version written into the file.
//
// Unlike storeFormatVersion, an UNKNOWN version here is not fatal. See Cursor.
const cursorFormatVersion = 1

// maxStoredCursors bounds the file.
//
// A cursor is written with a bus-supplied value, and a client that talks to many
// buses — or a hostile bus that hands back a fresh 512-byte cursor every poll —
// must not be able to grow a local file without limit. 256 positions is far more
// than any real agent needs; the oldest-updated is evicted, so the entries that
// survive are the ones actually in use. Worst case the file is roughly
// 256 x (512-byte cursor + ids) — a couple of hundred KiB, bounded.
const maxStoredCursors = 256

// cursorRecord is one persisted read position.
//
// It is keyed on (AgentID, BusURL) — BOTH — for exactly the reason store.go
// scopes idempotency records that way: the same agent id can exist on two
// different buses, and a cursor is bound to ONE bus's sequence space. A position
// from bus A applied to bus B would either replay or, far worse, SKIP messages
// that bus never delivered. (The bus enforces the agent half itself: a cursor
// carries the id it was issued to and presenting another agent's cursor is a
// 400. Nothing enforces the bus half but this key.)
type cursorRecord struct {
	AgentID string `json:"agent_id"`
	BusURL  string `json:"bus_url"`
	Cursor  string `json:"cursor"`

	// UpdatedAt is when this record last moved, RFC3339Nano. It is used only to
	// choose an eviction victim; nothing depends on its accuracy.
	UpdatedAt string `json:"updated_at"`
}

// cursorData is the on-disk document.
type cursorData struct {
	Version int            `json:"version"`
	Cursors []cursorRecord `json:"cursors"`
}

// CursorPath returns the full path of the cursor file.
func (s *Store) CursorPath() string { return filepath.Join(s.dir, cursorsFileName) }

// Cursor returns the persisted read position for (agentID, busURL), or "" when
// there is none.
//
// # It NEVER fails because of the file's contents
//
// A missing file is an EMPTY set. An unparseable file, an unknown format
// version, a record with an implausible cursor — all of them return "" and
// record a Store warning, and none of them returns an error.
//
// This is the deliberate OPPOSITE of identities.json, which REFUSES an unknown
// version (see (*Store).load). The two differ because the stakes are opposite:
//
//   - A credential misread is unrecoverable and dangerous — a private key
//     misparsed as a public one fails silently — so refusing to guess is the
//     only safe move.
//   - A cursor is a POSITION HINT, not a credential. Losing it re-delivers
//     messages from the start of the retained window, which at-least-once
//     delivery already permits and which any correct handler already tolerates
//     (it must deduplicate on message_id regardless). Refusing to start
//     `busctl watch` because a position hint is damaged would trade a harmless
//     replay for an outage.
//
// The error return exists for a caller BUG — an empty key — not for a store
// condition.
func (s *Store) Cursor(agentID, busURL string) (string, error) {
	if agentID == "" || busURL == "" {
		return "", newError(KindInternal, "cursor store",
			"a cursor lookup needs both an agent id and a bus URL", "")
	}
	d, ok := s.loadCursors()
	if !ok {
		return "", nil
	}
	for _, rec := range d.Cursors {
		if rec.AgentID != agentID || rec.BusURL != busURL {
			continue
		}
		if len(rec.Cursor) > maxCursorLen {
			s.warnCursor("the stored cursor for %s at %s is %d bytes (the limit is %d); ignoring it and replaying from the start of the retained window",
				agentID, busURL, len(rec.Cursor), maxCursorLen)
			return "", nil
		}
		if rec.Cursor != "" && validateServerCursor("cursor store", rec.Cursor) != nil {
			s.warnCursor("the stored cursor for %s at %s is not a well-formed opaque token; ignoring it and replaying from the start of the retained window",
				agentID, busURL)
			return "", nil
		}
		return rec.Cursor, nil
	}
	return "", nil
}

// SetCursor durably records cursor as the read position for (agentID, busURL).
//
// A write failure IS returned. Unlike a read, a caller must be able to tell that
// its position is not being persisted — silently continuing means the stored
// position drifts arbitrarily far behind the real one and nobody finds out until
// a restart replays a day of messages.
func (s *Store) SetCursor(agentID, busURL, cursor string) error {
	const op = "cursor store"
	if agentID == "" || busURL == "" {
		return newError(KindInternal, op, "a cursor needs both an agent id and a bus URL", "")
	}
	if len(cursor) > maxCursorLen {
		// The value came from the bus, so this is the bus misbehaving rather
		// than the caller. Refusing it is what keeps the file bounded.
		return newError(KindServer, op,
			"refusing to store a cursor longer than the protocol allows",
			"check that --bus points at an agent-bus server; a cursor is at most "+
				strconv.Itoa(maxCursorLen)+" bytes")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return s.updateCursors(func(d *cursorData) {
		for i := range d.Cursors {
			if d.Cursors[i].AgentID == agentID && d.Cursors[i].BusURL == busURL {
				d.Cursors[i].Cursor = cursor
				d.Cursors[i].UpdatedAt = now
				return
			}
		}
		d.Cursors = append(d.Cursors, cursorRecord{
			AgentID:   agentID,
			BusURL:    busURL,
			Cursor:    cursor,
			UpdatedAt: now,
		})
		evictOldestCursors(d)
	})
}

// ClearCursor forgets the position for (agentID, busURL). It is a no-op when
// there is none, so `busctl watch --replay` need not check first.
func (s *Store) ClearCursor(agentID, busURL string) error {
	const op = "cursor store"
	if agentID == "" || busURL == "" {
		return newError(KindInternal, op, "a cursor needs both an agent id and a bus URL", "")
	}
	return s.updateCursors(func(d *cursorData) {
		out := d.Cursors[:0]
		for _, rec := range d.Cursors {
			if rec.AgentID == agentID && rec.BusURL == busURL {
				continue
			}
			out = append(out, rec)
		}
		if len(out) == 0 {
			d.Cursors = nil
			return
		}
		d.Cursors = out
	})
}

// evictOldestCursors trims the file to maxStoredCursors, dropping the
// least-recently-updated first. A record whose timestamp will not parse sorts as
// the oldest: it is malformed, so it is the best candidate to lose.
func evictOldestCursors(d *cursorData) {
	if len(d.Cursors) <= maxStoredCursors {
		return
	}
	sort.SliceStable(d.Cursors, func(i, j int) bool {
		return cursorUpdatedAt(d.Cursors[i]).Before(cursorUpdatedAt(d.Cursors[j]))
	})
	d.Cursors = append([]cursorRecord(nil), d.Cursors[len(d.Cursors)-maxStoredCursors:]...)
}

func cursorUpdatedAt(rec cursorRecord) time.Time {
	t, err := time.Parse(time.RFC3339Nano, rec.UpdatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// warnCursor records a warning about the cursor file.
//
// It is a thin alias for warnf and exists only to mark, at every call site, that
// this is a warning found LONG AFTER OpenStore returned — a damaged cursors.json
// is discovered when a watch starts. That is why Store.warnings is guarded by a
// mutex rather than treated as write-once; the locking lives in warnf, where
// both writers and Warnings() can share it.
func (s *Store) warnCursor(format string, args ...interface{}) {
	s.warnf(format, args...)
}

// loadCursors reads the cursor file. ok is false when there is nothing usable —
// which is a normal, non-error outcome. See Cursor.
func (s *Store) loadCursors() (cursorData, bool) {
	b, err := os.ReadFile(s.CursorPath())
	if errors.Is(err, fs.ErrNotExist) {
		return cursorData{Version: cursorFormatVersion}, true
	}
	if err != nil {
		s.warnCursor("cannot read the cursor file %s (%v); starting from the beginning of the retained window", s.CursorPath(), err)
		return cursorData{}, false
	}
	var d cursorData
	if err := json.Unmarshal(b, &d); err != nil {
		s.warnCursor("the cursor file %s is not valid JSON; ignoring it and starting from the beginning of the retained window", s.CursorPath())
		return cursorData{}, false
	}
	if d.Version == 0 {
		d.Version = cursorFormatVersion
	}
	if d.Version != cursorFormatVersion {
		s.warnCursor("the cursor file %s is format version %d and this build understands version %d; ignoring it and starting from the beginning of the retained window",
			s.CursorPath(), d.Version, cursorFormatVersion)
		return cursorData{}, false
	}
	return d, true
}

// updateCursors runs mutate against the cursor file under the cursor lock and
// saves the result.
//
// A file that failed to load is treated as EMPTY here rather than refused: the
// mutation is about to rewrite it wholesale, and refusing to write because the
// previous contents were damaged would wedge a watch permanently on a file
// nothing else will ever repair.
func (s *Store) updateCursors(mutate func(d *cursorData)) error {
	release, err := s.lockCursors()
	if err != nil {
		return err
	}
	defer release()

	s.sweepCursorTempFiles()

	d, ok := s.loadCursors()
	if !ok {
		d = cursorData{Version: cursorFormatVersion}
	}
	mutate(&d)
	return s.saveCursors(d)
}

// saveCursors writes the cursor file atomically: a fresh 0600 temp file in the
// same directory, fsynced, then renamed over the target, then the directory
// itself fsynced so the rename survives a crash.
//
// DUPLICATED from (*Store).save — see the file comment for why. The one
// behavioural difference is the file name.
func (s *Store) saveCursors(d cursorData) error {
	const op = "cursor store"
	d.Version = cursorFormatVersion
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return wrapError(KindInternal, op, "cannot encode the cursor file", "", err)
	}
	body = append(body, '\n')

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return wrapError(KindInternal, op, "cannot generate a temporary file name", "", err)
	}
	tmp := filepath.Join(s.dir, cursorsFileName+".tmp-"+hex.EncodeToString(suffix[:]))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
	if err != nil {
		return wrapError(KindConfig, op, "cannot create a temporary file in "+s.dir, "check the directory is writable", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }

	if _, err := f.Write(body); err != nil {
		cleanup()
		return wrapError(KindConfig, op, "cannot write the cursor file", "check for a full or read-only filesystem", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return wrapError(KindConfig, op, "cannot flush the cursor file to disk", "check for a full or read-only filesystem", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, op, "cannot close the cursor file", "check for a full or read-only filesystem", err)
	}
	if err := os.Rename(tmp, s.CursorPath()); err != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, op, "cannot replace the cursor file "+s.CursorPath(), "check the directory is writable", err)
	}
	// Fsync the DIRECTORY so the rename itself is durable; without it the
	// contents are on disk but the name may not be. Not fatal if the platform
	// refuses it.
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// lockCursors takes the cursor file's exclusive lock and returns a release
// function.
//
// DUPLICATED from (*Store).lock — see the file comment. The ownership-token
// discipline is the same and is the load-bearing part: every lock carries a
// token, and both the stale break and the release are conditional on it, so two
// processes waiting on one abandoned lock cannot both conclude they hold it.
// The full reasoning is on (*Store).lock; do not re-derive it here, and do not
// weaken it here without weakening it there.
func (s *Store) lockCursors() (func(), error) {
	const op = "cursor store"
	path := filepath.Join(s.dir, cursorsLockFileName)
	deadline := time.Now().Add(lockAcquireTimeout)
	for {
		token, err := newLockToken()
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
		if err == nil {
			_, werr := f.WriteString(token)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				// A lock we cannot prove we own is worse than no lock: the
				// release below would refuse to remove it and it would go stale.
				_ = os.Remove(path)
				cause := werr
				if cause == nil {
					cause = cerr
				}
				return nil, wrapError(KindConfig, op,
					"cannot write the cursor lock "+path,
					"check for a full or read-only filesystem",
					cause)
			}
			return func() { removeIfToken(path, token) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, wrapError(KindConfig, op,
				"cannot create the cursor lock "+path,
				"check the directory is writable",
				err)
		}

		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			if held, rerr := os.ReadFile(path); rerr == nil {
				removeIfToken(path, string(held))
			}
			// Fall through to the deadline check and the sleep rather than
			// spinning: an unconditional continue here would be an unbounded hot
			// loop if the remove failed.
		}

		if time.Now().After(deadline) {
			return nil, newError(KindConfig, op,
				"timed out waiting for the cursor lock "+path,
				"another busctl process is updating the cursor file; retry, or remove the lock file if no other process is running")
		}
		time.Sleep(lockPollInterval)
	}
}

// sweepCursorTempFiles removes leftovers from a save that was killed between
// creating the temp file and renaming it. Called only with the lock held.
func (s *Store) sweepCursorTempFiles() {
	matches, err := filepath.Glob(filepath.Join(s.dir, cursorsFileName+".tmp-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
