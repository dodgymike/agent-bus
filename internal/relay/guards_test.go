package relay

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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

// wiringSites are the ONLY directories permitted to import this package, as
// slash-separated paths relative to the repository root.
//
// internal/httpapi is where a mount would be registered; cmd/agent-bus is the
// composition root that builds the pieces and hands them over. Nothing else has
// a reason to name this package — in particular cmd/agent-busctl, the CLI, which
// talks to a bus over HTTP and must never embed the bus-side relay ingress.
//
// This list is EXACT, not a prefix match. A new internal/httpapi/peer package
// would have to be added here deliberately, which is the point: adding a wiring
// site should be a reviewed line in this file rather than a silent consequence
// of creating a directory. Paths are slash-separated so the same strings can be
// rendered straight into the doc assertion below.
var wiringSites = map[string]bool{
	"internal/httpapi": true,
	"cmd/agent-bus":    true,
}

// importedOnlyByPhrase renders wiringSites as the sentence doc.go must contain,
// so that widening the allowlist without saying so in the package doc fails
// TestPackageDocCitesTheRulingThatGovernsMounting. A pinned literal would have
// gone stale silently the first time a third site was added.
func importedOnlyByPhrase() string {
	sites := make([]string, 0, len(wiringSites))
	for d := range wiringSites {
		sites = append(sites, d)
	}
	sort.Strings(sites)
	return "IMPORTED ONLY BY " + strings.Join(sites, " AND ")
}

// scannedFile is one parsed .go file outside this package.
//
// EVERY file is kept, not only the importers. That is the correction the
// security gate forced on the first draft: a guard that reads only the files
// which import this package cannot see the ordinary Go wiring shape, where one
// file holds the handler and ANOTHER — importing nothing from here — registers
// it by path string. internal/httpapi registers every route through a helper
// (server.go: "EVERY route MUST be registered through this"), so the mount site
// need never name this package at all.
type scannedFile struct {
	file string // relative to root, slash-separated, for failure messages
	dir  string // slash-separated, relative to root, matched against wiringSites
	fset *token.FileSet
	ast  *ast.File

	// locals is EVERY name this file can qualify relay identifiers with — a SET,
	// not one name, and blank imports are excluded because they qualify nothing.
	//
	// A scalar was wrong twice, in two different directions, and both were live
	// evasions demonstrated by the security gate: with first-match-wins, a decoy
	// blank-imported SUBPACKAGE in an earlier import group captured the name;
	// with last-write-wins, a DUPLICATE exact import ending in `_` captured it.
	// Go accepts both spellings. A set has no ordering to exploit — and it is
	// also stricter, since a subpackage alias is a real qualifier too.
	locals  map[string]bool
	display string // best name for failure messages
	imports bool   // imports this package or a subpackage, blank import included
	dot     bool   // imported with `.`, which erases the package qualifier
}

// qualifies reports whether id is a name this file can reach relay through.
func (s scannedFile) qualifies(id string) bool { return s.locals[id] }

