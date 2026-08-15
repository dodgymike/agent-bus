package store_test

// RELAY-24-FU-STOREMSGLOOKUP — THE CRASH-INJECTION HALF.
//
// CLAUDE.md: durability and recovery code must have a crash-injection test — a
// test that writes, kills at a chosen point in the write path, and asserts what
// recovery yields. "The code looks right" is not evidence for a durability
// claim, and this field's claim is a durability claim: OriginMessageID has
// exactly ONE consumer, relay.Forwarder.RecoverMessage, reached from exactly one
// place — Forwarder.Resume, which runs ONLY AFTER A RESTART. A correlation key
// held in memory alone would be empty at precisely the moment, and the only
// moment, it is read. So the unit round-trip in
// originlookup_relay24fu_test.go is necessary and not sufficient: this file
// drives the field through a REAL WAL, cuts the file, and asks what came back.
//
// # What is proven here
//
//	invariant 4  Nothing ACKNOWLEDGED is lost. Every entry whose commit fsync had
//	             returned when the file ended is resolvable after recovery — by
//	             its local id, and, for a relay ingest, by its ORIGIN id with the
//	             correlation key intact.
//	invariant 5  A crash recovers to a PREFIX of accepted history. The cut
//	             between present and absent sits exactly at the last completed
//	             commit fsync.
//	invariant 1  THE NEGATIVE, and the reason a point-lookup structure needs its
//	             own crash test at all: a DISCARDED record's id must never be
//	             re-resolvable. An index that outlived the record it names would
//	             hand a relay forward a message the log says was never accepted.
//	invariant 6  Recovery ALWAYS reaches a running server: the damaged tail is
//	             discarded and the store is usable, never a refusal to start.
//	invariant 10 The FALLBACK still works after recovery, which is what makes the
//	             single-hop egress case resumable with no new durable state.
//
// # Why truncation rather than a SIGKILL child
//
// internal/wal already owns the real-signal crash tests (replay_crash_test.go
// kills a child process mid-write) and proves that what a kill leaves on disk is
// a truncated or torn tail. This file is about what the STORE's lookup surface
// does with such a file, so it injects the crash by producing exactly those
// bytes — at EVERY offset in the last entry's frames, rather than at one offset
// somebody thought was interesting. store_test is an external test package and
// internal/wal does not import internal/store, so there is no cycle.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// originCrashApplier is the recovery-side Applier: it does what internal/hub's
// does, narrowed to messages — decode the durable record, stamp the DELIVERY
// POSITION from the WAL commit index, and fold it into the serving copy.
//
// Stamping Pos from CommitIndex rather than counting is the point: it is what
// makes a recovered message land at the position it committed under, so cursors
// survive a restart.
//
// A record that fails to DECODE is skipped rather than returned as an error.
// That mirrors Hub.Apply and invariant 6: recovery always reaches a running
// server, the discard is loud, and a refusal here would make Open fail — a bus
// held hostage by one bad record.
type originCrashApplier struct {
	s       *store.Store
	applied []string
	skipped []error
}

func (a *originCrashApplier) Apply(c wal.Committed) error {
	if c.Entry.Kind != store.RecordKind {
		return nil
	}
	m, err := store.Decode(c.Entry.Body)
	if err != nil {
		a.skipped = append(a.skipped, err)
		return nil
	}
	m.Pos = c.CommitIndex
	if err := a.s.Append(m); err != nil {
		return fmt.Errorf("applying %s at commit index %d: %w", m.ID, c.CommitIndex, err)
	}
	a.applied = append(a.applied, m.ID)
	return nil
}

// originCrashFixture is the accepted history every case starts from: two local
// sends, two relay ingests, and one NON-message entry in the middle so the
// commit indices the messages land on are not contiguous — a store that derived
// the position from a counter of its own would pass a contiguous fixture.
type originCrashFixture struct {
	dir string
	// msgs is the history in write order.
	msgs []store.Message
	// ackedAt[i] is the file size the instant msgs[i] became ACKNOWLEDGED: the
	// offset just past its COMMIT frame, where Write's fsync returned. Every
	// assertion is phrased against this rather than a record count, because
	// "acknowledged" is a fact about when fsync returned.
	ackedAt []int64
	// size is the full file size.
	size int64
	// sentAt is the clock the store must be run on so nothing prunes.
	sentAt time.Time
}

