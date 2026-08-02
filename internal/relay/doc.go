// Package relay owns bus-to-bus federation. Its first slice is the peer
// handshake: the payload shapes, the validation of a peer's claimed roster, an
// http.Handler for the responder side, and a Client for the initiator side.
//
// # THE HANDLER IS DELIBERATELY NOT REGISTERED ON ANY MUX. DO NOT REGISTER IT.
//
// Handler is constructed, exercised by tests, and reachable from nowhere. It
// performs NO AUTHENTICATION OF THE PEER, and none is missing by accident: the
// two mechanisms that authenticate a peer bus are separate, unlanded tasks, and
// both must land BEFORE this handler is served on a listener:
//
//   - INVITE-PEERGUARD (spec task f5d91dbe) — redeeming an operator-minted,
//     single-use, expiring invite is the ONLY route onto the bus, INCLUDING for
//     peer buses (CLAUDE.md invariant 3). Until that gate exists, serving this
//     handler creates exactly the ungated federation-enrolment path that task
//     exists to forbid.
//   - MTLS-RELAYGUARD (spec task 8192c3c7) — bus-to-bus links are MUTUALLY
//     authenticated with TLS client certificates (CLAUDE.md invariant 11). A
//     peer bus needs BOTH an invite and mutual TLS; neither alone is sufficient.
//
// So: wiring relay.Handler into internal/httpapi (or any other mux) as part of
// a task that is not INVITE-PEERGUARD or MTLS-RELAYGUARD is a security
// regression, not a convenience. guards_test.go fails if any other package in
// the repository imports internal/relay, so the mistake is caught rather than
// relied upon to be remembered. RELAY-1 shipped the exchange; those two guard
// tasks ship the gate.
//
// What this package does provide, and what it is careful about:
//
// Ids. Every agent in a handshake roster is fully qualified as
// "<bus-id>.<agent-id>" (invariant 2) — that namespacing is the entire point of
// exchanging rosters, because it is what makes cross-bus routing unambiguous.
//
// Trust. A peer's bus id and roster are UNTRUSTED INPUT: validated, never
// believed (invariant 1). In particular a peer may only assert ids inside its
// OWN namespace. An incoming id whose bus half is our bus id — or differs from
// it only by ASCII case — is rejected as id spoofing, because the
// fully-qualified id is the routing and authorization subject, and a peer able
// to mint ids in our namespace could impersonate our agents to us. The rules
// are applied in BOTH directions: the initiator validates the responder's reply
// with the identical validator.
//
// Allocation. Every bound is enforced before the allocation it bounds. The body
// is read through an io.LimitReader — a Content-Length header is a claim, not a
// fact — and the roster length is checked before any per-entry parsing. See the
// Max* constants in peer.go for each cap and why it was chosen.
//
// Retries. The idempotency key that makes peer-enrol safe to retry
// (invariant 10) travels in the idem.HeaderName header — internal/idem's one
// canonical carrier, no body field and no fallback — and a validated PeerRoster
// carries both the key and an idem.ComputeFingerprint of the payload, which is
// what lets the eventual registration site tell a legitimate retry (same key,
// same payload) from a protocol violation (same key, different payload).
//
// # What the gating tasks must not forget
//
// Three properties this package cannot provide from where it sits, each of
// which becomes load-bearing the moment the handler is served:
//
//   - AcceptPeer is the ONLY thing between an anonymous POST and our whole
//     roster. The invite check belongs in front of the handler or inside that
//     callback — never after it.
//   - Two DIFFERENT peers whose bus ids collide only by ASCII case both
//     validate here, because ValidatePeerBusID case-folds against OUR id alone.
//     The peer registry must refuse a bus id that case-collides with a peer it
//     already knows, for the reason ids.ValidateAgentName refuses uppercase
//     names: a confusable in the routing subject is a social-engineering
//     surface.
//   - There is no rate limit, no per-request read deadline and no cap on
//     concurrent handshakes here; those belong to the serving layer.
//
// # No wire protocol version field, on purpose
//
// The handshake payload deliberately carries NO version number. Wire protocol
// versions are RESERVED through the Spec Server reservations API, never chosen
// by the agent writing the code (CLAUDE.md, "Parallel-agent coordination"), and
// no relay wire-protocol version has been reserved. Because nothing serves this
// handler the format is not yet on any wire, so there is nothing to stay
// compatible with. The task that first REGISTERS this handler must reserve a
// version and add the field; that is recorded as a RELAY-1 follow-up.
package relay
