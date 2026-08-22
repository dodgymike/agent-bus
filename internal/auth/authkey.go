package auth

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"sort"
)

// AUTH-key uniqueness: the rule that keeps one enrolment keypair from holding
// two agent ids (AUTH-DUP-ENROL-KEY).
//
// Enrol used to validate the auth public key's LENGTH but never checked whether
// that key was already on the roster, so three enrolments with a byte-identical
// public key were all accepted and minted three distinct agent ids bound to ONE
// keypair. One private-key holder could then authenticate as any of them — an
// impersonation and accountability hole, and the reason a per-keypair cap (the
// active-session cap, AUTH-1-FU-ACTIVECAP) bought almost nothing, because the
// "distinct enrolments" it counted were one keypair, not many.
//
// This file is the AUTH-KEY mirror of certbind.go's certificate-fingerprint
// rule, and it is that on purpose: the codebase already keeps one CERTIFICATE
// from naming two agents (ErrCertFingerprintBound), and the auth public key is
// the same kind of identity-bearing credential, so it gets the same shape of
// rule — an authoritative refusal in Roster.Put and an advisory pre-mint read in
// Service.Enrol so the overwhelmingly common refusal costs no burned agent-id
// suffix.
//
// # WHY A SCAN AND NOT A SECOND MAP
//
// There is no authKey->agent map maintained beside byID, for the same reason
// certbind.go keeps none (invariant 8): a second copy of the truth has to be
// kept correct on the RECOVERY path too — WALRoster.Apply replays whatever is on
// disk — where an index that drifts is invisible and its failure mode (resolving
// a key to an agent that does not hold it) is the worst one available. A scan
// over byID cannot drift, byID IS the truth, and the roster is bounded by the
// enrolment admission limit.

// authKeyOwner resolves an AUTH public key to the single enrolled agent id that
// holds it.
//
// THE CALLER MUST HOLD THE LOCK GUARDING byID. It is a free function over the
// map, in the same shape and for the same reason as certFingerprintOwner: both
// roster implementations need identical behaviour, and two copies of a
// fail-closed rule are two chances for one of them to stop failing closed.
//
// The three answers are distinct and two of them are refusals:
//
//	exactly one holder -> that agent id, nil
//	no holder          -> "", ErrAuthKeyUnknown
//	more than one      -> "", ErrAuthKeyAmbiguous  — and NOT a pick
//
// Ambiguity FAILS CLOSED and is reachable even though checkAuthKeyUnbound refuses
// to CREATE a second holder: WALRoster.Apply does not run the write-side check
// (it cannot usefully refuse a record that is already durable — invariant 6), so
// a damaged log carrying two agents with one auth key recovers into exactly that
// state, and the read must decline to guess.
func authKeyOwner(byID map[string]RosterEntry, key ed25519.PublicKey) (string, error) {
	// A wrong-length or empty key names nobody and is refused before any compare.
	// It is the value a caller holds when there is no usable key, so resolving it
	// would turn "no key" into "agent X". Enrol never reaches here with such a key
	// (its length check runs first), but the guard keeps the read honest for any
	// other caller.
	if len(key) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: a %d-byte key is not a well-formed auth public key and names nobody", ErrAuthKeyUnknown, len(key))
	}

	var holders []string
	for agentID, e := range byID {
		if bytes.Equal(e.AuthPublicKey, key) {
			holders = append(holders, agentID)
		}
	}

	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		return "", fmt.Errorf("%w: no enrolled agent holds this enrolment public key", ErrAuthKeyUnknown)
	default:
		sort.Strings(holders)
		return "", fmt.Errorf("%w: %d agents hold one enrolment public key (%v); it resolves to nobody until an operator resolves the duplicate", ErrAuthKeyAmbiguous, len(holders), holders)
	}
}

// checkAuthKeyUnbound refuses e if its AuthPublicKey is already held by a
// DIFFERENT agent id.
//
// THE CALLER MUST HOLD THE LOCK GUARDING byID, and must run this in the same
// critical section as the write it guards — a check-then-write split across two
// lock acquisitions admits exactly the duplicate it refuses.
//
// # THE AUTH-KEY MIRROR OF Roster.Put's DUPLICATE-ID RULE
//
// Put refuses a duplicate AgentID rather than overwriting, because overwriting
// rebinds a live identity to a different keypair. The same rule holds on the
// auth-key axis, read the other way round: one key on two agent ids means one
// private-key holder can authenticate as two agents, so the key stops naming a
// single identity. Refusing at write is what keeps authKeyOwner's ambiguous arm
// a recovery-only case.
//
// # AN AGENT REBINDING ITS *OWN* KEY IS NOT REFUSED HERE
//
// The comparison skips e.AgentID itself. On this build that case is unreachable
// — the only writer is enrolment, which always carries a freshly-minted id — but
// writing the rule as "already on a DIFFERENT agent" rather than "already
// anywhere" is what makes a future re-key route correct by construction instead
// of having to relax this check, which is how a uniqueness rule gets deleted.
func checkAuthKeyUnbound(byID map[string]RosterEntry, e RosterEntry) error {
	for agentID, other := range byID {
		if agentID == e.AgentID {
			continue
		}
		if bytes.Equal(other.AuthPublicKey, e.AuthPublicKey) {
			return fmt.Errorf("%w: agent %q already holds this enrolment public key, so it cannot also be bound to %q", ErrAuthKeyBound, agentID, e.AgentID)
		}
	}
	return nil
}
