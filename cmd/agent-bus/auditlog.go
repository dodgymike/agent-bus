package main

// `agent-bus log` (CLI-6): read the append-only MESSAGE AUDIT TRAIL —
// bus.audit — and print it as METADATA ONLY.
//
// # WHAT THIS PRINTS, AND THE ONE THING IT NEVER PRINTS
//
// Invariant 6: the append-only log records METADATA AND ROUTING ONLY. Message
// id, sequence, sender, recipient(s), the ORDERED bus path traversed, the
// server's timestamp, the body's size and the content hash. It does NOT record
// message bodies, and it never will — internal/wal/audit.go states the reason at
// length and it is a decision (2026-08-02), not an omission: agent-bus is
// getting end-to-end encryption with forward secrecy, so a trail holding
// plaintext becomes unwritable the moment PFS lands, and a trail holding
// ciphertext this bus can never decrypt is dead weight.
//
// The consequence for THIS file is a rule with no exceptions: the output is
// composed FIELD BY FIELD from a decoded wal.AuditRecord, which has no body
// field. wal.Record.Payload — the raw on-disk JSON — is never printed, never
// embedded, and there is no "raw" or catch-all field for it. A future field is
// added by naming it here, not by dumping the frame. Anything that puts a body
// in the output of this command is the exact defect invariant 6 exists to
// prevent, and it would be indistinguishable from a working command in every
// positive test.
//
// # ON THE SERVER BINARY, NOT ON agent-busctl
//
// For `invite mint`'s reason (DECISIONS.md E4), restated by `peer add` and
// `key export-public`: the authority this command needs is FILESYSTEM ACCESS to
// the data directory, not a network privilege. There is no HTTP route that
// serves the audit trail and this command does not add one. agent-busctl is a
// pure HTTP client — it imports only client/, and has no data-directory or
// dirlock plumbing — so putting an offline file reader there would mean giving
// the network client filesystem and lock access to satisfy a spelling.
//
// It is still THE compiled client for this capability (invariant 7): an operator
// or an agent gets the trail by running a subcommand with --json, never by
// hand-parsing bus.audit or reaching for a shell wrapper.
//
// # THE BUS MUST BE STOPPED — and that is also why there is NO --follow
//
// This command takes the data directory's EXCLUSIVE dirlock, which a running bus
// holds, for `peer list`'s reason rather than because it writes: it writes
// nothing at all. A read racing an append can see a half-written tail record,
// and would then either report a message that is not yet durable or report
// perfectly healthy bus as damaged. One rule ("stop the bus to read the trail")
// is also easier to get right than a second consistency mechanism.
//
// A --follow/tail mode is therefore DEFERRED (CLI-6-FU-FOLLOW) rather than
// omitted by oversight: while this command holds the exclusive lock no bus is
// running, so nothing is appending to the file, and tailing a file nobody writes
// to is not a capability. Delivering it needs a lock-free consistent read first,
// which is its own decision.
//
// # STRICT AND LOUD, BECAUSE A QUIET AUDIT READER IS WORSE THAN NO READER
//
// Invariant 6's absolute requirement is that every discard is LOGGED, loudly and
// specifically — silent discard is the defect, not discard itself. A reader that
// skipped a bad record would report a CLEAN TRAIL OVER A DAMAGED ONE, which is
// the same failure wearing a reader's clothes. So:
//
//   - wal.ScanAll is used, not wal.RepairLog and not Replay. It repairs nothing,
//     truncates nothing and writes nothing; it returns every record it could
//     read in file order plus the precise offset at which the file stops being
//     trustworthy.
//   - Damage found by the scan is reported on STDERR naming the path, the byte
//     offset and the reason, AND (in --json) as a final `{"damaged":true,…}`
//     NDJSON object. Every record that WAS readable is still printed first.
//   - A record that is not a TypeAuditMessage, or that wal.DecodeAudit refuses,
//     is reported the same loud way and the read CONTINUES to the remaining
//     records. It is never skipped silently.
//   - FILTERS NEVER SUPPRESS DAMAGE. Damage is reported before any filter is
//     consulted, so `-sender nobody` on a damaged trail still says so and still
//     exits 1. A filter that could hide corruption would make this command's
//     silence meaningless.
//
// Any damage at all is exit 1, even when every readable record printed fine.
//
// # AND WHAT IT CANNOT AUTHENTICATE, IT REFUSES TO READ AT ALL (exit 5)
//
// Loud reporting of damage is the answer for a file this command has the
// standing to judge. Two states remove that standing entirely, and for those the
// answer is a refusal BEFORE the scan, with nothing printed as a record:
//
//   - NO wal-mac.key IN THE DATA DIRECTORY. Integrity here is a keyed MAC
//     (invariant 6); with no key nothing can be authenticated, and — worse —
//     wal would MINT one as a side effect of the read. See readAuditTrail.
//   - bus.audit IS NOT FORMAT VERSION 2. Version 1 frames carry an UNKEYED
//     CRC32C anyone can compute, so wal would happily "verify" records an
//     attacker wrote. See checkAuditFormatVersion.
//
// Both say the same thing — THIS READER CANNOT VOUCH FOR THESE BYTES — which is
// why they share one exit code. Printing a record on either path would be the
// invariant-6 defect in its most dangerous form: not a discard gone quiet, but a
// forgery presented as provenance.
//
// # THE --json SHAPE IS NDJSON, ONE OBJECT PER LINE
//
// One JSON object per record per line, so it streams and so a consumer can
// count. scripts/fed-smoke.sh selects on `.message_id` and `.bus_path` and
// requires EXACTLY ONE match per message, which is why no non-record object this
// command emits — not the damage object, not the failure object — may ever carry
// a `message_id` field.
//
// Field names mirror wal's auditPayload JSON tags exactly (message_id, seq,
// sender, broadcast, recipients, bus_path, sent_at, size, content_sha256,
// prepare_index) so an operator reading the file and an operator reading this
// output see the same words, plus two frame-level locators this command can see
// and the payload cannot: audit_index and offset.
//
// `bus_path` and `recipients` are ALWAYS emitted and are never null: a nil slice
// is normalised to []. bus_path being present and ORDERED on every record is the
// point of this task, so an empty path must be visibly [] rather than missing,
// and the order wal read is preserved exactly — nothing here sorts either slice.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// logCommandName is the single non-flag argument main() intercepts before server
// flag parsing. Pinned as a constant so the dispatch in main.go and the usage
// text cannot drift apart.
const logCommandName = "log"

