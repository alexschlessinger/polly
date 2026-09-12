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

Stdout: the settled answer, raw Markdown when redirected; a turn that swarm
settlement reopened prints each answer block in order. Stderr: live status,
plain lines under `TERM=dumb` or redirect. `--stream` emits text as it arrives.
`--quiet`: warnings and errors only. `--activity-details`: bounded
Thought/Tools/Agents/Images groups before the trailer. `--meta`: `polly-meta`
record. Token or iteration cap: trailer reads `incomplete`.

Settled message stats show the prompt cache hit percentage when the provider
reports cache usage for every measured request in the turn. The rate is cached
input tokens divided by total input tokens across those requests; resumed turns
retain it. Clicking the status context meter shows message counts and estimated
tokens by role, including generated system guidance, plus the cache hit rate
across the entire session. These estimates cover the full session before context
trimming and exclude tool definition overhead.

## TUI

No `-p`, no piped stdin: full-screen TUI. `TERM=dumb` or redirected: line
frontend. The framed inspector (scrollbar, draggable split, running glint,
amber border on pending approval) is always on; there is no theme flag or
environment variable for it.

Markdown tables in the interactive TUI fit the current pane: cells wrap within
columns, and narrow panes show labeled fields for each row. Tables update while
streaming and reflow when resizing or opening a conversation inspector.

### Sessions

Launch without `-c`: generated handle (`quiet-otter`), expires after 7 idle
days. Named sessions never expire; empty ones are discarded on exit. Resume:
`polly -L`, `polly -c quiet-otter`, `/resume`. Polly titles sessions once their
purpose is clear; the handle stays. `/title <text>` (or **F2** in the picker)
protects a title; `/rename <name>` changes the handle. Status row:
`41.2k/156k`, `~` = local estimate. Click the context readout for message
counts by role; Esc or a click outside closes the popover.

### Tabs

| Command | Effect |
|---|---|
| `/resume` | Pick a saved session or agent; or click the status-row name |
| `/new`, `/close` | Fresh tab / close visible tab (session kept; refused while running) |
| `Alt+1`..`9`, `Alt+]`, `Alt+[` | Jump, next, previous |

Parent navigation is a click on the divider link, or the inspector's parent
action. Settings are per tab. Hidden tabs keep running, queue input, post one
notice on completion. In the **Ctrl-G** picker, agents that need a decision or run
precede expandable **History** groups, and a workspace row counts them
(`1 needs decision · 2 running`); **Ctrl-G** and the status-row badge open the
first approval, else the first decision (its member, or the `/swarm` section
that lists it). Finished workflow attempts show
their outcomes and deferred-item counts. Research normally completes on durable
delivery; settled workspaces are released automatically while conversations and
original snapshots remain available for follow-ups. Pending approval: **Ctrl-G**, select, **Review**. Ctrl-C
interrupts the root turn; a second at quit cancels the rest. Sessions are
leased: another polly's show `in use`; an agent whose parent is leased
elsewhere opens with a read-only parent snapshot.

### Subagents

Ask the model to delegate, or use `/spawn [--read-only] [--review] <brief>`.
New agents require a short purpose label, which becomes their session title
before they run. `/spawn` derives it from your brief; model spawns and workflows
must supply it. Continuations preserve the existing label and title.
Only the root agent receives `set_session_title`; manual F2 and `/title` editing
remain available for all sessions.
The parent and its direct children share a swarm. Ordinary read-only research
finishes when its result is durably delivered; add `--review` when explicit
acceptance is part of the assignment. Editing children work in isolated Git
snapshots and make no commits. The parent finishes their exact task revisions
with one `swarm_integrate` call. Conflicts retain a candidate with a repair action;
unchanged work completes without an apply.

`/swarm` leads with **needs decision**, **working**, and **done**. The model gets
the same coordination view from `swarm_status` and the return of `swarm_wait`.
Use the listed next action; while work progresses, the parent waits for events.
A background workflow delivers one terminal report rather than each internal
agent's progress. `/workflow SCRIPT.js INPUT.json` runs a JavaScript workflow over
this same runtime. See [WORKFLOWS.md](WORKFLOWS.md) for the start/wait/finish patterns.

Safe settled workspaces are released automatically. The member's identity,
conversation, and source evidence remain available: a `swarm_followup` wakes that
same member with a new task and restores its workspace if needed. Research keeps
its starting snapshot; editing continues from its submitted result. An explicit
known snapshot refreshes the new task. Retained workspaces stay visible with a
cleanup reason. `/swarm cleanup all` removes safe inactive copies; `/swarm forget`
also drops snapshot refs once integration obligations are resolved.

Give repository-relative paths in briefs. Git 2.40+ is required; `source` selects
snapshot input, not a child's working directory. Children inherit `--maxiterations`.
Defaults are 32 concurrent executions and 256 starts per run (`--swarm-concurrent`,
`--swarm-executions`). `/swarm resume ID [N]` resumes with N additional model calls;
`/swarm grant N` extends the separate start budget. Quitting pauses unfinished work.

