# Pollytool as a Library

Pollytool's CLI is a thin layer over a set of Go packages you can use directly:
one streaming interface over seven LLM providers, plus tools, sandboxing,
skills, sessions, and structured output. This document is the tour.

## Installation

```bash
go get github.com/alexschlessinger/pollytool
```

## API Keys

Set the keys for whichever providers you use. `llm.GetDefaultClient()` reads:

- `POLLYTOOL_OPENAIKEY`
- `POLLYTOOL_ANTHROPICKEY`
- `POLLYTOOL_GEMINIKEY`
- `POLLYTOOL_OLLAMAKEY` (optional for a local Ollama)
- `POLLYTOOL_HUGGINGFACEKEY`

The `polly` CLI additionally reads `POLLYTOOL_DEEPSEEKKEY` and
`POLLYTOOL_OPENROUTERKEY`. In a library you reach those providers by passing
`deepseek` / `openrouter` keys to `llm.NewMultiPass` yourself.

## Quick Start

```go
package main

import (
    "context"
    "fmt"

    "github.com/alexschlessinger/pollytool/llm"
)

func main() {
    ctx := context.Background()

    // export POLLYTOOL_OPENAIKEY="your-key" first

    // One-shot completion
    joke, err := llm.QuickComplete(ctx, "openai/gpt-5.4", "Tell me a joke", 500)
    if err != nil {
        panic(err)
    }
    fmt.Println(joke)

    // Streaming completion
    err = llm.StreamComplete(ctx, "openai/gpt-5.4", "Write a short story", 500, func(chunk string) {
        fmt.Print(chunk)
    })
    if err != nil {
        panic(err)
    }
}
```

## Helpers

The `llm` package ships convenience functions for the common cases. Each one
builds a fresh router from environment variables, which is fine for scripts;
if you're making lots of calls, create one client with `llm.GetDefaultClient()`
(or `llm.NewMultiPass`) and reuse it.

### One-liners

```go
// Model + prompt + token budget. Keys come from POLLYTOOL_*KEY env vars.
response, err := llm.QuickComplete(ctx, "openai/gpt-5.4", "Tell me a joke", 1000)

// More budget for longer output
response, err = llm.QuickComplete(ctx, "anthropic/claude-opus-4-7", "Write a story", 4000)
```

### Conversation with history

```go
history := []messages.ChatMessage{
    {Role: messages.MessageRoleSystem, Content: "You are helpful"},
    {Role: messages.MessageRoleUser, Content: "Hi"},
    {Role: messages.MessageRoleAssistant, Content: "Hello! How can I help?"},
}

// Appends your new message and returns the assistant's reply
reply, err := llm.ChatWithHistory(ctx, "openai/gpt-5.4", history, "What did I just say?", 1000)
fmt.Println(reply.Content)
```

### Structured output, the easy way

`llm.SchemaFor` builds a JSON schema from a Go struct by reflection, and
`StructuredComplete` unmarshals the model's answer straight back into it:

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
// user == UserInfo{Name: "John Doe", Age: 30, Email: "john@example.com"}
```

Prefer writing the schema yourself? `llm.SchemaFromJSON(jsonString)` parses
one, and `&llm.Schema{Raw: map[string]any{...}}` builds one literally. See
[Structured Output](#structured-output) below.

### The builder

For anything past a one-liner, `CompletionBuilder` gives you a fluent API:

```go
client := llm.GetDefaultClient()

result, err := llm.NewCompletionBuilder("openai/gpt-5.4").
    WithSystemPrompt("You are a helpful assistant").
    WithUserMessage("Tell me about Go").
    WithTemperature(0.8).
    WithMaxTokens(500).
    Execute(ctx, client)
if err != nil {
    panic(err)
}
fmt.Println(result)

// Streaming variant
err = llm.NewCompletionBuilder("openai/gpt-5.4").
    WithSystemPrompt("You are a creative writer").
    WithUserMessage("Write a haiku").
    ExecuteStreaming(ctx, client, func(chunk string) {
        fmt.Print(chunk)
    })
```

### Automatic tool handling

`ExecuteWithTools` runs the whole tool loop for you — it executes each call
the model makes, feeds results back, and returns the model's final answer:

```go
registry := tools.NewToolRegistry([]tools.Tool{&WeatherTool{}})

response, err := llm.NewCompletionBuilder("openai/gpt-5.4").
    WithUserMessage("What's the weather in NYC?").
    ExecuteWithTools(ctx, client, registry)
// response.Content holds the final answer, tools already called

// Skills work on the builder too
result, err := llm.NewCompletionBuilder("openai/gpt-5.4").
    WithSystemPrompt("You are a helpful assistant").
    WithSkills(catalog).
    WithUserMessage("Tell me about Go").
    Execute(ctx, client)
