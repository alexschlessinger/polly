package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// compactionLLM answers each request with what reply returns for it, or
// fails it with the error, and records every request it is sent.
type compactionLLM struct {
	mu       sync.Mutex
	reply    func(req *CompletionRequest, call int) (messages.ChatMessage, error)
	requests []*CompletionRequest
}

func (m *compactionLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	m.mu.Lock()
	call := len(m.requests)
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	reply, err := m.reply(req, call)
	if err != nil {
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: err}
		close(events)
		return events
	}
	input := make(chan messages.ChatMessage, 1)
	input <- reply
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

// isSummaryRequest reports whether req asks for a summary, as a transcript or
// in place.
func isSummaryRequest(req *CompletionRequest) bool {
	return len(req.Messages) > 0 && (req.Messages[0].Content == summaryPrompt || strings.Contains(req.Messages[len(req.Messages)-1].Content, summaryPrompt))
}

func reply(content string, input int) messages.ChatMessage {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: content, StopReason: messages.StopReasonEndTurn}
	if input > 0 {
		msg.SetTokenUsage(input, 10)
	}
	return msg
}

func callTool(id, name string, input int) messages.ChatMessage {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: name, Arguments: `{}`}}}
	if input > 0 {
		msg.SetTokenUsage(input, 10)
	}
	return msg
}

func resultOf(id, name, content string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: name, Content: content}
}

func TestContextViewReplacesSummarizedConversation(t *testing.T) {
	image := artifacts.Ref{ID: "sha256:" + strings.Repeat("a", 64), Kind: artifacts.KindImage, MIMEType: "image/png", Bytes: 5, ImageToken: "[image #1]"}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "be terse"},
		{Role: messages.MessageRoleUser, Content: "old request", Parts: []messages.ContentPart{imageArtifactPart(image)}},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current request"},
		callTool("c1", "fetch", 0),
		resultOf("c1", "fetch", "fetched"),
		messages.Compaction{Summary: "the user asked for something old", KeepsTurn: true}.Message(),
		{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyUsageOnly: true}},
		reply("continuing", 0),
	}
	view := contextView(history, builtinProjectionTools(true))
	var roles []string
	for _, msg := range view {
		roles = append(roles, msg.Role)
	}
	if got := strings.Join(roles, ","); got != "system,user,user,assistant,tool,assistant" {
		t.Fatalf("view roles = %s", got)
	}
	summary := view[1]
	if isRealUser(summary) || !strings.Contains(messageText(summary), "the user asked for something old") || !strings.Contains(messageText(summary), "read_transcript") {
		t.Fatalf("summary message = %+v", summary)
	}
	if summary.Content != "" || len(summary.Parts) != 2 || summary.Parts[0].Type != "text" || summary.Parts[1].Artifact.ID != image.ID {
		t.Fatalf("summary text does not lead its parts, or summarized images are not referable: %+v", summary)
	}
	if view[2].Content != "current request" || view[5].Content != "continuing" {
		t.Fatalf("the kept turn or what followed changed: %+v", view)
	}

	// Without the turn kept, the summary stands for everything before it.
	history[6] = messages.Compaction{Summary: "everything so far"}.Message()
	view = contextView(history, builtinProjectionTools(false))
	if len(view) != 3 || view[0].Role != messages.MessageRoleSystem || strings.Contains(view[1].Content, "read_transcript") || view[2].Content != "continuing" {
		t.Fatalf("view = %+v", view)
	}
}

func TestContextViewClearsResultsThroughTheMarkedCall(t *testing.T) {
	stored := artifacts.Ref{ID: "sha256:" + strings.Repeat("b", 64), Kind: artifacts.KindText, Bytes: 9_000, Lines: 300}
	large := strings.Repeat("output line\n", 1_000)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("plain", "fetch", 0), resultOf("plain", "fetch", large),
		callTool("stored", "fetch", 0),
		{Role: messages.MessageRoleTool, ToolCallID: "stored", ToolName: "fetch", Content: large, Parts: []messages.ContentPart{{Type: "artifact", Artifact: &stored}}},
		callTool("recall", "read_transcript", 0), resultOf("recall", "read_transcript", large),
		callTool("denied", "fetch", 0), resultOf("denied", "fetch", ToolDeniedContent),
		callTool("small", "fetch", 0), resultOf("small", "fetch", "ok"),
		messages.Compaction{ClearThrough: "small"}.Message(),
		callTool("later", "fetch", 0), resultOf("later", "fetch", large),
	}
	view := contextView(history, builtinProjectionTools(true))
	results := messagesWithRole(view, messages.MessageRoleTool)
	if !strings.Contains(results[0].Content, "Earlier tool output cleared") || !strings.Contains(results[0].Content, "read_transcript") {
		t.Fatalf("inline result = %q", results[0].Content)
	}
	if results[1].Content != artifactReceipt(stored) {
		t.Fatalf("stored result = %q, want its receipt", results[1].Content)
	}
	if results[2].Content != recallResultStub("read_transcript") {
		t.Fatalf("recall result = %q, want its stub", results[2].Content)
	}
	if results[3].Content != ToolDeniedContent || results[4].Content != "ok" || results[5].Content != large {
		t.Fatalf("denial, small result or later result changed: %q %q %d bytes", results[3].Content, results[4].Content, len(results[5].Content))
	}
	if history[2].Content != large {
		t.Fatal("the view rewrote durable history")
	}
}

func TestCountedTokensFollowsTheLastReportedRequest(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("c1", "fetch", 5_000),
		resultOf("c1", "fetch", "fetched result"),
		{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyUsageOnly: true}},
	}
	// The report moves by how far the estimate grew from the recorded
	// estimate of the request it answered, across compaction markers too.
	history[1].SetRequestEstimate(8_000)
	if n, ok := countedTokens(history, 8_600); !ok || n != 5_600 {
		t.Fatalf("counted = %d, %v; want the report moved by the estimate's growth", n, ok)
	}
	compacted := append(slices.Clone(history), messages.Compaction{Summary: "summary"}.Message())
	if n, ok := countedTokens(compacted, 6_500); !ok || n != 3_500 {
		t.Fatalf("counted = %d, %v; want the report less what compaction took out", n, ok)
	}
	history[1].Metadata = nil
	if _, ok := countedTokens(history, 9_000); ok {
		t.Fatal("a response without usage covered the request")
	}
	history[1].SetTokenUsage(5_000, 10)
	if _, ok := countedTokens(history, 9_000); ok {
		t.Fatal("a response without a recorded estimate covered the request")
	}
}