Agent rows and pickers derive `idle`, `active`, `waiting`, or `paused` plus details
such as `idle · awaiting review` and `paused · iteration limit (3/5)`. The parent's
live state appears first. Inspect members, tasks, messages, publications, workflows,
integrations, previews, or raw records through `/swarm`; peer mail stays out of the
user conversation view. The [state model](docs/swarm-state-model.md) explains how
these views derive from the saved evidence.

Inspector: click an expanded agent, tool, or thought row, or `/inspect
[tools|thoughts|find|maximize]` (prev/next, back/forward, wider/narrower are
inspector buttons, not command arguments). **Stop** cancels the inspected
agent; **Review** answers its approval. Split 70/30 at 120+ columns, drag to
resize; below 120 columns the inspector takes the full width, replacing the
split. Keys act on the focused pane; **Tab** focuses the inspector from an
empty composer (**Esc** returns). Inspection never takes leases.

### Keys

| Key | Action |
|---|---|
| `Ctrl-C` | Interrupt root turn; again, or idle: quit |
| `Esc` | Dismiss completion/dialog/search, close inspector, interrupt, in that order |
| `Tab` / `Enter` | Accept an open completion; Enter again sends |
| `Left`/`Right` | Prev/next tool or thought over inspector; else cursor |
| `Up`/`Down`, `PgUp`/`PgDn`, `Home`/`End` | Scroll focused inspector; else edit or history |
| `Ctrl-R` / `Ctrl-G` / `Ctrl-O` | History search / sessions picker / reasoning toggle |
| `Ctrl-V` / `Ctrl-Z` | Attach clipboard image / suspend |

Mid-turn input queues; failed input returns as a draft. Select text with Shift-drag.

### Slash commands

```
/help [cmd]  /attach <path>  /clear  /context  /model  /keys
/set [key [value]]   (model, temp, maxtokens, maxcontext, thinking, tooltimeout)
/sessions  /new  /close  /inspect  /spawn  /swarm  /workflow
/tools [list [namespace]|show <name>]  /title <text>  /rename <name>
/reset confirm  /exit
```

`/keys` are process-local, never stored.

### Files and skills in the composer

In the managed TUI, type `@` to search workspace files, or `/` for skills
(and commands at the beginning of input). Arrow keys select a result,
Tab/Enter inserts it, and Escape dismisses the popup. Fully typed references
also work without selecting a result:

```text
use /polly-tui to inspect @cmd/polly/repl_composer.go
compare @"notes/design draft.md" with @README.md
```

`/skill name` selects a skill whose name collides with a built-in command.
A referenced skill activates before that turn runs, supplies its instructions,
and stays active in the session. Queueing a prompt does not activate its skills
in the running turn. A skill-only prompt starts a turn too.

Dropping files or pasting only existing file paths attaches them as `@path`
references, including mixed text/images and quoted or escaped paths. Prose and
code remain literal. Unsupported batches remain pasted text with an error.
Ordinary typed paths stay text. Backticks, fenced code, and `\@` or `\/`
escapes let you write references literally. Drops without terminal paste markers
remain ordinary input.

Sending snapshots file contents before queueing; failures preserve the draft.
Text attachments are UTF-8, at most 256 KiB each and 1 MiB combined, with 32
files per prompt and the existing image limits. Directories, PDFs, and other
binary files are unsupported. Completion reads `.gitignore` files directly, including nested rules and
negations, and excludes `.git` itself; explicit paths may include ignored or external
files allowed by the session's read policy. No reference grants extra access.
Click a submitted text attachment's prompt to inspect its saved contents.
Restored drafts retain those contents even if the source changes or disappears;
remove and reattach a reference to read it again. CLI and fallback REPL prompts
keep their existing literal behavior.

### Transcript

Tool calls: a collapsed `▸ N tools` row per batch; click for details,
click a detail for the inspector. Expanded tool calls fit one line at the current
pane width, with short labels such as `read`, `edit`, and `list`. File paths are
relative to the conversation's known workspace; long paths shorten from the
middle, keeping filenames and read ranges visible. `$` introduces Bash commands,
and `…` marks folded setup or omitted text. Status and timing take priority over
output counts. Tool state and elapsed time sit at the right edge of the
inspector's title row. The Bash inspector folds recognizable leading `cd` and
`export` steps into a collapsed `setup` row; click it to reveal the full setup. Expansion
is remembered when resizing or revisiting that call. Short pipelines stay on one
line when they fit; longer commands and output wrap at word or path boundaries
with indented continuations. Ambiguous shell setup stays visible. Stored calls and
output remain unchanged. `--thinking`: collapsed
`▸ thought` row with live timer, `Ctrl-O` for the tail. Both reopen after reload. Interrupted turns
keep every completed iteration and tool result. Agents: a collapsed `▸ N agents`
row per batch; expanded, a workflow lists only members that are busy, paused or
need a decision; its heading counts the decisions it owes and the finished ones
(`Workflow · judges · 2 need decision · ▸ 30 done`); click the count to list them.

### Images

