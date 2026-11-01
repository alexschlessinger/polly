package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func overviewText(render agentOverviewRender) string {
	return strings.Join(transcriptRowsText(render.rows), "\n")
}

func overviewToolStart(m *replModel, id, name, args string) messages.ChatMessageToolCall {
	call := messages.ChatMessageToolCall{ID: id, Name: name, Arguments: args}
	record := m.appendToolCallStart(call)
	m.refreshToolDisclosure(record)
	return call
}

func overviewToolEnd(m *replModel, call messages.ChatMessageToolCall, output string, err error) {
	m.displayCatalog.finishTool(call, output, time.Second, err)
	record, row := m.toolDisclosureRowForCall(call.ID)
	row.settled = true
	row.setPresentation(m.displayCatalog.toolForCall(call.ID).pres)
	row.output = newInlineToolOutput(call, liveToolResult(call, output, err))
	m.takeActiveTool(call.ID)
	m.refreshToolDisclosure(record)
	if len(m.activeTools) == 0 {
		m.completeToolDisclosure()
	}
}

func TestAgentOverviewEmptyIsFactual(t *testing.T) {
	m := newReplModel()
	m.status.title = "not an assignment or an update"
	got := overviewText(renderAgentOverview(m, 80, agentOverviewState{completed: true}))
	for _, want := range []string{"Task", "Assignment unavailable"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in overview:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Result") || strings.Contains(got, m.status.title) || strings.Contains(got, "Latest update") || strings.Contains(got, "Current operation") || strings.Contains(got, "Recent work") {
		t.Fatalf("overview invented assignment/result:\n%s", got)
	}
}

func TestAgentOverviewAssignmentAndLatestAssistant(t *testing.T) {
	m := newReplModel()
	assignment := "Inspect the actual [assignment].\n" + strings.Repeat("Keep this exact instruction.\n", 6) + "Last assignment detail."
	m.appendUserPrompt(assignment)
	m.appendAssistant("An older update.")
	m.finishAssistantBlock("")
	call := overviewToolStart(m, "read", "read_file", `{"path":"src/relevant.go"}`)
	overviewToolEnd(m, call, "tool output is not an assistant update", nil)
	m.appendAssistant("**Actual latest** assistant update.")
	m.finishAssistantBlock("")
	m.appendNoticeLine("A later notice is not an assistant update")
	m.renderPendingMarkdown()
	m.transcriptRows(80)
	before := m.visual

	collapsed := renderAgentOverview(m, 80, agentOverviewState{})
	got := overviewText(collapsed)
	for _, want := range []string{"▸ Task", "Inspect the actual [assignment].", "Actual latest assistant update.", "src/relevant.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Last assignment detail.", "An older update.", "A later notice", "tool output is not", "Show full assignment", "All tools → Watch"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("unexpected %q:\n%s", unwanted, got)
		}
	}
	if !reflect.DeepEqual(before, m.visual) {
		t.Fatal("overview replaced the transcript visual cache")
	}
	if !slices.Contains(collapsed.actions, "overview:assignment") || slices.Contains(collapsed.actions, "watch") {
		t.Fatalf("missing assignment expander or redundant Watch link: %v", collapsed.actions)
	}
	expanded := overviewText(renderAgentOverview(m, 80, agentOverviewState{assignmentExpanded: true}))
	if !strings.Contains(expanded, "Last assignment detail.") || strings.Contains(expanded, "Show full assignment") {
		t.Fatalf("full assignment did not expand:\n%s", expanded)
	}
}

func TestAgentOverviewLatestAssignmentAcrossHistoryWindow(t *testing.T) {
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "not the task", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}
	for i := range resumedTurnLimit + 2 {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("actual assignment %d", i)},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("actual answer %d", i)},
		)
	}
	m := newReplModel()
	m.hydrateHistory(history, "agent")
	for _, entry := range m.transcript {
		if entry.initialPrompt {
			t.Fatal("truncated history incorrectly marked a follow-up as the initial prompt")
		}
	}
	m.renderPendingMarkdown() // assistant provenance must survive materialization.
	copy := childDisplayCopy(m)
	got := overviewText(renderAgentOverview(copy, 80, agentOverviewState{}))
	if !strings.Contains(got, "actual assignment 6") || !strings.Contains(got, "actual answer 6") || strings.Contains(got, "not the task") {
		t.Fatalf("saved overview lost or inferred its assignment/update:\n%s", got)
	}
	m.clearDisplay()
	if m.assignment != "" {
		t.Fatal("clear retained assignment display content")
	}
}

