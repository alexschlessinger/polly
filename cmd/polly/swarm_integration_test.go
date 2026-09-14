package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestSwarmIntegrationInspectorShowsDecisionAndOutcome(t *testing.T) {
	commit, observed := strings.Repeat("a", 40), strings.Repeat("b", 40)
	s := &swarm.State{Integrations: map[string]*swarm.IntegrationCandidate{"candidate": {ID: "candidate", Status: "superseded", Drift: "paths", Accepted: true, Predecessor: "previous", Successor: "next", Merged: worktree.Snapshot{ID: "internal-tested", Commit: commit, Tree: "tree", Source: "/candidate"}, Conflicts: []worktree.Conflict{{Type: "CONFLICT (binary)", Paths: []string{"image.bin"}, Message: "binary conflict"}}}}, Applies: map[string]*swarm.ApplyRecord{"candidate": {Status: "recovery_required", ObservedParent: worktree.Snapshot{ID: "internal-observed", Commit: observed, Tree: "tree", Source: "/parent"}, Error: "interrupted"}}}
	s.Snapshots = map[string]*worktree.Snapshot{"internal-tested": &s.Integrations["candidate"].Merged, "internal-observed": &s.Applies["candidate"].ObservedParent}
	text := swarmInspectorText(s, nil, "integrations")
	if strings.Contains(text, "internal-tested") || strings.Contains(text, "internal-observed") || strings.Contains(text, "Validated snapshot") {
		t.Fatal(text)
	}
	for _, want := range []string{"superseded", "Drift: paths", "Accepted: true", "Predecessor: previous", "Superseded by: next", "Candidate commit: " + commit, "CONFLICT (binary)", "image.bin", "recovery_required", observed, "interrupted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
}
