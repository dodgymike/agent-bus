package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// RELAY-54's acceptance evidence for `agent-bus outbox`.
//
// The command answers ONE operator question — "does this bus still owe anything
// to a peer, and has it given up on anything?" — and answers it in the EXIT
// CODE, because a rollout gate branches on the code without parsing a word of
// output. So the tests here are overwhelmingly about codes, about the two ways
// that answer can be corrupted, and about the ONE thing the command must never
// do while producing it:
//
//   - IT MUST NOT MINT wal-mac.key. wal's macKeyFor creates the key as a SIDE
//     EFFECT OF A READ, so a reader without the guard silently manufactures the
//     key that authenticates the file it is about to judge, and converts a
//     recoverable "the key is missing" into "the key does not match", whose
//     documented remedy is to move bus.wal aside. The fixtures for that test are
//     built on the OBSERVED minting shapes (see ob54MintingWALs) rather than on
//     a guess, because the shapes that do NOT mint make the test unfailable.
//   - FILTERS MUST NOT LAUNDER THE VERDICT. -peer and -state change only what is
//     PRINTED; the code is computed over the WHOLE outbox first. A filter that
//     could turn a 6 into a 0 would make this command's silence meaningless, and
//     the silence is the entire product.
//
// Every helper is prefixed ob54* so it cannot collide with the cli6* fixtures
// auditlog_cli6_test.go owns.

const (
	// ob54BusID is THIS bus. ob54PeerA/B/C are peers: relay refuses a job whose
	// destination is the outbox's own bus, so they must differ from it and from
	// each other.
	ob54BusID     = "bus-outbox54-local"
	ob54PeerA     = "bus-outbox54-peer-a"
	ob54PeerB     = "bus-outbox54-peer-b"
	ob54OriginBus = "bus-outbox54-origin"

	// ob54Hash is a well-formed lowercase hex SHA-256. Its VALUE is irrelevant;
	// its SHAPE is what the durable record validates.
	ob54Hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// ob54AbandonReason is distinctive so the human and JSON renderings can be
	// asserted to carry it.
	ob54AbandonReason = "the peer answered 410 and will never take it"
)

// ob54Build is what a fixture does to a live, durable outbox.
type ob54Build func(t *testing.T, ob *relay.Outbox)

// ob54Dir builds a REAL bus data directory: a persisted bus-id, a real bus.wal
// written through the real two-phase durable path, and the wal-mac.key that path
// creates.
//
// Hand-crafted WAL bytes are deliberately NOT used for any of the happy paths.
// A verdict asserted against bytes this test wrote by hand would be a claim
// about the test's own encoder; asserted against a log the bus itself produced,
// it is a claim about the bus.
func ob54Dir(t *testing.T, build ob54Build) string {
	t.Helper()
	dir := t.TempDir()

	// The SAME id in the bus-id file and in the outbox: readOutbox loads the id
	// from the file, and relay refuses a record addressed to that id, so a
	// fixture built under a different id would be exercising a different bus.
	busID, err := ids.LoadOrCreateBusID(dir, ob54BusID)
	if err != nil {
		t.Fatalf("ids.LoadOrCreateBusID(%s): %v", dir, err)
	}
	if busID != ob54BusID {
		t.Fatalf("bus id = %q, want %q", busID, ob54BusID)
	}

	ob54Session(t, dir, nil, build)

	// The fixture must leave the artefacts the command's guards look for, or
	// every code this file asserts would be the guard's and not the verdict's.
	for _, name := range []string{busIDFileName, wal.MACKeyFileName, wal.WALFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("the fixture left no %s in %s: %v", name, dir, err)
		}
	}
	// And it must NOT leave the lock behind, or the lock test below would pass
	// for the wrong reason.
	if _, err := os.Stat(filepath.Join(dir, dirlock.LockFileName)); !os.IsNotExist(err) {
		t.Fatalf("the fixture left %s behind (stat err = %v)", dirlock.LockFileName, err)
	}
	return dir
}

// ob54Session opens an EXISTING data directory the way the server does — a real
// relay.Outbox attached to a real wal.Log, so wal.Open's own recovery runs — then
// applies build and closes cleanly.
//
// It is separate from ob54Dir for two reasons that both matter to the integrity
// tests below:
//
//   - now injects the CLOCK the durable records are stamped with. A record
//     enqueued under a clock 25 hours in the past is a legitimate durable record
//     that a later read REFUSES as past the retry horizon, which is the only way
//     to build the refusal fixture without hand-writing outbox bytes.
//   - build == nil makes this exactly the SERVER'S STARTUP PATH and nothing else,
//     which is how a damaged log gets repaired in these tests: the repair is the
//     bus's own, not a routine this file invented.
func ob54Session(t *testing.T, dir string, now func() time.Time, build ob54Build) {
	t.Helper()
	lg := logging.New(io.Discard, logging.LevelError)
	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: ob54BusID, Logger: lg, Now: now})
	if err != nil {
		t.Fatalf("relay.NewOutbox: %v", err)
	}
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ob, Logger: lg})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if err := ob.Attach(l); err != nil {
		l.Close()
		t.Fatalf("Outbox.Attach: %v", err)
	}
	if build != nil {
		build(t, ob)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("wal.Log.Close: %v", err)
	}
}

// ob54Enqueue durably records one job.
func ob54Enqueue(t *testing.T, ob *relay.Outbox, peer string, seq uint64) relay.OutboxRecord {
	t.Helper()
	msgID, err := ids.MessageID(ob54OriginBus, seq)
	if err != nil {
		t.Fatalf("ids.MessageID(%s, %d): %v", ob54OriginBus, seq, err)
	}
	rec, err := ob.Enqueue(relay.OutboxJob{
		PeerBusID:       peer,
		OriginMessageID: msgID,
		Size:            11,
		ContentSHA256:   ob54Hash,
	})
	if err != nil {
		t.Fatalf("Enqueue(peer %s, seq %d): %v", peer, seq, err)
	}
	return rec
}

// ob54Settle moves a job to a terminal state through the real durable path.
func ob54Settle(t *testing.T, ob *relay.Outbox, jobID string, state relay.OutboxState, reason string) {
	t.Helper()
	if _, err := ob.Settle(jobID, state, reason); err != nil {
		t.Fatalf("Settle(%s, %s): %v", jobID, state, err)
	}
}

// ob54Pending, ob54Delivered and ob54Abandoned are the three one-job fixtures
// the verdict table is composed from.
func ob54Pending(peer string, seq uint64) ob54Build {
	return func(t *testing.T, ob *relay.Outbox) { ob54Enqueue(t, ob, peer, seq) }
}

func ob54Delivered(peer string, seq uint64) ob54Build {
	return func(t *testing.T, ob *relay.Outbox) {
		rec := ob54Enqueue(t, ob, peer, seq)
		ob54Settle(t, ob, rec.JobID, relay.OutboxDelivered, "")
	}
}

func ob54Abandoned(peer string, seq uint64) ob54Build {
	return func(t *testing.T, ob *relay.Outbox) {
		rec := ob54Enqueue(t, ob, peer, seq)
		ob54Settle(t, ob, rec.JobID, relay.OutboxAbandoned, ob54AbandonReason)
	}
}

// ob54All composes several builds into one fixture.
func ob54All(builds ...ob54Build) ob54Build {
	return func(t *testing.T, ob *relay.Outbox) {
		for _, b := range builds {
			b(t, ob)
		}
	}
}

// ob54Run invokes the command in process. runOutboxCommand returns an exit code
// and never calls os.Exit, which is the seam this whole file relies on.
func ob54Run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runOutboxCommand(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// ob54JSON decodes the ONE object -json emits, asserting on the way through
// that it IS one object and not NDJSON.
func ob54JSON(t *testing.T, stdout string) map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var obj map[string]interface{}
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("-json stdout does not decode as a JSON object: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("-json emitted MORE than one object; the contract is ONE object, not NDJSON:\n%s", stdout)
	}
	return obj
}

// ob54Count reads one field out of the counts object.
func ob54Count(t *testing.T, obj map[string]interface{}, field string) int {
	t.Helper()
	counts, ok := obj["counts"].(map[string]interface{})
	if !ok {
		t.Fatalf("the result object has no counts object: %v", obj)
	}
	v, ok := counts[field].(float64)
	if !ok {
		t.Fatalf("counts.%s is missing or not a number: %v", field, counts)
	}
	return int(v)
}

// ob54JobIDs is the SELECTED job list, in order.
func ob54JobIDs(t *testing.T, obj map[string]interface{}) []string {
	t.Helper()
	raw, ok := obj["jobs"].([]interface{})
	if !ok {
		t.Fatalf("the result object has no jobs array: %v", obj)
	}
	out := make([]string, 0, len(raw))
	for i, j := range raw {
		m, ok := j.(map[string]interface{})
		if !ok {
			t.Fatalf("jobs[%d] is not an object: %v", i, j)
		}
		id, _ := m["job_id"].(string)
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. The verdict, and the rule that filters cannot change it
// ---------------------------------------------------------------------------

// TestOutboxCommandVerdict is the exit-code table. Every fixture is built by
// running a REAL outbox against a REAL wal.
//
// The last two rows are the point: a -state or -peer filter that selects NOTHING
// still exits 6 while a job is pending. They are separated from the rest because
// they are the rows that go red if the filters are ever moved above the verdict.
func TestOutboxCommandVerdict(t *testing.T) {
	cases := []struct {
		name  string
		build ob54Build
		args  []string
		want  int
		// wantSelected, when non-nil, is how many jobs the FILTERED list holds.
		// It is what proves the filter really did apply while the verdict did
		// not move: a row asserting only the code would also pass on a build
		// that ignored -state entirely.
		wantSelected *int
	}{
		{
			name:  "an empty outbox is drained",
			build: nil,
			want:  exitOutboxOK,
		},
		{
			name:  "only delivered jobs is drained",
			build: ob54All(ob54Delivered(ob54PeerA, 1), ob54Delivered(ob54PeerB, 2)),
			want:  exitOutboxOK,
		},
		{
			name:  "one pending job is NOT drained",
			build: ob54Pending(ob54PeerA, 1),
			want:  exitOutboxPending,
		},
		{
			name:  "one abandoned job and nothing pending",
			build: ob54Abandoned(ob54PeerA, 1),
			want:  exitOutboxAbandoned,
		},
		{
			// 6 TAKES PRECEDENCE over 7: "do not restart yet" outranks
			// "something was already lost".
			name:  "pending AND abandoned reports pending",
			build: ob54All(ob54Pending(ob54PeerA, 1), ob54Abandoned(ob54PeerB, 2)),
			want:  exitOutboxPending,
		},
		{
			// -state delivered SELECTS ONLY THE DELIVERED JOB and still exits 6,
			// because a pending job exists. If the filter ran before the verdict
			// this row would report 0 — a rollout script would restart onto a
			// new binary with a message still owed.
			name:         "a -state filter that hides the pending job still exits 6",
			build:        ob54All(ob54Pending(ob54PeerA, 1), ob54Delivered(ob54PeerB, 2)),
			args:         []string{"-state", "delivered"},
			want:         exitOutboxPending,
			wantSelected: ob54Int(1),
		},
		{
			// Same trap through -peer: the peer named here is owed NOTHING, and
			// the pending job belongs to the other one.
			name:         "a -peer filter that hides the pending job still exits 6",
			build:        ob54All(ob54Pending(ob54PeerA, 1), ob54Delivered(ob54PeerB, 2)),
			args:         []string{"-peer", ob54PeerB},
			want:         exitOutboxPending,
			wantSelected: ob54Int(1),
		},
		{
			// And a filter selecting nothing at all is still not a clean answer.
			name:         "a -peer filter selecting nothing still exits 6",
			build:        ob54Pending(ob54PeerA, 1),
			args:         []string{"-peer", "bus-outbox54-nobody"},
			want:         exitOutboxPending,
			wantSelected: ob54Int(0),
		},
	}

	for _, tc := range cases {
		tc := tc
		// NOT parallel: every run takes the data directory's exclusive lock, and
		// two runs racing for one directory would make this an assertion about
		// scheduling rather than about the verdict.
		t.Run(tc.name, func(t *testing.T) {
			dir := ob54Dir(t, tc.build)

			args := append([]string{"-data-dir", dir}, tc.args...)
			code, stdout, stderr := ob54Run(t, args...)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.want, stdout, stderr)
			}

			// The same verdict through -json, and the exit code echoed INSIDE the
			// object: an agent that captured only stdout must reach the same
			// answer a shell branching on $? does.
			jsonArgs := append([]string{"-data-dir", dir, "-json"}, tc.args...)
			jcode, jstdout, jstderr := ob54Run(t, jsonArgs...)
			if jcode != tc.want {
				t.Fatalf("-json exit = %d, want %d\nstdout: %s\nstderr: %s", jcode, tc.want, jstdout, jstderr)
			}
			obj := ob54JSON(t, jstdout)
			if got, _ := obj["exit_code"].(float64); int(got) != tc.want {
				t.Fatalf("-json exit_code = %v, want %d (it must equal the process exit code)", obj["exit_code"], tc.want)
			}
			if tc.wantSelected != nil {
				if got := ob54JobIDs(t, obj); len(got) != *tc.wantSelected {
					t.Fatalf("the filter selected %d job(s) (%v), want %d; the filter must apply to the PRINTED list even though it does not move the verdict",
						len(got), got, *tc.wantSelected)
				}
			}
		})
	}
}

