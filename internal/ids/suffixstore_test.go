package ids

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const suffixStoreTestBusID = "bus-suffixstore-test"

// mustEncodeSuffixFile renders floors to the on-disk form, failing the test if
// the encoder refuses. encodeSuffixFile refuses to persist a name that could not
// be read back — see its doc — so every fixture built here is one a real bus
// could legitimately have written.
func mustEncodeSuffixFile(t *testing.T, floors map[string]uint64) []byte {
	t.Helper()
	data, err := encodeSuffixFile(floors)
	if err != nil {
		t.Fatalf("encodeSuffixFile(%v): %v", floors, err)
	}
	return data
}

// TestEncodeSuffixFileRefusesAnUnreadableName pins the last guard before an
// IRREVERSIBLE write. A floors file that cannot be read back is never
// regenerated (ErrSuffixFileCorrupt), so persisting a name carrying a space or a
// newline would strand the data dir permanently — a space breaks the "<name>
// <suffix>" split, and a newline forges an entry for some other name.
func TestEncodeSuffixFileRefusesAnUnreadableName(t *testing.T) {
	for _, name := range []string{"has space", "has\nnewline", "UPPER", "bus-x.dotted", "", "-leading-dash"} {
		if _, err := encodeSuffixFile(map[string]uint64{name: 7}); !errors.Is(err, ErrSuffixFileCorrupt) {
			t.Errorf("encodeSuffixFile with name %q = %v, want an error satisfying errors.Is(err, ErrSuffixFileCorrupt)", name, err)
		}
	}
	// A floor of 0 is dropped rather than written, so it cannot strand anything
	// and needs no name check.
	if _, err := encodeSuffixFile(map[string]uint64{"has space": 0}); err != nil {
		t.Errorf("encodeSuffixFile with a zero floor = %v, want nil", err)
	}
}

