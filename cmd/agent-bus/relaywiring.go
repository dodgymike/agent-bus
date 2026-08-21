package main

// THE FEDERATION COMPOSITION ROOT (RELAY-24).
//
// internal/relay holds the handlers, internal/httpapi holds the mount, and
// neither may reach the other's half: httpapi imports relay, and
// internal/relay/guards_test.go forbids the reverse. This file is the one place
// that holds BOTH, plus the hub, plus the durable peer store — so it is the one
// place the federation ingress can actually be assembled.
//
// # WHAT IS WIRED HERE, AND WHAT IS DELIBERATELY NOT
//
// WIRED: the INGRESS. The three peer handlers, the acceptor behind
// RelayConfig.AcceptRelay, a relay.LocalIngest over hub.IngestRelayed, the
// registry the handlers' callbacks mutate, and the peer store serving both
// httpapi.Options.PeerPrincipals (inbound identity) and PeerSurface.Trust
// (origin signing-key pins).
//
// ALSO WIRED, AS OF RELAY-47: the ONWARD hop. federationOptions.Onward carries
// the relay.Forwarder the composition root builds for the egress half, so a
// message this bus accepts for an agent on a THIRD bus is now carried further
// instead of stopping here. This paragraph said "NOT WIRED: the EGRESS ...
// AcceptOptions.Onward is nil" and that was true when written; a status comment
// that survives the change it describes is worse than none.
//
// NIL IS STILL LEGAL AND STILL MEANS LEAF. A bus built without a peer store has
// no forwarder to pass, and AcceptOptions.Onward documents nil as a legitimate
// configuration rather than an omission: such a bus accepts relayed mail for its
// OWN agents and carries nothing further. The difference between the two is
// reported at startup (main.go, `onward_relay`) and in the log line
// warnIfCarriedNoFurther emits, which is the one place that can tell "leaf by
// design" from "egress not wired".
//
// # THE FIVE THINGS THIS FILE OWES THAT ARE NOT "PLUMBING"
//
// httpapi.PeerBusIDFromContext is unreachable from internal/relay by
// construction, so every check that needs the AUTHENTICATED PEER'S IDENTITY has
// to happen here, in the callbacks, or nowhere:
//
//  1. THE APPLIED-KEY TABLE IS METERED BY THE PROVEN PEER, not by the
//     peer-asserted sender label (RELAY-FU-IDEM-METER-BY-PEER). See
//     peerAdmission.
//  2. EVERY CLAIMED ID IS BOUND TO THE CONNECTION: PeerEnrollRequest.BusID,
//     RosterUpdate.BusID and the last hop of BusPath. See checkPeerAssertsOwnID
//     and checkPeerIsLastHop.
//  3. THERE IS A CONCURRENCY CAP AND A QUOTA ON THE RELAY PATH (RELAY-22).
//     Before this, relayed traffic met no bound of any kind.
//  4. idem.Outcome IS CARRIED UNCOLLAPSED across the hub seam. Its zero value is
//     OutcomeNew — the answer that RE-FORWARDS — so hubIngest sets it explicitly
//     on every return path, including the error path.
//  5. NO ROUTE IS REGISTERED HERE. This file names no peer path and touches no
//     mux; it hands httpapi.Options.Peer a complete surface and lets
//     mountPeerSurface decide. A registration here would evade the mount guard.
//
// # THE EXPLICIT peerBusID PARAMETER IS THE POINT, NOT A STYLE CHOICE
//
// Each callback is two functions: a thin adapter that reads the peer principal
// out of the request context, and the DECISION function that takes it as a
// required parameter. A context read can be silently forgotten by a later edit
// and everything still compiles; a missing parameter does not. The decision
// functions are also what the tests drive, so the binding rules are provable
// without standing up TLS.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// The local bus, as the relay ingress needs it
// ---------------------------------------------------------------------------

// hubIngest adapts *hub.Hub to relay.LocalIngest.
//
// It is fifteen lines here rather than a dependency either way round: internal/
// relay must not import the hub (the wiring site imports both), and the hub
// speaks its own vocabulary — RelayedIngestRequest mirrors relay.RelayedMessage
// field for field precisely so this translation is mechanical.
//
// # THE ONE FIELD THAT IS NOT MECHANICAL
//
// Outcome. idem.Outcome's zero value is idem.OutcomeNew, and "new" is the answer
// that makes relay.Acceptor RE-FORWARD; a seam that filled in the message id and
// forgot the outcome would report every duplicate as new and amplify exactly the
// traffic the applied-key table exists to terminate. So it is assigned from
// res.Outcome on BOTH return paths — including the error path, where a violation
// must not arrive looking like the zero value.
//
// # WHAT IS NOT COPIED, AND MUST NEVER BE
//
// RelayedMessage.OriginBus, OriginSeq, IdempotencyKey and Fingerprint are not
// passed: the hub derives the origin sequence from OriginMessageID and keys the
// applied-key scope on it itself, and two fields carrying one fact are two fields
// that can drift. RelayedMessage.TimestampUnixMilli IS passed, and the hub records
// it as PROVENANCE only — it never becomes the local record's SentAt, which is an
// authorization input (store.Message.VisibleTo).
type hubIngest struct{ h *hub.Hub }

// The adapter must satisfy the seam, or the seam is fiction.
var _ relay.LocalIngest = hubIngest{}

// Enrolled consults the roster and nothing else, as LocalIngest requires: it is
// the only authority on this bus's own namespace, and it is asked BEFORE the
// durable write so that a name nobody holds costs this bus nothing permanent
// (invariant 1 — ids are never reused, including across restarts).
func (l hubIngest) Enrolled(agentID string) bool { return l.h.Enrolled(agentID) }

// AcceptRelayed makes the message durable through the hub's ONE two-phase write
// path (invariant 4: it returns only once the record is committed and fsynced,
// because the handler's 200 is an acknowledgement).
func (l hubIngest) AcceptRelayed(ctx context.Context, m relay.RelayedMessage) (relay.LocalAcceptance, error) {
	res, err := l.h.IngestRelayed(ctx, hub.RelayedIngestRequest{
		Sender:             m.Sender,
		Recipients:         m.Recipients,
		Body:               m.Body,
		OriginMessageID:    m.OriginMessageID,
		OriginAttestation:  m.OriginAttestation,
		BusPath:            m.BusPath,
		TimestampUnixMilli: m.TimestampUnixMilli,
		Signature:          m.Signature,
	})
	if err != nil {
		// The outcome is carried out on the error path too, because ONE error is
		// also an outcome: a key reused with a DIFFERENT payload is invariant 10's
		// violation rather than a failure to apply.
		//
		// BE PRECISE ABOUT WHAT THIS BUYS TODAY, because an earlier draft of this
		// comment claimed more than it delivers: relay.Acceptor.Accept checks err
		// FIRST and discards the whole LocalAcceptance, so no current caller reads
		// this value. It is set anyway because the alternative is returning a zero
		// LocalAcceptance — which claims idem.OutcomeNew, the answer that
		// RE-FORWARDS — to any future caller that classifies by value rather than
		// by sentinel. Note also that hub.IngestRelayed's PRE-publish refusals
		// return a zero result, so on those paths this really is OutcomeNew; the
		// error is the thing to check, exactly as its doc says.
		return relay.LocalAcceptance{Outcome: res.Outcome}, err
	}
	return relay.LocalAcceptance{LocalMessageID: res.MessageID, Outcome: res.Outcome}, nil
}

// recoverRelayEnvelope is ForwarderOptions.RecoverMessage, as a named function.
//
// It answers ONE question, asked only by relay.Forwarder.Resume and therefore
// only after a restart: "here is the correlation key of a delivery this bus
// still owes a peer -- rebuild the envelope, or tell me it cannot be rebuilt."
// The three answers it can give are the three the forwarder distinguishes:
//
//	(env, true, nil)   rebuilt; the job is RE-OFFERED
//	(_, false, nil)    no such message; the job is settled ABANDONED, and this
//	                   is the ORDINARY case -- the message aged out of the
//	                   retained window
//	(_, false, err)    the message is there and the envelope cannot be built;
//	                   the job is settled ABANDONED and the reason is logged
//
// # WHY IT IS NOT A CLOSURE IN main.go
//
// It used to be, and being one made it effectively untestable: the only way to
// reach it was to boot a whole server, and the only thing it has to get right is
// what happens after a CRASH. A crash test that re-implements the closure beside
// itself proves the test's copy correct and says nothing about production's.
// Taking the store and the local-envelope builder as PARAMETERS is what lets the
// real thing be called directly. main.go keeps only the nil-wiring guard, which
// is about construction order rather than about recovery.
//
// # localEnvelope IS THE ORIGINATED-HERE BUILDER, AND IT IS NOT USED FOR TRANSIT
//
// It is relayEgress.envelope in production: the builder for a message THIS BUS
// ORIGINATED, which names this bus as the origin and MINTS a fresh attestation
// from the local roster. A relay-INGESTED message must never go through it (it
// would claim somebody else's message as ours, and attest.Sign would refuse to
// attest their agent anyway -- invariant 2), which is exactly why the branch
// below sends transit traffic to relayedOriginEnvelope instead.
func recoverRelayEnvelope(localBusID string, st *store.Store, localEnvelope func(store.Message) (relay.RelayedMessage, error), originMessageID string) (relay.RelayedMessage, bool, error) {
	m, ok := st.ByOriginMessageID(originMessageID)
	if !ok {
		// "No such message" -- a settled, ABANDONED job per
		// ForwarderOptions.RecoverMessage. Not an error: the message
		// aged out of the retained window, which is ordinary.
		return relay.RelayedMessage{}, false, nil
	}
	if m.OriginMessageID != "" {
		// A RELAYED-IN MESSAGE, AND IT IS REBUILT RATHER THAN REFUSED
		// (RELAY-48). This arm used to return an error, which
		// Forwarder.resumeJob turns into a settled ABANDONED job -- so a
		// pending ONWARD hop did not survive a restart, after this bus had
		// already answered the upstream peer 200. That peer will not
		// resend; the obligation was simply destroyed.
		//
		// TWO THINGS HAD TO CHANGE, AND ONE ALONE WOULD HAVE BEEN WORSE
		// THAN NEITHER. The first is that the ingest now records the
		// origin's message id durably (hub.publish, via
		// store.WithRelayOrigin), which is what makes the
		// ByOriginMessageID lookup above HIT at all. On its own that
		// change reaches exactly this line and fails here instead --
		// moving the abandonment's reason string and nothing else. The
		// second is that the same write records the ORIGIN'S
		// ATTESTATION, which is the one field of the outbound envelope
		// this bus can never regenerate: attest.Sign refuses to attest an
		// agent in another bus's namespace (invariant 2), so a
		// relayed-in envelope was previously unbuildable from durable
		// state BY CONSTRUCTION. It is now buildable, from the record and
		// from nothing else.
		//
		// ONE BUILDER PER PROVENANCE, NOT ONE BUILDER FOR BOTH. This is
		// deliberately NOT egressAdapter.envelope: that one names THIS bus
		// as the origin, uses the LOCAL message id and MINTS an
		// attestation from the local roster, all three of which are wrong
		// for somebody else's message. See relayedOriginEnvelope for the
		// full argument, including the bus-path convention it has to
		// invert.
		//
		// A REFUSAL HERE IS STILL AN ABANDONED JOB, and the one case that
		// still reaches it is a message relay-ingested BEFORE this bus had
		// anywhere durable to keep the attestation. That is a genuine,
		// bounded, historical loss and it is logged loudly and
		// specifically by the forwarder rather than silently (invariant 6).
		env, err := relayedOriginEnvelope(localBusID, m)
		if err != nil {
			return relay.RelayedMessage{}, false, err
		}
		return env, true, nil
	}
	// ONE ENVELOPE BUILDER, REUSED. The live forward path and this
	// recovery path must not be two constructions that could disagree
	// about what goes on the wire.
	env, err := localEnvelope(m)
	if err != nil {
		return relay.RelayedMessage{}, false, err
	}
	return env, true, nil
}

