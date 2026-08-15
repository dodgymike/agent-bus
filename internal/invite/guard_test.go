// INVITE-HARDEN's structural regression guard for the invite-secret comparison.
//
// It is an AST guard rather than a behavioural test, and it follows the
// precedent set by client/guard_test.go: the failure it exists to catch is
// silent, so no positive test can see it. A byte-at-a-time comparison of an
// invite secret still admits the right holder, still refuses the wrong one, and
// still passes every functional test in this package — while handing an
// enumerator a timing oracle over a bearer credential.
package invite_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Source loading
// ---------------------------------------------------------------------------

// parsePackageSources parses every NON-TEST .go file of package invite, from the
// package directory (the test's working directory).
//
// Test files are excluded because they are not the production surface — and
// because this file necessarily writes the shapes it bans in order to prove it
// can detect them (see the detector self-test below).
//
// It FAILS if it parsed nothing. A guard that inspected zero files passes
// forever and proves nothing; that is the vacuity trap CLAUDE.md names, and it
// is the same protection client/guard_test.go's walkGoFiles carries.
func parsePackageSources(t *testing.T) (*token.FileSet, map[string]*ast.File, []string) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	var names []string

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		files[name] = f
		names = append(names, name)
	}
	if len(files) == 0 {
		t.Fatalf("parsed 0 non-test .go files in the invite package directory; this guard is vacuous")
	}
	sort.Strings(names)
	return fset, files, names
}

// render prints an expression back to source on one line, for allowlist keys
// and for error messages. Allowlisting by RENDERED TEXT rather than by line
// number is deliberate: line numbers drift with every edit above them, so a
// line-numbered allowlist silently starts exempting a different expression.
func render(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, e); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

// ---------------------------------------------------------------------------
// Credential taint
// ---------------------------------------------------------------------------

// credentialNames are the identifiers and struct fields that hold, or are
// derived one-to-one from, an invite bearer secret OR from a value whose
// byte-at-a-time comparison would hand a caller something it is not entitled
// to:
//
//	Secret            — RedeemRequest.Secret and Mint.Secret: the PLAINTEXT credential
//	SecretDigest      — HashSecret's output as stored in a Record
//	dummyDigest       — the per-store random digest an UNKNOWN invite id is compared
//	                    against, which stands in for a real SecretDigest and must be
//	                    compared exactly as one
//	RedeemKey         — the idempotency key of the redemption that spent the invite
//	RedeemFingerprint — that redemption's payload fingerprint
//
// The last two are here because of what Store.Begin does with them, and the
// comment at that comparison already says it: once the secret has verified,
// matching BOTH decides whether the caller is handed the ORIGINAL RESULT, which
// for enrolment is an agent identity. A byte-at-a-time compare there lets a
// secret-holder recover the original key and fingerprint by timing and then
// replay somebody else's enrolment result to itself. Reverting either to `==`
// compiles, keeps every functional test green, and is invisible — which is
// exactly the class of regression this guard exists for.
var credentialNames = map[string]bool{
	"Secret":            true,
	"SecretDigest":      true,
	"dummyDigest":       true,
	"RedeemKey":         true,
	"RedeemFingerprint": true,
}

// importBindings maps the package qualifiers a file binds — `subtle`, `hmac`,
// whatever an import alias names them — onto the import paths behind them.
//
// It is the ONE place import resolution is spelled, because both guard 1 and
// safeComparator need it and they must not drift apart. A name-only check is a
// rename away from no protection at all: `subtle.ConstantTimeCompare` spelled
// over `import subtle "some/other/pkg"` satisfies every name-based rule while
// comparing however that package pleases, and every other test stays green.
func importBindings(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		out[local] = path
	}
	return out
}

// importPath resolves a package qualifier as bound BY THE FILE the expression
// lives in, and returns "" if that file binds no such name.
//
// Per-file resolution is the whole point. Guard 1 resolves `subtle` only in the
// file that declares VerifySecret; before this existed, safeComparator trusted
// the bare identifier `subtle` in EVERY other file, so a second file binding
// that name to an impostor package and comparing there stayed green — while
// guard 1's own comment told the reader that hole was closed.
func (a *analysis) importPath(pos token.Pos, local string) string {
	return a.imports[a.fset.Position(pos).Filename][local]
}

// safeComparator reports whether a call IS the approved comparison, and so
// terminates taint rather than propagating it.
//
// Without this, `subtle.ConstantTimeCompare(got[:], stored[:]) != 1` — the
// CORRECT code — would be flagged, because it is a `!=` whose left operand is a
// call over tainted arguments. A guard that fails correct work is a guard the
// next agent deletes.
//
// EXACTLY THREE SPELLINGS ARE SAFE, and each is checked against the file's own
// import bindings:
//
//   - crypto/subtle.ConstantTimeCompare — the only permitted comparison;
//   - crypto/hmac.Equal — a thin, audited wrapper over the same;
//   - VerifySecret — this package's one wrapper over subtle, whose body guard 1
//     pins.
//
// ANY OTHER crypto/subtle FUNCTION IS NOT SAFE HERE, even though it is a
// constant-time primitive. `for i := range got { if
// subtle.ConstantTimeByteEq(got[i], stored[i]) == 0 { return false } }` is
// constant-time per byte and leaks the prefix length through the loop's trip
// count — it is "inventing a bespoke construction out of otherwise-good
// primitives", which invariant 9 enumerates BY NAME precisely because it does
// not feel like writing your own crypto. Treating all of subtle.* as safe let
// that mutant stay green.
func (a *analysis) safeComparator(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "VerifySecret"
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		if !ok {
			return f.Sel.Name == "VerifySecret"
		}
		switch pkg.Name {
		case "subtle":
			return a.importPath(f.Pos(), "subtle") == "crypto/subtle" && f.Sel.Name == "ConstantTimeCompare"
		case "hmac":
			return a.importPath(f.Pos(), "hmac") == "crypto/hmac" && f.Sel.Name == "Equal"
		}
		return f.Sel.Name == "VerifySecret"
	}
	return false
}

