package main

// The operator-facing OPERATOR PRINCIPAL subcommand: `agent-bus operator
// keygen|add|list|revoke` (AUTH-10).
//
// # WHY AN OPERATOR IS NOT AN AGENT, IN ONE SENTENCE
//
// If an admin route reused AGENT authentication, an AGENT credential would
// authorise minting the credentials that CREATE AGENTS: any enrolled agent could
// mint itself an unlimited supply of new identities, which collapses invariant 3
// completely. So the principal is distinct in KIND, not merely in permission —
// its own id namespace, its own durable record, its own registry, its own Go
// principal type and its own session table (internal/auth/operator*.go).
//
// # WHY THIS IS A SERVER SUBCOMMAND AND NOT AN HTTP ROUTE
//
// The same decision `invite mint` records (DECISIONS.md E4): the minting
// authority is FILESYSTEM ACCESS to the data directory, exactly the model
// already used for wal-mac.key and the bus's private keys. Nothing new is
// exposed on the wire, so introducing an admin principal introduces NO new
// network-reachable privilege — which matters more here than anywhere else,
// because an online "create an operator" route would have to be authorised by
// something, and the only credential that exists before any operator does is the
// filesystem.
//
// Note the audience split invariant 7 draws: operator commands belong on
// `agent-bus`, the SERVER binary that already hosts `invite mint`, `peer` and
// `log` — NOT on `agent-busctl`, which is the AGENT's client. An admin
// capability does not go on the agent surface.
//
// # THE BUS MUST BE STOPPED for `add` and `revoke`. This is structural.
//
// Both append through internal/wal's two-phase path, and the data directory
// takes an EXCLUSIVE dirlock precisely so that two processes never append to one
// log. A running bus holds that lock, so these refuse with
// exitOperatorBusRunning and say so.
//
// # SCOPE, stated plainly so nobody draws the easy wrong conclusion
//
// This delivers the PRINCIPAL and the AUTHORIZATION CHECK. It adds NO admin
// route. Nothing on the wire consumes an OperatorPrincipal yet — AUTH-7 (clear
// an agent's sessions), INVMINT (online invite mint) and CONV-AUTHZ-ADMIN are
// the consumers, each a separate task with its own gates, and
// auth.NewOperatorService still has no non-test caller. AUTH-10-WIRING did NOT
// change that sentence and must not be read as softening it.
//
// What AUTH-10-WIRING (2026-08-21) did change: a record written here is durable
// AND IT IS NOW REPLAYED BY THE SERVER. This paragraph used to end "see the note
// in internal/auth/operator.go about the server's applier map, which does NOT
// yet register auth.OperatorRecordKind", and that is now false — main.go
// registers auth.OperatorRecordKind, so a record this command writes is applied
// at server startup instead of being passed over in silence. Replaying a
// principal is not consuming one; the two halves are separate claims.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"crypto/x509"

	"github.com/dodgymike/agent-bus/client"
	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// operatorCommandName is the single non-flag argument main() intercepts before
// server flag parsing. Pinned as a constant so the dispatch in main.go and the
// usage text cannot drift apart.
//
// THE DISPATCH IN main.go IS WIRED (AUTH-10-WIRING, 2026-08-21). This comment
// said the opposite for as long as it was true — "!! THE DISPATCH IN main.go IS
// NOT YET WIRED", the two lines main.go needed quoted beneath it, and the
// warning that until they landed `agent-bus operator …` "falls through to
// parseFlags and is refused as an unexpected argument". THAT IS NOW FALSE:
// main.go tests os.Args[1] == operatorCommandName beside the identical block for
// peerCommandName, and parseFlags' usage announces the subcommand, so all four
// verbs answer at a prompt.
//
// TestOperatorSubcommandIsReachableFromArgv (cmd/agent-bus/operatorwiring_test.go)
// proves that against the COMPILED BINARY, which is the only thing that can:
// operator_test.go calls runOperatorCommand directly and was green throughout
// the period the command could not be typed at a shell.
const operatorCommandName = "operator"

// operatorAuthKeyFileName is the operator's PRIVATE session-signing key, PKCS#8
// PEM, inside the operator's own identity directory.
//
// It lives beside client.ClientTLSDirName in the same identity directory because
// the two are halves of ONE operator credential: the certificate proves which
// key holder is on the connection, this key signs the session challenge, and
// invariant 11 requires BOTH. Splitting them across two directories would make
// "back up my operator credential" a two-step instruction, which is how one half
// gets lost.
const operatorAuthKeyFileName = "operator-auth-key.pem"

const (
	operatorIdentityDirMode fs.FileMode = 0o700
	operatorKeyFileMode     fs.FileMode = 0o600
)

// Exit codes for `agent-bus operator`. These are a CONTRACT (invariant 7: an
// agent shelling out branches on them) and are documented in CONTRACTS-CLI.md.
// They are numbered to MATCH `invite` and `peer` so that an operator scripting
// against all three does not have to learn a second table.
const (
	// exitOperatorOK: the command succeeded.
	exitOperatorOK = 0
	// exitOperatorFailed: the command failed.
	exitOperatorFailed = 1
	// exitOperatorUsage: bad flags, an unknown subcommand, or invalid input.
	exitOperatorUsage = 2
	// exitOperatorBusRunning: the data directory is locked by a live process.
	// Remedy: stop the bus, run the command, start it again.
	exitOperatorBusRunning = 3
	// exitOperatorNoIdentity: the data directory does not hold a usable bus
	// identity. NOTHING IS WRITTEN on this path.
	exitOperatorNoIdentity = 4
	// exitOperatorUnknown: the named operator is not registered. Separate from
	// the generic failure because the remedy is different — check the id, or
	// list the operators — and a caller that cannot tell them apart has to parse
	// English to know which.
	exitOperatorUnknown = 5
)

// runOperatorCommand dispatches `agent-bus operator <subcommand>`.
//
// It returns an exit code rather than calling os.Exit so that it is testable in
// process; main() is the only place that exits.
func runOperatorCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, operatorUsage)
		return exitOperatorUsage
	}
	switch args[0] {
	case "keygen":
		return runOperatorKeygen(args[1:], stdout, stderr)
	case "add":
		return runOperatorAdd(args[1:], stdout, stderr)
	case "list":
		return runOperatorList(args[1:], stdout, stderr)
	case "revoke":
		return runOperatorRevoke(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, operatorUsage)
		return exitOperatorOK
	default:
		// The unknown subcommand is NOT echoed: it is unvalidated argv reaching a
		// stderr line, the discipline every command in this binary applies.
		fmt.Fprint(stderr, "agent-bus operator: unknown subcommand\n\n")
		fmt.Fprint(stderr, operatorUsage)
		return exitOperatorUsage
	}
}

