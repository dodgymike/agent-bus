package auth

import (
	"crypto/ed25519"
	"fmt"
	"sort"
	"time"
)

// Client-certificate bindings: the fact that makes invariant 11's cross-check
// possible at all (MTLS-BIND).
//
// Invariant 11 requires that mTLS and the session token BOTH be present and be
// CROSS-CHECKED — "a session token presented over a connection whose client
// certificate belongs to a different agent must be rejected". A cross-check
// needs a stored fact to check AGAINST, and until this file there was none:
// RosterEntry.CertBindings was declared and populated by nothing, so a stolen
// session token was replayable from any machine and nothing could notice.
//
// This file is that fact and the two operations over it, and NOTHING ELSE. It
// does not decide any request. The enrolment path writes a binding; a caller
// that holds a fingerprint can ask which agent owns it. Making a route REFUSE on
// the answer is MTLS-CROSSCHECK's job, deliberately not done here — see
// "What this does NOT do" below.
//
// # THE COMPARISON IS EXACT-MATCH ON THE FINGERPRINT, AND MAY NEVER BECOME
// # CHAIN VERIFICATION
//
// A binding is compared by equality of sha256 over the certificate's DER. It is
// NEVER an x509.Verify against an x509.CertPool built out of enrolled agents'
// certificates, and that is a hard rule rather than a preference (the task's
// FORBIDDEN IMPLEMENTATION clause, from a security-testing finding on
// 2026-08-07). A certificate placed in a CertPool is a TRUSTED ROOT. Agent
// certificates are self-signed, so every one of them would be a root, and any
// agent could then mint a certificate for any name that chains to its own and
// validates — one enrolled agent becomes a CA for the whole bus. client/
// clientcert.go's template says the same thing from the other end: IsCA is
// false and KeyUsageCertSign is absent precisely so that nothing is tempted to
// build a pool. THIS EXACT-MATCH STEP IS WHAT MAKES CHAIN VERIFICATION
// UNNECESSARY; do not reach for a pool "for consistency" with anything else in
// the tree. See MTLS-RELAYGUARD-FU-BUSCERTPOOL for the same trap on the bus's
// own certificate.
//
// # WHY THE INDEX IS A SCAN AND NOT A MAP
//
// There is no fingerprint->agent map maintained beside byID, and that is a
// decision, not an omission (invariant 8). A second map is a second copy of the
// truth, and it has to be kept correct in FOUR places that each write the roster
// differently: MemoryRoster.Put, WALRoster.put, WALRoster.Apply — which is the
// RECOVERY path, replaying whatever is on disk — and any future retire/rebind
// operation. An index that drifts from byID on the recovery path is invisible
// (recovery has no client to fail) and its failure mode is the worst one
// available here: resolving a fingerprint to an agent that does not hold it.
// A scan over byID cannot drift, because byID IS the truth. The roster is a bus
// on a laptop, bounded by the enrolment admission limit, and every scan here
// already runs under the lock a map lookup would need anyway.
//
// # WHAT THIS DOES *NOT* DO, stated so nobody reads more into a stored binding
//
// A binding records that this bus accepted a certificate for an agent id at
// enrolment. On its own it authorises nothing:
//
//   - It does not check the certificate's VALIDITY WINDOW. crypto/tls proves
//     possession of the leaf's private key and never looks at NotBefore or
//     NotAfter, so an expired certificate arrives exactly like a fresh one. That
//     check belongs at the transport edge, before a fingerprint is ever computed
//     for lookup — internal/httpapi does it in WithClientCertificate, the same
//     place and the same way RELAY-20 does it for peer buses.
//   - It does not make any route require a certificate. Absence stays a normal
//     case on this build; see EnrolRequest.ClientCertFingerprint.
//   - It does not revoke. Retirement exists in the shape (CertBinding.RetiredAt)
//     and no code path sets it yet.

// AgentIDForClientCertificate resolves a client-certificate fingerprint to the
// agent id bound to it at enrolment, or refuses.
//
// It is the seam invariant 11's cross-check is built on: a caller holding the
// fingerprint of the certificate on a connection asks WHICH AGENT that
// certificate names, and compares the answer with the agent the presented
// session token names. It is on Service rather than reached through the roster
// directly so that the HTTP layer keeps exactly one dependency (the auth
// service) and cannot grow a second, differently-locked view of the roster.
//
// # A NIL ANSWER IS A REFUSAL, NEVER A PASS
//
// Both error cases mean "this certificate does not name a single agent", and a
// caller must treat both as a failed cross-check. ErrCertBindingUnknown in
// particular is easy to misread as "no constraint applies" — it is not; it means
// the certificate is unbound, which on a route that requires a bound identity is
// a refusal. See certFingerprintOwner.
func (s *Service) AgentIDForClientCertificate(fp [32]byte) (string, error) {
	return s.roster.AgentIDForCertFingerprint(fp)
}

