package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestInspectorRuntimeOnlyToolKeepsManualSectionsAtCheckpoint(t *testing.T) {
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
	root.swarmSnapshot = &swarm.State{
		Members:    map[string]*swarm.Member{id: {ID: id, Execution: "execution"}},
		Executions: map[string]*swarm.Execution{"execution": {ID: "execution", Generation: 2, Status: "running", InputSaved: true}},
	}
	call := messages.ChatMessageToolCall{ID: "provider-call", DisplayID: "host-call", Name: "bash", Arguments: `{"command":"sleep 60"}`}
	root.swarmActivities = map[string]swarm.LiveActivity{id: {
		Execution: "execution", Generation: 2, Phase: "tools",
		Tools: []swarm.LiveToolActivity{{Call: call, Started: time.Now()}},
	}}
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: id}})
	v := waitInspector(t, r, 140)
	record := v.model.currentTurnToolRecord()
	if record == nil || !record.expanded || len(record.rows) != 1 {
		t.Fatal("initial running call did not open")
	}
	v.model.setToolDisclosureExpanded(record, false, false)
	// A detailed-row choice must also survive the runtime row becoming durable.
	record.rows[0].outputExpanded = true
	state := r.workspace().viewState(r.workspace().inspector.target)
	rememberViewSections(v.model, state)
	check := func(v *viewInstance) *toolDisclosureRow {
		t.Helper()
		record := v.model.currentTurnToolRecord()
		if record == nil || record.expanded || len(record.rows) != 1 {
			t.Fatalf("manual fold or row identity lost: record=%+v sections=%+v", record, state.sections)
		}
		row := &record.rows[0]
		if row.sectionKey != "tool:host-call" || !row.outputExpanded {
			t.Fatalf("host-identified output choice lost: %+v", row)
		}
		return row
	}
	for _, width := range []int{140, 85} {
		r.inspectorRefreshAt = time.Time{}
		check(waitInspector(t, r, width))
	}

	child = testAcquireSession(t, store, "worker")
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "saved output"}
	result.SetToolSucceeded(true)
	testAddMessages(t, child, []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, result,
	})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	// Stale runtime metadata must not duplicate the now-durable host identity.
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	row := check(v)
	if !row.settled || row.output == nil {
		t.Fatal("checkpoint did not replace the running row")
	}
	output := row.output
	r.inspectorRefreshAt = time.Time{}
	if row := check(waitInspector(t, r, 85)); row.output != output {
		t.Fatal("reprojection discarded the carried output snapshot")
	}
}

func addProgressWorker(t *testing.T, store sessions.SessionStore, root *replTab, name string) string {
	t.Helper()
	worker := testAcquireSession(t, store, name)
	id := worker.(sessions.ViewIdentity).ViewID()
	testAddMessages(t, worker, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "task"},
		{Role: messages.MessageRoleAssistant, Content: "progress"},
	})
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if root.swarmSnapshot == nil {
		root.swarmSnapshot = &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}}
	}
	execution := "execution-" + id
	root.swarmSnapshot.Members[id] = &swarm.Member{ID: id, Name: name, Execution: execution}
	root.swarmSnapshot.Executions[execution] = &swarm.Execution{ID: execution, Member: id, Status: "running", Generation: 1}
	return id
}

func waitProgressTask(t *testing.T, r *managedREPL) {
	t.Helper()
	select {
	case fn := <-r.uiTasks:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("progress task did not finish")
	}
}

func waitAllAgentProgress(t *testing.T, r *managedREPL) *agentsInspectorState {
	t.Helper()
	for {
		r.refreshInspector(160)
		list := r.workspace().viewState(r.workspace().inspector.target).agents
		loaded := len(list.progress) == len(list.order)
		for _, p := range list.progress {
			loaded = loaded && !p.loading && p.key != ""
		}
		if loaded {
			return list
		}
		waitProgressTask(t, r)
	}
}

func TestAgentProgressCheckpointsShareCacheBounds(t *testing.T) {
	for _, bound := range []string{"entries", "bytes", "oversized"} {
		t.Run(bound, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "root")
			root := r.visibleTab()
			id := addProgressWorker(t, store, root, "worker-0")
			r.openAgentsInspector()
			waitAllAgentProgress(t, r)
			cached := r.childViews.entries["progress:"+id]
			if cached == nil || cached.model == nil || cached.bytes <= 0 || cached.model.artifactStore != nil {
				t.Fatal("busy progress checkpoint was not isolated and budgeted")
			}
			switch bound {
			case "entries":
				r.childViews.maxEntries = 2
			case "bytes":
				r.childViews.maxBytes = cached.bytes * 3 / 2
			case "oversized":
				r.childViews.maxBytes = cached.bytes / 2
			}
			// Re-admit the first checkpoint under the configured limits, too.
			r.childViews.take("progress:" + id)
			r.workspace().viewState(r.workspace().inspector.target).agents.progress[id].at = time.Time{}
			// Include ordinary visited child projections in the same budget.
			r.childViews.put(&cachedChildView{key: "other", info: &sessions.SessionView{ID: "other"}, model: newReplModel(), used: r.childViews.visit(), bytes: cached.bytes})
			for i := 1; i < 6; i++ {
				addProgressWorker(t, store, root, fmt.Sprintf("worker-%d", i))
			}
			list := waitAllAgentProgress(t, r)
			if len(r.childViews.entries) > r.childViews.maxEntries || r.childViews.bytes > r.childViews.maxBytes {
				t.Fatalf("progress bypassed shared bounds: entries=%d bytes=%d", len(r.childViews.entries), r.childViews.bytes)
			}
			for id, p := range list.progress {
				if got := progressText(p.fields); got != "progress" {
					t.Fatalf("eviction lost lightweight progress for %s: %q", id, got)
				}
			}
			if bound == "oversized" && len(r.childViews.entries) != 0 {
				t.Fatal("oversized checkpoints were retained")
			}
		})
	}
}

