package httpapi

// Invariant 11's CROSS-CHECK, on the AGENT plane (MTLS-CROSSCHECK).
//
// The invariant states it in one sentence: "a session token presented over a
// connection whose client certificate belongs to a DIFFERENT agent must be
// rejected, which is a stronger property than either mechanism gives alone".
// Until this file the two mechanisms ran side by side and never met — mTLS
// proved which key holder was on the connection (clientcert.go), the bearer
// token named an agent (authmw.go), and NOTHING compared them. A stolen token
// was therefore replayable from any machine, and the certificate on the
// connection could not notice.
//
// This file is the comparison. It decides requests; it stores nothing and mints
// nothing. The FACT it checks against is the durable enrolment binding written
// by MTLS-BIND (internal/auth/certbind.go), and the two questions it asks are
// deliberately asked from opposite ends — see enforceCertBinding.
//
// # WHAT THIS DOES NOT COVER. Five named gaps, so nobody reads more into a
// # passing check than it proves.
//
//  1. AN AGENT WITH NO LIVE BINDING IS UNCONSTRAINED ON THE AGENT SIDE. Every
//     agent enrolled before MTLS-BIND (2026-08-14) has no binding at all, and
//     enrolment still accepts a connection that presents no certificate. For
//     such an agent a stolen session token is STILL replayable from a connection
//     presenting no certificate, because there is nothing recorded to require
//     one against. That is the deliberate migration allowance — refusing them all
//     would be a flag day, not a migration — and it closes agent by agent as they
//     re-enrol under MTLS-CLIENTCERT. DO NOT DESCRIBE THIS TASK AS CLOSING TOKEN
//     REPLAY OUTRIGHT. It closes it for every agent that HAS a binding, and it
//     closes certificate-to-agent confusion for all of them.
//  2. THE UNAUTHENTICATED ROUTES ARE NOT CROSS-CHECKED AT ALL. /healthz,
//     /v1/info, /v1/discovery and /v1/enroll reach no principal — there is no
//     agent id to check a certificate against — so authMiddleware returns before
//     this runs. That is what keeps the container healthcheck, pre-enrolment
//     discovery and enrolment itself working; enrolment is where a binding is
//     CREATED, so requiring one there would be circular.
//  3. TEMPORAL: THE CHECK RUNS AT REQUEST ADMISSION ONLY. It is a gate, not a
//     supervisor, exactly as the session check is. A long poll admitted the
//     instant before an operator retires a binding runs to the end of its poll
//     timeout — bounded by hub.MaxPollTimeout (5 minutes), never indefinitely —
//     precisely as it outlives a revoked session. See "KNOWN COVERAGE
//     BOUNDARIES (2)" in authmw.go for the full path and bounds. Closing it is
//     AUTH-2-FU-POLLEXPIRY (03d7ca66-110e-4560-803e-1a7825d1accc), which must now
//     re-evaluate BOTH the session AND this cross-check before delivering, not
//     the session alone.
//  4. A BOUND AGENT WHOSE CERTIFICATE EXPIRES IS LOCKED OUT PERMANENTLY, AND
//     THIS CHANGE CREATES THAT (security gate, MTLS-CROSSCHECK). Say it plainly,
//     because it is the one gap here that is a REGRESSION rather than an
//     unfinished improvement: before this task an expired client certificate was
//     harmless, since nothing authorised on it. Now WithClientCertificate
//     attaches NOTHING for an out-of-date leaf, so guard B refuses EVERY route —
//     including /v1/session/begin, so the agent cannot even obtain a session to
//     ask for help. Client certificates are minted for 365 days and renewal is
//     NOT automatic (client/clientcert.go); CertBinding.RetiredAt is set by no
//     code path; there is no rebind or rotate route. So the only remedy is
//     re-enrolment, which mints a NEW agent id (invariant 1 — ids are never
//     reused), losing the identity and its mailbox.
//
//     This is NOT an argument for admitting an expired certificate: expiry is
//     the only automatic bound on a leaked client key, and RELAY-20 closed the
//     identical hole on the peer plane. It is an argument for a RETIRE/REBIND
//     route, which is owned by task 7a197025 and must land before any real
//     deployment binds certificates it cannot rotate. Until it does, an operator
//     whose agent's certificate has expired must repair the roster record
//     directly.
//  5. PEER ROUTES DO NOT REACH THIS FUNCTION. authMiddleware returns on
//     s.isPeerRoute before the bearer path runs, so a peer request is governed by
//     RequirePeerPrincipal instead. On that surface the certificate alone
//     authorises and there is no PAIR to cross-check — recorded as a NAMED
//     NARROWING of invariant 11 (DECISIONS.md, 2026-08-14, FEDERATION AMENDMENT
//     ruling (i)), not as compliance with it. See the long arm in authMiddleware.

