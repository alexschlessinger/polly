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
// returns the final answer. Capped at 250 rounds; hitting the cap returns
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

    Model          string
    MaxTokens      int
    Messages       []messages.ChatMessage
    Tools          []tools.Tool
    ResponseSchema *Schema         // JSON schema for structured output
    ThinkingEffort ThinkingEffort  // llm.EffortOff(), EffortLevel(LevelHigh), EffortBudget(n), EffortDynamic()
    Stream         *bool           // nil = streaming (default), false = non-streaming
    Skills         *skills.Catalog // Injects the skill prompt into the system prompt
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
worker := registry.Derive(tools.AllowTools("read_file", "search_files", "git__*"),
    tools.DenyTools("git__push"))
agent := llm.NewAgent(client, worker, llm.AgentConfig{})
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

## Subagents

The `subagent` package gives a model the `spawn_agent` tool: a brief, an
optional label, a tool allow-list, and optional model and iteration
overrides. What running the child means is the host's `Runner`; the
library's `AgentRunner` runs an in-memory `llm.Agent` over a derived view
of the parent's tools (never `spawn_agent` itself), with the brief as the
only user message after your base messages:

```go
registry := tools.NewToolRegistry([]tools.Tool{&WeatherTool{}})
base := llm.CompletionRequest{Model: "openai/gpt-5.4",
    Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "Be brief."}}}
registry.Register(subagent.NewTool(subagent.AgentRunner(client, registry, base, llm.AgentConfig{})))
registry.MarkAlwaysAllowed(subagent.ToolName)
```

The tool result is the child's final reply plus, when the runner gave it
one, its session name. `subagent.WithMaxConcurrent` bounds parallel
children (default four). The tool is exempt from `AgentConfig.ToolTimeout`
through the `tools.UntimedTool` interface. The polly CLI's runner opens a
child session on the same store, recorded with `Metadata.Parent`.

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
- `session.ArtifactStore()` is scoped to the session; artifact bytes commit
  in the same database as the transcript.

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
- Tool `Execute` implementations should be safe to call concurrently; the
  library doesn't serialize them.