func TestClearPointNeverClearsUnreadResults(t *testing.T) {
	large := strings.Repeat("x", 8_000)
	view := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("old", "fetch", 0), resultOf("old", "fetch", large),
		callTool("new", "fetch", 0), resultOf("new", "fetch", large),
	}
	through, saved := clearPoint(view, builtinProjectionTools(true), 100)
	if through != "old" || saved < 1_900 {
		t.Fatalf("clear point = %q saving %d; want the read result only", through, saved)
	}
	if through, _ := clearPoint(view[:3], builtinProjectionTools(true), 100); through != "" {
		t.Fatalf("cleared %q, which the model has not read", through)
	}
	// An id that recurs later is never the anchor: the marker would resolve
	// to the later result, which the model may not have read.
	reused := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("call_0", "fetch", 0), resultOf("call_0", "fetch", large),
		reply("done", 0),
		{Role: messages.MessageRoleUser, Content: "again"},
		callTool("call_0", "fetch", 0), resultOf("call_0", "fetch", large),
	}
	if through, _ := clearPoint(reused, builtinProjectionTools(true), 100); through != "" {
		t.Fatalf("clear point = %q, an id the newest, unread result reuses", through)
	}
	// A denial is never the anchor: hosts strip it before saving.
	denied := append(append([]messages.ChatMessage{}, view[:3]...), callTool("denied", "fetch", 0), resultOf("denied", "fetch", ToolDeniedContent), callTool("new", "fetch", 0), resultOf("new", "fetch", large))
	if through, _ := clearPoint(denied, builtinProjectionTools(true), 1_000); through != "old" {
		t.Fatalf("clear point = %q, want the result before the denial", through)
	}
}

func TestAgentClearsOldToolResultsBeforeSummarizing(t *testing.T) {
	large := strings.Repeat("x", 20_000)
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			t.Error("cleared results were enough, but a summary was requested")
		}
		if call == 0 {
			return callTool("new", "fetch", 6_000), nil
		}
		return reply("done", 0), nil
	}}
	fetch := &tools.Func{Name: "fetch", Run: func(context.Context, tools.Args) (string, error) { return strings.Repeat("y", 2_000), nil }}
	registry := tools.NewToolRegistry([]tools.Tool{fetch})
	agent := NewAgent(model, registry, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old"},
		callTool("old", "fetch", 0), resultOf("old", "fetch", large),
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "again"},
	}
	var notes []string
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 6_500}, &AgentCallbacks{
		OnAdaptation: func(note RequestAdaptation) { notes = append(notes, note.Message) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var marker messages.Compaction
	for _, msg := range response.AllMessages {
		if c, ok := msg.Compaction(); ok {
			marker = c
		}
	}
	if marker.ClearThrough != "old" || marker.Summary != "" {
		t.Fatalf("marker = %+v, want old results cleared", marker)
	}
	sent := messagesWithRole(model.requests[1].Messages, messages.MessageRoleTool)
	if len(sent) != 2 || !strings.Contains(sent[0].Content, "cleared") || sent[1].Content != strings.Repeat("y", 2_000) {
		t.Fatalf("second request's tool results = %+v", sent)
	}
	if len(notes) != 1 || notes[0] != "Context compacted · tool results cleared · 6,512 → 1,538 tokens" {
		t.Fatalf("notes = %q", notes)
	}
}

func TestAgentSummarizesWithTheCompactionModel(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			if req.Model != "cheap/summarizer" || len(req.Tools) != 0 || !strings.Contains(req.Messages[1].Content, "old request") || strings.Contains(req.Messages[1].Content, "current request") {
				t.Errorf("summary request = %+v", req)
			}
			return reply("SUMMARY: the user wanted the old thing", 900), nil
		}
		return reply("answer", 100), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "cheap/summarizer"})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request " + strings.Repeat("x", 12_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "current request"},
	}
	var reported []string
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "main/model", Messages: history, MaxContextTokens: 3_000}, &AgentCallbacks{
		OnCompactionUsage: func(model string, usage UsageUpdate) {
			reported = append(reported, fmt.Sprintf("%s %d", model, usage.InputTokens))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 || reported[0] != "cheap/summarizer 900" {
		t.Fatalf("compaction usage reported = %q", reported)
	}
	if len(model.requests) != 2 || !isSummaryRequest(model.requests[0]) {
		t.Fatalf("requests = %d", len(model.requests))
	}
	sent := model.requests[1]
	if sent.Model != "main/model" || len(sent.Messages) != 2 || !strings.Contains(sent.Messages[0].Content, "SUMMARY: the user wanted the old thing") || sent.Messages[1].Content != "current request" {
		t.Fatalf("request after the summary = %+v", sent.Messages)
	}
	if len(response.AllMessages) != 3 || !response.AllMessages[0].IsUsageRecord() {
		t.Fatalf("generated = %+v", response.AllMessages)
	}
	// The summary's provider reported no cache use: the record says none,
	// so the conversation's cache rate stays known.
	if _, ok := response.AllMessages[0].Metadata[messages.MetadataKeyCacheReadInputTokens]; !ok {
		t.Fatalf("usage record without cache counts: %+v", response.AllMessages[0])
	}
	if c, ok := response.AllMessages[1].Compaction(); !ok || !c.KeepsTurn {
		t.Fatalf("marker = %+v", response.AllMessages[1])
	}
	usage := response.TokenUsage()
	if usage.TotalInput != 1_000 || usage.PeakInput != 100 || usage.Compaction != (CompactionUsage{Model: "cheap/summarizer", Input: 900, Output: 10}) {
		t.Fatalf("usage = %+v, want the summary in the totals and its share, not in the peak", usage)
	}
}

func TestAgentCompactsAndResendsARejectedRequest(t *testing.T) {
	overflow := errors.New("This model's maximum context length is 8000 tokens. However, your messages resulted in 9000 tokens.")
	sends := 0
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			return reply("the earlier conversation", 0), nil
		}
		sends++
		if sends == 1 {
			return messages.ChatMessage{}, overflow
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 30_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sends != 2 || response.Message.Content != "answer" {
		t.Fatalf("sends = %d, response = %+v", sends, response.Message)
	}
	if c, ok := response.AllMessages[1].Compaction(); !ok || c.Summary != "the earlier conversation" {
		t.Fatalf("generated = %+v", response.AllMessages)
	}
	// The rejected request is not sent again to be summarized: the
	// summary goes as a transcript.
	if summary := model.requests[1]; summary.Messages[0].Content != summaryPrompt {
		t.Fatalf("summary of a rejected request = %+v", summary.Messages)
	}

	// A conversation with nothing to compact surfaces the rejection.
	sends = 0
	model.requests = nil
	_, err = NewAgent(model, nil, AgentConfig{}).Run(context.Background(), &CompletionRequest{Messages: messages.User("new")}, nil)
	var rejected *ContextOverflowError
	if !errors.As(err, &rejected) || sends != 1 || len(model.requests) != 1 {
		t.Fatalf("err = %v after %d requests", err, len(model.requests))
	}
}

// inPlaceTest is a run whose second request must compact: a tool result
// pushes it over the trigger, with a turn too large to keep. summary answers
// the requests for a summary; cb, when set, observes the run.
func inPlaceTest(t *testing.T, summary func(req *CompletionRequest) (messages.ChatMessage, error), cb *AgentCallbacks) (*compactionLLM, *AgentResponse) {
	t.Helper()
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		switch {
		case isSummaryRequest(req):
			return summary(req)
		case call == 0:
			return callTool("c1", "fetch", 0), nil
		}
		return reply("done", 0), nil
	}}
	fetch := &tools.Func{Name: "fetch", Run: func(context.Context, tools.Args) (string, error) { return strings.Repeat("r", 8_000), nil }}
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{fetch}), AgentConfig{})
	t.Cleanup(func() { agent.Close() })
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "SYSTEM"},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 6_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	req := &CompletionRequest{Model: "m/m", Messages: history, MaxContextTokens: 4_000, MaxTokens: 2_000, CacheSessionID: "session"}
	response, err := agent.Run(context.Background(), req, cb)
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	return model, response
}

