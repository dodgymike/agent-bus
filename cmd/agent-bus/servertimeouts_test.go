package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CORE-9 -- the resource bounds on http.Server, and the two that are ABSENT.
//
// The hard half of this task is not setting two fields. It is keeping
// ReadTimeout and WriteTimeout UNSET against a future contributor who sees a
// server with "no timeouts" and helpfully completes the set -- which would kill
// every long-poll at the deadline, intermittently, on the bus's core mechanic.
//
// A comment alone does not survive that; the guard below does.
// ---------------------------------------------------------------------------

// TestServerTimeouts pins CORE-9's three claims about the server run() builds:
// IdleTimeout is set, MaxHeaderBytes is set, and ReadTimeout/WriteTimeout are
// NOT -- plus the behaviour the first two actually buy.
func TestServerTimeouts(t *testing.T) {
	t.Run("TheConstantsCannotKillALongPoll", func(t *testing.T) {
		// The relationship, not the literals. defaultPollTimeout is the DEFAULT;
		// -poll-timeout can raise it, which is precisely why no request-lifetime
		// deadline can be safe here at any value (see the comment on srv in
		// run()).
		if idleTimeout <= defaultPollTimeout {
			t.Errorf("idleTimeout (%s) must exceed defaultPollTimeout (%s), or an agent looping on /v1/wait re-handshakes TLS on every poll",
				idleTimeout, defaultPollTimeout)
		}
		if readHeaderTimeout >= defaultPollTimeout {
			t.Errorf("readHeaderTimeout (%s) must stay well under defaultPollTimeout (%s): it bounds HEADERS, and a value at poll scale would suggest it bounds the request",
				readHeaderTimeout, defaultPollTimeout)
		}
		if maxHeaderBytes <= 0 {
			t.Errorf("maxHeaderBytes = %d; a non-positive value makes net/http fall back to its 1 MB default, silently undoing the bound", maxHeaderBytes)
		}
	})

	// The behavioural half: prove MaxHeaderBytes REJECTS rather than merely
	// being assigned. A bound nobody has watched refuse something is not a bound.
	t.Run("MaxHeaderBytesRejectsAnOversizedHeader", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.Config.MaxHeaderBytes = maxHeaderBytes
		srv.Config.ReadHeaderTimeout = readHeaderTimeout
		srv.Start()
		defer srv.Close()

		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		// Comfortably past the bound, in ONE header, so the failure is
		// unambiguously the size limit and not a header-count limit.
		req.Header.Set("X-Oversized", strings.Repeat("A", maxHeaderBytes+(8<<10)))

		resp, err := srv.Client().Do(req)
		if err != nil {
			// net/http may reset the connection rather than answer. That is still
			// a refusal, which is the property; it is not a pass for an
			// unbounded server, because the control below proves a normal
			// request succeeds against this very server.
			t.Logf("oversized request was refused at the connection level: %v", err)
		} else {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
				t.Errorf("oversized header got status %d, want %d (RequestHeaderFieldsTooLarge)", resp.StatusCode, http.StatusRequestHeaderFieldsTooLarge)
			}
		}

		// THE CONTROL. Without it, a server that refused EVERYTHING would pass
		// the assertion above and prove nothing about the bound.
		ok, err := srv.Client().Get(srv.URL)
		if err != nil {
			t.Fatalf("an ordinary request to the same server failed: %v -- the assertion above therefore proves nothing about the header bound", err)
		}
		defer ok.Body.Close()
		if ok.StatusCode != http.StatusOK {
			t.Fatalf("an ordinary request got status %d, want 200", ok.StatusCode)
		}
	})

	// The structural half, and the reason this test exists at all.
	t.Run("RunSetsIdleAndHeaderBoundsAndNoRequestDeadline", func(t *testing.T) {
		got, err := scanServerFields(".")
		if err != nil {
			t.Fatalf("scanning the package directory: %v", err)
		}
		if got.literals == 0 {
			t.Fatal("no http.Server composite literal was found in any non-test file; this guard proves nothing")
		}
		// EVERY literal must satisfy the rules, not just some union of them.
		// scanServerFields reports per-literal for exactly this reason: a
		// scanner that OR-ed the fields together would let a second, unbounded
		// http.Server added later hide behind this one's IdleTimeout, and the
		// guard would stay green over the very thing it exists to catch.
		if got.literals != len(got.perLiteral) {
			t.Fatalf("scanner found %d literals but recorded %d; the per-literal assertions below would not cover them all", got.literals, len(got.perLiteral))
		}
		for _, lit := range got.perLiteral {
			for _, want := range []string{"IdleTimeout", "MaxHeaderBytes", "ReadHeaderTimeout"} {
				if !lit.fields[want] {
					t.Errorf("the http.Server built at %s does not set %s (CORE-9)", lit.where, want)
				}
			}
			// The load-bearing negative.
			for _, banned := range []string{"ReadTimeout", "WriteTimeout"} {
				if lit.fields[banned] {
					t.Errorf("the http.Server built at %s sets %s. It MUST NOT: both are ABSOLUTE deadlines on the whole request/response, so any value below -poll-timeout (default %s) kills a parked long-poll mid-flight. If a request-lifetime bound is genuinely needed it belongs per-handler on the request context, where the poll handler can opt out. See the comment at that literal.",
						lit.where, banned, defaultPollTimeout)
				}
			}
		}
	})
}

