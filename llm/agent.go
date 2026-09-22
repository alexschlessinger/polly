package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

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

type iterationLimitKey struct{}

// WithIterationLimit bounds model calls for this run and runs using its context.
// It can only lower the agent's configured limit and does not mutate the agent.
func WithIterationLimit(ctx context.Context, limit int) context.Context {
	if limit <= 0 {
		return ctx
	}
	if existing, ok := ctx.Value(iterationLimitKey{}).(int); ok && existing < limit {
		limit = existing
	}
	return context.WithValue(ctx, iterationLimitKey{}, limit)
}

// ErrMaxIterations is returned (with a partial AgentResponse) when the agent
// loop reaches its MaxIterations cap before the model finishes.
var ErrMaxIterations = errors.New("max iterations exceeded")

// ErrInvalidToolApproval means the host returned a different number of approval
// decisions than requested. No tool in the batch is executed.
var ErrInvalidToolApproval = errors.New("invalid tool approval decisions")

// Agent handles the agentic loop without owning session state.
// It executes completions with automatic tool call handling.
type Agent struct {
	client      LLM
	tools       *tools.ToolRegistry
	sourceTools *tools.ToolRegistry
	// The completion builder can advertise an explicit WithTools selection.
	// Dispatch still uses the registry's current execution policy.
	requestTools  []tools.Tool
	config        AgentConfig
	artifactStore artifacts.Store
	artifactMu    sync.RWMutex
	artifactRefs  map[string]artifacts.Ref
	artifactOrder []string

	// transcript is the durable conversation snapshot served by
	// read_transcript, refreshed as the run generates messages.
	transcriptMu       sync.RWMutex
	transcript         []messages.ChatMessage
	transcriptText     strings.Builder
	transcriptRendered int
	transcriptIndex    int
}

// AgentConfig configures agent behavior
type AgentConfig struct {
	MaxIterations    int           // Maximum LLM calls before giving up (default: 1024)
	ToolTimeout      time.Duration // Per-tool execution timeout (0 = no timeout)
	MaxParallelTools int           // Maximum parallel tool executions (0 = unlimited)
	ResponseTool     string        // If set, require final response via this tool
	// RequireResponseToolSuccess requires a successful receipt, not merely a
	// named call. ContinueAfterFinal owns recovery; the legacy nudge is disabled.
	RequireResponseToolSuccess bool
	ArtifactStore              artifacts.Store // Optional private store for context artifacts
	// OpenArtifact authorizes and opens an artifact not referenced by this
	// conversation, such as explicitly published swarm evidence. The host must
	// enforce access, return matching metadata, and leave the reader at byte zero.
	// It is used only by read_artifact; private store access is not widened.
	OpenArtifact func(context.Context, string) (artifacts.Ref, io.ReadCloser, error)
	// DisableTools is an absolute upper bound, including private built-ins.
	DisableTools bool
}

// AgentCallbacks provides host gates, observers and execution controls. A direct
// Agent.Run caller owns all hooks. A coordinator that runs agents on a host's
// behalf may own AdmitInput, Checkpoint and JournalToolBatch to commit its input
// receipts and tool intent atomically; it rejects host-supplied versions of those
// hooks and composes the rest on a private copy of the host's callbacks.
type AgentCallbacks struct {
	OnAdaptation func(RequestAdaptation)
	// ContinueAfterFinal keeps a coordinator's answer provisional while work
	// remains. Returning input continues this same iteration budget.
	ContinueAfterFinal func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error)
	// AdmitInput stages peer input at a provider boundary, after the complete
	// preceding tool batch. Checkpoint commits it only after projection succeeds.
	AdmitInput func(context.Context) ([]messages.ChatMessage, error)
	// Checkpoint persists the generated prefix (including admitted input).
	// It is opt-in; legacy callers still persist AllMessages once at turn end.
	Checkpoint func(context.Context, AgentCheckpoint) error
	// BeforeToolBatch runs before any call starts. It can reject incompatible
	// parallel operations and journal intent without persisting an unanswered
	// assistant tool-use message into replayable conversation history.
	BeforeToolBatch func(context.Context, []messages.ChatMessageToolCall) error
	// JournalToolBatch stores an in-flight prefix separately from replayable
	// history so recovery can report uncertain effects without re-running them.
	JournalToolBatch func(context.Context, AgentCheckpoint) error
	// AfterToolBatch may park an execution at a provider-valid boundary.
	AfterToolBatch func(context.Context) error
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
	// Returns exactly one decision per call, in order. Errors or malformed
	// decisions abort the entire batch before any tool executes.
	// If nil, all tools are approved.
	ApproveToolCalls func(context.Context, []messages.ChatMessageToolCall) ([]bool, error)

	// OnToolEnd is called after each tool executes
	OnToolEnd func(call messages.ChatMessageToolCall, result string, duration time.Duration, err error)

	// OnToolResult follows OnToolEnd with the durable typed result that will be
	// replayed to the model. Unlike the textual observer above, it preserves
	// image/artifact parts for human-facing inspection receipts.
	OnToolResult func(call messages.ChatMessageToolCall, result messages.ChatMessage)

	// BeforeFirstRequest is called once per Run, after the initial projection
	// succeeds and before the first provider call, with that projection's
	// statistics. A caller that must persist new input before spending
	// provider tokens does so here, knowing the request can be sent: a
	// projection failure returns from Run before this point with nothing
	// generated. Returning an error aborts the run with that error and no
	// provider call; OnError is not called for it. Artifacts the projection
	// externalized are already stored; they are content-addressed, so a
	// retry reuses them.
	BeforeFirstRequest func(stats ProjectionStats) error

	// OnRequestProjection observes each sendable request, after the initial
	// persistence gate. Iteration is zero-based within this Run.
	OnRequestProjection func(iteration int, stats ProjectionStats)

	// OnIterationUsage reports completed provider usage once per iteration,
	// before its tools run. Counts are for this iteration, not cumulative.
	// Missing provider usage is reported as zero.
	OnIterationUsage func(iteration, inputTokens, outputTokens int)

	// OnComplete is called when the final response is ready (no more tool calls)
	OnComplete func(response *messages.ChatMessage)

	// OnError is called when an error occurs. A provider stream that dies
	// before showing anything is re-sent (see streamRetries) and reports only
	// if the last attempt also fails.
	OnError func(err error)
}

