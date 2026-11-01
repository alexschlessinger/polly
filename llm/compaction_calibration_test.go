package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestCompactionRebasesCalibrationAfterBudgetReduction(t *testing.T) {
	const summary = "The user supplied a long passage; the task is complete."
	for _, manual := range []bool{false, true} {
		for _, destination := range []string{"same model", "changed model", "changed endpoint"} {
			name := map[bool]string{false: "automatic", true: "manual"}[manual] + "/" + destination
			t.Run(name, func(t *testing.T) {
				model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
					if call == 0 {
						msg := reply("ok", 0)
						msg.SetTokenUsage(100_004, 1)
						return msg, nil
					}
					if isSummaryRequest(req) {
						return reply(summary, 100), nil
					}
					if text := projectedText(req.Messages); !strings.Contains(text, summary) || strings.Contains(text, strings.Repeat("你", 100)) {
						t.Errorf("continuation did not replace the old passage: %.200q", text)
					}
					return reply("continued", 0), nil
				}}
				agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
				defer agent.Close()
				req := &CompletionRequest{
					Model: "openai/large", BaseURL: "https://first.example/v1", MaxContextTokens: 200_000,
					Messages: messages.User(strings.Repeat("你", 100_000)),
				}
				first, err := agent.Run(context.Background(), req, nil)
				if err != nil {
					t.Fatal(err)
				}
				if estimate, ok := first.Message.RequestEstimate(); !ok || estimate != 75_004 {
					t.Fatalf("recorded estimate = %d, %v; want 75004", estimate, ok)
				}
				history := append(slices.Clone(req.Messages), first.AllMessages...)
				// Calibration and markers must also work after durable persistence.
				encoded, err := json.Marshal(history)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &history); err != nil {
					t.Fatal(err)
				}
				original := cloneMessages(history)
				req.Messages, req.MaxContextTokens = history, 24_576
				switch destination {
				case "changed model":
					req.Model = "openai/small"
				case "changed endpoint":
					req.BaseURL = "https://second.example/v1"
				}
				var compacted *AgentResponse
				if manual {
					compacted, err = agent.Compact(context.Background(), req, nil)
				} else {
					req.Messages = append(slices.Clone(history), messages.User("continue")...)
					compacted, err = agent.Run(context.Background(), req, nil)
				}
				if err != nil {
					t.Fatalf("budget reduction: %v (provider calls = %d)", err, len(model.requests))
				}
				want := 43 // the summary plus the fresh "continue" request
				if manual {
					want = 37 // only the covering summary
				}
				if stats := compacted.Projection; stats.Counted || stats.CountedTokens != want || stats.RequestEstimatedTokens != want {
					t.Fatalf("compacted projection retains removed-prefix bias: %+v; want %d", stats, want)
				}
				markers := 0
				for _, msg := range compacted.AllMessages {
					if c, ok := msg.Compaction(); ok {
						markers++
						if c.Summary != summary || c.KeepsTurn == manual {
							t.Fatalf("compaction marker = %+v", c)
						}
					}
				}
				if markers != 1 || !reflect.DeepEqual(history, original) {
					t.Fatalf("markers = %d or original history was rewritten", markers)
				}
				if manual {
					req.Messages = append(slices.Clone(history), compacted.AllMessages...)
					req.Messages = append(req.Messages, messages.User("continue")...)
					if _, err := agent.Run(context.Background(), req, nil); err != nil {
						t.Fatalf("continuation after manual compaction: %v", err)
					}
				}
				if len(model.requests) != 3 {
					t.Fatalf("provider calls = %d, want initial, summary and continuation", len(model.requests))
				}
				if !strings.Contains(agent.renderedTranscript(), original[0].Content) {
					t.Fatal("the original passage is missing from transcript recall")
				}
			})
		}
	}
}

func TestCompactionDoesNotDiscountReplacementWithRemovedPrefix(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
			model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
				if call == 0 {
					return reply("ok", 1_004), nil
				}
				if !isSummaryRequest(req) {
					t.Error("an over-budget summary was sent as a continuation")
				}
				return reply(strings.Repeat("summary ", 1_500), 100), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
			defer agent.Close()
			req := &CompletionRequest{Model: "openai/model", Messages: messages.User(strings.Repeat("x", 40_000)), MaxContextTokens: 20_000}
			first, err := agent.Run(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Messages = append(req.Messages, first.AllMessages...)
			req.MaxContextTokens = 500
			var response *AgentResponse
			var observed []messages.ChatMessage
			if manual {
				response, err = agent.Compact(context.Background(), req, nil)
			} else {
				req.Messages = append(req.Messages, messages.User("continue")...)
				response, err = agent.Run(context.Background(), req, &AgentCallbacks{
					OnMessage: func(index int, msg messages.ChatMessage) {
						if index != len(observed) {
							t.Fatalf("reused generated index %d after %d observations", index, len(observed))
						}
						observed = append(observed, msg)
					},
				})
				if !reflect.DeepEqual(observed, response.AllMessages) {
					t.Fatalf("missed surviving messages after budget error: observed=%+v generated=%+v", observed, response.AllMessages)
				}
			}
			var limit *ContextLimitError
			if !errors.As(err, &limit) || limit.EstimatedTokens <= req.MaxContextTokens || len(model.requests) != 2 {
				t.Fatalf("oversized replacement: error=%v calls=%d", err, len(model.requests))
			}
			if manual {
				assertCompactNoMarker(t, response)
			}
			if response.TokenUsage().TotalInput != 100 {
				t.Fatalf("summary usage was lost: %+v", response.TokenUsage())
			}
		})
	}
}

