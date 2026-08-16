package auth

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"

	"github.com/dodgymike/agent-bus/internal/ids"
)

// The OPERATOR ID: a bus-scoped, NON-AGENT identity (AUTH-10).
//
// # WHY THE PRINCIPAL IS DISTINCT IN KIND AND NOT MERELY IN PERMISSION
//
// If an admin route reused AGENT authentication, an AGENT credential would
// authorise minting the credentials that CREATE AGENTS: any enrolled agent could
// mint itself an unlimited supply of new identities, which collapses invariant 3
// completely. So the principal must be distinct in KIND, not merely in
// permission — a permission bit on a roster entry is one boolean away from being
// granted by whoever can write the roster, and one refactor away from being read
// by a handler that only ever meant to check "is this caller authenticated".
//
// The id namespace is the FIRST of the three places that distinction is made
// structural (the other two are the separate durable record — operatorrecord.go
// — and the separate Go principal TYPE and session table — operatorsession.go).
//
// # THE DISJOINTNESS IS STRUCTURAL, AND THAT IS THE POINT
//
// An operator id is "op:<bus-id>.<name>-<suffix>". The ':' is the whole
// argument:
//
//   - ids.BusIDPattern is `^[A-Za-z0-9_-]{1,64}$` and ids.AgentNamePattern is
//     `^[a-z0-9][a-z0-9_-]{0,63}$`. NEITHER admits ':'. So the leading "op:"
//     cannot be produced by any well-formed agent id, and ids.ParseAgentID
//     REJECTS every operator id — it splits on the first '.', hands the left half
//     to ids.ValidateBusID, and "op:<bus-id>" fails the pattern.
//   - In the other direction ParseOperatorID requires the "op:" prefix, so it
//     rejects every agent id before it looks at anything else. THE "op:" PREFIX
//     IS THE ONLY STRUCTURAL SEAL — do not relax the prefix check in the belief
//     that the suffix rule makes it redundant, because it does not. The suffix
//     rule NARROWS the overlap and does not close it: an agent id's suffix is
//     DECIMAL DIGITS ONLY (ids.validateSuffixSpelling) and an operator id's is
//     16 characters of lowercase RFC4648 base32, whose alphabet [a-z2-7]
//     CONTAINS THE DIGITS 2-7 — so "2222222222222222" is a legal base32 suffix
//     AND a legal all-digits agent suffix at that length. This file's own
//     fixture "<bus-id>.ops-2222222222222222" is a valid AGENT id, which is the
//     proof. What the suffix rule does buy is that a MINTED operator id is
//     overwhelmingly unlikely to collide by shape (a random 16-character draw
//     from [a-z2-7] is all-digits with probability (6/32)^16), and that is a
//     probability, not a guarantee. The prefix is the guarantee.
//
// This is a mutual, checkable property rather than a convention, and a test
// asserts it in both directions over several shapes. Invariant 2's
// "<bus-id>.<agent-id>" shape is PRESERVED FOR AGENTS and deliberately NOT
// claimed for operators: an operator is not an agent, it is never a routing
// subject, no message is ever addressed to it, and it must never be able to
// masquerade as one in an ACL, a log line or a relay path.
//
// # INVARIANT 1: SERVER-MINTED, AND NEVER REUSED
//
// The suffix is 16 characters of lowercase RFC4648 base32 (alphabet [a-z2-7],
// no padding) over 10 bytes from crypto/rand — the SAME construction
// ids.GenerateBusID and invite.GenerateInviteID use, and cited here so nobody
// reads it as invented for this task. There is NO counter and NO suffix-floor
// file: uniqueness is by randomness, exactly as for a bus id and an invite id.
// That is a deliberate difference from an AGENT id, whose per-name suffix is a
// monotonic counter with a durable floor (ids.OpenNameSuffixes) because the
// agent id is human-facing and is typed by people; an operator id is handed to
// one operator once and pasted into a config.
//
// A REVOKED OPERATOR'S ID IS NEVER RE-ISSUED. Revocation appends a record; it
// never deletes one, and the id keeps naming the revoked principal forever.
// Adding an operator with a previously-used NAME mints a brand new suffix and is
// therefore a DIFFERENT principal — which is exactly what invariant 1 requires
// and is why `operator add` has no way to choose an id.
const (
	// OperatorIDPrefix is the four bytes that make an operator id structurally
	// unable to be an agent id. It is a PREFIX rather than a suffix or a
	// separate field so that the disjointness is visible in the first byte of
	// every log line, error message and config file that carries one.
	OperatorIDPrefix = "op:"

	// OperatorNamePattern is the one definition of a legal operator name.
	//
	// It is byte-identical to ids.AgentNamePattern today, and it is DECLARED
	// HERE rather than aliased to it on purpose: the two answer different
	// questions ("what may an agent ask to be called" versus "what may an
	// operator be called"), and an alias would make a future change to the agent
	// rule silently change the operator rule — including in the direction that
	// admits a ':' or a '.', which is what the disjointness above rests on.
	// A test pins them equal, so a divergence is a decision somebody has to
	// make, not one that happens by accident.
	OperatorNamePattern = `^[a-z0-9][a-z0-9_-]{0,63}$`

	// operatorIDSep separates the bus id from the operator half — the same
	// character invariant 2 uses, so an operator id reads the same way an agent
	// id does once past the prefix.
	operatorIDSep = "."

	// operatorSuffixSep separates the name from the minted suffix.
	operatorSuffixSep = "-"

	// maxOperatorNameLen is the longest name OperatorNamePattern admits: one
	// leading byte plus 63 trailing ones.
	maxOperatorNameLen = 64

	// operatorSuffixRandBytes is the crypto/rand entropy in the minted suffix.
	// 10 bytes -> exactly 16 base32 characters with no padding, the same
	// 10-bytes-to-16-characters shape as ids.GenerateBusID and
	// invite.GenerateInviteID.
	operatorSuffixRandBytes = 10

	// operatorSuffixLen is the resulting suffix length. Pinned as a constant so
	// the mint and the parser cannot drift: a parser that accepted a shorter
	// suffix would accept an id with less entropy than this bus ever issued.
	operatorSuffixLen = 16
)

