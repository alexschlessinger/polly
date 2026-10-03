package llm

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentCompactNoOp(t *testing.T) {
	usage := reply("", 100).UsageRecord()
	for _, tc := range []struct {
		name    string
		history []messages.ChatMessage
	}{
		{"empty", nil},
		{"system only", []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "instructions"}}},
		{"internal only", []messages.ChatMessage{usage}},
		{"already summarized", []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "old request"}, reply("old answer", 0),
			messages.Compaction{Summary: "everything"}.Message(), usage,
		}},
		{"system after summary", []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "old request"},
			messages.Compaction{Summary: "everything"}.Message(),
			{Role: messages.MessageRoleSystem, Content: "new system instructions"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &compactionLLM{reply: func(*CompletionRequest, int) (messages.ChatMessage, error) {
				t.Fatal("a no-op called the provider")
				return messages.ChatMessage{}, nil
			}}
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			original := cloneMessages(tc.history)
			response, err := agent.Compact(context.Background(), &CompletionRequest{Messages: tc.history}, &AgentCallbacks{
				OnAdaptation:        func(RequestAdaptation) { t.Fatal("no-op reported an adaptation") },
				OnCompactionUsage:   func(string, UsageUpdate) { t.Fatal("no-op reported usage") },
				OnRequestProjection: func(int, ProjectionStats) { t.Fatal("no-op reported a projection") },
			})
			if err != nil || response == nil || response.Message != nil || len(response.AllMessages) != 0 || response.IterationCount != 0 || response.PersistedMessages != 0 || len(model.requests) != 0 {
				t.Fatalf("no-op: response=%+v error=%v requests=%d", response, err, len(model.requests))
			}
			if len(tc.history) != 0 && !reflect.DeepEqual(tc.history, original) {
				t.Fatal("no-op mutated history")
			}
		})
	}
}

