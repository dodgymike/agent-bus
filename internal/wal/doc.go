// Package wal owns the append-only write-ahead/audit log.
//
// Every message is written to this log (invariant 6). It is append-only in the
// strict sense during NORMAL operation: no in-place edits, and nothing is ever
// rewritten by the write path. Recovery is the exception, and the size of that
// exception changed on 2026-08-02 -- see "Recovery policy" below.
//
// On-disk layout (format version 1). All integers are big-endian so a hex dump
// reads left to right, and every checksum is CRC-32 Castagnoli.
//
//	file header, 16 bytes, written once at creation:
//	  [0:8]    magic "AGNTBUSW" (wal) or "AGNTBUSA" (audit)
//	  [8:12]   uint32 format version
//	  [12:16]  uint32 CRC32C over bytes [0:12]
//
//	record frame, a 20-byte header then the payload:
//	  [0:4]    uint32 payload length, at most MaxPayloadSize
//	  [4:12]   uint64 index, first record is 1, +1 per append, never reused
//	  [12:14]  uint16 record type
//	  [14:16]  uint16 reserved, written as 0, a non-zero value is corruption
//	  [16:20]  uint32 CRC32C over frame[0:16] ++ payload
//	  [20:...] payload, opaque to this package
//
// The checksum covers the length and the index as well as the payload, so a
// corrupted length is detected rather than acted on. CRC32C is an
// error-detecting code and NOT an integrity primitive: it is unkeyed, so a
// client who can get a chosen payload into the log can put bytes in it that
// recovery will accept as a record. Replacing it with a keyed MAC is a separate,
// reserved piece of work; until then, treat every checksum-based claim in this
// package as holding against ACCIDENT, not against an adversary.
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
//     implement, or a data directory another process is writing to -- still
//     refuses the start. None of those are damage, and "repairing" them would
//     destroy a file that is probably intact.
//
// The thing that keeps this honest is no longer "we never discard". It is
// "WE NEVER DISCARD SILENTLY". Every discarded region is reported in
// Repair.Discards or Recovered.Discarded and written to the operator log by
// RepairLog and Open -- at ERROR when what was lost had been acknowledged (a
// commit record) or cannot be identified, WARN otherwise. A discard that does
// not reach the log is a bug, and the crash-injection tests fail when one does
// not.
//
// # Damage does not cascade
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
//     its own checksum over the bytes actually present proves the payload is all
//     there. That is a genuine proof, and it is the one place the word is still
//     used in this package.
package wal
