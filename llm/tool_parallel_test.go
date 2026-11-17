package llm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func parallelCalls(count int) []messages.ChatMessageToolCall {
	calls := make([]messages.ChatMessageToolCall, count)
	for i := range calls {
		calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("call-%d", i), Name: "work", Arguments: fmt.Sprintf(`{"index":%d}`, i)}
	}
	return calls
}

// Count only stacks in this scheduler, rather than asserting a process-wide
// runtime.NumGoroutine value affected by unrelated background activity.
func toolSchedulerGoroutines() int {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		count := 0
		for stack := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "(*Agent).executeToolsParallel") {
				count++
			}
		}
		return count
	}
}

func TestRuntimeStorageScaleToolQueueGoroutinesAndCancellation(t *testing.T) {
	for _, tc := range []struct{ calls, limit int }{{100, 3}, {10_000, 3}, {100, 1}, {64, 0}, {64, -1}} {
		t.Run(fmt.Sprintf("calls=%d/limit=%d", tc.calls, tc.limit), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var invoked atomic.Int64
				registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(ctx context.Context, _ tools.Args) (string, error) {
					invoked.Add(1)
					<-ctx.Done()
					return "partial outcome", ctx.Err()
				}}})
				defer registry.Close()
				a := NewAgent(nil, registry, AgentConfig{MaxParallelTools: tc.limit})
				defer a.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := parallelCalls(tc.calls)
				var results []messages.ChatMessage
				var err error
				go func() { results, err = a.executeToolBatch(ctx, calls, nil, 1, nil, toolInlineTokenLimit) }()
				synctest.Wait() // dispatcher/workers have all reached channel waits
				active := tc.limit
				if active <= 0 {
					active = tc.calls
				}
				if got := invoked.Load(); got != int64(active) {
					t.Fatalf("active tools=%d, want %d", got, active)
				}
				stacks := toolSchedulerGoroutines()
				if stacks > active+1 {
					t.Fatalf("%d scheduler goroutines for %d active workers and %d calls", stacks, active, tc.calls)
				}
				t.Logf("calls=%d limit=%d active=%d scheduler goroutines=%d (includes dispatcher)", tc.calls, tc.limit, active, stacks)
				cancel()
				synctest.Wait()
				if tc.limit > 0 && !errors.Is(err, context.Canceled) {
					t.Fatalf("batch cancellation=%v", err)
				}
				if len(results) != len(calls) || invoked.Load() != int64(active) {
					t.Fatalf("cancellation started queued work or lost receipts: results=%d invoked=%d", len(results), invoked.Load())
				}
				partial := 0
				for i, result := range results {
					if result.Role != messages.MessageRoleTool || result.ToolCallID != calls[i].ID {
						t.Fatalf("receipt %d out of order: %+v", i, result)
					}
					if strings.Contains(result.Content, "partial outcome") {
						partial++
					}
				}
				if partial != active {
					t.Fatalf("kept %d partial outcomes, want %d", partial, active)
				}
			})
		})
	}
}

type parallelRichTool struct {
	tools.Func
	run func(context.Context, tools.Args) (tools.ToolOutput, error)
}

func (t *parallelRichTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	return t.run(ctx, tools.Args(args))
}

