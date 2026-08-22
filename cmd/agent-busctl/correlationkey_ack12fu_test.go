package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// ACK-12-FU-WATCH-CORRELATION-KEY — the CLI half.
//
// The end-to-end proof is TestThreeBusEndToEndAckNack in tests/e2e, and it
// drives `watch --json`. These cases cover what that run cannot reach: the
// HUMAN render (the e2e never asks for it) and the empty-key encoding (a live
// bus never serves one).

// TestNewWatchRecordCarriesCorrelationKeyVerbatim pins the carry. The value is
// computed server-side, in store.Message.OriginID(); this layer may only copy
// it. Deriving it here — "origin id when set, local id otherwise" — is the
// re-spelled branch whose wrong arm still yields a well-formed message id.
func TestNewWatchRecordCarriesCorrelationKeyVerbatim(t *testing.T) {
	tests := []struct {
		name           string
		messageID      string
		correlationKey string
	}{
		{"relayed", "bus-localaaaaaaaaaa-11", "bus-originbbbbbbbbb-4224"},
		{"same_bus", "bus-localaaaaaaaaaa-11", "bus-localaaaaaaaaaa-11"},
		{
			// A bus that did not send the field at all. The CLI must surface
			// the emptiness rather than substitute message_id: substituting is
			// how a recipient silently acks the WRONG id.
			"absent_from_the_bus", "bus-localaaaaaaaaaa-11", "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := newWatchRecord(client.Message{
				MessageID:      tc.messageID,
				CorrelationKey: tc.correlationKey,
			})
			if r.CorrelationKey != tc.correlationKey {
				t.Fatalf("watchRecord.CorrelationKey = %q, want the bus's value %q verbatim",
					r.CorrelationKey, tc.correlationKey)
			}
			if r.MessageID != tc.messageID {
				t.Fatalf("watchRecord.MessageID = %q, want %q", r.MessageID, tc.messageID)
			}
		})
	}
}

// TestWatchRecordAlwaysEncodesCorrelationKey pins the deliberate absence of
// `omitempty` on the JSON the agent actually parses.
func TestWatchRecordAlwaysEncodesCorrelationKey(t *testing.T) {
	raw, err := json.Marshal(newWatchRecord(client.Message{MessageID: "bus-localaaaaaaaaaa-11"}))
	if err != nil {
		t.Fatalf("marshal watchRecord: %v", err)
	}
	if !strings.Contains(string(raw), `"correlation_key":""`) {
		t.Fatalf("a record with no correlation key encoded to %s, which does not contain "+
			"`\"correlation_key\":\"\"`. The field must NOT be `omitempty`: `jq -r .correlation_key` has to "+
			"be one instruction for a relayed and a same-bus message alike, and an absent field would make "+
			"every consumer write `.correlation_key // .message_id` — a silent fallback to the id the ack "+
			"path refuses.", raw)
	}
}

