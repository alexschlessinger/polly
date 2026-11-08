package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func stampedReply(start time.Time, took time.Duration, calls ...messages.ChatMessageToolCall) messages.ChatMessage {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "ok", ToolCalls: calls}
	msg.SetStartedAt(start)
	msg.SetRequestDuration(took)
	return msg
}

func stampedResult(id string, start time.Time, took time.Duration) messages.ChatMessage {
	msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "bash", Content: "done"}
	msg.SetStartedAt(start)
	msg.SetToolDuration(took)
	return msg
}

func TestSavedSpans(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	bash := messages.ChatMessageToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"go test ./..."}`}
	msgs := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		stampedReply(at(0), 4*time.Second, bash),
		{Role: messages.MessageRoleAssistant, Content: "unstamped, from before the stamps"},
	}
	calls := map[string]messages.ChatMessageToolCall{}
	spans := savedSpans(msgs, calls)
	if len(spans) != 1 || spans[0].phase != phaseQuery || !spans[0].start.Equal(at(0)) || !spans[0].end.Equal(at(4)) {
		t.Fatalf("reply spans %+v", spans)
	}
	// The result arrives in a later read; the pending call names it.
	spans = savedSpans([]messages.ChatMessage{stampedResult("c1", at(4), 6*time.Second)}, calls)
	if len(spans) != 1 || spans[0].phase != phaseTool || spans[0].call.Arguments != bash.Arguments || !spans[0].end.Equal(at(10)) {
		t.Fatalf("tool spans %+v", spans)
	}
	if len(calls) != 0 {
		t.Fatalf("a read result must leave the pending calls: %v", calls)
	}
}

// pagedTranscript serves one member's messages two at a time under an epoch
// the test can replace.
type pagedTranscript struct {
	epoch string
	msgs  []messages.ChatMessage
	reads int
}

func (p *pagedTranscript) ReadTranscriptPage(_ context.Context, _ sessions.ViewTarget, q sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
	p.reads++
	if q.Epoch != "" && q.Epoch != p.epoch {
		return sessions.TranscriptPage{}, sessions.ErrStaleTranscriptEpoch
	}
	through := q.Through
	if through == 0 {
		through = int64(len(p.msgs))
	}
	next := min(through, q.After+2)
	return sessions.TranscriptPage{Epoch: p.epoch, First: q.After, Next: next, Through: through, Messages: p.msgs[q.After:next]}, nil
}

func TestReadAgentTranscriptIncremental(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	bash := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	store := &pagedTranscript{epoch: "e1", msgs: []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		stampedReply(at(0), 2*time.Second, bash),
		stampedResult("c1", at(2), 3*time.Second),
	}}
	first := readAgentTranscript(t.Context(), store, "m", agentTranscriptCursor{})
	if first.err != nil || first.reset || len(first.spans) != 2 || first.cursor.next != 3 || store.reads != 2 {
		t.Fatalf("first read %+v after %d pages", first, store.reads)
	}

	store.msgs = append(store.msgs, stampedReply(at(5), time.Second))
	again := readAgentTranscript(t.Context(), store, "m", first.cursor)
	if again.err != nil || again.reset || len(again.spans) != 1 || again.spans[0].phase != phaseQuery || again.cursor.next != 4 {
		t.Fatalf("incremental read %+v", again)
	}

	// A replaced transcript is read again from the start.
	store.epoch, store.msgs = "e2", store.msgs[:2]
	replaced := readAgentTranscript(t.Context(), store, "m", again.cursor)
	if replaced.err != nil || !replaced.reset || len(replaced.spans) != 1 || replaced.cursor.epoch != "e2" || replaced.cursor.next != 2 {
		t.Fatalf("replaced read %+v", replaced)
	}
	if replaced.cursor.calls["c1"].Name != "bash" {
		t.Fatal("the unanswered call must wait for its result")
	}
}

func TestAgentTimelineMergesSavedAndObserved(t *testing.T) {
	t0 := time.Unix(5000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	var tl agentTimeline
	tl.spans, tl.open = map[string][]agentSpan{}, map[string]bool{}
	// Watched from 50s: querying, then a tool that asked for approval, then
	// a query the transcript has not caught up with.
	tl.record("live", phaseQuery, messages.ChatMessageToolCall{}, at(50))
	tl.record("live", phaseTool, messages.ChatMessageToolCall{ID: "c1", Name: "bash"}, at(60))
	tl.record("live", phaseWaiting, messages.ChatMessageToolCall{}, at(70))
	tl.record("live", phaseTool, messages.ChatMessageToolCall{ID: "c1", Name: "bash"}, at(80))
	tl.record("live", phaseQuery, messages.ChatMessageToolCall{}, at(90))
	bash := messages.ChatMessageToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"make"}`}
	changed := tl.merge(map[string]agentTranscriptRead{"live": {spans: []agentSpan{
		{start: at(20), end: at(30), phase: phaseQuery},
		{start: at(30), end: at(55), phase: phaseTool, call: bash},
		{start: at(55), end: at(60), phase: phaseQuery},
		{start: at(60), end: at(88), phase: phaseTool, call: bash},
	}}})
	if !changed || tl.revision == 0 {
		t.Fatal("saved spans must repaint the lanes")
	}
	spans, live := tl.laneSpans("live", at(100))
	if !live {
		t.Fatal("the open observed span is live")
	}
	var got []string
	for _, s := range spans {
		got = append(got, s.phase.word()+" "+axisLabel(s.start.Sub(t0))+"-"+axisLabel(s.end.Sub(t0)))
	}
	// Observed query and tool spans that start inside saved time are
	// dropped, the late-sampled end of the tool call too; the approval wait
	// stays, and so does the query the transcript has not caught up with.
	want := "querying 0:20-0:30|toolcall 0:30-0:55|querying 0:55-1:00|toolcall 1:00-1:28|approval 1:10-1:20|querying 1:30-1:40"
	if strings.Join(got, "|") != want {
		t.Fatalf("lane spans\n got %s\nwant %s", strings.Join(got, "|"), want)
	}

	lanes := buildAgentLanes(timelineState(t0), &tl, []string{"old", "live"}, nil, at(100))
	lanes.cols = 10
	// The approval wait covers the column from 70s; the tool span around it
	// covers more, but the wait is what the agent was doing.
	if phase := lanes.cellPhase("live", 7); phase != phaseWaiting {
		t.Fatalf("column 7 shows %v, want the approval wait", phase.word())
	}
	if phase := lanes.cellPhase("live", 3); phase != phaseTool {
		t.Fatalf("column 3 shows %v, want the saved tool call", phase.word())
	}
	lanes.scrub, lanes.cursor = true, at(40)
	if row := strings.TrimSpace(plainStyledText(lanes.scrubRow("live", 60))); row != "■ $ make  0:10 in" {
		t.Fatalf("scrub row %q", row)
	}
}