func TestAgentCompactEntireHistoryBelowThreshold(t *testing.T) {
	for _, budget := range []int{0, 100_000} {
		t.Run(groupDigits(budget), func(t *testing.T) {
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "SYSTEM stays verbatim"},
				{Role: messages.MessageRoleUser, Content: "FIRST request " + strings.Repeat("a", 1_200)},
				callTool("old", "fetch", 0), resultOf("old", "fetch", "FULL TOOL OUTPUT "+strings.Repeat("b", 1_200)),
				reply("FIRST answer", 0),
				{Role: messages.MessageRoleUser, Content: "LATEST request " + strings.Repeat("c", 1_200)},
				reply("LATEST answer "+strings.Repeat("d", 1_200), 0),
			}
			history[1].Metadata = map[string]any{"nested": map[string]any{"key": "original"}}
			original := cloneMessages(history)
			model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
				if call > 0 {
					if isSummaryRequest(req) || len(req.Messages) != 3 || req.Messages[2].Content != "NEXT request" || !strings.Contains(req.Messages[1].Content, "SUMMARY covers all exchanges") {
						t.Errorf("future request = %+v", req.Messages)
					}
					return reply("next answer", 0), nil
				}
				if !isSummaryRequest(req) || len(req.Tools) != 0 || req.ResponseSchema != nil {
					t.Errorf("manual summary request = %+v", req)
				}
				transcript := req.Messages[1].Content
				for _, want := range []string{"FIRST request", "FIRST answer", "FULL TOOL OUTPUT", "LATEST request", "LATEST answer"} {
					if !strings.Contains(transcript, want) {
						t.Errorf("summary did not cover %q", want)
					}
				}
				if strings.Contains(transcript, "SYSTEM stays verbatim") {
					t.Error("summary paid for system instructions")
				}
				msg := reply("SUMMARY covers all exchanges", 1_337)
				msg.SetPromptCacheUsage(400, 30)
				msg.SetReportedCost(.25)
				return msg, nil
			}}
			registry := tools.NewToolRegistry(nil)
			defer registry.Close()
			agent := NewAgent(model, registry, AgentConfig{})
			defer agent.Close()
			cb := compactForbiddenCallbacks(t)
			var adaptations []RequestAdaptation
			cb.OnAdaptation = func(note RequestAdaptation) { adaptations = append(adaptations, note) }
			usageCalls, projectionCalls := 0, 0
			cb.OnCompactionUsage = func(model string, usage UsageUpdate) {
				usageCalls++
				if model != "" || usage.InputTokens != 1_337 || usage.OutputTokens != 10 || usage.CacheReadInputTokens != 400 || usage.CacheWriteInputTokens != 30 || !usage.CostReported || usage.ReportedCostUSD != .25 {
					t.Errorf("summary usage: model=%q usage=%+v", model, usage)
				}
			}
			cb.OnRequestProjection = func(iteration int, stats ProjectionStats) {
				projectionCalls++
				if iteration != 0 || stats.Budget != budget || stats.CountedTokens >= 1_200 || stats.RequestEstimatedTokens <= stats.EstimatedTokens {
					t.Errorf("accepted projection: iteration=%d stats=%+v", iteration, stats)
				}
			}
			response, err := agent.Compact(context.Background(), &CompletionRequest{Model: "main/model", Messages: history, MaxContextTokens: budget}, cb)
			if err != nil {
				t.Fatal(err)
			}
			if response.Message != nil || response.IterationCount != 0 || response.PersistedMessages != 0 || len(model.requests) != 1 || usageCalls != 1 || projectionCalls != 1 {
				t.Fatalf("manual result=%+v requests=%d usage=%d projections=%d", response, len(model.requests), usageCalls, projectionCalls)
			}
			if len(response.AllMessages) != 2 || !response.AllMessages[0].IsUsageRecord() {
				t.Fatalf("generated = %+v", response.AllMessages)
			}
			marker, ok := response.AllMessages[1].Compaction()
			if !ok || marker.Summary != "SUMMARY covers all exchanges" || marker.KeepsTurn || marker.ClearThrough != "" {
				t.Fatalf("manual marker = %+v", marker)
			}
			if usage := response.TokenUsage(); usage != (TokenUsage{TotalInput: 1_337, TotalOutput: 10, CacheRead: 400, CacheWrite: 30, ReportedCostUSD: .25}) {
				t.Fatalf("usage = %+v", usage)
			}
			if response.PromptCache != (PromptCacheStats{ReadInputTokens: 400, WriteInputTokens: 30}) {
				t.Fatalf("prompt cache = %+v", response.PromptCache)
			}
			if len(adaptations) != 1 || adaptations[0].Feature != FeatureCompaction || strings.Contains(adaptations[0].Message, "Context compacted") {
				t.Fatalf("adaptations announced success or omitted start: %+v", adaptations)
			}
			if !reflect.DeepEqual(history, original) {
				t.Fatal("manual compaction rewrote original history")
			}
			persisted := append(slices.Clone(history), response.AllMessages...)
			view := RequestView(persisted, agent.ToolRegistry().All())
			if len(view) != 2 || view[0].Content != history[0].Content || !strings.Contains(view[1].Content, marker.Summary) || !strings.Contains(view[1].Content, "read_transcript") {
				t.Fatalf("request view = %+v", view)
			}
			// A saved covering marker is a no-op on a repeated manual request.
			again, err := agent.Compact(context.Background(), &CompletionRequest{Messages: persisted}, cb)
			if err != nil || len(again.AllMessages) != 0 || len(model.requests) != 1 || projectionCalls != 1 {
				t.Fatalf("repeated compact: response=%+v error=%v", again, err)
			}
			persisted = append(persisted, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "NEXT request"})
			if _, err := agent.Run(context.Background(), &CompletionRequest{Model: "main/model", Messages: persisted, MaxContextTokens: budget}, nil); err != nil {
				t.Fatal(err)
			}
			reader, ok := agent.ToolRegistry().Get(BuiltinReadTranscript)
			if !ok {
				t.Fatal("transcript reader missing")
			}
			for _, query := range []string{"FIRST request", "FULL TOOL OUTPUT", "LATEST answer"} {
				text, err := reader.Execute(context.Background(), map[string]any{"query": query})
				if err != nil || !strings.Contains(text, query) {
					t.Fatalf("original %q not recalled: text=%q error=%v", query, text, err)
				}
			}
		})
	}
}

