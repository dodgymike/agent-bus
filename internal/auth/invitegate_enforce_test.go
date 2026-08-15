package auth_test

// INVITE-GATE-ENFORCE — the ENFORCEMENT half of invariant 3.
//
// INVITE-GATE landed the REDEMPTION half: an invite PRESENTED to /v1/enroll is
// spent atomically with the enrolment it authorises. It deliberately did not
// require one, and said so. This file covers the half that makes invariant 3
// true — "no agent may enrol without redeeming an operator-minted invite ...
// redeeming one is the ONLY way onto the bus" — and, with it, closes the
// permanent roster-exhaustion DoS characterised in rosterdos_test.go.
//
// # What each test pins, and why it is a SEPARATE test
//
// Every assertion below was mutation-tested individually: the guard it covers
// was broken on its own and this test was observed to go RED alone. A guard that
// only fails when another guard is also broken is not tested, and this is an
// auth path.
//
//	the refusal itself            -> TestInviteGateRefusesUninvitedEnrolment
//	the gate does not block invites -> TestInviteGateAdmitsInvitedEnrolment
//	it is OFF by default          -> TestInviteGateDefaultsOpenForEmbedders
//	it runs BEFORE the mint       -> TestInviteGateBurnsNoAgentIDSuffix
//	it runs BEFORE validation     -> TestInviteGateRefusesBeforeValidating
//	it touches no shared state    -> TestInviteGateRefusalRecordsNoIdempotency
//	the DoS is CLOSED             -> TestInviteGateClosesRosterExhaustion
//
// Invariants read IN FULL before writing this file: 3 (enrolment is INVITE-ONLY;
// invites single-use, expiring, revocable, the ONLY way onto the bus, including
// for peer buses — this is the invariant being honoured, not a new restriction),
// 1 (the server is authoritative on every id and ids are NEVER reused, which is
// why a roster slot can never be handed back and why the exhaustion is permanent
// rather than transient — and why the refusal must not burn a suffix), and 10
// (same key + same payload is a legitimate retry; same key + different payload
// is refused WITHOUT a disconnect; only replay of an already-accepted SIGNED
// message disconnects — a refusal here is none of those and drops no socket).

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/auth"
)

// gatedService builds a service with invite-only enrolment ON, over a roster
// that CAN take the composite invited write. Both halves are needed: a gated
// service over a roster with no PutWithInvite could refuse an INVITED enrolment
// for the unrelated reason ErrInviteNotAtomic, and a test that could not tell
// those two refusals apart would prove nothing.
func gatedService(t *testing.T, opts auth.Options) (*auth.Service, *igCompositeRoster) {
	t.Helper()
	roster := newIGCompositeRoster()
	opts.Roster = roster
	opts.RequireInvite = true
	svc, _ := newService(t, opts)
	return svc, roster
}

// TestInviteGateRefusesUninvitedEnrolment is the gate itself: the exact call an
// anonymous attacker makes — a name, a real key, an idempotency key, and NOTHING
// else — is refused, and costs the roster nothing.
func TestInviteGateRefusesUninvitedEnrolment(t *testing.T) {
	svc, roster := gatedService(t, auth.Options{})

	res, err := uninvitedEnrol(t, svc, "attacker", "invitegate-refuse-1")
	if !errors.Is(err, auth.ErrInviteRequired) {
		t.Fatalf("an enrolment with NO invite, NO session and NO client certificate returned (%+v, %v); want auth.ErrInviteRequired.\n"+
			"This is invariant 3: redeeming an operator-minted invite is the ONLY way onto the bus.", res, err)
	}
	if res.AgentID != "" {
		t.Errorf("a refused enrolment returned agent id %q; a refusal must mint nothing", res.AgentID)
	}

	// IT CONSUMED NO SLOT. This is the whole point: the roster cap is the
	// resource the DoS exhausts, so the refusal is worthless if the entry lands
	// anyway.
	if got := roster.Len(); got != 0 {
		t.Fatalf("the roster holds %d entries after a REFUSED enrolment, want 0", got)
	}
}

// TestInviteGateAdmitsInvitedEnrolment is the other side of the gate, and it is
// what stops the fix from being "refuse everything".
//
// A gate that refused invited enrolments too would pass every DoS assertion in
// this file and brick the bus completely.
func TestInviteGateAdmitsInvitedEnrolment(t *testing.T) {
	svc, roster := gatedService(t, auth.Options{})

	inv := newIGFakeInvite("inv-admitted")
	res, err := svc.Enrol(igEnrolReq("welcome", "invitegate-admit-1", inv))
	if err != nil {
		t.Fatalf("an enrolment presenting a valid invite was refused on a gated bus: %v", err)
	}
	if res.AgentID == "" {
		t.Fatal("an admitted enrolment returned an empty agent id")
	}
	if got := roster.Len(); got != 1 {
		t.Fatalf("the roster holds %d entries after an admitted invited enrolment, want 1", got)
	}

	// The invite was actually SPENT, not merely waved past the gate.
	if inv.commits != 1 {
		t.Errorf("the admitted enrolment committed the invite %d times, want 1", inv.commits)
	}
	if inv.aborts != 0 {
		t.Errorf("the admitted enrolment aborted the invite %d times, want 0", inv.aborts)
	}

	// PROVENANCE: the roster records WHICH invite admitted this agent, which is
	// the durable answer to "who authorised this identity onto the bus".
	entry, ok := roster.Get(res.AgentID)
	if !ok {
		t.Fatalf("no roster entry for the admitted enrolment %q", res.AgentID)
	}
	if entry.InviteID != "inv-admitted" {
		t.Errorf("the admitted agent records invite id %q, want %q", entry.InviteID, "inv-admitted")
	}
}

