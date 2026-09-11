// Package markdown renders assistant Markdown into the transcript's styled
// text: gotui markup with the semantic palette, highlighted code, aligned
// tables, and sidecar image slots for explicit local image references. The
// streaming Document renders a growing source by byte range so already
// shown text never moves.
package markdown

import (
	"fmt"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// mdParser is the shared goldmark instance used to render assistant markdown.
// Only the parser is used; rendering to gotui markup is done by the walker
// below so every color stays inside the semantic ANSI palette. Capability
// changes here must be reflected in richTerminalDisplayContract
// (display_contract.go), which tells the model what this renderer supports.
var mdParser = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Table))

// RenderWithLocalImages keeps the ordinary Markdown surface while
// replacing explicit, existing local image references with private transcript
// slots. The slots are consumed only by the managed TUI; callers that just
// need text use renderMarkdownDocument with a nil state. streaming marks the
// source as an in-flight prefix; the returned deferred flag reports that a
// table rendered unaligned and the caller must re-render at settle.
func RenderWithLocalImages(src, baseDir string, streaming bool) (string, []style.Image, bool) {
	return RenderWithCache(src, baseDir, streaming, nil)
}

func RenderWithCache(src, baseDir string, streaming bool, cache *CodeCache) (string, []style.Image, bool) {
	return RenderWithWidth(src, baseDir, streaming, cache, 0)
}

// RenderWithWidth enables responsive tables when width is positive.
// Widthless callers retain the append-only terminal rendering contract.
func RenderWithWidth(src, baseDir string, streaming bool, cache *CodeCache, width int) (string, []style.Image, bool) {
	state := &renderState{baseDir: baseDir, streaming: streaming, codeCache: cache, width: width}
	rendered := renderDocument(src, state)
	if cache != nil {
		cache.blocks = cache.blocks[:state.codeIndex]
	}
	return rendered, state.images, state.deferredTable
}

// Each code block retains only its latest rendering. Appending prose or a
// second block reuses completed highlighting without retaining every prefix
// of a growing block. The AST is still reparsed so late link definitions and
// table delimiters keep their normal Markdown semantics.
type CodeCache struct{ blocks []markdownCodeBlock }

// Len reports how many code blocks the cache holds.
func (c *CodeCache) Len() int { return len(c.blocks) }

// Block returns cached block i's source and highlighted lines; the lines
// slice is the cached one, so callers can check it was reused.
func (c *CodeCache) Block(i int) (code string, lines []string) {
	return c.blocks[i].code, c.blocks[i].lines
}

type markdownCodeBlock struct {
	code, lang string
	lines      []string
}

func (s *renderState) renderCode(code, lang string) []string {
	if s == nil || s.codeCache == nil {
		return renderCodeBlock(code, lang)
	}
	i := s.codeIndex
	s.codeIndex++
	cache := s.codeCache
	if i == len(cache.blocks) {
		cache.blocks = append(cache.blocks, markdownCodeBlock{})
	}
	b := &cache.blocks[i]
	if b.lines == nil || b.code != code || b.lang != lang {
		*b = markdownCodeBlock{code: code, lang: lang, lines: renderCodeBlock(code, lang)}
	}
	return b.lines
}

// RenderDocument renders src to styled text with no image slots, for callers
// that only need text.
func RenderDocument(src string) string { return renderDocument(src, nil) }

func renderDocument(src string, state *renderState) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	source := []byte(src)
	doc := mdParser.Parser().Parse(text.NewReader(source))
	lines := renderBlocks(doc, source, "", state)
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// markdownSourceText strips the private runes reserved for terminal-image
// slots whenever Markdown shares a transcript block with image sidecars.
// Image destinations are resolved from the original AST value before display
// text reaches this boundary, so paths containing those runes still work.
func markdownSourceText(s string, state *renderState) string {
	if state == nil {
		return s
	}
	return style.StripImageMarkers(s)
}

