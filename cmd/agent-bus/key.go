package main

// `agent-bus key export-public` (CLI-11): print the PUBLIC half of this bus's
// Ed25519 signing key, so an operator can pin it on a PEER bus with
// `agent-bus peer add -bus-id <this bus> -signing-key <the value printed here>`.
//
// Until this existed there was no compiled way to obtain that value at all, and
// the federation smoke test (scripts/fed-smoke.sh) could not begin: peering
// requires each bus to pin the other's signing key, and the only place the key
// lived was a 0600 PEM inside a data directory. The alternative an agent would
// otherwise reach for -- scraping the PEM with openssl -- is exactly the
// hand-rolled workaround invariant 7 exists to forbid.
//
// # ON THE SERVER BINARY, NOT ON agent-busctl
//
// For `invite mint`'s and `peer add`'s reason, and it is worth stating because
// this command was specified against agent-busctl and deliberately moved.
// agent-busctl is a pure HTTP client: it imports only client/, touches nothing
// under internal/, and has no data-directory or dirlock plumbing at all. The
// authority this command needs is FILESYSTEM ACCESS to the data directory
// (DECISIONS.md E4), not a network privilege, so it belongs beside the other two
// offline operator subcommands. Giving the network client filesystem and lock
// access to satisfy a spelling would have been a real architectural change
// justified by nothing.
//
// # IT EXPORTS THE PUBLIC HALF AND NOTHING ELSE
//
// There is NO public-key file on disk: the signing key is stored as one PKCS#8
// PEM holding the private key, from which the public half is derived. So this
// command necessarily LOADS the private key in order to print the public one,
// and the discipline that keeps that safe is narrow and deliberate:
// Material.SigningPublicKey() is the only accessor used here, and
// Material.SigningPrivateKey() is never called anywhere in this file. Nothing
// here logs the material, and the failure paths below name PATHS, never
// contents. TestKeyExportNeverPrintsPrivateKeyMaterial pins that end to end
// across every flag combination, on both streams -- a claim of this shape must
// be a test, not a comment, because a leak of this kind is silent.
//
// # IT NEVER MINTS AN IDENTITY. THAT IS THE WHOLE HAZARD.
//
// buscert.LoadOrCreate GENERATES a certificate and two private keys when the
// data directory holds NONE of the three (internal/buscert/buscert.go), and
// there is no load-only entry point. An EXPORT command that quietly minted would
// be a federation-wide identity event triggered by a read: the operator would
// copy a signing key that no running bus has ever used, pin it on a peer, and
// discover the fault only when a relayed message failed to verify -- which looks
// exactly like the substitution the pin exists to detect. It would also leave a
// half-built data directory behind that the real first start would then refuse.
//
// So the material is checked to be PRESENT before it is loaded, twice, exactly
// as mintInvite does it and for the same two reasons: the pre-lock check keeps a
// refusal from writing so much as a bus.lock into a directory the operator
// mistyped, and the post-lock check is the one that is load-bearing for
// correctness, because between the two a concurrent process could have removed a
// file and turned the load into a mint. material.Generated() is then checked as
// a last resort. TestKeyExportRefusesADirectoryWithNoKeyMaterial asserts the
// directory is still EMPTY afterwards, not merely that the exit code was
// nonzero: a test that checks only the code passes just as happily on a command
// that mints and then fails for some later reason.
//
// ONE HOLE IS KNOWN AND IS NOT CLOSED HERE, because closing it needs a change
// outside this command: ids.LoadOrCreateBusID has no Generated()-equivalent, so
// if the bus-id file is removed in the window between the post-lock check and
// that call, a fresh bus id is minted and persisted. The export is still
// refused -- the CommonName cross-check below catches it, and no key is
// reported -- but this command will have written, which is why the usage text
// claims only that it does not create a bus IDENTITY. A load-only accessor in
// internal/ids is the real fix; filed as CLI-11-FU-LOADONLY. The window requires
// write access to a 0700 data directory, so it crosses no privilege boundary.
//
// # THE BUS MUST BE STOPPED
//
// The exclusive dirlock is taken for `peer add`'s reason rather than because
// this command writes -- it writes nothing. Holding it is what makes the
// "material is present" check above hold still: a bus starting concurrently
// against a virgin directory is precisely the process that would mint underneath
// us. `healthcheck` is the deliberate contrast; it takes no lock because it
// reads one file and asserts nothing about the directory's shape.
//
// # THE ENCODING IS NOT CHOSEN HERE, IT IS MATCHED
//
// Standard base64 with padding: 44 characters for the 32-byte key. That is what
// `agent-bus peer add -signing-key` parses (base64.StdEncoding.DecodeString) and
// what internal/relay writes into a BusTrustRecord, so the value printed here can
// be pasted straight into the command that consumes it. Lowercase hex is a
// DIFFERENT encoding used for the TLS certificate fingerprint and must never be
// used for a signing key -- two 32-byte values in one workflow, distinguishable
// only by their encoding, is a mistake worth foreclosing. No encoding, hash or
// KDF is implemented here (invariant 9); this is stdlib base64 over a key the
// crypto library derived.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
)

