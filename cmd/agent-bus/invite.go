package main

// The operator-facing invite subcommand: `agent-bus invite mint`.
//
// # Why this is a SERVER SUBCOMMAND and not an HTTP route
//
// Decided 2026-08-02, DECISIONS.md E4 ("The first invite is minted
// server-side"): the minting authority is FILESYSTEM ACCESS to the data
// directory, exactly the model already used for wal-mac.key and the bus's
// private keys. Nothing new is exposed on the wire, so bootstrapping
// invite-only enrolment introduces no new network-reachable privilege — which
// is the whole point, given that invariant 3 exists because an unauthenticated
// enrolment route let an attacker mint its own agents. An admin HTTP route
// would have had to be authenticated by something, and the only credential
// available before any agent exists is the filesystem.
//
// # THE BUS MUST BE STOPPED. This is structural, not an oversight.
//
// Minting appends an invite record through internal/wal's two-phase path, and
// the data directory takes an EXCLUSIVE dirlock precisely so that two processes
// never append to one log. A running bus holds that lock, so this command
// refuses with exitInviteBusRunning and says so. The alternative — appending to
// a log another process is also appending to — destroys the log, and there is no
// version of that which is worth the convenience. Minting while the bus runs
// needs a route or an IPC channel and is deliberately NOT smuggled in here; it
// is filed as a separate follow-up rather than named by a key invented in a
// comment, because a task key nothing reserved is a dangling reference.
//
// # SCOPE: MINT ONLY.
//
// This does NOT gate enrolment. POST /v1/enroll still accepts callers with no
// invite at all — that is INVITE-GATE, and it is the task that makes an invite
// mean anything. A record written here is durable and will be replayed by the
// bus (both existing appliers ignore an unrecognised Entry.Kind), but nothing
// consumes it yet. Said plainly because "invites exist now" is the easiest
// wrong conclusion to draw from a working mint command.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/invite"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// inviteCommandName is the single non-flag argument main() intercepts before
// server flag parsing. Pinned as a constant so the dispatch in main.go and the
// usage text cannot drift apart.
const inviteCommandName = "invite"

// busIDFileName is the file within the data dir holding the persisted bus id.
//
// It DUPLICATES internal/ids/busid.go's unexported busIDFileName, which is the
// source of truth. Duplicated rather than exported because this command needs to
// ask "does this directory already have a bus id?" — a question
// ids.LoadOrCreateBusID cannot answer without ANSWERING IT BY CREATING ONE,
// which is precisely the behaviour that made checking necessary.
//
// The duplication is self-checking, not a hope:
// TestInviteMintBusIDFileNameMatchesIDsPackage builds a data directory through
// ids.LoadOrCreateBusID and asserts the file this constant names is the one that
// appeared. If internal/ids ever renames it, that test fails rather than this
// command silently deciding every data directory lacks a bus id.
const busIDFileName = "bus-id"

// Exit codes for `agent-bus invite`. These are a CONTRACT (invariant 7: an
// agent shelling out branches on them) and are documented in CONTRACTS-CLI.md.
//
// 3 and 4 are separate from the generic failure 1 because their remedies are
// OPPOSITE — one says stop the bus, the other says start it — and a caller that
// cannot tell them apart has to parse English to know which.
const (
	// exitInviteOK: the invite was minted and is durable.
	exitInviteOK = 0
	// exitInviteFailed: the mint failed and NO INVITE WAS RETURNED, so nothing is
	// redeemable by anyone.
	//
	// "Nothing was written" is ALMOST true and is deliberately not claimed: one
	// narrow path (Mint succeeded, the log Close did not) leaves a durable OPEN
	// record whose secret was discarded. It is unusable, but it exists and holds
	// a slot until it expires, so the message names it rather than pretending the
	// directory is untouched.
	exitInviteFailed = 1
	// exitInviteUsage: bad flags, an unknown subcommand, or a bad -bus-address.
	// Matches the server binary's existing "2 on invalid flags/config".
	exitInviteUsage = 2
	// exitInviteBusRunning: the data directory is locked by a live process.
	// Remedy: stop the bus, mint, start it again.
	exitInviteBusRunning = 3
	// exitInviteNoIdentity: the data directory does not hold a usable bus
	// identity — it is missing, is not a directory, lacks the bus-id file or the
	// certificate, or its certificate and bus id name DIFFERENT buses. The remedy
	// splits: start the bus once if it has never run, restore from backup if it
	// has. NOTHING IS WRITTEN on any of these paths.
	exitInviteNoIdentity = 4
)

