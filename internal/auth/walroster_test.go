package auth_test

// AUTH-3, part 2: the roster SURVIVES A RESTART.
//
// This is the headline claim of the task and the reason the record exists at
// all. Everything here drives the REAL write path — wal.Open, prepare fsync,
// commit fsync, Apply — against a fresh t.TempDir(), never the tracked data/
// directory and never a shared temp path (two agents run in parallel in this
// repo).

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/auth"
	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// walPath is the log file inside a data directory.
func walPath(dir string) string { return filepath.Join(dir, wal.WALFileName) }

// openRoster performs the three-step wiring WALRoster documents and the order
// of which is not optional: applier first, wal.Open (which REPLAYS into it),
// then Attach. Used for the first boot and for every restart, because a restart
// is the same three steps.
func openRoster(t *testing.T, dir string) (*auth.WALRoster, *wal.Log) {
	t.Helper()
	return openRosterWithLogger(t, dir, nil)
}

// openRosterWithLogger is openRoster with a logger attached to the ROSTER only.
// wal.LogOptions.Logger is deliberately left nil so the buffer a caller passes
// here holds this package's records and nothing else — invariant 6's "every
// discard is LOGGED" is a claim about what THIS package says, and a buffer full
// of wal's own recovery chatter would let an assertion pass on the wrong line.
func openRosterWithLogger(t *testing.T, dir string, logger *logging.Logger) (*auth.WALRoster, *wal.Log) {
	t.Helper()
	r := auth.NewWALRoster(logger)
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if err := r.Attach(l); err != nil {
		l.Close()
		t.Fatalf("Attach: %v", err)
	}
	return r, l
}

// openPlainLog opens a log with NO applier, for tests that need to put exact
// bytes on disk without a roster interpreting them on the way past.
func openPlainLog(t *testing.T, dir string) *wal.Log {
	t.Helper()
	l, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	return l
}

// agentRecordCount counts the agent-kind PREPARE records in a log. It is the
// measure behind "nothing was written" and "a duplicate never burns an fsync":
// a prepare record is a record that reached the platter, whether or not it went
// on to commit.
func agentRecordCount(t *testing.T, path string) int {
	t.Helper()
	n := 0
	for range agentRecordBodies(t, path) {
		n++
	}
	return n
}

// agentRecordBodies returns the raw body of every agent-kind PREPARE record in
// the log, in file order.
func agentRecordBodies(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	recs, _, err := wal.ScanAll(path, wal.KindWAL)
	if err != nil {
		t.Fatalf("wal.ScanAll(%s): %v", path, err)
	}
	var out []json.RawMessage
	for _, rec := range recs {
		if rec.Type != wal.TypePrepare {
			continue
		}
		entry, _, err := wal.DecodePrepare(path, rec)
		if err != nil {
			t.Fatalf("wal.DecodePrepare of record %d: %v", rec.Index, err)
		}
		if entry.Kind != auth.RecordKind {
			continue
		}
		out = append(out, entry.Body)
	}
	return out
}

// richEntry is an entry with EVERY reserved field populated. The reserved
// fields have to be proven to make the round trip through the REAL write path,
// not merely through Encode: the whole reason they are in the record from the
// first byte written is that a later migration would be a forced re-enrolment.
func richEntry(t *testing.T, name string, n uint64) auth.RosterEntry {
	t.Helper()
	e := baseEntry(t, name, n)
	e.MessagingPublicKey = fixedKey(0x44)
	e.InviteID = "invite-durable-1"
	e.CertBindings = []auth.CertBinding{
		{Fingerprint: fixedFingerprint(0x0A), BoundAt: recordEpoch, RetiredAt: retiredAt(recordEpoch.Add(time.Hour))},
		{Fingerprint: fixedFingerprint(0x0B), BoundAt: recordEpoch.Add(time.Hour)},
	}
	return e
}

// TestWALRosterSurvivesRestart is the claim: three enrolments recorded through
// the durable path are all present, field for field, after the process that
// wrote them is gone and the log is reopened from scratch.
func TestWALRosterSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	r1, l1 := openRoster(t, dir)
	want := []auth.RosterEntry{
		baseEntry(t, "worker", 1),
		richEntry(t, "relay", 1),
		func() auth.RosterEntry {
			e := baseEntry(t, "worker", 2)
			e.CertBindings = []auth.CertBinding{{Fingerprint: fixedFingerprint(0x7E), BoundAt: recordEpoch}}
			return e
		}(),
	}
	for _, e := range want {
		if err := r1.Put(e); err != nil {
			t.Fatalf("Put(%s): %v", e.AgentID, err)
		}
	}
	if got := r1.Len(); got != len(want) {
		t.Fatalf("before the restart the roster holds %d agents, want %d", got, len(want))
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("closing the first log: %v", err)
	}

	// --- the restart ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()

	if got := r2.Len(); got != len(want) {
		t.Fatalf("after the restart the roster holds %d agents, want %d; enrolments acknowledged before the restart were LOST (invariant 4)", got, len(want))
	}
	for _, e := range want {
		got, ok := r2.Get(e.AgentID)
		if !ok {
			t.Fatalf("agent %q is absent after the restart; it was acknowledged as enrolled before it", e.AgentID)
		}
		if !reflect.DeepEqual(got, normaliseEntry(e)) {
			t.Fatalf("agent %q came back changed by the restart.\n  got  %+v\n  want %+v", e.AgentID, got, normaliseEntry(e))
		}
	}

	// The reserved fields specifically, called out so a regression that drops
	// them reports as itself rather than as a diff of two large structs.
	rich, _ := r2.Get(mustAgentID(t, "relay", 1))
	if len(rich.MessagingPublicKey) == 0 {
		t.Errorf("the messaging public key did not survive the restart")
	}
	if rich.InviteID == "" {
		t.Errorf("the invite id did not survive the restart; without it revocation and audit have nothing to join on")
	}
	if len(rich.CertBindings) != 2 {
		t.Fatalf("the certificate history came back with %d bindings, want 2", len(rich.CertBindings))
	}
	if rich.CertBindings[0].RetiredAt == nil {
		t.Errorf("a RETIRED certificate binding came back LIVE; the bus would go on accepting a certificate it retired")
	}
	if rich.CertBindings[1].RetiredAt != nil {
		t.Errorf("a LIVE certificate binding came back retired at %v", *rich.CertBindings[1].RetiredAt)
	}

	t.Run("mutation isolation", func(t *testing.T) {
		// The stored-credential hazard the copies exist for: a caller that
		// keeps a reference to the slices it handed over — or mutates the ones
		// it was handed back — must not be able to change WHICH KEY
		// authenticates as an agent, or WHICH CERTIFICATE is accepted for it.
		dir := t.TempDir()
		r, l := openRoster(t, dir)
		defer l.Close()

		e := richEntry(t, "worker", 1)
		if err := r.Put(e); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// The snapshot is built from FRESHLY ALLOCATED values rather than from
		// e, so the mutations below cannot reach into it and quietly make the
		// comparison vacuous.
		snapshot := normaliseEntry(richEntry(t, "worker", 1))

		// (1) mutate what we PASSED IN, after the Put.
		e.AuthPublicKey[0] ^= 0xFF
		e.CertBindings[0].Fingerprint[0] ^= 0xFF
		e.CertBindings[1].BoundAt = recordEpoch.Add(999 * time.Hour)

		got, ok := r.Get(e.AgentID)
		if !ok {
			t.Fatalf("agent %q vanished", e.AgentID)
		}
		if !reflect.DeepEqual(got, snapshot) {
			t.Fatalf("mutating the caller's slices AFTER Put changed the STORED credential.\n  got  %+v\n  want %+v", got, snapshot)
		}

		// (2) mutate what we were HANDED BACK.
		got.AuthPublicKey[0] ^= 0xFF
		got.CertBindings[0].Fingerprint[0] ^= 0xFF
		if got.CertBindings[1].RetiredAt == nil {
			retired := recordEpoch
			got.CertBindings[1].RetiredAt = &retired
		}

		again, ok := r.Get(e.AgentID)
		if !ok {
			t.Fatalf("agent %q vanished", e.AgentID)
		}
		if !reflect.DeepEqual(again, snapshot) {
			t.Fatalf("mutating the entry returned by Get changed the STORED credential.\n  got  %+v\n  want %+v", again, snapshot)
		}
	})
}

