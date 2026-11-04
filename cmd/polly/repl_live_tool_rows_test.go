package main

import (
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func testToolStart(m *replModel, call messages.ChatMessageToolCall) messages.ChatMessageToolCall {
	record := m.appendToolCallStart(call)
	m.refreshToolDisclosure(record)
	return call
}

func testToolEnd(m *replModel, call messages.ChatMessageToolCall, output string, err error) {
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

// inspectorRowsText is the inspected transcript as the pane lays it out.
func inspectorRowsText(v *viewInstance, width int) string {
	var rows []string
	for _, row := range v.view.Rows(v.model, width) {
		rows = append(rows, plainCells(row))
	}
	return strings.Join(rows, "\n")
}

func TestLiveToolsJoinTheActivityLine(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	child := testAcquireSession(t, store, "worker")
	id := child.(sessions.ViewIdentity).ViewID()
	testAddMessages(t, child, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "Run the suite"},
		{Role: messages.MessageRoleAssistant, Content: "Running it now."},
	})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	root := r.visibleTab()
	execution := &swarm.Execution{ID: "execution", Generation: 2, Status: "running", InputSaved: true, Request: swarm.AgentRequest{Task: "Run the suite"}}
	root.swarmSnapshot = &swarm.State{Members: map[string]*swarm.Member{id: {ID: id, Execution: execution.ID}}, Executions: map[string]*swarm.Execution{execution.ID: execution}}
	call := messages.ChatMessageToolCall{ID: "call", DisplayID: "host-call", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`}
	root.swarmActivities = map[string]swarm.LiveActivity{id: {Execution: "execution", Generation: 2, Phase: "tools", Tools: []swarm.LiveToolActivity{{Call: call, Started: time.Now().Add(-3 * time.Second)}}}}
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: id}})
	v := waitInspector(t, r, 140)
	text := inspectorRowsText(v, 100)
	if !strings.Contains(text, "go test ./llm/...") || !strings.Contains(text, "Run the suite") {
		t.Fatalf("running call missing from the activity line, or the task missing from the body: %q", text)
	}
	if len(v.model.activeTools) != 1 || len(v.model.displayCatalog.tools) != 0 {
		t.Fatalf("live call did not tick, or polluted the catalog: active=%+v catalog=%d", v.model.activeTools, len(v.model.displayCatalog.tools))
	}
	v.model.refreshActiveTools()
	if v.model.activeToolsPhase < 0 {
		t.Fatal("paint did not tick the live row")
	}
	// The runtime no longer reports the call: the next projection drops it.
	a := root.swarmActivities[id]
	a.Tools = nil
	root.swarmActivities[id] = a
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	if text := inspectorRowsText(v, 100); strings.Contains(text, "go test ./llm/...") {
		t.Fatalf("settled call lingered without a transcript record: %q", text)
	}
}