```

## Core Types

### The LLM interface

Every provider implements one method:

```go
type LLM interface {
    ChatCompletionStream(context.Context, *CompletionRequest, EventStreamProcessor) <-chan *messages.StreamEvent
}
```

The processor turns raw message chunks into stream events;
`messages.NewStreamProcessor()` is the standard implementation.

### CompletionRequest

```go
type CompletionRequest struct {
    APIKey  string
    BaseURL string        // Custom endpoint (OpenAI-compatible providers)
    Timeout time.Duration

    // nil means "don't send temperature" — required for reasoning models
    // (o1, o3, gpt-5.x), which reject the parameter outright.
    // Use llm.Float32Ptr(0.7) to set it.
    Temperature *float32

    Model          string
    MaxTokens      int
    Messages       []messages.ChatMessage
    Tools          []tools.Tool
    ResponseSchema *Schema         // JSON schema for structured output
    ThinkingEffort ThinkingEffort  // Reasoning effort (see below)
    Stream         *bool           // nil = streaming (default), false = non-streaming
    Skills         *skills.Catalog // Skill catalog for system prompt augmentation
}
```

Two fields deserve a note:

- **Temperature is a pointer.** `Temperature: 0.7` won't compile — write
  `Temperature: llm.Float32Ptr(0.7)`, or leave it nil to let the provider
  default apply (and to keep reasoning models happy).
- **ThinkingEffort** controls reasoning depth: `llm.EffortOff()`,
  `llm.EffortLevel(llm.LevelHigh)` (levels `LevelMinimal` through `LevelMax`),
  `llm.EffortBudget(tokens)`, or `llm.EffortDynamic()`.

### Messages

```go
type ChatMessage struct {
    Role       string                // "system", "user", "assistant", "tool", "internal"
    Content    string                // Text content
    Parts      []ContentPart         // Multimodal content (images, files)
    ToolCalls  []ChatMessageToolCall // Tool calls made by the assistant
    ToolCallID string                // Set on tool-role replies
    ToolName   string                // Tool name on tool-role replies
    Reasoning  string                // Model reasoning, when the provider exposes it
    Metadata   map[string]any        // Token counts, error flags, etc.
    StopReason StopReason
}

type ChatMessageToolCall struct {
    ID        string // Unique identifier for this call
    Name      string // Tool to call
    Arguments string // JSON-encoded arguments
}
```

Roles are plain string constants:

```go
const (
    MessageRoleSystem    = "system"
    MessageRoleUser      = "user"
    MessageRoleAssistant = "assistant"
    MessageRoleTool      = "tool"
    MessageRoleInternal  = "internal" // app state; filter before sending upstream
)
```

`messages.User("hello")` is a handy shortcut for a one-message user history.

### Stream events

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
    EventTypeContent   StreamEventType = "content"   // Text chunk
    EventTypeReasoning StreamEventType = "reasoning" // Reasoning chunk (also in Content)
    EventTypeToolCall  StreamEventType = "tool_call" // A tool call was parsed
    EventTypeComplete  StreamEventType = "complete"  // Final assembled message
    EventTypeError     StreamEventType = "error"     // Something went wrong
)
```

## Providers

### Model identifiers

MultiPass routes on a `provider/model` prefix:

- OpenAI: `openai/gpt-5.4`, `openai/gpt-5.4-mini`
- Anthropic: `anthropic/claude-opus-4-7`, `anthropic/claude-sonnet-4-6`
- Gemini: `gemini/gemini-3.1-pro-preview`, `gemini/gemini-3.1-flash-lite-preview`
- Ollama: `ollama/gpt-oss`
- Hugging Face: `huggingface/...`, DeepSeek: `deepseek/...`, OpenRouter: `openrouter/...`

The prefix belongs to MultiPass. Direct provider clients take bare model
names (`"gpt-5.4"`, not `"openai/gpt-5.4"`) — they send whatever you give
them verbatim.

### MultiPass

`MultiPass` is the multi-provider router — a stateless one; it constructs
provider clients per call rather than memoizing them:

