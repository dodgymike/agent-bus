package main

// Tests for MTLS-LISTENER: the bus serves https AND ONLY https, refuses to start
// rather than serve without usable key material, and carries no switch anywhere
// that could turn either of those off.
//
// Invariant 11 in one paragraph, because every assertion below is a restatement
// of it: the session token is a BEARER credential. On a plaintext listener an
// on-path observer reads it, replays it, or kills a pending challenge, and every
// other authentication control in this repo becomes decoration. So there is no
// plaintext listener, no fallback, and no flag that asks for one -- and a bus
// that cannot serve TLS does not serve at all.
//
// The three behavioural tests here run the REAL process (the subprocess harness
// in wal_startup_test.go, or run() itself). That is deliberate: the defect this
// task closes was never in crypto/tls, it was in the handful of statements in
// run() that decide what gets wrapped, and only a real process proves what was
// actually bound. The remaining two are the source-level guard and the proof
// that the guard can go red.

import (
	"crypto/tls"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/logging"
)

// TestServerServesTLSOnly is the positive half of MTLS-LISTENER: a real bus,
// dialled over https by a client that trusts EXACTLY the certificate that bus
// wrote, answers 200 on /healthz -- and says so in its startup summary.
//
// Both halves matter. The request proves the listener really is TLS and really
// presents the data directory's certificate; the log line proves an operator
// reading one line learns which scheme to dial. A bus that served TLS while
// logging tls=false would be correct and unusable.
func TestServerServesTLSOnly(t *testing.T) {
	dir := t.TempDir()
	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	// --- the request, verified against dir's certificate and nothing else ---
	resp, err := busTestClient(t, dir).Get(busURL(addr, "/healthz"))
	if err != nil {
		t.Fatalf("GET %s: %v\nThe bus must serve TLS with the certificate in %q; a failure here is either a plaintext listener or a certificate that is not the one on disk.\n%s",
			busURL(addr, "/healthz"), err, dir, proc.stderr())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("reading the /healthz body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz over https = %d, want 200; body: %s", resp.StatusCode, body)
	}

	// resp.TLS is non-nil ONLY on a connection that completed a handshake, so
	// this is the assertion that cannot be satisfied by a plaintext listener that
	// happened to answer. The version floor is checked here rather than trusted
	// from the config: busTLSConfig says TLS 1.2, and this is the observation of
	// it on a live connection.
	if resp.TLS == nil {
		t.Fatalf("GET /healthz answered 200 with resp.TLS == nil: the connection was NOT TLS")
	}
	if !resp.TLS.HandshakeComplete {
		t.Errorf("the TLS handshake did not complete on a 200 response")
	}
	if resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS version %#04x, want at least TLS 1.2 (%#04x)", resp.TLS.Version, tls.VersionTLS12)
	}

	// --- the startup summary ---
	started := proc.line(t, msgServerStarted)
	fields := parseLogfmt(started)
	for _, c := range []struct{ key, want, why string }{
		{"tls", "true", "an operator reading one line must be able to tell that this listener is TLS"},
		{"scheme", "https", "the scheme is what an operator types; there is no http:// form of this bus"},
		{"tls_min_version", "1.2", "the floor is contract, and it matches client/pin.go's so neither end fails the other with an unreadable handshake error"},
		{"client_auth", "none", "MTLS-LISTENER does NOT request a client certificate: requiring one before MTLS-CLIENTCERT teaches the client to present one would refuse every agent at the handshake"},
	} {
		if got := fields[c.key]; got != c.want {
			t.Errorf("%q field %s = %q, want %q: %s\nline: %s", msgServerStarted, c.key, got, c.want, c.why, started)
		}
	}
	// The fingerprint stays in the summary: it is the value an agent must pass to
	// --bus-fingerprint, and there is deliberately no trust-on-first-use.
	if fp := fields["bus_cert_fingerprint"]; len(fp) != 64 {
		t.Errorf("%q field bus_cert_fingerprint = %q, want a 64-hex-character sha256; clients pin this value\nline: %s", msgServerStarted, fp, started)
	}

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
}

