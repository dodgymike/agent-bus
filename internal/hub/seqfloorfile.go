package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// SeqFloorFileName is the file within the data directory that holds the durable
// MESSAGE-SEQUENCE floor. It sits alongside bus-id, agent-suffixes and
// wal-index-floor: all of them are small, atomically-replaced files that carry
// the data directory's IDENTITY rather than its contents, and losing one loses
// continuity, not messages.
//
// It is EXPORTED because operators and CONTRACTS-ONDISK.md need to name it, and
// because the error a corrupt floor raises tells an operator to move exactly
// this path aside.
const SeqFloorFileName = "message-seq-floor"

// seqFloorFileMagic is the first token of the header line, spelled out in full
// so a stray file in a data directory is identifiable by `head -1` alone.
const seqFloorFileMagic = "agent-bus-message-seq-floor"

// maxSeqFloorFileSize bounds how much of this file is read into memory. A valid
// one is a header line plus "floor <n>" — under 200 bytes — so 4 KiB is orders
// of magnitude of headroom while still refusing a file planted to exhaust memory
// at startup.
const maxSeqFloorFileSize int64 = 4 << 10

// seqFloorFileVersion is the on-disk format version of this file.
//
// It is RESERVED, not chosen: value 5 in the Spec Server `ondisk-format-version`
// namespace, reserved 2026-08-07 for this work (1 and 2 are the WAL frame
// format, 3 is ids/agent-suffixes, 4 is wal/wal-index-floor). Never pick one of
// these by eyeballing the list — that is the parallel-agent collision class
// CLAUDE.md names explicitly.
//
// An UNKNOWN version is a HARD ERROR, never a "read what you can". A file
// written by a newer binary may encode a HIGHER floor this one cannot see, and
// reading it partially would LOWER a floor — which is the one thing this file
// exists to make impossible.
const seqFloorFileVersion = 5

// maxPlausibleSeqFloor is the highest floor a REAL bus could ever have reached,
// and anything above it is treated as corrupt-or-tampered rather than adopted.
//
// # Why a bound is needed at all, when the file already has a digest
//
// The digest is UNKEYED (see encodeSeqFloor), so it is an integrity check
// against media damage and accidental editing, not an authentication check:
// anyone who can write the data directory can recompute it in one line. And
// writing this file needs DIRECTORY write, not write on the 0600 file itself —
// replacing a file is unlink+create or rename, both permissions on the parent.
//
// A floor of math.MaxUint64 with a valid digest was demonstrated to brick a bus
// permanently and silently: it starts perfectly healthy, /healthz is fine, the
// roster and the log are intact, no warning is emitted — and every /v1/mint
// answers 500 "ids: sequence exhausted" for ever, across every restart, because
// the file persists. That is the WORST possible failure shape: total loss of the
// product's function, with no diagnosis anywhere.
//
// Note that "forging this file is equivalent to deleting it" is TRUE and
// IRRELEVANT. Deleting it RECOVERS — the bus rebuilds the floor from what the
// log proves and carries on. Forging it HIGH bricks. They are opposite outcomes,
// so an argument that collapses the two has only checked the harmless direction.
//
// # Why 2^56, and why the bound does not need to be tight
//
// The bound's only job is to separate "a value a real bus reached" from "a value
// only a tamperer or catastrophic corruption produces", and a false positive
// here refuses a start — so it is set generously.
//
// 2^56 is 72,057,594,037,927,936. At a sustained ONE MILLION minted sequences
// per second — four orders of magnitude beyond what a single-node bus on a
// laptop does — reaching it takes about 2,285 years. Meanwhile it leaves more
// than 1.8e19 numbers between it and exhaustion, so a value that passes this
// bound cannot bring exhaustion within reach either: an attacker gains nothing
// by picking the largest value that still passes.
//
// # The one honest caveat
//
// Only the READ is bounded; persistLocked is not. ensureSeqFloorLocked
// deliberately writes math.MaxUint64 on true arithmetic overflow ("claiming the
// whole space is burned is the safe direction"), and bounding writes would need
// a policy for that path rather than a constant. A bus that genuinely overflowed
// would therefore refuse its next start with a message that says "tampered",
// which would be the wrong word — but it is 1.8e19 messages away, and such a bus
// is already permanently unable to mint, so the refusal costs it nothing it had.
const maxPlausibleSeqFloor uint64 = 1 << 56