func TestAgentOverviewFollowupAssignmentSkipsSyntheticContextAndQueue(t *testing.T) {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "Original assignment"},
		{Role: messages.MessageRoleAssistant, Content: "Original answer"},
		{Role: messages.MessageRoleUser, Content: "Follow-up assignment"},
		{Role: messages.MessageRoleUser, Content: "Context only", Metadata: map[string]any{messages.MetadataKeyContextImport: true}},
		{Role: messages.MessageRoleUser, Content: "Synthetic mail", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}},
		{Role: messages.MessageRoleAssistant, Content: "Follow-up answer"},
	}, "agent")
	got := overviewText(renderAgentOverview(m, 80, agentOverviewState{completed: true}))
	if !strings.Contains(got, "Follow-up assignment") || strings.Contains(got, "Original assignment") || strings.Contains(got, "Context only") || strings.Contains(got, "Synthetic mail") {
		t.Fatalf("Task did not follow the genuine assignment: %q", got)
	}
	queued := queuedREPLInput{text: "Queued assignment"}
	m.appendQueuedInput(&queued)
	if m.assignment != "Follow-up assignment" {
		t.Fatal("queued input replaced the running assignment")
	}
	m.activateQueuedInput(queued)
	if m.assignment != "Queued assignment" {
		t.Fatal("starting queued input did not update assignment")
	}
}

func TestAgentOverviewResultRequiresFinalAssistant(t *testing.T) {
	for _, after := range []string{"none", "tool", "prompt", "running", "quiet tool"} {
		t.Run(after, func(t *testing.T) {
			m := newReplModel()
			m.appendUserPrompt("task")
			m.appendAssistant("actual assistant text")
			m.finishAssistantBlock("")
			switch after {
			case "tool":
				call := overviewToolStart(m, "tool", "bash", `{"command":"go test ./..."}`)
				overviewToolEnd(m, call, "passed", nil)
			case "prompt":
				m.appendUserPrompt("A new unanswered prompt")
			case "running":
				m.displayCatalog.startTool(messages.ChatMessageToolCall{ID: "quiet", Name: "bash"})
			case "quiet tool":
				m.quiet = true
				call := messages.ChatMessageToolCall{ID: "quiet", Name: "bash"}
				m.displayCatalog.startTool(call)
				m.displayCatalog.finishTool(call, "actual tool result", time.Second, nil)
			}
			got := overviewText(renderAgentOverview(m, 80, agentOverviewState{completed: true}))
			if result := strings.Contains(got, "\nResult\n"); result != (after == "none") {
				t.Fatalf("result heading=%v after=%s:\n%s", result, after, got)
			}
			got = overviewText(renderAgentOverview(m, 80, agentOverviewState{}))
			if strings.Contains(got, "\nResult\n") {
				t.Fatal("unconfirmed completion labeled Result")
			}
		})
	}
}

func TestAgentOverviewUpdateDisclosureAndStream(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt("task")
	m.appendAssistant("Actual streaming update.\n" + strings.Repeat("More detail.\n", 12) + "Last update detail.")
	collapsed := renderAgentOverview(m, 50, agentOverviewState{})
	if got := overviewText(collapsed); !strings.Contains(got, "Actual streaming update.") || strings.Contains(got, "Last update detail.") {
		t.Fatalf("update excerpt not bounded:\n%s", got)
	}
	if !slices.Contains(collapsed.actions, "overview:update") {
		t.Fatal("bounded update has no expansion action")
	}
	m.finishAssistantBlock("")
	m.renderPendingMarkdown()
	copy := childDisplayCopy(m)
	expanded := renderAgentOverview(copy, 50, agentOverviewState{updateExpanded: true})
	if got := overviewText(expanded); !strings.Contains(got, "Last update detail.") || !strings.Contains(got, "Collapse update") {
		t.Fatalf("update expansion did not survive snapshot/settlement:\n%s", got)
	}
}

