// RELAY-16 — EGRESS ADMISSION.
//
// /v1/send may address an agent this bus does not hold, provided a RemoteRouter
// says a peer bus does. Three properties are load-bearing and each has its own
// test below, because each one fails in a different and expensive direction:
//
//   - A NIL ROUTER IS TODAY'S BUS, EXACTLY. The seam lands before anything is
//     wired to it, so the non-federated bus must not be able to tell it is there —
//     same sentinel, same message, same nothing-written.
//   - AN UNROUTABLE RECIPIENT STILL 404s. This is the regression that matters:
//     admission is easy to widen by accident, and the failure mode of widening it
//     is a message durably accepted for an agent that will never receive it. The
//     honest refusal IS the feature.
//   - THE ROSTER STILL DECIDES THIS BUS'S OWN NAMESPACE, before the durable write.
//     A local id admitted by anything but the roster is cca64afd's permanent
//     id-space injury (see the recipient loop in publish).
package hub_test

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// peerBusID is the FOREIGN bus every remote recipient here is qualified with. It
// is deliberately not testBusID: an id in this bus's own namespace is the
// roster's business and is never offered to a router at all.
const peerBusID = "peerbus"

// fakeRouter is a RemoteRouter that answers from a closure and RECORDS every id
// it was asked about.
//
// The recording half is not decoration. Two of the properties below — "a local id
// is never offered to the router" and "a malformed id names nobody" — are
// statements about a call that must NOT happen, and a router that merely answers
// false cannot distinguish "asked and declined" from "never asked". The
// difference is exactly the invariant: the roster, not the router, is the
// authority on this bus's namespace.
type fakeRouter struct {
	mu     sync.Mutex
	answer func(agentID string) (string, bool)
	asked  []string
}

// routeTo builds a router that admits exactly the given recipient, via peer.
func routeTo(recipient, peer string) *fakeRouter {
	return &fakeRouter{answer: func(id string) (string, bool) {
		if id == recipient {
			return peer, true
		}
		return "", false
	}}
}

// routeNothing builds a router that admits nobody — a federated bus whose peer
// set does not contain the recipient.
func routeNothing() *fakeRouter {
	return &fakeRouter{answer: func(string) (string, bool) { return "", false }}
}

// routeAlwaysTo builds a router that answers "yes, via peer" for EVERY id,
// including ones it should never be asked about. It is how a hub-side guard is
// proven to be the thing doing the refusing.
func routeAlwaysTo(peer string) *fakeRouter {
	return &fakeRouter{answer: func(string) (string, bool) { return peer, true }}
}

func (r *fakeRouter) Route(agentID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, agentID)
	if r.answer == nil {
		return "", false
	}
	return r.answer(agentID)
}

// questions returns every id the router was asked about, in order.
func (r *fakeRouter) questions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

// openHubWithRouter opens a real, durable Hub over a fresh wal.Log with router
// injected and one enrolled agent per name.
//
// It builds its own hub rather than extending hub_test.go's helpers so that the
// RELAY-16 wiring is visible in one place: the ONLY difference from
// openHubOverDurable is Options.RemoteRouter, and a reader can see that the nil
// case below is the same call with that one field omitted.
func openHubWithRouter(t *testing.T, router hub.RemoteRouter, agents ...string) (*hub.Hub, *wal.Log) {
	t.Helper()
	return openHubWithRouterLogging(t, router, io.Discard, agents...)
}

