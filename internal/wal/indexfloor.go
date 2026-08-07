package wal

import (
	"bytes"
	"crypto/hmac"
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
// VERSION 4 IS DELIBERATELY TOLERANT OF TWO EARLIER SHAPES, and the reason is a
// live-deployment break that a security gate proved executably rather than
// argued (2026-08-07):
//
//	v4 body 2 lines, header `sha256=`      -- SHIPPED IN main (commit f56c723).
//	v4 body 3 lines, header `sha256=`      -- the same-day `sealed` addition.
//	v4 body 3 lines, header `hmac-sha256=` -- what this binary WRITES.
//
// An earlier revision of this comment claimed "version 4 has not shipped in any
// release, no deployed data directory carries a two-line body" and made a
// two-line body CORRUPT. THAT CLAIM WAS FALSE: f56c723 is in main and its
// encodeIndexFloor writes exactly a two-line v4 body. The consequence was not
// theoretical -- a routine upgrade from main hit ErrIndexFloorCorrupt, the bus
// refused to start, and the error's own printed remedy (delete the floor) then
// resumed the index BELOW numbers already handed out. That is the exact
// invariant-1 violation this file exists to close, reached through the upgrade
// path.
//
// So the two earlier shapes are ACCEPTED, READ CONSERVATIVELY, and rewritten in
// the current shape by the next begin. "Conservatively" is load-bearing and is
// spelled out at readIndexFloorFile: a body with no `sealed` line, and ANY body
// under the legacy UNKEYED `sha256=` header, loads as sealed=FALSE. See
// legacyUnauthenticated.
//
// THE VERSION NUMBER IS NOT BUMPED FOR EITHER OF THESE, because neither is a
// layout an older binary would misread into a LOWER floor -- which is the only
// thing the version field is defending. (It could not be bumped casually
// anyway: these numbers are RESERVED through the Spec Server, and 5 is already
// taken by internal/hub/seqfloorfile.go.)
//
// An UNKNOWN version is still a HARD ERROR, never a "read what you can". A file
// written by a newer binary may encode a HIGHER floor this one cannot see, and
// reading it partially would LOWER a floor -- which is the one thing this file
// exists to make impossible.
const indexFloorFileVersion = 4

// indexReserveBlock is how far AHEAD of the index actually being used the
// durable `reserved` ceiling is pushed each time it has to move.
//
// THE TRADE, stated rather than assumed, and WITH THE HONEST COST:
//
//   - Reserving a BLOCK amortises the floor write to roughly one per 64 appends.
//     A floor write is NOT one sync: atomicReplaceFile is a temp file + an fsync
//     of that file + a rename + an fsync of the DIRECTORY, so it is roughly
//     THREE sync operations. The WAL already fsyncs once per append, so a block
//     of 64 costs about 3 extra syncs per 64 appends -- call it ~5% on the
//     fsync count of the send path. An earlier draft of this comment claimed
//     "~0.4%" by counting a floor write as a single sync; that was about 2x
//     optimistic in a comment whose entire job is the cost argument, and a
//     reviewer flagged it. Reserving exactly one index per append instead would
//     DOUBLE the syncs on the write path to buy nothing, because the thing being
//     protected (an index is never reissued) is equally protected by a block.
//   - The cost of the block is that a CRASH BURNS up to indexReserveBlock-1
//     indices that were never used by any record -- 63 at this size. Those show
//     up as a HOLE in the log's index sequence on the next start. The burn is no
//     longer conditional on recovery finding damage (see log.go's start-index
//     derivation): any run that does not close cleanly resumes above the
//     ceiling, so 63 is the worst case on EVERY crash, not only a damaged one.
//     That is why the block came down from 256 to 64.
//
// HOLES ARE LEGAL, PERMANENT AND CORRECT HERE. Invariant 1 (ids are never
// reused) beats gap-freeness, and internal/ids/sequence.go already states the
// same rule for the neighbouring counter: "Gaps in the committed stream are
// CORRECT". Nothing in this system reads a dense index sequence as a
// requirement; scanFrom accepts a RISING index precisely because a repaired log
// already has holes.
//
// FOLLOW-UP, named so it is not mistaken for a measured value: BOTH numbers
// above are UNMEASURED. 64 is a knob NOBODY HAS BENCHMARKED, and the ~5% is
// arithmetic on sync counts, not a profile -- a directory fsync and a data fsync
// do not cost the same, and neither costs what a WAL append costs. If the send
// path is ever profiled, this is the number to look at, and the profile should
// replace this paragraph rather than be added next to it.
const indexReserveBlock = 64

// indexFloorTmpPattern is the os.CreateTemp pattern the floor file is replaced
// through. It is a CONSTANT rather than a literal at each site because two
// places depend on it agreeing exactly: atomicReplaceFile creates the temp
// files, and reapStaleFloorTemps removes the ones a crash left behind. A drift
// between the two would silently turn the reaper into a no-op.
const indexFloorTmpPattern = ".wal-index-floor-*"

// The two header tag spellings, and what each one means.
//
//	indexFloorTagHMAC   HMAC-SHA256 under the data directory's wal-mac.key --
//	                    the SAME key that authenticates every WAL frame. This is
//	                    what this binary writes, and it is AUTHENTICATION: only a
//	                    holder of the key can produce it.
//	indexFloorTagLegacy an UNKEYED SHA-256 over the body. Written by every
//	                    agent-bus up to and including f56c723. It is INTEGRITY
//	                    ONLY -- anyone who can write the file can recompute it --
//	                    and it is accepted solely so an upgrade does not brick a
//	                    running bus.
//
// WHY THE KEYED TAG IS NOT OPTIONAL (CLAUDE.md invariant 6: "integrity is
// protected by a keyed MAC ... never a CRC -- a CRC is unkeyed and linear, and a
// remote client was shown able to forge one"). A security gate demonstrated the
// same forgery here WITHOUT READING ANY KEY: flip `sealed 0` to `sealed 1`,
// recompute the unkeyed digest by hand, and the reopened bus reissues indices at
// almost every truncation offset. Forging the seal reissues MESSAGE IDS with no
// log tampering whatsoever, and every frame the bus then writes carries a VALID
// MAC because the server itself computes it -- so the corruption is invisible to
// everything downstream. The standing justification for the unkeyed digest ("an
// attacker with directory write access can read wal-mac.key anyway") defends
// forging log RECORDS; it does not defend this, because this attack needs no key
// at all.
const (
	indexFloorTagHMAC   = "hmac-sha256="
	indexFloorTagLegacy = "sha256="
)

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
// THAT CLAIM IS TRUE ONLY BECAUSE OF THE `sealed` BIT, and until 2026-08-07 it
// was FALSE AS IMPLEMENTED. Being outside the log is necessary and is not
// sufficient: the ceiling used to be consulted only when recovery could PROVE
// the log had lost something, and a truncation at a clean frame boundary is
// byte-indistinguishable from a shorter log, so a whole class of damage lowered
// the effective floor while this file sat on disk unread. A reviewer's probe
// found 25 of 2289 truncation offsets reissuing an index, all of them exactly
// the clean frame boundaries, plus the simpler case of deleting bus.wal after a
// crash. What closes it is that the ceiling is now consulted whenever the
// previous run did not close cleanly -- a fact this file records and no log
// damage can change. See sealedClean and log.go's start-index derivation.
//
// This is the same shape ids.DurableNameSuffixes uses for agent-id suffixes,
// deliberately: WRITE THE FLOOR AHEAD OF THE NUMBER IT AUTHORISES. It is
// invariant 4's ordering ("nothing is acknowledged before it is durable") one
// layer down -- nothing is USED before its floor is durable.
//
// # The three fields, and the invariant on each
//
//   - reserved -- NO WAL record index ABOVE this value has EVER been authorised
//     by this data directory. Strictly non-decreasing; a decrease is a bug and
//     is REFUSED in code, not silently accepted.
//   - written  -- every WAL record index AT OR BELOW this value is BURNED: it
//     has either been written to the log or been permanently SKIPPED by
//     recovery. Strictly non-decreasing, same enforcement.
//   - sealed   -- 1 means THE RUN THAT WROTE THIS FILE CLOSED CLEANLY, and
//     therefore `written` is EXACT: it is the highest index ever written to this
//     data directory's log, not merely a lower bound. 0 means unknown.
//
// And always: written <= reserved. A loaded file that violates that is corrupt,
// because a `written` above `reserved` would claim indices were used that were
// never authorised.
//
// # The rules that make `sealed` trustworthy
//
// The bit is worth exactly as much as the discipline around writing it, so the
// discipline is enumerated rather than left to be inferred:
//
//   - begin() writes sealed 0, FSYNCED, BEFORE the Writer may append anything.
//     This is the rule that makes the bit mean something: from the instant a run
//     can write a record, the file on disk says 0, so A CRASH CAN ONLY EVER LEAVE
//     sealed 0.
//   - reserve() writes sealed 0. It only ever runs mid-run, so it can never be
//     the thing that promotes a file to "closed cleanly".
//   - seal(highest) is the ONLY writer of sealed 1, and it is reachable only from
//     a clean Writer.Close -- not from a poisoned close, not from a defer, not
//     from a crash.
//   - A MISSING file loads as sealed 0, which is the conservative reading: a data
//     directory that predates this file has told us nothing about how it stopped.
//
// # Why `sealed` is exempt from the monotonicity check, and why that is safe
//
// persistLocked refuses to lower reserved or written. `sealed` is NOT a counter
// and is deliberately exempt: it goes 1 -> 0 on the very next begin, by design.
//
// THIS IS A DIFFERENT ANIMAL FROM THE FLAG THE SECTION BELOW ARGUES AGAINST, and
// the difference is the direction of the failure. The rejected flag was one that
// would LOWER a floor ("we shut down cleanly, so release the spare reservation").
// This one can only ever make the NEXT START MORE CONSERVATIVE: sealed 0 makes
// recovery take the ceiling, sealed 1 lets it take the (exact) written mark. And
// its failure direction is fail-safe -- if the seal write fails, or the process
// dies before it, the bit stays 0 and recovery burns a block it did not have to.
// There is no reachable state in which a lost or corrupted `sealed` makes the
// next start resume LOWER than it should.
//
// # What is deliberately NOT here
//
// There is NO field that can rewind a floor. In particular there is no
// "release the spare reservation on a clean shutdown" path: that reasoning is
// how a floor gets lowered, and neither reserved nor written has a code path
// that can go down.
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
	sealed   bool
	existed  bool

	// legacy records that the file this floor was LOADED FROM carried the
	// unkeyed sha256 digest rather than the keyed tag. It is diagnostics for
	// Open's upgrade warning; the conservative reading it forces (sealed=false)
	// has already been applied by the time it is set.
	legacy bool

	// unverif records that the file carried a KEYED tag that could not be
	// checked because the key that wrote it is gone. Same conservative reading,
	// louder log line. See openIndexFloor.
	unverif bool

	// key authenticates the file. It is the data directory's wal-mac.key -- the
	// same key the WAL frames are MAC'd under, deliberately: this file records
	// the index space of that log, so binding the two to one key is the whole
	// point. It is set once, at openIndexFloor, and is never empty: the caller
	// resolves the key (loading it, or creating one where macKeyFor judges that
	// safe) before the floor is opened, and persistLocked refuses to write an
	// unauthenticated file.
	key []byte
}

