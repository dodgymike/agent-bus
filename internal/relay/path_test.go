package relay

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/store"
)

// TestRelayLoopPrevention is RELAY-3's proof at the unit level: every way a
// traversed path can be malformed is refused, and a path this bus is already on
// is dropped — including when it is spelled in a different ASCII case.
func TestRelayLoopPrevention(t *testing.T) {
	overCap := make([]string, MaxReceivedBusPath+1)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("bus-%d", i)
	}
	atCap := make([]string, MaxReceivedBusPath)
	for i := range atCap {
		atCap[i] = fmt.Sprintf("bus-%d", i)
	}

	cases := []struct {
		name    string
		path    []string
		want    error
		because string
	}{
		{
			name:    "empty path",
			path:    nil,
			want:    ErrInvalidBusPath,
			because: "every relayed message has traversed at least the bus that originated it",
		},
		{
			name:    "over the hop cap",
			path:    overCap,
			want:    ErrBusPathTooLong,
			because: "the length is refused before any hop is parsed",
		},
		{
			name:    "malformed hop",
			path:    []string{peerBus, "bus.with.dots"},
			want:    ErrInvalidBusPath,
			because: "'.' is the qualification separator and may not appear in a bus id",
		},
		{
			name:    "oversized hop",
			path:    []string{strings.Repeat("z", MaxPeerBusIDLen+1)},
			want:    ErrInvalidBusPath,
			because: "an oversized hop is refused before ids.ValidateBusID quotes it",
		},
		{
			name:    "duplicate hop",
			path:    []string{peerBus, thirdBus, peerBus},
			want:    ErrInvalidBusPath,
			because: "a repeated hop is a completed loop or a fabrication; the message is not routable either way",
		},
		{
			name:    "duplicate hop in a different case",
			path:    []string{peerBus, strings.ToUpper(peerBus)},
			want:    ErrInvalidBusPath,
			because: "hops are compared case-insensitively, as ids.BusIDPattern admits both cases",
		},
		{
			name:    "this bus is on the path",
			path:    []string{peerBus, localBus, thirdBus},
			want:    ErrRelayLoop,
			because: "the message has been here before; that is RELAY-3's rule",
		},
		{
			name:    "this bus is on the path in a different case",
			path:    []string{peerBus, strings.ToUpper(localBus)},
			want:    ErrRelayLoop,
			because: "a case-sensitive membership test would let one flipped byte spin the cycle forever",
		},
		{
			name:    "this bus is absent",
			path:    []string{peerBus, thirdBus},
			because: "a message that has not been here is accepted",
		},
		{
			name:    "a path exactly at the cap is accepted",
			path:    atCap,
			because: "the cap is a limit, not an off-by-one",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := CheckIncomingPath(localBus, tc.path)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("CheckIncomingPath: %v (%s)", err, tc.because)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("CheckIncomingPath error = %v, want one wrapping %v (%s)", err, tc.want, tc.because)
			}
		})
	}
}

// TestRelayLoopPreventionDoesNotEchoAnOversizedHop pins the ordering inside
// validateHops: the length check runs BEFORE ids.ValidateBusID, whose message
// quotes the id with %q. A peer must not get to choose the size of the line we
// log about refusing it.
func TestRelayLoopPreventionDoesNotEchoAnOversizedHop(t *testing.T) {
	huge := strings.Repeat("z", 200_000)
	err := CheckIncomingPath(localBus, []string{huge})
	if !errors.Is(err, ErrInvalidBusPath) {
		t.Fatalf("error = %v, want one wrapping ErrInvalidBusPath", err)
	}
	if strings.Contains(err.Error(), huge[:1000]) {
		t.Fatalf("the error echoes the oversized hop (%d bytes of message)", len(err.Error()))
	}
	if len(err.Error()) > 500 {
		t.Fatalf("error message is %d bytes; an oversized hop must not size the log line", len(err.Error()))
	}
}

