package wal

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// idemFieldTS is the fixed timestamp every golden payload in this file is
// encoded at, so the expected bytes are literal rather than computed by the
// code under test.
var idemFieldTS = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// TestPrepareWithoutIdemIsByteIdentical is the additivity proof for IDEM-11's
// Entry.Idem: a prepare payload for an entry that carries NO applied-key record
// must be byte-for-byte what this codec produced before the field existed.
//
// This is the check that makes the change safe to land on an existing log. If
// omitempty were ever dropped (or the field written as an explicit null), every
// prepare in every existing file would gain a field, and the golden literal
// below is the only thing that would notice.
func TestPrepareWithoutIdemIsByteIdentical(t *testing.T) {
	tests := []struct {
		name string
		kind string
		body json.RawMessage
		want string
	}{
		{
			name: "with body",
			kind: "message",
			body: json.RawMessage(`{"n":1}`),
			want: `{"kind":"message","ts":"2026-08-02T12:00:00Z","body":{"n":1}}`,
		},
		{
			name: "nil body encodes as null, still no idem field",
			kind: "agent",
			body: nil,
			want: `{"kind":"agent","ts":"2026-08-02T12:00:00Z","body":null}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The pre-IDEM-11 spelling.
			old, err := encodePrepare(tc.kind, tc.body, idemFieldTS)
			if err != nil {
				t.Fatalf("encodePrepare: %v", err)
			}
			if string(old) != tc.want {
				t.Fatalf("encodePrepare wrote %s, want %s", old, tc.want)
			}
			// The idem-carrying spelling, with no record to carry.
			neu, err := encodePrepareWithIdem(tc.kind, tc.body, nil, idemFieldTS)
			if err != nil {
				t.Fatalf("encodePrepareWithIdem: %v", err)
			}
			if !bytes.Equal(old, neu) {
				t.Fatalf("a nil idem record changed the payload bytes:\n old %s\n new %s", old, neu)
			}
			if bytes.Contains(neu, []byte(`"idem"`)) {
				t.Fatalf("a nil idem record still wrote the field: %s", neu)
			}
		})
	}
}

// TestPrepareIdemRoundTrip proves the field survives the full write path: it is
// canonicalised on the way in exactly like Body, it appears in the Committed
// entry the Applier sees, and DecodePrepare recovers the same bytes off disk.
func TestPrepareIdemRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   json.RawMessage
		want string // "" means nil
	}{
		{name: "absent", in: nil, want: ""},
		{name: "explicit null normalises to nil", in: json.RawMessage(`null`), want: ""},
		{name: "compacted", in: json.RawMessage("{ \"key\" : \"k1\" ,\n\"seq\": 7 }"), want: `{"key":"k1","seq":7}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &testApplier{}
			l, path := openTestLog(t, a)
			c, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`), Idem: tc.in})
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := string(c.Entry.Idem); got != tc.want {
				t.Fatalf("Committed.Entry.Idem = %q, want %q", got, tc.want)
			}
			if tc.want == "" && c.Entry.Idem != nil {
				t.Fatalf("an absent idem record must canonicalise to nil, got %#v", c.Entry.Idem)
			}
			if a.count() != 1 {
				t.Fatalf("Apply calls = %d, want 1", a.count())
			}
			if got := string(a.at(0).Entry.Idem); got != tc.want {
				t.Fatalf("the applied entry carried idem %q, want %q", got, tc.want)
			}

			// And off disk: a replayed Apply must see the same bytes a live one did.
			recs, _, err := ScanAll(path, KindWAL)
			if err != nil {
				t.Fatalf("ScanAll: %v", err)
			}
			var seen bool
			for _, rec := range recs {
				if rec.Type != TypePrepare {
					continue
				}
				e, _, err := DecodePrepare(path, rec)
				if err != nil {
					t.Fatalf("DecodePrepare: %v", err)
				}
				if got := string(e.Idem); got != tc.want {
					t.Fatalf("DecodePrepare returned idem %q, want %q", got, tc.want)
				}
				seen = true
			}
			if !seen {
				t.Fatal("no PREPARE record found in the log")
			}
		})
	}
}

// TestPrepareIdemInvalidJSONRejected proves an unparseable applied-key record
// is refused BEFORE anything is written, with the same sentinel a bad body
// gets, so a record that cannot be stored fails the operation rather than
// landing on disk half-formed.
func TestPrepareIdemInvalidJSONRejected(t *testing.T) {
	l, path := openTestLog(t, nil)
	before, _ := scanTypes(t, path)
	_, err := l.Write(Entry{Kind: "message", Body: json.RawMessage(`{"n":1}`), Idem: json.RawMessage(`{oops`)})
	if !errors.Is(err, ErrInvalidBody) {
		t.Fatalf("Write with an unparseable idem record: err = %v, want ErrInvalidBody", err)
	}
	after, _ := scanTypes(t, path)
	if len(after) != len(before) {
		t.Fatalf("a rejected idem record wrote to the log: %d records -> %d", len(before), len(after))
	}
}
