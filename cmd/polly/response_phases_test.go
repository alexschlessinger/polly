package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/replay"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestCommentaryIsHiddenInLiveAndResumedTranscript(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "phase", 0, 0)
	m := r.model
	m.beginTurn("go")
	ui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	turn := &turnExecution{config: r.config, turnUI: ui}
	callbacks := turn.callbacks()
	commentary := messages.AssistantText{ID: "a", Phase: messages.PhaseCommentary, Text: "Checking the connection. Connection works."}
	callbacks.OnCommentary(commentary, true)
	visible := plainStyledText(strings.Join(rowsText(m.transcriptRows(100)), "\n"))
	if strings.Contains(visible, commentary.Text) || strings.Contains(visible, "commentary") || len(m.inspections.thoughts) > 0 {
		t.Fatalf("commentary became visible: %q", visible)
	}
	callbacks.OnContent("Connection works.")
	r.endTurn(nil)
	m.renderPendingMarkdown()
	visible = plainStyledText(strings.Join(rowsText(m.transcriptRows(100)), "\n"))
	if strings.Count(visible, "Connection works.") != 1 || strings.Contains(visible, "Checking the connection.") || strings.Contains(visible, "commentary") {
		t.Fatalf("settled=%q", visible)
	}
	history := append(messages.User("go"), messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Connection works.", TextBlocks: []messages.AssistantText{commentary, {ID: "b", Phase: messages.PhaseFinalAnswer, Text: "Connection works."}}, StopReason: messages.StopReasonEndTurn})
	loaded := newReplModel()
	loaded.hydrateHistory(history, "phase")
	loaded.renderPendingMarkdown()
	visible = plainStyledText(strings.Join(rowsText(loaded.transcriptRows(100)), "\n"))
	if strings.Count(visible, "Connection works.") != 1 || strings.Contains(visible, "Checking the connection.") || strings.Contains(visible, "commentary") || len(loaded.inspections.thoughts) > 0 {
		t.Fatalf("resumed=%q", visible)
	}
}

func TestPhaseCallbacksKeepCommentaryOffLineOutput(t *testing.T) {
	name := "phase-callback"
	replay.Install(name, []replay.Turn{{Steps: []replay.Step{{Text: &messages.AssistantText{ID: "a", Phase: messages.PhaseCommentary, Text: "Checking. same"}}, {Text: &messages.AssistantText{ID: "b", Phase: messages.PhaseFinalAnswer, Text: "same"}}}}}, nil)
	defer replay.Uninstall(name)
	var out, errout bytes.Buffer
	ui := &lineTurnUI{config: &Config{}, writer: &out, errWriter: &errout}
	ui.Start()
	defer ui.Stop()
	turn := &turnExecution{config: ui.config, turnUI: ui}
	callbacks := turn.callbacks()
	callbacks.BeforeFirstRequest = nil // This test exercises output; persistence is covered by the provider round trip.
	agent := llm.NewAgent(replay.NewProvider(), nil, llm.AgentConfig{})
	_, err := agent.Run(context.Background(), &llm.CompletionRequest{Model: name, Messages: messages.User("go"), Capabilities: &llm.ModelCapabilities{}}, callbacks)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "same" || strings.Contains(errout.String(), "Checking.") || strings.Contains(errout.String(), "commentary") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errout.String())
	}
}
