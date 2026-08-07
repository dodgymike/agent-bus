package wal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// IndexFloorFileName is the file within the data directory that holds the
// durable WAL record-index floor. It sits alongside wal-mac.key and the ids
// package's bus-id and agent-suffixes files: all of them are small, atomically
// replaced files that carry the data directory's IDENTITY rather than its
// contents, and losing one loses continuity, not messages.
//
// It is EXPORTED because operators and CONTRACTS-ONDISK.md need to name it, and
// because the error a corrupt floor raises tells an operator to move exactly
// this path aside.
const IndexFloorFileName = "wal-index-floor"

// indexFloorMagic is the first token of the header line, spelled out in full so
// a stray file in a data directory is identifiable by `head -1` alone.
const indexFloorMagic = "agent-bus-wal-index-floor"

// indexFloorFileVersion is the on-disk format version of the index floor file.
//
// It is RESERVED, not chosen: value 4 in the Spec Server `ondisk-format-version`
// namespace, reserved 2026-08-07 by feature-runner for this work (1 and 2 are
// the WAL frame format, 3 is ids/agent-suffixes). Never pick one of these by
// eyeballing the list -- that is the parallel-agent collision class CLAUDE.md
// names explicitly.
//
// An UNKNOWN version is a HARD ERROR, never a "read what you can". A file
// written by a newer binary may encode a HIGHER floor this one cannot see, and
// reading it partially would LOWER a floor -- which is the one thing this file
// exists to make impossible.
const indexFloorFileVersion = 4

// indexReserveBlock is how far AHEAD of the index actually being used the
// durable `reserved` ceiling is pushed each time it has to move.
//
// THE TRADE, stated rather than assumed:
//
//   - Reserving a BLOCK amortises the floor write to roughly one per 256
//     appends. The WAL already fsyncs once per append, so this adds about 0.4%
//     to the fsync count on the send path. Reserving exactly one index per
//     append instead would be simpler and would DOUBLE the fsyncs on the write
//     path -- doubling the cost of invariant 4's guarantee to buy nothing that
//     matters, because the thing being protected (an index is never reissued) is
//     equally protected by a block.
//   - The cost of the block is that a CRASH may BURN up to indexReserveBlock-1
//     indices that were never used by any record. Those show up as a HOLE in the
//     log's index sequence on the next start.
//
// HOLES ARE LEGAL, PERMANENT AND CORRECT HERE. Invariant 1 (ids are never
// reused) beats gap-freeness, and internal/ids/sequence.go already states the
// same rule for the neighbouring counter: "Gaps in the committed stream are
// CORRECT". Nothing in this system reads a dense index sequence as a
// requirement; scanFrom accepts a RISING index precisely because a repaired log
// already has holes.
//
// FOLLOW-UP, named so it is not mistaken for a measured value: 256 is a knob
// NOBODY HAS MEASURED. It was chosen to make the amortised write cost
// obviously negligible while keeping the worst-case burn small enough to be
// invisible in a log. If the send path is ever profiled, this is the number to
// look at.
const indexReserveBlock = 256

// ErrIndexFloorCorrupt is returned by openIndexFloor when the index floor file
// EXISTS but does not verify: bad header, unknown version, checksum mismatch,
// malformed number, or a body that contradicts itself (written > reserved).
//
// It is FATAL, and the file is deliberately NEVER regenerated. That is the same
// posture ids.ErrSuffixFileCorrupt and a corrupt persisted bus id take, for the
// same reason: regenerating means resuming the WAL record index -- and therefore
// the message sequence derived from it -- BELOW numbers already handed out,
// silently and undetectably.
//
// # Reconciling this with invariant 6 ("recovery always reaches a running
// server")
//
// This is NOT "the bus refuses to boot over corruption". The LOG still always
// starts: a damaged, unreadable, or entirely unsalvageable WAL is still
// truncated, rewritten or quarantined and the bus still comes up. What refuses
// here is a damaged IDENTITY file, which is the same narrow exception the user
// already granted for the MAC key and the persisted bus id -- damage to the data
// directory's identity is not damage to the log, and it is fixable in seconds.
//
// A crash can NEVER produce this state. The write is temp file + fsync + rename
// + directory fsync, so a reader sees the whole old file or the whole new one,
// never a torn one. Corruption therefore means media damage or tampering, and
// there is no benign cause to be generous to.
//
// The error message names a concrete one-step remedy, so the bus is never
// permanently bricked. See readIndexFloorFile.
var ErrIndexFloorCorrupt = errors.New("wal: the persisted WAL record index floor is corrupt")

