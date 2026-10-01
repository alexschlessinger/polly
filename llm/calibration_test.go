package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// windowLLM is a provider with a context window of its own that counts
// every request at ratio times its estimate. It rejects a request too long
// for the window, as reject words it, and otherwise answers with the next
// reply, reporting what it counted.
type windowLLM struct {
	mu      sync.Mutex
	ratio   float64
	window  int
	reject  func(input, output int) error
	replies []messages.ChatMessage

	requests  []CompletionRequest
	estimates []int
	rejected  []bool
}

func (l *windowLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	l.mu.Lock()
	estimate := requestTokens(req)
	input := int(float64(estimate) * l.ratio)
	l.requests = append(l.requests, *req)
	l.estimates = append(l.estimates, estimate)
	var err error
	if l.window > 0 && input+req.MaxTokens > l.window {
		err = l.reject(input, req.MaxTokens)
	}
	l.rejected = append(l.rejected, err != nil)
	var reply messages.ChatMessage
	if err == nil {
		accepted := 0
		for _, rejected := range l.rejected {
			if !rejected {
				accepted++
			}
		}
		reply = l.replies[min(accepted, len(l.replies))-1]
		reply.Metadata = map[string]any{messages.MetadataKeyInputTokens: input}
	}
	l.mu.Unlock()
	if err != nil {
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: err}
		close(events)
		return events
	}
	replies := make(chan messages.ChatMessage, 1)
	replies <- reply
	close(replies)
	return processor.ProcessMessagesToEvents(ctx, replies)
}

// deepSeekOverflow words a rejection as DeepSeek does, with exact counts.
func deepSeekOverflow(window int) func(input, output int) error {
	return func(input, output int) error { return deepSeekWording.reject(window, input, 0, output) }
}

// openAIOverflow words a rejection as OpenAI does now, with no counts.
func openAIOverflow(int, int) error { return openAIWording.reject(0, 0, 0, 0) }

func fetchReply(id string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "fetch", Arguments: `{"size":10}`}}}
}

// longHistory is a system prompt, exchanges of about exchange tokens each
// that the projection may omit, and a question.
func longHistory(exchanges, exchange int) []messages.ChatMessage {
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "You are terse."}}
	for i := range exchanges {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("question %d %s", i, strings.Repeat("word ", exchange*4/5))},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer"})
	}
	return append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "and now?"})
}

// Reported usage sizes every later request, whichever way the estimate was
// off, and agents that share a calibration start from it.
func TestReportedUsageSizesLaterRequests(t *testing.T) {
	shared := NewCalibration()
	run := func(ratio float64, calibration *Calibration) []int {
		t.Helper()
		model := &windowLLM{ratio: ratio, replies: []messages.ChatMessage{fetchReply("a"), answer("done")}}
		agent := NewAgent(model, registryWith(&sizedTool{name: "fetch"}), AgentConfig{Calibration: calibration})
		defer agent.Close()
		req := &CompletionRequest{Model: "vendor/model", MaxContextTokens: 20_000, Messages: longHistory(1, 50)}
		if _, err := agent.Run(context.Background(), req, nil); err != nil {
			t.Fatal(err)
		}
		var budgets []int
		for _, r := range model.requests {
			budgets = append(budgets, r.MaxContextTokens)
		}
		return budgets
	}
	within := func(got, want int) bool { return got >= want-want/50 && got <= want+want/50 }
	if got := run(2, shared); len(got) != 2 || got[0] != 20_000 || !within(got[1], 10_000) {
		t.Fatalf("counted at twice the estimate, budgets %v, want [20000 ~10000]", got)
	}
	// The next agent starts where the last request left off, and a count
	// below the estimate loosens the budget.
	if got := run(0.5, shared); len(got) != 2 || !within(got[0], 10_000) || !within(got[1], 40_000) {
		t.Fatalf("counted at half the estimate after twice, budgets %v, want [~10000 ~40000]", got)
	}
	if got := run(2, nil); got[0] != 20_000 {
		t.Fatalf("an agent of its own starts at %d, want the budget", got[0])
	}
}

// A request rejected as too long, with the counts, is sent again sized to
// what the rejection stated, and the caller hears a note, not an error.
func TestOverflowIsAnsweredWithARequestThatFits(t *testing.T) {
	const window = 6_000
	model := &windowLLM{ratio: 1.5, window: window, reject: deepSeekOverflow(window), replies: []messages.ChatMessage{answer("done")}}
	agent := NewAgent(model, registryWith(&sizedTool{name: "fetch"}), AgentConfig{})
	defer agent.Close()
	var errs []error
	var notes []string
	cb := &AgentCallbacks{
		OnError:      func(err error) { errs = append(errs, err) },
		OnAdaptation: func(note RequestAdaptation) { notes = append(notes, note.Message) },
	}
	req := &CompletionRequest{Model: "deepseek/deepseek-v4.1-flash", MaxTokens: 1_000, MaxContextTokens: 50_000, Messages: longHistory(20, 500)}
	response, err := agent.Run(context.Background(), req, cb)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("run = %v, %v", response, err)
	}
	if len(model.requests) != 2 || !model.rejected[0] || model.rejected[1] {
		t.Fatalf("rejections %v, want the first request alone rejected", model.rejected)
	}
	if real := int(float64(model.estimates[1]) * 1.5); real+model.requests[1].MaxTokens > window {
		t.Fatalf("retry counts %d tokens and asks %d, over the %d-token window", real, model.requests[1].MaxTokens, window)
	}
	if len(errs) != 0 {
		t.Fatalf("OnError heard %v for a rejection that was answered", errs)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "6000-token context window") {
		t.Fatalf("notes = %q", notes)
	}
}

