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
//   - suffixstore.go — DurableNameSuffixes (MSG-FU-SUFFIXFLOOR), the production
//     SuffixAllocator: it composes a NameSuffixes with a dedicated, atomically
//     replaced, fsynced file (<data-dir>/agent-suffixes, format version 3) and
//     writes floor[name] = n AHEAD of every suffix it hands out, so the durable
//     floor needs no derivation from WAL or roster state at all. Read its doc
//     before wiring it: it explains why a floor derived from committed history
//     is wrong (misses a suffix burned by a dangling prepare) and why a floor
//     derived from the roster is wrong (misses a departed agent's suffix) — the
//     same two holes NameSuffixes' own doc names below — and why writing the
//     floor ahead of the suffix removes the need to solve either.
//
// The allocators are at different stages of production wiring. Sequence's
// resume floor is derived, raised and sealed today — internal/hub does it on
// open (0bbbd27) — and it is the worked example of what every caller owes:
// RaiseFloor from every floor source, then Seal exactly once, before the
// allocator serves its first Next / NextSuffix. (Whether the floor hub derives
// is the RIGHT one is hub's claim to defend, not this package's.)
//
// NameSuffixes/DurableNameSuffixes do NOT have that wiring yet, and this is
// stated plainly rather than implied by the mechanism's existence.
// cmd/agent-bus/main.go:327 still builds a fresh ids.NewNameSuffixes() on every
// start, and a grep of the tree turns up ZERO production callers of
// OpenNameSuffixes anywhere outside this package. Agent ids are durable TODAY,
// inside WAL message bodies rather than inside any enrolment record:
// store.Message.Sender is a fully-qualified agent id on every message, and
// .Recipients holds them for a DIRECTED message (a broadcast is stored as a
// flag, so a broadcast-only recipient leaves no such trace). hub.publish encodes
// that record and writes it through the two-phase path, and the log has no
// compaction, so those bytes stay. A suffix burned by any agent that has sent a
// message, or been addressed by name, therefore outlives the restart, and the
// fresh counter still mints straight over it — the next start hands name-1 out
// again, to whatever keypair enrols first. That is re-minting a live agent id
// across restart, the exact failure invariant 1 forbids, and it is UNCHANGED by
// suffixstore.go landing: this package now HAS a mechanism that closes it
// (DurableNameSuffixes), but nothing in cmd/agent-bus calls it yet. Wiring
// main.go to OpenNameSuffixes — deriving legacy-dir backfill floors and calling
// RaiseFloor before the first Seal, exactly as hub already does for Sequence —
// is the remaining half of MSG-FU-SUFFIXFLOOR, tracked as a separate follow-up
// (AUTH-3 is the enrolment half of the same debt). Until it lands, do not read
// this package's contents as evidence the restart bug is fixed in production.
package ids