// TestAUTH3AcceptanceAgentAuthenticatesAndIsListedAfterARestart IS THE TASK'S
// ACCEPTANCE CRITERION, spelled out end to end: "an agent enrolled before a
// restart is still AUTHENTICATED and LISTED after one, with no re-enrolment".
//
// Everything else in this file proves a PART of that — the record round trips,
// the map refills, the reserved fields survive. None of them proved the whole
// claim, because until now the only post-restart Authenticate in the suite was
// a NEGATIVE one (a pre-restart token must stop working). A roster that
// recovered every field except the auth key would have passed the entire suite
// and failed every agent on the bus.
//
// So this drives the REAL objects, in the real order, across a real restart:
//
//  1. a real auth.Service over a real WALRoster over a real wal.Log, and an
//     agent enrolled with a REAL Ed25519 keypair;
//  2. the log closed and BOTH the roster and the service rebuilt from scratch,
//     so nothing whatsoever is carried over in memory;
//  3. the full challenge/response — BeginSession, sign SessionSigningContext +
//     token with the ORIGINAL private key, CompleteSession, Authenticate —
//     with no second Enrol anywhere;
//  4. List() carrying the agent's ORIGINAL EnrolledAt and Epoch, not the
//     instant of recovery (EnrolledAt is the epoch every read path filters
//     with, so a recovered agent stamped "now" silently loses its history);
//  5. a DIFFERENT keypair still refused, which is what makes step 3 evidence
//     that the recovered key is THE ENROLLED ONE rather than merely A key.
func TestAUTH3AcceptanceAgentAuthenticatesAndIsListedAfterARestart(t *testing.T) {
	dir := t.TempDir()

	// --- first boot ---
	r1, l1 := openRoster(t, dir)
	svc1, clock1 := newService(t, auth.Options{Roster: r1})

	// The enrolment happens at an instant the SECOND service's clock has not
	// reached, so "the original EnrolledAt" and "the instant of recovery" are
	// different values and step 4 cannot pass by coincidence.
	clock1.Advance(72 * time.Hour)
	enrolledAt := clock1.Now()

	pub, priv := newKeypair(t)
	res, err := svc1.Enrol(auth.EnrolRequest{Name: "worker", PublicKey: pub, IdempotencyKey: "auth3-acceptance"})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if !res.EnrolledAt.Equal(enrolledAt) {
		t.Fatalf("test bug: the enrolment is stamped %v, want the advanced clock %v", res.EnrolledAt, enrolledAt)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("closing the first log: %v", err)
	}

	// --- the restart: a BRAND NEW roster and a BRAND NEW service ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()
	svc2, clock2 := newService(t, auth.Options{Roster: r2})
	if clock2.Now().Equal(enrolledAt) {
		t.Fatalf("test bug: the recovering service's clock reads %v, the same as the enrolment instant; step 4 would prove nothing", clock2.Now())
	}

	// (3) the full challenge/response, WITHOUT re-enrolling.
	ch, err := svc2.BeginSession(res.AgentID)
	if err != nil {
		t.Fatalf("BeginSession for %q after the restart = %v; the agent was acknowledged as enrolled and never left, so the recovered roster must know it", res.AgentID, err)
	}
	sess, err := svc2.CompleteSession(ch.Token, signToken(priv, ch.Token))
	if err != nil {
		t.Fatalf("CompleteSession after the restart = %v; the agent signed with the SAME private key it enrolled with, so the recovered auth public key must verify it. A failure here means the durable roster survived the restart in name only and every agent on the bus must re-enrol", err)
	}
	if sess.AgentID != res.AgentID {
		t.Fatalf("the session authenticates %q, want %q", sess.AgentID, res.AgentID)
	}
	if sess.State != auth.SessionActive {
		t.Fatalf("the completed session is %v, want active", sess.State)
	}
	princ, err := svc2.Authenticate(ch.Token)
	if err != nil {
		t.Fatalf("Authenticate with the post-restart token = %v, want a principal", err)
	}
	if princ.AgentID != res.AgentID {
		t.Fatalf("Authenticate returned principal %q, want %q", princ.AgentID, res.AgentID)
	}

	// (4) listed, with the ORIGINAL provenance.
	list := r2.List()
	if len(list) != 1 {
		t.Fatalf("List() returned %d agents after the restart, want 1 (%+v); the hub rebuilds its roster from this, and an empty list is a bus that authenticates everyone and serves nobody", len(list), list)
	}
	got := list[0]
	if got.AgentID != res.AgentID {
		t.Fatalf("List() returned agent %q, want %q", got.AgentID, res.AgentID)
	}
	if !got.EnrolledAt.Equal(enrolledAt) {
		t.Fatalf("the listed agent enrolled at %v, want the ORIGINAL %v; EnrolledAt is the epoch every read path filters with (store.Message.VisibleTo), so a recovered agent stamped with the recovery instant silently loses its history at every restart", got.EnrolledAt, enrolledAt)
	}
	if !got.Epoch.Equal(enrolledAt) {
		t.Fatalf("the listed agent's epoch is %v, want the ORIGINAL %v", got.Epoch, enrolledAt)
	}
	if !bytes.Equal(got.AuthPublicKey, pub) {
		t.Fatalf("the listed agent carries a different auth public key than the one it enrolled with")
	}

	// (5) a DIFFERENT keypair is still refused. Without this, step 3 only shows
	// that SOME key verified.
	_, impostor := newKeypair(t)
	ch2, err := svc2.BeginSession(res.AgentID)
	if err != nil {
		t.Fatalf("BeginSession for the impostor attempt: %v", err)
	}
	if _, err := svc2.CompleteSession(ch2.Token, signToken(impostor, ch2.Token)); !errors.Is(err, auth.ErrBadSignature) {
		t.Fatalf("CompleteSession with a DIFFERENT keypair after the restart = %v, want ErrBadSignature; the recovered key must be the ENROLLED one, not merely a key", err)
	}
	if _, err := svc2.Authenticate(ch2.Token); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("the impostor's token authenticates (%v); a failed verification of a PENDING challenge must delete it", err)
	}
	// The genuine session is untouched by the impostor's failure.
	if _, err := svc2.Authenticate(ch.Token); err != nil {
		t.Fatalf("the genuine session stopped authenticating after an impostor failed a DIFFERENT challenge: %v", err)
	}
}

