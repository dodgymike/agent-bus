// Package store owns the message record and the in-memory serving copy of the
// message stream.
//
// Memory is the serving copy; disk is the truth (invariant 5). State is held in
// memory for speed and rebuilt by replaying the durable store on start. Writes
// are acknowledged only once committed through the two-phase (prepare→commit)
// path and fsynced (invariant 4) — this package is on the far side of that
// ordering and does not enforce it: Store.Append is called by internal/hub only
// after the durable write has returned.
//
// Two things live here:
//
//   - Message and Record — one message, and its durable JSON encoding. Record
//     is deliberately shaped so DUR-5 can build the metadata-only audit log
//     from it without a format change: every field invariant 6 names is a
//     top-level field, and the ONE field the audit log must not copy (Body) is
//     last and is the only one to drop.
//   - Store — the retained stream. ONE ordered stream, not per-agent queues,
//     read with a per-agent cursor and filtered through Message.VisibleTo.
//     Retention is 1 day or 1 GiB, whichever comes first (user decision,
//     2026-08-02), and drops whole messages from the oldest end only.
//
// Since SIGN-1 a sequence is minted (and durably burned) BEFORE the client
// signs and sends, so messages do not arrive in sequence order. IDENTITY and
// DELIVERY ORDER are therefore two different numbers on a Message
// (SIGN-1-FU-REORDER-WATERMARK): Seq is the server-minted, client-signed
// identity, and Pos is the delivery position — the WAL commit index, which is
// what Store keeps its slice ordered by, what a cursor points at, and what
// Since binary-searches. A late, low sequence gets a HIGH position, lands at
// the tail of the delivery order, and is served to every reader; the split is
// what stops an acknowledged message being invisible to a reader that had
// already passed its sequence. Pos is DERIVED from where the record sits in the
// log and is not part of the durable record, so it cost no on-disk change.
//
// One consequence is written up where it lives: the narrowed duplicate
// DETECTION across the already-pruned region (Append's P1 and Store.bySeq).
//
// Nothing in this package interprets a message body. It is carried and hashed
// as opaque bytes, which is what lets the CRYPTO epic put ciphertext there
// without anything on this path changing.
package store
