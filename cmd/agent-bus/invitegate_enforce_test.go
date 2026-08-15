package main

// INVITE-GATE-ENFORCE, at the COMPOSITION ROOT.
//
// internal/auth proves the gate refuses. internal/httpapi proves the refusal is
// a 403 and that the bus advertises it. Neither says anything about the binary
// an operator actually runs, and that is the claim that closes the P0: the gate
// is a configuration bit (auth.Options.RequireInvite, false by default), so a
// shipped server that simply never sets it would pass every other test in this
// change while remaining fully vulnerable.
//
// Invariants: 3 (invite-only enrolment is the ONLY way onto the bus), 1 (ids are
// never reused, which is why an exhausted roster is permanent and why this had
// to be closed at the door rather than cleaned up afterwards).

import "testing"

// TestInviteGateShippedServerRequiresAnInvite pins the one bit that decides
// whether the shipped bus is gated.
//
// It is asserted against the LITERAL true rather than against itself, so
// flipping the constant makes this RED. `if enrolmentInviteRequired !=
// enrolmentInviteRequired` would be the classic self-agreeing test.
func TestInviteGateShippedServerRequiresAnInvite(t *testing.T) {
	if !enrolmentInviteRequired {
		t.Fatal("enrolmentInviteRequired is false, so the shipped bus accepts ANONYMOUS enrolments.\n" +
			"That reopens the permanent roster-exhaustion DoS this constant closes: the roster caps at 4096, nothing ever frees a slot, there is no leave route, and agent ids are never reused (invariant 1), so ~4096 unauthenticated POSTs to /v1/enroll brick the bus forever. Memory stays bounded throughout, which is why it does not look like an attack.\n" +
			"Invariant 3: redeeming an operator-minted invite is the ONLY way onto the bus.")
	}
}

// # THERE IS DELIBERATELY NO TEST THAT "THE STARTUP LOG MATCHES THE ENFORCED BIT"
//
// One was written here and deleted before it shipped. It read, in effect,
// `announced := enrolmentInviteRequired; enforced := enrolmentInviteRequired;
// if announced != enforced { t.Fatal(...) }` — a test that CANNOT FAIL, dressed
// up as a guard against the exact defect this task corrects. It would have
// counted as coverage of the announcement while proving nothing at all.
//
// The property is real and is worth stating; it is simply not a test's to make.
// run() passes enrolmentInviteRequired to auth.Options.RequireInvite AND logs it
// as enrolment_invite_required, so the announcement and the behaviour are ONE
// SYMBOL and cannot disagree without an edit that reintroduces a literal. That
// is a compile-time property, and the honest way to hold it is a reviewer
// reading run() — plus TestInviteGateShippedServerRequiresAnInvite above, which
// really can go red, because a real constant really can be flipped.