// openHubWithRouterLogging is openHubWithRouter with the hub's ERROR log captured
// into logs, for the one property that is only observable there: what the bus
// does NOT print about a value it refused.
func openHubWithRouterLogging(t *testing.T, router hub.RemoteRouter, logs io.Writer, agents ...string) (*hub.Hub, *wal.Log) {
	t.Helper()
	dir := t.TempDir()
	lg := openTestLog(t, dir, true)
	path := lg.Path()
	roster := hub.NewStaticRoster()
	h, err := hub.Open(hub.Options{
		BusID:        testBusID,
		DataDir:      filepath.Dir(path),
		Durable:      lg,
		Replay:       func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex:    lg.Recovered().NextIndex,
		Roster:       roster,
		RemoteRouter: router,
		Logger:       logging.New(logs, logging.LevelError),
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, agents...)
	return h, lg
}

// remoteID is a fully-qualified agent id on the PEER bus (invariant 2).
func remoteID(t *testing.T, name string) string {
	t.Helper()
	return agentID(t, peerBusID, name)
}

// ---------------------------------------------------------------------------
// The routable arm
// ---------------------------------------------------------------------------

// TestSendAdmitsRemoteRecipientViaRemoteRouter is RELAY-16's proof: a recipient
// this bus does not hold is ACCEPTED when the router says a peer holds it, and
// the router is asked about the FULLY-QUALIFIED id it was sent — not a name, not
// a bus, and not something the hub reassembled.
func TestSendAdmitsRemoteRecipientViaRemoteRouter(t *testing.T) {
	to := remoteID(t, "beta")
	router := routeTo(to, peerBusID)
	h, _ := openHubWithRouter(t, router, "alpha")
	sender := agentID(t, testBusID, "alpha")

	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("over the wire"),
		IdempotencyKey: "relay16-accept",
	})
	if err != nil {
		t.Fatalf("Send to routable remote recipient %q = %v, want it accepted; a router that reports the recipient reachable is what RELAY-16 admits", to, err)
	}
	if res.MessageID == "" || res.Seq == 0 {
		t.Fatalf("Send returned %+v, want a server-minted message id and sequence", res)
	}
	if len(res.Recipients) != 1 || res.Recipients[0] != to {
		t.Fatalf("Send recipients = %v, want exactly [%q]: the remote recipient is recorded as addressed, not rewritten to the peer bus", res.Recipients, to)
	}

	// The router must be asked about the recipient EXACTLY as addressed. Anything
	// else means the hub is deriving a routing key of its own, and the peer's
	// answer would then be about a different agent than the message is for.
	asked := router.questions()
	if len(asked) != 1 || asked[0] != to {
		t.Fatalf("router was asked %v, want exactly [%q]", asked, to)
	}
}

// TestSendToRoutableRemoteRecipientIsAccepted proves the DURABILITY half of the
// same arm: acceptance is not "the router took it", it is invariant 4 — the
// message is on disk, readable by a fresh reader, before Send returns.
//
// Handing a message to a router is NOT durability, and this test is what stops
// that shortcut being taken later: the assertion reads the WAL directly rather
// than the serving copy, so an implementation that only enqueued for egress
// would fail here while every in-memory assertion still passed.
func TestSendToRoutableRemoteRecipientIsAccepted(t *testing.T) {
	to := remoteID(t, "beta")
	h, lg := openHubWithRouter(t, routeTo(to, peerBusID), "alpha")
	sender := agentID(t, testBusID, "alpha")

	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("durable before acknowledged"),
		IdempotencyKey: "relay16-durable",
	})
	if err != nil {
		t.Fatalf("Send to routable remote recipient: %v", err)
	}

	msgs := replayMessages(t, lg.Path())
	m, ok := findByID(msgs, res.MessageID)
	if !ok {
		t.Fatalf("message %s was acknowledged but is NOT in the durable log (%d message records replayed); nothing may be acknowledged before it is durable (invariant 4), and handing a message to a router is not durability", res.MessageID, len(msgs))
	}
	if len(m.Recipients) != 1 || m.Recipients[0] != to {
		t.Fatalf("durable record recipients = %v, want [%q]", m.Recipients, to)
	}
	if m.Seq != res.Seq {
		t.Fatalf("durable record seq = %d, acknowledged seq = %d; the sequence is server-minted and must be the one recorded (invariant 1)", m.Seq, res.Seq)
	}
	if m.Sender != sender {
		t.Fatalf("durable record sender = %q, want %q", m.Sender, sender)
	}
}

// ---------------------------------------------------------------------------
// The unroutable arm — the regression that matters
// ---------------------------------------------------------------------------