// A provider that rejects without counts gets a request shrunk a step, then
// the request that finishes the run without tools, then the rejection back
// as the run's error: never the same request twice.
func TestOverflowWithoutCountsEndsInItsError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		history   []messages.ChatMessage
		wantSends int
	}{
		// Omissions make the second request smaller, and the finishing
		// request is smaller again.
		{"shrinkable", longHistory(20, 500), 3},
		// Nothing can be omitted: the retry would repeat the request, so
		// the finishing one goes instead, and it cannot shrink either.
		{"unshrinkable", longHistory(0, 0), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &windowLLM{ratio: 1, window: 1, reject: openAIOverflow, replies: []messages.ChatMessage{answer("done")}}
			agent := NewAgent(model, registryWith(&sizedTool{name: "fetch"}), AgentConfig{})
			defer agent.Close()
			var errs []error
			cb := &AgentCallbacks{OnError: func(err error) { errs = append(errs, err) }}
			req := &CompletionRequest{Model: "openai/gpt-5.5", MaxContextTokens: 50_000, Messages: tc.history}
			response, err := agent.Run(context.Background(), req, cb)
			var overflow *ContextOverflowError
			if !errors.As(err, &overflow) {
				t.Fatalf("run error = %v, want the provider's rejection", err)
			}
			if response == nil || response.IterationCount != 1 {
				t.Fatalf("response = %+v, want one model call accounted", response)
			}
			if len(model.requests) != tc.wantSends {
				t.Fatalf("sent %d requests, want %d", len(model.requests), tc.wantSends)
			}
			for i := 1; i < len(model.requests); i++ {
				if model.estimates[i] >= model.estimates[i-1] {
					t.Fatalf("request %d estimated at %d after %d was rejected", i, model.estimates[i], model.estimates[i-1])
				}
			}
			if last := model.requests[len(model.requests)-1]; len(last.Tools) != 0 {
				t.Fatalf("the last request offers %d tools, want the finishing request", len(last.Tools))
			}
			if len(errs) != 1 {
				t.Fatalf("OnError heard %v, want the rejection once", errs)
			}
		})
	}
}

// An output reserve that leaves the input no room gives way.
func TestOverflowLowersAnOutputReserveThatCrowdsOutTheInput(t *testing.T) {
	const window = 8_000
	model := &windowLLM{ratio: 1, window: window, reject: deepSeekOverflow(window), replies: []messages.ChatMessage{answer("done")}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	req := &CompletionRequest{Model: "deepseek/deepseek-v4.1-flash", MaxTokens: window, MaxContextTokens: 3_000, Messages: longHistory(1, 100)}
	if _, err := agent.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("sent %d requests, want 2", len(model.requests))
	}
	if got := model.requests[1].MaxTokens; got != window-3_000 {
		t.Fatalf("retry reserves %d output tokens, want %d", got, window-3_000)
	}
}

// A first request rejected and sent again clears the caller's staged input
// once.
func TestOverflowRetryClearsTheFirstRequestOnce(t *testing.T) {
	const window = 4_000
	model := &windowLLM{ratio: 1, window: window, reject: deepSeekOverflow(window), replies: []messages.ChatMessage{answer("done")}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	cleared := 0
	cb := &AgentCallbacks{BeforeFirstRequest: func(ProjectionStats) error { cleared++; return nil }}
	req := &CompletionRequest{Model: "deepseek/deepseek-v4.1-flash", MaxContextTokens: 50_000, Messages: longHistory(20, 500)}
	if _, err := agent.Run(context.Background(), req, cb); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 || cleared != 1 {
		t.Fatalf("%d requests cleared %d times, want 2 requests cleared once", len(model.requests), cleared)
	}
}

func TestOverflowWithoutCountsTightensLearnedWindow(t *testing.T) {
	const window = 10_000
	calibration := NewCalibration()
	calibration.learnOverflow("m", &ContextOverflowError{Window: window, Input: 12_000, Output: 1_000}, 12_000, 1_000)
	model := &windowLLM{ratio: 1.3, window: window, reject: openAIOverflow, replies: []messages.ChatMessage{answer("done")}}
	registry := registryWith(&sizedTool{name: "fetch"})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{Calibration: calibration})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{
		Model: "m", MaxTokens: 1_000, MaxContextTokens: 20_000, Messages: longHistory(30, 500),
	}, nil)
	if err != nil {
		t.Fatalf("rejections=%v estimates=%v: %v", model.rejected, model.estimates, err)
	}
	if response.Message.Content != "done" || len(model.rejected) != 2 || !model.rejected[0] || model.rejected[1] {
		t.Fatalf("rejections=%v response=%+v", model.rejected, response.Message)
	}
	if len(model.requests[1].Tools) == 0 {
		t.Fatal("retry discarded tools instead of tightening the budget")
	}
}