```go
package main

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/alexschlessinger/pollytool/llm"
    "github.com/alexschlessinger/pollytool/messages"
)

func main() {
    ctx := context.Background()

    multipass := llm.NewMultiPass(map[string]string{
        "openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
        "anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
    })
    // or: multipass := llm.GetDefaultClient() to load every key from the env

    req := &llm.CompletionRequest{
        Model: "anthropic/claude-opus-4-7",
        Messages: []messages.ChatMessage{
            {Role: messages.MessageRoleSystem, Content: "You are a helpful assistant."},
            {Role: messages.MessageRoleUser, Content: "Hello, how are you?"},
        },
        Temperature: llm.Float32Ptr(0.7),
        MaxTokens:   1000,
        Timeout:     30 * time.Second,
    }

    processor := messages.NewStreamProcessor()
    for event := range multipass.ChatCompletionStream(ctx, req, processor) {
        switch event.Type {
        case messages.EventTypeContent:
            fmt.Print(event.Content)
        case messages.EventTypeComplete:
            fmt.Printf("\nComplete: %+v\n", event.Message)
        case messages.EventTypeError:
            fmt.Printf("Error: %v\n", event.Error)
        }
    }
}
```

### Direct provider clients

You can skip the router and talk to one provider. Note the differing shapes —
and remember: bare model names here.

```go
openai := llm.NewOpenAIClient(apiKey, "")          // second arg = optional base URL
anthropic := llm.NewAnthropicClient(apiKey)
gemini, err := llm.NewGeminiClient(apiKey)         // returns (client, error)
ollama := llm.NewOllamaClient("http://localhost:11434", "") // baseURL first; key optional
```

## Tools

### The Tool interface

```go
type Tool interface {
    GetSchema() *schema.ToolSchema
    Execute(ctx context.Context, args map[string]any) (string, error)

    GetName() string   // Namespaced name, e.g. "script__toolname"
    GetType() string   // "shell", "mcp", or "native"
    GetSource() string // Where it came from, e.g. "/path/to/script.sh"
}
```

Schemas are built with the `schema` package. `schema.Tool` assembles an
object schema, and there are small helpers for common parameter shapes:
`schema.S` (string), `schema.Int`, `schema.Bool`, `schema.Enum`,
`schema.Array`. If you already have schema JSON, `schema.ToolSchemaFromJSON`
parses it.

### Writing a tool

```go
import (
    "context"
    "fmt"

    "github.com/alexschlessinger/pollytool/schema"
)

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

### Running the tool loop

The easy path is the builder's `ExecuteWithTools` (shown earlier). If you
want the loop in your own hands — custom logging, approval gates, whatever —
here's the correct shape. Note that each round makes a *new*
`ChatCompletionStream` call: `for range` latches onto one channel, so you
need an outer loop, not a channel reassignment.

```go
registry := tools.NewToolRegistry([]tools.Tool{&WeatherTool{}})
processor := messages.NewStreamProcessor()

history := []messages.ChatMessage{
    {Role: messages.MessageRoleUser, Content: "What's the weather in San Francisco?"},
}

for {
    req := &llm.CompletionRequest{
        Model:    "openai/gpt-5.4",
        Messages: history,
        Tools:    registry.All(),
    }

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
        break // a normal answer — we're done
    }

    // Keep the assistant turn that made the calls, then answer each call.
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
            Role:       messages.MessageRoleTool,
            Content:    result,
            ToolCallID: call.ID,
            ToolName:   call.Name,
        })
    }
}
```

The ordering matters to providers: the assistant message carrying `ToolCalls`
must precede the tool-role replies, and it goes into history exactly once.

## Shell Tools

Any executable that speaks a two-flag protocol can be a tool: `--schema`
prints a JSON Schema, `--execute <json-args>` does the work.

### Example: weather.sh

```bash
#!/bin/bash
# weather.sh - a simple weather tool

if [ "$1" = "--schema" ]; then
    cat <<EOF
{
  "title": "get_weather",
  "description": "Get current weather for a location",
  "type": "object",
  "properties": {
    "location": {
      "type": "string",
      "description": "City and state, e.g. 'San Francisco, CA'"
    },
    "units": {
      "type": "string",
      "enum": ["celsius", "fahrenheit"],
      "default": "fahrenheit",
      "description": "Temperature units"
    }
  },
  "required": ["location"]
}
EOF
    exit 0
fi

if [ "$1" = "--execute" ]; then
    LOCATION=$(echo "$2" | jq -r '.location')
    UNITS=$(echo "$2" | jq -r '.units // "fahrenheit"')

    # In production, call a real weather API
    if [ "$UNITS" = "celsius" ]; then
        echo "18°C and foggy in $LOCATION"
    else
        echo "65°F and foggy in $LOCATION"
    fi
    exit 0
fi

echo "Usage: $0 [--schema | --execute <json-args>]"
exit 1
```

Make it executable with `chmod +x weather.sh`.

### Loading shell tools

Process-backed tools need an explicit sandbox policy — a registry without one
refuses to load shell tools (it won't even run their `--schema` command):

```go
package main

import (
    "context"
    "fmt"

    "github.com/alexschlessinger/pollytool/llm"
    "github.com/alexschlessinger/pollytool/tools"
    "github.com/alexschlessinger/pollytool/tools/sandbox"
)