const operatorUsage = `agent-bus operator — the bus's OPERATOR/ADMIN principals

An operator is a bus-scoped identity that is NOT an enrolled agent. It exists so
that operator-only capabilities can be authorised against something an AGENT
credential can never satisfy: if an admin route reused agent authentication, any
enrolled agent could mint itself new identities. The principal is therefore
distinct in KIND — its own id namespace ("op:<bus-id>.<name>-<suffix>"), its own
durable record, its own session table and its own Go principal type.

USAGE
  agent-bus operator keygen -identity-dir <dir> [-json]
  agent-bus operator add    -data-dir <dir> -name <name> -auth-pub <base64>
                            -cert-fingerprint <64 hex> [-label <text>] [-json]
  agent-bus operator list   -data-dir <dir> [-all] [-json]
  agent-bus operator revoke -data-dir <dir> -id <operator-id> -reason <text> [-json]

KEYGEN AND ADD ARE SEPARATE COMMANDS ON PURPOSE. keygen runs on the OPERATOR's
machine and generates the operator's own credential — a TLS client certificate
and an Ed25519 session-signing keypair. THE PRIVATE KEYS NEVER LEAVE THAT
MACHINE AND THE BUS NEVER GENERATES THEM. It prints the two PUBLIC values (the
base64 auth public key and the certificate fingerprint) that "add" needs; those
are all the bus ever sees.

THE BUS MUST NOT BE RUNNING for "add" and "revoke": both append to the
write-ahead log and take the data directory's exclusive lock, which a running bus
holds. "list" reads the log and takes the same lock, so it too needs the bus
stopped. "keygen" touches no data directory at all.

REVOCATION TAKES EFFECT FOR A RUNNING BUS ONLY AFTER IT IS RESTARTED, TODAY.
That is not a property of revocation, it is a property of this build: there is no
online admin route yet, so the only writer of an operator record is this offline
command. Inside a running server a revocation is refused at the very next request
with no restart, because the authorization check re-reads the registry on every
call and a revoked operator's live sessions are dropped synchronously — an online
revoke is a one-line consumer of auth.OperatorRegistry.Revoke when that surface
lands. Do not read the offline restriction as "revocation is slow".

FLAGS
  -identity-dir <dir>       (keygen) the OPERATOR's own credential directory. It
                            is created if absent, mode 0700, and existing key
                            material is NEVER overwritten — it is loaded instead.
  -data-dir <dir>           (add/list/revoke) the bus's data directory (default
                            "./data"). It must already hold this bus's identity;
                            these commands NEVER create one.
  -name <name>              (add) the operator's short name, matching
                            ^[a-z0-9][a-z0-9_-]{0,63}$. The bus mints the id.
  -auth-pub <base64>        (add) the operator's Ed25519 session-signing PUBLIC
                            key, base64 standard encoding, as printed by keygen.
  -cert-fingerprint <hex>   (add) sha256 of the operator's client certificate
                            DER, 64 lowercase hex characters, as printed by
                            keygen. It is MANDATORY: invariant 11's
                            session/certificate cross-check can only be applied
                            unnarrowed if there is always a pair to cross-check.
  -label <text>             (add) an operator note recorded with the record.
  -all                      (list) include REVOKED operators, with the instant
                            and the reason.
  -id <operator-id>         (revoke) the operator to revoke.
  -reason <text>            (revoke) REQUIRED. An operator action must be loudly
                            attributable (invariant 6).
  -json                     emit the result as one JSON object on stdout, for
                            both success AND failure.
  -log-level <lvl>          log level for recovery/durability lines on stderr.

THERE IS NO -operator-id FLAG, and none may be added. The server is authoritative
on every id (invariant 1). Ids are never reused: revoking an operator does not
free its id, and adding one with a name that was used before mints a NEW suffix
and is therefore a DIFFERENT principal.

EXIT CODES
  0  success
  1  the command failed
  2  usage: bad flag, unknown subcommand, or invalid input
  3  the data directory is locked — a bus is running; stop it and retry
  4  the data directory does not hold a usable bus identity. Nothing is written
  5  the named operator is not registered
`

// ---------------------------------------------------------------------------
// Result shapes
// ---------------------------------------------------------------------------

// operatorKeygenResult is `operator keygen`'s output: the two PUBLIC values the
// bus needs, plus the paths so the operator can find and back up the private
// halves. No private key material is ever printed.
type operatorKeygenResult struct {
	OK              bool   `json:"ok"`
	IdentityDir     string `json:"identity_dir"`
	CertPath        string `json:"cert_path"`
	CertKeyPath     string `json:"cert_key_path"`
	AuthKeyPath     string `json:"auth_key_path"`
	AuthPublicKey   string `json:"auth_pub"`
	CertFingerprint string `json:"cert_fingerprint"`

	// CreatedCert and CreatedAuthKey report whether THIS RUN minted the
	// material, as opposed to loading what was already there. They are the
	// honest answer to "did running this change anything", which matters because
	// new material means a new fingerprint and therefore an operator record that
	// no longer matches.
	CreatedCert    bool `json:"created_cert"`
	CreatedAuthKey bool `json:"created_auth_key"`

	// Warnings are conditions the operator should be told about (an expired
	// certificate, tightened permissions) that do not stop the command.
	Warnings []string `json:"warnings,omitempty"`
}

// operatorRecordResult is one operator as `add`, `list` and `revoke` report it.
// It carries only PUBLIC values: a public key and a digest.
type operatorRecordResult struct {
	OperatorID      string `json:"operator_id"`
	Name            string `json:"name"`
	AuthPublicKey   string `json:"auth_pub"`
	CertFingerprint string `json:"cert_fingerprint"`
	Label           string `json:"label,omitempty"`
	CreatedAt       string `json:"created_at"`
	RevokedAt       string `json:"revoked_at,omitempty"`
	RevokedReason   string `json:"revoked_reason,omitempty"`
}

// operatorResult is the success shape for add, list and revoke. `ok` is the one
// field a caller branches on, present in the same position in the failure shape.
type operatorResult struct {
	OK        bool                   `json:"ok"`
	BusID     string                 `json:"bus_id"`
	Operators []operatorRecordResult `json:"operators"`

	// Unchanged is true when the command was a NO-OP because the requested
	// state already held — re-revoking an already-revoked operator (invariant
	// 10: same key + same payload is a legitimate retry, so it returns the
	// original result and writes nothing).
	Unchanged bool `json:"unchanged,omitempty"`

	// Warnings are conditions the operator should be told about that do NOT stop
	// the command — today, adding an operator over a certificate a REVOKED
	// operator held. They are in the JSON document rather than only on stderr for
	// operatorKeygenResult.Warnings' reason: an agent shelling out with
	// 2>/dev/null must still see them.
	Warnings []string `json:"warnings,omitempty"`
}

