package main

// Tests for MTLS-BUSCERT's WIRING: a running bus must actually produce its
// certificate and its two private keys, must load exactly those files on every
// later start without minting new ones, and must refuse to start over material
// it cannot use.
//
// A FOURTH claim used to live here -- "and must NOT have started serving TLS
// while doing any of it", pinned by TestCmdDoesNotServeTLS. MTLS-LISTENER
// retired it: the listener IS TLS now, so that test is deleted (see the marker
// where it stood) and the guard that replaces it, in the opposite direction,
// lives in tlslisten_test.go.
//
// The remaining claims map one-to-one onto the tests below. Every one of them
// runs the REAL startup path in a subprocess through the harness in
// wal_startup_test.go, because the whole point of this task is that the library
// was complete and unreachable: a test that called buscert.LoadOrCreate itself
// would have passed on the day before this change landed.
//
// The lock-refusal claim ("a start refused at the lock touches nothing but
// bus.lock", which generating key material could easily have broken) is NOT
// re-asserted here. TestRunRefusesALockedDataDir in main_test.go already pins it
// by enumerating the directory, and a second copy would be one more thing to
// keep in step rather than one more thing proved.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/dodgymike/agent-bus/internal/buscert"
)

// The two operator-visible lines this wiring adds. They are contract in the same
// sense as msgWALOpened next door: the assertions below key on them, so renaming
// one in cmd/agent-bus/buscert.go must fail these tests rather than silently
// remove the only notice an operator gets that new key material was minted.
const (
	msgBusCertGenerated = `msg="bus certificate and signing key GENERATED`
	msgBusCertLoaded    = `msg="bus certificate and signing key loaded"`
)