// ClientCertificateBootstrapResult is the durable result of binding the first
// client certificate to an existing pre-TLS agent id.
type ClientCertificateBootstrapResult struct {
	AgentID      string
	Binding      CertBinding
	AlreadyBound bool
	Replayed     bool
}

// ClientCertificateBootstrapSigningContext is the pinned domain separator for
// the fresh auth-key proof on POST /v1/client-cert/bootstrap.
const ClientCertificateBootstrapSigningContext = "agent-bus:client-cert-bootstrap:v1:"

type clientCertificateBinder interface {
	BindClientCertificate(agentID string, fp [32]byte, idempotencyKey string, at time.Time) (RosterEntry, bool, error)
}

// ClientCertificateBootstrapSigningBytes returns the exact byte string an agent
// signs with its enrolled AUTH private key to bind a bootstrap request to the
// active bearer token, idempotency key and TLS-derived client certificate.
func ClientCertificateBootstrapSigningBytes(sessionToken, idempotencyKey string, fp [32]byte) []byte {
	return []byte(ClientCertificateBootstrapSigningContext + sessionToken + ":" + idempotencyKey + ":" + fmt.Sprintf("%x", fp[:]))
}

// BindClientCertificate binds fp to an already-enrolled agent without minting a
// new agent id. It is used only after the HTTP layer has authenticated the
// existing session and extracted fp from the mTLS connection. The signature is a
// fresh proof by the enrolled auth private key over the bootstrap intent; a
// stolen bearer token alone is not enough.
func (s *Service) BindClientCertificate(agentID, sessionToken, idempotencyKey string, fp [32]byte, signature []byte) (ClientCertificateBootstrapResult, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	if len(signature) != ed25519.SignatureSize {
		return ClientCertificateBootstrapResult{}, fmt.Errorf("%w: got a %d-byte signature, want exactly %d", ErrBadSignature, len(signature), ed25519.SignatureSize)
	}
	entry, ok := s.roster.Get(agentID)
	if !ok {
		return ClientCertificateBootstrapResult{}, fmt.Errorf("%w: %q", ErrUnknownAgent, agentID)
	}
	if !ed25519.Verify(entry.AuthPublicKey, ClientCertificateBootstrapSigningBytes(sessionToken, idempotencyKey, fp), signature) {
		return ClientCertificateBootstrapResult{}, ErrBadSignature
	}
	binder, ok := s.roster.(clientCertificateBinder)
	if !ok {
		return ClientCertificateBootstrapResult{}, fmt.Errorf("%w: roster cannot bind a client certificate for %q", ErrNotAttached, agentID)
	}
	entry, changed, err := binder.BindClientCertificate(agentID, fp, idempotencyKey, s.now().UTC())
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	for _, b := range entry.CertBindings {
		if b.RetiredAt == nil && b.Fingerprint == fp {
			replayed := entry.ClientCertBootstrapIdempotencyKey == idempotencyKey && !changed
			return ClientCertificateBootstrapResult{AgentID: entry.AgentID, Binding: b, AlreadyBound: !changed && !replayed, Replayed: replayed}, nil
		}
	}
	return ClientCertificateBootstrapResult{}, fmt.Errorf("%w: certificate binding for %q committed but is absent from the serving roster", ErrInvalidRecord, agentID)
}