// keyCommandName is the intercepted first argument, kept as a constant so the
// dispatch in main() and the usage text cannot drift apart.
const keyCommandName = "key"

// Exit codes for `agent-bus key`. These are a CONTRACT (invariant 7: an agent
// shelling out branches on them), and they mirror `agent-bus invite`'s codes
// value for value so an operator driving both does not have to hold two maps in
// their head.
const (
	// exitKeyOK: the public key was printed.
	exitKeyOK = 0
	// exitKeyFailed: the export failed for a reason that is not one of the
	// specific cases below -- an unreadable file, a corrupt certificate, an
	// expired one. Nothing was written either way.
	exitKeyFailed = 1
	// exitKeyUsage: bad flags, or an unknown subcommand.
	exitKeyUsage = 2
	// exitKeyBusRunning: the data directory is locked by a live process, which
	// is almost certainly the bus itself. Distinct from exitKeyFailed because
	// the remedy -- stop the bus -- is specific and the data directory is fine.
	exitKeyBusRunning = 3
	// exitKeyNoIdentity: the data directory does not exist, is not a directory,
	// or does not hold this bus's key material. NOTHING WAS WRITTEN: in
	// particular no key material was generated, which is the failure mode this
	// code exists to keep distinguishable from a mere error.
	//
	// TWO CARVE-OUTS, stated rather than hidden, because both are cases where 4
	// is returned precisely BECAUSE something was written -- in a race this
	// command lost, under the lock, by a library call on the way to the refusal:
	//
	//   - the material.Generated() backstop below, when buscert minted. Its
	//     remedy names the three files to DELETE, because Generated() is false on
	//     a re-run and the next invocation would otherwise succeed silently.
	//   - the CommonName cross-check below, when ids.LoadOrCreateBusID minted a
	//     bus id. That refusal is PERSISTENT -- the new id disagrees with the
	//     certificate on every subsequent run too -- so unlike the first it
	//     cannot decay into a silent successful export, and its remedy is to
	//     restore rather than to delete.
	//
	// Every other path to 4 wrote nothing.
	exitKeyNoIdentity = 4
)

// runKeyCommand dispatches `agent-bus key <subcommand>`.
//
// It returns an exit code rather than calling os.Exit so that it is testable in
// process; main() is the only place that exits.
func runKeyCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, keyUsage)
		return exitKeyUsage
	}
	switch args[0] {
	case "export-public":
		return runKeyExportPublic(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, keyUsage)
		return exitKeyOK
	}
	// The subcommand is NOT echoed: it is unvalidated argv on its way to a
	// terminal. The usage text below lists what is legal, which is the more
	// useful answer anyway.
	fmt.Fprintf(stderr, "agent-bus key: unknown subcommand\n")
	fmt.Fprint(stderr, keyUsage)
	return exitKeyUsage
}

