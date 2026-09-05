package attest

import (
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// ClockSkewAllowance is how far a verifier's clock may run ahead of a minter's
// before NotAfter is treated as passed. It follows the house precedent —
// internal/buscert's clockSkewAllowance, 5 minutes — so a bus and its peer
// disagreeing by a few seconds does not turn into a federation-wide refusal,
// which is the single most confusing way for a fresh deployment to fail.
//
// It is exported so an operator-facing diagnostic can state the tolerance it is
// applying, and so a test can reason about the boundary without duplicating the
// number.
const ClockSkewAllowance = 5 * time.Minute

// MaxAttestationLifetime is the VERIFIER-SIDE ceiling on how far into the future
// an attestation may still be trusted, measured from the verifier's own clock
// (NotAfter - now). An attestation whose remaining validity exceeds this is
// REFUSED at Verify with ErrLifetimeExceeded, REGARDLESS of the NotAfter the
// minter wrote — the whole point is that the minter does not get to choose it.
//
// # Why this exists (RELAY-28)
//
// NotAfter is minted by the ORIGIN bus and, until this ceiling, was trusted
// entirely at its discretion. Revocation across a non-adjacent link is UNSOLVED
// (FEDERATION_TRUST_DEEPDIVE.md §4.4, follow-up RELAY-29), so NotAfter is the
// ONLY bound on a compromised agent messaging key: once the key is trusted, only
// its expiry stops it. A minter that writes NotAfter = year 292278994 — whether
// compromised, buggy, or misconfigured — makes that key eternal, and no verifier
// downstream can tell. This ceiling is the verifier refusing to be bound by a
// number it did not compute: it independently caps the exposure at a value it
// derives itself, so an absurd NotAfter buys the attacker at most this window
// rather than forever. With revocation unsolved this is the load-bearing control,
// not defence in depth.
//
// # The derivation — every term is traceable, none is picked
//
// FEDERATION_TRUST_DEEPDIVE.md §4.2 (P1-5) and Sign's own doc REQUIRE that an
// honest minter derive NotAfter from the maximum relay retention/retry window,
// never from a plausible-sounding constant — "the 24h in an earlier draft was
// picked, which is the same defect class as picking a migration number". The
// egress minter (cmd/agent-bus/relayegress.go) obeys this exactly:
//
//	notAfter = issuedAt + relay.RetryHorizonCeiling
//
// and relay.RetryHorizonCeiling IS idem.PeerOutageBudget (internal/relay/
// forward.go:72). So the LARGEST validity window an honest bus in this federation
// ever mints is exactly idem.PeerOutageBudget. The verifier's ceiling is that
// same federation-trust bound, plus one ClockSkewAllowance:
//
//		MaxAttestationLifetime = idem.PeerOutageBudget + ClockSkewAllowance
//
//	  - idem.PeerOutageBudget is the maximum honestly-minted window. This ceiling
//	    imports it DIRECTLY, the same constant relay.RetryHorizonCeiling is defined
//	    as (internal/relay/forward.go:72), so the ceiling cannot silently loosen: it
//	    moves only if idem.PeerOutageBudget moves. attest cannot import
//	    internal/relay (relay imports attest — a guard, internal/relay/
//	    guards_test.go, forbids the reverse), so the assertion that this ceiling
//	    still tracks the minter's constant lives on the relay side:
//	    TestMaxAttestationLifetimeTracksMinter (internal/relay, package relay)
//	    fails the day relay.RetryHorizonCeiling + ClockSkewAllowance stops equalling
//	    attest.MaxAttestationLifetime.
//	  - + ClockSkewAllowance because the check is NotAfter - now, and the verifier's
//	    clock may LAG the minter's by up to ClockSkewAllowance (the same tolerance
//	    the expiry check already grants in the other direction). At the earliest a
//	    verifier can observe a freshly-minted attestation, now = issuedAt - skew, so
//	    NotAfter - now = PeerOutageBudget + skew. Without this term an honest
//	    attestation verified immediately on a lagging clock would be wrongly
//	    refused. It is added ONCE, not doubled: this is the margin, not a budget.
//
// The check bounds NotAfter - now rather than NotAfter - IssuedAtUnixMilli
// deliberately. IssuedAt is minter-controlled too, so a minter setting NotAfter
// to the far future could set IssuedAt to the far future beside it and keep the
// (NotAfter - IssuedAt) window small — a bypass. now is the verifier's OWN clock
// and nothing in the blob can move it, so NotAfter - now is the honest measure of
// "how long will I keep trusting this key". The comparison is strict ( > ), so an
// attestation sitting exactly at the ceiling still verifies.
const MaxAttestationLifetime = idem.PeerOutageBudget + ClockSkewAllowance

// The failure taxonomy. Every one is matchable with errors.Is, and they are
// DISTINCT on purpose.
//
// A verifier that returns one opaque error for "malformed", "not the agent it
// claims", "signed by a bus we do not pin", "does not verify" and "expired"
// makes it impossible to tell an OPERATOR fault from ATTACKER input — and this
// package is reached on the relay ingress, where those two are the entire
// question. Collapsing expiry into the signature-failure family in particular
// would send an operator hunting a forgery that never happened.
//
// None of these is retryable. An attestation that does not verify does not
// verify on the second attempt either, and nothing the presenting peer can do
// on a retry establishes a pin.
var (
	// ErrAgentIDMismatch reports that the attestation names a DIFFERENT agent
	// than the one the caller is about to attribute a message to.
	//
	// THIS IS THE LOAD-BEARING CHECK OF THIS PACKAGE, NOT DEFENCE IN DEPTH.
	// See Verify's doc for the attack it stops, walked line by line, and
	// TestAttestationDoesNotAuthoriseADifferentAgentOnTheSameBus for the
	// negative test that fails if the check is removed.
	ErrAgentIDMismatch = errors.New("attest: the attestation names a different agent than the one being attributed")

	// ErrOriginBusMismatch reports an attestation whose subject lives on a
	// different bus than the one whose pins were looked up. It stops a peer
	// presenting a validly-signed attestation from ANOTHER bus we also pin.
	ErrOriginBusMismatch = errors.New("attest: the attestation's subject belongs to a different bus than the origin bus")

	// ErrUnpinned reports that no usable pinned bus signing key was supplied:
	// an empty pin set, or a pin that is not a well-formed Ed25519 public key.
	//
	// A MALFORMED PIN IS REFUSED, NOT SKIPPED. A malformed pin means the
	// PINNING STORE is wrong, not that this peer did anything; proceeding with
	// the well-formed remainder would verify against LESS than the operator
	// believes is pinned, silently. Callers map this onto relay's
	// ErrUnpeeredBus, whose remedy is an operator action, never a retry.
	ErrUnpinned = errors.New("attest: no usable pinned bus signing key")

	// ErrVerify reports a well-formed attestation, over well-formed canonical
	// bytes, that DOES NOT VERIFY under any supplied pin.
	//
	// It deliberately says nothing about why, because there is nothing to say:
	// Ed25519 verification is a single boolean, and a tampered field, a
	// substituted key, a forged signature and an attestation from a bus we do
	// not pin are indistinguishable from inside.
	ErrVerify = errors.New("attest: attestation signature does not verify under any pinned bus signing key")

	// ErrNoClock reports that no verification clock was supplied.
	//
	// It is a LOCAL FAULT — this bus called Verify wrong — and it has its own
	// sentinel for exactly that reason. ErrInvalid means "the peer sent
	// something malformed" and a caller is directed to answer it as a 400; a
	// wiring bug on OUR side reported to a PEER as its own bad request is the
	// misattribution this package refuses everywhere else. Nothing the peer
	// sends can produce this error.
	ErrNoClock = errors.New("attest: no verification clock was supplied")

	// ErrExpired reports an attestation that verified but whose NotAfter has
	// passed, allowing for ClockSkewAllowance.
	//
	// This is the sentinel FEDERATION_TRUST_DEEPDIVE.md §4.2 binding check 4
	// calls ErrAttestationExpired; inside this package it is spelled ErrExpired
	// so callers read attest.ErrExpired rather than attest.ErrAttestationExpired.
	//
	// IT IS ITS OWN SENTINEL AND MUST STAY ONE. An expired attestation is not a
	// forgery — it is very often an honest message that sat in an intermediate's
	// queue across a partition, because an intermediate forwards VERBATIM and
	// cannot re-mint. Folding it into ErrVerify sends an operator hunting an
	// attacker who does not exist.
	ErrExpired = errors.New("attest: attestation has expired")

	// ErrLifetimeExceeded reports an attestation that verified but whose remaining
	// validity (NotAfter - now) exceeds MaxAttestationLifetime — the verifier-side
	// ceiling this bus enforces regardless of the NotAfter the minter wrote
	// (RELAY-28).
	//
	// IT IS ITS OWN SENTINEL, DISTINCT FROM ErrExpired. ErrExpired is the benign,
	// common case — a genuinely bus-signed attestation that grew old in a queue.
	// This is the opposite and anomalous case: a NotAfter so far in the future that
	// no honest minter in this federation could have produced it, which points at a
	// compromised, buggy or misconfigured origin bus rather than a stale message.
	// Folding the two together would let an operator read a policy refusal as
	// ordinary expiry and miss the anomaly.
	ErrLifetimeExceeded = errors.New("attest: attestation validity window exceeds the maximum permitted lifetime")
)

// Subject is what the CALLER is about to act on: the identity it will attribute
// a message to, and the bus whose pins it looked those pins up by.
//
// It is a struct rather than two positional string parameters because
// Verify(pins, a, fqAgentID, originBus, now) and
// Verify(pins, a, originBus, fqAgentID, now) both compile, and one of them is
// wrong. Naming the fields removes the transposition entirely.
//
// Both fields are the CALLER's own validated values — NEVER copied out of the
// attestation. Populating either of them FROM the attestation being checked
// makes both binding checks below tautological and this package worthless: the
// blob would then be checked only against itself.
type Subject struct {
	// FQAgentID is the fully-qualified agent id the caller will attribute the
	// message to — for a relay ingress, the SENDER FIELD INSIDE THE SIGNED
	// MESSAGE BYTES.
	FQAgentID string

	// OriginBus is the bus id the caller used to look up the pins — for a relay
	// ingress, the envelope's validated origin bus.
	OriginBus string
}

// Sign mints an attestation binding agentID to msgPub, signed with this bus's
// BUS SIGNING key (internal/buscert.Material.SigningPrivateKey).
//
// busID is THIS bus's own id, and it is a parameter rather than something
// derived from agentID so that the mint side enforces the rule a verifier
// enforces on the other end: A BUS MAY ONLY SPEAK FOR ITS OWN AGENTS. Attesting
// an agent id in someone else's namespace is refused here, byte for byte and
// case-sensitively, so a bus cannot accidentally mint the very artefact that
// Verify's binding check 2 exists to reject.
//
// issuedAt and notAfter are the caller's to choose and this package does not
// pick them. notAfter in particular MUST be derived from the maximum relay
// retention/retry window rather than set to a plausible-sounding constant: an
// intermediate forwards verbatim and cannot re-mint, so anything queued longer
// than this window becomes permanently undeliverable. Both are truncated to
// Unix MILLISECONDS, which is the wire and signed representation; sub-
// millisecond precision is discarded rather than silently carried in one
// representation and not the other.
//
// The canonical bytes go to ed25519.Sign UNHASHED. There is no padding scheme,
// no nonce, no framing beyond the documented field order, and there must never
// be one (invariant 9).
func Sign(busSigningKey ed25519.PrivateKey, busID, agentID string, msgPub ed25519.PublicKey, epoch uint64, issuedAt, notAfter time.Time) (Attestation, error) {
	// Checked BEFORE ed25519.Sign, which PANICS on a wrong-size private key. A
	// bus signing key arrives from a file on disk an operator may have
	// truncated, copied half of, or replaced with a public key by mistake, so
	// this is a reachable input rather than a theoretical one.
	//
	// The key's LENGTH is reported; the key is not, and must never be. A
	// private key that reaches a log line or a returned error has left the
	// machine.
	if len(busSigningKey) != signing.PrivateKeySize {
		return Attestation{}, fmt.Errorf("%w: bus signing key is %d bytes, want exactly %d", ErrInvalid, len(busSigningKey), signing.PrivateKeySize)
	}

	subjectBus, _, _, err := ids.ParseAgentID(agentID)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: agent id: %v", ErrInvalid, err)
	}
	if err := ids.ValidateBusID(busID); err != nil {
		return Attestation{}, fmt.Errorf("%w: attesting bus id: %v", ErrInvalid, err)
	}
	if subjectBus != busID {
		// Both ids are validated and bounded by the two checks above, so
		// quoting them here cannot be used to choose the size of this line.
		return Attestation{}, fmt.Errorf("%w: bus %q may only attest its own agents, and agent %q belongs to bus %q", ErrOriginBusMismatch, busID, agentID, subjectBus)
	}

	a := Attestation{
		AgentID:           agentID,
		KeyEpoch:          epoch,
		IssuedAtUnixMilli: issuedAt.UnixMilli(),
		NotAfterUnixMilli: notAfter.UnixMilli(),
	}
	// COPIED, never aliased. The caller's key may be a slice into a buffer it
	// goes on to reuse; an attestation that changes under its own signature
	// after it was minted is the time-of-check/time-of-use shape this codebase
	// already guards against on relay signatures.
	if msgPub != nil {
		a.MessagingPublicKey = make(ed25519.PublicKey, len(msgPub))
		copy(a.MessagingPublicKey, msgPub)
	}

	b, err := Canonicalize(a)
	if err != nil {
		// Canonicalize already failed closed: there are no bytes, so there is
		// nothing to sign. Signing a best-effort serialisation would produce a
		// signature over something nobody specified.
		return Attestation{}, err
	}
	a.Signature = ed25519.Sign(busSigningKey, b)
	return a, nil
}

