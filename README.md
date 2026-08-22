# Pollytool (polly)

<img src=".assets/polly.png" width="128" height="128">

https://en.wikipedia.org/wiki/Stochastic_parrot

This is my llm cli tool. There are many like it, but this one is mine.

## Command-Line Options
```
NAME:
   polly - Chat with LLMs using various providers

USAGE:
   polly [global options] [command [command options]]

COMMANDS:
   embed    Generate embedding vectors for text input
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --model string, -m string                                Model to use (provider/model format) (default: "anthropic/claude-sonnet-4-6") [$POLLYTOOL_MODEL]
   --temp float                                             Temperature for sampling (default: 1) [$POLLYTOOL_TEMP]
   --maxtokens int                                          Maximum tokens to generate (default: 64000) [$POLLYTOOL_MAXTOKENS]
   --maxiterations int                                      Maximum agent iterations (LLM calls) before stopping (default: 250) [$POLLYTOOL_MAXITERATIONS]
   --timeout duration                                       Request timeout (default: 2m0s) [$POLLYTOOL_TIMEOUT]
   --thinking string                                        Reasoning effort: off, dynamic, a level (minimal, low, medium, high, xhigh, max), or a token budget (e.g. 12000) (default: "off") [$POLLYTOOL_THINKING]
   --baseurl string                                         Base URL for API (for OpenAI-compatible endpoints or Ollama) [$POLLYTOOL_BASEURL]
   --skilldir string [ --skilldir string ]                  Skill directory or directory containing skill folders (can be specified multiple times) [$POLLYTOOL_SKILLDIR]
   --skill string, -S string [ --skill string, -S string ]  Skill to load: local directory, git repo URL, or archive URL. Auto-activated on start.
   --noskills                                               Disable Agent Skill discovery and runtime skill tools
   --listskills                                             List discovered Agent Skills
   --tool string, -t string [ --tool string, -t string ]    Tool provider: shell script (provides 1 tool) or MCP server (can provide multiple tools). Can be specified multiple times
   --tooltimeout duration                                   Timeout for tool execution (default: 30s) [$POLLYTOOL_TOOLTIMEOUT]
   --prompt string, -p string                               Initial prompt (reads from stdin if not provided; starts REPL when neither is provided)
   --system string, -s string                               System prompt (default: "Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.") [$POLLYTOOL_SYSTEM]
   --file string, -f string [ --file string, -f string ]    File, image, or URL to include (can be specified multiple times)
   --schema string                                          Path to JSON schema file for structured output
   --context string, -c string                              Context name for conversation continuity [$POLLYTOOL_CONTEXT]
   --last, -L                                               Use the last active context
   --reset string                                           Reset the specified context (clear conversation history, keep settings)
   --list                                                   List all available context IDs
   --delete string                                          Delete the specified context
   --add                                                    Add stdin content to context without making an API call
   --purge                                                  Delete all sessions and index (requires confirmation)
   --create string                                          Create a new context with specified name and configuration
   --show string                                            Show configuration for the specified context
   --maxcontext int                                         Maximum tokens to keep in history (0 = unlimited) (default: 100000)
   --confirm                                                Require confirmation before each tool call
   --nosandbox                                              Disable sandboxing of tool commands [$POLLYTOOL_NOSANDBOX]
   --quiet                                                  Suppress status and tool display output
   --debug, -d                                              Enable debug logging
   --help, -h                                               show help
```

## Features

- **Many Models**: OpenAI, Anthropic, Gemini, DeepSeek, OpenRouter, Ollama.
- **Multimodal**: Text, pics, random files.
- **Structured Output**: JSON on purpose, not by accident.
- **Tool Calling**: Bolt on shell scripts & MCP servers.
- **Agent Skills**: Discover `SKILL.md` bundles, activate them on demand, and expose bundled helper scripts as tools.
- **Contexts**: Memory, but opt‑in.
- **Interactive TUI**: A full-screen terminal UI (scrollback, history search, bracketed paste, slash commands) when you launch `polly` with no prompt.
- **Streaming**: Words appear while it thinks.
- **API**: Do the things yourself [docs](API.md)

## Installation

```bash
go build -o polly ./cmd/polly/
```

## Quick Start

