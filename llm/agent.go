package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"golang.org/x/sync/errgroup"
)

// ToolDeniedContent is the tool-result content recorded when the caller
// denies a tool call via AgentCallbacks.ApproveToolCalls. Callers can match
// on this exact string to detect denials (e.g. to filter denied exchanges
// out of persisted history).
const ToolDeniedContent = "Tool call denied by user."

// ToolInterruptedContent is the tool-result content recorded for a call whose
// batch aborted (cancellation or artifact-store failure) before a real result
// was captured. The tool may or may not have run. The stub keeps every tool
// call answered, so a partial run's messages stay valid provider history.
const ToolInterruptedContent = "Tool execution was interrupted; no result was recorded and the tool may or may not have run."

// ErrMaxIterations is returned (with a partial AgentResponse) when the agent
// loop reaches its MaxIterations cap before the model finishes.
var ErrMaxIterations = errors.New("max iterations exceeded")

// Agent handles the agentic loop without owning session state.
// It executes completions with automatic tool call handling.
type Agent struct {
	client        LLM
	tools         *tools.ToolRegistry
	config        AgentConfig
	artifactStore artifacts.Store
	artifactMu    sync.RWMutex
	artifactRefs  map[string]artifacts.Ref
	artifactOrder []string

	// transcript is the durable conversation snapshot served by
	// read_transcript, refreshed as the run generates messages.
	transcriptMu   sync.RWMutex
	transcript     []messages.ChatMessage
	transcriptTool bool
}

// AgentConfig configures agent behavior
type AgentConfig struct {
	MaxIterations    int             // Maximum LLM calls before giving up (default: 250)
	ToolTimeout      time.Duration   // Per-tool execution timeout (0 = no timeout)
	MaxParallelTools int             // Maximum parallel tool executions (0 = unlimited)
	ResponseTool     string          // If set, require final response via this tool
	ArtifactStore    artifacts.Store // Optional private store for context artifacts
}

// AgentCallbacks provides hooks for observing and customizing agent execution
type AgentCallbacks struct {
	// OnReasoning is called when reasoning/thinking content is streamed
	OnReasoning func(content string)

	// OnContent is called when regular content is streamed
	OnContent func(content string)

	// BeforeToolExecute is called before each tool executes.
	// Returns a (possibly modified) context to pass to the tool.
	// Use this to inject context values that tools need (e.g., IRC context).
	// If nil, context passes through unchanged.
	BeforeToolExecute func(ctx context.Context, call messages.ChatMessageToolCall, args map[string]any) context.Context

	// OnToolStart is called once before parallel tool execution begins with all tool calls
	OnToolStart func(calls []messages.ChatMessageToolCall)

	// ApproveToolCalls is called before parallel execution with all pending tool calls.
	// Returns a bool slice indicating which tools are approved.
	// If nil, all tools are approved.
	ApproveToolCalls func(calls []messages.ChatMessageToolCall) []bool

	// OnToolEnd is called after each tool executes
	OnToolEnd func(call messages.ChatMessageToolCall, result string, duration time.Duration, err error)

	// OnComplete is called when the final response is ready (no more tool calls)
	OnComplete func(response *messages.ChatMessage)

	// OnError is called when an error occurs
	OnError func(err error)
}

// AgentResponse contains the results after Run completes
type AgentResponse struct {
	Message        *messages.ChatMessage  // Final assistant message (no tool calls)
	AllMessages    []messages.ChatMessage // All messages generated (assistant + tool results)
	IterationCount int                    // Number of LLM calls made
	Projection     ProjectionStats        // Final provider-visible context projection
	PromptCache    PromptCacheStats       // Provider-reported cache use across all LLM calls
}

// PromptCacheStats is provider-reported prompt-cache accounting. Zero values
// mean either no cache activity or that the provider did not report details;
// Polly never estimates cache hits.
type PromptCacheStats struct {
	ReadInputTokens  int
	WriteInputTokens int
}

func hasToolCall(msg *messages.ChatMessage, name string) bool {
	for _, tc := range msg.ToolCalls {
		if tc.Name == name {
			return true
		}
	}
	return false
}

