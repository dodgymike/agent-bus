package client

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pinFile is the ONE file permitted to disable crypto/tls's default chain
// verification, and only in the arrangement TestPinnedSkipIsAlwaysPairedWithAPinCheck
// asserts. It is relative to the client package directory.
const pinFile = "pin.go"

// guardRoots are the two directories these guards police: this package and the
// CLI that shells over it.
func guardRoots() []string { return []string{".", filepath.Join("..", "cmd", "agent-busctl")} }

// walkGoFiles calls fn for every .go file under the guard roots, and fails the
// test if it visited none — a guard that inspected nothing passes.
//
// guard_test.go itself is skipped: it necessarily names the strings it bans in
// order to ban them. That is not a loophole. Every OTHER file, including every
// other test file, is still scanned and still counted.
func walkGoFiles(t *testing.T, includeTests bool, fn func(path string, src []byte)) {
	t.Helper()
	var scanned int
	for _, root := range guardRoots() {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Compared as a CLEANED PATH, not a basename. By basename, a file
			// added at cmd/agent-busctl/guard_test.go would have been skipped
			// silently — an unscanned hole with a name that looks deliberate.
			if filepath.Clean(path) == "guard_test.go" {
				return nil
			}
			if !includeTests && strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			scanned++
			fn(path, b)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 .go files under %v; this guard is vacuous", guardRoots())
	}
}

// TestNoInsecureSkipVerifyAnywhere is the standing-rule regression guard
// (DECISIONS.md, 2026-08-02 "E7: no plaintext escape hatch", amended 2026-08-07
// by MTLS-PIN): the string InsecureSkipVerify appears in EXACTLY ONE file under
// client/ or cmd/agent-busctl/, client/pin.go, and exactly once in it.
//
// # Why the rule is now "one named file" rather than "nowhere"
//
// It was "nowhere" while nothing needed it. Pinning does. agent-bus
// certificates are self-signed with NO certificate authority and NO
// trust-on-first-use (invariant 11), so the stdlib's default chain verification
// cannot succeed and cannot be made to: there is no root to chain to. crypto/tls
// offers exactly one supported way to substitute a different policy — turn the
// default check off and supply VerifyPeerCertificate — and its own documentation
// describes that pairing. A ban with no exception would not have prevented the
// exception; it would have pushed it somewhere this guard does not look, which
// is strictly worse.
//
// So the guard got NARROWER AND STRICTER at the same time, and the strictness is
// in the companion test below: the exemption is worth nothing on its own,
// because a skip WITHOUT a pin check is precisely the silent failure this task
// exists to prevent.
//
// Do not add a second file to this exemption. If another file needs it, that is
// a design question for the security gate, not a constant to append to.
func TestNoInsecureSkipVerifyAnywhere(t *testing.T) {
	const banned = "InsecureSkipVerify"
	var exemptOccurrences int
	walkGoFiles(t, true, func(path string, src []byte) {
		n := strings.Count(string(src), banned)
		if n == 0 {
			return
		}
		if filepath.Clean(path) == filepath.Clean(pinFile) {
			exemptOccurrences += n
			return
		}
		t.Errorf("%s contains %s. It may appear ONLY in client/%s, where it is paired with the pinned-fingerprint check (DECISIONS.md 2026-08-07, MTLS-PIN). "+
			"Tests mint real certificates instead — see newSelfSignedBusCert in pin_test.go.", path, banned, pinFile)
	})
	switch exemptOccurrences {
	case 1:
		// The single field assignment in pinnedTLSConfig. Correct.
	case 0:
		t.Errorf("client/%s no longer contains %s. If the pinned TLS configuration was removed or moved, this guard and its companion must move with it — a guard left pointing at a file that no longer does the thing is a guard that passes forever.", pinFile, banned)
	default:
		t.Errorf("client/%s contains %s %d times; exactly one occurrence is permitted, so that there is exactly one place to review. Do not mention it in prose here either — this guard counts occurrences, not uses.", pinFile, banned, exemptOccurrences)
	}
}

