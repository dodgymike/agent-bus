package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// ---------------------------------------------------------------------------
// DUR-5 -- the append-only message audit log (invariant 6).
//
// A SECOND FILE, distinct from the WAL, in the same data directory and under the
// same MAC key. Every message that is durably accepted is also written here, and
// the ONE thing that is never written here is the message BODY.
//
//	message id · sequence · sender · recipient(s) · bus path traversed ·
//	timestamp · size · content hash
//
// # Why the body is excluded, so nobody "improves" this later
//
// This is a decision (2026-08-02), not an omission. agent-bus is getting
// Signal-style end-to-end encryption with forward secrecy. An audit log holding
// PLAINTEXT becomes unwritable the moment PFS lands -- the bus will not have the
// plaintext. An audit log holding CIPHERTEXT it can never decrypt is dead weight
// that costs disk and leaks nothing but volume. So the audit trail is
// deliberately a ROUTING AND PROVENANCE record, and the content hash is what
// preserves the ability to prove WHAT was sent without retaining it.
//
// Any path that puts a body in this file is a DEFECT. There is a test that
// asserts the encoded payload of a record whose fields are stuffed with a
// recognisable body contains none of those bytes.
//
// # Why it is a superset of committed history, and never a subset
//
// The write order for a message is
//
//	prepare-fsync  ->  AUDIT-fsync  ->  commit-fsync  ->  apply
//
// so a crash in the window between the audit append and the commit append leaves
// an audit record for a message that never became accepted history. That
// direction is deliberate and is the safe one: the audit trail may over-report,
// never under-report. Reversing the order would let a message be acknowledged
// with no trace of it in the trail, which is the failure this file exists to
// prevent. The audit record carries the WAL prepare index precisely so that a
// later fsck can pair the two files and say which audit records correspond to
// entries that committed.
//
// # What this file does NOT get, and why
//
// NO DURABLE INDEX FLOOR. The floor (indexfloor.go) exists because the WAL's
// record index is the input from which internal/hub derives message SEQUENCES,
// so reissuing one reissues an id (invariant 1). An audit record's index is a
// position inside this file and nothing is ever derived from it, so there is no
// id to protect. Note the honest consequence: a quarantined audit log starts a
// fresh file at index 1, and audit indices are therefore NOT unique across the
// lifetime of a data directory. Use the message id or the sequence for identity.
//
// NO FORMAT VERSION 1 UPGRADE. A version 1 bus.audit is quarantined rather than
// converted, because converting it would re-sign unauthenticated records under
// this bus's MAC key. See openAuditLog.
//
// # What an I/O failure on this file does to the bus, stated plainly
//
// The audit writer poisons itself on a failed write or fsync, exactly as the WAL
// writer does (Writer.Append), and the poison LATCHES until the process
// restarts. So after a real disk failure on bus.audit the bus keeps enrolling
// agents, keeps serving polls and keeps writing roster, invite and session
// records -- and refuses EVERY MESSAGE, because a message that cannot be audited
// is not accepted (see ErrInvalidAudit's fail-closed reasoning; the same choice
// applies when the file is unwritable rather than the record unusable).
//
// That asymmetry is intended, not an accident of where the poison sits. Invariant
// 6 is about messages; refusing messages upholds it, and taking the whole bus
// down with them would lose the roster work that has nothing to do with the
// trail. Each refused message logs at ERROR through Txn.failBeforeCommit and
// abandons its transaction, so the condition is loud rather than a mystery.
//
// A poison that has ALREADY latched is answered in Begin, before a prepare is
// written, so a client retrying a doomed send costs nothing on disk. See there.
// ---------------------------------------------------------------------------

// AuditFileName is the name of the append-only message audit log inside the data
// directory. It is a sibling of WALFileName and shares the directory's
// wal-mac.key; its file magic ("AGNTBUSA") is what distinguishes the two on
// disk, so recovery refuses to read one as the other.
const AuditFileName = "bus.audit"

// ErrInvalidAudit reports an AuditRecord that is not fit to be written.
//
// It is detected in Begin, BEFORE the prepare record is appended, so a bad audit
// record leaves both files byte-for-byte unchanged and fails the caller's write
// outright. Failing closed is the point: a message that cannot be audited must
// not be accepted, because invariant 6 says every message is written to the
// audit log and a "mostly audited" trail is one nobody can rely on.
var ErrInvalidAudit = errors.New("wal: audit record is invalid")

