package auth

// The AGENT-SIDE half of invariant 11's cross-check (MTLS-CROSSCHECK).
//
// certbind.go answers the CERTIFICATE-side question — "which agent does this
// fingerprint name?" — and that answer alone is not enough to enforce the
// invariant. It is silent on the case that actually matters: a connection that
// presents NO certificate at all. The listener is tls.RequestClientCert
// (cmd/agent-bus/tlslisten.go), so a caller can simply omit one, and a rule
// shaped "if a certificate is present it must match" is defeated by not
// presenting one. Closing that needs the question asked the other way round —
// "does THIS AGENT require a certificate, and if so which?" — which is what
// this file answers, and nothing else.
//
// It decides no request. internal/httpapi's enforceCertBinding does that.

// LiveCertBindings returns the fingerprints of every LIVE client-certificate
// binding held by agentID, in the order they are stored, or nil when the agent
// is unknown or holds none.
//
// It is the read a cross-check needs and AgentIDForClientCertificate cannot
// give: that function starts from a fingerprint the connection presented, so it
// cannot say anything about a connection that presented none. This one starts
// from the AGENT, so an ABSENT certificate is a case it can decide.
//
// # A NIL RETURN MEANS "THIS AGENT HAS NO CERTIFICATE REQUIREMENT" — AND THAT
// # IS A DELIBERATE ALLOWANCE, NOT A GENERAL "NO CONSTRAINT APPLIES"
//
// Read the empty answer narrowly. It means exactly one thing: this agent
// enrolled over a connection that bound no certificate, so there is nothing on
// the agent side to check a presented certificate against. It does NOT mean the
// request is unconstrained — the certificate-side guard still applies, because a
// connection presenting a certificate bound to a DIFFERENT agent must be refused
// whether or not the named agent has a binding of its own.
//
// The allowance exists because bindings only started being written by MTLS-BIND
// (2026-08-14): every agent enrolled before it has none, and refusing them all
// would be a flag day rather than a migration. It is a NAMED GAP — a stolen
// session token for such an agent is still replayable from a connection carrying
// no certificate — and it closes agent by agent as they re-enrol under
// MTLS-CLIENTCERT. Do not read an empty answer as a finished authorisation, and
// do not describe the gate built on it as closing token replay outright.
//
// # A ZERO FINGERPRINT IS INCLUDED, WHICH FAILS CLOSED. FILTERING IT WOULD FAIL
// # OPEN.
//
// A live binding carrying the zero fingerprint is included in the result exactly
// like any other, and that is the fail-closed choice rather than an oversight.
//
// The zero value can only reach the roster from a damaged or hand-edited durable
// record — Enrol binds only a non-nil fingerprint taken from a real parsed
// certificate, and validateRosterEntry checks a binding's BoundAt and RetiredAt
// but does NOT reject a zero Fingerprint, so such a record decodes cleanly and is
// stored LIVE. It can never equal a real certificate's digest (sha256 of a DER
// encoding is not zero for anything a client can present), and certFingerprintOwner
// refuses to resolve it in terms.
//
// So the two options are:
//
//   - INCLUDE it, as here. The agent has a live binding no presented certificate
//     can ever satisfy, so every request naming it is refused until an operator
//     repairs the record. The agent is UNSATISFIABLE — loud, contained, fixable.
//   - FILTER it. The agent would then look UNBOUND, and the requirement recorded
//     against it would silently evaporate — an agent whose durable record is
//     DAMAGED would end up with LESS enforcement than one whose record is intact,
//     which is precisely backwards.
//
// The first is an outage for one agent; the second is a security control that
// disappears when a record rots, with nothing to notice it.
//
// # LOCKING
//
// None is taken here and none may be added. Roster.Get acquires the roster's own
// lock and returns a DEEP COPY of the entry, CertBindings included
// (copyRosterEntry), so the slice this function reads is private to this call and
// cannot be mutated underneath it. A second lock in this package over the same
// data would be a second, differently-ordered view of the roster — the exact
// thing certbind.go declines to build an index for.
func (s *Service) LiveCertBindings(agentID string) [][32]byte {
	e, ok := s.roster.Get(agentID)
	if !ok {
		// An unknown agent has no requirement to state. It is not this
		// function's job to refuse it: the caller is holding either a
		// server-minted principal (so the agent exists) or a client-supplied id
		// that BeginSession will refuse for itself, with the 404 that route
		// already gives.
		return nil
	}

	var out [][32]byte
	for _, b := range e.CertBindings {
		if b.RetiredAt != nil {
			// A retired binding names history, not a credential this bus will
			// accept. Counting it would keep refusing an agent that had
			// legitimately rotated away from it — the mirror of
			// hasLiveCertBinding's rule, and it must stay the mirror.
			continue
		}
		out = append(out, b.Fingerprint)
	}
	return out
}