// floorFile is what one on-disk floor file decodes to. It is a struct rather
// than five return values because the read has grown two compatibility outcomes
// and a positional signature had stopped being readable at the call sites.
type floorFile struct {
	reserved uint64
	written  uint64
	sealed   bool

	// existed is false, with everything else zero and a NIL error, when the file
	// has never been written. That is the migration case, and it is benign.
	existed bool

	// legacy is true when the file verified under the UNKEYED sha256 digest.
	// sealed has already been forced to false in that case -- see
	// readIndexFloorFile.
	legacy bool

	// unverified is true when the file carried a keyed tag that could not be
	// checked because the key that wrote it is gone. sealed is forced false for
	// this too.
	unverified bool
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
// key is the data directory's RESOLVED wal-mac.key -- the one the log's frames
// are read under -- and must not be empty: the floor is written under it, and
// writing an unkeyed floor is the forgeable shape this file exists to stop
// producing.
//
// keyIsOriginal says whether that key was ALREADY IN THE DIRECTORY when the
// directory was opened, or was minted by this recovery because none was there.
// It decides what a failed keyed tag MEANS, and the two readings are opposite:
//
//   - keyIsOriginal -- the key is this directory's own, so a tag that does not
//     verify means the FLOOR's bytes are wrong: media damage, or tampering.
//     FATAL. There is no benign cause to be generous to, and the numbers cannot
//     be believed.
//   - !keyIsOriginal -- the key was LOST and a new one minted, so NOTHING the
//     previous identity wrote can verify, floor included. That is not damage to
//     the floor and refusing over it would brick a bus that recovery has already
//     decided may be re-founded (macKeyFor). The floor is read UNVERIFIED
//     instead: numbers kept, `sealed` discarded, and the caller logs it at ERROR.
//
// Reading an unverified floor is deliberately NOT the same as ignoring it. Its
// numbers are only ever consumed as a RAISE (see log.go's start-index
// derivation), so believing a forged one can cost availability -- loudly -- but
// never a reissued index. And it is strictly better than the alternative on
// offer, because the attacker who could forge that file could equally DELETE it,
// which no MAC can prevent.
func openIndexFloor(dir string, key []byte, keyIsOriginal bool) (*indexFloor, error) {
	if dir == "" {
		return nil, errors.New("wal: opening the durable index floor: data dir must not be empty")
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("wal: opening the durable index floor in %s: no MAC key; the floor is authenticated under the data directory's %s, and an unkeyed floor is forgeable by anyone who can write the directory", dir, MACKeyFileName)
	}
	path := filepath.Join(dir, IndexFloorFileName)
	ff, err := readIndexFloorFile(path, key, keyIsOriginal)
	if err != nil {
		return nil, err
	}
	reapStaleFloorTemps(dir)
	return &indexFloor{
		dir: dir, path: path,
		reserved: ff.reserved, written: ff.written, sealed: ff.sealed,
		existed: ff.existed, legacy: ff.legacy, unverif: ff.unverified, key: key,
	}, nil
}

// legacyUnauthenticated reports that the floor was loaded from a file carrying
// the UNKEYED sha256 digest -- one written by a binary that predates the keyed
// tag. Its `sealed` bit has already been discarded as untrustworthy; the next
// begin rewrites the file with a keyed tag. Open logs it, loudly, once.
func (f *indexFloor) legacyUnauthenticated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.legacy
}