// Bounds on the audit record's variable-length fields.
//
// These are FRAMING-LAYER SANITY BOUNDS, deliberately looser than the
// application's own limits (store.MaxRecipients is 64), so that this package
// never becomes the de-facto policy on how many recipients a message may have
// -- the application rejects first, and these only stop a caller turning one
// audit record into an unbounded write. A record that exceeds them is rejected
// with a specific error rather than surfacing as ErrPayloadTooLarge from three
// layers down, WITH A DURABLE PREPARE ALREADY ON DISK.
//
// # Why there is a TOTAL budget and not only per-field limits
//
// The per-field limits alone do not deliver the sentence above, and a reviewer
// measured that: they bound RAW bytes, but the payload is JSON, and
// encoding/json escapes a control byte or an invalid UTF-8 byte to six
// characters. Measured worst case is exactly 6x: a NUL byte encodes as the six
// characters backslash-u-0-0-0-0, and an invalid UTF-8 byte becomes a three-byte
// replacement character in a two-character escape. So
// maxAuditRecipients ids at maxAuditIDBytes each could encode to roughly 3 MB
// against a MaxPayloadSize of 1 MiB -- exactly the failure the comment claimed
// could not happen.
//
// maxAuditFieldBytes closes it arithmetically rather than by hope: the SUM of
// every variable-length field's raw bytes is capped, and 6 x that sum plus the
// fixed JSON structure is comfortably below MaxPayloadSize. It is far above
// anything a real message produces (64 recipients at 64-byte ids is 4 KB).
const (
	// maxAuditIDBytes bounds any single id-shaped string: message id, sender,
	// one recipient, one bus id in the path, the content hash.
	maxAuditIDBytes = 512
	// maxAuditRecipients bounds the recipient list of one directed message.
	maxAuditRecipients = 1024
	// maxAuditBusPath bounds the traversed bus path. Relay loop prevention keeps
	// this short in practice; this is the ceiling, not the expectation.
	maxAuditBusPath = 256
	// maxAuditFieldBytes bounds the SUM of every variable-length field, raw.
	// 128 KiB x 6 (worst-case JSON escaping) is 768 KiB, which leaves a quarter
	// of MaxPayloadSize for field names, numbers and punctuation.
	maxAuditFieldBytes = 128 << 10
)

// sha256HexLen is the length of a hex-encoded SHA-256 digest.
const sha256HexLen = 64

// AuditRecord is the metadata written to the append-only audit log for one
// message. It is set on Entry.Audit by the caller; a nil Entry.Audit means the
// entry is not a message and gets no audit record (roster, invite and session
// records go through the same WAL and are not part of the message trail).
//
// EVERY FIELD IS SUPPLIED BY THE CALLER EXCEPT THE PREPARE INDEX, which this
// package fills in from the transaction (invariant 1: the server is
// authoritative on ids, and the WAL index is minted here).
//
// THERE IS NO BODY FIELD AND THERE MUST NEVER BE ONE. See the file comment.
type AuditRecord struct {
	// MessageID is the fully-qualified, server-minted message id.
	MessageID string

	// Seq is the server-minted sequence number: the bus's total order and the
	// value a recipient's cursor points at.
	Seq uint64

	// Sender is the fully-qualified "<bus-id>.<agent-id>" of the authenticated
	// sender (invariant 2).
	Sender string

	// Broadcast reports a message addressed to the whole bus. A broadcast has no
	// recipient list: expanding one would freeze the roster as it stood at send
	// time into a record that is supposed to describe routing, not membership.
	Broadcast bool

	// Recipients holds the fully-qualified ids a directed message is addressed
	// to. It must be empty for a broadcast and non-empty otherwise.
	Recipients []string

	// BusPath is the ordered list of bus ids this message has traversed,
	// starting with the bus that accepted it. It is the loop-prevention and
	// provenance field invariant 6 names, and it must never be empty: a message
	// has always been accepted by at least one bus.
	BusPath []string

	// SentAt is when this bus accepted the message, by the SERVER's clock. It is
	// not the sender's claimed timestamp: the audit trail records what this bus
	// will swear to.
	SentAt time.Time

	// Size is the message body's length in bytes. It is the one quantitative
	// fact about the content the trail keeps.
	Size int64

	// ContentSHA256 is the hex SHA-256 that identifies the content, lowercase,
	// exactly 64 characters.
	//
	// PROTOCOL.md §8.6 is binding on WHAT is hashed: it is SHA-256 over the
	// CANONICAL SIGNING BYTES (internal/signing.CanonicalDigest), the same bytes
	// Ed25519 signs -- not over the bare body. Hashing the bare body would
	// fingerprint content while proving nothing about who sent it, to whom, or in
	// what order, and would decouple the audit record from the signature. This
	// package cannot check which of the two it has been handed (both are 64 hex
	// characters), so the requirement is stated here and enforced by the caller.
	ContentSHA256 string
}

