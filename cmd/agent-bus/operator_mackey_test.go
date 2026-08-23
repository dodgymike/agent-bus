package main

// Guard against `agent-bus operator list` — a READ-ONLY command — minting
// wal-mac.key as a side effect of the read.
//
// wal's macKeyFor CREATES wal-mac.key whenever a replay reaches it with the key
// missing, and it does so SILENTLY (wal.Replay takes no logger). Integrity in
// this project is a keyed MAC (invariant 6): a read-only inspection command that
// mints the key manufactures the authority to authenticate the very log it is
// about to judge. A key minted now verifies nothing written under the real one,
// and it converts a recoverable wal.ErrMACKeyMissing (remedy: restore the key)
// into wal.ErrMACKeyMismatch, whose documented remedy is to move bus.wal aside.
// A minted key is also NOT a considered key lifecycle (invariant 9): key
// material must never be created as an accident of a list command.
//
// openOperatorRegistry's read-only path (!writable) closes this with a pre-lock
// and a post-lock checkMACKeyPresent (via operatorMACKeyGuard).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/dirlock"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// operatorMintingWALs are the ONLY bus.wal shapes for which wal.Replay reaches
// macKeyFor and MINTS wal-mac.key.
//
// This is established empirically (see the control test below), not guessed, and
// the negative result is the dangerous half: an ABSENT or ZERO-LENGTH bus.wal
// takes Replay's early empty-log exit and mints NOTHING, so a guard test built
// on either shape CANNOT FAIL and would be a dead guard. Each shape here is one
// whose header does not positively identify itself as format version 2, which is
// what makes wal's macKeyMayBeCreated permit creation.
var operatorMintingWALs = []struct {
	name  string
	bytes []byte
}{
	{
		name:  "non-empty with garbage magic",
		bytes: []byte("NOTAWALFILE-BUT-LONG-ENOUGH-TO-HAVE-A-HEADER"),
	},
	{
		name:  "a three-byte truncated header",
		bytes: []byte("AGN"),
	},
}