func ob54Int(n int) *int { return &n }

// TestOutboxCommandFilteredCountsAreStillWholeOutbox: counts and the per-peer
// breakdowns are the VERDICT and are never filtered, while jobs is only what was
// selected. Two fields that can legitimately disagree must be pinned, or a
// future "consistency" fix would quietly make the filter able to launder a 6.
func TestOutboxCommandFilteredCountsAreStillWholeOutbox(t *testing.T) {
	dir := ob54Dir(t, ob54All(
		ob54Pending(ob54PeerA, 1),
		ob54Delivered(ob54PeerB, 2),
		ob54Abandoned(ob54PeerB, 3),
	))

	code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json", "-state", "delivered")
	if code != exitOutboxPending {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOutboxPending, stderr)
	}
	obj := ob54JSON(t, stdout)

	for field, want := range map[string]int{"retained": 3, "pending": 1, "delivered": 1, "abandoned": 1} {
		if got := ob54Count(t, obj, field); got != want {
			t.Fatalf("counts.%s = %d, want %d; counts are over the WHOLE outbox, never the filtered selection:\n%s", field, got, want, stdout)
		}
	}
	if got := ob54JobIDs(t, obj); len(got) != 1 || !strings.HasPrefix(got[0], ob54PeerB+"|") {
		t.Fatalf("jobs = %v, want exactly the one delivered job for %s", got, ob54PeerB)
	}
	// The per-peer breakdowns are the verdict's detail and are equally unfiltered.
	if rows, ok := obj["pending_by_peer"].([]interface{}); !ok || len(rows) != 1 {
		t.Fatalf("pending_by_peer = %v, want the one unfiltered pending peer", obj["pending_by_peer"])
	}
	if rows, ok := obj["abandoned_by_peer"].([]interface{}); !ok || len(rows) != 1 {
		t.Fatalf("abandoned_by_peer = %v, want the one unfiltered abandoned peer", obj["abandoned_by_peer"])
	}
}

// ---------------------------------------------------------------------------
// 2. THE wal-mac.key MINT GUARD
// ---------------------------------------------------------------------------

// ob54MintingWALs are the ONLY bus.wal shapes for which wal.Replay reaches
// macKeyFor and MINTS wal-mac.key.
//
// This was established empirically against HEAD, not guessed, and the table is
// written down because the negative result is the dangerous half: an ABSENT or
// ZERO-LENGTH bus.wal takes Replay's early empty-log exit and mints NOTHING, so
// a guard test built on either of those shapes CANNOT FAIL and would be a dead
// guard — of which this repository already has four.
//
// Each shape is one whose header does not positively identify itself as format
// version 2, which is what makes macKeyMayBeCreated permit creation.
var ob54MintingWALs = []struct {
	name  string
	bytes []byte
}{
	{
		// Garbage magic: a full-length header that is not a recognised one.
		name:  "non-empty with garbage magic",
		bytes: []byte("NOTAWALFILE-BUT-LONG-ENOUGH-TO-HAVE-A-HEADER"),
	},
	{
		// A header cut off mid-magic: too short to identify, long enough to be
		// non-empty.
		name:  "a three-byte truncated header",
		bytes: []byte("AGN"),
	},
}

// TestOutboxCommandRefusesAMissingMACKeyAndMintsNothing is the guard that keeps
// a read-only evidence tool from destroying the artefact it was asked about.
//
// wal's macKeyFor CREATES wal-mac.key as a SIDE EFFECT OF A READ. Without the
// pre-lock and post-lock checks in readOutbox, this command would silently
// manufacture the key that authenticates the log it is about to judge — and, as
// the probe against `agent-bus log` showed, convert a recoverable
// wal.ErrMACKeyMissing into wal.ErrMACKeyMismatch, whose documented remedy is to
// move bus.wal aside. "Restore a 64-byte file" would become "destroy the
// write-ahead log".
//
// The refusal is PRE-LOCK, so it writes NOTHING AT ALL: not the key, and not the
// bus.lock whose mere presence makes an operator's next real start refuse to
// boot.
func TestOutboxCommandRefusesAMissingMACKeyAndMintsNothing(t *testing.T) {
	for _, shape := range ob54MintingWALs {
		shape := shape
		for _, asJSON := range []bool{false, true} {
			asJSON := asJSON
			name := shape.name
			if asJSON {
				name += " (-json)"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				if _, err := ids.LoadOrCreateBusID(dir, ob54BusID); err != nil {
					t.Fatalf("ids.LoadOrCreateBusID(%s): %v", dir, err)
				}
				walPath := filepath.Join(dir, wal.WALFileName)
				if err := os.WriteFile(walPath, shape.bytes, 0o600); err != nil {
					t.Fatalf("writing the fixture %s: %v", wal.WALFileName, err)
				}
				keyPath := filepath.Join(dir, wal.MACKeyFileName)
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("the fixture already holds a %s (stat err = %v), so this test asserts nothing", wal.MACKeyFileName, err)
				}

				args := []string{"-data-dir", dir}
				if asJSON {
					args = append(args, "-json")
				}
				code, stdout, stderr := ob54Run(t, args...)

				// THE ASSERTION THE TEST EXISTS FOR, AND IT IS CHECKED FIRST ON
				// PURPOSE. Removing the guard makes this command answer exit 1
				// instead of 5, so an exit-code assertion placed above this one
				// would fail first and the mint would never be looked at — the
				// mutation would go red for the less important reason, and a
				// future change that kept the code while reinstating the mint
				// would slip through.
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("the refusal MINTED %s (stat err = %v). A read-only command has manufactured the key that authenticates "+
						"the log it was asked to judge, turning \"restore a 64-byte file\" into \"the key does not match, move %s aside\""+
						"\n  exit was %d\n  stdout: %s\n  stderr: %s",
						wal.MACKeyFileName, err, wal.WALFileName, code, stdout, stderr)
				}
				if code != exitOutboxUnverifiable {
					t.Fatalf("exit = %d, want %d (UNVERIFIABLE)\nstdout: %s\nstderr: %s",
						code, exitOutboxUnverifiable, stdout, stderr)
				}
				// The guard is PRE-LOCK, so the refusal wrote NOTHING AT ALL.
				if _, err := os.Stat(filepath.Join(dir, dirlock.LockFileName)); !os.IsNotExist(err) {
					t.Fatalf("the refusal left %s behind (stat err = %v); a lone lock file makes the operator's first real start refuse to boot",
						dirlock.LockFileName, err)
				}
				// Nothing beyond the two files the fixture itself wrote.
				if got, want := ob54DirEntries(t, dir), []string{busIDFileName, wal.WALFileName}; !reflect.DeepEqual(got, want) {
					t.Fatalf("the data directory now holds %v, want only the fixture's %v", got, want)
				}

				// NOTHING WAS PRINTED as a job record: the command has no standing
				// to describe records it could not authenticate.
				if strings.Contains(stdout, ob54PeerA) || strings.Contains(stdout, "VERDICT") || strings.Contains(stdout, "\"jobs\"") {
					t.Fatalf("the refusal printed a report:\n%s", stdout)
				}
				if asJSON {
					obj := ob54JSON(t, stdout)
					if ok, _ := obj["ok"].(bool); ok {
						t.Fatalf("the failure object has ok = true: %v", obj)
					}
					if got, _ := obj["exit_code"].(float64); int(got) != exitOutboxUnverifiable {
						t.Fatalf("failure exit_code = %v, want %d", obj["exit_code"], exitOutboxUnverifiable)
					}
					if _, bad := obj["jobs"]; bad {
						t.Fatalf("the failure object carries a jobs key: %v", obj)
					}
					if !strings.Contains(fmt.Sprint(obj["error"]), wal.MACKeyFileName) {
						t.Fatalf("the failure object does not name %s: %v", wal.MACKeyFileName, obj)
					}
				} else {
					if !strings.Contains(stderr, wal.MACKeyFileName) {
						t.Fatalf("stderr does not name %s:\n%s", wal.MACKeyFileName, stderr)
					}
					if !strings.Contains(stderr, "no key was created") {
						t.Fatalf("stderr does not state that NO KEY WAS CREATED, which is the load-bearing half of this refusal:\n%s", stderr)
					}
				}
			})
		}
	}
}

