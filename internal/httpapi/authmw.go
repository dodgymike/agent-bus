package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/dodgymike/agent-bus/internal/auth"
)

// MaxBearerTokenLen bounds an inbound bearer token before anything is done
// with it.
//
// A real token is 43 characters: base64.RawURLEncoding of the 32 random bytes
// internal/auth mints. 512 is two orders of magnitude of headroom and still
// finite, which is the whole point -- an unauthenticated caller must not be
// able to push an unbounded string into the token-hashing path on every
// request.
const MaxBearerTokenLen = 512

// unauthenticatedRoutes is the EXPLICIT allow-list of paths that may be served
// without a credential. Everything not in this map requires a bearer token:
// the middleware is DEFAULT-DENY, so a route added tomorrow is authenticated
// the moment it is registered and nobody has to remember to protect it.
//
// Five entries, and each one has to earn its place (invariant 3):
//
//   - "/healthz" -- liveness. A load balancer, an orchestrator's probe and a
//     `docker compose` healthcheck all call it before any agent exists, and it
//     returns no state whatsoever.
//   - "/v1/info" -- pre-enrolment discovery. An agent needs the bus id and
//     version to decide whether to enrol at all; the payload is deliberately
//     kept to bus id, version and uptime for exactly this reason.
//   - RouteEnroll -- this is where an identity is created. There is by
//     definition no credential yet.
//   - RouteSessionBegin -- called with NO SESSION AT ALL. It is the request
//     that asks the server for a token to sign.
//   - RouteSessionComplete -- subtler, and the one that looks skippable. The
//     caller does hold a token here, but it is a PENDING one, and
//     auth.Service.Authenticate rejects a pending session exactly like an
//     unknown one. So a bearer requirement on this route could only ever be
//     satisfied by the very credential the call exists to create: it would be
//     unsatisfiable, not strict. Authentication on this route is the Ed25519
//     signature over the server-chosen token, which handleSessionComplete
//     verifies; the token in the body is not a credential until it succeeds.
//
// Matching is EXACT string equality against r.URL.Path -- no prefix match, no
// path cleaning, no normalisation, no trailing-slash tolerance. That is
// deliberate and fail-closed: any non-canonical spelling ("//healthz",
// "/healthz/", "/v1/enroll/../x") simply is not on this list, so it requires a
// credential. The failure mode of being strict here is a 401 on a
// misspelled-but-harmless probe; the failure mode of being lenient is a
// normalisation mismatch between this check and the mux, which is how
// allow-list bypasses are built.
var unauthenticatedRoutes = map[string]struct{}{
	"/healthz":           {},
	"/v1/info":           {},
	RouteEnroll:          {},
	RouteSessionBegin:    {},
	RouteSessionComplete: {},
}

// UnauthenticatedRoutes returns the paths served without a credential, sorted.
//
// It returns a COPY: the real allow-list is the security boundary of this
// server and no caller, test or documentation generator gets a handle on it
// that could mutate it.
func UnauthenticatedRoutes() []string {
	out := make([]string, 0, len(unauthenticatedRoutes))
	for p := range unauthenticatedRoutes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// IsUnauthenticatedRoute reports whether path is served without a credential.
// The comparison is exact; see unauthenticatedRoutes.
func IsUnauthenticatedRoute(path string) bool {
	_, ok := unauthenticatedRoutes[path]
	return ok
}

const ctxKeyPrincipal ctxKey = 1

// PrincipalFromContext returns the authenticated identity attached by
// authMiddleware, and whether there was one.
//
// The principal is placed in the context ONLY by authMiddleware, and ONLY
// after auth.Service.Authenticate returned it. A downstream handler may
// therefore treat it as the authenticated subject of the request, and must
// NEVER take an identity from a header, a query parameter or a request body:
// those are client-supplied claims (invariant 1 -- the server is authoritative
// on every id).
//
// A handler on an allow-listed route gets ok == false, because no principal is
// attached there. That is not an error condition, it is the definition of an
// unauthenticated route.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	if ctx == nil {
		return auth.Principal{}, false
	}
	p, ok := ctx.Value(ctxKeyPrincipal).(auth.Principal)
	return p, ok
}

// AgentIDFromContext returns the fully-qualified `<bus-id>.<agent-id>` of the
// authenticated caller (invariant 2), or "" when the request carried no
// principal. See PrincipalFromContext for why this, and nothing else, is the
// subject a handler may act on.
func AgentIDFromContext(ctx context.Context) string {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return ""
	}
	return p.AgentID
}

// errNoAuthHeader and errMalformedAuthHeader separate the two client mistakes
// that deserve different WWW-Authenticate hints: nothing was presented at all,
// versus something was presented that is not a well-formed bearer credential.
// Neither distinguishes anything about a token's VALIDITY -- that is
// Authenticate's job and its outcome is never described to the client.
var (
	errNoAuthHeader        = errors.New("httpapi: no Authorization header")
	errMalformedAuthHeader = errors.New("httpapi: malformed Authorization header")
)