// TestServerGeneratesBusKeyMaterialOnFirstStartAndReusesItAfter is the proof
// that internal/buscert is on the startup path at all.
//
// Before this task `grep -rn internal/buscert --include=*.go .` matched exactly
// one file: buscert's own test. A running bus produced none of these three
// files. The first subtest is what that gap looks like as a failing assertion.
func TestServerGeneratesBusKeyMaterialOnFirstStartAndReusesItAfter(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, buscert.CertFileName)
	tlsKeyPath := filepath.Join(dir, buscert.TLSKeyFileName)
	signingKeyPath := filepath.Join(dir, buscert.SigningKeyFileName)

	var (
		firstFingerprint string
		before           = map[string][]byte{}
	)

	t.Run("first start generates all three files and says so once", func(t *testing.T) {
		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)

		// THE MATERIAL GENERATED ABOVE IS WHAT THE LISTENER SERVES. Since
		// MTLS-LISTENER this is an https request VERIFIED against the certificate
		// this very start wrote into dir (busTestClient loads it as the sole
		// x509 root), so it is not merely "the bus answers": it is the proof that
		// the file on disk and the certificate presented on the handshake are the
		// same certificate. A start that generated one certificate and served
		// another would pass every file assertion below and fail here.
		//
		// This comment used to read "THE LISTENER IS UNCHANGED. This is a
		// PLAINTEXT http:// request" -- true for MTLS-BUSCERT, false the moment
		// MTLS-LISTENER landed, and rewritten rather than left as a stale claim
		// about the security posture of the thing under test.
		mustGetHealthz(t, dir, addr)

		// The files exist, with the modes the on-disk contract states. The two
		// KEYS are 0600 (anything that can read one IS this bus, to a client or
		// to a peer); the CERTIFICATE is 0644 because it is public by
		// construction -- it is sent to every client on every handshake.
		for _, want := range []struct {
			path string
			mode os.FileMode
			why  string
		}{
			{certPath, buscert.CertFileMode, "the certificate is PUBLIC: it is sent on every handshake and its fingerprint is published in invite blobs"},
			{tlsKeyPath, buscert.KeyFileMode, "anything that can read the TLS key IS this bus to every client that pinned its certificate"},
			{signingKeyPath, buscert.KeyFileMode, "anything that can read the signing key can forge this bus's attestations to every PEER bus"},
		} {
			fi, err := os.Stat(want.path)
			if err != nil {
				t.Fatalf("stat %q after a first start: %v; a running bus must PRODUCE its key material, not merely have a library that could", want.path, err)
			}
			if !fi.Mode().IsRegular() {
				t.Fatalf("%q is not a regular file (mode %s)", want.path, fi.Mode())
			}
			if got := fi.Mode().Perm(); got != want.mode.Perm() {
				t.Errorf("%q has mode %#o, want %#o: %s", want.path, got, want.mode.Perm(), want.why)
			}
			before[want.path] = mustReadFile(t, want.path)
		}

		// The announcement fires, and fires ONCE. It is the only notice an
		// operator gets that the fingerprint every client must pin has just come
		// into existence, so a duplicate would be as wrong as a missing one:
		// twice on one directory would mean the material was re-minted.
		if n := countLines(proc, msgBusCertGenerated); n != 1 {
			t.Fatalf("%q appears %d times on a first start, want exactly 1\n%s", msgBusCertGenerated, n, proc.stderr())
		}
		if n := countLines(proc, msgBusCertLoaded); n != 0 {
			t.Errorf("%q appears %d times on a FIRST start, want 0: generation and load are different events and must not both be reported\n%s", msgBusCertLoaded, n, proc.stderr())
		}

		// It must be LOUDER than an ordinary start, exactly as the suffix-floor
		// seal line is on a first start. An INFO here is the defect the
		// suffix-floor work already had to fix once: indistinguishable from a
		// routine boot.
		genLine := proc.line(t, msgBusCertGenerated)
		if lvl := parseLogfmt(genLine)["level"]; lvl != "warn" {
			t.Errorf("the generation line is at level %q, want %q; on a directory that was supposed to already hold key material this line means the material was LOST, and it must not read like a routine start\nline: %s", lvl, "warn", genLine)
		}

		// The announcement must NOT leak key material. The fingerprint and the
		// certificate path are the useful operator-facing facts; a private key,
		// or any PEM at all, in a log line is a secret in a place secrets are
		// shipped, tailed and pasted into tickets.
		for _, line := range proc.snapshot() {
			if strings.Contains(line, "PRIVATE KEY") || strings.Contains(line, "BEGIN ") {
				t.Fatalf("a startup log line looks like it carries PEM key material: %s", line)
			}
		}

		// The fingerprint is reported, and it is the fingerprint OF THE FILE ON
		// DISK -- computed here from the certificate's DER with crypto/sha256
		// rather than read back out of the package that wrote it, so a package
		// that hashed the wrong bytes could not agree with itself into a pass.
		firstFingerprint = parseLogfmt(genLine)["fingerprint"]
		if want := fingerprintOfCertFile(t, certPath); firstFingerprint != want {
			t.Fatalf("the generation line reports fingerprint %q, want %q (sha256 of the DER in %s): clients pin this value", firstFingerprint, want, certPath)
		}

		// The startup summary carries the same value, and states plainly that
		// this bus is serving TLS.
		startedFields := parseLogfmt(proc.line(t, msgServerStarted))
		if got := startedFields["bus_cert_fingerprint"]; got != firstFingerprint {
			t.Errorf("%q reports bus_cert_fingerprint=%q, want %q", msgServerStarted, got, firstFingerprint)
		}
		// tls=true, NOT the tls=false this asserted under MTLS-BUSCERT. The
		// listener is a tls.Listener now (invariant 11: there is no plaintext
		// listener and no flag that makes one), so a summary reporting tls=false
		// would be telling an operator to dial a scheme that cannot connect.
		if got := startedFields["tls"]; got != "true" {
			t.Errorf("%q reports tls=%q, want %q: MTLS-LISTENER serves https and only https, and this line is what an operator reads to learn which scheme to dial", msgServerStarted, got, "true")
		}

		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
		}
	})

	t.Run("second start loads the same material and re-announces nothing", func(t *testing.T) {
		if firstFingerprint == "" {
			t.Skip("the first start did not complete; nothing to reload")
		}

		proc := startServer(t, dir)
		addr := proc.awaitServerStarted(t)
		mustGetHealthz(t, dir, addr)

		// THE LOAD-BEARING ASSERTION OF THIS WHOLE FILE. Re-minting on a restart
		// would break every client that pinned the first fingerprint and would
		// kill the pin held by every peer bus -- and it would look, in a
		// directory listing, exactly like a working bus.
		if n := countLines(proc, msgBusCertGenerated); n != 0 {
			t.Fatalf("%q appears %d times on a SECOND start of the same data directory, want 0: material is generated once per directory and never regenerated\n%s", msgBusCertGenerated, n, proc.stderr())
		}
		if n := countLines(proc, msgBusCertLoaded); n != 1 {
			t.Fatalf("%q appears %d times on a second start, want exactly 1\n%s", msgBusCertLoaded, n, proc.stderr())
		}
		if lvl := parseLogfmt(proc.line(t, msgBusCertLoaded))["level"]; lvl != "info" {
			t.Errorf("the load line is at level %q, want %q: the steady state must not be noisy, or the loud generation line stops standing out", lvl, "info")
		}

		if got := parseLogfmt(proc.line(t, msgBusCertLoaded))["fingerprint"]; got != firstFingerprint {
			t.Fatalf("the second start reports fingerprint %q, want the first start's %q", got, firstFingerprint)
		}

		// Byte-for-byte: not merely "a certificate with the same fingerprint",
		// which a re-mint could never produce anyway, but the same three files
		// untouched -- including the signing key, which has no fingerprint in
		// any log line and would otherwise be unobserved.
		for path, want := range before {
			if got := mustReadFile(t, path); !reflect.DeepEqual(got, want) {
				t.Errorf("%q changed across a restart (%d bytes -> %d bytes); key material is never rewritten", path, len(want), len(got))
			}
		}

		proc.signal(t, syscall.SIGTERM)
		if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
			t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
		}
	})
}