type AgentCheckpoint struct {
	Generated  []messages.ChatMessage
	Iterations int
	Final      bool
	Request    bool
	// Err is the run's outcome and is set only on the Final checkpoint: nil
	// for a completed turn, otherwise the error Run is returning (a host park
	// sentinel, ErrMaxIterations, cancellation). Persistence uses it to tell a
	// parked execution from a finished one inside the same commit.
	Err error
}

// AgentResponse contains the results after Run completes
type AgentResponse struct {
	Message           *messages.ChatMessage  // Final assistant message (no tool calls)
	AllMessages       []messages.ChatMessage // All messages generated (assistant + tool results)
	IterationCount    int                    // Number of LLM calls made
	Projection        ProjectionStats        // Final provider-visible context projection
	PromptCache       PromptCacheStats       // Provider-reported cache use across all LLM calls
	PersistedMessages int                    // Prefix already acknowledged by Checkpoint.
}

// TokenUsage separates total provider usage from peak per-request context usage.
// Counts are provider-reported; unreported usage contributes zero.
type TokenUsage struct {
	TotalInput  int
	TotalOutput int
	PeakInput   int
}

// TokenUsage reports totals and peak input across this run's assistant messages.
func (r *AgentResponse) TokenUsage() TokenUsage {
	var usage TokenUsage
	for _, m := range r.AllMessages {
		if m.Role != messages.MessageRoleAssistant {
			continue
		}
		usage.TotalInput += m.GetInputTokens()
		usage.TotalOutput += m.GetOutputTokens()
		usage.PeakInput = max(usage.PeakInput, m.GetInputTokens())
	}
	return usage
}

// multiPass returns the agent's provider router, or nil for a custom LLM
// client, which has no process-local credentials and no model catalog.
func (a *Agent) multiPass() *MultiPass {
	m, _ := a.client.(*MultiPass)
	return m
}

// HasProviderKeyOverrides reports whether the client supports process-local credentials.
func (a *Agent) HasProviderKeyOverrides() bool { return a.multiPass() != nil }

// MissingAPIKey reports whether a request for model against baseURL would be
// refused for lack of a credential, naming the variable that supplies it.
// A client without a provider router refuses nothing.
func (a *Agent) MissingAPIKey(model, baseURL string) (envVar string, missing bool) {
	if m := a.multiPass(); m != nil {
		return m.MissingAPIKey(model, baseURL)
	}
	return "", false
}

// SetProviderAPIKey installs a process-local provider credential when the
// agent is backed by MultiPass. It returns false for custom LLM clients.
func (a *Agent) SetProviderAPIKey(provider, apiKey string) bool {
	m := a.multiPass()
	if m == nil {
		return false
	}
	m.SetAPIKey(provider, apiKey)
	return true
}

// ClearProviderAPIKey removes a process-local override without changing an
// environment-provided credential.
func (a *Agent) ClearProviderAPIKey(provider string) bool {
	m := a.multiPass()
	if m == nil {
		return false
	}
	m.ClearAPIKey(provider)
	return true
}

// ProviderAPIKeySource reports "session", "environment", or "" without
// revealing credential material.
func (a *Agent) ProviderAPIKeySource(provider string) string {
	if m := a.multiPass(); m != nil {
		return m.APIKeySource(provider)
	}
	return ""
}

// DiscoverModelContextWindow uses the agent's effective process-local
// credential without exposing it to the caller.
func (a *Agent) DiscoverModelContextWindow(ctx context.Context, model string) (int, error) {
	return discoverContextWindow(ctx, a, targetForRequest(&CompletionRequest{Model: model}))
}

// PromptCacheStats is provider-reported prompt-cache accounting. Zero values
// mean either no cache activity or that the provider did not report details;
// Polly never estimates cache hits.
type PromptCacheStats struct {
	ReadInputTokens  int
	WriteInputTokens int
}

// newAgent initializes the shared execution engine over a private view of
// the caller's registry; NewAgent also installs the private recall tools.
func newAgent(client LLM, registry *tools.ToolRegistry, config AgentConfig) *Agent {
	if config.MaxIterations <= 0 {
		config.MaxIterations = 1024
	}
	source := registry
	if registry != nil {
		registry = registry.Derive()
	} else if config.ArtifactStore != nil {
		registry = tools.NewToolRegistry(nil)
	}
	return &Agent{
		client: client, tools: registry, sourceTools: source, config: config,
		artifactStore: config.ArtifactStore,
		artifactRefs:  make(map[string]artifacts.Ref),
	}
}

// NewAgent creates an agent that handles the agentic loop and its compact,
// session-scoped model projection. The agent does not own transcript state:
// callers provide messages and persist the generated messages themselves.
// Agent built-ins are private to this agent. The caller retains ownership of
// registry and its configured tools; later registry changes remain visible.
// view_image is not an agent built-in: the registry's tool setup supplies it
// (natively through tools.WithNativeTools, or an independent toolset's own),
// and the agent never constructs or replaces it.
func NewAgent(client LLM, registry *tools.ToolRegistry, config AgentConfig) *Agent {
	agent := newAgent(client, registry, config)
	registry = agent.tools
	if config.ArtifactStore != nil && !config.DisableTools {
		reader := &readArtifactTool{store: config.ArtifactStore, lookup: agent.lookupArtifact, open: config.OpenArtifact}
		registry.Register(reader)
		registry.MarkAlwaysAllowed(reader.GetName())
		lister := &listArtifactsTool{list: agent.listArtifacts}
		registry.Register(lister)
		registry.MarkAlwaysAllowed(lister.GetName())
	}
	if registry != nil && !config.DisableTools {
		transcript := &readTranscriptTool{rendered: agent.renderedTranscript}
		registry.Register(transcript)
		registry.MarkAlwaysAllowed(transcript.GetName())
	}
	return agent
}