// TestSendToUnroutableRemoteRecipientIsStillRefused is the guard against widening
// admission by accident. A FEDERATED bus asked about an agent no peer holds must
// still refuse, and must still write nothing.
func TestSendToUnroutableRemoteRecipientIsStillRefused(t *testing.T) {
	to := remoteID(t, "nobody")
	router := routeNothing()
	h, lg := openHubWithRouter(t, router, "alpha")
	sender := agentID(t, testBusID, "alpha")

	before := shapeOf(h)
	beforeDurable := len(replayMessages(t, lg.Path()))

	_, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("addressed to nobody"),
		IdempotencyKey: "relay16-unroutable",
	})
	if !errors.Is(err, hub.ErrUnknownRecipient) {
		t.Fatalf("Send to an unroutable remote recipient = %v, want ErrUnknownRecipient; an id no peer advertises must get the truthful 404, never be accepted and silently dropped", err)
	}
	// The router WAS consulted — this is a refusal on the answer, not a refusal
	// that skipped the question.
	if asked := router.questions(); len(asked) != 1 || asked[0] != to {
		t.Fatalf("router was asked %v, want exactly [%q]", asked, to)
	}
	if after := shapeOf(h); after != before {
		t.Fatalf("store shape %+v -> %+v: a refused send must write nothing", before, after)
	}
	if after := len(replayMessages(t, lg.Path())); after != beforeDurable {
		t.Fatalf("durable message records %d -> %d: a refused send must reach the durable write path not at all", beforeDurable, after)
	}
}

// TestNilRemoteRouterRefusesExactlyAsBefore is the "safe to land unwired" proof.
//
// With no router, a foreign recipient must be refused with the SAME sentinel AND
// the SAME message the bus produced before RELAY-16 existed. The message text is
// asserted deliberately: a federated bus can honestly say "no peer advertises
// it", and a bus with no federation at all must not claim to have looked.
func TestNilRemoteRouterRefusesExactlyAsBefore(t *testing.T) {
	to := remoteID(t, "beta")
	h, lg := openHubWithRouter(t, nil, "alpha") // nil router: the pre-RELAY-16 bus
	sender := agentID(t, testBusID, "alpha")

	before := shapeOf(h)
	beforeDurable := len(replayMessages(t, lg.Path()))

	_, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("no federation here"),
		IdempotencyKey: "relay16-nilrouter",
	})
	if !errors.Is(err, hub.ErrUnknownRecipient) {
		t.Fatalf("Send with a nil router = %v, want ErrUnknownRecipient", err)
	}
	if got, want := err.Error(), `"`+to+`" is not enrolled on this bus`; !strings.HasSuffix(got, want) {
		t.Fatalf("nil-router refusal = %q, want it to end %q; a nil router must reproduce the pre-RELAY-16 refusal exactly, including not implying a peer lookup that never happened", got, want)
	}
	if after := shapeOf(h); after != before {
		t.Fatalf("store shape %+v -> %+v: a refused send must write nothing", before, after)
	}
	if after := len(replayMessages(t, lg.Path())); after != beforeDurable {
		t.Fatalf("durable message records %d -> %d", beforeDurable, after)
	}
}

// TestNilRemoteRouterStillDeliversLocally pins the other half of "behaviourally
// identical": the seam must not have cost the ordinary local send anything.
func TestNilRemoteRouterStillDeliversLocally(t *testing.T) {
	h, lg := openHubWithRouter(t, nil, "alpha", "bravo")
	sender := agentID(t, testBusID, "alpha")
	to := agentID(t, testBusID, "bravo")

	res, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("local as ever"),
		IdempotencyKey: "relay16-local",
	})
	if err != nil {
		t.Fatalf("local Send with a nil router: %v", err)
	}
	if _, ok := findByID(replayMessages(t, lg.Path()), res.MessageID); !ok {
		t.Fatalf("local message %s is not in the durable log", res.MessageID)
	}
}

// ---------------------------------------------------------------------------
// The roster keeps this bus's own namespace — cca64afd's precondition
// ---------------------------------------------------------------------------

