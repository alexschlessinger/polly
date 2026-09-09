package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	rw "github.com/mattn/go-runewidth"
	cellui "github.com/metaspartan/gotui/v5"
	"golang.org/x/term"
)

// Every owned row ends short of the last column and the cursor rests at the
// end of the last row. Scrollback is never cleared (no ED or alternate screen).
// widths also account for reflow when a terminal shrinks between frames.
type lineTerminalFrame struct{ widths []int }

func (f *lineTerminalFrame) physicalRows(columns int) int {
	rows := 0
	for _, width := range f.widths {
		rows += max(1, (width+max(1, columns)-1)/max(1, columns))
	}
	return rows
}

// clear and write each hand the terminal one write: a paint split across a
// write per row lets the terminal show a half-cleared frame in between.
func (f *lineTerminalFrame) clear(w io.Writer, columns, height int) {
	rows := f.physicalRows(columns)
	if height > 0 {
		rows = min(rows, height)
	}
	var b bytes.Buffer
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteString("\x1b[1A")
		}
		b.WriteString("\r\x1b[2K")
	}
	if b.Len() > 0 {
		_, _ = w.Write(b.Bytes())
	}
	f.widths = nil
}

func (f *lineTerminalFrame) write(w io.Writer, rows []string) {
	var b bytes.Buffer
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString("\r")
		b.WriteString(row)
		f.widths = append(f.widths, lineOutputWidth(row))
	}
	if b.Len() > 0 {
		_, _ = w.Write(b.Bytes())
	}
}

func lineOutputWidth(s string) int {
	// ANSI here is renderer-owned SGR, never model-provided cursor controls.
	var plain strings.Builder
	escape := false
	for _, r := range s {
		if r == '\x1b' {
			escape = true
			continue
		}
		if escape {
			if r == 'm' {
				escape = false
			}
			continue
		}
		plain.WriteRune(r)
	}
	return rw.StringWidth(plain.String())
}

func (ui *lineTurnUI) answerSizeLocked() (int, int) {
	if ui.size != nil {
		return ui.size(true)
	}
	columns, rows := max(2, ui.capabilities.columns), 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		if w > 0 {
			columns = w
		}
		if h > 0 {
			rows = h
		}
	}
	return columns, rows
}

func (ui *lineTurnUI) startRendererLocked() {
	if ui.stream == nil && (ui.activity == nil || !ui.activity.caps.live) {
		return
	}
	ui.renderCancel, ui.renderDone = make(chan struct{}), make(chan struct{})
	cancel, done := ui.renderCancel, ui.renderDone
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-cancel:
				return
			case <-ticker.C:
				ui.toolMu.Lock()
				ui.renderActivityLocked()
				ui.toolMu.Unlock()
			}
		}
	}()
}

func (ui *lineTurnUI) stopRendererLocked() {
	if ui.renderCancel != nil {
		close(ui.renderCancel)
		ui.renderCancel = nil
	}
}

type lineStream struct {
	frame           lineTerminalFrame
	cache           markdown.CodeCache
	committed       int // source bytes, not styled cells or rows
	lastPaint       time.Time
	lastSource      string
	lastFooter      string
	columns, height int
	displayedImages map[int]bool
}

func lineCellsOutput(cells []cellui.Cell, color bool) string {
	var out bytes.Buffer
	if color {
		appendANSIStyledCells(&out, cells)
	} else {
		for _, c := range cells {
			if !unicode.IsControl(c.Rune) || c.Rune == '\t' {
				out.WriteRune(c.Rune)
			}
		}
	}
	return out.String()
}

func (s *lineStream) footer(ui *lineTurnUI) []string {
	if !ui.sameTerminal || ui.activity == nil || !ui.activity.caps.live || ui.activity.paused || ui.activity.stopped {
		return nil
	}
	return ui.activityRowsLocked(false)
}

func (s *lineStream) clear(ui *lineTurnUI, columns, height int) {
	w := ui.writer
	if ui.sameTerminal {
		w = ui.errWriter
	}
	s.frame.clear(w, columns, height)
}

