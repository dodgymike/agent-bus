package wal

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// CheckpointParticipant is an application projection of the shared WAL.
// Calls are serialized by Log.mu; implementations must not call back into Log.
type CheckpointParticipant interface {
	Applier
	Name() string
	Kinds() []string
	Snapshot(highWater uint64) ([]byte, error)
	Restore(snapshot []byte, highWater uint64) error
}

// MultiApplier deterministically assigns each entry kind to exactly one
// checkpoint participant. An unowned kind is ignored for forward compatibility.
type MultiApplier struct {
	participants []CheckpointParticipant
	byKind       map[string]CheckpointParticipant
}

func NewMultiApplier(participants ...CheckpointParticipant) (*MultiApplier, error) {
	m := &MultiApplier{byKind: make(map[string]CheckpointParticipant)}
	names := make(map[string]bool)
	for _, p := range participants {
		if p == nil || !validCheckpointName(p.Name()) || names[p.Name()] {
			return nil, fmt.Errorf("wal: invalid or duplicate checkpoint participant name %q", participantName(p))
		}
		names[p.Name()] = true
		kinds := append([]string(nil), p.Kinds()...)
		if len(kinds) == 0 {
			return nil, fmt.Errorf("wal: checkpoint participant %q owns no kinds", p.Name())
		}
		for _, kind := range kinds {
			if strings.TrimSpace(kind) == "" || kind != strings.TrimSpace(kind) {
				return nil, fmt.Errorf("wal: checkpoint participant %q has invalid kind %q", p.Name(), kind)
			}
			if prior := m.byKind[kind]; prior != nil {
				return nil, fmt.Errorf("wal: checkpoint kind %q is owned by both %q and %q", kind, prior.Name(), p.Name())
			}
			m.byKind[kind] = p
		}
		m.participants = append(m.participants, p)
	}
	sort.Slice(m.participants, func(i, j int) bool { return m.participants[i].Name() < m.participants[j].Name() })
	return m, nil
}

func participantName(p CheckpointParticipant) string {
	if p == nil {
		return ""
	}
	return p.Name()
}

func validCheckpointName(s string) bool {
	if s == "" || s == "." || s == ".." || filepath.Base(s) != s {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (m *MultiApplier) Apply(c Committed) error {
	if m == nil {
		return nil
	}
	if p := m.byKind[c.Entry.Kind]; p != nil {
		return p.Apply(c)
	}
	return fmt.Errorf("wal: committed entry kind %q has no registered checkpoint participant", c.Entry.Kind)
}

func (m *MultiApplier) owns(kind string) bool { return m != nil && m.byKind[kind] != nil }

const (
	checkpointDirName        = "wal-generations"
	checkpointCurrent        = "CURRENT"
	checkpointManifestFile   = "manifest.json"
	checkpointTailFile       = "tail.wal"
	maxCheckpointSnapshot    = 64 << 20
	checkpointManifestDomain = "agent-bus/wal-checkpoint-manifest/v1"
	checkpointSnapshotDomain = "agent-bus/wal-checkpoint-snapshot/v1"
)

// checkpointFormatVersion is assigned from the Spec Server's
// ondisk-format-version namespace. It is deliberately distinct from WAL v2.
const checkpointFormatVersion uint32 = 7

type checkpointPart struct {
	Name   string   `json:"name"`
	Kinds  []string `json:"kinds"`
	File   string   `json:"file"`
	Length int64    `json:"length"`
	SHA256 string   `json:"sha256"`
}
type checkpointManifest struct {
	Domain        string           `json:"domain"`
	Version       uint32           `json:"version"`
	Generation    uint64           `json:"generation"`
	HighWater     uint64           `json:"high_water"`
	NextIndex     uint64           `json:"next_index"`
	Tail          string           `json:"tail"`
	TailID        string           `json:"tail_id"`
	TailHeaderMAC string           `json:"tail_header_mac"`
	Participants  []checkpointPart `json:"participants"`
	MAC           string           `json:"mac,omitempty"`
}
type snapshotEnvelope struct {
	Domain     string   `json:"domain"`
	Version    uint32   `json:"version"`
	Generation uint64   `json:"generation"`
	Name       string   `json:"name"`
	Kinds      []string `json:"kinds"`
	HighWater  uint64   `json:"high_water"`
	Data       []byte   `json:"data"`
	MAC        string   `json:"mac,omitempty"`
}

func authenticatedJSON(v interface {
	clearMAC()
	setMAC(string)
}, key []byte) ([]byte, error) {
	v.clearMAC()
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(plain)
	v.setMAC(hex.EncodeToString(m.Sum(nil)))
	return json.Marshal(v)
}
func (m *checkpointManifest) clearMAC()       { m.MAC = "" }
func (m *checkpointManifest) setMAC(s string) { m.MAC = s }
func (s *snapshotEnvelope) clearMAC()         { s.MAC = "" }
func (s *snapshotEnvelope) setMAC(v string)   { s.MAC = v }

func verifyAuthenticatedJSON(raw []byte, v interface {
	clearMAC()
	setMAC(string)
}, key []byte) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return err
	}
	var got string
	switch x := v.(type) {
	case *checkpointManifest:
		got = x.MAC
	case *snapshotEnvelope:
		got = x.MAC
	}
	tag, err := hex.DecodeString(got)
	if err != nil || len(tag) != sha256.Size {
		return errors.New("invalid MAC encoding")
	}
	v.clearMAC()
	plain, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(plain)
	if !hmac.Equal(tag, m.Sum(nil)) {
		return errors.New("HMAC verification failed")
	}
	v.setMAC(got)
	return nil
}