// TestPlaintextClientIsRejected is the negative half: the same bus, dialled with
// http://, must not serve anything.
//
// # WHAT "REJECTED" ACTUALLY LOOKS LIKE, stated rather than left as "any error"
//
// A TLS server handed the bytes "GET /healthz HTTP/1.1" sees a record whose
// first byte is 'G', which is not a legal TLS record type. crypto/tls reports
// that as a RecordHeaderError, and net/http's server RECOGNISES that specific
// case and writes back a plaintext "HTTP/1.0 400 Bad Request" carrying
// "Client sent an HTTP request to an HTTPS server." before closing. So the
// honest expectation is ONE of exactly two outcomes, and this test names both:
//
//  1. a transport error (the connection died before a response), or
//  2. HTTP 400 with that specific diagnostic body.
//
// Anything else -- and emphatically any 200, or any body carrying the /healthz
// payload -- is a plaintext listener and fails loudly. "The request errored" on
// its own would be a weak assertion, because a bus that was simply DOWN would
// satisfy it; TestServerServesTLSOnly and the https probe below are what stop
// this test passing vacuously against a dead process.
func TestPlaintextClientIsRejected(t *testing.T) {
	dir := t.TempDir()
	proc := startServer(t, dir)
	addr := proc.awaitServerStarted(t)

	// NOT VACUOUS: the bus is up and serving on this very port over https. Every
	// assertion below is therefore about the SCHEME and not about liveness.
	mustGetHealthz(t, dir, addr)

	assertPlaintextRefused(t, addr, proc.stderr)

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
}

// assertPlaintextRefused is the reusable half of TestPlaintextClientIsRejected:
// a plaintext GET /healthz against addr must not be served.
//
// detail is called only to build a failure message (typically a server's
// captured stderr); it may be nil. It is a func rather than a string so the
// snapshot is taken at the moment of failure rather than before the request.
//
// The CALLER is responsible for proving the bus is up first. Without that this
// assertion is satisfied by a dead process, which is the one way it could pass
// while proving nothing.
func assertPlaintextRefused(t *testing.T, addr string, detail func() string) {
	t.Helper()

	explain := func() string {
		if detail == nil {
			return ""
		}
		return "\n" + detail()
	}

	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Timeout:   busTestTimeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect to https would be a "helpful" plaintext listener, which
			// is still a plaintext listener. Never follow one; report it.
			return http.ErrUseLastResponse
		},
	}

	plainURL := "http://" + addr + "/healthz"
	resp, err := client.Get(plainURL)
	if err != nil {
		// Outcome 1. Logged, not merely swallowed, so a future change of failure
		// mode is visible in -v output rather than silently accepted.
		t.Logf("GET %s failed at the transport, as expected for a TLS-only listener: %v", plainURL, err)
		return
	}

	// Outcome 2, or a defect.
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		t.Logf("reading the plaintext response body: %v", readErr)
	}

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("GET %s answered 200 with body %q. THE BUS IS SERVING PLAINTEXT. Invariant 11: there is no plaintext listener, because the session token is a bearer credential and an on-path observer reads it off the wire.%s",
			plainURL, body, explain())
	}
	if strings.Contains(string(body), `"status"`) {
		t.Fatalf("GET %s answered %d but the body %q is the /healthz payload: a route was served over plaintext%s",
			plainURL, resp.StatusCode, body, explain())
	}
	// Pin the exact shape, so a change in Go's behaviour is REVIEWED rather than
	// absorbed by a test that would accept any non-200.
	const wantDiagnostic = "Client sent an HTTP request to an HTTPS server."
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), wantDiagnostic) {
		t.Fatalf("GET %s answered %d with body %q; want either a transport error or 400 containing %q. Any other response means something other than a TLS listener answered on this port.%s",
			plainURL, resp.StatusCode, body, wantDiagnostic, explain())
	}
	t.Logf("GET %s was refused with %d %q, as expected for a TLS-only listener", plainURL, resp.StatusCode, wantDiagnostic)
}