// disallowedCall reports whether a call is one this guard bans OVER CREDENTIAL
// MATERIAL, and why. The caller checks the arguments; this only classifies the
// callee.
//
// Three families:
//
//   - the non-constant-time comparison helpers in bannedComparators;
//   - an IMPOSTOR `subtle` or `hmac` — a file binding either name to something
//     other than crypto/subtle or crypto/hmac. Flagged as a call and not merely
//     as a non-terminator of taint, because `return hmac.Equal(a, b)` contains
//     no comparison for the ast.BinaryExpr arm to see;
//   - a crypto/subtle function that is NOT ConstantTimeCompare. Flagged even
//     where no comparison exists, so that an accumulator loop —
//     `ok &= subtle.ConstantTimeByteEq(got[i], stored[i])` — is caught as well
//     as the `== 0` spelling.
func (a *analysis) disallowedCall(fun ast.Expr) (string, bool) {
	pkg, name := funcName(fun)
	if bannedComparators[pkg][name] {
		return pkg + "." + name + " on invite-secret material", true
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if id, isID := sel.X.(*ast.Ident); !isID || (id.Name != "subtle" && id.Name != "hmac") {
		return "", false
	}
	want := map[string]string{"subtle": "crypto/subtle", "hmac": "crypto/hmac"}[pkg]
	if got := a.importPath(fun.Pos(), pkg); got != want {
		if got == "" {
			return "a call to `" + pkg + "." + name + "` in a file that does not import " + want, true
		}
		return "a call to `" + pkg + "." + name + "`, where `" + pkg + "` is bound to " + got + ", NOT " + want, true
	}
	if pkg == "subtle" && name != "ConstantTimeCompare" {
		return "subtle." + name + " used on credential material; the only approved comparison is subtle.ConstantTimeCompare", true
	}
	if pkg == "hmac" && name == "Equal" {
		return "", false
	}
	return "", false
}

func funcName(fun ast.Expr) (pkg, name string) {
	switch f := fun.(type) {
	case *ast.Ident:
		return "", f.Name
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name, f.Sel.Name
		}
		return "", f.Sel.Name
	}
	return "", ""
}

// isDigestType reports whether a type expression is a secret digest by SHAPE:
// [DigestSize]byte or [32]byte. Shape is checked because names are the thing an
// author is free to choose.
func isDigestType(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	if !ok || arr.Len == nil {
		return false
	}
	elt, ok := arr.Elt.(*ast.Ident)
	if !ok || elt.Name != "byte" {
		return false
	}
	switch n := arr.Len.(type) {
	case *ast.Ident:
		return n.Name == "DigestSize"
	case *ast.BasicLit:
		return n.Value == "32"
	}
	return false
}

// isConversion reports whether a call is a type conversion — string(b),
// []byte(s), [32]byte(x) — as opposed to a function call. A conversion carries
// its operand's value through unchanged, so it must carry taint through
// unchanged too.
func isConversion(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.ArrayType:
		return true
	case *ast.Ident:
		return f.Name == "string"
	case *ast.ParenExpr:
		return isConversion(f.X)
	}
	return false
}

// bannedComparators are the non-constant-time comparison helpers. Each returns
// or short-circuits on the first differing byte, so each leaks a prefix length
// through timing when either side is attacker-supplied.
//
// reflect.DeepEqual and the slices helpers are here because a mutant using
// either stayed GREEN against the first version of this guard: they read as
// obviously-correct generic equality, they are what an author reaches for on a
// `[32]byte` or a `[]byte`, and reflect.DeepEqual in particular walks byte by
// byte and returns at the first difference exactly like bytes.Equal.
var bannedComparators = map[string]map[string]bool{
	"bytes":   {"Equal": true, "EqualFold": true, "Compare": true, "HasPrefix": true, "HasSuffix": true, "Contains": true, "Index": true},
	"strings": {"EqualFold": true, "Compare": true, "HasPrefix": true, "HasSuffix": true, "Contains": true, "Index": true},
	"reflect": {"DeepEqual": true},
	"slices":  {"Equal": true, "EqualFunc": true, "Compare": true, "Contains": true, "Index": true},
}

type finding struct {
	file string
	pos  token.Pos
	expr string
	what string
}

// analysis is the whole-package taint state: which declarations exist, and
// which identifier names carry invite-secret material INSIDE each of them.
//
// Taint is per-DECLARATION rather than per-file (the shape this guard had
// before interprocedural propagation was added). That change is not cosmetic:
// once a call can taint a callee's parameter, a flat file-wide or package-wide
// name set collapses `v` in parseDigest onto `v` in parseTime and `s` in
// parseState onto the `s` receiver of every Store method, and the guard starts
// failing on `switch s { case "open": }`. A guard that fails on unrelated code
// is a guard the next agent deletes.
type analysis struct {
	fset  *token.FileSet
	files map[string]*ast.File
	names []string

	// funcs indexes package-local declarations: plain functions and methods,
	// keyed by their (method) name.
	funcs map[string][]*ast.FuncDecl

	// imports holds, per file, the package qualifiers that file binds.
	imports map[string]map[string]string

	// taint holds, per TOP-LEVEL DECLARATION, the identifier names tainted
	// inside it. A GenDecl is keyed here too, not only a FuncDecl: a package
	// level `var digestsEqual = func(a, b [DigestSize]byte) bool { return a ==
	// b }` is a credential comparison written inside a var declaration, and
	// scanning it with a permanently-empty taint map is what let it stay green
	// while the IDENTICAL code written as a func declaration is one of the
	// self-tests below.
	taint map[ast.Decl]map[string]bool
}

func newAnalysis(t *testing.T, fset *token.FileSet, files map[string]*ast.File) *analysis {
	t.Helper()
	a := &analysis{
		fset:    fset,
		files:   files,
		funcs:   map[string][]*ast.FuncDecl{},
		imports: map[string]map[string]string{},
		taint:   map[ast.Decl]map[string]bool{},
	}
	for name := range files {
		a.names = append(a.names, name)
	}
	sort.Strings(a.names)
	for _, name := range a.names {
		a.imports[name] = importBindings(files[name])
		for _, d := range files[name].Decls {
			a.taint[d] = map[string]bool{}
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			a.funcs[fd.Name.Name] = append(a.funcs[fd.Name.Name], fd)
		}
	}
	a.computeTaint(t)
	return a
}

// resolve maps a call target onto a PACKAGE-LOCAL declaration, or nil.
//
// A bare identifier resolves to a plain function; a selector resolves to a
// METHOD of that name. That split is what keeps `idem.ValidateKey(...)` and
// `hex.DecodeString(...)` — cross-package calls this guard cannot see into —
// from being mistaken for local methods, while still resolving
// `s.upsertLocked(...)` and `cur.Expired(now)`.
func (a *analysis) resolve(fun ast.Expr) *ast.FuncDecl {
	switch f := fun.(type) {
	case *ast.ParenExpr:
		return a.resolve(f.X)
	case *ast.Ident:
		for _, fd := range a.funcs[f.Name] {
			if fd.Recv == nil {
				return fd
			}
		}
	case *ast.SelectorExpr:
		for _, fd := range a.funcs[f.Sel.Name] {
			if fd.Recv != nil {
				return fd
			}
		}
	}
	return nil
}