// TestPinnedSkipIsAlwaysPairedWithAPinCheck is the guard that actually carries
// the security property, and it is an AST walk rather than a grep so it can see
// STRUCTURE: which fields are set in the SAME tls.Config literal.
//
// The failure it exists to catch is one line wide and completely silent.
// A tls.Config that disables the default chain check AND sets
// VerifyPeerCertificate verifies an exact certificate — stronger than the CA and
// hostname checks it replaces. The same literal with the callback deleted still
// compiles, still completes handshakes, still returns working connections, and
// verifies NOTHING. No ordinary test tells them apart, because every positive
// test passes either way; only a negative test and this guard do.
//
// It therefore asserts three things across client/ and cmd/agent-busctl/:
//
//  1. Any composite literal that sets InsecureSkipVerify to true must set
//     VerifyPeerCertificate, non-nil, in that same literal.
//  2. InsecureSkipVerify is never set by ASSIGNMENT (cfg.InsecureSkipVerify =
//     true). An assignment can be conditional, can be far from the literal, and
//     defeats check 1 entirely.
//  3. At least one such paired literal exists — otherwise this guard would pass
//     trivially on a tree where pinning had been deleted.
//
// # It is HALF the check, and the other half is a behavioural test
//
// This guard sees SHAPE, never SEMANTICS. A callback that ignores its argument
// and returns nil — `func([][]byte, [][]*x509.Certificate) error { return nil }`
// — satisfies every assertion here and accepts any certificate on earth. What
// catches that is TestClientRefusesChangedBusFingerprint in pin_test.go, which
// swaps the certificate under a live bus and asserts the connection is refused.
// The security gate confirmed the split empirically: deleting the callback or
// setting it nil fails THIS test; replacing it with an accept-everything body
// passes this one and fails THAT one.
//
// Neither test is redundant and neither may be deleted on the grounds that the
// other covers it. This cross-reference exists because that is exactly the
// argument someone will make.
func TestPinnedSkipIsAlwaysPairedWithAPinCheck(t *testing.T) {
	const (
		skipField   = "InsecureSkipVerify"
		verifyField = "VerifyPeerCertificate"
		// cacheField enables TLS session resumption, which SKIPS
		// verifyField entirely. See the check below.
		cacheField = "ClientSessionCache"
		// connField IS called on a resumed handshake — crypto/tls invokes it
		// inside the resumption branch, under its own comment "Make sure the
		// connection is still being verified whether or not this is a
		// resumption." It is therefore the supported way to keep the pin and
		// expiry checks alive if resumption is ever wanted, and the guard must
		// not reject the remedy it prescribes.
		connField = "VerifyConnection"
	)
	var paired int
	fset := token.NewFileSet()

	walkGoFiles(t, true, func(path string, src []byte) {
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if sel.Sel.Name == skipField {
						t.Errorf("%s: %s is set by ASSIGNMENT. It may only be set inside the single tls.Config composite literal in client/%s, where this guard can see that %s is set beside it.",
							fset.Position(sel.Pos()), skipField, pinFile, verifyField)
					}
					// The literal check below cannot see this. The security gate
					// reproduced the bypass over live TLS: with a cache attached
					// by assignment — in transport.go, or two lines under the
					// DO-NOT-ADD comment in pin.go itself — the second connection
					// RESUMED and was accepted while the server served a
					// completely unpinned certificate, because resumption skips
					// VerifyPeerCertificate entirely.
					if sel.Sel.Name == cacheField {
						t.Errorf("%s: %s is set by ASSIGNMENT. A TLS session cache makes resumed handshakes SKIP %s, which silently disables the pinned-certificate and expiry checks on every resumed connection — demonstrated over a live handshake accepting an unpinned certificate. There is no supported place to set it; if resumption is ever needed, the checks must also run in VerifyConnection.",
							fset.Position(sel.Pos()), cacheField, verifyField)
					}
				}
			case *ast.CompositeLit:
				fields := map[string]ast.Expr{}
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok {
						fields[key.Name] = kv.Value
					}
				}
				skip, hasSkip := fields[skipField]
				if !hasSkip {
					return true
				}
				if id, ok := skip.(*ast.Ident); !ok || id.Name != "true" {
					// Explicitly false, or something this guard cannot
					// evaluate. `false` disables nothing; anything else is a
					// shape nobody should be writing here.
					if !ok || id.Name != "false" {
						t.Errorf("%s: %s is set to an expression this guard cannot evaluate. Write it as a literal true, in one place, so that whether verification is on is a fact and not a computation.",
							fset.Position(node.Pos()), skipField)
					}
					return true
				}
				verify, hasVerify := fields[verifyField]
				if !hasVerify {
					t.Errorf("%s: this tls.Config sets %s=true WITHOUT %s. That is not 'pinning with the CA check relaxed', it is NO VERIFICATION AT ALL — it completes handshakes with any certificate and every positive test still passes. Set %s in this same literal (see pinnedTLSConfig).",
						fset.Position(node.Pos()), skipField, verifyField, verifyField)
					return true
				}
				if id, ok := verify.(*ast.Ident); ok && id.Name == "nil" {
					t.Errorf("%s: %s is set to nil alongside %s=true, which verifies nothing.",
						fset.Position(node.Pos()), verifyField, skipField)
					return true
				}
				// Counted BEFORE the cache check. Returning early here would
				// leave paired at zero and fire the terminal "pinning was
				// removed, or it moved somewhere this guard does not look"
				// error as well — which would be flatly false about a file
				// pairing them on the very next line. One mistake, one message.
				paired++

				// A session cache silently retires the callback we just proved
				// is present: crypto/tls does NOT call VerifyPeerCertificate on a
				// RESUMED handshake. Adding one for latency would bypass the pin
				// check and the expiry check on every resumed connection, with
				// every positive test still green — the same silent shape as
				// deleting the callback, wearing a performance argument.
				cache, hasCache := fields[cacheField]
				if id, ok := cache.(*ast.Ident); hasCache && ok && id.Name == "nil" {
					// An EXPLICIT nil disables resumption exactly as omitting the
					// field does, and saying so out loud is if anything safer
					// than silence. Failing it would be a guard that rejects a
					// clearer spelling of the thing it wants.
					hasCache = false
				}
				if hasCache {
					// Only a complaint when the REMEDY is absent. crypto/tls DOES
					// call VerifyConnection on a resumed handshake, so a literal
					// carrying both is the supported way to have resumption and
					// keep the checks. A guard that rejected that would be
					// prescribing a fix and then refusing it.
					conn, hasConn := fields[connField]
					if id, ok := conn.(*ast.Ident); hasConn && ok && id.Name == "nil" {
						// A nil callback is NOT the remedy — crypto/tls skips a
						// nil VerifyConnection, so this shape resumes with no
						// verification whatsoever: the exact bypass this branch
						// exists to close, wearing the remedy's name. The
						// VerifyPeerCertificate branch above already rejects its
						// own nil for the same reason; not doing so here would
						// leave this arm asymmetric with the file's own standard.
						hasConn = false
					}
					if !hasConn {
						t.Errorf("%s: this tls.Config sets %s alongside %s=true but no %s. crypto/tls does NOT call %s on a RESUMED handshake, so a session cache disables the pinned-certificate and expiry checks on every resumed connection — silently, while every positive test still passes (reproduced over live TLS: a resumed connection was accepted while the server served an unpinned certificate). If resumption is genuinely needed, run the same checks from %s, which IS called on resumption.",
							fset.Position(node.Pos()), cacheField, skipField, connField, verifyField, connField)
					}
				}
			}
			return true
		})
	})

	if paired == 0 {
		t.Errorf("no tls.Config literal pairs %s=true with %s. Either pinning was removed (in which case an https bus must now be unreachable, not unverified) or it moved somewhere this guard does not look.",
			skipField, verifyField)
	}
}

