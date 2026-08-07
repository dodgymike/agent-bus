package hub

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// # THE DURABLE MINT — why this file exists at all
//
// SIGN-1 settled that the SENDER signs the ORIGIN bus's minted message id and
// sequence (option (a)). That makes a send a TWO-STEP: the bus hands out a
// (message id, sequence) BEFORE it has anything to write, the client signs that
// assignment, and only then does the message arrive to be made durable.
//
// Handing out a sequence before any record is written breaks the argument Open
// used to rest on. That argument was a COUNTING one — "every sequence issued is
// <= the WAL index of the prepare carrying it", because each message consumed
// one sequence and at least two indices — and it is what let the restart floor
// be derived as NextIndex-1. A mint consumes a sequence and ZERO indices, so the
// counting stops holding on the very first mint: mint, restart, and the floor
// resumes BELOW numbers already handed out. Two validly-signed messages would
// then carry one origin message id, and nothing downstream can detect it,
// because both signatures verify.
//
// So the floor is no longer DERIVED, it is WRITTEN AHEAD. This is the same
// pattern MSG-FU-SUFFIXFLOOR established for per-name agent-id suffixes — see
// DECISIONS.md 2026-08-07 — and for the same reason: a number that has been
// handed to a client is burned whether or not anything came of it, and the only
// honest way to know that after a crash is to have said so on disk first.
//
// # WHERE the claim is written, and which copy wins
//
// It is written TWICE, and the two are NOT equal partners:
//
//   - <data-dir>/message-seq-floor (seqfloorfile.go) is AUTHORITATIVE. It is its
//     own atomically-replaced file, outside the log, so no repair, truncation or
//     QUARANTINE can lower it. It is written FIRST, and a failure to write it
//     fails the mint.
//   - the in-log SeqFloorRecordKind record below is DEFENCE IN DEPTH ONLY. It
//     exists for the case where the FILE is lost or moved aside by an operator
//     while the log is intact: replay then still recovers the burned range (Open
//     source (2)). It can do nothing on the case that matters most — a
//     quarantine discards it along with everything else in the log.
//
// The first draft of this mechanism had ONLY the in-log record, and that was a
// P0: a reviewer probe minted five sequences (two WAL indices, one floor
// record), quarantined the log, and watched the hub reissue 3, 4 and 5 —
// numbers a client already held Ed25519 signatures over. Do not "simplify" this
// back by deleting the file and keeping the record. The reverse simplification —
// deleting the record and keeping the file — is merely a loss of redundancy, and
// is the one to consider if the extra fsync per 256 mints ever matters.
//
// The property this file maintains, and the ONE the poison check now asserts:
//
//	every sequence this hub hands out is <= the durably-recorded sequence floor
//
// It is strictly stronger than the counting argument it retires, and unlike that
// argument it does not depend on how many WAL indices a message happens to
// consume — which is exactly the kind of incidental fact a future edit changes
// without noticing.

// SeqFloorRecordKind is the wal.Entry.Kind of the IN-LOG sequence-floor record.
//
// It is the SECONDARY copy of the claim: the authoritative one is the
// message-seq-floor FILE (seqfloorfile.go), which is the only copy that survives
// a quarantine. See "WHERE the claim is written" above before treating this
// record as the guarantee.
//
// It needs NO reservation: CONTRACTS-ONDISK.md records that Entry.Kind is a
// FREE-FORM APPLICATION STRING and not a reserved namespace, so a new kind
// costs nothing and collides with nothing. (Contrast store.RecordVersion, which
// IS reserved and was allocated for this same wave.)
//
// A record of this kind means exactly one thing, and it is a claim about the
// FUTURE rather than a record of the past:
//
//	every sequence <= Floor is BURNED. This bus will never issue any of them
//	again, whether or not a message ever carried one.
const SeqFloorRecordKind = "seqfloor"

// seqFloorRecordVersion versions the tiny JSON payload below. It is unexported
// because nothing outside this package writes or reads the record; a future
// field is a version bump, exactly as it is for store.Record.
const seqFloorRecordVersion = 1