// ToolRegistry returns the agent's effective tools, including its private
// built-ins. Configured tools and policies are inherited from the caller.
func (a *Agent) ToolRegistry() *tools.ToolRegistry { return a.tools }

// projectionTools describes registered recall tools for durable transcript
// rendering, independently of capability filtering on a model request.
func (a *Agent) projectionTools() projectionTools {
	if a.tools != nil && !a.config.DisableTools {
		return projectionToolsFor(a.tools.All())
	}
	return projectionTools{}
}

// isRecallTool reports whether name is a registered recall tool.
func (a *Agent) isRecallTool(name string) bool {
	if a.tools == nil {
		return false
	}
	tool, ok := a.tools.Get(name)
	if !ok {
		return false
	}
	_, recall := tools.RecallStub(tool)
	return recall
}

// BuiltinToolNames lists the tools NewAgent registers privately on an agent.
// They are present whatever the caller's registry allows, so a tool allow
// list need not name them. view_image is not among them: it belongs to the
// registry's tool setup and is visible through its own built-in marker.
func BuiltinToolNames() []string {
	return []string{"list_artifacts", "read_artifact", "read_transcript"}
}

// Close releases only the agent's private registry. The caller still owns its
// registry, MCP clients, and artifact store. Do not call it during Run.
func (a *Agent) Close() error {
	if a.tools != nil {
		return a.tools.Close()
	}
	return nil
}

func (a *Agent) commitToolChanges() {
	// Inherited skill tools stage changes in the registry they were created
	// with. Publish those changes at the same iteration boundary as before.
	if a.sourceTools != nil {
		a.sourceTools.CommitPendingChanges()
	}
	if a.tools != nil {
		a.tools.CommitPendingChanges()
	}
}

func (a *Agent) setTranscript(history []messages.ChatMessage) {
	snapshot := append([]messages.ChatMessage(nil), history...)
	a.transcriptMu.Lock()
	a.transcript = snapshot
	a.transcriptText.Reset()
	a.transcriptRendered = 0
	a.transcriptIndex = 0
	a.transcriptMu.Unlock()
}

// Appending never changes the prefix already published to a reader. A spill
// replacement uses copy-on-write below before changing an existing message.
func (a *Agent) appendTranscript(history ...messages.ChatMessage) {
	a.transcriptMu.Lock()
	a.transcript = append(a.transcript, history...)
	a.transcriptMu.Unlock()
}

func (a *Agent) renderedTranscript() string {
	a.transcriptMu.Lock()
	defer a.transcriptMu.Unlock()
	a.transcriptIndex = appendTranscriptText(&a.transcriptText, a.transcript[a.transcriptRendered:], a.transcriptIndex, a.projectionTools().recall)
	a.transcriptRendered = len(a.transcript)
	return a.transcriptText.String()
}

