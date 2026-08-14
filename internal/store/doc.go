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
// signs and sends, so messages do not arrive in sequence order and Store.Append
// inserts a late one into position rather than refusing it. Three consequences
// are written up where they live: the delivery gap that leaves a late arrival
// undelivered to a reader that has already passed it — filed as
// SIGN-1-FU-REORDER-WATERMARK, Spec Server task
// c829af9a-4418-437a-a0f8-34ef2f5d15d0 (Store, "KNOWN GAP"); the bounded softening of
// the age bound (pruneLocked); and the narrowed duplicate DETECTION across the
// already-pruned region (Append's P1 and the prunedHead branch).
//
// Nothing in this package interprets a message body. It is carried and hashed
// as opaque bytes, which is what lets the CRYPTO epic put ciphertext there
// without anything on this path changing.
package store
