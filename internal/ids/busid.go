package ids

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BusIDPattern is the one definition of a legal bus id.
// '.' is excluded on purpose: invariant 2 qualifies agents as "<bus-id>.<agent-id>".
const BusIDPattern = `^[A-Za-z0-9_-]{1,64}$`

var busIDRegexp = regexp.MustCompile(BusIDPattern)

// ValidateBusID validates an untrusted bus id. Returns a descriptive error.
func ValidateBusID(id string) error {
	if id == "" {
		return errors.New("bus id must not be empty")
	}
	if !busIDRegexp.MatchString(id) {
		return fmt.Errorf("bus id %q must match %s; in particular '.' is not allowed because it is the \"<bus-id>.<agent-id>\" qualification separator (invariant 2)", id, BusIDPattern)
	}
	return nil
}

// busIDRandBytes is the amount of crypto/rand entropy minted into a bus id:
// 10 bytes -> 16 base32 characters with no padding, plus the "bus-" prefix,
// for a 20-character id that always satisfies BusIDPattern.
const busIDRandBytes = 10

// GenerateBusID mints a fresh opaque bus id using crypto/rand.
func GenerateBusID() (string, error) {
	buf := make([]byte, busIDRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating bus id: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	id := "bus-" + strings.ToLower(enc)
	if err := ValidateBusID(id); err != nil {
		// Should never happen: the alphabet is a-z2-7 at a fixed length, which
		// always satisfies BusIDPattern. Kept as a defensive check because a
		// bus id that fails its own invariant must never reach a caller.
		return "", fmt.Errorf("generated bus id %q failed validation: %w", id, err)
	}
	return id, nil
}

// busIDFileName is the file within the data dir holding the persisted bus id.
const busIDFileName = "bus-id"

// LoadOrCreateBusID returns this bus's id, minting and persisting it on first
// start and returning the identical id on every subsequent start.
//
// override is the TEST-ONLY -bus-id flag: empty means "server decides". A
// non-empty override is validated like any other untrusted input. If a bus id
// is already persisted in dir, override may only confirm it — a mismatch is a
// fatal error, because silently switching ids (or overwriting the persisted
// one) would orphan every "<bus-id>.<agent-id>" already issued against this
// data dir (invariant 2).
func LoadOrCreateBusID(dir string, override string) (string, error) {
	if override != "" {
		if err := ValidateBusID(override); err != nil {
			return "", fmt.Errorf("invalid -bus-id override: %w", err)
		}
	}

	path := filepath.Join(dir, busIDFileName)
	existing, err := readBusIDFile(path)
	switch {
	case err == nil:
		// A persisted id must be valid. It is NEVER regenerated on failure:
		// that would silently orphan every "<bus-id>.<agent-id>" already
		// issued, so a corrupt or empty persisted id is a fatal error that
		// requires operator intervention.
		if verr := ValidateBusID(existing); verr != nil {
			return "", fmt.Errorf("persisted bus id in %s is corrupt (%v); this looks like a tampered or damaged data dir and will NOT be regenerated, because that would orphan every existing \"<bus-id>.<agent-id>\"; fix or remove %s manually", path, verr, path)
		}
	case os.IsNotExist(err):
		existing = ""
	default:
		return "", fmt.Errorf("reading persisted bus id from %s: %w", path, err)
	}

	if existing != "" {
		if override != "" && override != existing {
			return "", fmt.Errorf("data dir %s belongs to a different bus: persisted bus id is %q, -bus-id override is %q; refusing to overwrite the persisted id", dir, existing, override)
		}
		return existing, nil
	}

	id := override
	if id == "" {
		id, err = GenerateBusID()
		if err != nil {
			return "", err
		}
	}

	if err := writeBusIDFile(dir, path, id); err != nil {
		return "", err
	}
	return id, nil
}

// readBusIDFile reads and trims the persisted bus id. The returned error
// satisfies os.IsNotExist when the file has never been written.
func readBusIDFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeBusIDFile atomically persists id to path, mode 0600: a temp file in
// the SAME directory is written, fsynced and closed, then renamed into place,
// then the directory itself is fsynced so the rename is durable. The temp
// file is removed on any error path.
func writeBusIDFile(dir, path, id string) (err error) {
	tmp, err := os.CreateTemp(dir, ".bus-id-*")
	if err != nil {
		return fmt.Errorf("creating temp bus id file in %s: %w", dir, err)
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
	if _, err = tmp.WriteString(id + "\n"); err != nil {
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

// BusIdentity satisfies httpapi.Identity with a server-minted bus id.
type BusIdentity string

// BusID implements httpapi.Identity.
func (b BusIdentity) BusID() string { return string(b) }
