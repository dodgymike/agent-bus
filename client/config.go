package client

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Defaults for Config. Every one of them is finite: there is no "wait forever"
// and no "retry until it works", because an agent shelling out cannot be left
// hanging with no way to notice.
const (
	// DefaultTimeout bounds a single client operation end to end, including
	// its retries. It is deliberately shorter than the bus's long-poll ceiling;
	// the poll subcommand raises it explicitly rather than inheriting a value
	// tuned for request/response calls.
	DefaultTimeout = 30 * time.Second

	// DefaultRetryAttempts is the TOTAL number of tries, not the number of
	// extra ones: 3 means one attempt and at most two retries.
	DefaultRetryAttempts = 3

	// DefaultRetryBaseDelay is the first backoff interval; it doubles per
	// attempt up to DefaultRetryMaxDelay.
	DefaultRetryBaseDelay = 200 * time.Millisecond

	// DefaultRetryMaxDelay caps the exponential backoff.
	DefaultRetryMaxDelay = 5 * time.Second
)

// Environment variables read by Config.ApplyEnv.
//
// Credentials and endpoints come from the environment and the credential store
// — NEVER from a terminal prompt. An agent shelling out has no TTY, so a
// prompt is not an inconvenience to it, it is a hang (invariant 7).
const (
	// EnvBusURL is the bus base URL, e.g. https://127.0.0.1:8080.
	EnvBusURL = "AGENT_BUS_URL"

	// EnvIdentityDir is the credential store DIRECTORY (not an agent id).
	EnvIdentityDir = "AGENT_BUS_IDENTITY"

	// EnvAgentID selects which stored identity to act as, without mutating the
	// stored "current" selection. This is what parallel agents sharing a
	// config directory should use.
	EnvAgentID = "AGENT_BUS_AGENT_ID"

	// EnvTimeout overrides DefaultTimeout; any value time.ParseDuration takes.
	EnvTimeout = "AGENT_BUS_TIMEOUT"

	// EnvBusFingerprint is the SHA-256 fingerprint of the bus's TLS certificate
	// that this client will accept, as 64 lowercase hex characters.
	//
	// It is the value the invite blob carries. It is NOT a secret — it is
	// published in the bus's startup log and derivable by anyone who completes
	// a handshake — so an environment variable is a perfectly good carrier for
	// it, unlike a key.
	EnvBusFingerprint = "AGENT_BUS_FINGERPRINT"

	// EnvPersistSession opts in to caching the session token on disk between
	// processes. Any of "1", "true", "yes", "on" (case-insensitive) enables it;
	// anything else, including empty, leaves it OFF.
	//
	// It is a SECURITY-RELEVANT opt-in and the default is deliberately the safe
	// one — see Config.PersistSession.
	EnvPersistSession = "AGENT_BUS_PERSIST_SESSION"
)

// RetryPolicy bounds how hard the client tries before giving up.
//
// Retries are applied ONLY to requests the client knows are safe to repeat —
// see request.retryable. A retry of a non-idempotent mutation would violate
// invariant 10 from the client side, which no amount of server-side
// idempotency can fix if the client never sent a key.
type RetryPolicy struct {
	// Attempts is the total number of tries. Values below 1 are treated as 1.
	Attempts int

	// BaseDelay is the first backoff interval.
	BaseDelay time.Duration

	// MaxDelay caps the exponential growth of BaseDelay.
	MaxDelay time.Duration
}

