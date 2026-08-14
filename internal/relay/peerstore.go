package relay

// PeerStore is the DURABLE half of federation configuration: which buses this
// bus routes to, and which bus signing keys it pins for a bus.
//
// # Why this exists
//
// Registry (registry.go) is the SERVING copy and persists nothing: every peer it
// holds vanishes on restart. That is invariant 5 with one half missing — memory
// is the serving copy, but there was no disk for it to be the truth of, so an
// operator running a three-machine federation would have to re-peer every
// machine after every restart, by hand, over SSH. This file is that missing
// disk.
//
// # ROUTING AND TRUST ARE TWO RECORDS, NOT ONE
//
// This is the most important thing about the shape, it is not abstraction for
// its own sake, and it comes from RELAY-7's cross-bus trust deep-dive rather
// than from anybody's taste:
//
//	laptop(A) <-> internet(B) <-> this machine(C)
//
// C NEVER PEERS WITH A. It has no address for A, nothing to dial, and no reason
// to acquire one — B is the hop. But C must still PIN A's bus signing key,
// because a relayed message ORIGINATING at A is verified by C against that pin,
// and B is explicitly not allowed to vouch for it (signed.go's CrossBusTrust:
// "presentation is not attestation"). So C needs TRUST for A with NO ROUTE for
// A. A single record coupling an address to a key cannot express that, and the
// mistake would be undiscoverable until RELAY-17 tried to use it — by which
// point changing the shape is a migration, not an edit.
//
// The mirror case is just as real: a bus we relay THROUGH but accept no origin
// traffic from has a route and no pins.
//
// So:
//
//	Kind "peer"     -> PeerRecord      {bus_id, config_seq, state, base_url}
//	Kind "bustrust" -> BusTrustRecord  {bus_id, config_seq, state, bus_signing_keys[]}
//
// and the keys are a LIST because a signing-key rollover needs the outgoing and
// the incoming key pinned simultaneously (signed.go:178-182). A scalar would
// force a federation-wide outage on every rotation.
//
// # What is NOT here
//
// This file owns the two RECORDS and the TABLE. It does not wire itself into
// Registry, implements no part of CrossBusTrust, and registers no route and no
// subcommand: nothing in a running bus reads it yet. Restoring a Registry from a
// recovered PeerStore, the offline `agent-bus peer` subcommand that writes one,
// and the verification that consumes the pins are separate tasks. Read that as
// the honest scope statement it is — this is durability, not yet behaviour an
// operator can observe.
//
// # The shape is internal/invite's, deliberately
//
// Same "every entry carries the COMPLETE record in its post-transition state,
// never a delta", same single monotonic upsert that both the live path and the
// replay path go through, same never-return-an-error Apply, same cap enforced on
// the replay path too. Where it differs from invite it is because a PEER'S
// LIFECYCLE IS NOT AN INVITE'S, and each difference is argued where it appears.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dodgymike/agent-bus/internal/buscert"
	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// PeerRecordKind and BusTrustRecordKind are the wal.Entry.Kind discriminators
// for the two durable records.
//
// Entry.Kind is a FREE-FORM APPLICATION DISCRIMINATOR (internal/wal/log.go: "the
// application discriminator: \"message\", \"agent\", ..."), not a numbered
// on-disk record type: it sits inside the PREPARE payload, above the framing
// layer that wal.Type (TypePrepare, TypeCommit, ...) owns. So NO record-type
// number and NO ondisk-format-version was reserved for either of them, and
// internal/wal/format.go is untouched — exactly as auth.RecordKind = "agent" and
// invite.RecordKind = "invite" before them. This is written down because
// "numbers are reserved, not chosen" is a standing rule in this repo, and the
// next reader should not go and reserve a number nothing requires.
const (
	PeerRecordKind     = "peer"
	BusTrustRecordKind = "bustrust"
)

// PeerRecordVersion is the schema version carried in both records' "v" field. It
// follows auth.RecordVersion's precedent rather than invite's versionless
// record: these records are the durable home of a federation's routing table and
// its TRUST ANCHORS, and a future shape change should be diagnosable as "version
// 2, this binary reads 1" rather than as an unrecognised field.
const PeerRecordVersion = 1

// MaxPeerBaseURLLen bounds the base URL stored in a peer record.
//
// DERIVED, not guessed: a legal value is a BARE https origin, so the longest one
// is "https://" (8) + a maximum DNS name (253) + ":65535" (6) = 267 bytes. 512
// leaves room for a bracketed IPv6 literal with a percent-encoded zone, and can
// never refuse an origin that is otherwise legal.
const MaxPeerBaseURLLen = 512

// MaxPinnedBusSigningKeys bounds how many bus signing keys may be pinned for one
// bus.
//
// It is 2, and that is a DERIVATION rather than a round number: signed.go's
// CrossBusTrust doc fixes the meaning of a multi-key pin set — "MORE THAN ONE
// KEY IS RETURNED ONLY DURING A SIGNING-KEY ROLLOVER WINDOW ... It is NOT a
// general-purpose key list to be stuffed with anything we might like to accept."
// A rollover has exactly two participants, the outgoing key and the incoming
// one. A larger cap would silently license the general-purpose list that doc
// refuses, and it is also a per-message CPU bound: every extra pin is one more
// ed25519.Verify attempt on the inbound relay path.
const MaxPinnedBusSigningKeys = 2

// maxConfigSeq bounds the configuration sequence number.
//
// It is 2^53-1 because the record is JSON and the first tool an operator reaches
// for is `jq`, whose numbers are float64: above 2^53 the value an operator reads
// would stop being the value on disk. The counter advances at OPERATOR rate —
// one per configuration change — so this ceiling is unreachable in practice and
// exists so that the unreachable case is a loud refusal rather than a silent
// rounding.
const maxConfigSeq = uint64(1)<<53 - 1

// PeerTombstoneRetention is how long a REMOVED record is kept before it is
// swept.
//
// DERIVED from the only other constant in this system that says how long a peer
// stays relevant after it stops answering: idem.PeerOutageBudget (24h), the
// window inside which a peer's retries are still deduplicated. A tombstone must
// comfortably outlive the retry traffic of the peer it buried, and 30 budgets is
// a month — long enough to cover an operator removing a peer and rebuilding the
// machine, short enough that withdrawn records cannot permanently occupy the
// bounded table.
const PeerTombstoneRetention = 30 * idem.PeerOutageBudget

// Peer-store failures. All are checkable with errors.Is. The peer-shaped
// failures this file needs and peer.go/registry.go already define —
// ErrInvalidBusID, ErrBusIDCollision, ErrPeerBusIDCollision, ErrTooManyPeers,
// ErrUnknownPeer — are REUSED rather than duplicated under new names: one
// concept, one sentinel.
var (
	// ErrInvalidPeerRecord reports a record that is malformed or
	// self-contradictory, in either direction (see the validate methods).
	ErrInvalidPeerRecord = errors.New("relay: invalid peer configuration record")

	// ErrPeerNotDurable reports a mutating call on a PeerStore built without a
	// durable log. It is a REFUSAL, not a degraded in-memory mode: a pinned
	// signing key forgotten on restart is a trust anchor that silently stops
	// existing, and an operator would meet it as an unexplained verification
	// failure rather than as a refused write.
	ErrPeerNotDurable = errors.New("relay: peer store has no durable log")

	// ErrPeerConfigSeqExhausted reports the maxConfigSeq ceiling.
	ErrPeerConfigSeqExhausted = errors.New("relay: peer configuration sequence exhausted")

	// ErrPeerWithdrawn reports a record that a DURABLY RECORDED WITHDRAWAL
	// supersedes: it is at or below this bus's withdrawal floor for that bus, so
	// admitting it would reinstate a configuration the operator withdrew.
	//
	// It is a SEPARATE sentinel from ErrInvalidPeerRecord because the record is
	// not invalid and nothing is wrong with it — it is simply superseded, and on
	// a HEALTHY log it is superseded by a tombstone that is about to be replayed
	// two entries later. Apply logs it distinctly and at WARN for exactly that
	// reason; treating it as a corruption-grade ERROR would put an alarming line
	// in every normal boot's log and teach an operator to ignore the one that
	// matters. See busTable.withdrawnAt.
	ErrPeerWithdrawn = errors.New("relay: superseded by a durably recorded peer withdrawal")

	// ErrPeerNoWithdrawalFloor reports a withdrawal attempted on a store built
	// without a data directory.
	//
	// It is a REFUSAL and never a degraded mode, for the reason RELAY-34 exists:
	// a withdrawal recorded ONLY as a log entry can be un-said by losing that
	// entry, and for the trust table "un-said" means a revoked pinned bus signing
	// key comes back. Refusing to withdraw at all is the fail-CLOSED half of that
	// choice — the operator is told the revocation did not happen, rather than
	// being told it did and finding out otherwise after a torn write.
	ErrPeerNoWithdrawalFloor = errors.New("relay: peer store has no data directory, so a withdrawal cannot be made durable outside the log")

	// ErrTooManyPeerWithdrawals reports the maxPeerWithdrawalFloorEntries bound.
	ErrTooManyPeerWithdrawals = errors.New("relay: too many recorded peer withdrawals")

	// ErrPeerWithdrawalSeqTooHigh reports a withdrawal whose minted config_seq is
	// at or above maxPlausiblePeerWithdrawalSeq, so it cannot be recorded in the
	// durable withdrawal floor in a form this binary could read back.
	//
	// # It is a SEPARATE sentinel because the remedy is the OPPOSITE one
	//
	// A security-gate finding. This case used to wrap
	// ErrPeerWithdrawalFloorCorrupt, whose message says "the persisted peer
	// withdrawal floor is corrupt" and whose remedy is "move it aside and
	// restart". BOTH ARE FALSE HERE. The floor file is perfectly intact; what is
	// out of range is the sequence being minted, because some record in the LOG
	// carried an implausibly high config_seq and raised the counter (applyLocked
	// raises the high-water mark from every record, including ones it discards —
	// invariant 1 requires that). An operator following the corrupt-file remedy
	// would DELETE a healthy floor, permanently losing every revocation whose
	// tombstone had been swept, and STILL not be able to withdraw.
	//
	// The state is fail-closed for the FILE and fail-OPEN for the revocation —
	// the withdrawal is refused, so the pin is still served — which is why the
	// message has to name the real cause rather than send someone to the wrong
	// file. Reachable only by forging a WAL frame (the keyed HMAC means the
	// non-adversarial triggers can only DROP records), and an actor who can mint
	// frames already owns the trust table, so this is a diagnosis problem rather
	// than a new exposure.
	ErrPeerWithdrawalSeqTooHigh = errors.New("relay: this bus's configuration sequence is too high to record a withdrawal")

	// ErrPeerWithdrawalFloorCorrupt reports a peer-withdrawal-floor file that
	// EXISTS but does not verify: bad header, unknown version, checksum mismatch,
	// a malformed or duplicated entry, or a floor above maxConfigSeq.
	//
	// It is FATAL at construction and the file is NEVER regenerated, the same
	// posture ids.ErrSuffixFileCorrupt, wal.ErrIndexFloorCorrupt and
	// hub.ErrSeqFloorFileCorrupt take. Regenerating it means forgetting which
	// pins an operator revoked, silently — which is precisely the failure this
	// file exists to make impossible.
	//
	// # This does NOT contradict invariant 6
	//
	// Invariant 6 is about the LOG: recovery must always reach a running server
	// rather than refusing to boot over a damaged bus.wal, and it still does —
	// nothing here changes how a torn, bit-rotted or truncated log is repaired.
	// What refuses is a damaged IDENTITY file, which is the same narrow exception
	// already granted to bus-id, wal-mac.key, agent-suffixes, wal-index-floor and
	// message-seq-floor.
	//
	// A CRASH CAN NEVER PRODUCE THIS STATE. The write is temp file + fsync +
	// rename + directory fsync, so a reader sees the whole old file or the whole
	// new one, never a torn one. Corruption therefore means media damage or
	// tampering, and there is no benign cause to be generous to. The message
	// names a concrete one-step remedy, so a bus is never permanently bricked.
	ErrPeerWithdrawalFloorCorrupt = errors.New("relay: the persisted peer withdrawal floor is corrupt")
)

// PeerDurableLog is the two-phase write path, injected.
//
// It is an interface for the same reason invite.DurableLog is one: this store
// must be constructible without a data directory (for tests and for a read-only
// rebuild) and must record through internal/wal without this package deciding
// how the server assembled its log. It is satisfied by *wal.Log, and it is
// deliberately the SAME shape as invite.DurableLog so a server passes one value
// to both.
//
// wal.Log.Write runs the whole prepare -> fsync -> commit -> fsync -> Apply
// cycle and returns only once the entry is on stable storage, so an operation
// that returns a nil error here is DURABLE. Nothing in this file acknowledges
// anything before that (invariant 4).
type PeerDurableLog interface {
	Write(wal.Entry) (wal.Committed, error)
}

// ---------------------------------------------------------------------------
// The lifecycle state, shared by both records
// ---------------------------------------------------------------------------

// PeerRecordState is where a peer-configuration record is in its lifecycle. It
// is a CLOSED enum shared by both record kinds: a value outside these two is
// rejected in both directions.
//
// It is NOT named peerState — registry.go already owns that identifier for the
// in-memory serving entry, and two types one capital letter apart in one package
// is a trap.
type PeerRecordState uint8

const (
	// PeerRecordActive is a configured, usable record: a route with a base URL,
	// or a trust entry with at least one pinned key.
	PeerRecordActive PeerRecordState = iota + 1

	// PeerRecordRemoved is a configuration an operator has withdrawn. It is a
	// TOMBSTONE, not a deletion, and it deliberately carries no live
	// configuration at all — see busTable.upsert for what it holds on to.
	PeerRecordRemoved
)

// String returns the wire spelling of the state. It is the value that goes on
// disk, so it is a fixed string and not a number, for invite's reason: a numeric
// enum in a durable record is unreadable to the operator interpreting the log
// with `head -c` and a pretty-printer, and it silently changes meaning if the
// constants are ever reordered.
func (s PeerRecordState) String() string {
	switch s {
	case PeerRecordActive:
		return "active"
	case PeerRecordRemoved:
		return "removed"
	default:
		return fmt.Sprintf("PeerRecordState(%d)", uint8(s))
	}
}

// parsePeerState maps the wire spelling back onto a PeerRecordState. An
// unrecognised value is an error, never a default: guessing would turn a corrupt
// or future-format record into a plausible-looking active one, which is the
// direction that installs a routing target or a trust anchor nobody configured.
func parsePeerState(s string) (PeerRecordState, error) {
	switch s {
	case "active":
		return PeerRecordActive, nil
	case "removed":
		return PeerRecordRemoved, nil
	default:
		return 0, fmt.Errorf("%w: state %q is not one of active, removed", ErrInvalidPeerRecord, elidePeerText(s))
	}
}

// busScopedRecord is what the two records have in common, and it is the ONLY
// thing the shared table below knows about either of them: both are keyed by bus
// id, both carry a monotonic configuration sequence number, both have the same
// two-state lifecycle. Everything kind-specific — what a base URL is, what a
// pinned key is — stays in the record's own validate and Encode.
type busScopedRecord interface {
	recordBusID() string
	recordSeq() uint64
	recordState() PeerRecordState
	recordUpdatedAt() time.Time
	// sameGenerationAs reports whether other is the SAME record: the test for
	// "this is what I already have", used to keep a re-applied record silent
	// instead of alarming.
	sameGenerationAs(other busScopedRecord) bool
	// clone returns a deep copy sharing no mutable memory with the receiver.
	clone() busScopedRecord
}

// ---------------------------------------------------------------------------
// The ROUTING record
// ---------------------------------------------------------------------------

// PeerRecord is one peer bus's ROUTE as it exists on disk: which bus, which
// generation of the configuration, whether it is active, and where it lives.
//
// IT CARRIES NO KEY MATERIAL. See the package comment: trust is a separate
// record precisely because a bus may be trusted without being routable and
// routable without being trusted.
//
// EVERY DURABLE ENTRY CARRIES THE COMPLETE RECORD IN ITS POST-TRANSITION STATE,
// never a delta — invite's rule, load-bearing for the same two reasons: replay
// needs no ordering logic beyond a monotonic upsert, so there is no second
// mechanism that could disagree with the live path; and if an EARLIER record is
// discarded by recovery, a surviving LATER record still reconstructs the peer on
// its own, where a delta scheme would leave it holding a stale address.
type PeerRecord struct {
	// BusID is the peer's bus id, in its canonical spelling. It is the identity
	// of the record; there is no separate server-minted record id, because a bus
	// id already NAMES exactly one bus (invariant 2) and inventing a second
	// identifier for it would create two ways to say the same thing.
	BusID string

	// ConfigSeq is this bus's own configuration sequence number for this
	// generation. See PeerStore.configSeq and busTable.upsert for what it is,
	// what it guarantees, and why it is not a per-peer counter.
	ConfigSeq uint64

	// State is the lifecycle state. BaseURL is set if and only if State is
	// PeerRecordActive; validate enforces that in both directions.
	State PeerRecordState

	// BaseURL is where the peer lives: a BARE https origin — scheme, host and
	// optional port, and nothing else.
	//
	// It is OPERATOR CONFIGURATION and never something a peer asserts about
	// itself, the same point registry.SetPeerBaseURL makes: a peer-supplied base
	// URL would be an unauthenticated instruction telling us where to send every
	// message addressed to its agents.
	BaseURL string

	// NextHopTLSCertFingerprint pins the TLS certificate of the bus THAT ANSWERS
	// AT BaseURL — the NEXT HOP — and it is deliberately NOT keyed to BusID.
	//
	// # Why the name says NEXT HOP and says TLS
	//
	// Two traps meet on this field, and the name is what keeps a future reader
	// out of both.
	//
	// The first is the ROUTING one, and it is the reason this field exists in
	// this shape (cmd/agent-bus/peer.go's file comment, CONTRACTS-CLI.md):
	//
	//	a route record's BusID is the DESTINATION, and its BaseURL is the
	//	address of the NEXT HOP — for a non-adjacent destination those are
	//	DIFFERENT BUSES.
	//
	// `peer add -bus-id busB -url https://b:8443 -route-for busC` writes a
	// record whose BusID is busC and whose BaseURL is busB's address. The
	// certificate presented on a connection to that address is busB's, so a pin
	// keyed to the record's BusID would be pinning busC's identity against a
	// handshake that terminates at busB, and EVERY non-adjacent hop would be
	// refused. The field is therefore named NEXT HOP — the thing at BaseURL —
	// and the two fields sit next to each other so that a reader who copies one
	// thinks about the other.
	//
	// The second is a NAMING collision inside this package: peer.go's
	// peerFingerprint and idem.Fingerprint are the IDEMPOTENCY fingerprint of a
	// roster payload — a replay-protection digest over request bytes, with no
	// relation to transport or to any certificate. That is why this one says
	// TLS: nothing here may be called a bare "fingerprint" without saying which
	// of the two it is.
	//
	// # Construction and optionality
	//
	// It is a buscert.Fingerprint — THE fingerprint of the design,
	// sha256.Sum256(leaf.Raw), the exact value buscert.FingerprintOf returns from
	// a live connection's certificate. It is that TYPE and not a []byte or a
	// string precisely so that whatever eventually compares it computes it the
	// same way and cannot accidentally agree with a second construction — which
	// would fail SILENTLY, refusing every peer as unknown with nothing reporting
	// a mismatch.
	//
	// # WHICH CERTIFICATE, IN WHICH DIRECTION — read this before consuming it
	//
	// This is the certificate the hop at BaseURL presents WHEN THIS BUS DIALS IT:
	// an OUTBOUND, SERVER-side certificate, keyed to an ADDRESS. It is NOT a
	// source of INBOUND peer identity. A peer's CLIENT certificate arriving on a
	// connection TO us (r.TLS.PeerCertificates[0]) is the mirror-image problem,
	// and nothing here or in MTLS-CLIENTAUTH establishes that the two are the
	// same certificate; binding an inbound client certificate to a peer principal
	// needs its OWN record.
	//
	// So this must NOT be inverted into a `fingerprint -> bus id` lookup to
	// answer that question. Next-hop keying deliberately puts ONE fingerprint on
	// N records with N DIFFERENT bus ids (fpB sits on busB's route and on busC's),
	// so fingerprint-first is ambiguous BY CONSTRUCTION and would resolve an
	// inbound busB connection to busC — a peer principal spoofed out of entirely
	// correct data read backwards. `BaseURL -> bus id` is the same trap in the
	// other field. The sound direction is address-first and outbound only: "I am
	// dialling this address; does the certificate I was served match THIS
	// record's pin?" — and because the pin is duplicated across every record
	// sharing an address and can diverge, read each record's own pin rather than
	// caching one per address.
	//
	// The ZERO VALUE MEANS ABSENT, following invite.Record.CertFingerprint. It is
	// OPTIONAL on an active route and FORBIDDEN on a tombstone: validate enforces
	// "a pin only where there is a hop to pin", in both the encode and the decode
	// direction. The converse — requiring a pin on every active route — is
	// deliberately NOT enforced, because every route record already on disk was
	// written without one and a mandatory field would make an existing federation
	// undecodable at replay.
	NextHopTLSCertFingerprint buscert.Fingerprint

	// UpdatedAt is when this generation was written. For a tombstone it is also
	// the input to PeerTombstoneRetention, so a removed record without one could
	// never be swept.
	UpdatedAt time.Time
}

