package llm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// addingTool registers a tool whose schema is far larger than any budget the
// tests use, so the tools of the run outgrow the room once it has run.
type addingTool struct {
	tools.NativeTool
	registry *tools.ToolRegistry
}

func (t *addingTool) GetName() string { return "add" }
func (t *addingTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("add", "adds a large tool", schema.Params{})
}
func (t *addingTool) Execute(context.Context, map[string]any) (string, error) {
	t.registry.Register(&describedTool{sizedTool: sizedTool{name: "added"}, description: strings.Repeat("word ", 4_000)})
	return "added", nil
}

// A reply to a finishing request keeps only its calls to the response tool,
// which end the run. It is never dropped for want of room to offer every
// tool again: that would only ask the same finishing request, until the
// iteration limit.
func TestFinishingReplyInTheResponseToolIsNeverDropped(t *testing.T) {
	registry := registryWith(&outputTool{name: "respond", output: tools.ToolOutput{Text: "recorded"}})
	registry.Register(&addingTool{registry: registry})
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("add", 1, `{}`)
		}
		return callTools("respond", 1, `{}`)
	}}
	agent := NewAgent(llm, registry, AgentConfig{ArtifactStore: newTestArtifactStore(), ResponseTool: "respond", RequireResponseToolSuccess: true, MaxIterations: 6})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: 3_000}, nil)
	if err != nil || llm.calls != 2 {
		t.Fatalf("calls=%d err=%v", llm.calls, err)
	}
	if last := llm.tools[1]; len(last) != 1 || last[0] != "respond" {
		t.Fatalf("finishing request offered %v", last)
	}
}

// A reply that ends the turn yet carries tool calls is a tool turn, and its
// batch is planned like any other, whichever LLM implementation produced it.
func TestEndTurnReplyWithToolCallsIsPlanned(t *testing.T) {
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: strings.Repeat("page ", 3000)}}
	dropped := 0
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			reply := callTools("fetch", 150, `{}`)
			reply.StopReason = messages.StopReasonEndTurn
			return reply
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(fetch), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: 3_000}, &AgentCallbacks{OnResponseDropped: func(*messages.ChatMessage) { dropped++ }})
	if err != nil || fetch.runs.Load() != 0 || dropped != 1 {
		t.Fatalf("err=%v fetch ran %d, dropped %d", err, fetch.runs.Load(), dropped)
	}
}

// rejectingLLM rejects the first rejections requests as too long, then
// answers like the scriptLLM it wraps.
type rejectingLLM struct {
	scriptLLM
	rejections int
	rejected   int
}

func (l *rejectingLLM) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	if l.rejected < l.rejections {
		l.rejected++
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: &ContextOverflowError{Err: errors.New("too long")}}
		close(events)
		return events
	}
	return l.scriptLLM.ChatCompletionStream(ctx, req, processor)
}

// A rejected request's projection can store a tool result as an artifact.
// The request sent in its place carries that ref, the finishing request
// included, so the transcript keeps it once persisted. The rejected request
// spilled its one exchange's result, so no retry can be smaller, and the
// finishing request is what answers the rejection.
func TestRejectedRequestsArtifactRefsReachTheFinishingReply(t *testing.T) {
	result := strings.Repeat("fetched ", 4_000)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "f", Name: "fetch", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "f", ToolName: "fetch", Content: result},
	}
	llm := &rejectingLLM{scriptLLM: scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer("done") }}, rejections: 1}
	agent := NewAgent(llm, registryWith(&outputTool{name: "fetch"}), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 3_000}, nil)
	if err != nil || response.Message == nil || llm.rejected != 1 {
		t.Fatalf("err=%v rejected=%d", err, llm.rejected)
	}
	if len(llm.tools) != 1 || len(llm.tools[0]) != 0 {
		t.Fatalf("the rejection was not answered by the finishing request: tools offered %v", llm.tools)
	}
	var stored *artifacts.Ref
	for _, part := range response.Message.Parts {
		if part.Artifact != nil && part.Artifact.Kind == artifacts.KindText {
			stored = part.Artifact
		}
	}
	if stored == nil || stored.Bytes != int64(len(result)) {
		t.Fatalf("the finishing reply carries no ref for the stored result: %+v", response.Message.Parts)
	}
	if _, ok := agent.lookupArtifact(stored.ID); !ok {
		t.Fatal("the stored result is not readable")
	}
}