// auditPayload is the on-disk JSON of a TypeAuditMessage record. Field names
// mirror store.Record so an operator reading either file sees the same words.
//
// THERE IS NO BODY FIELD. Adding one would violate invariant 6.
//
// FORWARD COMPATIBILITY. The CRYPTO epic will add an encrypted-envelope
// descriptor here, and it must be able to do so WITHOUT an on-disk format break.
// Two things make that work:
//
//   - New fields are ADDITIVE: an older binary reading a newer record IGNORES
//     fields it does not know (see decodeAuditPayload) rather than treating the
//     record as corrupt.
//   - Optional fields are omitempty, so a record written without them is
//     byte-identical to one written before the field existed.
//
// THE LENIENT DECODE IS SPECIFIC TO THIS FILE AND IS NOT A LOOSENING OF THE WAL.
// The WAL's decoders use DisallowUnknownFields on purpose: a WAL record that
// does not fully decode means the file no longer says what history was accepted,
// and serving state built by guessing is the way acknowledged writes get lost.
// An audit record is never replayed into serving state -- it is a read-only
// trail -- so the same argument does not apply, and refusing to read a trail
// written by a newer binary would be the worse failure. Nothing is lost either
// way: the raw payload is still on Record.Payload for a reader that wants all
// of it.
//
// WHAT THE TOLERANCE DOES NOT COVER, stated so it is known rather than
// discovered: DecodeAudit re-applies the WRITER's full validate() to the fields
// it does know. So additive fields are safe, but a future RELAXATION OF THE
// SHAPE -- a message id that may be empty, a bus path that may be absent, a
// second hash algorithm whose digest is not 64 lowercase hex characters -- would
// make this binary report a newer, perfectly good trail as ErrCorrupt. That is
// the deliberate trade: an fsck that silently hands back rows the writer would
// have refused is worse than one that says it cannot read them. A change that
// relaxes the shape must therefore version the payload, and versioning it needs
// a number RESERVED through the Spec Server, never chosen.
type auditPayload struct {
	MessageID     string   `json:"message_id"`
	Seq           uint64   `json:"seq"`
	Sender        string   `json:"sender"`
	Broadcast     bool     `json:"broadcast"`
	Recipients    []string `json:"recipients,omitempty"`
	BusPath       []string `json:"bus_path"`
	SentAt        string   `json:"sent_at"`
	Size          int64    `json:"size"`
	ContentSHA256 string   `json:"content_sha256"`
	// PrepareIndex is the WAL index of the PREPARE record of the transaction
	// this message was written in -- the transaction id. It is stamped by this
	// package, never by the caller, and it is what lets an fsck pair an audit
	// record with the WAL entry that (may have) committed it.
	PrepareIndex uint64 `json:"prepare_index"`
}