// flatParams lists a declaration's parameter names in call order, flattening
// grouped declarations like `func eq(a, b []byte)`.
func flatParams(fd *ast.FuncDecl) []string {
	if fd.Type == nil || fd.Type.Params == nil {
		return nil
	}
	var out []string
	for _, f := range fd.Type.Params.List {
		if len(f.Names) == 0 {
			out = append(out, "")
			continue
		}
		for _, id := range f.Names {
			out = append(out, id.Name)
		}
	}
	return out
}

// returnsDigest reports whether a declaration's results include a digest-shaped
// value, so that a call to it carries taint out. Without this a two-line
// accessor — `func stored(r Record) [32]byte { return r.SecretDigest }` — would
// launder the digest past every name-based rule.
func returnsDigest(fd *ast.FuncDecl) bool {
	if fd.Type == nil || fd.Type.Results == nil {
		return false
	}
	for _, f := range fd.Type.Results.List {
		if isDigestType(f.Type) {
			return true
		}
	}
	return false
}

// maxTaintRounds bounds the fixpoint. Taint only ever GROWS, over a finite set
// of (declaration, identifier) pairs, so the iteration terminates on its own;
// the cap exists solely so a bug cannot spin forever. Package invite converges
// in 3 rounds as measured by the reviewer, so this is a ~5x margin.
const maxTaintRounds = 16

// computeTaint runs the taint rules to a FIXPOINT over the whole package.
//
// A fixpoint rather than a single pass because interprocedural propagation is
// transitive in practice: Store.Begin taints VerifySecret's parameters, and
// VerifySecret's body then taints HashSecret's. One hop would stop at the first
// helper, which is precisely the shape that let `eq(got[:], stored[:])` through.
//
// EXHAUSTING THE CAP IS A HARD FAILURE, not a fall-through. An under-approximated
// taint set is a SILENT MISS: the guard would keep passing while quietly no
// longer seeing whatever the unfinished rounds would have reached — the exact
// failure class invariant 9 warns about, where the protection is gone and every
// test is still green. If this fires, raise the cap deliberately after checking
// the rules still converge; do not remove the check.
func (a *analysis) computeTaint(t *testing.T) {
	t.Helper()
	for round := 0; round < maxTaintRounds; round++ {
		changed := false
		for _, name := range a.names {
			for _, d := range a.files[name].Decls {
				if a.taintDecl(d) {
					changed = true
				}
			}
		}
		if !changed {
			return
		}
	}
	t.Fatalf("the credential-taint fixpoint did not converge in %d rounds (maxTaintRounds). "+
		"The taint set is therefore UNDER-APPROXIMATED and this guard is now silently weaker than it reports: comparisons it would have flagged after the remaining rounds are simply not seen, while the test still passes. "+
		"Package invite converged in 3 rounds when this cap was set. Work out what grew — a new deeply-chained helper, or a rule that keeps adding names — and raise maxTaintRounds deliberately once the rules are known to converge. Do not delete this check.", maxTaintRounds)
}

// taintDecl applies one round of the taint rules inside a top-level declaration
// — a func declaration or a var/const block that may contain func literals —
// reporting whether anything new became tainted (here or in a callee).
func (a *analysis) taintDecl(decl ast.Decl) bool {
	local := a.taint[decl]
	changed := false
	add := func(m map[string]bool, name string) {
		if name == "" || name == "_" || m[name] {
			return
		}
		m[name] = true
		changed = true
	}

	ast.Inspect(decl, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncType:
			// Parameters and results declared as a DIGEST — [DigestSize]byte
			// or [32]byte — are tainted whatever they are called. Without
			// this, `func digestsEqual(a, b [DigestSize]byte) bool { return
			// a == b }` launders the credential through two neutral names
			// and slips past every name-based rule. Observed while
			// mutation-testing this guard, which is the only reason it is
			// here.
			//
			// It reaches func LITERALS as well as declarations, which is what
			// closes the var-block spelling of that same laundering.
			for _, group := range []*ast.FieldList{s.Params, s.Results} {
				if group == nil {
					continue
				}
				for _, f := range group.List {
					if !isDigestType(f.Type) {
						continue
					}
					for _, id := range f.Names {
						add(local, id.Name)
					}
				}
			}

		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(s.Rhs) {
					continue
				}
				if a.derived(s.Rhs[i], local, false) {
					add(local, id.Name)
				}
			}

		case *ast.ValueSpec:
			for i, id := range s.Names {
				if i < len(s.Values) && a.derived(s.Values[i], local, false) {
					add(local, id.Name)
				}
			}

		case *ast.CallExpr:
			// INTERPROCEDURAL PROPAGATION. Passing a tainted argument to a
			// package-local function taints the corresponding PARAMETER of
			// that function, so the comparison scan sees the callee's body as
			// credential-handling code.
			//
			// This closes the hole the reviewer demonstrated: taint reached
			// through [DigestSize]byte parameters but not through []byte ones,
			// so `func eq(a, b []byte) bool { return bytes.Equal(a, b) }`
			// called as `eq(got[:], stored[:])` stayed GREEN — the guard
			// advertised catching exactly that regression and did not.
			callee := a.resolve(s.Fun)
			if callee == nil {
				return true
			}
			params := flatParams(callee)
			for i, arg := range s.Args {
				if i >= len(params) {
					break
				}
				if a.derived(arg, local, true) {
					add(a.taint[callee], params[i])
				}
			}
		}
		return true
	})
	return changed
}