// scanRepoGoFiles parses every .go file under root outside this package, ONCE,
// and returns them with their internal/relay import resolved.
//
// It is an AST walk and not a grep, for the reason client/guard_test.go is one:
// a grep answers a question nobody asked. At the time of writing, ten files
// outside internal/relay contain the string "internal/relay" in a COMMENT —
// internal/store/message.go, internal/hub/roster.go, internal/attest/* and
// cmd/agent-bus/suffixfloors.go among them — every one of which is a
// cross-reference doing its job. A textual guard survives only while nobody
// writes the next one, and the first false positive is what gets a guard deleted
// rather than fixed. Parsing also means a module path inside a string constant
// or a testdata fixture cannot trip it.
//
// Subpackages count as an import too: a hypothetical internal/relay/mount that
// re-exported RelayHandler would otherwise bridge this package onto a mux from a
// directory the allowlist never mentions.
func scanRepoGoFiles(t *testing.T, root string) []scannedFile {
	t.Helper()
	selfDir := filepath.Join(root, "internal", "relay")
	fset := token.NewFileSet()

	var files []scannedFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path == root {
				return nil
			}
			name := info.Name()
			switch {
			case name == ".git", name == "vendor", name == "node_modules", name == "testdata":
				return filepath.SkipDir
			// The Go toolchain ignores directories starting with "_" or "."; a
			// half-written file parked in one must not redden a guard while
			// `go build ./...` stays green.
			case strings.HasPrefix(name, "_"), strings.HasPrefix(name, "."):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// THIS PACKAGE'S OWN FILES are skipped — but ONLY this directory, never
		// the subtree under it. The security gate demonstrated the difference:
		// with the subtree skipped, an internal/relay/mount package could
		// re-export PeerRosterPath and register it, be imported from
		// allowlisted internal/httpapi, and leave every guard green. The
		// RETIRED ban caught exactly that, so skipping the subtree would have
		// been a regression rather than a narrowing. A subpackage is now
		// scanned like any other package: it is not a wiring site, so importing
		// this package from one fails guard 1, and naming a peer route in one
		// fails guard 3.
		if filepath.Dir(path) == selfDir {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parsedFile, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			// A file this guard cannot parse is a file it cannot police, so it
			// fails rather than skipping. Skipping would make "add an unparsable
			// file" an evasion, and would hide a broken tree behind a green guard.
			// It is NOT necessarily a relay defect: read the position below first.
			t.Fatalf("the relay guards could not parse %s: %v\n"+
				"These guards walk the whole repository, so a syntax error in ANY package fails them. "+
				"Fix (or remove) that file; a guard cannot police what it cannot parse, and skipping "+
				"it would make an unparsable file an evasion route.", path, perr)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		sf := scannedFile{
			file: filepath.ToSlash(rel),
			dir:  filepath.ToSlash(filepath.Dir(rel)),
			fset: fset,
			ast:  parsedFile,
		}
		// EVERY qualifier is collected, and none of them wins over another. See
		// scannedFile.locals for the two evasions that a single scalar allowed.
		sf.locals = map[string]bool{}
		for _, spec := range parsedFile.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			if p != modulePath && !strings.HasPrefix(p, modulePath+"/") {
				continue
			}
			sf.imports = true
			name := p[strings.LastIndex(p, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name == "_" {
				// A blank import binds no name, so it can qualify nothing — and
				// treating it as a qualifier is exactly how it stole one.
				continue
			}
			if name == "." {
				sf.dot = true
				continue
			}
			sf.locals[name] = true
			if sf.display == "" || p == modulePath {
				sf.display = name
			}
		}
		if sf.display == "" {
			sf.display = "relay"
		}
		files = append(files, sf)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return files
}

// TestRelayImportedOnlyByWiringSites replaces the blanket import ban that
// TestHandshakeHandlerIsNotWiredIntoAnyMux used to enforce. RELAY-18 retired
// that ban DELIBERATELY, which is what its own comment asked for, and this is
// what it was replaced BY — the ban was not deleted.
//
// # What changed, and what did not
//
// The old rule was "no file outside internal/relay may name this package". It
// could not survive the composition root: cmd/agent-bus has to be able to build
// a peer store, an outbox and a relay client, and internal/httpapi has to be
// able to mount the routes. A guard that fails the moment correct work starts is
// a guard that gets deleted to make the build go green, and then nothing is left.
//
// What has NOT changed is the property underneath it. These handlers
// authenticate nothing (see doc.go), so what makes wiring them safe is not that
// the package is unreachable, but that it is reachable from a short reviewed
// list, that the ingress cannot be built without a real CrossBusTrust, and that
// no peer route is registered on a mux yet. This guard is the first of those
// three; TestRelayIngressCannotBeBuiltWithoutCrossBusTrust and
// TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile are the others. None is redundant with the
// rest and none may be dropped on the grounds that another covers it.
//
// IMPORTING IS NOT SERVING, and this guard deliberately measures only the first.
// What may be MOUNTED, and behind what principal, is governed by DECISIONS.md
// (2026-08-08, FEDERATION / RELAY-6, landed at 77d2b73) and owned by RELAY-20 —
// see doc.go.
func TestRelayImportedOnlyByWiringSites(t *testing.T) {
	root := repoRoot(t)
	all := scanRepoGoFiles(t, root)
	if len(all) == 0 {
		t.Fatalf("parsed 0 .go files under %s; this guard inspected nothing, which is not a pass", root)
	}

	for _, s := range all {
		if !s.imports {
			continue
		}
		// A dot-import is refused even at a wiring site. It erases the package
		// qualifier, so relay.RelayConfig becomes RelayConfig and every
		// selector-based check in this file — the Trust literal check, the mount
		// check — silently matches nothing. It is never a legitimate way to
		// import this package.
		if s.dot {
			t.Errorf("%s DOT-IMPORTS %s. Import it with its package name: a dot-import removes the qualifier "+
				"that TestRelayIngressCannotBeBuiltWithoutCrossBusTrust and TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile "+
				"match on, so it would turn both of them into no-ops without failing anything.", s.file, modulePath)
		}
		if wiringSites[s.dir] {
			continue
		}
		var allowed []string
		for d := range wiringSites {
			allowed = append(allowed, d)
		}
		t.Errorf("%s imports %s, but %s is not a wiring site (permitted: %v).\n"+
			"The relay handlers authenticate NO PEER by themselves — the mount is what has to carry a peer "+
			"principal (DECISIONS.md 2026-08-08, FEDERATION/RELAY-6 ruling (c), 77d2b73). Keeping the import "+
			"surface to the two composition sites is what keeps that reviewable. If you genuinely need a third "+
			"site, add it to wiringSites in this file as part of that task, so the widening is a reviewed line "+
			"rather than a directory appearing.",
			s.file, modulePath, s.dir, allowed)
	}
}

// TestRelayImportGuardCanActuallyFail proves the guard above is capable of
// failing, over a synthetic tree, because it currently has NOTHING to find: no
// wiring site imports this package yet (RELAY-20 is the task that will).
//
// A guard whose failure path has never executed is a guard nobody has evidence
// about. This is the same reason client/guard_test.go feeds its vocabulary a
// list of names it must match before trusting it to pass — except that here the
// vacuity is total, so the self-check is the only evidence there is until the
// first real import lands.
//
// It asserts all three behaviours in one tree: an allowed import is accepted, a
// disallowed one is reported, and a file that merely MENTIONS the module path in
// a comment and a string constant is not reported at all — the last being
// exactly what a grep-based guard would get wrong on this repository today.
func TestRelayImportGuardCanActuallyFail(t *testing.T) {
	root := t.TempDir()
	write := func(dir, name, src string) {
		t.Helper()
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	allowedDir := filepath.Join("internal", "httpapi")
	deniedDir := filepath.Join("internal", "hub")
	mentionDir := filepath.Join("internal", "store")

	write(allowedDir, "wire.go", "package httpapi\n\nimport relay \""+modulePath+"\"\n\nvar _ = relay.MaxRelayBytes\n")
	write(deniedDir, "router.go", "package hub\n\nimport \""+modulePath+"/subpkg\"\n\nvar _ = subpkg.Thing\n")
	write(mentionDir, "message.go", "package store\n\n// See "+modulePath+" for the bus-path rules.\nconst note = \""+modulePath+"\"\n")

	all := scanRepoGoFiles(t, root)
	if len(all) != 3 {
		t.Fatalf("parsed %d files, want 3; the synthetic tree was not scanned as written", len(all))
	}
	var sites []scannedFile
	for _, s := range all {
		if s.imports {
			sites = append(sites, s)
		}
	}

	found := map[string]bool{}
	for _, s := range sites {
		found[s.dir] = true
	}
	if !found[allowedDir] {
		t.Errorf("the guard did not see the import in %s; it would not notice a real wiring site either", allowedDir)
	}
	if !found[deniedDir] {
		t.Errorf("the guard did not see the SUBPACKAGE import in %s; a re-exporting subpackage would bridge this package onto a mux unnoticed", deniedDir)
	}
	if found[mentionDir] {
		t.Errorf("the guard flagged %s, which only MENTIONS %s in a comment and a string constant. Ten real files in this repository do exactly that; a guard that fails correct work is a guard that gets deleted.", mentionDir, modulePath)
	}

	// And the verdict the real test computes: exactly one of these is a
	// violation. If this ever reads zero, the allowlist has stopped rejecting
	// anything and the guard above is decorative.
	var violations int
	for _, s := range sites {
		if !wiringSites[s.dir] {
			violations++
		}
	}
	if violations != 1 {
		t.Fatalf("violations = %d, want exactly 1 (%s); the allowlist is not rejecting anything", violations, deniedDir)
	}
}

// TestRelayIngressCannotBeBuiltWithoutCrossBusTrust is the second half of what
// replaced the blanket import ban, and it is the half that carries the security
// property. The import guard says WHO may name this package; this one says that
// naming it does not get you an ingress that verifies nothing.
//
// # Why a nil trust is the failure that matters
//
// A RelayHandler with a nil CrossBusTrust would ingest relayed messages whose
// signatures nothing checks against a peering-time pin — the exact hole SIGN-7's
// "nil trust is a refusal, not a skip" exists to close, reached by omitting one
// struct field at a mount site. Outside this package the ONLY way to obtain a
// *RelayHandler is NewRelayHandler, because every field of the struct is
// unexported, so the constructor check below is not a convenience: it is the
// complete gate for every caller a wiring site can write.
//
// # Two halves again, for the same reason as the pin guard in client/
//
// The behavioural check proves the constructor refuses. The AST check then reads
// every relay.RelayConfig OUTSIDE this package and requires Trust to be set,
// non-nil, in the composite literal — so a mount that omits it is caught at
// review time with a message naming the remedy, rather than at runtime by a bus
// that fails to start. The AST half sees SHAPE and never SEMANTICS: a literal
// setting Trust to a variable that happens to be nil satisfies it and is caught
// by the constructor. That split is deliberate, and neither half is sufficient.
//
// It does NOT require that any such literal exists. Nothing is wired yet, and a
// guard that demands the wiring it is guarding would be red on a correct tree
// today. The behavioural check is what keeps this test non-vacuous in the
// meantime.
//
// # What it does NOT cover, said plainly rather than discovered later
//
// This guard is about the relay INGRESS only, because RelayConfig is the only
// one of the three peer surfaces with a trust chain to omit. The handshake
// (relay.Config/NewHandler) and the roster sync (relay.RosterConfig/
// NewRosterHandler) have no Trust field at all, so what protects those is not
// this test but TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile, which
// refuses to let any of
// the three be registered on a mux. The security gate reached them this way on
// the first draft of this file, when only RelayConfig was guarded.
func TestRelayIngressCannotBeBuiltWithoutCrossBusTrust(t *testing.T) {
	const guardBus = "guard-bus"
	accept := func(context.Context, RelayedMessage) (RelayAcceptance, error) {
		return RelayAcceptance{}, nil
	}

	if _, err := NewRelayHandler(RelayConfig{BusID: guardBus, AcceptRelay: accept}); err == nil {
		t.Error("NewRelayHandler built a relay ingress with a nil CrossBusTrust. Every relayed envelope is verified " +
			"through it before AcceptRelay sees the message (SIGN-7); without one the handler either refuses every " +
			"well-formed message a correct peer sent, or — if the refusal is ever softened — ingests unverified " +
			"messages attributed to another bus's agents. It must fail at CONSTRUCTION.")
	}
	// The positive control. Without it the check above would still pass on a
	// constructor that rejected EVERY config, which is a different bug wearing
	// the same green.
	if _, err := NewRelayHandler(RelayConfig{BusID: guardBus, AcceptRelay: accept, Trust: fakeCrossBusTrustForTest}); err != nil {
		t.Errorf("NewRelayHandler refused an otherwise-valid config carrying a CrossBusTrust: %v", err)
	}

	root := repoRoot(t)
	for _, s := range scanRepoGoFiles(t, root) {
		if !s.imports {
			continue
		}
		fset, file, local := s.fset, s.ast, s.display

		isCfgType := func(e ast.Expr) bool { return isRelayTypeExpr(e, s.locals, "RelayConfig") }
		isCfgLit := func(e ast.Expr) bool {
			lit, ok := e.(*ast.CompositeLit)
			return ok && isCfgType(lit.Type)
		}
		// collectBinds records the identifiers in one scope that hold a
		// relay.RelayConfig, and reports a declaration that never gets a literal.
		//
		// The set is built PER FUNCTION, not per file. A file-wide set made a
		// parameter named cfg in one function silence — or, worse, incriminate —
		// an unrelated cfg.Trust in another; the reviewer gate reproduced both
		// directions, and a guard that fails correct work is a guard the next
		// agent deletes.
		collectBinds := func(scope ast.Node, into map[string]bool) {
			ast.Inspect(scope, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.ValueSpec:
					declared := node.Type != nil && isCfgType(node.Type)
					for i, name := range node.Names {
						if declared || (i < len(node.Values) && isCfgLit(node.Values[i])) {
							into[name.Name] = true
						}
					}
					// `var cfg relay.RelayConfig` with no literal leaves every
					// field at its zero value, so Trust is nil and there is no
					// literal for the check below to read. A POINTER declaration
					// is exempt: `var cfg *relay.RelayConfig` is normally followed
					// by `cfg = &relay.RelayConfig{…}`, which IS a literal.
					if node.Type != nil && isRelayTypeExprExact(node.Type, s.locals, "RelayConfig") && len(node.Values) == 0 {
						t.Errorf("%s: a %s.RelayConfig is declared without a composite literal. Build it in one "+
							"literal so Trust is visible beside the other fields — a zero-valued config has a nil "+
							"Trust, and field-by-field assembly is exactly the shape this guard cannot read.",
							fset.Position(node.Pos()), local)
					}
				case *ast.AssignStmt:
					for i, lhs := range node.Lhs {
						id, ok := lhs.(*ast.Ident)
						if !ok || i >= len(node.Rhs) {
							continue
						}
						rhs := node.Rhs[i]
						if unary, ok := rhs.(*ast.UnaryExpr); ok && unary.Op == token.AND {
							rhs = unary.X
						}
						if isCfgLit(rhs) {
							into[id.Name] = true
						}
					}
				case *ast.Field:
					if isCfgType(node.Type) {
						for _, name := range node.Names {
							into[name.Name] = true
						}
					}
				}
				return true
			})
		}

		// Package-level declarations first: they are in scope for every function.
		pkgBound := map[string]bool{}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			collectBinds(gen, pkgBound)
		}
		// `type cfg = relay.RelayConfig` lets a SIBLING file that never imports
		// relay write cfg{...}, and that file has no relay import for the
		// composite-literal check below to key on.
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if ok && isCfgType(spec.Type) {
				t.Errorf("%s: %s.RelayConfig is aliased to %q. Use the qualified type at the construction site: "+
					"an alias can be used from a file that does not import this package, and the qualifier is what "+
					"this guard matches on.", fset.Position(spec.Pos()), local, spec.Name.Name)
			}
			return true
		})

		// Then each function with its own bindings layered on top.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			bound := make(map[string]bool, len(pkgBound))
			for k := range pkgBound {
				bound[k] = true
			}
			collectBinds(fn, bound)

			ast.Inspect(fn, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range assign.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Trust" {
						continue
					}
					base, ok := sel.X.(*ast.Ident)
					if !ok || !bound[base.Name] {
						// Some other type's Trust field. Not this guard's business.
						continue
					}
					t.Errorf("%s: %s.Trust is set by ASSIGNMENT on a %s.RelayConfig. Set it inside the literal, "+
						"where this guard can see it beside the other fields — an assignment can be conditional, "+
						"can be far from the literal, and defeats the literal check.",
						fset.Position(sel.Pos()), base.Name, local)
				}
				return true
			})
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if !isCfgType(node.Type) {
					return true
				}
				var (
					trust  ast.Expr
					keyed  bool
					fields int
				)
				for _, elt := range node.Elts {
					fields++
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					keyed = true
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Trust" {
						trust = kv.Value
					}
				}
				if fields > 0 && !keyed {
					t.Errorf("%s: this %s.RelayConfig literal is UNKEYED, so this guard cannot tell which field is "+
						"Trust. Write it with field names.", fset.Position(node.Pos()), local)
					return true
				}
				if trust == nil {
					t.Errorf("%s: this %s.RelayConfig literal does not set Trust. The relay ingress may only be "+
						"constructed with a non-nil CrossBusTrust; without one, signature verification has no "+
						"peering-time pin to work from. (NewRelayHandler also refuses it, but a bus that will not "+
						"start is a worse way to learn this than a red test.)", fset.Position(node.Pos()), local)
					return true
				}
				if id, ok := trust.(*ast.Ident); ok && id.Name == "nil" {
					t.Errorf("%s: this %s.RelayConfig literal sets Trust to nil explicitly. A nil trust is a refusal, "+
						"never a skip (SIGN-7) — there is no verification-disabled mode to select here.",
						fset.Position(node.Pos()), local)
				}
			}
			return true
		})
	}
}

