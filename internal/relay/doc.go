// Package relay owns bus-to-bus federation. Its first slice is the peer
// handshake: the payload shapes, the validation of a peer's claimed roster, an
// http.Handler for the responder side, and a Client for the initiator side.
//
// # THE BLANKET IMPORT BAN IS RETIRED, AND THIS IS WHAT REPLACED IT
//
// Until RELAY-18 this package could not be named from anywhere: a guard failed
// if any file outside internal/relay imported it. That ban was retired
// DELIBERATELY — its own comment asked for exactly that — and REPLACED, never
// deleted, by three narrower guards in guards_test.go:
//
//   - this package is IMPORTED ONLY BY cmd/agent-bus AND internal/httpapi. Any
//     other importer fails TestRelayImportedOnlyByWiringSites, including
//     cmd/agent-busctl: the CLI reaches a bus over HTTP and has no business
//     embedding the bus-side ingress.
//   - the relay ingress is constructible ONLY with a non-nil CrossBusTrust.
//     NewRelayHandler refuses a nil one; every field of RelayHandler is
//     unexported, so no caller outside this package can assemble one any other
//     way; and TestRelayIngressCannotBeBuiltWithoutCrossBusTrust asserts that
//     refusal AND the shape of every relay.RelayConfig literal at a wiring site.
//   - A PEER ROUTE IS REGISTERED IN EXACTLY ONE REVIEWED PLACE, enforced by
//     refusing to let one be NAMED anywhere else:
//     TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile fails if any file
//     outside this package — other than internal/httpapi/peermount.go, and that
//     package's own test files — mentions PeerEnrollPath, PeerRelayPath or
//     PeerRosterPath, or a string literal under "/v1/peer/". Until RELAY-20 the
//     rule was "nowhere at all"; it was NARROWED rather than deleted when the
//     mount landed, and what it still buys is that a SECOND mount cannot appear
//     and that cmd/agent-bus cannot mount these itself. Registration is checked
//     that broadly
//     because internal/httpapi registers every route through a helper of its
//     own, so the mount site need not import this package at all — an
//     argument-shaped check on Handle/HandleFunc misses the NORMAL wiring shape,
//     not merely a contrived one. It also refuses letting a peer route ESCAPE
//     this package — mounted here, or handed to a wiring site in a route table,
//     an exported return value, a variable, a var/const declaration or a map
//     key — which a naming rule cannot see, because this is the package that
//     legitimately names these paths. Dialling a peer's path is untouched,
//     which is what the existing peerURL call sites do. This is the guard that carries most of what the blanket ban carried
//     (its residuals are listed in guards_test.go, and they are adversarial
//     rather than accidental), and it exists because the first
//     draft of the
//     replacement did not have it: the handshake (Config/NewHandler) and the
//     roster sync (RosterConfig/NewRosterHandler) have NO trust parameter to
//     omit, so a wiring site could have mounted both with every other guard
//     green. Comments are untouched — it is an AST walk over literals and
//     selectors, so prose about these routes is unaffected. Two legitimate
//     things it does refuse, with the remedy in the same message: a fake remote
//     peer bus fixture, and a route-absence probe. Both belong in
//     internal/relay, where such fixtures already live.
//
// Why replaced rather than kept: the ban would have failed the moment correct
// work started, because the composition root has to build a peer store, an
// outbox and a client. A guard that fails correct work is a guard someone
// deletes to make the build green, and then nothing is left. The property it was
// protecting was never "unreachable" — it is "reachable from a short reviewed
// list, never built without a trust chain, and not served at all yet", which is
// what the three guards above say.
//
// # IMPORTING IS NOT SERVING, and what governs serving is a recorded ruling
//
// Handler still performs NO AUTHENTICATION OF THE PEER, and none is missing by
// accident. Nothing in this package registers a route; the MOUNT carries the
// peer principal, and RELAY-20 built it at internal/httpapi/peermount.go. So
// these handlers must still never be handed to a mux from anywhere else — every
// one of them assumes something in front has already decided WHICH ADJACENT BUS
// is on the connection.
//
// WHAT THE MOUNT NOW GUARANTEES TO EVERY HANDLER HERE, so a callback can rely on
// it rather than re-deriving it:
//
//   - the request arrived over TLS and presented a client certificate;
//   - that certificate was IN DATE, judged by crypto/x509 (tls.RequestClientCert
//     verifies no dates, so nothing upstream had checked);
//   - its leaf — index [0] only, never a searched chain — resolved through the
//     durable trust table to EXACTLY ONE adjacent bus, with unknown, withdrawn
//     and ambiguous all failing closed;
//   - httpapi.PeerBusIDFromContext(ctx) names that bus, and NO agent principal
//     is visible on the context at all.
//
// That last line is what makes gaps 3, 5 and 6 below implementable at last: a
// callback can now compare the claimed PeerEnrollRequest.BusID, the claimed
// RosterUpdate.BusID, and a bus path's last hop, against the authenticated
// connection.
//
// NONE OF THOSE THREE CHECKS EXISTS. The principal being AVAILABLE is not the
// same as it being USED, and RELAY-21 (14eafd9) landed without writing any of
// them — deliberately, since its callback signatures carry no peer principal and
// the security gate ruled the residual is replay rather than forgery (the
// envelope is bound to the origin agent's attested key). They are unowned
// follow-ups; see the note under gap 6 for exactly how a wiring site writes them
// and why an explicit parameter would be better than the context.
//
// RELAY-20 HAS MOUNTED THE ROUTES, AND THE RULING IT LANDED UNDER IS NOW
// WRITTEN DOWN — it was not, until 0adf263. Both halves of that sentence are
// load-bearing; read the second.
//
// What landed (2026-08-14): the three peer routes are served, behind an
// authenticated adjacent-bus principal, at internal/httpapi/peermount.go — now
// the ONE place outside this package permitted to name these paths. The gate it
// was authorised against is MTLS-CLIENTAUTH (a97f854), which flips the listener
// off tls.NoClientCert so a presented certificate reaches the application layer
// at all, plus RELAY-45, which supplies the durable binding from that
// certificate to exactly one adjacent bus (PeerStore.InboundPeerPrincipal).
//
// THE DIRECTION ARGUMENT is why those two and not the two ruling (c) names, and
// it is the part most likely to be lost: on /v1/peer/* WE ARE THE SERVER, so the
// principal comes from INGRESS — the certificate a peer presents to us — while
// MTLS-RELAYGUARD (8192c3c7) governs EGRESS, the certificate we present when we
// dial out. It authenticates the wrong direction to gate this mount, so making
// it a precondition would have been a category error rather than caution.
//
// # THE DEBT THAT WAS OUTSTANDING HERE IS NOW DISCHARGED, AND HOW MATTERS
//
// The 2026-08-08 ruling (c) — FEDERATION (RELAY-6), landed at 77d2b73 — named
// the old gate, and its "given up" clause read "nothing; this ruling authorises
// no shortcut; the handler stays unregistered until both gating tasks land".
// THAT ORIGINAL TEXT STILL READS EXACTLY THAT WAY AND ALWAYS WILL: DECISIONS.md
// is append-only, so a superseded ruling is corrected by a later dated section,
// never by an edit in place. Reading the 2026-08-08 section alone therefore
// tells you the OLD gate; it is not the current one.
//
// The amendment landed at 0adf263 as "2026-08-14 — FEDERATION (RELAY-6),
// AMENDMENT: ruling (c) is un-gated from the wrong direction, and two premises
// are corrected" (DECISIONS.md:4959-5317 at 88a5ade, delimited by a matched
// BEGIN/END fence pair — search the heading, not the line numbers, which have
// already drifted twice on this ruling). It restates (c) as (c-AMENDED) and
// records the MTLS-CLIENTAUTH + RELAY-45 substitution this file describes; its
// own "COROLLARY OUTSIDE THIS FILE — now discharged" paragraph names this
// package comment as the debt it settles. So the code and the recorded ruling
// now AGREE.
//
// What did NOT change, because the amendment says so in terms: the 2026-08-08
// security gate is not overturned, and its requirement — no peer route is
// mounted without an authenticated peer identity — is carried forward unchanged
// and unweakened. Only the MECHANISM named as the gate moved. An earlier draft
// of the paragraph above asserted an amendment that had not yet happened, which
// would have put a false dated claim in main; both review gates caught it. The
// remedy for a future disagreement is the same as it was — amend DECISIONS.md,
// never soften this comment, and never delete the mount to match stale text.
//
// WHAT INVITE-PEERGUARD (f5d91dbe) STILL OWNS EITHER WAY: the peering material
// itself (item 8 below), and the fact that the inbound binding is installed by an
// OPERATOR with `agent-bus peer add` rather than redeemed from a single-use
// invite. Invariant 3 says an invite is the only way onto the bus "including for
// peer buses", so that difference is real and is NOT closed by this mount.
//
// A SEPARATE, NARROWER BLOCKER SITS ON RELAY INGEST SPECIFICALLY, and it is not
// waived by either: no implementation of CrossBusTrust exists (RELAY-17 owns
// it), so every relayed message is ErrUnpeeredBus by construction. Note that
// gap 8 below, written before RELAY-10, gives a reason that has since changed:
// it says no pin can ever be ESTABLISHED because the peering handshake carries
// no bus signing key. RELAY-10 (f1a787c) landed the pin's source of truth as an
// operator-configured durable record — BusTrustRecord.SigningKeys in
// peerstore.go — deliberately NOT on the wire. So the gap's conclusion still
// holds and its stated cause no longer does; correcting the numbered list is
// RELAY-17's to do, since it owns the seam that closes it.
//
// The authority is DECISIONS.md, 2026-08-08, "FEDERATION (RELAY-6): deployment
// assumptions and what they defer", landed at 77d2b73, AS AMENDED by the
// 2026-08-14 RELAY-6 amendment (0adf263), which restates (c) as (c-AMENDED) and
// (b) as (b-CLARIFIED). Read both sections; the later one wins where they
// differ. Three of the rulings bear directly on serving these handlers:
//
//   - (a) every bus-to-bus link is an SSH tunnel and no bus process ever listens
//     publicly; one operator holds both ends of every tunnel.
//   - (b) INVITE-GATE does not block the FEDERATION epic, and it does not
//     BECAUSE OF (a): with no reachable /v1/enroll, the pre-auth attacker that
//     gate exists to stop does not exist. That deferral is bought entirely by
//     the topology, so it REVERSES MECHANICALLY on any ONE of — a bus bound to a
//     non-loopback interface, a tunnel endpoint shared with a non-operator, a
//     second local user on any of the three machines, or a peer bus the operator
//     does not control. What it does NOT buy, stated so nobody over-reads it:
//     the tunnel authenticates the MACHINE and not the bus process, and a bus
//     listening on loopback cannot tell tunnelled traffic from local traffic.
//   - (c) peer-principal authentication is NOT part of that deferral. It is a
//     forward precondition. AS ORIGINALLY LANDED the ruling named
//     INVITE-PEERGUARD (f5d91dbe) and MTLS-RELAYGUARD (8192c3c7) as the tasks
//     that close it; RELAY-20 mounted under MTLS-CLIENTAUTH + RELAY-45 instead,
//     on the direction argument above. That substitution is REAL and is now
//     RECORDED, as (c-AMENDED) in the 2026-08-14 RELAY-6 amendment (0adf263) —
//     so read (c) and (c-AMENDED) together, and treat the amended text as the
//     authority. The amendment also records the RELAY-41 chain that a draft
//     proposed as half the gate as REFUTED rather than adopted: that pin is an
//     OUTBOUND, address-keyed fingerprint, and inverting it would resolve one
//     peer's inbound connection to another peer's bus id. What holds regardless
//     and is unweakened by the amendment: a mount that authenticates no peer
//     principal is outside the ruling whatever the topology looks like, and the
//     way to change that is to amend DECISIONS.md — never to soften this
//     comment. The mount that landed authenticates one.
//
// Two of the gaps listed below are the FUNCTIONALITY half of (c), and neither is
// closed here: roster updates are not bound to the authenticated connection
// (gap 3), and a bus path's last hop is not checked against the sending peer
// (gap 6). RELAY-1 shipped the exchange; the mount ships the principal.
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
//     idem.OutcomeViolation — so the HONEST peer's real message is the one
//     refused, and the poisoned copy is what this bus delivers and attributes.
//     Since the 2026-08-08 narrowing that refusal is a 409 and a log line and
//     nothing more (see "Key reuse is REJECT-AND-LOG" below), so the attacker no
//     longer gets the victim's socket closed for it — but do not read that as
//     the hole being smaller. It is the same hole: an attacker still chooses
//     which of two copies of a message the bus accepts, and the honest sender
//     still cannot get its message through. What changed is only that the
//     collateral damage no longer extends to every other request on the honest
//     peer's connection.
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
//  4. CLOSED BY RELAY-21 (14eafd9). "FORWARD ONLY ON A NEW ACCEPTANCE" IS NOW
//     IN THIS PACKAGE, in accept.go. This item read "…AND IT IS NOT IN THIS
//     PACKAGE" until that commit, which was true when written and false the
//     moment Acceptor landed — the stale-package-doc failure this repository
//     keeps paying for, corrected here rather than left for the next reader.
//
//     Acceptor.Accept re-relays ONLY when the applied-key table reports
//     idem.OutcomeNew, so a duplicate is answered from the original result and
//     goes no further. That is what actually bounds fan-out: the split horizon
//     alone admits one copy per simple path, which in a full mesh is factorial,
//     not linear. Note the trap accept.go names at its Outcome field — the ZERO
//     VALUE of idem.Outcome IS OutcomeNew, so a seam left unfilled re-forwards
//     everything, which is the amplification this gate exists to stop.
//
//     WHAT IS STILL THE WIRING SITE'S: supplying the Acceptor with a real
//     applied-key store and a real forwarder. The seam is only as good as what
//     RELAY-24 passes into it.
//
//  5. THE BUS ID IN A HANDSHAKE RESPONSE IS NEVER BOUND TO THE HOST WE DIALLED.
//     Client.Enroll validates the responder's claimed id for SHAPE only, and
//     Registry.UpsertPeer then installs whatever it claimed. The gate must
//     cross-check it against the pinned certificate or the invite.
//
//     THIS ITEM IS THE OUTBOUND HALF ONLY, and the security gate on RELAY-20
//     found the INBOUND twin missing from this list entirely, which is worth
//     more than the item it sits under: PeerEnrollRequest.BusID — the id a peer
//     claims when it dials US — is likewise validated for SHAPE only
//     (handshake.go, ValidatePeerBusID), and AcceptPeer receives it unbound to
//     the connection. Once a wiring site routes AcceptPeer to
//     Registry.UpsertPeer, peer B presenting its own valid certificate and
//     claiming bus_id "C" REPLACES C's roster and resets its version. The fix is
//     the same one gaps 3 and 6 need and it is now available — see the note
//     under gap 6.
//
//  6. NOTHING CHECKS BusPath's LAST HOP AGAINST THE SENDING PEER. A path naming
//     three buses, none of them the peer on the connection, is accepted today.
//     Once the connection has an authenticated identity this is cheap to
//     enforce and worth enforcing.
//
//     THE IDENTITY NOW EXISTS AND IS REACHABLE, WHICH CHANGES THIS ITEM FROM
//     "IMPOSSIBLE" TO "NOT DONE" — for gaps 3, 5 and 6 alike, so read it once
//     here. RELAY-20's mount attaches the authenticated adjacent-bus principal
//     to the REQUEST CONTEXT, and every callback in this package takes a
//     context.Context: Config.AcceptPeer, RelayConfig.AcceptRelay and
//     RosterConfig.Apply all receive the same ctx the handler was called with.
//     So a wiring site writes
//
//     peerBus := httpapi.PeerBusIDFromContext(ctx)
//
//     and compares it against the claimed PeerEnrollRequest.BusID, the claimed
//     RosterUpdate.BusID, or BusPath's last hop. NOTHING IN THIS PACKAGE DOES
//     THAT YET, and none of these three checks exists.
//
//     THE ARGUMENT AGAINST LEAVING IT IN THE CONTEXT, recorded because it is a
//     real design question and not a nit: a value on a context is INVISIBLE in
//     the callback's signature, so a wiring site that never reads it compiles,
//     passes every test, and silently skips the binding. An explicit parameter —
//     `AcceptPeer(ctx, peerBusID string, p PeerRoster)` — cannot be forgotten,
//     because omitting it will not build. That change belongs to whoever closes
//     these gaps; it is NOT RELAY-20's, which owns the mount and must not
//     rewrite three callback signatures other tasks are actively editing. The
//     context route is what makes the work possible today; the signature route
//     is what would make it unforgettable.
//
//     NOTE THE IMPORT DIRECTION. This package must NEVER import internal/httpapi
//     (it would be a cycle, and TestRelayImportedOnlyByWiringSites governs the
//     other direction), so the check can only ever live in the CALLBACK, at the
//     wiring site — never in a handler here.
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
// # THESE ARE MOUNTED BY THE SAME ONE MOUNT, AND THEY RAISE THE STAKES
//
// Nothing here registers a route either, and the relay surface raises the stakes
// rather than lowering them: an ungated relay ingress accepts messages
// attributed to another bus's agents, and an ungated roster push edits our
// routing table. Registry.ApplyRosterUpdate's "a known peer only" rule is the
// ONLY thing between an anonymous POST and that table, which is why an update
// may never CREATE a peer: allowing it would make roster sync a second, ungated
// enrolment path around whatever principal the mount enforces.
//
// # Key reuse is REJECT-AND-LOG, and relay is the worst place to assume otherwise
//
// This section used to hand MTLS-RELAYGUARD an instruction to close the
// connection on idempotency-key reuse. THAT INSTRUCTION IS WITHDRAWN and must
// not be reinstated. Invariant 10 was NARROWED on 2026-08-08 by operator
// decision, after the behaviour was measured at the raw socket (code: 1c6c540;
// contract: 0dbb025, CLAUDE.md). What RelayHandler already does IS the complete
// rule, not two thirds of it awaiting a gate:
//
//   - SAME KEY + SAME PAYLOAD is a RETRY. Return the original result, apply
//     nothing, disconnect nobody. RelayHandler answers 200 with duplicate:true.
//   - SAME KEY + DIFFERENT PAYLOAD is still a protocol violation, and the whole
//     response to it is REJECT IT AND LOG IT — 409 CodeIdempotencyViolation plus
//     the Warn line. NOTHING FURTHER IS OWED BY ANY GATE TASK. An idempotency
//     key is scoped to the CALLER'S OWN agent, so reusing one for new content is
//     overwhelmingly a client that lost track of its keys; dropping the socket
//     destroys every other request pipelined on it, including a parked
//     long-poll, which lands the abuse defence on the party most likely to be
//     honest.
//   - THE ONE CASE THAT STILL DISCONNECTS ANYWHERE IN THIS CODEBASE is
//     THIRD-PARTY REPLAY of an already-accepted signed message, and even then
//     only when the claimed sender is a well-formed fully-qualified
//     <bus-id>.<agent-id> — an absent, unqualified or whitespace-padded claim
//     names nobody, is still refused, and must NOT disconnect. Relay ingest has
//     built no path to that detection yet.
//
// # The two questions to answer BEFORE wiring any disconnect here
//
// CLAUDE.md invariant 10 now carries them, and it carries them BECAUSE OF THIS
// PACKAGE:
//
//  1. Can a merely BUGGY peer reach this line?
//  2. Does this connection carry only ONE principal's traffic?
//
// FOR RELAY INGEST THE ANSWER TO (2) IS NO, and that is the fact a future
// implementer must not have to rediscover. A peer bus relays traffic on behalf
// of its ENTIRE LOCAL ROSTER over ONE link, so `sender != the connection's
// principal` is the NORMAL, CORRECT shape of every relayed message, for many
// agents at once. A per-socket disconnect on this link is therefore the wrong
// PRIMITIVE even for the one case that legitimately disconnects a single agent
// on /v1/send: one agent's buggy or hostile traffic would drop every agent
// behind that peer bus simultaneously. That is this repository's recurring
// "abuse defence aimed at the wrong party" defect, one scale up.
//
// OPEN QUESTION, DELIBERATELY NOT ANSWERED HERE, owned by whoever builds relay
// ingest for real (MTLS-RELAYGUARD 8192c3c7 / RELAY-2): if relay ever needs to
// punish a replaying peer, WHAT IS THE MECHANISM ON A MULTI-PRINCIPAL LINK?
// Per-origin-agent rejection without dropping the transport, per-peer rate
// limiting, and peer-level de-peering are all plausible and have different blast
// radii. Choosing one is a design task with its own evidence; inventing it in a
// package comment is how the wrong primitive gets inherited. Until it is chosen,
// there is no disconnect on this surface and none is missing.
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
//     one of those legitimate duplicates an idem.OutcomeViolation, so the
//     ordinary steady state of a CORRECT mesh would be a stream of 409s and
//     violation log lines between peers that are doing nothing wrong — and every
//     second arrival, which is exactly the one duplicate suppression exists to
//     absorb, would be refused instead of absorbed. (Before the 2026-08-08
//     narrowing this was worse still: it also had correct peers disconnect each
//     other. The narrowing removed the amputation, not the misdiagnosis, and the
//     misdiagnosis is what makes the mesh unusable.)
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
