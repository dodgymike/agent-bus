package ack

import "sort"

// This file is the SENDER-VISIBLE READ SIDE of the delivery lifecycle table
// (ACK-CONTRACT.md §13, ACK-9). Nothing here mutates a row.
//
// # WHY THE FILTER IS HERE AND NOT IN THE HANDLER
//
// Lookup's doc (store.go) states the debt this file pays:
//
//	"ACK-9 owes a `rows for (correlation key) filtered by sender == principal`
//	 accessor, and the filter belongs INSIDE it rather than at the handler."
//
// The reason is that Lookup takes a RECIPIENT. A handler that answered §13 by
// iterating candidate recipients through Lookup would satisfy the letter of the
// uniform-answer rule and break it completely: the caller chooses the
// recipients it probes, so the LOOP is the oracle — a probe that comes back
// with a row has told the prober that (key, recipient) exists. StatusRows takes
// no recipient at all. The caller cannot express the probe.
//
// # WHAT THIS FILE MAY NOT BECOME
//
// There is no "list my keys", no count, and no accessor that takes a recipient.
// §5.5 refuses aggregate delivery counts because "an aggregate is a roster-size
// oracle: it discloses bus membership to any sender", and Stats carries the same
// warning. The only question answerable here is the one a sender already knows
// the input to: "what happened to the message I sent, whose id this bus gave
// me?"

// indexAddLocked records k in the correlation-key index. The caller holds mu and
// must call it EXACTLY when a row is newly inserted, never on replacement — the
// index is a set, so a replacement changes nothing and a second add would be a
// no-op that hid a bookkeeping bug.
//
// # WHY AN INDEX AT ALL, RATHER THAN A SCAN
//
// A status read that scanned s.records would be O(retained) — up to MaxEntries
// (65536) map probes — while holding the mutex that every Accept, Settle and
// MarkInFlight needs, and Accept runs inside Hub.publish with the global write
// lock held. An authenticated agent looping GET /v1/ack/<garbage> would then
// stall every writer on the bus, and it would cost the prober one request per
// stall. That is the shape ACK-2 meant by "it needs its own secondary index".
//
// The index costs no NEW bound: it holds exactly the keys of s.records, so it is
// bounded by the same maxEntries that bounds the table, and delLocked removes
// from it on the one path that removes a row.
func (s *Store) indexAddLocked(k key) {
	set := s.byCorrelation[k.correlationKey]
	if set == nil {
		set = make(map[string]struct{}, 1)
		s.byCorrelation[k.correlationKey] = set
	}
	set[k.recipient] = struct{}{}
}

// indexRemoveLocked drops k from the correlation-key index, deleting the outer
// entry when its last recipient goes. The caller holds mu.
//
// The outer delete is NOT tidiness: an empty map left behind for every swept
// correlation key would make the index grow without bound while the table it
// indexes stayed at its cap — a leak whose whole point was to be bounded.
func (s *Store) indexRemoveLocked(k key) {
	set := s.byCorrelation[k.correlationKey]
	if set == nil {
		return
	}
	delete(set, k.recipient)
	if len(set) == 0 {
		delete(s.byCorrelation, k.correlationKey)
	}
}

// StatusRows returns every retained row for correlationKey that was sent by
// sender, ordered by recipient. It is the ONLY accessor the sender-visible
// status route may use (ACK-CONTRACT.md §13.3).
//
// # AN EMPTY RESULT IS THE UNIFORM ANSWER, AND IT IS FOUR CASES AT ONCE
//
// nil is returned when the key never existed, when it was swept, when it
// belongs to somebody else, AND when either argument is malformed. The caller
// renders all four as `unknown`, because §13.3 requires exactly that:
//
//	"Only the ORIGINAL SENDER may read a row. Every other case — key never
//	 existed, key swept, key belongs to someone else — returns the SAME
//	 answer: 200 with state: unknown."
//
// A 403 on the third case would confirm the message exists, which is the oracle
// ACK-4 is required to close. So this method has no error return: there is no
// value it could carry that would not distinguish those cases for the caller,
// and a caller that logged or rendered such a value would reopen the oracle by
// accident. Lookup takes the same posture, for the same reason, one layer down.
//
// # WHAT IS INDISTINGUISHABLE, STATED HONESTLY
//
// The RESULT is indistinguishable across all four cases; the TIME taken is not
// perfectly so. A retained-but-not-yours key costs one extra map probe and one
// string comparison more than a never-existed key. ACK-4 declared a coarser
// residual of the same kind honestly rather than claiming total
// indistinguishability, and this method does the same: content is
// indistinguishable, timing is not claimed to be.
//
// # THE RETURNED RECORD STILL CARRIES Sender
//
// Record.Sender is authorisation INPUT, never output. It is present in the
// returned value because the filter needs it, not because it may be served; the
// wire shape §13.2 defines has no sender field and must never grow one — the
// sender already knows who it is, and a row it is not allowed to see is one it
// never receives.
func (s *Store) StatusRows(correlationKey, sender string) []Record {
	// VALIDATED FIRST, and the refusal is INDISTINGUISHABLE from a miss. Both
	// mutating methods validate; a read that did not would be the laxer of the
	// two, and the read is the one a route exposes to a probe.
	if err := validateMessageID("correlation_key", correlationKey); err != nil {
		return nil
	}
	if err := validateAgentID("sender", sender); err != nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Swept BEFORE reading, like every other exported entry point. A row past
	// its retention window must read as `unknown` and not as a stale outcome:
	// §11's window is a promise about how long an answer is available, and
	// serving an expired row would quietly extend it.
	s.sweepLocked(s.now())

	set := s.byCorrelation[correlationKey]
	if len(set) == 0 {
		return nil
	}
	out := make([]Record, 0, len(set))
	for recipient := range set {
		r, ok := s.records[key{correlationKey: correlationKey, recipient: recipient}]
		if !ok {
			// Unreachable while the index and the table are maintained on the
			// same two paths (putLocked, delLocked). Skipped rather than
			// trusted: a stale index entry must not be able to synthesise a row
			// out of a zero value, which would report state invalid-state(0) —
			// or, if the enum were ever reordered, `accepted` — for a message
			// that no longer exists.
			continue
		}
		// THE SENDER FILTER. This one comparison is the whole of §13.3's
		// authorisation, and it is an exact match on the fully-qualified
		// "<bus-id>.<agent-id>" the server minted (invariant 2). Never a
		// prefix, never a suffix, never case-folded: "bus-a.alice" and
		// "bus-b.alice" are different principals, and a comparison that
		// confused them would hand one bus's agent another bus's outcomes.
		if r.Sender != sender {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		// Normalised to nil so "somebody else's key" and "no such key" are the
		// same value and not merely the same length — a caller that branched on
		// nil-vs-empty-slice would have rebuilt the oracle out of two things
		// that both mean "nothing to show you".
		return nil
	}
	// Ordered by recipient so a broadcast row set (deferred, §14 D6) is stable
	// across calls. Map iteration order is randomised in Go, and an ordering
	// that changed between two identical requests would look like the state had
	// changed when it had not.
	sort.Slice(out, func(i, j int) bool { return out[i].Recipient < out[j].Recipient })
	return out
}
