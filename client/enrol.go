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

	// Invite is the operator-minted invite blob.
	//
	// RESERVED AND NOT YET IMPLEMENTED. Enrolment is becoming invite-only
	// (invariant 3, 2026-08-02) and the blob will carry bus id, address, bus
	// certificate fingerprint and invite secret — but the WIRE SHAPE is settled
	// by task ENROL-SHAPE and is not settled yet, and /v1/enroll is explicitly
	// UNSTABLE until it, certificate binding and POPKEY all land.
	//
	// Setting it therefore fails FAST AND LOCALLY with a remedial error rather
	// than inventing a field name on the wire. Inventing one would be the same
	// class of mistake as hand-picking a record-type number: the shape is
	// reserved, not chosen.
	Invite string

	// IdempotencyKey makes the enrolment safe to retry (invariant 10). Leave
	// it empty and Enrol mints a fresh random one.
	//
	// Supply one only to RETRY a specific earlier attempt. Enrol then reuses
	// the key material that attempt generated — which is what makes the retry
	// legitimate, since the payload must be byte-identical — and refuses
	// locally if the key was used for different content.
	IdempotencyKey string

	// Save stores the resulting credential in the credential store. Callers
	// that only want to test enrolment can turn it off — but note that the
	// private key is then lost when the process exits, and the enrolment it
	// created stays on the bus.
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
}