// operatorDirEntries is the sorted file list of a data directory.
func operatorDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestOperatorListMACKeyGuard is the guard that keeps a read-only inspection
// command from destroying the artefact it was asked about.
//
// Without the pre-lock and post-lock guards in openOperatorRegistry, `operator
// list` would replay the log, wal would mint wal-mac.key, and "restore a
// 64-byte file" would become "the key does not match, move bus.wal aside".
//
// The refusal is PRE-LOCK, so it writes NOTHING AT ALL: not the key, and not the
// bus.lock whose mere presence makes an operator's next real start refuse to
// boot.
func TestOperatorListMACKeyGuard(t *testing.T) {
	for _, shape := range operatorMintingWALs {
		shape := shape
		for _, asJSON := range []bool{false, true} {
			asJSON := asJSON
			name := shape.name
			if asJSON {
				name += " (-json)"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				if _, err := ids.LoadOrCreateBusID(dir, "bus-optest"); err != nil {
					t.Fatalf("ids.LoadOrCreateBusID(%s): %v", dir, err)
				}
				walPath := filepath.Join(dir, wal.WALFileName)
				if err := os.WriteFile(walPath, shape.bytes, 0o600); err != nil {
					t.Fatalf("writing the fixture %s: %v", wal.WALFileName, err)
				}
				keyPath := filepath.Join(dir, wal.MACKeyFileName)
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("the fixture already holds a %s (stat err = %v), so this test asserts nothing", wal.MACKeyFileName, err)
				}

				args := []string{"list", "-data-dir", dir}
				if asJSON {
					args = append(args, "-json")
				}
				code, stdout, stderr := runOperator(t, args...)

				// THE ASSERTION THE TEST EXISTS FOR, CHECKED FIRST ON PURPOSE.
				// Removing the guard makes this command answer exit 1 instead of
				// 6, so an exit-code assertion placed above this one would fail
				// first and the mint would never be looked at.
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("the refusal MINTED %s (stat err = %v). A read-only command has manufactured the key that authenticates "+
						"the log it was asked to judge\n  exit was %d\n  stdout: %s\n  stderr: %s",
						wal.MACKeyFileName, err, code, stdout, stderr)
				}
				if code != exitOperatorUnverifiable {
					t.Fatalf("exit = %d, want %d (UNVERIFIABLE)\nstdout: %s\nstderr: %s",
						code, exitOperatorUnverifiable, stdout, stderr)
				}
				// The guard is PRE-LOCK, so the refusal wrote NOTHING AT ALL.
				if _, err := os.Stat(filepath.Join(dir, dirlock.LockFileName)); !os.IsNotExist(err) {
					t.Fatalf("the refusal left %s behind (stat err = %v); a lone lock file makes the operator's first real start refuse to boot",
						dirlock.LockFileName, err)
				}
				// Nothing beyond the two files the fixture itself wrote.
				if got, want := operatorDirEntries(t, dir), []string{busIDFileName, wal.WALFileName}; !reflect.DeepEqual(got, want) {
					t.Fatalf("the data directory now holds %v, want only the fixture's %v", got, want)
				}

				if asJSON {
					var obj map[string]interface{}
					if err := json.Unmarshal([]byte(stdout), &obj); err != nil {
						t.Fatalf("stdout is not the one JSON object the -json contract promises: %v\nstdout: %s", err, stdout)
					}
					if ok, _ := obj["ok"].(bool); ok {
						t.Fatalf("the failure object has ok = true: %v", obj)
					}
					if got, _ := obj["exit_code"].(float64); int(got) != exitOperatorUnverifiable {
						t.Fatalf("failure exit_code = %v, want %d", obj["exit_code"], exitOperatorUnverifiable)
					}
					if !strings.Contains(fmt.Sprint(obj["error"]), wal.MACKeyFileName) {
						t.Fatalf("the failure object does not name %s: %v", wal.MACKeyFileName, obj)
					}
				} else {
					if !strings.Contains(stderr, wal.MACKeyFileName) {
						t.Fatalf("stderr does not name %s:\n%s", wal.MACKeyFileName, stderr)
					}
					if !strings.Contains(stderr, "no key was created") {
						t.Fatalf("stderr does not state that NO KEY WAS CREATED, the load-bearing half of this refusal:\n%s", stderr)
					}
				}
			})
		}
	}
}

// TestOperatorListMACKeyFixtureMintsControl is the CONTROL for the guard test
// above, and it is not decoration.
//
// It proves the fixture shapes are ones on which wal.Replay DOES reach
// macKeyFor: the same directory, read with an unguarded wal.Replay, gains a
// wal-mac.key. Without this, a future change to wal that stopped minting on these
// shapes would leave the guard test passing for the wrong reason — asserting the
// absence of a file nothing would have created — and the real guard could then
// be deleted in silence.
func TestOperatorListMACKeyFixtureMintsControl(t *testing.T) {
	for _, shape := range operatorMintingWALs {
		shape := shape
		t.Run(shape.name, func(t *testing.T) {
			dir := t.TempDir()
			walPath := filepath.Join(dir, wal.WALFileName)
			if err := os.WriteFile(walPath, shape.bytes, 0o600); err != nil {
				t.Fatalf("writing the fixture %s: %v", wal.WALFileName, err)
			}
			keyPath := filepath.Join(dir, wal.MACKeyFileName)
			if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
				t.Fatalf("the fixture already holds a %s: %v", wal.MACKeyFileName, err)
			}

			// The unguarded read the command must never perform. Its ERROR is
			// irrelevant; its SIDE EFFECT is the whole point.
			_, _ = wal.Replay(walPath, func(wal.Committed) error { return nil })

			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("an unguarded wal.Replay over %q did NOT mint %s (%v), so the guard test built on this shape cannot fail "+
					"and is a dead guard; pick a shape that does mint", shape.name, wal.MACKeyFileName, err)
			}
		})
	}
}
