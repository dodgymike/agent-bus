package ids

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAtomicWriteFile pins the properties both durable identity files in this
// package now depend on through ONE implementation: the bus's own id
// (writeBusIDFile) and the per-name agent-id suffix floors
// (DurableNameSuffixes.writeFloors, which persists a floor AHEAD of the suffix
// it authorises).
//
// It exists because unifying the two copies moved a durability-critical
// sequence from "duplicated and separately covered by each caller's tests" to
// "shared", and a shared helper on the path of invariant 1 earns its own test
// rather than being covered only incidentally.
func TestAtomicWriteFile(t *testing.T) {
	t.Run("CreatesPrivateWithExactBytes", func(t *testing.T) {
		cases := []struct {
			name string
			data []byte
		}{
			{name: "empty", data: []byte{}},
			{name: "one line", data: []byte("bus-abc123\n")},
			{name: "no trailing newline", data: []byte("bus-abc123")},
			{name: "multi line", data: []byte("alpha 4\nbeta 17\n")},
			{name: "embedded NUL and high bytes", data: []byte{0x00, 0xff, '\n', 0x7f}},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "f")
				if err := atomicWriteFile(dir, path, ".f-*", tc.data); err != nil {
					t.Fatalf("atomicWriteFile: %v", err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading back: %v", err)
				}
				if string(got) != string(tc.data) {
					t.Fatalf("content = %q, want %q", got, tc.data)
				}
				fi, err := os.Stat(path)
				if err != nil {
					t.Fatalf("Stat: %v", err)
				}
				// 0600 matters because both callers hold identity state; a
				// world-readable bus id or floors file leaks the roster's shape.
				if perm := fi.Mode().Perm(); perm != 0o600 {
					t.Fatalf("mode = %#o, want 0600", perm)
				}
				assertNoTempResidue(t, dir, "f")
			})
		}
	})

	t.Run("ReplacesExistingContentAndResetsMode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		// A pre-existing file that is BOTH longer than the replacement and
		// world-readable. The length matters: a writer that truncated in place
		// instead of renaming would leave a tail behind, and a shorter floors
		// file that still carries an old tail reads as a DIFFERENT floor.
		if err := os.WriteFile(path, []byte("alpha 999999\nbeta 888888\n"), 0o644); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		want := "alpha 1\n"
		if err := atomicWriteFile(dir, path, ".f-*", []byte(want)); err != nil {
			t.Fatalf("atomicWriteFile: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading back: %v", err)
		}
		if string(got) != want {
			t.Fatalf("content = %q, want %q (no tail of the previous, longer file)", got, want)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("mode after replacing a 0644 file = %#o, want 0600", perm)
		}
		assertNoTempResidue(t, dir, "f")
	})

	t.Run("FailureLeavesTheOriginalAndNoResidue", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: directory permissions do not block writes")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		const original = "alpha 7\n"
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		defer os.Chmod(dir, 0o700)

		err := atomicWriteFile(dir, path, ".f-*", []byte("alpha 8\n"))
		if err == nil {
			t.Fatal("atomicWriteFile on an unwritable dir returned nil, want an error")
		}
		// The error must name the directory: an operator reading a startup
		// failure needs to know WHICH data dir refused.
		if !strings.Contains(err.Error(), dir) {
			t.Fatalf("error %v does not name the directory %s", err, dir)
		}
		got, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading back after a failed write: %v", rerr)
		}
		if string(got) != original {
			t.Fatalf("content after a FAILED write = %q, want the original %q; a failed write must never be half-applied", got, original)
		}
	})

	t.Run("TempFileIsCreatedInTheTargetDirectory", func(t *testing.T) {
		// The rename is only atomic when the temp file shares a filesystem with
		// the target, which is why the pattern goes to os.CreateTemp(dir, …) and
		// never to the system temp dir. Proved by giving the write a target in a
		// dir it cannot create in: if the helper had used the system temp dir the
		// create would have succeeded.
		if os.Geteuid() == 0 {
			t.Skip("running as root: directory permissions do not block writes")
		}
		dir := t.TempDir()
		sub := filepath.Join(dir, "ro")
		if err := os.Mkdir(sub, 0o500); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		defer os.Chmod(sub, 0o700)
		err := atomicWriteFile(sub, filepath.Join(sub, "f"), ".f-*", []byte("x"))
		if err == nil {
			t.Fatal("atomicWriteFile succeeded against an unwritable target dir; the temp file was not created in it")
		}
		if !strings.Contains(err.Error(), "creating temp file") {
			t.Fatalf("error = %v, want it to come from the temp-file creation step", err)
		}
	})

	t.Run("BothProductionCallersProduceAPrivateUntornFile", func(t *testing.T) {
		// End-to-end, through each caller's real entry point rather than the
		// helper: whatever they do, the file lands 0600 with no temp residue.
		t.Run("writeBusIDFile", func(t *testing.T) {
			dir := t.TempDir()
			id, err := LoadOrCreateBusID(dir, "")
			if err != nil {
				t.Fatalf("LoadOrCreateBusID: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "bus-id"))
			if err != nil {
				t.Fatalf("reading bus-id: %v", err)
			}
			if string(data) != id+"\n" {
				t.Fatalf("bus-id file = %q, want %q; the trailing newline is writeBusIDFile's content contract", data, id+"\n")
			}
			assertNoTempResidue(t, dir, "bus-id")
		})

		t.Run("DurableNameSuffixes", func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenNameSuffixes(dir)
			if err != nil {
				t.Fatalf("OpenNameSuffixes: %v", err)
			}
			if err := store.Seal(); err != nil {
				t.Fatalf("Seal: %v", err)
			}
			for i := 0; i < 3; i++ {
				if _, err := store.NextSuffix("alpha"); err != nil {
					t.Fatalf("NextSuffix #%d: %v", i, err)
				}
			}
			fi, err := os.Stat(store.Path())
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Fatalf("agent-suffixes mode = %#o, want 0600", perm)
			}
			assertNoTempResidue(t, dir, suffixFileName)
		})
	})

	// The unification is only worth anything while it HOLDS. This counts the
	// atomic-replace primitives across the package's production files so a
	// re-duplicated copy goes red rather than rotting quietly in one place.
	t.Run("TheSequenceExistsExactlyOnce", func(t *testing.T) {
		counts := countOSCallsInPackageIDs(t, map[string]bool{
			"CreateTemp": true,
			"Rename":     true,
		})
		for _, fn := range []string{"CreateTemp", "Rename"} {
			if got := len(counts[fn]); got != 1 {
				t.Errorf("os.%s is called %d time(s) in package ids production files (%v), want exactly 1 (in atomicfile.go). Two copies of the temp+write+fsync+rename+fsync-dir sequence is the duplication this package deliberately removed: every step is a durability property that fails silently, so a copy can lose one while all tests stay green. If the new call is an atomic replace, route it through atomicWriteFile; if it is genuinely something else (an archival rename, say), this guard is the right place to say so deliberately", fn, got, counts[fn])
				continue
			}
			if base := filepath.Base(strings.SplitN(counts[fn][0], ":", 2)[0]); base != "atomicfile.go" {
				t.Errorf("os.%s is called in %s (at %s), want it only in atomicfile.go", fn, base, counts[fn][0])
			}
		}
	})

	// Counting the replace primitives guards against RE-duplication. It does not
	// guard against DEGRADATION of the one remaining copy, which is the same
	// silent-failure class now concentrated in a single file: deleting either
	// fsync leaves every test in this package green, because no unit test can
	// observe a missing fsync without syscall-level crash injection. So the two
	// Sync calls are counted too. (Raised as LOW-2 by the security gate on the
	// unification task.)
	t.Run("BothFsyncsArePresent", func(t *testing.T) {
		src, err := os.ReadFile("atomicfile.go")
		if err != nil {
			t.Fatalf("reading atomicfile.go: %v", err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "atomicfile.go", src, 0)
		if err != nil {
			t.Fatalf("parsing atomicfile.go: %v", err)
		}
		// Positions, not just a count. The roles are what the durability
		// argument rests on, so the check has to cover them: counting to two
		// would pass on two file-fsyncs and no directory fsync, and the failure
		// text would then be asserting something the check never looked at.
		var syncPos []token.Pos
		var renamePos []token.Pos
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "Sync":
				syncPos = append(syncPos, call.Pos())
			case sel.Sel.Name == "Rename":
				if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == "os" {
					renamePos = append(renamePos, call.Pos())
				}
			}
			return true
		})
		// Exactly two: the FILE before the rename (without it the rename can be
		// durable while the bytes are not, so a crash yields a short file that
		// reads as a LOWER floor) and the DIRECTORY after it (without it the
		// rename itself can be lost, reverting the file to its previous, lower
		// content). Either omission is agent-id reuse after a crash, and the
		// agent id is the routing and authorization subject.
		if len(syncPos) != 2 {
			t.Fatalf("atomicfile.go makes %d Sync call(s), want exactly 2: one fsync of the FILE before the rename and one of the DIRECTORY after it. Dropping either is undetectable by every test in this package and turns a crash into a floor that reads back LOWER than what was persisted, which is agent-id reuse (invariant 1)", len(syncPos))
		}
		if len(renamePos) != 1 {
			t.Fatalf("atomicfile.go makes %d os.Rename call(s), want exactly 1", len(renamePos))
		}
		// The ordering IS the property. A file fsync after the rename does not
		// make the rename durable, and a directory fsync before it has nothing
		// to make durable yet.
		if !(syncPos[0] < renamePos[0] && renamePos[0] < syncPos[1]) {
			t.Fatalf("atomicfile.go's fsync/rename order is wrong: Sync at %s, os.Rename at %s, Sync at %s. Required order is fsync(FILE) -> os.Rename -> fsync(DIR): an fsync of the file AFTER the rename does not make the rename durable, and an fsync of the directory BEFORE it has nothing to make durable yet",
				fset.Position(syncPos[0]), fset.Position(renamePos[0]), fset.Position(syncPos[1]))
		}
	})
}

// assertNoTempResidue fails if dir holds anything other than the named files. A
// leftover ".bus-id-123456" or ".agent-suffixes-123456" means an error path
// forgot to clean up, which over a long-lived data dir is an unbounded leak of
// inodes in the directory the bus must fsync on every write.
func assertNoTempResidue(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, name := range allowed {
		ok[name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if !ok[e.Name()] {
			t.Errorf("%s left behind in %s; the temp file must be renamed away on success and removed on every error path", e.Name(), dir)
		}
	}
}

// countOSCallsInPackageIDs returns, per requested os function name, the
// positions at which it is called in this package's NON-TEST files. It parses
// rather than greps so the prose in this package that names the sequence — and
// there is a lot of it — cannot trip the count.
func countOSCallsInPackageIDs(t *testing.T, want map[string]bool) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		parsed++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" || !want[sel.Sel.Name] {
				return true
			}
			out[sel.Sel.Name] = append(out[sel.Sel.Name], fset.Position(call.Pos()).String())
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("no non-test .go files were parsed; this count proved nothing")
	}
	return out
}
