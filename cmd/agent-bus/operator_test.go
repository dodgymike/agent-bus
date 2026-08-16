package main

// AUTH-10, the CLI half: `agent-bus operator keygen|add|list|revoke`.
//
// Invariant 7 is why these exist at all — every capability ships with its
// subcommand in the SAME task, and a capability without one is the missing half
// of the task. Each test drives runOperatorCommand exactly as an operator's
// shell would, including the --json shapes an agent shelling out branches on.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// operatorDataDir builds a data directory holding a bus identity, the way a
// first server start would leave it.
func operatorDataDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	busID, err := ids.LoadOrCreateBusID(dir, "bus-optest")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID: %v", err)
	}
	if _, err := buscert.LoadOrCreate(dir, buscert.Options{BusID: busID, Hosts: []string{"127.0.0.1"}}); err != nil {
		t.Fatalf("buscert.LoadOrCreate: %v", err)
	}
	return dir, busID
}

// runOperator invokes the subcommand and returns its exit code plus both
// streams.
func runOperator(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runOperatorCommand(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// operatorCredential mints the two PUBLIC values `operator add` takes, the way
// `operator keygen` would print them.
func operatorCredential(t *testing.T, fpByte byte) (authPub string, certFP string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	var fp buscert.Fingerprint
	for i := range fp {
		fp[i] = fpByte
	}
	return base64.StdEncoding.EncodeToString(pub), fp.String()
}

// decodeOperatorJSON parses a --json result object off stdout.
func decodeOperatorJSON(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\ngot: %q", err, out)
	}
	return m
}

// TestOperatorAddListRevokeEndToEnd is the whole operator lifecycle through the
// CLI, against a temp data directory: add, list, revoke, list -all.
func TestOperatorAddListRevokeEndToEnd(t *testing.T) {
	dir, busID := operatorDataDir(t)
	authPub, certFP := operatorCredential(t, 0x11)

	// --- add ---
	code, stdout, stderr := runOperator(t, "add", "-data-dir", dir,
		"-name", "ops", "-auth-pub", authPub, "-cert-fingerprint", certFP,
		"-label", "mike, laptop", "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator add exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	added := decodeOperatorJSON(t, stdout)
	if added["ok"] != true {
		t.Fatalf("operator add ok = %v, want true", added["ok"])
	}
	if added["bus_id"] != busID {
		t.Fatalf("operator add bus_id = %v, want %q", added["bus_id"], busID)
	}
	ops, _ := added["operators"].([]interface{})
	if len(ops) != 1 {
		t.Fatalf("operator add returned %d operators, want 1", len(ops))
	}
	rec, _ := ops[0].(map[string]interface{})
	operatorID, _ := rec["operator_id"].(string)
	if !auth.IsOperatorID(operatorID) {
		t.Fatalf("the minted id %q is not a well-formed operator id", operatorID)
	}
	if !strings.HasPrefix(operatorID, "op:"+busID+".ops-") {
		t.Fatalf("the minted id %q is not scoped to bus %q and name \"ops\"", operatorID, busID)
	}
	if rec["cert_fingerprint"] != certFP || rec["auth_pub"] != authPub {
		t.Fatalf("the record round trip changed the credential: %v", rec)
	}
	// NOTHING SECRET IS PRINTED: the operator already holds its own private
	// halves and the bus never had them.
	if strings.Contains(stdout, "PRIVATE") {
		t.Fatalf("operator add printed something that looks like key material:\n%s", stdout)
	}

	// --- list (live only) ---
	code, stdout, stderr = runOperator(t, "list", "-data-dir", dir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator list exit = %d, want 0\nstderr: %s", code, stderr)
	}
	listed := decodeOperatorJSON(t, stdout)
	if got := len(listed["operators"].([]interface{})); got != 1 {
		t.Fatalf("operator list returned %d operators, want 1", got)
	}

	// --- revoke ---
	code, stdout, stderr = runOperator(t, "revoke", "-data-dir", dir, "-id", operatorID, "-reason", "laptop stolen", "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator revoke exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	revoked := decodeOperatorJSON(t, stdout)
	if revoked["unchanged"] == true {
		t.Fatal("the first revocation reported unchanged=true")
	}
	revRec := revoked["operators"].([]interface{})[0].(map[string]interface{})
	if revRec["revoked_at"] == nil || revRec["revoked_reason"] != "laptop stolen" {
		t.Fatalf("the revocation record is missing its instant or reason: %v", revRec)
	}

	// --- re-revoke is a legitimate retry: success, unchanged, nothing written ---
	code, stdout, stderr = runOperator(t, "revoke", "-data-dir", dir, "-id", operatorID, "-reason", "a different reason", "-json")
	if code != exitOperatorOK {
		t.Fatalf("re-revoke exit = %d, want 0 (invariant 10: same key + same payload is a legitimate retry)\nstderr: %s", code, stderr)
	}
	again := decodeOperatorJSON(t, stdout)
	if again["unchanged"] != true {
		t.Fatalf("re-revoke unchanged = %v, want true", again["unchanged"])
	}
	againRec := again["operators"].([]interface{})[0].(map[string]interface{})
	if againRec["revoked_reason"] != "laptop stolen" {
		t.Fatalf("re-revoke rewrote the original reason to %v; a retry returns the ORIGINAL result", againRec["revoked_reason"])
	}

	// --- list hides it; list -all shows it with the reason ---
	code, stdout, _ = runOperator(t, "list", "-data-dir", dir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator list after revocation exit = %d", code)
	}
	if got := len(decodeOperatorJSON(t, stdout)["operators"].([]interface{})); got != 0 {
		t.Fatalf("operator list showed %d LIVE operators after revocation, want 0", got)
	}
	code, stdout, _ = runOperator(t, "list", "-data-dir", dir, "-all", "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator list -all exit = %d", code)
	}
	all := decodeOperatorJSON(t, stdout)["operators"].([]interface{})
	if len(all) != 1 {
		t.Fatalf("operator list -all returned %d operators, want 1 — revocation is an APPEND, not a deletion", len(all))
	}
	if all[0].(map[string]interface{})["revoked_reason"] != "laptop stolen" {
		t.Fatalf("operator list -all lost the revocation reason: %v", all[0])
	}

	// --- an unknown id is exit 5, distinct from the generic failure ---
	unknown := "op:" + busID + ".nobody-aaaaaaaaaaaaaaaa"
	code, stdout, _ = runOperator(t, "revoke", "-data-dir", dir, "-id", unknown, "-reason", "typo", "-json")
	if code != exitOperatorUnknown {
		t.Fatalf("revoking an unknown operator exit = %d, want %d\nstdout: %s", code, exitOperatorUnknown, stdout)
	}
}

// TestOperatorAddRefusedWhileTheDataDirIsLocked: the bus must be STOPPED, and
// that is ENFORCED rather than requested — two writers appending to one log
// destroy it.
func TestOperatorAddRefusedWhileTheDataDirIsLocked(t *testing.T) {
	dir, _ := operatorDataDir(t)
	authPub, certFP := operatorCredential(t, 0x21)

	lock, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("acquiring the data dir lock: %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Fatalf("releasing the lock: %v", err)
		}
	}()

	for _, args := range [][]string{
		{"add", "-data-dir", dir, "-name", "ops", "-auth-pub", authPub, "-cert-fingerprint", certFP, "-json"},
		{"revoke", "-data-dir", dir, "-id", "op:bus-optest.ops-aaaaaaaaaaaaaaaa", "-reason", "x", "-json"},
		{"list", "-data-dir", dir, "-json"},
	} {
		code, stdout, stderr := runOperator(t, args...)
		if code != exitOperatorBusRunning {
			t.Fatalf("`operator %s` against a locked data dir exit = %d, want %d\nstdout: %s\nstderr: %s",
				args[0], code, exitOperatorBusRunning, stdout, stderr)
		}
		// The --json failure object goes to STDOUT (invariant 7's second
		// audience: an agent that redirected stderr away still gets a parseable
		// answer).
		fail := decodeOperatorJSON(t, stdout)
		if fail["ok"] != false {
			t.Fatalf("the failure object's ok = %v, want false", fail["ok"])
		}
		if got := fail["exit_code"]; got != float64(exitOperatorBusRunning) {
			t.Fatalf("the failure object's exit_code = %v, want %d", got, exitOperatorBusRunning)
		}
	}
}

// TestOperatorAddRefusesAFingerprintBoundToAnEnrolledAgent is the CROSS-PLANE
// check: one certificate must never name both an agent and an operator.
//
// Without it the collapse this whole task exists to prevent is reachable through
// the TRANSPORT instead of through a permission flag — an enrolled agent's own
// certificate would satisfy an admin route's cross-check.
func TestOperatorAddRefusesAFingerprintBoundToAnEnrolledAgent(t *testing.T) {
	dir, busID := operatorDataDir(t)

	var fp [32]byte
	for i := range fp {
		fp[i] = 0x31
	}

	// Enrol an agent DURABLY, with that certificate bound, through the same
	// durable roster the server uses.
	agentID, err := ids.AgentID(busID, "worker", 1)
	if err != nil {
		t.Fatalf("building an agent id: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	func() {
		lg := logging.New(&bytes.Buffer{}, logging.LevelError)
		roster := auth.NewWALRoster(lg)
		applier, err := auth.NewMultiplexApplier(lg, map[string]wal.Applier{auth.RecordKind: roster})
		if err != nil {
			t.Fatalf("building the applier: %v", err)
		}
		log, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: applier})
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		defer func() {
			if err := log.Close(); err != nil {
				t.Fatalf("closing the log: %v", err)
			}
		}()
		if err := roster.Attach(log); err != nil {
			t.Fatalf("attaching the roster: %v", err)
		}
		now := time.Now().UTC()
		if err := roster.Put(auth.RosterEntry{
			AgentID: agentID, Name: "worker", AuthPublicKey: pub,
			Epoch: now, EnrolledAt: now,
			CertBindings: []auth.CertBinding{{Fingerprint: fp, BoundAt: now}},
		}); err != nil {
			t.Fatalf("enrolling the agent: %v", err)
		}
	}()

	authPub, _ := operatorCredential(t, 0x00)
	code, stdout, stderr := runOperator(t, "add", "-data-dir", dir,
		"-name", "ops", "-auth-pub", authPub,
		"-cert-fingerprint", buscert.Fingerprint(fp).String(), "-json")
	if code != exitOperatorFailed {
		t.Fatalf("adding an operator over an AGENT's certificate exit = %d, want %d\nstdout: %s\nstderr: %s",
			code, exitOperatorFailed, stdout, stderr)
	}
	fail := decodeOperatorJSON(t, stdout)
	msg, _ := fail["error"].(string)
	if !strings.Contains(msg, agentID) {
		t.Fatalf("the refusal does not name the agent holding the certificate: %q", msg)
	}

	// And nothing was written: no operator exists.
	code, stdout, _ = runOperator(t, "list", "-data-dir", dir, "-all", "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator list exit = %d", code)
	}
	if got := len(decodeOperatorJSON(t, stdout)["operators"].([]interface{})); got != 0 {
		t.Fatalf("the refused add still registered %d operators", got)
	}
}

