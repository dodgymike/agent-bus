package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Client is a connection to one bus, acting as at most one identity.
//
// It is safe for concurrent use: the session it caches is guarded, so an
// embedding agent can drive several goroutines through one Client without
// each of them establishing its own session.
//
// A Client is CHEAP. Nothing is dialled, no file is read and no session is
// established until the first call that needs one, so constructing one in a
// command that turns out not to need the network costs nothing.
type Client struct {
	cfg   Config
	store *Store

	// pin is Config.BusFingerprint, parsed once in New so a typo fails at
	// construction and names the flag rather than surfacing mid-handshake.
	// Zero means "none was given explicitly"; the identity may still carry one.
	pin BusFingerprint

	// sleepFn overrides the backoff sleep in tests. Nil in production.
	sleepFn func(context.Context, time.Duration) error

	// nowFn overrides the clock in tests. Nil in production.
	nowFn func() time.Time

	mu sync.Mutex
	// http is the transport, built LAZILY.
	//
	// It cannot be built in New: the bus URL may come from the selected
	// identity and the pinned fingerprint may come from the same record, so
	// neither is known until the first request resolves them. httpPin is the
	// fingerprint http was built for; when the resolved pin differs — the
	// caller switched identity, or supplied one explicitly — the transport is
	// rebuilt rather than reused, because a pooled connection was verified
	// against the OLD pin.
	http    HTTPDoer
	httpPin BusFingerprint

	// cred caches the resolved credential so a command that touches the store
	// twice does not read and parse it twice.
	cred *Credential
	// session is the current bearer credential. It is NEVER written to disk —
	// see EnsureSession.
	session *session

	// handshakeMu single-flights session establishment. It is separate from mu
	// because it is held across two network round trips, and holding mu that
	// long would serialise every unrelated call on the client. See
	// ensureSession.
	handshakeMu sync.Mutex
}

// New builds a Client from cfg.
//
// It opens (creating if necessary) the credential store, because every
// operation this client offers either reads an identity or writes one, and
// discovering that the config directory is unwritable at the moment a
// server-minted agent id needs saving is the worst possible time to find out.
func New(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()

	if cfg.BusURL != "" {
		// Fail fast on a malformed --bus rather than at the first request,
		// so the error names the flag rather than an operation.
		if _, err := parseBusURL(cfg.BusURL); err != nil {
			return nil, err
		}
	}

	// Same reasoning for the pin: a mistyped fingerprint is a usage error about
	// a flag, and discovering it inside a TLS handshake would report it as a
	// connection failure.
	var pin BusFingerprint
	if cfg.BusFingerprint != "" {
		var err error
		if pin, err = ParseBusFingerprint(cfg.BusFingerprint); err != nil {
			return nil, err
		}
	}

	dir := cfg.IdentityDir
	if dir == "" {
		var err error
		dir, err = DefaultIdentityDir()
		if err != nil {
			return nil, err
		}
		cfg.IdentityDir = dir
	}
	store, err := OpenStore(dir)
	if err != nil {
		return nil, err
	}
	if cfg.KeyRing == nil {
		// The trust store lives beside the credentials, under the same 0700
		// directory, because "which keys do I hold" and "who am I" are answers to
		// the same question and an operator should not have to point two flags at
		// two places. Nothing is created here: a missing directory is an EMPTY
		// trust store, which is the correct reading before any key has been
		// exchanged.
		cfg.KeyRing = NewDirKeyRing(filepath.Join(dir, TrustedKeysDirName))
	}

	return &Client{cfg: cfg, pin: pin, store: store}, nil
}

// endpoint resolves the bus this client talks to AND the certificate
// fingerprint pinned for it, together.
//
// They are resolved as one thing on purpose: an address without its pin is the
// input to a trust-on-first-use connection, and keeping the two in separate
// functions is how a caller ends up with one and not the other.
func (c *Client) endpoint() (*url.URL, BusFingerprint, error) {
	u, err := c.resolveBusURL()
	if err != nil {
		return nil, BusFingerprint{}, err
	}
	pin, err := c.resolvePin(u)
	if err != nil {
		return nil, BusFingerprint{}, err
	}
	return u, pin, nil
}