// peerRoutePathIdents are the three exported path constants that name a peer
// surface. Registering any of them puts an unauthenticated handler on the wire.
var peerRoutePathIdents = map[string]bool{
	"PeerEnrollPath": true,
	"PeerRelayPath":  true,
	"PeerRosterPath": true,
}

// peerRoutePrefix is the value half of the same rule. All three constants live
// under it (peer.go, message.go, rosterhttp.go), so one prefix covers a path
// written out by hand — including one this package has not defined yet.
const peerRoutePrefix = "/v1/peer/"

// peerMountFile is THE ONE FILE outside this package permitted to name a peer
// route (RELAY-20). It is the mount, and it is a single file rather than a
// directory on purpose — see peerRouteMountSiteExempt.
const peerMountFile = "internal/httpapi/peermount.go"

// peerMountDir is the directory the mount lives in. Its TEST files are exempt
// too; see peerRouteMountSiteExempt.
const peerMountDir = "internal/httpapi"

// peerMountFunc is the ONE function a peer route may legitimately be passed to
// inside the mount file: it wraps the handler in RequirePeerPrincipal and
// records the path as a peer route in the same breath. Every other callee is an
// escape — see peerMountFileEscapes.
const peerMountFunc = "mountPeerRoute"

// peerRouteMountSiteExempt reports whether f is the reviewed mount file, or one
// of that package's own test files.
//
// # WHY A FILE AND NOT A PACKAGE, for PRODUCTION code
//
// internal/httpapi is a large package with a dozen non-test files, and it is the
// package that would most plausibly grow a SECOND registration — a route table,
// a convenience wrapper, a "peer" case in a switch. Exempting the DIRECTORY for
// production code would make every one of those invisible to this guard.
// Exempting one FILE keeps the property that matters after RELAY-20: not "nobody
// serves these" (somebody does now), but "exactly one reviewed place decides
// which handler is served at which peer path, and behind what".
//
// # WHY THE MOUNT PACKAGE'S TEST FILES ARE EXEMPT
//
// A _test.go file cannot be compiled into the server binary, so it can serve
// nothing — which is the same reason this guard already exempts internal/relay's
// own test files (see relayOwnMountRegistrations). And the retired guard's
// comment promised exactly this: "when RELAY-20 mounts them for real, it owns
// this guard and can carry such a test with the mount". The BEHAVIOURAL tests
// that replaced the syntactic half of the old rule have to name the paths they
// probe; sending them to internal/relay instead would put the mount's tests in a
// package that cannot see the mount.
//
// It is scoped to the mount's OWN directory, not to every test file in the
// repository. A peer-route fixture anywhere else is still refused, with the
// remedy in the message.
//
// Paths are matched EXACTLY, never by suffix. A suffix match would exempt
// anything ending in peermount.go anywhere in the tree, which is a file anyone
// can create.
func peerRouteMountSiteExempt(f scannedFile) bool {
	path := filepath.ToSlash(f.file)
	if path == peerMountFile {
		return true
	}
	return filepath.ToSlash(f.dir) == peerMountDir && strings.HasSuffix(path, "_test.go")
}