// renderBlocks renders a parent's block children, separating siblings with a
// prefix-bearing blank line so quoted blocks keep their gutter.
func renderBlocks(parent ast.Node, source []byte, prefix string, state *renderState) []string {
	var out []string
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		lines := renderBlock(child, source, prefix, prefix, state)
		if len(lines) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, strings.TrimRight(prefix, " "))
		}
		out = append(out, lines...)
	}
	return out
}

func renderBlock(n ast.Node, source []byte, firstPrefix, contPrefix string, state *renderState) []string {
	if state != nil && state.clip.excludes(n) {
		return nil
	}
	switch b := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return prefixLines(splitInline(renderInlineChildren(n, source, "", "", state)), firstPrefix, contPrefix)
	case *ast.Heading:
		fg, mod := markdownHeadingStyle(b.Level)
		return prefixLines([]string{renderInlineChildren(n, source, fg, mod, state)}, firstPrefix, contPrefix)
	case *ast.FencedCodeBlock:
		lang := markdownSourceText(string(b.Language(source)), state)
		lines := renderClippedCode(b.Lines(), source, lang, state)
		if lang != "" && state != nil && state.clip != nil && state.clip.start > state.clip.bounds[n].start && len(lines) > 0 {
			lines = lines[1:]
		}
		return prefixLines(lines, firstPrefix, contPrefix)
	case *ast.CodeBlock:
		return prefixLines(renderClippedCode(b.Lines(), source, "", state), firstPrefix, contPrefix)
	case *ast.Blockquote:
		gutter := style.Styled("▏ ", "muted", "")
		restore := insetTableWidth(state, 2+max(style.TextWidth(firstPrefix), style.TextWidth(contPrefix)))
		inner := renderBlocks(n, source, "", state)
		restore()
		lines := make([]string, len(inner))
		for i, l := range inner {
			lines[i] = gutter + l
		}
		return prefixLines(lines, firstPrefix, contPrefix)
	case *ast.List:
		return renderList(b, source, firstPrefix, contPrefix, state)
	case *east.Table:
		return renderTable(b, source, firstPrefix, contPrefix, state)
	case *ast.ThematicBreak:
		return prefixLines([]string{style.Styled(strings.Repeat("─", 12), "muted", "")}, firstPrefix, contPrefix)
	case *ast.HTMLBlock:
		html := markdownSourceText(clippedCodeBlockText(b.Lines(), source, state), state)
		return prefixLines(splitInline(style.Escape(html)), firstPrefix, contPrefix)
	default:
		raw := markdownSourceText(nodeText(n, source), state)
		if raw = strings.TrimRight(style.Escape(raw), "\n"); raw != "" {
			return prefixLines(strings.Split(raw, "\n"), firstPrefix, contPrefix)
		}
		return nil
	}
}

// markdownHeadingStyle ranks headings by weight alone: no marker glyphs, so
// nothing in assistant text can pose as the user gutter or a code fence.
func markdownHeadingStyle(level int) (fg, mod string) {
	switch level {
	case 1:
		return "accent", "bold"
	case 2:
		return "", "bold"
	case 3:
		return "muted", "bold"
	default:
		return "muted", ""
	}
}

func renderList(list *ast.List, source []byte, firstPrefix, contPrefix string, state *renderState) []string {
	var out []string
	num := list.Start
	if num == 0 {
		num = 1
	}
	first := true
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		marker := "• "
		if list.IsOrdered() {
			marker = fmt.Sprintf("%d. ", num)
			num++
		}
		if state != nil && state.clip.excludes(item) {
			continue
		}
		if state != nil && state.clip != nil && state.clip.start > state.clip.bounds[item].start {
			marker = strings.Repeat(" ", len(marker))
		}
		indent := strings.Repeat(" ", len(marker))
		lead, cont := firstPrefix, contPrefix
		if !first {
			lead = contPrefix
		}
		restore := insetTableWidth(state, len(marker)+max(style.TextWidth(lead), style.TextWidth(cont)))
		inner := renderBlocks(item, source, "", state)
		restore()
		for i, l := range inner {
			if i == 0 {
				out = append(out, lead+style.Styled(marker, "muted", "")+l)
			} else {
				out = append(out, cont+indent+l)
			}
		}
		first = false
	}
	return out
}

