package main

// Tests for `agent-bus key export-public` (CLI-11).
//
// Two of these carry most of the weight and neither is a routine assertion:
//
//   - TestKeyExportRefusesADirectoryWithNoKeyMaterial asserts the directory is
//     still EMPTY after a refusal, not merely that the exit code was nonzero.
//     buscert.LoadOrCreate MINTS a certificate and two private keys on a
//     directory holding none of the three, so a command that minted and then
//     failed for some later reason would satisfy an exit-code-only test while
//     having just created a bus identity nobody asked for.
//
//   - TestKeyExportNeverPrintsPrivateKeyMaterial asserts the private half never
//     reaches either stream under any flag combination. The public key is
//     DERIVED from the private one and there is no public-only file on disk, so
//     this command necessarily loads the secret in order to print the derived
//     value -- and a leak of that kind is silent, which is exactly why it has to
//     be a test rather than a comment.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
)

// runKeyExport invokes the subcommand and returns its exit code plus both
// streams.
func runKeyExport(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runKeyCommand(append([]string{"export-public"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestKeyExportBusSigningPublicKey is the capability: the command reports the
// PUBLIC half of this bus's signing key in the encoding `peer add` consumes.
func TestKeyExportBusSigningPublicKey(t *testing.T) {
	t.Parallel()

	t.Run("--json reports the key the peer add parser accepts", func(t *testing.T) {
		dir, busID, material := initDataDir(t)

		code, stdout, stderr := runKeyExport(t, "-data-dir", dir, "-json")
		if code != exitKeyOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitKeyOK, stdout, stderr)
		}

		var got keyExportResult
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("--json output is not JSON: %v\ngot: %s", err, stdout)
		}
		if !got.OK {
			t.Error(`a success reported "ok": false`)
		}
		if got.BusID != busID {
			t.Errorf("bus_id = %q, want %q", got.BusID, busID)
		}
		if got.KeyType != "ed25519" {
			t.Errorf("key_type = %q, want %q", got.KeyType, "ed25519")
		}

		// Decoded with the SAME call `agent-bus peer add -signing-key` uses
		// (base64.StdEncoding.DecodeString, cmd/agent-bus/peer.go). Standard
		// base64 with padding, not the URL-safe or raw alphabet: a key printed in
		// a different one is refused by the only command that consumes it.
		decoded, err := base64.StdEncoding.DecodeString(got.PublicKey)
		if err != nil {
			t.Fatalf("public_key %q is not standard base64, so `peer add -signing-key` would refuse it: %v", got.PublicKey, err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			t.Fatalf("public_key decodes to %d bytes, want %d", len(decoded), ed25519.PublicKeySize)
		}
		if !bytes.Equal(decoded, material.SigningPublicKey()) {
			t.Error("public_key is not this data directory's signing public key")
		}
		// peer.go documents "exactly 44 characters (32 raw bytes, standard base64
		// with padding)". Pinned so a switch to RawStdEncoding, which decodes
		// nowhere in this repo, cannot pass unnoticed.
		if len(got.PublicKey) != 44 {
			t.Errorf("public_key is %d characters, want 44 — `peer add` documents 44 for a 32-byte key", len(got.PublicKey))
		}
	})

	t.Run("the key is NOT the TLS fingerprint encoding", func(t *testing.T) {
		// Two 32-byte values live in this workflow -- the signing key and the TLS
		// certificate fingerprint -- and they are distinguishable only by their
		// encoding: base64 for the key, 64 lowercase hex for the fingerprint.
		// Pinning them apart here is cheap; confusing them installs a pin that can
		// never verify anything and reports no error until a relayed message fails.
		dir, _, material := initDataDir(t)

		code, stdout, stderr := runKeyExport(t, "-data-dir", dir, "-json")
		if code != exitKeyOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitKeyOK, stderr)
		}
		var got keyExportResult
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.PublicKey == material.Fingerprint().String() {
			t.Fatal("public_key is the TLS certificate fingerprint, not the signing key")
		}
		if _, err := hex.DecodeString(got.PublicKey); err == nil && len(got.PublicKey) == 64 {
			t.Errorf("public_key %q is 64 hex characters, which is the TLS FINGERPRINT encoding; a signing key is standard base64", got.PublicKey)
		}
	})

	t.Run("human output carries the same key and the command that consumes it", func(t *testing.T) {
		dir, busID, material := initDataDir(t)
		want := base64.StdEncoding.EncodeToString(material.SigningPublicKey())

		code, stdout, stderr := runKeyExport(t, "-data-dir", dir)
		if code != exitKeyOK {
			t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitKeyOK, stderr)
		}
		if !strings.Contains(stdout, want) {
			t.Errorf("human output does not contain the signing public key\ngot: %s", stdout)
		}
		if !strings.Contains(stdout, busID) {
			t.Errorf("human output does not name the bus id %q\ngot: %s", busID, stdout)
		}
		// The operator's next step is to pin it on a peer. Printing the exact
		// command is what keeps them out of the encoding trap above.
		if !strings.Contains(stdout, "peer add") || !strings.Contains(stdout, "-signing-key") {
			t.Errorf("human output does not show the `peer add -signing-key` command that consumes this value\ngot: %s", stdout)
		}
	})
}

