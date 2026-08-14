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
// NOT WIRED: the EGRESS. relay.Forwarder, its durable Outbox and Resume() are
// not constructed here, so AcceptOptions.Onward is nil — the LEAF configuration
// AcceptOptions.Onward documents as legitimate. A message this bus accepts for
// its OWN agents is delivered; one addressed onward is made durable and carried
// no further. That is stated as a gap rather than implied: cross-bus delivery
// FROM this bus does not work yet, and it cannot be finished here, because the
// local send path has no seam that hands a locally-published message to a
// forwarder at all. Both halves belong to a paired egress task.
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
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/httpapi"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
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
// The federation object
// ---------------------------------------------------------------------------

// federationOptions configures newFederation. Every field without a documented
// default is REQUIRED, and each one is checked: a federation assembled with a
// piece missing looks healthy and silently serves nobody.
type federationOptions struct {
	// BusID is THIS bus's server-minted id (invariant 1).
	BusID string

	// Local is the local bus behind the relay ingress. Production passes
	// hubIngest; it is an interface so the binding and metering rules can be
	// driven without a durable hub.
	Local relay.LocalIngest

	// Peers is the durable, ALREADY-REPLAYED peer store. It serves two distinct
	// jobs — httpapi.Options.PeerPrincipals (which bus is on this connection) and
	// PeerSurface.Trust (which signing keys the ORIGIN bus's messages are verified
	// against) — and they are deliberately different keys pinned at different
	// moments; see PeerStore.InboundPeerPrincipal.
	Peers *relay.PeerStore

	// LocalAgents returns this bus's fully-qualified agent ids for the handshake
	// reply. Called once per handshake, so it must be safe for concurrent use.
	LocalAgents func() []string

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

	registry, err := relay.NewRegistry(relay.RegistryOptions{BusID: opts.BusID, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("relay wiring: %w", err)
	}
	acceptor, err := relay.NewAcceptor(relay.AcceptOptions{
		BusID: opts.BusID,
		Local: opts.Local,
		// Onward is nil: this build accepts relayed messages for its OWN agents
		// and carries nothing further. See the file comment — the egress half is
		// not wired here, and nil is the documented LEAF configuration rather
		// than an omission.
		Onward: nil,
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
		log:        log,
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

	// EVERY FIELD, OR NONE. httpapi.PeerSurface treats a partial surface as "this
	// build does not federate" and registers nothing, so an omission here is a
	// silent outage rather than a compile error — which is why the struct is
	// filled in one literal with no conditional field.
	f.surface = &httpapi.PeerSurface{
		Enroll:   enroll,
		Relay:    relayIngest,
		Roster:   roster,
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
// SOMEBODY ELSE'S agents stops here.
//
// This build wires no egress (AcceptOptions.Onward is nil), which relay
// documents as the legitimate LEAF configuration — and it is, for a bus with one
// neighbour and no transit role. What it is NOT is silent: a peer that relays us
// a message addressed onward gets a 200, the message is durably ours, and it is
// then carried nowhere. The acceptor cannot say so, because a nil Onward is
// exactly what it was told to expect; this is the one place that knows the
// difference between "leaf by design" and "egress not wired yet".
//
// It is a WARN and it fires per message on purpose: a bus doing real transit will
// make this loud enough to notice, which is the intent. A bus that only ever
// receives mail for its own agents never emits it at all.
func (f *federation) warnIfCarriedNoFurther(m relay.RelayedMessage) {
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
		"remedy", "this build accepts relayed mail for its OWN agents only; onward relay needs the egress forwarder, which is not wired yet",
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
type unreplayedPeerRecords struct{ n atomic.Uint64 }

// Apply implements wal.Applier. It runs during recovery and on any live commit,
// and never fails — a non-nil error here would poison the log (wal.ErrDiverged)
// over records this build has merely chosen not to serve.
func (u *unreplayedPeerRecords) Apply(c wal.Committed) error {
	switch c.Entry.Kind {
	case relay.PeerRecordKind, relay.BusTrustRecordKind:
		u.n.Add(1)
	}
	return nil
}

// Count reports how many federation records were passed over.
func (u *unreplayedPeerRecords) Count() uint64 { return u.n.Load() }

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
