package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds a response body before it reaches json.Decode.
//
// The bus's replies on this surface are a few hundred bytes. 1 MiB is three
// orders of magnitude of headroom and still finite: a client that streams an
// attacker-chosen body into memory has handed over its process, and "the bus
// is trusted" stops being true the moment the address in --bus is wrong.
const maxResponseBytes = 1 << 20

// userAgent identifies this client to the bus. It carries no version yet;
// when the binary gains one it is appended here.
const userAgent = "agent-busctl"

// dial timings for the default transport. All finite: an agent shelling out
// must never be left hanging on a bus that accepted the TCP connection and
// then went quiet.
const (
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second

	// responseHeaderTimeout must exceed the LONGEST legitimate silence on this
	// API, and that is a long poll: GET /v1/wait parks on the bus for up to
	// MaxPollTimeout (5 minutes) without sending a single response header.
	// A 30s value here — which is what this was — did not bound a hung bus, it
	// silently broke every poll longer than half a minute, and raced the
	// 30-second DEFAULT poll for the rest.
	//
	// Nothing is weakened by the larger value: this is a per-transport bound,
	// while every ORDINARY call is bounded end to end by its context
	// (Config.Timeout, applied in contextWithTimeout), which is where a bus that
	// accepts the connection and then goes quiet is actually caught.
	responseHeaderTimeout = MaxPollTimeout + time.Minute

	idleConnTimeout = 90 * time.Second
)

// newHTTPClient builds the transport for one bus. It is THE ONLY place in this
// package where an http.Client is constructed, and pinnedTLSConfig (pin.go) is
// the only place DEFAULT VERIFICATION IS REPLACED.
//
// That second clause is deliberately narrower than "the only place a tls.Config
// is built", which an earlier draft of this comment said and which is false —
// the unpinned branch below builds one too, a few lines down. The distinction
// mattered for MTLS-CLIENTCERT, which landed 2026-08-07: the client certificate
// is offered from pinnedTLSConfig, NOT from the unpinned literal below, because
// the pinned branch REPLACES that value wholesale. A client certificate added
// to the wrong one is silently dropped on every pinned — i.e. every real —
// connection. It fails closed (the handshake is refused), so it costs an
// afternoon rather than a breach, which is exactly why it is worth a sentence
// here.
//
// The single-seam property is the point, and it is why this function exists
// rather than an inline &http.Client{} at each call site: invariant 11 makes
// TLS mandatory with SELF-SIGNED certificates and the bus's certificate
// fingerprint PINNED — no CA, and explicitly no trust-on-first-use.
//
// pins is the SET of certificates the bus may present — one normally, two for
// the duration of a rollover (MTLS-ROTATE). An EMPTY set means "no pin", and is
// legal ONLY for a plaintext loopback URL: transportSecurity refuses an https
// bus without one BEFORE this function is reached, so there is no path on which
// an unpinned TLS transport is built. It is spelled as a second branch here
// rather than an unconditional pinnedTLSConfig call because a
// pinnedTLSConfig(empty) would be a config that disables the default check and
// then matches nothing — a confusing failure instead of a clear refusal.
//
// CERTIFICATE VERIFICATION IS NEVER DISABLED. It is REPLACED, on the pinned
// path, by an exact-certificate check that is strictly stronger than the CA
// chain and hostname checks it stands in for; see pinnedTLSConfig's doc comment
// for what is and is not given up, and guard_test.go for the AST guard that
// keeps the two halves together. There is no Config field, flag or environment
// variable that turns verification off (DECISIONS.md, 2026-08-02, "E7: no
// plaintext escape hatch"), and tests mint real certificates rather than asking
// for one.
// It takes NO Config. Client.doer returns an embedder's cfg.HTTPClient before
// this function is reached, so a check for it here would be dead code — and
// dead code in the one place TLS is configured is exactly where a future reader
// draws the wrong conclusion about which branch runs. Nothing else on Config
// affects the transport: the timeout lives on the context, and the retry policy
// lives in do(). If that changes, pass the field, not the whole Config.
func newHTTPClient(pins BusPinSet, clientCert *tls.Certificate) HTTPDoer {
	// The pinned configuration, or — for the plaintext loopback case that is
	// all the bus serves until MTLS-LISTENER lands — the platform defaults with
	// verification fully ON.
	//
	// clientCert is deliberately NOT threaded into the unpinned literal. That
	// branch is reachable only for a PLAINTEXT loopback URL (transportSecurity
	// refuses an https bus with no pin before this function is reached), and a
	// plaintext connection performs no handshake, so there is nothing to
	// present. Adding it there would be dead configuration in the one place a
	// reader most needs to be able to tell which branch does the work.
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if !pins.IsEmpty() {
		tlsConfig = pinnedTLSConfig(pins, clientCert)
	}
	return &http.Client{
		Transport: &http.Transport{
			// NO PROXY, deliberately. Every request on this surface carries
			// either a bearer token or a signature over a server-chosen
			// challenge, and a proxy terminates the connection carrying it.
			// Once certificates are pinned (invariant 11) a proxy would also
			// present a certificate the invite never named, so honouring
			// HTTP_PROXY here would mean either a confusing failure or a
			// weakened check. A bus is reached directly.
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			// Chosen above: pinned when a fingerprint is in force, the
			// platform defaults otherwise. Never nil — a nil TLSClientConfig
			// is the platform default too, but spelling it out keeps one
			// obvious line for a reader asking "what does this verify".
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       idleConnTimeout,
			ForceAttemptHTTP2:     true,
		},
		// NEVER FOLLOW A REDIRECT. Go's default policy copies the
		// Authorization header across a redirect whenever the target's
		// canonical address matches — and canonicalAddr includes the port, so
		// https://bus:8080 → http://bus:8080 compares EQUAL and the bearer
		// token is forwarded in the clear. This API never legitimately
		// redirects, so returning the response as-is costs nothing and closes
		// a credential-downgrade path before there is a caller to walk into
		// it.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		// No per-client timeout: the deadline lives on the context, so one
		// bound covers the whole operation including its retries, and the poll
		// subcommand can raise it per call without reconfiguring the
		// transport.
	}
}