// runInviteCommand dispatches `agent-bus invite <subcommand>`.
//
// It returns an exit code rather than calling os.Exit so that it is testable in
// process; main() is the only place that exits.
func runInviteCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, inviteUsage)
		return exitInviteUsage
	}
	switch args[0] {
	case "mint":
		return runInviteMint(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, inviteUsage)
		return exitInviteOK
	default:
		// The unknown subcommand is NOT echoed: it is unvalidated argv reaching a
		// stderr line, the same discipline invite.ValidateInviteID applies to an
		// oversized id. The usage text below tells the operator what is legal,
		// which is the useful half anyway.
		fmt.Fprintf(stderr, "agent-bus invite: unknown subcommand\n\n")
		fmt.Fprint(stderr, inviteUsage)
		return exitInviteUsage
	}
}

const inviteUsage = `agent-bus invite mint — mint a single-use, expiring invite

USAGE
  agent-bus invite mint -data-dir <dir> -bus-address <url> [-ttl <dur>] [-label <text>] [-json]

The invite id and the invite secret are minted by the SERVER from crypto/rand
(invariant 1). There is deliberately no flag that supplies either: an invite
whose secret a caller could choose would not be a credential.

THE BUS MUST NOT BE RUNNING. Minting appends to the write-ahead log and takes the
data directory's exclusive lock, which a running bus holds. Stop the bus, mint,
start it again.

FLAGS
  -data-dir <dir>    the bus's data directory (default "./data"). It must already
                     hold this bus's identity (its bus-id file AND certificate).
                     This command NEVER creates either: start the bus once if it
                     has never run, restore from backup if a file was lost.
  -bus-address <url> REQUIRED. The base URL an agent dials, e.g.
                     https://bus.example:8080. Part of the invite blob; there is
                     no default, because a guessed address produces an invite
                     that points somewhere the operator did not mean.
  -ttl <dur>         how long the invite stays redeemable (default 24h, max 168h).
                     Refused if over the maximum rather than silently clamped.
  -label <text>      an operator note recorded with the invite. NEVER shown to
                     whoever redeems it.
  -json              emit the invite blob as one JSON object on stdout.
  -log-level <lvl>   log-level for recovery/durability lines on stderr
                     (default "warn").

EXIT CODES
  0  the invite was minted and is durable
  1  the mint failed and NO INVITE WAS RETURNED, so nothing is redeemable by
     anyone. (One narrow case still leaves a durable record: if the write
     succeeded but closing the log did not, the message names the orphaned
     invite id — its secret was discarded, so it is unusable, but it holds a
     slot until it expires.)
  2  usage: bad flag, unknown subcommand, or a bad -bus-address
  3  the data directory is locked — a bus is running; stop it and retry
  4  the data directory does not hold a usable bus identity: it is missing, is
     not a directory, has no bus-id file or certificate, or its certificate and
     bus id name DIFFERENT buses. Start the bus once if it has never run;
     restore the missing file from backup if it has. Nothing is written.

THE SECRET IS PRINTED ONCE AND IS NOT RECOVERABLE. Only its SHA-256 digest is
stored. Whoever holds the four blob values can enrol ONE agent onto this bus.
`