// TestRunRefusesToStartWithoutUsableCert is the OBSERVED refusal: with no
// plaintext listener there is nothing to fall back TO, so unusable key material
// must stop the process rather than produce a bus that looks up and serves
// nothing.
//
// The load-bearing part is the SECOND half. "run() returned an error" is only
// half a refusal; the other half is that the port was never taken. That is why
// this test uses a FIXED loopback port, proves it free before, and proves
// nothing accepted a connection on it after -- an assertion that is simply
// impossible against the ephemeral 127.0.0.1:0 every other test here uses.
//
// It also pins the ORDERING inside run(): busTLSConfig is built BEFORE
// net.Listen, so a bus refused over a broken certificate does not also leave the
// operator's port occupied while a second, healthier process tries to bind it.
func TestRunRefusesToStartWithoutUsableCert(t *testing.T) {
	cases := []struct {
		name string
		// damage mutates a data dir that has already completed one clean start.
		damage func(t *testing.T, dir string)
		// wantIn are fragments the refusal must contain. The PATH is always one:
		// the first question asked of a bus that will not start is "which file".
		wantIn []string
	}{
		{
			name: "the certificate is gone",
			damage: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, buscert.CertFileName)); err != nil {
					t.Fatalf("removing the bus certificate: %v", err)
				}
			},
			wantIn: []string{"incomplete", buscert.CertFileName},
		},
		{
			name: "the TLS key is truncated",
			damage: func(t *testing.T, dir string) {
				path := filepath.Join(dir, buscert.TLSKeyFileName)
				b := mustReadFile(t, path)
				if err := os.WriteFile(path, b[:len(b)/2], buscert.KeyFileMode); err != nil {
					t.Fatalf("truncating %q: %v", path, err)
				}
			},
			wantIn: []string{buscert.TLSKeyFileName},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			// One real, complete start, so the damage lands on material this
			// binary actually wrote rather than on a hand-built fixture.
			seed := startServer(t, dir)
			seed.awaitServerStarted(t)
			seed.signal(t, syscall.SIGTERM)
			if code := seed.awaitExit(t, shutdownTimeout); code != 0 {
				t.Fatalf("the seeding start exited %d, want 0\n%s", code, seed.stderr())
			}
			tc.damage(t, dir)

			// A port we have PROVED is free, and hold no claim on.
			addr := freeLoopbackAddr(t)
			mustBeUnbound(t, addr, "before the refused start")

			err := run(Config{
				Listen:      addr,
				DataDir:     dir,
				PollTimeout: time.Second,
				LogLevel:    logging.LevelError,
			})
			if err == nil {
				t.Fatalf("run() over damaged bus key material in %q = nil, want an error. There is no plaintext listener to fall back to (invariant 11), so this must be FATAL rather than a bus that came up serving nothing.", dir)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("run() error %q does not mention %q; an operator cannot act on a refusal that does not name the file", err.Error(), want)
				}
			}
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("run() error %q does not name the data dir %q", err.Error(), dir)
			}

			// AND NOTHING WAS SERVED. This is the half that a "returns an error"
			// assertion misses entirely.
			mustBeUnbound(t, addr, "after the refused start")
		})
	}
}

