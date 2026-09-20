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
on pending approval) is always on. Its colors come from your theme — see
[Themes](#themes) — except the fixed amber approval border and image rendering,
which no theme can change.

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
/add-dir [path]  (list or add extra read-only directories)
/sandbox [show|storage|clean caches|reset environment|try [command]|allow <kind> <item>|forget <item>]  (this workspace's sandbox profile)
/init [notes]  (set up the sandbox and record verified commands in AGENTS.md)
/set [key [value]]   (model, temp, maxtokens, maxcontext, thinking, tooltimeout)
/sessions  /new  /close  /inspect  /spawn  /workflow  /theme [name]
/tools [list [namespace]|show <name>|restart <server>]  /title <text>  /rename <name>
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

## Themes

A theme is a named set of colors for a fixed vocabulary of 25 role names.
Call sites name roles rather than colors, so one theme restyles the whole
interface at once: the transcript, the masthead bird at the top of it, the
inspector frame and its scrollbar, Markdown, fenced code, diffs, and tool
output. `--theme` / `POLLYTOOL_THEME` selects one.

### Roles

| Group | Roles | Unset value |
|---|---|---|
| Semantic | `ok` `err` `run` `accent` `active` `muted` `code` | polly's built-in color |
| Token | `syn-comment` `syn-keyword` `syn-string` `syn-number` `syn-func` `syn-add` `syn-del` | the semantic role it borrows |
| Bird | `polly-green` `polly-light` `polly-wing` `polly-crown` `polly-beak` `polly-mouth` `polly-face` `polly-eye` `polly-foot` | the built-in bird color |
| Surface | `background` `foreground` | `inherit`: the terminal's own |

The surface roles color the whole interface: `background` fills every cell
that names no background of its own, and `foreground` is the color of plain
text — your input and the model's replies. Set them together, since the
terminal's default text color was chosen for the terminal's background, not
yours. Only the full-screen TUI paints them; the line frontend scrolls with
the terminal and keeps its background.

Token roles carry syntax highlighting: comments, keywords, strings, numbers,
function names, inserted lines, and deleted lines. An unset token role falls
back to the semantic role it has always borrowed — `syn-comment`→`muted`,
`syn-keyword`→`accent`, `syn-string`→`ok`, `syn-number`→`active`,
`syn-func`→`code` (keeping its bold), `syn-add`→`ok`, `syn-del`→`err` — so a
theme that sets only semantic roles renders as it did before the token roles
had names. Fenced code and diffs resolve through the token roles, and bash and
generic tool result bodies are highlighted with the same fence token map;
inline code uses `code`.

### Values

A role's value is one of five forms:

- `#rrggbb` or `#rgb` — true color.
- `palette:N`, N from 0 to 255 — that ANSI palette slot. `palette:0` is the
  terminal's black slot and is distinct from an omitted key; slots 0–15 stay
  remappable by the terminal.
- A name the built-in parser knows, such as `green`, `grey`, or `darkred`.
- `inherit` — no color, so the terminal's default foreground shows through.
  Rejected for `accent` and `muted`, whose resolved colors other heuristics
  compare.
- Omit the key — polly's built-in color for that role.

The names of polly's own roles are not values: `"accent": "muted"` is rejected
rather than resolved, because values resolve against the built-in parser names.

### Theme files

A user theme is `~/.pollytool/themes/<name>.json`:

```json
{
  "name": "solarized-ish",
  "colors": { "accent": "#268bd2", "muted": "palette:8" }
}
```

A theme is one look: for a light and a dark version, write two themes and
switch with `/theme`. `name` and `colors` are the whole shape: an unknown
top-level key, an unknown role name, and an unparseable value are all load
errors, as is a `palette:N` outside 0–255 or `inherit` on `accent`/`muted`.
The retired `variant`, `light`, and `dark` keys of older files are the one
exception — they are ignored with a notice, so such a file keeps loading with
its `colors` alone.

Load errors never stop startup. An unknown name, a missing path, or a malformed
file prints a notice and falls back to the built-in `default` preset; `--quiet`
suppresses the notice. An unreadable user file is reported rather than silently
shadowed by a same-named preset.

### Selection and detection

`--theme` takes a preset name, a user theme name, or a path. A value containing
`/` or ending in `.json` is read as a path; otherwise
`~/.pollytool/themes/<name>.json` wins over a same-named compiled-in preset.
`default` is reserved — it is what every load failure falls back to, so a user
file of that name is ignored. Precedence is `--theme` > `POLLYTOOL_THEME` >
`~/.pollytool/config` > built-in `default`.

The `default` preset uses the ANSI palette slots the terminal itself remaps,
plus the fixed true colors of the bird, and leaves the background and text
color to the terminal — so it reads on a light or a dark terminal alike, and
nothing about the terminal is detected.

Four full themes ship as well — `amber-parrot`, `azure-parrot`,
`midnight-parrot`, and `verdant-parrot` — each setting every role, the
background and the bird included. At startup polly writes any of them that
has no file yet to `~/.pollytool/themes/<name>.json`, so they are yours to
edit: the file shadows the compiled-in copy and reloads live like any user
theme. An existing file is never overwritten; delete one to get the shipped
version back on the next launch.

### Applying a theme

`/theme` opens a picker of the presets and your theme files with the active
one selected: moving the selection previews each theme on the whole screen,
typing filters, Enter switches, and Escape puts the current theme back.
`/theme <name>` switches directly and reports the file the theme came from,
and `/theme default` returns to the preset. Either way the choice is saved as
`POLLYTOOL_THEME` in `~/.pollytool/config`, so the next launch starts on it; a
preview saves nothing. The interactive
line frontend (a redirect or `TERM=dumb`) has no repaint tick to reload on and
answers `/theme` with "theme switching unavailable"; a one-shot `-p` run takes
its theme from the flags and has no command input at all.

Editing the active theme file — or changing `POLLYTOOL_THEME` in
`~/.pollytool/config` — hot-reloads in the TUI: a size-and-mtime stat at most
once a second, applied on the next frame with no restart and no stale colors
anywhere. A half-written file keeps the previous theme and is retried; the
warning is printed once per failure, and the line frontend does not reload at
all.

`set_theme` is the model-facing path and the only writer of theme files. Without `persist` it
restyles the running session only. With `persist` and no `confirm` it writes
nothing and returns the exact path and colors in a `confirmation_required`
reply, so the model can show them and ask; a second call with `persist: true,
confirm: true` writes `~/.pollytool/themes/<name>.json` and sets
`POLLYTOOL_THEME` in `~/.pollytool/config` for later launches. Replacing an
existing file needs `overwrite: true`, and `default` is refused as a name. The
builtin `theme-designer` skill interviews you and drives this protocol. Error codes are
`UNKNOWN_ROLE`, `INVALID_COLOR`, `INVALID_THEME`, `THEME_EXISTS`, and
`THEME_WRITE_FAILED`.

Sandboxed tools cannot reach theme files by default: `POLLYTOOL_*` is stripped
from tool environments and `~/.pollytool/themes` is not a home read grant, so
`set_theme` is the sanctioned writer. A tool that must read a theme file needs
`--readpath ~/.pollytool/themes`.

### Color in the line frontend

The line frontend writes its own SGR. Palette slots 0–15 keep the classic codes
(`30`–`37` and `90`–`97` foreground, `40`–`47` and `100`–`107` background), so
the terminal still remaps them; slots 16–255 use `38;5;N` / `48;5;N`; a
true-color value uses `38;2;r;g;b` / `48;2;r;g;b` on a terminal that advertises
true color (`COLORTERM` of `truecolor`/`direct`/`24bit`, or a `TERM` ending in
`-direct`/`-truecolor`) and otherwise degrades to the nearest palette index.
`inherit` emits no color code, so the terminal default shows. With `NO_COLOR`
set, under `TERM=dumb`, or when stdout is not a terminal, no SGR is written at
all. `NO_COLOR` is honored by the line frontend, and by tcell itself in the
TUI, which renders monochrome while it is set; `TERM=dumb` never reaches the
TUI because polly falls back to the line frontend under it.

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
| QwenCloud | `qwencloud/qwen3.8-max` | `POLLYTOOL_QWENCLOUDKEY` |
| DeepSeek | `deepseek/deepseek-v4-pro` | `POLLYTOOL_DEEPSEEKKEY` |
| OpenRouter | `openrouter/openai/gpt-5` | `POLLYTOOL_OPENROUTERKEY` |
| Ollama | `ollama/gpt-oss` | `POLLYTOOL_OLLAMAKEY` (optional) |
| Hugging Face | `huggingface/...` | `POLLYTOOL_HUGGINGFACEKEY` |

`--baseurl` selects the inference and metadata endpoint for OpenAI-compatible
servers and Ollama. Native Anthropic and Gemini requests use their provider
endpoints.

QwenCloud uses the [international Chat Completions endpoint](https://docs.qwencloud.com/developer-guides/getting-started/first-api-call).
Set `POLLYTOOL_QWENCLOUDKEY` to your QwenCloud API key; use `--baseurl` for
a different compatible endpoint. Thinking `off` sends `enable_thinking: false`,
`dynamic` enables thinking at the model default, and levels or token budgets
send `thinking_budget`. Select a model supporting those controls; thinking-only
models cannot disable thinking. Assistant reasoning is replayed separately from
answer text.

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
`spawn_agent`, `set_session_title`, `set_theme`, `view_image`, and the recall
tools `list_artifacts`, `read_artifact`, and `read_transcript`. Any `--tool`
replaces the set.

**Theme.** `set_theme` restyles the running session; `persist: true` writes
`~/.pollytool/themes/<name>.json` only after a second call with `confirm: true`
(see [Themes](#themes)). It is registered for the full-screen TUI only, which is
the only frontend with a color table to keep.

**Sandbox setup.** During `/init`, `sandbox_prepare` creates and saves isolated
cache, dependency and configuration storage without a permission review.
`sandbox_trial` diagnoses unexpected denials; `sandbox_propose` reviews new host
access. These tools are restricted to the top-level init turn. Versioned recipes
cover Go, JavaScript package managers, Python, Rust, Java, .NET, Zig and C/C++.
See [Sandboxing](#sandboxing).

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
waves with review and integration (see `docs/WORKFLOWS.md`); `theme-designer`,
which interviews you about the colors you want and drives the `set_theme`
two-call persist protocol (see [Themes](#themes)); `simplify`, which fans
out four read-only reviewers (reuse, simplification, efficiency, altitude) over
your changes and applies the cleanups that keep behavior intact; and
`sandbox-setup`, which `/init` activates to set up the workspace's sandbox
profile and record verified build and test commands in `AGENTS.md` (see
[Sandboxing](#sandboxing)).

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
| `private-home` | hide home except granted toolchain/configuration paths |
| `ssh` | `SSH_AUTH_SOCK` passes; `~/.ssh/config` and `known_hosts` readable |
| `sshkeys` | all of `~/.ssh` readable |

Default `workspace+net+git`. Tools can read your existing home configuration,
toolchains, and other ordinary files; home writes still require a specific grant.
Known credential paths and Polly's internal storage remain masked. This does not
hide arbitrary secrets or personal files elsewhere in home. Add `+private-home`
for the stricter home visibility policy.
Also `--writepath`, `--denypath`, `--allownet`, `--nosandbox`. Credential
paths (`~/.ssh`, `~/.aws`, `~/.npmrc`, ...) stay masked unless you grant one
explicitly; the masthead and `/set sandbox` then name what is exposed.

Multi-directory projects get extra read-only paths with `--add-dir <path>`
(repeatable, also with one-shot `-p`): a repo plus sibling dependency repos,
vendored checkouts, or adjacent data trees become readable without widening
the writable workspace or weakening the preset. Each entry must exist as a
directory; the filesystem root, your home directory or an ancestor of it,
temp directories, paths inside the workspace, and directories containing or
inside masked credential paths are rejected (grant a credential with
`--readpath`), while an ancestor of the workspace is allowed (it also
exposes the workspace's siblings, read-only). The list is
per-session: it persists on the session record, resuming with `--add-dir`
merges into it, and `/add-dir <path>` adds a directory mid-session
(`/add-dir` alone lists them). A mid-session add reaches bash and shell
tools at once; a running MCP server keeps its earlier sandbox, and the reply
names it: `/tools restart <server>` starts it again under the new policy. Entries are read-only at both layers —
sandboxed writes and the file tools refuse them — and sub-agents, swarm
members, and worktrees inherit them as read grants. There is no
`POLLYTOOL_ADDDIRS` default: extra dirs are never an ambient grant, and
they are accepted with `--nosandbox` (where they persist and appear in model
context) so sandbox-less platforms keep the listing.

Exceptions a workspace needs every session live in its sandbox profile,
kept per repository under `~/.pollytool/workspaces/` where no sandboxed
command can reach it:

```
/init                                     set up the sandbox and update AGENTS.md
/sandbox try make test                    run it, see what the sandbox denied, allow some
/sandbox allow read ~/src/protos          a directory outside the workspace
/sandbox allow write ~/.foo/cache         a directory a tool insists on
/sandbox allow env GOCACHE=@cache/go-build   a cache of the workspace's own
/sandbox allow passenv NPM_TOKEN          a token the sandbox strips
/sandbox                                  list; /sandbox forget <n> removes one
```

`/sandbox try` runs the command as a trial and lists what the sandbox denied
it, each with the read or write grant that would allow it. Nothing is
ticked for you: tick what the workspace needs, run it again with them, then
save them to the profile or keep them for this session only. Alone,
`/sandbox try` offers the session's recent failed bash commands.

`/init` reads project instructions, lockfiles and CI, selects recipes, and
prepares predictable isolated storage before the first build. It uses existing
toolchains and runs dependency bootstrap, build and tests in the native sandbox.
New host reads, credentials and changes to host installations remain explicit.
Settings and storage persist across sessions; linked worktrees share repository
identity but have separate mutable state/configuration. Only recipes declaring
concurrent cache use share managed caches.

After final ordinary sandboxed verification, `/init` updates the dedicated
`Build and test in Polly's sandbox` section in `AGENTS.md`, including executable
bootstrap commands for new worktrees. It reports **Verified**, **Verified with
sandbox exclusions**, or **Incomplete**. An exclusion requires an observed
inherently unavailable operation and inspection of the named test, followed by a
successful filtered run that actually executes tests. Bugs, fixable permissions,
missing dependencies and unavailable services remain failures. Full-suite
commands and unrelated instructions are preserved.

`/sandbox show` distinguishes automatic settings from explicit grants;
`/sandbox forget` removes either. `/sandbox storage` lists paths, sizes, purposes,
sharing and cleanup categories. `/sandbox clean caches` clears only tracked
caches. `/sandbox reset environment` also clears tracked dependency state while
preserving configuration, declarations and explicit grants. It leaves checkout
files such as node_modules, .venv and build outputs alone and requires dependency
restoration and verification afterward. Cleanup runs in the background, requires
an idle session with members stopped, and reports busy if another session holds
the environment. Legacy/unmanaged directories are never silently adopted or
cleaned. `/sandbox forget @state/name` revokes that allocation's automatic grant;
its ownership record remains available for cleanup. A later `/init` can prepare it
again under the same name.

A change applies at once and every later start loads the profile, one-shot
`-p` included; `--nosandboxprofile` leaves it out of one launch. Items that
would let a sandboxed command change what the host runs are refused (PATH
directories, shell startup files, Git hooks and configuration, other
repositories, polly's own state), and a credential item stops applying if
the repository's origin changes.
Details: [SANDBOX.md](docs/SANDBOX.md).

## CLI reference

`polly --help`. Most flags have a `POLLYTOOL_*` environment variable, which
`~/.pollytool/config` can also set (see First run).

## See also

[Soulshack](https://github.com/pkdindustries/soulshack): IRC chatbot built on Polly.

## License

MIT
