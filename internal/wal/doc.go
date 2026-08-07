// Package wal owns the append-only write-ahead/audit log.
//
// Every message is written to this log (invariant 6). It is append-only in the
// strict sense during NORMAL operation: no in-place edits, and nothing is ever
// rewritten by the write path. Recovery is the exception, and the size of that
// exception changed on 2026-08-02 -- see "Recovery policy" below.
//
// On-disk layout (format version 2). All integers are big-endian so a hex dump
// reads left to right, and every tag is HMAC-SHA256 over the key described
// below.
//
//	file header, 48 bytes, written once at creation:
//	  [0:8]    magic "AGNTBUSW" (wal) or "AGNTBUSA" (audit)
//	  [8:12]   uint32 format version (2)
//	  [12:16]  uint32 reserved, written as 0
//	  [16:48]  HMAC-SHA256(key, header[0:16])
//
//	record frame, a 48-byte header then the payload:
//	  [0:4]    uint32 payload length, at most MaxPayloadSize
//	  [4:12]   uint64 index, first record is 1, +1 per append, never reused
//	  [12:14]  uint16 record type
//	  [14:16]  uint16 reserved, written as 0, a non-zero value is corruption
//	  [16:48]  HMAC-SHA256(key, frame[0:16] ++ payload)
//	  [48:...] payload, opaque to this package
//
// THE COVERED RANGE IS EXACTLY: the 16 header bytes (length, index, type,
// reserved) followed by every payload byte. The LENGTH FIELD IS INSIDE IT, which
// is what kills the length-inflation class of damage and attack: a corrupted or
// crafted length is detected rather than acted on. The concatenation needs no
// separator because the length is the first four covered bytes, so no two
// distinct records share a covered byte string.
//
// The FILE HEADER's MAC is deliberately also the KEY CHECK VALUE: it is how a
// wrong key is detected before a single record is touched. It authenticates 16
// CONSTANT bytes, so it does not bind the header to one particular file. That is
// an accepted limit, not an oversight -- an attacker who can rewrite the file can
// read the key next to it.
//
// # The MAC key
//
// One file per data directory: MACKeyFileName ("wal-mac.key"), mode 0600, 64
// lowercase hex characters (32 random bytes from crypto/rand). It is a function
// of the log's DIRECTORY, not its name, and the WAL and the audit log share it.
//
// It is GENERATED whenever the key file is absent, EXCEPT on a log that
// positively identifies itself as format version 2 -- our magic, version field
// 2 -- and is longer than its own file header; there an absent key is FATAL.
// That is deliberately the same condition recovery uses to raise
// ErrMACKeyMismatch: one predicate, two errors, for a key that is missing
// versus one that is merely wrong. Every other state (absent, zero-length,
// version 1, too short, garbage magic) can only reach quarantine, which renames
// the file aside without destroying a byte, so the fatal buys nothing there. A
// key file that exists but is malformed or unreadable is FATAL and is NEVER
// silently replaced.
//
// A MISSING OR WRONG KEY IS FATAL, and that is a DELIBERATE EXCEPTION to the
// always-restart policy below (user decision, DECISIONS.md 2026-08-02). The
// reasoning: a wrong key makes EVERY record fail verification, so "discard the
// unverifiable" would destroy the whole log over a misconfiguration.
// Always-restart exists to stop MEDIA DAMAGE holding the bus hostage; a wrong key
// is not media damage and is fixable in seconds. Recovery distinguishes the two
// by evidence, not guesswork: if the header MAC fails but ANY record verifies,
// the key is proven right and the header is simply rebuilt (nothing is lost); if
// the header MAC fails and not one record verifies anywhere, that is what a wrong
// key looks like and recovery refuses, naming both paths. The accepted cost is
// that a genuinely destroyed version 2 log needs one manual `mv` instead of
// self-quarantining.
//
// WHAT THE MAC BUYS, exactly. It COMPLETELY defeats the attack that motivated it:
// an ordinary enrolled client crafting a payload whose tag makes damage look like
// a complete record, or planting a frame header inside a message body for the
// forward search to find. A client cannot compute a tag over a key it does not
// hold. It buys NOTHING against an attacker who already has data-directory WRITE
// access -- the key file is in that directory. Accepted, recorded, and stated in
// PROTOCOL.md rather than discovered later.
//
// # The durable record-index floor
//
// One file per data directory: IndexFloorFileName ("wal-index-floor"), mode
// 0600, on-disk format version 4 (RESERVED in the Spec Server
// `ondisk-format-version` namespace, not chosen by eye -- 1 and 2 are the WAL
// frame format above, 3 is ids/agent-suffixes). A header line carrying an
// HMAC-SHA256 TAG, then three lines:
//
//	agent-bus-wal-index-floor v4 hmac-sha256=<64 hex>
//	reserved <decimal uint64>
//	written  <decimal uint64>
//	sealed   <0|1>
//
// THE TAG IS KEYED WITH THE DATA DIRECTORY'S OWN wal-mac.key -- the same key
// every WAL frame is authenticated under -- and covers the version line plus the
// body, i.e. the whole file except the tag field. Invariant 6 requires a keyed
// MAC here and never an unkeyed checksum, and the reason is concrete: under the
// unkeyed SHA-256 this file briefly carried, flipping `sealed 0` to `sealed 1`
// and recomputing the digest BY HAND -- reading no key, touching no log byte --
// made the reopened bus reissue indices at 2268 of 2289 truncation offsets, with
// every frame it then wrote carrying a valid MAC because the server computes it.
//
// VERSION 4 READS TWO OLDER SHAPES and writes only the current one: an unkeyed
// `sha256=` header over a three-line body, and the TWO-LINE body that commit
// f56c723 (which is in main) writes. Both are read with `sealed` forced to
// FALSE, because an unkeyed digest cannot support a trust claim, and both are
// rewritten with a keyed tag at the next begin. See indexfloor.go.
//
// `reserved` is the highest index this data directory has EVER AUTHORISED;
// `written` is the highest index that is BURNED -- written to the log or
// permanently skipped by recovery. Both are strictly non-decreasing, always
// written <= reserved, and NEITHER IS EVER LOWERED.
//
// `sealed` says whether THE RUN THAT WROTE THE FILE CLOSED CLEANLY, and it is
// the one field that goes down: begin fsyncs it to 0 before the Writer may
// append anything, and only a clean Writer.Close sets it to 1. When it is 1,
// `written` is EXACT and Open trusts it; when it is 0, Open resumes above
// `reserved` instead. Clearing it can only ever make the next start MORE
// conservative, which is why it does not contradict "no field that can rewind a
// floor" -- there is still no clean-shutdown optimisation that RELEASES a
// reservation, and both numeric fields remain monotonic.
//
// It exists because deriving the next index from the log ALONE reissues ids, in
// two ways that were both reported as defects and both reversed by the user:
// one past the highest SURVIVOR hands a discarded tail record's index straight
// back out, and a QUARANTINE resets the whole index space to 1. internal/hub
// derives the message-sequence floor from the recovered high-water mark, so both
// reissued MESSAGE IDS too. Keeping the floor OUTSIDE the log makes invariant 1
// structural rather than something every repair path must remember: no
// truncation, rewrite or quarantine can lower a number that was never in the
// file. Open resumes above the maximum of the replayed high-water mark, what the
// repair pass observed, and this floor's `written` -- plus, whenever `sealed` is
// 0, this floor's `reserved`.
//
// The floor is written AHEAD of the index it authorises, in blocks of
// indexReserveBlock (64). A floor write is a temp file + fsync + rename +
// directory fsync -- roughly THREE sync operations, not one -- so the amortised
// cost is about 3 syncs per 64 appends, call it ~5% on the send path's sync
// count. Both figures are UNMEASURED arithmetic, not a profile.
//
// The price of the block is that a crash burns up to 63 unused indices, which
// appear as a HOLE in the log's index sequence. That happens on EVERY crash, not
// only a damaged one, because the ceiling is keyed on `sealed` rather than on
// detected damage -- see indexfloor.go for why detected damage can never be a
// sound trigger. A CLEAN close/reopen burns nothing. Holes are legal, permanent
// and correct -- invariant 1 beats gap-freeness.
//
// A MISSING floor file is benign (a data directory written by a binary that
// predates it) and yields a zero floor with a WARN; making it fatal would brick
// every deployed bus on upgrade, which is the bricking upgradeV1 exists to
// avoid. A file that EXISTS but does not verify UNDER THIS DIRECTORY'S OWN KEY
// is FATAL, wraps ErrIndexFloorCorrupt, and is NEVER regenerated -- regenerating
// resumes the index below numbers already handed out, silently, and nothing
// downstream can detect that. That is the same narrow exception already granted
// for the MAC key and the persisted bus id: damage to the directory's IDENTITY,
// not to the log. A crash cannot produce it -- the write is temp file, fsync,
// rename, directory fsync -- so it means media damage or tampering.
//
// The error names a remedy AND ITS COST. It used to say "delete it and restart;
// the bus resumes from the log's own high-water mark, which is correct unless
// the log has ALSO been damaged". That caveat is UNSOUND: a log truncated at a
// record boundary is byte-for-byte a shorter log, so nobody can satisfy it, and
// following it after a crash reissued an index at 2268 of 2289 measured
// truncation offsets. It now says plainly that deleting the file FORFEITS
// INVARIANT 1 for that data directory unless the previous run closed cleanly.
//
// A file that cannot be verified because the KEY IS GONE -- the directory had no
// wal-mac.key at open and recovery minted one -- is a RE-FOUNDED DIRECTORY, not
// a damaged floor. It is read WITHOUT authentication (numbers kept, since they
// are only ever consumed as a raise; `sealed` discarded) and logged at ERROR.
// Refusing there would brick a bus over a lost key in a directory recovery has
// already decided may be re-founded, and an attacker who could forge that file
// could equally DELETE it, which no MAC can prevent -- what the MAC buys is that
// the forgery is no longer SILENT.
//
// THE FLOOR IS READ AFTER THE MAC KEY IS SETTLED, not before. Reading it first
// made a merely WRONG key surface as a corrupt floor, sending the operator to
// delete it when the fix was to restore the key.
//
// # Format version 1, and the upgrade
//
// Version 1 was the same shape with UNKEYED CRC32C tags: a 16-byte file header
// (CRC over [0:12] at [12:16]) and a 20-byte frame header (CRC over
// frame[0:16] ++ payload at [16:20]). It is still READ, so that an existing bus
// is not bricked by the version bump, and it is never written to.
//
//	A VERSION 1 LOG IS VERIFIED WITH CRC32C, REPAIRED IF DAMAGED WITH THE
//	VERSION 1 CODEC, THEN CONVERTED ONCE TO VERSION 2 AT STARTUP.
//
// A FILE IS ENTIRELY ONE VERSION. There is never a mixed version-1-then-version-2
// file: the version lives in the file header, and a version 2 writer never emits
// a version 1 frame. The conversion (upgradeV1) carries indices, types and
// payloads across BYTE FOR BYTE -- nothing is renumbered, because invariant 1
// forbids reusing an id -- writes to a temporary file, VERIFIES the result by
// re-scanning it and comparing a digest of every record, keeps a hard-linked
// backup where it can, and only then renames. The original is untouched until
// that rename, so a crash simply re-runs the whole upgrade.
//
// Writer is the append-only writer: its Append does not return until the bytes
// are fsynced (invariant 4). ScanAll is the strict reader: any malformed frame
// is an error, because deciding what to do about damage is recovery policy and
// does not belong in the framing layer. Replay turns a raw frame stream into
// accepted history.
//
// # Recovery policy: the bus always restarts
//
// The user's decision of 2026-08-02 (DECISIONS.md, "Availability over
// retention") is: "always be able to restart, prefer to discard messages and/or
// corruption, with logging". It REVERSED what this package used to do, which
// was to refuse to start on most damage. The rule now is:
//
// DAMAGE IS NEVER FATAL. NOT BEING ABLE TO READ THE FILE STILL IS.
//
//   - DAMAGE -- a torn frame, a flipped bit, a lost sector, a payload that no
//     longer decodes, a record type with no meaning here, a corrupt file header
//     -- is DISCARDED, LOGGED with its offset, record index, type and length,
//     and recovery continues to a running server.
//   - CANNOT READ -- permission denied, an I/O error from the device, an audit
//     file where a WAL was expected, a format version this binary does not
//     implement, a MISSING OR WRONG MAC KEY (see above), or a data directory
//     another process is writing to -- still refuses the start. None of those are
//     damage, and "repairing" them would destroy a file that is probably intact.
//
// The thing that keeps this honest is no longer "we never discard". It is
// "WE NEVER DISCARD SILENTLY". Discarded regions are reported in
// Repair.Discards or Recovered.Discarded and written to the operator log by
// RepairLog and Open -- at ERROR when what was lost had been acknowledged (a
// commit record) or cannot be identified, WARN otherwise. A discard that does
// not reach the log is a bug, and the crash-injection tests fail when one does
// not.
//
// The DETAIL is capped and the TOTALS are not, which is the exact claim: at
// most maxDiscardsRetained regions are retained for the caller and at most
// maxDiscardsLogged are named one by one, so a file that is damage from end to
// end cannot be held in memory as error text nor turn one restart into a
// hundred thousand log lines -- but the exact count and byte total are always
// emitted. "How much was lost" is never capped; only "which ones are described
// individually" is.
//
// # Damage does not cascade, with one bounded exception
//
// Discarding the DAMAGED record is sanctioned. Deleting later records that are
// themselves intact is not. After damage, recovery searches FORWARD for the
// next intact record by RECORD INDEX and resumes there (resyncFrom); it does
// not treat the first damage it meets as the end of the log. Anchoring that
// search on the index rather than on the end of the file is load-bearing: an
// end-of-file anchor only fires when the file ends exactly on a record
// boundary, which is precisely the case recovery does not exist for, and with
// one a reviewer's probe showed a single flipped bit in a mid-file length field
// deleting eight committed records.
//
// The search also runs in TWO STAGES, and the second one exists because the
// first was measurably wrong. Stage one narrows candidates by an index-density
// window, which is cheap and sharp; stage two repeats the scan without it. A
// repaired log has permanent index HOLES, so after a large hole the true next
// record's index exceeds what the density window allows, and with stage one
// alone the genuine record was rejected, the search reported "nothing follows",
// and recovery deleted every committed record to the end of the file WHILE
// LOGGING THAT IT HAD FOUND A TORN TAIL. The rule that came out of that, and
// that must not be relaxed: A BOUNDED SEARCH FINDING NOTHING IS NEVER ON ITS
// OWN GROUNDS FOR "NOTHING FOLLOWS".
//
// THE EXCEPTION: the search has a work budget, because the bytes it walks are
// attacker-influenced. A region dense enough with frame-like headers to exhaust
// it makes the search give up, and everything from the damage to the end of the
// file is then discarded without proof that any of it was unreadable. It is
// reported in Repair.Exhausted and logged at ERROR twice. That is the only way
// one damaged record can still cost an intact one.
//
// # What recovery does to the file, in order (see Open)
//
// First RepairLog, a framing-only pass:
//
//   - nothing, when the file scans clean -- the fast path, and the normal one;
//   - a TRUNCATION, when the only damage runs to the end of the file;
//   - a REWRITE, when damage sits in the middle, or the file header needs
//     rebuilding, or a record's length field was restored: surviving frames are
//     copied to a temporary file and renamed over the original, atomically, so
//     a crash mid-repair leaves the original untouched;
//   - a QUARANTINE, when the file cannot be interpreted at all and nothing can
//     be salvaged: it is RENAMED aside -- never deleted -- and startup
//     continues with a fresh log.
//
// Survivors KEEP THEIR ORIGINAL INDICES through a repair. Renumbering would
// reuse ids, which invariant 1 forbids, so a repaired log has permanent HOLES
// in its index sequence and scanFrom accepts a rising index rather than a dense
// one. Every hole is counted into Recovered.MissingRecords and logged on EVERY
// start, so a record lost to a bad sector cannot become a quiet startup.
//
// Then Replay, from the beginning. A COMMIT record makes its entry visible; a
// COMMIT is paired to the PREPARE it names BY INDEX, never by adjacency, since
// nothing in the format requires them to be neighbours. A prepare with no
// commit is discarded -- the ordinary crash artefact, since Append fsyncs whole
// frames, so the usual artefact is a complete, uncommitted prepare rather than a
// torn record -- and so is a prepare followed by an ABORT; either way an
// uncommitted prepare is never visible after a restart. Records whose CONTENT
// cannot be interpreted are discarded and reported rather than stopping the
// replay.
//
// Entries are replayed in COMMIT order, not prepare order, which is the order
// they were applied to memory before the crash, so the post-restart Apply
// sequence is identical to the pre-crash one.
//
// # The guarantee, stated narrowly enough to be true
//
// This package used to claim that a discarded frame was "only ever discarded
// when its fsync provably never completed", and called its checks "a proof
// rather than a heuristic". Both reviewer and security flagged that as false,
// and it is now false by policy as well as in fact. The honest statement:
//
//   - Nothing is lost through THIS PACKAGE'S OWN WRITE PATH. Append returns
//     only after its fsync, one frame is in flight at a time, and a failed
//     write poisons the Writer, so an acknowledged record is always fully on
//     disk before anyone hears about it.
//   - Recovery WILL discard acknowledged data when it finds that data damaged.
//     A single flipped bit in the payload of a complete, fsynced, acknowledged
//     final record is byte-indistinguishable from a torn write, and nothing in
//     this format can separate them. That record is discarded, its index is
//     reissued, and both facts are logged at ERROR. Invariant 4 is narrowed
//     accordingly and deliberately: the bus will not be held hostage to damaged
//     media.
//   - What is still protected is what invariant 1 protects: an id that was
//     OBSERVED is not handed out twice while the record carrying it survives,
//     because survivors are never renumbered. The exposure is bounded to the
//     tail -- a damaged record with an intact record behind it does not move
//     the high-water mark, because the search resumes at the survivor.
//   - A record whose LENGTH FIELD alone is corrupt is RECOVERED, not discarded:
//     its own tag over the bytes actually present is overwhelming evidence that
//     the payload is all there. Under version 1 that held against ACCIDENT only
//     -- a 32-bit CRC collides by chance about once in 2^32 wrong lengths, and
//     being unkeyed it could be collided on PURPOSE by anyone choosing payload
//     bytes. Under version 2 the tag is keyed, so a chosen-payload collision is
//     not something a client can compute: the length-field repair is now a real
//     proof against an adversary as well as against accident. The limit that
//     remains is the one stated above -- it says nothing about someone who can
//     already write to the data directory and read the key.
package wal
