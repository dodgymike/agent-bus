package client

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"net/url"
	"path/filepath"
	"strconv"
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
	//
	// It is deliberately ONE fingerprint, not a set, even though the stored
	// identity now holds a set. A flag is a per-invocation assertion about which
	// certificate is on the other end, and it NARROWS — a caller who names one
	// certificate is saying "this one", not "these as well". Widening the
	// accept-set is an operator act on the STORE (Store.AddBusPin), so that it
	// is deliberate, durable and auditable rather than a shell-history artefact.
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
	// neither is known until the first request resolves them. httpPins is the
	// accept-set http was built for; when the resolved set differs — the caller
	// switched identity, supplied a fingerprint explicitly, or added/retired a
	// pin — the transport is rebuilt rather than reused, because a pooled
	// connection was verified against the OLD set.
	http     HTTPDoer
	httpPins BusPinSet

	// warnMu guards warnings. Separate from mu because a warning is appended
	// while mu is NOT held (during the certificate load) and drained by the CLI
	// between commands; folding it into mu would put a disk read and a stderr
	// drain on the same lock every request contends on.
	warnMu   sync.Mutex
	warnings []string

	// clientCert caches this agent's OWN TLS material — the half of mutual TLS
	// this end presents — so a long-lived client reads and parses it once
	// rather than on every transport rebuild. Nil until first use; see
	// clientCertificate, which is the only writer and which never holds mu
	// across the disk read.
	clientCert *ClientCertificate

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
func (c *Client) endpoint() (*url.URL, BusPinSet, error) {
	u, err := c.resolveBusURL()
	if err != nil {
		return nil, BusPinSet{}, err
	}
	pins, err := c.resolvePins(u)
	if err != nil {
		return nil, BusPinSet{}, err
	}
	return u, pins, nil
}

// endpointWith is endpoint() for a bus named by an INVITE rather than by
// configuration.
//
// It takes both halves as arguments because with an invite neither can be
// resolved from the usual places: there is no stored identity for a bus this
// client has never joined, and --bus / --bus-fingerprint are unnecessary (the
// invite carries both). The pin still goes through resolvePinsWith, so the
// narrowing and disagreement rules that guard a stored accept-set apply exactly
// as they do on the ordinary path — enrolling a SECOND agent into a store that
// already pins this bus must not quietly widen it.
//
// src says which of the two the pin actually came from, and is passed through
// rather than assumed: inviteEndpoint falls back to the FLAG's fingerprint when
// the invite carries none, and a refusal that named the wrong source would send
// the operator to fix a value they never set.
//
// endpoint() is deliberately left alone rather than generalised: it is the
// no-invite path, and its behaviour must be byte-identical to what it was.
func (c *Client) endpointWith(u *url.URL, pin BusFingerprint, src pinSource) (*url.URL, BusPinSet, error) {
	pins, err := c.resolvePinsWith(u, pin, src)
	if err != nil {
		return nil, BusPinSet{}, err
	}
	return u, pins, nil
}

// pinSource says WHERE the asserted fingerprint reaching resolvePinsWith came
// from. It changes the REFUSAL TEXT only: the refusal itself, and everything
// that decides it, is identical on both paths.
//
// # Why the text has to differ, which is not a cosmetic point
//
// The flag remedy offers `agent-busctl pin add <fingerprint>` — correct there,
// because the fingerprint on the command line was typed by the operator, and a
// rollover is the ordinary reason it disagrees with the stored set.
//
// On the INVITE path both halves of that are false. The caller passed no flag,
// so naming one is advice they cannot act on (invariant 7: an error names a
// remedy that works). And under invariant 11's own threat model — "whoever can
// substitute an invite can point an agent at a bus of their choosing" — the
// disagreeing fingerprint is exactly the value an attacker chose, so a
// copy-pasteable `pin add` with it already filled in turns a correct refusal
// into a one-command defeat of the pin. The invite path therefore names the
// INVITE as the source and offers no command that accepts its fingerprint: the
// only safe next step is confirming out of band which certificate is genuine.
type pinSource struct {
	// invite is set when the fingerprint came from an invite blob rather than
	// from --bus-fingerprint / AGENT_BUS_FINGERPRINT.
	invite bool

	// inviteID names the invite in the message, and is empty on the flag path.
	// It has been through Invite.Validate — bounded by MaxInviteIDLen and
	// matched against serverIDPattern — before it can reach here, so it cannot
	// carry a control character or fill a scrollback.
	inviteID string
}