func main() {
    ctx := context.Background()

    registry := tools.NewToolRegistry(nil,
        tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()),
    )

    if _, err := tools.LoadShellToolsWithRegistry(registry, []string{
        "./weather.sh",
        "./calculator.sh", // load as many as you like
    }); err != nil {
        fmt.Printf("Warning: %v\n", err)
    }

    client := llm.GetDefaultClient()

    response, err := llm.NewCompletionBuilder("openai/gpt-5.4").
        WithUserMessage("What's the weather in San Francisco and New York?").
        WithMaxTokens(1000).
        ExecuteWithTools(ctx, client, registry)
    if err != nil {
        panic(err)
    }
    fmt.Println(response.Content)
}
```

### Script protocol requirements

Shell scripts used as tools must:

1. Accept `--schema` and print a JSON Schema describing the tool
2. Accept `--execute <json-args>` and process the JSON arguments
3. Return results as plain text on stdout
4. Exit 0 on success, non-zero on error

Shell tools run sandboxed by default. The schema may include `"sandbox"` at the top level to customize permissions. When the sandbox is applied, `[sandboxed]` is appended to the shell tool's description in the LLM-facing schema. If no supported sandbox backend is available, Polly exits with an error instead of running unsandboxed. Disable globally with `--nosandbox` or `POLLYTOOL_NOSANDBOX=true`. For a conversation run, explicitly supplied sandbox-policy flags are rejected rather than ignored while no-sandbox mode is effective; pass `--nosandbox=false` to override an ambient opt-out.

Tools that omit `"sandbox"` get the defaults below. Tool-controlled metadata cannot silently disable containment: `"sandbox": false` is refused unless the registry was constructed with the deliberately named `tools.WithUnsafeNoSandbox()` option (the CLI equivalent is `--nosandbox`). See [SANDBOX.md](SANDBOX.md) for design intent and platform differences.

#### Sandbox Spec Reference

Library callers executing an `exec.Cmd` through a built-in sandbox must use
`sandbox.WrapCmdManaged` (or `sandbox.WrapCmdWithEnvManaged`) and invoke the
returned idempotent cleanup after `Start`, `Run`, `Output`, or `CombinedOutput`
returns. Linux and macOS backends keep bootstrap, policy, and environment file
descriptors open until process start; the managed helpers close only those
backend-owned descriptors while preserving caller-owned `ExtraFiles`. The
legacy cleanup-less `Sandbox.Wrap`, `WrapCmd`, and `WrapCmdWithEnv` entry points
remain source-compatible but fail closed with `sandbox.ErrManagedWrapRequired`
before mutating the command when used with a built-in backend. Custom sandbox
implementations that do not opt into the built-in managed capability retain
the legacy behavior.

`"sandbox"` can be `true` (use defaults) or an object:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `allowNetwork` | bool | `false` | Allow outbound network access |
| `denyDNS` | bool | `false` | With `allowNetwork`, block DNS on macOS; on Linux, suppress the default resolver only (best effort). |
| `writablePaths` | string[] | `[]` | Directories where writes are allowed (supports `~`) |
| `readPaths` | string[] | `[]` | Paths exempted from the credential deny list (supports `~`) |
| `denyPaths` | string[] | `[]` | Extra paths blocked from reads, in addition to the built-in deny list (supports `~`) |
| `denyWritePaths` | string[] | `[]` | Paths kept read-only even inside a `writablePaths` entry (supports `~`). Mutable writable ancestors are pinned so the protected path cannot be bypassed by relocation. |
| `allowEnv` | string[] | all non-sensitive | If set, only these env vars are passed through (overrides the sensitive-var stripping) |
| `passEnv` | string[] | `[]` | Additively exempt these env vars from the sensitive-var stripping. Ignored when `allowEnv` is set. |
| `allowUnixSockets` | string[] | `[]` | Absolute Unix-socket paths the process may connect to, even while broad Unix-socket access stays blocked (supports `~`). A grant is dropped for any command where its path is not a live socket. |
| `denyWrite` | bool | `false` | Deny all file writes, including temp. Overrides `writablePaths`. |

**Base policy** (when `"sandbox": true` and the registry's base config is `sandbox.DefaultConfig()`):
- Writes: denied everywhere except the sandbox temp dir (`/tmp` is a private tmpfs on Linux)
- Network: denied
- Reads: all files accessible except credential paths (see below)
- Env: all vars passed through except sensitive ones (see below)
- Linux: private `/tmp` and `/run`, inherited capabilities dropped, filesystem Unix sockets denied, own PID and IPC namespaces, and own session

**CLI presets:** the `polly` CLI selects its base config with
`--sandbox <preset>` (components `base`, `readonly`, `workspace`, `git`, `net`,
`ssh`, and `sshkeys` joined with `+`; library equivalent `sandbox.ParsePreset`).
The CLI default is `workspace+net+git`: the canonical working directory is added
to `writablePaths`, `allowNetwork` is enabled, and `git` selects **leaf mode**
for the workspace's Git protection — only the dangerous metadata leaves
(`config`, `config.worktree`, `hooks/`, routing and worktree pointers) are added
to `denyWritePaths`, so `.git` stays writable and commit/rebase/fetch work while
hook-planting and config rewrites stay blocked. Absent leaves are created inert
(an empty `config`/`hooks/`, and `config.worktree` only when
`extensions.worktreeConfig` is enabled); a leaf that cannot be created falls
that one repository back to a whole-tree pin. Without `git`, `workspace` keeps
whole Git metadata trees read-only (including missing leaves such as
`config.worktree`), so `git commit` fails inside the sandbox. `git` requires
`workspace` and is rejected on its own. The `ssh` component passes
`SSH_AUTH_SOCK` through (`passEnv`), grants that one socket
(`allowUnixSockets`), and exempts `~/.ssh/config` and `~/.ssh/known_hosts` from
the deny list; `sshkeys` exempts all of `~/.ssh` (private keys included) for
agentless setups.

The `workspace` component refuses the filesystem root, the user's home
directory, exact mounted-volume roots on Linux and macOS, and exact Linux
private temp/runtime roots before recursive discovery. Descendants of mounted
volumes remain valid bounded workspaces; otherwise change into a project
directory or select `--sandbox base`. Bare-repository working directories;
symlinked or hard-linked Git routing/config/hook metadata; repository-local
`core.hooksPath`; and config includes are refused when their effective identity
or target cannot be pinned portably.

Polly accepts PATH-selected Git when it reaches fixed `/usr/bin/git` through a
stable non-symlink route outside writable paths. On Darwin it also accepts the
standard Homebrew `/opt/homebrew/bin/git` and `/usr/local/bin/git` leaf
symlinks when they resolve directly to a non-writable, single-link
`Cellar/git/<version>/bin/git` target outside writable paths. Polly executes
the resolved selected Git so its compiled config-prefix semantics are preserved
while resolving effective and overridden global/system `core.hooksPath` values
and recursively inspecting config includes regardless of current `includeIf`
conditions. The workspace preset is refused when a hook, config source, or
include path lands in host-visible writable content outside protected metadata
(including macOS host temp trees), when an existing config source has hard-link
aliases, or when a configured hook entry is symlinked or hard-linked;
`/dev/null` is accepted as an immutable hook-disabling target.

A tool's own `sandbox` object merges monotonically on top of the base config:
it may add grants or restrictions, but cannot remove an earlier entry.
`--writepath` and `--allownet` add to any preset. Before each final sandbox is
constructed, Polly re-runs the stored workspace config/include/hook audit
against all merged host-visible writable roots, so a CLI or per-tool overlay
cannot reopen one of those persistence routes (Linux's exact private
temp/runtime mounts remain non-host-visible). Effective configs are prepared
with canonical writable/read paths and private filesystem identities; the CLI
and tool registry retain those identities across later lazy sandbox
construction, rejecting a replaced or rerouted approved path before the
backend runs. Polly also emits a visible, deduplicated warning when an effective
global or per-tool policy leaves a home directory or filesystem root as a broad
writable grant.

**Credential paths denied by default:**
`~/.ssh`, `~/.gnupg`, `~/.gpg`, `~/.aws`, `~/.azure`, `~/.config/gcloud`, `~/.kube`, `~/.docker/config.json`, `~/.npmrc`, `~/.pypirc`, `~/.gem/credentials`, `~/.cargo/credentials`, `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.local/share/keyrings`, `~/Library/Keychains`

Deny paths are re-checked on every command, and symlinked entries are resolved to their real targets. The `--denypath` flag (env `POLLYTOOL_DENYPATHS`) adds global entries for all sandboxed tools.

Relative `writablePaths`, `readPaths`, `denyPaths`, and `denyWritePaths`
entries are resolved against the process working directory when the sandbox is
constructed. Empty path entries are rejected. On Linux, a `readPaths` entry may
name a child of a denied directory (for example `~/.ssh/config`); that child is
restored read-only while its siblings remain masked. A symlinked `readPaths`
entry keeps its approved lexical route, but both the route and canonical target
are frozen and revalidated so a later replacement or retarget fails closed.

**Sensitive env vars** are always stripped, even without `allowEnv`, matched
case-insensitively:

- the prefixes `POLLYTOOL_*` and `AWS_*`;
- names ending in `_API_KEY`, `_APIKEY`, `_TOKEN`, `_SECRET`, `_SECRET_KEY`,
  `_ACCESS_KEY`, `_PASSWORD`, `_PASSPHRASE`, `_CREDENTIALS`, `_PRIVATE_KEY` —
  and those same names bare (`TOKEN`, `PASSWORD`, `API_KEY`, ...);
- database credential carriers: `PGPASSWORD`, `PGPASSFILE`, `MYSQL_PWD`,
  `REDISCLI_AUTH`, `DATABASE_URL`;
- agent sockets and host runtime handles: `SSH_AUTH_SOCK`, `SSH_AGENT_PID`,
  `GPG_AGENT_INFO`, `DBUS_SESSION_BUS_ADDRESS`, `DBUS_SYSTEM_BUS_ADDRESS`,
  `DOCKER_HOST`, `CONTAINER_HOST`, `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`,
  `PULSE_SERVER`.

To pass one through, add it to `passEnv` (additive — everything else still
flows) or list it in `allowEnv` (strict — *only* the listed names flow, and
`passEnv` is then ignored). The `ssh` preset uses `passEnv` for
`SSH_AUTH_SOCK`.

**Conflict resolution:** `denyWrite: true` silently overrides `writablePaths` (and makes `denyWritePaths` redundant). `denyDNS: true` has no additional effect when `allowNetwork` is `false`. `passEnv` is ignored when `allowEnv` is set. An `allowUnixSockets` entry that is not a live socket at command time is dropped (never fails the command) and never lifts a credential deny that covers it. A missing or unresolvable `denyWritePaths` entry fails sandbox construction and is checked again before every command; neither backend can reliably reserve a nonexistent protected object. On both platforms, writable ancestors of a protected entry are pinned against relocation so moving an ancestor cannot expose a replacement at the original path.

#### Examples

Sandbox with defaults:
```json
{
  "title": "my_tool",
  "type": "object",
  "sandbox": true,
  "properties": { ... }
}
```

Network access and extra write paths:
```json
{
  "title": "api_tool",
  "type": "object",
  "sandbox": { "allowNetwork": true, "writablePaths": ["/tmp/data"] },
  "properties": { ... }
}
```

Network access with DNS blocked on macOS and the default resolver suppressed on
Linux (best effort; a hard-coded resolver remains reachable on Linux):
```json
{
  "title": "ip_only_tool",
  "type": "object",
  "sandbox": { "allowNetwork": true, "denyDNS": true },
  "properties": { ... }
}
```

Deploy tool that needs AWS credentials and specific env vars:
```json
{
  "title": "deploy_tool",
  "type": "object",
  "sandbox": {
    "allowNetwork": true,
    "writablePaths": ["/tmp/deploy"],
    "readPaths": ["~/.aws"],
    "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
  },
  "properties": { ... }
}
```

Fully read-only sandbox (no writes anywhere, not even temp):
```json
{
  "title": "readonly_tool",
  "type": "object",
  "sandbox": { "denyWrite": true },
  "properties": { ... }
}
```

## MCP Servers

Pollytool speaks the Model Context Protocol. Servers are declared in a JSON
config file (the Claude Desktop format), and a *server spec* names the file
plus, optionally, one server in it: `"mcp.json"` or `"mcp.json#filesystem"`.

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    },
    "remote": {
      "url": "https://example.com/mcp",
      "transport": "streamable"
    }
  }
}
```