// bearerToken extracts the token from an Authorization header, strictly.
//
// Every rule here is fail-closed, and the function does no I/O and touches no
// state so it can be unit-tested on its own:
//
//   - EXACTLY ONE Authorization header. Zero is "missing"; two or more is
//     malformed and rejected outright rather than resolved by picking one. A
//     proxy in front of us could have produced the duplicate, and choosing
//     which of two credentials to honour is precisely the ambiguity an
//     attacker uses to make the front and back halves of a stack disagree.
//   - Split on the FIRST space; the scheme must be "Bearer" case-insensitively
//     (RFC 7235 makes the scheme case-insensitive), and the remainder must be
//     non-empty and contain no further space.
//   - The length is bounded BEFORE the value is used for anything.
//   - The remainder must be base64url alphabet only, matching what
//     internal/auth mints with base64.RawURLEncoding.
//
// That last check is a cheap SYNTACTIC filter that keeps obviously-forged
// input out of the hashing path. It is emphatically NOT the authentication
// decision: a string can pass every rule here and still be a token this server
// never issued. auth.Service.Authenticate makes the decision, always.
func bearerToken(r *http.Request) (string, error) {
	vals := r.Header.Values("Authorization")
	switch len(vals) {
	case 0:
		return "", errNoAuthHeader
	case 1:
	default:
		return "", errMalformedAuthHeader
	}

	scheme, rest, found := cutFirstSpace(vals[0])
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", errMalformedAuthHeader
	}
	if rest == "" || strings.IndexByte(rest, ' ') >= 0 {
		return "", errMalformedAuthHeader
	}
	if len(rest) > MaxBearerTokenLen {
		return "", errMalformedAuthHeader
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_':
		default:
			return "", errMalformedAuthHeader
		}
	}
	return rest, nil
}

// cutFirstSpace splits s at its first space. It is strings.Cut, which this
// module's Go version (1.19) has, spelled out locally only so the split point
// -- the FIRST space, never the last -- is stated where it is relied on.
func cutFirstSpace(s string) (before, after string, found bool) {
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}

// WWW-Authenticate values for the two 401 shapes. The error codes are RFC 6750
// vocabulary: invalid_request for a credential that was absent or not
// well-formed, invalid_token for one that was well-formed and did not
// authenticate. That distinction is about the SHAPE of the request, which the
// client already knows, and reveals nothing about server state.
const (
	wwwAuthenticateInvalidRequest = `Bearer realm="agent-bus", error="invalid_request"`
	wwwAuthenticateInvalidToken   = `Bearer realm="agent-bus", error="invalid_token"`
)

// authMiddleware enforces invariant 3 across the WHOLE mux: every request is
// authenticated unless its exact path is on unauthenticatedRoutes.
//
// It is a METHOD rather than a free function so a 401 from here is written by
// s.writeJSON and carries the same ErrorResponse shape, Content-Type and
// nosniff header as every other error this server emits -- a client parses one
// error format, not two -- and so refusals land in s.log.
//
// Three things it deliberately does NOT do:
//
//   - It does not parse, decode or verify the token. Tokens are opaque
//     server-side handles (DECISIONS.md, 2026-08-02), not signed claims.
//   - It does not CACHE the result. Every request resolves against live
//     session state, which is what makes revocation and expiry immediate; a
//     cache would be a window in which a revoked credential still works.
//   - It never logs, echoes or otherwise records the token -- not truncated,
//     not hashed, not in an error string. The only value that ever leaves this
//     function is the resulting agent id.
//
// The client-facing 401 body is deliberately identical for "no such session",
// "expired" and "pending". Telling those apart is an enumeration oracle: it
// would let a caller probe which handles exist. The LOG gets the wrapped error
// from internal/auth, which names the reason precisely.
//
// KNOWN COVERAGE BOUNDARY, verified rather than assumed (security audit,
// 2026-08-02): this covers the mux, and the mux is everything this package
// serves -- but ONE request never reaches it. Go's net/http answers
// `OPTIONS * HTTP/1.1` itself, in serverHandler.ServeHTTP, with
// globalOptionsHandler: a bare 200 and Content-Length: 0, before the
// application handler runs at all. It exposes no application data, no route
// list and no state, so it is a curiosity rather than a hole -- but it is NOT
// authenticated by this function and nothing here can make it so.
// http.Server.DisableGeneralOptionsHandler, which would turn it off, is
// go1.20+ and this module is pinned at go1.19 (see go.mod). Written down so
// the next person to audit the 401 surface does not spend an afternoon
// rediscovering it.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsUnauthenticatedRoute(r.URL.Path) {
			// No principal is attached here, on purpose: a handler on an
			// unauthenticated route must never see one, so it cannot come to
			// depend on an identity that is not always present.
			next.ServeHTTP(w, r)
			return
		}

		if s.auth == nil {
			// A server built without an auth service can authenticate nobody,
			// so it serves the allow-list and nothing else. Warn, not Debug:
			// this is an operator misconfiguration, and every agent-facing
			// route answering 401 is worth one line per request to explain.
			s.log.Warn("request refused: this server was built without an auth service, so only the unauthenticated routes are served",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "authentication required")
			return
		}

		token, err := bearerToken(r)
		if err != nil {
			s.log.Debug("request refused: no usable bearer credential",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"err", err,
			)
			s.writeUnauthorized(w, r, wwwAuthenticateInvalidRequest, "authentication required")
			return
		}

		principal, err := s.auth.Authenticate(token)
		if err != nil {
			// Info, not Debug: a well-formed token that does not authenticate
			// is either a stale client or someone guessing, and an operator
			// should see the second by default. The wrapped error says which
			// of unknown/pending/expired it was; the client is told none of it.
			s.log.Info("request refused: bearer token did not authenticate",
				"request_id", RequestIDFromContext(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"err", err,
			)
			s.writeUnauthorized(w, r, wwwAuthenticateInvalidToken, "invalid or expired credential")
			return
		}

		s.log.Debug("request authenticated",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"agent_id", principal.AgentID,
		)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, principal)))
	})
}

// writeUnauthorized answers 401 with a WWW-Authenticate challenge and the
// standard error body. reason is the ONLY thing the client learns, and it is
// chosen from a fixed set: it must never depend on server state.
func (s *Server) writeUnauthorized(w http.ResponseWriter, r *http.Request, challenge, reason string) {
	w.Header().Set("WWW-Authenticate", challenge)
	s.writeJSON(w, r, http.StatusUnauthorized, ErrorResponse{Error: reason})
}
