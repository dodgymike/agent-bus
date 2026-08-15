package httpapi

import (
	"net/http"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
)

// discovery.go serves GET /v1/discovery: a bounded, STATIC, UNAUTHENTICATED
// document that tells a caller holding nothing but a bus URL how to enrol.
//
// It is a SEPARATE endpoint from /v1/info on purpose. /v1/info is a
// liveness/version probe whose exact three-field set is pinned by a security
// test precisely because that endpoint's growth was already judged a risk.
// Folding a protocol guide into it would force that pin either to cover a
// large nested structure or to go slack, and every future wording change to
// the protocol description would then be an edit to a security-sensitive test.
// Two endpoints, two jobs, two independent pins. /v1/info gains exactly one
// new field -- a constant pointer to this path -- so a caller that only knows
// /v1/info can still find this document.
//
// THE SECURITY RULES FOR THIS FILE, which are the whole point of it:
//
//   - It describes the PROTOCOL, never the ROSTER. No agent list, no agent
//     count, no data dir or any on-disk path, no listen address, no peer list,
//     no key material, no uptime, no config values. The only bus-specific
//     value in the entire document is the bus id, which /v1/info already
//     serves to the same anonymous caller.
//
//   - The endpoint list is a STATIC COMPILE-TIME CONSTANT and is NOT derived
//     from s.routes. This is load-bearing. authMiddleware deliberately answers
//     401 rather than 404 on an unknown path so that an anonymous caller
//     cannot enumerate WHICH ROUTES THIS BUILD SERVES; the messaging and
//     credential routes are registered only when Options.Hub and Options.Auth
//     are non-nil (see New). A list built from s.routes would therefore hand
//     out exactly what the 401-not-404 choice exists to withhold -- this
//     server's configuration. A static list describes the protocol the
//     software speaks and reveals nothing about how this instance is wired.
//
//   - The response MUST NOT grow with bus state. It is built ONCE in New --
//     it depends only on Identity.BusID(), which is stable for the process
//     lifetime, and on ONE CONSTRUCTION-TIME BOOLEAN (see below) -- and stored
//     on the Server; the handler writes the stored value and computes nothing.
//     "Cannot grow with state" is thereby structural rather than a promise a
//     later edit can quietly break.
//
//   - enrolment.invite_accepted IS THE SECOND INPUT, AND IT IS NOT A BREACH OF
//     THE RULE ABOVE (INVITE-GATE). It is derived ONCE IN New from
//     Options.Invites != nil, which is fixed for the process lifetime exactly as
//     the bus id is, so the document is still built once and the handler still
//     computes nothing. Nor is it the thing the static-endpoint-list rule
//     protects: that rule exists so an anonymous caller cannot enumerate WHICH
//     ROUTES this build serves, and this bit names no route -- /v1/enroll is
//     served either way. A client learns it anyway the moment it presents an
//     invite and gets 201 rather than 501, so withholding it here would buy
//     nothing and would leave a client guessing whether to spend a single-use
//     credential.
//
//   - The domain-separation prefix a session token is signed under is
//     DELIBERATELY NOT SERVED. auth.SessionSigningContext is documented as a
//     value the client must PIN: a client that learned the prefix from the
//     server would sign whatever a man in the middle chose to put in front of
//     the token. The session object carries a field that SAYS it is not served
//     and why, and points at the compiled client, which pins it.
//
//   - Values that belong to another package are DERIVED FROM THAT PACKAGE, not
//     copied as literals. The session lifetime and refresh advice below come
//     straight from internal/auth, which this package already imports and which
//     is the code that actually enforces them. A hand-copied 3600/2700 would be
//     a second source of truth that goes stale silently the moment auth changes
//     -- and a discovery document whose whole job is to be accurate is the worst
//     possible place for a value that can drift.

// RouteDiscovery is the unauthenticated protocol-discovery document.
const RouteDiscovery = "/v1/discovery"

// DiscoveryResponse is the body of GET /v1/discovery. Every field is static
// except BusID; see the file comment above for the rules this shape enforces.
type DiscoveryResponse struct {
	Service            string              `json:"service"`
	Description        string              `json:"description"`
	BusID              string              `json:"bus_id"`
	PathsAreRelativeTo string              `json:"paths_are_relative_to"`
	Steps              []string            `json:"steps"`
	Endpoints          []DiscoveryEndpoint `json:"endpoints"`
	Enrolment          DiscoveryEnrolment  `json:"enrolment"`
	Session            DiscoverySession    `json:"session"`
	Client             DiscoveryClient     `json:"client"`
	Limitations        []string            `json:"limitations"`
}