// TestCmdHasNoPlaintextListener is a PERMANENT source-level invariant, and it is
// the replacement for the deleted TestCmdDoesNotServeTLS (see the marker in
// buscert_test.go where that one stood).
//
// The two are not variations on a theme; they point in opposite directions and
// the swap is the whole change. The old guard was a temporary scaffold that
// banned tls.NewListener while MTLS-BUSCERT was generate-and-load only. This one
// encodes invariant 11 and is never to be deleted: there is NO plaintext
// listener, and no flag, environment variable or build tag that makes one.
//
// It asserts three things about the non-test .go files in this package:
//
//	a. InsecureSkipVerify appears NOWHERE. It is permitted in exactly one file in
//	   this repo (client/pin.go, where it is paired with VerifyPeerCertificate to
//	   implement fingerprint pinning) and cmd/agent-bus is not it. Invariant 11:
//	   never disable certificate verification to make something work.
//	b. tls.NewListener (or ServeTLS) IS present. This is the direction that
//	   matters most and the reason this is not a grep for banned words: delete the
//	   TLS wrap from run() and the listener silently becomes plaintext again, with
//	   every other test in this package still passing because they would simply
//	   fail to connect... or worse, would connect.
//	c. no flag registered on a *flag.FlagSet has a name that would make TLS
//	   optional. A flag is how this invariant would most plausibly be eroded --
//	   "just for local development" -- and a flag that does it SILENTLY is called
//	   out by name in invariant 11.
//	d. the value returned by net.Listen is used EXACTLY as the argument to
//	   tls.NewListener and nowhere else. Added after the reviewer gate found
//	   main.go asserting this check existed when it did not. It is the one that
//	   catches the defect the other three cannot see: a package that wraps its
//	   listener in TLS *and* serves the raw one beside it satisfies (a), (b) and
//	   (c) completely, while serving every route in the clear on the same port --
//	   and every other test in this package keeps passing, because they all dial
//	   the TLS listener.
//
// It is an AST walk rather than a grep so that the prose in tlslisten.go and
// main.go explaining WHY there is no InsecureSkipVerify does not trip it.
func TestCmdHasNoPlaintextListener(t *testing.T) {
	res, err := scanPlaintextListener(".")
	if err != nil {
		t.Fatalf("scanning the package directory: %v", err)
	}
	for _, f := range res.findings {
		t.Errorf("%s", f)
	}
	// A guard that inspected nothing passes. Prove it read the package.
	if res.checked == 0 {
		t.Fatal("no non-test .go files were parsed; this guard proves nothing")
	}
	// (b) The positive half. Deleting the tls.NewListener wrap in run() must fail
	// HERE, loudly, and not merely turn every other test in this package into a
	// connection error someone might "fix" by switching them back to http://.
	if res.servesTLSAt == "" {
		t.Fatalf("none of the %d non-test .go files in this package calls tls.NewListener (or ServeTLS): NOTHING SERVES TLS. Invariant 11 requires every HTTP surface to be served over TLS with no plaintext listener anywhere; run() must wrap its one net.Listen before serving on it.", res.checked)
	}
	t.Logf("TLS is served: %s (%d non-test files inspected)", res.servesTLSAt, res.checked)
}