func TestCompactionCannotReduceCalibrationByGrowingTheRequest(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
			model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
				if call == 0 {
					return reply("ok", 10_000), nil
				}
				if !isSummaryRequest(req) {
					t.Error("a growing summary was sent as a continuation")
				}
				return reply(strings.Repeat("summary ", 1_000), 100), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
			defer agent.Close()
			req := &CompletionRequest{Model: "openai/model", Messages: messages.User(strings.Repeat("x", 4_000)), MaxContextTokens: 20_000}
			first, err := agent.Run(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Messages = append(req.Messages, first.AllMessages...)
			req.MaxContextTokens = 3_000
			var response *AgentResponse
			var observed []messages.ChatMessage
			if manual {
				response, err = agent.Compact(context.Background(), req, nil)
			} else {
				req.Messages = append(req.Messages, messages.User("continue")...)
				response, err = agent.Run(context.Background(), req, &AgentCallbacks{
					OnMessage: func(index int, msg messages.ChatMessage) {
						if index != len(observed) {
							t.Fatalf("reused generated index %d after %d observations", index, len(observed))
						}
						observed = append(observed, msg)
					},
				})
				if !reflect.DeepEqual(observed, response.AllMessages) {
					t.Fatalf("observed nonshrinking summary after withdrawal: observed=%+v generated=%+v", observed, response.AllMessages)
				}
			}
			if err == nil || !strings.Contains(err.Error(), "did not reduce") || len(model.requests) != 2 {
				t.Fatalf("growing summary: error=%v calls=%d", err, len(model.requests))
			}
			assertCompactNoMarker(t, response)
		})
	}
}