func sortedKinds(p CheckpointParticipant) []string {
	k := append([]string(nil), p.Kinds()...)
	sort.Strings(k)
	return k
}

// Checkpoint publishes an immutable generation at one shared committed index.
func (l *Log) Checkpoint() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.diverged != nil {
		return l.diverged
	}
	if l.checkpoints == nil {
		return errors.New("wal: checkpoint requires a MultiApplier")
	}
	h := l.lastCommit
	if l.generation == ^uint64(0) {
		return errors.New("wal: checkpoint generation space exhausted")
	}
	gen := l.generation + 1
	root := filepath.Join(l.dir, checkpointDirName)
	if err := os.MkdirAll(root, dirMode); err != nil {
		return err
	}
	tmp := filepath.Join(root, fmt.Sprintf("gen-%020d.tmp", gen))
	final := strings.TrimSuffix(tmp, ".tmp")
	if err := os.Mkdir(tmp, dirMode); err != nil {
		return fmt.Errorf("wal: create checkpoint generation: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	tailID := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rand.Reader, tailID); err != nil {
		return fmt.Errorf("wal: create checkpoint tail identity: %w", err)
	}
	manifest := checkpointManifest{Domain: checkpointManifestDomain, Version: checkpointFormatVersion, Generation: gen, HighWater: h, NextIndex: l.w.NextIndex(), Tail: checkpointTailFile, TailID: hex.EncodeToString(tailID)}
	for _, p := range l.checkpoints.participants {
		data, err := p.Snapshot(h)
		if err != nil {
			return fmt.Errorf("wal: snapshot %q: %w", p.Name(), err)
		}
		if len(data) > maxCheckpointSnapshot {
			return fmt.Errorf("wal: snapshot %q exceeds %d bytes", p.Name(), maxCheckpointSnapshot)
		}
		env := &snapshotEnvelope{Domain: checkpointSnapshotDomain, Version: checkpointFormatVersion, Generation: gen, Name: p.Name(), Kinds: sortedKinds(p), HighWater: h, Data: data}
		raw, err := authenticatedJSON(env, l.codec.key)
		if err != nil {
			return err
		}
		name := p.Name() + ".snapshot"
		if err = checkpointFault("snapshot-write-before"); err != nil {
			return err
		}
		if err = writeSyncExclusive(filepath.Join(tmp, name), raw); err != nil {
			return err
		}
		if err = checkpointFault("snapshot-fsync"); err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		manifest.Participants = append(manifest.Participants, checkpointPart{Name: p.Name(), Kinds: sortedKinds(p), File: name, Length: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])})
	}
	tailCodec := l.codec
	tailCodec.context = append([]byte(nil), tailID...)
	if err := checkpointFault("tail-write-before"); err != nil {
		return err
	}
	tail, err := openWriter(filepath.Join(tmp, checkpointTailFile), KindWAL, tailCodec)
	if err != nil {
		return err
	}
	tail.next = manifest.NextIndex
	if err = tail.f.Sync(); err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("tail-fsync"); err != nil {
		_ = tail.Close()
		return err
	}
	manifest.TailHeaderMAC, err = checkpointTailHeaderMAC(tail.f, gen, h, l.codec.key)
	if err != nil {
		_ = tail.Close()
		return err
	}
	raw, err := authenticatedJSON(&manifest, l.codec.key)
	if err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("manifest-write-before"); err != nil {
		_ = tail.Close()
		return err
	}
	if err = writeSyncExclusive(filepath.Join(tmp, checkpointManifestFile), raw); err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("manifest-fsync"); err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("generation-dir-fsync-before"); err != nil {
		_ = tail.Close()
		return err
	}
	if err = syncDir(tmp); err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("generation-dir-fsync"); err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("generation-rename-before"); err != nil {
		_ = tail.Close()
		return err
	}
	if err = os.Rename(tmp, final); err != nil {
		_ = tail.Close()
		return err
	}
	if err = checkpointFault("generation-rename"); err != nil {
		_ = tail.Close()
		return l.poisonCheckpoint(err)
	}
	if err = checkpointSyncDir(root); err != nil {
		_ = tail.Close()
		return l.poisonCheckpoint(fmt.Errorf("wal: checkpoint generation rename durability is ambiguous: %w", err))
	}
	if err = atomicCurrent(root, filepath.Base(final)); err != nil {
		_ = tail.Close()
		return l.poisonCheckpoint(err)
	}
	if err = checkpointFault("writer-handoff-before"); err != nil {
		_ = tail.Close()
		return l.poisonCheckpoint(err)
	}
	if err = checkpointFault("writer-handoff"); err != nil {
		_ = tail.Close()
		return l.poisonCheckpoint(err)
	}
	old := l.w
	tail.floor = old.floor
	l.w = tail
	l.generation = gen
	ok = true
	if err = checkpointFault("old-writer-retirement-before"); err != nil {
		return l.poisonCheckpoint(err)
	}
	if err = checkpointFault("old-writer-retirement"); err != nil {
		return l.poisonCheckpoint(err)
	}
	if err = old.closeForHandoff(); err != nil {
		l.diverged = err
		return err
	}
	return nil
}

