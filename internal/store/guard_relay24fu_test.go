package store_test

// RELAY-24-FU-STOREMSGLOOKUP — THE STRUCTURAL GUARD OVER THE POINT LOOKUPS.
//
// # WHAT IS BANNED
//
// No NON-TEST Go file under internal/httpapi/, client/ or cmd/agent-busctl/ may
// name `ByID` or `ByOriginMessageID` as a SELECTOR on anything — called, taken as
// a method value, or otherwise. Those three trees are the request-handling and
// client-facing surfaces; the store's point lookups have no business in any of
// them.
//
// # WHY — this is an AUTHORIZATION boundary, not tidiness
//
// Store.ByID and Store.ByOriginMessageID DO NOT APPLY Message.VisibleTo. That
// one method is the whole authorization boundary of the read path: it carries the
// recipient filter AND the enrolment-epoch check that closed a real audit finding
// on 2026-08-02 (a newly-enrolled agent must not be handed messages sent before
// it enrolled). Unlike Since, these two take NO PRINCIPAL AT ALL — there is no
// agent id to pass and no epoch to compare — so on this surface MISUSE IS THE
// DEFAULT rather than an active mistake: a handler that reaches them is not
// forgetting to pass a principal, it is calling a function that has nowhere to
// put one.
//
// The id namespace makes the consequence concrete. A local message id is
// "<bus-id>-<n>" with a monotone n, which is TRIVIALLY ENUMERABLE. A single
// handler wired to ByID over a client-supplied id therefore hands any
// authenticated agent every retained message on the bus — direct mail addressed
// to someone else, and messages sent before it enrolled — by counting.
//
// # WHAT A FUTURE AUTHOR SHOULD USE INSTEAD
//
// Store.Since. It takes the principal and filters through Message.VisibleTo, and
// it is the only read the request path is entitled to.
//
// # WHY THE GUARD LIVES IN THE TEST SUITE RATHER THAN IN THE SIGNATURE
//
// Because the RELAY RESUME PATH LEGITIMATELY HAS NO PRINCIPAL. It runs at
// startup, rebuilding outbox jobs recorded before the restart, with no client
// connection and no agent to filter against. Requiring a principal in the
// signature would either break that caller or invite a `""` sentinel that
// defeats the filter for everyone — which is strictly worse than a lookup that
// is honest about taking none. So the methods stay principal-free and the
// restriction is on WHO MAY NAME THEM, which is exactly the shape a test can
// enforce and a compiler cannot.
//
// # THIS IS PREVENTION, NOT REMEDIATION
//
// There are ZERO non-test callers in those trees today, and the guard is here to
// keep it that way: the security gate on this task measured the gap at ONE LINE.
// internal/httpapi.Server holds a concrete *hub.Hub, and hub.Hub exports
// `Store() *store.Store` — so any handler is one selector away from an
// unfiltered, enumerable read of the whole serving copy. That escape hatch is
// asserted below so the link is recorded rather than remembered.
//
// It is an AST walk and NOT a grep, for the reason internal/relay/guards_test.go
// and client/guard_test.go are: a grep matches the names in comments, in doc
// prose and in string constants — this very file's doc comment would trip one —
// and the first false positive is what gets a guard deleted rather than fixed.

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/store"
)

// lookupGuardBannedSelectors are the principal-free point lookups. Both skip
// Message.VisibleTo; see the file comment.
var lookupGuardBannedSelectors = map[string]string{
	"ByID":              "Store.ByID resolves a LOCAL message id with no recipient filter and no enrolment-epoch check",
	"ByOriginMessageID": "Store.ByOriginMessageID resolves an ORIGIN message id (falling back to the local id) with the same absent filter",
}

// lookupGuardDirs are the trees no non-test file of which may name those
// selectors, slash-separated and relative to the repository root.
//
// internal/httpapi is the request surface and the one the security gate measured
// as a single line away (see the file comment). client and cmd/agent-busctl are
// the CLIENT side: they talk to a bus over HTTP and have no business holding a
// *store.Store at all, so a reference there is a sign the client has grown a
// server-internal dependency.
//
// The list is EXACT prefixes of a SUBTREE walk: adding internal/httpapi/peer
// would be covered, adding a whole new handler tree would not, and adding it here
// should be a reviewed line rather than a directory quietly appearing.
var lookupGuardDirs = []string{
	"internal/httpapi",
	"client",
	"cmd/agent-busctl",
}

// lookupGuardRef is one banned selector found in one file.
type lookupGuardRef struct {
	file string // display path, for the failure message
	pos  token.Position
	expr string // the selector rendered back to source
	why  string
}

