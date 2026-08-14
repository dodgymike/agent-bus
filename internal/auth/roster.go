package auth

import (
	"crypto/ed25519"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MaxCertBindings bounds RosterEntry.CertBindings.
//
// The history is BOUNDED because rule 1 of the ENROL-SHAPE decision
// (DECISIONS.md, 2026-08-07) says so, and it says so because an unbounded
// per-agent list is the SAME CLASS OF DEFECT as the unbounded applied-key
// table: memory that grows with client behaviour and is never reclaimed. It is
// reachable without any cleverness — an agent that rotates its client
// certificate in a loop appends a binding per rotation — and every one of those
// bindings is also decoded off DISK during recovery, where the input is
// whatever the file holds rather than whatever a handler validated.
//
// 16 is generous against the intended use. Rotation serves two certificates at
// once (invariant 11), so the live set during a rollover is 2, and the rest is
// retired history an operator can still read.
//
// The bound was declared BEFORE anything populated the field, so that Decode
// enforced it from the first durable record rather than having it retrofitted
// after records were on disk. Since MTLS-BIND (2026-08-14) enrolment does
// populate it, with exactly ONE binding per enrolment — so nothing on the
// shipped path approaches 16, and the bound now guards the DECODE side, where
// the input is whatever the file holds.
const MaxCertBindings = 16

// CertBinding is one client certificate bound to an agent id, with the window
// during which this bus accepted it.
//
// It is a HISTORY entry, not a current-value field. A single
// current-fingerprint field would make a rotating agent look, for the duration
// of the rotation, like a DIFFERENT agent — precisely what invariant 1 says an
// id must never become. The bus already serves two certificates during its own
// rollover; this is the client-side mirror of that decision.
type CertBinding struct {
	// Fingerprint is sha256.Sum256(cert.Raw) — named explicitly by the
	// ENROL-SHAPE decision so nobody invents a different one (a fingerprint over
	// the SPKI, or over a PEM encoding, would be a second incompatible identity
	// for the same certificate).
	Fingerprint [32]byte

	// BoundAt is when this bus began accepting the certificate.
	BoundAt time.Time

	// RetiredAt is when it stopped being accepted; nil means the binding is
	// LIVE. Retirement is EXPLICIT, never implicit-by-supersession (rule 3 of
	// ENROL-SHAPE): a binding that silently aged out is indistinguishable from
	// one that was revoked, and those need different responses from an operator.
	//
	// Verification, when it lands, accepts ANY binding that is not retired —
	// not "the newest" — because during a rollover both are legitimately live.
	RetiredAt *time.Time
}

// RosterEntry is one enrolled agent: the server-minted id, the short name the
// client asked for, the PUBLIC half of the agent's Ed25519 AUTH keypair, and
// the enrolment provenance the rest of the AUTH/CRYPTO epics attach to it.
//
// The server never holds a private half — see the package doc. Everything in
// this struct is safe to hand to anyone who is already entitled to know the
// agent exists; a public key is public and a certificate fingerprint is a hash
// of a certificate the client presents on every connection.
//
// # Why fields nothing populated were declared UP FRONT — and they are populated now
//
// MessagingPublicKey, InviteID and CertBindings were all declared while NO code
// path wrote them, because of the ordering rule in DECISIONS.md (2026-08-07,
// ENROL-SHAPE). Nothing in this package was persisted before AUTH-3, so that was
// the LAST MOMENT the durable record could be shaped without a migration — and,
// because an agent id is bound to a keypair, a migration here is not a schema
// edit but a FORCED RE-ENROLMENT OF EVERY AGENT. Writing the record once with
// reserved-but-empty fields cost a few omitted JSON keys; deciding it three
// times would have cost three migrations.
//
// THE BET PAID OFF AND ALL THREE ARE NOW WRITTEN, each by the task that owned
// it, with no migration: MessagingPublicKey by RELAY-13, InviteID by
// INVITE-GATE, CertBindings by MTLS-BIND (2026-08-14). NONE of them is reserved
// any more. Empty remains an ordinary value for each — records written before
// the respective task have none — so a reader must treat empty as "not
// recorded", never as "malformed"; see each field.
type RosterEntry struct {
	// AgentID is the fully-qualified "<bus-id>.<name>-<n>" (invariant 2),
	// minted by the server (invariant 1). It is the routing and authorization
	// subject and is never reused.
	AgentID string

	// Name is the short name the client requested, byte-identical to the name
	// half of AgentID. It is kept separately so a caller does not have to
	// re-parse the id, and it is byte-identical rather than normalised because
	// ids.ValidateAgentName rejects alternate spellings instead of folding
	// them — see that function for why the counter key, the name here and the
	// name inside the id must be the same bytes.
	Name string

	// AuthPublicKey is the agent's AUTH public key, exactly
	// ed25519.PublicKeySize bytes. The AUTH keypair is DISTINCT from the
	// messaging keypair and the two are never conflated.
	//
	// It was called PublicKey until 2026-08-07. With a messaging key alongside
	// it the old name no longer says WHICH key it is, and this codebase has
	// repeatedly been bitten by names that quietly stopped meaning what they
	// said. The rename was free while nothing persisted the field and is a
	// breaking change the moment something does — which is this task.
	AuthPublicKey ed25519.PublicKey

	// MessagingPublicKey is the agent's second Ed25519 key, for message
	// signing.
	//
	// NO LONGER RESERVED: RELAY-13 populates it, from the enrolment request.
	// It remains OPTIONAL — an enrolment that sends no messaging key is
	// accepted and stores none — so Decode validates it only when it is
	// present: empty is an ordinary value, not a malformed key.
	//
	// It is a separate field rather than a reuse of AuthPublicKey because
	// auth/messaging key separation is already a standing distinction in this
	// package (see the package doc); collapsing them into one field would
	// collapse a distinction the design depends on.
	MessagingPublicKey ed25519.PublicKey

	// InviteID names the invite this enrolment redeemed — the provenance that
	// answers "who authorised this agent onto the bus" (invariant 3: enrolment
	// is invite-only).
	//
	// NO LONGER RESERVED: INVITE-GATE populates it, from the redemption, so an
	// invited enrolment records WHICH invite admitted it. It is EMPTY for an
	// un-invited enrolment, which this build still accepts (invariant 3's
	// invite-only end state is not yet enforced), and empty for every agent
	// enrolled before the field was written — revocation and audit join on it
	// only where it is present.
	InviteID string

	// Epoch is the enrolment epoch, the hub's id-reuse guard (hub's
	// enrolmentEpoch is a time.Time, and this is that type).
	//
	// TODAY IT EQUALS EnrolledAt. It is STORED rather than derived so the
	// durable record carries it and a restart does not have to reconstruct it,
	// and it exists as its own field so that a future re-key or rotation can
	// BUMP THE EPOCH without rewriting EnrolledAt — the two answer different
	// questions ("since when is this credential current" versus "when did this
	// agent join"), and one field cannot answer both once they diverge.
	Epoch time.Time

	// CertBindings is the BOUNDED HISTORY of client certificates bound to this
	// agent id, at most MaxCertBindings of them. It is a history and not a
	// single fingerprint — see CertBinding.
	//
	// NO LONGER RESERVED (MTLS-BIND, 2026-08-14). Enrolment populates it with
	// exactly ONE live binding when the enrolling connection presented a client
	// certificate, and with NONE when it did not — see
	// EnrolRequest.ClientCertFingerprint for why an absent certificate is still
	// accepted on this build. Nothing else writes it, and nothing retires a
	// binding yet, so on this build the field holds zero or one element.
	//
	// EMPTY IS THEREFORE STILL AN ORDINARY STATE, not a damaged record: every
	// agent enrolled before this field was populated has none, and so does every
	// agent that enrols over a connection carrying no certificate. A reader must
	// treat empty as "this agent has no certificate to cross-check against",
	// never as "this agent is unauthenticated" — those become the same thing
	// only when a future task makes a certificate mandatory per agent.
	CertBindings []CertBinding

	// EnrolledAt is when the server accepted the enrolment.
	EnrolledAt time.Time
}

// Roster is the set of enrolled agents, as this package needs it.
//
// It is an injected interface so a durable implementation can record the
// enrolment through the two-phase write path and rebuild the roster by replay
// without reshaping Service or any caller. The method set is deliberately the
// minimum the enrolment and session paths use.
//
// An implementation MUST be safe for concurrent use.
//
// # Two implementations ship here, and only one is durable
//
//   - MemoryRoster writes nothing, fsyncs nothing and is lost entirely on
//     restart. It is the default when NewService is given no roster, and it is
//     what the tests use. Nothing about it may be read as a durability claim.
//   - WALRoster records every enrolment through internal/wal's prepare/commit
//     path and rebuilds itself from the log at Open (invariants 4 and 5).
//
// Which one is in force is a property of the PROCESS THAT CONSTRUCTED THE
// SERVICE, not of this package. cmd/agent-bus/main.go injects the WALRoster
// (AUTH-7); see the "What is durable, what is not, and what is actually WIRED"
// section of doc.go, which is kept honest about the SHIPPED BINARY.
type Roster interface {
	// Put records a new enrolment, and MUST refuse it on either of TWO
	// uniqueness rules. They guard the same identity from opposite directions,
	// and an implementation that enforces only the first is silently weaker than
	// this interface promises.
	//
	//  1. A duplicate AgentID -> ErrDuplicateAgentID, rather than overwriting.
	//     An overwrite would rebind a live identity to a different keypair,
	//     which is the worst outcome available on this path (invariants 1
	//     and 3). This rule keeps one AGENT ID from naming two keypairs.
	//  2. A CertBindings fingerprint already live on a DIFFERENT agent ->
	//     ErrCertFingerprintBound (MTLS-BIND). This rule keeps one CERTIFICATE
	//     from naming two agents; without it a single key holder could
	//     authenticate as either, and the fingerprint would stop naming anybody
	//     (invariant 11). Use checkCertFingerprintUnbound, and run it in the
	//     SAME critical section as the insert — a check-then-write split across
	//     two lock acquisitions admits exactly the duplicate it refuses.
	//
	// Rule 1 is decided FIRST, so re-putting an existing agent reports the taken
	// id rather than a certificate colliding with itself.
	//
	// Note for TEST DOUBLES: rule 2 is the one an in-package double is likely to
	// omit, and omitting it is legitimate ONLY when the double exists to build a
	// state the real rosters refuse (both doubles in this package do). A double
	// that skips it by accident makes the enrolment path look correct while the
	// rule is not being exercised at all.
	Put(RosterEntry) error

	// Get returns the entry for agentID and whether it was found.
	Get(agentID string) (RosterEntry, bool)

	// AgentIDForCertFingerprint resolves a client-certificate fingerprint to the
	// ONE agent id holding it as a live binding (MTLS-BIND). It is the read half
	// of the fact invariant 11's cross-check needs; see certbind.go.
	//
	// It MUST fail closed and MUST NOT guess: ErrCertBindingUnknown when no
	// agent holds it, ErrCertBindingAmbiguous when more than one does. An
	// implementation that returned a holder in the ambiguous case would resolve
	// a duplicated certificate to a definite agent, which is the credential
	// confusion the cross-check exists to prevent.
	//
	// The fingerprint is sha256 over the certificate's DER exactly as it
	// arrived — internal/buscert.FingerprintOf, never a second implementation
	// and never a re-marshalled certificate.
	AgentIDForCertFingerprint(fp [32]byte) (string, error)

	// Len reports how many agents are enrolled. It backs admission control on
	// the unauthenticated enrolment route.
	Len() int

	// List returns every enrolled agent, sorted by AgentID, as deep copies.
	//
	// # Why a listing seam exists at all
	//
	// internal/hub used to keep its OWN roster view, fed by internal/httpapi
	// calling hub.NoteEnrolment on each accepted enrolment. That was honest only
	// while the two views had IDENTICAL LIFETIMES — true exactly as long as both
	// were memory-only.
	//
	// A durable roster breaks that. auth's roster survives a restart while the
	// hub's would start empty, so after a restart a recovered agent
	// AUTHENTICATES fine and then hub.publish refuses it: 403 ErrUnknownSender
	// for every send, 404 for every recipient, and both read paths fail closed.
	// The result is a bus that authenticates everyone and serves nobody — and it
	// fails in the direction that looks like the auth layer working, so it is
	// slow to diagnose.
	//
	// This method is the auth half of the fix. AUTH-7 landed the other half: the
	// hub now takes a hub.RosterSource and keeps no roster of its own, and
	// NoteEnrolment and its call site are DELETED. cmd/agent-bus adapts this
	// interface onto that one (hubRoster) — a live view, never a snapshot — so
	// the production caller of this method is GET /v1/agents.
	//
	// The entries carry each agent's ORIGINAL EnrolledAt, which is load-bearing
	// rather than incidental: it is the enrolment epoch every read path filters
	// with (store.Message.VisibleTo), so a rebuilt hub roster must carry the
	// instant the agent FIRST enrolled and not the instant it was recovered, or
	// a continuous agent silently loses its history at every restart.
	List() []RosterEntry
}

// MemoryRoster is the in-memory Roster. It is safe for concurrent use.
//
// It stays after WALRoster landed: it is the default for NewService when no
// roster is injected, and it is what the tests in this package exercise the
// enrolment and session logic through, so that logic is testable without a
// data directory and two fsyncs per case.
//
// The zero value is not usable; construct with NewMemoryRoster.
type MemoryRoster struct {
	// mu guards byID. A plain mutex rather than an RWMutex: reads are one map
	// lookup behind an fsync-free path at small scale, and a single mutex keeps
	// Put's check-then-insert trivially atomic (invariant 8).
	mu   sync.Mutex
	byID map[string]RosterEntry
}

// NewMemoryRoster returns an empty in-memory roster.
func NewMemoryRoster() *MemoryRoster {
	return &MemoryRoster{byID: make(map[string]RosterEntry)}
}

// Put implements Roster.
//
// The entry is DEEP-COPIED in: both public keys and the CertBindings slice. The
// caller passed untrusted slices it may still hold a reference to, and a caller
// that later mutated a backing array would be mutating a STORED CREDENTIAL —
// silently changing which private key authenticates as that agent, or which
// certificate is accepted for it. Copying costs 64 bytes plus a short slice per
// enrolment and removes the whole class. The slice is the same hazard one level
// up from the key: sharing the backing array shares every binding in it.
func (r *MemoryRoster) Put(e RosterEntry) error {
	if err := validateRosterEntryKeys(e); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byID[e.AgentID]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateAgentID, e.AgentID)
	}
	// The certificate axis of the same rule, in the SAME critical section as the
	// id check and the insert — see checkCertFingerprintUnbound. Split across two
	// lock acquisitions it would admit the duplicate it exists to refuse.
	if err := checkCertFingerprintUnbound(r.byID, e); err != nil {
		return err
	}
	r.byID[e.AgentID] = copyRosterEntry(e)
	return nil
}