var checkpointSyncDir = syncDir
var checkpointFault = func(string) error { return nil }

func (l *Log) poisonCheckpoint(err error) error {
	wrapped := fmt.Errorf("wal: checkpoint publication failed after a rename; log is poisoned until restart: %w", err)
	l.diverged = wrapped
	if l.w != nil {
		l.w.mu.Lock()
		l.w.poisoned = wrapped
		l.w.mu.Unlock()
	}
	return wrapped
}

func checkpointTailHeaderMAC(f *os.File, generation, highWater uint64, key []byte) (string, error) {
	hdr := make([]byte, FileHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return "", fmt.Errorf("wal: read checkpoint tail header: %w", err)
	}
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "%s\x00%d\x00%d\x00", checkpointManifestDomain, generation, highWater)
	_, _ = m.Write(hdr)
	return hex.EncodeToString(m.Sum(nil)), nil
}

func writeSyncExclusive(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	return e
}
func atomicCurrent(root, name string) error {
	p := filepath.Join(root, checkpointCurrent+".tmp")
	_ = os.Remove(p)
	if err := checkpointFault("current-temp-write-before"); err != nil {
		return err
	}
	if err := writeSyncExclusive(p, []byte(name+"\n")); err != nil {
		return err
	}
	if err := checkpointFault("current-temp-fsync"); err != nil {
		return err
	}
	if err := checkpointFault("current-rename-before"); err != nil {
		return err
	}
	if err := os.Rename(p, filepath.Join(root, checkpointCurrent)); err != nil {
		return err
	}
	if err := checkpointFault("current-rename"); err != nil {
		return fmt.Errorf("wal: CURRENT rename durability is ambiguous: %w", err)
	}
	if err := checkpointFault("current-parent-fsync-before"); err != nil {
		return fmt.Errorf("wal: CURRENT rename durability is ambiguous: %w", err)
	}
	if err := checkpointSyncDir(root); err != nil {
		return fmt.Errorf("wal: CURRENT rename durability is ambiguous: %w", err)
	}
	if err := checkpointFault("current-parent-fsync"); err != nil {
		return fmt.Errorf("wal: CURRENT rename durability is ambiguous: %w", err)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	return len(a) == len(b) && bytes.Equal([]byte(strings.Join(a, "\x00")), []byte(strings.Join(b, "\x00")))
}

type checkpointSelection struct {
	path                             string
	generation, highWater, nextIndex uint64
	tailContext                      []byte
}

var errCheckpointGenerationRejected = errors.New("wal: checkpoint generation rejected")

func selectCheckpoint(dir string, registry *MultiApplier, logger *logging.Logger) (checkpointSelection, error) {
	legacy := checkpointSelection{path: filepath.Join(dir, WALFileName)}
	if registry == nil {
		return legacy, nil
	}
	root := filepath.Join(dir, checkpointDirName)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return legacy, nil
	}
	if err != nil {
		return legacy, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return legacy, errors.New("wal: checkpoint root is not a regular directory")
	}
	curRaw, currentErr := readRegularBounded(filepath.Join(root, checkpointCurrent), 128)
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		if currentInfo, statErr := os.Lstat(filepath.Join(root, checkpointCurrent)); statErr == nil && (!currentInfo.Mode().IsRegular() || currentInfo.Mode()&os.ModeSymlink != 0) {
			return legacy, currentErr
		}
	}
	var currentGen uint64
	currentValid := currentErr == nil
	if currentValid {
		current := strings.TrimSpace(string(curRaw))
		currentValid = validGenerationDir(current) && scanGeneration(current, &currentGen) == nil
	}
	key, err := loadMACKey(macKeyPath(dir))
	if err != nil {
		return legacy, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return legacy, err
	}
	var candidates []uint64
	sawPublishedGeneration := false
	for _, entry := range entries {
		var n uint64
		info, infoErr := entry.Info()
		if infoErr != nil {
			return legacy, infoErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return legacy, fmt.Errorf("wal: checkpoint path %q is a symlink", entry.Name())
		}
		if entry.Name() == checkpointCurrent+".tmp" {
			if err := quarantineCheckpointPath(root, entry.Name(), logger, "incomplete CURRENT publication temporary"); err != nil {
				return legacy, err
			}
			continue
		}
		if entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") && scanGeneration(strings.TrimSuffix(entry.Name(), ".tmp"), &n) == nil {
			if err := quarantineCheckpointPath(root, entry.Name(), logger, "incomplete temporary generation"); err != nil {
				return legacy, err
			}
			continue
		}
		generationName := validGenerationDir(entry.Name()) && scanGeneration(entry.Name(), &n) == nil
		temporaryName := strings.HasSuffix(entry.Name(), ".tmp") && scanGeneration(strings.TrimSuffix(entry.Name(), ".tmp"), &n) == nil
		if !entry.IsDir() && (generationName || temporaryName) {
			if err := quarantineCheckpointPath(root, entry.Name(), logger, "non-directory entry occupies a checkpoint generation name"); err != nil {
				return legacy, err
			}
			continue
		}
		if entry.IsDir() && generationName {
			sawPublishedGeneration = true
			candidates = append(candidates, n)
		}
	}
	if !sawPublishedGeneration {
		return legacy, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] > candidates[j] })
	for _, gen := range candidates {
		sel, snapshots, verifyErr := verifyGeneration(root, gen, registry, key)
		if verifyErr != nil {
			if errors.Is(verifyErr, errCheckpointGenerationRejected) || checkpointMaterialInvalid(verifyErr) {
				name := fmt.Sprintf("gen-%020d", gen)
				if quarantineErr := quarantineCheckpointPath(root, name, logger, verifyErr.Error()); quarantineErr != nil {
					return legacy, quarantineErr
				}
			}
			if logger != nil {
				logger.Error("wal rejected checkpoint generation and is trying an older complete generation", "generation", gen, "err", verifyErr)
			}
			continue
		}
		for i, participant := range registry.participants {
			if err := participant.Restore(snapshots[i], sel.highWater); err != nil {
				return legacy, fmt.Errorf("wal: restore checkpoint participant %q: %w", participant.Name(), err)
			}
		}
		if logger != nil && (!currentValid || currentGen != gen) {
			logger.Warn("wal recovered a complete checkpoint generation despite an inconsistent CURRENT pointer",
				"selected_generation", gen, "current_generation", currentGen, "current_valid", currentValid,
				"why", "CURRENT is only a publication hint and cannot roll recovery back or override authenticated generation material")
		}
		return sel, nil
	}
	if !currentValid {
		return legacy, fmt.Errorf("wal: CURRENT is missing or invalid and no complete authenticated checkpoint generation exists: %v", currentErr)
	}
	return legacy, fmt.Errorf("wal: no complete authenticated checkpoint generation at or before %d", currentGen)
}

