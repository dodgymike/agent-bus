package main

// `agent-bus outbox` (RELAY-54): read the relay OUTBOX out of a STOPPED bus's
// write-ahead log and answer ONE operator question — "does this bus still owe
// anything to a peer, and has it given up on anything?"
//
// # WHY IT EXISTS: A DRAIN THAT CANNOT BE VERIFIED IS AN ASSUMPTION
//
// RELAY-51 rejected the drain-and-restart rollout order for a single reason:
// there was no instrument that could confirm the drain. An abandoned outbox
// job — a message this bus accepted responsibility for and will NEVER deliver —
// was written durably to bus.wal and surfaced by no subcommand at all.
// `agent-bus log` reads bus.audit, a different artefact, which carries message
// provenance and knows nothing about relay delivery state.
//
// So the operator sequence this command exists for is:
//
//	stop bus A  ->  run `agent-bus outbox -data-dir <dir>`
//	  exit 0 -> nothing pending: start the NEW binary
//	  exit 6 -> jobs are still owed: restart the OLD binary and wait, then retry
//
// A stopped bus does not defeat the drain gate — stopping it is the restart's
// first half anyway, and the exclusive lock this command takes is precisely
// what guarantees nothing is appending underneath the read.
//
// # THE EXIT CODE IS THE ANSWER, AND THAT IS DELIBERATE
//
// 6 (something pending) and 7 (something abandoned) are not decoration on a
// report; they ARE the report, so a rollout script can branch on them without
// parsing a word of output (invariant 7's second audience). 6 TAKES PRECEDENCE
// over 7 when both hold — "do not restart yet" is the more urgent question than
// "something was already lost", and the lost jobs are still listed in the
// output either way.
//
// BUT THE TRUST CODES OUTRANK BOTH: the full precedence is 1 > 8 > 6 > 7 > 0,
// because "can I believe this answer?" has to be settled before "what does it
// say?". 1 means bytes were thrown away and 8 means records are absent or were
// refused; either way the verdict below them was computed over an incomplete
// table, so neither may be reported as a drain. See the exit-code block.
//
// FILTERS NEVER CHANGE THE VERDICT. The exit code is computed over the WHOLE
// outbox BEFORE -peer or -state is consulted; a filter changes only what is
// PRINTED. This mirrors `agent-bus log`'s "filters never suppress damage" rule
// and exists for the same reason: a filter that could turn a 6 into a 0 would
// make this command's silence meaningless, and its silence is the entire
// product.
//
// # CAN I BELIEVE THE ANSWER COMES BEFORE WHAT THE ANSWER SAYS
//
// The exit codes fall into two families and the ordering between them is the
// load-bearing part:
//
//	1  >  8  >  6  >  7  >  0
//
// 1 (the log is damaged) and 8 (records are missing or were refused, so the
// drain is UNVERIFIED) are TRUST-OF-THE-ANSWER codes; 6, 7 and 0 are VERDICT
// codes. Trust outranks verdict, always, because "can I believe this?" is
// answered before "what does it say?". The practical consequence is the one
// this precedence exists for: EXIT 0 IS UNREACHABLE WHENEVER ANYTHING WAS
// DISCARDED, REFUSED OR MISSING. A read that dropped the one pending record in
// the file must never be able to report a drain — that is not a quiet outbox,
// it is a quiet instrument, and the two are indistinguishable from the exit
// code alone unless this ordering holds.
//
// The two families are kept APART rather than merged into one scale because
// they answer different questions and an operator acts on them differently: 6
// says "wait for the old binary to finish"; 8 says "this file cannot tell you
// whether there is anything to wait for". 8 IS DELIBERATELY NOT A CLAIM OF
// DAMAGE — see exitOutboxUnverifiedDrain, and see the reason text wal writes
// for a hole in the index sequence, which may simply be an index a crash burned
// and no record ever carried (invariant 1: an index this bus authorised is
// never authorised again, so a gap is not evidence of loss).
//
// # EVERY DISCARD IS NAMED, WHICH IS INVARIANT 6's ACTUAL REQUIREMENT
//
// wal.Replay reports record-level discards in its Recovered value and RETURNS
// NIL — it deliberately takes no logger, which is what keeps it usable as a
// pure fsck (wal.Open is the caller that normally logs them). A reader that
// throws that value away therefore SILENTLY DISCARDS, which invariant 6 rates
// as the defect rather than the discard itself. So this command keeps the
// Recovered value, prints every retained Discard, emits each one through the
// logger as well, and refuses to reach exit 0 with any of them outstanding.
//
// # ON THE SERVER BINARY, NOT ON agent-busctl
//
// For `invite mint`'s reason (DECISIONS.md E4), restated by `peer add`, `key
// export-public` and `log`: the authority this needs is FILESYSTEM ACCESS to a
// data directory, not a network privilege. There is no HTTP route that serves
// the outbox and this adds none. agent-busctl is the AGENT's pure HTTP client;
// giving it data-directory and dirlock plumbing to satisfy a spelling would be
// the larger change. It is still THE compiled client for this capability
// (invariant 7): an operator gets the answer from a subcommand with -json,
// never by hand-parsing bus.wal and never from a shell wrapper.
//
// # STRUCTURALLY READ-ONLY: Durable IS NIL
//
// The outbox is built with relay.OutboxOptions.Durable left nil, so every
// mutating call on it fails with relay.ErrOutboxNotDurable rather than merely
// not being made. The log is read with wal.Replay — the package's read-only
// fsck: it repairs nothing, truncates nothing, and creates no file. This is
// `peer list`'s recipe (openPeerStore, writable=false) unchanged.
//
// # THE LANDMINE: A READ THAT MINTS THE KEY AUTHENTICATING WHAT IT JUDGES
//
// wal's macKeyFor (internal/wal/mackey.go) MINTS wal-mac.key AS A SIDE EFFECT
// OF A READ. wal.Replay reaches it through resolveCodec -> codecFor -> macKeyFor
// whenever the log is non-empty and does not positively identify itself as
// format version 2 — macKeyMayBeCreated permits creation for garbage magic, a
// short header, or a version 1 log. A reader that skips the guard therefore
// SILENTLY MANUFACTURES THE KEY THAT AUTHENTICATES THE FILE IT IS ABOUT TO
// JUDGE, and (as the probe against `agent-bus log` showed) converts a
// recoverable wal.ErrMACKeyMissing into wal.ErrMACKeyMismatch, whose documented
// remedy is to move bus.wal aside. A read-only evidence tool would have turned
// "restore a 64-byte file" into "destroy the write-ahead log".
//
// So checkMACKeyPresent (auditlog.go — REUSED, not copied, because two copies
// of one guard drift) is consulted BEFORE the lock and again UNDER it, and its
// absence is exit 5 with nothing read, nothing printed, and NO KEY CREATED.
//
// NOTE, and it is not this file's to fix: `peer list`'s read-only path in
// peer.go calls wal.Replay with NO such guard. That is filed separately.
//
// # METADATA ONLY (invariant 6)
//
// An OutboxRecord holds routing and accounting — job id, peer bus, the origin
// message id, size, content hash, timestamps, state and an abandonment reason.
// There is no message body in it and none may be added: no `payload`, no `raw`,
// no catch-all field that could carry one. Every output field below is named
// explicitly and composed from a decoded record, exactly as `agent-bus log`
// composes its own.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// outboxCommandName is the single non-flag argument main() intercepts before
// server flag parsing. Pinned as a constant so the dispatch in main.go and the
// usage text cannot drift apart — the same treatment logCommandName gets.
const outboxCommandName = "outbox"