// Input that does not fit whole is stored behind a receipt only when the run
// offers read_artifact; without it, a receipt would be of no use, so the
// input stays staged instead.
func TestInputIsNotStoredWithoutTheReader(t *testing.T) {
	admitted := false
	cb := &AgentCallbacks{AdmitInput: func(context.Context) ([]messages.ChatMessage, error) {
		if admitted {
			return nil, nil
		}
		admitted = true
		return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "PEER " + strings.Repeat("mail ", 20_000) + " END"}}, nil
	}}
	llm := &scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer("done") }}
	agent := NewAgent(llm, nil, AgentConfig{ArtifactStore: newTestArtifactStore(), Builtins: []string{}})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: 4_000}, cb)
	if err != nil || !admitted {
		t.Fatalf("err=%v admitted=%v", err, admitted)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleUser {
			t.Fatalf("input was committed without a reader: %.120q", msg.Content)
		}
	}
}

// exclusiveTool must be the only call of its batch that runs.
type exclusiveTool struct{ outputTool }

func (t *exclusiveTool) ExclusiveBatch() bool { return true }

// Calls the budget refused leave the batch before it is checked, so an
// exclusive tool that is the only call left to run is not aborted for the
// siblings that never run.
func TestExclusiveToolRunsWhenItsSiblingsAreRefused(t *testing.T) {
	spawn := &exclusiveTool{outputTool{name: "spawn", output: tools.ToolOutput{Text: "spawned"}}}
	fetch := &recallOutputTool{outputTool{name: "fetch", output: tools.ToolOutput{Text: strings.Repeat("fetched ", 2000)}}}
	for budget := 6_000; budget >= 1_000; budget -= 25 {
		spawn.runs.Store(0)
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			if call == 0 {
				batch := callTools("spawn", 1, `{}`)
				batch.ToolCalls = append(batch.ToolCalls, callTools("fetch", 30, `{}`).ToolCalls...)
				return batch
			}
			return answer("done")
		}}
		agent := NewAgent(llm, registryWith(spawn, fetch), AgentConfig{ArtifactStore: newTestArtifactStore()})
		response, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, nil)
		agent.Close()
		refused := refusedResults(response.AllMessages)
		if refused < 30 {
			continue
		}
		if err != nil || spawn.runs.Load() != 1 {
			t.Fatalf("budget %d: every sibling refused, but spawn ran %d times: %v", budget, spawn.runs.Load(), err)
		}
		return
	}
	t.Fatal("no budget refused every sibling of the exclusive call")
}

// A refused call is reported ended once the batch it was part of has been
// reported started, so a host places it with the batch.
func TestRefusedCallsEndAfterTheBatchStarts(t *testing.T) {
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: strings.Repeat("fetched ", 2000)}}
	for budget := 6_000; budget >= 1_500; budget -= 25 {
		var events []string
		cb := &AgentCallbacks{
			OnToolStart: func([]messages.ChatMessageToolCall) { events = append(events, "start") },
			OnToolEnd: func(_ messages.ChatMessageToolCall, _ string, _ time.Duration, err error) {
				if errors.Is(err, ErrToolCallRefused) {
					events = append(events, "refused")
				}
			},
		}
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			if call == 0 {
				return callTools("fetch", 30, `{}`)
			}
			return answer("done")
		}}
		agent := NewAgent(llm, registryWith(fetch), AgentConfig{ArtifactStore: newTestArtifactStore()})
		_, err := agent.Run(context.Background(), &CompletionRequest{Messages: longOmittedHistory(), MaxContextTokens: budget}, cb)
		agent.Close()
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if !slices.Contains(events, "refused") || !slices.Contains(events, "start") {
			continue
		}
		if events[0] != "start" {
			t.Fatalf("budget %d: refusals were reported before the batch started: %v", budget, events)
		}
		return
	}
	t.Fatal("no budget refused part of the batch")
}