func TestRelayLoopPreventionAppendHop(t *testing.T) {
	t.Run("appends our hop", func(t *testing.T) {
		got, err := AppendHop([]string{peerBus}, localBus)
		if err != nil {
			t.Fatalf("AppendHop: %v", err)
		}
		if len(got) != 2 || got[0] != peerBus || got[1] != localBus {
			t.Fatalf("AppendHop = %v, want [%s %s]", got, peerBus, localBus)
		}
	})

	t.Run("an empty path means we are the origin", func(t *testing.T) {
		// The ONE legitimate empty path: ValidateBusPath refuses it on INGRESS
		// (a message that arrived has traversed its origin), but on EGRESS from
		// the origin there is nothing yet to have traversed.
		got, err := AppendHop(nil, localBus)
		if err != nil {
			t.Fatalf("AppendHop(nil): %v", err)
		}
		if len(got) != 1 || got[0] != localBus {
			t.Fatalf("AppendHop(nil) = %v, want [%s]", got, localBus)
		}
	})

	t.Run("refuses to append a hop already on the path", func(t *testing.T) {
		if _, err := AppendHop([]string{peerBus, localBus}, localBus); !errors.Is(err, ErrRelayLoop) {
			t.Fatalf("error = %v, want one wrapping ErrRelayLoop", err)
		}
		if _, err := AppendHop([]string{strings.ToUpper(localBus)}, localBus); !errors.Is(err, ErrRelayLoop) {
			t.Fatalf("error = %v, want one wrapping ErrRelayLoop for a case variant", err)
		}
	})

	t.Run("maximum received path retains one local hop", func(t *testing.T) {
		atCap := make([]string, MaxReceivedBusPath)
		for i := range atCap {
			atCap[i] = fmt.Sprintf("bus-%d", i)
		}
		got, err := AppendHop(atCap, localBus)
		if err != nil {
			t.Fatalf("AppendHop at the maximum received boundary: %v", err)
		}
		if len(got) != store.MaxBusPath {
			t.Fatalf("appended path length = %d, want durable maximum %d", len(got), store.MaxBusPath)
		}
	})

	t.Run("returns a fresh slice", func(t *testing.T) {
		// A peer-supplied slice with spare capacity must not be written
		// through: one outbound forward would silently rewrite the path another
		// is about to read.
		backing := make([]string, 1, 8)
		backing[0] = peerBus
		got, err := AppendHop(backing, localBus)
		if err != nil {
			t.Fatalf("AppendHop: %v", err)
		}
		got[0] = thirdBus
		if backing[0] != peerBus {
			t.Fatal("AppendHop wrote through the caller's backing array")
		}
		if cap(backing) > 1 && len(backing) < cap(backing) {
			// Prove the spare capacity was not used for our hop.
			spare := backing[:cap(backing)]
			if spare[1] == localBus {
				t.Fatal("AppendHop appended into the caller's spare capacity")
			}
		}
	})
}

// TestRelayLoopPreventionSplitHorizon pins the division of labour between the
// egress optimisation and the ingress backstop.
func TestRelayLoopPreventionSplitHorizon(t *testing.T) {
	path := []string{peerBus, thirdBus}
	if NextHopAllowed(path, peerBus) {
		t.Error("the split horizon forwarded back to a bus already on the path")
	}
	if NextHopAllowed(path, strings.ToUpper(thirdBus)) {
		t.Error("the split horizon was defeated by an ASCII case change")
	}
	if !NextHopAllowed(path, "bus-fresh") {
		t.Error("the split horizon refused a bus that has not seen the message")
	}

	// The backstop still works when the peer LIES and strips itself out. This
	// is the case NextHopAllowed cannot cover, and it is why both exist.
	lying := []string{peerBus, localBus}
	if err := CheckIncomingPath(localBus, lying); !errors.Is(err, ErrRelayLoop) {
		t.Fatalf("the ingress backstop did not fire: %v", err)
	}
}

// TestMaxBusPathIsHardLinkedToTheDurableCap proves the relay ingress cap can
// never exceed the on-disk cap. A path we accepted that store.Decode would
// refuse is a message we would acknowledge and then fail to persist — the
// acknowledged-but-lost message invariant 5 forbids.
func TestMaxBusPathIsHardLinkedToTheDurableCap(t *testing.T) {
	atCap := make([]string, MaxReceivedBusPath)
	for i := range atCap {
		atCap[i] = fmt.Sprintf("bus-%d", i)
	}
	if err := validateHops(atCap); err != nil {
		t.Fatalf("a path of exactly MaxReceivedBusPath hops was refused: %v", err)
	}
	if err := validateHops(append(atCap, "bus-onemore")); !errors.Is(err, ErrBusPathTooLong) {
		t.Fatalf("error = %v, want one wrapping ErrBusPathTooLong", err)
	}
}
