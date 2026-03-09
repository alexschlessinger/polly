package main

import (
	"strings"
	"testing"
)

func TestSummarizeToolArgs(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		argsJSON string
		want     string
	}{
		{"bash command", "bash", `{"command":"git status"}`, "git status"},
		{"read file", "read", `{"file_path":"src/main.go"}`, "src/main.go"},
		{"read with range", "read", `{"file_path":"src/main.go","offset":10,"limit":20}`, "src/main.go (lines 10-30)"},
		{"read offset only", "read", `{"file_path":"src/main.go","offset":10}`, "src/main.go (from line 10)"},
		{"write file", "write", `{"file_path":"out.txt","content":"hello"}`, "out.txt"},
		{"edit file", "edit", `{"file_path":"src/main.go","old_string":"a","new_string":"b"}`, "src/main.go"},
		{"glob pattern", "glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"grep pattern", "grep", `{"pattern":"func main"}`, "func main"},
		{"unknown tool", "custom_tool", `{"foo":"bar"}`, ""},
		{"empty args", "bash", `{}`, ""},
		{"invalid json", "bash", `not json`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeToolArgs(tt.toolName, tt.argsJSON)
			if got != tt.want {
				t.Errorf("summarizeToolArgs(%q, %q) = %q, want %q", tt.toolName, tt.argsJSON, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	long := "a very long string that exceeds the limit"
	got := truncate(long, 20)
	if len(got) > 20 {
		t.Errorf("truncate long len = %d, want <= 20", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate long should end with ellipsis, got %q", got)
	}

	multiline := "first line\nsecond line"
	if got := truncate(multiline, 100); got != "first line" {
		t.Errorf("truncate multiline = %q, want %q", got, "first line")
	}
}
