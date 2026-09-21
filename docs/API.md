# Go API

Embed Polly's streaming clients, agent loop, tools, and sessions in a Go program.
For the terminal app, start with the [CLI guide](CLI.md).

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
    req, _, err := llm.Prepare(ctx, client, &llm.CompletionRequest{
        Model:     "openai/gpt-5.4",
        Messages:  messages.User("Tell me a joke"),
        MaxTokens: 500,
    }, false)
    if err != nil {
        log.Fatal(err)
    }
    answer, err := llm.Collect(ctx, client, req)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(answer)
}
```

`Collect` returns final text. For streaming, consume
`client.ChatCompletionStream(ctx, req, messages.NewStreamProcessor())`.
The channel carries these event types:

| Event | Read |
|---|---|
| `messages.EventTypeContent` | `event.Content`: incremental answer text |
| `messages.EventTypeReasoning` | `event.Content`: exposed reasoning text |
| `messages.EventTypeToolCall` | `event.ToolCall` |
| `messages.EventTypeComplete` | `event.Message`: assembled message |
| `messages.EventTypeError` | `event.Error` |

Use [the agent loop](#the-agent-loop) when the model should execute tools.

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

Common `llm.CompletionRequest` fields:

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
| `Stream` | `*bool`; nil defaults to streaming |
| `Tools`, `ResponseSchema`, `Skills` | Tool definitions, output contract, and skill catalog |
| `Capabilities` | Optional authoritative model facts; otherwise discovered |

A `ChatMessage` carries `Role`, `Content`, multimodal `Parts`, `ToolCalls`,
`ToolCallID`, `ToolName`, `Reasoning`, `Metadata`, and `StopReason`.
Use the `messages.MessageRole*` constants. Internal messages are application state
and must be excluded from provider input. Preserve content parts and metadata when
saving messages, including artifact references and composer selections.

Full types: [request contract](../llm/internal/contract/request.go),
[messages](../messages/types.go), [stream events](../messages/events.go).

### Structured output

Set `ResponseSchema` to a reflected struct, parsed JSON, or raw schema:

```go
type Person struct {
    Name string `json:"name"`
    Age  int    `json:"age,omitempty"`
}
req.ResponseSchema = llm.SchemaFor(Person{})
// Or: llm.SchemaFromJSON(`{"type":"object","properties":{...}}`)
```

`SchemaFor` creates a strict schema; fields without `omitempty` are required.
An explicitly unsupported response schema fails before generation. Anthropic can
carry the schema through a tool when tool calling is available.

## Providers

`llm.NewMultiPass(keys)` routes requests by prefix. Keys are indexed by provider
name. Construct the router once and reuse it.

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

Routing rules live in [defaultProviders](../llm/multipass.go). Direct clients take
bare model names: `llm.NewOpenAIClient(key, baseURL)`,
`NewAnthropicClient(key)`, `NewGeminiClient(key)` (also returns an error), and
`NewOllamaClient(baseURL, key)`.

### Discovery

`MultiPass` and `Agent` expose `ListModels(ctx, target, refresh)` and
`LookupModel(ctx, target, refresh)`. `ModelTarget` contains `Provider`, bare
`Model`, optional `Host`, `BaseURL`, and `APIKey`.

Omitted credentials use the effective runtime key. An explicit preview key does
not change inference credentials. `UseConfiguredKey:true` with an empty key
previews clearing a process override. Credential fields are not serialized.

`ModelCatalog` reports `Source`, `FetchedAt`, `Partial`, `Stale`, and `Error`.
Partial results may still be useful. Model and endpoint facts remain separate;
nil means unknown, distinct from false, zero, or an explicit empty list.
`UnlimitedLimits` explicitly names unlimited fields. Descriptions and bounded
`Raw` provider records are display data, not prompt instructions.

The optional `SetModelMetadataCache(store)` accepts a store with `GetModelCache`
and `PutModelCache`; `*sessions.SQLiteStore` implements it. Cache scope includes
provider, endpoint, credential, model, and host without storing the credential.
Data is fresh for one hour; stale reads trigger a background refresh. Failures
retain useful data and delay automatic retries for one minute. `refresh:true`
bypasses both timers. Fetches are bounded to four concurrent requests, ten seconds,
and 16 MiB each.

### Request preparation

`llm.Prepare(ctx, client, req, requireTools)` copies the request, resolves model
capabilities, adapts unsupported optional features, and injects skill guidance.
`Agent.Run` calls it each iteration. Direct MultiPass callers prepare explicitly,
as in the quick start.

Custom clients can implement `ModelMetadataProvider`; callers can also supply
`Capabilities` or call `PrepareCapabilities` themselves. Unknown facts do not
become unsupported features. Positive context budgets are clamped to the effective
model window with output headroom; `ClampContextBudget` is available separately.

Unsupported media becomes descriptive text in the outgoing copy. Optional tools
and settings may be omitted; completed tool exchanges become associated text.
Required tools and output schemas fail if explicitly unsupported. Stored messages
and settings stay intact. `AgentCallbacks.OnAdaptation` receives
`RequestAdaptation{Feature, Count, Message}`.

### OpenRouter routing and reasoning

`ModelHost` uses `provider.only` with fallbacks disabled. Other providers reject
it; Hugging Face selects hosts with its `model:host` suffix. A child given an
explicit model clears its inherited host pin unless another pin is supplied.

`openrouter.NewProvider(key, baseURL)` uses Chat Completions, as does MultiPass.
`openrouter.WithAPI(openrouter.ResponsesAPI)` selects Responses. Both support the
gateway's reasoning controls, host pinning, session ID, and reasoning replay.

`ResolveOpenRouterThinking` validates a preference and returns `Request`,
`Display`, and `Notice`. `ResolveOpenRouterRequestThinking` adapts an unsupported
saved preference for execution. Neither rewrites that preference.
`ThinkingEffortWordsFor` supplies model-aware form choices. See the
[CLI reasoning rules](CLI.md#thinking-on-openrouter).

Replay is tied to endpoint, requested model, and API dialect. Serving-provider
changes do not invalidate it. Metadata under `"openrouter"` records response
attribution and structured reasoning. `reasoning_details`, including an empty
array, takes precedence over plaintext. Responses uses `reasoning_items` with
opaque signatures/encrypted fields intact. Preserve this metadata; only the
selected representation is replayed and counted.

## The agent loop

`llm.NewAgent(client, registry, config)` handles request preparation, tool calls,
approvals, output, and continued model turns. `Run` accepts a request and optional
`*llm.AgentCallbacks`.

| `AgentConfig` field | Purpose |
|---|---|
| `MaxIterations` | Model calls per run; default 1,024 |
| `ToolTimeout`, `MaxParallelTools` | Per-tool timeout and parallelism; zero means unlimited |
| `DisableTools` | Disable all model tools, including private helpers |
| `ResponseTool` | Require a named final-response tool |
| `RequireResponseToolSuccess` | Require its successful receipt, not merely a call |
| `ArtifactStore`, `OpenArtifact` | Private output storage and optional authorized external reads |

The agent owns a derived registry with `read_transcript`, `read_artifact`, and
`list_artifacts`. The supplied registry provides `view_image`; the agent never
constructs or replaces it. Inspect the effective tools with `agent.ToolRegistry()`.
`agent.Close()` releases that view; the caller still owns its original registry,
MCP clients, and artifact store. One agent supports one `Run` at a time.

`llm.WithIterationLimit(ctx, n)` lowers a run's allowance without mutating the
agent; nested limits can only lower it.

### Callbacks and persistence

| Callback | Use |
|---|---|
| `OnContent`, `OnReasoning`, `OnComplete`, `OnError` | Display streamed output and outcomes |
| `ApproveToolCalls` | Approve each call in a batch; nil approves all |
| `BeforeToolExecute`, `OnToolStart`, `OnToolEnd` | Supply execution context and observe calls |
| `OnToolResult` | Observe durable rich results, including media/artifact parts |
| `BeforeFirstRequest` | Persist new input after successful projection, before any provider call; an error vetoes the run |
| `OnRequestProjection`, `OnIterationUsage` | Track each request's projected size and measured usage |
| `AdmitInput`, `Checkpoint`, `JournalToolBatch` | Coordinate durable peer input and recoverable tool intent |
| `BeforeToolBatch`, `AfterToolBatch`, `ContinueAfterFinal` | Validate batches, park executions, or continue provisional answers |

Direct `Agent.Run` callers own every callback. Managed parent and member runs
reserve `AdmitInput`, `Checkpoint`, and `JournalToolBatch` for the swarm's atomic
input receipts, transcript checkpoints, and tool intent. Supplying any of these
through `RunParent` or `Config.Callbacks` returns `swarm.ErrCallbackOwnership`
before the agent runs; the error names every conflicting hook.

The runtime copies host callbacks before binding its hooks. Host observers,
approvals, and execution-context hooks remain active. `BeforeFirstRequest` can
veto a member assignment before its input is saved. Host `BeforeToolBatch` runs
before the runtime's batch fence and journal; host `AfterToolBatch` runs before
member parking. An error stops the run and completed results still checkpoint.
Host `ContinueAfterFinal` runs before parent settlement or member completion
validation: returned input continues within the same iteration budget, an error
stops the run, and an empty successful return permits the runtime's final checks.
`OnComplete` fires only after those checks accept a final response.

`AgentResponse.AllMessages` contains the run's generated messages and admitted
peer input, not the initial history. Save it even on partial runs, with all content
parts intact. If using checkpoints, save only `AllMessages[PersistedMessages:]`. The
`Message` field alone is insufficient for durable replay.

Projection callbacks use zero-based iterations within each run. Usage callbacks
report that iteration's counts, zero when unavailable. `response.TokenUsage()`
returns peak input and summed output. Keep the latest projection until the same
request supplies measured input.

## Tools

Implement [tools.Tool](../tools/interface.go), or use `tools.Func`:

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

This example uses `context` and the `llm`, `messages`, `schema`, and `tools`
packages. `schema.Tool` and `schema.Params` assemble tool schemas; helpers include
`S`, `Int`, `Bool`, `Enum`, and `Array`. Use `tools.Args` for typed argument access,
especially numbers decoded from JSON.

| Optional interface | Contract |
|---|---|
| `OutputTool` | Return `ToolOutput{Text, Data, Media}`; wrappers must preserve all three |
| `UntimedTool` | Exempt a long call from the per-tool timeout, not cancellation |
| `ExclusiveTool` | Require the call to be alone in its model batch |
| `RecallTool` | Supply a stub for results reproducible on demand |
| `CoordinationTool` | Declare trusted orchestration behavior at the execution gate |
| `ContextTool` | Bind implementation and authority to another execution context |
| `ContextIndependentTool` | Declare safe independence from the current workspace |

After resolving and approving a tool, custom hosts should use
`registry.ExecuteTool(ctx, tool, args, timeout)`. It applies the timeout and gate,
rechecks that the approved handle is still registered and allowed after waiting
on the gate, preserves rich output, and returns the original error plus
`ToolExecution{Output, Invoked, ContextErr}`. Approval, artifact persistence, and
presentation remain the caller's responsibility.

Custom in-process Go tools run in the host. They must enforce their own filesystem
and network authority; registering one does not put the host process in a sandbox.

### Native setup

`NewToolRegistry` serves the tools supplied to it. Add `tools.WithNativeTools()`
to install the native constructors for Bash and the file tools, and register
`view_image`. Sandbox options configure authority separately. Hosts restoring
native tools through `LoadRegistry` must pass the option there too; CLI setup
already supplies it.

A generic registry and `Derive` install no native tools. A derived view inherits
constructors and live sandbox policy without preparing a new sandbox. Native
bindings require a source constructed with `WithNativeTools`; otherwise they
return `ErrNativeToolsRequired`.

`MarkBuiltin(name)` keeps a tool visible through derived allow-lists, execution
bindings, and skill policies. Native setup marks `view_image`. Built-ins are
excluded from `GetActiveToolLoaders`, so session persistence records only the
selected tools. A custom image tool may use the same marker.

## Shell tools

Executables implement `--schema` and `--execute <json-args>`.
[Protocol and example](CLI.md#shell-tools).

```go
registry := tools.NewToolRegistry(nil,
    tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()),
)
defer registry.Close()
if _, err := tools.LoadShellToolsWithRegistry(registry, []string{"./uppercase.sh"}); err != nil {
    return err
}
```

A process-backed tool requires a sandbox factory or explicit
`tools.WithUnsafeNoSandbox()`. Without either, loading fails before `--schema`
runs. Handle load errors before creating an agent.

Ordinary Bash uses `bash -c`: final-command status, no `errexit` or `pipefail`.
Workflow `exec` uses `bash -o pipefail -c`. Every call starts a fresh shell;
repeat `cd`, exports, and setup in each invocation. Use `set -e -o pipefail` when
all commands and pipeline stages must succeed, and handle expected failures with
`if`. External scripts retain their own shell options.

### Sandboxing in the library

The CLI's opt-in selection is separate from library construction.
`sandbox.DefaultConfig()` and `ParsePreset("")` produce temporary writes with no
network; `ParsePreset(sandbox.DefaultPresetSpec)` produces `workspace+net+git`.
[Complete policy reference](SANDBOX.md).

| Registry API | Effect |
|---|---|
| `WithSandboxFactory(factory, base)` | Install and freeze the base process policy |
| `AppendBaseReadPaths(paths...)` | Extend reads and rebuild loaded/staged Bash and shell tools transactionally |
| `WithSandboxLayer`, `SetSandboxLayer(name, layer)` | Add/replace a named layer; nil removes it |
| `SetSandboxLayerAndCommit(name, layer, commit)` | Publish only after both reconstruction and persistence succeed |
| `SandboxContext()` | Describe effective authority without environment values |
| `RunTrial(ctx, command, candidate)` | Execute a diagnostic overlay without changing registry policy |
| `ExecutionPolicy(root, grant)` | Derive a narrowed workspace policy |
| `BindExecutionContext(policy, allowedTools)` | Build a registry bound to that workspace; return omitted tools too |
| `GuardExecution` | Hold the execution gate for a custom executor |
| `BeginEnvironmentMaintenance` | Try to hold the registry's exclusive storage-maintenance gate |

Layers merge by name between base and tool declaration. Derived registries share
live layers; bound members receive the applicable `Members` policy at binding.
**Layers do not reach stdio MCP, schema discovery, or administrative Git.**
`SandboxChange` names servers still using their previous base policy.

The commit callback must not reenter the registry. On a failed save or rebuild,
previous tools and policy remain active. `SandboxLayer.Environment` materializes
managed storage per checkout or read-only scratch. Cross-process storage leases
remain the host's responsibility.

`Agent.Run` adds the current sandbox summary to its outgoing request.
`llm.WithSandboxContext` composes it for direct callers; use unaugmented history
after resolving skills. It is descriptive, not an enforcement mechanism.

### Process and trial ownership

Use `sandbox.WrapCmdManaged` / `WrapCmdWithEnvManaged` and run their idempotent
cleanup after `Start` or `Run`, including failures. Cleanup releases wrapper-owned
resources and preserves caller-owned `ExtraFiles`.

For finite commands, `sandbox.WrapFiniteCmdManaged` requires a command with its own
cancelable context. The owner still calls `Wait` and drains output. This scope is
for finite commands, not long-lived MCP servers.

Capture waits at most one second for inherited pipe writers after cancellation
or foreground exit. Incomplete capture returns `ErrCommandOutputIncomplete`, not
an ordinary `CommandError`; output truncation is a separate condition. Detached
writers are not guaranteed to be killed. Shell schema discovery has a 30-second
limit and a 1 MiB output bound.

A trial's nonzero process exit is in `TrialResult.ExitCode`; startup failures are
errors. Observations distinguish masks, private-home reads/writes, unwritable
paths, and network denial when the backend can observe them. Linux with readable
home cannot automatically collect denied operations. Inspect command output;
an empty observation is not proof of sufficient access.

### Context files and change tracking

`ReadContextFile(ctx, path, maxBytes)` returns `(absolutePath, data, error)` using
the registry's policy and safe regular-file reads. Oversized content is rejected,
not truncated. `ContextFilePaths` discovers bounded, Gitignore-aware candidates
without running Git; failed discovery does not prevent explicit reads.

`edit_file` and `write_file` return `tools.FileChanges` in `ToolOutput.Data`.
With a `ChangeTracker`, Bash includes the same payload in `CommandResult.Changes`.
`WithChangeTracker` / `SetChangeTracker` propagates it to derived/bound registries.
`worktree.NewChangeTracker` uses a private Git index/object store and reports
`Tracked:false` with a reason when observation is unavailable. `CountsUnknown`
distinguishes incomplete counts from zero.

`CaptureBaseline`, `RestoreBaseline`, and `WorkspaceChanges` support net changes
across turns. The self-contained baseline pack is limited to 64 MiB. Keep the
baseline and latest report in session-owned artifacts; close the tracker after
its users stop. Tool result deltas describe that call, not the net workspace.

## MCP servers

```go
_, err := registry.LoadMCPServer("./mcp.json#filesystem")
if err != nil {
    return err
}
```

Tools are namespaced, for example `filesystem__read_file`. Stdio servers use the
registry's base policy plus their declaration. Restarting applies current base
policy; named layers remain excluded. A failed replacement leaves the old server
running. Remote servers execute elsewhere and cannot be locally sandboxed.

`tools.NewUnsafeMCPClient(spec)` is an explicit standalone opt-out; its caller
owns and closes the client. Prefer registry loading when tools share policy.

## Derived registries

`registry.Derive(tools.AllowTools(...), tools.DenyTools(...))` creates a private
view that inherits tools, clients, and policy. Child-owned names can shadow parent
names, but parent policy still bounds access. Later child loads/skill activations
stay private. Closing a child closes only its clients; closing the parent removes
its inherited tools from descendants.

Close resources in reverse order: agent, derived registry, parent registry.
Use `subagent.ChildRegistry` for delegated work; it also excludes parent-only tools.

## Opening tools for a workspace

`tools.OpenTools` is a function with the signature
`func(context.Context, tools.ToolScope) (tools.ToolBinding, error)`. A scope carries
`Root`, `SourceRoot`, the `ExecutionGrant`, extra `ReadPaths`, `AllowedTools`, and
an optional shared `ExecutionGate`. Nil tool selection inherits; an empty slice
disables tools in the runner; patterns select names while retaining built-ins.

The binding returns a ready `Registry`, repository `Instructions`,
`ToolInstructions`, omitted-tool diagnostics, and a nonnil, idempotent `Close`.
The constructor enforces the scope or fails, releasing partial resources. The
caller closes the agent first and then the binding. Borrowed source resources
remain owned by their caller.

`tools.NativeOpenTools(source, opts...)` narrows native authority with
`ExecutionPolicy`, rebinds tools through `ContextTool`, reconnects local MCP, and
renders skill guidance. `ToolScope.SourceRoot`, when set, overrides
`ExecutionGrant.SourceRoot` for both tool paths and member-layer environment
paths. `WithNativeInstructions(loader)` supplies repository guidance under the
bound registry's read policy. Other constructors return their own tools without
native rebinding.

Validate a selection after registering host tools with
`Registry.ValidateToolSelection(allowed, llm.BuiltinToolNames())`. Compose the two
guidance fields under the host's prompt rules and clear inherited request skills
when the binding has already rendered them.

## Skills

`skills.LoadCatalog(directories)` reads skill directories.
`tools.NewSkillRuntime(catalog, registry)` registers activation tools.
`Activate(name)` enables a skill; `ActivatedSkills()` and `Restore(names)` persist
activation choices. Handle catalog/runtime errors before use.

Set `CompletionRequest.Skills` for automatic prompt injection, or compose guidance
with `catalog.RuntimeSystemPrompt(systemPrompt)`.
`skillRuntime.Derive(childRegistry)` inherits activations and policy without
reconnecting parent-owned MCP clients. New child activations remain private.

## Subagents

For an in-memory child, register
`subagent.NewTool(subagent.AgentRunner(client, registry, request, config))`.
`AgentRunner` uses the same agent loop with a derived tool view and supplied
request context. It does not provide durable sessions or background delivery;
background requests therefore finish synchronously. The CLI uses the managed
swarm runtime below instead.

`subagent.RunnerWithTools(client, open, scope, request, config, opts...)` opens a
binding per child instead of deriving a view. The child's tool selection becomes
`scope.AllowedTools`; repository and tool guidance are composed into its request,
and the agent closes before the binding. Both runners retain partial output and
usage when a run returns an error.

New children require a label of 1–80 characters. Nil `Request.Tools` inherits;
an explicit empty slice disables tools. Children cannot spawn or receive root-only
identity, theme, setup, or coordination-control tools. `WithCallbacks(factory)`
can supply approvals and display hooks.

`NewTool` limits concurrency to 32 by default; `WithMaxConcurrent` changes it.
A custom runner whose child outlives the call must return `Result.Done`, even on
cancellation, and close it when settled. `WithRuntimeScheduler` delegates admission
to an enclosing runtime. Model-facing calls cannot supply `MaxIterations`;
trusted Go hosts can.

## Swarms and workflows

The managed runtime unifies model spawns, `/spawn`, and workflow agents.
Read [WORKFLOWS.md](WORKFLOWS.md) for the task lifecycle and user-facing examples.

### Host setup

Construct `swarm.New(swarm.Config{...})` with `Store`, `Parent`, `Registry`,
`OpenTools`, `Client`, `Request`, `Agent`, and `Root`. Native hosts supply
`tools.NativeOpenTools(registry)`. Each member slice opens its binding after
acquiring the session lease and closes it before releasing that lease. This
applies to first runs, follow-ups, park/resume, and recovery. The parent retains
its existing registry; workflow steps still use native context binding.

`Parent` must implement
`sessions.CoordinationSession`; SQLite disk and memory sessions do. Disk storage
is needed for cross-process recovery. `Promote` lets a host arrange that before
coordination mutates state.

Register `runtime.RegisterParentTools(registry)`, then call
`RunParent(ctx, agent, request, callbacks, persistenceAllowed)` each turn. It binds
mail admission, checkpoints, tool intent, and settlement. Save only the response's
unpersisted suffix, then call `ParentTurnSettled(err)` with the persistence/output
verdict. Close the runtime before the parent session and registry.

`Config.Callbacks` supplies host hooks for each member slice. It follows the same
[callback ownership rules](#callbacks-and-persistence) as `RunParent`.

`PrepareMember` installs session-scoped tools on each slice under its current
lease; a member's tool selection may name them. A nil registry means tools are disabled. Bind member
tools to the supplied session, never a captured parent session. `UpdateDefaults`
changes future defaults without widening existing member authority.

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

Go `Followup` **creates a linked task but does not launch it**. Call `Agent` or
`Spawn` afterward. Model and JavaScript follow-up operations do both.

`AgentRequest` selects the brief/label, session/task, source/snapshot/context,
read-only/review mode, tools, model/host, result schema/input, and trusted call ID.
`AgentResult` returns `Value`, `Session`, `Context`, `Task`, `Execution`, `Revision`,
and `Usage`. A returned context may later be released.

Task requirements are fixed at creation: `delivered`, `reviewed`, or `applied`.
Unowned tasks default to `delivered`; create unowned editing work with explicit
`applied`. Dependencies need `done`. Review accepts reviewed research or requests
changes; editing completion uses integration.

### Budgets and final values

Defaults: 32 concurrent executions, 256 starts per run, 512 workspace slots.
`Resume` retains remaining model calls; a positive grant extends the start budget
when launch succeeds. `ResumeWithIterations` adds calls to the paused execution
without spending a start. Active workflow reservations must settle or be canceled.
Only trusted Go hosts can override `AgentRequest.MaxIterations`.

Exhaustion saves `paused` / `max_iterations` and returns `*swarm.IterationLimitError`
with IDs and used/allowed counts. It wraps `llm.ErrMaxIterations`.
`llm.IsIterationLimit` excludes joined persistence/provider errors.

A result `Schema` requires exclusive `swarm_complete({value})` when tools are
enabled, or validated JSON when disabled. Scalars and null are valid when the
schema permits them. Duplicate keys, trailing JSON, and schema violations fail.
Missing/invalid completion gets at most two corrections within the allowance;
empty unstructured finals get one. Partial work remains inspectable.

### Records, display, and recovery

Swarm records require domain `format`, key `swarm`, value `{"version":2}`.
The first coordination mutation writes it; incompatible records return
`swarm.ErrUnsupportedFormat` without altering saved files. This is separate from
SQLite schema versioning.

| Record | Source |
|---|---|
| Run, member, task, execution, mail, publication, parent turn | [swarm/state.go](../swarm/state.go) |
| Completion requirements and deferrals | [requirement.go](../swarm/requirement.go), [deferral.go](../swarm/deferral.go) |
| Workspace and snapshot | [worktree/worktree.go](../worktree/worktree.go) |
| Integration candidate and apply receipt | [integration.go](../swarm/integration.go), [apply.go](../swarm/apply.go) |
| Workflow report and step | [workflow/workflow.go](../workflow/workflow.go) |

Coordination changes and transcript receipts commit together under the lease.
Workflow steps have separate rows. Family rows and artifact pins cascade on root
deletion/expiry; Git workspaces do not. Pinned children do not independently expire.
Use task/execution provenance to locate result code: a member's current context
may have been released or replaced.

`swarm.Present` produces typed lifecycle, execution, task, decision, and control
fields. Use these for logic; `Display` is human text. Idle does not imply accepted
or integrated. `ReadStateView` uses a trusted read-only store without opening a
live runtime or taking a lease.

### Workflow host and contexts

`RunWorkflow` waits for a saved report. `StartWorkflow` returns an ID and detaches
from caller cancellation; runtime shutdown still stops it. Terminal foreground
results are acknowledged only when their matching tool result or artifact
reference commits. Failed saves leave the notice available. Background and direct
Go launches deliver through notices. Failed/canceled/interrupted reports require
acknowledgment or explicit deferral; neither accepts their tasks.

The independent `workflow.Runner{Host, Config}` accepts a trusted host with `Call`
and optional `Recorder.SaveWorkflow`. Teardown drains host calls and records late
apply receipts. JavaScript is never automatically replayed.

`ExecutionContext` binds root, scratch, read-only state, and narrowed policy.
Tools must implement `ContextTool` or declare `ContextIndependentTool` to survive
rebinding. Stdio MCP relaunches in context; remote MCP needs
`contextIndependent:true`. Indexed semantic search is omitted from members.

## Integration

`Runtime.Integrate` accepts exactly one of exact task revisions or a candidate ID.
`Drift` can be `paths` (default) or `tree` when preparing tasks, not when applying
an existing candidate. The result carries status, candidate, tasks, unchanged
state, receipt, and next action. Conflicts return a repairable candidate in the
structured error.

| Method | Purpose |
|---|---|
| `PrepareIntegration` | Build an ordered candidate; stop on the first conflict |
| `ReadIntegration` | Inspect the current candidate and receipt |
| `ReviseIntegration` | Adopt a repair based on the exact intermediate state |
| `RefreshIntegration` | Rebase a ready candidate against current parent files; report `Changed` |
| `AcceptIntegration`, `ApplyIntegration` | Stepwise acceptance/application |
| `ReconcileApply` | Observe an interrupted outcome without replaying its patch |

Applied receipts make retries idempotent, including after snapshot cleanup.
Unchanged outcomes need explicit filesystem proof. Uncertain writes block new
integration until reconciled. Apply defaults to two minutes, followed by bounded
outcome recording; runtime shutdown waits for both.

`paths` drift permits unrelated parent edits; `tree` requires the complete parent
tree to match. Validate merged candidates in disposable contexts before applying.
See [integration and repair](WORKFLOWS.md#integrating-editing-results).

## JavaScript API

Define exactly one `polly.workflow(name, inputSchema, run)`, or its object form
`polly.defineWorkflow({name, inputSchema, run})`. Put effects inside `run`, return
JSON-compatible output, and await every operation.

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

Agent options include `input`, `schema`, `tools`, `model`, `modelHost`, `readOnly`,
`review`, `source`, `commit`, `context`, `session`, and `taskID`. Tools cannot widen
inherited authority. A full local Git commit ID selects that exact commit;
omitting it captures current eligible dirty/untracked files.

Public candidates always have `receipt`: null before an apply attempt, otherwise
an object whose status is `applied`, `not_applied`, `applying`, or
`recovery_required`. Null alone proves nothing about parent contents.

Disposable contexts are for checks: release may remove their contents, and they
cannot be snapshotted or used as agent/copy sources. Ordinary contexts with
unintegrated changes remain retained. Scope defaults apply to work methods;
follow-up and integration still require explicit arguments.

Schemas reject extra object keys by default. `schema.keyed(ids, valueSchema)`
requires unique string IDs and every corresponding key. Pending host work on
return fails the attempt. A promise with nothing capable of settling it also
fails. Serialization cannot initiate effects.

The VM has no Node, module, filesystem, network, process, or timer APIs. Defaults
are five seconds per uninterrupted JS slice, 4,096 host calls, and 512 stack frames;
waiting on host work does not spend the slice budget. There is no hard heap limit.
Runtime authority and process sandboxing enforce external effects.

For complete signatures, errors, and examples, use the shipped
[workflow reference](../swarm/workflow_help.md) or `workflow_help()`.

## Sessions

`sessions.OpenStore(StoreConfig)` opens the same SQLite implementation in memory
or on disk. Disk mode needs an explicit literal path; expand `~` yourself.
The CLI uses `~/.pollytool/polly.db`.

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

`Metadata.Name` is the resume handle; `Title` is a descriptive label.
The optional `TitleSession.SetTitle(ctx, title, source)` normalizes whitespace,
rejects controls/empty text, and limits titles to 80 Unicode characters. Duplicate
titles are allowed. Agent writes cannot replace a user title (`ErrTitleProtected`).
`SetMetadata`, `Clear`, and `Reset` preserve title ownership; use `SetTitle` to
change it. `sessions.DisplayLabel` chooses title, child task, then handle.

`ExtraReadDirs` stores canonical session read grants and survives reset/clear.
`SpawnCallID` identifies the originating delegation; `SpawnOutcome` records only
the initial run. Follow-ups do not rewrite that outcome. `Metadata.Parent` is a
display name; stable `ParentID` is the ancestry key.

### Read-only views

`SQLiteStore.ReadView(ctx, ViewTarget, knownRevision)` returns identity, revision,
metadata, history, lease state, and a read-only artifact store without acquiring
a lease or updating last-used time. An unchanged revision omits history. Target
by name, stable ID, or parent/spawn-call pair; use `ExpectedID` when acquiring the
viewed session for a write.

`sessions.ViewStore`, `ViewIdentity`, and `CoordinationViewStore` are optional
capabilities. Coordination views inspect saved family state without granting model
tools access to another family. Missing/expired identities are refused. Views do
not extend retention; reset/deletion can remove their artifacts, and store shutdown
cancels readers. Writes return `ErrReadOnlyView`.

### Artifacts

`session.ArtifactStore()` is scoped to that session; SQLite commits bytes in the
same database as transcripts. Pass it to `AgentConfig.ArtifactStore`.

`OpenArtifact` optionally authorizes references absent from the conversation.
It returns matching `artifacts.Ref` metadata and an `io.ReadCloser` positioned at
zero; `read_artifact` closes it. For swarms, bind
`CoordinationSession.OpenPublishedArtifact` to the actual member's session.
A nil callback limits reads to conversation references. This does not expand
`list_artifacts` or permit unpublished peer reads.

## Errors, concurrency, and ownership

Provider streams emit error events; `Collect` and `Agent.Run` return errors.
Keep partial agent output before reporting failure. Tool failures use structured
`*tools.ToolError`.

`tools.CommandError` means an ordinary, fully captured target exit. Sandbox setup,
approval, cancellation, timeout, and incomplete capture are distinct failures.
Workflow `exec(command, {check:false})` recovers only ordinary exits. A denial inside an
already started shell is still that shell's exit; classification does not guess
intent from stderr.

- Reuse `MultiPass` and `SQLiteStore` concurrently. Run one turn per agent.
- Tool implementations must tolerate parallel calls. `UntimedTool` alone does
  not bypass coordination gates.
- Session mutations are transactional; multi-call read/modify/write needs host
  coordination. Use the lease context for work tied to ownership.
- Close agents before their registries; stop runtimes and trackers before closing
  sessions/stores. `Runtime.Close` waits for integration and outcome recording.
- Custom stores must preserve leases, stable identity, atomic coordination and
  transcript updates, and artifact ownership. Implement the optional capabilities
  your host actually uses.