func (a *Agent) applyTranscriptSpills(spills []toolResultSpill) {
	if len(spills) == 0 {
		return
	}
	a.transcriptMu.Lock()
	defer a.transcriptMu.Unlock()
	// Existing snapshots may still be held by concurrent readers.
	a.transcript = append([]messages.ChatMessage(nil), a.transcript...)
	a.applyDurableToolSpills(a.transcript, spills)
	a.transcriptText.Reset()
	a.transcriptRendered = 0
	a.transcriptIndex = 0
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

// runState is the state one Run owns across its iterations: the stable
// request shape for prompt-cache keys and the context projection cache.
// Requests never carry it; Run passes it to projection explicitly.
type runState struct {
	shape      *requestShapeCache
	projection *projectionCache
}

// agentRun is the loop-carried state of one Agent.Run. Its methods are the
// phases of an iteration, in order: buildRequest, project, stream, dispatch.
type agentRun struct {
	maxIterations int
	agent         *Agent
	cb            *AgentCallbacks
	caller        *CompletionRequest // as received; its history is already persisted
	// loopReq is the caller's request with skills resolved and the run's
	// replay cache attached; every iteration's request is a copy of it.
	loopReq CompletionRequest
	state   *runState
	// msgs is the owned history sent to the model; generated is what the
	// caller receives back, including admitted input.
	msgs      []messages.ChatMessage
	generated []messages.ChatMessage
	// reasoningNotices dedupes the repeated OpenRouter adaptation notice.
	reasoningNotices      map[string]bool
	nudgedResponseTool    bool
	responseToolCalled    bool
	responseToolSucceeded bool
	lastProjection        ProjectionStats
	promptCache           PromptCacheStats
	persisted             int
}

func (a *Agent) newRun(req *CompletionRequest, cb *AgentCallbacks) *agentRun {
	// Own the history's nested containers once. Projection can then share the
	// immutable prefix between iterations without exposing caller-owned slices.
	msgs := cloneMessages(req.Messages)
	// Resolve skills once before the loop to avoid double-augmentation
	// on subsequent iterations (where msgs[0] already has the augmented prompt).
	loopReq := *req
	if loopReq.Skills != nil && !loopReq.Skills.IsEmpty() {
		loopReq.Messages = msgs
		msgs = loopReq.ResolvedMessages()
		loopReq.Skills = nil
	}
	loopReq.Replay = &ReplayCache{}
	return &agentRun{
		agent: a, cb: cb, caller: req, loopReq: loopReq, msgs: msgs, maxIterations: a.config.MaxIterations,
		state:            &runState{shape: newRequestShapeCache(msgs), projection: &projectionCache{}},
		reasoningNotices: map[string]bool{},
	}
}

// response is the run's result after iterations model calls.
func (r *agentRun) response(message *messages.ChatMessage, iterations int) *AgentResponse {
	return &AgentResponse{
		Message: message, AllMessages: r.generated, IterationCount: iterations,
		Projection: r.lastProjection, PromptCache: r.promptCache,
	}
}

// append records generated messages in the model history, the caller's
// result, and the transcript served by read_transcript.
func (r *agentRun) append(msgs ...messages.ChatMessage) {
	r.msgs = append(r.msgs, msgs...)
	r.generated = append(r.generated, msgs...)
	r.agent.appendTranscript(msgs...)
}

func (r *agentRun) onError(err error) {
	if r.cb != nil && r.cb.OnError != nil {
		r.cb.OnError(err)
	}
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
func (a *Agent) Run(ctx context.Context, req *CompletionRequest, cb *AgentCallbacks) (result *AgentResponse, runErr error) {
	if a.config.RequireResponseToolSuccess && a.config.ResponseTool == "" {
		return nil, errors.New("a successful response tool requires a tool name")
	}
	r := a.newRun(req, cb)
	if limit, ok := ctx.Value(iterationLimitKey{}).(int); ok {
		r.maxIterations = min(r.maxIterations, limit)
	}
	defer func() {
		if result == nil || cb == nil || cb.Checkpoint == nil {
			return
		}
		// Cancellation cannot erase a completed tool result. The persistence
		// implementation must still enforce the session lease/generation fence.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := cb.Checkpoint(persistCtx, AgentCheckpoint{Generated: result.AllMessages, Iterations: result.IterationCount, Final: true, Err: runErr}); err != nil {
			runErr = errors.Join(runErr, err)
		} else {
			r.persisted = len(result.AllMessages)
		}
		result.PersistedMessages = r.persisted
	}()
	a.resetArtifactIndex(r.msgs)
	a.setTranscript(r.msgs)

	for iteration := 0; iteration < r.maxIterations; iteration++ {
		if a.config.RequireResponseToolSuccess {
			r.responseToolCalled, r.responseToolSucceeded = false, false
		}
		select {
		case <-ctx.Done():
			return r.response(nil, iteration), ctx.Err()
		default:
		}
		iterReq, admitted, err := r.buildRequest(ctx)
		if err != nil {
			return r.response(nil, iteration), err
		}
		newRefs, err := r.project(ctx, &iterReq, admitted, iteration)
		if err != nil {
			return r.response(nil, iteration), err
		}
		response, err := r.stream(ctx, &iterReq, iteration, newRefs)
		if err != nil {
			return r.response(nil, iteration+1), err
		}
		done, err := r.dispatch(ctx, response, iteration)
		if err != nil || done {
			return r.response(response, iteration+1), err
		}
	}

	// Stamp the last generated assistant message (the one whose tool calls
	// exhausted the budget) so callers that persist AllMessages record why the
	// turn ended. The stamp must land in generated itself — msgs holds
	// separate copies that are never returned.
	last := stampMaxIterations(r.generated)
	r.onError(ErrMaxIterations)
	// Return the partial response so the caller can save the history
	return r.response(last, r.maxIterations), ErrMaxIterations
}

// buildRequest admits staged input and prepares this iteration's request
// for its model. The admitted input is committed by project once the request
// is known to be sendable.
func (r *agentRun) buildRequest(ctx context.Context) (CompletionRequest, []messages.ChatMessage, error) {
	a := r.agent
	var admitted []messages.ChatMessage
	if r.cb != nil && r.cb.AdmitInput != nil {
		var err error
		admitted, err = r.cb.AdmitInput(ctx)
		if err != nil {
			return CompletionRequest{}, nil, err
		}
		for _, msg := range admitted {
			if msg.Role != messages.MessageRoleUser {
				return CompletionRequest{}, nil, errors.New("admitted input must be a user message")
			}
		}
	}
	iterReq := r.loopReq
	iterReq.Messages = r.msgs
	if len(admitted) > 0 {
		iterReq.Messages = append(cloneMessages(r.msgs), admitted...)
	}
	if a.config.DisableTools {
		iterReq.Tools = nil
	} else if len(a.requestTools) > 0 {
		iterReq.Tools = a.requestTools
	} else if a.tools != nil {
		iterReq.Tools = a.tools.All()
	}
	var err error
	iterReq.Messages, err = WithSandboxContext(iterReq.Messages, a.tools)
	if err != nil {
		return CompletionRequest{}, nil, err
	}
	prepared, notes, err := Prepare(ctx, a.client, &iterReq, a.config.RequireResponseToolSuccess || a.config.ResponseTool != "")
	if err != nil {
		return CompletionRequest{}, nil, err
	}
	iterReq = *prepared
	for _, note := range notes {
		if note.Feature == "reasoning" {
			if r.reasoningNotices[note.Message] {
				continue
			}
			r.reasoningNotices[note.Message] = true
		}
		if r.cb != nil && r.cb.OnAdaptation != nil {
			r.cb.OnAdaptation(note)
		}
	}
	// Preparation may have rewritten media to text and changed the system
	// prompts the prompt-cache key covers.
	r.state.projection.setOmitImages(iterReq.Capabilities != nil && omitsImages(*iterReq.Capabilities))
	r.state.shape.reseed(iterReq.Messages)
	r.state.shape.prepareTools(iterReq.Tools)
	return iterReq, admitted, nil
}

// project replaces the request history with its provider-visible projection,
// applies durable spills, gates the first request, checkpoints and commits the
// admitted input, and derives the prompt-cache key. It returns the artifact
// refs projection minted that the caller has not persisted yet.
func (r *agentRun) project(ctx context.Context, iterReq *CompletionRequest, admitted []messages.ChatMessage, iteration int) ([]artifacts.Ref, error) {
	a := r.agent
	projected, projection, err := projectCompletionRequest(ctx, iterReq, a.artifactStore, projectionToolsFor(iterReq.Tools), r.state)
	a.applyDurableToolSpills(r.msgs, projection.toolSpills)
	a.applyDurableToolSpills(r.generated, projection.toolSpills)
	a.applyTranscriptSpills(projection.toolSpills)
	if len(projection.toolSpills) != 0 {
		r.state.projection.invalidateMessages()
	}
	newRefs := unpersistedArtifactRefs(projection.artifactRefs, r.caller.Messages, r.generated)
	for _, ref := range projection.artifactRefs {
		a.indexArtifact(ref)
	}
	projection.artifactRefs = nil
	projection.toolSpills = nil
	r.lastProjection = projection
	if err != nil {
		r.onError(err)
		return nil, err
	}
	iterReq.Messages = projected
	if iteration == 0 && r.cb != nil && r.cb.BeforeFirstRequest != nil {
		// The request is known to be sendable; the caller may now commit
		// the input it staged, or decline before any provider tokens are spent.
		if err := r.cb.BeforeFirstRequest(r.lastProjection); err != nil {
			return nil, err
		}
		// The callback can update a caller-owned tool schema before this
		// request. Refresh the stable shape after that mutation boundary.
		r.state.shape.prepareTools(iterReq.Tools)
	}
	if r.cb != nil && r.cb.Checkpoint != nil {
		candidate := append(cloneMessages(r.generated), admitted...)
		if err := r.cb.Checkpoint(ctx, AgentCheckpoint{Generated: candidate, Iterations: iteration, Request: true}); err != nil {
			return nil, err
		}
		r.persisted = len(candidate)
	}
	r.append(admitted...)
	if iterReq.PromptCacheKey == "" {
		if key, keyErr := derivePromptCacheKey(iterReq, r.msgs, r.state.shape); keyErr == nil {
			iterReq.PromptCacheKey = key
		} else {
			slog.Debug("prompt_cache_key_omitted", "error", keyErr)
		}
	}
	if r.cb != nil && r.cb.OnRequestProjection != nil {
		r.cb.OnRequestProjection(iteration, r.lastProjection)
	}
	return newRefs, nil
}

// streamRetries bounds how often one iteration is re-sent after the provider
// stream dies. It matches the request-layer budget in llm/internal/httpx:
// that layer covers a connection that fails before the response body is
// handed over, this one covers a body that dies while being read.
const streamRetries = 2

// stream sends the request, accumulates the reply, records its usage, and
// appends it to the history. A transport failure that aborts the stream
// before any delta reached the callbacks is retried: nothing was shown and
// nothing was appended, so re-sending the identical request is invisible to
// the caller and to the model. Once deltas have been forwarded the error
// stands, because a frontend has already rendered them and the agent cannot
// take them back. Only the final failure reports through OnError.
func (r *agentRun) stream(ctx context.Context, iterReq *CompletionRequest, iteration int, newRefs []artifacts.Ref) (*messages.ChatMessage, error) {
	var response *messages.ChatMessage
	for attempt := 0; ; attempt++ {
		events := r.agent.client.ChatCompletionStream(ctx, iterReq, messages.NewStreamProcessor())
		reply, shown, err := r.agent.processEvents(ctx, events, r.cb)
		if err == nil {
			response = reply
			break
		}
		var reported streamEventError
		if !errors.As(err, &reported) {
			// Cancellation, or a stream that closed without a reply. Neither
			// has ever reported through OnError, and neither is re-sent.
			return nil, err
		}
		if shown || attempt >= streamRetries || ctx.Err() != nil || !transientStreamError(reported.err) {
			r.onError(reported.err)
			return nil, reported.err
		}
		slog.Debug("stream_retry", "iteration", iteration, "attempt", attempt+1, "error", reported.err)
		if waitErr := sleepBeforeStreamRetry(ctx, attempt); waitErr != nil {
			// Cancelled during the backoff: an interrupt, which the caller
			// shows as one, not the transport error that preceded it.
			return nil, waitErr
		}
	}
	if r.cb != nil && r.cb.OnIterationUsage != nil {
		r.cb.OnIterationUsage(iteration, response.GetInputTokens(), response.GetOutputTokens())
	}
	r.promptCache.ReadInputTokens += response.GetCacheReadInputTokens()
	r.promptCache.WriteInputTokens += response.GetCacheWriteInputTokens()

	// Projection can mint refs for older inline results without rewriting
	// the caller's history. Carry those refs in the generated transcript so
	// recall survives reloads and budgets that no longer need demotion.
	for _, ref := range newRefs {
		response.Parts = append(response.Parts, messages.ContentPart{Type: "artifact", Artifact: &ref})
	}
	// Ensure content is never null — some providers reject null content in history
	if response.Content == "" && len(response.ToolCalls) == 0 {
		response.Content = " "
	}
	r.append(*response)
	return response, nil
}

// dispatch runs the reply's tool batch or applies the response-tool policy,
// then lets the caller reopen a provisional final. It reports whether the run
// is complete; an error ends the run with this reply as its result.
func (r *agentRun) dispatch(ctx context.Context, response *messages.ChatMessage, iteration int) (bool, error) {
	a := r.agent
	// Classify the provider response before dispatch. All successful terminal
	// paths below converge on continuation, receipt validation, and OnComplete.
	// The streaming core already reports a reply with tool calls as a tool
	// turn; this keeps LLM implementations that bypass it to the same rule.
	if response.StopReason == messages.StopReasonEndTurn && len(response.ToolCalls) > 0 {
		response.StopReason = messages.StopReasonToolUse
	}
	runTools, err := responseNeedsTools(response)
	if err != nil {
		r.onError(err)
		return false, err
	}
	if runTools {
		for _, call := range response.ToolCalls {
			if a.config.ResponseTool != "" && call.Name == a.config.ResponseTool {
				r.responseToolCalled = true
			}
		}
		toolMsgs, toolErr := a.executeToolBatch(ctx, response.ToolCalls, r.generated, iteration+1, r.cb)
		r.append(toolMsgs...)
		if toolErr != nil {
			return false, toolErr
		}
		if r.cb != nil && r.cb.AfterToolBatch != nil {
			if err := r.cb.AfterToolBatch(ctx); err != nil {
				return false, err
			}
		}

		// A denied batch ends without a model denial replay. A response tool
		// ends with its structured result instead of an extra plain-text reply.
		denied := allDenied(toolMsgs)
		if denied && a.config.RequireResponseToolSuccess {
			return false, errors.New("tool batch denied before required response")
		}
		if !denied && !r.responseToolCalled {
			return false, nil
		}
		if r.responseToolCalled && a.config.RequireResponseToolSuccess {
			for _, result := range toolMsgs {
				if success, known := result.ToolSucceeded(); result.ToolName == a.config.ResponseTool && known && success {
					r.responseToolSucceeded = true
				}
			}
		}
	} else if response.StopReason != messages.StopReasonMaxTokens && a.config.ResponseTool != "" && !a.config.RequireResponseToolSuccess && !r.responseToolCalled && !r.nudgedResponseTool {
		// The legacy response-tool reminder is sent once, only after a
		// normal text completion. Receipt-based callers own their correction.
		r.nudgedResponseTool = true
		r.append(messages.ChatMessage{
			Role:     messages.MessageRoleUser,
			Content:  "Respond using the " + a.config.ResponseTool + " tool.",
			Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true},
		})
		return false, nil
	}

	// Outstanding coordination can reopen any provisional final, including
	// a denied batch or an unsuccessful response-tool receipt.
	if r.cb != nil && r.cb.ContinueAfterFinal != nil {
		input, err := r.cb.ContinueAfterFinal(ctx, response)
		if err != nil {
			return false, err
		}
		for _, msg := range input {
			if msg.Role != messages.MessageRoleUser || len(msg.ToolCalls) != 0 {
				return false, errors.New("continuation input must be user text")
			}
		}
		if len(input) > 0 {
			if iteration+1 >= r.maxIterations {
				// Keep the answer intact instead of appending input that no
				// remaining model call can answer.
				stampMaxIterations(r.generated)
				response.StopReason = messages.StopReasonMaxIterations
				r.onError(ErrMaxIterations)
				return false, ErrMaxIterations
			}
			r.append(input...)
			r.responseToolCalled = false
			return false, nil
		}
	}
	if a.config.RequireResponseToolSuccess && !r.responseToolSucceeded {
		return false, fmt.Errorf("missing successful %s result", a.config.ResponseTool)
	}
	if response.StopReason == messages.StopReasonMaxTokens {
		slog.Debug("response_truncated", "reason", "max_tokens")
	}
	if r.cb != nil && r.cb.OnComplete != nil {
		r.cb.OnComplete(response)
	}
	return true, nil
}