### Loading through the registry (recommended)

`ToolRegistry.LoadMCPServer` applies the registry's sandbox policy before
starting local stdio servers, and namespaces the tools it finds:

```go
registry := tools.NewToolRegistry(nil,
    tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()),
)

result, err := registry.LoadMCPServer("./mcp.json#filesystem")
if err != nil {
    panic(err)
}
for _, server := range result.Servers {
    fmt.Printf("%s: %v\n", server.Name, server.ToolNames)
}

// MCP tools now sit in the registry next to everything else
req := &llm.CompletionRequest{
    Model:    "openai/gpt-5.4",
    Messages: messages.User("List files in the current directory"),
    Tools:    registry.All(),
}
```

A server config may include `"sandbox"` overrides just like a shell tool
schema, and `"sandbox": false` is refused unless the registry opted in with
`tools.WithUnsafeNoSandbox()`.

### Direct client

If you're not using a registry, `NewUnsafeMCPClient` connects without any
sandboxing (the name is the warning):

```go
mcpClient, err := tools.NewUnsafeMCPClient("./mcp.json#filesystem")
if err != nil {
    panic(err)
}
defer mcpClient.Close()

mcpTools, err := mcpClient.ListTools()
if err != nil {
    panic(err)
}
registry := tools.NewToolRegistry(mcpTools)
```

