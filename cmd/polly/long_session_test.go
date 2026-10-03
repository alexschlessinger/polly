package main

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// localModel stands in for a small local model behind llama.cpp or vLLM: it
// counts input heavier than polly estimates it, rejects a request too long
// for its window as the server would, and answers each turn with calls tool
// calls before a reply. It notes a request that no longer carries the last
// tool result, which the model has yet to read.
type localModel struct {
	mu           sync.Mutex
	window       int
	countsOutput bool // vLLM: input and max_tokens must fit together
	drift        float64
	calls        int
	rng          *rand.Rand

	lastResult                                string
	maxInput, rejections, lostUnread          int
	summaries, summariesInPlace, conversation int
}

func (m *localModel) GetModelInfo(context.Context, llm.ModelTarget) (*llm.ModelInfo, error) {
	output := 16_384
	return &llm.ModelInfo{ModelCapabilities: llm.ModelCapabilities{ContextTokens: &m.window, OutputTokens: &output}}, nil
}

func (m *localModel) ChatCompletionStream(ctx context.Context, req *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	estimate := 0
	for _, msg := range req.Messages {
		estimate += llm.EstimateMessageTokens(msg)
	}
	input := int(float64(estimate)*m.drift) + 250*len(req.Tools)
	m.maxInput = max(m.maxInput, input)
	var rejection error
	switch {
	case m.countsOutput && input+req.MaxTokens > m.window:
		rejection = fmt.Errorf("This model's maximum context length is %d tokens. However, you requested %d tokens (%d in the messages, %d in the completion).", m.window, input+req.MaxTokens, input, req.MaxTokens)
	case !m.countsOutput && input > m.window:
		rejection = fmt.Errorf(`{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":%d,"n_ctx":%d}}`, input, m.window)
	}
	if rejection != nil {
		m.rejections++
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: rejection}
		close(events)
		return events
	}
	reply := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn}
	if last := req.Messages[len(req.Messages)-1]; strings.Contains(last.Content, "Summarize the conversation above") {
		m.summaries++
		if req.Messages[0].Content != summaryPromptForTest(req) {
			m.summariesInPlace++
		}
		reply.Content = "SUMMARY: " + strings.Repeat("the user is refactoring the parser; files read; decisions made. ", 90)
		reply.SetTokenUsage(input, 1_400)
		return streamReply(ctx, processor, reply)
	}
	m.conversation++
	if m.lastResult != "" && !carries(req.Messages, m.lastResult) {
		m.lostUnread++
	}
	step := 0
	for i := len(req.Messages) - 1; i >= 0 && req.Messages[i].Role != messages.MessageRoleUser; i-- {
		if req.Messages[i].Role == messages.MessageRoleTool {
			step++
		}
	}
	if step < m.calls {
		name := "read_sim"
		if m.rng.Intn(3) == 0 {
			name = "bash_sim"
		}
		reply.StopReason = messages.StopReasonToolUse
		reply.ToolCalls = []messages.ChatMessageToolCall{{ID: fmt.Sprintf("call_%d", step), Name: name, Arguments: `{}`}}
		reply.SetTokenUsage(input, 80)
	} else {
		reply.Content = "Done with this step. " + strings.Repeat("Explanation of the change. ", 40)
		reply.SetTokenUsage(input, 300)
		m.lastResult = ""
	}
	return streamReply(ctx, processor, reply)
}

// summaryPromptForTest is what a transcript summary request leads with: the
// compaction model's system prompt, in a request of its own.
func summaryPromptForTest(req *llm.CompletionRequest) string {
	if len(req.Messages) == 2 && req.Messages[0].Role == messages.MessageRoleSystem && strings.HasPrefix(req.Messages[0].Content, "You compact a conversation") {
		return req.Messages[0].Content
	}
	return ""
}

func carries(msgs []messages.ChatMessage, marker string) bool {
	for _, msg := range msgs {
		if strings.Contains(msg.Content, marker) {
			return true
		}
		for _, part := range msg.Parts {
			if strings.Contains(part.Text, marker) {
				return true
			}
		}
	}
	return false
}

