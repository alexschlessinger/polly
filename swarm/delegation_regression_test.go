package swarm

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestSpawnRejectsMalformedToolSelections(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 5)
	r.RegisterParentTools(r.config.Registry)
	spawn, _, _ := r.config.Registry.GetIfAllowed("spawn_agent")
	for _, value := range []any{nil, "bash", 1, true, map[string]any{}, []any{"bash", 42}} {
		_, err := spawn.Execute(context.Background(), map[string]any{"task_name": "worker", "message": "inspect", "read_only": true, "tools": value})
		if err == nil {
			t.Fatalf("accepted malformed tools: %#v", value)
		}
	}
	s, err := r.read(context.Background())
	if err != nil || len(s.Members) != 0 {
		t.Fatalf("malformed spawn created workers: %+v %v", s, err)
	}
}

func TestManagedCoordinationAllowlistBindsChildIdentity(t *testing.T) {
	var calls atomic.Int32
	selected := []string{"send_message", "list_agents", "wait_agent"}
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var names []string
		for _, tool := range req.Tools {
			names = append(names, tool.GetName())
		}
		for _, name := range selected {
			if !slices.Contains(names, name) {
				t.Errorf("selected member tool %s was omitted", name)
			}
		}
		for _, name := range []string{"spawn_agent", "followup_task", "interrupt_agent"} {
			if slices.Contains(names, name) {
				t.Errorf("member inherited parent authority: %s", name)
			}
		}
		if calls.Add(1) == 1 {
			return iterationTool("message", "send_message", `{"target":"/root","message":"child evidence"}`)
		}
		return answer("done")
	}), 1, 2)
	r.RegisterParentTools(r.config.Registry)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Tools: selected})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.State(ctx)
	for _, mail := range s.Messages {
		if mail.Text == "child evidence" && mail.From == result.Session && mail.To == r.ID {
			return
		}
	}
	t.Fatal("selected send_message did not use the member identity")
}

func TestWaitAgentWithoutTimeoutParksUntilAddressedInput(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("wait", "wait_agent", `{}`)
		}
		return answer("received")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "wait", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, r, ctx, func(s *State) bool { return s.Executions[i.id].Status == "waiting" && len(r.slots) == 0 })
	if !i.waitUntil.IsZero() {
		t.Fatalf("default wait has a polling deadline: %v", i.waitUntil)
	}
	if _, err := r.Send(ctx, r.ID, i.member, "info", "", "continue"); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	if calls.Load() != 2 {
		t.Fatalf("addressed input did not resume the parked execution: %d calls", calls.Load())
	}
}