import (
	"errors"
	"net/http"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// crossCheckRefusal is the ONE thing a refused caller ever learns, for every
// case here: no auth service, a certificate bound to somebody else, an ambiguous
// certificate, a required certificate that was absent, and a required
// certificate that did not match.
//
// One fixed string, for peerPrincipalRefusal's reason. The LOG says precisely
// which guard fired; the client is told only that the pair did not match.
//
// # BE PRECISE ABOUT WHAT THIS HIDES, BECAUSE IT IS LESS THAN IT LOOKS
//
// An earlier version of this comment claimed the fixed string stops a caller
// separating "this agent has a binding and you do not hold it" from "this agent
// has none", and called that a defence against mapping which agents are still
// replay-able. THAT CLAIM WAS FALSE and the reviewer gate measured it: the
// STATUS CODE is the channel, and the string cannot close it. At the
// UNAUTHENTICATED POST /v1/session/begin, an anonymous caller presenting no
// certificate at all sees
//
//	a bound agent    -> 403, this string
//	an UNBOUND agent -> 200, with a live challenge token
//	an unknown agent -> 404
//
// so whether a named agent holds a live binding is readable directly, and with
// it the map of which agents this bus still cannot protect from token replay.
//
// THAT IS AN ACCEPTED COST, not an oversight, and it is the price of refusing
// BEFORE the challenge is minted. The alternative — run the gate after
// BeginSession so every caller sees an identical shape — reintroduces exactly
// the defect MTLS-BIND's security gate found on the enrolment path, where the
// refusal ran after Mint and every refused attempt burned a server-minted
// resource. Disclosing migration state is the smaller harm, and the disclosure
// is bounded: it says an agent is not YET certificate-bound, never what its
// certificate is, never whose certificate a presented one is, and it shrinks to
// nothing as agents re-enrol under MTLS-CLIENTCERT.
//
// So what the fixed string ACTUALLY buys is narrower and still worth having: it
// hides WHICH GUARD fired. A caller cannot tell "your certificate belongs to
// someone else" from "this agent requires one and you presented none" from
// "your certificate is live on two agents" — which would otherwise let someone
// holding a certificate probe the roster's binding table from the outside.
// handleSessionBegin's own comment states the same thing; the two must not
// drift apart again.
const crossCheckRefusal = "this credential was not presented over the client certificate it is bound to"

// enforceCertBinding is invariant 11's cross-check for one request, and reports
// whether the request may proceed.
//
// On a refusal it has ALREADY written the response and logged the reason; the
// caller must simply return. That shape is chosen so a call site is one line and
// cannot half-apply the gate.
//
// agentID is the agent this request CLAIMS to act as. Two of the three call
// sites pass a server-minted value (an authenticated principal, a completed
// session); one — /v1/session/begin — passes a client-supplied string straight
// out of the request body, which is why every log line here routes it through
// agentIDLogFields.
//
// # TWO GUARDS, AND EACH ONE IS INDIVIDUALLY NECESSARY
//
// They are not belt-and-braces and they are not each other's backstop. They ask
// the question from OPPOSITE ENDS, and each has a case the other is silent on.
// Deleting either one leaves a live hole while every positive test still passes,
// which is why the cases are enumerated here rather than left to be inferred:
//
//   - ONLY GUARD A COVERS: agent A has NO binding (a legacy agent, gap 1 above)
//     and the connection presents a certificate bound to agent B. Guard B finds
//     an empty requirement for A and says nothing at all — so without guard A,
//     B's certificate would sail through on A's token, which is the exact
//     "belongs to a DIFFERENT agent" sentence of the invariant.
//   - ONLY GUARD B COVERS: agent A HAS a binding and the connection presents NO
//     certificate, or presents one bound to nobody. Guard A never runs (there is
//     no fingerprint to resolve) or reaches no verdict (ErrCertBindingUnknown).
//     THIS IS THE EVASION THE WHOLE TASK EXISTS TO CLOSE: the listener is
//     tls.RequestClientCert and never REQUIRES a certificate, so a rule shaped
//     "if a certificate is present it must match" is defeated by presenting none.
//     A stolen token would replay from anywhere, forever.
//   - ONLY GUARD A CAN REFUSE AN AMBIGUOUS FINGERPRINT, and this is the subtle
//     one. An ambiguous fingerprint is live on TWO agents (reachable from disk
//     only — see certFingerprintOwner, whose write-side check cannot run during
//     recovery). Guard B would find it present in BOTH agents' live sets and
//     admit EITHER, so ONE key holder would authenticate as TWO agents: the
//     certificate would have stopped naming anybody, which is precisely the
//     confusion invariant 11 exists to prevent. Guard A declines to guess and
//     refuses; guard B structurally cannot.
//
// ErrCertBindingUnknown is the one answer guard A deliberately does NOT act on.
// An unbound certificate is the ordinary case on this build — a client that grew
// a keypair before its bus recorded bindings presents one on every request — and
// refusing it here would make an unbound certificate strictly worse than none,
// which punishes the clients that are ahead of the migration. Whether it is
// tolerable is GUARD B's decision, per agent: unbound and the agent requires
// nothing, admit; unbound and the agent requires something, refuse.
//
// # THE ORDER MATTERS ONLY FOR THE LOG
//
// Both guards are pure reads and neither can be satisfied by the other's
// failure, so the verdict is the same either way. Guard A runs first because its
// refusal is the more specific one — it can NAME the agent the certificate
// actually belongs to, which is what an operator needs — and a request that
// trips both should be reported as the confusion it is rather than as a missing
// certificate.
//
// # WHY 403 AND NOT 401
//
// 403, with NO WWW-Authenticate header, mirroring writePeerForbidden. RFC 7235
// REQUIRES a challenge on a 401, the only challenge this server speaks is
// `Bearer`, and inviting a refused caller to retry with a different bearer token
// would advertise exactly the credential confusion this gate prevents. It would
// also be a lie: the wrong half of the pair is the CONNECTION's client
// certificate, which is chosen at handshake time and cannot be changed by
// resending a header.
//
// THE ACKNOWLEDGED TRADE, recorded rather than hidden: 403 here and 401 in
// authMiddleware does let a caller distinguish "valid token, wrong certificate"
// from "invalid token". That is a real distinction and it is accepted, because
// reaching the 403 at all requires ALREADY HOLDING A VALID SESSION TOKEN — 32
// random bytes minted by internal/auth — so the oracle is available only to
// someone who has the credential whose validity it would confirm. Collapsing it
// into a 401 would cost the honest client the one signal that tells it to
// re-enrol with its current keypair instead of retrying forever.
//
// # IT NEVER DISCONNECTS (invariant 10)
//
// No "Connection: close", on any path here, ever. Both of invariant 10's
// questions answer the wrong way for a disconnect: a merely BUGGY client reaches
// this line trivially — an agent that regenerated its client keypair without
// re-enrolling hits it on every single request — and a connection does not carry
// only one principal's traffic, so dropping the socket would destroy other
// requests pipelined on it, including a parked long poll belonging to whoever
// actually owns the machine.
func (s *Server) enforceCertBinding(w http.ResponseWriter, r *http.Request, agentID string) bool {
	if s.auth == nil {
		// FAIL CLOSED. A server that cannot resolve a binding cannot prove one is
		// satisfied, and "we could not check, so we allowed it" is how a control
		// disappears under a misconfiguration. Warn, not Debug: it is an operator
		// error and every agent-facing route is about to answer 403.
		s.log.Warn("request refused by the client-certificate cross-check: this server was built without an auth service, so no binding can be resolved and none can be proved satisfied",
			s.crossCheckLogFields(r, agentID, "no-auth-service")...)
		s.writeCrossCheckForbidden(w, r)
		return false
	}

	if agentID == "" {
		// FAIL CLOSED ON "NOBODY WAS NAMED" (reviewer gate, MTLS-CROSSCHECK).
		//
		// An empty id is NOT "an agent that happens to hold no binding". It is NO
		// AGENT AT ALL, and a gate asked to check a credential against nobody has
		// been asked a question it cannot answer -- exactly the s.auth == nil case
		// above, arriving from the other direction.
		//
		// WHY THIS IS NOT REDUNDANT WITH THE CALLERS. Every current call site is
		// safe: two pass a server-minted id, and /v1/session/begin's empty value is
		// refused downstream by BeginSession's 404. But that safety is a fact about
		// the CALLERS, not about this function, and a gate whose correctness is
		// argued from its callers stops being correct the moment a fourth one is
		// added -- which is the shape this repo has been bitten by repeatedly. It is
		// cheap to make the property local, so it is made local.
		//
		// PARTIAL COVER ALREADY EXISTED, AND ONLY PARTIAL: with a resolvable
		// certificate on the connection, guard A already refused an empty id
		// (owner != ""). It was the guard-B arm -- no certificate presented, so no
		// live set to require -- that admitted. This closes that arm.
		s.log.Warn("request refused by the client-certificate cross-check: no agent was named, so there is nothing to check the connection's certificate against (invariant 11)",
			s.crossCheckLogFields(r, agentID, "no-agent-named")...)
		s.writeCrossCheckForbidden(w, r)
		return false
	}

	fp, present := ClientCertFingerprintFromContext(r.Context())

	// GUARD A -- THE CERTIFICATE SIDE. "whose client certificate belongs to a
	// DIFFERENT agent".
	if present {
		owner, err := s.auth.AgentIDForClientCertificate([32]byte(fp))
		switch {
		case err == nil && owner != agentID:
			// The owner is a SERVER-MINTED agent id out of the roster, bounded and
			// validated, so it is logged in full: an operator diagnosing this needs
			// to know whose certificate it was. It is NOT returned to the client --
			// that would map a certificate an attacker possesses to an agent id on
			// this bus.
			kv := append(s.crossCheckLogFields(r, agentID, "certificate-belongs-to-another-agent"),
				"client_cert_sha256", fp.String(),
				"bound_agent_id", owner,
			)
			s.log.Warn("request refused by the client-certificate cross-check: the connection's client certificate is bound to a DIFFERENT agent than the credential names (invariant 11)", kv...)
			s.writeCrossCheckForbidden(w, r)
			return false

		case errors.Is(err, auth.ErrCertBindingAmbiguous):
			// One certificate live on two agents. See the doc comment: only this
			// guard can refuse it, and it must, or one key holder authenticates as
			// two agents. The wrapped error NAMES the holders, sorted, which is
			// what the operator needs in order to retire all but one.
			kv := append(s.crossCheckLogFields(r, agentID, "certificate-ambiguous"),
				"client_cert_sha256", fp.String(),
				"err", err,
			)
			s.log.Warn("request refused by the client-certificate cross-check: this client certificate is live on more than one agent, so it names nobody until an operator retires all but one (invariant 11)", kv...)
			s.writeCrossCheckForbidden(w, r)
			return false

			// errors.Is(err, auth.ErrCertBindingUnknown) and the matching case
			// (err == nil && owner == agentID) both fall through DELIBERATELY. The
			// first reaches NO VERDICT here -- guard B decides whether an unbound
			// certificate is tolerable for this agent. The second is the happy path
			// of this guard and still has to face guard B, because an agent may
			// hold SEVERAL live bindings during a rotation and this one being live
			// is already the answer guard B will compute.
		}
	}

	// GUARD B -- THE AGENT SIDE. Fail closed on ABSENCE, per agent.
	//
	// ANY live binding satisfies it, not the newest: rotation legitimately serves
	// two certificates at once (invariant 11), so requiring the latest would
	// refuse the outgoing certificate mid-rollover -- the one case the
	// two-certificate rule exists to support. hasLiveCertBinding takes the same
	// position on the write side, deliberately.
	live := s.auth.LiveCertBindings(agentID)
	if len(live) > 0 && !(present && containsFingerprint(live, [32]byte(fp))) {
		kv := s.crossCheckLogFields(r, agentID, "required-certificate-not-presented")
		kv = append(kv, "live_binding_count", len(live), "client_cert_presented", present)
		if present {
			kv = append(kv, "client_cert_sha256", fp.String())
		}
		s.log.Warn("request refused by the client-certificate cross-check: this agent has a live client-certificate binding and the connection did not present a certificate matching it (invariant 11)", kv...)
		s.writeCrossCheckForbidden(w, r)
		return false
	}

	return true
}

// writeCrossCheckForbidden answers 403 with the standard error body and the one
// fixed reason. It carries NO WWW-Authenticate header, and NO "Connection:
// close" -- see enforceCertBinding for both.
func (s *Server) writeCrossCheckForbidden(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusForbidden, ErrorResponse{Error: crossCheckRefusal})
}