// request is one call to the bus.
type request struct {
	// method and path are the HTTP method and the path below the bus base URL,
	// e.g. http.MethodPost and "/v1/enroll".
	method, path string

	// query is the URL query string, when the route takes one. It is separate
	// from path because path is escaped as a PATH — a '?' smuggled into it
	// would be percent-encoded and the bus would see a route it does not serve.
	query url.Values

	// body is marshalled as JSON when non-nil.
	body interface{}

	// out receives the decoded JSON response body when non-nil and the status
	// is 2xx.
	out interface{}

	// bearer is the session token, sent as `Authorization: Bearer`. Empty on
	// the three credential-issuing routes, which by definition have none.
	bearer string

	// op names the operation for error messages.
	op string

	// retryable declares that repeating this request verbatim is SAFE.
	//
	// It is opt-in, never inferred from the method. A POST carrying an
	// idempotency key is safe to retry — that is what invariant 10 buys — and
	// a POST without one is not, and only the call site knows which it built.
	retryable bool

	// maxResponse overrides maxResponseBytes for routes whose legitimate
	// response is larger than the default bound. Zero means the default.
	//
	// It is a per-request knob rather than a raised global because the bound is
	// a memory-safety bound: an 8-byte enrolment reply has no business being
	// allowed a multi-megabyte body just because a message batch is.
	maxResponse int64
}

// response is what the caller of do gets back.
type response struct {
	Status int
	Header http.Header
}

