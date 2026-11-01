package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func continuationMessage(id, role, content string) messages.ChatMessage {
	return messages.ChatMessage{Role: role, Content: content, Metadata: map[string]any{messages.MetadataKeyDisplayID: id}}
}

func continuationText(m *replModel) string {
	m.renderPendingMarkdown()
	m.refreshReasoningRecords(100)
	return strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n")
}

func TestLiveContinuationPreservesSavedBaseAndIdentity(t *testing.T) {
	history := messages.User("Check the tests")
	saved := newReplModel()
	saved.hydrateHistory(history, "agent")
	before := continuationText(saved)
	call := messages.ChatMessageToolCall{ID: "repeated", DisplayID: "host-call", Name: "bash", Arguments: `{"command":"go test ./..."}`}
	proposal := continuationMessage("host-message", messages.MessageRoleAssistant, "Running the relevant checks.")
	proposal.ToolCalls = []messages.ChatMessageToolCall{call}
	live := prepareLiveDisplay(saved, []messages.ChatMessage{proposal})
	text := continuationText(live)
	if strings.Contains(text, "incomplete") || strings.Count(text, proposal.Content) != 1 {
		t.Fatalf("unexpected live transcript: %s", text)
	}
	if len(live.displayCatalog.tools) != 1 || live.displayCatalog.tools[0].complete || live.displayCatalog.tools[0].key != "tool:host-call" {
		t.Fatalf("pending call not live: %+v", live.displayCatalog.tools)
	}
	for _, record := range live.toolDisclosures.all() {
		if record.complete || len(record.rows) != 1 || record.rows[0].settled || record.rows[0].pres.outcome != toolOutcomeRunning {
			t.Fatalf("pending row is not running: %+v", record)
		}
		record.expanded = true
		record.rows[0].outputExpanded = true
	}
	choice := viewState{}
	rememberViewSections(live, &choice)
	result := continuationMessage(call.DisplayID, messages.MessageRoleTool, "checks passed")
	result.ToolCallID, result.ToolName = call.ID, call.Name
	result.SetToolSucceeded(true)
	durable := newReplModel()
	durable.hydrateHistory(append(history, proposal, result), "agent")
	applyViewSections(durable, choice)
	for _, record := range durable.toolDisclosures.all() {
		if !record.expanded || !record.rows[0].outputExpanded || !record.rows[0].settled {
			t.Fatalf("choice/result lost at checkpoint: %+v", record)
		}
	}
	if got := continuationText(saved); got != before || len(saved.displayCatalog.tools) != 0 {
		t.Fatalf("saved base mutated: before %q after %q", before, got)
	}
	again := prepareLiveDisplay(saved, []messages.ChatMessage{proposal})
	if strings.Count(continuationText(again), proposal.Content) != 1 {
		t.Fatal("repeated live snapshot duplicated content")
	}
	proposal.ToolCalls[0].DisplayID = "next-call"
	next := prepareLiveDisplay(saved, []messages.ChatMessage{proposal})
	applyViewSections(next, choice)
	for _, record := range next.toolDisclosures.all() {
		if record.rows[0].outputExpanded {
			t.Fatal("new invocation inherited old call's output choice")
		}
	}
}

func TestLiveContinuationCarriesReasoningAndToolContext(t *testing.T) {
	first := continuationMessage("first", messages.MessageRoleAssistant, "")
	first.Reasoning = "First reasoning segment."
	call := messages.ChatMessageToolCall{ID: "same", DisplayID: "call-one", Name: "bash"}
	first.ToolCalls = []messages.ChatMessageToolCall{call}
	history := append(messages.User("Investigate"), first)
	saved := newReplModel()
	saved.hydrateHistory(history, "agent")
	result := continuationMessage(call.DisplayID, messages.MessageRoleTool, "result")
	result.ToolCallID, result.ToolName = call.ID, call.Name
	second := continuationMessage("second", messages.MessageRoleAssistant, "")
	second.Reasoning = "Second reasoning segment."
	live := prepareLiveDisplay(saved, []messages.ChatMessage{result, second})
	if len(live.reasoningOrder) != 1 || len(live.displayCatalog.thoughts) != 1 || live.displayCatalog.thoughts[0].key != "thought:first" {
		t.Fatalf("reasoning split across checkpoint: %#v, %#v", live.reasoningOrder, live.displayCatalog.thoughts)
	}
	if len(live.displayCatalog.tools) != 1 || !live.displayCatalog.tools[0].complete {
		t.Fatalf("saved call not settled by live result: %+v", live.displayCatalog.tools)
	}
	for _, record := range live.toolDisclosures.all() {
		if len(record.rows) != 1 || !record.rows[0].settled {
			t.Fatalf("duplicate/missing result row: %+v", record.rows)
		}
	}
}

func TestDisplayIdentitySurvivesDeniedProjectionWithRepeatedProviderID(t *testing.T) {
	call := messages.ChatMessageToolCall{ID: "reused", DisplayID: "allowed", Name: "bash"}
	allowed := continuationMessage("assistant-allowed", messages.MessageRoleAssistant, "")
	allowed.ToolCalls = []messages.ChatMessageToolCall{call}
	ok := continuationMessage("allowed", messages.MessageRoleTool, "good")
	ok.ToolCallID, ok.ToolName = call.ID, call.Name
	denied := continuationMessage("assistant-denied", messages.MessageRoleAssistant, "")
	call.DisplayID = "denied"
	denied.ToolCalls = []messages.ChatMessageToolCall{call}
	no := continuationMessage("denied", messages.MessageRoleTool, llm.ToolDeniedContent)
	no.ToolCallID, no.ToolName = call.ID, call.Name
	projected := durableTurnMessages([]messages.ChatMessage{allowed, ok, denied, no})
	m := newReplModel()
	m.hydrateHistory(append(messages.User("Task"), projected...), "agent")
	if len(m.displayCatalog.tools) != 2 || m.displayCatalog.tools[0].key != "tool:allowed" || m.displayCatalog.tools[1].key != "tool:denied" || m.displayCatalog.tools[0].pres.outcome == toolOutcomeDenied || m.displayCatalog.tools[1].pres.outcome != toolOutcomeDenied {
		t.Fatalf("denial mixed repeated provider IDs: %+v", m.displayCatalog.tools)
	}
}

