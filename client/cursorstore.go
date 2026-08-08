package client

import (
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
//     of every private key exists in a temp file (saveJSON creates it 0600 with
//     an exclusive create, then renames — the copy is short-lived, but
//     multiplying the number of them by three orders of magnitude is not free).
//   - It would also put the watch loop in LOCK CONTENTION with enrolment: the
//     store lock is held across a read-modify-write, so a `agent-busctl watch` and a
//     `agent-busctl enrol` in another terminal would serialise against each other for
//     no reason at all.
//
// So: cursors.json, mode 0600, in the same 0700 directory, with its OWN lock
// file.
//
// The atomic-save and locking discipline is store.go's — `(*Store).saveJSON`,
// `(*Store).lockFile` and `(*Store).sweepTempFiles`, selected by the cursorsFile
// descriptor below. It was DUPLICATED here until CLI-3-FU-STOREDEDUP, only
// because store.go was outside the implementing agent's file-ownership boundary
// during a parallel wave. The two copies agreed, which is exactly why the
// duplication was worth removing rather than leaving: what the original protects
// is a file of Ed25519 seeds, a divergence between the copies is a lost-update
// bug on private keys, and the reasoning that makes the lock correct (why a
// stale break must be conditional on the ownership token) lived in one copy and
// was merely referenced by the other.

const (
	// cursorsFileName holds one persisted read position per (agent, bus).
	cursorsFileName = "cursors.json"

	// cursorsLockFileName serialises read-modify-write cycles on it. It is a
	// DIFFERENT lock from identities.lock — see the file comment.
	cursorsLockFileName = "cursors.lock"
)

// cursorsFile is the cursor document's storeFile descriptor: the same atomic
// save, the same ownership-token lock, its own file and its own lock.
var cursorsFile = storeFile{
	op:       "cursor store",
	name:     cursorsFileName,
	lockName: cursorsLockFileName,
	what:     "the cursor file",
	lockWhat: "the cursor lock",
	busyWhat: "the cursor file",
}

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
// It is keyed on (AgentID, BusID) — BOTH — because a cursor is bound to ONE
// bus's sequence space. A position from bus A applied to bus B would either
// replay or, far worse, SKIP messages that bus never delivered. (The bus
// enforces the agent half itself: a cursor carries the id it was issued to and
// presenting another agent's cursor is a 400. Nothing enforces the bus half but
// this key.)
//
// # The bus half is the BUS ID, not the URL. That is CLI-3-FU-URLKEY
//
// It was `bus_url`, INCLUDING THE SCHEME, until a real agent migrated across a
// plaintext -> TLS switch and ended up with two records for one agent id:
//
//	{agent_id: …mic-array-1, bus_url: http://127.0.0.1:18080,  cursor: …|266}
//	{agent_id: …mic-array-1, bus_url: https://127.0.0.1:18080, cursor: …|266}
//
// The https record started empty, so the first watch after the flip re-received
// the agent's whole history. At-least-once delivery permits that and message_id
// dedup absorbs it — the scope is what makes it worth fixing. It fires for EVERY
// agent on the bus simultaneously, the moment TLS is required, and any handler
// that reacts per-message rather than deduping on message_id re-acts on its
// entire history at once.
//
// The server-minted bus id is the durable answer to "is this the same bus"
// (invariant 2 — `<bus-id>.<agent-id>` — and invariant 1, which makes the server
// authoritative on it). A URL is not: one bus is reachable at http:// and
// https:// during a migration window, and also across a port move, a DNS change
// or a reverse proxy appearing. All of those are one bus and share one cursor.
//
// bus_url is deliberately NOT retained as a second field. A non-key field that
// looks like a key is how this bug happened once already; the URL a client
// happened to dial is not a fact about the bus.
type cursorRecord struct {
	AgentID string `json:"agent_id"`

	// BusID is the server-minted bus id. A record read from a file written
	// before this change has none and is migrated on load — see
	// migrateCursorRecords.
	BusID  string `json:"bus_id"`
	Cursor string `json:"cursor"`

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

// Cursor returns the persisted read position for (agentID, busID), or "" when
// there is none.
//
// busID is the SERVER-MINTED bus id (invariant 1), not a URL — see cursorRecord
// for why that distinction is the whole of CLI-3-FU-URLKEY.
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
//     `agent-busctl watch` because a position hint is damaged would trade a harmless
//     replay for an outage.
//
// The error return exists for a caller BUG — an empty key — not for a store
// condition.
func (s *Store) Cursor(agentID, busID string) (string, error) {
	if agentID == "" || busID == "" {
		return "", newError(KindInternal, "cursor store",
			"a cursor lookup needs both an agent id and a bus id", "")
	}
	d, ok := s.loadCursors()
	if !ok {
		return "", nil
	}
	for _, rec := range d.Cursors {
		if rec.AgentID != agentID || rec.BusID != busID {
			continue
		}
		if len(rec.Cursor) > maxCursorLen {
			s.warnCursor("the stored cursor for %s on bus %s is %d bytes (the limit is %d); ignoring it and replaying from the start of the retained window",
				agentID, busID, len(rec.Cursor), maxCursorLen)
			return "", nil
		}
		if rec.Cursor != "" && validateServerCursor("cursor store", rec.Cursor) != nil {
			s.warnCursor("the stored cursor for %s on bus %s is not a well-formed opaque token; ignoring it and replaying from the start of the retained window",
				agentID, busID)
			return "", nil
		}
		return rec.Cursor, nil
	}
	return "", nil
}

// SetCursor durably records cursor as the read position for (agentID, busID).
//
// A write failure IS returned. Unlike a read, a caller must be able to tell that
// its position is not being persisted — silently continuing means the stored
// position drifts arbitrarily far behind the real one and nobody finds out until
// a restart replays a day of messages.
func (s *Store) SetCursor(agentID, busID, cursor string) error {
	const op = "cursor store"
	if agentID == "" || busID == "" {
		return newError(KindInternal, op, "a cursor needs both an agent id and a bus id", "")
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
			if d.Cursors[i].AgentID == agentID && d.Cursors[i].BusID == busID {
				d.Cursors[i].Cursor = cursor
				d.Cursors[i].UpdatedAt = now
				return
			}
		}
		d.Cursors = append(d.Cursors, cursorRecord{
			AgentID:   agentID,
			BusID:     busID,
			Cursor:    cursor,
			UpdatedAt: now,
		})
		evictOldestCursors(d)
	})
}

