package swarm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

type workflowExecutionTool struct {
	*tools.Func
	run func(context.Context) (tools.ToolOutput, error)
}

func (workflowExecutionTool) ContextIndependent() bool { return true }
func (t workflowExecutionTool) ExecuteOutput(ctx context.Context, _ map[string]any) (tools.ToolOutput, error) {
	return t.run(ctx)
}

func TestWorkflowSharedExecutionPreservesApprovalsGateAndMedia(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	type callbackKey struct{}
	called := 0
	failure := tools.NewToolError("failure with evidence", "test_failure")
	want := tools.ToolOutput{Text: "evidence", Data: map[string]any{"count": 3}, Media: []tools.ToolMedia{{Name: "evidence.bin", MIMEType: "application/octet-stream", Data: []byte{0, 1, 2}}}}
	tool := &workflowExecutionTool{Func: &tools.Func{Name: "execution_probe"}, run: func(callCtx context.Context) (tools.ToolOutput, error) {
		called++
		if callCtx.Value(callbackKey{}) != true {
			t.Fatal("execution lost callback context")
		}
		return want, failure
	}}
	r.config.Registry.Register(tool)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	copy, err := h.Call(ctx, workflow.Operation{Kind: "context", Args: map[string]any{"readOnly": true}})
	if err != nil {
		t.Fatal(err)
	}
	call := workflow.Operation{ID: "probe", Kind: "tool", Args: map[string]any{"context": copy, "name": "execution_probe"}}
	approved := false
	r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
		return &llm.AgentCallbacks{
			ApproveToolCalls: func([]messages.ChatMessageToolCall) []bool { return []bool{approved} },
			BeforeToolExecute: func(ctx context.Context, _ messages.ChatMessageToolCall, _ map[string]any) context.Context {
				return context.WithValue(ctx, callbackKey{}, true)
			},
		}
	}
	if _, err := h.Call(ctx, call); err == nil || called != 0 {
		t.Fatalf("denied tool executed: %v", err)
	}
	approved = true
	state, _ := r.read(ctx)
	registry, err := h.registry(ctx, state, state.Contexts[copy.(string)])
	if err != nil {
		t.Fatal(err)
	}
	gate := tools.NewExecutionGate()
	registry.SetExecutionGate(gate)
	release, err := gate.Exclusive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	_, err = h.Call(waitCtx, call)
	release()
	if !errors.Is(err, context.DeadlineExceeded) || called != 0 {
		t.Fatalf("workflow bypassed execution gate: %v", err)
	}
	result, err := h.Call(ctx, call)
	if !errors.Is(err, failure) || called != 1 {
		t.Fatalf("tool error lost: %v", err)
	}
	value := result.(map[string]any)
	refs := value["artifacts"].([]artifacts.Ref)
	if value["text"] != want.Text || value["data"].(map[string]any)["count"] != 3 || len(refs) != 1 {
		t.Fatalf("rich output lost: %+v", value)
	}
	blob, err := r.config.Parent.ArtifactStore().Open(ctx, refs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(blob)
	blob.Close()
	if err != nil || !bytes.Equal(data, want.Media[0].Data) {
		t.Fatalf("media lost: %v %v", data, err)
	}
	tool.run = func(context.Context) (tools.ToolOutput, error) { panic("test panic") }
	func() {
		defer func() {
			if recover() != "test panic" {
				t.Fatal("tool panic did not propagate")
			}
		}()
		_, _ = h.Call(ctx, call)
	}()
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	defer probeCancel()
	release, err = gate.Exclusive(probeCtx)
	if err != nil {
		t.Fatal("workflow error or panic leaked a permit", err)
	}
	release()
	if _, err := h.release(ctx, copy.(string)); err != nil {
		t.Fatal("workflow error or panic leaked the context lock", err)
	}
}