func TestCalibrationSizesRequests(t *testing.T) {
	c := NewCalibration()
	size := func(budget, maxTokens int) (int, int) {
		req := &CompletionRequest{Model: "m", MaxTokens: maxTokens}
		c.size(req, budget)
		return req.MaxContextTokens, req.MaxTokens
	}
	if got, _ := size(10_000, 0); got != 10_000 {
		t.Fatalf("unmeasured budget = %d", got)
	}
	c.learnUsage("m", 3_000, 1_000)
	if got, _ := size(9_000, 0); got != 3_000 {
		t.Fatalf("budget at ratio 3 = %d, want 3000", got)
	}
	if got, _ := size(0, 0); got != 0 {
		t.Fatalf("no budget and no window = %d, want none", got)
	}
	c.learnUsage("m", 1, 1_000)
	if got := c.Ratio("m"); got != minRatio {
		t.Fatalf("ratio = %v, want it held at %v", got, minRatio)
	}
	c.learnUsage("m", 1_000, 1_000)
	// A stated window bounds the budget after the output reserve, even when
	// there is no budget.
	c.learnOverflow("m", &ContextOverflowError{Window: 10_000, Input: 12_000, Output: 1_000}, 12_000, 1_000)
	if got, _ := size(0, 1_000); got != 9_000 {
		t.Fatalf("budget under a 10000 window = %d, want 9000", got)
	}
	if got, reserve := size(4_000, 9_000); got != 4_000 || reserve != 6_000 {
		t.Fatalf("budget and reserve = %d, %d, want 4000, 6000", got, reserve)
	}
	if got, reserve := size(0, 9_000); got != 5_000 || reserve != 5_000 {
		t.Fatalf("unbudgeted input and reserve = %d, %d, want half the window each", got, reserve)
	}
	// A rejection that states the window but no count raises the ratio past
	// what the window implies, by a step.
	c.learnOverflow("m", &ContextOverflowError{Window: 10_000}, 8_000, 1_000)
	if got, want := c.Ratio("m"), 9_000.0/8_000*overflowStep; got != want {
		t.Fatalf("ratio after a countless rejection = %v, want %v", got, want)
	}
	// One that states neither sets a limit a step below what was refused.
	c.learnUsage("n", 2_000, 1_000)
	c.learnOverflow("n", &ContextOverflowError{}, 5_000, 0)
	req := &CompletionRequest{Model: "n"}
	if c.size(req, 50_000); req.MaxContextTokens != 4_000 {
		t.Fatalf("budget after a bare rejection = %d, want 4000 estimated for 8000 provider tokens", req.MaxContextTokens)
	}
}

// A window one endpoint's rejection stated bounds requests to that endpoint,
// not those to another host serving the same model.
func TestCalibrationKeepsEachRouteApart(t *testing.T) {
	c := NewCalibration()
	small := &CompletionRequest{Model: "m", ModelHost: "small-host"}
	c.learnOverflow(calibrationRoute(small), &ContextOverflowError{Window: 8_192, Input: 9_000}, 9_000, 0)
	big := &CompletionRequest{Model: "m", ModelHost: "big-host"}
	if c.size(big, 120_000); big.MaxContextTokens != 120_000 {
		t.Fatalf("another host's request was sized to %d", big.MaxContextTokens)
	}
	if c.size(small, 120_000); small.MaxContextTokens > 8_192 {
		t.Fatalf("the rejecting host's request was sized to %d", small.MaxContextTokens)
	}
}

// A zero Calibration is empty and ready to use, like one from NewCalibration.
func TestZeroCalibrationIsReady(t *testing.T) {
	var c Calibration
	if c.Ratio("test/m") != 1 || c.heldFronts("s") != (projectionFronts{}) {
		t.Fatal("a zero calibration is not empty")
	}
	c.learnUsage("test/m", 3, 2)
	c.learnOverflow("test/m", &ContextOverflowError{}, 500, 100)
	c.keepFronts("s", projectionFronts{compacted: 1})
	if c.Ratio("test/m") != 1.5 || c.heldFronts("s").compacted != 1 {
		t.Fatalf("ratio %v, fronts %+v", c.Ratio("test/m"), c.heldFronts("s"))
	}
}
