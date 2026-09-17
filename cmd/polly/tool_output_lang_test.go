package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/messages"
)

const toolOutputDiffBody = `diff --git a/cmd/polly/repl_view.go b/cmd/polly/repl_view.go
index 1234567..89abcde 100644
--- a/cmd/polly/repl_view.go
+++ b/cmd/polly/repl_view.go
@@ -219,7 +219,7 @@ func appendInspectedToolOutput(ctx context.Context, m *replModel, t *inspectedTool) (string, error) {
 		// keeps visible spacing.
-		raw := markdown.HighlightCodeLines(text, "")
+		raw := markdown.HighlightCodeLines(text, toolOutputLanguage(text))
`

func TestToolOutputLanguage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"git diff", toolOutputDiffBody, "diff"},
		{"diff -u", "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n", "diff"},
		{"bare hunk", "@@ -1,2 +1,3 @@\n context\n", "diff"},
		{"prose", "Done. I updated the file and ran the tests.\n", ""},
		{"pretty json", "{\n  \"a\": 1\n}\n", ""},
		{"go test output", "=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nok  \tgithub.com/x/y\t0.12s\n", ""},
		{"markdown rule", "Title\n---\n\nbody\n", ""},
		{"weak analyser substring", "2 tools completed\n", ""},
		{"empty", "", ""},
		{"content chroma is confident about", "**free\ndcl-s x;\n", "RPGLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolOutputLanguage(tc.body)
			if got != tc.want {
				t.Fatalf("toolOutputLanguage(%q) = %q, want %q", tc.body, got, tc.want)
			}
			if got == lexers.Fallback.Config().Name {
				t.Fatalf("helper returned the fallback lexer name %q", got)
			}
			if got != "" && lexers.Get(got) == nil {
				t.Fatalf("helper returned %q, which chroma cannot resolve back to a lexer", got)
			}
		})
	}
}

// TestToolOutputUnlexableBodyIsPlain pins the no-regression half of criterion 4:
// a body chroma cannot confidently lex must be byte-identical to the literal
// code render (role "code", no lexer).
func TestToolOutputUnlexableBodyIsPlain(t *testing.T) {
	for _, body := range []string{
		"Done. I updated the file and ran the tests.\nsecond line",
		"=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nok  \tgithub.com/x/y\t0.12s",
		"{\n  \"a\": 1,\n  \"b\": \"two\"\n}",
		"col1\tcol2\tcol3",
		"",
	} {
		lang := toolOutputLanguage(body)
		if lang != "" {
			t.Fatalf("toolOutputLanguage(%q) = %q, want empty", body, lang)
		}
		if got, want := markdown.HighlightCodeLines(body, lang), markdown.HighlightCodeLines(body, ""); !slices.Equal(got, want) {
			t.Fatalf("unlexable body %q rendered %q, want plain %q", body, got, want)
		}
	}
}

// TestToolOutputDiffUsesInsertDeleteTones checks the diff token map end to end.
// The exact role names are owned by the markdown token-role task landing
// alongside this one: chroma's GenericInserted/GenericDeleted map to ok/err
// before it and to syn-add/syn-del after. Accept either so the assertion holds
// on both sides of that merge rather than pinning an in-flight role name.
func TestToolOutputDiffUsesInsertDeleteTones(t *testing.T) {
	highlighted := strings.Join(markdown.HighlightCodeLines(toolOutputDiffBody, toolOutputLanguage(toolOutputDiffBody)), "\n")
	plain := strings.Join(markdown.HighlightCodeLines(toolOutputDiffBody, ""), "\n")
	if highlighted == plain {
		t.Fatalf("diff body rendered identically to the plain code path:\n%s", highlighted)
	}
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"+raw := markdown.HighlightCodeLines(text, toolOutputLanguage(text))", []string{"fg:ok", "fg:syn-add"}},
		{"-raw := markdown.HighlightCodeLines(text, \"\")", []string{"fg:err", "fg:syn-del"}},
	} {
		got := strings.Join(markdown.HighlightCodeLines(tc.line, "diff"), "\n")
		if !containsAny(got, tc.want...) {
			t.Fatalf("%q rendered %q, want one of %q", tc.line, got, tc.want)
		}
	}
}

// TestAppendInspectedToolOutputHighlightsDiffBody exercises the real call site:
// the tool result body must reach the renderer with the sniffed language.
func TestAppendInspectedToolOutputHighlightsDiffBody(t *testing.T) {
	call := messages.ChatMessageToolCall{ID: "diff", Name: "bash", Arguments: `{"command":"git diff"}`}
	body := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-old\n+new"
	tool := inspectedTool{
		call:      call,
		result:    toolDataResult(t, call, body, nil),
		available: true,
		complete:  true,
	}
	tool.pres = newToolPresentation(toolPresentationInput{call: call, result: tool.result, complete: true})
	out := newReplModel()
	if _, err := appendInspectedToolOutput(context.Background(), out, &tool); err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(out.flattenTranscript(), "\n")
	if !containsAny(flat, "fg:ok", "fg:syn-add") || !containsAny(flat, "fg:err", "fg:syn-del") {
		t.Fatalf("diff body did not reach the renderer with diff tones:\n%s", flat)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
