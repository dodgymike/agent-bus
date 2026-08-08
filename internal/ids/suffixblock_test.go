package ids

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Block reservation (Spec Server 3e46d43b): NextSuffix reserves suffixBlockSize
// numbers in one durable write and issues them from memory.
//
// The claim under test, in one line: NO SUFFIX IS EVER RETURNED BEFORE A FLOOR
// AT LEAST THAT HIGH IS DURABLE — the amortisation makes the durable floor
// HIGHER than the issued one, never lower.
//
// That direction is the whole safety argument. A floor that is too high only
// skips numbers, and skipped numbers are correct (point 4 of the NameSuffixes
// doc). A floor that is too low is agent-id reuse, and because the agent id is
// the routing AND authorization subject, reuse hands a new keypair an identity
// this bus already wrote down. "The code looks right" is not evidence for that,
// so the property is proved two ways here: by making a write IMPOSSIBLE and
// showing what still succeeds and what correctly fails, and by really SIGKILLing
// a real process mid-block.

const (
	envIDsCrashDir     = "IDS_SUFFIX_CRASH_DIR"
	envIDsCrashJournal = "IDS_SUFFIX_CRASH_JOURNAL"
	envIDsCrashPlan    = "IDS_SUFFIX_CRASH_PLAN"
)

// TestDurableNameSuffixesReservesABlock proves the amortisation WITHOUT counting
// anything, by removing the ability to write.
//
// A counter in the production struct would prove "few writes"; making the data
// dir unwritable proves the stronger and more useful pair of facts:
//
//	inside a reserved block  -> mints succeed with the disk unavailable
//	at the block boundary    -> the mint FAILS rather than issuing an
//	                            unrecorded number
//
// The second half is the security-relevant one. An amortisation that kept
// issuing from memory when it could no longer persist would be exactly the
// "handed out a suffix a crash could leave unrecorded" defect the task warns
// about, and it would look identical to a correct one on every happy-path test.
func TestDurableNameSuffixesReservesABlock(t *testing.T) {
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

	// The first mint reserves the block. It is the only one in this test that
	// is allowed to touch the disk.
	n, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("first NextSuffix: %v", err)
	}
	if n != 1 {
		t.Fatalf("first suffix = %d, want 1", n)
	}
	if got := store.Floors()["alpha"]; got != suffixBlockSize {
		t.Fatalf("persisted floor after the first mint = %d, want %d: one write must reserve the WHOLE block", got, suffixBlockSize)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	// Always restore, even on failure, so t.TempDir() cleanup can remove it.
	defer os.Chmod(dir, 0o700)

	// The rest of the block issues from memory with the disk unavailable.
	for want := uint64(2); want <= suffixBlockSize; want++ {
		got, err := store.NextSuffix("alpha")
		if err != nil {
			t.Fatalf("NextSuffix #%d with an unwritable data dir: %v; every suffix inside a reserved block must come from memory", want, err)
		}
		if got != want {
			t.Fatalf("NextSuffix #%d = %d, want %d", want, got, want)
		}
	}

	// The block is used up. The next one needs a durable floor it cannot write,
	// so it must REFUSE rather than issue suffixBlockSize+1 unrecorded.
	over, err := store.NextSuffix("alpha")
	if err == nil {
		t.Fatalf("NextSuffix past the end of the block returned (%d, nil) with an unwritable data dir; that suffix is not covered by any persisted floor, so a crash would reissue it", over)
	}
	if over != 0 {
		t.Errorf("failed NextSuffix returned suffix %d, want 0", over)
	}
	if got := store.LastSuffix("alpha"); got != suffixBlockSize {
		t.Errorf("LastSuffix after the refused mint = %d, want %d: a refusal must not advance the counter", got, suffixBlockSize)
	}

	// With writing restored the number is handed out normally — it was never
	// burned, only deferred — and a fresh block is reserved.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod restore: %v", err)
	}
	next, err := store.NextSuffix("alpha")
	if err != nil {
		t.Fatalf("NextSuffix after restoring write access: %v", err)
	}
	if next != suffixBlockSize+1 {
		t.Fatalf("suffix after restoring write access = %d, want %d: the refused attempt must not have skipped a number", next, suffixBlockSize+1)
	}
	if got := store.Floors()["alpha"]; got != 2*suffixBlockSize {
		t.Fatalf("persisted floor after the second block = %d, want %d", got, 2*suffixBlockSize)
	}
}