// Exit codes for `agent-bus outbox`.
//
// The kinship with the other offline subcommands is REAL BUT PARTIAL, and it is
// stated exactly rather than generously, because an operator who believes a
// number means the same thing everywhere and is wrong is worse off than one who
// looks it up: 2 (usage) and 3 (the bus is running) match `log`, `peer` and
// `key` alike; 4 matches `peer` and `key` ("no bus identity") but NOT `log`,
// whose 4 is "no audit trail"; 5 matches `log` ("unverifiable — no MAC key")
// but NOT `peer`, whose 5 is "nothing was withdrawn", and `key` has no 5 at
// all. 0 is NOT shared either — `log`'s 0 explicitly does not carry an answer,
// while 0 here IS the answer.
//
// 6, 7, 8 and 9 are this command's own. 6 and 7 are the verdict it exists to
// give; 8 and 9 exist so that a verdict which cannot be TRUSTED and a verdict
// which could not be DELIVERED are never spelled the same way as a clean drain.
const (
	// exitOutboxOK: the outbox was read cleanly and IT IS DRAINED — nothing is
	// pending and nothing is abandoned inside the retention window. This is the
	// code a rollout gate waits for.
	//
	// "CLEANLY" IS PART OF THE MEANING, NOT A PREAMBLE TO IT: this code is
	// unreachable if the replay discarded anything, if any record index was
	// absent, or if any outbox record was refused. Those become 1 or 8. A quiet
	// instrument must never be able to spell itself the same way as a quiet
	// outbox.
	exitOutboxOK = 0
	// exitOutboxDamaged: the write-ahead log is damaged or could not be read, so
	// THE QUESTION WAS NOT ANSWERED. It is not "nothing is pending"; it is "this
	// command does not know", and a rollout must treat it as a stop.
	//
	// It is returned when Replay itself failed, and ALSO when Replay succeeded
	// but threw BYTES away: any discard with a non-zero Length, any discard wal
	// marked Severe, anything discarded at the "framing" stage, or more discards
	// than wal retains detail for (maxDiscardsRetained). In every one of those
	// cases a record that WAS in this log is missing from the table below, and
	// nothing here can say whether it was a pending job.
	exitOutboxDamaged = 1
	// exitOutboxUsage: a bad flag, an unexpected argument, or an unparseable
	// filter value. Nothing was read.
	exitOutboxUsage = 2
	// exitOutboxBusRunning: the data directory is locked by a live process,
	// almost certainly the bus. Remedy: stop the bus and retry. A running bus
	// cannot be drained anyway, so this is a coherent refusal rather than an
	// inconvenience.
	exitOutboxBusRunning = 3
	// exitOutboxNoIdentity: the data directory holds no bus-id file, so it is
	// not a bus's data directory at all. Distinct from 1 because the remedy is
	// entirely different: check -data-dir, or restore the identity.
	exitOutboxNoIdentity = 4
	// exitOutboxUnverifiable: wal-mac.key is absent, so nothing in the log can
	// be authenticated. Integrity here is a keyed MAC and never a CRC (invariant
	// 6); with no key every line this command could print would be an assertion
	// it has no standing to make. NOTHING WAS READ, NOTHING WAS PRINTED, AND NO
	// KEY WAS CREATED — see the file comment for why that last clause is the
	// load-bearing one.
	exitOutboxUnverifiable = 5
	// exitOutboxPending: read cleanly, and AT LEAST ONE JOB IS PENDING. THE
	// DRAIN IS NOT COMPLETE: this bus still owes a peer a message it accepted.
	// Do not restart onto a new binary; restart the old one and wait.
	exitOutboxPending = 6
	// exitOutboxAbandoned: read cleanly, nothing pending, but at least one job
	// is ABANDONED inside the retention window — messages this bus accepted will
	// never reach their peer. The drain gate is satisfied; the delivery promise
	// was not. 6 takes precedence when both hold.
	exitOutboxAbandoned = 7
	// exitOutboxUnverifiedDrain: the log WAS read — no bytes were thrown away —
	// but records are ABSENT from it or were REFUSED as they were applied, so
	// THE DRAIN IS UNVERIFIED. Treat it as a stop, exactly like 1.
	//
	// IT IS NOT A CLAIM OF DAMAGE, and that distinction is the whole reason it
	// is a separate code rather than more 1s. Two things reach it:
	//
	//   - A HOLE IN THE INDEX SEQUENCE (a wal Discard with Length == 0). wal's
	//     own reason text for these says a hole may be a record lost from the
	//     media, a record an earlier recovery correctly discarded without
	//     renumbering the survivors, or an index range BURNED BY A RESERVATION A
	//     CRASH NEVER USED — the ordinary post-crash signature, since the index
	//     floor authorises indices in blocks and an authorised index is never
	//     authorised again (invariant 1). Recovered.MissingRecords is documented
	//     as an UPPER BOUND ON LOSS, NOT A COUNT OF IT. Reporting "damaged" on
	//     every bus that ever crashed would be a channel that cries wolf, and
	//     wal's own comments say so; reporting "drained" would be worse.
	//   - AN OUTBOX RECORD REFUSED DURING THIS REPLAY: undecodable, or rejected
	//     by the table (past the retry horizon, future-dated, self-addressed, or
	//     over the capacity cap). relay logs each refusal at ERROR and Apply
	//     returns nil, so without this the read completes and the job is simply
	//     not in the answer.
	exitOutboxUnverifiedDrain = 8
	// exitOutboxReportFailed: the outbox WAS read and the verdict WAS computed,
	// but the report could not be written to stdout — a closed pipe, a full
	// disk under a redirect.
	//
	// It exists because the two honest alternatives are both lies. Returning 1
	// would say "the write-ahead log is damaged" about a log that replayed
	// perfectly (the mistake outboxFail's own comment refuses to make), and
	// returning the computed verdict would let a drained bus exit 0 while the
	// report that was supposed to justify it never reached the operator. The
	// answer exists; it did not arrive.
	exitOutboxReportFailed = 9
)

// outboxWindowProse renders the retention window as prose, DERIVED from the
// constant rather than typed out beside it.
//
// The "24 HOURS" in the help text and in the limits block used to be hardcoded
// while retention_window_seconds was computed from
// relay.OutboxSettledRetention. Two spellings of one number, one of which
// cannot go stale and one of which can, is a doc that lies silently the day the
// constant moves — and the sentence it lies in is the one telling an operator
// how much of history this command can see.
//
// long picks the sentence form ("24 HOURS") over the compact one ("24h"). A
// window that is not a whole number of hours falls back to Duration.String(),
// which is uglier and exactly right: better an awkward "90m0s" than a confident
// wrong number.
func outboxWindowProse(d time.Duration, long bool) string {
	if d <= 0 || d%time.Hour != 0 {
		if long {
			return strings.ToUpper(d.String())
		}
		return d.String()
	}
	h := int64(d / time.Hour)
	if !long {
		return fmt.Sprintf("%dh", h)
	}
	if h == 1 {
		return "1 HOUR"
	}
	return fmt.Sprintf("%d HOURS", h)
}

var (
	// outboxWindowLong is the retention window as a sentence ("24 HOURS").
	outboxWindowLong = outboxWindowProse(relay.OutboxSettledRetention, true)
	// outboxWindowShort is the same window compactly ("24h").
	outboxWindowShort = outboxWindowProse(relay.OutboxSettledRetention, false)
)

// outboxUsage is printed for -h (stdout, exit 0) and beside a usage error
// (stderr).
//
// It carries BOTH honesty limits in full. They are not footnotes: an operator
// who reads "nothing abandoned" without them will conclude nothing was lost,
// which this command cannot support.
//
// It is a var and not a const ONLY so the retention window can be interpolated
// from relay.OutboxSettledRetention; nothing writes to it.
var outboxUsage = `agent-bus outbox — what this bus still owes its peers, and what it gave up on

USAGE
  agent-bus outbox [-data-dir <dir>] [-json] [-peer <bus-id>] [-state <s>]...

WHAT THIS IS FOR — THE ROLLOUT DRAIN GATE
  stop bus A, run this, then:
    exit 0 -> nothing pending: start the NEW binary
    exit 6 -> jobs are still owed: start the OLD binary again, wait, retry
  A stopped bus does not defeat the gate — stopping it is the restart's first
  half anyway, and the exclusive lock is what makes the read consistent.

THE BUS MUST NOT BE RUNNING. This takes the data directory's exclusive lock,
which a running bus holds. It never writes to the outbox, never repairs or
truncates the log, and never creates a bus identity or a MAC key: the outbox is
built with no durable log, so every mutating operation on it is refused, and the
log is read with the write-ahead log's read-only fsck. The ONE file it creates
is the lock itself, ` + dirlock.LockFileName + `, and only on the path where the
read actually happens — every refusal above is checked BEFORE the lock is taken,
so a refusal creates nothing at all. The lock file is left in place after it is
released, exactly as the bus leaves it.

WHAT IS IN A RECORD — AND WHAT IS NOT
  Routing and accounting only: job id, peer bus, the origin message id, size,
  content SHA-256, enqueue and settle times, state, and the reason a job was
  abandoned. MESSAGE BODIES ARE NOT RECORDED and cannot be recovered from this
  command or any other (invariant 6).

LIMITS OF THIS VIEW — READ BOTH BEFORE YOU TRUST A QUIET ANSWER
  1. IT CAN ONLY EVER ANSWER ABOUT THE LAST ` + outboxWindowLong + `. Retained records are swept
     once they pass the retention window (` + outboxWindowShort + `); anything older is gone from the
     table and nothing here can report it.
  2. "NOTHING ABANDONED" DOES NOT MEAN "NOTHING LOST". When a pending job passes
     the retry horizon it is dropped WITHOUT a durable tombstone — the sweep
     cannot write one, because it runs holding a lock this package never holds
     across a durable write. The only trace is a WARN line in the SERVER's log
     at the moment it was dropped. So a message can be lost and leave nothing
     for this command to find. Tracked as open task
     RELAY-15-FU-SWEEP-TOMBSTONE (da1ba9b7-ab59-476b-831e-4202b1b09ccc).

FLAGS
  -data-dir <dir>   the bus's data directory (default "` + defaultDataDir + `"). It must
                    already hold a bus identity; this command never creates one.
  -json             emit ONE JSON object with "ok" first (not NDJSON), carrying
                    counts, the per-peer breakdowns, the selected jobs, the two
                    limits above as a "limits" array, the "integrity" of the
                    read, the "filter" that was applied, and "exit_code".
  -peer <bus-id>    print only jobs owed to this peer bus (exact match).
  -state <s>        print only jobs in this state: pending, delivered or
                    abandoned. Repeatable; an unrecognised value is exit 2.

FILTERS NEVER CHANGE THE EXIT CODE. The verdict is computed over the WHOLE
outbox before -peer and -state are consulted; they change only what is printed.
A filter that could turn a 6 into a 0 would make this command's silence
meaningless.

EXIT CODES
  0  read cleanly; NOTHING pending and NOTHING abandoned — the outbox is drained
  1  the write-ahead log is damaged — it could not be read, or bytes were thrown
     away by the replay; the question was NOT answered, so do not read this as
     "drained"
  2  usage: bad flag, unexpected argument, unparseable filter value
  3  the data directory is locked by a live process — stop the bus and retry
  4  the data directory holds no bus identity, so it is not a bus's data dir
  5  UNVERIFIABLE: ` + wal.MACKeyFileName + ` is absent, so nothing in the log can be
     authenticated. Nothing was read, nothing was printed, NO KEY WAS CREATED
  6  read cleanly; at least one job is PENDING — THE DRAIN IS NOT COMPLETE
  7  read cleanly; nothing pending, but at least one job is ABANDONED inside the
     window — messages this bus accepted will never reach their peer
  8  the log was read and NO BYTES WERE LOST, but record indices are absent from
     it or an outbox record was refused, so THE DRAIN IS UNVERIFIED. This is NOT
     a claim of damage: an absent index is an UPPER BOUND on loss, and is often
     an index a crash burned that no record ever carried. Treat it as a stop
  9  the outbox was read and the verdict computed, but the REPORT could not be
     written (a closed pipe, a failing disk). Nothing is implied about the log

PRECEDENCE:  1  >  8  >  6  >  7  >  0
  1 and 8 say whether the answer can be BELIEVED; 6, 7 and 0 say what it IS, and
  trust is decided first. So exit 0 is UNREACHABLE whenever anything was
  discarded, refused or missing — a quiet instrument must not be able to spell
  itself the same way as a quiet outbox. 9 is neither: it means the answer never
  reached you.
`

