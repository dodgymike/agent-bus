package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/store"
)

// ---------------------------------------------------------------------------
// The durable message record.
//
// Decode runs on the RECOVERY path, where the input is bytes off a disk rather
// than a value a handler validated. Every cross-field relationship is re-checked
// there, and the content-hash check is the load-bearing one: it is what makes a
// silently altered body a recovery error instead of a message this bus would go
// on to serve while still asserting its hash in the audit trail.
// ---------------------------------------------------------------------------

// recordOf renders a valid Record for mutation by the rejection table.
func recordOf(t *testing.T, m store.Message) store.Record {
	t.Helper()
	return m.Record()
}

// encodeRecord marshals a (possibly mangled) Record the way the write path does.
func encodeRecord(t *testing.T, rec store.Record) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling a test record: %v", err)
	}
	return json.RawMessage(b)
}

func TestMessageRoundTrip(t *testing.T) {
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")
	sentAt := time.Date(2026, 8, 2, 12, 34, 56, 789000000, time.UTC)

	cases := []struct {
		name       string
		broadcast  bool
		recipients []string
		body       []byte
	}{
		{"Broadcast", true, nil, []byte("hello everyone")},
		{"Directed", false, []string{b}, []byte("hello beta")},
		{"BinaryBody", true, nil, []byte{0x00, 0xff, 0x0a, 0x7f, 0x80, 0xfe}},
		{"BodyAtTheLimit", true, nil, bytes.Repeat([]byte("z"), store.MaxBodyBytes)},
		{"OneByteBody", true, nil, []byte{0x00}},
	}
	if len(cases) == 0 {
		t.Fatal("the round-trip table is empty")
	}
	checked := 0
	for i, c := range cases {
		c, i := c, i
		t.Run(c.name, func(t *testing.T) {
			m, err := store.NewMessage(testBusID, a, c.broadcast, c.recipients, uint64(i+1), sentAt, c.body, "rt-key")
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			raw, err := m.Encode()
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := store.Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.ID != m.ID || got.Seq != m.Seq || got.Sender != m.Sender {
				t.Fatalf("round trip changed identity: got %s/%d/%s, want %s/%d/%s", got.ID, got.Seq, got.Sender, m.ID, m.Seq, m.Sender)
			}
			if got.Broadcast != m.Broadcast {
				t.Fatalf("round trip changed Broadcast: %v -> %v", m.Broadcast, got.Broadcast)
			}
			if len(got.Recipients) != len(m.Recipients) {
				t.Fatalf("round trip changed Recipients: %v -> %v", m.Recipients, got.Recipients)
			}
			for j := range m.Recipients {
				if got.Recipients[j] != m.Recipients[j] {
					t.Fatalf("round trip changed Recipients: %v -> %v", m.Recipients, got.Recipients)
				}
			}
			if !bytes.Equal(got.Body, m.Body) {
				t.Fatalf("round trip changed the body (%d bytes -> %d bytes)", len(m.Body), len(got.Body))
			}
			if got.ContentSHA256 != store.ContentHash(c.body) {
				t.Fatalf("ContentSHA256 = %q, want %q", got.ContentSHA256, store.ContentHash(c.body))
			}
			if !got.SentAt.Equal(sentAt) {
				t.Fatalf("round trip changed SentAt: %v -> %v", sentAt, got.SentAt)
			}
			if got.IdempotencyKey != "rt-key" {
				t.Fatalf("round trip changed IdempotencyKey: %q", got.IdempotencyKey)
			}
			if len(got.BusPath) != 1 || got.BusPath[0] != testBusID {
				t.Fatalf("BusPath = %v, want [%s]", got.BusPath, testBusID)
			}
			if got.Size() != len(c.body) {
				t.Fatalf("Size() = %d, want %d", got.Size(), len(c.body))
			}
			checked++
		})
	}
	if checked != len(cases) {
		t.Fatalf("round-tripped %d shapes, want %d", checked, len(cases))
	}
}

