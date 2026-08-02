package wal

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// This file owns the HMAC-SHA256 key that authenticates every version 2 record.
//
// WHERE IT LIVES (user decision, DECISIONS.md 2026-08-02, "The HMAC key lives in
// the DATA DIR; a missing or wrong key is FATAL"): one file, mode 0600, in the
// data directory, shared by the WAL and the audit log because they are the same
// machinery over the same bytes and a second key would be a second thing to
// lose. The key is therefore a function of the LOG FILE'S DIRECTORY, never of
// its name.
//
// WHAT THIS BUYS, stated exactly. It COMPLETELY defeats the attack that
// motivated format version 2: an ordinary enrolled client crafting a payload
// whose CRC makes damage look like a complete record, because a client cannot
// compute a tag over a key it does not hold. It buys NOTHING against an attacker
// who already has data-directory WRITE access -- such an attacker can read the
// key sitting next to the log and forge freely. That is an accepted limit, not
// an oversight.

// MACKeyFileName is the name of the MAC key file inside the data directory.
const MACKeyFileName = "wal-mac.key"

// macKeySize is the key length in bytes. 32 bytes matches HMAC-SHA256's block
// output and is the size crypto/rand is asked for; it is not a tuning knob.
const macKeySize = 32

// macKeyMode is the permission bits of the key file. 0600 for the same reason
// the log is 0600, only more so: anything that can read this file can forge
// every record in the log.
const macKeyMode os.FileMode = 0600

// Sentinel errors for the key file. All are checkable with errors.Is, and every
// concrete error naming one also names the KEY PATH -- the first question asked
// of a bus that will not start over its key is always "which file".
var (
	// ErrMACKeyMissing reports that the key file does not exist and could not
	// safely be generated, because the log it protects positively identifies
	// itself as format version 2 and is longer than its own file header.
	ErrMACKeyMissing = errors.New("wal: the MAC key file is missing")

	// ErrMACKeyMalformed reports a key file that exists but cannot be used: the
	// wrong length, not hexadecimal, or unreadable. It is ALWAYS FATAL and the
	// key is NEVER silently regenerated -- a fresh key would make every record
	// in an intact log fail verification.
	ErrMACKeyMalformed = errors.New("wal: the MAC key file is malformed")

	// ErrMACKeyMismatch reports a log whose file header does not verify under
	// the key AND in which not one record verifies under it either. That is what
	// a WRONG KEY looks like, and it is the one shape of unreadable log recovery
	// refuses to quarantine. See repairLog.
	ErrMACKeyMismatch = errors.New("wal: the MAC key does not verify this log")
)

// macKeyErr is the concrete error for every key problem. errors.Is matches the
// sentinel; Unwrap still reaches the underlying I/O cause when there was one.
type macKeyErr struct {
	sentinel error
	msg      string
	cause    error
}

func (e *macKeyErr) Error() string {
	if e.cause != nil {
		return e.msg + ": " + e.cause.Error()
	}
	return e.msg
}

func (e *macKeyErr) Is(target error) bool { return target == e.sentinel }

func (e *macKeyErr) Unwrap() error { return e.cause }

// macKeyPath returns the key file that protects the logs in dir.
func macKeyPath(dir string) string { return filepath.Join(dir, MACKeyFileName) }

