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
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

func (r PeerRecord) sameGenerationAs(o busScopedRecord) bool {
	other, ok := o.(PeerRecord)
	return ok && r.State == other.State && r.BaseURL == other.BaseURL && r.UpdatedAt.Equal(other.UpdatedAt)
}

// peerRecordJSON is the routing record's wire shape.
//
// "rec" REPEATS the wal.Entry.Kind inside the body, deliberately. Without it the
// two kinds' TOMBSTONES are byte-identical — both are {v, bus_id, config_seq,
// state:"removed", updated_at} — so a Kind mix-up in future wiring would land a
// route withdrawal in the trust table with no decode error at all, silently
// un-pinning a bus. It costs eleven bytes and turns that class of bug into a
// refusal at the door.
type peerRecordJSON struct {
	Version   int    `json:"v"`
	Rec       string `json:"rec"`
	BusID     string `json:"bus_id"`
	ConfigSeq uint64 `json:"config_seq"`
	State     string `json:"state"`
	BaseURL   string `json:"base_url,omitempty"`
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
	// The state-owned field is written ONLY for the state that owns it, so the
	// encoder cannot produce a record its own validate would refuse on the way
	// back in.
	if r.State == PeerRecordActive {
		j.BaseURL = r.BaseURL
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
	r := PeerRecord{
		BusID:     j.BusID,
		ConfigSeq: j.ConfigSeq,
		State:     state,
		BaseURL:   j.BaseURL,
		UpdatedAt: updatedAt,
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
// The shared table
// ---------------------------------------------------------------------------

// busTable is one bounded, bus-id-keyed table of records with a monotonic
// upsert. There are two instances — routes and trust — and they share this code
// so the two cannot drift into different admission rules.
type busTable struct {
	// what names the table in every log line: "peer route" or "bus trust".
	what string

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

	max       int
	retention time.Duration
}

func newBusTable(what string, max int, retention time.Duration) *busTable {
	return &busTable{what: what, entries: make(map[string]busScopedRecord), max: max, retention: retention}
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
func (t *busTable) lookup(busID string) (busScopedRecord, bool) {
	rec, ok := t.entries[strings.ToLower(busID)]
	if !ok || rec.recordBusID() != busID {
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
	BusID string

	// BaseURL is a BARE https origin — scheme, host, optional port.
	BaseURL string
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
	// still Apply (rebuild by replay) and read, which is what a read-only audit
	// of a data directory needs.
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
	// A durable floor file (the wal-index-floor pattern) would close both, and is
	// deliberately not built here: it is the durability layer's mechanism, and
	// this counter is not an id.
	configSeq uint64

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

// NewPeerStore validates opts and returns an empty store.
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
		routes:  newBusTable("peer route", max, retention),
		trust:   newBusTable("bus trust", max, retention),
	}
	if s.log == nil {
		s.log = logging.New(io.Discard, logging.LevelError)
	}
	if s.now == nil {
		s.now = time.Now
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
// A Put whose address is identical to the peer's current active route is a
// NO-OP that returns the existing record and writes nothing. That is not an
// optimisation: an operator's config-management run that re-applies the same
// peering would otherwise append a record on every pass and the log would grow
// with nothing having changed. Note the deliberate asymmetry with invariant 10's
// wire-level rule — there is no client, no idempotency key and no lost ack here,
// because this is an in-process operator API reached from an offline subcommand
// under the dirlock, not a route.
func (s *PeerStore) Put(cfg PeerConfig) (PeerRecord, error) {
	rec, err := s.write(s.routes, cfg.BusID, func(existing busScopedRecord, seq uint64, now time.Time) (busScopedRecord, bool, error) {
		if cur, ok := existing.(PeerRecord); ok && cur.State == PeerRecordActive && cur.BaseURL == cfg.BaseURL {
			return cur, false, nil
		}
		return PeerRecord{BusID: cfg.BusID, ConfigSeq: seq, State: PeerRecordActive, BaseURL: cfg.BaseURL, UpdatedAt: now}, true, nil
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
// Removing an already-removed route is a NO-OP that writes nothing; removing an
// unknown one is ErrUnknownPeer.
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
	if !known {
		existing = nil
		if len(t.entries) >= t.max {
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

// Lookup returns the ROUTE record for a bus id, in whatever state it holds. The
// caller must check State: a tombstone is a known record and NOT a usable route.
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
// # A DISCARD IS FAIL-CLOSED FOR A ROUTE AND FAIL-OPEN FOR A REVOCATION
//
// Be precise about this, because the comfortable half is easy to state and the
// uncomfortable half is the one that matters for a trust anchor.
//
// Apply itself never INSTALLS anything this bus did not already hold: the bus
// stays unknown, or keeps the generation already in memory. For a ROUTE that is
// fail-closed all the way — the worst outcome is that a configured peer is not
// restored and the operator re-applies it.
//
// BUT A DISCARD CAN ALSO FAIL TO REMOVE SOMETHING, and that is not a coding slip,
// it is what the complete-record design costs. Every entry carries the whole
// post-transition state, so if recovery discards a WITHDRAWAL — a torn tail, a
// bit-rotted frame, a filesystem snapshot rolled back past it, all of which
// invariant 6 requires us to survive rather than refuse to boot — the previous
// generation is the surviving truth and is reinstated. For the routes table that
// means a peer the operator un-peered is routable again. FOR THE TRUST TABLE IT
// MEANS A REVOKED PINNED SIGNING KEY IS PINNED AGAIN: revocation fails OPEN.
//
// The security gate reproduced exactly that by truncating eight bytes from a
// bus.wal tail. It is not reachable today (nothing outside this package
// constructs a PeerStore), and closing it needs a mechanism this record does not
// have — a revocation that cannot be un-said by losing one entry, which is a
// design question rather than a wording one. It is recorded as a P1 follow-up
// and stated here so that RELAY-17, which will verify against these pins, builds
// on what is true rather than on the comfortable half of it.
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyLocked(table, rec, "replay"); err != nil {
		s.log.Error("DISCARDING a peer-configuration record that could not be applied; that bus keeps the generation already in memory",
			"local_bus", s.busID, "table", table.what, "prepare_index", c.PrepareIndex, "commit_index", c.CommitIndex,
			"peer_bus", rec.recordBusID(), "config_seq", rec.recordSeq(), "state", rec.recordState().String(), "err", err)
	}
	return nil
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