// responseNeedsTools distinguishes provider failures, tool batches, and finals.
// Explicit end_turn/max_tokens remain final even if they carry tool calls;
// receipt-based response tools normalize compatible end_turn responses first.
func responseNeedsTools(response *messages.ChatMessage) (bool, error) {
	switch response.StopReason {
	case messages.StopReasonEndTurn, messages.StopReasonMaxTokens:
		return false, nil
	case messages.StopReasonContentFilter:
		return false, errors.New("response blocked by content filter")
	case messages.StopReasonError:
		return false, errors.New("model produced malformed output")
	case messages.StopReasonToolUse:
		if len(response.ToolCalls) == 0 {
			return false, errors.New("model requested tool use without any tool calls")
		}
	}
	return len(response.ToolCalls) > 0, nil
}

// executeToolBatch validates the whole batch before starting any tool. Every
// failure returns a complete set of receipts, retaining results already earned.
func (a *Agent) executeToolBatch(ctx context.Context, calls []messages.ChatMessageToolCall, generated []messages.ChatMessage, iterations int, cb *AgentCallbacks) ([]messages.ChatMessage, error) {
	abort := func(err error) ([]messages.ChatMessage, error) {
		return completeAbortedToolBatch(calls, nil), err
	}
	// Providers can return calls even when no schemas were sent. DisableTools
	// is an execution bound and must be checked before callbacks or dispatch.
	if a.config.DisableTools {
		return abort(errors.New("tool execution is disabled"))
	}
	if len(calls) > 1 && a.tools != nil {
		for _, call := range calls {
			if tool, ok := a.tools.Get(call.Name); ok {
				if exclusive, ok := tool.(tools.ExclusiveTool); ok && exclusive.ExclusiveBatch() {
					return abort(fmt.Errorf("%s must be the only tool in its batch; no tools were started", call.Name))
				}
			}
		}
	}
	if cb != nil && cb.BeforeToolBatch != nil {
		if err := cb.BeforeToolBatch(ctx, calls); err != nil {
			return abort(err)
		}
	}
	if cb != nil && cb.JournalToolBatch != nil {
		if err := cb.JournalToolBatch(ctx, AgentCheckpoint{Generated: generated, Iterations: iterations}); err != nil {
			return abort(err)
		}
	}
	results, err := a.executeToolsParallel(ctx, calls, cb)
	if err != nil {
		results = completeAbortedToolBatch(calls, results)
	}
	a.commitToolChanges()
	a.indexArtifactMessages(results)
	return results, err
}