// ErrSeqFloorFileCorrupt is returned by openSeqFloorFile when the file EXISTS
// but does not verify: bad header, unknown version, checksum mismatch, a
// malformed number, or a floor above maxPlausibleSeqFloor.
//
// It is FATAL, and the file is deliberately NEVER regenerated. That is the same
// posture wal.ErrIndexFloorCorrupt and ids.ErrSuffixFileCorrupt take, for the
// same reason: regenerating means resuming the message sequence BELOW numbers
// already handed out to — and already SIGNED by — a client, silently and
// undetectably. Two validly-signed messages would then carry one origin message
// id and every signature on both would verify.
//
// # Reconciling this with invariant 6 ("recovery always reaches a running
// server")
//
// This is NOT "the bus refuses to boot over corruption". The LOG still always
// starts: a damaged or unsalvageable WAL is still truncated, rewritten or
// quarantined and the bus still comes up. What refuses here is a damaged
// IDENTITY file, which is the same narrow exception already granted for the MAC
// key, the persisted bus id, the agent-suffix floors and the WAL index floor.
//
// A crash can NEVER produce this state. The write is temp file + fsync + rename
// + directory fsync, so a reader sees the whole old file or the whole new one,
// never a torn one. Corruption therefore means media damage or tampering, and
// there is no benign cause to be generous to.
//
// The error message names a concrete one-step remedy, so the bus is never
// permanently bricked — see seqFloorCorrupt.
var ErrSeqFloorFileCorrupt = errors.New("hub: the persisted message sequence floor is corrupt")

// ErrSeqFloorUnprovable is returned by Open when the floor file is ABSENT and
// recovery has removed records from the durable log on the SAME start, so
// neither source can prove the sequence high-water mark.
//
// It is a DIFFERENT error from ErrSeqFloorFileCorrupt on purpose. Corrupt means
// "the bytes on disk are not what was written"; this means "there are no bytes,
// and the thing we would otherwise derive them from has holes in it". They send
// an operator to different remedies — one to a file to move aside, the other to
// a backup of the log or a floor value only they can supply — and collapsing
// them would hand out the wrong instruction on whichever case was not thought
// about.
//
// Like the corrupt case this is one of the narrow IDENTITY-file exceptions to
// invariant 6, not a bus refusing to boot over log corruption: a damaged log on
// a directory that HAS its floor file still starts, discards loudly and serves.
var ErrSeqFloorUnprovable = errors.New("hub: the message sequence floor cannot be proven from anything on disk")

