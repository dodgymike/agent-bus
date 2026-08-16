package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"time"
)

// SessionSigningContext is the DOMAIN SEPARATION prefix. The exact byte string
// signed is SessionSigningContext + token.
//
// It is PINNED here as a constant and is deliberately NOT learned from the
// server's response — the bus does not send it, precisely so that a man in the
// middle cannot choose what this client puts in front of the bytes it signs.
// A client that took the prefix from the wire would be a signing oracle for
// whatever the attacker prefixed, which is the whole reason domain separation
// exists.
//
// It mirrors internal/auth.SessionSigningContext; the duplication is required
// because the client package cannot import internal/ (invariant 7). If the two
// ever diverge, every signature fails closed — the server simply does not
// verify — which is the right failure direction.
const SessionSigningContext = "agent-bus:session-token:v1:"

// refreshFractionFallback is used only when the bus does not tell us when to
// refresh. The server's refresh_after_seconds is authoritative advice; this is
// the 75%-of-lifetime rule from the 2026-08-02 auth decision, applied locally
// so an older or terser bus still gets a sane schedule.
const refreshFractionFallback = 0.75

// sessionExpiryGrace is subtracted from the server's expiry when deciding
// whether a cached session is still usable.
//
// It exists because expiry is enforced against the SERVER's clock with no skew
// grace: a token we believe has two milliseconds left is a token the bus may
// already consider dead. Treating the last second as already expired turns a
// confusing 401 into a transparent re-handshake.
const sessionExpiryGrace = time.Second

// session is a live bearer credential.
//
// It is NOT persisted BY DEFAULT, and that default is the safe one: sessions do
// not survive a bus restart, last at most an hour, and are opaque server-side
// handles, so writing one to disk adds a bearer credential at rest in exchange
// for saving one round trip.
//
// Since 2026-08-16 a caller may OPT IN with Config.PersistSession
// (`--persist-session`, env AGENT_BUS_PERSIST_SESSION). That exists because the
// no-persistence rule has a cost this comment used to ignore: under invariant 7
// an agent shells out per command, each process burns a server-side session,
// the bus holds each for an hour against a per-agent cap of 32 and evicts
// nothing — so an agent working faster than one command every two minutes locks
// ITSELF out. See client/sessionstore.go for the full trade and DECISIONS.md
// 2026-08-16.
type session struct {
	agentID   string
	token     string
	expiresAt time.Time
	refreshAt time.Time
	lifetime  time.Duration
}

// String and GoString REDACT the token, mirroring persistedSession and
// Credential. A %v on this struct in any future error path must not print a
// live bearer credential.
func (s *session) String() string {
	if s == nil {
		return "<nil session>"
	}
	return "session{agent:" + s.agentID + " token:[REDACTED] expires:" + s.expiresAt.Format(time.RFC3339) + "}"
}

func (s *session) GoString() string { return s.String() }

// SessionInfo is the PUBLIC description of a session: everything except the
// token. Its json tags are a documented contract surface (CONTRACTS-CLI.md).
//
// There is deliberately no Token field, not even one tagged `json:"-"`. A
// field that exists can be printed by a caller with a debugger, a reflection
// walk or a struct copy into their own logging type; a field that does not
// exist cannot.
type SessionInfo struct {
	AgentID   string `json:"agent_id"`
	ExpiresAt string `json:"expires_at"`
	RefreshAt string `json:"refresh_at"`

	// LifetimeSeconds is what the bus said the session lasts.
	LifetimeSeconds int `json:"lifetime_seconds"`
}

func (s *session) info() SessionInfo {
	return SessionInfo{
		AgentID:         s.agentID,
		ExpiresAt:       s.expiresAt.UTC().Format(time.RFC3339Nano),
		RefreshAt:       s.refreshAt.UTC().Format(time.RFC3339Nano),
		LifetimeSeconds: int(s.lifetime / time.Second),
	}
}

// sessionBeginRequest mirrors httpapi.SessionBeginRequestBody.
type sessionBeginRequest struct {
	AgentID string `json:"agent_id"`
}

