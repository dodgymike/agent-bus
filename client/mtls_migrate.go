package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientCertificateBootstrapResult is the result of migrating a pre-TLS
// identity onto mTLS without changing its server-minted agent id.
type ClientCertificateBootstrapResult struct {
	Identity
	ClientCertFingerprint string `json:"client_cert_fingerprint"`
	BoundAt               string `json:"bound_at"`
	AlreadyBound          bool   `json:"already_bound"`
	IdempotencyKey        string `json:"idempotency_key"`
}

const clientCertificateBootstrapSigningContext = "agent-bus:client-cert-bootstrap:v1:"

type clientCertBootstrapRequestBody struct {
	IdempotencyKey string `json:"idempotency_key"`
	Signature      string `json:"signature"`
}

type clientCertBootstrapResponseBody struct {
	AgentID               string `json:"agent_id"`
	ClientCertFingerprint string `json:"client_cert_fingerprint"`
	BoundAt               string `json:"bound_at"`
	AlreadyBound          bool   `json:"already_bound"`
}

// BootstrapClientCertificate asks the bus to bind this agent's local client
// certificate to the existing server-minted agent id, then records the TLS URL
// and first bus pin locally. The store is mutated only after the server accepts
// the binding, so a failed attempt leaves the legacy HTTP identity retryable.
func (c *Client) BootstrapClientCertificate(ctx context.Context, fingerprint string) (ClientCertificateBootstrapResult, error) {
	pin, err := ParseBusFingerprint(strings.TrimSpace(fingerprint))
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	if c.cfg.BusURL == "" {
		return ClientCertificateBootstrapResult{}, usagef("pin",
			"pass --bus https://<host:port> for the TLS listener you are migrating this identity to",
			"client certificate bootstrap needs the https bus URL explicitly")
	}
	u, err := parseBusURL(c.cfg.BusURL)
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	if u.Scheme != "https" {
		return ClientCertificateBootstrapResult{}, newError(KindUsage, "pin",
			"client certificate bootstrap requires an https bus URL, got "+u.String(),
			"pass --bus https://<host:port> and the fingerprint the bus logs as bus_cert_fingerprint=...")
	}
	cred, err := c.credential()
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	priv, err := cred.PrivateKey()
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	cc, err := c.clientCertificate()
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	clientFP, err := ParseBusFingerprint(cc.Fingerprint())
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}

	idemKey, err := newIdempotencyKey()
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	pins := NewBusPinSet(pin)
	var body clientCertBootstrapResponseBody
	var lastAuthErr error
	for attempt := 0; attempt < 2; attempt++ {
		s, err := c.bootstrapSession(ctx, cred, u, pins, attempt > 0)
		if err != nil {
			if lastAuthErr != nil {
				return ClientCertificateBootstrapResult{}, lastAuthErr
			}
			return ClientCertificateBootstrapResult{}, err
		}
		sig := ed25519.Sign(priv, clientCertificateBootstrapSigningBytes(s.token, idemKey, clientFP))
		_, err = c.do(ctx, request{
			method:       http.MethodPost,
			path:         routeClientCertBootstrap,
			body:         clientCertBootstrapRequestBody{IdempotencyKey: idemKey, Signature: base64.StdEncoding.EncodeToString(sig)},
			out:          &body,
			op:           "client certificate bootstrap",
			retryable:    true,
			bearer:       s.token,
			busOverride:  u,
			pinsOverride: pins,
		})
		if err == nil {
			lastAuthErr = nil
			break
		}
		if KindOf(err) != KindAuth || attempt == 1 {
			return ClientCertificateBootstrapResult{}, writeFailed("client certificate bootstrap", idemKey, err)
		}
		lastAuthErr = err
		c.mu.Lock()
		c.session = nil
		c.mu.Unlock()
	}
	if lastAuthErr != nil {
		return ClientCertificateBootstrapResult{}, writeFailed("client certificate bootstrap", idemKey, lastAuthErr)
	}
	if body.AgentID != cred.AgentID {
		return ClientCertificateBootstrapResult{}, newError(KindServer, "client certificate bootstrap",
			fmt.Sprintf("the bus reported agent_id %q but this identity is %q", body.AgentID, cred.AgentID),
			"do not use this credential store until the bus operator investigates the mismatch")
	}
	if body.ClientCertFingerprint != cc.Fingerprint() {
		return ClientCertificateBootstrapResult{}, newError(KindServer, "client certificate bootstrap",
			fmt.Sprintf("the bus bound client certificate %q but this process presented %q", body.ClientCertFingerprint, cc.Fingerprint()),
			"do not use this credential store until the bus operator investigates the mismatch")
	}
	if err := validateServerTimestamp("client certificate bootstrap", "bound_at", body.BoundAt); err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	identity, err := c.store.BootstrapTLSPin(c.cfg.AgentID, u.String(), pin)
	if err != nil {
		return ClientCertificateBootstrapResult{}, err
	}
	c.forgetIdentity()
	return ClientCertificateBootstrapResult{
		Identity:              identity,
		ClientCertFingerprint: body.ClientCertFingerprint,
		BoundAt:               body.BoundAt,
		AlreadyBound:          body.AlreadyBound,
		IdempotencyKey:        idemKey,
	}, nil
}