// Verify checks a against want under pins, and returns the MESSAGING PUBLIC KEY
// the origin bus attests for want.FQAgentID.
//
// FAIL-CLOSED: a nil error is the ONLY outcome on which a caller may use the
// returned key. On any error the returned key is nil.
//
// pins are the ORIGIN bus's signing keys as pinned OUT OF BAND by the operator
// — never a key that arrived beside the message, never one we have merely seen
// before, never a re-attestation by an intermediate. THERE IS NO
// TRUST-ON-FIRST-USE HERE, not as a mode, not as a fallback, not as a hook for
// one: an empty pin set is ErrUnpinned and the remedy is for the operator to
// establish the pin, never for this function to grow a fallback branch.
//
// pins is a LIST, not a scalar, for EXACTLY ONE reason: a signing-key ROLLOVER
// window, mirroring the two-certificate TLS rollover, so a federation-wide
// rotation need not be simultaneous. It is not a general-purpose list to be
// stuffed with anything we might like to accept.
//
// A FIFTH check, the verifier-side lifetime ceiling (check 5 below), runs after
// binding check 4: an attestation whose remaining validity (NotAfter - now)
// exceeds MaxAttestationLifetime is refused with ErrLifetimeExceeded no matter
// what NotAfter the minter wrote. See MaxAttestationLifetime's doc for RELAY-28
// and the derivation.
//
// # THE FOUR BINDING CHECKS, AND WHY CHECK 1 IS LOAD-BEARING
//
//  1. a.AgentID == want.FQAgentID, byte for byte.
//  2. The bus half of a.AgentID == want.OriginBus.
//  3. ed25519.Verify against the pins, and NOTHING else.
//  4. NotAfter has not passed, allowing ClockSkewAllowance.
//
// Check 1 was called "defence in depth" in an early draft of the design and
// THAT WAS WRONG; the security gate caught it (FEDERATION_TRUST_DEEPDIVE.md
// §4.2, 2026-08-08 P1-1) and it is recorded here in full so nobody re-derives
// the mistake. Without check 1, ANYONE HOLDING ONE AGENT'S MESSAGING PRIVATE
// KEY CAN SIGN AS EVERY OTHER AGENT ON THAT BUS, across the federation
// boundary:
//
//   - The attacker holds K_alice, the messaging key of A.alice.
//   - It builds an envelope with OriginBus "A", a message id minted by "A", and
//     Sender "A.bob", and signs the canonical message bytes with K_alice.
//   - It attaches the GENUINE, UNMODIFIED attestation for A.alice. Attestations
//     travel in the clear on every relayed message, so observing one yields one.
//   - The relay's own sender/origin-bus check passes: both halves say "A".
//   - The canonical message format's check passes: the sender's bus and the
//     message id's bus are both "A".
//   - Without check 1, this function verifies A.alice's attestation against the
//     pin — which succeeds, it is genuine — and returns K_alice.
//   - The caller then verifies the message signature over Sender "A.bob" under
//     K_alice. The signature WAS made with K_alice over exactly those bytes, so
//     it verifies.
//
// The receiving bus attributes the message to A.bob and NOTHING can detect it:
// it holds no roster of A's agents, it routes only to direct peers, and an
// intermediate is structurally forbidden from telling it anything about A's
// namespace. The MESSAGE SIGNATURE DOES NOT SAVE YOU — it covers Sender, but
// the KEY IT IS CHECKED AGAINST is what this check selects.
//
// Check 2 is what ties the pin set to the subject: the caller looked the pins up
// by want.OriginBus, so without it a peer could present a validly-signed
// attestation minted by a DIFFERENT bus we also pin.
//
// Check 4 is a MUST, not a SHOULD. Revocation across a non-adjacent link is
// UNSOLVED, so NotAfter is the ONLY bound on a compromised agent messaging key;
// an implementer who treats it as advisory makes every attestation eternal.
//
// # THE ORDER IS PART OF THE CONTRACT
//
//  0. Every pin is present and well-formed — ErrUnpinned, refused not skipped.
//  1. A verification clock was supplied — ErrNoClock. A LOCAL fault, so it has
//     a sentinel of its own rather than one that reads as "the peer sent
//     rubbish".
//  2. The subject the caller named is bounded, so the refusal at step 5 may
//     quote BOTH sides of the comparison, not only the attestation's half.
//  3. The COVERED BYTES ARE SNAPSHOTTED, once. Everything below is derived
//     from that snapshot and nothing is re-read from the caller's blob.
//  4. The signature is present and 64 bytes — before any key is touched.
//  5. The snapshot canonicalizes — which is what BOUNDS its agent id, so that
//     every error below may safely quote it.
//  6. Binding check 1, then binding check 2.
//  7. Binding check 3: one ed25519.Verify per pin, until one succeeds.
//  8. Binding check 4: expiry — AFTER the signature, deliberately. If expiry
//     came first, a peer could present arbitrary unsigned garbage with an old
//     timestamp and be told "expired", so ErrExpired would stop meaning "a
//     genuinely bus-signed attestation grew old" — which is the whole reason it
//     is a separate sentinel.
//  9. Check 5: the lifetime ceiling — ErrLifetimeExceeded. Also after the
//     signature, and for the same reason: it must only ever name a genuinely
//     bus-signed attestation, never unsigned garbage carrying a far-future
//     timestamp.
//
// # THE RETURNED KEY IS THE KEY THE SIGNATURE COVERED
//
// Step 3's snapshot is what makes that sentence true rather than merely
// plausible. a.MessagingPublicKey is a slice, so it may be a window onto a
// decoded wire payload that another goroutine still holds; canonicalizing from
// the live array and then RE-READING it to build the return value would leave a
// gap in which the bytes that were verified and the bytes that are returned are
// not the same bytes. The security gate demonstrated exactly that against an
// earlier draft of this function (RELAY-14, 2026-08-08 P2-1): a write landing in
// that gap made Verify return an attacker-chosen key with a nil error. One
// snapshot, taken before the canonical bytes are derived and returned unchanged,
// closes it — and it is the same rule PROTOCOL.md §8.5 states for the message
// path: act on the bytes you re-derived, never on ones that arrived beside them.
//
// # The pins are consumed ENTIRELY here, and must never be tried against the MESSAGE
//
// The obvious wrong reading of "there may be several pins" is to try them
// against the message signature too. That would verify an AGENT's message with a
// BUS's signing key, which is a category error. Exactly one messaging key comes
// out of this function, and the caller runs exactly one message verification
// with it.
func Verify(pins []ed25519.PublicKey, a Attestation, want Subject, now time.Time) (ed25519.PublicKey, error) {
	// 0. The pins, first and unconditionally. An origin bus we hold no pin for
	// is refused HERE, before anything can look at the blob — which is what
	// makes "unpinned means unverifiable" structural rather than a claim in
	// prose.
	if len(pins) == 0 {
		return nil, fmt.Errorf("%w: no pinned signing key was supplied for the origin bus; pin that bus's signing key out of band — there is deliberately no trust-on-first-use fallback", ErrUnpinned)
	}
	for i, pin := range pins {
		if err := signing.ValidatePublicKey(pin); err != nil {
			return nil, fmt.Errorf("%w: pin %d is unusable (%v); a malformed pin is refused rather than skipped, because skipping it would verify against less than the operator believes is pinned", ErrUnpinned, i, err)
		}
	}

	// 1. A zero clock is a REFUSAL, not a permissive default. Expiry is a MUST
	// and the only bound on a compromised key; a caller that forgot to supply a
	// clock would otherwise get an unbounded-lifetime attestation and no
	// indication anything was wrong.
	if now.IsZero() {
		return nil, fmt.Errorf("%w: expiry is enforced rather than optional, so a verification clock is required", ErrNoClock)
	}

	// 2. The CALLER's half of binding check 1, bounded before step 6 quotes it.
	// ids.ParseAgentID bounds the attestation's half; without this line the
	// bound-before-quote discipline would hold on one side of a two-sided
	// comparison only. The id is not echoed here, for the same reason
	// ids.ParseAgentID does not echo an oversized one.
	if len(want.FQAgentID) > ids.MaxAgentIDLen {
		return nil, fmt.Errorf("%w: the subject the caller named is %d bytes, and an agent id is at most %d; it is not echoed here because it is oversized", ErrInvalid, len(want.FQAgentID), ids.MaxAgentIDLen)
	}

	// 3. THE SNAPSHOT. Every check below, and the returned key, is derived from
	// `checked` and never re-read from `a` — see "THE RETURNED KEY IS THE KEY
	// THE SIGNATURE COVERED" above. AgentID and the integers are value types and
	// are copied by the struct assignment; the two slices are copied explicitly.
	// A nil slice stays nil, so a missing key or signature is still caught by
	// the checks below rather than silently becoming an empty one.
	checked := a
	checked.MessagingPublicKey = append(ed25519.PublicKey(nil), a.MessagingPublicKey...)
	checked.Signature = append([]byte(nil), a.Signature...)

	// 4. The signature is present and the right length, BEFORE any verification.
	// The check is internal/signing's and not a second copy of it.
	if err := signing.ValidateSignature(checked.Signature); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	// 5. The bytes we are about to check, RE-DERIVED from the fields we will act
	// on — never a blob that arrived beside them. This is also what BOUNDS and
	// validates the agent id, so it must precede every error below that quotes
	// it.
	canonical, err := Canonicalize(checked)
	if err != nil {
		return nil, err
	}

	// 6. BINDING CHECK 1 — LOAD-BEARING. See the doc comment above: without
	// this line, one compromised agent messaging key impersonates every other
	// agent on that bus. subtle.ConstantTimeCompare is used for uniformity of
	// treatment of identity material, not because the ids are secret — they are
	// public — and it also refuses a length mismatch, so it cannot be satisfied
	// by a prefix.
	if len(checked.AgentID) != len(want.FQAgentID) || subtle.ConstantTimeCompare([]byte(checked.AgentID), []byte(want.FQAgentID)) != 1 {
		// Both operands are bounded: the attestation's by step 5, the caller's
		// by step 2.
		return nil, fmt.Errorf("%w: the attestation names %q but the message is attributed to %q; an attestation authorises exactly one agent", ErrAgentIDMismatch, checked.AgentID, want.FQAgentID)
	}

	// 7. BINDING CHECK 2 — the subject's bus half is the bus whose pins we were
	// handed. Step 5 already parsed the id successfully, so this cannot fail.
	subjectBus, _, _, err := ids.ParseAgentID(checked.AgentID)
	if err != nil {
		return nil, fmt.Errorf("%w: agent id: %v", ErrInvalid, err)
	}
	if subjectBus != want.OriginBus {
		return nil, fmt.Errorf("%w: the attestation's subject %q belongs to bus %q, but the pins are for origin bus %q", ErrOriginBusMismatch, checked.AgentID, subjectBus, want.OriginBus)
	}

	// 8. BINDING CHECK 3 — the pins, and nothing else. One high-level
	// ed25519.Verify per pin; more than one pin exists only for a rollover
	// window. Every pin was length-checked in step 0, so no call here can panic.
	verified := false
	for _, pin := range pins {
		if ed25519.Verify(pin, canonical, checked.Signature) {
			verified = true
			break
		}
	}
	if !verified {
		// Neither the signature nor any pin is echoed: the pair is what an
		// attacker chooses, and there is no diagnosis in either. The subject is
		// enough to find the event in the audit trail.
		return nil, fmt.Errorf("%w: subject %q, %d pin(s) tried", ErrVerify, checked.AgentID, len(pins))
	}

	// 9. BINDING CHECK 4 — expiry, enforced. Deliberately after the signature, so
	// ErrExpired only ever names a genuinely bus-signed attestation.
	notAfter := time.UnixMilli(checked.NotAfterUnixMilli)
	if now.Add(-ClockSkewAllowance).After(notAfter) {
		return nil, fmt.Errorf("%w: subject %q, not-after %s, now %s (allowing %s of clock skew); an intermediate forwards verbatim and cannot re-mint, so this is far more often a queued message than a forgery", ErrExpired, checked.AgentID, notAfter.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), ClockSkewAllowance)
	}

	// 10. CHECK 5 — the VERIFIER-SIDE lifetime ceiling (RELAY-28). NotAfter was
	// minted by the origin bus and, with revocation across a non-adjacent link
	// unsolved, is the ONLY bound on a compromised agent messaging key. A minter
	// that writes an absurd NotAfter would make that key eternal; this bus refuses
	// to be bound by a number it did not compute and caps the exposure itself.
	//
	// The measure is NotAfter - now, from THIS bus's own clock, not
	// NotAfter - IssuedAt: IssuedAt is minter-controlled too, so a far-future
	// NotAfter beside a far-future IssuedAt would keep that window small and slip
	// past. now is ours and nothing in the blob can move it. time.Time.Sub
	// saturates to the maximum Duration rather than overflowing, so even a
	// year-292278994 NotAfter yields a huge positive remaining and is refused here
	// rather than wrapping negative. Strict ">", so a window sitting exactly at the
	// ceiling still verifies. Placed after the signature so ErrLifetimeExceeded, like
	// ErrExpired, only ever names a genuinely bus-signed attestation.
	if remaining := notAfter.Sub(now); remaining > MaxAttestationLifetime {
		return nil, fmt.Errorf("%w: subject %q, not-after %s, now %s, remaining %s exceeds the %s ceiling this verifier enforces regardless of the minter's not-after", ErrLifetimeExceeded, checked.AgentID, notAfter.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), remaining, MaxAttestationLifetime)
	}

	// The snapshot itself, which nothing else holds a reference to. It is the
	// exact key material step 5 canonicalized and step 8 verified.
	return checked.MessagingPublicKey, nil
}
