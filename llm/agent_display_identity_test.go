package llm

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentDisplayIdentityAcrossObserversAndCheckpoints(t *testing.T) {
	// A provider can repeat its own IDs, or even return the same backing
	// containers. Host identities must be fresh without rewriting protocol IDs.
	toolReply := messages.ChatMessage{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		TextBlocks: []messages.AssistantText{{ID: "provider-text", Phase: messages.PhaseCommentary, Text: "checking"}},
		ToolCalls:  []messages.ChatMessageToolCall{{ID: "provider-call", Name: "work", Arguments: `{}`, DisplayID: "stale-call"}},
		Metadata:   map[string]any{messages.MetadataKeyDisplayID: "stale-message"},
	}
	model := &sequentialLLM{responses: []messages.ChatMessage{toolReply, toolReply}}
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { return "result", nil }}})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{MaxIterations: 3})
	defer agent.Close()
	input := messages.User("admitted input")
	var observed, persisted []messages.ChatMessage
	var starts, callIDs []string
	var current string
	admitted, journals, results := false, 0, 0
	response, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
			if admitted {
				return nil, nil
			}
			admitted = true
			return input, nil
		},
		OnMessageStart: func(id string) {
			if id == "" {
				t.Fatal("empty attempt identity")
			}
			current = id
			starts = append(starts, id)
		},
		OnCommentary: func(text messages.AssistantText, _ bool) {
			if current == "" || text.ID != "provider-text" {
				t.Fatalf("commentary before start or protocol ID changed: %q %#v", current, text)
			}
		},
		OnContent: func(string) {
			if current == "" {
				t.Fatal("content before start")
			}
		},
		OnMessage: func(index int, msg messages.ChatMessage) {
			if index != len(observed) || msg.DisplayID() == "" {
				t.Fatalf("unidentified/out-of-order message %d: %#v", index, msg)
			}
			if msg.Role == messages.MessageRoleAssistant && msg.DisplayID() != current {
				t.Fatalf("attempt/accepted identity mismatch: %q != %q", msg.DisplayID(), current)
			}
			if index == 0 && (len(persisted) != 1 || persisted[0].DisplayID() != msg.DisplayID()) {
				t.Fatal("admitted input was not stamped before checkpoint")
			}
			observed = append(observed, msg.Clone())
			// The observer must not be able to corrupt accepted identity.
			msg.Metadata[messages.MetadataKeyDisplayID] = "observer-mutation"
			if len(msg.ToolCalls) > 0 {
				msg.ToolCalls[0].DisplayID = "observer-mutation"
			}
		},
		Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
			for i, msg := range cp.Generated {
				if msg.DisplayID() == "" {
					t.Fatalf("checkpoint message %d has no identity", i)
				}
				if i < len(observed) && !reflect.DeepEqual(msg, observed[i]) {
					t.Fatalf("observer/checkpoint mismatch at %d: %#v != %#v", i, msg, observed[i])
				}
			}
			persisted = cloneMessages(cp.Generated)
			return nil
		},
		JournalToolBatch: func(_ context.Context, cp AgentCheckpoint) error {
			journals++
			last := cp.Generated[len(cp.Generated)-1]
			if !reflect.DeepEqual(last, observed[len(observed)-1]) || last.ToolCalls[0].DisplayID == "" {
				t.Fatalf("journal identity mismatch: %#v", last)
			}
			return nil
		},
		OnToolStart: func(calls []messages.ChatMessageToolCall) {
			call := calls[0]
			if call.ID != "provider-call" || call.DisplayID == "" || call.DisplayID == "stale-call" || journals != len(callIDs)+1 {
				t.Fatalf("tool start identity/journal: %#v", call)
			}
			callIDs = append(callIDs, call.DisplayID)
		},
		OnToolResult: func(call messages.ChatMessageToolCall, result messages.ChatMessage) {
			results++
			if result.DisplayID() != call.DisplayID || result.ToolCallID != call.ID {
				t.Fatalf("live result identity mismatch: %#v %#v", call, result)
			}
			result.Metadata[messages.MetadataKeyDisplayID] = "result-observer-mutation"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 3 || len(callIDs) != 2 || results != 2 || len(observed) != 6 {
		t.Fatalf("starts=%v calls=%v results=%d messages=%d", starts, callIDs, results, len(observed))
	}
	seen := map[string]bool{}
	for _, id := range append(starts, callIDs...) {
		if seen[id] {
			t.Fatalf("repeated host identity %q", id)
		}
		seen[id] = true
	}
	if !reflect.DeepEqual(response.AllMessages, observed) || !reflect.DeepEqual(persisted, observed) {
		t.Fatal("accepted identities changed before response/checkpoint")
	}
	for _, i := range []int{1, 3} {
		call, result := response.AllMessages[i].ToolCalls[0], response.AllMessages[i+1]
		if result.DisplayID() != call.DisplayID || call.ID != "provider-call" || result.ToolCallID != call.ID || response.AllMessages[i].TextBlocks[0].ID != "provider-text" {
			t.Fatal("display/protocol pair identity changed")
		}
	}
	if input[0].DisplayID() != "" || toolReply.DisplayID() != "stale-message" || toolReply.ToolCalls[0].DisplayID != "stale-call" {
		t.Fatal("stamping mutated caller/provider messages")
	}
}