// resolvePin returns the fingerprint the bus at u must present.
//
// The order matches every other setting (CONTRACTS-CLI.md): the explicit
// --bus-fingerprint / AGENT_BUS_FINGERPRINT value, else the one recorded on the
// selected identity at enrolment. With one addition that is specific to a
// security pin:
//
//   - The STORED pin is used only for the bus it was stored for. It is
//     recorded next to that identity's BusURL, and applying it to a different
//     --bus would be pinning bus A's certificate on bus B — which fails every
//     connection and reads as a broken client rather than as a mistake.
//   - When both exist and DISAGREE, this is a hard refusal, not a precedence
//     question. One of the two is wrong about which bus this is, and silently
//     preferring the value that arrived on the command line is precisely how an
//     operator is talked into re-pinning: "it did not work, so I passed the
//     fingerprint the other end gave me". Refusing makes the disagreement
//     visible, which is the only way it gets checked out of band.
//
// A missing or unreadable identity is NOT an error here — enrolment happens
// before any identity exists, and that is the case that most needs a pin.
func (c *Client) resolvePin(u *url.URL) (BusFingerprint, error) {
	stored, err := c.storedPin(u)
	if err != nil {
		return BusFingerprint{}, err
	}
	switch {
	case c.pin.IsZero():
		return stored, nil
	case stored.IsZero(), c.pin.Equal(stored):
		return c.pin, nil
	default:
		return BusFingerprint{}, newError(KindUsage, "config",
			"--bus-fingerprint says "+c.pin.String()+" but the stored identity for "+u.String()+" pinned "+stored.String(),
			"these name two different certificates and one of them is wrong. Confirm which is genuine OUT OF BAND (the bus logs `bus_cert_fingerprint=…` at startup) before doing anything else; then either drop the flag, or `agent-busctl logout` that identity and enrol again against the fingerprint you confirmed")
	}
}

// storedPin returns the fingerprint recorded on the selected identity, but only
// when that identity was enrolled with the bus at u.
func (c *Client) storedPin(u *url.URL) (BusFingerprint, error) {
	cred, err := c.credential()
	if err != nil {
		// No identity, or none selected. Not a pin problem: the caller is
		// enrolling, and the operations that need an identity raise this
		// themselves with a better message.
		return BusFingerprint{}, nil
	}
	if cred.BusFingerprint == "" || cred.BusURL != u.String() {
		return BusFingerprint{}, nil
	}
	pin, err := ParseBusFingerprint(cred.BusFingerprint)
	if err != nil {
		// Stored, and unreadable. Fail rather than fall back to "no pin": that
		// fallback would turn a damaged credential store into an unpinned
		// connection, which is the one outcome that must never happen quietly.
		return BusFingerprint{}, newError(KindConfig, "config",
			"the certificate fingerprint stored for identity "+cred.AgentID+" is not a valid fingerprint",
			"the credential store may be damaged; pass --bus-fingerprint <hex> for this command, or `agent-busctl logout "+cred.AgentID+"` and enrol again")
	}
	return pin, nil
}

// doer returns the transport for one bus, building it on first use and reusing
// it while the pin is unchanged.
//
// The security decision (transportSecurity) is made BEFORE the cache is
// consulted, so a refusal cannot be skipped by an earlier call having succeeded.
func (c *Client) doer(u *url.URL, pin BusFingerprint) (HTTPDoer, error) {
	if c.cfg.HTTPClient != nil {
		// The embedder's own transport. Documented on Config.HTTPClient as
		// bypassing this package's TLS configuration entirely — including the
		// pin — because an embedder that supplies a transport owns its
		// verification. It is not a way to relax anything: nothing in this
		// package or the CLI ever sets it.
		return c.cfg.HTTPClient, nil
	}
	if err := transportSecurity(u, pin); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http == nil || !c.httpPin.Equal(pin) {
		closeIdleConnections(c.http)
		c.http = newHTTPClient(pin)
		c.httpPin = pin
	}
	return c.http, nil
}

// closeIdleConnections drops the sockets of a transport that has been
// superseded, instead of leaving them to IdleConnTimeout's 90 seconds.
//
// This is hygiene, NOT the security control — a transport built for pin A
// structurally cannot accept a certificate matching pin B, so a lingering
// socket could never carry a wrongly-verified request. What it avoids is a
// client that switched identity holding open connections to a bus it has
// stopped talking to, which is confusing to anyone reading netstat during an
// incident.
//
// It is type-switched rather than typed, because HTTPDoer is deliberately the
// narrow Do-only interface an embedder can satisfy; a doer that is not an
// *http.Client simply has nothing to close.
func closeIdleConnections(doer HTTPDoer) {
	type idleCloser interface{ CloseIdleConnections() }
	if c, ok := doer.(idleCloser); ok {
		c.CloseIdleConnections()
	}
}