// stampMaxIterations marks the last generated assistant message as ended by
// the iteration budget and returns it.
func stampMaxIterations(generated []messages.ChatMessage) *messages.ChatMessage {
	for i := len(generated) - 1; i >= 0; i-- {
		if generated[i].Role == messages.MessageRoleAssistant {
			generated[i].StopReason = messages.StopReasonMaxIterations
			return &generated[i]
		}
	}
	return nil
}

// processEvents drains one provider stream. The second result reports whether
// a reasoning or content delta was handed to a callback: the caller may only
// re-send an attempt that showed nothing. On cancellation, buffered events from
// custom providers are drained while the producer shuts down.
func (a *Agent) processEvents(ctx context.Context, events <-chan *messages.StreamEvent, cb *AgentCallbacks) (*messages.ChatMessage, bool, error) {
	var response *messages.ChatMessage
	shown := false
	// Thinking time is the wall clock from the first reasoning delta to the
	// first content delta, or to the end of the response when the model went
	// straight from reasoning to tool calls. It lands on the message as
	// display-only metadata so a resumed transcript can show it.
	var thinkingStart, thinkingEnd time.Time

read:
	for {
		var event *messages.StreamEvent
		select {
		case <-ctx.Done():
			go drainAbandonedEvents(events)
			return nil, shown, ctx.Err()
		case next, ok := <-events:
			if !ok {
				break read
			}
			event = next
		}

		switch event.Type {
		case messages.EventTypeReasoning:
			if thinkingStart.IsZero() {
				thinkingStart = time.Now()
			}
			if cb != nil && cb.OnReasoning != nil {
				shown = true
				cb.OnReasoning(event.Content)
			}
		case messages.EventTypeContent:
			if !thinkingStart.IsZero() && thinkingEnd.IsZero() {
				thinkingEnd = time.Now()
			}
			if cb != nil && cb.OnContent != nil {
				shown = true
				cb.OnContent(event.Content)
			}
		case messages.EventTypeComplete:
			response = event.Message
			if response != nil && !thinkingStart.IsZero() {
				if thinkingEnd.IsZero() {
					thinkingEnd = time.Now()
				}
				response.SetThinkingDuration(thinkingEnd.Sub(thinkingStart))
			}
		case messages.EventTypeError:
			return nil, shown, streamEventError{event.Error}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, shown, err
	}
	if response == nil {
		return nil, shown, errors.New("no response received from LLM")
	}

	return response, shown, nil
}

// streamEventError marks an error the provider reported through the event
// stream, as opposed to cancellation or a stream that closed without a reply.
// Only these reach OnError, and only these are candidates for a re-send.
type streamEventError struct{ err error }

func (e streamEventError) Error() string { return e.err.Error() }
func (e streamEventError) Unwrap() error { return e.err }

// transientStreamError reports the transport failures worth re-sending: a
// connection the peer or a proxy dropped, and a body that ended early. A
// provider's own refusal (a rejected request, a content policy, an exhausted
// quota) is not transient and must surface on the first attempt, and neither
// is a request that never got a response: the request layer already retried
// that, and reports it as a *url.Error.
func transientStreamError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	// A read on the established connection that failed or timed out.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// net/http keeps its HTTP/2 error types unexported: a stream the peer
	// reset, or a connection it closed with GOAWAY, is known by its message.
	msg := err.Error()
	return strings.Contains(msg, "stream error:") || strings.Contains(msg, "GOAWAY") || strings.Contains(msg, "http2: client connection lost")
}