// TestDurableNameSuffixesValidatesNamesAtItsDoor: unlike NameSuffixes, which
// deliberately does not validate, the durable wrapper does — its keys become
// bytes in a file that is never regenerated.
func TestDurableNameSuffixesValidatesNamesAtItsDoor(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.RaiseFloor("has space", 5); err == nil {
		t.Error("RaiseFloor accepted an illegal agent name")
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if n, err := store.NextSuffix("has space"); err == nil {
		t.Errorf("NextSuffix accepted an illegal agent name and issued %d", n)
	}
	// The refusals must not have written anything about that name.
	floors, _, err := readSuffixFile(store.Path())
	if err != nil {
		t.Fatalf("readSuffixFile: %v", err)
	}
	if len(floors) != 0 {
		t.Errorf("floors after two refused calls = %v, want empty", floors)
	}
}

// restart opens the data dir the way a server start would: load the persisted
// floors, seal them, and build a minter on the result. Each call is a fresh
// process as far as the allocator is concerned — nothing carries over in memory,
// only what reached the disk.
func restart(t *testing.T, dir string) *AgentIDMinter {
	t.Helper()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes(%q): %v", dir, err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	m, err := NewAgentIDMinter(suffixStoreTestBusID, store)
	if err != nil {
		t.Fatalf("NewAgentIDMinter: %v", err)
	}
	return m
}

func mintSuffix(t *testing.T, m *AgentIDMinter, name string) (string, uint64) {
	t.Helper()
	id, err := m.Mint(name)
	if err != nil {
		t.Fatalf("Mint(%q): %v", name, err)
	}
	_, gotName, n, err := ParseAgentID(id)
	if err != nil {
		t.Fatalf("minted id %q does not parse: %v", id, err)
	}
	if gotName != name {
		t.Fatalf("minted id %q carries name %q, want %q", id, gotName, name)
	}
	return id, n
}

// TestAgentIDSuffixesResumeAcrossRestart is THE test this work exists for.
//
// It is deliberately not a "the counter increments" test: incrementing within
// one process proves nothing about the property at issue. The whole hazard lives
// at the restart boundary — cmd/agent-bus/main.go builds a FRESH counter every
// start, so enrolling "alpha" after a restart mints the previous alpha's id, and
// a new keypair inherits a live routing and authorization identity (invariant 1:
// ids are never reused, INCLUDING ACROSS RESTARTS).
//
// Note the PRESENT tense: this test proves the allocator in this package has the
// property, NOT that the shipped server does. main.go still calls
// NewNameSuffixes and is owned by another task; until that wiring lands, a real
// bus still re-mints. Do not read a pass here as "the bug is fixed in
// production".
//
// So every case here crosses at least one restart and asserts STRICTLY GREATER,
// never merely "not equal" and never "increments".
func TestAgentIDSuffixesResumeAcrossRestart(t *testing.T) {
	t.Run("same name across one restart is strictly greater", func(t *testing.T) {
		dir := t.TempDir()

		id1, n1 := mintSuffix(t, restart(t, dir), "alpha")
		id2, n2 := mintSuffix(t, restart(t, dir), "alpha")

		if n2 <= n1 {
			t.Fatalf("after restart, alpha was minted suffix %d, want strictly greater than %d (id %q then %q): a restarted bus reissued an agent id", n2, n1, id1, id2)
		}
		if id1 == id2 {
			t.Fatalf("after restart, alpha was minted the identical id %q: the previous holder's routing and authorization identity was handed to a new agent", id1)
		}
	})

	t.Run("strictly increasing across many restarts", func(t *testing.T) {
		dir := t.TempDir()
		seen := map[uint64]string{}
		prev := uint64(0)

		for i := 0; i < 12; i++ {
			id, n := mintSuffix(t, restart(t, dir), "alpha")
			if n <= prev {
				t.Fatalf("restart %d minted suffix %d, want strictly greater than %d (id %q)", i, n, prev, id)
			}
			if dup, ok := seen[n]; ok {
				t.Fatalf("restart %d reissued suffix %d, already issued as %q", i, n, dup)
			}
			seen[n] = id
			prev = n
		}
	})

	t.Run("several mints per run still resume above the last", func(t *testing.T) {
		dir := t.TempDir()
		prev := uint64(0)

		for run := 0; run < 4; run++ {
			m := restart(t, dir)
			for i := 0; i < 5; i++ {
				id, n := mintSuffix(t, m, "alpha")
				if n <= prev {
					t.Fatalf("run %d mint %d: suffix %d, want strictly greater than %d (id %q)", run, i, n, prev, id)
				}
				prev = n
			}
		}
	})

	t.Run("names are independent and each resumes above its own last", func(t *testing.T) {
		dir := t.TempDir()

		m := restart(t, dir)
		_, alpha1 := mintSuffix(t, m, "alpha")
		_, beta1 := mintSuffix(t, m, "beta")
		_, beta2 := mintSuffix(t, m, "beta")

		m = restart(t, dir)
		_, alpha2 := mintSuffix(t, m, "alpha")
		_, beta3 := mintSuffix(t, m, "beta")

		if alpha2 <= alpha1 {
			t.Errorf("alpha resumed at %d, want strictly greater than %d", alpha2, alpha1)
		}
		if beta3 <= beta2 {
			t.Errorf("beta resumed at %d, want strictly greater than %d", beta3, beta2)
		}
		if beta1 >= beta2 {
			t.Errorf("beta went %d then %d within one run, want strictly increasing", beta1, beta2)
		}
		// alpha's counter must not have been advanced by beta's traffic: the
		// per-name counters are independent, and a shared one would leak the
		// enrolment rate of one name into another's ids.
		//
		// The expected values are 1 then suffixBlockSize+1, not 1 then 2,
		// because alpha's first mint reserves a whole block and the restart
		// resumes above the WHOLE reserved block rather than above the last
		// suffix issued. The skipped numbers are correct (point 4 of the
		// NameSuffixes doc). What independence means here is that beta's three
		// mints move alpha's numbers not at all — which is why these are exact
		// values and not a "strictly greater" check.
		if alpha1 != 1 || alpha2 != suffixBlockSize+1 {
			t.Errorf("alpha suffixes were %d then %d, want 1 then %d: the per-name counters are not independent (beta's traffic moved alpha)", alpha1, alpha2, suffixBlockSize+1)
		}

		// The control that makes the claim above airtight: the same run with NO
		// beta traffic at all must give alpha exactly the same two numbers.
		ctrl := t.TempDir()
		_, ctrlAlpha1 := mintSuffix(t, restart(t, ctrl), "alpha")
		_, ctrlAlpha2 := mintSuffix(t, restart(t, ctrl), "alpha")
		if ctrlAlpha1 != alpha1 || ctrlAlpha2 != alpha2 {
			t.Errorf("alpha got %d then %d alongside beta, but %d then %d alone: beta's traffic perturbed alpha's counter", alpha1, alpha2, ctrlAlpha1, ctrlAlpha2)
		}
	})

	t.Run("a name that only enrolled in an earlier run is not re-minted", func(t *testing.T) {
		dir := t.TempDir()

		// gamma enrols once, then never again for several restarts. Its floor
		// must survive all of them: the counter is never reset, including when
		// the agent has long departed (point 5 of the NameSuffixes doc).
		_, gamma1 := mintSuffix(t, restart(t, dir), "gamma")
		for i := 0; i < 5; i++ {
			mintSuffix(t, restart(t, dir), "delta")
		}
		_, gamma2 := mintSuffix(t, restart(t, dir), "gamma")

		if gamma2 <= gamma1 {
			t.Fatalf("gamma resumed at %d after five restarts of other traffic, want strictly greater than %d: a departed agent's suffix was reissued", gamma2, gamma1)
		}
	})
}

// TestDurableNameSuffixesPersistsBeforeIssuing pins the ORDER that makes the
// restart property hold: the floor authorising a suffix is on disk before the
// suffix is returned to anyone. A crash immediately after the return must
// therefore find a floor at or above it.
func TestDurableNameSuffixesPersistsBeforeIssuing(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	for i := 0; i < 5; i++ {
		n, err := store.NextSuffix("alpha")
		if err != nil {
			t.Fatalf("NextSuffix: %v", err)
		}
		// Read the file back through a completely independent load, as a
		// restarting process would.
		floors, existed, rerr := readSuffixFile(filepath.Join(dir, suffixFileName))
		if rerr != nil {
			t.Fatalf("readSuffixFile: %v", rerr)
		}
		if !existed {
			t.Fatalf("after issuing suffix %d the floors file does not exist", n)
		}
		if floors["alpha"] < n {
			t.Fatalf("issued suffix %d but the persisted floor is %d; the suffix was handed out before its floor was durable", n, floors["alpha"])
		}
	}
}

func TestDurableNameSuffixesRefusesUntilSealed(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}

	n, err := store.NextSuffix("alpha")
	if !errors.Is(err, ErrFloorUnproven) {
		t.Fatalf("NextSuffix on an unsealed store = (%d, %v), want an error satisfying errors.Is(err, ErrFloorUnproven)", n, err)
	}
	if n != 0 {
		t.Errorf("refused NextSuffix returned suffix %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, suffixFileName)); !os.IsNotExist(err) {
		t.Errorf("an unsealed store touched the disk: os.Stat = %v, want not-exist", err)
	}

	// A minter over an unsealed store must refuse to mint rather than mint a
	// zero suffix: this is the fail-closed startup behaviour a caller whose
	// floor derivation failed depends on.
	m, err := NewAgentIDMinter(suffixStoreTestBusID, store)
	if err != nil {
		t.Fatalf("NewAgentIDMinter: %v", err)
	}
	if id, err := m.Mint("alpha"); !errors.Is(err, ErrFloorUnproven) {
		t.Fatalf("Mint on an unsealed store = (%q, %v), want ErrFloorUnproven", id, err)
	}
}

// TestDurableNameSuffixesMergesDerivedFloors covers BACKFILL: a data dir whose
// agent ids predate the floors file. The caller derives what it can and raises
// it; the floors in force must be the MAXIMUM of the file and the derivation,
// and a derivation that is too low must never lower a floor already on disk.
func TestDurableNameSuffixesMergesDerivedFloors(t *testing.T) {
	dir := t.TempDir()

	// Run 1: no file at all, caller derives alpha=100 from replay.
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if store.Existed() {
		t.Errorf("Existed() = true on a fresh dir, want false")
	}
	if err := store.RaiseFloor("alpha", 100); err != nil {
		t.Fatalf("RaiseFloor: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	n, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix: %v", err)
	}
	if n != 101 {
		t.Fatalf("first suffix after RaiseFloor(alpha, 100) = %d, want 101", n)
	}

	// Run 2: the file now says 101. A STALE derivation claiming 5 must not
	// lower it — that stale claim is exactly the committed-roster fold that
	// ID-2-WIRING showed is wrong.
	store, err = OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if !store.Existed() {
		t.Errorf("Existed() = false after the file was written, want true")
	}
	// What the file holds is the RESERVED high-water from run 1's block, not
	// run 1's issued suffix 101. Read it rather than hard-coding it, so the
	// assertion below states the actual property — "strictly above whatever
	// floor is persisted" — instead of a number that only holds for one
	// suffixBlockSize.
	persisted := store.Floors()["alpha"]
	if persisted < n {
		t.Fatalf("persisted floor for alpha = %d, want at least the issued suffix %d", persisted, n)
	}
	if err := store.RaiseFloor("alpha", 5); err != nil {
		t.Fatalf("RaiseFloor with a stale value: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	n, err = store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix: %v", err)
	}
	if n != persisted+1 {
		t.Fatalf("after a stale RaiseFloor(alpha, 5) the next suffix was %d, want %d: a stale derivation lowered a persisted floor", n, persisted+1)
	}
}

func TestDurableNameSuffixesSealDiscipline(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("first Seal: %v", err)
	}
	if err := store.Seal(); !errors.Is(err, ErrFloorSealed) {
		t.Errorf("second Seal = %v, want an error satisfying errors.Is(err, ErrFloorSealed)", err)
	}
	if err := store.RaiseFloor("alpha", 9); !errors.Is(err, ErrFloorSealed) {
		t.Errorf("RaiseFloor after Seal = %v, want ErrFloorSealed", err)
	}
	// The refused RaiseFloor must not have taken effect.
	n, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix: %v", err)
	}
	if n != 1 {
		t.Errorf("suffix after a REFUSED RaiseFloor(alpha, 9) = %d, want 1: the refused claim was applied anyway", n)
	}
}