// TestRemoteRouterIsNeverAskedAboutThisBusesOwnNamespace is the precondition
// test. A recipient qualified with THIS bus is the roster's business and nobody
// else's: an "always yes" router must not be able to admit one.
//
// The failure this prevents is not a lost message, it is permanent: admitting
// "<local-bus>.alpha-18446744073709551615" pushes that name's durable suffix
// floor to the top of the range and exhausts the name "alpha" for every future
// restart of this bus (cmd/agent-bus/suffixfloors.go). The router is asserted to
// have been asked NOTHING, because "asked and correctly declined" would be a
// guarantee that lives in the router rather than in the hub.
func TestRemoteRouterIsNeverAskedAboutThisBusesOwnNamespace(t *testing.T) {
	router := routeAlwaysTo(peerBusID)
	h, lg := openHubWithRouter(t, router, "alpha")
	sender := agentID(t, testBusID, "alpha")

	// A local id that is NOT enrolled — and the extreme suffix, which is the one
	// that costs the name for ever.
	to, err := ids.AgentID(testBusID, "ghost", 18446744073709551615)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}

	before := shapeOf(h)
	beforeDurable := len(replayMessages(t, lg.Path()))

	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender:         sender,
		To:             to,
		Body:           []byte("local id the roster never minted"),
		IdempotencyKey: "relay16-localnamespace",
	}); !errors.Is(err, hub.ErrUnknownRecipient) {
		t.Fatalf("Send to an unenrolled LOCAL id with an always-yes router = %v, want ErrUnknownRecipient; only the roster may admit an id in this bus's namespace", err)
	}
	if asked := router.questions(); len(asked) != 0 {
		t.Fatalf("router was asked about %v; an id qualified with this bus must never be offered to a router at all — the roster is the authority on this namespace", asked)
	}
	if after := shapeOf(h); after != before {
		t.Fatalf("store shape %+v -> %+v: the roster check must precede the durable write, so a refused local recipient writes nothing", before, after)
	}
	if after := len(replayMessages(t, lg.Path())); after != beforeDurable {
		t.Fatalf("durable message records %d -> %d: nothing durable may be written for an unenrolled local recipient", beforeDurable, after)
	}
}

// TestRemoteRouterIsNeverAskedAboutACaseVariantOfThisBus closes the looser half
// of the same guard.
//
// ids.BusIDPattern admits both cases, so "TESTBUS" is a legal bus id and a
// distinct STRING from "testbus" — but internal/relay compares bus ids with
// EqualFold throughout, so the layer that will implement RemoteRouter considers
// them ONE bus. A guard whose failure mode is a permanently exhausted agent name
// must not be the looser of the two comparisons, so the hub folds as well. The
// outcome for the client is unchanged (still a 404, since the roster is keyed on
// the exact string either way); what changes is that the question is never put to
// the router.
func TestRemoteRouterIsNeverAskedAboutACaseVariantOfThisBus(t *testing.T) {
	router := routeAlwaysTo(peerBusID)
	h, lg := openHubWithRouter(t, router, "alpha")

	to := agentID(t, strings.ToUpper(testBusID), "ghost")
	beforeDurable := len(replayMessages(t, lg.Path()))

	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender:         agentID(t, testBusID, "alpha"),
		To:             to,
		Body:           []byte("case-variant of this bus"),
		IdempotencyKey: "relay16-casevariant",
	}); !errors.Is(err, hub.ErrUnknownRecipient) {
		t.Fatalf("Send to %q (a case variant of this bus, %q) with an always-yes router = %v, want ErrUnknownRecipient", to, testBusID, err)
	}
	if asked := router.questions(); len(asked) != 0 {
		t.Fatalf("router was asked about %v; a case variant of this bus's own id must not be offered to a router — internal/relay folds case, so the hub must not be the looser comparison", asked)
	}
	if after := len(replayMessages(t, lg.Path())); after != beforeDurable {
		t.Fatalf("durable message records %d -> %d", beforeDurable, after)
	}
}

// TestRemoteRouterIsNotAskedAboutLocalEnrolledRecipients pins the cheap half of
// the ordering: a recipient the roster holds is answered by the roster, and the
// router never sees it.
func TestRemoteRouterIsNotAskedAboutLocalEnrolledRecipients(t *testing.T) {
	router := routeNothing()
	h, _ := openHubWithRouter(t, router, "alpha", "bravo")

	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender:         agentID(t, testBusID, "alpha"),
		To:             agentID(t, testBusID, "bravo"),
		Body:           []byte("straight to the roster"),
		IdempotencyKey: "relay16-localhit",
	}); err != nil {
		t.Fatalf("local Send with a router configured: %v", err)
	}
	if asked := router.questions(); len(asked) != 0 {
		t.Fatalf("router was asked about %v; an enrolled local recipient is answered by the roster", asked)
	}
}

// ---------------------------------------------------------------------------
// The router's ANSWER is validated, not trusted
// ---------------------------------------------------------------------------