// crossCheckLogFields is the common prefix of every refusal line here: the
// request, the agent id rendered SAFELY, and which guard fired.
//
// guard is a fixed compile-time string, never derived from input, so an operator
// can grep for one case without the field itself becoming a channel for
// attacker-chosen bytes.
//
// THE BEARER TOKEN APPEARS NOWHERE ON ANY PATH IN THIS FILE -- not truncated,
// not hashed, not inside an error. The client-certificate FINGERPRINT is logged
// where there is one, because it is PUBLIC data (anyone who completes a
// handshake with that peer can compute it) and it is the exact value an operator
// needs to correlate with a roster binding.
func (s *Server) crossCheckLogFields(r *http.Request, agentID, guard string) []interface{} {
	kv := []interface{}{
		"request_id", RequestIDFromContext(r.Context()),
		"method", r.Method,
		"path", r.URL.Path,
		"guard", guard,
	}
	return append(kv, agentIDLogFields(agentID)...)
}

// agentIDLogFields renders an agent id for a log line: the value itself when it
// is a WELL-FORMED id, and its LENGTH -- never its bytes -- when it is not.
//
// THIS IS ABOUT VOLUME, NOT ESCAPING, and it is the same discipline
// inviteIDLogFields applies for the same reason. logging.writeValue already runs
// every field through strconv.Quote, so the bytes are safe to write; the problem
// is WHO CHOOSES THEM AND HOW MANY. POST /v1/session/begin is unauthenticated,
// this server rate-limits nothing, and MaxAuthRequestBytes (8 KiB) lets an
// anonymous caller put a kilobyte of chosen bytes -- internal/logging's
// per-value cap -- into a Warn-level record on every cheap request. That is log
// amplification, and this gate is reached BEFORE anything has validated the id.
//
// A well-formed id is logged in full and must stay that way: it is bounded by
// ids.MaxAgentIDLen, and an operator correlating a refused request needs it.
// Everything else gets "agent_id_len" only -- deliberately NOT a truncated
// prefix, because a prefix of an attacker-chosen id is still attacker-chosen
// bytes, and it invites the next reader to "just log a bit more".
//
// AND THE OTHER HALF, which is easy to miss: this helper controls only the
// agent_id FIELD. No error constructed on this path may echo the raw value back
// through an "err" field -- the exact hole inviteIDLogFields documents, where a
// wrapped sentinel smuggled the id back into the record the helper had just
// taken it out of. The only "err" logged in this file is
// auth.ErrCertBindingAmbiguous's, which names roster agent ids (server-minted)
// and never the caller's string. ids.ParseAgentID is likewise never wrapped into
// a log line here: its messages quote the id.
//
// ParseAgentID, not a length check: it establishes only that the string is a
// well-formed <bus-id>.<agent-id> (invariant 2). It grants no authority and says
// nothing about whether that agent exists -- which is all this helper needs, and
// all it may be read as.
func agentIDLogFields(agentID string) []interface{} {
	if _, _, _, err := ids.ParseAgentID(agentID); err != nil {
		return []interface{}{"agent_id_len", len(agentID)}
	}
	return []interface{}{"agent_id", agentID}
}