func TestDurableNameSuffixesExhaustedName(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	const max = ^uint64(0)
	if err := store.RaiseFloor("alpha", max); err != nil {
		t.Fatalf("RaiseFloor: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if n, err := store.NextSuffix("alpha"); !errors.Is(err, ErrSuffixExhausted) {
		t.Fatalf("NextSuffix on an exhausted name = (%d, %v), want ErrSuffixExhausted — wrapping to 0 would reissue every id ever minted for the name", n, err)
	}
	// An unrelated name is unaffected.
	if n, err := store.NextSuffix("beta"); err != nil || n != 1 {
		t.Errorf("NextSuffix(beta) = (%d, %v), want (1, nil): one exhausted name must not exhaust the others", n, err)
	}
	// The exhausted floor survives the restart.
	store2, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes after restart: %v", err)
	}
	if got := store2.Floors()["alpha"]; got != max {
		t.Errorf("alpha's persisted floor after restart = %d, want %d", got, max)
	}
}

func TestDurableNameSuffixesConcurrentNextSuffix(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	const goroutines, each = 8, 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[uint64]bool{}

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				n, err := store.NextSuffix("alpha")
				if err != nil {
					t.Errorf("NextSuffix: %v", err)
					return
				}
				mu.Lock()
				if seen[n] {
					t.Errorf("suffix %d issued twice", n)
				}
				seen[n] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != goroutines*each {
		t.Fatalf("issued %d distinct suffixes, want %d", len(seen), goroutines*each)
	}
	// Every issued suffix must be at or below the persisted floor: the floor is
	// written ahead, so a restart resumes above all of them.
	floors, _, err := readSuffixFile(filepath.Join(dir, suffixFileName))
	if err != nil {
		t.Fatalf("readSuffixFile: %v", err)
	}
	for n := range seen {
		if n > floors["alpha"] {
			t.Fatalf("issued suffix %d exceeds the persisted floor %d", n, floors["alpha"])
		}
	}
}