func TestLimitedToolQueueAgentOrderingApprovalAndRichResults(t *testing.T) {
	secondDone := make(chan struct{})
	var stage atomic.Int32
	var invoked atomic.Int64
	tool := &parallelRichTool{Func: tools.Func{Name: "work"}, run: func(ctx context.Context, args tools.Args) (tools.ToolOutput, error) {
		if stage.Load() != 4 {
			t.Error("tool effect preceded batch intent/approval")
		}
		invoked.Add(1)
		i := args.Int("index", -1)
		if i == 0 {
			select {
			case <-secondDone:
			case <-ctx.Done():
				return tools.ToolOutput{}, ctx.Err()
			}
		}
		output := tools.ToolOutput{Text: fmt.Sprintf("result %d", i), Data: map[string]any{"index": i}}
		if i == 1 {
			output.Media = []tools.ToolMedia{{Data: []byte("evidence"), MIMEType: "application/pdf", Name: "evidence.pdf"}}
			return output, errors.New("ordinary failure with evidence")
		}
		return output, nil
	}}
	calls := parallelCalls(12)
	model := &sequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	registry := tools.NewToolRegistry([]tools.Tool{tool})
	defer registry.Close()
	a := NewAgent(model, registry, AgentConfig{MaxParallelTools: 2, ArtifactStore: newTestArtifactStore()})
	defer a.Close()
	advance := func(from, to int32) {
		if !stage.CompareAndSwap(from, to) {
			t.Errorf("batch callbacks stage=%d, want %d", stage.Load(), from)
		}
	}
	response, err := a.Run(context.Background(), &CompletionRequest{Messages: messages.User("go")}, &AgentCallbacks{
		BeforeToolBatch: func(context.Context, []messages.ChatMessageToolCall) error { advance(0, 1); return nil },
		JournalToolBatch: func(_ context.Context, checkpoint AgentCheckpoint) error {
			advance(1, 2)
			if len(checkpoint.Generated) != 1 || len(checkpoint.Generated[0].ToolCalls) != len(calls) {
				t.Error("intent journal omitted calls before effects")
			}
			return nil
		},
		OnToolStart: func([]messages.ChatMessageToolCall) { advance(2, 3) },
		ApproveToolCalls: func(context.Context, []messages.ChatMessageToolCall) ([]bool, error) {
			advance(3, 4)
			approved := make([]bool, len(calls))
			for i := range approved {
				approved[i] = i != 2
			}
			return approved, nil
		},
		OnToolResult: func(call messages.ChatMessageToolCall, _ messages.ChatMessage) {
			if call.ID == calls[1].ID {
				close(secondDone)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	results := messagesWithRole(response.AllMessages, messages.MessageRoleTool)
	if len(results) != len(calls) || invoked.Load() != int64(len(calls)-1) {
		t.Fatalf("results=%d effects=%d", len(results), invoked.Load())
	}
	for i, result := range results {
		if result.ToolCallID != calls[i].ID {
			t.Fatalf("receipt %d reordered: %+v", i, result)
		}
		if i == 2 {
			if result.Content != ToolDeniedContent {
				t.Fatal("denial lost")
			}
			continue
		}
		if succeeded, known := result.ToolSucceeded(); !known || succeeded != (i != 1) {
			t.Fatalf("receipt %d lost ordinary outcome", i)
		}
		if result.Metadata["tool_data"] == nil || !strings.Contains(result.Content, fmt.Sprintf("result %d", i)) {
			t.Fatalf("receipt %d lost typed/text outcome", i)
		}
	}
	if len(results[1].Parts) != 1 || results[1].Parts[0].Artifact == nil {
		t.Fatal("rich partial failure lost artifact evidence")
	}
}

func TestLimitedToolQueueRechecksQueuedHandles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var invoked, replacement atomic.Int64
		registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) {
			invoked.Add(1)
			<-release
			return "old", nil
		}}})
		defer registry.Close()
		a := NewAgent(nil, registry, AgentConfig{MaxParallelTools: 2})
		defer a.Close()
		calls := parallelCalls(20)
		var results []messages.ChatMessage
		var err error
		go func() { results, err = a.executeToolsParallel(context.Background(), calls, nil, toolInlineTokenLimit) }()
		synctest.Wait()
		if invoked.Load() != 2 {
			t.Fatalf("active=%d", invoked.Load())
		}
		a.tools.Register(&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { replacement.Add(1); return "replacement", nil }})
		close(release)
		synctest.Wait()
		if err != nil || invoked.Load() != 2 || replacement.Load() != 0 {
			t.Fatalf("queued handle substituted: old=%d replacement=%d err=%v", invoked.Load(), replacement.Load(), err)
		}
		for _, result := range results[2:] {
			if !strings.Contains(result.Content, "replaced before execution") {
				t.Fatalf("queued handle was not rechecked: %+v", result)
			}
		}
	})
}

