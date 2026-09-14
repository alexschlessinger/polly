package swarm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
)

// Available workers remain discoverable after their assignments settle.
func TestListAgentsOmitsDormantUnlessAll(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		for _, id := range []string{"a", "b", "c"} {
			s.Members[id] = &Member{ID: id, Name: id, Label: "worker " + id, ReadOnly: true}
		}
		for _, id := range []string{"a", "b"} {
			s.Members[id].Task = id
			s.Tasks[id] = &Task{ID: id, Owner: id, Status: "pending"}
		}
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
	if byDefault["total"] != float64(3) || len(byDefault["items"].([]any)) != 3 {
		t.Fatalf("default listing: %v", byDefault)
	}
	selected := page(tools.Args{"path_prefix": "/root/agent_c"})
	if selected["total"] != float64(1) {
		t.Fatalf("filtered listing: %v", selected)
	}
}
