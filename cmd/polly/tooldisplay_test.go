package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

func TestSummarizeToolArgsGenericDeterministic(t *testing.T) {
	args := `{"z":2,"nested":{"raw":"do not show"},"a":"hello world","items":[1,2],"enabled":true,"nothing":null}`
	want := `a="hello world", enabled=true, items=[2 items], nested={1 fields}, nothing=null, z=2`

	for i := 0; i < 20; i++ {
		if got := summarizeToolArgs("mcp__custom", args); got != want {
			t.Fatalf("generic summary = %q, want %q", got, want)
		}
	}
	if got := toolLabel(messagesToolCall("mcp__custom", args)); got != "mcp__custom "+want {
		t.Fatalf("tool label = %q, want %q", got, "mcp__custom "+want)
	}
}

func TestSummarizeToolArgsGenericRedactsSensitiveKeys(t *testing.T) {
	for _, key := range []string{"apiKey", "access_token", "client-secret", "password", "authorization", "credential_file", "cookieJar"} {
		args := `{"` + key + `":"do not show"}`
		got := summarizeToolArgs("custom", args)
		if strings.Contains(got, "do not show") || !strings.Contains(got, key+"=<redacted>") {
			t.Fatalf("sensitive key %q was not redacted: %q", key, got)
		}
	}

	got := summarizeToolArgs("custom", `{"author":"Ada","monkey":"capuchin"}`)
	for _, visible := range []string{`author="Ada"`, `monkey="capuchin"`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("ordinary field %q was hidden: %q", visible, got)
		}
	}
}

func TestSummarizeToolArgsGenericCapsWidthAndHidesNestedPayloads(t *testing.T) {
	args := `{"payload":{"password":"nested secret","body":"nested body"},"query":"` + strings.Repeat("界", 100) + `"}`
	got := summarizeToolArgs("custom", args)

	if rw.StringWidth(got) > genericToolSummaryWidth {
		t.Fatalf("summary width = %d, want <= %d: %q", rw.StringWidth(got), genericToolSummaryWidth, got)
	}
	for _, hidden := range []string{"nested secret", "nested body"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("summary dumped nested payload %q: %q", hidden, got)
		}
	}
	if !strings.Contains(got, "payload={2 fields}") {
		t.Fatalf("nested payload shape missing: %q", got)
	}
}

func TestSummarizeToolArgsPreservesSpecializedSummaries(t *testing.T) {
	if got := summarizeToolArgs("bash", `{"command":"git status","token":"must not replace specialized output"}`); got != "git status" {
		t.Fatalf("bash summary = %q, want git status", got)
	}
	if got := summarizeToolArgs("read", `{"file_path":"README.md","offset":10,"limit":5}`); got != "README.md (lines 10-15)" {
		t.Fatalf("read summary = %q, want specialized read summary", got)
	}
}

// messagesToolCall keeps the toolLabel assertion compact without obscuring the
// generic summarizer tests with unrelated message fields.
func messagesToolCall(name, arguments string) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{Name: name, Arguments: arguments}
}

func TestResultLineMeta(t *testing.T) {
	cases := []struct {
		result string
		want   string
	}{
		{"", ""},
		{"   \n  \n", ""},
		{"single", "1 line"},
		{"single trailing newline\n", "1 line"},
		{"a\nb\nc", "3 lines"},
		{"a\nb\nc\n", "3 lines"},
	}
	for _, tc := range cases {
		if got := resultLineMeta(tc.result); got != tc.want {
			t.Errorf("resultLineMeta(%q) = %q, want %q", tc.result, got, tc.want)
		}
	}
}

func TestToolFailureMetaExtractsExitCode(t *testing.T) {
	cmd := exec.Command("bash", "-c", "exit 3")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected the probe command to fail")
	}
	// Match bash.go's wrapping so the chain is representative.
	wrapped := fmt.Errorf("command failed: %w (output: boom)", err)
	if got := toolFailureMeta(wrapped); got != "exit 3" {
		t.Fatalf("toolFailureMeta = %q, want %q", got, "exit 3")
	}
	// Non-exec errors (MCP failures, timeouts) add nothing.
	if got := toolFailureMeta(fmt.Errorf("connection reset")); got != "" {
		t.Fatalf("non-exec error meta = %q, want empty", got)
	}
}

func TestExpandToolCallShowsBashCommandVerbatim(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"command": "go test ./... &&\ngofmt -l .\n"})
	got := expandToolCall(messages.ChatMessageToolCall{Name: "bash", Arguments: string(args)})
	if got != "go test ./... &&\ngofmt -l ." {
		t.Fatalf("bash expansion = %q", got)
	}
}

func TestExpandToolCallRedactsAndCaps(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"url": "https://x", "api_key": "hunter2"})
	got := expandToolCall(messages.ChatMessageToolCall{Name: "fetch", Arguments: string(args)})
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "<redacted>") {
		t.Fatalf("expansion leaked a sensitive value: %q", got)
	}
	if !strings.Contains(got, "https://x") {
		t.Fatalf("expansion lost ordinary args: %q", got)
	}

	long := strings.Repeat("echo line\n", approvalViewMaxLines+10)
	cmd, _ := json.Marshal(map[string]any{"command": long})
	capped := expandToolCall(messages.ChatMessageToolCall{Name: "bash", Arguments: string(cmd)})
	if lines := strings.Count(capped, "\n") + 1; lines != approvalViewMaxLines+1 {
		t.Fatalf("capped expansion has %d lines, want %d + elision row", lines, approvalViewMaxLines)
	}
	if !strings.Contains(capped, "more lines)") {
		t.Fatalf("capped expansion should note the elision: %q", capped)
	}
}

func TestExpandToolCallMalformedArguments(t *testing.T) {
	got := expandToolCall(messages.ChatMessageToolCall{Name: "bash", Arguments: "{not json"})
	if got != "{not json" {
		t.Fatalf("malformed args should render as-is, got %q", got)
	}
}