// validate rejects an audit record that is not fit to be written.
//
// It is called from Begin, before any I/O, so every failure here leaves both
// files untouched. The checks are structural only -- this package cannot know
// whether an id names a real agent -- but structure is enough to keep the trail
// machine-readable: a record with no message id or no content hash is a row
// nobody can join on.
func (a *AuditRecord) validate() error {
	if a == nil {
		return fmt.Errorf("%w: the record is nil", ErrInvalidAudit)
	}
	if err := auditID("message_id", a.MessageID); err != nil {
		return err
	}
	if a.Seq == 0 {
		return fmt.Errorf("%w: seq is 0, but sequences start at 1", ErrInvalidAudit)
	}
	if err := auditID("sender", a.Sender); err != nil {
		return err
	}
	// A message is EITHER a broadcast OR directed at a named list. Neither an
	// unaddressed directed message nor a broadcast carrying a frozen roster
	// snapshot is a thing this bus produces, and writing one into the trail
	// would make the routing record ambiguous.
	if a.Broadcast {
		if len(a.Recipients) != 0 {
			return fmt.Errorf("%w: a broadcast carries %d recipients, but a broadcast is addressed to the whole bus and must carry none",
				ErrInvalidAudit, len(a.Recipients))
		}
	} else {
		if len(a.Recipients) == 0 {
			return fmt.Errorf("%w: a directed message has no recipients", ErrInvalidAudit)
		}
		if len(a.Recipients) > maxAuditRecipients {
			return fmt.Errorf("%w: %d recipients, the limit is %d",
				ErrInvalidAudit, len(a.Recipients), maxAuditRecipients)
		}
		for i, r := range a.Recipients {
			if err := auditID(fmt.Sprintf("recipients[%d]", i), r); err != nil {
				return err
			}
		}
	}
	if len(a.BusPath) == 0 {
		return fmt.Errorf("%w: bus_path is empty, but a message has always been accepted by at least one bus", ErrInvalidAudit)
	}
	if len(a.BusPath) > maxAuditBusPath {
		return fmt.Errorf("%w: bus_path has %d entries, the limit is %d",
			ErrInvalidAudit, len(a.BusPath), maxAuditBusPath)
	}
	for i, b := range a.BusPath {
		if err := auditID(fmt.Sprintf("bus_path[%d]", i), b); err != nil {
			return err
		}
	}
	if a.SentAt.IsZero() {
		return fmt.Errorf("%w: sent_at is the zero time", ErrInvalidAudit)
	}
	if a.Size < 0 {
		return fmt.Errorf("%w: size is %d", ErrInvalidAudit, a.Size)
	}
	if !isLowerHex(a.ContentSHA256, sha256HexLen) {
		return fmt.Errorf("%w: content_sha256 %q is not %d lowercase hex characters",
			ErrInvalidAudit, elide(a.ContentSHA256, maxValueChars), sha256HexLen)
	}
	// THE TOTAL BUDGET, checked last because it is the only check that needs
	// every field to have passed its own. See maxAuditFieldBytes for why the
	// per-field limits do not imply it.
	total := len(a.MessageID) + len(a.Sender) + len(a.ContentSHA256)
	for _, r := range a.Recipients {
		total += len(r)
	}
	for _, b := range a.BusPath {
		total += len(b)
	}
	if total > maxAuditFieldBytes {
		return fmt.Errorf("%w: the variable-length fields total %d bytes, the limit is %d",
			ErrInvalidAudit, total, maxAuditFieldBytes)
	}
	return nil
}

// clone returns a DEEP copy: the struct, and fresh backing arrays for the two
// slices.
//
// It exists so that "validated" means something. Begin validates the record and
// Commit encodes it, and between those two points the Log's transaction lock is
// held but THE CALLER'S MEMORY IS NOT: a caller that reused its AuditRecord, or
// its Recipients slice, would have its later value written to the trail without
// ever being checked. Body and Idem are already canonicalised into fresh bytes
// at Begin for exactly this reason; this makes Audit behave the same way.
//
// It is nil-safe: a nil record clones to nil, which is how "this entry is not a
// message" travels.
func (a *AuditRecord) clone() *AuditRecord {
	if a == nil {
		return nil
	}
	c := *a
	if a.Recipients != nil {
		c.Recipients = append([]string(nil), a.Recipients...)
	}
	if a.BusPath != nil {
		c.BusPath = append([]string(nil), a.BusPath...)
	}
	return &c
}

// auditID validates one id-shaped field: non-empty and within the length bound.
// The value is quoted with %q and length-bounded, because it is client-derived
// and an error message is not a place to paste 64 KiB of chosen bytes.
func auditID(field, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidAudit, field)
	}
	if len(v) > maxAuditIDBytes {
		return fmt.Errorf("%w: %s is %d bytes, the limit is %d", ErrInvalidAudit, field, len(v), maxAuditIDBytes)
	}
	return nil
}

