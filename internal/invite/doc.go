// Package invite is the durable, bounded, SINGLE-USE invite record and its
// lifecycle — mint, lookup, redeem (consume), revoke, expire — written through
// internal/wal's two-phase path and rebuilt by replay.
//
// This is task INVITE-STORE. It is the STORE only: it registers no HTTP route
// (INVITE-GATE), ships no revocation surface (INVITE-REVOKE), and does not enrol
// anybody. What it owns is the state machine and its durability.
//
// The OPERATOR-FACING MINT landed separately as INVITE-MINT (2026-08-07): the
// `agent-bus invite mint` subcommand in cmd/agent-bus/invite.go, authorised by
// FILESYSTEM ACCESS to the data directory rather than by anything on the wire
// (DECISIONS.md, E4). It is a caller of Store.Mint and adds nothing to this
// package.
//
// INVITE-GATE (2026-08-14) MADE THE INVITE LIVE, AND DID NOT TURN THE GATE ON.
// A minted invite is no longer inert: an invite PRESENTED to POST /v1/enroll is
// REDEEMED, atomically with the enrolment it authorises. The composer is
// auth.Service.Enrol plus auth.WALRoster.PutWithInvite, which write the
// consumption record and the roster record as ONE wal.Entry of kind
// auth.EnrolInviteRecordKind ("agent+invite"). What has NOT changed is that
// enrolment is NOT YET GATED: an enrolment carrying NO invite is still accepted,
// and the discovery document says so. Requiring one (invariant 3's end state) is
// a separate task.
//
// # 1. Why it exists — build to this, not just to the API
//
// Every pre-auth attack this project has had to reason about shares one root
// cause: enrolment was UNAUTHENTICATED, so an attacker could mint its own
// agents. The invite removes that capability (invariant 3: enrolment is
// invite-only). Three consequences are requirements rather than nice
// properties:
//
//   - SINGLE USE IS THE PROPERTY EVERYTHING RESTS ON. If a restart forgets
//     which invites were spent, one invite is redeemable twice and the gate is
//     decorative. Durability is therefore not optional here and is not a later
//     optimisation — it is the feature.
//   - THE SECRET IS A BEARER CREDENTIAL AND THE INVITE BLOB IS THE TRUST ANCHOR
//     (DECISIONS.md, E6). The blob an agent receives carries bus id + address +
//     bus-certificate fingerprint + invite secret, so whoever substitutes an
//     invite points an agent at a bus of their choosing. The secret gets
//     session-token discipline: 32 bytes of crypto/rand, returned exactly once,
//     stored only as a SHA-256 digest, compared in constant time, never logged.
//   - REVOCATION DOES NOT REACH AN AGENT THAT ALREADY REDEEMED (DECISIONS.md,
//     E5). An invite is spent at redemption; cascading revocation of the
//     resulting agent credential is AUTH-4 and is explicitly not this package.
//     Revoke says so in its error rather than reporting a success that has no
//     effect.
//
// # 2. The model
//
// An invite is a Record: a server-minted id (invariant 1), the bus it admits
// to, the digest of its secret, a validity window, and — once spent — the
// terminal event that spent it. Three states: open, redeemed, revoked.
//
// "EXPIRED" IS NOT A STATE. Expiry is a pure predicate over ExpiresAt and the
// clock (Record.Expired), evaluated on every call and never stored, because a
// stored flag is one clock reading that immediately starts disagreeing with the
// clock — and keeping it true would mean rewriting records in an append-only
// log. Retention works the same way: the predicate is re-derived identically on
// the live path and the replay path, so memory and disk cannot drift.
//
// # 3. Durability
//
// wal.Entry.Kind = RecordKind ("invite"). Entry.Kind is a FREE-FORM APPLICATION
// DISCRIMINATOR, NOT a numbered frame type, so no record-type reservation is
// needed and internal/wal/format.go is untouched — noted here so the next
// reader does not go and reserve a number nothing requires.
//
// EVERY ENTRY CARRIES THE COMPLETE RECORD IN ITS POST-TRANSITION STATE, never a
// delta. Replay then needs no ordering logic beyond a monotonic upsert, and — a
// property a delta scheme cannot have — if an earlier record for an invite is
// discarded by recovery, a surviving LATER record still reconstructs the invite
// in its SPENT state. A delta scheme would leave it looking open, which is the
// one direction that produces a second redemption.
//
// The upsert is MONOTONIC: open -> redeemed and open -> revoked, nothing else.
// A record that would move an invite backwards is refused and logged loudly.
//
// # 4. Idempotency scope — THE decision recorded here
//
// THE SCOPE IS THE INVITE. The key namespace is (invite id, client idempotency
// key). Neither alternative works:
//
//   - PER-AGENT is impossible. Enrolment is what MINTS the identity; there is
//     no authenticated agent id at the moment the key is presented.
//   - BUS-WIDE is unsafe on an unauthenticated route: any caller could squat a
//     key ahead of a legitimate retry. (idem's bus-wide enrol scope is exactly
//     that shape, and idem's own doc records the risk as INVITE-GATE's to
//     close.)
//
// The invite is the right namespace because it is server-minted, single-use,
// and gated by a bearer secret — only the holder of that secret can write into
// it. And because it is single-use it holds at most ONE redemption record, so
// this namespace has no table to exhaust and none of the capacity concerns a
// per-key table carries.
//
// The triage on a redemption against a spent invite (invariant 10 — these must
// NOT be collapsed) is in Store.Begin's doc, and is the reason Begin returns an
// Outcome rather than a bool.
//
// # 5. THE FAIL-CLOSED RULE, AND ITS ONE EXCEPTION
//
// DROPPING A WHOLE INVITE MAKES IT UNKNOWN, AND AN UNKNOWN INVITE IS REJECTED.
// That covers expiry, the capacity bound, and a mint record discarded at
// replay. What such a drop CAN cost is availability — a still-valid invite
// becomes unusable — and the ability to answer a retry with its original
// result. It cannot produce a second redemption.
//
// THE EXCEPTION, STATED PLAINLY BECAUSE AN OVERSTATED SAFETY CLAIM IS WORSE
// THAN A NARROW ONE: losing a SPEND record while its MINT record survives is
// FAIL-OPEN. The invite is then present and OPEN, and it can be redeemed again.
// The two ways to reach it are a consumption record that will not decode at
// replay (Store.Apply) and one refused by the monotonic upsert for disagreeing
// with the existing record's identity; both are logged at ERROR naming the
// invite. Neither is reachable by corruption alone — internal/wal verifies every
// frame with a keyed HMAC-SHA256 and discards what fails, so a record that
// decodes is a record this bus wrote — which leaves a bug in this package as the
// realistic cause. It is bounded, not eliminated, and an operator seeing that
// ERROR line should revoke the named invite rather than assume the discard was
// safe.
//
// This is also why every entry carries the COMPLETE record (section 3): a
// surviving spend record reconstructs the spent invite on its own, so losing
// the MINT half is harmless in the direction that matters.
//
// THIS IS THE EXACT OPPOSITE OF idem's APPLIED-KEY TABLE, and the contrast is
// why the bound here is safe: forgetting an applied key fails OPEN and yields a
// SECOND EFFECT, which is why idem must never evict a live key. Forgetting an
// invite fails CLOSED. Both tables refuse rather than evict at their cap, but
// only one of them would be dangerous if it did otherwise.
//
// # 6. Redemption is a TWO-PHASE PARTICIPANT
//
// Redemption must be atomic with the effect it authorises — the roster write
// that creates the agent. A wal.Entry is exactly one transaction, so "atomic"
// means the consumption record and the roster record ride in the SAME entry.
// This package therefore exposes a participant (Store.Begin ->
// Redemption.Consume -> the caller's write -> Commit/Abort), not a one-shot.
//
// THE COMPOSER IS auth.Service.Enrol + auth.WALRoster.PutWithInvite
// (INVITE-GATE, 2026-08-14), writing one entry of kind
// auth.EnrolInviteRecordKind. Store.Redeem remains the standalone path and the
// enrolment route must NOT use it; see its doc, which is still true and points
// at the composer above.
//
// # 7. What this package does NOT do
//
//   - IT DOES NOT COLLAPSE ITS ERRORS. The sentinels in errors.go are distinct
//     because an operator needs to know which failure occurred. A CLIENT must
//     not: the set of answers is an oracle for "does this invite exist and is
//     it live". THE HTTP LAYER MUST COLLAPSE THEM into one indistinguishable
//     response — that is INVITE-HARDEN's task, and until it lands any handler
//     built on this package must do it itself.
//   - IT DOES NOT CLOSE EVERY TIMING SIDE CHANNEL. Store.Begin compares against
//     a per-store dummy digest for an unknown id, so the hash-and-compare work
//     is identical either way; a map lookup and the distinct sentinels above
//     still differ. See VerifySecret for the honest statement of what constant
//     time does and does not buy.
//   - IT DOES NOT VALIDATE THE CLIENT CERTIFICATE FINGERPRINT beyond its size.
//     Record.CertFingerprint is defined but unused from day one, deliberately,
//     so MTLS-BIND adds a check rather than a schema change.
package invite
