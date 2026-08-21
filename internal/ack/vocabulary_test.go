package ack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestAckVocabularyHasOneHome is ACK-13's acceptance evidence: the closed ACK
// vocabulary — the twelve NACK classes, the two attestation labels and the
// lifecycle states — is declared HERE and in no second place inside the server.
//
// # WHY A SYNTAX-TREE GUARD AND NOT A COMMENT
//
// A closed enum that exists twice is NOT CLOSED, and its failure mode is silent:
// one side gains a thirteenth member, the other rejects it as unrecognised, and
// the refusal reads as a peer's protocol violation rather than as version skew
// between two of our own packages. That is exactly what happened between
// internal/ack and internal/relay, and nothing went red for it. So the guard is
// mechanical.
//
// It walks internal/relay's non-test sources and fails on a vocabulary spelling
// that reappears as a STRING LITERAL there. A grep would be the wrong tool twice
// over: it matches inside comments (and the reasoning comments in both packages
// legitimately quote the spellings), and it cannot tell a const declaration from
// a structured-log field key.
//
// # THE TWO COPIES THAT REMAIN ARE OUT OF SCOPE, DELIBERATELY
//
//   - client/ack.go keeps its own string constants: client/ may not import
//     internal/ (invariant 7), because the client package is embeddable.
//   - internal/signing keeps its AckClass*/AckOutcome* constants: they are a
//     FROZEN WIRE ALPHABET, and deferring to ack.Class there would make a rename
//     here silently change signed bytes with signer and verifier following the
//     rename together. internal/signing/ackvocab_external_test.go is the drift
//     guard for that seam and it pins against THIS package.
func TestAckVocabularyHasOneHome(t *testing.T) {
	t.Run("internal_relay_declares_no_vocabulary_spelling", func(t *testing.T) {
		dir := relayPackageDir(t)

		// Every spelling in the closed vocabulary, derived from the membership
		// maps rather than typed out again — a hand-written list here would be
		// the very defect this test exists to prevent, one file further away.
		banned := map[string]string{}
		for _, c := range AllClasses() {
			banned[string(c)] = "NACK class"
		}
		for _, a := range AllAttestations() {
			banned[string(a)] = "attestation label"
		}
		for _, s := range AllTerminalStates() {
			banned[s.String()] = "terminal lifecycle state"
		}
		if len(banned) != 17 {
			t.Fatalf("the guard covers %d spellings, want 17 (12 classes + 2 attestations + 3 terminal states); a member was added or two spellings collide", len(banned))
		}

		// TWO CARVE-OUTS, BOTH NARROW, BOTH FOR A SPELLING THAT COLLIDES WITH
		// SOMETHING THAT IS NOT THIS VOCABULARY. Neither is a hole: each names
		// the exact spelling, and the first is refused the moment it appears in a
		// DECLARING position, which is where a re-declared vocabulary would.
		//
		//   - "peer_bus" is also internal/relay's structured-log FIELD KEY for
		//     the adjacent bus id (ackhttp.go, rosterhttp.go, handshake.go, and
		//     ack.go's redaction point). Banning the literal outright would force
		//     renaming an unrelated key operators grep for. Passing it as an
		//     argument is fine; declaring it is not.
		//
		//   - "delivered" is also a member of relay.OutboxState, a DIFFERENT
		//     closed enum (pending, delivered, abandoned) describing where a JOB
		//     is in its hop lifecycle rather than what a recipient said. The two
		//     sets share one word and nothing else, and OutboxState is not part
		//     of the vocabulary ACK-13 collapsed, so outbox.go keeps its own
		//     spelling. The exemption is pinned to that ONE file.
		exempt := map[string]struct {
			files []string // nil = any file
			decls bool     // may it appear in a declaring position?
			why   string
		}{
			"peer_bus":  {files: nil, decls: false, why: "structured-log field key for the adjacent bus id"},
			"delivered": {files: []string{"outbox.go"}, decls: true, why: "relay.OutboxState's own closed enum (pending, delivered, abandoned)"},
		}
		allowed := func(file, spelling string, declaring bool) bool {
			e, ok := exempt[spelling]
			if !ok {
				return false
			}
			if declaring && !e.decls {
				return false
			}
			if e.files == nil {
				return true
			}
			for _, f := range e.files {
				if filepath.Base(file) == f {
					return true
				}
			}
			return false
		}

		for _, file := range goSourceFiles(t, dir) {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatalf("parsing %s: %v", file, err)
			}
			var stack []ast.Node
			ast.Inspect(parsed, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						if kind, isVocab := banned[v]; isVocab {
							var parent ast.Node
							if len(stack) > 0 {
								parent = stack[len(stack)-1]
							}
							declaring := isDeclarationContext(parent)
							if !allowed(file, v, declaring) {
								where := fset.Position(lit.Pos())
								t.Errorf("%s: the %s %q is declared as a string literal in internal/relay (context %T).\n"+
									"THE VOCABULARY HAS ONE HOME: internal/ack. relay consumes it through type ALIASES (relay.AckClass = ack.Class), so it needs no spelling of its own.\n"+
									"Two vocabularies that must agree are two vocabularies that can disagree, and the disagreement is SILENT — one side gains a member, the other refuses it as a protocol violation (ACK-13).",
									where, kind, v, parent)
							}
						}
					}
				}
				stack = append(stack, n)
				return true
			})
		}
	})

	t.Run("the_iteration_helpers_are_derived_from_the_membership_maps", func(t *testing.T) {
		// (b) The helpers must agree with the maps in SIZE and in CONTENT. If
		// they were ever written out by hand they could drift, and every other
		// test in the repo that ranges over them would then be testing the copy.
		classes := AllClasses()
		if got, want := len(classes), len(busClasses)+len(recipientClasses); got != want {
			t.Fatalf("AllClasses has %d members, the membership maps have %d", got, want)
		}
		if got, want := len(classes), 12; got != want {
			t.Fatalf("the class set has %d members, want exactly %d: it is CLOSED (ACK-CONTRACT.md §5.2)", got, want)
		}
		seen := map[Class]bool{}
		for _, c := range classes {
			if seen[c] {
				t.Errorf("AllClasses lists %q twice", string(c))
			}
			seen[c] = true
			if !c.Valid() {
				t.Errorf("AllClasses lists %q, which is not a member of either half", string(c))
			}
			if c.BusEmitted() == c.RecipientEmitted() {
				t.Errorf("class %q is in both halves or neither; the halves are what make `refused` a RECIPIENT claim and `undeliverable` a ROUTING one", string(c))
			}
		}
		for c := range busClasses {
			if !seen[c] {
				t.Errorf("bus class %q is in the membership map and missing from AllClasses", string(c))
			}
		}
		for c := range recipientClasses {
			if !seen[c] {
				t.Errorf("recipient class %q is in the membership map and missing from AllClasses", string(c))
			}
		}
		if got, want := len(AllBusClasses()), 9; got != want {
			t.Errorf("AllBusClasses has %d members, want %d", got, want)
		}
		if got, want := len(AllRecipientClasses()), 3; got != want {
			t.Errorf("AllRecipientClasses has %d members, want %d", got, want)
		}

		attestations := AllAttestations()
		if got, want := len(attestations), 2; got != want {
			t.Fatalf("AllAttestations has %d members, want exactly %d", got, want)
		}
		for _, a := range attestations {
			if !a.Valid() {
				t.Errorf("AllAttestations lists %q, which is not a member", string(a))
			}
		}

		states := AllStates()
		if got, want := len(states), len(stateNames); got != want {
			t.Fatalf("AllStates has %d members, stateNames has %d", got, want)
		}
		if got, want := len(states), 5; got != want {
			t.Fatalf("the lifecycle has %d states, want exactly %d (§8.1)", got, want)
		}
		for _, s := range states {
			if s == StateInvalid {
				t.Errorf("AllStates lists the zero value, which is NEVER valid")
			}
			if _, ok := stateNames[s]; !ok {
				t.Errorf("AllStates lists %v, which stateNames does not name", s)
			}
		}
		if got, want := len(AllTerminalStates()), 3; got != want {
			t.Fatalf("%d states are terminal, want exactly %d (delivered, refused, undeliverable)", got, want)
		}
		for _, s := range AllTerminalStates() {
			if !s.Terminal() {
				t.Errorf("AllTerminalStates lists the non-terminal %s", s)
			}
		}

		// Deterministic order: a test that ranges over these to build a table
		// must get the same table every run.
		if !reflect.DeepEqual(AllClasses(), classes) || !reflect.DeepEqual(AllStates(), states) || !reflect.DeepEqual(AllAttestations(), attestations) {
			t.Error("the iteration helpers are not order-stable between calls; they must not expose Go's randomised map order")
		}
	})

	t.Run("every_member_round_trips_and_a_non_member_is_not_echoed", func(t *testing.T) {
		for _, c := range AllClasses() {
			if got := c.String(); got != string(c) {
				t.Errorf("class %q spells itself %q", string(c), got)
			}
			back, err := ParseClass(c.String())
			if err != nil || back != c {
				t.Errorf("ParseClass(%q) = (%q, %v), want (%q, nil)", c.String(), string(back), err, string(c))
			}
		}
		for _, a := range AllAttestations() {
			back, err := ParseAttestation(a.String())
			if err != nil || back != a {
				t.Errorf("ParseAttestation(%q) = (%q, %v), want (%q, nil)", a.String(), string(back), err, string(a))
			}
		}
		for _, s := range AllStates() {
			back, err := ParseState(s.String())
			if err != nil || back != s {
				t.Errorf("ParseState(%q) = (%v, %v), want (%v, nil)", s.String(), back, err, s)
			}
		}

		// A NON-MEMBER MUST NOT REACH AN OPERATOR LOG VERBATIM. The value comes
		// off the wire and String() is formatted into error text with %s; the
		// uint8 enum this type replaced could only ever print AckClass(200).
		const hostile = "recipient_refused_because_the_body_was_ugly"
		if got := Class(hostile).String(); strings.Contains(got, hostile) {
			t.Errorf("Class(%q).String() = %q: a non-member must not be echoed", hostile, got)
		}
		if got := Attestation("verified").String(); strings.Contains(got, "verified") {
			t.Errorf("Attestation(\"verified\").String() = %q: a non-member must not be echoed", got)
		}
		for _, bad := range []string{"", "custom", "no_route ", "NO_ROUTE", hostile} {
			if _, err := ParseClass(bad); err == nil {
				t.Errorf("ParseClass(%q) succeeded: an unrecognised spelling is REFUSED, never defaulted", bad)
			}
		}
		for _, claim := range []string{"verified", "recipient_signature_verified", "trusted", ""} {
			if _, err := ParseAttestation(claim); err == nil {
				t.Errorf("ParseAttestation(%q) succeeded: there is deliberately NO value meaning verified, because nothing can produce one", claim)
			}
		}
	})

	t.Run("recipient_sourced_is_the_two_states_a_recipient_speaks_for", func(t *testing.T) {
		// Moved here from relay.AckOutcome.RecipientSourced by ACK-13. It decides
		// whether an attestation must be present, and it must answer FALSE
		// outside the terminal set so callers bounds-check FIRST.
		for _, s := range []State{StateDelivered, StateRefused} {
			if !s.RecipientSourced() {
				t.Errorf("%s is not recipient-sourced; plane C is exactly what it is", s)
			}
		}
		for _, s := range []State{StateInvalid, StateAccepted, StateInFlight, StateUndeliverable, State(200)} {
			if s.RecipientSourced() {
				t.Errorf("%s reports itself recipient-sourced; an unchecked value would be labelled with a recipient attestation nobody made", s)
			}
		}
	})
}