// TestRouterAnswerIsValidated covers the answers a misconfigured or buggy router
// can give that would turn an honest 404 into a message accepted for delivery to
// a bus that cannot be named or reached. Every one of them must refuse.
func TestRouterAnswerIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer string
	}{
		{"empty peer bus id", ""},
		{"malformed peer bus id", "not a bus id"},          // spaces are not in ids.BusIDPattern
		{"oversized peer bus id", strings.Repeat("x", 65)}, // ValidateBusID caps at 64
		{"the local bus itself", testBusID},                // only the roster admits this namespace
		{"a case variant of the local bus", strings.ToUpper(testBusID)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			to := remoteID(t, "beta")
			var logs strings.Builder
			h, lg := openHubWithRouterLogging(t, routeAlwaysTo(tc.peer), &logs, "alpha")
			sender := agentID(t, testBusID, "alpha")

			beforeDurable := len(replayMessages(t, lg.Path()))
			_, err := mintedSend(t, h, hub.SendRequest{
				Sender:         sender,
				To:             to,
				Body:           []byte("routed nowhere"),
				IdempotencyKey: "relay16-badpeer",
			})
			if !errors.Is(err, hub.ErrUnknownRecipient) {
				t.Fatalf("router answered ok with peer %q and Send returned %v, want ErrUnknownRecipient; a peer id the bus cannot use means the message would be durable and undeliverable", tc.peer, err)
			}
			if after := len(replayMessages(t, lg.Path())); after != beforeDurable {
				t.Fatalf("durable message records %d -> %d: nothing may be written for a recipient that was refused", beforeDurable, after)
			}
			// The refusal must be LOUD (invariant 6: never silent) …
			if got := logs.String(); !strings.Contains(got, "REMOTE ROUTER") {
				t.Fatalf("a refused router answer logged nothing at ERROR; a misconfigured router must not be invisible.\nlogs:\n%s", got)
			}
			// … but it must not ECHO the value. ids.ValidateBusID formats the
			// offending id with %q and caps nothing, so logging its error would put
			// an arbitrarily long unvalidated string into the log; the code reports
			// a LENGTH instead. Pinned here so re-adding the error is caught.
			//
			// The SELF-REFERENTIAL rows are exempt: that branch logs bus_id, which
			// is this bus's OWN configured id and not the router's string, and it
			// happens to equal tc.peer. Own config is always safe to log.
			if tc.peer != "" && !strings.EqualFold(tc.peer, testBusID) && strings.Contains(logs.String(), tc.peer) {
				t.Fatalf("the ERROR line echoes the router's unvalidated peer value (%d bytes); it must report peer_bytes only.\nlogs:\n%s", len(tc.peer), logs.String())
			}
		})
	}
}