// TestKeyExportRefusesADirectoryWithNoKeyMaterial is the load-bearing test for
// this task: an EXPORT command must never MINT.
//
// buscert.LoadOrCreate generates a certificate and two private keys when a data
// directory holds none of the three, and there is no load-only entry point. A
// mint here would be a federation-wide identity event triggered by a read: the
// operator would pin a signing key no bus has ever served, and would find out
// only when a relayed message failed to verify.
//
// EVERY case asserts what is ON DISK afterwards, not just the exit code.
func TestKeyExportRefusesADirectoryWithNoKeyMaterial(t *testing.T) {
	t.Parallel()

	t.Run("a virgin data directory is refused and left COMPLETELY untouched", func(t *testing.T) {
		for _, mode := range [][]string{{}, {"-json"}} {
			name := "human"
			if len(mode) > 0 {
				name = "json"
			}
			t.Run(name, func(t *testing.T) {
				virgin := t.TempDir()
				args := append([]string{"-data-dir", virgin}, mode...)
				code, stdout, stderr := runKeyExport(t, args...)
				if code != exitKeyNoIdentity {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitKeyNoIdentity, stdout, stderr)
				}

				// THE ASSERTION THAT MATTERS. Not "no bus-signing.key": nothing at
				// all. A mint would leave three files; taking the lock before the
				// check would leave a bus.lock, and run() decides a directory "has
				// history" by asking whether it was EMPTY at startup, so even that
				// lone file makes the operator's first server start refuse to boot.
				entries, err := os.ReadDir(virgin)
				if err != nil {
					t.Fatalf("ReadDir: %v", err)
				}
				if len(entries) != 0 {
					var names []string
					for _, e := range entries {
						names = append(names, e.Name())
					}
					t.Fatalf("a refused export wrote %v into a virgin data directory.\n"+
						"It must write NOTHING. If %q is among them this command MINTED a bus identity while\n"+
						"claiming to export one, which is the exact hazard this test exists for.",
						names, buscert.SigningKeyFileName)
				}
				// The same predicate run() branches on has to still say "empty".
				empty, err := dirIsEmpty(virgin)
				if err != nil {
					t.Fatalf("dirIsEmpty: %v", err)
				}
				if !empty {
					t.Error("dirIsEmpty reports the directory is NOT empty after a refused export; the next server start will refuse to boot")
				}
			})
		}
	})

	t.Run("a data directory that does not exist is refused, never created", func(t *testing.T) {
		parent := t.TempDir()
		missing := filepath.Join(parent, "typo")

		code, stdout, stderr := runKeyExport(t, "-data-dir", missing, "-json")
		if code != exitKeyNoIdentity {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitKeyNoIdentity, stdout, stderr)
		}
		if _, err := os.Stat(missing); err == nil {
			t.Fatalf("a mistyped -data-dir %q was CREATED; a typo must never bring a second bus identity into existence", missing)
		}
	})

	t.Run("a partial restore is refused and the lost file is not regenerated", func(t *testing.T) {
		// Removing ONE file is the case buscert reports as ErrIncomplete rather
		// than minting, but the refusal must still come from this command's own
		// check -- before the lock -- so that nothing is written and the message
		// names the missing file.
		for _, missing := range []string{
			buscert.SigningKeyFileName,
			buscert.CertFileName,
			buscert.TLSKeyFileName,
			busIDFileName,
		} {
			missing := missing
			t.Run(missing, func(t *testing.T) {
				dir, _, _ := initDataDir(t)
				victim := filepath.Join(dir, missing)
				if err := os.Remove(victim); err != nil {
					t.Fatalf("removing %s: %v", missing, err)
				}
				before := dirEntryNames(t, dir)

				code, stdout, stderr := runKeyExport(t, "-data-dir", dir, "-json")
				if code != exitKeyNoIdentity {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitKeyNoIdentity, stdout, stderr)
				}
				if _, err := os.Stat(victim); err == nil {
					t.Fatalf("the refusal REGENERATED %s. A regenerated signing key invalidates the pin held by every\n"+
						"peer bus, and a regenerated bus id renames the bus away from its own certificate. It must be\n"+
						"restored from backup, never recreated by this command.", missing)
				}
				if after := dirEntryNames(t, dir); !equalStrings(before, after) {
					t.Errorf("a refused export changed the data directory contents\nbefore: %v\nafter:  %v", before, after)
				}

				var e keyError
				if err := json.Unmarshal([]byte(stdout), &e); err != nil {
					t.Fatalf("--json failure output is not JSON: %v\ngot: %s", err, stdout)
				}
				if e.OK {
					t.Error(`a failure reported "ok": true`)
				}
				if e.ExitCode != exitKeyNoIdentity {
					t.Errorf("exit_code field = %d, want %d", e.ExitCode, exitKeyNoIdentity)
				}
				// "restore", not "start the bus": starting the bus against a
				// directory missing one file does not recreate it either, and
				// against one missing all three it would mint a whole new identity.
				if !strings.Contains(e.Remedy, "restore") {
					t.Errorf("remedy %q does not mention restoring the lost file from backup", e.Remedy)
				}
			})
		}
	})
}