// indexFloor is the DURABLE, MONOTONIC record-index floor for one data
// directory. It lives OUTSIDE the log, and that is the entire point.
//
// # The defect it closes
//
// Until 2026-08-07 the index the next append would use was derived SOLELY from
// what survived in the log: one past the highest SURVIVING record. Two
// consequences, both reported as defects and both reversed by the user rather
// than accepted:
//
//   - A discarded TAIL record's index was handed straight back out. Recovery
//     cannot tell "a write that was interrupted" from "a write that completed,
//     was acknowledged, and then had a bit flipped", so an id a client had
//     already seen could be minted a second time.
//   - A QUARANTINE reset the index to 1, reissuing the ENTIRE index space.
//
// internal/hub derives the message-sequence floor from wal.Recovered.NextIndex,
// so both defects reissued MESSAGE IDS as well as record indices.
//
// # Why a separate file, and not the log
//
// A floor derived from the log drops whenever the log does. wal.RepairLog may
// truncate a tail, rewrite the middle, or move the whole file aside -- and a
// number stored inside the thing being repaired inherits every repair. Storing
// the floor in its own atomically-replaced file makes invariant 1 STRUCTURAL
// rather than something the recovery path has to remember: no amount of WAL
// damage can lower a floor that was never in the WAL. A quarantine then still
// starts a fresh log (invariant 6 intact) while the index still resumes above
// everything ever authorised (invariant 1 intact), and no refusal to boot is
// needed to get both.
//
// This is the same shape ids.DurableNameSuffixes uses for agent-id suffixes,
// deliberately: WRITE THE FLOOR AHEAD OF THE NUMBER IT AUTHORISES. It is
// invariant 4's ordering ("nothing is acknowledged before it is durable") one
// layer down -- nothing is USED before its floor is durable.
//
// # The two fields, and the invariant on each
//
//   - reserved -- NO WAL record index ABOVE this value has EVER been authorised
//     by this data directory. Strictly non-decreasing; a decrease is a bug and
//     is REFUSED in code, not silently accepted.
//   - written  -- every WAL record index AT OR BELOW this value is BURNED: it
//     has either been written to the log or been permanently SKIPPED by
//     recovery. Strictly non-decreasing, same enforcement.
//
// And always: written <= reserved. A loaded file that violates that is corrupt,
// because a `written` above `reserved` would claim indices were used that were
// never authorised.
//
// # What is deliberately NOT here
//
// There is NO "clean shutdown" flag and NO field that can rewind. That is a
// design choice, not an omission. A clean-shutdown flag invites exactly one
// piece of reasoning -- "we shut down cleanly, so the extra reservation can be
// released" -- and that reasoning is how a floor gets lowered. Every field here
// only ever goes up, so there is no code path, and no future well-meaning
// optimisation, that can lower one.
//
// The zero value is not usable; construct with openIndexFloor. It is safe for
// concurrent use: one mutex is held across the whole read-modify-persist
// sequence INCLUDING the fsync, because two goroutines that both observed the
// same ceiling and then raced to persist would authorise the same index twice,
// which is the entire bug this type exists to prevent.
type indexFloor struct {
	mu       sync.Mutex
	dir      string
	path     string
	reserved uint64
	written  uint64
	existed  bool
}