// derived reports whether an expression carries invite-secret material, either
// directly or through a slice, index or conversion.
//
// It recurses through index expressions on purpose: `got[i] != stored[i]`
// inside a loop is the textbook byte-at-a-time compare, and it is exactly the
// regression this guard exists to catch. Slice expressions matter for the same
// reason and for one more: `got[:]` is how an array reaches a []byte helper,
// which is the laundering route the reviewer's mutant used.
//
// throughCalls controls whether an ordinary function call propagates taint from
// its arguments to its result. It is TRUE when classifying a comparison operand
// (so that a comparison over a helper's return value is still seen) and FALSE
// when deciding whether an ASSIGNMENT taints a local. The distinction is not
// cosmetic: with it on, `err := parseDigest("secret_sha256", j.SecretDigest,
// r.SecretDigest[:])` made `err` a credential, and every `err != nil` in
// record.go became a violation. A guard that fails on `err != nil` is a guard
// the next agent deletes, so the assignment side stops at HashSecret, at a
// package-local function that RETURNS a digest, and at conversions.
//
// len()/cap() terminate taint either way: a LENGTH is not a credential, and
// bounding the size of untrusted input is a defence, not a leak.
func (a *analysis) derived(e ast.Expr, tainted map[string]bool, throughCalls bool) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return a.derived(x.X, tainted, throughCalls)
	case *ast.SliceExpr:
		return a.derived(x.X, tainted, throughCalls)
	case *ast.IndexExpr:
		return a.derived(x.X, tainted, throughCalls)
	case *ast.StarExpr:
		return a.derived(x.X, tainted, throughCalls)
	case *ast.UnaryExpr:
		return a.derived(x.X, tainted, throughCalls)
	case *ast.Ident:
		return tainted[x.Name] || credentialNames[x.Name]
	case *ast.SelectorExpr:
		return credentialNames[x.Sel.Name] || a.derived(x.X, tainted, throughCalls)
	case *ast.CallExpr:
		if a.safeComparator(x.Fun) {
			return false
		}
		if _, name := funcName(x.Fun); name == "HashSecret" {
			return true
		}
		if _, name := funcName(x.Fun); name == "len" || name == "cap" {
			return false
		}
		if callee := a.resolve(x.Fun); callee != nil && returnsDigest(callee) {
			return true
		}
		if !isConversion(x.Fun) && !throughCalls {
			return false
		}
		for _, arg := range x.Args {
			if a.derived(arg, tainted, throughCalls) {
				return true
			}
		}
		return false
	}
	return false
}

// findings returns every `==`, `!=`, switch/case equality, bytes.Equal,
// reflect.DeepEqual (etc.) applied to credential material anywhere in the
// package.
func (a *analysis) findings() []finding {
	var out []finding
	for _, name := range a.names {
		for _, d := range a.files[name].Decls {
			// EVERY top-level declaration is scanned with ITS OWN computed
			// taint, a var/const block exactly like a func declaration. The
			// earlier shape handed a GenDecl a permanently-empty map, so a
			// comparison inside a package-level func literal saw only the
			// name-based credential set — and
			// `var digestsEqual = func(a, b [DigestSize]byte) bool { return a
			// == b }` stayed green while the identical func DECLARATION was
			// already a self-test.
			out = append(out, a.scan(name, d, a.taint[d])...)
		}
	}
	return out
}

func (a *analysis) scan(file string, root ast.Node, tainted map[string]bool) []finding {
	var out []finding
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op != token.EQL && x.Op != token.NEQ {
				return true
			}
			if !a.derived(x.X, tainted, true) && !a.derived(x.Y, tainted, true) {
				return true
			}
			out = append(out, finding{file: file, pos: x.Pos(), expr: render(a.fset, x), what: x.Op.String() + " on invite-secret material"})

		case *ast.SwitchStmt:
			// `switch got { case stored: }` IS an equality comparison, spelled
			// so that no ast.BinaryExpr exists to inspect. A mutant using it
			// stayed GREEN against the first version of this guard.
			//
			// A TAGLESS switch is skipped deliberately: its cases are ordinary
			// boolean expressions and the ast.BinaryExpr arm above already
			// reports them, so handling it here would double-report every
			// `case r.RedeemKey != "":` in record.go.
			if x.Tag == nil || x.Body == nil {
				return true
			}
			tagTainted := a.derived(x.Tag, tainted, true)
			for _, stmt := range x.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, ce := range cc.List {
					if !tagTainted && !a.derived(ce, tainted, true) {
						continue
					}
					out = append(out, finding{
						file: file,
						pos:  ce.Pos(),
						expr: "switch " + render(a.fset, x.Tag) + " { case " + render(a.fset, ce) + " }",
						what: "switch/case equality on invite-secret material",
					})
				}
			}

		case *ast.CallExpr:
			what, bad := a.disallowedCall(x.Fun)
			if !bad {
				return true
			}
			for _, arg := range x.Args {
				if a.derived(arg, tainted, true) {
					out = append(out, finding{file: file, pos: x.Pos(), expr: render(a.fset, x), what: what})
					break
				}
			}
		}
		return true
	})
	return out
}

