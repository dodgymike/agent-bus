package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodgymike/agent-bus/client"
)

// TestOutputJSONSuccessIsOneLineWithOK checks Emit in JSON mode writes exactly
// one line of JSON to stdout, that it parses as a whole, and that it carries
// "ok": true.
func TestOutputJSONSuccessIsOneLineWithOK(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &output{json: true, stdout: &stdout, stderr: &stderr}

	type result struct {
		Foo string `json:"foo"`
	}
	if err := o.Emit(result{Foo: "bar"}, nil); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	trimmed := strings.TrimRight(stdout.String(), "\n")
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("stdout has more than one line: %q", stdout.String())
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("stdout is not parseable in its entirety as one JSON value: %v (%q)", err, stdout.String())
	}
	if ok, _ := parsed["ok"].(bool); !ok {
		t.Fatalf("parsed[\"ok\"] = %v, want true", parsed["ok"])
	}
	if parsed["foo"] != "bar" {
		t.Fatalf("parsed[\"foo\"] = %v, want %q", parsed["foo"], "bar")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr not empty on success: %q", stderr.String())
	}
}

// TestOutputJSONFailureHasOKFalseAndExitCode checks Fail in JSON mode writes
// the failure object to STDOUT (so an agent parsing stdout always gets a
// document) with "ok": false and an "exit_code" field.
func TestOutputJSONFailureHasOKFalseAndExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &output{json: true, stdout: &stdout, stderr: &stderr}

	err := &client.Error{Kind: client.KindUsage, Op: "x", Message: "bad request", Remedy: "fix it"}
	code := o.Fail(err)
	if code != client.ExitUsage {
		t.Fatalf("Fail() returned %d, want %d", code, client.ExitUsage)
	}

	var parsed map[string]interface{}
	if uerr := json.Unmarshal(stdout.Bytes(), &parsed); uerr != nil {
		t.Fatalf("stdout is not parseable JSON: %v (%q)", uerr, stdout.String())
	}
	if ok, _ := parsed["ok"].(bool); ok {
		t.Fatalf("parsed[\"ok\"] = %v, want false", parsed["ok"])
	}
	gotExit, ok := parsed["exit_code"].(float64)
	if !ok {
		t.Fatalf("parsed has no numeric exit_code field: %v", parsed)
	}
	if int(gotExit) != client.ExitUsage {
		t.Fatalf("exit_code = %v, want %d", gotExit, client.ExitUsage)
	}
}

// TestOutputStreamProducesOneCompleteJSONObjectPerLine checks the documented
// NDJSON convention Stream implements: N records in produce N lines out, each
// line parses as a whole, standalone JSON object with NO envelope (no "ok"
// field, unlike Emit) — a consumer reading a stream reads bare messages, not
// wrappers.
func TestOutputStreamProducesOneCompleteJSONObjectPerLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &output{json: true, stdout: &stdout, stderr: &stderr}

	type record struct {
		Seq int    `json:"seq"`
		Msg string `json:"msg"`
	}
	const n = 5
	for i := 0; i < n; i++ {
		if err := o.Stream(record{Seq: i, Msg: "hello"}); err != nil {
			t.Fatalf("Stream(%d): %v", i, err)
		}
	}

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("Stream wrote %d lines for %d records, want %d: %q", len(lines), n, n, stdout.String())
	}
	for i, line := range lines {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("line %d is not a complete JSON object: %v (%q)", i, err, line)
		}
		if _, ok := parsed["ok"]; ok {
			t.Fatalf("line %d carries an \"ok\" field %v — Stream records must have NO envelope", i, parsed["ok"])
		}
		if seq, ok := parsed["seq"].(float64); !ok || int(seq) != i {
			t.Fatalf("line %d seq = %v, want %d", i, parsed["seq"], i)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr not empty: %q", stderr.String())
	}
}

// TestOutputHumanModeErrorGoesToStderr checks a human-mode failure writes
// nothing to stdout and writes the message (plus remedy) to stderr.
func TestOutputHumanModeErrorGoesToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	o := &output{json: false, stdout: &stdout, stderr: &stderr}

	err := &client.Error{Kind: client.KindUsage, Op: "x", Message: "bad request", Remedy: "fix it"}
	o.Fail(err)

	if stdout.Len() != 0 {
		t.Fatalf("stdout not empty in human mode: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bad request") {
		t.Fatalf("stderr = %q, want it to contain the error message", stderr.String())
	}
	if !strings.Contains(stderr.String(), "fix it") {
		t.Fatalf("stderr = %q, want it to contain the remedy", stderr.String())
	}
}
