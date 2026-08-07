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
const userAgent = "busctl"

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

// newHTTPClient builds the transport. It is THE ONLY place in this package
// where an http.Client or a tls.Config is constructed.
//
// That single-seam property is the point, and it is why this function exists
// rather than an inline &http.Client{} at each call site: invariant 11 makes
// TLS mandatory with SELF-SIGNED certificates, MUTUAL authentication, and the
// bus's certificate fingerprint PINNED from the invite blob — no CA, and
// explicitly no trust-on-first-use. When that lands it configures
// tls.Config.Certificates (our client cert) and
// tls.Config.VerifyPeerCertificate (the pinned fingerprint check) HERE, and
// nowhere else.
//
// CERTIFICATE VERIFICATION IS NEVER DISABLED. tls.Config's skip-verification
// field is not set, is not settable through Config, and must never appear in
// this tree — including in tests (DECISIONS.md, 2026-08-02, "E7: no plaintext
// escape hatch"). Tests mint real certificates instead. The field is not named
// in this comment on purpose, so that grepping the tree for its name finds
// only real uses rather than the rules forbidding them.
func newHTTPClient(cfg Config) HTTPDoer {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
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
			// The zero tls.Config: the platform defaults, with verification
			// ON. The pinning configuration replaces this whole value when
			// invariant 11's listener lands; it is spelled out rather than
			// left nil so there is one obvious line to change.
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
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

// do executes req with retries and decodes the response.
func (c *Client) do(ctx context.Context, req request) (*response, error) {
	base, err := c.resolveBusURL()
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
		resp, err := c.attempt(ctx, target.String(), payload, req)
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
func (c *Client) attempt(ctx context.Context, urlStr string, payload []byte, req request) (*response, error) {
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

	httpResp, err := c.http.Do(httpReq)
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
	return nil, statusError(req.op, httpResp, body)
}

// statusError maps a non-2xx response to a classified *Error.
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
func statusError(op string, resp *http.Response, body []byte) *Error {
	detail := decodeServerError(body)
	e := &Error{Op: op, Status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = KindAuth
		e.Message = "the bus rejected this credential: " + detail
		e.Remedy = "the session may have expired or the bus may have restarted; retry, and if it persists re-enrol with `busctl enrol`"
	case resp.StatusCode == http.StatusNotFound:
		e.Kind = KindRejected
		e.Message = "the bus answered 404: " + detail
		e.Remedy = "this bus build may not serve that route yet; check `busctl --help` and the bus version"
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
	// either, and retrying an authentication failure looks like guessing.
	if isCertificateError(err) {
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