// pinFromFlag is the ordinary source: --bus-fingerprint / AGENT_BUS_FINGERPRINT,
// or nothing at all.
var pinFromFlag = pinSource{}

// pinFromInvite is the invite path, carrying the invite's id for the message.
func pinFromInvite(inviteID string) pinSource { return pinSource{invite: true, inviteID: inviteID} }

// resolvePins returns the SET of certificates the bus at u may present.
//
// The order matches every other setting (CONTRACTS-CLI.md): the explicit
// --bus-fingerprint / AGENT_BUS_FINGERPRINT value, else the set recorded on the
// selected identity. With two additions that are specific to a security pin:
//
//   - The STORED set is used only for the bus it was stored for. It is
//     recorded next to that identity's BusURL, and applying it to a different
//     --bus would be pinning bus A's certificate on bus B — which fails every
//     connection and reads as a broken client rather than as a mistake.
//   - An explicit fingerprint NARROWS the stored set to that one certificate;
//     it never widens it. Passing --bus-fingerprint <one of the two we accept>
//     mid-rollover is a legitimate way to insist on one side of the rollover,
//     and it is strictly stronger than the stored set, so it is allowed. A
//     fingerprint that is NOT in the stored set is a hard refusal, not a
//     precedence question: one of the two is wrong about which bus this is, and
//     silently preferring the value that arrived on the command line is exactly
//     how an operator is talked into re-pinning ("it did not work, so I passed
//     the fingerprint the other end gave me"). Refusing makes the disagreement
//     visible, which is the only way it gets checked out of band. Widening is
//     available — deliberately, durably and only through Store.AddBusPin.
//
// A missing or unreadable identity is NOT an error here — enrolment happens
// before any identity exists, and that is the case that most needs a pin.
func (c *Client) resolvePins(u *url.URL) (BusPinSet, error) {
	return c.resolvePinsWith(u, c.pin, pinFromFlag)
}

// resolvePinsWith is resolvePins with the ASSERTED fingerprint supplied by the
// caller instead of taken from Config.BusFingerprint.
//
// It exists for exactly one caller: enrolment with an invite, where the trust
// anchor is the INVITE's fingerprint rather than a flag (invariant 11 — the
// invite carries the fingerprint so the first connection is verifiable, and
// there is no trust-on-first-use). Everything below it is unchanged, which is
// the point: the invite's pin is subject to the same narrowing rule and the same
// hard refusal on disagreement with a stored set, rather than a second, weaker
// code path that happens to be reached only when an invite is present.
//
// The REFUSAL is the same on both paths; only the text differs, and src decides
// which. See pinSource for why a shared message was wrong rather than untidy.
// Enrol refuses a flag/invite disagreement BEFORE calling this, so an invite's
// fingerprint can only reach here alone.
func (c *Client) resolvePinsWith(u *url.URL, pin BusFingerprint, src pinSource) (BusPinSet, error) {
	stored, err := c.storedPins(u)
	if err != nil {
		return BusPinSet{}, err
	}
	switch {
	case pin.IsZero():
		return stored, nil
	case stored.IsEmpty(), stored.Contains(pin):
		return NewBusPinSet(pin), nil
	case src.invite:
		// The INVITE disagrees with a pin this store already holds for this bus.
		//
		// No `pin add` here, and no other command that would accept the invite's
		// fingerprint: see pinSource. The invite is the trust anchor, so a value
		// arriving inside one cannot be checked against anything else in the
		// invite — only out of band. The bus address is the invite's, so it goes
		// through safeText like every other invite-derived value that is printed.
		return BusPinSet{}, newError(KindUsage, "config",
			"invite "+src.inviteID+" names certificate "+pin.String()+" but the stored identity for "+safeText(u.String(), maxDetailBytes)+" accepts "+stored.String(),
			"these name different certificates and one of them is wrong, and the disagreement itself is the finding. The invite is the trust anchor (invariant 11): whoever can substitute one points this agent at a bus of their choosing, so an invite that disagrees with a pin you ALREADY hold for this bus is SUSPECT until it has been confirmed. "+
				"Confirm OUT OF BAND — with the operator, not through the channel the invite arrived on — which certificate the bus is really serving; it logs `bus_cert_fingerprint=…` at startup. "+
				"Do not widen or replace the stored pin to make this enrolment succeed: if the bus genuinely rotated, act on the fingerprint you confirmed rather than on the one the invite asserted")
	default:
		remedy := "these name different certificates and one of them is wrong. Confirm which is genuine OUT OF BAND (the bus logs `bus_cert_fingerprint=…` at startup) before doing anything else; " +
			"then either drop the flag, or — if the bus is rotating and the new certificate is genuine — `agent-busctl pin add " + pin.String() + "` to accept it alongside the old one for the rollover. " +
			"To replace the pin outright, `agent-busctl logout` that identity and enrol again against the fingerprint you confirmed"
		return BusPinSet{}, newError(KindUsage, "config",
			"--bus-fingerprint says "+pin.String()+" but the stored identity for "+u.String()+" accepts "+stored.String(),
			remedy)
	}
}

