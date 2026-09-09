# Pollytool as a Library

Pollytool's CLI is a thin layer over Go packages you can use directly: one
streaming interface over seven LLM providers, plus tools, sandboxing,
skills, sessions, and structured output.

**Contents:** [Quick Start](#quick-start) · [Helpers](#helpers) ·
[Core Types](#core-types) · [Providers](#providers) · [Tools](#tools) ·
[Shell Tools](#shell-tools) · [MCP Servers](#mcp-servers) ·
[Skills](#skills) · [Sessions](#sessions) ·
[Structured Output](#structured-output) · [Error Handling](#error-handling) ·
[Thread Safety](#thread-safety)

## Quick Start

```bash
go get github.com/alexschlessinger/pollytool
export POLLYTOOL_OPENAIKEY=...   # also POLLYTOOL_ANTHROPICKEY, POLLYTOOL_GEMINIKEY,
                                 # POLLYTOOL_OLLAMAKEY, POLLYTOOL_HUGGINGFACEKEY
```

```go
import "github.com/alexschlessinger/pollytool/llm"

// One-shot: model, prompt, token budget
joke, err := llm.QuickComplete(ctx, "openai/gpt-5.4", "Tell me a joke", 500)

// Streaming
err = llm.StreamComplete(ctx, "openai/gpt-5.4", "Write a short story", 500, func(chunk string) {
    fmt.Print(chunk)
})
```

`llm.GetDefaultClient()` reads the `POLLYTOOL_*KEY` variables above. For
DeepSeek and OpenRouter, pass `deepseek` / `openrouter` keys to
`llm.NewMultiPass` yourself.

## Helpers

The one-liners build a fresh router from the environment on every call,
which is fine for scripts. For many calls, create one client with
`llm.GetDefaultClient()` (or `llm.NewMultiPass`) and reuse it.

### Conversation with history

```go
history := []messages.ChatMessage{
    {Role: messages.MessageRoleSystem, Content: "You are helpful"},
    {Role: messages.MessageRoleUser, Content: "Hi"},
    {Role: messages.MessageRoleAssistant, Content: "Hello! How can I help?"},
}
reply, err := llm.ChatWithHistory(ctx, "openai/gpt-5.4", history, "What did I just say?", 1000)
fmt.Println(reply.Content)
```

### Structured output, the easy way

`llm.SchemaFor` reflects a JSON schema from a struct; `StructuredComplete`
unmarshals the answer back into it:

```go
type UserInfo struct {
    Name  string `json:"name"`
    Age   int    `json:"age,omitempty"`
    Email string `json:"email"`
}

var user UserInfo
err := llm.StructuredComplete(ctx, "openai/gpt-5.4",
    "Extract: John Doe, 30, john@example.com",
    llm.SchemaFor(UserInfo{}), 500, &user)
```

### The builder

```go
client := llm.GetDefaultClient()

result, err := llm.NewCompletionBuilder("openai/gpt-5.4").
    WithSystemPrompt("You are a helpful assistant").
    WithUserMessage("Tell me about Go").
    WithTemperature(0.8).
    WithMaxTokens(500).
    Execute(ctx, client)

// Streaming variant
err = llm.NewCompletionBuilder("openai/gpt-5.4").
    WithUserMessage("Write a haiku").
    ExecuteStreaming(ctx, client, func(chunk string) { fmt.Print(chunk) })

// Tool loop: executes each call the model makes, feeds results back, and
// returns the final answer. Capped at 1024 rounds; hitting the cap returns
// the last response together with ErrMaxIterations.
registry := tools.NewToolRegistry([]tools.Tool{&WeatherTool{}})
response, err := llm.NewCompletionBuilder("openai/gpt-5.4").
    WithUserMessage("What's the weather in NYC?").
    ExecuteWithTools(ctx, client, registry)

// Skills work on the builder too: .WithSkills(catalog)
```

## Core Types

Every provider implements one method:

```go
type LLM interface {
    ChatCompletionStream(context.Context, *CompletionRequest, EventStreamProcessor) <-chan *messages.StreamEvent
}
```

The processor turns raw chunks into stream events;
`messages.NewStreamProcessor()` is the standard implementation.

```go
type CompletionRequest struct {
    APIKey   string
    BaseURL  string        // Custom endpoint (OpenAI-compatible providers)
    Timeout  time.Duration // Stream stall budget: cancel after this much silence (0 disables)
    Deadline time.Duration // Hard per-call ceiling (0 = none)

    // nil means "don't send temperature" — required for reasoning models,
    // which reject the parameter. Use llm.Float32Ptr(0.7) to set it.
    Temperature *float32

    Model            string
    MaxTokens        int
    MaxContextTokens int             // Agent's estimated input budget (0 = unlimited)
    Messages         []messages.ChatMessage
    Tools            []tools.Tool
    ResponseSchema   *Schema         // JSON schema for structured output
    ThinkingEffort   ThinkingEffort  // llm.EffortOff(), EffortLevel(LevelHigh), EffortBudget(n), EffortDynamic()
    Stream           *bool           // nil = streaming (default), false = non-streaming
    Skills           *skills.Catalog // Injects the skill prompt into the system prompt
}

type ChatMessage struct {
    Role       string                // "system", "user", "assistant", "tool", "internal"
    Content    string
    Parts      []ContentPart         // Multimodal content (images, files)
    ToolCalls  []ChatMessageToolCall // Tool calls made by the assistant
    ToolCallID string                // Set on tool-role replies
    ToolName   string                // Tool name on tool-role replies
    Reasoning  string                // Model reasoning, when the provider exposes it
    Metadata   map[string]any        // Token counts, error flags, etc.
    StopReason StopReason
}

type ChatMessageToolCall struct {
    ID        string
    Name      string
    Arguments string // JSON-encoded
}
```

Roles are the constants `messages.MessageRoleSystem`, `MessageRoleUser`,
`MessageRoleAssistant`, `MessageRoleTool`, and `MessageRoleInternal` (app
state; filter it before sending upstream). `messages.User("hello")` builds a
one-message user history.

`AgentCallbacks.BeforeFirstRequest` runs once per `Run`, after the initial
projection succeeds and before the first provider call, with that projection's
statistics. Persist a new user message there: a request that cannot be sent (a
prompt that does not fit after clamping and deterministic reductions, a
selected image that cannot be read) returns from `Run` before that point with
nothing generated, and an error returned from the callback aborts the run
before any provider call. Set `MaxContextTokens` to the effective input budget
before running.

Two optional, nil-safe callbacks expose live accounting without inspecting
rendered output:

```go
OnRequestProjection func(iteration int, stats ProjectionStats)
OnIterationUsage    func(iteration, inputTokens, outputTokens int)
```

`OnRequestProjection` runs after each successful projection and before its
provider request. On the first iteration it follows the successful
`BeforeFirstRequest` persistence gate; a veto or projection failure emits no
request callback. `OnIterationUsage` runs after a provider iteration completes,
before its tools execute, with measured usage (zero when unavailable).
Iterations are zero-based within each `Run`, including subsequent runs of the
same agent. Store usage by iteration and replace repeated samples when
reconciling; the CLI's turn totals use peak input and summed output tokens.
Context usage follows the latest request's projection until that same request
reports measured input, even if projections shrink between iterations.

Persist `AgentResponse.AllMessages` with their content parts intact, including
on partial runs. Generated assistant messages may carry artifact references
created while compacting older tool results. Those references keep the stored
outputs discoverable after a reload or context-budget increase; they are
removed from provider-visible messages during projection.

`ChatCompletionStream` sends events on a channel as the response arrives:

```go
type StreamEvent struct {
    Type     StreamEventType
    Content  string          // Incremental text (content and reasoning chunks)
    ToolCall *tools.ToolCall // For tool_call events
    Message  *ChatMessage    // For the final complete event
    Error    error           // For error events
}

const (
    EventTypeContent   StreamEventType = "content"
    EventTypeReasoning StreamEventType = "reasoning"
    EventTypeToolCall  StreamEventType = "tool_call"
    EventTypeComplete  StreamEventType = "complete"  // Final assembled message
    EventTypeError     StreamEventType = "error"
)
```

## Providers

`MultiPass` routes on a `provider/model` prefix — `openai/gpt-5.4`,
`anthropic/claude-opus-4-7`, `gemini/gemini-3.1-pro-preview`,
`ollama/gpt-oss`, `huggingface/...`, `deepseek/...`, `openrouter/...`. It is
stateless and constructs provider clients per call.

```go
multipass := llm.NewMultiPass(map[string]string{
    "openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
    "anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
})
// or llm.GetDefaultClient() to load every key from the env

req := &llm.CompletionRequest{
    Model:       "anthropic/claude-opus-4-7",
    Messages:    messages.User("Hello, how are you?"),
    Temperature: llm.Float32Ptr(0.7),
    MaxTokens:   1000,
    Timeout:     30 * time.Second,
}

for event := range multipass.ChatCompletionStream(ctx, req, messages.NewStreamProcessor()) {
    switch event.Type {
    case messages.EventTypeContent:
        fmt.Print(event.Content)
    case messages.EventTypeComplete:
        fmt.Printf("\nComplete: %+v\n", event.Message)
    case messages.EventTypeError:
        fmt.Printf("Error: %v\n", event.Error)
    }
}
```

Direct provider clients skip the router and take bare model names
(`"gpt-5.4"`, not `"openai/gpt-5.4"`):

```go
openai := llm.NewOpenAIClient(apiKey, "")                   // second arg = optional base URL
anthropic := llm.NewAnthropicClient(apiKey)
gemini, err := llm.NewGeminiClient(apiKey)
ollama := llm.NewOllamaClient("http://localhost:11434", "") // baseURL first; key optional
```

## Tools

```go
type Tool interface {
    GetSchema() *schema.ToolSchema
    Execute(ctx context.Context, args map[string]any) (string, error)

    GetName() string   // Namespaced name, e.g. "script__toolname"
    GetType() string   // "shell", "mcp", or "native"
    GetSource() string // Where it came from, e.g. "/path/to/script.sh"
}
```

Build schemas with the `schema` package: `schema.Tool` assembles an object
schema from `schema.Params`, with helpers `schema.S` (string), `Int`,
`Bool`, `Enum`, and `Array`. `schema.ToolSchemaFromJSON` parses existing
schema JSON.

```go
type WeatherTool struct{}

func (w *WeatherTool) GetSchema() *schema.ToolSchema {
    return schema.Tool("get_weather", "Get the current weather for a location",
        schema.Params{
            "location": schema.S("The city and state, e.g. San Francisco, CA"),
        },
        "location", // required
    )
}

func (w *WeatherTool) Execute(ctx context.Context, args map[string]any) (string, error) {
    location, ok := args["location"].(string)
    if !ok {
        return "", fmt.Errorf("location is required")
    }
    return fmt.Sprintf("The weather in %s is sunny and 72°F", location), nil
}

func (w *WeatherTool) GetName() string   { return "get_weather" }
func (w *WeatherTool) GetType() string   { return "native" }
func (w *WeatherTool) GetSource() string { return "builtin" }
```

### Running the tool loop yourself

The builder's `ExecuteWithTools` is the easy path. To own the loop (logging,
approval gates), make a *new* `ChatCompletionStream` call per round —
`for range` latches onto one channel, so you need an outer loop:

```go
registry := tools.NewToolRegistry([]tools.Tool{&WeatherTool{}})
processor := messages.NewStreamProcessor()
history := messages.User("What's the weather in San Francisco?")

for {
    req := &llm.CompletionRequest{Model: "openai/gpt-5.4", Messages: history, Tools: registry.All()}

    var final *messages.ChatMessage
    for event := range client.ChatCompletionStream(ctx, req, processor) {
        switch event.Type {
        case messages.EventTypeContent:
            fmt.Print(event.Content)
        case messages.EventTypeComplete:
            final = event.Message
        case messages.EventTypeError:
            log.Fatal(event.Error)
        }
    }
    if final == nil || len(final.ToolCalls) == 0 {
        break // a normal answer — done
    }

    // The assistant turn carrying ToolCalls goes into history exactly once,
    // before the tool-role replies. Providers reject any other order.
    history = append(history, *final)
    for _, call := range final.ToolCalls {
        var args map[string]any
        if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
            log.Fatal(err)
        }
        tool, ok := registry.Get(call.Name)
        if !ok {
            log.Fatalf("model asked for unknown tool %q", call.Name)
        }
        result, err := tool.Execute(ctx, args)
        if err != nil {
            result = fmt.Sprintf("error: %v", err) // feed errors back to the model
        }
        history = append(history, messages.ChatMessage{
            Role: messages.MessageRoleTool, Content: result,
            ToolCallID: call.ID, ToolName: call.Name,
        })
    }
}
```

## Shell Tools

Loading the native `zvec_grep_search` tool requires `zg` on
`PATH` and a registry with a sandbox factory (or one that explicitly uses
`WithUnsafeNoSandbox`). It offers zg's own agent search request (`query`,
`queries`, `fts`, `vector`, `fuse`, globs, file types, symbol filters) and
creates and refreshes a local-model workspace index through sandboxed zg
subprocesses. `LoadToolAuto` returns `tools.ErrZvecGrepSearchUnavailable` when
zg is missing and does not register the tool. Exact lookups are the bash
tool's job (`grep`, `rg`). The library's default temp-only write policy does not
grant index writes to arbitrary workspaces. See [search behavior](README.md#built-in-tools).

Any executable that answers `--schema` and `--execute <json-args>` is a
tool; the protocol and a sample script are in
[README.md](README.md#shell-tools). Process-backed tools need an explicit
sandbox policy — a registry without one refuses to load them (it won't even
run `--schema`):

```go
registry := tools.NewToolRegistry(nil,
    tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()),
)
if _, err := tools.LoadShellToolsWithRegistry(registry, []string{"./uppercase.sh"}); err != nil {
    log.Printf("warning: %v", err)
}

response, err := llm.NewCompletionBuilder("openai/gpt-5.4").
    WithUserMessage("Uppercase 'hello world'").
    ExecuteWithTools(ctx, llm.GetDefaultClient(), registry)
```

### Sandboxing in the library

[SANDBOX.md](SANDBOX.md) is the policy reference — every `"sandbox"`
field, the merge rules, and platform behavior. The library-only corners:

- **Base config.** `sandbox.DefaultConfig()` is the base policy;
  `sandbox.ParsePreset("workspace+net+git")` builds the CLI-style presets.
- **Opting out.** `tools.WithUnsafeNoSandbox()` is the registry option that
  lets tool metadata declare `"sandbox": false` (the CLI's `--nosandbox`).
- **Wrapping commands yourself.** Wrap an `exec.Cmd` with
  `sandbox.WrapCmdManaged` (or `WrapCmdWithEnvManaged`) and call the
  returned idempotent cleanup after `Start`, `Run`, `Output`, or
  `CombinedOutput` returns; it closes only backend-owned descriptors and
  preserves caller-owned `ExtraFiles`. The legacy `Sandbox.Wrap`, `WrapCmd`,
  and `WrapCmdWithEnv` fail closed with `sandbox.ErrManagedWrapRequired` on
  built-in backends; custom sandbox implementations that don't opt into the
  managed capability keep the legacy behavior.

## MCP Servers

Servers are declared in Claude Desktop-format JSON
([example](README.md#mcp-servers)). A *server spec* names the file plus,
optionally, one server in it: `"mcp.json"` or `"mcp.json#filesystem"`.
`ToolRegistry.LoadMCPServer` applies the registry's sandbox policy to local
stdio servers and namespaces the tools it finds:

```go
// registry as in Shell Tools
result, err := registry.LoadMCPServer("./mcp.json#filesystem")
for _, server := range result.Servers {
    fmt.Printf("%s: %v\n", server.Name, server.ToolNames)
}
// registry.All() now includes the MCP tools
```

Without a registry, `tools.NewUnsafeMCPClient(spec)` connects with no
sandboxing (the name is the warning); its `ListTools()` result can be
handed to `NewToolRegistry`, and `Close()` shuts it down.

### Derived registries

`registry.Derive(opts...)` returns a registry that sees the parent's tools
through an allow-list and shares its MCP clients and sandbox policy, so a
narrower or separately governed tool set (a subagent's, say) does not start
the servers again:

```go
worker := registry.Derive(tools.AllowTools("read_file", "zvec_grep_search", "git__*"),
    tools.DenyTools("git__push"))
agent := llm.NewAgent(client, worker, llm.AgentConfig{})
defer agent.Close()  // releases the agent's private built-ins
defer worker.Close() // releases only what the worker loaded itself
```

A derived registry is a full registry of its own: tools it registers or
loads are private to it and shadow the parent's, its skill policy and
always-allowed set are its own (the allow-list bounds everything but those
built-ins), and a parent tool stays subject to the parent's policy too.
Closing the parent empties every registry derived from it.

## Skills

Skills are directories of model instructions activated on demand:

```go
// Expands ~, dedupes, validates. Empty input falls back to ~/.pollytool/skills.
catalog, err := skills.LoadCatalog([]string{"~/my-skills"})
if catalog == nil {
    return // no skills found
}

// registry as in Shell Tools
skillRuntime, err := tools.NewSkillRuntime(catalog, registry)

// Set Skills on a request and the skill prompt is injected automatically…
req := &llm.CompletionRequest{Model: "openai/gpt-5.4", Messages: messages.User("hi"), Skills: catalog}
// …or compose the system prompt yourself:
systemPrompt := catalog.RuntimeSystemPrompt("You are a helpful assistant")

// Activate from application code, and persist activations across runs
_, err = skillRuntime.Activate("code-reviewer")
saved := skillRuntime.ActivatedSkills()
err = skillRuntime.Restore(saved)
```

`llm.NewAgent` keeps `read_transcript`, `read_artifact`, `list_artifacts`, and
`view_image` in an agent-owned registry. Constructing an agent does not register
these tools in the caller's registry. Configured tools, sandbox policy, and
runtime updates remain inherited. Use `agent.ToolRegistry()` to inspect the
effective tool set, and `agent.Close()` when finished; closing an agent leaves
the caller's registry, MCP connections, and artifact store open. Each agent
supports one `Run` at a time; independent agents may share a configured registry.

For a child registry, `skillRuntime.Derive(registry.Derive(...))` inherits
active skills and their policy without reconnecting their MCP servers. The
parent owns those clients; the child's later activations remain private.

## Subagents

The `subagent` package gives a model the `spawn_agent` tool: a brief, an
optional label, a tool allow-list, and an optional model override.
Model calls inherit the host's iteration limit; the model-facing tool rejects
`max_iterations`. Trusted Go callers may set `Request.MaxIterations` explicitly.
What running the child means is the host's `Runner`; the
library's `AgentRunner` runs an in-memory `llm.Agent` over a derived view
of the parent's tools, with the brief as the
only user message after your base messages:

```go
registry := tools.NewToolRegistry([]tools.Tool{&WeatherTool{}})
base := llm.CompletionRequest{Model: "openai/gpt-5.4",
    Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "Be brief."}}}
registry.Register(subagent.NewTool(subagent.AgentRunner(client, registry, base, llm.AgentConfig{})))
registry.MarkAlwaysAllowed(subagent.ToolName)
```

The tool result is the child's final reply plus, when the runner gave it
one, its session name. A `background: true` call asks the runner to return
as soon as the child has started (`Result.Started`) and deliver the reply
later; `AgentRunner` has no way to deliver later and runs the child to
completion regardless. `subagent.WithCallbacks` gives `AgentRunner` a
factory returning the `llm.AgentCallbacks` for each child's request, to
stream its text, watch or approve its tool calls, or inject context values
its tools need; without it a child runs unobserved with every call
approved. `subagent.WithMaxConcurrent` bounds parallel children (default
32). A runner whose child outlives the call must return `Result.Done`,
including on cancellation or error. Close it only when the child actually
stops; its concurrency slot stays occupied until then. The tool is exempt from `AgentConfig.ToolTimeout`
through the `tools.UntimedTool` interface. The polly CLI uses the `swarm` runtime below. The standalone `AgentRunner`
retains the lightweight shared-registry behavior; constructing a swarm is optional.
`Result.Yielded` lets a runtime release a blocking parent while preserving its
child. `WithRuntimeScheduler` delegates slot ownership to that runtime.
The library's `AgentRunner` uses the base messages you supply; CLI coding
defaults and automatic `AGENTS.md` loading are not injected by the library.
`ChildRegistry` excludes `spawn_agent`, `swarm_*`, `workflow_*`, `list_agents`,
`send_message`, and `read_messages`, even when the parent registers them later.
Those coordination tools carry the parent's identity and cannot be inherited by
a lightweight child. Use the swarm runtime to bind a member's own identity.

## Swarms and workflows

The CLI/TUI uses one swarm runtime for model `spawn_agent` calls, typed `/spawn`,
and JavaScript `polly.agent`. Direct and scripted coordination share storage,
budgets, worktree policy, and execution. Standalone `llm.Agent` and the lightweight
`subagent.AgentRunner` remain available without constructing a swarm.

`swarm.New(swarm.Config{Store, Parent, Registry, Client, Request, Agent, Root})`
creates one parent's runtime. `Parent` must implement
`sessions.CoordinationSession`; SQLite memory and disk stores do. Close the
runtime before the parent session and registry. Disk storage is required for
cross-process recovery; the CLI supplies an automatic `Promote` callback.

Hosts with session-scoped tools can declare `MemberToolNames` and supply
`PrepareMember(ctx, session, registry)`. It runs on each execution slice with the
member's current lease; a nil registry means tools are disabled. Bind tools to
that session rather than capturing a parent session. Returned guidance is
request-only, omitted for structured output, and never enters saved history.
The CLI uses this hook to seed and edit child titles without changing handles.

Call `runtime.RegisterParentTools(registry)` and `runtime.BindParent(callbacks,
persistenceAllowed)` when running a parent model. `BindParent` adds safe mail
admission, progressive persistence, interrupted-tool journaling, and settlement.
Persist only `response.AllMessages[response.PersistedMessages:]` afterward.
`BeforeFirstRequest` retains its existing persistence veto. Legacy `llm.Agent`
callers without these callbacks still persist the whole response once.

`runtime.Agent(ctx, controllerID, swarm.AgentRequest{Task: brief, ReadOnly: true})`
uses the shared scheduler and returns `Value`, `Session`, `Context`, `Task`, and
usage. A nonempty controller reserves that member; ordinary hosts should use an
empty controller. `Session` continues an existing member and inherits its tool,
model, and filesystem authority. `Source`, `Snapshot`, and `Context` choose the
source for a **new** isolated checkout. `Tools: []string{}` disables all tools;
a nil slice inherits compatible parent tools. Logical executions default to
32 concurrent / 256 starts per run. Waits retain their execution ID and remaining
iteration budget. Runtime callbacks, instructions, limits, private filesystem
paths, and worktree directory are configurable through `swarm.Config`.
Use repository-relative paths in `Task` briefs. `Source` selects snapshot input,
not the member's working directory: tools and ordinary Git inspection run in
the assigned checkout. Parent files and Git writes stay denied. On macOS the
common Git read grant includes metadata-only traversal of ancestor directories,
without allowing their listings or file contents.
For history tasks, include a source commit ID in the brief: the member's `HEAD`
is a parentless snapshot, while `git log <source-commit>` reads repository history.
`UpdateDefaults(request, agentConfig, instructions)` safely refreshes the parent's
settings. Member identity, model, tool authority and files remain fixed; an
execution's iteration cap is captured at its start and survives waits/restarts.
`AgentRequest.MaxIterations` is a trusted Go host override; JavaScript cannot set
it. Iteration exhaustion returns `*swarm.IterationLimitError` (wrapping
`llm.ErrMaxIterations`) with the member/execution IDs and used/allowed counts,
retains partial results, and saves execution status `paused` with stop reason
`max_iterations`. `llm.IsIterationLimit(err)` excludes joined persistence or
provider failures from this recoverable classification.

Unstructured member finals without meaningful text or media receive one
corrective continuation within the same execution and iteration allowance.
The retry reservation persists across yields and recovery. A second blank final
returns `*swarm.EmptyResultError` wrapping `swarm.ErrEmptyResult`, with the member
and execution IDs, and saves a failed execution. Structured and response-tool
contracts retain their existing validation. Empty finals after denied tools or a
failed response tool fail immediately without another approval attempt. Text in
content parts is included in the returned value; media-only and successful
response-tool finals provide a reference to their saved member session.

`Resume(ctx, memberID, executionGrant)` preserves the remaining call allowance;
an exhausted one requires a trusted host to use
`ResumeWithIterations(ctx, memberID, additionalCalls)`. That grants calls to the
same logical execution, conversation, task and worktree without spending a start.
Positive `executionGrant` values extend the separate logical-start budget. Model
tools expose neither grant. Active workflow reservations must settle or be
canceled before a host takes over a member. A completed/failed execution starts
a new logical turn on resume, and launch refusals are returned synchronously.

The runtime exposes `State`, `CreateTask`, `Claim`, `Submit`, `Review`,
`UpdateTask`, `BlockTask`, `CancelTask`, `Send`, `Publish`, `Resume`, `ResumeWithIterations`, `StopMember`,
`Cleanup`, `Settle`, and lifecycle `OnEvent` callbacks. The Go host is trusted;
model-facing authority is bound in registered closures rather than supplied as a
caller ID. Task revisions and atomic transactions reject stale claims/submissions.
`Review` completes an accepted unchanged snapshot by comparing its immutable tree
with the task's original starting snapshot; changed candidates still require
integration. `Settle` also completes previously accepted unchanged submissions,
using retained provenance after context cleanup. `TaskStatus` provides display
text without changing machine statuses; the `swarm_tasks` tool includes this as
`displayStatus`, and `swarm_review` returns status plus any required next action.

Parent hosts use `PrepareIntegration(ctx, []TaskReference, drift)`,
`ReadIntegration`, `ReviseIntegration`, `RefreshIntegration`, `AcceptIntegration`,
and `ApplyIntegration`; `ReadTask` returns the current submitted task contract. `TaskReference` holds `Task` and the exact `Revision`.
`drift` defaults to `paths`; `tree` adds whole-tree equality. Candidates contain
ordered inputs, repair provenance, pending merges, structured conflicts, immutable
snapshots, acceptance and supersession links. `RefreshIntegration` returns
`IntegrationRefresh{IntegrationCandidate, Changed}`; a no-op retains the ID and
acceptance. Preparation never allocates a checkout. `ApplyIntegration` returns
an idempotent `ApplyRecord`; `ReconcileApply` observes uncertain writes without
replaying them. These are trusted host APIs; `swarm_integration` binds their
parent authority in the tool closure and is absent from child/bound registries.

`RunWorkflow(ctx, source, input)` runs a fresh Goja VM over the same runtime;
`StartWorkflow` returns a report ID for background execution. `CancelWorkflow`
cancels that attempt. Both launch modes share persistence, registration, member
reservation, and teardown. `RunWorkflow` honors caller cancellation and drains
host effects before returning; `StartWorkflow` detaches from caller cancellation.
Both stop on runtime shutdown. Saved reports include source, input, every operation intent,
results, failure details, and final output. Restarting a script is an explicit new
attempt; there is no persisted JavaScript heap or automatic effect replay.
Failed/interrupted reports block settlement until `AcknowledgeWorkflow` records
parent handling; this never accepts tasks or discards files.
Parent JavaScript uses `polly.integration.prepare/read/revise/refresh/accept/apply`
and `polly.tasks.read/review` over those same operations. `polly.release(context)`
removes only an inactive attempt-owned context whose contents are unchanged or
proven integrated, retaining snapshots and publications. A requested change does
not wake a reserved member; the script explicitly continues its session. Workflow
termination waits for host calls and saves late apply receipts before releasing
registries or reservations. Children and generic context tools gain no parent API.

The independent `workflow.Runner{Host, Config}` can be embedded over another
trusted host implementing `Call`; optional `Recorder.SaveWorkflow` supplies
persistence. See [WORKFLOWS.md](WORKFLOWS.md) for the JavaScript contract.

`tools.ExecutionContext` binds `Root`, `ReadOnly`, and a narrowed sandbox policy
to a fresh registry via `BindExecutionContext`. Custom Go tools implement
`ContextTool` to rebind or declare `ContextIndependentTool` when appropriate.
Local stdio MCP processes relaunch in that context; remote MCP servers must
explicitly declare `contextIndependent: true`. Media stays in `ToolOutput.Media`;
structured values stay in `ToolOutput.Data` and transcript `tool_data` metadata.
`tools.CommandError` represents an ordinary shell exit after target startup;
setup, cancellation, timeout, and policy-construction errors have distinct paths.

Schema v5 adds strict `swarm_records` (domain/key/JSON), `swarm_members`, and
`swarm_artifacts` tables. Coordination rows and transcript admission commit in one
lease-fenced transaction. Records and publication pins cascade with the parent,
including TTL expiry. Pinned child sessions do not independently expire while
retained by their parent. `sessions.DurableStore.Promote(ctx, path)` moves a memory
store into the disk store atomically without changing live handles, stable IDs,
cache identity, or artifact access. Existing unrelated disk sessions remain.

## Sessions

Sessions persist conversation history and artifacts in SQLite. Disk-backed
and in-memory stores share one implementation; only `StoreConfig` differs.

```go
// error checks elided
store, err := sessions.OpenStore(sessions.StoreConfig{
    Mode:           sessions.ModeDisk, // or ModeMemory with no Path
    Path:           "/path/to/polly.db",
    AutoSessionTTL: 7 * 24 * time.Hour,
})
defer store.Close()

session, err := store.Acquire(ctx, "my-session-id", sessions.AcquireOptions{})
defer session.Close() // releases this process's exclusive lease
sessionCtx := session.Context()

err = session.AddMessage(sessionCtx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "Hello!"})
history, err := session.GetHistory(sessionCtx) // feed to CompletionRequest.Messages
err = session.Clear(sessionCtx)

// Replace settings and clear transcript/artifacts in one transaction;
// the session's name and creation time are preserved.
metadata, err := session.GetMetadata(sessionCtx)
metadata.SystemPrompt = "A new system prompt"
err = session.Reset(sessionCtx, metadata)
```

- `ModeDisk` needs an explicit path, used literally — expand `~` yourself.
  The CLI uses `~/.pollytool/polly.db`.
- `AcquireOptions{Auto: true}` marks a newly created session for
  `AutoSessionTTL` retention. Named sessions don't expire by default, and
  reopening never changes a session's retention class.
- `Acquire` holds an exclusive lease. Close both session and store, and use
  `session.Context()` for work that should stop if the lease is lost. A
  competing owner receives `sessions.ErrSessionInUse`.
- `AcquireOptions{ExistingOnly: true}` refuses missing or expired sessions
  with `sessions.ErrSessionNotFound`, so navigation cannot create a new one.
- `ListSummaries` includes stable `ID` and `ParentID` alongside metadata,
  message count, and lease status. Family pickers can resolve ancestry without
  loading transcripts; `ParentID` is empty when the parent has been deleted.
- `Metadata.Title` is a descriptive label independent of the `Name` resume
  handle. `TitleSource` is `sessions.TitleSourceAgent` or `TitleSourceUser`;
  an absent title has no source. Existing sessions need no migration or
  backfill. A nonempty title with an unknown/missing source is user-owned.
- SQLite sessions implement the optional `sessions.TitleSession` capability:
  `SetTitle(ctx, title, source) (string, error)` returns the normalized title.
  It collapses whitespace, rejects control characters and empty titles, and
  allows up to 80 Unicode characters. Duplicate titles are allowed.
  Agent writes cannot replace user titles (`sessions.ErrTitleProtected`);
  invalid text/source returns `sessions.ErrInvalidTitle`. Manual writes claim
  ownership even when the title text is unchanged. Both require the lease and
  leave the handle, retention, TTL, and last-used time unchanged.
- `SetMetadata`, `Clear`, and `Reset` preserve the current title and ownership;
  use `TitleSession.SetTitle` to change them. `Rename` still changes the handle
  and its retention policy independently. `sessions.DisplayLabel(metadata)`
  chooses the title, then a child's task description, then the handle.
  The CLI registers `set_session_title` on each conversation's own runtime;
  the generic library agent does not inject naming policy or register it.
- `SQLiteStore.ReadView(ctx, sessions.ViewTarget{Name: name}, knownRevision)`
  reads a consistent snapshot without acquiring a lease or updating last-used
  time. Its `SessionView` includes stable `ID`, `ParentID`, `Revision`, metadata, history,
  lease status, and a read-only artifact store. Matching `knownRevision` sets
  `Unchanged` and omits history. Use `ViewTarget{ID: view.ID}` after renames, or
  `{Parent: parentName, SpawnCallID: callID}` to resolve an unambiguous child.
  `ParentID` is the stable ancestry link; `Metadata.Parent` is only a display
  name and may remain after parent deletion. Missing, expired, and deleted
  identities are refused. `sessions.ViewStore`
  is an optional capability alongside `SessionStore`; `sessions.ViewIdentity`
  exposes an acquired SQLite session's `ViewID()`.
- `AcquireOptions{ExpectedID: view.ID}` atomically verifies the viewed identity
  before taking a write lease, and implies `ExistingOnly`. A deleted name reused
  by a different session cannot receive a follow-up intended for the old view.
- A view's artifact store reads only that session's owned artifacts, remains
  usable after its writer closes, and rejects `Put`/`RemoveAll` with
  `sessions.ErrReadOnlyView`. Deletion/reset may remove those artifacts; store
  shutdown cancels readers. It does not retain or extend the session's lifetime.
- `session.ArtifactStore()` is scoped to the session; artifact bytes commit
  in the same database as the transcript.
- `AcquireOptions{Parent: name}` links a new session to the one whose agent
  spawns it. The link is by id, so `Metadata.Parent` reads as the parent's
  current name after renames, and `SetMetadata` ignores the field; a session
  whose parent was deleted keeps the last name it knew.
- Optional `Metadata.SpawnCallID` (`spawnCallID` in JSON) identifies the
  parent's originating `spawn_agent` call. `Metadata.SpawnOutcome`
  (`spawnOutcome`) records only that child's initial delegated run, using
  `ReportFinished`, `ReportFailed`, or `ReportCanceled`; empty means unknown
  or not yet settled. The CLI records these fields for Agents activity in
  the TUI and leaves the outcome unchanged on child follow-ups. They use the
  existing metadata JSON storage and require no schema migration.
  `SetMetadata` and `Reset` preserve each field once it has a nonempty value.
- `session.Report(ctx, sessions.Report{...})` posts a subagent's reply to
  its linked parent, and `store.PostReport(ctx, parent, report)` does the
  same for a named session, open or not. `session.TakeReports(ctx)` removes
  and returns the reports waiting for a leased session, oldest first. A
  report is deleted with its addressee and names its child as the child is
  called when read.
- `session.PeekReports(ctx)` reads reports without consuming them or taking
  a database write lock. `session.AddReportMessage(ctx, userMessage, reportIDs)`
  atomically persists the parent input and consumes those reports. The TUI
  uses this pair so a report queued before a tab closes remains recoverable.

## Structured Output

Besides reflecting one from a struct with `llm.SchemaFor` (strict;
required = non-omitempty fields; shown in
[Helpers](#structured-output-the-easy-way)), you can parse JSON or build
the raw map:

```go
schema := llm.SchemaFromJSON(`{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`)

schema = &llm.Schema{Raw: map[string]any{
    "type":       "object",
    "properties": map[string]any{"name": map[string]any{"type": "string"}},
    "required":   []string{"name"},
}}
```

Set one on a request via `ResponseSchema`, or let `llm.StructuredComplete`
handle the request and the unmarshaling together.

## Error Handling

Errors arrive as events on the same channel as everything else:

```go
for event := range client.ChatCompletionStream(ctx, req, processor) {
    if event.Type == messages.EventTypeError {
        if strings.Contains(event.Error.Error(), "rate limit") {
            time.Sleep(5 * time.Second) // back off and retry
        }
    }
}
```

## Thread Safety

- `MultiPass` is stateless and safe for concurrent use.
- `SQLiteStore` is safe for concurrent use. Different sessions can be active
  concurrently; acquiring the same session elsewhere waits for the lease
  and then returns `ErrSessionInUse`.
- `GetHistory` and `GetMetadata` return detached copies. Mutations are
  transactional, but read-modify-write sequences across calls need
  application-level coordination.
- Tool `Execute` implementations should be safe to call concurrently. After resolving
  and approving a tool, the agent loop and workflow host both use
  `ToolRegistry.ExecuteTool(ctx, tool, args, timeout)`. This shared boundary applies
  the timeout, holds the registry's execution gate, and preserves rich output.
  It returns the original error plus a `ToolExecution` containing output, whether
  invocation began, and the execution context's cancellation/timeout outcome.
  Each caller retains its own approval callbacks, error presentation, and artifact
  persistence. Swarm parent tools hold shared access and runtime apply takes
  exclusive access. `CoordinationTool` identifies trusted orchestration exemptions;
  `UntimedTool` alone does not. Custom hosts can use `ExecuteTool`, or
  `GuardExecution` with deferred release when implementing a different executor.
- `swarm.Config.ApplyTimeout` defaults to two minutes. `Runtime.Close` waits for
  active integration writes and their bounded outcome recording before the host
  closes its session. A confirmed apply is idempotent; uncertain writes are reconciled
  from persisted path states without automatically repeating the patch.
