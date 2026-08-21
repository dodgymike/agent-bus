package ack

import (
	"fmt"
	"sort"
)

// THIS PACKAGE IS THE SINGLE HOME OF THE CLOSED ACK VOCABULARY (ACK-13).
//
// The twelve NACK classes, the two attestation labels and the five lifecycle
// states are declared HERE and nowhere else inside the server. internal/relay
// consumes them through Go TYPE ALIASES (relay.AckClass = ack.Class, and so on)
// so there is no second set of spellings that could drift from these — a closed
// enum that exists twice is not closed, and the failure mode is silent: one side
// gains a thirteenth member, the other rejects it as unrecognised, and the
// refusal reads as a protocol violation rather than as version skew.
//
// Two copies remain OUTSIDE the server, both deliberately:
//
//   - client/ack.go holds its own string constants because client/ may not
//     import internal/ (invariant 7 — the client package is embeddable);
//   - internal/signing's AckClass* constants are a FROZEN WIRE ALPHABET. If
//     signing deferred to these types, renaming a constant here would silently
//     change signed bytes and invalidate every signature ever made, with no test
//     going red because signer and verifier would follow the rename together.
//     internal/signing/ackvocab_external_test.go is the drift guard between the
//     two, and it pins against THIS package.
//
// This file adds only the accessors those consumers need: the String/Parse pair
// for Class and Attestation, State.RecipientSourced, and the three DERIVED
// iteration helpers. It declares no new value and changes no spelling.

// String returns the durable spelling of a member of the closed class set.
//
// # A NON-MEMBER IS NOT ECHOED, AND THAT IS LOAD-BEARING
//
// Class is a string, so the obvious implementation — return string(c) — would
// print whatever bytes reached the field. The value on this plane frequently
// comes off the wire, chosen by a remote peer, and String is reached from error
// text and operator log lines (relay's ValidateAckClassForOutcome formats it
// with %s). The uint8 enum this type replaced printed "AckClass(200)" and could
// not echo anything; relay's elideAck exists for the same reason. So a
// non-member reports only that it is invalid and how large it was, mirroring
// State.String's invalid-state(%d) posture. The offending spelling IS shown, in
// elided form, by ParseClass — the one place a caller has asked to be told what
// it just refused.
func (c Class) String() string {
	if c.Valid() {
		return string(c)
	}
	return fmt.Sprintf("invalid-class(%d bytes)", len(c))
}

// Valid reports membership of the closed class set — either half of it.
//
// The halves stay separate (busClasses, recipientClasses) because validate
// enforces the PAIRING as well as the membership; Valid is the union, for the
// callers that only need to know the value is in the vocabulary at all.
func (c Class) Valid() bool { return c.BusEmitted() || c.RecipientEmitted() }

// ParseClass decodes a class spelling.
//
// AN UNRECOGNISED SPELLING IS AN ERROR, NEVER A DEFAULT — ParseState's posture
// and for the same reason: guessing turns a corrupt or future-format record into
// a plausible-looking outcome, and on this plane the plausible-looking outcome
// is a TERMINAL one that can never be revisited.
//
// The offending spelling is ELIDED: a remote party chooses those bytes and must
// not get to choose the size of the line we log about refusing them.
func ParseClass(s string) (Class, error) {
	c := Class(s)
	if c.Valid() {
		return c, nil
	}
	return "", fmt.Errorf("%w: %q is not one of the twelve acknowledgement classes; the set is CLOSED and an unrecognised spelling is refused rather than defaulted", ErrInvalidRecord, elide(s))
}

// String returns the durable spelling of a member of the closed attestation set.
// A non-member is NOT echoed, for Class.String's reason.
func (a Attestation) String() string {
	if a.Valid() {
		return string(a)
	}
	return fmt.Sprintf("invalid-attestation(%d bytes)", len(a))
}

// ParseAttestation decodes an attestation spelling. Unrecognised is an error,
// never a default — and in particular "verified" does not parse, because no such
// attestation exists: nothing in this system can verify one.
func ParseAttestation(s string) (Attestation, error) {
	a := Attestation(s)
	if a.Valid() {
		return a, nil
	}
	return "", fmt.Errorf("%w: %q is not one of the two attestation labels; there is deliberately no value meaning \"verified\", because nothing can produce one", ErrInvalidRecord, elide(s))
}

// RecipientSourced reports whether the state originates with the RECIPIENT
// APPLICATION (plane C) rather than with a bus's routing layer. It is what
// decides whether an attestation must be present.
//
// It answers FALSE for every non-terminal and out-of-range value, which is why
// callers bounds-check the state FIRST: an unchecked value would otherwise fall
// through the bus-sourced arm and be labelled peer_bus — this bus vouching, in a
// durable record, for something it could not even classify.
func (s State) RecipientSourced() bool { return s == StateDelivered || s == StateRefused }

// AllClasses returns the twelve classes in a stable order: the nine bus-emitted
// ones then the three recipient-emitted ones, each sorted by spelling.
//
// It is DERIVED from busClasses and recipientClasses rather than written out a
// second time. A hand-written list here would be exactly the defect ACK-13
// removed — a second declaration of a closed set, free to disagree with the
// first — only one file closer.
func AllClasses() []Class {
	out := make([]Class, 0, len(busClasses)+len(recipientClasses))
	out = append(out, sortedClasses(busClasses)...)
	out = append(out, sortedClasses(recipientClasses)...)
	return out
}

// AllBusClasses returns the nine bus-emitted classes, sorted by spelling.
func AllBusClasses() []Class { return sortedClasses(busClasses) }

// AllRecipientClasses returns the three recipient-emitted classes, sorted by
// spelling.
func AllRecipientClasses() []Class { return sortedClasses(recipientClasses) }

func sortedClasses(set map[Class]struct{}) []Class {
	out := make([]Class, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllAttestations returns the two attestation labels, sorted by spelling.
// Derived from the attestations set, for AllClasses's reason.
func AllAttestations() []Attestation {
	out := make([]Attestation, 0, len(attestations))
	for a := range attestations {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllStates returns the five durable lifecycle states in declaration order
// (accepted, in_flight, delivered, refused, undeliverable), which is also
// increasing progress through the machine. StateInvalid is NOT a member: it is
// the zero value and is never valid.
//
// Derived from stateNames, for AllClasses's reason.
func AllStates() []State {
	out := make([]State, 0, len(stateNames))
	for s := range stateNames {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllTerminalStates returns the three ABSORBING states, in AllStates's order.
// They are exactly the outcomes an ACK/NACK frame may carry: the two
// non-terminal states are facts about one bus, not about the recipient, and must
// never travel on a frame.
func AllTerminalStates() []State {
	out := make([]State, 0, 3)
	for _, s := range AllStates() {
		if s.Terminal() {
			out = append(out, s)
		}
	}
	return out
}