// relayedOriginEnvelope rebuilds the ONWARD envelope for a message this bus
// INGESTED over a relay hop, from durable state alone (RELAY-48).
//
// # WHY IT IS A SEPARATE BUILDER FROM relayEgress.envelope
//
// They answer different questions and neither can answer the other's. envelope()
// builds the envelope for a message THIS BUS ORIGINATED: it names this bus as the
// origin, uses the LOCAL message id, and MINTS a fresh attestation from the local
// roster. Every one of those is wrong here, and the last is not merely wrong but
// forbidden — attest.Sign refuses a subject in another bus's namespace
// (invariant 2), which is the whole reason the origin's attestation has to be
// durable rather than regenerated.
//
// So this is the one place a relay-ingested message becomes an outbound envelope
// again, and it does it by CARRYING what the origin said, never by restating it.
//
// # THE TWO BUS-PATH CONVENTIONS, WHICH ARE THE TRAP ON THIS PATH
//
// store.Message.BusPath is the path INCLUDING this bus's own hop, appended at
// ingest. relay.RelayedMessage.BusPath is the path AS RECEIVED, WITHOUT it —
// Forward appends our hop itself via AppendHop. So the final hop is stripped
// here, and it is CHECKED to be ours before stripping rather than assumed: a
// message whose last hop is somebody else has not been through this bus's ingest
// and nothing about the rest of this function would be true of it.
//
// # EVERY FAILURE IS A REFUSAL, NEVER A GUESS
//
// The caller (Forwarder.RecoverMessage) turns an error into a settled, ABANDONED
// job with the reason logged. That is the right outcome for all of these: what
// must never happen is an envelope assembled around a missing or invented field,
// because the far end would refuse it as a forgery and the two cases would be
// indistinguishable to an operator.
func relayedOriginEnvelope(localBusID string, m store.Message) (relay.RelayedMessage, error) {
	if m.OriginMessageID == "" {
		return relay.RelayedMessage{}, fmt.Errorf("message %s carries no origin message id, so it did not arrive over a relay hop and has no origin envelope to rebuild", m.ID)
	}
	originBus, originSeq, err := ids.ParseMessageID(m.OriginMessageID)
	if err != nil {
		// Structurally unreachable: both store.Message.WithRelayOrigin and
		// store.Decode parse this field, so it cannot reach a durable message
		// unparsed. Checked because the alternative is building an envelope around
		// an origin bus id nobody validated.
		return relay.RelayedMessage{}, fmt.Errorf("message %s carries an unparseable origin message id: %w", m.ID, err)
	}
	if !m.HasOriginAttestation() {
		// THE ONE GENUINELY LOSSY CASE, and it is a bounded, historical one: a
		// message relay-ingested by a build that had nowhere durable to keep the
		// origin's attestation. This bus cannot mint a replacement for another
		// bus's agent and must not send an unattested envelope — the peer would
		// refuse it, and that refusal would be indistinguishable from an attack.
		return relay.RelayedMessage{}, fmt.Errorf("message %s was relayed to this bus from %s but its durable record carries NO origin attestation, so no envelope the next hop could verify can be rebuilt for it. This bus cannot mint one: attesting an agent in another bus's namespace is exactly what invariant 2 forbids. A record written before RELAY-48 looks like this, and the onward hop of that message is unrecoverable", m.ID, originBus)
	}
	if len(m.BusPath) < 2 {
		return relay.RelayedMessage{}, fmt.Errorf("message %s claims origin %s but its bus path has %d hop(s); a relay-ingested message has at least the origin's hop and ours", m.ID, m.OriginMessageID, len(m.BusPath))
	}
	// EXACT equality, NOT strings.EqualFold. It mirrors the check on the write
	// path that produced this path -- store.NewMessageWithBusPath requires the
	// stored path to END with this bus's id, and it compares exactly -- and a
	// mirror looser than its original is a mirror that admits something the
	// original refused. Unreachable through the write path either way; the point
	// is that the two cannot drift apart.
	if last := m.BusPath[len(m.BusPath)-1]; last != localBusID {
		return relay.RelayedMessage{}, fmt.Errorf("message %s has bus path ending at %q rather than at this bus (%q); the stored path of an ingested message always ends with our own hop, so this record was not written by this bus's ingest", m.ID, last, localBusID)
	}

	return relay.RelayedMessage{
		// THE ORIGIN IS THEIRS, NOT OURS. Both halves come out of the ONE durable
		// correlation field, so they cannot drift from each other.
		OriginBus:       originBus,
		OriginMessageID: m.OriginMessageID,
		OriginSeq:       originSeq,

		Sender:     m.Sender,
		Broadcast:  m.Broadcast,
		Recipients: append([]string(nil), m.Recipients...),

		// AS RECEIVED: our own final hop removed, so AppendHop can put it back.
		// Leaving it on would hand AppendHop a path it is already on and every
		// resumed forward would come back ErrRelayLoop.
		BusPath: append([]string(nil), m.BusPath[:len(m.BusPath)-1]...),

		// THE SIGNED TIMESTAMP, not this bus's acceptance clock. It is inside the
		// canonical bytes the origin agent's signature covers; substituting SentAt
		// would make every resumed message fail verification at the far end.
		TimestampUnixMilli: m.TimestampUnixMilli,

		// VERBATIM, and the reason this task existed. Copied rather than aliased
		// for the same reason relay's own cloneAttestation copies.
		OriginAttestation: cloneRelayedAttestation(m.OriginAttestation),

		Signature:     append([]byte(nil), m.Signature...),
		Body:          append([]byte(nil), m.Body...),
		ContentSHA256: m.ContentSHA256,

		// THE RELAY IDEMPOTENCY KEY IS THE ORIGIN MESSAGE ID — not this bus's id,
		// which is what relayEgress.envelope uses because there the two are the
		// same value. ValidateRelayRequest REFUSES an envelope whose key differs
		// from its origin message id, and that equality is what makes two copies
		// arriving by disjoint paths land on ONE idem.Scope at the far end.
		IdempotencyKey: m.OriginMessageID,

		// Fingerprint is left ZERO deliberately: relayFingerprint is unexported,
		// the receiving bus derives it itself inside ValidateRelayRequest, and a
		// DIFFERENT value computed here would be worse than none.
	}, nil
}

// cloneRelayedAttestation snapshots the two byte slices inside a value-typed
// attestation on its way from the durable store onto an envelope.
//
// internal/relay has an identical unexported helper and this package cannot
// reach it. The duplication is deliberate and is the lesser of the two evils on
// offer: the alternative is exporting a mutable-slice-copying helper from relay
// purely for this call, and a struct assignment ALONE — which is what the
// duplication exists to prevent — silently aliases key and signature bytes into
// an envelope that is about to be queued for later transmission.
func cloneRelayedAttestation(a attest.Attestation) attest.Attestation {
	out := a
	out.MessagingPublicKey = append(ed25519.PublicKey(nil), a.MessagingPublicKey...)
	out.Signature = append([]byte(nil), a.Signature...)
	return out
}

// ---------------------------------------------------------------------------
// Per-peer admission control (RELAY-FU-IDEM-METER-BY-PEER, RELAY-22)
// ---------------------------------------------------------------------------

// The bounds. Every number below is derived from one this repo already holds to,
// rather than picked, so it cannot drift away from the thing it bounds.
const (
	// relayAppliedKeyShare is how many applied-key entries ONE PEER may be
	// responsible for at a time, and it is idem's own fair-share arithmetic with
	// the PROVEN PEER substituted for the unproven agent label:
	//
	//	idem.MaxEntries / (relay.MaxPeers + 1)
	//
	// The "+1" is idem's phantom slot and is load-bearing for the same reason
	// (internal/idem/retention.go): it reserves room for the party that has not
	// arrived yet — here, this bus's own LOCAL agents, which hold no relay keys
	// precisely because they are the ones being starved. Divide by MaxPeers alone
	// and a single peer's share is the whole table again.
	//
	// It is a CEILING FOR EVERY PEER rather than a share of what is left,
	// deliberately: computing it from the number of peers currently holding keys
	// would let the bound move under a peer that is filling the table.
	relayAppliedKeyShare = idem.MaxEntries / (relay.MaxPeers + 1)

	// relayAppliedKeyWindow is how long one charge is counted for, and it is
	// idem.RetentionWindow because that is exactly how long the entry it stands
	// for is retained. A shorter window would let a peer hold more live entries
	// than its share; a longer one would refuse a peer whose entries have already
	// aged out.
	relayAppliedKeyWindow = idem.RetentionWindow

	// maxConcurrentRelayIngestsPerPeer bounds how many relay ingests one peer may
	// have IN FLIGHT. It is the memory-and-work bound, and it is separate from the
	// quota because they bound different things: the quota bounds a peer's share
	// of a table that is retained for two days, this bounds what it can make this
	// process do at one instant.
	//
	// 8 matches nothing else on purpose — the comparable local bound,
	// hub.MaxOutstandingMintsPerAgent (64), is per AGENT, and a peer connection
	// multiplexes an entire remote bus's roster onto ONE principal, so a
	// per-agent-shaped number applied per PEER would be 64 times too generous.
	maxConcurrentRelayIngestsPerPeer = 8

	// maxOnwardBusesPerMessage bounds the FAN-OUT of one relayed message this bus
	// carries onward (RELAY-47). It is the second half of the per-peer bound on
	// PEER-TRIGGERED OUTBOUND WORK, and the two multiply:
	//
	//	maxConcurrentRelayIngestsPerPeer × maxOnwardBusesPerMessage = 8 × 8 = 64
	//
	// which is relay.MaxPeers — so ONE source peer can occupy at most one
	// federation-wide fan-out of onward copies at any instant. The in-flight slot
	// is keyed on the AUTHENTICATED PEER (peerAdmission.enter, taken in
	// acceptRelayFrom and held across the onward enqueue), never on the sender
	// label inside the envelope, which a peer chooses.
	//
	// # WHY A BOUND IS NEEDED HERE AT ALL
	//
	// An onward hop is OUTBOUND work a PEER asks this bus to do, and it is not
	// cheap: relay.Forwarder.Enqueue writes a durable outbox record per target —
	// two fsyncs each — before it returns, on this goroutine. Without a cap, one
	// relayed message naming store.MaxRecipients (64) recipients spread across 64
	// buses costs 128 fsyncs, and 8 concurrent ingests from one peer cost 1024,
	// contending with this bus's OWN agents for the same disk. That is the
	// amplification shape RELAY-FU-IDEM-METER-BY-PEER and the SSRF this repo has
	// already shipped both have.
	//
	// # IT IS COUNTED ON DESTINATION BUSES, FROM THE ENVELOPE ALONE
	//
	// The count is the number of DISTINCT FOREIGN BUS HALVES among the recipients,
	// and it is an UPPER BOUND on the outbound copies — never an under-count,
	// which is the only direction that would matter. One destination bus is at
	// most one outbox job and one POST, even where two destinations share a next
	// hop: relay.Registry.Route answers with the DESTINATION bus id
	// (registry.go), Registry.PeerBaseURL then resolves that id to whatever
	// next-hop address the operator configured for it, and
	// relay.Forwarder.targets dedupes on the value Route returned.
	//
	// IT IS AN OVER-ESTIMATE TODAY, NOT ONLY HYPOTHETICALLY, and saying otherwise
	// would license the very heuristic Enqueue's own comment forbids further
	// down. relay.Forwarder.targets drops a destination with NO ROUTE and drops
	// one the EGRESS SPLIT HORIZON refuses because it is already on the traversed
	// path, and neither is a routing collapse. So counted-destinations and
	// queued-copies routinely differ on entirely correct traffic. (A future
	// registry that collapsed destinations onto one job would be a third cause,
	// not the first.)
	//
	// It is counted HERE, from the envelope, rather than by asking the registry
	// per recipient, because a second routing lookup beside
	// relay.Forwarder.targets is a SECOND ROUTING AUTHORITY — the exact class of
	// drift relayegress.go's routesToSomePeer is written to avoid. Over-estimating
	// is the direction that refuses rather than the direction that lets a bound
	// slip. RELAY-47-FU-FANOUT is the refinement.
	//
	// # WHAT A REFUSAL COSTS, STATED PLAINLY
	//
	// The message is already durable here and the peer has already been told 200,
	// so a refusal DROPS the onward copy rather than back-pressuring anybody. That
	// is the same outcome a recipient with no route already gets, and it is
	// answered the same way: loudly and specifically, never silently (invariant
	// 6). Refusing BEFORE the durable write instead — a 503 the peer retries —
	// was considered and rejected for MVP: a legitimately wide message would then
	// be retried for the whole retry horizon and never become deliverable, which
	// is the "told to retry something it cannot fix" defect this repo has already
	// recorded twice.
	maxOnwardBusesPerMessage = maxConcurrentRelayIngestsPerPeer
)

