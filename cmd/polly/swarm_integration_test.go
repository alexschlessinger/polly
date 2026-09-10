package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestSwarmIntegrationInspectorShowsDecisionAndOutcome(t *testing.T) {
	s := &swarm.State{Integrations: map[string]*swarm.IntegrationCandidate{"candidate": {ID: "candidate", Status: "superseded", Drift: "paths", Accepted: true, Predecessor: "previous", Successor: "next", Merged: worktree.Snapshot{ID: "tested"}, Conflicts: []worktree.Conflict{{Type: "CONFLICT (binary)", Paths: []string{"image.bin"}, Message: "binary conflict"}}}}, Applies: map[string]*swarm.ApplyRecord{"candidate": {Status: "recovery_required", ObservedParent: worktree.Snapshot{ID: "observed"}, Error: "interrupted"}}}
	text := swarmInspectorText(s, nil, "integrations")
	for _, want := range []string{"superseded", "Drift: paths", "Accepted: true", "Predecessor: previous", "Superseded by: next", "tested", "CONFLICT (binary)", "image.bin", "recovery_required", "observed", "interrupted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
}