// AgentIDForCertFingerprint implements Roster. See certFingerprintOwner for the
// three answers and why two of them are refusals.
func (r *MemoryRoster) AgentIDForCertFingerprint(fp [32]byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return certFingerprintOwner(r.byID, fp)
}

// Get implements Roster. The returned entry is a DEEP COPY, for the mirror of
// the reason Put copies on the way in: a caller must not be able to reach into
// the stored credential through the slices it was handed.
func (r *MemoryRoster) Get(agentID string) (RosterEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.byID[agentID]
	if !ok {
		return RosterEntry{}, false
	}
	return copyRosterEntry(e), true
}

// Len implements Roster.
func (r *MemoryRoster) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// List implements Roster. Entries are DEEP COPIES, sorted by AgentID.
//
// Sorted because the consumer is a roster rebuild and an agent listing, and an
// order that varies run to run (Go randomises map iteration) turns a stable
// listing into a flaky one and a rebuild into something no test can pin.
func (r *MemoryRoster) List() []RosterEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedCopies(r.byID)
}

// sortedCopies renders a roster map as a deep-copied slice sorted by AgentID.
// Shared by both implementations so their List cannot drift in either the
// copying or the ordering. The caller must hold the map's lock.
func sortedCopies(byID map[string]RosterEntry) []RosterEntry {
	out := make([]RosterEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, copyRosterEntry(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// validateRosterEntryKeys enforces the key-shape rules every Roster
// implementation applies before storing an entry. It is shared so MemoryRoster
// and WALRoster cannot drift apart about what a storable entry is.
//
// AuthPublicKey is REQUIRED and must be exactly ed25519.PublicKeySize.
// MessagingPublicKey is OPTIONAL today and is checked only when non-empty:
// empty is the reserved/unpopulated state (see RosterEntry), and rejecting it
// would refuse every enrolment this build can currently make.
//
// Checked here as well as in Service.Enrol, not instead of it: this is the
// boundary that hands keys to ed25519.Verify, and a key of the wrong length
// reaching Verify is a PANIC, not a false (see ErrInvalidPublicKey).
func validateRosterEntryKeys(e RosterEntry) error {
	if len(e.AuthPublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: agent %q presented a %d-byte auth public key, want exactly %d", ErrInvalidPublicKey, e.AgentID, len(e.AuthPublicKey), ed25519.PublicKeySize)
	}
	if len(e.MessagingPublicKey) != 0 && len(e.MessagingPublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: agent %q carries a %d-byte messaging public key, want either none or exactly %d", ErrInvalidPublicKey, e.AgentID, len(e.MessagingPublicKey), ed25519.PublicKeySize)
	}
	if len(e.CertBindings) > MaxCertBindings {
		return fmt.Errorf("%w: agent %q carries %d certificate bindings, the limit is %d", ErrInvalidRecord, e.AgentID, len(e.CertBindings), MaxCertBindings)
	}
	return nil
}

// copyRosterEntry returns a deep copy: both keys and the CertBindings slice
// (including each binding's RetiredAt pointer) are freshly allocated, so the
// returned value shares no mutable memory with e.
func copyRosterEntry(e RosterEntry) RosterEntry {
	out := e
	out.AuthPublicKey = append(ed25519.PublicKey(nil), e.AuthPublicKey...)
	if len(e.MessagingPublicKey) != 0 {
		out.MessagingPublicKey = append(ed25519.PublicKey(nil), e.MessagingPublicKey...)
	} else {
		// Normalised to nil rather than copied: an empty-but-non-nil key and an
		// absent key are the same reserved state, and keeping one of them
		// distinguishable would let a round trip through Encode/Decode change
		// the value.
		out.MessagingPublicKey = nil
	}
	if e.CertBindings != nil {
		out.CertBindings = make([]CertBinding, len(e.CertBindings))
		for i, b := range e.CertBindings {
			out.CertBindings[i] = b
			if b.RetiredAt != nil {
				t := *b.RetiredAt
				out.CertBindings[i].RetiredAt = &t
			}
		}
	}
	return out
}
