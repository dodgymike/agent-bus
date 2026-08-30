package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// ErrConversationKeyReused reports invariant 10's SECOND case on the create
// path: the same idempotency key presented with a DIFFERENT payload (a
// different creator, name or recipient list). It is a protocol violation —
// REJECTED and logged — but it does NOT disconnect: the key is the caller's
// own (scoped per agent), so it is overwhelmingly a client that lost track of
// its keys rather than an attacker, and dropping the socket would punish every
// unrelated request that client had in flight (invariant 10, narrowed
// 2026-08-08). It is a single sentinel so a caller classifies with errors.Is.
var ErrConversationKeyReused = errors.New("store: conversation idempotency key already used with a different payload")

// conversationResultJSON is what the applied-key table stores as the result of
// a create. It holds ONLY the minted conversation id, NOT the whole record.
//
// # WHY ONLY THE ID (the IDEM-FU-RESULTBYTES-VS-MAXRECIPIENTS tension, resolved)
//
// idem.MaxResultBytes is 512. A conversation's stored result would otherwise
// have to carry its recipient list — up to MaxConversationRecipients (64)
// fully-qualified ids of up to ids.MaxAgentIDLen (150) bytes each — which is
// far over that cap. Rather than raise the cap (which enlarges the applied-key
// memory bound for every plane) the result stores the id alone, ~101 bytes, and
// a retry re-fetches the full record from the serving copy with Get. The record
// is durable and is recovered on restart (Apply), so it is always present when
// its applied key is; storing the recipient list a second time in the idem
// result would buy nothing and cost the cap.
type conversationResultJSON struct {
	ConversationID string `json:"conversation_id"`
}