// allowedComparisons are the comparisons touching credential-shaped values that
// are NOT credential checks. Each is keyed by "<file>: <expression>" and carries
// the one-line reason it is safe. Nothing else may be added without a security
// gate: an entry here is a hole in the guard.
//
// Every entry must MATCH something — a stale entry is asserted below. An
// allowlist pointing at an expression that no longer exists is how a guard
// quietly stops guarding.
var allowedComparisons = map[string]string{
	// A length/emptiness bound on untrusted input arriving pre-auth. It reveals
	// only whether the caller sent a field at all — no byte of any credential
	// participates, and there is nothing to enumerate. The companion
	// len(req.Secret) > MaxSecretLen is not a comparison this guard inspects.
	`store.go: req.Secret == ""`: "emptiness bound on untrusted input, not a credential comparison",

	// A WAL-replay divergence check between two SERVER-HELD digests: the record
	// already in the table and the record being folded in. No attacker supplies
	// either side and no attacker observes its timing, so there is no oracle;
	// what it detects is corruption or an attempt to rebind an invite to a
	// different secret.
	"store.go: existing.SecretDigest != r.SecretDigest": "server-held vs server-held replay divergence check; neither side is attacker-supplied",

	// A zero-value validity check on a server-held field: an all-zero digest is
	// an uninitialised field or corruption, never a presented credential.
	"record.go: r.SecretDigest == zeroDigest": "zero-value validity check on a server-held field",

	// --- added with RedeemKey / RedeemFingerprint (INVITE-HARDEN, review round 2)
	//
	// Each of the five below was read at its site before being exempted. Four
	// are presence/zero-value checks that consume no byte of either value beyond
	// "is it set", and the fifth compares two records that both came off the
	// durable log.

	// record.go, Record.validate, StateRedeemed arm. A ZERO-VALUE check on a
	// record read off disk: it refuses a redeemed record whose "redeem_fp" was
	// dropped or zeroed, because a stored zero would match a request carrying no
	// fingerprint at all and replay an agent identity to it. It compares against
	// a CONSTANT (the zero fingerprint), so there is no second value to recover
	// and no caller-supplied side.
	"record.go: r.RedeemFingerprint == (idem.Fingerprint{})": "zero-value validity check against a constant; no caller-supplied side and nothing to enumerate",

	// record.go, mustHaveNoRedemption. An EMPTINESS check enforcing "a
	// non-redeemed record carries no redeem_key". It compares against the empty
	// string, so it decides only whether the field is set at all; the key's bytes
	// never participate.
	`record.go: r.RedeemKey != ""`: "emptiness check enforcing that a non-redeemed record carries no redeem key; the key's bytes never participate",

	// record.go, mustHaveNoRedemption. The same emptiness check for the
	// fingerprint, against the zero value rather than "".
	"record.go: r.RedeemFingerprint != (idem.Fingerprint{})": "zero-value check enforcing that a non-redeemed record carries no fingerprint; against a constant, not a secret",

	// record.go, DecodeRecord. An emptiness check on the DECODED JSON STRING,
	// deciding whether the optional "redeem_fp" field is present and therefore
	// whether to parse it at all. It runs on the replay/decode path over bytes
	// this bus wrote, and it compares against "".
	`record.go: j.RedeemFingerprint != ""`: "presence check on an optional JSON field before parsing it; compares against \"\", on the decode path",

	// store.go, sameEvent. The REPLAY FOLD: "is the record being applied the one
	// I already have?", between two records that both came off the durable log
	// (Store.Apply) or between a durable record and its own in-memory copy
	// (foldIn). It is unreachable with an attacker-chosen key on the request
	// path — upsertLocked only consults sameEvent when BOTH records are already
	// in the same terminal state, and Store.Begin refuses a second redemption of
	// a spent invite (ErrAlreadyRedeemed / OutcomeReplay) before any second
	// record is ever built. So no caller supplies a side here and no caller times
	// it.
	//
	// THE ARGUMENT ABOVE IS ONLY HALF OF IT, and the other half lives in a
	// different function, so name it here: the Begin refusal does not by itself
	// close the INTERLEAVED route — reserve with K1, let the sweep reap the
	// reservation for being held past ReservationTTL, redeem afresh with K2,
	// then Commit the first redemption, which would hand sameEvent a
	// caller-chosen K1 against a stored K2. That route is closed independently
	// by the `r.t.done` guards at the top of Redemption.Consume and
	// Redemption.Commit: reaping sets done, Consume then refuses to build a
	// record at all and Commit returns without upserting, so the reaped
	// redemption never reaches this comparison. A change to reservation reaping
	// that relaxes either guard invalidates THIS exemption, in another file, with
	// nothing else going red.
	"store.go: a.RedeemKey == b.RedeemKey": "replay/fold identity check between two server-held records; Begin refuses a second redemption before this is reachable with a caller-chosen key, and the reap-then-commit interleaving is refused by the r.t.done guards in Redemption.Consume and Redemption.Commit",
}

// ---------------------------------------------------------------------------
// The guard
// ---------------------------------------------------------------------------

// TestInviteSecretComparedInConstantTime is INVITE-HARDEN's recorded proof, and
// it holds three INDEPENDENT structural guards over package invite.
//
// # WHAT THIS TEST ESTABLISHES, AND WHAT IT DOES NOT — read this before citing it
//
// It establishes STRUCTURE and ORDER, nothing more:
//
//   - guard 1: VerifySecret's body calls crypto/subtle.ConstantTimeCompare, and
//     the identifier `subtle` in that file really is crypto/subtle;
//   - guard 2: no non-test file in this package compares credential material —
//     the secret, its digest, the per-store dummy digest, the stored redemption
//     key or its fingerprint — with ==, !=, a switch/case, bytes.Equal,
//     strings.EqualFold, reflect.DeepEqual or slices.Equal, outside a small
//     commented allowlist of non-credential comparisons. Taint follows values
//     through slices, indexes, conversions and INTO package-local helpers;
//     see the KNOWN LIMITATIONS on guardNoUnsafeCredentialComparison for what it
//     still does not see;
//   - guard 3: Store.Begin's VerifySecret gate precedes EVERY invite-state
//     sentinel it returns — ErrRedemptionInFlight, ErrKeyReuse, ErrExpired,
//     ErrRevoked and ErrAlreadyRedeemed.
//
// It DOES NOT MEASURE TIME. It cannot. There is no wall-clock assertion here and
// there must never be one: a timing assertion on a shared CI box is flaky in
// both directions, and a flaky one that happens to pass would be a FALSE claim
// of evidence for the very property that matters. NO unit test in this
// repository proves a timing property, and this one does not either. What it
// proves is that the code still has the SHAPE that a constant-time comparison
// requires — which is exactly the thing a refactor silently destroys and which
// no functional test can see, since every functional test passes either way.
//
// Nor does it reach beyond this package. Whether the HTTP layer collapses the
// distinct error sentinels into one indistinguishable response is asserted
// where that code lives — httpapi.writeInviteError, covered by
// TestInviteGateEveryInviteRefusalIsIndistinguishable — not here.
//
// Invariant 9 is why this is written as a guard at all: broken crypto fails
// silently, so "our tests pass" is not evidence. Do not respond to a failure
// here by writing a comparison of your own — the ONLY permitted remedy is
// crypto/subtle.ConstantTimeCompare.
func TestInviteSecretComparedInConstantTime(t *testing.T) {
	fset, files, names := parsePackageSources(t)
	t.Logf("scanned %d non-test files: %s", len(files), strings.Join(names, " "))

	t.Run("guard1_VerifySecretUsesConstantTimeCompare", func(t *testing.T) {
		guardVerifySecretUsesSubtle(t, fset, files)
	})
	t.Run("guard2_NoByteAtATimeCredentialComparison", func(t *testing.T) {
		guardNoUnsafeCredentialComparison(t, fset, files)
	})
	t.Run("guard3_SecretGateRunsBeforeStateDisclosingErrors", func(t *testing.T) {
		guardBeginChecksSecretFirst(t, fset, files)
	})
}