// transportSecurity decides whether a transport may be built for this bus at
// all. It is the NO-TRUST-ON-FIRST-USE rule, expressed as a refusal.
//
// Two conditions, each fail-closed:
//
//   - https with NO pin is REFUSED. This is the whole point of the task: the
//     alternative — connect, remember whatever certificate turns up, and trust
//     it thereafter — is trust-on-first-use, and invariant 11 rules it out
//     explicitly ("there is no trust-on-first-use either"). The client learns
//     the fingerprint from the invite BEFORE its first connection or it does
//     not connect. A TOFU path is not offered as a flag, a fallback, or a test
//     convenience.
//   - http WITH a pin is REFUSED. There is no certificate on a plaintext
//     connection, so the pin cannot be checked — and a caller who passed
//     --bus-fingerprint believes it is being checked. Silently ignoring it
//     would hand back exactly the false sense of security this task exists to
//     prevent. (Plaintext at all is already limited to loopback by
//     parseBusURL.)
func transportSecurity(u *url.URL, pins BusPinSet) error {
	switch u.Scheme {
	case "https":
		if pins.IsEmpty() {
			return newError(KindConfig, "config",
				"no certificate fingerprint is pinned for "+u.String()+", and this client will not accept a certificate it was not told to expect",
				"agent-bus certificates are self-signed, there is no certificate authority, and there is deliberately no trust-on-first-use: pass --bus-fingerprint <hex> (env "+
					EnvBusFingerprint+") with the value from the invite, or enrol against this bus so the fingerprint is stored with the identity")
		}
	case "http":
		if !pins.IsEmpty() {
			return newError(KindUsage, "config",
				"a certificate fingerprint is pinned for "+u.String()+", but that is a plaintext URL and has no certificate to check",
				"use https:// so the fingerprint can actually be verified, or drop --bus-fingerprint / "+EnvBusFingerprint+" — it would otherwise look like a check that is not happening")
		}
	default:
		// UNREACHABLE TODAY — parseBusURL admits only http and https — and
		// present anyway, because a switch with no default fails OPEN here: an
		// unknown scheme would require no pin and no plaintext check, which is
		// the one outcome this function exists to prevent. That is the same
		// standard verifyPinnedBusCertificate applies to itself, and applying a
		// weaker one at the security seam because "the caller checks" is how
		// the seam stops being one.
		return newError(KindConfig, "config",
			"cannot decide how to secure a connection to "+u.String()+": unsupported URL scheme "+strconv.Quote(u.Scheme),
			"use an https:// bus URL (or http:// to a loopback address while the bus is still plaintext)")
	}
	return nil
}

// do executes req with retries and decodes the response.
func (c *Client) do(ctx context.Context, req request) (*response, error) {
	base, pins, err := c.endpoint()
	if err != nil {
		return nil, err
	}
	doer, err := c.doer(base, pins)
	if err != nil {
		return nil, err
	}

	var payload []byte
	if req.body != nil {
		payload, err = json.Marshal(req.body)
		if err != nil {
			return nil, wrapError(KindInternal, req.op, "cannot encode the request body", "", err)
		}
	}

	target := *base
	target.Path = base.Path + req.path
	if len(req.query) > 0 {
		target.RawQuery = req.query.Encode()
	}

	attempts := c.cfg.Retry.Attempts
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.backoff(attempt, lastErr)); err != nil {
				return nil, err
			}
		}
		resp, err := c.attempt(ctx, doer, target.String(), payload, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !req.retryable || !isRetryable(err) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt performs exactly one HTTP round trip.
//
// doer is passed in rather than read from the Client because it is chosen per
// bus, together with the certificate fingerprint pinned for that bus (see
// Client.doer). A transport picked up from a field could outlive the pin it was
// built for.
func (c *Client) attempt(ctx context.Context, doer HTTPDoer, urlStr string, payload []byte, req request) (*response, error) {
	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, urlStr, bodyReader)
	if err != nil {
		return nil, wrapError(KindInternal, req.op, "cannot build the request", "", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.bearer)
	}

	httpResp, err := doer.Do(httpReq)
	if err != nil {
		// The RESOLVED url, not c.cfg.BusURL: when the address came from the
		// stored identity, cfg.BusURL is empty and the message read "cannot
		// reach the bus at " with nothing after it, while the remedy named a
		// --bus flag the caller had not used.
		return nil, networkError(req.op, urlStr, err)
	}
	limit := int64(maxResponseBytes)
	if req.maxResponse > 0 {
		limit = req.maxResponse
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, limit))
		_ = httpResp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, limit))
	if err != nil {
		return nil, wrapError(KindNetwork, req.op, "the connection failed while reading the response", "retry; if it persists, check the bus's logs", err)
	}

	res := &response{Status: httpResp.StatusCode, Header: httpResp.Header}
	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		if req.out != nil {
			if err := json.Unmarshal(body, req.out); err != nil {
				return nil, wrapError(KindServer, req.op,
					"the bus returned a "+strconv.Itoa(httpResp.StatusCode)+" whose body is not the expected JSON",
					"check that --bus points at an agent-bus server and not at another service",
					err)
			}
		}
		return res, nil
	}
	return nil, statusError(req.op, req.path, httpResp, body)
}

