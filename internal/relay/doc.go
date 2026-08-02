// Package relay will own bus-to-bus federation.
//
// Buses exchange agent lists and forward messages addressed to a remote bus.
// Routing is unambiguous because every agent id is fully qualified as
// "<bus-id>.<agent-id>" (invariant 2), and remote-supplied ids are validated
// input, never trusted identity (invariant 1).
//
// Stub: the relay epic supplies the implementation.
package relay
