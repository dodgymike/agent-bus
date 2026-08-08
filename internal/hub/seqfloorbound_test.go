package hub

// Unit tests for maxPlausibleSeqFloor: the bound that turns "adopt an
// implausibly high floor and then fail every send for ever" into a refusal that
// names its own remedy.
//
// The whole-process proof lives in cmd/agent-bus/seqfloorforge_test.go — that is
// what shows the bus actually refusing to start rather than merely a function
// returning an error. These tests exist for the part that one cannot see: the
// exact boundary, and that the bound rejects ONLY implausible values.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRawSeqFloor writes a floor file with a VALID unkeyed digest, the way an
// attacker with write access to the data directory does — by hand, not through
// encodeSeqFloor, so the test cannot silently start agreeing with the code.
func writeRawSeqFloor(t *testing.T, dir string, floor uint64) string {
	t.Helper()
	body := fmt.Sprintf("floor %d\n", floor)
	sum := sha256.Sum256([]byte(body))
	data := fmt.Sprintf("%s v%d sha256=%s\n%s", seqFloorFileMagic, seqFloorFileVersion, hex.EncodeToString(sum[:]), body)
	path := filepath.Join(dir, SeqFloorFileName)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestSeqFloorFileRejectsAnImplausiblyHighFloor(t *testing.T) {
	cases := []struct {
		name  string
		floor uint64
		want  bool // true == must be REFUSED
	}{
		// The exploit as reported: a valid digest over the largest uint64.
		{"MaxUint64 — the reported exploit", math.MaxUint64, true},
		// One past the bound. Pins the boundary exactly; without it the check
		// could be >= and every test here would still pass.
		{"one past the bound", maxPlausibleSeqFloor + 1, true},
		// Exactly the bound is ACCEPTED. The bound is the largest plausible
		// value, not the smallest implausible one.
		{"exactly the bound", maxPlausibleSeqFloor, false},
		{"one below the bound", maxPlausibleSeqFloor - 1, false},
		// The false-positive guard, and the reason the bound is set so
		// generously: these are the values real buses actually hold, and a
		// bound that refused any of them would brick a healthy bus.
		{"zero", 0, false},
		{"one mint batch", MintBatchSize, false},
		{"a trillion messages", 1_000_000_000_000, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeRawSeqFloor(t, dir, tc.floor)

			got, existed, err := readSeqFloorFile(path)
			if !tc.want {
				if err != nil {
					t.Fatalf("floor %d was refused but is plausible: %v", tc.floor, err)
				}
				if !existed || got != tc.floor {
					t.Fatalf("readSeqFloorFile = (%d, %v), want (%d, true)", got, existed, tc.floor)
				}
				return
			}

			if err == nil {
				t.Fatalf("floor %d was ACCEPTED; adopting it exhausts the sequence allocator and fails every send for ever", tc.floor)
			}
			// It must be reported as CORRUPT-or-tampered, because that is the
			// error the caller treats as fatal. Returning some other error would
			// leave the value adopted or the failure mis-handled.
			if !errors.Is(err, ErrSeqFloorFileCorrupt) {
				t.Fatalf("floor %d was refused with %v, which does not wrap ErrSeqFloorFileCorrupt", tc.floor, err)
			}
			for _, want := range []string{"implausibly high", "TAMPERED WITH", "move " + path} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal of floor %d does not mention %q: %v", tc.floor, want, err)
				}
			}
		})
	}
}

// TestSeqFloorFileRoundTripsAtTheBound proves the bound is not merely a read
// check bolted on: a floor AT the bound written by the production encoder is
// read back unchanged, so nothing legitimate has been made unloadable.
func TestSeqFloorFileRoundTripsAtTheBound(t *testing.T) {
	dir := t.TempDir()
	f, err := openSeqFloorFile(dir)
	if err != nil {
		t.Fatalf("openSeqFloorFile(%q): %v", dir, err)
	}
	if err := f.raise(maxPlausibleSeqFloor); err != nil {
		t.Fatalf("raising to the bound: %v", err)
	}

	reopened, err := openSeqFloorFile(dir)
	if err != nil {
		t.Fatalf("reopening a data dir whose floor is exactly the bound: %v", err)
	}
	if got := reopened.burned(); got != maxPlausibleSeqFloor {
		t.Fatalf("floor after reopen = %d, want %d", got, maxPlausibleSeqFloor)
	}
}