// The request's own model summarizes a request that fits sent as it is,
// with the request for a summary appended: the same system prompt, tools,
// settings and cache keys, so the provider's prompt cache covers the
// conversation the last request sent.
func TestAgentSummarizesInPlaceOnItsOwnModel(t *testing.T) {
	model, response := inPlaceTest(t, func(*CompletionRequest) (messages.ChatMessage, error) {
		summary := reply("SUMMARY", 3_000)
		summary.SetPromptCacheUsage(2_900, 0)
		return summary, nil
	}, nil)
	if len(model.requests) != 3 || !isSummaryRequest(model.requests[1]) {
		t.Fatalf("requests = %d", len(model.requests))
	}
	first, summary := model.requests[0], model.requests[1]
	if len(summary.Messages) <= len(first.Messages) || !reflect.DeepEqual(summary.Messages[:len(first.Messages)], first.Messages) {
		t.Fatalf("the summary request (%d messages) does not extend the last request (%d)", len(summary.Messages), len(first.Messages))
	}
	names := func(list []tools.Tool) []string {
		var out []string
		for _, tool := range list {
			out = append(out, tool.GetSchema().Title())
		}
		return out
	}
	if first.PromptCacheKey == "" || summary.PromptCacheKey != first.PromptCacheKey || summary.CacheSessionID != "session" ||
		summary.MaxTokens != first.MaxTokens || len(first.Tools) == 0 || !slices.Equal(names(summary.Tools), names(first.Tools)) {
		t.Fatalf("summary request key %q session %q max %d tools %d, want key %q max %d tools %d", summary.PromptCacheKey, summary.CacheSessionID, summary.MaxTokens, len(summary.Tools), first.PromptCacheKey, first.MaxTokens, len(first.Tools))
	}
	if ask := summary.Messages[len(summary.Messages)-1]; ask.Role != messages.MessageRoleUser || !strings.Contains(ask.Content, "Do not call any tools") {
		t.Fatalf("request for the summary = %+v", ask)
	}
	var marker messages.Compaction
	for _, msg := range response.AllMessages {
		if c, ok := msg.Compaction(); ok {
			marker = c
		}
	}
	if marker.Summary != "SUMMARY" || response.TokenUsage().TotalInput != 3_000 {
		t.Fatalf("marker = %+v, usage = %+v", marker, response.TokenUsage())
	}
	// What it read from the cache counts in the run's cache use.
	if response.PromptCache.ReadInputTokens != 2_900 {
		t.Fatalf("prompt cache = %+v, want the summary's 2,900 cached tokens", response.PromptCache)
	}
}

// A model that answers the request for a summary with a tool call is asked
// again with a transcript, which carries no tools.
func TestInPlaceSummaryThatCallsAToolFallsBackToTheTranscript(t *testing.T) {
	model, response := inPlaceTest(t, func(req *CompletionRequest) (messages.ChatMessage, error) {
		if req.Messages[0].Content == summaryPrompt {
			return reply("SUMMARY", 500), nil
		}
		return callTool("c2", "fetch", 3_000), nil
	}, nil)
	if len(model.requests) != 4 || model.requests[2].Messages[0].Content != summaryPrompt || len(model.requests[2].Tools) != 0 {
		t.Fatalf("requests = %d", len(model.requests))
	}
	for _, msg := range response.AllMessages {
		if c, ok := msg.Compaction(); ok && c.Summary != "SUMMARY" {
			t.Fatalf("marker = %+v", c)
		}
	}
	if usage := response.TokenUsage(); usage.TotalInput != 3_500 {
		t.Fatalf("usage = %+v, want both summary attempts", usage)
	}
}

// A request for a summary in place that the provider refuses, as too long or
// for its shape, is asked again as a transcript.
func TestInPlaceSummaryThatFailsFallsBackToTheTranscript(t *testing.T) {
	for _, refusal := range []error{
		errors.New("prompt is too long: 6100 tokens > 6000 maximum"),
		errors.New("400 Bad Request: conversation roles must alternate user/assistant"),
	} {
		model, response := inPlaceTest(t, func(req *CompletionRequest) (messages.ChatMessage, error) {
			if req.Messages[0].Content == summaryPrompt {
				return reply("SUMMARY", 500), nil
			}
			return messages.ChatMessage{}, refusal
		}, nil)
		if len(model.requests) != 4 || model.requests[2].Messages[0].Content != summaryPrompt {
			t.Fatalf("%v: requests = %d", refusal, len(model.requests))
		}
		for _, msg := range response.AllMessages {
			if c, ok := msg.Compaction(); ok && c.Summary != "SUMMARY" {
				t.Fatalf("%v: marker = %+v", refusal, c)
			}
		}
	}
}