// TestDurableNameSuffixesRejectsCorruptFile pins the fail-closed posture: a
// floors file that does not verify is a fatal startup error, NEVER a silent
// regeneration. Regenerating means resuming every name from 1, which is the
// identity reuse this whole file exists to prevent.
func TestDurableNameSuffixesRejectsCorruptFile(t *testing.T) {
	good := string(mustEncodeSuffixFile(t, map[string]uint64{"alpha": 7, "beta": 3}))
	goodDigest := strings.Fields(strings.SplitN(good, "\n", 2)[0])[2]

	cases := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"header only, no newline", strings.SplitN(good, "\n", 2)[0]},
		{"wrong magic", "not-an-agent-bus-file v3 " + goodDigest + "\nalpha 7\n"},
		{"header has too few fields", suffixFileMagic + " v3\nalpha 7\n"},
		{"unknown newer version", suffixFileMagic + " v999 " + goodDigest + "\nalpha 7\nbeta 3\n"},
		{"digest not hex", suffixFileMagic + " v3 sha256=zzzz\nalpha 7\n"},
		{"digest wrong length", suffixFileMagic + " v3 sha256=00ff\nalpha 7\n"},
		{"digest missing prefix", suffixFileMagic + " v3 deadbeef\nalpha 7\n"},
		{"body tampered: a floor lowered", strings.Replace(good, "alpha 7", "alpha 1", 1)},
		{"body tampered: a name removed", strings.Replace(good, "beta 3\n", "", 1)},
		{"body tampered: a name appended", good + "gamma 5\n"},
		{"entry with no separator", string(encodeSuffixFileRaw("alpha7\n"))},
		{"entry with an illegal name", string(encodeSuffixFileRaw("Alpha 7\n"))},
		{"entry with a dotted name", string(encodeSuffixFileRaw("bus-x.alpha 7\n"))},
		{"entry with a non-numeric suffix", string(encodeSuffixFileRaw("alpha seven\n"))},
		{"entry with a leading zero", string(encodeSuffixFileRaw("alpha 007\n"))},
		{"entry with an explicit zero floor", string(encodeSuffixFileRaw("alpha 0\n"))},
		{"entry with an empty suffix", string(encodeSuffixFileRaw("alpha \n"))},
		{"entry overflowing uint64", string(encodeSuffixFileRaw("alpha 18446744073709551616\n"))},
		{"duplicate name", string(encodeSuffixFileRaw("alpha 7\nalpha 9\n"))},
		{"blank line in the body", string(encodeSuffixFileRaw("alpha 7\n\nbeta 3\n"))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, suffixFileName)
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			store, err := OpenNameSuffixes(dir)
			if err == nil {
				t.Fatalf("OpenNameSuffixes accepted a corrupt floors file and resumed at %v; a corrupt file must be fatal, never regenerated", store.Floors())
			}
			if !errors.Is(err, ErrSuffixFileCorrupt) {
				t.Fatalf("OpenNameSuffixes error = %v, want one satisfying errors.Is(err, ErrSuffixFileCorrupt)", err)
			}
			// The file must be left exactly as found: nothing repairs it.
			after, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("reading back the fixture: %v", rerr)
			}
			if string(after) != tc.content {
				t.Fatalf("the rejected floors file was modified on disk; it must be left for an operator")
			}
		})
	}
}