// MintBatchSize is how many sequence numbers one floor record burns ahead.
//
// The trade it makes is fsyncs against gaps:
//
//   - Too small (1) and every mint costs a full durable write plus fsync, on the
//     LATENCY path of every send, doubling the fsyncs a message costs.
//   - Too large and a restart discards a bigger run of unissued numbers. That
//     costs nothing but gap size, and gaps are already CORRECT here —
//     internal/ids/sequence.go documents at length that consumers must treat the
//     sequence as strictly increasing, NEVER as dense.
//
// 256 puts the amortised cost at one extra fsync per 256 mints and ZERO on the
// send path itself, while a restart wastes at most 255 numbers out of a 64-bit
// space. Raising it is safe; lowering it below about 16 starts to show up as
// fsync cost on a busy bus.
const MintBatchSize = 256

// Bounds on the outstanding-mint table. All three FAIL CLOSED: a mint is
// refused rather than another agent's mint evicted. Evicting somebody else's
// mint would let one client take a second client's assigned sequence out from
// under it, and that client would then present a signature over an id the bus
// no longer recognises — a refusal caused entirely by a stranger.
const (
	// MaxOutstandingMintsPerAgent bounds how many mints ONE agent may hold
	// un-spent at a time. A well-behaved client mints and sends immediately, so
	// it holds one; the slack is for a client pipelining a burst or retrying
	// across a slow link. Past this the agent is either broken or hoarding
	// sequence numbers, and both are its own problem to fix — hence
	// ErrAgentQuota, which names the agent rather than blaming the bus.
	MaxOutstandingMintsPerAgent = 64

	// MaxOutstandingMints bounds the whole table. It is the memory bound and the
	// bus-wide fail-closed point: 8192 mints is 128 agents each holding their
	// full per-agent share, and each entry is a handful of words plus a message
	// id, so the table is tens of kilobytes at the limit.
	MaxOutstandingMints = 8192

	// MintTTL is how long an un-spent mint is honoured. It is generous on
	// purpose — a client that mints, signs and sends is measured in
	// milliseconds, so 15 minutes is four orders of magnitude of slack — because
	// the cost of expiring one is a REJECTED SEND for a client that did nothing
	// wrong, while the cost of holding one too long is a few bytes.
	//
	// Expiry does NOT un-burn the sequence. The number stays burned for ever;
	// only the table entry goes.
	MintTTL = 15 * time.Minute
)

// seqFloorRecord is the durable payload of a SeqFloorRecordKind entry.
//
// It is deliberately minimal. The record is not evidence about a message and
// carries nothing about one: it is a single high-water claim, and every byte
// beyond that is a byte a future reader could disagree with.
type seqFloorRecord struct {
	V     int    `json:"v"`
	Floor uint64 `json:"floor"`
}

// mintKey is the (agent, operation, key) tuple a mint is scoped by.
//
// It is the SAME scoping internal/idem uses (idem.Scope), for the same two
// reasons: one agent must not be able to collide with — or probe for — another
// agent's keys, and one agent must not be able to collide with ITSELF across two
// routes. It is a distinct struct rather than an idem.Scope because idem.Scope
// hides its fields behind accessors and is not usable as a map key.
type mintKey struct {
	agent string
	op    idem.Operation
	key   string
}

// outstandingMint is one sequence handed out and not yet spent.
//
// NOTE WHAT IS NOT HERE: the body, the recipients, the timestamp and the
// signature. A mint reserves a NUMBER; it does not pre-commit to a message. The
// client is free to sign whatever it likes over that number, and the bus checks
// the SHAPE of what comes back rather than matching it against a promise.
type outstandingMint struct {
	messageID string
	seq       uint64
	expiresAt time.Time
}

// MintRequest asks this bus to reserve a message id and sequence.
//
// Sender is the AUTHENTICATED principal, supplied by the caller from the request
// context and NEVER from the request body — there is no sender field on the wire
// for /v1/mint, and there never may be (invariant 1).
type MintRequest struct {
	Sender string

	// Op is "send" or "broadcast". It is a plain string so the HTTP layer can
	// pass the client's word through unaltered and let THIS package adjudicate
	// it; an unrecognised value is ErrInvalidOp, never a silent default.
	//
	// It is part of the scope rather than cosmetic: minting under "send" and
	// spending under "broadcast" with the same key must not be the same
	// reservation, or one route's idempotency key would shadow the other's.
	Op string

	// IdempotencyKey is the client's key for the operation this mint is FOR —
	// the same key the subsequent send must carry. Re-minting under it returns
	// the SAME assignment and burns no further sequence (invariant 10).
	IdempotencyKey string
}