**In:** Markdown `![](./path.png)` or a bare local path in a tool result.
Kitty graphics (Kitty, Ghostty, WezTerm), Sixel (Windows Terminal 1.22+, foot),
else caption. `POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none`.
**Out:** `Ctrl-V` and `/attach` leave an `[image #N]` token. File drops and
`@path` references use the composer attachment flow above. A bare typed path stays text, so the model calls `view_image` itself.
**Limits:** 16 per prompt, 100 per request, 10 MB each,
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

`--baseurl` selects the inference and metadata endpoint, including remote Ollama,
OpenAI-compatible servers, Anthropic, and Gemini.

Click the model name in the status bar or use `/model` to open the form with a
provider selector, a model name,
and a key override masked with `*`. `/keys` opens the same form focused on the key.
**Up / Down** moves between fields; **Left / Right** cycles the single visible
provider backward or forward. The displayed arrows also accept clicks.
Advertised pricing appears at the lower left as `$input/$output/$cached`, per
million tokens. Pinned hosts use their own prices; Automatic uses model catalog
prices when available. A dash marks an unknown price; wholly unknown pricing stays hidden.
In the model field, dim text previews the next completion without changing the
draft. **Tab** fills the first matching completion; subsequent presses
cycle through matches for the original query. **Shift-Tab** cycles backward.
Typing starts a new completion cycle. Tab never moves focus between fields.
**Enter** advances to the next field or activates **Apply**, at the bottom right.
Apply saves the model and route and installs the key override together;
**Esc** discards the draft. Providers, fields, and Apply also accept clicks.

Autocomplete includes only models with advertised text and tool support.
Unknown capabilities produce no suggestions; manual model names always work,
including when discovery fails. Completions are inserted only on Tab. Ollama details
are fetched for matching typed prefixes; routed model details provide host choices.
**Ctrl-R** refreshes discovery. Failed discovery is silent; manual entry stays usable.

For OpenRouter and Hugging Face, enter `model:host` in the model field to pin a
host, for example `org/model:upstream`. Host suggestions use the same convention
and include only advertised live routes with text and tool support. A bare name
uses Automatic routing. OpenRouter catalog IDs containing a colon are preserved;
append another `:host` to pin those variants. Ollama `model:tag` names stay intact.
OpenRouter also accepts `--modelhost <routing-id>` or `/set modelhost <routing-id>`;
`/set modelhost automatic` clears the pin. Sessions retain pins; children inherit
them unless the child specifies another model or route.

Key overrides apply to that provider across all tabs in the running process and
are never saved to disk, environment variables, or conversation history. An
unchanged key field preserves the current credential. Editing the field stages a
replacement; **Ctrl-U** clears it so Apply restores the environment key. Discovery
can use the draft key without installing it; Escape leaves the active key alone.

Catalogs load only when their provider is opened, and model details when needed
for completion or a request. Polly caches discoveries for one hour in a separate table in the session
SQLite database; memory-mode stores stay in memory. Cached results appear first,
stale results refresh in the background, and failed refreshes keep the last good
result. Cache entries are scoped to provider, endpoint, credential, model, and host;
credentials are never stored in this cache. Failures suppress automatic refreshes
for one minute; Ctrl-R requests an immediate refresh.

The model selector’s editable Context field shows the resolved limit: an explicit
setting, otherwise the detected size, otherwise the fallback. Editing it saves an
explicit limit on Apply; enter `auto` to restore automatic sizing or `0` for unlimited.
New sessions default to that size, with an output reserve; when detection is unavailable,
the fallback is 256,000 tokens. Explicit `--maxcontext` or `/set maxcontext N` values
take precedence over detected limits, even when larger.
`--maxcontext 0` opts out; `/set maxcontext auto` restores automatic sizing.
Saved numeric limits remain in effect when resuming older sessions. Automatic routing uses a conservative
limit only when every eligible advertised host supplies one. Ollama's model capacity
and configured runtime context are separate constraints. Legacy per-session context
windows remain readable but no longer control clamping.

When metadata explicitly rules out images, Polly sends explanatory text references
and retains the originals. Unsupported optional tools, temperature, and reasoning
settings are omitted for that request, with notices; saved settings are preserved.
Completed tool exchanges become associated text when tool calling is unsupported.
An incompatible explicit response schema or required response tool fails clearly.
Missing metadata leaves existing behavior unchanged. This does not add image
batching, automatic retries, or provider-specific numeric image-limit enforcement.

## Tools

`-t`/`--tool`, repeatable. `*.sh`: shell tool. `*.json`: MCP config
(`#server` for one). Bare name: built-in. Names namespace as `server__tool`.
`--confirm` asks before each call.

### Built-in tools

Default set: `bash`, `read_file`, `write_file`, `edit_file`, `list_dir`,
`spawn_agent`, `set_session_title`, `view_image`, recall tools
`list_artifacts`, `read_artifact`, `read_transcript`, and `zvec_grep_search`
when `zg` is on `PATH` ([SEARCH.md](SEARCH.md)). Any `--tool` replaces the set.

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
loads and activates one, `--noskills` disables. Activation loads `mcp/` JSON
as servers and lists `scripts/` as paths to run via the bash tool.

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