const keyUsage = `agent-bus key export-public — print this bus's SIGNING PUBLIC KEY

USAGE
  agent-bus key export-public -data-dir <dir> [-json]

WHAT IT IS FOR
  A peer bus verifies messages that ORIGINATE here against a pinned copy of this
  bus's signing key. This command prints the value to pin. On the peer bus:

    agent-bus peer add -data-dir <peer's dir> -bus-id <this bus's id> \
      -signing-key <the key printed here>

  The value is standard base64 with padding — 44 characters for the 32-byte
  Ed25519 key — which is exactly what -signing-key parses. It is NOT the
  lowercase-hex encoding used for the TLS certificate fingerprint; the two are
  both 32-byte values and are not interchangeable.

THE BUS MUST NOT BE RUNNING. This takes the data directory's exclusive lock,
which a running bus holds. It writes no key material — only the bus.lock the
lock itself creates — and holding the lock is what guarantees no other process
can mint key material underneath the check below.

IT DOES NOT CREATE A BUS IDENTITY. A data directory that does not exist, or that
holds no key material, is REFUSED with exit 4 and left exactly as it was found —
a signing key nobody has ever served is worse than no answer at all. The one way
identity material can be written is another process removing a file from under
the lock while this runs; that is refused too, and the message says which file
is wrong and whether to delete it or restore it.

WHAT IS PRINTED IS PUBLIC. The private signing key never leaves the data
directory: it is not printed, not logged, and not named in any error here.

FLAGS
  -data-dir <dir>    the bus's data directory; it must already hold this bus's
                     identity. Default "./data"
  -json              emit one JSON object on stdout:
                     {"ok":true,"bus_id":…,"public_key":…,"key_type":"ed25519"}
                     On failure: {"ok":false,"error":…,"remedy":…,"exit_code":…}

EXIT CODES
  0  the public key was printed
  1  the export failed; nothing was written
  2  usage: bad flag, or an unknown subcommand
  3  the data directory is locked by a live process — stop the bus and retry
  4  no data directory, or no key material in it; nothing was created
`

// runKeyExportPublic is `agent-bus key export-public`.
func runKeyExportPublic(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir string
		asJSON  bool
	)
	fs := flag.NewFlagSet("agent-bus key export-public", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A NO-OP Usage, for the reason runInviteMint documents: flag calls Usage
	// both for -h and for a bad flag, but requested help is OUTPUT and belongs on
	// stdout while an error is diagnostics and belongs on stderr. The two cases
	// are separated at the Parse call instead.
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; it must already hold this bus's identity")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")

	// THERE IS NO FLAG THAT EXPORTS THE PRIVATE KEY, and none may be added. The
	// private half has exactly one legitimate location -- its own 0600 file in
	// this data directory -- and a backup of that file is the only supported way
	// to copy it. A convenience flag would put it in a shell history and a CI log
	// the first time anyone used it.

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Requested help is OUTPUT: stdout, exit 0.
			fmt.Fprint(stdout, keyUsage)
			return exitKeyOK
		}
		// flag has already printed the specific error to stderr; the usage text
		// follows it there.
		fmt.Fprint(stderr, keyUsage)
		return exitKeyUsage
	}
	if fs.NArg() > 0 {
		// The argument is NOT echoed: it is unvalidated argv on its way to a
		// terminal.
		fmt.Fprintf(stderr, "agent-bus key export-public: unexpected argument\n")
		return exitKeyUsage
	}

	busID, pub, kerr := exportBusSigningPublicKey(dataDir, stderr)
	if kerr != nil {
		return keyFail(stdout, stderr, asJSON, kerr.code, kerr.Error(), kerr.remedy)
	}

	encoded := base64.StdEncoding.EncodeToString(pub)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(keyExportResult{
			OK:        true,
			BusID:     busID,
			PublicKey: encoded,
			KeyType:   "ed25519",
		}); err != nil {
			fmt.Fprintf(stderr, "agent-bus key export-public: writing the JSON result failed: %v\n", err)
			return exitKeyFailed
		}
		return exitKeyOK
	}
	writeKeyExportHuman(stdout, busID, encoded)
	return exitKeyOK
}

