package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// batchThenDone answers the first request with one batch of calls, then ends
// the turn.
func batchThenDone(batch []messages.ChatMessageToolCall) *sequentialLLM {
	return &sequentialLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: batch}}}
}

// longOmittedHistory is an exchange far larger than the budgets below, which
// the projection omits and the model then pages back through.
func longOmittedHistory() []messages.ChatMessage {
	var lines []string
	for i := range 400 {
		lines = append(lines, fmt.Sprintf("line %d %s", i, strings.Repeat("y", 90)))
	}
	return []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Join(lines, "\n")},
		{Role: messages.MessageRoleAssistant, Content: "noted"},
		{Role: messages.MessageRoleUser, Content: "what did I send?"},
	}
}

func TestBudgetLoopWithoutSystemMessage(t *testing.T) {
	var runs atomic.Int32
	model := &loopLLM{rounds: 10, calls: 1, text: 10, args: 50, fetchOnly: true}
	registry := tools.NewToolRegistry([]tools.Tool{fetchTool(&runs)})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{ArtifactStore: newTestArtifactStore(), MaxIterations: 15})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: 2_196}, nil)
	if err != nil {
		t.Fatalf("model calls=%d tool runs=%d: %v", model.sent, runs.Load(), err)
	}
	if response.Message == nil || response.Message.Content == "" || runs.Load() == 0 {
		t.Fatalf("tool runs=%d response=%+v", runs.Load(), response.Message)
	}
}

func transcriptReads(offsets ...int) []messages.ChatMessageToolCall {
	var calls []messages.ChatMessageToolCall
	for i, offset := range offsets {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("read%d", i), Name: BuiltinReadTranscript, Arguments: fmt.Sprintf(`{"offset":%d}`, offset)})
	}
	return calls
}

func runRecallBatch(t *testing.T, budget int, batch []messages.ChatMessageToolCall) []messages.ChatMessage {
	t.Helper()
	agent := NewAgent(batchThenDone(batch), nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, nil)
	if err != nil {
		t.Fatalf("the batch overflowed a %d-token budget: %v", budget, err)
	}
	var results []messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool {
			results = append(results, msg)
		}
	}
	if len(results) != len(batch) {
		t.Fatalf("got %d results for %d calls", len(results), len(batch))
	}
	return results
}

// A page takes the room the next request has left, up to the ceiling: under
// a small budget it is cut short and says where to continue, and under a
// roomier one it is not held to a fixed share of the budget.
func TestRecallPageTakesTheRoomLeft(t *testing.T) {
	small := runRecallBatch(t, 4_000, transcriptReads(1))[0].Content
	if !strings.Contains(small, "continue with byte_offset=") {
		t.Fatalf("page under a small budget does not say where to continue: %q", small[max(0, len(small)-120):])
	}
	roomy := runRecallBatch(t, 20_000, transcriptReads(1))[0].Content
	if len(roomy) <= 20_000 || len(roomy) > tools.PageMaxBytes {
		t.Fatalf("page under a 20,000-token budget is %d bytes; want more than a quarter of the budget and at most the ceiling", len(roomy))
	}
	if len(small) >= len(roomy) {
		t.Fatalf("the smaller budget got the larger page: %d >= %d bytes", len(small), len(roomy))
	}
}

// Recall results the model has not read are all sent whole, so parallel
// reads share the room rather than each taking it.
func TestParallelRecallsShareTheRoom(t *testing.T) {
	const budget = 8_000
	results := runRecallBatch(t, budget, transcriptReads(1, 130, 260))
	total := 0
	for _, result := range results {
		if succeeded, _ := result.ToolSucceeded(); !succeeded {
			t.Fatalf("a read that fits was refused: %q", result.Content)
		}
		total += len(result.Content)
	}
	if total/4 > budget {
		t.Fatalf("three pages total %d bytes, more than the whole budget", total)
	}
}

