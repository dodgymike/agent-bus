package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/dodgymike/agent-bus/client"
)

// output is the rendering layer. Every subcommand writes through it and none
// of them calls fmt.Fprintf on stdout directly, because "--json on EVERY
// command" (invariant 7) is only true if there is one place that can be asked
// to honour it.
//
// Three modes, and the third is why this type exists now rather than when the
// long-poll subcommand needs it:
//
//   - HUMAN: a readable block on stdout.
//   - JSON: exactly ONE object on stdout, so `busctl ... --json | jq` works
//     without the caller knowing how many lines to expect.
//   - NDJSON (Stream): one compact object PER LINE, flushed as it is produced,
//     so a long poll can be consumed incrementally rather than buffered to
//     completion. The records are the bare result objects with NO envelope —
//     a consumer reading a stream reads messages, not wrappers.
//
// Diagnostics NEVER go to stdout. An agent that pipes stdout to a JSON parser
// must not have a warning appear in the middle of its document.
type output struct {
	json   bool
	stdout io.Writer
	stderr io.Writer
}

// flusher is implemented by writers that buffer, so Stream can push each
// record out as it is produced instead of at exit.
type flusher interface{ Flush() error }

// Emit renders one result.
//
// In JSON mode the object is the result's own fields plus `"ok": true`, so a
// consumer can tell success from failure (`"ok": false`) with a single field
// lookup regardless of which subcommand produced it. Keys are emitted sorted,
// so the output is byte-stable across runs and across Go versions.
func (o *output) Emit(v interface{}, human func(w io.Writer)) error {
	if !o.json {
		if human != nil {
			human(o.stdout)
		}
		return nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("rendering the result as JSON: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		// Not a JSON object (an array, say). Wrap rather than lose it.
		wrapped := map[string]json.RawMessage{
			"ok":     json.RawMessage("true"),
			"result": json.RawMessage(body),
		}
		return o.writeObject(wrapped)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["ok"] = json.RawMessage("true")
	return o.writeObject(fields)
}

// writeObject writes a JSON object with sorted keys and a trailing newline.
func (o *output) writeObject(fields map[string]json.RawMessage) error {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := make([]byte, 0, 256)
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return err
		}
		buf = append(buf, key...)
		buf = append(buf, ':')
		buf = append(buf, fields[k]...)
	}
	buf = append(buf, '}', '\n')
	_, err := o.stdout.Write(buf)
	return err
}

// Stream writes one NDJSON record: a single compact JSON object followed by a
// newline, flushed immediately.
//
// This is the convention for every streaming subcommand (the long poll, when
// it lands): no envelope, no array brackets, no pretty-printing — one object
// per line so a consumer can act on each as it arrives.
//
// It ignores --json deliberately: a stream is machine output by definition,
// and there is no readable rendering of "the next message, eventually".
func (o *output) Stream(v interface{}) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("rendering a stream record as JSON: %w", err)
	}
	body = append(body, '\n')
	if _, err := o.stdout.Write(body); err != nil {
		return err
	}
	if f, ok := o.stdout.(flusher); ok {
		return f.Flush()
	}
	return nil
}

// Fail renders err and returns the process exit code for it.
//
// In JSON mode the failure object goes to STDOUT, so an agent parsing stdout
// gets a document either way and never has to correlate two streams. In human
// mode it goes to stderr, on two lines: what happened, and what to do about it
// (invariant 7 — "errors that name the remedy rather than the stack").
func (o *output) Fail(err error) int {
	if err == nil {
		return client.ExitOK
	}
	payload := client.NewErrorPayload(err)
	if o.json {
		body, merr := json.Marshal(payload)
		if merr == nil {
			body = append(body, '\n')
			_, _ = o.stdout.Write(body)
			return payload.ExitCode
		}
		// Falling through to the human rendering is better than emitting
		// nothing at all; the exit code is still correct.
	}
	fmt.Fprintf(o.stderr, "busctl: %s\n", payload.Error)
	if payload.Remedy != "" {
		fmt.Fprintf(o.stderr, "  try: %s\n", payload.Remedy)
	}
	return payload.ExitCode
}