// keyRing returns the trust store the read path verifies against.
//
// A nil ring is returned as nil and MUST be treated by the caller as "trusts
// nobody" — see verifyReceivedMessage. It is deliberately not replaced with a
// permissive default here: a fallback that made an unconfigured client accept
// everything would be exactly the silent hole invariant 9 warns about, since
// every test would still pass while nothing was actually being verified.
func (c *Client) keyRing() KeyRing { return c.cfg.KeyRing }

// messagingKey returns this identity's MESSAGING private key, minting one on
// first use.
//
// The mint is a locked store write (Store.EnsureMessagingKey); the cached
// credential is refreshed here so a long-lived Client does not keep handing back
// the pre-mint copy with an empty seed and re-minting on every send.
func (c *Client) messagingKey() (ed25519.PrivateKey, error) {
	cred, err := c.credential()
	if err != nil {
		return nil, err
	}
	if cred.MessagingKeySeed == "" {
		cred, err = c.store.EnsureMessagingKey(cred.AgentID)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.cred = &cred
		c.mu.Unlock()
	}
	return cred.MessagingPrivateKey()
}

// MessagingPublicKey returns the base64 PUBLIC half of this identity's messaging
// key, minting the key on first use.
//
// This is the value that must reach a peer OUT OF BAND for that peer to be able
// to verify anything this agent sends. There is no route that publishes it: no
// messaging key is registered at enrolment and CRYPTO-4 (the server-attested key
// bundle) does not exist, so until it lands the exchange is a human copying this
// string into the peer's `agent-busctl trust`. Say so when you print it.
func (c *Client) MessagingPublicKey() (Identity, string, error) {
	cred, err := c.credential()
	if err != nil {
		return Identity{}, "", err
	}
	cred, err = c.store.EnsureMessagingKey(cred.AgentID)
	if err != nil {
		return Identity{}, "", err
	}
	c.mu.Lock()
	c.cred = &cred
	c.mu.Unlock()
	pub, err := cred.MessagingPublicKey()
	if err != nil {
		return Identity{}, "", err
	}
	return cred.Identity, pub, nil
}

// TrustPeer records encoded (standard base64 of a 32-byte Ed25519 messaging
// public key) as the key agentID's messages are verified with.
//
// It lives in this package rather than in the CLI because cmd/agent-busctl is a THIN
// shell over it (see cmd/agent-busctl/main.go): anything implemented only there is
// something an agent EMBEDDING the client cannot reach, and an embedder that
// cannot populate its own trust store cannot verify anything at all.
//
// The key must have come from OUT OF BAND — the peer ran `agent-busctl keygen` and a
// human, or a deployment system, carried the string across. A key learned from
// the bus, or from beside a signature, is worth nothing: see keyring.go.
func (c *Client) TrustPeer(agentID, encoded string) (TrustedKey, error) {
	ring, ok := c.keyRing().(*DirKeyRing)
	if !ok {
		return TrustedKey{}, newError(KindConfig, "trust",
			"this client's trust store is not a local directory and cannot be written to",
			"the embedding program supplied its own KeyRing; add the key there instead")
	}
	pub, err := decodeMessagingPublicKey(strings.TrimSpace(encoded))
	if err != nil {
		return TrustedKey{}, newError(KindUsage, "trust", err.Error(),
			"pass the base64 key exactly as the peer printed it with `agent-busctl keygen`")
	}
	if err := ring.Trust(agentID, pub); err != nil {
		return TrustedKey{}, err
	}
	return TrustedKey{AgentID: agentID, PublicKey: base64.StdEncoding.EncodeToString(pub)}, nil
}

// TrustedKeys lists the peers this agent holds a messaging key for.
func (c *Client) TrustedKeys() ([]TrustedKey, error) {
	ring, ok := c.keyRing().(*DirKeyRing)
	if !ok {
		return []TrustedKey{}, nil
	}
	return ring.List()
}

// Config returns the configuration this client resolved, with defaults filled
// in. It is a copy.
func (c *Client) Config() Config { return c.cfg }

// Store returns the credential store this client reads and writes.
func (c *Client) Store() *Store { return c.store }