// NewAgent creates an agent that handles the agentic loop and its compact,
// session-scoped model projection. The agent does not own transcript state:
// callers provide messages and persist the generated messages themselves.
func NewAgent(client LLM, registry *tools.ToolRegistry, config AgentConfig) *Agent {
	if config.MaxIterations <= 0 {
		config.MaxIterations = 250
	}
	if registry == nil && config.ArtifactStore != nil {
		registry = tools.NewToolRegistry(nil)
	}
	agent := &Agent{
		client: client, tools: registry, config: config,
		artifactStore: config.ArtifactStore,
		artifactRefs:  make(map[string]artifacts.Ref),
	}
	if config.ArtifactStore != nil {
		reader := &readArtifactTool{store: config.ArtifactStore, lookup: agent.lookupArtifact}
		registry.Register(reader)
		registry.MarkAlwaysAllowed(reader.GetName())
		lister := &listArtifactsTool{list: agent.listArtifacts}
		registry.Register(lister)
		registry.MarkAlwaysAllowed(lister.GetName())
	}
	if registry != nil {
		viewer := tools.NewViewImageTool(registry)
		registry.Register(viewer)
		registry.MarkAlwaysAllowed(viewer.GetName())
		transcript := &readTranscriptTool{snapshot: agent.transcriptSnapshot}
		registry.Register(transcript)
		registry.MarkAlwaysAllowed(transcript.GetName())
		agent.transcriptTool = true
	}
	return agent
}

func (a *Agent) setTranscript(history []messages.ChatMessage) {
	snapshot := append([]messages.ChatMessage(nil), history...)
	a.transcriptMu.Lock()
	a.transcript = snapshot
	a.transcriptMu.Unlock()
}

func (a *Agent) transcriptSnapshot() []messages.ChatMessage {
	a.transcriptMu.RLock()
	defer a.transcriptMu.RUnlock()
	return a.transcript
}

// SetToolTimeout updates the per-tool-call timeout for subsequent runs. Not
// safe to call while a Run is in flight.
func (a *Agent) SetToolTimeout(d time.Duration) {
	a.config.ToolTimeout = d
}

