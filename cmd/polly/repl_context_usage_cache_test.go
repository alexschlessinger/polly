package main

import (
	"errors"
	"slices"
	"testing"
)

func TestContextUsageRowsReuseUnchangedProjection(t *testing.T) {
	d := &contextPopover{}
	for i := 0; i < 500; i++ {
		d.snapshot.usage.agents = append(d.snapshot.usage.agents, contextAgentUsage{id: "worker", label: "worker"})
	}
	m := &replModal{context: d}
	d.usageOpen = true
	d.refreshItems(m, 50)
	first, details := &m.items[0], &m.details[0]
	m.selected, m.top = 150, 140
	d.refreshItems(m, 50)
	if &m.items[0] != first || &m.details[0] != details || m.selected != 150 || m.top != 140 {
		t.Fatal("unchanged usage rows were rebuilt or navigation moved")
	}
	d.snapshot.usage.agents = slices.Clone(d.snapshot.usage.agents)
	d.snapshot.usage.agents[0].label = "updated"
	d.refreshItems(m, 50)
	if &m.items[0] == first {
		t.Fatal("new immutable usage projection did not invalidate rows")
	}
	first = &m.items[0]
	d.refreshItems(m, 30)
	if &m.items[0] == first {
		t.Fatal("width change kept stale wrapping")
	}
	d.err = errors.New("")
	d.refreshItems(m, 30)
	if len(m.items) > 5 || m.titleNotice == "" {
		t.Fatal("empty error retained cached usage rows")
	}
}
