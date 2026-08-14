package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File names inside the credential store directory.
const (
	// storeFileName holds every enrolled identity, including PRIVATE KEYS.
	storeFileName = "identities.json"

	// lockFileName serialises read-modify-write cycles on storeFileName.
	lockFileName = "identities.lock"
)

// Permissions. These are asserted, not hoped for: the store holds Ed25519
// private keys, and a key that any local user can read is not a credential.
const (
	// storeDirMode is 0700 — owner only. A group- or world-EXECUTABLE
	// directory lets others traverse to the file even when the file itself is
	// 0600.
	storeDirMode fs.FileMode = 0o700

	// storeFileMode is 0600 — owner read/write only.
	storeFileMode fs.FileMode = 0o600
)

// storeFormatVersion is the schema version written into the file. A reader
// REFUSES a version it does not know rather than guessing: a private key
// misparsed as a public one is the kind of failure that is silent.
const storeFormatVersion = 1

// Lock timing. The lock is held for microseconds — a read, a mutation and an
// atomic rename — so a caller that waits a second has almost certainly hit a
// crashed holder rather than a busy one.
const (
	lockAcquireTimeout = 2 * time.Second
	lockPollInterval   = 20 * time.Millisecond

	// lockStaleAfter is when a lock file is treated as abandoned. A process
	// killed between creating the lock and releasing it would otherwise wedge
	// the store forever, and "delete your lock file" is not a remedy an agent
	// can carry out unattended.
	lockStaleAfter = 30 * time.Second
)

// Identity is the PUBLIC half of an enrolled agent: everything that is safe to
// print, log or hand to another process.
//
// The split from Credential is structural, not a convention. Anything that
// renders an identity for a human or for --json takes an Identity, so there is
// no code path on which a private key can be marshalled into output by
// forgetting a redaction step.
//
// The json tags are a documented contract surface (CONTRACTS-CLI.md).
type Identity struct {
	// AgentID is the SERVER-MINTED, fully-qualified `<bus-id>.<agent-id>`
	// (invariants 1 and 2). The client never chooses it.
	AgentID string `json:"agent_id"`

	// BusID is the bus half of AgentID, as the server reported it.
	BusID string `json:"bus_id"`

	// Name is the short name that was requested at enrolment. It is NOT an
	// identity — AgentID is.
	Name string `json:"name"`

	// BusURL is where this identity was enrolled, so a later command can reach
	// the right bus without being told again.
	BusURL string `json:"bus_url"`

	// BusFingerprints is the SET of bus certificates this identity accepts,
	// each the SHA-256 of a certificate's DER as 64 lowercase hex characters,
	// in the order they were pinned. It is empty when the enrolment was over
	// plaintext loopback and there was no certificate.
	//
	// It is stored so that "the trusted path is the easy path": the operator
	// supplies the fingerprint ONCE, at enrolment, from the invite — every
	// later command against this bus finds it here rather than needing the flag
	// again, and a bus whose certificate is outside the set is refused with no
	// further configuration. It is what makes this a PIN rather than a
	// per-invocation assertion.
	//
	// # Why a SET, since MTLS-ROTATE (2026-08-07)
	//
	// It held exactly one fingerprint when MTLS-PIN shipped, and the only
	// recovery from a changed certificate was `logout` plus a full re-enrolment.
	// DECISIONS.md E3 says a rotating bus serves TWO certificates during
	// rollover precisely so that "rotation must never require every client to
	// re-enrol — that would make routine key hygiene indistinguishable from a
	// security incident". One slot could not express that, so the first routine
	// rotation after MTLS-LISTENER would have wedged every enrolled agent at
	// once — and a wedged fleet is the pressure under which somebody proposes
	// letting --bus-fingerprint override the stored pin, turning a DETECTED
	// substitution into an ACCEPTED one.
	//
	// The set is bounded at MaxBusPins and every member is placed by an explicit
	// operator act (enrolment, or Store.AddBusPin after an OUT-OF-BAND
	// confirmation). It is NEVER extended with a certificate a bus presented.
	//
	// PUBLIC, like everything else in Identity: a certificate fingerprint is in
	// the bus's startup log and in every handshake. Safe to print, and printed
	// by `agent-busctl whoami` and `agent-busctl pin list`.
	//
	// omitempty, and storeFormatVersion is deliberately NOT bumped — the same
	// additive reasoning as MessagingKeySeed below. A credential written before
	// pinning existed is still perfectly valid, and refusing to load it would
	// lock an agent out of a bus it is legitimately enrolled on over a field it
	// never had. Such a credential simply has no pin, which is only usable
	// against the plaintext loopback bus it was created for; an https bus with
	// no pin is refused (transportSecurity). A credential written by the
	// single-pin build carries `bus_fingerprint` instead, and Store.load folds
	// it into this field — see migrateLegacyBusFingerprints.
	BusFingerprints []string `json:"bus_fingerprints,omitempty"`

	// PublicKey is the base64 (standard, padded) Ed25519 public key the bus
	// recorded. Public by definition; safe to print.
	PublicKey string `json:"public_key"`

	// EnrolledAt is the server's timestamp, verbatim.
	EnrolledAt string `json:"enrolled_at"`
}