// runInviteMint is `agent-bus invite mint`.
func runInviteMint(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir    string
		busAddress string
		ttl        time.Duration
		label      string
		asJSON     bool
		logLevel   string
	)
	fs := flag.NewFlagSet("agent-bus invite mint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A NO-OP Usage, deliberately. flag calls Usage both for -h and for a bad
	// flag, but those two want DIFFERENT STREAMS: requested help is output and
	// belongs on stdout, an error is diagnostics and belongs on stderr. Letting
	// flag print it would put -h on stderr — which is what this command did at
	// first, and it disagreed with `agent-bus invite -h` two lines below. The two
	// cases are separated at the Parse call instead.
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; it must already hold this bus's identity")
	fs.StringVar(&busAddress, "bus-address", "", "REQUIRED: the base URL an agent dials, e.g. https://bus.example:8080")
	fs.DurationVar(&ttl, "ttl", 0, "how long the invite stays redeemable; empty means the default")
	fs.StringVar(&label, "label", "", "an operator note recorded with the invite; never shown to whoever redeems it")
	fs.BoolVar(&asJSON, "json", false, "emit the invite blob as one JSON object on stdout")
	fs.StringVar(&logLevel, "log-level", "warn", "minimum log severity for durability lines on stderr ("+logging.Levels+")")

	// THERE IS NO -invite-id AND NO -invite-secret FLAG, and none may be added.
	// Invariant 1: the server is authoritative on every id, and the secret is a
	// bearer credential. A flag that let an operator supply either would make the
	// value predictable to whoever wrote the command line — in a shell history,
	// in a CI log, in a process list. invite.MintRequest has no field for them
	// either, which is what makes this structural rather than a rule this file
	// has to remember; TestInviteMintRejectsClientSuppliedSecret pins it.

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Requested help is OUTPUT: stdout, exit 0 — the same stream and code
			// as `agent-bus invite -h`.
			fmt.Fprint(stdout, inviteUsage)
			return exitInviteOK
		}
		// flag has already printed the specific error to stderr; the usage text
		// follows it there.
		fmt.Fprint(stderr, inviteUsage)
		return exitInviteUsage
	}
	if fs.NArg() > 0 {
		// The argument is NOT echoed: it is unvalidated argv on its way to a
		// terminal.
		fmt.Fprintf(stderr, "agent-bus invite mint: unexpected argument\n")
		return exitInviteUsage
	}

	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		return inviteFail(stdout, stderr, asJSON, exitInviteUsage, fmt.Sprintf("invalid -log-level: %v", err),
			"use one of "+logging.Levels)
	}
	busURL, err := parseInviteBusAddress(busAddress)
	if err != nil {
		return inviteFail(stdout, stderr, asJSON, exitInviteUsage, err.Error(),
			"pass -bus-address as the base URL an agent dials, e.g. https://bus.example:8080")
	}

	lg := logging.New(stderr, lvl)
	minted, fp, busID, err := mintInvite(dataDir, ttl, label, lg)
	if err != nil {
		var me *inviteMintError
		if errors.As(err, &me) {
			return inviteFail(stdout, stderr, asJSON, me.code, me.Error(), me.remedy)
		}
		return inviteFail(stdout, stderr, asJSON, exitInviteFailed, err.Error(), "")
	}

	blob := inviteBlob{
		OK: true,

		InviteID: minted.ID,

		BusID:              busID,
		BusAddress:         busURL,
		BusCertFingerprint: fp.String(),
		InviteSecret:       minted.Secret,

		CreatedAt: minted.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: minted.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Label:     minted.Label,
	}
	if strings.HasPrefix(busURL, "http://") {
		// Invariant 11: TLS is the required transport, and the LISTENER is TLS —
		// MTLS-LISTENER landed 2026-08-07 and the server now refuses to start
		// without usable key material. What is http here is the ADVERTISED bus
		// address, which an operator can still mis-configure independently of what
		// the listener serves. A redemption sent to that address does not reach
		// the TLS listener, so the secret crosses in cleartext and the fingerprint
		// in this blob pins nothing; a reader must not assume the pin is
		// protecting this invite.
		//
		// SIGNALLED IN BAND AS WELL AS ON STDERR. A stderr-only warning is
		// invisible to invariant 7's second audience — an agent shelling out with
		// --json and stderr discarded would receive a blob whose fingerprint pins
		// nothing, with nothing to branch on. TransportInsecure is that branch.
		blob.TransportInsecure = true
		lg.Warn("this invite names an http:// bus address, so the invite secret will cross the wire IN CLEARTEXT when it is redeemed, and the certificate fingerprint pins nothing. The listener itself serves https and only https; it is this ADVERTISED address that is plaintext, so re-mint the invite against the https address. Bounded only by the loopback default: do NOT expose this bus on a non-loopback interface with a plaintext advertised address",
			"bus_address", busURL, "invite_id", minted.ID)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(blob); err != nil {
			// The invite IS durable at this point; only the report failed. Say so
			// exactly, because "mint failed" would be a lie that costs an invite.
			fmt.Fprintf(stderr, "agent-bus invite mint: the invite %s was minted and IS durable, but writing the JSON blob failed: %v. The secret is now UNRECOVERABLE — revoke %s and mint another.\n", minted.ID, err, minted.ID)
			return exitInviteFailed
		}
		return exitInviteOK
	}
	writeInviteHuman(stdout, blob, minted.ExpiresAt)
	return exitInviteOK
}

