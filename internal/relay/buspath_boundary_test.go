package relay_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodgymike/agent-bus/internal/attest"
	"github.com/dodgymike/agent-bus/internal/hub"
	"github.com/dodgymike/agent-bus/internal/ids"
	"github.com/dodgymike/agent-bus/internal/relay"
	"github.com/dodgymike/agent-bus/internal/signing"
	"github.com/dodgymike/agent-bus/internal/store"
	"github.com/dodgymike/agent-bus/internal/wal"
)

// TestBusPathBoundaryAgreesAcrossRelayAndHub pins one received-path boundary
// across wire validation, hub ingest, and durable storage. The hub appends its
// own hop, so the largest accepted wire path becomes exactly MaxBusPath on
// disk; one more received hop is refused by both components before a write.
func TestBusPathBoundaryAgreesAcrossRelayAndHub(t *testing.T) {
	const (
		originBus = "boundary-origin"
		localBus  = "boundary-local"
	)
	path := make([]string, store.MaxReceivedBusPath+1)
	for i := range path {
		path[i] = fmt.Sprintf("boundary-hop-%02d", i)
	}
	path[0] = originBus

	accepted := path[:store.MaxReceivedBusPath]
	if err := relay.ValidateBusPath(accepted); err != nil {
		t.Fatalf("relay refused the maximum received path (%d hops): %v", len(accepted), err)
	}
	h, log := boundaryHub(t, localBus)
	req := boundaryRequest(t, originBus, localBus, 901, accepted)
	if _, err := h.IngestRelayed(context.Background(), req); err != nil {
		t.Fatalf("hub refused the path relay accepted: %v", err)
	}
	replayed := boundaryReplayMessages(t, log.Path())
	if len(replayed) != 1 {
		t.Fatalf("durable message count = %d, want 1", len(replayed))
	}
	if got := len(replayed[0].BusPath); got != store.MaxBusPath {
		t.Fatalf("persisted path has %d hops, want durable maximum %d after local append", got, store.MaxBusPath)
	}
	if replayed[0].BusPath[len(replayed[0].BusPath)-1] != localBus {
		t.Fatalf("persisted path does not end with local bus %q: %v", localBus, replayed[0].BusPath)
	}

	tooLong := path
	if err := relay.ValidateBusPath(tooLong); !errors.Is(err, relay.ErrBusPathTooLong) {
		t.Fatalf("relay error for %d received hops = %v, want ErrBusPathTooLong", len(tooLong), err)
	}
	h2, log2 := boundaryHub(t, localBus)
	req = boundaryRequest(t, originBus, localBus, 902, tooLong)
	if _, err := h2.IngestRelayed(context.Background(), req); !errors.Is(err, hub.ErrInvalidBusPath) {
		t.Fatalf("hub error for %d received hops = %v, want ErrInvalidBusPath", len(tooLong), err)
	}
	if got := len(boundaryReplayMessages(t, log2.Path())); got != 0 {
		t.Fatalf("hub durably wrote %d messages after rejecting an oversized received path, want 0", got)
	}
}

func boundaryHub(t *testing.T, busID string) (*hub.Hub, *wal.Log) {
	t.Helper()
	dir := t.TempDir()
	log, err := wal.Open(wal.LogOptions{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	roster := hub.NewStaticRoster()
	recipient, err := ids.AgentID(busID, "recipient", 1)
	if err != nil {
		t.Fatalf("ids.AgentID: %v", err)
	}
	roster.Add(hub.Agent{AgentID: recipient, Name: "recipient", EnrolledAt: time.Now().Add(-time.Hour)})
	h, err := hub.Open(hub.Options{
		BusID:     busID,
		DataDir:   dir,
		Durable:   log,
		Replay:    func(fn func(wal.Committed) error) (wal.Recovered, error) { return wal.Replay(log.Path(), fn) },
		NextIndex: log.Recovered().NextIndex,
		Roster:    roster,
	})
	if err != nil {
		t.Fatalf("hub.Open: %v", err)
	}
	return h, log
}

func boundaryRequest(t *testing.T, originBus, localBus string, seq uint64, path []string) hub.RelayedIngestRequest {
	t.Helper()
	sender, err := ids.AgentID(originBus, "sender", 1)
	if err != nil {
		t.Fatalf("sender id: %v", err)
	}
	recipient, err := ids.AgentID(localBus, "recipient", 1)
	if err != nil {
		t.Fatalf("recipient id: %v", err)
	}
	messageID, err := ids.MessageID(originBus, seq)
	if err != nil {
		t.Fatalf("message id: %v", err)
	}
	now := time.Now().UnixMilli()
	return hub.RelayedIngestRequest{
		Sender: sender, Recipients: []string{recipient}, Body: []byte("boundary"),
		OriginMessageID: messageID, BusPath: path, TimestampUnixMilli: now,
		Signature: make([]byte, 64),

		// RELAY-48 made this MANDATORY: the hub refuses a relayed ingest that
		// carries no origin attestation, because without one the message cannot
		// be rebuilt into an origin envelope after a restart and its onward hop
		// is abandoned. This fixture is about the BUS-PATH boundary, so the
		// attestation only has to be well-formed — it is never verified here.
		// Shape copied from RELAY-48's own r48Attestation rather than invented,
		// so the two stay in step.
		OriginAttestation: attest.Attestation{
			AgentID:            sender,
			MessagingPublicKey: bytes.Repeat([]byte{0x5A}, ed25519.PublicKeySize),
			KeyEpoch:           99,
			IssuedAtUnixMilli:  now,
			NotAfterUnixMilli:  now + 86_400_000,
			Signature:          bytes.Repeat([]byte{0xA5}, signing.SignatureSize),
		},
	}
}

func boundaryReplayMessages(t *testing.T, path string) []store.Message {
	t.Helper()
	var messages []store.Message
	if _, err := wal.Replay(filepath.Clean(path), func(committed wal.Committed) error {
		if committed.Entry.Kind != store.RecordKind {
			return nil
		}
		message, err := store.Decode(committed.Entry.Body)
		if err != nil {
			return err
		}
		messages = append(messages, message)
		return nil
	}); err != nil {
		t.Fatalf("wal.Replay: %v", err)
	}
	return messages
}