// certFingerprintOwner resolves a client-certificate fingerprint to the single
// agent id that holds it as a LIVE binding.
//
// THE CALLER MUST HOLD THE LOCK GUARDING byID. It is a free function over the
// map, in the same shape and for the same reason as sortedCopies: both roster
// implementations need identical behaviour here, and two copies of a
// fail-closed rule are two chances for one of them to stop failing closed.
//
// # THE THREE ANSWERS ARE DISTINCT, AND TWO OF THEM ARE REFUSALS
//
//	exactly one holder -> that agent id, nil
//	no holder          -> "", ErrCertBindingUnknown
//	more than one      -> "", ErrCertBindingAmbiguous  — and NOT a pick
//
// AMBIGUITY FAILS CLOSED, and it is reachable even though checkCertFingerprint
// Unbound refuses to CREATE a second live holder. The write-side check runs on
// the live enrolment path; WALRoster.Apply does not run it, because Apply is
// recovery, replaying records that are ALREADY DURABLE — refusing one there
// would not un-write it, it would only turn a damaged log into an outage
// (invariant 6, and the same reasoning Apply's duplicate-id arm records). So
// disk can present two live holders of one fingerprint, and the read must be the
// thing that declines to guess. Returning "the first" or "the newest" would
// resolve a stolen or duplicated certificate to a DEFINITE agent, which is
// exactly the confusion invariant 11 exists to prevent.
//
// The ambiguous error NAMES the holders, sorted, because the operator reading it
// has to go and retire one of them, and an unsorted list of map keys makes two
// reports of one incident look like two incidents.
func certFingerprintOwner(byID map[string]RosterEntry, fp [32]byte) (string, error) {
	// THE ZERO FINGERPRINT NAMES NOBODY, AND IS REFUSED BEFORE ANYTHING IS
	// COMPARED. It is the value a caller holds when there was NO certificate,
	// so resolving it would turn "this connection presented nothing" into "this
	// connection is agent X" — a fail-OPEN, and the worst one available here.
	//
	// This is not defence against a value the enrolment path can produce: Enrol
	// binds only a non-nil fingerprint taken from a real parsed certificate. It
	// is defence against the two ways the zero value actually arrives.
	//
	//  1. FROM DISK. validateRosterEntry checks a binding's BoundAt and
	//     RetiredAt but does NOT reject a zero Fingerprint, so a damaged or
	//     hand-edited record carrying one decodes cleanly and is stored LIVE.
	//     Without this guard it would then be matchable.
	//  2. FROM A CALLER THAT IGNORED ok. internal/httpapi's accessors return
	//     (zero, false) when no certificate was presented, and the idiomatic
	//     slip `fp, _ := ClientCertFingerprintFromContext(ctx)` yields exactly
	//     this value with no compiler complaint. MTLS-CROSSCHECK is the task
	//     most likely to write that line, and it is the task this function
	//     exists to serve.
	//
	// internal/relay/peerstore.go guards the identical case on the peer plane,
	// in the same place and for the same reason ("the zero fingerprint MEANS
	// ABSENT on a record, so matching it would match every record that has no
	// binding at all"). The two planes agree, deliberately.
	if fp == ([32]byte{}) {
		return "", fmt.Errorf("%w: the zero fingerprint is the ABSENCE of a certificate, never a digest, and names nobody", ErrCertBindingUnknown)
	}

	var holders []string
	for agentID, e := range byID {
		if hasLiveCertBinding(e, fp) {
			holders = append(holders, agentID)
		}
	}

	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		return "", fmt.Errorf("%w: no enrolled agent holds a live binding for this client certificate", ErrCertBindingUnknown)
	default:
		sort.Strings(holders)
		return "", fmt.Errorf("%w: %d agents hold a live binding for one client certificate (%v); the certificate resolves to nobody until an operator retires all but one", ErrCertBindingAmbiguous, len(holders), holders)
	}
}

// checkCertFingerprintUnbound refuses e if any fingerprint it would bind LIVE is
// already live on a DIFFERENT agent id.
//
// THE CALLER MUST HOLD THE LOCK GUARDING byID, and must run this in the same
// critical section as the write it guards — a check-then-write split across two
// lock acquisitions admits exactly the duplicate it exists to refuse.
//
// # THIS IS THE CERTIFICATE MIRROR OF Roster.Put's DUPLICATE-ID RULE
//
// Put refuses a duplicate AgentID rather than overwriting, because overwriting
// would rebind a live identity to a different keypair. The same rule has to hold
// on the certificate axis and for the same reason read the other way round: one
// certificate live on two agent ids means one key holder can authenticate as
// two agents, so the fingerprint stops naming anybody. Refusing at write is what
// keeps certFingerprintOwner's ambiguous arm a recovery-only case.
//
// # AN AGENT REBINDING ITS *OWN* FINGERPRINT IS NOT REFUSED HERE
//
// The comparison skips e.AgentID itself. On this build that case is
// unreachable — the only writer is enrolment, which always carries a
// freshly-minted id — but writing the rule as "already live on a DIFFERENT
// agent" rather than "already live anywhere" is what makes a future
// re-bind/rotate route (task 7a197025) correct by construction instead of
// having to relax this check, which is how a uniqueness rule gets deleted.
func checkCertFingerprintUnbound(byID map[string]RosterEntry, e RosterEntry) error {
	for _, b := range e.CertBindings {
		if b.RetiredAt != nil {
			// A RETIRED binding names history, not a credential this bus will
			// accept, so it constrains nothing and nothing constrains it.
			continue
		}
		for agentID, other := range byID {
			if agentID == e.AgentID {
				continue
			}
			if hasLiveCertBinding(other, b.Fingerprint) {
				return fmt.Errorf("%w: agent %q already holds a live binding for this client certificate, so it cannot also be bound to %q", ErrCertFingerprintBound, agentID, e.AgentID)
			}
		}
	}
	return nil
}

