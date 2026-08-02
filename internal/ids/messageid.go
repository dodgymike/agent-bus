package ids

import (
	"fmt"
	"strconv"
	"strings"
)

// messageIDSep separates the bus id from the sequence number in a message id.
const messageIDSep = "-"

// MaxMessageIDLen is the longest a well-formed message id can be: the 64 bytes
// BusIDPattern allows a bus id, the separator, and the 20 decimal digits of
// math.MaxUint64. Anything longer cannot be valid whatever it contains.
const MaxMessageIDLen = 64 + len(messageIDSep) + 20

// MessageID formats the message id "<bus-id>-<seq>".
//
// Both halves are server-minted (invariant 1): busID comes from
// LoadOrCreateBusID and seq from a Sequence. They are validated anyway, because
// a malformed id is cheaper to catch at the one place it is built than to chase
// through the audit log later.
//
// seq == 0 is rejected. Sequence never issues 0 — the first Next returns 1 — so
// a 0 here is an unset field that some caller forgot to fill, and formatting it
// would mint a real-looking id for a message that has no durable sequence at
// all.
func MessageID(busID string, seq uint64) (string, error) {
	if err := ValidateBusID(busID); err != nil {
		return "", fmt.Errorf("building message id: %w", err)
	}
	if seq == 0 {
		return "", fmt.Errorf("building message id for bus %q: sequence 0 is never allocated and means \"unset\"; message sequences start at 1", busID)
	}
	return busID + messageIDSep + strconv.FormatUint(seq, 10), nil
}

// ParseMessageID splits an untrusted message id into its bus id and sequence.
//
// This is a validator, not a decoder: a message id arriving from a client, a
// peer or a relay is input to be checked, never an identity to be trusted
// (invariant 1). Nothing here grants authority — it only establishes that the
// string is a well-formed id — so the caller must still check that the named
// bus and sequence mean what the sender claims.
//
// # Why splitting on the LAST '-' is unambiguous
//
// Bus ids may contain '-' (the minted form is literally "bus-<base32>", see
// GenerateBusID), so the FIRST '-' says nothing about where the boundary is.
// The sequence half, however, is decimal digits only and can never contain a
// '-' — MessageID writes it with strconv.FormatUint, and this function rejects
// any non-digit — so the last '-' in a valid id is necessarily the separator
// this package wrote. Splitting there is exact, not a heuristic.
//
// # One id, one spelling
//
// A leading '+' or '-' on the sequence, and any leading zero, are rejected so
// that each message id has EXACTLY one spelling. "bus-x-007", "bus-x-+7" and
// "bus-x-7" must not all parse to the same message: two spellings of one id
// defeat duplicate detection and idempotency (invariant 10), because a dedup
// table keyed on the string form would see distinct keys for one message while
// a table keyed on the parsed pair would see one — and an attacker chooses
// which. Rejecting the alternate spellings collapses that gap.
func ParseMessageID(id string) (busID string, seq uint64, err error) {
	// Length is checked FIRST, and this is the one error here that does not
	// echo its input. Every other message quotes the id with %q to make the
	// failure diagnosable, and %q escapes a control byte to four characters —
	// so a megabyte of NULs would be echoed as four megabytes, twice over once
	// the bus-half error wraps. An attacker choosing the input must not get to
	// choose a multiple of it back in a log line, and past MaxMessageIDLen the
	// content cannot matter anyway: no such string is a valid id.
	if len(id) > MaxMessageIDLen {
		return "", 0, fmt.Errorf("invalid message id: %d bytes, but a message id is at most %d (\"<bus-id>%s<seq>\"); the id is not echoed here because it is oversized", len(id), MaxMessageIDLen, messageIDSep)
	}

	i := strings.LastIndex(id, messageIDSep)
	if i < 0 {
		return "", 0, fmt.Errorf("invalid message id %q: expected the form \"<bus-id>%s<seq>\", but there is no %q separator", id, messageIDSep, messageIDSep)
	}

	busID, seqPart := id[:i], id[i+len(messageIDSep):]
	if err := ValidateBusID(busID); err != nil {
		// Covers the empty bus id (an id like "-7") as well as every other
		// malformed one, and reports it in the same terms as any other bus id
		// so there is one definition of "legal bus id" (BusIDPattern).
		return "", 0, fmt.Errorf("invalid message id %q: %w", id, err)
	}

	if err := validateSeqSpelling(id, seqPart); err != nil {
		return "", 0, err
	}

	// ParseUint at base 10 rejects a sign prefix, underscores and every
	// non-digit, and reports overflow past 64 bits as ErrRange rather than
	// truncating — so an id claiming a sequence larger than any this bus could
	// have issued fails instead of aliasing a real one.
	seq, perr := strconv.ParseUint(seqPart, 10, 64)
	if perr != nil {
		return "", 0, fmt.Errorf("invalid message id %q: sequence %q is not a 64-bit decimal number: %w", id, seqPart, perr)
	}
	if seq == 0 {
		return "", 0, fmt.Errorf("invalid message id %q: sequence 0 is never allocated; message sequences start at 1", id)
	}
	return busID, seq, nil
}

// validateSeqSpelling enforces the canonical spelling of the sequence half:
// non-empty, digits only, no leading zero. ParseUint would accept "007" and
// reject the rest; this exists so there is exactly one spelling per id, and so
// the rejection reason names what is actually wrong.
func validateSeqSpelling(id, seqPart string) error {
	if seqPart == "" {
		return fmt.Errorf("invalid message id %q: the sequence after the final %q is empty", id, messageIDSep)
	}
	for i := 0; i < len(seqPart); i++ {
		if c := seqPart[i]; c < '0' || c > '9' {
			return fmt.Errorf("invalid message id %q: sequence %q must be decimal digits only (no sign, no whitespace, no underscores), but contains %q", id, seqPart, seqPart[i:i+1])
		}
	}
	if len(seqPart) > 1 && seqPart[0] == '0' {
		canonical := strings.TrimLeft(seqPart, "0")
		if canonical == "" {
			// All zeros ("00", "000"). Falling through to the seq == 0 rejection
			// in ParseMessageID is deliberate: advising the caller to write this
			// as "0" would name a spelling that is itself rejected, which sends
			// them round a loop instead of telling them what is wrong.
			return nil
		}
		return fmt.Errorf("invalid message id %q: sequence %q has a leading zero; a message id has exactly one spelling, so it must be written as %q", id, seqPart, canonical)
	}
	return nil
}
