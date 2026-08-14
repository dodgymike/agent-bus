package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// emptyEnv is a lookupEnv that never finds anything, mirroring an agent
// shelling out with no relevant environment set. Shared by every CLI test in
// this package.
func emptyEnv(string) (string, bool) { return "", false }

// TestCLIHelpExitsZeroAndMentionsEnrol checks --help is a success (exit 0)
// and that the root help text names every command, in particular enrol.
func TestCLIHelpExitsZeroAndMentionsEnrol(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitOK {
		t.Fatalf("run(--help) = %d, want %d; stderr=%q", code, client.ExitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "enrol") {
		t.Fatalf("--help output does not mention enrol: %q", stdout.String())
	}
}

// TestCLIUnknownCommandJSON checks an unrecognised subcommand fails with
// ExitUsage and, under --json, renders a parseable error object on stdout.
func TestCLIUnknownCommandJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--json", "bogus"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(--json bogus) = %d, want %d; stdout=%q stderr=%q", code, client.ExitUsage, stdout.String(), stderr.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("stdout not parseable JSON: %v (%q)", err, stdout.String())
	}
	if ok, _ := parsed["ok"].(bool); ok {
		t.Fatalf("parsed[\"ok\"] = %v, want false", parsed["ok"])
	}
}

// TestCLIGlobalFlagsBeforeAndAfterSubcommand checks --json (and --identity)
// are honoured whether they appear before or after the subcommand name.
func TestCLIGlobalFlagsBeforeAndAfterSubcommand(t *testing.T) {
	dir := t.TempDir()
	cases := [][]string{
		{"--json", "--identity", dir, "whoami"},
		{"--identity", dir, "whoami", "--json"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr, emptyEnv)
			if code != client.ExitConfig {
				t.Fatalf("run(%v) = %d, want %d (empty store); stdout=%q stderr=%q", args, code, client.ExitConfig, stdout.String(), stderr.String())
			}
			var parsed map[string]interface{}
			if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
				t.Fatalf("run(%v): stdout not parseable JSON, so --json was not honoured in this flag position: %v (%q)", args, err, stdout.String())
			}
		})
	}
}

// TestCLIWhoamiEmptyStoreExitsConfig checks whoami with nothing enrolled
// yields ExitConfig (3).
func TestCLIWhoamiEmptyStoreExitsConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "whoami"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitConfig {
		t.Fatalf("run(whoami) on an empty store = %d, want %d; stderr=%q", code, client.ExitConfig, stderr.String())
	}
}

// TestCLIWhoamiAllEmptyStoreExitsEmpty checks whoami --all with nothing
// enrolled yields ExitEmpty (8), the "nothing to report" code.
func TestCLIWhoamiAllEmptyStoreExitsEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "whoami", "--all"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitEmpty {
		t.Fatalf("run(whoami --all) on an empty store = %d, want %d; stderr=%q", code, client.ExitEmpty, stderr.String())
	}
}

// TestCLIUseNoArgExitsUsage checks `use` with no positional argument is
// ExitUsage (2), not a silent no-op or a panic.
func TestCLIUseNoArgExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "use"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(use) with no argument = %d, want %d; stderr=%q", code, client.ExitUsage, stderr.String())
	}
}

// TestCLILogoutAllWithPositionalExitsUsage checks `logout --all <name>` is
// rejected (ExitUsage) rather than silently ignoring one of the two.
func TestCLILogoutAllWithPositionalExitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--identity", t.TempDir(), "logout", "--all", "someone"}, &stdout, &stderr, emptyEnv)
	if code != client.ExitUsage {
		t.Fatalf("run(logout --all someone) = %d, want %d; stderr=%q", code, client.ExitUsage, stderr.String())
	}
}

// exitCodeEntry is one row of a help text's EXIT CODES table: the number, and
// everything the table says about it with any continuation lines folded in.
type exitCodeEntry struct {
	Code int
	Text string
}