// Config configures a Client. The zero value is not usable; start from
// DefaultConfig.
type Config struct {
	// BusURL is the base URL of the bus, e.g. "https://127.0.0.1:8080". It may
	// carry a path prefix. It may be empty, in which case operations that need
	// a bus fall back to the selected identity's recorded URL, and enrolment —
	// which has no identity yet — fails with KindUsage.
	BusURL string

	// BusFingerprint is the SHA-256 of the bus certificate's DER, as 64
	// lowercase hex characters — the value the invite blob carries.
	//
	// # This is a PIN, and there is no alternative to it over https
	//
	// agent-bus certificates are self-signed and there is no certificate
	// authority (invariant 11), so this is the ONLY thing that says which bus is
	// on the other end. An https bus with no pin — from here, from
	// EnvBusFingerprint, or from the selected identity — is REFUSED, because the
	// alternative is trust-on-first-use and invariant 11 rules it out by name.
	//
	// Empty is legal for the plaintext-loopback case the bus still serves today
	// (and only that case: parseBusURL refuses non-loopback http, and
	// transportSecurity refuses a pin on a plaintext URL, since there would be
	// no certificate to check it against).
	//
	// It is NOT SECRET. It is in the bus's startup log and in every handshake.
	BusFingerprint string

	// IdentityDir is the credential store directory. Empty means
	// DefaultIdentityDir.
	IdentityDir string

	// AgentID selects a stored identity by fully-qualified id or by unique
	// short name. Empty means "whatever `use` last selected".
	AgentID string

	// Timeout bounds one operation end to end. Zero means DefaultTimeout.
	Timeout time.Duration

	// PersistSession caches the session token in the credential store directory
	// so the NEXT process reuses it instead of running a fresh handshake.
	//
	// # Default false, and the default is the security-preferred one
	//
	// A session is a bearer token: whoever reads the file can act as this agent
	// until it expires. Off, the token exists only in this process's memory.
	//
	// # Why the option exists at all
	//
	// The bus caps one agent at 32 concurrent sessions, holds each for an hour
	// and evicts nothing (internal/auth). Under invariant 7 an agent shells out
	// per command, so each command costs a session — and an agent working
	// faster than one command every two minutes exhausts its own cap and is
	// refused for up to an hour. Observed in production 2026-08-15.
	//
	// Enable it for an agent that shells out repeatedly on a machine whose
	// local users are all trusted. Leave it off for a long-lived embedding
	// client, which already reuses one session in memory and gains nothing, and
	// on any shared host. See client/sessionstore.go and DECISIONS.md
	// 2026-08-16.
	PersistSession bool

	// Retry bounds the retry loop. A zero Attempts means DefaultRetryAttempts.
	Retry RetryPolicy

	// HTTPClient overrides the transport entirely. It is the escape hatch for
	// an embedding agent that already has a configured, instrumented client —
	// and, deliberately, the ONLY way to substitute a transport, so that
	// newHTTPClient stays the single place TLS is configured. Leave it nil
	// unless you mean it.
	//
	// # SETTING THIS TURNS OFF CERTIFICATE PINNING. Both halves of it.
	//
	// Spelled out on the field an embedder actually reads, not only on the
	// interface below. Client.doer returns this value BEFORE it consults
	// transportSecurity, so supplying one bypasses:
	//
	//   - the pinned-fingerprint check itself (your transport's tls.Config is
	//     the one that runs), AND
	//   - the refusal to speak https to a bus with NO pin at all — i.e. you
	//     also take on the no-trust-on-first-use rule.
	//
	// That is the correct trade for an embedder who already owns its
	// verification, and it is not a supported way to relax anything: nothing in
	// this package or in cmd/agent-busctl ever sets it, and there is no flag or
	// environment variable that reaches it. If you set it, verify the bus's
	// certificate yourself — client.ParseBusFingerprint and
	// client.BusFingerprint are exported so you can reuse the same construction
	// rather than inventing a second one.
	HTTPClient HTTPDoer

	// KeyRing is the trust store the READ path verifies senders against. Nil
	// means a DirKeyRing under IdentityDir/TrustedKeysDirName, which is what New
	// installs and what `agent-busctl trust` writes.
	//
	// It is a knob so an embedding agent can source trust from wherever it
	// already keeps it. It is NOT a way to turn verification off: a KeyRing that
	// answers "yes" to everything is a bus that can forge any message from any
	// sender, and this package will never ship one. There is no allow-unsigned
	// mode, no --insecure, and no field that produces one. A Client whose KeyRing
	// is nil AND whose config was never defaulted trusts NOBODY and rejects
	// everything — fail closed, in the direction that loses messages rather than
	// accepting forged ones.
	KeyRing KeyRing
}