func TestLimitedToolQueueJournalFailureStartsNothing(t *testing.T) {
	var invoked atomic.Int64
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { invoked.Add(1); return "ok", nil }}})
	defer registry.Close()
	a := NewAgent(nil, registry, AgentConfig{MaxParallelTools: 2})
	defer a.Close()
	failure := errors.New("intent journal refused")
	results, err := a.executeToolBatch(context.Background(), parallelCalls(100), nil, 1, &AgentCallbacks{
		JournalToolBatch: func(context.Context, AgentCheckpoint) error { return failure },
	}, toolInlineTokenLimit)
	if !errors.Is(err, failure) || invoked.Load() != 0 || len(results) != 100 {
		t.Fatalf("failed journal allowed effects: %v invoked=%d results=%d", err, invoked.Load(), len(results))
	}
}

func TestLimitedToolQueueArtifactFailureRetainsCompletedResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		completed := make(chan struct{})
		var invoked atomic.Int64
		registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(ctx context.Context, args tools.Args) (string, error) {
			invoked.Add(1)
			switch args.Int("index", -1) {
			case 0:
				return "completed", nil
			case 1:
				<-completed
				return strings.Repeat("x", 60_000), nil
			default:
				<-ctx.Done()
				return "", ctx.Err()
			}
		}}})
		defer registry.Close()
		a := NewAgent(nil, registry, AgentConfig{MaxParallelTools: 2, ArtifactStore: failingArtifactStore{}})
		defer a.Close()
		calls := parallelCalls(100)
		results, err := a.executeToolBatch(context.Background(), calls, nil, 1, &AgentCallbacks{
			OnToolResult: func(call messages.ChatMessageToolCall, _ messages.ChatMessage) {
				if call.ID == calls[0].ID {
					close(completed)
				}
			},
		}, toolInlineTokenLimit)
		if !errors.Is(err, errFailingArtifactStore) || len(results) != len(calls) || invoked.Load() > 3 {
			t.Fatalf("store failure: %v, receipts=%d, effects=%d", err, len(results), invoked.Load())
		}
		if results[0].Content != "completed" || results[1].Content != ToolInterruptedContent {
			t.Fatalf("completed result or interruption lost: %+v", results[:2])
		}
		for i, result := range results {
			if result.ToolCallID != calls[i].ID || result.Role != messages.MessageRoleTool {
				t.Fatalf("receipt %d missing or reordered", i)
			}
		}
	})
}

func TestLimitedToolQueueSingleWorkerRequestOrder(t *testing.T) {
	next := 0
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(_ context.Context, args tools.Args) (string, error) {
		if i := args.Int("index", -1); i != next {
			t.Errorf("single-worker effect %d preceded %d", i, next)
		}
		next++
		return "ok", nil
	}}})
	defer registry.Close()
	a := NewAgent(nil, registry, AgentConfig{MaxParallelTools: 1})
	defer a.Close()
	if _, err := a.executeToolsParallel(context.Background(), parallelCalls(100), nil, toolInlineTokenLimit); err != nil || next != 100 {
		t.Fatalf("single worker: %v, effects=%d", err, next)
	}
}

func BenchmarkRuntimeStorageScaleToolQueue(b *testing.B) {
	for _, count := range []int{100, 10_000} {
		for _, limit := range []int{4, 0} {
			b.Run(fmt.Sprintf("calls=%d/limit=%d", count, limit), func(b *testing.B) {
				registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}})
				defer registry.Close()
				a := NewAgent(nil, registry, AgentConfig{MaxParallelTools: limit})
				defer a.Close()
				calls := parallelCalls(count)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := a.executeToolsParallel(context.Background(), calls, nil, toolInlineTokenLimit); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