// TestWatchHumanRenderShowsTheAckKeyOnlyWhenItDiffers covers the render an
// interactive human reads, which the JSON-driven e2e never exercises. The line
// exists because the human feed otherwise prints no id at all, so a relayed
// message would be unackable from it; and it is CONDITIONAL because on a
// same-bus message it would repeat the message id on every single line.
func TestWatchHumanRenderShowsTheAckKeyOnlyWhenItDiffers(t *testing.T) {
	const ackKeyLine = "  ack key: "

	tests := []struct {
		name    string
		msg     client.Message
		wantKey string // "" means the line must be absent
		why     string
	}{
		{
			name: "relayed_prints_the_origin_key",
			msg: client.Message{
				MessageID:      "bus-localaaaaaaaaaa-11",
				CorrelationKey: "bus-originbbbbbbbbb-4224",
				From:           "bus-originbbbbbbbbb.sender-1",
				SentAt:         "2026-08-22T06:41:30Z",
				Body:           []byte("relayed body"),
			},
			wantKey: "bus-originbbbbbbbbb-4224",
			why:     "without it the human feed names no id a recipient could ack a relayed message with",
		},
		{
			name: "same_bus_prints_nothing",
			msg: client.Message{
				MessageID:      "bus-localaaaaaaaaaa-11",
				CorrelationKey: "bus-localaaaaaaaaaa-11",
				From:           "bus-localaaaaaaaaaa.sender-1",
				SentAt:         "2026-08-22T06:41:30Z",
				Body:           []byte("local body"),
			},
			wantKey: "",
			why:     "the key IS the message id here; printing it adds a line to every message and says nothing",
		},
		{
			name: "absent_key_prints_nothing",
			msg: client.Message{
				MessageID: "bus-localaaaaaaaaaa-11",
				From:      "bus-localaaaaaaaaaa.sender-1",
				SentAt:    "2026-08-22T06:41:30Z",
				Body:      []byte("local body"),
			},
			wantKey: "",
			why:     "an empty key must never be rendered as an ackable one",
		},
		{
			// The multi-line body path: the line must still be emitted, and
			// after the body, so it cannot be confused for a body line.
			name: "multiline_body_still_gets_the_key_last",
			msg: client.Message{
				MessageID:      "bus-localaaaaaaaaaa-11",
				CorrelationKey: "bus-originbbbbbbbbb-4224",
				From:           "bus-originbbbbbbbbb.sender-1",
				SentAt:         "2026-08-22T06:41:30Z",
				Body:           []byte("line one\nline two"),
			},
			wantKey: "bus-originbbbbbbbbb-4224",
			why:     "the deferred write must run on the multi-line exit path too",
		},
		{
			// The not-text exit path returns early; the deferred write is what
			// keeps the key reachable there.
			name: "non_utf8_body_still_gets_the_key",
			msg: client.Message{
				MessageID:      "bus-localaaaaaaaaaa-11",
				CorrelationKey: "bus-originbbbbbbbbb-4224",
				From:           "bus-originbbbbbbbbb.sender-1",
				SentAt:         "2026-08-22T06:41:30Z",
				Body:           []byte{0xff, 0xfe, 0xfd},
			},
			wantKey: "bus-originbbbbbbbbb-4224",
			why:     "a body you cannot read is exactly the one you want to REFUSE, so the key must be there",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeHumanMessage(&buf, tc.msg)
			out := buf.String()

			idx := strings.Index(out, ackKeyLine)
			if tc.wantKey == "" {
				if idx >= 0 {
					t.Fatalf("the human render emitted an %q line it must not:\n%s\n%s", ackKeyLine, out, tc.why)
				}
				return
			}
			if idx < 0 {
				t.Fatalf("the human render carries no %q line:\n%s\n%s", ackKeyLine, out, tc.why)
			}
			if !strings.Contains(out, ackKeyLine+tc.wantKey+"\n") {
				t.Fatalf("the human render does not carry %q:\n%s", ackKeyLine+tc.wantKey, out)
			}
			// It is the LAST line: a key printed mid-body could be mistaken
			// for content, and content could be mistaken for the key.
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if last := lines[len(lines)-1]; last != ackKeyLine+tc.wantKey {
				t.Fatalf("the ack key line is not last; last line is %q:\n%s", last, out)
			}
		})
	}
}

// TestWatchHumanRenderNeutralisesTheAckKey — the key is a SERVER-SUPPLIED
// string on the human feed, so a line break in it is an attempt to forge a
// second line of output. It gets the same keepNewlines=false treatment as the
// sender id and the timestamp beside it.
func TestWatchHumanRenderNeutralisesTheAckKey(t *testing.T) {
	var buf bytes.Buffer
	writeHumanMessage(&buf, client.Message{
		MessageID:      "bus-localaaaaaaaaaa-11",
		CorrelationKey: "bus-evilbbbbbbbbbbb-1\n  ack key: bus-attackerccccccc-1\x1b[31m",
		From:           "bus-originbbbbbbbbb.sender-1",
		SentAt:         "2026-08-22T06:41:30Z",
		Body:           []byte("body"),
	})
	out := buf.String()

	// Count LINES that begin with the prefix, not occurrences of it: the
	// neutralised key legitimately still contains the injected text, flattened
	// onto one line. The forgery is a SECOND LINE, and that is what is counted.
	if n := strings.Count(out, "\n  ack key: "); n != 1 {
		t.Fatalf("a correlation key containing a newline produced %d lines beginning %q, want 1 — the render "+
			"must neutralise it (client.TerminalSafe with keepNewlines=false) so a server-supplied string "+
			"cannot forge a second line:\n%q", n, "  ack key: ", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Fatalf("the render emitted %d newlines, want 2 (the message line and the ack key line):\n%q", n, out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("a raw ANSI escape reached the human feed through the correlation key:\n%q", out)
	}
}