// openIndexFloor reads and VERIFIES the index floor in dir. It writes nothing:
// the file is created by the first begin.
//
// MISSING vs CORRUPT is the load-bearing judgement call in this file, so the
// argument is here rather than left to be inferred from the code:
//
//   - MISSING IS NOT FATAL. It has a legitimate benign cause -- a data directory
//     written by a binary that predates this file -- and making it fatal would
//     BRICK EVERY DEPLOYED BUS ON UPGRADE, which is exactly the bricking
//     upgradeV1 exists to avoid. The fallback (derive the index from the log, as
//     this package did before) is strictly NO WORSE than the status quo it
//     replaces, so refusing to start buys nothing and costs everything. Open
//     logs it at WARN when the data directory is not otherwise fresh.
//   - CORRUPT IS FATAL and the file is NEVER regenerated. See
//     ErrIndexFloorCorrupt for why, and for why that does not contradict
//     invariant 6.
//
// An I/O failure -- permission denied, a device error -- is returned AS-IS and
// is NOT dressed up as a corruption claim. "I could not read the file" and "the
// file is not what was written" call for different operator actions, and
// conflating them would send someone to delete a file that is probably fine.
func openIndexFloor(dir string) (*indexFloor, error) {
	if dir == "" {
		return nil, errors.New("wal: opening the durable index floor: data dir must not be empty")
	}
	path := filepath.Join(dir, IndexFloorFileName)
	reserved, written, existed, err := readIndexFloorFile(path)
	if err != nil {
		return nil, err
	}
	return &indexFloor{dir: dir, path: path, reserved: reserved, written: written, existed: existed}, nil
}

// existedAtOpen reports whether the floor file was present when this data
// directory was opened.
//
// It is false for a genuinely FRESH data directory AND for one whose floor file
// has never been written because the bus predates it. This type cannot tell
// those apart -- the information is simply not there -- so the CALLER must: a
// data directory that already holds WAL records but no floor file is the
// MIGRATION window, and until the file exists a quarantine can still reissue
// ids. Open says exactly that, at WARN.
func (f *indexFloor) existedAtOpen() bool { return f.existed }

// Path reports the file the floor is persisted to. It is for operator messages
// and tests; it is not a hook for writing to that file.
func (f *indexFloor) Path() string { return f.path }

// burned reports the highest index that is durably BURNED: written to the log
// or permanently skipped by recovery. The next index may safely be burned+1.
func (f *indexFloor) burned() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

// ceiling reports the highest index this data directory has ever AUTHORISED.
// Nothing above it was ever handed to a writer, so ceiling+1 is always a safe
// place to resume -- it is the answer used when recovery lost something it
// could not identify and the file can no longer say where it got to.
func (f *indexFloor) ceiling() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reserved
}