// outboxLimits is the two-item honesty block, in one place so the human output,
// the -h text and the JSON `limits` array cannot come to say different things.
//
// They are stated as plain sentences rather than as codes because the reader is
// an operator deciding whether to restart a bus, and the failure this guards
// against is a confident conclusion drawn from a quiet report.
var outboxLimits = []string{
	"This view can only ever answer about the LAST " + outboxWindowLong + ": retained outbox records are swept once " +
		"they pass the retention window, and anything older is gone from the table.",
	"\"NOTHING ABANDONED\" DOES NOT MEAN \"NOTHING LOST\": a pending job dropped at the retry horizon leaves " +
		"NO durable tombstone (the sweep cannot write one), only a WARN line in the server's log at the moment " +
		"it was dropped. A message can therefore be lost and leave nothing for this command to find. " +
		"Tracked as open task RELAY-15-FU-SWEEP-TOMBSTONE (da1ba9b7-ab59-476b-831e-4202b1b09ccc).",
}

// ---------------------------------------------------------------------------
// The --json shapes
// ---------------------------------------------------------------------------

// outboxJobJSON is one job. IT IS COMPOSED FIELD BY FIELD FROM A DECODED
// relay.OutboxRecord AND HAS NO BODY FIELD — no payload, no raw, no catch-all
// (invariant 6). A future field is added by naming it here.
type outboxJobJSON struct {
	JobID           string `json:"job_id"`
	PeerBusID       string `json:"peer_bus_id"`
	OriginMessageID string `json:"origin_message_id"`
	State           string `json:"state"`
	EnqueuedAt      string `json:"enqueued_at"`
	// SettledAt and Reason are set iff the state names their event — the record
	// itself enforces that in both directions — so they are omitempty rather
	// than emitted as empty strings on every pending job.
	SettledAt string `json:"settled_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Size and ContentSHA256 are the two quantitative facts about content that
	// invariant 6 keeps in a routing record: enough to prove WHAT was owed,
	// never the bytes themselves.
	Size          int    `json:"size"`
	ContentSHA256 string `json:"content_sha256"`
}

// outboxPeerCount is one row of a per-peer breakdown: which peer, how many
// jobs, and how long the oldest has been waiting.
type outboxPeerCount struct {
	PeerBusID string `json:"peer_bus_id"`
	Jobs      int    `json:"jobs"`
	// OldestEnqueuedAt is the age signal. For a pending row it says how long
	// this peer has been unreachable; for an abandoned row it says when the
	// losses started.
	OldestEnqueuedAt string `json:"oldest_enqueued_at"`
}

// outboxCounts is the summary. It is over the WHOLE outbox, ALWAYS unfiltered —
// see outboxResult.
type outboxCounts struct {
	Retained  int `json:"retained"`
	Pending   int `json:"pending"`
	Delivered int `json:"delivered"`
	Abandoned int `json:"abandoned"`
}

// outboxDiscardJSON is one thing the replay THREW AWAY or found ABSENT, in
// wal.Discard's own vocabulary so an operator can line it up against the
// server's startup log without a translation table.
//
// Length is the discriminator that matters and it is emitted as a number rather
// than summarised: 0 means "a record index is absent from the file" — nothing
// was removed here and this may be an index a crash burned — while any positive
// value means BYTES WERE THROWN AWAY. See exitOutboxUnverifiedDrain.
type outboxDiscardJSON struct {
	Stage  string `json:"stage"`
	Index  uint64 `json:"index"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Reason string `json:"reason"`
}

// outboxIntegrity is whether the answer can be BELIEVED, and it is ALWAYS
// emitted — never null, never omitted — because a field that appears only when
// there is bad news is a field a consumer writes no branch for.
//
// It exists because wal.Replay reports record-level discards ONLY in its return
// value: it takes no logger and returns a nil error for them, so a reader that
// ignores the Recovered value discards in silence, which is the defect
// invariant 6 names (not the discard itself).
type outboxIntegrity struct {
	// Trustworthy is false whenever ANYTHING was discarded, refused or missing.
	// It is the one field a consumer must branch on before reading counts: when
	// it is false, every count below is a lower bound on what the log actually
	// held. It is exactly equivalent to exit_code being 1 or 8.
	Trustworthy bool `json:"trustworthy"`
	// WALDiscards is Recovered.DiscardCount: the EXACT number of discards,
	// including index-sequence holes. It is not len(discarded) — see Discarded.
	WALDiscards int `json:"wal_discards"`
	// MissingRecords is Recovered.MissingRecords: the total size of the interior
	// holes in the index sequence. wal documents it as an UPPER BOUND ON LOSS,
	// NOT A COUNT OF IT — a hole may be a record lost from the media, a record
	// an earlier recovery discarded without renumbering the survivors, or an
	// index range burned by a reservation a crash never used. Do not render it
	// to an operator as "records lost".
	MissingRecords uint64 `json:"missing_records"`
	// IndexGaps is how many of the RETAINED discards were zero-length, i.e.
	// holes rather than removed bytes. Gaps alone are exit 8; anything with
	// bytes behind it is exit 1.
	IndexGaps int `json:"index_gaps"`
	// OutboxRecordsRefused counts outbox records this replay could not turn into
	// a job: undecodable bodies, plus jobs seen ONLY as pending records that are
	// absent from the final table (refused past the retry horizon, future-dated,
	// self-addressed, or over the capacity cap).
	//
	// IT IS DELIBERATELY NOT "every record the table did not accept". A job
	// whose settlement is in the log is NOT counted when its old pending record
	// is refused, and a settled record dropped by retention is not counted
	// either: those are the retention policy working as designed, and counting
	// them would make every bus with a log older than the retention window
	// report an unverified drain forever. What is counted is the case that
	// actually costs an answer — a job that was PENDING and is in neither the
	// table nor a settlement.
	OutboxRecordsRefused int `json:"outbox_records_refused"`
	// DanglingPrepares is len(Recovered.Dangling): prepares that reached neither
	// commit nor abort. It is REPORTED BUT DOES NOT AFFECT trustworthy or the
	// exit code — a dangling prepare was never committed, so it was never
	// acknowledged to anyone (invariant 4), and it is the ordinary signature of
	// a crash between the two fsyncs rather than a loss.
	DanglingPrepares int `json:"dangling_prepares"`
	// Discarded is the DETAIL, and it is CAPPED: wal retains at most
	// maxDiscardsRetained entries so a file that is damage end to end cannot
	// make recovery hold it all in memory. wal_discards is exact. NEVER read
	// len(discarded) as the total — compare it against wal_discards, and if it
	// is smaller, the ones you cannot see are the reason this read is reported
	// as damaged rather than merely unverified.
	Discarded []outboxDiscardJSON `json:"discarded"`
}

// outboxFilterJSON echoes what -peer and -state selected, ALWAYS, so the
// machine audience can see the two-scopes distinction the human output states
// twice. Without it the only statement that counts and jobs answer different
// questions lives in a Go doc comment no consumer will ever read.
type outboxFilterJSON struct {
	Peer   string   `json:"peer"`
	States []string `json:"states"`
	Note   string   `json:"note"`
}

// outboxFilterNote is that one-line explanation, in the JSON rather than only
// in the source.
const outboxFilterNote = "jobs is filtered; counts, the per-peer breakdowns and exit_code are computed over the whole outbox"

