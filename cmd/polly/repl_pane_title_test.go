package main

import (
	"slices"
	"testing"
)

func TestTabIsNamedAfterTheSession(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	// The frame names the tab once it has painted, with the session's label.
	r.model.status.contextName, r.model.status.title = "calm-otter", "Fix the login redirect"
	label := r.model.status.displayLabel()
	r.render()
	pumpUI(t, r, func() bool { return r.panes.titled != "" })
	if titles := fake.getTitles(); !slices.Equal(titles, []string{label}) || label == "" {
		t.Fatalf("the tab was named %q, want %q", titles, label)
	}
	// Nothing changed, so the host is not asked again.
	r.render()
	if r.panes.titling {
		t.Fatal("an unchanged title was sent to the host again")
	}
	// A new title replaces the one polly gave.
	r.titlePane("Second title", "calm-otter")
	pumpUI(t, r, func() bool { return r.panes.titled == "Second title" })
	if named := fake.getNamed(); named != "Second title" {
		t.Fatalf("the tab is named %q after a title change", named)
	}
	// The tab gets its own name back when polly leaves.
	r.closePanes()
	if named := fake.getNamed(); named != "" || r.panes.titled != "" {
		t.Fatalf("the tab kept the name %q after the run", named)
	}
}

func TestTabNamedByItsOwnerKeepsThatName(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	fake.setNamed("server")
	r.titlePane("Fix the login redirect", "calm-otter")
	pumpUI(t, r, func() bool { return r.panes.titled != "" })
	r.closePanes()
	if named, titles := fake.getNamed(), fake.getTitles(); named != "server" || len(titles) != 0 {
		t.Fatalf("a tab someone named was renamed: %q %q", named, titles)
	}
}

func TestTabLaunchedForASessionTakesItsTitle(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	// A tab polly was launched into carries the session's name until then.
	fake.setNamed("calm-otter")
	r.titlePane("Fix the login redirect", "calm-otter")
	pumpUI(t, r, func() bool { return r.panes.titled != "" })
	if named := fake.getNamed(); named != "Fix the login redirect" {
		t.Fatalf("the launched tab is named %q", named)
	}
}
