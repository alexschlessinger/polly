package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func TestInlineFilePathsRetainRangeContrastAndExactCall(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees", "parent", "slot-0000", "tree")
	path := filepath.Join(root, "swarm", "runtime_test.go")
	args, _ := json.Marshal(map[string]any{"path": path, "offset": 200, "limit": 220})
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: string(args)}
	m := newReplModel()
	m.toolBaseDir = root
	record := m.appendToolCallStart(call)
	record.rows[0].setLine(inlineToolLine{glyph: "✓", tone: "ok", modifier: "bold", duration: "0.0s"})
	m.toggleToolDisclosure(record.id)
	for _, width := range []int{40, 60, 120, 40} {
		rows := m.transcriptRows(width)
		text := strings.Join(transcriptRowsText(rows), "\n")
		if strings.Contains(text, "worktrees") || !strings.Contains(text, "runtime_test.go:200–419") || !strings.Contains(text, "✓ read ") {
			t.Fatalf("width %d lost useful path details: %s", width, text)
		}
		links := m.visibleInspectionLinks(fullViewport(len(rows), width), 0)
		if len(links) != 1 || links[0].key != record.rows[0].inspectionKey || links[0].rect.Dy() != 1 {
			t.Fatalf("width %d lost exact one-row inspector link: %#v", width, links)
		}
	}
	for width := 1; width <= 120; width++ {
		line := record.rows[0].inlineLineAt(width, root)
		if rw.StringWidth(plainStyledText(line)) > width {
			t.Fatalf("width %d overflow: %q", width, line)
		}
	}
	cells := style.ParseCells(record.rows[0].inlineLineAt(120, root), ui.StyleClear)
	text := ui.CellsToString(cells)
	start := strings.Index(text, "swarm/")
	if start < 0 || cells[len([]rune(text[:start]))].Style.Fg != ui.ColorClear {
		t.Fatalf("filename should use normal foreground: %q", text)
	}
	if m.inspections.toolForCall(call.ID).call.Arguments != call.Arguments {
		t.Fatal("display changed the stored absolute path")
	}
	outside := filepath.Join(root+"-sibling", "swarm", "runtime_test.go")
	if got := relativeToolPath(outside, root); got != filepath.ToSlash(outside) {
		t.Fatalf("path outside owner root was relativized: %q", got)
	}
}

func TestSavedAgentInspectorUsesOwnedExecutionRoot(t *testing.T) {
	withDisplayTTY(t)
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	parent := testAcquireSession(t, store, "root")
	parentID := parent.(sessions.ViewIdentity).ViewID()
	child, err := store.Acquire(ctx, "agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	childID := child.(sessions.ViewIdentity).ViewID()
	root := filepath.Join(t.TempDir(), "owned", "tree")
	metadata, _ := child.GetMetadata(ctx)
	metadata.SwarmID, metadata.ExecutionContext = parentID, "owned"
	if err := child.SetMetadata(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	if err := parent.(sessions.CoordinationSession).UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
		member, _ := json.Marshal(map[string]string{"id": childID, "context": "owned"})
		execution, _ := json.Marshal(map[string]string{"id": "owned", "owner": childID, "root": root})
		state.Records["format"] = map[string]json.RawMessage{"swarm": json.RawMessage(`{"version":2}`)}
		state.Records["member"] = map[string]json.RawMessage{childID: member}
		state.Records["context"] = map[string]json.RawMessage{"owned": execution}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(root, "swarm", "runtime_test.go"), "offset": 200, "limit": 220})
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: string(args)}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "original result"}
	result.SetToolSucceeded(true)
	result.SetToolDuration(time.Millisecond)
	testAddMessages(t, child, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read the file"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, result,
		{Role: messages.MessageRoleAssistant, Content: "done"},
	})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: childID}, kind: conversationViewKind})
	view := waitInspector(t, r, 140)
	if view.model.toolBaseDir != root {
		t.Fatalf("saved inspector root = %q, want %q", view.model.toolBaseDir, root)
	}
	for _, record := range view.model.toolDisclosures.all() {
		view.model.toggleToolDisclosure(record.id)
	}
	rows := view.view.Rows(view.model, 60)
	text := strings.Join(transcriptRowsText(rows), "\n")
	if !strings.Contains(text, "read swarm/runtime_test.go:200–419") || strings.Contains(text, root) {
		t.Fatalf("saved child path was not relative: %s", text)
	}
	view.model.inspectionLinks = view.model.visibleInspectionLinks(fullViewport(len(rows), 60), 0)
	if len(view.model.inspectionLinks) != 1 {
		t.Fatalf("saved child lost click target: %#v", view.model.inspectionLinks)
	}
	link := view.model.inspectionLinks[0]
	if !r.inspectViewAt(view.model, r.workspace().inspector.target, link.rect.Min) {
		t.Fatal("clicking saved child file did not open tool")
	}
	detail := inspectorText(waitInspector(t, r, 140))
	if !strings.Contains(detail, filepath.Join(root, "swarm", "runtime_test.go")) || !strings.Contains(detail, "original result") {
		t.Fatalf("file detail lost the original absolute path/result: %s", detail)
	}
}
