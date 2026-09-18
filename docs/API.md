# Pollytool as a Library

Pollytool's CLI is a thin layer over Go packages you can use directly: one
streaming interface over seven LLM providers, plus tools, sandboxing,
skills, sessions, and structured output.

**Contents:** [Quick Start](#quick-start) ·
[Core Types](#core-types) · [Providers](#providers) · [Tools](#tools) ·
[Shell Tools](#shell-tools) · [MCP Servers](#mcp-servers) ·
[Skills](#skills) · [Sessions](#sessions) ·
[Structured Output](#structured-output) · [Error Handling](#error-handling) ·
[Thread Safety](#thread-safety)

## Quick Start

```bash
go get github.com/alexschlessinger/pollytool
```

```go
import (
    "github.com/alexschlessinger/pollytool/llm"
    "github.com/alexschlessinger/pollytool/messages"
)

// One router for every provider; keys are indexed by provider name.
client := llm.NewMultiPass(map[string]string{
    "openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
    "anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
})

req := &llm.CompletionRequest{
    Model:     "openai/gpt-5.4",
    Messages:  messages.User("Tell me a joke"),
    MaxTokens: 500,
}

// One-shot: the final text of the completion
joke, err := llm.Collect(ctx, client, req)

// Streaming
for event := range client.ChatCompletionStream(ctx, req, messages.NewStreamProcessor()) {
    switch event.Type {
    case messages.EventTypeContent:
        fmt.Print(event.Content)
    case messages.EventTypeError:
        return event.Error
    }
}
```

Provider names are `openai`, `anthropic`, `gemini`, `ollama`, `huggingface`,
`deepseek`, and `openrouter`. Create the router once and reuse it. Conversation
history is the `Messages` slice; structured output is `ResponseSchema`
([Structured Output](#structured-output)); tool loops run through `llm.NewAgent`
([Tools](#tools)).

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
constructs provider clients per call and shares a scoped metadata cache.

```go
multipass := llm.NewMultiPass(map[string]string{
    "openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
    "anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
})
// Pass gemini, ollama, huggingface, deepseek and openrouter keys the same way.

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

### Model metadata and request capabilities

`MultiPass.ListModels(ctx, ModelTarget, refresh)` lists a provider catalog;
`LookupModel` retrieves a model and its advertised routes. `Agent` exposes the same
methods. `ModelTarget` takes `Provider`, a bare `Model` ID, optional `Host`,
`BaseURL`, and `APIKey`. Omitted credentials use inference's effective runtime key.
`UseConfiguredKey: true` with an empty `APIKey` bypasses the process override for
a preview of clearing it. Explicit preview keys do not change inference credentials;
neither credential field is serialized.
`GetModelInfo` provides the optional `ModelMetadataProvider` interface.
`DiscoverModelContextWindow` remains a compatibility wrapper over this service.
Each provider package fetches and decodes its own catalog (`openai.ListModels`,
`anthropic.ListModels`, `openrouter.ListModels`, and so on; Hugging Face's router
is served by `llm/openai`); the provider table in `llm/multipass.go` wires them and
holds the routing rules, so the caching service above is provider-neutral.

```go
multipass.SetModelMetadataCache(store) // optional: *sessions.SQLiteStore implements this
catalog, err := multipass.ListModels(ctx, llm.ModelTarget{Provider: "openrouter"}, false)
detail, err := multipass.LookupModel(ctx, llm.ModelTarget{
    Provider: "openrouter", Model: "org/model", Host: "upstream/routing-id",
}, false)
_ = catalog
_ = detail
_ = err
```

The optional store interface has `GetModelCache(context.Context, string) ([]byte, error)`
and `PutModelCache(context.Context, string, []byte) error`. Entries are separate from
conversation history. The additive SQLite v7 migration creates this table for disk
and memory stores. Scope hashes include provider, effective endpoint, credential,
model, and host; no credential is serialized. Freshness is one hour. Stale data is
returned immediately while a bounded refresh runs. Failures retain successful data
and suppress automatic attempts for one minute; `refresh=true` bypasses freshness
and failure cooldown. Concurrent reads coalesce. HTTP work is limited to four
concurrent fetches, ten seconds per fetch, and 16 MiB per response. Caller
cancellation stops uncached reads; background stale refreshes have their own deadline.

`ModelCatalog` reports `Source`, `FetchedAt`, `Partial`, `Stale`, and `Error`.
Partial catalogs may contain usable records. Model and endpoint facts stay separate:
identity, display text, lifecycle, input/output modalities, token limits, tools,
structured output, reasoning choices, parameter declarations, sampling metadata,
image constraints, pricing, and performance. Provider additions are inspectable in
bounded `Raw` records. Descriptions are display data and never enter model prompts.
Nil capability pointers/lists mean unknown; explicit false, zero, and empty lists
remain distinct. `UnlimitedLimits` is an explicit declaration keyed by normalized
limit name, never inferred from absent/zero fields. `LimitsApplyToAllRoutes` marks
an explicitly applicable model-wide limit; catalog maxima do not set it. Missing price units remain
unknown; normalized `Prices` retain their currency, amount, basis, and conditions.

Discovery uses only provider APIs: OpenAI and DeepSeek Models; paginated Anthropic
and Gemini Models; Ollama tags and lazy show (no downloads); Hugging Face router
model/detail records; OpenRouter models and lazy endpoints. Source contracts:
[OpenAI](https://platform.openai.com/docs/api-reference/models),
[Anthropic](https://platform.claude.com/docs/en/api/models/retrieve),
[Gemini](https://ai.google.dev/api/models),
[Ollama](https://docs.ollama.com/api/show),
[DeepSeek](https://api-docs.deepseek.com/api/list-models),
[Hugging Face](https://huggingface.co/docs/inference-providers/hub-api),
[OpenRouter](https://openrouter.ai/docs/api/api-reference/endpoints/list-all-endpoints-for-a-model).

`CompletionRequest.ModelHost` pins an OpenRouter routing ID using `provider.only`
with fallbacks disabled. Empty means Automatic. Other providers reject this field;
Hugging Face uses its existing `model:host` suffix. An explicit child model clears
an inherited pin unless another pin is supplied (`subagent.Request.ModelHost`,
spawn tool `model_host`, or workflow `modelHost`).

`Agent.Run` prepares a copied outgoing request before every provider call with
`Prepare(ctx, client, req, requireTools)`, which resolves the model's
capabilities, adapts the copy, records the capabilities on it, and injects the
skill prompt. `MultiPass` is a pure router: callers that stream through it
directly call `Prepare` themselves.
Custom clients can implement `ModelMetadataProvider`, or callers can set
`CompletionRequest.Capabilities` to normalized authoritative facts. Without either,
capabilities stay unknown. `ModelInfo.EffectiveCapabilities(host)` resolves endpoint
overrides and conservative Automatic guarantees. Positive `MaxContextTokens`
values are capped to the effective model window with output headroom; zero means unlimited.
Callers wanting a model-derived budget can use `ContextWindow()` and
`ClampContextBudget` to reserve output headroom before setting the request budget.

`PrepareCapabilities` also exposes adaptation independently. Unsupported media is
replaced with text identifying what the model could not view, without hydrating
or rewriting stored image parts. Optional tools and unsupported settings are
omitted; completed tool protocol exchanges become associated text. An explicitly
unsupported response schema or required successful response tool is an error.
Anthropic response schemas use a tool fallback when tool calling is available,
independently of native structured-output support.
Reasoning choices are validated only for complete declarations. Diagnostics are
returned by `Prepare` and delivered to `AgentCallbacks.OnAdaptation` as
`RequestAdaptation{Feature, Count, Message}`. Accounting and shape caches use the
adapted projection. No retry, model switch, or image batching occurs.

OpenRouter lives in `llm/openrouter`. It rides on the `llm/openai` transport in
either dialect: `openrouter.NewProvider(key, baseURL)` speaks Chat Completions
(what `MultiPass` wires), and `openrouter.WithAPI(openrouter.ResponsesAPI)`
selects the stateless Responses endpoint. Both carry the gateway's extensions:
the unified `reasoning` control, `provider.only` routing, `session_id`, and
reasoning replay. Replay is recorded per dialect, so a reply made over one is
not replayed over the other.

OpenRouter merges the cached model catalog's reasoning policy
with endpoint facts. Missing endpoint fields cannot erase model-wide policy;
route-specific tool/parameter checks remain conservative. `ModelCapabilities`
adds optional `ReasoningMandatory`, `ReasoningDefaultEnabled`,
`ReasoningDefaultEffort`, and `ReasoningMaxTokens` facts. `ReasoningPolicy` marks
explicit gateway policy. `ReasoningEffortsComplete=false` means unknown/partial;
when true, a nil effort list means unrestricted and a non-nil list (including an
empty one) is authoritative. Persisted discovery caches are refreshed under a new
cache identity; this requires no database migration.

`ResolveOpenRouterThinking(preference, capabilities)` returns the same `Request`,
`Display`, and `Notice` used by execution and settings. It never edits the saved
preference. Mandatory thinking plus `off` selects the lowest supported effort;
if the minimum is unknown, the request omits controls and reports use of the
provider default. Optional thinking plus `off` sends `reasoning.enabled=false`.
Unknown policy plus `off` omits controls and labels the effective setting unknown.
`dynamic` uses provider defaults. Unsupported explicit efforts fail before
generation with valid choices; unknown support sends the explicit effort as-is.
The unified `reasoning` object sends named efforts unchanged (including `max`),
or raw budgets as `max_tokens`. Other provider mappings are unchanged.
`Agent.CachedModelInfo(target)` and `OpenRouterThinkingWords(capabilities)` support
nonblocking UI display and completion. Reasoning adaptation notices are emitted
once per turn and resolved setting, including worker and workflow agent runs.

New OpenRouter assistant messages store diagnostic/replay data under
`ChatMessage.Metadata["openrouter"]`:

```json
{
  "endpoint": "https://openrouter.ai/api/v1",
  "requested_model": "z-ai/glm-5.3-flash",
  "response_id": "gen-example",
  "model": "z-ai/glm-5.3-flash",
  "provider": "response-supplied serving provider",
  "reasoning_details": []
}
```

`response_id`, `model`, and `provider` are optional, populated only from the current
response (including choice-free first/final streaming chunks). Endpoint identity
excludes credentials, query parameters, and fragments. Replay is bound to endpoint
and requested model, not the routed upstream. Structured `reasoning_details` take
precedence whenever present, including `[]`; otherwise `ChatMessage.Reasoning`
supplies plaintext replay. A reply made over the Responses dialect records
`reasoning_items` instead: every reasoning output item verbatim (encrypted
content, or the signature and format some upstreams use), passed back untouched
ahead of that assistant turn on the next request. Text/summary fragments are reassembled in order, with
signatures and opaque fields retained; encrypted blocks stay separate. Duplicate
plaintext display is not replayed or counted alongside structured details.
Context projection retains complete blocks with their assistant/tool exchange;
request fingerprints use the selected representation. Old reasoning lacking
origin remains inspectable but cannot be replayed. Storage/serialization and
tool argument bytes are unchanged; no raw response capture or transcript rewrite
is added. The opt-in `TestOpenRouterLiveToolRoundTrip` smoke test runs a bounded
GLM tool round-trip in both modes with `POLLYTOOL_OPENROUTER_LIVE_TEST=1` and
`POLLYTOOL_OPENROUTERKEY` set. It reports a skipped live replay assertion if the
serving provider returns no reasoning; tool execution and persisted attribution
are checked first. Wire contract: [OpenRouter reasoning controls and replay](https://openrouter.ai/docs/guides/best-practices/reasoning-tokens).

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

`llm.NewAgent` is the easy path: `Agent.Run` executes each call the model makes,
feeds results back, and returns the final answer. To own the loop (logging,
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

The built-in Bash tool runs `bash -c` and reports the final process
exit status. Pipelines use the last command's status. Parent and worker commands and workflow
`exec` share these defaults. External shell tools and separately launched scripts
retain their own shell options.

Each built-in Bash invocation starts a fresh shell. Changes made by `cd`,
exports, shell variables, and shell options do not persist between invocations.
Repeat required directory and environment setup in each command, or source a
setup file within that invocation. Use supplied writable scratch or
temporary paths for tool caches and disposable build output.
Treat sandbox permission failures as environment limits; do not change ownership,
persistent user configuration, or project code to bypass them.

Use `set -e -o pipefail` when every command and pipeline stage must succeed, and
handle expected failures with `if`. Account for intentional early-reader
termination when enabling `pipefail`. Conditional-list exceptions remain:
`false && printf unreachable; printf later` succeeds. Run required checks
separately or propagate failures explicitly; use `go test -count=1` for mutation
tests to avoid cached results.

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

agent := llm.NewAgent(client, registry, llm.AgentConfig{})
defer agent.Close()
result, err := agent.Run(ctx, &llm.CompletionRequest{
    Model:    "openai/gpt-5.4",
    Messages: messages.User("Uppercase 'hello world'"),
}, nil)
```

### Reading composer context files

`registry.ReadContextFile(ctx, path, maxBytes)` returns `(absolutePath, data,
error)` for a complete regular file. It uses the same path resolution, read
policy, and safe-open rules as the native file tools; oversized files fail
without truncation. The caller chooses the positive byte limit and interprets
the returned bytes. This helper does not activate skills or grant permissions.

`registry.ContextFilePaths(ctx, root)` returns workspace-relative completion
candidates from a bounded in-process walk. It reads `.gitignore` files directly,
applies nested rules and negations, and excludes `.git` itself. Candidates are
filtered against the registry read policy; discovery does not require Git, `rg`,
or a process-enabled registry. Discovery failure does not prevent explicit `ReadContextFile` reads.

Managed-TUI reference messages use existing text/image parts plus versioned
`polly_composer_v1` message metadata to preserve editable draft text and explicit
skill names. File parts carry `FileName` and `Reference`; text includes its file
label and boundaries. Applications should preserve this metadata and these
parts when saving/restoring messages. No database migration is required.

### Sandboxing in the library

[SANDBOX.md](SANDBOX.md) is the policy reference — every `"sandbox"`
field, the merge rules, and platform behavior. The library-only corners:

- **Base config.** `sandbox.DefaultConfig()` is the base policy;
  `sandbox.ParsePreset("workspace+net+git")` builds the CLI-style presets.
  The home directory is a private root: `ParsePreset` adds
  `sandbox.HomeToolchainGrants()` (Git configuration with its includes, the
  install prefixes of `PATH` entries under home) while `DefaultConfig()` does
  not, so a registry built on it sees nothing under home until you add
  `ReadPaths`. `sandbox.ReadAllowed` and `WriteAllowed` apply the same
  deepest-rule policy in-process; `ExecutionPolicy` hands members the
  parent's read and Unix-socket grants, explicit credential grants included,
  less any the parent's or the member's denied paths cover. `sandbox.DeniedBy` is that
  test: `ReadMasked`'s route matching without the credential list.
- **Changing a live policy.** `registry.AppendBaseReadPaths(paths...)`
  adds read grants to the base mid-session (polly's `/add-dir`). Before
  it returns, it rebuilds the loaded bash and shell tools under the new
  policy, all or nothing. The `tools.SandboxChange` it returns names those
  tools and the running stdio MCP servers, which keep the policy they
  started with. Load tools and change the policy from one goroutine: a
  load that overlaps a change may be built under either policy.
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
- **Finite commands.** `sandbox.WrapFiniteCmdManaged` accepts a command created
  with `exec.CommandContext`, applies the same descriptor ownership rules, and
  owns its cancellation policy. Call cleanup immediately after `Start`, even
  when startup fails. It stops the command's private Unix process group, or the
  built-in Linux sandbox's bubblewrap process and PID namespace. Other platforms
  stop the direct process. Use the existing wrapping APIs for long-lived MCP
  transports. This helper sets cancellation scope; callers still own `Wait` and
  output draining.

Bash, shell-tool execution, shell schema discovery, and indexed-search commands
share a finite-command runner. It captures output while the foreground process
runs and allows one second to drain after cancellation or observed foreground
exit, whichever comes first. If a descendant still holds an output pipe, the
runner closes its capture reader and returns the captured prefix with an error
matching `tools.ErrCommandOutputIncomplete` through `errors.Is`. Cancellation
and deadlines preserve their context error; descriptor/setup failures take
precedence over capture failures, which take precedence over ordinary exits.
`tools.CommandError` requires target startup and complete capture, so workflow
`exec(check: false)` cannot suppress incomplete-capture errors.
The existing output-size limits still discard excess bytes while draining;
size truncation is separate from incomplete capture. Shell schema discovery
retains its 30-second execution timeout and 1 MiB output limit.

Cancellation immediately stops the owned command scope. Once foreground exit
is observed, drain expiry does not send signals to background jobs; closing a
reader can still cause a later background write to fail with `SIGPIPE`. Jobs
that redirect their output retain existing background behavior where the
sandbox allows it. Deliberately detached sessions are outside the Unix
process-group termination guarantee, but cannot hold capture open indefinitely.

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
worker := registry.Derive(tools.AllowTools("read_file", "list_dir", "git__*"),
    tools.DenyTools("git__push"))
agent := llm.NewAgent(client, worker, llm.AgentConfig{})
defer agent.Close()  // releases the agent's private built-ins
defer worker.Close() // releases only what the worker loaded itself
```

A derived registry is a full registry of its own: tools it registers or
loads are private to it and shadow the parent's, its skill policy and
always-allowed set are its own (the allow-list bounds everything but those
built-ins), and a parent tool stays subject to the parent's policy too.
Closing the parent empties every registry derived from it. The sandbox
policy is the parent's own rather than a copy: a directory the parent adds
later with `AppendBaseReadPaths` reaches the registries derived before it,
and a derived registry refuses `AppendBaseReadPaths` itself.

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
required short label for new agents, a tool allow-list, and an optional model override.
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
`ChildRegistry` excludes `set_session_title`, `set_theme`, `spawn_agent`, `swarm_*`, `workflow_*`, `list_agents`,
`send_message`, and `read_messages`, even when the parent registers them later.
Those tools carry the parent's identity — `set_theme` restyles the parent's own
screen — and cannot be inherited by a lightweight child. Use the swarm runtime
to bind a member's own identity.

## Swarms and workflows

The CLI/TUI uses one `swarm.Runtime` for model spawns, `/spawn`, and workflow
agents. Standalone `llm.Agent` and `subagent.AgentRunner` remain available without
it. Start with [WORKFLOWS.md](WORKFLOWS.md) for coordination patterns.

### Host setup

`swarm.New(swarm.Config{Store, Parent, Registry, Client, Request, Agent, Root})`
creates one parent's runtime. `Parent` implements `sessions.CoordinationSession`;
SQLite memory and disk stores do. Disk storage is required for cross-process
recovery; `Promote` lets a host arrange it before coordination mutates state.
Close the runtime before the parent session and registry.

Register parent tools once with `RegisterParentTools(registry)`, then call
`RunParent(ctx, agent, request, callbacks, persistenceAllowed)` for each turn.
It binds mail admission, progressive persistence, interrupted-tool journaling,
and settlement to copied callbacks. Persist only
`response.AllMessages[response.PersistedMessages:]` afterward and call
`ParentTurnSettled(err)` with the persistence/output verdict. `BeforeFirstRequest`
keeps its existing persistence veto. An `llm.Agent` without these callbacks still
returns the whole response for its caller to persist once.

`MemberToolNames` and `PrepareMember(ctx, session, registry)` support session-scoped
tools. The hook runs on each execution slice with the member's current lease;
a nil registry means tools are disabled. Bind to that member session, not a
captured parent session. Returned guidance is request-only and omitted for
tool-free structured output. `UpdateDefaults` refreshes parent settings safely;
it does not change existing members' model or authority.

### Runtime methods

Methods are on `*swarm.Runtime` unless marked as package functions. Go hosts are
trusted; model authority is bound in registered closures, never caller-supplied IDs.

| Area | Methods | Contract |
| --- | --- | --- |
| Parent lifecycle | `RegisterParentTools`, `RunParent`, `ParentTurnSettled`, `ParentState`, `UpdateDefaults`, `Close` | Checkpoint and settle parent turns; close waits for active applies and their receipts. |
| Launch | `Agent(ctx, controller, AgentRequest)`, `Spawn(ctx, subagent.Request)` | Shared scheduler, tasks, budgets, and workspace policy; a nonempty controller reserves a workflow member. Ordinary Go hosts use an empty controller. |
| Follow-up creation | `Followup(ctx, controller, FollowupRequest)` | Creates a linked `*Task` on the completed task's member. **Does not launch it**; the caller uses `Agent` or `Spawn`. Model/JS follow-up operations perform both steps. |
| Execution control | `Resume(ctx, memberID, grant)`, `ResumeWithIterations(ctx, memberID, additional)`, `StopMember`, `HasActive` | Resume remaining calls or explicitly grant more; stop records `stopped`. `HasActive` reports active members/workflows, excluding background release; `Close` still joins the release worker. |
| Tasks | `CreateTask`, `ReadTask`, `Claim`, `Submit`, `Review`, `UpdateTask`, `BlockTask`, `CancelTask` | Exact revisions, ownership, dependencies, and immutable completion requirements. |
| Sharing | `Send`, `Publish` | Addressed mail and explicitly published family knowledge. |
| Completion | `Integrate`, `Settle` | Integrate editing revisions; settlement preserves every unresolved obligation. |
| Integration repair | `PrepareIntegration`, `ReadIntegration`, `ReviseIntegration`, `RefreshIntegration`, `AcceptIntegration`, `ApplyIntegration`, `ReconcileApply` | Supported advanced inspection, repair, and recovery operations; see below. |
| Workflows | `RunWorkflow`, `StartWorkflow`, `CancelWorkflow`, `SaveWorkflow`, `AcknowledgeWorkflow`, `DeferWorkflow` | Execute one fresh attempt, record receipts, and handle terminal reports. |
| Resources | `Cleanup(ctx, contextID)`, `Forget(ctx)` | Proof-based inactive cleanup; empty context selects the family. Forget additionally removes snapshot refs after obligations resolve. |
| State | `State`; package `ReadStateView`, `MemberState`, `Present`, `StatusCounts`, `DecisionCounts`, `FirstDecision`, `TaskStatus`, `TaskStatusIn`, `TaskDeferred`, `DeferredCount` | Typed reads and derived presentation; `ReadStateView` needs no live runtime or lease. |

`AgentRequest` has `Task` (brief), `Label`, `Session`, `TaskID`, `Source`, `Snapshot`,
`Context`, `ReadOnly`, `Review`, `Tools`, `Model`, `ModelHost`, `MaxIterations`, `Schema`, `Input`,
and host `CallID`. `Session` continues a member's conversation with inherited
authority. `Source`, `Snapshot`, and `Context` select input for a new isolated copy.
An empty non-nil `Tools` disables tools; nil inherits compatible parent tools.
`AgentResult` carries `Value`, `Session`, `Context`, `Task`, `Execution`, `Revision`,
and per-execution `Usage`. A returned context can subsequently be released.

`CreateTask(ctx, description, criteria, deps, owner, options...)` accepts
`CreateTaskOptions{Review, Requirement}`. Read-only defaults to `delivered`,
`Review:true` selects `reviewed`, and editing requires `applied`. Unowned tasks
default to `delivered`; use an explicit `applied` requirement for unowned editors.
Claims and reassignment refuse incompatible authority. `Review` accepts reviewed
research or requests changes on a submitted task; editing acceptance uses `Integrate`,
and delivered tasks use follow-ups. Dependencies require `done`, not cancellation.

`FollowupRequest{Task, Question, Snapshot, Label, Background, CallID}` inherits
the original requirement and owner without rewriting the old task. Read-only work
defaults to its starting snapshot; editing defaults to its submitted snapshot.
An explicit known snapshot refreshes only the new task. Non-Git research retains
its absolute live root. Missing provenance refuses restoration; current code is
never a fallback. `CallID` makes task creation idempotent for the same host call.

Defaults are 32 concurrent executions, 256 starts per run, and 512 workspace slots.
`Resume` preserves remaining calls; positive `grant` extends the separate start
budget when its launch succeeds. `ResumeWithIterations` adds calls to the same
paused execution without spending a start. Completed/failed executions resume as
new turns. Active workflow reservations must settle or be canceled first.
`AgentRequest.MaxIterations` is a trusted host override, forbidden in JavaScript.
Iteration exhaustion returns `*swarm.IterationLimitError`, wrapping
`llm.ErrMaxIterations`, with IDs and used/allowed counts, and saves `paused` with
`max_iterations`. `llm.IsIterationLimit` excludes joined persistence/provider errors.

`Schema` is a final-value contract. Tool-enabled agents finish through exclusive
`swarm_complete({value})`. Agents without a result schema return an ordinary final
answer, captured automatically.
Duplicate JSON keys, trailing JSON, and schema violations fail validation. Empty
schemas, scalars, and JSON null are supported. Missing/invalid completion gets two
corrective continuations within the original allowance, then fails with partial
work. Tool-free agents use validated JSON with the same limit. A committed
`StructuredCompletion` can finalize on explicit resume without another model call.
Publications are progress; task completion still needs its requirement's evidence.

`llm.AgentConfig.RequireResponseToolSuccess` enables receipt-based completion for
`ResponseTool`; naming the tool is insufficient. `ContinueAfterFinal` owns corrective
input in that mode, without the legacy reminder. The option defaults to false.
Unstructured empty finals get one persisted corrective continuation, then
`*swarm.EmptyResultError` wrapping `swarm.ErrEmptyResult`. Denied tools or failed
response tools do not trigger another approval attempt. Text content parts count;
media-only and successful response-tool finals reference the saved member session.

### Format 2 records

Each root with swarm data has domain `format`, key `swarm`, value `{"version":2}`.
The first coordination mutation writes it; opening/reading a new root does not.
A standalone parent-turn journal does not require a swarm format. Actual swarm
records with a missing or different version return `swarm.ErrUnsupportedFormat`
from `New`, `State`, and `ReadStateView`. There is no migration; records, transcripts,
artifacts, and worktrees remain, and new delegation uses a new root session.

SQLite schema versioning is separate (currently v6). Coordination uses domain/key
JSON rows, family membership, and artifact pins; affected records and transcript
receipts commit together under the lease. Workflow steps have separate rows and
are reattached to reports on read. Rows/pins cascade with parent deletion or TTL;
Git workspaces/refs do not. Pinned child sessions do not independently expire.
`sessions.DurableStore.Promote(ctx, path)` preserves handles, IDs, cache identity,
and artifacts while retaining unrelated existing disk sessions.

The table uses JSON names; `?` marks an `omitempty` tag. The exported Go types in
[swarm/state.go](swarm/state.go) and linked files are the exact serialization source.

| Type / domain | Fields |
| --- | --- |
| `FormatRecord` / `format` | `version` |
| `Run` / `run` | `id`, `status`, `starts`, `limit` |
| `Member` / `member` | `id`, `name`, `label`, `control?`, `controller?`, `context?`, `tools`, `model`, `task?`, `execution?`, `readOnly` |
| `Task` / `task` | `id`, `run`, `description`, `criteria`, `dependencies`, `owner?`, `status`, `revision`, `acceptedRevision?`, `result?`, `feedback?`, `snapshot?`, `requirement?`, `delivery?`, `follows?`, `followupCallID?`, `startingSnapshot?`, `sourceRoot?`, `execution?`, `deferral?` |
| `Execution` / `execution` | `id`, `run`, `member`, `status`, `request`, `iterations`, `generation`, `inputSaved`, `intent?`, `usage`, `result?`, `error?`, `stopReason?`, `workspace?`, `base?`, `sourceRoot?`, `workflow?`, `emptyFinalRetried?`, `resultCorrections?`, `pendingResultCorrection?`, `completion?` |
| `ExecutionContext` / `context` | `id`, `owner`, `root`, `readOnly`, `checkout?`, `scratch?`, `release?`, `reason?` |
| `Mail` / `mail` | `id`, `from`, `to`, `kind`, `replyTo?`, `replyID?`, `text`, `delivered`, `posted`, `task?`, `revision?`, `execution?`, `workflow?` |
| `Publication` / `publication` | `id`, `author`, `run`, `text`, `supersedes?`, `snapshot?`, `artifacts?`, `sources?`, `posted` |
| `ParentTurn` / `parent_turn` | `intent?` |
| [`worktree.Snapshot`](worktree/worktree.go) / `snapshot` | `id`, `commit`, `tree`, `source` |
| [`IntegrationCandidate`](swarm/integration.go) / `integration` | `id`, `run`, `status`, `inputs`, `repairs`, `pending`, `parent`, `merged`, `conflicts`, `drift`, `plan`, `accepted`, `predecessor?`, `successor?`, `created`, `receipt?` (hydrated from apply records on read) |
| [`ApplyRecord`](swarm/apply.go) / `apply` | `id`, `tasks`, `plan`, `observedParent`, `status`, `error?`, `started`, `finished?` |
| [`workflow.Report`](workflow/workflow.go) / `workflow` | `id`, `name`, `source`, `input`, `output?`, `status`, `steps`, `error?`, `started`, `finished?`, `callID?`, `run?`, `acknowledged?` |
| `workflow.Step` / `workflow_step` | Operation `id`, `kind`, `args`; `status`, `value?`, `error?`, `started`, `finished?` |
| `worktree.Preview` / `preview` | `id`, `parent`, `candidate`, `merged`, `checkout`, `conflicts?`; retained previews without task revision provenance cannot authorize integration. |

`State` maps these domains to `Runs`, `Members`, `Tasks`, `Executions`, `Contexts`,
`Messages`, `Publications`, `ParentTurns`, `Snapshots`, `Integrations`, `Applies`,
`Workflows`, and `Previews`, with optional `Format`.

| Embedded type | JSON fields / meaning |
| --- | --- |
| [`TaskDelivery`](swarm/requirement.go) | `via`, `ref`, `revision`, `execution`, `inline`, `at`. `via` is `mail` or `workflow_step`; the receipt matches the task's exact current result. `inline:false` does not claim all bytes were read. |
| `StructuredCompletion` | `task`, `callID?`, `value`. Pointer presence distinguishes accepted JSON null from no completion. |
| [`TaskDeferral`](swarm/deferral.go) | `workflow`, `execution`, `owner`, `revision`, `generation`, `acceptedRevision`, `status`, `note`; later mismatches invalidate deferral. |
| `TaskReference` | `task`, `revision`; positive exact revision. |
| `IntegrationInput` | A task reference plus `base` and `submitted` snapshots. |
| `Usage` | `samples?`, `inputTokens`, `outputTokens`, `cachedInputTokens`; missing provider components are null. |

Task/execution provenance survives workspace deletion. Use `Task.StartingSnapshot`,
`Task.Snapshot`, `Task.Execution`, and `Execution.Base/Workspace/SourceRoot` for
completion and restoration proof, never the member's current context pointer.
Release is empty, `releasing`, or `retained`; a released context record is absent
and `Member.Context` is empty. Member control is empty or `stopped`.

### Swarm lifecycle and decisions

`MemberState(state, member)` and `ParentState(state)` return `AgentPresentation`:
`Lifecycle`, `Busy`, `Outcome`, `Control`, `StopReason`, `Iterations`,
`MaxIterations`, `TaskStatus`, `Deferred`, `Delivering`, `Attention`, `Workflow`,
`Detail`, `Display`.
Consumers branch on typed fields; `Display` is for people. Lifecycle is derived,
never stored. Labels use `<lifecycle>[ · <detail>][ · deferred]`; the CLI may add
`approval needed`. Archived views without a live runtime omit the parent.

```mermaid
stateDiagram-v2
    [*] --> idle
    idle --> active: launch
    active --> waiting: park
    waiting --> active: wake
    active --> idle: complete
    active --> paused: fail, interrupt, or limit
    waiting --> paused: interrupt or stop
    idle --> paused: stop
    paused --> active: resume
```

`idle` may show `delivering`, `awaiting review`, `integration pending`, `integration
halted`, or `done`. `paused` may show `failed`, `interrupted`, `stopped`, or `iteration
limit (used/allowed)`. `TaskStatusIn` includes candidate/delivery context; `TaskStatus`
is context-free. Neither changes persisted task status. Done/canceled tasks keep
their own disposition in a mixed candidate. Waiting retains the execution and
allowance while releasing its slot, registry, and lease.

`Present(state, actor, parent)` derives `Decisions`, `Working`, `Counts`, `Budget`,
and `Next` from one set of coordination facts. Settlement consumes those facts
separately and unfolded; grouping never changes whether an obligation blocks.
Delivery is working, not a manual acceptance decision. Candidate contributions
fold into one integration decision; ready editing tasks can get one batch action.

| View | Wire fields |
| --- | --- |
| `DecisionItem` | `kind`, `id`, `label`, `why`, `action`, `member?`, `state?`; kinds `integration`, `mail`, `workflow`, `budget`, `task` |
| `WorkingItem` | `kind`, `id`, `label`, `state`, `task?`, `agents?` |
| Counts | `needsDecision`, `working`, `done`, `dormant`, optional `delivering`, `retained`, `deferred` |
| Budget | `unit`, `used`, `limit`, `exhausted` |
| `swarm_read` | `counts`, `budget?`, `next`, `needs_decision`, `working`, optional `needsDecisionNext`, `workingNext` |

Parent budget counts starts; member budget counts model calls. `swarm_read` returns status; `wait_agent` returns an update summary and `timed_out`, and a park of ten minutes or longer that expires names the work still in flight. Listings default to 50 items, maximum
100, 1-based `offset`, and a 16 KiB response budget; `next` is the next offset.
Counts cover all pages. `section:"decisions"`/`"working"` selects a status list.
`list_agents({path_prefix?, details?})` includes idle members and retained workspaces. Default entries contain `id`, `agent_name`, `label`, `readOnly` and compact `state` (`lifecycle`, `taskStatus?`, `attention`, `deferred`, `detail?`). `self`, `parent` and compact `parentState` remain in the envelope. Execution completion is separate from task acceptance. `details:true` returns full entries, including context/task/execution IDs, budgets and full state; release uses `items[].context` from that lookup. `swarm_read` views `workflows` (parent-only, ID required) and `tasks`
select captured details/results; large selections become complete text artifacts
with bounded previews. Reads neither resume work nor record delivery by themselves.

### Coordination model tools

The fixed parent set contains 14 tools; the child set contains six. Typed children
add `swarm_complete`; tool-free agents remain tool-free. Ordinary file, session,
transcript, and artifact tools are outside these counts. Authority is bound in the
runtime, never accepted from model arguments.

| Available to | Tools |
| --- | --- |
| Parents and children | `swarm_read`, `send_message`, `wait_agent`, `list_agents`, `swarm_publish` |
| Parents only | `spawn_agent`, `followup_task`, `interrupt_agent`, `swarm_review`, `swarm_integrate`, `swarm_control`, `workflow_run`, `swarm_help`, `workflow_help` |
| Children only | `swarm_block` |

Managed `spawn_agent({task_name, message, read_only, ...})` requires an explicit
boolean `read_only`: true for research, false for editing. Missing or non-boolean
values fail before capture or assignment. Go `AgentRequest.ReadOnly`, JavaScript
`readOnly`, generic `subagent.NewTool` and CLI defaults are unchanged.

Model task summaries retain `id`, `owner`, `description`, `status`, `displayStatus`,
`revision`, `requirement`, `deferred`, and a result-reading reference. Use
`swarm_read({view:"tasks",id,section:"details"})` for execution/run IDs, delivery
receipts, lineage, accepted revision and retained captures. JavaScript
`polly.tasks.read` and exported Go task contracts retain their full results.
These managed schema/default-response changes do not rewrite historical records.

`followup_task({target, message, refresh?})` accepts a strictly boolean `refresh`,
default false. Default calls steer active work, resume interrupted work, reopen
unresolved submissions or create linked assignments from saved provenance.
`refresh:true` requires an idle worker with a done assignment and no pending
follow-up, open task, workflow reservation or uncertain integration. It captures
current parent code (including eligible dirty and untracked files) and replaces
only the worker's safely releasable workspace. Retained or additional edits are
preserved and refused. Non-Git research keeps its live root and gets fresh scratch.
The worker retains identity, conversation, role, model, tools and completion
requirements. This operation grants no budget and accepts no work.

The model result is now `{member, message, operation, task, execution, baseOrigin,
baseCommit?, source?, note}` instead of a message echo. `message` is a durable ID;
operations are `steer`, `resume`, `new_task`. Origins are `parent`,
`previous_result`, `original_baseline`, `existing_workspace`, `live_source`.
Commit projection uses retained captures and omits unavailable commits; live
sources expose their path. Existing workspaces may include edits beyond the
baseline. Historical receipts omit unrecorded launch fields. Call-bound provenance
is separate from completion mail and cannot settle tasks or acknowledge delivery.
Matching retries reuse the original selection; changed arguments are refused.

The exported Go `FollowupTask(ctx, target, message, callID) (*Mail, error)` retains
its default behavior and return type. JavaScript
`polly.followup({task, question, commit?, ...})` is unchanged; it selects an explicit
repository commit or retained capture and returns an agent result. No JavaScript
operation, model tool, database migration or historical transcript rewrite is added. `send_message`
changes information only; default follow-ups keep worker code, refreshed follow-ups
select current parent code, and a new worker provides independent review.

`swarm_read` selects `view`: `status` (default), `tasks`, `messages`,
`publications`, or parent-only `workflows`. The common `id` selects a task or an
addressed message and is required for workflows. Preserve `section`, `step` and
JSON `pointer` selection; `query` filters publications
by case-insensitive literal text. All lists page with `offset`/`limit`/`next`.
Workflow inspection is excluded from child schemas and refused at dispatch.
Messages remain restricted to the caller's inbox. Reads never acknowledge or accept.

`workflow_run` takes JSON-encoded string `input` and either JavaScript `source` text or the `skill` and `path` of a script shipped with a discovered skill, which the host reads itself.
`background:false` (default) returns `{id, status, output, steps, next, error?}`;
`true` returns an ID immediately; the caller may continue its own work and park
with `wait_agent` when nothing else remains. Terminal
foreground summaries include the same failure and next-action guidance as notices.
Rich results attach large output; the Go tool's `Execute` still returns full text.

`swarm_control` takes `action` and `id`: `cancel_task`, `release`,
`cancel_workflow`, or `acknowledge_workflow`. Acknowledgment's `defer:true` requires
a nonblank `note`; it retains unresolved work without accepting, applying or
canceling it. Completed reports are acknowledged through durable delivery.
Budgets remain client-controlled. Release names a context from list_agents({details:true}) and
returns context, status (`released`, `ineligible`, `retained`, `busy`) and any reason.

Successful final answers submit reviewed research and editing automatically;
editing also captures an immutable snapshot. `swarm_review` accepts reviewed
research or requests changes; `swarm_integrate` accepts/applies editing revisions.
Ordinary research completes through durable delivery. `swarm_block` leaves a
child's task unresolved. Use `swarm_publish` only for findings or artifacts another
worker needs during ongoing work; final results need no separate publication.
`read_artifact` reads published and conversation artifacts.

Advanced task management and integration repair use JavaScript methods below.
The renamed model tools keep no aliases for their old names; Go methods and
stored history are preserved.

### Integration reference

`Integrate(ctx, IntegrateRequest{Tasks, Candidate, Drift})` takes exactly one of
`[]TaskReference` or a candidate ID. Task IDs must be unique/nonblank and revisions
positive. `Drift` is `paths` by default or `tree`; any nonempty drift with a candidate
is refused, including retries. Acceptance and apply share one exclusive gate and
task lock. Changed tasks finish only on a confirmed receipt; unchanged tasks use
immutable starting/submitted proof without an apply.

`IntegrationOutcome{Status, Candidate, Tasks, Unchanged, Receipt, Next}` returns
`applied` with a receipt or `done` without one. A conflict returns `workflow.Error`
code `conflicts`, candidate details in `Result`, and repair guidance in `Message`;
saved task acceptances survive. `parent_changed` requires explicit refresh and
revalidation if changed; `recovery_required` requires reconciliation. The
[workflow guide](WORKFLOWS.md#integrating-editing-results) gives the full halt path.

The following operations remain supported for advanced repair and recovery;
ordinary completion uses `Integrate`:

| Method | Result and constraints |
| --- | --- |
| `PrepareIntegration(ctx, refs, drift)` | Saves an ordered candidate without a checkout, merging each task against its own base; stops on conflict and retains pending inputs. |
| `ReadIntegration(ctx, id)` | Candidate, conflicts, provenance, acceptance, supersession, and authoritative apply receipt. |
| `ReviseIntegration(ctx, id, repair)` | A distinct editing task based on the exact intermediate snapshot becomes a contribution; remaining inputs merge afterward. |
| `RefreshIntegration(ctx, id)` | Refreshes a ready candidate against the latest parent; returns `IntegrationRefresh{IntegrationCandidate, Changed}`. Unchanged retains ID and acceptance. |
| `AcceptIntegration(ctx, id)` | Stepwise acceptance of a ready candidate and contributing task revisions. |
| `ApplyIntegration(ctx, id)` | Stepwise apply with acceptance, revision, authority, supersession, and filesystem checks; returns `*ApplyRecord`. |
| `ReconcileApply(ctx, id)` | Observes an uncertain write's before/after states without replay or rollback. |

Changed successors supersede current overlapping candidates. Completed unchanged
retries are read-only and require retained immutable proof. Applied-candidate retries
return their original receipt even after `Forget`. Completed replays leave unrelated
uncertain applies untouched; new work waits for all uncertain applies to reconcile.
A completed unchanged candidate remains inspectable without blocking forgetting.

Application protects touched path states (`paths`) or additionally the whole
parent tree (`tree`), while preserving the parent's branch, HEAD, and index.
A durable intent precedes writes; an outcome receipt follows. After the write
boundary, turn cancellation waits for the bounded apply (`Config.ApplyTimeout`,
default two minutes); parent lease loss still fences it. Outcome recording has a
separate ten-second bound. Recovery distinguishes applied, untouched, and mixed
states; later task revisions are never overwritten.

### Workflow host and resource binding

`RunWorkflow(ctx, source, input)` returns a saved report; `StartWorkflow` returns
its ID and detaches from caller cancellation. Both share registration, persistence,
member reservation, and teardown, and stop on runtime shutdown. `CancelWorkflow(id)`
cancels that attempt. `SaveWorkflow` records operation intents and exact completed
step receipts; delivered research becomes done before JavaScript receives the
value. A terminal report posts one parent notice, replacing per-member notices
while the workflow runs. There is no automatic JavaScript replay.

For a parent `workflow_run` call, `RunParent` binds the originating call ID and
composes `OnToolResult` to stage foreground delivery. A small host-generated
`ToolOutput.Data` marker is persisted as `tool_data`. Inbox admission suppresses
only matching staged terminal notices; the checkpoint commits delivery atomically
with the matching tool result or its durable artifact reference. Failed saves or
results removed by durable projection leave the notice available for recovery.
Suppression is local to that parent turn. Background launch responses carry no
terminal delivery marker, even if the workflow finishes immediately. Direct Go
launches without a persisted parent tool result keep asynchronous notice delivery.

`AcknowledgeWorkflow(ctx, id)` handles terminal failures and is a no-op for completed
reports, which are acknowledged by committed foreground delivery or notice admission. It never accepts
tasks. `DeferWorkflow(ctx, id, note)` explicitly acknowledges a failed/canceled/
interrupted report and records its exact unresolved task facts. Deferral does not
accept, apply, or cancel them. Recovery waits for a newer run to settle and retains
existing execution budgets.

The independent `workflow.Runner{Host, Config}` can use another trusted host
implementing `Call`; optional `Recorder.SaveWorkflow` provides persistence. Workflow
termination drains host calls and saves late apply receipts before releasing
registries and reservations. Scripts inherit authority; arguments cannot supply it.

`tools.ExecutionContext` binds `Root`, `ReadOnly`, `Scratch`, and a narrowed policy
through `BindExecutionContext`. `ExecutionPolicy(root, tools.ExecutionGrant{
ReadOnly, DeniedReads, DeniedWrites, Scratch})` builds that policy; `DeniedReads`
are private roots with the root and scratch granted back inside them, and
read-only without scratch denies all writes. `ContextTool` rebinds custom Go tools;
`ContextIndependentTool` declares safe independence. Stdio MCP relaunches in context;
remote MCP requires `contextIndependent:true`. Indexed semantic search is omitted
from member registries. Rich wrappers preserve `ToolOutput.Media` and `Data`.

File-mutating built-ins describe their change in `ToolOutput.Data`: `edit_file` and
`write_file` return a `tools.FileChanges` (workspace `Root`, sorted `Changes`, each a
`FileChange` with `Path`, `Kind`, `Additions`, `Deletions`, a bounded unified `Diff`,
and `Truncated`/`Binary` flags); `bash` adds the same payload as `Changes` on its
`CommandResult` when the registry has a `tools.ChangeTracker`, installed with
`WithChangeTracker` or `SetChangeTracker` and inherited by derived and bound
registries. `worktree.NewChangeTracker(registry, directory, privatePaths, limits)`
is the Git implementation: it snapshots the repository containing the command's
directory before and after the command with a private index and object store under
`directory`, and reports `Tracked=false` with a `Reason` outside Git or past its
`ChangeLimits`. The model-facing text of these tools does not include the diff.

Automatic release requires settled tasks, no active/paused execution or invocation,
no active reservation, and no uncertain apply, plus unchanged/integrated filesystem
proof. `Cleanup` waits cancelably for an automatic release pass before taking
scheduler locks. `Forget` removes snapshots after safe cleanup and resolved
integration obligations. `polly.release` can release
its own idle check copies or settled members while retaining its reservation;
repeated release returns `{released, dormant:true}` only to the historical owner.
Unintegrated changes or repeated cleanup failures produce a retained context with
reason. Task completion is unaffected.

### JavaScript surface

The model/JavaScript commit contract differs from Go storage structs; Go snapshot methods and record serialization remain compatible.

For baseline selection, `spawn_agent`, `polly.agent`, `polly.context`, and
`polly.followup` accept a full local commit object ID as well as a retained
capture. Local selection validates and pins the original commit, preserving its
SHA and history; omit `commit` to capture current dirty/untracked files. Source
checkouts must belong to the parent's repository. Existing workspace authority,
follow-up restrictions, and explicit task acceptance still apply. Publications
require retained capture provenance. Go callers can use
`worktree.Manager.RetainCommit(ctx, source, commit)` to obtain an unchanged
`Snapshot` record; existing Go request and storage formats are unchanged.

Public candidate objects always include `receipt`: `null` when no apply attempt
is recorded, otherwise the existing receipt object. This applies to prepare,
read, revise, refresh, and candidates nested in structured errors. Access
`candidate.receipt` directly. A null receipt does not establish unchanged parent
contents; that needs a separate check. Receipt `status` values are `applied`,
`not_applied`, `applying`, and `recovery_required`. The latter two require outcome
inspection/reconciliation; `not_applied` records an observed unapplied patch.
`integrate(...)` exposes application evidence as `result.receipt`; unchanged
outcomes can omit it. The exported Go candidate's optional field and storage
serialization, historical workflow results, and user-authored objects are unchanged.

Define exactly one `polly.workflow(name, inputSchema, run)`; `polly.defineWorkflow({name,
inputSchema, run})` is the same definition as one object. The table lists
methods on `polly`; nested integration methods are advanced repair operations.

| API | Result / options |
| --- | --- |
| `agent(label, task, options?)` or `agent({task, label?, input?, schema?, tools?, model?, readOnly?, review?, source?, commit?, context?, session?, taskID?})` | `AgentResult` with `value`, `session`, `context`, `task`, `execution`, `revision`, `usage`. `task` is the brief; `taskID` selects a precreated task after its dependencies are done. `label` is required for new agents (1–80 characters); continuations inherit it. The host seeds the session title at creation. |
| `research(label, task, options?)` | `agent` with `readOnly` forced to true; an explicit `readOnly: false` is refused. |
| `editor(source, label, task, options?)` | `agent` with `source` forced to the given nonblank path; a conflicting `source` option is refused. |
| `followup({task, question, commit?, label?})` | Creates and runs a linked task on the completed task's member; returns `AgentResult`. |
| `integrate({tasks?, candidate?, drift?})` | Parent editing completion; `IntegrationOutcome` with the same selectors and validation as Go. |
| `context({source?, commit?, context?, readOnly?, disposable?})` | Opaque ID for a fresh isolated copy. `disposable: true` declares that nothing the copy will hold is work: `release` then removes it whatever it contains, and it can be neither captured with `snapshot` nor passed as `context` to an agent or another copy. Use it for check copies, which build outputs would otherwise keep from being released. |
| `scope({context, label?}, async work => ...)` | Scoped work methods; `cwd` is refused. |
| `tool(name, args, {context})` | `{text, data, artifacts, step}` under context tool policy. |
| `exec(command, {context, check?})` | Tool result plus `exitCode`; runs `bash -o pipefail -c`, so any failing pipeline stage fails the pipeline. Default `check:true` checks the final exit status. `check:false` collects ordinary process failures; sandbox, timeout and cancellation errors still reject. Enable `set -e` explicitly when every command must succeed. |
| `snapshot(context)` | Immutable `{commit, tree, source}`; pass its `.commit` to another agent/context. |
| `release(context)` | Proof-based removal of an inactive attempt-owned context. |
| `integration.prepare({tasks:[{task,revision}], drift?})` | Ordered candidate with `receipt:null` before an apply attempt; default drift `paths`. |
| `integration.read(id)` | Current candidate; `candidate.receipt` is always present, null or an object. |
| `integration.revise(id, {task,revision})` | Successor adopting an exact intermediate repair. |
| `integration.refresh(id)` | Candidate fields plus `changed`. |
| `integration.reconcile(id)` | Observes interrupted apply outcomes without patch replay; returns an apply receipt. |
| `integration.accept(id)`, `integration.apply(id)` | Stepwise acceptance and apply; normal completion uses `integrate`. |
| `tasks.create({description,criteria?,dependencies?,owner?,review?,requirement?})` | Resulting task; requirement is fixed at creation. Unowned editing work requires `applied`. |
| `tasks.update({task,revision,owner,dependencies})` | Resulting task after validated reassignment. Stop active owners first; stale revisions, dependency cycles and incompatible owners are refused. Empty owner permits scheduler assignment. |
| `tasks.read(task)`, `tasks.get(task)` | Current task and result, with `baseCommit`/`resultCommit` instead of internal snapshot IDs. |
| `tasks.review({task,revision,accept,feedback?})` | Accept reviewed research or request changes on a submitted task. |
| `parallel(items, callback, {concurrency?, errors?})` | Ordered `{ok,value}` / `{ok,error}`; concurrency default 8, range 1–256; errors `collect` or `throw_after_all`. |
| `log(message)` | Awaitable saved progress step. |
| `fail(message, result?)` | Structured workflow failure. |
| `schema` | `string`, `number`, `integer`, `boolean`, `enum`, `array`, `object`, `keyed`, with `str`, `num`, `int`, `bool`, `arr`, `obj` as the same functions under short names; `polly.keyed` is `schema.keyed`. |

Scoped `work` exposes agent, research, editor, followup, integrate, tool, exec,
snapshot, context, release, and log. Followup/integrate take explicit arguments rather than scope
defaults; other work methods use applicable defaults. Integration/task namespaces,
schema, parallel, fail, and workflow definition remain on `polly`. Reconciliation is available through `polly.integration.reconcile(id)`.

Objects require declared keys and reject extras by default. `keyed(ids, schema)`
requires unique string IDs and every corresponding result key. Validate input
before effects and await all operations. Pending operations at return, or a promise
without a host operation capable of settling it, fail. Output/error serialization
cannot initiate effects. Thrown primitives are retained as structured failures.

The VM exposes no Node.js/modules/filesystem/network/process/timer APIs. One Go
owner resolves promises from asynchronous host work. Defaults: five seconds per
uninterrupted JS slice, 4,096 host calls per attempt, 512 stack frames; host waiting
does not spend the slice budget. There is no hard heap limit. Runtime authority
and process sandboxing remain the external-effect boundary.

`tools.CommandError` distinguishes ordinary target exit from sandbox/setup,
approval, timeout, and cancellation errors. Only ordinary exit is recoverable with
`check:false`; this does not change the command's shell options. A denial within an
already launched shell remains its ordinary exit; error classification never
infers intent from stderr. Text/JSON media is stored as readable artifacts;
wrappers must preserve its bytes and structured data.

Admitted mail is saved with `messages.MetadataKeySwarmMessages` delivery IDs and
`MetadataKeyAgentSynthetic:true`. It remains model-visible without becoming a
user turn in the TUI; older delivery-ID-only envelopes are recognized on replay.

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
- `Metadata.ExtraReadDirs` records extra read-only workspace directories
  (`--add-dir` / the `/add-dir` REPL command) as canonical absolute real
  paths. The session record is the source of truth: the list is restored
  when the session is opened, and `SetMetadata`, `Reset`, and `Clear`
  preserve it. In JSON it is `extraReadDirs`.
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
  The CLI registers `set_session_title` only for root conversations. Children
  receive their initial title from the launch label; manual F2 and `/title`
  editing remain available. The generic library agent does not inject naming
  policy or register the tool.
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
- `sessions.CoordinationViewStore.ReadCoordinationView(ctx, rootID)` is an
  optional trusted-host display capability implemented by SQLite. It reads a
  root's saved coordination records by stable identity without acquiring a
  lease, touching last-used time, or loading transcripts. `swarm.ReadStateView`
  decodes these records for history/status views; neither operation activates
  execution or grants model tools access to other families. It returns
  `swarm.ErrUnsupportedFormat` for roots written before the format record.
- `AcquireOptions{ExpectedID: view.ID}` atomically verifies the viewed identity
  before taking a write lease, and implies `ExistingOnly`. A deleted name reused
  by a different session cannot receive a follow-up intended for the old view.
- A view's artifact store reads only that session's owned artifacts, remains
  usable after its writer closes, and rejects `Put`/`RemoveAll` with
  `sessions.ErrReadOnlyView`. Deletion/reset may remove those artifacts; store
  shutdown cancels readers. It does not retain or extend the session's lifetime.
- `session.ArtifactStore()` is scoped to the session; artifact bytes commit
  in the same database as the transcript.
- `llm.AgentConfig.OpenArtifact` optionally authorizes and opens artifacts absent
  from the conversation's reference index. It returns `(artifacts.Ref,
  io.ReadCloser, error)` with matching metadata and a reader at byte zero;
  `read_artifact` owns closing it and uses the same paging/search/media behavior
  as for conversation artifacts. Supply `ArtifactStore` as usual. For a swarm
  parent, set this callback to its `sessions.CoordinationSession.OpenPublishedArtifact`.
  That method returns the reference and reader only after checking the family
  publication and granting session ownership. The CLI wires this automatically;
  swarm member execution always rebinds the callback to the member's session.
  A nil callback keeps reads limited to conversation references. It does not
  expand `list_artifacts` or permit reading unpublished peer artifacts.
- `AcquireOptions{Parent: name}` links a new session to the one whose agent
  spawns it. The link is by id, so `Metadata.Parent` reads as the parent's
  current name after renames, and `SetMetadata` ignores the field; a session
  whose parent was deleted keeps the last name it knew.
- Optional `Metadata.SpawnCallID` (`spawnCallID` in JSON) identifies the
  parent's originating `spawn_agent` call. `Metadata.SpawnOutcome`
  (`spawnOutcome`) records only that child's initial delegated run, using
  `ReportFinished`, `ReportFailed`, `ReportCanceled`, or `ReportPaused`; empty
  means unknown or not yet settled. The CLI records these fields for Agents
  activity in the TUI, renders archived outcomes with the lifecycle words
  (`idle · done`, `paused · failed`, `paused · interrupted`, `paused · iteration
  limit`), and leaves the outcome unchanged on child follow-ups. They use the
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

Set one on a request via `ResponseSchema`.

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