// The projection spills an active result whenever its receipt is smaller,
// however small the result, so fitting a batch keeps results the next request
// can carry as receipts instead of replacing them with notes.
func TestResultsTheProjectionCanSpillAreKept(t *testing.T) {
	small := &outputTool{name: "small", output: tools.ToolOutput{Text: strings.Repeat("line of command output\n", 70)}}
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("small", 8, `{}`)
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(small), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("run them"), MaxContextTokens: 2_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runs := small.runs.Load(); runs != 8 {
		t.Fatalf("ran %d of 8", runs)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.Content == droppedText("small") {
			t.Fatalf("a result the next request could carry as a receipt was dropped: %+v", msg)
		}
	}
}

// Fitting replaces the result whose note frees the room, not the largest: an
// ordinary result already costs only its receipt, while an unread recall page
// is carried whole.
func TestFittingDropsTheResultThatHoldsTheRoom(t *testing.T) {
	big := strings.Repeat("line of important output\n", 320)
	page := strings.Repeat("transcript page line\n", 280)
	checked := 0
	for budget := 1_800; budget >= 1_300; budget -= 25 {
		b := batchBudget{
			req:          &CompletionRequest{MaxContextTokens: budget},
			agentTools:   projectionTools{recall: recallStubs{"recall": "[recall elided]"}},
			hasStore:     true,
			inlineTokens: 10_000,
		}
		history := userAsks("go")
		response := callTools("big", 3, `{}`)
		response.ToolCalls = append(response.ToolCalls, messages.ChatMessageToolCall{ID: "r0", Name: "recall", Arguments: `{}`})
		history = append(history, response)
		var results []messages.ChatMessage
		for _, call := range response.ToolCalls {
			content := big
			if call.Name == "recall" {
				content = page
			}
			results = append(results, callResult(call, content))
		}
		withoutPage := slices.Clone(results)
		withoutPage[3] = callResult(response.ToolCalls[3], droppedText("recall"))
		if b.fits(history, results...) || !b.fits(history, withoutPage...) {
			continue
		}
		fitted, ok := fitResults(b, history, slices.Clone(results))
		if !ok {
			t.Fatalf("budget %d: the results do not fit though dropping the page does", budget)
		}
		for i, result := range fitted[:3] {
			if result.Content != big {
				t.Fatalf("budget %d: result %d, which costs only its receipt, was replaced: %q", budget, i, result.Content)
			}
		}
		if fitted[3].Content == page {
			t.Fatalf("budget %d: the unread page was kept", budget)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no budget needed only the page dropped")
	}
}

// A batch in which no call would run is dropped, not committed as refusals.
// Every budget is tried: the gap between refusing all but one call and
// dropping the batch is a few tokens wide, and where it falls in a run
// depends on the sandbox context, which names the working directory.
func TestBatchWithNothingToRunIsDropped(t *testing.T) {
	agent := NewAgent(&loopLLM{}, registryWith(fetchTool(nil)), AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	offered := agent.tools.All()
	response := callTools("fetch", 30, `{"q":"xxxxxxxxxx"}`)
	sawRefused := false
	for budget := 6_000; budget >= 1_500; budget-- {
		b := batchBudget{
			req:          &CompletionRequest{MaxContextTokens: budget},
			agentTools:   projectionToolsFor(offered),
			nextSchemas:  estimateToolSchemaTokens(offered),
			hasStore:     true,
			inlineTokens: 10_000,
		}
		plan := planBatch(b, longOmittedHistory(), &response)
		if plan.finish {
			continue
		}
		if len(plan.refused) == len(response.ToolCalls) {
			t.Fatalf("budget %d: every call refused, yet the batch is committed", budget)
		}
		sawRefused = sawRefused || len(plan.refused) > 0
	}
	if !sawRefused {
		t.Fatal("no budget refused part of the batch")
	}
}

// pageTool is a recall tool whose page is exactly the bound in force, as a
// pager's is on a large file. hook runs first.
type pageTool struct {
	tools.NativeTool
	hook func()
}

func (t *pageTool) GetName() string { return "read_page" }
func (t *pageTool) GetSchema() *schema.ToolSchema {
	return schema.Tool("read_page", "reads a page", schema.Params{})
}
func (t *pageTool) RecallStub() string { return "[page elided]" }
func (t *pageTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	if t.hook != nil {
		t.hook()
	}
	return strings.Repeat("word ", tools.PageBytes(ctx)/5), nil
}

// Another agent sharing the calibration can raise the model's ratio after a
// batch was planned, shrinking the budget below the state the run committed.
// The run then finishes from what it has instead of failing.
func TestBudgetShrunkBetweenRequestsFinishesTheRun(t *testing.T) {
	shared := NewCalibration()
	reader := &pageTool{hook: func() { shared.learnUsage("test/same", 2, 1) }}
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("read_page", 1, `{}`)
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(reader), AgentConfig{Calibration: shared, ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "You are a test."},
		{Role: messages.MessageRoleUser, Content: "Do the thing. " + strings.Repeat("context ", 800)},
	}
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "test/same", Messages: history, MaxContextTokens: 6_000}, nil)
	if err != nil {
		t.Fatalf("run failed for want of room: %v", err)
	}
	if response.Message == nil || response.Message.Content != "done" {
		t.Fatalf("final = %+v", response.Message)
	}
	if last := llm.tools[len(llm.tools)-1]; len(last) != 0 {
		t.Fatalf("the last request offered %v, want the finishing request", last)
	}
}

