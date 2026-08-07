package main

// Startup construction of the PRODUCTION per-name agent-id suffix allocator
// (MSG-FU-SUFFIXFLOOR).
//
// The allocator itself is ids.DurableNameSuffixes: it persists and fsyncs each
// name's floor BEFORE it issues the suffix that floor authorises, so no WAL tail
// repair and no log quarantine can rewind it. Everything in this file is the
// startup half of that: open the floor file, backfill a data dir that predates
// it, and seal.
//
// # Why the backfill exists at all
//
// A data dir written by a binary that predates the floors file has agent ids on
// disk and NO agent-suffixes file. ids.OpenNameSuffixes reports Existed() ==
// false there, and sealing the empty floor map it yields would assert "no suffix
// was ever written for any name" — the exact false claim that re-mints ids that
// are already durable. So on that one path the floors must be DERIVED from the
// log and folded in through RaiseFloor before the seal.
//
// The window closes on its own: the first start of THIS binary against a dir
// writes the floor file, and every start after that reads it instead. The
// backfill is a migration, not a permanent mechanism.
//
// # Where the derived floors come from, and what they do NOT cover
//
// The population is the SENDER and the RECIPIENTS of store message records
// (store.RecordKind). Those are fully-qualified agent ids (invariant 2), they
// are server-derived, and the WAL never compacts, so they are the ids that
// really are durable on a legacy dir.
//
// It is a LOWER BOUND, not a complete floor, and three holes are known:
//
//   - A BROADCAST stores a flag instead of a recipient list (store.Message.
//     Broadcast), deliberately. An agent that only ever RECEIVED broadcasts
//     therefore leaves no trace in any record and its suffix is invisible here.
//   - wal.Open's recovery may have DISCARDED or QUARANTINED records before this
//     scan runs, taking their ids with them. See suffixBackfillExposure below:
//     that is detected and logged at ERROR, and the bus still starts.
//   - A suffix burned by a PREPARE that this scan can see is counted, but one
//     burned by an allocation that never reached any record at all is not — and
//     never could be, because nothing wrote it down. (That case is harmless:
//     point 2 of the ids.NameSuffixes doc — a suffix that never reached disk may
//     safely be reissued.)
//
// Only the first two are real gaps, and both are structural to a DERIVED floor.
// They are exactly what the WRITTEN-AHEAD floor file removes, which is why this
// code runs once per data dir and then never again.
//
// # What is deliberately NOT used
//
// auth.EnrolmentSuffixesInWAL is NOT called as the floor source, and its own doc
// comment says why at length: it scans only enrolment records, and on every dir
// the shipped binary has written that set is EMPTY while the message records are
// the entire population. Sealing its result is indistinguishable from sealing an
// empty map on a bus with history.
//
// A GENERIC walk over record bodies, folding every string that happens to parse
// as an agent id, is also deliberately avoided. It looks strictly safer — floors
// only go up — but the idempotency key is CLIENT-SUPPLIED and durable, so a
// client could set it to "<bus>.alpha-18446744073709551615" and permanently
// exhaust the name "alpha" for this bus. Only server-derived fields are read
// here, and that is why.
//
// # A PRECONDITION ON RELAY, recorded here because this is where it bites
//
// "Server-derived" is true of `recipients` TODAY because hub.publish requires
// every recipient to be enrolled on this bus before the durable write. The relay
// path (internal/relay, currently unwired) validates recipient SHAPE only. The
// moment relay is served, a hostile peer could relay a message naming the local
// recipient "<local-bus>.alpha-18446744073709551615", and this derivation would
// fold it into a floor and exhaust that name permanently, across restarts.
//
// So: RELAY MUST roster-check local recipients before the durable write. Found
// by the security gate on this task (LOW, latent only because relay is unwired);
// it is stated here as well as filed, because this file is what turns a bad
// recipient into a permanent one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// openSuffixAllocator builds the sealed, durable per-name suffix allocator for
// dataDir, backfilling floors from walLog when the floors file does not yet
// exist.
//
// EVERY failure path returns an error, and the caller must treat it as FATAL.
// There is NO fallback to ids.NewNameSuffixes() here or anywhere else in cmd/,
// and adding one would silently restore the defect this function exists to fix:
// a fresh counter mints "-1" for every name, over ids that are already durable
// in the log. A loud, recoverable outage beats silent identity reuse (point 7 of
// the ids.NameSuffixes doc).
//
// walLog must already be OPEN, and that ordering is forced, not incidental — see
// walAgentIDFloors for the trade it makes. It is only READ when there is no
// floors file; see the comment on the backfill branch for why that condition is
// exactly right and neither narrower nor wider.
//
// dataDirWasEmpty must be the state of dataDir at the very start of the process,
// before anything wrote to it. It is only used to pick the LOG LEVEL of the seal
// line; nothing about the floors themselves depends on it.
func openSuffixAllocator(dataDir string, walLog *wal.Log, busID string, dataDirWasEmpty bool, lg *logging.Logger) (*ids.DurableNameSuffixes, error) {
	alloc, err := ids.OpenNameSuffixes(dataDir)
	if err != nil {
		// Includes ErrSuffixFileCorrupt, which is never recovered by
		// regenerating the file: regenerating means resuming every name from 1.
		return nil, fmt.Errorf("opening the persisted agent-id suffix floors: %w", err)
	}

	// The WAL is scanned ONLY when there is no floors file, and that condition is
	// load-bearing in BOTH directions.
	//
	// It must not be narrower: an ABSENT floors file on a dir that holds agent
	// ids is the whole backfill case, and it is also what a deleted or
	// never-written floors file looks like, so this is where that is caught.
	//
	// It must not be WIDER either, and the reviewer gate reproduced why. An
	// every-start scan would let the bus cross-check the persisted floors against
	// the log and catch a floors file that had been REWOUND (restored from an
	// older backup) rather than deleted. That check is genuinely worth having and
	// it is NOT worth this price: wal.ScanAll accumulates every Record, payload
	// included, in memory (internal/wal/reader.go), the WAL never compacts, and
	// internal/wal already has a measured incident on record where a per-record
	// INDEX LIST — far smaller than the payloads — cost 1.76 MB on a 23.7 MB log
	// and was called "the boot-time OOM the eviction was written to avoid" at
	// 10 GiB. Paying that on every start, forever, to detect a rare operator
	// error would be trading a certain availability regression for a possible
	// one. wal.Replay is streaming; the raw scan is not, because scanFrom is
	// unexported. Reinstating the every-start cross-check behind a streaming raw
	// scan is filed as a follow-up.
	//
	// What is therefore NOT detected today, stated plainly rather than implied by
	// omission: a floors file that EXISTS but has been rewound to an older
	// version. A DELETED one is caught (Existed() == false sends us here); a
	// CORRUPTED one is caught by ids.ErrSuffixFileCorrupt; a rewound-but-valid
	// one is not.
	backfilling := !alloc.Existed()
	raised := 0

	if backfilling {
		derived, err := walAgentIDFloors(walLog.Path(), busID)
		if err != nil {
			// No floors file AND no derivation: nothing can prove what suffixes
			// this data dir has already burned. Sealing here would assert an
			// empty map. REFUSE.
			return nil, fmt.Errorf("this data directory has no %s file, so the agent-id suffix floors must be derived from the write-ahead log before any id is minted, and that derivation FAILED: starting anyway would resume every agent name from suffix 1 and re-mint agent ids that are already durable in the log (invariant 1). Fix or move the log and restart: %w", alloc.Path(), err)
		}
		for name, n := range derived {
			if err := alloc.RaiseFloor(name, n); err != nil {
				return nil, fmt.Errorf("raising the agent-id suffix floor for %q to %d: %w", name, n, err)
			}
			raised++
		}

		// The scan COMPLETED, but recovery may have removed records before it
		// ran — so it is a lower bound on a lower bound. Boot anyway (see
		// DECISIONS.md 2026-08-07: the bus always restarts), but name the
		// exposure at ERROR rather than let it pass as a normal start.
		if rec := walLog.Recovered(); suffixBackfillExposure(rec) {
			lg.Error("agent-id suffix floors were backfilled from a log that recovery had already REPAIRED, so any agent id inside a discarded record is invisible to the backfill and its name could be re-minted; this is a one-time migration exposure on a data directory that predates the persisted floors file",
				"path", alloc.Path(),
				"wal", walLog.Path(),
				"repaired", rec.Repaired.Truncated,
				"rewritten", rec.Repaired.Rewritten,
				"quarantined", rec.Repaired.Quarantined,
				"lost_unidentified", rec.Repaired.LostUnidentified,
				"discard_count", rec.Repaired.DiscardCount,
				"discarded_bytes", rec.Repaired.DiscardedBytes,
			)
		}
	}

	// ONCE, and CHECKED. A failed Seal leaves the allocator unsealed, so every
	// NextSuffix would refuse with ErrFloorUnproven and every enrolment would
	// fail — a bus that looks up but cannot enrol. Fail startup instead.
	if err := alloc.Seal(); err != nil {
		return nil, fmt.Errorf("sealing the agent-id suffix floors in %s: %w", alloc.Path(), err)
	}

	// The LEVEL is part of the contract here, not decoration.
	//
	// The security gate reproduced the case this exists for: delete ONLY
	// agent-suffixes from a live data dir, leave bus-id and the log intact, and
	// the bus re-mints an id it has already issued — because an id that was
	// issued but never reached a durable record leaves nothing for the backfill
	// to find. The floors FILE was the only thing that knew, and it is gone. That
	// is a loss of identity continuity, and it went out at INFO, indistinguishable
	// from an ordinary start.
	//
	// It cannot be a refusal to boot: a legacy dir looks identical, so refusing
	// would block the very migration this code exists to perform, and it would
	// hand anyone with data-dir write access a permanent boot-denial primitive.
	// So it is LOUD instead, and graded by whether this dir has history:
	//
	//   - floors file present -> INFO. The steady state; nothing was derived.
	//   - absent, and the data dir was EMPTY when the process started -> WARN.
	//     This is a genuinely first start. One line, once per dir.
	//   - absent, and the dir was NOT empty (or the log holds records) -> ERROR.
	//     This dir HAS history and its floors file is missing, so any id it
	//     issued that no durable record names can be re-minted on this start.
	//
	// The discriminator is DIRECTORY EMPTINESS and not the record count, and that
	// correction came from running it: with enrolment still memory-only, a bus
	// can issue alpha-1 and alpha-2 and leave a COMPLETELY EMPTY log, so
	// "Records == 0" reported a lost floors file as a routine first start —
	// which is precisely the silence this grading exists to remove. The record
	// count is kept as a second trigger because it stays correct once enrolment
	// is durable, and because a non-empty log is history by any definition.
	fields := []interface{}{
		"path", alloc.Path(),
		"existed", alloc.Existed(),
		"backfilled", backfilling,
		"names", len(alloc.Floors()),
		"floors_raised", raised,
		"records_in_log", walLog.Recovered().Records,
		"data_dir_was_empty", dataDirWasEmpty,
	}
	switch {
	case !backfilling:
		lg.Info("agent-id suffix floors sealed", fields...)
	case dataDirWasEmpty && walLog.Recovered().Records == 0:
		lg.Warn("agent-id suffix floors sealed: this data directory was EMPTY at startup and had no persisted floors file, so this is a first start and every agent name begins at suffix 1. Expected exactly once per data directory; if you see it again, the floors file is being lost between starts", fields...)
	default:
		lg.Error("agent-id suffix floors sealed WITHOUT a persisted floors file on a data directory that HAS history: the floors were backfilled from the durable log, so any agent id this bus issued that no durable record names can be RE-MINTED on this start, handing a new keypair a previous holder's identity (invariant 1). This is expected exactly once, when migrating a data directory written before the floors file existed; on any other start it means the floors file was deleted, and the data directory should be investigated", fields...)
	}
	return alloc, nil
}

