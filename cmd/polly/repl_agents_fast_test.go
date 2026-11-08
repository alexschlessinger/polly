package main

import (
	"fmt"
	"image"
	"testing"

	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentProgressViewportWindow(t *testing.T) {
	r := newManagedREPL(&Config{}, "root", 0, 0)
	r.chrome.inner = image.Rect(0, 0, 80, 24)
	list := &agentsInspectorState{entries: map[string]agentsInspectorEntry{}}
	actions := make([]string, 1024)
	for i := range actions {
		id := fmt.Sprint(i)
		actions[i] = "agents-open:" + id
		list.entries[id] = agentsInspectorEntry{section: agentWorking}
	}
	r.setAgentProgressWindow(list, 800, actions)
	if len(list.progressWindow) > 32 || !list.wantsProgress("800") || list.wantsProgress("0") || list.wantsProgress("900") {
		t.Fatalf("unbounded/offscreen window: %+v", list.progressWindow)
	}
	r.setAgentProgressWindow(list, 0, actions)
	if !list.wantsProgress("0") || list.wantsProgress("800") {
		t.Fatal("scroll did not move load window")
	}
}

func TestUnchangedAgentsInspectorPaintWork(t *testing.T) {
	var baseline float64
	for _, n := range []int{16, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r := newManagedREPL(&Config{}, "root", 0, 0)
			r.chrome.inner = image.Rect(0, 0, 80, 24)
			s := &swarm.State{Members: map[string]*swarm.Member{}}
			for i := range n {
				id := fmt.Sprintf("m%04d", i)
				s.Members[id] = &swarm.Member{ID: id, Name: id}
			}
			root := r.visibleTab()
			root.swarmDisplay = swarm.ProjectDisplayState(s, root.viewID())
			r.openAgentsInspector()
			geometry := viewGeometry{width: 80}
			r.refreshAgentsInspector(geometry)
			original := r.workspace().inspector.current
			allocs := testing.AllocsPerRun(10, func() { r.refreshAgentsInspector(geometry) })
			if original != r.workspace().inspector.current {
				t.Fatal("unchanged list rebuilt")
			}
			if n == 16 {
				baseline = allocs
			} else if allocs > baseline+3 {
				t.Fatalf("unchanged paint grew with retained roster: %g vs %g", allocs, baseline)
			}
			t.Logf("members=%d unchanged paint allocations=%g", n, allocs)
			list := r.workspace().viewState(r.workspace().inspector.target).agents
			key := list.renderKey
			list.doneCollapsed = !list.doneCollapsed
			r.refreshAgentsInspector(geometry)
			if list.renderKey == key {
				t.Fatal("section toggle did not invalidate rows")
			}
		})
	}
}