// MaxOperatorIDLen is the longest a well-formed operator id can be: the prefix,
// the 64 bytes BusIDPattern allows a bus id, the qualification separator, the 64
// bytes OperatorNamePattern allows a name, the suffix separator and the 16
// characters of the minted suffix. Anything longer cannot be valid whatever it
// contains, which is what lets ParseOperatorID refuse an oversized string
// WITHOUT echoing it (ids.ParseAgentID's rule, and for its reason: an attacker
// choosing the input must not get to choose a multiple of it back out of a log
// line).
const MaxOperatorIDLen = len(OperatorIDPrefix) + 64 + len(operatorIDSep) + maxOperatorNameLen + len(operatorSuffixSep) + operatorSuffixLen

var (
	operatorNameRegexp = regexp.MustCompile(OperatorNamePattern)

	// operatorSuffixRegexp is the RFC4648 base32 alphabet, lowercased, at
	// exactly the minted length. It is a whitelist rather than a
	// "not obviously wrong" check because the suffix is the only part of the id
	// carrying entropy.
	operatorSuffixRegexp = regexp.MustCompile(`^[a-z2-7]{` + fmt.Sprint(operatorSuffixLen) + `}$`)
)

// ValidateOperatorName validates an untrusted operator name.
//
// Uppercase is REJECTED, not folded, for ids.ValidateAgentName's third reason
// read on this plane: an operator id is an AUTHORIZATION subject, so "Ops-1"
// sitting beside "ops-1" in an audit line is a social-engineering surface. The
// first two of that function's reasons (a folding function that must be
// preserved forever, and one spelling per counter key) do not apply here because
// there is no counter — but the security one does, and it is sufficient on its
// own.
func ValidateOperatorName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: an operator name must not be empty", ErrInvalidOperatorName)
	}
	if len(name) > maxOperatorNameLen {
		// Checked before the regexp, and the name is NOT echoed: it is
		// oversized, attacker- or typo-supplied, and on its way to a terminal.
		return fmt.Errorf("%w: the operator name is %d bytes, but an operator name is at most %d; it is not echoed here because it is oversized", ErrInvalidOperatorName, len(name), maxOperatorNameLen)
	}
	if !operatorNameRegexp.MatchString(name) {
		return fmt.Errorf("%w: operator name %q must match %s; in particular '.' and ':' are not allowed because they are the qualification separators that keep an operator id structurally disjoint from every agent id, and uppercase is REJECTED rather than folded", ErrInvalidOperatorName, name, OperatorNamePattern)
	}
	return nil
}

// MintOperatorID mints a fresh operator id for name on busID.
//
// THE SERVER IS AUTHORITATIVE (invariant 1): there is no path by which a caller
// supplies the suffix, and `agent-bus operator add` deliberately has no
// -operator-id flag. busID comes from the data directory's bus-id file and name
// is the operator's request; both are validated here rather than trusted.
//
// A crypto/rand failure is a HARD ERROR with no weaker fallback, exactly as in
// newToken and ids.GenerateBusID. A predictable operator id is not a credential
// on its own — the Ed25519 key and the client certificate are — but it is the
// name a revocation has to be able to pin down, and two operators colliding on
// one id would make a revocation ambiguous.
func MintOperatorID(busID, name string) (string, error) {
	if err := ids.ValidateBusID(busID); err != nil {
		return "", fmt.Errorf("auth: minting an operator id: %w", err)
	}
	if err := ValidateOperatorName(name); err != nil {
		return "", fmt.Errorf("auth: minting an operator id on bus %q: %w", busID, err)
	}

	buf := make([]byte, operatorSuffixRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: reading %d bytes from crypto/rand for an operator id suffix: %w; there is no weaker fallback", operatorSuffixRandBytes, err)
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))

	id := OperatorIDPrefix + busID + operatorIDSep + name + operatorSuffixSep + suffix
	if _, _, _, err := ParseOperatorID(id); err != nil {
		// Should never happen: the alphabet is a-z2-7 at a fixed length over two
		// already-validated halves. Kept as a defensive re-validation for
		// ids.GenerateBusID's reason — an id that fails its own invariant must
		// never reach a caller, because it would be stored and then rejected on
		// the way back off disk, at the worst possible moment.
		return "", fmt.Errorf("auth: the minted operator id %q failed its own validation: %w", id, err)
	}
	return id, nil
}