// seqFloorFile is the DURABLE, MONOTONIC message-sequence floor for one data
// directory. It lives OUTSIDE the log, and that is the entire point.
//
// # The defect it closes
//
// SIGN-2/SIGN-6 made a send a TWO-STEP: /v1/mint hands a client a message id and
// a sequence BEFORE anything is written, so the client can SIGN them. To stay
// safe across a restart the mint burns a BATCH of MintBatchSize numbers ahead of
// handing any of them out. Until 2026-08-07 that batch claim was written as a
// "seqfloor" record INSIDE THE WAL — the one artifact a quarantine discards.
//
// The hole that left, and it is not theoretical (a reviewer probe reproduced it
// verbatim): five mints consume five sequences but only TWO WAL indices, because
// one floor record covers 256 numbers. Quarantine the log and the surviving
// sources — wal.Recovered.NextIndex-1, the replayed floor records, the highest
// replayed message sequence — all collapse to about 2, so the next mint reissues
// 3, 4, 5 … numbers a client already holds a SIGNATURE over. Nothing downstream
// can detect it: both signatures verify.
//
// Before the batch existed, every sequence was <= the WAL index of the prepare
// carrying it, so wal's own durable INDEX floor transitively bounded this one.
// THAT IS THE ARGUMENT THE BATCH BROKE, and it is why the surviving
// wal-index-floor does not already cover this case. Do not reinstate any
// reasoning that ties a sequence to a WAL index: the two counters are no longer
// related, and mint.go says so at length.
//
// # Why a separate file, and not the log
//
// Quoting internal/wal/indexfloor.go, which states the principle this file
// applies: "A floor derived from the log drops whenever the log does.
// wal.RepairLog may truncate a tail, rewrite the middle, or move the whole file
// aside — and a number stored inside the thing being repaired inherits every
// repair." Storing the floor in its own atomically-replaced file makes invariant
// 1 STRUCTURAL rather than something the recovery path has to remember: no
// amount of WAL damage can lower a floor that was never in the WAL.
//
// This is the same shape ids.DurableNameSuffixes uses for agent-id suffixes and
// wal's indexFloor uses for record indices, deliberately: WRITE THE FLOOR AHEAD
// OF THE NUMBER IT AUTHORISES. It is invariant 4's ordering ("nothing is
// acknowledged before it is durable") one layer down — nothing is ISSUED before
// its floor is durable.
//
// # The one field, and the invariant on it
//
// floor — every message sequence AT OR BELOW this value is BURNED: it has either
// been handed to a client, been written to the log, or been permanently skipped.
// This bus will never issue any of them again, whether or not a message ever
// carried one. Strictly non-decreasing; a decrease is a bug and is REFUSED in
// code, not silently accepted.
//
// There is deliberately NO second field and NO "clean shutdown" flag. wal's
// indexFloor carries reserved/written because its recovery has to distinguish
// "authorised" from "consumed"; here the two collapse, because a burned sequence
// is burned identically whether a message carried it or a crash wasted it —
// internal/ids/sequence.go already declares the resulting gaps CORRECT. A second
// field would only create a state a future edit could reason downwards from, and
// a floor that can go down is the entire defect.
//
// The zero value is not usable; construct with openSeqFloorFile. It is safe for
// concurrent use: one mutex is held across the whole compare-and-persist
// sequence INCLUDING the fsync, because two goroutines that both observed the
// same floor and then raced to persist would authorise the same sequence twice,
// which is the entire bug this type exists to prevent. (In practice the hub
// serialises every caller on writeMu; the lock here is so that stays true even
// if it stops.)
type seqFloorFile struct {
	mu      sync.Mutex
	dir     string
	path    string
	floor   uint64
	existed bool
}

