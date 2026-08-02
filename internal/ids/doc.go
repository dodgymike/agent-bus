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
//     invariant 1 can be broken, and this package cannot detect it.
//   - messageid.go — the "<bus-id>-<seq>" message id format and its validator.
//
// The allocator has no callers yet. Deriving its resume floor from WAL recovery
// is a separate, deliberate wiring task.
package ids