// inviteBlob is the invite as an operator receives it: the TRUST ANCHOR of
// DECISIONS.md E6.
//
// The four fields that make it a trust anchor are BusID, BusAddress,
// BusCertFingerprint and InviteSecret. Together they are what lets an agent
// reach the right bus and verify it BEFORE its first connection, with no
// trust-on-first-use window — which is also why whoever can substitute this blob
// can point an agent at a bus of their choosing. Hand it over a channel whose
// integrity you trust.
//
// # This is the MINT COMMAND'S OUTPUT, not a settled wire shape
//
// Stated so nobody builds a parser against it believing it is fixed. There is
// deliberately NO single packed token here — no base64 of anything, no bespoke
// encoding — because the shape `client.EnrolOptions.Invite` will carry is
// settled by INVITE-CLIENT and is not this task's to choose. Inventing one here
// would be the same class of mistake as hand-picking a record-type number. Named
// JSON fields are the least-invented thing that carries all four values, and a
// packed form can be added later without retracting anything printed today.
type inviteBlob struct {
	// OK is true on this type by construction; it exists so that a caller
	// consuming --json can branch on one field for both success and failure,
	// which inviteError mirrors.
	OK bool `json:"ok"`

	// InviteID is the SERVER-MINTED id (invariant 1). It is a NAME, not a
	// credential: it is safe to log and to quote in a ticket. InviteSecret is
	// the credential.
	InviteID string `json:"invite_id"`

	BusID              string `json:"bus_id"`
	BusAddress         string `json:"bus_address"`
	BusCertFingerprint string `json:"bus_cert_fingerprint"`

	// InviteSecret is the PLAINTEXT BEARER CREDENTIAL, printed exactly once.
	// Only its SHA-256 digest is durable, so a lost secret is not recoverable —
	// revoke the invite and mint another.
	InviteSecret string `json:"invite_secret"`

	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`

	// TransportInsecure is true when BusAddress is plaintext http, meaning the
	// invite secret will cross the wire IN CLEARTEXT at redemption and
	// BusCertFingerprint pins NOTHING (invariant 11). The TLS listener landed at
	// MTLS-LISTENER, 2026-08-07 — this flag is about the ADVERTISED address being
	// plaintext, not the listener. It is emitted in band precisely so an
	// agent consuming --json with stderr discarded can still tell — a warning it
	// cannot see is a warning that does not exist. Omitted when false, so a
	// secure invite carries no field and the flag is only ever present as a
	// positive assertion of risk.
	TransportInsecure bool `json:"transport_insecure,omitempty"`

	// Label is the operator's own note. It is echoed back here because this
	// output goes to the OPERATOR; it must never be shown to whoever redeems the
	// invite, and internal/invite does not put it on any client-facing path.
	Label string `json:"label,omitempty"`
}

// inviteError is the --json failure shape. `ok` is the field a caller branches
// on, so it is present in both shapes and in the same position.
type inviteError struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Remedy   string `json:"remedy,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// inviteFail reports a failure in whichever mode was asked for and returns the
// exit code, so every failure path is one line at the call site.
//
// In --json mode the object goes to STDOUT, not stderr: an agent that redirected
// stderr away still gets a parseable answer, which is the whole reason --json
// exists (invariant 7's second audience).
func inviteFail(stdout, stderr io.Writer, asJSON bool, code int, msg, remedy string) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(inviteError{OK: false, Error: msg, Remedy: remedy, ExitCode: code}); err != nil {
			fmt.Fprintf(stderr, "agent-bus invite mint: %s\n", msg)
		}
		return code
	}
	fmt.Fprintf(stderr, "agent-bus invite mint: %s\n", msg)
	if remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", remedy)
	}
	return code
}