// Input admitted with the request that compacts stays verbatim after the
// summary, so the summary is asked as a transcript, which leaves it out.
func TestAgentSummarizesAsATranscriptWhenInputArrives(t *testing.T) {
	admits := 0
	cb := &AgentCallbacks{AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
		if admits++; admits == 2 {
			return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "PEER INPUT"}}, nil
		}
		return nil, nil
	}}
	model, _ := inPlaceTest(t, func(req *CompletionRequest) (messages.ChatMessage, error) {
		if req.Messages[0].Content != summaryPrompt {
			t.Error("the request carrying admitted input was summarized in place")
		} else if strings.Contains(req.Messages[1].Content, "PEER INPUT") {
			t.Error("the summary transcript carries the admitted input")
		}
		return reply("SUMMARY", 500), nil
	}, cb)
	if last := model.requests[len(model.requests)-1]; !slices.ContainsFunc(last.Messages, func(msg messages.ChatMessage) bool { return msg.Content == "PEER INPUT" }) {
		t.Fatalf("the admitted input was not sent after the summary: %+v", last.Messages)
	}
}

func TestSummaryFallsBackToClearedTranscript(t *testing.T) {
	large := strings.Repeat("z", 40_000)
	var transcripts []string
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		transcripts = append(transcripts, req.Messages[1].Content)
		if strings.Contains(req.Messages[1].Content, large) {
			return messages.ChatMessage{}, errors.New("prompt is too long: 300000 tokens > 200000 maximum")
		}
		return reply("summary", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "other/model"})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("c1", "fetch", 0), resultOf("c1", "fetch", large), reply("fetched", 0),
	}
	summary, _, err := agent.summarize(context.Background(), &CompletionRequest{Model: "main/model"}, "other/model", history, builtinProjectionTools(true), 1_000)
	if err != nil || summary != "summary" || len(transcripts) != 2 || !strings.Contains(transcripts[1], "Earlier tool output cleared") {
		t.Fatalf("summary = %q, %v after %d attempts", summary, err, len(transcripts))
	}
}

func TestAgentRefusesWhatNoCompactionCanFit(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		t.Error("a model was called for a request nothing can bring within budget")
		return reply("", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	// The system prompt alone is over the budget: no summary can help.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("rule ", 2_000)},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 8_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, &AgentCallbacks{
		BeforeFirstRequest: func(ProjectionStats) error {
			t.Error("the hook ran for a request that cannot be sent")
			return nil
		},
	})
	var limit *ContextLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want a context limit error", err)
	}
}

// planTest is a run over history, as far as the first request's gate.
func planTest(t *testing.T, history []messages.ChatMessage, budget int) (*agentRun, *CompletionRequest) {
	t.Helper()
	agent := NewAgent(&compactionLLM{}, nil, AgentConfig{})
	t.Cleanup(func() { agent.Close() })
	req := &CompletionRequest{Model: "test/model", Messages: history, MaxContextTokens: budget}
	return agent.newRun(req, nil), req
}

func TestPlanCompactionSkipsSummariesThatCannotHelp(t *testing.T) {
	// System prompt about 8,800 tokens of a 10,000 budget: a request at
	// 9,000 fits, and no summary could bring it under the trigger, so the
	// conversation is left alone rather than summarized on every turn.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("x", 35_200)},
		{Role: messages.MessageRoleUser, Content: "q1"},
		reply("a1 "+strings.Repeat("y", 600), 0),
		{Role: messages.MessageRoleUser, Content: "q2"},
	}
	r, req := planTest(t, history, 10_000)
	if plan := r.planCompaction(req, 9_000, false, false); !plan.none() {
		t.Fatalf("plan = %+v, want none", plan)
	}
}

func TestPlanCompactionDropsATurnThatCannotFitKept(t *testing.T) {
	// The turn, under a quarter of the 2,000 budget, is small enough to
	// keep, but system + summary + turn would be over the budget; summarizing
	// it too fits.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("x", 5_600)},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("o", 4_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new " + strings.Repeat("n", 400)},
		callTool("c1", "fetch", 0),
		resultOf("c1", "fetch", strings.Repeat("r", 1_400)),
	}
	r, req := planTest(t, history, 2_000)
	plan := r.planCompaction(req, 2_900, false, false)
	if !plan.summarize || plan.keepsTurn {
		t.Fatalf("plan = %+v, want a summary that covers the turn", plan)
	}
	// Input not yet committed is never summarized: with nothing that fits
	// while keeping it, nothing is planned and the request is refused.
	if plan := r.planCompaction(req, 2_900, false, true); !plan.none() {
		t.Fatalf("fresh plan = %+v, want none", plan)
	}
}

// A summary on the request's own model is sized to fit the request's output
// limit, so it is not cut off and refused.
func TestPlanCompactionSizesSummariesToTheOutputLimit(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("o", 120_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	r, req := planTest(t, history, 40_000)
	req.MaxTokens = 2_000
	if plan := r.planCompaction(req, 35_000, false, false); !plan.summarize || plan.target != 1_000 {
		t.Fatalf("plan = %+v, want a summary in 1,000 tokens", plan)
	}
	r.agent.config.CompactionModel = "other/model"
	if plan := r.planCompaction(req, 35_000, false, false); !plan.summarize || plan.target != 4_000 {
		t.Fatalf("another model's plan = %+v, want a summary in 4,000 tokens", plan)
	}
}

// A prompt too large to send, in a session with history, is refused before
// the caller commits it rather than summarized away.
func TestAgentRefusesAnOversizedPromptBeforeItIsCommitted(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		t.Error("a model was called for a prompt that cannot be sent")
		return reply("", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "earlier"},
		reply("earlier answer", 0),
		{Role: messages.MessageRoleUser, Content: strings.Repeat("p", 64_000)},
	}
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 6_000}, &AgentCallbacks{
		BeforeFirstRequest: func(ProjectionStats) error {
			t.Error("the prompt was cleared for commit")
			return nil
		},
	})
	var limit *ContextLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want a context limit error", err)
	}
}

