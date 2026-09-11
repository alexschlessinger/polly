package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/workflow"
)

type pickerWorkflowHistory struct {
	id, name, status string
	finished         time.Time
	deferred         int
	items            []replModalItem
}

// Group the existing session rows; no agent runtime or transcript is opened.
// Stable synthetic identities keep expansion independent of renamed sessions.
func (r *managedREPL) agentHistoryItems(p *sessionsPicker, items []replModalItem) []replModalItem {
	groups := map[string]map[string]*pickerWorkflowHistory{}
	historical := map[string]bool{}
	priorities := map[string]int{}
	for _, item := range items {
		priorities[item.identity] = 2
		info, ok := p.infos[item.identity]
		if !ok || info.ParentID == "" {
			continue
		}
		for _, root := range r.tabs {
			s := root.swarmSnapshot
			if s == nil {
				continue
			}
			m := s.Members[item.identity]
			if m == nil {
				continue
			}
			state := swarm.MemberState(s, m)
			_, approval, _ := r.swarmListing(item.identity, info.Metadata.SwarmID)
			if approval || state.Attention {
				priorities[item.identity] = 0
				break
			}
			if state.Busy {
				priorities[item.identity] = 1
				break
			}
			priorities[item.identity] = 2
			w := s.Workflows[state.Workflow]
			if w == nil && m.Context == "" && !state.Delivering {
				w = &workflow.Report{ID: "released:" + info.ParentID, Name: "released members", Status: "idle"}
			}
			if w == nil || w.Status == "running" {
				break
			}
			if groups[info.ParentID] == nil {
				groups[info.ParentID] = map[string]*pickerWorkflowHistory{}
			}
			group := groups[info.ParentID][w.ID]
			if group == nil {
				group = &pickerWorkflowHistory{id: w.ID, name: w.Name, status: w.Status, finished: w.Finished, deferred: swarm.DeferredCount(s, w.ID)}
				if group.name == "" {
					group.name = "untitled workflow"
				}
				groups[info.ParentID][w.ID] = group
			}
			group.items = append(group.items, item)
			historical[item.identity] = true
			break
		}
	}
	if len(groups) == 0 {
		return items
	}
	out := []replModalItem{}
	// Keep each root group in workspace order. Existing direct children retain
	// their stable order within attention/running/other classes.
	for i := 0; i < len(items); {
		root := items[i]
		end := i + 1
		for end < len(items) && items[end].parent != "" {
			end++
		}
		// Sort direct-child subtrees together so nested legacy agents never
		// precede or become detached from their own parent.
		var children [][]replModalItem
		for _, item := range items[i+1 : end] {
			if item.parent == root.value || len(children) == 0 {
				children = append(children, []replModalItem{item})
			} else {
				n := len(children) - 1
				children[n] = append(children[n], item)
			}
		}
		sort.SliceStable(children, func(a, b int) bool { return priorities[children[a][0].identity] < priorities[children[b][0].identity] })
		out = append(out, root)
		for _, subtree := range children {
			for _, item := range subtree {
				if !historical[item.identity] {
					out = append(out, item)
				}
			}
		}
		if history := groups[root.identity]; len(history) > 0 {
			historyID := "history:" + root.identity
			out = append(out, replModalItem{label: "  History", value: historyID, identity: historyID, parent: root.value, children: len(history), groupOnly: true})
			ordered := make([]*pickerWorkflowHistory, 0, len(history))
			for _, group := range history {
				ordered = append(ordered, group)
			}
			sort.Slice(ordered, func(a, b int) bool {
				if ordered[a].finished.Equal(ordered[b].finished) {
					return ordered[a].id < ordered[b].id
				}
				return ordered[a].finished.After(ordered[b].finished)
			})
			for _, group := range ordered {
				id := "workflow-history:" + group.id
				label := fmt.Sprintf("    %s · %s", group.name, group.status)
				if group.deferred > 0 {
					label += fmt.Sprintf(" · %d deferred", group.deferred)
				}
				out = append(out, replModalItem{label: label, value: id, identity: id, parent: historyID, children: len(group.items), groupOnly: true})
				for _, child := range group.items {
					child.parent = id
					child.label = "    " + child.label
					child.display = "    " + child.display
					child.selectedDisplay = "    " + child.selectedDisplay
					out = append(out, child)
				}
			}
		}
		i = end
	}
	return out
}

// If a selected agent finishes during refresh, expose its new ancestors so
// selection and inspector actions still address the same saved identity.
func expandPickerSelection(m *replModal, selected string) {
	byValue := map[string]replModalItem{}
	parent := ""
	for _, item := range m.items {
		byValue[item.value] = item
		if item.identity == selected || item.value == selected {
			parent = item.parent
		}
	}
	seen := map[string]bool{}
	for parent != "" && !seen[parent] {
		seen[parent] = true
		if m.expanded == nil {
			m.expanded = map[string]bool{}
		}
		m.expanded[parent] = true
		parent = byValue[parent].parent
	}
}