// TestServerRefusesToStartWithUnusableBusKeyMaterial covers the two ways an
// operator meets this code in anger: a key file that is damaged, and a key file
// that is GONE.
//
// Both are FATAL, and neither regenerates anything. That is the same ruling the
// WAL MAC key already carries (internal/wal/mackey.go; DECISIONS.md 2026-08-07):
// silently minting a replacement is not a recovery, it is the loss of every pin
// held by every client and every peer bus, performed automatically and reported
// as a normal start.
func TestServerRefusesToStartWithUnusableBusKeyMaterial(t *testing.T) {
	cases := []struct {
		name string
		// damage mutates a data directory that already holds a full, valid set.
		damage func(t *testing.T, dir string)
		// wantIn are fragments the refusal must contain. The PATH is always one
		// of them: the first question asked of a bus that will not start over its
		// key material is "which file".
		wantIn []string
		// survives are files that must still be on disk afterwards, unchanged.
		survives []string
	}{
		{
			name: "truncated TLS key",
			damage: func(t *testing.T, dir string) {
				path := filepath.Join(dir, buscert.TLSKeyFileName)
				b := mustReadFile(t, path)
				if err := os.WriteFile(path, b[:len(b)/2], buscert.KeyFileMode); err != nil {
					t.Fatalf("truncating %q: %v", path, err)
				}
			},
			wantIn:   []string{"agent-bus: ", "bus certificate and key material", buscert.TLSKeyFileName},
			survives: []string{buscert.CertFileName, buscert.SigningKeyFileName},
		},
		{
			name: "missing signing key",
			damage: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, buscert.SigningKeyFileName)); err != nil {
					t.Fatalf("removing the signing key: %v", err)
				}
			},
			// "incomplete" is buscert.ErrIncomplete's wording, and the message
			// must name what is MISSING and what is PRESENT: the remedy differs
			// (restore from backup, versus remove the survivors by hand after a
			// failed first start) and the operator cannot choose without both.
			wantIn:   []string{"agent-bus: ", "incomplete", buscert.SigningKeyFileName, buscert.CertFileName},
			survives: []string{buscert.CertFileName, buscert.TLSKeyFileName},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			// A real first start, so the damage is applied to material this
			// binary really wrote rather than to a hand-built fixture that could
			// drift from it.
			proc := startServer(t, dir)
			proc.awaitServerStarted(t)
			proc.signal(t, syscall.SIGTERM)
			if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
				t.Fatalf("seeding start exited %d, want 0\n%s", code, proc.stderr())
			}

			survivorsBefore := map[string][]byte{}
			for _, name := range tc.survives {
				survivorsBefore[name] = mustReadFile(t, filepath.Join(dir, name))
			}
			tc.damage(t, dir)

			second := startServer(t, dir)
			if code := second.awaitExit(t, startupTimeout); code != 1 {
				t.Fatalf("exit code with unusable bus key material = %d, want 1\n%s", code, second.stderr())
			}
			out := second.stderr()
			for _, want := range tc.wantIn {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not mention %q\n%s", want, out)
				}
			}

			// Nothing was served. A bus that comes up without usable key material
			// is the half-outage this refusal exists to prevent.
			for _, line := range second.snapshot() {
				if strings.Contains(line, msgServerStarted) {
					t.Fatalf("the server announced itself started despite unusable key material\n%s", out)
				}
			}

			// AND NOTHING WAS REGENERATED. This is the assertion that would catch
			// a well-meaning "repair" being added later: the surviving files are
			// byte-identical, and the missing one is still missing.
			for name, want := range survivorsBefore {
				if got := mustReadFile(t, filepath.Join(dir, name)); !reflect.DeepEqual(got, want) {
					t.Errorf("%q was rewritten by a refused start; a missing or damaged file is NEVER regenerated beside surviving ones", name)
				}
			}
			if tc.name == "missing signing key" {
				if _, err := os.Stat(filepath.Join(dir, buscert.SigningKeyFileName)); !os.IsNotExist(err) {
					t.Errorf("the refused start re-created %q (stat err = %v); regenerating a signing key invalidates the pin held by every peer bus", buscert.SigningKeyFileName, err)
				}
			}
		})
	}
}