func TestAgentCompactExistingSummaryAndKeptTurn(t *testing.T) {
	for _, keepsTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "new exchange", true: "kept turn"}[keepsTurn], func(t *testing.T) {
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "COVERED original"}, reply("covered answer", 0),
			}
			latest := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "LATEST request " + strings.Repeat("a", 1_000)}, reply("LATEST answer", 0),
			}
			if keepsTurn {
				history = append(history, latest...)
			}
			history = append(history, messages.Compaction{Summary: "PRIOR summary", KeepsTurn: keepsTurn}.Message())
			if !keepsTurn {
				history = append(history, latest...)
			}
			model := &compactionLLM{reply: func(req *CompletionRequest, _ int) (messages.ChatMessage, error) {
				transcript := req.Messages[1].Content
				if !strings.Contains(transcript, "PRIOR summary") || !strings.Contains(transcript, "LATEST request") || !strings.Contains(transcript, "LATEST answer") || strings.Contains(transcript, "COVERED original") {
					t.Errorf("not the entire effective view: %s", transcript)
				}
				return reply("new covering summary", 0), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			response, err := agent.Compact(context.Background(), &CompletionRequest{Messages: history}, nil)
			if err != nil {
				t.Fatal(err)
			}
			marker, ok := response.AllMessages[len(response.AllMessages)-1].Compaction()
			if !ok || marker.KeepsTurn || marker.Summary != "new covering summary" {
				t.Fatalf("marker = %+v", marker)
			}
		})
	}
}

func TestAgentCompactModelFallbackAndSettings(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider failure", true: "refused billed summary"}[refused], func(t *testing.T) {
			caps := &ModelCapabilities{}
			req := &CompletionRequest{
				Model: "openai/main", ModelHost: "host", BaseURL: "https://example.test", APIKey: "request key",
				Timeout: 3 * time.Second, Deadline: 9 * time.Second, StreamMode: Buffered,
				MaxTokens: 1_000, MaxContextTokens: 50_000, ThinkingEffort: EffortLevel(LevelHigh), Temperature: Float32Ptr(.3), Fast: true,
				Capabilities: caps, CacheSessionID: "session", ResponseSchema: MustSchemaFor(struct{ Answer string }{}),
				Messages: messages.User(strings.Repeat("whole conversation ", 100)),
			}
			original := *req
			original.Messages = cloneMessages(req.Messages)
			model := &compactionLLM{reply: func(ask *CompletionRequest, call int) (messages.ChatMessage, error) {
				if !isSummaryRequest(ask) || ask.BaseURL != req.BaseURL || ask.Timeout != req.Timeout || ask.Deadline != req.Deadline || ask.StreamMode != Buffered || len(ask.Tools) != 0 || ask.ResponseSchema != nil {
					t.Errorf("summary endpoint or shape = %+v", ask)
				}
				if call == 0 {
					if ask.Model != "cheap/summary" || ask.APIKey != "" || ask.ModelHost != "" || ask.Capabilities != nil || ask.MaxTokens != 0 || ask.ThinkingEffort.IsEnabled() || ask.Temperature != nil || ask.Fast || ask.CacheSessionID != "" {
						t.Errorf("compaction model inherited request-specific settings: %+v", ask)
					}
					if refused {
						return reply("", 100), nil
					}
					return messages.ChatMessage{}, errors.New("compaction model unavailable")
				}
				if ask.Model != req.Model || ask.APIKey != req.APIKey || ask.ModelHost != req.ModelHost || ask.Capabilities != caps || ask.MaxTokens != req.MaxTokens || ask.ThinkingEffort != req.ThinkingEffort || ask.Temperature != req.Temperature || !ask.Fast || ask.CacheSessionID != req.CacheSessionID {
					t.Errorf("fallback lost request settings: %+v", ask)
				}
				return reply("fallback summary", 200), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{CompactionModel: "cheap/summary"})
			defer agent.Close()
			var notes []RequestAdaptation
			var usageModels []string
			response, err := agent.Compact(context.Background(), req, &AgentCallbacks{
				OnAdaptation:      func(note RequestAdaptation) { notes = append(notes, note) },
				OnCompactionUsage: func(model string, _ UsageUpdate) { usageModels = append(usageModels, model) },
			})
			if err != nil || len(model.requests) != 2 || !slices.ContainsFunc(notes, func(note RequestAdaptation) bool {
				return note.Feature == FeatureCompactionFailure && strings.Contains(note.Message, "summarizing with openai/main")
			}) {
				t.Fatalf("fallback: response=%+v error=%v notes=%+v", response, err, notes)
			}
			if !reflect.DeepEqual(req, &original) {
				t.Fatal("manual compaction changed request settings or history")
			}
			want := TokenUsage{TotalInput: 200, TotalOutput: 10}
			wantModels := []string{""}
			if refused {
				want.TotalInput += 100
				want.TotalOutput += 10
				want.Compaction = CompactionUsage{Model: "cheap/summary", Input: 100, Output: 10}
				wantModels = []string{"cheap/summary", ""}
			}
			if usage := response.TokenUsage(); usage != want || !reflect.DeepEqual(usageModels, wantModels) {
				t.Fatalf("usage = %+v models=%q; want %+v models=%q", usage, usageModels, want, wantModels)
			}
		})
	}
}