Some servers worth knowing about: `@modelcontextprotocol/server-filesystem`,
`server-github`, `server-gitlab`, `server-postgres`, `server-sqlite`.

## Skills

Skills are directories of model instructions that can be activated on demand.
Load a catalog, wire it into a registry-backed runtime, and set it on requests:

```go
package main

import (
    "fmt"

    "github.com/alexschlessinger/pollytool/llm"
    "github.com/alexschlessinger/pollytool/messages"
    "github.com/alexschlessinger/pollytool/skills"
    "github.com/alexschlessinger/pollytool/tools"
    "github.com/alexschlessinger/pollytool/tools/sandbox"
)

func main() {
    // Expands ~, dedupes, validates. Empty input falls back to ~/.pollytool/skills.
    catalog, err := skills.LoadCatalog([]string{"~/my-skills"})
    if err != nil {
        panic(err)
    }
    if catalog == nil {
        fmt.Println("no skills found — carrying on without them")
        return
    }

    registry := tools.NewToolRegistry(nil,
        tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()),
    )
    skillRuntime, err := tools.NewSkillRuntime(catalog, registry)
    if err != nil {
        panic(err)
    }

    // Set Skills on a request and the skill prompt is injected automatically.
    req := &llm.CompletionRequest{
        Model:    "openai/gpt-5.4",
        Messages: messages.User("hi"),
        Skills:   catalog,
    }
    _ = req // pass it to ChatCompletionStream as usual

    // Or take manual control of the system prompt:
    systemPrompt := catalog.RuntimeSystemPrompt("You are a helpful assistant")
    _ = systemPrompt

    // Activate a skill from application code
    if _, err := skillRuntime.Activate("code-reviewer"); err != nil {
        panic(err)
    }

    // Persist active skills across runs
    saved := skillRuntime.ActivatedSkills()
    if err := skillRuntime.Restore(saved); err != nil {
        panic(err)
    }
}
```