// TestOperatorAddHasNoOperatorIDFlag pins invariant 1 at the CLI surface: the
// server is authoritative on every id, so there is no way to supply one.
//
// An -operator-id flag would let an operator be added under an id a REVOKED
// principal already used, quietly resurrecting a credential the log records as
// dead.
func TestOperatorAddHasNoOperatorIDFlag(t *testing.T) {
	dir, _ := operatorDataDir(t)
	authPub, certFP := operatorCredential(t, 0x41)

	code, stdout, stderr := runOperator(t, "add", "-data-dir", dir,
		"-name", "ops", "-auth-pub", authPub, "-cert-fingerprint", certFP,
		"-operator-id", "op:bus-optest.ops-aaaaaaaaaaaaaaaa")
	if code != exitOperatorUsage {
		t.Fatalf("`operator add -operator-id` exit = %d, want %d (the flag must not exist)\nstdout: %s\nstderr: %s",
			code, exitOperatorUsage, stdout, stderr)
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("stderr does not report an undefined flag, so -operator-id may have been accepted:\n%s", stderr)
	}
	if strings.Contains(operatorUsage, "-operator-id <") {
		t.Fatal("the usage text documents an -operator-id flag; the server is authoritative on every id (invariant 1)")
	}
}

// TestOperatorKeygenIsNonDestructive: keygen generates the operator's OWN
// credential on the OPERATOR's machine, touches no data directory, and NEVER
// overwrites existing material — regenerating would silently invalidate an
// operator record the bus already holds while looking like a no-op.
func TestOperatorKeygenIsNonDestructive(t *testing.T) {
	identityDir := filepath.Join(t.TempDir(), "operator-identity")

	code, stdout, stderr := runOperator(t, "keygen", "-identity-dir", identityDir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("operator keygen exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	first := decodeOperatorJSON(t, stdout)
	if first["created_auth_key"] != true || first["created_cert"] != true {
		t.Fatalf("the first keygen did not report creating the material: %v", first)
	}
	authPub, _ := first["auth_pub"].(string)
	certFP, _ := first["cert_fingerprint"].(string)
	if authPub == "" || certFP == "" {
		t.Fatalf("keygen printed no public values: %v", first)
	}
	if _, err := base64.StdEncoding.DecodeString(authPub); err != nil {
		t.Fatalf("auth_pub is not base64: %v", err)
	}
	if _, err := buscert.ParseFingerprint(certFP); err != nil {
		t.Fatalf("cert_fingerprint is not a fingerprint: %v", err)
	}
	// NO PRIVATE KEY IS PRINTED. The private halves never leave this machine and
	// the bus never sees them.
	if strings.Contains(stdout, "BEGIN PRIVATE KEY") {
		t.Fatalf("keygen printed private key material:\n%s", stdout)
	}

	keyPath := filepath.Join(identityDir, operatorAuthKeyFileName)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat %s: %v", keyPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %v, want 0600", keyPath, got)
	}

	// Run it again: same values, nothing created.
	code, stdout, stderr = runOperator(t, "keygen", "-identity-dir", identityDir, "-json")
	if code != exitOperatorOK {
		t.Fatalf("the second keygen exit = %d\nstderr: %s", code, stderr)
	}
	second := decodeOperatorJSON(t, stdout)
	if second["created_auth_key"] != false || second["created_cert"] != false {
		t.Fatalf("the second keygen claims to have created material: %v", second)
	}
	if second["auth_pub"] != authPub || second["cert_fingerprint"] != certFP {
		t.Fatalf("the second keygen returned DIFFERENT public values; existing material must be LOADED, never regenerated\nfirst:  %v / %v\nsecond: %v / %v",
			authPub, certFP, second["auth_pub"], second["cert_fingerprint"])
	}
}