// sessionBeginResponse mirrors httpapi.SessionBeginResponseBody.
type sessionBeginResponse struct {
	AgentID            string `json:"agent_id"`
	Token              string `json:"token"`
	ChallengeExpiresAt string `json:"challenge_expires_at"`
}

// sessionCompleteRequest mirrors httpapi.SessionCompleteRequestBody.
type sessionCompleteRequest struct {
	Token     string `json:"token"`
	Signature string `json:"signature"`
}

// sessionCompleteResponse mirrors httpapi.SessionCompleteResponseBody.
type sessionCompleteResponse struct {
	AgentID             string `json:"agent_id"`
	ExpiresAt           string `json:"expires_at"`
	LifetimeSeconds     int    `json:"lifetime_seconds"`
	RefreshAfterSeconds int    `json:"refresh_after_seconds"`
}

// EnsureSession returns a live session for the selected identity, establishing
// or refreshing one if necessary, and reports its public details.
//
// The handshake is: ask the bus for a token, SIGN THE TOKEN THE BUS CHOSE with
// the enrolment private key, and present it back. The client never chooses the
// bytes it signs — a client-chosen challenge would permit pre-computation and
// prove far less (invariant 3).
func (c *Client) EnsureSession(ctx context.Context) (SessionInfo, error) {
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	s, err := c.ensureSession(ctx, false)
	if err != nil {
		return SessionInfo{}, err
	}
	return s.info(), nil
}

// VerifySession establishes a session AGAINST THE BUS, unconditionally, and
// reports its details. It never returns a cached answer.
//
// # Why this exists separately from EnsureSession
//
// `whoami --verify` promises: "actually authenticate ... this is the ONLY way
// to tell a stored credential the bus still honours from one it has forgotten
// — sessions do not survive a bus restart". EnsureSession is a CACHE lookup by
// design, and once --persist-session made that cache outlive the process,
// --verify started answering from disk and exit 0 against a bus that was DOWN
// — silently failing at the one job it has, in exactly the restart case its
// help text names. Caught by the review gate 2026-08-16.
//
// It therefore costs a handshake every time, deliberately. A caller who wants
// the cheap answer wants EnsureSession.
func (c *Client) VerifySession(ctx context.Context) (SessionInfo, error) {
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	// Drop the in-memory copy first so the re-check under handshakeMu cannot
	// hand back what we are trying to bypass, then force past the disk read.
	c.mu.Lock()
	c.session = nil
	c.mu.Unlock()

	s, err := c.ensureSession(ctx, true)
	if err != nil {
		return SessionInfo{}, err
	}
	return s.info(), nil
}

// ensureSession returns a usable session, reusing the cached one unless it is
// past its refresh point or force is set.
//
// Handshakes are SINGLE-FLIGHTED. c.mu cannot be held across the two network
// round trips (that would serialise every unrelated call on the client), so
// without a separate gate N goroutines that find the cache cold would each run
// a full handshake — N entries burned in the bus's bounded session table, and
// N-1 of them immediately discarded. handshakeMu makes the losers wait and
// then RE-CHECK the cache, so the common case costs one handshake.
func (c *Client) ensureSession(ctx context.Context, force bool) (*session, error) {
	if !force {
		if s, ok := c.cachedSession(); ok {
			return s, nil
		}
	}

	c.handshakeMu.Lock()
	defer c.handshakeMu.Unlock()

	// Re-check under the gate: whoever held it may have established the very
	// session this call wanted.
	//
	// CAVEAT, and it is a real one for an EMBEDDING client (the one-shot CLI is
	// single-goroutine and cannot hit it). With persistence on, this re-check
	// can be satisfied by a token another goroutine LOADED FROM DISK rather
	// than one it fetched from the network. So a forced refresh after a 401 can
	// still observe the dead token here. It self-heals — the next 401 forces
	// again and establishSession overwrites the file — but it costs an extra
	// round trip. Before persistence this re-check could only ever be satisfied
	// by a token from the network, which is what the comment here used to
	// assume. Noted by the review gate 2026-08-16.
	if s, ok := c.cachedSession(); ok {
		return s, nil
	}

	cred, err := c.credential()
	if err != nil {
		return nil, err
	}

	// The DISK cache, when the caller opted in. It sits here — under
	// handshakeMu, after the memory re-check and before the network — so it
	// costs nothing on the hot path and cannot race two loads.
	//
	// `force` skips it deliberately. A forced refresh follows a 401, which
	// means the token just failed; the file holds that same dead token, so
	// reading it would hand back exactly what the bus refused. Persistence
	// must never turn one 401 into a loop.
	if c.cfg.PersistSession && !force {
		if s, ok := c.loadPersistedSession(cred); ok {
			c.mu.Lock()
			c.session = s
			c.mu.Unlock()
			return s, nil
		}
	}

	s, err := c.establishSession(ctx, cred)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
	if c.cfg.PersistSession {
		c.savePersistedSession(cred, s)
	}
	return s, nil
}