// loadMACKey reads and decodes the key at keyPath.
//
// The file is 64 lowercase hexadecimal characters with an optional trailing
// newline, so it can be read, copied and backed up by a human without a
// base64/binary round trip going wrong in a terminal.
//
// EVERY failure other than "it is not there" is FATAL and names the path.
// Nothing here ever falls back to generating a key: a key file that exists but
// cannot be parsed is a broken deployment, and replacing it would convert a
// fixable misconfiguration into the total loss of a log that is probably intact.
func loadMACKey(keyPath string) ([]byte, error) {
	b, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &macKeyErr{sentinel: ErrMACKeyMissing,
				msg:   fmt.Sprintf("wal: the MAC key file %s does not exist", keyPath),
				cause: err}
		}
		// Deliberately NOT treated as "absent": a permission error or an I/O
		// error means the key may well be sitting right there, and generating a
		// new one over the top of it would destroy the only thing that can read
		// the log.
		return nil, &macKeyErr{sentinel: ErrMACKeyMalformed,
			msg:   fmt.Sprintf("wal: the MAC key file %s exists but cannot be read", keyPath),
			cause: err}
	}
	text := strings.TrimRight(string(b), "\r\n")
	if want := hex.EncodedLen(macKeySize); len(text) != want {
		return nil, &macKeyErr{sentinel: ErrMACKeyMalformed,
			msg: fmt.Sprintf("wal: the MAC key file %s holds %d characters, want exactly %d hexadecimal characters (a %d-byte key)",
				keyPath, len(text), want, macKeySize)}
	}
	key := make([]byte, macKeySize)
	if _, err := hex.Decode(key, []byte(text)); err != nil {
		// The cause is hex.InvalidByteError, which quotes the offending
		// character; the key material itself is never rendered.
		return nil, &macKeyErr{sentinel: ErrMACKeyMalformed,
			msg:   fmt.Sprintf("wal: the MAC key file %s is not hexadecimal", keyPath),
			cause: err}
	}
	return key, nil
}

// createMACKey generates a fresh key and writes it durably.
//
// The bytes come from crypto/rand and nowhere else. O_EXCL makes "did I create
// this file?" an atomic answer from the kernel rather than a guess with a race
// in it, and both the file and its parent directory are fsynced for the same
// reason initFile syncs both: a key that is not durably NAMED is a key the next
// crash loses, and losing it costs the whole log.
func createMACKey(keyPath string) ([]byte, error) {
	key := make([]byte, macKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("wal: generate a MAC key for %s: %w", keyPath, err)
	}
	text := make([]byte, hex.EncodedLen(macKeySize)+1)
	hex.Encode(text, key)
	text[len(text)-1] = '\n'

	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, macKeyMode)
	if err != nil {
		return nil, err // classified by the caller: os.ErrExist is a race, not a failure
	}
	if n, err := f.Write(text); err != nil || n != len(text) {
		f.Close()
		os.Remove(keyPath)
		return nil, fmt.Errorf("wal: write the MAC key %s: %w", keyPath, shortWrite(err, n, len(text)))
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(keyPath)
		return nil, fmt.Errorf("wal: fsync the MAC key %s: %w", keyPath, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(keyPath)
		return nil, fmt.Errorf("wal: close the MAC key %s: %w", keyPath, err)
	}
	if err := syncDir(filepath.Dir(keyPath)); err != nil {
		return nil, fmt.Errorf("wal: fsync the directory of the MAC key %s: %w", keyPath, err)
	}
	return key, nil
}