func streamReply(ctx context.Context, processor llm.EventStreamProcessor, reply messages.ChatMessage) <-chan *messages.StreamEvent {
	input := make(chan messages.ChatMessage, 1)
	input <- reply
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

// A long session on a small local model stays within its window turn after
// turn, though the model counts tokens heavier than polly estimates them:
// no request is rejected or fails, no result the model has yet to read is
// compacted away, summaries reuse the request's prompt cache, and each
// compaction is a note, not a warning.
func TestLongSessionOnASmallLocalModel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		window       int
		countsOutput bool
		drift        float64
	}{
		{"llama.cpp 32k, tokenizer 25% heavier", 32_768, false, 1.25},
		{"vLLM 32k counting output, 15% heavier", 32_768, true, 1.15},
		{"llama.cpp 16k, 15% heavier", 16_384, false, 1.15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "long-session")
			rng := rand.New(rand.NewSource(7))
			model := &localModel{window: tc.window, countsOutput: tc.countsOutput, drift: tc.drift, calls: 5, rng: rng}
			results := 0
			output := func(least, spread int) func(context.Context, tools.Args) (string, error) {
				return func(context.Context, tools.Args) (string, error) {
					results++
					marker := fmt.Sprintf("RESULT-%d", results)
					model.mu.Lock()
					model.lastResult = marker
					model.mu.Unlock()
					var b strings.Builder
					b.WriteString(marker + "\n")
					for n := least + rng.Intn(spread); b.Len() < n; {
						fmt.Fprintf(&b, "%d: func handle%d(x int) error { return process(x, %q) }\n", b.Len(), b.Len(), "payload")
					}
					return b.String(), nil
				}
			}
			registry := tools.NewToolRegistry([]tools.Tool{
				&tools.Func{Name: "read_sim", Run: output(4_000, 30_000)},
				&tools.Func{Name: "bash_sim", Run: output(500, 60_000)},
			})
			defer registry.Close()
			agent := llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: session.ArtifactStore()})
			defer agent.Close()
			state := &conversationState{session: session, artifactStore: session.ArtifactStore(), toolRegistry: registry, agent: agent,
				settings: Settings{Model: "local/coder", MaxTokens: 32_000, AutoMaxContext: true, MaxHistoryTokens: defaultContextBudget}}
			var stdout, stderr bytes.Buffer
			base := newLineTurnUI(&Config{}, nil)
			base.writer, base.errWriter = &stdout, &stderr
			base.interactive = true
			ui := &contextUsageRecorder{TurnUI: base}
			for turn := 1; turn <= 20; turn++ {
				prompt := messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("Step %d: keep refactoring the parser.", turn)}
				if code, err := executeTurnWithUserMessage(ctx, &Config{}, state, prompt, nil, nil, ui, false); code != 0 || err != nil {
					t.Fatalf("turn %d: code %d, %v", turn, code, err)
				}
			}
			markers := 0
			for _, msg := range testSessionHistory(t, session) {
				if _, ok := msg.Compaction(); ok {
					markers++
				}
			}
			log := stderr.String() + stdout.String()
			switch {
			case model.rejections != 0 || model.maxInput > tc.window:
				t.Fatalf("%d rejections; largest request %d tokens of a %d window", model.rejections, model.maxInput, tc.window)
			case model.lostUnread != 0:
				t.Fatalf("%d requests lost a result the model had yet to read", model.lostUnread)
			case markers == 0 || model.summaries == 0:
				t.Fatalf("the session never compacted: %d markers, %d summaries over %d requests", markers, model.summaries, model.conversation)
			case model.summariesInPlace != model.summaries:
				t.Fatalf("%d of %d summaries reused the request's prompt cache", model.summariesInPlace, model.summaries)
			case strings.Count(log, "Context compacted") != markers || strings.Contains(log, "Warning:"):
				t.Fatalf("%d compactions, notes:\n%s", markers, log)
			case ui.limit != tc.window-llm.ContextReserve(tc.window, 32_000):
				t.Fatalf("meter budget %d, want the window less the reply's room", ui.limit)
			}
		})
	}
}
