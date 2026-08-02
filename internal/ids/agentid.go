package ids

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// AgentNamePattern is the one definition of a legal requested agent name — the
// short, human-chosen half an enrolling client asks for, before the server
// qualifies it with the bus id and a minted suffix.
//
// '.' is excluded on purpose: it is the "<bus-id>.<agent-id>" qualification
// separator (invariant 2), and a name containing one would make the boundary
// between the two halves ambiguous. The first byte may not be '-' or '_' so a
// name can never be mistaken for a flag or an empty component.
const AgentNamePattern = `^[a-z0-9][a-z0-9_-]{0,63}$`

var agentNameRegexp = regexp.MustCompile(AgentNamePattern)

// maxAgentNameLen is the longest name AgentNamePattern admits: one leading
// byte plus 63 trailing ones.
const maxAgentNameLen = 64

// agentIDSep separates the bus id from the agent id (invariant 2).
const agentIDSep = "."

// agentSuffixSep separates the requested name from the server-minted suffix.
const agentSuffixSep = "-"

// MaxAgentIDLen is the longest a well-formed fully-qualified agent id can be:
// the 64 bytes BusIDPattern allows a bus id, the qualification separator, the
// 64 bytes AgentNamePattern allows a name, the suffix separator, and the 20
// decimal digits of math.MaxUint64. Anything longer cannot be valid whatever it
// contains.
const MaxAgentIDLen = 64 + len(agentIDSep) + maxAgentNameLen + len(agentSuffixSep) + 20

// ValidateAgentName validates an untrusted requested agent name. It returns a
// descriptive error, and it is deliberately the ONLY definition of "legal name"
// in this package — everything that keys on a name (the per-name suffix
// counters, the durable roster) assumes the name reached it through here.
//
// # Uppercase is REJECTED, not case-folded
//
// Folding "Bob" to "bob" would be friendlier and is wrong, for three reasons:
//
//   - It creates a mapping that must be preserved FOREVER. The per-name counter
//     key has to be byte-identical to the name half embedded in the ids already
//     on disk; if the server folded case, the durable recovery path (AUTH-3)
//     would have to reproduce that exact folding function on every future
//     restart, and the day it drifts — a Unicode table update, a "tidier"
//     normalisation — a name gets a FRESH counter and re-mints an id that is
//     already live. Rejecting means input bytes == counter key bytes == id
//     bytes, with no mapping to keep in step.
//   - It gives one spelling per name, exactly as validateSeqSpelling gives one
//     spelling per message id — and it does so by REFUSING the alternate
//     spelling rather than silently rewriting it, which is this package's
//     established posture (LoadOrCreateBusID refuses a corrupt persisted id
//     rather than regenerating it).
//   - Security: it removes ASCII case-confusables from the id space at the
//     door. A fully-qualified agent id is the routing and authorization subject
//     (invariants 2 and 3), so "Bob-1" sitting next to "bob-1" in an agent list
//     is a social-engineering surface, not a cosmetic one.
//
// The cost to the client is one strings.ToLower before it enrols. Note that the
// roster (AUTH-3) may still keep the originally requested string as a DISPLAY
// attribute if it wants to; it just must not be the id, and must not be what
// the counter is keyed on.
func ValidateAgentName(name string) error {
	if name == "" {
		return errors.New("agent name must not be empty")
	}
	if len(name) > maxAgentNameLen {
		// Checked before the regexp so an oversized name is reported as
		// oversized rather than as a generic pattern miss, and so the message
		// below never quotes an unbounded string.
		return fmt.Errorf("agent name is %d bytes, but an agent name is at most %d; the name is not echoed here because it is oversized", len(name), maxAgentNameLen)
	}
	if !agentNameRegexp.MatchString(name) {
		return fmt.Errorf("agent name %q must match %s; in particular '.' is not allowed because it is the \"<bus-id>.<agent-id>\" qualification separator (invariant 2), and uppercase is REJECTED rather than folded to lowercase so that the name the counter is keyed on is byte-identical to the name embedded in the ids on disk — lowercase the name in the client", name, AgentNamePattern)
	}
	return nil
}

