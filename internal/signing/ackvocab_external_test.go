// This file is package signing_test — an EXTERNAL test package — for one
// structural reason, and it is internal/attest ALONE: attest imports
// internal/signing, so an in-package test file importing it fails with "import
// cycle not allowed in test". An external test package may import a package
// that depends on it, and that is what lets the disjointness guard below exist.
//
// internal/ack is NOT a reason. It imports only idem, ids, logging and wal, so
// an in-package test could import it perfectly well; it is here because the
// disjointness guard needs attest and the two guards belong together. (An
// earlier draft of this comment claimed ack would be a cycle too. It would not,
// and the paragraph below already said so — a permanent wire format is exactly
// the wrong file to leave a self-contradicting comment in.)
//
// internal/signing itself imports NEITHER, and must not: the production
// dependency runs one way only, and this package deliberately sits at the
// bottom of it.
//
// # Why the vocabulary is pinned against internal/ack and NOT internal/relay
//
// relay declares the same closed vocabulary a second time (ACK-13), and pinning
// against it would be the more obvious choice — relay.AckClass is what the WIRE
// frame parses. But internal/relay carries an architectural guard,
// TestRelayImportedOnlyByWiringSites, restricting its importers to
// internal/httpapi and cmd/agent-bus so that "the mount carries the peer
// principal" stays reviewable (DECISIONS.md 2026-08-08, RELAY-6 ruling (c)).
// A test import from here trips it, and widening wiringSites is an
// architectural decision that belongs to a task that owns internal/relay — not
// to a silent third entry added by a package that only wanted a drift check.
//
// internal/ack imports only idem, ids, logging and wal, is under no such guard,
// and is the DURABLE-RECORD vocabulary — the spellings that reach disk. Pinning
// there is the stronger pin anyway. When ACK-13 collapses the two declarations
// into one, this guard follows the survivor and pins everything.
package signing_test

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/ack"
	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/signing"
)

