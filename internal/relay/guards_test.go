package relay

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// modulePath is this package's import path, as it would appear in another
// package's import block.
const modulePath = "github.com/dodgymike/agent-bus/internal/relay"

// repoRoot locates the repository root from this test file's own location, and
// fails rather than skips if it cannot: a guard that quietly does nothing is
// worse than no guard, because it reads as green.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file, so the guard cannot run")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/relay -> internal -> repo root
	// Anchor on two files that must exist, so a wrong root fails loudly here
	// instead of silently walking an empty tree and passing.
	for _, anchor := range []string{"CLAUDE.md", filepath.Join("internal", "httpapi")} {
		if _, err := os.Stat(filepath.Join(root, anchor)); err != nil {
			t.Fatalf("repo root %q does not contain %s (%v); the guard would scan the wrong tree", root, anchor, err)
		}
	}
	return root
}

// TestHandshakeHandlerIsNotWiredIntoAnyMux is the guard the package doc
// promises: NOTHING outside internal/relay may import internal/relay.
//
// The handler authenticates no peer, so serving it would create exactly the
// ungated federation-enrolment path INVITE-PEERGUARD (f5d91dbe) exists to
// forbid, on a link MTLS-RELAYGUARD (8192c3c7) has not yet made mutually
// authenticated. Both must land before this package is reachable from a
// listener — and when they do, THIS TEST is the thing to change deliberately,
// which is the entire point of it failing loudly first.
func TestHandshakeHandlerIsNotWiredIntoAnyMux(t *testing.T) {
	root := repoRoot(t)
	selfDir := filepath.Join(root, "internal", "relay")

	var importers []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch {
			case path == selfDir:
				return filepath.SkipDir
			case info.Name() == ".git", info.Name() == "vendor", info.Name() == "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Both the package itself AND any subpackage of it: a hypothetical
		// internal/relay/mount that re-exported Handler would otherwise bridge
		// this package onto a mux without tripping the guard.
		text := string(src)
		if strings.Contains(text, `"`+modulePath+`"`) || strings.Contains(text, `"`+modulePath+`/`) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			importers = append(importers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(importers) != 0 {
		t.Fatalf("these files import %s: %v\n"+
			"The peer handshake handler authenticates NOTHING and must not be reachable from any mux "+
			"until INVITE-PEERGUARD (f5d91dbe, invite redemption is the only route onto the bus, including "+
			"for peer buses) and MTLS-RELAYGUARD (8192c3c7, bus-to-bus links are mutually authenticated) "+
			"have landed. If you are one of those tasks, change this test deliberately as part of that work.",
			modulePath, importers)
	}
}

// TestPackageDocNamesTheGatingTasks pins the warning in doc.go.
//
// The doc comment IS the control here: it is what the next agent reads before
// deciding whether wiring this up is safe. If the warning is softened or the
// task keys are dropped, the next agent gets a handler that looks ready to
// serve, so the text is asserted rather than trusted.
func TestPackageDocNamesTheGatingTasks(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file")
	}
	docPath := filepath.Join(filepath.Dir(thisFile), "doc.go")
	src, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}
	doc := string(src)

	for _, must := range []string{
		"NOT REGISTERED ON ANY MUX",
		"INVITE-PEERGUARD",
		"f5d91dbe",
		"MTLS-RELAYGUARD",
		"8192c3c7",
	} {
		if !strings.Contains(doc, must) {
			t.Errorf("doc.go no longer contains %q; the package doc is the warning the next agent reads before wiring this handler up", must)
		}
	}
}