// originCrashOrigins are the ORIGIN ids of the two relay ingests: index 1 is the
// one that must SURVIVE, index 3 the one on the tail that must be DISCARDED.
const (
	originCrashSurvivingOrigin = originPeerBusID + "-31"
	originCrashTailOrigin      = originPeerBusID + "-77"
)

// buildOriginCrashHistory writes the fixture through the REAL two-phase WAL path
// and reports where each entry became acknowledged.
func buildOriginCrashHistory(t *testing.T) originCrashFixture {
	t.Helper()
	dir := t.TempDir()
	sentAt := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	alpha := agentIDFor(t, "alpha")
	beta := agentIDFor(t, "beta")
	peerAlpha := originAgentIDOn(t, originPeerBusID, "alpha")

	msgs := []store.Message{
		// [0] a LOCAL send — no origin key on disk at all.
		mkLocalMessageAt(t, alpha, []string{beta}, 1, 1, sentAt, "local one"),
		// [1] a RELAY INGEST — carries the correlation key. This is the one whose
		// origin id must survive the crash.
		mkRelayMessageAt(t, peerAlpha, []string{beta}, 2, 2, sentAt, "relayed one", originCrashSurvivingOrigin),
		// [2] a second LOCAL send.
		mkLocalMessageAt(t, alpha, []string{beta}, 3, 3, sentAt, "local two"),
		// [3] THE TAIL — a relay ingest that the crash cuts away.
		mkRelayMessageAt(t, peerAlpha, []string{beta}, 4, 4, sentAt, "relayed two", originCrashTailOrigin),
	}

	n := 0
	l, err := wal.Open(wal.LogOptions{Dir: dir, Now: func() time.Time {
		n++
		return sentAt.Add(time.Duration(n) * time.Second)
	}})
	if err != nil {
		t.Fatalf("building the accepted history: wal.Open: %v", err)
	}
	walPath := filepath.Join(dir, wal.WALFileName)

	ackedAt := make([]int64, len(msgs))
	for i, m := range msgs {
		if i == 2 {
			// A NON-MESSAGE entry sharing the log: roster records live here too,
			// and it exists so message commit indices are not contiguous.
			if _, err := l.Write(wal.Entry{Kind: "agent", Body: json.RawMessage(`{"note":"not a message"}`)}); err != nil {
				t.Fatalf("building the accepted history: writing the non-message entry: %v", err)
			}
		}
		raw, err := m.Encode()
		if err != nil {
			t.Fatalf("building the accepted history: Encode %s: %v", m.ID, err)
		}
		if _, err := l.Write(wal.Entry{Kind: store.RecordKind, Body: raw}); err != nil {
			t.Fatalf("building the accepted history: Write %s: %v", m.ID, err)
		}
		// Write has returned, so this entry's COMMIT record is fsynced: it is
		// ACKNOWLEDGED, and the file is exactly this long at that instant.
		ackedAt[i] = originCrashFileSize(t, walPath)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("building the accepted history: Close: %v", err)
	}

	f := originCrashFixture{dir: dir, msgs: msgs, ackedAt: ackedAt, size: originCrashFileSize(t, walPath), sentAt: sentAt}
	if f.ackedAt[2] >= f.size {
		t.Fatalf("the fixture has no tail to cut: entry 3 was acknowledged at %d and the file is %d bytes", f.ackedAt[2], f.size)
	}
	return f
}

func originCrashFileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// originCrashCopyDir copies a whole DATA DIRECTORY. The whole directory, not
// just the log: the MAC key that authenticates the bytes and the durable index
// floor are properties of the directory, and a log without them is not a crashed
// bus but a misconfigured one — a fixture that left them behind would be testing
// the wrong failure entirely.
func originCrashCopyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatalf("originCrashCopyDir: mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("originCrashCopyDir: read %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			originCrashCopyDir(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("originCrashCopyDir: read %s: %v", e.Name(), err)
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("originCrashCopyDir: stat %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, info.Mode().Perm()); err != nil {
			t.Fatalf("originCrashCopyDir: write %s: %v", e.Name(), err)
		}
	}
}