func TestLiveContinuationNewReasoningDoesNotRekeySavedThought(t *testing.T) {
	first := continuationMessage("first", messages.MessageRoleAssistant, "First answer.")
	first.Reasoning = "First thought."
	saved := newReplModel()
	saved.hydrateHistory(append(messages.User("Task"), first), "agent")
	second := continuationMessage("second", messages.MessageRoleAssistant, "")
	second.Reasoning = "Second thought."
	live := prepareLiveDisplay(saved, []messages.ChatMessage{second})
	if len(live.displayCatalog.thoughts) != 2 || len(live.reasoningOrder) != 2 {
		t.Fatalf("duplicate thought catalog: %+v / %+v", live.displayCatalog.thoughts, live.reasoningOrder)
	}
	for i, want := range []string{"thought:first", "thought:second"} {
		if got := live.reasoningRecords.get(live.reasoningOrder[i]).sectionKey; got != want {
			t.Fatalf("thought %d = %q, want %q", i, got, want)
		}
	}
	continuationText(live)
	keys := map[string]bool{}
	for _, block := range live.visual.blocks {
		keys[block.key] = true
	}
	if !keys["section:thought:first"] || !keys["section:thought:second"] || !keys["message:first"] {
		t.Fatalf("layout overwrote stable block identities: %+v", keys)
	}
}

func TestLiveDeniedReasoningKeepsDisclosureAndAnchor(t *testing.T) {
	saved := newReplModel()
	saved.hydrateHistory(messages.User("Task"), "agent")
	proposal := continuationMessage("thought-host", messages.MessageRoleAssistant, "")
	proposal.Reasoning = strings.Repeat("Examine this possibility.\n", 10)
	proposal.ToolCalls = []messages.ChatMessageToolCall{{ID: "reused", DisplayID: "denied-host", Name: "bash"}}
	live := prepareLiveDisplay(saved, []messages.ChatMessage{proposal})
	thought := live.reasoningRecords.get(live.reasoningOrder[0])
	thought.expanded, thought.dirty = true, true
	state := viewState{}
	rememberViewSections(live, &state)
	continuationText(live)
	result := continuationMessage("denied-host", messages.MessageRoleTool, llm.ToolDeniedContent)
	result.ToolCallID, result.ToolName = "reused", "bash"
	durable := newReplModel()
	durable.hydrateHistory(append(messages.User("Task"), durableTurnMessages([]messages.ChatMessage{proposal, result})...), "agent")
	applyViewSections(durable, state)
	continuationText(durable)
	record := durable.reasoningRecords.get(durable.reasoningOrder[0])
	if !record.expanded || record.sectionKey != "thought:thought-host" {
		t.Fatalf("denied thought lost identity/choice: %+v", record)
	}
	found := false
	for _, block := range durable.visual.blocks {
		found = found || block.key == "section:thought:thought-host"
	}
	if !found {
		t.Fatal("denied thought lost its rendered activity anchor")
	}
}

func TestLiveCommentaryIsDisplayedAndClosesReasoningRun(t *testing.T) {
	info := &sessions.SessionView{Metadata: &sessions.Metadata{Name: "agent"}, History: messages.User("Task")}
	saved := prepareChildDisplay(info, &Config{}, 100)
	update := continuationMessage("update", messages.MessageRoleAssistant, "")
	update.Reasoning = "Initial thought."
	update.TextBlocks = []messages.AssistantText{{ID: "provider-text", Phase: messages.PhaseCommentary, Text: "Checking the actual work now."}}
	later := continuationMessage("later", messages.MessageRoleAssistant, "")
	later.Reasoning = "Later thought."
	live := prepareLiveDisplay(saved, []messages.ChatMessage{update, later})
	if text := continuationText(live); !strings.Contains(text, "Checking the actual work now.") {
		t.Fatalf("commentary missing: %q", text)
	}
	if len(live.reasoningOrder) != 2 || len(live.displayCatalog.thoughts) != 2 || live.displayCatalog.thoughts[1].key != "thought:later" {
		t.Fatalf("commentary reasoning boundary lost: %+v / %+v", live.reasoningOrder, live.displayCatalog.thoughts)
	}
	// Denial removes only protocol calls/results, not the interim update.
	update.ToolCalls = []messages.ChatMessageToolCall{{ID: "denied", DisplayID: "denied", Name: "bash"}}
	denial := continuationMessage("denied", messages.MessageRoleTool, llm.ToolDeniedContent)
	denial.ToolCallID, denial.ToolName = "denied", "bash"
	info.History = append(messages.User("Task"), durableTurnMessages([]messages.ChatMessage{update, denial})...)
	durable := prepareChildDisplay(info, &Config{}, 100)
	if text := continuationText(durable); strings.Count(text, "Checking the actual work now.") != 1 || len(durable.displayCatalog.thoughts) != 1 || durable.displayCatalog.thoughts[0].key != "thought:update" {
		t.Fatalf("denied commentary/identity lost: %q / %+v", text, durable.displayCatalog.thoughts)
	}
}
