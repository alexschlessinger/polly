# Pollytool (polly)

<img src="polly.png" width="128" height="128">

https://en.wikipedia.org/wiki/Stochastic_parrot

This is my llm cli tool. There are many like it, but this one is mine.


## Features

- **Many Models**: OpenAI, Anthropic, Gemini, Ollama.
- **Multimodal**: Text, pics, random files.
- **Structured Output**: JSON on purpose, not by accident.
- **Tool Calling**: Bolt on shell scripts & MCP servers.
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

# tools example
./polly -p "create a file in my workspace called news.txt with todays news" --mcp "uvx perplexity-mcp" --mcp "npx -y @modelcontextprotocol/server-filesystem /home/alex/workspace/"
```

## Configuration

### Environment Variables

#### API Keys
| Variable | Purpose |
|----------|---------|
| `POLLYTOOL_ANTHROPICKEY` | Claude |
| `POLLYTOOL_OPENAIKEY` | GPT |
| `POLLYTOOL_GEMINIKEY` | Gemini |
| `POLLYTOOL_OLLAMAKEY` | Ollama bearer (optional) |

#### Config
| Variable | Meaning | Default |
|----------|---------|---------|
| `POLLYTOOL_MODEL` | Default model | `anthropic/claude-sonnet-4-20250514` |
| `POLLYTOOL_TEMP` | Creative wiggle | `1.0` |
| `POLLYTOOL_MAXTOKENS` | Output cap | `4096` |
| `POLLYTOOL_TIMEOUT` | Give up after | `2m` |
| `POLLYTOOL_SYSTEM` | System prompt | `Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.` |
| `POLLYTOOL_BASEURL` | Custom endpoint | (unset) |
| `POLLYTOOL_CONTEXT` | Default context | (unset) |

### Model Selection

The default model is `anthropic/claude-sonnet-4-20250514`. Override with `-m` flag:


### Create and Use Named Contexts

```bash
# Create a new named context (auto-creates if doesn't exist)
polly -c project -p "I'm working on a Python web app"

# Continue the conversation
polly -c project -p "What database should I use?"

# Reset a context (clear conversation, keep settings)
polly --reset -c project -p "Let's start fresh"

# List all contexts
polly --list

# Delete a context
polly --delete project

# delete all contexts
polly --purge
```

### Context Settings Persistence

Contexts remember your settings (model, temperature, system prompt, tools) between conversations:

```bash
# First use - settings are saved
polly -c helper -m gemini/gemini-2.5-pro -s "You are a SQL expert" -p "Hello"

# Later uses - settings are automatically restored
polly -c helper -p "Write a complex JOIN query"

# Use the last active context
polly --last -p "Explain the query"

```


### Settings Priority

Settings are applied in this order (highest priority first):
1. **Command-line flags** - Always take precedence
2. **Context stored settings** - Remembered from previous uses
3. **Environment variables** - Default values
4. **Hard-coded defaults** - Fallback values


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

### MCP Servers

```bash
# Use MCP server
polly --mcp "npx @modelcontextprotocol/server-filesystem /path/to/files" \
      -p "List files in the directory"
```


### Custom API Endpoints

```bash
# Use a custom Ollama endpoint
polly --baseurl http://192.168.1.100:11434 -m ollama/llama3.2 -p "Hello"

# Use OpenAI-compatible endpoint
polly --baseurl https://api.openrouter.ai/api/v1 -m openai/whatevermodel -p "Hello"
```

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
   --baseurl string                                       Base URL for API (OpenAI-compatible endpoints or Ollama)
   --tool string, -t string [ --tool string, -t string ]  Shell tool executable path (can be specified multiple times)
   --mcp string [ --mcp string ]                          MCP server and arguments (can be specified multiple times)
   --prompt string, -p string                             Prompt (reads from stdin if not provided)
   --system string, -s string                             System prompt
   --file string, -f string [ --file string, -f string ]  File, image, or URL to include (can be specified multiple times)
   --schema string                                        Path to JSON schema file for structured output
   --context string, -c string                            Context name for conversation continuity (or uses POLLYTOOL_CONTEXT env var if set)
   --last, -L                                             Use the last active context (default: false)
   --reset                                                Reset context (clear conversation history, keep settings) - requires -c or --last
   --list                                                 List all available context IDs (default: false)
   --delete string                                        Delete the specified context
   --add                                                  Add stdin content to context without making an API call (default: false)
   --quiet                                                Suppress confirmation messages (default: false)
   --debug, -d                                            Enable debug logging (default: false)
   --help, -h                                             show help
```

## Provider-Specific Notes

### OpenAI
- Supports GPT-4, 4.1, 5 and their distills
- Structured output uses `additionalProperties: false` in schema
- Reliable schema support

### Anthropic
- Supports Claude family (Opus, Sonnet, Haiku)
- Uses tool-use pattern for structured output
- Excellent for long-form content and analysis
- Mostly reliable schema support

### Gemini
- Supports Gemini Pro and Flash models
- Native structured output support via ResponseSchema
- Good balance of speed and capability
- Reliable schema support

### Ollama
- Requires Ollama installation
- Supports any model available in Ollama
- Use --baseurl for remote instances
- Schema support hit and miss


## License

MIT
