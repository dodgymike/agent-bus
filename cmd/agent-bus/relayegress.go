package main

// relayegress.go is the EGRESS half of federation: the adapter that turns a
// message this bus has just committed into the cross-bus envelope
// relay.Forwarder sends to a peer (RELAY-24-BLOCKER-EGRESS).
//
// # WHY IT LIVES IN cmd/ AND NOT IN internal/hub
//
// Deliberately, and it is not a layering accident. Building the envelope needs
// two things the hub does not have and must not acquire:
//
//   - the SENDER'S MESSAGING PUBLIC KEY, which lives on the enrolment roster
//     (auth.RosterEntry.MessagingPublicKey). hub.Agent does not expose it, and
//     widening hub.RosterSource to carry it would push a relay concern into the
//     one interface every read path in the hub goes through.
//   - the BUS SIGNING KEY (buscert.Material.SigningPrivateKey), which mints the
//     origin attestation. A hub that held the bus's signing key would be a hub
//     that can speak for the bus.
//
// The composition root is the only place that holds the roster, the key material
// and the hub at the same time, so the mapping belongs here — the same argument
// that moved hub.Open itself into main.
//
// # STATUS: WIRED. IT RUNS ON EVERY LOCAL SEND ON A BUS WITH A PEER STORE
//
// This header said "NOT WIRED IN THIS BUILD" until the outbound TLS blocker was
// resolved, and a status comment that survives the change it describes is worse
// than none — so it is corrected rather than left standing. main.go constructs
// this adapter, hands it to hub.Options.Egress alongside the registry as
// RemoteRouter, and hub.publish calls Forward on every committed message.
//
// The blocker was real and is worth recording, because the shape of the answer
// is the reusable part. relay.PeerRecord.NextHopTLSCertFingerprint is a 32-byte
// pin of a SELF-SIGNED certificate with no CA anywhere (invariant 11), and
// crypto/tls supports exactly ONE way to check such a pin: disable the default
// chain check and supply VerifyPeerCertificate. That literal is permitted in
// exactly one file in this repo (client/pin.go), and writing it again here would
// be a SECOND occurrence — which invariant 11 refuses on its own terms.
//
// It would ALSO not compile past this package's own guard. cmd/agent-bus is not
// unscanned: scanPlaintextListener (cmd/agent-bus/tlslisten_test.go, driven by
// TestCmdHasNoPlaintextListener) walks every non-test .go file HERE and flags the
// identifier OUTRIGHT — no paired-VerifyPeerCertificate exception, which makes it
// STRICTER than client/guard_test.go's AST rule. internal/relay is the genuinely
// unscanned direction, and that is the one this file's resolution avoids.
//
// The resolution adds ZERO new occurrences: client/pin.go EXPORTS its existing
// pinnedTLSConfig as client.PinnedTLSConfig, and cmd/agent-bus/relaydial.go
// calls it, resolving the pin by the ADDRESS being dialled. See DECISIONS.md
// (2026-08-15).

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/store"
)

// egressRoster is the read-through view of the enrolment roster this adapter
// needs: one lookup, for the sender's messaging public key and enrolment epoch.
//
// It is an interface rather than *auth.WALRoster so the adapter is constructible
// in a test without a durable roster — and so that the ONE fact it reads is
// visible in the type rather than buried in a concrete dependency.
type egressRoster interface {
	Get(agentID string) (auth.RosterEntry, bool)
}

// egressRouter is the ROUTING TABLE, read for one purpose only: to answer
// "could this message possibly route to a peer?" before an ed25519 attestation
// is minted for it under the hub's write lock.
//
// *relay.Registry satisfies it, and the composition root passes the SAME
// registry instance the forwarder routes on — see routesToSomePeer, which
// depends on that for its superset property.
type egressRouter interface {
	Route(agentID string) (string, bool)
	BroadcastTargets(busPath []string) []string
}

// relayEnqueuer is the forwarder seam: the one method this adapter calls.
//
// *relay.Forwarder satisfies it. It is an interface here for the same reason
// egressRoster is one: the adapter must be exercisable without a network, and
// relay.Forwarder's own constructor requires a TLS-configured client.
type relayEnqueuer interface {
	// Enqueue offers the envelope to every peer it routes to and returns how
	// many queues accepted it. It NEVER WAITS ON A NETWORK PEER — every queue
	// send is a select with a default arm — and never reports a failure of the
	// local send. See relay.Forwarder.Enqueue, and hub.forwardOnward for what
	// that guarantee does and does not cover (it is not "does no I/O": the
	// durable outbox record is fsynced inside this call).
	Enqueue(m relay.RelayedMessage) (int, error)
}