// TestCmdHasNoPlaintextListenerGuardIsRedCapable proves the guard above can
// FAIL, which is the only thing that makes its green meaningful.
//
// A guard nobody has watched go red is not evidence: this repo's own CLAUDE.md
// records a doc proof that passed on an incidental match and would have
// green-lit closing a task two reviewers had blocked. So each of the three
// claims is run here against a synthetic package that violates exactly it, in a
// throwaway directory the walker parses but nothing compiles.
func TestCmdHasNoPlaintextListenerGuardIsRedCapable(t *testing.T) {
	t.Parallel()

	// The compliant baseline every case below mutates one property of. It must
	// pass, or a case that "fails" would prove nothing about which property.
	const good = `package main

import (
	"crypto/tls"
	"flag"
	"net"
	"net/http"
)

func run() {
	fs := flag.NewFlagSet("agent-bus", flag.ContinueOnError)
	var listen string
	fs.StringVar(&listen, "listen", "127.0.0.1:8080", "TCP address")
	raw, _ := net.Listen("tcp", listen)
	ln := tls.NewListener(raw, &tls.Config{MinVersion: tls.VersionTLS12})
	srv := &http.Server{}
	_ = srv.Serve(ln)
}
`

	cases := []struct {
		name     string
		src      string
		wantFind string // substring of the finding; empty means "no finding"
		wantTLS  bool
	}{
		{name: "compliant", src: good, wantTLS: true},
		{
			name:     "InsecureSkipVerify as a struct field",
			src:      strings.Replace(good, "MinVersion: tls.VersionTLS12", "InsecureSkipVerify: true", 1),
			wantFind: "InsecureSkipVerify",
			wantTLS:  true,
		},
		{
			name:     "InsecureSkipVerify by assignment",
			src:      strings.Replace(good, "srv := &http.Server{}", "cfg := &tls.Config{}\n\tcfg.InsecureSkipVerify = true\n\tsrv := &http.Server{}", 1),
			wantFind: "InsecureSkipVerify",
			wantTLS:  true,
		},
		{
			// Two claims fire here, and that is right rather than redundant:
			// (b) nothing wraps a listener any more, and (d) the raw listener
			// is used somewhere other than as tls.NewListener's argument.
			name:     "the TLS wrap is deleted",
			src:      strings.Replace(good, "ln := tls.NewListener(raw, &tls.Config{MinVersion: tls.VersionTLS12})", "ln := raw", 1),
			wantFind: "RAW listener",
			wantTLS:  false,
		},
		{
			// THE CASE CLAIMS (a)-(c) ALL MISS, and the reason claim (d) exists.
			// TLS is wrapped and served exactly as in the compliant baseline --
			// and the raw listener is ALSO served, in the clear, on the same
			// port. Every other assertion in this package still passes against
			// the TLS listener while every route is readable on the plaintext
			// one. The reviewer gate found main.go claiming this was caught when
			// it was not.
			name: "a second Serve on the RAW listener beside the TLS one",
			src: strings.Replace(good, "_ = srv.Serve(ln)",
				"go srv.Serve(ln)\n\t_ = srv.Serve(raw)", 1),
			wantFind: "RAW listener",
			wantTLS:  true,
		},
		{
			// The subtler shape of the same defect: no second Serve, the raw
			// listener is merely leaked to something else that could serve it.
			// There is no legitimate second use, so this is a finding too.
			name: "the raw listener escapes to another function",
			src: strings.Replace(good, "srv := &http.Server{}",
				"debugServe(raw)\n\tsrv := &http.Server{}", 1),
			wantFind: "RAW listener",
			wantTLS:  true,
		},
		{
			name:     "a -plaintext flag",
			src:      strings.Replace(good, `"listen", "127.0.0.1:8080", "TCP address"`, `"plaintext", "", "serve without TLS"`, 1),
			wantFind: `"plaintext"`,
			wantTLS:  true,
		},
		{
			name:     "a -no-tls boolean flag",
			src:      strings.Replace(good, `fs.StringVar(&listen, "listen", "127.0.0.1:8080", "TCP address")`, `var off bool`+"\n\t"+`fs.BoolVar(&off, "no-tls", false, "disable TLS")`+"\n\t"+`listen = "127.0.0.1:8080"`, 1),
			wantFind: `"no-tls"`,
			wantTLS:  true,
		},
		{
			name:     "an -INSECURE flag, differently cased",
			src:      strings.Replace(good, `"listen", "127.0.0.1:8080", "TCP address"`, `"INSECURE", "", "skip verification"`, 1),
			wantFind: `"INSECURE"`,
			wantTLS:  true,
		},
		{
			// The prose in tlslisten.go and main.go says "InsecureSkipVerify"
			// repeatedly to explain why there is none. An AST walk must not
			// trip on a comment; a grep would.
			name:     "the word only in a comment and a string",
			src:      strings.Replace(good, "func run() {", "// InsecureSkipVerify must never appear here.\nvar why = \"no InsecureSkipVerify, no -plaintext flag\"\n\nfunc run() {", 1),
			wantFind: "",
			wantTLS:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// A _test.go decoy alongside it: the walk must skip test files, or
			// this very file's fixtures above would fail the real guard.
			for name, src := range map[string]string{
				"main.go":      tc.src,
				"main_test.go": "package main\n\nvar leak = \"InsecureSkipVerify -plaintext\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
					t.Fatalf("writing the fixture %s: %v", name, err)
				}
			}

			res, err := scanPlaintextListener(dir)
			if err != nil {
				t.Fatalf("scanPlaintextListener(%q): %v", dir, err)
			}
			if res.checked != 1 {
				t.Fatalf("parsed %d non-test files, want exactly 1 (main.go); the walk must skip _test.go", res.checked)
			}
			if (res.servesTLSAt != "") != tc.wantTLS {
				t.Errorf("servesTLSAt = %q, want present = %v", res.servesTLSAt, tc.wantTLS)
			}
			joined := strings.Join(res.findings, "\n")
			if tc.wantFind == "" {
				if len(res.findings) != 0 {
					t.Fatalf("the guard flagged a COMPLIANT fixture:\n%s", joined)
				}
				return
			}
			if len(res.findings) == 0 {
				t.Fatalf("the guard found nothing in a fixture that violates %q; it cannot go red and so proves nothing\nsource:\n%s", tc.name, tc.src)
			}
			if !strings.Contains(joined, tc.wantFind) {
				t.Fatalf("the guard's finding does not mention %q:\n%s", tc.wantFind, joined)
			}
		})
	}
}