// isDeclarationContext reports whether a string literal in this syntactic
// position is DECLARING a value rather than passing one.
//
// It is the shape a re-declared vocabulary takes: a const or var, a String()
// method's return, a switch case, or a comparison against a spelling. Passing a
// literal as a call argument or as an element of a log-field slice is not, which
// is what keeps the "peer_bus" log key legal.
func isDeclarationContext(parent ast.Node) bool {
	switch parent.(type) {
	case *ast.ValueSpec, *ast.ReturnStmt, *ast.CaseClause, *ast.BinaryExpr, *ast.AssignStmt, *ast.KeyValueExpr:
		return true
	default:
		return false
	}
}

// relayPackageDir locates internal/relay from THIS file's compiled-in path, so
// the test does not depend on the working directory. A directory it cannot find
// is a FAILURE, never a skip: a guard that quietly does nothing is worse than no
// guard, because the report says PASS either way.
func relayPackageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not report this test file's path; the guard cannot locate internal/relay")
	}
	dir := filepath.Join(filepath.Dir(self), "..", "relay")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	// Fall back to walking up from the working directory, for a tree laid out
	// somewhere other than where it was compiled.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; ; {
		candidate := filepath.Join(d, "internal", "relay")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	t.Fatalf("cannot locate internal/relay from %s or %s; the one-home guard must not pass by finding nothing to check", filepath.Dir(self), wd)
	return ""
}

// goSourceFiles lists a package's non-test .go files. Test files are excluded
// because a test legitimately writes a spelling to assert on it.
func goSourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no non-test Go source; the guard would pass by checking nothing", dir)
	}
	return out
}