// outboxResult is the single --json object, mirroring peerListResult's shape
// with `ok` first so a caller branches on the same field in the same position
// that `agent-bus peer` and `agent-bus invite` already publish.
//
// # counts AND jobs CAN DISAGREE, BY DESIGN
//
// Counts, PendingByPeer and AbandonedByPeer are computed over the WHOLE outbox
// BEFORE any filter — they are the verdict, and the verdict is what ExitCode
// reports. Jobs is only what -peer and -state SELECTED. So `-state abandoned`
// on a bus with two pending jobs emits counts.pending == 2, an empty
// pending_by_peer selection nowhere in jobs, and exit_code 6. Two fields that
// can disagree must be explained rather than left to be discovered: they are
// answering different questions, and the filtered one is never the answer.
//
// Every slice is ALWAYS emitted as [] and never as null, so a consumer can
// index without a nil check.
type outboxResult struct {
	// OK means A REPORT WAS PRODUCED — the log was read and the fields below are
	// populated. IT IS NOT A STATEMENT THAT THE REPORT CAN BE BELIEVED: read
	// Integrity.Trustworthy (and exit_code) for that. The two are kept apart
	// because `ok:false` is the failure SHAPE here (outboxError, with `error`
	// and `remedy` and no counts at all), and a consumer that meets `ok:false`
	// carrying a full report would look for an `error` field that is not there.
	OK      bool   `json:"ok"`
	BusID   string `json:"bus_id"`
	DataDir string `json:"data_dir"`
	// Integrity is whether this read can be trusted, and it is ALWAYS present.
	// It sits above the counts on purpose: it qualifies every one of them.
	Integrity outboxIntegrity `json:"integrity"`
	// Filter is what -peer and -state selected, ALWAYS present, and it applies
	// to Jobs ALONE — see the two-scopes note on this type.
	Filter outboxFilterJSON `json:"filter"`
	// RetentionWindowSeconds exposes limit 1 as a NUMBER as well as prose, so a
	// script can reason about the window instead of parsing "24 hours" out of
	// an English sentence.
	RetentionWindowSeconds int               `json:"retention_window_seconds"`
	Counts                 outboxCounts      `json:"counts"`
	PendingByPeer          []outboxPeerCount `json:"pending_by_peer"`
	AbandonedByPeer        []outboxPeerCount `json:"abandoned_by_peer"`
	Jobs                   []outboxJobJSON   `json:"jobs"`
	// Limits is outboxLimits verbatim. It is in the JSON because an agent
	// consuming this must be able to reach the same caveats a human reading the
	// text does; a caveat only humans can see is a caveat that gets dropped.
	Limits   []string `json:"limits"`
	ExitCode int      `json:"exit_code"`
}

