package invite

import (
	"time"

	"github.com/dodgymike/agent-bus/internal/idem"
)

// The BOUNDED FIELDS of an invite record, derived rather than picked. Every one
// of them is ENFORCED by Record.validate — a bound nothing checks is a
// description, not a bound, and the size arithmetic below is only worth writing
// down if every term in it is a limit the code actually holds to.
const (
	// MaxBusIDLen bounds the bus id a record may carry. It is the upper bound of
	// ids.BusIDPattern (`^[A-Za-z0-9_-]{1,64}$`).
	//
	// It is RESTATED here rather than imported for the same reason
	// idem.MaxAgentLen restates ids.MaxAgentIDLen: this package is deliberately
	// thin on internal dependencies, and the number's job here is to be a term
	// in a size derivation. The drift that restating invites should be caught by
	// a cross-check test in a package that may import both.
	MaxBusIDLen = 64

	// MaxLabelLen bounds Record.Label, the operator's note on why an invite was
	// minted. 128 bytes, matching idem.MaxKeyLen: it is a one-line human note,
	// not a description field, and the same precedent already fixes the size of
	// every other short caller-supplied string in this codebase.
	//
	// The label is never echoed to a client (see Record.Label): it is operator
	// text and may name a person, a team or a ticket.
	MaxLabelLen = 128

	// MaxReasonLen bounds Record.RevokedReason, on the same reasoning and to the
	// same 128 bytes as MaxLabelLen.
	MaxReasonLen = 128
)

// The MEMORY BOUND, derived term by term the way idem/retention.go derives its
// own — because a bound that was picked is a bound nobody can check.
//
// # Where MaxRecordBytes comes from
//
// One retained Record's worst-case footprint, field by field, in its encoded
// form plus the Go overhead of holding it in a map:
//
//	id             <= MaxInviteIDLen                     64 B
//	bus            <= MaxBusIDLen                        64 B
//	secret_sha256  == DigestSize, hex                    64 B
//	cert_sha256    == DigestSize, hex                    64 B
//	redeem_fp      == idem.FingerprintSize, hex          64 B
//	redeemed_by    <= idem.MaxAgentLen                  150 B
//	redeem_key     <= idem.MaxKeyLen                    128 B
//	result         <= idem.MaxResultBytes               512 B
//	label          <= MaxLabelLen                       128 B
//	revoked_reason <= MaxReasonLen                      128 B
//	4 x RFC3339Nano timestamp @ ~30 B                   120 B
//	state          <= len("redeemed") rounded             8 B
//	JSON keys, quotes, colons, commas, braces           192 B
//	map bucket + string/slice headers + struct pad      200 B
//	                                                 -------
//	                                                  1886 B  ->  rounded up to 2 KiB
//
// The worst case deliberately counts fields that are mutually exclusive in
// practice (a redeemed record carries no revocation reason), because a bound
// that assumed the exclusion would have to be re-derived every time the state
// machine grew a state.
//
// # Why the budget is a QUARTER of idem's
//
// MaxRetainedBytes is 16 MiB against idem.MaxRetainedBytes' 64 MiB, and the
// reason is the ARRIVAL RATE, not the record size. Applied-key records arrive
// at the rate clients send; invites are OPERATOR-minted, through the filesystem
// (INVITE-MINT), so their arrival rate is bounded by a human with a shell. A
// budget sized for machine-driven traffic would be four times larger than any
// plausible use and would still be a fixed number nobody could justify.
//
// # 8192 invites, and what happens at the cap
//
// The cap FAILS CLOSED and evicts NOTHING, exactly as idem.ErrCapacity does. It
// is easier to justify here than there: an evicted invite is an UNKNOWN invite
// and therefore an unredeemable one, so eviction could never produce a second
// redemption. What it would produce is an operator's live invite silently
// ceasing to work. A refused mint is loud, immediate and recoverable.
const (
	// MaxRecordBytes is one retained record's worst-case footprint. See the
	// arithmetic above.
	MaxRecordBytes = 2 << 10

	// MaxRetainedBytes is the memory budget for the whole invite table.
	MaxRetainedBytes = 16 << 20

	// MaxInvites is the hard cap on retained invite records: 8192.
	MaxInvites = MaxRetainedBytes / MaxRecordBytes
)