// dirIsEmpty reports whether dir contains no entries at all.
//
// It reads ONE entry rather than the whole directory: the answer is decided by
// the first, and a data dir can legitimately hold a large number of files.
func dirIsEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(names) == 0, nil
}

// suffixBackfillExposure reports whether recovery removed anything from the log
// before the backfill scan could read it. Every field here means "bytes this
// file no longer carries", and any of them can hide an agent id.
func suffixBackfillExposure(rec wal.Recovered) bool {
	r := rec.Repaired
	return r.Truncated ||
		r.Rewritten ||
		r.Quarantined != "" ||
		r.LostUnidentified ||
		r.Exhausted ||
		r.DiscardCount > 0
}

// walAgentIDFloors derives, per agent name, the highest suffix of any LOCAL
// agent id appearing as the sender or a recipient of a message record in
// walPath.
//
// # Failure is TOTAL: never a partial map
//
// Any failure returns (nil, err). A derivation that got every floor it SAW right
// but MISSED A NAME ENTIRELY seals exactly as cleanly as a complete one, and
// every missed name then mints from 1 onto suffixes that are already on disk
// (the "limit of the seal" section of the ids.NameSuffixes doc). The name SET is
// as load-bearing as the per-name maxima, so there is no such thing as a usefully
// incomplete answer.
//
// A MISSING LOG IS NOT A FAILURE: it returns an empty, non-nil map and a nil
// error, the honest "nothing on disk" claim a fresh bus makes. The test is
// errors.Is(err, os.ErrNotExist) and NOT os.IsNotExist, because wal.ScanAll
// wraps its open error with %w and the legacy predicate does not unwrap — so
// os.IsNotExist would report false on exactly the case this branch exists to
// catch, turning a fresh bus's first start into a refusal to boot.
//
// # The ordering trade, stated rather than hidden
//
// This runs AFTER wal.Open, and the alternative is worse rather than better:
//
//   - BEFORE wal.Open the file is UNREPAIRED, so wal.ScanAll's strict framing
//     fails on an ORDINARY torn tail — a routine power loss would then make the
//     derivation fail, and a failed derivation on a legacy dir refuses to boot.
//     That trades a real availability loss for a hypothetical one.
//   - AFTER wal.Open the file has been repaired, so anything recovery removed is
//     invisible and a floor can come out too low (reproduced in
//     internal/auth/floors.go's doc: 5 -> 2).
//
// There is no clean fix at this layer. openSuffixAllocator therefore consults
// wal.Log.Recovered() and logs the exposure at ERROR when recovery removed
// anything, and DECISIONS.md (2026-08-07) records why the bus boots anyway.
//
// # The decode is DELIBERATELY LENIENT, and it will look like a bug
//
// The body is read with a minimal struct pulling out only "sender" and
// "recipients", not store.Decode. That is intentional: a record whose content
// hash does not match, whose schema version this build does not understand, or
// whose body is a size this build rejects STILL BURNED A SUFFIX. Refusing to see
// it is precisely how a floor ends up too low. Validation protects the SERVING
// COPY; a floor is a claim about BYTES THAT REACHED DISK, and those two
// questions have different right answers.
func walAgentIDFloors(walPath, busID string) (map[string]uint64, error) {
	floors := make(map[string]uint64)

	recs, _, err := wal.ScanAll(walPath, wal.KindWAL)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return floors, nil
		}
		return nil, fmt.Errorf("deriving agent-id suffix floors from %s: %w", walPath, err)
	}

	for _, rec := range recs {
		// PREPARES only. A commit or abort carries no body, and an ABORTED
		// prepare counts just as much as a committed one: the abort says the
		// operation will never take effect, it does not say the suffix was never
		// written.
		if rec.Type != wal.TypePrepare {
			continue
		}
		entry, _, err := wal.DecodePrepare(walPath, rec)
		if err != nil {
			return nil, fmt.Errorf("deriving agent-id suffix floors from %s: record %d does not decode, so the derivation is INCOMPLETE and must not be used: %w", walPath, rec.Index, err)
		}
		if entry.Kind != store.RecordKind {
			continue
		}

		agentIDs, err := scanMessageAgentIDs(entry.Body)
		if err != nil {
			return nil, fmt.Errorf("deriving agent-id suffix floors from %s: message record %d: %w", walPath, rec.Index, err)
		}
		for _, id := range agentIDs {
			bus, name, n, err := ids.ParseAgentID(id)
			if err != nil {
				return nil, fmt.Errorf("deriving agent-id suffix floors from %s: message record %d carries an unparseable agent id: %w", walPath, rec.Index, err)
			}
			if bus != busID {
				// A foreign id burned no LOCAL suffix — the suffix space is per
				// bus per name — and a relayed message legitimately carries one.
				continue
			}
			if n > floors[name] {
				floors[name] = n
			}
		}
	}
	return floors, nil
}

