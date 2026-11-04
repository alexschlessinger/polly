package main

import (
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
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
	if fields := m.agentProgressFields(80); fields != nil {
		t.Fatalf("empty transcript has progress: %q", progressText(fields))
	}
	m.appendUserPrompt("Run the suite")
	read := testToolStart(m, messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"llm/agent.go"}`})
	testToolEnd(m, read, "package llm\n", nil)
	run := testToolStart(m, messages.ChatMessageToolCall{ID: "run", Name: "bash", Arguments: `{"command":"go test ./llm/..."}`})
	testToolEnd(m, run, "ok", nil)
	m.appendAssistant("All green.")
	m.finishAssistantBlock("")
	if got := progressText(m.agentProgressFields(80)); got != "2 tool calls · ✓ $ go test ./llm/..." {
		t.Fatalf("activity turn: %q", got)
	}
	// A later turn without activity shows its answer's first line, not the
	// previous turn's calls; an unanswered one shows nothing.
	m.appendUserPrompt("Summarize")
	m.appendAssistant("Retry handling has coverage.\nCancellation does not.")
	m.finishAssistantBlock("")
	if got := progressText(m.agentProgressFields(80)); got != "Retry handling has coverage." {
		t.Fatalf("plain turn: %q", got)
	}
	m.appendUserPrompt("And then")
	if fields := m.agentProgressFields(80); fields != nil {
		t.Fatalf("unanswered turn has progress: %q", progressText(fields))
	}
	// A running call is the line's last field while it runs.
	testToolStart(m, messages.ChatMessageToolCall{ID: "sleep", Name: "bash", Arguments: `{"command":"sleep 5"}`})
	if got := progressText(m.agentProgressFields(80)); got != "1 tool call · $ sleep 5" {
		t.Fatalf("running turn: %q", got)
	}
}

func TestAgentProgressRowsShareExpanderLayout(t *testing.T) {
	call := activityField("✓ $ go test ./llm/...", activityTools, false)
	call.latest = true
	// Fields after the latest call stay with the counts, not the call row.
	fields := []turnDockField{activityField("2 tool calls", activityTools, false), activityField("edited 1 file", activityTools, false), call, activityField("1 agent running", activityAgents, false)}
	for _, width := range []int{80, 34, 30, 20, 12, 8} {
		rows := agentProgressRows(fields, width)
		if len(rows) != 2 {
			t.Fatalf("width %d: summary and command need separate rows: %v", width, rows)
		}
		main, _, _ := renderActivityRow(fields, turnDockField{}, width)
		want := strings.Replace(plainStyledText(main), "  ▸ ", activityIndent, 1)
		if got := plainStyledText(strings.Join(rows, "\n")); got != want {
			t.Fatalf("width %d: preview %q differs from expander %q", width, got, want)
		}
		for _, row := range rows {
			cells := style.VisualRows(row, ui.StyleClear, width)
			if len(cells) != 1 || style.CellsWidth(cells[0]) > width {
				t.Fatalf("width %d: preview wrapped/overflowed: %q", width, row)
			}
		}
		if strings.Contains(plainStyledText(rows[0]), "$ ") || width >= 12 && !strings.Contains(plainStyledText(rows[1]), "$ ") {
			t.Fatalf("width %d: command not on its own row: %v", width, rows)
		}
	}
	if got := plainStyledText(strings.Join(agentProgressRows(fields, 80), "\n")); got != "    2 tool calls · edited 1 file · 1 agent running\n    ✓ $ go test ./llm/..." {
		t.Fatalf("wide preview: %q", got)
	}
	if rows := agentProgressRows(fields, 6); len(rows) != 0 {
		t.Fatalf("unusable width has rows: %v", rows)
	}
	if rows := agentProgressRows(nil, 80); len(rows) != 0 {
		t.Fatalf("empty activity has rows: %v", rows)
	}
	answer := []turnDockField{activityField("No tools needed.", activityTools, false)}
	if rows := agentProgressRows(answer, 80); len(rows) != 1 || plainStyledText(rows[0]) != "    No tools needed." {
		t.Fatalf("answer-only preview: %v", rows)
	}
}

func TestAgentProgressRefitsCallsOnResize(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	root := r.visibleTab()
	id := addProgressWorker(t, store, root, "worker")
	worker := testAcquireSession(t, store, "worker")
	call := messages.ChatMessageToolCall{ID: "run", Name: "bash", Arguments: `{"command":"go test ./cmd/polly ./swarm ./tools ./sessions ./llm/... -count=1"}`}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"}
	result.SetToolSucceeded(true)
	testAddMessages(t, worker, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "Run the suite"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		result,
	})
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
	r.state.sessionStore = counted
	r.openAgentsInspector()
	list := waitAllAgentProgress(t, r)
	base := r.childViews.entries["progress:"+id].model
	histories := counted.histories.Load()
	for _, status := range []string{"running", "completed"} {
		root.swarmSnapshot.Executions[root.swarmSnapshot.Members[id].Execution].Status = status
		for _, width := range []int{36, 100, 44, 100} {
			r.refreshAgentsInspector(viewGeometry{width: width})
			p := list.progress[id]
			if !p.loading {
				t.Fatalf("%s width %d: resize did not schedule a projection", status, width)
			}
			waitProgressTask(t, r)
			r.refreshAgentsInspector(viewGeometry{width: width})
			if p.loading || p.width != width || len(p.fields) != 2 || !p.fields[1].latest {
				t.Fatalf("%s width %d: resized progress %+v", status, width, p)
			}
			if width == 100 && p.fields[1].raw != "✓ $ go test ./cmd/polly ./swarm ./tools ./sessions ./llm/... -count=1" {
				t.Fatalf("wide command still capped: %q", p.fields[1].raw)
			}
			if width < 100 && !strings.HasSuffix(p.fields[1].raw, "…") {
				t.Fatalf("narrow command not refitted: %q", p.fields[1].raw)
			}
			if status == "running" && (r.childViews.entries["progress:"+id].model != base || counted.histories.Load() != histories) {
				t.Fatal("resize reread or rehydrated a cached checkpoint")
			}
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
		// Completed executions awaiting review stay visible with their activity.
		Tasks: map[string]*swarm.Task{"t1": {ID: "t1", Owner: workerID, Status: "awaiting_review"}, "t2": {ID: "t2", Owner: quietID, Status: "awaiting_review"}},
	}
	r.openAgentsInspector()
	text := waitAgentsList(t, r, 160, "✓ $ go test ./llm/...")
	if !strings.Contains(text, "1 tool call\n    ✓ $ go test") {
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
	if open(workerID) != 3 || open(quietID) != 1 {
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
