// Package wal will own the append-only write-ahead/audit log.
//
// Every message is written to this log (invariant 6). It is append-only in the
// strict sense: no in-place edits and no truncation except a verified-corrupt
// tail during recovery. Recovery must yield a prefix of the accepted history --
// no torn records, no acknowledged-but-lost messages.
//
// Stub: the durability epic supplies the implementation.
package wal