func TestAgentOverviewParallelToolsAndExplicitChoices(t *testing.T) {
	m := newReplModel()
	first := overviewToolStart(m, "first", "bash", `{"command":"go test ./cmd/polly"}`)
	second := overviewToolStart(m, "second", "read_file", `{"path":"cmd/polly/repl_model.go"}`)
	firstKey := m.displayCatalog.toolForCall(first.ID).key
	secondKey := m.displayCatalog.toolForCall(second.ID).key
	state := agentOverviewState{tools: map[string]bool{firstKey: false}}
	got := overviewText(renderAgentOverview(m, 100, state))
	if strings.Count(got, "Output pending") != 1 || !strings.Contains(got, "go test ./cmd/polly") || !strings.Contains(got, "repl_model.go") {
		t.Fatalf("parallel tools/manual collapse wrong:\n%s", got)
	}
	if agentOverviewToolExpanded(m, firstKey, state) || !agentOverviewToolExpanded(m, secondKey, state) {
		t.Fatal("click default differs from renderer")
	}
	state.tools[secondKey] = true
	overviewToolEnd(m, second, "second tool actual result", nil)
	copy := childDisplayCopy(m)
	got = overviewText(renderAgentOverview(copy, 100, state))
	if !strings.Contains(got, "second tool actual result") || strings.Contains(got, "Output pending") {
		t.Fatalf("manual choices lost across settlement/snapshot:\n%s", got)
	}
	state.disclosures = disclosuresDetailed
	if agentOverviewToolExpanded(copy, firstKey, state) {
		t.Fatal("expand-all overrode explicit close")
	}
	third := overviewToolStart(m, "third", "list_dir", `{"path":"cmd/polly"}`)
	overviewToolEnd(m, third, "directory result", nil)
	if !agentOverviewToolExpanded(m, m.displayCatalog.toolForCall(third.ID).key, state) {
		t.Fatal("new settled tool did not inherit sticky expand-all")
	}
}

func TestAgentOverviewRecentToolsRetainExplicitExpansion(t *testing.T) {
	m := newReplModel()
	var keys []string
	for i := range 5 {
		call := overviewToolStart(m, fmt.Sprint(i), "read_file", fmt.Sprintf(`{"path":"file-%d.go"}`, i))
		err := error(nil)
		if i == 3 {
			err = errors.New("failed to read file")
		}
		overviewToolEnd(m, call, fmt.Sprintf("result-%d", i), err)
		keys = append(keys, m.displayCatalog.toolForCall(call.ID).key)
	}
	state := agentOverviewState{}
	got := overviewText(renderAgentOverview(m, 100, state))
	if strings.Contains(got, "file-0.go") || strings.Contains(got, "file-1.go") || !strings.Contains(got, "file-2.go") || !strings.Contains(got, "failed") {
		t.Fatalf("recent work selection/status wrong:\n%s", got)
	}
	state.tools = map[string]bool{keys[0]: true, keys[1]: false}
	got = overviewText(renderAgentOverview(childDisplayCopy(m), 100, state))
	if !strings.Contains(got, "file-0.go") || !strings.Contains(got, "result-0") || strings.Contains(got, "file-1.go") {
		t.Fatalf("explicit old expansion was evicted:\n%s", got)
	}
}

func TestAgentOverviewOutputTailBoundedAndSanitized(t *testing.T) {
	m := newReplModel()
	call := overviewToolStart(m, "run", "bash", `{"command":"long-running-command"}`)
	_, row := m.toolDisclosureRowForCall(call.ID)
	body := "discarded beginning\n" + strings.Repeat("中line\n", 4000) + "last output [literal]\n\x1b[31mfinal output" + string(style.ImageMarker(0))
	// The projection accepts an existing output snapshot, without inventing
	// streaming callbacks that the runtime does not yet supply.
	row.output = &inlineToolOutput{body: body, loaded: true}
	got := overviewText(renderAgentOverview(m, 60, agentOverviewState{}))
	if strings.Contains(got, "discarded beginning") || !strings.Contains(got, "last output [literal]") || !strings.Contains(got, "final output") || !strings.Contains(got, "… output tail") {
		t.Fatalf("wrong output tail:\n%s", got)
	}
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, style.ImageMarker(0)) || strings.Count(got, "中line") > agentOverviewOutputRows {
		t.Fatalf("output leaked control/media tokens or exceeded row bound:\n%s", got)
	}
	key := m.displayCatalog.toolForCall(call.ID).key
	got = overviewText(renderAgentOverview(m, 60, agentOverviewState{tools: map[string]bool{key: false}}))
	if strings.Contains(got, "final output") {
		t.Fatal("automatic tail overrode manual collapse")
	}
	tail, clipped := agentOverviewOutputTail(strings.Repeat("中", agentOverviewOutputBytes))
	if !clipped || !utf8.ValidString(tail) || len(tail) > agentOverviewOutputBytes {
		t.Fatalf("tail not byte-bounded UTF-8: bytes=%d valid=%v", len(tail), utf8.ValidString(tail))
	}
}

