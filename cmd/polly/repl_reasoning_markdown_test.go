package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestReasoningMarkdownLiveRestoredAndResized(t *testing.T) {
	const source = "# Plan\n\nA paragraph with **bold**, *italic*, and [docs](https://example.com).\n\n- outer\n  - nested\n\n```go\nfunc f() {\n    println(\"literal **stars**\")\n}\n```\n\n| Column | Value |\n|---|---|\n| wide 界 label | `code` |"
	m := newReplModel()
	m.beginTurn("explain")
	// Deliberately split inline and block syntax across callbacks.
	for _, part := range []string{source[:30], source[30:101], source[101:]} {
		m.appendThinking(part)
		m.refreshReasoningRecords(80)
	}
	record := m.currentReasoningRecord()
	m.completeThinkingTurn(false)
	restored := newReplModel()
	restored.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "explain"},
		{Role: messages.MessageRoleAssistant, Reasoning: source},
	}, "markdown")
	reloaded := restored.reasoningRecords.get(restored.reasoningOrder[0])
	for _, width := range []int{100, 40, 80} {
		for _, tc := range []struct {
			model  *replModel
			record *reasoningRecord
		}{{m, record}, {restored, reloaded}} {
			tc.record.expanded = true
			tc.record.ensureSegments()
			tc.record.segments[0].full = true
			tc.model.refreshReasoningRecords(width)
			want := markdown.RenderMutedRows(source, width-style.TextWidth(reasoningBlockIndent)-stepColumn(width), nil)
			if got := tc.record.previewLines; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("width %d: live/restored preview differs from shared renderer", width)
			}
			entry := tc.model.transcript[tc.record.transcriptIndex].text
			plain := plainStyledText(entry)
			for _, marker := range []string{"• outer", "• nested", "╭─ go", "literal **stars**", "Column", "\n  \n"} {
				if !strings.Contains(plain, marker) {
					t.Fatalf("width %d lost Markdown structure %q: %q", width, marker, plain)
				}
			}
			for _, row := range transcriptBlockRows(entry, false, width) {
				if style.CellsWidth(row) > width {
					t.Fatalf("reasoning overflows width %d", width)
				}
			}
		}
	}
}

func TestReasoningMarkdownTruncationKeepsLaterProse(t *testing.T) {
	const ending = "```\n\n# Next heading\n\n**Important** conclusion."
	source := "```text\n" + strings.Repeat("payload line\n", 1500) + ending
	for _, chunkSize := range []int{len(source), 64} {
		m := newReplModel()
		m.beginTurn("explain")
		for start := 0; start < len(source); start += chunkSize {
			m.appendThinking(source[start:min(len(source), start+chunkSize)])
		}
		m.completeThinkingTurn(false)
		m.refreshReasoningRecords(80)
		record := m.reasoningRecords.get(m.reasoningOrder[0])
		if !record.tailCut || len(record.tail) > reasoningTailLimitRunes || record.tailPrefix == "" {
			t.Fatalf("chunk size %d lost bounded Markdown context", chunkSize)
		}
		got := plainStyledText(strings.Join(record.previewLines, "\n"))
		if strings.Contains(got, "# Next heading") || strings.Contains(got, "**Important**") || !strings.HasSuffix(got, "Important conclusion.") {
			t.Fatalf("chunk size %d turned a closing fence into an opener: %q", chunkSize, got)
		}
		copy := cloneReasoningRecord(record)
		if copy.tailPrefix != record.tailPrefix || copy.previewCode != nil {
			t.Fatal("view copy lost fence context or shared its mutable highlight cache")
		}
	}
}

func TestReasoningMarkdownSettlementFlushesHeldMarkup(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	m.appendThinking("visible **unfinished")
	record := m.currentReasoningRecord()
	m.refreshReasoningRecords(80)
	if got := plainStyledText(strings.Join(record.previewLines, "\n")); strings.Contains(got, "unfinished") {
		t.Fatalf("live reasoning leaked incomplete Markdown: %q", got)
	}
	version := record.tailVersion
	m.completeThinkingTurn(false)
	m.refreshReasoningRecords(80)
	if record.tailVersion != version || record.previewActive {
		t.Fatal("settlement changed the source or left a live preview cache")
	}
	if got := plainStyledText(strings.Join(record.previewLines, "\n")); got != "visible **unfinished" {
		t.Fatalf("settlement failed to reveal held text: %q", got)
	}
}

func TestReasoningMarkdownWindowsRenderedRows(t *testing.T) {
	const source = "# Heading\n\nParagraph.\n\n- first\n- second\n\n```text\n    code\n    final\n```"
	m := newReplModel()
	m.beginTurn("explain")
	m.appendThinking(source)
	m.completeThinkingTurn(false)
	record := m.reasoningRecords.get(m.reasoningOrder[0])
	record.expanded = true
	m.refreshReasoningRecords(80)
	block := activityBlocks(m, 80)[0]
	preview := strings.Split(block.activityReasoningDetail, "\n")
	if len(preview) != reasoningPreviewLines || strings.Contains(plainStyledText(preview[0]), "earlier lines") {
		t.Fatalf("abridged preview must keep %d rows of text and no mark: %q", reasoningPreviewLines, preview)
	}
	record.ensureSegments()
	record.segments[0].full = true
	m.visual.invalidate()
	expanded := plainStyledText(activityBlocks(m, 80)[0].activityReasoningDetail)
	// One-row paragraphs stack; the two-row list keeps its break before the code.
	if !strings.Contains(expanded, "Heading\n  Paragraph.\n  • first") || !strings.Contains(expanded, "• second\n  \n  ╭─ text") || !strings.Contains(expanded, "│     code") {
		t.Fatalf("expanded reasoning lost list or code spacing: %q", expanded)
	}
	if strings.Contains(expanded, "view stored") {
		t.Fatal("reasoning must remain inline, without a separate viewer")
	}
}

func TestLineReasoningDetailsKeepMarkdownSpacing(t *testing.T) {
	line, _, status := activityTestUI(t, true, &Config{ActivityDetails: true})
	line.ShowThinking("**bold**\n\n```text\n    indented\n```")
	line.CompleteTurn(turnCompletion{})
	got := lineTestScreen(status.String())
	if !strings.Contains(got, "bold\n\n  ╭─ text\n  │     indented") || strings.Contains(got, "**bold**") || strings.Contains(got, "fg:") {
		t.Fatalf("line details lost Markdown layout or leaked markup: %q", got)
	}
}

func TestReasoningMarkdownUsesEmphasisNotBlanketItalics(t *testing.T) {
	lines := reasoningTailLines("plain **bold** *italic* ~~gone~~", 80, 0)
	cells := style.ParseCells(strings.Join(lines, "\n"), ui.StyleClear)
	for _, tc := range []struct {
		text string
		mod  ui.Modifier
	}{{"plain", 0}, {"bold", ui.ModifierBold}, {"italic", ui.ModifierItalic}, {"gone", ui.ModifierStrike}} {
		at := strings.Index(ui.CellsToString(cells), tc.text)
		if at < 0 || cells[at].Style.Modifier != tc.mod || cells[at].Style.Fg != ui.StyleParserColorMap["muted"] {
			t.Fatalf("%q does not have muted Markdown styling: %q", tc.text, lines)
		}
	}
}