// unverified reports that the floor carried a KEYED tag that could not be
// checked, because the key that would check it is gone and this recovery minted
// a new one. Its `sealed` bit has been discarded and its numbers are believed
// only upward. Open logs it at ERROR -- this is a data directory that has lost
// its identity, not a routine start.
func (f *indexFloor) unverified() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unverif
}

// reapStaleFloorTemps removes temp files left behind by a crash BETWEEN
// os.CreateTemp and os.Rename inside atomicReplaceFile.
//
// Such a file is meaningless: it is a partial or complete copy of a floor that
// was never adopted, and nothing ever reads one. Before this existed nothing
// removed them either, so a data directory accumulated one per crash-during-a-
// floor-write, for ever.
//
// # Why deleting them cannot race another writer
//
// The obvious hazard is deleting a temp file some OTHER process is in the middle
// of writing. It cannot happen here, and the reason is not timing: the data
// directory is held under an exclusive flock for the lifetime of the server (see
// internal/dirlock, and the note in log.go's Open about why the replay/writer
// offset check is NOT a lock). One process at a time owns this directory, so a
// temp file that exists at Open belongs to a process that is already gone. If
// that lock is ever removed or made advisory-in-name-only, THIS REAP BECOMES
// UNSAFE and must go with it.
//
// # Why it reads the directory instead of globbing it
//
// It used to be filepath.Glob(filepath.Join(dir, indexFloorTmpPattern)), and
// THAT WAS A DEFECT, because `dir` is operator input (-data) interpolated
// UNESCAPED into a glob PATTERN. A security gate verified both halves
// empirically:
//
//   - -data /srv/bus[1] makes the pattern match the SIBLING directory /srv/bus1,
//     so the reaper unlinks another data directory's temp file while never
//     matching its own.
//   - -data /srv/bus[ makes Glob return ErrBadPattern for ever, so the reap
//     becomes a permanent silent no-op -- and the old comment "the pattern is a
//     constant, so this is unreachable" was simply wrong about which part of the
//     pattern was constant.
//
// os.ReadDir takes a PATH, not a pattern, so no metacharacter in `dir` means
// anything. Matching is then done on the base name against the same
// os.CreateTemp pattern atomicReplaceFile creates with, split the way
// os.CreateTemp splits it (at the LAST '*'), so the two cannot drift.
//
// # It never fails the open
//
// A reap failure is not a reason to refuse to start: the floor itself has
// already been read and verified, and a leftover temp file harms nothing but
// tidiness. Errors are swallowed deliberately -- not logged, because
// openIndexFloor has no logger by design (it is called before recovery has one)
// and threading one in for a housekeeping step is not worth the coupling.
func reapStaleFloorTemps(dir string) {
	prefix, suffix := tempNamePrefixSuffix(indexFloorTmpPattern)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if len(name) < len(prefix)+len(suffix) {
			continue
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// tempNamePrefixSuffix splits an os.CreateTemp pattern the way os.CreateTemp
// itself does: the LAST '*' is the random part, and a pattern with no '*' gets
// the random part appended. Anything this returns matches exactly the set of
// names atomicReplaceFile can create, which is what keeps the reaper from ever
// touching bus.wal, wal-mac.key or the floor file itself.
func tempNamePrefixSuffix(pattern string) (prefix, suffix string) {
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		return pattern[:i], pattern[i+1:]
	}
	return pattern, ""
}

// existedAtOpen reports whether the floor file was present when this data
// directory was OPENED. It means exactly that and nothing later: persistLocked
// deliberately does NOT set it, so the accessor keeps answering the question its
// name asks even after this process has written the file.
//
// (It used to be flipped true by persistLocked, which made the answer "has this
// floor ever been written, by anyone, including me, a moment ago" -- a different
// question, and one no caller wants. log.go additionally snapshots the value
// before its begin; that stays as belt-and-braces, but the field is now honest
// on its own and a caller does not have to know the trick.)
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
// place to resume -- it is the answer used whenever the previous run did not
// close cleanly, and therefore whenever the log's own arithmetic might be a
// lower bound rather than the answer.
func (f *indexFloor) ceiling() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reserved
}

