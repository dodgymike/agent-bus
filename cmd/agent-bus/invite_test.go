package main

// Tests for `agent-bus invite mint` (INVITE-MINT).
//
// The subcommand is exercised END TO END against a real data directory, a real
// dirlock and a real write-ahead log — no stubs. The point of the feature is
// that a minted invite is DURABLE and that the values in the blob are the ones
// a client will actually pin, and neither of those can be proved against a fake.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// initDataDir builds a data directory holding a bus identity, the way a first
// server start would leave it: a bus id file and the three key-material files.
// It returns the directory, the bus id and the loaded material.
func initDataDir(t *testing.T) (string, string, *buscert.Material) {
	t.Helper()
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, "bus-minttest")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}
	material, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: busID, Hosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("buscert.LoadOrCreate: %v", err)
	}
	if !material.Generated() {
		t.Fatalf("buscert.LoadOrCreate did not generate material in a fresh dir")
	}
	return dir, busID, material
}

// runMint invokes the subcommand and returns its exit code plus both streams.
func runMint(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runInviteCommand(append([]string{"mint"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestInviteMintSubcommand is the operator-facing half of INVITE-MINT: the
// subcommand mints a durable, single-use, expiring invite and reports the four
// trust-anchor values.
func TestInviteMintSubcommand(t *testing.T) {
	t.Parallel()

	t.Run("mints a durable invite and reports the blob", func(t *testing.T) {
		dir, busID, material := initDataDir(t)

		code, stdout, stderr := runMint(t,
			"-data-dir", dir,
			"-bus-address", "https://bus.example:8443",
			"-ttl", "2h",
			"-label", "for the deploy runner",
			"-json")
		if code != exitInviteOK {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteOK, stdout, stderr)
		}

		var blob inviteBlob
		if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
			t.Fatalf("--json output is not one JSON object: %v\ngot: %s", err, stdout)
		}
		if !blob.OK {
			t.Error(`--json success output has "ok": false`)
		}

		// INVARIANT 1: the id is the server's.
		if !regexp.MustCompile(invite.InviteIDPattern).MatchString(blob.InviteID) {
			t.Errorf("invite_id %q does not match %s", blob.InviteID, invite.InviteIDPattern)
		}

		// The four values that make this blob the TRUST ANCHOR (DECISIONS.md E6).
		if blob.BusID != busID {
			t.Errorf("bus_id = %q, want %q", blob.BusID, busID)
		}
		if blob.BusAddress != "https://bus.example:8443" {
			t.Errorf("bus_address = %q, want the -bus-address value", blob.BusAddress)
		}
		if want := material.Fingerprint().String(); blob.BusCertFingerprint != want {
			t.Errorf("bus_cert_fingerprint = %q, want %q (sha256 of this data directory's certificate DER)", blob.BusCertFingerprint, want)
		}
		if blob.InviteSecret == "" {
			t.Fatal("invite_secret is empty")
		}

		// The secret is 32 bytes of crypto/rand in base64.RawURLEncoding.
		raw, err := base64.RawURLEncoding.DecodeString(blob.InviteSecret)
		if err != nil {
			t.Fatalf("invite_secret is not base64.RawURLEncoding: %v", err)
		}
		if len(raw) != invite.SecretBytes {
			t.Errorf("invite_secret decodes to %d bytes, want %d", len(raw), invite.SecretBytes)
		}

		// The TTL is honoured, not silently defaulted.
		exp, err := time.Parse(time.RFC3339Nano, blob.ExpiresAt)
		if err != nil {
			t.Fatalf("expires_at %q is not RFC3339Nano: %v", blob.ExpiresAt, err)
		}
		created, err := time.Parse(time.RFC3339Nano, blob.CreatedAt)
		if err != nil {
			t.Fatalf("created_at %q is not RFC3339Nano: %v", blob.CreatedAt, err)
		}
		if got := exp.Sub(created); got != 2*time.Hour {
			t.Errorf("expires_at - created_at = %s, want 2h (the requested -ttl)", got)
		}
		if blob.Label != "for the deploy runner" {
			t.Errorf("label = %q, want the -label value", blob.Label)
		}

		// DURABILITY IS THE FEATURE. Reopen the data directory in a second
		// process-shaped pass — a fresh store, a fresh replay — and the invite must
		// be there and OPEN. Without this the command could be printing a secret
		// it never wrote, and every other assertion here would still pass.
		reopened := replayInvites(t, dir, busID)
		rec, ok := reopened.Lookup(blob.InviteID)
		if !ok {
			t.Fatalf("invite %s is NOT in the table after replay; the mint was not durable", blob.InviteID)
		}
		if rec.State != invite.StateOpen {
			t.Errorf("replayed invite state = %s, want %s", rec.State, invite.StateOpen)
		}
		if rec.BusID != busID {
			t.Errorf("replayed invite bus id = %q, want %q", rec.BusID, busID)
		}
		// The replayed digest must verify the secret that was PRINTED. If it did
		// not, the invite on disk would be unredeemable and only a redemption test
		// would ever notice.
		if !invite.VerifySecret(blob.InviteSecret, rec.SecretDigest) {
			t.Error("the printed secret does not verify against the digest that survived replay; the invite is unredeemable")
		}

		// THE PLAINTEXT SECRET IS NEVER DURABLE.
		walBytes, err := os.ReadFile(filepath.Join(dir, "bus.wal"))
		if err != nil {
			t.Fatalf("reading the WAL: %v", err)
		}
		if bytes.Contains(walBytes, []byte(blob.InviteSecret)) {
			t.Error("the PLAINTEXT invite secret is in the write-ahead log; only its SHA-256 digest may be durable, and the log is append-only so it could never be redacted")
		}
		if !bytes.Contains(walBytes, []byte(blob.InviteID)) {
			t.Fatal("the invite id is not in the WAL, so the secret-absence check above proves nothing")
		}

		// stderr must not leak the credential either.
		if strings.Contains(stderr, blob.InviteSecret) {
			t.Error("the invite secret appears on stderr; it belongs on stdout only, where --json puts it deliberately")
		}
	})

	t.Run("the fingerprint is exactly what a pinning client parses", func(t *testing.T) {
		// The blob's fingerprint is the value a client later pins. If this
		// encoding and client.ParseBusFingerprint's disagreed, the client would
		// reject every genuine bus — silently, and fatally, because a pin
		// mismatch is indistinguishable from the attack pinning exists to catch.
		//
		// Asserted as the PROPERTY (64 LOWERCASE hex characters, sha256 of the
		// leaf DER) rather than by importing the client package, which
		// cmd/agent-bus deliberately does not depend on. client/pin.go's
		// ParseBusFingerprint requires exactly this: hex.EncodedLen(32) length,
		// hex-decodable, and byte-identical to its own lowercase re-encoding.
		dir, _, material := initDataDir(t)
		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteOK {
			t.Fatalf("exit = %d: %s %s", code, stdout, stderr)
		}
		var blob inviteBlob
		if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		fp := blob.BusCertFingerprint
		if want := hex.EncodedLen(buscert.DigestSize); len(fp) != want {
			t.Errorf("bus_cert_fingerprint is %d characters, want exactly %d", len(fp), want)
		}
		if fp != strings.ToLower(fp) {
			t.Errorf("bus_cert_fingerprint %q is not LOWERCASE; a pinning client rejects uppercase rather than normalising it", fp)
		}
		if _, err := hex.DecodeString(fp); err != nil {
			t.Errorf("bus_cert_fingerprint is not hexadecimal: %v", err)
		}
		// And it round-trips through the one shared parser.
		parsed, err := buscert.ParseFingerprint(fp)
		if err != nil {
			t.Fatalf("buscert.ParseFingerprint(%q): %v", fp, err)
		}
		if !parsed.Equal(material.Fingerprint()) {
			t.Error("the parsed fingerprint is not this bus's certificate fingerprint")
		}
	})

	t.Run("every invite is distinct", func(t *testing.T) {
		dir, _, _ := initDataDir(t)
		seenID, seenSecret := map[string]bool{}, map[string]bool{}
		for i := 0; i < 5; i++ {
			code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-json")
			if code != exitInviteOK {
				t.Fatalf("mint %d: exit %d: %s %s", i, code, stdout, stderr)
			}
			var blob inviteBlob
			if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
				t.Fatalf("mint %d: unmarshal: %v", i, err)
			}
			if seenID[blob.InviteID] {
				t.Errorf("mint %d reissued invite id %q", i, blob.InviteID)
			}
			if seenSecret[blob.InviteSecret] {
				t.Errorf("mint %d reissued a secret", i)
			}
			seenID[blob.InviteID] = true
			seenSecret[blob.InviteSecret] = true
		}
	})

	t.Run("human output names the secret once and warns about it", func(t *testing.T) {
		dir, busID, material := initDataDir(t)
		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443")
		if code != exitInviteOK {
			t.Fatalf("exit = %d: %s %s", code, stdout, stderr)
		}
		// The default output is for a HUMAN (invariant 7's first audience): it
		// must carry all four trust-anchor values, and it must say the secret is
		// not recoverable — an operator who loses it has to revoke and re-mint.
		for _, want := range []string{busID, material.Fingerprint().String(), "https://bus.example:8443", "SINGLE USE", "not recoverable"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("human output does not mention %q:\n%s", want, stdout)
			}
		}
		if strings.Contains(stdout, "{") {
			t.Error("the default output looks like JSON; --json is opt-in")
		}
	})

	t.Run("an http bus address warns that the secret crosses the wire in clear", func(t *testing.T) {
		dir, _, _ := initDataDir(t)
		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "http://127.0.0.1:8080", "-json", "-log-level", "warn")
		if code != exitInviteOK {
			t.Fatalf("exit = %d: %s %s", code, stdout, stderr)
		}
		// Invariant 11: TLS is the required transport, and until MTLS-LISTENER
		// lands the fingerprint in this blob pins nothing. An operator must not
		// have to infer that.
		if !strings.Contains(stderr, "CLEARTEXT") {
			t.Errorf("no cleartext warning on stderr for an http bus address:\n%s", stderr)
		}
		// AND IN BAND. Raised by the security gate (LOW-2): an agent shelling out
		// with --json and stderr discarded is invariant 7's second audience, and a
		// warning it cannot see is a warning that does not exist. It must be able
		// to branch on a field.
		var blob inviteBlob
		if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !blob.TransportInsecure {
			t.Error("transport_insecure is not set for an http:// bus address; an agent consuming --json with stderr discarded has no way to learn its pin is inert")
		}

		// The converse: an https address must NOT carry the flag, and because it
		// is omitempty the key must be absent entirely rather than false.
		dir2, _, _ := initDataDir(t)
		code, stdout, _ = runMint(t, "-data-dir", dir2, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteOK {
			t.Fatalf("exit = %d", code)
		}
		if strings.Contains(stdout, "transport_insecure") {
			t.Errorf("an https invite carries transport_insecure; it is omitempty so the flag is only ever a positive assertion of risk:\n%s", stdout)
		}
	})

	t.Run("a hostile label cannot reach the terminal raw", func(t *testing.T) {
		// Raised by the security gate (LOW-1). The label is operator argv,
		// length-bounded but not charset-validated, and it is DURABLE — so raw
		// ANSI would replay out of a future `invite list` too.
		dir, _, _ := initDataDir(t)
		const nasty = "ops\x1b[31mRED\x07\nfake line"
		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-label", nasty)
		if code != exitInviteOK {
			t.Fatalf("exit = %d: %s %s", code, stdout, stderr)
		}
		for _, raw := range []string{"\x1b", "\x07"} {
			if strings.Contains(stdout, raw) {
				t.Errorf("the human output contains a raw control byte %q from -label; render it with %%q", raw)
			}
		}
		// It must still be REPORTED, escaped — suppressing it entirely would hide
		// what the operator actually recorded.
		if !strings.Contains(stdout, "RED") {
			t.Errorf("the label is not shown at all:\n%s", stdout)
		}

		// --json is safe by construction (encoding/json escapes < 0x20), and the
		// round trip must return the label verbatim.
		code, stdout, _ = runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-label", nasty, "-json")
		if code != exitInviteOK {
			t.Fatalf("exit = %d", code)
		}
		if strings.Contains(stdout, "\x1b") {
			t.Error("--json output contains a raw escape byte")
		}
		var blob inviteBlob
		if err := json.Unmarshal([]byte(stdout), &blob); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if blob.Label != nasty {
			t.Errorf("label round-tripped as %q, want %q", blob.Label, nasty)
		}
	})

	t.Run("a refusal on a virgin data directory leaves it COMPLETELY untouched", func(t *testing.T) {
		// REGRESSION TEST for a defect this command shipped with for about an
		// hour, found on its first end-to-end run and not by any unit test.
		//
		// dirlock.Acquire CREATES bus.lock. The first version of mintInvite took
		// the lock BEFORE checking whether the directory held a bus identity, so
		// the natural bootstrap mistake — mint first, then start the bus — left a
		// lone bus.lock behind. run() decides whether a data directory "has
		// history" by asking whether it was EMPTY at startup (dirIsEmpty), so the
		// operator's very FIRST `agent-bus` start then refused to boot:
		// openSuffixAllocator saw a non-empty directory with no agent-suffixes
		// file and demanded -backfill-suffix-floors. A mint that cannot run must
		// not be able to wedge the directory it could not run against.
		//
		// The assertion is deliberately "the directory is still EMPTY", not "no
		// bus.lock", because emptiness is the exact predicate run() branches on.
		virgin := t.TempDir()
		code, stdout, stderr := runMint(t, "-data-dir", virgin, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteNoIdentity {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteNoIdentity, stdout, stderr)
		}
		entries, err := os.ReadDir(virgin)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) != 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("a refused mint wrote %v into a virgin data directory.\n"+
				"It must write NOTHING: run() treats a non-empty directory as one with HISTORY, so anything left here\n"+
				"makes the operator's first server start refuse to boot and demand -backfill-suffix-floors.", names)
		}

		// And the directory must still be usable: the same predicate run() uses
		// has to still say "empty".
		empty, err := dirIsEmpty(virgin)
		if err != nil {
			t.Fatalf("dirIsEmpty: %v", err)
		}
		if !empty {
			t.Error("dirIsEmpty reports the directory is NOT empty after a refused mint; the next server start will refuse to boot")
		}
	})

	t.Run("a lost bus-id file is REFUSED, never regenerated", func(t *testing.T) {
		// REGRESSION TEST for reviewer finding P1-1, reproduced end to end before
		// the fix.
		//
		// ids.LoadOrCreateBusID CREATES a bus id when the file is absent. The
		// first version of this command stat'd only the certificate, so a data
		// directory whose bus-id file had been lost — a partial restore, a stray
		// rm — got a FRESHLY MINTED bus id persisted into it, and only then did
		// the CommonName cross-check notice the mismatch and refuse. The refusal
		// left the bus PERMANENTLY RENAMED away from its own certificate, and
		// run() has no such cross-check, so the next start adopted the new id
		// happily. Every agent id the bus had issued ("<bus-id>.<agent-id>",
		// invariant 2) then named a bus that no longer existed.
		dir, busID, _ := initDataDir(t)
		if err := os.Remove(filepath.Join(dir, busIDFileName)); err != nil {
			t.Fatalf("removing the bus id file: %v", err)
		}

		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteNoIdentity {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteNoIdentity, stdout, stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, busIDFileName)); err == nil {
			regenerated, _ := os.ReadFile(filepath.Join(dir, busIDFileName))
			t.Fatalf("the refusal REGENERATED the bus id file as %q (the bus is %q).\n"+
				"A regenerated bus id renames the bus away from its own certificate, and every agent id it\n"+
				"ever issued then names a bus that does not exist. The file must be restored from backup,\n"+
				"never recreated by this command.", regenerated, busID)
		}
		// The remedy must say "restore", not "start the bus" — starting the bus
		// would recreate the file with a NEW id and make the damage permanent.
		var e inviteError
		if err := json.Unmarshal([]byte(stdout), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !strings.Contains(e.Remedy, "restore") {
			t.Errorf("remedy %q does not mention restoring the lost file from backup", e.Remedy)
		}
	})

	t.Run("refuses a locked data directory with exit 3", func(t *testing.T) {
		dir, _, _ := initDataDir(t)
		lock, err := dirlock.Acquire(dir)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer func() { _ = lock.Release() }()

		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteBusRunning {
			t.Fatalf("exit = %d, want %d (the bus is running)\nstdout: %s\nstderr: %s", code, exitInviteBusRunning, stdout, stderr)
		}
		var e inviteError
		if err := json.Unmarshal([]byte(stdout), &e); err != nil {
			t.Fatalf("--json failure output is not JSON: %v\ngot: %s", err, stdout)
		}
		if e.OK {
			t.Error(`a failure reported "ok": true`)
		}
		if e.ExitCode != exitInviteBusRunning {
			t.Errorf("exit_code field = %d, want %d", e.ExitCode, exitInviteBusRunning)
		}
		// The remedy is the whole reason this code is distinct from 1 and from 4.
		if !strings.Contains(e.Remedy, "stop the bus") {
			t.Errorf("remedy %q does not tell the operator to stop the bus", e.Remedy)
		}
		// NOTHING may have been minted, and the refusal must not have touched the
		// log at all: a mint refused AT THE LOCK has the same obligation the
		// server's own lock refusal has (TestRunRefusesALockedDataDir).
		if got := walInviteIDs(t, dir); len(got) != 0 {
			t.Errorf("a mint refused at the lock still wrote invite records: %v", got)
		}
	})

	t.Run("refuses a data directory with no bus identity with exit 4", func(t *testing.T) {
		empty := t.TempDir()
		code, stdout, stderr := runMint(t, "-data-dir", empty, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteNoIdentity {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteNoIdentity, stdout, stderr)
		}
		// AND it must not have created one. A mint that generated a bus identity
		// on a typo'd -data-dir would hand the operator an invite pinning a
		// certificate no running bus serves.
		for _, name := range []string{buscert.CertFileName, buscert.TLSKeyFileName, buscert.SigningKeyFileName, "bus.wal"} {
			if _, err := os.Stat(filepath.Join(empty, name)); err == nil {
				t.Errorf("the refused mint created %s; it must never create a bus identity", name)
			}
		}
	})

	t.Run("refuses a missing -data-dir with exit 4", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "typo")
		code, _, _ := runMint(t, "-data-dir", missing, "-bus-address", "https://bus.example:8443", "-json")
		if code != exitInviteNoIdentity {
			t.Fatalf("exit = %d, want %d", code, exitInviteNoIdentity)
		}
		if _, err := os.Stat(missing); err == nil {
			t.Error("the mint created the missing -data-dir; it must never create one")
		}
	})

	t.Run("refuses a TTL over the maximum rather than clamping it", func(t *testing.T) {
		dir, _, _ := initDataDir(t)
		code, stdout, stderr := runMint(t, "-data-dir", dir, "-bus-address", "https://bus.example:8443", "-ttl", "1000h", "-json")
		if code != exitInviteFailed {
			t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteFailed, stdout, stderr)
		}
		if len(walInviteIDs(t, dir)) != 0 {
			t.Error("a refused over-long TTL still minted an invite")
		}
	})

	t.Run("unknown subcommand and bad flags exit 2", func(t *testing.T) {
		dir, _, _ := initDataDir(t)
		cases := []struct {
			name string
			args []string
		}{
			{"no subcommand", nil},
			{"unknown subcommand", []string{"revoke"}},
			{"missing -bus-address", []string{"mint", "-data-dir", dir}},
			{"empty -bus-address", []string{"mint", "-data-dir", dir, "-bus-address", "  "}},
			{"no scheme", []string{"mint", "-data-dir", dir, "-bus-address", "bus.example:8443"}},
			{"bad scheme", []string{"mint", "-data-dir", dir, "-bus-address", "ftp://bus.example"}},
			{"http to a non-loopback host", []string{"mint", "-data-dir", dir, "-bus-address", "http://bus.example:8080"}},
			{"userinfo", []string{"mint", "-data-dir", dir, "-bus-address", "https://user:pw@bus.example"}},
			{"query", []string{"mint", "-data-dir", dir, "-bus-address", "https://bus.example?a=b"}},
			{"unknown flag", []string{"mint", "-data-dir", dir, "-bus-address", "https://bus.example", "-nope"}},
			{"positional argument", []string{"mint", "-data-dir", dir, "-bus-address", "https://bus.example", "extra"}},
			{"bad log level", []string{"mint", "-data-dir", dir, "-bus-address", "https://bus.example", "-log-level", "shout"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				if code := runInviteCommand(tc.args, &stdout, &stderr); code != exitInviteUsage {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitInviteUsage, stdout.String(), stderr.String())
				}
			})
		}
		// No usage error may have minted anything.
		if got := walInviteIDs(t, dir); len(got) != 0 {
			t.Errorf("a usage error minted invites: %v", got)
		}
	})

	t.Run("help exits 0 on STDOUT, at every level", func(t *testing.T) {
		// Requested help is OUTPUT and belongs on stdout; only an ERROR goes to
		// stderr. `invite mint -h` printed to stderr at first while `invite -h`
		// printed to stdout — raised by the reviewer, and it matters because the
		// server's own -h now tells operators to run `invite mint -h`.
		cases := [][]string{{"-h"}, {"--help"}, {"help"}, {"mint", "-h"}, {"mint", "--help"}}
		for _, args := range cases {
			var stdout, stderr bytes.Buffer
			if code := runInviteCommand(args, &stdout, &stderr); code != exitInviteOK {
				t.Fatalf("%v: exit = %d, want 0", args, code)
			}
			help := stdout.String()
			if help == "" {
				t.Fatalf("%v: help went nowhere on stdout (stderr: %q)", args, stderr.String())
			}
			// An agent shelling out branches on these; they are a contract.
			for _, want := range []string{"EXIT CODES", "-bus-address", "-json", "MUST NOT BE RUNNING"} {
				if !strings.Contains(help, want) {
					t.Errorf("%v help does not mention %q", args, want)
				}
			}
		}
	})
}

