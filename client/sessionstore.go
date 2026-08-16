package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// # Persisted sessions: OPT-IN, and a deliberate reversal
//
// A session is a BEARER credential. Until 2026-08-16 this package never wrote
// one to disk, and session.go said so as an invariant: writing one would add a
// stealable token at rest in exchange for saving a single round trip. That
// reasoning is still correct and is why persistence is OFF unless the caller
// asks for it by name.
//
// What changed is the OTHER side of the ledger. The bus holds each session for
// SessionLifetime (one hour), caps one agent at
// DefaultMaxActiveSessionsPerAgent (32), and EVICTS NOTHING — deliberately, so
// that a stolen key cannot be used to destroy the victim's live sessions. Under
// invariant 7 an agent drives the bus by SHELLING OUT to this CLI, and every
// invocation is a fresh process whose in-memory session cache dies with it. So
// each command burns one server-side session that then occupies a slot for a
// full hour. More than 32 commands in a rolling hour — roughly one every two
// minutes — and the agent locks ITSELF out of its own identity, for up to an
// hour, with no error it can act on and nothing it can do to recover.
//
// That was observed in production on 2026-08-15: one agent, 32 x HTTP 503 on
// session/complete, locked out while every other agent on the bus was fine.
// The discriminator was process shape — the healthy agents ran ONE long-lived
// `watch`; the broken one shelled out per action.
//
// So the trade is: a token at rest, readable only by this user, for at most an
// hour, versus an agent that bricks itself at an ordinary work rate. Neither
// side is free. The operator chooses, per invocation, and the default stays
// SAFE. See DECISIONS.md 2026-08-16.
//
// # What this file will NOT do
//
// It does not weaken any check that applies to a live session. A loaded token
// is still bound to one agent id and one bus URL, is still refused past its
// refresh point, and is still just an opaque handle the bus can invalidate at
// will — restarting the bus destroys every session whether or not a copy sits
// on disk. Persistence changes WHERE the token lives between two processes,
// never what it authorises.

// sessionFileVersion is the on-disk format version. A file carrying anything
// else is IGNORED rather than rejected: a session is disposable, so the useful
// behaviour on an unrecognised file is a silent re-handshake, not a hard error
// that strands the caller over a cache.
const sessionFileVersion = 1

// sessionFilePrefix is the stem of a persisted session file inside the
// credential store directory. One file per agent id, so two identities in one
// store cannot overwrite each other's token.
const sessionFilePrefix = "session-"