// TestKeyExportNeverPrintsPrivateKeyMaterial pins the safety property in the
// task's title: this exports a PUBLIC key.
//
// It is checked on BOTH streams and across every flag combination, because the
// leak this guards against is not a deliberate print -- it is a helper that
// marshals the whole keypair, or an error string carrying the wrong half.
func TestKeyExportNeverPrintsPrivateKeyMaterial(t *testing.T) {
	t.Parallel()

	dir, _, material := initDataDir(t)
	priv := material.SigningPrivateKey()

	// An ed25519 private key is seed(32) || public(32), so the PUBLIC half is a
	// substring of the private key bytes and is LEGITIMATELY printed. Only
	// secret-specific encodings are searched for: the seed, the full 64-byte
	// value, and the on-disk PEM body. Searching for "any prefix of priv" would
	// fire on the correct output.
	seed := priv.Seed()
	forbidden := map[string]string{
		"the signing key SEED, base64":     base64.StdEncoding.EncodeToString(seed),
		"the signing key SEED, raw base64": base64.RawStdEncoding.EncodeToString(seed),
		"the signing key SEED, hex":        hex.EncodeToString(seed),
		"the full private key, base64":     base64.StdEncoding.EncodeToString(priv),
		"the full private key, hex":        hex.EncodeToString(priv),
	}
	pem, err := os.ReadFile(filepath.Join(dir, buscert.SigningKeyFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", buscert.SigningKeyFileName, err)
	}
	bodyLines := 0
	for i, line := range strings.Split(string(pem), "\n") {
		line = strings.TrimSpace(line)
		// The body lines only; the BEGIN/END armour is not secret and is short
		// enough to appear by coincidence.
		if line == "" || strings.HasPrefix(line, "-----") {
			continue
		}
		// The map key is INDEXED. An earlier version used a constant key, so a
		// multi-line PEM silently searched for only the last line -- which today
		// is every line, because this key is one line, and would have stopped
		// being true the moment the format changed.
		forbidden[fmt.Sprintf("body line %d of %s", i, buscert.SigningKeyFileName)] = line
		bodyLines++
	}
	if bodyLines == 0 {
		t.Fatalf("%s has no PEM body lines, so this test is searching for nothing", buscert.SigningKeyFileName)
	}

	// Every combination an operator or an agent can reach, including the failure
	// paths: a refusal is where a careless error string would carry the material.
	locked, _, _ := initDataDir(t)
	lock, err := dirlock.Acquire(locked)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = lock.Release() }()

	cases := [][]string{
		{"-data-dir", dir},
		{"-data-dir", dir, "-json"},
		{"-json", "-data-dir", dir},
		{"-data-dir", dir, "-json=true"},
		{"-h"},
		{"-data-dir", locked, "-json"},
		{"-data-dir", filepath.Join(t.TempDir(), "absent"), "-json"},
	}
	for _, args := range cases {
		_, stdout, stderr := runKeyExport(t, args...)
		for _, stream := range []struct{ name, body string }{{"stdout", stdout}, {"stderr", stderr}} {
			for what, secret := range forbidden {
				if secret == "" {
					continue
				}
				if strings.Contains(stream.body, secret) {
					// The secret itself is NOT echoed into the failure message.
					t.Fatalf("`key export-public %s` printed %s on %s. This command exports the PUBLIC half only.",
						strings.Join(args, " "), what, stream.name)
				}
			}
		}
	}
}

