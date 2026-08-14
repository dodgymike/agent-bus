package client

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// AgentNamePattern is the shape of a requested agent name, PINNED here to
// match internal/ids.AgentNamePattern.
//
// It is duplicated rather than imported because the client package must not
// depend on internal/ (invariant 7: an agent has to be able to EMBED this
// package, and Go forbids importing internal/ from another module). The server
// remains authoritative: this check exists only so a typo produces a remedial
// message locally instead of a round trip and a terse 400.
//
// Lowercase only, and uppercase is REJECTED rather than folded — see
// internal/ids/agentid.go for why folding would eventually re-mint a live id.
const AgentNamePattern = `^[a-z0-9][a-z0-9_-]{0,63}$`

var agentNameRegexp = regexp.MustCompile(AgentNamePattern)

// Route paths on the bus. Pinned here for the same reason as
// AgentNamePattern.
const (
	routeEnroll          = "/v1/enroll"
	routeSessionBegin    = "/v1/session/begin"
	routeSessionComplete = "/v1/session/complete"
)

// EnrolOptions is the input to Enrol.
type EnrolOptions struct {
	// Name is the short, human-chosen name being requested. The SERVER decides
	// the actual id (invariant 1); this is only half of it, and two agents may
	// ask for the same one.
	Name string

	// Invite is the operator-minted invite blob being redeemed, or nil.
	//
	// It is the TRUST ANCHOR of this enrolment (invariant 3: enrolment is
	// invite-only; invariant 11: the invite carries the bus's certificate
	// fingerprint so the FIRST connection is verifiable and there is no
	// trust-on-first-use). When it is set it supplies BOTH the bus address and
	// the pin, so Config.BusURL and Config.BusFingerprint are unnecessary — and
	// one that DISAGREES with the invite is refused rather than silently
	// preferred, because one of the two is wrong about which bus this is.
	//
	// Load it with LoadInviteFile or ParseInvite; Enrol validates it either way,
	// so an embedder that built one by hand cannot skip the checks. The secret
	// inside it goes in the request body and NOWHERE else: not on argv, not in
	// EnrolResult, not in an error, not in a log line.
	//
	// nil is still accepted, because the bus still accepts an un-invited
	// enrolment (httpapi.DiscoveryEnrolment.InviteRequired is false today).
	// Invariant 3 says that must change; when it does, the bus refuses, not this
	// client.
	Invite *Invite

	// IdempotencyKey makes the enrolment safe to retry (invariant 10). Leave
	// it empty and Enrol mints a fresh random one.
	//
	// Supply one only to RETRY a specific earlier attempt. Enrol then reuses
	// the key material that attempt generated — which is what makes the retry
	// legitimate, since the payload must be byte-identical — and refuses
	// locally if the key was used for different content.
	IdempotencyKey string

	// Save stores the resulting credential in the credential store. Callers
	// that only want to test enrolment can turn it off — but note that BOTH
	// private keys are then lost when the process exits, and the enrolment they
	// created stays on the bus, holding a messaging public key nobody can ever
	// sign for.
	Save bool

	// MakeCurrent selects the new identity as the one subsequent commands act
	// as. Ignored when Save is false.
	MakeCurrent bool
}

// EnrolResult is what Enrol returns. Its json tags are a documented contract
// surface (CONTRACTS-CLI.md); Identity is embedded, so agent_id, bus_id, name,
// bus_url, public_key and enrolled_at appear at the top level.
type EnrolResult struct {
	Identity

	// Replayed reports that the bus answered from its idempotency table rather
	// than enrolling again — i.e. this was a retry and the original result is
	// what came back. Not an error.
	Replayed bool `json:"replayed"`

	// IdempotencyKey is the key this enrolment was applied under.
	//
	// It is REPORTED, not just used, because it is the only handle that can
	// resume the attempt. An enrolment killed before it printed anything
	// leaves its key material on disk reachable by this key alone; emitting it
	// on success, and naming it in the failure remedy, is what keeps the
	// stored seed recoverable rather than merely stored.
	IdempotencyKey string `json:"idempotency_key"`

	// Stored reports whether the credential was written to the local store.
	Stored bool `json:"stored"`

	// StorePath is where it was written, when Stored.
	StorePath string `json:"store_path,omitempty"`

	// InviteID is the invite this enrolment redeemed, or "" when none was
	// presented.
	//
	// The ID is a NAME, not a credential — cmd/agent-bus/invite.go says so
	// explicitly, and the SECRET is the credential — so reporting it is
	// provenance an operator needs: it answers "which single-use invite did this
	// agent spend" without a log dive. The secret is NEVER reported, here or
	// anywhere else.
	InviteID string `json:"invite_id,omitempty"`
}

