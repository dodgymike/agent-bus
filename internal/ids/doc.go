// Package ids will own identifier minting for the bus.
//
// The server is authoritative on every id (invariant 1): bus ids, agent ids,
// message ids and sequence numbers are minted here and never accepted from a
// client. Ids are never reused, including across restarts. Agent ids are
// always fully qualified as "<bus-id>.<agent-id>" (invariant 2).
//
// Stub: the ID epic supplies the implementation.
package ids
