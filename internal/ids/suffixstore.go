package ids

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// suffixFileName is the file within the data dir holding the per-name agent-id
// suffix high-water marks. It sits alongside bus-id (see busid.go): both are
// small, atomically-replaced identity files, and both are part of the data dir
// in the sense that losing one loses the bus's identity continuity.
const suffixFileName = "agent-suffixes"

// suffixFileMagic is the first token of the header line. It is spelled out in
// full so a stray file is identifiable by `head -1` alone.
const suffixFileMagic = "agent-bus-agent-suffixes"

// suffixFileVersion is the on-disk format version of the agent-suffixes file.
//
// It is RESERVED, not chosen: value 3 in the Spec Server `ondisk-format-version`
// namespace, reserved 2026-08-07 by feature-runner for MSG-FU-SUFFIXFLOOR
// (values 1 and 2 are the WAL's). Never pick one of these by eyeballing the
// list — that is the parallel-agent collision class CLAUDE.md names explicitly.
//
// An UNKNOWN version is a hard error, never a "read what you can". A file
// written by a newer binary may encode floors this one cannot see, and reading
// it partially would lower a floor — which is the one thing this whole file
// exists to make impossible.
const suffixFileVersion = 3

// suffixBlockSize is how many suffixes NextSuffix reserves for a name in one
// durable write. Numbers within a reserved block are issued from memory with no
// I/O at all; the next write happens only when the block is used up.
//
// It is a TUNING CONSTANT, not a reserved number: it does not appear on disk, it
// is not part of the file format, and changing it needs no version bump. A file
// written by a binary with a different block size is read identically, because
// what is stored is a floor and the reader only ever needs "at least this high".
//
// The trade it sets:
//
//   - larger — fewer fsyncs per enrolment (one per suffixBlockSize), more
//     unissued numbers skipped when a process dies mid-block;
//   - smaller — the reverse.
//
// 64 is chosen as the smallest value that makes the write cost plainly
// non-quadratic in enrol/leave churn while keeping the per-restart skip small
// enough to stay legible to an operator reading agent ids. Both directions are
// SAFE for invariant 1 — the durable floor is >= every issued suffix either way
// — so this is a cost knob, not a correctness one. Setting it to 1 restores the
// pre-amortisation behaviour exactly.
const suffixBlockSize = 64

// ErrSuffixFileCorrupt is returned by OpenNameSuffixes when the agent-suffixes
// file exists but does not verify: bad header, unknown version, checksum
// mismatch, malformed or duplicated entry.
//
// It is a FATAL condition and is deliberately NOT recoverable by regenerating
// the file — the same posture LoadOrCreateBusID takes to a corrupt persisted bus
// id, and for the same reason. Regenerating means resuming every name from 1,
// which re-mints agent ids that are already on disk (invariant 1). A bus that
// cannot prove its floors must REFUSE TO START; a loud, recoverable outage beats
// silent identity reuse.
var ErrSuffixFileCorrupt = errors.New("ids: the persisted agent-id suffix floors are corrupt")