// checkpointMaterialInvalid distinguishes persistent malformed on-disk bytes
// from a registry/configuration mismatch. Persistent bad material is moved out
// of the candidate namespace so it is diagnosed once rather than retried on
// every restart.
func checkpointMaterialInvalid(err error) bool {
	s := err.Error()
	for _, marker := range []string{"manifest authentication", "manifest identity", "snapshot length/read", "digest mismatch", " authentication:", " envelope mismatch", "not a regular file", "symlink"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func quarantineCheckpointPath(root, name string, logger *logging.Logger, why string) error {
	from := filepath.Join(root, name)
	to := from + ".orphan"
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("wal: cannot quarantine checkpoint %q: destination already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("wal: quarantine checkpoint %q: %w", name, err)
	}
	if err := checkpointSyncDir(root); err != nil {
		return fmt.Errorf("wal: checkpoint quarantine rename durability is ambiguous: %w", err)
	}
	if logger != nil {
		logger.Error("wal quarantined orphan checkpoint generation", "path", from, "quarantine", to, "why", why)
	}
	return nil
}

func validGenerationDir(s string) bool {
	var n uint64
	return filepath.Base(s) == s && !strings.HasSuffix(s, ".tmp") && scanGeneration(s, &n) == nil && n > 0
}

func scanGeneration(s string, n *uint64) error {
	if len(s) != len("gen-")+20 || !strings.HasPrefix(s, "gen-") {
		return errors.New("invalid generation name")
	}
	value, err := strconv.ParseUint(strings.TrimPrefix(s, "gen-"), 10, 64)
	if err == nil {
		*n = value
	}
	return err
}

func verifyGeneration(root string, gen uint64, registry *MultiApplier, key []byte) (checkpointSelection, [][]byte, error) {
	dir := filepath.Join(root, fmt.Sprintf("gen-%020d", gen))
	raw, err := readBounded(filepath.Join(dir, checkpointManifestFile), 1<<20)
	if err != nil {
		return checkpointSelection{}, nil, err
	}
	var manifest checkpointManifest
	if err = verifyAuthenticatedJSON(raw, &manifest, key); err != nil {
		return checkpointSelection{}, nil, fmt.Errorf("manifest authentication: %w", err)
	}
	tailID, tailIDErr := hex.DecodeString(manifest.TailID)
	if manifest.Domain != checkpointManifestDomain || manifest.Version != checkpointFormatVersion || manifest.Generation != gen || manifest.Tail != checkpointTailFile || manifest.NextIndex <= manifest.HighWater || tailIDErr != nil || len(tailID) != sha256.Size {
		return checkpointSelection{}, nil, errors.New("manifest identity, version, tail, or high-water is invalid")
	}
	tailPath := filepath.Join(dir, checkpointTailFile)
	tail, err := openRegularRead(tailPath)
	if err != nil {
		return checkpointSelection{}, nil, err
	}
	wantTailMAC, tailErr := checkpointTailHeaderMAC(tail, gen, manifest.HighWater, key)
	closeErr := tail.Close()
	if tailErr != nil {
		return checkpointSelection{}, nil, tailErr
	}
	if closeErr != nil {
		return checkpointSelection{}, nil, closeErr
	}
	if !hmac.Equal([]byte(wantTailMAC), []byte(manifest.TailHeaderMAC)) {
		return checkpointSelection{}, nil, fmt.Errorf("%w: checkpoint tail header/generation binding failed", errCheckpointGenerationRejected)
	}
	// Validate the complete tail against the authenticated snapshot boundary
	// before restoring any participant. replay deliberately turns an Applier
	// rejection into a record-level discard for ordinary WAL recovery; that
	// policy is not appropriate here. A commit at or below H proves this tail
	// does not belong to this snapshot generation, so the WHOLE generation must
	// be rejected and an older wholly authenticated generation selected.
	var boundaryErr error
	tailCodec := codec{version: FormatVersion, key: key, context: tailID}
	// Candidate validation is deliberately read-only. In particular it must not
	// run repairLog: doing so would discard bytes before this generation has
	// been selected, and the later normal Open repair could no longer report the
	// loss in Recovered.Repaired or the operator log. The tolerant salvage walk
	// validates every surviving authenticated frame without changing the file;
	// the selected tail is repaired exactly once by Open below selectCheckpoint.
	tailReplay, replayErr := validateCheckpointTail(tailPath, tailCodec, manifest.HighWater, func(err error) {
		boundaryErr = err
	})
	if replayErr != nil {
		return checkpointSelection{}, nil, fmt.Errorf("%w: checkpoint tail validation: %v", errCheckpointGenerationRejected, replayErr)
	}
	if boundaryErr != nil {
		return checkpointSelection{}, nil, fmt.Errorf("%w: checkpoint tail validation: %v", errCheckpointGenerationRejected, boundaryErr)
	}
	_ = tailReplay // replay's successful full scan is the structural tail proof.
	if len(manifest.Participants) != len(registry.participants) {
		return checkpointSelection{}, nil, errors.New("manifest participant set does not exactly match configured registry")
	}
	snapshots := make([][]byte, len(registry.participants))
	for i, participant := range registry.participants {
		meta, kinds := manifest.Participants[i], sortedKinds(participant)
		if meta.File != participant.Name()+".snapshot" {
			return checkpointSelection{}, nil, fmt.Errorf("%w: participant %q snapshot path mismatch", errCheckpointGenerationRejected, participant.Name())
		}
		if meta.Name != participant.Name() || !equalStrings(meta.Kinds, kinds) || meta.Length < 0 || meta.Length > maxCheckpointSnapshot+(1<<20) {
			return checkpointSelection{}, nil, fmt.Errorf("participant %q metadata mismatch", participant.Name())
		}
		snapshotRaw, err := readBounded(filepath.Join(dir, meta.File), maxCheckpointSnapshot+(1<<20))
		if err != nil || int64(len(snapshotRaw)) != meta.Length {
			return checkpointSelection{}, nil, fmt.Errorf("participant %q snapshot length/read: %w", participant.Name(), err)
		}
		sum := sha256.Sum256(snapshotRaw)
		if hex.EncodeToString(sum[:]) != meta.SHA256 {
			return checkpointSelection{}, nil, fmt.Errorf("participant %q digest mismatch", participant.Name())
		}
		var envelope snapshotEnvelope
		if err = verifyAuthenticatedJSON(snapshotRaw, &envelope, key); err != nil {
			return checkpointSelection{}, nil, fmt.Errorf("participant %q authentication: %w", participant.Name(), err)
		}
		if envelope.Domain != checkpointSnapshotDomain || envelope.Version != checkpointFormatVersion || envelope.Generation != gen || envelope.Name != participant.Name() || envelope.HighWater != manifest.HighWater || !equalStrings(envelope.Kinds, kinds) || len(envelope.Data) > maxCheckpointSnapshot {
			return checkpointSelection{}, nil, fmt.Errorf("participant %q envelope mismatch", participant.Name())
		}
		snapshots[i] = envelope.Data
	}
	if _, err = regularFileInfo(filepath.Join(dir, checkpointTailFile)); err != nil {
		return checkpointSelection{}, nil, err
	}
	return checkpointSelection{path: filepath.Join(dir, checkpointTailFile), generation: gen, highWater: manifest.HighWater, nextIndex: manifest.NextIndex, tailContext: append([]byte(nil), tailID...)}, snapshots, nil
}

// validateCheckpointTail performs the generation-boundary check over the
// records that recovery can salvage, but never rewrites or truncates path.
// Framing damage is therefore compatible with selecting a generation while
// remaining observable when Open performs its one normal logged repair pass.
func validateCheckpointTail(path string, c codec, highWater uint64, reject func(error)) (repairPlan, error) {
	open := make(map[uint64]struct{})
	plan, err := salvage(path, KindWAL, c, func(rec Record) error {
		switch rec.Type {
		case TypePrepare:
			if _, _, err := DecodePrepare(path, rec); err == nil {
				open[rec.Index] = struct{}{}
			}
		case TypeCommit:
			prepare, err := DecodeCommit(path, rec)
			if err == nil {
				if _, ok := open[prepare]; ok {
					delete(open, prepare)
					if rec.Index <= highWater {
						reject(fmt.Errorf("commit index %d is not above authenticated high-water %d", rec.Index, highWater))
					}
				}
			}
		case TypeAbort:
			prepare, _, err := DecodeAbort(path, rec)
			if err == nil {
				delete(open, prepare)
			}
		}
		return nil
	})
	return plan, err
}

func readBounded(path string, max int64) ([]byte, error) {
	return readRegularBounded(path, max)
}

func regularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("wal: checkpoint path %s is not a regular file", path)
	}
	return info, nil
}

func readRegularBounded(path string, max int64) ([]byte, error) {
	f, err := openRegularRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() < 0 || stat.Size() > max {
		return nil, fmt.Errorf("wal: checkpoint file %s size %d exceeds bound %d", path, stat.Size(), max)
	}
	b := make([]byte, stat.Size())
	_, err = io.ReadFull(f, b)
	return b, err
}

// openRegularRead rejects special files before opening, uses O_NOFOLLOW to
// close the final-component symlink race, and fstats the opened descriptor so
// a path swap cannot turn a checked regular file into a FIFO/device.
func openRegularRead(path string) (*os.File, error) {
	before, err := regularFileInfo(path)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	after, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, fmt.Errorf("wal: checkpoint path %s changed or is not a regular file", path)
	}
	return f, nil
}