// TestAckAlphabetMatchesTheDurableVocabulary pins signing's FROZEN alphabet
// against the closed vocabulary the durable ACK record uses.
//
// # Why signing holds its own copy at all
//
// The tokens are encoded as their WIRE SPELLINGS, so the moment a recipient
// signs "recipient_refused_policy" that byte string is permanent for
// AckFormatVersion 3. If signing deferred to ack.Class, renaming a constant
// there would SILENTLY CHANGE THE SIGNED BYTES and invalidate every signature
// ever made — with no test anywhere going red, because signer and verifier
// would both follow the rename together. So the spellings are frozen in
// signing, and this test is the seam that stops the two declarations drifting
// apart unnoticed instead.
//
// If this test goes red, the question is NOT "which spelling do I copy across".
// It is: has a wire spelling changed? If so that is a new AckFormatVersion with
// a new context string, reserved from the signing-format-version namespace —
// never an in-place edit.
//
// This is deliberately NOT a third copy of the twelve-class vocabulary ACK-13
// is about. It is the FORMAT's alphabet: the five tokens a recipient signature
// can contain. The nine bus-emitted classes are absent because a bus-sourced
// outcome carries no attestation for them to appear in.
func TestAckAlphabetMatchesTheDurableVocabulary(t *testing.T) {
	t.Run("outcomes", func(t *testing.T) {
		for _, tc := range []struct {
			frozen string
			state  ack.State
		}{
			{signing.AckOutcomeDelivered, ack.StateDelivered},
			{signing.AckOutcomeRefused, ack.StateRefused},
		} {
			if tc.frozen != tc.state.String() {
				t.Errorf("signing froze outcome %q but the durable record now spells it %q", tc.frozen, tc.state)
			}
			if !tc.state.Terminal() {
				t.Errorf("%q is no longer a terminal state; this format signs only terminal recipient outcomes", tc.frozen)
			}
		}
	})

	t.Run("classes", func(t *testing.T) {
		for _, tc := range []struct {
			frozen string
			class  ack.Class
		}{
			{signing.AckClassRecipientRefusedPolicy, ack.ClassRecipientRefusedPolicy},
			{signing.AckClassRecipientRefusedUndecodable, ack.ClassRecipientRefusedUndecodable},
			{signing.AckClassRecipientRefusedNotAddressed, ack.ClassRecipientRefusedNotAddressed},
		} {
			if tc.frozen != string(tc.class) {
				t.Errorf("signing froze class %q but the durable record now spells it %q", tc.frozen, tc.class)
			}
			if !tc.class.RecipientEmitted() {
				t.Errorf("%q is no longer recipient-emitted; this format signs only recipient-emitted classes", tc.frozen)
			}
		}
	})

	// THE CLOSURE CHECK — and READ THE LIMIT BEFORE TRUSTING IT.
	//
	// What it DOES catch: a class MOVED between the two halves of the set (a
	// recipient class reclassified as bus-emitted, or the reverse), a class
	// RESPELT, and a bus-emitted class wrongly given a frozen spelling.
	//
	// What it CANNOT catch, stated plainly because the comment it replaces
	// claimed the opposite: a THIRTEENTH class added to internal/ack. The list
	// below is a hardcoded literal, so `len(all) != 12` is tautological — it
	// fires only when someone edits this test. internal/ack keeps busClasses
	// and recipientClasses unexported behind the BusEmitted/RecipientEmitted
	// predicates and exports no enumerator, so a package outside it cannot walk
	// the set. (relay.AckClass CAN be walked, over its uint8 range via the
	// String() fallback — but importing internal/relay from here trips
	// TestRelayImportedOnlyByWiringSites, which is the whole reason this pins
	// against internal/ack. See the file head.)
	//
	// A new recipient-emitted class would therefore be accepted on the wire,
	// have no frozen spelling, be refused by CanonicalizeAck at signing time,
	// and NOTHING HERE WOULD GO RED. Closing that needs an exported enumeration
	// from internal/ack, which is outside this task's file boundary and is
	// filed as a follow-up. Until it lands, adding a class to the vocabulary
	// means re-examining the frozen alphabet BY HAND.
	t.Run("no recipient-emitted class is missing from the frozen alphabet", func(t *testing.T) {
		frozen := map[ack.Class]bool{
			ack.Class(signing.AckClassRecipientRefusedPolicy):       true,
			ack.Class(signing.AckClassRecipientRefusedUndecodable):  true,
			ack.Class(signing.AckClassRecipientRefusedNotAddressed): true,
		}
		all := []ack.Class{
			ack.ClassNoRoute, ack.ClassNoSuchRecipient, ack.ClassHopRefused,
			ack.ClassHopUnauthenticated, ack.ClassLoopDropped, ack.ClassFanoutExceeded,
			ack.ClassHorizonExpired, ack.ClassLocalCapacity, ack.ClassObligationLost,
			ack.ClassRecipientRefusedPolicy, ack.ClassRecipientRefusedUndecodable,
			ack.ClassRecipientRefusedNotAddressed,
		}
		// Tautological by construction — `all` is a literal — and kept only as
		// a tripwire for someone editing this list without reading the comment
		// above. It is NOT evidence that the vocabulary is still twelve.
		if len(all) != 12 {
			t.Fatalf("this test enumerates %d classes but the vocabulary is CLOSED at twelve; a member was added or removed and the frozen alphabet must be re-examined BY HAND", len(all))
		}
		recipients := 0
		for _, c := range all {
			if c.BusEmitted() == c.RecipientEmitted() {
				t.Errorf("class %q is in neither half of the closed set, or in both", c)
				continue
			}
			if !c.RecipientEmitted() {
				if frozen[c] {
					t.Errorf("class %q is BUS-emitted but has a frozen spelling in internal/signing; a recipient must not be able to sign a routing claim", c)
				}
				continue
			}
			recipients++
			if !frozen[c] {
				t.Errorf("class %q is recipient-emitted but has no frozen spelling in internal/signing; a recipient could choose a class it cannot sign", c)
			}
		}
		if recipients != len(frozen) {
			t.Errorf("the vocabulary declares %d recipient-emitted classes but the frozen alphabet holds %d", recipients, len(frozen))
		}
	})

	// The negative half of the outcome set: "undeliverable" is a ROUTING claim
	// asserted by a bus. relay.ValidateAckAttestation refuses a signature on
	// one, so signable bytes for it must not exist either — otherwise an agent
	// could produce an attestation for a claim it has no standing to make.
	t.Run("a bus-asserted routing outcome is not signable", func(t *testing.T) {
		for _, s := range []ack.State{ack.StateUndeliverable, ack.StateAccepted, ack.StateInFlight} {
			if _, err := signing.CanonicalizeAck(signing.Ack{
				CorrelationKey:     "bus-a-7",
				Recipient:          "bus-b.bob-2",
				Outcome:            s.String(),
				EmittedAtUnixMilli: 1,
			}); err == nil {
				t.Errorf("CanonicalizeAck produced signable bytes for %q; only the two RECIPIENT-sourced terminal outcomes may be signed", s)
			}
		}
	})
}

