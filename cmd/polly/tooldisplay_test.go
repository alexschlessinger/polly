package main

import (
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