// begin records the index the next append will use, ONCE per Open, AFTER the
// start index has been computed and BEFORE the Writer may append anything.
//
// It sets written = max(written, start-1) and reserved = max(reserved,
// start+indexReserveBlock-1), then persists and fsyncs.
//
// RECORDING written = start-1 IS LOAD-BEARING, and it is the point most easily
// missed. It is what durably records "recovery skipped past everything below
// start". Without it a run that jumped the index -- because a quarantine or an
// unidentifiable discard forced it to -- and then wrote NOTHING and crashed
// would leave no trace of the jump, and the next run, seeing an intact log and
// no damage, would happily resume from the log's own high-water mark and reissue
// the indices the previous run had already authorised. That is the exact
// induction hole a naive "only jump when damaged" rule falls into.
func (f *indexFloor) begin(start uint64) error {
	if start == 0 {
		// The first WAL record index is 1, so a start of 0 is a programming
		// error upstream, not a state to encode. Refusing is cheap and stops
		// start-1 underflowing to MaxUint64, which would burn the whole index
		// space irrecoverably.
		return fmt.Errorf("wal: %s: refusing to record a start index of 0; the first WAL record index is 1", f.path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	written := f.written
	if start-1 > written {
		written = start - 1
	}
	reserved := f.reserved
	if r := satAdd(start, indexReserveBlock-1); r > reserved {
		reserved = r
	}
	if written > reserved {
		reserved = written
	}
	return f.persistLocked(reserved, written)
}

// reserve makes index n durably AUTHORISED before it is used.
//
// It is the hot path, so the common case costs nothing: if n is already at or
// below the durable ceiling this is a pure in-memory no-op and returns nil
// WITHOUT touching the disk. That is sound because the ceiling's meaning is
// "nothing above this has ever been authorised" -- an index below it was already
// covered by an earlier, already-fsynced write.
//
// Otherwise the ceiling is pushed to n+indexReserveBlock-1 (saturating) and
// PERSISTED AND FSYNCED BEFORE RETURNING. On success f.reserved >= n, which is
// the post-condition Append relies on: the number it is about to stamp into a
// frame is already durable somewhere that no log repair can lower.
func (f *indexFloor) reserve(n uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= f.reserved {
		return nil
	}
	return f.persistLocked(satAdd(n, indexReserveBlock-1), f.written)
}

// seal records, on a CLEAN close, that every index up to highest has been used:
// written = max(written, highest), reserved = max(reserved, written).
//
// It never lowers anything, and SKIPPING IT IS ALWAYS SAFE. A crash, or a
// poisoned writer whose close deliberately does not seal, simply leaves the
// reservation standing -- and a standing reservation only ever makes the NEXT
// start more conservative (it resumes higher, burning at most a block). Being
// conservative about an id space is free; being optimistic about one is the
// defect.
func (f *indexFloor) seal(highest uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	written := f.written
	if highest > written {
		written = highest
	}
	reserved := f.reserved
	if written > reserved {
		reserved = written
	}
	return f.persistLocked(reserved, written)
}

// persistLocked writes the given pair atomically and only then adopts it in
// memory. The caller must hold f.mu.
//
// The ORDER is deliberate and mirrors ids.DurableNameSuffixes: memory never
// claims more than disk does, so a failed write leaves the in-memory floor
// exactly where the last successful write left it. A caller that saw an error
// has authorised nothing.
//
// A DECREASE IS REFUSED IN CODE rather than silently accepted. Every caller
// above computes a maximum, so a decrease here can only be a bug -- and a bug
// that lowers this file is precisely the failure that no downstream check can
// detect, so it is caught at the last point before the bytes are written.
func (f *indexFloor) persistLocked(reserved, written uint64) error {
	if reserved < f.reserved || written < f.written {
		return fmt.Errorf("wal: internal error: refusing to lower the durable index floor in %s (reserved %d -> %d, written %d -> %d); the floor is monotonic by construction and lowering it would reissue indices already authorised (invariant 1)",
			f.path, f.reserved, reserved, f.written, written)
	}
	if written > reserved {
		return fmt.Errorf("wal: internal error: refusing to write an index floor to %s claiming %d indices burned but only %d reserved; an index cannot be used before it is authorised",
			f.path, written, reserved)
	}
	if err := atomicReplaceFile(f.dir, f.path, ".wal-index-floor-*", encodeIndexFloor(reserved, written)); err != nil {
		return fmt.Errorf("wal: persisting the durable index floor to %s: %w", f.path, err)
	}
	f.reserved, f.written = reserved, written
	f.existed = true
	return nil
}

// satAdd adds b to a, saturating at MaxUint64 rather than wrapping. A wrap here
// would silently RESET the ceiling to a tiny number, which is the one arithmetic
// mistake this file cannot afford.
func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// encodeIndexFloor renders the canonical on-disk form:
//
//	agent-bus-wal-index-floor v4 sha256=<hex of the body>
//	reserved <decimal>
//	written <decimal>
//
// The digest covers the BODY -- the two lines after the header -- so the bytes
// are a function of the two numbers alone, which is what makes the checksum
// meaningful and the file readable by eye. Numbers are canonical decimal (no
// sign, no leading zeros) so each floor has exactly one spelling.
//
// The checksum is an INTEGRITY check against media damage and accidental
// editing, NOT an authentication check: anyone who can write the file can
// recompute the digest. It defends the data directory's integrity, not its
// authenticity -- exactly as the agent-suffixes digest does, and for the same
// reason (an attacker with write access to the data directory can read the MAC
// key sitting next to it anyway).
func encodeIndexFloor(reserved, written uint64) []byte {
	var body bytes.Buffer
	body.WriteString("reserved ")
	body.WriteString(strconv.FormatUint(reserved, 10))
	body.WriteString("\nwritten ")
	body.WriteString(strconv.FormatUint(written, 10))
	body.WriteByte('\n')

	sum := sha256.Sum256(body.Bytes())

	var out bytes.Buffer
	out.WriteString(indexFloorMagic)
	out.WriteString(" v")
	out.WriteString(strconv.Itoa(indexFloorFileVersion))
	out.WriteString(" sha256=")
	out.WriteString(hex.EncodeToString(sum[:]))
	out.WriteByte('\n')
	out.Write(body.Bytes())
	return out.Bytes()
}

// readIndexFloorFile loads and verifies the floor file. It reports existed=false
// with zero floors and a NIL ERROR when the file has never been written; every
// other failure is fatal and wraps ErrIndexFloorCorrupt (or the underlying I/O
// error, which is not a corruption claim).
//
// The SHA-256 is checked BEFORE either number is parsed. A digest that does not
// match means the bytes are not the bytes that were written, and a floor read
// out of them could be LOWER than the one persisted -- which is exactly the
// silent rewind this whole file exists to prevent.
//
// Every corruption message names the same one-step remedy, in the same voice as
// the wrong-MAC-key message in recover.go, so an operator is never left with a
// bus that cannot start and no instruction.
func readIndexFloorFile(path string) (reserved, written uint64, existed bool, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("wal: reading the durable index floor from %s: %w", path, rerr)
	}

	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return 0, 0, false, indexFloorCorrupt(path, "it has no header line")
	}
	header, body := string(data[:nl]), data[nl+1:]

	fields := strings.Split(header, " ")
	if len(fields) != 3 || fields[0] != indexFloorMagic {
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("it does not start with a %q header line (got %q)", indexFloorMagic, clipFragment(header)))
	}
	wantVersion := "v" + strconv.Itoa(indexFloorFileVersion)
	if fields[1] != wantVersion {
		// An unknown version is NOT read partially. A newer binary may encode a
		// HIGHER floor in a shape this one cannot see, and taking the part it
		// understands would lower the floor.
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("it is on-disk format %s, but this binary understands only %s; a file written by a NEWER agent-bus may encode a higher floor this binary cannot see, and reading it partially would LOWER the floor -- run the version of agent-bus that wrote it, or migrate the data directory deliberately",
			clipFragment(fields[1]), wantVersion))
	}
	const sumPrefix = "sha256="
	if !strings.HasPrefix(fields[2], sumPrefix) {
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("its header has no %s digest (got %q)", sumPrefix, clipFragment(fields[2])))
	}
	want, derr := hex.DecodeString(strings.TrimPrefix(fields[2], sumPrefix))
	if derr != nil || len(want) != sha256.Size {
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("its header digest %q is not %d hex bytes", clipFragment(fields[2]), sha256.Size))
	}
	if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("it fails its own checksum (header says %x, body hashes to %x), so it is not the bytes that were written and a floor read from it could be LOWER than the one persisted", want, got[:]))
	}

	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 2 {
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("its body has %d lines, expected exactly 2 (\"reserved <n>\" and \"written <n>\")", len(lines)))
	}
	reserved, err = parseIndexFloorLine(path, lines[0], "reserved")
	if err != nil {
		return 0, 0, false, err
	}
	written, err = parseIndexFloorLine(path, lines[1], "written")
	if err != nil {
		return 0, 0, false, err
	}
	if written > reserved {
		return 0, 0, false, indexFloorCorrupt(path, fmt.Sprintf("it claims %d indices burned but only %d reserved; an index cannot have been used before it was authorised, so the file contradicts itself", written, reserved))
	}
	return reserved, written, true, nil
}