// Run executes a completion with automatic tool call handling.
// It loops until the LLM returns a response with no tool calls,
// or until MaxIterations is reached.
//
// The caller provides messages in req.Messages and receives back
// all generated messages (assistant responses + tool results) in
// AgentResponse.AllMessages. The caller is responsible for adding
// these to their session.
//
// On error, Run still returns an AgentResponse carrying whatever was
// generated before the failure (Message may be nil) so callers can account
// for iterations and tokens actually spent. AllMessages always ends at a
// provider-valid boundary — a tool batch the failure cut short is completed
// with interrupted-tool stubs — so callers can persist the partial turn and
// replay it in later requests.
func (a *Agent) Run(ctx context.Context, req *CompletionRequest, cb *AgentCallbacks) (*AgentResponse, error) {
	// Work with a copy of messages - don't mutate input
	msgs := make([]messages.ChatMessage, len(req.Messages))
	copy(msgs, req.Messages)

	// Resolve skills once before the loop to avoid double-augmentation
	// on subsequent iterations (where msgs[0] already has the augmented prompt).
	loopReq := *req
	if loopReq.Skills != nil && !loopReq.Skills.IsEmpty() {
		loopReq.Messages = msgs
		msgs = loopReq.ResolvedMessages()
		loopReq.Skills = nil
	}

	var allGenerated []messages.ChatMessage
	var nudgedResponseTool bool
	var responseToolCalled bool
	var lastProjection ProjectionStats
	var promptCache PromptCacheStats
	a.resetArtifactIndex(msgs)
	a.setTranscript(msgs)
	responseFor := func(message *messages.ChatMessage, iterations int) *AgentResponse {
		return &AgentResponse{
			Message: message, AllMessages: allGenerated, IterationCount: iterations,
			Projection: lastProjection, PromptCache: promptCache,
		}
	}

	for iteration := 0; iteration < a.config.MaxIterations; iteration++ {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return responseFor(nil, iteration), ctx.Err()
		default:
		}

		// Build request with accumulated messages
		iterReq := loopReq
		// Tool schemas share the model's context with the projected messages;
		// budget the projection for what remains after them.
		budget := req.MaxContextTokens
		if budget > 0 && a.tools != nil {
			overhead := estimateToolSchemaTokens(a.tools.All())
			if overhead >= budget {
				err := fmt.Errorf("tool schemas alone need about %d tokens, exceeding the %d-token context budget", overhead, budget)
				if cb != nil && cb.OnError != nil {
					cb.OnError(err)
				}
				return responseFor(nil, iteration), err
			}
			budget -= overhead
		}
		projected, projection, err := projectMessages(ctx, msgs, budget, a.artifactStore, a.transcriptTool)
		a.applyDurableToolSpills(msgs, projection.toolSpills)
		a.applyDurableToolSpills(allGenerated, projection.toolSpills)
		for _, ref := range projection.artifactRefs {
			a.indexArtifact(ref)
		}
		projection.artifactRefs = nil
		projection.toolSpills = nil
		lastProjection = projection
		if err != nil {
			if cb != nil && cb.OnError != nil {
				cb.OnError(err)
			}
			return responseFor(nil, iteration), err
		}
		a.indexArtifactMessages(projected)
		iterReq.Messages = projected
		if a.tools != nil {
			iterReq.Tools = a.tools.All()
		}
		if iterReq.PromptCacheKey == "" {
			if key, keyErr := derivePromptCacheKey(&iterReq, msgs); keyErr == nil {
				iterReq.PromptCacheKey = key
			} else {
				slog.Debug("prompt_cache_key_omitted", "error", keyErr)
			}
		}

		// Stream completion
		processor := messages.NewStreamProcessor()

		events := a.client.ChatCompletionStream(ctx, &iterReq, processor)

		// Process events
		response, err := a.processEvents(ctx, events, cb)
		if err != nil {
			return responseFor(nil, iteration+1), err
		}
		promptCache.ReadInputTokens += response.GetCacheReadInputTokens()
		promptCache.WriteInputTokens += response.GetCacheWriteInputTokens()

		// Ensure content is never null — some providers reject null content in history
		if response.Content == "" && len(response.ToolCalls) == 0 {
			response.Content = " "
		}
		msgs = append(msgs, *response)
		allGenerated = append(allGenerated, *response)
		a.setTranscript(msgs)

		// Check stop reason to determine next action
		switch response.StopReason {
		case messages.StopReasonEndTurn:
			if a.config.ResponseTool != "" && !responseToolCalled && !nudgedResponseTool {
				nudgedResponseTool = true
				nudge := messages.ChatMessage{
					Role:     messages.MessageRoleUser,
					Content:  "Respond using the " + a.config.ResponseTool + " tool.",
					Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true},
				}
				msgs = append(msgs, nudge)
				allGenerated = append(allGenerated, nudge)
				continue
			}
			// Normal completion
			if cb != nil && cb.OnComplete != nil {
				cb.OnComplete(response)
			}
			return responseFor(response, iteration+1), nil

		case messages.StopReasonMaxTokens:
			// Response truncated - warn and return
			slog.Debug("response_truncated", "reason", "max_tokens")
			if cb != nil && cb.OnComplete != nil {
				cb.OnComplete(response)
			}
			return responseFor(response, iteration+1), nil

		case messages.StopReasonContentFilter:
			// Response blocked by safety/policy
			err := errors.New("response blocked by content filter")
			if cb != nil && cb.OnError != nil {
				cb.OnError(err)
			}
			return responseFor(response, iteration+1), err

		case messages.StopReasonError:
			// Model produced malformed output
			err := errors.New("model produced malformed output")
			if cb != nil && cb.OnError != nil {
				cb.OnError(err)
			}
			return responseFor(response, iteration+1), err

		case messages.StopReasonToolUse:
			if len(response.ToolCalls) == 0 {
				err := errors.New("model requested tool use without any tool calls")
				if cb != nil && cb.OnError != nil {
					cb.OnError(err)
				}
				return responseFor(response, iteration+1), err
			}
			// Continue to execute tool calls below

		default:
			// Unknown stop reason with no tool calls = treat as completion
			if len(response.ToolCalls) == 0 {
				if a.config.ResponseTool != "" && !responseToolCalled && !nudgedResponseTool {
					nudgedResponseTool = true
					nudge := messages.ChatMessage{
						Role:     messages.MessageRoleUser,
						Content:  "Respond using the " + a.config.ResponseTool + " tool.",
						Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true},
					}
					msgs = append(msgs, nudge)
					allGenerated = append(allGenerated, nudge)
					continue
				}
				if cb != nil && cb.OnComplete != nil {
					cb.OnComplete(response)
				}
				return responseFor(response, iteration+1), nil
			}
			// Has tool calls, continue to execute them
		}

		// Track if response tool was called in this batch
		if a.config.ResponseTool != "" {
			for _, tc := range response.ToolCalls {
				if tc.Name == a.config.ResponseTool {
					responseToolCalled = true
					break
				}
			}
		}

		// Execute tool calls in parallel
		toolMsgs, toolErr := a.executeToolsParallel(ctx, response.ToolCalls, cb)
		if toolErr != nil {
			// The batch aborted, but tools that finished already changed the
			// world: keep their real results, stub the unanswered calls, and
			// return the completed batch so AllMessages stays replayable.
			toolMsgs = completeAbortedToolBatch(response.ToolCalls, toolMsgs)
			if a.tools != nil {
				a.tools.CommitPendingChanges()
			}
			a.indexArtifactMessages(toolMsgs)
			allGenerated = append(allGenerated, toolMsgs...)
			return responseFor(response, iteration+1), toolErr
		}
		if a.tools != nil {
			a.tools.CommitPendingChanges()
		}
		a.indexArtifactMessages(toolMsgs)
		msgs = append(msgs, toolMsgs...)
		allGenerated = append(allGenerated, toolMsgs...)

		// Short-circuit when every tool in the batch was denied. Looping to
		// feed the denials back would just make the model editorialize ("I
		// can't run that"), which pollutes history and teaches it to refuse
		// preemptively on later turns. The caller already saw the denial.
		if allDenied(toolMsgs) {
			if cb != nil && cb.OnComplete != nil {
				cb.OnComplete(response)
			}
			return responseFor(response, iteration+1), nil
		}

		// Short-circuit when the response tool was called: the caller
		// extracts the structured response from the tool call's arguments,
		// so making another LLM call to "process" the tool result would
		// generate plain text the caller discards anyway.
		if responseToolCalled {
			if cb != nil && cb.OnComplete != nil {
				cb.OnComplete(response)
			}
			return responseFor(response, iteration+1), nil
		}
	}

	// Stamp the last generated assistant message (the one whose tool calls
	// exhausted the budget) so callers that persist AllMessages record why the
	// turn ended. The stamp must land in allGenerated itself — msgs holds
	// separate copies that are never returned.
	var last *messages.ChatMessage
	for i := len(allGenerated) - 1; i >= 0; i-- {
		if allGenerated[i].Role == messages.MessageRoleAssistant {
			last = &allGenerated[i]
			last.StopReason = messages.StopReasonMaxIterations
			break
		}
	}
	if cb != nil && cb.OnError != nil {
		cb.OnError(ErrMaxIterations)
	}
	// Return the partial response so the caller can save the history
	return responseFor(last, a.config.MaxIterations), ErrMaxIterations
}