// parseExitCodeTable extracts the EXIT CODES block from a help text.
//
// # The parse rule is written down HERE because the ambiguity IS the defect
//
// These tables are laid out in two columns. That layout is readable and it is
// also how `agent-busctl watch` came to document a fatal 503 under exit 5 while the
// code returned 6: with codes 1 and 5 on the same physical line, a continuation
// line indented under the RIGHT column reads, to a human skimming the file, as
// belonging to whichever number their eye last landed on. So the rule a machine
// applies is spelled out rather than implied:
//
//  1. The block is the run of consecutive NON-BLANK lines immediately after a
//     line whose trimmed text is "EXIT CODES". A blank line ends it. That
//     matters for `agent-busctl agents`, which follows its table with an indented
//     prose paragraph beginning "8 is rare in practice…"; folding that in would
//     invent a second entry for 8.
//  2. The right column begins at ONE offset for the whole block, DISCOVERED
//     rather than assumed: the smallest index past the left column at which
//     some line carries a run of digits preceded by two or more spaces and
//     followed by a space. The "followed by a space" clause is what keeps
//     "409: this key was used…" from being mistaken for a column of its own.
//  3. A line is cut into a left cell and a right cell at that offset ONLY IF
//     the two characters immediately before it are both spaces. Otherwise the
//     whole line is one wide left cell — which is what `agent-busctl send`'s "2 bad
//     usage: no recipient, no body, two body sources, body too large" is, and
//     slicing it at the column would have manufactured a nonsense continuation
//     for whatever entry sat above it on the right.
//  4. Within a column, read top to bottom: a cell beginning with digits then a
//     space starts a NEW entry; any other non-empty cell is a CONTINUATION of
//     the previous entry IN THAT COLUMN.
//
// It fails the test rather than returning an empty slice when a block yields no
// entries, so a help text that is reformatted out of recognition can never make
// a caller pass vacuously.
func parseExitCodeTable(t *testing.T, name, help string) []exitCodeEntry {
	t.Helper()

	lines := strings.Split(help, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "EXIT CODES" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: its help text has no EXIT CODES block; every subcommand documents one, and a missing block makes this check vacuous", name)
	}
	var block []string
	for i := start; i < len(lines) && strings.TrimSpace(lines[i]) != ""; i++ {
		block = append(block, lines[i])
	}
	if len(block) == 0 {
		t.Fatalf("%s: its EXIT CODES block is empty", name)
	}

	splitCol := rightColumnStart(block)

	var leftCells, rightCells []string
	for _, line := range block {
		l, r := splitTwoColumn(line, splitCol)
		leftCells = append(leftCells, l)
		rightCells = append(rightCells, r)
	}

	entries := append(parseExitCodeColumn(t, name, "left", leftCells), parseExitCodeColumn(t, name, "right", rightCells)...)
	if len(entries) == 0 {
		t.Fatalf("%s: its EXIT CODES block parsed to zero entries; the layout changed and this check would pass vacuously.\nblock:\n%s", name, strings.Join(block, "\n"))
	}
	return entries
}

// rightColumnStart discovers the offset at which the right column begins, or 0
// when the block is a single column. See parseExitCodeTable rule 2.
func rightColumnStart(block []string) int {
	best := 0
	for _, line := range block {
		// i starts at 3: the LEFT column's own digit sits at index 2 behind the
		// two-space indent, and it is not a column boundary.
		for i := 3; i < len(line); i++ {
			if !isASCIIDigit(line[i]) || line[i-1] != ' ' || line[i-2] != ' ' {
				continue
			}
			j := i
			for j < len(line) && isASCIIDigit(line[j]) {
				j++
			}
			if j >= len(line) || line[j] != ' ' {
				continue
			}
			if best == 0 || i < best {
				best = i
			}
			break
		}
	}
	return best
}

// splitTwoColumn applies rule 3.
func splitTwoColumn(line string, col int) (left, right string) {
	if col < 2 || len(line) <= col || line[col-1] != ' ' || line[col-2] != ' ' {
		return line, ""
	}
	return line[:col], line[col:]
}

