package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestTypewriterPacesGraphemesAndBoundsBurstDelay(t *testing.T) {
	start := time.Unix(1, 0)
	clusters := []string{"A", "e\u0301", "👩🏽‍💻", "界", "🇦🇺", "Z"}
	raw := strings.Join(clusters, "")
	boundaries := map[int]bool{0: true}
	end := 0
	for _, cluster := range clusters {
		end += len(cluster)
		boundaries[end] = true
	}
	var s streamTypewriter
	s.append(len(raw), start)
	previous := 0
	for frame := 0; frame <= 12; frame++ {
		n := s.visibleLen(raw, start.Add(time.Duration(frame)*10*time.Millisecond))
		if !boundaries[n] || n < previous {
			t.Fatalf("frame %d revealed a partial or regressing grapheme: %q", frame, raw[:n])
		}
		if frame == 0 && n != len(clusters[0]) {
			t.Fatalf("first paint = %q, want one immediate character", raw[:n])
		}
		previous = n
	}
	if previous != len(raw) {
		t.Fatal("burst did not catch up by its deadline")
	}
	// A long network pause must not make the next burst appear all at once.
	raw += " another sentence"
	next := start.Add(time.Hour)
	s.append(len(raw), next)
	if n := s.visibleLen(raw, next); n != previous {
		t.Fatalf("idle time was banked as reveal credit: %d -> %d", previous, n)
	}
	if n := s.visibleLen(raw, next.Add(typewriterDelay)); n != len(raw) {
		t.Fatal("second burst missed its deadline")
	}
}

func TestTypewriterSustainedFastStreamCannotAccumulateLag(t *testing.T) {
	start := time.Unix(1, 0)
	var s streamTypewriter
	var raw strings.Builder
	previous := 0
	for step := 0; step < 200; step++ {
		now := start.Add(time.Duration(step) * 10 * time.Millisecond)
		raw.WriteString(strings.Repeat("x", 100))
		s.append(raw.Len(), now)
		if step%3 != 0 { // Frames and provider chunks have different cadences.
			continue
		}
		n := s.visibleLen(raw.String(), now)
		due := max(0, step-11) * 100
		if n < due || n < previous || n > raw.Len() {
			t.Fatalf("step %d: showed %d bytes, previous %d, deadline %d", step, n, previous, due)
		}
		previous = n
	}
	if n := s.visibleLen(raw.String(), start.Add(3*time.Second)); n != raw.Len() || len(s.arrivals) != 0 {
		t.Fatal("finished stream retained a reveal backlog")
	}
}

func TestTypewriterPreservesMarkdownAndFlushesAtBoundaries(t *testing.T) {
	withDisplayTTY(t)
	for _, boundary := range []string{"finished", "tool", "canceled"} {
		t.Run(boundary, func(t *testing.T) {
			r, _ := affordanceTestREPL(t)
			m := r.model
			m.beginTurn("show a response")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			raw := "Here is **bold text** and `code` with 界.\n\n```go\nfmt.Println(42)\n```\n"
			tui.AppendAssistantText(raw)
			index := m.currentAssistant
			at := m.streamTypewriter.receivedAt
			m.renderPendingMarkdownAt(at)
			if m.streamRaw.String() != raw || m.streamTypewriter.prefix() == nil {
				t.Fatal("reveal changed incoming content or showed the whole burst at once")
			}
			// The revealed prefix, not the settled entry, is what the viewer
			// sees mid-reveal: it must already be formatted output.
			m.renderPendingMarkdownAt(at.Add(40 * time.Millisecond))
			shown := ui.CellsToString(m.streamTypewriter.prefix())
			if full := plainStyledText(m.transcript[index].text); !strings.Contains(shown, "bold") || !strings.HasPrefix(full, shown) {
				t.Fatalf("typewriter did not reveal formatted text past the emphasis: %q", shown)
			}
			switch boundary {
			case "finished":
				tui.FinishTextTurn()
			case "tool":
				tui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "read", Name: "read_file"}})
			case "canceled":
				r.endTurn(context.Canceled)
			}
			m.renderPendingMarkdownAt(at.Add(41 * time.Millisecond))
			want, _, _ := renderMarkdownWithCache(strings.TrimRight(raw, "\n"), m.imageBaseDir, false, nil)
			if m.transcript[index].text != want || m.currentAssistant != -1 || len(m.streamTypewriter.arrivals) != 0 {
				t.Fatal("semantic boundary failed to flush the exact complete Markdown")
			}
		})
	}
}