func TestAgentDisplayIdentityFreshAttemptsAndUsage(t *testing.T) {
	for _, reportUsage := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing usage", true: "reported usage"}[reportUsage], func(t *testing.T) {
			calls := 0
			model := retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
				calls++
				if calls == 1 {
					events := []*messages.StreamEvent{{Type: messages.EventTypeReasoning, Content: "provisional"}}
					if reportUsage {
						events = append(events, &messages.StreamEvent{Type: messages.EventTypeUsage, InputTokens: 2, OutputTokens: 1})
					}
					return retryEvents(append(events, &messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF})...)
				}
				return retryEvents(retryAnswer())
			})
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			var starts []string
			var observed []messages.ChatMessage
			response, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(1)}, &AgentCallbacks{
				OnMessageStart: func(id string) { starts = append(starts, id) },
				OnReasoning: func(string) {
					if len(starts) != 1 {
						t.Fatal("reasoning before attempt start")
					}
				},
				OnAttemptAborted: func(int, int, error) {},
				OnMessage: func(index int, msg messages.ChatMessage) {
					if index != len(observed) || msg.DisplayID() == "" {
						t.Fatalf("message %d identity = %q", index, msg.DisplayID())
					}
					observed = append(observed, msg)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantMessages := 1
			if reportUsage {
				wantMessages++
			}
			if len(starts) != 2 || starts[0] == starts[1] || response.Message.DisplayID() != starts[1] || len(observed) != wantMessages {
				t.Fatalf("attempt identity/usage: starts=%v response=%#v observed=%#v", starts, response.Message, observed)
			}
			if reportUsage && (!observed[0].IsUsageRecord() || observed[0].GetInputTokens() != 2) {
				t.Fatalf("failed usage missing: %#v", observed)
			}
		})
	}
}

