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
//     confident about. The registry walk here is that analyse with the winning
//     score kept: Analyse re-scoring the winner would read the body twice for
//     the same answer, so the score the walk already computed is what the
//     floor checks. A weak match falls through to "" rather than recolouring
//     prose.
//
// "" means "render as plain code", which is exactly what HighlightCodeLines
// does with an empty language, so anything chroma is not confident about stays
// byte-identical to the old plain code path.
func toolOutputLanguage(body string) string {
	if looksLikeUnifiedDiff(body) {
		return "diff"
	}
	var best chroma.Lexer
	var bestScore float32
	for _, lexer := range lexers.GlobalLexerRegistry.Lexers {
		analyser, ok := lexer.(chroma.Analyser)
		if !ok {
			continue
		}
		if score := analyser.AnalyseText(body); score > bestScore {
			best, bestScore = lexer, score
		}
	}
	if best == nil || best == lexers.Fallback || bestScore < contentAnalysisFloor {
		return ""
	}
	return best.Config().Name
}

// looksLikeUnifiedDiff reports whether body carries a unified-diff hunk header
// or a git diff header. Both are specific to diffs: a lone "---" line is not
// enough, since prose, markdown and yaml separators use it too, and every other
// unified-diff producer (diff -u, svn diff, ...) still emits "@@ " hunks.
func looksLikeUnifiedDiff(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "diff --git ") {
			return true
		}
	}
	return false
}
