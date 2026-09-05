package httpapi

// The FEDERATION INGRESS mount (RELAY-20).
//
// This file is the one place /v1/peer/enroll, /v1/peer/relay and
// /v1/peer/roster become reachable, and it is deliberately the only place in
// the repository outside internal/relay that names those paths at all — see
// internal/relay/guards_test.go, which enforces exactly that.
//
// # WHAT CHANGED, AND WHAT DID NOT
//
// Until this task the three relay handlers existed and were registered nowhere.
// They authenticate NOTHING themselves and that has not changed: relay.Handler,
// relay.RelayHandler and relay.RosterHandler still read no credential, and they
// must never be handed to a mux by anything but mountPeerRoute below. What this
// file adds is the principal in front of them.
//
// # THE THREE RULES THIS MOUNT EXISTS TO KEEP
//
//  1. NO PEER PATH IS ON unauthenticatedRoutes. That map means "served with NO
//     credential at all"; a peer route is served with a credential — the TLS
//     client certificate — and putting it on the allow-list would create the
//     ungated federation path the retired guard forbade. mountPeerRoute REFUSES
//     to register a pattern that is on the allow-list, so the two can never be
//     reconciled by accident.
//
//  2. THE ROUTES REGISTER ONLY WHEN THE WHOLE CHAIN IS PRESENT, and when any
//     link is missing they are NOT REGISTERED AT ALL — the catch-all answers
//     404, exactly like any other path this build does not serve. Never a
//     registered 503: a route that exists and refuses is a claim that the
//     surface is there, readable by an unauthenticated caller, and on THIS
//     surface that claim tells a stranger the bus federates.
//
//  3. THE PEER PRINCIPAL IS THE ONLY WAY IN. Every peer route is wrapped by
//     (*Server).RequirePeerPrincipal, in the same function that records the path
//     as a peer route, so a path cannot become bearer-exempt without also
//     becoming certificate-gated. That coupling is the whole security argument
//     of this file; see mountPeerRoute.
//
// # WHY A PEER ROUTE DOES NOT TAKE A BEARER TOKEN, AND WHY THAT IS NOT A HOLE
//
// authMiddleware is default-deny over the whole mux: anything not on the
// allow-list needs a session token. This file used to argue that a peer bus
// holds no session token of ours, carries no Ed25519 enrolment key here, and
// could not obtain either — so that a bearer requirement would be unsatisfiable,
// the same shape as /v1/session/complete. THAT PREMISE IS FALSE, refuted by a
// security gate and corrected in DECISIONS.md (2026-08-14, the FEDERATION
// AMENDMENT's ruling (i) — NOT the 2026-08-08 FEDERATION section, whose rulings
// stop at (f); ruling (a) cited below IS 2026-08-08, so check the date, not just
// the letter): enrolment is open to a peer bus like any other client,
// so a peer bus is ALSO an enrolled principal on the buses it peers with and a
// peer request may well carry a valid session token. peerprincipal.go's "THE
// AGENT PRINCIPAL IS REMOVED" section is written on exactly that assumption.
//
// The true reason is CONFLATION: a session token names an AGENT, and a peer
// route is BUS-scoped. Requiring one here would make an agent credential
// authorise a peer route — one principal accepted as the other. So the bearer
// requirement would be SATISFIABLE AND WRONG, which is a stronger objection than
// unsatisfiability and points the opposite way: not "the allow-list is harmless
// here", but "an AGENT-scoped token must never be consulted on this surface".
// The qualifier is load-bearing rather than pedantic — ruling (i) reverses the
// moment a BUS-SCOPED bearer credential exists, one naming the peer bus rather
// than an agent, and at that point requiring it here becomes right rather than
// wrong. What is banned is the conflation, not the second factor.
//
// So a peer route is authenticated by a DIFFERENT authenticator, not by none:
// authMiddleware hands the request to the mux, and RequirePeerPrincipal — which
// is fail-closed, refuses when no resolver is configured, refuses when there is
// no TLS, refuses when no certificate was presented and refuses when the leaf
// resolves to no single adjacent bus — decides. The two authenticators are
// disjoint by construction and neither is ever accepted as the other: they live
// under different context keys, have different Go types, and RequirePeerPrincipal
// SHADOWS OUT any agent principal before the handler runs.
//
// This is the point INVARIANT 11's cross-check ("a session token presented over
// a connection whose client certificate belongs to a different agent must be
// rejected") reduces to on this surface — invariant 11, not invariant 3, which
// this comment misattributed it to until 2026-08-14; invariant 3 governs
// invite-only enrolment and the client-signs-a-server-token direction, and the
// cross-check clause has never been part of it. There is no pair to cross-check
// here, because a peer handler never sees an agent principal at all — the
// stronger of the two available answers, and the reason it is stated here rather
// than left implicit.
//
// BUT RULING (i) NAMES THAT A NARROWING OF INVARIANT 11, NOT COMPLIANCE WITH IT,
// and this file should not claim otherwise. Invariant 11 asks for TWO factors,
// cross-checked; ONE authorises here — the certificate. What is given up is the
// revocable, time-bounded half: a peer link is withdrawn by an OFFLINE operator
// action that needs a RESTART, not by online revocation, and nothing caps a peer
// certificate's NotAfter, so even expiry bounds a peer's authority only as far as
// the peer chose to bound itself. See RESIDUAL 2 below for the invite half of the
// same narrowing.
//
// # KNOWN RESIDUAL 1: WHETHER THIS BUS FEDERATES IS OBSERVABLE PRE-AUTH
//
// Found while writing this mount, and recorded rather than papered over. The
// refusal codes differ by build:
//
//	federating build,     anonymous POST /v1/peer/relay -> 403 (the peer gate)
//	non-federating build, anonymous POST /v1/peer/relay -> 401 (default-deny)
//
// so one request distinguishes "this bus serves federation ingress" from "it
// does not". That is a genuine new disclosure. Everywhere else this server is
// careful not to leak its shape: the catch-all is registered THROUGH s.route so
// an anonymous caller gets 401 for known and unknown paths alike, and the
// discovery document deliberately does not report which optional surfaces a
// build registered.
//
// IT IS NOT CLOSED, AND THE CHEAP FIXES ARE WORSE. Answering 401 here would
// require a WWW-Authenticate challenge (RFC 7235), and the only scheme this
// server speaks is Bearer — which invites a refused peer to retry with a session
// token, advertising exactly the credential confusion this gate exists to
// prevent. Answering 401 only when NO certificate was presented closes the
// no-certificate probe and nothing else, since anyone can mint a self-signed
// certificate and probe again. Both trade a real property for a cosmetic one.
//
// WHAT BOUNDS IT: DECISIONS.md (2026-08-08, FEDERATION) ruling (a) — every
// bus-to-bus link is an SSH tunnel and no bus process listens publicly, so the
// prober must already have reached the loopback listener.
//
// STATED THAT WAY ON PURPOSE, because ruling (h) corrects the overstatement this
// comment carried until 2026-08-14: that the deployed topology leaves nobody able
// to send this probe. THE PROBER EXISTS. Every enrolled agent on the loopback
// listener can send this request, and so can anything at the far end of the
// tunnel — the probe needs no credential, which is what makes it PRE-auth.
// What is bounded is the SET OF PARTIES who can ask: parties the operator has
// already admitted to the machine. What they learn is one bit — "this bus
// federates" — and no peer id, no roster and no count.
//
// That is the SAME GROUND ruling (b) STANDS ON for INVITE-GATE — ground, not a
// deferral, which is the word (b-CLARIFIED) exists to disclaim: INVITE-GATE is
// neither deferred nor deprioritised, it remains P0, and ruling (b) says only
// that it does not BLOCK the federation critical path. It REVERSES ON THE SAME
// TRIGGERS: a bus bound to a non-loopback interface, a tunnel endpoint
// shared with a non-operator, or a second local user on any of the machines.
//
// # KNOWN RESIDUAL 2: THE CREDENTIAL IS OPERATOR-INSTALLED, NOT INVITE-REDEEMED
//
// Passing this gate
// proves possession of a bound certificate's private key. Invariant 3 says
// invites are the only way onto the bus "including for peer buses", and the
// binding this gate reads is installed by an operator with `agent-bus peer add`,
// NOT redeemed from a single-use invite. That is a real difference from agent
// enrolment and it is INVITE-PEERGUARD's (f5d91dbe) to close; it is not made
// worse by mounting, because before this task the same handlers were reachable
// by nobody and the same invite was redeemed by nobody.

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/dodgymike/agent-bus/internal/relay"
)

