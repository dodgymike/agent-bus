package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/logging"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// These fixtures are a single-bus conversation on "testbus". The recipients and
// creator are fully-qualified "<bus>.<agent>" ids (invariant 2), which is what
// ids.ParseAgentID requires.
const (
	convBusID     = "testbus"
	convCreator   = "testbus.alice-1"
	convRecipient = "testbus.bob-1"
)

func convFixture(t *testing.T) store.ConversationRecord {
	t.Helper()
	id, err := store.NewConversationID(convBusID)
	if err != nil {
		t.Fatalf("NewConversationID: %v", err)
	}
	return store.ConversationRecord{
		ID:         id,
		Creator:    convCreator,
		Name:       "incident-4821 responders",
		Recipients: []string{convRecipient, "testbus.carol-2"},
		CreatedAt:  time.Date(2026, 8, 29, 12, 0, 0, 123456789, time.UTC),
	}
}

// TestConversationRecordRoundTrip is the encode -> decode identity, the core
// property CONV-RECORD owes: a record encoded to its durable bytes and read back
// off disk is field-for-field the same record.
func TestConversationRecordRoundTrip(t *testing.T) {
	rec := convFixture(t)
	body, err := rec.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := store.DecodeConversationRecord(body)
	if err != nil {
		t.Fatalf("DecodeConversationRecord: %v", err)
	}
	if got.ID != rec.ID || got.Creator != rec.Creator || got.Name != rec.Name {
		t.Errorf("scalar fields differ after round-trip:\n got %+v\nwant %+v", got, rec)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("created_at = %s, want %s", got.CreatedAt, rec.CreatedAt)
	}
	if strings.Join(got.Recipients, ",") != strings.Join(rec.Recipients, ",") {
		t.Errorf("recipients = %v, want %v", got.Recipients, rec.Recipients)
	}
	// Encoding the decoded record must reproduce the same bytes: the canonical
	// form is stable, which is what lets a live apply and a replayed apply agree.
	body2, err := got.Encode()
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if !bytes.Equal(body, body2) {
		t.Errorf("re-encode is not byte-stable:\n%s\nvs\n%s", body, body2)
	}
}

// TestConversationRecordEmptyNameOmitted proves the optional name: an empty name
// is permitted, is not written to the durable body, and round-trips empty.
func TestConversationRecordEmptyNameOmitted(t *testing.T) {
	rec := convFixture(t)
	rec.Name = ""
	body, err := rec.Encode()
	if err != nil {
		t.Fatalf("Encode with empty name: %v", err)
	}
	if bytes.Contains(body, []byte(`"name"`)) {
		t.Errorf("empty name was written to the body; it must be omitted:\n%s", body)
	}
	got, err := store.DecodeConversationRecord(body)
	if err != nil {
		t.Fatalf("DecodeConversationRecord: %v", err)
	}
	if got.Name != "" {
		t.Errorf("name = %q, want empty", got.Name)
	}
}

// TestConversationRecordIDShape proves the id shape ruling (CONV-ID-SHAPE): the
// minted id is "<busID>.<uuid-v4>", the uuid half is a canonical version-4 UUID,
// and it is server-authoritative (minted from the bus's own id, no client input).
func TestConversationRecordIDShape(t *testing.T) {
	id, err := store.NewConversationID(convBusID)
	if err != nil {
		t.Fatalf("NewConversationID: %v", err)
	}
	prefix := convBusID + "."
	if !strings.HasPrefix(id, prefix) {
		t.Fatalf("minted id %q does not start with %q (invariant 2 qualification)", id, prefix)
	}
	uuid := strings.TrimPrefix(id, prefix)
	if len(uuid) != 36 {
		t.Fatalf("uuid half %q is %d bytes, want 36", uuid, len(uuid))
	}
	for _, i := range []int{8, 13, 18, 23} {
		if uuid[i] != '-' {
			t.Errorf("uuid byte %d = %q, want '-'", i, string(uuid[i]))
		}
	}
	if uuid[14] != '4' {
		t.Errorf("uuid version nibble = %q, want '4' (UUIDv4)", string(uuid[14]))
	}
	switch uuid[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("uuid variant nibble = %q, want one of 8,9,a,b (RFC 4122)", string(uuid[19]))
	}
	// Two mints never collide (invariant 1: never reused).
	id2, err := store.NewConversationID(convBusID)
	if err != nil {
		t.Fatalf("second NewConversationID: %v", err)
	}
	if id == id2 {
		t.Errorf("two mints produced the same id %q; a conversation id is never reused", id)
	}
	// A minted id must survive its own validator, on the decode path, in a record.
	rec := convFixture(t)
	rec.ID = id
	if _, err := rec.Encode(); err != nil {
		t.Errorf("a freshly minted id failed record validation: %v", err)
	}
	// An invalid bus id cannot mint an id.
	if _, err := store.NewConversationID("bad.bus"); err == nil {
		t.Errorf("NewConversationID accepted a bus id containing '.'; that would make the qualification ambiguous")
	}
}