// encodeSuffixFileRaw builds a floors file with a VALID header and checksum over
// an arbitrary body, so the body-parsing cases above are not merely rediscovering
// the checksum check.
func encodeSuffixFileRaw(body string) []byte {
	// Reuse the real encoder for the header shape by encoding an empty map and
	// then substituting the body plus a recomputed digest.
	var out []byte
	sum := sha256.Sum256([]byte(body))
	out = append(out, fmt.Sprintf("%s v%d sha256=%x\n", suffixFileMagic, suffixFileVersion, sum[:])...)
	out = append(out, body...)
	return out
}

func TestSuffixFileRoundTrip(t *testing.T) {
	in := map[string]uint64{"alpha": 1, "beta": 18446744073709551615, "z9_x-y": 42, "zero": 0}
	dir := t.TempDir()
	path := filepath.Join(dir, suffixFileName)
	if err := os.WriteFile(path, mustEncodeSuffixFile(t, in), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	out, existed, err := readSuffixFile(path)
	if err != nil {
		t.Fatalf("readSuffixFile: %v", err)
	}
	if !existed {
		t.Fatalf("existed = false for a file that exists")
	}
	// A floor of 0 is spelled by ABSENCE, so "zero" must not round-trip.
	want := map[string]uint64{"alpha": 1, "beta": 18446744073709551615, "z9_x-y": 42}
	if len(out) != len(want) {
		t.Fatalf("round-tripped %v, want %v", out, want)
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("floor[%q] = %d, want %d", k, out[k], v)
		}
	}
	// Encoding is canonical: the same map always produces the same bytes.
	if a, b := string(mustEncodeSuffixFile(t, in)), string(mustEncodeSuffixFile(t, out)); a != b {
		t.Errorf("encoding is not canonical:\n%q\n%q", a, b)
	}
}

func TestSuffixFileIsCreatedPrivate(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	fi, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("floors file mode = %o, want 600", perm)
	}
	// No temp files are left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != suffixFileName {
			t.Errorf("stray file %q left in the data dir", e.Name())
		}
	}
}

func TestOpenNameSuffixesRejectsEmptyDir(t *testing.T) {
	if _, err := OpenNameSuffixes(""); err == nil {
		t.Fatal("OpenNameSuffixes(\"\") = nil error, want a failure")
	}
}