// TestInviteMintBusIDFileNameMatchesIDsPackage makes the duplicated file-name
// constant self-checking.
//
// cmd/agent-bus needs to ask "does this directory already hold a bus id?", and
// ids.LoadOrCreateBusID cannot answer that without answering it BY CREATING ONE
// — which is the exact behaviour the check exists to prevent. So the name is
// duplicated from internal/ids/busid.go's unexported busIDFileName. If that is
// ever renamed, this fails loudly rather than letting the mint command silently
// conclude that every data directory lacks a bus id and refuse them all.
func TestInviteMintBusIDFileNameMatchesIDsPackage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, "bus-namecheck")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}
	path := filepath.Join(dir, busIDFileName)
	got, err := os.ReadFile(path)
	if err != nil {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("ids.LoadOrCreateBusID did not write %q; the directory holds %v.\n"+
			"cmd/agent-bus's busIDFileName has drifted from internal/ids's, so `invite mint` would refuse "+
			"every data directory as having no bus id.", busIDFileName, names)
	}
	if strings.TrimSpace(string(got)) != busID {
		t.Errorf("%s holds %q, want the bus id %q", path, strings.TrimSpace(string(got)), busID)
	}
}

// TestInviteMintBusAddressRules pins parseInviteBusAddress against the rule
// client.parseBusURL applies, since the two must agree or a blob carries an
// address the client refuses.
func TestInviteMintBusAddressRules(t *testing.T) {
	t.Parallel()
	ok := []struct{ in, want string }{
		{"https://bus.example:8443", "https://bus.example:8443"},
		{"https://bus.example", "https://bus.example"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://localhost:8080", "http://localhost:8080"},
		{"http://[::1]:8080", "http://[::1]:8080"},
		{"https://bus.example:8443/prefix/", "https://bus.example:8443/prefix"},
		{"  https://bus.example:8443  ", "https://bus.example:8443"},
		// CANONICALISATION, raised by the security gate (LOW-3): the client uses
		// this string as an idempotency SCOPE KEY, so two spellings of one bus
		// are two scopes. These are the cases that distinguished this validator
		// from client.parseBusURL before it mirrored canonicalHost.
		{"https://BUS.Example:8443", "https://bus.example:8443"},
		{"https://bus.example:443", "https://bus.example"},
		{"http://LOCALHOST:80", "http://localhost"},
		{"HTTPS://bus.example", "https://bus.example"},
		{"https://BUS.example", "https://bus.example"},
		// IPv6 KEEPS ITS BRACKETS when the default port is dropped. Raised by the
		// security gate: net.SplitHostPort strips them, so the naive form emits
		// "http://::1" — not a parseable URL host, i.e. an UNDIALLABLE address in
		// the trust anchor. It fails closed rather than open, but the blob is the
		// one place an unusable value is most expensive.
		{"http://[::1]:80", "http://[::1]"},
		{"https://[2001:DB8::1]:443", "https://[2001:db8::1]"},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"http://[::1]:8080", "http://[::1]:8080"},
	}
	for _, tc := range ok {
		got, err := parseInviteBusAddress(tc.in)
		if err != nil {
			t.Errorf("parseInviteBusAddress(%q) = error %v, want %q", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("parseInviteBusAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	bad := []string{
		"",
		"   ",
		"bus.example:8443",
		"ftp://bus.example",
		"https://",
		"http://bus.example",   // plaintext to a non-loopback host
		"http://10.0.0.5:8080", // ditto, IP form
		"https://u:p@bus.example",
		"https://bus.example?x=1",
		"https://bus.example#frag",
	}
	for _, in := range bad {
		if got, err := parseInviteBusAddress(in); err == nil {
			t.Errorf("parseInviteBusAddress(%q) = %q, want an error", in, got)
		}
	}
}

// replayInvites reopens the data directory's log with a fresh invite store as
// its applier, which is what a restarting bus would do, and returns the
// rebuilt table.
func replayInvites(t *testing.T, dir, busID string) *invite.Store {
	t.Helper()
	lg := logging.New(&bytes.Buffer{}, logging.LevelError)
	dl := &deferredLog{}
	st, err := invite.NewStore(invite.StoreOptions{BusID: busID, Durable: dl, Logger: lg})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	log, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open for replay: %v", err)
	}
	dl.log = log
	t.Cleanup(func() { _ = log.Close() })
	return st
}

// walInviteIDs returns every invite id present in the log, by replaying it. It
// is how the "nothing was written" assertions are made positively rather than
// by the absence of an error.
func walInviteIDs(t *testing.T, dir string) []string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "bus.wal")); err != nil {
		return nil
	}
	var found []string
	lg := logging.New(&bytes.Buffer{}, logging.LevelError)
	log, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: applierFunc(func(c wal.Committed) error {
		if c.Entry.Kind != invite.RecordKind {
			return nil
		}
		rec, err := invite.DecodeRecord(c.Entry.Body)
		if err != nil {
			t.Errorf("an invite record in the log will not decode: %v", err)
			return nil
		}
		found = append(found, rec.ID)
		return nil
	})})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	_ = log.Close()
	return found
}

type applierFunc func(wal.Committed) error

func (f applierFunc) Apply(c wal.Committed) error { return f(c) }