// ParseOperatorID splits an untrusted operator id into its bus id, name and
// minted suffix.
//
// It is a VALIDATOR, not a decoder, and it grants nothing (ids.ParseAgentID's
// rule): parsing establishes only that the string is a well-formed operator id.
// It says nothing about whether that operator exists, is revoked, or is the one
// on the other end of the connection — that is OperatorService.Authenticate's
// job, and it re-reads the registry on every call.
//
// # Why each split is EXACT
//
// The prefix is a fixed four bytes. The bus id is everything up to the FIRST
// '.', and neither ids.BusIDPattern nor OperatorNamePattern admits a '.', so a
// well-formed id contains exactly one and the first one is it. The suffix is
// everything after the LAST '-', and a name may legitimately contain '-'
// ("release-ops"), so the last one is the separator this package wrote — the
// same argument ids.ParseAgentID makes, one component further in.
func ParseOperatorID(id string) (busID, name, suffix string, err error) {
	if len(id) > MaxOperatorIDLen {
		return "", "", "", fmt.Errorf("invalid operator id: %d bytes, but an operator id is at most %d (%q); the id is not echoed here because it is oversized", len(id), MaxOperatorIDLen, OperatorIDPrefix+"<bus-id>"+operatorIDSep+"<name>"+operatorSuffixSep+"<suffix>")
	}
	if !strings.HasPrefix(id, OperatorIDPrefix) {
		// THE FIRST AND MOST IMPORTANT REFUSAL: it is what makes every AGENT id
		// fail here. Do not relax it to "accept an id with or without the
		// prefix" — that single change is what would let an agent id be parsed
		// as an operator id, and it would be invisible in every positive test.
		return "", "", "", fmt.Errorf("invalid operator id %q: it does not begin with %q, so it is not an operator id; an AGENT id is deliberately unable to satisfy this, because an operator is not an agent", id, OperatorIDPrefix)
	}
	rest := id[len(OperatorIDPrefix):]

	i := strings.Index(rest, operatorIDSep)
	if i < 0 {
		return "", "", "", fmt.Errorf("invalid operator id %q: expected the form %q, but there is no %q separator; an operator id is always qualified with its bus", id, OperatorIDPrefix+"<bus-id>"+operatorIDSep+"<name>"+operatorSuffixSep+"<suffix>", operatorIDSep)
	}
	busPart, tail := rest[:i], rest[i+len(operatorIDSep):]
	if verr := ids.ValidateBusID(busPart); verr != nil {
		// Reported in ids' own terms so there is ONE definition of "legal bus
		// id" in the system (BusIDPattern), never a second one here.
		return "", "", "", fmt.Errorf("invalid operator id %q: %v", id, verr)
	}

	j := strings.LastIndex(tail, operatorSuffixSep)
	if j < 0 {
		return "", "", "", fmt.Errorf("invalid operator id %q: the operator half %q has no %q separator, so it carries no server-minted suffix; a bare name is not an operator id (invariant 1)", id, tail, operatorSuffixSep)
	}
	namePart, suffixPart := tail[:j], tail[j+len(operatorSuffixSep):]
	if verr := ValidateOperatorName(namePart); verr != nil {
		return "", "", "", fmt.Errorf("invalid operator id %q: %w", id, verr)
	}
	if !operatorSuffixRegexp.MatchString(suffixPart) {
		return "", "", "", fmt.Errorf("invalid operator id %q: suffix %q must be exactly %d characters of lowercase RFC4648 base32 ([a-z2-7]); that is what this bus mints, and a shorter or differently-spelled one carries less entropy than any id it has ever issued", id, suffixPart, operatorSuffixLen)
	}
	return busPart, namePart, suffixPart, nil
}

// IsOperatorID reports whether id is a well-formed operator id.
//
// It is the cheap question a caller asks when it needs to know WHICH KIND of
// principal a string names — for example a future admin route deciding that a
// value arriving where an agent id belongs is not one. It answers on the SHAPE
// alone and, like ParseOperatorID, authorises nothing.
func IsOperatorID(id string) bool {
	_, _, _, err := ParseOperatorID(id)
	return err == nil
}