// persistedSession is the on-disk form. Its json tags are a contract surface
// (CONTRACTS-CLI.md).
//
// It records the BINDING as well as the token — agent id and bus URL — so a
// file that ends up beside the wrong identity, or is copied to a machine
// pointed at a different bus, is DISCARDED rather than presented. Presenting a
// token to the wrong bus would leak it to that bus.
type persistedSession struct {
	Version   int       `json:"version"`
	AgentID   string    `json:"agent_id"`
	BusURL    string    `json:"bus_url"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	RefreshAt time.Time `json:"refresh_at"`

	// LifetimeSeconds is carried so a reloaded session reports the same
	// lifetime it was issued with, rather than one inferred from what is left.
	LifetimeSeconds float64 `json:"lifetime_seconds"`
}

// sessionFileName returns the file name holding agentID's session.
//
// It REFUSES an id carrying anything outside the fully-qualified id alphabet.
// The id reaches here from a store document and from --as, so it is attacker
// influenced in exactly the way that turns a file name into a path: a '/' or a
// ".." would write outside the store. ids.BusIDPattern and the agent-id pattern
// both allow only [A-Za-z0-9_-], joined by '.', so anything else is not a valid
// id and has no business being turned into a path.
func sessionFileName(agentID string) (string, error) {
	if agentID == "" {
		return "", newError(KindUsage, "session", "no agent id to key a persisted session on", "select an identity with `agent-busctl use` or --as")
	}
	for _, r := range agentID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return "", newError(KindUsage, "session",
				fmt.Sprintf("agent id %q contains a character that cannot appear in a fully-qualified id", agentID),
				"fully-qualified ids are <bus-id>.<agent-id> over [A-Za-z0-9_-]")
		}
	}
	// A name that is only dots would still be a traversal ("..").
	if strings.Trim(agentID, ".") == "" {
		return "", newError(KindUsage, "session",
			fmt.Sprintf("agent id %q is not a usable file name", agentID),
			"select a real identity with `agent-busctl use` or --as")
	}
	return sessionFilePrefix + agentID + ".json", nil
}

// warn appends one warning for the CLI to drain to stderr between commands.
// It takes warnMu, never mu: a warning is raised on paths that already hold or
// are about to take mu, and folding the two locks together would deadlock.
func (c *Client) warn(msg string) {
	c.warnMu.Lock()
	c.warnings = append(c.warnings, msg)
	c.warnMu.Unlock()
}

// sessionFilePath returns the absolute path of agentID's session file.
func (c *Client) sessionFilePath(agentID string) (string, error) {
	name, err := sessionFileName(agentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.store.Dir(), name), nil
}

// loadPersistedSession reads a session previously written for cred.
//
// Every failure returns (nil, false): a session is a cache, and the correct
// response to a missing, stale, malformed or mis-bound file is to hand back
// nothing and let the caller run the handshake. The ONE thing it does loudly is
// warn when the file is readable by other users — that is not a cache miss, it
// is a credential that may already have been read, and staying silent about it
// would be the failure mode the mode check exists to prevent.
// busURL is the bus this client will ACTUALLY talk to, canonicalised. It is
// resolved through resolveBusURL — NOT read off the credential — because
// --bus / AGENT_BUS_URL overrides the credential's recorded URL (transport.go
// resolveBusURL). Comparing two values that both come off the credential is a
// tautology that fires never: it moved the connection without moving the check,
// so a token could be presented to whatever --bus named. Caught by the security
// gate 2026-08-16, demonstrated leaking to a rogue loopback listener.
func (c *Client) busURL() (string, bool) {
	u, err := c.resolveBusURL()
	if err != nil || u == nil {
		return "", false
	}
	return u.String(), true
}

func (c *Client) loadPersistedSession(cred Credential) (*session, bool) {
	path, err := c.sessionFilePath(cred.AgentID)
	if err != nil {
		return nil, false
	}

	// Lstat, NOT Stat: Stat resolves a symlink, so a link planted at this path
	// would have IsRegular() and the mode check evaluated against its TARGET
	// and an attacker-chosen token accepted silently. Gated in practice by the
	// 0700 store, but the failure was silent and the fix is one word.
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, false
	}
	if !fi.Mode().IsRegular() {
		return nil, false
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		// Do NOT present it. A token other local users can read is a token
		// they can spend, and the point of the warning is that this may
		// already have happened. Removing it here would destroy the evidence,
		// so it is left in place and named.
		// Move it ASIDE rather than leaving it: this same command goes on to
		// save a fresh 0600 file over this path, which would silently destroy
		// the evidence AND make the remedy ("run session logout") name a file
		// that no longer exists. Renaming preserves both. If the rename fails
		// the warning still stands.
		aside := path + ".INSECURE"
		renamed := os.Rename(path, aside) == nil
		msg := fmt.Sprintf(
			"the persisted session file %s is mode %04o, so other local users could read a bearer token; it was IGNORED and NOT used",
			path, perm)
		if renamed {
			msg += ". It has been moved to " + aside + " so the evidence survives; DELETE IT and treat the token as disclosed"
		} else {
			msg += ". Run `agent-busctl session logout` and treat the token as disclosed"
		}
		c.warn(msg)
		return nil, false
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var doc persistedSession
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	if doc.Version != sessionFileVersion || doc.Token == "" {
		return nil, false
	}
	// The binding checks. A file that does not name THIS identity on THIS bus
	// is not this process's session, whatever it is.
	if doc.AgentID != cred.AgentID {
		return nil, false
	}
	// Against the bus we are about to TALK to, canonicalised the same way
	// --bus is. A bus we cannot resolve is a miss: better a spare handshake
	// than a token offered to an unknown endpoint.
	target, ok := c.busURL()
	if !ok || doc.BusURL != target {
		return nil, false
	}

	s := &session{
		agentID:   doc.AgentID,
		token:     doc.Token,
		expiresAt: doc.ExpiresAt,
		refreshAt: doc.RefreshAt,
		lifetime:  time.Duration(doc.LifetimeSeconds * float64(time.Second)),
	}
	// Same usability rule as a session held in memory — no separate, laxer
	// path for a token that came off disk.
	if !c.sessionUsable(s) {
		return nil, false
	}
	return s, true
}

// savePersistedSession writes s for cred, atomically and 0600.
//
// A failure to save is NOT a failure of the operation that established the
// session: the caller has a perfectly good token in memory and the command
// should succeed. It warns instead, because silently not persisting would look
// identical to persistence working and would leave the caller believing the
// session-count problem is solved when it is not.
func (c *Client) savePersistedSession(cred Credential, s *session) {
	path, err := c.sessionFilePath(cred.AgentID)
	if err != nil {
		c.warn("cannot persist the session: " + err.Error())
		return
	}
	target, ok := c.busURL()
	if !ok {
		// Nothing to bind to means nothing safe to write. Silent: the command
		// itself succeeded and the session works in memory.
		return
	}
	doc := persistedSession{
		Version:         sessionFileVersion,
		AgentID:         s.agentID,
		BusURL:          target,
		Token:           s.token,
		ExpiresAt:       s.expiresAt,
		RefreshAt:       s.refreshAt,
		LifetimeSeconds: s.lifetime.Seconds(),
	}
	if err := writeSessionFile(path, doc); err != nil {
		c.warn("cannot persist the session to " + path + ": " + err.Error())
	}
}

// writeSessionFile writes doc to path atomically under 0600.
//
// It mirrors Store.saveJSON: a fresh O_EXCL temp file created 0600 (never
// written then chmodded, so there is no instant at which a bearer token exists
// under a looser mode), fsynced, renamed over the target. The directory fsync
// that saveJSON does is deliberately SKIPPED — a session is disposable, and
// losing the rename to a crash costs one handshake, so it is not worth the
// fsync on a path that runs on every command.
func writeSessionFile(path string, doc persistedSession) error {
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	// A RANDOM suffix, matching Store.saveJSON. A fixed name meant two
	// concurrent processes for the same agent — the normal shape once the flag
	// is on — raced on unlink-and-recreate, and a killed process left a
	// complete token in a file nothing swept.
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := path + ".tmp-" + hex.EncodeToString(suffix[:])
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(body); err != nil {
		_ = fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ForgetPersistedSession removes the persisted session for agentID.
//
// It reports whether a file was actually removed, so a caller can tell "there
// was one and it is gone" from "there was nothing to remove" — the CLI uses
// that to pick its exit code. A missing file is NOT an error.
//
// # This is LOCAL ONLY, and that limit is the whole point of saying so here
//
// It deletes this machine's copy of the token. It does NOT tell the bus, and
// the bus therefore keeps the session — and its slot against the per-agent cap
// — until it expires on its own, up to SessionLifetime away. There is no
// server-side session-end route to call; adding one is filed as its own task.
// Until then, `session logout` reduces exposure of the token at rest and does
// nothing whatsoever for the session count.
func (c *Client) ForgetPersistedSession(agentID string) (bool, error) {
	path, err := c.sessionFilePath(agentID)
	if err != nil {
		return false, err
	}
	// handshakeMu FIRST, held across the whole removal: without it a concurrent
	// handshake in an embedding client can save a fresh file microseconds after
	// this one deletes it, so logout silently leaves a live token behind. Lock
	// order is handshakeMu -> mu everywhere (see ensureSession); taking them in
	// this order preserves that.
	c.handshakeMu.Lock()
	defer c.handshakeMu.Unlock()

	// Drop the in-memory copy too, so a long-lived embedding client does not
	// keep presenting the token this call was asked to forget.
	c.mu.Lock()
	if c.session != nil && c.session.agentID == agentID {
		c.session = nil
	}
	c.mu.Unlock()

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, wrapError(KindConfig, "session logout",
			"cannot remove the persisted session file "+path,
			"check the file's permissions and that the store directory is writable", err)
	}
	// Temp files from interrupted writes hold the same token, as does a file
	// moved aside by the world-readable check. Removing the target while
	// leaving those behind would defeat the command.
	if matches, err := filepath.Glob(path + ".tmp-*"); err == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	_ = os.Remove(path + ".INSECURE")
	return true, nil
}

// String and GoString REDACT the token. The repo adds these pre-emptively to
// every type holding a secret (Credential at client/store.go, the invite blob)
// because the leak they prevent is a single %+v in a future error path, added
// by someone who had no reason to know this struct holds a bearer credential.
func (p persistedSession) String() string {
	return fmt.Sprintf("persistedSession{agent:%s bus:%s token:[REDACTED] expires:%s}",
		p.AgentID, p.BusURL, p.ExpiresAt.Format(time.RFC3339))
}

func (p persistedSession) GoString() string { return p.String() }