// relayEgressOptions configures newRelayEgress. Every field is REQUIRED and
// every one is checked: an adapter assembled with a piece missing would look
// healthy and silently forward nothing, which is the failure this repo keeps
// paying for.
type relayEgressOptions struct {
	// BusID is THIS bus's id. It is the attesting bus and the origin bus.
	BusID string

	// SigningKey is this bus's BUS SIGNING key —
	// buscert.Material.SigningPrivateKey — which mints the origin attestation.
	// It is NOT the TLS key and the two are never conflated.
	SigningKey ed25519.PrivateKey

	// Roster is the live enrolment roster, read through on every forward rather
	// than snapshotted: an agent that re-keys must be attested under its NEW
	// key, and a snapshot would attest the old one for the life of the process.
	Roster egressRoster

	// Forwarder is the cross-bus delivery path.
	Forwarder relayEnqueuer

	// Router is the routing table the forwarder itself routes on. It MUST be
	// the same instance: routesToSomePeer's safety argument is that it can only
	// ever be MORE permissive than relay.Forwarder.targets, and two tables could
	// disagree in the other direction.
	Router egressRouter

	// Logger is optional; nil discards.
	Logger *logging.Logger

	// Now supplies the attestation clock. nil means time.Now.
	Now func() time.Time
}

// relayEgress implements hub.Egress: it is handed every message this bus
// commits and offers the forwardable ones to the forwarder.
type relayEgress struct {
	busID      string
	signingKey ed25519.PrivateKey
	roster     egressRoster
	forwarder  relayEnqueuer
	router     egressRouter
	log        *logging.Logger
	now        func() time.Time
}

// It must satisfy hub.Egress, and that is asserted at COMPILE time rather than
// discovered at the wiring call: the seam is optional, so a signature drift
// would otherwise show up as "federation quietly does nothing".
var _ hub.Egress = (*relayEgress)(nil)