// writeInviteHuman is the default, readable output (invariant 7's first
// audience). It leads with what the operator must do with the thing, not with
// the field dump.
func writeInviteHuman(w io.Writer, b inviteBlob, expiresAt time.Time) {
	fmt.Fprintf(w, "Invite minted. It is SINGLE USE and expires %s (in %s).\n\n",
		b.ExpiresAt, time.Until(expiresAt).Round(time.Second))
	fmt.Fprintf(w, "  invite id             %s\n", b.InviteID)
	fmt.Fprintf(w, "  bus id                %s\n", b.BusID)
	fmt.Fprintf(w, "  bus address           %s\n", b.BusAddress)
	fmt.Fprintf(w, "  bus cert fingerprint  %s\n", b.BusCertFingerprint)
	fmt.Fprintf(w, "  invite secret         %s\n", b.InviteSecret)
	if b.Label != "" {
		// %q, NOT %s. The label is operator argv, length-bounded but not
		// charset-validated, so raw ANSI/control bytes would otherwise reach the
		// terminal verbatim — and the label is durable, so a future `invite list`
		// would replay them. %q renders them as escapes. The --json path needs no
		// equivalent: encoding/json already escapes everything below 0x20.
		fmt.Fprintf(w, "  label                 %q\n", b.Label)
	}
	if b.TransportInsecure {
		fmt.Fprint(w, "\nWARNING: the bus address above is plaintext http. The invite secret will cross\n"+
			"the wire IN CLEARTEXT when it is redeemed, and the fingerprint above pins nothing\n"+
			"until the bus serves https. Keep this bus on loopback until mTLS lands.\n")
	}
	fmt.Fprint(w, "\nThose four bus/secret values ARE the invite: they let an agent find this bus,\n"+
		"verify its certificate before the first connection, and enrol ONE agent.\n"+
		"The secret is shown ONCE and is not recoverable — only its SHA-256 digest is\n"+
		"stored. Hand the four values over a channel whose integrity you trust: whoever\n"+
		"can substitute them can point an agent at a bus of their choosing.\n")
}

// inviteMintError carries the exit code and the remedy for the failures that
// have a specific one, so runInviteMint maps them without matching error text.
type inviteMintError struct {
	code   int
	remedy string
	msg    string
	cause  error
}

func (e *inviteMintError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *inviteMintError) Unwrap() error { return e.cause }

