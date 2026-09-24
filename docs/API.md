# Go API

Polly's streaming clients, agent loop, tools, and sessions are all ordinary Go
packages, so you can embed any of them in your own program. If you're after the
terminal app instead, start with the [CLI guide](CLI.md).

```sh
go get github.com/alexschlessinger/pollytool
```

## Contents

- [Quick start](#quick-start)
- [Requests and messages](#requests-and-messages)
- [Providers and model metadata](#providers)
- [The agent loop](#the-agent-loop)
- [Tools](#tools)
- [Shell tools and sandboxing](#shell-tools)
- [MCP servers](#mcp-servers)
- [Derived registries](#derived-registries)
- [Opening tools for a workspace](#opening-tools-for-a-workspace)
- [Skills](#skills)
- [Subagents](#subagents)
- [Swarms and workflows](#swarms-and-workflows)
- [Integration](#integration)
- [JavaScript API](#javascript-api)
- [Sessions and artifacts](#sessions)
- [Errors, concurrency, and ownership](#errors-concurrency-and-ownership)

[Documentation index](README.md) · [Sandbox policy](SANDBOX.md) ·
[Workflow guide](WORKFLOWS.md)

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/alexschlessinger/pollytool/llm"
    "github.com/alexschlessinger/pollytool/messages"
)

func main() {
    ctx := context.Background()
    client := llm.NewMultiPass(map[string]string{
        "openai": os.Getenv("POLLYTOOL_OPENAIKEY"),
    })
    answer, err := llm.Collect(ctx, client, &llm.CompletionRequest{
        Model:     "openai/gpt-5.4",
        Messages:  messages.User("Tell me a joke"),
        MaxTokens: 500,
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(answer)
}
```

`Collect` prepares the request and hands back the text. When you need more than
text, `Complete(ctx, client, req)` returns the full `*messages.ChatMessage`,
with usage, reasoning, content parts, tool calls, and the stop reason intact.
Neither helper executes tools, and both return whatever text had streamed along
with the error when generation fails.

To stream yourself, call `Prepare` first, then read from
`client.ChatCompletionStream(ctx, req, messages.NewStreamProcessor())`. Cancel
the context to stop early; the provider and processor release any blocked reads
and writes. A custom `EventStreamProcessor` receives that same context in
`ProcessMessagesToEvents(ctx, messages)` and must honor its cancellation.

The channel carries these events:

| Event | Read |
|---|---|
| `messages.EventTypeContent` | `event.Content`: incremental answer text |
| `messages.EventTypeReasoning` | `event.Content`: exposed reasoning text |
| `messages.EventTypeToolCall` | `event.ToolCall` |
| `messages.EventTypeComplete` | `event.Message`: assembled message |
| `messages.EventTypeError` | `event.Error` |

When the model should actually run tools, reach for
[the agent loop](#the-agent-loop).

## Requests and messages

Every provider implements `llm.LLM`:

```go
type LLM interface {
    ChatCompletionStream(
        context.Context,
        *CompletionRequest,
        EventStreamProcessor,
    ) <-chan *messages.StreamEvent
}
```

These are the `llm.CompletionRequest` fields you'll use most:

| Field | Meaning |
|---|---|
| `Model` | `provider/model` through MultiPass; bare model for a direct client |
| `Messages` | Conversation as `[]messages.ChatMessage` |
| `APIKey`, `BaseURL` | Request credential and endpoint overrides |
| `ModelHost` | OpenRouter upstream routing ID; empty means Automatic |
| `MaxTokens` | Output allowance |
| `MaxContextTokens` | Estimated input budget; zero is unlimited |
| `Temperature` | `*float32`; nil omits it, `llm.Float32Ptr(0.7)` sets it |
| `Timeout`, `Deadline` | Stream stall budget and hard call limit; zero disables each |
| `ThinkingEffort` | `EffortOff`, `EffortLevel`, `EffortBudget`, or `EffortDynamic` |
| `StreamMode` | `llm.Streaming` (zero value) or `llm.Buffered` |
| `Tools`, `ResponseSchema`, `Skills` | Tool definitions, output contract, and skill catalog |
| `Capabilities` | Optional authoritative model facts; otherwise discovered |

`StreamMode` only changes how the upstream delivers the response. Both modes
use the same event channel and work with `Complete` and `Collect`.
`Temperature` is a pointer so that "unset" and zero stay distinct: nil leaves
the parameter out, and `llm.Float32Ptr(0)` explicitly sends zero.

A `ChatMessage` carries `Role`, `Content`, multimodal `Parts`, `ToolCalls`,
`ToolCallID`, `ToolName`, `Reasoning`, `Metadata`, and `StopReason`; use the
`messages.MessageRole*` constants for roles. Internal messages are application
state, so keep them out of provider input. When you save messages, keep their
content parts and metadata, including artifact references and composer
selections.

For the full types, see the [request contract](../llm/internal/contract/request.go),
[messages](../messages/types.go), and [stream events](../messages/events.go).

### Structured output

Set `ResponseSchema` from a reflected struct, parsed JSON, or a raw schema:

```go
type Person struct {
    Name string `json:"name"`
    Age  int    `json:"age,omitempty"`
}
responseSchema, err := llm.SchemaFor(Person{})
if err != nil {
    return err
}
req.ResponseSchema = responseSchema
// JSON definitions: llm.SchemaFromJSON(text) also returns (*Schema, error).
// Static definitions: llm.MustSchemaFor(Person{}) panics on construction errors.
```

`SchemaFor` builds a strict schema in which every field without `omitempty` is
required. An empty, malformed, or null JSON definition is a construction error;
the `Must*` variants panic on it instead. A model that explicitly doesn't
support response schemas fails before generation starts.

## Providers

`llm.NewMultiPass(keys)` routes each request by its model prefix, with keys
indexed by provider name. Build the router once and reuse it.

| Prefix | Implementation |
|---|---|
| `openai/` | [llm/openai](../llm/openai) |
| `anthropic/` | [llm/anthropic](../llm/anthropic) |
| `gemini/` | [llm/gemini](../llm/gemini) |
| `ollama/` | [llm/ollama](../llm/ollama) |
| `huggingface/` | Hugging Face router through [llm/openai](../llm/openai) |
| `deepseek/` | [llm/deepseek](../llm/deepseek) |
| `qwencloud/` | [llm/qwencloud](../llm/qwencloud) |
| `openrouter/` | [llm/openrouter](../llm/openrouter) |
| `replay/` | [llm/replay](../llm/replay): scripted turns installed by a headless shot fixture; inert otherwise |

All routing rules live in [defaultProviders](../llm/multipass.go).

You can also skip the router and use a provider directly, with bare model
names. Import the matching `llm/<provider>` package and call
`openai.NewProvider(key, baseURL)`, `anthropic.NewProvider(key, baseURL)`,
`gemini.NewProvider(key, baseURL)` (which also returns an error), or
`ollama.NewProvider(baseURL, key)`. Anthropic and Gemini use their public
endpoints when `baseURL` is empty. OpenAI uses the Responses API with an empty
URL and Chat Completions with an explicit one; `openai.NewResponsesProvider`
gets you Responses at a custom URL.

### HTTP clients

To use your own transport for inference and model discovery, pass
`llm.WithHTTPClient(httpClient)` to `NewMultiPass`. Direct provider constructors
take the same option from their own package, such as
`openai.WithHTTPClient(httpClient)`, and so do the OpenAI, Anthropic, and Gemini
wire-client constructors. For embeddings, use
`llm.Embed(ctx, req, llm.WithHTTPClient(httpClient))`.

Polly reuses your client without modifying its configuration, and nil selects a
default. The one exception is Ollama, which copies the client to wrap its
transport for bearer authentication. You own the client, so don't mutate it
while requests are in flight. Request cancellation and configured stream budgets
still apply, and model discovery keeps its ten-second bound.

### Discovery

`MultiPass` and `Agent` both expose `ListModels(ctx, target, refresh)` and
`LookupModel(ctx, target, refresh)`. A `ModelTarget` holds the `Provider`, the
bare `Model`, and optionally a `Host`, `BaseURL`, and `APIKey`.

When the target omits credentials, discovery uses the effective runtime key. An
explicit preview key never changes inference credentials, and
`UseConfiguredKey:true` with an empty key previews what clearing a process
override would do. Credential fields are never serialized.

A `ModelCatalog` reports its `Source`, `FetchedAt`, `Partial`, `Stale`, and
`Error`. Throughout, nil means unknown, which is different from false, zero, or
an explicitly empty list. Descriptions and `Raw` provider records are display
data, never prompt instructions.

To cache metadata, pass a store to the optional `SetModelMetadataCache(store)`;
`*sessions.SQLiteStore` works. Entries stay fresh for an hour and refresh in the
background, credentials are never stored, and `refresh:true` bypasses the
cache.

### Request preparation

`llm.Prepare(ctx, client, req, requireTools)` copies the request, resolves the
model's capabilities, adapts unsupported optional features, and injects skill
guidance. `Agent.Run` calls it on every iteration and `Complete` and `Collect`
call it once, but if you stream directly, you call it yourself.

A custom client can implement `ModelMetadataProvider`. Callers can also supply
`Capabilities` directly or call `PrepareCapabilities` themselves. Unknown facts
never turn into unsupported features. Positive context budgets are clamped to
the model's effective window, leaving output headroom; `ClampContextBudget` does
the same on its own.

Adaptation only touches the outgoing copy. Unsupported media becomes
descriptive text, optional tools and settings may be dropped, and completed tool
exchanges can become plain text. Required tools and output schemas fail instead
if the model explicitly doesn't support them. Stored messages and settings stay
exactly as they were, and `AgentCallbacks.OnAdaptation` receives a
`RequestAdaptation{Feature, Count, Message}` for each change.

### OpenRouter routing and reasoning

`ModelHost` pins OpenRouter to one upstream with `provider.only` and fallbacks
disabled. Other providers reject it; Hugging Face picks hosts through its
`model:host` suffix instead. A child given an explicit model drops its inherited
host pin unless it's given a new one.

`openrouter.NewProvider(key, baseURL)` uses Chat Completions, as MultiPass does,
and `openrouter.WithAPI(openrouter.ResponsesAPI)` switches to Responses. Both
support the gateway's reasoning controls, host pinning, session IDs, and
reasoning replay.

`ResolveOpenRouterThinking` validates a preference and returns `Request`,
`Display`, and `Notice`. `ResolveOpenRouterRequestThinking` adapts a saved
preference the model no longer supports so a request can still run. Neither one
rewrites the saved preference. `ThinkingEffortWordsFor` supplies the
model-aware choices for a form. The [CLI reasoning rules](CLI.md#thinking-on-openrouter)
describe the resulting behavior.

Reasoning replay is tied to the endpoint, the requested model, and the API
dialect. It lives in message metadata under `"openrouter"`, so preserve that
metadata when you save messages.

## The agent loop

`llm.NewAgent(client, registry, config)` handles the whole cycle: preparing
requests, running tool calls, asking for approvals, streaming output, and
continuing the model's turn. Call `Run` with a request and an optional
`*llm.AgentCallbacks`.

| `AgentConfig` field | Purpose |
|---|---|
| `MaxIterations` | Model calls per run; default 1,024 |
| `ToolTimeout`, `MaxParallelTools` | Per-tool timeout and parallelism; zero means unlimited |
| `DisableTools` | Disable all model tools, including private helpers |
| `ResponseTool` | Require a named final-response tool |
| `RequireResponseToolSuccess` | Require its successful receipt, not merely a call |
| `ArtifactStore`, `OpenArtifact` | Private output storage and optional authorized external reads |

The agent owns a derived registry that adds `read_transcript`, `read_artifact`,
and `list_artifacts`. `view_image` comes from the registry you supply; the agent
never constructs or replaces it. `agent.ToolRegistry()` shows the effective
tools.

`agent.Close()` releases the agent's view, but you still own the original
registry, MCP clients, and artifact store. An agent runs one `Run` at a time.

To lower a single run's allowance without touching the agent, use
`llm.WithIterationLimit(ctx, n)`. Nested limits can only lower it further.

### Callbacks and persistence

| Callback | Use |
|---|---|
| `OnContent`, `OnReasoning`, `OnComplete`, `OnError` | Display streamed output and outcomes |
| `ApproveToolCalls` | `func(context.Context, []messages.ChatMessageToolCall) ([]bool, error)`; nil approves all |
| `BeforeToolExecute`, `OnToolStart`, `OnToolEnd` | Supply execution context and observe calls |
| `OnToolResult` | Observe durable rich results, including media/artifact parts |
| `BeforeFirstRequest` | Persist new input after successful projection, before any provider call; an error vetoes the run |
| `OnRequestProjection`, `OnIterationUsage` | Track each request's projected size and measured usage |
| `OnUsageProgress` | Observe provider usage and billed cost while a response streams |
| `AdmitInput`, `Checkpoint`, `JournalToolBatch` | Coordinate durable peer input and recoverable tool intent |
| `BeforeToolBatch`, `AfterToolBatch`, `ContinueAfterFinal` | Validate batches, park executions, or continue provisional answers |

`ApproveToolCalls` returns exactly one decision per call, in order. An error
aborts the batch, and a mismatched count returns `llm.ErrInvalidToolApproval`
before any tool runs. Honor the supplied context while you wait for the user.

If you call `Agent.Run` directly, every callback is yours. Managed parent and
member runs are different: the swarm reserves `AdmitInput`, `Checkpoint`, and
`JournalToolBatch` for its atomic input receipts, transcript checkpoints, and
tool intent. Passing any of them through `RunParent` or `Config.Callbacks`
returns `swarm.ErrCallbackOwnership`, naming every conflicting hook, before the
agent runs.

The rest of your hooks still work. The runtime copies your callbacks before
binding its own, so your observers, approvals, and execution-context hooks stay
active, and they run in a predictable order:

- `BeforeFirstRequest` can veto a member assignment before its input is saved.
- Your `BeforeToolBatch` runs before the runtime's batch fence and journal.
- Your `AfterToolBatch` runs before the member parks. An error from either batch
  hook stops the run, and completed results are still checkpointed.
- Your `ContinueAfterFinal` runs before parent settlement or member completion
  checks. Returning input continues the run within the same iteration budget, an
  error stops it, and an empty successful return lets the runtime's final checks
  proceed.
- `OnComplete` fires only once those checks accept a final response.

`AgentResponse.AllMessages` holds the messages the run generated plus any peer
input it admitted, but not the initial history. Save it even when the run ends
early, with every content part intact. If you use checkpoints, save only
`AllMessages[PersistedMessages:]`. The `Message` field on its own isn't enough
for durable replay.

`OnUsageProgress` may fire repeatedly with rising counts while a response
streams; `OnIterationUsage` is the final word for each iteration.
`response.TokenUsage()` returns `TokenUsage{TotalInput, TotalOutput, PeakInput,
CacheRead, CacheWrite, ReportedCostUSD}`. Use the totals for accounting and peak
input (the largest single request) for context display. `ReportedCostUSD` is
what gateways such as OpenRouter billed, and zero when none reported a cost.

## Tools

Implement [tools.Tool](../tools/interface.go), or reach for `tools.Func` when a
closure will do:

```go
func runAgent(ctx context.Context, client llm.LLM) (*llm.AgentResponse, error) {
    registry := tools.NewToolRegistry([]tools.Tool{
        &tools.Func{
            Name: "echo",
            Desc: "Return the supplied text",
            Params: schema.Params{"text": schema.S("Text to return")},
            Required: []string{"text"},
            Run: func(ctx context.Context, args tools.Args) (string, error) {
                return args.String("text"), nil
            },
        },
    })
    defer registry.Close()
    agent := llm.NewAgent(client, registry, llm.AgentConfig{MaxIterations: 8})
    defer agent.Close()
    return agent.Run(ctx, &llm.CompletionRequest{
        Model: "openai/gpt-5.4",
        Messages: messages.User("Echo hello using the tool"),
    }, nil)
}
```

`schema.Params` has helpers such as `S`, `Int`, `Bool`, `Enum`, and `Array`,
and `tools.Args` gives typed access to arguments, which matters most for numbers
decoded from JSON.

A tool can opt into more behavior by implementing any of these:

| Optional interface | Contract |
|---|---|
| `OutputTool` | Return `ToolOutput{Text, Data, Media}`; wrappers must preserve all three |
| `UntimedTool` | Exempt a long call from the per-tool timeout, not cancellation |
| `ExclusiveTool` | Require the call to be alone in its model batch |
| `RecallTool` | Supply a stub for results reproducible on demand |
| `CoordinationTool` | Declare trusted orchestration behavior at the execution gate |
| `ContextTool` | Bind implementation and authority to another execution context |
| `ContextIndependentTool` | Declare safe independence from the current workspace |

A custom host that resolves and approves tools itself should then call
`registry.ExecuteTool(ctx, tool, args, timeout)`. It applies the timeout and the
execution gate, checks again after waiting on the gate that the approved handle
is still registered and allowed, and preserves rich output. It returns the
original error alongside `ToolExecution{Output, Invoked, ContextErr}`. Approval,
artifact persistence, and presentation are still up to you.

Keep in mind that custom in-process Go tools run inside your host. They have to
enforce their own filesystem and network authority, because registering one
doesn't put your process in a sandbox.

### Native setup

`NewToolRegistry` serves exactly the tools you give it. Add
`tools.WithNativeTools()` to install the native constructors for Bash and the
file tools and to register `view_image`; sandbox options configure authority
separately. A host that restores native tools through `LoadRegistry` needs to
pass the option there too (CLI setup already does).

A generic registry and `Derive` install no native tools. A derived view inherits
the native constructors and live sandbox policy without preparing a new sandbox,
but native bindings need a source built with `WithNativeTools` and otherwise
return `ErrNativeToolsRequired`.

`MarkBuiltin(name)` keeps a tool visible through derived allow-lists, execution
bindings, and skill policies; native setup marks `view_image` this way, and a
custom image tool can use the same marker. Built-ins are left out of
`GetActiveToolLoaders`, so session persistence records only the tools that were
actually selected.

## Shell tools

A shell tool is an executable that implements `--schema` and
`--execute <json-args>`; the [CLI guide](CLI.md#shell-tools) has the protocol
and an example.

```go
registry := tools.NewToolRegistry(nil,
    tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()),
)
defer registry.Close()
if _, err := tools.LoadShellToolsWithRegistry(registry, []string{"./uppercase.sh"}); err != nil {
    return err
}
```

Any process-backed tool needs either a sandbox factory or an explicit
`tools.WithUnsafeNoSandbox()`; without one, loading fails before `--schema`
even runs. The [CLI guide](CLI.md#bash) describes how Bash calls behave.

### Sandboxing in the library

The CLI's opt-in rules don't apply to library construction.
`sandbox.DefaultConfig()` and `ParsePreset("")` give you temporary writes and no
network, while `ParsePreset(sandbox.DefaultPresetSpec)` gives you
`workspace+net+git`. The [sandbox guide](SANDBOX.md) is the complete policy
reference.

| Registry API | Effect |
|---|---|
| `WithSandboxFactory(factory, base)` | Install and freeze the base process policy |
| `AppendBaseReadPaths(paths...)` | Extend reads and rebuild loaded/staged Bash and shell tools transactionally |
| `WithSandboxLayer`, `SetSandboxLayer(name, layer)` | Add/replace a named layer; nil removes it |
| `SetSandboxLayerAndCommit(name, layer, commit)` | Publish only after both reconstruction and persistence succeed |
| `SandboxPolicyRevision()` | Read a shared revision for invalidating cached bindings after successful base/layer changes; no native preparation |
| `SandboxContext()` | Describe effective authority without environment values |
| `RunTrial(ctx, command, candidate)` | Execute a diagnostic overlay without changing registry policy |
| `ExecutionPolicy(root, grant)` | Derive a narrowed workspace policy |
| `BindExecutionContext(policy, allowedTools)` | Build a registry bound to that workspace; return omitted tools too |
| `GuardExecution` | Hold the execution gate for a custom executor |
| `BeginEnvironmentMaintenance` | Try to hold the registry's exclusive storage-maintenance gate |

Layers merge by name, between the base policy and each tool's own declaration.
Derived registries share live layers, while bound members get the applicable
`Members` policy at binding time. **Layers don't reach stdio MCP, schema
discovery, or administrative Git.** `SandboxChange` names the servers still
running under their previous base policy.

The commit callback must not reenter the registry. If the save or the rebuild
fails, the previous tools and policy stay active. `SandboxLayer.Environment`
materializes managed storage per checkout, or as read-only scratch. Leasing that
storage across processes is up to the host.

`Agent.Run` adds a summary of the current sandbox to each outgoing request. If
you call the model directly, `llm.WithSandboxContext` composes the same summary;
apply it to unaugmented history after resolving skills. The summary describes
the policy to the model and enforces nothing.

### Process and trial ownership

Wrap commands with `sandbox.WrapCmdManaged` or `WrapCmdWithEnvManaged`, and run
the idempotent cleanup they return after `Start` or `Run`, even when those fail.
Cleanup releases the wrapper's own resources and leaves caller-owned
`ExtraFiles` alone.

For finite commands, `sandbox.WrapFiniteCmdManaged` requires a command with its
own cancelable context. You still call `Wait` and drain the output. It's meant
for commands that finish, not for long-lived MCP servers.

If a command exits while background writers still hold its output pipes,
capture waits at most a second and then returns `ErrCommandOutputIncomplete`
rather than an ordinary `CommandError`.

A trial reports the process's exit in `TrialResult.ExitCode` and its observed
denials where the backend can see them. On Linux with a readable home it can't,
and an empty observation never proves the access was sufficient.

### Context files and change tracking

`ReadContextFile(ctx, path, maxBytes)` reads a file under the registry's policy
with safe regular-file reads and returns `(absolutePath, data, error)`.
Oversized content is rejected, not truncated. `ContextFilePaths` discovers
bounded, Gitignore-aware candidates without running Git, and a failed discovery
doesn't stop explicit reads.

`edit_file` and `write_file` return `tools.FileChanges` in `ToolOutput.Data`.
Given a `ChangeTracker`, Bash includes the same payload in
`CommandResult.Changes`. `WithChangeTracker` / `SetChangeTracker` propagate the
tracker to derived and bound registries. `worktree.NewChangeTracker` uses a
private Git index and object store, and reports `Tracked:false` with a reason
when it can't observe changes. `CountsUnknown` distinguishes incomplete counts
from zero.

For net changes across turns, use `CaptureBaseline`, `RestoreBaseline`, and
`WorkspaceChanges`. The self-contained baseline pack is limited to 64 MiB. Keep
the baseline and the latest report in session-owned artifacts, and close the
tracker once its users are done. A tool result's delta describes that one call,
not the net workspace.

## MCP servers

```go
_, err := registry.LoadMCPServer("./mcp.json#filesystem")
if err != nil {
    return err
}
```

MCP tools are namespaced, as in `filesystem__read_file`. Stdio servers run under
the registry's base policy plus their own declaration. Restarting a server
applies the current base policy, and named layers stay excluded. If a
replacement fails to start, the old server keeps running. Remote servers execute
elsewhere and can't be sandboxed locally.

`tools.NewUnsafeMCPClient(spec)` is an explicit, standalone opt-out, and its
caller owns and closes the client. When tools should share a policy, load them
through the registry instead.

## Derived registries

`registry.Derive(tools.AllowTools(...), tools.DenyTools(...))` creates a private
view that inherits the parent's tools, clients, and policy. A child can shadow
parent names with its own tools, but the parent's policy still bounds access.
Anything the child loads or activates later stays private to it. Closing a child
closes only its own clients, while closing the parent removes its inherited
tools from every descendant.

Close things in reverse order: agent, derived registry, then parent registry.
For delegated work, use `subagent.ChildRegistry`, which also strips out
parent-only tools.

## Opening tools for a workspace

`tools.OpenTools` is a function type,
`func(context.Context, tools.ToolScope) (tools.ToolBinding, error)`, that opens
a tool set for one workspace. A scope carries the `Root`, `SourceRoot`,
`ExecutionGrant`, extra `ReadPaths`, `AllowedTools`, and an optional shared
`ExecutionGate`. A nil tool selection inherits, an empty slice disables tools in
the runner, and patterns select names while keeping built-ins.

The binding gives you a ready `Registry`, repository `Instructions`,
`ToolInstructions`, diagnostics for omitted tools, and a non-nil, idempotent
`Close`. The constructor either enforces the scope or fails, releasing anything
it had partly built. Close the agent first and the binding second. Borrowed
source resources remain owned by whoever lent them.

`tools.NativeOpenTools(source, opts...)` is the native implementation. It
narrows native authority with `ExecutionPolicy`, rebinds tools through
`ContextTool`, reconnects local MCP servers, and renders skill guidance. When
`ToolScope.SourceRoot` is set, it overrides `ExecutionGrant.SourceRoot` for both
tool paths and member-layer environment paths. `WithNativeInstructions(loader)`
supplies repository guidance read under the bound registry's policy. Other
constructors return their own tools without native rebinding.

After registering host tools, validate a selection with
`Registry.ValidateToolSelection(allowed, llm.BuiltinToolNames())`. Compose the
two guidance fields under your host's prompt rules, and clear inherited request
skills when the binding has already rendered them.

## Skills

`skills.LoadCatalog(directories)` reads skill directories, and
`tools.NewSkillRuntime(catalog, registry)` registers the activation tools.
`Activate(name)` turns a skill on, and `ActivatedSkills()` and `Restore(names)`
let you persist those choices. Handle catalog and runtime errors before using
either.

Set `CompletionRequest.Skills` to have skill guidance injected into the prompt
automatically, or compose it yourself with `catalog.RuntimeSystemPrompt(systemPrompt)`.
`skillRuntime.Derive(childRegistry)` inherits activations and policy without
reconnecting the parent's MCP clients, and anything the child activates stays
private to it.

## Subagents

For a simple in-memory child, register
`subagent.NewTool(subagent.AgentRunner(client, registry, request, config))`.
`AgentRunner` runs the same agent loop with a derived tool view and the request
context you supply. It has no durable sessions or background delivery, so
background requests finish synchronously. The CLI uses the managed swarm
runtime described below instead.

`subagent.RunnerWithTools(client, open, scope, request, config, opts...)` opens
a fresh binding per child rather than deriving a view. The child's tool
selection becomes `scope.AllowedTools`, its repository and tool guidance are
composed into its request, and the agent closes before the binding. Both runners
keep partial output and usage when a run returns an error.

New children need a label of 1–80 characters. A nil `Request.Tools` inherits,
and an explicitly empty slice disables tools. Children never get `spawn_agent`,
nor any root-only identity, theme, setup, or coordination-control tools.
`WithCallbacks(factory)` can supply approvals and display hooks.

`NewTool` allows 32 concurrent children by default; `WithMaxConcurrent` changes
that. A custom runner whose child outlives the call must return `Result.Done`,
even on cancellation, and close it once the child settles.
`WithRuntimeScheduler` hands admission to an enclosing runtime. Model-facing
calls can't set `MaxIterations`, but trusted Go hosts can.

## Swarms and workflows

The managed runtime handles model spawns, `/spawn`, and workflow agents through
one system. [WORKFLOWS.md](WORKFLOWS.md) walks through the task lifecycle with
user-facing examples; this section covers hosting it.

### Host setup

Construct the runtime with `swarm.New(swarm.Config{...})`, supplying `Store`,
`Parent`, `Registry`, `OpenTools`, `Client`, `Request`, `Agent`, and `Root`.
Native hosts pass `tools.NativeOpenTools(registry)` as `OpenTools`.

`OpenTools` is called for every member slice while it holds its session lease,
and the binding is closed before the lease is released. The parent keeps its
existing registry.

`Parent` must implement `sessions.CoordinationSession`, which both SQLite disk
and memory sessions do. Cross-process recovery needs disk storage, and `Promote`
lets a host arrange that before coordination starts changing state.

On each turn, register `runtime.RegisterParentTools(registry)` and call
`RunParent(ctx, agent, request, callbacks, persistenceAllowed)`, which wires in
mail admission, checkpoints, tool intent, and settlement. Save only the
response's unpersisted suffix, then report the persistence and output verdict
with `ParentTurnSettled(err)`. Close the runtime before the parent session and
registry.

The CLI supplies `NativeOpenTools` with a repository-instruction loader, so
members read guidance under their own bound policy; a custom constructor
supplies its own guidance. Native Git administration, CLI repository reads, and
sandbox management still need native services, and custom model tools don't
replace those host operations.

`Config.Callbacks` supplies host hooks for each member slice, under the same
[callback ownership rules](#callbacks-and-persistence) as `RunParent`.

`PrepareMember` installs session-scoped tools on each slice under its current
lease, and a member's tool selection may name them. A nil registry means tools
are disabled. Bind member tools to the session you're handed, never to a
captured parent session. `UpdateDefaults` changes future defaults without
widening any existing member's authority.

### Runtime methods

| Area | Methods |
|---|---|
| Parent | `RegisterParentTools`, `RunParent`, `ParentTurnSettled`, `ParentState`, `UpdateDefaults`, `Close` |
| Launch/control | `Agent`, `Spawn`, `Followup`, `Resume`, `ResumeWithIterations`, `StopMember`, `HasActive` |
| Tasks | `CreateTask`, `ReadTask`, `Claim`, `Submit`, `Review`, `UpdateTask`, `BlockTask`, `CancelTask` |
| Sharing/settlement | `Send`, `Publish`, `Integrate`, `Settle` |
| Workflows | `RunWorkflow`, `StartWorkflow`, `CancelWorkflow`, `SaveWorkflow`, `AcknowledgeWorkflow`, `DeferWorkflow` |
| Resources | `Cleanup(ctx, contextID)`; empty ID selects the family. `Forget(ctx)` also removes settled snapshot refs |
| Inspection | `State`; package functions `ReadStateView`, `Present`, `MemberState`, `TaskStatus`, `DecisionCounts` |

One thing to watch: in Go, `Followup` **creates a linked task but doesn't
launch it**. Call `Agent` or `Spawn` afterward. The model and JavaScript
follow-up operations do both.

An `AgentRequest` picks the brief and label, session and task,
source/snapshot/context, read-only or review mode, tools, model and host, result
schema and input, and a trusted call ID. An `AgentResult` returns `Value`,
`Session`, `Context`, `Task`, `Execution`, `Revision`, and `Usage`. The returned
context may be released later.

Each task's completion requirement is fixed when it's created: `delivered`,
`reviewed`, or `applied`. Unowned tasks default to `delivered`, so create
unowned editing work with an explicit `applied`. Dependencies need `done`.
Review either accepts reviewed research or requests changes, while editing work
completes through integration.

### Budgets and final values

The defaults are 32 concurrent executions, 256 starts per run, and 512
workspace slots. `Resume` keeps the remaining model calls, and a positive grant
extends the start budget if the launch succeeds. `ResumeWithIterations` adds
calls to a paused execution without spending a start. Active workflow
reservations must settle or be canceled. Only trusted Go hosts can override
`AgentRequest.MaxIterations`.

When a member runs out, it's saved as `paused` / `max_iterations` and returns a
`*swarm.IterationLimitError` carrying the IDs and the used and allowed counts.
It wraps `llm.ErrMaxIterations`, and `llm.IsIterationLimit` excludes errors
joined with persistence or provider failures.

A result `Schema` requires an exclusive `swarm_complete({value})` call when
tools are enabled, or validated JSON when they're disabled. Scalars and null
are fine when the schema permits them; duplicate keys, trailing JSON, and schema
violations fail. A `value` that is a JSON string is decoded once and accepted
when the decoded value validates; a root type mismatch is reported by type
only, other errors are clipped to 512 bytes, and a key that differs from the
schema only by case is named (`did you mean "detail"?`). A missing or invalid
completion gets at most three corrections within the allowance, counted down in
the correction (`Correction 1 of 3` through `Correction 3 of 3`); the fourth
invalid result fails the task. An empty unstructured final gets one correction. Partial work
stays inspectable either way. JSON parse errors in any tool call name the byte
offset and quote the text around it.

### Records, display, and recovery

Swarm records carry a `format` domain entry with key `swarm` and value
`{"version":2}`, written by the first coordination mutation. Incompatible
records return `swarm.ErrUnsupportedFormat` without touching saved files. This
is separate from SQLite schema versioning.

| Record | Source |
|---|---|
| Run, member, task, execution, mail, publication, parent turn, parent (publication receipts) | [swarm/state.go](../swarm/state.go) |
| Completion requirements and deferrals | [requirement.go](../swarm/requirement.go), [deferral.go](../swarm/deferral.go) |
| Workspace and snapshot | [worktree/worktree.go](../worktree/worktree.go) |
| Integration candidate and apply receipt | [integration.go](../swarm/integration.go), [apply.go](../swarm/apply.go) |
| Workflow report and step | [workflow/workflow.go](../workflow/workflow.go) |

Deleting or expiring a root session removes its family's records but not its
Git workspaces. To find a result's code, follow the task and execution
provenance, since a member's current context may have been released or
replaced.

`swarm.Present` produces typed lifecycle, execution, task, decision, and control
fields. Build logic on those; `Display` is text for humans. An idle member isn't
necessarily accepted or integrated. `ReadStateView` reads through a trusted
read-only store without opening a live runtime or taking a lease.

### Workflow host and contexts

`RunWorkflow` waits for the saved report. `StartWorkflow` returns an ID right
away and detaches from the caller's cancellation, though runtime shutdown still
stops it. A terminal foreground result is acknowledged only once its matching
tool result or artifact reference commits, so a failed save leaves the notice
available. Background and direct Go launches deliver through notices. Failed,
canceled, or interrupted reports need an acknowledgment or an explicit deferral,
and neither one accepts their tasks.

The standalone `workflow.Runner{Host, Config}` takes a trusted host with `Call`
and an optional `Recorder.SaveWorkflow`. Teardown drains host calls and records
late apply receipts. JavaScript is never replayed automatically.

An `ExecutionContext` binds a root, scratch space, read-only state, and a
narrowed policy. A tool has to implement `ContextTool` or declare
`ContextIndependentTool` to survive rebinding. Stdio MCP servers relaunch inside
the context, and remote MCP needs `contextIndependent:true`. Indexed semantic
search isn't available to members.

## Integration

`Runtime.Integrate` takes exactly one of: exact task revisions, or a candidate
ID. When preparing from tasks, `Drift` can be `paths` (the default) or `tree`;
it doesn't apply to an existing candidate. The result carries the status,
candidate, tasks, unchanged state, receipt, and next action. A conflict returns
a repairable candidate inside the structured error.

For finer control, integration also breaks into steps:

| Method | Purpose |
|---|---|
| `PrepareIntegration` | Build an ordered candidate; stop on the first conflict |
| `ReadIntegration` | Inspect the current candidate and receipt |
| `ReviseIntegration` | Adopt a repair based on the exact intermediate state |
| `RefreshIntegration` | Rebase a ready candidate against current parent files; report `Changed` |
| `AcceptIntegration`, `ApplyIntegration` | Stepwise acceptance/application |
| `ReconcileApply` | Observe an interrupted outcome without replaying its patch |

Applied receipts make retries idempotent, even after snapshot cleanup. An
unchanged outcome needs explicit filesystem proof, and an uncertain write blocks
new integration until it's reconciled. Apply times out after two minutes by
default, followed by bounded outcome recording, and runtime shutdown waits for
both.

`paths` drift allows unrelated parent edits, while `tree` requires the whole
parent tree to match. Validate merged candidates in disposable contexts before
applying them. See [integration and repair](WORKFLOWS.md#integrating-editing-results)
for the user-facing side.

## JavaScript API

A workflow script defines exactly one workflow, with
`polly.workflow(name, inputSchema, run)` or its object form
`polly.defineWorkflow({name, inputSchema, run})`. Put every effect inside `run`,
return JSON-compatible output, and await every operation.

| Method on `polly` | Result / rule |
|---|---|
| `agent(label, task, options?)` or `agent({...})` | `AgentResult`; new agents need a label, continuations inherit it |
| `research(label, task, options?)` | Agent forced read-only |
| `editor(source, label, task, options?)` | Agent bound to a nonblank source checkout |
| `followup({task, question, commit?, label?})` | Create and run a linked task on the same member |
| `integrate({tasks?, candidate?, drift?})` | Accept and integrate exact editing results |
| `context({source?, commit?, context?, readOnly?, disposable?})` | Opaque isolated-copy ID |
| `scope({context, label?}, async work => ...)` | Scoped host methods; no `cwd` override |
| `tool(name, args, {context})` | `{text, data, artifacts, step}` |
| `exec(command, {context, check?})` | Tool result plus `exitCode`; Bash with `pipefail`, no `errexit`; `check` defaults true |
| `snapshot(context)` | Immutable `{commit, tree, source}` |
| `release(context)` | Remove an inactive owned context after proof checks |
| `tasks.create`, `tasks.update`, `tasks.read` / `get`, `tasks.review` | Explicit dependencies, ownership, and research review |
| `integration.prepare`, `read`, `revise`, `refresh`, `accept`, `apply`, `reconcile` | Advanced candidate repair/recovery |
| `parallel(items, callback, options?)` | Ordered success/error results; default concurrency 8, allowed 1–256 |
| `log(message)`, `fail(message, result?)` | Saved progress or structured failure |
| `schema` | `string`, `number`, `integer`, `boolean`, `enum`, `array`, `object`, `keyed`; short aliases available |

Agent options include `input`, `schema`, `tools`, `model`, `modelHost`,
`readOnly`, `review`, `source`, `commit`, `context`, `session`, and `taskID`.
`tools` can never widen inherited authority. A full local Git commit ID selects
that exact commit, while omitting `commit` captures the current eligible dirty
and untracked files.

The VM has no Node, module, filesystem, network, process, or timer APIs, and
returning while host work is still pending fails the attempt.

For complete signatures, errors, and examples, see the shipped
[workflow reference](../swarm/workflow_help.md), also available as
`workflow_help()`.

## Sessions

`sessions.OpenStore(StoreConfig)` opens the same SQLite implementation either in
memory or on disk. Disk mode needs an explicit literal path, so expand `~`
yourself. The CLI uses `~/.pollytool/polly.db`.

```go
store, err := sessions.OpenStore(sessions.StoreConfig{
    Mode: sessions.ModeDisk,
    Path: "/path/to/polly.db",
})
if err != nil {
    return err
}
defer store.Close()
session, err := store.Acquire(ctx, "project", sessions.AcquireOptions{})
if err != nil {
    return err
}
defer session.Close()
ctx = session.Context() // canceled if this process loses its lease
err = session.AddMessage(ctx, messages.ChatMessage{
    Role: messages.MessageRoleUser, Content: "Hello",
})
```

| Operation / option | Contract |
|---|---|
| `Acquire` | Exclusive lease; a competing owner gets `ErrSessionInUse` |
| `ExistingOnly` | Refuse missing/expired sessions with `ErrSessionNotFound` |
| `ExpectedID` | Verify a viewed stable identity; implies `ExistingOnly` |
| `Auto:true`, `AutoSessionTTL` | Give newly created automatic sessions a retention limit; reopen preserves the class |
| `GetHistory`, `GetMetadata` | Detached copies |
| `Clear`, `Reset` | Clear transcript/artifacts; Reset also replaces settings |
| `ListSummaries` | Stable IDs, parent IDs, metadata, counts, and lease state |
| `Parent` | Link a child by stable identity, independent of rename |
| `DurableStore.Promote` | Move a memory store to disk while preserving handles, IDs, and artifacts |

### Metadata and titles

`Metadata.Name` is the handle you resume by, and `Title` is a descriptive
label. The optional `TitleSession.SetTitle(ctx, title, source)` normalizes
whitespace, rejects control characters and empty text, and caps titles at 80
Unicode characters. Duplicate titles are fine. An agent can't replace a title
the user set (`ErrTitleProtected`). `SetMetadata`, `Clear`, and `Reset` all
preserve who owns the title, so use `SetTitle` to change it.
`sessions.DisplayLabel` picks the title, then the child task, then the handle.

`ExtraReadDirs` stores the session's canonical read grants and survives reset
and clear. `SpawnCallID` identifies the delegation that created a session, and
`SpawnOutcome` records only that first run; follow-ups don't rewrite it.
`Metadata.Parent` is a display name, while the stable `ParentID` is the real
ancestry key.

### Read-only views

`SQLiteStore.ReadView(ctx, ViewTarget, knownRevision)` returns a session's
identity, revision, metadata, history, lease state, and a read-only artifact
store, without acquiring a lease or bumping its last-used time. If the revision
hasn't changed, history is left out. Target a view by name, by stable ID, or by
parent and spawn-call pair, and pass `ExpectedID` when you later acquire the
viewed session to write to it.

`sessions.ViewStore`, `ViewIdentity`, and `CoordinationViewStore` are optional
capabilities. Coordination views let you inspect a family's saved state without
giving model tools access to another family. Missing or expired identities are
refused. Views don't extend retention, so a reset or deletion can remove their
artifacts, and store shutdown cancels their readers. Writes return
`ErrReadOnlyView`.

### Artifacts

`session.ArtifactStore()` is scoped to its session, and SQLite commits artifact
bytes in the same database as transcripts. Pass it to `AgentConfig.ArtifactStore`.

`OpenArtifact` is an optional callback that authorizes references the
conversation doesn't contain. It returns the matching `artifacts.Ref` metadata
and an `io.ReadCloser` positioned at zero, which `read_artifact` closes. For
swarms, bind `CoordinationSession.OpenPublishedArtifact` to the actual member's
session. With a nil callback, reads are limited to references in the
conversation. Either way, it doesn't expand `list_artifacts` or allow reading a
peer's unpublished artifacts.

## Errors, concurrency, and ownership

Provider streams report failures as error events, while `Collect` and
`Agent.Run` return errors. Keep any partial agent output before you report a
failure. Tool failures use the structured `*tools.ToolError`.

`tools.CommandError` means the target ran and exited normally, with its output
fully captured. Sandbox setup, approval, cancellation, timeout, and incomplete
capture are all distinct failures. Workflow `exec(command, {check:false})`
recovers only from ordinary exits. A denial inside a shell that has already
started is still that shell's exit, since Polly doesn't guess intent from
stderr.

A few rules of thumb:

- `MultiPass` and `SQLiteStore` are safe to share concurrently. Run one turn per
  agent at a time.
- Tool implementations must tolerate parallel calls. `UntimedTool` alone doesn't
  bypass coordination gates.
- Session mutations are transactional, but a read-modify-write spread over
  several calls needs coordination from the host. Use the lease context for
  work tied to ownership.
- Close agents before their registries, and stop runtimes and trackers before
  closing sessions and stores. `Runtime.Close` waits for integration and outcome
  recording to finish.
- Custom stores must preserve leases, stable identity, atomic coordination and
  transcript updates, and artifact ownership. Implement whichever optional
  capabilities your host actually uses.
