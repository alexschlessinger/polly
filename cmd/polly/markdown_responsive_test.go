package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

const paneTable = "| Skill | Description | Extras |\n|---|---|---|\n| release-linksnaps | Coordinate frontend and backend release with tests and browser validation | scripts/ |\n| deploy | Build and deploy the production application | deploy.sh |"

func tablePaneText(t *testing.T, m *replModel, width int) string {
	t.Helper()
	rows := m.transcriptRows(width)
	var lines []string
	for _, row := range rows {
		if style.CellsWidth(row) > width {
			t.Fatalf("overflow at %d: %q", width, ui.CellsToString(row))
		}
		lines = append(lines, ui.CellsToString(row))
	}
	return strings.Join(lines, "\n")
}

func TestResponsiveTranscriptResizeAndCache(t *testing.T) {
	m := newReplModel()
	m.appendAssistant(paneTable + "\n\n```go\nvar x = 1\n```")
	m.finishAssistantBlock("")
	m.renderPendingMarkdown()
	wide := tablePaneText(t, m, 100)
	if strings.Contains(wide, "Description:") {
		t.Fatal("wide table stacked")
	}
	_, code := m.transcript[0].codeCache.Block(0)
	first := &code[0]
	cached := m.transcript[0].text
	oldRows := m.transcriptRows(100)
	if rows := m.transcriptRows(100); &rows[0][0] != &oldRows[0][0] || m.transcript[0].text != cached {
		t.Fatal("unchanged layout not cached")
	}
	narrow := tablePaneText(t, m, 30)
	if !strings.Contains(narrow, "Description:") {
		t.Fatal("narrow table not stacked")
	}
	if got := tablePaneText(t, m, 100); got != wide {
		t.Fatalf("resize did not restore layout:\n%s", got)
	}
	if _, code := m.transcript[0].codeCache.Block(0); &code[0] != first {
		t.Fatal("resize repeated syntax highlighting")
	}
	if !strings.HasPrefix(m.transcript[0].markdownSource, paneTable) {
		t.Fatal("resize lost original Markdown")
	}
}

func TestResponsiveTranscriptStreamingAndCancellation(t *testing.T) {
	withDisplayTTY(t)
	m := newReplModel()
	m.transcriptRows(60)
	m.appendAssistant("| Skill | Description | Extras |\n")
	m.renderPendingMarkdown()
	if !strings.Contains(plainStyledText(m.transcript[0].text), "| Skill |") {
		t.Fatal("header recognized before delimiter")
	}
	m.appendAssistant("|---|---|---|\n| deploy | short | x |\n")
	m.renderPendingMarkdown()
	if !strings.Contains(m.transcript[0].text, "─") {
		t.Fatal("live table not laid out")
	}
	m.appendAssistant("| release-linksnaps | a much longer description that needs several lines | scripts/ |")
	m.renderPendingMarkdown()
	tablePaneText(t, m, 30)
	if m.streamTypewriter.prefix() != nil {
		t.Fatal("reflow replayed existing content")
	}
	raw := m.streamRaw.String()
	m.finishAssistantBlock("canceled")
	m.renderPendingMarkdown()
	tablePaneText(t, m, 80)
	if m.transcript[0].markdownSource != raw {
		t.Fatal("cancellation discarded partial source")
	}
}

func TestResponsiveRestoredConversationAndInspectorCopy(t *testing.T) {
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "show skills"}, {Role: messages.MessageRoleAssistant, Content: paneTable}}
	m := newReplModel()
	m.hydrateHistory(history, "table-test")
	m.renderPendingMarkdown()
	wide := tablePaneText(t, m, 100)
	child := childDisplayCopy(m)
	if got := tablePaneText(t, child, 30); !strings.Contains(got, "Description:") {
		t.Fatal("conversation inspector did not reflow")
	}
	if got := tablePaneText(t, m, 100); got != wide {
		t.Fatal("inspector changed parent")
	}
	if got := tablePaneText(t, child, 100); got != wide {
		t.Fatal("inspector widening did not restore table")
	}
	if !slices.EqualFunc(history, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "show skills"}, {Role: messages.MessageRoleAssistant, Content: paneTable}}, func(a, b messages.ChatMessage) bool { return a.Content == b.Content }) {
		t.Fatal("history mutated")
	}
}
