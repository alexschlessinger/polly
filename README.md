# Pollytool (polly)

My [LLM](https://en.wikipedia.org/wiki/Stochastic_parrot) harness.
There are many like it. This one is mine.

This file: CLI and TUI. [API.md](API.md): Go library. [SANDBOX.md](SANDBOX.md):
sandbox. [WORKFLOWS.md](WORKFLOWS.md): swarms and workflows.
[SEARCH.md](SEARCH.md): semantic search.

![polly TUI](.assets/interactive.png)

## Install

```bash
go build -o polly ./cmd/polly/
```

## Quick start

```bash
export POLLYTOOL_ANTHROPICKEY=...
polly                                   # TUI
echo "Hello?" | polly                   # one-shot, stdin
polly -m openai/gpt-5.4 -p "Hello?"     # one-shot, flag
polly -f image.jpg -f https://example.com/chart.png -p "Tie these together"
polly -p "uppercase this" -t ./uppercase.sh -t filesystem.json   # shell tool, MCP
```

Default model `anthropic/claude-sonnet-4-6`; `-m provider/model` or `POLLYTOOL_MODEL`.

## One-shot output

Stdout: one settled answer, raw Markdown when redirected. Stderr: live status,
plain lines under `TERM=dumb` or redirect. `--stream` emits text as it arrives.
`--quiet`: warnings and errors only. `--activity-details`: bounded
Thought/Tools/Agents/Images groups before the trailer. `--meta`: `polly-meta`
record. Token or iteration cap: trailer reads `incomplete`.

## TUI

No `-p`, no piped stdin: full-screen TUI. `TERM=dumb` or redirected: line
frontend. `--theme=halo` (`POLLYTOOL_THEME`): framed inspector with scrollbar,
draggable split, running glint, amber border on pending approval.

### Sessions

Launch without `-c`: generated handle (`quiet-otter`), expires after 7 idle
days. Named sessions never expire; empty ones are discarded on exit. Resume:
`polly -L`, `polly -c quiet-otter`, `/resume`. Polly titles sessions once their
purpose is clear; the handle stays. `/title <text>` (or **F2** in the picker)
protects a title; `/rename <name>` changes the handle. Status row:
`ctx 41.2k/156k`, `~` = local estimate.

### Tabs

| Command | Effect |
|---|---|
| `/resume` | Pick a saved session or agent; or click the status-row name |
| `/new`, `/close` | Fresh tab / close visible tab (session kept; refused while running) |
| `/tab [n\|name]` | List or switch |
| `/parent` | Tool or thought to conversation; agent to caller |
| `Alt+1`..`9`, `Alt+]`, `Alt+[` | Jump, next, previous |

Settings are per tab. Hidden tabs keep running, queue input, post one notice
on completion. Pending approval: **Ctrl-G**, select, **Review**. Ctrl-C
interrupts the root turn; a second at quit cancels the rest. Sessions are
leased: another polly's show `in use`; an agent whose parent is leased
elsewhere opens with a read-only parent snapshot.

### Subagents

`spawn_agent` (model) and `/spawn [--read-only] <brief>` (you) share one
runtime. Parent plus direct children form a swarm; no grandchildren. Editing
children get isolated Git worktrees (Git 2.40+), never commit; the parent
previews a three-way merge and applies. Children inherit `--maxiterations`.
Give repository-relative paths in briefs; children run Git inspection in their
assigned worktrees. `source` selects snapshot input, not their working directory.
Limits: 32 concurrent, 256 executions per run (`--swarm-concurrent`,
`--swarm-executions`). Quitting pauses; `/swarm resume ID [N]` continues with
N extra model calls; `/swarm grant N` adds execution starts. `/swarm`
inspects members, tasks, messages, publications, reports, integrations.
`/workflow SCRIPT.js INPUT.json` runs a JavaScript workflow. Everything else:
[WORKFLOWS.md](WORKFLOWS.md).

Inspector: click an expanded agent, tool, or thought row, or `/inspect
[tools|thoughts|prev|next|back|forward|find|maximize|wider|narrower]`.
**Stop** cancels the inspected agent; **Review** answers its approval. Split
70/30 at 120+ columns, drag to resize; narrow terminals stack. Keys act on the
pane under the pointer. Inspection never takes leases.

### Keys

| Key | Action |
|---|---|
| `Ctrl-C` | Interrupt root turn; again, or idle: quit |
| `Esc` | Dismiss dialog/search, close inspector, interrupt, in that order |
| `Left`/`Right` | Prev/next tool or thought over inspector; else cursor |
| `Up`/`Down`, `PgUp`/`PgDn`, `Home`/`End` | Scroll hovered pane; else edit or history |
| `Ctrl-R` / `Ctrl-G` / `Ctrl-O` | History search / agents dialog / reasoning toggle |
| `Ctrl-V` / `Ctrl-Z` | Attach clipboard image / suspend |

Mid-turn input queues; failed input returns as a draft. Select text with Shift-drag.

### Slash commands

```
/help [cmd]  /attach <path>  /clear  /context  /get <key|all>  /model  /keys
/set <key> <val>   (model, temp, maxtokens, maxcontext, thinking, tooltimeout)
/sessions  /new  /tab  /close  /parent  /inspect  /spawn  /swarm  /workflow
/tools [list|show <n>]  /skills  /title <text>  /rename <name>  /reset confirm  /exit
```

`/keys` are process-local, never stored.

### Transcript

Tool calls: one collapsed `▸ N tool calls` row per turn; click for details,
click a detail for the inspector. `--thinking`: collapsed `▸ thought` row with
live timer, `Ctrl-O` for the tail. Both reopen after reload. Interrupted turns
keep every completed iteration and tool result.

### Images

**In:** Markdown `![](./path.png)` or a bare local path in a tool result.
Kitty graphics (Kitty, Ghostty, WezTerm), Sixel (Windows Terminal 1.22+, foot),
else caption. `POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none`.
**Out:** local path in the prompt, `Ctrl-V`, drag-and-drop, `/attach`; each an
`[image #N]` token. **Limits:** 16 per prompt, 100 per request, 10 MB each,
16 MiB total, 1568px long edge. GIF (first frame) and BMP become PNG.

## Contexts

Named persistent conversations. One-shot runs are stateless without `-c`.

```bash
polly --create project --model openai/gpt-5.6    # create with settings
polly -c project -p "What database?"             # continue; -L = last active
cat notes.txt | polly -c project --add           # add stdin, no API call
polly --show|--reset|--delete project            # config, clear history, remove
polly --list [--flat]  /  polly --purge          # list; delete all (asks)
```

Settings stick to the context; flags override and persist. A new system prompt
resets history. Without `--system`, Polly adds a coding policy and loads
`AGENTS.md` from the Git root down to the working directory (32 KiB per file,
64 KiB total). `--schema` omits all additions. Storage: `~/.pollytool/polly.db`;
back up with SQLite's [online backup API](https://www.sqlite.org/backup.html).

## Models

| Provider | Example | Key |
|---|---|---|
| OpenAI | `openai/gpt-5.4` | `POLLYTOOL_OPENAIKEY` |
| Anthropic | `anthropic/claude-sonnet-4-6` | `POLLYTOOL_ANTHROPICKEY` |
| Gemini | `gemini/gemini-3.1-pro-preview` | `POLLYTOOL_GEMINIKEY` |
| DeepSeek | `deepseek/deepseek-v4-pro` | `POLLYTOOL_DEEPSEEKKEY` |
| OpenRouter | `openrouter/openai/gpt-5` | `POLLYTOOL_OPENROUTERKEY` |
| Ollama | `ollama/gpt-oss` | `POLLYTOOL_OLLAMAKEY` (optional) |
| Hugging Face | `huggingface/...` | `POLLYTOOL_HUGGINGFACEKEY` |

`--baseurl`: remote Ollama or any OpenAI-compatible endpoint.

## Tools

`-t`/`--tool`, repeatable. `*.sh`: shell tool. `*.json`: MCP config
(`#server` for one). Bare name: built-in. Names namespace as `server__tool`.
`--confirm` asks before each call.

### Built-in tools

Default set: `bash`, `read_file`, `write_file`, `edit_file`, `list_dir`,
`spawn_agent`, `set_session_title`, recall tools `list_artifacts`,
`read_artifact`, `read_transcript`, and `zvec_grep_search` when `zg` is on
`PATH` ([SEARCH.md](SEARCH.md)). Any `--tool` replaces the set.

### Shell tools

Any executable answering `--schema` (JSON Schema on stdout) and
`--execute <json-args>` (result on stdout, exit 0). A top-level `"sandbox"`
schema field customizes its policy.

### MCP servers

Claude Desktop-format JSON. Stdio servers run sandboxed; `sse` and streamable
HTTP servers (`"transport"`, `"url"`, `"headers"`, `"timeout"`) run elsewhere.

## Skills

[Agent Skills](https://agentskills.io/specification), one `SKILL.md` per folder.
`--skilldir` (default `~/.pollytool/skills`), `--listskills`, `-S <dir|git|url>`
loads and activates one, `--noskills` disables. Activation loads `scripts/` as
shell tools and `mcp/` as servers.

## Structured output

`--schema person.schema.json`: validated JSON on stdout. Works with `-f image.jpg`.

## Sandboxing

Tool commands run sandboxed by default. `--sandbox <preset+preset>` (`POLLYTOOL_SANDBOX`):

| Preset | Meaning |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | no writes, no network |
| `workspace` | working directory writable; Git metadata read-only |
| `git` | with `workspace`: `.git` writable except config, hooks, routing pointers |
| `net` | outbound network |
| `ssh` | `SSH_AUTH_SOCK` passes; `~/.ssh/config` and `known_hosts` readable |
| `sshkeys` | all of `~/.ssh` readable |

Default **`workspace+net+git`**. Also `--writepath`, `--denypath`, `--allownet`,
`--nosandbox`. Details: [SANDBOX.md](SANDBOX.md).

## CLI reference

`polly --help`. Most flags have a `POLLYTOOL_*` environment variable.

## See also

[Soulshack](https://github.com/pkdindustries/soulshack): IRC chatbot built on Polly.

## License

MIT
