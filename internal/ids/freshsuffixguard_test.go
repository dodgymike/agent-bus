package ids

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// freshSuffixGuardProbeImportPath is this package's import path as the SYNTHETIC
// probe sources below spell it. The module-wide scan does NOT use this constant:
// it derives the path from go.mod, because a constant pinned to v1 would keep
// the guard green over a tree whose module line had moved to /v2 — every import
// would then resolve to nothing and the scan would report zero offences on a
// tree containing a live call. See freshSuffixGuardModule.
const freshSuffixGuardProbeImportPath = "github.com/dodgymike/agent-bus/internal/ids"

// freshSuffixGuardName is the constructor no production file may reference.
const freshSuffixGuardName = "NewNameSuffixes"

// freshSuffixGuardSkipDirs are directories whose contents are not this module's
// production source.
//
// This map is a HOLE if it is widened: adding a directory here silences the
// guard for everything under it. TestNoProductionCallerOfNewNameSuffixes
// asserts the set is EXACTLY these entries, so widening it goes red rather than
// quietly excusing a package. (An earlier version of this file claimed that
// property in a comment without testing it, and adding "hub" to this map while
// planting a real banned call in internal/hub left the suite printing ok.)
var freshSuffixGuardSkipDirs = map[string]bool{
	".git":         true,
	"vendor":       true,
	"testdata":     true,
	"node_modules": true,
}

// freshSuffixScan is what one walk of a tree found.
type freshSuffixScan struct {
	// Offences are the positions at which the forbidden constructor is
	// referenced, in any position — not only where it is called.
	Offences []string
	// Parsed counts the non-test .go files actually parsed. A scan that parsed
	// nothing proves nothing.
	Parsed int
	// Importers are the files that resolved the ids import path at all. It is
	// the positive control: a scan that resolved NO importers is not evidence of
	// a clean tree, it is evidence the import path is wrong.
	Importers []string
}

// scanForFreshSuffixCounter walks every NON-TEST .go file under root and reports
// every reference to ids.NewNameSuffixes, where idsImportPath is the import path
// the ids package is reached by.
//
// # It matches REFERENCES, not calls
//
// This is deliberate and it is the correction of a real hole: the first version
// of this guard matched only *ast.CallExpr, so three shapes stayed green while
// reaching the identical fresh, all-zero counter —
//
//	f := ids.NewNameSuffixes; f()
//	buildWith(ids.NewNameSuffixes)                 // passed as a value
//	reflect.ValueOf(ids.NewNameSuffixes).Call(nil)
//
// — the last of which defeats any call-position rule by construction. So the
// match is on the resolved selector (and, where an unqualified reference can
// mean this constructor, on the bare identifier) WHEREVER it appears. There is
// no legitimate production reason to name this constructor at all, so "taking a
// reference to it is an offence" costs nothing and closes the family rather than
// three members of it.
//
// The "documentation is not a defect" property survives that widening, and it is
// mechanical rather than lucky: parser mode 0 retains no comments at all, so the
// several files in this module that name the forbidden constructor in prose
// produce no AST nodes to match. A grep guard could not make that distinction,
// which is why this parses.
//
// # Resolution rules
//
// Each is an evasion the cmd/ guard's reviewer named:
//
//   - a plain import contributes the name "ids";
//   - an aliased import (import x ".../ids") contributes "x";
//   - a dot-import makes an UNQUALIFIED NewNameSuffixes an offence;
//   - a blank import contributes nothing, because nothing can be reached
//     through it.
//
// Files belonging to package ids itself are scanned too, with an unqualified
// reference counted as an offence — internal/ids is a production package like
// any other, and a footgun is no less loaded because the package that defines it
// is the one that picks it up. The FuncDecl that DECLARES the constructor is
// excluded by position, so the declaration does not trip its own guard, and a
// selector into some other package that happens to have a member of the same
// name is not matched because a selector is resolved through its receiver.
func scanForFreshSuffixCounter(root, idsImportPath string) (freshSuffixScan, error) {
	var scan freshSuffixScan
	fset := token.NewFileSet()

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if path != root && freshSuffixGuardSkipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		scan.Parsed++

		idsNames := map[string]bool{}
		// An unqualified reference means this constructor inside package ids
		// itself (where it is in scope with no selector) or through a dot-import.
		bareIsOffence := file.Name.Name == "ids"
		imported := false
		for _, imp := range file.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil || p != idsImportPath {
				continue
			}
			imported = true
			switch {
			case imp.Name == nil:
				idsNames["ids"] = true
			case imp.Name.Name == ".":
				bareIsOffence = true
			case imp.Name.Name == "_":
				// Blank import: nothing can be reached through it.
			default:
				idsNames[imp.Name.Name] = true
			}
		}
		if imported {
			scan.Importers = append(scan.Importers, path)
		}

		// The constructor's own declaration is not a reference to it.
		declared := map[token.Pos]bool{}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Name != nil && fd.Name.Name == freshSuffixGuardName {
				declared[fd.Name.Pos()] = true
			}
		}

		report := func(pos token.Pos) {
			scan.Offences = append(scan.Offences, fset.Position(pos).String())
		}

		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if pkg, isIdent := node.X.(*ast.Ident); isIdent && idsNames[pkg.Name] && node.Sel.Name == freshSuffixGuardName {
					report(node.Pos())
				}
				// Descend into the RECEIVER only. A selector's member name is
				// not an unqualified reference, so letting the walk see node.Sel
				// as a bare *ast.Ident would flag other.NewNameSuffixes inside
				// package ids as if it were ours.
				ast.Inspect(node.X, visit)
				return false
			case *ast.Ident:
				if bareIsOffence && node.Name == freshSuffixGuardName && !declared[node.Pos()] {
					report(node.Pos())
				}
				return false
			}
			return true
		}
		ast.Inspect(file, visit)
		return nil
	})
	if walkErr != nil {
		return scan, walkErr
	}
	sort.Strings(scan.Offences)
	return scan, nil
}