func TestAgentCompactDoesNotExecuteTools(t *testing.T) {
	for _, modelCallsTool := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete historical batch", true: "summary calls a tool"}[modelCallsTool], func(t *testing.T) {
			invoked := 0
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "effect", Run: func(context.Context, tools.Args) (string, error) {
				invoked++
				return "effect", nil
			}}})
			defer registry.Close()
			model := &compactionLLM{reply: func(req *CompletionRequest, _ int) (messages.ChatMessage, error) {
				if len(req.Tools) != 0 || !strings.Contains(req.Messages[1].Content, "tool call effect") {
					t.Errorf("unfinished tool history not rendered without tools: %+v", req)
				}
				if modelCallsTool {
					msg := callTool("new", "effect", 100)
					msg.Content = "not a summary"
					return msg, nil
				}
				return reply("summary of unfinished work", 100), nil
			}}
			agent := NewAgent(model, registry, AgentConfig{})
			defer agent.Close()
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: strings.Repeat("unfinished task ", 100)},
				callTool("pending", "effect", 0),
			}
			response, err := agent.Compact(context.Background(), &CompletionRequest{Messages: history}, compactForbiddenCallbacks(t))
			if invoked != 0 || len(model.requests) != 1 || response.TokenUsage().TotalInput != 100 || (err != nil) != modelCallsTool {
				t.Fatalf("tools=%d requests=%d response=%+v error=%v", invoked, len(model.requests), response, err)
			}
			if modelCallsTool {
				assertCompactNoMarker(t, response)
			} else if len(response.AllMessages) != 2 {
				t.Fatalf("unexpected generated history: %+v", response.AllMessages)
			}
		})
	}
}

func TestAgentCompactFailuresRetainUsageWithoutMarker(t *testing.T) {
	failure := errors.New("provider unavailable")
	for _, tc := range []struct {
		name  string
		reply messages.ChatMessage
		err   error
		usage int
	}{
		{"provider error", messages.ChatMessage{}, failure, 0},
		{"empty summary", reply("", 100), nil, 100},
		{"cut off summary", func() messages.ChatMessage {
			m := reply("partial summary", 100)
			m.StopReason = messages.StopReasonMaxTokens
			return m
		}(), nil, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &compactionLLM{reply: func(*CompletionRequest, int) (messages.ChatMessage, error) { return tc.reply, tc.err }}
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			response, err := agent.Compact(context.Background(), &CompletionRequest{Messages: messages.User(strings.Repeat("history ", 100))}, &AgentCallbacks{
				OnRequestProjection: func(int, ProjectionStats) { t.Fatal("failed summary reported an accepted projection") },
			})
			if err == nil || tc.err != nil && !errors.Is(err, tc.err) || response.TokenUsage().TotalInput != tc.usage {
				t.Fatalf("failure: response=%+v usage=%+v error=%v", response, response.TokenUsage(), err)
			}
			assertCompactNoMarker(t, response)
		})
	}
}