// TestOperatorWarnsAboutSilentlyDangerousReuse covers the three conditions that
// used to succeed in SILENCE. None of them is an error — each is a state only
// the person running the command can judge — so each is reported as a warning,
// IN THE JSON DOCUMENT as well as in the human output, for
// inviteBlob.TransportInsecure's reason: an agent shelling out with 2>/dev/null
// must still see it.
func TestOperatorWarnsAboutSilentlyDangerousReuse(t *testing.T) {
	t.Run("keygen warns that it REUSED existing material", func(t *testing.T) {
		identityDir := filepath.Join(t.TempDir(), "operator-identity")
		code, stdout, stderr := runOperator(t, "keygen", "-identity-dir", identityDir, "-json")
		if code != exitOperatorOK {
			t.Fatalf("the first keygen exit = %d\nstderr: %s", code, stderr)
		}
		first := decodeOperatorJSON(t, stdout)
		if warns, ok := first["warnings"]; ok {
			for _, w := range warns.([]interface{}) {
				if strings.Contains(w.(string), "REUSING") {
					t.Fatalf("the FIRST keygen claimed to be reusing material: %v", w)
				}
			}
		}

		code, stdout, stderr = runOperator(t, "keygen", "-identity-dir", identityDir, "-json")
		if code != exitOperatorOK {
			t.Fatalf("the second keygen exit = %d\nstderr: %s", code, stderr)
		}
		second := decodeOperatorJSON(t, stdout)
		warns, ok := second["warnings"].([]interface{})
		if !ok || len(warns) == 0 {
			t.Fatalf("the second keygen reported NO warnings; silently reusing a certificate is how one certificate ends up naming both an agent and an operator: %v", second)
		}
		found := false
		for _, w := range warns {
			if strings.Contains(w.(string), "REUSING") && strings.Contains(w.(string), identityDir) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no reuse warning naming the identity directory: %v", warns)
		}
	})

	t.Run("keygen tightens and warns about a loose identity directory", func(t *testing.T) {
		identityDir := filepath.Join(t.TempDir(), "loose-identity")
		if err := os.MkdirAll(identityDir, 0o777); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Chmod(identityDir, 0o777); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		code, stdout, stderr := runOperator(t, "keygen", "-identity-dir", identityDir, "-json")
		if code != exitOperatorOK {
			t.Fatalf("keygen exit = %d\nstderr: %s", code, stderr)
		}
		warns, _ := decodeOperatorJSON(t, stdout)["warnings"].([]interface{})
		found := false
		for _, w := range warns {
			// The MODE is named, because it is evidence about the window in which
			// the material was exposed.
			if strings.Contains(w.(string), "0777") {
				found = true
			}
		}
		if !found {
			t.Fatalf("a 0777 identity directory produced no warning naming the mode: %v", warns)
		}
		info, err := os.Stat(identityDir)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != operatorIdentityDirMode {
			t.Fatalf("identity directory mode = %v, want %v; directory WRITE permission lets a local user replace %s",
				got, operatorIdentityDirMode, operatorAuthKeyFileName)
		}
	})

	t.Run("add warns over a REVOKED operator's fingerprint", func(t *testing.T) {
		dir, _ := operatorDataDir(t)
		authPub, certFP := operatorCredential(t, 0x77)

		code, stdout, stderr := runOperator(t, "add", "-data-dir", dir, "-name", "ops",
			"-auth-pub", authPub, "-cert-fingerprint", certFP, "-json")
		if code != exitOperatorOK {
			t.Fatalf("add exit = %d\nstderr: %s", code, stderr)
		}
		first := decodeOperatorJSON(t, stdout)
		if _, ok := first["warnings"]; ok {
			t.Fatalf("the first add warned about nothing in particular: %v", first["warnings"])
		}
		operatorID := first["operators"].([]interface{})[0].(map[string]interface{})["operator_id"].(string)

		if code, _, stderr := runOperator(t, "revoke", "-data-dir", dir, "-id", operatorID, "-reason", "laptop stolen", "-json"); code != exitOperatorOK {
			t.Fatalf("revoke exit = %d\nstderr: %s", code, stderr)
		}

		// The SAME certificate, a new principal. It succeeds — a revoked binding
		// constrains nothing — and it must say so.
		code, stdout, stderr = runOperator(t, "add", "-data-dir", dir, "-name", "ops2",
			"-auth-pub", authPub, "-cert-fingerprint", certFP, "-json")
		if code != exitOperatorOK {
			t.Fatalf("the second add exit = %d\nstderr: %s", code, stderr)
		}
		warns, _ := decodeOperatorJSON(t, stdout)["warnings"].([]interface{})
		found := false
		for _, w := range warns {
			if strings.Contains(w.(string), operatorID) && strings.Contains(w.(string), "REVOKED") {
				found = true
			}
		}
		if !found {
			t.Fatalf("adding an operator over a REVOKED operator's certificate warned nothing naming %q; the motivating case is a STOLEN LAPTOP: %v", operatorID, warns)
		}
	})
}