// isLowerHex reports whether s is exactly n lowercase hexadecimal characters.
//
// It is deliberately stricter than encoding/hex, which accepts uppercase: a hash
// that renders two ways is a hash two records can disagree about while naming
// the same content, and the audit trail is meant to be joined on.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// encodeAudit renders the on-disk payload for an audit record.
//
// prepareIndex is the WAL index of the transaction's PREPARE record. It is
// passed in rather than carried on AuditRecord so a caller cannot choose it: the
// server is authoritative on ids (invariant 1), and this one is minted by the
// WAL writer.
func encodeAudit(a *AuditRecord, prepareIndex uint64) ([]byte, error) {
	return encodeJSON(auditPayload{
		MessageID:     a.MessageID,
		Seq:           a.Seq,
		Sender:        a.Sender,
		Broadcast:     a.Broadcast,
		Recipients:    a.Recipients,
		BusPath:       a.BusPath,
		SentAt:        a.SentAt.UTC().Format(time.RFC3339Nano),
		Size:          a.Size,
		ContentSHA256: a.ContentSHA256,
		PrepareIndex:  prepareIndex,
	})
}

// DecodeAudit decodes a TypeAuditMessage record from the audit log, returning
// the metadata and the WAL prepare index the message was written under.
//
// It is the reader half of this file: an fsck, an operator tool, and the tests.
// Nothing in the serving path calls it -- the audit log is never replayed into
// memory, which is exactly why its decoder may be tolerant of fields it does not
// know while the WAL's may not (see auditPayload).
func DecodeAudit(path string, rec Record) (AuditRecord, uint64, error) {
	if rec.Type != TypeAuditMessage {
		return AuditRecord{}, 0, frameCorruptf(path, rec,
			"record %d is a %s record, want %s", rec.Index, rec.Type, TypeAuditMessage)
	}
	var p auditPayload
	// NOT DisallowUnknownFields: see auditPayload.
	dec := json.NewDecoder(bytes.NewReader(rec.Payload))
	if err := dec.Decode(&p); err != nil {
		e := frameCorruptf(path, rec, "record %d: %s payload does not decode", rec.Index, TypeAuditMessage)
		e.Err = err
		return AuditRecord{}, 0, e
	}
	if dec.More() {
		return AuditRecord{}, 0, frameCorruptf(path, rec,
			"record %d: %s payload has trailing data after the JSON object", rec.Index, TypeAuditMessage)
	}
	sentAt, err := time.Parse(time.RFC3339Nano, p.SentAt)
	if err != nil {
		e := frameCorruptf(path, rec, "record %d: audit payload sent_at %q is not RFC3339Nano",
			rec.Index, elide(p.SentAt, maxValueChars))
		e.Err = err
		return AuditRecord{}, 0, e
	}
	a := AuditRecord{
		MessageID:     p.MessageID,
		Seq:           p.Seq,
		Sender:        p.Sender,
		Broadcast:     p.Broadcast,
		Recipients:    p.Recipients,
		BusPath:       p.BusPath,
		SentAt:        sentAt,
		Size:          p.Size,
		ContentSHA256: p.ContentSHA256,
	}
	// The SAME validation the writer applied, applied on the way out. A record
	// that would never have been written is damage however it got there, and
	// this package refuses to hand a caller a row it would have rejected.
	if err := a.validate(); err != nil {
		e := frameCorruptf(path, rec, "record %d: %s", rec.Index, err)
		e.Err = err
		return AuditRecord{}, 0, e
	}
	return a, p.PrepareIndex, nil
}