func TestAgentCompactWithdrawsOversizeAndNonShrinkingSummaries(t *testing.T) {
	window := 1_000
	for _, tc := range []struct {
		name    string
		budget  int
		caps    *ModelCapabilities
		learned int
		want    string
	}{
		{"explicit budget", 300, nil, 0, "context budget"},
		{"model window budget", 100_000, &ModelCapabilities{ContextTokens: &window}, 0, "context budget"},
		{"learned budget", 100_000, nil, 400, "context budget"},
		{"nonshrinking unlimited", 0, nil, 0, "did not reduce"},
		{"nonshrinking below threshold", 100_000, nil, 0, "did not reduce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &compactionLLM{reply: func(*CompletionRequest, int) (messages.ChatMessage, error) {
				return reply(strings.Repeat("long summary ", 600), 300), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			req := &CompletionRequest{Model: "main/model", Messages: messages.User(strings.Repeat("history ", 100)), MaxContextTokens: tc.budget, MaxTokens: 128, Capabilities: tc.caps}
			if tc.learned > 0 {
				agent.limits = map[ModelTarget]contextLimit{routeOf(req): {budget: tc.learned}}
			}
			response, err := agent.Compact(context.Background(), req, &AgentCallbacks{
				OnRequestProjection: func(int, ProjectionStats) { t.Fatal("withdrawn summary reported an accepted projection") },
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) || response.TokenUsage().TotalInput != 300 {
				t.Fatalf("withdrawal: response=%+v error=%v", response, err)
			}
			assertCompactNoMarker(t, response)
			if strings.Contains(agent.renderedTranscript(), "long summary") {
				t.Fatal("withdrawn summary remained in recall")
			}
		})
	}
}

func TestAgentCompactCancellation(t *testing.T) {
	for _, stage := range []string{"before request", "before provider", "after usage", "after projection"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before request" {
				cancel()
			}
			model := &compactionLLM{reply: func(*CompletionRequest, int) (messages.ChatMessage, error) {
				return reply("summary", 100), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{CompactionModel: "other/model"})
			defer agent.Close()
			response, err := agent.Compact(ctx, &CompletionRequest{Messages: messages.User(strings.Repeat("history ", 100))}, &AgentCallbacks{
				OnAdaptation: func(note RequestAdaptation) {
					if stage == "before provider" && note.Feature == FeatureCompaction {
						cancel()
					}
				},
				OnCompactionUsage: func(string, UsageUpdate) {
					if stage == "after usage" {
						cancel()
					}
				},
				OnRequestProjection: func(int, ProjectionStats) {
					if stage == "after projection" {
						cancel()
					}
				},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
			assertCompactNoMarker(t, response)
			wantUsage := 0
			if stage == "after usage" || stage == "after projection" {
				wantUsage = 100
			}
			if response.TokenUsage().TotalInput != wantUsage || len(model.requests) > 1 {
				t.Fatalf("cancellation usage=%+v requests=%d", response.TokenUsage(), len(model.requests))
			}
			if (stage == "before request" || stage == "before provider") && len(model.requests) != 0 {
				t.Fatal("canceled request called provider")
			}
		})
	}
}

type compactWaitingLLM struct {
	entered chan struct{}
	stopped chan struct{}
}

func (m *compactWaitingLLM) ChatCompletionStream(ctx context.Context, _ *CompletionRequest, _ EventStreamProcessor) <-chan *messages.StreamEvent {
	events := make(chan *messages.StreamEvent)
	close(m.entered)
	go func() {
		<-ctx.Done()
		close(events)
		close(m.stopped)
	}()
	return events
}

