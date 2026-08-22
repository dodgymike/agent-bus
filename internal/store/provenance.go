package store

// ACK-5 — THE BODY-FREE PROVENANCE ACCESSOR.
//
// This file exists so ONE question can be asked from a request handler without
// the whole message coming back with the answer: "is the authenticated
// principal a named recipient of a RELAYED copy held under this correlation
// key?" Its consumer is hub.AcknowledgeDelivery's transit branch, which needs
// exactly that and nothing else (ACK-CONTRACT.md §9.4).

// RelayProvenance is the routing provenance of a retained message: who it was
// addressed to and the path it travelled. NO BODY, EVER — and no sender, no
// signature, no attestation, no timestamps.
//
// The omissions are the point rather than an economy. Invariant 6 draws the
// line this struct sits on: the append-only trail records METADATA AND ROUTING
// ONLY, never bodies, and a routing question deserves a routing-only answer. A
// caller that needs the message itself already has ByID and ByOriginMessageID,
// both of which are server-internal by their own doc comments; adding a field
// here to save such a caller a lookup would put a body on a path an
// authenticated agent can drive, which is the whole thing this type prevents.
//
// Both slices are FRESH COPIES owned by the caller (see
// RelayProvenanceByOriginMessageID), never a window into the serving copy.
type RelayProvenance struct {
	// Recipients is the message's directed recipient list, each entry fully
	// qualified `<bus-id>.<agent-id>` (invariant 2). It is empty for a
	// broadcast, which carries a FLAG and no expanded audience
	// (Message.Broadcast) — so a broadcast can never make a membership test
	// against this field true, which is the correct answer for it here.
	Recipients []string

	// BusPath is the path the message travelled, ORIGIN-FIRST and — for a
	// relayed message — ENDING AT THIS BUS, because that is what
	// hub.relayedBusPath writes: it validates the path as it arrived and
	// appends this bus as the final hop. It is NOT the path as it came off the
	// wire.
	//
	// That shape is load-bearing for the consumer: relay.UpstreamHop takes the
	// hop at index len-2 and REFUSES to search for this bus elsewhere in the
	// path, precisely so a peer that fabricates a path cannot choose which bus
	// this one contacts. Handing a wire path to that function would reopen it.
	BusPath []string

	// Relayed reports that this bus holds a COPY OF SOMEBODY ELSE'S MESSAGE:
	// the message arrived over a relay hop, so OriginMessageID is set and the
	// origin bus is not this one.
	//
	// It is derived from OriginMessageID being non-empty, which is sound in one
	// direction only and that is the direction used: BOTH write paths
	// (Message.WithOriginMessageID and Decode) REFUSE an OriginMessageID whose
	// bus half is this bus, so a set value always names a foreign origin. A
	// locally-originated message has it empty and Message.OriginID() falls back
	// to the local id — which is why the correlation key of a local message
	// resolves through ByOriginMessageID's fallback arm with Relayed == false.
	//
	// THE CONSUMER MUST NOT COLLAPSE THE TWO. A local message that has no
	// lifecycle row is swept-or-never and owes the uniform refusal; a relayed
	// one has no row by design (hub.recordAcceptance writes none for relayed
	// ingest) and is the transit case.
	Relayed bool
}

