package main

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// contentAnalysisFloor is the minimum chroma analyser score accepted as a
// confident content match. lexers.Analyse returns the highest scorer with no
// floor at all, so it calls "2 tools completed" GDScript because one analyser
// regex matches the substring "tool" (score 0.2); tool output is arbitrary
// prose, so require a real signal before restyling it. Real detections score
// 0.7-0.9.
const contentAnalysisFloor = 0.5

// toolOutputLanguage sniffs a chroma language for a tool result body.
//
// Tool results carry no language tag, so the language has to come from the
// content. Two sources, in order:
//
//  1. The unified-diff shape. chroma ships no content analyser for its Diff
//     lexer (it is selected by file extension), so Analyse never picks it, yet
//     bash tool output is frequently a diff. Detect that shape so a diff body
//     gets the same token->role map as a fenced diff.
//  2. lexers.Analyse, chroma's content analyser, for content it is genuinely
//     confident about. Its result is re-scored so a weak match falls through to
//     "" rather than recolouring prose.
//
// "" means "render as plain code", which is exactly what HighlightCodeLines
// does with an empty language, so anything chroma is not confident about stays
// byte-identical to the old plain code path.
func toolOutputLanguage(body string) string {
	if looksLikeUnifiedDiff(body) {
		return "diff"
	}
	lexer := lexers.Analyse(body)
	if lexer == nil || lexer == lexers.Fallback {
		return ""
	}
	analyser, ok := lexer.(chroma.Analyser)
	if !ok || analyser.AnalyseText(body) < contentAnalysisFloor {
		return ""
	}
	return lexer.Config().Name
}

// looksLikeUnifiedDiff reports whether body carries a unified-diff hunk header
// or a git diff header. Both are specific to diffs: a lone "---" line is not
// enough, since prose, markdown and yaml separators use it too, and every other
// unified-diff producer (diff -u, svn diff, ...) still emits "@@ " hunks.
func looksLikeUnifiedDiff(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "@@ "):
			return true
		case strings.HasPrefix(line, "diff --git "):
			return true
		}
	}
	return false
}