// openAuditLog prepares and opens the append-only audit log in dir.
//
// It is the audit-file half of Open's recovery and it follows the same rules,
// for the same reasons: DAMAGE IS NEVER FATAL and every discard is LOGGED, but
// being unable to READ the file at all still refuses the start. A bus that will
// not boot because one sector of its audit trail is bad is worse than a bus that
// has lost an audit record and said so -- and a WAL sitting where the audit log
// should be is not damage, it is a data directory that is not what we think it
// is, so detectFormat's fatal "file is a wal file, want a audit file" is left
// fatal.
//
// c is the WAL's already-resolved codec. Reusing it is correct and deliberate:
// the MAC key is per DATA DIRECTORY, not per file, so the audit log is
// authenticated by the same key and recovery loads it once.
//
// # THERE IS NO FORMAT VERSION 1 UPGRADE HERE, AND THAT IS A SECURITY DECISION
//
// An earlier draft mirrored Open's WAL path: detect version 1, repair it with a
// version 1 codec, then upgradeV1 it to version 2. It was removed after the
// security gate PROVED what it buys an attacker.
//
// A version 1 frame is authenticated by CRC32C, which is UNKEYED: anyone can
// compute one. So an attacker who can write this data directory but does NOT
// hold wal-mac.key could plant a version 1 bus.audit full of records they wrote
// themselves, and the upgrade would obligingly RE-SIGN every one of them under
// the server's key. The gate's probe got `message_id="FORGED-BY-ATTACKER"` back
// verifying under the real key. That is a laundering path, and it exists only
// because the upgrade was wired up for a case that cannot occur: audit records
// have only ever been written at format version 2, so a version 1 bus.audit is
// never a real bus's file.
//
// What happens instead is a QUARANTINE, taken here and explicitly. It has to be
// explicit: repairLog will not touch a file whose header positively identifies a
// layout it does not implement -- checkSalvageHeader returns "format version 1,
// want 2 ... recovery will not guess at it", which is a FATAL error, and a bus
// that will not boot because of a file an attacker planted is a denial of service
// with extra steps. So the file is RENAMED ASIDE (never deleted -- an operator is
// owed the bytes), the reason is logged at ERROR, and the bus starts with a fresh
// trail. Availability is preserved, the evidence is preserved, and nothing an
// attacker wrote is ever given this server's signature.
//
// Note the WAL's own version 1 branch (log.go) has the same shape and the same
// hazard, and it is worse there because forged entries reach serving state. It
// is PRE-EXISTING and out of scope for this task. It is tracked as
// DUR-12-FU-V1LAUNDER, which the security gate re-confirmed LIVE by planting a
// CRC32C-forged v1 bus.wal and watching the forged record reach SERVING STATE,
// verifying afterwards under the real key. Mirror this file's answer there:
// QUARANTINE, do not try to gate the upgrade.
func openAuditLog(dir string, c codec, walRecords uint64, logger *logging.Logger) (*Writer, Repair, error) {
	path := filepath.Join(dir, AuditFileName)

	// ASKED BEFORE ANYTHING TOUCHES THE FILE, because openWriter CREATES it and
	// the answer would be gone a few lines later. See the loss check below.
	sizeAtOpen, err := logSize(path)
	if err != nil {
		return nil, Repair{Path: path}, err
	}

	// THE VERSION 1 QUARANTINE. See the block comment above for why this file
	// kind gets a quarantine where the WAL gets an upgrade.
	version, err := detectFormat(path, KindAudit)
	if err != nil {
		return nil, Repair{Path: path}, err // names the path; a WAL here is still fatal
	}
	var v1Quarantine string
	if version == formatVersionV1 {
		moved, qerr := quarantine(path)
		if qerr != nil {
			return nil, Repair{Path: path}, qerr
		}
		v1Quarantine = moved
		logger.Error("wal quarantined an append-only message audit log written in on-disk format version 1 and started a fresh one",
			"path", path, "moved_to", moved,
			"why", "audit records have only ever been written at format version 2, so no bus this code has ever run produced this file. Version 1 frames are authenticated by an UNKEYED CRC32C, which anyone able to write this directory can compute -- upgrading the file would RE-SIGN every record in it under this bus's MAC key and launder content nobody authenticated. The file is renamed aside, not deleted; if it is genuinely yours, it was not written by agent-bus")
	}

	repair, err := repairLog(path, KindAudit, c, logger)
	if err != nil {
		return nil, repair, err
	}
	if v1Quarantine != "" {
		// repairLog saw no file (the quarantine took it) and reported a zero
		// Repair. Report what actually happened instead, IN THE SAME SHAPE
		// repairLog's own quarantine path uses (recover.go), because a caller
		// reading Recovered.AuditRepaired must not have to know which of the two
		// code paths produced it:
		//
		//	NextIndex 1        the honest answer to "where does THIS FILE start"
		//	                   -- the old file is renamed away and the one that
		//	                   replaces it is empty. It is NOT an index the bus
		//	                   resumes at; nothing is derived from audit indices.
		//	LostUnidentified   the quarantined bytes could have held any number of
		//	                   records, none of which can now be enumerated.
		//	DiscardCount /     an exact count and byte total are emitted on every
		//	DiscardedBytes     repair path without exception. Zeroes here would
		//	                   have printed "discarded_records=0" beside a
		//	                   quarantined path, which reads as "nothing was lost".
		const why = "the audit log was in on-disk format version 1, which no bus ever wrote; it was quarantined rather than upgraded, because upgrading re-signs unauthenticated records under this bus's MAC key"
		repair.Quarantined = v1Quarantine
		repair.LostUnidentified = true
		repair.NextIndex = 1
		repair.Reason = why
		repair.DiscardCount = 1
		repair.DiscardedBytes = sizeAtOpen
		repair.Discards = []Discard{{Stage: "framing", Offset: 0, Length: sizeAtOpen, Reason: why}}
	}

	// AN ABSENT OR EMPTY AUDIT LOG BESIDE A NON-EMPTY WAL IS ANNOUNCED, LOUDLY.
	//
	// It is the one loss this file cannot report as a discard, and the security
	// gate caught it: delete bus.audit outright and recovery is entirely silent
	// -- repairLog has nothing to repair, Repair comes back all zeroes, and the
	// trail simply restarts at index 1. Invariant 6's "every discard is LOGGED"
	// is about damage; a file that is not there is not damage, so nothing fired.
	// Silence is exactly the defect that invariant rates P0, so it is said here.
	//
	// IT CANNOT TELL THE TWO CAUSES APART, and does not pretend to: this is
	// either a data directory written before the audit log existed (in which case
	// it is a one-time, expected line on the first start after the upgrade), or
	// the file was deleted, lost with its media, or restored from a backup that
	// predates it. Both need the operator to know; only they can say which.
	//
	// WHAT IT DOES NOT CATCH, stated so nobody mistakes silence for health. The
	// predicate is size == 0, so a trail TRUNCATED TO EXACTLY ITS FILE HEADER --
	// 48 bytes, every record erased, which is a perfectly well-framed file --
	// passes in total silence. That is a known limitation of the heuristic, not
	// an oversight: `<= FileHeaderSize` would fire on every restart of a bus that
	// has simply never sent a message, and a loss channel that cries wolf is the
	// mirror image of the silent-discard defect. Closing it properly needs the
	// durable high-water mark below, which is the follow-up.
	//
	// The durable fix -- an audit high-water mark that survives the file, so
	// recovery can say HOW MANY records are missing rather than that some are,
	// and can see a header-only file as the loss it is -- needs durable state of
	// its own, and is a follow-up rather than something this task quietly grew.
	// Its ticket also owns two gaps this line does not close: a trail truncated to
	// exactly its file header, and a data directory that has lost BOTH files (the
	// walRecords > 0 guard cannot fire when the WAL is gone too).
	if sizeAtOpen == 0 && walRecords > 0 && repair.Quarantined == "" {
		logger.Error("wal found no append-only message audit log beside a write-ahead log that holds records",
			"path", path, "wal_records", walRecords,
			"why", "either this data directory predates the audit log (expected once, on the first start after the upgrade) or the trail was deleted, lost with its media, or restored from a backup that predates it. Recovery cannot tell those apart",
			"effect", "IF any of those WAL records is a message, it has no provenance record and cannot be evidenced from this bus. This layer cannot tell messages from roster, invite and session records -- they share the WAL and wal does not interpret an entry's kind -- so wal_records is a count of ALL records, and a directory that has only ever enrolled agents has lost nothing")
	}

	// repairLog has already logged each discard individually. This line is the
	// one that says WHICH TRAIL lost them: an operator reading "wal discarded a
	// damaged record" needs to know that the provenance record for some messages
	// is gone, which is a different fact from the WAL losing a record and is not
	// something a path in a key-value pair reliably conveys on its own.
	if repair.DiscardCount > 0 || repair.Truncated || repair.Rewritten || repair.Quarantined != "" {
		logger.Error("wal repaired the append-only message audit log; provenance records were lost",
			"path", path, "discarded_records", repair.DiscardCount, "discarded_bytes", repair.DiscardedBytes,
			"truncated", repair.Truncated, "rewritten", repair.Rewritten, "quarantined", repair.Quarantined,
			"why", "the audit trail is the record of what this bus routed (invariant 6). Damage in it is discarded so the bus can start, but the messages those records described can no longer be evidenced from this file. The WAL is a separate file and is repaired separately")
	}

	w, err := openWriter(path, KindAudit, c)
	if err != nil {
		return nil, repair, err
	}
	return w, repair, nil
}
