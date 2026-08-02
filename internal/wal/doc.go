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
// recovery policy and does not belong in the framing layer.
package wal