func TestNewMessageRejects(t *testing.T) {
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")
	now := time.Now()

	cases := []struct {
		name       string
		busID      string
		seq        uint64
		recipients []string
		body       []byte
	}{
		{"SequenceZero", testBusID, 0, nil, []byte("x")},
		{"BadBusID", "bus.with.dots", 1, nil, []byte("x")},
		{"EmptyBusID", "", 1, nil, []byte("x")},
		{"OversizedBody", testBusID, 1, nil, bytes.Repeat([]byte("x"), store.MaxBodyBytes+1)},
		{"TooManyRecipients", testBusID, 1, repeatAgent(b, store.MaxRecipients+1), []byte("x")},
	}
	if len(cases) == 0 {
		t.Fatal("the NewMessage rejection table is empty")
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := store.NewMessage(c.busID, a, len(c.recipients) == 0, c.recipients, c.seq, now, c.body, "k")
			if !errors.Is(err, store.ErrInvalidMessage) {
				t.Fatalf("NewMessage = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestNewMessageCopiesItsInputs(t *testing.T) {
	// A caller that still holds the slices must not be able to re-address or
	// rewrite a message that is already on its way to disk.
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")
	g := agentIDFor(t, "gamma")

	recipients := []string{b}
	body := []byte("original")

	m, err := store.NewMessage(testBusID, a, false, recipients, 1, time.Now(), body, "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	recipients[0] = g
	body[0] = 'X'

	if m.Recipients[0] != b {
		t.Fatalf("mutating the caller's recipient slice re-addressed the message to %s", m.Recipients[0])
	}
	if string(m.Body) != "original" {
		t.Fatalf("mutating the caller's body slice changed the message to %q", m.Body)
	}
	if m.ContentSHA256 != store.ContentHash([]byte("original")) {
		t.Fatalf("ContentSHA256 no longer matches the retained body")
	}
}

func TestDecodeRejects(t *testing.T) {
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")
	sentAt := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	broadcast, err := store.NewMessage(testBusID, a, true, nil, 7, sentAt, []byte("payload"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	directed, err := store.NewMessage(testBusID, a, false, []string{b}, 8, sentAt, []byte("payload"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	cases := []struct {
		name    string
		mangle  func(rec store.Record) store.Record
		base    store.Message
		wantSub string
	}{
		{
			name:    "WrongSchemaVersion",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.V = store.RecordVersion + 1; return r },
			wantSub: "schema version",
		},
		{
			name:    "SchemaVersionZero",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.V = 0; return r },
			wantSub: "schema version",
		},
		{
			name:    "SequenceZero",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.Seq = 0; return r },
			wantSub: "sequence 0",
		},
		{
			name:    "MalformedMessageID",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.MessageID = "not-a-message-id-@"; return r },
			wantSub: "",
		},
		{
			name:    "IDSequenceMismatch",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.MessageID = testBusID + "-9"; return r },
			wantSub: "carries sequence",
		},
		{
			name:    "BadSender",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.Sender = "alpha"; return r },
			wantSub: "sender",
		},
		{
			name:    "EmptySender",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.Sender = ""; return r },
			wantSub: "sender",
		},
		{
			name:    "BroadcastWithRecipients",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.Recipients = []string{b}; return r },
			wantSub: "broadcast carries no recipient list",
		},
		{
			name:    "DirectedWithNoRecipient",
			base:    directed,
			mangle:  func(r store.Record) store.Record { r.Recipients = nil; return r },
			wantSub: "must name at least one recipient",
		},
		{
			name:    "MalformedRecipient",
			base:    directed,
			mangle:  func(r store.Record) store.Record { r.Recipients = []string{"beta"}; return r },
			wantSub: "recipient 0",
		},
		{
			name:    "TooManyRecipients",
			base:    directed,
			mangle:  func(r store.Record) store.Record { r.Recipients = repeatAgent(b, store.MaxRecipients+1); return r },
			wantSub: "recipients, the limit",
		},
		{
			name:    "SizeMismatch",
			base:    broadcast,
			mangle:  func(r store.Record) store.Record { r.Size = len(r.Body) + 1; return r },
			wantSub: "carries",
		},
		{
			name: "ContentHashMismatch",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				// The body is altered and the DECLARED hash is left alone: the
				// exact shape of a silently corrupted payload. The framing MAC
				// proves the bytes are the bytes we wrote; only this check proves
				// they are still coherent.
				r.Body = []byte("payloa!")
				r.Size = len(r.Body)
				return r
			},
			wantSub: "content hash mismatch",
		},
		{
			name: "ContentHashMismatchWithAdjustedSize",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.ContentSHA256 = store.ContentHash([]byte("something else entirely"))
				return r
			},
			wantSub: "content hash mismatch",
		},
		{
			name: "OversizedBody",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.Body = bytes.Repeat([]byte("x"), store.MaxBodyBytes+1)
				r.Size = len(r.Body)
				r.ContentSHA256 = store.ContentHash(r.Body)
				return r
			},
			wantSub: "the limit is",
		},
		{
			// Bounded HERE as well as at the handler, because this decoder is the
			// boundary for records THIS process did not validate. The key is
			// reflected into the server log, so an unbounded one off disk is an
			// unbounded log line.
			name: "OversizedIdempotencyKey",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.IdempotencyKey = strings.Repeat("k", store.MaxIdempotencyKeyLen+1)
				return r
			},
			wantSub: "idempotency key is",
		},
		{
			// BusPath is echoed verbatim to every client that reads the message
			// and is what the RELAY epic makes loop-prevention decisions from, so
			// an unbounded hop list off disk is attacker-chosen response content.
			name: "BusPathTooLong",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.BusPath = busPath(store.MaxBusPath + 1)
				return r
			},
			wantSub: "hops, the limit is",
		},
		{
			name: "BusPathHopIsNotAValidBusID",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.BusPath = []string{testBusID, "not a bus id!"}
				return r
			},
			wantSub: "bus path hop 1",
		},
		{
			name: "BusPathFirstHopIsNotAValidBusID",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.BusPath = []string{"bus.with.dots"}
				return r
			},
			wantSub: "bus path hop 0",
		},
		{
			name: "BusPathHopIsEmpty",
			base: broadcast,
			mangle: func(r store.Record) store.Record {
				r.BusPath = []string{testBusID, ""}
				return r
			},
			wantSub: "bus path hop 1",
		},
	}
	if len(cases) == 0 {
		t.Fatal("the Decode rejection table is empty")
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			raw := encodeRecord(t, c.mangle(recordOf(t, c.base)))
			got, err := store.Decode(raw)
			if !errors.Is(err, store.ErrInvalidMessage) {
				t.Fatalf("Decode = (%+v, %v), want ErrInvalidMessage", got, err)
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("Decode error %q does not mention %q — the operator cannot tell which check fired", err, c.wantSub)
			}
			if got.Seq != 0 || got.ID != "" {
				t.Fatalf("a rejected record still produced a usable Message: %+v", got)
			}
		})
	}

	t.Run("NotJSON", func(t *testing.T) {
		if _, err := store.Decode(json.RawMessage(`{not json`)); !errors.Is(err, store.ErrInvalidMessage) {
			t.Fatalf("Decode of malformed JSON = %v, want ErrInvalidMessage", err)
		}
	})

	t.Run("ValuesExactlyAtTheLimitStillDecode", func(t *testing.T) {
		// Without this the rejections above are consistent with a blanket refusal
		// of any idempotency key or any bus path at all.
		boundaries := []struct {
			name   string
			mangle func(store.Record) store.Record
			check  func(*testing.T, store.Message)
		}{
			{
				name: "IdempotencyKeyExactlyAtTheLimit",
				mangle: func(r store.Record) store.Record {
					r.IdempotencyKey = strings.Repeat("k", store.MaxIdempotencyKeyLen)
					return r
				},
				check: func(t *testing.T, m store.Message) {
					if len(m.IdempotencyKey) != store.MaxIdempotencyKeyLen {
						t.Fatalf("decoded idempotency key is %d bytes, want %d", len(m.IdempotencyKey), store.MaxIdempotencyKeyLen)
					}
				},
			},
			{
				name: "BusPathExactlyAtTheLimit",
				mangle: func(r store.Record) store.Record {
					r.BusPath = busPath(store.MaxBusPath)
					return r
				},
				check: func(t *testing.T, m store.Message) {
					if len(m.BusPath) != store.MaxBusPath {
						t.Fatalf("decoded bus path has %d hops, want %d", len(m.BusPath), store.MaxBusPath)
					}
				},
			},
			{
				// An ABSENT bus path is filled in from the message id, which is
				// what a record written before the field existed looks like.
				name: "EmptyBusPathIsFilledFromTheMessageID",
				mangle: func(r store.Record) store.Record {
					r.BusPath = nil
					return r
				},
				check: func(t *testing.T, m store.Message) {
					if len(m.BusPath) != 1 || m.BusPath[0] != testBusID {
						t.Fatalf("decoded bus path = %v, want [%s]", m.BusPath, testBusID)
					}
				},
			},
		}
		if len(boundaries) == 0 {
			t.Fatal("the Decode boundary table is empty")
		}
		checked := 0
		for _, bc := range boundaries {
			bc := bc
			t.Run(bc.name, func(t *testing.T) {
				got, err := store.Decode(encodeRecord(t, bc.mangle(recordOf(t, broadcast))))
				if err != nil {
					t.Fatalf("Decode rejected a record at the limit: %v", err)
				}
				bc.check(t, got)
			})
			checked++
		}
		if checked != len(boundaries) {
			t.Fatalf("checked %d boundary records, want %d", checked, len(boundaries))
		}
	})

	t.Run("TheUnmangledBaseStillDecodes", func(t *testing.T) {
		// Without this, every rejection above could be explained by the fixture
		// itself being invalid.
		for i, base := range []store.Message{broadcast, directed} {
			raw := encodeRecord(t, recordOf(t, base))
			if _, err := store.Decode(raw); err != nil {
				t.Fatalf("base record %d does not decode: %v", i, err)
			}
		}
	})

	t.Run("BodyDoesNotLeakIntoTheHashMismatchError", func(t *testing.T) {
		// The body may be a megabyte, and once the CRYPTO epic lands it is
		// ciphertext nobody here can read. It must not be echoed.
		secret := strings.Repeat("SECRETBODY", 8)
		m, err := store.NewMessage(testBusID, a, true, nil, 11, sentAt, []byte(secret), "k")
		if err != nil {
			t.Fatalf("NewMessage: %v", err)
		}
		rec := recordOf(t, m)
		rec.ContentSHA256 = store.ContentHash([]byte("elsewhere"))
		_, err = store.Decode(encodeRecord(t, rec))
		if err == nil {
			t.Fatal("a hash mismatch was accepted")
		}
		if strings.Contains(err.Error(), "SECRETBODY") {
			t.Fatalf("the content-hash error echoes the body: %v", err)
		}
	})
}