// DurableNameSuffixes is a per-name agent-id suffix allocator whose floors
// SURVIVE RESTART. It is the SuffixAllocator an AgentIDMinter should be built
// on in production, and it is the answer to the question NameSuffixes' doc
// leaves open: where does the resume floor come from?
//
// # The property
//
// For every name, every suffix this allocator issues is strictly greater than
// every suffix it has EVER issued for that name, in this process or in any
// previous one that shared the data dir. Enrol "alpha", restart the bus, enrol
// "alpha" again, and the second id is strictly greater than the first — never
// equal. That is invariant 1 ("ids are never reused, INCLUDING ACROSS RESTARTS")
// applied to the routing and authorization subject.
//
// # Where the floor comes from, and why it is NOT derived from history
//
// This is the load-bearing design decision, so it is stated plainly.
//
// The obvious way to resume a counter is to derive it from what is already on
// disk — replay the log, find the highest suffix, resume above it. ID-2-WIRING's
// analysis (ID2_WIRING_DEEPDIVE.md) showed that derivation is WRONG when it is
// the only source, twice over:
//
//   - COMMITTED history is not enough. A suffix reserved by a PREPARE that never
//     committed is still burned: the number reached disk, the client may have
//     been told it, and no committed record mentions it. Folding the committed
//     stream misses it exactly.
//   - The ROSTER is not enough either. It holds the agents currently enrolled,
//     so every departed agent's burned suffix is invisible to it (point 5 of the
//     NameSuffixes doc).
//
// So this type does not derive the floor from history at all. It WRITES THE
// FLOOR AHEAD OF THE SUFFIX IT AUTHORISES: NextSuffix persists and fsyncs
// floor[name] = n BEFORE it returns n to anyone. A number that was returned was
// necessarily durable first, so on the next start it is already below the floor.
// A crash between the fsync and the return burns n without issuing it, which
// point 4 of the NameSuffixes doc already declares correct — gaps are expected
// and must not be compacted.
//
// This ordering is the same shape as invariant 4's ("nothing is acknowledged
// before it is durable"), one layer down: nothing is ISSUED before its floor is
// durable.
//
// # Why it CANNOT be rewound by log repair
//
// The floors live in their own atomically-replaced file, not in the WAL. That
// matters because wal.RepairTail may discard a verified-corrupt tail, and a
// floor derived from the log would drop with it. CLAUDE.md's invariant 1 states
// the rule for the neighbouring counter — "when recovery discards a record, the
// sequence advances past the hole; it never rewinds" — and this file makes it
// structural rather than a thing the recovery path must remember to do: no
// amount of WAL damage can lower a floor that was never stored in the WAL.
//
// The write is a temp-file-plus-rename, fsynced before the rename and with the
// directory fsynced after it, so the file is never torn: a reader sees either
// the complete previous map or the complete new one. There is no partial state
// to repair, and therefore no tail-repair rule that could lower a floor.
// Media damage that mutates the bytes in place is caught by the SHA-256 over the
// body and is a fatal ErrSuffixFileCorrupt, not a silently lower floor.
//
// # Assembly, then seal
//
// An allocator from OpenNameSuffixes is born UNSEALED, exactly like
// ResumeNameSuffixes, and refuses NextSuffix until Seal is called. That window
// exists for one reason that matters in practice: BACKFILL.
//
// A data dir that predates this file has agent ids on disk — inside WAL message
// bodies, as senders and recipients — and no agent-suffixes file to resume from.
// Opening it yields an EMPTY floor map, and issuing from that would mint straight
// over ids that already exist. So the caller must fold whatever it can derive
// from replay in through RaiseFloor before sealing, and the floors in force are
// the MAXIMUM of the file and the derivation. Seal persists that merged map
// before it lets anything issue.
//
// A caller whose derivation FAILS must simply not call Seal: every NextSuffix
// then refuses with an error satisfying errors.Is(err, ErrFloorUnproven), which
// is a startup failure rather than silent identity reuse. Falling back to
// NewNameSuffixes at that point is the one thing a caller must never do.
//
// # The residual, stated honestly
//
// On a data dir that ALREADY has agent ids on disk and no agent-suffixes file,
// this type is only as good as the floors the caller derives and hands to
// RaiseFloor — and per ID-2-WIRING that derivation cannot today see a suffix
// burned by a dangling prepare, because wal.Replay hands the application
// committed entries only and the prepare observer (ID-2-WIRING-OBSERVER) is not
// implemented. So for a LEGACY dir the guarantee is "no suffix that reached
// COMMITTED history is reissued", not the full "no suffix that reached disk".
//
// For a dir that has been served by this type from the start there is no
// derivation and therefore no gap: the floor is written ahead of every suffix,
// so the strong property holds unconditionally. The residual is a MIGRATION
// window, not a permanent limit.
//
// # Cost, and the block reservation that bounds it
//
// The whole floor map is rewritten on every write, which is O(distinct names
// EVER seen) — where the WAL, being append-only, is O(record). And the map is
// monotonic by design: a name is never forgotten, INCLUDING after the agent
// leaves (point 5 of the NameSuffixes doc), because forgetting it is the reset
// that reissues its ids.
//
// It is therefore WRONG to say, as an earlier draft of this comment did, that
// bounding this is admission control's job "exactly as it is for the in-memory
// map". The in-memory map costs O(1) per NEW name; the file used to cost O(N)
// per EVERY issued suffix, re-enrolments included. While the roster cap is a
// hard lifetime cap on distinct names, N is bounded and the total is small. The
// day a leave/revocation path frees roster slots, enrol-leave churn grows N
// without bound while every enrolment rewrites all of it — cumulative I/O
// quadratic in the churn, from an unauthenticated request.
//
// So NextSuffix no longer writes per issued suffix. It reserves a BLOCK of
// suffixBlockSize numbers for a name in one write and then issues them from
// memory, writing again only when the block runs out. The file therefore holds a
// RESERVED HIGH-WATER, not the highest suffix issued, and one write covers
// suffixBlockSize enrolments of that name.
//
// # This does NOT weaken the write-ahead property — read this before changing it
//
// The property is unchanged and it is the whole point of the type: NO SUFFIX IS
// EVER RETURNED BEFORE A FLOOR AT LEAST THAT HIGH IS DURABLE. Blocking makes the
// durable floor HIGHER than the issued one, never lower:
//
//	reserve:  persist and fsync floor[name] = n + suffixBlockSize - 1
//	issue:    hand out n, n+1, … up to that floor, from memory, no writes
//
// Every number handed out is <= a floor that was fsynced BEFORE the first of
// them was returned. A crash at any instant therefore leaves disk >= every
// suffix any client was ever told, so the next start resumes strictly above all
// of them. A floor that is too HIGH is safe — it only skips numbers, and the
// gaps that leaves are already declared correct by point 4 of the NameSuffixes
// doc, which is why the amortisation is available at all. A floor that is too
// LOW is id reuse. Any future change here must preserve the direction of that
// inequality, not merely the fact that a write happens.
//
// What is given up, precisely: up to suffixBlockSize-1 unissued numbers per name
// per process lifetime, because a restart resumes above the whole reserved block
// rather than above the last issued suffix. That is a cosmetic jump in agent-id
// suffixes across restarts and nothing more; at 64 per restart, exhausting a
// uint64 name would take ~2.8e17 restarts.
//
// The residual, stated honestly: this amortises REPEAT use of a name by a factor
// of suffixBlockSize. It does not bound the growth of the map itself — a churn
// pattern that invents an ENDLESS SUPPLY OF NEW NAMES still writes once per new
// name over a map that never shrinks, so the cumulative cost is still quadratic
// in the number of DISTINCT names. Bounding that is admission control's job (the
// roster cap), and this type cannot do it: it may not forget a name.
//
// # Single writer per data dir
//
// This type assumes it is the only writer of its file: two processes sharing a
// data dir would each rewrite the whole map from its own view and the last
// rename would win, silently lowering floors. That assumption is not enforced
// here — it is enforced one layer up, by the data-dir lock the server takes at
// startup. Nothing in this package may be used against a dir another process
// holds.
//
// The zero value is not usable; construct with OpenNameSuffixes. It is safe for
// concurrent use WITHIN one process.
type DurableNameSuffixes struct {
	// mu serializes the whole allocate-persist-commit sequence. It must be held
	// across the fsync: two goroutines that both peeked the same n and then
	// raced to persist would issue n twice, which is the entire bug this type
	// exists to prevent. Holding a mutex across an fsync is deliberate here —
	// see "Cost" above for why that is affordable.
	mu sync.Mutex

	dir  string
	path string

	// mem is the in-memory counter. It is created by ResumeNameSuffixes from
	// the loaded floors and is therefore born unsealed; it owns the seal gate,
	// the exhaustion check and the per-name arithmetic, so none of that is
	// reimplemented here.
	mem *NameSuffixes

	// durable mirrors what the file on disk says. It is updated only after a
	// successful atomic write, so it never claims more than disk does.
	durable map[string]uint64

	// existed records whether the file was present at open. A caller can use it
	// to distinguish a genuinely fresh data dir from one whose floors file has
	// gone missing — see Existed.
	existed bool
}