// sleepBeforeStreamRetry backs off 0.5s, doubling, like the request layer.
func sleepBeforeStreamRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(500 * time.Millisecond << attempt)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainAbandonedEvents consumes an abandoned event stream until it closes, so the
// processor and provider goroutines feeding it can finish instead of blocking
// on a channel nobody reads.
func drainAbandonedEvents(events <-chan *messages.StreamEvent) {
	for range events {
	}
}

// resolvedTool is a tool handle looked up before approval, so the approved
// call runs exactly the tool the caller resolved; the registry rejects a
// handle replaced or disallowed in the meantime rather than substituting.
type resolvedTool struct {
	tool    tools.Tool
	exists  bool
	allowed bool
}

// resolveTool looks a call's handle up once, before approval.
func (a *Agent) resolveTool(name string) resolvedTool {
	if a.tools == nil || a.config.DisableTools {
		return resolvedTool{}
	}
	var handle resolvedTool
	handle.tool, handle.exists, handle.allowed = a.tools.GetIfAllowed(name)
	return handle
}

func (a *Agent) resolveTools(calls []messages.ChatMessageToolCall) []resolvedTool {
	handles := make([]resolvedTool, len(calls))
	for i, tc := range calls {
		handles[i] = a.resolveTool(tc.Name)
	}
	return handles
}

// executeTool executes a single tool call and returns the result message. Tool
// execution failures remain durable tool outcomes, while artifact persistence
// failures abort the turn because a configured store is authoritative.
func (a *Agent) executeTool(ctx context.Context, tc messages.ChatMessageToolCall, handle resolvedTool, cb *AgentCallbacks) (messages.ChatMessage, error) {
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
	output, err := a.executeToolCall(execCtx, tc, args, handle)
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
	msg.SetToolDuration(duration)
	if cb != nil && cb.OnToolResult != nil {
		cb.OnToolResult(tc, msg.Clone())
	}
	return msg, nil
}

