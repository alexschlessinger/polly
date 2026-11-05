package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

// agentProgress is one listed agent's progress line: the activity line of
// its latest turn, projected from its saved checkpoint plus the live tail and
// running calls its runtime reports, as its own view would show it. The
// projection runs off the loop; the list paints whatever has landed.
type agentProgress struct {
	fields []turnDockField
	width  int // pane width used to fit the latest call
	// fence names the swarm facts the fields were computed under; key the
	// checkpoint, live tail and running calls they were projected from.
	fence, key string
	// Full checkpoints live only in the shared, bounded childViews cache,
	// never in this navigation state.
	at       time.Time
	loading  bool
	failures int
	retryAt  time.Time
}

const (
	agentProgressLoads = 4  // projections in flight at once
	agentProgressWidth = 80 // checkpoint projection width; call labels fit the pane separately
)

// agentProgressFence changes when the agent's execution, its task or the
// calls its runtime reports change; a busy agent re-reads on a clock besides.
func agentProgressFence(s *swarm.State, m *swarm.Member, a swarm.LiveActivity) string {
	fence := m.ID
	if e := s.Executions[m.Execution]; e != nil {
		fence += fmt.Sprintf(":%s:%d:%s", e.ID, e.Generation, e.Status)
	}
	if task := s.Tasks[m.Task]; task != nil {
		fence += fmt.Sprintf(":%s:%d", task.Status, task.Revision)
	}
	if currentAgentActivity(s, m, a) {
		fence += ":" + liveToolsKey(a.Tools)
	}
	return fence
}

// retireAgentProgress releases checkpoints when the list leaves the screen.
// Dropping its entries also fences off callbacks from retired projections.
func (r *managedREPL) retireAgentProgress(list *agentsInspectorState) {
	if list == nil {
		return
	}
	for id := range list.progress {
		r.childViews.take("progress:" + id)
	}
	list.progress = nil
}

// refreshAgentProgress schedules the projections the list is missing, a few
// at a time and in list order, so the agents needing attention and the
// running ones fill in first. Listing never acquires a session lease.
func (r *managedREPL) refreshAgentProgress(list *agentsInspectorState, width int) {
	list.width = width
	root := r.rootTab(r.visibleTab())
	snapshot := root.swarmSnapshot
	if snapshot == nil || r.state == nil {
		r.retireAgentProgress(list)
		return
	}
	reader, ok := r.state.sessionStore.(sessions.ViewStore)
	if !ok {
		r.retireAgentProgress(list)
		return
	}
	if list.progress == nil {
		list.progress = map[string]*agentProgress{}
	}
	for id := range list.progress {
		member := snapshot.Members[id]
		visible := list.visible(id)
		if member == nil || !visible || !agentExecutionBusy(snapshot, member) {
			r.childViews.take("progress:" + id)
		}
		if member == nil || !visible {
			delete(list.progress, id)
		}
	}
	inFlight := 0
	for _, p := range list.progress {
		if p.loading {
			inFlight++
		}
	}
	now := time.Now()
	for _, id := range list.order {
		member := snapshot.Members[id]
		if member == nil || !list.visible(id) {
			continue
		}
		p := list.progress[id]
		if p == nil {
			p = &agentProgress{}
			list.progress[id] = p
		}
		busy := agentExecutionBusy(snapshot, member)
		fence := agentProgressFence(snapshot, member, root.swarmActivities[id])
		current := p.width == width && p.fence == fence && (!busy || now.Sub(p.at) < time.Second)
		if p.loading || current || p.failures > 0 && now.Before(p.retryAt) {
			continue
		}
		if inFlight >= agentProgressLoads {
			break
		}
		inFlight++
		r.loadAgentProgress(reader, list, id, p, fence, busy, width)
	}
}

