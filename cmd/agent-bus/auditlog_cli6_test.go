package main

// CLI-6 -- `agent-bus log`, the OFFLINE reader for the append-only message
// audit trail (bus.audit).
//
// Every test here is named TestCLILog... on purpose. The task's registered proof
// is `go test -race -run 'TestCLILog' ./cmd/agent-bus/...`, and a -run pattern
// that matches nothing prints "ok ... [no tests to run]" and EXITS 0 -- the
// vacuous pass this repo has been bitten by. The naming is what makes that proof
// non-vacuous.
//
// The properties under test, in the order a reviewer should check them:
//
//	1. bus_path is PRESENT and ORDERED on every record, and never null
//	2. INVARIANT 6: no message body can reach this command's output -- asserted
//	   both by sentinel and, structurally, by an exact output key allow-list
//	3. damage is LOUD: named on stderr with path/offset/reason, emitted as a
//	   {"damaged":true} object under --json, exit 1, and NEVER suppressed by a
//	   filter
//	4. "no trail" (exit 4) and "empty trail" (exit 1) are different answers
//	5. a locked data directory is exit 3 and names the remedy
//	6. the filters select what they say they select, at their boundaries
//	7. usage errors are exit 2 and -h is exit 0
//	8. A TRAIL THIS READER CANNOT AUTHENTICATE IS REFUSED (exit 5), NOT PRINTED
//	   -- no wal-mac.key, or a bus.audit that is not on-disk format version 2 --
//	   and the refusal MINTS NO KEY
//	9. EVERY CLIENT-DERIVED STRING IS ESCAPED in the human rendering: no raw ESC
//	   and no raw CR reach the terminal, and no forged record line can be
//	   authored by the subject of the audit
//	10. "the trail is ABSENT" (exit 4) and "the trail could NOT BE EXAMINED"
//	    (exit 1) are different answers to different stat errors
//
// FIXTURES ARE WRITTEN THROUGH internal/wal DIRECTLY, not through a running bus.
// That is deliberate: hub.publish has only two callers (Send, Broadcast) and
// neither sets busPath, so no production path can currently produce a MULTI-HOP
// bus_path -- and a multi-hop, order-sensitive path is the thing this task
// exists to expose. Writing the trail through wal.Log.Write is the same durable
// path the server uses; only the caller of it differs.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// cli6Base is a fixed clock so -since/-until boundaries are exact rather than
// approximately right.
var cli6Base = time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)

// cli6SHA builds a valid content hash: 64 lowercase hex characters, which is
// what wal's writer AND its decoder both insist on.
func cli6SHA(n byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", n), 32)
}

// cli6Fixture is one message to write into the trail: the audit metadata, plus
// the BODY that rides in the WAL beside it. The body exists so the invariant-6
// test has something real to look for.
type cli6Fixture struct {
	audit *wal.AuditRecord
	body  string
}

// cli6Trail is the standard three-record fixture, and it is shaped to carry
// every property the filters and the bus_path assertions need:
//
//	seq 1  alice -> bob                 1 hop   busA
//	seq 2  bob   -> alice, carol        2 hops  busA, busB
//	seq 3  alice -> BROADCAST           3 hops  busA, busB, busC
//
// The hop counts ascend so that a reader which sorted, reversed or deduped the
// path would produce a visibly different answer on at least one record.
func cli6Trail() []cli6Fixture {
	return []cli6Fixture{
		{
			audit: &wal.AuditRecord{
				MessageID:     "busA.msg-1",
				Seq:           1,
				Sender:        "busA.alice",
				Recipients:    []string{"busA.bob"},
				BusPath:       []string{"busA"},
				SentAt:        cli6Base,
				Size:          11,
				ContentSHA256: cli6SHA(0xa1),
			},
			body: `{"text":"one"}`,
		},
		{
			audit: &wal.AuditRecord{
				MessageID:     "busA.msg-2",
				Seq:           2,
				Sender:        "busA.bob",
				Recipients:    []string{"busA.alice", "busB.carol"},
				BusPath:       []string{"busA", "busB"},
				SentAt:        cli6Base.Add(60 * time.Second),
				Size:          22,
				ContentSHA256: cli6SHA(0xb2),
			},
			body: `{"text":"two"}`,
		},
		{
			audit: &wal.AuditRecord{
				MessageID:     "busA.msg-3",
				Seq:           3,
				Sender:        "busA.alice",
				Broadcast:     true,
				BusPath:       []string{"busA", "busB", "busC"},
				SentAt:        cli6Base.Add(120 * time.Second),
				Size:          33,
				ContentSHA256: cli6SHA(0xc3),
			},
			body: `{"text":"three"}`,
		},
	}
}

// cli6WriteTrail writes the fixtures into a fresh data directory through the
// real durable path (prepare-fsync -> audit-fsync -> commit-fsync) and returns
// the directory.
func cli6WriteTrail(t *testing.T, fixtures []cli6Fixture) string {
	t.Helper()
	dir := t.TempDir()
	cli6AppendTrail(t, dir, fixtures)
	return dir
}

