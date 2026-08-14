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
// signedAs names the (message id, sequence) pair the CONTENT HASH must be
// computed under, when that is NOT the pair the local record carries.
//
// # It is non-zero for exactly one thing: a message ingested from a peer bus
//
// A relayed message's local record carries an id THIS bus minted (invariant 1 —
// a bus never adopts a peer's id) and a sender belonging to the ORIGIN bus
// (invariant 2). signing.Canonicalize REFUSES that pair, deliberately and by
// design: "a message is signed by an agent of the bus that minted its id". So a
// relayed record has NO canonical bytes of its own and cannot have any.
//
// The bytes that DO exist are the origin's — the exact bytes the origin agent
// signed and that internal/relay verified before handing the message over
// (relay.RelayedMessage.CanonicalBytes builds precisely this). Hashing those is
// what keeps auditContentHash's documented promise: the hash covers the bytes
// the stored signature covers, which is what lets the trail prove authorship
// without holding the content. Hashing anything else would leave a relayed
// record's hash covering bytes NOBODY ever signed.
//
// # WHAT A READER OF THE TRAIL MUST KNOW, stated carefully
//
// For a relayed record the content hash is over the ORIGIN's canonical bytes, so
// it does NOT reproduce from that record's own message id and sequence. It is
// still reproducible: the origin message id is durably recorded as the message
// record's idempotency_key (IngestRelayed passes it as the key), and the origin
// sequence parses out of it, so the message log carries everything needed. The
// AUDIT log alone does not — wal.AuditRecord carries neither the origin id nor
// the sender's timestamp — but that limitation is not new and is equally true of
// a local record.
//
// DO NOT USE THE BUS PATH TO TELL THE TWO APART. A multi-hop path does NOT imply
// a relayed record: hub's own buspath_test publishes a 3-hop path with a LOCAL
// sender, and that record's hash IS locally reproducible. The structural
// discriminator is the SENDER: a record whose sender's bus half is not this bus's
// is one whose hash was taken under the origin's assignment. That is exactly the
// condition IngestRelayed enforces and publish gates the substitution on.
type signedAs struct {
	messageID string
	seq       uint64
}

func auditRecordFor(m store.Message, signed signedAs) (*wal.AuditRecord, error) {
	hash, err := auditContentHash(m, signed)
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
		// The FULL path, however many hops it has. Since RELAY-11 this is one hop
		// for a local send and the whole traversed list for a message ingested from
		// a peer -- which is what makes a relay hop auditable at all (invariant 6).
		//
		// It is carried, not re-derived and not re-validated: store's constructor is
		// where an untrusted, peer-supplied path is checked hop by hop
		// (store.NewMessageWithBusPath), so a path that reached a store.Message has
		// already been through ids.ValidateBusID and the MaxBusPath bound. Checking
		// it a second time here would put a second, drifting definition of a legal
		// path between the message and its trail; wal.AuditRecord.validate applies
		// the framing-layer bounds on the far side.
		BusPath: m.BusPath,
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
//
// # THE RELAYED CASE
//
// See signedAs. When it is set, the digest is taken over the ORIGIN's canonical
// bytes rather than the local record's — which is not a second definition of the
// hash but the SAME one ("the bytes the signature covers") applied to a message
// whose signature was made on another bus. Only the id and the sequence are
// substituted; the sender, the recipients, the sender's timestamp and the body
// are already the origin's on both sides, so the two derivations differ in
// exactly the two fields this bus re-minted.
func auditContentHash(m store.Message, signed signedAs) (string, error) {
	sm := m.SigningMessage()
	if signed.messageID != "" {
		sm.MessageID = signed.messageID
		sm.Sequence = signed.seq
	}
	digest, err := signing.CanonicalDigest(sm)
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