// DiscoveryEndpoint is one entry of the static endpoint list. Auth is "none"
// or "bearer".
type DiscoveryEndpoint struct {
	Name    string `json:"name"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Auth    string `json:"auth"`
	Purpose string `json:"purpose"`
}

// DiscoveryEnrolment describes what enrolment costs and yields.
//
// InviteRequired and InviteAccepted are two DIFFERENT questions and must not be
// collapsed: this build ACCEPTS and redeems an invite while still accepting an
// enrolment without one, so the honest answer is accepted=true, required=false.
type DiscoveryEnrolment struct {
	// InviteRequired reports whether an enrolment MUST carry an invite. It is
	// false in this build, and that is the truth, not a placeholder.
	InviteRequired bool `json:"invite_required"`

	// InviteAccepted reports whether an invite PRESENTED to /v1/enroll is
	// redeemed. Derived once in New from whether an invite store was supplied;
	// when it is false, presenting one is answered 501 rather than ignored.
	InviteAccepted bool `json:"invite_accepted"`

	InviteNote string `json:"invite_note"`
	YouSupply  string `json:"you_supply"`
	YouReceive string `json:"you_receive"`
}

// DiscoverySession describes the session handshake. It never carries the
// signing prefix; see SigningContext.
type DiscoverySession struct {
	Model               string `json:"model"`
	LifetimeSeconds     int    `json:"lifetime_seconds"`
	RefreshAfterSeconds int    `json:"refresh_after_seconds"`
	AuthorizationHeader string `json:"authorization_header"`
	SigningContext      string `json:"signing_context"`
}

// DiscoveryClient points at the client an agent is expected to use, because
// agents never hand-write HTTP against this bus (invariant 7).
type DiscoveryClient struct {
	Binary    string `json:"binary"`
	Build     string `json:"build"`
	GoPackage string `json:"go_package"`
	Note      string `json:"note"`
}

// discoverySessionLifetimeSeconds and discoverySessionRefreshAfterSeconds are
// DERIVED from internal/auth rather than copied, so the document cannot drift
// from the rule the server actually enforces. auth.SessionLifetime is the
// ceiling Authenticate checks against; auth.RefreshAfter() is 0.75 of it.
//
// Whole seconds, truncated: both values are round minutes today, and a client
// reading this document wants an integer schedule, not a float. Truncation is
// the safe direction for the refresh advice -- it can only tell a client to
// refresh EARLIER than strictly required, never later than expiry.
var (
	discoverySessionLifetimeSeconds     = int(auth.SessionLifetime / time.Second)
	discoverySessionRefreshAfterSeconds = int(auth.RefreshAfter() / time.Second)
)

// discoveryEndpoints is the STATIC endpoint list. It is a package-level var of
// a fixed length, NOT a projection of s.routes -- see the file comment for why
// that distinction is a security property and not a style preference. The path
// values come from the Route* constants so a rename cannot silently desync the
// document from the mux; /healthz and /v1/info are string literals only
// because neither has a constant today.
//
// /v1/broadcast is deliberately absent: it answers 501, and advertising a
// route that refuses everything is worse than not advertising it. It is
// covered honestly in discoveryLimitations instead.
var discoveryEndpoints = []DiscoveryEndpoint{
	{
		Name:    "discovery",
		Method:  http.MethodGet,
		Path:    RouteDiscovery,
		Auth:    "none",
		Purpose: "This document: how to enrol and what this bus can and cannot do.",
	},
	{
		Name:    "info",
		Method:  http.MethodGet,
		Path:    "/v1/info",
		Auth:    "none",
		Purpose: "Bus id, build version and uptime. A liveness and version probe, nothing more.",
	},
	{
		Name:    "healthz",
		Method:  http.MethodGet,
		Path:    "/healthz",
		Auth:    "none",
		Purpose: "Liveness only, for probes and orchestrators. Returns no state.",
	},
	{
		Name:    "enroll",
		Method:  http.MethodPost,
		Path:    RouteEnroll,
		Auth:    "none",
		Purpose: "Create an identity on this bus from a public key you generated. Returns the server-minted agent id.",
	},
	{
		Name:    "session-begin",
		Method:  http.MethodPost,
		Path:    RouteSessionBegin,
		Auth:    "none",
		Purpose: "Ask for an opaque session token to sign. There is no credential yet at this point.",
	},
	{
		Name:    "session-complete",
		Method:  http.MethodPost,
		Path:    RouteSessionComplete,
		Auth:    "none",
		Purpose: "Prove the token is yours with an Ed25519 signature; activates the token as your bearer credential.",
	},
	{
		Name:    "agents",
		Method:  http.MethodGet,
		Path:    RouteAgents,
		Auth:    "bearer",
		Purpose: "List the agents enrolled on this bus, by fully-qualified id.",
	},
	{
		Name:    "mint",
		Method:  http.MethodPost,
		Path:    RouteMint,
		Auth:    "bearer",
		Purpose: "Reserve the server-minted message id and sequence for one send, so you can sign them.",
	},
	{
		Name:    "send",
		Method:  http.MethodPost,
		Path:    RouteSend,
		Auth:    "bearer",
		Purpose: "Deliver a direct message to one recipient, using the reservation from /v1/mint and its signature.",
	},
	{
		Name:    "messages",
		Method:  http.MethodGet,
		Path:    RouteMessages,
		Auth:    "bearer",
		Purpose: "Read your messages from a cursor, without blocking.",
	},
	{
		Name:    "wait",
		Method:  http.MethodGet,
		Path:    RouteWait,
		Auth:    "bearer",
		Purpose: "Long-poll for messages: blocks until one arrives or the poll ceiling is reached.",
	},
}

// discoverySteps is the ordered enrolment recipe, each step naming the exact
// method and path a caller has to use.
var discoverySteps = []string{
	"1. GET " + RouteDiscovery + " -- this document -- to learn the protocol. No credential needed.",
	"2. GET /v1/info for the bus id, build version and uptime. No credential needed.",
	"3. Generate an Ed25519 AUTH keypair and keep the private half. Never send the private half anywhere. This is the key that identifies you to the bus, and it is the ONLY key the bus ever learns.",
	"4. POST " + RouteEnroll + " with {name, public_key, idempotency_key}. The response carries the SERVER-MINTED agent_id: save it, it is your identity on this bus, and you can neither choose nor change it.",
	"5. POST " + RouteSessionBegin + " with {agent_id} to receive an opaque token to sign.",
	"6. Sign that token with your enrolled private key under the pinned domain-separation prefix (see session.signing_context) and POST " + RouteSessionComplete + " with {token, signature}.",
	"7. Send the same token as `Authorization: Bearer <token>` on every other route. It is valid for session.lifetime_seconds, and you should begin a new session after session.refresh_after_seconds.",
	"8. GET " + RouteWait + " to long-poll for messages. To send one, POST " + RouteMint + " to reserve the message_id and seq, sign them, then POST " + RouteSend + ". NOTE that a message signature is made with a SECOND, SEPARATE Ed25519 MESSAGING key -- not the auth key from step 3, which only authenticates you TO THE BUS. The bus never learns your messaging public key, and there is no endpoint that would let a recipient fetch it, so today nobody can actually verify a message signature. See limitation 2.",
}

// discoveryLimitations is blunt on purpose. Every entry is verified true of
// this build; a reader making a trust decision needs the gaps stated, not
// implied by their absence.
var discoveryLimitations = []string{
	"1. TLS IS ON, MUTUAL TLS IS NOT, AND TLS ALONE PROTECTS YOU FROM A PASSIVE OBSERVER ONLY. This bus serves https and ONLY https; there is no plaintext listener, and a plaintext request never reaches a route. But the certificate is SELF-SIGNED, there is NO certificate authority, and there is deliberately NO trust-on-first-use, so YOU MUST PIN THE BUS'S CERTIFICATE FINGERPRINT, out of band, from your invite (the bus also logs it at startup as bus_cert_fingerprint). UNTIL YOU DO, an ACTIVE on-path attacker can terminate TLS with a certificate of its own and read your session token in full -- including on THIS DOCUMENT, which is unauthenticated, and which such an attacker would be the one serving you. This document cannot bootstrap trust in itself; only the fingerprint you already hold can. A client that accepts whatever certificate turns up has verified nothing. SECOND, and separately, on CLIENT certificates -- read the limit of this carefully, because it protects some callers and not others. The bus DOES request a client certificate, and does NOT require or verify one at the handshake, so a connection presenting none is still accepted and completes normally. What presenting one buys you is a BINDING: a client certificate presented at enrolment is recorded against your agent id, and a session token for a BOUND agent is refused (403) on the agent routes unless it arrives over that same certificate -- so for a bound agent the token alone is no longer enough to impersonate it. IF YOU HAVE NO BINDING YOU HAVE NONE OF THAT: an agent that enrolled without a client certificate holds a pure bearer token, and anyone who obtains it can use it from a connection presenting no certificate at all. Enrol over a client certificate if you want your token tied to a key you hold.",
	"2. MESSAGES ARE SIGNED BUT THE BUS DOES NOT VERIFY THEM. A send must carry an Ed25519 signature and the bus enforces its SHAPE (present, strict base64, exactly 64 bytes), but it never checks it against the sender's key. That is BY DESIGN, not a gap waiting on a release: THE BUS ENFORCES SHAPE, THE RECIPIENT ENFORCES AUTHENTICITY -- a bus that verified would quietly move the trust boundary onto itself. Verification is therefore YOUR job as a recipient, and today you cannot do it either, because no endpoint distributes messaging public keys. Until one exists, treat every message as UNAUTHENTICATED and do not infer authenticity from the fact that signatures are mandatory.",
	"3. NO END-TO-END ENCRYPTION. Message bodies are held, PERSISTED TO DISK UNENCRYPTED, and served in the clear. Anyone who can read this bus's data directory can read every message body it has stored. Only the append-only audit log omits bodies; the message store does not.",
	"4. BROADCAST IS REFUSED. POST " + RouteBroadcast + " answers 501: the canonical signing format has no defined audience for a broadcast, and the bus refuses it rather than accept unsigned traffic.",
	"5. SINGLE BUS ONLY. Cross-bus relay is not served yet; a recipient on another bus is a 404.",
}

// newDiscoveryResponse builds the whole document once, at construction. Its two
// inputs are the bus id and whether this build redeems a presented invite --
// both fixed for the process lifetime, which is what lets this be computed once
// and served forever. See the file comment for why the second one does not
// breach the "must not grow with bus state" rule.
func newDiscoveryResponse(busID string, inviteAccepted, inviteRequired bool) DiscoveryResponse {
	// Three sentences, and all are checked against the code every time this
	// changes: what happens to an invite you present, what happens if you present
	// none, and -- when the gate is on -- what a refused agent must actually DO
	// about it. This document is the first thing an agent reads, so a note that
	// merely refuses without naming the remedy costs a support round trip every
	// time.
	inviteNote := "Enrolment on this bus is currently OPEN: any caller that can reach it may enrol, with or without an invite. " +
		"This build moreover DOES NOT REDEEM INVITES -- it was built without an invite store, so POST " + RouteEnroll +
		" answers 501 if you present `invite_id` and `invite_secret`, rather than ignoring them and leaving you believing a single-use invite was spent. " +
		"Invite-only enrolment is being built and will become mandatory. That is imminent, not live."
	if inviteAccepted {
		inviteNote = "Enrolment on this bus is currently OPEN: any caller that can reach it may enrol, and an enrolment presenting NO invite is accepted -- `invite_required` above is false and describes what is true now. " +
			"What HAS changed: an invite presented to POST " + RouteEnroll + " as `invite_id` and `invite_secret` is genuinely REDEEMED -- single use, spent ATOMICALLY with your enrolment in one durable transaction, so it can never be spent twice nor spent on an enrolment that did not happen. " +
			"Invite-only enrolment is being built and will become mandatory. That is imminent, not live."
	}
	if inviteRequired {
		// THE GATE IS ON. Nothing in this string may soften that into "preferred"
		// or "recommended": an agent that reads it as advice will retry an
		// enrolment that can never succeed.
		inviteNote = "Enrolment on this bus is INVITE-ONLY, and `invite_required` above is true (invariant 3). " +
			"POST " + RouteEnroll + " WITHOUT `invite_id` and `invite_secret` is refused 403, always, however well-formed the rest of the request is -- there is no anonymous way onto this bus and retrying unchanged will not help. " +
			"An invite is single-use, expiring and revocable, and is spent ATOMICALLY with the enrolment it authorises in one durable transaction, so it can never be spent twice nor spent on an enrolment that did not happen. " +
			"TO GET ON: ask the bus operator for an invite file, then run `agent-busctl enrol --invite-file <path>`. " +
			"Note for the operator reading this on someone's behalf: minting an invite requires the bus to be STOPPED, because `agent-bus invite mint` takes the data directory's exclusive lock that a running bus holds. Stop the bus, mint, restart. Agents already enrolled are unaffected and keep working across that restart."
	}
	return DiscoveryResponse{
		Service:     "agent-bus",
		Description: "A durable inter-agent message bus. Agents enrol, long-poll for messages, and send direct messages to each other by fully-qualified id. Nothing is acknowledged before it is durable.",
		BusID:       busID,
		PathsAreRelativeTo: "Every `path` below is relative to the base URL you fetched this document from. " +
			"This bus deliberately does NOT echo a self-URL: the Host header is client-supplied, so a reflected URL " +
			"could be used to point a reader at an attacker's bus. Resolve paths against the URL you already trust.",
		Steps:     discoverySteps,
		Endpoints: discoveryEndpoints,
		Enrolment: DiscoveryEnrolment{
			// OBSERVED FROM THE ENFORCING LAYER, never a constant: it is
			// auth.Service.InviteRequired(), the same bit Enrol's gate reads, so
			// this field cannot drift from the behaviour it describes. It was
			// hard-coded false while the gate did not exist; hard-coding it either
			// way is what let advertising and enforcement disagree in the first
			// place.
			//
			// It is a SEPARATE bit from InviteAccepted below, which says only that
			// a presented invite is redeemed. The two are independent: a bus can
			// redeem an invite without requiring one (the build before this gate),
			// and requiring one without being able to redeem one would be an
			// unenrollable bus -- which is why cmd/agent-bus wires the invite store
			// unconditionally and httpapi.New warns loudly if it ever sees that
			// combination.
			InviteRequired: inviteRequired,
			InviteAccepted: inviteAccepted,
			InviteNote:     inviteNote,
			YouSupply:      "A base64 (standard encoding, padded) Ed25519 public key you generated yourself. The bus stores only the public half, so it can verify your calls and can never forge them.",
			YouReceive:     "A server-minted, fully-qualified agent id of the form `<bus-id>.<agent-id>`. You do not choose it: the `name` you send is a hint, and the server is authoritative on every id.",
		},
		Session: DiscoverySession{
			Model:               "You sign a SERVER-PROVIDED opaque token with the same Ed25519 key you enrolled. " + RouteSessionBegin + " issues the token, " + RouteSessionComplete + " verifies the signature and activates it, and that same token is then your bearer credential.",
			LifetimeSeconds:     discoverySessionLifetimeSeconds,
			RefreshAfterSeconds: discoverySessionRefreshAfterSeconds,
			AuthorizationHeader: "Authorization: Bearer <token>",
			SigningContext: "NOT SERVED HERE, deliberately. The domain-separation prefix the token is signed under is a value the client PINS: " +
				"a client that learned the prefix from the server would sign whatever a man in the middle chose to put in front of the token. " +
				"Take it from the compiled client below, which pins it.",
		},
		Client: DiscoveryClient{
			Binary:    "agent-busctl",
			Build:     "go build -o agent-busctl ./cmd/agent-busctl",
			GoPackage: "github.com/dodgymike/agent-bus/client",
			Note:      "Agents never hand-write HTTP against this bus. The compiled CLI is the client, and it is a thin shell over the importable Go package above -- which is deliberately not under internal/ so an agent can embed it.",
		},
		Limitations: discoveryLimitations,
	}
}

// handleDiscovery serves GET /v1/discovery. It writes the document built in
// New and computes NOTHING: no route enumeration, no state read, no clock. See
// the file comment -- that is what keeps the response static and bounded.
func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if !s.requireGET(w, r) {
		return
	}
	if s.discoveryJSON == nil {
		// Only reachable if marshalling failed in New, which the shape makes
		// impossible; see there. Correct, just slower.
		s.writeJSON(w, r, http.StatusOK, s.discovery)
		return
	}
	s.writePreformattedJSON(w, r, http.StatusOK, s.discoveryJSON)
}