// OpenNameSuffixes loads the persisted per-name suffix floors from dir and
// returns an UNSEALED allocator resuming strictly above them.
//
// A missing file means "this data dir has never issued an agent id through this
// store" and yields an empty floor map — see Existed for why the caller should
// not treat that as proof the dir is fresh. A file that exists but does not
// verify is a FATAL error wrapping ErrSuffixFileCorrupt and is never
// regenerated: see that sentinel's doc.
//
// Nothing is written here. The file is created on the first Seal.
//
// The returned allocator refuses NextSuffix until Seal is called. Callers that
// have floors to merge in — from a replay derivation, on a data dir that
// predates this file — must RaiseFloor them all first.
func OpenNameSuffixes(dir string) (*DurableNameSuffixes, error) {
	if dir == "" {
		return nil, errors.New("ids: opening agent-id suffix floors: data dir must not be empty")
	}
	path := filepath.Join(dir, suffixFileName)

	floors, existed, err := readSuffixFile(path)
	if err != nil {
		return nil, err
	}

	return &DurableNameSuffixes{
		dir:     dir,
		path:    path,
		mem:     ResumeNameSuffixes(floors),
		durable: floors,
		existed: existed,
	}, nil
}

// Path reports the file the floors are persisted to. Useful in operator
// messages and in tests; it is not a hook for writing to that file.
func (d *DurableNameSuffixes) Path() string { return d.path }