// The provider counts the system prompt and tools well below their estimate,
// as GLM counts polly's. A summary that brings the real request well under
// budget must not be refused because the estimate of the whole request,
// which still prices the system prompt high, is over it.
func TestAgentSizesCompactedRequestsByTheProviderCount(t *testing.T) {
	const budget = 6_000
	system := strings.Repeat("rule ", 4_400) // about 5,500 estimated tokens
	reported := func(req *CompletionRequest) int {
		total := estimateToolSchemaTokens(req.Tools)
		for _, msg := range req.Messages {
			total += estimateMessageTokensWith(msg, false)
		}
		return total * 6 / 10 // the provider counts 40% below the estimate
	}
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			return reply(strings.Repeat("summary ", 300), 0), nil // about 600 estimated tokens
		}
		if call >= 10 {
			return reply("done", reported(req)), nil
		}
		msg := callTool(fmt.Sprintf("c%d", call), "fetch", reported(req))
		msg.Content = strings.Repeat("progress ", 220) // about 500 estimated tokens a step
		return msg, nil
	}}
	fetch := &tools.Func{Name: "fetch", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}
	registry := tools.NewToolRegistry([]tools.Tool{fetch})
	agent := NewAgent(model, registry, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: system}, {Role: messages.MessageRoleUser, Content: "go"}}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: budget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	summarized := false
	for _, msg := range response.AllMessages {
		if c, ok := msg.Compaction(); ok && c.Summary != "" {
			summarized = true
		}
	}
	if !summarized {
		t.Fatal("the fixture did not summarize")
	}
	if counted := response.Projection.CountedTokens; counted <= 0 || counted > budget || counted >= response.Projection.RequestEstimatedTokens || !response.Projection.Counted {
		t.Fatalf("last request counted at %d (estimated %d), want the provider's count under the %d budget", counted, response.Projection.RequestEstimatedTokens, budget)
	}
}

// Providers send only a message's parts once it has any. For a model that
// cannot view images, preparation turns the summary's carried images into
// text parts, so the summary text must be a part too.
func TestSummaryTextSurvivesAModelThatCannotViewImages(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "shot.png", Reference: "[image #1]", Data: []byte("png")})
	model := &metadataRecordingLLM{info: ModelInfo{ModelCapabilities: ModelCapabilities{InputModalities: []string{"text"}}}}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "look"}, imageArtifactPart(ref)}},
		reply("seen", 0),
		{Role: messages.MessageRoleUser, Content: "next"},
		reply("done", 0),
		messages.Compaction{Summary: "SUMMARY-TEXT"}.Message(),
		{Role: messages.MessageRoleUser, Content: "continue"},
	}
	if _, err := agent.Run(context.Background(), &CompletionRequest{Model: "custom/m", Messages: history}, nil); err != nil {
		t.Fatal(err)
	}
	summary := model.requests[0][0]
	if len(summary.Parts) == 0 || !strings.Contains(summary.Parts[0].Text, "SUMMARY-TEXT") {
		t.Fatalf("summary text is not among the parts providers send: %+v", summary)
	}
}

// A request between the trigger and its budget fits as it is: a compaction
// model that cannot answer costs the headroom, not the turn.
func TestAgentSendsAFittingRequestWhenCompactionFails(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			return messages.ChatMessage{}, errors.New("missing API key for the compaction model")
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "other/model"})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 7_300)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	var notes []string
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, &AgentCallbacks{
		OnAdaptation: func(note RequestAdaptation) { notes = append(notes, note.Message) },
	})
	if err != nil || response.Message.Content != "answer" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	if last := notes[len(notes)-1]; !strings.Contains(last, "Compaction failed") || !strings.Contains(last, "missing API key") {
		t.Fatalf("notes = %q", notes)
	}
	for _, msg := range response.AllMessages {
		if _, ok := msg.Compaction(); ok {
			t.Fatalf("a failed compaction left a marker: %+v", response.AllMessages)
		}
	}
}

// A summary covering a recall result the model has not acted on yet carries
// what it read, rather than leaving the model to read it again.
func TestSummaryTranscriptCarriesRecallResults(t *testing.T) {
	var transcript string
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		transcript = req.Messages[1].Content
		return reply("summary", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "SYSTEM-RULES"},
		{Role: messages.MessageRoleUser, Content: "find the code"},
		callTool("r1", "read_transcript", 0),
		resultOf("r1", "read_transcript", "1: the code is NEEDLE-4411"),
	}
	if _, _, err := agent.summarize(context.Background(), &CompletionRequest{Model: "m/m"}, "m/m", history, builtinProjectionTools(true), 1_000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(transcript, "NEEDLE-4411") {
		t.Fatalf("the summary transcript elided the recall result: %q", transcript)
	}
	// Requests keep the system messages beside the summary; the summary
	// transcript leaves them out.
	if strings.Contains(transcript, "SYSTEM-RULES") {
		t.Fatalf("the summary transcript carries the system prompt: %q", transcript)
	}
}

// A summary that covers the request in progress keeps sending its images:
// the model is still working from them.
func TestSummaryCoveringTheTurnKeepsItsImages(t *testing.T) {
	store := newTestArtifactStore()
	shot := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "bug.png", Reference: "[image #1]", Data: []byte("bug")})
	old := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "old.png", Reference: "[image #2]", Data: []byte("old")})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "earlier"}, imageArtifactPart(old)}},
		reply("seen", 0),
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "fix the layout in this screenshot"}, imageArtifactPart(shot)}},
		callTool("c1", "read_file", 0),
		resultOf("c1", "read_file", strings.Repeat("css ", 4_000)),
		messages.Compaction{Summary: "the user wants a layout fix"}.Message(),
	}
	projected, stats, err := projectMessages(context.Background(), history, store, true)
	if err != nil {
		t.Fatal(err)
	}
	images := projectedImageParts(projected)
	if stats.HydratedImages != 1 || len(images) != 1 {
		t.Fatalf("hydrated %d images, want the turn's screenshot alone: %+v", stats.HydratedImages, projected)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(images[0].ImageData); string(decoded) != "bug" {
		t.Fatalf("hydrated %q, want the turn's screenshot", decoded)
	}
}

// A rejection states the window the provider enforces: the request is sent
// again within it, clearing the read result that brings it under, and the
// run's later requests keep within it too.
func TestAgentKeepsWithinTheWindowARejectionStates(t *testing.T) {
	sends := 0
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			t.Error("clearing the read result was enough, but a summary was requested")
			return reply("summary", 0), nil
		}
		sends++
		if sends == 1 {
			return messages.ChatMessage{}, errors.New("This model's maximum context length is 8400 tokens. However, your messages resulted in 10080 tokens.")
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("read", "fetch", 0), resultOf("read", "fetch", strings.Repeat("a", 20_000)),
		callTool("new", "fetch", 0), resultOf("new", "fetch", strings.Repeat("b", 20_000)),
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 100_000}, nil)
	if err != nil || response.Message.Content != "answer" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	var marker messages.Compaction
	for _, msg := range response.AllMessages {
		if c, ok := msg.Compaction(); ok {
			marker = c
		}
	}
	if marker.ClearThrough != "read" {
		t.Fatalf("marker = %+v, want the read result cleared", marker)
	}
	limit := ClampContextBudget(8_400, 8_400, 0)
	if response.Projection.CountedTokens > limit {
		t.Fatalf("resent request at %d tokens, over the stated window's %d", response.Projection.CountedTokens, limit)
	}
	// The budget the agent applied is reported with the request.
	if stats := response.Projection; stats.Budget != limit || !stats.Learned {
		t.Fatalf("projection budget = %d (learned %v), want %d learned", stats.Budget, stats.Learned, limit)
	}
	// Later runs to the same route start within the window it stated.
	sent := len(model.requests)
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "next"}}, MaxContextTokens: 100_000}, nil); err != nil {
		t.Fatal(err)
	}
	if next := model.requests[sent]; next.MaxContextTokens != limit {
		t.Fatalf("the next run's request had a %d-token budget, want %d", next.MaxContextTokens, limit)
	}
}