// cachedSession returns the cached session when it is still usable.
func (c *Client) cachedSession() (*session, bool) {
	c.mu.Lock()
	cached := c.session
	c.mu.Unlock()
	if cached == nil || !c.sessionUsable(cached) {
		return nil, false
	}
	return cached, true
}

// sessionUsable reports whether s can still be presented.
//
// It refreshes at the point the BUS nominated (75% of lifetime), not at the
// expiry boundary: a token renewed at the boundary is a token that expires
// mid-request for anyone with a slow link or a clock a second out.
func (c *Client) sessionUsable(s *session) bool {
	now := c.now()
	if !now.Before(s.expiresAt.Add(-sessionExpiryGrace)) {
		return false
	}
	return now.Before(s.refreshAt)
}

// establishSession runs the two-step handshake.
func (c *Client) establishSession(ctx context.Context, cred Credential) (*session, error) {
	priv, err := cred.PrivateKey()
	if err != nil {
		return nil, err
	}

	var begun sessionBeginResponse
	if _, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   routeSessionBegin,
		op:     "session begin",
		body:   sessionBeginRequest{AgentID: cred.AgentID},
		out:    &begun,
		// NOT retryable: each call mints a fresh pending challenge, so a blind
		// retry burns entries in the bus's bounded session table. The caller's
		// own retry is one handshake, not several.
		retryable: false,
	}); err != nil {
		return nil, annotateSessionError(err, cred.AgentID)
	}
	if err := validateChallengeToken(begun.Token); err != nil {
		return nil, err
	}
	// The bus must be issuing a challenge for the agent we asked about. A
	// mismatch means either a confused server or a response that belongs to
	// somebody else, and signing it would be signing a challenge on another
	// identity's behalf.
	if begun.AgentID != "" && begun.AgentID != cred.AgentID {
		return nil, newError(KindServer, "session begin",
			"the bus issued a challenge for a different agent than the one requested",
			"check that --bus points at the bus this identity enrolled with")
	}

	signature := ed25519.Sign(priv, []byte(SessionSigningContext+begun.Token))

	var completed sessionCompleteResponse
	if _, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   routeSessionComplete,
		op:     "session complete",
		body: sessionCompleteRequest{
			Token:     begun.Token,
			Signature: base64.StdEncoding.EncodeToString(signature),
		},
		out:       &completed,
		retryable: false,
	}); err != nil {
		return nil, annotateSessionError(err, cred.AgentID)
	}

	if completed.AgentID != "" && completed.AgentID != cred.AgentID {
		return nil, newError(KindServer, "session complete",
			"the bus activated a session for a different agent than the one that signed the challenge",
			"check that --bus points at the bus this identity enrolled with")
	}

	now := c.now()
	s := &session{
		agentID:  cred.AgentID,
		token:    begun.Token,
		lifetime: time.Duration(completed.LifetimeSeconds) * time.Second,
	}
	if t, err := time.Parse(time.RFC3339Nano, completed.ExpiresAt); err == nil {
		s.expiresAt = t
	} else if s.lifetime > 0 {
		s.expiresAt = now.Add(s.lifetime)
	} else {
		return nil, newError(KindServer, "session complete",
			"the bus activated the session but gave no usable expiry",
			"check that --bus points at an agent-bus server")
	}
	if s.lifetime <= 0 {
		s.lifetime = s.expiresAt.Sub(now)
	}

	switch {
	case completed.RefreshAfterSeconds > 0:
		s.refreshAt = now.Add(time.Duration(completed.RefreshAfterSeconds) * time.Second)
	default:
		s.refreshAt = now.Add(time.Duration(float64(s.lifetime) * refreshFractionFallback))
	}
	// The bus is authoritative on expiry; never schedule a refresh past it.
	if !s.refreshAt.Before(s.expiresAt) {
		s.refreshAt = s.expiresAt.Add(-sessionExpiryGrace)
	}
	return s, nil
}