// PeerSurface is the federation ingress a build serves, or nil when it does not
// federate.
//
// EVERY FIELD IS REQUIRED. A partial surface is a wiring bug, and it is treated
// as "this build does not federate" — nothing is registered — rather than as
// "serve the parts that are present", because a bus serving /v1/peer/enroll but
// not /v1/peer/relay accepts a peering it can never carry traffic for, and the
// peer discovers that only when a message is silently unroutable.
//
// # WHY Registry AND Trust ARE HERE WHEN THIS PACKAGE READS NEITHER
//
// They are the DECLARED PRECONDITION, and their presence is the gate the task
// record names: routes register only when registry AND trust are both non-nil.
// The composition root (RELAY-24) builds both; if either is missing, federation
// cannot work end to end even though the handlers would construct — and the
// failure would be invisible, because an unregistered route and a route whose
// backing state is empty look identical from the outside.
//
// Two of the three are also enforced structurally by internal/relay itself, and
// that redundancy is deliberate rather than sloppy: relay.NewRelayHandler
// REFUSES a nil CrossBusTrust, so a non-nil Relay implies a trust chain existed
// at construction, and relay.NewRosterHandler refuses a nil Apply, which is
// where the wiring site calls Registry.ApplyRosterUpdate. Stating the
// precondition here as well makes it checkable at the mount rather than
// inferable from two other packages' constructors.
//
// THE HANDLER FIELDS ARE CONCRETE TYPES, NOT http.Handler, ON PURPOSE. A
// wiring site must not be able to serve some other handler — or its own
// wrapper of one — at a peer path. The path constants come from internal/relay
// too, so neither half of "which handler at which path" is caller-chosen.
type PeerSurface struct {
	// Enroll answers POST /v1/peer/enroll: the peering handshake.
	Enroll *relay.Handler

	// Relay answers POST /v1/peer/relay: cross-bus message ingest.
	Relay *relay.RelayHandler

	// Roster answers POST /v1/peer/roster: the ongoing roster sync.
	Roster *relay.RosterHandler

	// Ack answers POST /v1/peer/ack: peer-hop delivery ACK/NACK ingest (ACK-3).
	//
	// ITS TYPE IS NOT http.Handler AND THAT IS DELIBERATE. *relay.AckHandler has
	// no ServeHTTP; it has ServeAuthenticated(w, r, peerBusID), and the peer bus
	// id it takes MUST be the one RequirePeerPrincipal resolved from the TLS
	// client certificate. relay.AuthorizePeerAck's anti-forgery rule authorises
	// DeriveJobID(peerBusID, correlationKey), so a peerBusID sourced from
	// anywhere a remote party can influence would authorise the NAME A PEER CHOSE
	// rather than the peer that sent the frame — and every guard in
	// internal/relay/ack.go would become decorative while every positive test
	// still passed.
	//
	// Making the field a non-http.Handler is what turns that from a comment into
	// a compile error: this route cannot be handed to mountPeerRoute directly,
	// so somebody must write servePeerAck below, and at that point the ONLY
	// source of a bus id in scope is PeerPrincipalFromContext.
	Ack *relay.AckHandler

	// Registry is the peer roster and routing table the handlers' callbacks
	// mutate. Required; see the type doc for why it is named here.
	Registry *relay.Registry

	// Trust yields the peering-time bus signing-key pins every relayed message
	// is verified against. Required; see the type doc.
	Trust relay.CrossBusTrust
}