func TestAgentDisplayIdentityFreshForSameAttemptCounters(t *testing.T) {
	// Overflow recovery can start another stream with the same iteration and
	// attempt counters. Identity belongs to the actual attempt, not counters.
	reply := messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "repeated", Name: "work"}}}
	agent := NewAgent(&sequentialLLM{responses: []messages.ChatMessage{reply, reply}}, nil, AgentConfig{})
	defer agent.Close()
	var starts []string
	cb := &AgentCallbacks{OnMessageStart: func(id string) { starts = append(starts, id) }}
	first, _, _, err := agent.processAttempt(context.Background(), &CompletionRequest{}, cb, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, err := agent.processAttempt(context.Background(), &CompletionRequest{}, cb, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 2 || starts[0] == starts[1] || first.DisplayID() != starts[0] || second.DisplayID() != starts[1] || first.ToolCalls[0].DisplayID == second.ToolCalls[0].DisplayID {
		t.Fatalf("actual attempts reused identity: %#v %#v starts=%v", first, second, starts)
	}
}

func TestAgentDisplayIdentityDeniedAndInterruptedResults(t *testing.T) {
	for _, mode := range []string{"denied", "cancelled", "journal failure", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reply := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "provider-call", Name: "work", Arguments: `{}`}}}
			if mode == "truncated" {
				reply.StopReason = messages.StopReasonMaxTokens
			}
			agent := NewAgent(&sequentialLLM{responses: []messages.ChatMessage{reply}}, nil, AgentConfig{})
			defer agent.Close()
			var observed []messages.ChatMessage
			cb := &AgentCallbacks{OnMessage: func(_ int, msg messages.ChatMessage) { observed = append(observed, msg) }}
			var wantErr error
			switch mode {
			case "denied":
				cb.ApproveToolCalls = func(context.Context, []messages.ChatMessageToolCall) ([]bool, error) { return []bool{false}, nil }
			case "cancelled":
				cb.OnToolStart = func([]messages.ChatMessageToolCall) { cancel() }
				wantErr = context.Canceled
			case "journal failure":
				wantErr = errors.New("journal rejected")
				cb.JournalToolBatch = func(context.Context, AgentCheckpoint) error { return wantErr }
			}
			response, err := agent.Run(ctx, &CompletionRequest{}, cb)
			if !errors.Is(err, wantErr) {
				t.Fatalf("err=%v want=%v", err, wantErr)
			}
			if len(observed) != 2 || !reflect.DeepEqual(observed, response.AllMessages) {
				t.Fatalf("missing result observation: %#v", observed)
			}
			call, result := observed[0].ToolCalls[0], observed[1]
			if call.DisplayID == "" || call.DisplayID != result.DisplayID() || call.ID != result.ToolCallID {
				t.Fatalf("stub identity mismatch: %#v %#v", call, result)
			}
			wantContent := ToolInterruptedContent
			if mode == "denied" {
				wantContent = ToolDeniedContent
			}
			if result.Content != wantContent {
				t.Fatalf("result=%q want=%q", result.Content, wantContent)
			}
		})
	}
}

func TestAgentMessageObservationDoesNotRequireDurability(t *testing.T) {
	agent := NewAgent(&sequentialLLM{}, nil, AgentConfig{})
	defer agent.Close()
	var observed messages.ChatMessage
	persistErr := errors.New("not durable")
	response, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		OnMessage: func(_ int, msg messages.ChatMessage) { observed = msg },
		Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
			if cp.Final {
				return persistErr
			}
			return nil
		},
	})
	if !errors.Is(err, persistErr) || observed.DisplayID() == "" || observed.DisplayID() != response.Message.DisplayID() || response.PersistedMessages != 0 {
		t.Fatalf("acceptance/durability conflated: response=%#v observed=%#v err=%v", response, observed, err)
	}
}

func TestStripDeniedExchangesUsesDisplayIdentity(t *testing.T) {
	for _, legacyFirst := range []bool{false, true} {
		firstCall := messages.ChatMessageToolCall{ID: "repeated", Name: "work", DisplayID: "successful"}
		firstResult := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "repeated", Content: "success", Metadata: map[string]any{messages.MetadataKeyDisplayID: "successful"}}
		if legacyFirst {
			firstCall.DisplayID = ""
			firstResult.Metadata = nil
		}
		msgs := []messages.ChatMessage{
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{firstCall}}, firstResult,
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "repeated", Name: "work", DisplayID: "denied"}}},
			{Role: messages.MessageRoleTool, ToolCallID: "repeated", Content: ToolDeniedContent, Metadata: map[string]any{messages.MetadataKeyDisplayID: "denied"}},
		}
		got := StripDeniedExchanges(msgs)
		if len(got) != 3 || !reflect.DeepEqual(got[:2], msgs[:2]) || !reflect.DeepEqual(got[2], msgs[2].UsageRecord()) {
			t.Fatalf("later denial changed earlier success or lost usage (legacy=%v): %#v", legacyFirst, got)
		}
		if len(msgs[2].ToolCalls) != 1 {
			t.Fatal("filter mutated original calls")
		}
	}
}
