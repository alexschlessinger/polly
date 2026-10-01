package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
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
	transcriptMu       sync.Mutex
	transcript         []messages.ChatMessage
	transcriptText     strings.Builder
	transcriptRendered int
	transcriptIndex    int

	// fronts is how far the last run's projection compacted and omitted, for
	// a conversation without a cache session id; the next run starts from it
	// when its history extends the last one's (see heldFronts).
	fronts projectionFronts
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
	// Builtins selects, by name, which private built-ins NewAgent installs
	// (see BuiltinToolNames). Nil installs every built-in the configuration
	// supports: read_transcript, plus list_artifacts and read_artifact when
	// ArtifactStore is set. A non-nil list installs only the named ones, so an
	// empty list installs none; names that are not built-ins are ignored. The
	// projection follows the installed set: its omission marker recommends
	// read_transcript and list_artifacts only when the model has them.
	// Omitting the artifact readers while ArtifactStore is set leaves the
	// model unable to open the receipts that projection writes for stored
	// tool output, so a host that omits them should serve that need itself.
	// DisableTools still overrides everything.
	Builtins []string
	// DisableTools is an absolute upper bound, including private built-ins.
	DisableTools bool
	// InlineToolResultTokens is the estimated size above which a tool's text
	// result is stored as an artifact the moment it is produced and shown to
	// the model as a bounded head/tail preview with a receipt, when
	// ArtifactStore is set. Results at or below it stay inline until the
	// projection demotes them under budget pressure. Recall tools are never
	// stored this way. Zero keeps the default of 10,000 tokens.
	InlineToolResultTokens int
	// Calibration keeps what providers report about each model's requests,
	// and every request is sized by it (see Calibration). Agents that share
	// one start where the last request to the model left off; nil gives the
	// agent one of its own.
	Calibration *Calibration
}

// inlineToolResultTokens is the effective InlineToolResultTokens.
func (c AgentConfig) inlineToolResultTokens() int {
	if c.InlineToolResultTokens > 0 {
		return c.InlineToolResultTokens
	}
	return toolInlineTokenLimit
}

// Names of the private built-ins NewAgent installs; see AgentConfig.Builtins.
const (
	BuiltinListArtifacts  = "list_artifacts"
	BuiltinReadArtifact   = "read_artifact"
	BuiltinReadTranscript = "read_transcript"
)

// BuiltinTools lists the built-ins NewAgent installs for this configuration,
// in BuiltinToolNames order: the catalog narrowed by Builtins, without the
// artifact readers when ArtifactStore is nil, and nothing under DisableTools.
// Selection validators pass it where they would pass BuiltinToolNames, so a
// selection can only name a built-in the agent will have.
func (c AgentConfig) BuiltinTools() []string {
	if c.DisableTools {
		return nil
	}
	installed := make([]string, 0, 3)
	for _, name := range BuiltinToolNames() {
		if c.installsBuiltin(name) {
			installed = append(installed, name)
		}
	}
	return installed
}

// installsBuiltin reports whether NewAgent installs the named built-in.
func (c AgentConfig) installsBuiltin(name string) bool {
	if c.DisableTools {
		return false
	}
	if (name == BuiltinListArtifacts || name == BuiltinReadArtifact) && c.ArtifactStore == nil {
		return false
	}
	return c.Builtins == nil || slices.Contains(c.Builtins, name)
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

	// OnCommentary receives interim assistant updates. It never receives final text.
	OnCommentary func(text messages.AssistantText, start bool)

	// OnContent is called when answer or unspecified content is streamed
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

	// OnModelRequest observes each provider attempt, including retries. Both
	// counters are zero-based within this Run.
	OnModelRequest func(iteration, attempt int)

	// OnStreamActivity observes provider data, including tool argument chunks
	// that do not produce text events. It runs on the provider goroutine, may
	// overlap the other callbacks, and must be fast and safe for concurrent use.
	OnStreamActivity func(iteration, attempt int)

	// OnIterationUsage reports completed provider usage once per iteration,
	// before its tools run. Counts are for this iteration, not cumulative.
	// Missing provider usage is reported as zero.
	OnIterationUsage func(iteration, inputTokens, outputTokens int)

	// OnUsageProgress reports provider usage for the in-flight iteration while
	// its response is still streaming, whenever the provider reports a change.
	// It may fire repeatedly with rising counts, including from an attempt
	// that is later re-sent; OnIterationUsage remains the authoritative close
	// of each iteration.
	OnUsageProgress func(usage UsageUpdate)

	// OnComplete is called when the final response is ready (no more tool calls)
	OnComplete func(response *messages.ChatMessage)

	// OnError is called when an error occurs. A provider stream that dies
	// before showing anything is re-sent (see streamRetries) and reports only
	// if the last attempt also fails.
	OnError func(err error)

	// OnResponseDropped reports a reply the agent streamed and then left out
	// of the run unrun, because the context budget had no room for its tool
	// batch. Only its usage is retained as an internal record; the next model
	// call answers without it. A host that showed the reply can mark it.
	OnResponseDropped func(response *messages.ChatMessage)
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
	AllMessages       []messages.ChatMessage // Generated messages, admitted input, and internal usage records
	IterationCount    int                    // Number of LLM calls made
	Projection        ProjectionStats        // Final provider-visible context projection
	PromptCache       PromptCacheStats       // Provider-reported cache use across all LLM calls
	PersistedMessages int                    // Prefix already acknowledged by Checkpoint.
}