// ob54DirEntries is the sorted file list of a data directory.
func ob54DirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestOutboxCommandMintingFixturesReallyMint is the CONTROL for the guard test
// above, and it is not decoration.
//
// It proves the two fixture shapes are ones on which wal.Replay DOES reach
// macKeyFor: the same directory, read with wal.Replay directly and no guard in
// front of it, gains a wal-mac.key. Without this, a future change to wal that
// stopped minting on these shapes would leave the guard test passing for the
// wrong reason — asserting the absence of a file nothing would have created —
// and the real guard could then be deleted in silence.
func TestOutboxCommandMintingFixturesReallyMint(t *testing.T) {
	for _, shape := range ob54MintingWALs {
		shape := shape
		t.Run(shape.name, func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, wal.WALFileName)
			if err := os.WriteFile(walPath, shape.bytes, 0o600); err != nil {
				t.Fatalf("writing the fixture %s: %v", wal.WALFileName, err)
			}
			keyPath := filepath.Join(dir, wal.MACKeyFileName)
			if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
				t.Fatalf("the fixture already holds a %s: %v", wal.MACKeyFileName, err)
			}

			// The unguarded read the command must never perform. Its ERROR is
			// irrelevant; its SIDE EFFECT is the whole point.
			_, _ = wal.Replay(walPath, func(wal.Committed) error { return nil })

			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("an unguarded wal.Replay over %q did NOT mint %s (%v), so the guard test built on this shape cannot fail "+
					"and is a dead guard; pick a shape that does mint", shape.name, wal.MACKeyFileName, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. The other refusals
// ---------------------------------------------------------------------------

// TestOutboxCommandRefusesADirectoryWithNoBusID: a directory with no bus-id file
// is not a bus's data directory. The refusal is exit 4, and it must write
// NOTHING — above all it must not let ids.LoadOrCreateBusID's "Create" half mint
// a fresh identity, which would rename the bus away from every agent id it has
// ever issued (invariants 1 and 2).
func TestOutboxCommandRefusesADirectoryWithNoBusID(t *testing.T) {
	dir := t.TempDir()

	code, stdout, stderr := ob54Run(t, "-data-dir", dir)
	if code != exitOutboxNoIdentity {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOutboxNoIdentity, stdout, stderr)
	}
	if !strings.Contains(stderr, busIDFileName) {
		t.Fatalf("stderr does not name the %s file:\n%s", busIDFileName, stderr)
	}
	if got := ob54DirEntries(t, dir); len(got) != 0 {
		t.Fatalf("the refusal created %v in the data directory; it must write NOTHING — not a bus-id, and not a %s",
			got, dirlock.LockFileName)
	}
}

// TestOutboxCommandRefusesAnAbsentDataDir: a mistyped -data-dir is exit 4 and
// the path is NOT created. run() does MkdirAll because a server is entitled to
// start a fresh bus; a read-only reader is not, and a typo that minted a whole
// new identity would report a DRAINED outbox for a bus that does not exist.
func TestOutboxCommandRefusesAnAbsentDataDir(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "no-such-data-dir")

	code, stdout, stderr := ob54Run(t, "-data-dir", missing)
	if code != exitOutboxNoIdentity {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOutboxNoIdentity, stdout, stderr)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the refusal CREATED %s (stat err = %v); a typo in -data-dir must never mint a data directory", missing, err)
	}

	// A path that exists but is a FILE is the same refusal for the same reason.
	notADir := filepath.Join(parent, "a-file")
	if err := os.WriteFile(notADir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", notADir, err)
	}
	code, _, stderr = ob54Run(t, "-data-dir", notADir)
	if code != exitOutboxNoIdentity {
		t.Fatalf("-data-dir pointing at a file: exit = %d, want %d (stderr: %s)", code, exitOutboxNoIdentity, stderr)
	}
	if !strings.Contains(stderr, "not a directory") {
		t.Fatalf("stderr does not say the path is not a directory:\n%s", stderr)
	}
}

// TestOutboxCommandRefusesWhenTheDataDirectoryIsLocked. flock is held per open
// file description, so a second Acquire in THIS process conflicts exactly as a
// running bus would.
//
// This test does NOT call t.Parallel: the property under test IS a lock, and
// interleaving another run against the same directory would make the assertion
// about scheduling.
func TestOutboxCommandRefusesWhenTheDataDirectoryIsLocked(t *testing.T) {
	dir := ob54Dir(t, ob54Pending(ob54PeerA, 1))

	lock, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("dirlock.Acquire(%s): %v", dir, err)
	}
	defer lock.Release()

	code, stdout, stderr := ob54Run(t, "-data-dir", dir)
	if code != exitOutboxBusRunning {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOutboxBusRunning, stdout, stderr)
	}
	if !strings.Contains(stderr, "locked") {
		t.Fatalf("stderr does not say the directory is locked:\n%s", stderr)
	}
	// The message must name the REMEDY, not just the symptom.
	if !strings.Contains(stderr, "stop the bus") {
		t.Fatalf("stderr does not tell the operator to stop the bus:\n%s", stderr)
	}
	if strings.Contains(stdout, "VERDICT") {
		t.Fatalf("a report was printed despite the lock refusal:\n%s", stdout)
	}

	// And once the lock is released the same command answers, so the refusal
	// really was the lock and not something permanent about the fixture. The
	// pending job makes that answer 6 rather than 0, which also proves the
	// fixture had something to say all along.
	if err := lock.Release(); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	code, _, stderr = ob54Run(t, "-data-dir", dir)
	if code != exitOutboxPending {
		t.Fatalf("after releasing the lock, exit = %d, want %d (stderr: %s)", code, exitOutboxPending, stderr)
	}
}

// TestOutboxCommandDamagedWALIsExit1AndSaysTheQuestionWasNotAnswered.
//
// THE MAC KEY IS PRESENT, deliberately: without it the exit-5 refusal fires
// first and this test would be asserting about the guard rather than about
// damage. Exit 1 is NOT "nothing is pending" — it is "this command does not
// know" — and the message has to say so, because a rollout script whose operator
// reads a damage report as a drain report restarts onto a new binary with
// messages still owed.
func TestOutboxCommandDamagedWALIsExit1AndSaysTheQuestionWasNotAnswered(t *testing.T) {
	dir := ob54Dir(t, ob54Pending(ob54PeerA, 1))
	walPath := filepath.Join(dir, wal.WALFileName)
	if err := os.WriteFile(walPath, []byte("NOTAWALFILE-BUT-LONG-ENOUGH-TO-HAVE-A-HEADER"), 0o600); err != nil {
		t.Fatalf("damaging %s: %v", walPath, err)
	}
	if _, err := os.Stat(filepath.Join(dir, wal.MACKeyFileName)); err != nil {
		t.Fatalf("the fixture left no %s, so this would assert the exit-5 refusal rather than damage: %v", wal.MACKeyFileName, err)
	}

	code, stdout, stderr := ob54Run(t, "-data-dir", dir)
	if code != exitOutboxDamaged {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOutboxDamaged, stdout, stderr)
	}
	if !strings.Contains(stderr, "THE QUESTION WAS NOT ANSWERED") {
		t.Fatalf("stderr does not state that the question was NOT answered, which is the difference between exit 1 and exit 0:\n%s", stderr)
	}
	if !strings.Contains(stderr, "not evidence that the outbox is drained") {
		t.Fatalf("stderr does not warn against reading damage as a drain:\n%s", stderr)
	}
	if strings.Contains(stdout, "DRAINED") {
		t.Fatalf("a verdict was printed over a log that could not be read:\n%s", stdout)
	}
}

// TestOutboxCommandUsageErrorsAreExit2 covers every way argv can be wrong.
func TestOutboxCommandUsageErrorsAreExit2(t *testing.T) {
	dir := ob54Dir(t, nil)

	cases := []struct {
		name string
		args []string
	}{
		{name: "an unknown flag", args: []string{"-data-dir", dir, "-nope"}},
		{name: "an unexpected positional argument", args: []string{"-data-dir", dir, "surprise"}},
		{name: "an unknown -state value", args: []string{"-data-dir", dir, "-state", "qqq"}},
		{name: "an empty -data-dir", args: []string{"-data-dir", "  "}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := ob54Run(t, tc.args...)
			if code != exitOutboxUsage {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOutboxUsage, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("a usage error wrote to stdout; diagnostics belong on stderr:\n%s", stdout)
			}
			if stderr == "" {
				t.Fatalf("a usage error printed nothing to stderr")
			}
		})
	}

	// The offending VALUE is unvalidated argv on its way to a terminal and is
	// never echoed; the legal set is the useful half of the message.
	code, _, stderr := ob54Run(t, "-data-dir", dir, "-state", "qqq")
	if code != exitOutboxUsage {
		t.Fatalf("exit = %d, want %d", code, exitOutboxUsage)
	}
	if !strings.Contains(stderr, "must be one of pending, delivered, abandoned") {
		t.Fatalf("stderr does not name the legal -state values:\n%s", stderr)
	}
	if code, _, stderr := ob54Run(t, "-data-dir", dir, "surprise-argument"); code == exitOutboxUsage && strings.Contains(stderr, "surprise-argument") {
		t.Fatalf("the usage error echoed the unvalidated argument back to the terminal:\n%s", stderr)
	}
}

// TestOutboxCommandHelpIsExit0OnStdoutAndCarriesBothLimits: requested help is
// OUTPUT, so it goes to stdout with exit 0 — an agent that captured stdout gets
// the text, and a shell that asked for help does not see a failure.
func TestOutboxCommandHelpIsExit0OnStdoutAndCarriesBothLimits(t *testing.T) {
	code, stdout, stderr := ob54Run(t, "-h")
	if code != exitOutboxOK {
		t.Fatalf("-h exit = %d, want %d (stderr: %s)", code, exitOutboxOK, stderr)
	}
	if stdout == "" {
		t.Fatalf("-h printed nothing to stdout")
	}
	if !strings.Contains(stdout, "USAGE") {
		t.Fatalf("-h did not print the usage text:\n%s", stdout)
	}
	for _, needle := range ob54HelpLimitLines {
		if !strings.Contains(stdout, needle) {
			t.Fatalf("-h does not carry the honesty limit %q:\n%s", needle, stdout)
		}
	}
	// Both new exit codes are documented where an operator will look for them.
	for _, needle := range []string{
		"6  read cleanly; at least one job is PENDING",
		"7  read cleanly; nothing pending, but at least one job is ABANDONED",
	} {
		if !strings.Contains(stdout, needle) {
			t.Fatalf("-h does not document %q:\n%s", needle, stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. The honesty limits, and invariant 6
// ---------------------------------------------------------------------------

// ob54HelpLimitLines are the SPECIFIC lines each honesty limit is pinned by, in
// the -h text.
//
// They are whole clauses rather than a word like "24" or "abandoned", both of
// which appear incidentally elsewhere in the same output: an incidental match is
// how a doc proof passes over a fix that was never made.
var ob54HelpLimitLines = []string{
	"IT CAN ONLY EVER ANSWER ABOUT THE LAST 24 HOURS",
	`"NOTHING ABANDONED" DOES NOT MEAN "NOTHING LOST"`,
}

// ob54HumanLimitLines are the same two limits as they appear in the REPORT,
// which is a different string from the usage text and must carry them too.
var ob54HumanLimitLines = []string{
	"LAST 24 HOURS",
	`"NOTHING ABANDONED" DOES NOT MEAN "NOTHING LOST"`,
}

// TestOutboxCommandHumanOutputCarriesBothHonestyLimits.
//
// The quiet answer is the dangerous one: an operator who reads "nothing
// abandoned" without limit 2 concludes nothing was LOST, which this command
// cannot support — a pending job dropped at the retry horizon leaves NO durable
// tombstone, only a WARN line in the server's log at the moment it was dropped.
// So the limits are asserted on the DRAINED run, where the temptation to omit
// them is greatest, as well as on a run that reports jobs.
func TestOutboxCommandHumanOutputCarriesBothHonestyLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build ob54Build
		want  int
	}{
		{name: "a drained outbox", build: nil, want: exitOutboxOK},
		{name: "an outbox with a pending job", build: ob54Pending(ob54PeerA, 1), want: exitOutboxPending},
		{name: "an outbox with an abandoned job", build: ob54Abandoned(ob54PeerA, 1), want: exitOutboxAbandoned},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := ob54Dir(t, tc.build)
			code, stdout, stderr := ob54Run(t, "-data-dir", dir)
			if code != tc.want {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, tc.want, stderr)
			}
			if !strings.Contains(stdout, "LIMITS OF THIS VIEW") {
				t.Fatalf("the report has no LIMITS OF THIS VIEW block:\n%s", stdout)
			}
			for _, needle := range ob54HumanLimitLines {
				if !strings.Contains(stdout, needle) {
					t.Fatalf("the report does not carry the honesty limit %q:\n%s", needle, stdout)
				}
			}
		})
	}
}

