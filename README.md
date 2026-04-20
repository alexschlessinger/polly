# Pollytool (polly)

<img src=".assets/polly.png" width="128" height="128">

https://en.wikipedia.org/wiki/Stochastic_parrot

This is my llm cli tool. There are many like it, but this one is mine.

## Command-Line Options
```
NAME:
   polly - Chat with LLMs using various providers

USAGE:
   polly [global options]

GLOBAL OPTIONS:
   --model string, -m string                              Model to use (provider/model format) (default: "anthropic/claude-sonnet-4-20250514")
   --temp float                                           Temperature for sampling (default: 1)
   --maxtokens int                                        Maximum tokens to generate (default: 4096)
   --timeout duration                                     Request timeout (default: 2m0s)
   --think                                                Enable thinking/reasoning (low effort) (default: false)
   --think-medium                                         Enable thinking/reasoning (medium effort) (default: false)
   --think-hard                                           Enable thinking/reasoning (high effort) (default: false)
   --baseurl string                                       Base URL for API (for OpenAI-compatible endpoints or Ollama)
   --skilldir string [ --skilldir string ]              Skill directory or directory containing skill folders (can be specified multiple times)
   --noskills                                            Disable Agent Skill discovery and runtime skill tools (default: false)
   --listskills                                          List discovered Agent Skills (default: false)
   --tool string, -t string [ --tool string, -t string ]  Tool provider: shell script or MCP server (can be specified multiple times)
   --tooltimeout duration                                 Tool execution timeout (default: 2m0s)
   --prompt string, -p string                             Initial prompt (reads from stdin if not provided)
   --system string, -s string                             System prompt (default: "Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.")
   --file string, -f string [ --file string, -f string ]  File, image, or URL to include (can be specified multiple times)
   --schema string                                        Path to JSON schema file for structured output
   --context string, -c string                            Context name for conversation continuity (uses POLLYTOOL_CONTEXT env var if not set)
   --last, -L                                             Use the last active context (default: false)
   --reset                                                Reset context (clear conversation history, keep settings) (default: false)
   --list                                                 List all available context IDs (default: false)
   --delete string                                        Delete the specified context
   --add                                                  Add stdin content to context without making an API call (default: false)
   --purge                                                Delete all sessions and index (requires confirmation) (default: false)
   --create string                                        Create a new context with specified name and configuration
   --show string                                          Show configuration for the specified context
   --quiet                                                Suppress confirmation messages (default: false)
   --debug, -d                                            Enable debug logging (default: false)
   --help, -h                                             show help
```

## Features

- **Many Models**: OpenAI, Anthropic, Gemini, Ollama.
- **Multimodal**: Text, pics, random files.
- **Structured Output**: JSON on purpose, not by accident.
- **Tool Calling**: Bolt on shell scripts & MCP servers.
- **Agent Skills**: Discover `SKILL.md` bundles, activate them on demand, and expose bundled helper scripts as tools.
- **Contexts**: Memory, but opt‑in.
- **Streaming**: Words appear while it thinks.
- **API**: Do the things yourself [docs](API.md)

## Installation

```bash
go build -o polly ./cmd/polly/
```

## Quick Start

```bash
export POLLYTOOL_ANTHROPICKEY=...
export POLYTOOL_OPENAIKEY=....

# Basic
echo "Hello?" | polly

# Pick a model
echo "Quantum computing in one breath" | polly -m openai/gpt-4.1

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
### Model Selection

The default model is `anthropic/claude-sonnet-4-20250514`. Override with `-m` flag:


### Create and Use Named Contexts

```bash
# Create a new named context with configuration
polly --create project --model openai/gpt-4.1 --maxtokens 4096

# Show context configuration
polly --show project

# Use the context (prompt required via -p or piped input)
echo "I'm working on a Python web app" | polly -c project

# Continue the conversation
polly -c project -p "What database should I use?"

# Reset a context (clear conversation, keep settings)
polly --reset -c project

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
polly -c helper -m gemini/gemini-2.5-pro -s "You are a SQL expert" -p "Hello"

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
  If your context uses `openai/gpt-5` but you run `-m openai/gpt-4.1`, Polly switches to GPT-4.1 and saves this change for future use.

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
  text=$(echo "$2" | jq -r .text)
  echo "${text^^}"
fi
```

Use the tool:
```bash
polly -t ./uppercase_tool.sh -p "Convert 'hello world' to uppercase"
```

#### Sandboxing

Shell tools and MCP servers can opt into sandboxing by setting `"sandbox"` to `true` (defaults) or an object with overrides. Opted-in tools run with restricted file writes (temp directory only, plus any explicit `writablePaths`), no network access, no reads to sensitive credential paths, and `POLLYTOOL_*` env vars stripped. The tool's description will include a `[sandboxed]` hint so the LLM knows the tool is restricted. If no supported sandbox backend is available, Polly exits with an error instead of running unsandboxed. Disable with `--nosandbox` or `POLLYTOOL_NOSANDBOX=true`. See [API docs](API.md) for the full sandbox spec reference.

```bash
# Shell tool — sandbox with defaults
if [ "$1" = "--schema" ]; then
  cat <<SCHEMA
{
  "title": "file_processor",
  "description": "Process files in the workspace",
  "type": "object",
  "sandbox": true,
  "properties": {
    "path": {"type": "string"}
  },
  "required": ["path"]
}
SCHEMA
fi
```

```bash
# Shell tool — sandbox with overrides
"sandbox": { "allowNetwork": true, "writablePaths": ["/tmp/data"] }

# Shell tool — sandbox with read paths and env filtering
"sandbox": {
  "allowNetwork": true,
  "writablePaths": ["/tmp/deploy"],
  "readPaths": ["~/.aws"],
  "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
}

# Shell tool — network access without DNS (connect by IP only)
"sandbox": { "allowNetwork": true, "denyDNS": true }

# Shell tool — fully read-only sandbox (no writes, not even temp)
"sandbox": { "denyWrite": true }
```

`POLLYTOOL_*` env vars (API keys) are always stripped from sandboxed processes unless explicitly included in `allowEnv`.

```json
// MCP server — sandbox the server process
{
  "mcpServers": {
    "filesystem": {
      "command": "node",
      "args": ["server.js"],
      "sandbox": true
    },
    "api_proxy": {
      "command": "python",
      "args": ["proxy.py"],
      "sandbox": { "allowNetwork": true }
    }
  }
}
```

Tools and servers that do not set `"sandbox"` run without restrictions, even when sandboxing is active.

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
polly --baseurl http://192.168.1.100:11434 -m ollama/llama3.2 -p "Hello"

# Use OpenAI-compatible endpoint
polly --baseurl https://api.openrouter.ai/api/v1 -m openai/whatevermodel -p "Hello"
```


## Provider-Specific Notes

### OpenAI
- Supports GPT-4, 4.1, 5, 5.4 and their distills
- Native OpenAI uses the Responses API when `--baseurl` is not set
- OpenAI-compatible endpoints stay on Chat Completions when `--baseurl` is set
- Structured output uses `additionalProperties: false` in schema
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

### Ollama
- Requires Ollama installation
- Supports any model available in Ollama
- Use --baseurl for remote instances
- Schema support hit and miss, depends on model

## See Also

- [Soulshack](https://github.com/pkdindustries/soulshack) - An IRC chatbot that uses Polly for LLM features.

## License

MIT