// Calls the budget refused were never put to approval, so a batch whose
// approved-for calls the user all denied still ends the run.
func TestDeniedBatchWithRefusalsEndsTheRun(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "question " + strings.Repeat("w", 400)},
	}
	for budget := 600; budget <= 12_000; budget += 25 {
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			if call == 0 {
				return callTools(BuiltinReadTranscript, 3, `{}`)
			}
			return answer("done")
		}}
		asked, refused := 0, 0
		cb := &AgentCallbacks{
			ApproveToolCalls: func(_ context.Context, calls []messages.ChatMessageToolCall) ([]bool, error) {
				asked += len(calls)
				return make([]bool, len(calls)), nil
			},
			OnToolEnd: func(_ messages.ChatMessageToolCall, _ string, _ time.Duration, err error) {
				if errors.Is(err, ErrToolCallRefused) {
					refused++
				}
			},
		}
		agent := NewAgent(llm, registryWith(), AgentConfig{})
		_, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: budget}, cb)
		agent.Close()
		if err != nil || refused == 0 || asked == 0 {
			continue
		}
		if llm.calls != 1 {
			t.Fatalf("budget %d: %d refused, %d denied, yet the model was called %d times", budget, refused, asked, llm.calls)
		}
		return
	}
	t.Fatal("no budget refused part of the batch")
}

// An agent run started by a tool of another agent's batch pages to its own
// budget: neither the batch's page bound nor its plan carries into it.
func TestNestedRunIsNotBoundByTheCallingBatch(t *testing.T) {
	var seen int
	var childResult string
	child := func(ctx context.Context, budget int) {
		big := &outputTool{name: "big", output: tools.ToolOutput{Text: strings.Repeat("x", 30_000)}}
		llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			switch call {
			case 0:
				return callTools("read_page", 1, `{}`)
			case 1:
				return callTools("big", 1, `{}`)
			}
			return answer("child done")
		}}
		agent := NewAgent(llm, registryWith(&pageSizeTool{pageTool: &pageTool{}, seen: &seen}, big), AgentConfig{})
		defer agent.Close()
		response, err := agent.Run(ctx, &CompletionRequest{Messages: userAsks("read"), MaxContextTokens: budget}, nil)
		if err != nil {
			t.Errorf("child: %v", err)
			return
		}
		for _, msg := range response.AllMessages {
			if msg.ToolName == "big" {
				childResult = msg.Content
			}
		}
	}
	for _, childBudget := range []int{200_000, 0} {
		spawn := &outputTool{name: "spawn"}
		parent := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
			if call == 0 {
				return callTools("spawn", 1, `{}`)
			}
			return answer("parent done")
		}}
		agent := NewAgent(parent, registryWith(&hookTool{outputTool: spawn, hook: func(ctx context.Context) { child(ctx, childBudget) }}), AgentConfig{})
		seen, childResult = 0, ""
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "Do the thing. " + strings.Repeat("context ", 800)}}
		if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 3_000}, nil); err != nil {
			t.Fatal(err)
		}
		agent.Close()
		if childBudget > 0 && seen != tools.PageMaxBytes {
			t.Fatalf("child with its own %d budget paged at %d bytes, want %d", childBudget, seen, tools.PageMaxBytes)
		}
		if childBudget == 0 && len(childResult) != 30_000 {
			t.Fatalf("unbudgeted child's result was cut to %d bytes", len(childResult))
		}
	}
}