// missingParts names every required field that is nil, for the operator-facing
// log line. It returns nil for a complete surface.
//
// It reports ALL of them rather than the first, because a half-wired
// composition root is usually missing several and a one-at-a-time diagnosis is
// several restarts.
func (p *PeerSurface) missingParts() []string {
	if p == nil {
		return []string{"the whole PeerSurface"}
	}
	var missing []string
	if p.Enroll == nil {
		missing = append(missing, "Enroll")
	}
	if p.Relay == nil {
		missing = append(missing, "Relay")
	}
	if p.Roster == nil {
		missing = append(missing, "Roster")
	}
	if p.Ack == nil {
		missing = append(missing, "Ack")
	}
	if p.Registry == nil {
		missing = append(missing, "Registry")
	}
	if p.Trust == nil {
		missing = append(missing, "Trust")
	}
	return missing
}

// PeerRoutes returns the peer-bus patterns this server registered, sorted, or
// an empty slice when this build does not federate.
//
// It returns a COPY, for the reason UnauthenticatedRoutes does: the set is a
// security boundary — membership is what makes authMiddleware hand a request to
// the certificate gate instead of demanding a bearer token — and no caller gets
// a handle that could add to it.
func (s *Server) PeerRoutes() []string {
	out := make([]string, 0, len(s.peerRoutes))
	for p := range s.peerRoutes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// peerSurfaceMounted reports whether this build registered any peer-bus route,
// i.e. the federation ingress is being served (RELAY-55). It reads s.peerRoutes
// HERE, in peermount.go — the sole file TestPeerRoutesSetHasExactlyOneWriter
// permits to name that field — so handleReadyz can report federation readiness
// without touching the map directly. A boolean, not a copy of the set: the
// caller only needs "did it mount", and no membership escapes.
func (s *Server) peerSurfaceMounted() bool {
	return len(s.peerRoutes) > 0
}

// isPeerRoute reports whether path is a registered peer-bus route.
//
// Matching is EXACT string equality against r.URL.Path — no prefix match, no
// cleaning, no trailing-slash tolerance — for the reason unauthenticatedRoutes
// gives: a non-canonical spelling simply is not in this map, so it falls through
// to the bearer requirement and is refused. Being strict here fails closed;
// being lenient is how a normalisation mismatch between this check and the mux
// becomes a bypass.
func (s *Server) isPeerRoute(path string) bool {
	if len(s.peerRoutes) == 0 {
		return false
	}
	_, ok := s.peerRoutes[path]
	return ok
}

// mountPeerSurface registers the federation ingress, or registers nothing.
//
// Called from New with the mux, before the middleware is wrapped around it.
// Every refusal path here is silent to the network (the routes simply do not
// exist) and LOUD in the log, because "this build does not federate" and "this
// build was meant to federate and is mis-wired" are indistinguishable from
// outside and must not be from inside.
func (s *Server) mountPeerSurface(mux *http.ServeMux) {
	if s.peer == nil {
		// The ordinary case for every non-federating build. Not even Debug:
		// there is nothing to say about a surface nobody asked for.
		return
	}

	if missing := s.peer.missingParts(); len(missing) > 0 {
		s.log.Error("FEDERATION IS NOT SERVED: a PeerSurface was supplied but is incomplete, so no peer route is registered and every /v1/peer/ path answers 404",
			"missing", fmt.Sprintf("%v", missing),
			"remedy", "supply every field of httpapi.PeerSurface, or supply none; a partial surface would accept a peering this bus cannot carry traffic for",
		)
		return
	}

	// THE RESOLVER IS PART OF THE CHAIN. Without it RequirePeerPrincipal
	// refuses every request, so registering would put three routes on the wire
	// that answer 403 to everyone — a registered-and-refusing surface, which is
	// exactly the shape rule 2 above forbids. It is also the one missing link
	// that would otherwise LOOK like it worked, because the refusal is the same
	// 403 an unbound certificate gets.
	if s.peerPrincipals == nil {
		s.log.Error("FEDERATION IS NOT SERVED: a complete PeerSurface was supplied but no inbound peer principal resolver was, so this bus could authorise no peer bus and no peer route is registered",
			"remedy", "set httpapi.Options.PeerPrincipals (satisfied by *relay.PeerStore); a peer route mounted without it would answer 403 to every peer, which advertises the surface while serving nobody",
		)
		return
	}

	s.mountPeerRoute(mux, relay.PeerEnrollPath, s.peer.Enroll)
	s.mountPeerRoute(mux, relay.PeerRelayPath, s.peer.Relay)
	s.mountPeerRoute(mux, relay.PeerRosterPath, s.peer.Roster)
	s.mountPeerRoute(mux, relay.PeerAckPath, http.HandlerFunc(s.servePeerAck))
}

// servePeerAck is THE ONE PLACE the authenticated peer principal meets the ACK
// handler, and the only place a peer bus id is supplied to
// relay.AuthorizePeerAck's binding rule.
//
// # THE BUS ID COMES FROM THE CERTIFICATE. IT MUST NEVER COME FROM THE REQUEST
//
// principal.BusID is what RequirePeerPrincipal resolved by looking the presented
// client certificate's fingerprint up in the durable peer binding. It is the ONE
// identity on this surface that was proved rather than asserted.
//
// A version of this function that read the bus id from anywhere else — a frame
// field, a header, a query parameter — would still compile, still pass every
// positive test, and would silently authorise any peer to settle any other
// peer's obligations. relay's ACK frame carries no bus-id field precisely so
// there is nothing here to reach for; a header would have to be invented on
// purpose. If you are editing this line, that is what you are editing.
//
// # AND IT IS DELIBERATELY THE ONLY THING THIS FUNCTION DOES
//
// Nothing is validated, decoded, logged or decided here. Everything else belongs
// to relay.AckHandler, which owns the wire vocabulary and the status mapping, so
// that this adapter cannot acquire a second, drifting copy of any rule. Its
// entire content is "read the proved identity, hand it over".
func (s *Server) servePeerAck(w http.ResponseWriter, r *http.Request) {
	principal, ok := PeerPrincipalFromContext(r.Context())
	if !ok {
		// UNREACHABLE: mountPeerRoute wraps this in RequirePeerPrincipal, which
		// is fail-closed and refuses before the handler runs. Refused rather than
		// passed with an empty id, because an empty peer id derives a job id
		// nobody owns — every legitimate acknowledgement would be refused with the
		// uniform answer and the surface would look exactly like a working
		// anti-forgery rule while settling nothing.
		//
		// It answers the SAME fixed refusal as every other case in that gate, so
		// an anonymous caller cannot tell a mis-wired build from an unbound
		// certificate.
		s.log.Error("REFUSING a peer acknowledgement: the request reached the ACK route with no peer principal in its context, which means RequirePeerPrincipal did not run. The route is being served WITHOUT its authenticator",
			"remedy", "mount it only through mountPeerRoute; nothing may be inserted between the gate and this handler",
		)
		s.writePeerForbidden(w, r)
		return
	}
	s.peer.Ack.ServeAuthenticated(w, r, principal.BusID)
}

// mountPeerRoute registers ONE peer route behind the peer principal and records
// it as a peer route, in that order and in one function.
//
// # THE COUPLING IS THE SECURITY ARGUMENT — do not split this function
//
// Recording a path in s.peerRoutes is what makes authMiddleware hand it to the
// certificate gate instead of demanding a bearer token. Wrapping it in
// RequirePeerPrincipal is what makes the certificate gate run. SPLIT ACROSS TWO
// CALL SITES, one could be done without the other, and the failure is silent
// and total: a path recorded but not wrapped is served to ANYONE, with no
// credential of any kind, and every positive test still passes because a
// legitimate peer's request succeeds either way.
//
// So they happen here, together, and nothing else in this package writes to
// s.peerRoutes.
//
// # THE WRAPPER ORDER IS ALSO LOAD-BEARING
//
// The composed handler is
//
//	authMiddleware( RequirePeerPrincipal( h ) )
//
// with authMiddleware applied by New around the whole mux. RequirePeerPrincipal
// is the INNERMOST auth-bearing wrapper, which its own doc requires: it shadows
// the agent principal out of the context, and a wrapper that re-attached one
// INSIDE it would undo the shadow silently, with every positive test still
// green. Nothing may be inserted between h and the gate.
//
// The certificate VALIDITY WINDOW is checked INSIDE RequirePeerPrincipal rather
// than by a wrapper here, deliberately: a second wrapper could be forgotten by a
// future mount, and it would produce a second refusal path that could drift from
// the first. See checkClientCertValidity below.
func (s *Server) mountPeerRoute(mux *http.ServeMux, pattern string, h http.Handler) {
	// BELT AND BRACES against the one edit that would turn this mount into the
	// ungated federation path. It cannot fire today — the three constants are
	// compile-time strings and unauthenticatedRoutes is a compile-time map, and
	// a test asserts they are disjoint — so it is here to fail loudly if a
	// future edit makes them overlap, rather than to handle a live case.
	if IsUnauthenticatedRoute(pattern) {
		s.log.Error("REFUSING to mount a peer route: it is on the unauthenticated allow-list, which would serve federation ingress to an anonymous caller",
			"pattern", pattern,
			"remedy", "remove it from unauthenticatedRoutes in internal/httpapi/authmw.go; a peer route is authenticated by the TLS client certificate, never by nothing",
		)
		return
	}
	if h == nil {
		// Unreachable: missingParts already refused a nil handler. Checked
		// because a mount helper with a path that registers something other
		// than the gated handler is the failure this function exists to make
		// impossible.
		s.log.Error("REFUSING to mount a peer route with no handler", "pattern", pattern)
		return
	}

	gated := s.RequirePeerPrincipal(h)

	if s.peerRoutes == nil {
		s.peerRoutes = make(map[string]struct{}, 3)
	}
	s.peerRoutes[pattern] = struct{}{}
	s.route(mux, pattern, gated.ServeHTTP)
}

// ErrClientCertNotYetValidOrExpired reports a presented client certificate that
// is outside its stated validity window. It is a sentinel so the log line and a
// test can name the condition; the CLIENT is told nothing but the one fixed
// refusal (peerPrincipalRefusal).
var ErrClientCertNotYetValidOrExpired = errors.New("httpapi: the presented client certificate is outside its validity window")

// checkClientCertValidity asks crypto/x509 whether leaf is usable at `at`, and
// is called by RequirePeerPrincipal before the durable binding is consulted.
//
// It is a free function rather than a method because it touches no server
// state, which is what lets a test drive the boundary cases directly.
//
// # WHY THIS EXISTS AT ALL, AND WHY IT LANDS WITH THE MOUNT
//
// The listener uses tls.RequestClientCert: it REQUESTS a certificate and never
// requires one, and crypto/tls does NO chain verification in that mode. So
// nothing anywhere on this side of the connection has ever looked at NotBefore
// or NotAfter — an expired client certificate completes the handshake and
// arrives in r.TLS.PeerCertificates exactly like a fresh one.
//
// DECISIONS.md recorded that as harmless "only while nothing authorises on a
// client certificate". THIS MOUNT IS THE FIRST THING THAT DOES. Without this
// check, an operator who bound a peer's certificate in the trust table would
// find that the binding outlives the certificate: a peer bus whose key material
// has aged out of its own stated lifetime keeps authenticating indefinitely, and
// the certificate's expiry — the only leak-containment bound the design has for
// a credential that is never revoked — would mean nothing on this surface. The
// binding is revocable (RemoveTrust fsyncs a withdrawal floor), but revocation
// is an operator ACTION; expiry is supposed to be automatic.
//
// # IT AUTHENTICATES NOTHING, and must never be called as if it did
//
// It judges dates and only dates. An attacker-minted, in-date, self-signed
// certificate passes it cleanly. The identity comes entirely from
// RequirePeerPrincipal's fingerprint lookup, which runs immediately after it and
// is unconditional. This is the same division client/pin.go draws between
// checkBusCertificateValidity and the pin, for the same reason: a caller that
// invoked THIS without THAT would have checked that a stranger's certificate is
// in date and nothing else.
//
// The refusal it produces is the one fixed peerPrincipalRefusal string, like
// every other case in that gate. An anonymous caller must not be able to tell
// "expired" from "unknown" from "withdrawn" — that difference says whether this
// bus ever bound the certificate, which is the peer-enumeration oracle the fixed
// string exists to withhold.
//
// # crypto/x509 DECIDES; THIS FUNCTION ONLY ASKS (invariant 9)
//
// The verdict comes from x509.Certificate.Verify, never from a local
// `at.Before(leaf.NotBefore) || at.After(leaf.NotAfter)`. That comparison does
// not FEEL like writing crypto, which is exactly why invariant 9 enumerates
// "inventing a bespoke construction out of otherwise-good primitives": the
// half-open intervals, the zero time and a NotAfter before NotBefore are
// certificate-handling details a library exists to get right, and a second
// answer to a question that must have one is how the two answers disagree.
// client/pin.go made the same call and its reasoning is the authority.
//
// The leaf goes in a FRESH x509.NewCertPool as its own root: agent-bus
// certificates are self-signed with no CA anywhere (invariant 11), so an
// ordinary Verify fails with UnknownAuthorityError before it ever considers the
// dates. Putting the leaf in the pool is the stdlib's supported way to say
// "trust is already established, apply the remaining checks". A fresh pool is
// neither nil nor the system pool, so the platform verifier is never reached
// and this path is identical on every operating system. KeyUsages is
// ExtKeyUsageAny because EKU policy is not this check's question — and because
// the default (ServerAuth) would reject a peer bus's certificate, which carries
// both ServerAuth and ClientAuth, reporting it as a validity problem it is not.
//
// # THERE IS NO CLOCK-SKEW ALLOWANCE HERE, ON PURPOSE
//
// internal/buscert already backdates NotBefore by five minutes when it MINTS a
// certificate. That is the right place for an allowance: applied once, by the
// party that knows the certificate is fresh, and visible in the certificate
// itself. A second, invisible allowance here would extend every peer
// certificate's usable life beyond the NotAfter it states, silently weakening
// the bound this check exists to enforce.
//
// # THE ERROR IS FOR THE LOG, and it names the whole window
//
// The message carries NotBefore, NotAfter and the instant judged, because
// "expired" alone leaves an operator unable to separate the two causes, which
// have nothing in common: a peer genuinely serving a stale certificate, or THIS
// MACHINE'S CLOCK being wrong. A NotAfter years away printed beside an "it is
// now 1970" needs no further diagnosis. None of it reaches the client.
func checkClientCertValidity(leaf *x509.Certificate, at time.Time) error {
	if leaf == nil {
		// Unreachable from a handshake. Refused rather than passed, because a
		// validity check with a path that returns nil without having judged
		// anything is how a silent accept gets in.
		return fmt.Errorf("%w: there is no certificate to judge", ErrClientCertNotYetValidOrExpired)
	}
	if at.IsZero() {
		// REFUSED rather than repaired. x509 substitutes time.Now() for a zero
		// CurrentTime, so the verdict would be right by accident while the log
		// line beside it named the wrong instant — and a caller with no clock
		// has not judged anything. client/pin.go refuses the same input for the
		// same reason.
		return fmt.Errorf("%w: it was checked against the zero time, which is not a clock", ErrClientCertNotYetValidOrExpired)
	}

	selfSigned := x509.NewCertPool()
	selfSigned.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       selfSigned,
		CurrentTime: at,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		// EVERY non-nil verdict refuses, not only x509.Expired. On this build
		// the only other reachable one is an unhandled critical extension; a
		// default arm that returned nil for a verdict it had not thought of
		// would accept everything it did not enumerate.
		return fmt.Errorf("%w: %s (valid %s to %s, checked at %s)", ErrClientCertNotYetValidOrExpired, err,
			leaf.NotBefore.UTC().Format(time.RFC3339),
			leaf.NotAfter.UTC().Format(time.RFC3339),
			at.UTC().Format(time.RFC3339))
	}
	return nil
}