// Existed reports whether the floors file was present when this allocator was
// opened.
//
// It is false for a genuinely fresh data dir AND for a dir whose floors file has
// been deleted, restored from an older backup, or never written because the bus
// predates this store. This type cannot tell those apart — the information is
// simply not there — so the CALLER must: a data dir that already holds a bus-id,
// a WAL, or any agent id, but no floors file, is a BACKFILL case and its floors
// must be derived and raised before Seal, not assumed empty.
func (d *DurableNameSuffixes) Existed() bool { return d.existed }

// Floors returns a copy of the floors currently held on DISK. It is a snapshot
// for logging and tests; mutating it does not affect the allocator.
//
// Since the block reservation landed this is the RESERVED HIGH-WATER, not the
// highest suffix issued: after one NextSuffix for a fresh name it reads
// suffixBlockSize, not 1. That is the honest answer to "what does disk say", and
// disk deliberately says something higher than what has been handed out — see
// the block-reservation section on the type. For "what did I hand out", use
// LastSuffix.
func (d *DurableNameSuffixes) Floors() map[string]uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]uint64, len(d.durable))
	for k, v := range d.durable {
		out[k] = v
	}
	return out
}

// RaiseFloor merges an externally-derived floor for name, so that every
// subsequent suffix for that name is strictly greater than atLeast. It never
// lowers a floor, and it is legal only before Seal (afterwards it returns an
// error wrapping ErrFloorSealed, unchanged from NameSuffixes.RaiseFloor).
//
// It does NOT write to disk. Nothing needs to: the merged map is persisted by
// Seal before anything may issue, and until then no suffix has been handed out
// that a crash could strand. Persisting each raise separately would multiply the
// fsyncs of a startup fold by the number of records replayed for no benefit.
//
// Unlike NameSuffixes.RaiseFloor, name IS validated here — see "Names are
// validated at this boundary" on the type.
//
// A peer-supplied claim is UNTRUSTED INPUT and its VALUE is not bounded here —
// see the same warning on NameSuffixes.RaiseFloor. Validate and bound the value
// before it gets this far.
func (d *DurableNameSuffixes) RaiseFloor(name string, atLeast uint64) error {
	if err := ValidateAgentName(name); err != nil {
		return fmt.Errorf("raising the persisted agent id suffix floor: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mem.RaiseFloor(name, atLeast)
}

// Seal persists the merged floors and ends assembly: after it, NextSuffix may
// issue and RaiseFloor may not.
//
// The ORDER is the point. The file is written and fsynced FIRST, and only if
// that succeeds is the in-memory allocator sealed. A Seal whose write fails
// leaves the allocator UNSEALED, so every NextSuffix keeps refusing with
// ErrFloorUnproven and the bus fails to start rather than issuing from floors
// that are not on disk.
//
// Sealing twice returns an error wrapping ErrFloorSealed and writes nothing —
// the floors are claimed by exactly one startup path (see NameSuffixes.Seal).
func (d *DurableNameSuffixes) Seal() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Ask the in-memory allocator for the merged floors BEFORE writing, but seal
	// it AFTER: a failed write must leave the allocator refusing to issue.
	merged := d.mem.floorSnapshot()

	if d.mem.isSealed() {
		// Delegate so there is exactly one wording for a double seal, and so the
		// error is produced without having written anything.
		return d.mem.Seal()
	}

	if err := d.writeFloors(merged); err != nil {
		return fmt.Errorf("sealing agent-id suffix floors: %w", err)
	}
	if err := d.mem.Seal(); err != nil {
		return err
	}
	d.durable = merged
	return nil
}

// NextSuffix allocates the next suffix for name, NEVER RETURNING ONE THAT IS NOT
// ALREADY COVERED BY A DURABLE FLOOR.
//
// The order is the whole contract of this type:
//
//	n := <the suffix the in-memory counter would issue next>   (nothing mutated)
//	if n > <the floor on disk for name>:
//	        persist and fsync floor[name] = n + suffixBlockSize - 1
//	                                                           (a BLOCK is burned)
//	commit n in memory
//	return n
//
// The write is CONDITIONAL, and that is the only thing that changed when the
// block reservation landed. The invariant it preserves is unchanged and
// absolute: at the moment n is returned, a floor >= n has already been fsynced.
// When n is inside a block reserved by an earlier call, that fsync happened
// earlier — which is still "before", and is the entire saving.
//
// A failure at the persist step returns (0, err) and issues nothing: n has not
// been handed out, so a later attempt legitimately yields n again. A crash at
// any point leaves the unissued remainder of the reserved block burned, which
// leaves a gap — and gaps are correct (point 4 of the NameSuffixes doc), never
// something to compact.
//
// An UNSEALED allocator issues nothing and writes nothing: it returns an error
// satisfying errors.Is(err, ErrFloorUnproven) before touching the disk.
// Likewise an exhausted name returns ErrSuffixExhausted without a write.
func (d *DurableNameSuffixes) NextSuffix(name string) (uint64, error) {
	if err := ValidateAgentName(name); err != nil {
		return 0, fmt.Errorf("allocating agent id suffix: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// peekNext runs the seal and exhaustion gates and mutates nothing, so a
	// refusal here never costs a write and never burns a number.
	n, err := d.mem.peekNext(name)
	if err != nil {
		return 0, err
	}

	// The reserved block already covers n: it was fsynced by an earlier call, so
	// n may be issued from memory with no I/O. This is the amortisation, and the
	// comparison is the ONLY thing standing between it and id reuse — it must
	// stay ">", so that n == the durable floor still counts as covered and
	// n == floor+1 does not.
	if n > d.durable[name] {
		// Reserve a fresh block. The high-water saturates rather than wrapping:
		// wrapping would set the floor BELOW the suffix being issued, which is
		// precisely the reuse this type exists to prevent. Saturating leaves the
		// floor at the maximum, which is safe — it simply means every remaining
		// suffix for this name is already covered and needs no further write,
		// until NameSuffixes' own exhaustion check refuses at MaxUint64.
		high := n + suffixBlockSize - 1
		if high < n {
			high = math.MaxUint64
		}

		prev, had := d.durable[name]
		d.durable[name] = high
		if err := d.writeFloors(d.durable); err != nil {
			// Roll the mirror back to what disk is KNOWN to hold.
			//
			// Be precise about why this is safe, because the tempting
			// justification is false. It is NOT true that "the file never goes
			// backwards": a write that in fact landed leaves disk at high while
			// the mirror says prev, and the next write — for ANY name, since the
			// whole map is written — then rewrites this name's entry back down
			// to prev.
			//
			// The property that actually holds, and the one the restart
			// guarantee rests on, is weaker but sufficient:
			//
			//	every map ever persisted is >= every suffix ever ISSUED
			//
			// It holds because this branch is reached only when prev < n, and n
			// is the number the counter has NOT yet issued — so prev is exactly
			// the highest suffix issued for this name, and rolling back to it
			// still covers every id that exists. Nothing was returned to anyone
			// here, so no id bears any number in the abandoned block; those
			// numbers are simply un-burned and will be handed out later.
			if had {
				d.durable[name] = prev
			} else {
				delete(d.durable, name)
			}
			return 0, fmt.Errorf("allocating agent id suffix for %q: %w", name, err)
		}
	}

	got, err := d.mem.NextSuffix(name)
	if err != nil {
		// Unreachable: peekNext ran the same gates under the same lock. Reported
		// rather than ignored because the number is already burned on disk.
		return 0, fmt.Errorf("allocating agent id suffix for %q after its floor was persisted: %w", name, err)
	}
	if got != n {
		return 0, fmt.Errorf("ids: internal error allocating agent id suffix for %q: persisted floor %d but the counter issued %d; the persisted floor no longer bounds the issued suffix", name, n, got)
	}
	if got > d.durable[name] {
		// Belt to the braces above: the returned suffix must never exceed the
		// floor on disk. If this ever fires the amortisation has broken invariant
		// 1, so it refuses rather than returning the number.
		return 0, fmt.Errorf("ids: internal error allocating agent id suffix for %q: issued %d but the durable floor is only %d; refusing to return a suffix that is not covered by a persisted floor", name, got, d.durable[name])
	}
	return n, nil
}

// LastSuffix reports the highest suffix this allocator has ISSUED for name in
// THIS process, or 0 if it has issued none — the same "what did I hand out"
// question NameSuffixes.LastSuffix answers. It is deliberately not "what is
// burned on disk": for that, read Floors.
func (d *DurableNameSuffixes) LastSuffix(name string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mem.LastSuffix(name)
}

// writeFloors encodes floors and replaces the file atomically. The caller must
// hold d.mu.
func (d *DurableNameSuffixes) writeFloors(floors map[string]uint64) error {
	data, err := encodeSuffixFile(floors)
	if err != nil {
		return err
	}
	return atomicWriteFile(d.dir, d.path, ".agent-suffixes-*", data)
}

// encodeSuffixFile renders floors in the canonical on-disk form:
//
//	agent-bus-agent-suffixes v3 sha256=<hex of the body>
//	<name> <suffix>
//	...
//
// Entries are sorted by name so the bytes are a function of the map alone —
// which is what makes the checksum meaningful and the file diffable. A floor of
// 0 is omitted: absent and zero mean the same thing (point 7 of the NameSuffixes
// doc), and writing it would be a second spelling of one state.
//
// Names need no quoting BECAUSE every name written is validated here first:
// ValidateAgentName admits only [a-z0-9_-], so a space separator is unambiguous.
// That validation is not decoration and not merely a repeat of the caller's. A
// name carrying a space would produce a file readSuffixFile rejects, and a name
// carrying a newline would forge an entry for another name — and because a
// rejected floors file is NEVER regenerated (ErrSuffixFileCorrupt), either one
// would brick the data dir permanently, with no way back that does not resume
// every name from 1. This is the last point before an irreversible write, so it
// refuses rather than writes. DurableNameSuffixes validates at its own door too;
// this is the belt to that pair of braces.
func encodeSuffixFile(floors map[string]uint64) ([]byte, error) {
	names := make([]string, 0, len(floors))
	for name, n := range floors {
		if n == 0 {
			continue
		}
		if err := ValidateAgentName(name); err != nil {
			return nil, fmt.Errorf("%w: refusing to persist agent id suffix floors: %v; a floors file holding that name could never be read back, and a floors file that cannot be read back is never regenerated, so writing it would permanently strand this data dir", ErrSuffixFileCorrupt, err)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var body bytes.Buffer
	for _, name := range names {
		body.WriteString(name)
		body.WriteByte(' ')
		body.WriteString(strconv.FormatUint(floors[name], 10))
		body.WriteByte('\n')
	}

	sum := sha256.Sum256(body.Bytes())

	var out bytes.Buffer
	out.WriteString(suffixFileMagic)
	out.WriteString(" v")
	out.WriteString(strconv.Itoa(suffixFileVersion))
	out.WriteString(" sha256=")
	out.WriteString(hex.EncodeToString(sum[:]))
	out.WriteByte('\n')
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// clip bounds a fragment of a CORRUPT file before it is echoed into an error.
// The bytes are arbitrary — that is what "corrupt" means — so a damaged or
// hostile file could otherwise put a megabyte of anything into the operator's
// startup log, several times over once the errors wrap. %q escapes a control
// byte to four characters, so the multiplier is real.
func clip(s string) string {
	const max = 128
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d bytes total, truncated)", len(s))
}

// readSuffixFile loads and verifies the floors file. It reports existed=false
// with an empty map and a nil error when the file has never been written; every
// other failure is fatal and wraps ErrSuffixFileCorrupt (or the underlying I/O
// error, which is not a corruption claim).
//
// The SHA-256 is checked BEFORE any entry is parsed. A checksum that does not
// match means the bytes are not the bytes that were written, and a floor read
// out of them could be lower than the one that was persisted — which is exactly
// the silent rewind this file exists to prevent. It is an integrity check
// against media damage and accidental editing, NOT an authentication check:
// anyone who can write the file can recompute the digest, so it defends the data
// dir's integrity, not its authenticity.
func readSuffixFile(path string) (map[string]uint64, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]uint64), false, nil
		}
		return nil, false, fmt.Errorf("ids: reading persisted agent-id suffix floors from %s: %w", path, err)
	}

	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return nil, false, fmt.Errorf("%w: %s has no header line", ErrSuffixFileCorrupt, path)
	}
	header, body := string(data[:nl]), data[nl+1:]

	fields := strings.Split(header, " ")
	if len(fields) != 3 || fields[0] != suffixFileMagic {
		return nil, false, fmt.Errorf("%w: %s does not start with a %q header line (got %q); this does not look like an agent-bus suffix floors file and it will NOT be regenerated, because that would resume every name from 1 and re-mint agent ids that are already on disk (invariant 1) — fix or move it manually", ErrSuffixFileCorrupt, path, suffixFileMagic, clip(header))
	}

	wantVersion := "v" + strconv.Itoa(suffixFileVersion)
	if fields[1] != wantVersion {
		return nil, false, fmt.Errorf("%w: %s is format %s, but this binary understands only %s; a file written by a NEWER agent-bus may encode floors this binary cannot see, and reading it partially would lower a floor — run the version of agent-bus that wrote it, or migrate the data dir deliberately", ErrSuffixFileCorrupt, path, clip(fields[1]), wantVersion)
	}

	const sumPrefix = "sha256="
	if !strings.HasPrefix(fields[2], sumPrefix) {
		return nil, false, fmt.Errorf("%w: %s header has no %s digest (got %q)", ErrSuffixFileCorrupt, path, sumPrefix, clip(fields[2]))
	}
	want, derr := hex.DecodeString(strings.TrimPrefix(fields[2], sumPrefix))
	if derr != nil || len(want) != sha256.Size {
		return nil, false, fmt.Errorf("%w: %s header digest %q is not %d hex bytes", ErrSuffixFileCorrupt, path, clip(fields[2]), sha256.Size)
	}
	got := sha256.Sum256(body)
	if !bytes.Equal(got[:], want) {
		return nil, false, fmt.Errorf("%w: %s fails its own checksum (header says %x, body hashes to %x); the file is not the bytes that were written, so a floor read from it could be LOWER than the one persisted — it will NOT be regenerated, because resuming every name from 1 re-mints agent ids that are already on disk (invariant 1)", ErrSuffixFileCorrupt, path, want, got[:])
	}

	floors := make(map[string]uint64)
	for lineNo, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if line == "" {
			if len(floors) == 0 && len(body) == 0 {
				break // empty body: no floors yet, which is legal.
			}
			return nil, false, fmt.Errorf("%w: %s entry %d is a blank line", ErrSuffixFileCorrupt, path, lineNo+1)
		}
		name, n, perr := parseSuffixEntry(line)
		if perr != nil {
			return nil, false, fmt.Errorf("%w: %s entry %d: %v", ErrSuffixFileCorrupt, path, lineNo+1, perr)
		}
		if _, dup := floors[name]; dup {
			return nil, false, fmt.Errorf("%w: %s lists agent name %q twice; the floors file holds exactly one floor per name and a reader that took either one could take the lower", ErrSuffixFileCorrupt, path, name)
		}
		floors[name] = n
	}
	return floors, true, nil
}