// Exit codes for `agent-bus log`. These are a CONTRACT (invariant 7: an agent or
// an operator's script shells out and branches on them) and are the same numbers
// with the same meanings as `agent-bus invite`, `peer` and `key`, so an operator
// driving several of them does not have to hold four tables in their head.
const (
	// exitLogOK: the WHOLE trail was read and every record in it decoded. It is
	// still 0 when a filter matched nothing — an empty result is an answer.
	exitLogOK = 0
	// exitLogDamaged: the trail is DAMAGED or unreadable. Every record that
	// could be read has still been printed, and every discard has been named on
	// stderr (and as a `damaged` object under --json). This is also the code for
	// a zero-length bus.audit, which wal reports as a file with no header.
	exitLogDamaged = 1
	// exitLogUsage: bad flags, an unexpected argument, or an unparseable
	// -since/-until. Nothing was read.
	exitLogUsage = 2
	// exitLogBusRunning: the data directory is locked by a live process, which
	// is almost certainly the bus itself. Remedy: stop the bus and retry.
	exitLogBusRunning = 3
	// exitLogNoTrail: this data directory holds no bus.audit at all, so any
	// messages it routed have NO provenance record. Distinct from 1 because the
	// remedy is different and because "there is no trail" and "the trail is
	// broken" must never be reported as the same thing.
	exitLogNoTrail = 4
	// exitLogUnverifiable: THIS READER CANNOT VOUCH FOR THESE BYTES. The trail
	// exists, but it cannot be AUTHENTICATED — either the MAC key that
	// authenticates it is absent, or the file does not identify itself as a
	// format version 2 trail, which is the only version this bus has ever
	// written and the only one whose integrity rests on a keyed MAC rather than
	// on a checksum anyone can compute.
	//
	// NOTHING WAS READ AND NOTHING SHOULD BE BELIEVED. It is distinct from 1 for
	// the reason 4 is: "the trail is damaged" invites recovery of the readable
	// part, while this says the readable part carries no authority in the first
	// place, so there is nothing here to salvage and printing any of it would
	// have been the defect. It is a refusal BEFORE the scan, not a report of a
	// scan, so no record is printed on this path.
	exitLogUnverifiable = 5
)

// logUsage is printed for -h (stdout, exit 0) and beside a usage error (stderr).
//
// It MUST keep the words "metadata only": that phrase is what tells an operator,
// in the place they will actually look, that no body is retrievable from this
// command however hard they try. CLI-6's stored proof greps for it.
const logUsage = `agent-bus log — read this bus's append-only MESSAGE AUDIT TRAIL (metadata only)

USAGE
  agent-bus log [-data-dir <dir>] [-json] [-sender <id>] [-recipient <id>]
                [-since <RFC3339>] [-until <RFC3339>] [-min-seq <n>] [-max-seq <n>]

WHAT IS IN THE TRAIL — AND WHAT IS NOT
  This trail is metadata only: routing and provenance. Each record carries the
  message id, sequence, sender, broadcast flag or recipient list, the ORDERED
  bus path the message traversed, the time this bus accepted it, the body's
  size in bytes, and the SHA-256 that identifies the content.

  MESSAGE BODIES ARE NOT RECORDED. They are not in this file and they cannot be
  recovered from it, by this command or any other — that is a deliberate design
  decision (invariant 6), taken so the trail stays compatible with end-to-end
  encrypted payloads. The content hash is what preserves the ability to prove
  WHAT was sent without retaining it.

  The trail is a SUPERSET of committed history: a crash between the audit write
  and the commit write leaves a record for a message that never became accepted
  history. prepare_index names the write-ahead-log transaction, so an audit
  record can be paired with the WAL entry that (may have) committed it.

THE BUS MUST NOT BE RUNNING. This takes the data directory's exclusive lock,
which a running bus holds. It writes nothing and repairs nothing, but holding
the lock is what stops a read from seeing a half-written tail record and
reporting a healthy bus as damaged. There is deliberately no --follow: while
this command holds the lock, nothing is appending.

DAMAGE IS ALWAYS REPORTED, AND FILTERS NEVER HIDE IT. A record that will not
decode is named on stderr with its offset and the reason, the remaining records
are still read, and the command exits 1 — even if a filter excluded every
record. Silence from this command means the trail really is intact.

A TRAIL THIS COMMAND CANNOT AUTHENTICATE IS REFUSED, NOT PRINTED (exit 5). Two
cases: the data directory holds no ` + wal.MACKeyFileName + `, so no record's keyed MAC can be
checked at all; or ` + wal.AuditFileName + ` does not identify itself as format version 2, the
only version this bus has ever written. Version 1 frames are authenticated by an
UNKEYED checksum that anyone can compute, so a version 1 file is not evidence of
anything. In both cases nothing is read and nothing is printed as a record.

FLAGS
  -data-dir <dir>     the bus's data directory (default "./data"). It must
                      already hold ` + wal.AuditFileName + `; this command never creates one.
  -json               emit NDJSON: one JSON object per record, one per line,
                      plus a final {"damaged":true,…} object if anything was
                      unreadable. Field names match the on-disk record.
  -sender <id>        only records whose sender is EXACTLY this fully-qualified
                      "<bus-id>.<agent-id>".
  -recipient <id>     only records whose recipient list CONTAINS this id. A
                      broadcast never matches: it carries no recipient list, so
                      matching it would be a guess about a roster this trail
                      deliberately does not record.
  -since <RFC3339>    only records with sent_at >= this instant (INCLUSIVE).
  -until <RFC3339>    only records with sent_at <  this instant (EXCLUSIVE), so
                      that -until X -since X on the next run covers every record
                      exactly once with no gap and no overlap.
  -min-seq <n>        only records with seq >= n (inclusive). 0 means no bound.
  -max-seq <n>        only records with seq <= n (inclusive). 0 means no bound;
                      sequences start at 1, so 0 can never exclude a real record.

EXIT CODES
  0  the whole trail was read and every record decoded
  1  the trail is damaged or unreadable; every readable record was still
     printed and every discard was named on stderr
  2  usage: bad flag, unexpected argument, or an unparseable -since/-until
  3  the data directory is locked by a live process — stop the bus and retry
  4  this data directory holds no ` + wal.AuditFileName + `, so it has no audit trail at all
  5  the trail cannot be AUTHENTICATED — ` + wal.MACKeyFileName + ` is missing, or the file is
     not format version 2. Nothing was read and nothing should be believed
`

