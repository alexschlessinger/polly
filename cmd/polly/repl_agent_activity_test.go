package main

import (
	"context"
	"image"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentActivityShowsOneLineWithoutOpeningChild(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	root := r.visibleTab()
	s := root.swarmSnapshot
	m := s.Members[ids[1]]
	e := s.Executions[m.Execution]
	e.Generation = 2
	now := time.Now()
	a := swarm.LiveActivity{Execution: e.ID, Generation: 2, Phase: "thinking", Since: now.Add(-6*time.Minute - 14*time.Second), RequestStarted: now.Add(-6*time.Minute - 14*time.Second), LastData: now.Add(-time.Second), RequestActive: true, Attempt: 1, StallTimeout: 30 * time.Minute, Deadline: 2 * time.Hour, StreamedBytes: 16000}
	root.swarmActivities = map[string]swarm.LiveActivity{m.ID: a}
	if status, warn := agentRowStatus(s, m, a, now); status != "thinking   6m" || warn {
		t.Fatal(status, warn)
	}
	r.openAgentsInspector()
	v := waitInspector(t, r, 160)
	if got := r.agentsInspectorEntries(80)[m.ID].status; got != "thinking   6m" {
		t.Fatalf("list status %q", got)
	}
	// Names of different lengths still start their statuses in one column.
	columns := map[string]int{}
	for _, line := range strings.Split(inspectorText(v), "\n") {
		for _, status := range []string{"approval", "thinking   6m"} {
			if i := strings.Index(line, status); i >= 0 {
				columns[status] = utf8.RuneCountInString(line[:i])
			}
		}
	}
	if len(columns) != 2 || columns["approval"] != columns["thinking   6m"] {
		t.Fatalf("status columns %v:\n%s", columns, inspectorText(v))
	}
	r.inspectorAction("agents-open:" + m.ID)
	waitInspector(t, r, 160)
	for _, width := range []int{15, 36, 80} {
		header := r.inspectorHeader(width, 40, 0, 0)
		checkInspectorHeaderGeometry(t, header, image.Rect(0, 0, width, header.rows))
		text := plainStyledText(header.text)
		status, warn := agentRowStatus(s, m, a, now)
		if !strings.Contains(header.text, rowStatusMarkup(status, "muted", warn)) || strings.Contains(text, "timeout") || strings.Contains(text, "elapsed:") {
			t.Fatalf("header differs from inline status or duplicates its clock: %s", text)
		}
		// The status sits on the title row when a readable name fits beside
		// it, else on its own row; the saved word follows it whole.
		if inline := strings.HasSuffix(headerTitle(header.text), "  "+status+" · saved"); inline != (width >= 36) || !inline && !strings.Contains(text, "\n"+status+"\nsaved") {
			t.Fatalf("width %d: saved word split from the status: %q", width, text)
		}
		if !headerButton(header.buttons, "stop").Empty() || !headerButton(header.buttons, "resume").Empty() {
			t.Fatal("read-only view offered execution controls")
		}
	}
	// Another attempt warns the same clock in both views; inspection spells
	// out why below it. Wrapping must keep the clock's color and hitboxes.
	a.Attempt = 2
	root.swarmActivities[m.ID] = a
	for _, width := range []int{3, 10, 12, 15, 36, 80} {
		header := r.inspectorHeader(width, 80, 0, 0)
		checkInspectorHeaderGeometry(t, header, image.Rect(0, 0, width, header.rows))
		if !strings.Contains(header.text, style.Styled(" 6m", "active", "")) {
			t.Fatalf("width %d: header clock not warned: %s", width, header.text)
		}
		if width >= 15 {
			status, warn := agentRowStatus(s, m, a, now)
			if !strings.Contains(header.text, rowStatusMarkup(status, "muted", warn)) || !strings.Contains(plainStyledText(header.text)+"\n", "\nattempt 2\n") {
				t.Fatalf("request attempt missing from inspection: %s", header.text)
			}
		}
	}
	if status, warn := agentRowStatus(s, m, a, now); status != "thinking   6m" || !warn {
		t.Fatalf("compact list lost its warned clock: %q, warn %v", status, warn)
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
	s := r.visibleTab().displaySnapshot()
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
	for _, action := range []string{"parent", "stop"} {
		header := r.inspectorHeader(80, 30, 0, 0)
		if button := headerButton(header.buttons, action); button.Empty() || button.Min.Y != 0 {
			t.Fatalf("swarm action %s missing from title row: %s", action, header.text)
		}
	}
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
	if strings.Contains(plainStyledText(header.text), "Stopped by you") || !headerButton(header.buttons, "resume").Empty() {
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
	if got, _ := agentRowStatus(s, m, a, time.Now()); got != "" {
		t.Fatalf("stale generation: %s", got)
	}
	a.Generation = 2
	e.Status = "completed"
	if got, _ := agentRowStatus(s, m, a, time.Now()); got != "" {
		t.Fatalf("settled stream still live: %s", got)
	}
	m.Control = swarm.MemberControlStopped
	e.Status = "running"
	if got, _ := agentRowStatus(s, m, a, time.Now()); got != "Stopping · stopped by you" {
		t.Fatal(got)
	}
	e.Status = "paused"
	if got, _ := agentRowStatus(s, m, a, time.Now()); got != "Stopped by you" {
		t.Fatal(got)
	}
	e.Error = "context canceled"
	if reason := agentFailureReason(s, m); reason != "" {
		t.Fatalf("a stop by the user is not a failure: %q", reason)
	}
	m.Control = swarm.MemberControlEnabled
	if reason := agentFailureReason(s, m); reason != "reason: context canceled" {
		t.Fatalf("paused execution lost its error: %q", reason)
	}
}

func TestAgentActivityLineNamesAttemptsAndNearLimits(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	s := r.visibleTab().swarmSnapshot
	m := s.Members[ids[1]]
	e := s.Executions[m.Execution]
	e.Status = "running"
	now := time.Now()
	started := now.Add(-2 * time.Second)
	a := swarm.LiveActivity{Execution: e.ID, Generation: e.Generation, Phase: "requesting", Since: started, RequestStarted: started, RequestActive: true, Attempt: 1, StallTimeout: 30 * time.Minute, Deadline: 2 * time.Hour}
	for _, step := range []struct {
		apply       func()
		row, extras string
	}{
		{func() {}, "querying   2s", ""},
		{func() { a.Attempt = 2 }, "querying   2s", "attempt 2"},
		{func() {
			a.Since, a.RequestStarted = now.Add(-29*time.Minute-59*time.Second-400*time.Millisecond), now.Add(-29*time.Minute-59*time.Second-400*time.Millisecond)
		}, "querying  29m", "attempt 2 · timeout in 1s"},
		{func() {
			a.Since, a.RequestStarted = now.Add(-29*time.Minute-10*time.Second), now.Add(-29*time.Minute-10*time.Second)
		}, "querying  29m", "attempt 2 · timeout in 50s"},
		{func() { a.Phase, a.LastData = "thinking", now.Add(-time.Second) }, "thinking  29m", "attempt 2"},
		{func() { a.Attempt = 1 }, "thinking  29m", ""},
		// The deadline is sooner than a fresh silence window once the
		// request has run most of its two hours.
		{func() { a.RequestStarted = now.Add(-110 * time.Minute) }, "thinking  29m", "timeout in 10m00s"},
		{func() { a.StallTimeout, a.Deadline = 0, 0 }, "thinking  29m", ""},
		{func() { a.Phase, a.Since, a.RequestActive = "tools", now.Add(-12*time.Second), false }, "toolcall  12s", ""},
		{func() { a.Since = now.Add(-3 * time.Hour) }, "toolcall   3h", ""},
		{func() { e.Status = "waiting" }, "parked     3h", ""},
	} {
		step.apply()
		row, warn := agentRowStatus(s, m, a, now)
		if extras := agentLiveExtras(a, now); row != step.row || extras != step.extras || warn != (extras != "") {
			t.Fatalf("row %q warn %v extras %q, want %q %q", row, warn, extras, step.row, step.extras)
		}
	}
}

func TestAgentActivityRowWarnsClockAndHoldsCountsUntilSettled(t *testing.T) {
	a := &agentActivity{label: "Builder", live: "waiting   29m", liveWarn: true, state: swarm.AgentPresentation{Lifecycle: swarm.LifecycleActive, Busy: true}}
	line := agentActivityLine(a, 12)
	if plain := plainStyledText(line); plain != "  Builder       waiting   29m" {
		t.Fatalf("live row %q", plain)
	}
	if !strings.Contains(line, style.Styled("29m", "active", "")) {
		t.Fatalf("clock not warned: %s", line)
	}
	a.approval = true
	if line := agentActivityLine(a, 12); strings.Contains(line, style.Styled("29m", "active", "")) || !strings.Contains(line, "approval needed") {
		t.Fatalf("approval row colored a clock: %s", line)
	}
	a.approval, a.live, a.liveWarn = false, "", false
	a.state = localAgentState("done", false)
	if plain := plainStyledText(agentActivityLine(a, 12)); plain != "✓ Builder       done" {
		t.Fatalf("settled row %q", plain)
	}
}