// TestNameSuffixesZeroValueDoesNotPanic pins the nil-map trap that the type doc
// used to list as a known, deliberately-unfixed hazard. Both call sites wrote to
// a nil map on a struct-literal allocator; the RaiseFloor one fired during
// STARTUP FLOOR ASSEMBLY, the one window in which the floors are being proven,
// so a panic there took the bus down at exactly the wrong moment.
func TestNameSuffixesZeroValueDoesNotPanic(t *testing.T) {
	t.Run("RaiseFloor on an unsealed zero value", func(t *testing.T) {
		var s NameSuffixes
		if err := s.RaiseFloor("alpha", 5); err != nil {
			t.Fatalf("RaiseFloor on a zero value = %v, want nil", err)
		}
		if err := s.RaiseFloor("alpha", 0); err != nil {
			t.Fatalf("RaiseFloor(alpha, 0) = %v, want nil", err)
		}
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		n, err := s.NextSuffix("alpha")
		if err != nil {
			t.Fatalf("NextSuffix: %v", err)
		}
		if n != 6 {
			t.Errorf("NextSuffix after RaiseFloor(alpha, 5) = %d, want 6", n)
		}
	})

	t.Run("NextSuffix on a sealed zero value", func(t *testing.T) {
		var s NameSuffixes
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		n, err := s.NextSuffix("alpha")
		if err != nil {
			t.Fatalf("NextSuffix on a sealed zero value = (%d, %v), want (1, nil)", n, err)
		}
		if n != 1 {
			t.Errorf("NextSuffix = %d, want 1", n)
		}
		if got := s.LastSuffix("alpha"); got != 1 {
			t.Errorf("LastSuffix = %d, want 1", got)
		}
	})

	t.Run("peekNext mutates nothing", func(t *testing.T) {
		s := ResumeNameSuffixes(map[string]uint64{"alpha": 10})
		if err := s.Seal(); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		for i := 0; i < 3; i++ {
			n, err := s.peekNext("alpha")
			if err != nil {
				t.Fatalf("peekNext: %v", err)
			}
			if n != 11 {
				t.Fatalf("peekNext call %d = %d, want 11 every time: peeking issued a number", i, n)
			}
		}
		if got := s.LastSuffix("alpha"); got != 0 {
			t.Errorf("LastSuffix after peeking = %d, want 0", got)
		}
		n, err := s.NextSuffix("alpha")
		if err != nil || n != 11 {
			t.Errorf("NextSuffix after peeking = (%d, %v), want (11, nil)", n, err)
		}
	})

	t.Run("peekNext refuses on an unsealed allocator", func(t *testing.T) {
		s := ResumeNameSuffixes(nil)
		if n, err := s.peekNext("alpha"); !errors.Is(err, ErrFloorUnproven) || n != 0 {
			t.Errorf("peekNext unsealed = (%d, %v), want (0, ErrFloorUnproven)", n, err)
		}
	})
}

// TestDurableNameSuffixesGapAfterPersistBeforeIssue simulates a crash landing
// exactly where NextSuffix's doc says one is safe to land: AFTER the floor is
// persisted and fsynced but BEFORE the suffix is returned to anyone. It is
// simulated directly, by writing a floors file that is AHEAD of anything this
// process (or any previous one) ever actually issued — precisely what disk
// would show if the process died in that window. Recovery must resume STRICTLY
// ABOVE the persisted floor, and the resulting gap (the burned-but-never-issued
// numbers) is CORRECT and must never be compacted (point 4 of the NameSuffixes
// doc).
func TestDurableNameSuffixesGapAfterPersistBeforeIssue(t *testing.T) {
	dir := t.TempDir()

	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Three suffixes are genuinely issued and returned: 1, 2, 3.
	var last uint64
	for i := 0; i < 3; i++ {
		n, err := store.NextSuffix("alpha")
		if err != nil {
			t.Fatalf("NextSuffix: %v", err)
		}
		last = n
	}
	if last != 3 {
		t.Fatalf("issued suffixes ended at %d, want 3", last)
	}

	// Simulate the crash window: overwrite the floors file with a floor far
	// ahead of anything issued, as if NextSuffix had persisted floor 10 and the
	// process died before returning it to anyone. 4..9 are burned, unaccounted
	// for, and permanently unavailable — a gap, not a bug.
	path := filepath.Join(dir, suffixFileName)
	if err := os.WriteFile(path, mustEncodeSuffixFile(t, map[string]uint64{"alpha": 10}), 0o600); err != nil {
		t.Fatalf("simulating a crashed persist: %v", err)
	}

	// Recovery: a fresh allocator over the same dir must resume strictly above
	// the persisted floor, not above the last suffix truly issued (3).
	store2, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes after simulated crash: %v", err)
	}
	if got := store2.Floors()["alpha"]; got != 10 {
		t.Fatalf("Floors()[alpha] after simulated crash = %d, want 10", got)
	}
	if err := store2.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	n, err := store2.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix after simulated crash: %v", err)
	}
	if n != 11 {
		t.Fatalf("first suffix after a crashed persist at floor 10 = %d, want 11: recovery must resume strictly above the persisted floor, leaving 4..10 as a permanent gap, not resume above the last suffix (3) that was actually returned", n)
	}
}