// TestWALRosterEpochAndEnrolledAtSurviveIndependently: Epoch and EnrolledAt are
// TWO fields answering two questions — "since when is this credential current"
// and "when did this agent join" — and they legitimately diverge the moment a
// re-key or a rotation lands.
//
// Today Service.Enrol sets them equal, which is why nothing else in the suite
// can tell them apart: a mutation collapsing Epoch into EnrolledAt anywhere in
// Encode/Decode left every other test green. Epoch is one of the four
// ENROL-SHAPE fields this whole task was blocked on, and a field the durable
// record cannot distinguish is a field that is not really there.
//
// So this constructs entries where the two DIFFER and carries them through the
// REAL durable path — Put, prepare fsync, commit fsync, close, reopen, replay,
// Get — asserting each keeps its own value.
func TestWALRosterEpochAndEnrolledAtSurviveIndependently(t *testing.T) {
	tests := []struct {
		name       string
		epoch      time.Time
		enrolledAt time.Time
	}{
		{
			// The realistic one: the agent joined, then re-keyed ten days later.
			name:       "epoch AFTER enrolled_at, as a later re-key leaves it",
			epoch:      recordEpoch.Add(240 * time.Hour),
			enrolledAt: recordEpoch,
		},
		{
			// The mirror, so a "max of the two" or "min of the two" collapse is
			// caught as well as a straight assignment.
			name:       "epoch BEFORE enrolled_at",
			epoch:      recordEpoch,
			enrolledAt: recordEpoch.Add(240 * time.Hour),
		},
		{
			// One nanosecond apart: the RFC3339Nano rendering must not round
			// them together either.
			name:       "epoch and enrolled_at one nanosecond apart",
			epoch:      recordEpoch.Add(time.Nanosecond),
			enrolledAt: recordEpoch,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.epoch.Equal(tc.enrolledAt) {
				t.Fatalf("test bug: the two instants are equal, so this case proves nothing")
			}
			dir := t.TempDir()

			r1, l1 := openRoster(t, dir)
			e := baseEntry(t, "worker", 1)
			e.Epoch = tc.epoch
			e.EnrolledAt = tc.enrolledAt
			if err := r1.Put(e); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if err := l1.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// --- the restart ---
			r2, l2 := openRoster(t, dir)
			defer l2.Close()

			got, ok := r2.Get(e.AgentID)
			if !ok {
				t.Fatalf("agent %q is absent after the restart", e.AgentID)
			}
			if !got.Epoch.Equal(tc.epoch) {
				t.Fatalf("Epoch came back %v, want %v; the epoch is STORED rather than derived precisely so a restart does not have to reconstruct it", got.Epoch, tc.epoch)
			}
			if !got.EnrolledAt.Equal(tc.enrolledAt) {
				t.Fatalf("EnrolledAt came back %v, want %v", got.EnrolledAt, tc.enrolledAt)
			}
			if got.Epoch.Equal(got.EnrolledAt) {
				t.Fatalf("Epoch and EnrolledAt came back IDENTICAL (%v) from an entry where they differed (%v vs %v); one field cannot answer both questions, and collapsing them makes a re-keyed credential indistinguishable from a fresh join",
					got.Epoch, tc.epoch, tc.enrolledAt)
			}
		})
	}
}

// TestWALRosterPersistsNothingButAgentRecords proves the "sessions do NOT
// survive a restart" half of invariant 3 rather than assuming it.
//
// A full enrolment + BeginSession + CompleteSession is driven through a real
// Service over a real durable roster, and then EVERY prepare record in the log
// is required to be an agent record. Nothing about a session, a challenge or a
// token may be on the platter: a token is a live bearer credential and putting
// replayable credential material on disk is exactly what the memory-only
// decision avoids.
func TestWALRosterPersistsNothingButAgentRecords(t *testing.T) {
	dir := t.TempDir()
	r, l := openRoster(t, dir)

	svc, _ := newService(t, auth.Options{Roster: r})
	pub, priv := newKeypair(t)
	res, err := svc.Enrol(auth.EnrolRequest{Name: "worker", PublicKey: pub, IdempotencyKey: "enrol-1"})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	ch, err := svc.BeginSession(res.AgentID)
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	sess, err := svc.CompleteSession(ch.Token, signToken(priv, ch.Token))
	if err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	if svc.SessionCount() != 1 {
		t.Fatalf("the live service holds %d sessions, want 1; the rest of this test is meaningless without one", svc.SessionCount())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	path := walPath(dir)
	recs, _, err := wal.ScanAll(path, wal.KindWAL)
	if err != nil {
		t.Fatalf("wal.ScanAll: %v", err)
	}
	prepares := 0
	for _, rec := range recs {
		if rec.Type != wal.TypePrepare {
			continue
		}
		prepares++
		entry, _, err := wal.DecodePrepare(path, rec)
		if err != nil {
			t.Fatalf("wal.DecodePrepare of record %d: %v", rec.Index, err)
		}
		if entry.Kind != auth.RecordKind {
			t.Fatalf("record %d has kind %q, but this package writes EXACTLY ONE kind (%q); a session, challenge or token record on disk is replayable credential material",
				rec.Index, entry.Kind, auth.RecordKind)
		}
	}
	if prepares != 1 {
		t.Fatalf("the log holds %d prepare records after one enrolment and one full session handshake, want exactly 1 (the enrolment)", prepares)
	}

	// Belt and braces on the bytes themselves: neither the token nor its hash
	// may appear anywhere in the file, including inside a record this scan
	// classified as an enrolment.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if idx := indexOf(raw, ch.Token); idx >= 0 {
		t.Fatalf("THE SESSION TOKEN IS ON DISK at byte %d. It is a live bearer credential; anyone who can read the data directory can authenticate as %q", idx, res.AgentID)
	}
	if idx := indexOf(raw, sess.TokenHash); idx >= 0 {
		t.Fatalf("the session token hash is on disk at byte %d; sessions are memory-only and nothing about one is persisted", idx)
	}

	// --- the restart ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()
	if _, ok := r2.Get(res.AgentID); !ok {
		t.Fatalf("agent %q is not on the roster after the restart, but its enrolment was acknowledged", res.AgentID)
	}
	svc2, _ := newService(t, auth.Options{Roster: r2})
	if n := svc2.SessionCount(); n != 0 {
		t.Fatalf("a service built over the recovered roster holds %d sessions, want 0; sessions do NOT survive a restart", n)
	}
	if _, err := svc2.Authenticate(ch.Token); !errors.Is(err, auth.ErrUnknownSession) {
		t.Fatalf("Authenticate with the pre-restart token = %v, want ErrUnknownSession; a token that outlived the process would be an unrevokable credential", err)
	}
}