// errRelayPeerBusy is the concurrency refusal; errRelayPeerQuota is the quota
// refusal. Both reach the peer as 503 CodeUnavailable — the relay handler maps
// any unclassified callback error that way — which is the right answer for both:
// "not now" rather than "never", so a correct peer backs off and retries, and
// NOTHING was written for either.
var (
	errRelayPeerBusy  = errors.New("relay ingest: this peer already has the maximum number of relayed messages in flight on this bus")
	errRelayPeerQuota = errors.New("relay ingest: this peer is at its share of this bus's applied-key table")
)

// peerAdmission meters the relay ingest path BY THE AUTHENTICATED PEER.
//
// # THE DEFECT IT CLOSES, in one sentence
//
// The applied-key scope a relayed message lands in is keyed on its SENDER, and
// on this path the sender is a label the peer asserts and nobody has proved
// (hub.IngestRelayed says so in its own doc). idem's per-agent fair share is safe
// only because its bucket key is a proven identity everywhere else, so on this
// one path a peer asserting many distinct sender names takes a growing share of a
// bus-wide table that fails CLOSED and evicts nothing — ending with this bus's
// own agents refused with ErrCapacity for up to idem.RetentionWindow. The fix
// cannot live in internal/hub or internal/idem: neither can see the connection.
// It lives here, keyed on the one identity that WAS proved — the peer principal
// resolved from the TLS client certificate.
//
// # WHAT IT DOES AND DOES NOT CLOSE — measured, not claimed
//
// An earlier draft of this comment said it closed that defect. It does not, and
// the security gate put a number on the remainder: because enforcement is gated
// on the pressure line (below), and idem's line is maxEntries/2, a peered but
// hostile bus can still place ~32768 entries under ~32768 distinct sender labels
// before meeting a single refusal — REACHING the line is the damage, and the
// fair-share denominator it then distorts is keyed on those unproven labels.
//
// What this DOES buy, and it is worth having: one peer can no longer take more
// than roughly half the table, so peer traffic alone can no longer drive this
// bus's own agents into global ErrCapacity, and no peer can take another peer's
// room. The other half of the fix belongs to internal/idem — a denominator that
// counts PROVEN principals rather than asserted labels — and is filed separately.
//
// # IT COUNTS LIVE ENTRIES. IT IS NOT A RATE LIMIT, AND THE DIFFERENCE IS THE BUG
//
// The first version of this was a token bucket refilling `share` tokens per
// RetentionWindow, and the security gate showed what that actually is: a
// PERMANENT ~20-messages-an-hour speed limit per peer, applied whether the
// applied-key table was 1% full or 99% full, whose 503 the sending bus retries
// for its whole ~24h horizon before dropping the message. It would have throttled
// every honest federation on the bus to protect a table that was empty. A token
// bucket also admits capacity+rate*window = TWICE the share inside one window, so
// it did not even bound the thing it cost that much to bound.
//
// What is here instead is a SLIDING WINDOW COUNT: the charge instants inside the
// last window, which is exactly "how many applied-key entries this peer is
// currently responsible for", because an entry is retained for exactly that
// window. A peer under its share is NEVER slowed, however fast it sends.
//
// # AND IT ONLY REFUSES UNDER PRESSURE, WHICH IS idem'S OWN POSTURE
//
// internal/idem/retention.go engages its fair share only above PressureLine, on
// the reasoning that below it "whatever one agent has consumed is BY CONSTRUCTION
// still available to everyone else". The same is true here, so the same rule
// applies: the count is kept ALWAYS (so the bound engages the instant pressure
// arrives, with a full window of history behind it) and it REFUSES only while the
// table is actually under pressure. A bus that never approaches its cap sees no
// behaviour change from this meter whatsoever.
//
// # WHAT IS CHARGED, AND WHY IT IS NOT EVERY REQUEST
//
// A charge is taken for an ingest about to attempt a durable write, and REVERSED
// when the write turned out to be a duplicate or failed. So the count tracks
// entries this peer actually put in the table, and a peer RETRYING CORRECTLY —
// invariant 10's legitimate retry, which returns the original result and applies
// nothing — does not spend its share on messages it never added. The concurrency
// cap is what bounds a flood of duplicates; this bounds the table.
//
// # IT REFUSES BEFORE THE WRITE, NEVER AFTER
//
// Both checks run before relay.Acceptor.Accept, so a refusal costs this bus
// nothing durable — the same discipline the acceptor's own roster check follows.
// A quota enforced after the write would be a counter, not a bound.
type peerAdmission struct {
	mu      sync.Mutex
	buckets map[string]*peerBucket

	share         int
	window        time.Duration
	maxConcurrent int
	maxPeers      int
	now           func() time.Time

	// underPressure reports whether the applied-key table is at the fill level
	// where one peer's share is worth defending. Production passes the hub's own
	// idem.Stats; see federationOptions.UnderPressure for why nil means "yes".
	underPressure func() bool
}

// peerBucket is one peer's state: the charge instants still inside the window,
// and the in-flight count.
//
// charged is bounded by share: reserve stops appending once the count reaches it
// (including on the below-pressure path, where the request is still admitted), so
// the slice cannot grow past the number of entries the peer is allowed to hold.
// An earlier version of this comment claimed that bound while the below-pressure
// path appended without limit; the security gate measured it at 100 entries for a
// share of 4, and TestPeerAdmissionChargeSliceIsBoundedByTheShare now pins it.
type peerBucket struct {
	charged  []time.Time
	inFlight int
}

// newPeerAdmission builds the meter. Zero values mean the derived defaults
// above; a NEGATIVE value is a construction error rather than a silent default,
// because "unlimited" must not be spellable.
func newPeerAdmission(share int, window time.Duration, maxConcurrent int, now func() time.Time, underPressure func() bool) (*peerAdmission, error) {
	if share < 0 || window < 0 || maxConcurrent < 0 {
		return nil, fmt.Errorf("relay wiring: peer admission bounds must not be negative (share=%d window=%s concurrent=%d)", share, window, maxConcurrent)
	}
	if share == 0 {
		share = relayAppliedKeyShare
	}
	if window == 0 {
		window = relayAppliedKeyWindow
	}
	if maxConcurrent == 0 {
		maxConcurrent = maxConcurrentRelayIngestsPerPeer
	}
	if now == nil {
		now = time.Now
	}
	if underPressure == nil {
		// FAIL CLOSED. A wiring site that cannot say whether the table is under
		// pressure gets the bound enforced, not disabled: the cost of being wrong
		// here is a refused relayed message, and the cost of the other default is
		// the unbounded table this meter exists to prevent.
		underPressure = func() bool { return true }
	}
	return &peerAdmission{
		buckets:       make(map[string]*peerBucket),
		share:         share,
		window:        window,
		maxConcurrent: maxConcurrent,
		maxPeers:      relay.MaxPeers,
		now:           now,
		underPressure: underPressure,
	}, nil
}

// bucketLocked returns peerBusID's bucket, creating it EMPTY (a peer we have not
// heard from holds no entries).
//
// # THE MAP IS BOUNDED, AND AN IDLE BUCKET IS RECLAIMED RATHER THAN HOARDED
//
// Growth is not attacker-driven — every key has already been resolved from an
// operator-installed certificate binding — but the cap is real, so at the cap a
// bucket that holds NOTHING (no live charges, nothing in flight) is dropped to
// make room. Without that, 64 peers that once spoke would permanently lock out
// the 65th legitimate one. Nothing is evicted to make room for a peer while its
// own state still means something, so eviction can never hide a live charge.
func (a *peerAdmission) bucketLocked(peerBusID string) (*peerBucket, error) {
	key := strings.ToLower(peerBusID)
	if b, ok := a.buckets[key]; ok {
		return b, nil
	}
	if len(a.buckets) >= a.maxPeers {
		a.reclaimIdleLocked()
	}
	if len(a.buckets) >= a.maxPeers {
		return nil, fmt.Errorf("%w: %d peers are already metered, the limit, and every one of them holds live applied keys or has work in flight", errRelayPeerBusy, len(a.buckets))
	}
	b := &peerBucket{}
	a.buckets[key] = b
	return b, nil
}

// reclaimIdleLocked drops every bucket carrying no live charge and no in-flight
// work. Those buckets are pure bookkeeping: recreating one costs an allocation
// and loses nothing, because an empty bucket and a missing bucket say the same
// thing about the peer.
func (a *peerAdmission) reclaimIdleLocked() {
	now := a.now()
	for key, b := range a.buckets {
		b.pruneLocked(now, a.window)
		if b.inFlight == 0 && len(b.charged) == 0 {
			delete(a.buckets, key)
		}
	}
}

// enter takes one of this peer's in-flight slots. The returned release is safe
// to call exactly once and MUST be deferred by the caller.
func (a *peerAdmission) enter(peerBusID string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := a.bucketLocked(peerBusID)
	if err != nil {
		return nil, err
	}
	if b.inFlight >= a.maxConcurrent {
		return nil, fmt.Errorf("%w: %d in flight, the limit is %d", errRelayPeerBusy, b.inFlight, a.maxConcurrent)
	}
	b.inFlight++
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if b.inFlight > 0 {
			b.inFlight--
		}
	}, nil
}

// reserve charges one applied-key entry to this peer, returning the REVERSAL to
// call when the write did not create one (a duplicate, or a failure).
//
// Reversing rather than not charging is what keeps the check BEFORE the write:
// the outcome is not known until the write has happened, and a bound that only
// engages afterwards is not a bound.
func (a *peerAdmission) reserve(peerBusID string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := a.bucketLocked(peerBusID)
	if err != nil {
		return nil, err
	}
	now := a.now()
	b.pruneLocked(now, a.window)
	if len(b.charged) >= a.share {
		// AT ITS SHARE — but refused only while the table is actually contended.
		// Below the pressure line the entries this peer holds are not denying
		// anybody anything, and refusing would be a speed limit rather than a
		// bound.
		if a.underPressure() {
			return nil, fmt.Errorf("%w: it is responsible for %d of the last %d applied keys, its share, and the table is under pressure", errRelayPeerQuota, len(b.charged), a.share)
		}
		// ADMITTED AND NOT RECORDED, which is what keeps `charged` bounded by the
		// share. The count is already AT the ceiling, so a further entry changes
		// no decision this meter will ever make — the peer is refused the moment
		// pressure arrives either way — while appending would let a peer sending
		// freely over an uncontended table grow this slice without limit. The
		// security gate measured that: share=4 and 100 charges gave len=100,
		// against a doc claiming the share bounded it.
		return func() {}, nil
	}
	b.charged = append(b.charged, now)
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if n := len(b.charged); n > 0 {
			b.charged = b.charged[:n-1]
		}
	}, nil
}

// pruneLocked drops charges older than the window — the entries they stand for
// have expired out of the applied-key table, so the peer is no longer
// responsible for them.
//
// A clock that steps BACKWARDS prunes NOTHING rather than pruning wrongly: the
// comparison is one-directional, so a stepped clock can only delay a peer's
// budget returning, never grant it early and never take it away.
func (b *peerBucket) pruneLocked(now time.Time, window time.Duration) {
	keep := 0
	for _, t := range b.charged {
		if now.Sub(t) < window {
			break
		}
		keep++
	}
	if keep > 0 {
		b.charged = append(b.charged[:0], b.charged[keep:]...)
	}
}