func TestAgentProgressReusesOnlyCachedCheckpoints(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	id := addProgressWorker(t, store, r.visibleTab(), "worker")
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
	r.state.sessionStore = counted
	r.openAgentsInspector()
	list := waitAllAgentProgress(t, r)
	base := r.childViews.entries["progress:"+id].model
	histories := counted.histories.Load()
	list.progress[id].at = time.Time{}
	waitAllAgentProgress(t, r)
	if r.childViews.entries["progress:"+id].model != base || counted.histories.Load() != histories {
		t.Fatal("unchanged cached checkpoint reread or rehydrated history")
	}
	r.childViews.take("progress:" + id)
	list.progress[id].at = time.Time{}
	waitAllAgentProgress(t, r)
	if r.childViews.entries["progress:"+id].model == base || counted.histories.Load() != 2*histories || progressText(list.progress[id].fields) != "progress" {
		t.Fatal("cache eviction did not rebuild the checkpoint and preserve progress")
	}
	r.visibleTab().swarmSnapshot = nil
	r.refreshInspector(160)
	if list.progress != nil || r.childViews.entries["progress:"+id] != nil {
		t.Fatal("unavailable swarm retained progress checkpoints")
	}
}

func TestAgentProgressReleasesInactiveList(t *testing.T) {
	for _, leave := range []string{"close", "agent", "history", "workspace"} {
		t.Run(leave, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "other", "root")
			root := r.visibleTab()
			id := addProgressWorker(t, store, root, "worker")
			r.inspect(tabViewTarget(root)) // give history a previous destination
			r.openAgentsInspector()
			list := waitAllAgentProgress(t, r)
			list.selected = id
			if r.childViews.entries["progress:"+id] == nil {
				t.Fatal("busy agent did not retain its checkpoint")
			}
			switch leave {
			case "close":
				r.closeInspector()
			case "agent":
				r.agentsInspectorAction("agents-open:" + id)
			case "history":
				r.inspectorHistory(-1)
			case "workspace":
				r.showTab(0)
			}
			root.swarmSnapshot.Executions[root.swarmSnapshot.Members[id].Execution].Status = "completed"
			if list.progress != nil || r.childViews.entries["progress:"+id] != nil {
				t.Fatal("inactive list retained progress projections")
			}
			if list.selected != id || len(list.order) != 1 {
				t.Fatal("retiring projections discarded list navigation")
			}
		})
	}
}

func TestAgentProgressRejectsRetiredCallbacks(t *testing.T) {
	for _, change := range []string{"close", "agent", "reopen", "completed", "removed", "resize", "done"} {
		t.Run(change, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "root")
			root := r.visibleTab()
			id := addProgressWorker(t, store, root, "worker")
			gate := make(chan struct{})
			r.state.sessionStore = &gatedChildViewStore{SessionStore: store, reader: store.(sessions.ViewStore), gate: gate}
			r.openAgentsInspector()
			r.refreshInspector(160)
			list := r.workspace().viewState(r.workspace().inspector.target).agents
			p := list.progress[id]
			if p == nil || !p.loading {
				t.Fatal("progress projection did not start")
			}
			switch change {
			case "close", "reopen":
				r.closeInspector()
				if change == "reopen" {
					r.openAgentsInspector()
					// Simulate a newly scheduled entry before the old callback lands.
					list.progress = map[string]*agentProgress{id: {loading: true}}
				}
			case "agent":
				r.agentsInspectorAction("agents-open:" + id)
			case "completed":
				root.swarmSnapshot.Executions[root.swarmSnapshot.Members[id].Execution].Status = "completed"
			case "removed":
				delete(root.swarmSnapshot.Members, id)
				r.refreshInspector(160)
			case "resize":
				r.refreshAgentsInspector(viewGeometry{width: 36})
			case "done":
				member := root.swarmSnapshot.Members[id]
				member.Task = "task"
				root.swarmSnapshot.Tasks = map[string]*swarm.Task{"task": {ID: "task", Owner: id, Status: "done"}}
				root.swarmSnapshot.Executions[member.Execution].Status = "completed"
				r.refreshInspector(160)
			}
			close(gate)
			waitProgressTask(t, r)
			if p.key != "" || len(p.fields) > 0 || r.childViews.entries["progress:"+id] != nil {
				t.Fatal("stale callback repopulated retired progress")
			}
			if change == "reopen" && !list.progress[id].loading {
				t.Fatal("stale callback modified the replacement entry")
			}
		})
	}
}

func TestAgentProgressDropsCompletedCheckpointBeforeRetry(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	root := r.visibleTab()
	id := addProgressWorker(t, store, root, "worker")
	r.openAgentsInspector()
	list := waitAllAgentProgress(t, r)
	if r.childViews.entries["progress:"+id] == nil {
		t.Fatal("missing busy checkpoint")
	}
	list.progress[id].failures = 1
	list.progress[id].retryAt = time.Now().Add(time.Hour)
	root.swarmSnapshot.Executions[root.swarmSnapshot.Members[id].Execution].Status = "completed"
	r.refreshInspector(160)
	for key := range r.childViews.entries {
		if strings.HasPrefix(key, "progress:") {
			t.Fatal("completed member retained a checkpoint during retry backoff")
		}
	}
}