// storedPins returns the accept-set recorded on the selected identity, but only
// when that identity was enrolled with the bus at u.
func (c *Client) storedPins(u *url.URL) (BusPinSet, error) {
	cred, err := c.credential()
	if err != nil {
		// No identity, or none selected. Not a pin problem: the caller is
		// enrolling, and the operations that need an identity raise this
		// themselves with a better message.
		return BusPinSet{}, nil
	}
	if len(cred.BusFingerprints) == 0 || cred.BusURL != u.String() {
		return BusPinSet{}, nil
	}
	pins, err := ParseBusPinSet(cred.BusFingerprints)
	if err != nil {
		// Stored, and unreadable. Fail rather than fall back to "no pin": that
		// fallback would turn a damaged credential store into an unpinned
		// connection, which is the one outcome that must never happen quietly.
		// The remedy deliberately does NOT offer --bus-fingerprint. Both gates
		// caught the earlier wording, which named it: resolvePins calls this
		// function FIRST and returns its error before c.pin is ever consulted,
		// so the flag could not have helped — and a remedy that does not work
		// sends the operator looking for the flag that turns the check off (see
		// pinError). Nor should the flag be made to override an unreadable
		// store: that is precisely how a damaged store becomes an unpinned
		// connection.
		return BusPinSet{}, newError(KindConfig, "config",
			"the certificate fingerprints stored for identity "+cred.AgentID+" are not all valid fingerprints",
			"the credential store is damaged. Inspect it with `agent-busctl pin list`; if a readable pin remains, `agent-busctl pin remove <the unreadable one>` — otherwise `agent-busctl logout "+cred.AgentID+"` and enrol again with the fingerprint from the invite")
	}
	if pins.Len() > MaxBusPins {
		// Only reachable through a hand-edited store: AddBusPin refuses past the
		// cap. Refused HERE, at connection time, rather than at load, so that
		// `agent-busctl pin list` and `pin remove` — the two commands that fix
		// it — still work. An accept-set that has grown without bound is the
		// "accept every certificate this bus ever had" failure the cap exists to
		// prevent, so it must not be quietly honoured.
		return BusPinSet{}, newError(KindConfig, "config",
			"identity "+cred.AgentID+" accepts "+strconv.Itoa(pins.Len())+" bus certificates, more than the maximum of "+strconv.Itoa(MaxBusPins),
			"a rollover needs two at most, so a larger set means the store was edited by hand. Inspect it with `agent-busctl pin list` and retire the certificates the bus no longer serves with `agent-busctl pin remove <fingerprint>`")
	}
	return pins, nil
}

