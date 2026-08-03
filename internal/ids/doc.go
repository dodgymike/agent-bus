// Package ids will own identifier minting for the bus.
//
// The server is authoritative on every id (invariant 1): bus ids, agent ids,
// message ids and sequence numbers are minted here and never accepted from a
// client. Ids are never reused, including across restarts. Agent ids are
// always fully qualified as "<bus-id>.<agent-id>" (invariant 2).
//
// What is here so far:
//
//   - busid.go   — minting and persisting this bus's own id (ID-1).
//   - sequence.go — the strictly monotonic message-sequence allocator (ID-2).
//     Read its doc before wiring it: the resume floor is the one place
//     invariant 1 can be broken, and this package cannot detect it. The
//     allocator is born UNSEALED and refuses Next with ErrFloorUnproven until
//     Seal is called, so a wrong floor now fails closed instead of silently
//     minting — Seal proves only that a floor CLAIM was made, not that the
//     claim is correct.
//   - messageid.go — the "<bus-id>-<seq>" message id format and its validator.
//   - agentid.go  — the "<bus-id>.<name>-<n>" agent id format, the legal-name
//     rule (AgentNamePattern) and its validator (ID-3).
//   - agentmint.go — the per-name suffix allocator and the minter that turns a
//     client's requested name into a fully-qualified agent id (ID-3). Read
//     NameSuffixes' doc before wiring it: like the message sequence, its resume
//     floors are the one place invariant 1 can be broken, and re-minting an
//     agent id is worse than re-minting a message id because the agent id is the
//     routing and authorization subject. The allocator now refuses NextSuffix
//     with ErrFloorUnproven until Seal is called, so wrong floors fail closed
//     instead of silently minting. The seal is GLOBAL, not per name: while
//     unsealed NO name may issue, and sealing asserts something about the names
//     ABSENT from the floor map too — that they were never written — which is
//     the proof obligation point 7 of that doc demands. Its two constructors
//     differ deliberately: ResumeNameSuffixes is born UNSEALED and must Seal,
//     while NewNameSuffixes is the FRESH-BUS constructor and is born SEALED,
//     because it has a live caller (cmd/agent-bus/main.go) that must keep
//     enrolling.
//
// Both allocators still owe their resume wiring, and one of those debts is
// already overdue rather than merely pending. cmd/agent-bus/main.go builds a
// fresh NewNameSuffixes on every start, which would be sound only while no agent
// id reaches disk — and that is NOT the state of this tree. Agent ids are
// durable TODAY, inside WAL message bodies rather than inside any enrolment
// record: store.Message.Sender and .Recipients are fully-qualified agent ids,
// hub.publish encodes that message and writes it through the two-phase path, and
// the log has no compaction, so those bytes stay. A suffix burned by any agent
// that has sent or received a message therefore outlives the restart, and the
// fresh counter mints straight over it — the next start hands name-1 out again,
// to whatever keypair enrols first. That is re-minting a live agent id across
// restart, the exact failure invariant 1 forbids, and it is tracked as P0
// MSG-FU-SUFFIXFLOOR, which must switch main.go to ResumeNameSuffixes with
// floors derived from replay (AUTH-3 is the enrolment half of the same debt).
// Deriving the message-sequence resume floor from WAL recovery is the matching,
// separate wiring task for Sequence. Whatever the call sites look like by the
// time you read this, the obligation on each is identical: RaiseFloor from every
// floor source, then Seal exactly once, before the allocator serves its first
// Next / NextSuffix.
package ids