// TestOperatorListIsSilentOverAnInviteGatedEnrolment: an operator subcommand run
// against a data directory whose log holds a REAL invite-gated enrolment must
// print NOTHING on stderr.
//
// Before the fix, openOperatorRegistry's applier map registered the operator
// registry and the enrolment roster but NOT the invite store, so every composite
// "agent+invite" record made auth.MultiplexApplier log at ERROR that the invite
// "may be REDEEMABLE AGAIN until a restart with the applier wired". That claim is
// FALSE in this process — nothing here can redeem an invite — and it printed at
// the default -log-level warn, once per gated enrolment. A false alarm about a
// fail-open credential is not a cosmetic defect: it is what teaches an operator
// to skim past the invariant-6 discard lines that DO mean something.
func TestOperatorListIsSilentOverAnInviteGatedEnrolment(t *testing.T) {
	dir, busID := operatorDataDir(t)

	// A REAL invite-gated enrolment, written the way the server writes one: mint
	// an invite, redeem it, and commit the enrolment and the invite consumption as
	// ONE composite record.
	agentID, err := ids.AgentID(busID, "worker", 1)
	if err != nil {
		t.Fatalf("building an agent id: %v", err)
	}
	func() {
		lg := logging.New(&bytes.Buffer{}, logging.LevelError)
		roster := auth.NewWALRoster(lg)
		dl := &deferredLog{}
		store, err := invite.NewStore(invite.StoreOptions{BusID: busID, Durable: dl, Logger: lg})
		if err != nil {
			t.Fatalf("invite.NewStore: %v", err)
		}
		applier, err := auth.NewMultiplexApplier(lg, map[string]wal.Applier{
			auth.RecordKind:   roster,
			invite.RecordKind: store,
		})
		if err != nil {
			t.Fatalf("building the applier: %v", err)
		}
		log, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: applier})
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		dl.log = log
		defer func() {
			if err := log.Close(); err != nil {
				t.Fatalf("closing the log: %v", err)
			}
		}()
		if err := roster.Attach(log); err != nil {
			t.Fatalf("attaching the roster: %v", err)
		}

		minted, err := store.Mint(invite.MintRequest{Label: "for the test"})
		if err != nil {
			t.Fatalf("minting an invite: %v", err)
		}
		red, err := store.Begin(invite.RedeemRequest{
			InviteID:    minted.ID,
			Secret:      minted.Secret,
			Key:         "0123456789abcdef",
			Fingerprint: idem.ComputeFingerprint([]byte("worker")),
		})
		if err != nil {
			t.Fatalf("beginning the redemption: %v", err)
		}
		rider, err := red.Consume(invite.Result{AgentID: agentID, Response: json.RawMessage(`{"agent_id":"` + agentID + `"}`)})
		if err != nil {
			t.Fatalf("consuming the invite: %v", err)
		}
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("ed25519.GenerateKey: %v", err)
		}
		now := time.Now().UTC()
		if _, err := roster.PutWithInvite(auth.RosterEntry{
			AgentID: agentID, Name: "worker", AuthPublicKey: pub,
			Epoch: now, EnrolledAt: now,
		}, auth.InviteRider{Kind: invite.RecordKind, Body: rider}); err != nil {
			t.Fatalf("writing the composite enrolment: %v", err)
		}
		if err := red.Commit(); err != nil {
			t.Fatalf("committing the redemption: %v", err)
		}
	}()

	for _, args := range [][]string{
		{"list", "-data-dir", dir, "-json"},
		{"list", "-data-dir", dir, "-all", "-json"},
	} {
		code, stdout, stderr := runOperator(t, args...)
		if code != exitOperatorOK {
			t.Fatalf("`operator %v` exit = %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
		}
		if strings.Contains(stderr, "REDEEMABLE AGAIN") {
			t.Fatalf("`operator %v` printed the FALSE invite warning; the invite store is not registered in the applier map:\n%s", args, stderr)
		}
		if stderr != "" {
			t.Fatalf("`operator %v` wrote to stderr at the default log level:\n%s", args, stderr)
		}
	}
}

