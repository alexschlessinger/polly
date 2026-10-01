package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// scriptLLM answers each request with what fn returns for it.
type scriptLLM struct {
	fn    func(req *CompletionRequest, call int) messages.ChatMessage
	calls int
	tools [][]string
}

func (l *scriptLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.GetName())
	}
	l.tools = append(l.tools, names)
	response := l.fn(req, l.calls)
	l.calls++
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

func answer(content string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: content}
}

func callTools(name string, n int, args string) messages.ChatMessage {
	response := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse}
	for i := range n {
		response.ToolCalls = append(response.ToolCalls, messages.ChatMessageToolCall{ID: fmt.Sprintf("%s%d", name, i), Name: name, Arguments: args})
	}
	return response
}

// outputTool is a tool whose output the test chooses and whose runs it counts.
type outputTool struct {
	tools.NativeTool
	name   string
	output tools.ToolOutput
	runs   atomic.Int32
}

func (t *outputTool) GetName() string { return t.name }
func (t *outputTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.name, "a test tool", schema.Params{})
}
func (t *outputTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	output, err := t.ExecuteOutput(ctx, args)
	return output.Text, err
}
func (t *outputTool) ExecuteOutput(context.Context, map[string]any) (tools.ToolOutput, error) {
	t.runs.Add(1)
	return t.output, nil
}

// recallOutputTool is an outputTool that declares itself a recall tool but
// does not page its output.
type recallOutputTool struct{ outputTool }

func (t *recallOutputTool) RecallStub() string { return "[recall elided]" }

func registryWith(list ...tools.Tool) *tools.ToolRegistry {
	registry := tools.NewToolRegistry([]tools.Tool{}, tools.WithUnsafeNoSandbox())
	for _, tool := range list {
		registry.Register(tool)
	}
	return registry
}

// refusedResults counts the results the context budget refused.
func refusedResults(msgs []messages.ChatMessage) int {
	refused := 0
	for _, msg := range msgs {
		if msg.Role == messages.MessageRoleTool && strings.HasPrefix(msg.Content, "Not run:") {
			refused++
		}
	}
	return refused
}

func userAsks(text string) []messages.ChatMessage {
	return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: text}}
}

// Finishing rewrites tool calls as text, so reasoning a provider replays with
// those calls goes with them; plain reasoning, which DeepSeek and Qwen require
// on prior assistant turns, stays.
func TestFinalFormDropsReplayOfRewrittenCallsButKeepsReasoning(t *testing.T) {
	replay := map[string]any{
		anthropic.ThinkingBlocksKey:        []any{map[string]any{"thinking": "t", "signature": "sig"}},
		openai.ResponsesReasoningItemsKey:  []any{"encrypted"},
		openai.ResponsesReasoningModelKey:  "gpt",
		messages.MetadataKeyAgentSynthetic: false,
	}
	withCalls := messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: "why", Metadata: replay,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "fetch", Arguments: `{}`}}}
	final := finalForm(withCalls, nil)
	if len(final.ToolCalls) != 0 || final.Reasoning != "why" {
		t.Fatalf("final form = %+v", final)
	}
	for _, key := range reasoningReplayKeys {
		if _, ok := final.Metadata[key]; ok {
			t.Errorf("final form kept %s", key)
		}
	}
	if _, ok := withCalls.Metadata[anthropic.ThinkingBlocksKey]; !ok {
		t.Fatal("final form changed the durable message")
	}
	textOnly := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", Metadata: replay}
	if _, ok := finalForm(textOnly, nil).Metadata[anthropic.ThinkingBlocksKey]; !ok {
		t.Fatal("final form dropped the reasoning of a reply without calls")
	}
}

// Replayed reasoning costs what is sent: the larger of the plain reasoning and
// the provider's replay state.
func TestReplayedReasoningIsCounted(t *testing.T) {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", Reasoning: "short"}
	plain := estimateProjectedMessageTokens(msg)
	msg.Metadata = map[string]any{anthropic.ThinkingBlocksKey: []any{map[string]any{"thinking": strings.Repeat("deliberation ", 400), "signature": strings.Repeat("s", 2000)}}}
	if replayed := estimateProjectedMessageTokens(msg); replayed <= plain+1000 {
		t.Fatalf("replayed thinking counted %d over %d", replayed, plain)
	}
}

