package main

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func (r *managedREPL) inspectingAgent() bool {
	i := &r.workspace().inspector
	return i.open && i.target.kind == conversationViewKind && !r.targetsVisibleTab(i.target)
}

func (r *managedREPL) inspectorOverview() bool {
	return r.inspectingAgent() && !r.workspace().viewState(r.workspace().inspector.target).watch
}

func (r *managedREPL) watchInspectedAgent(watch bool) {
	if !r.inspectingAgent() {
		return
	}
	i := &r.workspace().inspector
	s := r.workspace().viewState(i.target)
	if s.watch == watch {
		return
	}
	if !watch && i.current != nil && i.current.model != nil {
		rememberViewPosition(i.current.model, s)
	}
	s.watch = watch
	if watch {
		if !s.watchInitialized {
			s.watchInitialized, s.disclosures, s.promptExpanded = true, disclosuresDetailed, true
			s.revision++
		}
		if v := i.current; v != nil && v.model != nil {
			// Apply choices to the visible copy while its full-width projection
			// is loading. Later messages inherit them through Project.
			v.model.setInitialPromptExpanded(s.promptExpanded)
			applyViewSections(v.model, *s)
			v.model.visual.invalidate()
		}
		s.watchWasMaximized, i.maximized = i.maximized, true
		s.restoreWatchAnchor = true
	} else {
		i.maximized = s.watchWasMaximized
	}
	i.searching, i.keyboardAction = false, ""
}

// Completion is a fact about the execution, not a guess based on its last
// message. The header separately describes delivery, review and integration.
func (r *managedREPL) inspectedAgentCompleted() bool {
	i := &r.workspace().inspector
	owner := r.rootTab(r.visibleTab())
	if snapshot := owner.swarmSnapshot; snapshot != nil {
		if member := snapshot.Members[i.target.session.ID]; member != nil {
			e := snapshot.Executions[member.Execution]
			return e != nil && e.Status == "completed"
		}
	}
	if tab := r.inspectionTab(i.target); tab != nil {
		tab.model.mu.Lock()
		defer tab.model.mu.Unlock()
		return !tab.model.busy && tab.model.lastOutcome == turnOutcomeDone
	}
	if v := i.current; v != nil && v.info != nil && v.info.Metadata != nil {
		return v.info.Metadata.SpawnOutcome == "finished"
	}
	return false
}

func (r *managedREPL) agentOverviewState() agentOverviewState {
	i := &r.workspace().inspector
	state := r.workspace().viewState(i.target).overview
	state.completed = r.inspectedAgentCompleted()
	owner := r.rootTab(r.visibleTab())
	if snapshot := owner.swarmSnapshot; snapshot != nil {
		if member := snapshot.Members[i.target.session.ID]; member != nil {
			if execution := snapshot.Executions[member.Execution]; execution != nil {
				state.assignment = execution.Request.Task
				if v := i.current; v != nil {
					state.currentTranscript = v.live.identity == (inspectorLiveIdentity{execution.ID, execution.Generation})
				}
			}
			activity := owner.swarmActivities[member.ID]
			if currentAgentActivity(snapshot, member, activity) {
				state.liveExecution = fmt.Sprintf("%s:%d", activity.Execution, activity.Generation)
				state.liveTools = activity.Tools
				state.toolStarted = make(map[string]time.Time, len(activity.Tools))
				for _, tool := range activity.Tools {
					if tool.Call.DisplayID != "" {
						state.toolStarted[tool.Call.DisplayID] = tool.Started
					}
				}
				if v := i.current; v != nil && v.model != nil {
					state.liveTools = agentOverviewUnrecordedLiveTools(v.model, state.liveTools)
				}
			}
		}
	}
	return state
}

func (r *managedREPL) renderInspectorOverview(width, height, x, y int) {
	i := &r.workspace().inspector
	s := r.workspace().viewState(i.target)
	body := agentOverviewRender{rows: style.VisualRows("Loading…", ui.StyleClear, width)}
	if v := i.current; v != nil && v.model != nil {
		if v.unavailable || v.failures > 0 {
			body.rows = v.view.Rows(v.model, width)
		} else {
			state := r.agentOverviewState()
			body = renderAgentOverviewCached(v.model, width, state, &v.overviewCache)
		}
	}
	s.overviewTop = max(0, min(s.overviewTop, len(body.rows)-height))
	r.inspectorW.Rows, r.inspectorW.TopRow, r.inspectorW.PinBottom = body.rows, s.overviewTop, false
	r.inspectorW.OverlayBottom = nil
	for row, action := range body.actions {
		if action == "" || row < s.overviewTop || row >= s.overviewTop+height {
			continue
		}
		at := y + row - s.overviewTop
		r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, at, x+width, at+1), action})
	}
}

func (r *managedREPL) toggleOverviewDetails() {
	i := &r.workspace().inspector
	if i.current == nil || i.current.model == nil {
		return
	}
	s := r.workspace().viewState(i.target)
	s.overview.disclosures = s.overview.disclosures.next()
	expand := s.overview.disclosures >= disclosuresOpen
	s.overview.assignmentExpanded, s.overview.updateExpanded = expand, expand
	// The selected level now owns every row, including tools arriving later.
	clear(s.overview.tools)
}

func (r *managedREPL) inspectorOverviewAction(action string) bool {
	if !strings.HasPrefix(action, "overview:") || !r.inspectorOverview() {
		return false
	}
	i := &r.workspace().inspector
	s := r.workspace().viewState(i.target)
	switch action {
	case "overview:assignment":
		s.overview.assignmentExpanded = !s.overview.assignmentExpanded
	case "overview:update":
		s.overview.updateExpanded = !s.overview.updateExpanded
	default:
		if key, ok := strings.CutPrefix(action, "overview:tool:"); ok && i.current != nil && i.current.model != nil {
			if s.overview.tools == nil {
				s.overview.tools = make(map[string]bool)
			}
			state := r.agentOverviewState()
			s.overview.tools[key] = !agentOverviewToolExpanded(i.current.model, key, state)
		}
	}
	return true
}