// mintInvite does the durable work: lock the data directory, load (never
// create) this bus's identity, replay the log, mint, and return the invite with
// the bus's certificate fingerprint.
//
// The ORDER below mirrors run() in main.go deliberately — lock, then bus id,
// then certificate, then wal.Open — because the reasons run() gives for that
// order all apply here: replay must happen inside the lock, and a refusal must
// not have moved a byte of the log.
func mintInvite(dataDir string, ttl time.Duration, label string, lg *logging.Logger) (invite.Minted, buscert.Fingerprint, string, error) {
	var zero buscert.Fingerprint

	// The data directory is NOT created. run() does MkdirAll because a server is
	// entitled to start a fresh bus; this command is not, and a typo in
	// -data-dir that minted a whole new bus identity would hand the operator an
	// invite pinning a certificate no running bus serves. Refuse instead.
	info, err := os.Stat(dataDir)
	if err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{
			code:   exitInviteNoIdentity,
			msg:    fmt.Sprintf("cannot read the data directory %q", dataDir),
			remedy: "check -data-dir; this command never creates one, because a typo would mint a second bus identity",
			cause:  err,
		}
	}
	if !info.IsDir() {
		return invite.Minted{}, zero, "", &inviteMintError{
			code:   exitInviteNoIdentity,
			msg:    fmt.Sprintf("-data-dir %q is not a directory", dataDir),
			remedy: "point -data-dir at the bus's data directory",
		}
	}

	// REFUSE a directory that does not hold BOTH halves of this bus's identity —
	// the bus id file AND the certificate — BEFORE TAKING THE LOCK, so that this
	// refusal writes NOTHING AT ALL.
	//
	// BOTH are checked, and checking only the certificate was a real defect:
	// ids.LoadOrCreateBusID CREATES a bus id when the file is absent, so a
	// directory whose bus-id file had been lost (a partial restore, a stray rm)
	// got a FRESHLY MINTED bus id persisted into it before the CommonName
	// cross-check below noticed the mismatch and refused. The refusal then left
	// the bus PERMANENTLY RENAMED away from its own certificate, and because
	// run() has no such cross-check the next start adopts the new id happily —
	// at which point every agent id this bus ever issued
	// ("<bus-id>.<agent-id>", invariant 2) names a bus that no longer exists.
	// Reproduced end to end before this check was added.
	//
	// The ordering here is load-bearing and was got wrong once: dirlock.Acquire
	// CREATES bus.lock, and run() decides whether a data directory "has history"
	// by asking whether it was EMPTY at startup (dirIsEmpty, main.go). So a mint
	// refused after taking the lock on a virgin directory leaves a lone bus.lock
	// behind, and the operator's very first `agent-bus` start then refuses to
	// boot — openSuffixAllocator sees a non-empty directory with no
	// agent-suffixes file and demands -backfill-suffix-floors. In other words the
	// natural bootstrap mistake (mint first, then start the bus) would have
	// wedged the data directory. Measured, not theorised: it happened on the
	// first end-to-end run of this command.
	//
	// Checking outside the lock is a time-of-check/time-of-use gap. It is closed
	// by CHECKING AGAIN AFTER THE LOCK (below) rather than by an argument about
	// why the window is too small to matter — the pre-lock check exists to keep a
	// refusal from writing, and the post-lock check is the one that is actually
	// load-bearing for correctness.
	if e := checkBusIdentityPresent(dataDir); e != nil {
		return invite.Minted{}, zero, "", e
	}

	// The exclusive lock, BEFORE anything else reads or writes the directory —
	// the same non-negotiable ordering run() documents. This is also the check
	// that makes "the bus must be stopped" enforced rather than merely requested.
	lock, err := dirlock.Acquire(dataDir)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return invite.Minted{}, zero, "", &inviteMintError{
				code:   exitInviteBusRunning,
				msg:    "the data directory is locked by a live process, which is almost certainly the bus itself",
				remedy: "stop the bus, run this command, then start it again; minting appends to the write-ahead log and two writers would destroy it",
				cause:  err,
			}
		}
		return invite.Minted{}, zero, "", &inviteMintError{
			code:  exitInviteFailed,
			msg:   "locking the data directory",
			cause: err,
		}
	}
	defer func() {
		if err := lock.Release(); err != nil {
			lg.Error("releasing data directory lock failed", "data_dir", dataDir, "err", err)
		}
	}()

	// RE-CHECK, now that the directory is exclusively ours. This is the check
	// that makes the "never creates an identity" guarantee true rather than
	// merely likely: the pre-lock check runs while another process could still be
	// deleting bus-id, and losing that race regenerates one. Nothing can move
	// between here and the LoadOrCreateBusID call below, because holding the lock
	// is precisely the property the WAL depends on too.
	if e := checkBusIdentityPresent(dataDir); e != nil {
		return invite.Minted{}, zero, "", e
	}

	// A LOAD, never a create: the check immediately above — taken under the lock
	// — is the only thing that makes LoadOrCreateBusID's "Create" half
	// unreachable here.
	busID, err := ids.LoadOrCreateBusID(dataDir, "")
	if err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{code: exitInviteFailed, msg: "resolving the bus id", cause: err}
	}

	// Hosts is nil on purpose: it is consumed only when material is GENERATED,
	// and the stat above has already established it will not be. Passing
	// certHosts here would imply this command has an opinion about the listener,
	// which it must not — SANs are baked in at generation and are never revised.
	material, err := buscert.LoadOrCreate(dataDir, buscert.Options{BusID: busID})
	if err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{
			code:   exitInviteFailed,
			msg:    "loading the bus certificate",
			remedy: "the bus itself refuses to start on the same error; fix it there first",
			cause:  err,
		}
	}
	if material.Generated() {
		// Unreachable given the stat above, and refused rather than assumed away.
		// If it ever fires, this command has just minted a NEW bus identity, and
		// an invite pinning it would name a certificate no running bus serves —
		// which looks exactly like the certificate substitution the pin exists to
		// detect. Refuse before writing an invite; the material stays on disk for
		// the operator to inspect.
		return invite.Minted{}, zero, "", &inviteMintError{
			code:   exitInviteNoIdentity,
			msg:    fmt.Sprintf("this command GENERATED fresh bus key material in %q, which it must never do; no invite was minted", dataDir),
			remedy: "inspect the data directory: it was missing all three of " + buscert.CertFileName + ", " + buscert.TLSKeyFileName + " and " + buscert.SigningKeyFileName + " between the check and the load",
		}
	}
	if cn := material.Certificate().Subject.CommonName; cn != busID {
		// The certificate's CommonName is descriptive only — nothing
		// authenticates on it — but it is set to the bus id at generation, so a
		// disagreement means the certificate and the bus-id file come from
		// DIFFERENT buses. An invite built from that pair names one bus and pins
		// the other's certificate, and no client could ever redeem it.
		return invite.Minted{}, zero, "", &inviteMintError{
			code:   exitInviteNoIdentity,
			msg:    fmt.Sprintf("the bus certificate in %q names bus %q but this data directory's bus id is %q; the certificate and the bus id come from different buses", dataDir, cn, busID),
			remedy: "restore the matching certificate and bus-id file from backup; do not mint an invite pinning a certificate that belongs to another bus",
		}
	}

	// The invite store is the WAL's applier for this process, so replay rebuilds
	// the table before the capacity and id-collision checks in Mint run against
	// it. deferredLog resolves the cycle — the store needs a durable log at
	// construction and the log needs the store as its applier at Open — the same
	// shape auth.WALRoster.Attach uses for the roster.
	dl := &deferredLog{}
	store, err := invite.NewStore(invite.StoreOptions{BusID: busID, Durable: dl, Logger: lg})
	if err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{code: exitInviteFailed, msg: "creating the invite store", cause: err}
	}
	walLog, err := wal.Open(wal.LogOptions{Dir: dataDir, Logger: lg, Applier: store})
	if err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{
			code:   exitInviteFailed,
			msg:    "opening the write-ahead log",
			remedy: "the bus itself refuses to start on the same error; fix it there first",
			cause:  err,
		}
	}
	dl.log = walLog
	closed := false
	defer func() {
		if closed {
			return
		}
		if err := walLog.Close(); err != nil {
			lg.Error("closing the write-ahead log failed", "data_dir", dataDir, "path", walLog.Path(), "err", err)
		}
	}()

	minted, err := store.Mint(invite.MintRequest{Label: label, TTL: ttl})
	if err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{code: exitInviteFailed, msg: "minting the invite", cause: err}
	}

	// Close BEFORE returning success, and report a close failure as a FAILED
	// MINT. Mint has already fsynced both phases, so the record is durable — but
	// a failing Close is a durability signal about the file as a whole, and an
	// operator is better served by minting again than by trusting an invite whose
	// log could not be closed cleanly. The invite id is named so the extra record
	// is identifiable.
	closed = true
	if err := walLog.Close(); err != nil {
		return invite.Minted{}, zero, "", &inviteMintError{
			code: exitInviteFailed,
			msg:  fmt.Sprintf("invite %s was written, but closing the write-ahead log failed, so its durability is not trustworthy; it is NOT reported and its secret is discarded", minted.ID),
			// The orphan is named because it is REAL: the record is durable and
			// OPEN, so it holds a slot in the bounded invite table with a secret
			// nobody holds. Unguessable, therefore not a way in — but on flaky
			// storage a "just mint again" remedy leaks a slot per attempt, and an
			// operator who is not told the id cannot reclaim it.
			remedy: "check the data directory and the underlying storage, then mint again; invite " + minted.ID +
				" is now an unusable ORPHAN holding a slot in the invite table — revoke it (INVITE-REVOKE) once that surface exists",
			cause: err,
		}
	}
	return minted, material.Fingerprint(), busID, nil
}