// A run that must end in its response tool finishes in it: the finishing
// request offers that tool alone, and the planner never refuses it.
func TestResponseToolRunFinishesInTheResponseTool(t *testing.T) {
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: strings.Repeat("page ", 3000)}}
	respond := &outputTool{name: "respond", output: tools.ToolOutput{Text: "recorded"}}
	llm := &scriptLLM{fn: func(req *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			// Far more calls than the budget can take even refused.
			return callTools("fetch", 150, `{}`)
		}
		return callTools("respond", 1, `{}`)
	}}
	agent := NewAgent(llm, registryWith(fetch, respond), AgentConfig{ArtifactStore: newTestArtifactStore(), ResponseTool: "respond", RequireResponseToolSuccess: true})
	defer agent.Close()
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: 3_000}, nil); err != nil {
		t.Fatal(err)
	}
	if fetch.runs.Load() != 0 || respond.runs.Load() != 1 {
		t.Fatalf("fetch ran %d, respond ran %d", fetch.runs.Load(), respond.runs.Load())
	}
	if last := llm.tools[len(llm.tools)-1]; len(last) != 1 || last[0] != "respond" {
		t.Fatalf("finishing request offered %v", last)
	}
}

func TestResponseToolCallIsNeverRefused(t *testing.T) {
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: "fetched"}}
	respond := &outputTool{name: "respond", output: tools.ToolOutput{Text: "recorded"}}
	for budget := 6_000; budget >= 2_000; budget -= 250 {
		fetch.runs.Store(0)
		respond.runs.Store(0)
		llm := &scriptLLM{fn: func(req *CompletionRequest, call int) messages.ChatMessage {
			batch := callTools("fetch", 40, `{}`)
			batch.ToolCalls = append(batch.ToolCalls, messages.ChatMessageToolCall{ID: "r", Name: "respond", Arguments: `{}`})
			return batch
		}}
		agent := NewAgent(llm, registryWith(fetch, respond), AgentConfig{ArtifactStore: newTestArtifactStore(), ResponseTool: "respond", RequireResponseToolSuccess: true, MaxIterations: 3})
		_, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, nil)
		agent.Close()
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if respond.runs.Load() != 1 {
			t.Fatalf("budget %d: respond ran %d times", budget, respond.runs.Load())
		}
	}
}

// Input admitted between model calls is stored behind a receipt when it
// would not fit whole, and stays readable.
func TestAdmittedInputIsStoredWhenItWouldNotFit(t *testing.T) {
	admitted := false
	cb := &AgentCallbacks{AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
		if admitted {
			return nil, nil
		}
		admitted = true
		return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "PEER " + strings.Repeat("mail ", 20_000) + " END"}}, nil
	}}
	agent := NewAgent(&scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer("done") }}, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: 4_000}, cb)
	if err != nil {
		t.Fatal(err)
	}
	var input messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleUser {
			input = msg
		}
	}
	id := regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(input.Content)
	if !strings.Contains(input.Content, "\nPEER") || !strings.Contains(input.Content, "END") || id == "" {
		t.Fatalf("admitted input = %.200q", input.Content)
	}
	if _, ok := agent.lookupArtifact(id); !ok {
		t.Fatal("the stored input is not readable")
	}
}

func TestAdmittedImagesRespectCapabilities(t *testing.T) {
	for _, budget := range []int{0, 4_000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			model := &scriptLLM{fn: func(req *CompletionRequest, _ int) messages.ChatMessage {
				omitted := false
				for _, msg := range req.Messages {
					for _, part := range msg.Parts {
						if isImagePart(part) {
							t.Fatal("text-only model received an admitted image")
						}
						omitted = omitted || strings.Contains(part.Text, "this model cannot view images")
					}
					omitted = omitted || strings.Contains(msg.GetContent(), "this model cannot view images")
				}
				if !omitted {
					t.Fatal("image omission descriptor is missing")
				}
				return answer("done")
			}}
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			input := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "look", Parts: []messages.ContentPart{{
				Type: "image_base64", MimeType: "image/png", ImageData: base64.StdEncoding.EncodeToString(independentPNG(t)),
			}}}
			response, err := agent.Run(context.Background(), &CompletionRequest{
				Messages: userAsks("go"), MaxContextTokens: budget, Capabilities: &ModelCapabilities{InputModalities: []string{"text"}},
			}, &AgentCallbacks{AdmitInput: func(context.Context) ([]messages.ChatMessage, error) { return []messages.ChatMessage{input}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			if len(response.AllMessages[0].Parts) != 1 || !isImagePart(response.AllMessages[0].Parts[0]) {
				t.Fatal("durable admitted image was changed")
			}
		})
	}
}