// guardVerifySecretUsesSubtle is guard 1, the POSITIVE half: the one function
// every redemption funnels through must still perform the approved comparison.
//
// It also resolves the import, not just the identifier. `subtle.ConstantTimeCompare`
// spelled over an `import subtle "some/other/pkg"` would satisfy a name-only
// check while comparing however that package pleases — a rename away from no
// protection at all, with every other test still green.
func guardVerifySecretUsesSubtle(t *testing.T, fset *token.FileSet, files map[string]*ast.File) {
	t.Helper()

	var decl *ast.FuncDecl
	var declFile string
	var declAST *ast.File
	found := 0
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "VerifySecret" {
				continue
			}
			found++
			decl, declFile, declAST = fd, name, f
		}
	}
	if found != 1 {
		t.Fatalf("found %d package-level func VerifySecret in package invite; expected exactly 1. "+
			"If it was renamed, moved or split, this guard must move with it — a guard pointing at a function that no longer exists passes forever.", found)
	}

	// The identifier `subtle` in the declaring file must be crypto/subtle.
	//
	// This resolves ONLY the declaring file — which is all guard 1 can claim.
	// The same importBindings helper is applied per-file by guard 2's
	// safeComparator, so a second file binding `subtle` to an impostor and
	// comparing there is caught THERE; before that existed this comment
	// over-promised, and a mutant proved it.
	subtlePath := importBindings(declAST)["subtle"]
	if subtlePath != "crypto/subtle" {
		if subtlePath == "" {
			t.Errorf("%s declares VerifySecret but does not import \"crypto/subtle\". Invariant 9: the comparison of a bearer credential must use crypto/subtle.ConstantTimeCompare, never a hand-rolled or 'adapted' one.", declFile)
		} else {
			t.Errorf("%s binds the identifier `subtle` to %q, not \"crypto/subtle\". A local package spelled `subtle` would satisfy a name-only check while comparing however it likes — invariant 9 forbids exactly that.", declFile, subtlePath)
		}
	}

	var calls int
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
			calls++
		}
		return true
	})
	if calls == 0 {
		t.Errorf("%s: VerifySecret's body contains no call to subtle.ConstantTimeCompare (%s). "+
			"An invite secret is a BEARER CREDENTIAL: a byte-at-a-time comparison still admits the right holder and refuses the wrong one, so every functional test in this package passes either way, while leaking the digest prefix through timing. "+
			"The only permitted fix is crypto/subtle.ConstantTimeCompare — do not write, adapt or wrap a comparison of your own (invariant 9).",
			fset.Position(decl.Pos()), declFile)
	}
}