// HTTPDoer is the narrow slice of *http.Client the transport uses. It is an
// interface so an embedder can supply an instrumented or policy-wrapped client
// without this package depending on their type; *http.Client satisfies it.
//
// Supplying one BYPASSES newHTTPClient, and therefore bypasses BOTH the pinned
// certificate check AND the refusal to speak https to a bus with no pin
// (invariant 11). See Config.HTTPClient above for what that leaves you
// responsible for. It is a deliberate, documented trade for embedders who
// already own their transport — it is not a supported way to relax
// verification, and this package will never ship a Config field that does.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DefaultConfig returns a Config with every default filled in and no bus,
// identity or store path chosen.
func DefaultConfig() Config {
	return Config{
		Timeout: DefaultTimeout,
		Retry: RetryPolicy{
			Attempts:  DefaultRetryAttempts,
			BaseDelay: DefaultRetryBaseDelay,
			MaxDelay:  DefaultRetryMaxDelay,
		},
	}
}

// ApplyEnv fills any UNSET field of c from the environment and returns the
// result. It never overrides a value that is already set.
//
// THE RESOLUTION ORDER IS: explicit value (a CLI flag) > environment variable >
// the selected identity's recorded value (applied later, at request time, for
// BusURL and BusFingerprint) > built-in default. It is deterministic and it is
// documented in CONTRACTS-CLI.md; do not add a step without updating both.
//
// BusFingerprint has ONE documented deviation, and it is deliberate: when an
// explicit fingerprint and the stored identity's fingerprint name DIFFERENT
// certificates for the SAME bus, neither wins and the operation is refused
// (Client.resolvePin). Precedence is the right rule for an address and the
// wrong rule for a security pin — "it stopped working so I passed the
// fingerprint the other end gave me" is how a substituted certificate gets
// accepted, and letting the flag win would turn a detected substitution into a
// successful one. That deviation lives in resolvePin, not here: this function
// only merges flags with the environment, and the flag > env half is normal.
//
// lookup is os.LookupEnv in production and a map in tests; passing it in keeps
// this function pure and keeps the tests from mutating process state.
func (c Config) ApplyEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if c.BusURL == "" {
		if v, ok := lookup(EnvBusURL); ok {
			c.BusURL = v
		}
	}
	if c.BusFingerprint == "" {
		if v, ok := lookup(EnvBusFingerprint); ok {
			// TrimSpace, matching how EnvTimeout is read below: surrounding
			// whitespace on an environment value is a shell artefact, not part
			// of the value. ParseBusFingerprint itself stays strict — it
			// rejects uppercase, colons and any other spelling — so this
			// tolerates a stray newline without tolerating a different
			// fingerprint.
			c.BusFingerprint = strings.TrimSpace(v)
		}
	}
	if c.IdentityDir == "" {
		if v, ok := lookup(EnvIdentityDir); ok {
			c.IdentityDir = v
		}
	}
	if c.AgentID == "" {
		if v, ok := lookup(EnvAgentID); ok {
			c.AgentID = v
		}
	}
	if c.Timeout == 0 {
		if v, ok := lookup(EnvTimeout); ok && strings.TrimSpace(v) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(v))
			if err != nil {
				return c, usagef("config",
					"set "+EnvTimeout+" to a Go duration such as 30s, 1m or 500ms",
					"%s is not a valid duration: %q", EnvTimeout, v)
			}
			if d <= 0 {
				return c, usagef("config",
					"set "+EnvTimeout+" to a positive duration such as 30s",
					"%s must be positive, got %s", EnvTimeout, d)
			}
			c.Timeout = d
		}
	}
	if !c.PersistSession {
		if v, ok := lookup(EnvPersistSession); ok {
			c.PersistSession = envTruthy(v)
		}
	}
	return c, nil
}

