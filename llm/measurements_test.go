package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A request that extends the last one sent with the same tools prices the
// forms it appended at the provider's count, split by what they were priced
// at; any other request prices nothing and becomes the last.
func TestMeasurementsPriceAppendedForms(t *testing.T) {
	m := &measurements{}
	a, b, c, d := sentForm{1, 50}, sentForm{2, 50}, sentForm{3, 30}, sentForm{4, 10}
	if learned := m.learn([]sentForm{a, b}, "tools", 100); len(learned) != 0 {
		t.Fatalf("the first request priced %v", learned)
	}
	learned := m.learn([]sentForm{a, b, c, d}, "tools", 180)
	if len(learned) != 2 || learned[3] != 60 || learned[4] != 20 {
		t.Fatalf("learned %v, want c at 60 and d at 20", learned)
	}
	if n, ok := m.count(3); !ok || n != 60 {
		t.Fatalf("c counts %d, %v", n, ok)
	}
	if _, ok := m.count(1); ok {
		t.Fatal("a was priced without ever being appended")
	}
	// Other tools, or a request that does not extend the last, price nothing.
	e := sentForm{5, 10}
	if learned := m.learn([]sentForm{a, b, c, d, e}, "other", 300); len(learned) != 0 {
		t.Fatalf("other tools priced %v", learned)
	}
	if learned := m.learn([]sentForm{a, c, e}, "other", 400); len(learned) != 0 {
		t.Fatalf("a request that does not extend the last priced %v", learned)
	}
	// A count far from what the appended forms were priced at is not theirs.
	wild := &measurements{}
	wild.learn([]sentForm{a}, "tools", 50)
	if learned := wild.learn([]sentForm{a, e}, "tools", 50+100); len(learned) != 0 {
		t.Fatalf("a count ten times the price was attributed: %v", learned)
	}
	if learned := wild.learn([]sentForm{a, e, c}, "tools", 150); len(learned) != 0 {
		t.Fatalf("a count that did not grow was attributed: %v", learned)
	}
}

// pricedLLM counts a request as price says each message costs, plus the
// tool schemas as estimated, and reports the count as its usage.
type pricedLLM struct {
	price   func(messages.ChatMessage) int
	replies []messages.ChatMessage
	calls   int
	inputs  []int
}

func (l *pricedLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	input := estimateToolSchemaTokens(req.Tools)
	for _, msg := range req.Messages {
		input += l.price(msg)
	}
	l.inputs = append(l.inputs, input)
	reply := l.replies[min(l.calls, len(l.replies)-1)]
	l.calls++
	reply.Metadata = map[string]any{messages.MetadataKeyInputTokens: input}
	replies := make(chan messages.ChatMessage, 1)
	replies <- reply
	close(replies)
	return processor.ProcessMessagesToEvents(ctx, replies)
}

func fetchCall(id string, size int) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "fetch", Arguments: fmt.Sprintf(`{"size":%d}`, size)}}}
}

