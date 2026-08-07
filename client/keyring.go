package client

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The trust store: where a recipient gets the key it verifies WITH.
//
// # THE KNOWN BLOCKER, stated plainly
//
// No messaging keypair is registered at enrolment, and CRYPTO-4 — the
// server-attested key-bundle endpoint — does not exist. There is therefore NO
// WAY to obtain a sender's messaging public key FROM THE BUS. A recipient can
// verify a sender only if it obtained that sender's key OUT OF BAND and put it
// here, with `agent-busctl trust <agent-id> <base64-key>`.
//
// The consequence is not hidden and must not be softened: on a bus where nobody
// has exchanged keys, EVERY message is unverifiable, every body is discarded,
// and the cursor advances past all of them. That is the honest state of the
// world until CRYPTO-4 lands, and it is strictly better than the alternative.
//
// # DO NOT INVENT A FALLBACK
//
// There is no trust-on-first-use, no "trust the key the bus handed over", no
// verification-optional switch and no --insecure, and none may be added. Each of
// them turns the signature into theatre: a bus that can choose the verification
// key can forge any message from any sender, which is precisely the property the
// messaging key exists to deny it. If a test needs a trusted key, give it one —
// do not give the code a way to skip the check.

// TrustedKeysDirName is the trust store directory, inside the credential store
// directory. One file per peer.
const TrustedKeysDirName = "trusted-keys"

// trustedKeyFileSuffix is the extension of a trust-store file. The file NAME is
// the fully-qualified agent id, so `ls` answers "whose keys do I hold" without a
// parser, and a stale entry is removed with `rm`.
const trustedKeyFileSuffix = ".pub"

// trustedKeyFileMode is 0600 and trustedKeyDirMode is 0700 — the same posture
// the credential file has, and NOT for the same reason.
//
// These files hold PUBLIC keys; there is nothing here to keep secret. What is
// being protected is INTEGRITY: whoever can write this directory decides whose
// signatures this agent will accept, so a trust store any local user can modify
// is a trust store that proves nothing. Read permission is restricted only
// because a 0700 directory is the simplest way to get a 0700-equivalent write
// restriction, and the credential store is already 0700 anyway.
const (
	trustedKeyDirMode  fs.FileMode = 0o700
	trustedKeyFileMode fs.FileMode = 0o600
)

// maxTrustedKeyFileBytes bounds a trust-store file before it is read. A base64
// 32-byte key is 44 bytes; 4 KiB is three orders of magnitude of headroom and
// still finite, so a file that has been replaced with something enormous is
// refused rather than read into memory.
const maxTrustedKeyFileBytes = 4 << 10

// ErrNoTrustedKey reports that no messaging public key is held for a sender.
//
// It is a DISTINCT error from "the key I hold is malformed", because the two are
// different events with different remedies: the first is the ordinary state of
// the world before CRYPTO-4 (nobody has exchanged keys yet), the second means
// the local trust store is damaged or was written by hand incorrectly. Folding
// them together would bury an operator fault among a hundred routine ones.
var ErrNoTrustedKey = errors.New("client: no trusted messaging key for this sender")

// KeyRing resolves a fully-qualified sender id to the messaging public key this
// agent will verify that sender's messages with.
//
// It is an interface so an embedding agent can source trust from wherever it
// already keeps it — a config-management system, a hardware token, a directory
// service — without this package pretending to know. What it must NOT be is a
// source that takes the key from the message, the envelope or the bus: see the
// file comment, and verifySignedMessage's "WHICH KEY" section.
//
// A nil KeyRing is not an error condition to be worked around; it means "this
// agent trusts nobody", and every message is then unverifiable. Fail closed.
type KeyRing interface {
	// MessagingKey returns the trusted messaging public key for agentID, or an
	// error wrapping ErrNoTrustedKey when none is held.
	//
	// It must never return a key for an id it does not actually hold one for,
	// and must never derive one from the id.
	MessagingKey(agentID string) (ed25519.PublicKey, error)
}

// TrustedKey is one entry of the trust store: a peer and the key this agent
// will verify its messages with. Its json tags are a contract surface
// (CONTRACTS-CLI.md).
type TrustedKey struct {
	// AgentID is the fully-qualified `<bus-id>.<agent-id>` (invariant 2). It is
	// the ONLY thing a verification key is looked up by.
	AgentID string `json:"agent_id"`

	// PublicKey is the standard, padded base64 of the 32-byte Ed25519 messaging
	// public key. Public by definition; safe to print, and meant to be — this is
	// the value a peer hands over out of band.
	PublicKey string `json:"public_key"`
}

// DirKeyRing is the directory-backed trust store: one 0600 file per peer, named
// "<fully-qualified-agent-id>.pub", holding the standard base64 of a 32-byte
// Ed25519 messaging public key.
//
// The format is deliberately the dullest thing that could work — one key, one
// file, no index, no JSON envelope — because an operator has to be able to
// inspect, add and remove entries with `cat`, `cp` and `rm` during an incident,
// and because a corrupt index would take out trust in every peer at once
// whereas a corrupt file takes out one.
type DirKeyRing struct {
	dir string
}

// NewDirKeyRing returns a trust store rooted at dir. It creates nothing: a
// missing directory is an EMPTY trust store, which is the correct reading (this
// agent has been given nobody's key) and means the common read path never has to
// write to disk.
func NewDirKeyRing(dir string) *DirKeyRing { return &DirKeyRing{dir: dir} }

// Dir returns the directory this trust store reads and writes.
func (r *DirKeyRing) Dir() string { return r.dir }