// TestOutboxCommandJSONShape pins the machine contract in one place: ONE object,
// ok first, counts over the whole outbox, jobs reflecting the filters, exit_code
// matching the process code, and limits as a TWO-element array — because an
// agent consuming this must reach the same caveats a human reading the text
// does, and a caveat only humans can see is a caveat that gets dropped.
//
// The empty slices are asserted on the RAW JSON TEXT. Unmarshalling into
// interface{} renders both `[]` and `null` as a nil slice, so a check made after
// decoding cannot tell them apart — and `null` is exactly what a consumer that
// indexes without a nil check would crash on.
func TestOutboxCommandJSONShape(t *testing.T) {
	t.Run("a drained outbox emits empty arrays, never null", func(t *testing.T) {
		dir := ob54Dir(t, nil)
		code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
		if code != exitOutboxOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOutboxOK, stderr)
		}
		if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
			t.Fatalf("-json emitted more than one line; the contract is ONE object, not NDJSON:\n%s", stdout)
		}
		for _, needle := range []string{`"pending_by_peer":[]`, `"abandoned_by_peer":[]`, `"jobs":[]`} {
			if !strings.Contains(stdout, needle) {
				t.Fatalf("the raw JSON does not contain %s; an empty slice must be [] and NEVER null, so a consumer can index without a nil check:\n%s",
					needle, stdout)
			}
		}
		obj := ob54JSON(t, stdout)
		if ok, _ := obj["ok"].(bool); !ok {
			t.Fatalf("ok is not true on a clean read: %v", obj)
		}
		if got, _ := obj["bus_id"].(string); got != ob54BusID {
			t.Fatalf("bus_id = %q, want %q", got, ob54BusID)
		}
		if got, _ := obj["data_dir"].(string); got != dir {
			t.Fatalf("data_dir = %q, want %q", got, dir)
		}
		limits, ok := obj["limits"].([]interface{})
		if !ok || len(limits) != 2 {
			t.Fatalf("limits = %v, want a TWO-element array (the retention window, and \"nothing abandoned\" is not \"nothing lost\")", obj["limits"])
		}
		joined := fmt.Sprint(limits...)
		for _, needle := range ob54HumanLimitLines {
			if !strings.Contains(joined, needle) {
				t.Fatalf("the JSON limits array does not carry %q: %v", needle, limits)
			}
		}
		if got, _ := obj["retention_window_seconds"].(float64); int(got) <= 0 {
			t.Fatalf("retention_window_seconds = %v, want the window as a NUMBER so a script need not parse \"24 hours\" out of English", obj["retention_window_seconds"])
		}
		if got, _ := obj["exit_code"].(float64); int(got) != exitOutboxOK {
			t.Fatalf("exit_code = %v, want %d", obj["exit_code"], exitOutboxOK)
		}
	})

	t.Run("a job object carries routing only, never a body", func(t *testing.T) {
		dir := ob54Dir(t, ob54All(
			ob54Pending(ob54PeerA, 1),
			ob54Abandoned(ob54PeerB, 2),
		))
		code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
		if code != exitOutboxPending {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOutboxPending, stderr)
		}
		obj := ob54JSON(t, stdout)
		raw, _ := obj["jobs"].([]interface{})
		if len(raw) != 2 {
			t.Fatalf("jobs holds %d entries, want 2:\n%s", len(raw), stdout)
		}

		// INVARIANT 6: the record holds routing and accounting ONLY. The key set
		// is asserted EXACTLY rather than by scanning for a forbidden name, so a
		// field added later has to be named here — which is the moment someone
		// would have to justify it.
		wantByState := map[string][]string{
			"pending":   {"content_sha256", "enqueued_at", "job_id", "origin_message_id", "peer_bus_id", "size", "state"},
			"abandoned": {"content_sha256", "enqueued_at", "job_id", "origin_message_id", "peer_bus_id", "reason", "settled_at", "size", "state"},
		}
		seen := map[string]bool{}
		for i, j := range raw {
			m, ok := j.(map[string]interface{})
			if !ok {
				t.Fatalf("jobs[%d] is not an object: %v", i, j)
			}
			state, _ := m["state"].(string)
			want, known := wantByState[state]
			if !known {
				t.Fatalf("jobs[%d] has unexpected state %q", i, state)
			}
			seen[state] = true
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("jobs[%d] (%s) key set = %v, want exactly %v; a body field here would be an invariant 6 violation",
					i, state, keys, want)
			}
			for _, forbidden := range []string{"payload", "body", "raw", "content", "message_body"} {
				if _, bad := m[forbidden]; bad {
					t.Fatalf("jobs[%d] carries a %q field: the outbox record holds routing and accounting ONLY (invariant 6)", i, forbidden)
				}
			}
		}
		if !seen["pending"] || !seen["abandoned"] {
			t.Fatalf("the fixture did not exercise both key sets: %v", seen)
		}
		// The abandonment REASON reaches the operator: invariant 6 makes the
		// silent discard the defect, so a lost message that cannot say why is a
		// silent discard with a timestamp.
		if !strings.Contains(stdout, ob54AbandonReason) {
			t.Fatalf("the JSON does not carry the abandonment reason:\n%s", stdout)
		}
	})

	t.Run("the human report carries no body field either", func(t *testing.T) {
		dir := ob54Dir(t, ob54Abandoned(ob54PeerA, 1))
		code, stdout, stderr := ob54Run(t, "-data-dir", dir)
		if code != exitOutboxAbandoned {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOutboxAbandoned, stderr)
		}
		// The abandonment reason IS printed, and it is the only free text in the
		// record: invariant 6 makes the SILENT discard the defect, so a lost
		// message that cannot say why is a silent discard with a timestamp.
		//
		// It is printed QUOTED — strconv.Quote — and the assertion says so
		// rather than looking for the bare text, because the quoting is a
		// SECURITY CONTROL and an assertion that passes either way would let it
		// be deleted in silence. See
		// TestOutboxCommandStoredReasonCannotRepaintTheTerminal for what the
		// quoting stops.
		if !strings.Contains(stdout, "REASON:  "+strconv.Quote(ob54AbandonReason)) {
			t.Fatalf("the report does not print the abandonment reason, QUOTED, on its own REASON line:\n%s", stdout)
		}
		// The two quantitative facts about content invariant 6 keeps in a routing
		// record are there; the bytes themselves are not.
		if !strings.Contains(stdout, "bytes, sha256 "+ob54Hash) {
			t.Fatalf("the report does not print the size and content hash:\n%s", stdout)
		}
		for _, forbidden := range []string{"payload", "\"body\"", "body bytes"} {
			if strings.Contains(stdout, forbidden) {
				t.Fatalf("the human report mentions %q, which suggests a body reached the output (invariant 6):\n%s", forbidden, stdout)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 5. CAN THE ANSWER BE BELIEVED — the integrity of the read
// ---------------------------------------------------------------------------
//
// This section is the P0 the review gates blocked on, and it is worth stating
// what the defect WAS, because every test below is shaped by it: a bus.wal that
// had thrown away the one PENDING record in the file produced
//
//	VERDICT: DRAINED — nothing pending and nothing abandoned ...
//	         It is safe to start the new binary.
//
// with exit 0 and an EMPTY stderr. wal.Replay reports record-level discards in
// its Recovered value and returns a NIL ERROR for them — it takes no logger on
// purpose — so a reader that keeps only the error reads a mangled log as a clean
// one. That is the silent discard invariant 6 rates as the defect (the discard
// itself is sanctioned; the silence is not), and on THIS command it converts
// into an operator restarting onto a new binary with a message still owed.
//
// So the tests here assert the two halves separately:
//
//   - THE CODE. Exit 0 must be UNREACHABLE whenever anything was discarded,
//     refused or missing: 1 > 8 > 6 > 7 > 0, trust before verdict.
//   - THE CLASSIFICATION. Bytes thrown away is 1; a hole in the index sequence
//     is 8 and NOT 1, because wal documents a hole as an UPPER BOUND on loss
//     that is often just an index a crash burned (invariant 1 — an authorised
//     index is never authorised again). A channel that cries wolf on every bus
//     that ever crashed is the mirror image of the silent discard.
//
// EVERY DAMAGED FIXTURE IS BUILT BY FLIPPING ONE BYTE IN A LOG THE BUS ITSELF
// WROTE AND THEN LETTING THE BUS'S OWN STARTUP RECOVERY RUN OVER IT. No frame is
// hand-crafted: a hand-built discard would prove something about this file's
// encoder, and the discards that matter are the ones wal.Open's repair leaves
// behind in a file it has just made startable.

// ob54WALPath is the write-ahead log inside a fixture directory.
func ob54WALPath(dir string) string { return filepath.Join(dir, wal.WALFileName) }

// ob54IsIndexHole classifies one wal.Discard the way the command must: a
// ZERO-LENGTH, non-Severe, non-framing discard is a hole in the index sequence —
// nothing was removed — and anything else took bytes with it.
//
// It is written out here rather than calling outboxBytesWereLost, DELIBERATELY.
// The fixture search below uses this predicate to decide which damage it has
// built, and if it borrowed the production classifier then a mutation of that
// classifier would silently re-label the fixtures and the classification test
// could not go red. The test's idea of the two shapes has to be independent of
// the code's.
func ob54IsIndexHole(d wal.Discard) bool {
	return d.Length == 0 && !d.Severe && d.Stage != "framing"
}

// ob54ProbeWAL replays a fixture's log the way the command does and reports what
// the replay found: the Recovered value, how many PENDING jobs survived into the
// table, and Replay's error.
//
// It exists so a fixture can be CHECKED to be the shape a test needs before that
// test asserts anything — the discipline TestOutboxCommandMintingFixturesReallyMint
// already applies to the mint guard. A fixture that quietly stopped producing
// damage would otherwise leave the assertions passing over nothing.
func ob54ProbeWAL(t *testing.T, dir string) (wal.Recovered, int, error) {
	t.Helper()
	lg := logging.New(io.Discard, logging.LevelError)
	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: ob54BusID, Logger: lg})
	if err != nil {
		t.Fatalf("relay.NewOutbox: %v", err)
	}
	rec, err := wal.Replay(ob54WALPath(dir), ob.Apply)
	if err != nil {
		return rec, 0, err
	}
	return rec, len(ob.Pending()), nil
}

// ob54TryRepair runs the SERVER'S OWN startup recovery over a directory and
// returns rather than failing the test.
//
// Non-fatal on purpose: the fixture search below drives it over candidate damage
// that may be unrepairable, and an unrepairable candidate is a candidate to skip,
// not a broken test.
func ob54TryRepair(dir string) error {
	lg := logging.New(io.Discard, logging.LevelError)
	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: ob54BusID, Logger: lg})
	if err != nil {
		return err
	}
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: ob, Logger: lg})
	if err != nil {
		return err
	}
	return l.Close()
}

