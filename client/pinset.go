package client

import (
	"strconv"
	"strings"
)

// MaxBusPins is the largest number of bus certificates one identity may accept.
//
// # Why there is a bound at all, and why it is exactly two
//
// The accept-set exists for ONE event: a certificate rollover. DECISIONS.md E3
// says a rotating bus "serves TWO certificates during rollover so clients can
// re-pin without downtime", so two is not a round number picked for headroom —
// it is the width of the thing being modelled, and any larger value would be
// arbitrary.
//
// The bound is the whole reason a set is safe. An unbounded, never-pruned
// accept-set degenerates into "accept every certificate this bus has ever had",
// which is a slow-motion version of the substitution the pin exists to detect:
// a key compromised two rotations ago would still be honoured, forever, and
// nothing would look wrong. Retiring a pin is therefore as deliberate an act as
// adding one — see Store.RemoveBusPin.
//
// A third pin is refused rather than silently evicting the oldest. Eviction
// would decide, on the operator's behalf and without telling them, which
// certificate stops being trusted; at a cap of two the operator names the
// retired certificate explicitly and the refusal says so.
const MaxBusPins = 2

// BusPinSet is the set of bus certificates an identity will accept: normally
// one, and TWO for the duration of a rollover.
//
// # Membership is granted, never learned
//
// A fingerprint enters this set only by an explicit operator act — the invite's
// fingerprint at enrolment, or `agent-busctl pin add` after the new value has
// been confirmed OUT OF BAND. Nothing in this package ever adds the certificate
// a bus happened to present: that is trust-on-first-use wearing the costume of
// a rotation, and invariant 11 abolished TOFU by name.
//
// TestPinIsNeverLearnedFromAHandshake is the permanent guard, and it has two
// halves that are NOT equally strong — the security gate was right to push back
// on an earlier draft that called the property "structural":
//
//   - The BEHAVIOURAL half is what carries it. Across a refused handshake, a
//     repeated refusal and a successful one, the PERSISTED set is unchanged.
//   - The STRUCTURAL half raises the cost rather than closing the door.
//     Derivation is confined to pin.go, which cannot reach the credential store,
//     and the pin-writing calls are confined to a named set of files. But
//     BusFingerprintError.Presented carries a derived fingerprint OUT of pin.go
//     by design (an operator has to be told what was presented), so an auto-heal
//     written against that field would have to be added to a file the guard
//     names — which is the point: it cannot be done in a diff that does not
//     obviously say so.
//
// # Shape
//
// The slice is unexported and every method is a value method returning a fresh
// set, so a caller cannot alias, resize or reorder a pin set another goroutine
// is verifying against. Order is INSERTION order — the enrolment pin first,
// anything added during a rollover after it — which makes `pin list` and every
// error message read chronologically rather than by hex accident.
//
// The zero value is the EMPTY set. Empty means ABSENT, never "any": every
// caller treats it as "refuse to speak TLS to this bus" (transportSecurity),
// and verifyPinnedBusCertificate refuses it a second time inside the handshake
// callback.
type BusPinSet struct {
	pins []BusFingerprint
}

// NewBusPinSet builds a set from fingerprints, in order, dropping duplicates
// and zero values.
//
// Zero fingerprints are DROPPED rather than rejected because the zero value
// means "absent" throughout this package; a caller assembling a set from
// optional sources would otherwise have to filter first, and the one thing that
// must not happen — a zero slipping into the set and being compared against a
// certificate — cannot, because it is never stored.
//
// It does NOT enforce MaxBusPins. Construction is not the operator act; growth
// is. See With, which is the only path that adds to an existing set and the
// only one that has an operator to refuse.
func NewBusPinSet(pins ...BusFingerprint) BusPinSet {
	var s BusPinSet
	for _, p := range pins {
		if p.IsZero() || s.Contains(p) {
			continue
		}
		s.pins = append(s.pins, p)
	}
	return s
}

// ParseBusPinSet decodes a list of textual fingerprints, in order.
//
// Each element goes through ParseBusFingerprint, so the one textual form (64
// lowercase hex, no colons, no whitespace) is enforced element by element.
func ParseBusPinSet(hexes []string) (BusPinSet, error) {
	parsed := make([]BusFingerprint, 0, len(hexes))
	for _, h := range hexes {
		f, err := ParseBusFingerprint(h)
		if err != nil {
			return BusPinSet{}, err
		}
		parsed = append(parsed, f)
	}
	return NewBusPinSet(parsed...), nil
}