// Mint is an accepted reservation.
type Mint struct {
	// MessageID is the server-minted "<bus-id>-<seq>" the client must sign
	// (invariant 1). The client does not choose it and cannot alter it: a send
	// presenting a different id is ErrMintMismatch.
	MessageID string

	// Seq is the sequence half of MessageID, carried separately because the
	// canonical signing format encodes it as a fixed-width integer as well as
	// inside the id, and checks the two agree (signing.Canonicalize).
	Seq uint64

	// Sender is the authenticated principal echoed back, so a client that sees
	// an unexpected value there has learned something worth knowing.
	Sender string

	// Op is the operation this mint may be spent on.
	Op string

	// ExpiresAt is when this reservation stops being honoured. After it the send
	// is ErrUnknownMint and the client must re-mint under the same idempotency
	// key; the old number stays burned.
	ExpiresAt time.Time

	// Replayed reports that this reservation came from the outstanding-mint
	// table rather than from a fresh allocation — the client re-minted under a
	// key it already holds, and NOTHING was allocated. Like Result.Replayed it
	// describes THIS CALL, so the HTTP layer surfaces it as a header and leaves
	// the body byte-identical to the original.
	Replayed bool
}

// Mint reserves a message id and sequence for sender, DURABLY, and returns it.
//
// # The order below is the whole of the guarantee and must not be rearranged
//
//  1. validate everything that can be validated without touching state
//  2. take writeMu — the mint allocates a sequence, so it belongs on the SAME
//     lock as publish or the two could interleave and reorder allocations
//  3. expire, then answer a repeat from the table (invariant 10: a retry is
//     never punished and allocates nothing)
//  4. apply the bounds — FAIL CLOSED, never evict
//  5. WRITE AND FSYNC THE SEQUENCE FLOOR if the number about to be issued sits
//     above the proven one
//  6. only then allocate, and assert the number is at or below that floor
//
// Step 5 before step 6 is the reason this file exists. A number handed out
// before it is durably burned is a number a restart can hand out again.
func (h *Hub) Mint(req MintRequest) (Mint, error) {
	op, err := parseMintOp(req.Op)
	if err != nil {
		return Mint{}, err
	}
	if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
		return Mint{}, err
	}
	if !h.Enrolled(req.Sender) {
		// Checked here as well as in publish, and BEFORE the lock, exactly as
		// publish does — see the Enrolled TOCTOU note. A mint is a durable act
		// (it burns numbers), so an unenrolled caller must not reach it.
		return Mint{}, fmt.Errorf("%w: %q", ErrUnknownSender, req.Sender)
	}
	if _, err := idem.NewAgentScope(req.Sender, op, req.IdempotencyKey); err != nil {
		// The key's SHAPE was already checked above, so a failure here is the
		// sender id being unusable — an internal fault, since the sender was
		// authenticated before it reached this function. Same posture, same
		// wording, as publish.
		return Mint{}, fmt.Errorf("hub: building the idempotency scope for %s: %w", op, err)
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	if h.poisoned != nil {
		return Mint{}, h.poisoned
	}
	if h.durable == nil {
		// Invariant 4 has no "best effort" setting, and that applies to the mint
		// as much as to the send: the floor record below is a durable write, and
		// a bus that cannot make it cannot honestly promise the number will
		// never be reissued.
		return Mint{}, ErrNotDurable
	}

	now := h.now()
	h.expireMintsLocked(now)

	mk := mintKey{agent: req.Sender, op: op, key: req.IdempotencyKey}
	if m, ok := h.mints[mk]; ok {
		// A legitimate re-mint: the ack was probably lost in flight, or the
		// client is retrying the whole two-step. Return the ORIGINAL assignment,
		// allocate nothing, burn nothing, and — like every retry under invariant
		// 10 — do not punish it. The expiry returned is the ORIGINAL expiry, so
		// the response body is byte-identical to the first one.
		return Mint{
			MessageID: m.messageID,
			Seq:       m.seq,
			Sender:    req.Sender,
			Op:        string(op),
			ExpiresAt: m.expiresAt,
			Replayed:  true,
		}, nil
	}

	// THE BOUNDS. Per-agent first, because it is the more specific fact: an
	// agent at its own share must be told it is ITS fault, not that the bus is
	// full — the same ordering, for the same reason, as the idem admission in
	// publish.
	if h.mintsByAgent[req.Sender] >= MaxOutstandingMintsPerAgent {
		return Mint{}, newAgentQuotaError("agent %q holds %d un-spent sequence reservations, its per-agent limit; nothing is evicted to make room, because evicting another mint would take a sequence back from a client that has already signed it", req.Sender, h.mintsByAgent[req.Sender])
	}
	if len(h.mints) >= MaxOutstandingMints {
		return Mint{}, fmt.Errorf("%w: %d sequence reservations are outstanding, the limit; nothing is evicted, because evicting a mint takes a sequence back from a client that has already signed it", ErrCapacity, len(h.mints))
	}

	if err := h.ensureSeqFloorLocked(); err != nil {
		return Mint{}, err
	}

	seq, err := h.seq.Next()
	if err != nil {
		return Mint{}, fmt.Errorf("hub: allocating a message sequence: %w", err)
	}
	if err := h.assertSeqFloorLocked("mint", "", seq); err != nil {
		return Mint{}, err
	}
	id, err := ids.MessageID(h.busID, seq)
	if err != nil {
		// The sequence is spent either way — invariant 1 forbids reusing it — so
		// this leaves a gap and nothing else. It cannot happen: the bus id was
		// validated in Open and Next never returns 0.
		return Mint{}, fmt.Errorf("hub: building the message id for sequence %d: %w", seq, err)
	}

	expires := now.Add(MintTTL)
	h.mints[mk] = outstandingMint{messageID: id, seq: seq, expiresAt: expires}
	h.mintsByAgent[req.Sender]++

	return Mint{
		MessageID: id,
		Seq:       seq,
		Sender:    req.Sender,
		Op:        string(op),
		ExpiresAt: expires,
	}, nil
}

