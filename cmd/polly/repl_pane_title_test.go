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
	if !slices.Equal(fake.titles, []string{label}) || label == "" {
		t.Fatalf("the tab was named %q, want %q", fake.titles, label)
	}
	// Nothing changed, so the host is not asked again.
	r.render()
	if r.panes.titling {
		t.Fatal("an unchanged title was sent to the host again")
	}
	// A new title replaces the one polly gave.
	r.titlePane("Second title", "calm-otter")
	pumpUI(t, r, func() bool { return r.panes.titled == "Second title" })
	if fake.named != "Second title" {
		t.Fatalf("the tab is named %q after a title change", fake.named)
	}
	// The tab gets its own name back when polly leaves.
	r.closePanes()
	if fake.named != "" || r.panes.titled != "" {
		t.Fatalf("the tab kept the name %q after the run", fake.named)
	}
}

func TestTabNamedByItsOwnerKeepsThatName(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	fake.named = "server"
	r.titlePane("Fix the login redirect", "calm-otter")
	pumpUI(t, r, func() bool { return r.panes.titled != "" })
	r.closePanes()
	if fake.named != "server" || len(fake.titles) != 0 {
		t.Fatalf("a tab someone named was renamed: %q %q", fake.named, fake.titles)
	}
}

func TestTabLaunchedForASessionTakesItsTitle(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	// A tab polly was launched into carries the session's name until then.
	fake.named = "calm-otter"
	r.titlePane("Fix the login redirect", "calm-otter")
	pumpUI(t, r, func() bool { return r.panes.titled != "" })
	if fake.named != "Fix the login redirect" {
		t.Fatalf("the launched tab is named %q", fake.named)
	}
}