// The TIME WINDOWS, each derived from something that already exists rather than
// chosen for roundness.
const (
	// SpentRetention is how long a record is remembered AFTER THE EVENT THAT
	// ENDED ITS USEFULNESS — redemption for a spent invite, expiry or revocation
	// for one that was never spent. It is idem.RetentionWindow (50h10m22s) and
	// it is that number BY CITATION, not by coincidence: the retry it has to
	// outlive is the same kind of retry idem's window is derived for — a client
	// that never saw the acknowledgement of its call and is still trying — so
	// re-deriving it here could only produce a second number free to drift from
	// the first.
	//
	// Its FIRST job is the legitimate-retry carve-out of invariant 10: for this
	// long after redemption, a retry with the SAME key and the SAME fingerprint
	// gets the ORIGINAL result back.
	//
	// Its SECOND job is DIAGNOSABILITY, and it is why an expired or revoked
	// record is kept past the moment it stopped working rather than dropped at
	// it. Dropping at ExpiresAt makes an expired invite indistinguishable from
	// one that never existed, so the ONLY answer this package could ever give an
	// operator chasing a failed enrolment is ErrUnknownInvite — ErrExpired would
	// be unreachable by construction, and INVITE-HARDEN would be collapsing a
	// sentinel that never fires. The window is the same one for the same reason:
	// an agent holding an invite is diagnosable for exactly as long as it could
	// still plausibly be retrying with it.
	//
	// It costs retention, not safety. A retained expired or revoked record is
	// still REFUSED (Store.Begin checks the state and the clock before it checks
	// anything else); what the record buys is the ability to say WHY. The
	// capacity consequence is stated plainly: with MaxTTL of 7 days plus this
	// window, the table holds every invite minted in the last ~9.1 days, capped
	// at MaxInvites — about 900 invites a day sustained, for a resource an
	// operator mints by hand.
	SpentRetention = idem.RetentionWindow

	// DefaultTTL is the lifetime of an invite whose mint requested none.
	//
	// An invite must survive the ordinary gap between an operator minting it and
	// an agent using it — a working day — and no longer than it has to, because
	// it is a bearer credential travelling over a channel this bus does not
	// control (DECISIONS.md, E6: the invite blob is the TRUST ANCHOR).
	DefaultTTL = 24 * time.Hour

	// MaxTTL is the longest lifetime an operator may request: a weekend plus a
	// full working day of operator/agent scheduling mismatch. Past that, the
	// exposure window of the bearer credential outweighs the convenience, and
	// the right answer is to mint a fresh invite rather than to have issued a
	// longer-lived one.
	//
	// A TTL over this is REJECTED (ErrInvalidTTL), never clamped.
	MaxTTL = 7 * 24 * time.Hour

	// ReservationTTL is how long a reservation taken by Store.Begin may be held
	// before Consume without being reaped.
	//
	// It must EXCEED the caller's worst case between Begin and Consume — minting
	// an agent id, then building the durable record — with generous margin, and
	// it must be short enough that a caller that died in that window does not
	// strand the invite for long. 30 seconds is roughly two orders of magnitude
	// above the two fsyncs and one id mint that the path actually costs.
	//
	// IT SWEEPS ONLY BEFORE Consume. After Consume the caller may already have
	// committed a durable consumption record, so reaping the reservation would
	// let a SECOND redemption in while the log says the invite is spent. After
	// Consume the reservation is resolvable only by Commit or Abort, and an
	// abandoned one stays locked until restart — which is fail-closed.
	ReservationTTL = 30 * time.Second
)