// resync clears the owned frame after accounting for a shrunken terminal.
// Resizing can itself scroll the top of an old frame out of view: that
// prefix has become scrollback, so its source boundary advances without
// being written again and only the remaining owned rows are cleared. Every
// clear must go through here, or a later flush replays the scrolled-off
// prefix from a stale committed offset.
func (s *lineStream) resync(ui *lineTurnUI, columns, height int) {
	if overflow := s.frame.physicalRows(columns) - height; overflow > 0 && height > 0 {
		src := ui.markdownBuffer.String()
		visible := max(markdown.SafeVisibleLen(src), s.committed)
		if s.committed < visible {
			doc := markdown.NewDocument(src[:visible], ui.imageBaseDir, true, &s.cache)
			s.committed = doc.FitPrefix(s.committed, visible, max(1, columns-1), overflow)
		}
	}
	s.clear(ui, columns, height)
}

func (s *lineStream) emit(ui *lineTurnUI, doc *markdown.Document, start, end, width int) {
	rows, images := doc.Render(start, end, width)
	for _, cells := range rows {
		if index, prefix := lineImageMarker(cells, len(images)); index >= 0 {
			key := doc.ImagePositions[index]
			if !s.displayedImages[key] {
				s.displayedImages[key] = true
				if payload := lineImagePayload(images[index], ui.capabilities, styledCellsWidth(cells[:prefix])); len(payload) > 0 {
					_, _ = ui.writer.Write(payload)
				}
			}
			continue
		}
		fmt.Fprint(ui.writer, "\r", lineCellsOutput(cells, !ui.capabilities.noColor), "\n")
		ui.contentPrinted, ui.endsWithNewline = true, true
	}
	s.committed = end
}

func (s *lineStream) draw(ui *lineTurnUI, force bool) {
	if ui.prompting || ui.completed {
		return
	}
	if ui.activity != nil && ui.activity.paused {
		return
	}
	src := ui.markdownBuffer.String()
	columns, height := ui.answerSizeLocked()
	width := max(1, columns-1)
	// Every streamed chunk lands here; the throttle decides before the
	// footer is styled so a skipped paint costs one size probe and nothing
	// else. A resize or an explicit request still paints at once.
	throttled := !force && columns == s.columns && height == s.height && len(s.frame.widths) > 0
	if throttled && time.Since(s.lastPaint) < 200*time.Millisecond {
		return
	}
	footer := s.footer(ui)
	footerText := strings.Join(footer, "\n")
	if throttled && src == s.lastSource && footerText == s.lastFooter {
		return
	}
	visible := markdown.SafeVisibleLen(src)
	if visible < s.committed {
		visible = s.committed
	}
	doc := markdown.NewDocument(src[:visible], ui.imageBaseDir, true, &s.cache)
	budget := max(1, height-len(footer))
	s.resync(ui, columns, height)
	if ui.bufferSeparator && visible > s.committed {
		fmt.Fprintln(ui.writer)
		ui.bufferSeparator = false
	}
	if complete := doc.CompletedEnd(); complete > s.committed {
		s.emit(ui, doc, s.committed, complete, width)
		fmt.Fprintln(ui.writer)
	}
	rows, images := doc.Render(s.committed, visible, width)
	for len(rows) > budget {
		cut := doc.FitPrefix(s.committed, visible, width, len(rows)-budget)
		s.emit(ui, doc, s.committed, cut, width)
		rows, images = doc.Render(s.committed, visible, width)
	}
	var answer []string
	for _, cells := range rows {
		if index, _ := lineImageMarker(cells, len(images)); index >= 0 {
			continue
		}
		answer = append(answer, lineCellsOutput(cells, !ui.capabilities.noColor))
	}
	s.frame.write(ui.writer, answer)
	if len(footer) > 0 {
		if len(answer) > 0 {
			fmt.Fprint(ui.errWriter, "\r\n")
		}
		s.frame.write(ui.errWriter, footer)
	}
	s.lastPaint, s.lastSource, s.lastFooter = time.Now(), src, footerText
	s.columns, s.height = columns, height
}

// A boundary commits the whole remaining segment before notices, approvals,
// typed-image receipts, details, or completion can take over the terminal.
func (s *lineStream) flush(ui *lineTurnUI) {
	columns, height := ui.answerSizeLocked()
	s.resync(ui, columns, height)
	src := ui.markdownBuffer.String()
	if ui.bufferSeparator {
		fmt.Fprintln(ui.writer)
		ui.bufferSeparator = false
	}
	doc := markdown.NewDocument(src, ui.imageBaseDir, false, &s.cache)
	s.emit(ui, doc, s.committed, len(src), max(1, columns-1))
	ui.markdownBuffer.Reset()
	s.committed = 0
	s.cache = markdown.CodeCache{}
	s.lastSource = ""
	s.displayedImages = make(map[int]bool)
}