// TestConversationRecordNameBoundConstruct enforces the CONV-NAME-INV6 bound on
// the CONSTRUCTION path (Encode): over-128-bytes, an embedded control character
// and invalid UTF-8 are each REFUSED, and valid names — including a multibyte one
// and one exactly at the 128-byte bound — are accepted.
func TestConversationRecordNameBoundConstruct(t *testing.T) {
	accepted := []struct {
		name string
		val  string
	}{
		{"short ascii", "release-triage coordination"},
		{"multibyte utf8", "café-résumé déjà-vu"},
		{"exactly 128 bytes", strings.Repeat("a", 128)},
		{"empty", ""},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			rec := convFixture(t)
			rec.Name = tc.val
			if _, err := rec.Encode(); err != nil {
				t.Errorf("Encode refused a valid name %q: %v", tc.val, err)
			}
		})
	}
	refused := []struct {
		name string
		val  string
	}{
		{"129 bytes", strings.Repeat("a", 129)},
		{"newline", "line1\nline2"},
		{"carriage return", "a\rb"},
		{"tab", "a\tb"},
		{"nul", "a\x00b"},
		{"del", "a\x7fb"},
		{"c1 control", "ab"},
		{"line separator U+2028", "a b"},
		{"paragraph separator U+2029", "a b"},
		{"invalid utf8", string([]byte{'a', 0xff, 0xfe, 'b'})},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			rec := convFixture(t)
			rec.Name = tc.val
			_, err := rec.Encode()
			if !errors.Is(err, store.ErrInvalidConversation) {
				t.Errorf("Encode accepted an invalid name (%s); want ErrInvalidConversation, got %v", tc.name, err)
			}
		})
	}
}

// convJSON is the durable JSON shape, mirrored in the test so a body with a
// tampered field can be handed to DecodeConversationRecord directly. Its json
// tags MUST match conversationRecordJSON exactly, or DisallowUnknownFields would
// reject a well-formed record for the wrong reason.
type convJSON struct {
	Version    int      `json:"record_version"`
	ID         string   `json:"conversation_id"`
	Creator    string   `json:"creator"`
	Name       string   `json:"name,omitempty"`
	Recipients []string `json:"recipients"`
	CreatedAt  string   `json:"created_at"`
}

func (j convJSON) marshal(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal test body: %v", err)
	}
	return b
}

