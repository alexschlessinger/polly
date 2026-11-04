package main

import (
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func progressText(fields []turnDockField) string {
	var raw []string
	for _, field := range fields {
		raw = append(raw, field.raw)
	}
	return strings.Join(raw, " · ")
}

func TestAgentProgressFieldsFollowTheLatestTurn(t *testing.T) {
	m := newReplModel()
	if fields := m.agentProgressFields(); fields != nil {
		t.Fatalf("empty transcript has progress: %q", progressText(fields))
	}
	m.appendUserPrompt("Run the suite")
	read := testToolStart(m, messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"llm/agent.go"}`})
	testToolEnd(m, read, "package llm\n", nil)
	run := testToolStart(m, messages.ChatMessageToolCall{ID: "run", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`})
	testToolEnd(m, run, "ok", nil)
	m.appendAssistant("All green.")
	m.finishAssistantBlock("")
	if got := progressText(m.agentProgressFields()); got != "2 tool calls · ✓ $ go test ./llm/..." {
		t.Fatalf("activity turn: %q", got)
	}
	// A later turn without activity shows its answer's first line, not the
	// previous turn's calls; an unanswered one shows nothing.
	m.appendUserPrompt("Summarize")
	m.appendAssistant("Retry handling has coverage.\nCancellation does not.")
	m.finishAssistantBlock("")
	if got := progressText(m.agentProgressFields()); got != "Retry handling has coverage." {
		t.Fatalf("plain turn: %q", got)
	}
	m.appendUserPrompt("And then")
	if fields := m.agentProgressFields(); fields != nil {
		t.Fatalf("unanswered turn has progress: %q", progressText(fields))
	}
	// A running call is the line's last field while it runs.
	testToolStart(m, messages.ChatMessageToolCall{ID: "sleep", Name: "bash", Arguments: `{"command":"sleep 5"}`})
	if got := progressText(m.agentProgressFields()); got != "1 tool call · $ sleep 5" {
		t.Fatalf("running turn: %q", got)
	}
}

func TestAgentProgressRowNeverWraps(t *testing.T) {
	fields := []turnDockField{activityField("2 tool calls", activityTools, false), activityField("edited 1 file", activityTools, false), activityField("✓ $ go test ./llm/...", activityTools, false)}
	for width, want := range map[int]string{
		80: "    2 tool calls · edited 1 file · ✓ $ go test ./llm/...",
		34: "    2 tool calls · edited 1 file · …",
		30: "    2 tool calls · …",
		20: "    2 tool calls · …",
		12: "    2 tool …",
		6:  "",
	} {
		if got := plainStyledText(agentProgressRow(fields, width)); got != want {
			t.Fatalf("width %d: %q, want %q", width, got, want)
		}
	}
}

// waitAgentsList drives the loop until the Agents list contains want.
func waitAgentsList(t *testing.T, r *managedREPL, width int, want string) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		r.inspectorRefreshAt = time.Time{}
		r.refreshInspector(width)
		if v := r.workspace().inspector.current; v != nil && v.model != nil {
			if text := inspectorText(v); strings.Contains(text, want) {
				return text
			}
		}
		select {
		case fn := <-r.uiTasks:
			fn()
		case <-time.After(20 * time.Millisecond):
		case <-deadline.C:
			text := ""
			if v := r.workspace().inspector.current; v != nil && v.model != nil {
				text = inspectorText(v)
			}
			t.Fatalf("Agents list never showed %q: %q", want, text)
		}
	}
}

func TestAgentsListShowsEachAgentsLatestActivityLine(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	worker := testAcquireSession(t, store, "worker")
	workerID := worker.(sessions.ViewIdentity).ViewID()
	call := messages.ChatMessageToolCall{ID: "run", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"}
	result.SetToolSucceeded(true)
	testAddMessages(t, worker, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "Run the suite"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		result,
		{Role: messages.MessageRoleAssistant, Content: "All green."},
	})
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	quiet := testAcquireSession(t, store, "quiet")
	quietID := quiet.(sessions.ViewIdentity).ViewID()
	if err := quiet.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	root := r.visibleTab()
	root.swarmSnapshot = &swarm.State{
		Members: map[string]*swarm.Member{
			workerID: {ID: workerID, Name: "worker", Label: "worker", Execution: "e1", Task: "t1"},
			quietID:  {ID: quietID, Name: "quiet", Label: "quiet", Execution: "e2", Task: "t2"},
		},
		Executions: map[string]*swarm.Execution{
			"e1": {ID: "e1", Member: workerID, Status: "completed", Generation: 1},
			"e2": {ID: "e2", Member: quietID, Status: "completed", Generation: 1},
		},
		Tasks: map[string]*swarm.Task{"t1": {ID: "t1", Owner: workerID, Status: "done"}, "t2": {ID: "t2", Owner: quietID, Status: "done"}},
	}
	r.openAgentsInspector()
	text := waitAgentsList(t, r, 160, "✓ $ go test ./llm/...")
	if !strings.Contains(text, "1 tool call · ✓ $ go test") {
		t.Fatalf("progress line lacks the turn's counts: %q", text)
	}
	v := r.workspace().inspector.current
	open := func(id string) int {
		n := 0
		for _, action := range v.agentsActions {
			if action == "agents-open:"+id {
				n++
			}
		}
		return n
	}
	if open(workerID) != 2 || open(quietID) != 1 {
		t.Fatalf("rows per agent: %v", v.agentsActions)
	}
	// The keys move by agent, not by row.
	state := r.workspace().viewState(r.workspace().inspector.target)
	state.agents.selected = ""
	r.workspace().inspector.focused = true
	r.navigateAgentsInspector("<Down>")
	first := state.agents.selected
	r.navigateAgentsInspector("<Down>")
	if second := state.agents.selected; first == second || first == "" || second == "" {
		t.Fatalf("Down did not move to the next agent: %q then %q", first, second)
	}
	// A running agent's line follows the calls its runtime reports.
	root.swarmSnapshot.Executions["e1"].Status = "running"
	root.swarmActivities = map[string]swarm.LiveActivity{workerID: {Execution: "e1", Generation: 1, Phase: "tools", Tools: []swarm.LiveToolActivity{{Call: messages.ChatMessageToolCall{ID: "sleep", DisplayID: "host-sleep", Name: "bash", Arguments: `{"command":"sleep 5"}`}, Started: time.Now()}}}}
	if text := waitAgentsList(t, r, 160, "$ sleep 5"); strings.Contains(text, "✓ $ go test") {
		t.Fatalf("running call did not replace the settled one: %q", text)
	}
	if sessionInUse(t, store, "worker") {
		t.Fatal("listing progress acquired the agent's lease")
	}
}
