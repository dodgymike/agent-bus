package store_test

import (
	"errors"
	"testing"

	"github.com/dodgymike/agent-bus/internal/idem"
	"github.com/dodgymike/agent-bus/internal/store"
)

// TestConversationCreateIdempotency is CONV-CREATE-CLI's proof for the three
// invariant-10 cases of a create, NOT collapsed, exercised through the
// production three-step wiring (openConvStore) so the applied-key record's
// live-write and replay paths are both real.
func TestConversationCreateIdempotency(t *testing.T) {
	t.Run("same key + same payload is a legitimate retry: the ORIGINAL conversation, replayed=true, no second record", func(t *testing.T) {
		dir := t.TempDir()
		st, l, _ := openConvStore(t, dir)
		defer l.Close()

		recipients := []string{convRecipient, "testbus.carol-2"}
		first, replayed, err := st.CreateIdempotent(convCreator, "triage", recipients, "key-A")
		if err != nil {
			t.Fatalf("first create: %v", err)
		}
		if replayed {
			t.Fatalf("first create reported replayed=true; it is the original")
		}
		if st.Len() != 1 {
			t.Fatalf("after one create the table holds %d, want 1", st.Len())
		}

		second, replayed, err := st.CreateIdempotent(convCreator, "triage", recipients, "key-A")
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if !replayed {
			t.Fatalf("retry reported replayed=false; a same-key-same-payload retry must return the original (invariant 10)")
		}
		if second.ID != first.ID {
			t.Fatalf("retry minted a SECOND conversation %q, want the original %q", second.ID, first.ID)
		}
		if st.Len() != 1 {
			t.Fatalf("a retry created a second record: table holds %d, want 1", st.Len())
		}
	})

	t.Run("same key + different payload is a protocol violation: ErrConversationKeyReused, nothing minted", func(t *testing.T) {
		dir := t.TempDir()
		st, l, _ := openConvStore(t, dir)
		defer l.Close()

		if _, _, err := st.CreateIdempotent(convCreator, "triage", []string{convRecipient}, "key-B"); err != nil {
			t.Fatalf("first create: %v", err)
		}
		before := st.Len()

		// A different recipient list under the same key.
		_, _, err := st.CreateIdempotent(convCreator, "triage", []string{"testbus.carol-2"}, "key-B")
		if !errors.Is(err, store.ErrConversationKeyReused) {
			t.Fatalf("key reuse with a different payload err = %v, want ErrConversationKeyReused", err)
		}
		if st.Len() != before {
			t.Fatalf("a violation minted a conversation: table went from %d to %d", before, st.Len())
		}

		// A different NAME under the same key is also a violation.
		if _, _, err := st.CreateIdempotent(convCreator, "different-name", []string{convRecipient}, "key-B"); !errors.Is(err, store.ErrConversationKeyReused) {
			t.Fatalf("key reuse with a different name err = %v, want ErrConversationKeyReused", err)
		}
		if st.Len() != before {
			t.Fatalf("a name-mismatch violation minted a conversation: table now %d, want %d", st.Len(), before)
		}
	})

	t.Run("a different key mints a fresh conversation", func(t *testing.T) {
		dir := t.TempDir()
		st, l, _ := openConvStore(t, dir)
		defer l.Close()

		a, _, err := st.CreateIdempotent(convCreator, "one", []string{convRecipient}, "key-C1")
		if err != nil {
			t.Fatalf("create one: %v", err)
		}
		b, _, err := st.CreateIdempotent(convCreator, "two", []string{convRecipient}, "key-C2")
		if err != nil {
			t.Fatalf("create two: %v", err)
		}
		if a.ID == b.ID {
			t.Fatalf("two different keys produced the same conversation id %q", a.ID)
		}
		if st.Len() != 2 {
			t.Fatalf("two distinct creates left %d records, want 2", st.Len())
		}
	})

	t.Run("a missing or malformed idempotency key is refused before anything is minted", func(t *testing.T) {
		dir := t.TempDir()
		st, l, _ := openConvStore(t, dir)
		defer l.Close()

		if _, _, err := st.CreateIdempotent(convCreator, "x", []string{convRecipient}, ""); !errors.Is(err, idem.ErrMissingKey) {
			t.Fatalf("empty key err = %v, want idem.ErrMissingKey", err)
		}
		if _, _, err := st.CreateIdempotent(convCreator, "x", []string{convRecipient}, "bad key with spaces"); !errors.Is(err, idem.ErrInvalidKey) {
			t.Fatalf("malformed key err = %v, want idem.ErrInvalidKey", err)
		}
		if st.Len() != 0 {
			t.Fatalf("a refused create minted a record: table holds %d, want 0", st.Len())
		}
	})

	t.Run("an unqualified recipient is refused before anything is minted", func(t *testing.T) {
		dir := t.TempDir()
		st, l, _ := openConvStore(t, dir)
		defer l.Close()
		if _, _, err := st.CreateIdempotent(convCreator, "x", []string{"bob-1"}, "key-D"); !errors.Is(err, store.ErrInvalidConversation) {
			t.Fatalf("unqualified recipient err = %v, want ErrInvalidConversation", err)
		}
		if st.Len() != 0 {
			t.Fatalf("a refused create minted a record: table holds %d, want 0", st.Len())
		}
	})
}

// TestConversationCreateIdempotencyDurableAcrossRestart is the durability proof
// (invariant 10, "durable across restart"): the conversation AND its
// idempotency key survive a close-and-reopen, so a retry after the restart
// returns the ORIGINAL conversation rather than minting a second. This is the
// property the applied-key record riding in the same wal.Entry, and Apply
// recovering it on replay, exists to deliver.
func TestConversationCreateIdempotencyDurableAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	st, l, _ := openConvStore(t, dir)
	recipients := []string{convRecipient, "testbus.carol-2"}
	original, _, err := st.CreateIdempotent(convCreator, "durable", recipients, "restart-key")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// REOPEN: a fresh store replays the same log. Both the conversation record
	// AND its applied-key record are rebuilt by Apply.
	st2, l2, _ := openConvStore(t, dir)
	defer l2.Close()

	if got, ok := st2.Get(original.ID); !ok || got.Name != "durable" {
		t.Fatalf("the conversation did not survive the restart: ok=%v got=%+v", ok, got)
	}
	if st2.Len() != 1 {
		t.Fatalf("after restart the table holds %d conversations, want 1", st2.Len())
	}

	// A retry after the restart, same key + same payload, must return the
	// ORIGINAL — the applied-key record was recovered from the log.
	replayedRec, replayed, err := st2.CreateIdempotent(convCreator, "durable", recipients, "restart-key")
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if !replayed {
		t.Fatalf("retry after restart reported replayed=false; the idempotency key did not survive the restart (invariant 10)")
	}
	if replayedRec.ID != original.ID {
		t.Fatalf("retry after restart minted a SECOND conversation %q, want the original %q", replayedRec.ID, original.ID)
	}
	if st2.Len() != 1 {
		t.Fatalf("retry after restart created a second record: table holds %d, want 1", st2.Len())
	}

	// And a DIFFERENT payload under the same key is still a violation after the
	// restart — the recovered key remembers its fingerprint.
	if _, _, err := st2.CreateIdempotent(convCreator, "durable", []string{convRecipient}, "restart-key"); !errors.Is(err, store.ErrConversationKeyReused) {
		t.Fatalf("post-restart key reuse with a different payload err = %v, want ErrConversationKeyReused", err)
	}
}