// encodeConversationResult renders the applied-key result. It is compact and
// HTML-escaping is off, the same discipline every durable encoder in this
// package uses, so a live Remember and a replayed Recover hold identical bytes.
func encodeConversationResult(id string) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(conversationResultJSON{ConversationID: id}); err != nil {
		return nil, fmt.Errorf("%w: encoding the applied-key result: %v", ErrInvalidConversation, err)
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// decodeConversationResult parses a stored applied-key result back into the
// minted conversation id. It is STRICT — unknown fields and trailing data are
// refused — because a stored result read back for a retry is untrusted input,
// exactly like every other record this package decodes (invariant 1).
func decodeConversationResult(b json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j conversationResultJSON
	if err := dec.Decode(&j); err != nil {
		return "", fmt.Errorf("%w: decoding the applied-key result: %v", ErrInvalidConversation, err)
	}
	if dec.More() {
		return "", fmt.Errorf("%w: trailing data after the applied-key result", ErrInvalidConversation)
	}
	if err := validateConversationID(j.ConversationID); err != nil {
		return "", err
	}
	return j.ConversationID, nil
}

// conversationCreateFingerprint is the payload fingerprint invariant 10 turns
// on: two creates with the SAME key are a legitimate retry when their
// fingerprints match and a protocol violation when they do not.
//
// # THE FIXED FIELD LIST AND ORDER (ComputeFingerprint's contract)
//
// creator, then name, then each recipient in the ORDER SUPPLIED. Every field is
// length-prefixed by ComputeFingerprint, so field boundaries are unambiguous
// and a differing number of recipients yields a different digest. The recipient
// order is part of the payload: a retry that reorders the recipients is a
// DIFFERENT payload and is refused as a violation (a 409), which is safe — it
// never mints a second conversation — and keeps the fingerprint a pure function
// of exactly the bytes the record will store, which preserve that order.
func conversationCreateFingerprint(creator, name string, recipients []string) idem.Fingerprint {
	fields := make([][]byte, 0, 2+len(recipients))
	fields = append(fields, []byte(creator), []byte(name))
	for _, r := range recipients {
		fields = append(fields, []byte(r))
	}
	return idem.ComputeFingerprint(fields...)
}

// CreateIdempotent mints a conversation exactly like Create, but idempotently:
// the same (creator, key) presented again returns the ORIGINAL conversation
// rather than minting a second (invariant 10). It layers the applied-key table
// on top of Create's two-phase durable write, writing the conversation record
// and its applied-key record in ONE wal.Entry so the key becomes durable when —
// and only when — the conversation does.
//
// The three cases of invariant 10, NOT collapsed:
//
//   - same key + SAME payload -> LEGITIMATE RETRY: returns the original
//     conversation and (replayed=true); mints nothing, writes nothing.
//   - same key + DIFFERENT payload -> PROTOCOL VIOLATION: returns
//     ErrConversationKeyReused; writes nothing. It does NOT disconnect — that is
//     the caller (the httpapi handler) obeying invariant 10's narrowing, and
//     there is nothing to disconnect at this layer anyway.
//   - never seen -> a fresh create.
//
// The replay-of-a-signed-message disconnect case does NOT apply here: a create
// is not a signed relayed message, a create RPC carries one principal's traffic
// (the authenticated creator), and a merely buggy client can reach the
// key-reuse path — so, by invariant 10's two questions, no disconnect is added.
//
// creator is the fully-qualified authenticated principal (invariant 2), derived
// by the caller from the session/certificate and NEVER from a request field.
// The key is client-supplied and its shape is validated here (idem.ValidateKey,
// via NewAgentScope) before anything is minted or written.
//
// It returns only once the conversation is on stable storage (invariant 4). The
// whole orchestration is serialised by createMu, so two concurrent creates
// under one key cannot both pass the lookup.
func (s *ConversationStore) CreateIdempotent(creator, name string, recipients []string, idemKey string) (ConversationRecord, bool, error) {
	// Build the scope FIRST. NewAgentScope validates the key SHAPE
	// (idem.ValidateKey -> idem.ErrMissingKey / idem.ErrInvalidKey) and refuses
	// an empty creator, so a malformed request is rejected before any lock is
	// taken, any id is minted or anything is written. The creator's full
	// fully-qualified shape is validated again by canonicalConversation below;
	// this only refuses an empty one.
	sc, err := idem.NewAgentScope(creator, idem.OpConversationCreate, idemKey)
	if err != nil {
		return ConversationRecord{}, false, err
	}
	fp := conversationCreateFingerprint(creator, name, recipients)

	// createMu spans the whole orchestration: lookup -> admit -> write ->
	// remember. Without it, two retries of one key could both see OutcomeNew and
	// both write. It is released only when the conversation is durable and
	// remembered (or the attempt has failed with nothing written).
	s.createMu.Lock()
	defer s.createMu.Unlock()

	durable := s.durableLog()
	if durable == nil {
		return ConversationRecord{}, false, ErrConversationNotDurable
	}

	prev, outcome := s.idem.Lookup(sc, fp)
	switch outcome {
	case idem.OutcomeViolation:
		// The key is charset-bounded (it passed ValidateKey above), so it is safe
		// to name in the error; the two payloads are NOT echoed.
		return ConversationRecord{}, false, fmt.Errorf("%w: key %q was already applied to a different conversation create", ErrConversationKeyReused, idemKey)
	case idem.OutcomeRetry:
		// A legitimate retry: return the ORIGINAL conversation. The stored result
		// carries only the id; the full record is re-fetched from the serving
		// copy, which is durable and recovered on restart.
		id, derr := decodeConversationResult(prev.Result)
		if derr != nil {
			return ConversationRecord{}, false, fmt.Errorf("store: replaying the original conversation for idempotency key %q: %w", idemKey, derr)
		}
		rec, ok := s.Get(id)
		if !ok {
			// The applied key names a conversation the serving copy does not hold.
			// This cannot happen without a server bug or a tampered log (the two
			// are written and recovered together); failing is the only honest
			// option, because minting a fresh conversation under a key already
			// remembered would be a double-apply.
			return ConversationRecord{}, false, fmt.Errorf("store: the applied-key table names conversation %s for key %q but the conversation is not in the serving copy", id, idemKey)
		}
		return rec, true, nil
	}

	// OutcomeNew. Admit against the applied-key table's bounds — the bus-wide cap
	// and this creator's per-agent fair share — BEFORE minting anything. Nothing
	// is held between Admit and the write, but createMu keeps this table from
	// changing underneath us, so the admission decision still holds at the write.
	if err := s.idem.Admit(sc); err != nil {
		return ConversationRecord{}, false, err
	}

	id, err := NewConversationID(s.busID)
	if err != nil {
		return ConversationRecord{}, false, err
	}
	// Defensive copy: the caller keeps ownership of its slice, and the record we
	// validate, encode and retain must not alias a slice the caller can mutate
	// after the durable write.
	rcpts := append([]string(nil), recipients...)
	rec := ConversationRecord{
		ID:         id,
		Creator:    creator,
		Name:       name,
		Recipients: rcpts,
		CreatedAt:  s.now().UTC(),
	}
	// Canonicalised BEFORE the table is touched: a malformed creator, name or
	// recipient (ErrInvalidConversation) is refused here, with nothing written,
	// and the record folded into memory is byte-identical to the one replay reads
	// back.
	canon, body, err := canonicalConversation(rec)
	if err != nil {
		return ConversationRecord{}, false, err
	}

	s.mu.Lock()
	if len(s.records) >= MaxConversations {
		held := len(s.records)
		s.mu.Unlock()
		s.log.Error("REFUSING to create a conversation: the conversation table is at its hard entry cap and nothing is evicted to make room. Evicting a live conversation would be silent loss, not a bound",
			"held", held, "limit", MaxConversations, "creator", creator)
		return ConversationRecord{}, false, fmt.Errorf("%w: %d conversations retained against a limit of %d; the conversation is NOT created and nothing is evicted", ErrConversationCapacity, held, MaxConversations)
	}
	s.mu.Unlock()

	// The applied-key record, built and validated BEFORE the durable write.
	// CommittedAt is canon.CreatedAt — the SAME clock reading the conversation
	// carries, not a second s.now() — because retention is computed from it and
	// two readings would let the conversation and its key disagree about when the
	// create happened.
	result, err := encodeConversationResult(canon.ID)
	if err != nil {
		return ConversationRecord{}, false, fmt.Errorf("store: encoding the applied-key result for conversation %s: %w", id, err)
	}
	idemRecord := idem.Record{
		Agent:       creator,
		Op:          idem.OpConversationCreate,
		Key:         idemKey,
		Fingerprint: fp,
		Result:      result,
		CommittedAt: canon.CreatedAt,
	}
	encodedIdem, err := idemRecord.Encode()
	if err != nil {
		return ConversationRecord{}, false, fmt.Errorf("store: encoding the applied-key record for conversation %s: %w", id, err)
	}

	// THE DURABLE WRITE. Body and Idem ride in ONE two-phase transaction, so the
	// applied key becomes durable when — and only when — the conversation does,
	// in one fsync (invariant 10's load-bearing requirement). A separate write
	// ordered after the conversation would leave a window in which the
	// conversation is durable and the key is not; a crash there plus a retry
	// mints a second conversation.
	if _, err := durable.Write(wal.Entry{Kind: ConversationRecordKind, Body: body, Idem: encodedIdem}); err != nil {
		// NOTHING was acknowledged and nothing is in memory.
		return ConversationRecord{}, false, fmt.Errorf("store: writing the conversation record %s: %w", id, err)
	}

	// Reflect the already-durable state into memory. foldIn and Remember are BOTH
	// idempotent on an already-present entry, so if this Store is ALSO wired as
	// the wal applier — in which case Write called Apply above, which folded the
	// record in and Recovered the applied key — these are no-ops rather than
	// double-applies. If it is not (a unit test with a bare durable log that does
	// not call Apply), these do the insert.
	s.foldIn(canon)
	if err := s.idem.Remember(idemRecord); err != nil {
		// The conversation is COMMITTED but its applied key is not in the serving
		// table, so a retry would mint a SECOND conversation. It cannot happen —
		// Admit checked the identical predicate under createMu and Encode already
		// validated the record — but a "cannot happen" that corrupts the
		// applied-key table silently is exactly what must fail loudly.
		s.log.Error("a committed conversation's applied-key record could not be remembered, so a retry of its key would mint a second conversation",
			"conversation_id", id, "creator", creator, "err", err)
		return ConversationRecord{}, false, fmt.Errorf("store: remembering the applied-key record for conversation %s: %w", id, err)
	}
	return canon, false, nil
}

// IdempotencyStats reports the observable state of the conversation plane's
// applied-key table, for an operator or a test. It mirrors what the hub exposes
// for its own table; the bound is verified rather than assumed.
func (s *ConversationStore) IdempotencyStats() idem.Stats { return s.idem.Stats() }
