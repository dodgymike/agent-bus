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
// RELAY-2, RELAY-3 and SIGN-7 add eight more, and they are listed in full
// because THIS LIST IS THE HANDOFF. A gate task reads it and nothing else; a property that is
// only in a field comment somewhere is a property the gate will not implement.
// Each was raised by the security gate on 2026-08-07 and each is P1 now, P0 the
// moment a handler is served:
//
//  1. THE SIGNATURE EXISTS AND THE TRUST MODEL IS NOW DECIDED; WHAT IS MISSING
//     IS THE PEERING MATERIAL THAT WOULD MAKE IT IMPLEMENTABLE (see item 8).
//
//     SIGN-7 landed the mechanism: RelayRequest carries a detached Ed25519
//     Signature over the signed field values, RelayedMessage.CanonicalBytes
//     RE-DERIVES the signed bytes with signing.Canonicalize from the exact
//     fields this bus will route, deliver, attribute and log (PROTOCOL.md
//     §8.5 — never a blob that arrived beside them), and verification is
//     MANDATORY at ingest: ValidateRelayRequest takes a CrossBusTrust as a
//     required parameter and runs VerifyRelayed before it returns, so no
//     unverified RelayedMessage can exist. A nil trust is a refusal, not a
//     skip. A relayed BROADCAST is refused outright, because canonical format
//     v1 will not sign an empty recipient set and an exemption would be an
//     unauthenticated downgrade selectable from the wire (SIGN-3 owns that).
//
//     THE TRUST QUESTION IS NO LONGER OPEN. DECISIONS.md (2026-08-07, "Cross-bus
//     key trust: pin the origin bus key at peering, no TOFU"): the ORIGIN bus's
//     attestation of its own agent's messaging key travels INTACT, signed by the
//     ORIGIN BUS'S SIGNING KEY, and is NEVER re-attested by an intermediate; that
//     signing key is PINNED AT PEERING TIME; and there is NO trust-on-first-use
//     anywhere, not even as a fallback. TOFU's exposure window is FIRST CONTACT,
//     which is exactly when a hostile intermediate is best placed to act, and a
//     fallback would reintroduce the entire hole for every peer not yet seen —
//     which is every peer, once.
//
//     CrossBusTrust is that ruling made STRUCTURAL. It replaced a one-method
//     SignerKeyResolver, which was a per-agent key oracle: nothing in that
//     signature distinguished an implementation that checked the origin's
//     attestation from one that believed the nearest bus's re-attestation.
//     PinnedBusSigningKeys yields the peering-time pins, and those pins are
//     PASSED INTO AttestedSignerKey — so an implementation has nothing else to
//     verify an attestation against. VerifyRelayed fetches the pin FIRST and
//     UNCONDITIONALLY, which is what makes "an unpeered bus's messages cannot be
//     verified" a property of the code (ErrUnpeeredBus, 403 CodeUnpeeredBus)
//     rather than a claim in prose.
//
//     WHAT REMAINS is (a) the peering field that would give PinnedBusSigningKeys
//     a source of truth — item 8, owned by INVITE-PEERGUARD — and (b) CRYPTO-4's
//     attested-bundle format, its transport and `key_epoch`. This package ships
//     NO implementation of CrossBusTrust and no default, deliberately.
//
//     WHY THE RESOLVER IS LOAD-BEARING RATHER THAN A REFINEMENT — this is the
//     attack a wrong one re-opens, unchanged from before SIGN-7: if a peer can
//     get a signature accepted for a bus it does not own, it can forge
//     OriginBus, Sender and MessageID. Message ids are "<bus>-<seq>" and
//     sequential, so it can PRE-POISON A RANGE of a victim bus's ids. When the
//     genuine copy arrives it is the same key with a different fingerprint —
//     idem.OutcomeViolation — so invariant 10's mandated disconnect fires AT
//     THE HONEST PEER. The disconnect becomes the attacker's weapon.
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
//  8. THE PEERING HANDSHAKE CARRIES NO BUS SIGNING KEY, SO NO PIN CAN EVER BE
//     ESTABLISHED — AND RELAY INGEST THEREFORE CANNOT BE SERVED.
//
//     PeerEnrollRequest and PeerEnrollResponse (peer.go) carry ONLY bus_id and
//     agents. They carry NO BUS SIGNING KEY AND NO TLS CERTIFICATE FINGERPRINT,
//     so there is today NO MOMENT AT WHICH A PIN IS ESTABLISHED: the peering
//     material that the cross-bus trust ruling requires does not exist on the
//     wire. Until it does, CrossBusTrust.PinnedBusSigningKeys has NO SOURCE OF
//     TRUTH, every relayed message is ErrUnpeeredBus by construction, and the
//     relay ingest cannot be served at all.
//
//     THE PEERING MATERIAL MUST CARRY THE PEER'S BUS SIGNING KEY. That is a
//     DIFFERENT KEY from the TLS certificate whose fingerprint travels in the
//     invite blob, and the invite's fingerprint DOES NOT SUBSTITUTE for it
//     (DECISIONS.md 2026-08-07, "Bus TLS key and bus signing key are separate").
//     The TLS key authenticates the CONNECTION and is pinned by CLIENTS from the
//     invite; the SIGNING key attests AGENT KEY BUNDLES and is pinned by PEERS at
//     peering time. A peer pins TWO THINGS, OBTAINED AT DIFFERENT MOMENTS, and
//     they must never be conflated in code, in a field name or in a doc phrase —
//     which is why nothing here is called "busKey". Their rotations differ in
//     blast radius (a TLS rotation is one bus's clients and rolls over on two
//     certificates; a SIGNING-key rotation invalidates the pins held by every
//     peer bus) and so do their compromises (a TLS key impersonates the bus to
//     CLIENTS; a signing key forges attestations for ANY AGENT ON THE BUS).
//
//     INVITE-PEERGUARD (f5d91dbe) OWNS THE PEERING HANDSHAKE AND THEREFORE OWNS
//     ADDING IT. It is not relay's to add unilaterally: the field is peering
//     material, it must be delivered over the same operator-mediated channel the
//     invite uses, and adding a key field to this envelope without that channel
//     would be trust-on-first-use wearing a field name — precisely what the
//     ruling forbids.
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
// # The relayed timestamp must never become the local SentAt
//
// RelayedMessage.TimestampUnixMilli is the SENDING AGENT's signed wall clock and
// is untrusted peer input. store.Message.VisibleTo compares SentAt against an
// agent's enrolment instant to enforce the enrolment epoch, which is an
// AUTHORIZATION boundary — so a wiring task that copied this value into the
// local record would let a peer choose a message's visibility. The local bus
// stamps its own acceptance time; the relayed one is provenance only, and a
// VALID SIGNATURE OVER IT CHANGES NOTHING: the signature proves the agent chose
// that number, not that the number is true.
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