// plaintextScan is what one pass over a package directory found.
type plaintextScan struct {
	findings    []string // one per violation, already formatted with file:line
	servesTLSAt string   // where TLS is actually served, "" if nowhere
	checked     int      // non-test .go files parsed
}

// scanPlaintextListener implements TestCmdHasNoPlaintextListener's three claims
// over the non-test .go files in dir.
//
// It is a function returning findings rather than a test calling t.Errorf inline
// so that TestCmdHasNoPlaintextListenerGuardIsRedCapable can watch it FAIL on
// synthetic sources. A guard that has only ever been observed passing is not
// evidence that it can fail.
func scanPlaintextListener(dir string) (plaintextScan, error) {
	var res plaintextScan

	// (c) Names that would make TLS optional. Compared case-insensitively and
	// exactly: "tls-min-version" is a fine flag to add one day, "tls" is not.
	bannedFlagNames := map[string]string{
		"tls":                  "a -tls flag implies TLS is a choice; it is not",
		"no-tls":               "there is no plaintext listener to select",
		"notls":                "there is no plaintext listener to select",
		"insecure":             "invariant 11: never ship a flag that disables verification",
		"plaintext":            "the session token is a bearer credential; plaintext leaks it",
		"allow-plaintext":      "the session token is a bearer credential; plaintext leaks it",
		"disable-tls":          "the server must refuse to start rather than fall back to plaintext",
		"insecure-skip-verify": "invariant 11: never ship a flag that disables verification",
		"http":                 "the bus is https-only; an -http flag is a plaintext listener by another name",
		"allow-http":           "the bus is https-only; an -http flag is a plaintext listener by another name",
	}
	// Every flag.FlagSet (and package-level flag) registration form. The NAME is
	// the first string LITERAL argument in all of them: fs.String("name", def,
	// usage) and fs.StringVar(&v, "name", def, usage) alike.
	flagRegistrars := map[string]bool{
		"Bool": true, "BoolVar": true,
		"Duration": true, "DurationVar": true,
		"Float64": true, "Float64Var": true,
		"Int": true, "IntVar": true,
		"Int64": true, "Int64Var": true,
		"String": true, "StringVar": true,
		"Uint": true, "UintVar": true,
		"Uint64": true, "Uint64Var": true,
		"Func": true, "TextVar": true, "Var": true,
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	fset := token.NewFileSet()

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return res, err
		}
		res.checked++

		ast.Inspect(file, func(n ast.Node) bool {
			// (a) and (b): any reference to the identifier, whether as a
			// selector (tls.NewListener) or as a struct field key
			// (tls.Config{InsecureSkipVerify: true}).
			var name string
			var pkg string
			var pos token.Pos
			switch node := n.(type) {
			case *ast.SelectorExpr:
				name, pos = node.Sel.Name, node.Sel.Pos()
				if id, ok := node.X.(*ast.Ident); ok {
					pkg = id.Name
				}
			case *ast.KeyValueExpr:
				if id, ok := node.Key.(*ast.Ident); ok {
					name, pos = id.Name, id.Pos()
				}
			}
			switch {
			case name == "InsecureSkipVerify":
				res.findings = append(res.findings, fmt.Sprintf(
					"%s references InsecureSkipVerify. Invariant 11: certificate verification is NEVER disabled to make something work, and never behind a flag that does it silently. It is permitted in exactly one file in this repo (client/pin.go, paired with VerifyPeerCertificate) and this package is not it.",
					fset.Position(pos)))
			case name == "NewListener" && pkg == "tls", name == "ServeTLS", name == "ListenAndServeTLS":
				if res.servesTLSAt == "" {
					res.servesTLSAt = fset.Position(pos).String()
				}
			}

			// (c) flag registration.
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !flagRegistrars[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				flagName := strings.Trim(lit.Value, "`\"")
				if why, bad := bannedFlagNames[strings.ToLower(flagName)]; bad {
					res.findings = append(res.findings, fmt.Sprintf(
						"%s registers the flag %q via %s: %s. Invariant 11: TLS is the required transport, the server refuses to start rather than fall back to plaintext, and there is no flag that changes that.",
						fset.Position(lit.Pos()), flagName, sel.Sel.Name, why))
				}
				break // only the FIRST string literal is the flag name
			}
			return true
		})

		// (d) THE RAW LISTENER IS ONLY EVER WRAPPED. Kept out of the walk above
		// because it needs the whole file, not one node at a time.
		res.findings = append(res.findings, scanRawListenerUses(fset, file)...)
	}
	return res, nil
}