// insecureVocabulary is the naming this client must never offer: any flag,
// environment variable or Config field whose name promises to relax, skip or
// disable certificate verification.
//
// It matches on NAMES rather than behaviour on purpose. Behaviour is guarded by
// the two tests above and by the negative tests in pin_test.go; this one closes
// the other half — a knob added "just for local testing" that nothing else
// notices because it is off by default.
//
// `verify` alone is deliberately NOT in it: `agent-busctl whoami --verify`
// authenticates against the bus and is the opposite of this concern.
//
// It is deliberately tight rather than greedy for the same reason: it once
// flagged ErrBusPresentedNoCertificate, and a guard that fails correct work is
// a guard the next agent deletes.
var insecureVocabulary = regexp.MustCompile(
	`(?i)insecure|skip[-_]?verify|no[-_]?verify|noverify|unverified|` +
		`no[-_](tls|cert|check)|plaintext[-_]?ok|` +
		`disable[-_]?(tls|cert|verif)|allow[-_]?(any|self|insecure|unverified)|` +
		`trust[-_]?(any|all)|tofu|` +
		// Pin-specific, added after the reviewer gate observed that
		// --skip-pin, --unpinned and --ignore-fingerprint would all have
		// sailed through a vocabulary written before pinning existed.
		`skip[-_]?pin|unpinned|no[-_]?pin\b|ignore[-_]?(pin|fingerprint|cert)|any[-_]?cert`)

