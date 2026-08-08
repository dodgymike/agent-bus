package main

// Tests for the data-directory permission gate.
//
// # What it is for, and why the per-file modes did not already cover it
//
// Every identity file in the data directory is written 0600 — the bus id, the
// agent-suffix floors, the WAL MAC key, the WAL index floor, the message
// sequence floor. That mode governs who may OPEN those files. It does not govern
// who may REPLACE them: unlinking a file and creating another in its place, or
// renaming over it, are permissions on the CONTAINING DIRECTORY. So a data
// directory that is group- or other-writable hands every local user the ability
// to substitute any identity file in it, whatever mode the file itself carries.
//
// That is not hypothetical here. It is exactly the privilege the forged
// message-seq-floor exploit needs (see seqfloorforge_test.go), and it defeats
// per-file hardening by construction, which is why the fix lives at the
// directory rather than being repeated once per file.
//
// # Why a pre-existing directory is the only case that can fail
//
// run() calls os.MkdirAll(dir, 0o700), and a umask can only CLEAR permission
// bits, never set them — so a directory this binary created is at most 0700 and
// can never trip this gate. MkdirAll on an ALREADY-EXISTING directory does not
// chmod it at all: it returns nil and leaves 0777 as 0777. Every fixture below
// therefore pre-creates the directory, because that is the only shape in which
// the defect exists.
//
// mkdirMode is used rather than t.TempDir() alone for the same reason: TempDir
// hands back a 0700 directory, and the mode has to be set with an explicit
// Chmod because os.Mkdir's mode argument is masked by the umask.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// msgDataDirTightened is the stderr line the gate emits when it corrects a
// group-writable directory. It is asserted rather than assumed: the line is the
// ONLY surviving record that the directory was ever exposed, so renaming it
// silently is the defect, exactly as for msgWALQuarantined.
//
// Note the deliberately UNCLOSED quote. The gate's message continues past the
// headline into the explanation an operator needs, so logfmt renders it as
// msg="data directory permissions tightened: it was writable…" — a constant
// with a closing quote would never match, and the test would fail against a
// gate that was working perfectly.
const msgDataDirTightened = `msg="data directory permissions tightened`

// mkdirMode creates a subdirectory of the test's temp dir with EXACTLY mode,
// chmodding after Mkdir so the umask cannot quietly clear the bit under test.
func mkdirMode(t *testing.T, name string, mode fs.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("chmod %#o %s: %v", mode, dir, err)
	}
	if got := statMode(t, dir); got != mode {
		t.Fatalf("fixture %s is mode %#o, want %#o; the test would prove nothing", dir, got, mode)
	}
	return dir
}

func statMode(t *testing.T, dir string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	return info.Mode().Perm()
}

// TestRunRefusesAnOtherWritableDataDir is the security claim at its narrowest: a
// world-writable data directory is a REFUSAL, not a warning.
//
// The refusal (rather than a silent tighten) is the deliberate call recorded in
// DECISIONS.md. A directory that was world-writable may ALREADY have had an
// identity file replaced, and adopting a forged one is silent and undetectable —
// so the operator has to be stopped and told, not merely informed in a line that
// scrolls past.
func TestRunRefusesAnOtherWritableDataDir(t *testing.T) {
	dir := mkdirMode(t, "world-writable", 0o777)

	proc := startServer(t, dir)
	code := proc.awaitExit(t, startupTimeout)
	if code != 1 {
		t.Fatalf("exit code on a 0777 data dir = %d, want 1 (the server must refuse)\n%s", code, proc.stderr())
	}

	stderr := proc.stderr()
	for _, want := range []string{
		dir,                // WHICH directory
		"0777",             // the mode it actually has, not a generic complaint
		"chmod 700 " + dir, // the remedy, runnable as printed
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("the refusal does not mention %q; an operator cannot act on it.\nstderr:\n%s", want, stderr)
		}
	}

	// It must REFUSE, not tighten-and-refuse: leaving the mode alone keeps the
	// evidence intact for whoever investigates, and a half-action would let a
	// second start succeed silently over a directory that may already have been
	// tampered with.
	if got := statMode(t, dir); got != 0o777 {
		t.Fatalf("the refused start changed the data dir mode to %#o; it must leave 0777 alone for the operator to see", got)
	}

	// Nothing may have been created in it either. A refusal that has already
	// minted a bus id or taken the lock is a refusal that ran too late.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the refused start wrote %v into the data dir; the permission gate must run before anything touches it", names)
	}
}

// TestRunTightensAGroupWritableDataDir pins the OTHER half of the decision:
// group-writable is tightened to 0700 and warned about, not refused.
//
// The asymmetry is deliberate and is argued in DECISIONS.md: the set of
// principals a group grants is bounded and administratively chosen, the benign
// cause (a umask-002 `mkdir data`, or an ops group on the deployment) dominates,
// and refusing would brick working buses on upgrade over a condition that a
// chmod fully removes. "Other" grants an unbounded set and has no benign cause.
func TestRunTightensAGroupWritableDataDir(t *testing.T) {
	dir := mkdirMode(t, "group-writable", 0o770)

	proc := startServer(t, dir)
	proc.awaitServerStarted(t)

	// The warning is operator contract, not decoration: it is the only record
	// that the directory was exposed at all, since the mode itself is about to
	// be corrected out of existence.
	line := proc.line(t, msgDataDirTightened)
	for _, want := range []string{dir, "0770", "0700"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the tighten warning does not mention %q: %s", want, line)
		}
	}
	if !strings.Contains(line, "level=warn") {
		t.Fatalf("the tighten notice must be WARN, not lower — it is a security event: %s", line)
	}

	if got := statMode(t, dir); got != 0o700 {
		t.Fatalf("data dir mode after start = %#o, want 0700; the warning claimed it was tightened", got)
	}

	proc.signal(t, syscall.SIGTERM)
	if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
		t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
	}
}

// TestRunAcceptsAPrivateDataDirSilently is the false-positive guard. Without it
// the gate could be "refuse everything" and both tests above would still pass.
func TestRunAcceptsAPrivateDataDirSilently(t *testing.T) {
	for _, mode := range []fs.FileMode{0o700, 0o750, 0o755} {
		mode := mode
		t.Run(fmt.Sprintf("%#o", mode), func(t *testing.T) {
			dir := mkdirMode(t, "private", mode)

			proc := startServer(t, dir)
			proc.awaitServerStarted(t)

			for _, l := range proc.snapshot() {
				if strings.Contains(l, msgDataDirTightened) {
					t.Fatalf("a %#o data dir was tightened; only GROUP- or OTHER-WRITABLE modes may be touched: %s", mode, l)
				}
			}
			if got := statMode(t, dir); got != mode {
				t.Fatalf("data dir mode changed from %#o to %#o; a directory that is not writable by others must be left exactly as the operator set it", mode, got)
			}

			proc.signal(t, syscall.SIGTERM)
			if code := proc.awaitExit(t, shutdownTimeout); code != 0 {
				t.Fatalf("exit code after SIGTERM = %d, want 0\n%s", code, proc.stderr())
			}
		})
	}
}