// statusError maps a non-2xx response to a classified *Error.
//
// path is the request path this response answered — req.path, not anything
// parsed back off resp, because a test double's *http.Response has no
// populated Request field and a caller must not have to fake one just to get
// the right classification. It is used ONLY for the 404 case, to name the
// missing route (see the KindVersionSkew case below and 52930611).
//
// The mapping is by STATUS, and the bus's own `{"error":"..."}` message is
// carried through verbatim as the detail: the server deliberately writes a
// terse, non-enumerating reason there, so quoting it is both safe and the most
// useful thing we can tell a human.
//
// # The 503 split: Retry-After is the discriminator, not decoration
//
// A 503 from this bus means one of two OPPOSITE things, and the only signal
// telling them apart is whether a Retry-After header is present:
//
//   - WITH Retry-After — a live in-memory capacity bound (the applied-key
//     table, the per-agent waiter count, the roster, the session table). It is
//     transient by construction: something in flight finishes and the capacity
//     comes back. Back off and retry.
//   - WITHOUT Retry-After — hub.ErrNotDurable / hub.ErrPoisoned: the hub cannot
//     durably accept messages at all. The header's ABSENCE is deliberate, not an
//     oversight; dressing this up as retryable would be a lie, and retrying it
//     burns the caller's budget while hiding a fault an operator has to fix.
//     That is the one 503 the bus emits without the header today — EVERY
//     capacity refusal on EVERY route carries it — so its absence is a reliable
//     signal rather than a guess.
//
// The discriminator is the header's PRESENCE, not the delay parseRetryAfter got
// out of it. Those are not the same test: parseRetryAfter deliberately returns 0
// for the HTTP-date form, for `Retry-After: 0`, and for anything it cannot
// parse. Keying the split on the parsed value therefore condemned a 503 whose
// header was present but unparseable as "the bus cannot durably accept
// messages" and stopped a watch permanently. A present-but-unparseable header is
// now treated as TRANSIENT and falls back to the ordinary jittered backoff,
// which is the safer of the two mistakes: retrying a genuinely fatal 503 wastes
// a few requests, whereas giving up on a transient one takes a healthy watcher
// off the bus until a human notices.
//
// The second case is marked fatal, which takes it out of the retry loop
// (isRetryable) and is reported to callers by IsFatalUnavailable so a long-lived
// watcher can stop and say so instead of looping forever on a dead bus.
func statusError(op, path string, resp *http.Response, body []byte) *Error {
	detail := decodeServerError(body)
	e := &Error{Op: op, Status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = KindAuth
		e.Message = "the bus rejected this credential: " + detail
		e.Remedy = "the session may have expired or the bus may have restarted; retry, and if it persists re-enrol with `agent-busctl enrol`"
	case resp.StatusCode == http.StatusNotFound && path != routeSend:
		// KindVersionSkew, not KindRejected, for every route EXCEPT
		// routeSend: /v1/send answers a genuine PER-RESOURCE 404 —
		// hub.ErrUnknownRecipient, "unknown recipient" — for a message
		// addressed to an agent the bus does not know, which is exactly the
		// dynamic per-resource lookup this branch assumes does not exist on
		// this surface. That was a real defect (security gate finding F1,
		// 52930611): an unknown-recipient send was surfacing as an
		// infrastructure/version fault, with a remedy that told the caller to
		// point --bus at a DIFFERENT bus — actively wrong advice, and a nudge
		// toward trust-anchor churn under invariant 11's pinning. Every OTHER
		// route this client calls is a fixed path with no dynamic lookup
		// behind it (checked against every 404 site in internal/httpapi:
		// the session routes below, this recipient case, and the server's
		// generic route-not-registered catch-all are the only three), so a
		// 404 on any of THOSE still means the bus does not know the route.
		//
		// session.go's annotateSessionError OVERRIDES this for the two
		// session routes regardless of what is set here: a 404 on
		// /v1/session/begin or /v1/session/complete means the bus does not
		// know THIS AGENT (a stale local credential against a rebuilt bus),
		// which is an auth condition, not a missing route.
		e.Kind = KindVersionSkew
		e.Message = "the bus has no route for " + op + " (" + path + "): " + detail
		e.Remedy = "this bus build predates that route; upgrade the bus, or point --bus at one built at/after the commit that added it — this is not a rejection of your request, the bus does not know the route exists"
	case resp.StatusCode == http.StatusNotFound:
		// routeSend's 404: a genuine per-resource refusal (unknown
		// recipient), not a missing route. Falls through to the ordinary
		// KindRejected wording below, same as any other 4xx the bus
		// understood and refused on its merits.
		e.Kind = KindRejected
		e.Message = "the bus refused the request: " + detail
		e.Remedy = "correct the request and try again"
	case resp.StatusCode == http.StatusConflict:
		e.Kind = KindRejected
		e.Message = "the bus refused the request: " + detail
		e.Remedy = "an idempotency key was reused with different content; use a fresh key for new content (invariant 10)"
	case resp.StatusCode == http.StatusNotImplemented:
		// A DELIBERATE, PERMANENT refusal, not a fault. Today the only route
		// that answers 501 is /v1/broadcast (SIGN-6: a broadcast cannot be
		// signed under signing format v1, and the bus fails CLOSED rather than
		// carrying unsigned traffic — see the broadcast-specific annotation in
		// messages.go for the remedy an agent can act on). Nothing was applied:
		// the bus refuses before it even decodes the request body. KindRejected,
		// not KindServer, and no retry is ever advised — the same request will
		// fail the same way every time, so "retry" is actively wrong advice
		// here, unlike an ordinary 5xx.
		e.Kind = KindRejected
		e.Message = "the bus deliberately refuses this request: " + detail
		e.Remedy = "this is not a bus fault and not transient; do not retry"
	case resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") == "":
		// NOT transient. See the 503 split in this function's doc comment.
		e.Kind = KindServer
		e.fatal = true
		e.Message = "the bus cannot durably accept messages: " + detail
		e.Remedy = "this is not transient and retrying will not clear it — check the bus's logs for a non-durable or poisoned write path; nothing is acknowledged before it is durable (invariant 4), so the bus is refusing rather than losing data"
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusServiceUnavailable:
		e.Kind = KindServer
		e.Message = "the bus is at capacity: " + detail
		e.Remedy = "retry in a few seconds"
	case resp.StatusCode >= 500:
		e.Kind = KindServer
		e.Message = "the bus reported an internal error: " + detail
		e.Remedy = "check the bus's logs; the request id is in them"
	case resp.StatusCode >= 400:
		e.Kind = KindRejected
		e.Message = "the bus refused the request: " + detail
		e.Remedy = "correct the request and try again"
	default:
		e.Kind = KindServer
		e.Message = fmt.Sprintf("the bus answered %d: %s", resp.StatusCode, detail)
	}
	return e
}