// MessagingKey implements KeyRing.
//
// agentID is used as a FILE NAME, so it is validated first — and not merely for
// tidiness. An unvalidated id is a path-traversal primitive: a "sender" of
// "../../.ssh/id_ed25519.pub" would otherwise make this function read a file
// outside the trust store and treat its contents as a trusted key. serverIDPattern
// admits no '/' and no NUL, and the fully-qualified check rejects "." and ".."
// outright.
func (r *DirKeyRing) MessagingKey(agentID string) (ed25519.PublicKey, error) {
	if r == nil || r.dir == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoTrustedKey, safeText(agentID, 60))
	}
	path, err := r.path(agentID)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoTrustedKey, safeText(agentID, 60))
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the trusted messaging key for %s: %v", safeText(agentID, 60), err)
	}
	if info.Size() > maxTrustedKeyFileBytes {
		return nil, fmt.Errorf("the trusted messaging key file for %s is %d bytes; a key file holds one base64 line", safeText(agentID, 60), info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the trusted messaging key for %s: %v", safeText(agentID, 60), err)
	}
	return decodeMessagingPublicKey(strings.TrimSpace(string(raw)))
}

// Trust records pub as the key agentID's messages are verified with, replacing
// any key already held.
//
// Replacement is allowed and is NOT silent trust-on-first-use: something outside
// this process — a human, a deployment system — has decided this key belongs to
// this agent, and the whole point of an out-of-band trust store is that it says
// what it was told. What the client must never do is decide that for itself from
// something on the wire.
func (r *DirKeyRing) Trust(agentID string, pub ed25519.PublicKey) error {
	if r == nil || r.dir == "" {
		return newError(KindConfig, "trust", "no trust store directory", "pass --identity <dir> or set "+EnvIdentityDir)
	}
	if len(pub) != MessagingPublicKeySize {
		return newError(KindUsage, "trust",
			fmt.Sprintf("a messaging public key is %d bytes, got %d", MessagingPublicKeySize, len(pub)),
			"pass the base64 key exactly as the peer printed it with `agent-busctl keygen`")
	}
	path, err := r.path(agentID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.dir, trustedKeyDirMode); err != nil {
		return wrapError(KindConfig, "trust", "cannot create the trust store directory "+r.dir,
			"check the path is writable, or point --identity somewhere else", err)
	}
	body := base64.StdEncoding.EncodeToString(pub) + "\n"
	// Written through a temp file and an atomic rename so a killed process can
	// never leave a HALF-WRITTEN key behind. A truncated key is not a harmless
	// corruption: it is a key that fails to decode, which turns every message
	// from that peer into a rejection until somebody notices.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, trustedKeyFileMode)
	if err != nil {
		return wrapError(KindConfig, "trust", "cannot write the trust store entry for "+safeText(agentID, 60),
			"check the trust store directory is writable", err)
	}
	if _, werr := f.WriteString(body); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return wrapError(KindConfig, "trust", "cannot write the trust store entry for "+safeText(agentID, 60),
			"check for a full or read-only filesystem", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return wrapError(KindConfig, "trust", "cannot flush the trust store entry for "+safeText(agentID, 60),
			"check for a full or read-only filesystem", serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, "trust", "cannot close the trust store entry for "+safeText(agentID, 60),
			"check for a full or read-only filesystem", cerr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return wrapError(KindConfig, "trust", "cannot replace the trust store entry for "+safeText(agentID, 60),
			"check the trust store directory is writable", rerr)
	}
	return nil
}

// List returns every entry in the trust store, sorted by agent id.
//
// A file whose contents do not decode is reported as an ENTRY WITH AN EMPTY
// PublicKey rather than dropped, because "you hold a broken key for this peer"
// and "you hold no key for this peer" produce the same symptom — every message
// rejected — with completely different remedies, and a listing that hides the
// first sends the operator looking for the wrong one.
func (r *DirKeyRing) List() ([]TrustedKey, error) {
	if r == nil || r.dir == "" {
		return []TrustedKey{}, nil
	}
	entries, err := os.ReadDir(r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []TrustedKey{}, nil
	}
	if err != nil {
		return nil, wrapError(KindConfig, "trust", "cannot read the trust store directory "+r.dir,
			"check the directory is readable by this user", err)
	}
	out := make([]TrustedKey, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, trustedKeyFileSuffix) {
			continue
		}
		agentID := strings.TrimSuffix(name, trustedKeyFileSuffix)
		entry := TrustedKey{AgentID: agentID}
		if pub, kerr := r.MessagingKey(agentID); kerr == nil {
			entry.PublicKey = base64.StdEncoding.EncodeToString(pub)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

// path maps a fully-qualified agent id to its file, refusing anything that is
// not one. See MessagingKey for why this is a security check and not validation
// for its own sake.
func (r *DirKeyRing) path(agentID string) (string, error) {
	if _, err := qualifyingBusID(agentID); err != nil {
		return "", newError(KindUsage, "trust",
			"not a fully-qualified agent id: "+safeText(agentID, 60),
			"use the fully-qualified `<bus-id>.<agent-id>`; find it with `agent-busctl agents`")
	}
	return filepath.Join(r.dir, agentID+trustedKeyFileSuffix), nil
}

// decodeMessagingPublicKey parses the base64 form of a 32-byte Ed25519
// messaging public key.
//
// The length check is not cosmetic: ed25519.Verify PANICS on a wrong-size public
// key rather than returning false, so a key that never gets past this function
// is a key that can never reach that panic. Strict() base64 rejects
// non-canonical padding, so one key has exactly one spelling.
func decodeMessagingPublicKey(encoded string) (ed25519.PublicKey, error) {
	if encoded == "" {
		return nil, errors.New("the messaging public key is empty")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("the messaging public key is not valid standard base64: %v", err)
	}
	if len(raw) != MessagingPublicKeySize {
		return nil, fmt.Errorf("a messaging public key is %d bytes, got %d", MessagingPublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