func TestAgentCompactCancelsInFlightSummaryWithoutFallback(t *testing.T) {
	model := &compactWaitingLLM{entered: make(chan struct{}), stopped: make(chan struct{})}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "other/model"})
	defer agent.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		response *AgentResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := agent.Compact(ctx, &CompletionRequest{Messages: messages.User(strings.Repeat("history ", 100))}, nil)
		done <- outcome{response, err}
	}()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("summary did not reach the provider")
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("in-flight cancellation = %v", result.err)
		}
		assertCompactNoMarker(t, result.response)
		if len(result.response.AllMessages) != 0 {
			t.Fatalf("unfinished summary generated messages: %+v", result.response.AllMessages)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Compact ignored cancellation")
	}
	select {
	case <-model.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("summary producer did not shut down")
	}
}

func TestAgentCompactIgnoresResponseToolRequirement(t *testing.T) {
	unsupported := false
	model := &compactionLLM{reply: func(*CompletionRequest, int) (messages.ChatMessage, error) { return reply("summary", 0), nil }}
	agent := NewAgent(model, nil, AgentConfig{ResponseTool: "deliver", RequireResponseToolSuccess: true})
	defer agent.Close()
	response, err := agent.Compact(context.Background(), &CompletionRequest{
		Messages: messages.User(strings.Repeat("completed history ", 100)), Capabilities: &ModelCapabilities{Tools: &unsupported},
	}, nil)
	if err != nil || len(response.AllMessages) != 2 {
		t.Fatalf("response-tool policy constrained manual summary: response=%+v error=%v", response, err)
	}
}

func assertCompactNoMarker(t *testing.T, response *AgentResponse) {
	t.Helper()
	if response == nil || response.Message != nil || response.IterationCount != 0 || response.PersistedMessages != 0 {
		t.Fatalf("unexpected manual result: %+v", response)
	}
	for _, msg := range response.AllMessages {
		if _, ok := msg.Compaction(); ok || !msg.IsUsageRecord() {
			t.Fatalf("failed compaction returned more than usage: %+v", response.AllMessages)
		}
	}
}

func compactForbiddenCallbacks(t *testing.T) *AgentCallbacks {
	t.Helper()
	unexpected := func() { t.Helper(); t.Fatal("manual compaction invoked an unsupported callback") }
	return &AgentCallbacks{
		ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			unexpected()
			return nil, nil
		},
		AdmitInput:       func(context.Context) ([]messages.ChatMessage, error) { unexpected(); return nil, nil },
		Checkpoint:       func(context.Context, AgentCheckpoint) error { unexpected(); return nil },
		BeforeToolBatch:  func(context.Context, []messages.ChatMessageToolCall) error { unexpected(); return nil },
		JournalToolBatch: func(context.Context, AgentCheckpoint) error { unexpected(); return nil },
		AfterToolBatch:   func(context.Context) error { unexpected(); return nil },
		OnReasoning:      func(string) { unexpected() },
		OnCommentary:     func(messages.AssistantText, bool) { unexpected() },
		OnContent:        func(string) { unexpected() },
		BeforeToolExecute: func(ctx context.Context, _ messages.ChatMessageToolCall, _ map[string]any) context.Context {
			unexpected()
			return ctx
		},
		OnToolStart:      func([]messages.ChatMessageToolCall) { unexpected() },
		ApproveToolCalls: func(context.Context, []messages.ChatMessageToolCall) ([]bool, error) { unexpected(); return nil, nil },
		OnToolEnd:        func(messages.ChatMessageToolCall, string, time.Duration, error) { unexpected() },
		OnToolResult:     func(messages.ChatMessageToolCall, messages.ChatMessage) { unexpected() },
		BeforeFirstRequest: func(ProjectionStats) error {
			unexpected()
			return nil
		},
		OnModelRequest:   func(int, int) { unexpected() },
		OnStreamActivity: func(int, int) { unexpected() },
		OnIterationUsage: func(int, int, int) { unexpected() },
		OnUsageProgress:  func(UsageUpdate) { unexpected() },
		OnComplete:       func(*messages.ChatMessage) { unexpected() },
		OnError:          func(error) { unexpected() },
	}
}
