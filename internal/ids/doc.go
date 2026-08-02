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
//     floor is the one place invariant 1 can be broken, and re-minting an agent
//     id is worse than re-minting a message id because the agent id is the
//     routing and authorization subject.
//
// Neither allocator has any callers yet. Deriving the message-sequence resume
// floor from WAL recovery is a separate, deliberate wiring task; so is agent id
// minting, where wiring the minter into enrolment is AUTH-1 and restoring the
// per-name suffix floors from replay is AUTH-3. That sequence-resume wiring
// task must now also RaiseFloor from every floor source and then call Seal
// exactly once before the allocator serves its first Next.
package ids