// maxChallengeTokenLen mirrors httpapi.MaxBearerTokenLen: the bus refuses to
// accept a bearer longer than this, so a token longer than it could never be
// presented anyway.
const maxChallengeTokenLen = 512

// validateChallengeToken bounds and checks the token BEFORE it is signed.
//
// The signature is domain-separated, so a hostile bus cannot turn this into an
// oracle for anything but a session token — the check is not what makes the
// signing safe. What it does is refuse to sign a megabyte of attacker-chosen
// bytes, and refuse a token the bus's own bearer parser would reject, which
// turns a confusing 401 later into a clear error here. The alphabet is
// base64url because that is what internal/auth mints with RawURLEncoding.
func validateChallengeToken(token string) error {
	if token == "" {
		return newError(KindServer, "session begin",
			"the bus issued a challenge with no token",
			"check that --bus points at an agent-bus server")
	}
	if len(token) > maxChallengeTokenLen {
		return newError(KindServer, "session begin",
			"the bus issued a challenge token longer than the protocol allows",
			"check that --bus points at an agent-bus server")
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return newError(KindServer, "session begin",
				"the bus issued a challenge token containing characters a bearer token cannot contain",
				"check that --bus points at an agent-bus server")
		}
	}
	return nil
}

// annotateSessionError improves the remedy on the two failures an operator
// actually hits during a handshake.
func annotateSessionError(err error, agentID string) error {
	// errors.As, not a type assertion: a wrapped *Error would otherwise slip
	// through and lose the improved remedy below.
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Status {
	case http.StatusNotFound:
		// The bus does not know this agent. The overwhelmingly common cause is
		// a bus that was restarted or rebuilt with a fresh data directory
		// while a stale credential stayed in the local store.
		e.Kind = KindAuth
		e.Message = "the bus does not know " + agentID
		e.Remedy = "this identity was enrolled with a different bus, or the bus lost its state; enrol again with `agent-busctl enrol --bus <url> --name <name>`"
	case http.StatusUnauthorized:
		e.Kind = KindAuth
		e.Message = "the bus rejected the signature for " + agentID
		e.Remedy = "the stored private key does not match the key the bus recorded; enrol again with `agent-busctl enrol --bus <url> --name <name>`"
	}
	return e
}

// authorizedRequest performs req with a live bearer token, re-establishing the
// session once if the bus rejects it.
//
// The retry exists because SESSIONS DO NOT SURVIVE A BUS RESTART. Without it,
// the first call after a restart fails with a 401 that looks to the operator
// like a credential problem, when the correct and invisible response is to
// handshake again. It re-establishes AT MOST ONCE — a second 401 is a real
// authentication failure and looping on it would be credential guessing.
//
// Nothing calls this yet: /v1/enroll, /v1/session/begin and
// /v1/session/complete are the only routes the bus serves, and all three are
// unauthenticated by necessity. It is here so the first authenticated
// subcommand is additive.
func (c *Client) authorizedRequest(ctx context.Context, req request) (*response, error) {
	s, err := c.ensureSession(ctx, false)
	if err != nil {
		return nil, err
	}
	req.bearer = s.token
	resp, err := c.do(ctx, req)
	if err == nil {
		return resp, nil
	}
	if KindOf(err) != KindAuth {
		return nil, err
	}

	c.mu.Lock()
	c.session = nil
	c.mu.Unlock()

	s, rerr := c.ensureSession(ctx, true)
	if rerr != nil {
		return nil, rerr
	}
	req.bearer = s.token
	return c.do(ctx, req)
}