// A batch with more reads than floor-sized pages fit in runs the ones that fit
// and refuses the rest, rather than overflowing the next request.
func TestRecallsBeyondTheRoomAreRefused(t *testing.T) {
	results := runRecallBatch(t, 3_000, transcriptReads(1, 60, 120, 180, 240, 300))
	ran, refused := 0, 0
	for _, result := range results {
		if succeeded, _ := result.ToolSucceeded(); succeeded {
			ran++
			if len(result.Content) < tools.PageMinBytes/2 {
				t.Errorf("a page that ran is only %d bytes", len(result.Content))
			}
			continue
		}
		if !strings.HasPrefix(result.Content, "Not run: the context budget has no room") {
			t.Fatalf("refusal = %q", result.Content)
		}
		refused++
	}
	if ran == 0 || refused == 0 {
		t.Fatalf("ran %d and refused %d of 6 reads under a budget with room for a few", ran, refused)
	}
}

func TestContextFloorTokens(t *testing.T) {
	agent := NewAgent(nil, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	withTools := agent.ToolRegistry().All()

	bare := ContextFloorTokens(&CompletionRequest{})
	system := ContextFloorTokens(&CompletionRequest{Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: strings.Repeat("be terse. ", 200)}}})
	tooled := ContextFloorTokens(&CompletionRequest{Tools: withTools})
	if system <= bare || tooled <= bare {
		t.Fatalf("floor did not grow with a prompt (%d) or tools (%d) over %d", system, tooled, bare)
	}

	// Earlier exchanges add nothing: the projection can omit them.
	older := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("old ", 5000)},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
	}
	active := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "question"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: BuiltinReadTranscript, Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "1", ToolName: BuiltinReadTranscript, Content: strings.Repeat("page ", 5000)},
	}
	withOlder := ContextFloorTokens(&CompletionRequest{Tools: withTools, Messages: append(append([]messages.ChatMessage(nil), older...), active...)})
	if activeOnly := ContextFloorTokens(&CompletionRequest{Tools: withTools, Messages: active}); withOlder != activeOnly {
		t.Fatalf("an omittable exchange changed the floor: %d != %d", withOlder, activeOnly)
	}
	// A recall result already read counts as its stub.
	if withOlder-tooled > 200 {
		t.Fatalf("the active exchange's read page was not counted as its stub: %d tokens over the tools", withOlder-tooled)
	}
}

// Across budgets and batch sizes, a batch of reads never overflows the
// request it feeds, as long as the budget holds one floor-sized page.
func TestRecallBatchesNeverOverflow(t *testing.T) {
	for budget := 3_000; budget <= 30_000; budget += 1_500 {
		for size := 1; size <= 8; size++ {
			offsets := make([]int, size)
			for i := range offsets {
				offsets[i] = 1 + i*45
			}
			agent := NewAgent(batchThenDone(transcriptReads(offsets...)), nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
			if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, nil); err != nil {
				t.Errorf("budget %d, %d reads: %v", budget, size, err)
			}
			agent.Close()
		}
	}
}

// loopLLM keeps a tool loop going for rounds iterations, each response
// carrying interim text and a batch of calls that grow as it goes, then
// answers. A request without tools gets the answer at once.
type loopLLM struct {
	rounds, calls, text, args int
	// fetchOnly leaves out the transcript reads every other call makes.
	fetchOnly bool
	sent      int
	finished  bool
}

func (l *loopLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	l.sent++
	response := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "the answer"}
	if len(req.Tools) == 0 {
		l.finished = true
	} else if l.sent <= l.rounds {
		response = messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, Content: strings.Repeat("thinking aloud ", l.text*l.sent)}
		for i := range l.calls {
			name, args := "fetch", fmt.Sprintf(`{"q":%q}`, strings.Repeat("x", l.args*l.sent))
			if i%2 == 1 && !l.fetchOnly {
				name, args = BuiltinReadTranscript, fmt.Sprintf(`{"offset":%d}`, 1+40*i)
			}
			response.ToolCalls = append(response.ToolCalls, messages.ChatMessageToolCall{ID: fmt.Sprintf("r%dc%d", l.sent, i), Name: name, Arguments: args})
		}
	}
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

// fetchTool returns a large result and counts its runs.
func fetchTool(runs *atomic.Int32) *tools.Func {
	return &tools.Func{
		Name: "fetch",
		Desc: "fetch a page",
		Run: func(context.Context, tools.Args) (string, error) {
			runs.Add(1)
			return strings.Repeat("fetched content ", 3000), nil
		},
	}
}