// indexOf reports the first index of needle in haystack, or -1. Written out
// rather than pulled from bytes so the failure message can name the offset.
func indexOf(haystack []byte, needle string) int {
	if needle == "" {
		return -1
	}
	n := []byte(needle)
	for i := 0; i+len(n) <= len(haystack); i++ {
		if string(haystack[i:i+len(n)]) == needle {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Put semantics.
// ---------------------------------------------------------------------------

// TestWALRosterPutBeforeAttachWritesNothing covers the construction-order
// window: between NewWALRoster and Attach the roster can be READ and REBUILT
// but not WRITTEN. A silent in-memory success here would acknowledge an
// enrolment that never reached disk.
func TestWALRosterPutBeforeAttachWritesNothing(t *testing.T) {
	dir := t.TempDir()
	r := auth.NewWALRoster(nil)
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: r})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	// Deliberately NOT attached.

	e := baseEntry(t, "worker", 1)
	err = r.Put(e)
	if !errors.Is(err, auth.ErrNotAttached) {
		t.Fatalf("Put before Attach = %v, want ErrNotAttached", err)
	}
	if _, ok := r.Get(e.AgentID); ok {
		t.Errorf("the refused entry is in memory; a Put that wrote nothing must leave nothing behind")
	}
	if r.Len() != 0 {
		t.Errorf("Len = %d after a refused Put, want 0", r.Len())
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}
	if n := agentRecordCount(t, walPath(dir)); n != 0 {
		t.Fatalf("the log holds %d agent records after a Put that returned ErrNotAttached, want 0", n)
	}
}

// countingWriter is a DurableWriter double: it counts calls, remembers the
// entries and can be made to fail.
//
// On SUCCESS it calls applier.Apply, because that is what the real wal.Log does
// — Txn.Commit applies after the commit fsync, and Log.Write's contract is
// "durable AND visible in memory". A double that skipped it was not a cheaper
// wal.Log, it was a DIFFERENT one, and it modelled precisely the mis-wiring
// (a log whose applier is not this roster) that Put now refuses. On FAILURE it
// applies nothing, which is what lets it show that a failed Write leaves memory
// empty.
type countingWriter struct {
	mu      sync.Mutex
	n       int
	err     error
	entries []wal.Entry

	// applier is the roster to apply into. Left nil by the tests that only
	// count calls and never expect a Put to succeed.
	applier *auth.WALRoster
}

func (w *countingWriter) Write(e wal.Entry) (wal.Committed, error) {
	w.mu.Lock()
	w.n++
	w.entries = append(w.entries, e)
	failWith := w.err
	n := w.n
	applier := w.applier
	w.mu.Unlock()

	if failWith != nil {
		return wal.Committed{}, failWith
	}
	c := wal.Committed{PrepareIndex: uint64(2*n - 1), CommitIndex: uint64(2 * n), Entry: e}
	if applier != nil {
		// Apply is called OUTSIDE w.mu, mirroring the real Log: Apply takes the
		// roster's own map lock, and holding an unrelated lock across it would
		// invent a lock ordering the production path does not have.
		if err := applier.Apply(c); err != nil {
			return wal.Committed{}, err
		}
	}
	return c, nil
}

func (w *countingWriter) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// TestWALRosterSecondAttachIsRefused: a roster is bound to EXACTLY ONE durable
// log. Two would mean two durable histories behind one in-memory roster, and
// whichever won the race would silently own enrolments the other had already
// acknowledged.
func TestWALRosterSecondAttachIsRefused(t *testing.T) {
	r := auth.NewWALRoster(nil)
	first := &countingWriter{}
	second := &countingWriter{}
	// Only the FIRST writer applies, because only the first is ever attached —
	// that is the whole assertion below.
	first.applier = r

	if err := r.Attach(first); err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	if err := r.Attach(second); err == nil {
		t.Fatalf("second Attach succeeded; a roster must be bound to exactly one log")
	}
	if err := r.Attach(nil); err == nil {
		t.Fatalf("Attach(nil) succeeded; a nil writer would leave Put succeeding in memory with nothing on disk")
	}

	if err := r.Put(baseEntry(t, "worker", 1)); err != nil {
		t.Fatalf("Put after the refused second Attach: %v", err)
	}
	if got := first.calls(); got != 1 {
		t.Errorf("the FIRST writer saw %d writes, want 1; the refused Attach must not have displaced it", got)
	}
	if got := second.calls(); got != 0 {
		t.Errorf("the SECOND writer saw %d writes, want 0; it was never attached", got)
	}
}

// TestWALRosterDuplicateAgentIDBurnsNoFsync: a duplicate agent id is rejected
// from the in-memory map BEFORE anything is encoded or written, so the log does
// not grow by a single record. That is what makes "a duplicate never burns an
// fsync" a fact rather than a comment.
func TestWALRosterDuplicateAgentIDBurnsNoFsync(t *testing.T) {
	dir := t.TempDir()
	r, l := openRoster(t, dir)
	defer l.Close()

	e := baseEntry(t, "worker", 1)
	if err := r.Put(e); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	before := agentRecordCount(t, l.Path())
	if before != 1 {
		t.Fatalf("the log holds %d agent records after one Put, want 1", before)
	}

	// A DIFFERENT payload under the same id, so a silent overwrite would be
	// visible as well as a silent append.
	dup := e
	dup.AuthPublicKey = fixedKey(0x99)
	err := r.Put(dup)
	if !errors.Is(err, auth.ErrDuplicateAgentID) {
		t.Fatalf("Put of a duplicate agent id = %v, want ErrDuplicateAgentID", err)
	}
	if after := agentRecordCount(t, l.Path()); after != before {
		t.Fatalf("the log grew from %d to %d agent records on a REJECTED duplicate; a duplicate must be refused before the encode and the fsync", before, after)
	}
	got, ok := r.Get(e.AgentID)
	if !ok {
		t.Fatalf("agent %q vanished", e.AgentID)
	}
	if !reflect.DeepEqual(got.AuthPublicKey, normaliseEntry(e).AuthPublicKey) {
		t.Fatalf("the duplicate OVERWROTE the stored key; an overwrite rebinds a live identity to a different keypair (invariants 1 and 3)")
	}
}

// TestWALRosterRefusesAnIDThatSurvivedARestart is the RESTART half of the check
// above, and it is the one that matters for identity.
//
// # The hazard this pins
//
// The agent id suffix counter and the roster are recovered by DIFFERENT
// mechanisms, and they can disagree. If a bus restarts with a suffix counter
// that resumes from 1 (ids.NewNameSuffixes, which is still what
// cmd/agent-bus/main.go builds) while the roster is DURABLE, the very next
// enrolment of a previously-used name mints an id that ALREADY EXISTS in the
// recovered roster. Rebinding it would hand a NEW agent, holding a DIFFERENT
// keypair, the routing and authorization identity of the previous holder —
// exactly what invariant 1 forbids, and materially worse than the same bug
// against an empty in-memory roster, because now the previous holder is real.
//
// The defence is that Put checks the RECOVERED map, so it refuses. This test
// exists because the same-process duplicate check above would still pass with a
// roster that rebuilt nothing at replay — it is the restart that makes the
// assertion about recovered state rather than about a map this process filled.
//
// It FAILS CLOSED: the enrolment is refused with ErrDuplicateAgentID (a 500,
// an internal invariant breach) and NOTHING is rewritten. A refused enrolment is
// recoverable; a rebound identity is not. Note the defence is keyed on the
// recovered ROSTER, which makes it strictly stronger than hub's h.recovered
// detector — that one is populated only from message senders and recipients, so
// an agent that enrolled and never sent leaves it no trace at all.
func TestWALRosterRefusesAnIDThatSurvivedARestart(t *testing.T) {
	dir := t.TempDir()

	first := baseEntry(t, "worker", 1)
	r1, l1 := openRoster(t, dir)
	if err := r1.Put(first); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- restart: a brand new roster, rebuilt only by replay ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()
	if r2.Len() != 1 {
		t.Fatalf("after the restart the roster holds %d agents, want 1; the rest of this test would prove nothing", r2.Len())
	}
	before := agentRecordCount(t, l2.Path())

	// A suffix counter that restarted from 1 re-mints this exact id, and the
	// agent behind it holds a DIFFERENT keypair.
	collision := baseEntry(t, "worker", 1)
	collision.AuthPublicKey = fixedKey(0x99)
	if collision.AgentID != first.AgentID {
		t.Fatalf("test bug: the collision entry has id %q, want the recovered id %q", collision.AgentID, first.AgentID)
	}

	err := r2.Put(collision)
	if !errors.Is(err, auth.ErrDuplicateAgentID) {
		t.Fatalf("Put of an id that survived the restart = %v, want ErrDuplicateAgentID; a durable roster that accepts a re-minted id rebinds a live identity to a different keypair (invariant 1)", err)
	}
	if after := agentRecordCount(t, l2.Path()); after != before {
		t.Fatalf("the log grew from %d to %d agent records on a REJECTED post-restart collision; the refusal must happen before the encode and the fsync", before, after)
	}
	got, ok := r2.Get(first.AgentID)
	if !ok {
		t.Fatalf("agent %q vanished from the recovered roster", first.AgentID)
	}
	if !reflect.DeepEqual(got.AuthPublicKey, normaliseEntry(first).AuthPublicKey) {
		t.Fatalf("the post-restart collision REBOUND %q to a different keypair; the recovered holder's key must be untouched", first.AgentID)
	}
}

// TestWALRosterWriteFailureLeavesMemoryMatchingDisk: when the durable write
// fails, the error is returned and the entry is NOT in memory — Apply never
// ran, so memory still matches disk.
func TestWALRosterWriteFailureLeavesMemoryMatchingDisk(t *testing.T) {
	boom := errors.New("the platter is on fire")
	w := &countingWriter{err: boom}
	r := auth.NewWALRoster(nil)
	if err := r.Attach(w); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	e := baseEntry(t, "worker", 1)
	err := r.Put(e)
	if !errors.Is(err, boom) {
		t.Fatalf("Put with a failing writer = %v, want the writer's error wrapped", err)
	}
	if _, ok := r.Get(e.AgentID); ok {
		t.Fatalf("the entry is in MEMORY after the durable write FAILED; memory must never hold an enrolment disk does not")
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d after a failed Put, want 0", r.Len())
	}
	if w.calls() != 1 {
		t.Fatalf("the writer saw %d calls, want 1", w.calls())
	}
}

// TestWALRosterConcurrentPuts runs under -race: concurrency here is the
// product, and a data race is a P0.
func TestWALRosterConcurrentPuts(t *testing.T) {
	const n = 16

	t.Run("distinct ids all land", func(t *testing.T) {
		dir := t.TempDir()
		r, l := openRoster(t, dir)
		defer l.Close()

		// Entries and the probe id are built on the TEST goroutine: baseEntry
		// takes *testing.T, and a helper that may call Fatalf has no business
		// running on a goroutine the test is not waiting on.
		entries := make([]auth.RosterEntry, n)
		for i := range entries {
			entries[i] = baseEntry(t, "worker", uint64(i+1))
		}
		probe := mustAgentID(t, "worker", 1)

		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = r.Put(entries[i])
			}()
		}
		// Concurrent readers, so Get and Len are exercised against a moving map
		// under the race detector rather than against a quiescent one.
		stop := make(chan struct{})
		var readers sync.WaitGroup
		for i := 0; i < 4; i++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for {
					select {
					case <-stop:
						return
					default:
						_ = r.Len()
						_, _ = r.Get(probe)
					}
				}
			}()
		}
		wg.Wait()
		close(stop)
		readers.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("Put of worker-%d: %v", i+1, err)
			}
		}
		if got := r.Len(); got != n {
			t.Fatalf("the roster holds %d agents after %d concurrent Puts, want %d; an enrolment was LOST", got, n, n)
		}
		if got := agentRecordCount(t, l.Path()); got != n {
			t.Fatalf("the log holds %d agent records after %d concurrent Puts, want %d", got, n, n)
		}
	})

	t.Run("the same id: exactly one wins", func(t *testing.T) {
		dir := t.TempDir()
		r, l := openRoster(t, dir)
		defer l.Close()

		e := baseEntry(t, "worker", 1)
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = r.Put(e)
			}()
		}
		wg.Wait()

		wins := 0
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, auth.ErrDuplicateAgentID):
			default:
				t.Fatalf("Put %d = %v, want nil or ErrDuplicateAgentID", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d of %d concurrent Puts of the SAME agent id succeeded, want exactly 1", wins, n)
		}
		if got := r.Len(); got != 1 {
			t.Fatalf("the roster holds %d agents, want 1", got)
		}
		if got := agentRecordCount(t, l.Path()); got != 1 {
			t.Fatalf("the log holds %d agent records for one agent id, want exactly 1; a duplicate reached the platter", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Replay semantics: what Apply does with records Put would never have produced.
// These are reached by writing bytes straight through wal.Log.Write, which is
// the only way to construct a log Put refuses to create.
// ---------------------------------------------------------------------------

// TestWALRosterReplayKeepsTheFirstOfADuplicatePair: a duplicate agent id in the
// log keeps the FIRST record. Never an overwrite — an overwrite rebinds a live
// identity to a different keypair (invariants 1 and 3).
//
// The two records differ in EnrolledAt, so the assertion names which of them
// survived rather than merely counting one.
func TestWALRosterReplayKeepsTheFirstOfADuplicatePair(t *testing.T) {
	dir := t.TempDir()
	l := openPlainLog(t, dir)

	first := baseEntry(t, "worker", 1)
	second := first
	second.EnrolledAt = recordEpoch.Add(48 * time.Hour)
	second.Epoch = second.EnrolledAt
	second.AuthPublicKey = fixedKey(0x99)

	for _, e := range []auth.RosterEntry{first, second} {
		body, err := auth.Encode(e)
		if err != nil {
			t.Fatalf("Encode(%s): %v", e.AgentID, err)
		}
		if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: body}); err != nil {
			t.Fatalf("wal.Write: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, l2 := openRoster(t, dir)
	defer l2.Close()

	if got := r.Len(); got != 1 {
		t.Fatalf("the roster holds %d agents after replaying two records for ONE id, want 1", got)
	}
	got, ok := r.Get(first.AgentID)
	if !ok {
		t.Fatalf("agent %q is absent after replay", first.AgentID)
	}
	if !got.EnrolledAt.Equal(first.EnrolledAt) {
		t.Fatalf("replay kept the SECOND record (enrolled_at %v), want the FIRST (%v); an overwrite rebinds a live identity to a different keypair",
			got.EnrolledAt, first.EnrolledAt)
	}
	if !reflect.DeepEqual(got, normaliseEntry(first)) {
		t.Fatalf("the surviving entry is not the first record.\n  got  %+v\n  want %+v", got, normaliseEntry(first))
	}
}

// TestWALRosterReplaySkipsAnUndecodableRecordAndStarts: invariant 6 settled this
// trade on 2026-08-02 — recovery ALWAYS reaches a running server, damaged
// records are discarded, and the bus starts. A bus held hostage by one bad
// record is worse than a bus that has lost an enrolment.
func TestWALRosterReplaySkipsAnUndecodableRecordAndStarts(t *testing.T) {
	dir := t.TempDir()
	l := openPlainLog(t, dir)

	good := baseEntry(t, "worker", 1)
	goodBody, err := auth.Encode(good)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Valid JSON (wal insists on that) but not a record THIS build understands.
	bad := json.RawMessage(`{"v":999}`)
	if _, err := auth.Decode(bad); !errors.Is(err, auth.ErrInvalidRecord) {
		t.Fatalf("the fixture is not actually undecodable: Decode = %v, want ErrInvalidRecord", err)
	}

	for _, body := range []json.RawMessage{bad, goodBody} {
		if _, err := l.Write(wal.Entry{Kind: auth.RecordKind, Body: body}); err != nil {
			t.Fatalf("wal.Write: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The bus STARTS. That is the assertion: openRoster fails the test if
	// wal.Open returns an error.
	r, l2 := openRoster(t, dir)
	defer l2.Close()

	if got := r.Len(); got != 1 {
		t.Fatalf("the roster holds %d agents, want 1: the undecodable record must be skipped and the good one kept", got)
	}
	if _, ok := r.Get(good.AgentID); !ok {
		t.Fatalf("the GOOD record was lost alongside the undecodable one; a discard must not cascade")
	}
}

// TestWALRosterToleratesRecordsOfAnotherKind: THE WAL IS SHARED. It carries
// store.RecordKind message records — on a real bus, overwhelmingly more of them
// than enrolments — and whatever kinds later epics add. Apply's foreign-Kind
// skip is what lets one log serve them all, and it had no test: every other
// test in this package writes agent records exclusively, so a mutation making
// Apply ERROR on a foreign Kind stayed green across the whole suite.
//
// # The two halves are asserted separately because they FAIL DIFFERENTLY
//
// It is worth being exact about the blast radius, because it is not the one an
// "aborts recovery" summary would suggest:
//
//   - ON A LIVE COMMIT it is FATAL AND IMMEDIATE. wal.Txn.Commit calls the
//     applier after the commit fsync; an error there marks the whole Log
//     DIVERGED and every subsequent write fails. On a shared log that is the
//     FIRST message a restarted bus sends.
//   - AT REPLAY it is SILENT AND TOTAL. wal.Replay does NOT abort on an applier
//     error (replay.go: "the entry is dropped from the rebuilt memory state and
//     reported as the acknowledged loss it is"). Every message record on the
//     bus would be counted as a discard and dropped from the rebuilt state, and
//     the bus would start looking healthy.
//
// So the live half is asserted by writing the foreign records through the log
// the roster IS the applier of — which is the production wiring, and the reason
// the earlier version of this test proved nothing: it wrote them through a log
// with no applier at all, where Apply never ran.
func TestWALRosterToleratesRecordsOfAnotherKind(t *testing.T) {
	dir := t.TempDir()

	// The roster IS this log's applier, exactly as the shared production log
	// will have it. Every Write below therefore runs through WALRoster.Apply.
	r1, l1 := openRoster(t, dir)

	before := baseEntry(t, "worker", 1)
	after := baseEntry(t, "worker", 2)
	if err := r1.Put(before); err != nil {
		t.Fatalf("Put(%s): %v", before.AgentID, err)
	}

	// "message" is store.RecordKind, spelled out rather than imported so this
	// test does not drag internal/store into the auth test binary. It is also
	// the exact name internal/wal's own Entry.Kind doc gives this discriminator.
	foreign := []wal.Entry{
		{Kind: "message", Body: json.RawMessage(`{"id":"msg-1","seq":1,"sender":"` + testBusID + `.worker-1"}`)},
		// A kind no build has seen, for the "whatever later epics add" half.
		{Kind: "some-future-epic", Body: json.RawMessage(`{"whatever":true}`)},
	}
	for _, e := range foreign {
		if _, err := l1.Write(e); err != nil {
			t.Fatalf("wal.Write of a %q record through a log this roster applies for = %v.\nA roster that treats another component's records as damage poisons the SHARED log: wal.Txn.Commit marks the whole Log diverged on an applier error, so every write after this one fails too", e.Kind, err)
		}
	}

	// An enrolment AFTER the foreign records, so the assertion is not merely
	// "the log survived" but "it carried on working through them".
	if err := r1.Put(after); err != nil {
		t.Fatalf("Put(%s) after two foreign-kind records: %v", after.AgentID, err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- the restart ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()

	if rec := l2.Recovered(); rec.DiscardCount != 0 {
		t.Fatalf("recovery counted %d DISCARDS replaying a log holding two enrolments and two foreign-kind records, want 0 (%+v).\nwal.Replay does not abort when an applier rejects an entry — it DROPS the entry and records the loss — so a roster that errored on a foreign Kind would silently delete every message record on the bus while the start still looked healthy", rec.DiscardCount, rec.Discarded)
	}
	if got := r2.Len(); got != 2 {
		t.Fatalf("the roster holds %d agents after replaying two enrolments either side of two foreign-kind records, want 2", got)
	}
	for _, e := range []auth.RosterEntry{before, after} {
		if _, ok := r2.Get(e.AgentID); !ok {
			t.Fatalf("agent %q is absent after replay; a foreign-kind record interrupted the enrolments around it", e.AgentID)
		}
	}
}

// ---------------------------------------------------------------------------
// Invariant 6: EVERY DISCARD IS LOGGED. That is the absolute half — silent
// discard is the defect that was rated P0, not discard itself.
//
// Nothing else in this package's tests passes a logger at all (every
// construction is NewWALRoster(nil)), so the loudness of both discard paths was
// entirely unproven: deleting either r.log.Error call left the suite green
// while turning a lost enrolment into an invisible one.
// ---------------------------------------------------------------------------

// discardLine returns the single logged line containing want, failing the test
// if there is not exactly one. Asserting on the LINE rather than on the whole
// buffer is what makes the prepare/commit index checks below meaningful — an
// index logged on some other record is not this discard being specific.
func discardLine(t *testing.T, logged string, want string) string {
	t.Helper()
	var hits []string
	for _, line := range strings.Split(strings.TrimRight(logged, "\n"), "\n") {
		if strings.Contains(line, want) {
			hits = append(hits, line)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("the log holds %d lines containing %q, want exactly 1.\n--- log ---\n%s-----------", len(hits), want, logged)
	}
	return hits[0]
}

var logIndexRE = regexp.MustCompile(`(?:^| )(prepare_index|commit_index)=(\d+)`)

// assertNamesTheRecord requires a discard line to carry BOTH wal indices with
// numeric values, and the commit to sit after the prepare. Without them an
// operator is told an enrolment was dropped but not WHICH one, which is barely
// better than silence: the whole remedy is to go and look at that record.
func assertNamesTheRecord(t *testing.T, line string) {
	t.Helper()
	found := map[string]uint64{}
	for _, m := range logIndexRE.FindAllStringSubmatch(line, -1) {
		n, err := strconv.ParseUint(m[2], 10, 64)
		if err != nil {
			t.Fatalf("the discard line reports %s=%q, which is not a record index.\n  line: %s", m[1], m[2], line)
		}
		found[m[1]] = n
	}
	for _, key := range []string{"prepare_index", "commit_index"} {
		if _, ok := found[key]; !ok {
			t.Fatalf("the discard line names no %s; an operator is told an enrolment was dropped but not which record to look at.\n  line: %s", key, line)
		}
	}
	// Compared NUMERICALLY, never as text: "9" sorts after "10" as a string.
	if found["prepare_index"] >= found["commit_index"] {
		t.Errorf("the discard line reports prepare_index=%d and commit_index=%d; the commit record is written after the prepare it names.\n  line: %s", found["prepare_index"], found["commit_index"], line)
	}
}

// TestWALRosterLogsEveryDiscard covers BOTH paths on which Apply drops a record
// it has already been handed: a record it cannot decode, and a duplicate agent
// id. Each costs an acknowledged agent its enrolment, and each returns nil so
// the bus starts anyway (invariant 6) — which is exactly why the log line is
// the only thing standing between a lost agent and a mystery.
func TestWALRosterLogsEveryDiscard(t *testing.T) {
	t.Run("an undecodable enrolment record", func(t *testing.T) {
		dir := t.TempDir()
		l := openPlainLog(t, dir)

		// Valid JSON (wal insists) but not a record THIS build understands.
		bad := json.RawMessage(`{"v":999}`)
		if _, err := auth.Decode(bad); !errors.Is(err, auth.ErrInvalidRecord) {
			t.Fatalf("the fixture is not actually undecodable: Decode = %v", err)
		}
		putEncoded(t, l, bad)
		good, err := auth.Encode(baseEntry(t, "worker", 1))
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		putEncoded(t, l, good)
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		var logged bytes.Buffer
		r, l2 := openRosterWithLogger(t, dir, logging.New(&logged, logging.LevelDebug))
		defer l2.Close()
		if r.Len() != 1 {
			t.Fatalf("the roster holds %d agents, want 1 (the good record); the fixture is wrong", r.Len())
		}

		line := discardLine(t, logged.String(), "DISCARDING an enrolment record that could not be decoded")
		if !strings.Contains(line, "level=error") {
			t.Errorf("the discard is logged at a level below error.\n  line: %s", line)
		}
		assertNamesTheRecord(t, line)
	})

	t.Run("a duplicate agent id", func(t *testing.T) {
		dir := t.TempDir()
		l := openPlainLog(t, dir)

		first := baseEntry(t, "worker", 1)
		second := first
		second.EnrolledAt = recordEpoch.Add(48 * time.Hour)
		second.Epoch = second.EnrolledAt
		second.AuthPublicKey = fixedKey(0x99)
		for _, e := range []auth.RosterEntry{first, second} {
			body, err := auth.Encode(e)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			putEncoded(t, l, body)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		var logged bytes.Buffer
		r, l2 := openRosterWithLogger(t, dir, logging.New(&logged, logging.LevelDebug))
		defer l2.Close()
		if r.Len() != 1 {
			t.Fatalf("the roster holds %d agents after replaying two records for ONE id, want 1", r.Len())
		}

		line := discardLine(t, logged.String(), "DISCARDING a DUPLICATE enrolment record")
		if !strings.Contains(line, "level=error") {
			t.Errorf("the duplicate discard is logged at a level below error.\n  line: %s", line)
		}
		if !strings.Contains(line, "agent_id="+first.AgentID) {
			t.Errorf("the discard line does not name the agent id it dropped (want agent_id=%s).\n  line: %s", first.AgentID, line)
		}
		assertNamesTheRecord(t, line)
	})

	t.Run("a nil logger is still safe", func(t *testing.T) {
		// NewWALRoster documents that logger may be nil and logging.Logger's
		// methods are nil-safe. If that ever stops being true, both discard
		// paths panic during recovery — which is a refusal to boot, the one
		// outcome invariant 6 forbids.
		dir := t.TempDir()
		l := openPlainLog(t, dir)
		putEncoded(t, l, json.RawMessage(`{"v":999}`))
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r, l2 := openRoster(t, dir)
		defer l2.Close()
		if r.Len() != 0 {
			t.Fatalf("the roster holds %d agents, want 0", r.Len())
		}
	})
}

// ---------------------------------------------------------------------------
// Put's POST-WRITE CONFIRMATION, and the mis-wiring it exists to catch.
// ---------------------------------------------------------------------------

// TestWALRosterPutRefusesWhenTheEntryNeverReachedTheServingRoster pins the one
// failure mode a durable roster cannot detect any other way.
//
// Attach takes any DurableWriter and cannot check that the log was opened with
// THIS roster as its applier. Hand it a log wired to a different applier — easy,
// since the hub is an applier too and the startup path has to multiplex them —
// and without this check every Put writes durably, returns nil, and the roster
// stays permanently EMPTY. The agent is told an id it can then never
// authenticate with, and nothing anywhere fails.
//
// A countingWriter with applier left nil IS that mis-wiring: it succeeds, it is
// durable, and it applies nothing.
func TestWALRosterPutRefusesWhenTheEntryNeverReachedTheServingRoster(t *testing.T) {
	r := auth.NewWALRoster(nil)
	w := &countingWriter{} // applier deliberately nil: a log wired elsewhere
	if err := r.Attach(w); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	e := baseEntry(t, "worker", 1)
	err := r.Put(e)
	if err == nil {
		t.Fatalf("Put through a writer that commits but never applies returned NO error; the enrolment is durable and the serving roster is empty, so the agent holds an id it can never authenticate with and nothing fails")
	}
	// Matched on text, unavoidably: there is no sentinel for this, because it
	// is a WIRING defect an operator has to read about rather than a condition
	// any caller can branch on. Asserted anyway so the message that names the
	// remedy cannot silently disappear.
	if !strings.Contains(err.Error(), "ABSENT from the serving roster") {
		t.Errorf("Put = %v; the error must say the entry is ABSENT from the serving roster and point at the wal.Open Applier wiring", err)
	}
	for _, sentinel := range []error{auth.ErrNotAttached, auth.ErrDuplicateAgentID, auth.ErrInvalidRecord} {
		if errors.Is(err, sentinel) {
			t.Errorf("Put = %v, which wraps %v; a mis-wired applier is none of those and must not be diagnosed as one", err, sentinel)
		}
	}

	if _, ok := r.Get(e.AgentID); ok {
		t.Errorf("Get reports %q present after a Put that never applied it", e.AgentID)
	}
	if n := r.Len(); n != 0 {
		t.Errorf("Len = %d after a Put that never applied, want 0", n)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List = %+v after a Put that never applied, want empty", got)
	}
	if got := w.calls(); got != 1 {
		t.Errorf("the writer saw %d calls, want 1; the record IS durable and the failure is about the SERVING copy, not about the write", got)
	}
}

// ---------------------------------------------------------------------------
// Put's validation runs BEFORE anything is written.
// ---------------------------------------------------------------------------

// TestWALRosterPutRefusesAnUnstorableEntryBeforeWriting is the Put half of
// TestEncodeRefusesAnUnstorableEntry (record_test.go), over the SAME table.
//
// The assertion that earns its keep is the record COUNT: "refused" and
// "refused with NOTHING WRITTEN" are different guarantees, and only the second
// leaves the log free of a record no reader will accept.
//
// Be precise about what this does and does NOT kill, because it was previously
// justified with a claim that measurement refuted. Deleting validateRosterEntry
// from Put ALONE changes nothing observable — Encode validates too (record.go),
// so the entry is still refused with nothing written and this test stays green.
// It is the BOTH-SITES deletion that this test kills, together with
// TestEncodeRefusesAnUnstorableEntry. Neither test alone pins the pair; keeping
// both is the point.
func TestWALRosterPutRefusesAnUnstorableEntryBeforeWriting(t *testing.T) {
	for _, tc := range unstorableEntries(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r, l := openRoster(t, dir)
			defer l.Close()

			if before := agentRecordCount(t, l.Path()); before != 0 {
				t.Fatalf("the fresh log already holds %d agent records", before)
			}

			err := r.Put(tc.entry)
			if err == nil {
				t.Fatalf("Put(%+v) returned NO error; this entry cannot be stored", tc.entry)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("Put = %v, want an error satisfying errors.Is(err, %v)", err, tc.sentinel)
			}
			if n := agentRecordCount(t, l.Path()); n != 0 {
				t.Fatalf("the log grew by %d agent records on a REJECTED entry, want 0; a record that cannot be stored must fail with NOTHING written, not be discovered as broken at the next replay when it is already durable and acknowledged", n)
			}
			if _, ok := r.Get(tc.entry.AgentID); ok {
				t.Errorf("the refused entry %q is in memory", tc.entry.AgentID)
			}
			if n := r.Len(); n != 0 {
				t.Errorf("Len = %d after a refused Put, want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// List(): the seam that makes a restart survivable OUTSIDE this package.
//
// Without it the hub rebuilds no roster after a restart, and every recovered
// agent authenticates perfectly while every send it makes is refused 403 and
// every message addressed to it 404 — a bus that authenticates everyone and
// serves nobody, failing in the direction that looks like auth working.
// ---------------------------------------------------------------------------

// rosterImpl is one Roster implementation under the shared List expectations.
// BOTH must satisfy them: MemoryRoster is what the enrolment and session tests
// run through, WALRoster is what production will, and a List that behaved
// differently between them would make every one of those tests a claim about
// the wrong object.
type rosterImpl struct {
	name string
	open func(t *testing.T) auth.Roster
}

func rosterImpls() []rosterImpl {
	return []rosterImpl{
		{"MemoryRoster", func(t *testing.T) auth.Roster {
			t.Helper()
			return auth.NewMemoryRoster()
		}},
		{"WALRoster", func(t *testing.T) auth.Roster {
			t.Helper()
			r, l := openRoster(t, t.TempDir())
			t.Cleanup(func() { _ = l.Close() })
			return r
		}},
	}
}

// TestRosterListIsSortedAndDeepCopied covers the shared contract of the two
// List implementations.
func TestRosterListIsSortedAndDeepCopied(t *testing.T) {
	for _, impl := range rosterImpls() {
		impl := impl
		t.Run(impl.name, func(t *testing.T) {
			t.Run("an empty roster lists nothing, and not a nil slice", func(t *testing.T) {
				got := impl.open(t).List()
				if got == nil {
					t.Fatalf("List() on an empty roster returned a NIL slice; a caller must be able to tell \"listed, and it is empty\" from \"not listed\"")
				}
				if len(got) != 0 {
					t.Fatalf("List() on an empty roster returned %d entries (%+v)", len(got), got)
				}
			})

			t.Run("entries come back sorted by agent id", func(t *testing.T) {
				r := impl.open(t)
				// Inserted in an order that is neither sorted nor reverse
				// sorted, and including worker-10 so a numeric-looking sort and
				// the documented lexicographic one are distinguishable.
				for _, e := range []auth.RosterEntry{
					baseEntry(t, "worker", 2),
					baseEntry(t, "relay", 1),
					baseEntry(t, "worker", 10),
					baseEntry(t, "worker", 1),
				} {
					if err := r.Put(e); err != nil {
						t.Fatalf("Put(%s): %v", e.AgentID, err)
					}
				}
				want := []string{
					mustAgentID(t, "relay", 1),
					mustAgentID(t, "worker", 1),
					mustAgentID(t, "worker", 10),
					mustAgentID(t, "worker", 2),
				}
				if !sort.StringsAreSorted(want) {
					t.Fatalf("test bug: the expected order %v is not sorted", want)
				}
				// Repeated because Go RANDOMISES map iteration: one unsorted
				// call has a real chance of coming out ordered by luck, and a
				// listing that is only sometimes ordered is a flaky consumer.
				for i := 0; i < 8; i++ {
					var got []string
					for _, e := range r.List() {
						got = append(got, e.AgentID)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("List() call %d returned %v, want %v; an order that varies run to run turns a stable listing into a flaky one and a roster rebuild into something no test can pin", i, got, want)
					}
				}
			})

			t.Run("the entries are deep copies", func(t *testing.T) {
				r := impl.open(t)
				e := richEntry(t, "worker", 1)
				if err := r.Put(e); err != nil {
					t.Fatalf("Put: %v", err)
				}
				// Built fresh rather than from e, so the mutations below cannot
				// reach into the expectation and make the comparison vacuous.
				snapshot := normaliseEntry(richEntry(t, "worker", 1))

				list := r.List()
				if len(list) != 1 {
					t.Fatalf("List() returned %d entries, want 1", len(list))
				}
				if !reflect.DeepEqual(list[0], snapshot) {
					t.Fatalf("List() returned an entry that is not the one stored.\n  got  %+v\n  want %+v", list[0], snapshot)
				}

				// (1) mutate the SLICE and the scalar fields of its entries.
				list[0].AgentID = "tampered"
				list[0].Name = "tampered"
				list[0].InviteID = "tampered"
				list[0].EnrolledAt = recordEpoch.Add(999 * time.Hour)
				list = append(list, richEntry(t, "worker", 2))
				_ = list

				// (2) mutate the SHARED-BACKING-ARRAY fields — the ones that
				// decide WHICH KEY authenticates as this agent and WHICH
				// CERTIFICATE is accepted for it.
				again := r.List()
				again[0].AuthPublicKey[0] ^= 0xFF
				again[0].MessagingPublicKey[0] ^= 0xFF
				again[0].CertBindings[0].Fingerprint[0] ^= 0xFF
				if again[0].CertBindings[0].RetiredAt == nil {
					t.Fatalf("the fixture's first binding is not retired, so the pointer aliasing check below proves nothing")
				}
				// Through the POINTER: a shared *time.Time would un-retire a
				// revoked certificate the bus must stop accepting.
				*again[0].CertBindings[0].RetiredAt = recordEpoch.Add(4242 * time.Hour)
				again[0].CertBindings[1].BoundAt = recordEpoch.Add(4242 * time.Hour)

				relisted := r.List()
				if len(relisted) != 1 {
					t.Fatalf("List() returned %d entries after the mutations, want 1", len(relisted))
				}
				if !reflect.DeepEqual(relisted[0], snapshot) {
					t.Fatalf("mutating what List() handed back changed the STORED credential, seen through List().\n  got  %+v\n  want %+v", relisted[0], snapshot)
				}
				if got := mustGet(t, r, snapshot.AgentID); !reflect.DeepEqual(got, snapshot) {
					t.Fatalf("mutating what List() handed back changed the STORED credential, seen through Get().\n  got  %+v\n  want %+v", got, snapshot)
				}
			})
		})
	}
}

// mustGet fetches an entry or fails the test.
func mustGet(t *testing.T, r auth.Roster, agentID string) auth.RosterEntry {
	t.Helper()
	e, ok := r.Get(agentID)
	if !ok {
		t.Fatalf("agent %q is absent from the roster", agentID)
	}
	return e
}

// TestWALRosterListReturnsEveryRecoveredAgent is the restart half: List is what
// the hub's roster is rebuilt FROM, so it has to carry every recovered agent
// with the provenance it enrolled with — not the instant recovery ran.
//
// Each agent is given a DISTINCT EnrolledAt and an Epoch that differs from it,
// so a recovery that stamped "now", or that collapsed the two fields, is
// visible per agent rather than as a single indistinguishable diff.
func TestWALRosterListReturnsEveryRecoveredAgent(t *testing.T) {
	dir := t.TempDir()

	r1, l1 := openRoster(t, dir)
	want := make([]auth.RosterEntry, 0, 4)
	for i, spec := range []struct {
		name string
		n    uint64
	}{
		{"worker", 2},
		{"relay", 1},
		{"worker", 10},
		{"worker", 1},
	} {
		e := baseEntry(t, spec.name, spec.n)
		e.EnrolledAt = recordEpoch.Add(time.Duration(i+1) * time.Hour)
		e.Epoch = e.EnrolledAt.Add(time.Duration(i+1) * time.Minute)
		if i == 1 {
			e = richEntry(t, spec.name, spec.n)
			e.EnrolledAt = recordEpoch.Add(2 * time.Hour)
			e.Epoch = e.EnrolledAt.Add(2 * time.Minute)
		}
		if err := r1.Put(e); err != nil {
			t.Fatalf("Put(%s): %v", e.AgentID, err)
		}
		want = append(want, normaliseEntry(e))
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sort.Slice(want, func(i, j int) bool { return want[i].AgentID < want[j].AgentID })

	// --- the restart ---
	r2, l2 := openRoster(t, dir)
	defer l2.Close()

	got := r2.List()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() after the restart did not return the recovered roster.\n  got  %+v\n  want %+v", got, want)
	}
	for i := range got {
		if !got[i].EnrolledAt.Equal(want[i].EnrolledAt) {
			t.Errorf("agent %q was listed with enrolled_at %v, want the ORIGINAL %v", got[i].AgentID, got[i].EnrolledAt, want[i].EnrolledAt)
		}
		if got[i].Epoch.Equal(got[i].EnrolledAt) {
			t.Errorf("agent %q came back with epoch == enrolled_at (%v) from a record where they differed", got[i].AgentID, got[i].Epoch)
		}
	}
}

// TestRosterListUnderConcurrentPuts runs under -race. Concurrency here is the
// product: List copies out of a map other goroutines are inserting into, and a
// listing that raced would be a P0.
func TestRosterListUnderConcurrentPuts(t *testing.T) {
	const n = 16

	for _, impl := range rosterImpls() {
		impl := impl
		t.Run(impl.name, func(t *testing.T) {
			r := impl.open(t)

			// Built on the TEST goroutine: baseEntry may call Fatalf, which has
			// no business running anywhere the test is not waiting on.
			entries := make([]auth.RosterEntry, n)
			for i := range entries {
				entries[i] = baseEntry(t, "worker", uint64(i+1))
			}

			var wg sync.WaitGroup
			errs := make([]error, n)
			for i := 0; i < n; i++ {
				i := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = r.Put(entries[i])
				}()
			}

			// Readers assert sortedness on every snapshot they see. They report
			// through a slice rather than calling Fatalf, which is only legal
			// on the test goroutine.
			stop := make(chan struct{})
			var readers sync.WaitGroup
			var badMu sync.Mutex
			var bad []string
			for i := 0; i < 4; i++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						snapshot := r.List()
						ids := make([]string, 0, len(snapshot))
						for _, e := range snapshot {
							ids = append(ids, e.AgentID)
						}
						if !sort.StringsAreSorted(ids) {
							badMu.Lock()
							bad = append(bad, strings.Join(ids, ","))
							badMu.Unlock()
						}
					}
				}()
			}
			wg.Wait()
			close(stop)
			readers.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("Put of worker-%d: %v", i+1, err)
				}
			}
			badMu.Lock()
			defer badMu.Unlock()
			if len(bad) != 0 {
				t.Fatalf("List() returned %d UNSORTED snapshots while Puts were in flight, e.g. %s", len(bad), bad[0])
			}
			if got := r.List(); len(got) != n {
				t.Fatalf("List() returned %d entries after %d concurrent Puts, want %d; an enrolment was LOST", len(got), n, n)
			}
		})
	}
}