// processEvents processes the event stream and returns the final message
func (a *Agent) processEvents(ctx context.Context, events <-chan *messages.StreamEvent, cb *AgentCallbacks) (*messages.ChatMessage, error) {
	var response *messages.ChatMessage

	for event := range events {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		switch event.Type {
		case messages.EventTypeReasoning:
			if cb != nil && cb.OnReasoning != nil {
				cb.OnReasoning(event.Content)
			}
		case messages.EventTypeContent:
			if cb != nil && cb.OnContent != nil {
				cb.OnContent(event.Content)
			}
		case messages.EventTypeComplete:
			response = event.Message
		case messages.EventTypeError:
			if cb != nil && cb.OnError != nil {
				cb.OnError(event.Error)
			}
			return nil, event.Error
		}
	}

	if response == nil {
		return nil, errors.New("no response received from LLM")
	}

	return response, nil
}

// executeTool executes a single tool call and returns the result message. Tool
// execution failures remain durable tool outcomes, while artifact persistence
// failures abort the turn because a configured store is authoritative.
func (a *Agent) executeTool(ctx context.Context, tc messages.ChatMessageToolCall, cb *AgentCallbacks) (messages.ChatMessage, error) {
	// Parse args early so we can pass them to BeforeToolExecute
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		args = nil // Will be handled in executeToolCall
	}

	// Allow callback to modify context (e.g., inject IRC context)
	execCtx := ctx
	if cb != nil && cb.BeforeToolExecute != nil {
		execCtx = cb.BeforeToolExecute(ctx, tc, args)
	}

	start := time.Now()
	output, err := a.executeToolCall(execCtx, tc, args)
	duration := time.Since(start)
	result := output.Text
	for _, media := range output.Media {
		result += fmt.Sprintf("\n[%s media: %s, %d bytes]", media.MIMEType, media.Name, len(media.Data))
	}

	msg, artifactErr := a.toolOutputMessage(execCtx, tc, output)
	if cb != nil && cb.OnToolEnd != nil {
		cb.OnToolEnd(tc, result, duration, errors.Join(err, artifactErr))
	}
	if artifactErr != nil {
		return messages.ChatMessage{}, artifactErr
	}
	// Record an explicit ordinary tool outcome so transcript hydration can
	// distinguish it from older tool messages whose outcome is unknown. Tool
	// failures must not use the terminal stream-error metadata.
	msg.SetToolSucceeded(err == nil)
	return msg, nil
}

