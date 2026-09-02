# Pollytool (polly)

<img src=".assets/polly.png" width="128" height="128">

This is my [LLM](https://en.wikipedia.org/wiki/Stochastic_parrot) CLI tool.
There are many like it, but this one is mine.

- **Many models** — OpenAI, Anthropic, Gemini, DeepSeek, OpenRouter, Ollama,
  Hugging Face. One interface.
- **Interactive TUI** — streaming, scrollback, history search, images.
- **Multimodal** — text, pics, random files.
- **Contexts** — memory, but opt-in. Conversations persist in SQLite.
- **Tool calling** — bolt on shell scripts and MCP servers.
- **Sandboxed by default** — tool commands run in an OS-level sandbox that
  masks your credentials.
- **Agent Skills** — discover `SKILL.md` bundles and activate them on demand.
- **Structured output** — JSON on purpose, not by accident.

This file covers the CLI and the TUI. [API.md](API.md) is the Go library
tour; [SANDBOX.md](SANDBOX.md) is the sandbox reference.

## Installation

```bash
go build -o polly ./cmd/polly/
```

## Quick Start

```bash
export POLLYTOOL_ANTHROPICKEY=...
export POLLYTOOL_OPENAIKEY=...

polly                                   # bare polly opens the TUI
echo "Hello?" | polly                   # one-shot from stdin
polly -p "Hello?"                       # or from a flag
polly -m openai/gpt-5.4 -p "Quantum computing in one breath"

# Attach files, images, URLs
polly -f image.jpg -p "What's this?"
polly -f notes.txt -f https://example.com/chart.png -p "Tie these together"

# Tools: shell scripts and MCP servers, auto-detected by file type
polly -p "uppercase this: hello" --tool ./uppercase.sh
polly -p "create news.txt with today's news" --tool perp.json --tool filesystem.json
```

The default model is `anthropic/claude-sonnet-4-6`; override it with `-m
provider/model` or `POLLYTOOL_MODEL`.

## The Interactive TUI

`polly` with no `--prompt` and no piped stdin opens a full-screen terminal UI
with streaming, scrollback, reverse history search, and bracketed paste.
When a managed screen can't run (`TERM=dumb`, redirected terminal endpoints)
polly falls back to a line frontend: Markdown with ANSI styling on a TTY,
the raw response when output is redirected or `NO_COLOR` is set.

### Sessions

Launching without `-c` starts a persistent session under a generated name
like `quiet-otter`. Resume it with `polly -L` (last active) or
`polly -c quiet-otter`, or name it permanently with `/rename`. Generated
sessions expire after 7 days of inactivity; named ones never expire. A
session where no turn ran is discarded on exit. The status row shows the
active model and context use (`ctx 41.2k/156k`; a leading `~` marks a local
estimate).

### Keys and input

| Key | Action |
|---|---|
| `Ctrl-C` / `Esc` | Interrupt the in-flight turn; `Ctrl-C` again (or at an idle prompt) quits |
| `Ctrl-R` | Reverse history search |
| `Ctrl-O` | Show or hide the reasoning disclosure |
| `Ctrl-V` | Attach an image from the system clipboard |
| `Ctrl-Z` | Suspend to the shell; `fg` resumes |

Input submitted mid-turn is queued and marked `(queued)`; failed or
canceled input returns to the composer as a draft. Polly enables mouse
reporting for scrolling and image clicks, so use the terminal's override
(usually Shift-drag) to select text.

### Slash commands

```
/help [command]              Show help
/attach <image-path>         Attach a local image to the next prompt
/clear                       Clear the display (history kept)
/context  (/stats)           Show transcript size and model budget
/get <key|all>               Inspect current settings
/set <key> <value>           Change a setting for this session
                             (model, temp, maxtokens, maxcontext, thinking, tooltimeout)
/model                       Pick a provider and model
/keys                        Set masked, process-local provider keys
/resume                      Pick and resume a saved session
/tools [list [ns]|show <n>]  List or inspect loaded tools
/skills                      List discovered Agent Skills
/rename <name>               Rename the current context
/reset confirm               Clear durable conversation history
/exit  (/quit)               Leave the TUI
```

Keys entered with `/keys` last until polly exits and are never written to
the transcript, history, database, or environment.

### Tool calls, reasoning, and interrupted turns

Tool activity appears once per turn as a collapsed `▸ N tool calls`
disclosure; click it to watch timers, outcomes, and tool-produced images
update in place. Details show call labels and outcomes, never raw result
bodies — the model still receives every result, and durable history keeps
the full exchange. With `--thinking`, reasoning appears as a collapsed
`Thinking…` disclosure; click it or press `Ctrl-O` for a live tail. Both
auto-collapse when the turn ends and can be reopened later, even after a
reload.

A turn that fails or is canceled keeps everything it completed: each
finished model iteration and tool result is written to durable history, so
a retry continues from real state. Only text streamed by the interrupted
final call is lost. Such turns are labeled `failed · completed work saved`,
`canceled · …`, or `not saved` when nothing durable was produced.

### Images

**Seeing them.** The TUI renders thumbnails when assistant Markdown
contains `![alt](./path.png)`, or when a tool result emits a local image
path on a line by itself. Relative paths resolve from polly's working
directory; remote images and paths inside prose, JSON, or code blocks are
not opened. Kitty graphics are used on Kitty, Ghostty, and WezTerm; Sixel on
Windows Terminal 1.22+ and foot; everything else (tmux and Zellij
included) gets a caption/path fallback. Override with
`POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none`.

**Sending them.** Typing a path to a local image attaches it on submit —
`describe .assets/polly.png` just works. `Ctrl-V` grabs the clipboard image,
drag-and-drop attaches a file, and `/attach <path>` handles paths with
spaces. Each attachment is a `[image #N]` token at the cursor; delete it to
drop the attachment. Bytes are captured on submit, so queued turns are
unaffected by later changes to the file.

**Limits.** At most 16 images per prompt and 100 across a request; 10 MB
per image, 16 MiB per request. Images are downscaled to 1568px on the long
edge. PNG, JPEG, and WebP pass through; GIFs (first frame) and BMPs are
normalized to PNG. This is the portable intersection of the
[OpenAI](https://developers.openai.com/api/docs/guides/images-vision),
[Anthropic](https://platform.claude.com/docs/en/build-with-claude/vision),
and [Gemini](https://ai.google.dev/gemini-api/docs/image-understanding)
image inputs.

## Contexts

A context is a named, persistent conversation. One-shot runs are stateless
unless you name one with `-c`.

```bash
polly --create project --model openai/gpt-5.4 --maxtokens 4096   # create with settings
polly --show project                        # show its configuration
polly -c project -p "What database should I use?"    # continue the conversation
polly -c project                            # or continue in the TUI
polly --last -p "Explain the query"         # -L / --last reuses the most recent context
cat notes.txt | polly -c project --add      # add stdin to the context, no API call
polly --reset project                       # clear history, keep settings
polly --list                                # list all contexts
polly --delete project                      # delete one
polly --purge                               # delete all (asks first)
```

Settings used with a context — model, temperature, system prompt, tools —
are saved to it and restored next run; flags win over stored settings and
the change sticks. Changing the system prompt of a context with history
resets the conversation. The system prompt holds only your persona;
terminal rendering guidance is added per request and never stored.

Everything lives in one SQLite database at `~/.pollytool/polly.db` (old
JSON files under `~/.pollytool/contexts` are ignored). To back it up, quit
polly and copy the file, or use SQLite's
[online backup API](https://www.sqlite.org/backup.html) while it runs — a
plain copy can miss data still in the write-ahead log.

## Models and Providers

Models are named `provider/model`.

| Provider | Example model | API key |
|---|---|---|
| OpenAI | `openai/gpt-5.4` | `POLLYTOOL_OPENAIKEY` |
| Anthropic | `anthropic/claude-sonnet-4-6` | `POLLYTOOL_ANTHROPICKEY` |
| Gemini | `gemini/gemini-3.1-pro-preview` | `POLLYTOOL_GEMINIKEY` |
| DeepSeek | `deepseek/deepseek-v4-pro` | `POLLYTOOL_DEEPSEEKKEY` |
| OpenRouter | `openrouter/anthropic/claude-sonnet-4-5` | `POLLYTOOL_OPENROUTERKEY` |
| Ollama | `ollama/gpt-oss` | `POLLYTOOL_OLLAMAKEY` (optional) |
| Hugging Face | `huggingface/...` | `POLLYTOOL_HUGGINGFACEKEY` |

`--baseurl` points at a remote Ollama or any OpenAI-compatible endpoint:

```bash
polly --baseurl http://192.168.1.100:11434 -m ollama/gpt-oss -p "Hello"
polly --baseurl https://api.openrouter.ai/api/v1 -m openai/whatevermodel -p "Hello"
```

Native OpenAI uses the Responses API (Chat Completions with `--baseurl`);
strict tool schemas with optional parameters are downgraded to non-strict
there. DeepSeek reasoning models require their `reasoning_content` echoed
back on follow-up turns, which polly does automatically. OpenRouter takes
the upstream slug after the prefix (`openrouter/openai/gpt-5`). Ollama
schema support depends on the model.

## Tools

Load tools with `-t`/`--tool` (repeatable). Polly auto-detects a **shell
script** (`*.sh`, one tool), an **MCP server config** (`*.json`, one or more
tools), or a **built-in name** (`bash`, `read_file`, ...). Names are
namespaced — `uppercase__to_uppercase`, `filesystem__read_file` — and
`--confirm` asks before each call. New contexts start with `bash` and all
built-in file tools; passing any `--tool` replaces that default.

### Built-in tools

- `bash` — sandboxed shell execution.
- `read_file` — paged numbered lines, with search and raw byte windows.
- `write_file` — create or replace a file, creating parent directories.
- `edit_file` — replace an exact literal string that must be unique (or
  pass `replace_all`).
- `list_dir` — one directory, non-recursive.
- `search_files` — `path:line: text` matches, literal or RE2 with `regex`,
  filtered by an `include` glob; skips `.git`, symlinks, binaries, and
  read-denied paths.

All of them enforce the sandbox policy in-process
([details](SANDBOX.md#what-gets-sandboxed)).

### Shell tools

Any executable is a tool if it answers `--schema` (print a JSON Schema) and
`--execute <json-args>` (do the work, print the result to stdout, exit 0):

```bash
#!/bin/bash
# uppercase.sh
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

```bash
chmod +x uppercase.sh
polly -t ./uppercase.sh -p "Convert 'hello world' to uppercase"
```

The schema may include a top-level `"sandbox"` field to customize the
tool's policy — see [Sandboxing](#sandboxing).

### MCP servers

Servers are declared in Claude Desktop-format JSON. Load every server in a
file with `-t mcp.json`, or one with `-t mcp.json#filesystem`.

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/workspace"]
    },
    "remote-api": {
      "transport": "sse",
      "url": "https://api.example.com/mcp",
      "headers": { "Authorization": "Bearer ..." },
      "timeout": "60s"
    }
  }
}
```

Stdio servers run sandboxed like any other tool and may carry their own
`"sandbox"` overrides. Remote servers (`sse` or streamable HTTP) run
elsewhere.

## Agent Skills

Polly discovers [Agent Skills](https://agentskills.io/specification) — a
folder per skill with a `SKILL.md` manifest — from one or more directories.

```bash
polly --listskills                          # default: ~/.pollytool/skills
polly --skilldir ~/.pollytool/skills --skilldir ./skills --listskills
polly -S ./my-skill -p "..."                # load one skill directly (dir, git URL,
                                            # or archive URL); auto-activated
polly --noskills -p "summarize this file"   # disable skills for a run
```

Discovered skills are advertised in the system prompt next to the
`activate_skill` and `read_skill_file` tools. Activation loads the skill's
`scripts/` as shell tools and its `mcp/` configs as MCP servers, namespaced
by skill name, and enforces its `allowed-tools` globs on later turns
(additively across activations).

## Structured Output

Pass a JSON schema and get validated JSON back. Works with images too
(`polly -f receipt.jpg --schema receipt.schema.json`).

```bash
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

echo "John Doe is 30 years old, email: john@example.com" | \
  polly --schema person.schema.json
# {"name": "John Doe", "age": 30, "email": "john@example.com"}
```

## Sandboxing

Tool commands — the builtin `bash` tool, shell tools, and stdio MCP servers
— run **sandboxed by default**: the filesystem is read-only outside the
policy's writable paths, credential paths (`~/.ssh`, `~/.aws`, `~/.gnupg`,
...) are hidden, and credential-shaped environment variables (`POLLYTOOL_*`,
`AWS_*`, `*_API_KEY`, `*_TOKEN`, `SSH_AUTH_SOCK`, ...) are stripped.

`--sandbox <preset>` (`POLLYTOOL_SANDBOX`) selects the base policy.
Components join with `+`:

| Preset | Meaning |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | no writes at all, not even temp; no network |
| `workspace` | working directory writable; Git metadata read-only |
| `git` | with `workspace`: keep `.git` writable, pin only its dangerous leaves (config, hooks, routing pointers) so commit/rebase/fetch work |
| `net` | outbound network allowed |
| `ssh` | agent-based SSH: `SSH_AUTH_SOCK` and its socket pass through, `~/.ssh/config` and `known_hosts` readable; private keys stay masked |
| `sshkeys` | read all of `~/.ssh` including private keys; still not writable |

The default is **`workspace+net+git`**. Tighten with `--sandbox base` or
`--sandbox readonly` when tools only need to compute or inspect. On top of
any preset: `--writepath <dir>` and `--denypath <path>` (both repeatable),
`--allownet`, and `--nosandbox` to disable sandboxing entirely. Individual
tools can widen or tighten their own policy with a `"sandbox"` field in the
shell tool schema or MCP server entry.

**[SANDBOX.md](SANDBOX.md)** has every `"sandbox"` field, the merge rules,
the Git metadata protection, platform details, and limitations.

## CLI Reference

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
   --timeout duration                                       Stream stall timeout: cancel a request after this long with no provider data (0 disables) (default: 30m0s) [$POLLYTOOL_TIMEOUT]
   --deadline duration                                      Hard per-request ceiling: cancel a request after this total time even if data is still arriving (0 = no ceiling) (default: 2h0m0s) [$POLLYTOOL_DEADLINE]
   --thinking string                                        Reasoning effort: off, dynamic, a level (minimal, low, medium, high, xhigh, max), or a token budget (e.g. 12000) (default: "off") [$POLLYTOOL_THINKING]
   --baseurl string                                         Base URL for API (for OpenAI-compatible endpoints or Ollama) [$POLLYTOOL_BASEURL]
   --skilldir string [ --skilldir string ]                  Skill directory or directory containing skill folders (can be specified multiple times) [$POLLYTOOL_SKILLDIR]
   --skill string, -S string [ --skill string, -S string ]  Skill to load: local directory, git repo URL, or archive URL. Auto-activated on start.
   --noskills                                               Disable Agent Skill discovery and runtime skill tools
   --listskills                                             List discovered Agent Skills
   --tool string, -t string [ --tool string, -t string ]    Tool provider: shell script (provides 1 tool) or MCP server (can provide multiple tools). Can be specified multiple times
   --tooltimeout duration                                   Timeout for tool execution (default: 5m0s) [$POLLYTOOL_TOOLTIMEOUT]
   --prompt string, -p string                               Initial prompt (reads from stdin if not provided; starts REPL when neither is provided)
   --system string, -s string                               System prompt (persona; a per-frontend display contract is added automatically) [$POLLYTOOL_SYSTEM]
   --file string, -f string [ --file string, -f string ]    File, image, or URL to include (can be specified multiple times)
   --schema string                                          Path to JSON schema file for structured output
   --context string, -c string                              Context name for conversation continuity [$POLLYTOOL_CONTEXT]
   --last, -L                                               Use the last active context
   --maxcontext int                                         Maximum estimated tokens sent to the model, clamped to the model's advertised context window when discoverable; full history is retained (0 = unlimited, never clamped) (default: 256000)
   --confirm                                                Require confirmation before each tool call (default: false)
   --sandbox string                                         Sandbox preset: base, readonly, workspace, git, net, ssh, sshkeys — join with + (e.g. workspace+net+git+ssh); git requires workspace (default: "workspace+net+git") [$POLLYTOOL_SANDBOX]
   --nosandbox                                              Disable sandboxing of tool commands [$POLLYTOOL_NOSANDBOX]
   --denypath string [ --denypath string ]                  Additional path blocked from sandboxed reads (repeatable, supports ~) [$POLLYTOOL_DENYPATHS]
   --writepath string [ --writepath string ]                Additional path sandboxed tools may write to (repeatable, supports ~) [$POLLYTOOL_WRITEPATHS]
   --allownet                                               Allow sandboxed tools outbound network access [$POLLYTOOL_ALLOWNET]
   --quiet                                                  Suppress status and tool display output
   --debug, -d                                              Enable debug logging
   --meta                                                   Emit a machine-readable run-outcome trailer (polly-meta key=value lines) to stderr
   --help, -h                                               show help
   --reset string                                           Reset the specified context (clear conversation history, keep settings)
   --purge                                                  Delete all sessions (requires confirmation)
   --create string                                          Create a new context with specified name and configuration
   --show string                                            Show configuration for the specified context
   --list                                                   List all available context IDs
   --delete string                                          Delete the specified context
   --add                                                    Add stdin content to context without making an API call
```

## See Also

- [Soulshack](https://github.com/pkdindustries/soulshack) — an IRC chatbot
  that uses Polly for LLM features.

## License

MIT
