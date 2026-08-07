package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// EnrolmentSuffixesInWAL reports, per agent name, the highest suffix appearing
// in an ENROLMENT RECORD in this WAL — committed, aborted AND dangling.
//
// # IT IS NOT A FLOOR. DO NOT SEAL IT INTO AN ALLOCATOR.
//
// The name of this function used to be SuffixFloors and its first line used to
// claim "the highest suffix EVER WRITTEN TO DISK by this bus". That claim was
// FALSE, the security gate reproduced it, and both the name and the sentence
// were the whole of the danger — so both are gone. What it actually returns is
// the maximum over records of Kind == RecordKind, and that is a STRICT SUBSET of
// the agent ids on disk:
//
//   - An agent id is also durable inside a store "message" record, as the sender
//     and in the recipient list. Those records are Kind == store.RecordKind and
//     this function does not look at them at all.
//   - That subset was once EMPTY, and on a pre-AUTH-7 data directory it still
//     is: enrolment records only began reaching the WAL when the durable roster
//     was wired into cmd/agent-bus/main.go (commit aad611c — the wave was
//     labelled "AUTH-7", which is not a real task key; cite the sha, it stays
//     checkable), so on anything the binary wrote before that, the message
//     records are the entire population.
//     Reproduced by the security gate: a WAL holding sender bus-abc.worker-9
//     returns an EMPTY map and a NIL error, which is indistinguishable from a
//     fresh bus and is exactly the claim ids.Seal accepts. Sealing it re-mints
//     worker-1..worker-9 onto different keypairs — the mass identity reuse this
//     whole area exists to prevent.
//   - A DURABLE ENROLMENT DOES NOT CLOSE THAT, and this is the bullet to read
//     twice. This paragraph used to say enrolment was "still memory-only" as a
//     PRESENT FACT; that stopped being true when AUTH-7 landed, and the
//     tempting inference — the subset is no longer empty, so the map has become
//     sealable — is WRONG. Two holes remain and either one alone is fatal.
//     (a) A message record still names its sender and its recipients, and this
//     function does not look at message records; those suffixes are burned all
//     the same. (b) A PREPARE torn by a crash carries a suffix that WAS issued
//     and that no scan of this log can ever name again, because the bytes that
//     named it never reached the platter. Hole (b) is not a thought experiment:
//     TestAuthCrashInjectionTornPrepare (crash point D) SIGKILLs a real process
//     mid-prepare and then asserts that this function reports worker:1 for a
//     bus that had already issued worker-7. A floor must cover every id ever
//     issued. This map covers the ones that happened to survive.
//
// So: a caller may use this to CROSS-CHECK, to audit, or to answer "what did the
// enrolment path actually write". A caller may NOT hand the result to
// ids.ResumeNameSuffixes and Seal it. There is no combination of arguments that
// makes that safe, which is why the function is no longer named as though there
// were.
//
// # There is no safe-and-available ordering against wal.Open either
//
// Also found by the security gate, also reproduced, and it has no fix at this
// layer:
//
//   - Scanning BEFORE wal.Open reads an unrepaired file, so wal.ScanAll's strict
//     framing errors on an ORDINARY torn tail — the bus refuses to boot after a
//     routine power loss.
//   - Scanning AFTER wal.Open reads the already-repaired file, so any record
//     repair removed is invisible. Reproduced: a floor dropping 5 -> 2, silently
//     re-minting suffixes 3, 4 and 5.
//
// A caller that insists on using this for anything load-bearing must therefore
// consult wal.Log.Recovered().Repaired (Quarantined, DiscardCount,
// DiscardedBytes) and refuse to proceed when it is non-empty. That is a
// requirement on the CALLER because this function cannot see the Recovered
// value; it is stated here because this is where someone will come looking.
//
// # DO NOT WIRE THIS AS THE PRODUCTION FLOOR SOURCE — read this first
//
// ids.DurableNameSuffixes (internal/ids/suffixstore.go, landed 2026-08-07 in
// commit 61b7c9a, AFTER this function was written) is the production allocator,
// and it does not derive a floor at all: it PERSISTS AND FSYNCS each name's
// floor BEFORE issuing the suffix, in its own `agent-suffixes` file. That is
// strictly stronger than anything derivable from the WAL, because no tail
// repair and no log quarantine can rewind a floor that was written ahead — the
// residual hole documented at the bottom of this comment is structural in a
// DERIVED floor and absent from a WRITTEN-AHEAD one.
//
// So a startup path must build ids.OpenNameSuffixes(dir), NOT
// EnrolmentSuffixesInWAL + ids.ResumeNameSuffixes. Wiring this function into startup
// would be a REGRESSION dressed as a fix. It is left here, and kept tested,
// because it answers a question the floor file cannot: what suffixes actually
// reached the WAL. A WAL whose highest suffix for a name EXCEEDS the floor file
// is a detectable integrity failure — a rewound or restored-from-backup floor
// file — and that cross-check is the only remaining use for this function.
//
// The rest of this comment documents the derivation on its own terms, because
// the cross-check has to be right for the same reasons the derivation did.
//
// # Why it scans PREPARES and not the replayed roster
//
// Point 3 of the ids.NameSuffixes doc is the paragraph this function exists to
// obey, and it warns that the OBVIOUS wiring is wrong: "Replay hands you
// COMMITTED state, so folding the committed roster gives you the highest suffix
// among agents that are CURRENTLY ENROLLED — and that is wrong twice over. It
// misses a suffix burned by a dangling prepare (allocated, prepare fsynced,
// crash before commit: the number is on disk and in the audit log, but no
// committed roster entry mentions it). And it misses every agent that has since
// LEFT."
//
// wal.Replay and wal.Applier expose COMMITTED entries only. A dangling prepare
// is, by construction, invisible to them — so deriving floors from them is
// EXACTLY the derivation ids forbids. The only way to see one is to scan the
// raw record stream, which is what this does: wal.ScanAll plus
// wal.DecodePrepare for every wal.TypePrepare record, ignoring commits and
// aborts entirely. An ABORTED prepare counts too: the abort record says the
// enrolment will never commit, it does not say the suffix was never written.
//
// The consequence of getting this wrong is not a duplicate message id. The
// fully-qualified agent id is the ROUTING and AUTHORIZATION subject: re-minting
// one hands a NEW agent, holding a DIFFERENT keypair, the exact identity a
// previous agent used.
//
// # The decoder here is DELIBERATELY LENIENT, and it will look like a bug
//
// The prepare body is read with a MINIMAL struct that pulls out only
// "agent_id". It is NOT the strict, validating Decode this package uses
// everywhere else, and the difference is intentional: a record whose
// certificate bindings are malformed, whose messaging key is the wrong length,
// or whose schema version this build does not understand STILL BURNED A SUFFIX.
// Refusing to see it is precisely how a floor ends up too low, and a floor too
// low re-mints a live agent id. Validation protects the ROSTER; the floor is a
// claim about BYTES THAT REACHED DISK, and those two questions have different
// right answers.
//
// Unknown fields are allowed for the same reason — a record written by a newer
// build, carrying a field this one has never heard of, must still raise the
// floor.
//
// # Failure is TOTAL: never a partial map
//
// Any failure returns (nil, err): a ScanAll error, a DecodePrepare error, or an
// agent-kind body with no parseable agent_id. Quoting the ids doc: "a
// derivation that got every floor it SAW right but MISSED A NAME ENTIRELY —
// a partial scan, a truncated replay, a replay that stopped at the first decode
// error — seals exactly as cleanly as a complete one, and every missed name
// then mints from 1 onto suffixes that are already on disk", and "a loud,
// recoverable outage beats silent identity reuse". The name SET is as
// load-bearing as the per-name maxima, so there is no such thing as a usefully
// incomplete answer here.
//
// A MISSING WAL FILE IS NOT A FAILURE. It returns an empty, non-nil map and a
// nil error — the honest "nothing on disk" claim a fresh bus makes, and the one
// the caller may legitimately seal. "Does not exist" is distinguished from
// every other error precisely so a permission denial or an I/O error can never
// be mistaken for an empty bus.
//
// The test is errors.Is(err, os.ErrNotExist) and NOT os.IsNotExist(err): the
// legacy predicate does not unwrap, and wal.ScanAll wraps its open error with
// %w, so os.IsNotExist reports FALSE on exactly the case this branch exists to
// catch — turning a fresh bus's first start into a refusal to boot.
//
// # Foreign bus ids are SKIPPED
//
// Every id is parsed with ids.ParseAgentID and its bus half compared with
// busID. A foreign id burned no LOCAL suffix — the suffix space is per bus per
// name — and a future peer-roster record legitimately carries one, so folding
// it in would inflate a local floor from a remote bus's history.
//
// # Known residual hole, not fixed here
//
// wal.Open's RepairLog may DISCARD or QUARANTINE records before this scan ever
// runs, and a discarded prepare's suffix becomes invisible, lowering the floor.
// That is tracked by existing backlog items — db350e39-3dde-4166-b241-b21fa4635359
// (whole-log quarantine reissues every sequence) and
// e120153b-9d8a-4b6a-bd4e-89431954496b (recovery reissuing a discarded tail
// index) — and is out of scope here. It is written down so it is known rather
// than discovered.
//
// # This is a SECOND full scan of the WAL, deliberately, for now
//
// wal.Open already walks the whole file to replay it; this walks it again to
// see the prepares that replay cannot expose. ID-2-WIRING-OBSERVER
// (c31f6999-da4e-400d-ab55-178b82e2a42e) adds wal.ReplayWithPrepares, which
// makes it one pass. This function is the interim shape on purpose: correctness
// first, and one extra sequential read at startup is a price worth paying to
// avoid reissuing agent ids in the meantime.
//
// EnrolmentSuffixesInWAL does NOT call ids.ResumeNameSuffixes or Seal. Returning the map
// is the whole job — construction and the seal belong to the startup path,
// which is the one place that can honestly assert the claim Seal makes.
func EnrolmentSuffixesInWAL(walPath string, busID string) (map[string]uint64, error) {
	floors := make(map[string]uint64)

	recs, _, err := wal.ScanAll(walPath, wal.KindWAL)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing on disk. An empty NON-NIL map, so the caller can tell
			// "derived, and it is empty" from a nil it might read as "not
			// derived".
			return floors, nil
		}
		return nil, fmt.Errorf("auth: deriving agent id suffix floors from %s: %w", walPath, err)
	}

	for _, rec := range recs {
		if rec.Type != wal.TypePrepare {
			continue
		}
		entry, _, err := wal.DecodePrepare(walPath, rec)
		if err != nil {
			return nil, fmt.Errorf("auth: deriving agent id suffix floors from %s: record %d does not decode, so the derivation is INCOMPLETE and must not be used: %w", walPath, rec.Index, err)
		}
		if entry.Kind != RecordKind {
			continue
		}

		agentID, err := scanAgentID(entry.Body)
		if err != nil {
			return nil, fmt.Errorf("auth: deriving agent id suffix floors from %s: enrolment record %d: %w", walPath, rec.Index, err)
		}
		bus, name, n, err := ids.ParseAgentID(agentID)
		if err != nil {
			return nil, fmt.Errorf("auth: deriving agent id suffix floors from %s: enrolment record %d carries an unparseable agent id: %w", walPath, rec.Index, err)
		}
		if bus != busID {
			// A foreign id burned no local suffix. Not an error: a peer-roster
			// record legitimately carries one.
			continue
		}
		if n > floors[name] {
			floors[name] = n
		}
	}
	return floors, nil
}