// RelayProvenanceByOriginMessageID returns the routing provenance of the
// retained message that corresponds to originMessageID — ACK-CONTRACT.md §3's
// correlation key, the ORIGIN bus's server-minted message id.
//
// The second return is false when no retained message matches, which includes
// the message having been pruned by retention.
//
// # WHY THIS EXISTS AND WHY THE CALLER DOES NOT JUST USE ByOriginMessageID
//
// Two reasons, and the first is a resource bound rather than a preference.
//
//  1. ByOriginMessageID returns a deep copy, and copyMessage copies the BODY.
//     This accessor sits on a path an authenticated agent drives ONCE PER
//     POST /v1/ack, so returning a Message would mean copying up to a whole
//     message body per acknowledgement — an amplifier whose size the agent
//     chose when it sent the message, and which no ACK ever needed.
//  2. Invariant 6's rule applies to the shape of the answer: this is a ROUTING
//     question ("was I addressed, and did this arrive over a hop?") and the
//     answer must carry routing and metadata only. Returning a body here would
//     also put message content one accessor away from a request handler, which
//     is exactly the distance ByOriginMessageID's own warning is protecting.
//
// # WHY IT IS ACCEPTABLE FOR *THIS* ONE TO BE REACHED FROM A REQUEST HANDLER
//
// ByOriginMessageID's doc says, in terms, NEVER from a request handler and
// NEVER with a client-supplied id — because it hands back a whole retained
// message, so an agent guessing `<bus-id>-<n>` over a trivially enumerable
// namespace would be handed other agents' mail. That warning is about WHAT
// ESCAPES, and this accessor is the filter that changes the answer:
//
//   - it returns no body, no sender, no signature and no timestamps, so a hit
//     discloses nothing about content or authorship;
//   - its caller, hub.AcknowledgeDelivery, uses it to decide ONE thing — is the
//     AUTHENTICATED PRINCIPAL a named recipient of a RELAYED message under this
//     key — and the principal is not a request field, so an agent can only ever
//     test itself for membership;
//   - a MISS and a NON-MEMBERSHIP produce byte-identical behaviour: the uniform
//     refusal of §13.3. An agent therefore learns only about messages addressed
//     to itself, which it is entitled to know because it was handed them.
//
// THE RESIDUAL, STATED RATHER THAN HIDDEN. A coarse TIMING difference exists
// between the transit path (which resolves a message, then forwards a hop) and
// the uniform refusal (which does not), so the answers are indistinguishable in
// content but not in latency. That is the same residual `ACK-4` already
// declares for `GET /v1/ack/<key>`, it is not closed here, and closing it would
// mean equalising work on a path that performs network I/O on one arm.
//
// A SECOND RESIDUAL, ON COST: the resolution below still runs copyMessage
// INTERNALLY, so one transient body copy per call is made and immediately
// dropped. It is never returned and never retained. Removing it needs a
// body-free resolution helper inside this file's package neighbours (store.go),
// which is a change to code this task does not own.
//
// # HOW IT RESOLVES: THROUGH ByOriginMessageID, DELIBERATELY NOT BESIDE IT
//
// The `byOrigin` hit then LOCAL-ID fallback is subtle — it is sound only
// because no message can sit in byOrigin under a key of this bus's own shape —
// and that reasoning is written down in exactly one place. Re-spelling those
// two lines here would create a second copy of a rule that a future change to
// the index would have to find twice. So this is a thin projection over the one
// implementation, taken WITHOUT s.mu held (that method takes it, and s.mu is
// not reentrant) and therefore inheriting its retention-first behaviour
// unchanged.
func (s *Store) RelayProvenanceByOriginMessageID(originMessageID string) (RelayProvenance, bool) {
	m, ok := s.ByOriginMessageID(originMessageID)
	if !ok {
		return RelayProvenance{}, false
	}
	return RelayProvenance{
		// COPIED AGAIN, ON PURPOSE. copyMessage has already made these fresh,
		// so this second copy buys no correctness today — it makes the
		// no-aliasing property of RelayProvenance LOCAL to this file instead of
		// inherited from a helper this type does not own. It is bounded and
		// cheap: MaxRecipients is 64 and MaxBusPath is 64. If the copy above
		// ever stops being deep, this line is what keeps a caller from getting
		// a window into the serving copy — which is the failure copyMessage's
		// own doc describes as silent right up to the moment something mutates
		// it.
		Recipients: append([]string(nil), m.Recipients...),
		BusPath:    append([]string(nil), m.BusPath...),

		// Read from the field rather than through OriginID(): OriginID answers
		// "which id correlates this message", and the question here is the
		// different one of WHETHER it arrived over a hop. OriginID() folds the
		// two cases together by design, so it cannot answer this one.
		Relayed: m.OriginMessageID != "",
	}, true
}