func validConvJSON(t *testing.T) convJSON {
	t.Helper()
	id, err := store.NewConversationID(convBusID)
	if err != nil {
		t.Fatalf("NewConversationID: %v", err)
	}
	return convJSON{
		Version:    1,
		ID:         id,
		Creator:    convCreator,
		Name:       "ok",
		Recipients: []string{convRecipient},
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// TestConversationRecordNameBoundDecode enforces the CONV-NAME-INV6 bound on the
// DISK-DECODE path: a record read back from a tampered log with an over-long or
// control-bearing name is REFUSED, not trusted. This is the dual-enforcement
// crux of the ruling — the bound holds even when the handler never ran.
func TestConversationRecordNameBoundDecode(t *testing.T) {
	// Sanity: the valid body decodes.
	if _, err := store.DecodeConversationRecord(validConvJSON(t).marshal(t)); err != nil {
		t.Fatalf("the control (valid) body did not decode: %v", err)
	}
	cases := []struct {
		name string
		mut  func(j *convJSON)
	}{
		{"over 128 bytes", func(j *convJSON) { j.Name = strings.Repeat("a", 200) }},
		{"embedded newline", func(j *convJSON) { j.Name = "audit-line\nforged-second-line" }},
		{"embedded tab", func(j *convJSON) { j.Name = "a\tb" }},
		{"embedded del", func(j *convJSON) { j.Name = "a\x7fb" }},
		{"line separator", func(j *convJSON) { j.Name = "a b" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := validConvJSON(t)
			tc.mut(&j)
			_, err := store.DecodeConversationRecord(j.marshal(t))
			if !errors.Is(err, store.ErrInvalidConversation) {
				t.Errorf("decode accepted a tampered name (%s); want ErrInvalidConversation, got %v", tc.name, err)
			}
		})
	}
}

// TestConversationRecordIDBoundDecode enforces the id shape on the disk-decode
// path: a record with a malformed id — no separator, a non-v4 uuid, uppercase
// hex, an over-long id — is refused, not trusted (invariant 1).
func TestConversationRecordIDBoundDecode(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"no separator", "testbus550e8400e29b41d4a716446655440000"},
		{"empty uuid", "testbus."},
		{"short uuid", "testbus.550e8400"},
		{"uppercase hex", "testbus.550E8400-E29B-41D4-A716-446655440000"},
		{"not version 4", "testbus.550e8400-e29b-11d4-a716-446655440000"},
		{"bad variant", "testbus.550e8400-e29b-41d4-0716-446655440000"},
		{"dot in bus half is caught elsewhere; oversize id", "testbus." + strings.Repeat("a", 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := validConvJSON(t)
			j.ID = tc.id
			_, err := store.DecodeConversationRecord(j.marshal(t))
			if !errors.Is(err, store.ErrInvalidConversation) {
				t.Errorf("decode accepted a malformed id %q; want ErrInvalidConversation, got %v", tc.id, err)
			}
		})
	}
}

// TestConversationRecordRecipientBoundDecode enforces the recipient-list bounds
// on the disk-decode path: zero recipients, over MaxConversationRecipients, a
// duplicate, and a non-fully-qualified recipient are each refused.
func TestConversationRecordRecipientBoundDecode(t *testing.T) {
	many := make([]string, store.MaxConversationRecipients+1)
	for i := range many {
		many[i] = fmt.Sprintf("testbus.agent-%d", i)
	}
	cases := []struct {
		name string
		rs   []string
	}{
		{"zero recipients", []string{}},
		{"over the cap", many},
		{"duplicate", []string{convRecipient, convRecipient}},
		{"not fully qualified", []string{"bob"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := validConvJSON(t)
			j.Recipients = tc.rs
			_, err := store.DecodeConversationRecord(j.marshal(t))
			if !errors.Is(err, store.ErrInvalidConversation) {
				t.Errorf("decode accepted recipients (%s); want ErrInvalidConversation, got %v", tc.name, err)
			}
		})
	}
}

