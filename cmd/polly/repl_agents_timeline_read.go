package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

// Saved phases: every reply and tool result carries when it started and how
// long it took, so a member's transcript can be drawn after a restart. The
// list reads each member's new messages in the background, at most once a
// second, and only while lanes are shown. Saved spans outrank observed
// query and tool spans for the time they cover; approval waits and parking
// are never saved, so the observed ones stay on top.

// agentTimelineReadEvery spaces background transcript reads.
const agentTimelineReadEvery = time.Second

// agentTranscriptCursor is how far one member's transcript has been read.
type agentTranscriptCursor struct {
	epoch string
	next  int64
	// key is the execution state the last read saw; a finished member is
	// read once more after it changes, then left alone.
	key string
	// calls are tool calls whose results have not been read yet.
	calls map[string]messages.ChatMessageToolCall
}

type agentTranscriptRead struct {
	cursor agentTranscriptCursor
	spans  []agentSpan
	// reset replaces the member's saved spans: its transcript was replaced.
	reset bool
	err   error
}

// savedSpans turns stamped messages into spans: a reply is a query over its
// request, a tool result a tool span over its run. Unstamped messages, from
// before the stamps existed, contribute nothing.
func savedSpans(msgs []messages.ChatMessage, calls map[string]messages.ChatMessageToolCall) []agentSpan {
	var out []agentSpan
	for i := range msgs {
		msg := &msgs[i]
		switch msg.Role {
		case messages.MessageRoleAssistant:
			for _, call := range msg.ToolCalls {
				if call.ID != "" {
					calls[call.ID] = call
				}
			}
			if start := msg.StartedAt(); !start.IsZero() {
				out = append(out, agentSpan{start: start, end: start.Add(msg.RequestDuration()), phase: phaseQuery})
			}
		case messages.MessageRoleTool:
			call, ok := calls[msg.ToolCallID]
			if !ok {
				call = messages.ChatMessageToolCall{ID: msg.ToolCallID, Name: msg.ToolName}
			}
			delete(calls, msg.ToolCallID)
			if start := msg.StartedAt(); !start.IsZero() {
				out = append(out, agentSpan{start: start, end: start.Add(msg.ToolDuration()), phase: phaseTool, call: call})
			}
		}
	}
	return out
}

// readAgentTranscript reads one member's messages after its cursor. A
// replaced transcript starts over from the beginning.
func readAgentTranscript(ctx context.Context, pages sessions.TranscriptPageStore, id string, c agentTranscriptCursor) agentTranscriptRead {
	out := agentTranscriptRead{cursor: c}
	if out.cursor.calls == nil {
		out.cursor.calls = map[string]messages.ChatMessageToolCall{}
	}
	q := sessions.TranscriptQuery{Epoch: c.epoch, After: c.next, Limit: 256}
	for {
		page, err := pages.ReadTranscriptPage(ctx, sessions.ViewTarget{ID: id}, q)
		if errors.Is(err, sessions.ErrStaleTranscriptEpoch) || errors.Is(err, sessions.ErrInvalidTranscriptRange) && q.Epoch != "" {
			if out.reset {
				out.err = err
				return out
			}
			out = agentTranscriptRead{cursor: agentTranscriptCursor{key: c.key, calls: map[string]messages.ChatMessageToolCall{}}, reset: true}
			q = sessions.TranscriptQuery{Limit: 256}
			continue
		}
		if err != nil {
			out.err = err
			return out
		}
		out.spans = append(out.spans, savedSpans(page.Messages, out.cursor.calls)...)
		out.cursor.epoch, out.cursor.next = page.Epoch, page.Next
		if page.Next >= page.Through || len(page.Messages) == 0 {
			return out
		}
		q = sessions.TranscriptQuery{Epoch: page.Epoch, After: page.Next, Through: page.Through, Limit: 256}
	}
}