// parseSuffixEntry splits and validates one "<name> <suffix>" line.
//
// The name goes through ValidateAgentName — the file is input to be validated,
// not a trusted store — so a tampered file cannot introduce a counter key that
// no legal name could ever match, which would leave that name silently resuming
// from 1. The number is required to be canonical (decimal, no sign, no leading
// zero, non-zero) so each floor has exactly one spelling, matching the rule
// ParseAgentID enforces on the suffix inside an id.
func parseSuffixEntry(line string) (string, uint64, error) {
	i := strings.IndexByte(line, ' ')
	if i < 0 {
		return "", 0, fmt.Errorf("expected \"<name> <suffix>\", got %q", clip(line))
	}
	name, numPart := line[:i], line[i+1:]
	if err := ValidateAgentName(name); err != nil {
		return "", 0, fmt.Errorf("invalid agent name: %w", err)
	}
	if numPart == "" {
		return "", 0, fmt.Errorf("agent name %q has an empty suffix", name)
	}
	for j := 0; j < len(numPart); j++ {
		if c := numPart[j]; c < '0' || c > '9' {
			return "", 0, fmt.Errorf("suffix %q for agent name %q must be decimal digits only", numPart, name)
		}
	}
	if len(numPart) > 1 && numPart[0] == '0' {
		return "", 0, fmt.Errorf("suffix %q for agent name %q has a leading zero; a floor has exactly one spelling", numPart, name)
	}
	n, err := strconv.ParseUint(numPart, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("suffix %q for agent name %q is not a 64-bit decimal number: %w", numPart, name, err)
	}
	if n == 0 {
		return "", 0, fmt.Errorf("agent name %q has floor 0; floor 0 is spelled by ABSENCE, so an explicit 0 is a second spelling of one state", name)
	}
	return name, n, nil
}

// The atomic temp-file-plus-rename writer this file's floors are persisted
// through lives in atomicfile.go, shared with busid.go's writeBusIDFile. It used
// to be a second, byte-identical copy here; see atomicWriteFile's doc for why
// exactly one copy is a durability property and not a style preference.