// outboxError is the --json failure shape. It uses peerError's spelling — `ok`,
// `error`, `remedy`, `exit_code` — because a caller should branch on one
// contract across the server binary's offline subcommands.
type outboxError struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Remedy   string `json:"remedy,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// outboxCmdError carries the exit code and the remedy alongside the message so
// every failure path is one line at the call site. It mirrors logCommandError
// and peerCmdError.
type outboxCmdError struct {
	code   int
	msg    string
	remedy string
	cause  error
}

func (e *outboxCmdError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *outboxCmdError) Unwrap() error { return e.cause }

// outboxFail reports a failure in whichever mode was asked for and returns the
// exit code.
//
// In -json mode the object goes to STDOUT, not stderr: an agent that redirected
// stderr away still gets a parseable answer, which is the whole reason -json
// exists (invariant 7's second audience). On an encode failure the ORIGINAL
// code is returned — substituting 1 would report "the log is damaged" for what
// was actually a missing identity (4) or an unauthenticatable log (5), turning
// a broken pipe into a false statement about the operator's data.
func outboxFail(stdout, stderr io.Writer, asJSON bool, e *outboxCmdError) int {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(outboxError{OK: false, Error: e.Error(), Remedy: e.remedy, ExitCode: e.code}); err == nil {
			return e.code
		}
		fmt.Fprintf(stderr, "agent-bus %s: %s\n", outboxCommandName, e.Error())
		return e.code
	}
	fmt.Fprintf(stderr, "agent-bus %s: %s\n", outboxCommandName, e.Error())
	if e.remedy != "" {
		fmt.Fprintf(stderr, "  remedy: %s\n", e.remedy)
	}
	return e.code
}

// ---------------------------------------------------------------------------
// Flags
// ---------------------------------------------------------------------------

// outboxStateFilter collects a repeatable -state flag.
//
// It validates against relay.OutboxState.String() rather than calling relay's
// own parser, which is unexported: the three spellings are the ones that go on
// disk, so checking against String() keeps this filter tied to the durable
// vocabulary instead of to a private duplicate of it. An unrecognised value is
// an ERROR and never a default — guessing would silently answer a different
// question than the operator asked.
type outboxStateFilter struct {
	states []relay.OutboxState
	// raw preserves what was asked for, in order, for the human header.
	raw []string
}

func (f *outboxStateFilter) String() string { return strings.Join(f.raw, ",") }

func (f *outboxStateFilter) Set(v string) error {
	for _, s := range []relay.OutboxState{relay.OutboxPending, relay.OutboxDelivered, relay.OutboxAbandoned} {
		if s.String() == v {
			// Duplicates are NOT rejected and NOT deduplicated here: relay.Jobs
			// tests membership per record, so a repeated state yields each record
			// once. Refusing a duplicate would turn a harmless script into a
			// usage error for no benefit.
			f.states = append(f.states, s)
			f.raw = append(f.raw, v)
			return nil
		}
	}
	// The value is NOT echoed: it is unvalidated argv on its way to a terminal,
	// and the legal set is the useful half of the message.
	return errors.New("must be one of pending, delivered, abandoned")
}

// ---------------------------------------------------------------------------
// The command
// ---------------------------------------------------------------------------

// runOutboxCommand is `agent-bus outbox`.
//
// It returns an exit code rather than calling os.Exit so that it is testable in
// process; main() is the only place that exits. Same rule as runLogCommand.
func runOutboxCommand(args []string, stdout, stderr io.Writer) int {
	var (
		dataDir string
		asJSON  bool
		peer    string
		states  outboxStateFilter
	)
	fs := flag.NewFlagSet("agent-bus "+outboxCommandName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// A NO-OP Usage, for runLogCommand's reason: flag calls Usage both for -h
	// and for a bad flag, but requested help is OUTPUT and belongs on stdout
	// while an error is diagnostics and belongs on stderr. The two cases are
	// separated at the Parse call instead.
	fs.Usage = func() {}
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "the bus's data directory; it must already hold a bus identity")
	fs.BoolVar(&asJSON, "json", false, "emit one JSON object with \"ok\" first")
	fs.StringVar(&peer, "peer", "", "print only jobs owed to this peer bus (exact match); does not change the exit code")
	fs.Var(&states, "state", "print only jobs in this state (pending|delivered|abandoned); repeatable")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Requested help is OUTPUT: stdout, exit 0.
			fmt.Fprint(stdout, outboxUsage)
			return exitOutboxOK
		}
		// flag has already printed the specific error to stderr; the usage text
		// follows it there.
		fmt.Fprint(stderr, outboxUsage)
		return exitOutboxUsage
	}
	if fs.NArg() > 0 {
		// The argument is NOT echoed — unvalidated argv bound for a terminal.
		fmt.Fprintf(stderr, "agent-bus %s: unexpected argument\n", outboxCommandName)
		fmt.Fprint(stderr, outboxUsage)
		return exitOutboxUsage
	}
	if strings.TrimSpace(dataDir) == "" {
		return outboxFail(stdout, stderr, asJSON, &outboxCmdError{
			code:   exitOutboxUsage,
			msg:    "-data-dir must not be empty",
			remedy: "pass -data-dir <the bus's data directory>",
		})
	}
	// The LENGTH is checked before the value is ever printed back, the
	// discipline validatePeerCLIBusID uses: an oversized filter must not get to
	// choose the size of the diagnostic we print about refusing it. The value is
	// otherwise NOT validated as a bus id — an exact-match filter that selects
	// nothing is a legitimate answer, and the verdict does not depend on it.
	if len(peer) > relay.MaxPeerBusIDLen {
		return outboxFail(stdout, stderr, asJSON, &outboxCmdError{
			code:   exitOutboxUsage,
			msg:    fmt.Sprintf("-peer is %d bytes, but a bus id is at most %d; it is not echoed here because it is oversized", len(peer), relay.MaxPeerBusIDLen),
			remedy: "pass the peer's bus id exactly as that bus's own `agent-bus` reports it",
		})
	}

	// Errors only from the reader's own plumbing, but WARN is let through
	// deliberately: the sweep that runs as the table is queried logs every
	// pending job it drops at the retry horizon, individually and by name
	// (invariant 6's loud-discard requirement). That WARN line is the ONLY
	// trace such a job ever leaves — see limit 2 — so suppressing it here would
	// throw away the one piece of evidence this command can still surface about
	// it.
	lg := logging.New(stderr, logging.LevelWarn)

	busID, jobs, integrity, cerr := readOutbox(dataDir, lg)
	if cerr != nil {
		return outboxFail(stdout, stderr, asJSON, cerr)
	}

	result := buildOutboxResult(busID, dataDir, jobs, integrity, peer, states.states)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(stderr, "agent-bus %s: writing JSON failed: %v\n", outboxCommandName, err)
			// NOT exitOutboxDamaged: the log replayed fine and the verdict was
			// computed — only the report failed to reach stdout. Returning 1
			// here would state "the write-ahead log is damaged" about a healthy
			// log, which is the same false statement outboxFail refuses to make
			// when ITS encode fails. Returning result.ExitCode would be worse
			// still: a drained bus would exit 0 with no report behind it.
			return exitOutboxReportFailed
		}
		return result.ExitCode
	}
	writeOutboxHuman(stdout, result, peer, states.raw)
	return result.ExitCode
}

// ---------------------------------------------------------------------------
// The read
// ---------------------------------------------------------------------------

// readOutbox opens dataDir, takes the exclusive lock, rebuilds the outbox from
// the write-ahead log and returns EVERY retained record, unfiltered.
//
// It is split from the rendering so each half is drivable on its own: this one
// needs a real data directory, and the renderer needs nothing but records.
//
// # IT CREATES NOTHING, AND THAT IS ENFORCED BY CHECKS RATHER THAN INTENDED
//
// The order — stat, check, check, lock, RE-check both, load, replay — is
// openPeerStore's and readAuditTrail's, and each step is load-bearing:
//
//   - The PRE-LOCK checks keep a refusal from writing so much as a bus.lock into
//     a directory the operator mistyped. A lone bus.lock in a virgin directory
//     makes the operator's very first `agent-bus` start refuse to boot.
//   - The bus-id check is what makes ids.LoadOrCreateBusID's "Create" half
//     UNREACHABLE. Without it, a directory whose bus-id file was lost would get
//     a freshly minted id persisted into it, renaming the bus away from every
//     agent id it has ever issued ("<bus-id>.<agent-id>", invariants 1 and 2).
//   - The MAC key check is the landmine described in the file comment: without
//     it, wal.Replay MINTS the key that authenticates the log it is judging.
//   - The POST-LOCK re-checks close the time-of-check/time-of-use window rather
//     than arguing it away: between the two, another process could have removed
//     either file.
//
// Both guards are REUSED from their existing homes — checkPeerBusIDPresent
// (peer.go) and checkMACKeyPresent (auditlog.go), both already in package main
// — and their errors are translated to this command's codes. They are not
// copied: two copies of one guard drift, and the guard that drifts here mints a
// key.
func readOutbox(dataDir string, lg *logging.Logger) (string, []relay.OutboxRecord, outboxIntegrity, *outboxCmdError) {
	// The data directory is NOT created. run() does MkdirAll because a server is
	// entitled to start a fresh bus; a read-only reader is not, and a typo in
	// -data-dir that minted a whole new identity would report a drained outbox
	// for a bus that does not exist.
	info, err := os.Stat(dataDir)
	if err != nil {
		return "", nil, outboxIntegrity{}, &outboxCmdError{
			code:   exitOutboxNoIdentity,
			msg:    fmt.Sprintf("cannot read the data directory %q, so there is no outbox to read", dataDir),
			remedy: "check -data-dir; this command never creates one",
			cause:  err,
		}
	}
	if !info.IsDir() {
		return "", nil, outboxIntegrity{}, &outboxCmdError{
			code:   exitOutboxNoIdentity,
			msg:    fmt.Sprintf("-data-dir %q is not a directory", dataDir),
			remedy: "point -data-dir at the bus's data directory",
		}
	}
	// BEFORE THE LOCK, so that either refusal writes NOTHING AT ALL — not even
	// the bus.lock that dirlock.Acquire creates.
	if e := outboxBusIDGuard(dataDir); e != nil {
		return "", nil, outboxIntegrity{}, e
	}
	if e := outboxMACKeyGuard(dataDir); e != nil {
		return "", nil, outboxIntegrity{}, e
	}

	lock, err := dirlock.Acquire(dataDir)
	if err != nil {
		if errors.Is(err, dirlock.ErrLocked) {
			return "", nil, outboxIntegrity{}, &outboxCmdError{
				code: exitOutboxBusRunning,
				msg:  "the data directory is locked by a live process, which is almost certainly the bus itself",
				remedy: "stop the bus and retry. That is not an inconvenience here: a running bus is still enqueueing, " +
					"so a drain cannot be verified against one, and stopping it is the restart's first half anyway",
				cause: err,
			}
		}
		return "", nil, outboxIntegrity{}, &outboxCmdError{
			code:   exitOutboxDamaged,
			msg:    "locking the data directory",
			remedy: "check the data directory's permissions and that no stale lock file is unwritable",
			cause:  err,
		}
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := lock.Release(); err != nil {
			lg.Error("releasing the data directory lock failed", "data_dir", dataDir, "err", err)
		}
	}
	defer release()

	// RE-CHECK BOTH under the lock. The pre-lock pair exists so a refusal writes
	// nothing; these are what make "never creates an identity" and "never mints
	// a MAC key" true rather than merely likely.
	if e := outboxBusIDGuard(dataDir); e != nil {
		return "", nil, outboxIntegrity{}, e
	}
	if e := outboxMACKeyGuard(dataDir); e != nil {
		return "", nil, outboxIntegrity{}, e
	}

	// A LOAD, never a create: the check immediately above, taken under the lock,
	// is the only thing that makes LoadOrCreateBusID's "Create" half unreachable.
	busID, err := ids.LoadOrCreateBusID(dataDir, "")
	if err != nil {
		return "", nil, outboxIntegrity{}, &outboxCmdError{
			code:   exitOutboxDamaged,
			msg:    "resolving the bus id",
			remedy: "check the permissions and contents of the bus-id file in the data directory",
			cause:  err,
		}
	}

	// Durable IS NIL, and that is what makes this STRUCTURALLY read-only: every
	// mutating call on the returned outbox fails with relay.ErrOutboxNotDurable
	// rather than merely not being made.
	ob, err := relay.NewOutbox(relay.OutboxOptions{BusID: busID, Logger: lg})
	if err != nil {
		return "", nil, outboxIntegrity{}, &outboxCmdError{
			code:   exitOutboxDamaged,
			msg:    "creating the read-only outbox",
			remedy: "the bus itself fails on the same error at startup; fix it there first",
			cause:  err,
		}
	}

	// wal.Replay is the package's read-only fsck: it repairs nothing, truncates
	// nothing and creates no file. A missing or zero-length bus.wal is reported
	// by it as an EMPTY log rather than as damage, which is the honest answer —
	// a bus that never wrote a record owes nobody anything.
	//
	// THE Recovered VALUE IS KEPT, AND THAT IS A FIX RATHER THAN A DETAIL.
	// wal.Replay returns a NIL ERROR for a record-level discard — it reports
	// those only in Recovered, and it takes no logger on purpose (wal.Open is
	// what normally logs them). So `if _, err := wal.Replay(...)` reads as a
	// clean replay of a file that just lost a record, and this command would go
	// on to print VERDICT: DRAINED over the hole the pending job was in. That is
	// exactly the silent discard invariant 6 rates as the defect.
	walPath := filepath.Join(dataDir, wal.WALFileName)
	tally := &outboxReplayTally{pending: map[string]relay.OutboxRecord{}, settled: map[string]struct{}{}}
	rec, err := wal.Replay(walPath, tally.apply(ob))
	if err != nil {
		return "", nil, outboxIntegrity{}, &outboxCmdError{
			code: exitOutboxDamaged,
			msg:  fmt.Sprintf("replaying the write-ahead log %q, so THE QUESTION WAS NOT ANSWERED: this is not evidence that the outbox is drained", walPath),
			remedy: "the bus itself repairs a damaged log at startup and this read-only path deliberately does not; " +
				"start the bus once against this directory and retry, and treat the rollout as blocked until it answers",
			cause: err,
		}
	}

	// Every state, unfiltered. The verdict is computed over ALL of it; the
	// filters are applied later and only to what is PRINTED.
	jobs := ob.Jobs()
	integrity := outboxIntegrityOf(rec, tally, jobs, lg)
	release()
	return busID, jobs, integrity, nil
}

// outboxReplayTally watches the records go past on their way into the outbox,
// so that a record the table REFUSED can be detected without changing relay's
// API.
//
// It is needed because Outbox.Apply swallows its own refusals by design: an
// undecodable body and a record upsertLocked rejects are both logged at ERROR
// and then return nil, because returning an error from an applier is a hard
// failure that would make a live wal.Open refuse to start. Correct for the
// server; invisible to a reader that only looks at the error.
//
// It records STATE PER JOB rather than a bare count. A count of records cannot
// be compared with a count of jobs — one job legitimately writes a pending
// record and then a settlement — so the comparison that means something is
// "which job ids did the log mention, and which of those are missing from the
// table, and did the log also carry a settlement for them".
type outboxReplayTally struct {
	// undecodable is a body that could not be turned into a record at all. The
	// job it described is unknowable, so it is always a loss of information.
	undecodable int
	// pending holds the FIRST pending record seen for each job id, keyed by job
	// id — the detail needed to name the job in a log line if it goes missing.
	pending map[string]relay.OutboxRecord
	// settled is the set of job ids the log carried a terminal record for. A
	// pending record for one of these being absent from the table is retention
	// working, not a refusal.
	settled map[string]struct{}
}

// apply wraps the outbox's own applier with the counting.
//
// ONLY relay.OutboxRecordKind RECORDS ARE JUDGED. A WAL carries other kinds and
// they belong to other subsystems; counting one of those as "an outbox record
// refused" would be this command reporting on evidence it cannot read.
func (t *outboxReplayTally) apply(ob *relay.Outbox) func(wal.Committed) error {
	return func(c wal.Committed) error {
		if c.Entry.Kind == relay.OutboxRecordKind {
			if r, err := relay.DecodeOutboxRecord(c.Entry.Body); err != nil {
				t.undecodable++
			} else if r.State.Terminal() {
				t.settled[r.JobID] = struct{}{}
			} else if _, seen := t.pending[r.JobID]; !seen {
				t.pending[r.JobID] = r
			}
		}
		return ob.Apply(c)
	}
}

// outboxIntegrityOf turns the replay's own account of itself into the
// trust-of-the-answer block, and LOGS every discard on the way past.
//
// The logging is not decoration. Invariant 6's requirement is that a discard is
// recorded loudly and specifically, and this is the read path that would
// otherwise be the quiet one: wal.Replay has no logger, so nothing else in this
// process will ever mention these records.
func outboxIntegrityOf(rec wal.Recovered, t *outboxReplayTally, jobs []relay.OutboxRecord, lg *logging.Logger) outboxIntegrity {
	out := outboxIntegrity{
		WALDiscards:      rec.DiscardCount,
		MissingRecords:   rec.MissingRecords,
		DanglingPrepares: len(rec.Dangling),
		Discarded:        []outboxDiscardJSON{},
	}
	for _, d := range rec.Discarded {
		out.Discarded = append(out.Discarded, outboxDiscardJSON{
			Stage: d.Stage, Index: d.Index, Offset: d.Offset, Length: d.Length, Reason: d.Reason,
		})
		if d.Length == 0 && !d.Severe && d.Stage != "framing" {
			out.IndexGaps++
			// WARN, not ERROR: a hole may be nothing but an index a crash burned.
			// The logger quotes every value it writes, so the stored reason text
			// cannot forge a second log record here.
			lg.Warn("a record index is ABSENT from the write-ahead log; nothing was removed here, and this is an UPPER BOUND on loss rather than a count of it",
				"stage", d.Stage, "index", d.Index, "offset", d.Offset, "reason", d.Reason)
			continue
		}
		lg.Error("the write-ahead log DISCARDED a record; if it was a pending outbox job, that relay hop is LOST and no count below can see it",
			"stage", d.Stage, "index", d.Index, "offset", d.Offset, "length", d.Length, "severe", d.Severe, "reason", d.Reason)
	}
	if t.undecodable > 0 {
		out.OutboxRecordsRefused += t.undecodable
		lg.Error("outbox records in the write-ahead log could not be DECODED, so the jobs they described are unknowable and are in no count below",
			"records", t.undecodable)
	}

	// A JOB THE LOG MENTIONED AS PENDING AND THE TABLE DOES NOT HOLD. See
	// outboxIntegrity.OutboxRecordsRefused for why a settled sibling exempts it:
	// a settlement swept by retention is the policy working, while a pending job
	// that reached neither the table nor a settlement is an answer this command
	// has lost.
	present := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		present[j.JobID] = struct{}{}
	}
	missing := make([]string, 0, len(t.pending))
	for id := range t.pending {
		if _, ok := present[id]; ok {
			continue
		}
		if _, ok := t.settled[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	// Sorted so two runs over the same directory emit the same lines in the same
	// order; a map is not an order.
	sort.Strings(missing)
	for _, id := range missing {
		r := t.pending[id]
		out.OutboxRecordsRefused++
		lg.Error("an outbox job was PENDING in the write-ahead log and is NOT in the recovered table; it was refused or aged out during this replay, so this command cannot say whether it was ever delivered",
			"job_id", r.JobID, "peer_bus", r.PeerBusID, "origin_message_id", r.OriginMessageID,
			"enqueued_at", r.EnqueuedAt.UTC().Format(time.RFC3339Nano))
	}

	out.Trustworthy = out.WALDiscards == 0 && out.MissingRecords == 0 && out.OutboxRecordsRefused == 0
	return out
}

// outboxBytesWereLost reports whether the replay threw BYTES away, which is the
// difference between exit 1 and exit 8.
//
// The last clause is the conservative one and it matters: wal retains at most
// maxDiscardsRetained Discard entries while DiscardCount stays exact, so a log
// with more discards than that has losses NOBODY CAN INSPECT. "I cannot see
// what I discarded" is read as damage, not as a gap — a hundred discards is not
// the signature of a tidy crash.
func outboxBytesWereLost(i outboxIntegrity) bool {
	if i.WALDiscards > len(i.Discarded) {
		return true
	}
	// Every retained discard that is NOT an index gap took something with it —
	// bytes, a frame wal could not read, or a loss wal itself marked Severe.
	// IndexGaps counts ONLY the zero-length, non-framing, non-Severe kind, so
	// this subtraction is the whole discrimination in one line.
	return i.WALDiscards > i.IndexGaps
}

// outboxBusIDGuard is checkPeerBusIDPresent (peer.go) with its exit code
// translated. The CHECK is not duplicated — only the mapping from peer's code
// space to this command's, which happen to be the same number for the same
// reason: "this is not a bus's data directory".
func outboxBusIDGuard(dataDir string) *outboxCmdError {
	e := checkPeerBusIDPresent(dataDir)
	if e == nil {
		return nil
	}
	// The MESSAGE is restated because peer.go's names the reason IT cares about
	// ("no identity to configure federation for"), and this command configures
	// nothing. The remedy is reused verbatim: "restore bus-id, do not let
	// anything recreate it" is the same advice either way.
	return &outboxCmdError{
		code: exitOutboxNoIdentity,
		msg: fmt.Sprintf("this data directory holds no bus id file (%q), so it is not a bus's data directory and there is no outbox to read",
			filepath.Join(dataDir, busIDFileName)),
		remedy: e.remedy,
		cause:  e.cause,
	}
}

// outboxMACKeyGuard is checkMACKeyPresent (auditlog.go) with its error
// translated.
//
// The GUARD ITSELF IS REUSED, deliberately and not out of tidiness: it is the
// one check standing between a read-only command and wal minting the key that
// authenticates the file it is about to judge, and two copies of it would
// drift. Only the message is restated, because auditlog's names bus.audit while
// the artefact here is the write-ahead log; the remedy is reused verbatim,
// since "restore the key, do not let anything create one" is the same advice
// for both files.
func outboxMACKeyGuard(dataDir string) *outboxCmdError {
	e := checkMACKeyPresent(dataDir)
	if e == nil {
		return nil
	}
	return &outboxCmdError{
		code: exitOutboxUnverifiable,
		msg: fmt.Sprintf("this data directory holds no MAC key (%q), so NOTHING in %s can be authenticated; the outbox was NOT read and no key was created",
			filepath.Join(dataDir, wal.MACKeyFileName), wal.WALFileName),
		remedy: e.remedy,
		cause:  e.cause,
	}
}

// ---------------------------------------------------------------------------
// The verdict and the rendering
// ---------------------------------------------------------------------------

// buildOutboxResult turns the unfiltered record set into the answer.
//
// THE VERDICT IS COMPUTED FIRST, OVER EVERYTHING. Counts, both per-peer
// breakdowns and ExitCode are derived from `all` before peerFilter or
// stateFilter is consulted; the filters then select only what goes in Jobs. A
// filter that could change the verdict would make an exit 0 from this command
// unsafe to act on, which is the one thing it exists to be.
func buildOutboxResult(busID, dataDir string, all []relay.OutboxRecord, integrity outboxIntegrity, peerFilter string, stateFilter []relay.OutboxState) outboxResult {
	// The applied filter is echoed back from the SAME values that select the
	// jobs below, so the echo cannot come to describe a different filter than
	// the one that ran. The state names are taken from OutboxState.String(),
	// which is what -state parses against and what goes on disk.
	filter := outboxFilterJSON{Peer: peerFilter, States: []string{}, Note: outboxFilterNote}
	for _, s := range stateFilter {
		filter.States = append(filter.States, s.String())
	}
	out := outboxResult{
		OK:                     true,
		BusID:                  busID,
		DataDir:                dataDir,
		Integrity:              integrity,
		Filter:                 filter,
		RetentionWindowSeconds: int(relay.OutboxSettledRetention / time.Second),
		PendingByPeer:          []outboxPeerCount{},
		AbandonedByPeer:        []outboxPeerCount{},
		Jobs:                   []outboxJobJSON{},
		Limits:                 outboxLimits,
	}
	if out.Integrity.Discarded == nil {
		// Never null, so a consumer can range over it without a nil check.
		out.Integrity.Discarded = []outboxDiscardJSON{}
	}

	pendingByPeer := map[string]*outboxPeerCount{}
	abandonedByPeer := map[string]*outboxPeerCount{}
	for _, r := range all {
		out.Counts.Retained++
		switch r.State {
		case relay.OutboxPending:
			out.Counts.Pending++
			accumulateOutboxPeer(pendingByPeer, r)
		case relay.OutboxDelivered:
			out.Counts.Delivered++
		case relay.OutboxAbandoned:
			out.Counts.Abandoned++
			accumulateOutboxPeer(abandonedByPeer, r)
		}
	}
	out.PendingByPeer = sortedOutboxPeerCounts(pendingByPeer)
	out.AbandonedByPeer = sortedOutboxPeerCounts(abandonedByPeer)

	// THE PRECEDENCE:  1  >  8  >  6  >  7  >  0.
	//
	// TRUST OUTRANKS VERDICT — "can I believe this?" is answered before "what
	// does it say?" — so 1 (bytes were thrown away) and 8 (records absent or
	// refused) come first, and EXIT 0 IS UNREACHABLE whenever anything was
	// discarded, refused or missing. That is the point: a read that lost the one
	// pending record in the file must not be able to spell itself the same way
	// as a genuinely drained bus.
	//
	// Then 6 BEFORE 7: "do not restart yet" outranks "something was already
	// lost". Both are still visible in the counts and in the job list, and so
	// are the counts under a 1 or an 8 — a suppressed report would leave the
	// operator with nothing to act on at exactly the moment they need most.
	switch {
	case !integrity.Trustworthy && outboxBytesWereLost(integrity):
		out.ExitCode = exitOutboxDamaged
	case !integrity.Trustworthy:
		out.ExitCode = exitOutboxUnverifiedDrain
	case out.Counts.Pending > 0:
		out.ExitCode = exitOutboxPending
	case out.Counts.Abandoned > 0:
		out.ExitCode = exitOutboxAbandoned
	default:
		out.ExitCode = exitOutboxOK
	}

	// ONLY NOW are the filters applied, and only to the printed list. `all` is
	// already in relay.Jobs's deterministic order (oldest enqueue first, job id
	// as the tie-break), and selecting from it in order preserves that.
	for _, r := range all {
		if peerFilter != "" && r.PeerBusID != peerFilter {
			continue
		}
		if !outboxStateSelected(r.State, stateFilter) {
			continue
		}
		out.Jobs = append(out.Jobs, outboxJobJSON{
			JobID:           r.JobID,
			PeerBusID:       r.PeerBusID,
			OriginMessageID: r.OriginMessageID,
			State:           r.State.String(),
			EnqueuedAt:      formatOutboxTime(r.EnqueuedAt),
			SettledAt:       formatOutboxTime(r.SettledAt),
			Reason:          r.Reason,
			Size:            r.Size,
			ContentSHA256:   r.ContentSHA256,
		})
	}
	return out
}

// outboxStateSelected reports whether a record survives the -state filter. An
// EMPTY filter selects everything, matching relay.Jobs's own convention so the
// two cannot come to mean different things.
func outboxStateSelected(s relay.OutboxState, filter []relay.OutboxState) bool {
	if len(filter) == 0 {
		return true
	}
	for _, want := range filter {
		if want == s {
			return true
		}
	}
	return false
}

// accumulateOutboxPeer folds one record into a per-peer row, keeping the OLDEST
// enqueue time. `all` arrives oldest-first, so the first record seen for a peer
// is already the oldest — the comparison is kept anyway rather than resting on
// a caller's ordering for a correctness claim an operator will read as an age.
func accumulateOutboxPeer(into map[string]*outboxPeerCount, r relay.OutboxRecord) {
	row, ok := into[r.PeerBusID]
	if !ok {
		into[r.PeerBusID] = &outboxPeerCount{PeerBusID: r.PeerBusID, Jobs: 1, OldestEnqueuedAt: formatOutboxTime(r.EnqueuedAt)}
		return
	}
	row.Jobs++
	if oldest := formatOutboxTime(r.EnqueuedAt); oldest != "" && (row.OldestEnqueuedAt == "" || oldest < row.OldestEnqueuedAt) {
		// RFC3339Nano in UTC with a fixed layout sorts lexicographically in the
		// same order it sorts chronologically, which is why the comparison is on
		// the formatted value: one representation, compared once.
		row.OldestEnqueuedAt = oldest
	}
}

// sortedOutboxPeerCounts flattens a per-peer map into a slice ordered by bus id.
// Map order is not an order; an operator diffing two runs of this command must
// see the rows in the same sequence both times.
func sortedOutboxPeerCounts(rows map[string]*outboxPeerCount) []outboxPeerCount {
	out := make([]outboxPeerCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerBusID < out[j].PeerBusID })
	return out
}

// formatOutboxTime renders a timestamp the way the durable record does —
// RFC3339Nano in UTC — so the operator reading this output and the operator
// reading a record out of the log see the same string. A zero time is "",
// because a pending job has no settle time and "0001-01-01T00:00:00Z" reads as
// data rather than as absence.
func formatOutboxTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// writeOutboxHuman prints the report for a person.
//
// The order is deliberate: the VERDICT first, because that is the question;
// then the counts; then who is owed what; then the individual jobs; and the
// LIMITS OF THIS VIEW block LAST, so it is the final thing read before a
// decision is made. Putting the limits at the top would let them scroll away
// above a long job list on exactly the run where they matter most.
func writeOutboxHuman(w io.Writer, out outboxResult, peerFilter string, stateFilter []string) {
	// The data directory is QUOTED. It is argv, so it is whatever the caller
	// typed or whatever a script interpolated, and an unquoted path containing
	// ESC could repaint the report below it — see the same treatment of a job's
	// stored reason further down, where the attack is spelled out in full. The
	// bus id needs no quoting: ids.LoadOrCreateBusID validates the persisted
	// value and refuses a corrupt one rather than returning it.
	fmt.Fprintf(w, "Relay outbox for bus %s (%s).\n\n", out.BusID, strconv.Quote(out.DataDir))

	writeOutboxIntegrity(w, out.Integrity)

	// THE VERDICT IS SELECTED ON THE COUNTS AND ON TRUSTWORTHINESS, NOT ON
	// out.ExitCode. Switching on the exit code would put every trust code (1, 8)
	// into the default branch — and the default branch is the one that prints
	// "DRAINED". That is precisely how a discarded record produced a clean drain
	// report before this was fixed.
	switch {
	case out.Counts.Pending > 0:
		fmt.Fprintf(w, "VERDICT: NOT DRAINED — %d job(s) still pending. Do NOT restart onto a new binary;\n", out.Counts.Pending)
		fmt.Fprint(w, "         start the old binary again, let it deliver, and run this command again.\n")
		if out.Counts.Abandoned > 0 {
			fmt.Fprintf(w, "         (%d job(s) are also ABANDONED — see below. The pending question is reported\n", out.Counts.Abandoned)
			fmt.Fprint(w, "          first because it is the one that gates the restart.)\n")
		}
	case out.Counts.Abandoned > 0:
		fmt.Fprintf(w, "VERDICT: DRAINED, BUT %d JOB(S) WERE ABANDONED — messages this bus accepted will\n", out.Counts.Abandoned)
		if out.Integrity.Trustworthy {
			fmt.Fprint(w, "         never reach their peer. Nothing is pending, so a restart is safe.\n")
		} else {
			fmt.Fprint(w, "         never reach their peer. Nothing PENDING WAS FOUND — but the read above was\n")
			fmt.Fprint(w, "         incomplete, so that is not the same as nothing being pending.\n")
		}
	case !out.Integrity.Trustworthy:
		fmt.Fprint(w, "VERDICT: UNVERIFIED — nothing pending and nothing abandoned was FOUND, but the read\n")
		fmt.Fprint(w, "         above was incomplete, so this is NOT a drain and must not be acted on as\n")
		fmt.Fprint(w, "         one. Do not start the new binary on the strength of this run.\n")
	default:
		fmt.Fprint(w, "VERDICT: DRAINED — nothing pending and nothing abandoned in the retained window.\n")
		fmt.Fprint(w, "         It is safe to start the new binary.\n")
	}

	fmt.Fprintf(w, "\nCOUNTS (over the WHOLE outbox — filters never change these, or the exit code)\n")
	fmt.Fprintf(w, "  retained %d   pending %d   delivered %d   abandoned %d\n",
		out.Counts.Retained, out.Counts.Pending, out.Counts.Delivered, out.Counts.Abandoned)

	writeOutboxPeerSection(w, "PENDING BY PEER (what this bus still owes)", out.PendingByPeer)
	writeOutboxPeerSection(w, "ABANDONED BY PEER (what this bus gave up on)", out.AbandonedByPeer)

	fmt.Fprint(w, "\nJOBS")
	if peerFilter != "" || len(stateFilter) > 0 {
		fmt.Fprint(w, " (filtered")
		if peerFilter != "" {
			// QUOTED for the reason the reason and the data dir are: -peer is
			// length-bounded (64) but deliberately NOT charset-validated, and 64
			// bytes is more than enough for a clear-screen, a cursor-home and a
			// forged VERDICT line printed under the real one. A drain gate must
			// not be able to render a lie, whoever supplied the bytes.
			fmt.Fprintf(w, "; peer %s", strconv.Quote(peerFilter))
		}
		if len(stateFilter) > 0 {
			fmt.Fprintf(w, "; state %s", strings.Join(stateFilter, ","))
		}
		fmt.Fprint(w, " — the counts and the verdict above are NOT filtered)")
	}
	fmt.Fprint(w, "\n")
	if len(out.Jobs) == 0 {
		fmt.Fprint(w, "  (none)\n")
	}
	for _, j := range out.Jobs {
		// The detail lines are indented by a FIXED amount rather than padded to
		// the width of the job id. A job id is "<peer>|<origin-message-id>" and
		// is not bounded to any column width, so column padding would misalign
		// on every real record and read as damage.
		fmt.Fprintf(w, "  %s  %s  peer %s\n", j.JobID, j.State, j.PeerBusID)
		fmt.Fprintf(w, "      message %s  (%d bytes, sha256 %s)\n", j.OriginMessageID, j.Size, j.ContentSHA256)
		fmt.Fprintf(w, "      enqueued %s\n", j.EnqueuedAt)
		if j.SettledAt != "" {
			fmt.Fprintf(w, "      settled  %s\n", j.SettledAt)
		}
		if j.Reason != "" {
			// THE REASON IS QUOTED, AND THAT IS A SECURITY CONTROL RATHER THAN A
			// TYPOGRAPHIC ONE — its purpose is invisible from the code alone, so
			// it is spelled out here.
			//
			// sanitiseOutboxReason and OutboxRecord.validate between them enforce
			// exactly two things about a stored reason: that it is valid UTF-8
			// and that it is at most relay.MaxOutboxReasonLen bytes. CONTROL
			// CHARACTERS SURVIVE BOTH. Printed raw, a reason of
			// "\x1b[2J\x1b[H\rVERDICT: DRAINED - nothing pending. Safe to
			// restart.\n" clears the operator's screen, homes the cursor and
			// paints A FAKE VERDICT over the real one — on the one command whose
			// entire product is a restart decision.
			//
			// The realistic source is not a live peer (PeerRefusedError.Code is
			// allow-listed, and the other routes into a job's reason go through
			// %q): it is an ATTACKER-AUTHORED DATA DIRECTORY — a copied, restored
			// or forensic one, which is exactly what an offline tool is pointed
			// at, and which authenticates itself because wal-mac.key sits in the
			// same directory as bus.wal.
			//
			// strconv.Quote is the house answer, the same one
			// internal/logging.writeValue calls "the log-injection defence": it
			// escapes newlines, every other control character and every non-ASCII
			// byte. -json was already safe (encoding/json escapes control
			// characters); this is the human path catching up.
			fmt.Fprintf(w, "      REASON:  %s\n", strconv.Quote(j.Reason))
		}
	}

	fmt.Fprint(w, "\nLIMITS OF THIS VIEW — read both before you trust a quiet answer\n")
	for i, limit := range out.Limits {
		fmt.Fprintf(w, "  %d. %s\n", i+1, limit)
	}

	fmt.Fprint(w, "\nTHE ROLLOUT SEQUENCE THIS EXISTS FOR\n")
	fmt.Fprint(w, "  stop bus A -> run this -> if nothing is pending, start the NEW binary;\n")
	fmt.Fprint(w, "  otherwise start the OLD binary again and wait. A stopped bus does not defeat\n")
	fmt.Fprint(w, "  the drain gate: the stop is the restart's first half anyway.\n")
}

// writeOutboxIntegrity prints whether the answer can be believed, ABOVE the
// verdict.
//
// The position is the point: a reader must know the answer is doubtful BEFORE
// they read the answer. Below the verdict it is a footnote on a decision
// already made, and this report is read by someone deciding whether to restart
// a bus.
//
// The clean case is STATED rather than left blank, the same convention
// writeOutboxPeerSection follows for an empty section: "no bad news" and "this
// report does not cover bad news" must not look identical.
func writeOutboxIntegrity(w io.Writer, i outboxIntegrity) {
	if i.Trustworthy {
		fmt.Fprint(w, "INTEGRITY OF THIS READ: the log replayed with nothing discarded, no record index\n")
		fmt.Fprint(w, "         absent and no outbox record refused, so the counts below are the whole of\n")
		fmt.Fprint(w, "         what the log holds.\n")
		// REPORTED EVEN ON THE TRUSTWORTHY PATH, because wal.Replay has no logger
		// and this read path is otherwise the quiet one. A dangling prepare is NOT
		// a trust signal -- it is the ordinary crash-between-the-two-fsyncs
		// signature, and nothing about it was ever acknowledged (invariant 4), so
		// it does not make the answer doubtful. But wal.Open logs these at WARN on
		// a normal start, and an operator who reads "nothing discarded" and is
		// never told a prepare was dropped has been told less than the bus itself
		// would have told them.
		if i.DanglingPrepares > 0 {
			fmt.Fprintf(w, "         (%d prepare(s) reached neither commit nor abort and were dropped: a crash\n", i.DanglingPrepares)
			fmt.Fprint(w, "          between the two fsyncs. NOTHING THERE WAS EVER ACKNOWLEDGED, so no message\n")
			fmt.Fprint(w, "          was lost and the verdict below still stands.)\n")
		}
		fmt.Fprint(w, "\n")
		return
	}

	if outboxBytesWereLost(i) {
		fmt.Fprint(w, "!! THE ANSWER BELOW CANNOT BE TRUSTED — THE WRITE-AHEAD LOG IS DAMAGED !!\n")
		fmt.Fprint(w, "   The replay THREW RECORDS AWAY. If one of them was a pending job, it is in no\n")
		fmt.Fprint(w, "   count and no list below, and this command cannot tell you it existed. Do NOT\n")
		fmt.Fprint(w, "   read a quiet verdict here as a drain.\n")
	} else {
		fmt.Fprint(w, "!! THE DRAIN IS UNVERIFIED — RECORDS ARE MISSING OR WERE REFUSED !!\n")
		fmt.Fprint(w, "   The log was read and NO BYTES WERE LOST, so this is NOT a claim of damage.\n")
		if i.IndexGaps > 0 || i.MissingRecords > 0 {
			// The caveat is printed ONLY when a gap was actually seen. Telling an
			// operator their data may be gone when the run found nothing but
			// refusals would be the same over-statement in the other direction.
			fmt.Fprint(w, "   An absent record index is an UPPER BOUND on what could have been lost, not a\n")
			fmt.Fprint(w, "   count of it: the index may simply be one a crash burned that no record ever\n")
			fmt.Fprint(w, "   carried, or one an earlier recovery discarded without renumbering the rest.\n")
		}
		if i.OutboxRecordsRefused > 0 {
			fmt.Fprint(w, "   Outbox records in this log were REFUSED as they were applied (see the ERROR\n")
			fmt.Fprint(w, "   lines above): they are in no count and no list below.\n")
		}
		fmt.Fprint(w, "   Either way, records the log held are not in the table, so the verdict below is\n")
		fmt.Fprint(w, "   NOT a drain that has been verified.\n")
	}
	fmt.Fprintf(w, "   discarded %d   record indices absent %d (upper bound)   index gaps %d\n",
		i.WALDiscards, i.MissingRecords, i.IndexGaps)
	fmt.Fprintf(w, "   outbox records refused %d   dangling prepares %d (never committed, so never acknowledged)\n",
		i.OutboxRecordsRefused, i.DanglingPrepares)
	if len(i.Discarded) < i.WALDiscards {
		fmt.Fprintf(w, "   ONLY %d OF THE %d DISCARDS ARE DETAILED BELOW — the write-ahead log caps how many\n", len(i.Discarded), i.WALDiscards)
		fmt.Fprint(w, "   it retains detail for, so the rest cannot be inspected at all.\n")
	}
	for _, d := range i.Discarded {
		// stage/index/offset/length first so two discards line up when read down
		// the page, and the reason QUOTED for the same terminal-repainting reason
		// a job's reason is quoted: this text comes out of the data directory.
		fmt.Fprintf(w, "   discarded: stage=%s index=%d offset=%d length=%d\n", d.Stage, d.Index, d.Offset, d.Length)
		fmt.Fprintf(w, "     reason: %s\n", strconv.Quote(d.Reason))
	}
	fmt.Fprint(w, "\n")
}

// writeOutboxPeerSection prints one per-peer breakdown. The absence is STATED
// rather than left blank, following `peer list`: an empty pending section is
// the answer an operator is looking for, and it must be visible rather than
// inferred from a missing heading.
func writeOutboxPeerSection(w io.Writer, title string, rows []outboxPeerCount) {
	fmt.Fprintf(w, "\n%s\n", title)
	if len(rows) == 0 {
		fmt.Fprint(w, "  (none)\n")
		return
	}
	for _, row := range rows {
		fmt.Fprintf(w, "  %-24s %d job(s)  oldest enqueued %s\n", row.PeerBusID, row.Jobs, row.OldestEnqueuedAt)
	}
}