// A rejection that an output reserve the window cannot hold caused is
// answered by cutting the reserve, not by compacting, and later runs on the
// same route start within what it showed.
func TestAgentCutsAnOutputReserveTheWindowCannotHold(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			t.Error("the input fit, but a summary was requested")
			return reply("summary", 0), nil
		}
		if req.MaxTokens > 20_000 {
			return messages.ChatMessage{}, fmt.Errorf("This model's maximum context length is 32768 tokens. However, you requested %d tokens (2000 in the messages, %d in the completion).", 2_000+req.MaxTokens, req.MaxTokens)
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "go"}}
	var notes []string
	cb := &AgentCallbacks{OnAdaptation: func(note RequestAdaptation) { notes = append(notes, note.Message) }}
	req := &CompletionRequest{Model: "local/m", Messages: history, MaxTokens: 64_000}
	if response, err := agent.Run(context.Background(), req, cb); err != nil || response.Message.Content != "answer" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	budget := ClampContextBudget(32_768, 32_768, 64_000)
	output := 32_768 - budget
	if len(model.requests) != 2 || model.requests[1].MaxTokens != output || !strings.Contains(notes[len(notes)-1], fmt.Sprintf("output limited to %d tokens", output)) {
		t.Fatalf("requests = %d, resent with %d output tokens; notes = %q", len(model.requests), model.requests[len(model.requests)-1].MaxTokens, notes)
	}
	if _, err := agent.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if next := model.requests[2]; len(model.requests) != 3 || next.MaxTokens != output || next.MaxContextTokens != budget {
		t.Fatalf("the next run sent %d requests, the first with %d output and %d budget", len(model.requests)-2, next.MaxTokens, next.MaxContextTokens)
	}
}

// A rejection on a provider whose window bounds input alone teaches the
// window itself, keeping no room in it for the reply.
func TestAgentLearnsAnInputWindowWhole(t *testing.T) {
	sends := 0
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			return reply("summary", 0), nil
		}
		if sends++; sends == 1 {
			return messages.ChatMessage{}, errors.New("The input token count (10080) exceeds the maximum number of tokens allowed (8400).")
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 30_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "gemini/m", Messages: history, MaxTokens: 4_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats := response.Projection; stats.Budget != 8_400 || stats.MaxTokens != 4_000 || !stats.Learned {
		t.Fatalf("projection budget %d, max tokens %d (learned %v); want the 8,400 window and the output limit unchanged", stats.Budget, stats.MaxTokens, stats.Learned)
	}
}

// A compaction model whose window is unknown and smaller than the transcript
// gets ever shorter transcripts, never the same one twice.
func TestSummaryShrinksTheTranscriptWithoutRepeatingIt(t *testing.T) {
	var sizes []int
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		sizes = append(sizes, len(req.Messages[1].Content))
		if len(req.Messages[1].Content) > 7_000 {
			return messages.ChatMessage{}, errors.New("prompt is too long: 30000 tokens > 2000 maximum")
		}
		return reply("summary", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "other/model"})
	defer agent.Close()
	// A plain chat: clearing changes nothing.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 20_000)},
		reply("answer", 0),
	}
	summary, _, err := agent.summarize(context.Background(), &CompletionRequest{Model: "main/model"}, "other/model", history, builtinProjectionTools(true), 1_000)
	if err != nil || summary != "summary" {
		t.Fatalf("summary = %q, %v after attempts %v", summary, err, sizes)
	}
	if len(sizes) != 3 || sizes[0] <= sizes[1] || sizes[1] <= sizes[2] {
		t.Fatalf("attempt sizes = %v, want three, each shorter", sizes)
	}
}

// Once the model's window is known, the transcript is cut to what it leaves,
// and to half that when dense text undercuts the estimate.
func TestSummaryCutsToTheKnownWindowAndThenHalfIt(t *testing.T) {
	var sizes []int
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		sizes = append(sizes, len(req.Messages[1].Content))
		if len(req.Messages[1].Content) > 7_000 {
			return messages.ChatMessage{}, errors.New("prompt is too long: 3500 tokens > 3000 maximum")
		}
		return reply("summary", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 20_000)},
		reply("answer", 0),
	}
	req := &CompletionRequest{Model: "m/m", MaxContextTokens: 3_000}
	limit := agent.summaryLimit(context.Background(), req, req.Model, 256)
	summary, _, err := agent.summarize(context.Background(), req, req.Model, history, builtinProjectionTools(true), 256)
	if err != nil || summary != "summary" {
		t.Fatalf("summary = %q, %v after attempts %v", summary, err, sizes)
	}
	// The first attempt is the cut the window leaves: the full transcript
	// does not fit it.
	if len(sizes) != 2 || sizes[0] > limit*4+200 || sizes[0] <= 7_000 || sizes[1] > limit*2+200 {
		t.Fatalf("attempt sizes = %v, want a cut to %d bytes, then to %d", sizes, limit*4, limit*2)
	}
}

// The results after the model's last reply, which it has yet to act on, stay
// whole when the transcript's read results are cleared.
func TestClearedSummaryTranscriptKeepsUnreadResults(t *testing.T) {
	large := strings.Repeat("z", 40_000)
	var transcripts []string
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		transcripts = append(transcripts, req.Messages[1].Content)
		if strings.Contains(req.Messages[1].Content, large) {
			return messages.ChatMessage{}, errors.New("prompt is too long: 300000 tokens > 200000 maximum")
		}
		return reply("summary", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "other/model"})
	defer agent.Close()
	page := "1: the code is NEEDLE-4411\n" + strings.Repeat("p", 4_000)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		callTool("c1", "fetch", 0), resultOf("c1", "fetch", large),
		callTool("r1", "read_transcript", 0), resultOf("r1", "read_transcript", page),
	}
	if _, _, err := agent.summarize(context.Background(), &CompletionRequest{Model: "main/model"}, "other/model", history, builtinProjectionTools(true), 1_000); err != nil {
		t.Fatal(err)
	}
	last := transcripts[len(transcripts)-1]
	if len(transcripts) != 2 || !strings.Contains(last, "Earlier tool output cleared") || !strings.Contains(last, "NEEDLE-4411") {
		t.Fatalf("cleared transcript (attempt %d) = %.400q", len(transcripts), last)
	}
}