// TestInviteGateDefaultsOpenForEmbedders pins that RequireInvite is FALSE unless
// asked for.
//
// This is not a preference for the insecure default — it is the only value that
// cannot brick a caller that has not been told about the gate, because
// internal/httpapi answers 501 to a presented invite when it was built with no
// invite store, so a defaulted-ON gate would make every such deployment
// unenrollable. cmd/agent-bus sets it true explicitly and wires an invite store
// unconditionally; TestInviteGateShippedServerRequiresAnInvite in
// cmd/agent-bus covers that the SHIPPED bus is gated.
func TestInviteGateDefaultsOpenForEmbedders(t *testing.T) {
	roster := auth.NewMemoryRoster()
	svc, _ := newService(t, auth.Options{Roster: roster}) // RequireInvite NOT set

	if _, err := uninvitedEnrol(t, svc, "embedder", "invitegate-default-1"); err != nil {
		t.Fatalf("an uninvited enrolment against a service built WITHOUT RequireInvite was refused: %v\n"+
			"The default must stay open: flipping it would silently brick every embedder that wires no invite store.", err)
	}
	if got := roster.Len(); got != 1 {
		t.Fatalf("the roster holds %d entries, want 1", got)
	}
}

// TestInviteGateBurnsNoAgentIDSuffix is the PLACEMENT test, and it is the one
// that distinguishes this fix from a fix that merely returns the right error.
//
// Per-name agent-id suffixes are minted from a counter that is NEVER reclaimed
// (point 8 of the ids.NameSuffixes doc) and whose floor is rewritten and FSYNCED
// on every mint. A gate placed AFTER the mint would refuse the enrolment while
// still letting an anonymous caller drive an unbounded, fsyncing write on an
// unauthenticated route — trading a bounded roster DoS for an unbounded disk-IO
// one, on the same route, and every assertion about the roster would still pass.
//
// It is measured, not read off the source: after many refusals the FIRST agent
// admitted under a given name must be that name's suffix 1.
func TestInviteGateBurnsNoAgentIDSuffix(t *testing.T) {
	const refusals = 50
	const name = "same-name"

	svc, _ := gatedService(t, auth.Options{})

	for i := 0; i < refusals; i++ {
		if _, err := uninvitedEnrol(t, svc, name, fmt.Sprintf("invitegate-suffix-%d", i)); !errors.Is(err, auth.ErrInviteRequired) {
			t.Fatalf("uninvited enrolment %d returned %v, want auth.ErrInviteRequired", i, err)
		}
	}

	// The same NAME, now with an invite. If any of the refusals above had
	// reached the minter, this would be name-51.
	res, err := svc.Enrol(igEnrolReq(name, "invitegate-suffix-admitted", newIGFakeInvite("inv-suffix")))
	if err != nil {
		t.Fatalf("the invited enrolment after %d refusals was refused: %v", refusals, err)
	}
	if want := name + "-1"; !strings.HasSuffix(res.AgentID, want) {
		t.Fatalf("after %d REFUSED uninvited enrolments the first admitted agent is %q, want an id ending %q.\n"+
			"A refusal reached the MINTER, so the gate is placed after the mint: an anonymous caller can still burn agent-id suffixes and drive an fsync per request on an unauthenticated route. Move the gate above the mint.",
			refusals, res.AgentID, want)
	}
}