// enrolRequestBody mirrors httpapi.EnrolRequestBody. The server rejects
// unknown fields, so this struct is exactly the wire shape and nothing more.
type enrolRequestBody struct {
	Name           string `json:"name"`
	PublicKey      string `json:"public_key"`
	IdempotencyKey string `json:"idempotency_key"`
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
// The private key is generated HERE and only the public half is sent. The bus
// can therefore verify this agent's calls and can never forge them.
//
// # Ordering: the key is on disk BEFORE the request goes out
//
// The seed is written to the credential store as a PENDING enrolment before
// /v1/enroll is called, and promoted to a full credential when the bus
// answers. Two things fall out of that ordering, and both are the point:
//
//   - A process killed after the bus minted an id but before the answer was
//     stored does not lose the private key. Retrying with the SAME idempotency
//     key recovers the identity, because the same key material is still there.
//   - A retry is a real retry. Invariant 10 defines one as "same key + SAME
//     payload"; the payload contains the public key, so a client that
//     regenerated its key pair would be sending "same key + DIFFERENT payload"
//     — a protocol violation that gets it DISCONNECTED. Reusing the pending
//     record is what makes the retry legitimate rather than an offence.
//
// When the same idempotency key is presented with different content, Enrol
// refuses LOCALLY. It could send it and let the bus refuse, but the bus's
// refusal comes with a disconnection, and there is no reason to spend that on
// a mistake we can see from here.
func (c *Client) Enrol(ctx context.Context, opts EnrolOptions) (EnrolResult, error) {
	const op = "enrol"

	if opts.Invite != "" {
		return EnrolResult{}, newError(KindUsage, op,
			"invite redemption is not implemented yet",
			"enrol without --invite for now; the invite wire shape is settled by task ENROL-SHAPE and /v1/enroll is UNSTABLE until it, certificate binding and POPKEY land")
	}

	name := strings.TrimSpace(opts.Name)
	if err := validateAgentName(op, name); err != nil {
		return EnrolResult{}, err
	}

	// Enrolment needs an EXPLICIT bus, and says so here rather than in the CLI.
	//
	// Every other operation may fall back to the selected identity's recorded
	// URL, but enrolment by definition has no identity on the bus it is
	// joining, so that fallback is meaningless — and letting it happen would
	// surface as KindConfig ("no identity has been enrolled", exit 3) when the
	// caller's actual mistake was a missing --bus (KindUsage, exit 2). Putting
	// the check in the package means an EMBEDDING caller gets the same
	// classification the CLI does.
	if strings.TrimSpace(c.cfg.BusURL) == "" {
		return EnrolResult{}, newError(KindUsage, op,
			"no bus URL",
			"enrolment needs an explicit bus: pass --bus <url> or set "+EnvBusURL)
	}

	// Resolve the bus URL before anything is written, so a malformed --bus
	// does not leave a pending record behind.
	busURL, err := c.resolveBusURL()
	if err != nil {
		return EnrolResult{}, err
	}

	idemKey := strings.TrimSpace(opts.IdempotencyKey)
	if idemKey == "" {
		if idemKey, err = newIdempotencyKey(); err != nil {
			return EnrolResult{}, err
		}
	} else if err := validateIdempotencyKey(op, idemKey); err != nil {
		return EnrolResult{}, err
	}

	// A fresh key pair for a new attempt. ClaimEnrolment may hand back an
	// earlier attempt's material instead, in which case this one is discarded
	// unused — cheap, and it keeps the random draw outside the lock.
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return EnrolResult{}, wrapError(KindInternal, op,
			"cannot generate a key pair: the system random source failed", "", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))

	want := pendingEnrolment{
		IdempotencyKey: idemKey,
		Name:           name,
		BusURL:         busURL.String(),
		PublicKey:      pubB64,
		PrivateKeySeed: base64.StdEncoding.EncodeToString(seed),
		CreatedAt:      c.now().UTC().Format(time.RFC3339Nano),
	}

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
			// the old key is exactly the violation that earns a disconnect.
			return EnrolResult{
				Identity:       claim.Applied.Identity,
				Replayed:       true,
				Stored:         true,
				StorePath:      c.store.Path(),
				IdempotencyKey: idemKey,
			}, nil
		}
		effective = claim.Pending
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

	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()

	var body enrolResponseBody
	resp, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   routeEnroll,
		op:     op,
		body: enrolRequestBody{
			Name:           name,
			PublicKey:      pubB64,
			IdempotencyKey: idemKey,
		},
		out: &body,
		// Safe to repeat: the request carries an idempotency key and the
		// payload is byte-identical on every attempt, so the bus replays the
		// original result rather than enrolling twice (invariant 10).
		retryable: true,
	})
	if err != nil {
		return EnrolResult{}, c.enrolFailed(op, idemKey, busURL.String(), opts, err)
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

	cred := Credential{
		Identity: Identity{
			AgentID:    body.AgentID,
			BusID:      body.BusID,
			Name:       body.Name,
			BusURL:     busURL.String(),
			PublicKey:  pubB64,
			EnrolledAt: body.EnrolledAt,
		},
		PrivateKeySeed: seedB64,
		IdempotencyKey: idemKey,
	}

	result := EnrolResult{
		Identity:       cred.Identity,
		Replayed:       strings.EqualFold(resp.Header.Get(idempotencyReplayedHeader), "true"),
		IdempotencyKey: idemKey,
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

// enrolFailed cleans up after a failed enrolment and improves the error.
//
// The distinction it draws is the useful one. A request the bus REFUSED (a bad
// name, an unknown route) will be refused identically forever, so the pending
// key material is dead weight and is dropped. A request that never got an
// answer — a timeout, a connection failure — may well have been APPLIED, so
// the key material is kept and the caller is told the exact flag that resumes
// it.
func (c *Client) enrolFailed(op, idemKey, busURL string, opts EnrolOptions, err error) error {
	if !opts.Save {
		return err
	}
	switch KindOf(err) {
	case KindNetwork, KindServer:
		// errors.As, not a type assertion — see annotateSessionError.
		var e *Error
		if !errors.As(err, &e) {
			return err
		}
		e.Remedy = "retry with --idempotency-key " + idemKey +
			" — the bus may already have enrolled this agent, and that key resumes the SAME enrolment instead of creating a second one"
		return e
	default:
		_ = c.store.DropPending(idemKey, busURL)
		return err
	}
}

// idempotencyConflict is the LOCAL refusal of a key reused for different
// content on the same bus.
//
// Catching it here rather than letting the bus catch it is not politeness: the
// bus's answer to this is a 409 AND a disconnection (invariant 10), so the
// round trip costs a connection and teaches the caller nothing this message
// does not.
func idempotencyConflict(op, key, previousName, busURL string) *Error {
	return newError(KindUsage, op,
		fmt.Sprintf("idempotency key %q was already used to enrol %q at %s", key, previousName, busURL),
		"use a fresh idempotency key for different content — reusing one with a different payload is a protocol violation that disconnects the client (invariant 10)")
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
// the same key with a different payload, which is a protocol violation that
// DISCONNECTS the client (invariant 10). That consequence is why this is
// crypto/rand and not a timestamp.
func newIdempotencyKey() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", wrapError(KindInternal, "enrol",
			"cannot generate an idempotency key: the system random source failed", "", err)
	}
	return "busctl-" + hex.EncodeToString(buf[:]), nil
}