// AgentID formats the fully-qualified agent id "<bus-id>.<name>-<n>"
// (invariant 2).
//
// busID comes from LoadOrCreateBusID and n from a SuffixAllocator: both halves
// the server controls are server-minted (invariant 1). name is the client's
// requested short name and is untrusted. All three are validated anyway,
// because a malformed id is cheaper to catch at the one place it is built than
// to chase through the audit log later.
//
// n == 0 is rejected, for the same reason MessageID rejects sequence 0:
// NameSuffixes never issues 0 — the first NextSuffix for a name returns 1 — so
// a 0 here is an unset field some caller forgot to fill, and formatting it
// would mint a real-looking id for an agent that has no allocated suffix at all.
func AgentID(busID, name string, n uint64) (string, error) {
	if err := ValidateBusID(busID); err != nil {
		return "", fmt.Errorf("building agent id: %w", err)
	}
	if err := ValidateAgentName(name); err != nil {
		return "", fmt.Errorf("building agent id on bus %q: %w", busID, err)
	}
	if n == 0 {
		return "", fmt.Errorf("building agent id for %q on bus %q: suffix 0 is never allocated and means \"unset\"; agent name suffixes start at 1", name, busID)
	}
	return busID + agentIDSep + name + agentSuffixSep + strconv.FormatUint(n, 10), nil
}

// ParseAgentID splits an untrusted fully-qualified agent id into its bus id,
// requested name and minted suffix.
//
// This is a validator, not a decoder: an agent id arriving from a client, a
// peer or a relay is input to be checked, never an identity to be trusted
// (invariant 1). Nothing here grants authority — it only establishes that the
// string is a well-formed id — so a caller must still check the presented
// credential (invariant 3) before believing the bearer is the named agent. In
// particular, parsing an id successfully says nothing about whether that agent
// exists, is enrolled, or is the one on the other end of the connection.
//
// # Why splitting on the FIRST '.' is exact
//
// Neither half may contain '.': BusIDPattern excludes it and AgentNamePattern
// excludes it, both precisely so this boundary is unambiguous (invariant 2). A
// well-formed id therefore contains EXACTLY one '.', and the first one is it. A
// second '.' anywhere necessarily lands in the name half, where
// ValidateAgentName rejects it — so an id like "bus-x.a.b-1" fails as a bad
// name rather than being silently re-split. This is exact, not a heuristic.
//
// # Why splitting the remainder on the LAST '-' is exact
//
// Names may contain '-' (AgentNamePattern allows it, and "code-reviewer" is the
// obvious case), so the FIRST '-' says nothing about where the suffix starts.
// The suffix, however, is decimal digits only and can never contain a '-' —
// AgentID writes it with strconv.FormatUint, and this function rejects any
// non-digit — so the last '-' in a valid id is necessarily the separator this
// package wrote. Same argument as ParseMessageID, one component further in.
//
// # One id, one spelling
//
// A leading zero on the suffix, a sign, whitespace or underscores are rejected
// so each agent id has EXACTLY one spelling. See validateSuffixSpelling.
func ParseAgentID(id string) (busID, name string, n uint64, err error) {
	// Length is checked FIRST, and this is the one error here that does not
	// echo its input. Every other message quotes the id with %q to make the
	// failure diagnosable, and %q escapes a control byte to four characters —
	// so a megabyte of NULs would be echoed as four megabytes, several times
	// over once the inner errors wrap. An attacker choosing the input must not
	// get to choose a multiple of it back out in a log line, and past
	// MaxAgentIDLen the content cannot matter anyway: no such string is a valid
	// id.
	if len(id) > MaxAgentIDLen {
		return "", "", 0, fmt.Errorf("invalid agent id: %d bytes, but an agent id is at most %d (\"<bus-id>%s<name>%s<n>\"); the id is not echoed here because it is oversized", len(id), MaxAgentIDLen, agentIDSep, agentSuffixSep)
	}

	i := strings.Index(id, agentIDSep)
	if i < 0 {
		return "", "", 0, fmt.Errorf("invalid agent id %q: expected the form \"<bus-id>%s<name>%s<n>\", but there is no %q separator; an agent id is always fully qualified with its bus (invariant 2)", id, agentIDSep, agentSuffixSep, agentIDSep)
	}

	busPart, rest := id[:i], id[i+len(agentIDSep):]
	if err := ValidateBusID(busPart); err != nil {
		// Covers the empty bus id (an id like ".a-1") as well as every other
		// malformed one, and reports it in the same terms as any other bus id
		// so there is one definition of "legal bus id" (BusIDPattern).
		return "", "", 0, fmt.Errorf("invalid agent id %q: %w", id, err)
	}

	j := strings.LastIndex(rest, agentSuffixSep)
	if j < 0 {
		return "", "", 0, fmt.Errorf("invalid agent id %q: the agent half %q has no %q separator, so it carries no server-minted suffix; a bare name is not an agent id (invariant 1)", id, rest, agentSuffixSep)
	}

	namePart, suffixPart := rest[:j], rest[j+len(agentSuffixSep):]
	if err := ValidateAgentName(namePart); err != nil {
		return "", "", 0, fmt.Errorf("invalid agent id %q: %w", id, err)
	}

	if err := validateSuffixSpelling(id, suffixPart); err != nil {
		return "", "", 0, err
	}

	// ParseUint at base 10 rejects a sign prefix, underscores and every
	// non-digit, and reports overflow past 64 bits as ErrRange rather than
	// truncating — so an id claiming a suffix larger than any this bus could
	// have issued fails instead of aliasing a real one.
	n, perr := strconv.ParseUint(suffixPart, 10, 64)
	if perr != nil {
		return "", "", 0, fmt.Errorf("invalid agent id %q: suffix %q is not a 64-bit decimal number: %w", id, suffixPart, perr)
	}
	if n == 0 {
		return "", "", 0, fmt.Errorf("invalid agent id %q: suffix 0 is never allocated; agent name suffixes start at 1", id)
	}
	return busPart, namePart, n, nil
}