// runLogCommand is `agent-bus log`.
//
// It returns an exit code rather than calling os.Exit so that it is testable in
// process; main() is the only place that exits.
func runLogCommand(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir   string
		asJSON    bool
		sender    string
		recipient string
		since     string
		until     string
		minSeq    uint64
		maxSeq    uint64
	)
	fs := flag.NewFlagSet("agent-bus "+logCommandName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A NO-OP Usage, for the reason runKeyExportPublic documents: flag calls
	// Usage both for -h and for a bad flag, but requested help is OUTPUT and
	// belongs on stdout while an error is diagnostics and belongs on stderr. The
	// two cases are separated at the Parse call instead.
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; it must already hold "+wal.AuditFileName)
	fs.BoolVar(&asJSON, "json", false, "emit NDJSON: one JSON object per record, one per line")
	fs.StringVar(&sender, "sender", "", "only records sent by exactly this fully-qualified id")
	fs.StringVar(&recipient, "recipient", "", "only records whose recipient list contains this fully-qualified id; a broadcast never matches")
	fs.StringVar(&since, "since", "", "only records with sent_at at or after this RFC3339 instant (inclusive)")
	fs.StringVar(&until, "until", "", "only records with sent_at strictly before this RFC3339 instant (exclusive)")
	fs.Uint64Var(&minSeq, "min-seq", 0, "only records with seq at or above this value (inclusive); 0 means no bound")
	fs.Uint64Var(&maxSeq, "max-seq", 0, "only records with seq at or below this value (inclusive); 0 means no bound")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Requested help is OUTPUT: stdout, exit 0.
			fmt.Fprint(stdout, logUsage)
			return exitLogOK
		}
		// flag has already printed the specific error to stderr; the usage text
		// follows it there.
		fmt.Fprint(stderr, logUsage)
		return exitLogUsage
	}
	if fs.NArg() > 0 {
		// The argument is NOT echoed: it is unvalidated argv on its way to a
		// terminal. The usage text says what is legal, which is the useful half.
		fmt.Fprintf(stderr, "agent-bus %s: unexpected argument\n", logCommandName)
		fmt.Fprint(stderr, logUsage)
		return exitLogUsage
	}
	if strings.TrimSpace(dataDir) == "" {
		return logFail(stdout, stderr, asJSON, exitLogUsage,
			"-data-dir must not be empty",
			"pass -data-dir <the bus's data directory>")
	}

	filter, ferr := newAuditFilter(sender, recipient, since, until, minSeq, maxSeq)
	if ferr != nil {
		return logFail(stdout, stderr, asJSON, ferr.code, ferr.Error(), ferr.remedy)
	}

	recs, path, lerr := readAuditTrail(dataDir, stderr)
	if lerr != nil {
		return logFail(stdout, stderr, asJSON, lerr.code, lerr.Error(), lerr.remedy)
	}
	return writeAuditTrail(stdout, stderr, asJSON, path, recs, filter)
}

// auditLogRecord is the --json record shape: one of these per line.
//
// IT IS BUILT FIELD BY FIELD FROM A DECODED wal.AuditRecord AND HAS NO BODY
// FIELD. There is no `payload`, no `raw`, no catch-all, and wal.Record.Payload
// is never marshalled — see the file comment. Adding a field here that carried
// message content would violate invariant 6.
//
// recipients and bus_path are deliberately NOT omitempty and are normalised away
// from nil, so every line has both keys and neither is ever null.
type auditLogRecord struct {
	// AuditIndex is the record's position in bus.audit (wal.Record.Index). It is
	// a FRAME LOCATOR, not an identity: a quarantined audit log restarts at 1, so
	// audit indices are not unique across the lifetime of a data directory. Join
	// on message_id or seq.
	AuditIndex uint64 `json:"audit_index"`
	// Offset is the byte offset of the record's frame header in bus.audit, so a
	// damage report and a record can be lined up against the same file.
	Offset int64 `json:"offset"`

	MessageID     string   `json:"message_id"`
	Seq           uint64   `json:"seq"`
	Sender        string   `json:"sender"`
	Broadcast     bool     `json:"broadcast"`
	Recipients    []string `json:"recipients"`
	BusPath       []string `json:"bus_path"`
	SentAt        string   `json:"sent_at"`
	Size          int64    `json:"size"`
	ContentSHA256 string   `json:"content_sha256"`
	// PrepareIndex is the WAL index of the PREPARE record of the transaction the
	// message was written in — the transaction id, stamped by internal/wal. It is
	// what lets an operator pair this record with the WAL entry that (may have)
	// committed it.
	PrepareIndex uint64 `json:"prepare_index"`
}

// auditLogDamage is the --json damage shape, emitted inline for a record that
// would not decode and once at the end for a scan that stopped early.
//
// IT MUST NEVER CARRY A message_id FIELD. scripts/fed-smoke.sh counts objects
// matching a message id and requires exactly one per message, so a damage object
// with that key would be counted as a delivery.
type auditLogDamage struct {
	// Damaged is true by construction; it is the key a consumer branches on.
	Damaged bool   `json:"damaged"`
	Path    string `json:"path"`
	// AuditIndex is the frame's index when a specific record failed, and is
	// omitted for damage found by the framing scan itself, where there is no
	// record to name.
	AuditIndex uint64 `json:"audit_index,omitempty"`
	// Offset is the byte offset the damage was found at, or the offset just past
	// the last good record when the scan stopped.
	Offset int64  `json:"offset"`
	Reason string `json:"reason"`
	Remedy string `json:"remedy,omitempty"`
}