```bash
export POLLYTOOL_ANTHROPICKEY=...
export POLLYTOOL_OPENAIKEY=...

# Bare polly launches the interactive TUI
polly

# Basic
echo "Hello?" | polly

# Pick a model
echo "Quantum computing in one breath" | polly -m openai/gpt-5.4

# Image
polly -f image.jpg -p "What’s this?"

# Remote image
polly -f https://example.com/image.png -p "Describe it"

# Mixed bag
polly -f notes.txt -f https://example.com/chart.png -p "Tie these together"

# Tools example - auto-detects shell tools vs MCP servers
./polly -p "uppercase this: hello" --tool ./uppercase.sh
./polly -p "create news.txt with today's news" --tool perp.json --tool filesystem.json

# Agent Skills
./polly --skilldir ~/.pollytool/skills --listskills
./polly --skilldir ~/.pollytool/skills -p "review this patch for regressions"
```
## Interactive TUI

Running `polly` with no `--prompt` and no piped stdin drops you into a full-screen
terminal UI (built on tcell/gotui). It supports streaming responses, scrollback,
reverse history search (Ctrl-R), bracketed paste, and tool/skill display. If the
terminal isn't a TTY (e.g. `TERM=dumb` or redirected I/O), polly falls back to
plain one-shot mode.

Slash commands inside the TUI:

```
/help [command]              Show help
/clear                       Clear the conversation
/context  (/stats)           Show context info and token stats
/get <key|all>               Inspect current settings
/tools [list [ns]|show <n>]  List or inspect loaded tools
/skills                      List discovered Agent Skills
/exit  (/quit)               Leave the TUI
```

Ctrl-C interrupts an in-flight turn; pressing it again (or at an idle prompt)
quits.

### Model Selection

The default model is `anthropic/claude-sonnet-4-6`. Override with `-m` flag:


### Create and Use Named Contexts

```bash
# Create a new named context with configuration
polly --create project --model openai/gpt-5.4 --maxtokens 4096

# Show context configuration
polly --show project

# Use the context in one-shot mode
echo "I'm working on a Python web app" | polly -c project

# Continue the conversation
polly -c project -p "What database should I use?"

# Or continue interactively in the REPL
polly -c project

# Reset a context (clear conversation, keep settings)
polly --reset project

# List all contexts
polly --list

# Delete a context
polly --delete project

# Delete all contexts (requires confirmation)
polly --purge
```

### Context Settings Persistence

Contexts remember your settings (model, temperature, system prompt, active tools) between conversations:

```bash
# First use - settings are saved
polly -c helper -m gemini/gemini-3.1-pro-preview -s "You are a SQL expert" -p "Hello"

# Later uses - settings are automatically restored
polly -c helper -p "Write a complex JOIN query"

# Use the last active context
polly --last -p "Explain the query"

```

### Settings Priority

Polly manages context settings with a clear priority system:

1. **Settings Persistence**  
  When you use a context, your current settings (model, temperature, system prompt, tools) are automatically saved to that context's metadata.

2. **Settings Inheritance**  
  When you resume a context, Polly restores the previously saved settings—unless you override them with command-line flags.

3. **Settings Priority**  
  Command-line flags always take precedence over stored context settings.  
  *Example:*  
  If your context uses `openai/gpt-5.4` but you run `-m openai/gpt-5.4-mini`, Polly switches to GPT-5.4-mini and saves this change for future use.

4. **System Prompt Changes**  
  If you change the system prompt for a context with existing conversation history, Polly automatically resets the conversation to keep things consistent.

## Tool Management

Polly now provides unified tool management for both shell scripts and MCP servers:

### Command-Line Tool Loading

Tools are auto-detected based on file type:
- **Shell scripts** (`.sh` files): Loaded as individual tools
- **MCP servers** (`.json` files): Can provide multiple tools from one server

```bash
# Load a shell tool
polly -t ./uppercase.sh -p "make this LOUD: hello"

# Load an MCP server (JSON config)
polly -t filesystem.json -p "list files in /tmp"

# Mix both types
polly -t ./mytool.sh -t perplexity.json -p "search and process"
```


### Tool Namespacing

To avoid conflicts, tools are automatically namespaced:
- Shell tools: `scriptname__toolname` (e.g., `uppercase__to_uppercase`)
- MCP tools: `servername__toolname` (e.g., `filesystem__read_file`)

### Tool Persistence