// parseRetryAfter reads the delta-seconds form of Retry-After.
//
// The HTTP-date form is deliberately NOT accepted. The bus only ever sends
// delta-seconds (httpapi.capacityRetryAfterSeconds), and honouring a date
// would mean scheduling our retries against a remote clock we have no reason
// to trust and no way to check. An unparseable value is simply ignored, and
// the ordinary exponential backoff applies.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	const maxRetryAfter = 60
	if secs > maxRetryAfter {
		secs = maxRetryAfter
	}
	return time.Duration(secs) * time.Second
}

// decodeServerError extracts the bus's `{"error":"..."}` message, falling back
// to whatever the body actually was.
//
// BOTH branches go through safeText. The JSON branch is the one that matters
// and was the one originally missed: a hostile bus controls that string
// completely, and returning it verbatim put raw ESC, CR and BEL — unbounded —
// straight into a human's terminal. See sanitize.go.
func decodeServerError(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		if detail := safeText(payload.Error, maxDetailBytes); detail != "" {
			return detail
		}
		return "(no detail)"
	}
	if text := safeText(string(body), maxDetailBytes); text != "" {
		return text
	}
	return "(no detail)"
}

// networkError classifies a transport-level failure.
func networkError(op, busURL string, err error) *Error {
	// A cancelled or expired context is the caller's own deadline, not the
	// bus's fault, and saying "connection refused" would be a lie.
	if errors.Is(err, context.DeadlineExceeded) {
		return wrapError(KindNetwork, op,
			"timed out talking to the bus at "+busURL,
			"raise --timeout, or check the bus is reachable",
			err)
	}
	if errors.Is(err, context.Canceled) {
		return wrapError(KindNetwork, op, "cancelled while talking to the bus at "+busURL, "", err)
	}
	// The pin check FIRST. A fingerprint mismatch is a different event from a
	// certificate that failed the ordinary checks — it means the bus is not the
	// bus we were told to expect — and it gets its own message and its own
	// remedy rather than being folded into "did not verify".
	if isPinError(err) {
		return pinError(op, busURL, err)
	}
	if isCertificateError(err) {
		return wrapError(KindNetwork, op,
			"the bus's TLS certificate did not verify",
			"the certificate must match the fingerprint the invite named; do NOT work around this by disabling verification",
			err)
	}
	return wrapError(KindNetwork, op,
		"cannot reach the bus at "+busURL,
		"check --bus / "+EnvBusURL+" and that the bus is running",
		err)
}

