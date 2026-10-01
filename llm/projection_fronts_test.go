package llm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// frontExchanges are completed exchanges whose tool results demote under a
// tight budget: about 600 estimated tokens each.
func frontExchanges(from, to int) []messages.ChatMessage {
	var history []messages.ChatMessage
	for j := from; j < to; j++ {
		id := fmt.Sprintf("call-%02d", j)
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("u%02d", j)},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "bash", Arguments: `{}`}}},
			messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "bash", Content: fmt.Sprintf("%02d ", j) + strings.Repeat("r", 2_400)},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "noted"},
		)
	}
	return history
}

func TestCompactionFrontHoldsWhenTheBudgetWidens(t *testing.T) {
	store := newTestArtifactStore()
	history := append(frontExchanges(0, 10), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "current question"})
	cache := &projectionCache{}
	agentTools := builtinProjectionTools(false)

	tight, tightStats, err := projectMessagesCached(context.Background(), cloneMessages(history), 3_000, store, agentTools, cache)
	if err != nil {
		t.Fatal(err)
	}
	if tightStats.CompactedToolResults == 0 || tightStats.CompactedToolResults == 10 {
		t.Fatalf("front is not partial: %+v", tightStats)
	}

	// The budget a calibration re-sizes by a compacted request's lower count
	// widens; the demoted results must not come back.
	wide, wideStats, err := projectMessagesCached(context.Background(), cloneMessages(history), 300_000, store, agentTools, cache)
	if err != nil {
		t.Fatal(err)
	}
	if wideStats.CompactedToolResults != tightStats.CompactedToolResults {
		t.Fatalf("front retreated when the budget widened: %d -> %d", tightStats.CompactedToolResults, wideStats.CompactedToolResults)
	}
	if !reflect.DeepEqual(wide, tight) {
		t.Fatal("projection changed when the budget widened")
	}

	// Growth under the wide budget keeps the prefix byte for byte.
	grown := append(cloneMessages(history),
		messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "ok"},
		messages.ChatMessage{Role: messages.MessageRoleUser, Content: "again"},
	)
	after, afterStats, err := projectMessagesCached(context.Background(), grown, 300_000, store, agentTools, cache)
	if err != nil {
		t.Fatal(err)
	}
	if afterStats.CompactedToolResults != tightStats.CompactedToolResults || !reflect.DeepEqual(after[:len(tight)], tight) {
		t.Fatalf("prefix not byte-stable across growth under the wide budget: %+v", afterStats)
	}

	// A fresh cache under the wide budget compacts nothing: the hold is the
	// cache's, not the history's.
	_, freshStats, err := projectMessagesCached(context.Background(), cloneMessages(history), 300_000, store, agentTools, &projectionCache{})
	if err != nil {
		t.Fatal(err)
	}
	if freshStats.CompactedToolResults != 0 {
		t.Fatalf("fresh projection compacted %d results under the wide budget", freshStats.CompactedToolResults)
	}
}

func TestOmissionFrontHoldsWhenTheBudgetWidens(t *testing.T) {
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "sys"}}
	for j := 0; j < 40; j++ {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("u%02d %s", j, strings.Repeat("q", 120))},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: strings.Repeat("a", 80)},
		)
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "current question"})
	cache := &projectionCache{}
	agentTools := builtinProjectionTools(false)

	tight, tightStats, err := projectMessagesCached(context.Background(), cloneMessages(history), 600, nil, agentTools, cache)
	if err != nil {
		t.Fatal(err)
	}
	if tightStats.OmittedExchanges == 0 {
		t.Fatalf("omission projection = %+v", tightStats)
	}
	wide, wideStats, err := projectMessagesCached(context.Background(), cloneMessages(history), 100_000, nil, agentTools, cache)
	if err != nil {
		t.Fatal(err)
	}
	if wideStats.OmittedExchanges != tightStats.OmittedExchanges {
		t.Fatalf("front retreated when the budget widened: %d -> %d", tightStats.OmittedExchanges, wideStats.OmittedExchanges)
	}
	if !reflect.DeepEqual(wide, tight) {
		t.Fatal("projection changed when the budget widened")
	}
	if !strings.Contains(wide[0].Content, "[Context projection:") {
		t.Fatalf("marker missing from the held omission: %q", wide[0].Content)
	}
}

func TestRefusedProjectionHoldsNoFronts(t *testing.T) {
	store := newTestArtifactStore()
	// The current question alone is over the tight budget, so no front can
	// make the projection fit.
	history := append(frontExchanges(0, 10), messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 40_000)})
	cache := &projectionCache{}
	agentTools := builtinProjectionTools(false)

	_, _, err := projectMessagesCached(context.Background(), cloneMessages(history), 3_000, store, agentTools, cache)
	var limit *ContextLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want a context limit error", err)
	}
	_, stats, err := projectMessagesCached(context.Background(), cloneMessages(history), 300_000, store, agentTools, cache)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CompactedToolResults != 0 || stats.OmittedExchanges != 0 {
		t.Fatalf("a refused projection held its fronts: %+v", stats)
	}
}