// originCrashRecover starts a store on dir exactly the way the server does —
// wal.Open replays the durable log into it before returning — and reports the
// store plus which message ids were applied.
func originCrashRecover(t *testing.T, dir string, sentAt time.Time) (*store.Store, []string) {
	t.Helper()
	app := &originCrashApplier{s: store.New(store.Options{Now: func() time.Time { return sentAt.Add(time.Minute) }})}
	l, err := wal.Open(wal.LogOptions{Dir: dir, Applier: app})
	if err != nil {
		t.Fatalf("recovery REFUSED to start on %s: %v\n"+
			"invariant 6: recovery always reaches a running server — damaged records are discarded and the bus starts. A bus held hostage by one bad sector is worse than a bus that has lost a message and said so", dir, err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("closing the recovered log: %v", err)
	}
	return app.s, app.applied
}

// TestStoreOriginLookupSurvivesACrashedTail is the crash-injection proof that
// the correlation key is genuinely DURABLE and that the two point-lookup indexes
// are rebuilt from the log rather than surviving in memory.
func TestStoreOriginLookupSurvivesACrashedTail(t *testing.T) {
	f := buildOriginCrashHistory(t)

	// THE CONTROL, and it runs first. Without it every assertion below could
	// hold against a recovery that resolved nothing at all: "the tail is not
	// resolvable" is trivially true in a store that is empty.
	t.Run("ControlTheUntruncatedLogRecoversEverything", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "pristine")
		originCrashCopyDir(t, f.dir, dir)
		s, applied := originCrashRecover(t, dir, f.sentAt)

		if len(applied) != len(f.msgs) {
			t.Fatalf("recovery applied %d messages, want %d: %v", len(applied), len(f.msgs), applied)
		}
		for _, m := range f.msgs {
			got, ok := s.ByID(m.ID)
			if !ok {
				t.Fatalf("ByID(%q) is NOT FOUND after recovering an INTACT log; nothing else in this test would then be meaningful", m.ID)
			}
			if got.OriginMessageID != m.OriginMessageID {
				t.Fatalf("recovery of %s lost the correlation key: OriginMessageID = %q, want %q", m.ID, got.OriginMessageID, m.OriginMessageID)
			}
		}
		// The TAIL record, specifically — the one every case below cuts away. If
		// it were not resolvable here, "not resolvable after the crash" would be
		// an unfailable assertion.
		tail := f.msgs[3]
		if got, ok := s.ByOriginMessageID(originCrashTailOrigin); !ok || got.ID != tail.ID {
			t.Fatalf("ByOriginMessageID(%q) = (%s, %v) after recovering an INTACT log, want (%s, true). Every truncation case below asserts this is ABSENT, and that assertion is only evidence if it is PRESENT here", originCrashTailOrigin, got.ID, ok, tail.ID)
		}
		// And the delivery positions came off the WAL commit index, so they are
		// not contiguous: the non-message entry in the middle burned indices.
		first, _ := s.ByID(f.msgs[0].ID)
		last, _ := s.ByID(f.msgs[3].ID)
		if last.Pos-first.Pos != 8 {
			t.Fatalf("recovered positions %d..%d: the gap is %d, want 8 (four two-record transactions plus the non-message entry). The position must be the WAL COMMIT INDEX, not a counter of the store's own — a counter restarts at 1 and silently renumbers every stored cursor",
				first.Pos, last.Pos, last.Pos-first.Pos)
		}
	})

	// THE SWEEP. Cut at EVERY byte offset inside the last entry's frames: from
	// the instant entry 3 was acknowledged up to one byte short of the end. Every
	// such file is what a power loss during the fourth append leaves behind, and
	// in every one of them entry 4's commit fsync had NOT returned — so entry 4
	// was never acknowledged and must not come back, while entries 1-3 were and
	// must.
	t.Run("EveryCutInsideTheTailEntryDiscardsItAndKeepsTheRest", func(t *testing.T) {
		parent := t.TempDir()
		survivors := f.msgs[:3]
		tail := f.msgs[3]

		for cut := f.ackedAt[2]; cut < f.size; cut++ {
			dir := filepath.Join(parent, fmt.Sprintf("cut-%06d", cut))
			originCrashCopyDir(t, f.dir, dir)
			originCrashTruncate(t, filepath.Join(dir, wal.WALFileName), cut)

			s, applied := originCrashRecover(t, dir, f.sentAt)

			// (1) INVARIANT 4 — nothing acknowledged is lost, and the correlation
			// key comes back with it.
			for _, m := range survivors {
				got, ok := s.ByID(m.ID)
				if !ok {
					t.Fatalf("cut at %d of %d: ByID(%q) is NOT FOUND. That entry's commit fsync had returned at offset %d, so it was ACKNOWLEDGED and must survive (invariant 4). Applied: %v",
						cut, f.size, m.ID, f.ackedAt[indexOfMessage(t, f.msgs, m.ID)], applied)
				}
				if got.OriginMessageID != m.OriginMessageID {
					t.Fatalf("cut at %d: %s recovered with OriginMessageID %q, want %q — the correlation key is durable or it is nothing", cut, m.ID, got.OriginMessageID, m.OriginMessageID)
				}
			}

			// (2) The RELAY ingest resolves by its ORIGIN id, intact. This is the
			// whole durability claim: relay.Forwarder.Resume runs only after a
			// restart, and this is the lookup it makes.
			got, ok := s.ByOriginMessageID(originCrashSurvivingOrigin)
			if !ok {
				t.Fatalf("cut at %d: ByOriginMessageID(%q) is NOT FOUND after recovery. The origin-id index is rebuilt from the durable record, and a relay job that cannot correlate its message is abandoned for nothing", cut, originCrashSurvivingOrigin)
			}
			if got.ID != f.msgs[1].ID {
				t.Fatalf("cut at %d: ByOriginMessageID(%q) resolved to %s, want %s", cut, originCrashSurvivingOrigin, got.ID, f.msgs[1].ID)
			}
			if got.OriginID() != originCrashSurvivingOrigin {
				t.Fatalf("cut at %d: OriginID() = %q after recovery, want %q", cut, got.OriginID(), originCrashSurvivingOrigin)
			}

			// (3) INVARIANT 10's single-hop egress case: a LOCALLY-ORIGINATED
			// message is still found through the FALLBACK after recovery, with no
			// durable state of its own.
			local := f.msgs[0]
			gotLocal, ok := s.ByOriginMessageID(local.ID)
			if !ok || gotLocal.ID != local.ID {
				t.Fatalf("cut at %d: ByOriginMessageID(%q) = (%s, %v) after recovery, want (%s, true). This bus is the origin, so its local id already IS the origin id — the fallback is what makes single-hop egress resumable across a restart with no new durable state", cut, local.ID, gotLocal.ID, ok, local.ID)
			}
			if gotLocal.OriginMessageID != "" {
				t.Fatalf("cut at %d: a locally-originated message recovered carrying OriginMessageID %q", cut, gotLocal.OriginMessageID)
			}

			// (4) INVARIANT 1 — THE NEGATIVE. The discarded tail must not be
			// re-resolvable by EITHER key. A lookup structure that outlived the
			// record it names would hand a relay forward a message the log says
			// was never accepted.
			if got, ok := s.ByID(tail.ID); ok {
				t.Fatalf("cut at %d of %d: ByID(%q) resolved to %s. That entry's commit fsync had NOT returned when the file ended (it completes at %d), so it was never acknowledged and must not be visible (invariant 5) — and invariant 1 forbids a discarded id ever being re-resolvable",
					cut, f.size, tail.ID, got.ID, f.ackedAt[3])
			}
			if got, ok := s.ByOriginMessageID(originCrashTailOrigin); ok {
				t.Fatalf("cut at %d of %d: ByOriginMessageID(%q) resolved to %s after the record carrying that origin id was DISCARDED", cut, f.size, originCrashTailOrigin, got.ID)
			}
			if got, ok := s.ByOriginMessageID(tail.ID); ok {
				t.Fatalf("cut at %d of %d: ByOriginMessageID(%q) resolved to %s through the FALLBACK after that record was DISCARDED", cut, f.size, tail.ID, got.ID)
			}

			if len(applied) != len(survivors) {
				t.Fatalf("cut at %d of %d: recovery applied %v, want exactly the %d acknowledged messages", cut, f.size, applied, len(survivors))
			}
		}
	})

	// THE OTHER SHAPE A CRASH LEAVES: a frame that reached the disk with a bad
	// byte in it.
	//
	// # The tail entry has TWO legitimate fates here, and both are asserted
	//
	// Damage inside the covered bytes fails the keyed MAC (invariant 6 —
	// integrity is an HMAC, never a CRC) and the record is DISCARDED, exactly as
	// a truncation discards it. But internal/wal also SALVAGES a record whose
	// LENGTH FIELD alone is corrupt: the tag is computed over the length and the
	// payload together, so a length that verifies against the bytes actually
	// present proves the record complete, and the log is rewritten with the
	// length repaired ("wal restored records whose length field was corrupt but
	// whose checksum proved them complete"). That entry is not damaged data, it
	// is intact data behind a damaged pointer, and discarding it would lose an
	// acknowledged write for nothing.
	//
	// So this sweep asserts the property that holds under BOTH fates — the one
	// that is actually about this task:
	//
	//	the tail entry is resolvable by its LOCAL id if and only if it is
	//	resolvable by its ORIGIN id, and when it is, the correlation key is
	//	EXACTLY what was written.
	//
	// A half-present entry — an id index naming a record the log discarded, or a
	// recovered message whose correlation key did not come back with it — is the
	// invariant-1 failure this file exists to catch, and it is a failure under
	// either fate. Both fates are required to OCCUR, so neither branch of the
	// assertion can quietly go unexercised.
	t.Run("EveryBitFlipInsideTheTailEntryLeavesItWhollyPresentOrWhollyAbsent", func(t *testing.T) {
		parent := t.TempDir()
		survivors := f.msgs[:3]
		tail := f.msgs[3]
		var discarded, salvaged int

		for _, off := range originCrashTailFlipOffsets(t, filepath.Join(f.dir, wal.WALFileName), f.ackedAt[2]) {
			dir := filepath.Join(parent, fmt.Sprintf("flip-%06d", off))
			originCrashCopyDir(t, f.dir, dir)
			originCrashFlipByte(t, filepath.Join(dir, wal.WALFileName), off)

			s, _ := originCrashRecover(t, dir, f.sentAt)

			// THE ANTI-CASCADE RULE. One flipped byte sits inside the LAST entry's
			// frames; every entry built out of other frames is untouched and must
			// come back, correlation key and all.
			for _, m := range survivors {
				got, ok := s.ByID(m.ID)
				if !ok {
					t.Fatalf("bit flip at %d of %d: ByID(%q) is NOT FOUND. The damaged byte is in the LAST entry's frames, so this ACKNOWLEDGED entry must survive (invariant 4)", off, f.size, m.ID)
				}
				if got.OriginMessageID != m.OriginMessageID {
					t.Fatalf("bit flip at %d: %s recovered with OriginMessageID %q, want %q", off, m.ID, got.OriginMessageID, m.OriginMessageID)
				}
			}
			if got, ok := s.ByOriginMessageID(originCrashSurvivingOrigin); !ok || got.ID != f.msgs[1].ID {
				t.Fatalf("bit flip at %d: ByOriginMessageID(%q) = (%s, %v), want (%s, true)", off, originCrashSurvivingOrigin, got.ID, ok, f.msgs[1].ID)
			}

			byLocal, okLocal := s.ByID(tail.ID)
			byOrigin, okOrigin := s.ByOriginMessageID(originCrashTailOrigin)
			if okLocal != okOrigin {
				t.Fatalf("bit flip at %d of %d: the tail entry is resolvable by its LOCAL id (%v) but not by its ORIGIN id (%v), or the reverse.\n"+
					"The two indexes are pure mirrors of one serving copy: a record is either recovered with its correlation key or it is not recovered at all. A discarded id that is still resolvable — or a recovered message whose origin id did not come back — is the invariant-1 failure",
					off, f.size, okLocal, okOrigin)
			}
			if !okLocal {
				discarded++
				// A discarded record must not be reachable through the FALLBACK
				// either: the fallback goes through the same id index, so a stale
				// entry would show up here too.
				if got, ok := s.ByOriginMessageID(tail.ID); ok {
					t.Fatalf("bit flip at %d of %d: the tail entry was DISCARDED, yet ByOriginMessageID(%q) resolved to %s through the fallback", off, f.size, tail.ID, got.ID)
				}
				continue
			}
			salvaged++
			if byOrigin.ID != tail.ID {
				t.Fatalf("bit flip at %d: ByOriginMessageID(%q) resolved to %s, want %s", off, originCrashTailOrigin, byOrigin.ID, tail.ID)
			}
			if byLocal.OriginMessageID != originCrashTailOrigin {
				t.Fatalf("bit flip at %d of %d: the tail entry was RECOVERED but its correlation key came back as %q, want %q.\n"+
					"A length-field repair rewrites the frame around the SAME payload; if the origin id does not survive that, the field is not durable at all", off, f.size, byLocal.OriginMessageID, originCrashTailOrigin)
			}
			if !bytesEqualString(byLocal.Body, "relayed two") {
				t.Fatalf("bit flip at %d: the recovered tail entry carries body %q, want %q", off, byLocal.Body, "relayed two")
			}
		}

		// BOTH FATES MUST OCCUR, or one half of the assertion above was never
		// evaluated and this sweep would pass for the wrong reason.
		if discarded == 0 {
			t.Fatalf("no bit flip in the tail entry's %d bytes caused it to be DISCARDED; the invariant-1 negative was never exercised", f.size-f.ackedAt[2])
		}
		if salvaged == 0 {
			t.Fatalf("no bit flip in the tail entry's %d bytes was SALVAGED; the length-field repair path — and with it the assertion that the correlation key survives a frame rewrite — was never exercised", f.size-f.ackedAt[2])
		}
	})
}