// TestClientHasNoInsecureVerificationFlag asserts there is no way to ASK for
// weakened verification: no CLI flag, no environment variable, and no Config
// field named for it.
//
// Invariant 11 forbids "a flag that does it silently"; this reads that as
// forbidding the flag outright, silent or not. An operator who has been told to
// pass --insecure will pass it, and a documented one is not better than a hidden
// one — it is just a hole with a manual.
func TestClientHasNoInsecureVerificationFlag(t *testing.T) {
	// Prove the matcher can fail before trusting it to pass. A vocabulary
	// regexp that matched nothing would give this test a permanent green.
	for _, bad := range []string{
		"insecure", "tls-skip-verify", "--no-verify", "AllowAnyCertificate", "AGENT_BUS_INSECURE", "trust-any",
		"skip-pin", "unpinned", "no-pin", "ignore-fingerprint", "AcceptAnyCert",
	} {
		if !insecureVocabulary.MatchString(bad) {
			t.Fatalf("insecureVocabulary does not match %q; this guard would pass on a real escape hatch", bad)
		}
	}
	for _, good := range []string{"bus", "bus-fingerprint", "identity", "as", "json", "timeout", "verify", "all", "name", "invite", "keep-current", "idempotency-key", "BusFingerprint", "AGENT_BUS_FINGERPRINT", "ErrBusPresentedNoCertificate", "verifyPinnedBusCertificate"} {
		if insecureVocabulary.MatchString(good) {
			t.Fatalf("insecureVocabulary matches the legitimate name %q; a guard that fails correct work gets deleted", good)
		}
	}

	fset := token.NewFileSet()
	walkGoFiles(t, false, func(path string, src []byte) {
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				// flag registrations: fs.String("name", …), fs.BoolVar(&x, "name", …).
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || !strings.HasPrefix(sel.Sel.Name, "String") && !strings.HasPrefix(sel.Sel.Name, "Bool") &&
					!strings.HasPrefix(sel.Sel.Name, "Int") && !strings.HasPrefix(sel.Sel.Name, "Duration") {
					return true
				}
				for _, arg := range node.Args {
					lit, ok := arg.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					name, err := strconv.Unquote(lit.Value)
					if err != nil || strings.Contains(name, " ") {
						// A usage string, not a flag name.
						continue
					}
					if insecureVocabulary.MatchString(name) {
						t.Errorf("%s: flag or option named %q offers to weaken verification. Invariant 11: never ship a flag that disables certificate verification — a bus that looks secure and is not is worse than no TLS.",
							fset.Position(lit.Pos()), name)
					}
					break
				}
			case *ast.Field:
				// Struct fields (Config and anything else) and their names.
				for _, id := range node.Names {
					if insecureVocabulary.MatchString(id.Name) {
						t.Errorf("%s: field %q offers to weaken verification; there is no supported way to turn the pin check off.",
							fset.Position(id.Pos()), id.Name)
					}
				}
			case *ast.ValueSpec:
				// Environment-variable constants: const EnvFoo = "AGENT_BUS_FOO".
				for i, id := range node.Names {
					if insecureVocabulary.MatchString(id.Name) {
						t.Errorf("%s: identifier %q offers to weaken verification.", fset.Position(id.Pos()), id.Name)
					}
					if i < len(node.Values) {
						if lit, ok := node.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil && insecureVocabulary.MatchString(v) {
								t.Errorf("%s: constant value %q names an escape hatch.", fset.Position(lit.Pos()), v)
							}
						}
					}
				}
			}
			return true
		})
	})
}