// agentReadKey is the execution state that decides whether a settled member
// needs another read.
func agentReadKey(s *swarm.DisplaySnapshot, m *swarm.Member) string {
	e := s.Executions[m.Execution]
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s/%d/%s/%d", e.ID, e.Generation, e.Status, e.Ended.UnixMilli())
}

// readAgentTimelines starts a background read of the members whose
// transcripts may have grown: busy ones each time, settled ones once after
// their execution changes.
func (r *managedREPL) readAgentTimelines(root *replTab, snapshot *swarm.DisplaySnapshot) {
	t := &root.agentTimeline
	if snapshot == nil || t.reading || time.Now().Before(t.readAt) || r.state == nil {
		return
	}
	// Only native paging: the legacy adapter reads a whole view per page,
	// too much to repeat every second for every busy member.
	access, ok := r.state.sessionStore.(sessions.TranscriptPageStore)
	if !ok {
		return
	}
	want := map[string]agentTranscriptCursor{}
	for id, m := range snapshot.Members {
		key := agentReadKey(snapshot, m)
		if key == "" {
			continue
		}
		c, seen := t.cursors[id]
		e := snapshot.Executions[m.Execution]
		if seen && c.key == key && !executionBusy(e) {
			continue
		}
		c.key = key
		c.calls = maps.Clone(c.calls)
		want[id] = c
	}
	t.readAt = time.Now().Add(agentTimelineReadEvery)
	if len(want) == 0 {
		return
	}
	t.reading = true
	if !r.background(func() {
		results := make(map[string]agentTranscriptRead, len(want))
		for id, c := range want {
			results[id] = readAgentTranscript(r.work.ctx, access, id, c)
		}
		r.postUI(r.work.ctx, func() {
			t.reading = false
			if t.merge(results) {
				r.inspectorRefreshAt = time.Time{}
			}
		})
	}) {
		t.reading = false
	}
}

// merge lands background reads and reports whether any lane changed. A
// failed read keeps its old cursor and is tried again next time.
func (t *agentTimeline) merge(results map[string]agentTranscriptRead) bool {
	if t.saved == nil {
		t.saved, t.cursors = map[string][]agentSpan{}, map[string]agentTranscriptCursor{}
	}
	changed := false
	for id, read := range results {
		if read.err != nil {
			continue
		}
		t.cursors[id] = read.cursor
		if !read.reset && len(read.spans) == 0 {
			continue
		}
		spans := read.spans
		if !read.reset {
			spans = append(t.saved[id], spans...)
		}
		slices.SortStableFunc(spans, func(a, b agentSpan) int { return a.start.Compare(b.start) })
		if len(spans) > agentTimelineSpans {
			spans = slices.Delete(spans, 0, len(spans)-agentTimelineSpans)
		}
		t.saved[id] = spans
		changed = true
	}
	if changed {
		t.revision++
	}
	return changed
}

// laneSpans is one member's spans for a paint: the saved ones, then the
// observed ones. Observed query and tool spans the transcript already covers
// are dropped; those left are the not-yet-saved tail.
func (t *agentTimeline) laneSpans(id string, now time.Time) (spans []agentSpan, live bool) {
	saved := t.saved[id]
	var savedEnd time.Time
	for _, s := range saved {
		if s.end.After(savedEnd) {
			savedEnd = s.end
		}
	}
	spans = slices.Clone(saved)
	observed := t.spans[id]
	for n, s := range observed {
		if n == len(observed)-1 && t.open[id] {
			s.end, live = now, true
		}
		// A sample is taken after the change it sees, so an observed span
		// that starts before the transcript ends began inside saved time.
		if (s.phase == phaseQuery || s.phase == phaseTool) && s.start.Before(savedEnd) {
			continue
		}
		spans = append(spans, s)
	}
	slices.SortStableFunc(spans, func(a, b agentSpan) int { return a.start.Compare(b.start) })
	return spans, live
}