// A summary stream that dies before showing anything is sent again; a
// summary cut off at the output limit is refused.
func TestSummaryRetriesDeadStreamsAndRefusesCutOffSummaries(t *testing.T) {
	calls := 0
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		calls++
		if calls == 1 {
			return messages.ChatMessage{}, io.ErrUnexpectedEOF
		}
		return reply("summary", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "go"}, reply("done", 0)}
	if summary, _, err := agent.summarize(context.Background(), &CompletionRequest{Model: "m/m"}, "m/m", history, builtinProjectionTools(true), 1_000); err != nil || summary != "summary" || calls != 2 {
		t.Fatalf("summary = %q, %v after %d calls", summary, err, calls)
	}
	model.reply = func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		cut := reply("summary without its last section", 0)
		cut.StopReason = messages.StopReasonMaxTokens
		return cut, nil
	}
	if _, _, err := agent.summarize(context.Background(), &CompletionRequest{Model: "m/m"}, "m/m", history, builtinProjectionTools(true), 1_000); err == nil || !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("cut-off summary: err = %v", err)
	}
}

// Artifacts that admitted and continuation input carry are the
// conversation's: read_artifact opens them.
func TestAgentReadsArtifactsFromAdmittedAndContinuationInput(t *testing.T) {
	for _, via := range []string{"admitted", "continuation"} {
		t.Run(via, func(t *testing.T) {
			store := newTestArtifactStore()
			ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "evidence.txt", Data: []byte("peer evidence body")})
			input := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "see " + ref.ID, Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}}
			var result messages.ChatMessage
			model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleTool && msg.ToolName == "read_artifact" {
						return reply("done", 0), nil
					}
				}
				if strings.Contains(projectedText(req.Messages), ref.ID) {
					msg := callTool("read", "read_artifact", 0)
					msg.ToolCalls[0].Arguments = fmt.Sprintf(`{"id":%q}`, ref.ID)
					return msg, nil
				}
				return reply("first answer", 0), nil
			}}
			agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
			defer agent.Close()
			admitted, continued := false, false
			cb := &AgentCallbacks{OnToolResult: func(_ messages.ChatMessageToolCall, msg messages.ChatMessage) { result = msg }}
			if via == "admitted" {
				cb.AdmitInput = func(context.Context) ([]messages.ChatMessage, error) {
					if admitted {
						return nil, nil
					}
					admitted = true
					return []messages.ChatMessage{input}, nil
				}
			} else {
				cb.ContinueAfterFinal = func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
					if continued {
						return nil, nil
					}
					continued = true
					return []messages.ChatMessage{input}, nil
				}
			}
			if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("go")}, cb); err != nil {
				t.Fatal(err)
			}
			if succeeded, known := result.ToolSucceeded(); !known || !succeeded || !strings.Contains(result.Content, "peer evidence body") {
				t.Fatalf("read_artifact result = %+v", result)
			}
		})
	}
}

// An iteration that compacts builds its request twice; its preparation notes
// are reported once.
func TestAgentReportsPreparationNotesOncePerIteration(t *testing.T) {
	model := &metadataRecordingLLM{info: ModelInfo{ModelCapabilities: ModelCapabilities{InputModalities: []string{"text"}}}}
	model.responses = []messages.ChatMessage{reply("the earlier conversation", 0), reply("answer", 0)}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 12_000), Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "data:image/png;base64,iVBORw0KGgo=", FileName: "old.png"}}},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	var images int
	_, err := agent.Run(context.Background(), &CompletionRequest{Model: "custom/m", Messages: history, MaxContextTokens: 3_000}, &AgentCallbacks{
		OnAdaptation: func(note RequestAdaptation) {
			if note.Feature == "images" {
				images++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if images != 1 {
		t.Fatalf("images note reported %d times in one iteration, want once", images)
	}
}

// A cleared note counts lines as a receipt of the same output does.
func TestClearedNoteCountsLinesLikeAReceipt(t *testing.T) {
	output := strings.Repeat("output line\n", 1_000)
	ref := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindText, Data: []byte(output)})
	note, _ := clearedForm(resultOf("c1", "fetch", output), builtinProjectionTools(true))
	if want := fmt.Sprintf("%d lines", ref.Lines); !strings.Contains(note, want) {
		t.Fatalf("note = %q, want %s as the receipt says", note, want)
	}
}

// A run makes each cleared note once; a result with the same id but other
// content gets a note of its own.
func TestClearedNotesAreKeptForTheRun(t *testing.T) {
	p := builtinProjectionTools(true)
	p.notes = map[clearedKey]string{}
	first := resultOf("c1", "fetch", strings.Repeat("a\n", 3_000))
	note, ok := clearedForm(first, p)
	if !ok || len(p.notes) != 1 || !strings.Contains(note, "3000 lines") {
		t.Fatalf("note = %q, kept %d", note, len(p.notes))
	}
	if again, _ := clearedForm(first, p); again != note || len(p.notes) != 1 {
		t.Fatalf("the kept note was not reused: %q", again)
	}
	other := resultOf("c1", "fetch", strings.Repeat("b", 6_000))
	if note, _ := clearedForm(other, p); !strings.Contains(note, "1 lines") || len(p.notes) != 2 {
		t.Fatalf("another result reused a kept note: %q", note)
	}
}

// A second summary in the same turn still sends the request's images, and
// so does one over the images the last tool call returned; once the turn is
// over, they go back to being referable, with no message of their own.
func TestSummariesKeepTheRequestsImagesOnlyWhileItIsInProgress(t *testing.T) {
	store := newTestArtifactStore()
	shot := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "bug.png", Reference: "[image #1]", Data: []byte("bug")})
	render := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "render.png", Reference: "[image #2]", Data: []byte("render")})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "fix the layout in this screenshot"}, imageArtifactPart(shot)}},
		callTool("c1", "read_file", 0), resultOf("c1", "read_file", strings.Repeat("css ", 4_000)),
		messages.Compaction{Summary: "first summary"}.Message(),
		callTool("c2", "render", 0),
		{Role: messages.MessageRoleTool, ToolCallID: "c2", ToolName: "render", Content: "rendered", Parts: []messages.ContentPart{imageArtifactPart(render)}},
		messages.Compaction{Summary: "second summary"}.Message(),
	}
	_, stats, err := projectMessages(context.Background(), history, store, true)
	if err != nil || stats.HydratedImages != 2 {
		t.Fatalf("hydrated %d images (%v), want the request's and the render's", stats.HydratedImages, err)
	}
	finished := append(slices.Clone(history), reply("done", 0), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "thanks"})
	projected, stats, err := projectMessages(context.Background(), finished, store, true)
	if err != nil || stats.HydratedImages != 0 || strings.Contains(projectedText(projected), "request in progress") {
		t.Fatalf("after the turn: hydrated %d (%v), text %q", stats.HydratedImages, err, projectedText(projected))
	}
}