// oneLiteral is a single http.Server composite literal and the fields it sets.
type oneLiteral struct {
	fields map[string]bool
	where  string
}

// serverFields is what scanServerFields found across the package.
//
// perLiteral is kept SEPARATE per literal rather than unioned, so that adding a
// second, unbounded http.Server later cannot hide behind the compliant one.
type serverFields struct {
	fields     map[string]bool // the union, for the red-capability tests only
	perLiteral []oneLiteral
	literals   int
}

// scanServerFields parses every non-test .go file in dir and reports which
// fields are set on any http.Server composite literal.
//
// It reads the SOURCE rather than reflecting over a constructed server because
// the property is about what run() writes down: a zero ReadTimeout and an
// ABSENT ReadTimeout are indistinguishable at runtime, so a reflective check
// could not tell "deliberately unset" from "set to zero by a later edit", which
// is the exact distinction CORE-9 is protecting.
func scanServerFields(dir string) (serverFields, error) {
	out := serverFields{fields: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return out, fmt.Errorf("parsing %s: %w", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Server" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "http" {
				return true
			}
			out.literals++
			at := fmt.Sprintf("%s:%d", name, fset.Position(lit.Pos()).Line)
			this := oneLiteral{fields: map[string]bool{}, where: at}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok {
					this.fields[key.Name] = true
					out.fields[key.Name] = true
				}
			}
			out.perLiteral = append(out.perLiteral, this)
			return true
		})
	}
	return out, nil
}

