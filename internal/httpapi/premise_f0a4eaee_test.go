package httpapi

// A GUARD OVER A JUSTIFICATION, not over behaviour (task f0a4eaee).
//
// Every other test in this package asserts what the server DOES. This one
// asserts why two comments say it does it, and it exists because the defect it
// guards is invisible to every behavioural test in the repository: authmw.go and
// peermount.go both justified the peer-route bearer exemption with a premise a
// security gate refuted — that a peer bus cannot obtain a session token, so a
// bearer requirement would be "unsatisfiable rather than strict", the same shape
// as /v1/session/complete on unauthenticatedRoutes.
//
// The SHIPPED BEHAVIOUR was and is correct: peer routes stay gated by
// RequirePeerPrincipal either way. That is exactly what makes the wrong reason
// dangerous — nothing is broken, so nothing goes red, so it survives review, and
// the next maintainer inherits an argument whose conclusion is "an allow-list is
// safe here" and whose obvious extension is putting a peer path directly on
// unauthenticatedRoutes. That is the one thing two separate guards
// (RequirePeerPrincipal's fail-closed chain, and the derived-not-declared
// s.peerRoutes set) exist to prevent.
//
// # WHY IT IS A TEST AND NOT A REVIEW NOTE
//
// A comment correction with no guard is reverted by the next person who
// reformats the paragraph, and nothing anywhere would notice. Pinning the
// premise costs one file read per run.
//
// # WHAT THIS GUARD DOES AND DOES NOT PROMISE — stated exactly, because a guard
// # whose own comment overclaims is this task's defect one level up
//
// TEN corrections are pinned (four from the task, six added by the reviewer and
// security gates on it), and EVERY ONE is pinned by at least one anchor IN THE
// FILE THAT CARRIES IT. That last clause is not decoration: an earlier version of
// this guard anchored the DECISIONS.md date correction in authmw.go only, and
// both gates independently mutated peermount.go's identical citation back to the
// wrong date and watched this test stay GREEN. A guard that cannot fail on half
// the defect it names is the same non-discriminating shape the repo keeps
// catching in its proof commands, so each half is anchored separately and each
// was mutation-tested on its own.
//
// WHAT GOES RED: a literal revert of any pinned phrasing — the forbidden premise
// reappearing, or a required correction being deleted, reworded or de-emphasised.
// Each anchor fails independently; no correction is protected only by two anchors
// that would have to be removed together.
//
// WHAT DOES NOT GO RED, said plainly: the same false PROPOSITION reasserted in
// fresh words. These are substring anchors, not a semantic check, and the
// corrected prose deliberately paraphrases the false premise ("cannot obtain a
// session token at all") rather than quoting it, so as not to trip its own
// forbidden anchors. Reviewing a rewrite of these paragraphs is still a human
// job; this test only makes a silent revert loud.
//
// # WHY THE SIX GATE-ADDED CORRECTIONS HAVE REQUIRED ANCHORS BUT NO FORBIDDEN ONES
//
// A forbidden anchor is only worth having if it can be OBSERVED failing. The four
// original corrections each replaced text that was in the tree, so their bans
// were seen red at HEAD. The gate-added corrections mostly added text that HEAD
// had no counterpart for at all — HEAD carries no ruling-(i) citation to misdate
// and no narrowing paragraph to weaken — so a ban would have had nothing to
// match and would have passed vacuously from the day it was written. The one
// exception is "deferral", which HEAD did carry — and it is PINNED rather than
// banned: the corrected prose keeps the word once ("ground, not a deferral"), so
// a ban would fire on the correction itself. What protects it is the required
// anchor on its replacement, mutation-tested by restoring the old wording.

import (
	"os"
	"strings"
	"testing"
)

// premiseAnchor is one substring that must be present or absent from a source
// file's prose, with the reason stated so a failure explains itself rather than
// printing a string nobody recognises.
type premiseAnchor struct {
	// file is the source file, relative to this package directory.
	file string

	// text is matched against the file's whitespace-NORMALISED contents, so a
	// gofmt rewrap of the comment does not change the verdict.
	//
	// THE TWO HALVES MATCH DIFFERENTLY, and the asymmetry is deliberate:
	//
	//   - FORBIDDEN anchors fold case. The premise is just as wrong shouted as
	//     whispered, and the original authmw.go wording was "UNSATISFIABLE
	//     rather than strict" against peermount.go's lower-case "unsatisfiable
	//     rather than strict" — one anchor, two spellings. A case-SENSITIVE ban
	//     was tried first and the authmw.go arm PASSED AT HEAD over the live
	//     defect, i.e. it was a guard that could not fail, which is the exact
	//     non-discriminating shape this repo keeps catching in its proofs.
	//   - REQUIRED anchors do not fold case. The emphasis is part of what is
	//     being pinned; a lower-cased restatement of "SATISFIABLE AND WRONG" is
	//     a weaker claim in a comment whose whole job is to stop a maintainer
	//     skimming past the correction.
	text string

	// why explains what a failure means.
	why string
}