// executeToolCall performs the actual tool execution with the handle
// resolved before approval.
func (a *Agent) executeToolCall(ctx context.Context, tc messages.ChatMessageToolCall, args map[string]any, handle resolvedTool) (tools.ToolOutput, error) {
	if a.config.DisableTools {
		err := errors.New("tool execution is disabled")
		return tools.ToolOutput{Text: err.Error()}, err
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

	if !handle.exists {
		errMsg := fmt.Sprintf("Tool not found: %s", tc.Name)
		return tools.ToolOutput{Text: errMsg}, errors.New("tool not found: " + tc.Name)
	}
	if !handle.allowed {
		errMsg := fmt.Sprintf("Tool not allowed by active skill policy: %s", tc.Name)
		return tools.ToolOutput{Text: errMsg}, errors.New("tool not allowed: " + tc.Name)
	}

	execution, err := a.tools.ExecuteTool(ctx, handle.tool, args, a.config.ToolTimeout)
	output := execution.Output
	if !execution.Invoked {
		output.Text = err.Error()
		return output, err
	}
	if err != nil {
		if msg, ok := tools.FormatToolError(err); ok {
			output.Text = mergeToolErrorText(msg, output.Text)
			return output, err
		}
		if execution.ContextErr == context.DeadlineExceeded {
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
	if output.Data != nil {
		// Keep typed data durable without feeding a second copy into model
		// text. JSON round-tripping severs caller-owned mutable containers.
		data, err := json.Marshal(output.Data)
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("encode structured tool data: %w", err)
		}
		var value any
		if err = json.Unmarshal(data, &value); err != nil {
			return messages.ChatMessage{}, err
		}
		msg.Metadata = map[string]any{"tool_data": value}
	}
	var textArtifact *artifacts.Ref
	if !a.isRecallTool(tc.Name) && output.Text != "" && estimatedStringTokens(output.Text) > toolInlineTokenLimit && a.artifactStore != nil {
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
		} else if (strings.HasPrefix(strings.ToLower(media.MIMEType), "text/") || strings.EqualFold(media.MIMEType, "application/json")) && utf8.Valid(media.Data) {
			kind = artifacts.KindText
		}
		if a.artifactStore != nil {
			ref, err := a.artifactStore.Put(ctx, artifacts.Blob{Kind: kind, MIMEType: media.MIMEType, Name: media.Name, Reference: media.Reference, Data: media.Data})
			if err != nil {
				return messages.ChatMessage{}, fmt.Errorf("store %s artifact %q for tool %q: %w", kind, media.Name, tc.Name, err)
			}
			msg.Parts = append(msg.Parts, messages.ContentPart{Type: partType, Artifact: &ref, MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken})
			if textArtifact == nil {
				descriptor := artifactMediaDescriptor(ref)
				if kind == artifacts.KindText {
					descriptor = artifactReceipt(ref)
				}
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

// Only the caller's supplied history and AllMessages are durable. The loop's
// msgs may already contain a spill applied to a copy of an input message.
func unpersistedArtifactRefs(refs []artifacts.Ref, histories ...[]messages.ChatMessage) []artifacts.Ref {
	if len(refs) == 0 {
		return nil
	}
	durable := make(map[string]artifacts.Kind)
	for _, history := range histories {
		for _, msg := range history {
			if msg.Role == messages.MessageRoleInternal {
				continue
			}
			for _, part := range msg.Parts {
				if ref := part.Artifact; ref != nil && artifactKindPriority(ref.Kind) > artifactKindPriority(durable[ref.ID]) {
					durable[ref.ID] = ref.Kind
				}
			}
		}
	}
	var pending []artifacts.Ref
	for _, ref := range refs {
		if artifactKindPriority(ref.Kind) > artifactKindPriority(durable[ref.ID]) {
			pending = append(pending, ref)
			durable[ref.ID] = ref.Kind
		}
	}
	return pending
}

func (a *Agent) applyDurableToolSpills(history []messages.ChatMessage, spills []toolResultSpill) {
	for _, spill := range spills {
		for i := len(history) - 1; i >= 0; i-- {
			msg := &history[i]
			if msg.Role != messages.MessageRoleTool || msg.ToolCallID != spill.ToolCallID || msg.ToolName != spill.ToolName || msg.Content != spill.Content || textArtifactRef(*msg) != nil {
				continue
			}
			ref := spill.Ref
			msg.Parts = appendArtifactPart(msg.Parts, ref)
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

	// Resolve every handle before approval: the approved arguments run
	// against the tool the batch was resolved with, never a replacement.
	handles := a.resolveTools(toolCalls)

	// Determine which tools are approved
	approved := make([]bool, len(toolCalls))
	for i := range approved {
		approved[i] = true
	}
	if cb != nil && cb.ApproveToolCalls != nil {
		var err error
		approved, err = cb.ApproveToolCalls(ctx, toolCalls)
		if err != nil {
			return results, fmt.Errorf("approve tool calls: %w", err)
		}
		if len(approved) != len(toolCalls) {
			return results, fmt.Errorf("%w: got %d decisions for %d calls", ErrInvalidToolApproval, len(approved), len(toolCalls))
		}
	}

	if err := ctx.Err(); err != nil {
		return results, err
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

	// A single worker executes in request order, preserving dependencies
	// between calls without launching goroutines that race for the semaphore.
	parallelism := a.effectiveParallelism(len(approvedIndices))
	if parallelism == 1 {
		for _, idx := range approvedIndices {
			if err := ctx.Err(); err != nil {
				return results, err
			}
			result, err := a.executeTool(ctx, toolCalls[idx], handles[idx], cb)
			if err != nil {
				return results, err
			}
			results[idx] = result
		}
		return results, nil
	}

	g, ctx := errgroup.WithContext(ctx)

	// Semaphore for concurrency limiting
	sem := make(chan struct{}, parallelism)

	for _, idx := range approvedIndices {
		tc, handle := toolCalls[idx], handles[idx]
		g.Go(func() error {
			// Acquire semaphore (respects context cancellation)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return ctx.Err()
			}

			result, err := a.executeTool(ctx, tc, handle, cb)
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
// no remaining tool_calls is dropped unless it carries context artifact refs;
// those survive in a neutral message without denied reasoning/protocol state.
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
				refs := artifactRefsInMessages([]messages.ChatMessage{m})
				if len(refs) == 0 {
					continue
				}
				// Drop denied reasoning/protocol state, but keep context refs
				// minted during projection authorized in the saved transcript.
				m = messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: " "}
				for _, ref := range refs {
					m.Parts = append(m.Parts, messages.ContentPart{Type: "artifact", Artifact: &ref})
				}
			}
		}
		out = append(out, m)
	}
	return out
}