// lookupGuardRepoRoot walks up from the test's working directory to the
// directory containing go.mod.
//
// It FAILS rather than skips if it cannot find one, and it then FAILS again if
// any guarded directory is missing: a guard that silently scans nothing reads as
// green forever, which is the vacuity trap this repository has hit repeatedly.
func lookupGuardRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("the guard cannot determine its working directory, so it cannot locate the repository root: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked up from the test's working directory without finding a go.mod, so the guard cannot locate the repository root and would scan nothing")
		}
		dir = parent
	}
}

// lookupGuardScan is THE DETECTOR: every banned selector in one parsed file.
//
// It matches on ast.SelectorExpr.Sel alone — the selector NAME, whatever it is
// selected from. That is deliberate and it is deliberately broad: the receiver
// may be a *store.Store, a hub accessor chained inline (h.Store().ByID(...)), an
// interface, a local alias or an embedded field, and a guard that tried to prove
// the receiver's type would need go/types and would miss the shapes it could not
// resolve. A false positive here is a name collision a human resolves in one
// line; a false negative is an unfiltered read of the whole bus.
func lookupGuardScan(fset *token.FileSet, file *ast.File, display string) []lookupGuardRef {
	var out []lookupGuardRef
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		why, banned := lookupGuardBannedSelectors[sel.Sel.Name]
		if !banned {
			return true
		}
		out = append(out, lookupGuardRef{
			file: display,
			pos:  fset.Position(sel.Pos()),
			expr: lookupGuardRender(fset, sel),
			why:  why,
		})
		return true
	})
	return out
}

// lookupGuardRender prints an expression back to source on one line, for the
// failure message.
func lookupGuardRender(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, e); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// lookupGuardScanDir parses every non-test .go file under one guarded tree and
// returns the references found plus the number of files actually inspected.
//
// _test.go files are skipped: they are not the production surface, and this
// package's own tests exercise both methods by design.
func lookupGuardScanDir(t *testing.T, root, rel string) ([]lookupGuardRef, int) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// NOT A PASS. A guarded tree that has moved or been renamed leaves this
		// guard inspecting nothing while still reporting green, so it is reported
		// as a failure with the remedy named.
		t.Fatalf("guarded directory %q does not exist under %s (%v).\n"+
			"A guard that scans a missing tree passes forever and proves nothing. If that tree moved or was renamed, update lookupGuardDirs in this file as part of the same change.", rel, root, err)
	}

	fset := token.NewFileSet()
	var refs []lookupGuardRef
	files := 0
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			name := fi.Name()
			if path != dir && (name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this guard cannot parse is a file it cannot police, so it
			// fails rather than skipping: skipping would make "add an unparsable
			// file" an evasion route.
			t.Fatalf("the store lookup guard could not parse %s: %v\nFix or remove that file; a guard cannot police what it cannot parse.", path, perr)
		}
		display := path
		if r, rerr := filepath.Rel(root, path); rerr == nil {
			display = filepath.ToSlash(r)
		}
		files++
		refs = append(refs, lookupGuardScan(fset, parsed, display)...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking guarded directory %s: %v", dir, walkErr)
	}
	if files == 0 {
		t.Fatalf("guarded directory %q contains no non-test .go files; this guard inspected nothing there, which is not a pass", rel)
	}
	return refs, files
}

// TestStorePointLookupsAreNotReachableFromRequestHandlers is the guard.
//
// See the file comment for what it bans and why. It holds three subtests: the
// detector's own self-check, the scan of the guarded trees, and the record of the
// one-line escape hatch that makes the scan worth having.
func TestStorePointLookupsAreNotReachableFromRequestHandlers(t *testing.T) {
	root := lookupGuardRepoRoot(t)

	// THE GUARD CAN ACTUALLY FAIL. Run first, so a broken walk or a broken
	// matcher is reported as a detector failure rather than as a clean tree.
	t.Run("selfCheck_TheDetectorCanActuallyFail", func(t *testing.T) {
		lookupGuardSelfCheck(t)
	})

	t.Run("noReferencesInRequestOrClientSurfaces", func(t *testing.T) {
		total := 0
		for _, rel := range lookupGuardDirs {
			refs, files := lookupGuardScanDir(t, root, rel)
			total += files
			t.Logf("scanned %d non-test .go files under %s", files, rel)
			for _, ref := range refs {
				t.Errorf("%s: %s references the store point lookup `%s`.\n"+
					"\t%s.\n"+
					"\tNeither method applies Message.VisibleTo, which carries BOTH the recipient filter and the enrolment-epoch check — and neither takes a principal, so there is nowhere to put one. Local message ids are \"<bus-id>-<n>\" and trivially enumerable, so a request path wired to either hands any authenticated agent every retained message on the bus, including direct mail addressed to someone else and messages sent before it enrolled.\n"+
					"\tUse Store.Since, which takes the principal and filters. These two exist for the relay resume path, which runs at startup with no principal at all — that is why the restriction is here rather than in the signature.",
					ref.pos, ref.file, ref.expr, ref.why)
			}
		}
		if total == 0 {
			t.Fatalf("scanned 0 files across %v; the guard inspected nothing, which is not a pass", lookupGuardDirs)
		}
	})

	t.Run("escapeHatch_HubStoreIsOneSelectorAway", func(t *testing.T) {
		lookupGuardEscapeHatch(t, root)
	})
}