// Continuation input is bounded like admitted input. When a long answer
// leaves no room even for a receipt, the run ends with the answer intact.
func TestContinuationInputIsBoundedOrEndsTheRun(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		continued := false
		cb := &AgentCallbacks{ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			if continued {
				return nil, nil
			}
			continued = true
			return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: strings.Repeat("more work ", 20_000), Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}, nil
		}}
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			return answer(fmt.Sprintf("answer %d", call))
		}}
		agent := NewAgent(llm, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
		defer agent.Close()
		response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: 4_000}, cb)
		if err != nil || llm.calls != 2 || response.Message.Content != "answer 1" {
			t.Fatalf("calls=%d err=%v message=%+v", llm.calls, err, response.Message)
		}
	})
	t.Run("no room", func(t *testing.T) {
		cb := &AgentCallbacks{ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			// Synthetic, so it continues the exchange the long answer is in;
			// a real user message would start a new one and let it go.
			return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "and then?", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}, nil
		}}
		long := strings.Repeat("a very long answer ", 1_000)
		agent := NewAgent(&scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer(long) }}, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
		defer agent.Close()
		response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: 4_000}, cb)
		if !errors.Is(err, ErrContextExhausted) || response.Message == nil || response.Message.Content != long {
			t.Fatalf("err=%v message=%.40q", err, response.Message.Content)
		}
	})
}

// A result that does not fit once it has run, like an image a store-less
// tool returned, is replaced by a note rather than overflowing the next
// request.
func TestResultsThatDoNotFitAreDropped(t *testing.T) {
	shot := &outputTool{name: "shot", output: tools.ToolOutput{Text: "took it", Media: []tools.ToolMedia{{Data: independentPNG(t), MIMEType: "image/png", Name: "shot.png"}}}}
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("shot", 1, `{}`)
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(shot), AgentConfig{})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("look"), MaxContextTokens: 1_500}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.Content != droppedText("shot") {
			t.Fatalf("result that could not fit = %+v", msg)
		}
	}
}

// Calls the budget refused are neither started nor put to approval.
func TestRefusedCallsAreNeitherStartedNorApproved(t *testing.T) {
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: strings.Repeat("fetched ", 2000)}}
	sawRefusal := false
	for budget := 6_000; budget >= 1_500 && !sawRefusal; budget -= 25 {
		var asked, started int
		cb := &AgentCallbacks{
			ApproveToolCalls: func(_ context.Context, calls []messages.ChatMessageToolCall) ([]bool, error) {
				asked += len(calls)
				approved := make([]bool, len(calls))
				for i := range approved {
					approved[i] = true
				}
				return approved, nil
			},
			OnToolStart: func(calls []messages.ChatMessageToolCall) { started += len(calls) },
		}
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			if call == 0 {
				return callTools("fetch", 30, `{}`)
			}
			return answer("done")
		}}
		agent := NewAgent(llm, registryWith(fetch), AgentConfig{ArtifactStore: newTestArtifactStore()})
		response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, cb)
		agent.Close()
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		refused := refusedResults(response.AllMessages)
		if refused > 0 && refused < 30 {
			sawRefusal = true
			if asked != 30-refused || started != 30-refused {
				t.Fatalf("budget %d: %d refused, but %d put to approval and %d started", budget, refused, asked, started)
			}
		}
	}
	if !sawRefusal {
		t.Fatal("no budget refused part of the batch")
	}
}

// A reply cut off at its output limit is a final answer, not a batch to plan.
func TestTruncatedReplyWithCallsIsNotDropped(t *testing.T) {
	llm := &scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage {
		reply := callTools("fetch", 150, `{}`)
		reply.StopReason = messages.StopReasonMaxTokens
		reply.Content = "cut off"
		return reply
	}}
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: "fetched"}}
	agent := NewAgent(llm, registryWith(fetch), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: 3_000}, nil)
	if err != nil || llm.calls != 1 || response.Message == nil || response.Message.Content != "cut off" {
		t.Fatalf("calls=%d err=%v message=%+v", llm.calls, err, response.Message)
	}
}