// macKeyFor resolves the key that protects logPath, generating one only when
// that provably cannot destroy anything.
//
// THE AUTO-CREATION RULE:
//
//	Creation is REFUSED, and an absent key is FATAL, exactly when the log
//	POSITIVELY IDENTIFIES ITSELF as format version 2 -- our magic, with a
//	version field that reads FormatVersion -- AND is longer than a version 2
//	file header. In every other state a key is created.
//
// That refusing predicate is deliberately the SAME condition repairLog uses to
// raise ErrMACKeyMismatch: one condition, two errors, depending on whether the
// key file is ABSENT or merely WRONG. Keeping them identical is not tidiness,
// it is what makes the pair complete -- a wrong key damages neither the magic
// nor the version field, so this predicate cannot miss the case the decision is
// about.
//
// WHY THE RULE IS THIS NARROW, rather than "refuse for any non-empty, non-v1
// log". The fatal is owed to a single argument: under a fresh key every record
// fails verification, so a discard-the-unverifiable pass would destroy the whole
// log over a misconfiguration. Trace what ACTUALLY happens under a fresh key on
// a log we CANNOT identify -- garbage magic, a truncated stub, a file that is
// not one of ours at all. The file header tag fails (HeaderDamaged), no record
// verifies (!Salvageable), and that lands in the QUARANTINE branch: the file is
// RENAMED ASIDE, never truncated and never deleted, and the bus starts. Nothing
// is destroyed. The operator still has every byte, and restoring the key and
// renaming the file back recovers the log completely. The genuinely destructive
// paths -- tail truncation and rewrite-and-discard -- are unreachable from that
// state, because both require a file header that VERIFIES. So the argument for
// the fatal simply does not reach those states, and refusing to boot over them
// bought nothing.
//
// The Size narrowing matches repairLog's for the same reason it does there: a
// file no longer than its own header holds no record, so there is nothing a
// missing key could be hiding and nothing to lose.
//
// THE HONEST RESIDUAL: a REAL version 2 log whose MAGIC is also damaged, sitting
// in a directory whose key file is missing, is no longer refused -- it gets a
// fresh key, fails to identify, and is QUARANTINED. That is a residual, not a
// hole, because quarantine destroys nothing: the log is renamed aside with every
// byte intact, and the startup summary names where it went. Restore the key,
// rename it back, restart.
//
// Where the rule DOES refuse, it fails loudly and names BOTH paths
// (DECISIONS.md 2026-08-02).
func macKeyFor(logPath string, kind Kind, logger *logging.Logger) ([]byte, error) {
	keyPath := macKeyPath(filepath.Dir(logPath))

	key, err := loadMACKey(keyPath)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, ErrMACKeyMissing) {
		return nil, err // malformed or unreadable: always fatal, never regenerated
	}

	mayCreate, err := macKeyMayBeCreated(logPath, kind)
	if err != nil {
		return nil, err
	}
	if !mayCreate {
		return nil, &macKeyErr{sentinel: ErrMACKeyMissing,
			msg: fmt.Sprintf("wal: %s: the MAC key file %s does not exist, and this log identifies itself as on-disk format version %d and is longer than its own file header, so a key cannot be generated for it: under a fresh key EVERY record would fail verification, and recovery will not discard a log that is probably intact over a misconfiguration (a missing or wrong key is a deliberate exception to the always-restart policy). Restore %s, or -- if the key is genuinely lost -- move %s aside by hand and restart.",
				logPath, keyPath, FormatVersion, keyPath, logPath)}
	}

	key, err = createMACKey(keyPath)
	if errors.Is(err, os.ErrExist) {
		// Another process created it between the read and the create. Its key is
		// as good as ours would have been, and it won the race.
		return loadMACKey(keyPath)
	}
	if err != nil {
		return nil, fmt.Errorf("wal: create the MAC key %s: %w", keyPath, err)
	}
	logger.Info("wal generated a new MAC key", "path", keyPath, "mode", macKeyMode.String())
	return key, nil
}

// macKeyMayBeCreated reports whether generating a key for logPath is safe.
//
// It is safe everywhere EXCEPT on a log that positively identifies itself as
// format version 2 and is longer than a version 2 file header -- the exact
// condition repairLog tests before raising ErrMACKeyMismatch. See macKeyFor for
// why the two predicates are deliberately identical, and why every other state
// (absent, zero-length, version 1, too short to hold a header, garbage magic)
// can only ever reach the non-destructive quarantine path.
func macKeyMayBeCreated(logPath string, kind Kind) (bool, error) {
	size, err := logSize(logPath)
	if err != nil {
		return false, err
	}
	if size == 0 {
		// Absent or zero-length: provably no record, and detectFormat would only
		// say "unknown" anyway.
		return true, nil
	}
	version, err := detectFormat(logPath, kind)
	if err != nil {
		return false, err
	}
	// detectFormat returns FormatVersion only for our magic carrying a version
	// field of exactly 2, which is the positive identification this turns on.
	current := codec{version: FormatVersion}
	return !(version == FormatVersion && size > current.fileHeaderSize()), nil
}