// ---------------------------------------------------------------------------
// Binding a peer's CLAIMS to its authenticated identity (invariant 2)
// ---------------------------------------------------------------------------

// errPeerClaimMismatch is the refusal a peer earns by asserting an id outside its
// own namespace. It is wrapped in the SURFACE'S OWN sentinel by each caller, so a
// peer sees that surface's vocabulary rather than a new one.
var errPeerClaimMismatch = errors.New("the authenticated peer asserted an id that is not its own")

// checkPeerAssertsOwnID is invariant 2 applied to a CLAIM: a peer may assert ids
// in its own namespace and in no other.
//
// # WHAT IT STOPS, CONCRETELY
//
// Without it, peer B posts a peer-enrol or a roster update claiming bus_id "C"
// and REPLACES C's roster in this bus's routing table — Registry.UpsertPeer
// installs a full roster by claimed bus id and resets the version, so one request
// re-points every agent of C's at B. Nothing else on the path can catch it: the
// handler validates that the claim is well formed and is not OURS, which both a
// legitimate claim and this one satisfy.
//
// # AN EMPTY peerBusID IS A REFUSAL, NOT A SKIP
//
// It means the request did not come through RequirePeerPrincipal — a wiring
// mistake, since these callbacks are only reachable from gated routes — and the
// safe reading of "I do not know who this is" is not "then anything they say is
// fine".
//
// # THE COMPARISON IS EXACT, NOT CASE-FOLDED
//
// A claim differing from the authenticated id only by ASCII case is a confusable
// in the routing subject, and this bus is entitled to insist on the spelling the
// operator bound. Folding here would admit "BUS-C" for "bus-c" and hand the
// registry two spellings of one identity, which is the same door
// Registry.UpsertPeer closes on itself.
func checkPeerAssertsOwnID(peerBusID, claimed string) error {
	if peerBusID == "" {
		return fmt.Errorf("%w: this request carries no authenticated peer principal, so nothing it asserts can be attributed to a bus", errPeerClaimMismatch)
	}
	if claimed == peerBusID {
		return nil
	}
	if strings.EqualFold(claimed, peerBusID) {
		return fmt.Errorf("%w: it authenticated as %q and claims a spelling that differs only by case; a confusable in the routing subject is refused rather than folded", errPeerClaimMismatch, peerBusID)
	}
	return fmt.Errorf("%w: it authenticated as %q and claims %s", errPeerClaimMismatch, peerBusID, elidePeerClaim(claimed))
}

// checkPeerIsLastHop binds the traversed path to the connection it arrived on:
// the LAST hop of an incoming bus path is the bus that sent it to us, because
// relay.RelayedMessage.Forward appends the sender's own hop before it goes on the
// wire (relay.AppendHop). That hop must be the peer we authenticated.
//
// # IT DOES NOT MAKE THE PATH TRUSTWORTHY, AND MUST NOT BE READ AS DOING SO
//
// PROTOCOL.md §8.5 settles that the path is outside the signature and a lying
// peer can rewrite the rest of it; loop prevention is an availability mechanism,
// never a security one. What this check buys is narrow and worth having: the ONE
// hop we can independently verify is verified, so a peer cannot hide its own
// participation by stamping somebody else's id in the position it occupies —
// which is what would let it evade the egress split horizon and the audit trail's
// account of who handed us the message.
func checkPeerIsLastHop(peerBusID string, busPath []string) error {
	if peerBusID == "" {
		return fmt.Errorf("%w: this relayed message arrived with no authenticated peer principal", errPeerClaimMismatch)
	}
	if len(busPath) == 0 {
		// Unreachable through the handler — ValidateBusPath refuses an empty
		// ingress path — so this is the direct-caller belt to that braces.
		return fmt.Errorf("%w: this relayed message carries no traversed bus path, so nothing names the bus that sent it", errPeerClaimMismatch)
	}
	last := busPath[len(busPath)-1]
	if last == peerBusID {
		return nil
	}
	return fmt.Errorf("%w: it authenticated as %q but the last hop of the path it sent is %s", errPeerClaimMismatch, peerBusID, elidePeerClaim(last))
}

// elidePeerClaim renders a peer-supplied id for a log line or an error.
//
// Every claim reaching the checks above has been through a validator that caps
// its length — but they are exported-shaped functions on an untrusted path, and
// the ONE thing a refusal must never do is let the refused party choose the size
// of the line written about it. Oversized input is named by its length instead.
func elidePeerClaim(s string) string {
	if len(s) > relay.MaxPeerBusIDLen {
		return fmt.Sprintf("a %d-byte value, which is not echoed here because it is oversized", len(s))
	}
	return fmt.Sprintf("%q", s)
}

// ---------------------------------------------------------------------------
// The ONWARD hop: carrying a peer's message to a FURTHER bus (RELAY-47)
// ---------------------------------------------------------------------------

// onwardRelay is what relay.Acceptor is given as AcceptOptions.Onward: the
// bounded, loud wrapper around the SAME relay.Forwarder the egress half uses.
//
// # IT IS A WRAPPER, NOT A SECOND FORWARDER
//
// relay.Forwarder already satisfies relay.OnwardForwarder outright — there is a
// compile-time assertion saying so (internal/relay/accept.go) — so this type
// exists for exactly two things the forwarder cannot do from where it sits:
//
//  1. BOUND the peer-triggered fan-out (maxOnwardBusesPerMessage).
//  2. SAY SO when a message this bus accepted for somebody else's agents is
//     carried no further after all. The forwarder counts its own drops, but the
//     line an operator needs — "this specific relayed message stops here, and
//     here is why" — belongs where the accepted message and its recipients are
//     both in hand.
//
// It adds NO routing decision of its own: which peers receive a copy is
// relay.Forwarder.targets's answer and nothing else's, and the split horizon,
// the hop stamp and the hop limit stay entirely inside internal/relay
// (NextHopAllowed, AppendHop). This type must never grow a second copy of any of
// them.
//
// # THE MESSAGE IS PASSED THROUGH UNTOUCHED, AND THAT IS LOAD-BEARING
//
// relay.RelayedMessage.BusPath here is the path AS RECEIVED, WITHOUT this bus's
// hop; Forward appends it via relay.AppendHop. The recipients, the body, the
// signature and the ORIGIN attestation all travel verbatim — an intermediate
// re-signs nothing and re-attests nothing (invariant 2: the sender belongs to
// the origin bus, and attest.Sign refuses to sign a subject in anybody else's
// namespace). Rewriting the recipient list to trim the fan-out is therefore not
// merely undesirable, it is IMPOSSIBLE: the recipients are covered by the
// sender's signature, so a trimmed copy would fail verification at the next hop.
type onwardRelay struct {
	busID string
	next  relay.OnwardForwarder
	log   *logging.Logger

	forwardedCopies  atomic.Uint64
	refusedFanOut    atomic.Uint64
	carriedNoFurther atomic.Uint64
}

// The wrapper must satisfy the seam it is passed as, or the seam is fiction.
var _ relay.OnwardForwarder = (*onwardRelay)(nil)

// newOnwardRelay wraps next. It returns nil when next is nil, so that a bus with
// no forwarder passes a genuinely nil AcceptOptions.Onward — the documented LEAF
// configuration — rather than a non-nil interface holding a nil pointer, which
// the acceptor would call and which would panic on a bus that does not federate
// at all. Same trap, same shape, as hub.Options.Egress in main.go.
func newOnwardRelay(busID string, next relay.OnwardForwarder, log *logging.Logger) *onwardRelay {
	if next == nil {
		return nil
	}
	if log == nil {
		log = logging.New(discardWriter{}, logging.LevelError)
	}
	return &onwardRelay{busID: busID, next: next, log: log}
}

// Enqueue implements relay.OnwardForwarder.
//
// It is called by relay.Acceptor.Accept as STEP 3, and only for a NEW acceptance
// (idem.OutcomeNew) — a duplicate is answered and forwarded nowhere, which is
// what terminates traffic in a cyclic topology. That gate lives in the acceptor
// and must not be repeated here: two copies of one rule are two rules that can
// drift.
func (o *onwardRelay) Enqueue(m relay.RelayedMessage) (int, error) {
	if m.Broadcast {
		// A RELAYED BROADCAST HAS NO ROSTER-CHECKABLE AUDIENCE and carries no
		// recipient list, so the fan-out count below would be 0 and this function
		// would return silently — a message accepted, made durable and dropped
		// from the onward path with no line about it, which is invariant 6's
		// silent discard exactly.
		//
		// Unreachable today: ValidateRelayRequest check 11a and Acceptor.Accept
		// both refuse a relayed broadcast, because the canonical signing format
		// has no bytes for an empty audience. It is checked HERE anyway because
		// the day SIGN-3 defines a broadcast's signed audience, those two
		// refusals go away and this arm becomes the one that decides — and the
		// failure it would otherwise produce is silent rather than loud.
		o.carriedNoFurther.Add(1)
		o.log.Warn("a relayed BROADCAST was accepted and is being carried NO FURTHER: this build has no onward fan-out rule for a message with no recipient list",
			"local_bus", o.busID,
			"origin_bus", m.OriginBus,
			"origin_message_id", m.OriginMessageID,
			"remedy", "SIGN-3 must define a broadcast's signed audience before a relayed broadcast can be routed onward; until then a relayed broadcast is refused at ingest and this line should never appear",
		)
		return 0, nil
	}
	foreign := foreignBuses(m.Recipients, o.busID)
	if len(foreign) == 0 {
		// Every recipient is ours. This bus is the DESTINATION, not a hop, and
		// there is nothing to carry — the ordinary case, and silent on purpose.
		return 0, nil
	}
	if len(foreign) > maxOnwardBusesPerMessage {
		o.refusedFanOut.Add(1)
		o.log.Warn("a relayed message was ACCEPTED AND DURABLY RECORDED but is being carried NO FURTHER: it names recipients on more foreign buses than one relayed message may fan out to on this bus. The sending peer has been told 200 and will not retry, so those recipients will never receive it",
			"local_bus", o.busID,
			"origin_bus", m.OriginBus,
			"origin_message_id", m.OriginMessageID,
			"foreign_buses", len(foreign),
			"fan_out_limit", maxOnwardBusesPerMessage,
			"refused_fan_out_total", o.refusedFanOut.Load(),
			"remedy", "an onward hop costs two fsyncs per destination peer and is work a PEER asks this bus to do, so it is bounded per message. Address the message to fewer buses, or peer the destination buses with the origin directly so no single bus carries the whole fan-out",
		)
		return 0, nil
	}

	queued, err := o.next.Enqueue(m)
	if err != nil {
		// ErrForwarderClosed — a lifecycle condition of THIS bus. The acceptor
		// logs it and does NOT undo the acceptance; nothing to add here.
		return queued, err
	}
	// WHAT IS NOT DETECTED HERE, STATED SO NOBODY READS THIS AS COMPLETE
	// COVERAGE: a message reaching SOME of its destinations and not others.
	// relay.Forwarder.targets counts a recipient it cannot route and moves on
	// without a line, so a message naming one routable and one unroutable
	// foreign bus returns queued=1 and passes through here in silence.
	//
	// `queued < len(foreign)` IS NOT THE MISSING TEST, and inventing it would be
	// worse than the gap, because it FIRES ON CORRECT TRAFFIC: the EGRESS SPLIT
	// HORIZON legitimately drops a destination bus that is already on the
	// traversed path. A message relayed A→B naming recipients on both A and C
	// counts two foreign buses at B and queues exactly one copy — nothing is
	// lost, A already has it, and a detector built on that comparison would
	// report a delivery failure on the ordinary transit case this task exists to
	// support. (An earlier draft of this comment justified it by claiming a
	// -route-for topology collapses destinations onto one job. It does NOT:
	// `peer add -route-for` writes a SEPARATE peer record per destination, so two
	// destinations behind one next hop are two jobs and two POSTs to one address
	// — as maxOnwardBusesPerMessage's own comment says. The conclusion was right
	// and the reason was wrong, which is the harder defect to spot.)
	//
	// The sound version needs the target set the forwarder actually resolved,
	// which only the forwarder has. Filed as RELAY-50.
	if queued == 0 {
		// THE HONEST BAD NEWS, AND IT MUST STAY LOUD. The forwarder resolved no
		// target: no route to the destination bus, or the split horizon refused
		// every candidate (the message has already been where it is going), or the
		// path is at its hop limit. Each of those is counted inside the forwarder;
		// what is added here is WHICH MESSAGE stopped, which the forwarder's own
		// counters cannot say.
		o.carriedNoFurther.Add(1)
		o.log.Warn("a relayed message was ACCEPTED AND DURABLY RECORDED but is being carried NO FURTHER: it names recipients on another bus and the cross-bus forwarder queued no copy for any of them. The sending peer has been told 200 and will not retry, so those recipients will never receive it",
			"local_bus", o.busID,
			"origin_bus", m.OriginBus,
			"origin_message_id", m.OriginMessageID,
			"foreign_buses", len(foreign),
			"path_hops", len(m.BusPath),
			"carried_no_further_total", o.carriedNoFurther.Load(),
			"remedy", "onward relay IS wired on this bus, so this is a ROUTING answer rather than a missing capability: check that a peer record names the destination bus (`agent-bus peer add -bus-id <destination> -url <next hop> ...`, or -route-for on the next hop's record), that the next hop is not already on this message's traversed path, and that the path has not reached the 64-hop limit",
		)
		return 0, nil
	}
	o.forwardedCopies.Add(uint64(queued))
	return queued, nil
}