// operatorError is the --json failure shape.
type operatorError struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Remedy   string `json:"remedy,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// operatorCmdError carries the exit code and the remedy for the failures that
// have a specific one, so the run* functions map them without matching error
// text.
type operatorCmdError struct {
	code   int
	msg    string
	remedy string
	cause  error
}

func (e *operatorCmdError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *operatorCmdError) Unwrap() error { return e.cause }

func usageOperatorError(msg, remedy string) *operatorCmdError {
	return &operatorCmdError{code: exitOperatorUsage, msg: msg, remedy: remedy}
}

// operatorFail reports a failure in whichever mode was asked for and returns the
// exit code, so every failure path is one line at the call site.
//
// In --json mode the object goes to STDOUT, not stderr: an agent that redirected
// stderr away still gets a parseable answer, which is the whole reason --json
// exists (invariant 7's second audience).
func operatorFail(stdout, stderr io.Writer, asJSON bool, sub string, e *operatorCmdError) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(operatorError{OK: false, Error: e.Error(), Remedy: e.remedy, ExitCode: e.code}); err != nil {
			fmt.Fprintf(stderr, "agent-bus operator %s: %s\n", sub, e.Error())
		}
		return e.code
	}
	fmt.Fprintf(stderr, "agent-bus operator %s: %s\n", sub, e.Error())
	if e.remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", e.remedy)
	}
	return e.code
}

// ---------------------------------------------------------------------------
// keygen
// ---------------------------------------------------------------------------

// runOperatorKeygen is `agent-bus operator keygen`.
//
// It runs on the OPERATOR's machine, touches NO data directory, takes NO lock
// and writes NO bus state. It generates the two halves of an operator's own
// credential and prints the two PUBLIC values `operator add` needs.
//
// # WHY keygen AND add ARE SEPARATE COMMANDS
//
// THE PRIVATE KEYS NEVER LEAVE THE OPERATOR'S MACHINE AND THE BUS NEVER
// GENERATES THEM. A single command that both minted a keypair and recorded it
// would necessarily have held the private half on the bus host — in memory, in a
// process listing's argv, in a shell history, or in a file somebody forgot to
// delete. The split makes that structurally impossible: everything crossing from
// this command to `add` is a public key and a digest, both safe to paste into a
// ticket.
//
// It is NON-DESTRUCTIVE, the same rule client.LoadOrCreateClientCertificate
// documents: existing material is LOADED, never overwritten. Regenerating either
// half would silently invalidate an operator record the bus already holds — the
// fingerprint or the public key would no longer match — while looking like a
// no-op.
func runOperatorKeygen(args []string, stdout, stderr io.Writer) int {
	const sub = "keygen"
	var (
		identityDir string
		asJSON      bool
	)
	fs := flag.NewFlagSet("agent-bus operator keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A NO-OP Usage, deliberately: flag calls Usage both for -h and for a bad
	// flag, and those want DIFFERENT STREAMS — requested help is output and
	// belongs on stdout, an error is diagnostics and belongs on stderr. The two
	// cases are separated at the Parse call instead.
	fs.Usage = func() {}
	fs.StringVar(&identityDir, "identity-dir", "", "REQUIRED: the OPERATOR's own credential directory")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, operatorUsage)
			return exitOperatorOK
		}
		fmt.Fprint(stderr, operatorUsage)
		return exitOperatorUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, "agent-bus operator keygen: unexpected argument\n")
		return exitOperatorUsage
	}
	if identityDir == "" {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-identity-dir is required",
			"point it at a directory only this operator can read; it holds the PRIVATE half of this operator's credential and there is no safe default to guess"))
	}

	if err := os.MkdirAll(identityDir, operatorIdentityDirMode); err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, &operatorCmdError{
			code:  exitOperatorFailed,
			msg:   fmt.Sprintf("creating the identity directory %q", identityDir),
			cause: err,
		})
	}
	// MkdirAll applies operatorIdentityDirMode only to a directory it CREATES,
	// and not at all to one that already existed (nor, on a umask-less shell, to
	// the mode actually landed). An EXISTING loose directory is the case that
	// matters: directory WRITE permission for another local user is enough to
	// REPLACE the 0600 signing key with one of their own, at which point they can
	// complete this operator's challenges — the 0600 on the file protects its
	// contents, not its identity.
	dirWarnings := tightenOperatorIdentityDir(identityDir)

	// The TLS client certificate, through the SAME code path an agent uses
	// (client.LoadOrCreateClientCertificate): one implementation of "this
	// machine's client certificate" means one fingerprint construction, one set
	// of permissions and one non-destructive rule. cmd/agent-bus already imports
	// client in relaydial.go, so this is an established dependency rather than a
	// new one.
	cert, err := client.LoadOrCreateClientCertificate(identityDir)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, &operatorCmdError{
			code:   exitOperatorFailed,
			msg:    "preparing the operator's TLS client certificate",
			remedy: "check the identity directory's permissions and contents; existing material is never overwritten, so an incomplete directory must be repaired or removed deliberately",
			cause:  err,
		})
	}

	pub, created, err := loadOrCreateOperatorAuthKey(identityDir)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, &operatorCmdError{
			code:   exitOperatorFailed,
			msg:    "preparing the operator's session-signing key",
			remedy: "existing key material is never overwritten; if " + operatorAuthKeyFileName + " is damaged, move it aside deliberately — regenerating it invalidates any operator record the bus already holds",
			cause:  err,
		})
	}

	out := operatorKeygenResult{
		OK:              true,
		IdentityDir:     identityDir,
		CertPath:        cert.CertPath,
		CertKeyPath:     cert.KeyPath,
		AuthKeyPath:     filepath.Join(identityDir, operatorAuthKeyFileName),
		AuthPublicKey:   base64.StdEncoding.EncodeToString(pub),
		CertFingerprint: cert.Fingerprint(),
		CreatedCert:     cert.Created,
		CreatedAuthKey:  created,
		Warnings:        append(append([]string{}, cert.Warnings...), dirWarnings...),
	}
	if !cert.Created {
		// SILENT REUSE IS THE DANGEROUS DEFAULT. Loading existing material is the
		// documented non-destructive rule and it is right — but the operator
		// running this may believe they just minted a fresh identity, and the case
		// that costs something is pointing -identity-dir at an AGENT's directory:
		// the command would then bind ONE certificate to both an agent and an
		// operator, which `operator add` refuses outright (the cross-plane check)
		// and which, if it ever slipped through, is the exact credential collapse
		// invariant 11 exists to prevent, arrived at through the transport.
		//
		// It goes in Warnings rather than only on stderr for inviteBlob's
		// TransportInsecure reason: an agent shelling out with 2>/dev/null must
		// still see it, so a --json consumer gets it in the document.
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"REUSING the existing client certificate in %s; nothing was regenerated. Check this directory belongs to an OPERATOR and to nobody else — if it is an AGENT's identity directory, this binds one certificate to both an agent and an operator, and `operator add` will refuse the fingerprint", identityDir))
	}
	if cert.IsExpired(time.Now()) {
		out.Warnings = append(out.Warnings, "this operator's client certificate is OUTSIDE its validity window; the bus does not check expiry on a binding today, but replace it deliberately (generate a fresh identity directory, then `operator add` the new fingerprint and `operator revoke` the old principal)")
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "agent-bus operator keygen: the key material IS on disk, but writing the JSON result failed: %v\n", err)
			return exitOperatorFailed
		}
		return exitOperatorOK
	}
	writeOperatorKeygenHuman(stdout, out)
	return exitOperatorOK
}

// tightenOperatorIdentityDir repairs a loose identity directory and returns one
// warning per repair, or per repair it could not make.
//
// It is client/clientcert.go's tightenPermissions applied to the same kind of
// directory for the same reason, and it is deliberately the same shape: CHMOD
// AND WARN, never chmod in silence. Silence would hide the interval during which
// the material WAS exposed, and the mode is evidence about that interval.
//
// # DIRECTORY WRITE PERMISSION IS THE THREAT, NOT DIRECTORY READ
//
// The signing key is written 0600, so another local user cannot read it. But a
// world-writable directory lets them REPLACE it — unlink the file, drop in their
// own keypair — and this command NEVER overwrites existing material, so the next
// run would load the attacker's key and print the attacker's public half for
// `operator add`. The 0600 protects the key's contents; only the directory mode
// protects which key is there.
//
// A chmod that FAILS is a warning rather than a fatal error: the credential is
// still usable, refusing would leave the operator with no way forward on a
// filesystem that cannot express the mode, and the warning is what makes the
// exposure visible either way.
func tightenOperatorIdentityDir(dir string) []string {
	info, err := os.Stat(dir)
	if err != nil {
		return []string{fmt.Sprintf("could not check the permissions of the identity directory %s (%v); confirm by hand that no other local user can write to it — directory write permission is enough to REPLACE this operator's signing key", dir, err)}
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, operatorIdentityDirMode); err != nil {
		return []string{fmt.Sprintf("the identity directory %s is mode %#o and could NOT be tightened to %#o (%v); any local user with write permission here can REPLACE this operator's %s and then complete this operator's challenges — run: chmod 700 %s", dir, perm, operatorIdentityDirMode, err, operatorAuthKeyFileName, dir)}
	}
	return []string{fmt.Sprintf("the identity directory %s was mode %#o (other local users could reach it); tightened to %#o. If it was WRITABLE by others for any length of time, treat the operator credential as compromised: a writable directory lets a local user replace %s, and this command never overwrites existing material", dir, perm, operatorIdentityDirMode, operatorAuthKeyFileName)}
}

// loadOrCreateOperatorAuthKey returns the operator's Ed25519 session-signing
// PUBLIC key, generating the keypair on first use, and reports whether this call
// created it.
//
// The private half is written as PKCS#8 PEM at mode 0600 — the same encoding and
// the same mode buscert uses for the bus's own keys and client/clientcert.go for
// the agent's, so an operator debugging with `openssl pkey` does not have to
// learn a second format.
//
// IT NEVER OVERWRITES. An existing file is parsed and its public half returned;
// a damaged one is an ERROR, never a silent regeneration. Regenerating would
// look like a repair and would in fact revoke this operator's ability to
// authenticate, because the bus holds the OLD public key and nothing would say
// why the signature stopped verifying.
func loadOrCreateOperatorAuthKey(identityDir string) (ed25519.PublicKey, bool, error) {
	path := filepath.Join(identityDir, operatorAuthKeyFileName)

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		block, rest := pem.Decode(data)
		if block == nil {
			return nil, false, fmt.Errorf("%s does not hold a PEM block; it may be truncated or not PEM at all", path)
		}
		if block.Type != "PRIVATE KEY" {
			return nil, false, fmt.Errorf("%s holds a PEM %q block, want %q", path, block.Type, "PRIVATE KEY")
		}
		if len(rest) != 0 {
			return nil, false, fmt.Errorf("%s holds more than one PEM block; exactly one PRIVATE KEY is expected", path)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("%s does not hold a parseable PKCS#8 private key: %w", path, err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			// Refused rather than accommodated: invariant 9 says use the audited
			// high-level primitive, and here that is crypto/ed25519 — one
			// algorithm, no negotiation, nothing to downgrade.
			return nil, false, fmt.Errorf("%s holds a %T private key, want an Ed25519 key", path, key)
		}
		if len(priv) != ed25519.PrivateKeySize {
			return nil, false, fmt.Errorf("%s holds a %d byte Ed25519 private key, want %d", path, len(priv), ed25519.PrivateKeySize)
		}
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			return nil, false, fmt.Errorf("%s: the private key does not yield an Ed25519 public key", path)
		}
		return pub, false, nil

	case errors.Is(err, os.ErrNotExist):
		// Fall through to generation.

	default:
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}

	// crypto/ed25519 over crypto/rand, the audited stdlib primitive and nothing
	// hand-rolled (invariant 9).
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("generating the operator's Ed25519 session-signing keypair: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, false, fmt.Errorf("encoding the operator's session-signing key as PKCS#8: %w", err)
	}
	blob := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// O_EXCL: if another process won the race between the read above and here,
	// this fails rather than clobbering the winner's key — the non-destructive
	// rule enforced by the filesystem instead of by an argument about how narrow
	// the window is.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, operatorKeyFileMode)
	if err != nil {
		return nil, false, fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("fsyncing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("closing %s: %w", path, err)
	}
	// The directory entry too, so the file survives a crash rather than merely
	// its contents. A key that is not there after a power cut is a credential
	// the operator believes they hold.
	if d, err := os.Open(identityDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return pub, true, nil
}

// writeOperatorKeygenHuman is the default, readable output (invariant 7's first
// audience). It leads with what the operator must DO with the values, not with a
// field dump.
func writeOperatorKeygenHuman(w io.Writer, r operatorKeygenResult) {
	if r.CreatedCert || r.CreatedAuthKey {
		fmt.Fprint(w, "Operator credential ready. The PRIVATE halves stay on this machine.\n\n")
	} else {
		fmt.Fprint(w, "Operator credential loaded; nothing was regenerated.\n\n")
	}
	fmt.Fprintf(w, "  identity dir          %s\n", r.IdentityDir)
	fmt.Fprintf(w, "  tls certificate       %s\n", r.CertPath)
	fmt.Fprintf(w, "  tls private key       %s  (SECRET)\n", r.CertKeyPath)
	fmt.Fprintf(w, "  auth private key      %s  (SECRET)\n", r.AuthKeyPath)
	fmt.Fprint(w, "\nGive these two PUBLIC values to whoever runs the bus:\n\n")
	fmt.Fprintf(w, "  -auth-pub             %s\n", r.AuthPublicKey)
	fmt.Fprintf(w, "  -cert-fingerprint     %s\n", r.CertFingerprint)
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "\nWARNING: %s\n", warn)
	}
	fmt.Fprint(w, "\nThey are a public key and a digest: safe to paste into a ticket. The private\n"+
		"halves above are NOT, and the bus never sees them. On the bus host, with the bus\n"+
		"STOPPED, run:\n\n"+
		"  agent-bus operator add -data-dir <dir> -name <name> \\\n"+
		"      -auth-pub <the value above> -cert-fingerprint <the value above>\n")
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

// runOperatorAdd is `agent-bus operator add`: it registers a new operator
// principal durably.
func runOperatorAdd(args []string, stdout, stderr io.Writer) int {
	const sub = "add"
	var (
		dataDir  string
		name     string
		authPub  string
		certFP   string
		label    string
		asJSON   bool
		logLevel string
	)
	fs := flag.NewFlagSet("agent-bus operator add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; it must already hold this bus's identity")
	fs.StringVar(&name, "name", "", "REQUIRED: the operator's short name; the bus mints the id")
	fs.StringVar(&authPub, "auth-pub", "", "REQUIRED: the operator's Ed25519 session-signing PUBLIC key, base64 standard encoding")
	fs.StringVar(&certFP, "cert-fingerprint", "", "REQUIRED: sha256 of the operator's client certificate DER, 64 lowercase hex characters")
	fs.StringVar(&label, "label", "", "an operator note recorded with the operator")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for durability lines on stderr ("+logging.Levels+")")

	// THERE IS NO -operator-id FLAG, and none may be added — the same rule
	// runInviteMint states about -invite-id/-invite-secret, for the same reason.
	// Invariant 1: the server is authoritative on every id. An operator id whose
	// suffix a caller could choose would be predictable to whoever wrote the
	// command line, and — worse here than for an invite — it would let an
	// operator be added under an id a REVOKED principal already used, quietly
	// resurrecting a credential the log records as dead. auth.MintOperatorID has
	// no parameter for it either, which is what makes this structural rather
	// than a rule this file has to remember.

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, operatorUsage)
			return exitOperatorOK
		}
		fmt.Fprint(stderr, operatorUsage)
		return exitOperatorUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, "agent-bus operator add: unexpected argument\n")
		return exitOperatorUsage
	}
	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(fmt.Sprintf("invalid -log-level: %v", err), "use one of "+logging.Levels))
	}

	if err := auth.ValidateOperatorName(name); err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			fmt.Sprintf("invalid -name: %v", err),
			"operator names match "+auth.OperatorNamePattern+"; lowercase the name rather than expecting the bus to fold it"))
	}
	if authPub == "" {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-auth-pub is required",
			"run `agent-bus operator keygen -identity-dir <dir>` on the OPERATOR's machine and pass the auth public key it prints"))
	}
	pubBytes, err := base64.StdEncoding.DecodeString(authPub)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-auth-pub is not base64 (standard encoding)",
			"pass the value `operator keygen` printed, unmodified"))
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		// Refused HERE, at the door, because a wrong-size public key reaching
		// ed25519.Verify is a PANIC rather than a false.
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			fmt.Sprintf("-auth-pub decodes to %d bytes, want exactly %d", len(pubBytes), ed25519.PublicKeySize),
			"pass an Ed25519 public key, as printed by `operator keygen`"))
	}
	if certFP == "" {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-cert-fingerprint is required",
			"an operator MUST have a client certificate: invariant 11's session/certificate cross-check can only be applied unnarrowed if there is always a pair to cross-check. `operator keygen` prints the fingerprint"))
	}
	fp, err := buscert.ParseFingerprint(certFP)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			fmt.Sprintf("invalid -cert-fingerprint: %v", err),
			"pass the 64 lowercase hex characters `operator keygen` printed"))
	}

	lg := logging.New(stderr, lvl)
	busID, rec, warnings, cmdErr := applyOperatorAdd(dataDir, name, ed25519.PublicKey(pubBytes), [32]byte(fp), label, lg)
	if cmdErr != nil {
		return operatorFail(stdout, stderr, asJSON, sub, cmdErr)
	}
	return writeOperatorResult(stdout, stderr, asJSON, sub, operatorResult{
		OK:        true,
		BusID:     busID,
		Operators: []operatorRecordResult{rec},
		Warnings:  warnings,
	})
}

// applyOperatorAdd does the durable work: lock the data directory, load (never
// create) this bus's identity, replay the log into BOTH principal planes, mint
// the operator id and append the record.
// It also returns the non-fatal warnings the caller must report.
func applyOperatorAdd(dataDir, name string, pub ed25519.PublicKey, fp [32]byte, label string, lg *logging.Logger) (string, operatorRecordResult, []string, *operatorCmdError) {
	reg, roster, closeStore, busID, cmdErr := openOperatorRegistry(dataDir, true, lg)
	if cmdErr != nil {
		return "", operatorRecordResult{}, nil, cmdErr
	}
	defer closeStore()

	// ONE CERTIFICATE MUST NEVER NAME BOTH AN AGENT AND AN OPERATOR. This is the
	// cross-plane check, and it is the reason this command replays the ENROLMENT
	// roster as well as the operator registry.
	//
	// Without it the collapse this whole task exists to prevent is reachable
	// through the TRANSPORT instead of through a permission flag: one key holder
	// presents one certificate, and whichever plane is asked answers "yes, that
	// is my principal" — so an enrolled agent's own certificate would satisfy an
	// admin route's cross-check. That is the same failure as an admin route
	// reusing agent authentication, arrived at one layer down.
	//
	// ErrCertBindingUnknown is the ORDINARY answer and the only one that lets the
	// add proceed. An AMBIGUOUS answer (two agents holding one certificate,
	// reachable only off disk — invariant 6) is a refusal too: a certificate that
	// already resolves to nobody must not be handed a third meaning.
	if agentID, err := roster.AgentIDForCertFingerprint(fp); err == nil {
		return busID, operatorRecordResult{}, nil, &operatorCmdError{
			code:   exitOperatorFailed,
			msg:    fmt.Sprintf("this client certificate is already bound to the ENROLLED AGENT %q, so it cannot also name an operator", agentID),
			remedy: "generate a SEPARATE identity directory for the operator (`agent-bus operator keygen -identity-dir <a new dir>`) and add its fingerprint; one certificate naming both an agent and an operator would let one key holder satisfy an admin check with an agent credential",
		}
	} else if !errors.Is(err, auth.ErrCertBindingUnknown) {
		return busID, operatorRecordResult{}, nil, &operatorCmdError{
			code:   exitOperatorFailed,
			msg:    "this client certificate does not resolve to a single principal on the AGENT plane, so it must not be given another meaning",
			remedy: "inspect the enrolment roster: more than one agent holds a live binding for this certificate, which recovery can produce but the live path refuses to create",
			cause:  err,
		}
	}

	// A REVOKED OPERATOR'S FINGERPRINT DOES NOT BLOCK THE ADD, BUT IT MUST NOT
	// PASS IN SILENCE. A revoked binding constrains nothing (that is what
	// revocation means, and auth.OperatorRegistry.Add deliberately ignores it), so
	// re-adding the same certificate under a new id succeeds — which is right when
	// an operator's principal was revoked for an administrative reason and wrong
	// when it was revoked because the LAPTOP WAS STOLEN. Those two are
	// indistinguishable from here, and only the person running the command can
	// tell them apart, so it is told rather than guessed at.
	var warnings []string
	for _, o := range reg.List() {
		if o.Revoked() && o.CertFingerprint == fp {
			warnings = append(warnings, fmt.Sprintf(
				"this client certificate was held by the REVOKED operator %q (revoked %s: %q); a revoked binding does not block a new one, so the new operator is LIVE on the SAME certificate. If that principal was revoked because the key material was lost or stolen, STOP and generate a fresh identity directory instead",
				o.OperatorID, o.RevokedAt.UTC().Format(time.RFC3339Nano), o.RevokedReason))
		}
	}

	operatorID, err := auth.MintOperatorID(busID, name)
	if err != nil {
		return busID, operatorRecordResult{}, nil, &operatorCmdError{code: exitOperatorFailed, msg: "minting the operator id", cause: err}
	}

	now := time.Now().UTC()
	o := auth.Operator{
		OperatorID:      operatorID,
		Name:            name,
		AuthPublicKey:   pub,
		CertFingerprint: fp,
		Label:           label,
		CreatedAt:       now,
	}
	if err := reg.Add(o); err != nil {
		code := exitOperatorFailed
		remedy := ""
		if errors.Is(err, auth.ErrOperatorCertBound) || errors.Is(err, auth.ErrOperatorCertAmbiguous) {
			remedy = "this certificate already names another operator; run `agent-bus operator list` and either use that principal or generate a fresh identity directory"
		}
		return busID, operatorRecordResult{}, nil, &operatorCmdError{code: code, msg: "recording the operator", remedy: remedy, cause: err}
	}
	return busID, operatorRecordToResult(o), warnings, nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

// runOperatorList is `agent-bus operator list`. It is READ-ONLY: the registry is
// built with NO durable log, so every mutating call on it fails with
// auth.ErrNotAttached rather than being possible at all, and the log is read
// with wal.Replay, which repairs nothing and creates nothing.
//
// It still takes the data directory's exclusive lock, exactly as `peer list`
// does, so it needs the bus stopped. That is the lock discipline of this binary's
// offline commands and not a property of reading.
func runOperatorList(args []string, stdout, stderr io.Writer) int {
	const sub = "list"
	var (
		dataDir  string
		all      bool
		asJSON   bool
		logLevel string
	)
	fs := flag.NewFlagSet("agent-bus operator list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory")
	fs.BoolVar(&all, "all", false, "include REVOKED operators, with the instant and the reason")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for recovery lines on stderr ("+logging.Levels+")")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, operatorUsage)
			return exitOperatorOK
		}
		fmt.Fprint(stderr, operatorUsage)
		return exitOperatorUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, "agent-bus operator list: unexpected argument\n")
		return exitOperatorUsage
	}
	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(fmt.Sprintf("invalid -log-level: %v", err), "use one of "+logging.Levels))
	}
	lg := logging.New(stderr, lvl)

	reg, _, closeStore, busID, cmdErr := openOperatorRegistry(dataDir, false, lg)
	if cmdErr != nil {
		return operatorFail(stdout, stderr, asJSON, sub, cmdErr)
	}
	defer closeStore()

	out := operatorResult{OK: true, BusID: busID, Operators: []operatorRecordResult{}}
	for _, o := range reg.List() {
		if o.Revoked() && !all {
			continue
		}
		out.Operators = append(out.Operators, operatorRecordToResult(o))
	}
	return writeOperatorResult(stdout, stderr, asJSON, sub, out)
}

// ---------------------------------------------------------------------------
// revoke
// ---------------------------------------------------------------------------

// runOperatorRevoke is `agent-bus operator revoke`.
func runOperatorRevoke(args []string, stdout, stderr io.Writer) int {
	const sub = "revoke"
	var (
		dataDir    string
		operatorID string
		reason     string
		asJSON     bool
		logLevel   string
	)
	fs := flag.NewFlagSet("agent-bus operator revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory")
	fs.StringVar(&operatorID, "id", "", "REQUIRED: the operator id to revoke")
	fs.StringVar(&reason, "reason", "", "REQUIRED: why this operator is being revoked")
	fs.BoolVar(&asJSON, "json", false, "emit the result as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for durability lines on stderr ("+logging.Levels+")")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, operatorUsage)
			return exitOperatorOK
		}
		fmt.Fprint(stderr, operatorUsage)
		return exitOperatorUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprint(stderr, "agent-bus operator revoke: unexpected argument\n")
		return exitOperatorUsage
	}
	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(fmt.Sprintf("invalid -log-level: %v", err), "use one of "+logging.Levels))
	}
	if operatorID == "" {
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-id is required", "run `agent-bus operator list` to see the registered operators"))
	}
	if !auth.IsOperatorID(operatorID) {
		// The id is NOT echoed: it is unvalidated argv on its way to a terminal.
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-id is not a well-formed operator id",
			"an operator id looks like \"op:<bus-id>.<name>-<suffix>\"; an AGENT id is deliberately unable to be one, because an operator is not an agent"))
	}
	if reason == "" {
		// REQUIRED, and not defaulted to something bland. Invariant 6 wants an
		// operator action to be loudly attributable, and a revocation nobody can
		// explain six months later is the one an incident review needs most.
		return operatorFail(stdout, stderr, asJSON, sub, usageOperatorError(
			"-reason is required",
			"record why: a revocation is permanent, and an operator action must be attributable (invariant 6)"))
	}

	lg := logging.New(stderr, lvl)
	busID, rec, unchanged, cmdErr := applyOperatorRevoke(dataDir, operatorID, reason, lg)
	if cmdErr != nil {
		return operatorFail(stdout, stderr, asJSON, sub, cmdErr)
	}
	return writeOperatorResult(stdout, stderr, asJSON, sub, operatorResult{
		OK:        true,
		BusID:     busID,
		Operators: []operatorRecordResult{rec},
		Unchanged: unchanged,
	})
}

// applyOperatorRevoke appends the revocation record.
//
// RE-REVOKING IS A NO-OP SUCCESS (invariant 10: same key + same payload is a
// legitimate retry — return the original result, do not re-apply). It is
// reported as `unchanged` so a caller can tell a fresh revocation from a replay
// without having to compare timestamps.
func applyOperatorRevoke(dataDir, operatorID, reason string, lg *logging.Logger) (string, operatorRecordResult, bool, *operatorCmdError) {
	reg, _, closeStore, busID, cmdErr := openOperatorRegistry(dataDir, true, lg)
	if cmdErr != nil {
		return "", operatorRecordResult{}, false, cmdErr
	}
	defer closeStore()

	prev, known := reg.Get(operatorID)
	if !known {
		return busID, operatorRecordResult{}, false, &operatorCmdError{
			code:   exitOperatorUnknown,
			msg:    "no such operator on this bus",
			remedy: "run `agent-bus operator list -all` to see every operator this bus has registered, revoked ones included; check -data-dir names the bus you meant",
		}
	}
	alreadyRevoked := prev.Revoked()

	o, err := reg.Revoke(operatorID, reason, time.Now().UTC())
	if err != nil {
		if errors.Is(err, auth.ErrUnknownOperator) {
			return busID, operatorRecordResult{}, false, &operatorCmdError{
				code:   exitOperatorUnknown,
				msg:    "no such operator on this bus",
				remedy: "run `agent-bus operator list -all`",
				cause:  err,
			}
		}
		return busID, operatorRecordResult{}, false, &operatorCmdError{code: exitOperatorFailed, msg: "revoking the operator", cause: err}
	}
	return busID, operatorRecordToResult(o), alreadyRevoked, nil
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// openOperatorRegistry opens the data directory and rebuilds BOTH principal
// planes from the log: the operator registry and the enrolment roster.
//
// THE ROSTER IS NOT OPTIONAL EVEN THOUGH ONLY `add` READS IT. Building the same
// applier map for every subcommand is what keeps the two planes' replay
// identical: a map that varied per subcommand would be a place for `add` to see
// a roster `list` does not, which is exactly the drift that makes a cross-plane
// uniqueness check unreliable.
//
//   - writable=true takes the exclusive dirlock, opens the log with wal.Open and
//     attaches the registry, so it can append.
//   - writable=false builds the registry with NO durable log (every mutating
//     call fails with auth.ErrNotAttached) and rebuilds it with wal.Replay,
//     which is the package's read-only fsck: it repairs nothing, truncates
//     nothing and creates no file.
//
// The lock discipline is `openPeerStore`'s and `mintInvite`'s, unchanged, and the
// pre-lock/post-lock TOCTOU pair is load-bearing rather than belt-and-braces:
// the pre-lock check exists so a REFUSAL WRITES NOTHING AT ALL (including no
// bus.lock, which dirlock.Acquire creates and which the server reads as "this
// directory has history"), and the post-lock check is what makes
// ids.LoadOrCreateBusID's "Create" half unreachable — a directory whose bus-id
// file was lost would otherwise get a FRESHLY MINTED id persisted into it,
// renaming the bus away from every agent id it has ever issued (invariant 2).
//
// The returned close function closes the log and releases the lock; call it
// exactly once, deferred.
func openOperatorRegistry(dataDir string, writable bool, lg *logging.Logger) (*auth.OperatorRegistry, *auth.WALRoster, func(), string, *operatorCmdError) {
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil, nil, nil, "", &operatorCmdError{
			code:   exitOperatorNoIdentity,
			msg:    fmt.Sprintf("cannot read the data directory %q", dataDir),
			remedy: "check -data-dir; this command never creates one, because a typo would register an operator on a bus that does not exist",
			cause:  err,
		}
	}
	if !info.IsDir() {
		return nil, nil, nil, "", &operatorCmdError{
			code:   exitOperatorNoIdentity,
			msg:    fmt.Sprintf("-data-dir %q is not a directory", dataDir),
			remedy: "point -data-dir at the bus's data directory",
		}
	}
	if cmdErr := checkOperatorBusIDPresent(dataDir); cmdErr != nil {
		return nil, nil, nil, "", cmdErr
	}

	lock, err := dirlock.Acquire(dataDir)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return nil, nil, nil, "", &operatorCmdError{
				code:   exitOperatorBusRunning,
				msg:    "the data directory is locked by a live process, which is almost certainly the bus itself",
				remedy: "stop the bus, run this command, then start it again; operator records append to the write-ahead log and two writers would destroy it",
				cause:  err,
			}
		}
		return nil, nil, nil, "", &operatorCmdError{code: exitOperatorFailed, msg: "locking the data directory", cause: err}
	}
	releaseLock := func() {
		if err := lock.Release(); err != nil {
			lg.Error("releasing data directory lock failed", "data_dir", dataDir, "err", err)
		}
	}

	// RE-CHECK under the lock — see the doc comment.
	if cmdErr := checkOperatorBusIDPresent(dataDir); cmdErr != nil {
		releaseLock()
		return nil, nil, nil, "", cmdErr
	}

	// A LOAD, never a create: the check immediately above, taken under the lock,
	// is the only thing that makes LoadOrCreateBusID's "Create" half
	// unreachable.
	busID, err := ids.LoadOrCreateBusID(dataDir, "")
	if err != nil {
		releaseLock()
		return nil, nil, nil, "", &operatorCmdError{code: exitOperatorFailed, msg: "resolving the bus id", cause: err}
	}

	reg := auth.NewOperatorRegistry(lg)
	roster := auth.NewWALRoster(lg)

	// THE INVITE STORE IS A REQUIRED PARTICIPANT EVEN THOUGH NO OPERATOR
	// SUBCOMMAND READS IT.
	//
	// An invite-gated enrolment is written as ONE composite "agent+invite" record
	// (auth.EnrolInviteRecordKind), and MultiplexApplier splits it into the
	// enrolment half and the invite RIDER. With no applier registered for
	// invite.RecordKind, every such record made the multiplexer log at ERROR that
	// the invite "may be REDEEMABLE AGAIN until a restart with the applier
	// wired" — printed at the default -log-level warn, once per gated enrolment,
	// on a command that writes nothing to the invite plane at all. The claim was
	// FALSE IN THIS PROCESS (nothing here can redeem an invite) and false-alarming
	// an operator about a fail-open invite is exactly the noise that gets a real
	// invariant-6 discard line ignored.
	//
	// It is built with NO durable log, deliberately: this store is a replay
	// destination only, so every mutating call on it fails rather than being
	// possible — the same read-only shape `operator list` gives the registry.
	inviteStore, err := invite.NewStore(invite.StoreOptions{BusID: busID, Logger: lg})
	if err != nil {
		releaseLock()
		return nil, nil, nil, "", &operatorCmdError{code: exitOperatorFailed, msg: "creating the invite store", cause: err}
	}

	applier, err := auth.NewMultiplexApplier(lg, map[string]wal.Applier{
		auth.OperatorRecordKind: reg,
		auth.RecordKind:         roster,
		invite.RecordKind:       inviteStore,
	})
	if err != nil {
		releaseLock()
		return nil, nil, nil, "", &operatorCmdError{code: exitOperatorFailed, msg: "creating the write-ahead log applier", cause: err}
	}

	if !writable {
		if _, err := wal.Replay(filepath.Join(dataDir, wal.WALFileName), applier.Apply); err != nil {
			releaseLock()
			return nil, nil, nil, "", &operatorCmdError{
				code:   exitOperatorFailed,
				msg:    "replaying the write-ahead log",
				remedy: "the bus itself repairs a damaged log at startup and this read-only path deliberately does not; start the bus once and retry",
				cause:  err,
			}
		}
		return reg, roster, releaseLock, busID, nil
	}

	walLog, err := wal.Open(wal.LogOptions{Dir: dataDir, Logger: lg, Applier: applier})
	if err != nil {
		releaseLock()
		return nil, nil, nil, "", &operatorCmdError{
			code:   exitOperatorFailed,
			msg:    "opening the write-ahead log",
			remedy: "the bus itself refuses to start on the same error; fix it there first",
			cause:  err,
		}
	}
	closeStore := func() {
		if err := walLog.Close(); err != nil {
			lg.Error("closing the write-ahead log failed", "data_dir", dataDir, "path", walLog.Path(), "err", err)
		}
		releaseLock()
	}

	// STEP 3 of the three-step order (auth.WALRoster's type doc): only NOW, with
	// the whole log replayed, may the registry accept a write. Before this line
	// every mutating call refuses with auth.ErrNotAttached, which is the correct
	// order — recovery must finish before the first live append, or an add could
	// mint over state it has not yet seen.
	//
	// The ROSTER is deliberately NOT attached: this command never writes an
	// enrolment, and a roster that could write one from here would be a second
	// enrolment path outside the invite gate (invariant 3).
	if err := reg.Attach(walLog); err != nil {
		closeStore()
		return nil, nil, nil, "", &operatorCmdError{code: exitOperatorFailed, msg: "attaching the operator registry to the log", cause: err}
	}
	return reg, roster, closeStore, busID, nil
}

// checkOperatorBusIDPresent reports whether dataDir holds this bus's bus-id
// file.
//
// It is called TWICE — once before the lock so a refusal writes nothing at all,
// and once after it so the time-of-check/time-of-use window is closed rather
// than argued away. UNLIKE `invite mint` the CERTIFICATE is not required: an
// operator record pins no certificate of ours and puts none in any blob, so
// demanding one would refuse a legitimate directory for a file this command
// never reads (`peer` makes the same call for the same reason).
func checkOperatorBusIDPresent(dataDir string) *operatorCmdError {
	path := filepath.Join(dataDir, busIDFileName)
	if _, err := os.Stat(path); err != nil {
		return &operatorCmdError{
			code: exitOperatorNoIdentity,
			msg:  fmt.Sprintf("this data directory holds no bus id file (%q), so this bus has no identity to scope an operator to", path),
			remedy: "if this bus has never run, start it once against this -data-dir, stop it, then add operators; " +
				"if it HAS run, that file has been lost and must be restored from backup — this command will not recreate it, because a regenerated id would rename the bus away from every id it has issued. Nothing was written by this refusal",
			cause: err,
		}
	}
	return nil
}

// operatorRecordToResult renders one durable operator as the CLI reports it.
// Only PUBLIC values cross this boundary: a public key and a digest.
func operatorRecordToResult(o auth.Operator) operatorRecordResult {
	r := operatorRecordResult{
		OperatorID:      o.OperatorID,
		Name:            o.Name,
		AuthPublicKey:   base64.StdEncoding.EncodeToString(o.AuthPublicKey),
		CertFingerprint: buscert.Fingerprint(o.CertFingerprint).String(),
		Label:           o.Label,
		CreatedAt:       o.CreatedAt.UTC().Format(time.RFC3339Nano),
		RevokedReason:   o.RevokedReason,
	}
	if o.RevokedAt != nil {
		r.RevokedAt = o.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	return r
}

// writeOperatorResult emits a success in whichever mode was asked for.
func writeOperatorResult(stdout, stderr io.Writer, asJSON bool, sub string, out operatorResult) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			// The record IS durable at this point; only the report failed. Said
			// exactly, because "the command failed" would be a lie that costs an
			// operator id.
			fmt.Fprintf(stderr, "agent-bus operator %s: the operation SUCCEEDED and is durable, but writing the JSON result failed: %v\n", sub, err)
			return exitOperatorFailed
		}
		return exitOperatorOK
	}
	writeOperatorHuman(stdout, sub, out)
	return exitOperatorOK
}

// writeOperatorHuman is the default, readable output (invariant 7's first
// audience).
func writeOperatorHuman(w io.Writer, sub string, out operatorResult) {
	switch {
	case sub == "add":
		fmt.Fprintf(w, "Operator added to bus %s.\n\n", out.BusID)
	case sub == "revoke" && out.Unchanged:
		fmt.Fprintf(w, "Operator was ALREADY revoked on bus %s; nothing was written.\n\n", out.BusID)
	case sub == "revoke":
		fmt.Fprintf(w, "Operator revoked on bus %s.\n\n", out.BusID)
	case len(out.Operators) == 0:
		fmt.Fprintf(w, "Bus %s has no operators.\n", out.BusID)
		return
	default:
		fmt.Fprintf(w, "Operators on bus %s:\n\n", out.BusID)
	}
	for _, o := range out.Operators {
		fmt.Fprintf(w, "  operator id           %s\n", o.OperatorID)
		fmt.Fprintf(w, "  name                  %s\n", o.Name)
		fmt.Fprintf(w, "  cert fingerprint      %s\n", o.CertFingerprint)
		fmt.Fprintf(w, "  auth public key       %s\n", o.AuthPublicKey)
		fmt.Fprintf(w, "  created at            %s\n", o.CreatedAt)
		if o.Label != "" {
			// %q, NOT %s. The label is operator argv, length-bounded but not
			// charset-validated, so raw ANSI/control bytes would otherwise reach
			// the terminal verbatim — and the label is durable, so every future
			// `operator list` would replay them.
			fmt.Fprintf(w, "  label                 %q\n", o.Label)
		}
		if o.RevokedAt != "" {
			fmt.Fprintf(w, "  REVOKED at            %s\n", o.RevokedAt)
			fmt.Fprintf(w, "  revoked reason        %q\n", o.RevokedReason)
		}
		fmt.Fprint(w, "\n")
	}
	for _, warn := range out.Warnings {
		fmt.Fprintf(w, "WARNING: %s\n\n", warn)
	}
	if sub == "add" {
		fmt.Fprint(w, "The operator id is a NAME, not a credential: the operator's private keys never\n"+
			"left their machine and nothing secret was printed here. The id is never reused,\n"+
			"including after revocation.\n")
	}
}