// logIsEmpty reports whether path is absent or zero-length -- the two states
// that provably hold no record.
func logIsEmpty(path string) (bool, error) {
	size, err := logSize(path)
	if err != nil {
		return false, err
	}
	return size == 0, nil
}

// logSize returns the size of path in bytes, or 0 when it does not exist -- the
// two states that provably hold no record collapse to the same answer. A stat
// that fails for any OTHER reason is an error rather than "empty": "I could not
// look at the log" must never be read as "the log is not there".
func logSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("wal: stat %s: %w", path, err)
	}
	return fi.Size(), nil
}

// detectFormat reports which on-disk format version path holds, reading only
// the first 12 bytes -- the magic and the version, the two fields that mean the
// same thing in every version and that need no key to read.
//
// It returns version 0, meaning "unknown or new, use the current format", for a
// file that does not exist, is zero-length, is shorter than 12 bytes, or whose
// magic is not one this package writes. GARBAGE MAGIC IS DELIBERATELY NOT A
// VERSION QUESTION: a wrong key does not damage the magic, so a damaged magic
// over intact records is the existing header-repair path and must not be
// mistaken for a layout this binary cannot read.
//
// It is fatal, unchanged from the behaviour it replaces, when the magic names
// the OTHER kind of log, and when our magic carries a version this binary does
// not implement.
func detectFormat(path string, kind Kind) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("wal: open %s: %w", path, err)
	}
	defer f.Close()

	var hdr [12]byte
	n, err := io.ReadFull(f, hdr[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("wal: read the file header of %s at offset 0: %w", path, err)
	}
	if n < len(hdr) {
		return 0, nil
	}
	magic := string(hdr[0:8])
	if got := kindForMagic(magic); got != 0 && got != kind {
		return 0, corruptf(path, 0, "file is a %s file, want a %s file; recovery will not reinterpret one log as the other", got, kind)
	}
	if magic != kind.magic() {
		return 0, nil
	}
	switch v := binary.BigEndian.Uint32(hdr[8:12]); v {
	case formatVersionV1, FormatVersion:
		return v, nil
	default:
		return 0, corruptf(path, 0, "format version %d, want %d; this binary does not implement that layout and recovery will not guess at it", v, FormatVersion)
	}
}

// resolveCodec works out how to read path: which format version it is in, and
// -- for version 2 -- which key authenticates it.
//
// Every exported entry point calls this once and threads the result down, so a
// process never has to guess which format it is looking at and never loads the
// key twice for one operation.
func resolveCodec(path string, kind Kind, logger *logging.Logger) (codec, error) {
	version, err := detectFormat(path, kind)
	if err != nil {
		return codec{}, err
	}
	return codecFor(path, kind, version, logger)
}

// codecFor builds the codec for a log whose version has already been detected.
//
// A version 1 codec carries NO KEY and never asks for one: version 1 frames are
// authenticated by an unkeyed CRC32C, so requiring a key to read one would make
// a legacy log unreadable for no reason -- and would mean a read-only scan
// created a file as a side effect.
func codecFor(path string, kind Kind, version uint32, logger *logging.Logger) (codec, error) {
	if version == formatVersionV1 {
		return codec{version: formatVersionV1}, nil
	}
	key, err := macKeyFor(path, kind, logger)
	if err != nil {
		return codec{}, err
	}
	return codec{version: FormatVersion, key: key}, nil
}

// currentCodec is the codec this binary WRITES: the current format version, with
// the key loaded or -- where macKeyFor permits it -- created. It is what a new
// log is created with and what upgradeV1 converts INTO.
func currentCodec(path string, kind Kind, logger *logging.Logger) (codec, error) {
	return codecFor(path, kind, FormatVersion, logger)
}