// openSeqFloorFile reads and VERIFIES the sequence floor in dir. It writes
// nothing: the file is created by the first raise that actually raises.
//
// MISSING vs CORRUPT is the load-bearing judgement call in this file, so the
// argument is here rather than left to be inferred from the code:
//
//   - MISSING IS NOT FATAL. It has a legitimate benign cause — a data directory
//     written by a binary that predates this file — and making it fatal would
//     BRICK EVERY DEPLOYED BUS ON UPGRADE. The fallback (the three log-derived
//     sources Open already takes a maximum over, including the in-log "seqfloor"
//     records mint.go still writes) is EXACTLY the behaviour that shipped before
//     this file existed, so refusing to start buys nothing and costs everything.
//     Open logs it at WARN when the data directory is not otherwise fresh, and
//     CLOSES the window immediately by persisting the derived floor.
//
//     Note this is deliberately the OPPOSITE call to ids.OpenNameSuffixes, whose
//     caller (openSuffixAllocator) treats a missing file on a non-empty data
//     directory as FATAL with a -backfill opt-in, on a security gate's
//     instruction. The difference is that there is NO other durable source for
//     an agent-id suffix — enrolment was memory-only, so a missing floors file
//     really does resume every name from 1 — whereas here the log still carries
//     three independent sources that bound the sequence on any start that is not
//     ALSO a quarantine. Missing-file plus quarantine on the SAME start is the
//     one uncovered case, it is a single-start migration window, and Open says so
//     at ERROR rather than pretending otherwise.
//
//   - CORRUPT IS FATAL and the file is NEVER regenerated. See
//     ErrSeqFloorFileCorrupt for why, and for why that does not contradict
//     invariant 6.
//
// An I/O failure — permission denied, a device error — is returned AS-IS and is
// NOT dressed up as a corruption claim. "I could not read the file" and "the
// file is not what was written" call for different operator actions, and
// conflating them would send someone to delete a file that is probably fine.
//
// The DIRECTORY is checked here rather than at the first write, because a data
// directory that cannot be written is a bus that can never burn a number: it
// would pass startup and then refuse the first mint, which is a startup failure
// wearing a runtime disguise.
func openSeqFloorFile(dir string) (*seqFloorFile, error) {
	if dir == "" {
		return nil, errors.New("hub: opening the durable message sequence floor: data dir must not be empty")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("hub: opening the durable message sequence floor in %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("hub: opening the durable message sequence floor: %s is not a directory", dir)
	}
	path := filepath.Join(dir, SeqFloorFileName)
	floor, existed, err := readSeqFloorFile(path)
	if err != nil {
		return nil, err
	}
	return &seqFloorFile{dir: dir, path: path, floor: floor, existed: existed}, nil
}

// existedAtOpen reports whether the floor file was present when this data
// directory was opened.
//
// It is false for a genuinely FRESH data directory AND for one whose floor file
// has never been written because the bus predates it. This type cannot tell
// those apart — the information is simply not there — so the CALLER must: a data
// directory that already has history but no floor file is the MIGRATION window,
// and until the file exists a quarantine can still reissue minted sequences.
// Open says exactly that.
func (f *seqFloorFile) existedAtOpen() bool { return f.existed }

// Path reports the file the floor is persisted to. It is for operator messages
// and tests; it is not a hook for writing to that file.
func (f *seqFloorFile) Path() string { return f.path }

// burned reports the highest sequence this data directory has durably recorded
// as burned. Nothing at or below it may ever be issued again.
func (f *seqFloorFile) burned() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.floor
}

// raise makes every sequence up to n durably BURNED before any of them is
// issued, and returns only once the bytes and the directory entry are fsynced.
//
// The common case costs nothing: if n is already at or below the durable floor
// this is a pure in-memory no-op and returns nil WITHOUT touching the disk. That
// is sound because the floor's meaning is "nothing at or below this will ever be
// issued again" — a number below it was already covered by an earlier,
// already-fsynced write.
//
// On success f.floor >= n, which is the post-condition Hub.Mint relies on: the
// number it is about to hand to a client is already durable somewhere that no
// log repair, and no quarantine, can lower.
func (f *seqFloorFile) raise(n uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= f.floor {
		return nil
	}
	return f.persistLocked(n)
}

// ensureExists creates the file at the CURRENT floor if it is not on disk yet,
// and does nothing when it already is.
//
// # Why a floor of 0 is worth an fsync
//
// raise(0) deliberately writes nothing, so before this existed a data directory
// only grew a floor file once something had actually burned a number. That left
// "the file is absent" meaning two completely different things — a directory
// written by a binary older than the file, and a directory this binary has
// opened but which has never minted — and the guard in Open has to tell them
// apart, because it REFUSES to start on the first.
//
// Measured, and this is the case that forced it: a brand-new bus that is opened
// and then kill -9'd with no traffic leaves an empty log whose durable index
// floor has already reserved a block (records=0, NextIndex=65). That is
// indistinguishable, from the log alone, from a log whose records were
// destroyed. Writing the file on every start collapses the ambiguity at the
// source: after ANY start with this binary the file exists, so its absence means
// "legacy directory" and nothing else.
//
// It also closes the migration window one step earlier than before — at the
// first START rather than the first MINT — which is strictly better, since the
// window is exactly the interval in which the file is missing.
func (f *seqFloorFile) ensureExists() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existed {
		return nil
	}
	return f.persistLocked(f.floor)
}