// ClearCursor forgets the position for (agentID, busID). It is a no-op when
// there is none, so `agent-busctl watch --replay` need not check first.
func (s *Store) ClearCursor(agentID, busID string) error {
	const op = "cursor store"
	if agentID == "" || busID == "" {
		return newError(KindInternal, op, "a cursor needs both an agent id and a bus id", "")
	}
	return s.updateCursors(func(d *cursorData) {
		out := d.Cursors[:0]
		for _, rec := range d.Cursors {
			if rec.AgentID == agentID && rec.BusID == busID {
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

// cursorBusID resolves the bus id a cursor is keyed by, from the credential.
//
// The value is DERIVED from the agent id, not read from cred.BusID, and that is
// the deliberate half. Both are recorded by the bus at enrolment, but they are
// not validated to the same standard: cred.BusID is checked only by
// validateServerField (up to 256 bytes of [A-Za-z0-9._-] — dots included),
// whereas the prefix of cred.AgentID is checked by qualifyingBusID against
// busIDRegexp, which is the actual bus-id grammar and excludes '.' precisely
// because '.' is what qualifies an agent id (invariant 2). Keying a local file
// on the looser of two values, when the stricter one is sitting beside it and
// says the same thing, is free to avoid.
//
// cred.BusID is still used — as a CROSS-CHECK. If the bus reported a bus id that
// disagrees with the one inside the agent id it issued, the stored identity is
// self-contradictory and this refuses rather than picking a winner: the two
// disagree about which bus's sequence space this cursor belongs to, and guessing
// wrong is how a cursor gets applied to the wrong bus.
//
// Failing here rather than falling back to something URL-shaped is deliberate
// for the same reason: silently keying a cursor on a substitute would
// reintroduce exactly the ambiguity this key exists to remove.
func cursorBusID(cred Credential) (string, error) {
	const op = "cursor store"
	busID, err := qualifyingBusID(cred.AgentID)
	if err != nil {
		return "", newError(KindConfig, op,
			"the stored identity "+safeText(cred.AgentID, 60)+" carries no bus id and none can be derived from it",
			"re-enrol with `agent-busctl enrol`; every agent id is `<bus-id>.<agent-id>` (invariant 2)")
	}
	if cred.BusID != "" && cred.BusID != busID {
		return "", newError(KindConfig, op,
			"the stored identity "+safeText(cred.AgentID, 60)+" records bus id "+safeText(cred.BusID, 60)+", which is not the bus id inside its own agent id",
			"re-enrol with `agent-busctl enrol`; this credential is self-contradictory about which bus it belongs to")
	}
	return busID, nil
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
	s.migrateCursorRecords(&d)
	return d, true
}

// migrateCursorRecords upgrades records written before CLI-3-FU-URLKEY, which
// were keyed by `bus_url` rather than `bus_id`.
//
// It runs on LOAD rather than as a one-shot rewrite, so a read never has to take
// the write lock and an agent that only ever reads still resolves its position.
// The migrated shape is written back the next time anything calls SetCursor or
// ClearCursor, which is every batch on an active watch.
//
// # Deriving the bus id, and why it needs no file format bump
//
// The bus id is recoverable from the record itself: invariant 2 makes every
// agent id `<bus-id>.<agent-id>`, so the prefix of the stored agent_id IS the
// bus. Nothing has to be guessed from the URL, and no data is lost — which is
// exactly why cursorFormatVersion does not change.
//
// Mixing builds is safe in BOTH directions, which is the test that decides
// whether a version bump is owed:
//
//   - An older build READING a new file finds no bus_url, matches nothing, and
//     replays. At-least-once already permits that; it is what the old build did
//     anyway on any unrecognised record.
//   - An older build WRITING to a new file STRIPS bus_id from every record: its
//     own cursorRecord has no such field, so the value is dropped on unmarshal
//     and absent when updateCursors rewrites the document. That sounds worse
//     than it is — the agent_id, which is what bus_id was derived FROM, is
//     untouched, so the next new-build load re-derives every one of them and
//     collapses any duplicate the old build appended. The file degrades to the
//     old shape and is losslessly re-migrated, rather than losing a position.
//
// A version bump would have bought nothing here and would have cost the older
// build the whole file (loadCursors REFUSES an unknown version), turning a
// harmless mixed-build window into a guaranteed full replay.
//
// # Collapsing the collision
//
// The two url-keyed records a TLS flip produced (http:// and https:// for one
// agent on one bus) both map to the same key, so one must win. The most recently
// UPDATED wins: a cursor is opaque and this client cannot ask which is further
// along, the timestamp is the only ordering available, and choosing the newest
// replays at most the gap between the two rather than the whole history.
//
// A record whose agent_id carries no bus prefix is DROPPED. It cannot be keyed,
// it could never be matched by a lookup, and it would otherwise occupy one of
// the maxStoredCursors slots for ever. Dropping a position hint costs a replay,
// which is what every other damaged-cursor path here already chooses.
func (s *Store) migrateCursorRecords(d *cursorData) {
	migrated := 0
	dropped := 0
	collapsed := 0
	out := d.Cursors[:0]
	// index of the record already kept for a given key, so a collision is
	// resolved in one pass.
	kept := make(map[string]int, len(d.Cursors))

	for _, rec := range d.Cursors {
		if rec.BusID == "" {
			busID, err := qualifyingBusID(rec.AgentID)
			if err != nil {
				dropped++
				continue
			}
			rec.BusID = busID
			migrated++
		}
		key := rec.AgentID + "\x00" + rec.BusID
		if i, seen := kept[key]; seen {
			collapsed++
			if cursorUpdatedAt(rec).After(cursorUpdatedAt(out[i])) {
				out[i] = rec
			}
			continue
		}
		kept[key] = len(out)
		out = append(out, rec)
	}

	if migrated == 0 && dropped == 0 && collapsed == 0 {
		d.Cursors = out
		return
	}
	if len(out) == 0 {
		d.Cursors = nil
	} else {
		d.Cursors = out
	}
	if dropped > 0 {
		s.warnCursor("dropped %d cursor record(s) in %s whose agent id is not fully qualified, so no bus id could be derived; those positions replay from the start of the retained window",
			dropped, s.CursorPath())
	}
	// A discard is ANNOUNCED, never silent. Invariant 6 rates a silent discard
	// as the defect rather than the discard itself, and the same reasoning
	// applies here: the losing record of a collapsed pair is a read position
	// this client is choosing to forget, and the visible consequence — a replay
	// of whatever lies between the two — is otherwise indistinguishable from the
	// bus having re-sent something.
	if collapsed > 0 {
		s.warnCursor("collapsed %d duplicate cursor record(s) in %s onto one (agent, bus) key, keeping the most recently updated of each; this is the one-time migration off the old bus_url key and may replay the messages between the two positions",
			collapsed, s.CursorPath())
	}
}

// updateCursors runs mutate against the cursor file under the cursor lock and
// saves the result.
//
// A file that failed to load is treated as EMPTY here rather than refused: the
// mutation is about to rewrite it wholesale, and refusing to write because the
// previous contents were damaged would wedge a watch permanently on a file
// nothing else will ever repair.
func (s *Store) updateCursors(mutate func(d *cursorData)) error {
	release, err := s.lockFile(cursorsFile)
	if err != nil {
		return err
	}
	defer release()

	s.sweepTempFiles(cursorsFile)

	d, ok := s.loadCursors()
	if !ok {
		d = cursorData{Version: cursorFormatVersion}
	}
	mutate(&d)
	return s.saveCursors(d)
}

// saveCursors writes the cursor document. See (*Store).saveJSON.
//
// It exists as a TYPED wrapper, mirroring (*Store).save, and that is its whole
// job: saveJSON takes an interface{}, so nothing in its signature stops a caller
// pairing the identitiesFile descriptor with cursor data and truncating a file
// of Ed25519 seeds. Both documents now reach saveJSON through exactly one typed
// function each, so the pairing is fixed in one place per document rather than
// at every call site.
//
// The version is stamped HERE rather than inside saveJSON, which is
// document-agnostic: cursorFormatVersion and storeFormatVersion are independent
// schemas that happen to be equal today.
func (s *Store) saveCursors(d cursorData) error {
	d.Version = cursorFormatVersion
	return s.saveJSON(cursorsFile, d)
}
