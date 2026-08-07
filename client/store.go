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
	// # Optional and ADDITIVE, on purpose
	//
	// It is omitempty and storeFormatVersion is deliberately NOT bumped: a
	// credential written before signing existed is still perfectly valid, and
	// refusing to load it would lock an agent out of a bus it is legitimately
	// enrolled on over a field it never needed. A credential without one gets a
	// key MINTED ON FIRST USE (Store.EnsureMessagingKey), under the store lock,
	// and the minting is the only write.
	//
	// # It never leaves the machine
	//
	// Nothing sends it, nothing prints it, and there is no route that accepts
	// one. Only the PUBLIC half is ever handed out, and only out of band —
	// `busctl keygen` prints it for a human to pass to a peer. When CRYPTO-4
	// lands, the public half is what gets registered; this field stays here.
	MessagingKeySeed string `json:"messaging_key_seed,omitempty"`

	// IdempotencyKey is the key this enrolment was accepted under.
	//
	// It is kept AFTER success, not discarded, so that re-running the same
	// enrol command is answered from here instead of going back to the bus.
	// Without it, a repeat would generate a fresh key pair and present the old
	// key with a new payload — which invariant 10 defines as a protocol
	// violation and the bus punishes with a disconnect. Remembering the key is
	// what makes the obvious human action (run it again) safe.
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
			"the credential store is damaged; re-enrol with `busctl enrol --name <name>`",
			err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, newError(KindConfig, "credential",
			fmt.Sprintf("the stored private key for %s is %d bytes, expected %d", c.AgentID, len(seed), ed25519.SeedSize),
			"the credential store is damaged; re-enrol with `busctl enrol --name <name>`")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

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
			"run `busctl keygen` to mint one, then hand the printed public key to your peers")
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(c.MessagingKeySeed)
	if err != nil {
		return nil, wrapError(KindConfig, "messaging key",
			"the stored messaging key for "+c.AgentID+" is not valid base64",
			"the credential store is damaged; remove the messaging_key_seed field for this identity and run `busctl keygen`, then re-distribute the new public key to your peers",
			err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, newError(KindConfig, "messaging key",
			fmt.Sprintf("the stored messaging key for %s is %d bytes, expected %d", c.AgentID, len(seed), ed25519.SeedSize),
			"the credential store is damaged; remove the messaging_key_seed field for this identity and run `busctl keygen`, then re-distribute the new public key to your peers")
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
				"enrol with `busctl enrol --bus <url> --name <name>`")
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
// pair is "same key + DIFFERENT payload" — a protocol violation that gets the
// client DISCONNECTED. A client that cannot reproduce its original payload
// cannot legitimately retry, which is exactly what this record stores.
type pendingEnrolment struct {
	// IdempotencyKey is the key the enrolment was sent with; it is the record's
	// identity.
	IdempotencyKey string `json:"idempotency_key"`

	// Name and BusURL are the rest of the request, kept so a reuse of the same
	// key with DIFFERENT content can be refused locally instead of being sent
	// and earning a disconnect.
	Name   string `json:"name"`
	BusURL string `json:"bus_url"`

	PublicKey string `json:"public_key"`

	// PrivateKeySeed is the base64 32-byte Ed25519 seed. SECRET.
	PrivateKeySeed string `json:"private_key_seed"`

	// CreatedAt is when the record was written, RFC3339. Used only for
	// pruning; nothing depends on its accuracy.
	CreatedAt string `json:"created_at"`
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
			"upgrade busctl, or move the store aside and re-enrol")
	}
	return d, nil
}

// save writes the store atomically: a fresh 0600 temp file in the same
// directory, fsynced, then renamed over the target, then the directory itself
// fsynced so the rename survives a crash.
//
// The temp file is created 0600 with O_EXCL rather than written and chmodded,
// so there is no instant at which the private keys exist under a looser mode.
func (s *Store) save(d storeData) error {
	d.Version = storeFormatVersion
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return wrapError(KindInternal, "store", "cannot encode the credential store", "", err)
	}
	body = append(body, '\n')

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return wrapError(KindInternal, "store", "cannot generate a temporary file name", "", err)
	}
	tmp := filepath.Join(s.dir, storeFileName+".tmp-"+hex.EncodeToString(suffix[:]))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
	if err != nil {
		return wrapError(KindConfig, "store", "cannot create a temporary file in "+s.dir, "check the directory is writable", err)
	}
	// From here on the temp file must not be left behind on any error path.
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }

	if _, err := f.Write(body); err != nil {
		cleanup()
		return wrapError(KindConfig, "store", "cannot write the credential store", "check for a full or read-only filesystem", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return wrapError(KindConfig, "store", "cannot flush the credential store to disk", "check for a full or read-only filesystem", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, "store", "cannot close the credential store", "check for a full or read-only filesystem", err)
	}
	if err := os.Rename(tmp, s.Path()); err != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, "store", "cannot replace the credential store "+s.Path(), "check the directory is writable", err)
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

// lock takes the store's exclusive lock and returns a release function.
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
// update is lost. Since the file holds private keys, "lost update" means "lost
// identity". Comparing the token before unlinking closes both halves: a break
// only removes the exact file that was observed stale, and a release only
// removes a lock that is still ours.
func (s *Store) lock() (func(), error) {
	path := filepath.Join(s.dir, lockFileName)
	deadline := time.Now().Add(lockAcquireTimeout)
	for {
		token, err := newLockToken()
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, storeFileMode)
		if err == nil {
			_, werr := f.WriteString(token)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				// A lock we cannot prove we own is worse than no lock: the
				// release below would refuse to remove it and it would go
				// stale. Remove it now and report.
				_ = os.Remove(path)
				cause := werr
				if cause == nil {
					cause = cerr
				}
				return nil, wrapError(KindConfig, "store",
					"cannot write the credential store lock "+path,
					"check for a full or read-only filesystem",
					cause)
			}
			return func() { removeIfToken(path, token) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, wrapError(KindConfig, "store",
				"cannot create the credential store lock "+path,
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
			return nil, newError(KindConfig, "store",
				"timed out waiting for the credential store lock "+path,
				"another busctl process is updating the store; retry, or remove the lock file if no other process is running")
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

	s.sweepTempFiles()

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

// sweepTempFiles removes leftovers from a save that was killed between
// creating the temp file and renaming it. Called only with the lock held.
func (s *Store) sweepTempFiles() {
	matches, err := filepath.Glob(filepath.Join(s.dir, storeFileName+".tmp-*"))
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
			"pick one with `busctl use <agent-id>`, or list them with `busctl whoami --all`")
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
			"list them with `busctl whoami --all`, or enrol with `busctl enrol --bus <url> --name <name>`")
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
			"enrol with `busctl enrol --bus <url> --name <name>`")
	}
	return newError(KindConfig, "identity",
		"no identity is selected",
		"select one with `busctl use <agent-id>`, or list them with `busctl whoami --all`")
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
	CreatedAt      string `json:"created_at"`
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
// classifies as a protocol violation — the bus answers 409 and DISCONNECTS —
// and one of the two private keys is overwritten and lost.
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
func (s *Store) PromotePending(key string, cred Credential, makeCurrent bool) error {
	if cred.AgentID == "" || cred.PrivateKeySeed == "" {
		return newError(KindInternal, "store", "refusing to store an incomplete credential", "")
	}
	return s.update(func(d *storeData) error {
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
	})
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
