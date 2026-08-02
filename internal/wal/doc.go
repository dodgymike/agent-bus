// Package wal will own the append-only write-ahead/audit log.
//
// Every message is written to this log (invariant 6). It is append-only in the
// strict sense: no in-place edits and no truncation except a verified-corrupt
// tail during recovery. Recovery must yield a prefix of the accepted history --
// no torn records, no acknowledged-but-lost messages.
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
// corrupted length is detected rather than acted on.
//
// Writer is the append-only writer: its Append does not return until the bytes
// are fsynced (invariant 4). ScanAll is the strict reader: any malformed frame
// is an error, because deciding that a corrupt TAIL may be truncated is
// recovery policy and does not belong in the framing layer. Replay is the
// recovery layer built on top of ScanAll: it turns a raw frame stream into
// accepted history.
//
// # Recovery
//
// On start Open runs two passes over the file, in this order, both before the
// writer is opened, so nothing can be appended ahead of recovery.
//
// First RepairTail: a framing-only check that verifies the file header and
// every frame, and truncates the file back to the end of the last verified-good
// record if -- and only if -- the damage is provably a torn tail, meaning a
// single incomplete frame at the very end with nothing after it. That
// truncation is fsynced, and it is the ONLY truncation this package ever
// performs: invariant 6 allows exactly one exception to append-only, "a
// verified-corrupt tail during recovery", and this is it. Nothing acknowledged
// can be lost by it, because Append returns only after its fsync, so a torn
// frame is one whose write never succeeded and whose contents were never
// visible to anybody. Corruption ANYWHERE ELSE -- in a frame whose checksum
// verified, in the file header, or anywhere with intact records after it -- is
// a refusal to start, not a repair. See RepairTail for the classification rules
// and for the deliberately conservative cases it declines to touch.
//
// Then Replay, from the beginning (see Replay). A COMMIT record makes its entry
// visible; a COMMIT is paired to the PREPARE it names BY INDEX, never by
// adjacency, since nothing in the format requires them to be neighbours. A
// prepare with no commit is discarded -- the
// ordinary crash artefact, since Append fsyncs whole frames, so the usual
// artefact is a complete, uncommitted prepare rather than a torn record -- and
// so is a prepare followed by an ABORT; either way an uncommitted prepare is
// never visible after a restart.
//
// Entries are replayed in COMMIT order, not prepare order, which is the order
// they were applied to memory before the crash, so the post-restart Apply
// sequence is identical to the pre-crash one. The high-water index reported by
// replay is above every index ever written, including one burned by a
// discarded prepare, so an index is never reissued -- a reused index would let
// two different messages share an id.
//
// Replay is strict: a record it cannot interpret stops recovery and refuses
// the start, rather than silently skipping it and serving a state that might
// be missing an acknowledged write. The corrupt-tail policy -- verifying and
// truncating a torn tail -- is NOT implemented here: it is RepairTail, and it
// has already run, before replay. A torn tail therefore does not reach Replay
// through Open at all; when Replay is called directly on one it still fails, and
// reports the offset where the good prefix ends.
// That offset is where a torn tail would be cut, but it is NOT a licence to
// truncate on its own: most replay failures are damage in a frame whose
// checksum verified, which can sit anywhere in the file with committed records
// after it, and cutting there would delete accepted history. CorruptError
// carries FrameIntact and FrameEnd so recovery can tell the two apart.
package wal