// validateSuffixSpelling enforces the canonical spelling of the minted suffix:
// non-empty, decimal digits only, no leading zero. ParseUint would accept "007"
// and reject the rest; this exists so there is exactly one spelling per agent
// id, and so the rejection reason names what is actually wrong.
//
// Two spellings of one agent id are worse here than for a message id: the
// fully-qualified agent id is the ROUTING and AUTHORIZATION subject (invariants
// 2 and 3), so "bus-x.bob-07" and "bus-x.bob-7" resolving to the same agent
// while comparing unequal as strings would let an ACL keyed on the string form
// and a router keyed on the parsed form disagree about who is being addressed —
// and the sender chooses which.
//
// This deliberately enforces the SAME canonical-spelling rule as
// validateSeqSpelling in messageid.go, with agent-id wording. The duplication
// is intentional: that file ships under ID-2 and this task has no reason to
// change its behaviour or its error text. The two must NOT drift — a test pins
// them to the same accept/reject set, so any change here needs the same change
// there and vice versa.
func validateSuffixSpelling(id, suffixPart string) error {
	if suffixPart == "" {
		return fmt.Errorf("invalid agent id %q: the suffix after the final %q is empty", id, agentSuffixSep)
	}
	for i := 0; i < len(suffixPart); i++ {
		if c := suffixPart[i]; c < '0' || c > '9' {
			return fmt.Errorf("invalid agent id %q: suffix %q must be decimal digits only (no sign, no whitespace, no underscores), but contains %q", id, suffixPart, suffixPart[i:i+1])
		}
	}
	if len(suffixPart) > 1 && suffixPart[0] == '0' {
		canonical := strings.TrimLeft(suffixPart, "0")
		if canonical == "" {
			// All zeros ("00", "000"). Falling through to the n == 0 rejection
			// in ParseAgentID is deliberate: advising the caller to write this
			// as "0" would name a spelling that is itself rejected, which sends
			// them round a loop instead of telling them what is wrong.
			return nil
		}
		return fmt.Errorf("invalid agent id %q: suffix %q has a leading zero; an agent id has exactly one spelling, so it must be written as %q", id, suffixPart, canonical)
	}
	return nil
}
