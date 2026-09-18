# Pollytool (polly)

My [LLM](https://en.wikipedia.org/wiki/Stochastic_parrot) harness.
There are many like it. This one is mine.

This file: CLI and TUI. [API.md](docs/API.md): Go library. [SANDBOX.md](docs/SANDBOX.md):
sandbox. [WORKFLOWS.md](docs/WORKFLOWS.md): swarms and workflows.

![polly TUI](.assets/interactive.png)

## Install

```bash
go build -o polly ./cmd/polly/
```

## Quick start

```bash
polly                                   # TUI; the first run opens the setup form
echo "Hello?" | polly                   # one-shot, stdin
polly -m openai/gpt-5.4 -p "Hello?"     # one-shot, flag
polly -f image.jpg -f https://example.com/chart.png -p "Tie these together"
polly -p "uppercase this" -t ./uppercase.sh -t filesystem.json   # shell tool, MCP
```

Default model `anthropic/claude-sonnet-4-6`; `-m provider/model` or `POLLYTOOL_MODEL`.

### First run

With no `~/.pollytool/config`, a TUI launch opens the setup form before the
first prompt, prefilled from whatever flags and environment gave it: provider, model, key, context limit,
endpoint, and thinking effort. Apply saves them to `~/.pollytool/config`
and uses them at once; Escape skips and records the skip so the
form does not reopen. `polly --setup` and `/setup` reopen it any time.

Keys are never written: they come from `POLLYTOOL_<PROVIDER>KEY` in the
environment, and polly does not start without one for the provider of the
model the session runs on (Ollama and custom `--baseurl` endpoints need
none). Apply warns when a shell variable will override a saved value. A key typed
into the setup form lasts for that process, like `/keys`.

The file holds one `POLLYTOOL_*` variable per line, the same names the flags
read from the environment, and any of them but keys may be set there by hand:

```text
POLLYTOOL_MODEL=openai/gpt-5.4
POLLYTOOL_THINKING=high
```

Flags override the environment, which overrides the file. Environment and
file values seed new contexts; only a flag changes a resumed one.

## One-shot output

Stdout carries the settled answer, as raw Markdown when redirected. A turn that
swarm settlement reopened prints each answer block in order. Stderr carries live
status, as plain lines under `TERM=dumb` or when redirected.

| Flag | Effect |
|---|---|
| `--stream` | emit text as it arrives |
| `--quiet` | warnings and errors only |
| `--activity-details` | bounded Thought/Tools/Agents/Images groups before the trailer |
| `--meta` | a `polly-meta` record |

When a token or iteration cap stops the turn, the trailer reads `incomplete`.

Settled message stats show the prompt cache hit percentage when the provider
reports cache usage for every measured request in the turn. The rate is cached
input tokens divided by total input tokens across those requests; resumed turns
keep it.

Clicking the status context meter shows message counts and estimated tokens by
role, including generated system guidance, plus the cache hit rate across the
whole session. These estimates cover the full session before context trimming
and exclude tool definition overhead.

## TUI

Without `-p` or piped stdin, polly runs the full-screen TUI. Under `TERM=dumb`
or a redirect it runs the line frontend instead.

The framed inspector (scrollbar, draggable split, running glint, amber border
on pending approval) is always on; there is no theme flag or environment
variable for it.

Markdown tables fit the current pane: cells wrap within columns, and narrow
panes show labeled fields for each row. Tables update while streaming and
reflow when you resize or open a conversation inspector.

### Sessions

Launch without `-c` and you get a generated handle (`quiet-otter`) that expires
after 7 idle days. Named sessions never expire; empty ones are discarded on
exit. Resume with `polly -L`, `polly -c quiet-otter`, or `/resume`.

Polly titles a session once its purpose is clear; the handle stays.
`/title <text>` (or `F2` in the picker) protects a title. `/rename <name>`
changes the handle.

The status row shows context use as `41.2k/156k`; `~` marks a local estimate.
Click the readout for message counts by role.

Once a session's tools have changed files, the status row also shows their
cumulative diff (`+100 −20`). Click it to open the **Changes** inspector, which
lists every changed file's diff.

Esc or a click outside closes any dialog, popover, or the inspector, and that
click does nothing else. Links beside the inspector, and the Agents and Changes
status fields, retarget it instead.

### Tabs

| Command | Effect |
|---|---|
| `/resume` | Pick a saved root session; or click the status-row name |
| `/new`, `/close` | Fresh tab / close visible tab (session kept; refused while running) |
| `Alt+1`..`9`, `Alt+]`, `Alt+[` | Jump, next, previous |

- Settings are per tab.
- Hidden tabs keep running, queue input, and post one notice on completion.
- To reach a parent, click the divider link or use the inspector's parent action.
- The `Ctrl-G` picker lists root sessions with title, age, and message count,
  with the current session highlighted.
- Sessions are leased. Another polly's sessions show a muted `×` and cannot be
  opened in the picker. An agent whose parent is leased elsewhere opens with a
  read-only parent snapshot.
- Ctrl-C interrupts the root turn; a second Ctrl-C at quit cancels the rest.

#### Agents status

Once a session has agents, the status row counts them:
`Agents · 1 needs approval · 2 running · 3 finished`. Finished means the latest
run ended, however it ended.

Clicking it opens the **Agents** inspector: names and short statuses, with
completed agents under a collapsed **History**. Click an agent to inspect its
conversation; **‹** returns to the list with its scroll position preserved.

To answer a pending approval: click the agent badge, select the agent, then
**Review**.

Research normally completes on durable delivery. Settled workspaces are
released automatically, while conversations and original snapshots stay
available for follow-ups.

### Subagents

Ask the model to delegate, or use `/spawn [--read-only] [--review] <brief>`.

**Labels.** New agents need a short purpose label, which becomes their session
title before they run. `/spawn` derives it from your brief; model spawns and
workflows must supply it. Continuations keep the existing label and title.
Only the root agent gets `set_session_title`; manual `F2` and `/title` editing
work for every session.

**Read-only and editing children.** The parent and its direct children share a
swarm. Ordinary read-only research finishes when its result is durably
delivered; add `--review` when explicit acceptance is part of the assignment.
Editing children work in isolated Git snapshots and make no commits. The parent
integrates their exact task revisions with one `swarm_integrate` call.
Conflicts keep a candidate with a repair action; unchanged work completes
without an apply.

**Coordination tools.** `/swarm` and its subcommands are temporarily disabled
and omitted from help and completion; the Agents inspector still works. The
model reads coordination state from `swarm_read`, and `wait_agent` waits for
updates. The default surface has 14 parent tools and six child tools; typed
children add `swarm_complete`. [WORKFLOWS.md](docs/WORKFLOWS.md) lists them. Use
the listed next action; results reach the parent automatically as work
progresses, so it can continue its own work and park only when nothing else
remains.

Managed `spawn_agent` requires explicit `read_only:true` for research or
`read_only:false` for editing. Worker listings and task summaries are compact;
use `list_agents({details:true})` or task `section:"details"` for provenance.

**Commits.** Model tools and JavaScript identify captured code by full Git
commit IDs. `spawn_agent`, `polly.agent`, `polly.context`, and `polly.followup`
accept commits already in the source repository as well as retained captures;
explicit selection keeps the exact SHA and history. Use `commit`, detailed task
`baseCommit`/`resultCommit`, and `candidate.merged.commit`; integration
candidate IDs are separate. Omitting `commit` captures current files, including
eligible uncommitted and untracked files.

**Workflows.** Foreground `workflow_run` delivers its result once, with failure
details and next actions. A background workflow delivers one terminal notice
instead; an unsaved foreground result keeps its notice for recovery.
Integration candidates always expose `receipt` (null before a recorded apply
attempt). `/workflow SCRIPT.js INPUT.json` runs a JavaScript workflow over the
same runtime. See [WORKFLOWS.md](docs/WORKFLOWS.md) for the start/wait/finish
patterns.

**Follow-ups.** Safe settled workspaces are released automatically, but the
member's identity, conversation, and source evidence stay available. A
`followup_task` wakes that same member with a new task and restores its
workspace if needed. By default, research keeps its baseline commit and editing
continues from its submitted result. Use
`followup_task({target, message, refresh:true})` on an idle worker whose
assignment is done to select current parent code. Its compact result identifies
the task, execution, and baseline. Non-Git research keeps its live source.
Messages only change information; a new worker provides independent review.
The advanced `polly.followup` keeps its optional `commit` selection. Retained
workspaces stay visible with a cleanup reason. `/swarm cleanup all` removes safe
inactive copies; `/swarm forget` also drops snapshot refs once integration
obligations are resolved.

**Limits and requirements.**

- Give repository-relative paths in briefs.
- Git 2.40+ is required. `source` selects snapshot input, not a child's
  working directory.
- Children inherit `--maxiterations`.
- Defaults are 32 concurrent executions and 256 starts per run
  (`--swarm-concurrent`, `--swarm-executions`).
- `/swarm resume ID [N]` resumes with N additional model calls; `/swarm grant N`
  extends the separate start budget.
- Quitting pauses unfinished work.

**Agent rows.** Rows and pickers derive `idle`, `active`, `waiting`, or `paused`
plus details such as `idle · awaiting review` and
`paused · iteration limit (3/5)`. The parent's live state appears first.
Inspect members, tasks, messages, publications, workflows, integrations,
previews, or raw records through `/swarm`; peer mail stays out of the user
conversation view. Every view derives from the saved records.

### Inspector

Open it by clicking an expanded agent, tool, or thought row, or with
`/inspect [tools|thoughts|changes|find|maximize]`. **Stop** cancels the
inspected agent; **Review** answers its approval.

At 120+ columns the inspector splits 70/30 and you can drag to resize. Below
120 columns it takes the full width, replacing the split.

Keys act on the focused pane, and focus follows the pointer: over the inspector
frame the inspector has the keys, over the conversation the composer does.
`Tab` focuses the inspector from an empty composer and `Esc` returns, until the
pointer moves again. Inspection never takes leases.

### Keys

| Key | Action |
|---|---|
| `Ctrl-C` | Interrupt root turn; again, or idle: quit |
| `Esc` | Dismiss completion/dialog/search, close inspector, interrupt, in that order |
| `Tab` / `Enter` | Tab accepts an open completion; Enter sends |
| `Left`/`Right` | Prev/next thought in its inspector; no sideways tool navigation; else cursor |
| `Up`/`Down`, `PgUp`/`PgDn`, `Home`/`End` | Scroll focused inspector; else edit or history |
| `Ctrl-R` / `Ctrl-G` / `Ctrl-O` | History search / sessions picker / expand or collapse every inline block |
| `Ctrl-V` / `Ctrl-Z` | Attach clipboard image / suspend |

Mid-turn input queues; failed input returns as a draft. Select text with Shift-drag.

### Slash commands

```
/help [cmd]  /attach <path>  /clear  /context  /model  /keys  /setup
/set [key [value]]   (model, temp, maxtokens, maxcontext, thinking, tooltimeout)
/sessions  /new  /close  /inspect  /spawn  /workflow
/tools [list [namespace]|show <name>]  /title <text>  /rename <name>
/reset confirm  /exit
```

`/keys` are process-local, never stored; `/setup` saves the other fields to
`~/.pollytool/config`.

### Files and skills in the composer

Type `@` to search workspace files, or begin input with `/` for skills and
commands. Arrow keys select a result, Tab or Enter inserts it, and Escape
dismisses the popup (Enter sends the draft once the popup is closed). Fully
typed references also work without selecting a result:

```text
/polly-tui inspect @cmd/polly/repl_composer.go
compare @"notes/design draft.md" with @README.md
```

**References.** Unresolved bare `@word` references stay literal text. Quoted
references and explicit paths such as `@./file` report missing-file errors.
Ordinary typed paths stay text. Backticks, fenced code, and `\@` or `\/`
escapes let you write references literally.

**Skills.** `/name` activates a skill only at the beginning of input;
`/skill name` selects a skill anywhere, including names that collide with a
built-in command. A referenced skill activates before that turn runs, supplies
its instructions, and stays active in the session. Queueing a prompt does not
activate its skills in the running turn. A skill-only prompt starts a turn too.

**Drops and pastes.** Dropping files or pasting only existing file paths
attaches them as `@path` references, including mixed text/images and quoted or
escaped paths. Prose and code stay literal. Unsupported batches stay pasted
text with an error. Drops without terminal paste markers are ordinary input.

**Attachments.** Sending snapshots file contents before queueing; failures keep
the draft. Text attachments are UTF-8, at most 256 KiB each and 1 MiB combined,
with 32 files per prompt and the existing image limits. Directories, PDFs, and
other binary files are unsupported.

Completion reads `.gitignore` files directly, including nested rules and
negations, and excludes `.git` itself. Explicit paths may include ignored or
external files allowed by the session's read policy. No reference grants extra
access.

Click a submitted text attachment's prompt to inspect its saved contents.
Restored drafts keep those contents even if the source changes or disappears;
remove and reattach a reference to read it again. CLI and line-frontend prompts
treat `@` and `/` as literal text.

### Transcript

**Tool rows.** Each batch of tool calls collapses to a `▸ N tools` row. Click
it for details, and click a detail for the inspector. Expanded tool calls fit
one line at the current pane width, with short labels such as `read`, `edit`,
and `list`. File paths are relative to the conversation's known workspace; long
paths shorten from the middle, keeping filenames and read ranges visible. `$`
introduces Bash commands, and `…` marks folded setup or omitted text. Status
and timing take priority over output counts.

**Change sizes.** A call that changed files shows its size (`+3 −1`, `new +12`)
after the label. Expanded, a single changed file shows a bounded hunk under the
row, and a Bash command that touched several files lists them with their
counts. The inspector shows every changed file's full diff above the output.
The status row totals the whole session's changes (`+100 −20`); clicking the
total, or `/inspect changes`, lists every changed file oldest first; click a file's row to open its diff, or
press `Ctrl-O` to open or fold them all. Outside a Git
repository Bash edits are not tracked, and the transcript says so once per
session.

**Tool inspector.** It lists the whole conversation oldest first, using the
same compact previews. Click a preview to reveal its setup, command (arguments
for other tools), diffs, and output together; click it again to collapse. Each
call opens independently, including failed calls. Bash setup contains
recognizable leading `cd` and `export` steps and is omitted when absent.
Expansion survives resizing and reopening; clicking an inline tool scrolls to
that call's collapsed preview. Full output and images load only when the call
is opened. New tools append below; the list follows at the bottom and holds
position when scrolled away. The `‹ Tools` title returns to the conversation.

Short pipelines stay on one line when they fit; longer commands and output wrap
at word or path boundaries with indented continuations. Ambiguous shell setup
stays visible.

**Thoughts.** With `--thinking`, each thought is a collapsed `▸ thought` row
with a live timer. `Ctrl-O` opens every thinking, tool, agent, and image block
in the view, or closes them all; while open, blocks that arrive later open too,
until `Ctrl-O` closes everything again. Both reopen after reload. Interrupted
turns keep every completed iteration and tool result.

**Agent rows.** Each batch collapses to a `▸ N agents` row. Expanded, a workflow
lists only members that are busy, paused, or need a decision. Its heading
counts the decisions it owes and the finished ones
(`Workflow · judges · 2 need decision · ▸ 30 done`); click the count to list
them.

Adjacent thought, tool, agent, and image controls share one row
(`▸ thought 2.1s · ▸ 3 tools · ▸ 2 agents`); each triangle opens its own detail
beneath the row. Open details hang from one muted `│` rail under the first
triangle, with a bare rail row between two open details.

### Images

In: Markdown `![](./path.png)` in an assistant message, or image media a tool
returns (`view_image` shows as `image viewed`). Tool output that merely quotes
a path or Markdown image syntax stays text.
Kitty graphics (Kitty, Ghostty, WezTerm), Sixel (Windows Terminal 1.22+, foot),
else caption. `POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none`.

Out: `Ctrl-V` and `/attach` leave an `[image #N]` token. File drops and
`@path` references use the composer attachment flow above. A bare typed path
stays text, so the model calls `view_image` itself.

Limits: 16 per prompt, 100 per request, 10 MB each, 16 MiB total, 1568px long
edge. GIF (first frame) and BMP become PNG.

## Contexts

Named persistent conversations. One-shot runs are stateless without `-c`.

```bash
polly --create project --model openai/gpt-5.6    # create with settings
polly -c project -p "What database?"             # continue; -L = last active
cat notes.txt | polly -c project --add           # add stdin, no API call
polly --show|--reset|--delete project            # config, clear history, remove
polly --list [--flat]  /  polly --purge          # list; delete all (asks)
```

Settings stick to the context; flags given on the command line override and
persist. `POLLYTOOL_*` environment variables and `~/.pollytool/config` are
defaults for new contexts only and never change a stored one. A new system
prompt resets history.

Without `--system`, Polly adds a coding policy and loads `AGENTS.md` from the
Git root down to the working directory (32 KiB per file, 64 KiB total).
`--schema` omits all additions.

Storage is `~/.pollytool/polly.db`; back up with SQLite's
[online backup API](https://www.sqlite.org/backup.html).

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

`--baseurl` selects the inference and metadata endpoint for OpenAI-compatible
servers and Ollama. Native Anthropic and Gemini requests use their provider
endpoints.

### Model form

Click the model name in the status bar or use `/model` to open the form: a
provider selector, a model name, and a key override masked with `*`. `/keys`
opens the same form focused on the key. `/setup` opens it with two more
fields, endpoint and thinking effort, and Apply also saves every field but
the key to `~/.pollytool/config` as the defaults for later launches (see
First run).

- `Up`/`Down` moves between fields; `Left`/`Right` cycles the single visible
  provider backward or forward. The displayed arrows also accept clicks.
- In the model field, dim text previews the next completion without changing
  the draft. `Tab` fills the first matching completion; later presses cycle
  through matches for the original query, and `Shift-Tab` cycles backward.
  Typing starts a new cycle. Tab never moves focus between fields.
- `Enter` advances to the next field or activates **Apply**, at the bottom
  right. Apply saves the model and route and installs the key override
  together; `Esc` discards the draft. Providers, fields, and Apply also accept
  clicks.
- Advertised pricing appears at the lower left as `$input/$output/$cached`, per
  million tokens. Pinned hosts use their own prices; Automatic uses model
  catalog prices when available. A dash marks an unknown price; wholly unknown
  pricing stays hidden.

### Discovery and completion

Autocomplete includes only models with advertised text and tool support.
Unknown capabilities produce no suggestions; manual model names always work,
including when discovery fails. Completions are inserted only on Tab. Ollama
details are fetched for matching typed prefixes; routed model details provide
host choices. `Ctrl-R` refreshes discovery. Failed discovery is silent, and
manual entry stays usable.

Catalogs load only when their provider is opened, and model details only when
needed for completion or a request. OpenRouter requests also load the model
catalog to combine its reasoning policy with endpoint capabilities.

Polly caches discoveries for one hour in a separate table in the session SQLite
database; memory-mode stores stay in memory. Cached results appear first, stale
results refresh in the background, and failed refreshes keep the last good
result. Cache entries are scoped to provider, endpoint, credential, model, and
host; credentials are never stored in this cache. Failures suppress automatic
refreshes for one minute; Ctrl-R requests an immediate refresh.

### Host pinning

For OpenRouter and Hugging Face, select a discovered `model:host` in the model
field to pin a host, for example `org/model:upstream`. Host suggestions use the
same convention and include only advertised live routes with text and tool
support. A bare name uses Automatic routing.

OpenRouter keeps the selected model and host while the field is unchanged,
including during loading, refreshes, and key edits. For new input, an exact
catalog model ID takes precedence over a discovered `model:host` route; an
unknown colon suffix stays part of the literal model ID. Append another
discovered `:host` to pin a variant. Ollama `model:tag` names stay intact.

OpenRouter also accepts `--modelhost <routing-id>` or
`/set modelhost <routing-id>`; `/set modelhost automatic` clears the pin.
Sessions keep pins; children inherit them unless the child specifies another
model or route.

### Key overrides

Key overrides apply to that provider across all tabs in the running process and
are never saved to disk, environment variables, or conversation history. An
unchanged key field keeps the current credential. Editing the field stages a
replacement; `Ctrl-U` clears it so Apply restores the environment key.
Discovery can use the draft key without installing it; Escape leaves the active
key alone.

### Context limit

The model selector's editable Context field shows the resolved limit: an
explicit setting, otherwise the detected size, otherwise the fallback. Editing
it saves an explicit limit on Apply; enter `auto` to restore automatic sizing
or `0` for unlimited.

New sessions default to that size, with an output reserve. When detection is
unavailable, the fallback is 256,000 tokens. Explicit `--maxcontext` or
`/set maxcontext N` values set the requested budget; positive budgets are
capped to the detected window with output headroom, including saved numeric
limits and inherited child budgets. `--maxcontext 0` opts out;
`/set maxcontext auto` restores automatic sizing.

Saved numeric limits remain in effect when resuming older sessions. Automatic
routing uses a conservative limit only when every eligible advertised host
supplies one. Ollama's model capacity and configured runtime context are
separate constraints. Context windows saved by older sessions are still
readable but do not affect clamping.

### Thinking on OpenRouter

`/set thinking` shows the saved preference and its effective setting. For
example, `off → low (required)` means the model requires thinking and `low` is
its lowest advertised effort; the saved preference remains `off`.

- If the required minimum is unknown, Polly uses the provider default and
  reports that fallback.
- Optional thinking can be explicitly disabled.
- With unknown policy, `off` uses the provider default and labels the effective
  setting unknown.
- `dynamic` always uses the provider default.
- Explicit unsupported efforts are rejected with valid choices when editing the
  setting.
- If a saved preference is unsupported after switching models, the request uses
  the provider default and reports the adaptation while keeping the saved
  preference. Unknown support leaves an explicit effort unchanged.

Completion hints use cached capabilities without waiting for network access.

OpenRouter reasoning survives tool follow-ups and session reloads. Each
response keeps its reasoning details and the gateway and model that produced
them. Polly replays them only to that same gateway and requested model;
changing the upstream host under Automatic routing does not invalidate them.
Reasoning saved without that origin stays visible in transcripts but is not
replayed. Session message metadata also keeps the response ID, returned model,
and serving provider when supplied. Polly does not log raw responses or rewrite
saved transcripts.

### Capability adaptation

When metadata explicitly rules out images, Polly sends explanatory text
references and keeps the originals. Unsupported optional tools and temperature
are omitted for that request, with notices; saved settings are preserved.
Completed tool exchanges become plain text when tool calling is unsupported.
An incompatible explicit response schema or required response tool fails with
an error. Missing metadata leaves other capability behavior alone. Polly does
not batch images, retry automatically, or enforce provider-specific image
limits.

## Tools

`-t`/`--tool`, repeatable. `*.sh`: shell tool. `*.json`: MCP config
(`#server` for one). Bare name: built-in. Names namespace as `server__tool`.
`--confirm` asks before each call.

### Built-in tools

Default set: `bash`, `read_file`, `write_file`, `edit_file`, `list_dir`,
`spawn_agent`, `set_session_title`, `view_image`, and the recall tools
`list_artifacts`, `read_artifact`, and `read_transcript`. Any `--tool` replaces
the set.

**Diffs.** `edit_file`, `write_file`, and `bash` report what they changed to the
TUI as a diff; the model's result text is unchanged. For `bash` the diff comes
from two snapshots of the workspace around the command, taken with a private
Git index and object store under `~/.pollytool/changes/`. Untracked files are
included and ignored files excluded; the repository's own index, refs, and
objects are never written. Outside a Git repository, or when the repository is
too large to snapshot quickly, Bash calls show no diff.

**Bash.** `bash` runs `bash -c` and reports the final process exit status.
Pipelines use the last command's status. These defaults apply to parent
commands, worker commands, and workflow `exec`; external shell tools and
separately launched scripts keep their own shell options.

Each Bash call starts a fresh shell. Changes made by `cd`, exports, shell
variables, and shell options do not persist into later calls. Repeat required
directory and environment setup in each call, or source a setup file within
that call. Use supplied writable scratch or temporary paths for tool caches and
disposable build output. Treat sandbox permission failures as environment
limits; do not change ownership, persistent user configuration, or project code
to bypass them.

Use `set -e -o pipefail` when every command and pipeline stage must succeed,
and handle expected failures with `if`. Account for intentional early-reader
termination such as `yes | head` when enabling `pipefail`. Bash
conditional-list exceptions apply: `false && printf unreachable; printf later`
succeeds. Run required validations separately or propagate failures
explicitly. For Go mutation tests, use `go test -count=1` to avoid cached
results. Utility flags depend on the host platform.

**Recall.** `read_artifact` pages or searches conversation artifacts and
evidence explicitly published in the session's swarm, and can reattach stored
images. Other agents' unpublished artifacts stay private.

**Coordination help.** Parents also have `swarm_help()` for coordination
guidance. It points to `workflow_help()` for the JavaScript API and runnable
examples when writing workflow scripts. Both guides ship inside the binary and
work outside Polly's checkout; reuse them from history and reload when needed.
See [Swarms and workflows](docs/WORKFLOWS.md).

### Shell tools

Any executable answering `--schema` (JSON Schema on stdout) and
`--execute <json-args>` (result on stdout, exit 0). A top-level `"sandbox"`
schema field customizes its policy.

```bash
#!/bin/bash
# uppercase.sh
case "$1" in
  --schema)  echo '{"title":"uppercase","description":"Uppercase text","type":"object","properties":{"text":{"type":"string"}},"required":["text"]}' ;;
  --execute) jq -r .text <<<"$2" | tr a-z A-Z ;;
esac
```

### MCP servers

Claude Desktop-format JSON. Stdio servers run sandboxed; `sse` and streamable
HTTP servers (`"transport"`, `"url"`, `"headers"`, `"timeout"`) run elsewhere.

## Skills

[Agent Skills](https://agentskills.io/specification), one `SKILL.md` per folder.
`--skilldir` (default `~/.pollytool/skills`), `--listskills`, `-S <dir|git|url>`
loads and activates one, `--noskills` disables. Activation loads `mcp/` JSON
as servers and lists `scripts/` as paths to run via the bash tool.

Builtin skills ship inside the binary and are always discoverable: they are
synced to `~/.pollytool/builtin-skills` (a polly-managed directory) at
startup, and a same-named skill in your own directories shadows the builtin.
Currently that is `feature-workflow`, an end-to-end feature pipeline —
brainstorming and grilling into an approved spec, a research fan-out that
produces an implementation plan, then parallel implementation in dependency
waves with review and integration (see `docs/WORKFLOWS.md`).

## Structured output

`--schema person.schema.json`: validated JSON on stdout. Works with `-f image.jpg`.

## Sandboxing

Tool commands run sandboxed by default. `--sandbox <preset+preset>` (`POLLYTOOL_SANDBOX`):

| Preset | Meaning |
|---|---|
| `base` | temp-dir writes only, no network, home directory private |
| `readonly` | no writes, no network, home directory private |
| `workspace` | working directory writable; Git metadata read-only |
| `git` | with `workspace`: `.git` writable except config, hooks, routing pointers |
| `net` | outbound network |
| `ssh` | `SSH_AUTH_SOCK` passes; `~/.ssh/config` and `known_hosts` readable |
| `sshkeys` | all of `~/.ssh` readable |

Default `workspace+net+git`. Your home directory is hidden from tools except
your Git configuration with its includes, the install prefixes of `PATH`
entries under home, skill directories, and paths you grant with `--readpath`.
Also `--writepath`, `--denypath`, `--allownet`, `--nosandbox`.
Details: [SANDBOX.md](docs/SANDBOX.md).

## CLI reference

`polly --help`. Most flags have a `POLLYTOOL_*` environment variable, which
`~/.pollytool/config` can also set (see First run).

## See also

[Soulshack](https://github.com/pkdindustries/soulshack): IRC chatbot built on Polly.

## License

MIT