// pageSizeTool records the page bound its reads run under.
type pageSizeTool struct {
	*pageTool
	seen *int
}

func (t *pageSizeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	*t.seen = tools.PageBytes(ctx)
	return t.pageTool.Execute(ctx, args)
}

// hookTool runs hook under the tool's context before answering.
type hookTool struct {
	*outputTool
	hook func(context.Context)
}

func (t *hookTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	t.hook(ctx)
	return "ok", nil
}
func (t *hookTool) ExecuteOutput(ctx context.Context, args map[string]any) (tools.ToolOutput, error) {
	t.hook(ctx)
	return tools.ToolOutput{Text: "ok"}, nil
}

// A tool result written as text is labeled before its content, so reading
// down, each result follows the label of its own call.
func TestFlattenedResultFollowsItsLabel(t *testing.T) {
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "c1", ToolName: "read_file", Content: "AAA"}
	final := finalForm(result, nil)
	if len(final.Parts) != 2 || final.Parts[0].Text != "Result of tool call c1 (read_file):" || final.Parts[1].Text != "AAA" {
		t.Fatalf("flattened result parts = %+v", final.Parts)
	}
}

// inputFailingStore fails to store input, as a store that lost its lease or
// its disk does.
type inputFailingStore struct{ *testArtifactStore }

var errInputStore = errors.New("store unavailable")

func (s inputFailingStore) Put(ctx context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	if blob.Name == "input" {
		return artifacts.Ref{}, errInputStore
	}
	return s.testArtifactStore.Put(ctx, blob)
}

// A store that fails while input is bounded is the run's error, not a lack of
// room: the input is neither silently dropped nor reported as exhausting the
// budget.
func TestInputStoreFailureIsTheRunsError(t *testing.T) {
	llm := &scriptLLM{fn: func(*CompletionRequest, int) messages.ChatMessage { return answer("final answer") }}
	store := inputFailingStore{newTestArtifactStore()}
	agent := NewAgent(llm, registryWith(), AgentConfig{ArtifactStore: store, MaxIterations: 5})
	defer agent.Close()
	continued := false
	cb := &AgentCallbacks{ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
		if continued {
			return nil, nil
		}
		continued = true
		return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: strings.Repeat("delivered member result ", 1_500), Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}, nil
	}}
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("hi"), MaxContextTokens: 3_000}, cb)
	if !errors.Is(err, errInputStore) || errors.Is(err, ErrContextExhausted) {
		t.Fatalf("err = %v, want the store's failure", err)
	}
}

// An image the user attached that the budget turns away is sent as its
// descriptor, so the model still learns an image was there and where it is.
func TestUserImageTheBudgetOmitsStandsAsItsDescriptor(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "screenshot.png", Data: independentPNG(t)})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "What is wrong in this screenshot? " + strings.Repeat("context ", 200),
			Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref, MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken}}},
	}
	projected, stats, err := projectCompletionRequest(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 1_500}, store, projectionTools{}, nil)
	if err != nil || stats.HydratedImages != 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if text := messageText(projected[len(projected)-1]); !strings.Contains(text, "image omitted to fit the context budget") || !strings.Contains(text, ref.ID) {
		t.Fatalf("the omitted image left no descriptor: %q", text)
	}
}