// peerMountFileEscapes finds a peer route ESCAPING the exempted mount file.
//
// # THE EXEMPTION IS NOT A LICENCE TO HAND THE PATH OUT (security gate, RELAY-20)
//
// The gate DEMONSTRATED this on the first draft: a value exported from
// peermount.go and ranged over by a SECOND file in internal/httpapi registers
// all three peer routes UNGATED with every guard green — the naming rule sees
// only the exempt file, and the sibling file names no path of its own. That is
// the exact shape relayOwnMountRegistrations already closes for internal/relay,
// and the exemption re-opened it one directory over. It is the structural twin
// of the subpackage bridge, and it is the shape an honest agent would most
// plausibly write, because "put the route table next to the mount" reads like
// good hygiene.
//
// # IT IS STRICTER THAN THE internal/relay RULE, AND IT HAS TO BE
//
// relayOwnMountRegistrations only flags a return from an EXPORTED function,
// because an unexported one cannot be reached by a wiring site in another
// package. HERE THE DANGER IS IN THE SAME PACKAGE: every sibling file of
// peermount.go shares its scope, so an UNEXPORTED var, const or func carries the
// path just as far. Export status is therefore not consulted at all.
//
// The one legitimate shape is what the mount actually does: the path as a CALL
// ARGUMENT to mountPeerRoute, which wraps and records it in one step. That is
// outside every rule below, which is why this costs nothing today.
//
// Residual, adversarial only: a path assembled by concatenation, and an escape
// laundered through a call this guard does not model — the same two
// relayOwnMountRegistrations carries.
func peerMountFileEscapes(t *testing.T, root string) []peerRouteMention {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(peerMountFile))
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Reported by the caller's sawMountSite check, which says what to do.
			return nil
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, path, src, 0)
	if perr != nil {
		t.Fatalf("parsing %s: %v", path, perr)
	}

	// In THIS file the paths are reached through the relay qualifier, so the
	// selector form is the one that matters; the bare ident and the raw string
	// are checked too, so a local copy is caught as well.
	namesPeerRoute := func(e ast.Expr) string {
		switch a := e.(type) {
		case *ast.Ident:
			if peerRoutePathIdents[a.Name] {
				return a.Name
			}
		case *ast.SelectorExpr:
			if peerRoutePathIdents[a.Sel.Name] {
				return a.Sel.Name
			}
		case *ast.BasicLit:
			if a.Kind == token.STRING {
				if v, uerr := strconv.Unquote(a.Value); uerr == nil && strings.HasPrefix(v, peerRoutePrefix) {
					return v
				}
			}
		}
		return ""
	}

	var out []peerRouteMention
	record := func(n ast.Node, named, how string) {
		out = append(out, peerRouteMention{
			pos:   fset.Position(n.Pos()).String(),
			named: named + " (" + how + ")",
		})
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			// THE ONE PERMITTED SHAPE IS A CALL ARGUMENT TO mountPeerRoute, and
			// this case is what makes that sentence true rather than merely
			// asserted. The security gate demonstrated the gap: with no CallExpr
			// case at all, ANY callee accepted a peer route silently, so swapping
			// the three mountPeerRoute calls for a sibling file's
			// mountPeerRouteFast — which records without gating — left every
			// guard green.
			callee := ""
			switch fn := node.Fun.(type) {
			case *ast.SelectorExpr:
				callee = fn.Sel.Name
			case *ast.Ident:
				callee = fn.Name
			}
			if callee == peerMountFunc {
				// The legitimate mount. Its arguments are not an escape; what
				// makes it safe is that it wraps and records in one function,
				// which TestPeerRoutesSetHasExactlyOneWriter pins separately.
				return true
			}
			for _, arg := range node.Args {
				if named := namesPeerRoute(arg); named != "" {
					record(arg, named, "passed to "+callee+"(), which is not "+peerMountFunc)
				}
			}
		case *ast.CompositeLit:
			for _, elt := range node.Elts {
				exprs := []ast.Expr{elt}
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					exprs = []ast.Expr{kv.Key, kv.Value}
				}
				for _, e := range exprs {
					if named := namesPeerRoute(e); named != "" {
						record(e, named, "carried in a composite literal")
					}
				}
			}
		case *ast.ValueSpec:
			for _, v := range node.Values {
				if named := namesPeerRoute(v); named != "" {
					record(v, named, "declared as a var/const")
				}
			}
		case *ast.AssignStmt:
			for _, rhs := range node.Rhs {
				if named := namesPeerRoute(rhs); named != "" {
					record(rhs, named, "bound to a variable")
				}
			}
			for _, lhs := range node.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				if named := namesPeerRoute(idx.Index); named != "" {
					record(idx, named, "used as a map key")
				}
			}
		case *ast.ReturnStmt:
			// EXPORT STATUS IS NOT CONSULTED; see the doc comment.
			for _, res := range node.Results {
				if named := namesPeerRoute(res); named != "" {
					record(res, named, "returned from a function")
				}
			}
		}
		return true
	})
	return out
}

// TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile is the guard that
// survives from the
// retired blanket ban, moved to the place the property actually lives.
//
// Retiring the import ban was right — the composition root has to be able to
// name this package — but the security gate showed on the first draft that
// import-plus-construction guards left the two surfaces WITHOUT a trust
// parameter completely open: a wiring site could call NewHandler and
// NewRosterHandler and mux.Handle all three peer paths with the whole guard
// suite green. relay.Config and relay.RosterConfig have no Trust field to omit,
// so there was nothing for the construction guard to catch. The blanket ban had
// caught that tree instantly, which is precisely what a replacement must not
// give up.
//
// So the line is drawn at REGISTRATION rather than at import: OUTSIDE this
// package, no peer route path may be NAMED at all — neither the exported
// constant nor its string value. What makes that legal is DECISIONS.md
// (2026-08-08, FEDERATION/RELAY-6, 77d2b73) ruling (c) — peer-principal
// authentication is a forward precondition that ruling (b)'s SSH-tunnel deferral
// explicitly does NOT cover — plus the fact that relay ingest has no
// CrossBusTrust implementation yet (RELAY-17 owns it), so every relayed message
// would be ErrUnpeeredBus by construction.
//
// # Why it bans NAMING and not just Handle(), which is broader than it first looks
//
// A first draft only inspected Handle/HandleFunc calls in files that IMPORT this
// package, and the reviewer gate showed that misses the ordinary wiring shape
// rather than some contrived evasion. internal/httpapi registers every route
// through a helper of its own (server.go: "EVERY route MUST be registered
// through this, not through mux.HandleFunc"), so the registration is a call to
// THAT, with a path string, in a file that need not import relay at all. A route
// table, a slice of {path, handler} structs, or a `p := relay.PeerRelayPath`
// indirection all defeat an argument-shaped check for the same reason.
//
// Banning the NAME closes all of those with one rule, and it costs nothing
// today: at the time of writing no file outside this package contains either the
// constants or the "/v1/peer/" prefix in a string literal. Comments are
// untouched — this is an AST walk over string literals and selectors, so the
// several files that discuss these paths in prose (internal/idem/doc.go among
// them) are not affected.
//
// # Residuals and escape hatches, written down rather than left to be discovered
//
// Not caught. Both are adversarial rather than accidental, and note that only
// the FIRST still has the constructor check behind it — Config and RosterConfig
// have no Trust field, so for the handshake and roster surfaces these guards are
// the whole of the control:
//
//   - a path that is not a Go STRING LITERAL in a scanned file: assembled by
//     concatenation ("/v1/" + "peer/relay"), read from config, or embedded.
//   - the same unauthenticated handler served at a DIFFERENT path
//     (mux.Handle("/v1/federation/roster", rosterHandler)). Self-limiting,
//     because a peer bus dials the constants, but the retired ban did catch it.
//
// A THIRD shape used to belong on that list and no longer does: a mount helper
// written INSIDE internal/relay and called from an allowlisted wiring site. It
// is now caught by relayOwnMountRegistrations below, which checks for the path
// ESCAPING this package — a route table, an exported return, a variable
// binding, a var/const declaration, a map key — and not merely for a Handle()
// call. It is called out here because it is the shape an honest agent would
// most plausibly write: putting the wiring "next to the handlers" reads like
// good hygiene.
//
// That rule was widened THREE TIMES under review, each time because a gate
// produced a green tree serving all three peer surfaces through the shape the
// previous revision had just declared closed: Handle-args only, then
// assignments but not declarations, then declarations but not map keys. The
// pattern is worth remembering more than the list is — every "closed" claim
// here was one spelling away from false, which is why the self-test pins each
// shape rather than the prose asserting it.
//
// What it still does NOT model, both adversarial: an escape laundered through a
// call it cannot type-check (register(mux, PeerEnrollPath, h) is shape-identical
// to the legitimate peerURL(base, PeerRelayPath) dial sites), and a concatenated
// path. And one KNOWN false positive, with a remedy: a peer path inside an
// unrelated composite literal — url.URL{Path: PeerRelayPath} — reads as a route
// table to this check. Build dial URLs with peerURL, as every existing call site
// does.
//
// TWO LEGITIMATE THINGS THIS RULE REFUSES, and what to do instead — they are
// named here because both gates predicted them, and an agent whose correct work
// is failed by a guard deletes the guard:
//
//   - a test that stands up a FAKE REMOTE PEER BUS and serves /v1/peer/… as the
//     counterparty. Put it in internal/relay, where those fixtures already live
//     (relay_test.go, cycle_test.go) and where the paths are in scope anyway.
//   - a test ASSERTING these routes are absent (a 404 probe). Same remedy; and
//     when RELAY-20 mounts them for real, it owns this guard and can carry such
//     a test with the mount.
//
// # RETIRED AND REPLACED BY RELAY-20 (2026-08-14), NOT DELETED
//
// The guard above was TestRelayPeerRoutesAreNotMountedYet, and its rule was "no
// file outside internal/relay may name a peer route AT ALL". That rule expired
// the moment its own exit condition was met: RELAY-20 mounted the three routes
// behind an authenticated peer-bus principal (internal/httpapi/peermount.go),
// authorised against MTLS-CLIENTAUTH plus RELAY-45 rather than the two tasks
// DECISIONS.md ruling (c) names.
//
// THAT SUBSTITUTION IS NOT RECORDED IN DECISIONS.md. Ruling (c) still names
// INVITE-PEERGUARD and MTLS-RELAYGUARD, and its "given up" clause still reads
// "the handler stays unregistered until both gating tasks land"; RELAY-6
// (0f7275b9) owes the amendment. An earlier draft of THIS COMMENT said the
// ruling "had been amended", which was a false claim about a file this test does
// not own — the review gate caught it here after catching the identical sentence
// in internal/relay/doc.go, which is why it is spelled out rather than trimmed.
// The full statement of the debt lives in doc.go; do not restate the amendment
// as fact in either place until it exists.
//
// It is REPLACED rather than deleted, following RELAY-18's own precedent one
// level down, and the replacement is NARROWER rather than absent: the paths may
// now be named in EXACTLY ONE FILE outside this package, and nowhere else. What
// that still buys, after the mount exists:
//
//   - a SECOND mount cannot appear. Serving relay.Handler at /v1/federation/x,
//     or wiring a peer path through a route table in another file of
//     internal/httpapi, fails here — and neither is caught by anything else,
//     because Config and RosterConfig have no Trust field for the construction
//     guard to miss.
//   - cmd/agent-bus cannot mount them. The composition root (RELAY-24) supplies
//     the handlers to httpapi.Options.Peer and never names a path, so the choice
//     of "which handler at which path" stays where it can be reviewed once.
//   - the exemption is a FILE, so a new file in the same package is refused with
//     the remedy in the message.
//
// WHAT IT DELIBERATELY NO LONGER CHECKS: that the routes are unserved. That is
// now a BEHAVIOURAL property with a behavioural test —
// TestPeerRoutesRegisterOnlyWithRegistryAndTrust and its siblings in
// internal/httpapi/peermount_relay20_test.go assert that every registered peer
// route refuses a caller with no client certificate, with an agent bearer token,
// and with an expired certificate, and that nothing is registered at all when
// any link of the chain is missing. A syntactic guard cannot say any of that,
// which is exactly why the replacement gives that half up rather than pretending
// to keep it.
func TestRelayPeerRoutesAreMountedOnlyByTheGatedMountFile(t *testing.T) {
	root := repoRoot(t)
	files := scanRepoGoFiles(t, root)
	if len(files) == 0 {
		t.Fatalf("parsed 0 .go files under %s; this guard inspected nothing, which is not a pass", root)
	}
	// THIS PACKAGE'S OWN non-test files are checked too, for REGISTRATION rather
	// than naming — it defines the constants, so naming is all it does.
	//
	// The reviewer gate found the shape this closes and it is the structural
	// twin of the subpackage bridge: a MountAll(mux, …) helper written HERE,
	// calling mux.Handle(PeerEnrollPath, h) with unqualified constants, invoked
	// from allowlisted internal/httpapi. Every other guard passes — the import
	// is from a wiring site, the wiring site names no path, and Config and
	// RosterConfig have no Trust field to omit — while two unauthenticated peer
	// surfaces are live on a real mux. The retired ban caught it, so a
	// replacement that did not would be giving something up.
	//
	// TEST files are deliberately exempt. A helper reachable from a wiring site
	// has to be in a non-test file, and internal/relay's tests are the sanctioned
	// home for a fake remote peer bus — which is exactly what the failure
	// message above tells people to write here.
	for _, m := range relayOwnMountRegistrations(t, root) {
		t.Errorf("%s: internal/relay puts the peer route %s where a wiring site can serve it.\n"+
			"Anything in this package is reachable from every site allowed to import it, so mounting here — or "+
			"handing the path out in a route table, a return value or a variable — serves the unauthenticated "+
			"handler just as surely as a registration in internal/httpapi would, and it is invisible to the "+
			"naming rule because this is the package that legitimately names these paths. Dialling a PEER's path "+
			"is fine and is what the existing peerURL(base, Peer…Path) call sites do. If you are RELAY-20, "+
			"mounting behind a peer principal, you own changing THIS TEST. If you are writing a fake remote peer "+
			"bus for a test, put it in a _test.go file, which this check exempts.",
			m.pos, m.named)
	}
	// sawMountSite proves the exemption is not silently dead. If peermount.go is
	// renamed, moved or emptied, the loop below would pass over a tree with NO
	// mount at all and report success — a guard that stopped inspecting the one
	// file it exists to bound.
	var sawMountSite bool

	for _, f := range files {
		if peerRouteMountSiteExempt(f) {
			// Only the MOUNT ITSELF counts as evidence the exemption is live; a
			// test file naming the paths proves nothing about the mount.
			if filepath.ToSlash(f.file) == peerMountFile && len(peerRouteMentions(f)) > 0 {
				sawMountSite = true
			}
			continue
		}
		for _, m := range peerRouteMentions(f) {
			t.Errorf("%s: %s names the peer route %s outside internal/relay and outside %s.\n"+
				"These handlers authenticate NO PEER of their own (see internal/relay/doc.go); the PEER PRINCIPAL "+
				"is supplied by the mount, and there is exactly ONE mount. Serving one of them anywhere else puts "+
				"an anonymous POST in front of our roster, our routing table and our relay ingest — and a path can "+
				"be registered from a file that never imports this package, which is why NAMING it is what fails "+
				"here. If you need a peer route served, do it in %s, which wraps every one of them in "+
				"(*Server).RequirePeerPrincipal in the same function that records it (mountPeerRoute). If you are "+
				"writing a FAKE PEER BUS fixture or a route-absence probe, put it in internal/relay where those "+
				"fixtures already live.",
				m.pos, f.file, m.named, peerMountFile, peerMountFile)
		}
	}

	// THE EXEMPTION IS BOUNDED, not merely granted. A path that ESCAPES the
	// mount file into its own package is an ungated registration one file away,
	// and the security gate demonstrated it against the first draft of this
	// replacement.
	for _, m := range peerMountFileEscapes(t, root) {
		t.Errorf("%s: the exempted mount file %s lets the peer route %s ESCAPE it.\n"+
			"Being the one permitted mount does not permit handing the path out. Every sibling file in %s shares this "+
			"file's scope, so a route table, a returned value, a var/const or a variable binding registers these routes "+
			"from a file that names no path at all — with every guard here green. The ONLY permitted shape is the path "+
			"as a CALL ARGUMENT to mountPeerRoute, which wraps it in RequirePeerPrincipal and records it as a peer route "+
			"in one step.",
			m.pos, peerMountFile, m.named, peerMountDir)
	}

	if !sawMountSite {
		t.Errorf("the exempted mount file %s named no peer route, so this guard's one exemption inspected nothing.\n"+
			"Either the mount moved — in which case update peerMountFile in this file, as a reviewed line — or the "+
			"peer surface was unmounted, in which case the behavioural tests in "+
			"internal/httpapi/peermount_relay20_test.go should be red and this guard must not be the thing that "+
			"quietly tolerates it.", peerMountFile)
	}
}

