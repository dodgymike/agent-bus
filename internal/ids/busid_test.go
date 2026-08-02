package ids

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBusIDPersistence is the proof command for the ID-1 task: mint into a
// fresh data dir, persist mode 0600, and load the identical id on every
// subsequent call.
func TestBusIDPersistence(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreateBusID(dir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID() first call: %v", err)
	}
	if err := ValidateBusID(id1); err != nil {
		t.Fatalf("minted id %q failed ValidateBusID: %v", id1, err)
	}

	path := filepath.Join(dir, "bus-id")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("bus-id file mode = %o, want 0600", perm)
	}

	id2, err := LoadOrCreateBusID(dir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID() second call: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("LoadOrCreateBusID() second call = %q, want byte-identical id %q", id2, id1)
	}

	id3, err := LoadOrCreateBusID(dir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID() third call: %v", err)
	}
	if id3 != id1 {
		t.Fatalf("LoadOrCreateBusID() third call = %q, want byte-identical id %q", id3, id1)
	}
}

func TestGenerateBusIDFormat(t *testing.T) {
	for i := 0; i < 20; i++ {
		id, err := GenerateBusID()
		if err != nil {
			t.Fatalf("GenerateBusID(): %v", err)
		}
		if !strings.HasPrefix(id, "bus-") {
			t.Fatalf("GenerateBusID() = %q, want prefix \"bus-\"", id)
		}
		if strings.Contains(id, ".") {
			t.Fatalf("GenerateBusID() = %q, must not contain '.'", id)
		}
		if err := ValidateBusID(id); err != nil {
			t.Fatalf("GenerateBusID() = %q, failed ValidateBusID: %v", id, err)
		}
		if len(id) != 20 {
			t.Fatalf("GenerateBusID() = %q, want length 20, got %d", id, len(id))
		}
	}
}

func TestGenerateBusIDDistinct(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id, err := GenerateBusID()
		if err != nil {
			t.Fatalf("GenerateBusID(): %v", err)
		}
		if seen[id] {
			t.Fatalf("GenerateBusID() produced duplicate id %q on iteration %d", id, i)
		}
		seen[id] = true
	}
}

// TestLoadOrCreateBusIDCorruptPersisted covers the fatal, non-regenerating
// path for a persisted id that fails validation (including empty / whitespace
// files): this is a corrupt-data-dir condition, not a "mint a new one" one.
func TestLoadOrCreateBusIDCorruptPersisted(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"dot in id", "has.a.dot\n"},
		{"empty file", ""},
		{"whitespace only", "   \n\t\n"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bus-id")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("seeding corrupt bus-id file: %v", err)
			}

			if _, err := LoadOrCreateBusID(dir, ""); err == nil {
				t.Fatalf("LoadOrCreateBusID() with corrupt persisted id = nil error, want a fatal error")
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s after failed LoadOrCreateBusID: %v", path, err)
			}
			if string(after) != tt.content {
				t.Fatalf("LoadOrCreateBusID() rewrote the corrupt file: got %q, want it left unchanged as %q", after, tt.content)
			}
		})
	}
}

func TestLoadOrCreateBusIDOverridePersists(t *testing.T) {
	dir := t.TempDir()
	const override = "my-forced-bus"

	id, err := LoadOrCreateBusID(dir, override)
	if err != nil {
		t.Fatalf("LoadOrCreateBusID() with override: %v", err)
	}
	if id != override {
		t.Fatalf("LoadOrCreateBusID() = %q, want override %q", id, override)
	}

	persisted, err := os.ReadFile(filepath.Join(dir, "bus-id"))
	if err != nil {
		t.Fatalf("reading persisted bus-id: %v", err)
	}
	if got := strings.TrimSpace(string(persisted)); got != override {
		t.Fatalf("persisted bus id = %q, want %q", got, override)
	}

	// A later call with no override must load the persisted override id.
	id2, err := LoadOrCreateBusID(dir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID() second call without override: %v", err)
	}
	if id2 != override {
		t.Fatalf("LoadOrCreateBusID() second call = %q, want %q", id2, override)
	}
}

func TestLoadOrCreateBusIDOverrideMismatch(t *testing.T) {
	dir := t.TempDir()

	persisted, err := LoadOrCreateBusID(dir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateBusID() seeding persisted id: %v", err)
	}

	const other = "some-other-bus"
	if _, err := LoadOrCreateBusID(dir, other); err == nil {
		t.Fatalf("LoadOrCreateBusID() with mismatched override = nil error, want an error")
	} else if !strings.Contains(err.Error(), persisted) || !strings.Contains(err.Error(), other) {
		t.Fatalf("LoadOrCreateBusID() mismatch error = %q, want it to mention both %q and %q", err, persisted, other)
	}
}

func TestValidateBusID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"empty", "", true},
		{"simple valid", "bus-abc123_XYZ", false},
		{"64 chars ok", strings.Repeat("a", 64), false},
		{"65 chars rejected", strings.Repeat("a", 65), true},
		{"dot rejected", "bus.id", true},
		{"slash rejected", "bus/id", true},
		{"space rejected", "bus id", true},
		{"unicode rejected", "bus-éd", true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBusID(tt.id)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateBusID(%q) = nil, want an error", tt.id)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateBusID(%q) = %v, want nil", tt.id, err)
			}
		})
	}
}