// messageAgentIDsJSON is the MINIMAL, LENIENT view of a message record used only
// to derive suffix floors. Both fields are SERVER-DERIVED — the sender is the
// authenticated principal and the recipients were validated at send — which is
// what makes them safe to fold into a floor. No other field is read; see the
// file header for why a generic walk over client-supplied strings would be a
// denial-of-service vector rather than extra safety.
//
// One property of encoding/json that is load-bearing and easy to forget: field
// matching is CASE-INSENSITIVE and LAST-KEY-WINS, so a body carrying both
// "sender" and "SENDER" decodes to the second. That is harmless here only
// because store.Record has no client-controlled top-level key — the message body
// is base64 inside "body" and can never introduce one — and it is written down
// so that a future field addition does not quietly make it exploitable.
type messageAgentIDsJSON struct {
	Sender     string   `json:"sender"`
	Recipients []string `json:"recipients"`
}

// scanMessageAgentIDs pulls the agent ids out of a message record body without
// validating anything else about it.
//
// An absent or empty SENDER is an error: the record is a message (its Kind said
// so) that names no sender, so this scan cannot tell which name's suffix it
// burned — a missed name, the one failure the derivation may never paper over.
// An empty RECIPIENT LIST is not an error: a broadcast legitimately has none.
func scanMessageAgentIDs(body json.RawMessage) ([]string, error) {
	if len(body) == 0 {
		return nil, errors.New("the prepare body is empty, so no agent id can be read from it")
	}
	var j messageAgentIDsJSON
	// No DisallowUnknownFields, on purpose: a record written by a newer build
	// carrying fields this one has never seen still burned a suffix.
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("the prepare body is not JSON this scan can read: %w", err)
	}
	if j.Sender == "" {
		return nil, errors.New("the message record carries no sender, so the name whose suffix it burned cannot be identified")
	}
	out := make([]string, 0, 1+len(j.Recipients))
	out = append(out, j.Sender)
	for _, r := range j.Recipients {
		if r == "" {
			return nil, errors.New("the message record carries an empty recipient, so the name whose suffix it burned cannot be identified")
		}
		out = append(out, r)
	}
	return out, nil
}
