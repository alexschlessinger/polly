package main

import (
	"context"
	"image"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentActivityShowsStreamAndTimeoutsWithoutOpeningChild(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	root := r.visibleTab()
	s := root.swarmSnapshot
	m := s.Members[ids[1]]
	e := s.Executions[m.Execution]
	e.Generation = 2
	now := time.Now()
	e.ResumedAt, e.ResumedBy = now.Add(-7*time.Minute), "parent"
	a := swarm.LiveActivity{Execution: e.ID, Generation: 2, Phase: "thinking", Since: now.Add(-6*time.Minute - 14*time.Second), RequestStarted: now.Add(-6*time.Minute - 14*time.Second), LastData: now.Add(-time.Second), RequestActive: true, Attempt: 1, StallTimeout: 30 * time.Minute, Deadline: 2 * time.Hour, StreamedBytes: 16000}
	root.swarmActivities = map[string]swarm.LiveActivity{m.ID: a}
	status := agentLiveSummary(s, m, a, now)
	if status != "Thinking · 6m14s · data 1s ago" {
		t.Fatal(status)
	}
	details := strings.Join(agentActivityDetails(s, m, a, now), "\n")
	for _, want := range []string{"Model request · 6m14s", "≈4.0k output tokens", "Silence timeout · 30m00s (29m59s left)", "Request deadline · 2h00m00s (1h53m46s left)", "Resumed by parent at"} {
		if !strings.Contains(details, want) {
			t.Fatalf("missing %q: %s", want, details)
		}
	}
	r.openAgentsInspector()
	waitInspector(t, r, 160)
	if !strings.Contains(r.agentsInspectorEntries()[m.ID].status, "Thinking") {
		t.Fatal("list lacks live phase")
	}
	r.inspectorAction("agents-open:" + m.ID)
	waitInspector(t, r, 160)
	for _, width := range []int{36, 80} {
		header := r.inspectorHeader(width, 40, 0, 0)
		checkInspectorHeaderGeometry(t, header, image.Rect(0, 0, width, header.rows))
		text := plainStyledText(header.text)
		if !strings.Contains(text, "Thinking") || !strings.Contains(text, "Silence timeout") || !strings.Contains(text, "Request deadline") {
			t.Fatalf("header lacks live status: %s", text)
		}
		if !headerButton(header.buttons, "stop").Empty() || !headerButton(header.buttons, "resume").Empty() {
			t.Fatal("read-only view offered execution controls")
		}
	}
	if sessionInUse(t, root.state.sessionStore, m.Name) {
		t.Fatal("activity display acquired child lease")
	}
}
func TestInspectorUserStopAndResume(t *testing.T) {
	var calls atomic.Int32
	started, resumed := make(chan struct{}), make(chan struct{})
	r := newSwarmTestREPL(t, integrationModel(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
		} else {
			close(resumed)
		}
		return spawnTestReply("done")
	}), nil)
	r.runTabCommand("/spawn --read-only inspect")
	runUITask(t, r)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}
	refreshPickerSwarm(t, r)
	s := r.visibleTab().swarmSnapshot
	var m *swarm.Member
	for _, member := range s.Members {
		m = member
	}
	if m == nil {
		t.Fatal("missing member")
	}
	target := viewTarget{session: sessions.ViewTarget{ID: m.ID, Name: m.Name}}
	r.inspect(target)
	waitInspector(t, r, 140)
	r.inspectorAction("stop")
	r.applyTabRequests()
	waitSwarmIdle(t, r.state.swarm)
	refreshPickerSwarm(t, r)
	header := r.inspectorHeader(80, 30, 0, 0)
	if headerButton(header.buttons, "resume").Empty() || !headerButton(header.buttons, "stop").Empty() || !strings.Contains(plainStyledText(header.text), "Stopped by you") {
		t.Fatalf("stopped controls: %s", header.text)
	}
	r.inspectorAction("resume")
	r.applyTabRequests()
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("Resume agent did not run the child")
	}
	waitSwarmIdle(t, r.state.swarm)
	refreshPickerSwarm(t, r)
	header = r.inspectorHeader(80, 30, 0, 0)
	if !strings.Contains(plainStyledText(header.text), "Resumed by you") || !headerButton(header.buttons, "resume").Empty() {
		t.Fatalf("resumed controls: %s", header.text)
	}
	if len(r.tabs) != 1 {
		t.Fatal("resuming created an inspection runtime")
	}
}

func TestAgentActivityFencesGenerationAndSettlement(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	s := r.visibleTab().swarmSnapshot
	m := s.Members[ids[1]]
	e := s.Executions[m.Execution]
	e.Generation = 2
	a := swarm.LiveActivity{Execution: e.ID, Generation: 1, Phase: "thinking", RequestActive: true}
	if got := agentLiveSummary(s, m, a, time.Now()); got != "" {
		t.Fatalf("stale generation: %s", got)
	}
	a.Generation = 2
	e.Status = "completed"
	if got := agentLiveSummary(s, m, a, time.Now()); got != "" {
		t.Fatalf("settled stream still live: %s", got)
	}
	m.Control = swarm.MemberControlStopped
	e.Status = "running"
	if got := agentLiveSummary(s, m, a, time.Now()); got != "Stopping · stopped by you" {
		t.Fatal(got)
	}
	e.Status = "paused"
	if got := agentLiveSummary(s, m, a, time.Now()); got != "Stopped by you" {
		t.Fatal(got)
	}
	if details := strings.Join(agentActivityDetails(s, m, a, time.Now()), " "); !strings.Contains(details, "Only you can resume") || strings.Contains(details, "Silence timeout") {
		t.Fatal(details)
	}
	m.Control = swarm.MemberControlEnabled
	e.Error = "context canceled"
	if details := strings.Join(agentActivityDetails(s, m, a, time.Now()), " "); !strings.Contains(details, "Reason: context canceled") || strings.Contains(details, "Only you") {
		t.Fatal(details)
	}
	e.Status = "running"
	a.RequestStarted, a.Since = time.Now(), time.Now()
	if details := strings.Join(agentActivityDetails(s, m, a, time.Now()), " "); !strings.Contains(details, "no data yet") || !strings.Contains(details, "Silence timeout · off Request deadline · off") {
		t.Fatal(details)
	}
}