// stats reports what this wrapper has done. It is read by tests and is the shape
// an operator-facing counter would take; the forwarder keeps its own.
func (o *onwardRelay) stats() (forwarded, refusedFanOut, carriedNoFurther uint64) {
	return o.forwardedCopies.Load(), o.refusedFanOut.Load(), o.carriedNoFurther.Load()
}

// foreignBuses returns the DISTINCT bus halves among recipients that are not
// localBusID, folded case-insensitively.
//
// The fold matches relay.Acceptor.unknownLocalRecipients, which treats a
// confusable spelling of OUR bus id as OURS and refuses it: a recipient that
// folds to us is therefore never counted as foreign here either, and the two
// sides cannot disagree about whose namespace a name is in. A recipient that
// does not parse is not attributed to any bus — internal/relay has already
// refused the message if one did (ValidateRelayRequest parses every recipient),
// so this is the direct-caller path, and counting an unparseable name as a
// foreign destination would let it inflate the fan-out bound.
func foreignBuses(recipients []string, localBusID string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range recipients {
		bus, _, _, err := ids.ParseAgentID(r)
		if err != nil {
			continue
		}
		if strings.EqualFold(bus, localBusID) {
			continue
		}
		out[strings.ToLower(bus)] = struct{}{}
	}
	return out
}

// ---------------------------------------------------------------------------
// The federation object
// ---------------------------------------------------------------------------

// federationOptions configures newFederation. Every field without a documented
// default is REQUIRED, and each one is checked: a federation assembled with a
// piece missing looks healthy and silently serves nobody.
type federationOptions struct {
	// BusID is THIS bus's server-minted id (invariant 1).
	BusID string

	// Registry is the ONE routing table this bus has, and it is now CONSTRUCTED
	// BY THE CALLER and passed in rather than built here
	// (RELAY-24-BLOCKER-EGRESS).
	//
	// It moved for a structural reason, not a stylistic one: the same table is
	// read by the EGRESS half — hub.Options.RemoteRouter answers "is this
	// recipient behind a peer", and relay.Forwarder resolves the peer's address
	// through Registry.PeerBaseURL — and both of those are wired BEFORE the hub
	// exists, while this ingress is assembled after it. A registry built in here
	// would be a SECOND table: the handshake would populate one, the forwarder
	// would route on the other, and a peer that had just handshaked would still
	// be unroutable. Two routing tables that can disagree is precisely the class
	// of bug this repo keeps finding.
	//
	// REQUIRED, and nil is a construction error like every other missing piece.
	Registry *relay.Registry

	// Local is the local bus behind the relay ingress. Production passes
	// hubIngest; it is an interface so the binding and metering rules can be
	// driven without a durable hub.
	Local relay.LocalIngest

	// Onward is the cross-bus forwarder a RELAY-INGESTED message is handed to
	// when it names recipients on a further bus (RELAY-47). Production passes the
	// SAME *relay.Forwarder the egress adapter uses — one outbox, one set of peer
	// queues, one place that decides targets.
	//
	// OPTIONAL, and nil is a legitimate LEAF configuration rather than a mistake:
	// a bus with no peer store has no forwarder, accepts relayed mail for its own
	// agents and carries nothing further. See relay.AcceptOptions.Onward, which
	// says the same thing one layer down, and warnIfCarriedNoFurther, which is
	// what makes the leaf case audible instead of silent.
	//
	// IT IS AN INTERFACE, NOT *relay.Forwarder, AND THAT MATTERS AT THE CALL
	// SITE. A nil *relay.Forwarder assigned into an interface field is NOT nil —
	// it is a non-nil interface holding a nil pointer, which the acceptor would
	// dutifully call. main.go therefore assigns this from an interface-typed
	// variable set only on the branch that actually built a forwarder, exactly as
	// it does for hub.Options.Egress and hub.Options.RemoteRouter.
	Onward relay.OnwardForwarder

	// Peers is the durable, ALREADY-REPLAYED peer store. It serves two distinct
	// jobs — httpapi.Options.PeerPrincipals (which bus is on this connection) and
	// PeerSurface.Trust (which signing keys the ORIGIN bus's messages are verified
	// against) — and they are deliberately different keys pinned at different
	// moments; see PeerStore.InboundPeerPrincipal.
	Peers *relay.PeerStore

	// LocalAgents returns this bus's fully-qualified agent ids for the handshake
	// reply. Called once per handshake, so it must be safe for concurrent use.
	LocalAgents func() []string

	// Outbox is THE SAME durable per-hop obligation table the egress forwarder
	// writes to (relay.Forwarder.Enqueue), and it is required for federation.
	//
	// It is the ANTI-FORGERY CORE of the ACK plane (ACK-CONTRACT.md §6.2): a
	// peer-hop acknowledgement from peer P is authoritative for correlation key K
	// if and only if DeriveJobID(P, K) names a job THIS BUS DURABLY WROTE. That
	// is computable from this table and from nothing else — a second source of
	// obligations would be a second answer to "did we owe this?", and the two
	// could disagree.
	Outbox *relay.Outbox

	// AckLifecycle is the durable sender-visible delivery lifecycle table
	// (ACK-2). A peer-hop acknowledgement settles a row in it, AFTER the
	// obligation binding above has authorised the peer to speak about the key.
	//
	// Required for federation for the same reason Outbox is: an ACK route that
	// authorised a settlement and then had nowhere to record it would answer 200
	// to a peer while recording nothing, which is exactly the "silently discard"
	// shape every other required callback in this file exists to prevent.
	AckLifecycle *ack.Store

	// Logger is optional; nil discards.
	Logger *logging.Logger

	// UnderPressure reports whether the applied-key table is at the fill level
	// where one peer's share is worth defending. Production passes
	// hub.IdempotencyStats().UnderPressure — idem's OWN pressure line, so the two
	// bounds engage together rather than at two numbers that could drift.
	//
	// NIL MEANS "ASSUME PRESSURE", which is the fail-closed reading: a meter that
	// cannot see the table enforces the bound rather than disabling it.
	UnderPressure func() bool

	// Now, AppliedKeyShare, AppliedKeyWindow and MaxConcurrentPerPeer override the
	// admission bounds. Zero means the derived default in every case; they exist
	// so a test can demonstrate a bound instead of asserting it, which a bound
	// that only exists at 1008 entries over 50 hours could never do.
	Now                  func() time.Time
	AppliedKeyShare      int
	AppliedKeyWindow     time.Duration
	MaxConcurrentPerPeer int
}

// federation owns the assembled ingress: the routing table, the acceptor, the
// meter, and the complete surface handed to httpapi.
type federation struct {
	busID     string
	registry  *relay.Registry
	acceptor  *relay.Acceptor
	admission *peerAdmission
	surface   *httpapi.PeerSurface
	log       *logging.Logger

	// acks is the durable sender-visible delivery lifecycle table a peer-hop
	// acknowledgement settles a row in (ACK-2). It is reached only from
	// settleAck, and only after relay.AuthorizePeerAck has bound the frame to an
	// obligation this bus wrote: ack.Store.Settle takes NO PRINCIPAL and says so
	// in its own doc, so the authorization it cannot do is owed by every caller.
	acks *ack.Store

	// onward is the wrapper handed to the acceptor, or nil on a LEAF build. It is
	// kept here for one reason: warnIfCarriedNoFurther must say something
	// DIFFERENT depending on which of the two this bus is, and "the egress
	// forwarder is not wired yet" is a false statement on a bus where it is.
	onward *onwardRelay

	// mu guards rosterMemo and enrolMemo, AND IS HELD ACROSS THE REGISTRY CALL
	// each of them guards. See applyRosterFrom for why that is not merely tidy.
	mu sync.Mutex

	// rosterMemo is ONE SLOT PER PEER remembering the last roster update it
	// applied, keyed by the folded peer bus id. It is NOT a second applied-key
	// table and must not grow into one.
	//
	// # WHY IT EXISTS
	//
	// RosterConfig.Apply hands us an idempotency key and a fingerprint and says,
	// in its own doc, that discarding them is a REAL DEFECT rather than a
	// simplification: a peer whose acknowledgement was lost retries the identical
	// update, meets Registry.ApplyRosterUpdate's version-monotonicity check and
	// gets 409 STALE. That punishes exactly the peers retrying correctly, which is
	// what invariant 10 exists to prevent.
	//
	// # WHY IT IS NOT THE SECOND TABLE internal/idem FORBIDS
	//
	// The prohibition is against a second answer to "have I applied this?" that
	// could DRIFT FROM THE DURABLE ONE. A roster update has no durable answer to
	// drift from: the registry itself is in-memory and is rebuilt by re-handshake
	// after a restart, so this memo has exactly the same lifetime as the state it
	// describes. It holds one key and one fingerprint per peer, and it can only
	// ever turn an EXACT repeat into the original success.
	//
	// # IT IS BOUNDED BY THE REGISTRY, NOT BY AN ASSERTION
	//
	// A slot is written only AFTER Registry.ApplyRosterUpdate succeeded, and that
	// refuses any bus it does not already know (ErrUnknownPeer) while itself
	// holding at most relay.MaxPeers peers. So the memo cannot have more slots
	// than the registry has peers — the bound is mechanical rather than a claim.
	//
	// # IT IS CLEARED WHEN THE STATE IT DESCRIBES IS REPLACED
	//
	// A completed handshake REPLACES a peer's roster and resets its version to 0
	// (Registry.UpsertPeer), so a memo surviving that would answer "already
	// applied" for an update that is no longer applied to anything — a 200 over a
	// registry that never saw it. acceptPeerFrom clears both slots for that peer
	// in the same critical section as the upsert.
	//
	// Same key + DIFFERENT fingerprint is invariant 10's violation: rejected and
	// logged, and NOBODY IS DISCONNECTED.
	rosterMemo map[string]rosterMemoEntry

	// enrolMemo is the SAME one-slot-per-peer memo for the HANDSHAKE, and it
	// exists for a sharper reason than the roster one: a completed handshake
	// RESETS that peer's roster version to 0 (Registry.UpsertPeer), so a peer
	// whose acknowledgement was lost and which correctly retries its enrolment
	// silently invalidates every roster update it has pushed since. That is
	// invariant 10's legitimate retry punished with data loss rather than with a
	// status code.
	//
	// relay.ValidatePeerEnrollRequest already computes both halves and puts them
	// on the PeerRoster (IdempotencyKey, Fingerprint); before this they were
	// carried all the way here and dropped.
	enrolMemo map[string]rosterMemoEntry
}