// Credential is an Identity plus its SECRET half. It exists only inside the
// store and the signing path.
//
// It carries a String method that redacts the seed, so an accidental %v or %s
// in a log line prints the identity rather than the key. That is a safety net,
// not a licence: do not print credentials.
type Credential struct {
	Identity

	// PrivateKeySeed is the base64 (standard, padded) 32-byte Ed25519 seed.
	// SECRET. It never leaves this process except into the 0600 store file.
	//
	// The SEED is stored rather than the expanded 64-byte private key because
	// it is the canonical minimal representation: the expanded form contains
	// the public key as its second half, so storing it would keep two copies
	// of the same fact that could disagree.
	PrivateKeySeed string `json:"private_key_seed"`

	// MessagingKeySeed is the base64 (standard, padded) 32-byte Ed25519 seed of
	// this identity's MESSAGING key. SECRET, and NEVER SENT TO THE BUS.
	//
	// # Two keys, and why they must not be one key
	//
	// PrivateKeySeed above is the AUTH key: it proves this agent to the BUS, the
	// bus holds its public half, and the bus is the only party that ever checks
	// it. MessagingKeySeed proves this agent to its PEERS. It is the one a
	// COMPROMISED BUS CANNOT FORGE — which is the entire value of message
	// signing, and which collapses the moment the two are the same key, because
	// the bus already holds the auth public key and could then be the one
	// deciding what a "verified" message from this agent says.
	//
	// # Where it comes from: enrolment, or first use for an older credential
	//
	// Since RELAY-13 it is MINTED AT ENROLMENT and its public half is registered
	// with the bus in the same request (see Enrol and pendingEnrolment). That is
	// what lets the origin bus later ATTEST `fq-agent-id -> messaging public key`
	// to a peer bus: a bus cannot attest a key it never recorded.
	//
	// # Optional and ADDITIVE, on purpose
	//
	// It is omitempty and storeFormatVersion is deliberately NOT bumped: a
	// credential written before signing existed is still perfectly valid, and
	// refusing to load it would lock an agent out of a bus it is legitimately
	// enrolled on over a field it never needed. Such a credential gets a key
	// MINTED ON FIRST USE (Store.EnsureMessagingKey), under the store lock, and
	// the minting is the only write. That path is now the LEGACY one rather than
	// the normal one, and it stays because the bus has no route to add a
	// messaging key to an existing enrolment — the field is write-once server
	// side, so an agent that enrolled without one keeps a key its own bus cannot
	// attest until it re-enrols under a new id.
	//
	// # The PRIVATE half never leaves the machine
	//
	// Nothing sends the seed, nothing prints it, and there is no route that
	// accepts one. Only the PUBLIC half is ever handed out: to the bus at
	// enrolment as `messaging_public_key`, and to a peer out of band.
	MessagingKeySeed string `json:"messaging_key_seed,omitempty"`

	// IdempotencyKey is the key this enrolment was accepted under.
	//
	// It is kept AFTER success, not discarded, so that re-running the same
	// enrol command is answered from here instead of going back to the bus.
	// Without it, a repeat would generate a fresh key pair and present the old
	// key with a new payload — which invariant 10 defines as a protocol
	// violation: the bus REJECTS AND LOGS it (it does NOT disconnect; narrowed
	// 2026-08-08), and the enrolment simply fails. Remembering the key is what
	// makes the obvious human action (run it again) safe.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// String redacts BOTH private halves. See the type comment, and note that a new
// secret field added to this struct MUST be added here too — the redaction is
// enumerated rather than derived, so a forgotten field prints in full.
func (c Credential) String() string {
	return fmt.Sprintf("Credential{AgentID:%s BusID:%s Name:%s BusURL:%s PrivateKeySeed:[REDACTED] MessagingKeySeed:[REDACTED]}",
		c.AgentID, c.BusID, c.Name, c.BusURL)
}

// PrivateKey expands the stored seed into a usable Ed25519 private key.
func (c Credential) PrivateKey() (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.Strict().DecodeString(c.PrivateKeySeed)
	if err != nil {
		return nil, wrapError(KindConfig, "credential",
			"the stored private key for "+c.AgentID+" is not valid base64",
			"the credential store is damaged; re-enrol with `agent-busctl enrol --name <name>`",
			err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, newError(KindConfig, "credential",
			fmt.Sprintf("the stored private key for %s is %d bytes, expected %d", c.AgentID, len(seed), ed25519.SeedSize),
			"the credential store is damaged; re-enrol with `agent-busctl enrol --name <name>`")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// damagedMessagingSeedRemedy is what to do when the stored messaging seed
// cannot be read back.
//
// It used to say "remove the messaging_key_seed field and mint a new one, then
// re-distribute the public key to your peers". That advice was correct only
// while the messaging key was purely local. Since RELAY-13 the BUS records the
// public half at enrolment and the field is WRITE-ONCE server side — there is no
// route to update it — so minting a replacement leaves the agent signing with a
// key its own bus still attests the predecessor of. Every peer that verifies
// through an attestation would then reject every message, and re-distributing by
// hand fixes only the peers that trust out of band. Re-enrolling is the only
// route that puts the bus and the agent back in agreement.
//
// It is stated as one value used by both damaged-seed branches so the two cannot
// drift apart, which is how one of them would keep the stale advice.
//
// It says "the bus recorded this messaging key at enrolment" as a flat fact
// where it strictly means a CONSERVATIVE ASSUMPTION: a credential whose key was
// minted locally by EnsureMessagingKey (the legacy path this file still keeps)
// has no bus-recorded key, and Credential carries no field that distinguishes
// the two, so the remedy cannot branch. Re-enrolling is correct in both cases
// and is the only advice that is never wrong; the alternative wording would be
// hedged enough to read as optional, on a path where following the old advice
// silently breaks every attestation-verifying peer.
const damagedMessagingSeedRemedy = "the credential store is damaged; the bus recorded this messaging key at enrolment and cannot be told a new one, so re-enrol with `agent-busctl enrol --bus <url> --name <name>` — you will get a NEW agent id, and minting a replacement key locally would leave you signing with a key the bus does not attest"

// MessagingPrivateKey expands the stored messaging seed into a usable Ed25519
// private key.
//
// A credential with no messaging seed is an ERROR here rather than a silent
// mint: minting is a WRITE, it belongs under the store lock, and a read-only
// accessor that quietly created key material would mean two goroutines could
// each mint a different key and one of them would sign with a key nobody else
// will ever hold. Call Store.EnsureMessagingKey first — Client.messagingKey
// does.
func (c Credential) MessagingPrivateKey() (ed25519.PrivateKey, error) {
	if c.MessagingKeySeed == "" {
		return nil, newError(KindConfig, "messaging key",
			"identity "+c.AgentID+" has no messaging key yet",
			"run `agent-busctl keygen` to mint one, then hand the printed public key to your peers")
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(c.MessagingKeySeed)
	if err != nil {
		return nil, wrapError(KindConfig, "messaging key",
			"the stored messaging key for "+c.AgentID+" is not valid base64",
			damagedMessagingSeedRemedy,
			err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, newError(KindConfig, "messaging key",
			fmt.Sprintf("the stored messaging key for %s is %d bytes, expected %d", c.AgentID, len(seed), ed25519.SeedSize),
			damagedMessagingSeedRemedy)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// MessagingPublicKey returns the standard base64 of the PUBLIC half of the
// messaging key — the value that is handed to a peer out of band.
//
// It is DERIVED from the seed on every call rather than stored beside it. The
// two could otherwise disagree, and a stored public key that does not match the
// stored private key is the worst possible failure here: peers would be handed a
// key that verifies nothing this agent signs, and the symptom (every message
// rejected, by everyone) points nowhere near the cause.
func (c Credential) MessagingPublicKey() (string, error) {
	priv, err := c.MessagingPrivateKey()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)), nil
}

// EnsureMessagingKey returns ref's credential, minting its messaging key first
// if it has none.
//
// # Why minting is a locked read-modify-write and not a lazy field initialiser
//
// The store is shared by parallel agents in this project by design, and the
// credential file is rewritten whole. Minting outside the lock means two
// processes generate two DIFFERENT keys, both write, and one wins: the loser
// spends the rest of its life signing with a key that is no longer on disk, and
// every peer it handed the losing public key to rejects everything it sends. The
// lock plus insert-if-absent makes the second caller adopt the first caller's
// key instead, which is the only outcome that keeps one identity to one key.
//
// A credential that ALREADY has a seed is returned untouched and nothing is
// written, so this is safe to call on every send.
func (s *Store) EnsureMessagingKey(ref string) (Credential, error) {
	// Read first, outside the lock: the overwhelmingly common case is that the
	// key already exists, and taking an exclusive file lock on every send to
	// discover that would serialise every parallel agent sharing this store
	// behind one another for nothing.
	if cred, err := s.Resolve(ref); err == nil && cred.MessagingKeySeed != "" {
		return cred, nil
	}

	var out Credential
	err := s.update(func(d *storeData) error {
		cred, rerr := resolveIn(*d, ref)
		if rerr != nil {
			return rerr
		}
		i := findCredential(d.Credentials, cred.AgentID)
		if i < 0 {
			return newError(KindConfig, "messaging key",
				"identity "+cred.AgentID+" is not in the credential store",
				"enrol with `agent-busctl enrol --bus <url> --name <name>`")
		}
		if d.Credentials[i].MessagingKeySeed == "" {
			seed := make([]byte, ed25519.SeedSize)
			if _, gerr := rand.Read(seed); gerr != nil {
				return wrapError(KindInternal, "messaging key",
					"cannot generate a messaging key pair: the system random source failed", "", gerr)
			}
			d.Credentials[i].MessagingKeySeed = base64.StdEncoding.EncodeToString(seed)
		}
		out = d.Credentials[i]
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	return out, nil
}

// pendingEnrolment is key material generated for an enrolment whose outcome we
// do not yet know.
//
// It exists because of a specific, reachable data-loss window: the key pair is
// generated locally, the public half is sent, and the bus mints an id. A
// process killed between the bus's 201 and the local save would leave an
// enrolment on the roster whose private key no longer exists anywhere — a
// roster slot that can never authenticate and cannot be cleaned up until
// AUTH-4's revocation surface exists.
//
// Persisting the seed FIRST closes that window, and it is what makes a RETRY
// possible at all. Invariant 10 says a retry is "same key + SAME payload"; the
// payload includes the public key, so retrying with a freshly generated key
// pair is "same key + DIFFERENT payload" — a protocol violation the bus
// rejects and logs (it does NOT disconnect; narrowed 2026-08-08). A client that
// cannot reproduce its original payload cannot legitimately retry, which is
// exactly what this record stores.
type pendingEnrolment struct {
	// IdempotencyKey is the key the enrolment was sent with; it is the record's
	// identity.
	IdempotencyKey string `json:"idempotency_key"`

	// Name and BusURL are the rest of the request, kept so a reuse of the same
	// key with DIFFERENT content can be refused locally instead of being sent
	// and answered with a 409 the caller learns nothing from.
	Name   string `json:"name"`
	BusURL string `json:"bus_url"`

	PublicKey string `json:"public_key"`

	// PrivateKeySeed is the base64 32-byte Ed25519 seed. SECRET.
	PrivateKeySeed string `json:"private_key_seed"`

	// MessagingKeySeed is the base64 32-byte Ed25519 seed of the MESSAGING key
	// this attempt registered. SECRET, and stored for the same reason
	// PrivateKeySeed is, one step further.
	//
	// The enrolment payload carries the messaging PUBLIC key (RELAY-13), so the
	// messaging key is now part of what makes a retry "same key + SAME payload".
	// Without this field a resumed `enrol --idempotency-key` would mint a FRESH
	// messaging key, present the old idempotency key with different content, and
	// be answered with a 409 — turning the one correct response to an
	// interrupted enrolment into a permanent failure, which is exactly what
	// invariant 10 exists to prevent.
	//
	// It is also the half the bus does NOT hold: the bus records the public key
	// and can attest it, and if this seed were regenerated after the bus had
	// recorded its predecessor, every peer verifying against the attested key
	// would reject everything this agent signs.
	//
	// omitempty, and an EMPTY value is meaningful rather than merely absent: a
	// pending record written before this field existed registered no messaging
	// key at all, so its retry must register none either. Enrol reads it that
	// way — see the resume path there.
	MessagingKeySeed string `json:"messaging_key_seed,omitempty"`

	// InviteID is the invite this attempt redeemed, or "" when it presented
	// none. The ID ONLY — the invite SECRET is a bearer credential and must
	// never reach the disk (see enrolRequestBody.InviteSecret, which is the one
	// place it is allowed to exist).
	//
	// It is here because the invite id is part of what the enrolment ASSERTS:
	// the server's idempotency fingerprint covers (name, auth key, messaging
	// key, invite_id), so resuming this key with a DIFFERENT invite is "same
	// key + DIFFERENT payload" — invariant 10's protocol violation — and is
	// answered 409. Without this field that mistake could only be caught by the
	// BUS, and the client's handling of that refusal used to DESTROY the key
	// material of an attempt the bus may already have applied. Recording the id
	// is what lets ClaimEnrolment refuse it here, before anything is sent and
	// before anything is dropped.
	//
	// omitempty, and an EMPTY value is AMBIGUOUS rather than merely absent: it
	// is either a genuinely un-invited attempt or a record written before this
	// field existed. ClaimEnrolment therefore refuses only a stored id that
	// DISAGREES, never an absent one — refusing an absent one would strand the
	// legitimate resume of an interrupted enrolment, which is the one thing
	// this record exists for. Enrol.enrolFailed's keep-on-resume rule is what
	// protects the ambiguous case.
	InviteID string `json:"invite_id,omitempty"`

	// CreatedAt is when the record was written, RFC3339. Used only for
	// pruning; nothing depends on its accuracy.
	CreatedAt string `json:"created_at"`
}

// String redacts BOTH seeds. See Credential.String's comment: this is the same
// safety net for the same reason, and it exists for the same failure mode —
// no code path formats a pendingEnrolment today, but the whole point of a
// redacting String() is that it protects the field BEFORE someone adds the
// %v that would print it, not after.
//
// It carries TWO secrets since RELAY-13 — the messaging seed is minted at
// enrolment rather than on first send — and, exactly as Credential.String's
// comment warns, the redaction is enumerated rather than derived, so a new
// secret field added above must be added here too.
//
// InviteID is PRINTED, not redacted: it is a NAME and not a credential (the
// same judgement EnrolResult.InviteID makes), and it is what tells an operator
// which single-use invite this in-flight attempt belongs to. The invite SECRET
// is not a field of this struct at all, which is the stronger guarantee.
func (p pendingEnrolment) String() string {
	return fmt.Sprintf("pendingEnrolment{IdempotencyKey:%s Name:%s BusURL:%s PublicKey:%s PrivateKeySeed:[REDACTED] MessagingKeySeed:[REDACTED] InviteID:%s CreatedAt:%s}",
		p.IdempotencyKey, p.Name, p.BusURL, p.PublicKey, p.InviteID, p.CreatedAt)
}

// pendingTTL is how long an unresolved enrolment's key material is kept.
//
// Long enough to survive an outage a human sleeps through, short enough that
// the store does not accumulate secrets for enrolments nobody will ever
// complete.
const pendingTTL = 24 * time.Hour

// storeData is the on-disk document. Unexported: the file layout is this
// package's business, and every caller goes through a Store method.
type storeData struct {
	Version int `json:"version"`

	// Current is the AgentID selected by `use`. It may name an identity that
	// no longer exists (a store edited by hand); readers treat that as "no
	// current identity" rather than an error.
	Current string `json:"current"`

	Credentials []Credential `json:"identities"`

	// Pending holds key material for enrolments still in flight. See
	// pendingEnrolment.
	Pending []pendingEnrolment `json:"pending,omitempty"`
}

// Store is the on-disk credential store: a 0700 directory holding one 0600
// JSON file.
//
// Concurrency. Every mutation is a read-modify-write under an exclusive lock
// file and lands through an atomic rename, because parallel agents genuinely
// do share one config directory in this project. Without the lock, two
// simultaneous enrolments would silently lose one PRIVATE KEY — and the agent
// whose key vanished holds a server-side enrolment it can never authenticate.
type Store struct {
	dir string

	// warnMu guards warnings. It is a real requirement, not defensive tidiness:
	// warnings are NOT "written once at open" — an earlier version of this
	// comment said so and that claim is what made the unlocked read look safe.
	// The cursor file is read when a watch STARTS, long after OpenStore
	// returned, and every damaged-cursor condition appends here (cursorstore.go).
	// An agent EMBEDDING this package — an explicit audience of invariant 7 —
	// can therefore run Watch on one goroutine and Warnings on another, which
	// without this lock is a data race on the slice header itself.
	warnMu sync.Mutex

	// warnings records conditions an operator must be TOLD about rather than
	// silently repaired — see OpenStore, and Cursor for the ones found later.
	// Read and written only under warnMu.
	warnings []string
}

// OpenStore opens (creating if needed) the credential store at dir.
//
// It creates the directory 0700 and, when the directory or the credential file
// already exists with looser permissions, TIGHTENS it AND RECORDS A WARNING.
// os.MkdirAll is a no-op on an existing directory — it does not chmod — so a
// store created under a wider umask would otherwise stay group-readable
// forever, which is exactly the trap scripts/bus-serve.sh documents for the
// server's data dir.
//
// The warning is the part that matters, and it is why this does not simply fix
// the mode and move on. A file of Ed25519 private keys that was EVER readable
// by another local user must be assumed compromised; quietly chmodding it
// makes the evidence disappear and leaves the operator believing a credential
// is private when it may not be. Callers surface Warnings on stderr.
func OpenStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, usagef("store", "pass --identity <dir> or set "+EnvIdentityDir, "no credential store directory")
	}
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return nil, wrapError(KindConfig, "store",
			"cannot create the credential store directory "+dir,
			"check the path is writable, or point --identity somewhere else",
			err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, wrapError(KindConfig, "store", "cannot stat the credential store directory "+dir, "check the path is readable", err)
	}
	if !info.IsDir() {
		return nil, newError(KindConfig, "store",
			dir+" exists and is not a directory",
			"point --identity at a directory, or move the file out of the way")
	}

	s := &Store{dir: dir}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(dir, storeDirMode); err != nil {
			return nil, wrapError(KindConfig, "store",
				fmt.Sprintf("the credential store directory %s is mode %#o and could not be tightened to %#o", dir, perm, storeDirMode),
				"run: chmod 700 "+dir,
				err)
		}
		s.warnf("credential store directory %s was mode %#o (others could traverse it); tightened to %#o", dir, perm, storeDirMode)
	}
	if finfo, ferr := os.Stat(s.Path()); ferr == nil {
		if perm := finfo.Mode().Perm(); perm&0o077 != 0 {
			if err := os.Chmod(s.Path(), storeFileMode); err != nil {
				return nil, wrapError(KindConfig, "store",
					fmt.Sprintf("the credential file %s is mode %#o and could not be tightened to %#o", s.Path(), perm, storeFileMode),
					"run: chmod 600 "+s.Path(),
					err)
			}
			s.warnf("credential file %s was mode %#o (readable by other local users); tightened to %#o — treat every key in it as compromised and re-enrol",
				s.Path(), perm, storeFileMode)
		}
	}
	return s, nil
}

func (s *Store) warnf(format string, args ...interface{}) {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	s.warnings = append(s.warnings, fmt.Sprintf(format, args...))
}

// Warnings returns conditions the operator should be told about, in the order
// they were found. It returns a copy, taken under warnMu, so it is safe to call
// from a goroutine other than the one driving the store.
func (s *Store) Warnings() []string {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	out := make([]string, len(s.warnings))
	copy(out, s.warnings)
	return out
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the full path of the credential file.
func (s *Store) Path() string { return filepath.Join(s.dir, storeFileName) }

// load reads the store. A missing file is an EMPTY store, not an error: the
// first run of `enrol` must not have to be preceded by an init step.
func (s *Store) load() (storeData, error) {
	b, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return storeData{Version: storeFormatVersion}, nil
	}
	if err != nil {
		return storeData{}, wrapError(KindConfig, "store",
			"cannot read the credential store "+s.Path(),
			"check the file is readable by this user",
			err)
	}
	var d storeData
	if err := json.Unmarshal(b, &d); err != nil {
		return storeData{}, wrapError(KindConfig, "store",
			"the credential store "+s.Path()+" is not valid JSON",
			"move it aside and re-enrol; a damaged store is not repaired automatically because the private keys in it are unrecoverable either way",
			err)
	}
	if d.Version == 0 {
		d.Version = storeFormatVersion
	}
	if d.Version != storeFormatVersion {
		return storeData{}, newError(KindConfig, "store",
			fmt.Sprintf("the credential store %s is format version %d, but this build understands version %d", s.Path(), d.Version, storeFormatVersion),
			"upgrade agent-busctl, or move the store aside and re-enrol")
	}
	migrateLegacyBusFingerprints(b, &d)
	return d, nil
}

// migrateLegacyBusFingerprints folds the single-pin `bus_fingerprint` field
// written by the MTLS-PIN build into Identity.BusFingerprints.
//
// # Why this is a second decode rather than a field on Identity
//
// The obvious shape — keep a legacy field on Identity, or give Identity an
// UnmarshalJSON — is a trap in Go. Credential EMBEDS Identity, so an
// UnmarshalJSON on Identity is PROMOTED to Credential and becomes the method
// json uses for the whole credential: the private key seed would silently stop
// being read, and every enrolled identity would lose its key on the next load.
// A second, narrow decode has no such reach. Keeping a legacy exported field
// instead would put two spellings of one fact in the struct, in --json output
// and in the file, which is the disagreement Credential.MessagingPublicKey's
// comment already argues against.
//
// The migration is one-way and silent-on-write: once anything writes the store,
// the legacy key is gone and only `bus_fingerprints` remains.
//
// A DOWNGRADE has two distinct consequences and both must be stated, because
// stating only the first reads as reassurance:
//
//   - An older binary READING that store sees no pin and therefore refuses to
//     speak https to the bus (transportSecurity). It fails CLOSED, which is the
//     right direction, rather than connecting unverified.
//   - An older binary WRITING that store re-marshals the credentials without a
//     `bus_fingerprints` field it does not know, and the accept-set is
//     PERMANENTLY LOST. That is not a silent downgrade of trust — the identity
//     is then unpinned and https is refused — but it is unrecoverable from the
//     file, so the operator must re-pin. Store.AddBusPin deliberately permits an
//     https identity with an empty set for exactly this case, so the recovery is
//     `agent-busctl pin add <hex>` rather than a full re-enrolment.
//
// A record that already has bus_fingerprints is left alone: the new field wins,
// so a store written by this build is never reinterpreted through the old one.
func migrateLegacyBusFingerprints(raw []byte, d *storeData) {
	var legacy struct {
		Credentials []struct {
			AgentID        string `json:"agent_id"`
			BusFingerprint string `json:"bus_fingerprint"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		// Unreachable: the caller has already decoded this document. A failure
		// here means no legacy values are recoverable, which leaves the affected
		// identity unpinned — and unpinned means REFUSED over https, never
		// unverified.
		return
	}
	for _, l := range legacy.Credentials {
		if l.BusFingerprint == "" {
			continue
		}
		i := findCredential(d.Credentials, l.AgentID)
		if i < 0 || len(d.Credentials[i].BusFingerprints) > 0 {
			continue
		}
		d.Credentials[i].BusFingerprints = []string{l.BusFingerprint}
	}
}

// AddBusPin adds pin to ref's accept-set, under the store lock.
//
// See Client.AddBusPin for what this is for and why the fingerprint must have
// been confirmed out of band. The refusals here are the substantive part:
//
//   - A PLAINTEXT identity cannot gain a pin. It enrolled over http, where there
//     is no certificate, so an accept-set for it would be a check that never
//     runs — the same reason transportSecurity refuses --bus-fingerprint on an
//     http URL. Re-enrol against the https URL instead.
//   - At MaxBusPins the add is refused rather than evicting the oldest (see
//     BusPinSet.With and MaxBusPins).
//   - Adding a pin already held succeeds and writes nothing new, so re-running
//     the command after an interrupted rollover is safe.
//
// It deliberately does NOT refuse an https identity whose set is EMPTY, even
// though enrolment cannot produce one. The security gate found the wedge: a
// downgrade to a build that does not know `bus_fingerprints` REWRITES the store
// without the field, so an upgraded identity can legitimately be https and
// unpinned — and refusing there would leave logout plus a full re-enrolment as
// the only recovery, which is the outcome this whole task exists to remove.
// On whether that "narrows" — an earlier draft said it strictly did, and the
// security gate asked for both paths to be written down, because they differ:
//
//   - Against a set that already has members, adding does not narrow; it widens
//     by one, bounded at MaxBusPins, every member operator-granted.
//   - Against an EMPTY set it goes from "refused, no pin" to "exactly this one
//     accepted". In lattice terms that is a widening too. It is justified
//     because the new pin has IDENTICAL PROVENANCE to an enrolment pin — an
//     operator argument, confirmed out of band — and because the state it
//     replaces is unusable rather than safe.
//
// Either way, nothing is accepted that an operator did not name.
func (s *Store) AddBusPin(ref string, pin BusFingerprint) (Identity, error) {
	var out Identity
	err := s.update(func(d *storeData) error {
		i, cred, err := locateForPin(*d, ref)
		if err != nil {
			return err
		}
		current, err := ParseBusPinSet(cred.BusFingerprints)
		if err != nil {
			return err
		}
		if isPlaintextBusURL(cred.BusURL) {
			return newError(KindUsage, "pin",
				"identity "+cred.AgentID+" enrolled against "+cred.BusURL+", which is a plaintext URL and presents no certificate",
				"a pin there would be a check that never runs. If this bus now serves TLS, enrol against its https URL with `agent-busctl enrol --bus <https-url> --bus-fingerprint <hex> --name <name>`")
		}
		updated, err := current.With(pin)
		if err != nil {
			return err
		}
		d.Credentials[i].BusFingerprints = updated.Strings()
		out = d.Credentials[i].Identity
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return out, nil
}

// RemoveBusPin retires pin from ref's accept-set, under the store lock.
//
// Removing the LAST pin is refused. An https identity with an empty set cannot
// connect at all (transportSecurity), so the command would read as a tidy-up
// and land as a lockout; `logout` is the operation that means "stop using this
// identity", and it says so.
//
// Removing a pin that is not held is an error rather than a no-op: the operator
// believes they retired a certificate, and letting a mistyped fingerprint
// report success would leave the real one still accepted.
//
// It parses the stored set LENIENTLY, dropping entries it cannot read, where
// AddBusPin refuses outright. The asymmetry is deliberate and the security gate
// found the case that needs it: a single unparseable fingerprint in a
// hand-edited store made this fail — while the over-cap message at
// Client.storedPins points the operator at exactly this command to repair it.
// Removal can only ever NARROW what is accepted, so proceeding past a garbage
// entry cannot admit anything; refusing to proceed can lock the store shut.
//
// It does NOT repair every damaged shape, and the reviewer gate was right that
// an earlier draft of this comment implied it did. A set of {garbage, valid}
// still has no way out through these commands: removing the valid entry hits
// the last-pin refusal below, and the garbage entry cannot even be NAMED,
// because Client.RemoveBusPin parses its argument strictly (as it must — the
// argument is operator input, not stored data). Such a store needs the file
// edited or the identity re-enrolled. That is a hand-edit-only state, and
// widening the argument parser to reach it would mean accepting a malformed
// fingerprint on the command line, which is a worse trade.
func (s *Store) RemoveBusPin(ref string, pin BusFingerprint) (Identity, error) {
	var out Identity
	err := s.update(func(d *storeData) error {
		i, cred, err := locateForPin(*d, ref)
		if err != nil {
			return err
		}
		current := parseBusPinSetLenient(cred.BusFingerprints)
		if !current.Contains(pin) {
			return newError(KindUsage, "pin",
				"identity "+cred.AgentID+" does not accept certificate "+pin.String(),
				"it accepts "+current.String()+"; list them with `agent-busctl pin list`")
		}
		if current.Len() == 1 {
			return newError(KindUsage, "pin",
				"refusing to remove the last pinned certificate from identity "+cred.AgentID,
				"an identity with no pin cannot connect to an https bus at all, so this would be a lockout rather than a tidy-up. Add the replacement first (`agent-busctl pin add <new>`) and retire this one after, or `agent-busctl logout "+cred.AgentID+"` if you mean to stop using the identity")
		}
		updated, _ := current.Without(pin)
		d.Credentials[i].BusFingerprints = updated.Strings()
		out = d.Credentials[i].Identity
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return out, nil
}

// isPlaintextBusURL reports whether a recorded bus URL is an http one, i.e. one
// that presents no certificate for a pin to check.
//
// It parses rather than prefix-matching, so "https://…" cannot be read as
// plaintext by a sloppy comparison. An UNPARSEABLE URL is reported as NOT
// plaintext: that direction merely allows a pin to be recorded against an
// address nothing can connect to, whereas the other would refuse a legitimate
// https identity over a stored value this function could not read.
func isPlaintextBusURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "http"
}

// locateForPin resolves ref to an index into d.Credentials, so a pin mutation
// edits the stored record rather than a copy of it.
func locateForPin(d storeData, ref string) (int, Credential, error) {
	cred, err := resolveIn(d, ref)
	if err != nil {
		return 0, Credential{}, err
	}
	i := findCredential(d.Credentials, cred.AgentID)
	if i < 0 {
		return 0, Credential{}, newError(KindConfig, "pin",
			"identity "+cred.AgentID+" is not in the credential store",
			"list the enrolled identities with `agent-busctl whoami --all`")
	}
	return i, d.Credentials[i], nil
}

// storeFile names one JSON document in the store directory, together with the
// lock that serialises writes to it and the exact words its failures are
// reported in.
//
// It exists so saveJSON, lockFile and sweepTempFiles are ONE implementation
// shared by identities.json and cursors.json (CLI-3-FU-STOREDEDUP). They were
// two copies until then — written apart only because store.go was outside the
// implementing agent's file-ownership boundary during a parallel wave — and they
// agreed on the day they were written, which is precisely the guarantee that
// decays without anything failing. What the original protects is a file of
// Ed25519 private-key seeds; a divergence between the copies is a lost-update
// bug on private keys, and the reasoning that makes the lock correct (below) is
// subtle enough that a second copy would eventually lose it.
//
// The nouns are carried rather than derived because the two documents' messages
// are not variations on one template: "the credential store lock" and "the
// cursor lock" do not share a stem, and inventing one would change strings a
// user reads for no benefit.
type storeFile struct {
	// op is the Error.Op these failures carry.
	op string

	// name is the document's file name inside s.dir, and the stem of its
	// ".tmp-*" temp files. Two descriptors must never share one.
	name string

	// lockName is the document's own lock file. Two descriptors must never
	// share one either: a shared lock would put the cursor hot loop in
	// contention with enrolment, which is exactly what the split avoids.
	lockName string

	// what names the document in a save failure — "the credential store".
	what string

	// lockWhat names it in a lock failure — "the credential store lock".
	lockWhat string

	// busyWhat names it in the lock-timeout REMEDY, which is phrased from the
	// reader's point of view: "another agent-busctl process is updating <this>".
	busyWhat string
}

// identitiesFile is the credential document: every enrolled identity, including
// PRIVATE KEYS.
var identitiesFile = storeFile{
	op:       "store",
	name:     storeFileName,
	lockName: lockFileName,
	what:     "the credential store",
	lockWhat: "the credential store lock",
	busyWhat: "the store",
}

// save writes the credential store atomically. See saveJSON.
func (s *Store) save(d storeData) error {
	d.Version = storeFormatVersion
	return s.saveJSON(identitiesFile, d)
}

// saveJSON writes one document atomically: a fresh 0600 temp file in the same
// directory, fsynced, then renamed over the target, then the directory itself
// fsynced so the rename survives a crash.
//
// The temp file is created 0600 with O_EXCL rather than written and chmodded,
// so there is no instant at which the private keys exist under a looser mode.
func (s *Store) saveJSON(f storeFile, v interface{}) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return wrapError(KindInternal, f.op, "cannot encode "+f.what, "", err)
	}
	body = append(body, '\n')

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return wrapError(KindInternal, f.op, "cannot generate a temporary file name", "", err)
	}
	target := filepath.Join(s.dir, f.name)
	tmp := filepath.Join(s.dir, f.name+".tmp-"+hex.EncodeToString(suffix[:]))

	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
	if err != nil {
		return wrapError(KindConfig, f.op, "cannot create a temporary file in "+s.dir, "check the directory is writable", err)
	}
	// From here on the temp file must not be left behind on any error path.
	cleanup := func() { _ = fh.Close(); _ = os.Remove(tmp) }

	if _, err := fh.Write(body); err != nil {
		cleanup()
		return wrapError(KindConfig, f.op, "cannot write "+f.what, "check for a full or read-only filesystem", err)
	}
	if err := fh.Sync(); err != nil {
		cleanup()
		return wrapError(KindConfig, f.op, "cannot flush "+f.what+" to disk", "check for a full or read-only filesystem", err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, f.op, "cannot close "+f.what, "check for a full or read-only filesystem", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, f.op, "cannot replace "+f.what+" "+target, "check the directory is writable", err)
	}
	// Fsync the DIRECTORY so the rename itself is durable. Without it the
	// file's contents are on disk but the name may not be, and a crash can
	// leave the store as it was before — losing a key the bus has already
	// recorded. Not fatal if the platform refuses it.
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// lock takes the credential store's exclusive lock. See lockFile.
func (s *Store) lock() (func(), error) { return s.lockFile(identitiesFile) }

// lockFile takes one document's exclusive lock and returns a release function.
//
// The lock is an O_EXCL file rather than flock(2) so this package stays
// portable stdlib-only (invariant 8). Its one weakness is that a holder which
// dies leaves the file behind, which lockStaleAfter handles — and breaking a
// stale lock is where the subtlety is.
//
// EVERY LOCK CARRIES AN OWNERSHIP TOKEN (pid + 16 random bytes), and both the
// break and the release are conditional on it. Without that, two processes
// waiting on the same abandoned lock race like this: A stats it, finds it
// stale, removes it and wins the O_EXCL create; B — which stat'd the OLD file
// a moment earlier — then removes A's LIVE lock and wins its own create. Both
// believe they hold the lock, both read-modify-write, and one whole-file
// update is lost. Since the credential file holds private keys, "lost update"
// means "lost identity". Comparing the token before unlinking closes both
// halves: a break only removes the exact file that was observed stale, and a
// release only removes a lock that is still ours.
//
// This reasoning is the reason the function is shared rather than copied. It
// lived in one copy and was REFERENCED by the other, which is a comment
// promising a property the code beside it no longer had to keep.
func (s *Store) lockFile(f storeFile) (func(), error) {
	path := filepath.Join(s.dir, f.lockName)
	deadline := time.Now().Add(lockAcquireTimeout)
	for {
		token, err := newLockToken()
		if err != nil {
			return nil, err
		}
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
		if err == nil {
			_, werr := fh.WriteString(token)
			cerr := fh.Close()
			if werr != nil || cerr != nil {
				// A lock we cannot prove we own is worse than no lock: the
				// release below would refuse to remove it and it would go
				// stale. Remove it now and report.
				_ = os.Remove(path)
				cause := werr
				if cause == nil {
					cause = cerr
				}
				return nil, wrapError(KindConfig, f.op,
					"cannot write "+f.lockWhat+" "+path,
					"check for a full or read-only filesystem",
					cause)
			}
			return func() { removeIfToken(path, token) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, wrapError(KindConfig, f.op,
				"cannot create "+f.lockWhat+" "+path,
				"check the directory is writable",
				err)
		}

		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			// Abandoned by a crashed process. Read the token we are about to
			// break so the removal cannot hit a lock somebody has taken since.
			if held, rerr := os.ReadFile(path); rerr == nil {
				removeIfToken(path, string(held))
			}
			// Fall through to the deadline check and the sleep rather than
			// spinning: if the remove failed (a sticky-bit directory, a lock
			// owned by another uid) an unconditional `continue` here would be
			// an unbounded hot loop that never times out.
		}

		if time.Now().After(deadline) {
			return nil, newError(KindConfig, f.op,
				"timed out waiting for "+f.lockWhat+" "+path,
				"another agent-busctl process is updating "+f.busyWhat+"; retry, or remove the lock file if no other process is running")
		}
		time.Sleep(lockPollInterval)
	}
}

// newLockToken mints an ownership token: the pid, for a human reading the file
// during an incident, plus 16 random bytes, which is what actually makes it
// unforgeable by coincidence (pids are reused).
func newLockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", wrapError(KindInternal, "store",
			"cannot generate a lock token: the system random source failed", "", err)
	}
	return fmt.Sprintf("%d %s\n", os.Getpid(), hex.EncodeToString(buf[:])), nil
}

// removeIfToken unlinks path only if its contents still equal token.
//
// There is a residual race — the file could be replaced between the read and
// the unlink — but it is bounded in a way the unconditional remove was not:
// the window is microseconds rather than the whole staleness interval, and the
// only way to lose is for a lock to be taken AND released AND retaken inside
// it. POSIX has no compare-and-unlink; this is as close as stdlib gets.
func removeIfToken(path, token string) {
	held, err := os.ReadFile(path)
	if err != nil || string(held) != token {
		return
	}
	_ = os.Remove(path)
}

// update runs mutate against the store under the lock and saves the result.
// mutate must not retain d.
//
// Two housekeeping steps run here, under the lock, on EVERY write rather than
// on one particular operation:
//
//   - expired pending records are pruned, so the documented 24h TTL is real
//     for any store that is written at all, not only for one that enrols again;
//   - abandoned temp files are swept, because each one is a complete 0600 copy
//     of every private key in the store and nothing else would ever remove it.
func (s *Store) update(mutate func(d *storeData) error) error {
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()

	s.sweepTempFiles(identitiesFile)

	d, err := s.load()
	if err != nil {
		return err
	}
	d.Pending = prunePending(d.Pending, time.Now())
	if err := mutate(&d); err != nil {
		return err
	}
	return s.save(d)
}

// sweepTempFiles removes leftovers from a save of f that was killed between
// creating the temp file and renaming it. Called only with f's lock held.
//
// The glob is scoped to f.name, and that scoping is load-bearing rather than
// incidental: the descriptors' globs are DISJOINT, so a cursor write can never
// delete an in-flight copy of the credential document (or the reverse) while the
// other writer holds a different lock.
func (s *Store) sweepTempFiles(f storeFile) {
	matches, err := filepath.Glob(filepath.Join(s.dir, f.name+".tmp-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// List returns every stored identity's PUBLIC half, sorted by agent id, and
// the currently selected agent id ("" when none is selected or the selection
// dangles).
func (s *Store) List() ([]Identity, string, error) {
	d, err := s.load()
	if err != nil {
		return nil, "", err
	}
	out := make([]Identity, 0, len(d.Credentials))
	for _, c := range d.Credentials {
		out = append(out, c.Identity)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	current := d.Current
	if current != "" && findCredential(d.Credentials, current) < 0 {
		current = ""
	}
	return out, current, nil
}

// Resolve finds the credential a command should act as.
//
// ref may be a fully-qualified agent id, or a short name when exactly one
// stored identity has it. An ambiguous short name is an ERROR that names the
// candidates rather than a guess — picking one would mean an agent could act
// as the wrong identity on a bus it did not intend.
//
// An empty ref means "the current selection".
func (s *Store) Resolve(ref string) (Credential, error) {
	d, err := s.load()
	if err != nil {
		return Credential{}, err
	}
	return resolveIn(d, ref)
}

func resolveIn(d storeData, ref string) (Credential, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		if d.Current == "" {
			return Credential{}, noIdentityError(d)
		}
		if i := findCredential(d.Credentials, d.Current); i >= 0 {
			return d.Credentials[i], nil
		}
		return Credential{}, newError(KindConfig, "identity",
			"the selected identity "+d.Current+" is not in the credential store",
			"pick one with `agent-busctl use <agent-id>`, or list them with `agent-busctl whoami --all`")
	}
	if i := findCredential(d.Credentials, ref); i >= 0 {
		return d.Credentials[i], nil
	}
	var matches []Credential
	for _, c := range d.Credentials {
		if c.Name == ref {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Credential{}, newError(KindConfig, "identity",
			"no enrolled identity matches "+ref,
			"list them with `agent-busctl whoami --all`, or enrol with `agent-busctl enrol --bus <url> --name <name>`")
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.AgentID)
		}
		sort.Strings(ids)
		return Credential{}, newError(KindConfig, "identity",
			fmt.Sprintf("%q matches %d enrolled identities: %s", ref, len(matches), strings.Join(ids, ", ")),
			"name the fully-qualified <bus-id>.<agent-id> instead")
	}
}

func noIdentityError(d storeData) *Error {
	if len(d.Credentials) == 0 {
		return newError(KindConfig, "identity",
			"no identity has been enrolled",
			"enrol with `agent-busctl enrol --bus <url> --name <name>`")
	}
	return newError(KindConfig, "identity",
		"no identity is selected",
		"select one with `agent-busctl use <agent-id>`, or list them with `agent-busctl whoami --all`")
}

func findCredential(creds []Credential, agentID string) int {
	for i := range creds {
		if creds[i].AgentID == agentID {
			return i
		}
	}
	return -1
}

// SetCurrent selects ref (an agent id or an unambiguous short name) and
// returns the identity now selected.
func (s *Store) SetCurrent(ref string) (Identity, error) {
	var selected Identity
	err := s.update(func(d *storeData) error {
		cred, err := resolveIn(*d, ref)
		if err != nil {
			return err
		}
		d.Current = cred.AgentID
		selected = cred.Identity
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return selected, nil
}

// Remove deletes ref from the store and reports what it removed and which
// identity is selected afterwards.
//
// When the removed identity was the current one, the selection falls back to
// the lowest remaining agent id — deterministic, so two agents that remove the
// same identity end up in the same state — or to "" when the store is empty.
func (s *Store) Remove(ref string) (removed []string, current string, err error) {
	err = s.update(func(d *storeData) error {
		cred, rerr := resolveIn(*d, ref)
		if rerr != nil {
			return rerr
		}
		i := findCredential(d.Credentials, cred.AgentID)
		d.Credentials = append(d.Credentials[:i], d.Credentials[i+1:]...)
		removed = []string{cred.AgentID}
		// Any in-flight attempt that would have produced this identity is dead
		// weight now, and it holds a private key. Drop it with the credential.
		d.Pending = removePending(d.Pending, cred.IdempotencyKey, cred.BusURL)
		if d.Current == cred.AgentID {
			d.Current = lowestAgentID(d.Credentials)
		}
		current = d.Current
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return removed, current, nil
}

// RemoveAll empties the store — credentials AND in-flight enrolments — and
// returns the agent ids it removed, sorted.
//
// Clearing Pending is not tidiness. `logout --all` promises the private keys
// are destroyed, and a pending record holds a private key for an enrolment the
// bus may well have applied before the answer was lost. Leaving those behind
// would mean a live roster entry whose key survives a wipe the operator
// believes was complete.
func (s *Store) RemoveAll() ([]string, error) {
	var removed []string
	err := s.update(func(d *storeData) error {
		for _, c := range d.Credentials {
			removed = append(removed, c.AgentID)
		}
		sort.Strings(removed)
		d.Credentials = nil
		d.Current = ""
		d.Pending = nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// Idempotency records are scoped to (key, bus URL), never to the key alone.
//
// The bus is the scope on the SERVER too: an idempotency key is remembered by
// one bus, and a different bus has never seen it. Scoping locally the same way
// means presenting the same key to a second bus is a fresh enrolment (correct)
// rather than a conflict (wrong), while presenting it to the SAME bus with
// different content is still caught before it is sent.

// FindApplied returns the credential already enrolled under (key, busURL).
func (s *Store) FindApplied(key, busURL string) (Credential, bool, error) {
	if key == "" {
		return Credential{}, false, nil
	}
	d, err := s.load()
	if err != nil {
		return Credential{}, false, err
	}
	for _, c := range d.Credentials {
		if c.IdempotencyKey == key && c.BusURL == busURL {
			return c, true, nil
		}
	}
	return Credential{}, false, nil
}

// PendingEnrolment is the PUBLIC view of an in-flight enrolment: everything
// except the private key. Its json tags are a documented contract surface
// (CONTRACTS-CLI.md).
//
// It exists so a killed enrolment is RECOVERABLE. The key material is on disk,
// but it is reachable only through its idempotency key — and until this
// existed, a process killed between the write and the answer had never printed
// that key anywhere, so the seed sat in the store permanently unusable while
// the bus held a roster slot nobody could authenticate as. Listing pending
// records is what turns "safely stored" into "actually recoverable".
type PendingEnrolment struct {
	IdempotencyKey string `json:"idempotency_key"`
	Name           string `json:"name"`
	BusURL         string `json:"bus_url"`

	// InviteID is the invite this attempt redeems, or "" when it presented
	// none. The ID only; the secret is not stored at all.
	//
	// Reported because the resume now DEPENDS on it: since
	// INVITE-CLIENT-FU-PENDINGINVITE a resume that presents a different invite
	// is refused locally, so "resume with --idempotency-key K" is only half an
	// instruction for an invited attempt. Naming the invite is what keeps the
	// record recoverable rather than merely listed.
	InviteID string `json:"invite_id,omitempty"`

	CreatedAt string `json:"created_at"`
}

// ListPending returns every in-flight enrolment, sorted by creation time, with
// no key material.
func (s *Store) ListPending() ([]PendingEnrolment, error) {
	d, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]PendingEnrolment, 0, len(d.Pending))
	for _, p := range prunePending(d.Pending, time.Now()) {
		out = append(out, PendingEnrolment{
			IdempotencyKey: p.IdempotencyKey,
			Name:           p.Name,
			BusURL:         p.BusURL,
			InviteID:       p.InviteID,
			CreatedAt:      p.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// FindPending returns the in-flight enrolment recorded under (key, busURL).
func (s *Store) FindPending(key, busURL string) (pendingEnrolment, bool, error) {
	if key == "" {
		return pendingEnrolment{}, false, nil
	}
	d, err := s.load()
	if err != nil {
		return pendingEnrolment{}, false, err
	}
	for _, p := range d.Pending {
		if p.IdempotencyKey == key && p.BusURL == busURL {
			return p, true, nil
		}
	}
	return pendingEnrolment{}, false, nil
}

// enrolClaim is the outcome of ClaimEnrolment.
type enrolClaim struct {
	// Applied is set when this (key, bus) was already enrolled successfully.
	// The caller must answer from it and send NOTHING.
	Applied *Credential

	// Pending is the key material to use. It is the record the caller offered,
	// or a pre-existing one it must reuse.
	Pending pendingEnrolment

	// Resumed reports that Pending pre-existed, i.e. this is a retry.
	Resumed bool
}

// ClaimEnrolment atomically decides what an enrolment attempt should do, in
// ONE locked read-modify-write.
//
// The three outcomes — already applied, resume an in-flight attempt, start a
// new one — must be decided under a single lock. Deciding them with separate
// unlocked reads is a check-then-act race with a very specific and very bad
// ending: two concurrent `enrol --idempotency-key K` both see nothing, both
// generate a DIFFERENT key pair, and both POST key K with a different
// public_key. That is "same key + different payload", which invariant 10
// classifies as a protocol violation — the bus answers 409, rejecting and
// logging it (it does NOT disconnect; narrowed 2026-08-08) — and one of the two
// private keys is overwritten and lost. The lost key is the damage here, not
// the socket.
//
// Insert-if-absent is what makes the loser correct rather than merely lucky:
// it gets the winner's key material back and its request is byte-identical, so
// its retry is legitimate.
func (s *Store) ClaimEnrolment(want pendingEnrolment, now time.Time) (enrolClaim, error) {
	var claim enrolClaim
	err := s.update(func(d *storeData) error {
		for i := range d.Credentials {
			c := d.Credentials[i]
			if c.IdempotencyKey != want.IdempotencyKey || c.BusURL != want.BusURL {
				continue
			}
			if c.Name != want.Name {
				return idempotencyConflict("enrol", want.IdempotencyKey, c.Name, c.BusURL)
			}
			applied := c
			claim.Applied = &applied
			return nil
		}
		for i := range d.Pending {
			p := d.Pending[i]
			if p.IdempotencyKey != want.IdempotencyKey || p.BusURL != want.BusURL {
				continue
			}
			if p.Name != want.Name {
				return idempotencyConflict("enrol", want.IdempotencyKey, p.Name, p.BusURL)
			}
			// The invite is part of the payload the bus fingerprints, so a
			// resume presenting a different one is refused HERE — the same
			// judgement the name check above makes, one field further, and for
			// a sharper reason: this record is the ONLY copy of the attempt's
			// two private key seeds, and sending the mismatch is what used to
			// get them dropped on the 409 that came back.
			//
			// Only a stored id that DISAGREES. An empty stored id is ambiguous
			// (see the field's comment) and must not be refused, or a legitimate
			// resume of a record written before this field existed becomes a
			// permanent failure with its key material stranded.
			if p.InviteID != "" && p.InviteID != want.InviteID {
				return inviteConflict("enrol", want.IdempotencyKey, p.InviteID, want.InviteID, p.BusURL)
			}
			claim.Pending = p
			claim.Resumed = true
			return nil
		}
		d.Pending = append(d.Pending, want)
		claim.Pending = want
		return nil
	})
	if err != nil {
		return enrolClaim{}, err
	}
	return claim, nil
}

// DropPending removes the in-flight record for (key, busURL). It is a no-op
// when there is none.
func (s *Store) DropPending(key, busURL string) error {
	return s.update(func(d *storeData) error {
		d.Pending = removePending(d.Pending, key, busURL)
		return nil
	})
}

// PromotePending stores cred and clears the in-flight record for
// (key, cred.BusURL), in ONE locked read-modify-write.
//
// Doing both in one cycle is what makes the outcome atomic from a reader's
// point of view: there is no instant at which the store holds a usable
// credential AND a pending record for the same enrolment.
// # A REPLACED credential does not inherit the old one's read position
//
// findCredential matches on AgentID ALONE, so enrolling again under an agent id
// already in the store OVERWRITES it. Since CLI-3-FU-URLKEY the watch cursor is
// keyed by (agent id, bus id) — both derivable from that same agent id — so the
// incoming credential would silently adopt the position the previous holder of
// that id had reached.
//
// That direction is the dangerous one. A cursor is a "everything up to here has
// been seen" watermark, and the bus's cursor is deliberately unsigned and NOT
// bus-scoped (internal/hub's DecodeCursor binds only the agent half), so a
// position minted elsewhere is accepted and only messages AFTER it are returned.
// The result is a SKIP — silent message loss — where every other failure mode in
// this client is a replay. Under the old bus_url key the two records could not
// collide because they sat under different URLs; re-keying removed that
// accidental separation, so the separation now has to be deliberate.
//
// Clearing costs a replay of the retained window for that identity, which
// at-least-once delivery already permits and any correct handler already
// tolerates. It is the cheap side of the trade by a wide margin.
//
// # It is UNCONDITIONAL, and that is the second half of the fix
//
// An earlier version cleared only when findCredential had matched, i.e. only on
// the overwrite path. That left the hole open one step further out: `logout`
// REMOVES the credential but does not touch cursors.json, so a logout followed
// by an enrolment lands on the APPEND path with the previous holder's position
// still sitting in the file, waiting under a key the new credential derives
// identically. Clearing whether or not anything was replaced closes both routes
// with less code than distinguishing them. A genuinely fresh identity has no
// record under its key, so the clear is a no-op — it cannot disturb another
// agent's position, which is what the "unrelated cursor" subtest pins.
//
// # Ordering and failure mode
//
// It runs AFTER the update rather than inside the mutate callback: the callback
// holds the identities lock, ClearCursor takes the cursors lock, and nesting the
// second inside the first would buy nothing. s.update's deferred release runs
// when update returns, so the identities lock is already released here.
//
// A FAILED clear is reported, not fatal. Failing the enrolment would be wrong —
// the credential is already durable at that point, so the caller would be told
// its enrolment failed when it had succeeded. But be precise about what is left
// behind, because the obvious guess is backwards: a surviving record is the
// PREVIOUS holder's position, so the risk is a SKIP, not a replay. That is why
// the remedy names --replay, which is the one thing that reliably steps around
// a position that is too far ahead.
func (s *Store) PromotePending(key string, cred Credential, makeCurrent bool) error {
	if cred.AgentID == "" || cred.PrivateKeySeed == "" {
		return newError(KindInternal, "store", "refusing to store an incomplete credential", "")
	}
	if err := s.update(func(d *storeData) error {
		if i := findCredential(d.Credentials, cred.AgentID); i >= 0 {
			d.Credentials[i] = cred
		} else {
			d.Credentials = append(d.Credentials, cred)
		}
		if makeCurrent || d.Current == "" {
			d.Current = cred.AgentID
		}
		d.Pending = removePending(d.Pending, key, cred.BusURL)
		return nil
	}); err != nil {
		return err
	}
	busID, err := cursorBusID(cred)
	if err != nil {
		// The credential is self-contradictory about its bus. Nothing can be
		// keyed, so there is nothing to clear and nothing that could be read
		// back later either.
		return nil
	}
	if cerr := s.ClearCursor(cred.AgentID, busID); cerr != nil {
		s.warnf("enrolled %s but could not clear the stored read position left under its key (%v); a position left there belongs to a PREVIOUS holder of this id and would make the next watch SKIP past messages — run `agent-busctl watch --replay` once, or delete %s",
			cred.AgentID, cerr, s.CursorPath())
	}
	return nil
}

func removePending(pending []pendingEnrolment, key, busURL string) []pendingEnrolment {
	out := pending[:0]
	for _, p := range pending {
		if p.IdempotencyKey == key && p.BusURL == busURL {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// prunePending drops records older than pendingTTL. A record whose timestamp
// cannot be parsed is KEPT: discarding key material because a timestamp is
// unreadable would turn a cosmetic defect into a lost identity.
func prunePending(pending []pendingEnrolment, now time.Time) []pendingEnrolment {
	out := pending[:0]
	for _, p := range pending {
		created, err := time.Parse(time.RFC3339Nano, p.CreatedAt)
		if err == nil && now.Sub(created) > pendingTTL {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func lowestAgentID(creds []Credential) string {
	lowest := ""
	for _, c := range creds {
		if lowest == "" || c.AgentID < lowest {
			lowest = c.AgentID
		}
	}
	return lowest
}