// TestConversationRecordStrictDecode proves the decoder is strict: an unknown
// field, trailing data and a wrong record_version are each refused rather than
// read leniently.
func TestConversationRecordStrictDecode(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		valid := validConvJSON(t).marshal(t)
		// Splice an unknown field in.
		injected := bytes.Replace(valid, []byte(`"creator"`), []byte(`"surprise":1,"creator"`), 1)
		if _, err := store.DecodeConversationRecord(injected); !errors.Is(err, store.ErrInvalidConversation) {
			t.Errorf("decode accepted an unknown field; want ErrInvalidConversation, got %v", err)
		}
	})
	t.Run("trailing data", func(t *testing.T) {
		body := append(validConvJSON(t).marshal(t), []byte(`{"more":1}`)...)
		if _, err := store.DecodeConversationRecord(body); !errors.Is(err, store.ErrInvalidConversation) {
			t.Errorf("decode accepted trailing data; want ErrInvalidConversation, got %v", err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		j := validConvJSON(t)
		j.Version = 2
		if _, err := store.DecodeConversationRecord(j.marshal(t)); !errors.Is(err, store.ErrInvalidConversation) {
			t.Errorf("decode accepted record_version 2; want ErrInvalidConversation, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Durable write path + recovery/replay against a REAL wal.Log.
// ---------------------------------------------------------------------------

// convCapLog captures log output so the invariant-6 discard assertions can read
// the ERROR lines rather than have them discarded.
type convCapLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *convCapLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *convCapLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// openConvStore opens a REAL wal.Log over dir with a fresh ConversationStore as
// its applier — build the store, Open (which REPLAYS into it), then Attach —
// exactly the three-step cmd/agent-bus uses.
func openConvStore(t *testing.T, dir string) (*store.ConversationStore, *wal.Log, *convCapLog) {
	t.Helper()
	cap := &convCapLog{}
	lg := logging.New(cap, logging.LevelDebug)
	st, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: convBusID, Logger: lg})
	if err != nil {
		t.Fatalf("NewConversationStore: %v", err)
	}
	l, err := wal.Open(wal.LogOptions{Dir: dir, Logger: lg, Applier: st})
	if err != nil {
		t.Fatalf("wal.Open(%s): %v", dir, err)
	}
	if err := st.Attach(l); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return st, l, cap
}

// TestConversationRecordDurableCreateAndReplay is the recovery/replay proof
// (invariants 4 and 5): a conversation created and committed through the
// two-phase log is present after a clean reopen, reconstructed from the log alone.
func TestConversationRecordDurableCreateAndReplay(t *testing.T) {
	dir := t.TempDir()

	st, l, _ := openConvStore(t, dir)
	rec, err := st.Create(convCreator, "release-triage", []string{convRecipient, "testbus.carol-2"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.ID == "" {
		t.Fatalf("Create returned an empty id")
	}
	if got, ok := st.Get(rec.ID); !ok || got.Name != "release-triage" {
		t.Fatalf("the created conversation is not in the serving copy: ok=%v got=%+v", ok, got)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen with a fresh store: replay must rebuild the record.
	st2, l2, _ := openConvStore(t, dir)
	defer l2.Close()
	got, ok := st2.Get(rec.ID)
	if !ok {
		t.Fatalf("the conversation is missing after a clean reopen; its Create committed and fsynced, so recovery dropped a record it had already acknowledged (invariant 4)")
	}
	if got.Creator != rec.Creator || got.Name != rec.Name || !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("recovered record differs from the created one:\n got %+v\nwant %+v", got, rec)
	}
	if strings.Join(got.Recipients, ",") != strings.Join(rec.Recipients, ",") {
		t.Errorf("recovered recipients = %v, want %v", got.Recipients, rec.Recipients)
	}
	if n := st2.Len(); n != 1 {
		t.Errorf("Len after replay = %d, want 1", n)
	}
}

// TestConversationRecordCreateBeforeAttach proves Create fails closed before a
// durable log is attached: an unattached table would mint ids and report
// conversations no restart could reproduce (invariant 4 has no best-effort mode).
func TestConversationRecordCreateBeforeAttach(t *testing.T) {
	st, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: convBusID})
	if err != nil {
		t.Fatalf("NewConversationStore: %v", err)
	}
	if _, err := st.Create(convCreator, "x", []string{convRecipient}); !errors.Is(err, store.ErrConversationNotDurable) {
		t.Errorf("Create before Attach = %v, want ErrConversationNotDurable", err)
	}
}

// TestConversationRecordApplyDiscardsTamperedRecord proves the disk-decode
// enforcement inside the replay path: a committed entry whose body fails the
// bounds is DISCARDED (not folded in) and logged loudly at ERROR (invariant 6),
// and Apply never returns an error — an error there would poison the whole log.
func TestConversationRecordApplyDiscardsTamperedRecord(t *testing.T) {
	cap := &convCapLog{}
	lg := logging.New(cap, logging.LevelDebug)
	st, err := store.NewConversationStore(store.ConversationStoreOptions{BusID: convBusID, Logger: lg})
	if err != nil {
		t.Fatalf("NewConversationStore: %v", err)
	}
	// A committed entry carrying a name with an embedded newline — the log-injection
	// case the charset bound closes.
	j := validConvJSON(t)
	j.Name = "forged\nsecond-line"
	bad := wal.Committed{PrepareIndex: 7, CommitIndex: 8, Entry: wal.Entry{Kind: store.ConversationRecordKind, Body: j.marshal(t)}}
	if err := st.Apply(bad); err != nil {
		t.Fatalf("Apply returned %v; it must never error, or a durable-commit divergence would poison the log", err)
	}
	if n := st.Len(); n != 0 {
		t.Errorf("a tampered record was folded into the serving copy: Len = %d, want 0", n)
	}
	if !strings.Contains(cap.String(), "level=error") || !strings.Contains(cap.String(), "DISCARDING") {
		t.Errorf("the discard was not logged loudly at ERROR (invariant 6). log:\n%s", cap.String())
	}
}