// keyExportResult is the --json success shape.
type keyExportResult struct {
	// OK is true on this type by construction; it exists so a caller consuming
	// --json can branch on one field for both success and failure, which keyError
	// mirrors.
	OK bool `json:"ok"`

	// BusID is the bus this key belongs to. It is reported alongside the key
	// because a peer pins the PAIR -- `peer add -bus-id X -signing-key Y` -- and
	// a bare key with no bus id is an invitation to pin it against the wrong one.
	BusID string `json:"bus_id"`

	// PublicKey is the PUBLIC half, standard base64 with padding, ready to paste
	// into `agent-bus peer add -signing-key`. The private half is never included
	// in this struct, and there is no field for it.
	PublicKey string `json:"public_key"`

	// KeyType names the algorithm so a consumer never has to infer it from the
	// length. Ed25519 is the only value today.
	KeyType string `json:"key_type"`
}

// keyError is the --json failure shape. `ok` is the field a caller branches on,
// so it is present in both shapes and in the same position.
type keyError struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Remedy   string `json:"remedy,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// keyFail reports a failure in whichever mode was asked for and returns the exit
// code, so every failure path is one line at the call site.
//
// In --json mode the object goes to STDOUT, not stderr: an agent that redirected
// stderr away still gets a parseable answer, which is the whole reason --json
// exists (invariant 7's second audience).
func keyFail(stdout, stderr io.Writer, asJSON bool, code int, msg, remedy string) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(keyError{OK: false, Error: msg, Remedy: remedy, ExitCode: code}); err != nil {
			fmt.Fprintf(stderr, "agent-bus key export-public: %s\n", msg)
			return exitKeyFailed
		}
		return code
	}
	fmt.Fprintf(stderr, "agent-bus key export-public: %s\n", msg)
	if remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", remedy)
	}
	return code
}

func writeKeyExportHuman(w io.Writer, busID, encoded string) {
	fmt.Fprintf(w, "bus id:             %s\n", busID)
	fmt.Fprintf(w, "signing public key: %s\n", encoded)
	fmt.Fprintf(w, "\nPin it on a PEER bus so that bus can verify messages originating here:\n")
	fmt.Fprintf(w, "  agent-bus peer add -data-dir <that bus's data dir> -bus-id %s -signing-key %s\n", busID, encoded)
	fmt.Fprintf(w, "\nThis is the PUBLIC half and is safe to copy. The private signing key stays in\nthis data directory and is not recoverable from the value above.\n")
}

// keyExportError carries the exit code and remedy alongside the message, so the
// caller reports every failure the same way. It mirrors inviteMintError.
type keyExportError struct {
	code   int
	msg    string
	remedy string
	cause  error
}

func (e *keyExportError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *keyExportError) Unwrap() error { return e.cause }

// exportBusSigningPublicKey loads this bus's identity from dataDir and returns
// the bus id and the PUBLIC half of its signing key.
//
// It NEVER creates anything: not the directory, not the bus id, not the key
// material. See the "IT NEVER MINTS AN IDENTITY" section at the top of this file
// for why that is the load-bearing property rather than a nicety, and why the
// presence check runs twice.
//
// The ORDER below mirrors mintInvite deliberately -- stat, check, lock, re-check,
// bus id, certificate -- because the reasons that function gives for it all apply
// here unchanged.
func exportBusSigningPublicKey(dataDir string, stderr io.Writer) (string, ed25519.PublicKey, *keyExportError) {
	// The data directory is NOT created. run() does MkdirAll because a server is
	// entitled to start a fresh bus; this command is not, and a typo in -data-dir
	// that minted a whole new bus identity would hand the operator a signing key
	// that no bus has ever used.
	info, err := os.Stat(dataDir)
	if err != nil {
		return "", nil, &keyExportError{
			code:   exitKeyNoIdentity,
			msg:    fmt.Sprintf("cannot read the data directory %q", dataDir),
			remedy: "check -data-dir; this command never creates one, because a typo would mint a second bus identity",
			cause:  err,
		}
	}
	if !info.IsDir() {
		return "", nil, &keyExportError{
			code:   exitKeyNoIdentity,
			msg:    fmt.Sprintf("-data-dir %q is not a directory", dataDir),
			remedy: "point -data-dir at the bus's data directory",
		}
	}

	// BEFORE THE LOCK, so that this refusal writes NOTHING AT ALL -- not even the
	// bus.lock that dirlock.Acquire creates. That matters more than it sounds:
	// run() decides whether a data directory "has history" by asking whether it
	// was empty at startup, so a lone bus.lock left in a virgin directory makes
	// the operator's very first `agent-bus` start refuse to boot. The same trap
	// was measured on `invite mint`'s first end-to-end run.
	if e := checkBusKeyMaterialPresent(dataDir); e != nil {
		return "", nil, e
	}

	// The exclusive lock. This command writes nothing, so the lock is not
	// protecting a write of ours -- it is what stops a bus STARTING against a
	// virgin directory from minting key material between the check above and the
	// load below.
	lock, err := dirlock.Acquire(dataDir)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return "", nil, &keyExportError{
				code:   exitKeyBusRunning,
				msg:    "the data directory is locked by a live process, which is almost certainly the bus itself",
				remedy: "stop the bus, run this command, then start it again; the signing key does not change across a restart, so the value you get afterwards is the same one",
				cause:  err,
			}
		}
		return "", nil, &keyExportError{
			code:  exitKeyFailed,
			msg:   "locking the data directory",
			cause: err,
		}
	}
	defer func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(stderr, "agent-bus key export-public: releasing the data directory lock failed: %v\n", err)
		}
	}()

	// RE-CHECK, now that the directory is exclusively ours. This is the check
	// that makes "never creates an identity" true rather than merely likely: the
	// pre-lock check ran while another process could still have been removing a
	// file, and losing that race turns the load below into a mint.
	if e := checkBusKeyMaterialPresent(dataDir); e != nil {
		return "", nil, e
	}

	// A LOAD, never a create: the check immediately above -- taken under the lock
	// -- is the only thing that makes LoadOrCreateBusID's "Create" half
	// unreachable here.
	busID, err := ids.LoadOrCreateBusID(dataDir, "")
	if err != nil {
		return "", nil, &keyExportError{code: exitKeyFailed, msg: "resolving the bus id", cause: err}
	}

	// Hosts is nil on purpose, for mintInvite's reason: it is consulted only when
	// material is GENERATED, and the checks above have already established that
	// it will not be. Passing certHosts here would imply this command has an
	// opinion about the listener, which it must not.
	material, err := buscert.LoadOrCreate(dataDir, buscert.Options{BusID: busID})
	if err != nil {
		return "", nil, &keyExportError{
			code: exitKeyFailed,
			msg:  "loading this bus's key material",
			// Naming the shared cause is the useful remedy: buscert's errors are
			// the same ones the server refuses to start on, so the operator has one
			// problem to fix rather than two to correlate. An EXPIRED CERTIFICATE
			// reaches here too, even though the signing key it blocks is
			// independent of the certificate: buscert validates the date window on
			// the way to loading the signing key and there is no load-only
			// accessor. Filed as CLI-11-FU-LOADONLY.
			remedy: "the bus itself refuses to start on the same error; fix it there first. Nothing was written or regenerated by this refusal",
			cause:  err,
		}
	}
	if material.Generated() {
		// Unreachable given the two checks above, and refused rather than assumed
		// away. If it ever fires, this command has just minted a NEW bus identity:
		// exporting its key would send an operator off to pin a value no bus has
		// ever served, which is indistinguishable at the peer from the
		// substitution the pin exists to detect.
		//
		// THE REMEDY MUST SAY "DELETE THEM", and that is not tidiness. This
		// refusal leaves the three minted files on disk -- deleting key material
		// automatically is never this command's call -- and Generated() is FALSE
		// on the next load, so a re-run would sail through every check above and
		// export the freshly minted key with exit 0 and no warning at all. The
		// operator would then pin, on a peer, a key no bus has ever served, which
		// at that peer is indistinguishable from the substitution the pin exists
		// to detect. The one-line remedy is the only thing standing between this
		// refusal and that outcome.
		return "", nil, &keyExportError{
			code: exitKeyNoIdentity,
			msg:  fmt.Sprintf("this command GENERATED fresh bus key material in %q, which it must never do; no key was exported", dataDir),
			remedy: "the directory was missing all three of " + buscert.CertFileName + ", " + buscert.TLSKeyFileName +
				" and " + buscert.SigningKeyFileName + " between the check and the load. DELETE those three files BY HAND before doing anything else: " +
				"they are a bus identity nothing has ever served, and a second run of this command would export the new key with exit 0 and no warning. " +
				"Then restore the real material from backup",
		}
	}
	if cn := material.Certificate().Subject.CommonName; cn != busID {
		// mintInvite refuses the same disagreement for the same reason: the
		// CommonName is descriptive only, but it is set to the bus id at
		// generation, so a mismatch means the certificate and the bus-id file come
		// from DIFFERENT buses. This command reports the key and the bus id
		// TOGETHER, and a peer pins them as a pair -- so reporting a key under the
		// wrong bus id would install a pin that can never verify anything.
		return "", nil, &keyExportError{
			code:   exitKeyNoIdentity,
			msg:    fmt.Sprintf("the bus certificate in %q names bus %q but this data directory's bus id is %q; the certificate and the bus id come from different buses", dataDir, cn, busID),
			remedy: "restore the matching certificate and bus-id file from backup; do not pin a signing key against a bus id that may belong to another bus",
		}
	}

	// SigningPublicKey is the ONLY accessor used. SigningPrivateKey exists on the
	// same type and is never called in this file.
	return busID, material.SigningPublicKey(), nil
}

// checkBusKeyMaterialPresent refuses a data directory that does not already hold
// this bus's complete identity, BEFORE anything can create part of it.
//
// All four files are checked, not just the signing key this command reports. The
// bus id is checked because ids.LoadOrCreateBusID CREATES one when the file is
// absent, and a freshly minted bus id would be persisted into the directory,
// permanently renaming the bus away from its own certificate. The TLS key and
// the certificate are checked because buscert.LoadOrCreate mints all three when
// ALL THREE are absent -- so requiring every one of them present is what makes
// the mint branch structurally unreachable, rather than merely unlikely.
func checkBusKeyMaterialPresent(dataDir string) *keyExportError {
	for _, want := range []struct{ path, what string }{
		{filepath.Join(dataDir, busIDFileName), "bus id file"},
		{filepath.Join(dataDir, buscert.CertFileName), "bus certificate"},
		{filepath.Join(dataDir, buscert.TLSKeyFileName), "bus TLS key"},
		{filepath.Join(dataDir, buscert.SigningKeyFileName), "bus signing key"},
	} {
		_, err := os.Stat(want.path)
		if err == nil {
			continue
		}
		// "I could not look at the file" is NOT "the file is not there", and the
		// difference matters in the REMEDY rather than in the outcome: both
		// refuse, but telling an operator to restore from backup a file that is
		// present and merely unreadable invites them to overwrite it. This is
		// internal/buscert's own rule (fileExists, buscert.go), applied to the
		// same question one level up.
		if !os.IsNotExist(err) {
			return &keyExportError{
				code:   exitKeyFailed,
				msg:    fmt.Sprintf("cannot determine whether this data directory holds a %s (%s)", want.what, want.path),
				remedy: "fix the permissions or the I/O error on that path and retry; the file may well be present, so do NOT restore over it until you have looked",
				cause:  err,
			}
		}
		return &keyExportError{
			code: exitKeyNoIdentity,
			msg:  fmt.Sprintf("this data directory holds no %s (%s), so this bus has no signing key to export", want.what, want.path),
			remedy: "if this bus has never run, start it once against this -data-dir and let it come up, then stop it and retry; " +
				"if it HAS run, that file has been lost and must be restored from backup — this command will not recreate it, because a regenerated signing key invalidates the pin held by every peer bus. Nothing was written by this refusal",
			cause: err,
		}
	}
	return nil
}