// rosterMemoEntry is one peer's last applied roster update.
type rosterMemoEntry struct {
	key         string
	fingerprint idem.Fingerprint
}

// newFederation assembles the ingress, or fails saying which piece was missing.
//
// Every validation is at CONSTRUCTION, following relay's own constructors: a
// startup failure names which side is broken, while a runtime one arrives as a
// peer's unexplained 503 hours later.
func newFederation(opts federationOptions) (*federation, error) {
	log := opts.Logger
	if log == nil {
		log = logging.New(discardWriter{}, logging.LevelError)
	}
	if opts.Local == nil {
		return nil, errors.New("relay wiring: federationOptions.Local is required; without it the acceptor would answer a peer with an acknowledgement while nothing was written")
	}
	if opts.Peers == nil {
		return nil, errors.New("relay wiring: federationOptions.Peers is required; it is both the inbound peer principal resolver and the cross-bus trust, and a federation without it can authenticate no peer and verify no message")
	}
	if opts.LocalAgents == nil {
		return nil, errors.New("relay wiring: federationOptions.LocalAgents is required; \"this bus has no agents\" and \"nobody wired the roster up\" must not look identical to a federating peer")
	}
	if opts.Registry == nil {
		return nil, errors.New("relay wiring: federationOptions.Registry is required; it is the ONE routing table, shared with the egress forwarder and with the hub's remote-recipient admission, and building a second one here would leave a peer that had just handshaked routable on one table and unknown on the other")
	}
	if opts.Outbox == nil {
		return nil, errors.New("relay wiring: federationOptions.Outbox is required; it is the durable per-hop obligation table the ACK plane's anti-forgery rule is computed from (ACK-CONTRACT.md §6.2), and a federation without it could bind no acknowledgement to anything this bus actually wrote")
	}
	if opts.AckLifecycle == nil {
		return nil, errors.New("relay wiring: federationOptions.AckLifecycle is required; a peer acknowledgement route with nowhere durable to record an outcome would answer 200 to a peer and record nothing, which is indistinguishable from working")
	}
	registry := opts.Registry

	// THE ONWARD SEAM (RELAY-47). newOnwardRelay returns a genuinely nil
	// *onwardRelay when there is no forwarder, and it is assigned to an
	// INTERFACE-typed local before it reaches AcceptOptions — a nil *onwardRelay
	// stored straight into the interface field would be non-nil, and the
	// acceptor's `if a.onward != nil` gate would call through it.
	wrapped := newOnwardRelay(opts.BusID, opts.Onward, log)
	var onward relay.OnwardForwarder
	if wrapped != nil {
		onward = wrapped
	}

	acceptor, err := relay.NewAcceptor(relay.AcceptOptions{
		BusID: opts.BusID,
		Local: opts.Local,
		// Onward carries a relay-ingested message to a FURTHER hop. It is nil
		// exactly when this build has no cross-bus forwarder, which
		// AcceptOptions.Onward documents as the legitimate LEAF configuration
		// rather than an omission.
		Onward: onward,
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("relay wiring: %w", err)
	}
	admission, err := newPeerAdmission(opts.AppliedKeyShare, opts.AppliedKeyWindow, opts.MaxConcurrentPerPeer, opts.Now, opts.UnderPressure)
	if err != nil {
		return nil, err
	}

	f := &federation{
		busID:      opts.BusID,
		registry:   registry,
		acceptor:   acceptor,
		admission:  admission,
		acks:       opts.AckLifecycle,
		log:        log,
		onward:     wrapped,
		rosterMemo: make(map[string]rosterMemoEntry),
		enrolMemo:  make(map[string]rosterMemoEntry),
	}

	enroll, err := relay.NewHandler(relay.Config{
		BusID:       opts.BusID,
		LocalRoster: opts.LocalAgents,
		AcceptPeer:  f.acceptPeer,
		Logger:      log,
	})
	if err != nil {
		return nil, fmt.Errorf("relay wiring: %w", err)
	}
	relayIngest, err := relay.NewRelayHandler(relay.RelayConfig{
		BusID:       opts.BusID,
		AcceptRelay: f.acceptRelay,
		Trust:       opts.Peers,
		Logger:      log,
	})
	if err != nil {
		return nil, fmt.Errorf("relay wiring: %w", err)
	}
	roster, err := relay.NewRosterHandler(relay.RosterConfig{
		BusID:  opts.BusID,
		Apply:  f.applyRoster,
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("relay wiring: %w", err)
	}
	ackIngest, err := relay.NewAckHandler(relay.AckConfig{
		BusID: opts.BusID,
		// The SAME table the forwarder wrote the obligation to. relay.AckHandler
		// refuses a nil one at construction, so a federating build that reached
		// here without an outbox fails loudly rather than serving a route that
		// refuses every acknowledgement.
		Obligations: opts.Outbox,
		// THE METER, AND IT IS THE EXISTING ONE. peerAdmission.enter is the
		// per-authenticated-peer in-flight bound RELAY-22 already built for relay
		// ingest, keyed on the same certificate-resolved principal. ACK-3
		// deliberately forks no second abuse-control scheme: the open decision is
		// 48223968 ("Choose the abuse-control primitive for a MULTI-PRINCIPAL
		// relay link"), and when it rules, it rules HERE, in one place, and both
		// peer routes inherit it.
		//
		// The QUOTA half (reserve) is deliberately NOT taken: it counts
		// applied-key entries and an acknowledgement creates none, so charging
		// one would meter this route against a table it does not touch. The
		// CONCURRENCY half is the one that bounds the actual harm — contention on
		// the outbox's exclusive mutex, which is a function of how many
		// acknowledgements are IN FLIGHT rather than of how many arrive per hour.
		Admit:     f.admission.enter,
		SettleAck: f.settleAck,
		Logger:    log,
	})
	if err != nil {
		return nil, fmt.Errorf("relay wiring: %w", err)
	}

	// EVERY FIELD, OR NONE. httpapi.PeerSurface treats a partial surface as "this
	// build does not federate" and registers nothing, so an omission here is a
	// silent outage rather than a compile error — which is why the struct is
	// filled in one literal with no conditional field.
	f.surface = &httpapi.PeerSurface{
		Enroll:   enroll,
		Relay:    relayIngest,
		Roster:   roster,
		Ack:      ackIngest,
		Registry: registry,
		Trust:    opts.Peers,
	}
	return f, nil
}

// Surface is the value for httpapi.Options.Peer.
func (f *federation) Surface() *httpapi.PeerSurface { return f.surface }

// ---------------------------------------------------------------------------
// The three callbacks. Each is an adapter plus a decision function.
// ---------------------------------------------------------------------------

// acceptPeer is relay.Config.AcceptPeer: it reads the authenticated peer out of
// the request context and hands it to the decision function as a PARAMETER.
func (f *federation) acceptPeer(ctx context.Context, peer relay.PeerRoster) error {
	return f.acceptPeerFrom(httpapi.PeerBusIDFromContext(ctx), peer)
}

// acceptPeerFrom records a completed handshake, and ONLY for a peer asserting its
// own id.
//
// The refusal is wrapped in relay.ErrPeerRejected, which the handshake handler
// answers with 403 CodePeerRejected — "we will not have you", final and not
// retryable, which is the accurate answer for a claim the peer cannot fix by
// resending.
func (f *federation) acceptPeerFrom(peerBusID string, peer relay.PeerRoster) error {
	if err := checkPeerAssertsOwnID(peerBusID, peer.BusID); err != nil {
		f.log.Warn("peer handshake REFUSED: the authenticated peer claimed another bus's id. Accepting it would install that bus's roster from a party that does not own it, re-pointing every one of its agents at this peer (invariant 2)",
			"local_bus", f.busID,
			"authenticated_peer", peerBusID,
			"claimed_bus", elidePeerClaim(peer.BusID),
			"claimed_agents", len(peer.Agents),
		)
		return fmt.Errorf("%w: %v", relay.ErrPeerRejected, err)
	}

	key := strings.ToLower(peerBusID)

	// THE WHOLE OF THE REST OF THIS FUNCTION HOLDS f.mu, INCLUDING THE REGISTRY
	// CALL. See applyRosterFrom: a memo consulted outside the critical section
	// that mutates the thing it describes does not prevent the double-apply, it
	// just makes it rarer.
	f.mu.Lock()
	defer f.mu.Unlock()

	if prev, seen := f.enrolMemo[key]; seen && prev.key == peer.IdempotencyKey && peer.IdempotencyKey != "" {
		if prev.fingerprint == peer.Fingerprint {
			// INVARIANT 10, CASE ONE. Re-running UpsertPeer here would be far worse
			// than a wasted write: it RESETS this peer's roster version to 0, so a
			// peer retrying a lost handshake acknowledgement would silently discard
			// every roster update it has pushed since the first one landed.
			f.log.Debug("peer handshake is a duplicate: the original result is replayed, the roster is NOT reinstalled and the peer's roster version is NOT reset",
				"local_bus", f.busID, "peer_bus", peerBusID)
			return nil
		}
		// INVARIANT 10, CASE TWO: rejected and logged, NOBODY DISCONNECTED.
		f.log.Warn("peer handshake REJECTED: this peer reused one idempotency key with a DIFFERENT roster (invariant 10). Rejected and logged, and that is the WHOLE remedy — the peer is deliberately NOT disconnected",
			"local_bus", f.busID, "peer_bus", peerBusID)
		return &enrolViolation{peerBusID: peerBusID}
	}

	if err := f.registry.UpsertPeer(peer); err != nil {
		return err
	}

	// THE ROSTER MEMO IS CLEARED, because the state it describes has just been
	// replaced: UpsertPeer installs a whole new roster and resets the version to
	// 0, so a surviving slot would answer "already applied" for an update that is
	// no longer applied to anything — a 200 over a registry that never saw it.
	delete(f.rosterMemo, key)
	f.enrolMemo[key] = rosterMemoEntry{key: peer.IdempotencyKey, fingerprint: peer.Fingerprint}
	return nil
}

// enrolViolation is invariant 10's violation case ON THE HANDSHAKE SURFACE, and
// it matches TWO sentinels on purpose.
//
// # WHY IT IS NOT JUST fmt.Errorf("%w", relay.ErrIdempotencyViolation)
//
// The handshake handler classifies an AcceptPeer failure with exactly one test —
// errors.Is(err, ErrPeerRejected) → 403 CodePeerRejected — and sends EVERYTHING
// else to a 503 (internal/relay/handshake.go:236). So the obvious wrapping would
// tell a peer that reused a key with different content to RETRY, which is the one
// answer guaranteeing it keeps doing the thing being refused. rosterhttp.go added
// a 409 arm for exactly this and recorded that its absence "told a peer to retry"
// as a real defect; the handshake surface has no such arm and adding one is
// outside this task's file boundary.
//
// So this value reads as BOTH: ErrPeerRejected, so the wire answer is a final
// 403 rather than a retryable 503, and ErrIdempotencyViolation, so anything
// classifying by value gets the precise diagnosis. That double-match is the same
// device internal/idem uses for agentQuotaError (which matches ErrAgentQuota AND
// ErrCapacity) and is used here for the same reason: one refusal genuinely
// belongs to two categories.
//
// FOLLOW-UP: a 409 arm in handshake.go, at which point this should narrow to the
// single sentinel.
type enrolViolation struct{ peerBusID string }

func (e *enrolViolation) Error() string {
	return "relay wiring: peer handshake idempotency key reused with a different roster, from " + e.peerBusID
}

func (e *enrolViolation) Is(target error) bool {
	return target == relay.ErrIdempotencyViolation || target == relay.ErrPeerRejected
}

// applyRoster is relay.RosterConfig.Apply.
func (f *federation) applyRoster(ctx context.Context, u relay.RosterUpdate, idempotencyKey string, fingerprint idem.Fingerprint) error {
	return f.applyRosterFrom(httpapi.PeerBusIDFromContext(ctx), u, idempotencyKey, fingerprint)
}

// applyRosterFrom applies a roster delta from the peer that owns it.
//
// The claim is bound FIRST, before the idempotency memo is consulted, so a peer
// can neither replace another bus's roster nor learn anything about another
// bus's update keys.
//
// The refusal is wrapped in relay.ErrUnknownPeer — 403 CodeUnknownPeer, "you are
// not the bus you say you are" — which is the arm the roster handler already has
// for a caller it will not act on.
func (f *federation) applyRosterFrom(peerBusID string, u relay.RosterUpdate, idempotencyKey string, fingerprint idem.Fingerprint) error {
	if err := checkPeerAssertsOwnID(peerBusID, u.BusID); err != nil {
		f.log.Warn("roster update REFUSED: the authenticated peer claimed another bus's id. One accepted update would REPLACE that bus's routing entries with this peer's (invariant 2)",
			"local_bus", f.busID,
			"authenticated_peer", peerBusID,
			"claimed_bus", elidePeerClaim(u.BusID),
			"version", u.Version,
		)
		return fmt.Errorf("%w: %v", relay.ErrUnknownPeer, err)
	}

	key := strings.ToLower(peerBusID)

	// THE MEMO AND THE APPLY ARE ONE CRITICAL SECTION. Reading the memo, then
	// releasing the lock, then applying is the classic check-then-act: two
	// concurrent copies of ONE retry both miss the memo, both apply, and the
	// second earns the 409 STALE this memo exists to prevent. Holding f.mu across
	// Registry.ApplyRosterUpdate is safe — the registry takes only its own lock
	// and nothing anywhere takes f.mu underneath it — and the section is a map
	// operation plus an in-memory roster delta, not I/O.
	f.mu.Lock()
	defer f.mu.Unlock()

	prev, seen := f.rosterMemo[key]
	if seen && prev.key == idempotencyKey {
		if prev.fingerprint == fingerprint {
			// INVARIANT 10, CASE ONE: same key, same payload is a legitimate
			// retry. The original result is returned, nothing is re-applied, and
			// the peer is NOT punished with a 409 for the crime of retrying after
			// a lost acknowledgement.
			f.log.Debug("roster update is a duplicate: the original result is replayed and nothing is re-applied",
				"local_bus", f.busID, "peer_bus", peerBusID, "version", u.Version)
			return nil
		}
		// INVARIANT 10, CASE TWO: same key, DIFFERENT payload is a protocol
		// violation — rejected and logged, and NOBODY IS DISCONNECTED. Neither
		// payload is echoed: this is precisely the situation where two payloads
		// exist and neither party may be shown the other's.
		f.log.Warn("roster update REJECTED: this peer reused one idempotency key with a DIFFERENT roster delta (invariant 10). Rejected and logged, and that is the WHOLE remedy — the peer is deliberately NOT disconnected",
			"local_bus", f.busID, "peer_bus", peerBusID, "version", u.Version)
		return fmt.Errorf("%w: roster update from %s", relay.ErrIdempotencyViolation, peerBusID)
	}

	if err := f.registry.ApplyRosterUpdate(u); err != nil {
		return err
	}
	f.rosterMemo[key] = rosterMemoEntry{key: idempotencyKey, fingerprint: fingerprint}
	return nil
}

// acceptRelay is relay.RelayConfig.AcceptRelay.
func (f *federation) acceptRelay(ctx context.Context, m relay.RelayedMessage) (relay.RelayAcceptance, error) {
	return f.acceptRelayFrom(ctx, httpapi.PeerBusIDFromContext(ctx), m)
}

// acceptRelayFrom is the metered, connection-bound relay ingest.
//
// THE ORDER IS THE WHOLE OF THIS FUNCTION and every step before the last one
// writes NOTHING:
//
//  1. refuse a request carrying no authenticated peer at all;
//  2. take one of that peer's in-flight slots — BEFORE any other check, so that
//     the CHEAPEST refusal on this surface is itself metered;
//  3. bind the path's last hop to the authenticated peer;
//  4. charge one applied-key entry to that peer;
//  5. only then hand the message to the acceptor, which asks the roster and
//     performs the single durable write (invariant 4).
//
// STEP 2 IS AHEAD OF STEP 3 DELIBERATELY, and it was the other way round until
// the security gate pointed out what that costs: the last-hop refusal is the
// cheapest answer a hostile peer can provoke — no write, no lookup — and it emits
// a Warn line, so leaving it entirely outside the meter meant the cheapest thing
// on this surface was also the only unmetered one. Nothing about the REFUSAL
// changes; what changes is that it now costs the peer one of its own in-flight
// slots while it happens.
//
// BE PRECISE ABOUT WHAT THAT BUYS, because the obvious reading is wrong: this
// bounds CONCURRENCY (maxConcurrentRelayIngestsPerPeer), not RATE. A peer issuing
// requests one after another stays inside its slot count and can still provoke
// one Warn per request. What is bounded is how much of this process one peer can
// occupy at any instant; a true request-rate limit on refusals is not here, and
// this comment should not be read as claiming one.
//
// The charge is REVERSED when the write turned out not to create an entry — a
// duplicate, or a failure — so invariant 10's legitimate retry costs the peer
// nothing.
//
// # KNOWN GAP, RECORDED RATHER THAN LEFT TO BE DISCOVERED
//
// The last-hop refusal reaches the peer as 503 CodeUnavailable, which is
// RETRYABLE, because relayhttp.go's post-callback switch classifies only
// ErrUnknownLocalRecipient (404) and ErrIdempotencyViolation (409) and sends
// everything else to the 503 default. A claim mismatch is PERMANENT and should be
// a final 4xx like its two sibling surfaces (403 on enrol, 403 on roster) — a
// peer that lies about the path will be told to try again for its whole retry
// horizon. Closing it needs a sentinel and an arm inside internal/relay, which is
// outside this task's file boundary; it is filed rather than silently accepted,
// and step 2 above is what bounds the cost in the meantime.
func (f *federation) acceptRelayFrom(ctx context.Context, peerBusID string, m relay.RelayedMessage) (relay.RelayAcceptance, error) {
	if peerBusID == "" {
		// Checked before the meter because there is no peer to meter it against:
		// an empty principal names nobody, and bucketing it would create one
		// shared bucket for every unauthenticated caller. Unreachable through the
		// gated route.
		err := fmt.Errorf("%w: this relayed message arrived with no authenticated peer principal", errPeerClaimMismatch)
		f.log.Warn("relayed message REFUSED and NOTHING was written: it carries no authenticated peer principal, so nothing it asserts can be attributed to a bus",
			"local_bus", f.busID, "origin_message_id", m.OriginMessageID)
		return relay.RelayAcceptance{}, err
	}

	release, err := f.admission.enter(peerBusID)
	if err != nil {
		f.log.Warn("relayed message REFUSED and NOTHING was written: this peer is at its in-flight limit on this bus. It is answered 'not now' rather than 'never', so a correct peer backs off and retries",
			"local_bus", f.busID, "peer_bus", peerBusID, "err", err.Error())
		return relay.RelayAcceptance{}, err
	}
	defer release()

	if err := checkPeerIsLastHop(peerBusID, m.BusPath); err != nil {
		f.log.Warn("relayed message REFUSED and NOTHING was written: the last hop of the traversed path is not the peer that sent it. The path is untrusted input everywhere else, but the hop THIS bus can check is checked (invariant 2)",
			"local_bus", f.busID,
			"authenticated_peer", peerBusID,
			"origin_bus", m.OriginBus,
			"origin_message_id", m.OriginMessageID,
			"path_hops", len(m.BusPath),
		)
		return relay.RelayAcceptance{}, err
	}

	refund, err := f.admission.reserve(peerBusID)
	if err != nil {
		f.log.Warn("relayed message REFUSED and NOTHING was written: this peer is at its share of this bus's applied-key table and the table is under pressure. The share is metered by the AUTHENTICATED PEER, not by the sender label inside the envelope, which a peer chooses",
			"local_bus", f.busID, "peer_bus", peerBusID, "err", err.Error())
		return relay.RelayAcceptance{}, err
	}

	acc, err := f.acceptor.Accept(ctx, m)
	if err != nil || acc.Duplicate {
		// Nothing new landed in the applied-key table, so nothing is owed for it.
		refund()
		return acc, err
	}
	f.warnIfCarriedNoFurther(m)
	return acc, err
}

// warnIfCarriedNoFurther says out loud that a message this bus accepted for
// SOMEBODY ELSE'S agents stops here BECAUSE THIS BUS IS A LEAF.
//
// # IT NOW COVERS ONE OF TWO CASES, AND THE SPLIT IS THE POINT (RELAY-47)
//
// A bus with no cross-bus forwarder has AcceptOptions.Onward nil, which relay
// documents as the legitimate LEAF configuration — and it is, for a bus with one
// neighbour and no transit role. What it is NOT is silent: a peer that relays us
// a message addressed onward gets a 200, the message is durably ours, and it is
// then carried nowhere. That is this function.
//
// The OTHER case — onward relay IS wired and the message went nowhere AT ALL (no
// route, split horizon, hop limit, a fan-out over the bound) — is reported by
// onwardRelay.Enqueue, which is the only place that knows how many copies were
// actually queued. This function returns immediately on such a bus, because its
// text would otherwise assert "this build wires no onward relay" about a build
// that does, and a startup or runtime line that survives the change it describes
// is worse than no line at all.
//
// BETWEEN THEM THEY DO NOT COVER EVERYTHING, AND THE HOLE IS NAMED RATHER THAN
// GLOSSED: a message that reaches SOME of its destination buses and not others
// is logged by neither, because the unroutable recipient is counted inside
// relay.Forwarder.targets without a line and this bus cannot soundly infer it
// from the copy count. See the note in onwardRelay.Enqueue for why the obvious
// heuristic is wrong. Neither fires when the message WAS carried onward.
//
// It is a WARN and it fires per message on purpose: a bus doing real transit will
// make this loud enough to notice, which is the intent. A bus that only ever
// receives mail for its own agents never emits it at all.
func (f *federation) warnIfCarriedNoFurther(m relay.RelayedMessage) {
	if f.onward != nil {
		// Onward relay is wired. Whether this particular message was carried is
		// onwardRelay.Enqueue's answer, and it has already said so.
		return
	}
	foreign := 0
	for _, r := range m.Recipients {
		if bus, _, _, err := ids.ParseAgentID(r); err == nil && !strings.EqualFold(bus, f.busID) {
			foreign++
		}
	}
	if foreign == 0 {
		return
	}
	f.log.Warn("a relayed message was ACCEPTED AND DURABLY RECORDED but is being carried NO FURTHER: it names recipients on another bus, and this build wires no onward relay. The sending peer has been told 200 and will not retry, so those recipients will never receive it",
		"local_bus", f.busID,
		"origin_bus", m.OriginBus,
		"origin_message_id", m.OriginMessageID,
		"foreign_recipients", foreign,
		"remedy", "this bus has no cross-bus forwarder, so it accepts relayed mail for its OWN agents only. A forwarder is built only when the data directory holds peer configuration: run `agent-bus peer add` for the next hop, then restart. Onward relay is wired as soon as one exists",
	)
}

// unreplayedPeerRecords counts federation records the log still holds on a build
// that could not construct a peer store, so the skip is COUNTED rather than
// silent.
//
// auth.MultiplexApplier returns nil for a kind nobody registered, without a word
// — correct for the message and seqfloor records it must ignore, and wrong here:
// invariant 6 requires a discard to be logged loudly AND SPECIFICALLY, and "this
// bus's own peer configuration was replayed into nothing" is exactly the kind of
// silence that invariant exists to forbid. Registering this in the peer store's
// place turns it into a number an operator can act on.
//
// It applies NOTHING. That is the point: on this path there is no store to apply
// to, and the alternative to counting is not applying-anyway, it is silence.
//
// # THE TWO COUNTS ARE SEPARATE BECAUSE THE REMEDIES ARE DIFFERENT
//
// main.go registers this for THREE kinds, not two. Peer and bus-trust records are
// CONFIGURATION: nothing is lost, they are still in the log, and they return in
// full as soon as the store can be built. An OUTBOX record is a DELIVERY THIS BUS
// OWED A PEER — a message it accepted responsibility for — and one replayed into
// nothing is not owed by anything in this run, whatever happens next. Rolling
// both into one number would tell an operator "restart and it comes back" about
// the half where that is not true, so they are counted and reported apart.
//
// (Until 2026-08-15 the outbox kind was registered here and matched by NOTHING in
// the switch below, so it was passed over counting nothing at all — the silent
// discard invariant 6 rates as the actual defect, in the file written to prevent
// it. Three documents asserted it WAS counted.)
type unreplayedPeerRecords struct {
	config atomic.Uint64
	outbox atomic.Uint64
}

// Apply implements wal.Applier. It runs during recovery and on any live commit,
// and never fails — a non-nil error here would poison the log (wal.ErrDiverged)
// over records this build has merely chosen not to serve.
func (u *unreplayedPeerRecords) Apply(c wal.Committed) error {
	switch c.Entry.Kind {
	case relay.PeerRecordKind, relay.BusTrustRecordKind:
		u.config.Add(1)
	case relay.OutboxRecordKind:
		u.outbox.Add(1)
	}
	return nil
}

// Count reports how many federation records were passed over, of every kind.
func (u *unreplayedPeerRecords) Count() uint64 { return u.config.Load() + u.outbox.Load() }

// ConfigCount reports the CONFIGURATION half: peer routes and bus trust. These
// are recoverable — the records are still in the log and return intact once the
// peer store can be built.
func (u *unreplayedPeerRecords) ConfigCount() uint64 { return u.config.Load() }

// OutboxCount reports the DELIVERY half: cross-bus hops this bus owed a peer.
// These are NOT recoverable by this run — nothing owes them — which is why they
// are reported separately from the configuration count.
func (u *unreplayedPeerRecords) OutboxCount() uint64 { return u.outbox.Load() }

// ---------------------------------------------------------------------------
// Whether this build federates at all
// ---------------------------------------------------------------------------

// bindablePeerCount reports how many adjacent buses could actually authenticate
// to this one: an ACTIVE trust record carrying the peer's INBOUND CLIENT
// certificate fingerprint, which is the only thing
// PeerStore.InboundPeerPrincipal will resolve.
//
// # WHY THIS AND NOT "ARE ANY PEERS CONFIGURED"
//
// httpapi's mount refuses to register a surface that would answer 403 to
// everyone, because a registered-and-refusing route advertises that this bus
// federates while serving nobody. A peer store holding routes and signing-key
// pins but NO client-certificate binding is exactly that case: every field of the
// PeerSurface is present, the resolver is present, and no certificate on earth
// resolves to a principal. Counting the bindings is the only way to tell the two
// apart from here.
func bindablePeerCount(store *relay.PeerStore) int {
	if store == nil {
		return 0
	}
	n := 0
	for _, rec := range store.TrustedBuses() {
		if rec.PeerClientTLSCertFingerprint != (buscert.Fingerprint{}) {
			n++
		}
	}
	return n
}

// discardWriter is an io.Writer that drops everything, for the nil-logger
// default. It is a type rather than io.Discard so this file does not need an io
// import for one expression.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---------------------------------------------------------------------------
// The peer acknowledgement callback (ACK-3)
// ---------------------------------------------------------------------------

// settleAck is relay.AckConfig.SettleAck: it makes an ALREADY-AUTHORIZED
// peer-hop terminal outcome durable, and reports whether it was a duplicate.
//
// # WHAT HAS ALREADY HAPPENED BY THE TIME THIS IS CALLED, AND WHAT HAS NOT
//
// HAS: the frame arrived behind RequirePeerPrincipal (layer 1 — WHICH BUS), its
// version, outcome, class and attestation shape were validated against the
// closed sets, and relay.AuthorizePeerAck bound it to an outbox job THIS BUS
// DURABLY WROTE TO THAT PEER for that correlation key (layer 2 — §6.2).
//
// HAS NOT: anything has been verified cryptographically. No bus verifies a
// recipient attestation and none may claim to; the label travelling into the
// record says `recipient_signature_unverified` for exactly that reason.
//
// # THE TWO HALVES OF AUTHORIZATION ARE CONJUNCTIVE, AND THIS IS THE SECOND
//
// The job binding proves the PEER may speak about the KEY. It cannot prove the
// peer may settle THIS RECIPIENT, because an outbox record carries no recipient
// — it is (peer, origin message id) and nothing else identifying. The second
// half is ack.Store's own: a row exists only for a recipient THE SENDER NAMED,
// so a legitimately-bound peer settling on behalf of an agent that was never
// addressed finds no row. ErrNoRecord is therefore translated into relay's
// UNIFORM refusal below — not into a distinguishable "no such recipient", which
// would disclose which recipients a message named.
//
// # INVARIANT 4 IS SATISFIED BY ack.Store.Settle AND NOT BY THIS FUNCTION
//
// Settle writes through the same durable.Write(wal.Entry{...}) path with the
// applied-key record riding in the SAME prepare payload as the effect, one
// fsync. This function adds no ordering of its own, and must not: a second,
// separately ordered write here would leave exactly the window invariant 10
// exists to close.
func (f *federation) settleAck(_ context.Context, s relay.SettledAck) (relay.AckSettlement, error) {
	state, class, attested, err := ackVocabulary(s.Ack)
	if err != nil {
		// OUR bug, not the peer's: the frame passed relay's closed-set validation
		// and this bus could not map it onto the durable vocabulary. Answered as a
		// plain error (503 "not now"), never as a 4xx blaming the sender, and
		// logged loudly — this is the drift the two parallel vocabularies exist to
		// be caught by.
		f.log.Error("REFUSING to record a peer acknowledgement: its outcome or class passed the wire vocabulary and could not be mapped onto the DURABLE one. The two spellings of the closed set have drifted; nothing was written",
			"local_bus", f.busID, "peer_bus", s.PeerBusID, "err", err.Error())
		return relay.AckSettlement{}, err
	}

	// READ-THEN-SETTLE, and the read is ADVISORY. ack.Store.Settle re-decides
	// under its own lock and is the authority; this read exists only to tell an
	// APPLY from a REPLAY for the `duplicate` field, which relay.DecideAck is the
	// single spelling of (invariant 10's three cases).
	//
	// A race here can only mislabel a replay as an apply or the reverse, never
	// change what is recorded: Settle absorbs a byte-identical retry and refuses
	// a different terminal whatever this read saw. Deciding it here INSTEAD of in
	// Settle would be the defect — two answers to "have I applied this?", one of
	// them not under the lock that matters.
	incoming := s.Ack.Terminal()
	prior, hasPrior := f.priorTerminal(s.Ack.CorrelationKey, s.Ack.Recipient)
	decision, err := relay.DecideAck(prior, hasPrior, incoming)
	if err != nil {
		// DecideAck returns AckConflict with ErrAckOutcomeConflict; the handler
		// maps that to 409 reject-and-log and DOES NOT DISCONNECT (§12).
		return relay.AckSettlement{}, err
	}
	if decision == relay.AckReplay {
		// Invariant 10's FIRST case: return the ORIGINAL result, RE-APPLY
		// NOTHING. No durable write is attempted at all, which is what makes
		// "re-apply nothing" structural rather than a promise Settle keeps.
		return relay.AckSettlement{Duplicate: true}, nil
	}

	switch err := f.acks.Settle(s.Ack.CorrelationKey, s.Ack.Recipient, state, class, attested); {
	case err == nil:
		return relay.AckSettlement{Duplicate: false}, nil

	case errors.Is(err, ack.ErrNoRecord):
		// §8.2's "(none)" row. Translated into relay's UNIFORM refusal so that a
		// key we never held, a key held for a DIFFERENT recipient, and a key that
		// has been swept are byte-identical on the wire. Told apart only in the
		// log, by the handler's own redaction point.
		return relay.AckSettlement{}, relay.ErrAckNotBound

	case errors.Is(err, ack.ErrTerminal):
		// The advisory read above lost a race with another transition. Still
		// invariant 10's second case and still reject-and-log: the FIRST terminal
		// stands. Re-spelled as relay's sentinel so the handler's one status
		// mapping covers both the raced and the unraced path.
		return relay.AckSettlement{}, fmt.Errorf("%w: %v", relay.ErrAckOutcomeConflict, err)

	default:
		// Everything else — ErrNotDurable, a WAL failure, ErrConcurrentTransition
		// — is "not now" (503) and NOTHING WAS WRITTEN. A correct peer retries and
		// the terminal outcome is not lost.
		return relay.AckSettlement{}, err
	}
}

// priorTerminal reports the TERMINAL outcome already recorded for this pair, if
// any.
//
// hasPrior is false for a row that exists but is still `accepted` or
// `in_flight`: relay.DecideAck's contract is "no TERMINAL outcome is recorded
// YET", not "the pair is unknown". A non-terminal row is a pair this bus is
// waiting on, and the incoming frame is the first terminal for it.
func (f *federation) priorTerminal(correlationKey, recipient string) (relay.AckTerminal, bool) {
	rec, ok := f.acks.Lookup(correlationKey, recipient)
	if !ok || !rec.State.Terminal() {
		return relay.AckTerminal{}, false
	}
	// ACK-13: relay.AckOutcome IS ack.State and relay.AckClass IS ack.Class (type
	// aliases), so what used to be ParseAckOutcome(rec.State.String()) is a no-op
	// round trip through a spelling. The value is used directly.
	//
	// THE VALIDITY CHECK THAT PARSE WAS ALSO DOING IS KEPT, EXPLICITLY, because
	// both fields come off a DURABLE RECORD and are input to be validated:
	//   - the outcome is checked by rec.State.Terminal() above, which is exactly
	//     what ParseAckOutcome accepted (it ranges the terminal states);
	//   - the class is checked here by Valid().
	// An unrecognised class must NOT be reported as "no prior": that would let a
	// conflicting terminal overwrite a recorded one. It is reported as a prior
	// that matches nothing, so DecideAck answers conflict and Settle's own
	// absorbing check refuses the write.
	outcome := rec.State
	var class relay.AckClass
	if rec.Class != "" {
		if !rec.Class.Valid() {
			return relay.AckTerminal{Outcome: outcome}, true
		}
		class = rec.Class
	}
	return relay.AckTerminal{Outcome: outcome, Class: class}, true
}

// ackVocabulary is the LAST GATE between a validated wire acknowledgement and a
// durable, ABSORBING terminal row.
//
// # IT NO LONGER TRANSLATES, BECAUSE THERE IS ONLY ONE VOCABULARY (ACK-13)
//
// internal/relay/ack.go (ACK-4) and internal/ack/state.go (ACK-2) were written
// concurrently and each declared its own spelling of the twelve classes, the
// three terminal outcomes and the two attestation labels. ACK-13 collapsed them:
// internal/ack is the single home and relay's names are Go type ALIASES for it,
// so relay.AckOutcome IS ack.State and no mapping step remains that could rot.
//
// # IT IS STILL A CHECK, AND IT STILL FAILS CLOSED
//
// The function is kept — rather than the call site simply assigning the fields —
// because a caller reaching Settle with a non-terminal state, an unrecognised
// class or a missing attestation would write, or fail to write, an outcome that
// can NEVER afterwards be corrected. Refusing HERE names the fault; refusing
// inside Settle names only the symptom. An error is answered "not now" (503) and
// nothing is written.
func ackVocabulary(v relay.ValidatedPeerAck) (ack.State, ack.Class, ack.Attestation, error) {
	if !v.Outcome.Terminal() {
		return ack.StateInvalid, "", "", fmt.Errorf("acknowledgement outcome %s is not a terminal durable state; only a terminal outcome may be recorded", v.Outcome)
	}
	if v.Class != "" && !v.Class.Valid() {
		return ack.StateInvalid, "", "", fmt.Errorf("acknowledgement class %s is outside the closed durable set", v.Class)
	}
	if !v.Attestation.Valid() {
		return ack.StateInvalid, "", "", fmt.Errorf("acknowledgement attestation %s is outside the closed durable set", v.Attestation)
	}
	return v.Outcome, v.Class, v.Attestation, nil
}