// ob54CopyDir copies a fixture data directory, so each candidate in the damage
// search gets a PRISTINE directory rather than one an earlier candidate's repair
// has already rewritten.
func ob54CopyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			ob54CopyDir(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", filepath.Join(src, e.Name()), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			t.Fatalf("writing %s: %v", filepath.Join(dst, e.Name()), err)
		}
	}
}

// ob54DamageWant describes the shape of damage a test needs a fixture to hold.
type ob54DamageWant struct {
	// BytesThrownAway selects between the two classifications: true wants a
	// discard that REMOVED SOMETHING (a non-zero Length, or Severe, or the
	// framing stage, or more discards than wal retained detail for); false wants
	// a log whose ONLY discards are zero-length holes in the index sequence.
	BytesThrownAway bool
	// PendingSurvives says whether a PENDING job must still be in the recovered
	// table afterwards.
	//
	// false is the P0 fixture: the damage swallowed the only pending job, so
	// every count is zero and the pre-fix command answered "DRAINED, exit 0".
	// true is the PRECEDENCE fixture: a pending job is plainly there, so a
	// command that ranked verdict above trust would answer 6, and 6 reads as "the
	// log is fine, just wait" rather than "this file cannot be trusted".
	PendingSurvives bool
}

// ob54DamageStride is how far apart the candidate byte offsets are.
//
// A stride rather than every byte because flipping any byte inside one record
// produces the same class of damage, and 13 is coprime with every frame size in
// the file, so the candidates walk across record boundaries instead of landing
// on the same field of every record.
const ob54DamageStride = 13

// ob54DamagedDir builds a fixture, FLIPS ONE BYTE in the write-ahead log, lets
// the bus's own startup recovery run over it, and returns the first directory
// whose recovered state matches want.
//
// # WHY THE OFFSET IS SEARCHED FOR RATHER THAN HARDCODED
//
// The reviewer's decisive fixture was "1 pending + 3 delivered, damage at offset
// 451". That offset is not a property of the format: a durable outbox record
// carries RFC3339Nano timestamps and generated ids, so the byte at 451 is in a
// different record — or a different FIELD — from one run to the next, and a
// hardcoded offset would make this test assert whatever that run happened to
// produce. The damage is still exactly the reviewer's (ONE flipped byte, then
// the bus's own repair); only the choice of WHICH byte is made by looking.
//
// The search fails LOUDLY when no candidate matches. That is the important half:
// a fixture that stopped producing damage must break the test rather than leave
// it passing over a healthy log.
func ob54DamagedDir(t *testing.T, build ob54Build, want ob54DamageWant) string {
	t.Helper()
	pristine := ob54Dir(t, build)
	original, err := os.ReadFile(ob54WALPath(pristine))
	if err != nil {
		t.Fatalf("reading the pristine %s: %v", wal.WALFileName, err)
	}
	// The file header is not a candidate: damaging it is "this is not a wal at
	// all", which Replay reports as an ERROR rather than as a discard, and that
	// path is already covered by
	// TestOutboxCommandDamagedWALIsExit1AndSaysTheQuestionWasNotAnswered.
	const firstCandidate = 64
	work := t.TempDir()
	tried := 0
	for off := int64(firstCandidate); off < int64(len(original)); off += ob54DamageStride {
		tried++
		dir := filepath.Join(work, fmt.Sprintf("candidate-%d", off))
		ob54CopyDir(t, pristine, dir)
		damaged := append([]byte(nil), original...)
		damaged[off] ^= 0xff
		if err := os.WriteFile(ob54WALPath(dir), damaged, 0o600); err != nil {
			t.Fatalf("writing the damaged %s: %v", wal.WALFileName, err)
		}
		// THE BUS'S OWN STARTUP RECOVERY, not a repair this file invented.
		if err := ob54TryRepair(dir); err != nil {
			continue
		}
		rec, pending, perr := ob54ProbeWAL(t, dir)
		if perr != nil || rec.DiscardCount == 0 {
			// Either the repair left a log that still cannot be read (that is the
			// exit-1-by-error path, covered elsewhere) or it left no trace at all.
			continue
		}
		holes := 0
		for _, d := range rec.Discarded {
			if ob54IsIndexHole(d) {
				holes++
			}
		}
		// "Bytes were thrown away" includes the case where wal retained detail for
		// fewer discards than it counted: what cannot be inspected cannot be
		// called a mere hole.
		bytesGone := holes < len(rec.Discarded) || rec.DiscardCount > len(rec.Discarded)
		if bytesGone != want.BytesThrownAway {
			continue
		}
		if !want.BytesThrownAway && holes == 0 {
			continue
		}
		if (pending > 0) != want.PendingSurvives {
			continue
		}
		t.Logf("damage fixture: flipped one byte at offset %d of %d (candidate %d); after the bus's own repair: discards=%d retained=%d missing=%d holes=%d pending=%d",
			off, len(original), tried, rec.DiscardCount, len(rec.Discarded), rec.MissingRecords, holes, pending)
		return dir
	}
	t.Fatalf("no single-byte flip in %d bytes of %s (%d candidates, stride %d) left the shape this test needs "+
		"(bytes thrown away = %v, a pending job survives = %v) after the bus's own recovery. The fixture no longer builds the damage "+
		"it claims to; fix the fixture rather than the assertion", len(original), wal.WALFileName, tried, ob54DamageStride,
		want.BytesThrownAway, want.PendingSurvives)
	return ""
}

// ob54IntegrityOf pulls the integrity object out of a --json result, failing if
// it is absent or null — which is itself part of the contract.
func ob54IntegrityOf(t *testing.T, obj map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, ok := obj["integrity"]
	if !ok {
		t.Fatalf("the result object has no integrity object: %v", obj)
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("integrity is not an object (it is %T: %v); it must be present on EVERY result, never null", raw, raw)
	}
	return m
}

// ob54Trustworthy reads integrity.trustworthy.
func ob54Trustworthy(t *testing.T, obj map[string]interface{}) bool {
	t.Helper()
	i := ob54IntegrityOf(t, obj)
	v, ok := i["trustworthy"].(bool)
	if !ok {
		t.Fatalf("integrity.trustworthy is missing or not a bool: %v", i)
	}
	return v
}

// ob54IntegrityInt reads one numeric field out of the integrity object.
func ob54IntegrityInt(t *testing.T, obj map[string]interface{}, field string) int {
	t.Helper()
	i := ob54IntegrityOf(t, obj)
	v, ok := i[field].(float64)
	if !ok {
		t.Fatalf("integrity.%s is missing or not a number: %v", field, i)
	}
	return int(v)
}

// ob54DiscardedOf reads integrity.discarded, which must be an array and never
// null.
func ob54DiscardedOf(t *testing.T, obj map[string]interface{}) []interface{} {
	t.Helper()
	i := ob54IntegrityOf(t, obj)
	raw, ok := i["discarded"]
	if !ok {
		t.Fatalf("integrity has no discarded array: %v", i)
	}
	arr, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("integrity.discarded is not an array (it is %T: %v)", raw, raw)
	}
	return arr
}

// TestOutboxCommandDamagedLogIsNeverAQuietDrain IS THE P0 REGRESSION TEST.
//
// The fixture is the reviewer's: a real bus.wal with a PENDING job among
// delivered ones, ONE byte flipped, and then the bus's own startup recovery run
// over it — which is what a real operator's directory looks like after a bad
// sector and a restart. The damage swallows the pending job, so the outbox table
// is EMPTY and every count is zero.
//
// Before the fix that shape printed "VERDICT: DRAINED ... It is safe to start the
// new binary", exited 0 and wrote NOTHING to stderr, because wal.Replay returns
// a nil error for a record-level discard and the reader kept only the error. A
// rollout script branching on that code restarts onto a new binary with a
// message still owed and no trace that it existed.
//
// Both discard classifications are exercised, because the quiet drain is
// reachable through either: bytes thrown away (exit 1) and a hole in the index
// sequence (exit 8). What they must share is that NEITHER IS EXIT 0 and neither
// prints a drain.
func TestOutboxCommandDamagedLogIsNeverAQuietDrain(t *testing.T) {
	// One pending job among three delivered ones — the reviewer's shape. The
	// pending job is the one whose loss the pre-fix command reported as a drain.
	build := ob54All(
		ob54Pending(ob54PeerA, 1),
		ob54Delivered(ob54PeerB, 2),
		ob54Delivered(ob54PeerB, 3),
		ob54Delivered(ob54PeerB, 4),
	)

	for _, tc := range []struct {
		name string
		want ob54DamageWant
		code int
		// stderrNeedle is the log line invariant 6 requires: the discard must be
		// named SPECIFICALLY, not merely counted.
		stderrNeedle string
		// humanNeedle is the headline of the INTEGRITY OF THIS READ block.
		humanNeedle string
	}{
		{
			name:         "the replay threw bytes away",
			want:         ob54DamageWant{BytesThrownAway: true},
			code:         exitOutboxDamaged,
			stderrNeedle: "the write-ahead log DISCARDED a record",
			humanNeedle:  "THE WRITE-AHEAD LOG IS DAMAGED",
		},
		{
			name:         "a record index is absent from the sequence",
			want:         ob54DamageWant{BytesThrownAway: false},
			code:         exitOutboxUnverifiedDrain,
			stderrNeedle: "a record index is ABSENT from the write-ahead log",
			humanNeedle:  "THE DRAIN IS UNVERIFIED",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := ob54DamagedDir(t, build, tc.want)

			code, stdout, stderr := ob54Run(t, "-data-dir", dir)
			if code == exitOutboxOK {
				t.Fatalf("exit = 0 over a log that discarded records. THIS IS THE P0: a quiet instrument must not be able to "+
					"spell itself the same way as a quiet outbox\nstdout: %s\nstderr: %s", stdout, stderr)
			}
			if code != tc.code {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout, stderr)
			}
			if strings.Contains(stdout, "VERDICT: DRAINED") {
				t.Fatalf("the report printed a DRAINED verdict over a log that discarded records:\n%s", stdout)
			}
			if !strings.Contains(stdout, "INTEGRITY OF THIS READ") && !strings.Contains(stdout, tc.humanNeedle) {
				t.Fatalf("the report has no integrity block at all:\n%s", stdout)
			}
			if !strings.Contains(stdout, tc.humanNeedle) {
				t.Fatalf("the integrity block does not carry %q:\n%s", tc.humanNeedle, stdout)
			}
			// THE DISCARD IS NAMED IN THE REPORT, with the stage and the offset an
			// operator would line up against the server's own startup log.
			if !strings.Contains(stdout, "discarded: stage=") {
				t.Fatalf("the integrity block does not detail the discard (stage/index/offset/length):\n%s", stdout)
			}
			// AND ON STDERR. wal.Replay has no logger, so if this command does not
			// emit the line, nothing in the process ever will — which is the silent
			// discard invariant 6 names as the defect.
			if !strings.Contains(stderr, tc.stderrNeedle) {
				t.Fatalf("stderr does not name the discard (%q); wal.Replay takes no logger, so this command is the ONLY thing "+
					"that can report it:\n%s", tc.stderrNeedle, stderr)
			}
			// The integrity block sits ABOVE the verdict: a reader must know the
			// answer is doubtful BEFORE they read the answer.
			if v := strings.Index(stdout, "VERDICT:"); v >= 0 && strings.Index(stdout, tc.humanNeedle) > v {
				t.Fatalf("the integrity warning is printed BELOW the verdict, where it is a footnote on a decision already made:\n%s", stdout)
			}

			jcode, jstdout, _ := ob54Run(t, "-data-dir", dir, "-json")
			if jcode != tc.code {
				t.Fatalf("-json exit = %d, want %d:\n%s", jcode, tc.code, jstdout)
			}
			obj := ob54JSON(t, jstdout)
			if ob54Trustworthy(t, obj) {
				t.Fatalf("integrity.trustworthy is true over a log that discarded records:\n%s", jstdout)
			}
			if got := ob54IntegrityInt(t, obj, "wal_discards"); got == 0 {
				t.Fatalf("integrity.wal_discards = 0 although the replay discarded records:\n%s", jstdout)
			}
			if got := len(ob54DiscardedOf(t, obj)); got == 0 {
				t.Fatalf("integrity.discarded is empty although the replay discarded records:\n%s", jstdout)
			}
			// THE TRAP, STATED: every count is zero. The pending job is gone, so
			// the ONLY thing standing between this report and "it is safe to start
			// the new binary" is the integrity block and the exit code.
			if got := ob54Count(t, obj, "pending"); got != 0 {
				t.Fatalf("counts.pending = %d, but this fixture requires the damage to have swallowed the pending job — "+
					"otherwise the test is not exercising the quiet drain:\n%s", got, jstdout)
			}
			if got, _ := obj["exit_code"].(float64); int(got) != tc.code {
				t.Fatalf("-json exit_code = %v, want %d", obj["exit_code"], tc.code)
			}
		})
	}
}