func (r *managedREPL) loadAgentProgress(reader sessions.ViewStore, list *agentsInspectorState, id string, p *agentProgress, fence string, busy bool, width int) {
	target := viewTarget{session: sessions.ViewTarget{ID: id}}
	liveReader, identity := r.inspectorLiveSource(target)
	root := r.rootTab(r.visibleTab())
	var liveTools []swarm.LiveToolActivity
	if member := root.swarmSnapshot.Members[id]; member != nil && currentAgentActivity(root.swarmSnapshot, member, root.swarmActivities[id]) {
		liveTools = slices.Clone(root.swarmActivities[id].Tools)
	}
	// Everything the projection needs is captured here; p and the cache are
	// read and written on the loop only. Taking transfers the checkpoint to
	// this worker; eviction cannot leave a full model pinned in list state.
	cacheKey := "progress:" + id
	var saved *replModel
	var known string
	var size int64
	if cached := r.childViews.take(cacheKey); cached != nil {
		saved, known, size = cached.model, cached.info.Revision, cached.bytes
	}
	previousKey, fields := p.key, p.fields
	cfg := r.config
	p.loading = true
	if !r.background(func() {
		info, transcript, status, err := readInspectorConversation(r.work.ctx, reader, target.session, known, liveReader, identity)
		key := previousKey
		var cached *cachedChildView
		if err == nil {
			if !info.Unchanged {
				saved = prepareChildDisplay(info, cfg, agentProgressWidth)
				resolveToolBaseDir(r.work.ctx, reader, info, saved)
				size = 0
			}
			saved.artifactStore = info.Artifacts
			key = inspectorConversationRevision(info, transcript, status) + "|" + liveToolsKey(liveTools) + fmt.Sprintf("|width:%d", width)
			if key != previousKey && saved != nil {
				// The retained checkpoint stays immutable: tails and running
				// calls land on a copy.
				model := saved
				if status.active {
					model = prepareLiveDisplay(saved, transcript.Messages)
				} else if len(liveTools) > 0 {
					model = childDisplayCopy(saved)
				}
				model.adoptLiveTools(liveTools)
				fields = model.agentProgressFields(width)
			}
			saved.artifactStore = nil
			if busy {
				if size == 0 {
					size = childViewSize(saved)
				}
				cached = &cachedChildView{key: cacheKey, model: saved, info: &sessions.SessionView{ID: id, Revision: info.Revision}, bytes: size}
			}
		}
		r.postUI(r.work.ctx, func() {
			if list.progress[id] != p {
				return // the list was retired or this member was removed
			}
			p.loading = false
			snapshot := root.swarmSnapshot
			if list.width != width || snapshot == nil || snapshot.Members[id] == nil || agentProgressFence(snapshot, snapshot.Members[id], root.swarmActivities[id]) != fence {
				return // resize, completion or a new execution superseded the sample
			}
			p.at = time.Now()
			if err != nil {
				p.failures++
				p.retryAt = p.at.Add(time.Duration(min(p.failures, 5)) * 2 * time.Second)
				return
			}
			p.failures = 0
			p.fence, p.key, p.fields, p.width = fence, key, fields, width
			r.childViews.put(cached)
			// The list repaints with the landed line on its next refresh.
			r.inspectorRefreshAt = time.Time{}
		})
	}) {
		p.loading = false
	}
}

// agentProgressFields is the activity summary of the newest turn as the list
// shows it: the same fields and call fitting as a folded transcript expander.
// A turn that ran nothing shows the first line of its answer; a turn with
// neither shows nothing.
func (m *replModel) agentProgressFields(width int) []turnDockField {
	// A finished answer materializes its Markdown on its first paint.
	m.renderPendingMarkdown()
	start := m.newestTurnStart()
	var reasoningIDs, toolIDs []int64
	for i := start; i < len(m.transcript); i++ {
		if id := m.reasoningRecords.idAt(i); id != 0 {
			reasoningIDs = append(reasoningIDs, id)
		}
		if id := m.toolDisclosures.idAt(i); id != 0 {
			toolIDs = append(toolIDs, id)
		}
	}
	var fields []turnDockField
	if field, ok := m.inlineReasoningField(reasoningIDs); ok {
		fields = append(fields, field)
	}
	fields = append(fields, m.inlineToolFields(toolIDs, false, width-len(activityIndent))...)
	if field, ok := m.agentField(toolIDs, false); ok {
		fields = append(fields, field)
	}
	if field, _, ok := m.inlineImageField(toolIDs, false); ok {
		fields = append(fields, field)
	}
	if len(fields) > 0 {
		return fields
	}
	for i := len(m.transcript) - 1; i >= start; i-- {
		entry := m.transcript[i]
		if !entry.assistant {
			continue
		}
		for _, row := range style.VisualRows(entry.text, ui.StyleClear, 4*agentProgressWidth) {
			if text := strings.TrimSpace(ui.CellsToString(row)); text != "" {
				return []turnDockField{activityField(text, activityTools, false)}
			}
		}
	}
	return nil
}

// agentProgressRows uses the transcript's summary layout without a fold
// triangle: counts on one row, the latest call on its own row beneath them.
// Each row is a click target for the agent, and neither wraps.
func agentProgressRows(fields []turnDockField, width int) []string {
	if width < len(activityIndent)+4 {
		return nil
	}
	text, _, _ := renderActivityFields(fields, turnDockField{}, width, "")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