// renderTable lays a GFM table out at natural column widths: bold header,
// muted per-column underline, two-space gaps, and the same muted "│ " gutter
// code fences use — which the wrap layer classifies as hard-wrap content, so
// an overwide table wraps without reflowing its columns. While the table is
// still streaming its widths aren't final, so rows render unaligned and
// state.deferredTable asks the stream owner for a settle re-render.
func renderTable(table *east.Table, source []byte, firstPrefix, contPrefix string, state *renderState) []string {
	// A clip may start inside the table when a streamed frame overflows the
	// screen. Widths still come from every row, so the committed prefix and
	// the mutable remainder pad identically, and the header underline follows
	// the header row rather than whichever row happens to be emitted first.
	var rows [][]string
	widths := make([]int, len(table.Alignments))
	headerShown := false
	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		_, isHeader := row.(*east.TableHeader)
		mod := ""
		if isHeader {
			mod = "bold"
		}
		// Clipped-out rows are measured, not emitted: the clip-aware inline
		// renderer would slice their text away, so they render unclipped.
		excluded := state != nil && state.clip.excludes(row)
		cellState := state
		if excluded {
			cellState = nil
		}
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			if cellState != nil {
				cellState.tableCell = cellState.width > 0
			}
			cells = append(cells, renderInlineChildren(cell, source, "", mod, cellState))
			if cellState != nil {
				cellState.tableCell = false
			}
		}
		for len(widths) < len(cells) {
			widths = append(widths, 0)
		}
		for i, cell := range cells {
			widths[i] = max(widths[i], style.TextWidth(cell))
		}
		if excluded {
			continue
		}
		headerShown = headerShown || isHeader
		rows = append(rows, cells)
	}
	cols := len(widths)
	if len(rows) == 0 || cols == 0 {
		return nil
	}

	if state != nil && state.width > 0 {
		return responsiveTable(rows, widths, table, firstPrefix, contPrefix, state.width)
	}
	gutter := style.Styled("│ ", "muted", "")
	if state != nil && state.streaming && atStreamEdge(table) {
		state.deferredTable = true
		sep := style.Styled(" │ ", "muted", "")
		lines := make([]string, len(rows))
		for i, cells := range rows {
			lines[i] = gutter + strings.Join(cells, sep)
		}
		return prefixLines(lines, firstPrefix, contPrefix)
	}

	var lines []string
	for _, cells := range rows {
		parts := make([]string, cols)
		for i := range parts {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			parts[i] = padTableCell(cell, widths[i], tableAlignment(table, i))
		}
		lines = append(lines, gutter+strings.TrimRight(strings.Join(parts, "  "), " "))
		if len(lines) == 1 && headerShown {
			underlines := make([]string, cols)
			for i, w := range widths {
				underlines[i] = strings.Repeat("─", w)
			}
			lines = append(lines, gutter+style.Styled(strings.Join(underlines, "  "), "muted", ""))
		}
	}
	return prefixLines(lines, firstPrefix, contPrefix)
}

func tableAlignment(table *east.Table, col int) east.Alignment {
	if col < len(table.Alignments) {
		return table.Alignments[col]
	}
	return east.AlignNone
}

// padTableCell pads a styled cell to width with plain spaces outside the
// markup; the visible width comes from the rendered form, so links, escapes,
// and wide runes all measure correctly.
func padTableCell(cell string, width int, align east.Alignment) string {
	pad := width - style.TextWidth(cell)
	if pad <= 0 {
		return cell
	}
	switch align {
	case east.AlignRight:
		return strings.Repeat(" ", pad) + cell
	case east.AlignCenter:
		left := pad / 2
		return strings.Repeat(" ", left) + cell + strings.Repeat(" ", pad-left)
	default:
		return cell + strings.Repeat(" ", pad)
	}
}

// atStreamEdge reports whether n sits on the document's trailing spine — the
// only position where a streamed source may still be appending to it.
func atStreamEdge(n ast.Node) bool {
	for ; n != nil && n.Parent() != nil; n = n.Parent() {
		if n.NextSibling() != nil {
			return false
		}
	}
	return true
}