// originCrashTailFlipOffsets names the byte offsets the bit-flip sweep damages:
// every offset in the frame HEADER of each frame at or past from — the length
// field, the index, the type, the reserved bytes and the whole keyed MAC — plus
// the ends and the middle of each payload.
//
// It is STRUCTURAL rather than exhaustive on purpose. internal/wal already
// sweeps every byte of the whole file (TestCrashInjectionSingleBitCorruptionSweep)
// and owns that quantifier; what this file needs is that each distinct REGION of
// a frame is damaged at least once, because each drives a different recovery
// path — a corrupt MAC discards, a corrupt length field is salvaged, and both
// fates must be observed for the assertion around this loop to mean anything.
// The exhaustive quantifier is kept where it belongs for THIS task, on the
// truncation sweep, which is the invariant-4 and invariant-5 evidence.
func originCrashTailFlipOffsets(t *testing.T, path string, from int64) []int64 {
	t.Helper()
	recs, _, err := wal.ScanAll(path, wal.KindWAL)
	if err != nil {
		t.Fatalf("scanning the pristine log for its frame boundaries: %v", err)
	}
	// The version 2 frame header, from internal/wal/format.go:
	//
	//	[0:4]   payloadLen   [4:12]  index
	//	[12:14] type         [14:16] reserved (always 0)
	//	[16:48] HMAC-SHA256(key, frame[0:16] ++ payload)
	//
	// Every one of those regions is inside the covered bytes, so a flip in any of
	// them is DETECTED — which is the whole reason the length field is covered
	// (a crafted length is the length-inflation class of attack) and the reason
	// integrity here is a keyed MAC rather than a CRC (invariant 6).
	headerOffsets := []int64{
		0, 1, 2, 3, // payloadLen — the one region wal can SALVAGE
		4, 8, 11, // index
		12, 13, // type
		14, 15, // reserved
		16, 17, 31, 46, 47, // the MAC itself
	}
	var offs []int64
	frames := 0
	for _, r := range recs {
		if r.Offset < from {
			continue
		}
		frames++
		for _, i := range headerOffsets {
			offs = append(offs, r.Offset+i)
		}
		body := r.Offset + wal.FrameHeaderSize
		n := int64(len(r.Payload))
		for _, rel := range []int64{0, 1, n / 2, n - 2, n - 1} {
			if rel >= 0 && rel < n {
				offs = append(offs, body+rel)
			}
		}
	}
	if frames != 2 {
		t.Fatalf("expected exactly two frames (prepare and commit) at or past offset %d, found %d", from, frames)
	}
	return offs
}

// bytesEqualString compares a recovered body with the literal that was written.
func bytesEqualString(b []byte, s string) bool { return string(b) == s }

// indexOfMessage reports where id sits in msgs, for an error message that can
// name the offset at which the entry became acknowledged.
func indexOfMessage(t *testing.T, msgs []store.Message, id string) int {
	t.Helper()
	for i, m := range msgs {
		if m.ID == id {
			return i
		}
	}
	t.Fatalf("indexOfMessage: %q is not in the fixture", id)
	return -1
}

// originCrashTruncate cuts path to size — the file a power loss mid-append
// leaves behind.
func originCrashTruncate(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("truncating %s to %d: %v", path, size, err)
	}
}

// originCrashFlipByte flips the low bit of one byte — the other shape damage
// takes, and the one a CRC could be forged around and a keyed MAC cannot.
func originCrashFlipByte(t *testing.T, path string, off int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if off < 0 || off >= int64(len(b)) {
		t.Fatalf("flip offset %d is outside the %d-byte file", off, len(b))
	}
	b[off] ^= 0x01
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
