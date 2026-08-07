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
// Allocation. Every bound is enforced before the allocation THIS PACKAGE makes,
// and the precise wording matters because an earlier version of this paragraph
// overclaimed. The body is read through an io.LimitReader — a Content-Length
// header is a claim, not a fact — and the roster, recipient and bus-path lengths
// are checked before any per-entry PARSING. What that does NOT mean is that no
// slice is materialised first: encoding/json allocates the whole decoded
// []string before any count check of ours runs, so the real bound on decode-time
// memory is the PRE-DECODE BYTE CAP (MaxHandshakeBytes, MaxRelayBytes,
// MaxRosterUpdateBytes), and the count caps bound the per-entry WORK and the
// retained footprint, not the transient decode. See the Max* constants in
// peer.go and message.go for each cap and why it was chosen.
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
//     concurrent handshakes here; those belong to the serving layer. The same is
//     true of the relay and roster surfaces, where it costs MORE: every
//     acceptance there is a durable fsync.
//
// RELAY-2 and RELAY-3 add seven more, and they are listed in full because THIS
// LIST IS THE HANDOFF. A gate task reads it and nothing else; a property that is
// only in a field comment somewhere is a property the gate will not implement.
// Each was raised by the security gate on 2026-08-07 and each is P1 now, P0 the
// moment a handler is served:
//
//  1. SIGN-6 IS A PREREQUISITE OF SERVING RelayHandler, NOT A FOLLOW-UP.
//     RelayRequest carries no signature field, so nothing binds an envelope to
//     the agent that supposedly sent it: a peer can forge OriginBus, Sender and
//     MessageID for a bus it does not own, and it is accepted. Message ids are
//     "<bus>-<seq>" and sequential, so a peer can PRE-POISON A RANGE of a
//     victim bus's ids. When the genuine copy arrives it is the same key with a
//     different fingerprint — idem.OutcomeViolation — so invariant 10's
//     mandated disconnect fires AT THE HONEST PEER. The disconnect becomes the
//     attacker's weapon. PROTOCOL.md §8.5 already requires signed relay ingest;
//     this is where that requirement bites.
//
//  2. THE APPLIED-KEY TABLE MUST BE METERED BY THE AUTHENTICATED PEER, NOT BY
//     THE ASSERTED ORIGIN SENDER. RelayedMessage.Scope() keys internal/idem on
//     m.Sender, which is peer-asserted. internal/idem's admitAgentLocked
//     documents at length that its per-agent fair share is safe ONLY because
//     the agent id is a PROVEN identity, and cites auth.BeginSession's removed
//     MaxPendingPerAgent as the same bug. OpRelay reintroduces that shape:
//     roughly 32.7k forged relays attributed to one named remote agent take it
//     to its share and lock it out, while consuming half the bus-wide table.
//
//  3. RosterUpdate.BusID MUST BE BOUND TO THE AUTHENTICATED CONNECTION. Nothing
//     here checks that the peer on the wire IS the bus the update describes.
//     One no-delta update claiming another peer's bus id with Version set to
//     MaxUint64 wedges that peer PERMANENTLY: every genuine update it sends is
//     then refused as stale, recoverable only by re-handshake. One request.
//
//  4. "FORWARD ONLY ON A NEW ACCEPTANCE" IS A REAL RULE AND IT IS NOT IN THIS
//     PACKAGE. Re-relaying only when the applied-key table says OutcomeNew — so
//     a duplicate is answered and goes no further — is what actually bounds
//     fan-out. The split horizon alone admits one copy per simple path, which
//     in a full mesh is factorial, not linear. It lives in the wiring site's
//     AcceptRelay callback, which is why the cycle test implements it there.
//
//  5. THE BUS ID IN A HANDSHAKE RESPONSE IS NEVER BOUND TO THE HOST WE DIALLED.
//     Client.Enroll validates the responder's claimed id for SHAPE only, and
//     Registry.UpsertPeer then installs whatever it claimed. The gate must
//     cross-check it against the pinned certificate or the invite.
//
//  6. NOTHING CHECKS BusPath's LAST HOP AGAINST THE SENDING PEER. A path naming
//     three buses, none of them the peer on the connection, is accepted today.
//     Once the connection has an authenticated identity this is cheap to
//     enforce and worth enforcing.
//
//  7. A FAIR-SHARE OR CAPACITY REFUSAL FROM AcceptRelay BECOMES A 503, which
//     tells the peer to retry the very thing that is refusing it. The gate
//     should map idem.ErrAgentQuota / idem.ErrCapacity to a back-off-shaped
//     answer instead.
//
// # No wire protocol version field, on purpose
//
// The handshake payload deliberately carries NO version number. Wire protocol
// versions are RESERVED through the Spec Server reservations API, never chosen
// by the agent writing the code (CLAUDE.md, "Parallel-agent coordination"), and
// no relay wire-protocol version has been reserved. Because nothing serves this
// handler the format is not yet on any wire, so there is nothing to stay
// compatible with. The task that first REGISTERS this handler must reserve a
// version and add the field; that is recorded as a RELAY-1 follow-up. RELAY-2's
// relay and roster-sync envelopes carry no wire-protocol version either, for
// exactly the same reason and under exactly the same obligation.
//
// TWO DIFFERENT THINGS ARE CALLED "VERSION" HERE AND THEY MUST NOT BE CONFLATED.
// RosterUpdate DOES have a Version field, and it is NOT a protocol version: it
// is the peer's monotonic ROSTER EPOCH, minted by the peer for its own namespace
// so a late or reordered update cannot resurrect a departed agent (see
// Registry.ApplyRosterUpdate). It already occupies the JSON key "version" — so
// the task that adds the protocol version MUST NOT reuse that key on this
// envelope. Use "protocol_version", or rename the epoch, but do it deliberately:
// two meanings on one key is how a peer ends up applying a roster epoch as a
// format number.
//
// # RELAY-2 and RELAY-3: the relay and roster-sync surfaces
//
// The package now also carries a message relay (RelayHandler, Client.Relay,
// PeerRelayPath), an ongoing roster sync (RosterHandler, Client.PushRoster,
// PeerRosterPath, Registry), loop prevention over the traversed bus path
// (path.go) and a background Forwarder.
//
// # THESE ARE ALSO NOT REGISTERED ON ANY MUX. DO NOT REGISTER THEM.
//
// Everything above is gated by the SAME two unlanded tasks as the handshake —
// INVITE-PEERGUARD (f5d91dbe) and MTLS-RELAYGUARD (8192c3c7) — and the relay
// surface raises the stakes rather than lowering them: an ungated relay ingress
// accepts messages attributed to another bus's agents, and an ungated roster
// push edits our routing table. Registry.ApplyRosterUpdate's "a known peer
// only" rule is the ONLY thing between an anonymous POST and that table, which
// is why an update may never CREATE a peer: allowing it would make roster sync
// a second, ungated enrolment path around both gates.
//
// One more handoff MTLS-RELAYGUARD owns: invariant 10 requires that an
// idempotency key reused with a DIFFERENT payload is rejected, logged AND THE
// OFFENDING PEER DISCONNECTED. RelayHandler does the first two (409 plus a log
// line that says so); it cannot close a connection it does not own. The gate
// task must wire the disconnect.
//
// # Loop prevention is AVAILABILITY, never security
//
// PROTOCOL.md §8.5: the traversed bus path is outside the signature and can
// never be inside it — it grows on every hop — so a lying peer can rewrite it,
// including stripping us out of it. RELAY-3 therefore bounds the TRAFFIC a
// cycle produces; it does not and cannot bound what a hostile peer delivers.
// Duplicate suppression rests on idempotency plus the origin identity
// (internal/idem, IDEM-15), and RELAY-3 COMPLEMENTS that and never substitutes
// for it (invariant 10). The egress split horizon (NextHopAllowed) and the
// ingress check (CheckIncomingPath) are both needed: the first stops cycle
// traffic at the first hop, the second still works when the peer lies.
//
// # The relay fingerprint EXCLUDES bus_path, and the relay key IS the origin id
//
// Two decisions that look like details and are not:
//
//   - relayFingerprint covers the message's identity-defining content and NOT
//     the traversed path. In a mesh, one message arrives by several routes with
//     a different path each time; a path-covering fingerprint would make every
//     one of those legitimate duplicates an idem.OutcomeViolation, and
//     invariant 10 would then have correct peers DISCONNECT each other as the
//     ordinary steady state of a correct mesh.
//   - the relay idempotency key IS the origin's message id. That identity is
//     what makes two copies arriving by two disjoint paths resolve to ONE
//     idem.Scope; a peer free to mint a fresh key per hop would defeat
//     invariant 10 silently, because every copy would look new.
//
// # OriginSentAt must never become the local SentAt
//
// RelayedMessage.OriginSentAt is the ORIGIN bus's clock and is untrusted peer
// input. store.Message.VisibleTo compares SentAt against an agent's enrolment
// instant to enforce the enrolment epoch, which is an AUTHORIZATION boundary —
// so a wiring task that copied OriginSentAt into the local record would let a
// peer choose a message's visibility. The local bus stamps its own acceptance
// time; the origin's is provenance only.
//
// # The Forwarder is IN-MEMORY and therefore LOSSY
//
// Stated here so nobody has to find it in forward.go: a message accepted by
// Forwarder.Enqueue is NOT guaranteed to reach the peer. It is lost on a crash
// with a non-empty queue, and dropped (counted, logged) when a peer stays down
// long enough to fill its queue. There is no durable relay outbox and no retry;
// RELAY-4 owns retry and backoff, and a durable outbox is a follow-up. Until
// both exist, cross-bus delivery is BEST EFFORT and nothing in the product may
// claim otherwise.
//
// # Accepted residuals, written down rather than discovered later
//
//   - WHAT bus_path CAN AND CANNOT GUARANTEE, exactly. Guaranteed: well-formed,
//     at most MaxBusPath hops, no duplicate hop, BusPath[0] == OriginBus, and we
//     are not on it. NOT guaranteed: that any of it is TRUE. A peer that strips
//     us out defeats the ingress check completely and there is no detection.
//     There is, however, no SECOND evasion route — PathContains folds ASCII
//     case, and validateHops has already restricted every hop to
//     ids.ValidateBusID's ASCII [A-Za-z0-9_-], so no Unicode-folding trick
//     reaches the comparison.
//
//   - Forwarder.queues and their goroutines are NEVER RECLAIMED; there is no
//     counterpart to Registry.RemovePeer. Peer churn leaks one channel of
//     DefaultQueueDepth slots plus one goroutine per bus id ever routed to.
//     Bounded in practice by the peer set, unbounded in principle under a
//     hostile or flapping topology.
//
//   - RosterHandler's status switch does not name ErrPeerBusIDCollision, so if a
//     wiring site ever surfaces one from its Apply callback it becomes a 503
//     rather than the 400 it is. Harmless today because ValidatePeerBusID
//     catches the same condition earlier in the handler.
//
//   - The 403-unknown-peer versus 409-stale-roster split IS ITSELF a
//     peer-enumeration oracle: one POST distinguishes "we federate with X" from
//     "we do not". The earlier note claiming 403 AVOIDS an oracle was wrong. It
//     is accepted because the gate authenticates the caller before this handler
//     is reachable at all, which is precisely when it stops mattering.
package relay