func TestCalibrationDoesNotCrossRequestRoutes(t *testing.T) {
	for _, change := range []string{"unchanged", "credential", "equivalent capabilities", "model", "endpoint", "host", "vision capability", "tool capability"} {
		t.Run(change, func(t *testing.T) {
			model := &compactionLLM{reply: func(*CompletionRequest, int) (messages.ChatMessage, error) {
				return reply("ok", 1_000), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
			defer agent.Close()
			req := &CompletionRequest{Model: "openai/one", BaseURL: "https://user:password@first.example/v1?token=secret", APIKey: "private-key", ModelHost: "one", Messages: messages.User(strings.Repeat("x", 6_000))}
			first, err := agent.Run(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := json.Marshal(first.Message.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{req.BaseURL, "password", "secret", req.APIKey} {
				if strings.Contains(string(metadata), secret) {
					t.Fatalf("calibration persisted endpoint credentials: %s", metadata)
				}
			}
			req.Messages = append(req.Messages, first.AllMessages...)
			req.Messages = append(req.Messages, messages.User("continue")...)
			switch change {
			case "credential":
				req.APIKey = "rotated-key"
			case "model":
				req.Model = "openai/two"
			case "endpoint":
				req.BaseURL = "https://second.example/v1"
			case "host":
				req.ModelHost = "two"
			case "equivalent capabilities":
				yes := true
				req.Capabilities = &ModelCapabilities{InputModalities: []string{"text", "image"}, Tools: &yes}
			case "vision capability":
				req.Capabilities = &ModelCapabilities{InputModalities: []string{"text"}}
			case "tool capability":
				no := false
				req.Capabilities = &ModelCapabilities{Tools: &no}
			}
			second, err := agent.Run(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			stats := second.Projection
			if change == "unchanged" || change == "credential" || change == "equivalent capabilities" {
				want := 1_000 + stats.RequestEstimatedTokens - first.Projection.RequestEstimatedTokens
				if !stats.Counted || stats.CountedTokens != want {
					t.Fatalf("unchanged request lost useful calibration: %+v, want %d", stats, want)
				}
			} else if stats.Counted || stats.CountedTokens != stats.RequestEstimatedTokens {
				t.Fatalf("%s change retained another route's calibration: %+v", change, stats)
			}
		})
	}
}

func TestCompactionRebasesOverflowCalibrationInFinalValidation(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if call == 0 {
			return messages.ChatMessage{}, &ContextOverflowError{Input: 18_000, Window: 10_000}
		}
		if !isSummaryRequest(req) {
			t.Error("a request above the learned count was sent")
		}
		return reply(strings.Repeat("x", 14_000), 100), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
	defer agent.Close()
	req := &CompletionRequest{Model: "openai/model", MaxContextTokens: 30_000, MaxTokens: 1_000, Messages: []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("x", 12_000)},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("x", 24_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "continue"},
	}}
	response, err := agent.Run(context.Background(), req, nil)
	var limit *ContextLimitError
	if !errors.As(err, &limit) || limit.Limit != 9_000 || limit.EstimatedTokens <= limit.Limit || len(model.requests) != 2 {
		t.Fatalf("overflow validation: error=%v calls=%d", err, len(model.requests))
	}
	if stats := response.Projection; !stats.Counted || stats.RequestEstimatedTokens >= stats.Budget || stats.CountedTokens <= stats.Budget {
		t.Fatalf("overflow's retained system calibration was lost: %+v", stats)
	}
}

func TestCalibrationRetainsSystemsToolsAndKeptTurns(t *testing.T) {
	for _, prior := range []string{"plain", "summary", "clear"} {
		t.Run(prior, func(t *testing.T) {
			req := &CompletionRequest{Model: "test/model", Tools: []tools.Tool{&tools.Func{Name: "fetch"}}}
			p, estimate := projectionToolsFor(req.Tools), messageEstimator(req)
			history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "unchanged system"}}
			history = append(history, messages.User(strings.Repeat("old ", 1_000))...)
			history = append(history, callTool("old", "fetch", 0), resultOf("old", "fetch", strings.Repeat("result ", 500)), reply("old answer", 0))
			switch prior {
			case "summary":
				history = append(history, messages.Compaction{Summary: "previous summary"}.Message())
			case "clear":
				history = append(history, messages.Compaction{ClearThrough: "old"}.Message())
			}
			fixed := estimate(history[0]) + estimateToolSchemaTokens(req.Tools)
			turn := messages.User(strings.Repeat("current ", 100))[0]
			history = append(history, turn)
			sent := estimateToolSchemaTokens(req.Tools)
			for _, msg := range contextView(history, p) {
				sent += estimate(msg)
			}
			response := reply("current answer", 2*sent)
			response.SetRequestEstimate(sent)
			history = append(history, response)
			for _, keepsTurn := range []bool{true, false} {
				history = append(history, messages.Compaction{Summary: "replacement summary", KeepsTurn: keepsTurn}.Message())
				current := estimateToolSchemaTokens(req.Tools)
				for _, msg := range contextView(history, p) {
					current += estimate(msg)
				}
				retained := fixed
				if keepsTurn {
					retained += estimate(turn)
				}
				if got, ok := countedTokens(history, req, current); !ok || got != current+retained {
					t.Fatalf("keepsTurn=%v: counted=%d, %v; want %d with only retained input calibrated", keepsTurn, got, ok, current+retained)
				}
			}
			// A newer response replaces the old calibration, even when it has
			// no usage. No lookup may fall back through it to the old bias.
			history = append(history, reply("new answer", 0))
			if got, ok := countedTokens(history, req, 123); ok || got != 123 {
				t.Fatalf("uncounted newer response: %d, %v", got, ok)
			}
		})
	}
}

func TestClearingDoesNotRetireCalibrationTwice(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("old", "fetch", 0), resultOf("old", "fetch", strings.Repeat("old ", 1_000)),
		messages.Compaction{ClearThrough: "old"}.Message(),
		callTool("new", "fetch", 0), resultOf("new", "fetch", strings.Repeat("new ", 1_000)),
	}
	req := &CompletionRequest{}
	sent := 0
	for _, msg := range RequestView(history, nil) {
		sent += EstimateMessageTokens(msg)
	}
	response := reply("answer", 2*sent)
	response.SetRequestEstimate(sent)
	history = append(history, response, messages.Compaction{ClearThrough: "new"}.Message())
	removed := estimatedStringTokens(history[5].Content)
	if got, ok := countedTokens(history, req, 200); !ok || got != 200+sent-removed {
		t.Fatalf("clearing calibrated already-cleared content: got %d, %v; want %d", got, ok, 200+sent-removed)
	}
}

func TestClearingRebasesCalibrationAfterBudgetReduction(t *testing.T) {
	large := strings.Repeat("你", 100_000)
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			t.Error("clearing should have made room without a summary")
		}
		return reply("ok", 100_004), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{DisableTools: true})
	defer agent.Close()
	req := &CompletionRequest{Model: "openai/large", MaxContextTokens: 200_000, Messages: []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("read", "fetch", 0), resultOf("read", "fetch", large),
	}}
	first, err := agent.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, first.AllMessages...)
	req.Messages = append(req.Messages, messages.User("continue")...)
	req.MaxContextTokens = 24_576
	second, err := agent.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("clear after budget reduction: %v", err)
	}
	if len(model.requests) != 2 || second.Projection.CountedTokens > 200 {
		t.Fatalf("cleared request retains old bias: calls=%d projection=%+v", len(model.requests), second.Projection)
	}
	marker, ok := second.AllMessages[0].Compaction()
	if !ok || marker.ClearThrough != "read" || marker.Summary != "" {
		t.Fatalf("clearing marker = %+v", marker)
	}
	if req.Messages[2].Content != large || !strings.Contains(agent.renderedTranscript(), large) {
		t.Fatal("clearing rewrote the original result")
	}
}