// auditLogError is the --json failure shape for the paths that never get as far
// as reading records. `ok` is the field a caller branches on. It carries no
// message_id, for auditLogDamage's reason.
type auditLogError struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Remedy   string `json:"remedy,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// logCommandError carries the exit code and remedy alongside the message so that
// every failure path is one line at the call site. It mirrors keyExportError.
type logCommandError struct {
	code   int
	msg    string
	remedy string
	cause  error
}

func (e *logCommandError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *logCommandError) Unwrap() error { return e.cause }

// logFail reports a failure in whichever mode was asked for and returns the exit
// code.
//
// In --json mode the object goes to STDOUT, not stderr: an agent that redirected
// stderr away still gets a parseable answer, which is the whole reason --json
// exists (invariant 7's second audience).
func logFail(stdout, stderr io.Writer, asJSON bool, code int, msg, remedy string) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(auditLogError{OK: false, Error: msg, Remedy: remedy, ExitCode: code}); err != nil {
			// The ORIGINAL code, not a substituted one. A failed write to stdout
			// says nothing about WHY the command failed, and returning 1 here
			// would report "the trail is damaged" for what was actually a missing
			// trail (4) or an unauthenticatable one (5) — turning a broken pipe
			// into a false statement about the operator's data. The message is
			// repeated on stderr so the answer survives the failed encode.
			fmt.Fprintf(stderr, "agent-bus %s: %s\n", logCommandName, msg)
			return code
		}
		return code
	}
	fmt.Fprintf(stderr, "agent-bus %s: %s\n", logCommandName, msg)
	if remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", remedy)
	}
	return code
}

// scannedAudit is what one strict pass over bus.audit yielded: the frames it
// could read, and — if the file stopped being trustworthy — where and why.
type scannedAudit struct {
	records []wal.Record
	// end is the byte offset just past the last good record.
	end int64
	// scanErr is non-nil when the file is damaged. The records above are still
	// everything that WAS readable and are still printed; this is reported
	// afterwards and forces exit 1.
	scanErr error
}

// readAuditTrail opens dataDir, takes the exclusive lock and scans bus.audit.
//
// # IT CREATES NOTHING — AND THAT IS ENFORCED BY A CHECK, NOT BY INTENTION
//
// An earlier version of this comment simply CLAIMED "it never creates anything:
// not the directory, not the trail, not the MAC key". The claim was FALSE, and
// the security gate proved it with a probe: a directory holding only a
// zero-length bus.audit came back holding wal-mac.key as well. wal.ScanAll
// resolves a codec, which resolves a MAC key, and wal's macKeyMayBeCreated
// (internal/wal/mackey.go) permits CREATING one for exactly the shapes a reader
// is most likely to be pointed at — a zero-length file, an unknown magic, or a
// version 2 file that is only its own header. The creation was also SILENT here,
// because ScanAll takes no logger and wal's "generated a new MAC key" line is
// suppressed when the logger is nil.
//
// The harm is not hypothetical. On a directory whose bus.wal is INTACT but whose
// key has been lost, one run of this READ-ONLY command minted a key and thereby
// converted wal.ErrMACKeyMissing — whose remedy is "restore the key" — into
// wal.ErrMACKeyMismatch, whose documented remedy is to move bus.wal aside. A
// read-only evidence tool turned "restore a 64-byte file" into "destroy the
// write-ahead log". That is the worst thing this command could possibly do.
//
// So the key must ALREADY EXIST and this refuses if it does not
// (checkMACKeyPresent, exit 5). That single check closes the whole class rather
// than one shape of it: macKeyFor only ever reaches createMACKey when loadMACKey
// returns ErrMACKeyMissing, so with the key file present no creation path can
// fire, for ANY shape of bus.audit. And refusing is the only honest answer
// anyway — a reader with no MAC key cannot authenticate a single record it
// reads, so minting one would both fabricate the authority to verify and poison
// the directory for the real bus.
//
// The ORDER mirrors exportBusSigningPublicKey and mintInvite — stat, check,
// lock, re-check, read — because the reasons those give for it apply unchanged:
// the pre-lock check keeps a refusal from writing so much as a bus.lock into a
// directory the operator mistyped (a lone bus.lock in a virgin directory makes
// the operator's very first `agent-bus` start refuse to boot), and the post-lock
// check is the one that is load-bearing, because between the two a concurrent
// process could have removed the file.
func readAuditTrail(dataDir string, stderr io.Writer) (scannedAudit, string, *logCommandError) {
	path := filepath.Join(dataDir, wal.AuditFileName)

	info, err := os.Stat(dataDir)
	if err != nil {
		return scannedAudit{}, path, &logCommandError{
			code:   exitLogNoTrail,
			msg:    fmt.Sprintf("cannot read the data directory %q, so there is no audit trail to read", dataDir),
			remedy: "check -data-dir; this command never creates one",
			cause:  err,
		}
	}
	if !info.IsDir() {
		return scannedAudit{}, path, &logCommandError{
			code:   exitLogNoTrail,
			msg:    fmt.Sprintf("-data-dir %q is not a directory", dataDir),
			remedy: "point -data-dir at the bus's data directory",
		}
	}
	// BEFORE THE LOCK, so that this refusal writes NOTHING AT ALL — not even the
	// bus.lock that dirlock.Acquire creates.
	if e := checkAuditTrailPresent(dataDir); e != nil {
		return scannedAudit{}, path, e
	}

	// The exclusive lock, for `peer list`'s reason: this command writes nothing,
	// and the lock is what stops a concurrent append being read as a torn tail.
	lock, err := dirlock.Acquire(dataDir)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return scannedAudit{}, path, &logCommandError{
				code: exitLogBusRunning,
				msg:  "the data directory is locked by a live process, which is almost certainly the bus itself",
				remedy: "stop the bus, run this command, then start it again; the audit trail is append-only, " +
					"so everything you see after the restart is a superset of what you would have seen now",
				cause: err,
			}
		}
		return scannedAudit{}, path, &logCommandError{
			code:   exitLogDamaged,
			msg:    "locking the data directory",
			remedy: "check the data directory's permissions and that no stale lock file is unwritable",
			cause:  err,
		}
	}
	defer func() {
		if err := lock.Release(); err != nil {
			fmt.Fprintf(stderr, "agent-bus %s: releasing the data directory lock failed: %v\n", logCommandName, err)
		}
	}()

	// RE-CHECK under the lock: the pre-lock check ran while another process could
	// still have been removing the file.
	if e := checkAuditTrailPresent(dataDir); e != nil {
		return scannedAudit{}, path, e
	}

	// THE TWO AUTHENTICATION CHECKS, DELIBERATELY AFTER THE LOCK. Unlike the
	// presence check above they READ FILE CONTENTS, so they must be serialised
	// against a bus that could be writing; and a bus.lock left behind by a
	// refusal is only a hazard for the mistyped-directory case, which the
	// pre-lock presence check has already caught. The lock is released by the
	// defer above on every one of these paths.
	if e := checkMACKeyPresent(dataDir); e != nil {
		return scannedAudit{}, path, e
	}
	if e := checkAuditFormatVersion(path); e != nil {
		return scannedAudit{}, path, e
	}

	// wal.ScanAll is STRICT and READ-ONLY: it repairs nothing, truncates nothing
	// and writes nothing. It returns every record it could read IN FILE ORDER
	// even when it also returns an error, which is exactly the contract this
	// command needs — print what survived, then say what did not.
	recs, end, scanErr := wal.ScanAll(path, wal.KindAudit)
	return scannedAudit{records: recs, end: end, scanErr: scanErr}, path, nil
}

// checkAuditTrailPresent refuses a data directory with no bus.audit in it.
//
// This is exit 4 and not exit 1 because the two say different things to an
// operator: "the trail is broken" is a recovery problem, while "there is no
// trail" means any messages this bus routed have no provenance record at all.
//
// # ABSENT VS. UNREADABLE — THE ONE THING THIS FUNCTION MUST NEVER CONFUSE
//
// os.Stat fails for two entirely different reasons and they must never share a
// message. os.ErrNotExist means the file genuinely is not there — the case this
// function exists to report, exit 4, "no provenance record". Any OTHER stat
// error (EACCES, EIO, a bad mount, a permission bit on a parent directory) means
// the OPPOSITE: this command simply could not LOOK — the trail may be sitting
// there completely intact. Reporting the second case with the first case's
// message would tell an operator their provenance is gone when it may not be,
// which is exactly the failure invariant 6 exists to prevent: a reader that
// misreports the state of the trail is the defect, not a slower failure mode of
// it. So the two are split with errors.Is, and the unreadable case is reported
// under exitLogDamaged (1) rather than exitLogNoTrail (4) or exitLogUnverifiable
// (5): it is neither "the trail is broken" (1's usual meaning, but still the
// closest honest fit — "do not rely on what this command told you") nor "there
// is no trail" (4, which this case must not claim) nor "the bytes are present
// but cannot be authenticated" (5, which presupposes we could even see the
// bytes). It gets its own message and a remedy that points at permissions and
// ownership rather than at backup restoration.
func checkAuditTrailPresent(dataDir string) *logCommandError {
	path := filepath.Join(dataDir, wal.AuditFileName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &logCommandError{
				code: exitLogNoTrail,
				msg: fmt.Sprintf("this data directory holds no audit trail (%s): there is NO provenance record for any message this bus may have routed",
					path),
				remedy: "if this bus has never accepted a message, that is expected — start it, send one, stop it, and retry; " +
					"if it HAS, the trail has been lost and must be restored from backup. This command will not create one, " +
					"because an empty trail written now would look exactly like a bus that never carried anything",
				cause: err,
			}
		}
		// NOT absence: something (permissions, I/O, a bad mount) stopped this
		// command from even STATTING the file. The trail may exist and be
		// perfectly intact — this is not evidence either way, so exitLogDamaged
		// (1) is used rather than exitLogNoTrail (4): "damaged" here means "this
		// reader could not read it", not "it is broken", but it is the honest
		// code because it says the same thing exit 1 always says — do not rely
		// on what this command told you — without asserting the trail is gone.
		return &logCommandError{
			code: exitLogDamaged,
			msg: fmt.Sprintf("the audit trail %s could NOT BE EXAMINED, so this is NOT evidence that it is missing or damaged",
				path),
			remedy: "check the permissions and ownership of the data directory and of " + wal.AuditFileName +
				", and that the filesystem is healthy; run this command as the user that owns the bus's data directory",
			cause: err,
		}
	}
	return nil
}

// checkMACKeyPresent refuses a data directory that holds no wal-mac.key.
//
// TWO REASONS, EITHER OF WHICH WOULD BE ENOUGH.
//
// Honesty: integrity in this log is a keyed MAC and never a CRC (invariant 6).
// Without the key not one record can be authenticated, so every line this
// command could print would be an assertion it has no standing to make. "I
// cannot vouch for these bytes" is the only true answer available.
//
// Safety: it stops wal from MINTING a key as a side effect of the read — see
// readAuditTrail for the probe that proved that happening, and for how a minted
// key escalates a lost key into a destroyed WAL.
//
// wal.MACKeyFileName is used rather than the literal string so this cannot drift
// from the name wal actually opens.
func checkMACKeyPresent(dataDir string) *logCommandError {
	keyPath := filepath.Join(dataDir, wal.MACKeyFileName)
	if _, err := os.Stat(keyPath); err != nil {
		return &logCommandError{
			code: exitLogUnverifiable,
			msg: fmt.Sprintf("this data directory holds no MAC key (%s), so NOTHING in the audit trail can be authenticated and none of it was read",
				keyPath),
			remedy: "restore " + wal.MACKeyFileName + " from backup and retry. Do NOT start the bus against this directory first, and do not " +
				"let anything create a key here: a key minted now verifies nothing that was written under the real one, and it turns " +
				"a recoverable \"the key is missing\" into \"the key does not match\", whose remedy is to move " + wal.AuditFileName + " and " +
				"bus.wal aside. This command deliberately will not create one",
			cause: err,
		}
	}
	return nil
}

// auditMagic is the eight-byte file magic of a message audit trail.
//
// The SOURCE OF TRUTH is internal/wal/format.go's magicAudit, which is
// unexported. Duplicating eight ASCII bytes here is deliberate and minimal: a
// read-only tool must be able to REFUSE a format it must not interpret before it
// hands the file to a decoder, and widening wal's API to export a constant used
// only for a refusal is the larger commitment. If wal ever changes it, this
// check fails closed — it refuses a real trail rather than accepting a fake one.
const auditMagic = "AGNTBUSA"

// auditHeaderPrefix is how many bytes identify the format: magic[8] ++
// version[4], big-endian. Every layout wal has written begins with those twelve
// bytes, which is what lets a reader that implements NEITHER layout still tell
// them apart.
const auditHeaderPrefix = 8 + 4

// checkAuditFormatVersion refuses a bus.audit that is not format version 2.
//
// # WHY A READER HAS TO CHECK THIS ITSELF
//
// Version 1 frames are authenticated by an UNKEYED CRC32C — anyone can compute
// one. wal will still READ a version 1 file (detectFormat returns 1 and codecFor
// hands back a CRC32C codec), so wal.ScanAll "verifies" records an attacker
// authored, and this command would print them under a header promising routing
// and provenance, with exit 0 and an empty stderr. The security gate did exactly
// that and got `message bus-a.msg-FORGED-BY-ATTACKER` out of it.
//
// internal/wal/audit.go records that AUDIT RECORDS HAVE ONLY EVER BEEN WRITTEN
// AT FORMAT VERSION 2, so a version 1 bus.audit is never a real bus's file — it
// is a planted one — and the server QUARANTINES it rather than reading it. This
// command runs against a STOPPED bus, by design, in the window before any
// quarantine can fire; on a backup or a forensic copy it may be the only thing
// that ever reads the file. So it makes the same judgement the server makes,
// itself.
//
// A ZERO-LENGTH FILE IS DELIBERATELY NOT REFUSED HERE. It carries no header to
// judge and no record to misbelieve, and it is already a documented, tested
// answer: wal.ScanAll reports "file is empty: it has no file header", which is
// damage (exit 1) and is loud. Saying "unverifiable" about a file with nothing
// in it would replace a precise answer with a vaguer one. A file that is
// non-empty but too short to hold the twelve bytes IS refused: it claims to be
// something and cannot be checked.
//
// # COULD NOT BE EXAMINED (exit 1) VS. CANNOT BE AUTHENTICATED (exit 5) — THE
// # SPLIT THIS FUNCTION MUST NOT COLLAPSE
//
// "The audit trail exists but this command cannot read it" is ONE operator
// condition, and it must report under ONE exit code regardless of WHICH path
// was unreadable. checkAuditTrailPresent already makes this split for a
// data-directory-level os.Stat failure (EACCES on the directory: exit 1, "could
// NOT BE EXAMINED", not exit 5). Before this fix, the identical condition one
// level lower — bus.audit itself unreadable while its parent directory stats
// fine (chmod 000 on the file) — fell through checkAuditTrailPresent's os.Stat
// (which only needs the parent's search bit) and checkMACKeyPresent (a
// different file), then hit THIS function's os.Open and came out as exit 5. Exit
// 5 is wrong there by this function's own reasoning below: it "presupposes we
// could even see the bytes" (checkAuditTrailPresent's EACCES comment), which a
// permission-denied os.Open is precisely the case of NOT having done. So the
// same split is repeated here, one layer down:
//
//   - os.Open failing for anything OTHER than the file having vanished
//     (EACCES, EIO, a bad mount) is NOT a judgement about content — this reader
//     never got to look — so it is exitLogDamaged (1), with the same message
//     shape checkAuditTrailPresent's EACCES branch uses: say the trail could
//     NOT BE EXAMINED, that this is NOT evidence it is missing, damaged or
//     unauthentic, and point the remedy at permissions and ownership.
//   - A read failure AFTER a successful open that is not about the file's
//     CONTENT (an I/O error mid-read, as opposed to hitting EOF partway
//     through the twelve-byte prefix) is the same case and gets the same
//     exit 1 treatment, for the same reason.
//   - Everything that IS a judgement about content — wrong magic, a format
//     version other than wal.FormatVersion, or a non-empty file too short to
//     hold the twelve-byte prefix (io.ErrUnexpectedEOF, i.e. we DID read some
//     bytes and they do not add up to a header) — stays exit 5
//     (exitLogUnverifiable): we saw the bytes and they are not something this
//     reader may interpret.
//   - A file that existed for checkAuditTrailPresent's re-check but is gone by
//     the time THIS function opens it (os.ErrNotExist, i.e. removed between
//     the two) gets exit 4 (exitLogNoTrail): "no provenance record" is the
//     honest answer for a file that is not there, not "cannot authenticate".
func checkAuditFormatVersion(path string) *logCommandError {
	unreadable := func(msg, remedy string, cause error) *logCommandError {
		return &logCommandError{code: exitLogDamaged, msg: msg, remedy: remedy, cause: cause}
	}
	unverifiable := func(msg, remedy string, cause error) *logCommandError {
		return &logCommandError{code: exitLogUnverifiable, msg: msg, remedy: remedy, cause: cause}
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &logCommandError{
				code: exitLogNoTrail,
				msg: fmt.Sprintf("this data directory holds no audit trail (%s): there is NO provenance record for any message this bus may have routed",
					path),
				remedy: "if this bus has never accepted a message, that is expected — start it, send one, stop it, and retry; " +
					"if it HAS, the trail has been lost and must be restored from backup. This command will not create one, " +
					"because an empty trail written now would look exactly like a bus that never carried anything",
				cause: err,
			}
		}
		// NOT a content judgement: this command never got to LOOK at the bytes, so
		// it cannot say they are missing, damaged, or unauthentic — only that it
		// could not examine them. See the split explained above.
		return unreadable(
			fmt.Sprintf("the audit trail %s could NOT BE EXAMINED, so this is NOT evidence that it is missing, damaged, or unauthentic",
				path),
			"check the permissions and ownership of "+wal.AuditFileName+", and that the filesystem is healthy; "+
				"run this command as the user that owns the bus's data directory",
			err)
	}
	defer f.Close()

	var head [auditHeaderPrefix]byte
	n, rerr := io.ReadFull(f, head[:])
	if rerr != nil {
		if n == 0 && errors.Is(rerr, io.EOF) {
			// Zero-length: let the scan report it as the damage it is.
			return nil
		}
		if !errors.Is(rerr, io.ErrUnexpectedEOF) {
			// A read failure that is NOT "we ran out of bytes partway through the
			// header" — an I/O error, a permission change mid-read, and so on. This
			// is the same "could not examine it" case as the os.Open failure above,
			// not a judgement that the content is bad.
			return unreadable(
				fmt.Sprintf("the audit trail %s could NOT BE EXAMINED (reading its format header failed), so this is NOT evidence that it is missing, damaged, or unauthentic",
					path),
				"check the permissions and ownership of "+wal.AuditFileName+", and that the filesystem is healthy; "+
					"run this command as the user that owns the bus's data directory",
				rerr)
		}
		return unverifiable(
			fmt.Sprintf("the audit trail %s is %d bytes, too short to hold even the %d-byte format header, so nothing in it can be identified or authenticated",
				path, n, auditHeaderPrefix),
			"keep the file and do not let the bus append to it; a trail this short holds no record, and whatever truncated it is the thing to investigate",
			rerr)
	}

	if got := string(head[:8]); got != auditMagic {
		return unverifiable(
			fmt.Sprintf("the audit trail %s does not begin with this bus's audit magic (%q, want %q), so it is not a trail this bus wrote and none of it was read",
				path, got, auditMagic),
			"keep the file, do NOT let the bus append to it, and inspect it out of band. Its contents must not be treated as evidence: "+
				"nothing here was written or authenticated by this bus",
			nil)
	}
	if version := binary.BigEndian.Uint32(head[8:12]); version != wal.FormatVersion {
		return unverifiable(
			fmt.Sprintf("the audit trail %s declares format version %d, but only version %d is authenticated by a keyed MAC; version 1 frames are "+
				"authenticated by an UNKEYED checksum that ANYONE CAN COMPUTE, and audit records have only ever been written at version %d, so a file "+
				"like this was never written by this bus. None of it was read",
				path, version, wal.FormatVersion, wal.FormatVersion),
			"keep the file, do NOT let the bus append to it, and inspect it out of band. Its contents MUST NOT be treated as evidence: any record in "+
				"it could have been authored by anyone who could write this directory",
			nil)
	}
	return nil
}

// auditFilter is the record-selection predicate built from the flags. A zero
// auditFilter matches everything.
//
// IT IS APPLIED ONLY TO RECORDS THAT DECODED. Damage is reported before any
// filter is consulted (see writeAuditTrail), because a filter that could hide
// corruption would make this command's silence worthless.
type auditFilter struct {
	sender    string
	recipient string
	// since is INCLUSIVE and until is EXCLUSIVE, so consecutive windows tile the
	// timeline with no gap and no double-count. Both are zero when unset.
	since  time.Time
	until  time.Time
	minSeq uint64
	maxSeq uint64
}

// newAuditFilter validates the filter flags before anything is opened, so a
// mistyped timestamp costs no I/O and takes no lock.
func newAuditFilter(sender, recipient, since, until string, minSeq, maxSeq uint64) (auditFilter, *logCommandError) {
	f := auditFilter{sender: sender, recipient: recipient, minSeq: minSeq, maxSeq: maxSeq}
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return auditFilter{}, &logCommandError{
				code:   exitLogUsage,
				msg:    "-since is not an RFC3339 instant",
				remedy: `use e.g. -since 2026-08-14T09:00:00Z (the trail's sent_at is the SERVER's clock, in UTC)`,
				cause:  err,
			}
		}
		f.since = t
	}
	if until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return auditFilter{}, &logCommandError{
				code:   exitLogUsage,
				msg:    "-until is not an RFC3339 instant",
				remedy: `use e.g. -until 2026-08-14T10:00:00Z (the trail's sent_at is the SERVER's clock, in UTC)`,
				cause:  err,
			}
		}
		f.until = t
	}
	if minSeq != 0 && maxSeq != 0 && minSeq > maxSeq {
		return auditFilter{}, &logCommandError{
			code:   exitLogUsage,
			msg:    fmt.Sprintf("-min-seq %d is above -max-seq %d, so no record can match", minSeq, maxSeq),
			remedy: "the range is inclusive at both ends; give -min-seq <= -max-seq, or omit one of them",
		}
	}
	if !f.since.IsZero() && !f.until.IsZero() && !f.until.After(f.since) {
		return auditFilter{}, &logCommandError{
			code:   exitLogUsage,
			msg:    "-until is not after -since, so no record can match",
			remedy: "-since is inclusive and -until is exclusive; give -until strictly after -since",
		}
	}
	return f, nil
}

// matches reports whether a decoded record passes every filter that was set.
func (f auditFilter) matches(a wal.AuditRecord) bool {
	if f.sender != "" && a.Sender != f.sender {
		return false
	}
	if f.recipient != "" {
		// A BROADCAST NEVER MATCHES -recipient. It carries no recipient list by
		// construction (wal.AuditRecord.Broadcast), because expanding one would
		// have frozen the roster as it stood at send time into a record that
		// describes routing, not membership. So this bus cannot say from the trail
		// who a broadcast reached, and guessing "everyone" here would be an
		// assertion the audit trail does not support.
		found := false
		for _, r := range a.Recipients {
			if r == f.recipient {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !f.since.IsZero() && a.SentAt.Before(f.since) {
		return false
	}
	if !f.until.IsZero() && !a.SentAt.Before(f.until) {
		return false
	}
	if f.minSeq != 0 && a.Seq < f.minSeq {
		return false
	}
	if f.maxSeq != 0 && a.Seq > f.maxSeq {
		return false
	}
	return true
}

// writeAuditTrail renders the scan and returns the exit code.
//
// The ORDER OF OPERATIONS IS THE INVARIANT-6 PART. For each frame: decode; if it
// will not decode, REPORT IT LOUDLY and carry on to the next frame; only a frame
// that decoded is offered to the filter. Damage is therefore never suppressed by
// a filter, and any damage at all — including damage the scan reported after the
// last readable record — makes this exit 1 no matter how much printed cleanly.
func writeAuditTrail(stdout, stderr io.Writer, asJSON bool, path string, scan scannedAudit, filter auditFilter) int {
	var enc *json.Encoder
	if asJSON {
		enc = json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
	} else {
		writeAuditHeader(stdout, path)
	}

	damaged := false
	shown := 0

	for _, rec := range scan.records {
		if rec.Type != wal.TypeAuditMessage {
			// A frame that verified but is not a message record. It is NOT skipped
			// silently: this file is the message audit trail and nothing else
			// belongs in it, so an unexpected type means the file is not what it
			// claims to be.
			damaged = true
			reportAuditDamage(stderr, enc, path, rec.Index, rec.Offset,
				fmt.Sprintf("record %d is a %s record, but %s holds only %s records",
					rec.Index, rec.Type, wal.AuditFileName, wal.TypeAuditMessage),
				"this record is NOT a message and carries no provenance; keep the file and inspect it before the bus appends to it again")
			continue
		}
		a, prepareIndex, err := wal.DecodeAudit(path, rec)
		if err != nil {
			damaged = true
			reportAuditDamage(stderr, enc, path, rec.Index, rec.Offset,
				err.Error(),
				"one message has LOST its provenance record; the surrounding records are still shown and are still trustworthy")
			continue
		}
		if !filter.matches(a) {
			continue
		}
		shown++
		if asJSON {
			if encErr := enc.Encode(newAuditLogRecord(rec, a, prepareIndex)); encErr != nil {
				fmt.Fprintf(stderr, "agent-bus %s: writing the JSON record for audit index %d failed: %v\n",
					logCommandName, rec.Index, encErr)
				return exitLogDamaged
			}
			continue
		}
		writeAuditHuman(stdout, rec, a, prepareIndex)
	}

	// The scan's own error is reported LAST, because it describes the point at
	// which the file stopped being readable and everything above it is what did
	// read. A zero-length bus.audit arrives here as "file is empty: it has no
	// N-byte file header", which is correct and deliberately loud.
	if scan.scanErr != nil {
		damaged = true
		var ce *wal.CorruptError
		reason := scan.scanErr.Error()
		offset := scan.end
		// THE REMEDY BRANCHES ON WHETHER THIS IS ACTUALLY CORRUPTION. Only a
		// CorruptError names a byte at which the file stops being trustworthy;
		// every other failure the scan can return — a permission denied, an I/O
		// error, a key that will not load — is a MISCONFIGURATION, and telling an
		// operator "the trail is readable up to byte N, do not truncate it" for a
		// chmod points them at their data instead of at the actual fault. The
		// reason is already correct in both cases and is left alone.
		remedy := "the trail could not be scanned to the end, and this is NOT a report of corruption at a byte offset: check the file's " +
			"permissions and ownership and that " + wal.MACKeyFileName + " is present and readable. Change nothing about " + wal.AuditFileName +
			" until you know which it is"
		if errors.As(scan.scanErr, &ce) {
			reason = ce.Reason
			offset = ce.Offset
			remedy = fmt.Sprintf("the trail is readable up to byte %d and stops being trustworthy there; "+
				"everything printed above that point is intact. Do not truncate the file by hand — keep it for inspection", scan.end)
		}
		reportAuditDamage(stderr, enc, path, 0, offset, reason, remedy)
	}

	if !asJSON {
		writeAuditFooter(stdout, shown, damaged)
	}
	if damaged {
		return exitLogDamaged
	}
	return exitLogOK
}

// newAuditLogRecord composes the output object FIELD BY FIELD from the decoded
// record. There is no path here through which wal.Record.Payload — the raw frame
// — can reach the output; see the file comment.
func newAuditLogRecord(rec wal.Record, a wal.AuditRecord, prepareIndex uint64) auditLogRecord {
	return auditLogRecord{
		AuditIndex: rec.Index,
		Offset:     rec.Offset,
		MessageID:  a.MessageID,
		Seq:        a.Seq,
		Sender:     a.Sender,
		Broadcast:  a.Broadcast,
		// NEVER null and NEVER omitted: an absent recipient list or bus path must
		// be visibly [], because a missing key reads as "not recorded" when the
		// truth is "recorded, and empty". Order is wal's, unsorted.
		Recipients:    nonNilStrings(a.Recipients),
		BusPath:       nonNilStrings(a.BusPath),
		SentAt:        a.SentAt.UTC().Format(time.RFC3339Nano),
		Size:          a.Size,
		ContentSHA256: a.ContentSHA256,
		PrepareIndex:  prepareIndex,
	}
}

// nonNilStrings returns s, or an empty non-nil slice when s is nil, so
// encoding/json emits [] rather than null. It never reorders or copies contents.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// reportAuditDamage names one piece of damage on stderr, and — under --json —
// also as an NDJSON object with no message_id key.
//
// STDERR IS ALWAYS WRITTEN, including in --json mode: a human watching the
// terminal and a script parsing stdout must both learn about a discard, and
// invariant 6's requirement is that the report is loud, not that it is
// machine-readable in one place.
func reportAuditDamage(stderr io.Writer, enc *json.Encoder, path string, auditIndex uint64, offset int64, reason, remedy string) {
	if auditIndex != 0 {
		fmt.Fprintf(stderr, "agent-bus %s: DAMAGED: %s: audit index %d at byte offset %d: %s\n",
			logCommandName, path, auditIndex, offset, reason)
	} else {
		fmt.Fprintf(stderr, "agent-bus %s: DAMAGED: %s: at byte offset %d: %s\n",
			logCommandName, path, offset, reason)
	}
	if remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", remedy)
	}
	if enc == nil {
		return
	}
	if err := enc.Encode(auditLogDamage{
		Damaged:    true,
		Path:       path,
		AuditIndex: auditIndex,
		Offset:     offset,
		Reason:     reason,
		Remedy:     remedy,
	}); err != nil {
		fmt.Fprintf(stderr, "agent-bus %s: writing the JSON damage report failed: %v\n", logCommandName, err)
	}
}

// writeAuditHeader states, before any record is printed, that bodies are not in
// this trail. It is the first thing an operator reading the output sees, so that
// nobody concludes from a long clean listing that the content is here somewhere.
func writeAuditHeader(w io.Writer, path string) {
	fmt.Fprintf(w, "append-only message audit trail: %s\n", path)
	fmt.Fprintf(w, "\nThis trail is METADATA ONLY — routing and provenance. MESSAGE BODIES ARE NOT\n"+
		"RECORDED here and cannot be recovered from this file by this command or any\n"+
		"other (invariant 6). The content hash below is what identifies what was sent.\n\n")
}

// writeAuditHuman prints one record over two lines: the routing facts first,
// then the provenance and the frame locators.
//
// EVERY CLIENT-DERIVED STRING IS PRINTED WITH %q, NEVER %s. wal's auditID bounds
// these fields only on emptiness and length (internal/wal/audit.go) — it imposes
// NO character restriction — so a sender, a recipient, a bus id or a message id
// can legitimately reach this function containing a newline, a carriage return
// or an ANSI escape. The security gate put a newline in `sender` and got a
// COMPLETE FABRICATED RECORD LINE into the human output, naming a message id, a
// seq and a sender that appear nowhere in the file; it also got ESC[2J and
// ESC[1;31m through to a terminal unaltered. An audit reader whose output can be
// authored by the subject of the audit is worse than no reader.
//
// %q is strconv.Quote's escaping: a newline becomes \n, an ESC becomes \x1b, and
// the value gains delimiters so where it begins and ends is unambiguous. This is
// also wal's OWN house style — it already %q-quotes and elides these same values
// on its damage path, so before this change the success path was less careful
// than the package it wraps.
//
// The --json path needs nothing equivalent: encoding/json escapes both cases,
// and one object per line means a fabricated line cannot be smuggled in either.
func writeAuditHuman(w io.Writer, rec wal.Record, a wal.AuditRecord, prepareIndex uint64) {
	to := "broadcast (no recipient list is recorded)"
	if !a.Broadcast {
		to = quoteJoin(a.Recipients, ", ")
		if to == "" {
			to = "(none)"
		}
	}
	fmt.Fprintf(w, "seq %d  %s  from %q  to %s\n",
		a.Seq, a.SentAt.UTC().Format(time.RFC3339Nano), a.Sender, to)
	fmt.Fprintf(w, "    bus path: %s\n", renderBusPath(a.BusPath))
	fmt.Fprintf(w, "    message %q  size %d bytes  sha256 %s\n", a.MessageID, a.Size, a.ContentSHA256)
	fmt.Fprintf(w, "    audit index %d at byte %d, wal prepare %d\n", rec.Index, rec.Offset, prepareIndex)
}

// quoteJoin renders each element with %q and joins them with sep.
//
// PER ELEMENT, NOT ON THE JOINED STRING: quoting the join would let one element
// containing the separator masquerade as two, which is the same forgery in a
// smaller frame. Order is preserved exactly; nothing here sorts or dedupes.
func quoteJoin(elems []string, sep string) string {
	var b strings.Builder
	for i, e := range elems {
		if i > 0 {
			b.WriteString(sep)
		}
		fmt.Fprintf(&b, "%q", e)
	}
	return b.String()
}

// renderBusPath renders the ORDERED traversal, first bus first. An empty path is
// rendered explicitly rather than as a blank, because a blank would be read as a
// formatting slip instead of as the fact it is.
//
// Each element is quoted individually — see quoteJoin — because a bus id is
// client-derived and " -> " is four ordinary characters a bus id may contain.
func renderBusPath(path []string) string {
	if len(path) == 0 {
		return "(none)"
	}
	return quoteJoin(path, " -> ")
}

func writeAuditFooter(w io.Writer, shown int, damaged bool) {
	fmt.Fprintf(w, "\n%d record(s) shown.\n", shown)
	if damaged {
		fmt.Fprintf(w, "THE TRAIL IS DAMAGED: every discard is named on stderr above. "+
			"The count here covers only the records that could be read.\n")
	}
}