// executeToolCall performs the actual tool execution
func (a *Agent) executeToolCall(ctx context.Context, tc messages.ChatMessageToolCall, args map[string]any) (tools.ToolOutput, error) {
	// Apply timeout
	if a.config.ToolTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.config.ToolTimeout)
		defer cancel()
	}

	// Parse args if not already parsed
	if args == nil {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			errMsg := fmt.Sprintf("Error parsing arguments: %v", err)
			return tools.ToolOutput{Text: errMsg}, err
		}
	}

	// Get tool from registry
	if a.tools == nil {
		errMsg := fmt.Sprintf("Tool not found: %s (no registry)", tc.Name)
		return tools.ToolOutput{Text: errMsg}, errors.New("no tool registry")
	}

	tool, exists, allowed := a.tools.GetIfAllowed(tc.Name)
	if !exists {
		errMsg := fmt.Sprintf("Tool not found: %s", tc.Name)
		return tools.ToolOutput{Text: errMsg}, errors.New("tool not found: " + tc.Name)
	}
	if !allowed {
		errMsg := fmt.Sprintf("Tool not allowed by active skill policy: %s", tc.Name)
		return tools.ToolOutput{Text: errMsg}, errors.New("tool not allowed: " + tc.Name)
	}

	// Execute
	var output tools.ToolOutput
	var err error
	if rich, ok := tool.(tools.OutputTool); ok {
		output, err = rich.ExecuteOutput(ctx, args)
	} else {
		output.Text, err = tool.Execute(ctx, args)
	}
	if err != nil {
		if msg, ok := tools.FormatToolError(err); ok {
			output.Text = mergeToolErrorText(msg, output.Text)
			return output, err
		}
		if ctx.Err() == context.DeadlineExceeded {
			output.Text = mergeToolErrorText(fmt.Sprintf("Error: tool execution timed out after %v", a.config.ToolTimeout), output.Text)
			return output, err
		}
		output.Text = mergeToolErrorText(fmt.Sprintf("Error: %v", err), output.Text)
		return output, err
	}

	return output, nil
}

func mergeToolErrorText(errorText, resultText string) string {
	resultText = strings.TrimSpace(resultText)
	if resultText == "" || strings.Contains(errorText, resultText) {
		return errorText
	}
	return errorText + "\n" + resultText
}

func (a *Agent) toolOutputMessage(ctx context.Context, tc messages.ChatMessageToolCall, output tools.ToolOutput) (messages.ChatMessage, error) {
	msg := messages.ChatMessage{Role: messages.MessageRoleTool, Content: output.Text, ToolCallID: tc.ID, ToolName: tc.Name}
	var textArtifact *artifacts.Ref
	if !isRecallToolName(tc.Name) && output.Text != "" && estimatedStringTokens(output.Text) > toolInlineTokenLimit && a.artifactStore != nil {
		ref, err := a.artifactStore.Put(ctx, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: toolArtifactName(msg), Data: []byte(output.Text)})
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("store text artifact for tool %q: %w", tc.Name, err)
		}
		msg.Parts = append(msg.Parts, messages.ContentPart{Type: "artifact", Artifact: &ref})
		textArtifact = &ref
	}
	for _, media := range output.Media {
		kind := artifacts.KindBinary
		partType := "artifact"
		if strings.HasPrefix(strings.ToLower(media.MIMEType), "image/") {
			kind = artifacts.KindImage
			partType = "image_artifact"
		}
		if a.artifactStore != nil {
			ref, err := a.artifactStore.Put(ctx, artifacts.Blob{Kind: kind, MIMEType: media.MIMEType, Name: media.Name, Reference: media.Reference, Data: media.Data})
			if err != nil {
				return messages.ChatMessage{}, fmt.Errorf("store %s artifact %q for tool %q: %w", kind, media.Name, tc.Name, err)
			}
			msg.Parts = append(msg.Parts, messages.ContentPart{Type: partType, Artifact: &ref, MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken})
			if textArtifact == nil {
				descriptor := artifactMediaDescriptor(ref)
				msg.Content = strings.TrimSpace(msg.Content + "\n" + descriptor)
			}
			continue
		}
		if kind == artifacts.KindImage {
			msg.Parts = append(msg.Parts, messages.ContentPart{Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(media.Data), MimeType: media.MIMEType, FileName: media.Name})
		} else {
			msg.Parts = append(msg.Parts, messages.ContentPart{Type: "file", Text: base64.StdEncoding.EncodeToString(media.Data), MimeType: media.MIMEType, FileName: media.Name})
			msg.Content = strings.TrimSpace(msg.Content + "\n" + fmt.Sprintf("[binary media %s (%s), %d bytes; payload retained outside model text]", media.Name, media.MIMEType, len(media.Data)))
		}
	}
	if textArtifact != nil {
		head, tail := previewWindows([]byte(output.Text))
		msg.Content = artifactPreviewWithDescriptors(*textArtifact, head, tail, msg)
	}
	return msg, nil
}