// parseExitCodeColumn applies rule 4 to one column's cells, in line order.
func parseExitCodeColumn(t *testing.T, name, side string, cells []string) []exitCodeEntry {
	t.Helper()

	var entries []exitCodeEntry
	for i, cell := range cells {
		text := strings.TrimSpace(cell)
		if text == "" {
			continue
		}
		code, rest, ok := leadingExitCode(text)
		if !ok {
			if len(entries) == 0 {
				t.Fatalf("%s: the %s column of its EXIT CODES block continues an entry that was never started, on line %d: %q",
					name, side, i+1, text)
			}
			last := &entries[len(entries)-1]
			last.Text = strings.TrimSpace(last.Text + " " + text)
			continue
		}
		entries = append(entries, exitCodeEntry{Code: code, Text: rest})
	}
	return entries
}

// leadingExitCode splits "6 the bus reported…" into 6 and the description.
func leadingExitCode(text string) (int, string, bool) {
	i := 0
	for i < len(text) && isASCIIDigit(text[i]) {
		i++
	}
	if i == 0 || i >= len(text) || text[i] != ' ' {
		return 0, "", false
	}
	code, err := strconv.Atoi(text[:i])
	if err != nil {
		return 0, "", false
	}
	return code, strings.TrimSpace(text[i:]), true
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// validExitCodes is the CLOSED SET of exit codes a help table may document: the
// client.Exit* constants, named, so a failure can say which one a number is.
//
// It is a function rather than a literal inside the table test because a second
// test checks the set itself against client/errors.go. Written out by hand and
// not derived, deliberately: the compiler then checks each entry exists, and
// TestExitCodeClosedSetCoversEveryClientExitConstant checks nothing is missing.
// Deriving it from the source would make this test agree with any future
// constant by construction, which is exactly the drift it exists to catch.
//
// client.ExitVersionSkew was MISSING here until INVITE-CLIENT-FU-EXIT9, and the
// omission was not harmless: exit 9 is reachable (a 404 on a fixed route means
// the bus is older than this client), so every help table that documented it
// went red, and the tables therefore documented an incomplete contract that an
// agent branching on the exit code could actually receive.
func validExitCodes() map[int]string {
	return map[int]string{
		client.ExitOK:          "client.ExitOK",
		client.ExitError:       "client.ExitError",
		client.ExitUsage:       "client.ExitUsage",
		client.ExitConfig:      "client.ExitConfig",
		client.ExitAuth:        "client.ExitAuth",
		client.ExitNetwork:     "client.ExitNetwork",
		client.ExitServer:      "client.ExitServer",
		client.ExitRejected:    "client.ExitRejected",
		client.ExitEmpty:       "client.ExitEmpty",
		client.ExitVersionSkew: "client.ExitVersionSkew",
	}
}

// versionSkewCommands are the subcommands that can actually EXIT 9, and
// therefore must document it.
//
// It is a hand-written list because reachability is a judgement about which
// routes a command calls, and there is nothing in the help text to derive it
// from. Each entry is checked against client/transport.go's ONE assignment of
// KindVersionSkew — a 404 on any fixed route EXCEPT routeSend:
//
//	enrol      POST /v1/enroll
//	agents     GET  /v1/agents
//	watch      POST /v1/wait, GET /v1/messages
//	send       POST /v1/mint — the id reservation it signs. NOT /v1/send,
//	           whose 404 is a per-resource "unknown recipient" and is
//	           deliberately carved out to KindRejected.
//	broadcast  POST /v1/broadcast
//
// Deliberately ABSENT: whoami, whose only remote calls are the two session
// routes, where client/session.go's annotateSessionError overrides a 404 to
// KindAuth ("the bus does not know this agent"); and use/logout/pin/client-cert,
// which make no HTTP call at all.
var versionSkewCommands = []string{"enrol", "agents", "watch", "send", "broadcast"}

// TestEveryVersionSkewCommandDocumentsExitNine is the OTHER direction of the
// help-table check, and the reviewer gate proved it was missing: deleting the
// exit-9 row from a single subcommand left both other guards GREEN.
//
// TestHelpExitCodeTablesAgreeWithClientExitCodes only checks that a documented
// code is real and means what the row says. It cannot notice a code that is
// reachable and documented NOWHERE — which is exactly the state
// INVITE-CLIENT-FU-EXIT9 was filed about, and exactly the state this repo would
// silently return to the first time someone tidied a table.
func TestEveryVersionSkewCommandDocumentsExitNine(t *testing.T) {
	byName := map[string]command{}
	for _, c := range commands() {
		byName[c.name] = c
	}
	for _, name := range versionSkewCommands {
		c, ok := byName[name]
		if !ok {
			t.Errorf("versionSkewCommands names %q, which is not a subcommand; the list is stale and the check below is weaker than it looks", name)
			continue
		}
		documented := false
		for _, e := range parseExitCodeTable(t, c.name, c.help) {
			if e.Code == client.ExitVersionSkew {
				documented = true
				break
			}
		}
		if !documented {
			t.Errorf("%s can exit %d (a 404 on a fixed route it calls: the bus is older than this client) but its EXIT CODES table does not document it.\n"+
				"An undocumented exit code an agent can actually receive is a broken contract (invariant 7).", name, client.ExitVersionSkew)
		}
	}
}

// TestExitCodeClosedSetCoversEveryClientExitConstant is what keeps the closed
// set CLOSED.
//
// Growing the set by hand fixes today's gap and does nothing about the next
// one: a constant added to client/errors.go tomorrow would be just as reachable
// and just as undocumentable, and the table test would keep passing while
// rejecting the row that documents it. That is the exact shape of the bug this
// task fixed, so the fix must not be a one-off edit.
//
// It reads the SOURCE rather than using reflection because Go constants are not
// reflectable at run time: there is no way to enumerate a package's untyped int
// constants from a test binary. Parsing is the only mechanism that sees a
// constant nobody has referenced yet — which is precisely the one that would
// otherwise slip through.
func TestExitCodeClosedSetCoversEveryClientExitConstant(t *testing.T) {
	source := clientExitConstants(t)
	set := validExitCodes()

	for code, name := range source {
		got, ok := set[code]
		if !ok {
			t.Errorf("client/errors.go defines %s = %d, which validExitCodes() omits.\n"+
				"An exit code an agent can RECEIVE must be documentable: add it to the closed set AND to the EXIT CODES table of every subcommand that can produce it. "+
				"Leaving it out does not hide the code — it only makes the row that documents it fail (INVITE-CLIENT-FU-EXIT9).", name, code)
			continue
		}
		if got != name {
			t.Errorf("exit %d is %s in validExitCodes() but %s in client/errors.go", code, got, name)
		}
	}
	for code, name := range set {
		if _, ok := source[code]; !ok {
			t.Errorf("validExitCodes() carries %s = %d, which client/errors.go no longer defines; a retired value must never be reused, so this set must not keep claiming it", name, code)
		}
	}
}

// clientExitConstants parses the client package and returns every exported
// Exit* constant, by value.
//
// It parses the whole package directory rather than errors.go alone so that a
// constant MOVED to another file is still seen — a guard that can be defeated
// by moving a declaration is not a guard.
func clientExitConstants(t *testing.T) map[int]string {
	t.Helper()

	const dir = "../../client"
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}

	out := map[int]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, ident := range vs.Names {
						if !strings.HasPrefix(ident.Name, "Exit") || !ident.IsExported() {
							continue
						}
						if i >= len(vs.Values) {
							t.Fatalf("client.%s has no literal value (an iota form?); this guard reads literals, and a form it cannot read must not be allowed to pass silently", ident.Name)
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.INT {
							t.Fatalf("client.%s is not an integer literal; this guard cannot verify it and must not pass it silently", ident.Name)
						}
						code, cerr := strconv.Atoi(lit.Value)
						if cerr != nil {
							t.Fatalf("client.%s = %q is not an integer: %v", ident.Name, lit.Value, cerr)
						}
						if prev, dup := out[code]; dup {
							t.Errorf("client.%s and %s share exit code %d; an exit code is a contract and two meanings for one number is not one", ident.Name, prev, code)
						}
						out[code] = "client." + ident.Name
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no client.Exit* constants were found in %s; the parse found nothing and every check below would be vacuous", dir)
	}
	return out
}

// TestHelpExitCodeTablesAgreeWithClientExitCodes checks every subcommand's
// documented exit-code table against the CLOSED set of codes client/errors.go
// defines and against what each of those codes MEANS.
//
// This exists because a help text is a contract an agent reads and branches on,
// and it is the one part of the contract no compiler checks. `agent-busctl watch`
// documented a fatal 503 under exit 5 while client.ExitCode returned 6 for it;
// nothing failed, because prose cannot be wrong at build time.
//
// The check is deliberately phrased in the direction that catches drift: a
// DESCRIPTION implies a code. "unreachable" means the bus could not be reached,
// which is ExitNetwork and nothing else; "503" is the bus reporting on itself,
// which is ExitServer. Where a description says one thing and sits under a
// different number, this fails and names both.
func TestHelpExitCodeTablesAgreeWithClientExitCodes(t *testing.T) {
	valid := validExitCodes()

	// Canonical meanings: a lower-cased phrase, and the ONE code it can belong
	// to. A description matching no phrase is not an error — the wording of "0
	// switched" carries no claim to check — but every phrase here is a claim
	// about which failure this is, and it must sit under the matching number.
	canonical := []struct {
		phrase string
		code   int
	}{
		{"internal error", client.ExitError},
		{"bad usage", client.ExitUsage},
		{"no usable identity", client.ExitConfig},
		{"no such identity", client.ExitConfig},
		{"no identity enrolled", client.ExitConfig},
		{"credential store is unusable", client.ExitConfig},
		{"credential rejected", client.ExitAuth},
		{"rejected the credential", client.ExitAuth},
		{"unreachable", client.ExitNetwork},
		{"could not be reached", client.ExitNetwork},
		{"reported an error of its own", client.ExitServer},
		{"reported a failure of its own", client.ExitServer},
		// The 503 split (client/transport.go): a 503 with no Retry-After is the
		// bus saying its write path is not durable. It is KindServer, and
		// KindServer is ExitServer — it is NOT "the bus could not be reached".
		{"503", client.ExitServer},
		{"refused", client.ExitRejected},
		{"nothing to report", client.ExitEmpty},
		{"delivered nothing", client.ExitEmpty},
		{"empty", client.ExitEmpty},
		// A 404 on a fixed route this client depends on: the bus never
		// understood the request because it does not know the route exists.
		// Deliberately NOT "refused", which is ExitRejected and is the opposite
		// claim — the bus understood and said no (client/transport.go's 404
		// split, and the routeSend carve-out inside it).
		{"no route for", client.ExitVersionSkew},
		{"older than this client", client.ExitVersionSkew},
	}

	// The ROOT usage text is checked too, and it is not in commands() — which
	// is why nothing checked it: the security gate put a nonexistent exit code
	// 17 in root.go's table and both guards stayed green. It documents the same
	// contract for the same readers, so it is held to the same rule.
	var rootHelp strings.Builder
	writeRootHelp(&rootHelp)
	cmds := append(commands(), command{name: "(root usage)", help: rootHelp.String()})
	if len(cmds) == 0 {
		t.Fatalf("commands() returned nothing; there is no help text to check")
	}
	for _, c := range cmds {
		c := c
		t.Run(c.name, func(t *testing.T) {
			entries := parseExitCodeTable(t, c.name, c.help)
			seen := make(map[int]string, len(entries))
			for _, e := range entries {
				if _, ok := valid[e.Code]; !ok {
					t.Errorf("%s documents exit code %d (%q), which is not one of the client.Exit* constants", c.name, e.Code, e.Text)
					continue
				}
				if prev, dup := seen[e.Code]; dup {
					t.Errorf("%s documents exit code %d twice: %q and %q — a reader cannot tell which one is the contract", c.name, e.Code, prev, e.Text)
				}
				seen[e.Code] = e.Text

				lower := strings.ToLower(e.Text)
				for _, m := range canonical {
					if !strings.Contains(lower, m.phrase) {
						continue
					}
					if e.Code != m.code {
						t.Errorf("%s documents %q under exit %d, but %q means %s (%d).\n"+
							"The help text and client.ExitCode disagree, and the help text is what an agent branches on.",
							c.name, e.Text, e.Code, m.phrase, valid[m.code], m.code)
					}
				}
			}
		})
	}
}
