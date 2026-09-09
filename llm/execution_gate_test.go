package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

type gateOutputTool struct{ *tools.Func }

func (t gateOutputTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	_, err := t.Execute(ctx, args)
	return tools.ToolOutput{Text: "retained", Data: 42, Media: []tools.ToolMedia{{Data: []byte("image"), MIMEType: "image/png"}}}, err
}

func TestAgentGateReleaseAndRichOutput(t *testing.T) {
	for _, result := range []string{"success", "error", "panic"} {
		t.Run(result, func(t *testing.T) {
			gate := tools.NewExecutionGate()
			tool := gateOutputTool{&tools.Func{Name: "writer", Run: func(ctx context.Context, _ tools.Args) (string, error) {
				wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				if _, err := gate.Exclusive(wait); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("writer not gated: %v", err)
				}
				if result == "panic" {
					panic("tool panic")
				}
				if result == "error" {
					return "", errors.New("tool failure")
				}
				return "", nil
			}}}
			r := tools.NewToolRegistry([]tools.Tool{tool}, tools.WithUnsafeNoSandbox())
			defer r.Close()
			r.SetExecutionGate(gate)
			a := NewAgent(nil, r, AgentConfig{})
			func() {
				defer func() {
					if v := recover(); v != nil && result != "panic" {
						t.Fatal(v)
					}
				}()
				out, err := a.executeToolCall(context.Background(), messages.ChatMessageToolCall{Name: "writer"}, map[string]any{})
				if (err != nil) != (result == "error") || out.Data != 42 || len(out.Media) != 1 {
					t.Fatalf("output lost: %+v %v", out, err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := gate.Exclusive(ctx)
			if err != nil {
				t.Fatal("permit leaked", err)
			}
			release()
		})
	}
}

func TestForegroundCoordinatorCanApply(t *testing.T) {
	gate := tools.NewExecutionGate()
	tool := &tools.Func{Name: "coordinator", Coordinator: true, Run: func(ctx context.Context, _ tools.Args) (string, error) {
		release, err := gate.Exclusive(ctx)
		if err == nil {
			release()
		}
		return "applied", err
	}}
	r := tools.NewToolRegistry([]tools.Tool{tool}, tools.WithUnsafeNoSandbox())
	defer r.Close()
	r.SetExecutionGate(gate)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := NewAgent(nil, r, AgentConfig{}).executeToolCall(ctx, messages.ChatMessageToolCall{Name: "coordinator"}, map[string]any{})
	if err != nil {
		t.Fatal("coordinator deadlocked", err)
	}
}