// persistLocked writes floor atomically and only then adopts it in memory. The
// caller must hold f.mu.
//
// The ORDER is deliberate and mirrors ids.DurableNameSuffixes and wal's
// indexFloor: memory never claims more than disk does, so a failed write leaves
// the in-memory floor exactly where the last successful write left it. A caller
// that saw an error has authorised nothing, and may safely retry with the same
// number.
//
// A DECREASE IS REFUSED IN CODE rather than silently accepted. Every caller
// above computes a maximum, so a decrease here can only be a bug — and a bug
// that lowers this file is precisely the failure that no downstream check can
// detect, so it is caught at the last point before the bytes are written.
func (f *seqFloorFile) persistLocked(floor uint64) error {
	if floor < f.floor {
		return fmt.Errorf("hub: internal error: refusing to lower the durable message sequence floor in %s (%d -> %d); the floor is monotonic by construction and lowering it would reissue sequences already handed out and already signed (invariant 1)", f.path, f.floor, floor)
	}
	if err := atomicReplaceSeqFloor(f.dir, f.path, encodeSeqFloor(floor)); err != nil {
		return fmt.Errorf("hub: persisting the durable message sequence floor to %s: %w", f.path, err)
	}
	f.floor = floor
	f.existed = true
	return nil
}

// encodeSeqFloor renders the canonical on-disk form:
//
//	agent-bus-message-seq-floor v5 sha256=<hex of the body>
//	floor <decimal>
//
// The digest covers the BODY — the single line after the header — so the bytes
// are a function of the number alone, which is what makes the checksum
// meaningful and the file readable by eye. The number is canonical decimal (no
// sign, no leading zeros) so each floor has exactly one spelling.
//
// The checksum is an INTEGRITY check against media damage and accidental
// editing, NOT an authentication check: anyone who can write the file can
// recompute the digest. It defends the data directory's integrity, not its
// authenticity — exactly as the agent-suffixes and wal-index-floor digests do.
//
// # CORRECTED 2026-08-07 — the reason that used to be given here was WRONG
//
// This comment previously justified leaving the digest unkeyed on the grounds
// that "an attacker with write access to the data directory can read the WAL MAC
// key sitting next to it anyway". THAT IS FALSE, and it equates two independent
// permissions. Replacing this file needs write on the DIRECTORY — unlink+create,
// or rename — while reading wal-mac.key needs read on a 0600 FILE. A local user
// on a group- or other-writable data directory has the first and not the second,
// so there really is an attacker who can forge this unkeyed file and cannot
// forge the keyed WAL index floor. The keying is exactly the difference.
//
// The fix for that is NOT to key this digest. Keying would not help the attacker
// who CAN read the key, and it would leave every other file in the directory
// creatable, deletable and renameable by the same user. The permission itself is
// the defect, so it is closed at the DIRECTORY, once, for every file in it: see
// enforceDataDirPermissions in cmd/agent-bus/datadirperm.go, which refuses to
// start on an other-writable data directory and tightens a group-writable one.
// Keying this file remains worth doing for consistency with wal-index-floor, but
// as a separate and honestly-labelled change — it is not the answer to that
// finding, and recording it as one would leave the real hole open.
//
// maxPlausibleSeqFloor is the second half of the answer, in depth: whatever the
// permissions, a value no real bus could reach is refused rather than adopted.
func encodeSeqFloor(floor uint64) []byte {
	var body bytes.Buffer
	body.WriteString("floor ")
	body.WriteString(strconv.FormatUint(floor, 10))
	body.WriteByte('\n')

	sum := sha256.Sum256(body.Bytes())

	var out bytes.Buffer
	out.WriteString(seqFloorFileMagic)
	out.WriteString(" v")
	out.WriteString(strconv.Itoa(seqFloorFileVersion))
	out.WriteString(" sha256=")
	out.WriteString(hex.EncodeToString(sum[:]))
	out.WriteByte('\n')
	out.Write(body.Bytes())
	return out.Bytes()
}