func cli6AppendTrail(t *testing.T, dir string, fixtures []cli6Fixture) {
	t.Helper()
	l, err := wal.Open(wal.LogOptions{Dir: dir, Now: func() time.Time { return cli6Base }})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	for i, f := range fixtures {
		if _, err := l.Write(wal.Entry{Kind: "message", Body: json.RawMessage(f.body), Audit: f.audit}); err != nil {
			l.Close()
			t.Fatalf("wal.Write(fixture %d): %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("wal.Log.Close: %v", err)
	}
}

// cli6AuditPath is <dir>/bus.audit.
func cli6AuditPath(dir string) string { return filepath.Join(dir, wal.AuditFileName) }

// cli6Frames returns the framing facts about the trail on disk: each record's
// index, type and byte offset. It is how the damage tests know WHERE to cut.
func cli6Frames(t *testing.T, dir string) []wal.Record {
	t.Helper()
	recs, _, err := wal.ScanAll(cli6AuditPath(dir), wal.KindAudit)
	if err != nil {
		t.Fatalf("ScanAll(%s): %v", cli6AuditPath(dir), err)
	}
	return recs
}

// cli6Run invokes the command in process. runLogCommand returns an exit code and
// never calls os.Exit, which is the seam this whole file relies on.
func cli6Run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runLogCommand(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// cli6JSONLines splits NDJSON stdout into decoded objects, preserving order.
func cli6JSONLines(t *testing.T, stdout string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// cli6RecordObjects returns only the record objects (everything that is not a
// damage or failure object), so a test can assert on records without having to
// know how many damage lines preceded or followed them.
func cli6RecordObjects(objs []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, o := range objs {
		if _, damaged := o["damaged"]; damaged {
			continue
		}
		if _, failed := o["ok"]; failed {
			continue
		}
		out = append(out, o)
	}
	return out
}

func cli6DamageObjects(objs []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, o := range objs {
		if _, damaged := o["damaged"]; damaged {
			out = append(out, o)
		}
	}
	return out
}

// cli6Strings converts a decoded JSON array of strings.
func cli6Strings(t *testing.T, v interface{}, field string) []string {
	t.Helper()
	arr, ok := v.([]interface{})
	if !ok {
		t.Fatalf("%s is %T, want a JSON array (it must never be null or absent)", field, v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s contains %T, want a string", field, e)
		}
		out = append(out, s)
	}
	return out
}

// cli6Seqs pulls the seq of every record object, in emitted order.
func cli6Seqs(t *testing.T, stdout string) []uint64 {
	t.Helper()
	var seqs []uint64
	for _, o := range cli6RecordObjects(cli6JSONLines(t, stdout)) {
		f, ok := o["seq"].(float64)
		if !ok {
			t.Fatalf("record object has seq %T, want a number: %v", o["seq"], o)
		}
		seqs = append(seqs, uint64(f))
	}
	return seqs
}

// ---------------------------------------------------------------------------
// 1. bus_path is present and ORDERED on every record
// ---------------------------------------------------------------------------

// TestCLILogJSONBusPathIsOrderedOnEveryRecord is the assertion that unblocks
// scripts/fed-smoke.sh: the ordered traversal is exposed per record, exactly as
// wal read it -- not sorted, not reversed, not deduped -- and bus_path and
// recipients are ALWAYS present and never JSON null.
func TestCLILogJSONBusPathIsOrderedOnEveryRecord(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
	objs := cli6RecordObjects(cli6JSONLines(t, stdout))
	if len(objs) != 3 {
		t.Fatalf("got %d record objects, want 3: %s", len(objs), stdout)
	}

	wantPaths := [][]string{
		{"busA"},
		{"busA", "busB"},
		{"busA", "busB", "busC"},
	}
	for i, o := range objs {
		// PRESENCE is asserted separately from value: a missing key and a null
		// value both decode to nil, and both would be a silent regression.
		raw, ok := o["bus_path"]
		if !ok {
			t.Fatalf("record %d has no bus_path key at all: %v", i, o)
		}
		if raw == nil {
			t.Fatalf("record %d has bus_path null; it must be an array, [] when empty: %v", i, o)
		}
		got := cli6Strings(t, raw, "bus_path")
		if len(got) != len(wantPaths[i]) {
			t.Fatalf("record %d bus_path = %v, want %v", i, got, wantPaths[i])
		}
		for j := range got {
			if got[j] != wantPaths[i][j] {
				t.Fatalf("record %d bus_path = %v, want EXACTLY %v (order is the point: no sorting, reversing or deduping)",
					i, got, wantPaths[i])
			}
		}
		rawR, ok := o["recipients"]
		if !ok {
			t.Fatalf("record %d has no recipients key at all: %v", i, o)
		}
		if rawR == nil {
			t.Fatalf("record %d has recipients null; a broadcast must emit [] not null: %v", i, o)
		}
	}

	// The BROADCAST is the record that would most plausibly emit null: wal
	// stores no recipient list for it at all.
	broadcast := objs[2]
	if got := cli6Strings(t, broadcast["recipients"], "recipients"); len(got) != 0 {
		t.Fatalf("broadcast recipients = %v, want []", got)
	}
	if b, ok := broadcast["broadcast"].(bool); !ok || !b {
		t.Fatalf("record 3 broadcast = %v, want true", broadcast["broadcast"])
	}
	if !strings.Contains(stdout, `"recipients":[]`) {
		t.Fatalf("broadcast did not serialise recipients as []: %s", stdout)
	}
}

// TestCLILogHumanBusPathIsOrdered pins the same ordering in the human rendering,
// which is a separate code path (renderBusPath) and could drift from --json.
//
// THE QUOTING IS DELIBERATE (HIGH-3) -- DO NOT "FIX" IT BACK. Each element is
// rendered with %q, PER ELEMENT, so the expected strings are
//
//	bus path: "busA" -> "busB"
//
// and not the bare `busA -> busB` this test originally pinned. wal's auditID
// bounds a bus id on emptiness and length only (internal/wal/audit.go) and
// imposes NO character restriction, so a bus id is client-derived text that may
// contain a newline, a CR, an ANSI escape -- or the four ordinary characters
// " -> " that separate the hops. The security gate got a COMPLETE FABRICATED
// RECORD LINE into this output through an unquoted `sender`, and got ESC[2J
// through to a terminal unaltered; quoting per element is that fix, and quoting
// the JOINED string instead would still let one element masquerade as two. If
// this assertion ever goes red because the quotes vanished, the defect is in
// writeAuditHuman/renderBusPath, not here -- see
// TestCLILogHumanEscapesTerminalControlBytes.
//
// The property under test is UNCHANGED: the traversal is exposed first-bus-first
// as exactly busA, then busA->busB, then busA->busB->busC -- not sorted, not
// reversed, not deduped -- and the three records appear in that order.
func TestCLILogHumanBusPathIsOrdered(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
	wants := []string{
		"bus path: \"busA\"\n",
		"bus path: \"busA\" -> \"busB\"\n",
		"bus path: \"busA\" -> \"busB\" -> \"busC\"\n",
	}
	at := make([]int, len(wants))
	for i, want := range wants {
		at[i] = strings.Index(stdout, want)
		if at[i] < 0 {
			t.Fatalf("human output is missing %q (each hop is %%q-quoted PER ELEMENT -- that is the HIGH-3 fix, not a regression):\n%s",
				want, stdout)
		}
	}
	// ...and the RECORDS themselves are in file order, so a reader that
	// reordered records would not pass merely by rendering each path correctly.
	for i := 1; i < len(at); i++ {
		if at[i] <= at[i-1] {
			t.Fatalf("bus paths are not rendered in record order (%q at %d appears before %q at %d):\n%s",
				wants[i], at[i], wants[i-1], at[i-1], stdout)
		}
	}
	if !strings.Contains(stdout, "3 record(s) shown.") {
		t.Fatalf("human output does not report the record count:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// 2. INVARIANT 6 -- no body can leak
// ---------------------------------------------------------------------------

// cli6BodySentinel is long and recognisable so that a partial leak is still a
// hit.
const cli6BodySentinel = "SUPERSECRET-BODY-BYTES-DO-NOT-LEAK"

// TestCLILogNeverPrintsAMessageBody is the highest-value test in this file.
//
// It writes a message whose BODY is a sentinel through the same durable
// transaction that writes the audit record, proves the sentinel really did reach
// the data directory (otherwise the test would pass vacuously against a bus that
// never stored anything), and then asserts the sentinel appears NOWHERE in
// stdout or stderr in either output mode.
func TestCLILogNeverPrintsAMessageBody(t *testing.T) {
	t.Parallel()
	fixtures := cli6Trail()
	for i := range fixtures {
		fixtures[i].body = fmt.Sprintf(`{"text":%q}`, cli6BodySentinel+"-"+fixtures[i].audit.MessageID)
	}
	dir := cli6WriteTrail(t, fixtures)

	// PROOF THAT THE TEST IS NOT VACUOUS: the sentinel is genuinely on disk in
	// this data directory (in the WAL), so "it did not appear in the output" is
	// a statement about the command and not about an empty fixture.
	walBytes, err := os.ReadFile(filepath.Join(dir, "bus.wal"))
	if err != nil {
		t.Fatalf("reading the WAL: %v", err)
	}
	if !bytes.Contains(walBytes, []byte(cli6BodySentinel)) {
		t.Fatalf("the sentinel body never reached the data directory, so this test proves nothing")
	}
	// And it is NOT in the audit trail either -- invariant 6 at the storage
	// layer, restated here because a leak could originate in wal rather than in
	// this command.
	auditBytes, err := os.ReadFile(cli6AuditPath(dir))
	if err != nil {
		t.Fatalf("reading the audit trail: %v", err)
	}
	if bytes.Contains(auditBytes, []byte(cli6BodySentinel)) {
		t.Fatalf("bus.audit itself contains a message body; invariant 6 is violated below this command")
	}

	for _, mode := range []struct {
		name string
		args []string
	}{
		{"human", []string{"-data-dir", dir}},
		{"json", []string{"-data-dir", dir, "-json"}},
	} {
		code, stdout, stderr := cli6Run(t, mode.args...)
		if code != exitLogOK {
			t.Fatalf("%s: exit = %d, want %d (stderr: %s)", mode.name, code, exitLogOK, stderr)
		}
		if strings.Contains(stdout, cli6BodySentinel) {
			t.Fatalf("%s: A MESSAGE BODY LEAKED TO STDOUT (invariant 6):\n%s", mode.name, stdout)
		}
		if strings.Contains(stderr, cli6BodySentinel) {
			t.Fatalf("%s: A MESSAGE BODY LEAKED TO STDERR (invariant 6):\n%s", mode.name, stderr)
		}
	}
}

// cli6AllowedRecordKeys is the EXACT set of keys a --json record object may
// carry. It is an allow-list, not a subset check, and that is the point:
//
// a sentinel test only ever catches the leak it was told about. Asserting the
// key set EXACTLY means that ANY future field added to the output -- including a
// "raw", "payload" or catch-all frame dump, which is precisely how a body would
// re-enter this command -- turns this test RED on the commit that adds it,
// whether or not anyone thought to extend the sentinel.
var cli6AllowedRecordKeys = []string{
	"audit_index",
	"broadcast",
	"bus_path",
	"content_sha256",
	"message_id",
	"offset",
	"prepare_index",
	"recipients",
	"seq",
	"sender",
	"sent_at",
	"size",
}

// TestCLILogJSONRecordKeysAreExactlyTheAllowList states invariant 6 as a
// STRUCTURAL property of the output rather than as a search for known-bad bytes.
func TestCLILogJSONRecordKeysAreExactlyTheAllowList(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
	objs := cli6RecordObjects(cli6JSONLines(t, stdout))
	if len(objs) == 0 {
		t.Fatal("no record objects were emitted, so the key allow-list proves nothing")
	}
	want := append([]string(nil), cli6AllowedRecordKeys...)
	sort.Strings(want)
	for i, o := range objs {
		got := make([]string, 0, len(o))
		for k := range o {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("record %d key set =\n  %v\nwant EXACTLY\n  %v\n"+
				"A NEW KEY HERE IS NOT AUTOMATICALLY SAFE: this command must never gain a raw/catch-all "+
				"field, because that is how a message body re-enters the audit output (invariant 6). "+
				"If the new field is genuinely metadata, add it to cli6AllowedRecordKeys deliberately.",
				i, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Damage is LOUD
// ---------------------------------------------------------------------------

// TestCLILogTruncatedTrailIsReportedLoudly cuts the file in the MIDDLE of the
// last record's frame. Everything before the cut must still print, the damage
// must be named on stderr with path, offset and reason, --json must emit a final
// {"damaged":true,...} object, and the exit must be 1.
func TestCLILogTruncatedTrailIsReportedLoudly(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())
	frames := cli6Frames(t, dir)
	if len(frames) != 3 {
		t.Fatalf("fixture wrote %d audit frames, want 3", len(frames))
	}
	// Mid-frame: past record 3's frame header, part-way through its payload.
	cut := frames[2].Offset + wal.FrameHeaderSize + 2
	path := cli6AuditPath(dir)
	if err := os.Truncate(path, cut); err != nil {
		t.Fatalf("truncating %s to %d: %v", path, cut, err)
	}

	t.Run("human", func(t *testing.T) {
		code, stdout, stderr := cli6Run(t, "-data-dir", dir)
		if code != exitLogDamaged {
			t.Fatalf("exit = %d, want %d (damage must not be exit 0)", code, exitLogDamaged)
		}
		// The records BEFORE the damage are still printed.
		for _, id := range []string{"busA.msg-1", "busA.msg-2"} {
			if !strings.Contains(stdout, id) {
				t.Fatalf("record %s before the damage was not printed:\n%s", id, stdout)
			}
		}
		if strings.Contains(stdout, "busA.msg-3") {
			t.Fatalf("the truncated record was printed anyway:\n%s", stdout)
		}
		// The damage is named on STDERR, with path, offset and reason.
		if !strings.Contains(stderr, "DAMAGED") {
			t.Fatalf("stderr does not announce damage:\n%s", stderr)
		}
		if !strings.Contains(stderr, path) {
			t.Fatalf("stderr does not name the damaged path %q:\n%s", path, stderr)
		}
		if !strings.Contains(stderr, fmt.Sprintf("byte offset %d", frames[2].Offset)) {
			t.Fatalf("stderr does not name the byte offset %d of the damage:\n%s", frames[2].Offset, stderr)
		}
		if !strings.Contains(stderr, "truncated") {
			t.Fatalf("stderr does not give a reason mentioning truncation:\n%s", stderr)
		}
		if !strings.Contains(stdout, "THE TRAIL IS DAMAGED") {
			t.Fatalf("the human footer does not say the trail is damaged:\n%s", stdout)
		}
	})

	t.Run("json", func(t *testing.T) {
		code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
		if code != exitLogDamaged {
			t.Fatalf("exit = %d, want %d", code, exitLogDamaged)
		}
		objs := cli6JSONLines(t, stdout)
		if len(objs) < 3 {
			t.Fatalf("want 2 records plus a damage object, got %d lines:\n%s", len(objs), stdout)
		}
		if got := len(cli6RecordObjects(objs)); got != 2 {
			t.Fatalf("got %d record objects, want 2 (the readable prefix):\n%s", got, stdout)
		}
		dmg := cli6DamageObjects(objs)
		if len(dmg) != 1 {
			t.Fatalf("got %d damage objects, want exactly 1:\n%s", len(dmg), stdout)
		}
		// THE FINAL LINE is the damage object: a consumer streaming the trail
		// must see every readable record before it learns the trail ended badly.
		if _, isDamage := objs[len(objs)-1]["damaged"]; !isDamage {
			t.Fatalf("the damage object is not the last line:\n%s", stdout)
		}
		// NO message_id. scripts/fed-smoke.sh counts objects by message id and
		// requires exactly one per message; a damage object carrying one would be
		// counted as a delivery.
		if _, bad := dmg[0]["message_id"]; bad {
			t.Fatalf("the damage object carries a message_id key, which would corrupt fed-smoke's per-message count: %v", dmg[0])
		}
		if d, _ := dmg[0]["damaged"].(bool); !d {
			t.Fatalf("damage object has damaged = %v, want true: %v", dmg[0]["damaged"], dmg[0])
		}
		if p, _ := dmg[0]["path"].(string); p != path {
			t.Fatalf("damage object path = %q, want %q", p, path)
		}
		if off, _ := dmg[0]["offset"].(float64); int64(off) != frames[2].Offset {
			t.Fatalf("damage object offset = %v, want %d", dmg[0]["offset"], frames[2].Offset)
		}
		if r, _ := dmg[0]["reason"].(string); r == "" {
			t.Fatalf("damage object has no reason: %v", dmg[0])
		}
		// stderr is written even in --json mode: a human watching the terminal
		// and a script parsing stdout must both learn about a discard.
		if !strings.Contains(stderr, "DAMAGED") {
			t.Fatalf("--json suppressed the stderr damage report:\n%s", stderr)
		}
	})
}

// TestCLILogMACFailureIsReportedLoudly flips a byte inside a frame's payload so
// the keyed MAC no longer verifies. Integrity here is a MAC, never a CRC
// (invariant 6), and a frame that does not verify must be reported, not skipped.
func TestCLILogMACFailureIsReportedLoudly(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())
	frames := cli6Frames(t, dir)
	path := cli6AuditPath(dir)

	// One byte, inside record 2's payload.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	corruptAt := frames[1].Offset + wal.FrameHeaderSize + 3
	var one [1]byte
	if _, err := f.ReadAt(one[:], corruptAt); err != nil {
		f.Close()
		t.Fatalf("reading byte %d: %v", corruptAt, err)
	}
	one[0] ^= 0xff
	if _, err := f.WriteAt(one[:], corruptAt); err != nil {
		f.Close()
		t.Fatalf("writing byte %d: %v", corruptAt, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
	if code != exitLogDamaged {
		t.Fatalf("exit = %d, want %d (stdout: %s)(stderr: %s)", code, exitLogDamaged, stdout, stderr)
	}
	if !strings.Contains(stderr, "DAMAGED") {
		t.Fatalf("stderr does not announce the MAC failure:\n%s", stderr)
	}
	if !strings.Contains(stderr, "checksum mismatch") {
		t.Fatalf("stderr does not give the MAC-verification reason:\n%s", stderr)
	}
	if !strings.Contains(stderr, fmt.Sprintf("byte offset %d", frames[1].Offset)) {
		t.Fatalf("stderr does not name the offset %d of the bad frame:\n%s", frames[1].Offset, stderr)
	}
	// The record before the damage still printed; the damaged one did not.
	recs := cli6RecordObjects(cli6JSONLines(t, stdout))
	if len(recs) != 1 {
		t.Fatalf("got %d record objects, want 1 (only the prefix before the bad frame):\n%s", len(recs), stdout)
	}
	if id, _ := recs[0]["message_id"].(string); id != "busA.msg-1" {
		t.Fatalf("surviving record is %q, want busA.msg-1", id)
	}
	dmg := cli6DamageObjects(cli6JSONLines(t, stdout))
	if len(dmg) != 1 {
		t.Fatalf("got %d damage objects, want 1:\n%s", len(dmg), stdout)
	}
	if _, bad := dmg[0]["message_id"]; bad {
		t.Fatalf("the damage object carries a message_id key: %v", dmg[0])
	}
}

// cli6RawAppend appends a frame to bus.audit directly, bypassing the audit
// writer's own record shaping. It is how a WRONG-TYPE record gets into the
// middle of an otherwise healthy trail -- something no production path produces,
// which is exactly why the reader's response to it needs pinning.
func cli6RawAppend(t *testing.T, dir string, typ wal.Type, payload []byte) {
	t.Helper()
	w, err := wal.OpenWriter(cli6AuditPath(dir), wal.KindAudit)
	if err != nil {
		t.Fatalf("wal.OpenWriter(audit): %v", err)
	}
	if _, err := w.Append(typ, payload); err != nil {
		w.Close()
		t.Fatalf("Append(%s): %v", typ, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the audit writer: %v", err)
	}
}

// cli6AuditPayload renders a well-formed on-disk audit payload, so a raw append
// can produce a record the reader accepts. Field names are wal's auditPayload
// JSON tags.
func cli6AuditPayload(messageID string, seq uint64, sender string, busPath []string, sentAt time.Time) []byte {
	return []byte(fmt.Sprintf(
		`{"message_id":%q,"seq":%d,"sender":%q,"broadcast":false,"recipients":["busA.bob"],`+
			`"bus_path":[%q],"sent_at":%q,"size":7,"content_sha256":%q,"prepare_index":99}`,
		messageID, seq, sender, busPath[0], sentAt.UTC().Format(time.RFC3339Nano), cli6SHA(0xd4)))
}

// TestCLILogWrongRecordTypeIsReportedAndReadContinues puts a record that is NOT
// a TypeAuditMessage between two good ones.
//
// Two properties, and the second is the one a naive implementation gets wrong:
// the unexpected record is reported LOUDLY (never skipped silently -- a reader
// that skipped it would report a clean trail over a file that is not what it
// claims to be), AND the read CONTINUES to the records after it rather than
// aborting at the first surprise.
func TestCLILogWrongRecordTypeIsReportedAndReadContinues(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail()[:1]) // one good record: busA.msg-1

	// A PREPARE record has no business in the message audit trail.
	cli6RawAppend(t, dir, wal.TypePrepare, []byte(`{"kind":"message"}`))
	// ...and a perfectly good audit record AFTER it, so "the read continues"
	// is observable rather than assumed.
	cli6RawAppend(t, dir, wal.TypeAuditMessage,
		cli6AuditPayload("busA.msg-after", 9, "busA.alice", []string{"busA"}, cli6Base.Add(300*time.Second)))

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
	if code != exitLogDamaged {
		t.Fatalf("exit = %d, want %d (stdout: %s)(stderr: %s)", code, exitLogDamaged, stdout, stderr)
	}
	if !strings.Contains(stderr, "DAMAGED") {
		t.Fatalf("the wrong-type record was not announced on stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "audit index 2") {
		t.Fatalf("stderr does not name the offending record's audit index:\n%s", stderr)
	}
	recs := cli6RecordObjects(cli6JSONLines(t, stdout))
	if len(recs) != 2 {
		t.Fatalf("got %d record objects, want 2 (the read must CONTINUE past the bad record):\n%s", len(recs), stdout)
	}
	gotIDs := []string{}
	for _, r := range recs {
		id, _ := r["message_id"].(string)
		gotIDs = append(gotIDs, id)
	}
	if gotIDs[0] != "busA.msg-1" || gotIDs[1] != "busA.msg-after" {
		t.Fatalf("message ids = %v, want [busA.msg-1 busA.msg-after]", gotIDs)
	}
	dmg := cli6DamageObjects(cli6JSONLines(t, stdout))
	if len(dmg) != 1 {
		t.Fatalf("got %d damage objects, want 1:\n%s", len(dmg), stdout)
	}
	if _, bad := dmg[0]["message_id"]; bad {
		t.Fatalf("the damage object carries a message_id key: %v", dmg[0])
	}
	if idx, _ := dmg[0]["audit_index"].(float64); uint64(idx) != 2 {
		t.Fatalf("damage object audit_index = %v, want 2", dmg[0]["audit_index"])
	}
}

// TestCLILogFiltersNeverSuppressDamage is invariant 6 against the most plausible
// way to accidentally silence this command: a filter that matches nothing must
// still report the damage and must still exit 1. If a filter could hide
// corruption, silence from this command would mean nothing at all.
func TestCLILogFiltersNeverSuppressDamage(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())
	frames := cli6Frames(t, dir)
	if err := os.Truncate(cli6AuditPath(dir), frames[2].Offset+wal.FrameHeaderSize+2); err != nil {
		t.Fatalf("truncating: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"sender-matches-nothing", []string{"-sender", "nobody.at.all"}},
		{"recipient-matches-nothing", []string{"-recipient", "nobody.at.all"}},
		{"seq-window-matches-nothing", []string{"-min-seq", "900", "-max-seq", "999"}},
		{"json-sender-matches-nothing", []string{"-json", "-sender", "nobody.at.all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-data-dir", dir}, tc.args...)
			code, stdout, stderr := cli6Run(t, args...)
			if code != exitLogDamaged {
				t.Fatalf("exit = %d, want %d: a filter must not turn a damaged trail into a clean one", code, exitLogDamaged)
			}
			if !strings.Contains(stderr, "DAMAGED") {
				t.Fatalf("the filter suppressed the damage report:\n%s", stderr)
			}
			if strings.Contains(stdout, "busA.msg-1") {
				t.Fatalf("a filter matching nothing still emitted a record:\n%s", stdout)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. No trail (exit 4) vs empty trail (exit 1)
// ---------------------------------------------------------------------------

// TestCLILogMissingTrailIsExit4AndLeavesNoLock: "there is no trail" and "the
// trail is broken" must never be reported as the same thing, and a refusal must
// not leave a bus.lock behind in a directory the operator mistyped -- a lone
// bus.lock in a virgin directory makes the operator's first real start refuse to
// boot.
func TestCLILogMissingTrailIsExit4AndLeavesNoLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogNoTrail {
		t.Fatalf("exit = %d, want %d (stdout: %s)(stderr: %s)", code, exitLogNoTrail, stdout, stderr)
	}
	if !strings.Contains(stderr, "no audit trail") {
		t.Fatalf("stderr does not plainly say the directory has no audit trail:\n%s", stderr)
	}
	if !strings.Contains(stderr, wal.AuditFileName) {
		t.Fatalf("stderr does not name %s:\n%s", wal.AuditFileName, stderr)
	}
	// NOTHING was written: not the trail, and above all not the lock file.
	if _, err := os.Stat(filepath.Join(dir, dirlock.LockFileName)); !os.IsNotExist(err) {
		t.Fatalf("a failed run left %s behind (stat err = %v); the operator's first real start would then refuse to boot",
			dirlock.LockFileName, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a refusal created files in the data directory: %v", names)
	}
}

// TestCLILogMissingTrailJSONCarriesNoMessageID: the failure object is machine
// readable, branchable on `ok`, and -- like the damage object -- must not carry
// a message_id.
func TestCLILogMissingTrailJSONCarriesNoMessageID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	code, stdout, _ := cli6Run(t, "-data-dir", dir, "-json")
	if code != exitLogNoTrail {
		t.Fatalf("exit = %d, want %d", code, exitLogNoTrail)
	}
	objs := cli6JSONLines(t, stdout)
	if len(objs) != 1 {
		t.Fatalf("want exactly one failure object, got %d:\n%s", len(objs), stdout)
	}
	if ok, _ := objs[0]["ok"].(bool); ok {
		t.Fatalf("failure object has ok = true: %v", objs[0])
	}
	if c, _ := objs[0]["exit_code"].(float64); int(c) != exitLogNoTrail {
		t.Fatalf("failure object exit_code = %v, want %d", objs[0]["exit_code"], exitLogNoTrail)
	}
	if _, bad := objs[0]["message_id"]; bad {
		t.Fatalf("the failure object carries a message_id key: %v", objs[0])
	}
}

// TestCLILogEmptyTrailIsLoudCorruptionNotACleanRead: a zero-length bus.audit is
// a file with no header, which is damage. Reporting it as a clean read of zero
// records would be the silent-discard defect wearing a reader's clothes.
//
// THE MAC KEY MUST BE PRESENT FOR THIS TEST TO MEAN ANYTHING. Two pre-flight
// refusals now run BEFORE wal.ScanAll (exit 5: no wal-mac.key, and a bus.audit
// that is not format version 2), and a zero-length file with NO key takes the
// first of them -- see TestCLILogNoMACKeyIsExit5AndNeverMintsOne. So the key is
// asserted present here: this test pins that a zero-length trail is still
// SCANNED, and still answered with the precise "file is empty" damage report,
// rather than being swallowed by the newer, vaguer refusal.
func TestCLILogEmptyTrailIsLoudCorruptionNotACleanRead(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())
	if err := os.Truncate(cli6AuditPath(dir), 0); err != nil {
		t.Fatalf("truncating the trail to zero: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, wal.MACKeyFileName)); err != nil {
		t.Fatalf("the fixture left no %s, so this asserts the exit-5 refusal rather than the empty-file scan: %v",
			wal.MACKeyFileName, err)
	}

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogDamaged {
		t.Fatalf("exit = %d, want %d (a zero-length trail is damage, NOT a clean read of zero records)\nstdout: %s\nstderr: %s",
			code, exitLogDamaged, stdout, stderr)
	}
	if !strings.Contains(stderr, "DAMAGED") {
		t.Fatalf("stderr does not announce the empty file as damage:\n%s", stderr)
	}
	if !strings.Contains(stderr, "file is empty") {
		t.Fatalf("stderr does not say the file is empty:\n%s", stderr)
	}
}

// ---------------------------------------------------------------------------
// 5. The dirlock
// ---------------------------------------------------------------------------

// TestCLILogRefusesWhenTheDataDirectoryIsLocked. flock is held per open file
// description, so a second Acquire in THIS process conflicts exactly as a
// running bus would.
//
// This test does NOT call t.Parallel: it owns its directory, but the property
// under test is a lock, and interleaving another run against the same directory
// would make the assertion about scheduling rather than about the command.
func TestCLILogRefusesWhenTheDataDirectoryIsLocked(t *testing.T) {
	dir := cli6WriteTrail(t, cli6Trail())

	lock, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("dirlock.Acquire(%s): %v", dir, err)
	}
	defer lock.Release()

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogBusRunning {
		t.Fatalf("exit = %d, want %d (stdout: %s)(stderr: %s)", code, exitLogBusRunning, stdout, stderr)
	}
	if !strings.Contains(stderr, "locked") {
		t.Fatalf("stderr does not say the directory is locked:\n%s", stderr)
	}
	// The message must name the REMEDY, not just the symptom.
	if !strings.Contains(stderr, "stop the bus") {
		t.Fatalf("stderr does not tell the operator to stop the bus:\n%s", stderr)
	}
	// No record leaked out before the refusal.
	if strings.Contains(stdout, "busA.msg-1") {
		t.Fatalf("records were printed despite the lock refusal:\n%s", stdout)
	}

	// And once the lock is released the same command succeeds, so the refusal
	// really was the lock and not something permanent about the fixture.
	if err := lock.Release(); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	code, _, stderr = cli6Run(t, "-data-dir", dir)
	if code != exitLogOK {
		t.Fatalf("after releasing the lock, exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
}

// ---------------------------------------------------------------------------
// 6. Filters
// ---------------------------------------------------------------------------

// TestCLILogFilters is table-driven over ONE fixture trail.
//
// The subtests deliberately do NOT run in parallel: they share a data directory,
// and every run takes its exclusive dirlock, so concurrent subtests would race
// each other to exit 3.
func TestCLILogFilters(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	at := func(d time.Duration) string { return cli6Base.Add(d).Format(time.RFC3339) }

	for _, tc := range []struct {
		name string
		args []string
		want []uint64
	}{
		{"no-filter", nil, []uint64{1, 2, 3}},
		{"sender-alice", []string{"-sender", "busA.alice"}, []uint64{1, 3}},
		{"sender-bob", []string{"-sender", "busA.bob"}, []uint64{2}},
		// EXACT match, not a prefix or a substring: an unqualified id names
		// nobody (invariant 2).
		{"sender-unqualified-matches-nothing", []string{"-sender", "alice"}, nil},
		{"sender-unknown", []string{"-sender", "busZ.nobody"}, nil},
		{"recipient-bob", []string{"-recipient", "busA.bob"}, []uint64{1}},
		{"recipient-carol", []string{"-recipient", "busB.carol"}, []uint64{2}},
		// seq 3 is a BROADCAST from alice. It must NOT match a -recipient
		// filter: the trail records no recipient list for a broadcast, so
		// matching it would be a guess about a roster this file does not hold.
		{"recipient-alice-excludes-the-broadcast", []string{"-recipient", "busA.alice"}, []uint64{2}},
		{"recipient-unknown", []string{"-recipient", "busZ.nobody"}, nil},
		// -since is INCLUSIVE: the record AT the boundary is in.
		{"since-boundary-is-inclusive", []string{"-since", at(60 * time.Second)}, []uint64{2, 3}},
		{"since-before-everything", []string{"-since", at(-time.Second)}, []uint64{1, 2, 3}},
		{"since-after-everything", []string{"-since", at(time.Hour)}, nil},
		// -until is EXCLUSIVE: the record AT the boundary is out, which is what
		// makes consecutive windows tile with no gap and no double count.
		{"until-boundary-is-exclusive", []string{"-until", at(60 * time.Second)}, []uint64{1}},
		{"until-after-everything", []string{"-until", at(time.Hour)}, []uint64{1, 2, 3}},
		{"window-tiles-lower-half", []string{"-until", at(60 * time.Second)}, []uint64{1}},
		{"window-tiles-upper-half", []string{"-since", at(60 * time.Second)}, []uint64{2, 3}},
		{"window-single-record", []string{"-since", at(60 * time.Second), "-until", at(120 * time.Second)}, []uint64{2}},
		// -min-seq / -max-seq are INCLUSIVE at both ends; 0 means unbounded.
		{"min-seq-inclusive", []string{"-min-seq", "2"}, []uint64{2, 3}},
		{"max-seq-inclusive", []string{"-max-seq", "2"}, []uint64{1, 2}},
		{"seq-range-single", []string{"-min-seq", "2", "-max-seq", "2"}, []uint64{2}},
		{"seq-zero-is-unbounded", []string{"-min-seq", "0", "-max-seq", "0"}, []uint64{1, 2, 3}},
		{"seq-above-everything", []string{"-min-seq", "99"}, nil},
		// Filters compose with AND.
		{"sender-and-seq", []string{"-sender", "busA.alice", "-min-seq", "2"}, []uint64{3}},
		{"sender-and-window", []string{"-sender", "busA.alice", "-until", at(60 * time.Second)}, []uint64{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-data-dir", dir, "-json"}, tc.args...)
			code, stdout, stderr := cli6Run(t, args...)
			// A filter that matches nothing is an ANSWER, not an error.
			if code != exitLogOK {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
			}
			got := cli6Seqs(t, stdout)
			if len(got) != len(tc.want) {
				t.Fatalf("seqs = %v, want %v\n%s", got, tc.want, stdout)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("seqs = %v, want %v (in this order)", got, tc.want)
				}
			}
		})
	}
}

// TestCLILogFilterMatchingNothingIsHumanReadableAndExit0 pins the human-mode
// answer for an empty result: it is a count of zero, not silence and not an
// error.
func TestCLILogFilterMatchingNothingIsHumanReadableAndExit0(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-sender", "busZ.nobody")
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
	if !strings.Contains(stdout, "0 record(s) shown.") {
		t.Fatalf("an empty result did not report a zero count:\n%s", stdout)
	}
	if strings.Contains(stdout, "THE TRAIL IS DAMAGED") {
		t.Fatalf("an intact trail was reported as damaged:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// 7. Usage
// ---------------------------------------------------------------------------

// TestCLILogUsageErrorsAreExit2 covers every way to be told off before anything
// is read. None of these may take a lock or read a byte.
func TestCLILogUsageErrorsAreExit2(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown-flag", []string{"-data-dir", dir, "-nope"}},
		{"unexpected-positional-argument", []string{"-data-dir", dir, "extra"}},
		{"malformed-since", []string{"-data-dir", dir, "-since", "not-a-timestamp"}},
		{"malformed-until", []string{"-data-dir", dir, "-until", "2026-13-45"}},
		{"since-with-no-timezone", []string{"-data-dir", dir, "-since", "2026-08-14T09:00:00"}},
		{"until-not-after-since", []string{"-data-dir", dir, "-since", "2026-08-14T10:00:00Z", "-until", "2026-08-14T09:00:00Z"}},
		{"min-seq-above-max-seq", []string{"-data-dir", dir, "-min-seq", "5", "-max-seq", "2"}},
		{"empty-data-dir", []string{"-data-dir", ""}},
		{"non-numeric-min-seq", []string{"-data-dir", dir, "-min-seq", "many"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := cli6Run(t, tc.args...)
			if code != exitLogUsage {
				t.Fatalf("exit = %d, want %d (stdout: %s)(stderr: %s)", code, exitLogUsage, stdout, stderr)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatal("a usage error said nothing on stderr")
			}
			// A usage error must not have printed any of the trail.
			if strings.Contains(stdout, "busA.msg-1") {
				t.Fatalf("records were printed for a usage error:\n%s", stdout)
			}
		})
	}
}

// TestCLILogUsageErrorDoesNotEchoTheOffendingArgument: an unexpected positional
// is unvalidated argv on its way to a terminal, so it is counted, not quoted.
func TestCLILogUsageErrorDoesNotEchoTheOffendingArgument(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())
	const nasty = "\x1b[31mPWNED\x07"

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, nasty)
	if code != exitLogUsage {
		t.Fatalf("exit = %d, want %d", code, exitLogUsage)
	}
	if strings.Contains(stderr, nasty) || strings.Contains(stdout, nasty) {
		t.Fatalf("the offending argument was echoed back to the terminal verbatim:\nstdout: %q\nstderr: %q", stdout, stderr)
	}
}

// TestCLILogHelpIsExit0OnStdoutAndSaysMetadataOnly.
//
// The lowercase substring "metadata only" is a CONTRACT, not a nicety: it is
// what tells an operator, in the place they will actually look, that no body is
// retrievable from this command. CLI-6's stored proof greps `log --help` for it;
// this pins the same property as a real assertion so the property survives even
// if the proof command is ever changed.
func TestCLILogHelpIsExit0OnStdoutAndSaysMetadataOnly(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := cli6Run(t, "-h")
	if code != exitLogOK {
		t.Fatalf("-h exit = %d, want %d", code, exitLogOK)
	}
	if !strings.Contains(stdout, "metadata only") {
		t.Fatalf(`-h output does not contain the lowercase substring "metadata only":\n%s`, stdout)
	}
	// Requested help is OUTPUT and belongs on stdout; stderr stays clean so a
	// script can capture help without merging streams.
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("-h wrote to stderr:\n%s", stderr)
	}
	// The exit-code table is part of the contract an agent branches on.
	for _, want := range []string{"EXIT CODES", "-data-dir", "-json", "-since", "-until", "-min-seq", "-max-seq"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("-h output does not document %q:\n%s", want, stdout)
		}
	}
}

// TestCLILogHumanHeaderStatesBodiesAreNotRecorded: the very first thing an
// operator sees, before any record, must be that bodies are not in this file --
// so nobody concludes from a long clean listing that the content is here
// somewhere.
func TestCLILogHumanHeaderStatesBodiesAreNotRecorded(t *testing.T) {
	t.Parallel()
	dir := cli6WriteTrail(t, cli6Trail())

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
	if !strings.Contains(stdout, "METADATA ONLY") {
		t.Fatalf("the human header does not say the trail is metadata only:\n%s", stdout)
	}
	if !strings.Contains(stdout, "MESSAGE BODIES ARE NOT") {
		t.Fatalf("the human header does not say bodies are not recorded:\n%s", stdout)
	}
	// The header precedes the first record.
	hdr := strings.Index(stdout, "METADATA ONLY")
	first := strings.Index(stdout, "busA.msg-1")
	if hdr < 0 || first < 0 || hdr > first {
		t.Fatalf("the metadata-only header does not precede the first record:\n%s", stdout)
	}
}

// ---------------------------------------------------------------------------
// 8. Exit 5 -- THIS READER CANNOT VOUCH FOR THESE BYTES
// ---------------------------------------------------------------------------
//
// Exit 5 is a refusal taken BEFORE the scan, and it is categorically different
// from exit 1. Exit 1 says "this trail is damaged, here is everything that WAS
// readable"; exit 5 says the readable part carries no authority in the first
// place, so there is nothing to salvage and printing any of it would present a
// forgery as provenance. Nothing is printed as a record on this path, in either
// output mode.

// cli6MACKeyPath is <dir>/wal-mac.key, named through wal.MACKeyFileName so this
// cannot drift from the file wal actually opens.
func cli6MACKeyPath(dir string) string { return filepath.Join(dir, wal.MACKeyFileName) }

// cli6NoRecordLeaked asserts that nothing from the trail -- not a message id,
// not a bus id, not a header promising routing and provenance -- reached stdout.
// It is the assertion that separates a REFUSAL from a report.
func cli6NoRecordLeaked(t *testing.T, stdout string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if strings.Contains(stdout, m) {
			t.Fatalf("a refusal (exit %d) printed record content %q; nothing on this path may be presented as provenance:\n%s",
				exitLogUnverifiable, m, stdout)
		}
	}
	if strings.Contains(stdout, "append-only message audit trail:") {
		t.Fatalf("a refusal printed the record header, which reads as the start of a listing:\n%s", stdout)
	}
	if strings.Contains(stdout, "record(s) shown.") {
		t.Fatalf("a refusal printed a record count, which asserts a read that never happened:\n%s", stdout)
	}
}

// cli6AssertJSONRefusal decodes the --json failure object and pins its shape:
// exactly one object, ok=false, the right exit_code, and NO message_id (which
// scripts/fed-smoke.sh would count as a delivery).
func cli6AssertJSONRefusal(t *testing.T, stdout string, wantCode int) map[string]interface{} {
	t.Helper()
	objs := cli6JSONLines(t, stdout)
	if len(objs) != 1 {
		t.Fatalf("want exactly one failure object, got %d lines:\n%s", len(objs), stdout)
	}
	if ok, _ := objs[0]["ok"].(bool); ok {
		t.Fatalf("failure object has ok = true: %v", objs[0])
	}
	if c, _ := objs[0]["exit_code"].(float64); int(c) != wantCode {
		t.Fatalf("failure object exit_code = %v, want %d", objs[0]["exit_code"], wantCode)
	}
	if _, bad := objs[0]["message_id"]; bad {
		t.Fatalf("the failure object carries a message_id key: %v", objs[0])
	}
	if _, bad := objs[0]["damaged"]; bad {
		t.Fatalf("a refusal emitted a `damaged` object; exit %d is NOT a damage report: %v", wantCode, objs[0])
	}
	return objs[0]
}

// TestCLILogNoMACKeyIsExit5AndNeverMintsOne is the regression test for the worst
// thing this command could possibly do.
//
// # WHAT WENT WRONG
//
// The security gate probed a data directory holding only a ZERO-LENGTH
// bus.audit and no wal-mac.key. The command exited 1 ("file is empty"), which is
// defensible -- and the directory came back holding `wal-mac.key mode=-rw-------
// len=65`. wal.ScanAll resolves a codec, which resolves a MAC key, and wal's
// macKeyMayBeCreated permits CREATING one for exactly the shapes a reader is
// most likely to be pointed at: a zero-length file, an unknown magic, or a
// version 2 file that is only its own header. The creation was SILENT, because
// ScanAll takes no logger and wal's "generated a new MAC key" line is suppressed
// when the logger is nil.
//
// The harm is not the wasted 65 bytes. On a directory whose bus.wal is INTACT
// but whose key was lost, one run of this READ-ONLY command converted
// wal.ErrMACKeyMissing -- remedy "restore a 64-byte file from backup" -- into
// wal.ErrMACKeyMismatch, whose documented remedy is to move bus.wal aside. A
// read-only evidence tool turned a recoverable loss into a destroyed
// write-ahead log.
//
// # WHAT THIS ASSERTS
//
// The load-bearing assertion is the LAST one in each case: after the run, NO
// wal-mac.key exists in the directory. The exit code and the stderr wording are
// the honesty half (with no key, not one record can be authenticated, so every
// line the command could print would be an assertion it has no standing to
// make); the absent key file is the safety half, and it is the one that was
// actually broken.
//
// The zero-length subcase is the exact shape the probe used and the one wal
// would still mint for. The full-trail subcase is the honesty half on a
// directory wal would REFUSE to mint for, so the refusal is this command's own
// judgement rather than a side effect of wal declining.
func TestCLILogNoMACKeyIsExit5AndNeverMintsOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// build returns a data directory that holds a bus.audit and NO
		// wal-mac.key.
		build func(t *testing.T) string
	}{
		{
			// THE PROBE'S EXACT SHAPE, and the one wal.ScanAll would mint a key
			// for: size 0 means macKeyMayBeCreated returns true unconditionally.
			name: "zero-length-trail-is-the-shape-wal-would-mint-for",
			build: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(cli6AuditPath(dir), nil, 0o600); err != nil {
					t.Fatalf("creating a zero-length %s: %v", wal.AuditFileName, err)
				}
				return dir
			},
		},
		{
			// A real, intact, three-record trail whose key has been lost.
			name: "intact-trail-whose-key-was-lost",
			build: func(t *testing.T) string {
				dir := cli6WriteTrail(t, cli6Trail())
				// NON-VACUOUS: the fixture really did create a key, so removing
				// it is a state change and not a no-op.
				if _, err := os.Stat(cli6MACKeyPath(dir)); err != nil {
					t.Fatalf("the fixture never created %s, so removing it proves nothing: %v", wal.MACKeyFileName, err)
				}
				if err := os.Remove(cli6MACKeyPath(dir)); err != nil {
					t.Fatalf("removing %s: %v", wal.MACKeyFileName, err)
				}
				return dir
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			t.Run("human", func(t *testing.T) {
				dir := tc.build(t)
				code, stdout, stderr := cli6Run(t, "-data-dir", dir)
				if code != exitLogUnverifiable {
					t.Fatalf("exit = %d, want %d (a trail with no MAC key cannot be authenticated at all)\nstdout: %s\nstderr: %s",
						code, exitLogUnverifiable, stdout, stderr)
				}
				// NOTHING on stdout: not a record, not a header, not a count.
				if stdout != "" {
					t.Fatalf("a refusal wrote to stdout in human mode; stdout must be empty:\n%q", stdout)
				}
				// LOUD on stderr, and specific enough to act on.
				for _, want := range []string{
					"agent-bus log:",
					"no MAC key",
					cli6MACKeyPath(dir),
					"none of it was read",
					"remedy:",
					wal.MACKeyFileName,
				} {
					if !strings.Contains(stderr, want) {
						t.Fatalf("stderr does not contain %q, so the refusal is not loud or not specific:\n%s", want, stderr)
					}
				}

				// THE ASSERTION THIS TEST EXISTS FOR.
				assertNoMACKeyMinted(t, dir)
			})

			t.Run("json", func(t *testing.T) {
				dir := tc.build(t)
				code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
				if code != exitLogUnverifiable {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitLogUnverifiable, stdout, stderr)
				}
				obj := cli6AssertJSONRefusal(t, stdout, exitLogUnverifiable)
				if msg, _ := obj["error"].(string); !strings.Contains(msg, "no MAC key") {
					t.Fatalf("the JSON failure object does not say the MAC key is missing: %v", obj)
				}
				cli6NoRecordLeaked(t, stdout, "busA.msg-1", "busA.msg-2", "busA.msg-3")
				// In --json the answer goes to STDOUT, so an agent that
				// redirected stderr away still gets a parseable refusal. That is
				// the whole reason --json exists (invariant 7's second audience).
				if strings.TrimSpace(stderr) != "" {
					t.Fatalf("--json duplicated the refusal onto stderr; the parseable answer must stand alone on stdout:\n%s", stderr)
				}

				assertNoMACKeyMinted(t, dir)
			})
		})
	}
}

// assertNoMACKeyMinted is the regression assertion, factored out so every exit-5
// path can restate it: a READ-ONLY command must not leave durable key material
// behind, because a key minted now verifies nothing that was written under the
// real one and escalates "the key is missing" into "the key does not match".
func assertNoMACKeyMinted(t *testing.T, dir string) {
	t.Helper()
	if fi, err := os.Stat(cli6MACKeyPath(dir)); err == nil {
		t.Fatalf("THE READ-ONLY COMMAND MINTED %s (mode=%s len=%d). "+
			"A key minted now authenticates nothing that was written under the real one, and it converts a recoverable "+
			"wal.ErrMACKeyMissing (\"restore a 64-byte file\") into wal.ErrMACKeyMismatch, whose documented remedy is to "+
			"move bus.wal aside. This is the finding the exit-5 pre-flight check exists to close.",
			wal.MACKeyFileName, fi.Mode(), fi.Size())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", cli6MACKeyPath(dir), err)
	}
}

// cli6WriteRawAudit replaces bus.audit with exactly these bytes, in a directory
// that already holds a real wal-mac.key -- so the format check, and not the MAC
// key check, is the thing under test. The two run in that order.
func cli6WriteRawAudit(t *testing.T, raw []byte) string {
	t.Helper()
	dir := cli6WriteTrail(t, cli6Trail())
	if _, err := os.Stat(cli6MACKeyPath(dir)); err != nil {
		t.Fatalf("the fixture left no %s, so this would assert the MAC-key refusal instead: %v", wal.MACKeyFileName, err)
	}
	if err := os.WriteFile(cli6AuditPath(dir), raw, 0o600); err != nil {
		t.Fatalf("writing the hand-built %s: %v", wal.AuditFileName, err)
	}
	return dir
}

// cli6PlantedMarker is put INSIDE the hand-built file. Its absence from stdout
// is what proves the refusal happened before any content was interpreted --
// which is the point of the whole check, because the security gate got
// `message bus-a.msg-FORGED-BY-ATTACKER` out of a version 1 file.
const cli6PlantedMarker = "FORGED-BY-ATTACKER-PLANTED-MARKER"

// cli6AuditHeaderBytes builds an eight-byte audit magic followed by a four-byte
// BIG-ENDIAN format version, which is the twelve-byte prefix EVERY layout wal
// has ever written begins with -- and therefore the only twelve bytes a reader
// implementing neither layout can still use to tell them apart.
func cli6AuditHeaderBytes(magic string, version uint32) []byte {
	head := make([]byte, 12)
	copy(head[:8], magic)
	head[8] = byte(version >> 24)
	head[9] = byte(version >> 16)
	head[10] = byte(version >> 8)
	head[11] = byte(version)
	return head
}

// TestCLILogUnauthenticatableFormatIsExit5 refuses a bus.audit this reader must
// not interpret, and proves the refusal happens BEFORE any content is read.
//
// # WHY A READER HAS TO MAKE THIS JUDGEMENT ITSELF
//
// Version 1 frames are authenticated by an UNKEYED CRC32C, which anyone who can
// write the directory can compute. wal will happily READ a version 1 file --
// detectFormat returns 1 and codecFor hands back a CRC32C codec -- so wal.ScanAll
// "verifies" records an attacker authored, and this command would have printed
// them under a header promising routing and provenance, with exit 0 and an empty
// stderr. The security gate did exactly that and got
// `message bus-a.msg-FORGED-BY-ATTACKER` out of it. internal/wal/audit.go records
// that audit records have ONLY EVER been written at format version 2, so a
// version 1 bus.audit is never a real bus's file -- it is a planted one.
//
// The load-bearing assertion in every case is the LAST one: cli6PlantedMarker is
// inside the file, and it must appear NOWHERE in stdout. An exit code alone
// would not distinguish "refused before reading" from "read it, then complained".
func TestCLILogUnauthenticatableFormatIsExit5(t *testing.T) {
	t.Parallel()

	marker := []byte(cli6PlantedMarker)

	for _, tc := range []struct {
		name string
		raw  []byte
		// wantStderr are substrings the refusal must state. They are the
		// operator-facing half: an exit code says "no", these say why and what
		// to do with the file.
		wantStderr []string
	}{
		{
			// THE ATTACK. Real magic, version 1, records that would "verify"
			// under a checksum anyone can compute.
			name: "format-version-1-is-authenticated-by-an-unkeyed-checksum",
			raw:  append(cli6AuditHeaderBytes(auditMagic, 1), marker...),
			wantStderr: []string{
				"declares format version 1",
				"version 1 frames are authenticated by an UNKEYED checksum that ANYONE CAN COMPUTE",
				"None of it was read",
				"MUST NOT be treated as evidence",
			},
		},
		{
			// A version this binary has never written in either direction.
			name: "an-unimplemented-future-version-is-also-refused",
			raw:  append(cli6AuditHeaderBytes(auditMagic, 99), marker...),
			wantStderr: []string{
				"declares format version 99",
				"None of it was read",
			},
		},
		{
			// Not this bus's file at all.
			name: "wrong-magic-is-not-a-trail-this-bus-wrote",
			raw:  append(cli6AuditHeaderBytes("NOTAUDIT", wal.FormatVersion), marker...),
			wantStderr: []string{
				"does not begin with this bus's audit magic",
				"none of it was read",
			},
		},
		{
			// NON-EMPTY BUT SHORTER THAN THE TWELVE-BYTE PREFIX. It claims to be
			// something and cannot be checked, which is not the same as the
			// zero-length case below (that one carries no header to judge and is
			// answered precisely, as damage, by the scan).
			name: "non-empty-but-too-short-to-hold-a-format-header",
			raw:  []byte("AGNTBUS"), // 7 bytes: one short of the magic
			wantStderr: []string{
				"too short to hold even the 12-byte format header",
				"nothing in it can be identified or authenticated",
			},
		},
		{
			// Exactly one byte short of the twelve-byte prefix: the boundary.
			name: "eleven-bytes-is-one-short-of-the-prefix",
			raw:  cli6AuditHeaderBytes(auditMagic, wal.FormatVersion)[:11],
			wantStderr: []string{
				"the audit trail",
				"is 11 bytes, too short",
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := cli6WriteRawAudit(t, tc.raw)

			t.Run("human", func(t *testing.T) {
				code, stdout, stderr := cli6Run(t, "-data-dir", dir)
				if code != exitLogUnverifiable {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitLogUnverifiable, stdout, stderr)
				}
				if stdout != "" {
					t.Fatalf("a refusal wrote to stdout in human mode; stdout must be empty:\n%q", stdout)
				}
				for _, want := range tc.wantStderr {
					if !strings.Contains(stderr, want) {
						t.Fatalf("stderr does not contain %q:\n%s", want, stderr)
					}
				}
				if !strings.Contains(stderr, cli6AuditPath(dir)) {
					t.Fatalf("stderr does not name the refused file:\n%s", stderr)
				}
				// NO CONTENT FROM THE FILE REACHED THE OPERATOR.
				cli6NoRecordLeaked(t, stdout, cli6PlantedMarker)
				if strings.Contains(stdout, cli6PlantedMarker) {
					t.Fatalf("planted content from an unauthenticatable file reached stdout:\n%s", stdout)
				}
			})

			t.Run("json", func(t *testing.T) {
				code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
				if code != exitLogUnverifiable {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitLogUnverifiable, stdout, stderr)
				}
				cli6AssertJSONRefusal(t, stdout, exitLogUnverifiable)
				cli6NoRecordLeaked(t, stdout, cli6PlantedMarker)
			})
		})
	}
}

// TestCLILogRefusalOrderIsMACKeyThenFormat pins that the MAC-key refusal wins
// when BOTH faults are present.
//
// It is not a stylistic preference. "Restore wal-mac.key" is an actionable
// remedy an operator can carry out; "this file is not format version 2" invites
// them to go looking at the file. Reporting the second when the first is also
// true would send an operator to inspect bytes they could not have authenticated
// even if the format had been right.
func TestCLILogRefusalOrderIsMACKeyThenFormat(t *testing.T) {
	t.Parallel()
	dir := cli6WriteRawAudit(t, append(cli6AuditHeaderBytes(auditMagic, 1), []byte(cli6PlantedMarker)...))
	if err := os.Remove(cli6MACKeyPath(dir)); err != nil {
		t.Fatalf("removing %s: %v", wal.MACKeyFileName, err)
	}

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogUnverifiable {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogUnverifiable, stderr)
	}
	if !strings.Contains(stderr, "no MAC key") {
		t.Fatalf("with BOTH faults present the refusal must name the actionable one (the missing key) first:\n%s", stderr)
	}
	if stdout != "" {
		t.Fatalf("a refusal wrote to stdout:\n%q", stdout)
	}
	// And a version 1 file is one of the shapes wal WOULD mint a key for, so
	// this is also the regression assertion on the more dangerous combination.
	assertNoMACKeyMinted(t, dir)
}

// TestCLILogHelpDocumentsExit5 -- the exit codes are a CONTRACT an agent's
// script branches on (invariant 7), so a new one that is not in --help is a
// capability nobody can use correctly.
func TestCLILogHelpDocumentsExit5(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := cli6Run(t, "-h")
	if code != exitLogOK {
		t.Fatalf("-h exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}
	// The lowercase phrase, restated here because it is what CLI-6's stored
	// proof greps for and it must not be lost while this text is edited.
	if !strings.Contains(stdout, "metadata only") {
		t.Fatalf(`-h output does not contain the lowercase substring "metadata only":\n%s`, stdout)
	}
	for _, want := range []string{
		// The exit-code table row itself, not merely the digit 5 somewhere.
		"5  the trail cannot be AUTHENTICATED",
		// And the prose that tells an operator which two states reach it.
		"CANNOT AUTHENTICATE IS REFUSED, NOT PRINTED (exit 5)",
		wal.MACKeyFileName,
		"not format version 2",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("-h does not document exit 5 with %q:\n%s", want, stdout)
		}
	}
}

// ---------------------------------------------------------------------------
// 9. HIGH-3 -- the subject of the audit must not be able to author the output
// ---------------------------------------------------------------------------

// cli6ESC and cli6CR are the two raw bytes that must never reach a terminal
// through this command.
const (
	cli6ESC = "\x1b"
	cli6CR  = "\r"
)

// cli6ForgedSender is a sender id carrying, in one value: a CLEAR-SCREEN escape,
// a colour escape, a carriage return, and a newline followed by text shaped
// EXACTLY like a real record's first line. wal's auditID bounds this field on
// emptiness and length only -- it imposes NO character restriction -- so every
// byte here can legitimately reach the renderer from the on-disk record.
//
// Unquoted, this printed a COMPLETE FABRICATED RECORD LINE naming a message id,
// a seq and a sender that appear nowhere in the file.
const cli6ForgedSender = "busA.evil" + cli6ESC + "[2J" + cli6ESC + "[1;31m" + cli6CR +
	"\nseq 999  2026-08-14T09:00:00Z  from busA.INNOCENT  to \"busA.victim\""

// cli6ForgedBusHop is a bus-path element carrying an escape AND the four
// ordinary characters that separate hops, so a renderer that quoted the JOINED
// path instead of each element would let one hop masquerade as three.
const cli6ForgedBusHop = "busB" + cli6ESC + "[1;31m -> busFORGED"

// TestCLILogHumanEscapesTerminalControlBytes is HIGH-3.
//
// An audit reader whose output can be AUTHORED BY THE SUBJECT OF THE AUDIT is
// worse than no reader: the operator reading it believes they are reading the
// file. Three separate harms, all from the same missing %q:
//
//	CLEAR SCREEN / COLOUR -- ESC[2J and ESC[1;31m reached the terminal raw, so
//	  the attacker chooses what the operator sees around their record.
//	CARRIAGE RETURN -- overwrites the line already printed, so a record can be
//	  visually replaced by one that is not in the file.
//	NEWLINE -- forges a COMPLETE record line. The gate got a fabricated
//	  `seq ... from ... to ...` line naming ids that appear nowhere on disk.
//
// The fix is %q on every client-derived string, PER ELEMENT for the slices. This
// test asserts the harms are gone AND that the value still survives, escaped, so
// the fix is not "drop the field".
func TestCLILogHumanEscapesTerminalControlBytes(t *testing.T) {
	t.Parallel()

	dir := cli6WriteTrail(t, []cli6Fixture{{
		audit: &wal.AuditRecord{
			MessageID:     "busA.msg-" + cli6ESC + "[5m",
			Seq:           1,
			Sender:        cli6ForgedSender,
			Recipients:    []string{"busA.bob" + cli6CR + "OVERWRITTEN"},
			BusPath:       []string{"busA", cli6ForgedBusHop},
			SentAt:        cli6Base,
			Size:          11,
			ContentSHA256: cli6SHA(0xe5),
		},
		body: `{"text":"forged"}`,
	}})

	// NON-VACUOUS: the hostile bytes really are on disk, so "they did not reach
	// the terminal" is a statement about the renderer and not about the fixture.
	auditBytes, err := os.ReadFile(cli6AuditPath(dir))
	if err != nil {
		t.Fatalf("reading %s: %v", wal.AuditFileName, err)
	}
	if !bytes.Contains(auditBytes, []byte("busA.evil")) {
		t.Fatalf("the hostile sender never reached %s, so this test proves nothing", wal.AuditFileName)
	}

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (the record is VALID; hostile content is not damage)\nstderr: %s", code, exitLogOK, stderr)
	}

	// 1. NO RAW ESC ANYWHERE. Not in the record, not in the header, not in the
	//    footer -- an operator's terminal must not be reprogrammed by the file.
	if i := strings.IndexByte(stdout, 0x1b); i >= 0 {
		t.Fatalf("a RAW ESC byte (0x1b) reached stdout at offset %d; the terminal is under the audited party's control:\n%q", i, stdout)
	}
	// 2. NO RAW CR, which would overwrite a line already printed.
	if i := strings.IndexByte(stdout, '\r'); i >= 0 {
		t.Fatalf("a RAW CR byte reached stdout at offset %d; a printed record can be visually replaced:\n%q", i, stdout)
	}
	// 3. NO FORGED RECORD LINE. Exactly one record was written, so exactly one
	//    line may begin with "seq ".
	seqLines := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "seq ") {
			seqLines++
		}
		if strings.HasPrefix(line, "seq 999") {
			t.Fatalf("A FABRICATED RECORD LINE was authored by the sender field: %q\n%s", line, stdout)
		}
	}
	if seqLines != 1 {
		t.Fatalf("stdout has %d lines beginning with \"seq \", want exactly 1 (one record was written):\n%s", seqLines, stdout)
	}
	if !strings.Contains(stdout, "1 record(s) shown.") {
		t.Fatalf("the footer does not report exactly one record:\n%s", stdout)
	}

	// 4. THE VALUE SURVIVES, ESCAPED. The fix is not "drop the field": an audit
	//    reader that silently discarded a hostile id would hide the very thing an
	//    operator needs to see. %q is strconv.Quote, so ESC becomes \x1b, CR
	//    becomes \r and LF becomes \n, all as literal two/four-character text.
	for _, want := range []string{
		`busA.evil\x1b[2J\x1b[1;31m\r\nseq 999`, // the sender, intact and inert
		`\x1b[1;31m -> busFORGED`,               // the hop, still one element
		`busA.bob\rOVERWRITTEN`,                 // the recipient
		`busA.msg-\x1b[5m`,                      // the message id
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the escaped value %q is not in the output; escaping must PRESERVE the value, not drop it:\n%s", want, stdout)
		}
	}

	// 5. PER-ELEMENT QUOTING. The hostile hop contains " -> ", so a renderer
	//    that quoted the JOINED path would produce three visible hops from two
	//    real ones. Quoted per element, the path is exactly two quoted strings.
	line := ""
	for _, l := range strings.Split(stdout, "\n") {
		if strings.Contains(l, "bus path: ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no bus path line in the output:\n%s", stdout)
	}
	if got := strings.Count(line, `" -> "`); got != 1 {
		t.Fatalf("the bus path line shows %d element separators, want 1 (two hops): %q\n"+
			"Quoting the JOINED path instead of each element lets one hop masquerade as several.", got, line)
	}
}

// TestCLILogJSONEncodesTerminalControlBytes is the same property for --json,
// where encoding/json does the escaping. The shape assertion is the one that
// matters: ONE OBJECT PER LINE. A newline smuggled through a field would split a
// record across two lines, and a consumer counting lines -- scripts/fed-smoke.sh
// counts objects per message id -- would see two deliveries for one message.
func TestCLILogJSONEncodesTerminalControlBytes(t *testing.T) {
	t.Parallel()

	dir := cli6WriteTrail(t, []cli6Fixture{{
		audit: &wal.AuditRecord{
			MessageID:     "busA.msg-1",
			Seq:           1,
			Sender:        cli6ForgedSender,
			Recipients:    []string{"busA.bob" + cli6CR + "OVERWRITTEN"},
			BusPath:       []string{"busA", cli6ForgedBusHop},
			SentAt:        cli6Base,
			Size:          11,
			ContentSHA256: cli6SHA(0xe5),
		},
		body: `{"text":"forged"}`,
	}})

	code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
	if code != exitLogOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogOK, stderr)
	}

	// EXACTLY ONE LINE, hence exactly one object: the forged newline did not
	// split the record in two.
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("--json emitted %d lines for ONE record; a newline in a field split the object across lines:\n%q", len(lines), stdout)
	}
	// No raw control bytes in the NDJSON stream either.
	if i := strings.IndexByte(stdout, 0x1b); i >= 0 {
		t.Fatalf("a RAW ESC byte reached --json stdout at offset %d:\n%q", i, stdout)
	}
	if i := strings.IndexByte(stdout, '\r'); i >= 0 {
		t.Fatalf("a RAW CR byte reached --json stdout at offset %d:\n%q", i, stdout)
	}
	// encoding/json's escaping for ESC is \u001b.
	if !strings.Contains(stdout, `\u001b`) {
		t.Fatalf("--json did not JSON-encode the escape bytes:\n%q", stdout)
	}

	// AND THE VALUE ROUND-TRIPS EXACTLY. Escaping is a transport concern; a
	// consumer that decodes the line must get the bytes that are on disk, or the
	// trail would not be joinable against the file.
	objs := cli6RecordObjects(cli6JSONLines(t, stdout))
	if len(objs) != 1 {
		t.Fatalf("got %d record objects, want 1:\n%s", len(objs), stdout)
	}
	if got, _ := objs[0]["sender"].(string); got != cli6ForgedSender {
		t.Fatalf("sender did not round-trip:\n got %q\nwant %q", got, cli6ForgedSender)
	}
	path := cli6Strings(t, objs[0]["bus_path"], "bus_path")
	if len(path) != 2 || path[0] != "busA" || path[1] != cli6ForgedBusHop {
		t.Fatalf("bus_path = %q, want exactly [busA %q] -- the hostile hop must stay ONE element", path, cli6ForgedBusHop)
	}
}