// checkBusIdentityPresent reports whether dataDir holds BOTH halves of a bus
// identity: the bus id file and the certificate.
//
// BOTH are checked, and checking only the certificate was a real defect:
// ids.LoadOrCreateBusID CREATES a bus id when the file is absent, so a directory
// whose bus-id file had been lost (a partial restore, a stray rm) got a FRESHLY
// MINTED bus id persisted into it before the CommonName cross-check noticed the
// mismatch and refused. The refusal then left the bus PERMANENTLY RENAMED away
// from its own certificate, and because run() has no such cross-check the next
// start adopts the new id happily — at which point every agent id this bus ever
// issued ("<bus-id>.<agent-id>", invariant 2) names a bus that no longer exists.
// Reproduced end to end before this check was added.
//
// It is called TWICE: once before the lock so that a refusal writes nothing at
// all, and once after it so the time-of-check/time-of-use window is closed
// rather than argued away.
func checkBusIdentityPresent(dataDir string) *inviteMintError {
	for _, want := range []struct{ path, what string }{
		{filepath.Join(dataDir, busIDFileName), "bus id file"},
		{filepath.Join(dataDir, buscert.CertFileName), "bus certificate"},
	} {
		if _, err := os.Stat(want.path); err != nil {
			return &inviteMintError{
				code: exitInviteNoIdentity,
				msg:  fmt.Sprintf("this data directory holds no %s (%s), so this bus has no identity for an invite to pin", want.what, want.path),
				remedy: "if this bus has never run, start it once against this -data-dir and let it come up, stop it, then mint; " +
					"if it HAS run, that file has been lost and must be restored from backup — this command will not recreate it, because a regenerated one would rename the bus away from its own certificate. Nothing was written by this refusal",
				cause: err,
			}
		}
	}
	return nil
}

// deferredLog satisfies invite.DurableLog before the log it delegates to
// exists.
//
// The cycle it breaks is real: invite.NewStore takes its DurableLog at
// construction, and wal.Open takes its Applier at Open, so one of the two must
// be handed a value that is not ready yet. Giving the STORE a deferred log is
// the safe direction, because nothing in the replay path writes — Store.Apply
// only folds records into memory — so Write cannot be reached before Open
// returns. The nil check below is what turns "cannot be reached" into an error
// instead of a nil dereference if that ever stops being true.
type deferredLog struct{ log *wal.Log }