// guardNoUnsafeCredentialComparison is guard 2, the NEGATIVE half — and it is
// the one that catches the realistic regression.
//
// Guard 1 only pins the function that exists today. The way this protection
// actually disappears is a SECOND verification helper: a well-meant
// verifySecretFast, a replay-comparison added to a new code path, a digest
// equality check written with == because two [32]byte arrays compare so
// naturally in Go. Each of those reads as obviously correct and each reintroduces
// the oracle beside a VerifySecret that is still perfect.
//
// # KNOWN LIMITATIONS — read these before citing this guard as coverage
//
// THIS LIST IS NOT EXHAUSTIVE. It records the holes that have been MEASURED —
// each one confirmed by compiling a mutant and watching the detector stay green
// — and every round of review so far has found more. Two rounds after the list
// below was first written as if it were complete, a security re-verification
// added four entries and a reviewer added a fifth. Treat it as the running
// tally of what is KNOWN to be missing, never as the boundary of what is
// missing: an absent shape means nobody has tried it yet, not that it is
// covered. This paragraph is the most important line in the file, because under
// invariant 9 an over-claimed guard is worse than no guard — a reader who
// believes the list is complete stops looking.
//
// This is a syntactic taint analysis with no type information. The measured
// holes:
//
//   - NO TYPE CHECKING. Everything here is name- and shape-based. A digest is
//     recognised as `[DigestSize]byte` or `[32]byte`; a credential field is
//     recognised by the names in credentialNames. Rename SecretDigest to
//     something else in record.go and the whole analysis stops seeing it —
//     which is why the credential set sits at the top of this file with a
//     comment, and why a rename must update it in the same change.
//
//   - TAINT DOES NOT FLOW OUT OF ARBITRARY RETURN VALUES. It flows out of
//     HashSecret and out of any package-local function whose RESULT is
//     digest-shaped (returnsDigest). A helper that returns a credential as a
//     []byte or a string — `func key(r Record) string { return r.RedeemKey }`,
//     then `key(a) == key(b)` — is NOT seen.
//
//   - TAINT DOES NOT FLOW THROUGH CONTAINERS OR THROUGH STRUCT FIELDS THIS
//     GUARD DOES NOT NAME. Assignment taint is recorded against plain
//     identifiers only, so `m["a"] = HashSecret(p)` and `x := box{v: stored}`
//     record nothing. Note the asymmetry, because it is what makes this hole
//     narrow: laundering ONE side is still caught (`m["a"] == stored` fires on
//     the tainted right-hand side), so the shape that escapes is one where BOTH
//     sides have been put through a container or an unnamed field first.
//
//   - TAINT DOES NOT FLOW THROUGH INDIRECTION. A call through a function value,
//     a method value, an interface or a func-typed parameter cannot be resolved
//     to a declaration, so no parameter of the target is tainted: assigning
//     `cmp := eq` and calling `cmp(got[:], stored[:])` is not seen.
//
//   - IT CANNOT SEE INTO ANOTHER PACKAGE. Handing a digest to a helper in
//     internal/foo and comparing it there is invisible; only package invite is
//     parsed.
//
//   - IT IS NOT SCOPE-EXACT WITHIN A FUNCTION. Taint is a set of NAMES per
//     declaration, so a shadowed re-use of a tainted name inside a nested block
//     or a func literal stays tainted. That direction is the safe one: a false
//     alarm a human resolves, never a silent miss.
//
//   - A CALL TARGET IS RESOLVED BY NAME, SO A SHADOWING METHOD LAUNDERS TAINT.
//     resolve picks the FIRST package-local declaration of a given name — for a
//     selector, the first METHOD so named, whatever its receiver type. Declare
//     a second `func (x otherType) verify(...)` and interprocedural propagation
//     may taint that one's parameters instead of the intended receiver's, so
//     the real callee's body is scanned with less taint than it should carry.
//     Found by the reviewer; closing it needs go/types, which this guard
//     deliberately does not depend on.
//
//   - IMPORT RESOLUTION IS PER-FILE AND SYNTACTIC. `subtle` and `hmac` are
//     checked against the importing file's own bindings, which closes the
//     impostor-package hole, but nothing here understands build tags, so a file
//     excluded from this build is still parsed and a file included by a tag
//     this guard cannot evaluate is treated the same as any other.
//
//   - IT SAYS NOTHING ABOUT TIME. See the parent test's doc comment.
func guardNoUnsafeCredentialComparison(t *testing.T, fset *token.FileSet, files map[string]*ast.File) {
	t.Helper()

	// Prove the detector can fail before trusting it to pass. A taint analysis
	// that matched nothing would give this guard a permanent green — the same
	// trap client/guard_test.go's vocabulary self-test closes.
	assertDetects(t, "digest equality", `package p
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	return got == stored
}`, true)
	assertDetects(t, "byte-at-a-time loop", `package p
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	for i := range got {
		if got[i] != stored[i] {
			return false
		}
	}
	return true
}`, true)
	assertDetects(t, "bytes.Equal on a digest", `package p
func f(req RedeemRequest, cur Record) bool {
	return bytes.Equal([]byte(req.Secret), cur.SecretDigest[:])
}`, true)
	assertDetects(t, "digest laundered through neutral parameter names", `package p
func digestsEqual(a, b [DigestSize]byte) bool { return a == b }`, true)

	// The reviewer's mutant: taint reached through [DigestSize]byte parameters
	// but not through []byte ones, so this spelling of the very regression the
	// guard advertises catching stayed GREEN. It is a self-test now because a
	// detector hole, once closed, is exactly the thing a later simplification
	// reopens.
	assertDetects(t, "digest laundered through a []byte helper (reviewer's bypass)", `package p
func eq(a, b []byte) bool { return bytes.Equal(a, b) }
func verifySecret2(p string, stored [DigestSize]byte) bool {
	got := HashSecret(p)
	return eq(got[:], stored[:])
}`, true)

	// Security's mutants: an equality with no ast.BinaryExpr to inspect, and two
	// comparison helpers that were missing from bannedComparators.
	assertDetects(t, "switch/case equality on a digest (security's bypass)", `package p
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	switch got {
	case stored:
		return true
	}
	return false
}`, true)
	assertDetects(t, "reflect.DeepEqual on a digest (security's bypass)", `package p
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	return reflect.DeepEqual(got[:], stored[:])
}`, true)
	assertDetects(t, "slices.Equal on a digest (security's bypass)", `package p
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	return slices.Equal(got[:], stored[:])
}`, true)

	// The stored redemption key and fingerprint are credentials for this
	// guard's purposes: matching them decides whether a caller is handed the
	// ORIGINAL enrolment result. Reverting either to == compiles and keeps every
	// functional test green.
	assertDetects(t, "the redemption key compared with != (reviewer's bypass)", `package p
func f(cur Record, req RedeemRequest) bool {
	return cur.RedeemKey != req.Key
}`, true)
	assertDetects(t, "the redemption fingerprint compared with ==", `package p
func f(cur Record, req RedeemRequest) bool {
	return cur.RedeemFingerprint == req.Fingerprint
}`, true)

	// Security's re-verification mutants, round 3. Each of the three below
	// COMPILED against the real package and stayed GREEN.

	// safeComparator used to treat any `subtle.<anything>` as safe and
	// taint-terminating, while the import binding that proves `subtle` really is
	// crypto/subtle was checked by guard 1 in the declaring file ONLY. A second
	// file binding the name elsewhere compared however that package pleased.
	assertDetects(t, "an impostor `subtle` in a second file (security's bypass)", `package p
import subtle "example.com/not/crypto/subtle"
func verifySecretFast(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	return subtle.ConstantTimeCompare(got[:], stored[:]) == 1
}`, true)

	// A constant-time PRIMITIVE assembled into a comparison that is not: the
	// loop returns at the first differing byte, so the trip count leaks the
	// prefix length exactly like bytes.Equal. Invariant 9 enumerates "inventing
	// a bespoke construction out of otherwise-good primitives" by name.
	assertDetects(t, "subtle.ConstantTimeByteEq assembled into a byte loop (security's bypass)", `package p
import "crypto/subtle"
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	for i := range got {
		if subtle.ConstantTimeByteEq(got[i], stored[i]) == 0 {
			return false
		}
	}
	return true
}`, true)

	// The same laundering as the digestsEqual self-test above, moved into a
	// package-level var. A GenDecl used to be scanned with a permanently-empty
	// taint map, so the func literal's digest-shaped parameters were never
	// tainted.
	assertDetects(t, "digest equality inside a package-level func literal (security's bypass)", `package p
var digestsEqual = func(a, b [DigestSize]byte) bool { return a == b }`, true)

	assertDetects(t, "the approved constant-time compare", `package p
import "crypto/subtle"
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	return subtle.ConstantTimeCompare(got[:], stored[:]) == 1
}`, false)
	// crypto/hmac.Equal is the one other approved spelling, and it must not be
	// flagged — nor must an hmac used for its actual purpose.
	assertDetects(t, "the approved hmac.Equal", `package p
import "crypto/hmac"
func f(presented string, stored [32]byte) bool {
	got := HashSecret(presented)
	return hmac.Equal(got[:], stored[:])
}`, false)
	assertDetects(t, "an unrelated comparison", `package p
func f(cur Record, want string) bool {
	return cur.BusID == want && len(cur.Label) == 0
}`, false)
	// The switch arm must not fire on an ordinary state switch, or every
	// enum-dispatch in this package becomes a violation.
	assertDetects(t, "an unrelated switch", `package p
func f(r Record) string {
	switch r.State {
	case StateOpen:
		return "open"
	}
	return ""
}`, false)

	used := map[string]bool{}
	for _, fd := range newAnalysis(t, fset, files).findings() {
		key := fd.file + ": " + fd.expr
		if _, ok := allowedComparisons[key]; ok {
			used[key] = true
			continue
		}
		t.Errorf("%s: %s\n\t%s\n\tAn invite secret, its digest, and the stored redemption key and fingerprint are BEARER-CREDENTIAL material and must be compared ONLY with crypto/subtle.ConstantTimeCompare. "+
			"A ==, a switch/case, bytes.Equal, reflect.DeepEqual or strings.EqualFold returns on the first differing byte, which lets a caller recover the value one byte at a time by timing — while admitting the right holder and refusing the wrong one exactly as before, so no functional test notices. "+
			"If this comparison genuinely touches no credential, add it to allowedComparisons WITH its one-line reason and take it past the security gate.",
			fset.Position(fd.pos), fd.what, fd.expr)
	}

	// A stale allowlist entry is a hole nobody is looking at any more, and it is
	// also this guard's vacuity check: these sites firing proves the taint
	// analysis really does classify real expressions in this package, not just
	// the synthetic ones above.
	for key, reason := range allowedComparisons {
		if !used[key] {
			t.Errorf("allowedComparisons entry %q (%s) matched nothing. Either the expression moved or changed shape — in which case re-review it and update the key — or the exemption is dead and must be deleted. An allowlist pointing at code that no longer exists is how a guard quietly stops guarding.", key, reason)
		}
	}
}

