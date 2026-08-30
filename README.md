# Pollytool (polly)

<img src=".assets/polly.png" width="128" height="128">

This is my [LLM](https://en.wikipedia.org/wiki/Stochastic_parrot) CLI tool.
There are many like it, but this one is mine.

- **Many models** — OpenAI, Anthropic, Gemini, DeepSeek, OpenRouter, Ollama,
  Hugging Face. One interface.
- **Interactive TUI** — a full-screen terminal UI with streaming, scrollback,
  history search, and images.
- **Multimodal** — text, pics, random files.
- **Contexts** — memory, but opt-in. Conversations persist in SQLite.
- **Tool calling** — bolt on shell scripts and MCP servers.
- **Sandboxed by default** — tool commands run in an OS-level sandbox that
  masks your credentials.
- **Agent Skills** — discover `SKILL.md` bundles and activate them on demand.
- **Structured output** — JSON on purpose, not by accident.

**The docs:** this file covers the CLI and the TUI. [API.md](API.md) is the
tour of the Go library. [SANDBOX.md](SANDBOX.md) is the sandbox reference.

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

# One-shot: pipe a prompt in, or pass it with -p
echo "Hello?" | polly
polly -p "Hello?"

# Pick a model
echo "Quantum computing in one breath" | polly -m openai/gpt-5.4

# Attach files, images, URLs
polly -f image.jpg -p "What's this?"
polly -f https://example.com/image.png -p "Describe it"
polly -f notes.txt -f https://example.com/chart.png -p "Tie these together"

# Tools: shell scripts and MCP servers, auto-detected by file type
polly -p "uppercase this: hello" --tool ./uppercase.sh
polly -p "create news.txt with today's news" --tool perp.json --tool filesystem.json

# Keep a conversation going
polly -c project -p "I'm working on a Python web app"
polly -c project -p "What database should I use?"

# Agent Skills
polly --skilldir ~/.pollytool/skills --listskills
polly --skilldir ~/.pollytool/skills -p "review this patch for regressions"
```

The default model is `anthropic/claude-sonnet-4-6`; override it with `-m
provider/model` or `POLLYTOOL_MODEL`.

## The Interactive TUI

Running `polly` with no `--prompt` and no piped stdin opens a full-screen
terminal UI (built on tcell/gotui) with streaming responses, scrollback,
reverse history search, and bracketed paste. When the managed screen cannot
run (`TERM=dumb` or redirected terminal endpoints), polly uses its line
frontend instead. On a capable stdout TTY, line output buffers each assistant
segment and renders Markdown with ANSI styling. Redirected output and
`NO_COLOR` stream the model's original response unchanged; Polly adds no
terminal escape or graphics sequences.

### Sessions

Launching the TUI without `-c` starts a persistent session under a generated
name like `quiet-otter`. Resume it later with `polly -L` (last active) or
`polly -c quiet-otter`, or give it a permanent name with `/rename`. A session
where no turn ever ran is discarded on exit. Generated sessions expire after
7 days of inactivity; explicitly named and renamed contexts never expire
automatically.

### Keys and input

| Key | Action |
|---|---|
| `Ctrl-C` / `Esc` | Interrupt the in-flight turn; `Ctrl-C` again (or at an idle prompt) quits |
| `Ctrl-R` | Reverse history search |
| `Ctrl-O` | Show or hide the reasoning disclosure |
| `Ctrl-V` | Attach an image from the system clipboard |
| `Ctrl-Z` | Suspend to the shell; `fg` resumes the same TUI state |

Input submitted while a turn is running appears immediately in the transcript
with a `(queued)` marker that disappears when its turn starts. Failed or
canceled input returns to the composer as an editable draft; pending entries
are marked `(not sent)` and stay available through input history.

Polly enables button-level mouse reporting for transcript scrolling and image
clicks, so use the terminal's mouse override — usually Shift-drag, or
Option-drag in some macOS terminals — to select text.

### Slash commands

```
/help [command]              Show help
/attach <image-path>         Attach a local image to the next prompt
/clear                       Clear the display (history kept)
/context  (/stats)           Show durable transcript size and model budget
/get <key|all>               Inspect current settings
/set <key> <value>           Change a setting for this session
                             (model, temp, maxtokens, maxcontext, thinking, tooltimeout)
/tools [list [ns]|show <n>]  List or inspect loaded tools
/skills                      List discovered Agent Skills
/rename <name>               Rename the current context
/reset confirm               Clear durable conversation history
/exit  (/quit)               Leave the TUI
```

### Tool calls and reasoning

Tool activity appears once per turn as a collapsed `▸ N tool calls`
disclosure, from the first call onward. Click it to inspect every row; while
open, running timers, outcomes, and tool-produced images update in place.
Explicitly opened activity stays open across later calls, then auto-collapses
when the turn ends — including failed or canceled turns — and completed
disclosures can be reopened later, even after a session reload. Hydrated
details contain only safe call labels and outcomes, never raw result bodies;
the model still receives every result, and durable history retains the full
exchange.

With `--thinking` enabled, reasoning appears once per turn as a quiet
`Thinking…` disclosure, collapsed at the start of every turn. Click the label
or press `Ctrl-O` for a bounded three-row live tail (at least two full rows
when the terminal has room); the oldest text scrolls off the top as new
reasoning arrives. The disclosure stays open across tool calls when
explicitly opened, then collapses when the turn finishes. Completed
disclosures remain expandable and summarize all reasoning segments from the
turn. Reasoning from successful turns survives a session reload; when a turn
fails or is canceled before generating anything durable, its reasoning is
marked unsaved and lasts only for the current process.

### Interrupted turns

A turn that fails or is canceled partway keeps everything it completed:
each finished model iteration and every executed tool result is written to
durable history, so a retry continues from real state instead of redoing
side-effectful work. Tool calls cut off mid-batch are recorded as
interrupted stubs. The settled turn is labeled `failed · completed work
saved` (or `canceled · …`); only text streamed by the interrupted final
call is lost, and a turn that failed before generating anything is still
labeled `not saved`. Reloaded sessions show these turns with a
`turn interrupted · completed work retained` marker.

### Images

**Seeing them.** The TUI renders static thumbnails when assistant Markdown
contains `![alt](./path.png)`; tool results may use the same form or emit a
local image path on a line by itself. Rich line output also renders local
images deliberately embedded in assistant Markdown, but does not open images
directly from tool results. Relative paths resolve from polly's working
directory. Remote images, paths buried in prose or JSON, and paths inside code
blocks are not opened. Thumbnails preserve the source aspect ratio inside a
maximum 50-column by 10-row box, accounting for rectangular terminal cells.

Kitty graphics are used on Kitty, Ghostty, and WezTerm; Sixel on Windows
Terminal 1.22+ and foot; other terminals get a compact caption/path fallback.
tmux and Zellij currently always use the fallback — native placement there
needs explicit multiplexer passthrough support. Override auto-detection with
`POLLYTOOL_IMAGE_PROTOCOL=kitty`, `sixel`, or `none`. On image-capable
terminals the startup splash draws the polly logo as a native image (embedded
in the binary); elsewhere, and on short terminals, it keeps the half-block
ANSI bird. The same protocol selection applies to rich line output, but
redirected stdout and `NO_COLOR` always suppress native graphics.

**Sending them.** Typing a path to an existing local image attaches it on
submit — `describe .assets/polly.png` just works. `Ctrl-V` grabs an image off
the system clipboard (macOS built-in `osascript`, or `pngpaste` if installed;
Linux `wl-paste`/`xclip`; Windows PowerShell). Drag-and-dropping an image
file onto the terminal attaches it (a paste consisting only of image paths is
treated as a drop), and `/attach <path>` does the same explicitly — use it
for paths containing spaces.

Each attachment appears as a literal `[image #N]` token at the cursor —
delete the token to drop the attachment, or reorder and reuse it freely. When
input is accepted, polly prepares the exact image payload; queued turns keep
those bytes even if the source file changes or disappears. Sessions persist
prepared images, and if the last image turn is incomplete after a reload the
draft returns to the composer — resubmitting it unchanged reuses those exact
bytes. (Attached text bodies and context imports are not restored; they
cannot be reconstructed safely from prompt text.) Clipboard captures and
prepared previews are stored under the user cache directory
(`pollytool/attachments`) and swept after two weeks; durable data lives in
the SQLite session database, not this preview cache.

**Limits and formats.**

- At most 16 unique images per composer prompt, 16 image parts per
  model-visible message, and 100 images across all retained turns in a
  request.
- Each base64-encoded image is capped at 10 MB, and image data in the
  model-visible history plus the candidate prompt at 16 MiB total — headroom
  under the documented Gemini and Anthropic inline limits.
- Images are downscaled to at most 1568px on the long edge before upload.
  PNG, JPEG, and WebP pass through when already within limits; animated GIFs
  are reduced to their first frame and normalized to PNG, as are BMPs. JPEG
  orientation metadata is applied whenever an image is resized or re-encoded.
- Older persisted raster parts are normalized in the request without
  rewriting session history; legacy SVG parts become a short omission marker
  instead of blocking the context.
- These formats are the portable intersection documented by
  [OpenAI image inputs](https://developers.openai.com/api/docs/guides/images-vision),
  [Anthropic vision](https://platform.claude.com/docs/en/build-with-claude/vision),
  and [Gemini image understanding](https://ai.google.dev/gemini-api/docs/image-understanding).

## Contexts

A context is a named, persistent conversation. One-shot runs are stateless
unless you name one with `-c`.

```bash
polly --create project --model openai/gpt-5.4 --maxtokens 4096   # create with settings
polly --show project                        # show its configuration
echo "I'm working on a Python web app" | polly -c project
polly -c project -p "What database should I use?"    # continues the conversation
polly -c project                            # or continue interactively in the TUI
polly --last -p "Explain the query"         # -L / --last reuses the most recent context
cat notes.txt | polly -c project --add      # add stdin to the context, no API call
polly --reset project                       # clear history, keep settings
polly --list                                # list all contexts
polly --delete project                      # delete one
polly --purge                               # delete all (asks first)
```

### Settings follow the context

- Settings used with a context — model, temperature, system prompt, tools —
  are saved to it and restored on the next run.
- Command-line flags always win over stored settings, and the change is saved
  for future runs: if a context uses `openai/gpt-5.4` and you run
  `-m openai/gpt-5.4-mini`, the context switches to GPT-5.4-mini.
- Changing the system prompt of a context with existing history resets the
  conversation to keep things consistent.
- The system prompt holds only your persona. Markdown-capability and terminal
  rendering guidance is a per-frontend display contract composed into each
  request and never stored, so a context moves freely between one-shot use and
  the REPL. Markdown is available rather than mandatory; explicit format
  requests still win.
- Tools are part of the deal: load `-t ./build.sh` in a context once and it's
  restored on every later use of that context.

### Storage and backups

Session history, settings, and durable artifact bytes live in one SQLite
database at `~/.pollytool/polly.db`. This is a clean break from the old
per-context JSON format: files under `~/.pollytool/contexts` are left
untouched, not imported, and ignored — the SQLite catalog starts empty on the
first run after the cutover.

For a simple backup, quit every polly process and copy
`~/.pollytool/polly.db`. While polly is running, use SQLite's
[online backup API](https://www.sqlite.org/backup.html) or
[`VACUUM INTO`](https://www.sqlite.org/lang_vacuum.html#vacuuminto) instead —
copying the file alone can miss committed data still in the write-ahead log.

## Models and Providers

Models are named `provider/model`. The default is
`anthropic/claude-sonnet-4-6`.

| Provider | Example model | API key |
|---|---|---|
| OpenAI | `openai/gpt-5.4` | `POLLYTOOL_OPENAIKEY` |
| Anthropic | `anthropic/claude-sonnet-4-6` | `POLLYTOOL_ANTHROPICKEY` |
| Gemini | `gemini/gemini-3.1-pro-preview` | `POLLYTOOL_GEMINIKEY` |
| DeepSeek | `deepseek/deepseek-v4-pro` | `POLLYTOOL_DEEPSEEKKEY` |
| OpenRouter | `openrouter/anthropic/claude-sonnet-4-5` | `POLLYTOOL_OPENROUTERKEY` |
| Ollama | `ollama/gpt-oss` | `POLLYTOOL_OLLAMAKEY` (optional) |
| Hugging Face | `huggingface/...` | `POLLYTOOL_HUGGINGFACEKEY` |

### Custom endpoints

```bash
# A remote Ollama
polly --baseurl http://192.168.1.100:11434 -m ollama/gpt-oss -p "Hello"

# Any OpenAI-compatible endpoint
polly --baseurl https://api.openrouter.ai/api/v1 -m openai/whatevermodel -p "Hello"
```

### Provider notes

**OpenAI** — GPT-5.4 and its distills (5.4-mini, 5.4-nano). Native OpenAI
uses the Responses API; setting `--baseurl` keeps OpenAI-compatible endpoints
on Chat Completions. Reliable schema support (structured output uses
`additionalProperties: false`); strict tool schemas with optional parameters
are downgraded to non-strict on native Responses. Built-in Responses tools
are not exposed yet.

**Anthropic** — the Claude family (Opus, Sonnet, Haiku). Structured output
via the tool-use pattern; mostly reliable schema support. Excellent for
long-form content and analysis.

**Gemini** — Pro and Flash models. Good balance of speed and capability;
reliable schema output via ResponseSchema.

**DeepSeek** — hosted models such as `deepseek/deepseek-v4-pro` and
`deepseek/deepseek-v4-flash`. Reasoning models emit a non-standard
`reasoning_content` field the API requires echoed back on follow-up turns;
polly captures and replays it automatically, so tool use works without
configuration.

**OpenRouter** — routes to many upstream providers through one
OpenAI-compatible endpoint. Use the upstream `provider/model` slug after the
prefix: `openrouter/anthropic/claude-sonnet-4-5`, `openrouter/openai/gpt-5`,
`openrouter/deepseek/deepseek-chat`.

**Ollama** — requires an Ollama installation; any model available in Ollama
works. Use `--baseurl` for remote instances. Schema support is hit and miss,
depending on the model.

## Tools

Load tools with `-t`/`--tool` (repeatable). Polly auto-detects what you give
it:

- a **shell script** (`*.sh`) — one tool per script, speaking a two-flag
  protocol
- an **MCP server config** (`*.json`) — one or more tools per server
- a **built-in name** (`bash`, `read_file`, ...) — native tools compiled in

Tool names are namespaced to avoid conflicts: `scriptname__toolname` for
shell tools (`uppercase__to_uppercase`), `servername__toolname` for MCP tools
(`filesystem__read_file`). Add `--confirm` to approve each tool call by hand.

New contexts start with `bash` and all of the built-in file tools enabled.
Existing contexts keep their persisted tool set, including an empty one.
Passing one or more `--tool` values replaces the defaults for that context.

### Built-in tools

```bash
# Sandboxed shell execution
polly -t bash -p "list the largest files in this directory"

# Direct file access: paged reads, whole-file writes, exact string edits
polly -t read_file -t write_file -t edit_file -p "fix the typo in README.md"

# Discovery: directory listings and cross-file search, no shell required
polly -t list_dir -t search_files -t read_file -p "where is retry handled?"
```

- `bash` — sandboxed shell execution.
- `read_file` — pages a text file as numbered lines, with literal search and
  raw byte windows.
- `write_file` — creates or replaces a file, creating missing parent
  directories.
- `edit_file` — replaces an exact literal string that must be unique in the
  file (or pass `replace_all`).
- `list_dir` — lists one directory (non-recursive).
- `search_files` — reports matching lines as `path:line: text` — literal by
  default, RE2 with `regex`, filtered with an `include` glob — skipping
  `.git`, symlinks, binaries, and read-denied paths, with output bounded so
  one minified file can't flood the context.

All of them enforce the sandbox policy in-process: reads follow the read
policy, and writes are confined to the policy's writable paths minus its
write-denied islands (protected Git metadata included). `write_file` and
`edit_file` refuse to load without sandboxing unless the registry explicitly
opts out; the read-only tools load anywhere, so a session with no `bash` at
all can still browse, search, and read.

### Shell tools

Any executable can be a tool if it answers two flags: `--schema` prints a
JSON Schema describing the tool, and `--execute <json-args>` does the work
and prints the result to stdout.

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

Shell tools run sandboxed by default, and the schema may include a top-level
`"sandbox"` field to customize the tool's policy — see
[Sandboxing](#sandboxing) below. [API.md](API.md#shell-tools) has the full
protocol contract.

### MCP servers

MCP servers are declared in Claude Desktop-format JSON. One file can define
several servers; load them all with `-t mcp.json`, or a single one with
`-t mcp.json#filesystem`.

```json
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
```

Remote servers connect over SSE or streamable HTTP:

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

Local (stdio) servers run sandboxed like any other tool, and a server entry
may carry its own `"sandbox"` overrides ([Sandboxing](#sandboxing)).

## Agent Skills

Polly discovers [Agent Skills](https://agentskills.io/specification) from one
or more directories. Each skill lives in a folder named after the skill and
contains a `SKILL.md` manifest with YAML frontmatter.

```bash
polly --listskills                          # default directory: ~/.pollytool/skills
polly --skilldir ~/.pollytool/skills --skilldir ./skills --listskills
polly --skilldir ~/.pollytool/skills -p "help me review this Go change"
polly -S ./my-skill -p "..."                # load one skill directly (local dir,
                                            # git repo URL, or archive URL); auto-activated
polly --noskills -p "summarize this file"   # disable skills for a run
```

At runtime polly:

- advertises discovered skills in the system prompt and exposes the
  `activate_skill` and `read_skill_file` native tools
- on activation, loads executables under the skill's `scripts/` directory as
  normal shell tools, and Claude Desktop-style MCP configs under its optional
  `mcp/` directory — both namespaced by skill name
- enforces the skill's `allowed-tools` on future turns, matching polly tool
  names with `*` glob support; skill-bundled tools remain auto-approved

`allowed-tools` is additive for the duration of the run: activating another
skill can widen access, but it never revokes tools an earlier activation
already allowed.

## Structured Output

Pass a JSON schema and get validated JSON back:

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
```

```json
{
  "name": "John Doe",
  "age": 30,
  "email": "john@example.com"
}
```

Works with images too: `polly -f receipt.jpg --schema receipt.schema.json`.

## Sandboxing

Tool commands — the builtin `bash` tool, shell tools, and stdio MCP servers —
run **sandboxed by default**: the filesystem is read-only outside the
policy's writable paths, credential paths (`~/.ssh`, `~/.aws`, `~/.gnupg`,
...) are blocked from reads, and credential-shaped environment variables
(`POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, `SSH_AUTH_SOCK`, ...) are
stripped. On Linux, tools additionally get private `/tmp` and `/run`, their
own PID and IPC namespaces, and no access to host Unix sockets. Sandboxed
tools get a `[sandboxed]` suffix on their LLM-facing description so the model
knows they're restricted — with a `.git is read-only` note when Git metadata
is fully pinned, so a failing `git commit` isn't mistaken for a transient
error.

`--sandbox <preset>` (env `POLLYTOOL_SANDBOX`) selects the base policy for
all sandboxed tools. Components join with `+`:

| Preset | Meaning |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | no writes at all, not even temp; no network |
| `workspace` | the working directory is writable; Git metadata stays read-only so a tool cannot replace `.git` or plant hooks |
| `git` | with `workspace`: keep `.git` writable and pin only its dangerous leaves (config, hooks, routing pointers), so `git commit`/rebase/fetch work |
| `net` | outbound network allowed |
| `ssh` | agent-based SSH: `SSH_AUTH_SOCK` and its socket pass through, `~/.ssh/config` and `known_hosts` readable; private keys stay masked |
| `sshkeys` | read all of `~/.ssh` including private keys (agentless setups); still not writable |

The default is **`workspace+net+git`**: tools can edit the project, reach the
network, and use Git, while credentials stay masked and everything outside
the workspace is read-only. Tighten with `--sandbox base` or `--sandbox
readonly` when tools only need to compute or inspect.

Fine-tune on top of any preset:

- `--writepath <dir>` (repeatable, `POLLYTOOL_WRITEPATHS`) — extra writable
  paths
- `--denypath <path>` (repeatable, `POLLYTOOL_DENYPATHS`) — extra
  read-blocked paths
- `--allownet` (`POLLYTOOL_ALLOWNET`) — allow network
- `--nosandbox` (`POLLYTOOL_NOSANDBOX`) — disable sandboxing entirely

Leaving a home directory or filesystem root broadly writable earns a visible
warning naming the settings to inspect.

Individual tools can customize their own policy with a `"sandbox"` field in
the shell tool schema or MCP server entry — grant network, extra writable
paths, specific env vars, and so on:

```jsonc
// in a shell tool schema or MCP server entry
"sandbox": true                                                    // base policy
"sandbox": { "allowNetwork": true, "writablePaths": ["/tmp/data"] }
"sandbox": { "readPaths": ["~/.aws"], "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"] }
"sandbox": { "passEnv": ["GITHUB_TOKEN"] }
"sandbox": { "denyWrite": true }
```

Per-tool policy merges monotonically on top of the preset: it can add grants
or restrictions but never remove one, and `"sandbox": false` is refused
unless the caller also makes the explicit global unsafe choice with
`--nosandbox`.

Sandboxing requires the fixed system `/usr/bin/bwrap` on Linux, or
`/usr/bin/sandbox-exec` plus `/usr/bin/perl` on macOS. If the required
trusted backend is unavailable, polly refuses to run sandboxed tools rather
than silently running them unsandboxed. And when no-sandbox mode is in
effect, explicitly supplied sandbox flags (`--sandbox`, `--denypath`,
`--writepath`, `--allownet`) are an error rather than a silently ignored
policy; `--nosandbox=false` overrides an ambient `POLLYTOOL_NOSANDBOX=true`.

**[SANDBOX.md](SANDBOX.md) is the full reference** — every `"sandbox"` field,
how the workspace protects Git metadata (and which exotic repository layouts
it refuses), platform implementation details, and honest limitations.

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
   --system string, -s string                               System prompt (persona; a per-frontend display contract is added automatically) [$POLLYTOOL_SYSTEM]
   --file string, -f string [ --file string, -f string ]    File, image, or URL to include (can be specified multiple times)
   --schema string                                          Path to JSON schema file for structured output
   --context string, -c string                              Context name for conversation continuity [$POLLYTOOL_CONTEXT]
   --last, -L                                               Use the last active context
   --reset string                                           Reset the specified context (clear conversation history, keep settings)
   --list                                                   List all available context IDs
   --delete string                                          Delete the specified context
   --add                                                    Add stdin content to context without making an API call
   --purge                                                  Delete all sessions (requires confirmation)
   --create string                                          Create a new context with specified name and configuration
   --show string                                            Show configuration for the specified context
   --maxcontext int                                         Maximum estimated tokens sent to the model, clamped to the model's advertised context window when discoverable; full history is retained (0 = unlimited, never clamped) (default: 256000)
   --confirm                                                Require confirmation before each tool call
   --sandbox string                                         Sandbox preset: base, readonly, workspace, git, net, ssh, sshkeys — join with + (e.g. workspace+net+git+ssh); git requires workspace (default: "workspace+net+git") [$POLLYTOOL_SANDBOX]
   --nosandbox                                              Disable sandboxing of tool commands [$POLLYTOOL_NOSANDBOX]
   --denypath string [ --denypath string ]                  Additional path blocked from sandboxed reads (repeatable, supports ~) [$POLLYTOOL_DENYPATHS]
   --writepath string [ --writepath string ]                Additional path sandboxed tools may write to (repeatable, supports ~) [$POLLYTOOL_WRITEPATHS]
   --allownet                                               Allow sandboxed tools outbound network access [$POLLYTOOL_ALLOWNET]
   --quiet                                                  Suppress status and tool display output
   --debug, -d                                              Enable debug logging
   --help, -h                                               show help
```

## See Also

- [Soulshack](https://github.com/pkdindustries/soulshack) — an IRC chatbot
  that uses Polly for LLM features.

## License

MIT