// parseMintOp turns the client's word into the operation the scope is built
// from. It admits EXACTLY the two mutating message operations and nothing else.
//
// There is deliberately no default: an unrecognised op silently treated as
// "send" would let a client mint under one route's namespace and spend under
// another's, which is precisely the self-collision the scope tuple exists to
// prevent. idem.MutatingOperations carries four more values (enrol, leave,
// peer-enrol, relay); none of them mints a message sequence, so none of them
// belongs here, and a future one has to be added deliberately.
func parseMintOp(op string) (idem.Operation, error) {
	switch idem.Operation(op) {
	case idem.OpSend:
		return idem.OpSend, nil
	case idem.OpBroadcast:
		return idem.OpBroadcast, nil
	default:
		// The value is NOT echoed: it is untrusted, unbounded client input on
		// its way to a log line, and the set of legal answers is the whole of
		// what the caller needs.
		return "", fmt.Errorf("%w: a mint is for %q or %q", ErrInvalidOp, idem.OpSend, idem.OpBroadcast)
	}
}

// ensureSeqFloorLocked makes sure the number the allocator is about to issue is
// already covered by a durable floor, writing and fsyncing one if it is not.
// Caller holds writeMu.
//
// # The two writes, and their ORDER
//
//  1. the message-seq-floor FILE — AUTHORITATIVE, quarantine-proof, fsynced
//     (file and directory entry) before it returns;
//  2. the in-log "seqfloor" record — defence in depth for a lost file.
//
// The file goes FIRST so that the guarantee holds at every intermediate point: a
// crash after (1) and before (2) leaves the numbers burned by the copy that
// matters, while a crash the other way round would leave them burned only inside
// the artifact a quarantine deletes. Both must succeed before any number is
// handed out — a failure at either step returns an error and the mint issues
// nothing, so the numbers are merely burned early, which internal/ids/sequence.go
// declares CORRECT rather than damage to repair.
//
// # Why it peeks rather than allocating first
//
// The whole point is to write the claim BEFORE the number leaves this process.
// ids.Sequence.Peek exists for exactly this: it reports what Next would issue
// without issuing it, allocates nothing, and is safe to act on here because
// writeMu serialises the whole peek -> burn -> Next sequence against any other
// allocation.
//
// A Peek of 0 means the allocator will not issue at all — unsealed, or
// exhausted. This function returns nil in that case rather than inventing a
// diagnosis: the caller's very next act is Next(), which reports the real,
// distinct error (ErrFloorUnproven or ErrSequenceExhausted). Guessing here would
// mean two places deciding what is wrong with the allocator, and the wrong one
// would win.
func (h *Hub) ensureSeqFloorLocked() error {
	next := h.seq.Peek()
	if next == 0 {
		return nil
	}
	if next <= h.durableSeqFloor {
		// Already covered by a record on disk. This is the common case — 255 out
		// of every 256 mints — and it is why the mint costs no fsync of its own.
		return nil
	}

	target := next + MintBatchSize - 1
	if target < next {
		// Overflow. Only reachable within MintBatchSize of math.MaxUint64, i.e.
		// after ~1.8e19 messages, and it is handled rather than wrapped for the
		// same reason ids.Sequence refuses to wrap: a wrapped floor would claim
		// a LOW number is burned and permit reissuing every id this bus has ever
		// minted. Claiming the whole space is burned is the safe direction — the
		// allocator then exhausts and the bus stops accepting sends, loudly.
		target = math.MaxUint64
	}

	// THE AUTHORITATIVE DURABLE WRITE. Nothing may be handed out before it
	// returns, and no repair of the log can undo it.
	//
	// A nil file here means a hub with no data directory, which Open only permits
	// when there is no durable write path at all — so this is unreachable from
	// Mint, which refuses on h.durable == nil first. It is checked anyway,
	// fail-closed: this is the last gate before a number leaves the process, and
	// the day someone makes DataDir optional again the failure must be loud
	// rather than a silently unburned sequence.
	if h.seqFloorFile == nil {
		return fmt.Errorf("%w: this hub has no durable message sequence floor file, so a number handed out now could be handed out again after a restart (invariant 1)", ErrNotDurable)
	}
	if err := h.seqFloorFile.raise(target); err != nil {
		return fmt.Errorf("hub: durably burning message sequences up to %d before minting: %w", target, err)
	}

	payload, err := json.Marshal(seqFloorRecord{V: seqFloorRecordVersion, Floor: target})
	if err != nil {
		return fmt.Errorf("hub: encoding the sequence floor record for %d: %w", target, err)
	}
	// THE SECONDARY DURABLE WRITE, and nothing may be handed out before it
	// returns either — see the ORDER note above for why this one is second. It
	// carries no Idem record: a floor record is not an operation a client
	// performed, so there is no applied key to remember, and no Audit record:
	// invariant 6's audit trail is about MESSAGES, and a burned range is not one.
	if _, err := h.durable.Write(wal.Entry{Kind: SeqFloorRecordKind, Body: payload}); err != nil {
		return fmt.Errorf("hub: durably burning message sequences up to %d before minting: %w", target, err)
	}
	h.durableSeqFloor = target
	h.log.Debug("burned a batch of message sequence numbers ahead of minting",
		"floor", target,
		"batch", MintBatchSize,
	)
	return nil
}