// parseIndexFloorLine reads one "<field> <decimal>" line. The number must be
// CANONICAL -- decimal digits only, no sign, no leading zero -- so that a floor
// has exactly one spelling and the digest is a function of the value.
func parseIndexFloorLine(path, line, field string) (uint64, error) {
	prefix := field + " "
	if !strings.HasPrefix(line, prefix) {
		return 0, indexFloorCorrupt(path, fmt.Sprintf("expected a %q line, got %q", field, clipFragment(line)))
	}
	num := strings.TrimPrefix(line, prefix)
	if num == "" {
		return 0, indexFloorCorrupt(path, fmt.Sprintf("its %s field is empty", field))
	}
	for i := 0; i < len(num); i++ {
		if c := num[i]; c < '0' || c > '9' {
			return 0, indexFloorCorrupt(path, fmt.Sprintf("its %s field %q must be decimal digits only", field, clipFragment(num)))
		}
	}
	if len(num) > 1 && num[0] == '0' {
		return 0, indexFloorCorrupt(path, fmt.Sprintf("its %s field %q has a leading zero; a floor has exactly one spelling", field, clipFragment(num)))
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, indexFloorCorrupt(path, fmt.Sprintf("its %s field %q is not a 64-bit decimal number: %v", field, clipFragment(num), err))
	}
	return n, nil
}

