package main

import (
	"bytes"
	"strings"
	"unicode/utf8"

	ui "github.com/metaspartan/gotui/v5"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type markdownSourceSpan struct{ start, end int }
type markdownSourceRange struct {
	start, end int
	bounds     map[ast.Node]markdownSourceSpan
}

func (r *markdownSourceRange) excludes(n ast.Node) bool {
	if r == nil {
		return false
	}
	b, ok := r.bounds[n]
	if ok {
		switch n.(type) {
		case *ast.Image, *ast.String, *ast.ThematicBreak:
			return b.start < r.start || b.start >= r.end
		}
	}
	return ok && (b.end <= r.start || b.start >= r.end)
}

func (r *markdownSourceRange) slice(value []byte, offset int) []byte {
	if r == nil {
		return value
	}
	start, end := max(0, r.start-offset), min(len(value), r.end-offset)
	if start >= end {
		return nil
	}
	return value[start:end]
}

func clippedCodeBlockText(lines *text.Segments, source []byte, state *markdownRenderState) string {
	if state == nil || state.clip == nil {
		return codeBlockText(lines, source)
	}
	var b strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(state.clip.slice(seg.Value(source), seg.Start))
	}
	return b.String()
}

// Source spans are taken from the parsed tree, before any styling or wrapping.
// A late reference definition can change a link's presentation, but never
// moves this boundary back into already committed source.
func markdownSourceBounds(doc ast.Node, source []byte) map[ast.Node]markdownSourceSpan {
	bounds := make(map[ast.Node]markdownSourceSpan)
	var visit func(ast.Node) (markdownSourceSpan, bool)
	visit = func(n ast.Node) (markdownSourceSpan, bool) {
		b := markdownSourceSpan{start: len(source), end: -1}
		add := func(start, end int) { b.start, b.end = min(b.start, start), max(b.end, end) }
		if n.Type() == ast.TypeBlock || n.Type() == ast.TypeDocument {
			lines := n.Lines()
			for i := 0; i < lines.Len(); i++ {
				s := lines.At(i)
				add(s.Start, s.Stop)
			}
		}
		switch node := n.(type) {
		case *ast.Text:
			add(node.Segment.Start, node.Segment.Stop)
		case *ast.RawHTML:
			for i := 0; i < node.Segments.Len(); i++ {
				s := node.Segments.At(i)
				add(s.Start, s.Stop)
			}
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if child, ok := visit(c); ok {
				add(child.start, child.end)
			}
		}
		if b.end < 0 {
			return b, false
		}
		bounds[n] = b
		return b, true
	}
	visit(doc)
	bounds[doc] = markdownSourceSpan{start: 0, end: len(source)}
	// Leaves without source segments (escapes and synthetic text) inherit
	// their containing source span. Empty structural blocks use their gap.
	var fill func(ast.Node)
	fill = func(n ast.Node) {
		if _, ok := bounds[n]; !ok {
			b := bounds[n.Parent()]
			for prev := n.PreviousSibling(); prev != nil; prev = prev.PreviousSibling() {
				if span, ok := bounds[prev]; ok {
					b.start = span.end
					break
				}
			}
			for next := n.NextSibling(); next != nil; next = next.NextSibling() {
				if span, ok := bounds[next]; ok {
					b.end = span.start
					break
				}
			}
			b.start = min(len(source), max(0, b.start))
			b.end = min(len(source), max(b.start, b.end))
			var value []byte
			if link, ok := n.(*ast.AutoLink); ok {
				value = link.Label(source)
			} else if text, ok := n.(*ast.String); ok {
				value = text.Value
			}
			if len(value) > 0 {
				if i := bytes.Index(source[b.start:b.end], value); i >= 0 {
					b.start += i
					b.end = b.start + len(value)
				}
			}
			bounds[n] = b
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			fill(c)
		}
	}
	fill(doc)
	return bounds
}

type lineMarkdownDocument struct {
	source         []byte
	doc            ast.Node
	bounds         map[ast.Node]markdownSourceSpan
	baseDir        string
	cache          *markdownCodeCache
	streaming      bool
	imagePositions []int
}

func newLineMarkdownDocument(src, baseDir string, streaming bool, cache *markdownCodeCache) *lineMarkdownDocument {
	source := []byte(src)
	doc := mdParser.Parser().Parse(text.NewReader(source))
	return &lineMarkdownDocument{source: source, doc: doc, bounds: markdownSourceBounds(doc, source), baseDir: baseDir, streaming: streaming, cache: cache}
}

func (d *lineMarkdownDocument) render(start, end, width int) ([][]ui.Cell, []transcriptImage) {
	if start >= end {
		return nil, nil
	}
	state := &markdownRenderState{baseDir: d.baseDir, streaming: d.streaming, codeCache: d.cache, clip: &markdownSourceRange{start: start, end: end, bounds: d.bounds}}
	lines := renderBlocks(d.doc, d.source, "", state)
	d.imagePositions = state.imagePositions
	if d.cache != nil {
		d.cache.blocks = d.cache.blocks[:state.codeIndex]
	}
	markup := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if markup == "" {
		return nil, nil
	}
	// Prose tabs, like code tabs, must have deterministic physical widths.
	cells := parseStyledCells(markup, ui.StyleClear)
	var expanded []ui.Cell
	column := 0
	for _, c := range cells {
		if c.Rune == '\t' {
			spaces := codeTabWidth - column%codeTabWidth
			c.Rune = ' '
			for range spaces {
				expanded = append(expanded, c)
			}
			column += spaces
		} else {
			expanded = append(expanded, c)
			if c.Rune == '\n' {
				column = 0
			} else {
				column += transcriptCellWidth(c)
			}
		}
	}
	return ui.SplitCells(wrapTranscriptCells(expanded, width), '\n'), state.images
}

func (d *lineMarkdownDocument) completedEnd() int {
	last := d.doc.LastChild()
	if last == nil || last.PreviousSibling() == nil {
		return 0
	}
	start := d.bounds[last].start
	// Include the block's opening marker in its uncommitted range.
	return strings.LastIndexByte(string(d.source[:start]), '\n') + 1
}

// fitPrefix finds a source boundary, never a rendered-row index. It may split
// an open paragraph or code line without replaying its earlier source later.
func (d *lineMarkdownDocument) fitPrefix(start, end, width, rows int) int {
	lo, hi := start, end
	for lo < hi {
		mid := (lo + hi + 1) / 2
		for mid < end && !utf8.RuneStart(d.source[mid]) {
			mid++
		}
		part, _ := d.render(start, mid, width)
		if len(part) <= rows {
			lo = mid
		} else {
			hi = mid - 1
			for hi > lo && !utf8.RuneStart(d.source[hi]) {
				hi--
			}
		}
	}
	if lo == start {
		_, size := utf8.DecodeRune(d.source[start:end])
		return start + size
	}
	return lo
}
