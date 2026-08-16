// Package ack is the DURABLE SENDER-VISIBLE DELIVERY LIFECYCLE TABLE (ACK-2).
//
// It answers exactly one question, for one (message, recipient) pair: what does
// the SENDER'S OWN BUS durably know about what happened to this message. The
// specification is ACK-CONTRACT.md §3 (correlation key), §7 (the record), §8
// (the state machine) and §11 (retention); every ruling below is that
// contract's, not this package's, and none of them may be re-litigated here.
//
// # THE THREE PLANES, AND WHICH ONE THIS IS
//
// The whole ACK epic exists because three different facts were being collapsed
// into one word:
//
//	A. LOCAL ACCEPTANCE   this bus committed and fsynced the message (invariant 4)
//	B. PEER-HOP RECEIPT   the next bus took responsibility for a copy
//	C. RECIPIENT DELIVERY the addressed agent's application accepted it
//
// This table records the SENDER-VISIBLE state, which is planes A and C. Plane B
// is already recorded, per hop, by relay.Outbox, and it is a DIFFERENT table on
// purpose:
//
//	A HOP ACK DOES NOT ADVANCE THE STATE IN THIS TABLE. EVER.
//
// That sentence is the point of the epic (ACK-CONTRACT.md §8.2 note 3, §8.4). A
// peer answering 200/accepted settles this bus's obligation TO THAT PEER and
// moves the sender's view of delivery not at all. There is deliberately NO
// method on Store that a hop ACK could call: the absence is the enforcement.
// If you are about to add one, you are about to re-create the conflation this
// package exists to prevent.
//
// # WHAT IS AND IS NOT WIRED IN THIS BUILD
//
// Wired: the E1 transition. A LOCAL, NON-BROADCAST send records one `accepted`
// row per recipient, after the message is durable, through Hub.Options.Acks
// (internal/hub/hub.go). Nothing reads those rows yet — GET /v1/ack is ACK-9 —
// so this table is currently WRITE-ONLY, and that is expected rather than a
// defect: the row has to exist before anything can report it.
//
// NOT wired, and each belongs to a named later task:
//
//	E2 in_flight        a pending outbox job exists          ACK-5
//	E4 undeliverable    a routing failure is final           ACK-4 / ACK-5
//	E5 delivered        the recipient application ACKed      ACK-6
//	E6 refused          the recipient application NACKed     ACK-6
//	relayed ingest      an intermediate bus's rows           ACK-5
//	crash reconstruction beyond replay                       ACK-8 (§14 D1)
//
// The METHODS for E2/E4/E5/E6 exist and are tested here, because a state
// machine that is only half-expressible cannot be reviewed as a state machine.
// They have no production caller yet.
//
// # CHECKPOINTS ARE NOT INVOLVED, AND THAT IS DELIBERATE
//
// The log is opened with Applier: and never Checkpoints:
// (cmd/agent-bus/main.go). Durability here is exactly wal.Log.Write's
// Begin(fsync) -> Commit(fsync) (internal/wal/log.go:799-815), whose own
// comment states that a nil error means the entry is on stable storage AND
// visible in memory, and only then may the caller acknowledge it. That is
// invariant 4 in full, and it is checkpoint-independent. This package
// implements no CheckpointParticipant on purpose: seven of the eight production
// kinds have none, and adding one here would not change that debt (it is
// ACK-8's).
//
// # THREE PROPERTIES, THE SAME THREE relay.Outbox AND invite.Store DOCUMENT
//
//  1. Apply is the only writer on the replay path, and the LIVE path folds in
//     the SAME canonical record — literally DecodeRecord(Encode(r)) — so a live
//     apply and a replayed apply cannot drift.
//  2. THE LOCK IS NEVER HELD ACROSS A DURABLE WRITE. wal.Log.Write calls
//     Applier.Apply synchronously and this Store IS that applier, so holding mu
//     across the write would self-deadlock. Every mutating method decides under
//     the lock, RESERVES what it decided, releases, writes, and folds in.
//  3. The upsert is MONOTONIC and terminal is ABSORBING. See upsertLocked.
package ack
