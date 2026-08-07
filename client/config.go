package client

import (
	"net"
	"net/http"
	"net/url"
	"os"
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

	// IdentityDir is the credential store directory. Empty means
	// DefaultIdentityDir.
	IdentityDir string

	// AgentID selects a stored identity by fully-qualified id or by unique
	// short name. Empty means "whatever `use` last selected".
	AgentID string

	// Timeout bounds one operation end to end. Zero means DefaultTimeout.
	Timeout time.Duration

	// Retry bounds the retry loop. A zero Attempts means DefaultRetryAttempts.
	Retry RetryPolicy

	// HTTPClient overrides the transport entirely. It is the escape hatch for
	// an embedding agent that already has a configured, instrumented client —
	// and, deliberately, the ONLY way to substitute a transport, so that
	// newHTTPClient stays the single place TLS is configured. Leave it nil
	// unless you mean it.
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
// Supplying one BYPASSES newHTTPClient, and therefore bypasses the TLS
// configuration this package will pin certificates in (invariant 11). That is
// a deliberate, documented trade for embedders who already own their
// transport — it is not a supported way to relax verification, and this
// package will never ship a Config field that does.
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
// the selected identity's recorded value (applied later, at request time, and
// only for BusURL) > built-in default. It is deterministic and it is
// documented in CONTRACTS-CLI.md; do not add a step without updating both.
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
	return c, nil
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
// A trailing slash is trimmed so joining a path is unambiguous.
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
	// payload: 409 and a disconnect, caused entirely by capitalisation.
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = canonicalHost(u.Scheme, u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// canonicalHost lower-cases the host and drops the port when it is the
// scheme's default, so equivalent spellings produce one string.
func canonicalHost(scheme, host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port present (SplitHostPort's error case for a bare host).
		return strings.ToLower(host)
	}
	h = strings.ToLower(h)
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
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