// indexFloorCorrupt builds the fatal error, appending the SAME one-step operator
// remedy every time. The remedy matters as much as the diagnosis: without it
// this error is a permanently bricked bus, which is precisely what invariant 6
// forbids.
func indexFloorCorrupt(path, why string) error {
	return fmt.Errorf("%w: %s: %s. It will NOT be regenerated, because regenerating it resumes the WAL record index (and therefore the message sequence derived from it) below numbers already handed out, which invariant 1 forbids and which nothing downstream can detect. If it is genuinely lost, delete %s and restart: the bus will then resume from the log's own high-water mark, which is correct unless the log has ALSO been damaged or quarantined",
		ErrIndexFloorCorrupt, path, why, path)
}

// clipFragment bounds a fragment of a CORRUPT file before it is echoed into an
// error. The bytes are arbitrary -- that is what "corrupt" means -- so a damaged
// or hostile file could otherwise put a megabyte of anything into the operator's
// startup log, several times over once the errors wrap. %q escapes a control
// byte to four characters, so the multiplier is real. Same rule, same reason, as
// ids.clip.
func clipFragment(s string) string {
	const max = 128
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d bytes total, truncated)", len(s))
}

// atomicReplaceFile writes data to path via a temp file in the SAME directory:
// the temp file is written, chmodded 0600, fsynced and closed, renamed into
// place, and then the directory itself is fsynced so the rename is durable. A
// reader therefore sees either the complete old file or the complete new one,
// never a torn one -- which is what makes "a crash can never produce a corrupt
// floor file" a true statement rather than a hope.
//
// THIS IS THE THIRD COPY of that sequence in this repository:
// ids/busid.go's writeBusIDFile, ids/suffixstore.go's atomicWriteFile, and here.
// The three MUST MOVE TOGETHER -- a fix to the sync ordering in one that is not
// applied to the others leaves a durability hole in whichever was missed. It is
// duplicated rather than shared because the other two live in a different
// package and are unexported, and hoisting them into a new shared package is a
// refactor with no behavioural content -- out of scope under CLAUDE.md's "do not
// refactor unless the task explicitly asks", and filed as a follow-up instead.
//
// tmpPattern is distinct per caller so that two files being replaced in the same
// data directory cannot collide on a temp name.
func atomicReplaceFile(dir, path, tmpPattern string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}

	dirFile, derr := os.Open(dir)
	if derr != nil {
		err = fmt.Errorf("opening %s to fsync directory entry: %w", dir, derr)
		return err
	}
	defer dirFile.Close()
	if serr := dirFile.Sync(); serr != nil {
		err = fmt.Errorf("syncing directory %s: %w", dir, serr)
		return err
	}
	return nil
}