// UsageUpdate is the provider usage reported so far for one iteration's
// response. Cache counts are subsets of InputTokens. ReportedCostUSD is the
// provider-billed cost, meaningful only when CostReported.
type UsageUpdate struct {
	InputTokens           int
	OutputTokens          int
	CacheReadInputTokens  int
	CacheWriteInputTokens int
	ReportedCostUSD       float64
	CostReported          bool
}

// TokenUsage separates total provider usage from peak per-request context usage.
// Counts are provider-reported; unreported usage contributes zero. Cache
// counts are subsets of TotalInput. ReportedCostUSD sums the cost providers
// billed and is zero when none reported one.
type TokenUsage struct {
	TotalInput      int
	TotalOutput     int
	PeakInput       int
	CacheRead       int
	CacheWrite      int
	ReportedCostUSD float64
}

// TokenUsage reports totals and peak input across this run's assistant
// messages and the internal usage records of dropped replies.
func (r *AgentResponse) TokenUsage() TokenUsage {
	var usage TokenUsage
	for _, m := range r.AllMessages {
		if m.Role != messages.MessageRoleAssistant && !m.IsUsageRecord() {
			continue
		}
		usage.TotalInput += m.GetInputTokens()
		usage.TotalOutput += m.GetOutputTokens()
		usage.PeakInput = max(usage.PeakInput, m.GetInputTokens())
		usage.CacheRead += m.GetCacheReadInputTokens()
		usage.CacheWrite += m.GetCacheWriteInputTokens()
		if cost, ok := m.GetReportedCost(); ok {
			usage.ReportedCostUSD += cost
		}
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

// LoginRequired reports whether a request for model would be refused for
// lack of a sign-in. A client without a provider router refuses nothing.
func (a *Agent) LoginRequired(model string) bool {
	if m := a.multiPass(); m != nil {
		return m.LoginRequired(model)
	}
	return false
}

// ProviderAccount describes the sign-in a provider is using, without
// revealing credential material.
func (a *Agent) ProviderAccount(provider string) (Account, bool) {
	if m := a.multiPass(); m != nil {
		return m.Account(provider)
	}
	return Account{}, false
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
	if config.Calibration == nil {
		config.Calibration = NewCalibration()
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
// Agent built-ins are private to this agent, and config.Builtins chooses
// which of them it gets. The caller retains ownership of registry and its
// configured tools; later registry changes remain visible. view_image is not
// an agent built-in: the registry's tool setup supplies it (natively through
// tools.WithNativeTools, or an independent toolset's own), and the agent
// never constructs or replaces it.
func NewAgent(client LLM, registry *tools.ToolRegistry, config AgentConfig) *Agent {
	agent := newAgent(client, registry, config)
	registry = agent.tools
	if registry == nil {
		return agent
	}
	install := func(tool tools.Tool) {
		if config.installsBuiltin(tool.GetName()) {
			registry.Register(tool)
			registry.MarkAlwaysAllowed(tool.GetName())
		}
	}
	if config.ArtifactStore != nil {
		install(&readArtifactTool{store: config.ArtifactStore, lookup: agent.lookupArtifact, open: config.OpenArtifact})
		install(&listArtifactsTool{list: agent.listArtifacts})
	}
	install(&readTranscriptTool{rendered: agent.renderedTranscript})
	return agent
}

// ToolRegistry returns the agent's effective tools, including its private
// built-ins. Configured tools and policies are inherited from the caller.
func (a *Agent) ToolRegistry() *tools.ToolRegistry { return a.tools }

// projectionTools describes registered recall tools for durable transcript
// rendering, independently of capability filtering on a model request.
func (a *Agent) projectionTools() projectionTools {
	if a.tools != nil && !a.config.DisableTools {
		return a.projectionToolsFor(a.tools.All())
	}
	return a.projectionToolsFor(nil)
}

// projectionToolsFor describes list for this agent's projection, with the
// agent's inline tool result limit.
func (a *Agent) projectionToolsFor(list []tools.Tool) projectionTools {
	p := projectionToolsFor(list)
	p.inlineTokens = a.config.inlineToolResultTokens()
	return p
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

// BuiltinToolNames is the catalog of tools NewAgent can register privately
// on an agent. Those it installs are present whatever the caller's registry
// allows, so a tool allow list need not name them; AgentConfig.BuiltinTools
// narrows the catalog to one configuration. view_image is not among them: it
// belongs to the registry's tool setup and is visible through its own
// built-in marker.
func BuiltinToolNames() []string {
	return []string{BuiltinListArtifacts, BuiltinReadArtifact, BuiltinReadTranscript}
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
	a.applyDurableToolSpills(a.transcript, spills)
	a.transcriptText.Reset()
	a.transcriptRendered = 0
	a.transcriptIndex = 0
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
	// budget is the current request's context budget as the caller set it,
	// in provider tokens, before the calibration sizes it in estimated ones.
	budget int
	// began is set once the first request has been cleared to send.
	began bool
	// next is the kind of request the next model call makes: a loop request,
	// unless a dropped batch or tools that outgrew the room made it a
	// finishing one. carried holds the refs a dropped reply carried, for the
	// finishing request to attach.
	next    requestKind
	carried []artifacts.Ref
	// batch is what the current tool batch was planned against; memo keeps
	// what the history costs its room checks.
	batch batchBudget
	memo  *roomMemo
	// sandbox is what the sandbox context this iteration's request carries
	// costs, resolved once per request.
	sandbox int
	agent   *Agent
	cb      *AgentCallbacks
	caller  *CompletionRequest // as received; its history is already persisted
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
		state:            &runState{shape: newRequestShapeCache(msgs), projection: &projectionCache{fronts: a.heldFronts(req)}},
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

// requestKind is what a model call asks for.
type requestKind int

const (
	// loopRequest is the next request with tools, admitting staged input.
	loopRequest requestKind = iota
	// retryRequest is a loop request sent in place of one the provider
	// rejected: it admits no input, so that it is no larger.
	retryRequest
	// finishingRequest finishes the run: the history in final form, and no
	// tools but the response tool.
	finishingRequest
)

// preparedRequest is a model call's request, prepared: its kind, the request
// as sent, and the artifact refs its projection minted that the caller has
// not persisted yet.
type preparedRequest struct {
	kind    requestKind
	req     CompletionRequest
	newRefs []artifacts.Ref
}

// reportPrepared reports a failure to prepare a request, unless it is the
// projection finding no room, which call answers or reports itself.
func (r *agentRun) reportPrepared(err error) {
	if !outOfRoom(err) {
		r.onError(err)
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
	// A run started by a tool of another agent's batch is sized to its own
	// budget, not to that batch's share of the other agent's next request.
	ctx = withoutBatchPlan(ctx)
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
		prepared, response, sent, err := r.call(ctx, iteration)
		if err != nil {
			if !sent {
				return r.response(nil, iteration), err
			}
			return r.response(nil, iteration+1), err
		}
		// The next call is a loop request, unless what this reply asks for
		// makes it a finishing one.
		r.next, r.carried = loopRequest, nil
		finished := prepared.kind == finishingRequest
		if finished {
			a.keepResponseToolCalls(response)
		}
		iterReq, newRefs := prepared.req, prepared.newRefs
		// The streaming core already reports a reply with tool calls as a
		// tool turn; an LLM implementation that bypasses it is held to the
		// same rule before the batch is planned.
		if response.StopReason == messages.StopReasonEndTurn && len(response.ToolCalls) > 0 {
			response.StopReason = messages.StopReasonToolUse
		}
		// What the provider reported for this request sizes the budget the
		// batch its reply asks for is planned against.
		r.learnUsage(response, &iterReq)
		r.batch = r.budgetFor(&iterReq)
		toolCtx := ctx
		// Only a batch dispatch will run is planned; a reply that ended for
		// another reason is dispatch's to classify.
		if runTools, _ := responseNeedsTools(response); runTools {
			plan := planBatch(r.batch, r.msgs, response)
			if plan.finish && !finished {
				// The budget has no room for this batch: drop it unrun, and
				// let the next model call answer from what the run has.
				r.dropResponse(response, newRefs)
				continue
			}
			// A finishing reply keeps only its calls to the response tool,
			// which end the run, so it is never dropped: dropping it would
			// only ask the same finishing request again.
			toolCtx = withBatchPlan(ctx, plan)
		}
		r.append(*response)
		done, err := r.dispatch(toolCtx, response, iteration)
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

// maxOverflowRetries bounds how many smaller requests replace one the
// provider rejected as too long for the context window, per model call.
const maxOverflowRetries = 2

// call makes the iteration's model call: it builds and projects the request,
// or the request that finishes the run, and streams the reply. sent reports
// whether a request reached the provider. A rejection as too long for the
// context window is learned from and answered with a smaller request, at most
// maxOverflowRetries times: first the request projected again at what the
// rejection showed, then the request that finishes the run without tools. A
// retry that would be no smaller is not sent; when not even the finishing
// request can be, the rejection is the call's error.
func (r *agentRun) call(ctx context.Context, iteration int) (prepared preparedRequest, response *messages.ChatMessage, sent bool, err error) {
	var rejected *ContextOverflowError
	var rejectedSize requestSize
	// minted holds the refs the rejected attempts' projections minted, which
	// the request sent in their place carries so that they are persisted.
	var minted []artifacts.Ref
	kind := r.next
	for retries := 0; ; {
		prepared, err = r.prepareCall(ctx, iteration, kind)
		for _, ref := range prepared.newRefs {
			minted = appendArtifactRef(minted, ref)
		}
		prepared.newRefs = minted
		if rejected != nil && (outOfRoom(err) || err == nil && !rejectedSize.shrunk(prepared.req, r.lastProjection)) {
			if kind == finishingRequest {
				r.onError(rejected)
				return prepared, nil, sent, rejected
			}
			kind = finishingRequest
			continue
		}
		if outOfRoom(err) && kind != finishingRequest && iteration > 0 {
			// The run's committed state was checked against the budget, but
			// the budget can shrink since: another agent sharing the
			// calibration may have raised the model's ratio. Finish from
			// what the run has rather than fail it.
			if r.cb != nil && r.cb.OnAdaptation != nil {
				r.cb.OnAdaptation(RequestAdaptation{Feature: "context", Message: "The context budget has no room for another request with tools; answering without them"})
			}
			kind = finishingRequest
			continue
		}
		if err != nil {
			if outOfRoom(err) {
				// No request answers it: this one was not sent in place of
				// a rejected one.
				r.onError(err)
			}
			return prepared, nil, sent, err
		}
		response, err = r.stream(ctx, &prepared.req, iteration, prepared.newRefs)
		sent = true
		var overflow *ContextOverflowError
		if !errors.As(err, &overflow) {
			return prepared, response, sent, err
		}
		r.agent.config.Calibration.learnOverflow(calibrationRoute(&prepared.req), overflow, r.lastProjection.RequestEstimatedTokens, prepared.req.MaxTokens)
		if retries == maxOverflowRetries {
			r.onError(overflow)
			return prepared, nil, sent, overflow
		}
		retries++
		if kind != finishingRequest {
			kind = retryRequest
		}
		if retries == maxOverflowRetries {
			kind = finishingRequest
		}
		rejected, rejectedSize = overflow, sizeOf(prepared.req, r.lastProjection, overflow)
		if r.cb != nil && r.cb.OnAdaptation != nil {
			r.cb.OnAdaptation(RequestAdaptation{Feature: "context", Count: overflow.Input, Message: overflowNote(overflow)})
		}
	}
}

// overflowNote tells the caller a rejection is being answered.
func overflowNote(overflow *ContextOverflowError) string {
	switch {
	case overflow.Input > 0 && overflow.Window > 0:
		return fmt.Sprintf("The provider counted %d tokens against a %d-token context window and rejected the request; sending a smaller one", overflow.Input, overflow.Window)
	case overflow.Window > 0:
		return fmt.Sprintf("The provider rejected the request as too long for its %d-token context window; sending a smaller one", overflow.Window)
	}
	return "The provider rejected the request as too long for the context window; sending a smaller one"
}

// requestSize is what a rejected request asked of the window: its estimate,
// and the output it reserved.
type requestSize struct{ estimated, reserve int }

func sizeOf(req CompletionRequest, stats ProjectionStats, overflow *ContextOverflowError) requestSize {
	reserve := req.MaxTokens
	if reserve <= 0 {
		reserve = overflow.Output
	}
	return requestSize{estimated: stats.RequestEstimatedTokens, reserve: reserve}
}

// shrunk reports whether req asks less of the window than the rejected
// request did.
func (s requestSize) shrunk(req CompletionRequest, stats ProjectionStats) bool {
	reserve := req.MaxTokens
	if reserve <= 0 {
		reserve = s.reserve
	}
	return stats.RequestEstimatedTokens < s.estimated || reserve < s.reserve
}

// outOfRoom reports whether err is the projection finding no room for a
// request in its budget.
func outOfRoom(err error) bool {
	var limit *ContextLimitError
	var schemas *schemaLimitError
	return errors.As(err, &limit) || errors.As(err, &schemas)
}

// prepareCall builds and projects the iteration's request of the given kind.
func (r *agentRun) prepareCall(ctx context.Context, iteration int, kind requestKind) (preparedRequest, error) {
	prepared := preparedRequest{kind: kind}
	if kind == finishingRequest {
		var err error
		prepared.req, prepared.newRefs, err = r.finishRequest(ctx)
		for _, ref := range r.carried {
			prepared.newRefs = appendArtifactRef(prepared.newRefs, ref)
		}
		if err != nil {
			return prepared, err
		}
		return prepared, r.commitRequest(ctx, &prepared.req, nil, iteration)
	}
	iterReq, admitted, err := r.buildRequest(ctx, kind == loopRequest)
	if err != nil {
		return prepared, err
	}
	newRefs, err := r.project(ctx, &iterReq, admitted, iteration)
	if err != nil {
		return prepared, err
	}
	prepared.req, prepared.newRefs = iterReq, newRefs
	if iteration == 0 && kind == loopRequest && r.cb != nil && r.cb.OnAdaptation != nil {
		if least := MinContextTokens(&iterReq); iterReq.MaxContextTokens > 0 && iterReq.MaxContextTokens < least {
			r.cb.OnAdaptation(RequestAdaptation{Feature: "context", Count: least, Message: fmt.Sprintf("Context budget %d is below %d, the least in which this request can read a page; tool results may be refused or dropped", iterReq.MaxContextTokens, least)})
		}
	}
	return prepared, nil
}

// buildRequest admits staged input, when admit allows, and prepares this
// iteration's request for its model. The admitted input is committed by
// project once the request is known to be sendable.
func (r *agentRun) buildRequest(ctx context.Context, admit bool) (CompletionRequest, []messages.ChatMessage, error) {
	a := r.agent
	var admitted []messages.ChatMessage
	if admit && r.cb != nil && r.cb.AdmitInput != nil {
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
	iterReq.Tools = r.loopTools()
	sandbox, err := a.tools.SandboxContext()
	if err != nil {
		return CompletionRequest{}, nil, err
	}
	iterReq.Messages = withSandboxContext(iterReq.Messages, sandbox)
	r.sandbox = 0
	if sandbox != "" {
		// Appended to the first system message, or a system message of its
		// own when there is none.
		r.sandbox = estimatedStringTokens("\n\n"+sandbox) + 4
	}
	prepared, notes, err := Prepare(ctx, a.client, &iterReq, a.config.RequireResponseToolSuccess || a.config.ResponseTool != "")
	if err != nil {
		return CompletionRequest{}, nil, err
	}
	iterReq = *prepared
	r.budget = iterReq.MaxContextTokens
	a.config.Calibration.size(&iterReq, r.budget)
	if len(admitted) > 0 {
		kept := len(iterReq.Messages) - len(admitted)
		budget := r.budgetFor(&iterReq)
		bounded, ok, err := r.boundInput(ctx, budget, admitted)
		if err != nil {
			return CompletionRequest{}, nil, err
		}
		if !ok {
			// No room even as receipts: the input stays staged for a later
			// boundary, and this request goes without it.
			bounded = nil
		}
		// Keep the bounded input durable in its original form, while the
		// provider sees the same capability adaptation as the rest of history.
		iterReq.Messages = append(iterReq.Messages[:kept:kept], budget.adapt(bounded)...)
		admitted = bounded
	}
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
	projected, projection, err := projectCompletionRequest(ctx, iterReq, a.artifactStore, a.projectionToolsFor(iterReq.Tools), r.state)
	a.keepFronts(r.caller, r.state.projection.fronts)
	a.applyDurableToolSpills(r.msgs, projection.toolSpills)
	a.applyDurableToolSpills(r.generated, projection.toolSpills)
	a.applyTranscriptSpills(projection.toolSpills)
	if len(projection.toolSpills) != 0 {
		// Spilled results were rewritten in place: what was measured of
		// them no longer holds.
		r.state.projection.invalidateMessages()
		r.memo = nil
	}
	newRefs, err := r.recordProjection(iterReq, projection, err)
	if err != nil {
		return nil, err
	}
	iterReq.Messages = projected
	if iteration == 0 && !r.began && r.cb != nil && r.cb.BeforeFirstRequest != nil {
		// The request is known to be sendable; the caller may now commit
		// the input it staged, or decline before any provider tokens are spent.
		if err := r.cb.BeforeFirstRequest(r.lastProjection); err != nil {
			return nil, err
		}
		r.began = true
		// The callback can update a caller-owned tool schema before this
		// request. Refresh the stable shape after that mutation boundary.
		r.state.shape.prepareTools(iterReq.Tools)
	}
	return newRefs, r.commitRequest(ctx, iterReq, admitted, iteration)
}

// commitRequest commits req, known to be sendable: it checkpoints the
// generated prefix with the admitted input, commits that input, derives the
// prompt-cache key and reports the projection.
func (r *agentRun) commitRequest(ctx context.Context, req *CompletionRequest, admitted []messages.ChatMessage, iteration int) error {
	if r.cb != nil && r.cb.Checkpoint != nil {
		candidate := append(cloneMessages(r.generated), admitted...)
		if err := r.cb.Checkpoint(ctx, AgentCheckpoint{Generated: candidate, Iterations: iteration, Request: true}); err != nil {
			return err
		}
		r.persisted = len(candidate)
	}
	r.append(admitted...)
	if req.PromptCacheKey == "" {
		if key, keyErr := derivePromptCacheKey(req, r.msgs, r.state.shape); keyErr == nil {
			req.PromptCacheKey = key
		} else {
			slog.Debug("prompt_cache_key_omitted", "error", keyErr)
		}
	}
	if r.cb != nil && r.cb.OnRequestProjection != nil {
		r.cb.OnRequestProjection(iteration, r.lastProjection)
	}
	return nil
}

// streamRetries bounds how often one iteration is re-sent after the provider
// stream dies. It matches the request-layer budget in llm/internal/httpx:
// that layer covers a connection that fails before the response body is
// handed over, this one covers a body that dies while being read.
const streamRetries = 2

// stream sends the request, accumulates the reply, and records its usage; the
// loop appends the reply once it has planned its batch. A transport failure that aborts the stream
// before any delta reached the callbacks is retried: nothing was shown and
// nothing was appended, so re-sending the identical request is invisible to
// the caller and to the model. Once deltas have been forwarded the error
// stands, because a frontend has already rendered them and the agent cannot
// take them back. Only the final failure reports through OnError.
func (r *agentRun) stream(ctx context.Context, iterReq *CompletionRequest, iteration int, newRefs []artifacts.Ref) (*messages.ChatMessage, error) {
	var response *messages.ChatMessage
	for attempt := 0; ; attempt++ {
		requestCtx := ctx
		if r.cb != nil {
			if r.cb.OnModelRequest != nil {
				r.cb.OnModelRequest(iteration, attempt)
			}
			if r.cb.OnStreamActivity != nil {
				requestCtx = streaming.WithActivityObserver(ctx, func() { r.cb.OnStreamActivity(iteration, attempt) })
			}
		}
		events := r.agent.client.ChatCompletionStream(requestCtx, iterReq, messages.NewStreamProcessor())
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
		if overflow, ok := contextOverflow(reported.err); ok && !shown {
			// The caller answers it with a smaller request, or reports it.
			return nil, overflow
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
	if response.Content == "" && len(response.ToolCalls) == 0 && len(response.TextBlocks) == 0 {
		response.Content = " "
	}
	return response, nil
}

// recordProjection keeps what req's projection produced: it indexes the
// artifacts the projection minted, sizes the projection in the provider's
// count, and reports a projection the budget refused. It returns the refs
// the caller has not persisted yet.
func (r *agentRun) recordProjection(req *CompletionRequest, projection ProjectionStats, err error) ([]artifacts.Ref, error) {
	a := r.agent
	newRefs := unpersistedArtifactRefs(projection.artifactRefs, r.caller.Messages, r.generated)
	for _, ref := range projection.artifactRefs {
		a.indexArtifact(ref)
	}
	projection.artifactRefs, projection.toolSpills = nil, nil
	projection.CalibratedTokens = int(math.Round(float64(projection.RequestEstimatedTokens) * a.config.Calibration.Ratio(calibrationRoute(req))))
	r.lastProjection = projection
	if err != nil {
		r.reportPrepared(err)
		return nil, err
	}
	return newRefs, nil
}

// finishRequest is the request that finishes the run from what it has, after
// a batch the budget had no room for was dropped: the history with the
// results the run holds inline written as text, earlier exchanges left to the
// projection to omit, and no tools but the response tool. When those results
// do not fit, it is the history in final form, which planBatch showed fits
// before the run committed its state.
func (r *agentRun) finishRequest(ctx context.Context) (CompletionRequest, []artifacts.Ref, error) {
	a := r.agent
	var recall recallStubs
	if a.tools != nil {
		recall = recallStubsFor(a.tools.All())
	} else {
		recall = projectionToolsFor(r.loopReq.Tools).recall
	}
	var req CompletionRequest
	var projected []messages.ChatMessage
	var projection ProjectionStats
	var err error
	forms := []func(messages.ChatMessage) messages.ChatMessage{
		func(msg messages.ChatMessage) messages.ChatMessage {
			return keptForm(restoredSpill(ctx, a.artifactStore, msg), recall)
		},
		func(msg messages.ChatMessage) messages.ChatMessage { return finalForm(msg, recall) },
	}
	for _, form := range forms {
		history := make([]messages.ChatMessage, len(r.msgs))
		for i, msg := range r.msgs {
			history[i] = form(msg)
		}
		req = r.loopReq
		req.Messages, req.Tools = history, a.finishTools()
		var prepared *CompletionRequest
		prepared, _, err = Prepare(ctx, a.client, &req, len(req.Tools) > 0)
		if err != nil {
			r.onError(err)
			return CompletionRequest{}, nil, err
		}
		req = *prepared
		r.budget = req.MaxContextTokens
		a.config.Calibration.size(&req, r.budget)
		// Tool exchanges are text here, so nothing spills; media the
		// projection stored is indexed and carried like project does.
		projected, projection, err = projectCompletionRequest(ctx, &req, a.artifactStore, projectionToolsFor(req.Tools), nil)
		if !outOfRoom(err) {
			break
		}
	}
	newRefs, err := r.recordProjection(&req, projection, err)
	if err != nil {
		return CompletionRequest{}, nil, err
	}
	req.Messages = projected
	return req, newRefs, nil
}

// finishTools is what a finishing request offers: the response tool alone,
// when the run must end in it.
func (a *Agent) finishTools() []tools.Tool {
	if a.config.ResponseTool == "" || a.tools == nil {
		return nil
	}
	if tool, ok := a.tools.Get(a.config.ResponseTool); ok {
		return []tools.Tool{tool}
	}
	return nil
}

// keepResponseToolCalls leaves a finishing reply only the calls it can make:
// to the response tool, the one tool a finishing request offers.
func (a *Agent) keepResponseToolCalls(response *messages.ChatMessage) {
	response.ToolCalls = slices.DeleteFunc(response.ToolCalls, func(call messages.ChatMessageToolCall) bool {
		return a.config.ResponseTool == "" || call.Name != a.config.ResponseTool
	})
	if len(response.ToolCalls) == 0 {
		if response.StopReason == messages.StopReasonToolUse {
			response.StopReason = messages.StopReasonEndTurn
		}
		if response.Content == "" {
			response.Content = " "
		}
	}
}

// dropResponse leaves a reply out of the run: its batch had no room. The
// next model call finishes from what the run has, carrying the refs the
// dropped reply would have. The reply was already streamed, so the host hears
// that it will not reach the transcript.
func (r *agentRun) dropResponse(response *messages.ChatMessage, refs []artifacts.Ref) {
	r.next = finishingRequest
	r.carried = append(r.carried, refs...)
	// Dropping an unrun batch does not undo the provider's billed request.
	// Internal records survive checkpoints but never enter provider replay.
	r.append(response.UsageRecord())
	if r.cb != nil && r.cb.OnResponseDropped != nil {
		r.cb.OnResponseDropped(response)
	}
	if r.cb != nil && r.cb.OnAdaptation != nil {
		r.cb.OnAdaptation(RequestAdaptation{Feature: "tools", Count: len(response.ToolCalls), Message: "Tool calls dropped: the context budget has no room for their results; answering without them"})
	}
}

// budgetFor is what a batch answering req is planned and fitted against.
func (r *agentRun) budgetFor(req *CompletionRequest) batchBudget {
	a := r.agent
	next := r.loopTools()
	b := batchBudget{
		req:          req,
		agentTools:   projectionToolsFor(next),
		nextSchemas:  r.state.shape.schemaTokens(next),
		hasStore:     a.artifactStore != nil,
		inlineTokens: a.config.inlineToolResultTokens(),
		responseTool: a.config.ResponseTool,
		imageRecall:  a.imageRecall,
		caps:         req.Capabilities,
	}
	b.finishSchemas = estimateToolSchemaTokens(a.finishTools())
	b.sandbox = r.sandbox
	if r.memo == nil {
		r.memo = &roomMemo{}
	}
	r.memo.reset(b.memoKey(next))
	b.memo = r.memo
	return b
}

// heldFronts is where the projection of req's conversation compacted and
// omitted up to. A conversation with a cache session id is known to every
// agent that shares the calibration, so a swarm member's next slice starts
// from it; one without is known to this agent alone.
func (a *Agent) heldFronts(req *CompletionRequest) projectionFronts {
	if req.CacheSessionID != "" {
		return a.config.Calibration.heldFronts(req.CacheSessionID)
	}
	return a.fronts
}

// keepFronts records where the projection of req's conversation has
// compacted and omitted up to, for the next run to start from.
func (a *Agent) keepFronts(req *CompletionRequest, fronts projectionFronts) {
	if req.CacheSessionID != "" {
		a.config.Calibration.keepFronts(req.CacheSessionID, fronts)
		return
	}
	a.fronts = fronts
}

// learnUsage keeps what the provider reported req's input costing against
// what the projection estimated, and sizes req's budget by it, so the batch
// the reply asks for is planned against what the provider counts.
func (r *agentRun) learnUsage(response *messages.ChatMessage, req *CompletionRequest) {
	calibration := r.agent.config.Calibration
	calibration.learnUsage(calibrationRoute(req), response.GetInputTokens(), r.lastProjection.RequestEstimatedTokens)
	calibration.size(req, r.budget)
}

// loopTools is what a request with tools offers, as buildRequest builds it.
func (r *agentRun) loopTools() []tools.Tool {
	a := r.agent
	switch {
	case a.config.DisableTools:
		return nil
	case len(a.requestTools) > 0:
		return a.requestTools
	case a.tools != nil:
		return a.tools.All()
	}
	return nil
}

// imageRecall reports whether call reads an image artifact back, which the
// next request carries whole.
func (a *Agent) imageRecall(call messages.ChatMessageToolCall) bool {
	if call.Name != BuiltinReadArtifact {
		return false
	}
	var args struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return false
	}
	ref, ok := a.lookupArtifact(strings.TrimSpace(args.ID))
	return ok && ref.Kind == artifacts.KindImage
}

// boundInput makes input committed between model calls fit b's room, storing
// messages as artifacts behind a receipt and as much head and tail as fits. It
// reports false when the input does not fit even as receipts, and an error
// when the store fails: a configured store is authoritative.
func (r *agentRun) boundInput(ctx context.Context, b batchBudget, input []messages.ChatMessage) ([]messages.ChatMessage, bool, error) {
	if b.req == nil || b.req.MaxContextTokens <= 0 || len(input) == 0 {
		return input, true, nil
	}
	if b.fits(r.msgs, input...) {
		return input, true, nil
	}
	// A receipt is of no use to a run that cannot read it back.
	store := r.agent.artifactStore
	if store == nil || !b.agentTools.artifactsReadable {
		return nil, false, nil
	}
	// Prospective receipts measure exactly as the stored ones will; the
	// input is stored once a bounded form is known to fit.
	refs := make([]artifacts.Ref, len(input))
	for i, msg := range input {
		refs[i] = prospectiveTextRef(msg.GetContent())
	}
	shown := func(preview int) []messages.ChatMessage {
		out := make([]messages.ChatMessage, len(input))
		for i, msg := range input {
			m := msg.Clone()
			m.Content = inputPreview(msg.GetContent(), refs[i], preview)
			m.Parts = append(slices.DeleteFunc(m.Parts, func(part messages.ContentPart) bool { return part.Type == "text" }), messages.ContentPart{Type: "artifact", Artifact: &refs[i]})
			out[i] = m
		}
		return out
	}
	if !b.fits(r.msgs, shown(0)...) {
		return nil, false, nil
	}
	// The largest preview that fits, found by halving.
	lo, hi := 0, 0
	for _, msg := range input {
		hi = max(hi, len(msg.GetContent()))
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if b.fits(r.msgs, shown(mid)...) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for i, msg := range input {
		ref, err := store.Put(ctx, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "input", Data: []byte(msg.GetContent())})
		if err != nil {
			return nil, false, fmt.Errorf("store input artifact: %w", err)
		}
		refs[i] = ref
	}
	bounded := shown(lo)
	r.agent.indexArtifactMessages(bounded)
	return bounded, true, nil
}

// inputPreview stands for input stored as an artifact: a receipt alone, or
// the stored-output preview with up to preview bytes of the head and tail.
func inputPreview(content string, ref artifacts.Ref, preview int) string {
	const noun = "message"
	if preview <= 0 {
		return artifactReceipt(noun, ref)
	}
	header, gap := previewFrame(noun, ref)
	data := []byte(content)
	return storedPreview(noun, ref, data, data, len(header)+len(gap)+preview)
}

// dispatch runs the reply's tool batch or applies the response-tool policy,
// then lets the caller reopen a provisional final. It reports whether the run
// is complete; an error ends the run with this reply as its result.
func (r *agentRun) dispatch(ctx context.Context, response *messages.ChatMessage, iteration int) (bool, error) {
	a := r.agent
	// Classify the provider response before dispatch. All successful terminal
	// paths below converge on continuation, receipt validation, and OnComplete.
	runTools, err := responseNeedsTools(response)
	if !runTools && len(response.ToolCalls) > 0 {
		// A reply that ended for another reason, cut off at its output
		// limit or blocked, keeps calls that will not run. Each is answered
		// as interrupted, so the transcript stays one a provider accepts.
		r.append(completeAbortedToolBatch(response.ToolCalls, nil)...)
	}
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
		// The batch may have changed the tools, as activating a skill does,
		// so the next request's schemas are measured again, for the results
		// and for any input committed before that request.
		r.batch = r.budgetFor(r.batch.req)
		toolMsgs, fits := fitResults(r.batch, r.msgs, toolMsgs)
		if !fits {
			// The tools now outgrow the room: the next model call finishes
			// without them.
			r.next = finishingRequest
		}
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
		nudge, ok, err := r.boundInput(ctx, r.batch, []messages.ChatMessage{{
			Role:     messages.MessageRoleUser,
			Content:  "Respond using the " + a.config.ResponseTool + " tool.",
			Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true},
		}})
		if err != nil {
			r.onError(err)
			return false, err
		}
		if ok {
			r.append(nudge...)
			return false, nil
		}
		// No room for the reminder: the text answer stands, as it does
		// once the reminder has been sent.
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
			bounded, ok, err := r.boundInput(ctx, r.batch, input)
			if err != nil {
				r.onError(err)
				return false, err
			}
			if !ok {
				// The answer stands; the budget has no room to reopen it.
				r.onError(ErrContextExhausted)
				return false, ErrContextExhausted
			}
			r.append(bounded...)
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

// executeToolBatch answers the calls the context budget refused, then
// validates the rest of the batch before starting any tool. Every failure
// returns a complete set of receipts, retaining results already earned.
func (a *Agent) executeToolBatch(ctx context.Context, calls []messages.ChatMessageToolCall, generated []messages.ChatMessage, iterations int, cb *AgentCallbacks) ([]messages.ChatMessage, error) {
	// Calls the budget refused never run: they are neither checked, journaled,
	// started nor put to approval, and each takes its refusal as its result,
	// reported ended once the rest of the batch has been reported started.
	plan, planned := batchPlanFrom(ctx)
	if !planned || len(plan.refused) == 0 {
		return a.executeRunnableBatch(ctx, calls, generated, iterations, cb, nil)
	}
	results := make([]messages.ChatMessage, len(calls))
	var runnable []messages.ChatMessageToolCall
	var positions []int
	refused := make([]int, 0, len(plan.refused))
	for i, tc := range calls {
		if !plan.refused[tc.ID] {
			runnable = append(runnable, tc)
			positions = append(positions, i)
			continue
		}
		results[i] = callResult(tc, refusalText(tc.Name))
		results[i].SetToolSucceeded(false)
		refused = append(refused, i)
	}
	ran, err := a.executeRunnableBatch(ctx, runnable, generated, iterations, cb, func() {
		for _, i := range refused {
			if cb != nil && cb.OnToolEnd != nil {
				cb.OnToolEnd(calls[i], results[i].Content, 0, ErrToolCallRefused)
			}
		}
	})
	for i, result := range ran {
		results[positions[i]] = result
	}
	return results, err
}

// executeRunnableBatch validates the calls of a batch that run, and runs
// them. started, if not nil, is called once they have been reported started.
func (a *Agent) executeRunnableBatch(ctx context.Context, runnable []messages.ChatMessageToolCall, generated []messages.ChatMessage, iterations int, cb *AgentCallbacks, started func()) ([]messages.ChatMessage, error) {
	if len(runnable) == 0 {
		// Nothing runs: there is no batch to check, journal or start.
		if started != nil {
			started()
		}
		return nil, nil
	}
	abort := func(err error) ([]messages.ChatMessage, error) {
		return completeAbortedToolBatch(runnable, nil), err
	}
	// Providers can return calls even when no schemas were sent. DisableTools
	// is an execution bound and must be checked before callbacks or dispatch.
	if a.config.DisableTools {
		return abort(errors.New("tool execution is disabled"))
	}
	if len(runnable) > 1 && a.tools != nil {
		for _, call := range runnable {
			if tool, ok := a.tools.Get(call.Name); ok {
				if exclusive, ok := tool.(tools.ExclusiveTool); ok && exclusive.ExclusiveBatch() {
					return abort(fmt.Errorf("%s must be the only tool in its batch; no tools were started", call.Name))
				}
			}
		}
	}
	if cb != nil && cb.BeforeToolBatch != nil {
		if err := cb.BeforeToolBatch(ctx, runnable); err != nil {
			return abort(err)
		}
	}
	if cb != nil && cb.JournalToolBatch != nil {
		if err := cb.JournalToolBatch(ctx, AgentCheckpoint{Generated: generated, Iterations: iterations}); err != nil {
			return abort(err)
		}
	}
	// Fire callback once with all tools before parallel execution.
	if cb != nil && cb.OnToolStart != nil {
		cb.OnToolStart(runnable)
	}
	if started != nil {
		started()
	}
	results, err := a.executeToolsParallel(ctx, runnable, cb)
	if err != nil {
		results = completeAbortedToolBatch(runnable, results)
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
		case messages.EventTypeCommentary:
			if !thinkingStart.IsZero() && thinkingEnd.IsZero() {
				thinkingEnd = time.Now()
			}
			if cb != nil && cb.OnCommentary != nil && event.Text != nil {
				shown = true
				cb.OnCommentary(*event.Text, event.TextStart)
			}
		case messages.EventTypeContent:
			if !thinkingStart.IsZero() && thinkingEnd.IsZero() {
				thinkingEnd = time.Now()
			}
			if cb != nil && cb.OnContent != nil {
				shown = true
				cb.OnContent(event.Content)
			}
		case messages.EventTypeUsage:
			if cb != nil && cb.OnUsageProgress != nil {
				cb.OnUsageProgress(UsageUpdate{
					InputTokens:           event.InputTokens,
					OutputTokens:          event.OutputTokens,
					CacheReadInputTokens:  event.CacheReadTokens,
					CacheWriteInputTokens: event.CacheWriteTokens,
					ReportedCostUSD:       event.CostUSD,
					CostReported:          event.CostReported,
				})
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
			// Name the fault's position: some providers replay a failed
			// call's arguments as an empty object, so the message is all
			// the model gets to see where its JSON broke.
			err = schema.LocateJSONError(tc.Arguments, err)
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
	output.Text = boundPage(ctx, output.Text, a.isRecallTool(tc.Name))
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
	if !a.isRecallTool(tc.Name) && output.Text != "" && estimatedStringTokens(output.Text) > a.config.inlineToolResultTokens() && a.artifactStore != nil {
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
					descriptor = artifactReceipt("tool output", ref)
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

// allDenied reports whether the user denied every call of the batch put to
// approval: every message in toolMsgs is a denial stub, or a budget refusal.
func allDenied(toolMsgs []messages.ChatMessage) bool {
	denied := false
	for _, m := range toolMsgs {
		switch {
		case m.Content == ToolDeniedContent:
			denied = true
		case IsToolRefusal(m):
			// The context budget refused it before approval: it was never
			// put to the user, so it neither denies nor continues the batch.
		default:
			return false
		}
	}
	return denied
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