// ---------------------------------------------------------------------------
// 10. ABSENT vs. UNREADABLE -- the accuracy fix
// ---------------------------------------------------------------------------

// TestCLILogUnreadableTrailIsNotReportedAsMissing.
//
// os.Stat fails for two entirely different reasons and they must never share a
// message. os.ErrNotExist means the trail genuinely is not there: exit 4, "there
// is NO provenance record". ANY OTHER stat error -- EACCES, EIO, a bad mount --
// means the OPPOSITE: this command could not LOOK, and the trail may be sitting
// there completely intact. Telling an operator their provenance is gone when it
// may not be is exactly the mis-reporting invariant 6 exists to prevent, so the
// unreadable case is exit 1 with its own wording and a remedy that points at
// permissions rather than at backup restoration.
//
// EACCES is produced by chmod 000 on the data directory itself: os.Stat of the
// DIRECTORY still succeeds (that needs search permission on its PARENT), while
// os.Stat of bus.audit INSIDE it fails, which is precisely the branch under test.
func TestCLILogUnreadableTrailIsNotReportedAsMissing(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		// chmod cannot deny the owner when the owner is root, so this test
		// cannot produce EACCES and a "pass" here would be fabricated.
		t.Skip("running as root: chmod 000 does not deny root, so EACCES cannot be produced")
	}

	dir := cli6WriteTrail(t, cli6Trail())
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod 000 %s: %v", dir, err)
	}
	// Restore before t.TempDir's cleanup runs, or the cleanup itself fails.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// NON-VACUOUS: the trail really is unreadable now, and really does still
	// exist -- which is the whole point of the distinction being tested.
	if _, err := os.Stat(cli6AuditPath(dir)); err == nil {
		t.Skip("the filesystem does not enforce directory permissions here, so EACCES cannot be produced")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture's %s vanished; this would test the ABSENT branch, not the unreadable one", wal.AuditFileName)
	}

	t.Run("human", func(t *testing.T) {
		code, stdout, stderr := cli6Run(t, "-data-dir", dir)
		if code != exitLogDamaged {
			t.Fatalf("exit = %d, want %d (an unreadable trail is NOT an absent one)\nstdout: %s\nstderr: %s",
				code, exitLogDamaged, stdout, stderr)
		}
		if !strings.Contains(stderr, "could NOT BE EXAMINED") {
			t.Fatalf("stderr does not say the trail could not be examined:\n%s", stderr)
		}
		// THE LOAD-BEARING CLAUSE: it must say what this is NOT evidence of.
		if !strings.Contains(stderr, "NOT evidence that it is missing or damaged") {
			t.Fatalf("stderr does not state that this is NOT evidence the trail is missing or damaged:\n%s", stderr)
		}
		// And it must NOT reuse the absent case's wording, which asserts a fact
		// this command has not established.
		for _, forbidden := range []string{
			"holds no audit trail",
			"there is NO provenance record",
			"must be restored from backup",
		} {
			if strings.Contains(stderr, forbidden) {
				t.Fatalf("an UNREADABLE trail was reported with the ABSENT trail's wording %q:\n%s", forbidden, stderr)
			}
		}
		// The remedy points at permissions, not at backups.
		if !strings.Contains(stderr, "permissions and ownership") {
			t.Fatalf("the remedy does not point at permissions and ownership:\n%s", stderr)
		}
		if stdout != "" {
			t.Fatalf("a refusal wrote to stdout:\n%q", stdout)
		}
	})

	t.Run("json", func(t *testing.T) {
		code, stdout, stderr := cli6Run(t, "-data-dir", dir, "-json")
		if code != exitLogDamaged {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitLogDamaged, stderr)
		}
		obj := cli6AssertJSONRefusal(t, stdout, exitLogDamaged)
		if msg, _ := obj["error"].(string); !strings.Contains(msg, "could NOT BE EXAMINED") {
			t.Fatalf("the JSON failure object does not say the trail could not be examined: %v", obj)
		}
	})
}