// sealedClean reports whether the run that last wrote this file CLOSED CLEANLY.
//
// When it is true, `written` is EXACT -- the highest index ever written to this
// data directory's log -- so written+1 already dominates every index that was
// ever put in a frame and no reservation needs to be burned. When it is false,
// the previous run may have written anything up to the ceiling and left no
// trace, so only ceiling+1 is safe.
//
// It is FALSE for a missing file, which is the conservative reading. See the
// type doc for the four write rules that make it trustworthy.
func (f *indexFloor) sealedClean() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sealed
}

// begin records the index the next append will use, ONCE per Open, AFTER the
// start index has been computed and BEFORE the Writer may append anything.
//
// It sets written = max(written, start-1), reserved = max(reserved,
// start+indexReserveBlock-1) and sealed = FALSE, then persists and fsyncs.
//
// CLEARING `sealed` HERE IS WHAT MAKES THE BIT TRUSTWORTHY. It is written and
// fsynced BEFORE the Writer may append a single record, so from the moment this
// run can put an index in a frame the file on disk already says "unknown". A
// crash can therefore only ever leave sealed 0, and a sealed 1 on disk is proof
// -- not a hint -- that some run reached Writer.Close without being killed.
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
	return f.persistLocked(reserved, written, false)
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
//
// It writes sealed 0, which is a no-op in practice -- begin already cleared it
// before any append was possible -- but is stated positively rather than left
// to that assumption: reserve runs ONLY mid-run, so it must never be the call
// that leaves "closed cleanly" on disk.
func (f *indexFloor) reserve(n uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= f.reserved {
		return nil
	}
	return f.persistLocked(satAdd(n, indexReserveBlock-1), f.written, false)
}