func TestDecodeToleratesUnknownFields(t *testing.T) {
	// Forward compatibility, and deliberately the OPPOSITE of the request
	// decoders: this reads records THIS SERVER WROTE, possibly by a newer build
	// during a rolling restart. Refusing an unknown field here would turn a
	// forward-compatible addition into a refusal to recover.
	a := agentIDFor(t, "alpha")
	m, err := store.NewMessage(testBusID, a, true, nil, 3, time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC), []byte("payload"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	raw, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshalling the encoded record: %v", err)
	}
	// The shape a future CRYPTO-epic envelope descriptor would take.
	generic["envelope"] = map[string]interface{}{"scheme": "x3dh", "epoch": 4}
	generic["future_scalar"] = 42
	augmented, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}

	got, err := store.Decode(json.RawMessage(augmented))
	if err != nil {
		t.Fatalf("Decode rejected a record carrying unknown fields: %v", err)
	}
	if got.ID != m.ID || !bytes.Equal(got.Body, m.Body) {
		t.Fatalf("Decode of an augmented record lost data: got %s / %q, want %s / %q", got.ID, got.Body, m.ID, m.Body)
	}
}

func TestMessageVisibleTo(t *testing.T) {
	a := agentIDFor(t, "alpha")
	b := agentIDFor(t, "beta")
	g := agentIDFor(t, "gamma")
	now := time.Now()

	broadcast, err := store.NewMessage(testBusID, a, true, nil, 1, now, []byte("all"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	dm, err := store.NewMessage(testBusID, a, false, []string{b}, 2, now, []byte("one"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	multi, err := store.NewMessage(testBusID, a, false, []string{b, g}, 3, now, []byte("two"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	selfDM, err := store.NewMessage(testBusID, a, false, []string{a}, 4, now, []byte("self"), "k")
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	// Every fixture message above carries SentAt == now. `enrolled` precedes all
	// of them, so the addressing rows below are not silently answered by the
	// enrolment epoch; the epoch has its own rows at the bottom of the table.
	enrolled := now.Add(-time.Hour)

	cases := []struct {
		name       string
		m          store.Message
		agent      string
		enrolledAt time.Time
		want       bool
	}{
		{"BroadcastSenderExcluded", broadcast, a, enrolled, false},
		{"BroadcastVisibleToOthers", broadcast, b, enrolled, true},
		{"BroadcastVisibleToAThirdParty", broadcast, g, enrolled, true},
		{"BroadcastNeverVisibleToTheEmptyID", broadcast, "", enrolled, false},
		{"DMVisibleToTheNamedRecipient", dm, b, enrolled, true},
		{"DMNotVisibleToAThirdParty", dm, g, enrolled, false},
		{"DMNotVisibleToItsSender", dm, a, enrolled, false},
		{"DMNeverVisibleToTheEmptyID", dm, "", enrolled, false},
		{"MultiRecipientFirst", multi, b, enrolled, true},
		{"MultiRecipientSecond", multi, g, enrolled, true},
		{"MultiRecipientNotTheSender", multi, a, enrolled, false},
		{"SelfAddressedIsStillInvisibleToItsSender", selfDM, a, enrolled, false},
		{"UnknownAgentSeesNoDM", dm, agentIDFor(t, "nobody"), enrolled, false},

		// THE ENROLMENT EPOCH (the P0 from the 2026-08-02 security audit): you do
		// not receive mail sent before you existed, whatever it is addressed to.
		{"EpochBlocksABroadcastSentBeforeEnrolment", broadcast, b, now.Add(time.Nanosecond), false},
		{"EpochBlocksADMSentBeforeEnrolment", dm, b, now.Add(time.Nanosecond), false},
		{"EpochBlocksTheWholeRetentionWindow", multi, g, now.Add(24 * time.Hour), false},
		// The boundary has exactly one spelling: SentAt.Before(enrolledAt), so a
		// message sent AT the enrolment instant is delivered.
		{"SentAtExactlyTheEnrolmentInstantIsVisible", broadcast, b, now, true},
		{"SentOneNanosecondAfterEnrolmentIsVisible", broadcast, b, now.Add(-time.Nanosecond), true},
		// A ZERO epoch disables the check. That is for a roster-less caller — an
		// operator dump, an audit tool — and never for a request path.
		{"ZeroEpochDisablesTheCheck", broadcast, b, time.Time{}, true},
		// …but disabling the epoch never WIDENS addressing: a zero epoch still
		// does not hand a DM to a third party or echo a message to its sender.
		{"ZeroEpochStillDoesNotWidenAddressing", dm, g, time.Time{}, false},
		{"ZeroEpochStillExcludesTheSender", broadcast, a, time.Time{}, false},
	}
	if len(cases) == 0 {
		t.Fatal("the VisibleTo table is empty")
	}
	checked := 0
	for _, c := range cases {
		if got := c.m.VisibleTo(c.agent, c.enrolledAt); got != c.want {
			t.Fatalf("%s: VisibleTo(%q, %v) = %v, want %v", c.name, c.agent, c.enrolledAt, got, c.want)
		}
		checked++
	}
	if checked != len(cases) {
		t.Fatalf("checked %d visibility cases, want %d", checked, len(cases))
	}
}

func TestContentHash(t *testing.T) {
	// One definition of "the hash of this body", so the write path and the
	// recovery check can never disagree.
	cases := []struct {
		body string
		want string
	}{
		// echo -n '' | sha256sum
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		// echo -n 'abc' | sha256sum
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	if len(cases) == 0 {
		t.Fatal("the content-hash table is empty")
	}
	for _, c := range cases {
		if got := store.ContentHash([]byte(c.body)); got != c.want {
			t.Fatalf("ContentHash(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

// busPath builds n distinct well-formed bus ids, so a "path too long" case is
// rejected for its LENGTH and not because a hop is malformed.
func busPath(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("bus-%d", i))
	}
	return out
}

// repeatAgent builds n distinct well-formed recipient ids from a template, so a
// "too many recipients" case is rejected for its LENGTH and not because the ids
// are duplicated or malformed.
func repeatAgent(base string, n int) []string {
	stem := strings.TrimSuffix(base, "-1")
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s-%d", stem, i+1))
	}
	return out
}