// TestKeyExportReportsUnusableMaterialAsExitOne pins the exit code that is
// neither "no identity" nor "the bus is running": the material is THERE and
// cannot be used.
//
// It exists because exit 1 was documented in two places with no test behind it,
// and because this is the path most likely to carry key bytes in an error
// string -- the file it is complaining about is the private key.
func TestKeyExportReportsUnusableMaterialAsExitOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{
			name: "the signing key is not a usable PEM",
			corrupt: func(t *testing.T, dir string) {
				path := filepath.Join(dir, buscert.SigningKeyFileName)
				if err := os.WriteFile(path, []byte("not a pem at all\n"), 0o600); err != nil {
					t.Fatalf("corrupting %s: %v", buscert.SigningKeyFileName, err)
				}
			},
		},
		{
			name: "the signing key is world-readable",
			corrupt: func(t *testing.T, dir string) {
				// buscert refuses a key file whose mode is looser than 0600. The
				// bus refuses to start on the same error, which is exactly why
				// this is exit 1 and not exit 4: the material is present, it is
				// the directory's state that is wrong.
				if err := os.Chmod(filepath.Join(dir, buscert.SigningKeyFileName), 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, _, _ := initDataDir(t)
			tc.corrupt(t, dir)

			code, stdout, stderr := runKeyExport(t, "-data-dir", dir, "-json")
			if code != exitKeyFailed {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitKeyFailed, stdout, stderr)
			}
			var e keyError
			if err := json.Unmarshal([]byte(stdout), &e); err != nil {
				t.Fatalf("--json failure output is not JSON: %v\ngot: %s", err, stdout)
			}
			if e.OK {
				t.Error(`a failure reported "ok": true`)
			}
			if e.ExitCode != exitKeyFailed {
				t.Errorf("exit_code field = %d, want %d", e.ExitCode, exitKeyFailed)
			}
			// The failure must name the PATH and nothing from inside the file.
			if !strings.Contains(e.Error, buscert.SigningKeyFileName) {
				t.Errorf("the error does not name %s, so an operator cannot tell which file is wrong: %q", buscert.SigningKeyFileName, e.Error)
			}
		})
	}
}