`skills.ResolveDirs` is the standalone directory-resolution step if you want
it separately; `LoadCatalog` already calls it for you.

## Sessions

Sessions persist conversation history and artifacts. Disk-backed and ephemeral
stores use the same SQLite implementation; only `StoreConfig` changes:

```go
store, err := sessions.OpenStore(sessions.StoreConfig{
    Mode:           sessions.ModeDisk,
    Path:           "/path/to/polly.db",
    AutoSessionTTL: 7 * 24 * time.Hour,
})
if err != nil {
    panic(err)
}
defer func() {
    if err := store.Close(); err != nil {
        panic(err)
    }
}()

session, err := store.Acquire(ctx, "my-session-id", sessions.AcquireOptions{})
if err != nil {
    panic(err)
}
defer func() {
    if err := session.Close(); err != nil {
        panic(err)
    }
}() // releases this process's exclusive lease

sessionCtx := session.Context()
if err := session.AddMessage(sessionCtx, messages.ChatMessage{
    Role:    messages.MessageRoleUser,
    Content: "Hello!",
}); err != nil {
    panic(err)
}

history, err := session.GetHistory(sessionCtx)
if err != nil {
    panic(err)
}
_ = history // feed this to CompletionRequest.Messages

if err := session.Clear(sessionCtx); err != nil {
    panic(err)
}

// Replace settings and clear transcript/artifacts in one transaction. Reset
// preserves the session's canonical name and creation time.
metadata, err := session.GetMetadata(sessionCtx)
if err != nil {
    panic(err)
}
metadata.SystemPrompt = "A new system prompt"
if err := session.Reset(sessionCtx, metadata); err != nil {
    panic(err)
}
```

Notes:

- `ModeDisk` requires an explicit database path. The Polly CLI uses
  `~/.pollytool/polly.db`; paths are used literally, so library callers must
  expand `~` themselves.
- For tests or ephemeral use, select `ModeMemory` and omit `Path`. Schema,
  transactions, artifacts, leases, and retention behavior are otherwise the
  same.
- `AcquireOptions{Auto: true}` marks a newly created generated session for
  `AutoSessionTTL` retention. Explicitly named sessions do not expire by
  default, and reopening an existing session never changes its retention
  class.
- `Acquire` holds an exclusive session lease. Always close both the session and
  store, and use `session.Context()` for work that should stop if the lease is
  lost. A competing owner eventually receives `sessions.ErrSessionInUse`.
- `session.ArtifactStore()` is mandatory and scoped to the session. Artifact
  bytes and their ownership links are committed in the same database as the
  transcript.
- The SQLite format is a clean break from the former per-context JSON store.
  Legacy `~/.pollytool/contexts` files are neither imported nor removed, so the
  first run after the cutover starts with an empty SQLite session catalog.

