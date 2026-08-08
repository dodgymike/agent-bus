package hub

import (
	"encoding/hex"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ---------------------------------------------------------------------------
// The PRODUCER half of DUR-5 -- the hub's side of the append-only message audit
// log (invariant 6).
//
// internal/wal owns the record's format, its validation and its placement in
// the two-phase write (prepare-fsync -> AUDIT-fsync -> commit-fsync). This file
// owns the ONE question that package cannot answer: what the fields actually
// are for the message being written. It exists as its own file, and not as six
// lines inline in publish, because the content hash below is a correctness trap
// that is invisible at the call site (see auditContentHash).
//
// # Why this is fail-closed, and why that is not a choice made here
//
// wal.Begin validates the record BEFORE the prepare record is appended, so a
// record this function gets wrong fails the send with NOTHING written -- and a
// message that cannot be audited is not accepted. That is wal's ErrInvalidAudit
// reasoning and this file inherits it rather than restating it as policy.
//
// The DEFECT this file was written to fix is the other half of that bargain
// going missing: publish passed a zero-valued wal.AuditRecord as a placeholder,
// so once wal's validation landed, EVERY send returned HTTP 500 with
// "audit record is invalid: message_id is empty". The message id was never
// empty -- the RECORD was. Anyone reading only the error audits the id
// allocator, which is the wrong layer entirely; that is why the error this file
// returns names the message it was building the record FOR.
// ---------------------------------------------------------------------------

// auditRecordFor builds the audit-log record for a message that is about to be
// written durably.
//
// Every field invariant 6 names is populated from the message itself -- message
// id, sequence, sender, recipient(s), bus path, timestamp, size, content hash --
// and the message BODY is deliberately absent. That exclusion is a decision
// (2026-08-02), not an oversight: the audit trail must stay writable once
// payloads are end-to-end encrypted and the bus no longer holds the plaintext.
// wal.AuditRecord has no body field, so adding one here is not merely
// discouraged, it does not compile -- keep it that way.
//
// The prepare index is NOT set here. wal stamps it from the transaction, so
// that the one field tying an audit record to a WAL entry cannot be chosen by
// the caller (invariant 1).
func auditRecordFor(m store.Message) (*wal.AuditRecord, error) {
	hash, err := auditContentHash(m)
	if err != nil {
		return nil, err
	}
	return &wal.AuditRecord{
		MessageID: m.ID,
		Seq:       m.Seq,
		Sender:    m.Sender,
		Broadcast: m.Broadcast,
		// Carried, not expanded. store.Message keeps a broadcast as a FLAG with
		// an empty recipient list, and wal.AuditRecord.validate REQUIRES that
		// shape: a broadcast that arrived here carrying a roster snapshot would
		// be freezing membership into a record that describes ROUTING.
		Recipients: m.Recipients,
		BusPath:    m.BusPath,
		// The SERVER's clock (store.Message.SentAt), never the sender's claimed
		// TimestampUnixMilli. The trail records what this bus will swear to; the
		// sender's clock is covered by the signature and is the sender's claim,
		// which is a different fact and is not what an audit answers.
		SentAt:        m.SentAt,
		Size:          int64(len(m.Body)),
		ContentSHA256: hash,
	}, nil
}

// auditContentHash returns the audit record's content hash: the hex SHA-256
// over the CANONICAL SIGNING BYTES of the message.
//
// # READ THIS BEFORE "simplifying" it to m.ContentSHA256
//
// store.Message already carries a field called ContentSHA256, it is already a
// 64-character lowercase hex SHA-256, and assigning it here compiles, passes
// every test in this repository, and is WRONG.
//
// The two hashes cover different things:
//
//	store.ContentHash(body)  -- SHA-256 of the BARE BODY
//	signing.CanonicalDigest  -- SHA-256 of the canonical bytes: the body
//	                            length-prefixed together with the message id,
//	                            sequence, sender, recipient set and the sender's
//	                            timestamp
//
// PROTOCOL.md 8.6 is binding and names the second one. The bare-body hash
// fingerprints CONTENT while proving nothing about who sent it, to whom, or in
// what order, and it decouples the audit record from the signature -- the
// canonical bytes are the exact bytes Ed25519 signed, so hashing them is what
// lets the trail prove delivery, ordering and authorship without ever holding
// the content.
//
// Nothing downstream can catch the mistake. Both values are 64 lowercase hex
// characters, so wal.AuditRecord.validate cannot tell them apart, DecodeAudit
// cannot, and no assertion on shape ever will. The only defence is this comment
// and the test that pins the digest to signing.CanonicalDigest by value.
func auditContentHash(m store.Message) (string, error) {
	digest, err := signing.CanonicalDigest(m.SigningMessage())
	if err != nil {
		// The one reachable case is a BROADCAST: signing.Canonicalize refuses an
		// empty recipient set, and a broadcast is stored as a flag with no
		// recipients, so it has no canonical bytes and therefore no content hash
		// under signing format v1.
		//
		// This is the SAME unanswered question that makes POST /v1/broadcast
		// return 501 (DECISIONS.md, Decision 4): what the canonical audience of a
		// broadcast IS belongs to SIGN-3. It is deliberately NOT answered here.
		// Substituting the bare-body hash, or a digest over a synthesised
		// audience, would settle SIGN-3 by accident -- in a file nobody would
		// think to read when they came to settle it properly -- and would write
		// that answer into a durable, append-only trail that cannot be rewritten.
		//
		// So this fails closed and says why. The alternative, auditing a
		// broadcast under a hash we invented, is worse than refusing it: it
		// produces a trail that looks authoritative and proves nothing.
		return "", fmt.Errorf("hub: no audit content hash for message %s: %w", m.ID, err)
	}
	// Lowercase hex, which is what wal.AuditRecord.validate requires.
	return hex.EncodeToString(digest[:]), nil
}