// assertDetects runs the guard-2 detector over a synthetic source file and
// asserts whether it fires, so that the detector is known to be capable of both
// verdicts.
func assertDetects(t *testing.T, name, src string, want bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("self-test %q: parsing synthetic source: %v", name, err)
	}
	got := len(newAnalysis(t, fset, map[string]*ast.File{"synthetic.go": f}).findings()) > 0
	if got != want {
		t.Fatalf("self-test %q: detector fired = %v, want %v. The detector is not doing what this guard claims; a detector that cannot fire gives a permanent false green.", name, got, want)
	}
}

// guardBeginChecksSecretFirst is guard 3, and it is LOAD-BEARING FOR A SECURITY
// VERDICT rather than merely tidy.
//
// Store.Begin returns five errors that describe the invite's INTERNAL STATE:
// ErrRedemptionInFlight (another redemption holds it right now), ErrKeyReuse
// (already redeemed under this key with a different payload), ErrExpired,
// ErrRevoked and ErrAlreadyRedeemed. Every one of them is a rich signal to
// someone enumerating invite ids — "this id exists, and here is what happened to
// it".
//
// The reason the security review can say they DISCLOSE NOTHING is ordering, and
// only ordering: all five are raised strictly AFTER the VerifySecret gate, so a
// caller who has not proved it holds the secret can never reach any of them — it
// gets the same ErrUnknownInvite as for an id that does not exist, compared
// against a per-store dummy digest so even the work performed matches.
//
// EARLIER THIS GUARD PINNED ONLY TWO OF THE FIVE, which meant hoisting the
// `cur.Expired(now)` / ErrExpired check above the gate passed GREEN — a mutant
// that hands any anonymous caller a live/expired oracle over every invite id.
// That is why the list below is the FULL set of post-gate sentinels, and why a
// new one must be added here in the same change that introduces it.
//
// ErrUnknownInvite is deliberately NOT in the list: it is the gate's own answer
// and the pre-gate validation's, so it is expected before the gate.
//
// Move any of these returns above the VerifySecret gate and that verdict becomes
// false SILENTLY. Every existing test still passes: the same errors are still
// returned to the same callers for the same reasons. What changes is that a
// non-holder can now distinguish "in flight", "expired", "revoked", "already
// redeemed" and "no such invite" — an enumeration oracle over the bus's
// admission tickets, introduced by moving an if block.
//
// Asserted by AST position, never by line number: line numbers drift with every
// edit above them, and a guard that fails on unrelated edits gets deleted.
func guardBeginChecksSecretFirst(t *testing.T, fset *token.FileSet, files map[string]*ast.File) {
	t.Helper()

	var begin *ast.FuncDecl
	var beginFile string
	found := 0
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "Begin" || fd.Recv == nil || fd.Body == nil {
				continue
			}
			if !receiverIs(fd, "Store") {
				continue
			}
			found++
			begin, beginFile = fd, name
		}
	}
	if found != 1 {
		t.Fatalf("found %d methods named Begin on *Store; expected exactly 1. If the redemption entry point was renamed or split, this ordering guard must move with it.", found)
	}

	gate, gateOK := locateInBody(begin, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		_, name := funcName(call.Fun)
		return name == "VerifySecret"
	})
	if !gateOK {
		t.Fatalf("%s: Store.Begin no longer calls VerifySecret. Without that gate every state-disclosing error below it is reachable by a caller who holds no secret — which is an enumeration oracle over invite ids, not a refactor.", beginFile)
	}

	// EVERY invite-state sentinel Store.Begin can return. Adding a sixth without
	// adding it here is how this guard silently stops covering it.
	for _, sentinel := range []string{
		"ErrRedemptionInFlight",
		"ErrKeyReuse",
		"ErrExpired",
		"ErrRevoked",
		"ErrAlreadyRedeemed",
	} {
		name := sentinel
		site, ok := locateInBody(begin, func(n ast.Node) bool {
			id, isID := n.(*ast.Ident)
			return isID && id.Name == name
		})
		if !ok {
			t.Errorf("%s: Store.Begin no longer mentions %s. If that outcome moved elsewhere, its ordering relative to the VerifySecret gate must be re-established there — this guard can no longer see it.", beginFile, name)
			continue
		}
		if site.before(gate) {
			t.Errorf("%s: %s is returned at %s, BEFORE the VerifySecret gate at %s (statement %d vs %d in Begin's body).\n"+
				"\tThat ordering is the entire reason this error is considered to disclose nothing: raised only after the secret is proved, it is visible solely to the invite's holder, who already knows its state. "+
				"Raised before, it tells any caller — with no credential at all — whether an invite id exists and what state it is in, which is an enumeration oracle over the bus's admission tickets. "+
				"Nothing else fails when it moves: the same errors go to the same callers for the same reasons, and every functional test stays green. Put the VerifySecret gate first.",
				beginFile, name, fset.Position(site.pos), fset.Position(gate.pos), site.stmt, gate.stmt)
		}
	}
}

// site is a position inside a function body, recorded as both the index of the
// enclosing top-level statement and the exact token position.
type site struct {
	stmt int
	pos  token.Pos
}

// before orders two sites by enclosing statement first, then by position, so
// that a move between statements and a move within one are both detected.
func (s site) before(other site) bool {
	if s.stmt != other.stmt {
		return s.stmt < other.stmt
	}
	return s.pos < other.pos
}

// locateInBody finds the first node matching pred and reports which top-level
// statement of the function body contains it.
func locateInBody(fd *ast.FuncDecl, pred func(ast.Node) bool) (site, bool) {
	for i, stmt := range fd.Body.List {
		var hit token.Pos
		ast.Inspect(stmt, func(n ast.Node) bool {
			if hit.IsValid() || n == nil {
				return false
			}
			if pred(n) {
				hit = n.Pos()
				return false
			}
			return true
		})
		if hit.IsValid() {
			return site{stmt: i, pos: hit}, true
		}
	}
	return site{}, false
}

// receiverIs reports whether fd is a method on the named type, by value or by
// pointer.
func receiverIs(fd *ast.FuncDecl, typeName string) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	expr := fd.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == typeName
}