func clientCertificateBootstrapSigningBytes(sessionToken, idempotencyKey string, fp BusFingerprint) []byte {
	return []byte(clientCertificateBootstrapSigningContext + sessionToken + ":" + idempotencyKey + ":" + fp.String())
}

func (c *Client) bootstrapSession(ctx context.Context, cred Credential, u *url.URL, pins BusPinSet, force bool) (*session, error) {
	if !force {
		if s, ok := c.cachedSession(); ok {
			return s, nil
		}
	}
	c.handshakeMu.Lock()
	defer c.handshakeMu.Unlock()
	if !force {
		if s, ok := c.cachedSession(); ok {
			return s, nil
		}
	}
	return c.establishSessionOn(ctx, cred, u, pins)
}

func (c *Client) establishSessionOn(ctx context.Context, cred Credential, u *url.URL, pins BusPinSet) (*session, error) {
	priv, err := cred.PrivateKey()
	if err != nil {
		return nil, err
	}
	var begun sessionBeginResponse
	if _, err := c.do(ctx, request{
		method:       http.MethodPost,
		path:         routeSessionBegin,
		op:           "session begin",
		body:         sessionBeginRequest{AgentID: cred.AgentID},
		out:          &begun,
		retryable:    false,
		busOverride:  u,
		pinsOverride: pins,
	}); err != nil {
		return nil, annotateSessionError(err, cred.AgentID)
	}
	if err := validateChallengeToken(begun.Token); err != nil {
		return nil, err
	}
	if begun.AgentID != "" && begun.AgentID != cred.AgentID {
		return nil, newError(KindServer, "session begin",
			"the bus issued a challenge for a different agent than the one requested",
			"check that --bus points at the bus this identity enrolled with")
	}
	signature := ed25519.Sign(priv, []byte(SessionSigningContext+begun.Token))
	var completed sessionCompleteResponse
	if _, err := c.do(ctx, request{
		method:       http.MethodPost,
		path:         routeSessionComplete,
		op:           "session complete",
		body:         sessionCompleteRequest{Token: begun.Token, Signature: base64.StdEncoding.EncodeToString(signature)},
		out:          &completed,
		retryable:    false,
		busOverride:  u,
		pinsOverride: pins,
	}); err != nil {
		return nil, annotateSessionError(err, cred.AgentID)
	}
	if completed.AgentID != "" && completed.AgentID != cred.AgentID {
		return nil, newError(KindServer, "session complete",
			"the bus activated a session for a different agent than the one that signed the challenge",
			"check that --bus points at the bus this identity enrolled with")
	}
	now := c.now()
	s := &session{agentID: cred.AgentID, token: begun.Token, lifetime: time.Duration(completed.LifetimeSeconds) * time.Second}
	if t, err := time.Parse(time.RFC3339Nano, completed.ExpiresAt); err == nil {
		s.expiresAt = t
	} else if s.lifetime > 0 {
		s.expiresAt = now.Add(s.lifetime)
	} else {
		s.expiresAt = now.Add(DefaultTimeout)
	}
	if completed.RefreshAfterSeconds > 0 {
		s.refreshAt = now.Add(time.Duration(completed.RefreshAfterSeconds) * time.Second)
	} else if s.lifetime > 0 {
		s.refreshAt = now.Add(time.Duration(float64(s.lifetime) * refreshFractionFallback))
	} else {
		s.refreshAt = s.expiresAt
	}
	c.mu.Lock()
	c.session = s
	c.mu.Unlock()
	return s, nil
}