// containsFingerprint reports whether fp is one of the fingerprints in live.
//
// Comparison is Go's == on [32]byte, which is a fixed-size array: it compares
// all 32 bytes, and a fingerprint is PUBLIC data -- anyone who completes a
// handshake with that peer can compute it -- so there is no secret here for a
// timing difference to leak. hasLiveCertBinding says the same thing on the
// storage side, deliberately in the same terms. It is not a byte-slice compare:
// a slice comparison needs a length check that an array makes impossible to
// forget.
// THE ZERO FINGERPRINT NEVER MATCHES, EVEN AGAINST A ZERO BINDING (security
// gate, MTLS-CROSSCHECK). It is the value a caller holds when there was NO
// certificate, so matching it would turn "this connection presented nothing"
// into "this connection satisfies the requirement" — the fail-OPEN, and the
// worst outcome available on this path.
//
// It is unreachable TODAY: enforceCertBinding only calls this behind
// `present &&`, and Go short-circuits, so a no-certificate request never gets
// here. THAT IS EXACTLY WHY THE GUARD IS HERE. The property was resting on a
// single token in a condition one file away: deleting `present &&` — a
// plausible "simplification", since the expression still compiles and every
// positive test still passes — would let a request presenting NO certificate
// match a damaged roster record carrying a live zero-fingerprint binding, and
// be ADMITTED. LiveCertBindings deliberately KEEPS such a binding precisely so
// that agent is unsatisfiable; without this guard that intent would invert into
// the one case that satisfies it.
//
// internal/auth/certbind.go's certFingerprintOwner refuses the zero value in
// the same terms and for the same reason, as does internal/relay/peerstore.go
// on the peer plane. Three planes, one rule: the zero digest is the ABSENCE of
// a certificate, never a certificate.
func containsFingerprint(live [][32]byte, fp [32]byte) bool {
	if fp == ([32]byte{}) {
		return false
	}
	for _, b := range live {
		if b == fp {
			return true
		}
	}
	return false
}