// readSeqFloorFile loads and verifies the floor file. It reports existed=false
// with a zero floor and a NIL ERROR when the file has never been written; every
// other failure is fatal and wraps ErrSeqFloorFileCorrupt (or the underlying I/O
// error, which is not a corruption claim).
//
// The SHA-256 is checked BEFORE the number is parsed. A digest that does not
// match means the bytes are not the bytes that were written, and a floor read
// out of them could be LOWER than the one persisted — which is exactly the
// silent rewind this whole file exists to prevent.
func readSeqFloorFile(path string) (floor uint64, existed bool, err error) {
	// BOUNDED READ. A legitimate floor file is two short lines — well under 200
	// bytes — but this one is written by whoever can write the data directory,
	// which is precisely the attacker this file's other defences exist for. An
	// unbounded os.ReadFile on a multi-gigabyte "message-seq-floor" would be a
	// trivial memory-exhaustion at startup, before anything has authenticated.
	// maxSeqFloorFileSize is far above any real file and far below anything that
	// matters, and the over-long case is reported as corruption because that is
	// exactly what it is.
	f, rerr := os.Open(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("hub: reading the durable message sequence floor from %s: %w", path, rerr)
	}
	defer f.Close()
	data, rerr := io.ReadAll(io.LimitReader(f, maxSeqFloorFileSize+1))
	if rerr != nil {
		return 0, false, fmt.Errorf("hub: reading the durable message sequence floor from %s: %w", path, rerr)
	}
	if int64(len(data)) > maxSeqFloorFileSize {
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("it is larger than %d bytes; a real floor file is two short lines, so this is damaged or planted and is NOT read into memory", maxSeqFloorFileSize))
	}

	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return 0, false, seqFloorCorrupt(path, "it has no header line")
	}
	header, body := string(data[:nl]), data[nl+1:]

	fields := strings.Split(header, " ")
	if len(fields) != 3 || fields[0] != seqFloorFileMagic {
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("it does not start with a %q header line (got %q)", seqFloorFileMagic, clipSeqFloorFragment(header)))
	}
	wantVersion := "v" + strconv.Itoa(seqFloorFileVersion)
	if fields[1] != wantVersion {
		// An unknown version is NOT read partially. A newer binary may encode a
		// HIGHER floor in a shape this one cannot see, and taking the part it
		// understands would lower the floor.
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("it is on-disk format %s, but this binary understands only %s; a file written by a NEWER agent-bus may encode a higher floor this binary cannot see, and reading it partially would LOWER the floor — run the version of agent-bus that wrote it, or migrate the data directory deliberately",
			clipSeqFloorFragment(fields[1]), wantVersion))
	}
	const sumPrefix = "sha256="
	if !strings.HasPrefix(fields[2], sumPrefix) {
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("its header has no %s digest (got %q)", sumPrefix, clipSeqFloorFragment(fields[2])))
	}
	want, derr := hex.DecodeString(strings.TrimPrefix(fields[2], sumPrefix))
	if derr != nil || len(want) != sha256.Size {
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("its header digest %q is not %d hex bytes", clipSeqFloorFragment(fields[2]), sha256.Size))
	}
	if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("it fails its own checksum (header says %x, body hashes to %x), so it is not the bytes that were written and a floor read from it could be LOWER than the one persisted", want, got[:]))
	}

	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 1 {
		return 0, false, seqFloorCorrupt(path, fmt.Sprintf("its body has %d lines, expected exactly 1 (\"floor <n>\")", len(lines)))
	}
	floor, err = parseSeqFloorLine(path, lines[0])
	if err != nil {
		return 0, false, err
	}
	return floor, true, nil
}