When tools are loaded in a context, they're automatically saved and restored:
```bash
# Tools are saved with the context
polly -c project -t ./build.sh -p "build the project"

# Later, tools are automatically restored
polly -c project -p "run tests"  # build.sh is still available
```

## Agent Skills

Polly can discover [Agent Skills](https://agentskills.io/specification) from one or more directories. Each skill lives in a folder named after the skill and contains a `SKILL.md` manifest with YAML frontmatter.

At runtime Polly:
- advertises discovered skills in the system prompt
- exposes `activate_skill` and `read_skill_file` native tools
- loads executable files under a skill's `scripts/` directory as normal Polly shell tools, namespaced by skill name, when the skill is activated
- loads Claude Desktop style MCP configs from a skill's optional `mcp/` directory, namespaced by skill and server name, when the skill is activated
- enforces `allowed-tools` on future turns after activation, matching Polly tool names with `*` glob support; skill-bundled tools remain auto-approved, and multiple skill activations combine their allowlists additively

`allowed-tools` is additive for the duration of the run: activating another skill can widen access, but it does not revoke tools that were already allowed by a previously activated skill.

Examples:

```bash
# Use the default ~/.pollytool/skills directory if it exists
polly --listskills

# Point at one or more explicit skill directories
polly --skilldir ~/.pollytool/skills --skilldir ./skills --listskills

# Run with skills enabled
polly --skilldir ~/.pollytool/skills -p "help me review this Go change"

# Disable skills for a run
polly --noskills -p "summarize this file"
```

## Structured Output

Use JSON schemas to get structured, validated responses:

```bash
# Create a schema file
cat > person.schema.json << 'EOF'
{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "integer"},
    "email": {"type": "string"}
  },
  "required": ["name", "age"]
}
EOF

# Extract structured data
echo "John Doe is 30 years old, email: john@example.com" | \
  polly --schema person.schema.json
```

Output:
```json
{
  "name": "John Doe",
  "age": 30,
  "email": "john@example.com"
}
```

### Image Analysis with Schema

```bash
# Analyze image with structured output
polly -f receipt.jpg --schema receipt.schema.json
```

## Tool Integration

### Shell Tools

Create executable scripts that follow the tool protocol:

```bash
#!/bin/bash
# uppercase_tool.sh

if [ "$1" = "--schema" ]; then
  cat <<SCHEMA
{
  "title": "uppercase",
  "description": "Convert text to uppercase",
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Text to convert"}
  },
  "required": ["text"]
}
SCHEMA
elif [ "$1" = "--execute" ]; then
  echo "$2" | jq -r .text | tr '[:lower:]' '[:upper:]'
fi
```

Use the tool:
```bash
polly -t ./uppercase_tool.sh -p "Convert 'hello world' to uppercase"
```

#### Sandboxing

The builtin `bash` tool, shell tools, and stdio MCP servers run **sandboxed by default**. A sandboxed process runs with a read-only root filesystem, writes restricted to the policy's writable paths, sensitive paths (`~/.ssh`, `~/.aws`, `~/.gnupg`, ...) blocked from reads, and credential/runtime env vars (`POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, `SSH_AUTH_SOCK`, `DBUS_SESSION_BUS_ADDRESS`, ...) stripped. On Linux, `/tmp` and `/run` are private, filesystem Unix sockets are blocked, and the process gets private PID and IPC namespaces plus a detached session. Shell tools get a `[sandboxed]` suffix on their description so the LLM knows they're restricted. Extra paths can be blocked globally with `--denypath` (or `POLLYTOOL_DENYPATHS`).

**Presets** — `--sandbox <preset>` (env `POLLYTOOL_SANDBOX`) selects the base policy for all sandboxed tools. Components join with `+`:

| Preset | Meaning |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | no writes at all, not even temp; no network |
| `workspace` | the working directory is writable; discovered Git routing entries and metadata trees stay read-only so a tool cannot replace `.git` or alter repository-local hook/config entry points |
| `net` | outbound network allowed |

The default is **`workspace+net`**: tools can edit the project and reach the network, but credentials stay masked and everything outside the workspace is read-only. Tighten with `--sandbox base` or `--sandbox readonly` when tools only need to compute or inspect. Granular additions: `--writepath <dir>` (repeatable, env `POLLYTOOL_WRITEPATHS`) grants extra writable paths and `--allownet` (env `POLLYTOOL_ALLOWNET`) grants network on top of any preset.

Sandboxing requires the fixed system `/usr/bin/bwrap` (Linux), or fixed system `/usr/bin/sandbox-exec` plus `/usr/bin/perl` (macOS). If the required trusted backend is unavailable, Polly refuses to run sandboxed tools rather than silently running them unsandboxed. Disable with `--nosandbox` or `POLLYTOOL_NOSANDBOX=true`. See [API.md](API.md) for the full spec and [SANDBOX.md](SANDBOX.md) for design intent and platform differences.

**Full example** — `"sandbox": true` inherits the CLI preset; add `--sandbox base` to limit writes to `$TMPDIR` with no network:

```bash
cat > ./sandboxed_uppercase.sh << 'EOF'
#!/bin/bash
if [ "$1" = "--schema" ]; then
  cat <<'SCHEMA'
{
  "title": "sandboxed_uppercase",
  "description": "Uppercase input text",
  "type": "object",
  "sandbox": true,
  "properties": {"text": {"type": "string"}},
  "required": ["text"]
}
SCHEMA
elif [ "$1" = "--execute" ]; then
  echo "$2" | jq -r .text | tr '[:lower:]' '[:upper:]'
fi
EOF
chmod +x ./sandboxed_uppercase.sh

polly -t ./sandboxed_uppercase.sh -p "uppercase 'hello world' using the tool"
```

**Config variations** — replace `"sandbox": true` in the schema with any of:

```jsonc
// Grant network + extra writable path
"sandbox": { "allowNetwork": true, "writablePaths": ["/tmp/data"] }

// Allow network but block DNS (IP-only connections)
"sandbox": { "allowNetwork": true, "denyDNS": true }

// Unmask specific sensitive paths and pass through selected env vars
"sandbox": {
  "readPaths": ["~/.aws"],
  "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
}

// Fully read-only — no writes, not even to temp
"sandbox": { "denyWrite": true }

// Tool metadata cannot disable the sandbox by itself. The caller must make
// the explicit global unsafe choice with --nosandbox.
```

Credential-shaped env vars (`POLLYTOOL_*`, `AWS_*`, names ending in `_API_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD`, ..., plus agent sockets like `SSH_AUTH_SOCK`) are stripped from sandboxed processes unless explicitly listed in `allowEnv`.

Relative `writablePaths`, `readPaths`, and `denyPaths` entries resolve against
Polly's working directory when the sandbox is created. A child read exemption
such as `~/.ssh/config` exposes only that path; the rest of the denied parent
remains hidden. `denyWritePaths` entries carve read-only islands out of a
writable tree. The `workspace` preset recursively discovers regular, nested,
submodule, and linked-worktree `.git` entries and protects each routing entry,
per-worktree gitdir, `commondir` pointer, and common gitdir. This intentionally
blocks direct writes into those Git metadata trees (index/ref updates as well
as hooks/config) while leaving working-tree files writable. Bare-repository
working directories and symlinked Git routing or executable/config metadata
are refused because their identity cannot be pinned portably. Hard-linked
protected files, repository-local `core.hooksPath`, and config includes are
refused for the same reason: protecting the config pathname would not protect
the writable alias or redirected target. Polly accepts either a stable route
to fixed OS Git or, on Darwin, a standard Homebrew `bin/git` leaf symlink to a
non-writable, single-link Cellar target; every accepted route and target must stay
outside writable paths. It executes the resolved selected Git with a
routing-free environment, preserving that Git's compiled config-prefix
semantics. It checks effective and overridden
global/system `core.hooksPath` values plus every nested include (even inactive
`includeIf` branches). The workspace preset is refused when a hook target,
selected config source, or config include is in host-visible writable content
(including macOS temp trees) outside protected Git metadata; hard-linked config
sources and symlinked or hard-linked configured hook entries are refused as
mutable aliases. `/dev/null` remains a supported `core.hooksPath` value for
disabling hooks. The config/hook audit is repeated when each sandbox is
constructed, after CLI `--writepath` and per-tool `writablePaths` overlays are
merged, so a later grant cannot make an otherwise external config, include, or
hook target plantable.

Writable and read-exemption paths are canonicalized when the effective policy
is prepared. Missing grants are dropped, and the approved filesystem identities
survive later per-tool merges; replacing or rerouting one before another tool
is loaded fails closed instead of silently widening that later sandbox.

**MCP server sandboxing** — set `sandbox` on each server entry in the config:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp/workspace"],
      "sandbox": true
    },
    "api_proxy": {
      "command": "python",
      "args": ["proxy.py"],
      "sandbox": { "allowNetwork": true, "writablePaths": ["/tmp/proxy"] }
    }
  }
}
```

Tools and servers without a `sandbox` field get the CLI preset (`workspace+net` unless `--sandbox` says otherwise). A tool's own `sandbox` config merges on top of the preset and can widen it, never narrow it. A `"sandbox": false` request is refused unless the caller also made the explicit global unsafe choice with `--nosandbox`.

### MCP Servers

MCP servers are configured through JSON files using the Claude Desktop format. A single config file can define multiple servers:

```bash
# Create an MCP server config
cat > mcp.json << 'EOF'
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/workspace"]
    },
    "perplexity": {
      "command": "uvx",
      "args": ["perplexity-mcp"],
      "env": {
        "PERPLEXITY_API_KEY": "pplx-..."
      }
    }
  }
}
EOF