// floorScanJSON is the MINIMAL, LENIENT view of an enrolment record used only
// to derive suffix floors. It reads one field and ignores every other,
// INCLUDING the schema version — see the "deliberately lenient" section of
// EnrolmentSuffixesInWAL for why validating here would be the bug rather than the fix.
type floorScanJSON struct {
	AgentID string `json:"agent_id"`
}

// scanAgentID pulls the agent id out of an enrolment prepare body without
// validating anything else about it. An absent or empty agent_id IS an error:
// the record is an enrolment (its Kind said so) that names no id, so this scan
// cannot tell which name's floor it should have raised — which is a missed
// name, the one failure the derivation may never paper over.
func scanAgentID(body json.RawMessage) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("%w: the prepare body is empty, so no agent id can be read from it", ErrInvalidRecord)
	}
	var j floorScanJSON
	// No DisallowUnknownFields, on purpose: a record written by a newer build
	// carrying fields this one has never seen still burned a suffix.
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&j); err != nil {
		return "", fmt.Errorf("%w: the prepare body is not JSON this scan can read: %v", ErrInvalidRecord, err)
	}
	if j.AgentID == "" {
		return "", fmt.Errorf("%w: the prepare body carries no agent_id, so the name whose suffix it burned cannot be identified", ErrInvalidRecord)
	}
	return j.AgentID, nil
}