// The request that finishes a run after a dropped batch carries the results
// the run gathered when they fit, so the model answers from them.
func TestFinishingRequestKeepsResultsThatFit(t *testing.T) {
	lookup := &outputTool{name: "lookup", output: tools.ToolOutput{Text: "The secret code is ZEBRA-7731. " + strings.Repeat("filler text ", 100)}}
	fetch := &outputTool{name: "fetch", output: tools.ToolOutput{Text: "x"}}
	var finishing *CompletionRequest
	llm := &scriptLLM{fn: func(req *CompletionRequest, call int) messages.ChatMessage {
		switch call {
		case 0:
			return callTools("lookup", 1, `{}`)
		case 1:
			return callTools("fetch", 300, `{}`)
		}
		if finishing == nil {
			finishing = req
		}
		return answer("done")
	}}
	agent := NewAgent(llm, registryWith(lookup, fetch), AgentConfig{ArtifactStore: newTestArtifactStore(), MaxIterations: 6})
	defer agent.Close()
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("what is the code?"), MaxContextTokens: 8_000}, nil); err != nil {
		t.Fatal(err)
	}
	if finishing == nil || len(finishing.Tools) != 0 {
		t.Fatal("the dropped batch was not followed by a finishing request")
	}
	for _, msg := range finishing.Messages {
		if strings.Contains(messageText(msg), "ZEBRA-7731") {
			return
		}
	}
	t.Fatal("the finishing request does not carry the lookup result the run gathered")
}

// When a batch adds tools the room cannot hold, the next request finishes
// without them, and the batch's results need only fit that request: the ones
// that do are kept, and a denied call's result is never rewritten as having
// run.
func TestResultsOutlastToolsThatOutgrowTheRoom(t *testing.T) {
	registry := registryWith()
	registry.Register(&addingTool{registry: registry})
	lookup := &outputTool{name: "lookup", output: tools.ToolOutput{Text: "The secret code is ZEBRA-7731. " + strings.Repeat("filler text ", 100)}}
	danger := &outputTool{name: "danger"}
	registry.Register(lookup)
	registry.Register(danger)
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			batch := callTools("add", 1, `{}`)
			batch.ToolCalls = append(batch.ToolCalls,
				messages.ChatMessageToolCall{ID: "l", Name: "lookup", Arguments: `{}`},
				messages.ChatMessageToolCall{ID: "d", Name: "danger", Arguments: `{}`})
			return batch
		}
		return answer("done")
	}}
	cb := &AgentCallbacks{ApproveToolCalls: func(_ context.Context, calls []messages.ChatMessageToolCall) ([]bool, error) {
		approved := make([]bool, len(calls))
		for i, call := range calls {
			approved[i] = call.Name != "danger"
		}
		return approved, nil
	}}
	agent := NewAgent(llm, registry, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: userAsks("go"), MaxContextTokens: 4_000}, cb)
	if err != nil {
		t.Fatal(err)
	}
	if last := llm.tools[len(llm.tools)-1]; len(last) != 0 {
		t.Fatalf("the request after the batch offered %v, want the finishing request", last)
	}
	for _, msg := range response.AllMessages {
		switch msg.ToolName {
		case "danger":
			if msg.Content != ToolDeniedContent {
				t.Fatalf("the denied call's result became %q", msg.Content)
			}
		case "lookup":
			if !strings.Contains(msg.Content, "ZEBRA-7731") {
				t.Fatalf("a result the finishing request has room for became %q", msg.Content)
			}
		}
	}
}