## Structured Output

Three ways to get a schema, in decreasing order of convenience:

```go
// 1. Reflect it from a struct (strict mode, required = non-omitempty fields)
schema1 := llm.SchemaFor(UserInfo{})

// 2. Parse schema JSON you already have
schema2 := llm.SchemaFromJSON(`{
    "type": "object",
    "properties": {"name": {"type": "string"}},
    "required": ["name"]
}`)

// 3. Build the raw map yourself
schema3 := &llm.Schema{
    Raw: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "name":  map[string]any{"type": "string"},
            "age":   map[string]any{"type": "integer"},
            "email": map[string]any{"type": "string"},
        },
        "required": []string{"name", "email"},
    },
}
```

Set one on a request via `ResponseSchema`, or let `llm.StructuredComplete`
handle the request *and* the unmarshaling in one call (see
[Helpers](#structured-output-the-easy-way)).

```go
req := &llm.CompletionRequest{
    Model:          "openai/gpt-5.4",
    Messages:       messages.User("Extract user info from: John Doe, 30, john@example.com"),
    ResponseSchema: schema1,
}
```

## Error Handling

Errors arrive as events on the same channel as everything else:

```go
for event := range client.ChatCompletionStream(ctx, req, processor) {
    switch event.Type {
    case messages.EventTypeError:
        if strings.Contains(event.Error.Error(), "rate limit") {
            time.Sleep(5 * time.Second) // back off and retry
        } else if strings.Contains(event.Error.Error(), "context length") {
            req.Messages = req.Messages[len(req.Messages)-5:] // truncate history
        }
    }
}
```

## A Complete Example

Everything together — a session-backed streaming chat:

```go
package main

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
    "time"

    "github.com/alexschlessinger/pollytool/llm"
    "github.com/alexschlessinger/pollytool/messages"
    "github.com/alexschlessinger/pollytool/sessions"
)

func main() {
    ctx := context.Background()

    client := llm.GetDefaultClient()

    home, err := os.UserHomeDir()
    if err != nil {
        panic(err)
    }
    store, err := sessions.OpenStore(sessions.StoreConfig{
        Mode:           sessions.ModeDisk,
        Path:           filepath.Join(home, ".pollytool", "polly.db"),
        AutoSessionTTL: 7 * 24 * time.Hour,
    })
    if err != nil {
        panic(err)
    }
    defer func() {
        if err := store.Close(); err != nil {
            panic(err)
        }
    }()

    session, err := store.Acquire(ctx, "example-session", sessions.AcquireOptions{})
    if err != nil {
        panic(err)
    }
    defer func() {
        if err := session.Close(); err != nil {
            panic(err)
        }
    }()
    sessionCtx := session.Context()

    // Seed the system prompt on first run
    history, err := session.GetHistory(sessionCtx)
    if err != nil {
        panic(err)
    }
    if len(history) == 0 {
        if err := session.AddMessage(sessionCtx, messages.ChatMessage{
            Role:    messages.MessageRoleSystem,
            Content: "You are a helpful AI assistant.",
        }); err != nil {
            panic(err)
        }
    }

    if err := session.AddMessage(sessionCtx, messages.ChatMessage{
        Role:    messages.MessageRoleUser,
        Content: "Tell me a joke",
    }); err != nil {
        panic(err)
    }
    history, err = session.GetHistory(sessionCtx)
    if err != nil {
        panic(err)
    }

    req := &llm.CompletionRequest{
        Model:       "openai/gpt-5.4",
        Messages:    history,
        Temperature: llm.Float32Ptr(0.7),
        MaxTokens:   500,
        Timeout:     30 * time.Second,
    }

    processor := messages.NewStreamProcessor()
    fmt.Print("Assistant: ")

    for event := range client.ChatCompletionStream(sessionCtx, req, processor) {
        switch event.Type {
        case messages.EventTypeContent:
            fmt.Print(event.Content)
        case messages.EventTypeComplete:
            if err := session.AddMessage(sessionCtx, *event.Message); err != nil {
                panic(err)
            }
            fmt.Println()
        case messages.EventTypeError:
            fmt.Printf("\nError: %v\n", event.Error)
            return
        }
    }
}
```

## Thread Safety

- `MultiPass` is stateless and safe for concurrent use.
- `SQLiteStore` is safe for concurrent use. Different sessions can be active
  concurrently; acquiring the same session from another process or store waits
  for its exclusive lease and then returns `ErrSessionInUse`.
- `GetHistory` and `GetMetadata` return detached copies. Mutations are
  transactional, but compound read-modify-write sequences across multiple
  calls still need application-level coordination.
- Tool `Execute` implementations should be safe to call concurrently — the
  library doesn't serialize them for you.
