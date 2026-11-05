package markdown

import (
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// RenderMutedRows uses the response renderer and wrapping rules for a text-only
// preview. Markdown structure and emphasis survive; the foreground stays muted.
// Image references remain links, without resolving files or creating image slots.
func RenderMutedRows(src string, width int, cache *CodeCache) []string {
	if width < 1 {
		return nil
	}
	state := &renderState{width: width, textOnly: true, codeCache: cache}
	rendered := renderDocument(src, state)
	if cache != nil {
		cache.blocks = cache.blocks[:state.codeIndex]
	}
	if rendered == "" {
		return nil
	}
	rows := style.VisualRows(rendered, ui.StyleClear, width)
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = cellMarkup(row, "muted")
	}
	return lines
}

// TrimPreview bounds the retained source, preferring a whole-line boundary.
// A fenced block spanning the cut keeps its opener as a small rendering prefix:
// its eventual closing fence must not become an opener for subsequent prose.
// This parses source only; it never resolves theme colors or local images.
func TrimPreview(src string, keepRunes int) (tail, prefix string) {
	runes := []rune(src)
	if len(runes) <= keepRunes {
		return src, ""
	}
	if keepRunes <= 0 {
		return "", ""
	}
	start := len(src) - len(string(runes[len(runes)-keepRunes:]))
	if src[start-1] != '\n' {
		if next := strings.IndexByte(src[start:], '\n'); next >= 0 && start+next+1 < len(src) {
			start += next + 1
		}
	}
	source := []byte(src)
	doc := mdParser.Parser().Parse(text.NewReader(source))
	var bounds map[ast.Node]markdownSourceSpan
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		fence, ok := n.(*ast.FencedCodeBlock)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		if fence.Lines().Len() == 0 {
			// Empty fences have no body span. Drop a partial empty block rather
			// than retaining an orphan closer before the next meaningful block.
			if bounds == nil {
				bounds = markdownSourceBounds(doc, source)
			}
			span := bounds[n]
			if start >= span.start && start < span.end {
				if span.end == len(src) {
					start = len(src)
				} else {
					start = max(start, strings.LastIndexByte(src[:span.end], '\n')+1)
				}
			}
			return ast.WalkContinue, nil
		}
		first, last := fence.Lines().At(0), fence.Lines().At(fence.Lines().Len()-1)
		end := strings.LastIndexByte(src[:first.Start], '\n')
		if start < end+1 || start > last.Stop {
			return ast.WalkContinue, nil
		}
		if end >= 0 {
			begin := strings.LastIndexByte(src[:end], '\n') + 1
			opener := []rune(src[begin:end])
			prefix = string(opener[:min(len(opener), keepRunes)]) + "\n"
		}
		return ast.WalkStop, nil
	})
	return src[start:], prefix
}