// isCertificateError reports whether err is a TLS certificate verification
// failure.
//
// It matches the crypto/x509 error types rather than tls.CertificateVerification-
// Error, which is go1.20+ and this module is pinned at go1.19 (see go.mod and
// DECISIONS.md "go1.19 pin"). These three cover what a pinned self-signed
// deployment actually produces: an issuer we do not trust, a name that does
// not match, and an expired or otherwise invalid certificate.
func isCertificateError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var invalid x509.CertificateInvalidError
	return errors.As(err, &invalid)
}

// isRetryable reports whether repeating a request that failed with err could
// plausibly succeed. Only two families qualify: a transport failure, and the
// bus explicitly saying it is temporarily out of capacity.
func isRetryable(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	// A deadline or a cancellation is the caller's, and retrying it only burns
	// the remaining budget.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// A certificate that does not verify will not verify on the next attempt
	// either, and retrying an authentication failure looks like guessing. A
	// PINNED-fingerprint mismatch is the hardest of these: retrying it is
	// retrying a connection to a bus we have decided is the wrong one.
	if isPinError(err) || isCertificateError(err) {
		return false
	}
	// A 503 the bus declined to put a Retry-After on is a durability fault, not
	// a capacity refusal. Retrying it burns the caller's budget and delays the
	// moment an operator sees the real problem. See statusError.
	if e.fatal {
		return false
	}
	switch e.Kind {
	case KindNetwork:
		return true
	case KindServer:
		return e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable || e.Status == 0
	default:
		return false
	}
}

// backoff returns how long to wait before attempt n (1-based for the first
// RETRY), honouring a Retry-After the bus asked for.
//
// Full jitter — a uniform draw from [0, window) rather than the window itself
// — because several agents that were refused by the same capacity limit at the
// same instant would otherwise all come back at the same instant.
func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	window := c.cfg.Retry.BaseDelay << (attempt - 1)
	if window > c.cfg.Retry.MaxDelay || window <= 0 {
		window = c.cfg.Retry.MaxDelay
	}
	if hinted, ok := retryAfter(lastErr); ok && hinted > window {
		window = hinted
		if window > c.cfg.Retry.MaxDelay {
			window = c.cfg.Retry.MaxDelay
		}
	}
	return time.Duration(randUint64(uint64(window) + 1))
}

// retryAfter reads a Retry-After the bus sent, in seconds. The HTTP-date form
// is not accepted: the bus only ever sends the delta-seconds form, and
// accepting a date would mean trusting a remote clock to schedule our retries.
func retryAfter(err error) (time.Duration, bool) {
	var e *Error
	if !errors.As(err, &e) || e.retryAfter <= 0 {
		return 0, false
	}
	return e.retryAfter, true
}

// randUint64 returns a uniform value in [0, n) using crypto/rand.
//
// crypto/rand rather than math/rand so no seeding, no shared mutable state and
// no data race when several goroutines in an embedding agent back off at once.
func randUint64(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Cannot read randomness: fall back to the full window. Waiting longer
		// than intended is always safe; waiting zero is not.
		return n - 1
	}
	return binary.BigEndian.Uint64(buf[:]) % n
}

// sleep waits d, or returns early if ctx ends.
func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if c.sleepFn != nil {
		return c.sleepFn(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return wrapError(KindNetwork, "retry", "cancelled while backing off before a retry", "", ctx.Err())
	case <-t.C:
		return nil
	}
}

// resolveBusURL applies the documented resolution order for the bus address:
// the explicit --bus / AGENT_BUS_URL value, else the URL recorded on the
// selected identity at enrolment.
func (c *Client) resolveBusURL() (*url.URL, error) {
	if c.cfg.BusURL != "" {
		return parseBusURL(c.cfg.BusURL)
	}
	cred, err := c.credential()
	if err != nil {
		return nil, err
	}
	if cred.BusURL == "" {
		return nil, usagef("config", "pass --bus <url> or set "+EnvBusURL,
			"identity %s has no recorded bus URL", cred.AgentID)
	}
	return parseBusURL(cred.BusURL)
}
