package streaming

import (
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestUpdateToolCallAtIndex_GrowsSlice(t *testing.T) {
	s := NewStreamState()
	s.UpdateToolCallAtIndex(3, func(tc *messages.ChatMessageToolCall) {
		tc.Name = "myTool"
	})

	calls := s.GetToolCalls()
	if len(calls) != 4 {
		t.Fatalf("expected 4 tool calls, got %d", len(calls))
	}
	// Slots 0-2 should be padded with empty Arguments "{}"
	for i := 0; i < 3; i++ {
		if calls[i].Arguments != "{}" {
			t.Errorf("slot %d: expected Arguments %q, got %q", i, "{}", calls[i].Arguments)
		}
	}
	if calls[3].Name != "myTool" {
		t.Errorf("slot 3: expected Name %q, got %q", "myTool", calls[3].Name)
	}
}

func TestUpdateToolCallAtIndex_ExistingSlotPreserved(t *testing.T) {
	s := NewStreamState()
	s.AddToolCall(messages.ChatMessageToolCall{
		ID:        "tc-1",
		Name:      "existing",
		Arguments: `{"x":1}`,
	})

	// Update index 0's arguments only
	s.UpdateToolCallAtIndex(0, func(tc *messages.ChatMessageToolCall) {
		tc.Arguments = `{"x":1,"y":2}`
	})

	calls := s.GetToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "tc-1" {
		t.Errorf("expected ID %q, got %q", "tc-1", calls[0].ID)
	}
	if calls[0].Name != "existing" {
		t.Errorf("expected Name %q preserved, got %q", "existing", calls[0].Name)
	}
	if calls[0].Arguments != `{"x":1,"y":2}` {
		t.Errorf("expected updated Arguments, got %q", calls[0].Arguments)
	}
}

func TestResetToolCalls_ThenAdd(t *testing.T) {
	s := NewStreamState()
	s.AddToolCall(messages.ChatMessageToolCall{Name: "first"})
	s.ResetToolCalls()
	s.AddToolCall(messages.ChatMessageToolCall{Name: "second"})

	calls := s.GetToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call after reset+add, got %d", len(calls))
	}
	if calls[0].Name != "second" {
		t.Errorf("expected %q, got %q", "second", calls[0].Name)
	}
}

func TestClone_Independence(t *testing.T) {
	s := NewStreamState()
	s.AppendContent("hello")
	s.AppendReasoning("thinking")
	s.AddToolCall(messages.ChatMessageToolCall{ID: "1", Name: "tool1", Arguments: `{}`})
	s.SetMetadata("key", "value")
	s.SetTokenUsage(10, 20)
	s.SetStopReason(messages.StopReasonEndTurn)

	clone := s.Clone()

	// Mutate the original
	s.AppendContent(" world")
	s.AppendReasoning(" more")
	s.AddToolCall(messages.ChatMessageToolCall{ID: "2", Name: "tool2", Arguments: `{}`})
	s.SetMetadata("key", "changed")
	s.SetMetadata("new", "data")
	s.SetTokenUsage(100, 200)
	s.SetStopReason(messages.StopReasonError)

	// Verify clone is unaffected
	if clone.ResponseContent != "hello" {
		t.Errorf("clone content: want %q, got %q", "hello", clone.ResponseContent)
	}
	if clone.ReasoningContent != "thinking" {
		t.Errorf("clone reasoning: want %q, got %q", "thinking", clone.ReasoningContent)
	}
	cloneCalls := clone.GetToolCalls()
	if len(cloneCalls) != 1 {
		t.Fatalf("clone tool calls: want 1, got %d", len(cloneCalls))
	}
	if v, _ := clone.GetMetadata("key"); v != "value" {
		t.Errorf("clone metadata key: want %q, got %v", "value", v)
	}
	if _, ok := clone.GetMetadata("new"); ok {
		t.Error("clone should not have 'new' metadata key")
	}
	if clone.InputTokens != 10 || clone.OutputTokens != 20 {
		t.Errorf("clone tokens: want 10/20, got %d/%d", clone.InputTokens, clone.OutputTokens)
	}
	if clone.StopReason != messages.StopReasonEndTurn {
		t.Errorf("clone stop reason: want %q, got %q", messages.StopReasonEndTurn, clone.StopReason)
	}
}

func TestClone_AllFields(t *testing.T) {
	s := NewStreamState()
	s.AppendContent("content")
	s.AppendReasoning("reasoning")
	s.SetTokenUsage(42, 84)
	s.SetStopReason(messages.StopReasonToolUse)
	s.AddToolCall(messages.ChatMessageToolCall{ID: "a", Name: "fn", Arguments: `{"k":"v"}`})
	s.SetMetadata("m1", 123)

	clone := s.Clone()

	if clone.ResponseContent != "content" {
		t.Errorf("ResponseContent: want %q, got %q", "content", clone.ResponseContent)
	}
	if clone.ReasoningContent != "reasoning" {
		t.Errorf("ReasoningContent: want %q, got %q", "reasoning", clone.ReasoningContent)
	}
	if clone.InputTokens != 42 {
		t.Errorf("InputTokens: want 42, got %d", clone.InputTokens)
	}
	if clone.OutputTokens != 84 {
		t.Errorf("OutputTokens: want 84, got %d", clone.OutputTokens)
	}
	if clone.StopReason != messages.StopReasonToolUse {
		t.Errorf("StopReason: want %q, got %q", messages.StopReasonToolUse, clone.StopReason)
	}
	calls := clone.GetToolCalls()
	if len(calls) != 1 || calls[0].ID != "a" {
		t.Errorf("ToolCalls: want [{ID:a}], got %v", calls)
	}
	if v, ok := clone.GetMetadata("m1"); !ok || v != 123 {
		t.Errorf("Metadata m1: want 123, got %v (ok=%v)", v, ok)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStreamState()
	var wg sync.WaitGroup
	n := 100

	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.AppendContent("x")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.AddToolCall(messages.ChatMessageToolCall{Name: "t"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.SetMetadata("k", i)
		}
	}()

	wg.Wait()

	if len(s.ResponseContent) != n {
		t.Errorf("expected content length %d, got %d", n, len(s.ResponseContent))
	}
	if len(s.GetToolCalls()) != n {
		t.Errorf("expected %d tool calls, got %d", n, len(s.GetToolCalls()))
	}
}