func (a *Agent) indexArtifactMessages(history []messages.ChatMessage) {
	for _, msg := range history {
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		for _, part := range msg.Parts {
			if part.Artifact != nil {
				a.indexArtifact(*part.Artifact)
			}
		}
	}
}

// resetArtifactIndex scopes read_artifact authorization to the transcript
// supplied for this run. An Agent can outlive /reset, but references from the
// cleared conversation must not remain authorized merely because a prior run
// indexed them.
func (a *Agent) resetArtifactIndex(history []messages.ChatMessage) {
	a.artifactMu.Lock()
	a.artifactRefs = make(map[string]artifacts.Ref)
	a.artifactOrder = nil
	a.artifactMu.Unlock()
	a.indexArtifactMessages(history)
}

func (a *Agent) indexArtifact(ref artifacts.Ref) {
	if !artifacts.ValidID(ref.ID) {
		return
	}
	a.artifactMu.Lock()
	current, exists := a.artifactRefs[ref.ID]
	if !exists {
		a.artifactOrder = append(a.artifactOrder, ref.ID)
	}
	if !exists || artifactKindPriority(ref.Kind) > artifactKindPriority(current.Kind) {
		a.artifactRefs[ref.ID] = ref
	}
	a.artifactMu.Unlock()
}

// listArtifacts returns the run's authorized refs in first-reference order:
// durable-transcript order at run start, then in-run discovery order.
func (a *Agent) listArtifacts() []artifacts.Ref {
	a.artifactMu.RLock()
	defer a.artifactMu.RUnlock()
	refs := make([]artifacts.Ref, 0, len(a.artifactOrder))
	for _, id := range a.artifactOrder {
		refs = append(refs, a.artifactRefs[id])
	}
	return refs
}

func artifactKindPriority(kind artifacts.Kind) int {
	switch kind {
	case artifacts.KindText:
		return 3
	case artifacts.KindImage:
		return 2
	case artifacts.KindBinary:
		return 1
	default:
		return 0
	}
}

func (a *Agent) lookupArtifact(id string) (artifacts.Ref, bool) {
	a.artifactMu.RLock()
	ref, ok := a.artifactRefs[id]
	a.artifactMu.RUnlock()
	return ref, ok
}

func (a *Agent) applyDurableToolSpills(history []messages.ChatMessage, spills []toolResultSpill) {
	for _, spill := range spills {
		for i := len(history) - 1; i >= 0; i-- {
			msg := &history[i]
			if msg.Role != messages.MessageRoleTool || msg.ToolCallID != spill.ToolCallID || msg.ToolName != spill.ToolName || msg.Content != spill.Content || textArtifactRef(*msg) != nil {
				continue
			}
			ref := spill.Ref
			msg.Parts = append(msg.Parts, messages.ContentPart{Type: "artifact", Artifact: &ref})
			// The durable final form is exactly what the spilling projection
			// sent, so later pass-through projections stay byte-identical.
			msg.Content = spill.Receipt
			break
		}
	}
}