// TestOutboxCommandDiscardClassificationSplitsOneFromEight is the distinction
// the whole eight-code exists for, and the one most easily got wrong in either
// direction.
//
//   - A discard that REMOVED SOMETHING — a non-zero Length, one wal marked
//     Severe, one at the framing stage, or more discards than wal retained
//     detail for — is exit 1: bytes that were in this log are not in the table
//     and nothing can say whether one of them was a pending job.
//   - A HOLE IN THE INDEX SEQUENCE (Length == 0) is exit 8 and NOT exit 1. wal's
//     own reason text says a hole may be "BURNED BY A RESERVATION A CRASH NEVER
//     USED" — the durable index floor authorises indices in blocks and an
//     authorised index is never authorised again (invariant 1) — and documents
//     MissingRecords as an UPPER BOUND ON LOSS, NOT A COUNT OF IT. Reporting
//     "the log is damaged" on every bus that ever crashed is a channel that
//     cries wolf, which is the mirror image of the silent discard.
//
// The two rows are built from the SAME fixture and the SAME kind of damage — one
// flipped byte, the bus's own repair — so the only thing that differs is which
// byte, which is exactly the discrimination under test.
func TestOutboxCommandDiscardClassificationSplitsOneFromEight(t *testing.T) {
	build := ob54All(
		ob54Pending(ob54PeerA, 1),
		ob54Delivered(ob54PeerB, 2),
		ob54Delivered(ob54PeerB, 3),
		ob54Delivered(ob54PeerB, 4),
	)

	for _, tc := range []struct {
		name string
		want ob54DamageWant
		code int
	}{
		{name: "bytes thrown away is DAMAGED", want: ob54DamageWant{BytesThrownAway: true}, code: exitOutboxDamaged},
		{name: "a pure index-sequence hole is UNVERIFIED, not damaged", want: ob54DamageWant{BytesThrownAway: false}, code: exitOutboxUnverifiedDrain},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := ob54DamagedDir(t, build, tc.want)
			code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
			if code != tc.code {
				t.Fatalf("exit = %d, want %d — the classification is the difference between \"this log is damaged\" and "+
					"\"this log cannot confirm a drain\", and they carry different remedies\nstdout: %s\nstderr: %s",
					code, tc.code, stdout, stderr)
			}
			obj := ob54JSON(t, stdout)
			discarded := ob54DiscardedOf(t, obj)
			gaps := ob54IntegrityInt(t, obj, "index_gaps")

			if tc.want.BytesThrownAway {
				// At least one retained discard really did remove something, or wal
				// retained detail for fewer than it counted.
				removed := false
				for _, d := range discarded {
					m, _ := d.(map[string]interface{})
					if n, _ := m["length"].(float64); n > 0 {
						removed = true
					}
				}
				if !removed && ob54IntegrityInt(t, obj, "wal_discards") <= len(discarded) {
					t.Fatalf("the fixture claims bytes were thrown away, but every retained discard has length 0 and none were "+
						"elided; the fixture is not the shape this row asserts:\n%s", stdout)
				}
				// stderr says it at ERROR, not at WARN: this one IS a loss.
				if !strings.Contains(stderr, "level=error") {
					t.Fatalf("a discard that removed bytes was not logged at ERROR:\n%s", stderr)
				}
				return
			}

			// The hole row. EVERY retained discard is zero-length, and none were
			// elided — otherwise "no bytes were lost" would be a guess.
			if gaps == 0 {
				t.Fatalf("integrity.index_gaps = 0 on the index-hole fixture:\n%s", stdout)
			}
			if got := ob54IntegrityInt(t, obj, "wal_discards"); got != len(discarded) || got != gaps {
				t.Fatalf("wal_discards = %d, index_gaps = %d, len(discarded) = %d: for exit %d every discard must be an "+
					"inspectable zero-length hole\n%s", got, gaps, len(discarded), exitOutboxUnverifiedDrain, stdout)
			}
			for i, d := range discarded {
				m, _ := d.(map[string]interface{})
				if n, _ := m["length"].(float64); n != 0 {
					t.Fatalf("discarded[%d].length = %v on the index-hole fixture; a hole removes nothing:\n%s", i, m["length"], stdout)
				}
			}
			if got := ob54IntegrityInt(t, obj, "missing_records"); got == 0 {
				t.Fatalf("integrity.missing_records = 0 although a record index is absent:\n%s", stdout)
			}
			// AND THE OTHER HALF OF THE HONESTY: the report must not call this
			// damage, and must say the absent index is an upper bound.
			_, human, _ := ob54Run(t, "-data-dir", dir)
			if strings.Contains(human, "THE WRITE-AHEAD LOG IS DAMAGED") {
				t.Fatalf("a hole in the index sequence was reported as DAMAGE. wal documents a hole as an UPPER BOUND on loss "+
					"and often an index a crash burned; claiming damage here makes the channel cry wolf on every bus that has "+
					"ever crashed:\n%s", human)
			}
			if !strings.Contains(human, "UPPER BOUND") {
				t.Fatalf("the report does not say an absent record index is an UPPER BOUND on loss:\n%s", human)
			}
			if !strings.Contains(human, "NO BYTES WERE LOST") {
				t.Fatalf("the report does not state that no bytes were lost, which is what separates 8 from 1:\n%s", human)
			}
		})
	}
}

// TestOutboxCommandTrustOutranksVerdict pins the precedence 1 > 8 > 6 > 7 > 0
// where it actually bites: a fixture that HAS a pending job the reader can see.
//
// A command that ranked the verdict above trust would answer 6 here, and 6 is
// not a milder version of 1 or 8 — it is a DIFFERENT INSTRUCTION. 6 says "the
// log is fine, the old binary just has not finished; start it again and wait",
// and an operator who follows it walks away believing the file is sound. 1 and 8
// say "this file cannot tell you what is owed". The counts and the job list stay
// visible under both, which is why nothing is lost by ranking trust first.
func TestOutboxCommandTrustOutranksVerdict(t *testing.T) {
	// Two pending jobs to different peers, so damage can take one and leave the
	// other plainly in the table.
	build := ob54All(
		ob54Pending(ob54PeerA, 1),
		ob54Pending(ob54PeerB, 2),
		ob54Delivered(ob54PeerB, 3),
		ob54Delivered(ob54PeerA, 4),
	)

	for _, tc := range []struct {
		name string
		want ob54DamageWant
		code int
	}{
		{
			name: "bytes thrown away beats a pending job",
			want: ob54DamageWant{BytesThrownAway: true, PendingSurvives: true},
			code: exitOutboxDamaged,
		},
		{
			name: "an index hole beats a pending job",
			want: ob54DamageWant{BytesThrownAway: false, PendingSurvives: true},
			code: exitOutboxUnverifiedDrain,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := ob54DamagedDir(t, build, tc.want)
			code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
			if code == exitOutboxPending {
				t.Fatalf("exit = %d (PENDING) over an untrustworthy read. Trust outranks verdict: %d says \"the log is fine, "+
					"wait for the old binary\", which is a different instruction from \"this file cannot tell you what is owed\"\n%s",
					code, exitOutboxPending, stdout)
			}
			if code != tc.code {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout, stderr)
			}
			obj := ob54JSON(t, stdout)
			// THE FIXTURE'S OWN CONTROL: a pending job really is visible, so the
			// row would be 6 under any ordering that put the verdict first.
			if got := ob54Count(t, obj, "pending"); got == 0 {
				t.Fatalf("counts.pending = 0, so this row cannot distinguish trust-first from verdict-first ordering:\n%s", stdout)
			}
			if ob54Trustworthy(t, obj) {
				t.Fatalf("integrity.trustworthy is true on a damaged fixture:\n%s", stdout)
			}
			// The report still SHOWS the pending job: suppressing the counts under
			// a trust code would leave the operator with nothing to act on at
			// exactly the moment they need it most.
			_, human, _ := ob54Run(t, "-data-dir", dir)
			if !strings.Contains(human, "VERDICT: NOT DRAINED") {
				t.Fatalf("the human report does not still report the pending job under a trust code:\n%s", human)
			}
		})
	}
}