// seal records, on a CLEAN close, that every index up to highest has been used:
// written = max(written, highest), reserved = max(reserved, written), and
// sealed = TRUE.
//
// IT IS THE ONLY WRITER OF sealed 1 and it is reachable only from a clean
// Writer.Close -- never from a poisoned close, never from a defer, and never
// from a process that was killed. That is what lets the next Open read the bit
// as proof that `written` is EXACT rather than a lower bound, and therefore skip
// the ceiling and burn no indices at all on an ordinary restart.
//
// It never lowers anything, and SKIPPING IT IS ALWAYS SAFE. A crash, or a
// poisoned writer whose close deliberately does not seal, simply leaves the
// reservation standing AND the bit at 0 -- and both of those only ever make the
// NEXT start more conservative (it resumes higher, burning at most a block).
// Being conservative about an id space is free; being optimistic about one is
// the defect.
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
	return f.persistLocked(reserved, written, true)
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
//
// `sealed` IS EXEMPT from that check, deliberately: it is a fact about the last
// run, not a counter, and it goes 1 -> 0 on the very next begin. See the type
// doc for why that exemption cannot lower a floor -- clearing it only ever makes
// the next start MORE conservative.
//
// It does NOT set f.existed. That field answers "was the file there when this
// data directory was opened", and writing the file now is not an answer to that
// question. See existedAtOpen.
func (f *indexFloor) persistLocked(reserved, written uint64, sealed bool) error {
	if reserved < f.reserved || written < f.written {
		return fmt.Errorf("wal: internal error: refusing to lower the durable index floor in %s (reserved %d -> %d, written %d -> %d); the floor is monotonic by construction and lowering it would reissue indices already authorised (invariant 1)",
			f.path, f.reserved, reserved, f.written, written)
	}
	if written > reserved {
		return fmt.Errorf("wal: internal error: refusing to write an index floor to %s claiming %d indices burned but only %d reserved; an index cannot be used before it is authorised",
			f.path, written, reserved)
	}
	if len(f.key) == 0 {
		// Writing an unkeyed floor is the forgeable shape this change exists to
		// stop writing, so it is refused rather than silently downgraded. Every
		// reachable caller (begin, reserve, seal) runs after Open has adopted the
		// resolved key, so this is a programming error, not an operator state.
		return fmt.Errorf("wal: internal error: refusing to write the durable index floor %s without the data directory's %s; an index floor that is not authenticated under the same key as the log's frames can be forged by anyone who can write the directory, and a forged `sealed 1` reissues indices silently (invariant 6: integrity is a keyed MAC, never an unkeyed checksum)",
			f.path, MACKeyFileName)
	}
	if err := atomicReplaceFile(f.dir, f.path, indexFloorTmpPattern, encodeIndexFloor(reserved, written, sealed, f.key)); err != nil {
		return fmt.Errorf("wal: persisting the durable index floor to %s: %w", f.path, err)
	}
	f.reserved, f.written, f.sealed = reserved, written, sealed
	// The file on disk is now keyed under the current key, so neither
	// "loaded from a legacy unkeyed file" nor "could not be verified" describes
	// it any more. Clearing both keeps the accessors honest about the CURRENT
	// state rather than about a file that has since been replaced.
	f.legacy, f.unverif = false, false
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
//	agent-bus-wal-index-floor v4 hmac-sha256=<hex tag>
//	reserved <decimal>
//	written <decimal>
//	sealed <0|1>
//
// Numbers are canonical decimal (no sign, no leading zeros) so each floor has
// exactly one spelling, and `sealed` is spelled 0 or 1 for the same reason. The
// file stays readable by eye, which is deliberate -- an operator diagnosing a
// bus that will not start should be able to `cat` it.
//
// THE TAG IS AN HMAC, NOT A CHECKSUM, and it is keyed with the SAME
// wal-mac.key that authenticates every WAL frame. See indexFloorTagHMAC for why
// an unkeyed digest was not adequate here, and indexFloorTag for what it covers.
func encodeIndexFloor(reserved, written uint64, sealed bool, key []byte) []byte {
	sealedDigit := "0"
	if sealed {
		sealedDigit = "1"
	}
	var body bytes.Buffer
	body.WriteString("reserved ")
	body.WriteString(strconv.FormatUint(reserved, 10))
	body.WriteString("\nwritten ")
	body.WriteString(strconv.FormatUint(written, 10))
	body.WriteString("\nsealed ")
	body.WriteString(sealedDigit)
	body.WriteByte('\n')

	var out bytes.Buffer
	out.WriteString(indexFloorHeaderPrefix())
	out.WriteString(" ")
	out.WriteString(indexFloorTagHMAC)
	out.WriteString(hex.EncodeToString(indexFloorTag(key, body.Bytes())))
	out.WriteByte('\n')
	out.Write(body.Bytes())
	return out.Bytes()
}

// indexFloorHeaderPrefix is the part of the header line BEFORE the tag: the
// magic and the version. It is a function rather than a constant because
// indexFloorTag covers it, and the tag and the file must be built from the same
// bytes or they cannot agree.
func indexFloorHeaderPrefix() string {
	return indexFloorMagic + " v" + strconv.Itoa(indexFloorFileVersion)
}

// indexFloorTag computes the HMAC-SHA256 tag over the header prefix followed by
// the body.
//
// # What it covers, and why
//
// It covers indexFloorHeaderPrefix() ++ "\n" ++ body -- i.e. EXACTLY THE FILE
// MINUS THE TAG FIELD ITSELF. Covering the version line as well as the body is
// not decoration: it binds the tag to the format version, so a body lifted out
// of a future v5 file cannot be replayed as a v4 one. Only the tag field is
// excluded, because a tag cannot cover itself.
//
// # Construction (invariant 9: never write your own crypto)
//
// This is the SAME PATTERN codec.mac already uses for every WAL frame --
// hmac.New(sha256.New, key) and Write the covered bytes -- with the same stdlib
// packages, used exactly as documented. NOTHING here is a bespoke construction:
// there is no invented domain-separation scheme, no nonce, no padding, no
// key derivation. The domain separation that exists is structural and inherited
// rather than designed: this tag's input begins with the ASCII magic
// "agent-bus-wal-index-floor", while a frame tag's input begins with a 4-byte
// big-endian payload length bounded by MaxPayloadSize (1 MiB), so no frame the
// server will ever MAC can have this prefix.
//
// Verification is hmac.Equal -- never == and never bytes.Equal. A tag comparison
// that leaks timing is a forgery oracle, which is the reason format.go gives for
// the same choice.
func indexFloorTag(key, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(indexFloorHeaderPrefix()))
	m.Write([]byte("\n"))
	m.Write(body)
	return m.Sum(nil)
}

