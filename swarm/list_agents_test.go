package swarm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
)

// list_agents leaves retired members out and counts them; all: true lists
// everyone in the same shape.
func TestListAgentsOmitsRetiredUnlessAll(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		for _, id := range []string{"a", "b", "c"} {
			s.Members[id] = &Member{ID: id, Name: id, Label: "worker " + id, ReadOnly: true}
		}
		s.Members["c"].Control = MemberControlRetired
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page := func(args tools.Args) map[string]any {
		t.Helper()
		out, err := r.inspectAgents(ctx, r.ID, args)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	byDefault := page(tools.Args{})
	if byDefault["total"] != float64(2) || byDefault["retired"] != float64(1) || len(byDefault["items"].([]any)) != 2 {
		t.Fatalf("default listing: %v", byDefault)
	}
	for _, item := range byDefault["items"].([]any) {
		if item.(map[string]any)["id"] == "c" {
			t.Fatal("retired member listed by default")
		}
	}
	everyone := page(tools.Args{"all": true})
	if everyone["total"] != float64(3) || everyone["retired"] != nil || len(everyone["items"].([]any)) != 3 {
		t.Fatalf("all listing: %v", everyone)
	}
}