// doer returns the transport for one bus, building it on first use and reusing
// it while the pin is unchanged.
//
// The security decision (transportSecurity) is made BEFORE the cache is
// consulted, so a refusal cannot be skipped by an earlier call having succeeded.
func (c *Client) doer(u *url.URL, pins BusPinSet) (HTTPDoer, error) {
	if c.cfg.HTTPClient != nil {
		// The embedder's own transport. Documented on Config.HTTPClient as
		// bypassing this package's TLS configuration entirely — including the
		// pin — because an embedder that supplies a transport owns its
		// verification. It is not a way to relax anything: nothing in this
		// package or the CLI ever sets it.
		return c.cfg.HTTPClient, nil
	}
	if err := transportSecurity(u, pins); err != nil {
		return nil, err
	}

	// This end's OWN certificate — mutual TLS's other half (invariant 11).
	//
	// Loaded only on the pinned path, because that is the only path with a
	// handshake: an empty pin set means a plaintext loopback URL, where there
	// is nothing to present and where requiring a writable credential store to
	// make a request would be a new failure for no gain.
	//
	// It is resolved BEFORE c.mu is taken. clientCertificate reads the disk,
	// and doing that under the mutex every other call contends on would
	// serialise the client behind a filesystem — and worse, the two would be
	// one lock ordering to get wrong later.
	//
	// A failure here STOPS the request. It does not fall back to connecting
	// without a certificate: today that would appear to work, because the bus
	// does not ask for one, and would become a lockout the day it does — a
	// failure that arrives months after the change that caused it.
	var clientCert *tls.Certificate
	if !pins.IsEmpty() {
		cc, err := c.clientCertificate()
		if err != nil {
			return nil, err
		}
		clientCert = cc.certificate()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http == nil || !c.httpPins.Equal(pins) {
		closeIdleConnections(c.http)
		c.http = newHTTPClient(pins, clientCert)
		c.httpPins = pins
	}
	return c.http, nil
}

// ClientCertificate returns this agent's own TLS material, minting it on first
// use.
//
// Exported for `agent-busctl client-cert`, which is how an operator finds the
// files and reads the fingerprint the bus will bind. It is idempotent: calling
// it never replaces material already on disk.
func (c *Client) ClientCertificate() (*ClientCertificate, error) { return c.clientCertificate() }

// clientCertificate loads-or-mints this agent's TLS material once per Client.
//
// The disk read happens OUTSIDE the lock, so two goroutines racing on a fresh
// store may both call LoadOrCreateClientCertificate. That is safe by
// construction rather than by luck — creation installs the material with a
// single directory rename, so one of them wins and the other loads what the
// winner wrote (see LoadOrCreateClientCertificate). The second-check-under-lock
// then makes sure every caller is handed the SAME *ClientCertificate, so the
// certificate a transport was built with cannot differ from the one another
// goroutine is reporting.
func (c *Client) clientCertificate() (*ClientCertificate, error) {
	c.mu.Lock()
	cached := c.clientCert
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	cc, err := LoadOrCreateClientCertificate(c.cfg.IdentityDir)
	if err != nil {
		return nil, err
	}

	// Anything the load had to REPAIR is recorded for the caller to surface.
	//
	// This is not decoration. tightenPermissions chmods a world-readable
	// private key back to 0600 and records why, and this file argues at length
	// that tightening SILENTLY is the actual defect — the operator ends up
	// believing a key was private when another local user may already have read
	// it. Before this, cc.Warnings was read only by `agent-busctl client-cert`,
	// a command whose own help says you rarely need to run it, so on every
	// ordinary command the warning was collected and dropped on the floor. The
	// security gate was right to call that a P1: the detection existed and the
	// telling did not.
	if len(cc.Warnings) > 0 {
		c.warnMu.Lock()
		c.warnings = append(c.warnings, cc.Warnings...)
		c.warnMu.Unlock()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clientCert == nil {
		c.clientCert = cc
	}
	return c.clientCert, nil
}

// Warnings returns and CLEARS conditions the operator should be told about
// which did not stop the client — today, key material found with permissions
// that had to be tightened.
//
// It drains rather than accumulates so a long-running command (`watch`) can
// poll it without reprinting the same line forever. Store.Warnings is the
// sibling for the credential store; the two are separate because they are
// populated at different moments — the store's at New, these on the first
// pinned request.
func (c *Client) Warnings() []string {
	c.warnMu.Lock()
	defer c.warnMu.Unlock()
	if len(c.warnings) == 0 {
		return nil
	}
	out := c.warnings
	c.warnings = nil
	return out
}

// closeIdleConnections drops the sockets of a transport that has been
// superseded, instead of leaving them to IdleConnTimeout's 90 seconds.
//
// This is hygiene, NOT the security control — a transport built for accept-set
// A structurally cannot accept a certificate outside A, so a lingering socket
// could never carry a wrongly-verified request. What it avoids is a
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
// to verify anything this agent sends. Since RELAY-13 this key IS registered at
// enrolment (Enrol sends it as messaging_public_key and the bus stores it on the
// roster entry), but there is still no route that PUBLISHES it: no endpoint
// serves another agent's messaging key, and CRYPTO-4 (the server-attested key
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
	c.httpPins = BusPinSet{}
	c.mu.Unlock()
}

// AddBusPin records fingerprint as a certificate identity ref will ALSO accept,
// and returns the updated identity.
//
// # This is the operator act that makes a rollover survivable
//
// DECISIONS.md E3: a rotating bus serves TWO certificates during rollover so
// clients can re-pin without downtime, and "rotation must never require every
// client to re-enrol — that would make routine key hygiene indistinguishable
// from a security incident". This is how a client is told about the incoming
// certificate BEFORE it appears, or told to accept it after a refusal, without
// dropping the outgoing one and without a re-enrolment.
//
// # fingerprint MUST have been confirmed out of band
//
// It is an argument, from the operator, and it is never the value the bus just
// presented. Nothing in this package derives a fingerprint from a live
// certificate and stores it — that is trust-on-first-use, which invariant 11
// abolished, and TestPinIsNeverLearnedFromAHandshake is its standing guard. The
// mismatch error prints the presented value precisely so a HUMAN can compare it
// against the bus's `bus_cert_fingerprint=…` startup log before running this.
//
// It lives on Client, not only in the CLI, because cmd/agent-busctl is a THIN
// shell (invariant 7): an agent that embeds this package must be able to survive
// a rotation too.
func (c *Client) AddBusPin(ref, fingerprint string) (Identity, error) {
	pin, err := ParseBusFingerprint(strings.TrimSpace(fingerprint))
	if err != nil {
		return Identity{}, err
	}
	id, err := c.store.AddBusPin(ref, pin)
	if err != nil {
		return Identity{}, err
	}
	// The cached credential AND the cached transport are both stale now: the
	// transport was built to accept the OLD set, and reusing it would leave the
	// newly-accepted certificate refused until the process restarted.
	c.forgetIdentity()
	return id, nil
}

// RemoveBusPin retires fingerprint from identity ref's accept-set and returns
// the updated identity.
//
// Retiring is as deliberate as adding, and for the same reason: a set that only
// ever grows becomes "accept every certificate this bus has ever had", so a
// certificate compromised two rotations ago would still be honoured. It is the
// SECOND half of a rollover and skipping it leaves the fleet permanently wider
// than it needs to be.
//
// Removing the last remaining pin is REFUSED (Store.RemoveBusPin). An identity
// on an https bus with an empty set cannot connect at all, so the operation
// would look like a tidy-up and land as a lockout.
func (c *Client) RemoveBusPin(ref, fingerprint string) (Identity, error) {
	pin, err := ParseBusFingerprint(strings.TrimSpace(fingerprint))
	if err != nil {
		return Identity{}, err
	}
	id, err := c.store.RemoveBusPin(ref, pin)
	if err != nil {
		return Identity{}, err
	}
	c.forgetIdentity()
	return id, nil
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