// freshSuffixGuardModule returns this module's root directory and the import
// path of this package, both DERIVED from go.mod rather than assumed.
//
// Deriving the import path is the fix for a vacuity vector a gate measured: the
// earlier version checked go.mod with strings.Contains(…, "module
// github.com/dodgymike/agent-bus"), which stays true when the module line moves
// to ".../agent-bus/v2" while a hard-coded v1 import path stops matching
// anything. Every import then resolves to nothing and the scan reports a clean
// tree while a live call sits in it (measured: checked=1, offences=0).
func freshSuffixGuardModule(t *testing.T) (root, idsImportPath string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	gomodPath := filepath.Join(root, "go.mod")
	gomod, err := os.ReadFile(gomodPath)
	if err != nil {
		t.Fatalf("reading %s: %v; this guard must scan the whole module and could not find its root", gomodPath, err)
	}
	modulePath := ""
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		modulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		if unquoted, uerr := strconv.Unquote(modulePath); uerr == nil {
			modulePath = unquoted
		}
		break
	}
	if modulePath == "" {
		t.Fatalf("%s declares no module path; the guard cannot resolve how this package is imported", gomodPath)
	}
	return root, modulePath + "/internal/ids"
}

// TestNoProductionCallerOfNewNameSuffixes is the module-wide regression guard
// required by MSG-FU-SUFFIXFLOOR-FU-UNSEAL (d).
//
// NewNameSuffixes is the FRESH-BUS constructor: it starts every name at suffix
// 1. Production must construct through OpenNameSuffixes, which persists each
// name's floor ahead of the suffix it authorises and therefore resumes strictly
// above every suffix the data dir has ever issued (invariant 1: ids are never
// reused, INCLUDING ACROSS RESTARTS). A production reference to
// NewNameSuffixes — including one reached only as a fallback after a failed
// open or a failed seal — re-mints agent ids that are already on disk, and
// every other test in this module stays green while it does. Because the agent
// id is the routing AND authorization subject, reissuing one is an
// impersonation primitive, not merely a bookkeeping slip.
//
// cmd/agent-bus/suffixfloors_test.go:TestNoFreshSuffixCounterInCmd already
// asserts this for package main. This generalises it to the WHOLE module, which
// is what the follow-up asked for: the next startup path to reach for the
// obvious-looking constructor will not necessarily live in cmd/.
func TestNoProductionCallerOfNewNameSuffixes(t *testing.T) {
	t.Run("ModuleHasNoProductionCaller", func(t *testing.T) {
		root, idsImportPath := freshSuffixGuardModule(t)
		scan, err := scanForFreshSuffixCounter(root, idsImportPath)
		if err != nil {
			t.Fatalf("scanning %s: %v", root, err)
		}
		// Two positive controls. A guard that parsed nothing, or that resolved
		// the import path to something no file imports, would pass silently —
		// exactly the vacuous proof CLAUDE.md warns about.
		if scan.Parsed == 0 {
			t.Fatalf("no non-test .go files were parsed under %s; this guard proved nothing", root)
		}
		if len(scan.Importers) == 0 {
			t.Fatalf("no non-test file under %s imports %q; the guard resolved an import path nothing uses, so a clean result is not evidence of a clean tree", root, idsImportPath)
		}
		for _, at := range scan.Offences {
			t.Errorf("%s references ids.%s: that is a FRESH counter starting every name at suffix 1, so a restart re-mints agent ids that are already durable on disk (invariant 1). Construct through ids.OpenNameSuffixes instead — it handles a fresh data dir on its own, so no case needs this as a fallback", at, freshSuffixGuardName)
		}
	})

	// The RED case, made permanent. A guard nobody has watched fail is not
	// evidence that it can fail, and the failure modes that matter here are the
	// evasions (alias, dot-import, non-call reference) and the false positives
	// (prose, an unrelated same-named function) — none of which the happy path
	// above exercises, because the module currently contains no offending
	// reference at all.
	t.Run("GuardDetectsAReintroducedReference", func(t *testing.T) {
		const header = "package probe\n\n"
		imp := `import "` + freshSuffixGuardProbeImportPath + `"` + "\n\n"
		cases := []struct {
			name string
			src  string
			want int
		}{
			{
				name: "plain import, called",
				src:  header + imp + "func build() interface{} { return ids.NewNameSuffixes() }\n",
				want: 1,
			},
			{
				name: "aliased import",
				src: header + `import x "` + freshSuffixGuardProbeImportPath + `"

func build() interface{} { return x.NewNameSuffixes() }
`,
				want: 1,
			},
			{
				name: "dot import",
				src: header + `import . "` + freshSuffixGuardProbeImportPath + `"

func build() interface{} { return NewNameSuffixes() }
`,
				want: 1,
			},
			{
				name: "function value taken without calling",
				src:  header + imp + "func build() interface{} {\n\tf := ids.NewNameSuffixes\n\treturn f()\n}\n",
				want: 1,
			},
			{
				name: "passed as an argument",
				src:  header + imp + "func use(f func() interface{}) {}\n\nfunc build() { use(ids.NewNameSuffixes) }\n",
				want: 1,
			},
			{
				name: "stored in a package-level var",
				src:  header + imp + "var fallback = ids.NewNameSuffixes\n",
				want: 1,
			},
			{
				name: "reached through reflect",
				src: header + `import (
	"reflect"

	"` + freshSuffixGuardProbeImportPath + `"
)

func build() interface{} { return reflect.ValueOf(ids.NewNameSuffixes).Call(nil) }
`,
				want: 1,
			},
			{
				name: "fallback buried in an error path",
				src: header + imp + `func build(dir string) interface{} {
	a, err := ids.OpenNameSuffixes(dir)
	if err != nil {
		return ids.NewNameSuffixes()
	}
	return a
}
`,
				want: 1,
			},
			{
				name: "two references in one file are both reported",
				src:  header + imp + "func a() interface{} { return ids.NewNameSuffixes() }\nfunc b() interface{} { return ids.NewNameSuffixes() }\n",
				want: 2,
			},
			{
				name: "package ids itself, unqualified",
				src:  "package ids\n\nfunc build() interface{} { return NewNameSuffixes() }\n",
				want: 1,
			},
			{
				name: "package ids: the declaration is not a reference to itself",
				src:  "package ids\n\ntype NameSuffixes struct{}\n\nfunc NewNameSuffixes() *NameSuffixes { return &NameSuffixes{} }\n",
				want: 0,
			},
			{
				name: "package ids: a same-named member of ANOTHER package is not ours",
				src: `package ids

import "example.com/other"

func build() interface{} { return other.NewNameSuffixes() }
`,
				want: 0,
			},
			{
				name: "prose naming the constructor is not a reference",
				src: header + imp + `// There is NO fallback to ids.NewNameSuffixes() here or anywhere else.
// NewNameSuffixes must not come back, not even as a value.
func build(dir string) interface{} {
	a, _ := ids.OpenNameSuffixes(dir) // not ids.NewNameSuffixes
	return a
}
`,
				want: 0,
			},
			{
				name: "a same-named constructor in an unrelated package is not ours",
				src: header + `type other struct{}

func NewNameSuffixes() *other { return &other{} }

func build() interface{} { return NewNameSuffixes() }
`,
				want: 0,
			},
			{
				name: "blank import cannot be reached through",
				src: header + `import _ "` + freshSuffixGuardProbeImportPath + `"

func build() interface{} { return nil }
`,
				want: 0,
			},
			{
				name: "a _test.go file is not production and is skipped",
				src:  header + imp + "func build() interface{} { return ids.NewNameSuffixes() }\n",
				want: 0,
			},
		}

		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				fname := "probe.go"
				if strings.Contains(tc.name, "_test.go") {
					fname = "probe_test.go"
				}
				// Nest it, so the walk is exercised rather than a flat read.
				sub := filepath.Join(dir, "internal", "somepkg")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatalf("creating %s: %v", sub, err)
				}
				if err := os.WriteFile(filepath.Join(sub, fname), []byte(tc.src), 0o644); err != nil {
					t.Fatalf("writing %s: %v", fname, err)
				}

				scan, err := scanForFreshSuffixCounter(dir, freshSuffixGuardProbeImportPath)
				if err != nil {
					t.Fatalf("scanning %s: %v", dir, err)
				}
				wantParsed := 1
				if fname == "probe_test.go" {
					wantParsed = 0
				}
				if scan.Parsed != wantParsed {
					t.Fatalf("parsed %d file(s), want %d", scan.Parsed, wantParsed)
				}
				if len(scan.Offences) != tc.want {
					t.Fatalf("found %d offence(s) %v, want %d; source:\n%s", len(scan.Offences), scan.Offences, tc.want, tc.src)
				}
			})
		}
	})

	// The skip list is the guard's one deliberate blind spot, so its CONTENTS
	// are pinned, not merely its behaviour.
	//
	// This subtest replaces one that claimed — in a comment — to stop anyone
	// "widening the list into a hole without a test going red", and did not:
	// adding "hub" to the map and planting a real banned reference in
	// internal/hub left the suite printing ok, because nothing asserted the set
	// was exactly the declared four. The exact-set assertion below is what that
	// sentence was promising.
	t.Run("SkipListIsExactlyTheDeclaredSetAndIsHonoured", func(t *testing.T) {
		wantSkipped := []string{".git", "node_modules", "testdata", "vendor"}
		got := make([]string, 0, len(freshSuffixGuardSkipDirs))
		for name, skip := range freshSuffixGuardSkipDirs {
			if skip {
				got = append(got, name)
			}
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(wantSkipped, ",") {
			t.Fatalf("freshSuffixGuardSkipDirs = %v, want exactly %v. Every entry SILENCES the guard for everything beneath it, so adding one must be a deliberate, reviewed change and not a quiet exclusion of a package that turned out to be inconvenient", got, wantSkipped)
		}

		src := "package probe\n\nimport \"" + freshSuffixGuardProbeImportPath + "\"\n\nfunc build() interface{} { return ids.NewNameSuffixes() }\n"
		check := func(t *testing.T, dirName string, wantSkip bool) {
			t.Helper()
			root := t.TempDir()
			sub := filepath.Join(root, dirName, "pkg")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatalf("creating %s: %v", sub, err)
			}
			if err := os.WriteFile(filepath.Join(sub, "probe.go"), []byte(src), 0o644); err != nil {
				t.Fatalf("writing probe.go: %v", err)
			}
			scan, err := scanForFreshSuffixCounter(root, freshSuffixGuardProbeImportPath)
			if err != nil {
				t.Fatalf("scanning %s: %v", root, err)
			}
			if wantSkip && len(scan.Offences) != 0 {
				t.Fatalf("%s/ was scanned (%v); it is on the skip list", dirName, scan.Offences)
			}
			if !wantSkip && len(scan.Offences) != 1 {
				t.Fatalf("%s/ yielded %d offence(s) %v, want 1; it must be scanned", dirName, len(scan.Offences), scan.Offences)
			}
		}

		// Every declared entry really is honoured...
		for _, dirName := range wantSkipped {
			dirName := dirName
			t.Run("skipped/"+dirName, func(t *testing.T) { check(t, dirName, true) })
		}
		// ...and the real production trees are not.
		for _, dirName := range []string{"internal", "cmd", "client", "hub"} {
			dirName := dirName
			t.Run("scanned/"+dirName, func(t *testing.T) { check(t, dirName, false) })
		}
	})
}