// peerRouteMention is one place a file names a peer route.
type peerRouteMention struct {
	pos   string
	named string
}

// relayOwnMountRegistrations finds, in this package's own NON-TEST files, the
// two ways a peer route reaches a mux from in here.
//
// # It checks for ESCAPE, not only for a Handle() call
//
// A first version checked Handle/HandleFunc arguments and the security gate
// showed that is the same argument-shaped mistake this file condemns elsewhere:
// internal/httpapi registers through its OWN helper (server.go's route()), so an
// exported `Routes() map[string]http.Handler{PeerEnrollPath: h, …}` here, ranged
// over there, mounts both surfaces with nobody calling Handle in a scanned file.
//
// So two rules, and the second is the load-bearing one:
//
//  1. REGISTRATION — a Handle/HandleFunc call naming a peer route.
//  2. ESCAPE — a peer route inside a COMPOSITE LITERAL (a route table is one),
//     or returned from an EXPORTED function, or bound to a variable by a plain
//     assignment (which launders the constant past rule 1).
//
// The legitimate in-package uses are all outside both rules, which is why this
// costs nothing today: the three const declarations; error strings built with
// fmt.Errorf; and dial-side URL building, where the path is a CALL ARGUMENT
// (peerURL(base, PeerRelayPath) in client.go, registry.go, relayhttp.go,
// rosterhttp.go). Dialling a peer's path is the opposite of serving our own.
//
// Residual, adversarial only: a path assembled by concatenation, and an escape
// laundered through a call this guard does not model.
func relayOwnMountRegistrations(t *testing.T, root string) []peerRouteMention {
	t.Helper()
	dir := filepath.Join(root, "internal", "relay")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []peerRouteMention
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading %s: %v", path, rerr)
		}
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		// namesPeerRoute reports the route an expression IS, for the three
		// spellings available inside this package.
		namesPeerRoute := func(e ast.Expr) string {
			switch a := e.(type) {
			case *ast.Ident:
				if peerRoutePathIdents[a.Name] {
					return a.Name
				}
			case *ast.SelectorExpr:
				if peerRoutePathIdents[a.Sel.Name] {
					return a.Sel.Name
				}
			case *ast.BasicLit:
				if a.Kind == token.STRING {
					if v, uerr := strconv.Unquote(a.Value); uerr == nil && strings.HasPrefix(v, peerRoutePrefix) {
						return v
					}
				}
			}
			return ""
		}
		record := func(n ast.Node, named, how string) {
			out = append(out, peerRouteMention{
				pos:   fset.Position(n.Pos()).String(),
				named: named + " (" + how + ")",
			})
		}
		exportedFuncs := map[*ast.FuncDecl]bool{}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() {
				exportedFuncs[fn] = true
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				// Rule 1: registration.
				fn, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || (fn.Sel.Name != "Handle" && fn.Sel.Name != "HandleFunc") {
					return true
				}
				for _, arg := range node.Args {
					if named := namesPeerRoute(arg); named != "" {
						record(node, named, "registered on a mux")
					}
				}
			case *ast.CompositeLit:
				// Rule 2a: a route table, or any other structure carrying the
				// path across the package boundary.
				for _, elt := range node.Elts {
					exprs := []ast.Expr{elt}
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						exprs = []ast.Expr{kv.Key, kv.Value}
					}
					for _, e := range exprs {
						if named := namesPeerRoute(e); named != "" {
							record(e, named, "carried in a composite literal")
						}
					}
				}
			case *ast.ValueSpec:
				// Rule 2b, DECLARATION half. `var p = PeerEnrollPath` and
				// `const MountPath = PeerEnrollPath` are not AssignStmts, so
				// without this case they were visited by no rule at all — one
				// token away from the := shape below, and both gates
				// independently demonstrated a fully green tree serving all
				// three peer routes through it. An exported one is worse still:
				// the wiring site then names relay.MountPath, which is not one
				// of peerRoutePathIdents, so the naming rule cannot see it
				// either.
				//
				// The three CANONICAL declarations are the one place these
				// values belong, and they are exempted BY NAME. An earlier
				// exemption by position was inert — a const value is never a
				// composite-literal element — and believing it was live is
				// what left this hole.
				canonical := false
				for _, name := range node.Names {
					if peerRoutePathIdents[name.Name] {
						canonical = true
					}
				}
				if canonical {
					return true
				}
				for _, v := range node.Values {
					if named := namesPeerRoute(v); named != "" {
						record(v, named, "declared as a var/const")
					}
				}
			case *ast.AssignStmt:
				// Rule 2b: laundering the constant into a variable, which
				// defeats rule 1 by one line.
				for _, rhs := range node.Rhs {
					if named := namesPeerRoute(rhs); named != "" {
						record(rhs, named, "bound to a variable")
					}
				}
				// A route map filled by index assignment — m[PeerEnrollPath] = h
				// — is the composite-literal table written one key at a time.
				for _, lhs := range node.Lhs {
					idx, ok := lhs.(*ast.IndexExpr)
					if !ok {
						continue
					}
					if named := namesPeerRoute(idx.Index); named != "" {
						record(idx, named, "used as a map key")
					}
				}
			case *ast.FuncDecl:
				// Rule 2c: returned from an EXPORTED function, i.e. handed to
				// whoever may import this package.
				if !exportedFuncs[node] || node.Body == nil {
					return true
				}
				ast.Inspect(node.Body, func(inner ast.Node) bool {
					ret, ok := inner.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					for _, res := range ret.Results {
						if named := namesPeerRoute(res); named != "" {
							record(res, named, "returned from exported "+node.Name.Name)
						}
					}
					return true
				})
			}
			return true
		})
	}
	return out
}