// The finishing request is committed like any other before it is sent: the
// generated prefix is checkpointed and its projection reported.
func TestFinishingRequestIsCommittedBeforeItIsSent(t *testing.T) {
	shared := NewCalibration()
	reader := &pageTool{hook: func() { shared.learnUsage("test/same", 2, 1) }}
	llm := &scriptLLM{fn: func(_ *CompletionRequest, call int) messages.ChatMessage {
		if call == 0 {
			return callTools("read_page", 1, `{}`)
		}
		return answer("done")
	}}
	checkpoints, projections, generated := 0, 0, 0
	cb := &AgentCallbacks{
		Checkpoint: func(_ context.Context, c AgentCheckpoint) error {
			if c.Request {
				checkpoints++
				generated = len(c.Generated)
			}
			return nil
		},
		OnRequestProjection: func(int, ProjectionStats) { projections++ },
	}
	agent := NewAgent(llm, registryWith(reader), AgentConfig{Calibration: shared, ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "You are a test."},
		{Role: messages.MessageRoleUser, Content: "Do the thing. " + strings.Repeat("context ", 800)},
	}
	if _, err := agent.Run(context.Background(), &CompletionRequest{Model: "test/same", Messages: history, MaxContextTokens: 6_000}, cb); err != nil {
		t.Fatal(err)
	}
	if last := llm.tools[len(llm.tools)-1]; len(last) != 0 {
		t.Fatalf("the last request offered %v, want the finishing request", last)
	}
	if checkpoints != llm.calls || projections != llm.calls {
		t.Fatalf("%d requests, %d request checkpoints, %d projections reported", llm.calls, checkpoints, projections)
	}
	if generated < 2 {
		t.Fatalf("the finishing request's checkpoint carries %d messages, want the tool exchange", generated)
	}
}

// The memo's measure of a history with something appended is the measure of
// the whole, whatever is appended: nothing, results, a reply with its
// results, input, or a usage record; from any point of the history, with and
// without a store, for a model with and without images and tools.
func TestRoomMemoMeasuresAsTheWholeDoes(t *testing.T) {
	offered := registryWith(fetchTool(nil), &pageTool{}).All()
	ref := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("png")})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "You are a test."},
		{Role: messages.MessageRoleUser, Content: "first " + strings.Repeat("q ", 300)},
		{Role: messages.MessageRoleAssistant, Content: "noted"},
		{Role: messages.MessageRoleUser, Content: "look", Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref}}},
		callTools("fetch", 2, `{"q":"x"}`),
		{Role: messages.MessageRoleTool, ToolCallID: "fetch0", ToolName: "fetch", Content: strings.Repeat("fetched ", 400)},
		{Role: messages.MessageRoleTool, ToolCallID: "fetch1", ToolName: "fetch", Content: "short"},
		{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyUsageOnly: true}},
		callTools("read_page", 1, `{}`),
		{Role: messages.MessageRoleTool, ToolCallID: "read_page0", ToolName: "read_page", Content: strings.Repeat("word ", 500)},
	}
	response := callTools("fetch", 1, `{"q":"y"}`)
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "fetch0", ToolName: "fetch", Content: strings.Repeat("more ", 300)}
	extras := [][]messages.ChatMessage{
		nil,
		{result},
		{response},
		{response, result},
		{{Role: messages.MessageRoleUser, Content: "and now?"}},
		{{Role: messages.MessageRoleUser, Content: "see", Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref}}}, result},
		{{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyUsageOnly: true}}},
	}
	falseCap := false
	for _, caps := range []*ModelCapabilities{nil, {InputModalities: []string{"text"}}, {InputModalities: []string{"text", "image"}, Tools: &falseCap}} {
		for _, hasStore := range []bool{false, true} {
			b := batchBudget{
				req:           &CompletionRequest{Model: "test/m", MaxContextTokens: 50_000},
				agentTools:    projectionToolsFor(offered),
				nextSchemas:   100,
				finishSchemas: 10,
				sandbox:       5,
				hasStore:      hasStore,
				caps:          caps,
			}
			memoed := b
			memoed.memo = &roomMemo{}
			for n := 0; n <= len(history); n++ {
				for e, extra := range extras {
					wantNext, wantFinish := b.room(append(history[:n:n], extra...))
					gotNext, gotFinish := memoed.roomWith(history[:n], extra)
					if gotNext != wantNext || gotFinish != wantFinish {
						t.Fatalf("caps %+v store %v prefix %d extra %d: memo measures %d, %d; the whole %d, %d", caps, hasStore, n, e, gotNext, gotFinish, wantNext, wantFinish)
					}
				}
			}
		}
	}
}