func TestFrontsCarryAcrossRunsThatExtendTheHistory(t *testing.T) {
	store := newTestArtifactStore()
	history := append(frontExchanges(0, 10), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "current question"})
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	defer agent.Close()

	first, err := agent.Run(context.Background(), &CompletionRequest{Messages: cloneMessages(history), MaxContextTokens: 3_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Projection.CompactedToolResults == 0 || first.Projection.CompactedToolResults == 10 {
		t.Fatalf("first run's front is not partial: %+v", first.Projection)
	}

	// The next turn's history extends the last one's; the wide budget its
	// request is sized to must not send the demoted results back.
	next := append(cloneMessages(history), first.AllMessages...)
	next = append(next, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "next"})
	second, err := agent.Run(context.Background(), &CompletionRequest{Messages: cloneMessages(next), MaxContextTokens: 300_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Projection.CompactedToolResults != first.Projection.CompactedToolResults {
		t.Fatalf("front retreated across the run boundary: %d -> %d", first.Projection.CompactedToolResults, second.Projection.CompactedToolResults)
	}
	if len(model.requests) != 2 || !reflect.DeepEqual(model.requests[1][:len(model.requests[0])], model.requests[0]) {
		t.Fatal("the second request's prefix is not the first request")
	}

	// A fresh agent has nothing to hold to.
	freshModel := &recordingSequentialLLM{}
	fresh := NewAgent(freshModel, nil, AgentConfig{ArtifactStore: store})
	defer fresh.Close()
	freshRun, err := fresh.Run(context.Background(), &CompletionRequest{Messages: cloneMessages(next), MaxContextTokens: 300_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if freshRun.Projection.CompactedToolResults != 0 {
		t.Fatalf("fresh agent compacted %d results under the wide budget", freshRun.Projection.CompactedToolResults)
	}

	// A history that does not extend the last one's starts from no fronts.
	other := append(frontExchanges(20, 30), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "elsewhere"})
	third, err := agent.Run(context.Background(), &CompletionRequest{Messages: other, MaxContextTokens: 300_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Projection.CompactedToolResults != 0 || third.Projection.OmittedExchanges != 0 {
		t.Fatalf("fronts carried to an unrelated history: %+v", third.Projection)
	}
}

// plainChat is a tool-free conversation: n exchanges whose user messages carry
// size bytes, then a question.
func plainChat(prefix string, n, size int) []messages.ChatMessage {
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "sys"}}
	for j := range n {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("%s-%d %s", prefix, j, strings.Repeat("q", size))},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: prefix + " noted"},
		)
	}
	return append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: prefix + " question"})
}

// An agent reused for an unrelated conversation of the same shape starts it
// from no fronts: what the user said tells the two apart.
func TestFrontsDoNotCarryIntoAnUnrelatedConversation(t *testing.T) {
	llm := &scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer("done") }}
	agent := NewAgent(llm, nil, AgentConfig{})
	defer agent.Close()
	first, err := agent.Run(context.Background(), &CompletionRequest{Messages: plainChat("A", 3, 8_000), MaxContextTokens: 3_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Projection.OmittedExchanges == 0 {
		t.Fatal("the first conversation omitted nothing")
	}
	second, err := agent.Run(context.Background(), &CompletionRequest{Messages: plainChat("B", 4, 20), MaxContextTokens: 3_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if omitted := second.Projection.OmittedExchanges; omitted != 0 {
		t.Fatalf("an unrelated conversation that fits had %d exchanges omitted", omitted)
	}
}

// A request without a budget omits nothing, whatever an earlier budgeted one
// of the conversation omitted.
func TestUnbudgetedRequestOmitsNothing(t *testing.T) {
	llm := &scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer("done") }}
	calibration := NewCalibration()
	agent := NewAgent(llm, nil, AgentConfig{Calibration: calibration})
	defer agent.Close()
	history := plainChat("A", 40, 120)
	first, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 1_200, CacheSessionID: "s1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Projection.OmittedExchanges == 0 {
		t.Fatal("the budgeted run omitted nothing")
	}
	next := append(append(slices.Clone(history), first.AllMessages...), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "follow-up"})
	second, err := agent.Run(context.Background(), &CompletionRequest{Messages: next, CacheSessionID: "s1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if omitted := second.Projection.OmittedExchanges; omitted != 0 {
		t.Fatalf("an unbudgeted request omitted %d exchanges", omitted)
	}
}