func prefixLines(lines []string, firstPrefix, contPrefix string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if i == 0 {
			out[i] = firstPrefix + l
		} else {
			out[i] = contPrefix + l
		}
	}
	return out
}

func splitInline(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// renderInlineChildren walks inline nodes, threading the current color and
// modifier. Inner spans override outer ones (gotui markup can't combine
// modifiers), which is the right reading for nested emphasis.
func renderInlineChildren(n ast.Node, source []byte, fg, mod string, state *renderState) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		b.WriteString(renderInline(c, source, fg, mod, state))
	}
	return b.String()
}

func renderInline(n ast.Node, source []byte, fg, mod string, state *renderState) string {
	if state != nil && state.clip.excludes(n) {
		return ""
	}
	switch i := n.(type) {
	case *ast.Text:
		value := i.Segment.Value(source)
		if state != nil {
			value = state.clip.slice(value, i.Segment.Start)
		}
		s := style.Styled(markdownSourceText(string(value), state), fg, mod)
		if (i.SoftLineBreak() || i.HardLineBreak()) && (state == nil || state.clip == nil || state.clip.end >= i.Segment.Stop) {
			s += "\n"
		}
		return s
	case *ast.String:
		return style.Styled(markdownSourceText(string(i.Value), state), fg, mod)
	case *ast.CodeSpan:
		if state == nil || state.clip == nil {
			return style.Styled(markdownSourceText(nodeText(n, source), state), "code", mod)
		}
		return renderInlineChildren(n, source, "code", mod, state)
	case *ast.Emphasis:
		if i.Level >= 2 {
			return renderInlineChildren(n, source, fg, "bold", state)
		}
		return renderInlineChildren(n, source, fg, "italic", state)
	case *east.Strikethrough:
		return renderInlineChildren(n, source, fg, "strike", state)
	case *ast.Link:
		dest := string(i.Destination)
		if state != nil && state.clip != nil && state.clip.end < state.clip.bounds[n].end {
			dest = ""
		}
		return renderLink(
			renderInlineChildren(n, source, "accent", mod, state),
			markdownSourceText(nodeText(n, source), state),
			markdownSourceText(dest, state),
			state,
		)
	case *ast.Image:
		if state != nil && len(state.images) < style.MaxImagesPerBlock {
			if img, ok := ResolveLocalImage(string(i.Destination), nodeText(n, source), state.baseDir); ok {
				index := len(state.images)
				state.images = append(state.images, img)
				if state.clip != nil {
					state.imagePositions = append(state.imagePositions, state.clip.bounds[n].start)
				}
				return style.RenderImage(index, img, "", n.PreviousSibling() != nil, n.NextSibling() != nil)
			}
		}
		return renderLink(
			renderInlineChildren(n, source, "accent", mod, state),
			markdownSourceText(nodeText(n, source), state),
			markdownSourceText(string(i.Destination), state),
			state,
		)
	case *ast.AutoLink:
		value := i.URL(source)
		if state != nil && state.clip != nil {
			value = state.clip.slice(value, state.clip.bounds[n].start)
		}
		return style.Styled(markdownSourceText(string(value), state), "accent", mod)
	case *ast.RawHTML:
		var b strings.Builder
		for s := 0; s < i.Segments.Len(); s++ {
			seg := i.Segments.At(s)
			value := seg.Value(source)
			if state != nil {
				value = state.clip.slice(value, seg.Start)
			}
			b.Write(value)
		}
		return style.Styled(markdownSourceText(b.String(), state), fg, mod)
	default:
		return style.Styled(markdownSourceText(nodeText(n, source), state), fg, mod)
	}
}

// renderLink shows the label in accent plus the destination muted — unless
// the label already is the destination, where repeating it would just shout.
func renderLink(label, labelText, dest string, state *renderState) string {
	if dest == "" || dest == labelText {
		return label
	}
	if state == nil || !state.tableCell {
		dest = style.Truncate(dest, 40)
	}
	return label + style.Styled(" ("+dest+")", "muted", "")
}

func nodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	collectText(n, source, &b)
	return b.String()
}

func collectText(n ast.Node, source []byte, b *strings.Builder) {
	switch t := n.(type) {
	case *ast.Text:
		b.Write(t.Segment.Value(source))
	case *ast.String:
		b.Write(t.Value)
	default:
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			collectText(c, source, b)
		}
	}
}

func codeBlockText(lines *text.Segments, source []byte) string {
	var b strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(source))
	}
	return b.String()
}

// renderCodeBlock keeps the established fence look — a muted "╭─ lang" header
// and "│ " gutters — with chroma highlighting inside, mapped onto the same
// semantic ANSI slots as the rest of the UI so it follows the terminal theme.
func renderCodeBlock(code, lang string) []string {
	lines := HighlightCodeLines(strings.TrimRight(code, "\n"), lang)
	if lang == "" {
		return gutterLines(lines)
	}
	return RenderFence(lang, lines)
}

// RenderFence titles a payload block: the muted "╭─ title" header, then the
// lines under the muted "│ " gutter that the wrapper hard-wraps like code.
// Tool arguments and output use it with section titles; fenced Markdown
// code uses it with the language.
func RenderFence(title string, lines []string) []string {
	return append([]string{style.Styled("╭─ "+title, "muted", "")}, gutterLines(lines)...)
}

func gutterLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	gutter := style.Styled("│ ", "muted", "")
	for _, line := range lines {
		out = append(out, gutter+line)
	}
	return out
}

const codeTabWidth = 4

// expandCodeTabs gives literal tabs deterministic terminal semantics. Terminal
// widgets do not agree on tab width (and some count the styled gutter), so
// expand them before syntax highlighting using tab stops relative to the code
// itself.
func expandCodeTabs(code string) string {
	if !strings.ContainsRune(code, '\t') {
		return code
	}

	var out strings.Builder
	out.Grow(len(code))
	column := 0
	for _, r := range code {
		switch r {
		case '\t':
			spaces := codeTabWidth - column%codeTabWidth
			out.WriteString(strings.Repeat(" ", spaces))
			column += spaces
		case '\n':
			out.WriteRune(r)
			column = 0
		case '\r':
			out.WriteRune(r)
			column = 0
		default:
			out.WriteRune(r)
			if width := rw.RuneWidth(r); width > 0 {
				column += width
			}
		}
	}
	return out.String()
}

func HighlightCodeLines(code, lang string) []string {
	if code == "" {
		return nil
	}
	code = expandCodeTabs(code)
	var lexer chroma.Lexer
	if lang != "" {
		lexer = lexers.Get(lang)
	}
	if lexer == nil {
		return styledLines(code, "code", "")
	}
	iterator, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err != nil {
		return styledLines(code, "code", "")
	}
	var lines []string
	var cur strings.Builder
	for _, tok := range iterator.Tokens() {
		fg, mod := chromaStyle(tok.Type)
		parts := strings.Split(tok.Value, "\n")
		for i, part := range parts {
			if i > 0 {
				lines = append(lines, cur.String())
				cur.Reset()
			}
			if part != "" {
				cur.WriteString(style.Styled(part, fg, mod))
			}
		}
	}
	lines = append(lines, cur.String())
	return lines
}

func styledLines(s, fg, mod string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, len(raw))
	for i, l := range raw {
		out[i] = style.Styled(l, fg, mod)
	}
	return out
}

// chromaStyle maps chroma token categories onto the semantic palette. The
// mapping is deliberately coarse — a handful of hues that follow the terminal
// theme beats a faithful truecolor scheme that fights it.
func chromaStyle(t chroma.TokenType) (fg, mod string) {
	switch {
	case t.InCategory(chroma.Comment):
		return "muted", ""
	case t.InCategory(chroma.Keyword):
		return "accent", ""
	case t.InSubCategory(chroma.LiteralString):
		return "ok", ""
	case t.InSubCategory(chroma.LiteralNumber):
		return "active", ""
	case t == chroma.NameFunction || t == chroma.NameClass || t == chroma.NameNamespace:
		return "code", "bold"
	default:
		return "code", ""
	}
}