func (d *deferredLog) Write(e wal.Entry) (wal.Committed, error) {
	if d.log == nil {
		return wal.Committed{}, errors.New("agent-bus: the durable log is not open yet; nothing may be written before replay completes")
	}
	return d.log.Write(e)
}

// parseInviteBusAddress validates -bus-address and returns its canonical form.
//
// The rule deliberately MIRRORS client.parseBusURL (client/config.go), because
// the value printed here is the value that is later passed to the client as
// --bus: https anywhere, http ONLY to a loopback host. A blob carrying an
// address the client will refuse is a blob that fails at the worst moment, in
// the hands of whoever is least able to diagnose it.
//
// It is duplicated rather than imported because cmd/agent-bus is the SERVER
// binary and must not take a dependency on the client package to validate a
// string. The two are pinned together by TestInviteMintBusAddressRules.
func parseInviteBusAddress(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("-bus-address is required: it is the address an agent dials, and there is no safe default to guess")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("-bus-address is not a URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return "", fmt.Errorf("-bus-address %q has no scheme; include it, e.g. https://%s", trimmed, trimmed)
	default:
		return "", fmt.Errorf("-bus-address scheme %q is not supported; use http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("-bus-address %q has no host; use a full base URL such as https://127.0.0.1:8080", trimmed)
	}
	if u.User != nil {
		return "", errors.New("-bus-address must not carry userinfo: credentials do not belong in a URL, and this one is handed to an agent")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("-bus-address must not carry a query or a fragment: pass only the scheme, host and any path prefix")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("-bus-address %q has no host; use a full base URL such as https://127.0.0.1:8080", trimmed)
	}
	if u.Scheme == "http" && !isLoopbackHost(host) {
		return "", fmt.Errorf("-bus-address %q is plaintext http to a non-loopback host; the invite secret is a bearer credential and would cross the wire in the clear (invariant 11). Use https, or a loopback address", trimmed)
	}
	// CANONICALISE the same way client.parseBusURL does — lower-case the scheme
	// and host, drop a default port, trim a trailing slash — so that the address
	// printed in the blob is byte-identical to the one the client will derive
	// from it. The client uses this string as an idempotency SCOPE KEY, so two
	// spellings of one bus are two scopes; an invite that seeded the non-canonical
	// spelling would push that divergence onto every agent it admitted.
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = canonicalInviteHost(u.Scheme, u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String(), nil
}

// canonicalInviteHost lower-cases the host and drops the port when it is the
// scheme's default. It mirrors client.canonicalHost (client/config.go), with ONE
// DELIBERATE DIVERGENCE, called out here because the mirroring is otherwise the
// point and a silent difference would be worse than either behaviour.
//
// THE DIVERGENCE: an IPv6 literal is RE-BRACKETED when the default port is
// dropped. net.SplitHostPort strips the brackets, so returning the bare host
// turns "[::1]:443" into "::1" — which is not a parseable URL host, and would
// put an UNDIALLABLE address in the invite blob, i.e. in the trust anchor
// itself. client.canonicalHost has this bug today (both tables only cover a
// non-default IPv6 port, so neither caught it); it fails closed there, and it is
// filed as a follow-up against client/ rather than fixed here, because client/
// is outside this task's ownership.
//
// Re-bracketing also interoperates: the client canonicalising "https://[::1]"
// takes SplitHostPort's no-port error path and lower-cases it unchanged, so both
// ends agree on the string.
func canonicalInviteHost(scheme, host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port present — SplitHostPort's error case for a bare host. An IPv6
		// literal is already bracketed here and stays that way.
		return strings.ToLower(host)
	}
	h = strings.ToLower(h)
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		if strings.Contains(h, ":") {
			// An IPv6 literal: SplitHostPort removed the brackets that make it a
			// legal URL host, so put them back.
			return "[" + h + "]"
		}
		return h
	}
	return net.JoinHostPort(h, port)
}

// isLoopbackHost reports whether a URL host is the loopback interface, by the
// same test client.parseBusURL applies: the literal name "localhost", or an IP
// literal that net.IP.IsLoopback accepts. A NAME that merely resolves to
// loopback does not count — resolution is not a property of the string, and it
// can change under the invite.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