// readIndexFloorFile loads and verifies the floor file. It reports existed=false
// with zero floors and a NIL ERROR when the file has never been written; every
// other failure is fatal and wraps ErrIndexFloorCorrupt (or the underlying I/O
// error, which is not a corruption claim).
//
// THE TAG IS CHECKED BEFORE A SINGLE NUMBER IS PARSED. A tag that does not
// verify means the bytes are not the bytes that were written, and a floor read
// out of them could be LOWER than the one persisted -- which is exactly the
// silent rewind this whole file exists to prevent.
//
// # The three shapes it accepts, and the conservative reading of each
//
//	hmac-sha256= + 3-line body  the current shape. Verified with hmac.Equal
//	                            under key; `sealed` is read and TRUSTED, because
//	                            only a holder of wal-mac.key could have written
//	                            it.
//	sha256=      + 3-line body  the same-day pre-HMAC shape. The digest is
//	                            verified so media damage is still caught, but
//	                            `sealed` is FORCED TO FALSE: an unkeyed digest is
//	                            recomputable by anyone who can write the file, and
//	                            a forged `sealed 1` reissues indices silently.
//	sha256=      + 2-line body  the shape SHIPPED IN main (f56c723). Same
//	                            treatment; there is no `sealed` line to read, so
//	                            false is also the only reading.
//
// Forcing sealed=false on a legacy file is not a hedge, it is the correct
// reading: the bit's whole meaning is "a run reached Writer.Close", and an
// unauthenticated file cannot make that claim credibly. The cost of being wrong
// is one burned reservation block on the first start after upgrade -- a hole in
// the index sequence, which is legal, permanent and correct here. The cost of
// trusting it is invariant 1.
//
// `reserved` and `written` from a legacy file ARE trusted, and that asymmetry is
// deliberate: both are only ever consumed as a RAISE (log.go takes a maximum),
// so a forged value cannot lower a floor. The residual is availability -- a
// forged `reserved` near 2^64 refuses the start -- and that arm is loud, not
// silent, which is the distinction that decides everything else in this file.
//
// # A keyed tag that will not verify
//
// keyIsOriginal decides whether that is FATAL or merely unverified; see
// openIndexFloor, which owns that argument.
//
// Every corruption message names the same remedy, in the same voice as the
// wrong-MAC-key message in recover.go, so an operator is never left with a bus
// that cannot start and no instruction.
func readIndexFloorFile(path string, key []byte, keyIsOriginal bool) (floorFile, error) {
	var ff floorFile

	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			// A MISSING file loads as sealed 0, which is the conservative
			// reading: a data directory that predates this file has said nothing
			// about how the run before it stopped, so recovery must assume the
			// worst and consult the ceiling.
			return ff, nil
		}
		return ff, fmt.Errorf("wal: reading the durable index floor from %s: %w", path, rerr)
	}

	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return ff, indexFloorCorrupt(path, "it has no header line")
	}
	header, body := string(data[:nl]), data[nl+1:]

	fields := strings.Split(header, " ")
	if len(fields) != 3 || fields[0] != indexFloorMagic {
		return ff, indexFloorCorrupt(path, fmt.Sprintf("it does not start with a %q header line (got %q)", indexFloorMagic, clipFragment(header)))
	}
	wantVersion := "v" + strconv.Itoa(indexFloorFileVersion)
	if fields[1] != wantVersion {
		// An unknown version is NOT read partially. A newer binary may encode a
		// HIGHER floor in a shape this one cannot see, and taking the part it
		// understands would lower the floor.
		return ff, indexFloorCorrupt(path, fmt.Sprintf("it is on-disk format %s, but this binary understands only %s; a file written by a NEWER agent-bus may encode a higher floor this binary cannot see, and reading it partially would LOWER the floor -- run the version of agent-bus that wrote it, or migrate the data directory deliberately",
			clipFragment(fields[1]), wantVersion))
	}

	legacy, unverified, err := verifyIndexFloorTag(path, fields[2], body, key, keyIsOriginal)
	if err != nil {
		return ff, err
	}
	ff.legacy, ff.unverified = legacy, unverified

	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	// A TWO-LINE BODY IS THE SHAPE main WRITES (f56c723) and must not be read as
	// corruption -- doing so bricked the bus on a routine upgrade and sent the
	// operator to a remedy that forfeits invariant 1. It carries no `sealed`
	// line, which is exactly the conservative reading anyway.
	switch len(lines) {
	case 2:
		if !legacy {
			// No binary has ever written a keyed tag over a two-line body, so
			// this is a hand-assembled file rather than any version of ours.
			return ff, indexFloorCorrupt(path, "its body has 2 lines but its header carries a keyed "+indexFloorTagHMAC+" tag; no agent-bus has ever written that combination")
		}
	case 3:
	default:
		return ff, indexFloorCorrupt(path, fmt.Sprintf("its body has %d lines, expected 3 (\"reserved <n>\", \"written <n>\", \"sealed <0|1>\") or the 2 lines written by agent-bus up to f56c723", len(lines)))
	}

	ff.reserved, err = parseIndexFloorLine(path, lines[0], "reserved")
	if err != nil {
		return floorFile{}, err
	}
	ff.written, err = parseIndexFloorLine(path, lines[1], "written")
	if err != nil {
		return floorFile{}, err
	}
	if len(lines) == 3 {
		ff.sealed, err = parseIndexFloorSealed(path, lines[2])
		if err != nil {
			return floorFile{}, err
		}
	}
	if legacy || unverified {
		// The seal is a TRUST DECISION -- "a run reached Writer.Close" -- and
		// neither an unkeyed digest nor an unverifiable tag says anything about
		// who wrote it. Discard it; the next begin rewrites the file under the
		// current key and the bit is trustworthy from then on.
		ff.sealed = false
	}
	if ff.written > ff.reserved {
		return floorFile{}, indexFloorCorrupt(path, fmt.Sprintf("it claims %d indices burned but only %d reserved; an index cannot have been used before it was authorised, so the file contradicts itself", ff.written, ff.reserved))
	}
	ff.existed = true
	return ff, nil
}