// normaliseSource strips comment markers and collapses every run of whitespace
// to a single space.
//
// Without this, an anchor is really an assertion about where gofmt happened to
// break the line, and the guard would fail on a harmless rewrap while a genuine
// reversal that fit on one line would pass. Collapsing is done on the WHOLE
// file rather than on parsed comments because both anchors' subject matter is
// prose, and a premise restated in a string literal or an error message would be
// just as wrong there.
func normaliseSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	s := strings.ReplaceAll(string(b), "//", " ")
	return strings.Join(strings.Fields(s), " ")
}

// TestPeerRouteJustificationIsNotTheRefutedPremise pins the TEN corrections
// listed in the file comment above and fails if any pinned phrasing is reverted,
// each anchor failing independently.
//
// It does NOT catch the same false proposition reasserted in fresh words — see
// "WHAT THIS GUARD DOES AND DOES NOT PROMISE" above, and do not restate the
// promise here more strongly than it is stated there. An earlier version of this
// godoc said "the four corrections" and "fails if any one of them is reverted";
// both halves were false, and the second is exactly the overclaim that let a real
// gap through review — the date correction was anchored in one of the two files
// that carry it, and a mutation of the other passed green.
func TestPeerRouteJustificationIsNotTheRefutedPremise(t *testing.T) {
	// THE REFUTED PREMISE AND THE TWO MISSTATEMENTS BESIDE IT. Each of these was
	// present in the tree before this task, so this half of the guard was
	// observed RED — a forbidden-substring check that never matched anything
	// proves nothing about its own ability to fail.
	forbidden := []premiseAnchor{
		{
			file: "authmw.go",
			text: "unsatisfiable rather than strict",
			why: "the peer-route exemption is justified again by unsatisfiability. It is SATISFIABLE: a peer bus enrols as an ordinary agent " +
				"wherever it peers and can present a valid session token (DECISIONS.md 2026-08-14, the FEDERATION AMENDMENT's ruling (i)). The reason is conflation " +
				"-- a session token names an AGENT and a peer route is BUS-scoped -- and unsatisfiability argues for putting a peer path on " +
				"unauthenticatedRoutes, which mountPeerRoute refuses and which would create an ungated federation ingress",
		},
		{
			file: "authmw.go",
			text: "no route through which it could obtain one",
			why:  "the refuted claim that a peer bus has no way to get a session token is back; enrolment is open to a peer bus like any other client",
		},
		{
			file: "peermount.go",
			text: "unsatisfiable rather than strict",
			why:  "the same refuted premise returned to the mount's own doc comment; see the authmw.go entry",
		},
		{
			file: "peermount.go",
			text: "no route through which it could obtain one",
			why:  "the refuted claim that a peer bus has no way to get a session token is back in the mount's doc comment",
		},
		{
			file: "peermount.go",
			text: "invariant 3's cross-check",
			why: "the cross-check clause is misattributed to invariant 3 again. It is INVARIANT 11's (mTLS and the session token are both required and " +
				"are cross-checked); invariant 3 governs invite-only enrolment and the client-signs-a-server-provided-token direction",
		},
		{
			file: "peermount.go",
			text: "pre-auth prober does not exist",
			why: "the overstatement DECISIONS.md ruling (h) corrected is back: the prober DOES exist -- every enrolled agent on the loopback listener, " +
				"and anything at the far end of the tunnel, can send the unauthenticated probe. What ruling (a) bounds is WHO can ask, not whether anyone can",
		},
	}

	// THE CORRECTED ARGUMENT. Deleting the false premise without replacing it
	// would leave the conclusion (this exemption is safe) standing on nothing,
	// which is the failure mode the task named explicitly, so the true reason is
	// pinned as well as the false one banned.
	required := []premiseAnchor{
		{
			file: "authmw.go",
			text: "SESSION TOKEN NAMES AN AGENT",
			why:  "the conflation argument -- the actual reason the bearer path is skipped on a peer route -- has been removed from authMiddleware",
		},
		{
			file: "authmw.go",
			text: "SATISFIABLE AND WRONG",
			why:  "authMiddleware no longer states that a bearer requirement on a peer route is satisfiable and wrong, which is the whole correction",
		},
		{
			file: "authmw.go",
			text: "DO NOT EXTEND IT BY ADDING A PEER PATH TO unauthenticatedRoutes",
			why:  "the explicit warning against the extension the false premise argued for has been removed",
		},
		{
			file: "peermount.go",
			text: "SATISFIABLE AND WRONG",
			why:  "the mount's doc comment no longer carries the corrected justification",
		},
		{
			file: "peermount.go",
			text: "INVARIANT 11's cross-check",
			why:  "the mount no longer attributes the cross-check clause to invariant 11",
		},
		{
			file: "peermount.go",
			text: "THE PROBER EXISTS",
			why:  "the mount no longer states that the pre-auth prober exists and is merely bounded, which is DECISIONS.md ruling (h)'s correction",
		},
		// The eight below — covering six corrections — were added by the reviewer
		// and security gates on this same task, and they guard the SECOND-ORDER
		// version of the same defect:
		// a correction that is itself slightly wrong, or that states only the
		// flattering half of a narrowing.
		{
			file: "authmw.go",
			text: "NOT the 2026-08-08 FEDERATION section, whose rulings stop at (f)",
			why: "the citation of ruling (i) has lost the correction that it lives in the 2026-08-14 AMENDMENT. The 2026-08-08 FEDERATION section " +
				"contains rulings (a)-(f) ONLY (DECISIONS.md:4340-4432); (g), (h) and (i) are in the amendment (header :4961). Ruling (a), cited " +
				"elsewhere in peermount.go as 2026-08-08, IS correct at that date -- so a right and a wrong date sat side by side in the same format",
		},
		{
			file: "authmw.go",
			text: "NAMED NARROWING",
			why: "authMiddleware no longer records that ruling (i) NARROWS invariant 11 on the peer surface (one factor authorises, not two " +
				"cross-checked). Claiming the conflation argument without the narrowing reads as 'invariant 11 is fully honoured here', which is " +
				"the same wrong-reason-survives-review defect this task exists to remove, one level up",
		},
		// THE DATE LITERAL ITSELF, in each file that carries a ruling-(i)
		// citation. An earlier anchor pinned only the prose AROUND the citation
		// ("so check the date, not just the letter"), and a mutation that
		// rewrote the date to 2026-08-08 while leaving that sentence standing
		// SURVIVED GREEN. Pin the claim, not its neighbour.
		{
			file: "authmw.go",
			text: "2026-08-14, the FEDERATION AMENDMENT's ruling (i)",
			why:  "authmw.go's ruling-(i) citation no longer names the 2026-08-14 amendment; the 2026-08-08 FEDERATION section stops at ruling (f)",
		},
		{
			file: "peermount.go",
			text: "2026-08-14, the FEDERATION AMENDMENT's ruling (i)",
			why:  "peermount.go's ruling-(i) citation no longer names the 2026-08-14 amendment; the 2026-08-08 FEDERATION section stops at ruling (f)",
		},
		{
			file: "peermount.go",
			text: "so check the date, not just the letter",
			why: "peermount.go's OWN citation of ruling (i) has lost the date correction. This anchor exists because the guard once pinned that " +
				"correction in authmw.go alone: both gates mutated THIS file's citation back to 2026-08-08 and the test stayed green, so half " +
				"the defect the reviewer raised as F1 could return unnoticed. Each file's citation is anchored in that file",
		},
		{
			file: "peermount.go",
			text: "an AGENT-scoped token must never be consulted on this surface",
			why: "the qualifier is gone and the claim is absolute again. DECISIONS.md ruling (i) (:5301) reverses the narrowing when a BUS-SCOPED " +
				"bearer credential exists -- one naming the peer bus rather than an agent -- at which point requiring it here becomes right. What " +
				"is banned is the conflation, not the second factor",
		},
		{
			file: "peermount.go",
			text: "NARROWING OF INVARIANT 11, NOT COMPLIANCE WITH IT",
			why:  "the mount no longer records that one factor authorises on the peer surface and what that gives up (no online revocation; no cap on a peer certificate's NotAfter)",
		},
		{
			file: "peermount.go",
			text: "SAME GROUND ruling (b) STANDS ON",
			why: "the mount calls ruling (b) a DEFERRAL again. DECISIONS.md (b-CLARIFIED) (:5171) exists specifically to disclaim that word: " +
				"INVITE-GATE is neither deferred nor deprioritised and remains P0; (b) says only that it does not block the federation critical path",
		},
	}

	// Each file is read once; the anchors are grouped by file only for the
	// failure message's benefit.
	sources := map[string]string{}
	for _, a := range append(append([]premiseAnchor{}, forbidden...), required...) {
		if _, ok := sources[a.file]; !ok {
			sources[a.file] = normaliseSource(t, a.file)
		}
	}

	for _, a := range forbidden {
		a := a
		t.Run("absent/"+a.file+"/"+a.text, func(t *testing.T) {
			// Case-folded; see premiseAnchor.text for why this half folds and
			// the required half does not.
			if strings.Contains(strings.ToLower(sources[a.file]), strings.ToLower(a.text)) {
				t.Errorf("%s contains the refuted premise %q: %s", a.file, a.text, a.why)
			}
		})
	}

	for _, a := range required {
		a := a
		t.Run("present/"+a.file+"/"+a.text, func(t *testing.T) {
			if !strings.Contains(sources[a.file], a.text) {
				t.Errorf("%s no longer contains %q: %s", a.file, a.text, a.why)
			}
		})
	}
}