// Without a store nothing shrinks to a receipt, so ordinary results are paged.
func TestStorelessResultsArePaged(t *testing.T) {
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: strings.Repeat("fetched content ", 6000)}}
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("fetch", 1, `{}`)
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(fetch), AgentConfig{})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("fetch it"), MaxContextTokens: 6_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && (!strings.Contains(msg.Content, "truncated to fit the context budget") || len(msg.Content) > 6_000*4) {
			t.Fatalf("store-less result is %d bytes: %.80q", len(msg.Content), msg.Content)
		}
	}
}

// A recall tool that does not page its output is held to its page anyway.
func TestCustomRecallToolIsBoundedToItsPage(t *testing.T) {
	recall := &recallOutputTool{outputTool{name: "recall", output: tools.ToolOutput{Text: strings.Repeat("remembered ", 10_000)}}}
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("recall", 1, `{}`)
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(recall), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("recall"), MaxContextTokens: 5_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && len(msg.Content) > 5_000*4 {
			t.Fatalf("recall result is %d bytes", len(msg.Content))
		}
	}
}

// The marker's artifact clause names read_artifact, so it needs it.
func TestMarkerNamesArtifactReadersOnlyWhenBothInstalled(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current"},
	}
	for name, list := range map[string][]tools.Tool{
		"list only": {&listArtifactsTool{}},
		"both":      {&listArtifactsTool{}, &readArtifactTool{}},
	} {
		projected, stats, err := projectMessagesCached(context.Background(), cloneMessages(history), 250, newTestArtifactStore(), projectionToolsFor(list), nil)
		if err != nil || stats.OmittedExchanges == 0 {
			t.Fatalf("%s: stats=%+v err=%v", name, stats, err)
		}
		if got := strings.Contains(projectedText(projected), "read_artifact"); got != (name == "both") {
			t.Fatalf("%s: marker names read_artifact = %v", name, got)
		}
	}
}

// The reply the agent drops is reported, since the host already showed it.
func TestDroppedReplyIsReported(t *testing.T) {
	var dropped []*messages.ChatMessage
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			reply := callTools("fetch", 150, `{}`)
			reply.Content = "let me look"
			return reply
		}
		return answer("done")
	}}
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: "fetched"}}
	agent := NewAgent(llm, registryWith(fetch), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: 3_000}, &AgentCallbacks{OnResponseDropped: func(m *messages.ChatMessage) { dropped = append(dropped, m) }})
	if err != nil || len(dropped) != 1 || dropped[0].Content != "let me look" {
		t.Fatalf("err=%v dropped=%+v", err, dropped)
	}
}

// An image the active exchange cannot hold is sent as its descriptor rather
// than failing the request.
func TestImagesAreDroppedRatherThanOverflow(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "shot.png", Data: independentPNG(t)})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "look"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "shot", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "1", ToolName: "shot", Content: artifactMediaDescriptor(ref), Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref, MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken}}},
	}
	_, stats, err := projectCompletionRequest(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 1_000}, store, projectionTools{}, nil)
	if err != nil || stats.HydratedImages != 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if _, stats, err = projectCompletionRequest(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 10_000}, store, projectionTools{}, nil); err != nil || stats.HydratedImages != 1 {
		t.Fatalf("with room: stats=%+v err=%v", stats, err)
	}
}

// A batch that adds tools and ends in the response tool leaves continuation
// input to be measured against the tools as they now are.
func TestInputAfterToolsGrowMeetsTheNewTools(t *testing.T) {
	for budget := 2_000; budget <= 9_000; budget += 100 {
		registry := registryWith(&sizedTool{name: "respond"})
		registry.Register(&growTool{registry: registry})
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			if call == 0 {
				batch := callTools("grow", 1, `{"size":4242}`)
				batch.ToolCalls = append(batch.ToolCalls, messages.ChatMessageToolCall{ID: "r", Name: "respond", Arguments: `{}`})
				return batch
			}
			return answer("done")
		}}
		continued := false
		cb := &AgentCallbacks{ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
			if continued {
				return nil, nil
			}
			continued = true
			return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: strings.Repeat("more ", 2_000), Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}, nil
		}}
		agent := NewAgent(llm, registry, AgentConfig{ArtifactStore: newTestArtifactStore(), ResponseTool: "respond"})
		_, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: budget}, cb)
		agent.Close()
		var limit *ContextLimitError
		if llm.calls > 0 && (errors.As(err, &limit) || (err != nil && strings.Contains(err.Error(), "tool schemas alone"))) {
			t.Fatalf("budget %d: %v", budget, err)
		}
	}
}