// assertSeqFloorLocked is the id-authority assertion that REPLACED the counting
// argument. Caller holds writeMu.
//
// It is checked at BOTH ends of a sequence's life — where the number is issued
// (Mint) and again where the message carrying it is written (publish) — because
// the two are separated by a network round trip and by a client, and the whole
// value of the assertion is that it does not depend on the code between them
// staying correct.
//
// On violation the hub is POISONED: this operation fails, no further send or
// mint is accepted, and an operator gets an ERROR naming both numbers. That is
// the same response the old check had, and for the same reason — by the time
// this can fire, serving on would mean minting ids from a floor the next start
// cannot reconstruct, and the damage (reissued message ids) is undetectable
// downstream.
func (h *Hub) assertSeqFloorLocked(op, messageID string, seq uint64) error {
	if seq <= h.durableSeqFloor {
		return nil
	}
	h.poisoned = fmt.Errorf("%w: %s took sequence %d but the durably-recorded sequence floor is only %d; a restart would derive a floor BELOW a number already handed out, and message ids would repeat (invariant 1)",
		ErrPoisoned, op, seq, h.durableSeqFloor)
	h.log.Error("POISONED: a message sequence has overtaken the durably-recorded sequence floor, so a restart could reissue message ids; refusing all further mints and sends",
		"op", op,
		"message_id", messageID,
		"seq", seq,
		"durable_seq_floor", h.durableSeqFloor,
	)
	return h.poisoned
}