// TestOperatorUsageAndFailureShapes covers the contract an agent shelling out
// depends on (invariant 7's second audience): stable exit codes, -h on stdout,
// --json failure objects on stdout, and no unvalidated argv echoed to a
// terminal.
func TestOperatorUsageAndFailureShapes(t *testing.T) {
	t.Run("-h goes to stdout with exit 0", func(t *testing.T) {
		code, stdout, stderr := runOperator(t, "-h")
		if code != exitOperatorOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(stdout, "agent-bus operator") {
			t.Fatalf("usage did not reach stdout:\n%s", stdout)
		}
		if stderr != "" {
			t.Fatalf("requested help wrote to stderr:\n%s", stderr)
		}
	})

	t.Run("an unknown subcommand is not echoed", func(t *testing.T) {
		code, _, stderr := runOperator(t, "\x1b[31mrm -rf\x1b[0m")
		if code != exitOperatorUsage {
			t.Fatalf("exit = %d, want %d", code, exitOperatorUsage)
		}
		if strings.Contains(stderr, "rm -rf") {
			t.Fatalf("the unknown subcommand was echoed to stderr:\n%q", stderr)
		}
	})

	t.Run("--json failures go to stdout", func(t *testing.T) {
		dir, _ := operatorDataDir(t)
		for _, tc := range []struct {
			name string
			args []string
			want int
		}{
			{"missing name", []string{"add", "-data-dir", dir, "-auth-pub", "x", "-cert-fingerprint", "y", "-json"}, exitOperatorUsage},
			{"bad auth-pub", []string{"add", "-data-dir", dir, "-name", "ops", "-auth-pub", "not base64!", "-cert-fingerprint", strings.Repeat("a", 64), "-json"}, exitOperatorUsage},
			{"missing cert fingerprint", []string{"add", "-data-dir", dir, "-name", "ops", "-auth-pub", base64.StdEncoding.EncodeToString(make([]byte, 32)), "-json"}, exitOperatorUsage},
			{"revoke with no reason", []string{"revoke", "-data-dir", dir, "-id", "op:bus-optest.ops-aaaaaaaaaaaaaaaa", "-json"}, exitOperatorUsage},
			{"revoke an AGENT id", []string{"revoke", "-data-dir", dir, "-id", "bus-optest.worker-1", "-reason", "x", "-json"}, exitOperatorUsage},
			{"no such data dir", []string{"list", "-data-dir", filepath.Join(dir, "nope"), "-json"}, exitOperatorNoIdentity},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				code, stdout, stderr := runOperator(t, tc.args...)
				if code != tc.want {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, tc.want, stdout, stderr)
				}
				fail := decodeOperatorJSON(t, stdout)
				if fail["ok"] != false {
					t.Fatalf("ok = %v, want false", fail["ok"])
				}
				if fail["exit_code"] != float64(tc.want) {
					t.Fatalf("exit_code = %v, want %d", fail["exit_code"], tc.want)
				}
				if fail["error"] == "" {
					t.Fatal("the failure object carries no error text")
				}
			})
		}
	})
}
