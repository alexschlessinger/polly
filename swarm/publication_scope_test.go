package swarm

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/worktree"
)

// Independent workers should share a canonical claim without losing their
// differently worded evidence or pretending each confirmation was delivered.
func TestPublicationFactCoalescesParaphrasesAndPreservesEvidence(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		for _, id := range []string{"a", "b", "c", "d", "reader"} {
			s.Members[id] = &Member{ID: id}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	texts := []string{"CLAUDE.md is absent.", "No root CLAUDE.md exists in this workspace.", "The captured checkout has no CLAUDE.md file.", "Checked the repository root; CLAUDE.md was not found."}
	var records []*Publication
	for i, author := range []string{"a", "b", "c", "d"} {
		p, err := r.Publish(ctx, author, Publication{Key: "files/claude-md", Value: "absent", Scope: PublicationScopeWorkspace, Text: texts[i], Sources: []string{author + "/probe.log"}})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, p)
		if i == 0 && p.DuplicateOf != "" || i > 0 && p.DuplicateOf != records[0].ID {
			t.Fatalf("confirmation %d: %+v", i, p)
		}
	}
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, original := range records {
		if got := s.Publications[original.ID]; !reflect.DeepEqual(got, original) || got.Text != texts[i] {
			t.Fatalf("lost persisted attribution/evidence: %+v", got)
		}
	}
	first := records[0]
	if got := pendingPublications(s, "reader", first.Run); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("paraphrases rebroadcast: %+v", got)
	}
	if got := pendingPublications(s, "reader", "later-run"); len(got) != 0 {
		t.Fatalf("workspace observation escaped its run: %+v", got)
	}
	if got := pendingPublications(s, r.ID, "later-run"); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("parent lost scoped evidence: %+v", got)
	}
	view := PresentPublication(s, s.Publications[first.ID])
	if len(view.Confirmations) != 3 || view.Scope != PublicationScopeWorkspace || view.Value != "absent" {
		t.Fatalf("confirmation links: %+v", view)
	}
	for _, p := range records[1:] {
		if !slices.Contains(view.Confirmations, p.ID) {
			t.Fatalf("missing confirmation %s", p.ID)
		}
	}
	admitParent(t, r)
	s, _ = r.read(ctx)
	if got := pendingPublications(s, r.ID, first.Run); len(got) != 0 || !slices.Contains(publicationReaders(s, first), r.ID) {
		t.Fatalf("canonical delivery receipt lost: pending=%v readers=%v", got, publicationReaders(s, first))
	}
	for _, p := range records[1:] {
		if got := publicationReaders(s, p); len(got) != 0 {
			t.Fatalf("undelivered confirmation claimed readers: %v", got)
		}
	}
	// A contrary claim is retained and delivered separately, never silently
	// replacing a teammate's claim through a shared key.
	conflict, err := r.Publish(ctx, "b", Publication{Key: first.Key, Value: "present", Scope: first.Scope, Text: "CLAUDE.md is present in my workspace."})
	if err != nil || conflict.DuplicateOf != "" {
		t.Fatalf("conflicting evidence coalesced: %+v %v", conflict, err)
	}
	s, _ = r.read(ctx)
	if got := visiblePublications(s, first.Run, false); len(got) != 2 {
		t.Fatalf("conflict hidden: %+v", got)
	}
	if _, err := r.Publish(ctx, "b", Publication{Supersedes: first.ID, Text: "overwrite another author"}); err == nil {
		t.Fatal("another author could supersede the canonical claim")
	}
	// Key searches return every original evidence record, including confirmations.
	r.RegisterParentTools(r.config.Registry)
	reader, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	text, err := reader.Execute(ctx, map[string]any{"view": "publications", "query": first.Key})
	var page struct{ Items []PublicationView }
	if err != nil || json.Unmarshal([]byte(text), &page) != nil || len(page.Items) != 5 {
		t.Fatalf("key lookup lost provenance: %s %v", text, err)
	}
}

func TestPublicationScopesAndCorrections(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	commit := strings.Repeat("a", 40)
	if err := r.update(ctx, func(s *State) error {
		s.Snapshots["base"] = &worktree.Snapshot{ID: "base", Commit: commit, Tree: "tree", Source: "/repo"}
		s.Members["worker"] = &Member{ID: "worker", Execution: "execution"}
		s.Executions["execution"] = &Execution{ID: "execution", Member: "worker", Run: "earlier-run", Base: "base"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, err := r.Publish(ctx, "worker", Publication{Scope: PublicationScopeSnapshot, Key: "files/claude-md", Value: "absent", Text: "No file in this captured baseline."})
	if err != nil || first.Snapshot != "base" || first.Run != "earlier-run" {
		t.Fatalf("inferred scope: %+v %v", first, err)
	}
	correction, err := r.Publish(ctx, "worker", Publication{Supersedes: first.ID, Value: "present", Text: "Correction: it exists in the nested project."})
	if err != nil || correction.Scope != first.Scope || correction.Key != first.Key || correction.Snapshot != first.Snapshot || correction.Author != first.Author {
		t.Fatalf("correction lost identity: %+v %v", correction, err)
	}
	s, _ := r.read(ctx)
	if got := visiblePublications(s, first.Run, false); len(got) != 1 || got[0].ID != correction.ID {
		t.Fatalf("superseded evidence still current: %+v", got)
	}
	if got := PresentPublication(s, correction); got.Commit != commit || got.Supersedes != first.ID {
		t.Fatalf("correction provenance: %+v", got)
	}
	// Legacy host publications remain machine-wide; unkeyed records retain
	// their historical delivery behavior rather than being guessed equivalent.
	for range 2 {
		if _, err := r.Publish(ctx, r.ID, Publication{Kind: PublicationKindHost, Text: "legacy machine fact"}); err != nil {
			t.Fatal(err)
		}
	}
	s, _ = r.read(ctx)
	if got := pendingPublications(s, "worker", "later-run"); len(got) != 2 {
		t.Fatalf("legacy host compatibility: %+v", got)
	}
	for _, p := range []Publication{
		{Key: "Bad key", Text: "invalid key"},
		{Value: "absent", Text: "missing key"},
		{Scope: PublicationScopeMachine, Text: "machine finding"},
		{Kind: PublicationKindHost, Scope: PublicationScopeWorkspace, Text: "local host"},
		{Scope: PublicationScopeSnapshot, Text: "no parent baseline"},
	} {
		if _, err := r.Publish(ctx, r.ID, p); err == nil {
			t.Fatalf("accepted invalid scope/identity: %+v", p)
		}
	}
}
