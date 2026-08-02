// Package logging is agent-bus's small structured logger, built on stdlib log.
//
// It exists because log/slog landed in go1.21 and this project targets the
// toolchain on the box (go1.19.4), and because invariant 8 ("simple beats
// clever") rules out pulling in a third-party logging dependency for what is
// a hundred lines of formatting.
//
// Output is logfmt-style key=value on one line per event:
//
//	ts=2026-08-02T09:14:02.311Z level=info msg="request" method=GET path=/healthz status=200
//
// logfmt was chosen over JSON because these logs are read by humans tailing a
// dev server and grepped by shell wrappers, and because a single line per
// event keeps the format stable when it is later fed to a log shipper. Values
// are quoted with strconv.Quote whenever they contain whitespace, quotes, '='
// or any control character, which also neutralises log injection: no attacker
// controlled value can emit a newline and forge a second log record.
package logging

import (
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"
)

// Level is a log severity. The zero value is LevelDebug, the most verbose.
type Level int

// Severity levels, in increasing order.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String returns the lowercase name of the level.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "level(" + strconv.Itoa(int(l)) + ")"
	}
}

// ParseLevel maps a flag value such as "info" onto a Level. It is
// case-insensitive and accepts "warning" as an alias for "warn". An unknown
// value is an error so the server can fail fast rather than start with a
// silently wrong verbosity.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q (want one of: debug, info, warn, error)", s)
	}
}

// Levels lists the accepted -log-level values, for usage strings.
const Levels = "debug|info|warn|error"

// maxValueLen bounds a single formatted field value. A log record is not a
// transport for arbitrary payloads; anything longer is truncated.
const maxValueLen = 1024

// Logger writes structured records at or above its level. It is safe for
// concurrent use: the level is fixed at construction and all writes go through
// a single *log.Logger, whose Output is mutex-protected.
type Logger struct {
	out   *log.Logger
	level Level
	base  string // pre-rendered fields appended to every record, e.g. " component=httpapi"
}

// New returns a Logger writing records to w at or above level.
func New(w io.Writer, level Level) *Logger {
	// Flag 0: this package renders its own ts= field so the timestamp is a
	// structured field like every other, not a bare prefix.
	return &Logger{out: log.New(w, "", 0), level: level}
}

// With returns a copy of the Logger whose records always carry the given
// key/value pairs. The receiver is unchanged.
func (l *Logger) With(kv ...interface{}) *Logger {
	if l == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString(l.base)
	appendPairs(&b, kv)
	return &Logger{out: l.out, level: l.level, base: b.String()}
}

// Level reports the minimum severity this Logger emits.
func (l *Logger) Level() Level {
	if l == nil {
		return LevelError
	}
	return l.level
}

// Enabled reports whether a record at level would be emitted. Callers use it
// to skip expensive field construction.
func (l *Logger) Enabled(level Level) bool {
	return l != nil && level >= l.level
}

// Debug logs at LevelDebug. Trailing arguments are alternating key/value pairs.
func (l *Logger) Debug(msg string, kv ...interface{}) { l.log(LevelDebug, msg, kv) }

// Info logs at LevelInfo.
func (l *Logger) Info(msg string, kv ...interface{}) { l.log(LevelInfo, msg, kv) }

// Warn logs at LevelWarn.
func (l *Logger) Warn(msg string, kv ...interface{}) { l.log(LevelWarn, msg, kv) }

// Error logs at LevelError.
func (l *Logger) Error(msg string, kv ...interface{}) { l.log(LevelError, msg, kv) }

func (l *Logger) log(level Level, msg string, kv []interface{}) {
	if !l.Enabled(level) {
		return
	}
	var b strings.Builder
	b.WriteString("ts=")
	b.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteString(" level=")
	b.WriteString(level.String())
	b.WriteString(" msg=")
	writeValue(&b, msg)
	b.WriteString(l.base)
	appendPairs(&b, kv)
	// log.Logger.Output serialises concurrent writers.
	_ = l.out.Output(0, b.String())
}

// appendPairs renders alternating key/value arguments as " key=value". An odd
// trailing argument is reported rather than dropped, so a miswired call site
// is visible in the logs instead of silently losing data.
func appendPairs(b *strings.Builder, kv []interface{}) {
	for i := 0; i < len(kv); i += 2 {
		if i+1 >= len(kv) {
			b.WriteString(" !badkey=")
			writeValue(b, format(kv[i]))
			return
		}
		b.WriteByte(' ')
		b.WriteString(sanitiseKey(format(kv[i])))
		b.WriteByte('=')
		writeValue(b, format(kv[i+1]))
	}
}

func format(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "<nil>"
	case string:
		return t
	case error:
		if t == nil {
			return "<nil>"
		}
		return t.Error()
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// sanitiseKey keeps keys to a boring, greppable alphabet. Keys come from code,
// not from the wire, so anything else is a bug worth making visible.
func sanitiseKey(k string) string {
	if k == "" {
		return "!emptykey"
	}
	ok := true
	for i := 0; i < len(k); i++ {
		if !isKeyByte(k[i]) {
			ok = false
			break
		}
	}
	if ok {
		return k
	}
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		if isKeyByte(k[i]) {
			b.WriteByte(k[i])
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func isKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-'
}

// writeValue emits a value, quoting it whenever it is not a bare token. This
// is the log-injection defence: strconv.Quote escapes newlines and every other
// control character, so a value taken from an untrusted header can never end
// the record and start a forged one.
func writeValue(b *strings.Builder, v string) {
	if len(v) > maxValueLen {
		v = v[:maxValueLen] + "...(truncated)"
	}
	if needsQuote(v) {
		b.WriteString(strconv.Quote(v))
		return
	}
	b.WriteString(v)
}

func needsQuote(v string) bool {
	if v == "" {
		return true
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c <= ' ' || c == 0x7f || c == '"' || c == '=' || c == '\\' {
			return true
		}
	}
	return false
}