// appendFirstClientCertificateBinding returns the roster entry that binds fp to
// agentID, without mutating byID. The caller must hold the roster lock and must
// apply the returned entry atomically with the durable write.
func appendFirstClientCertificateBinding(byID map[string]RosterEntry, agentID string, fp [32]byte, idempotencyKey string, at time.Time) (RosterEntry, bool, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return RosterEntry{}, false, err
	}
	if fp == ([32]byte{}) {
		return RosterEntry{}, false, fmt.Errorf("%w: the zero fingerprint is the absence of a client certificate", ErrCertBindingUnknown)
	}
	prev, ok := byID[agentID]
	if !ok {
		return RosterEntry{}, false, fmt.Errorf("%w: %q", ErrUnknownAgent, agentID)
	}
	if prev.ClientCertBootstrapIdempotencyKey == idempotencyKey {
		if hasLiveCertBinding(prev, fp) {
			return copyRosterEntry(prev), false, nil
		}
		return RosterEntry{}, false, fmt.Errorf("%w: key %q was applied for a different client certificate", ErrIdempotencyKeyReused, idempotencyKey)
	}
	if hasLiveCertBinding(prev, fp) {
		return copyRosterEntry(prev), false, nil
	}
	for _, b := range prev.CertBindings {
		if b.RetiredAt == nil {
			return RosterEntry{}, false, fmt.Errorf("%w: agent %q already has a different live client certificate binding", ErrCertBindingAlreadyExists, agentID)
		}
	}
	if len(prev.CertBindings) >= MaxCertBindings {
		return RosterEntry{}, false, fmt.Errorf("%w: agent %q already carries %d certificate bindings, the limit is %d", ErrCapacity, agentID, len(prev.CertBindings), MaxCertBindings)
	}
	next := copyRosterEntry(prev)
	next.CertBindings = append(next.CertBindings, newCertBinding(fp, at))
	next.ClientCertBootstrapIdempotencyKey = idempotencyKey
	if err := checkCertFingerprintUnbound(byID, next); err != nil {
		return RosterEntry{}, false, err
	}
	if err := validateRosterEntry(next); err != nil {
		return RosterEntry{}, false, err
	}
	return next, true, nil
}

// hasLiveCertBinding reports whether e accepts fp right now: a binding with this
// fingerprint and no RetiredAt.
//
// ANY live binding counts, not the newest. Rotation legitimately serves two
// certificates at once (invariant 11, and CertBinding's own doc says so in
// terms), so "the newest wins" would refuse the outgoing certificate mid-
// rollover — the one case the two-certificate rule exists to support.
//
// Comparison is Go's == on [32]byte, which is a fixed-size array: it compares
// all 32 bytes, and a fingerprint is a PUBLIC value (anyone who completes a
// handshake can compute it), so there is no secret here for a timing difference
// to leak. It is deliberately not a byte-slice compare — a slice comparison
// would need a length check that an array makes impossible to forget.
func hasLiveCertBinding(e RosterEntry, fp [32]byte) bool {
	for _, b := range e.CertBindings {
		if b.RetiredAt == nil && b.Fingerprint == fp {
			return true
		}
	}
	return false
}

// newCertBinding builds the binding an enrolment records for the certificate the
// client presented on the connection it enrolled over.
//
// RetiredAt is nil: the binding is LIVE from the moment the enrolment is
// acknowledged. BoundAt is the enrolment instant passed in rather than a
// time.Now() read here, so the binding, the entry's Epoch and its EnrolledAt all
// carry the SAME instant — three fields that describe one event and would
// otherwise disagree by a few microseconds, which is enough to make a test that
// compares them flaky and an audit that joins on them wrong.
func newCertBinding(fp [32]byte, at time.Time) CertBinding {
	return CertBinding{Fingerprint: fp, BoundAt: at}
}
