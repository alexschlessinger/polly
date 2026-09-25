package swarm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
)

// The integration gate serialises the parent's tools with the coordinator's
// exclusive Git writes; a member's binding is independent of it.
func TestParentGateDoesNotReachMemberBindings(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("list", "list_dir", `{"path":"."}`)
		}
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "timed out") {
			t.Error("member tool waited on the parent's gate")
		}
		return answer("done")
	})
	r := runtimeTest(t, model, 1, 1)
	if _, err := r.config.Registry.LoadToolAuto("list_dir"); err != nil {
		t.Fatal(err)
	}
	release, err := r.gate.Exclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	parentTool, _, _ := r.config.Registry.GetIfAllowed("list_dir")
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if execution, err := r.config.Registry.ExecuteTool(short, parentTool, map[string]any{"path": "."}, time.Second); execution.Invoked || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent tool ran while the gate was held: %+v %v", execution, err)
	}

	ctx, cancelRun := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRun()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "list", ReadOnly: true})
	if err != nil || result.Text != "done" || calls.Load() != 2 {
		t.Fatalf("member did not run while the gate was held: %+v %v (calls %d)", result, err, calls.Load())
	}
}