// peerRouteMentions finds every reference to a peer route path in one file:
// the exported constants by selector, and their values as string literals.
func peerRouteMentions(f scannedFile) []peerRouteMention {
	var out []peerRouteMention
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if !peerRoutePathIdents[node.Sel.Name] {
				return true
			}
			// A dot-import would erase the qualifier, which is why
			// TestRelayImportedOnlyByWiringSites refuses one outright.
			if id, ok := node.X.(*ast.Ident); ok && f.qualifies(id.Name) {
				out = append(out, peerRouteMention{
					pos:   f.fset.Position(node.Pos()).String(),
					named: id.Name + "." + node.Sel.Name,
				})
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(node.Value)
			if err != nil || !strings.HasPrefix(v, peerRoutePrefix) {
				return true
			}
			out = append(out, peerRouteMention{
				pos:   f.fset.Position(node.Pos()).String(),
				named: v,
			})
		}
		return true
	})
	return out
}

// TestPeerRouteMountGuardCanActuallyFail is the mount guard's self-check, for
// the same reason TestRelayImportGuardCanActuallyFail exists: on the real tree
// the guard finds nothing, so without this its failure path has never run.
//
// The three shapes are the three the reviewer and security gates demonstrated
// against the first draft — a direct mux.Handle, a registration through a
// package-local helper in a file that does NOT import relay, and a route table
// — plus the negative case: a file that names the path only in a COMMENT is not
// flagged, which is what separates this from a grep.
func TestPeerRouteMountGuardCanActuallyFail(t *testing.T) {
	root := t.TempDir()
	write := func(dir, name, src string) {
		t.Helper()
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	const dir = "internal/httpapi"
	write(dir, "direct.go", "package httpapi\n\nimport relay \""+modulePath+"\"\n\n"+
		"func mountA(mux Mux) { mux.Handle(relay.PeerEnrollPath, nil) }\n")
	write(dir, "helper.go", "package httpapi\n\n"+
		"// Registered through this package's own route helper, in a file that does\n"+
		"// NOT import relay — the shape the first draft of the guard missed.\n"+
		"func mountB(s *Server) { s.route(\"/v1/peer/roster\", nil) }\n")
	write(dir, "table.go", "package httpapi\n\n"+
		"var routes = []struct{ path string }{{path: \"/v1/peer/relay\"}}\n")
	write(dir, "comment.go", "package httpapi\n\n"+
		"// mountD would serve /v1/peer/enroll behind the peer principal. Prose only.\n"+
		"func mountD() {}\n")
	// THE EXEMPTED MOUNT ITSELF (RELAY-20). It names all three paths, exactly as
	// the real one does, and must be exempt — otherwise the guard fails the one
	// piece of correct work it now exists to permit.
	//
	// It uses the ONE legitimate shape — the path as a call argument to
	// mountPeerRoute, which wraps and records in a single function — because
	// peerMountFileEscapes now flags every other callee. Writing it with a bare
	// mux.Handle would be the very escape the guard exists to catch.
	write(dir, "peermount.go", "package httpapi\n\nimport relay \""+modulePath+"\"\n\n"+
		"func mountAll(s *Server, mux Mux) {\n"+
		"\ts.mountPeerRoute(mux, relay.PeerEnrollPath, nil)\n"+
		"\ts.mountPeerRoute(mux, relay.PeerRelayPath, nil)\n"+
		"\ts.mountPeerRoute(mux, relay.PeerRosterPath, nil)\n}\n")
	// A DECOY at the same base name in a DIFFERENT directory. The exemption is an
	// exact path, so this must still be flagged — a suffix match would make
	// "create a file called peermount.go anywhere" the whole bypass.
	write("cmd/agent-bus", "peermount.go", "package main\n\n"+
		"func mountE(mux Mux) { mux.Handle(\"/v1/peer/enroll\", nil) }\n")
	// The mount package's OWN test file: exempt, because a _test.go file cannot
	// be compiled into the server binary and the behavioural tests that replaced
	// the syntactic guard have to name the paths they probe.
	write(dir, "peermount_relay20_test.go", "package httpapi\n\n"+
		"func probe() string { return \"/v1/peer/relay\" }\n")
	// A test file ELSEWHERE is NOT exempt: the relaxation is scoped to the mount's
	// own package, not to every _test.go in the tree.
	write("internal/store", "peerprobe_test.go", "package store\n\n"+
		"func probe() string { return \"/v1/peer/roster\" }\n")

	files := scanRepoGoFiles(t, root)
	if len(files) != 8 {
		t.Fatalf("parsed %d files, want 8; the synthetic tree was not scanned as written", len(files))
	}
	flagged := map[string]int{}
	exempt := map[string]bool{}
	for _, f := range files {
		flagged[filepath.ToSlash(f.file)] = len(peerRouteMentions(f))
		exempt[filepath.ToSlash(f.file)] = peerRouteMountSiteExempt(f)
	}
	for _, name := range []string{
		dir + "/direct.go", dir + "/helper.go", dir + "/table.go",
		"cmd/agent-bus/peermount.go", "internal/store/peerprobe_test.go",
	} {
		if flagged[name] == 0 {
			t.Errorf("the mount guard did not flag %s; that registration shape would reach a mux unnoticed", name)
		}
		if exempt[name] {
			t.Errorf("%s was treated as the exempted mount site; the exemption must be the single exact path %s "+
				"and nothing that merely resembles it", name, peerMountFile)
		}
	}
	if flagged[dir+"/comment.go"] != 0 {
		t.Errorf("the mount guard flagged comment.go, which names the path only in PROSE. A guard that fails "+
			"correct work is a guard that gets deleted — and doc comments discussing these routes are correct work. "+
			"(got %d findings)", flagged[dir+"/comment.go"])
	}
	if !exempt[peerMountFile] {
		t.Errorf("%s was NOT treated as the exempted mount site, so the guard would fail the one registration it "+
			"is now meant to permit — which is how a guard gets deleted instead of narrowed", peerMountFile)
	}
	if flagged[peerMountFile] == 0 {
		t.Errorf("the synthetic %s named no peer route, so this self-check proved nothing about the exemption", peerMountFile)
	}
	if !exempt[dir+"/peermount_relay20_test.go"] {
		t.Errorf("%s/peermount_relay20_test.go was not exempt; the mount's own behavioural tests must be able to name "+
			"the paths they probe, or the guard fails the work that replaced its syntactic half", dir)
	}

	// AND THE EXEMPTION IS BOUNDED. The synthetic peermount.go above uses only
	// the legitimate shape (a path as a call argument), so it must produce NO
	// escape findings; each shape that hands the path to a sibling file must
	// produce one. This is the evasion the security gate reproduced against the
	// first draft, so its self-check is not optional.
	t.Run("the exempted mount file may not let a path escape", func(t *testing.T) {
		if got := peerMountFileEscapes(t, root); len(got) != 0 {
			t.Errorf("the legitimate mount shape (path as a CALL ARGUMENT) was flagged as an escape: %v.\n"+
				"A guard that fails the one registration it exists to permit is a guard someone deletes", got)
		}

		for _, esc := range []struct {
			name string
			src  string
		}{
			{
				name: "an exported route table ranged over by a sibling file",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"var PeerPaths = []string{relay.PeerEnrollPath, relay.PeerRelayPath, relay.PeerRosterPath}\n",
			},
			{
				// UNEXPORTED is just as bad here: a sibling file in the same
				// package reads it. This is the case relayOwnMountRegistrations
				// deliberately does not flag for internal/relay, and must.
				name: "an UNEXPORTED var, which every sibling file can still read",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"var peerPaths = []string{relay.PeerEnrollPath}\n",
			},
			{
				name: "returned from an unexported function",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"func enrollPath() string { return relay.PeerEnrollPath }\n",
			},
			{
				name: "laundered into a local variable",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"func mount(mux Mux) { p := relay.PeerRelayPath; mux.Handle(p, nil) }\n",
			},
			{
				name: "used as a map key one entry at a time",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"func fill(m map[string]int) { m[relay.PeerRosterPath] = 1 }\n",
			},
			{
				name: "a const copy of the raw string",
				src:  "package httpapi\n\nconst rosterPath = \"/v1/peer/roster\"\n",
			},
			{
				// The gate's F1: with no CallExpr case, ANY callee accepted a
				// peer route silently, so a sibling file's mountPeerRouteFast —
				// which records the bearer-skip without gating — left every
				// guard green.
				name: "passed to a helper that is not mountPeerRoute",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"func mount(s *Server, mux Mux) { s.mountPeerRouteFast(mux, relay.PeerEnrollPath, nil) }\n",
			},
			{
				name: "passed to a package-level helper by bare name",
				src: "package httpapi\n\nimport relay \"" + modulePath + "\"\n\n" +
					"func mount(mux Mux) { register(mux, relay.PeerRelayPath, nil) }\n",
			},
		} {
			esc := esc
			t.Run(esc.name, func(t *testing.T) {
				escRoot := t.TempDir()
				full := filepath.Join(escRoot, filepath.FromSlash(peerMountDir))
				if err := os.MkdirAll(full, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(escRoot, filepath.FromSlash(peerMountFile)), []byte(esc.src), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if got := peerMountFileEscapes(t, escRoot); len(got) == 0 {
					t.Errorf("the escape guard missed %q; that shape registers three UNGATED peer routes from a "+
						"sibling file that names no path at all, with every other guard green", esc.name)
				}
			})
		}
	})
}

// TestInPackageMountEscapeGuardCanActuallyFail is the self-check for
// relayOwnMountRegistrations, and it carries the NEGATIVE cases too — which
// matter more here than anywhere else in this file, because this guard reads a
// package that legitimately uses these constants several times a day.
//
// Positives are the four shapes the gates demonstrated; negatives are the four
// real spellings that exist in internal/relay today and must stay green.
func TestInPackageMountEscapeGuardCanActuallyFail(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "relay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// The escapes. d/e/f/g were each found by a gate AFTER an earlier revision
	// of this guard called the previous one closed, which is why the negative
	// list below matters as much as this one.
	write("a_handle.go", "package relay\n\nfunc mount(mux Mux) { mux.Handle(PeerEnrollPath, h) }\n")
	write("b_table.go", "package relay\n\nfunc Routes() map[string]int { return map[string]int{PeerRosterPath: 1} }\n")
	write("c_return.go", "package relay\n\nfunc RelayPath() string { return PeerRelayPath }\n")
	write("d_launder.go", "package relay\n\nfunc mount2(mux Mux) { p := PeerEnrollPath; mux.Handle(p, h) }\n")
	write("e_vardecl.go", "package relay\n\nfunc mount3(mux Mux) { var p = PeerEnrollPath; mux.Handle(p, h) }\n")
	write("f_constdecl.go", "package relay\n\nconst MountPath = PeerRosterPath\n")
	write("g_mapkey.go", "package relay\n\nfunc mount4(m map[string]int) { m[PeerRelayPath] = 1 }\n")
	// A RENAMED COPY of the literal is an escape too: the wiring site would name
	// the copy, which peerRoutePathIdents does not know.
	write("h_constcopy.go", "package relay\n\nconst PeerEnrollPathCopy = \"/v1/peer/enroll\"\n")

	// The legitimate spellings that exist in this package today.
	write("ok_error.go", "package relay\n\nfunc e() error { return fmt.Errorf(\"not allowed on %s\", PeerRelayPath) }\n")
	write("ok_dial.go", "package relay\n\nfunc dial(b string) (string, error) { return peerURL(b, PeerRosterPath) }\n")
	write("ok_unexported_return.go", "package relay\n\nfunc peerEnrollURL(b string) (string, error) { return peerURL(b, PeerEnrollPath) }\n")
	// The CANONICAL declarations, exempted by name — the one place these values
	// belong. Without this exemption the guard would fail on peer.go itself.
	write("ok_canonical.go", "package relay\n\nconst PeerEnrollPath = \"/v1/peer/enroll\"\n")

	// And a test file, which is exempt by design.
	write("fake_peer_test.go", "package relay\n\nfunc fakePeer(mux Mux) { mux.Handle(PeerEnrollPath, h) }\n")

	found := map[string]bool{}
	for _, m := range relayOwnMountRegistrations(t, root) {
		found[filepath.Base(strings.SplitN(m.pos, ":", 2)[0])] = true
	}
	for _, name := range []string{
		"a_handle.go", "b_table.go", "c_return.go", "d_launder.go",
		"e_vardecl.go", "f_constdecl.go", "g_mapkey.go", "h_constcopy.go",
	} {
		if !found[name] {
			t.Errorf("relayOwnMountRegistrations missed %s; that shape hands a peer route to a wiring site, "+
				"which serves the unauthenticated handler just as a mux.Handle here would", name)
		}
	}
	for _, name := range []string{"ok_error.go", "ok_dial.go", "ok_unexported_return.go", "ok_canonical.go", "fake_peer_test.go"} {
		if found[name] {
			t.Errorf("relayOwnMountRegistrations flagged %s, which is a spelling internal/relay uses legitimately "+
				"today (an error string, dial-side URL building, the canonical const declaration, or a test "+
				"fixture). A guard that fails correct work is a guard that gets deleted.", name)
		}
	}
}

// isRelayTypeExprExact reports whether e names <qualifier>.<name> with NO
// pointer indirection. `var cfg *relay.RelayConfig` is normally assigned a
// literal a line later, so the no-literal complaint must not fire on it.
func isRelayTypeExprExact(e ast.Expr, locals map[string]bool, name string) bool {
	if _, isPtr := e.(*ast.StarExpr); isPtr {
		return false
	}
	return isRelayTypeExpr(e, locals, name)
}

// isRelayTypeExpr reports whether e names <qualifier>.<name>, for ANY qualifier
// this file can reach relay through, past any number of pointer indirections.
func isRelayTypeExpr(e ast.Expr, locals map[string]bool, name string) bool {
	for {
		star, ok := e.(*ast.StarExpr)
		if !ok {
			break
		}
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && locals[id.Name]
}

// TestPackageDocCitesTheRulingThatGovernsMounting replaces
// TestPackageDocNamesTheGatingTasks, which pinned the text of the retired
// blanket ban.
//
// The doc comment is still the control — it is what the next agent reads before
// deciding whether wiring this up is safe — but it now has to say a harder
// thing than "do not". It must say what the narrowed rule IS, cite the ruling
// that governs mounting rather than an unlanded task key, and carry the
// mechanical triggers that put the deferred gate back in force, so nobody has to
// re-derive them from a topology assumption they cannot see from here.
//
// Deleting the paragraph would satisfy any absence check, which is why every
// assertion here is a PRESENCE assertion.
func TestPackageDocCitesTheRulingThatGovernsMounting(t *testing.T) {
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
		// The narrowed import rule, RENDERED FROM wiringSites so that widening
		// the allowlist without amending the package doc fails here rather than
		// leaving a stale sentence behind. And the construction rule beside it.
		importedOnlyByPhrase(),
		"non-nil CrossBusTrust",
		// The ruling that governs SERVING, cited by date, name and commit — the
		// authority, rather than a task key, is what settles a dispute.
		"RELAY-6",
		"77d2b73",
		// But the two task keys ruling (c) names ARE operative: it resolves only
		// once both land, and dropping them from the doc would leave the reader
		// with a ruling whose completion condition is invisible. The security
		// gate required these back after the first draft removed them.
		"INVITE-PEERGUARD",
		"f5d91dbe",
		"MTLS-RELAYGUARD",
		"8192c3c7",
		// The deferral is conditional, and the conditions are mechanical.
		"REVERSES MECHANICALLY",
		// Importing is not serving: the distinction the whole change rests on.
		"IMPORTING IS NOT SERVING",
	} {
		if !strings.Contains(doc, must) {
			t.Errorf("doc.go no longer contains %q; the package doc is what the next agent reads before mounting "+
				"these handlers, and RELAY-18 replaced a blanket ban with a narrower rule that only works if the "+
				"rule, its authority and its reversal triggers are all still written down here", must)
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
