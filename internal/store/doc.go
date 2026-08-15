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
// # THREE NOTIONS, and they must not be conflated
//
// A THIRD number-like field joins those two, and it is not a variant of either:
//
//	Seq                IDENTITY — server-minted, client-signed, spendable out of
//	                   order.
//	Pos                DELIVERY POSITION — the WAL commit index; what cursors
//	                   point at, what the slice is ordered by, what Since
//	                   binary-searches.
//	OriginMessageID    CORRELATION KEY — which message on the ORIGIN bus this is
//	                   a local copy of, set only on a relay ingest, empty when
//	                   this bus is the origin (Message.OriginID).
//
// The correlation key takes part in NO ORDERING, NO CURSOR and NO RETENTION
// DECISION. It is indexed for point lookup (Store.ByID, Store.ByOriginMessageID
// — both server-internal routing lookups that do NOT apply Message.VisibleTo and
// must never be reached from a request handler) and is otherwise inert. Adding a
// fourth axis to the ordering of this package carelessly is how
// SIGN-1-FU-OUTOFORDER-POISON happened.
//
// Nothing in this package interprets a message body. It is carried and hashed
// as opaque bytes, which is what lets the CRYPTO epic put ciphertext there
// without anything on this path changing.
package store