// TestDurableNameSuffixesWriteFailureIssuesNothing covers the failure half of
// the write-ahead contract: a persist failure must issue NOTHING, must not be
// remembered as durable, and a later successful call must land on the SAME
// number the failed attempt would have — never skip it, never reuse a number
// that a sibling name or an earlier call already issued.
//
// The data dir is made unwritable with os.Chmod(dir, 0o500) so the temp-file
// creation inside atomicWriteFile fails with a permission error; the test is
// skipped when running as root, where directory permissions do not block
// writes.
func TestDurableNameSuffixesWriteFailureIssuesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not block writes")
	}

	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// alpha issues once successfully before the dir goes read-only, so there is
	// a real prior suffix on disk to check for skip/reuse against.
	n1, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix before failure: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("first suffix for alpha = %d, want 1", n1)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	// Always restore write access, even on failure, so t.TempDir() cleanup can
	// remove the directory afterwards.
	defer os.Chmod(dir, 0o700)

	n, err := store.NextSuffix("beta")
	if err == nil {
		t.Fatalf("NextSuffix on an unwritable data dir = (%d, nil), want an error", n)
	}
	if n != 0 {
		t.Errorf("failed NextSuffix returned suffix %d, want 0", n)
	}
	if got := store.Floors()["beta"]; got != 0 {
		t.Errorf("Floors()[beta] after a failed persist = %d, want 0: a failed write must not be remembered as durable", got)
	}
	// alpha's own last-issued suffix must be untouched by beta's unrelated
	// failure.
	if got := store.LastSuffix("alpha"); got != 1 {
		t.Errorf("LastSuffix(alpha) after an unrelated failure for beta = %d, want 1", got)
	}
	// No temp file survives a CreateTemp failure: there is nothing to leave
	// behind, since the failure is at file creation, before any bytes are
	// written. Assert the dir holds only the real floors file already written
	// for alpha.
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("ReadDir under the unwritable dir: %v", rerr)
	}
	for _, e := range entries {
		if e.Name() != suffixFileName {
			t.Errorf("stray file %q left in the data dir after a failed write", e.Name())
		}
	}

	// Restore write access and retry: the failed attempt must not have burned
	// beta's suffix 1, so the retry must land on 1, not skip to 2.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod restore: %v", err)
	}
	n2, err := store.NextSuffix("beta")
	if err != nil {
		t.Fatalf("NextSuffix after restoring write access: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("suffix for beta after a failed-then-retried NextSuffix = %d, want 1: the failed attempt must not have skipped or burned a number", n2)
	}

	// A fully independent restart over the same dir must show exactly the
	// history that actually landed on disk — alpha=1, beta=1 — and must never
	// reissue either across the whole run.
	store2, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes after restart: %v", err)
	}
	// Each name that issued reserved a block, so disk holds suffixBlockSize for
	// both — a floor ABOVE the suffix 1 each of them actually handed out. Higher
	// is the safe direction: it can only skip numbers, never reissue one.
	floors := store2.Floors()
	if floors["alpha"] != suffixBlockSize {
		t.Errorf("persisted floor[alpha] after restart = %d, want %d (the reserved block high-water)", floors["alpha"], suffixBlockSize)
	}
	if floors["beta"] != suffixBlockSize {
		t.Errorf("persisted floor[beta] after restart = %d, want %d (the reserved block high-water)", floors["beta"], suffixBlockSize)
	}
	if err := store2.Seal(); err != nil {
		t.Fatalf("Seal after restart: %v", err)
	}
	n3, err := store2.NextSuffix("beta")
	if err != nil {
		t.Fatalf("NextSuffix after restart: %v", err)
	}
	if n3 != suffixBlockSize+1 {
		t.Fatalf("beta after restart = %d, want %d: the retry's suffix 1 must never be reissued across the whole history", n3, suffixBlockSize+1)
	}
	n4, err := store2.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix after restart: %v", err)
	}
	if n4 != suffixBlockSize+1 {
		t.Fatalf("alpha after restart = %d, want %d: alpha's suffix 1 must never be reissued across the whole history", n4, suffixBlockSize+1)
	}
	// The claim that actually matters, stated independently of any block
	// arithmetic: nothing issued after the restart may collide with anything
	// issued before it.
	for name, before := range map[string]uint64{"alpha": n1, "beta": n2} {
		after := map[string]uint64{"alpha": n4, "beta": n3}[name]
		if after <= before {
			t.Fatalf("%s issued %d before the restart and %d after: a suffix was reissued", name, before, after)
		}
	}
}