// TestOutboxCommandRefusedRecordIsNotAQuietDrain is security's fixture, and it
// is the OTHER way this command could go quiet — one that does not involve a
// damaged file at all.
//
// A PENDING record enqueued 25 hours ago is a perfectly well-formed durable
// record. Replaying it today, relay's table REFUSES it as past the retry horizon:
// it logs at ERROR and Apply RETURNS NIL, because an applier that returned an
// error would make a live wal.Open refuse to start. Correct for the server —
// invisible to a reader that only watches the error.
//
// Before the fix, this directory answered: exit 0, ok:true, every count 0,
// "VERDICT: DRAINED — ... It is safe to start the new binary." A record that WAS
// in the log, describing a message this bus accepted, reached no count and no
// list, and the report said the outbox was empty.
func TestOutboxCommandRefusedRecordIsNotAQuietDrain(t *testing.T) {
	dir := t.TempDir()
	if _, err := ids.LoadOrCreateBusID(dir, ob54BusID); err != nil {
		t.Fatalf("ids.LoadOrCreateBusID(%s): %v", dir, err)
	}
	// 25 hours: one hour past relay.OutboxRetryHorizon, DERIVED from the constant
	// rather than typed out, so moving the horizon moves the fixture with it.
	stale := time.Now().Add(-(relay.OutboxRetryHorizon + time.Hour))
	ob54Session(t, dir, func() time.Time { return stale }, ob54Pending(ob54PeerA, 1))

	// THE FIXTURE'S OWN CONTROL, and it is not decoration: this must be a
	// REFUSAL and not damage, or the test would pass for the wrong reason and the
	// refusal-counting could be deleted in silence.
	rec, pending, err := ob54ProbeWAL(t, dir)
	if err != nil {
		t.Fatalf("the fixture's log does not replay (%v); this test needs a HEALTHY log whose record is refused", err)
	}
	if rec.DiscardCount != 0 || rec.MissingRecords != 0 {
		t.Fatalf("the fixture's log discarded %d record(s) and is missing %d: this test must exercise the REFUSAL channel, "+
			"not damage", rec.DiscardCount, rec.MissingRecords)
	}
	if pending != 0 {
		t.Fatalf("the stale record was NOT refused by the table (%d pending); the retry horizon no longer rejects it, so this "+
			"fixture cannot fail — fix the fixture", pending)
	}

	code, stdout, stderr := ob54Run(t, "-data-dir", dir)
	if code == exitOutboxOK {
		t.Fatalf("exit = 0 although a pending record in this log was REFUSED as it was applied. The record described a message "+
			"this bus accepted; it is in no count below\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if code != exitOutboxUnverifiedDrain {
		t.Fatalf("exit = %d, want %d (UNVERIFIED — the log read, but a record did not reach the table)\nstdout: %s\nstderr: %s",
			code, exitOutboxUnverifiedDrain, stdout, stderr)
	}
	if strings.Contains(stdout, "VERDICT: DRAINED") {
		t.Fatalf("the report printed a DRAINED verdict although a record was refused:\n%s", stdout)
	}
	if !strings.Contains(stdout, "THE DRAIN IS UNVERIFIED") {
		t.Fatalf("the report has no unverified-drain warning:\n%s", stdout)
	}
	if !strings.Contains(stdout, "REFUSED as they were applied") {
		t.Fatalf("the integrity block does not say records were REFUSED, which is the only thing distinguishing this run from "+
			"an empty outbox:\n%s", stdout)
	}
	if !strings.Contains(stdout, "outbox records refused 1") {
		t.Fatalf("the integrity block does not count the refusal:\n%s", stdout)
	}
	// It must NOT be reported as damage: nothing was discarded and no byte was
	// lost. The remedies differ, and so must the words.
	if strings.Contains(stdout, "THE WRITE-AHEAD LOG IS DAMAGED") {
		t.Fatalf("a refused record was reported as a DAMAGED log:\n%s", stdout)
	}
	// The refused job is NAMED on stderr — invariant 6's "loudly AND
	// SPECIFICALLY". A count alone would leave the operator unable to say which
	// message it was.
	if !strings.Contains(stderr, "an outbox job was PENDING in the write-ahead log") {
		t.Fatalf("stderr does not name the refused job:\n%s", stderr)
	}
	if !strings.Contains(stderr, ob54PeerA) {
		t.Fatalf("stderr does not name the peer the refused job was owed to:\n%s", stderr)
	}

	jcode, jstdout, _ := ob54Run(t, "-data-dir", dir, "-json")
	if jcode != exitOutboxUnverifiedDrain {
		t.Fatalf("-json exit = %d, want %d:\n%s", jcode, exitOutboxUnverifiedDrain, jstdout)
	}
	obj := ob54JSON(t, jstdout)
	if ob54Trustworthy(t, obj) {
		t.Fatalf("integrity.trustworthy is true although a record was refused:\n%s", jstdout)
	}
	if got := ob54IntegrityInt(t, obj, "outbox_records_refused"); got < 1 {
		t.Fatalf("integrity.outbox_records_refused = %d, want at least 1:\n%s", got, jstdout)
	}
	// The counts really are all zero: that is what made the pre-fix answer look
	// like a clean drain, and it is why the integrity block has to carry the news.
	for _, field := range []string{"retained", "pending", "delivered", "abandoned"} {
		if got := ob54Count(t, obj, field); got != 0 {
			t.Fatalf("counts.%s = %d, want 0 — the refused record reaches no count, which is the whole point:\n%s", field, got, jstdout)
		}
	}
	// And this is NOT the damage channel.
	if got := ob54IntegrityInt(t, obj, "wal_discards"); got != 0 {
		t.Fatalf("integrity.wal_discards = %d on the refusal fixture; a refusal is not a discard:\n%s", got, jstdout)
	}
}

// TestOutboxCommandRefusalOutranksASurvivingPendingJob: the same refusal
// alongside a job that DID reach the table. The answer is 8, not 6 — see
// TestOutboxCommandTrustOutranksVerdict for why those are different
// instructions rather than different severities.
func TestOutboxCommandRefusalOutranksASurvivingPendingJob(t *testing.T) {
	dir := t.TempDir()
	if _, err := ids.LoadOrCreateBusID(dir, ob54BusID); err != nil {
		t.Fatalf("ids.LoadOrCreateBusID(%s): %v", dir, err)
	}
	stale := time.Now().Add(-(relay.OutboxRetryHorizon + time.Hour))
	ob54Session(t, dir, func() time.Time { return stale }, ob54Pending(ob54PeerA, 1))
	// A SECOND session under the real clock appends a job that will survive the
	// replay. It reopens the same directory exactly as a restarted bus would.
	ob54Session(t, dir, nil, ob54Pending(ob54PeerB, 2))

	code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
	if code != exitOutboxUnverifiedDrain {
		t.Fatalf("exit = %d, want %d; a refusal outranks a pending job because it says the table is INCOMPLETE, "+
			"which %d does not\nstdout: %s\nstderr: %s", code, exitOutboxUnverifiedDrain, exitOutboxPending, stdout, stderr)
	}
	obj := ob54JSON(t, stdout)
	if got := ob54Count(t, obj, "pending"); got != 1 {
		t.Fatalf("counts.pending = %d, want the ONE surviving job; without it this row cannot tell trust-first from "+
			"verdict-first:\n%s", got, stdout)
	}
	if got := ob54IntegrityInt(t, obj, "outbox_records_refused"); got != 1 {
		t.Fatalf("integrity.outbox_records_refused = %d, want exactly the one stale record:\n%s", got, stdout)
	}
	// The surviving job is still reported — a trust code suppresses nothing.
	if ids := ob54JobIDs(t, obj); len(ids) != 1 || !strings.HasPrefix(ids[0], ob54PeerB+"|") {
		t.Fatalf("jobs = %v, want the one surviving pending job for %s:\n%s", ids, ob54PeerB, stdout)
	}
}

// TestOutboxCommandCleanReadIsExplicitlyTrustworthy is the control for the whole
// section: on a healthy log every integrity field is zero, trustworthy is TRUE,
// and the report SAYS SO rather than leaving the block out.
//
// Without it, a change that hard-coded trustworthy to false would make every
// test above pass while destroying the command's ability to ever report a drain.
func TestOutboxCommandCleanReadIsExplicitlyTrustworthy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build ob54Build
		code  int
	}{
		{name: "an empty outbox", build: nil, code: exitOutboxOK},
		{name: "a delivered job", build: ob54Delivered(ob54PeerA, 1), code: exitOutboxOK},
		{name: "a pending job", build: ob54Pending(ob54PeerA, 1), code: exitOutboxPending},
		{name: "an abandoned job", build: ob54Abandoned(ob54PeerA, 1), code: exitOutboxAbandoned},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := ob54Dir(t, tc.build)
			code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
			if code != tc.code {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, tc.code, stderr)
			}
			obj := ob54JSON(t, stdout)
			if !ob54Trustworthy(t, obj) {
				t.Fatalf("integrity.trustworthy is false on a healthy log; the command could then never report a drain:\n%s", stdout)
			}
			for _, field := range []string{"wal_discards", "missing_records", "index_gaps", "outbox_records_refused"} {
				if got := ob54IntegrityInt(t, obj, field); got != 0 {
					t.Fatalf("integrity.%s = %d on a healthy log:\n%s", field, got, stdout)
				}
			}
			if got := ob54DiscardedOf(t, obj); len(got) != 0 {
				t.Fatalf("integrity.discarded is not empty on a healthy log: %v", got)
			}
			_, human, _ := ob54Run(t, "-data-dir", dir)
			// The clean case is STATED, not left blank: "no bad news" and "this
			// report does not cover bad news" must not look identical.
			if !strings.Contains(human, "INTEGRITY OF THIS READ: the log replayed with nothing discarded") {
				t.Fatalf("the report does not state that the read was clean:\n%s", human)
			}
		})
	}
}

// TestOutboxCommandJSONAlwaysCarriesIntegrityAndFilter pins the two objects the
// gates required be UNCONDITIONAL, and asserts them on the RAW JSON TEXT.
//
// Unmarshalling into interface{} renders `[]` and `null` identically as a nil
// slice, so a check made after decoding cannot tell them apart — and `null` is
// exactly what a consumer that ranges without a nil check crashes on. The same
// argument applies to the objects themselves: a field that appears only when
// there is bad news is a field nobody writes a branch for, so `integrity` must
// be there on a perfectly clean run too.
func TestOutboxCommandJSONAlwaysCarriesIntegrityAndFilter(t *testing.T) {
	stale := time.Now().Add(-(relay.OutboxRetryHorizon + time.Hour))

	for _, tc := range []struct {
		name  string
		dirOf func(t *testing.T) string
		args  []string
	}{
		{
			name:  "a drained outbox",
			dirOf: func(t *testing.T) string { return ob54Dir(t, nil) },
		},
		{
			name:  "a pending job",
			dirOf: func(t *testing.T) string { return ob54Dir(t, ob54Pending(ob54PeerA, 1)) },
		},
		{
			name: "a damaged log",
			dirOf: func(t *testing.T) string {
				return ob54DamagedDir(t, ob54All(ob54Pending(ob54PeerA, 1), ob54Delivered(ob54PeerB, 2)), ob54DamageWant{BytesThrownAway: true})
			},
		},
		{
			name: "a refused record",
			dirOf: func(t *testing.T) string {
				dir := t.TempDir()
				if _, err := ids.LoadOrCreateBusID(dir, ob54BusID); err != nil {
					t.Fatalf("ids.LoadOrCreateBusID: %v", err)
				}
				ob54Session(t, dir, func() time.Time { return stale }, ob54Pending(ob54PeerA, 1))
				return dir
			},
		},
		{
			name:  "a filtered run",
			dirOf: func(t *testing.T) string { return ob54Dir(t, ob54Pending(ob54PeerA, 1)) },
			args:  []string{"-peer", ob54PeerA, "-state", "pending"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dirOf(t)
			args := append([]string{"-data-dir", dir, "-json"}, tc.args...)
			_, stdout, stderr := ob54Run(t, args...)
			if stdout == "" {
				t.Fatalf("-json printed nothing (stderr: %s)", stderr)
			}
			for _, forbidden := range []string{`"integrity":null`, `"filter":null`, `"discarded":null`, `"states":null`, `"jobs":null`} {
				if strings.Contains(stdout, forbidden) {
					t.Fatalf("the raw JSON contains %s; these are NEVER null, so a consumer can branch and range without a "+
						"nil check:\n%s", forbidden, stdout)
				}
			}
			for _, needle := range []string{`"integrity":{`, `"trustworthy":`, `"wal_discards":`, `"missing_records":`,
				`"index_gaps":`, `"outbox_records_refused":`, `"dangling_prepares":`, `"discarded":[`,
				`"filter":{`, `"peer":`, `"states":[`, `"note":`} {
				if !strings.Contains(stdout, needle) {
					t.Fatalf("the raw JSON is missing %s; integrity and filter are emitted on EVERY result:\n%s", needle, stdout)
				}
			}
			// The filter echo describes the filter that actually ran, so the
			// machine audience can see that jobs is filtered while counts and
			// exit_code are not.
			obj := ob54JSON(t, stdout)
			filter, ok := obj["filter"].(map[string]interface{})
			if !ok {
				t.Fatalf("filter is not an object: %v", obj["filter"])
			}
			wantPeer := ""
			wantStates := []string{}
			for i := 0; i < len(tc.args); i += 2 {
				switch tc.args[i] {
				case "-peer":
					wantPeer = tc.args[i+1]
				case "-state":
					wantStates = append(wantStates, tc.args[i+1])
				}
			}
			if got, _ := filter["peer"].(string); got != wantPeer {
				t.Fatalf("filter.peer = %q, want %q", got, wantPeer)
			}
			gotStates := []string{}
			for _, s := range filter["states"].([]interface{}) {
				gotStates = append(gotStates, fmt.Sprint(s))
			}
			if !reflect.DeepEqual(gotStates, wantStates) {
				t.Fatalf("filter.states = %v, want %v", gotStates, wantStates)
			}
			if note, _ := filter["note"].(string); !strings.Contains(note, "jobs is filtered") || !strings.Contains(note, "exit_code") {
				t.Fatalf("filter.note does not say that jobs is filtered while the counts and exit_code are not: %q", note)
			}
		})
	}
}

