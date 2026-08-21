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
//	bounded replay via WAL checkpoints                       ACK-8-FU-CHECKPOINTS
//
// ACK-8 DELIVERED PART OF §14 D1, AND THE PART IT DID NOT IS NAMED. D1 asks for
// reconstruction after a crash at ANY transition boundary; ACK-8 was NARROWED on
// 2026-08-21 to the LOCAL-ACCEPT and SETTLE boundaries, which are the two this
// package owns. Within those, it proved — against a real on-disk wal.Log rather
// than this package's fakeLog stub — that restart reconstructs every state
// exactly, that a settled row cannot be resurrected, that a torn tail AND a
// bit-flipped *acknowledged* record are each discarded LOUDLY while the bus
// still starts and the WAL index advances past the hole instead of rewinding,
// and that a SIGKILL at each of three points in the SETTLE write path recovers
// to a prefix of accepted history. See restart_ack8_test.go and
// crash_ack8_test.go.
//
// STILL OPEN under D1, so do not read the above as "D1 is closed":
//
//	the HOP boundary                    ACK-8-FU-HOPBOUNDARY
//	§14 D2 obligation_lost              ACK-8-FU-D2-OBLIGATIONLOST
//	bounding how LONG replay takes      ACK-8-FU-CHECKPOINTS (the checkpoint
//	                                    clause ACK-8's description also carried;
//	                                    it is WAL-wide, not ack's — see below)
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
// ACK-8-FU-CHECKPOINTS's, and it is WAL-wide: LogOptions.Checkpoints has ZERO
// production assignments, and assigning it TODAY would refuse every message
// publish and every enrolment, because log.go makes an unowned kind a hard
// error on write and checkpoint.go makes it a hard error on Apply).
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