# Load all servers from the config
polly -t mcp.json -p "List files and search for tutorials"

# Or load a specific server
polly -t mcp.json#filesystem -p "List files in the workspace"
```

#### Remote MCP Servers

MCP servers can also connect via SSE or Streamable HTTP transports:

```json
{
  "mcpServers": {
    "remote-api": {
      "transport": "sse",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "Bearer ..."
      },
      "timeout": "60s"
    }
  }
}
```

#### MCP Server Examples

```bash
cat > servers.json << 'EOF'
{
  "mcpServers": {
    "perplexity": {
      "command": "uvx",
      "args": ["perplexity-mcp"],
      "env": {
        "PERPLEXITY_API_KEY": "pplx-..."
      }
    },
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/files"]
    },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_TOKEN": "ghp_..."
      }
    }
  }
}
EOF
```


### Custom API Endpoints

```bash
# Use a custom Ollama endpoint
polly --baseurl http://192.168.1.100:11434 -m ollama/gpt-oss -p "Hello"

# Use OpenAI-compatible endpoint
polly --baseurl https://api.openrouter.ai/api/v1 -m openai/whatevermodel -p "Hello"
```


## Provider-Specific Notes

### OpenAI
- Supports GPT-5.4 and its distills (5.4-mini, 5.4-nano)
- Native OpenAI uses the Responses API when `--baseurl` is not set
- OpenAI-compatible endpoints stay on Chat Completions when `--baseurl` is set
- Structured output uses `additionalProperties: false` in schema
- Strict tool schemas with optional parameters are downgraded to non-strict on native OpenAI Responses
- Reliable schema support
- Built-in Responses tools are not exposed yet

### Anthropic
- Supports Claude family (Opus, Sonnet, Haiku)
- Uses tool-use pattern for structured output
- Excellent for long-form content and analysis
- Mostly reliable schema support

### Gemini
- Supports Gemini Pro and Flash models
- Good balance of speed and capability
- Reliable schema output support via ResponseSchema

### DeepSeek
- Supports DeepSeek's hosted models (e.g. `deepseek-v4-pro`, `deepseek-v4-flash`)
- API key in `POLLYTOOL_DEEPSEEKKEY`
- Reasoning models emit a non-standard `reasoning_content` field that the API requires echoed back on follow-up turns; the provider captures and replays it automatically, so tool use works without configuration

```bash
export POLLYTOOL_DEEPSEEKKEY=...
polly -m deepseek/deepseek-v4-pro -p "Hello!"
```

### OpenRouter
- Routes to many upstream providers through one OpenAI-compatible endpoint
- API key in `POLLYTOOL_OPENROUTERKEY`
- Use the upstream `provider/model` slug after the `openrouter/` prefix

```bash
export POLLYTOOL_OPENROUTERKEY=...
polly -m openrouter/anthropic/claude-sonnet-4-5 -p "Hello!"
polly -m openrouter/openai/gpt-5 -p "Hello!"
polly -m openrouter/deepseek/deepseek-chat -p "Hello!"
```

### Ollama
- Requires Ollama installation
- Supports any model available in Ollama
- Use --baseurl for remote instances
- Schema support hit and miss, depends on model

## See Also

- [Soulshack](https://github.com/pkdindustries/soulshack) - An IRC chatbot that uses Polly for LLM features.

## License

MIT