func (r PeerRecord) recordBusID() string          { return r.BusID }
func (r PeerRecord) recordSeq() uint64            { return r.ConfigSeq }
func (r PeerRecord) recordState() PeerRecordState { return r.State }
func (r PeerRecord) recordUpdatedAt() time.Time   { return r.UpdatedAt }

// clone is a plain copy: a PeerRecord holds no slice or map, so there is no
// backing array a caller could reach into.
func (r PeerRecord) clone() busScopedRecord { return r }

// sameGenerationAs compares EVERY configured field, the TLS pin included. It
// must: this predicate is what keeps a re-applied record silent, so a field it
// ignored would let a record differing only in that field be folded in as "what
// I already have" — and for this field that is a stale certificate pin surviving
// a rotation.
func (r PeerRecord) sameGenerationAs(o busScopedRecord) bool {
	other, ok := o.(PeerRecord)
	return ok && r.State == other.State && r.BaseURL == other.BaseURL &&
		r.NextHopTLSCertFingerprint == other.NextHopTLSCertFingerprint &&
		r.UpdatedAt.Equal(other.UpdatedAt)
}

// peerRecordJSON is the routing record's wire shape.
//
// "rec" REPEATS the wal.Entry.Kind inside the body, deliberately. Without it the
// two kinds' TOMBSTONES are byte-identical — both are {v, bus_id, config_seq,
// state:"removed", updated_at} — so a Kind mix-up in future wiring would land a
// route withdrawal in the trust table with no decode error at all, silently
// un-pinning a bus. It costs eleven bytes and turns that class of bug into a
// refusal at the door.
//
// THE VERSION IS NOT BUMPED BY next_hop_tls_cert_sha256, and that is a decision
// rather than an omission. The field is ADDITIVE and OPTIONAL, so this binary
// reads every record ever written by an older one. A bump would do the opposite
// of what it looks like it does: DecodePeerRecord requires the version to EQUAL
// PeerRecordVersion, so raising it to 2 would make this binary refuse every v1
// record on disk — it would not be a compatibility marker, it would be a
// migration nobody asked for. The cost of not bumping is stated where an
// operator can act on it (CONTRACTS-ONDISK.md): an OLDER binary reading a record
// that carries this field refuses it as an unknown field, so a downgrade after
// pinning a next-hop certificate is not supported.
type peerRecordJSON struct {
	Version   int    `json:"v"`
	Rec       string `json:"rec"`
	BusID     string `json:"bus_id"`
	ConfigSeq uint64 `json:"config_seq"`
	State     string `json:"state"`
	BaseURL   string `json:"base_url,omitempty"`

	// NextHopTLSCertSHA256 is the LOWERCASE HEX spelling of
	// PeerRecord.NextHopTLSCertFingerprint — buscert.Fingerprint.String(), the
	// one textual form (buscert.ParseFingerprint rejects every other spelling,
	// uppercase included). The key names the NEXT HOP, not the record's bus, so
	// that the constraint survives in the durable format and not only in a Go
	// doc comment.
	NextHopTLSCertSHA256 string `json:"next_hop_tls_cert_sha256,omitempty"`

	UpdatedAt string `json:"updated_at"`
}

// validate checks a PeerRecord is self-consistent.
//
// IT RUNS IN BOTH DIRECTIONS, and both matter for different reasons — invite's
// rule, restated because it is easy to drop half of it. On the way OUT (Encode,
// before the durable write) a record that cannot be stored fails the operation
// with NOTHING written, rather than being discovered at replay when the effect
// is already durable and every remaining option is bad. On the way IN (Decode) a
// record read off disk is UNTRUSTED INPUT even though this server wrote it —
// because "this server wrote it" is exactly the claim corruption disproves.
//
// It deliberately does NOT check the record against our OWN bus id: that is a
// fact about this server, not about the record, and it is enforced in
// PeerStore.applyLocked where the local id is known. Keeping it out of here
// means a record stays decodable and auditable by a tool that has no bus.
func (r PeerRecord) validate() error {
	if err := validateRecordHeader(r.BusID, r.ConfigSeq, r.UpdatedAt); err != nil {
		return err
	}
	switch r.State {
	case PeerRecordActive:
		if len(r.BaseURL) > MaxPeerBaseURLLen {
			return fmt.Errorf("%w: base URL is %d bytes, but a peer base URL is at most %d; it is not echoed here because it is oversized", ErrInvalidPeerRecord, len(r.BaseURL), MaxPeerBaseURLLen)
		}
		if r.BaseURL == "" {
			return fmt.Errorf("%w: an active peer route must say where the peer lives", ErrInvalidPeerRecord)
		}
		if err := validateBareHTTPSOrigin(r.BaseURL); err != nil {
			return err
		}
	case PeerRecordRemoved:
		// A tombstone carries NO live configuration, checked rather than
		// trusted: a "removed" record that still carried an address would be
		// exactly the shape a resurrection wants.
		if r.BaseURL != "" {
			// Not echoed: on a record that should not have one it is either
			// corruption or an injected value, and quoting it back puts
			// attacker-chosen text into an operator's log.
			return fmt.Errorf("%w: a removed peer route carries a base URL (%d bytes); a tombstone holds no live configuration", ErrInvalidPeerRecord, len(r.BaseURL))
		}
		// The pin is live configuration too, and it is refused here for a
		// sharper reason than symmetry: a pin is a property of the HOP AT
		// BaseURL, and a tombstone has no BaseURL, so a fingerprint on one names
		// a hop that is not there. Left admissible it would also be the second
		// half of a resurrection — an address and the credential to trust it.
		// The value is not echoed even though it is public: on a record that
		// should not have one it is either corruption or injected.
		if r.NextHopTLSCertFingerprint != (buscert.Fingerprint{}) {
			return fmt.Errorf("%w: a removed peer route carries a next-hop TLS certificate fingerprint; a tombstone holds no live configuration, and a pin is a property of the address it has just given up", ErrInvalidPeerRecord)
		}
	default:
		return fmt.Errorf("%w: %s is not one of the fixed lifecycle states", ErrInvalidPeerRecord, r.State)
	}
	return nil
}

// Encode renders the routing record as the opaque JSON that rides in
// wal.Entry.Body. IT VALIDATES BEFORE IT RETURNS, so a record that cannot be
// stored fails the operation with nothing written.
func (r PeerRecord) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	j := peerRecordJSON{
		Version:   PeerRecordVersion,
		Rec:       PeerRecordKind,
		BusID:     r.BusID,
		ConfigSeq: r.ConfigSeq,
		State:     r.State.String(),
		UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	// The state-owned fields are written ONLY for the state that owns them, so
	// the encoder cannot produce a record its own validate would refuse on the
	// way back in. The pin travels with the address it is a pin FOR: they are
	// written under one condition because they are one fact about one hop.
	if r.State == PeerRecordActive {
		j.BaseURL = r.BaseURL
		if r.NextHopTLSCertFingerprint != (buscert.Fingerprint{}) {
			// String() is buscert's one textual form. The field is omitempty, so
			// a route with no pin is byte-identical on disk to one written before
			// this field existed — which is what keeps the change additive.
			j.NextHopTLSCertSHA256 = r.NextHopTLSCertFingerprint.String()
		}
	}
	return encodePeerJSON(j)
}