// TestKeyExportRefusesALockedDataDir pins the third exit code: the data
// directory is held by a live process, which is almost certainly the bus.
func TestKeyExportRefusesALockedDataDir(t *testing.T) {
	t.Parallel()

	dir, _, _ := initDataDir(t)
	lock, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = lock.Release() }()

	code, stdout, stderr := runKeyExport(t, "-data-dir", dir, "-json")
	if code != exitKeyBusRunning {
		t.Fatalf("exit = %d, want %d (the bus is running)\nstdout: %s\nstderr: %s", code, exitKeyBusRunning, stdout, stderr)
	}
	var e keyError
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("--json failure output is not JSON: %v\ngot: %s", err, stdout)
	}
	if e.OK {
		t.Error(`a failure reported "ok": true`)
	}
	if e.ExitCode != exitKeyBusRunning {
		t.Errorf("exit_code field = %d, want %d", e.ExitCode, exitKeyBusRunning)
	}
	// The remedy is the whole reason this code is distinct from 1 and from 4.
	if !strings.Contains(e.Remedy, "stop the bus") {
		t.Errorf("remedy %q does not tell the operator to stop the bus", e.Remedy)
	}
}

// TestKeyExportUsageContract pins the exit codes and streams an agent shelling
// out branches on (invariant 7's second audience).
func TestKeyExportUsageContract(t *testing.T) {
	t.Parallel()

	t.Run("-h is OUTPUT: stdout, exit 0", func(t *testing.T) {
		code, stdout, stderr := runKeyExport(t, "-h")
		if code != exitKeyOK {
			t.Errorf("exit = %d, want %d", code, exitKeyOK)
		}
		if !strings.Contains(stdout, "export-public") {
			t.Errorf("help did not go to stdout\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		if stderr != "" {
			t.Errorf("requested help wrote to stderr: %s", stderr)
		}
	})

	t.Run("a bad flag is a usage error on stderr", func(t *testing.T) {
		code, stdout, _ := runKeyExport(t, "-nonesuch")
		if code != exitKeyUsage {
			t.Errorf("exit = %d, want %d", code, exitKeyUsage)
		}
		if stdout != "" {
			t.Errorf("a usage error wrote to stdout, which a --json consumer would try to parse: %s", stdout)
		}
	})

	t.Run("an unexpected argument is refused and never echoed", func(t *testing.T) {
		nasty := "$(touch /tmp/agent-bus-key-export-should-not-exist)"
		code, stdout, stderr := runKeyExport(t, "-data-dir", t.TempDir(), nasty)
		if code != exitKeyUsage {
			t.Errorf("exit = %d, want %d", code, exitKeyUsage)
		}
		if strings.Contains(stdout+stderr, nasty) {
			t.Error("the unexpected argument was echoed back; it is unvalidated argv on its way to a terminal")
		}
	})

	t.Run("an unknown subcommand is a usage error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := runKeyCommand([]string{"export-private"}, &stdout, &stderr); code != exitKeyUsage {
			t.Errorf("exit = %d, want %d", code, exitKeyUsage)
		}
		if strings.Contains(stdout.String()+stderr.String(), "export-private") {
			t.Error("the unknown subcommand was echoed back; it is unvalidated argv")
		}
		stdout.Reset()
		stderr.Reset()
		if code := runKeyCommand(nil, &stdout, &stderr); code != exitKeyUsage {
			t.Errorf("no subcommand: exit = %d, want %d", code, exitKeyUsage)
		}
	})
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