func TestAgentOverviewQuietCatalogAndUnknownOutcomes(t *testing.T) {
	m := newReplModel()
	m.quiet = true
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "actual task"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "legacy", Name: "read_file", Arguments: `{"path":"legacy.txt"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "legacy", ToolName: "read_file", Content: "legacy output without outcome"},
	}, "agent")
	got := overviewText(renderAgentOverview(m, 80, agentOverviewState{}))
	if !strings.Contains(got, "legacy.txt") || strings.Contains(got, "✓") || strings.Contains(got, "Output pending") {
		t.Fatalf("catalog-only tool inferred success/activity:\n%s", got)
	}
}

func TestAgentOverviewMediaAndArtifactsStayInWatch(t *testing.T) {
	m := newReplModel()
	m.imageBaseDir = t.TempDir()
	path := filepath.Join(m.imageBaseDir, "chart.png")
	writeImageFixture(t, path, 8, 4)
	m.appendUserPrompt("Inspect the chart")
	m.appendAssistant("Actual chart update.\n\n![chart](chart.png)")
	m.finishAssistantBlock("")
	call := overviewToolStart(m, "image", "view_image", `{"source":"chart.png"}`)
	overviewToolEnd(m, call, "saved output receipt", nil)
	_, row := m.toolDisclosureRowForCall(call.ID)
	row.inspectionImages = []style.Image{{Path: path, DisplayPath: "chart.png", Alt: "inspected chart", Width: 8, Height: 4}}
	row.output.artifact = &artifacts.Ref{ID: "saved-output", Kind: artifacts.KindText}
	row.output.loaded = false
	key := m.displayCatalog.toolForCall(call.ID).key
	state := agentOverviewState{tools: map[string]bool{key: true}}
	got := renderAgentOverview(m, 80, state)
	text := overviewText(got)
	if !strings.Contains(text, "chart.png") || !strings.Contains(text, "View media in Watch") || !strings.Contains(text, "saved output receipt") || !strings.Contains(text, "Full output in Watch") {
		t.Fatalf("missing media/artifact affordances:\n%s", text)
	}
	if strings.ContainsRune(text, style.ImageMarker(0)) || row.output.loaded || row.outputExpanded {
		t.Fatal("overview leaked markers or changed source output state")
	}
	latest := agentOverviewLatestAssistant(m)
	if latest < 0 || len(m.transcript[latest].images) != 1 || len(row.inspectionImages) != 1 {
		t.Fatal("overview discarded Watch media")
	}
}

func TestAgentOverviewActionsTrackWrappedRows(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt("A long actual assignment with several words and [literal] brackets.")
	m.appendAssistant("An actual long assistant update that wraps in a narrow pane.")
	m.finishAssistantBlock("")
	call := overviewToolStart(m, "tool", "bash", `{"command":"printf 'a useful actual argument'"}`)
	overviewToolEnd(m, call, "a long actual result with [literal] brackets", nil)
	key := m.displayCatalog.toolForCall(call.ID).key
	for _, width := range []int{4, 17, 37, 80} {
		render := renderAgentOverview(m, width, agentOverviewState{tools: map[string]bool{key: true}})
		if len(render.rows) != len(render.actions) {
			t.Fatalf("width %d: rows=%d actions=%d", width, len(render.rows), len(render.actions))
		}
		for i, row := range render.rows {
			if cols := style.TextWidth(ui.CellsToString(row)); cols > width {
				t.Fatalf("width %d: row %d has %d columns: %q", width, i, cols, ui.CellsToString(row))
			}
		}
		for _, action := range []string{"overview:assignment", "overview:tool:" + key, "watch"} {
			if !slices.Contains(render.actions, action) {
				t.Fatalf("width %d: missing action %q", width, action)
			}
		}
	}
}