// enrolRequestBody mirrors httpapi.EnrolRequestBody. The server rejects
// unknown fields, so this struct is exactly the wire shape and nothing more.
type enrolRequestBody struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`

	// MessagingPublicKey is the base64 (standard, padded) public half of this
	// identity's MESSAGING key — the one PEERS verify signed messages with, and
	// the one this bus later ATTESTS to a peer bus. It is a DIFFERENT key from
	// PublicKey and the server refuses an enrolment that presents one value for
	// both roles.
	//
	// Registering it HERE is what makes cross-bus trust possible at all: the
	// attestation binds `fq-agent-id -> messaging public key`, and a bus cannot
	// attest a key it never recorded.
	//
	// The PRIVATE half is not sent, is not derivable from this, and never leaves
	// the machine (Credential.MessagingKeySeed).
	//
	// omitempty, and the server accepts an absent value. That is not
	// forward-compatibility theatre — it is load-bearing on ONE path: resuming an
	// enrolment whose pending record was written before this field existed, which
	// must resend that attempt's payload byte for byte or be answered with a 409
	// (invariant 10). Every fresh enrolment sends one.
	//
	// Note what it does NOT prove. There is no proof-of-possession of the
	// messaging private key at enrolment: an enroller can register a public key
	// it harvested from somewhere else. That is a known, accepted gap — it is
	// NOT covered by AUTH-1-FU-POPKEY, which is about the auth key — so nothing
	// downstream may treat "the bus recorded this key" as "this agent holds it".
	MessagingPublicKey string `json:"messaging_public_key,omitempty"`

	IdempotencyKey string `json:"idempotency_key"`

	// InviteID and InviteSecret redeem an operator-minted invite
	// (httpapi.EnrolRequestBody). They are presented TOGETHER or not at all: the
	// bus answers half an invite with a 400, deliberately, so a client with a bug
	// cannot walk away believing its single-use credential was spent.
	//
	// BOTH carry omitempty, and that is load-bearing rather than tidiness. The
	// no-invite path must send a body BYTE-IDENTICAL to the one it sent before
	// these fields existed, or an in-flight enrolment being retried after an
	// upgrade becomes "same key, DIFFERENT payload" — a 409 the retry can never
	// escape (invariant 10). The server's idempotency fingerprint covers
	// invite_id but NOT the secret, so a resumed attempt must present the same
	// invite; it may not present a different one.
	InviteID string `json:"invite_id,omitempty"`

	// InviteSecret is a BEARER CREDENTIAL. It appears in this struct, on the
	// wire, and nowhere else in this package: not in EnrolResult, not in an
	// *Error, not in a pending record on disk, and not in Invite.String.
	InviteSecret string `json:"invite_secret,omitempty"`
}

// enrolResponseBody mirrors httpapi.EnrolResponseBody.
type enrolResponseBody struct {
	AgentID    string `json:"agent_id"`
	BusID      string `json:"bus_id"`
	Name       string `json:"name"`
	EnrolledAt string `json:"enrolled_at"`
}

// idempotencyReplayedHeader mirrors httpapi.IdempotencyReplayedHeader: the
// body of a replay is byte-identical to the original by design, so the fact
// that it WAS a replay is carried out of band.
const idempotencyReplayedHeader = "Idempotency-Replayed"

// Enrol registers a new agent with the bus and stores the credential.
//
// TWO key pairs are generated HERE and only their public halves are sent, so
// the bus can verify this agent and can forge neither of them:
//
//   - the AUTH key, which proves this agent TO THE BUS in the session
//     handshake;
//   - the MESSAGING key, which proves this agent TO ITS PEERS. Its public half
//     is registered at enrolment (RELAY-13) because the origin bus attests
//     `fq-agent-id -> messaging public key` to a peer bus, and a bus cannot
//     attest a key it never recorded.
//
// Registering it is not a claim that this agent HOLDS the matching private
// key: enrolment obtains no proof of possession of the messaging key, so a
// caller could register one it harvested. That gap is known and accepted here.
//
// # Ordering: both keys are on disk BEFORE the request goes out
//
// The seeds are written to the credential store as a PENDING enrolment before
// /v1/enroll is called, and promoted to a full credential when the bus
// answers. Two things fall out of that ordering, and both are the point:
//
//   - A process killed after the bus minted an id but before the answer was
//     stored does not lose either private key. Retrying with the SAME
//     idempotency key recovers the identity, because the same key material is
//     still there — and, for the messaging key, because the bus has recorded a
//     public half whose private half must not be regenerated underneath it.
//   - A retry is a real retry. Invariant 10 defines one as "same key + SAME
//     payload"; the payload contains BOTH public keys, so a client that
//     regenerated either key pair would be sending "same key + DIFFERENT
//     payload" — which the bus answers with a 409. Reusing the pending record
//     is what makes the retry legitimate rather than an offence.
//
// When the same idempotency key is presented with different content — a
// different name, or a different invite — Enrol refuses LOCALLY. It could send
// it and let the bus refuse, but the bus's answer is a 409 that teaches the
// caller nothing this refusal does not, and the round trip spends a redemption
// attempt against a single-use invite. The bus does NOT disconnect for this
// (invariant 10, narrowed 2026-08-08); the reason to refuse here is that the
// mistake is visible from here and the local refusal drops nothing.
func (c *Client) Enrol(ctx context.Context, opts EnrolOptions) (EnrolResult, error) {
	const op = "enrol"

	name := strings.TrimSpace(opts.Name)
	if err := validateAgentName(op, name); err != nil {
		return EnrolResult{}, err
	}

	// Resolve the bus URL AND its pinned fingerprint before anything is
	// written, so a malformed --bus or an https bus with no pin does not leave
	// a pending record behind.
	//
	// endpoint rather than resolveBusURL: enrolment is the FIRST connection to
	// this bus and therefore the one that must not be trust-on-first-use. The
	// pin has to come from the invite, before the handshake, and refusing here
	// is what makes that true (invariant 11). Nothing is stored yet, so the pin
	// can only be the invite's or the explicit one.
	var (
		busURL *url.URL
		pins   BusPinSet
		err    error
	)
	if opts.Invite != nil {
		if busURL, pins, err = c.inviteEndpoint(op, opts.Invite); err != nil {
			return EnrolResult{}, err
		}
	} else {
		// Enrolment needs an EXPLICIT bus, and says so here rather than in the
		// CLI.
		//
		// Every other operation may fall back to the selected identity's
		// recorded URL, but enrolment by definition has no identity on the bus
		// it is joining, so that fallback is meaningless — and letting it happen
		// would surface as KindConfig ("no identity has been enrolled", exit 3)
		// when the caller's actual mistake was a missing --bus (KindUsage, exit
		// 2). Putting the check in the package means an EMBEDDING caller gets
		// the same classification the CLI does.
		if strings.TrimSpace(c.cfg.BusURL) == "" {
			return EnrolResult{}, newError(KindUsage, op,
				"no bus URL",
				"enrolment needs an explicit bus: pass --bus <url> or set "+EnvBusURL+", or redeem an invite with --invite-file <path>, which carries the address and the certificate fingerprint")
		}
		if busURL, pins, err = c.endpoint(); err != nil {
			return EnrolResult{}, err
		}
	}

	idemKey := strings.TrimSpace(opts.IdempotencyKey)
	if idemKey == "" {
		if idemKey, err = newIdempotencyKey(); err != nil {
			return EnrolResult{}, err
		}
	} else if err := validateIdempotencyKey(op, idemKey); err != nil {
		return EnrolResult{}, err
	}

	// TWO fresh key pairs for a new attempt — the AUTH key, which proves this
	// agent to the bus, and the MESSAGING key, whose public half the bus records
	// so it can later attest it to a peer bus. They must never be the same key
	// (the server refuses that outright), which is why these are two independent
	// draws and not one seed used twice.
	//
	// ClaimEnrolment may hand back an earlier attempt's material instead, in
	// which case both of these are discarded unused — cheap, and it keeps the
	// random draws outside the lock.
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return EnrolResult{}, wrapError(KindInternal, op,
			"cannot generate a key pair: the system random source failed", "", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))

	msgSeed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(msgSeed); err != nil {
		return EnrolResult{}, wrapError(KindInternal, op,
			"cannot generate a messaging key pair: the system random source failed", "", err)
	}

	// The invite's two values, read out ONCE.
	//
	// The ID is read here, BEFORE the pending record is written, because it is
	// part of what this attempt asserts and the record has to carry it — that
	// is what lets ClaimEnrolment refuse a resume that presents a different
	// invite, locally, before anything is sent (INVITE-CLIENT-FU-PENDINGINVITE).
	// opts.Invite has already been validated by inviteEndpoint above.
	//
	// The SECRET goes in the request body and NOWHERE else: not in the pending
	// record, not on argv, not in EnrolResult, not in an error, not in a log
	// line.
	inviteID, inviteSecret := "", ""
	if opts.Invite != nil {
		inviteID, inviteSecret = opts.Invite.InviteID, opts.Invite.InviteSecret
	}

	want := pendingEnrolment{
		IdempotencyKey:   idemKey,
		Name:             name,
		BusURL:           busURL.String(),
		PublicKey:        pubB64,
		PrivateKeySeed:   base64.StdEncoding.EncodeToString(seed),
		MessagingKeySeed: base64.StdEncoding.EncodeToString(msgSeed),
		InviteID:         inviteID,
		CreatedAt:        c.now().UTC().Format(time.RFC3339Nano),
	}

	// resumed reports that the key material below belongs to an EARLIER attempt
	// — one the bus may already have applied — rather than to the draws made a
	// few lines above. It decides whether a refusal may drop that material; see
	// enrolFailed.
	resumed := false

	effective := want
	if opts.Save {
		// Durability before the acknowledgement — the client side of the
		// principle invariant 4 states for the server — and the decision about
		// what this attempt IS, both under one lock.
		claim, cerr := c.store.ClaimEnrolment(want, c.now())
		if cerr != nil {
			return EnrolResult{}, cerr
		}
		if claim.Applied != nil {
			// Already enrolled under this key. Answer from the store and send
			// NOTHING: re-running a completed enrolment is the obvious human
			// action, and going back to the bus with a fresh key pair under
			// the old key is exactly the violation it rejects and logs.
			return EnrolResult{
				Identity:       claim.Applied.Identity,
				Replayed:       true,
				Stored:         true,
				StorePath:      c.store.Path(),
				IdempotencyKey: idemKey,
			}, nil
		}
		effective = claim.Pending
		resumed = claim.Resumed
	}

	seed, err = base64.StdEncoding.Strict().DecodeString(effective.PrivateKeySeed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return EnrolResult{}, newError(KindConfig, op,
			"the saved key material for idempotency key "+idemKey+" is damaged",
			"enrol again without --idempotency-key to generate a fresh key pair")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pubB64 = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	seedB64 := effective.PrivateKeySeed

	// The stored public key is recomputed from the seed and CHECKED rather
	// than trusted. They can only disagree if the record was edited or
	// corrupted, and continuing would send a public key the bus records
	// against a private key we do not hold — an identity that enrols
	// successfully and can never authenticate.
	if effective.PublicKey != "" && effective.PublicKey != pubB64 {
		return EnrolResult{}, newError(KindConfig, op,
			"the saved key material for idempotency key "+idemKey+" is inconsistent: the stored public key does not match the stored private key",
			"enrol again without --idempotency-key to generate a fresh key pair")
	}

	// The messaging public key is DERIVED from the seed this attempt is
	// committed to, through Credential.MessagingPublicKey — the one derivation
	// in this package — rather than recomputed here. A second copy of "seed to
	// public key" is a second place for the two to disagree, and a messaging
	// public key that does not match the seed we keep is the worst failure
	// available: the bus records and attests it, and then every peer rejects
	// everything this agent signs, with a symptom that points nowhere near the
	// cause.
	//
	// An EMPTY seed means a resumed attempt from a pending record written before
	// this field existed. Send NO messaging key then. The alternative — mint one
	// now — would present the original idempotency key with different content,
	// which the bus answers with a 409 (invariant 10): the retry of an
	// interrupted enrolment would become a permanent failure. Such an identity
	// keeps a locally-minted messaging key its bus cannot attest, which is the
	// pre-RELAY-13 state and is recoverable only by re-enrolling under a new id.
	msgSeedB64 := effective.MessagingKeySeed
	msgPubB64 := ""
	if msgSeedB64 != "" {
		if msgPubB64, err = (Credential{MessagingKeySeed: msgSeedB64}).MessagingPublicKey(); err != nil {
			// Deliberately NOT the wrapped error from MessagingPublicKey: it
			// names an agent id, and there is no agent id yet on this path, so
			// it would read "the stored messaging key for  is not valid base64".
			return EnrolResult{}, newError(KindConfig, op,
				"the saved messaging key material for idempotency key "+idemKey+" is damaged",
				"enrol again without --idempotency-key to generate a fresh key pair")
		}
	}

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	// CLOSED (INVITE-CLIENT-FU-PENDINGINVITE): the pending record now carries
	// the invite id, so resuming with --idempotency-key and a DIFFERENT invite
	// file is refused by ClaimEnrolment above — locally, with no round trip and
	// with the key material kept. It used to be caught only by the BUS, which
	// fingerprints (name, auth key, messaging key, invite_id) and answers 409;
	// that 409 is KindRejected, which landed in enrolFailed's default branch and
	// DROPPED the pending record, destroying the only copy of the original
	// attempt's private keys — an identity the bus may already have minted, and
	// which no further retry could then produce. Two things now stand between
	// that sequence and the key material: the local refusal, and enrolFailed's
	// rule that a RESUMED attempt never drops.
	var busOverride *url.URL
	var pinsOverride BusPinSet
	if opts.Invite != nil {
		// Only on this path. Leaving the no-invite path to resolve its own
		// endpoint inside do() keeps it byte-identical to what it was.
		busOverride, pinsOverride = busURL, pins
	}

	var body enrolResponseBody
	resp, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   routeEnroll,
		op:     op,
		body: enrolRequestBody{
			Name:               name,
			PublicKey:          pubB64,
			MessagingPublicKey: msgPubB64,
			IdempotencyKey:     idemKey,
			InviteID:           inviteID,
			InviteSecret:       inviteSecret,
		},
		out: &body,
		// The endpoint this ONE request goes to, when it came from the INVITE
		// rather than from the config or a stored identity. Nil on the no-invite
		// path, where do() resolves it exactly as it always has.
		// transportSecurity still runs either way.
		busOverride:  busOverride,
		pinsOverride: pinsOverride,
		// Safe to repeat: the request carries an idempotency key and the
		// payload is byte-identical on every attempt, so the bus replays the
		// original result rather than enrolling twice (invariant 10). That
		// holds WITH an invite too, and is the reason the invite secret is
		// absent from the server's idempotency fingerprint: a redemption whose
		// key and payload match one already applied is invite.OutcomeReplay —
		// the original 201 body, with Idempotency-Replayed: true — and NOT a
		// second spend of a single-use invite.
		retryable: true,
	})
	if err != nil {
		return EnrolResult{}, c.enrolFailed(op, idemKey, busURL.String(), opts, resumed, err)
	}
	// The bus is authoritative on ids (invariant 1) — which makes them
	// authoritative, not unvalidated. These values are STORED and reprinted by
	// every later command, so a hostile or broken bus that returned control
	// characters here would poison the local store permanently. Reject rather
	// than sanitise: silently rewriting an id would leave us disagreeing with
	// the bus about who we are.
	if err := validateServerField(op, "agent id", body.AgentID); err != nil {
		return EnrolResult{}, err
	}
	if err := validateServerField(op, "bus id", body.BusID); err != nil {
		return EnrolResult{}, err
	}
	if err := validateServerField(op, "name", body.Name); err != nil {
		return EnrolResult{}, err
	}
	if err := validateServerTimestamp(op, "enrolled_at", body.EnrolledAt); err != nil {
		return EnrolResult{}, err
	}

	// The accept-set is recorded ONLY as empty, or as the fingerprints that were
	// actually in force for the connection that just succeeded. It is not
	// copied from a flag we did not use, and it is never derived from the
	// certificate the bus happened to present — deriving it here is exactly
	// trust-on-first-use, wearing the costume of a stored pin.
	//
	// It is USUALLY one fingerprint — the single --bus-fingerprint /
	// AGENT_BUS_FINGERPRINT value — but NOT always, and an earlier draft of this
	// comment claimed "at most one, by definition", which both gates showed to
	// be false. resolvePins consults the CURRENTLY SELECTED identity, so
	// enrolling a SECOND agent into the same credential store against a bus that
	// is mid-rollover inherits that identity's two-pin set.
	//
	// That is correct rather than a hole: both members were granted for this
	// exact bus URL by an explicit operator act, so nothing is trusted here that
	// the operator has not already confirmed out of band. It is written down
	// because the alternative — assuming one — is how a future reader
	// "simplifies" this to pins.Strings()[0] and quietly halves a rollover.
	// Joining a rollover LATER is still the deliberate path: `agent-busctl pin add`.
	cred := Credential{
		Identity: Identity{
			AgentID:         body.AgentID,
			BusID:           body.BusID,
			Name:            body.Name,
			BusURL:          busURL.String(),
			BusFingerprints: pins.Strings(),
			PublicKey:       pubB64,
			EnrolledAt:      body.EnrolledAt,
		},
		PrivateKeySeed: seedB64,
		// Stored because the BUS now holds its public half. Dropping it here
		// would leave EnsureMessagingKey to mint a different key on the first
		// send, and the agent would sign with a key that is not the one its own
		// bus recorded and attests — so every peer would reject every message.
		// Empty only on the legacy-resume path above, where the bus recorded no
		// messaging key either, and first-use minting is then still correct.
		MessagingKeySeed: msgSeedB64,
		IdempotencyKey:   idemKey,
	}

	result := EnrolResult{
		Identity:       cred.Identity,
		Replayed:       strings.EqualFold(resp.Header.Get(idempotencyReplayedHeader), "true"),
		IdempotencyKey: idemKey,
		// The id only. The secret is not carried out of this function.
		InviteID: inviteID,
	}

	if opts.Save {
		if err := c.store.PromotePending(idemKey, cred, opts.MakeCurrent); err != nil {
			// The enrolment SUCCEEDED on the bus and we could not record it.
			// Say exactly that: the remedy is different from a failed
			// enrolment, and a caller told only "store error" would retry the
			// whole thing and orphan a second agent id.
			return result, wrapError(KindConfig, op,
				"the bus enrolled "+body.AgentID+" but the credential could not be saved",
				"fix the credential store, then retry with --idempotency-key "+idemKey+" to recover the same identity",
				err)
		}
		result.Stored = true
		result.StorePath = c.store.Path()
		c.forgetIdentity()
	}
	return result, nil
}

// inviteEndpoint resolves the bus and the certificate pin FROM THE INVITE.
//
// The invite is the trust anchor (invariant 11, DECISIONS.md E6): it carries
// both values so the very first connection can be verified, which is what
// removes the trust-on-first-use window. So with an invite, --bus and
// --bus-fingerprint are unnecessary — and a supplied one that DISAGREES is
// refused rather than resolved by precedence.
//
// Refusing is the same judgement resolvePins already makes about a flag that
// disagrees with a stored pin, and it is made here for the same reason: one of
// the two values is wrong about which bus this is, and silently preferring the
// one that arrived on the command line is exactly how an operator is talked into
// pointing at the wrong bus ("it did not work, so I passed the address the other
// end gave me"). Refusing makes the disagreement visible, which is the only way
// it gets checked out of band.
func (c *Client) inviteEndpoint(op string, inv *Invite) (*url.URL, BusPinSet, error) {
	if err := inv.Validate(); err != nil {
		return nil, BusPinSet{}, err
	}
	u, err := inv.busURL()
	if err != nil {
		return nil, BusPinSet{}, err
	}
	// COMPARED AFTER parseBusURL, on both sides, so two spellings of the same
	// bus — a trailing slash, an uppercase host — are not reported as a
	// disagreement. See canonical.go.
	if raw := strings.TrimSpace(c.cfg.BusURL); raw != "" {
		flagged, ferr := parseBusURL(raw)
		if ferr != nil {
			return nil, BusPinSet{}, ferr
		}
		if flagged.String() != u.String() {
			// The invite's address goes through safeText; the flag's does not
			// need to (the caller typed it, and parseBusURL has canonicalised
			// it). See Invite.busURL for why an invite-supplied URL is treated
			// as hostile text.
			return nil, BusPinSet{}, newError(KindUsage, op,
				"--bus says "+flagged.String()+" but invite "+inv.InviteID+" is for "+safeText(u.String(), maxDetailBytes),
				"these name different buses and one of them is wrong. The invite carries the address AND the certificate fingerprint, so drop --bus / "+EnvBusURL+" and let the invite name the bus — or, if you believe the invite is wrong, confirm it OUT OF BAND and ask the operator for a fresh one")
		}
	}

	pin, err := inv.fingerprint()
	if err != nil {
		return nil, BusPinSet{}, err
	}
	// A flag that disagrees with the invite is refused BEFORE resolvePinsWith
	// sees either, so the message can name both sources. An equal one is simply
	// redundant and is allowed.
	if !c.pin.IsZero() && !pin.IsZero() && c.pin != pin {
		return nil, BusPinSet{}, newError(KindUsage, op,
			"--bus-fingerprint says "+c.pin.String()+" but invite "+inv.InviteID+" names certificate "+pin.String(),
			"these name different certificates and one of them is wrong. The invite is the trust anchor (invariant 11), so drop --bus-fingerprint / "+EnvBusFingerprint+
				" and let the invite pin the bus — do NOT substitute a fingerprint from anywhere else; if you believe the invite is wrong, confirm the bus's `bus_cert_fingerprint=…` startup log line OUT OF BAND and ask the operator for a fresh invite")
	}
	src := pinFromInvite(inv.InviteID)
	if pin.IsZero() {
		// An invite with no fingerprint is legal ONLY for a plaintext loopback
		// bus (Validate refuses an https invite without one). Fall through to
		// whatever the flag said so transportSecurity gives the ordinary
		// "pinned a plaintext URL" refusal rather than silently dropping it.
		//
		// The SOURCE follows the value: the pin now under test came from the
		// flag, so a disagreement must be reported against the flag — naming the
		// invite would send the operator to inspect a field it does not carry.
		pin = c.pin
		src = pinFromFlag
	}
	return c.endpointWith(u, pin, src)
}

// enrolFailed cleans up after a failed enrolment and improves the error.
//
// The distinction it draws is the useful one. A request the bus REFUSED (a bad
// name, an unknown route) will be refused identically forever, so the pending
// key material is dead weight and is dropped. A request that never got an
// answer — a timeout, a connection failure — may well have been APPLIED, so
// the key material is kept and the caller is told the exact flag that resumes
// it.
//
// # TWO conditions forbid the drop, and it takes both to be safe
//
// The drop is only sound for material this call MINTED and never sent. Two
// separate things break that assumption, and an earlier draft of this fix had
// only the first — the security gate reproduced the hole the second one closes.
//
//  1. `resumed`: the seeds belong to an EARLIER attempt, one interrupted after
//     its request went out, which is the entire reason it is still on disk. The
//     bus's answer to THIS request is not evidence about that one. A resume
//     presenting a different invite is answered 409 because the server's
//     fingerprint covers invite_id (INVITE-CLIENT-FU-PENDINGINVITE).
//
//  2. A 409 on a FRESH attempt. `resumed` means "the store already held a
//     record", but the property that actually matters is "a request under this
//     key has already reached the bus" — and those differ, because this request
//     is sent with retryable: true. do() retries a KindNetwork failure up to
//     RetryPolicy.Attempts times, so attempt 2 can land inside attempt 1's own
//     [Begin, Commit) window and be answered 409 "another redemption of this
//     invite is in flight" with resumed still FALSE. A 409 is the bus SAYING it
//     already holds this key with other content, which is only possible if a
//     request under it arrived; the local seeds may therefore be the private
//     half of something already applied. So a 409 never drops, whoever minted
//     the material. (INVITE-CLIENT-FU-INFLIGHT409 still owns the OTHER half of
//     that bug: the remedy text tells the caller to use a fresh key, which
//     against a single-use invite converts a transient failure into a
//     permanently lost enrolment.)
//
// The trade is not close. Keeping a record that turns out to be dead costs one
// 0600 store entry until prunePending expires it at pendingTTL; dropping one
// that was live destroys a private key permanently and takes the agent id with
// it. Everything else still drops: a 400, a 403 on a FRESH attempt, an unknown
// route — refusals that will repeat identically and that no request under this
// key could have survived.
//
// This now matches writeFailed's (messages.go) shape byte-for-byte, which was
// the model it was always meant to converge on (45b2e17a / 799aea40 —
// near-duplicate reports of the same three defects): the retry clause is
// COMPOSED onto e.Remedy rather than replacing it (so an unreachable-bus
// enrol keeps "check --bus / AGENT_BUS_URL and that the bus is running"
// instead of losing it, and — per the MTLS-EXPIRY cross-reference on
// 45b2e17a — the certificate-pin remedy on the enrol path, which the old
// overwrite also destroyed), e.fatal is checked so a 503 the bus cannot
// durably accept is told NOT to retry rather than being handed the ordinary
// retry wording, and e.IdempotencyKey is ALWAYS stamped — including when
// opts.Save is false and when the error is not ambiguous — because errors.go
// documents an empty key as meaning no key ever existed, which was false for
// every failed enrol before this fix (`enrol --json`'s idempotency_key field
// was silently empty where `send`'s was not).
func (c *Client) enrolFailed(op, idemKey, busURL string, opts EnrolOptions, resumed bool, err error) error {
	// errors.As, not a type assertion — see annotateSessionError. Stamped
	// before the opts.Save branch below: the key was sent to the bus either
	// way, and IdempotencyKeyOf(err) must answer it regardless of whether the
	// caller asked this attempt to be persisted locally.
	var e *Error
	if !errors.As(err, &e) {
		// Nothing to stamp or compose onto — pass through unchanged, same as
		// writeFailed does for a non-*Error. KindOf(err) would report
		// KindInternal here too, so the switch below could never have matched
		// KindNetwork/KindServer for this err anyway.
		//
		// !resumed for the reason in the doc comment: an unclassified failure is
		// the LEAST evidence there is that an earlier attempt was not applied,
		// so it is the last place that may delete that attempt's key material.
		if opts.Save && !resumed {
			_ = c.store.DropPending(idemKey, busURL)
		}
		return err
	}
	e.IdempotencyKey = idemKey
	annotateInviteRefusal(opts.Invite, e)
	annotateInviteNetworkRemedy(opts.Invite, e)

	if !opts.Save {
		return e
	}

	switch e.Kind {
	case KindNetwork, KindServer:
		clause := "this " + op + " may or may not have been applied; retry with --idempotency-key " + idemKey +
			" so the retry is the SAME enrolment rather than a second one (invariant 10)"
		if e.fatal {
			// A 503 with no Retry-After: the bus cannot durably accept right
			// now (invariant 4), and that will not clear by asking again
			// immediately — see the identical reasoning in writeFailed.
			clause = "this " + op + " may or may not have been applied; do NOT retry until the bus can durably accept again, then use --idempotency-key " + idemKey +
				" so the retry is the SAME enrolment rather than a second one (invariant 10)"
		}
		// TrimRight so a remedy that already ends in a separator does not
		// produce ";; " — the join must add exactly one.
		if base := strings.TrimRight(e.Remedy, "; "); base != "" {
			e.Remedy = base + "; " + clause
		} else {
			e.Remedy = clause
		}
		return e
	default:
		// A 409 counts EVEN WHEN the material is this call's own: it means a
		// request under this key already reached the bus, which an in-call
		// retry can achieve without any pending record having pre-existed. See
		// the doc comment.
		if resumed || e.Status == http.StatusConflict {
			// The seeds may be the private half of something the bus already
			// applied, so this refusal is not evidence against them. Keep them
			// — and SAY so with the key that reaches them, because material
			// stored where nobody is told to look is stored, not recoverable.
			clause := "the key material for --idempotency-key " + idemKey +
				" is KEPT and NOT dropped: the bus may already hold its public halves, so it is the only thing that could ever recover that identity"
			if base := strings.TrimRight(e.Remedy, "; "); base != "" {
				e.Remedy = base + "; " + clause
			} else {
				e.Remedy = clause
			}
			return e
		}
		_ = c.store.DropPending(idemKey, busURL)
		return e
	}
}

// annotateInviteRefusal rewrites the remedy for a bus that REFUSED the invite.
//
// Necessary because a refused invite arrives as HTTP 403, which statusError
// maps to KindAuth with "the session may have expired or the bus may have
// restarted; retry, and if it persists re-enrol with `agent-busctl enrol`" —
// advice that is actively wrong here on every clause. There is no session
// (enrolment is one of the three routes that has none), retrying cannot help
// (an invite is single-use, and expiry and revocation do not reverse), and
// "re-enrol" is what the caller was already doing.
//
// The server's writeInviteError deliberately answers unknown, expired, revoked,
// already-redeemed and malformed with the SAME 403 and the same terse text, so
// that a caller cannot use the response to probe which invites exist. This
// remedy therefore enumerates the possibilities rather than claiming to know
// which one applies — saying "your invite has expired" would be inventing a fact
// the bus refused to disclose.
//
// Only the invite ID appears, which is a name and not a credential. The SECRET
// is never put in an error. The id goes through safeText even so: Validate has
// already refused anything outside serverIDPattern by the time Enrol reaches
// here, but this function is package-level and takes any *Invite, and a
// neutraliser that depends on a caller having validated first is one that stops
// holding the day someone adds a second caller.
//
// The KindAuth classification is left alone: it is what routes this to the
// default branch of enrolFailed, which drops the pending key material of a
// FRESH attempt — correct, because a refused invite will be refused identically
// forever, so that attempt has nothing to recover. It does NOT drop a RESUMED
// attempt's material: there the refusal is of the invite presented on the
// RETRY, and it says nothing about whether the original request was applied.
func annotateInviteRefusal(inv *Invite, e *Error) {
	if inv == nil || e == nil || e.Status != http.StatusForbidden {
		return
	}
	// statusError's 401/403 wording is about a SESSION credential, which this
	// route does not have. Strip it if it is there rather than prefixing it, so
	// the line does not read "…refused invite X: the bus rejected this
	// credential: …". A TrimPrefix that matches nothing simply leaves the
	// message intact, which is still correct, just wordier.
	e.Message = "the bus refused invite " + safeText(inv.InviteID, MaxInviteIDLen) + ": " +
		strings.TrimPrefix(e.Message, "the bus rejected this credential: ")
	e.Remedy = "an invite is single-use, expiring and revocable, so this one may already have been redeemed, may have expired, or may have been revoked — the bus deliberately does not say which. Retrying will not change it: ask the operator to mint a fresh invite (`agent-bus invite mint`) and redeem that instead"
}

// annotateInviteNetworkRemedy corrects the ONE base remedy that names a flag
// the invite path never used.
//
// networkError's unreachable-bus remedy is "check --bus / AGENT_BUS_URL and
// that the bus is running". That is right everywhere else and wrong here: with
// --invite-file the address came from the blob, so the flag it names is one the
// caller never passed and correcting it would fix nothing (invariant 7 — an
// error names a remedy that WORKS). Worse, it points at the wrong artefact: if
// the address is unreachable, the thing to check is the INVITE.
//
// It REPLACES only that exact base text and only for KindNetwork, because the
// other network remedies are already correct on both paths — a timeout says
// "raise --timeout", and a fingerprint mismatch has its own pinError wording
// that must not be paperd over. It runs BEFORE enrolFailed composes the retry
// clause, so the clause is still COMPOSED onto a base rather than replacing it
// (45b2e17a / 799aea40: overwriting the base is the defect that fix removed,
// and this must not reintroduce it by another route).
func annotateInviteNetworkRemedy(inv *Invite, e *Error) {
	if inv == nil || e == nil || e.Kind != KindNetwork {
		return
	}
	// Coupled to networkError (transport.go) by exact text on purpose: a
	// prefix match would also catch a future remedy that mentions --bus for a
	// different and legitimate reason, and replacing THAT would lose real
	// advice. If the two ever drift apart the caller keeps the old wording,
	// which is merely inapplicable rather than wrong about the failure.
	const busFlagRemedy = "check --bus / " + EnvBusURL + " and that the bus is running"
	if e.Remedy != busFlagRemedy {
		return
	}
	e.Remedy = "check that the bus is running and reachable at the address the invite carries. That address came from --invite-file, not from --bus / " + EnvBusURL +
		", so there is no flag here to correct: if the address is wrong then the INVITE is wrong — confirm it out of band and ask the operator to mint a fresh one"
}

// idempotencyConflict is the LOCAL refusal of a key reused for different
// content on the same bus.
//
// Catching it here rather than letting the bus catch it is not politeness:
// the bus's answer to this is a 409 that it rejects and logs (invariant 10),
// so the round trip costs a connection attempt and teaches the caller
// nothing this message does not — and, per the field evidence in
// IDEM-14-FU-CLIENTTEXT (see annotateIdempotencyConflict in messages.go), it
// does NOT disconnect, so this text must not claim more than that either.
func idempotencyConflict(op, key, previousName, busURL string) *Error {
	return newError(KindUsage, op,
		fmt.Sprintf("idempotency key %q was already used to enrol %q at %s", key, previousName, busURL),
		"use a fresh idempotency key for different content — reusing one with a different payload is a protocol violation that the bus rejects and logs (invariant 10)")
}

// inviteConflict is the LOCAL refusal of a resume that presents a DIFFERENT
// invite from the one the stored key material was minted for.
//
// It is the same judgement idempotencyConflict makes about a different name,
// and it is made for a sharper reason. The server's idempotency fingerprint
// covers invite_id, so this is "same key + DIFFERENT payload" and the bus
// answers 409 — but the client's handling of that 409 used to DROP the pending
// record, destroying the only copy of the private keys of an attempt the bus
// may already have applied (INVITE-CLIENT-FU-PENDINGINVITE). Refusing here
// costs a round trip that could only ever fail, and it keeps the material.
//
// Both ids go through safeText. The stored one is ours, but the store is a file
// on disk that could have been edited, and the presented one came out of an
// invite blob — attacker-influenced text that is about to be printed to a
// terminal. Neither SECRET appears: this function is not given one, which is
// the point at which that guarantee is easiest to keep.
func inviteConflict(op, key, storedInvite, presentedInvite, busURL string) *Error {
	presented := "no invite at all"
	if presentedInvite != "" {
		presented = "invite " + safeText(presentedInvite, MaxInviteIDLen)
	}
	e := newError(KindUsage, op,
		fmt.Sprintf("idempotency key %q is an in-flight enrolment at %s that redeems invite %s, but this attempt presents %s",
			key, busURL, safeText(storedInvite, MaxInviteIDLen), presented),
		"resume it with the invite it started with (--invite-file for invite "+safeText(storedInvite, MaxInviteIDLen)+
			"), or start a NEW enrolment with a fresh idempotency key and a fresh invite. The bus fingerprints the invite id, so sending this would be the same key with a different payload — a protocol violation it rejects and logs (invariant 10). Nothing has been sent and the stored key material is KEPT: it is the only copy of that attempt's private keys, and the bus may already have recorded their public halves")
	// The key is the caller's OWN and is the handle that resumes the attempt,
	// so it is stamped: errors.go documents an empty one as meaning no key ever
	// existed, which is false here.
	e.IdempotencyKey = key
	return e
}

// validateAgentName checks a requested name against the server's rule, with a
// message that names the remedy.
func validateAgentName(op, name string) error {
	if name == "" {
		return usagef(op, "pass --name <name>, e.g. --name planner", "no agent name")
	}
	if agentNameRegexp.MatchString(name) {
		return nil
	}
	if lowered := strings.ToLower(name); lowered != name && agentNameRegexp.MatchString(lowered) {
		// The single most likely mistake, and the one whose bare error is
		// least informative. The server rejects rather than folds case, on
		// purpose — see internal/ids/agentid.go.
		return usagef(op, "use --name "+lowered+" instead; agent names are lowercase and are rejected rather than folded",
			"agent name %q contains uppercase letters", name)
	}
	return usagef(op,
		"names are 1-64 bytes of [a-z0-9_-], starting with a letter or digit, and may not contain '.' (it separates the bus id from the agent id)",
		"agent name %q is not valid", name)
}

// idempotencyKeyPattern mirrors internal/auth.validateIdempotencyKey:
// non-empty, at most 128 bytes, [A-Za-z0-9._-] only.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validateIdempotencyKey(op, key string) error {
	if idempotencyKeyPattern.MatchString(key) {
		return nil
	}
	return usagef(op,
		"an idempotency key is 1-128 bytes of [A-Za-z0-9._-]; omit the flag entirely to have one generated",
		"idempotency key is not valid")
}

// newIdempotencyKey mints a random key.
//
// 16 bytes of crypto/rand, hex encoded: inside the server's [A-Za-z0-9._-]
// alphabet and its 128-byte bound, and wide enough that two agents enrolling
// at the same instant cannot collide — a collision would look to the bus like
// the same key with a different payload, which invariant 10 makes a protocol
// violation: the bus rejects and logs it (it does NOT disconnect; narrowed
// 2026-08-08), so the loser's enrolment fails outright and, worse, one of the
// two agents' key material is the one at risk. That consequence is why this is
// crypto/rand and not a timestamp.
func newIdempotencyKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", wrapError(KindInternal, "enrol",
			"cannot generate an idempotency key: the system random source failed", "", err)
	}
	// The "busctl-" prefix is DELIBERATELY NOT renamed to "agent-busctl-"
	// alongside the CLI binary rename (CLI-1-FU-BINARYNAME, DECISIONS.md
	// 2026-08-07). This value is wire-visible: it is the idempotency key the
	// server durably remembers (invariant 10), so changing it would not
	// rename a label, it would change the identity of every key this client
	// mints going forward. Leave it exactly "busctl-".
	return "busctl-" + hex.EncodeToString(buf[:]), nil
}