// TestMalformedRecipientIsRefusedWithoutConsultingTheRouter pins invariant 2 at
// this seam: an id that is not a fully-qualified "<bus-id>.<agent-id>" names
// nobody, so there is nothing to route and the router is not asked.
func TestMalformedRecipientIsRefusedWithoutConsultingTheRouter(t *testing.T) {
	for _, to := range []string{
		"",                // nobody
		"beta",            // unqualified: no bus
		"peerbus.beta",    // no server-minted suffix
		" peerbus.beta-1", // whitespace-padded
		"peerbus.beta-1 ", // whitespace-padded, trailing
		".beta-1",         // empty bus id
		"peerbus.beta-01", // non-canonical suffix spelling
	} {
		to := to
		t.Run(strings.ReplaceAll(to, " ", "_"), func(t *testing.T) {
			router := routeAlwaysTo(peerBusID)
			h, _ := openHubWithRouter(t, router, "alpha")

			_, err := mintedSend(t, h, hub.SendRequest{
				Sender:         agentID(t, testBusID, "alpha"),
				To:             to,
				Body:           []byte("malformed recipient"),
				IdempotencyKey: "relay16-malformed",
			})
			if err == nil {
				t.Fatalf("Send to malformed recipient %q was accepted; a malformed id names nobody (invariant 2)", to)
			}
			// EITHER sentinel is correct here, and the distinction is not this
			// test's subject. Send parses first and answers ErrInvalidRecipient;
			// routeRemote parses AGAIN because publish has a second, non-client
			// caller in prospect (relay ingest), and on that path an unparseable id
			// is simply not routable. What both must never do is ASK.
			if !errors.Is(err, hub.ErrInvalidRecipient) && !errors.Is(err, hub.ErrUnknownRecipient) {
				t.Fatalf("Send to malformed recipient %q = %v, want ErrInvalidRecipient or ErrUnknownRecipient", to, err)
			}
			if asked := router.questions(); len(asked) != 0 {
				t.Fatalf("router was asked about %v; a malformed recipient is refused before any routing question", asked)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The id-reuse detector stays honest about foreign ids
// ---------------------------------------------------------------------------

// TestRecoveredForeignRecipientsAreNotReportedAsIdReuse is the restart half of
// RELAY-16, and it was found by the reviewer and security gates independently.
//
// Egress admission makes FOREIGN agent ids durable in store.Message.Recipients
// for the first time. The startup id-reuse detector (noteRecoveredIdentities)
// reports every recovered id the roster does not hold, and its claim is strong:
// "a different keypair once held each of these ids, and the names they were
// minted from are free to be enrolled again." A foreign recipient is ALWAYS
// absent from the local roster, so without a filter every federated bus would
// make that false claim about every peer agent it had ever addressed, at every
// start, for ever — drowning the true signal (the shape of a lost suffix-floor
// file) in noise, and re-emitting a client-chosen, uncapped id list each time.
//
// The test restarts a hub over a log holding one LOCAL send and one REMOTE send,
// with the roster deliberately left EMPTY so the detector definitely fires, and
// asserts the line names the local id and NOT the foreign one. Asserting the line
// still fires is half the point: a filter that silenced the detector outright
// would pass a test that only checked the foreign id was absent.
func TestRecoveredForeignRecipientsAreNotReportedAsIdReuse(t *testing.T) {
	dir := t.TempDir()
	lg := openTestLog(t, dir, false)
	path := lg.Path()

	roster := hub.NewStaticRoster()
	remote := remoteID(t, "beta")
	h, err := hub.Open(hub.Options{
		BusID:        testBusID,
		DataDir:      dir,
		Durable:      lg,
		Replay:       func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex:    lg.Recovered().NextIndex,
		Roster:       roster,
		RemoteRouter: routeTo(remote, peerBusID),
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	enrolAll(t, roster, testBusID, "alpha", "bravo")
	sender := agentID(t, testBusID, "alpha")
	local := agentID(t, testBusID, "bravo")

	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender: sender, To: local, Body: []byte("local"), IdempotencyKey: "relay16-recover-local",
	}); err != nil {
		t.Fatalf("local Send: %v", err)
	}
	if _, err := mintedSend(t, h, hub.SendRequest{
		Sender: sender, To: remote, Body: []byte("remote"), IdempotencyKey: "relay16-recover-remote",
	}); err != nil {
		t.Fatalf("remote Send: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	// RESTART with an EMPTY roster: every recovered LOCAL id is then genuinely
	// missing, which is precisely the condition the detector exists to report.
	lg2 := openTestLog(t, dir, true)
	var logs strings.Builder
	if _, err := hub.Open(hub.Options{
		BusID:     testBusID,
		DataDir:   dir,
		Durable:   lg2,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(path, fn) },
		NextIndex: lg2.Recovered().NextIndex,
		Roster:    hub.NewStaticRoster(),
		Logger:    logging.New(&logs, logging.LevelError),
	}); err != nil {
		t.Fatalf("hub.Open (restart): %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "AGENT IDS RECOVERED FROM MESSAGES ARE ABSENT FROM THE ROSTER") {
		t.Fatalf("the id-reuse detector did not fire on a restart with an empty roster; the RELAY-16 filter must remove FOREIGN ids, not disable the check.\nlogs:\n%s", out)
	}
	if !strings.Contains(out, sender) || !strings.Contains(out, local) {
		t.Fatalf("the id-reuse line does not name the local ids %q and %q that really are missing from the roster.\nlogs:\n%s", sender, local, out)
	}
	if strings.Contains(out, remote) {
		t.Fatalf("the id-reuse line names the FOREIGN recipient %q. No keypair on this bus ever held it and no local name was minted from it, so reporting it asserts a reuse that did not happen — and the list is client-chosen, uncapped and re-emitted at every start.\nlogs:\n%s", remote, out)
	}
}