// scanRawListenerUses implements claim (d): the value returned by net.Listen is
// used EXACTLY as the argument to tls.NewListener, and nowhere else.
//
// This is the check the invariant actually needs, and it was missing until the
// reviewer gate found main.go claiming it existed. Claims (a)-(c) are all
// satisfied by a package that wraps its listener in TLS and ALSO does
// srv.Serve(rawLn) two lines later: tls.NewListener is present, no banned flag is
// registered, no InsecureSkipVerify appears -- and every route is served in the
// clear on the same port, with every other test in this package still green
// because they all dial the TLS one. That is the exact defect invariant 11
// describes, and nothing else here catches it.
//
// The rule enforced: for each identifier bound to a net.Listen result, every
// later reference to it must be a direct argument of a tls.NewListener call. A
// bare `_ = rawLn` would trip it, which is correct -- there is no legitimate
// second use, and a guard that guessed at which ones were harmless would be back
// to proving nothing.
//
// It is deliberately syntactic and single-file. It does not resolve types, so a
// net.Listener obtained some other way is out of its reach; that is a bounded,
// stated limitation rather than a hidden one, and claim (b) still requires that
// SOMETHING in the package wraps a listener in TLS.
func scanRawListenerUses(fset *token.FileSet, file *ast.File) []string {
	// Pass one: identifiers assigned from net.Listen.
	raw := map[string]token.Pos{}
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Listen" {
				continue
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "net" {
				continue
			}
			// net.Listen returns (Listener, error); the listener is the first
			// LHS operand. "_" is not a use of anything.
			if len(assign.Lhs) == 0 {
				continue
			}
			if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
				raw[id.Name] = id.Pos()
			}
		}
		return true
	})
	if len(raw) == 0 {
		return nil
	}

	// Pass two: every position at which such an identifier is a direct argument
	// of tls.NewListener. Recorded by POSITION so the reference-counting pass
	// below can tell the wrapped use from any other.
	wrapped := map[token.Pos]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewListener" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "tls" {
			return true
		}
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				if _, isRaw := raw[id.Name]; isRaw {
					wrapped[id.Pos()] = true
				}
			}
		}
		return true
	})

	// Pass three: any other reference is a finding.
	var findings []string
	ast.Inspect(file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		declPos, isRaw := raw[id.Name]
		if !isRaw || id.Pos() == declPos || wrapped[id.Pos()] {
			return true
		}
		findings = append(findings, fmt.Sprintf(
			"%s uses the RAW listener %q somewhere other than as the argument to tls.NewListener. The result of net.Listen must be wrapped and then forgotten: serving on it directly is a PLAINTEXT LISTENER on the same port, and it would leave every test in this package passing because they all dial the TLS one. Invariant 11: there is no plaintext listener and no fallback.",
			fset.Position(id.Pos()), id.Name))
		return true
	})
	return findings
}

// freeLoopbackAddr returns a loopback host:port that was bindable a moment ago
// and is unbound now.
//
// Reserving a port by binding and releasing it is inherently a small race with
// the rest of the machine, and that is accepted deliberately: the alternative is
// a hard-coded port, which races with every other checkout of this repo on the
// same box. Go sets SO_REUSEADDR on listeners, so the released port is
// immediately re-bindable rather than stuck in TIME_WAIT (nothing ever connected
// to it). mustBeUnbound below turns the residual race into a NAMED failure
// rather than a mysterious one.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port %s: %v", addr, err)
	}
	return addr
}

// mustBeUnbound asserts that nothing accepts a connection on addr.
//
// Two independent observations, because either alone is weak: a dial that is
// REFUSED proves no listener answered, and a successful re-bind proves the port
// is genuinely free rather than merely filtered.
func mustBeUnbound(t *testing.T, addr, when string) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("something accepted a TCP connection on %s %s; a refused start must never have taken the port", addr, when)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("cannot bind %s %s: %v; the port must be free (if another process on this machine grabbed it, re-run -- this test reserves an ephemeral port and releases it)", addr, when, err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener on %s: %v", addr, err)
	}
}