// parseBusPinSetLenient decodes what it can and DISCARDS what it cannot.
//
// It exists for exactly one caller — Store.RemoveBusPin — and the justification
// is that removal can only ever NARROW what is accepted. ParseBusPinSet is
// all-or-nothing, which is right everywhere a set is about to be USED (a store
// we cannot fully read must never become "no pin"), and wrong on the one path
// whose job is to repair a store that is already wrong: a single garbage entry
// would otherwise disable the command the error message tells the operator to
// run.
//
// It is unexported, and must stay that way. A lenient parser reachable from the
// connection path would silently turn a damaged accept-set into a smaller one
// that still connects — which is a different bug in the same family as the one
// it fixes.
func parseBusPinSetLenient(hexes []string) BusPinSet {
	parsed := make([]BusFingerprint, 0, len(hexes))
	for _, h := range hexes {
		if f, err := ParseBusFingerprint(h); err == nil {
			parsed = append(parsed, f)
		}
	}
	return NewBusPinSet(parsed...)
}

// Len is how many certificates this set accepts.
func (s BusPinSet) Len() int { return len(s.pins) }

// IsEmpty reports whether no certificate is pinned, i.e. "no pin was
// configured". It is the set-valued spelling of BusFingerprint.IsZero, and it
// means ABSENT rather than ANY.
func (s BusPinSet) IsEmpty() bool { return len(s.pins) == 0 }

// Contains reports whether f is one of the accepted certificates.
//
// A zero f never matches, even against an empty set: absent is not a wildcard.
func (s BusPinSet) Contains(f BusFingerprint) bool {
	if f.IsZero() {
		return false
	}
	for _, p := range s.pins {
		if p.Equal(f) {
			return true
		}
	}
	return false
}

// There is deliberately NO exported accessor returning []BusFingerprint. An
// embedder that needs to see the members has Strings (the textual form the
// invite and the bus's log use) and Contains (the question actually being
// asked); a third accessor would be exported API with no caller, on the one
// type where the fewer ways there are to get the bytes out, the better.

// Strings renders the set as lowercase hex, in insertion order. It is what the
// store persists and what --json emits.
//
// An empty set returns nil rather than an empty slice, so `omitempty` on the
// stored field actually omits.
func (s BusPinSet) Strings() []string {
	if len(s.pins) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.pins))
	for _, p := range s.pins {
		out = append(out, p.String())
	}
	return out
}

// String renders the set for a human, comma-separated. An empty set renders as
// "(none)" rather than "" so a message never trails off into nothing.
func (s BusPinSet) String() string {
	if len(s.pins) == 0 {
		return "(none)"
	}
	return strings.Join(s.Strings(), ", ")
}

// Equal reports set equality, ignoring order.
//
// Order-insensitive on purpose: this is what decides whether a cached transport
// may be reused (Client.doer), and {A,B} and {B,A} accept exactly the same
// certificates. Treating them as different would rebuild the transport — and
// drop live connections — over a reordering that changes nothing.
func (s BusPinSet) Equal(other BusPinSet) bool {
	if len(s.pins) != len(other.pins) {
		return false
	}
	for _, p := range s.pins {
		if !other.Contains(p) {
			return false
		}
	}
	return true
}

// With returns the set plus f, and is the ONLY way a set grows.
//
// Adding a fingerprint already present is a no-op that succeeds: re-running
// `pin add` with the same value is the obvious human action after an
// interrupted rollover, and punishing it would teach the operator to reach for
// something stronger.
//
// At MaxBusPins it REFUSES, naming the pins already held so the operator can
// see which one is the retired certificate. It does not evict — see MaxBusPins.
func (s BusPinSet) With(f BusFingerprint) (BusPinSet, error) {
	if f.IsZero() {
		return BusPinSet{}, newError(KindUsage, "pin",
			"refusing to pin the zero fingerprint",
			"pass the 64 lowercase hex characters the bus logs as `bus_cert_fingerprint=…` at startup")
	}
	if s.Contains(f) {
		return s, nil
	}
	if len(s.pins) >= MaxBusPins {
		return BusPinSet{}, newError(KindUsage, "pin",
			"this identity already accepts "+strconv.Itoa(len(s.pins))+" certificates ("+s.String()+"), which is the maximum of "+strconv.Itoa(MaxBusPins),
			"a rollover needs exactly two: the outgoing certificate and the incoming one. Retire the one the bus no longer serves first — `agent-busctl pin remove <fingerprint>` — then add the new one. Nothing is evicted for you, because that would silently decide which certificate stops being trusted")
	}
	out := BusPinSet{pins: make([]BusFingerprint, len(s.pins), len(s.pins)+1)}
	copy(out.pins, s.pins)
	out.pins = append(out.pins, f)
	return out, nil
}

// Without returns the set minus f, and reports whether f was in it.
//
// It does NOT refuse to empty the set — that judgement belongs to the caller
// that knows whether an unpinned identity is usable (Store.RemoveBusPin
// refuses; a caller narrowing a set in memory does not).
func (s BusPinSet) Without(f BusFingerprint) (BusPinSet, bool) {
	if !s.Contains(f) {
		return s, false
	}
	out := BusPinSet{}
	for _, p := range s.pins {
		if p.Equal(f) {
			continue
		}
		out.pins = append(out.pins, p)
	}
	return out, true
}