// envTruthy reports whether an environment value means "on".
//
// It is deliberately a CLOSED set rather than "non-empty means true". This
// switch turns on writing a bearer token to disk, and the classic shape
// `AGENT_BUS_PERSIST_SESSION=0` or `=false` — set by someone intending to
// DISABLE it — must not enable it. An unrecognised value leaves it off, which
// is the safe direction.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// withDefaults returns c with every zero-valued knob replaced by its default.
func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Retry.Attempts <= 0 {
		c.Retry.Attempts = DefaultRetryAttempts
	}
	if c.Retry.BaseDelay <= 0 {
		c.Retry.BaseDelay = DefaultRetryBaseDelay
	}
	if c.Retry.MaxDelay <= 0 {
		c.Retry.MaxDelay = DefaultRetryMaxDelay
	}
	return c
}

// DefaultIdentityDir returns the credential store directory: the user's config
// directory plus "agent-bus".
//
// It is os.UserConfigDir, so it honours XDG_CONFIG_HOME on Linux. It is NEVER
// a path inside the repository: credentials in a working tree get committed,
// and this is the function that makes "somewhere sensible" the default rather
// than something a caller has to remember.
func DefaultIdentityDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", wrapError(KindConfig, "config",
			"cannot determine the user configuration directory",
			"set "+EnvIdentityDir+" or pass --identity <dir> to choose the credential store location",
			err)
	}
	return filepath.Join(dir, "agent-bus"), nil
}

// parseBusURL validates and canonicalises a bus base URL.
//
// The restrictions are all fail-closed, and each one has a reason:
//
//   - The scheme must be http or https. Anything else is a typo or an attempt
//     to make the client speak a protocol it does not.
//   - There must be a host. "http:///v1" would otherwise dial nothing.
//   - USERINFO IS REJECTED. Credentials in a URL end up in shell history,
//     process listings and logs, and this client's credential is an Ed25519
//     key in a 0600 file precisely so it never has to travel that way.
//   - Query and fragment are rejected: a base URL is a prefix, and silently
//     dropping a query string a caller thought was meaningful is worse than
//     refusing it.
//
// The path is canonicalised: redundant literal slashes and dot segments are
// collapsed, and an empty-or-root result becomes "" so equivalent base URLs
// share one scope key.
func parseBusURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, usagef("config",
			"pass --bus <url> or set "+EnvBusURL,
			"no bus URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, wrapError(KindUsage, "config",
			"bus URL is not a valid URL: "+trimmed,
			"use a full base URL such as https://127.0.0.1:8080",
			err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Invariant 11: TLS is the required transport, and the session token
		// is a BEARER credential that /v1/session/begin returns in a response
		// body. Plaintext to anything but loopback puts it on the wire in
		// clear, where an on-path observer can read it or kill a pending
		// challenge.
		//
		// The bus does not serve TLS yet, so refusing http outright would
		// leave no working client at all. Refusing NON-LOOPBACK http is the
		// bound that costs nothing today and forecloses the dangerous case:
		// it is the client-side half of E8's sequencing constraint ("the bus
		// must NOT be exposed on a non-loopback interface before mTLS lands").
		// DELETE THIS CASE ENTIRELY when the TLS listener ships.
		if !isLoopbackHost(u.Hostname()) {
			return nil, usagef("config",
				"use https:// — plaintext is permitted only to a loopback address, because the session token is a bearer credential and travels in the clear over http (invariant 11)",
				"bus URL %q is plaintext http to a non-loopback host", trimmed)
		}
	case "":
		return nil, usagef("config",
			"include the scheme, e.g. https://"+trimmed,
			"bus URL %q has no scheme", trimmed)
	default:
		return nil, usagef("config",
			"use an http:// or https:// URL",
			"bus URL scheme %q is not supported", u.Scheme)
	}
	if u.Host == "" {
		return nil, usagef("config",
			"use a full base URL such as https://127.0.0.1:8080",
			"bus URL %q has no host", trimmed)
	}
	if u.User != nil {
		return nil, usagef("config",
			"remove the credentials from the URL; this client authenticates with an enrolled key, not with URL userinfo",
			"bus URL must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, usagef("config",
			"pass only the scheme, host and any path prefix",
			"bus URL must not contain a query string or fragment")
	}
	// CANONICALISE, because this string is a scope key.
	//
	// The stored idempotency records are scoped to (key, bus URL) — matching
	// the server, where a key is remembered by one bus and unknown to another.
	// If "https://BUS:443" and "https://bus" were different scopes, a retry
	// spelled the other way would miss its own pending record, generate a
	// fresh key pair and re-send the same idempotency key with a different
	// payload: a 409 the bus rejects and logs (it does NOT disconnect; narrowed
	// 2026-08-08), caused entirely by capitalisation.
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = canonicalHost(u.Scheme, u.Host)
	setCanonicalURLPath(u)
	return u, nil
}