// now is the clock, overridable in tests.
func (c *Client) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// credential resolves the identity this client acts as, caching the result.
//
// The selection order is: Config.AgentID (from --as / AGENT_BUS_AGENT_ID),
// else whatever `agent-busctl use` last selected. It never falls back to "the only
// identity in the store" when a selection exists but dangles — guessing which
// identity to act as is how a message ends up sent from the wrong agent.
func (c *Client) credential() (Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cred != nil {
		return *c.cred, nil
	}
	cred, err := c.store.Resolve(c.cfg.AgentID)
	if err != nil {
		return Credential{}, err
	}
	c.cred = &cred
	return cred, nil
}

// Identity returns the PUBLIC half of the identity this client acts as.
func (c *Client) Identity() (Identity, error) {
	cred, err := c.credential()
	if err != nil {
		return Identity{}, err
	}
	return cred.Identity, nil
}

// Identities lists every stored identity and the currently selected agent id.
func (c *Client) Identities() ([]Identity, string, error) {
	return c.store.List()
}

// Use selects ref as the identity subsequent commands act as, and returns it.
//
// It mutates SHARED state — the store's "current" pointer — so an agent
// running in parallel with others against the same credential store should
// prefer --as / AGENT_BUS_AGENT_ID, which selects without mutating anything.
func (c *Client) Use(ref string) (Identity, error) {
	id, err := c.store.SetCurrent(ref)
	if err != nil {
		return Identity{}, err
	}
	// forgetIdentity rather than clearing two fields by hand: the new identity
	// may be on a different bus with a different pinned certificate, and the
	// cached transport has to go with it.
	c.forgetIdentity()
	return id, nil
}

// LogoutResult reports what a logout removed. Its json tags are a documented
// contract surface (CONTRACTS-CLI.md).
type LogoutResult struct {
	// Removed lists the fully-qualified agent ids whose credentials were
	// deleted from the local store.
	Removed []string `json:"removed"`

	// Current is the agent id selected afterwards, or "" when none remains.
	//
	// Tagged current_agent_id, matching `whoami --all`: `whoami` and `use`
	// carry a BOOLEAN is_current, and one key that is a bool in one subcommand
	// and a string in another makes `jq .current` unpredictable across the CLI.
	Current string `json:"current_agent_id"`

	// ServerNotified reports whether the BUS was told. It is ALWAYS false
	// today: /v1/leave does not exist yet, so logout is a purely local
	// operation and the enrolment remains on the bus.
	//
	// The field exists now, rather than being added when /v1/leave lands,
	// precisely so a consumer written today cannot mistake local deletion for
	// revocation. It flips to true only when the bus has actually been told.
	ServerNotified bool `json:"server_notified"`
}

// Logout deletes ref's credential from the LOCAL store.
//
// It does NOT revoke anything. The bus has no /v1/leave route yet, so the
// enrolment and any live session survive this call; see
// LogoutResult.ServerNotified.
func (c *Client) Logout(ref string) (LogoutResult, error) {
	removed, current, err := c.store.Remove(ref)
	if err != nil {
		return LogoutResult{}, err
	}
	c.forgetIdentity()
	return LogoutResult{Removed: removed, Current: current, ServerNotified: false}, nil
}

// LogoutAll deletes every credential from the LOCAL store. See Logout for what
// it does not do.
func (c *Client) LogoutAll() (LogoutResult, error) {
	removed, err := c.store.RemoveAll()
	if err != nil {
		return LogoutResult{}, err
	}
	if removed == nil {
		removed = []string{}
	}
	c.forgetIdentity()
	return LogoutResult{Removed: removed, Current: "", ServerNotified: false}, nil
}

// forgetIdentity drops the cached credential, session AND transport after the
// store has changed underneath them.
//
// The transport goes too because a different identity may be enrolled with a
// different bus, and therefore a different pinned certificate. Keeping it would
// leave pooled connections that were verified against the previous identity's
// pin — the connection would be reused, the new pin would never be checked, and
// nothing would look wrong.
func (c *Client) forgetIdentity() {
	c.mu.Lock()
	c.cred = nil
	c.session = nil
	closeIdleConnections(c.http)
	c.http = nil
	c.httpPin = BusFingerprint{}
	c.mu.Unlock()
}

// contextWithTimeout applies Config.Timeout when ctx has no deadline of its
// own, so every operation is bounded without overriding a caller who set a
// tighter or deliberately longer one.
func (c *Client) contextWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.cfg.Timeout)
}