// A resumed run brings no new input: its history ends in the tool batch it
// yielded after, and a summary may cover the whole turn so the run goes on.
func TestAgentSummarizesAResumedTurn(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			return reply("the task so far", 0), nil
		}
		return reply("done", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "you are a worker"},
		{Role: messages.MessageRoleUser, Content: "the task"},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "c1", Name: "fetch", Arguments: `{}`}, {ID: "c2", Name: "fetch", Arguments: `{}`}}},
		resultOf("c1", "fetch", strings.Repeat("a", 22_000)), resultOf("c2", "fetch", strings.Repeat("b", 22_000)),
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 10_000}, &AgentCallbacks{
		BeforeFirstRequest: func(ProjectionStats) error { return nil },
	})
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("resumed run = %v, %+v", err, response)
	}
}

// Over budget, with no summary that fits, a clear that brings the request
// within budget is taken rather than refusing it.
func TestAgentTakesAClearThatFitsWhenNoSummaryCan(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			t.Error("a summary was requested though none could fit")
		}
		return reply("done", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("s", 32_000)},
		{Role: messages.MessageRoleUser, Content: "old"},
		callTool("c1", "fetch", 0), resultOf("c1", "fetch", strings.Repeat("r", 12_000)),
		reply("answered", 0),
		{Role: messages.MessageRoleUser, Content: "new " + strings.Repeat("p", 4_000)},
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 10_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := response.AllMessages[0].Compaction(); !ok || c.ClearThrough != "c1" {
		t.Fatalf("generated = %+v, want the read result cleared", response.AllMessages)
	}
}

// A summary refused for stopping at its output limit was billed: its usage is
// recorded and reported like any summary's.
func TestRefusedSummaryUsageIsRecorded(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			cut := reply("half a summary", 9_000)
			cut.StopReason = messages.StopReasonMaxTokens
			return cut, nil
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 7_300)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	var reported int
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, &AgentCallbacks{
		OnCompactionUsage: func(_ string, usage UsageUpdate) { reported += usage.InputTokens },
	})
	if err != nil || response.Message.Content != "answer" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	if usage := response.TokenUsage(); usage.TotalInput != 9_000 || usage.PeakInput != 0 || reported != 9_000 {
		t.Fatalf("refused summary usage = %+v, reported %d", usage, reported)
	}
}

// A compaction model that cannot summarize hands the summary to the
// request's own model, whose usage prices as the conversation's requests do.
func TestAgentSummarizesWithTheRequestModelWhenTheCompactionModelFails(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			if req.Model == "cheap/summarizer" {
				return messages.ChatMessage{}, errors.New("missing API key for provider 'cheap'")
			}
			return reply("SUMMARY", 800), nil
		}
		return reply("answer", 100), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{CompactionModel: "cheap/summarizer"})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 12_000)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	var reported []string
	var notes []string
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "main/model", Messages: history, MaxContextTokens: 3_000}, &AgentCallbacks{
		OnCompactionUsage: func(model string, usage UsageUpdate) {
			reported = append(reported, fmt.Sprintf("%q %d", model, usage.InputTokens))
		},
		OnAdaptation: func(note RequestAdaptation) { notes = append(notes, note.Message) },
	})
	if err != nil || response.Message.Content != "answer" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	if len(model.requests) != 3 || model.requests[0].Model != "cheap/summarizer" || model.requests[1].Model != "main/model" || !isSummaryRequest(model.requests[1]) {
		t.Fatalf("requests = %d", len(model.requests))
	}
	if !slices.ContainsFunc(notes, func(note string) bool { return strings.Contains(note, "summarizing with main/model") }) {
		t.Fatalf("notes = %q", notes)
	}
	if len(reported) != 1 || reported[0] != `"" 800` {
		t.Fatalf("compaction usage reported = %q", reported)
	}
	if usage := response.TokenUsage(); usage.TotalInput != 900 || usage.PeakInput != 100 || usage.Compaction != (CompactionUsage{}) {
		t.Fatalf("usage = %+v", usage)
	}
}

// A compaction that fails while the request fits is not paid for again on
// every later iteration of the run.
func TestAgentTriesAFailingSummaryOncePerRun(t *testing.T) {
	summaries := 0
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			summaries++
			return reply("", 0), nil
		}
		if call < 4 {
			return callTool(fmt.Sprintf("c%d", call), "fetch", 0), nil
		}
		return reply("done", 0), nil
	}}
	fetch := &tools.Func{Name: "fetch", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{fetch}), AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 6_400)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, nil); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 {
		t.Fatalf("summaries attempted = %d, want 1", summaries)
	}
}

// A summary that comes out longer than planned, leaving a request that fit
// over its budget, is withdrawn: the request goes uncompacted, and what the
// summary cost stays recorded.
func TestAgentWithdrawsASummaryTooLongForItsRoom(t *testing.T) {
	model := &compactionLLM{reply: func(req *CompletionRequest, call int) (messages.ChatMessage, error) {
		if isSummaryRequest(req) {
			return reply(strings.Repeat("long ", 2_400), 500), nil
		}
		return reply("answer", 0), nil
	}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 7_300)},
		reply("old answer", 0),
		{Role: messages.MessageRoleUser, Content: "new"},
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 2_000}, nil)
	if err != nil || response.Message.Content != "answer" {
		t.Fatalf("run = %v, %+v", err, response)
	}
	for _, msg := range response.AllMessages {
		if _, ok := msg.Compaction(); ok {
			t.Fatalf("the too-long summary was kept: %+v", response.AllMessages)
		}
	}
	if response.TokenUsage().TotalInput != 500 {
		t.Fatalf("the withdrawn summary's usage was lost: %+v", response.TokenUsage())
	}
}
