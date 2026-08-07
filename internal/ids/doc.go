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
// NameSuffixes/DurableNameSuffixes HAVE that wiring now, and this paragraph is
// corrected accordingly: it used to say cmd/agent-bus/main.go built a fresh
// ids.NewNameSuffixes() on every start and that a grep of the tree turned up
// ZERO production callers of OpenNameSuffixes. Both are now false.
// ids.NewNameSuffixes() has been removed from cmd/ entirely — the call site
// that used to build it, in main.go's run(), now carries a comment stating it
// "is gone from cmd/ and MUST NOT come back, on any path, including as a
// fallback for a failed open or a failed seal." cmd/agent-bus/suffixfloors.go's
// openSuffixAllocator calls OpenNameSuffixes(dataDir) on every startup, deriving
// and raising legacy-dir backfill floors before the first Seal exactly as hub
// already does for Sequence, and main.go's run() calls openSuffixAllocator
// ahead of the agent-id minter and the auth service. Agent ids are durable
// TODAY for two independent reasons that both now hold: inside WAL message
// bodies (store.Message.Sender is a fully-qualified agent id on every message,
// and .Recipients holds them for a DIRECTED message; hub.publish writes that
// record through the two-phase path and the log has no compaction, so those
// bytes stay), and — since this wiring landed — inside the dedicated,
// write-ahead agent-suffixes file that persists and fsyncs floor[name] = n
// BEFORE the suffix it authorises is ever handed out. A restarting bus
// therefore does NOT re-mint a live agent id: the next start resumes strictly
// above every suffix this data directory has issued. The one remaining
// operator-facing gap is a data directory that predates the agent-suffixes
// file and has history but no floors file — that case REFUSES to boot unless
// the operator passes -backfill-suffix-floors, a one-time migration opt-in
// documented in CONTRACTS.md's MSG-FU-SUFFIXFLOOR entry and in
// DECISIONS.md (2026-08-07, "Four rulings: refuse-to-boot exception, format
// break, binary rename, redeploy", §1). Read this package's contents as
// evidence the restart-reuse bug IS fixed in production, with that one migration
// case as the documented exception.
package ids