// TestDurableNameSuffixesBlockSaturatesAtMaxUint64 pins the one arithmetic that
// could invert the safety direction: n + suffixBlockSize - 1 overflowing.
//
// A wrapped high-water would be BELOW the suffix being issued — a floor that
// says less than what was handed out, which is the reuse this type exists to
// prevent, arrived at by addition rather than by a missing write.
func TestDurableNameSuffixesBlockSaturatesAtMaxUint64(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes: %v", err)
	}
	// Park the floor five short of the top, so reserving a block of 64 must
	// overflow.
	const near = math.MaxUint64 - 5
	if err := store.RaiseFloor("alpha", near); err != nil {
		t.Fatalf("RaiseFloor: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	for want := uint64(near + 1); want > near; want++ {
		got, err := store.NextSuffix("alpha")
		if err != nil {
			t.Fatalf("NextSuffix for %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("NextSuffix = %d, want %d", got, want)
		}
		if floor := store.Floors()["alpha"]; floor < got {
			t.Fatalf("issued %d but the persisted floor is %d; the block arithmetic wrapped and the floor is now BELOW an issued suffix", got, floor)
		}
		if want == math.MaxUint64 {
			break
		}
	}

	if got := store.Floors()["alpha"]; got != math.MaxUint64 {
		t.Fatalf("persisted floor = %d, want MaxUint64: the reservation must saturate, never wrap", got)
	}
	// The name is now genuinely finished, and says so rather than wrapping to 0.
	n, err := store.NextSuffix("alpha")
	if !errors.Is(err, ErrSuffixExhausted) {
		t.Fatalf("NextSuffix at MaxUint64 = (%d, %v), want ErrSuffixExhausted", n, err)
	}
	if n != 0 {
		t.Errorf("exhausted NextSuffix returned %d, want 0", n)
	}
}

// TestDurableNameSuffixesBlockCrashInjection is the crash-injection proof the
// block reservation owes: a real process mints, is really SIGKILLed mid-block,
// and the surviving data dir is asked what it will issue next.
//
// The interesting instant is precisely the one blocking introduces. A process
// dies holding a reserved block it has only partly issued; the unissued
// remainder is burned on disk and nothing in memory survives to say which
// numbers were handed out. Recovery must resume above the WHOLE block, not
// above the last suffix issued — it has no way to know the latter, which is
// exactly why the floor is written ahead.
func TestDurableNameSuffixesBlockCrashInjection(t *testing.T) {
	type round struct {
		name string
		plan string
	}
	// Each round is a fresh process that mints and then dies. They share one
	// data dir, so the assertions accumulate across four real crashes.
	rounds := []round{
		{name: "seal only, no mint", plan: ""},
		{name: "one suffix of a fresh block", plan: "alpha:1"},
		{name: "part of a block, two names", plan: "alpha:3,beta:5"},
		{name: "more than one whole block", plan: "alpha:" + strconv.Itoa(suffixBlockSize+2)},
	}

	dir := t.TempDir()
	journal := filepath.Join(t.TempDir(), "issued.journal")

	issued := map[string][]uint64{}
	seen := map[string]map[uint64]bool{}

	for i, r := range rounds {
		runIDsCrashChild(t, dir, journal, r.plan)

		// What the dead process actually handed out, as it recorded it (fsynced
		// after every mint) before dying.
		got := readIssuedJournal(t, journal)

		// Every number the child returned must be covered by a floor that is on
		// disk NOW — this is the write-ahead property, observed after a real
		// kill rather than argued.
		floors, existed, err := readSuffixFile(filepath.Join(dir, suffixFileName))
		if err != nil {
			t.Fatalf("round %d (%s): readSuffixFile after the crash: %v", i, r.name, err)
		}
		if !existed {
			t.Fatalf("round %d (%s): no floors file after the crash; Seal must persist before anything may issue", i, r.name)
		}
		for name, ns := range got {
			for _, n := range ns {
				if floors[name] < n {
					t.Fatalf("round %d (%s): the child was issued %s-%d but the surviving floor is only %d; a suffix was handed out that a crash left unrecorded", i, r.name, name, n, floors[name])
				}
				if seen[name] == nil {
					seen[name] = map[uint64]bool{}
				}
				if seen[name][n] {
					t.Fatalf("round %d (%s): suffix %s-%d was issued TWICE across crashes; the previous holder's routing and authorization identity was handed to a new agent", i, r.name, name, n)
				}
				seen[name][n] = true
				issued[name] = append(issued[name], n)
			}
		}
	}

	// Finally, a surviving process opens the same dir and mints again. Nothing
	// it issues may collide with anything any dead child was told.
	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("OpenNameSuffixes after the crashes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("Seal after the crashes: %v", err)
	}
	for name, ns := range issued {
		if len(ns) == 0 {
			continue
		}
		var high uint64
		for _, n := range ns {
			if n > high {
				high = n
			}
		}
		for k := 0; k < 3; k++ {
			n, err := store.NextSuffix(name)
			if err != nil {
				t.Fatalf("NextSuffix(%q) after the crashes: %v", name, err)
			}
			if n <= high {
				t.Fatalf("after four crashes %s was minted suffix %d, but %d was already issued to a process that died: a restarted bus reissued an agent id", name, n, high)
			}
			if seen[name][n] {
				t.Fatalf("after four crashes %s was minted suffix %d, which a dead child had already been told", name, n)
			}
			seen[name][n] = true
		}
	}
}

// TestIDsSuffixCrashChild is the child half of the crash injection. It is a
// no-op unless the parent sets its environment, and it never returns normally
// when it is one: it SIGKILLs itself, so a clean exit means the crash was not
// injected and the parent's assertions would be testing nothing.
func TestIDsSuffixCrashChild(t *testing.T) {
	dir := os.Getenv(envIDsCrashDir)
	if dir == "" {
		t.Skip("not a crash child: " + envIDsCrashDir + " is unset")
	}
	journalPath := os.Getenv(envIDsCrashJournal)
	if journalPath == "" {
		t.Fatalf("child: %s is set but %s is empty", envIDsCrashDir, envIDsCrashJournal)
	}

	// Truncate: the parent reads this file after every round and wants only the
	// suffixes THIS child was issued.
	journal, err := os.OpenFile(journalPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("child: opening the journal: %v", err)
	}

	store, err := OpenNameSuffixes(dir)
	if err != nil {
		t.Fatalf("child: OpenNameSuffixes: %v", err)
	}
	if err := store.Seal(); err != nil {
		t.Fatalf("child: Seal: %v", err)
	}

	for _, item := range strings.Split(os.Getenv(envIDsCrashPlan), ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, countPart, ok := cutOnce(item, ":")
		if !ok {
			t.Fatalf("child: malformed plan item %q, want <name>:<count>", item)
		}
		count, cerr := strconv.Atoi(countPart)
		if cerr != nil {
			t.Fatalf("child: malformed count in %q: %v", item, cerr)
		}
		for i := 0; i < count; i++ {
			n, err := store.NextSuffix(name)
			if err != nil {
				t.Fatalf("child: NextSuffix(%q) #%d: %v", name, i, err)
			}
			// Record what was HANDED OUT, and fsync it, before doing anything
			// else. This file is the parent's only witness to what the dead
			// process was told, so it must be at least as durable as the claim
			// it is used to check.
			if _, werr := fmt.Fprintf(journal, "%s %d\n", name, n); werr != nil {
				t.Fatalf("child: writing the journal: %v", werr)
			}
			if serr := journal.Sync(); serr != nil {
				t.Fatalf("child: fsyncing the journal: %v", serr)
			}
		}
	}

	idsSuicide()
}

// cutOnce is strings.Cut, which this module's Go version predates.
func cutOnce(s, sep string) (before, after string, found bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// idsSuicide SIGKILLs this process. SIGKILL specifically: it cannot be caught,
// deferred, or handled, so no cleanup runs and nothing gets a last chance to
// flush. That is the point — anything that survives it survived because it was
// already on the platter.
func idsSuicide() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	// Unreachable. If it is ever reached the crash was not injected and the
	// parent must not be allowed to treat the run as evidence.
	time.Sleep(5 * time.Second)
	panic("ids crash test: SIGKILL to self did not kill the process")
}

// runIDsCrashChild re-execs this test binary and PROVES it died on SIGKILL.
//
// The check is not ceremony. Without it, a child that t.Fatalf'd on its first
// line would exit 1, leave a floors file that no suffix was ever issued from,
// and every "no suffix was reissued" assertion in the parent would pass for the
// wrong reason — the classic vacuous proof.
func runIDsCrashChild(t *testing.T, dir, journal, plan string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, self, "-test.run=^TestIDsSuffixCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		envIDsCrashDir+"="+dir,
		envIDsCrashJournal+"="+journal,
		envIDsCrashPlan+"="+plan,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash child (plan %q) did not finish in time: %v\n--- child output ---\n%s", plan, ctx.Err(), out.String())
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("crash child (plan %q): Run returned %v, want an *exec.ExitError from a signalled death\n--- child output ---\n%s", plan, err, out.String())
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("crash child (plan %q): wait status is %T, want syscall.WaitStatus", plan, ee.Sys())
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("crash child (plan %q) exited with status %d instead of dying on SIGKILL; the crash was never injected, so nothing below is being tested\n--- child output ---\n%s",
			plan, ws.ExitStatus(), out.String())
	}
}

// readIssuedJournal returns the suffixes the dead child recorded as issued.
func readIssuedJournal(t *testing.T, path string) map[string][]uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the issued journal: %v", err)
	}
	defer f.Close()

	out := map[string][]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		name, numPart, ok := cutOnce(line, " ")
		if !ok {
			t.Fatalf("malformed journal line %q", line)
		}
		n, perr := strconv.ParseUint(numPart, 10, 64)
		if perr != nil {
			t.Fatalf("malformed journal line %q: %v", line, perr)
		}
		out[name] = append(out[name], n)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the issued journal: %v", err)
	}
	return out
}