// TestPackageDocDoesNotReviveTheWithdrawnDisconnect pins the SUBSTANCE of the
// 2026-08-08 narrowing of invariant 10 into the files a future implementer of
// relay ingest actually reads.
//
// This is a guard and not a comment because the defect it closes was a comment.
// doc.go and relayhttp.go used to instruct MTLS-RELAYGUARD to close the
// connection on idempotency-key reuse, which invariant 10 no longer wants at
// all. Worse, relay is the one surface where a per-socket disconnect is the
// wrong PRIMITIVE regardless of the case: a relay link multiplexes an entire
// peer bus's roster, so dropping it punishes every agent behind that peer for
// one agent's traffic. An implementer who inherited the old instruction would
// either wire a disconnect nothing asks for, or generalise the one legitimate
// third-party-replay disconnect onto a multi-tenant link.
//
// The absence assertion alone would be satisfied by DELETING the paragraph, so
// the presence assertions are the load-bearing half: the replacement must still
// tell the next reader what the rule IS, that the old instruction was withdrawn
// rather than forgotten, and that the multi-principal scoping question is open
// rather than answered.
func TestPackageDocDoesNotReviveTheWithdrawnDisconnect(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file")
	}
	dir := filepath.Dir(thisFile)

	// FIXED SUBSTRINGS WOULD NOT BE ENOUGH, and an earlier version of this guard
	// made exactly that mistake: it matched four literal phrases and therefore
	// missed "disconnect the offending peer" — the VERBATIM pre-narrowing
	// CLAUDE.md wording, i.e. the single most likely form a revival would take.
	//
	// So the rule is CO-OCCURRENCE on a line: any line that talks about
	// disconnecting AND names the party (offending / peer / client / sender /
	// caller) in an instructing voice. That catches a re-wording, and the
	// allowlist below carries the handful of lines that legitimately say the
	// opposite — which is what stops the guard from being satisfied by deleting
	// the explanation.
	// THE WINDOW IS THREE LINES WIDE AND CENTRED, not one line.
	//
	// doc.go wraps at 80 columns, so a sentence about disconnecting routinely
	// spans lines in EITHER direction — the verb on one line and the party on the
	// next, or a negation ("the peer is NOT / disconnected: ...") split the same
	// way. A per-line rule let the first form through verbatim, which is the most
	// likely ACCIDENT rather than an adversarial evasion; and a per-line
	// allowlist then produced a FALSE POSITIVE on the second. Centring on the
	// line that carries the trigger word fixes both, and reporting only that line
	// keeps one finding per occurrence.
	joinWindow := func(lines []string, i int) string {
		lo, hi := i-1, i+2
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		parts := make([]string, 0, hi-lo)
		for _, l := range lines[lo:hi] {
			parts = append(parts, strings.TrimLeft(l, " \t/"))
		}
		return strings.ToLower(strings.Join(parts, " "))
	}
	triggers := []string{"disconnect", "close the connection"}
	hasTrigger := func(l string) bool {
		low := strings.ToLower(l)
		for _, t := range triggers {
			if strings.Contains(low, t) {
				return true
			}
		}
		return false
	}
	// Windows that state the NARROWED rule, or describe the withdrawn one as
	// history, are the POINT of the fix rather than a revival of it. Without
	// these the guard would be satisfied by DELETING the explanation, which is
	// the opposite of what it is for.
	exempt := []string{
		"not disconnect", "not disconnected", "disconnect nobody", "no disconnect",
		"never disconnect", "narrowed", "withdrawn", "wrong primitive",
		"before the 2026-08-08", "until the 2026-08-08", "reject-and-log",
		"it also had correct peers disconnect", "before adding any disconnect",
		"before wiring any disconnect", "legitimately disconnects a single agent",
		"cannot close the connection it does not own",
	}
	parties := []string{"offending", "peer", "client", "sender", "caller"}

	for _, name := range []string{"doc.go", "relayhttp.go", "rosterhttp.go", "message.go", "peer.go"} {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		lines := strings.Split(string(src), "\n")
		for i := range lines {
			if !hasTrigger(lines[i]) {
				continue
			}
			w := joinWindow(lines, i)
			skip := false
			for _, e := range exempt {
				if strings.Contains(w, e) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			named := false
			for _, p := range parties {
				if strings.Contains(w, p) {
					named = true
					break
				}
			}
			if !named {
				continue
			}
			t.Errorf("%s:%d reads as an instruction to disconnect a peer:\n  %s\n"+
				"Invariant 10 was NARROWED on 2026-08-08 (code 1c6c540, contract 0dbb025): same key + DIFFERENT "+
				"payload is rejected and logged and NOTHING ELSE. The one remaining disconnect is third-party replay "+
				"of an accepted signed message, and on a relay link — which multiplexes a whole peer bus's roster — "+
				"a per-socket disconnect is the wrong primitive even for that.", name, i+1, strings.TrimSpace(lines[i]))
		}
	}

	doc, err := os.ReadFile(filepath.Join(dir, "doc.go"))
	if err != nil {
		t.Fatalf("reading doc.go: %v", err)
	}
	for _, must := range []string{
		// The rule itself.
		"REJECT-AND-LOG",
		"THAT INSTRUCTION IS WITHDRAWN",
		// The two questions invariant 10 now mandates, and relay's answer to the
		// second — which is the whole reason the questions exist.
		"Does this connection carry only ONE principal's traffic?",
		"FOR RELAY INGEST THE ANSWER TO (2) IS NO",
		// The scoping decision is named as open, not invented here.
		"OPEN QUESTION",
	} {
		if !strings.Contains(string(doc), must) {
			t.Errorf("doc.go no longer contains %q; the replacement text must state the rule, say the old instruction was withdrawn, "+
				"and leave the multi-principal scoping question explicitly OPEN — deleting the paragraph satisfies the absence check "+
				"while leaving the next implementer with nothing to read", must)
		}
	}
}

// callerFile is runtime.Caller(1) split out so the guard above reads cleanly.
func callerFile() (uintptr, string, bool) {
	pc, file, _, ok := runtime.Caller(1)
	return pc, file, ok
}

// TestIdempotencyKeyCarrierIsTheCanonicalHeader pins relay to internal/idem
// rather than to a second, drifting copy of the key rules.
//
// This package used to re-state the 128-byte cap and the charset. It no longer
// does: IDEM-10 defines ONE carrier (a header, no body field, no fallback) and
// one validator, and a peer-enrol key has to reach the applied-key table
// unchanged (invariant 10). The assertion is behavioural — the handler must
// take the key from idem.HeaderName and nowhere else — so it fails if anyone
// reintroduces a body field as a "convenience".
func TestIdempotencyKeyCarrierIsTheCanonicalHeader(t *testing.T) {
	remote := newResponder(t, localBus, nil, nil)

	// The key in the body, where it used to live, is now an unknown field.
	body := []byte(`{"bus_id":"` + peerBus + `","idempotency_key":"in-the-body","agents":[]}`)
	if status, code := remote.postRaw(t, "application/json", "", body); status != 400 || code != CodeInvalidIdempotencyKey {
		t.Errorf("a body-carried key gave %d/%q, want 400/%q: there is no body carrier and no fallback", status, code, CodeInvalidIdempotencyKey)
	}

	// The same request with the key in the canonical header is accepted.
	ok := []byte(`{"bus_id":"` + peerBus + `","agents":[]}`)
	if status, code := remote.postRaw(t, "application/json", "canonical-key", ok); status != 200 {
		t.Errorf("a header-carried key gave %d/%q, want 200", status, code)
	}
	accepted := remote.acceptedRosters()
	if len(accepted) != 1 || accepted[0].IdempotencyKey != "canonical-key" {
		t.Fatalf("accepted rosters = %+v, want one carrying the header key", accepted)
	}
}
