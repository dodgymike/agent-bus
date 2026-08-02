package logging_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/internal/logging"
)

// TestParseLevel is a cheap round-trip check: every accepted spelling maps
// to the expected Level and back through String(), and unknown input is an
// error rather than a silently-wrong verbosity.
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    logging.Level
		wantErr bool
	}{
		{"debug", logging.LevelDebug, false},
		{"DEBUG", logging.LevelDebug, false},
		{"  info  ", logging.LevelInfo, false},
		{"warn", logging.LevelWarn, false},
		{"warning", logging.LevelWarn, false},
		{"WARNING", logging.LevelWarn, false},
		{"error", logging.LevelError, false},
		{"bogus", logging.LevelInfo, true},
		{"", logging.LevelInfo, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := logging.ParseLevel(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, nil; want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if got.String() != tc.want.String() {
				t.Fatalf("round trip String() = %q, want %q", got.String(), tc.want.String())
			}
		})
	}
}

// TestValueQuotingEscapesControlChars pins the log-injection defence at the
// package's own level: any value containing whitespace, quotes or control
// characters must come out quoted/escaped on a single line.
func TestValueQuotingEscapesControlChars(t *testing.T) {
	var buf bytes.Buffer
	lg := logging.New(&buf, logging.LevelInfo)
	lg.Info("evt", "clean", "bareword", "spacey", "a b", "newline", "a\nb", "quote", `a"b`)

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("record contains an unescaped newline, breaking one-line-per-record: %q", out)
	}
	if !strings.Contains(out, "clean=bareword") {
		t.Fatalf("bare token was unexpectedly quoted: %q", out)
	}
	if !strings.Contains(out, `spacey="a b"`) {
		t.Fatalf("space-containing value not quoted: %q", out)
	}
	if !strings.Contains(out, `newline="a\nb"`) {
		t.Fatalf("embedded newline not escaped: %q", out)
	}
	if !strings.Contains(out, `quote="a\"b"`) {
		t.Fatalf("embedded quote not escaped: %q", out)
	}
}

// TestLevelFilteringSuppressesBelowConfigured is the package-level companion
// to the middleware-level check in internal/httpapi.
func TestLevelFilteringSuppressesBelowConfigured(t *testing.T) {
	var buf bytes.Buffer
	lg := logging.New(&buf, logging.LevelWarn)
	lg.Debug("should not appear")
	lg.Info("should not appear either")
	if buf.Len() != 0 {
		t.Fatalf("Debug/Info emitted below configured Warn level: %q", buf.String())
	}
	lg.Warn("should appear")
	if buf.Len() == 0 {
		t.Fatalf("Warn was suppressed at configured Warn level")
	}
}