// ---------------------------------------------------------------------------
// Streaming holdback
// ---------------------------------------------------------------------------

// holdbackCap bounds how much text an unclosed inline construct may withhold.
// Past it the text shows literally and restyles when the construct closes —
// bounded latency beats a stalled stream.
const holdbackCap = 120

// SafeVisibleLen returns how much of an in-flight markdown message can render
// without showing markup that is still likely to change: unclosed inline
// delimiters near the end of the current line are held back so text appears
// already styled instead of visibly transforming. Completed lines always show.
func SafeVisibleLen(s string) int {
	lineStart := strings.LastIndexByte(s, '\n') + 1
	line := s[lineStart:]
	if insideOpenFence(s[:lineStart]) {
		// Fence bodies are inert — except a trailing backtick-run line, which
		// may be about to become the closing fence.
		if t := strings.TrimLeft(line, " "); t != "" && strings.Trim(t, "`") == "" {
			return lineStart
		}
		return len(s)
	}
	if hold := scanInlineHold(line); hold >= 0 && len(line)-hold <= holdbackCap {
		return lineStart + hold
	}
	return len(s)
}

// insideOpenFence reports whether the text ends inside an unclosed ``` fence.
func insideOpenFence(prefix string) bool {
	open := false
	for _, line := range strings.Split(prefix, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "```") {
			open = !open
		}
	}
	return open
}

// scanInlineHold scans one source line and returns the earliest offset of an
// inline construct that is still open — a backtick span, a bracket that may
// become a link, or an emphasis run — or -1 when everything is settled. The
// emphasis rules are deliberately conservative (opener must follow a space or
// line start): a missed hold only means a rare visible restyle, while a false
// hold delays real text.
func scanInlineHold(line string) int {
	n := len(line)
	codeOpen, codeTicks := -1, 0
	var brackets []int
	linkFrom := -1
	var emph []int

	runLen := func(i int, c byte) int {
		j := i
		for j < n && line[j] == c {
			j++
		}
		return j - i
	}
	escaped := func(i int) bool { return i > 0 && line[i-1] == '\\' }

	i := 0
	for i < n {
		c := line[i]
		if codeOpen >= 0 {
			if c == '`' {
				if k := runLen(i, '`'); k >= codeTicks {
					codeOpen = -1
					i += k
					continue
				} else {
					i += k
					continue
				}
			}
			i++
			continue
		}
		switch c {
		case '`':
			k := runLen(i, '`')
			if !escaped(i) {
				codeOpen, codeTicks = i, k
			}
			i += k
		case '[':
			if !escaped(i) {
				brackets = append(brackets, i)
			}
			i++
		case ']':
			if escaped(i) || len(brackets) == 0 {
				i++
				continue
			}
			start := brackets[len(brackets)-1]
			brackets = brackets[:len(brackets)-1]
			i++
			switch {
			case i < n && line[i] == '(':
				// Link destination in progress: hold from the label's '['.
				linkFrom = start
				for i < n && line[i] != ')' {
					i++
				}
				if i < n {
					linkFrom = -1
					i++
				}
			case i == n:
				// ']' at the buffer edge — '(' may be the next chunk.
				linkFrom = start
			}
		case '*', '_':
			k := runLen(i, c)
			if escaped(i) {
				i += k
				continue
			}
			prevSpace := i == 0 || line[i-1] == ' '
			end := i + k
			nextNonSpace := end < n && line[end] != ' '
			switch {
			case !prevSpace && len(emph) > 0:
				emph = emph[:len(emph)-1]
			case prevSpace && (nextNonSpace || end == n):
				emph = append(emph, i)
			}
			i += k
		default:
			i++
		}
	}

	hold := -1
	consider := func(p int) {
		if p >= 0 && (hold < 0 || p < hold) {
			hold = p
		}
	}
	consider(codeOpen)
	consider(linkFrom)
	for _, p := range brackets {
		consider(p)
	}
	if len(emph) > 0 {
		consider(emph[0])
	}
	return hold
}