// TestStoreDuplicateOriginLogIsEmittedOnceAndTheCounterKeepsCounting is the
// evidence for the ONE logic change made alongside the guard above: the
// duplicate-origin-id ERROR line in Append is now emitted ONCE per process.
//
// It lives in this file because it is the proof for that change, and it asserts
// BOTH halves, which is the whole point of the shape:
//
//   - THE LOG IS THROTTLED. The branch is PEER-TRIGGERABLE — the relay-ingest
//     applied-key scope is (sender, relay, origin message id) and the sender label
//     is peer-asserted, so one peer can present one origin id under two sender
//     labels and be admitted twice — and relay ingest is concurrency-limited but
//     NOT rate-limited. An unthrottled ERROR per occurrence is a log-flood a peer
//     drives at will.
//   - THE COUNTER IS NOT. DuplicateOriginMessageIDs must keep counting every
//     occurrence, or throttling the line would have destroyed the signal instead
//     of bounding it. That is the half a "log once" change silently gets wrong.
//
// Invariant 6 is not weakened: nothing is DISCARDED on this branch — both
// messages are retained and delivered — so "every discard must be logged loudly
// and specifically" does not apply, and the one line that is emitted carries the
// full diagnosis.
func TestStoreDuplicateOriginLogIsEmittedOnceAndTheCounterKeepsCounting(t *testing.T) {
	clock := newClock()
	s, logBuf := newOriginLookupStore(clock, store.Options{})
	beta := agentIDFor(t, "beta")
	peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

	// FOUR messages under ONE origin id: three duplicate appends after the first.
	originID := originPeerBusID + "-7"
	for seq := uint64(1); seq <= 4; seq++ {
		m := mkRelayMessageAt(t, peerAlpha, []string{beta}, seq, seq, clock.now(), "copy", originID)
		if err := s.Append(m); err != nil {
			t.Fatalf("Append(seq=%d) = %v, want nil: the record is already committed and fsynced by the time the serving copy sees it (invariant 4), so a refusal orphans it and poisons the hub", seq, err)
		}
	}

	if got := s.DuplicateOriginMessageIDs(); got != 3 {
		t.Fatalf("DuplicateOriginMessageIDs() = %d, want 3.\n"+
			"The counter must record EVERY occurrence. Only the log line is deduplicated; throttling the counter too would turn a bounded log into a lost signal, and this counter is the only queryable evidence the operator has.", got)
	}

	logged := logBuf.String()
	if n := strings.Count(logged, "SAME origin message id"); n != 1 {
		t.Fatalf("the duplicate-origin ERROR line was emitted %d times over 3 duplicates, want exactly 1.\n"+
			"This branch is peer-triggerable and relay ingest is not rate-limited, so an unthrottled line here is a log-flood vector a peer drives. Log once, count always.\n%s", n, logged)
	}
	// The ONE line that is emitted must still carry the full diagnosis — a
	// throttle that also thinned the message would be the silent-suppression
	// defect wearing a different hat.
	for _, want := range []string{originID, peerAlpha, "duplicate_origin_total"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the single duplicate-origin line does not mention %q; the one line that survives the throttle must name the origin id, the sender and the running total:\n%s", want, logged)
		}
	}
}