// Whatever a tool loop does, a run whose first request fits never fails for
// want of room: batches that cannot fit are refused in part or dropped, and
// the run answers.
func TestToolLoopsNeverOutgrowTheBudget(t *testing.T) {
	for budget := 2_500; budget <= 24_000; budget += 2_700 {
		for _, shape := range []loopLLM{
			{rounds: 10, calls: 1, text: 10, args: 50},
			{rounds: 10, calls: 4, text: 40, args: 200},
			{rounds: 3, calls: 12, text: 5, args: 20},
			{rounds: 6, calls: 2, text: 400, args: 2000},
		} {
			var runs atomic.Int32
			registry := tools.NewToolRegistry([]tools.Tool{}, tools.WithUnsafeNoSandbox())
			registry.Register(fetchTool(&runs))
			llm := shape
			agent := NewAgent(&llm, registry, AgentConfig{ArtifactStore: newTestArtifactStore(), MaxIterations: 12})
			response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, nil)
			agent.Close()
			var limit *ContextLimitError
			if errors.As(err, &limit) {
				t.Errorf("budget %d, %+v: %v", budget, shape, err)
				continue
			}
			if err != nil {
				t.Errorf("budget %d, %+v: %v", budget, shape, err)
				continue
			}
			if response.Message == nil || response.Message.Content != "the answer" {
				t.Errorf("budget %d, %+v: run ended without the answer: %+v", budget, shape, response.Message)
			}
		}
	}
}

// A batch the budget has no room for is not run: the run answers from what it
// has, without tools, and the dropped calls leave no trace in its transcript.
func TestBatchWithoutRoomIsDroppedAndTheRunAnswers(t *testing.T) {
	var runs atomic.Int32
	registry := tools.NewToolRegistry([]tools.Tool{}, tools.WithUnsafeNoSandbox())
	registry.Register(fetchTool(&runs))
	// Enough calls that even their refusals overflow the budget.
	llm := &loopLLM{rounds: 1, calls: 120, args: 10}
	agent := NewAgent(llm, registry, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: 3_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 0 {
		t.Fatalf("a dropped batch ran %d tools", runs.Load())
	}
	if !llm.finished || response.Message.Content != "the answer" {
		t.Fatalf("the run did not answer without tools: finished=%v, %+v", llm.finished, response.Message)
	}
	for _, msg := range response.AllMessages {
		if len(msg.ToolCalls) > 0 || msg.Role == messages.MessageRoleTool {
			t.Fatalf("the dropped batch reached the transcript: %+v", msg)
		}
	}
}

// As the budget shrinks, a batch's ordinary calls go from all running, to
// some refused before they run, to the batch being dropped; the run answers at
// every step.
func TestOrdinaryCallsBeyondTheRoomAreNotRun(t *testing.T) {
	var sawAll, sawSome, sawDropped bool
	for budget := 6_000; budget >= 1_500; budget -= 25 {
		var runs atomic.Int32
		registry := tools.NewToolRegistry([]tools.Tool{}, tools.WithUnsafeNoSandbox())
		registry.Register(fetchTool(&runs))
		llm := &loopLLM{rounds: 1, calls: 30, args: 10, fetchOnly: true}
		agent := NewAgent(llm, registry, AgentConfig{ArtifactStore: newTestArtifactStore()})
		response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, nil)
		agent.Close()
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		refused := refusedResults(response.AllMessages)
		ran := int(runs.Load())
		switch {
		case ran == 30 && refused == 0:
			sawAll = true
		case ran > 0 && ran+refused == 30:
			sawSome = true
		case ran == 0 && refused == 0 && llm.finished:
			sawDropped = true
		default:
			t.Fatalf("budget %d: ran %d, refused %d, finished %v", budget, ran, refused, llm.finished)
		}
	}
	if !sawAll || !sawSome || !sawDropped {
		t.Fatalf("all ran %v, some refused %v, dropped %v", sawAll, sawSome, sawDropped)
	}
}