// setCanonicalURLPath collapses redundant literal slashes and dot segments so
// equivalent base URLs share one idempotency scope key, while preserving any
// escaped spelling the caller used for non-dot path bytes.
func setCanonicalURLPath(u *url.URL) {
	escaped := u.EscapedPath()
	if escaped == "" {
		u.Path = ""
		u.RawPath = ""
		return
	}
	cleaned := path.Clean(escaped)
	if cleaned == "." || cleaned == "/" {
		u.Path = ""
		u.RawPath = ""
		return
	}
	decoded, err := url.PathUnescape(cleaned)
	if err != nil {
		// url.Parse accepted the path and path.Clean only removed separators and
		// literal dot segments, so this should be unreachable. Fail closed to the
		// cleaned string if it ever happens rather than keeping two scope keys.
		u.Path = cleaned
		u.RawPath = ""
		return
	}
	u.Path = decoded
	if cleaned == decoded {
		u.RawPath = ""
		return
	}
	u.RawPath = cleaned
}

// canonicalHost lower-cases the host and drops the port when it is the
// scheme's default, so equivalent spellings produce one string.
//
// It mirrors cmd/agent-bus/invite.go's canonicalInviteHost, which fixed the
// server-side copy of this exact bug (2cf20abf's sibling report) and
// documents the divergence in detail: net.SplitHostPort strips the brackets
// off an IPv6 literal, and the default-port branch used to return the bare
// host unchanged — turning "[::1]:443" into "::1", which is not a legal URL
// host. This string is used as an idempotency scope key (see the CANONICALISE
// comment on parseBusURL above), so a malformed host here does not just fail
// to dial — it corrupts which retries scope to which record.
func canonicalHost(scheme, host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port present — SplitHostPort's error case for a bare host. An
		// IPv6 literal from a parsed URL is already bracketed here (url.URL.Host
		// keeps the brackets when there is no port) and stays that way.
		return strings.ToLower(host)
	}
	h = strings.ToLower(h)
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		if strings.Contains(h, ":") {
			// An IPv6 literal: SplitHostPort removed the brackets that make it
			// a legal URL host, so put them back.
			return "[" + h + "]"
		}
		return h
	}
	return net.JoinHostPort(h, port)
}

// isLoopbackHost reports whether host names the local machine.
//
// It is deliberately NARROW: literal loopback addresses and the exact name
// "localhost". It does NOT resolve DNS, because a name that resolves to
// 127.0.0.1 today can resolve elsewhere tomorrow, and a security check that
// depends on a resolver is a check an attacker can move.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