// TestCmdDoesNotServeTLS WAS HERE, AND IS DELETED ON PURPOSE (MTLS-LISTENER).
//
// It was a temporary scaffold: an AST guard asserting that cmd/agent-bus
// referenced NONE of ServeTLS/ListenAndServeTLS/tls.NewListener/TLSConfig,
// because MTLS-BUSCERT was generate-and-load only and serving TLS before a
// client could speak it would have locked out every agent at the handshake. Its
// own doc comment said to delete it, in full, in the task that legitimately made
// the listener TLS. That task is this one.
//
// Its replacement is the PERMANENT invariant in the opposite direction:
// TestCmdHasNoPlaintextListener (cmd/agent-bus/tlslisten_test.go) requires
// tls.NewListener to be PRESENT, bans InsecureSkipVerify, and bans any flag that
// would make TLS optional. Deleting this one without adding that one is the
// change to refuse in review.

// TestCertHosts pins which extra subject alternative names the -listen address
// contributes. It is a pure unit test of the one piece of judgement in
// cmd/agent-bus/buscert.go.
func TestCertHosts(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		want   []string
		why    string
	}{
		{
			name:   "loopback default",
			listen: defaultListen,
			want:   []string{"127.0.0.1"},
			why:    "the loopback set is added by buscert regardless; naming it again is harmless and is de-duplicated there",
		},
		{
			name:   "routable address",
			listen: "10.0.0.5:8080",
			want:   []string{"10.0.0.5"},
			why:    "a client verifies the name it DIALLED against the SANs; Go dropped the CommonName fallback in 1.15",
		},
		{
			name:   "dns name",
			listen: "bus.internal:8080",
			want:   []string{"bus.internal"},
			why:    "a hostname bind is dialled by that hostname",
		},
		{
			name:   "wildcard bind, empty host",
			listen: ":8080",
			want:   nil,
			why:    `"every interface" is not a name any client dials`,
		},
		{
			name:   "wildcard bind, 0.0.0.0",
			listen: "0.0.0.0:8080",
			want:   nil,
			why:    "0.0.0.0 as an IP SAN matches nothing a client could dial",
		},
		{
			name:   "wildcard bind, ::",
			listen: "[::]:8080",
			want:   nil,
			why:    ":: as an IP SAN matches nothing a client could dial",
		},
		{
			name:   "unsplittable, already rejected by Config.validate",
			listen: "not-an-address",
			want:   nil,
			why:    "unreachable from run(); a certificate with the loopback set is still correct",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := certHosts(tc.listen); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("certHosts(%q) = %v, want %v: %s", tc.listen, got, tc.want, tc.why)
			}
		})
	}
}

// countLines reports how many captured stderr lines contain want. The COUNT is
// what matters for the generation announcement: "at least once" would not catch
// a second one, and a second one means re-minted key material.
func countLines(p *serverProc, want string) int {
	n := 0
	for _, l := range p.snapshot() {
		if strings.Contains(l, want) {
			n++
		}
	}
	return n
}

// fingerprintOfCertFile computes sha256 over the DER inside a PEM certificate
// file, with crypto/sha256 and encoding/pem directly.
//
// It deliberately does NOT call buscert.FingerprintOf: the claim under test is
// that the value in the log line identifies the certificate ON DISK, and reading
// it back through the same package that produced it would let one wrong
// construction agree with itself.
func fingerprintOfCertFile(t *testing.T, path string) string {
	t.Helper()
	block, rest := pem.Decode(mustReadFile(t, path))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("%q does not hold a PEM CERTIFICATE block", path)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("%q holds more than one PEM block", path)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}