// verifyIndexFloorTag checks the header's tag field against the body and
// classifies the result: authenticated (both false), LEGACY unkeyed digest, or
// UNVERIFIED because the key that wrote it is gone.
//
// The keyed prefix is tested FIRST. "hmac-sha256=" does not have "sha256=" as a
// prefix so the order cannot matter today, but a tag scheme that could be
// DOWNGRADED by reordering two string tests is not a scheme anyone should have
// to re-derive, so the safe order is written down and this comment says why.
func verifyIndexFloorTag(path, field string, body, key []byte, keyIsOriginal bool) (legacy, unverified bool, err error) {
	switch {
	case strings.HasPrefix(field, indexFloorTagHMAC):
		want, derr := hex.DecodeString(strings.TrimPrefix(field, indexFloorTagHMAC))
		if derr != nil || len(want) != sha256.Size {
			return false, false, indexFloorCorrupt(path, fmt.Sprintf("its header tag %q is not %d hex bytes", clipFragment(field), sha256.Size))
		}
		// hmac.Equal, never == or bytes.Equal: a tag comparison that leaks
		// timing is a forgery oracle. Same rule, same reason, as codec.verifyTag.
		if hmac.Equal(indexFloorTag(key, body), want) {
			return false, false, nil
		}
		if !keyIsOriginal {
			// The key that wrote this file is GONE and recovery minted a new one,
			// so nothing the previous identity wrote can verify. Not damage to
			// the floor, and not a reason to brick a directory recovery has
			// already decided may be re-founded. Read it unverified; the caller
			// logs at ERROR and the numbers are believed only upward.
			return false, true, nil
		}
		return false, false, indexFloorCorrupt(path, fmt.Sprintf("it fails its HMAC-SHA256 tag under this data directory's own %s, so it is not the bytes this data directory wrote -- it has been damaged or tampered with, and a floor read from it could be LOWER than the one persisted", MACKeyFileName))

	case strings.HasPrefix(field, indexFloorTagLegacy):
		want, derr := hex.DecodeString(strings.TrimPrefix(field, indexFloorTagLegacy))
		if derr != nil || len(want) != sha256.Size {
			return false, false, indexFloorCorrupt(path, fmt.Sprintf("its header digest %q is not %d hex bytes", clipFragment(field), sha256.Size))
		}
		if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
			return false, false, indexFloorCorrupt(path, fmt.Sprintf("it fails its own checksum (header says %x, body hashes to %x), so it is not the bytes that were written and a floor read from it could be LOWER than the one persisted", want, got[:]))
		}
		return true, false, nil
	}
	return false, false, indexFloorCorrupt(path, fmt.Sprintf("its header has no %s tag and no legacy %s digest (got %q)", indexFloorTagHMAC, indexFloorTagLegacy, clipFragment(field)))
}