// TestInviteGateRefusesBeforeValidating pins that the gate is the FIRST check,
// ahead of every input validation.
//
// Two things ride on the ordering. It means a refused caller cannot use the
// SHAPE of the error as an oracle for which of its fields the server dislikes —
// every un-invited request gets one identical answer whatever it contains. And
// it means the cheapest possible refusal on the one route that is
// unauthenticated by construction: no validation, no allocation, no lock.
func TestInviteGateRefusesBeforeValidating(t *testing.T) {
	svc, _ := gatedService(t, auth.Options{})

	// Each of these would produce a DIFFERENT, specific error if validation ran
	// first. All must be indistinguishable ErrInviteRequired.
	for _, tc := range []struct {
		label string
		req   auth.EnrolRequest
	}{
		{"empty everything", auth.EnrolRequest{}},
		{"invalid name", auth.EnrolRequest{Name: "not a valid name!!", PublicKey: fixedKey(0x11), IdempotencyKey: "invitegate-order-1"}},
		{"absent public key", auth.EnrolRequest{Name: "fine", IdempotencyKey: "invitegate-order-2"}},
		{"public key of the wrong length", auth.EnrolRequest{Name: "fine", PublicKey: []byte{1, 2, 3}, IdempotencyKey: "invitegate-order-3"}},
		{"empty idempotency key", auth.EnrolRequest{Name: "fine", PublicKey: fixedKey(0x11)}},
		{"oversized idempotency key", auth.EnrolRequest{Name: "fine", PublicKey: fixedKey(0x11), IdempotencyKey: strings.Repeat("k", auth.MaxIdempotencyKeyLen+1)}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, err := svc.Enrol(tc.req)
			if !errors.Is(err, auth.ErrInviteRequired) {
				t.Fatalf("an un-invited enrolment (%s) returned %v, want auth.ErrInviteRequired.\n"+
					"The gate must be the FIRST check: a request that gets a validation error instead has been told which of its fields the server parsed, and has been validated at an anonymous caller's request.", tc.label, err)
			}
		})
	}
}

// TestInviteGateRefusalRecordsNoIdempotency pins that a refusal mutates NOTHING
// — specifically that it does not populate the idempotency table, which is
// itself a capped resource an anonymous caller could otherwise exhaust
// (DefaultMaxIdempotencyEntries, and ErrCapacity when it fills).
//
// Observable consequence: a key burned on a refusal must still be usable. If the
// refusal had recorded the key, the later invited enrolment would come back as a
// REPLAY of a result that never existed.
func TestInviteGateRefusalRecordsNoIdempotency(t *testing.T) {
	const key = "invitegate-reused-key"

	svc, roster := gatedService(t, auth.Options{})

	if _, err := uninvitedEnrol(t, svc, "probe", key); !errors.Is(err, auth.ErrInviteRequired) {
		t.Fatalf("the uninvited enrolment returned %v, want auth.ErrInviteRequired", err)
	}

	// The SAME key, now with an invite. It must be treated as brand new.
	res, err := svc.Enrol(igEnrolReq("probe", key, newIGFakeInvite("inv-reuse")))
	if err != nil {
		t.Fatalf("re-using an idempotency key that was only ever REFUSED was itself refused: %v\n"+
			"A refusal must record nothing, or an anonymous caller can consume the idempotency table AND a client that fixes its request is locked out of its own key.", err)
	}
	if res.Replayed {
		t.Fatal("the invited enrolment came back as a REPLAY; the refused attempt recorded an idempotency entry it must not have")
	}
	if got := roster.Len(); got != 1 {
		t.Fatalf("the roster holds %d entries, want 1", got)
	}
}

// TestInviteGateClosesRosterExhaustion is the DoS closure, stated as the
// attacker's own procedure.
//
// rosterdos_test.go's TestRosterDoSRosterCapacityFailsClosed performs exactly
// this loop against an UNGATED service and fills the roster to its cap. The same
// loop here, against a gated one, must leave the roster EMPTY and the bus
// enrollable — the cap is never approached because the anonymous path never
// reaches the roster at all.
func TestInviteGateClosesRosterExhaustion(t *testing.T) {
	// Deliberately MORE attempts than the injected cap: the attacker does not
	// stop at the cap, and a fix that merely slowed the fill would show up here.
	const cap = 8
	const attempts = 100

	svc, roster := gatedService(t, auth.Options{MaxRosterEntries: cap})

	for i := 0; i < attempts; i++ {
		// The same name every time, as the real attack does: the server mints
		// from its own per-name counter (invariant 1), so no name variety is
		// needed to fill a roster.
		_, err := uninvitedEnrol(t, svc, "attacker", fmt.Sprintf("invitegate-dos-%d", i))
		if !errors.Is(err, auth.ErrInviteRequired) {
			t.Fatalf("anonymous enrolment attempt %d of %d returned %v, want auth.ErrInviteRequired", i+1, attempts, err)
		}
	}

	if got := roster.Len(); got != 0 {
		t.Fatalf("after %d anonymous enrolment attempts the roster holds %d entries, want 0", attempts, got)
	}

	// AND THE BUS IS STILL ENROLLABLE. This is the assertion that separates
	// "the DoS is closed" from "the bus is broken for everyone": a legitimate
	// invited agent gets on immediately after the flood, with no capacity
	// refusal, because the flood consumed nothing.
	res, err := svc.Enrol(igEnrolReq("legitimate", "invitegate-dos-legit", newIGFakeInvite("inv-legit")))
	if err != nil {
		t.Fatalf("after %d anonymous attempts a LEGITIMATE invited enrolment was refused: %v\n"+
			"The flood must cost the bus nothing at all — if this is ErrCapacity, the anonymous path still consumes the resource.", attempts, err)
	}
	if res.AgentID == "" {
		t.Fatal("the legitimate enrolment returned an empty agent id")
	}
	if got := roster.Len(); got != 1 {
		t.Fatalf("the roster holds %d entries, want 1", got)
	}
}