// TestDurableNameSuffixesFloorsIsACopy pins Floors' doc: the returned map is a
// snapshot, and mutating it must not perturb the allocator's real floors.
func TestDurableNameSuffixesFloorsIsACopy(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := store.NextSuffix("alpha"); err != nil {
		t.Fatalf("NextSuffix: %v", err)
	}

	got := store.Floors()
	got["alpha"] = 999
	got["intruder"] = 1

	real := store.Floors()
	if real["alpha"] != suffixBlockSize {
		t.Errorf("mutating the map returned by Floors() affected the allocator: floor[alpha] = %d, want %d (the reserved block high-water)", real["alpha"], suffixBlockSize)
	}
	if _, present := real["intruder"]; present {
		t.Errorf("mutating the map returned by Floors() injected a new name into the allocator")
	}

	n, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix after mutating a returned Floors() map: %v", err)
	}
	if n != 2 {
		t.Fatalf("NextSuffix after mutating a returned Floors() map = %d, want 2: an external mutation of the snapshot must not perturb the real floor", n)
	}
}

// TestDurableNameSuffixesExistedReflectsFilePresenceNotContent pins Existed's
// contract precisely: it tracks whether the FILE was present at open, not
// whether it holds any floor. Sealing with nothing raised writes a file with
// an empty body, and a subsequent open must still report Existed() = true.
func TestDurableNameSuffixesExistedReflectsFilePresenceNotContent(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if store.Existed() {
		t.Fatalf("Existed() = true before the file is ever written")
	}
	// Seal with nothing raised: the file is written with an EMPTY floor map.
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("Stat after Seal: %v", err)
	}

	store2, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes after Seal: %v", err)
	}
	if !store2.Existed() {
		t.Errorf("Existed() = false on a dir whose floors file was written but is empty, want true")
	}
	if len(store2.Floors()) != 0 {
		t.Errorf("Floors() on a freshly-sealed-empty file = %v, want empty", store2.Floors())
	}
}

// TestDurableNameSuffixesLastSuffixIsPerProcessNotPerDisk pins the distinction
// LastSuffix's doc draws: it answers "what did THIS allocator hand out",
// never "what is burned on disk" (that is Floors). A freshly-opened allocator
// over a dir with a non-zero persisted floor must still report LastSuffix = 0
// until it issues something itself, both before and immediately after Seal.
func TestDurableNameSuffixesLastSuffixIsPerProcessNotPerDisk(t *testing.T) {
	dir := t.TempDir()

	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := store.LastSuffix("alpha"); got != 0 {
		t.Fatalf("LastSuffix on a freshly-sealed store = %d, want 0", got)
	}
	n1, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix: %v", err)
	}
	if got := store.LastSuffix("alpha"); got != n1 {
		t.Fatalf("LastSuffix after issuing = %d, want %d", got, n1)
	}

	// A fresh allocator over the SAME dir has issued nothing itself yet, even
	// though the persisted FLOOR is non-zero.
	store2, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes after restart: %v", err)
	}
	if got := store2.LastSuffix("alpha"); got != 0 {
		t.Errorf("LastSuffix on a freshly-opened, unsealed store = %d, want 0", got)
	}
	// Floors() reports what DISK says, which since the block reservation is the
	// reserved high-water and therefore strictly above the single suffix that
	// was actually issued. The write-ahead property is the >= ; the exact value
	// pins the block.
	if got := store2.Floors()["alpha"]; got < n1 {
		t.Fatalf("Floors()[alpha] after restart = %d, want at least the issued suffix %d: the floor authorising a suffix must be durable before it is handed out", got, n1)
	}
	if got := store2.Floors()["alpha"]; got != suffixBlockSize {
		t.Fatalf("Floors()[alpha] after restart = %d, want %d (the reserved block high-water, not the issued suffix %d)", got, suffixBlockSize, n1)
	}
	if err := store2.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got := store2.LastSuffix("alpha"); got != 0 {
		t.Errorf("LastSuffix right after Seal (nothing issued yet in this process) = %d, want 0", got)
	}
	n2, err := store2.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix after restart: %v", err)
	}
	if got := store2.LastSuffix("alpha"); got != n2 {
		t.Errorf("LastSuffix after issuing in the new process = %d, want %d", got, n2)
	}
	if n2 <= n1 {
		t.Fatalf("suffix after restart = %d, want strictly greater than %d", n2, n1)
	}
}