func TestTypewriterSkipsUnseenTextAndRespectsDisplayPreferences(t *testing.T) {
	for _, mode := range []string{"quiet", "no color", "hidden", "unfocused", "scrollback"} {
		t.Run(mode, func(t *testing.T) {
			m := newReplModel()
			m.affordances.enabled = true
			m.beginTurn("inspect")
			switch mode {
			case "quiet":
				m.quiet = true
			case "no color":
				m.affordances.enabled = false
			case "hidden":
				m.hidden = true
			case "unfocused":
				m.focusKnown, m.focused = true, false
			case "scrollback":
				m.followBottom = false
			}
			m.appendAssistant("already received text")
			m.quiet, m.hidden = false, false
			m.affordances.enabled, m.focused, m.followBottom = true, true, true
			m.renderPendingMarkdown()
			if m.streamShown != m.streamRaw.Len() || m.streamTypewriter.prefix() != nil {
				t.Fatal("returning to the response replayed unseen typing")
			}
		})
	}
	// Losing focus during a reveal flushes its transient state before return.
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.beginTurn("inspect")
	m.appendAssistant("a response still catching up")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusLostID})
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusGainedID})
	m.renderPendingMarkdown()
	if m.streamShown != m.streamRaw.Len() || m.streamTypewriter.prefix() != nil {
		t.Fatal("focus return replayed pending typing")
	}
}

func TestTypewriterRevealsFinalStylesWithoutMarkdownFlicker(t *testing.T) {
	withDisplayTTY(t)
	for _, raw := range []string{
		"# A heading",
		"**A bold sentence** and `code`.",
		"1. A numbered item",
		"```go\nvar x = []int{1, 2}\n```",
		"[A link](https://example.com) and literal \\[brackets\\]",
	} {
		t.Run(raw, func(t *testing.T) {
			m := newReplModel()
			m.affordances.enabled = true
			m.beginTurn("render")
			m.appendAssistant(raw)
			at := m.streamTypewriter.receivedAt
			m.renderPendingMarkdownAt(at)
			canonical := m.transcript[m.currentAssistant].text
			full := parseStyledCells(strings.TrimRight(canonical, "\r\n"), ui.StyleClear)
			for frame := 0; frame <= 6; frame++ {
				m.renderPendingMarkdownAt(at.Add(time.Duration(frame) * 20 * time.Millisecond))
				prefix := m.streamTypewriter.prefix()
				if prefix != nil && !slices.Equal(prefix, full[:len(prefix)]) {
					t.Fatalf("frame %d changed the revealed text's final styles", frame)
				}
				if m.transcript[m.currentAssistant].text != canonical {
					t.Fatal("typing reparsed a synthetic Markdown prefix")
				}
				// Exercise the real projection and wrapping, including escaped
				// brackets inside code or ordinary text.
				for _, width := range []int{8, 80} {
					m.transcriptRows(width)
					for _, block := range m.visual.blocks {
						if block.cells != nil && !slices.Equal(block.cells, prefix) {
							t.Fatal("wrapped projection lost its formatted prefix")
						}
					}
				}
			}
			if m.streamTypewriter.prefix() != nil {
				t.Fatal("formatted text did not finish revealing")
			}
		})
	}
}

func TestTypewriterKeepsImageSlotsWhole(t *testing.T) {
	dir := t.TempDir()
	writeImageFixture(t, filepath.Join(dir, "chart.png"), 8, 4)
	m := newReplModel()
	m.affordances.enabled = true
	m.imageBaseDir = dir
	m.beginTurn("show an image")
	m.appendAssistant("The chart:\n\n![chart](chart.png)\n\nIts caption follows.")
	m.renderPendingMarkdownAt(m.streamTypewriter.receivedAt)
	entry := m.transcript[m.currentAssistant]
	if len(entry.images) != 1 || m.streamTypewriter.prefix() != nil {
		t.Fatal("image response retained a partial typing projection")
	}
	for _, block := range m.transcriptDisplayEntries(80) {
		if len(block.images) > 0 && strings.Count(block.text, string(transcriptImageMarker(0))) != transcriptImageThumbnailRows {
			t.Fatal("typing split the reserved image slot")
		}
	}
}