// Once a request's count has priced the messages it appended, later requests
// are off by their unmeasured messages alone, within a run and into the next
// run of the conversation.
func TestMeasuredCountsPriceLaterRequests(t *testing.T) {
	// Tool output counts twice as dense as the estimate rates it, with a
	// framing cost; everything else counts as estimated.
	dense := func(msg messages.ChatMessage) int {
		if msg.Role == messages.MessageRoleTool {
			return len(msg.Content)/2 + 11
		}
		return estimateProjectedMessageTokens(msg)
	}
	model := &pricedLLM{price: dense, replies: []messages.ChatMessage{fetchCall("c1", 4_000), fetchCall("c2", 6_000), answer("done")}}
	calibration := NewCalibration()
	agent := NewAgent(model, registryWith(&sizedTool{name: "fetch"}), AgentConfig{Calibration: calibration})
	defer agent.Close()
	var stats []ProjectionStats
	cb := &AgentCallbacks{OnRequestProjection: func(_ int, s ProjectionStats) { stats = append(stats, s) }}
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "You are terse."}, {Role: messages.MessageRoleUser, Content: "fetch twice"}}
	req := &CompletionRequest{Model: "vendor/model", CacheSessionID: "conversation", Messages: history}
	response, err := agent.Run(context.Background(), req, cb)
	if err != nil || len(model.inputs) != 3 || len(stats) != 3 {
		t.Fatalf("run = %v, %d requests, %d projections", err, len(model.inputs), len(stats))
	}
	generated := response.AllMessages
	if len(generated) != 5 || generated[1].Role != messages.MessageRoleTool || generated[3].Role != messages.MessageRoleTool {
		t.Fatalf("generated %d messages, want a call, a result, a call, a result and an answer", len(generated))
	}
	first, second := generated[1], generated[3]
	// The second request was estimated, and the provider counted the first
	// result denser; the third was priced at the second's count for the
	// first call and result and is closer to the provider's count.
	if off := model.inputs[1] - stats[1].CalibratedTokens; off != dense(first)-estimateProjectedMessageTokens(first) {
		t.Fatalf("the second request was off by %d, want the first result's density", off)
	}
	if before, after := model.inputs[1]-stats[1].CalibratedTokens, model.inputs[2]-stats[2].CalibratedTokens; abs(after) >= abs(before)/4 {
		t.Fatalf("the third request was off by %d after the second was off by %d", after, before)
	}
	measured := calibration.measurementsFor("conversation")
	for _, pair := range [][2]messages.ChatMessage{{generated[0], first}, {generated[2], second}} {
		m1, ok1 := measured.count(formFingerprint(pair[0]))
		m2, ok2 := measured.count(formFingerprint(pair[1]))
		if !ok1 || !ok2 || m1+m2 != dense(pair[0])+dense(pair[1]) {
			t.Fatalf("a call and its result measured %d (%v) and %d (%v), want their count %d between them", m1, ok1, m2, ok2, dense(pair[0])+dense(pair[1]))
		}
	}
	// The next run of the conversation extends the last request sent and
	// is priced whole.
	model.replies = []messages.ChatMessage{answer("again")}
	next := append(append(append([]messages.ChatMessage{}, history...), generated...), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "and again"})
	stats = nil
	if _, err := agent.Run(context.Background(), &CompletionRequest{Model: "vendor/model", CacheSessionID: "conversation", Messages: next}, cb); err != nil {
		t.Fatal(err)
	}
	last, again := generated[4], next[len(next)-1]
	m1, ok1 := measured.count(formFingerprint(last))
	m2, ok2 := measured.count(formFingerprint(again))
	if len(model.inputs) != 4 || !ok1 || !ok2 || m1+m2 != dense(last)+dense(again) {
		t.Fatalf("the next run's request priced the answer and the new input at %d (%v) and %d (%v), want %d between them", m1, ok1, m2, ok2, dense(last)+dense(again))
	}
}

// A measured count prices the form it was measured for: a result back
// inline costs its count, and its stub costs the stub's estimate.
func TestMeasuredCountFollowsTheForm(t *testing.T) {
	content := strings.Repeat("line of transcript\n", 1_000)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "You are terse."},
		{Role: messages.MessageRoleUser, Content: "read it"},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "t1", Name: "read_transcript", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "t1", ToolName: "read_transcript", Content: content},
		{Role: messages.MessageRoleAssistant, Content: "read"},
	}
	measured := &measurements{counts: map[uint64]int{formFingerprint(history[3]): 9_000}}
	project := func(budget int) ProjectionStats {
		t.Helper()
		cache := &projectionCache{measured: measured}
		_, stats, err := projectMessagesCached(context.Background(), cloneMessages(history), budget, nil, builtinProjectionTools(true), cache)
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	rest := 0
	for i, msg := range history {
		if i != 3 {
			rest += estimateProjectedMessageTokens(msg)
		}
	}
	if got := project(0).EstimatedTokens; got != rest+9_000 {
		t.Fatalf("priced the measured result at %d, want its 9000", got-rest)
	}
	stub := history[3]
	stub.Content = recallResultStub("read_transcript")
	stubbed := project(rest + 8_000)
	if stubbed.CompactedToolResults != 1 || stubbed.EstimatedTokens != rest+estimateProjectedMessageTokens(stub) {
		t.Fatalf("stubbed %d results priced at %d, want the stub's estimate %d", stubbed.CompactedToolResults, stubbed.EstimatedTokens-rest, estimateProjectedMessageTokens(stub))
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