// parseIndexFloorSealed reads the "sealed <0|1>" line.
//
// It is STRICT about the spelling for the same reason parseIndexFloorLine is:
// the digest is computed over the body, so a value with two spellings would be
// two different files claiming the same state. "true"/"false"/"yes" are
// therefore refused rather than accepted helpfully.
//
// An UNREADABLE sealed line is CORRUPT rather than silently read as 0. It would
// be tempting to fail safe here -- 0 is the conservative value -- but the rest
// of the file is not safe to read out of bytes that do not parse, and this line
// is inside the digested body, so if it is malformed the checksum has already
// passed on bytes nothing wrote.
func parseIndexFloorSealed(path, line string) (bool, error) {
	switch line {
	case "sealed 0":
		return false, nil
	case "sealed 1":
		return true, nil
	}
	if !strings.HasPrefix(line, "sealed ") {
		return false, indexFloorCorrupt(path, fmt.Sprintf("expected a %q line, got %q", "sealed", clipFragment(line)))
	}
	return false, indexFloorCorrupt(path, fmt.Sprintf("its sealed field %q must be exactly 0 or 1", clipFragment(strings.TrimPrefix(line, "sealed "))))
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

// indexFloorCorrupt builds the fatal error, appending the SAME operator remedy
// every time. The remedy matters as much as the diagnosis: without it this error
// is a permanently bricked bus, which is precisely what invariant 6 forbids.
//
// # The remedy STATES ITS COST, and that is not padding
//
// It used to end "delete %s and restart: the bus will then resume from the log's
// own high-water mark, WHICH IS CORRECT UNLESS THE LOG HAS ALSO BEEN DAMAGED OR
// QUARANTINED". That caveat was UNSOUND, not merely narrow, and it was unsound
// for exactly the reason the `sealed` bit exists: A TRUNCATION AT A CLEAN FRAME
// BOUNDARY IS BYTE-INDISTINGUISHABLE FROM A LEGITIMATELY SHORTER LOG. Neither
// recovery nor an operator has any signal that would show it. The old text
// therefore asked the operator to certify something nobody can know, and a
// security gate measured what happens when they get it wrong: floor deleted,
// crash, cut at a clean boundary, and the bus reissued an index at 2268 of 2289
// truncation offsets.
//
// CLAUDE.md requires an error to name the remedy rather than the stack. But a
// remedy that silently breaks the repository's most load-bearing invariant is
// worse than no remedy, so the text names the remedy AND names what it costs,
// and draws the line where it can actually be drawn: a run that CLOSED CLEANLY
// sealed the floor, and only in that case is the log's own high-water mark the
// whole truth.
func indexFloorCorrupt(path, why string) error {
	return fmt.Errorf("%w: %s: %s. CHECK %s FIRST: this file is authenticated under that key, so a key that is not this data directory's own makes an intact floor look corrupt, and the fix for that is to restore the key, NOT to touch this file. If the key is right, the file will NOT be regenerated, because regenerating it resumes the WAL record index (and therefore the message sequence derived from it) below numbers already handed out, which invariant 1 forbids and which nothing downstream can detect. Restore the file from a backup or a replica if you can. DELETING IT IS A LAST RESORT AND IT FORFEITS INVARIANT 1 FOR THIS DATA DIRECTORY unless the previous run shut down CLEANLY -- and note you CANNOT read that from this file, because the flag recording it lives in the very body that will not verify; you have to know how the bus stopped. If it did not stop cleanly, the bus would resume from the log's own high-water mark, and that mark is a lower bound rather than the answer whenever the log has lost its tail -- which cannot be detected, because a log truncated at a record boundary is byte-for-byte identical to a log that was simply shorter. If you delete %s, treat every WAL record index and message id this bus has issued as potentially reissued",
		ErrIndexFloorCorrupt, path, why, MACKeyFileName, path)
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