// lookupGuardSelfCheck feeds the SAME detector synthetic sources and asserts both
// verdicts.
//
// Without it, a detector whose walk is broken — a matcher that matches nothing,
// an ast.Inspect that returns false too early — gives this guard a permanent
// false green over a tree it has stopped reading. That has happened in this
// repository, which is why all three existing guards carry a self-check and why
// this one runs before the real scan.
//
// The NEGATIVE cases matter as much as the positive ones: a detector that fires
// on the name in a comment or in a string constant is a grep wearing an AST's
// clothes, and it would redden on correct work (this file's own doc comment
// names both methods repeatedly).
func lookupGuardSelfCheck(t *testing.T) {
	t.Helper()

	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "a handler calling ByID on a store",
			src: `package httpapi
func (s *Server) handle(id string) {
	m, ok := s.store.ByID(id)
	_, _ = m, ok
}`,
			want: 1,
		},
		{
			name: "the one-line escape hatch: hub.Store().ByID chained inline",
			src: `package httpapi
func (s *Server) handle(id string) {
	m, _ := s.hub.Store().ByID(id)
	_ = m
}`,
			want: 1,
		},
		{
			name: "ByOriginMessageID taken as a METHOD VALUE, never called",
			src: `package httpapi
func (s *Server) wire() func(string) (interface{}, bool) {
	return s.store.ByOriginMessageID
}`,
			want: 1,
		},
		{
			name: "both names, through an interface-typed field",
			src: `package client
type lookup interface{}
func f(l lookup, id string) {
	_ = l.(interface{ ByID(string) }).ByID
	_ = l.(interface{ ByOriginMessageID(string) }).ByOriginMessageID
}`,
			want: 2,
		},
		{
			name: "the names in a COMMENT and a STRING CONSTANT only",
			src: `package httpapi
// This handler must never reach ByID or ByOriginMessageID; use Since.
const note = "ByID and ByOriginMessageID skip VisibleTo"
func (s *Server) handle(agent string, after uint64) {
	_, _ = s.store.Since(agent, after, 0)
}`,
			want: 0,
		},
		{
			name: "an unrelated selector",
			src: `package httpapi
func (s *Server) handle() {
	_ = s.hub.BusID()
}`,
			want: 0,
		},
	}

	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "synthetic.go", c.src, 0)
		if err != nil {
			t.Fatalf("self-check %q: parsing the synthetic source: %v", c.name, err)
		}
		got := lookupGuardScan(fset, f, "synthetic.go")
		if len(got) != c.want {
			t.Fatalf("self-check %q: detector reported %d references, want %d (%v).\n"+
				"The detector is not doing what this guard claims. A detector that cannot fire gives a permanent false green over the request surface.",
				c.name, len(got), c.want, got)
		}
	}
}

// lookupGuardEscapeHatch records, and asserts, the one-line path the security
// gate named: internal/httpapi.Server holds a concrete *hub.Hub, and hub.Hub
// exports `Store() *store.Store`, so a handler is a single selector away from an
// unfiltered read of the serving copy.
//
// It is asserted CONDITIONALLY, in one direction only: IF that accessor still
// exists, THEN internal/httpapi must still be in lookupGuardDirs. Removing the
// accessor CLOSES the hatch and must not turn this red — a guard that fails when
// someone hardens the code is a guard the next agent deletes.
func lookupGuardEscapeHatch(t *testing.T, root string) {
	t.Helper()

	hubDir := filepath.Join(root, "internal", "hub")
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		t.Fatalf("reading %s: %v; this guard's escape-hatch record cannot be checked", hubDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fset := token.NewFileSet()
	found := ""
	for _, n := range names {
		f, perr := parser.ParseFile(fset, filepath.Join(hubDir, n), nil, 0)
		if perr != nil {
			t.Fatalf("parsing internal/hub/%s: %v", n, perr)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Name.Name != "Store" || fd.Type.Results == nil {
				continue
			}
			for _, res := range fd.Type.Results.List {
				if strings.Contains(lookupGuardRender(fset, res.Type), "store.Store") {
					found = fset.Position(fd.Pos()).String()
				}
			}
		}
	}

	if found == "" {
		t.Logf("internal/hub no longer exports an accessor returning *store.Store; the one-selector path from a handler to the point lookups is CLOSED. This guard stays as defence in depth.")
		return
	}
	t.Logf("escape hatch open at %s: hub.Hub.Store() returns *store.Store, so any file holding a *hub.Hub is ONE selector from ByID/ByOriginMessageID", found)

	for _, rel := range lookupGuardDirs {
		if rel == "internal/httpapi" {
			return
		}
	}
	t.Errorf("hub.Hub.Store() (%s) still returns *store.Store, so a handler is one selector away from the unfiltered point lookups — but internal/httpapi is no longer in lookupGuardDirs (%v).\n"+
		"That combination leaves the request surface unguarded while this test still passes everything else. Put internal/httpapi back, or close the hatch.", found, lookupGuardDirs)
}