// TestAckContextIsDisjointFromEveryOtherSignedLanguage checks the domain
// separation against EVERY language this codebase signs, in one place, rather
// than leaving each pair to whichever package was written last.
//
// The standing requirement it enforces: any future artefact signed by ANY key
// in this system must be added to the table below and must remain disjoint from
// everything already in it. The message and acknowledgement languages SHARE A
// KEY — an agent's messaging key — so for that pair the context prefix is the
// only separation there is.
//
// # The two FRAMING STYLES, which is why this cannot be a simple string compare
//
// The three CANONICAL formats length-prefix their context, so their first bytes
// are a uint32 length. The two SESSION languages are raw string concatenations
// (auth.SessionSigningContext + token), so their first byte is 'a' (0x61).
// Modelling the session contexts as though they were length-prefixed would
// compare a string that is never signed, and the test would assert nothing
// about the real bytes — so each entry declares its own framing.
func TestAckContextIsDisjointFromEveryOtherSignedLanguage(t *testing.T) {
	lengthPrefixed := func(ctx string) string {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ctx)))
		return string(l[:]) + ctx
	}
	raw := func(ctx string) string { return ctx }

	langs := []struct {
		name    string
		key     string // WHICH key signs it — two languages under one key are the dangerous case
		context string
		framing func(string) string
	}{
		{"message", "agent messaging", signing.Context, lengthPrefixed},
		{"acknowledgement", "agent messaging", signing.AckContext, lengthPrefixed},
		{"bus attestation", "bus signing", attest.Context, lengthPrefixed},
		{"agent session", "agent auth", auth.SessionSigningContext, raw},
		{"operator session", "operator auth", auth.OperatorSessionSigningContext, raw},
	}

	// Sanity-check the table itself. This checks each entry's framing is
	// WELL-FORMED — either a raw context, or that context behind a uint32
	// length — and, crucially, that a raw one never begins with 0x00. That last
	// clause is what keeps the two styles mutually unmistakable: a length-framed
	// artefact always starts 0x00, a raw one never does, so the cross-style
	// pairs below are disjoint at their FIRST BYTE regardless of content.
	//
	// It does NOT verify that an entry names the framing its package actually
	// uses; nothing outside those packages can. Adding an entry means reading
	// the signing call site.
	for _, l := range langs {
		got := l.framing(l.context)
		switch {
		case strings.HasSuffix(got, l.context) && len(got) == len(l.context)+4:
			if got[0] != 0x00 {
				t.Fatalf("%s: a length-prefixed framing must begin with the 0x00 of a uint32 length, got %#x", l.name, got[0])
			}
		case got == l.context:
			if got == "" || got[0] == 0x00 {
				t.Fatalf("%s: a raw framing must not begin with 0x00, or it is indistinguishable from a length word", l.name)
			}
		default:
			t.Fatalf("%s: framing helper produced neither a raw nor a length-prefixed form", l.name)
		}
	}

	seen := map[string]string{}
	for _, l := range langs {
		if other, dup := seen[l.context]; dup {
			t.Fatalf("%s and %s share the domain-separation context %q; a signature over one could be replayed as the other", l.name, other, l.context)
		}
		seen[l.context] = l.name
	}

	for _, a := range langs {
		for _, b := range langs {
			if a.name == b.name {
				continue
			}
			fa, fb := a.framing(a.context), b.framing(b.context)
			if len(fa) <= len(fb) && fb[:len(fa)] == fa {
				t.Fatalf("the %s framing (signed by the %s key) is a PREFIX of the %s framing (signed by the %s key); the two languages are not disjoint",
					a.name, a.key, b.name, b.key)
			}
		}
	}
}