// ob54ManyDiscardJobs is how many delivered jobs the cap fixture writes. It has
// to be enough that scattering damage across the file produces MORE discards
// than wal retains detail for (maxDiscardsRetained, 64 at the time of writing —
// unexported, so this test never names the number and asserts the RELATION
// instead).
const ob54ManyDiscardJobs = 60

// TestOutboxCommandDiscardDetailIsCappedButTheCountIsExact is the rule a
// consumer would otherwise get wrong in the most damaging direction.
//
// wal retains at most maxDiscardsRetained Discard entries so that a file which
// is damage from end to end cannot make recovery hold it all in memory, while
// DiscardCount stays EXACT. So `len(discarded)` is not the number of discards,
// and a script that reads it as one under-reports the loss precisely on the runs
// where the loss is worst.
//
// The command's answer to that is twofold and both halves are asserted here:
// wal_discards carries the exact count beside the capped list, and a read whose
// discards it cannot even INSPECT is reported as DAMAGED (exit 1) rather than as
// a tidy set of holes — "I cannot see what I discarded" is not the signature of
// a clean crash.
func TestOutboxCommandDiscardDetailIsCappedButTheCountIsExact(t *testing.T) {
	builds := make([]ob54Build, 0, ob54ManyDiscardJobs+1)
	for i := uint64(1); i <= ob54ManyDiscardJobs; i++ {
		builds = append(builds, ob54Delivered(ob54PeerB, i))
	}
	builds = append(builds, ob54Pending(ob54PeerA, 9999))
	dir := ob54Dir(t, ob54All(builds...))

	// Damage SCATTERED across the whole file rather than one flipped byte: the
	// point of this fixture is a log with more losses than wal will detail, which
	// one bad byte cannot produce. The file header (the first 48 bytes) is left
	// alone so the file is still identifiable as a wal — otherwise this would be
	// the exit-1-by-error path, which is a different test.
	walPath := ob54WALPath(dir)
	raw, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("reading %s: %v", walPath, err)
	}
	for off := 64; off < len(raw); off += 200 {
		raw[off] ^= 0xff
	}
	if err := os.WriteFile(walPath, raw, 0o600); err != nil {
		t.Fatalf("writing the damaged %s: %v", walPath, err)
	}
	// The bus's own startup recovery, exactly as in every other damage fixture.
	if err := ob54TryRepair(dir); err != nil {
		t.Fatalf("the bus's own recovery could not open the damaged fixture (%v); this test needs a log that STARTS and still "+
			"reports its losses", err)
	}

	// THE FIXTURE'S CONTROL: it must really have more discards than wal retains
	// detail for, or the capping assertion below asserts nothing.
	rec, _, perr := ob54ProbeWAL(t, dir)
	if perr != nil {
		t.Fatalf("the repaired fixture does not replay (%v); this test needs a readable log with elided discards", perr)
	}
	if rec.DiscardCount <= len(rec.Discarded) {
		t.Fatalf("the fixture produced %d discards and wal retained detail for %d of them: nothing was elided, so this test "+
			"cannot prove the cap. Scatter more damage", rec.DiscardCount, len(rec.Discarded))
	}

	code, stdout, stderr := ob54Run(t, "-data-dir", dir, "-json")
	if code != exitOutboxDamaged {
		t.Fatalf("exit = %d, want %d: a read that cannot INSPECT what it discarded must be reported as damaged rather than as "+
			"a set of holes\nstdout: %s\nstderr: %s", code, exitOutboxDamaged, stdout, stderr)
	}
	obj := ob54JSON(t, stdout)
	exact := ob54IntegrityInt(t, obj, "wal_discards")
	detailed := len(ob54DiscardedOf(t, obj))
	if exact <= detailed {
		t.Fatalf("integrity.wal_discards = %d and len(integrity.discarded) = %d: the exact count must survive the cap, or a "+
			"consumer reading len(discarded) would under-report the loss on the worst runs:\n%s", exact, detailed, stdout)
	}
	if exact != rec.DiscardCount {
		t.Fatalf("integrity.wal_discards = %d but the replay counted %d: wal_discards is the EXACT count", exact, rec.DiscardCount)
	}
	if ob54Trustworthy(t, obj) {
		t.Fatalf("integrity.trustworthy is true on a log with %d discards:\n%s", exact, stdout)
	}

	// The human report says the list is short AND by how much, so an operator
	// reading the detail knows they are not reading all of it.
	_, human, _ := ob54Run(t, "-data-dir", dir)
	needle := fmt.Sprintf("ONLY %d OF THE %d DISCARDS ARE DETAILED BELOW", detailed, exact)
	if !strings.Contains(human, needle) {
		t.Fatalf("the report does not warn that the discard list is capped (%q):\n%s", needle, human)
	}
}

// ob54EvilReason is security's demonstration, verbatim in shape: a stored
// abandonment reason that CLEARS THE SCREEN, homes the cursor and paints a fake
// verdict.
//
// A reason survives sanitiseOutboxReason and OutboxRecord.validate if it is valid
// UTF-8 and within relay.MaxOutboxReasonLen — CONTROL CHARACTERS ARE NOT
// STRIPPED. The realistic source is not a live peer but an ATTACKER-AUTHORED
// DATA DIRECTORY: a copied, restored or forensic one, which is exactly what an
// offline tool is pointed at, and which authenticates itself because wal-mac.key
// sits beside bus.wal.
const ob54EvilReason = "peer refused\x1b[2J\x1b[H\rVERDICT: DRAINED — nothing pending. It is safe to start the new binary.\n\n"

// TestOutboxCommandStoredReasonCannotRepaintTheTerminal.
//
// This is the one command whose entire product is a restart decision, so a
// reason that can paint "VERDICT: DRAINED" over the real verdict is not a
// cosmetic problem: it is the whole answer, forged, from data.
//
// The defence is strconv.Quote — the house answer, the same one
// internal/logging.writeValue calls the log-injection defence. --json was
// already safe because encoding/json escapes control characters; this is the
// human path.
func TestOutboxCommandStoredReasonCannotRepaintTheTerminal(t *testing.T) {
	dir := ob54Dir(t, ob54All(
		ob54Pending(ob54PeerA, 1),
		func(t *testing.T, ob *relay.Outbox) {
			rec := ob54Enqueue(t, ob, ob54PeerB, 2)
			ob54Settle(t, ob, rec.JobID, relay.OutboxAbandoned, ob54EvilReason)
		},
	))

	code, stdout, stderr := ob54Run(t, "-data-dir", dir)
	if code != exitOutboxPending {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOutboxPending, stderr)
	}

	// 1. NO RAW ESCAPE, and no bare carriage return, reaches the terminal.
	for _, b := range []struct {
		name string
		ch   byte
	}{{"ESC (0x1b)", 0x1b}, {"CR (0x0d)", '\r'}} {
		if strings.IndexByte(stdout, b.ch) >= 0 {
			t.Fatalf("a raw %s from a STORED abandonment reason reached the report. It can clear the screen and repaint the "+
				"verdict of the one command whose product is a restart decision:\n%q", b.name, stdout)
		}
	}

	// 2. The reason IS still shown — escaped. A defence that dropped the text
	// would be a silent discard of the only explanation a lost message has.
	if !strings.Contains(stdout, `\x1b[2J`) {
		t.Fatalf("the escaped form of the control sequence is not in the report, so the reason was either printed raw or "+
			"thrown away:\n%s", stdout)
	}
	if !strings.Contains(stdout, "peer refused") {
		t.Fatalf("the report does not carry the reason text at all:\n%s", stdout)
	}

	// 3. NO LINE CAN BE READ AS A VERDICT except the real one. The forged text
	// survives as escaped bytes INSIDE the REASON line, which is why the check is
	// per line rather than a substring search over the whole report.
	verdicts := []string{}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "VERDICT:") {
			verdicts = append(verdicts, line)
		}
	}
	if len(verdicts) != 1 {
		t.Fatalf("the report has %d verdict lines, want exactly 1: %q\n%s", len(verdicts), verdicts, stdout)
	}
	if !strings.Contains(verdicts[0], "NOT DRAINED") {
		t.Fatalf("the single verdict line is %q, but the fixture has a PENDING job; a forged verdict has displaced the real one:\n%s",
			verdicts[0], stdout)
	}

	// 4. --json escapes it too, and the decoded value is still the original
	// bytes — the escaping is a rendering, not a mutation of the record.
	jcode, jstdout, _ := ob54Run(t, "-data-dir", dir, "-json")
	if jcode != exitOutboxPending {
		t.Fatalf("-json exit = %d, want %d", jcode, exitOutboxPending)
	}
	if strings.IndexByte(jstdout, 0x1b) >= 0 {
		t.Fatalf("a raw ESC reached the --json output:\n%q", jstdout)
	}
	if !strings.Contains(jstdout, `\u001b`) {
		t.Fatalf("--json does not carry the escaped control character, so the reason did not survive to the machine audience:\n%s", jstdout)
	}
	obj := ob54JSON(t, jstdout)
	found := false
	for _, j := range obj["jobs"].([]interface{}) {
		m, _ := j.(map[string]interface{})
		if r, _ := m["reason"].(string); r == ob54EvilReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("no job carries the stored reason byte for byte; --json must render it, not rewrite it:\n%s", jstdout)
	}

	// 5. The DATA DIRECTORY is quoted in the header for the same reason: it is
	// argv, and an unquoted path holding an ESC repaints the report below it.
	if !strings.Contains(stdout, strconv.Quote(dir)) {
		t.Fatalf("the report header does not print the data directory QUOTED:\n%s", stdout)
	}
}