// newRelayEgress validates opts and returns the adapter.
func newRelayEgress(opts relayEgressOptions) (*relayEgress, error) {
	if err := ids.ValidateBusID(opts.BusID); err != nil {
		return nil, fmt.Errorf("relay egress: bus id: %w", err)
	}
	if len(opts.SigningKey) != ed25519.PrivateKeySize {
		// The LENGTH is reported and the key is not, and must never be: a
		// private key that reaches a log line or an error has left the machine.
		// attest.Sign makes the same check for the same reason (ed25519.Sign
		// PANICS on a wrong-size key); making it here too means a mis-wired
		// composition root fails at STARTUP rather than on the first message.
		return nil, fmt.Errorf("relay egress: the bus signing key is %d bytes, want exactly %d; without it no origin attestation can be minted and relay.VerifyRelayed refuses an envelope carrying none", len(opts.SigningKey), ed25519.PrivateKeySize)
	}
	if opts.Roster == nil {
		return nil, errors.New("relay egress: a roster is required; it is the only source of the sender's MESSAGING public key, and an attestation cannot bind an agent to a key nobody looked up")
	}
	if opts.Forwarder == nil {
		return nil, errors.New("relay egress: a forwarder is required; an egress adapter with nowhere to hand a message would accept every send and forward none, which is indistinguishable from a healthy federated bus from the outside")
	}
	if opts.Router == nil {
		return nil, errors.New("relay egress: a router is required; without one this adapter would mint an ed25519 origin attestation under the hub's write lock for every local send, including the overwhelming majority that route to no peer at all")
	}
	e := &relayEgress{
		busID:      opts.BusID,
		signingKey: opts.SigningKey,
		roster:     opts.Roster,
		forwarder:  opts.Forwarder,
		router:     opts.Router,
		log:        opts.Logger,
		now:        opts.Now,
	}
	if e.log == nil {
		e.log = logging.New(discardWriter{}, logging.LevelError)
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e, nil
}

// Forward implements hub.Egress. It is called on the send path with the hub's
// write lock held, AFTER the message is durable and after local readers have
// been woken.
//
// IT NEVER RETURNS AN ERROR AND NEVER WAITS ON A NETWORK PEER. The local send is
// already acknowledged by its own durable write (invariant 4); every failure
// below is a failure of the CROSS-BUS HOP alone and is counted and logged rather
// than propagated. See hub.Hub.forwardOnward for the full argument, including
// the at-most-once window this seam deliberately leaves open.
//
// "NEVER BLOCKS" WOULD BE THE WRONG WORD AND IS NOT USED: the guarantee is about
// PEERS, not about DISK. relay.Forwarder.Enqueue writes a durable outbox record
// per target — two fsyncs each — before it returns, and that happens under
// writeMu. hub.forwardOnward states the same distinction; keep the two wordings
// matched.
func (e *relayEgress) Forward(m store.Message) {
	// A RELAYED MESSAGE IS NOT THIS SEAM'S BUSINESS, and forwarding one here
	// would be actively wrong rather than merely redundant.
	//
	// hub.publish serves BOTH locally-originated sends and relay INGEST (see
	// publishRequest.relayed). An ingested message has already traversed a path;
	// rebuilding it here would claim OUR bus as its origin, mint an attestation
	// for an agent in SOMEONE ELSE'S namespace (which attest.Sign refuses
	// outright, byte for byte — invariant 2), and hand AppendHop an empty path,
	// erasing the loop-prevention history that made the hop safe.
	//
	// Carrying an ingested message further is relay.AcceptOptions.Onward's job,
	// which is a different seam with a different envelope. Declining here is
	// therefore the LEAF configuration newFederation already documents, not a
	// gap: nothing is lost that this seam was ever asked to carry.
	//
	// # THE GATE IS THE BUS PATH, BECAUSE THAT IS THE FIELD PRODUCTION SETS
	//
	// It USED to be `m.OriginMessageID != ""`, and that check was DEAD CODE:
	// nothing in this build ever sets store.Message.OriginMessageID — the origin
	// id rides on hub's internal publishRequest, for the audit content hash only
	// (hub/relayingest.go), and Message.WithOriginMessageID has no non-test
	// caller tree-wide. Every relay-ingested message therefore reached envelope()
	// and attest(), and only failed closed by ACCIDENT further down (see attest).
	// A control that cannot fire is worse than no control, because it is read as
	// one.
	//
	// store.Message.BusPath IS always set, by the only two constructors there
	// are: store.NewMessage builds LocalBusPath(busID) — exactly [our bus] — for
	// a local send, and store.NewMessageWithBusPath records the received path
	// with our hop APPENDED for an ingest, where hub.relayedBusPath has already
	// refused an empty path and refused any path we already appear in. So
	// "hop zero is us" is true of every locally-originated message and false of
	// every ingested one, and it is not derivable from anything a client sends.
	//
	// An EMPTY path is structurally unreachable (neither constructor can produce
	// one, and Decode refuses one off disk) and is DECLINED rather than assumed
	// local: a path-less message names no origin, and forwarding one would assert
	// a provenance nobody recorded. It is logged, and it is not remote-reachable
	// — an ingest always carries at least two hops.
	if len(m.BusPath) == 0 {
		e.log.Warn("a committed message carries NO bus path, so this bus cannot tell whether it originated here; it will NOT be forwarded to any peer. The message is durable and was delivered to local recipients",
			"message_id", m.ID,
			"seq", m.Seq,
			"sender", m.Sender,
		)
		return
	}
	if !strings.EqualFold(m.BusPath[0], e.busID) {
		return
	}
	// A BELT, AND HONESTLY LABELLED AS ONE. Nothing sets this field today, so
	// this line cannot fire; it is kept because the day something does set it,
	// the value means "this message came from elsewhere" and the bus path may
	// not yet have caught up. It is NOT the control — the bus path above is.
	if m.OriginMessageID != "" {
		return
	}

	// A CONSERVATIVE PRE-CHECK, NOT A SECOND ROUTING AUTHORITY. See
	// routesToSomePeer: it exists only to avoid minting an ed25519 attestation
	// under the hub's write lock for a send that provably routes to nobody.
	if !e.routesToSomePeer(m) {
		return
	}

	env, err := e.envelope(m)
	if err != nil {
		// FAIL CLOSED, LOUDLY, AND WITHOUT TOUCHING THE LOCAL SEND. The message
		// is durable and was delivered to every LOCAL recipient; what is lost is
		// the cross-bus copy, and invariant 6 requires that discard be specific
		// rather than silent. The remedy is named because the overwhelmingly
		// likely cause is an agent enrolled without a messaging public key —
		// auth.RosterEntry.MessagingPublicKey is OPTIONAL, so this is a
		// legitimate state and not a corrupted roster.
		e.log.Warn("a locally-published message will NOT be forwarded to any peer: its cross-bus envelope could not be built. The message is durable on this bus and was delivered to local recipients; only the cross-bus copy is lost, and no restart brings it back",
			"message_id", m.ID,
			"seq", m.Seq,
			"sender", m.Sender,
			"err", err.Error(),
			"remedy", "an agent with no MESSAGING public key on the roster cannot be attested to a peer bus (the key is optional at enrolment). Re-enrol that agent presenting its messaging public key; nothing is fabricated in its place and no unattested envelope is ever sent",
		)
		return
	}

	// THE FORWARDER IS STILL THE ONLY ROUTING AUTHORITY. Forwarder.Enqueue
	// resolves the targets itself (relay.Forwarder.targets), counts every
	// no-route and every loop drop, and returns (0, nil) for "this message routes
	// to no peer". routesToSomePeer above does NOT decide anything it decides: it
	// is a strictly wider filter that only ever suppresses a MINT, and every
	// message it lets through is routed here, from scratch, by the forwarder.
	//
	// (An earlier version of this comment said no pre-check of any kind may
	// exist, on the grounds that a second answer could drift from the first. That
	// argument holds for a pre-check that could answer NO where the forwarder
	// would answer YES — which is exactly the shape routesToSomePeer is built to
	// exclude — and it was costing an ed25519 signature under the hub's global
	// write lock on every purely local send.)
	//
	// The only error Enqueue can return is ErrForwarderClosed — a shutdown
	// condition on THIS bus, never a verdict on the message.
	if _, err := e.forwarder.Enqueue(env); err != nil {
		e.log.Warn("a locally-published message was not offered to the cross-bus forwarder; the forwarder is shutting down. The message is durable on this bus and was delivered to local recipients",
			"message_id", m.ID,
			"sender", m.Sender,
			"err", err.Error(),
		)
	}
}

// routesToSomePeer is a CONSERVATIVE GATE, not a routing decision: it reports
// whether this message could possibly have a peer target, and its only job is to
// stop an ed25519 attestation being minted for a send that provably has none.
//
// # WHY IT EXISTS
//
// Forward runs inside hub.publish with writeMu — the hub's GLOBAL write lock —
// held. Building the envelope calls attest.Sign, an ed25519 signature measured
// at ~29 µs on this path, and until this gate existed that cost was paid on
// EVERY local send, including the overwhelming majority addressed only to agents
// on this bus, because the envelope was built BEFORE Enqueue was ever consulted.
//
// # THE SAFETY PROPERTY, WHICH IS THE WHOLE DESIGN
//
// This predicate is a STRICT SUPERSET of relay.Forwarder.targets: everything
// targets would route, this returns true for. A disagreement can therefore only
// ever cost an UNNECESSARY MINT, never a SKIPPED FORWARD — the failure that
// would matter is structurally excluded rather than tested for. Two properties
// give it:
//
//   - THE SAME TABLE. relayEgressOptions.Router is the very *relay.Registry the
//     forwarder routes on (the composition root passes one instance), so Route
//     cannot answer differently here.
//   - THE SPLIT HORIZON IS OMITTED, DELIBERATELY. targets also applies
//     NextHopAllowed, which can only ever REMOVE targets. Not applying it here
//     keeps this side wider by construction. (On this path the envelope's
//     BusPath is empty anyway, so it removes nothing today — but relying on that
//     would make the superset property depend on a value set somewhere else.)
//
// Nothing here is authoritative: a message that passes is routed again, from
// scratch, by Enqueue, and every drop is counted there.
func (e *relayEgress) routesToSomePeer(m store.Message) bool {
	if m.Broadcast {
		// An empty path, matching the envelope this seam builds — and matching
		// the wider side of the argument above, since a longer path can only
		// remove peers.
		return len(e.router.BroadcastTargets(nil)) > 0
	}
	for _, r := range m.Recipients {
		// Registry.Route already excludes this bus's own agents and anything
		// that does not parse, so a purely local send answers false for every
		// recipient without a single allocation past the parse.
		if _, ok := e.router.Route(r); ok {
			return true
		}
	}
	return false
}

// envelope maps a committed local message onto the cross-bus envelope, minting
// the origin attestation that goes with it.
//
// It is separated from Forward so the mapping can be asserted directly: every
// field below is either copied from the message or derived from this bus's own
// key material, and getting any one of them wrong fails SILENTLY at the far end
// (a signature check that simply returns false, for every message, for ever).
func (e *relayEgress) envelope(m store.Message) (relay.RelayedMessage, error) {
	att, err := e.attest(m.Sender)
	if err != nil {
		return relay.RelayedMessage{}, err
	}
	return relay.RelayedMessage{
		// WE ARE THE ORIGIN. The message id is the one THIS bus minted
		// (invariant 1: server-authoritative, never adopted from anywhere), and
		// store.Message.OriginMessageID is empty on a locally-originated
		// message precisely because ID already IS the origin id.
		OriginBus:       e.busID,
		OriginMessageID: m.ID,
		OriginSeq:       m.Seq,
		Sender:          m.Sender,
		Broadcast:       m.Broadcast,
		Recipients:      append([]string(nil), m.Recipients...),

		// BusPath IS DELIBERATELY EMPTY, AND THIS IS THE TRAP ON THIS PATH.
		//
		// RelayedMessage.BusPath is the path AS RECEIVED, NOT including this
		// bus: Forward(localBusID) appends our hop via AppendHop, whose doc
		// names an empty input path as the ONE legal empty case, meaning "this
		// bus is the ORIGIN".
		//
		// store.Message.BusPath on a locally-originated message is
		// store.LocalBusPath(busID) — that is, [busID], OUR OWN HOP ALREADY IN
		// IT. Copying it here would hand AppendHop a path it is already on, so
		// every single forward would come back ErrRelayLoop and be dropped, on a
		// bus whose logs would say "loop" about a message that had never left.
		// Leaving it nil is not an omission; it is the value.
		BusPath: nil,

		// THE SIGNED TIMESTAMP, NOT THIS BUS'S CLOCK. store.Message.SigningMessage
		// is the one place the mapping from a stored message to the bytes its
		// Signature covers is written down, and it names TimestampUnixMilli.
		// Substituting SentAt (this bus's acceptance clock, deliberately NOT
		// covered by the signature) would make every relayed signature fail to
		// verify at the far end, silently and universally.
		TimestampUnixMilli: m.TimestampUnixMilli,

		OriginAttestation: att,

		// COPIED, NOT ALIASED — and the copy is not defensive style, it is a
		// known defect being worked around: store.copyMessage does NOT deep-copy
		// Signature (RELAY-24-FU-STOREMSGLOOKUP-SIGCOPY), so the slice handed to
		// us may share a backing array with the serving copy. An envelope whose
		// signature bytes can change under it after it was built is the
		// time-of-check/time-of-use shape relay guards against everywhere else.
		Signature: append([]byte(nil), m.Signature...),
		Body:      append([]byte(nil), m.Body...),

		// Re-derived by the receiving bus and checked against what we declare;
		// the stored value is the same hash over the same bytes.
		ContentSHA256: m.ContentSHA256,

		// THE RELAY IDEMPOTENCY KEY IS THE ORIGIN MESSAGE ID, and it is not a
		// choice: ValidateRelayRequest REFUSES an envelope whose key differs.
		// That equality is what makes two copies of one message arriving by two
		// disjoint paths land on ONE idem.Scope at the far end (invariant 10).
		IdempotencyKey: m.ID,

		// Fingerprint is left ZERO, and that is checked rather than assumed:
		// relayFingerprint is unexported, so this package cannot compute the
		// canonical value, and computing a DIFFERENT one would be worse than
		// none. Nothing on the egress path reads it — RelayedMessage.Forward
		// does not, and neither does Forwarder.Enqueue or targets — because the
		// receiving bus derives it itself inside ValidateRelayRequest, which is
		// the only place it may come from anyway.
	}, nil
}

// attest mints the ORIGIN ATTESTATION for a locally-enrolled sender: this bus's
// signed statement binding that agent id to the messaging public key a peer must
// verify its message under.
//
// It is on the CRITICAL PATH, not an optional decoration: relay.VerifyRelayed
// refuses an envelope carrying a zero attestation with ErrMissingAttestation, so
// a message without one cannot be relayed at all.
//
// Invariant 9: attest.Sign and NOTHING ELSE. No key derivation, no framing, no
// second construction of the canonical bytes.
func (e *relayEgress) attest(agentID string) (attest.Attestation, error) {
	entry, ok := e.roster.Get(agentID)
	if !ok {
		// Structurally unreachable for a LOCAL send — hub.publish validated the
		// sender against this same roster before it took the write lock — but a
		// roster read is not held across that window, so it is checked rather
		// than assumed.
		//
		// IT IS ALSO THE SECOND LINE OF DEFENCE AGAINST RE-ATTESTING A FOREIGN
		// SENDER, NOT THE FIRST. The first is Forward's bus-path gate, which
		// declines an ingested message before this function is reached at all.
		// This one, and attest.Sign's own refusal to sign a subject whose bus
		// half is not e.busID (invariant 2), are what would catch a foreign
		// sender if that gate were ever removed — good defence in depth, and
		// deliberately not the control anything is documented to rely on.
		return attest.Attestation{}, fmt.Errorf("sender %q is not on this bus's enrolment roster, so nothing can be attested for it", agentID)
	}
	if len(entry.MessagingPublicKey) != ed25519.PublicKeySize {
		// A LEGITIMATE STATE, NOT A CORRUPTION: auth.RosterEntry.MessagingPublicKey
		// is OPTIONAL and an enrolment may carry none. Such an agent cannot be
		// attested, so its message cannot be relayed — and the three wrong
		// answers are named so nobody supplies one later: do NOT fabricate a
		// key, do NOT send an unattested envelope (the peer would refuse it and
		// the refusal would be indistinguishable from an attack), and do NOT
		// fail the local send, which has already been acknowledged.
		return attest.Attestation{}, fmt.Errorf("agent %q has no messaging public key on the roster (it holds %d bytes, want %d), so this bus cannot attest it to a peer", agentID, len(entry.MessagingPublicKey), ed25519.PublicKeySize)
	}

	// THE KEY EPOCH IS THE ENROLMENT EPOCH IN UNIX MILLISECONDS. That is a
	// design call and is recorded as one (DECISIONS.md, 2026-08-15).
	//
	// attest.Attestation.KeyEpoch is an unvalidated uint64 whose only
	// requirement is that the ORIGIN bus assigns it, and auth.RosterEntry.Epoch
	// is a time.Time documented as bumpable on a future re-key. Unix
	// milliseconds of that instant is therefore monotone per re-key by
	// construction and needs no second counter to maintain, no durable record of
	// its own, and no reconciliation after a restart.
	//
	// A ZERO OR PRE-1970 EPOCH IS RECORDED AS 0 rather than converted: uint64 of
	// a negative int64 wraps to a value near 2^64, which is an epoch NO later
	// re-key could ever exceed — a monotonicity inversion produced by a cast,
	// which is exactly the class of bug that is invisible until it matters.
	var epoch uint64
	if ms := entry.Epoch.UnixMilli(); ms > 0 {
		epoch = uint64(ms)
	}

	// NOT-AFTER IS DERIVED FROM THE MAXIMUM RELAY RETRY WINDOW, WHICH IS WHAT
	// attest.Sign'S OWN DOC REQUIRES — not a plausible-sounding constant. An
	// intermediate forwards VERBATIM and cannot re-mint, so anything that can
	// still be queued when this expires becomes permanently undeliverable.
	//
	// relay.RetryHorizonCeiling IS idem.PeerOutageBudget, and the forwarder's
	// last attempt cannot LEAVE after issuedAt + (ceiling - timeout) and takes at
	// most timeout, so it arrives by issuedAt + ceiling. This value is that
	// bound exactly.
	//
	// IT CONTAINS NO ALLOWANCE FOR CLOCK SKEW BETWEEN BUSES, and none is
	// invented here: attest.Verify applies its own ClockSkewAllowance (5
	// minutes) on the far side, so the margin belongs to the verifier — which is
	// the only end that knows how far its clock has drifted — rather than being
	// padded into every attestation this bus mints.
	issuedAt := e.now()
	notAfter := issuedAt.Add(relay.RetryHorizonCeiling)

	return attest.Sign(e.signingKey, e.busID, agentID, entry.MessagingPublicKey, epoch, issuedAt, notAfter)
}