// parseSeqFloorLine reads the "floor <decimal>" line. The number must be
// CANONICAL — decimal digits only, no sign, no leading zero — so that a floor
// has exactly one spelling and the digest is a function of the value.
func parseSeqFloorLine(path, line string) (uint64, error) {
	const prefix = "floor "
	if !strings.HasPrefix(line, prefix) {
		return 0, seqFloorCorrupt(path, fmt.Sprintf("expected a %q line, got %q", "floor", clipSeqFloorFragment(line)))
	}
	num := strings.TrimPrefix(line, prefix)
	if num == "" {
		return 0, seqFloorCorrupt(path, "its floor field is empty")
	}
	for i := 0; i < len(num); i++ {
		if c := num[i]; c < '0' || c > '9' {
			return 0, seqFloorCorrupt(path, fmt.Sprintf("its floor field %q must be decimal digits only", clipSeqFloorFragment(num)))
		}
	}
	if len(num) > 1 && num[0] == '0' {
		return 0, seqFloorCorrupt(path, fmt.Sprintf("its floor field %q has a leading zero; a floor has exactly one spelling", clipSeqFloorFragment(num)))
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, seqFloorCorrupt(path, fmt.Sprintf("its floor field %q is not a 64-bit decimal number: %v", clipSeqFloorFragment(num), err))
	}
	// THE PLAUSIBILITY BOUND. It is checked here, at the last point before the
	// value becomes a floor, and it is deliberately a REFUSAL rather than a
	// clamp: silently lowering a number this file exists to keep monotonic is
	// the one operation it must never perform, and silently ADOPTING it is the
	// permanent brick this bound was added to close. See maxPlausibleSeqFloor.
	if n > maxPlausibleSeqFloor {
		return 0, seqFloorCorrupt(path, fmt.Sprintf("its floor is %d, which is implausibly high: no bus reaches %d in any lifetime (that is roughly 2,285 years at a million minted sequences a second), so this file has been TAMPERED WITH or the media is damaged. Adopting it would exhaust the sequence allocator and make every send fail with \"sequence exhausted\", permanently and across every restart, on a bus that otherwise looks completely healthy",
			n, maxPlausibleSeqFloor))
	}
	return n, nil
}

// seqFloorCorrupt builds the fatal error, appending the SAME one-step operator
// remedy every time. The remedy matters as much as the diagnosis: without it
// this error is a permanently bricked bus, which is precisely what invariant 6
// forbids.
//
// The remedy is deliberately NOT "delete it": deleting drops the floor to
// whatever the log can prove, which is correct ONLY while the log is intact. So
// it says so, in those words, rather than handing an operator a command that is
// safe on most days.
func seqFloorCorrupt(path, why string) error {
	return fmt.Errorf("%w: %s: %s. It will NOT be regenerated, because regenerating it resumes the message sequence below numbers already handed out to clients and already signed by them, which invariant 1 forbids and which nothing downstream can detect — both signatures verify. If it is genuinely lost, move %s aside and restart: the bus will then fall back to the floor the log itself proves (its high-water index, its \"seqfloor\" records and its highest message sequence), which is correct ONLY if that log has not also been damaged or quarantined",
		ErrSeqFloorFileCorrupt, path, why, path)
}

// clipSeqFloorFragment bounds a fragment of a CORRUPT file before it is echoed
// into an error. The bytes are arbitrary — that is what "corrupt" means — so a
// damaged or hostile file could otherwise put a megabyte of anything into the
// operator's startup log, several times over once the errors wrap. %q escapes a
// control byte to four characters, so the multiplier is real. Same rule, same
// reason, as wal.clipFragment and ids.clip.
func clipSeqFloorFragment(s string) string {
	const max = 128
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d bytes total, truncated)", len(s))
}

// atomicReplaceSeqFloor writes data to path via a temp file in the SAME
// directory: the temp file is written, chmodded 0600, fsynced and closed,
// renamed into place, and then the directory itself is fsynced so the rename is
// durable. A reader therefore sees either the complete old file or the complete
// new one, never a torn one — which is what makes "a crash can never produce a
// corrupt floor file" a true statement rather than a hope.
//
// THIS IS THE FOURTH COPY of that sequence in this repository:
// ids/busid.go's writeBusIDFile, ids/suffixstore.go's atomicWriteFile,
// wal/indexfloor.go's atomicReplaceFile, and here. THEY MUST MOVE TOGETHER — a
// fix to the sync ordering in one that is not applied to the others leaves a
// durability hole in whichever was missed. It is duplicated rather than shared
// because the others are unexported in packages this one must not import (wal is
// the very thing this file exists to be independent of), and hoisting them into
// a shared package is a refactor with no behavioural content — out of scope
// under CLAUDE.md's "do not refactor unless the task explicitly asks", and filed
// as a follow-up instead.
func atomicReplaceSeqFloor(dir, path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".message-seq-floor-*")
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
