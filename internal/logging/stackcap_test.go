package logging_test

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// The cap under test is spelled as a literal (1024 / 8192) rather than read
// from the package. A test that asserts a constant against itself cannot fail;
// these numbers are the behaviour, so the test states them.

// deepStack recurses depth times and returns debug.Stack() from the bottom, so
// the caller can force a stack of a chosen size rather than hope the one the
// test harness happens to produce is long enough.
//
// That hope is the actual CORE-6 bug. The pre-existing coverage drove a
// panicking handler through httptest, whose call stack rendered 962 bytes --
// under the 1024 cap -- so the test passed while a real net/http request path,
// measured at 1238 bytes, lost its tail in production. A stack's tail is its
// useful half: the deepest frames are where the fault is.
func deepStack(depth int) []byte {
	if depth <= 0 {
		return debug.Stack()
	}
	return deepStack(depth - 1)
}

// stackOverOldCap returns a rendered stack that is comfortably LONGER than the
// old 1024-byte cap and comfortably SHORTER than the new 8192-byte one, by
// searching for the depth that produces it rather than hardcoding one.
//
// The depth is searched, not fixed, because a frame's rendered size depends on
// the absolute path of the source file: the same recursion depth yields ~1 KiB
// in a short checkout path and 14 KiB under a long temp directory. A hardcoded
// depth therefore either fails to reach the old cap (the test passes having
// proved nothing -- the exact CORE-6 blind spot) or sails past the new one
// (the test measures the new limit instead of the exemption). Neither is what
// this test is for.
func stackOverOldCap(t *testing.T) string {
	t.Helper()
	const (
		mustExceed    = 1024 // the OLD cap: below this, truncation is never exercised
		mustStayUnder = 6144 // slack under the NEW 8192 cap
	)
	for depth := 0; depth <= 256; depth++ {
		s := string(deepStack(depth))
		if len(s) > mustExceed {
			if len(s) >= mustStayUnder {
				t.Fatalf("the shortest stack exceeding %d bytes is already %d, at or over this test's %d-byte ceiling; one frame renders too large here to measure the exemption",
					mustExceed, len(s), mustStayUnder)
			}
			return s
		}
	}
	t.Fatalf("no recursion depth up to 256 produced a stack longer than %d bytes", mustExceed)
	return ""
}

// TestPanicStackNotTruncated pins CORE-6 at the logger's own level: the
// `stack` field is exempt from maxValueLen, every other field is not.
func TestPanicStackNotTruncated(t *testing.T) {
	stack := stackOverOldCap(t)

	// GUARD, restated at the point of use: without it the whole test could
	// pass on a stack that never reached the old cap -- exactly the blind spot
	// being fixed.
	if len(stack) <= 1024 {
		t.Fatalf("the generated stack is only %d bytes, at or under the OLD 1024-byte cap, so nothing below exercises truncation", len(stack))
	}

	t.Run("the stack field survives intact", func(t *testing.T) {
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		lg.Error("panic serving request", "stack", stack)

		out := buf.String()
		if strings.Contains(out, "...(truncated)") {
			t.Fatalf("the stack field was truncated at the 1024-byte cap (stack is %d bytes).\n"+
				"THE CORE-6 DEFECT: the deepest frames -- where the panic actually happened -- are the ones the tail cut discards.",
				len(stack))
		}
		if !strings.Contains(out, "deepStack") {
			t.Fatalf("the emitted record does not name deepStack, so the deep frames did not survive:\n%s", out)
		}
		if !strings.Contains(out, "TestPanicStackNotTruncated") {
			t.Fatalf("the emitted record does not reach the test frame; the stack was cut short:\n%s", out)
		}
		// One line, still. The exemption raises the cap; it must not weaken
		// the quoting that keeps a record to a single line of printable ASCII.
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("the stack field broke the one-line-per-record rule: %d newlines", strings.Count(out, "\n"))
		}
	})

	t.Run("every OTHER field is still capped at 1024", func(t *testing.T) {
		// The exemption must be narrow. maxValueLen exists so an
		// attacker-controlled value -- a header, a request id, an error string
		// built from client input -- cannot turn one record into a multi-
		// kilobyte payload, and that reasoning is untouched for every field
		// but `stack`, whose content comes from runtime/debug.
		for _, key := range []string{"err", "panic", "request_id", "stacks", "the_stack", "STACK"} {
			key := key
			t.Run(key, func(t *testing.T) {
				var buf bytes.Buffer
				lg := logging.New(&buf, logging.LevelInfo)
				lg.Error("evt", key, strings.Repeat("A", 4096))

				if !strings.Contains(buf.String(), "...(truncated)") {
					t.Fatalf("a 4096-byte %q value was NOT truncated; the cap must stay at %d for every field but the exempt one",
						key, 1024)
				}
			})
		}
	})

	t.Run("the stack exemption is itself bounded", func(t *testing.T) {
		// 8192 is a bound, not "unlimited". A pathological goroutine stack --
		// or a non-panic call site that happens to use this key -- must still
		// not emit an unbounded line.
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		lg.Error("evt", "stack", strings.Repeat("B", 8193))

		if !strings.Contains(buf.String(), "...(truncated)") {
			t.Fatal("an 8193-byte stack value was not truncated; the exemption must raise the cap, not remove it")
		}
	})

	t.Run("the msg field is still capped", func(t *testing.T) {
		// msg goes through writeValue without a key, so it must keep the
		// default limit -- a regression here would let a long message bypass
		// the bound entirely.
		var buf bytes.Buffer
		lg := logging.New(&buf, logging.LevelInfo)
		lg.Error(strings.Repeat("C", 4096))
		if !strings.Contains(buf.String(), "...(truncated)") {
			t.Fatal("a 4096-byte msg was not truncated")
		}
	})
}