// expireMintsLocked drops every reservation whose TTL has passed. Caller holds
// writeMu.
//
// It runs on EVERY mint (Mint) and EVERY send (publish, just before the
// reservation is looked up) rather than on a timer: a background sweeper would
// be a second clock and a second goroutine to reason about, and this table is
// only ever touched under one lock. A bus with no traffic therefore never
// expires anything, which is correct — nothing is competing for the space.
//
// BOTH CALL SITES ARE LOAD-BEARING and neither is a tidy-up. The send-path call
// is what makes MintTTL true rather than decorative: without it an expired
// reservation is still spendable, and Mint.ExpiresAt — which this bus returns to
// the client as a fact — is not one. It was genuinely missing between SIGN-6 and
// 2026-08-07, and nothing caught it, because every test spent its mint
// immediately. TestMintTTLExpiry in mint_test.go is that missing test.
//
// The numbers are NOT un-burned. Expiry frees a table slot; the sequence stays
// burned for ever, leaving a gap, which internal/ids/sequence.go documents as
// CORRECT rather than as damage to repair.
func (h *Hub) expireMintsLocked(now time.Time) {
	if len(h.mints) == 0 {
		return
	}
	for k, m := range h.mints {
		if now.Before(m.expiresAt) {
			continue
		}
		delete(h.mints, k)
		h.decMintCountLocked(k.agent)
	}
}

// decMintCountLocked drops one from an agent's outstanding-mint count and
// removes the entry entirely at zero. Caller holds writeMu.
//
// The map entry is deleted rather than left at 0 so that an agent that mints
// once and never returns does not leave a permanent row: mintsByAgent must not
// become an unbounded, attacker-grown index of every id that ever minted.
func (h *Hub) decMintCountLocked(agent string) {
	n := h.mintsByAgent[agent]
	if n <= 1 {
		delete(h.mintsByAgent, agent)
		return
	}
	h.mintsByAgent[agent] = n - 1
}

// applySeqFloor folds a replayed SeqFloorRecordKind entry into the recovered
// floor. It runs during recovery, from Hub.Apply.
//
// An UNDECODABLE record is skipped LOUDLY, at ERROR, and that is the whole
// reason this is not three lines: invariant 6 sanctions the DISCARD, and makes
// SILENT discard the defect. A dropped floor record is not cosmetic — it is a
// claim that a range of sequences is burned, and losing it is exactly how the
// next start resumes below numbers already handed out.
//
// It cannot return an error: returning one would abort recovery and refuse to
// start the bus, which invariant 6 settled against on 2026-08-02. What protects
// the id space when this happens is above all the message-seq-floor FILE (Open
// source (0)), which is written first and independently and which a lost or
// undecodable log record cannot affect at all — plus the other two log-derived
// sources Open takes the maximum over, the WAL high-water index and the highest
// replayed message sequence.
//
// Losing THIS record is therefore no longer the serious event the paragraph
// above describes; losing it AND the floor file is. The discard stays at ERROR
// regardless: an undecodable record in a healthy log is evidence of damage
// whatever else survives.
func (h *Hub) applySeqFloor(c wal.Committed) error {
	var rec seqFloorRecord
	if err := json.Unmarshal(c.Entry.Body, &rec); err != nil {
		h.log.Error("DISCARDING a sequence-floor record that could not be decoded during recovery; the sequence numbers it burned are no longer proven burned by THIS record, and the floor falls back to the WAL high-water index and the highest replayed message sequence (invariant 1)",
			"prepare_index", c.PrepareIndex,
			"commit_index", c.CommitIndex,
			"err", err,
		)
		return nil
	}
	if rec.V != seqFloorRecordVersion {
		h.log.Error("DISCARDING a sequence-floor record written in a schema version this build does not understand; the sequence numbers it burned are no longer proven burned by THIS record (invariant 1)",
			"prepare_index", c.PrepareIndex,
			"record_version", rec.V,
			"understood_version", seqFloorRecordVersion,
		)
		return nil
	}
	if rec.Floor > h.replayedSeqFloor {
		h.replayedSeqFloor = rec.Floor
	}
	return nil
}