// executeToolsParallel executes multiple tool calls concurrently and returns results in order.
// If context is cancelled, all running tools are notified via their context.
func (a *Agent) executeToolsParallel(ctx context.Context, toolCalls []messages.ChatMessageToolCall, cb *AgentCallbacks) ([]messages.ChatMessage, error) {
	if len(toolCalls) == 0 {
		return nil, nil
	}
	// Fire callback once with all tools before parallel execution
	if cb != nil && cb.OnToolStart != nil {
		cb.OnToolStart(toolCalls)
	}

	results := make([]messages.ChatMessage, len(toolCalls))

	// Determine which tools are approved
	approved := make([]bool, len(toolCalls))
	for i := range approved {
		approved[i] = true
	}
	if cb != nil && cb.ApproveToolCalls != nil {
		approved = cb.ApproveToolCalls(toolCalls)
	}

	// Fill in denied results immediately
	var approvedIndices []int
	for i, tc := range toolCalls {
		if !approved[i] {
			results[i] = messages.ChatMessage{
				Role:       messages.MessageRoleTool,
				Content:    ToolDeniedContent,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
			}
			if cb != nil && cb.OnToolEnd != nil {
				cb.OnToolEnd(tc, results[i].Content, 0, nil)
			}
		} else {
			approvedIndices = append(approvedIndices, i)
		}
	}

	g, ctx := errgroup.WithContext(ctx)

	// Semaphore for concurrency limiting
	sem := make(chan struct{}, a.effectiveParallelism(len(approvedIndices)))

	for _, idx := range approvedIndices {
		idx := idx
		tc := toolCalls[idx]
		g.Go(func() error {
			// Acquire semaphore (respects context cancellation)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return ctx.Err()
			}

			result, err := a.executeTool(ctx, tc, cb)
			if err != nil {
				return err
			}
			results[idx] = result
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return results, err // Return partial results + error
	}
	return results, nil
}

// effectiveParallelism returns the concurrency limit based on config and number of tools.
func (a *Agent) effectiveParallelism(n int) int {
	if a.config.MaxParallelTools <= 0 || a.config.MaxParallelTools > n {
		return n
	}
	return a.config.MaxParallelTools
}

// completeAbortedToolBatch fills the holes an aborted parallel batch left
// behind. Results from tools that finished (including denial stubs) are kept
// verbatim — their side effects are real — and every unanswered call gets an
// interrupted stub so the assistant message's tool calls all stay answered.
func completeAbortedToolBatch(calls []messages.ChatMessageToolCall, results []messages.ChatMessage) []messages.ChatMessage {
	completed := make([]messages.ChatMessage, len(calls))
	for i, tc := range calls {
		if i < len(results) && results[i].Role == messages.MessageRoleTool {
			completed[i] = results[i]
			continue
		}
		stub := messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			Content:    ToolInterruptedContent,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
		}
		stub.SetToolSucceeded(false)
		completed[i] = stub
	}
	return completed
}

// allDenied reports whether every message in toolMsgs is a denial stub.
func allDenied(toolMsgs []messages.ChatMessage) bool {
	if len(toolMsgs) == 0 {
		return false
	}
	for _, m := range toolMsgs {
		if m.Content != ToolDeniedContent {
			return false
		}
	}
	return true
}

// StripDeniedExchanges removes tool-denial pairs from a message slice so they
// don't pollute persisted history. It drops the "Tool call denied by user."
// tool-result messages and strips the matching tool_calls from the assistant
// messages that proposed them. An assistant message left with no content and
// no remaining tool_calls is dropped entirely.
func StripDeniedExchanges(msgs []messages.ChatMessage) []messages.ChatMessage {
	deniedIDs := map[string]bool{}
	for _, m := range msgs {
		if m.Role == messages.MessageRoleTool && m.Content == ToolDeniedContent {
			deniedIDs[m.ToolCallID] = true
		}
	}
	if len(deniedIDs) == 0 {
		return msgs
	}

	out := make([]messages.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == messages.MessageRoleTool && deniedIDs[m.ToolCallID] {
			continue
		}
		if m.Role == messages.MessageRoleAssistant && len(m.ToolCalls) > 0 {
			remaining := make([]messages.ChatMessageToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if !deniedIDs[tc.ID] {
					remaining = append(remaining, tc)
				}
			}
			m.ToolCalls = remaining
			if len(remaining) == 0 && m.Content == "" {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}