// TestAgentsInspectorReadsSavedPhases: a member this process never watched
// gets its phases from its transcript.
func TestAgentsInspectorReadsSavedPhases(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	finished := ids[2]
	s := r.visibleTab().swarmSnapshot
	start := time.Now().Add(-90 * time.Second).Truncate(time.Millisecond)
	for _, id := range ids {
		s.Executions[id].Started = start
	}
	s.Executions[finished].Ended = start.Add(60 * time.Second)
	bash := messages.ChatMessageToolCall{ID: "c1", Name: "bash", Arguments: `{"command":"go vet ./..."}`}
	session, err := r.state.sessionStore.Acquire(t.Context(), "Finished", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessages(t.Context(), []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "check"},
		stampedReply(start, 5*time.Second, bash),
		stampedResult("c1", start.Add(5*time.Second), 40*time.Second),
		stampedReply(start.Add(45*time.Second), 10*time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	r.openAttention()
	waitInspector(t, r, 160)
	root := r.rootTab(r.visibleTab())
	deadline := time.After(5 * time.Second)
	for len(root.agentTimeline.saved[finished]) < 3 {
		select {
		case fn := <-r.uiTasks:
			fn()
		case <-deadline:
			t.Fatalf("saved spans never landed: %+v", root.agentTimeline.saved)
		}
	}
	if !r.inspectorRefreshAt.IsZero() {
		t.Fatal("landed spans must ask for a repaint")
	}
	r.toggleAgentTimeline(keyContext{})
	r.agentsList().cursor = start.Add(20 * time.Second)
	r.inspectorRefreshAt = time.Time{}
	v := waitInspector(t, r, 160)
	if text := inspectorText(v); !strings.Contains(text, "$ go vet ./...  0:15 in") {
		t.Fatalf("the scrub row does not name the saved tool call:\n%s", text)
	}

	// A settled member is not read again until its execution changes.
	session, err = r.state.sessionStore.Acquire(t.Context(), "Finished", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(t.Context(), stampedReply(start.Add(70*time.Second), time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	settle := func() int64 {
		t.Helper()
		root.agentTimeline.readAt = time.Time{}
		r.readAgentTimelines(root, root.displaySnapshot())
		deadline := time.After(5 * time.Second)
		for root.agentTimeline.reading {
			select {
			case fn := <-r.uiTasks:
				fn()
			case <-deadline:
				t.Fatal("read never landed")
			}
		}
		return root.agentTimeline.cursors[finished].next
	}
	if next := settle(); next != 4 {
		t.Fatalf("settled member reread: cursor at %d", next)
	}
	s.Executions[finished].Generation++
	if next := settle(); next != 5 {
		t.Fatalf("a changed execution must be read again: cursor at %d", next)
	}
}