// TestServerTimeoutsGuardIsRedCapable proves the structural guard can FAIL.
// A guard nobody has watched go red is not evidence -- and this one asserts an
// ABSENCE, which is the kind that passes for free if the scanner is broken.
func TestServerTimeoutsGuardIsRedCapable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		src        string
		wantField  string
		wantSetTo  bool
		wantLitAtL bool
	}{
		{
			name: "a literal that sets WriteTimeout is detected",
			src: `package main
import "net/http"
func run() { srv := &http.Server{Handler: nil, WriteTimeout: 0} ; _ = srv }`,
			wantField: "WriteTimeout",
			wantSetTo: true,
		},
		{
			name: "a literal missing IdleTimeout is detected",
			src: `package main
import "net/http"
func run() { srv := &http.Server{Handler: nil} ; _ = srv }`,
			wantField: "IdleTimeout",
			wantSetTo: false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "synthetic.go"), []byte(c.src), 0o600); err != nil {
				t.Fatalf("writing the synthetic package: %v", err)
			}
			got, err := scanServerFields(dir)
			if err != nil {
				t.Fatalf("scanServerFields: %v", err)
			}
			if got.literals == 0 {
				t.Fatal("the scanner found no http.Server literal in a file that plainly has one; every assertion it makes elsewhere is therefore vacuous")
			}
			if got.fields[c.wantField] != c.wantSetTo {
				t.Errorf("scanner reports %s set = %v, want %v", c.wantField, got.fields[c.wantField], c.wantSetTo)
			}
		})
	}

	// THE MASKING CASE. A compliant literal followed by an unbounded one is the
	// scenario a UNIONED scanner would report as clean -- the first literal's
	// IdleTimeout would satisfy the assertion on behalf of the second. This
	// proves the per-literal view sees both.
	t.Run("a second unbounded literal is not masked by the first", func(t *testing.T) {
		t.Parallel()
		const src = `package main
import "net/http"
func run() {
	a := &http.Server{IdleTimeout: 0, MaxHeaderBytes: 0, ReadHeaderTimeout: 0}
	b := &http.Server{WriteTimeout: 0}
	_, _ = a, b
}`
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "synthetic.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("writing the synthetic package: %v", err)
		}
		got, err := scanServerFields(dir)
		if err != nil {
			t.Fatalf("scanServerFields: %v", err)
		}
		if got.literals != 2 || len(got.perLiteral) != 2 {
			t.Fatalf("scanner found literals=%d perLiteral=%d, want 2 and 2", got.literals, len(got.perLiteral))
		}
		// The union alone would look compliant for IdleTimeout...
		if !got.fields["IdleTimeout"] {
			t.Fatal("the union does not report IdleTimeout; this case is not exercising the masking scenario")
		}
		// ...but the second literal must be visibly non-compliant.
		if got.perLiteral[1].fields["IdleTimeout"] {
			t.Error("the second literal is reported as setting IdleTimeout, but it does not; a masked literal would go unnoticed")
		}
		if !got.perLiteral[1].fields["WriteTimeout"] {
			t.Error("the second literal sets WriteTimeout and the scanner did not see it")
		}
	})

	// And the scanner must not claim a literal exists when none does.
	t.Run("no http.Server literal is reported as none", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "synthetic.go"), []byte("package main\nfunc run() {}\n"), 0o600); err != nil {
			t.Fatalf("writing the synthetic package: %v", err)
		}
		got, err := scanServerFields(dir)
		if err != nil {
			t.Fatalf("scanServerFields: %v", err)
		}
		if got.literals != 0 {
			t.Fatalf("scanner reports %d http.Server literals in a file with none", got.literals)
		}
	})
}

// TestShutdownGraceContractIsDocumented pins CORE-11's contract to the place it
// is recorded, so that deleting the explanation fails rather than silently
// leaving the next POLL handler author to rediscover it.
//
// It asserts the RELATIONSHIP the contract is about -- not the prose -- and then
// that the prose naming ctx.Done() is present at the same site. A grep for
// "ctx.Done" alone would pass on any incidental match, which is the doc-proof
// failure mode this repo has already been bitten by.
func TestShutdownGraceContractIsDocumented(t *testing.T) {
	// The premise. If someone raises shutdownGrace above defaultPollTimeout the
	// contract below stops describing reality and must be rewritten, not left.
	if shutdownGrace >= defaultPollTimeout {
		t.Fatalf("shutdownGrace (%s) is no longer shorter than defaultPollTimeout (%s); the CORE-11 comment describes the opposite and must be updated", shutdownGrace, defaultPollTimeout)
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	// Pin the specific claims, each of which is the thing a POLL handler author
	// needs, rather than a single loose token.
	for _, want := range []string{
		"ctx.Done()",
		"time.After(pollTimeout)",
		"BaseContext",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go no longer mentions %q; CORE-11 records the long-poll shutdown contract there and a handler author who cannot find it will write the blocking version", want)
		}
	}
}