// TestBusTLSConfig is the direct table over busTLSConfig, added on the reviewer
// gate's finding that the function was only ever exercised through run().
//
// Two things are pinned here that a whole-process test cannot pin cheaply. The
// POSITIVE half is the policy itself -- the floor, the ALPN list, the client-auth
// setting and the disabled session tickets are all contract (CONTRACTS-CLI.md
// documents them), and a silent change to any of them is exactly the kind of
// weakening that leaves every other test green. The NEGATIVE half is the two
// refusal messages, which are unreachable in practice (internal/buscert refuses
// first) and therefore have no other coverage at all.
//
// ClientAuth deserves its own sentence, because this is the assertion that would
// fail the day someone "finishes" mutual TLS here rather than in
// MTLS-CLIENTAUTH: the bus must NOT request a client certificate until
// MTLS-CLIENTCERT teaches the client to present one, or every agent is refused at
// the handshake.
func TestBusTLSConfig(t *testing.T) {
	t.Parallel()

	t.Run("policy", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		material, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: "bus-tlscfgtest"})
		if err != nil {
			t.Fatalf("minting material for the fixture: %v", err)
		}

		cfg, err := busTLSConfig(material)
		if err != nil {
			t.Fatalf("busTLSConfig over freshly minted material = %v, want a usable config", err)
		}
		if got, want := len(cfg.Certificates), 1; got != want {
			t.Fatalf("cfg.Certificates has %d entries, want %d: the bus must present its own certificate", got, want)
		}
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("cfg.MinVersion = %#04x, want TLS 1.2 (%#04x); it is deliberately the SAME floor as client/pin.go's pinnedTLSConfig, so neither end fails the other with an unreadable handshake error", cfg.MinVersion, tls.VersionTLS12)
		}
		if cfg.ClientAuth != tls.NoClientCert {
			t.Errorf("cfg.ClientAuth = %v, want tls.NoClientCert. Requiring a client certificate is MTLS-CLIENTAUTH and MUST NOT land before MTLS-CLIENTCERT teaches the client to present one -- a bus that demanded one today would refuse every agent in the fleet at the handshake, before any route, log line or error they could act on.", cfg.ClientAuth)
		}
		if !cfg.SessionTicketsDisabled {
			t.Errorf("cfg.SessionTicketsDisabled = false: crypto/tls does not call VerifyPeerCertificate on a RESUMED handshake, and this project's entire certificate pin lives in that callback (client/pin.go). Refusing to issue tickets is what makes the pin unbypassable from the server end regardless of what any client does later.")
		}
		if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
			t.Errorf("cfg.NextProtos = %q, want [\"http/1.1\"]: this listener is wrapped by tls.NewListener and served by Serve, which does not configure HTTP/2, so advertising anything else would be a lie told over ALPN", cfg.NextProtos)
		}
		// InsecureSkipVerify is meaningless on a server and must never be set;
		// the AST guard covers the source, this covers the value.
		if cfg.InsecureSkipVerify {
			t.Errorf("cfg.InsecureSkipVerify = true on a SERVER config")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name     string
			material *buscert.Material
			wantIn   string
		}{
			{
				name:     "no material at all",
				material: nil,
				wantIn:   "no bus certificate material",
			},
			{
				name:     "material carrying neither a chain nor a key",
				material: &buscert.Material{},
				wantIn:   "no certificate chain",
			},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg, err := busTLSConfig(tc.material)
				if err == nil {
					t.Fatalf("busTLSConfig(%s) = %+v, nil; want an error. A zero tls.Certificate is accepted by tls.NewListener without complaint and then fails at EVERY handshake with no startup signal at all -- the silent half-outage the refusal exists to prevent.", tc.name, cfg)
				}
				if cfg != nil {
					t.Errorf("busTLSConfig returned a non-nil config alongside an error; there is no plaintext listener to fall back to, so there must be nothing to serve WITH either")
				}
				if !strings.Contains(err.Error(), tc.wantIn) {
					t.Errorf("busTLSConfig error %q does not mention %q", err.Error(), tc.wantIn)
				}
			})
		}
	})
}
