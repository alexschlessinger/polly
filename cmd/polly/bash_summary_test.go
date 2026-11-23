package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	rw "github.com/mattn/go-runewidth"
)

func TestBashSummaryPreservesShellStructure(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"git status --short", "git status --short"},
		{"cd /workspace/polly && GOCACHE=/tmp/cache go test ./cmd/polly", "… go test ./cmd/polly"},
		{"cd /workspace/polly && for r in a b; do echo \"$r\"; done", "for r in a b; do echo \"$r\"; done"},
		{"cd /a && cd /b && go test ./...", "cd /b && go test ./..."},
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
		// Shell options ahead of the work are setup, like a leading cd.
		{"set -e -o pipefail; gofmt -l .; go test ./...", "gofmt -l . ; go test ./..."},
		{"set -euo pipefail\ngo build ./...", "go build ./..."},
		{"set -e && go vet ./...", "go vet ./..."},
		{"set +e; false; echo $?", "false ; echo $?"},
		{"set -e", "set -e"},
		{"echo hi; set -e; false", "echo hi ; set -e ; false"},
		{"set -- a b; echo \"$1\"", "set -- a b ; echo \"$1\""},
		{"set -x >log; echo hi", "set -x >log ; echo hi"},
		{"set -o; echo hi", "set -o ; echo hi"},
		{"set; echo hi", "set ; echo hi"},
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

// A script too long for its row shows how it begins: its first commands, each
// with room to be read, then an ellipsis after the operator that led to the
// rest. It is never a stub of every command.
func TestBashSummaryShowsALongScriptsFirstCommands(t *testing.T) {
	const script = "set -e -o pipefail; git diff --check; git ls-files -zco --exclude-standard '*.go' | while IFS= read -r -d '' p; do if test -f \"$p\"; then printf '%s\\0' \"$p\"; fi; done | xargs -0 gofmt -l; git diff -U3 -- cmd/polly/repl_transcript.go | grep -C 7 -E 'hold|until'; git diff --stat | grep -A47 handoff || test \"$?\" = 1"
	summary := newBashSummary(script)
	for width, want := range map[int]string{
		131: `git diff --check ; git ls-files -zco --exclude-standard '*.go' | while IFS= read -r -d '' p; do if test -f… | xargs -0 gofmt -l ; …`,
		60:  `git diff --check ; git ls-files -zco --exclude-standard… | …`,
	} {
		if got := summary.fit(width); got != want {
			t.Errorf("width %d:\n got %q\nwant %q", width, got, want)
		}
	}
	operators := regexp.MustCompile(` (;|\||\|\||&&) `)
	for width := 1; width <= 400; width++ {
		got := summary.fit(width)
		if rw.StringWidth(got) > width {
			t.Fatalf("width %d overflows: %q", width, got)
		}
		// A command cut short still has a third of the row, up to
		// bashStageRoom columns; only the first may have less, when the row
		// is too narrow to promise even that.
		room := min(bashStageRoom, max(bashStageStub, width/3))
		for i, stage := range operators.Split(strings.TrimSuffix(got, " …"), -1) {
			if i > 0 && strings.HasSuffix(stage, "…") && rw.StringWidth(stage) < room {
				t.Fatalf("width %d: %q is a stub in %q", width, stage, got)
			}
		}
	}
	if got := summary.fit(400); strings.Contains(got, "…") || !strings.HasPrefix(got, "git diff --check ; git ls-files") || !strings.HasSuffix(got, `|| test "$?" = 1`) {
		t.Fatalf("a row wide enough shows the whole script: %q", got)
	}
}

func BenchmarkBashSummaryFitLongScript(b *testing.B) {
	for _, count := range []int{1000, 4000, 16000, 28000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			summary := newBashSummary(strings.Repeat(":;", count) + ":")
			for b.Loop() {
				_ = summary.fit(100)
			}
		})
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
