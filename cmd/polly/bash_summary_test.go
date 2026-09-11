package main

import (
	"strings"
	"testing"

	rw "github.com/mattn/go-runewidth"
)

func TestBashSummaryPreservesShellStructure(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"git status --short", "git status --short"},
		{"cd /workspace/polly && GOCACHE=/tmp/cache go test ./cmd/polly", "… && … go test ./cmd/polly"},
		{"env FOO=bar BAR=baz go test ./...", "… go test ./..."},
		{"env -i FOO=bar go test ./...", "env -i FOO=bar go test ./..."},
		{"cd /workspace/polly || echo failed", "cd /workspace/polly || echo failed"},
		{"cd /workspace/polly >log && pwd", "cd /workspace/polly >log && pwd"},
		{"! cd /workspace/polly && pwd", "! cd /workspace/polly && pwd"},
		{`printf '%s; %s | %s' "a && b" 'c'; git status`, `printf '%s; %s | %s' "a && b" 'c' ; git status`},
		{"go test ./... | tail -40", "go test ./... | tail -40"},
		{"go test ./...\ngit status", "go test ./... ; git status"},
		{"echo one &", "echo one &"},
		{"echo one & echo two", "echo one & echo two"},
		{"FOO=bar; echo two", "FOO=bar ; echo two"},
		{"echo one >log; echo two", "echo one >log ; echo two"},
		{"for f in *.go; do echo \"$f\"; done; echo end", "for f in *.go; do echo \"$f\"; done ; echo end"},
		{"for f in *.go; do echo \"$f\"; done", "for f in *.go; do echo \"$f\"; done"},
		{"python <<'PY'\nprint('hello')\nPY", "python <<'PY' …"},
		{"echo 'unterminated\nnext line", "echo 'unterminated …"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			if got := newBashSummary(tc.command).fit(200); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBashSummaryBudgetsStagesAndPaths(t *testing.T) {
	command := "go test ./cmd/polly -run TestExpandedToolDisclosure -count=1 | tail -40"
	summary := newBashSummary(command)
	if got := summary.fit(40); !strings.HasPrefix(got, "go test ") || !strings.HasSuffix(got, " | tail -40") || !strings.Contains(got, "…") {
		t.Fatalf("pipeline lost a stage: %q", got)
	}
	path := "cat cmd/polly/internal/termimg/assets/logo.png"
	if got := newBashSummary(path).fit(32); got != "cat cmd/…/logo.png" {
		t.Fatalf("path summary = %q", got)
	}
	for _, command := range []string{command, path, "echo '界界界界界界界界界界界界界界界界界界界界'", "echo [x] && git status || exit 1", "echo \t hello", strings.Repeat("x", 70<<10)} {
		summary := newBashSummary(command)
		for width := 1; width <= 150; width++ {
			got := summary.fit(width)
			if rw.StringWidth(got) > width || strings.ContainsAny(got, "\n\t\r") {
				t.Fatalf("width %d summary overflows: %q", width, got)
			}
		}
	}
	if got := summary.fit(200); got != command {
		t.Fatalf("widening did not restore command: %q", got)
	}
}

func TestBashSummaryDoesNotShortenQuotedTextOrExpressionsAsPaths(t *testing.T) {
	for _, text := range []string{
		`echo 'a /some/very/long/path/to/file b'`,
		`curl https://example.com/a/very/long/path/to/file`,
		`echo /some/very/long/path/$variable/file`,
		`echo /some/very/long/path/*/file`,
	} {
		if got := shortenBashPaths(text); got != text {
			t.Fatalf("changed quoted text or expression: %q -> %q", text, got)
		}
	}
}