// TestCLILogAbsentTrailKeepsExit4Wording is the other half of the same branch:
// splitting os.ErrNotExist out must not have changed the answer for a trail that
// really is absent. Exit 4, the original wording, and -- because this refusal
// happens BEFORE the lock -- a directory left COMPLETELY empty.
func TestCLILogAbsentTrailKeepsExit4Wording(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	code, stdout, stderr := cli6Run(t, "-data-dir", dir)
	if code != exitLogNoTrail {
		t.Fatalf("exit = %d, want %d (an ABSENT trail is not an unreadable one)\nstdout: %s\nstderr: %s",
			code, exitLogNoTrail, stdout, stderr)
	}
	for _, want := range []string{
		"holds no audit trail",
		"there is NO provenance record",
		"restored from backup",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the ABSENT-trail message lost %q:\n%s", want, stderr)
		}
	}
	// It must NOT have acquired the unreadable case's hedging, which would tell
	// an operator to go looking at permissions for a file that is simply absent.
	if strings.Contains(stderr, "could NOT BE EXAMINED") {
		t.Fatalf("an ABSENT trail was reported with the UNREADABLE trail's wording:\n%s", stderr)
	}
	// Still completely empty: not the trail, not the MAC key, not bus.lock.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a refusal created files in the data directory: %v", names)
	}
}