// DecodePeerRecord parses a routing record read back off disk.
//
// It is STRICT in exactly the way invite.DecodeRecord and wal.decodePayload are:
// unknown fields are refused, trailing data is refused, the version must be the
// one this binary implements, and every field is re-validated. A lenient decoder
// here would reinstate a peer at a mangled address.
func DecodePeerRecord(b []byte) (PeerRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j peerRecordJSON
	if err := dec.Decode(&j); err != nil {
		return PeerRecord{}, fmt.Errorf("%w: %v", ErrInvalidPeerRecord, err)
	}
	if dec.More() {
		return PeerRecord{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidPeerRecord)
	}
	if j.Version != PeerRecordVersion {
		return PeerRecord{}, fmt.Errorf("%w: record version %d, but this binary implements version %d", ErrInvalidPeerRecord, j.Version, PeerRecordVersion)
	}
	if j.Rec != PeerRecordKind {
		return PeerRecord{}, fmt.Errorf("%w: the body says it is a %q record, but it is being read as a %q record; the entry kind and the body disagree", ErrInvalidPeerRecord, elidePeerText(j.Rec), PeerRecordKind)
	}
	state, err := parsePeerState(j.State)
	if err != nil {
		return PeerRecord{}, err
	}
	updatedAt, err := parsePeerTime(j.UpdatedAt)
	if err != nil {
		return PeerRecord{}, err
	}
	// ABSENT stays absent — a record written before this field existed decodes
	// to the zero fingerprint, which validate reads as "no pin". Present is
	// parsed by buscert.ParseFingerprint and by nothing else: it is the same
	// strict 64-lowercase-hex rule the invite blob and the CLI flag use, so a
	// record whose pin was hand-edited into a different spelling is refused here
	// rather than silently failing to match a live certificate later.
	var fp buscert.Fingerprint
	if j.NextHopTLSCertSHA256 != "" {
		parsed, ferr := buscert.ParseFingerprint(j.NextHopTLSCertSHA256)
		if ferr != nil {
			// The offending text is NOT echoed: it is file-derived and its only
			// relevant property is that it is not a fingerprint. buscert's own
			// error quotes no input either.
			return PeerRecord{}, fmt.Errorf("%w: next_hop_tls_cert_sha256 is not a certificate fingerprint: %v", ErrInvalidPeerRecord, ferr)
		}
		// AN EXPLICIT 64 ZEROS IS REFUSED, matching the all-zero refusal this
		// package already applies to a pinned signing key — and here for a
		// sharper reason: the zero value is this record's marker for NO PIN, so
		// a present-but-zero field would decode to a hop that reads as unpinned
		// while the bytes on disk say an operator pinned something. That is the
		// fail-silent direction, and a record that cannot mean two things is
		// cheaper than a reader that has to know which one it meant.
		if parsed == (buscert.Fingerprint{}) {
			return PeerRecord{}, fmt.Errorf("%w: next_hop_tls_cert_sha256 is all zero; the field is omitted when a hop is unpinned, so a present all-zero value is either corruption or an injected 'pinned to nothing'", ErrInvalidPeerRecord)
		}
		fp = parsed
	}
	r := PeerRecord{
		BusID:                     j.BusID,
		ConfigSeq:                 j.ConfigSeq,
		State:                     state,
		BaseURL:                   j.BaseURL,
		NextHopTLSCertFingerprint: fp,
		UpdatedAt:                 updatedAt,
	}
	if err := r.validate(); err != nil {
		return PeerRecord{}, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// The TRUST record
// ---------------------------------------------------------------------------

// BusTrustRecord is the set of Ed25519 BUS SIGNING keys pinned for one bus.
//
// IT CARRIES NO TRANSPORT. That is the point of splitting it from PeerRecord: in
// an A <-> B <-> C line, C pins A's signing key while having no route to A at
// all, because a message originating at A is verified by C against that pin and
// the intermediate bus is not allowed to vouch for it.
//
// THE KEYS ARE OPERATOR CONFIGURATION, copied out of band exactly as the invite
// blob carries a bus's TLS certificate fingerprint (DECISIONS.md, E6). A key
// learned from the network would be a trust anchor chosen by whoever we are
// trying to authenticate.
type BusTrustRecord struct {
	// BusID is the bus these pins are for — NOT necessarily a routing peer.
	BusID string

	// ConfigSeq is this bus's own configuration sequence number for this
	// generation. It shares one counter with PeerRecord, so every configuration
	// change this bus makes is totally ordered.
	ConfigSeq uint64

	// State is the lifecycle state. SigningKeys is non-empty if and only if
	// State is PeerRecordActive.
	State PeerRecordState

	// SigningKeys are the pinned bus signing keys, in the operator's order, at
	// most MaxPinnedBusSigningKeys of them and pairwise distinct.
	//
	// MORE THAN ONE MEANS A ROLLOVER IS IN PROGRESS and nothing else
	// (signed.go:178-182). It is not a general-purpose accept list.
	SigningKeys []ed25519.PublicKey

	// UpdatedAt is when this generation was written; on a tombstone it is also
	// the input to PeerTombstoneRetention.
	UpdatedAt time.Time
}

func (r BusTrustRecord) recordBusID() string          { return r.BusID }
func (r BusTrustRecord) recordSeq() uint64            { return r.ConfigSeq }
func (r BusTrustRecord) recordState() PeerRecordState { return r.State }
func (r BusTrustRecord) recordUpdatedAt() time.Time   { return r.UpdatedAt }

func (r BusTrustRecord) clone() busScopedRecord {
	out := r
	out.SigningKeys = copySigningKeys(r.SigningKeys)
	return out
}

func (r BusTrustRecord) sameGenerationAs(o busScopedRecord) bool {
	other, ok := o.(BusTrustRecord)
	return ok && r.State == other.State && r.UpdatedAt.Equal(other.UpdatedAt) && sameKeySet(r.SigningKeys, other.SigningKeys)
}

// busTrustRecordJSON is the trust record's wire shape. "rec" is there for the
// reason peerRecordJSON's is: the two kinds' tombstones would otherwise be
// byte-identical, and a Kind mix-up would silently un-pin a bus.
type busTrustRecordJSON struct {
	Version     int      `json:"v"`
	Rec         string   `json:"rec"`
	BusID       string   `json:"bus_id"`
	ConfigSeq   uint64   `json:"config_seq"`
	State       string   `json:"state"`
	SigningKeys []string `json:"bus_signing_keys,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

// validate checks a BusTrustRecord is self-consistent, in both directions, for
// PeerRecord.validate's reasons — and with more at stake, because what this
// record carries is the material other buses' signatures are verified against.
func (r BusTrustRecord) validate() error {
	if err := validateRecordHeader(r.BusID, r.ConfigSeq, r.UpdatedAt); err != nil {
		return err
	}
	switch r.State {
	case PeerRecordActive:
		if len(r.SigningKeys) == 0 {
			return fmt.Errorf("%w: an active trust record must pin at least one bus signing key; a trust anchor that verifies nothing is not a weaker anchor, it is an absent one wearing the name of a present one", ErrInvalidPeerRecord)
		}
		if len(r.SigningKeys) > MaxPinnedBusSigningKeys {
			return fmt.Errorf("%w: %d pinned bus signing keys, but at most %d may be pinned; more than one means a ROLLOVER is in progress (the outgoing key and the incoming one) and the pin set is not a general-purpose accept list", ErrInvalidPeerRecord, len(r.SigningKeys), MaxPinnedBusSigningKeys)
		}
		for i, k := range r.SigningKeys {
			if len(k) != ed25519.PublicKeySize {
				return fmt.Errorf("%w: pinned key %d is %d bytes, want exactly %d; a wrong-size key is refused HERE because ed25519.Verify PANICS on one", ErrInvalidPeerRecord, i, len(k), ed25519.PublicKeySize)
			}
			if isAllZero(k) {
				// An all-zero key is either an uninitialised field or
				// corruption, and it is also a small-order point. Refused rather
				// than stored: the far worse reading — that the key was simply
				// never set — must not be allowed to look normal.
				return fmt.Errorf("%w: pinned key %d is all zero, which is either an uninitialised field or corruption", ErrInvalidPeerRecord, i)
			}
			for j := 0; j < i; j++ {
				if bytes.Equal(r.SigningKeys[j], k) {
					// Refused rather than deduplicated: in a two-key set a
					// duplicate means the operator believes a rollover is in
					// progress when only one key is really pinned, and silently
					// collapsing it would hide that.
					return fmt.Errorf("%w: pinned key %d repeats pinned key %d; a pin set is a set", ErrInvalidPeerRecord, i, j)
				}
			}
		}
	case PeerRecordRemoved:
		if len(r.SigningKeys) != 0 {
			return fmt.Errorf("%w: a removed trust record still pins %d key(s); a tombstone holds no trust anchor", ErrInvalidPeerRecord, len(r.SigningKeys))
		}
	default:
		return fmt.Errorf("%w: %s is not one of the fixed lifecycle states", ErrInvalidPeerRecord, r.State)
	}
	return nil
}

// Encode renders the trust record as the opaque JSON that rides in
// wal.Entry.Body. It validates before it returns.
func (r BusTrustRecord) Encode() (json.RawMessage, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	j := busTrustRecordJSON{
		Version:   PeerRecordVersion,
		Rec:       BusTrustRecordKind,
		BusID:     r.BusID,
		ConfigSeq: r.ConfigSeq,
		State:     r.State.String(),
		UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if r.State == PeerRecordActive {
		j.SigningKeys = make([]string, 0, len(r.SigningKeys))
		for _, k := range r.SigningKeys {
			j.SigningKeys = append(j.SigningKeys, base64.StdEncoding.EncodeToString(k))
		}
	}
	return encodePeerJSON(j)
}

// DecodeBusTrustRecord parses a trust record read back off disk, as strictly as
// DecodePeerRecord — and this is the decoder that matters most: a lenient one
// here reinstates a TRUST ANCHOR nobody configured.
func DecodeBusTrustRecord(b []byte) (BusTrustRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var j busTrustRecordJSON
	if err := dec.Decode(&j); err != nil {
		return BusTrustRecord{}, fmt.Errorf("%w: %v", ErrInvalidPeerRecord, err)
	}
	if dec.More() {
		return BusTrustRecord{}, fmt.Errorf("%w: trailing data after the record", ErrInvalidPeerRecord)
	}
	if j.Version != PeerRecordVersion {
		return BusTrustRecord{}, fmt.Errorf("%w: record version %d, but this binary implements version %d", ErrInvalidPeerRecord, j.Version, PeerRecordVersion)
	}
	if j.Rec != BusTrustRecordKind {
		return BusTrustRecord{}, fmt.Errorf("%w: the body says it is a %q record, but it is being read as a %q record; the entry kind and the body disagree", ErrInvalidPeerRecord, elidePeerText(j.Rec), BusTrustRecordKind)
	}
	state, err := parsePeerState(j.State)
	if err != nil {
		return BusTrustRecord{}, err
	}
	updatedAt, err := parsePeerTime(j.UpdatedAt)
	if err != nil {
		return BusTrustRecord{}, err
	}
	r := BusTrustRecord{
		BusID:     j.BusID,
		ConfigSeq: j.ConfigSeq,
		State:     state,
		UpdatedAt: updatedAt,
	}
	// The list LENGTH is checked before any key is decoded, so a hostile record
	// cannot make us base64-decode an unbounded number of them first.
	if len(j.SigningKeys) > MaxPinnedBusSigningKeys {
		return BusTrustRecord{}, fmt.Errorf("%w: %d pinned bus signing keys, but at most %d may be pinned", ErrInvalidPeerRecord, len(j.SigningKeys), MaxPinnedBusSigningKeys)
	}
	for i, s := range j.SigningKeys {
		key, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			// The offending text is NOT echoed: it is file-derived, and its only
			// relevant property here is that it is not base64.
			return BusTrustRecord{}, fmt.Errorf("%w: pinned key %d is not base64: %v", ErrInvalidPeerRecord, i, err)
		}
		r.SigningKeys = append(r.SigningKeys, ed25519.PublicKey(key))
	}
	if err := r.validate(); err != nil {
		return BusTrustRecord{}, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Shared record helpers
// ---------------------------------------------------------------------------

// validateRecordHeader checks the three fields both records share. The bus id's
// LENGTH is checked before ids.ValidateBusID, whose message quotes the id: an
// oversized id off a damaged or hostile log must not get to choose the size of
// the line we log about rejecting it.
func validateRecordHeader(busID string, seq uint64, updatedAt time.Time) error {
	if len(busID) > MaxPeerBusIDLen {
		return fmt.Errorf("%w: bus id is %d bytes, but a bus id is at most %d; it is not echoed here because it is oversized", ErrInvalidPeerRecord, len(busID), MaxPeerBusIDLen)
	}
	if err := ids.ValidateBusID(busID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPeerRecord, err)
	}
	if seq == 0 {
		return fmt.Errorf("%w: config_seq is zero, which is the unset value; the first configuration this bus writes is 1", ErrInvalidPeerRecord)
	}
	if seq > maxConfigSeq {
		return fmt.Errorf("%w: config_seq %d exceeds %d, above which a JSON reader using float64 numbers would no longer read back the value that is on disk", ErrInvalidPeerRecord, seq, maxConfigSeq)
	}
	if updatedAt.IsZero() {
		return fmt.Errorf("%w: updated_at is the zero time, but tombstone retention is computed from it, so a removed record without one could never be swept", ErrInvalidPeerRecord)
	}
	return nil
}

// validateBareHTTPSOrigin enforces what a PERSISTED peer address is allowed to
// be: scheme, host, optional port, and NOTHING ELSE.
//
// It is stricter than the package's own peerURL, deliberately. peerURL rejects a
// query, a fragment and userinfo but ACCEPTS a path, so it would let
// "https://h.example/../../x" become durable and then be joined with
// PeerRelayPath at every dial for the rest of that peer's life. A rejected
// request is a moment; a persisted bad address is forever, and it is written
// once by an operator who will not look at it again. Tightening peerURL itself
// would change every caller in files this task does not own, so the strictness
// lives here, where the durable value is minted.
//
// The https requirement is invariant 11: there is no plaintext listener, and a
// roster, a sender id and a message body are exactly the material an on-path
// observer would want.
func validateBareHTTPSOrigin(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: the peer base URL is unparseable: %v", ErrInvalidPeerRecord, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: the peer base URL has scheme %q, but a bus-to-bus link is always https — there is no plaintext listener (invariant 11)", ErrInvalidPeerRecord, elidePeerText(u.Scheme))
	}
	if u.Host == "" {
		return fmt.Errorf("%w: the peer base URL has no host", ErrInvalidPeerRecord)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%w: the peer base URL carries a path (%d bytes); a stored peer address is a BARE ORIGIN, because the relay path is appended at every dial and a stored path would ride along on all of them", ErrInvalidPeerRecord, len(u.Path))
	}
	// ForceQuery is checked alongside RawQuery because it is the "https://h?"
	// case, where the query is EMPTY but the '?' is present: url.String()
	// re-emits the '?', so joining PeerRelayPath onto it would turn the relay
	// path into the query string and every dial would land on the peer's "/".
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil || u.Opaque != "" {
		return fmt.Errorf("%w: the peer base URL must be a bare origin: no query, no fragment, no userinfo", ErrInvalidPeerRecord)
	}
	return nil
}

// encodePeerJSON renders a record's wire struct compactly, without HTML escaping
// and without the trailing newline Encoder.Encode adds (the carrier is
// length-delimited and needs no terminator).
func encodePeerJSON(j interface{}) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPeerRecord, err)
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// parsePeerTime decodes one RFC3339Nano timestamp, normalised to UTC.
func parsePeerTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		// Quoted through elidePeerText: it is file-derived text of unbounded
		// length until validate has run.
		return time.Time{}, fmt.Errorf("%w: updated_at (%q) is not RFC3339Nano: %v", ErrInvalidPeerRecord, elidePeerText(v), err)
	}
	return t.UTC(), nil
}

// isAllZero reports whether b is entirely zero bytes.
//
// It uses crypto/subtle only for uniformity with the rest of this codebase's key
// handling; a public key is public, so the timing property is NOT load-bearing
// and nothing here should be read as implying that it is.
func isAllZero(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(b, make([]byte, len(b))) == 1
}

// copySigningKeys deep-copies a pin set, so a stored record shares no mutable
// memory with a caller. Applied on the way IN and on the way OUT of the table —
// auth.copyRosterEntry's discipline, and it matters more here: a caller holding
// the backing array of a stored record could mutate the serving copy of a PINNED
// TRUST ANCHOR without going anywhere near the write path.
func copySigningKeys(in []ed25519.PublicKey) []ed25519.PublicKey {
	if len(in) == 0 {
		// Normalised to nil so an empty-but-non-nil set and an absent one are
		// the same value, and a round trip through Encode/Decode cannot change
		// it.
		return nil
	}
	out := make([]ed25519.PublicKey, 0, len(in))
	for _, k := range in {
		out = append(out, append(ed25519.PublicKey(nil), k...))
	}
	return out
}

// sameKeySet reports whether two pin sets are equal, ORDER INCLUDED. Order is
// part of the record (the operator's order), so two sets differing only in order
// are two different generations and must not be mistaken for a re-applied one.
func sameKeySet(a, b []ed25519.PublicKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// maxElidedPeerChars bounds how much untrusted, file-derived text may appear in
// an error string, the same discipline wal's CorruptError applies: an operator's
// log must not be sizeable by whoever wrote the damaged bytes.
const maxElidedPeerChars = 64

// elidePeerText truncates untrusted text for inclusion in an error message.
func elidePeerText(s string) string {
	if len(s) <= maxElidedPeerChars {
		return s
	}
	// Truncated on a RUNE boundary: cutting mid-rune would put an invalid UTF-8
	// fragment in an operator's log, which %q then renders as an escape nobody
	// can read back to the original bytes.
	cut := maxElidedPeerChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}

// ---------------------------------------------------------------------------
// The durable WITHDRAWAL FLOOR — the mechanism that makes a revocation STICK
// ---------------------------------------------------------------------------

// PeerWithdrawalFloorFileName is the file within the data directory that holds
// the durable per-bus WITHDRAWAL FLOOR: for each table and each bus, the
// config_seq at which this bus last withdrew that configuration.
//
// # The defect it closes (RELAY-34), and why nothing inside the log could
//
// Every durable peer record carries the COMPLETE post-transition state rather
// than a delta. That is what makes replay a plain monotonic upsert with no
// ordering logic — and it is also what made a withdrawal LOSABLE. If recovery
// discards the tombstone (a torn tail, a bit-rotted frame, a filesystem or VM
// snapshot rolled back past it — all of which invariant 6 REQUIRES us to survive
// and boot from rather than refuse), the previous generation is the surviving
// truth and is reinstated. For routes that means an un-peered bus is routable
// again. FOR THE TRUST TABLE IT MEANS A REVOKED PINNED BUS SIGNING KEY IS
// PINNED AGAIN: revocation failed OPEN. The security gate reproduced exactly
// that by truncating eight bytes off a bus.wal tail, and
// TestPeerStoreTrustSurvivesATornWALTail is that reproduction, kept as a
// regression test.
//
// No amount of care INSIDE the log fixes it, and that is the whole reason this
// is a file. Quoting internal/wal/indexfloor.go, which states the principle:
// "A floor derived from the log drops whenever the log does. wal.RepairLog may
// truncate a tail, rewrite the middle, or move the whole file aside — and a
// number stored inside the thing being repaired inherits every repair." A
// withdrawal stored only as a log entry inherits every repair too.
//
// So this is the SAME SHAPE ids.DurableNameSuffixes (agent-suffixes), wal's
// indexFloor (wal-index-floor) and hub's seqFloorFile (message-seq-floor)
// already use, and it is chosen for their reason rather than by analogy: WRITE
// THE FLOOR AHEAD OF THE THING IT AUTHORISES, OUTSIDE THE LOG IT MUST OUTLIVE.
// PeerStore.write fsyncs floor[table][bus] = seq BEFORE it hands the withdrawal
// to the durable log, so a withdrawal that was ACKNOWLEDGED was necessarily
// floored first. It is invariant 4's ordering one layer down.
//
// # It cannot make anything fail OPEN, in either failure direction
//
//   - floor write fails    -> the log entry is never written, the withdrawal is
//     REFUSED, the operator is told, the old pins stand. Nothing was claimed.
//   - floor written, log write fails -> the floor stands alone. The pins are
//     already un-served (busTable.lookup consults the floor, not just the
//     table), and they stay un-served across every restart. Fail-CLOSED: fewer
//     keys trusted than the log alone would suggest, which is the direction
//     RELAY-34's brief requires when one has to be chosen.
//
// # SINGLE WRITER PER DATA DIRECTORY — an assumption, not a guarantee this
// # package makes
//
// The whole map is rewritten on every withdrawal, so TWO processes sharing a
// data directory would each rewrite it from its own view and the last rename
// would win — silently LOWERING a floor, which is the one operation this file
// must never perform. That assumption is not enforced here and cannot be: it is
// enforced one layer up, by the data-directory lock the server takes at startup
// (internal/dirlock), which the offline `agent-bus peer` subcommand also takes
// (DECISIONS.md, FEDERATION (e)). It is exactly the assumption
// ids.DurableNameSuffixes and hub.seqFloorFile already rest on, written down
// here because the consequence is worse: theirs skip numbers, this one forgets a
// revocation. NOTHING in this package may be used against a directory another
// process holds.
//
// It is EXPORTED because operators and CONTRACTS-ONDISK.md need to name it, and
// because the error a corrupt floor raises tells an operator to move exactly
// this path aside.
const PeerWithdrawalFloorFileName = "peer-withdrawal-floor"

// peerWithdrawalFloorMagic is the first token of the header line, spelled out in
// full so a stray file in a data directory is identifiable by `head -1` alone.
const peerWithdrawalFloorMagic = "agent-bus-peer-withdrawal-floor"

// peerWithdrawalFloorVersion is the on-disk format version of this file.
//
// It is RESERVED, not chosen: value 6 in the Spec Server `ondisk-format-version`
// namespace, reserved 2026-08-08 for RELAY-34 (1 and 2 are the WAL frame format,
// 3 is ids/agent-suffixes, 4 is wal/wal-index-floor, 5 is hub/message-seq-floor).
// Never pick one of these by eyeballing the list — that is the parallel-agent
// collision class CLAUDE.md names explicitly.
//
// Note the contrast with PeerRecordKind/BusTrustRecordKind, which needed NO
// reservation: those are free-form application discriminators inside a WAL
// entry's body. This is a FILE FORMAT, so it takes a number.
//
// An UNKNOWN version is a HARD ERROR, never a "read what you can". A file
// written by a newer binary may encode withdrawals this one cannot see, and
// reading it partially would forget a revocation — which is the one thing this
// file exists to make impossible.
const peerWithdrawalFloorVersion = 6

// maxPeerWithdrawalFloorEntries bounds how many (table, bus) withdrawals the
// file may hold, ACROSS BOTH TABLES.
//
// A bound is needed because the map is MONOTONIC: an entry may never be dropped,
// since the record it defends against is still sitting in an append-only log
// that is never compacted. So distinct-buses-ever-withdrawn grows without limit
// under enough operator churn, and an unbounded file read at startup is a
// memory-exhaustion shape however unlikely the traffic pattern.
//
// 4096 is DERIVED as 32x the live cap: a bus routes to at most MaxPeers (64)
// peers and pins at most MaxPeers, so 4096 is thirty-two complete turnovers of
// both tables. Every one of those is a MANUAL operator action through an offline
// subcommand under the dirlock, so the ceiling is unreachable in practice and
// exists so the unreachable case is a loud refusal rather than a silent
// unbounded file.
//
// Reaching it REFUSES A WITHDRAWAL, which is stated rather than glossed: it is
// the one place this design can refuse a revocation. It is still fail-closed in
// the sense that matters — the caller gets an error naming the remedy and the
// operator knows the revocation did not happen — but it is not "revocation
// always succeeds", and pretending otherwise would be the comfortable version.
const maxPeerWithdrawalFloorEntries = 4096

// maxPeerWithdrawalFloorFileSize bounds how much of the file is read into
// memory.
//
// DERIVED: a full file is maxPeerWithdrawalFloorEntries lines of
// "<token> <bus-id> <seq>", at most 5 + 1 + MaxPeerBusIDLen + 1 + 16 + 1 = 88
// bytes each, so about 352 KiB plus an 80-byte header. 1 MiB is roughly 3x that
// and far below anything that matters, while still refusing a multi-gigabyte
// file planted to exhaust memory at startup — which is before anything has
// authenticated.
const maxPeerWithdrawalFloorFileSize int64 = 1 << 20

// maxPlausiblePeerWithdrawalSeq is the highest config_seq a REAL bus could ever
// have withdrawn at, and anything at or above it is treated as tampered-or-
// damaged rather than adopted. It is hub.maxPlausibleSeqFloor's idea applied to
// this counter, and it was added because the security gate demonstrated the
// failure it prevents.
//
// # Why a bound is needed when the file already has a digest
//
// The digest is UNKEYED, so it is an integrity check and not an authentication
// one: anyone who can write the data directory can recompute it. NewPeerStore
// SEEDS configSeq from these floors, so a single planted entry at maxConfigSeq
// seeds the counter to its ceiling — and then every Put, PutTrust, Remove and
// RemoveTrust, for every bus, in BOTH tables, fails with
// ErrPeerConfigSeqExhausted naming a ceiling nobody reached. The bus starts
// perfectly healthy, reads fine, and can never be reconfigured again, across
// every restart, because the file persists. That is the worst failure shape
// available here: total loss of function with the diagnosis pointing somewhere
// else entirely.
//
// # Why 2^32, and why the bound does not need to be tight
//
// The bound's only job is to separate "a value a real bus reached" from "a value
// only a tamperer or catastrophic corruption produces", and a false positive
// refuses a start — so it is set generously.
//
// config_seq advances at OPERATOR rate: one per configuration change, typed
// through an offline subcommand under the dirlock. 2^32 is 4.29 billion of them.
// At one configuration change EVERY SECOND, sustained, without pause, reaching
// it takes about 136 years. Meanwhile it leaves more than 9.0e15 numbers between
// it and maxConfigSeq, so a value that passes this bound cannot bring exhaustion
// within reach either: a tamperer gains nothing by picking the largest value
// that still passes.
//
// The honest caveat: only the READ is bounded. A bus that genuinely wrote a
// withdrawal above this would refuse its next start with a message saying
// "tampered", which would be the wrong word — but it is 136 years of continuous
// operator input away.
const maxPlausiblePeerWithdrawalSeq = uint64(1) << 32

// The stable on-disk TOKENS for the two tables. They are deliberately NOT
// busTable.what ("peer route", "bus trust"): what is prose for log lines and may
// be reworded, and a durable key that can be reworded by an edit to a log
// message is a durable key that can silently forget every revocation recorded
// under the old spelling.
const (
	routeTableToken = "route"
	trustTableToken = "trust"
)

// peerWithdrawalEntry is one line of the file: which table, which bus (in its
// FOLDED spelling, matching busTable's key), and the config_seq of the
// withdrawal.
type peerWithdrawalEntry struct {
	table string
	busID string
	seq   uint64
}

// encodePeerWithdrawalFloors renders the canonical on-disk form:
//
//	agent-bus-peer-withdrawal-floor v6 sha256=<hex of the body>
//	route <bus-id> <config_seq>
//	trust <bus-id> <config_seq>
//	...
//
// The digest covers the BODY, and entries are sorted by (table, bus id), so the
// bytes are a function of the withdrawal set alone — which is what makes the
// checksum meaningful and the file diffable and readable by eye. Numbers are
// canonical decimal (no sign, no leading zeros) so a floor has exactly one
// spelling.
//
// Bus ids are VALIDATED HERE, at the last point before an irreversible write,
// and this is not decoration. A bus id carrying a space would produce a file
// readPeerWithdrawalFloors rejects, and one carrying a newline would forge an
// entry for another bus — and because a rejected floor file is NEVER regenerated
// (ErrPeerWithdrawalFloorCorrupt), either would strand the data directory. They
// are written FOLDED and required to already be folded, so the file cannot hold
// two spellings of one bus and a reader cannot take the lower of them.
//
// # The digest is INTEGRITY, not AUTHENTICATION — and that is a KNOWN asymmetry
//
// It is an unkeyed SHA-256, exactly like agent-suffixes and message-seq-floor,
// and unlike wal-index-floor's keyed HMAC. Anyone who can write the data
// directory can recompute it. That asymmetry across the four floor files is a
// REAL open question with its own task; RELAY-34 deliberately does not resolve
// it here, because picking a side would be a crypto decision made as a side
// effect of a durability fix, and invariant 9 is explicit that crypto is never
// the incidental part of a change. What is claimed is what is true: this defends
// the data directory's INTEGRITY against media damage and accidental editing.
// Its authenticity is defended one layer up, at the directory, by
// enforceDataDirPermissions (cmd/agent-bus/datadirperm.go) and the dirlock.
//
// Note also which direction tampering runs here. Forging a floor HIGH un-pins a
// bus — fail-closed, visible, and repairable by re-pinning. Forging it LOW, or
// deleting the file, restores exactly the pre-RELAY-34 behaviour and no worse.
// Neither is a new capability for anyone who can already rewrite the directory.
func encodePeerWithdrawalFloors(entries []peerWithdrawalEntry) ([]byte, error) {
	sorted := make([]peerWithdrawalEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].table != sorted[j].table {
			return sorted[i].table < sorted[j].table
		}
		return sorted[i].busID < sorted[j].busID
	})

	var body bytes.Buffer
	for i, e := range sorted {
		if e.table != routeTableToken && e.table != trustTableToken {
			return nil, fmt.Errorf("%w: refusing to persist a withdrawal for table %q, which is neither %q nor %q", ErrPeerWithdrawalFloorCorrupt, elidePeerText(e.table), routeTableToken, trustTableToken)
		}
		if err := ids.ValidateBusID(e.busID); err != nil {
			return nil, fmt.Errorf("%w: refusing to persist a withdrawal floor: %v; a floor file holding that bus id could never be read back, and a floor file that cannot be read back is never regenerated, so writing it would permanently strand this data directory", ErrPeerWithdrawalFloorCorrupt, err)
		}
		if e.busID != strings.ToLower(e.busID) {
			return nil, fmt.Errorf("%w: refusing to persist a withdrawal floor for %q: the file holds FOLDED bus ids, so that two spellings of one bus cannot become two floors and a reader cannot take the lower", ErrPeerWithdrawalFloorCorrupt, elidePeerText(e.busID))
		}
		// THE WRITE SIDE REFUSES EXACTLY WHAT THE READ SIDE REFUSES, and the
		// bound is maxPlausiblePeerWithdrawalSeq rather than maxConfigSeq.
		//
		// A security-gate finding, and the failure it prevents is the worst in
		// this file. When the two bounds differed, this writer could persist a
		// file parsePeerWithdrawalEntry refuses — so the next start failed with
		// ErrPeerWithdrawalFloorCorrupt, and following the remedy it prints
		// (move it aside, restart) had reconcileWithdrawalFloor re-derive the
		// same out-of-range value from the tombstone still in the log and write
		// the identical unreadable file. A bus that never boots again, with a
		// remedy that LOOPS. That is the same class the bus-id check above
		// exists for, and it is why the two bounds must move together.
		//
		// Refusing here is fail-CLOSED and keeps the bus bootable: the
		// withdrawal fails loudly, the operator is told, and nothing unreadable
		// reaches the disk.
		if e.seq == 0 || e.seq >= maxPlausiblePeerWithdrawalSeq {
			return nil, fmt.Errorf("%w: refusing to persist a withdrawal floor of %d for %s; a config_seq recorded here is between 1 and %d, and persisting one this reader would refuse would leave a data directory that cannot start and whose printed remedy rebuilds the same unreadable file", ErrPeerWithdrawalFloorCorrupt, e.seq, e.busID, maxPlausiblePeerWithdrawalSeq-1)
		}
		if i > 0 && sorted[i-1].table == e.table && sorted[i-1].busID == e.busID {
			return nil, fmt.Errorf("%w: refusing to persist two withdrawal floors for %s in the %s table; there is exactly one floor per (table, bus) and a reader that took either could take the lower", ErrPeerWithdrawalFloorCorrupt, e.busID, e.table)
		}
		body.WriteString(e.table)
		body.WriteByte(' ')
		body.WriteString(e.busID)
		body.WriteByte(' ')
		body.WriteString(strconv.FormatUint(e.seq, 10))
		body.WriteByte('\n')
	}

	sum := sha256.Sum256(body.Bytes())

	var out bytes.Buffer
	out.WriteString(peerWithdrawalFloorMagic)
	out.WriteString(" v")
	out.WriteString(strconv.Itoa(peerWithdrawalFloorVersion))
	out.WriteString(" sha256=")
	out.WriteString(hex.EncodeToString(sum[:]))
	out.WriteByte('\n')
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// readPeerWithdrawalFloors loads and verifies the file, returning one map per
// table token.
//
// A MISSING file yields empty maps and a NIL error: that is a data directory
// that has never withdrawn a peer configuration, which is every data directory
// in existence at the time this landed (nothing outside internal/relay had
// constructed a PeerStore, so no data directory holds a "peer" or "bustrust"
// record at all). There is therefore NO migration window in which a real
// withdrawal predates the file — which is exactly the window that would
// otherwise be fail-open, and it is closed by arithmetic rather than by hope.
//
// Every other failure is FATAL and wraps ErrPeerWithdrawalFloorCorrupt, except
// an I/O failure — permission denied, a device error — which is returned AS-IS
// and NOT dressed up as a corruption claim. "I could not read the file" and "the
// file is not what was written" call for different operator actions.
//
// The SHA-256 is checked BEFORE any entry is parsed. A digest that does not
// match means the bytes are not the bytes that were written, so a floor read out
// of them could be LOWER than the one persisted — the silent rewind this whole
// mechanism exists to prevent.
func readPeerWithdrawalFloors(path string) (map[string]map[string]uint64, error) {
	out := map[string]map[string]uint64{
		routeTableToken: {},
		trustTableToken: {},
	}

	// BOUNDED READ. This file is written by whoever can write the data
	// directory, so an unbounded os.ReadFile on a multi-gigabyte
	// "peer-withdrawal-floor" would be a trivial memory exhaustion at startup.
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("relay: reading the durable peer withdrawal floor from %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPeerWithdrawalFloorFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("relay: reading the durable peer withdrawal floor from %s: %w", path, err)
	}
	if int64(len(data)) > maxPeerWithdrawalFloorFileSize {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("it is larger than %d bytes; a real floor file holds at most %d short lines, so this is damaged or planted and is NOT read into memory", maxPeerWithdrawalFloorFileSize, maxPeerWithdrawalFloorEntries))
	}

	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return nil, peerWithdrawalFloorCorrupt(path, "it has no header line")
	}
	header, body := string(data[:nl]), data[nl+1:]

	fields := strings.Split(header, " ")
	if len(fields) != 3 || fields[0] != peerWithdrawalFloorMagic {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("it does not start with a %q header line (got %q)", peerWithdrawalFloorMagic, elidePeerText(header)))
	}
	wantVersion := "v" + strconv.Itoa(peerWithdrawalFloorVersion)
	if fields[1] != wantVersion {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("it is on-disk format %s, but this binary understands only %s; a file written by a NEWER agent-bus may record withdrawals this binary cannot see, and reading it partially would FORGET a revocation — run the version of agent-bus that wrote it, or migrate the data directory deliberately", elidePeerText(fields[1]), wantVersion))
	}
	const sumPrefix = "sha256="
	if !strings.HasPrefix(fields[2], sumPrefix) {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("its header has no %s digest (got %q)", sumPrefix, elidePeerText(fields[2])))
	}
	want, derr := hex.DecodeString(strings.TrimPrefix(fields[2], sumPrefix))
	if derr != nil || len(want) != sha256.Size {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("its header digest %q is not %d hex bytes", elidePeerText(fields[2]), sha256.Size))
	}
	if got := sha256.Sum256(body); !bytes.Equal(got[:], want) {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("it fails its own checksum (header says %x, body hashes to %x), so it is not the bytes that were written and a floor read from it could FORGET a revocation this bus recorded", want, got[:]))
	}

	if len(body) == 0 {
		// A legal empty body: the file exists and records no withdrawals.
		return out, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) > maxPeerWithdrawalFloorEntries {
		return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("it holds %d entries, above the %d this binary retains", len(lines), maxPeerWithdrawalFloorEntries))
	}
	for i, line := range lines {
		e, perr := parsePeerWithdrawalEntry(line)
		if perr != nil {
			return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("entry %d: %v", i+1, perr))
		}
		if _, dup := out[e.table][e.busID]; dup {
			return nil, peerWithdrawalFloorCorrupt(path, fmt.Sprintf("entry %d lists %s in the %s table twice; there is exactly one floor per (table, bus) and a reader that took either could take the LOWER, forgetting a revocation", i+1, e.busID, e.table))
		}
		out[e.table][e.busID] = e.seq
	}
	return out, nil
}

// parsePeerWithdrawalEntry reads one "<table> <bus-id> <config_seq>" line.
//
// Every field is validated: the file is UNTRUSTED INPUT even though this bus
// wrote it, because "this bus wrote it" is exactly the claim corruption
// disproves. The bus id must be valid AND already folded, so a tampered file
// cannot introduce a second spelling of a bus whose floor a reader would then
// miss; the number must be canonical decimal within maxConfigSeq, so a floor has
// exactly one spelling and cannot be a value no config_seq could ever reach.
func parsePeerWithdrawalEntry(line string) (peerWithdrawalEntry, error) {
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return peerWithdrawalEntry{}, fmt.Errorf("expected %q, got %q", "<table> <bus-id> <config_seq>", elidePeerText(line))
	}
	table, busID, num := parts[0], parts[1], parts[2]
	if table != routeTableToken && table != trustTableToken {
		return peerWithdrawalEntry{}, fmt.Errorf("table %q is neither %q nor %q", elidePeerText(table), routeTableToken, trustTableToken)
	}
	if len(busID) > MaxPeerBusIDLen {
		return peerWithdrawalEntry{}, fmt.Errorf("its bus id is %d bytes, but a bus id is at most %d; it is not echoed here because it is oversized", len(busID), MaxPeerBusIDLen)
	}
	if err := ids.ValidateBusID(busID); err != nil {
		return peerWithdrawalEntry{}, err
	}
	if busID != strings.ToLower(busID) {
		return peerWithdrawalEntry{}, fmt.Errorf("bus id %q is not folded; the file holds FOLDED bus ids so that one bus has exactly one floor", elidePeerText(busID))
	}
	if num == "" {
		return peerWithdrawalEntry{}, fmt.Errorf("bus id %q has an empty config_seq", busID)
	}
	for i := 0; i < len(num); i++ {
		if c := num[i]; c < '0' || c > '9' {
			return peerWithdrawalEntry{}, fmt.Errorf("config_seq %q for %s must be decimal digits only", elidePeerText(num), busID)
		}
	}
	if len(num) > 1 && num[0] == '0' {
		return peerWithdrawalEntry{}, fmt.Errorf("config_seq %q for %s has a leading zero; a floor has exactly one spelling", elidePeerText(num), busID)
	}
	seq, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return peerWithdrawalEntry{}, fmt.Errorf("config_seq %q for %s is not a 64-bit decimal number: %v", elidePeerText(num), busID, err)
	}
	if seq == 0 {
		return peerWithdrawalEntry{}, fmt.Errorf("%s has floor 0, which is the unset value; the first configuration this bus writes is 1, so no withdrawal can be at 0", busID)
	}
	// THE PLAUSIBILITY BOUND, checked at the last point before the value becomes
	// a floor and deliberately a REFUSAL rather than a clamp. See
	// maxPlausiblePeerWithdrawalSeq: silently ADOPTING an implausible value seeds
	// configSeq to its ceiling and permanently refuses every write in BOTH tables
	// on a bus that otherwise looks completely healthy.
	if seq >= maxPlausiblePeerWithdrawalSeq {
		return peerWithdrawalEntry{}, fmt.Errorf("config_seq %d for %s is implausibly high: no bus reaches %d in any lifetime (that is roughly 136 years at one configuration change every second), so this file has been TAMPERED WITH or the media is damaged. Adopting it would seed the configuration counter near its ceiling and make EVERY peer and trust write fail with \"configuration sequence exhausted\", permanently and across every restart", seq, busID, maxPlausiblePeerWithdrawalSeq)
	}
	return peerWithdrawalEntry{table: table, busID: busID, seq: seq}, nil
}

// peerWithdrawalFloorCorrupt builds the fatal error, appending the SAME one-step
// operator remedy every time. The remedy matters as much as the diagnosis:
// without it this error is a permanently unstartable bus, which is precisely the
// shape invariant 6 forbids.
//
// The remedy is deliberately NOT a bare "delete it": deleting it FORGETS every
// revocation it recorded, and a forgotten revocation is a pinned bus signing key
// that quietly comes back. So it says so, in those words, and tells the operator
// what actually happens next.
//
// # CORRECTED by the security gate — the previous remedy silently did nothing
//
// It used to say "move it aside, restart, and RE-APPLY every withdrawal by hand".
// That instruction FAILED SILENTLY, and the gate reproduced it: the tombstone is
// still in the log, so RemoveTrust takes its already-removed no-op branch,
// returns success, and writes no floor. The operator was told the revocation was
// back in place while it was not. Worse, once the tombstone had been swept,
// RemoveTrust reported ErrUnknownPeer and there was no way to re-establish the
// floor at all.
//
// Both are now fixed in code rather than in prose — reconcileWithdrawalFloor
// repairs the floor from every withdrawal the log still holds, and re-applying a
// withdrawal re-asserts its floor — so the remedy below states what the restart
// really does, and confines the manual work to the withdrawals the log can no
// longer prove.
func peerWithdrawalFloorCorrupt(path, why string) error {
	return fmt.Errorf("%w: %s: %s. It will NOT be regenerated in place, because silently rebuilding it would FORGET which peer configurations this bus withdrew — and for the trust table a forgotten withdrawal is a REVOKED pinned bus signing key that comes back, with nothing downstream able to tell it from a key the operator configured. Move %s aside and restart: the bus REBUILDS the floor from every withdrawal its log still holds, logging each repair. Withdrawals whose log records are gone cannot be rebuilt, so re-apply those by hand (`agent-bus peer remove` / `remove-trust`) before trusting anything this bus pins",
		ErrPeerWithdrawalFloorCorrupt, path, why, path)
}

// atomicReplacePeerWithdrawalFloor writes data to path via a temp file in the
// SAME directory: the temp file is created, chmodded 0600, written, fsynced and
// closed, renamed into place, and then the directory itself is fsynced so the
// rename is durable. A reader therefore sees either the complete old file or the
// complete new one, never a torn one — which is what makes "a crash can never
// produce a corrupt floor file" a true statement rather than a hope.
//
// # It is a COPY, and the honest account of why (corrected by the reviewer gate)
//
// There are three other production copies of this sequence:
// internal/ids/atomicfile.go's atomicWriteFile (shared by ids/busid.go's
// writeBusIDFile, which is a one-line delegation to it, and by
// ids/suffixstore.go), internal/wal/indexfloor.go's atomicReplaceFile, and
// internal/hub/seqfloorfile.go's atomicReplaceSeqFloor. This is the fourth.
//
// An earlier version of this comment said "the FIFTH copy", named files that do
// not contain the code, and justified the duplication with the exact argument
// that Spec Server task 1aed37a9-3a8e-4940-8b36-ee2dbe28afb5 COMPLETED by
// removing — internal/ids/atomicfile.go:20-49 records that removal, and
// ids/atomicfile_test.go enforces single-copy inside package ids with an AST
// guard. Reinstating "duplication here is deliberate" as a principle would undo
// a finished task, so it is not claimed.
//
// What is true: the copy exists because ids.atomicWriteFile is UNEXPORTED, and
// this package must not import internal/wal (that would tie the floor to the
// very thing it exists to be independent of). Unifying them means EXPORTING a
// shared helper across four packages, which is a refactor with no behavioural
// content and is out of scope under CLAUDE.md's "do not refactor unless the task
// explicitly asks". It is reported as a follow-up by RELAY-34 rather than
// asserted to be already filed. Until then: every step below is load-bearing and
// each is a silent, undetectable-by-test omission if dropped — fsync the FILE
// before the rename, fsync the DIRECTORY after it, keep the temp file in dir so
// the rename is an intra-filesystem swap, and chmod 0600 before any content is
// written.
func atomicReplacePeerWithdrawalFloor(dir, path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".peer-withdrawal-floor-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}

	dirFile, derr := os.Open(dir)
	if derr != nil {
		err = fmt.Errorf("opening %s to fsync directory entry: %w", dir, derr)
		return err
	}
	defer dirFile.Close()
	if serr := dirFile.Sync(); serr != nil {
		err = fmt.Errorf("syncing directory %s: %w", dir, serr)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// The shared table
// ---------------------------------------------------------------------------

// busTable is one bounded, bus-id-keyed table of records with a monotonic
// upsert. There are two instances — routes and trust — and they share this code
// so the two cannot drift into different admission rules.
type busTable struct {
	// what names the table in every log line: "peer route" or "bus trust".
	what string

	// token is this table's STABLE on-disk spelling in the withdrawal floor file
	// ("route", "trust"). It is separate from what for the reason given at
	// routeTableToken: what is prose and may be reworded, and a durable key that
	// an edit to a log message can rename is a durable key that can silently
	// forget every revocation recorded under the old spelling.
	token string

	// entries is keyed on the LOWERCASED bus id — the same key Registry uses,
	// and for the same reason: two bus ids differing only by ASCII case must
	// land on one key so the second can be SEEN and refused. The canonical
	// spelling is kept inside the record.
	entries map[string]busScopedRecord

	// sweptMax is the ADMISSION FLOOR: the highest config_seq this table has
	// ever thrown away by sweeping a tombstone. It only rises.
	//
	// It exists because a tombstone does two jobs and the sweep only ends one of
	// them. While the tombstone is present, an older duplicate is refused by the
	// ordinary monotonicity check below. Once it is swept, the bus is UNKNOWN
	// again and the !known branch would insert whatever arrives — so a duplicated
	// or reordered older record would resurrect a withdrawn route at its old
	// address, or a REVOKED TRUST ANCHOR with its old pinned key. That is the
	// exact resurrection the tombstone exists to prevent, reappearing the moment
	// the tombstone expires.
	//
	// PeerStore.configSeq cannot serve as this floor, and an earlier version of
	// this file wrongly documented that it did: configSeq is a MINTING floor
	// (nothing is ever issued at or below it) and is raised by every record,
	// including the newest one, so testing an arriving record against it would
	// refuse the very record that had just raised it. sweptMax is raised ONLY by
	// a sweep, which is exactly the event that removes the other defence.
	//
	// IT CANNOT REFUSE A LEGITIMATE RECORD, and that rests on a property of the
	// write path rather than on good luck: PeerStore.writeMu serialises mint ->
	// durable write -> fold, so the order of records in the log IS the order
	// their sequences were minted in. An in-order replay therefore only ever
	// presents sequences above everything it has swept (a tombstone is swept only
	// while applying a LATER record), and a live write is minted above every
	// sequence ever applied. Without writeMu that is false, and both gates
	// reproduced the resulting permanent loss of an acknowledged write — see the
	// writeMu field.
	sweptMax uint64

	// withdrawnAt is the DURABLE PER-BUS WITHDRAWAL FLOOR, mirrored from
	// <data-dir>/peer-withdrawal-floor and keyed on the folded bus id: the
	// config_seq at which this bus last withdrew this table's configuration for
	// that bus. It only ever rises.
	//
	// # It is NOT a second sweptMax, and collapsing the two would reopen RELAY-34
	//
	// sweptMax defends against a record duplicated or reordered WITHIN a log that
	// still holds the tombstone (or held it until a sweep). It is rebuilt from
	// the log every start, so it drops whenever the log does. This map is read
	// from a file OUTSIDE the log at construction, so it survives exactly the
	// events sweptMax cannot: a discarded tail, a bit-rotted frame, a snapshot
	// rolled back past the withdrawal. That difference is the whole content of
	// RELAY-34 — see PeerWithdrawalFloorFileName.
	//
	// It is read under PeerStore.mu like every other field here, and written only
	// by PeerStore.recordWithdrawal, AFTER the file it mirrors is fsynced. Memory
	// therefore never claims a withdrawal disk does not hold.
	withdrawnAt map[string]uint64

	max       int
	retention time.Duration
}

func newBusTable(what, token string, max int, retention time.Duration) *busTable {
	return &busTable{
		what:        what,
		token:       token,
		entries:     make(map[string]busScopedRecord),
		withdrawnAt: make(map[string]uint64),
		max:         max,
		retention:   retention,
	}
}

// withdrawn reports whether rec is superseded by a durably recorded withdrawal:
// it is ACTIVE and at or below this table's withdrawal floor for its bus.
//
// A TOMBSTONE is never withdrawn-by-the-floor, and that is deliberate rather
// than an omission. The record that SET the floor is itself a tombstone at
// exactly that sequence, so blocking it would refuse the very write that had
// just been floored; and admitting an older tombstone reinstates nothing,
// because a tombstone carries no live configuration at all.
//
// The caller must hold PeerStore.mu.
func (t *busTable) withdrawn(rec busScopedRecord) bool {
	if rec.recordState() != PeerRecordActive {
		return false
	}
	return rec.recordSeq() <= t.withdrawnAt[strings.ToLower(rec.recordBusID())]
}

// upsert is the ONE place anything enters or changes a table, and it is
// MONOTONIC on the configuration sequence number.
//
// # WHAT MONOTONICITY IS KEYED ON, AND WHY THAT CHOICE IS SAFE
//
// It is keyed on ConfigSeq: strictly greater applies, equal is idempotent only
// if the record is the same generation, and lower is REFUSED and logged.
//
// It is NOT keyed on the lifecycle state, and that is the one place this design
// departs from invite.Store.upsertLocked. An invite's states are TERMINAL —
// open -> redeemed is a one-way door, and refusing to go back is the whole
// mechanism of single use. A PEER'S ARE NOT: an operator legitimately re-peers a
// bus they removed, rotates a signing key, or moves a peer to a new address, so
// "removed -> active" is ordinary and must be allowed. State-keyed monotonicity
// would either forbid re-peering — making removal unrecoverable short of wiping
// the log — or permit anything at all.
//
// It is NOT keyed on the timestamp either: clocks step backwards, and a trust
// anchor must not be decided by NTP.
//
// # WHAT IT BUYS
//
// The case it exists for: a record DUPLICATED or REORDERED on replay. Recovery
// is allowed to rewrite a log around damage and to discard a tail, so "the bytes
// replay hands us are exactly the bytes we wrote, in exactly the order we wrote
// them" is an assumption this package must not make. Without the sequence, an
// older surviving record applied after a newer one would silently replace a
// bus's PINNED SIGNING KEYS with a previous set — a DOWNGRADE OF A TRUST ANCHOR,
// indistinguishable downstream from a legitimate rotation.
//
// An EQUAL sequence carrying a DIFFERENT record is refused too, and the FIRST
// one wins. Between "keep what is already applied" and "accept an unexpected
// change to a pinned key", only the first is safe.
//
// # THE TOMBSTONE, AND THE FLOOR THAT OUTLIVES IT
//
// A withdrawn record is kept rather than deleted because it carries that bus's
// last sequence number forward: a duplicated older record arriving after a
// removal finds a tombstone at a HIGHER sequence and is refused, where an empty
// slot would have been filled with the resurrected address or pin set. Once the
// tombstone is SWEPT, that defence is gone and the !known branch would insert
// the duplicate — so the sweep hands the sequence to t.sweptMax, and the !known
// branch refuses anything at or below it. Both gates found the version of this
// file that had only the first half of that and documented it as having both.
func (t *busTable) upsert(rec busScopedRecord, now time.Time, log *logging.Logger, localBusID, source string) error {
	t.sweep(now, log, localBusID)

	busID := rec.recordBusID()
	folded := strings.ToLower(busID)

	// THE DURABLE WITHDRAWAL FLOOR, checked FIRST and on every path. This is
	// RELAY-34's mechanism and it is the only defence in this file that survives
	// the loss of the withdrawal record itself: t.withdrawnAt comes from a file
	// outside the log, so a discarded, bit-rotted or snapshot-rolled-back
	// tombstone cannot lower it. Without this the previous generation is the
	// surviving truth and is reinstated — for the trust table, a REVOKED PINNED
	// BUS SIGNING KEY.
	//
	// It cannot refuse a legitimate write: PeerStore.configSeq is seeded from
	// these floors at construction and raised by every applied record, so a live
	// write is minted strictly above every floor. What it refuses is a record
	// this bus already superseded with a withdrawal it made durable.
	if t.withdrawn(rec) {
		return fmt.Errorf("%w: refusing to reinstate the %s record for %s at config_seq %d: this bus durably recorded a WITHDRAWAL of it at config_seq %d, in %s, which is outside the log and therefore survives a discarded tail. The record carrying that withdrawal is not what is being replayed here, so either it is simply still to come (the healthy case, on an intact log) or it has been LOST — and reinstating this generation is how a revoked pinned bus signing key comes back (source %s)",
			ErrPeerWithdrawn, t.what, busID, rec.recordSeq(), t.withdrawnAt[folded], PeerWithdrawalFloorFileName, source)
	}

	existing, known := t.entries[folded]
	if !known {
		// THE ADMISSION FLOOR. See busTable.sweptMax: without this, a record
		// duplicated or reordered behind a SWEPT tombstone re-creates a
		// configuration the operator withdrew — for the trust table, a revoked
		// pinned signing key. It cannot refuse a legitimate write: a live write's
		// sequence is minted above every sequence ever applied, and an in-order
		// replay only ever presents sequences above everything it has swept.
		if rec.recordSeq() <= t.sweptMax {
			// The message states the MECHANISM and not a cause. The floor is
			// TABLE-WIDE, so the tombstone whose sequence set it may belong to a
			// different bus than the one named here — an earlier wording asserted
			// that this bus had been withdrawn, which was sometimes simply untrue
			// and sent an operator looking for a removal that never happened.
			return fmt.Errorf("%w: refusing a %s record for %s at config_seq %d: this table has swept a withdrawal tombstone at config_seq %d, and admitting an older record for a bus it no longer holds would RESURRECT a configuration that was withdrawn. Records reach this only out of order or duplicated; a write made by this bus is minted above every sequence ever applied (source %s)",
				ErrInvalidPeerRecord, t.what, busID, rec.recordSeq(), t.sweptMax, source)
		}
		// THE CAP IS ENFORCED ON EVERY PATH, INCLUDING REPLAY: it is a MEMORY
		// bound, and a bound one path could exceed is not a bound.
		//
		// AN ALREADY-DURABLE RECORD CAN REACH THIS REFUSAL, and saying otherwise
		// would be the comfortable version rather than the true one. A live write
		// checks the same bound under s.mu and holds writeMu through its fold, so
		// no other WRITE can take the slot in between — but the bound is only as
		// stable as the SWEEP that frees slots, and the sweep reads the clock. A
		// write admitted at live time because a tombstone had expired can be
		// refused at replay time on a corrected clock, where that tombstone is
		// once again inside its retention and the table is once again full. The
		// security gate reproduced it with a four-entry table and a two-hour skew.
		// It is fail-closed (the configuration is not restored, the operator
		// re-applies it), it is logged specifically, and it self-heals once the
		// clock is right — but it is a real way to lose an acknowledged write, so
		// it is written down rather than denied.
		if len(t.entries) >= t.max {
			return fmt.Errorf("%w: %d %s records are retained, the limit; this record is DISCARDED, so that configuration is not restored and must be re-applied by the operator", ErrTooManyPeers, t.max, t.what)
		}
		t.entries[folded] = rec.clone()
		return nil
	}

	// A bus id NEVER changes spelling. Two ids differing only by ASCII case are
	// two DIFFERENT buses as far as anything downstream is concerned, and loop
	// prevention compares hops case-insensitively — so admitting both would make
	// them indistinguishable to the split horizon while remaining distinct
	// routing targets.
	if existing.recordBusID() != busID {
		return fmt.Errorf("%w: this store already holds %s for %q, and %q differs from it only by ASCII case", ErrPeerBusIDCollision, t.what, existing.recordBusID(), busID)
	}

	switch {
	case rec.recordSeq() > existing.recordSeq():
		t.entries[folded] = rec.clone()
		return nil

	case rec.recordSeq() == existing.recordSeq():
		if existing.sameGenerationAs(rec) {
			// The same log replayed twice, or a live fold Apply already
			// performed. Idempotent.
			return nil
		}
		return fmt.Errorf("%w: %s for %s already holds a DIFFERENT record at config_seq %d; the first is kept, because accepting an unexpected change at a sequence already applied would let a damaged log rewrite a pinned bus signing key",
			ErrInvalidPeerRecord, t.what, busID, rec.recordSeq())

	default:
		return fmt.Errorf("%w: refusing a NON-MONOTONIC %s record for %s: config_seq %d is below the applied %d; an older record replacing a newer one would DOWNGRADE this bus's configuration — for a pin set, back to a signing key that nothing downstream could tell from a rotation (source %s)",
			ErrInvalidPeerRecord, t.what, busID, rec.recordSeq(), existing.recordSeq(), source)
	}
}

// sweep drops tombstones the clock has left behind, in BOTH directions, and
// hands each one's sequence to the admission floor on the way out.
//
// The predicate is PURE — the record's own updated_at against the clock — and it
// is evaluated identically on the live path and the replay path.
//
// # A tombstone stamped in the FUTURE is swept too
//
// The obvious rule ("older than retention") leaves a record stamped ahead of the
// clock in the table forever, because now.Sub(updated_at) is negative and stays
// that way. That is not hypothetical: write() stamps s.now(), so a machine whose
// clock is far ahead when an operator withdraws a peer — an NTP step, a restored
// VM snapshot — writes a permanently unsweepable tombstone with nobody having
// touched the log. Enough of them and the bounded table is full and every new
// peering is refused. The symmetric rule costs nothing, because ACTIVE records
// are never swept either way: the only thing a wrong clock can drop is a
// WITHDRAWAL, and dropping a withdrawal is safe precisely because sweptMax keeps
// its sequence.
//
// # IT IS SAFE TO RUN DURING REPLAY
//
// Worth stating because the obvious worry is that a skewed clock makes recovery
// non-deterministic. It does not, because whatever the clock does, an older
// record is refused: by the tombstone if it is still there, and by t.sweptMax if
// the sweep has just removed it. What the clock changes is only whether an
// expired tombstone still occupies a slot.
// TestPeerStoreReplayIsClockIndependent replays a history that INCLUDES a
// duplicated older record under two clocks a decade apart and requires the same
// recovered state; TestPeerStoreASweptTombstoneStillRefusesAnOlderRecord pins
// the floor on its own.
//
// An ACTIVE record is NEVER swept. A configuration stays until an operator
// withdraws it, however long that is; expiring one silently would be a bus that
// stops federating for no reason an operator could see.
func (t *busTable) sweep(now time.Time, log *logging.Logger, localBusID string) {
	for folded, rec := range t.entries {
		if rec.recordState() != PeerRecordRemoved {
			continue
		}
		// COMPARED AS TIMES, NOT AS A DURATION. time.Duration saturates at
		// +/-292 years, and at the saturation point negating it overflows back to
		// the same value — so a duration-based symmetric test silently stops
		// firing exactly where it is needed most (a year-9999 stamp), which is
		// the defect the symmetric rule was added to close.
		at := rec.recordUpdatedAt()
		if !at.Before(now.Add(-t.retention)) && !at.After(now.Add(t.retention)) {
			continue
		}
		delete(t.entries, folded)
		// The sequence outlives the record: this is what stops a duplicated
		// older record filling the slot the sweep just emptied.
		if n := rec.recordSeq(); n > t.sweptMax {
			t.sweptMax = n
		}
		reason := "past its retention"
		if at.After(now) {
			reason = "stamped AHEAD of this clock by more than the retention, which would otherwise make it unsweepable forever"
		}
		log.Info("dropping a withdrawn peer-configuration tombstone",
			"local_bus", localBusID, "table", t.what, "peer_bus", rec.recordBusID(),
			"config_seq", rec.recordSeq(), "reason", reason,
			"removed_at", rec.recordUpdatedAt().UTC().Format(time.RFC3339Nano),
			"retention", t.retention.String())
	}
}

// lookup returns the record for a bus id in its canonical spelling only,
// matching Registry.Route: the table is keyed case-insensitively so a confusable
// can be REFUSED at the door, not so that both spellings resolve.
//
// A record the DURABLE WITHDRAWAL FLOOR supersedes is reported ABSENT. That is
// the belt to upsert's braces and it covers a window upsert cannot: PeerStore's
// write path fsyncs the floor BEFORE it hands the withdrawal to the log, so if
// the log write then fails the floor stands alone while the table still holds
// the generation being withdrawn. Serving that generation would be exactly the
// fail-open this whole mechanism exists to close, so every reader — Lookup,
// LookupTrust and therefore PinnedKeys — goes through here and sees nothing.
//
// The caller must hold PeerStore.mu.
func (t *busTable) lookup(busID string) (busScopedRecord, bool) {
	rec, ok := t.entries[strings.ToLower(busID)]
	if !ok || rec.recordBusID() != busID {
		return nil, false
	}
	if t.withdrawn(rec) {
		return nil, false
	}
	return rec, true
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

// PeerConfig is one operator instruction: route to this bus at this address.
type PeerConfig struct {
	// BusID is the peer's bus id. It is validated against OUR id
	// (ValidatePeerBusID): a peer may never assert our own namespace.
	//
	// FOR A STATIC NEXT-HOP ROUTE THIS IS THE DESTINATION, and BaseURL below is
	// the address of the intermediate bus that carries the traffic. The two name
	// different buses, which is exactly why the pin is keyed to the address.
	BusID string

	// BaseURL is a BARE https origin — scheme, host, optional port.
	BaseURL string

	// NextHopTLSCertFingerprint pins the certificate of whatever answers at
	// BaseURL. Optional; the zero value means no pin. See
	// PeerRecord.NextHopTLSCertFingerprint for why it is keyed to BaseURL and
	// never to BusID.
	NextHopTLSCertFingerprint buscert.Fingerprint
}

// BusTrust is one operator instruction: pin these bus signing keys for this bus.
//
// The bus NEED NOT BE A ROUTING PEER. Pinning a non-adjacent origin bus is the
// case this record exists for.
type BusTrust struct {
	// BusID is the bus these pins are for.
	BusID string

	// SigningKeys are the pins, in the operator's order. Two only during a
	// rollover.
	SigningKeys []ed25519.PublicKey
}

// PeerStoreOptions configures NewPeerStore. Every zero value means "the derived
// default", so a caller with no opinion gets the derivation rather than an
// accidental zero window.
type PeerStoreOptions struct {
	// BusID is THIS bus's server-minted id. It is what every record is measured
	// against, and it is required.
	BusID string

	// Durable is the two-phase write path. A nil Durable makes every mutating
	// operation fail with ErrPeerNotDurable — see that error for why this is a
	// refusal and not a degraded in-memory mode. A store built without one can
	// still Apply (rebuild by replay) and read, which is what an audit of a data
	// directory needs.
	//
	// SUCH A STORE IS NOT READ-ONLY ON DISK, and this doc used to say it was.
	// When Dir is set, Apply repairs the durable withdrawal floor from any
	// withdrawal the log holds that the floor has fallen behind
	// (reconcileWithdrawalFloor), so a replay writes the data directory even with
	// no Durable. That is deliberate — a floor that lost data must be repaired by
	// whichever process notices first, and refusing to repair from a read path
	// would leave the hole open for exactly as long as nobody wrote — but it
	// means the SINGLE-WRITER rule on PeerWithdrawalFloorFileName binds READERS
	// too: a store with a Dir may only be built inside the data-directory lock.
	// cmd/agent-bus's `peer` subcommand holds the dirlock on both its read and
	// its write path, so this is satisfied today.
	//
	// THE CALLER MUST REPLAY THE LOG INTO THIS STORE BEFORE THE FIRST WRITE. The
	// configSeq high-water mark starts at zero and is rebuilt only from the
	// records Apply is handed, so a store wired to a log it has NOT replayed
	// would mint sequence 1 over a log that already holds 1..N and reintroduce
	// exactly the defect configSeq exists to prevent. The two supported wirings
	// both satisfy this: pass the store as wal.LogOptions.Applier (Open replays
	// before it returns), or call wal.Replay(path, store.Apply) before issuing a
	// write. There is no way for this package to check it, which is why it is
	// stated here rather than assumed.
	Durable PeerDurableLog

	// Dir is the DATA DIRECTORY, and it is where the durable withdrawal floor
	// (PeerWithdrawalFloorFileName) lives. It must be the same directory the
	// durable log lives in, and this package cannot check that — the log arrives
	// as an interface precisely so it can be built without one.
	//
	// # An EMPTY Dir does not silently degrade: it REFUSES WITHDRAWALS
	//
	// Remove and RemoveTrust both fail with ErrPeerNoWithdrawalFloor, and reads
	// and Apply still work. That is deliberate, and it is the fail-closed choice
	// rather than the convenient one: a withdrawal recorded ONLY in the log can
	// be un-said by losing one entry (RELAY-34), so a store that cannot write the
	// floor must not be allowed to tell an operator their revocation succeeded.
	//
	// The corresponding honest caveat, stated rather than left to be discovered:
	// a store built WITHOUT Dir over a data directory that HAS a floor file
	// cannot consult it, so it may serve a pin the operator revoked. Such a store
	// is an unwired audit shape only and must never back a routing or
	// verification decision. Any caller that constructs a PeerStore for a running
	// bus — or for the offline `agent-bus peer` subcommand, which is where an
	// operator's revocation is actually typed — MUST set Dir.
	//
	// A non-empty Dir must exist and be a directory; NewPeerStore checks, because
	// a store that cannot write its floor is a bus that would pass construction
	// and then refuse the first revocation, which is a startup failure wearing a
	// runtime disguise.
	Dir string

	// Logger receives every discard and every refused transition. It may be nil.
	Logger *logging.Logger

	// Now supplies the clock the retention predicate is evaluated against. nil
	// means time.Now.
	Now func() time.Time

	// MaxPeers is the hard cap on retained records IN EACH TABLE, active and
	// tombstone together. 0 means MaxPeers; negative is a construction error.
	MaxPeers int

	// TombstoneRetention is how long a withdrawn record is kept. 0 means
	// PeerTombstoneRetention; negative is a construction error.
	TombstoneRetention time.Duration
}

// PeerStore is the durable, bounded federation-configuration store: routes and
// trust pins, written through a two-phase log and rebuilt by replay.
//
// The three properties that make it safe are invite.Store's, and they are the
// reason this is a copy of that design rather than a fresh one:
//
// # 1. applyLocked is the ONLY writer to the tables, on BOTH paths
//
// Every mutating operation encodes a COMPLETE post-transition record, writes it
// through Durable, and then folds the identical record into memory. The record
// folded in is the one produced by Decode(Encode(rec)) — literally the bytes
// replay will read — so a live Apply and a replayed Apply cannot drift.
//
// # 2. THE STORE LOCK IS NEVER HELD ACROSS A DURABLE WRITE
//
// wal.Log.Write calls Applier.Apply synchronously (internal/wal/log.go, Commit),
// and this store may itself be (or be reached from) that Applier. Holding s.mu
// across Durable.Write would self-deadlock. Every mutating method therefore takes
// s.mu, decides, RELEASES it, writes, and takes it again to fold the result in.
//
// WHAT MAKES THAT SAFE IS writeMu, a SECOND mutex held across the whole
// operation — mint, durable write, fold. No decision is left unguarded, because
// no other write can run between the decision and the fold: the capacity check,
// the sequence and the record all come from one serialised pass. Apply never
// takes writeMu, so the only lock order is writeMu -> mu and there is no cycle.
// (An earlier version guarded this with per-bus reservations instead and let
// writes overlap; that let WAL order diverge from mint order, which both review
// gates showed could permanently lose an acknowledged write. See writeMu.)
//
// # 3. The upsert is MONOTONIC on a sequence this bus mints
//
// See busTable.upsert and the configSeq field.
type PeerStore struct {
	// mu guards every field below. A plain Mutex rather than an RWMutex: every
	// exported entry point sweeps first, and sweeping mutates.
	mu sync.Mutex

	busID   string
	durable PeerDurableLog
	log     *logging.Logger
	now     func() time.Time

	// dir is the data directory, and floorPath is
	// <dir>/peer-withdrawal-floor within it. Both are empty when the store was
	// built without a Dir, in which case a withdrawal is REFUSED rather than
	// recorded only in the log — see PeerStoreOptions.Dir.
	dir       string
	floorPath string

	routes *busTable
	trust  *busTable

	// configSeq is the HIGH-WATER MARK of every configuration sequence number
	// this bus has ever put on disk, ACROSS BOTH TABLES.
	//
	// # It is a bus-wide counter, NOT a per-peer one, and that is the fix for a
	// # real defect rather than a stylistic choice
	//
	// A per-peer counter has to be derived from that peer's own entry, and that
	// entry can legitimately LEAVE the table: swept once its tombstone expires,
	// or discarded on replay by the capacity cap. The next write for that bus
	// would then restart at 1 while the durable log still holds records at
	// 1..N — and on the following replay the OLD generation, arriving first at
	// an equal sequence, WINS. The operator's current address, or their current
	// pin set, is silently replaced by a superseded one. That is precisely
	// invariant 1's rule ("recovery may not reissue an index it has already
	// handed out, EVEN FOR A RECORD IT DISCARDS") being broken, and it was found
	// by the security gate on the first version of this file.
	//
	// One bus-wide counter cannot regress that way, because it is raised by
	// EVERY record replay decodes — before any decision to discard it — and is
	// never lowered by a sweep, a cap discard or a removal. The cost is that a
	// bus's numbers are not contiguous (bus A gets 1, bus B gets 2, A's next is
	// 3), which is the same trade invariant 1 already makes: uniqueness and
	// monotonicity hold, contiguity does not and never did.
	//
	// THE RESIDUAL, STATED RATHER THAN GLOSSED: the mark is raised from a record
	// only once that record DECODES, so a number carried by a body this binary
	// cannot read is unknown to it and could be issued again. Two ways to get
	// there, and they are not equally comfortable:
	//
	//   - a CORRUPT frame. Harmless: corruption does not heal, so recovery
	//     discards the same frame on every subsequent start and no two SURVIVING
	//     records can ever claim one number.
	//   - an INTACT body this binary refuses — most concretely a "v":2 record
	//     written by a newer binary and then read by an older one. That body is
	//     still there and still readable BY THE NEWER BINARY, so a downgrade
	//     followed by an upgrade could present two records at one sequence. This
	//     is the same downgrade hazard internal/wal states for Entry.Idem, and
	//     the same answer applies: downgrade is not a supported operation here.
	//     Written down so it is known rather than discovered.
	//
	// A durable floor file (the wal-index-floor pattern) would close both. Since
	// RELAY-34 there IS one — <data-dir>/peer-withdrawal-floor — and NewPeerStore
	// seeds this mark from it, so the two residuals above are narrowed but NOT
	// eliminated: the file records the sequence of every WITHDRAWAL, not of every
	// record, so a number carried only by an unreadable ACTIVE record is still
	// unknown to this binary. Closing them fully would mean flooring every write,
	// which is a durability-layer mechanism for a counter that is not an id, and
	// remains deliberately unbuilt.
	configSeq uint64

	// floorMu serialises the WHOLE update of the durable withdrawal floor:
	// snapshot the mirror, encode, atomically replace the file, adopt it in
	// memory. It is a THIRD mutex and it is not redundant with writeMu.
	//
	// # Why writeMu cannot do this job — a security-gate finding
	//
	// recordWithdrawal releases s.mu across the file write, and its safety used
	// to be argued from writeMu: "the caller holds it for the whole mint ->
	// write -> fold, so no second write can interleave". That argument covered
	// exactly ONE of its two callers. reconcileWithdrawalFloor is the other, and
	// it holds no writeMu BY NECESSITY — it runs from Apply, which write()
	// reaches while already holding writeMu, so taking it there self-deadlocks.
	//
	// With the precondition unmet, two callers could interleave
	// snapshot-then-write and the second could write a snapshot taken before the
	// first's entry existed, DROPPING a floor the caller had already been told
	// was recorded. The gate reproduced it: a concurrent Remove and Apply left
	// the route floor missing from disk although Remove returned success. That
	// is the "silently LOWERING a floor" this file must never do.
	//
	// Not reachable through today's wiring, since writeMu serialises peer writes
	// and wal.Txn holds its lock across Apply — but it becomes reachable the
	// moment a second source of applied peer records exists, which is exactly
	// relay ingest. Fixed structurally rather than left as an assumption about
	// callers.
	//
	// LOCK ORDER is floorMu -> mu, and nothing takes floorMu while holding mu, so
	// there is no cycle. Apply never takes writeMu, so writeMu -> floorMu -> mu
	// is the only chain and it is acyclic.
	floorMu sync.Mutex

	// writeMu serialises WHOLE WRITES: mint the sequence, write it durably, fold
	// it in. It is a SECOND mutex rather than a longer hold of s.mu because s.mu
	// cannot be held across Durable.Write (wal.Log.Write calls Applier.Apply
	// synchronously and this store may be that Applier, so holding it would
	// self-deadlock), and writeMu is never taken by Apply — so there is no cycle.
	//
	// # It is here to make WAL ORDER equal MINT ORDER, which is a correctness
	// # requirement and not a convenience
	//
	// An earlier version minted under s.mu, released it, and wrote — so two
	// concurrent writers could mint 2 and 3 and land in the log as 3 then 2. Both
	// were durable and both were acknowledged, but on a later replay past the
	// tombstone retention the seq-2 record arrives BEHIND a swept seq-3
	// tombstone and busTable.sweptMax refuses it: an operator configuration that
	// was acknowledged is then lost, permanently, on every subsequent boot. Both
	// gates reproduced it. Serialising is the honest fix rather than weakening
	// the floor, and it costs nothing here: writes come from an OFFLINE operator
	// subcommand under the dirlock (DECISIONS.md, FEDERATION (e)), so there is no
	// concurrency to preserve — which is also why the previous per-bus
	// reservation bookkeeping could simply be deleted rather than corrected.
	writeMu sync.Mutex
}

// NewPeerStore validates opts and returns a store holding nothing but the
// DURABLE WITHDRAWAL FLOORS read from o.Dir.
//
// Those floors are the one piece of state that is loaded here rather than
// replayed, and that is the point of them: they are what a replay cannot
// reconstruct, because the record they came from may be the one recovery threw
// away. A corrupt floor file is FATAL and is never regenerated — see
// ErrPeerWithdrawalFloorCorrupt, including why that does not contradict
// invariant 6.
func NewPeerStore(o PeerStoreOptions) (*PeerStore, error) {
	if err := ids.ValidateBusID(o.BusID); err != nil {
		return nil, fmt.Errorf("relay: peer store bus id: %w", err)
	}
	if o.MaxPeers < 0 {
		return nil, fmt.Errorf("relay: PeerStoreOptions.MaxPeers is %d; it must be zero (meaning %d) or positive", o.MaxPeers, MaxPeers)
	}
	if o.TombstoneRetention < 0 {
		return nil, fmt.Errorf("relay: PeerStoreOptions.TombstoneRetention is %s; it must be zero (meaning %s) or positive", o.TombstoneRetention, PeerTombstoneRetention)
	}
	max := o.MaxPeers
	if max == 0 {
		max = MaxPeers
	}
	retention := o.TombstoneRetention
	if retention == 0 {
		retention = PeerTombstoneRetention
	}
	s := &PeerStore{
		busID:   o.BusID,
		durable: o.Durable,
		log:     o.Logger,
		now:     o.Now,
		routes:  newBusTable("peer route", routeTableToken, max, retention),
		trust:   newBusTable("bus trust", trustTableToken, max, retention),
	}
	if s.log == nil {
		s.log = logging.New(io.Discard, logging.LevelError)
	}
	if s.now == nil {
		s.now = time.Now
	}

	if o.Dir != "" {
		// Checked HERE rather than at the first write, so a Dir that is missing or
		// is not a directory at all fails at construction rather than at the
		// moment an operator tries to revoke a key.
		//
		// It checks EXISTENCE, not WRITABILITY, and the difference is stated
		// rather than glossed: a directory that exists but cannot be written
		// still constructs, and the first withdrawal then fails with the
		// underlying I/O error. That is fail-CLOSED — the revocation is refused
		// and the operator is told — but it is not the same as proving up front
		// that a withdrawal will be possible, and a comment claiming otherwise
		// would be the comfortable version rather than the true one.
		info, err := os.Stat(o.Dir)
		if err != nil {
			return nil, fmt.Errorf("relay: opening the durable peer withdrawal floor in %s: %w", o.Dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("relay: opening the durable peer withdrawal floor: %s is not a directory", o.Dir)
		}
		s.dir = o.Dir
		s.floorPath = filepath.Join(o.Dir, PeerWithdrawalFloorFileName)
		floors, err := readPeerWithdrawalFloors(s.floorPath)
		if err != nil {
			return nil, err
		}
		for _, t := range []*busTable{s.routes, s.trust} {
			for folded, seq := range floors[t.token] {
				t.withdrawnAt[folded] = seq
				// THE CONFIGURATION SEQUENCE IS SEEDED FROM THE FLOORS, and this
				// is load-bearing rather than tidy-mindedness.
				//
				// configSeq is otherwise rebuilt only from the records Apply is
				// handed, so a data directory whose LOG was lost or quarantined
				// but whose floor file survived would resume minting at 1 — below
				// every floor — and the operator's very next PutTrust for a
				// previously-revoked bus would be refused by the floor it just
				// dropped under. Forever, since every retry mints the same low
				// number.
				//
				// Seeding closes that, and it is always SAFE because the floor is
				// written strictly BEFORE the record carrying the same sequence:
				// a floor value is therefore always a sequence this bus really
				// did put on disk, so adopting it as a high-water mark can only
				// agree with, never overstate, what the log would have proven.
				if seq > s.configSeq {
					s.configSeq = seq
				}
			}
		}
	} else if o.Durable != nil {
		// A store that can WRITE but cannot record a withdrawal outside the log
		// is the shape RELAY-34 exists to stop shipping. It is not refused
		// outright — an in-memory test double is a legitimate use — but it is
		// said loudly, once, at construction, because the alternative is finding
		// out at the moment an operator tries to revoke a key.
		s.log.Warn("this peer store has a durable log but NO data directory, so peer and trust WITHDRAWALS WILL BE REFUSED; a withdrawal recorded only in the log can be un-said by a discarded tail, and for the trust table that means a revoked pinned bus signing key comes back (RELAY-34). Set PeerStoreOptions.Dir",
			"local_bus", s.busID, "floor_file", PeerWithdrawalFloorFileName)
	}
	return s, nil
}

// BusID reports this bus's own id.
func (s *PeerStore) BusID() string { return s.busID }

// Put durably records a ROUTE to a peer, or a new generation of one.
//
// Nothing is returned until the record is DURABLE (invariant 4): the capacity
// check and the sequence number are decided BEFORE anything is written, and the
// record is folded into memory only after Durable.Write has fsynced both phases.
//
// A Put whose address AND next-hop certificate pin are identical to the peer's
// current active route is a NO-OP that returns the existing record and writes
// nothing. That is not an optimisation: an operator's config-management run that
// re-applies the same peering would otherwise append a record on every pass and
// the log would grow with nothing having changed. Note the deliberate asymmetry
// with invariant 10's wire-level rule — there is no client, no idempotency key
// and no lost ack here, because this is an in-process operator API reached from
// an offline subcommand under the dirlock, not a route.
//
// THE PIN IS PART OF THAT COMPARISON, and leaving it out would be the bug this
// sentence exists to prevent: re-pinning a rotated certificate at an unchanged
// address is precisely a Put whose BaseURL has not moved, and it would be
// swallowed as a no-op while the operator was told it succeeded.
func (s *PeerStore) Put(cfg PeerConfig) (PeerRecord, error) {
	rec, err := s.write(s.routes, cfg.BusID, func(existing busScopedRecord, seq uint64, now time.Time) (busScopedRecord, bool, error) {
		if cur, ok := existing.(PeerRecord); ok && cur.State == PeerRecordActive &&
			cur.BaseURL == cfg.BaseURL && cur.NextHopTLSCertFingerprint == cfg.NextHopTLSCertFingerprint {
			return cur, false, nil
		}
		return PeerRecord{
			BusID:                     cfg.BusID,
			ConfigSeq:                 seq,
			State:                     PeerRecordActive,
			BaseURL:                   cfg.BaseURL,
			NextHopTLSCertFingerprint: cfg.NextHopTLSCertFingerprint,
			UpdatedAt:                 now,
		}, true, nil
	})
	if err != nil {
		return PeerRecord{}, err
	}
	out, typed := rec.(PeerRecord)
	if !typed {
		// Unreachable: this method's own build closure produced the record.
		// Reported rather than asserted, so a future edit that broke the pairing
		// would fail the call instead of panicking the process.
		return PeerRecord{}, fmt.Errorf("%w: %T is not a PeerRecord", ErrInvalidPeerRecord, rec)
	}
	return out, nil
}

// Remove durably withdraws a route, leaving a TOMBSTONE rather than deleting the
// entry (see busTable.upsert for what the tombstone holds on to).
//
// Removing an unknown route is ErrUnknownPeer. Removing an ALREADY-REMOVED one
// writes no log entry, but it is not a no-op on disk: it RE-ASSERTS the durable
// withdrawal floor, so an operator re-applying a withdrawal after the floor file
// was lost gets it back (a security-gate finding — see recordWithdrawal). On a
// store built without PeerStoreOptions.Dir it therefore fails with
// ErrPeerNoWithdrawalFloor rather than reporting a success it cannot make stick.
func (s *PeerStore) Remove(busID string) (PeerRecord, error) {
	rec, err := s.write(s.routes, busID, func(existing busScopedRecord, seq uint64, now time.Time) (busScopedRecord, bool, error) {
		cur, ok := existing.(PeerRecord)
		if !ok {
			return nil, false, fmt.Errorf("%w: %q has no route on this bus", ErrUnknownPeer, busID)
		}
		if cur.State == PeerRecordRemoved {
			return cur, false, nil
		}
		return PeerRecord{BusID: cur.BusID, ConfigSeq: seq, State: PeerRecordRemoved, UpdatedAt: now}, true, nil
	})
	if err != nil {
		return PeerRecord{}, err
	}
	out, typed := rec.(PeerRecord)
	if !typed {
		// Unreachable: this method's own build closure produced the record.
		// Reported rather than asserted, so a future edit that broke the pairing
		// would fail the call instead of panicking the process.
		return PeerRecord{}, fmt.Errorf("%w: %T is not a PeerRecord", ErrInvalidPeerRecord, rec)
	}
	return out, nil
}

// PutTrust durably pins a bus's signing keys, or a new generation of them.
//
// THE BUS NEED NOT HAVE A ROUTE, and that is the whole reason this is a separate
// record: in an A <-> B <-> C line, C pins A while never peering with A.
//
// Passing the identical pin set, in the identical order, is a no-op.
func (s *PeerStore) PutTrust(t BusTrust) (BusTrustRecord, error) {
	// Copied before anything else so the caller cannot mutate the slice between
	// validation and the durable write.
	keys := copySigningKeys(t.SigningKeys)
	rec, err := s.write(s.trust, t.BusID, func(existing busScopedRecord, seq uint64, now time.Time) (busScopedRecord, bool, error) {
		if cur, ok := existing.(BusTrustRecord); ok && cur.State == PeerRecordActive && sameKeySet(cur.SigningKeys, keys) {
			return cur, false, nil
		}
		return BusTrustRecord{BusID: t.BusID, ConfigSeq: seq, State: PeerRecordActive, SigningKeys: keys, UpdatedAt: now}, true, nil
	})
	if err != nil {
		return BusTrustRecord{}, err
	}
	out, typed := rec.(BusTrustRecord)
	if !typed {
		// Unreachable: this method's own build closure produced the record.
		// Reported rather than asserted, so a future edit that broke the pairing
		// would fail the call instead of panicking the process.
		return BusTrustRecord{}, fmt.Errorf("%w: %T is not a BusTrustRecord", ErrInvalidPeerRecord, rec)
	}
	return out, nil
}

// RemoveTrust durably un-pins a bus, leaving a tombstone.
//
// It is the operator's revocation path: after it, PinnedKeys answers with
// nothing, which is the same answer an unknown bus gets.
//
// A duplicated older record cannot put the pins back — first because the
// tombstone outranks it, and after PeerTombstoneRetention has expired the
// tombstone because busTable.sweptMax carries its sequence forward. The revoked
// key does not come back when the tombstone goes away; that was a real hole in
// the first version of this file and both gates found it.
//
// NOR DOES IT COME BACK WHEN THE TOMBSTONE ITSELF IS LOST. That was a second,
// deeper hole (RELAY-34): both defences above live inside the log, so a
// discarded tail took them with it. The revocation is now fsynced into
// <data-dir>/peer-withdrawal-floor BEFORE the tombstone reaches the log, and
// that file is what a torn tail cannot reach. The consequence a caller must
// handle: on a store built without PeerStoreOptions.Dir this method REFUSES with
// ErrPeerNoWithdrawalFloor rather than recording a revocation it cannot make
// stick.
func (s *PeerStore) RemoveTrust(busID string) (BusTrustRecord, error) {
	rec, err := s.write(s.trust, busID, func(existing busScopedRecord, seq uint64, now time.Time) (busScopedRecord, bool, error) {
		cur, ok := existing.(BusTrustRecord)
		if !ok {
			return nil, false, fmt.Errorf("%w: %q has no pinned bus signing keys on this bus", ErrUnknownPeer, busID)
		}
		if cur.State == PeerRecordRemoved {
			return cur, false, nil
		}
		return BusTrustRecord{BusID: cur.BusID, ConfigSeq: seq, State: PeerRecordRemoved, UpdatedAt: now}, true, nil
	})
	if err != nil {
		return BusTrustRecord{}, err
	}
	out, typed := rec.(BusTrustRecord)
	if !typed {
		// Unreachable: this method's own build closure produced the record.
		// Reported rather than asserted, so a future edit that broke the pairing
		// would fail the call instead of panicking the process.
		return BusTrustRecord{}, fmt.Errorf("%w: %T is not a BusTrustRecord", ErrInvalidPeerRecord, rec)
	}
	return out, nil
}

// write is the shared durable-write path for both tables.
//
// build decides, under the lock, what record to write: it is handed the existing
// entry (nil if none), the sequence number reserved for this generation and the
// timestamp, and returns the record, whether it must be WRITTEN at all (false
// means "nothing changed", the no-op case), and any refusal.
func (s *PeerStore) write(t *busTable, busID string, build func(existing busScopedRecord, seq uint64, now time.Time) (busScopedRecord, bool, error)) (busScopedRecord, error) {
	if s.durable == nil {
		return nil, fmt.Errorf("%w: refusing to record federation configuration that would be lost on restart", ErrPeerNotDurable)
	}
	if err := ValidatePeerBusID(s.busID, busID); err != nil {
		return nil, err
	}

	// ONE WRITE AT A TIME, ACROSS MINT -> DURABLE WRITE -> FOLD. See the
	// writeMu field: this is what makes the order of records IN THE LOG the
	// order their sequence numbers were minted in, which busTable.sweptMax and
	// the whole monotonic upsert depend on.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := s.now().UTC()
	folded := strings.ToLower(busID)

	s.mu.Lock()
	t.sweep(now, s.log, s.busID)
	existing, known := t.entries[folded]
	if known && existing.recordBusID() != busID {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: this store already holds %s for %q, and %q differs from it only by ASCII case; a confusable in the routing subject is refused at the door", ErrPeerBusIDCollision, t.what, existing.recordBusID(), busID)
	}
	// A record the DURABLE WITHDRAWAL FLOOR supersedes is ABSENT to the write
	// path too, exactly as it is to every reader.
	//
	// This is not symmetry for its own sake — dropping it makes re-pinning
	// impossible. Put/PutTrust return a NO-OP when the incoming configuration
	// equals the current active one, so an operator re-pinning the SAME key after
	// a lost tombstone would match the superseded record, be told "nothing to
	// do", and be left with a bus that is still un-pinned. The withdrawal is also
	// what Remove/RemoveTrust must not find, so an already-withdrawn bus reports
	// ErrUnknownPeer rather than writing a second tombstone.
	// slotHeld is whether this bus ALREADY OCCUPIES a slot in the table, which is
	// a different question from whether its record is usable. The floor-hidden
	// case below makes the record invisible but does NOT free its slot, so
	// gating the capacity check on `known` would refuse an operator
	// RECONFIGURING A BUS THE TABLE ALREADY HOLDS, at exactly MaxPeers, when no
	// new slot is needed at all. Caught by the reviewer gate.
	slotHeld := known
	if known && t.withdrawn(existing) {
		known = false
	}
	if !known {
		existing = nil
		if !slotHeld && len(t.entries) >= t.max {
			// Read UNDER the lock and only then formatted: reading it after the
			// unlock would be a data race on the field being reported.
			held := len(t.entries)
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: %d %s records are retained (active and tombstone), the limit of %d", ErrTooManyPeers, held, t.what, t.max)
		}
	}
	if s.configSeq >= maxConfigSeq {
		held := s.configSeq
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %d configuration records have been written, the ceiling above which a JSON reader using float64 numbers could no longer read the value back", ErrPeerConfigSeqExhausted, held)
	}
	seq := s.configSeq + 1
	rec, mustWrite, err := build(existing, seq, now)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !mustWrite {
		s.mu.Unlock()
		// RE-APPLYING AN ALREADY-APPLIED WITHDRAWAL RE-ASSERTS ITS FLOOR.
		//
		// This is a security-gate finding (RELAY-34, P1) and not tidiness. The
		// remedy for a lost or corrupt floor file tells an operator to re-apply
		// every withdrawal by hand — but the tombstone is still in the log, so
		// RemoveTrust took this no-op branch, returned the tombstone and nil, and
		// wrote NOTHING. The operator was told the revocation was in place while
		// the floor was still absent, and the next torn tail resurrected the key.
		// The gate reproduced exactly that.
		//
		// Re-flooring here is free in the normal case: recordWithdrawal returns
		// immediately when the floor already covers the sequence, so this costs
		// an uncontended lock and no I/O unless the floor is genuinely missing or
		// behind.
		if rec.recordState() == PeerRecordRemoved {
			if err := s.recordWithdrawal(t, folded, rec.recordSeq()); err != nil {
				return nil, err
			}
		}
		return rec.clone(), nil
	}
	// The sequence is CONSUMED here, before the write, and never reused. A write
	// that then fails BURNS the number: that is a gap, which is expected and is
	// not a defect (see the configSeq field).
	s.configSeq = seq
	// s.mu is RELEASED for the durable write and only that. It must be, because
	// wal.Log.Write calls Applier.Apply synchronously and this store may be that
	// Applier — holding s.mu across it self-deadlocks. writeMu is still held, so
	// no second write can interleave.
	s.mu.Unlock()

	body, canonical, err := canonicalPeerRecord(rec)
	if err != nil {
		return nil, err
	}
	// THE WITHDRAWAL FLOOR IS FSYNCED BEFORE THE LOG ENTRY IS WRITTEN. This
	// ordering is the whole of RELAY-34's fix and it must not be reversed for any
	// reason, including latency: it is invariant 4's rule one layer down —
	// nothing is ACKNOWLEDGED as withdrawn before the fact of the withdrawal is
	// durable somewhere no log repair can reach.
	//
	// Written AHEAD rather than after, because the interesting failure is the
	// crash BETWEEN the two. Floor-then-log leaves a bus un-pinned with no
	// tombstone (fail-CLOSED: fewer keys trusted, and the operator's retry
	// completes it). Log-then-floor would leave a tombstone the very next torn
	// tail can discard, with nothing outside the log remembering — which is
	// precisely the state this task exists to make unreachable.
	if canonical.recordState() == PeerRecordRemoved {
		if err := s.recordWithdrawal(t, folded, canonical.recordSeq()); err != nil {
			return nil, err
		}
	}

	kind := PeerRecordKind
	if t == s.trust {
		kind = BusTrustRecordKind
	}
	if _, err := s.durable.Write(wal.Entry{Kind: kind, Body: body}); err != nil {
		return nil, fmt.Errorf("relay: recording %s for %s durably: %w", t.what, busID, err)
	}

	// The record is on stable storage. A refusal from here is logged and
	// swallowed: the durable log is the truth and a restart rebuilds memory from
	// it, so returning an error would tell the caller an operation failed that
	// demonstrably did not.
	s.mu.Lock()
	if err := s.applyLocked(t, canonical, "write"); err != nil {
		s.log.Error("a peer-configuration record is DURABLE but was refused by the in-memory table; the durable log is the truth and a restart will rebuild from it",
			"local_bus", s.busID, "table", t.what, "peer_bus", busID, "config_seq", canonical.recordSeq(), "err", err)
	}
	s.mu.Unlock()
	return canonical, nil
}

// recordWithdrawal raises this table's durable withdrawal floor for one bus to
// seq and returns only once the bytes and the directory entry are FSYNCED.
//
// On success, floor[table][bus] >= seq is on stable storage OUTSIDE the log —
// which is the post-condition PeerStore.write relies on before it lets the
// withdrawal record anywhere near the durable log.
//
// # The lock discipline
//
// s.floorMu is held across the WHOLE sequence — snapshot, encode, atomic
// replace, adopt — so two callers can never interleave and write a snapshot
// taken before the other's entry existed. s.mu is released across the file write
// instead, because every reader takes it and a reader blocked on an operator's
// disk is a bus that stops answering for the duration.
//
// CORRECTED by the security gate: this used to argue the safety from writeMu
// ("the caller holds it for the whole mint -> write -> fold"), which covered
// only ONE of the two callers. reconcileWithdrawalFloor holds no writeMu and
// cannot, and the gate reproduced a lost floor entry through that gap. See the
// floorMu field.
//
// # Memory NEVER claims more than disk
//
// t.withdrawnAt is updated only AFTER a successful write, mirroring
// ids.DurableNameSuffixes, wal's indexFloor and hub's seqFloorFile. A caller
// that saw an error has floored nothing and may safely retry with the same
// number — and, crucially, has NOT been told a revocation happened.
//
// A seq at or below the floor already on disk is a pure no-op that touches
// nothing, which is what makes a repeated withdrawal free.
func (s *PeerStore) recordWithdrawal(t *busTable, folded string, seq uint64) error {
	if s.dir == "" {
		return fmt.Errorf("%w: refusing to withdraw the %s for %s. A withdrawal that exists only as a log entry can be UN-SAID by losing that entry — a torn tail, a bit-rotted frame, a snapshot rolled back past it — and for the trust table an un-said withdrawal is a REVOKED PINNED BUS SIGNING KEY that comes back (RELAY-34). Build this store with PeerStoreOptions.Dir set to the data directory so the withdrawal can be recorded in %s, outside the log",
			ErrPeerNoWithdrawalFloor, t.what, folded, PeerWithdrawalFloorFileName)
	}

	// THE SEQUENCE BOUND, checked here rather than only in the encoder, so the
	// caller gets ErrPeerWithdrawalSeqTooHigh and its accurate remedy instead of
	// a corrupt-file error that would send an operator to delete a healthy floor.
	// See that sentinel.
	if seq >= maxPlausiblePeerWithdrawalSeq {
		return fmt.Errorf("%w: refusing to withdraw the %s for %s at config_seq %d, which is at or above %d — the highest this bus can record in %s and read back. THE FLOOR FILE IS NOT CORRUPT: do NOT move it aside, because that would delete every revocation it holds. The cause is in the LOG: a peer-configuration record carrying an implausibly high config_seq has raised this bus's configuration counter (the counter is raised by every record, including discarded ones, so that a sequence is never reissued). Until that record is out of the replayed history this withdrawal cannot be recorded, and the configuration it would withdraw is STILL IN FORCE",
			ErrPeerWithdrawalSeqTooHigh, t.what, folded, seq, maxPlausiblePeerWithdrawalSeq, PeerWithdrawalFloorFileName)
	}

	// HELD ACROSS SNAPSHOT -> ENCODE -> RENAME -> ADOPT. See the floorMu field:
	// s.mu cannot do it (it is released across the file write so readers are not
	// blocked on an operator's disk) and writeMu cannot either (reconcile runs
	// from Apply, which write() reaches while already holding it).
	s.floorMu.Lock()
	defer s.floorMu.Unlock()

	s.mu.Lock()
	if seq <= t.withdrawnAt[folded] {
		s.mu.Unlock()
		return nil
	}
	if _, already := t.withdrawnAt[folded]; !already {
		if n := len(s.routes.withdrawnAt) + len(s.trust.withdrawnAt); n >= maxPeerWithdrawalFloorEntries {
			s.mu.Unlock()
			return fmt.Errorf("%w: %d peer withdrawals are recorded in %s, the limit, and a withdrawal may never be forgotten (the record it defends against is still in an append-only log). This withdrawal is REFUSED rather than recorded only in the log, so it has NOT taken effect — the configuration stands and must be withdrawn again once the limit is raised",
				ErrTooManyPeerWithdrawals, n, PeerWithdrawalFloorFileName)
		}
	}
	entries := make([]peerWithdrawalEntry, 0, len(s.routes.withdrawnAt)+len(s.trust.withdrawnAt)+1)
	for _, tab := range []*busTable{s.routes, s.trust} {
		for bus, at := range tab.withdrawnAt {
			if tab == t && bus == folded {
				continue // superseded by the pending raise, appended below.
			}
			entries = append(entries, peerWithdrawalEntry{table: tab.token, busID: bus, seq: at})
		}
	}
	entries = append(entries, peerWithdrawalEntry{table: t.token, busID: folded, seq: seq})
	s.mu.Unlock()

	data, err := encodePeerWithdrawalFloors(entries)
	if err != nil {
		return err
	}
	if err := atomicReplacePeerWithdrawalFloor(s.dir, s.floorPath, data); err != nil {
		return fmt.Errorf("relay: recording the withdrawal of the %s for %s in %s: %w", t.what, folded, s.floorPath, err)
	}

	s.mu.Lock()
	if seq > t.withdrawnAt[folded] {
		t.withdrawnAt[folded] = seq
	}
	s.mu.Unlock()
	return nil
}

// Lookup returns the ROUTE record for a bus id, in whatever state it holds. The
// caller must check State: a tombstone is a known record and NOT a usable route.
//
// A record superseded by a durably recorded WITHDRAWAL is reported ABSENT rather
// than returned as a tombstone — see busTable.lookup.
func (s *PeerStore) Lookup(busID string) (PeerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes.sweep(s.now(), s.log, s.busID)
	rec, ok := s.routes.lookup(busID)
	if !ok {
		return PeerRecord{}, false
	}
	// CHECKED, not asserted. A wrong concrete type in a table is unreachable —
	// Apply dispatches on Entry.Kind and each decoder now refuses a body whose
	// own "rec" field disagrees — but a panic here would take down a running bus
	// over a bug this package could report instead. Same posture as Apply's
	// never-return-an-error rule.
	out, typed := rec.(PeerRecord)
	if !typed {
		s.log.Error("the route table holds a record of the wrong type; it is being reported as absent rather than panicking a running bus",
			"local_bus", s.busID, "peer_bus", busID, "type", fmt.Sprintf("%T", rec))
		return PeerRecord{}, false
	}
	return out, true
}

// LookupTrust returns the TRUST record for a bus id, in whatever state it holds.
func (s *PeerStore) LookupTrust(busID string) (BusTrustRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trust.sweep(s.now(), s.log, s.busID)
	rec, ok := s.trust.lookup(busID)
	if !ok {
		return BusTrustRecord{}, false
	}
	out, typed := rec.clone().(BusTrustRecord)
	if !typed {
		s.log.Error("the trust table holds a record of the wrong type; it is being reported as absent rather than panicking a running bus",
			"local_bus", s.busID, "peer_bus", busID, "type", fmt.Sprintf("%T", rec))
		return BusTrustRecord{}, false
	}
	return out, true
}

// PinnedKeys returns the bus signing keys pinned for a bus, freshly allocated.
//
// It returns NOTHING for an unknown bus and nothing for a withdrawn one, which
// is the same answer on purpose: signed.go's CrossBusTrust is explicit that an
// empty pin set and an error mean the same thing to VerifyRelayed, and that
// there is no "unknown, so allow" outcome to reach by accident.
//
// This is NOT an implementation of CrossBusTrust — that interface must also
// VERIFY an attestation, which is RELAY-17's work. This is the storage side of
// its first method and nothing more.
//
// # For RELAY-17: ABSENCE now means what it says (RELAY-34)
//
// Until RELAY-34 the PRESENCE of a pin here was sound and its ABSENCE was not:
// absence meant "no surviving record says otherwise", and a discarded withdrawal
// could manufacture exactly that. A revoked key came back. Absence now means
// "not currently trusted", because a withdrawal is durable outside the log
// before it is acknowledged and this read consults it. Both halves are safe to
// build a cross-bus trust anchor on — PROVIDED the store was built with
// PeerStoreOptions.Dir; one built without it cannot see the floor and must not
// back a verification decision.
func (s *PeerStore) PinnedKeys(busID string) []ed25519.PublicKey {
	rec, ok := s.LookupTrust(busID)
	if !ok || rec.State != PeerRecordActive {
		return nil
	}
	return rec.SigningKeys
}

// ActivePeers returns every ACTIVE route, sorted by bus id, freshly allocated.
//
// Tombstones are excluded: they are bookkeeping that stops a resurrection, never
// something to dial. The name is ActivePeers rather than Peers so it cannot be
// confused with Registry.Peers, which returns the SERVING copy's view.
func (s *PeerStore) ActivePeers() []PeerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes.sweep(s.now(), s.log, s.busID)
	out := make([]PeerRecord, 0, len(s.routes.entries))
	for _, rec := range s.routes.entries {
		// The durable withdrawal floor applies to the LIST paths too. They read
		// the map directly rather than through busTable.lookup, so leaving them
		// out would be a hole in the one direction that matters: a withdrawn
		// route reappearing in the set a caller iterates to decide where to send.
		if s.routes.withdrawn(rec) {
			continue
		}
		r, typed := rec.(PeerRecord)
		if !typed || r.State != PeerRecordActive {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusID < out[j].BusID })
	return out
}

// TrustedBuses returns every bus with ACTIVE pins, sorted by bus id, freshly
// allocated. A bus appears here whether or not it has a route.
func (s *PeerStore) TrustedBuses() []BusTrustRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trust.sweep(s.now(), s.log, s.busID)
	out := make([]BusTrustRecord, 0, len(s.trust.entries))
	for _, rec := range s.trust.entries {
		// See ActivePeers: the floor applies here too, and here it is the
		// difference between listing a bus as trusted and listing one whose pins
		// the operator revoked.
		if s.trust.withdrawn(rec) {
			continue
		}
		r, typed := rec.clone().(BusTrustRecord)
		if !typed || r.State != PeerRecordActive {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusID < out[j].BusID })
	return out
}

// Apply implements wal.Applier: it folds one committed peer-configuration entry
// into the serving copy, on replay at Open and — if the server wires this store
// as the log's Applier — on live commits too. It cannot tell the two apart, and
// must not need to: the record is complete in both cases.
//
// Entries of any other Kind are skipped SILENTLY. This log carries messages,
// enrolments, invites and applied keys; a store that treated its neighbours'
// records as damage would fill the log with false alarms.
//
// # APPLY MUST NEVER RETURN A NON-NIL ERROR
//
// From a live write a non-nil error POISONS the log (wal.ErrDiverged); from
// recovery it makes Open fail, and invariant 6 settled that recovery ALWAYS
// reaches a running server. So every failure path here — an undecodable record,
// an invalid one, the capacity bound, a non-monotonic sequence — LOGS LOUDLY AND
// SPECIFICALLY at ERROR and returns nil, exactly as invite.Store.Apply and
// hub.Apply do. SILENT DISCARD IS THE DEFECT, not discard itself. Each line
// names the prepare/commit index and the reason; a record that could not be
// DECODED cannot name the bus it was for, because that is exactly what could not
// be read — it names the entry kind instead.
//
// # A DISCARD IS FAIL-CLOSED IN BOTH DIRECTIONS (corrected by RELAY-34)
//
// THIS SECTION USED TO SAY THE OPPOSITE, and the correction is the substance of
// RELAY-34 rather than a rewording — read it before changing anything below.
//
// Apply itself never INSTALLS anything this bus did not already hold: the bus
// stays unknown, or keeps the generation already in memory. That half was always
// true, and for a ROUTE it was the whole story — the worst outcome is that a
// configured peer is not restored and the operator re-applies it.
//
// The half that was FALSE: a discard cannot install a configuration, but it CAN
// FAIL TO REMOVE ONE, and for a revocation that is the only direction that
// matters. Every entry carries the whole post-transition state, so a discarded
// WITHDRAWAL — a torn tail, a bit-rotted frame, a snapshot rolled back past it,
// all of which invariant 6 REQUIRES us to survive and boot from rather than
// refuse — left the previous generation as the surviving truth, and reinstated
// it. For routes an un-peered bus became routable again; FOR THE TRUST TABLE A
// REVOKED PINNED SIGNING KEY CAME BACK. The security gate reproduced it by
// truncating eight bytes from a bus.wal tail.
//
// WHAT CLOSES IT is the durable withdrawal floor: a withdrawal is fsynced into
// <data-dir>/peer-withdrawal-floor, OUTSIDE the log, BEFORE the tombstone is
// handed to the log at all. busTable.upsert refuses any active record at or
// below that floor and busTable.lookup hides one, so losing the tombstone now
// loses only the tombstone — the revocation itself is not in the log and cannot
// be discarded with it. See PeerWithdrawalFloorFileName for the mechanism and
// TestPeerStoreTrustSurvivesATornWALTail for the reproduction, kept as a
// regression test.
//
// The residual, stated rather than glossed: this holds for a store built with
// PeerStoreOptions.Dir. A store built WITHOUT one refuses to withdraw at all,
// so it can never acknowledge a revocation it could not floor — but it also
// cannot CONSULT a floor another process wrote, so it must not back a routing or
// verification decision. That is stated on PeerStoreOptions.Dir and warned about
// at construction.
func (s *PeerStore) Apply(c wal.Committed) error {
	var (
		rec   busScopedRecord
		table *busTable
		err   error
	)
	switch c.Entry.Kind {
	case PeerRecordKind:
		var r PeerRecord
		r, err = DecodePeerRecord(c.Entry.Body)
		rec, table = r, s.routes
	case BusTrustRecordKind:
		var r BusTrustRecord
		r, err = DecodeBusTrustRecord(c.Entry.Body)
		rec, table = r, s.trust
	default:
		return nil
	}
	if err != nil {
		s.log.Error("DISCARDING a peer-configuration record that could not be decoded; that configuration is therefore NOT restored and must be re-applied by the operator",
			"local_bus", s.busID, "kind", c.Entry.Kind, "prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex, "err", err)
		return nil
	}
	// s.mu is taken and released EXPLICITLY rather than deferred, because the
	// floor reconciliation below must run WITHOUT it: recordWithdrawal takes s.mu
	// itself and fsyncs a file, and neither may happen under a lock every reader
	// contends on.
	s.mu.Lock()
	if err := s.applyLocked(table, rec, "replay"); err != nil {
		if errors.Is(err, ErrPeerWithdrawn) {
			// NOT an ERROR, and the level is a deliberate judgement rather than
			// an oversight. On an INTACT log this fires on every healthy boot —
			// the active record is replayed, the floor already knows it was
			// withdrawn, and the tombstone that says so is two entries further on
			// — so logging it at ERROR would put an alarming line in every normal
			// startup and train an operator to ignore the ones that matter. It is
			// still specific and still loud enough to find: invariant 6's rule is
			// that a discard must not be SILENT, not that it must be an error.
			s.log.Warn("NOT RESTORING a peer configuration that a durably recorded WITHDRAWAL supersedes; this is the withdrawal floor doing its job, and it is what stops a discarded tombstone from reinstating a revoked pinned bus signing key",
				"local_bus", s.busID, "table", table.what, "prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
				"peer_bus", rec.recordBusID(), "config_seq", rec.recordSeq(), "state", rec.recordState().String(), "err", err)
			s.mu.Unlock()
			return nil
		}
		s.log.Error("DISCARDING a peer-configuration record that could not be applied; that bus keeps the generation already in memory",
			"local_bus", s.busID, "table", table.what, "prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
			"peer_bus", rec.recordBusID(), "config_seq", rec.recordSeq(), "state", rec.recordState().String(), "err", err)
	}
	s.mu.Unlock()

	// The floor is repaired from a withdrawal the log still holds, if it has
	// fallen behind. Free on a live commit and on any healthy start; it does I/O
	// only when the floor file really has lost data. See
	// reconcileWithdrawalFloor.
	s.reconcileWithdrawalFloor(table, rec)
	return nil
}

// reconcileWithdrawalFloor re-derives the durable withdrawal floor from a
// WITHDRAWAL the log still holds, whenever the file has fallen behind the log.
//
// # Why this exists — the remedy has to actually work
//
// A security-gate finding (RELAY-34, P1). The floor file can be lost or
// corrupted independently of the log: bit-rot on it, an inconsistent
// snapshot/backup restore that brings bus.wal forward without it, or an operator
// following ErrPeerWithdrawalFloorCorrupt's instruction to move it aside. The
// state that leaves — a tombstone in the log with no floor beside it — is
// exactly the pre-RELAY-34 hole, silently re-opened. And once the tombstone has
// been SWEPT there is no operator command that can re-establish the floor at
// all, because RemoveTrust then reports ErrUnknownPeer.
//
// So the bus repairs it itself, on the next start, from the one source that
// still has the answer. This is not deriving the floor from the log in the sense
// the file exists to avoid — the floor is still WRITTEN AHEAD of the log entry,
// and is still the ONLY source when the tombstone is gone. It is the reverse
// direction: while the tombstone IS present, it proves a withdrawal happened, so
// a floor behind it is a floor that lost data, not a log that gained it.
//
// # Why it cannot be abused, and cannot lower anything
//
// It only ever RAISES: recordWithdrawal refuses a sequence at or below the
// floor, so a replayed history can advance a floor and never rewind one.
//
// On forgery, stated at the strength the threat model actually supports rather
// than the flattering one: WAL frames carry a keyed HMAC, so the NON-ADVERSARIAL
// triggers this whole mechanism exists for — bit-rot, a torn write, a snapshot
// rollback — can only DROP records, never invent a tombstone. That is the case
// that matters here. It is NOT a claim against a deliberate attacker: wal-mac.key
// lives in the same data directory (internal/wal/mackey.go), so whoever can write
// the directory can generally also read the key and mint frames. What bounds that
// actor is direction — an injected tombstone only ever UN-pins a bus, which is
// fail-closed and repairable by re-pinning — plus the sequence bound above, which
// stops an injected record making the directory unstartable.
//
// On a LIVE commit it costs nothing at all: write() floored the withdrawal
// before the log entry existed, so the floor already covers it and
// recordWithdrawal returns without touching the disk. The only start that pays
// an fsync is one where the floor really had fallen behind.
//
// It NEVER fails the caller. Apply may not return an error (invariant 6), and a
// data directory that cannot be written must not stop a bus from booting — so a
// failure here is logged loudly and specifically and the bus carries on with the
// floor it has, which is the pre-existing behaviour rather than a new hazard.
//
// The caller must NOT hold s.mu.
func (s *PeerStore) reconcileWithdrawalFloor(t *busTable, rec busScopedRecord) {
	if s.dir == "" || rec.recordState() != PeerRecordRemoved {
		return
	}
	// A record whose sequence this binary would refuse to READ BACK is never
	// copied into the floor. Before the repair path existed, a floor entry could
	// only come from write(), where the sequence is minted; reconcile is the one
	// place a value from the LOG reaches the file, so it is the one place that
	// has to bound it. Propagating it would write a file the next start refuses,
	// and the remedy for that refusal runs this same code again.
	// A record naming OUR OWN bus is refused on every path (applyLocked), so it
	// must not be floored either. Without this, a `removed` record carrying the
	// local bus id — which nothing may legitimately produce — wrote a permanent
	// floor entry for this bus's own id, a row no operator action can ever
	// explain or remove. Security-gate finding.
	if err := ValidatePeerBusID(s.busID, rec.recordBusID()); err != nil {
		return
	}
	// AND a record the table refused as an ASCII-CASE CONFUSABLE of a bus it
	// already holds. This is the other half of the same finding: reconcile
	// deliberately runs on records applyLocked REFUSED — a refused record can
	// still be a withdrawal whose floor must be kept — but an identity refusal is
	// different in kind. Two bus ids differing only by case are two DIFFERENT
	// buses downstream.
	//
	// # WHAT THIS GUARD DOES NOT COVER, stated rather than implied
	//
	// It is ORDER-DEPENDENT: it can only fire once the table HOLDS the legitimate
	// record to compare against. A confusable `removed` record replaying FIRST,
	// against an empty table, is still floored — under the FOLDED key, which is
	// the legitimate bus's key.
	//
	// That gap is deliberate, because closing it would break the mechanism. The
	// only available rule would be "do not floor a withdrawal for a bus the table
	// does not already hold", and that is exactly the case the floor exists for:
	// a tombstone whose ACTIVE record was discarded arrives with nothing in the
	// table, and refusing to floor it is the original defect.
	//
	// The residual harm is bounded and is NOT a new un-pinning: the confusable
	// occupies the slot and the legitimate bus is refused by ErrPeerBusIDCollision
	// either way, so the served outcome matches the pre-RELAY-34 behaviour. What
	// is left is a permanent floor row nothing can explain or remove, consuming
	// one of maxPeerWithdrawalFloorEntries. It is reachable only by forging a WAL
	// frame, since nothing outside this file emits these record kinds and the
	// keyed HMAC means the non-adversarial triggers can only DROP records.
	folded := strings.ToLower(rec.recordBusID())
	s.mu.Lock()
	existing, known := t.entries[folded]
	confusable := known && existing.recordBusID() != rec.recordBusID()
	s.mu.Unlock()
	if confusable {
		s.log.Error("REFUSING to record a withdrawal floor for a bus id that differs only by ASCII case from one this table already holds; flooring it would durably un-pin the legitimate bus, which is a revocation nobody performed",
			"local_bus", s.busID, "table", t.what, "peer_bus", rec.recordBusID(),
			"held_as", existing.recordBusID(), "config_seq", rec.recordSeq())
		return
	}
	if rec.recordSeq() >= maxPlausiblePeerWithdrawalSeq {
		s.log.Error("REFUSING to record a withdrawal floor from a log record whose config_seq is implausibly high; the floor is left as it is, so this withdrawal is NOT protected against the loss of its log record, but the data directory stays startable",
			"local_bus", s.busID, "table", t.what, "peer_bus", rec.recordBusID(),
			"config_seq", rec.recordSeq(), "limit", maxPlausiblePeerWithdrawalSeq)
		return
	}
	s.mu.Lock()
	behind := rec.recordSeq() > t.withdrawnAt[folded]
	s.mu.Unlock()
	if !behind {
		return
	}
	if err := s.recordWithdrawal(t, folded, rec.recordSeq()); err != nil {
		s.log.Error("a WITHDRAWAL in the log is not recorded in the durable withdrawal floor and the floor could NOT be repaired; until it is, losing that log record would reinstate the configuration it withdrew — for a trust record, a REVOKED pinned bus signing key",
			"local_bus", s.busID, "table", t.what, "peer_bus", rec.recordBusID(),
			"config_seq", rec.recordSeq(), "floor_file", s.floorPath, "err", err)
		return
	}
	s.log.Warn("REPAIRED the durable withdrawal floor from a withdrawal the log still holds; the floor file had fallen behind the log, which happens when it is lost, restored from an inconsistent snapshot, or moved aside after corruption",
		"local_bus", s.busID, "table", t.what, "peer_bus", rec.recordBusID(),
		"config_seq", rec.recordSeq(), "floor_file", s.floorPath)
}

// applyLocked is the single entry into the tables, shared by replay and by the
// live fold. The caller must hold mu.
func (s *PeerStore) applyLocked(t *busTable, rec busScopedRecord, source string) error {
	// THE HIGH-WATER MARK IS RAISED FIRST, BEFORE ANY DECISION TO DISCARD.
	//
	// This ordering is the whole defence described on the configSeq field: a
	// sequence number this bus has already put on disk must never be issued
	// again, INCLUDING for a record that is about to be thrown away by the cap,
	// by the monotonicity rule or by the self-peer check. Invariant 1 states the
	// same rule for WAL indices in as many words. Move this below the checks and
	// the defect the security gate found comes straight back.
	if n := rec.recordSeq(); n > s.configSeq {
		s.configSeq = n
	}
	// A record naming OUR OWN bus is refused on every path, including replay. A
	// self-peer is a routing loop and a namespace collision at once, and this is
	// the boundary where the local id is known — the record's own validate
	// deliberately cannot make this check.
	if err := ValidatePeerBusID(s.busID, rec.recordBusID()); err != nil {
		return err
	}
	return t.upsert(rec, s.now(), s.log, s.busID, source)
}

// canonicalPeerRecord encodes a record and decodes it straight back, returning
// the durable bytes and the record they decode to.
//
// The round trip is the point, not a redundancy: the record folded into memory
// is then LITERALLY THE ONE REPLAY WILL PRODUCE from the same bytes — same UTC
// time, same normalisation of the key slices — so a live Apply and a replayed
// Apply can never hold records that differ. It also proves, before anything is
// written, that the record this bus is about to make durable is one it can read
// back and will accept (both Encode and Decode validate).
func canonicalPeerRecord(rec busScopedRecord) (json.RawMessage, busScopedRecord, error) {
	switch r := rec.(type) {
	case PeerRecord:
		body, err := r.Encode()
		if err != nil {
			return nil, nil, err
		}
		out, err := DecodePeerRecord(body)
		if err != nil {
			// Unreachable unless Encode and Decode disagree, which would be a
			// bug in this package rather than bad input. Surfaced rather than
			// ignored: it would otherwise appear later as a record that cannot
			// be replayed.
			return nil, nil, fmt.Errorf("relay: the encoded peer route for %s does not decode back: %w", r.BusID, err)
		}
		return body, out, nil
	case BusTrustRecord:
		body, err := r.Encode()
		if err != nil {
			return nil, nil, err
		}
		out, err := DecodeBusTrustRecord(body)
		if err != nil {
			return nil, nil, fmt.Errorf("relay: the encoded bus trust record for %s does not decode back: %w", r.BusID, err)
		}
		return body, out, nil
	default:
		return nil, nil, fmt.Errorf("%w: %T is not a durable peer-configuration record", ErrInvalidPeerRecord, rec)
	}
}
