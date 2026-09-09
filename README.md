# Pollytool (polly)


My [LLM](https://en.wikipedia.org/wiki/Stochastic_parrot) harness.
There are many like it. This one is mine.

This file: the CLI and the TUI. [API.md](API.md): the Go library.
[SANDBOX.md](SANDBOX.md): the sandbox.

![polly TUI](.assets/interactive.png)

## Install

```bash
go build -o polly ./cmd/polly/
```

## Quick start

```bash
export POLLYTOOL_ANTHROPICKEY=...
export POLLYTOOL_OPENAIKEY=...

polly                                   # TUI
echo "Hello?" | polly                   # one-shot, stdin
polly -p "Hello?"                       # one-shot, flag
polly -m openai/gpt-5.4 -p "Quantum computing in one breath"

polly -f image.jpg -p "What's this?"    # files, images, URLs
polly -f notes.txt -f https://example.com/chart.png -p "Tie these together"

polly -p "uppercase this: hello" --tool ./uppercase.sh                                # shell tool
polly -p "create news.txt with today's news" --tool perp.json --tool filesystem.json  # MCP servers
```

Default model: `anthropic/claude-sonnet-4-6`. Override with `-m provider/model`
or `POLLYTOOL_MODEL`.

## One-shot output

With `-p` or piped input, stdout contains one settled answer (or blocker report).
Progress remains live on stderr. Use `--stream` to emit intermediate assistant
text immediately. This applies to all one-shot runs, including runs without a swarm. A capable terminal gets up to two compact status rows,
collapsing to one when they fit. Activity, thought duration, parent tools,
agents, and inspected images occupy the first row; elapsed time and token usage
occupy the second. Usage updates after each provider iteration.
Successful calls update compact counts instead of printing individual tool or
agent rows. Failures and denials get brief notices. Child activity is labeled
with its agent; unnamed agents get short numbered names instead of repeating
their task briefs. While waiting for agents, the status shows aggregate agent
counts. Child work stays under Agents and does not inflate parent
tool, image, or token totals. The final summary is the TUI's turn trailer, same fields in
the same order, without its click glyphs. A turn cut short by a token or iteration cap reads
`incomplete`. Status colors use the TUI's terminal palette.

Stdout carries only the answer. Redirecting or piping it preserves raw Markdown
without rendering escapes, receipts, or status-induced whitespace; terminal
progress on stderr remains live independently. Redirected stderr and `TERM=dumb`
get plain state changes and a complete, untruncated trailer without cursor
controls. `NO_COLOR` disables color while retaining supported cursor updates. `--quiet` suppresses activity and details, including
image receipts, but still reports warnings and errors.

With `--stream` on a terminal, completed answer blocks enter scrollback. Only the visible
answer tail is restyled as Markdown arrives or the terminal resizes. Tool
calls, notices, approvals, images, and completion flush that tail. Native
images are transmitted once when committed.

`--activity-details` (or `POLLYTOOL_ACTIVITY_DETAILS=true`) prints bounded
Thought, Tools, Agents, and Images groups on stderr after the answer and before
the trailer, including on failure or cancellation. Thought shows the last five
lines; Tools shows up to six rows, with earlier calls counted in an elision row.
Agent summaries follow launch order. Image receipts follow result order, with
up to 64 captions and a count for additional receipts; previews are not resent.
The flag applies to one-shot runs only; the REPL ignores it, so the environment variable can stay exported.

```bash
polly -p "Review this change" > answer.md
polly -p "Continue the review" >> answer.md
polly -p "Explain the code" --activity-details > answer.md 2> activity.log
polly -p "Summarize" | cat
```

Structured `--schema` stdout is unchanged. With `--meta`, all activity and
optional details finish before the existing `polly-meta` record on stderr.

## TUI

No `-p`, no piped stdin: full-screen TUI. Streaming, scrollback, reverse
history search, bracketed paste.

Run `polly --theme=halo` for mint chrome on the inspector and charcoal and mint
dialogs. The inspector shares the main conversation's background. The main
conversation, composer, startup screen, and status controls
keep their original colors and layout. The existing appearance remains
`--theme=default`. `POLLYTOOL_THEME=halo` sets the startup default; an explicit
flag overrides it. Themes apply only to the managed TUI and are not saved in
sessions.

Halo gives the inspector its own rounded frame, scrollbar, and a leading `<`
parent control, with no separator beneath the title. `/inspect maximize` expands or
restores it. The frame stays when maximized or when a narrow
terminal shows only the inspector. Drag its left edge to resize the split, drag
the scrollbar thumb, or click the track to page. Existing wheel and keyboard
controls continue to work, and scrolling to the bottom resumes following new
output. Dialog lists support scrollbar controls, row clicks, and
Home/End/Page Up/Page Down. The composer retains automatic multiline sizing.

A glint travels around the inspector while the inspected work runs. Idle frames
stay still; pending approvals use a steady amber border. Motion pauses for
dialogs, quiet mode, and lost focus. Very small terminals use compact inspector
layout with Halo colors; limited-color and monochrome terminals retain readable
fallbacks.

No managed screen (`TERM=dumb`, redirected endpoints): line frontend.
Markdown on a capable TTY, with color unless `NO_COLOR` is set. Raw text when
redirected or `TERM=dumb`.

### Sessions

Every launch without `-c` gets a session with a generated name (`quiet-otter`).

- Resume: `polly -L` (last active), `polly -c quiet-otter`, or `/resume`.
- Keep: `/rename`. Named sessions never expire. Generated ones expire after 7 idle days.
- A session with no turns is discarded on exit.
- Status row: model and context use. `ctx 41.2k/156k`. A leading `~` means local estimate.

### Tabs

One polly, many concurrent session workspaces. The tab list contains root
sessions; agents, tools, and thoughts open in one inspector inside a workspace.

| Command | Effect |
|---|---|
| `/resume` | Select or open a saved workspace. Selecting an agent opens its root workspace and inspects the agent. Clicking the status-row session name opens the picker. |
| `/new` | New tab, fresh generated session |
| `/tab` | List root workspaces, including descendant activity and approval counts |
| `/tab <n>`, `/tab <name>` | Switch |
| `/parent` | Go up from a tool or thought to its conversation, or from an agent to its caller. |
| `/close` | Close the visible tab. Session stays saved. A generated session with no turns is discarded. Last tab closed: polly exits. |
| `Alt+1`..`Alt+9` | Jump to a tab by position |
| `Alt+]`, `Alt+[` | Next tab, previous tab |

Rules:

- Settings are per tab. `/set` and `/model` touch only the visible one.
- Turns keep running in hidden tabs. Start a long run, switch away, keep working. Input queued behind a hidden turn runs when it settles.
- A hidden tab that finishes or fails posts a one-line notice in the visible transcript, once the visible tab is idle.
- An agent needing approval raises a persistent attention indicator. Press **Ctrl-G** or use `/agents`, select it, and choose **Review** for an explicitly addressed approval dialog.
- Closing a workspace with active turns is refused. Ctrl-C interrupts the root turn; **Stop** in an agent inspector interrupts that agent.
- Quitting with turns running elsewhere warns once. A second Ctrl-C cancels them, waits briefly for completed work to save, exits.
- Open sessions are leased. The picker marks sessions held by another polly `in use` and will not open them. Picking a session already open here jumps to its workspace. When opening an agent whose parent is leased elsewhere, the parent is a labeled read-only snapshot. Execution still requires acquiring its lease. A deleted or expired parent is never recreated; a surviving agent opens with **Parent unavailable**.

### Subagents

The model delegates with `spawn_agent`. A parent and its direct children
form a shared swarm automatically. Children cannot spawn another generation.
Each has a stable session ID, an assignment, private conversation history, and
access to a roster, addressed messages, tasks, and explicitly published findings.
Only the parent creates or reassigns tasks, accepts results, and integrates edits.

Children inherit the parent's configured `--maxiterations` limit (default 1024
model calls). `spawn_agent` and JavaScript `polly.agent` cannot override it;
`max_iterations` / `maxIterations` arguments are rejected. This is a ceiling,
not a target: agents finish as soon as their assignments are complete.

Editing children receive isolated Git worktrees seeded from the parent's current
files, including staged, unstaged, and non-ignored new files. Source paths choose
what to copy; they never grant editing access to that checkout. Children do not
write repository Git metadata or commit. The parent previews a three-way merge
and applies an accepted candidate without changing its index or branch. Git 2.40+
is required. `read_only: true` supports research; outside Git it observes live
files. See [worktree restrictions](WORKFLOWS.md#worktrees-and-integration).

Tools, native paths, shell commands, local MCP servers, repository instructions,
and available skills bind to the member's directory. Incompatible optional tools
are reported and omitted; explicitly requested incompatible tools fail launch.
Semantic search is omitted in members; use exact search with bash. `tools: []`
disables every model tool, including coordination and private built-ins.

Up to 32 children execute concurrently, with 256 logical executions per run.
`--swarm-concurrent` and `--swarm-executions` set these limits. Waiting releases
resources and preserves the remaining iteration allowance; it does not spend a
new execution. Additional service turns and retries do. Quitting pauses work;
reopening the parent and using `/swarm resume ID` explicitly resumes it. A
finished member keeps its conversation and files until cleanup.

Reaching the iteration limit pauses the member and displays the used/allowed
model calls. Its conversation, task, findings and worktree remain available.
`/swarm resume ID N` explicitly grants **N additional model calls** and continues
the same execution without spending another logical start. Plain `resume` cannot
reset an exhausted allowance. Changing the configured default affects new executions;
existing ones retain their saved limit until an explicit grant. This is separate
from `/swarm grant N`, which adds logical execution starts to the family budget.
If that budget is paused, grant it separately before resuming members.

**Messages and background work.** `background: true` returns a stable member ID
immediately. Prefer background calls plus `swarm_wait` while coordinating. A
blocking spawn can also return `yielded` when a child needs the parent; the child
is still alive. Requests and replies can wake idle members. Information waits for
the next active turn. Workflow reservations and stopped/failed members prevent
unsolicited restarts. Mail is admitted before a model request, after the entire
previous tool batch, with an atomic transcript checkpoint and delivery receipt.

Parent answers remain provisional while work is outstanding. The runtime waits
for active work and gives the parent a corrective turn for unresolved review,
reply, or integration work. An unchanged blocker produces an incomplete result,
not an endless wait. One-shot memory sessions are promoted to the normal SQLite
store before the first coordination mutation, so the parent can be resumed.

Inline agent rows and `/agents` show the swarm's current member state, including
queued, running, waiting, paused (with the iteration-limit reason when applicable),
failed, and awaiting review. A completed
background spawn call does not mean its member finished. Status refreshes do
not require a child tab or acquire the child's execution lease.

`/swarm` opens the inspector. Categories include members, tasks, messages,
publications, workflow reports, and integration candidates/previews, with a raw view for
full saved records. `/swarm stop ID`,
`/swarm resume ID [ADDITIONAL_ITERATIONS]`, and `/swarm grant N` control execution; budget grants require
an explicit client/user action. `/swarm cleanup CONTEXT_ID` retires an inactive
context only when its current edits are integrated. `/swarm cleanup all` also
retires Git snapshot references. Unintegrated changes are refused.

Parent tools can combine several editing task revisions with `swarm_integration`.
Prepare, resolve conflicts in a separate copy, review/check, accept, and apply.
The default checks touched paths and preserves unrelated parent edits; choose
`drift:"tree"` when the whole parent checkout must match the validated snapshot.
`/swarm integrations` shows conflicts, supersession, acceptance, and apply receipts.

**JavaScript workflows.** `/workflow SCRIPT.js INPUT.json` starts an explicit
workflow and saves its report. Model tools `workflow_run`, `workflow_start`,
`workflow_read`, `workflow_cancel`, and `workflow_acknowledge` use that same runtime in the library, CLI,
and TUI. Scripts coordinate typed agent results, commands, and tools; they cannot
accept or integrate tasks. See [the workflow API and runnable example](WORKFLOWS.md).
Managed members are inspected through `/agents`; resume execution through their
parent so the worktree and policy are restored together.

**In the TUI.** Inline Thought, Tools, Agents, and Images viewed summaries
keep their existing expand/collapse behavior. Clicking an expanded agent task,
tool row, or thought detail opens the inspector. **Ctrl-G** and `/agents` open
the Agents dialog, showing attention-needed agents first, then running agents,
then completed agents newest first. Saved agent outcomes describe the initial
delegated run; later follow-ups do not change that outcome.
Expanded agent rows show input/output token counts as each model response reports
usage. Input is the peak request size and output is the total for the delegated
turn, matching the turn summary. Counts stay visible after that run finishes.

The inspector observes work while the main composer stays addressed to the root
session. **Stop** cancels the inspected agent's current turn and
keeps its view open. **Review** addresses that agent's pending approval without
changing the inspected target. Completion never switches your selection.

At 120 columns or wider the default split is 70% transcript and 30% inspector,
subject to each pane's minimum width. Drag
the divider to resize, with at least 50 columns per pane. Narrow terminals show the
inspector above the main composer. Typing stays in that composer.
Mouse scrolling follows the pointer. Left/Right navigate previous/next tools or
thoughts while the pointer is over the inspector; elsewhere they move the editor
cursor. Over either transcript pane, Up/Down scroll one line, Page Up/Down page
through that pane, Home goes to the top, and End follows new output at the bottom.
Over the composer, arrows and Home/End edit normally; Page Up/Down scroll the
main transcript. Ctrl-A/Ctrl-E always address the editor. Dialogs and searches
retain their keyboard controls.
Clicking the title or its leading `<` goes to the parent, or closes the inspector
when the parent is the main session. Tool names and thought positions appear
without a parent breadcrumb. Agent actions appear below the title.
Agent headings show the task title, falling back to the session name when untitled.
Agent transcript inspectors collapse the launch prompt into a clickable
`▸ Prompt` row above the first agent message; later follow-up prompts remain visible.
Tool inspector arguments use a JSON code block with syntax highlighting.
Use `/inspect prev`, `/inspect next`, `/inspect back`, and `/inspect forward`
for navigation, `/inspect find` for search, and `/inspect maximize`,
`/inspect wider`, or `/inspect narrower` to adjust the pane. Agent **Stop** appears
while running and **Review** appears when an approval is pending.
Switching inspector items starts at the top; press End to follow new output.
Escape dismisses a dialog or search first, then closes the inspector, before
existing cancellation handling. Ctrl-C always keeps its root interrupt behavior.

`/inspect` reopens the last selection, otherwise the newest root tool call.
`/inspect thoughts` opens the newest thought block. Tool navigation includes
all saved turns in that conversation, including agent launches, in transcript
order without wrapping. Parent and child sequences stay separate. Launch views
include arguments, result, and an **Open agent** action. Tool views show running status,
then captured completed output, with arguments always visible, readable JSON,
plain text, supported images, and full stored text artifacts. They do not reread files
or stream intermediate shell output. Missing, empty, and unavailable full output
are labeled. Thought views show the reasoning text supplied and retained by the
provider. Scrolling away holds position and offers **new output** when it arrives.

Inspection does not acquire leases or keep completed execution runtimes alive.
Conversation, tool, and thought display projections share a cache of at most
16 inactive views and an estimated 64 MiB, including a four-entry/8 MiB allowance
for unviewed completions. User visits determine recency. Displayed panes stay
available; eviction drops reloadable display content, preserving navigation,
search, expanded sections, scroll state, and drafts separately. Saved content
loads and formats off the UI loop. Workspace state and split width last for this
Polly run; a restart begins with the inspector closed unless opening an agent
explicitly with `polly -c <agent>`.

Closing a workspace with running agents is refused.
`/spawn <brief>` starts a background child of the visible tab by hand.

One-shot and line mode: the tool always waits for the reply.

### Keys

| Key | Action |
|---|---|
| `Ctrl-C` | Interrupt the root turn. Again, or at an idle prompt: quit |
| `Esc` | Dismiss dialog/search, then close inspector, then interrupt |
| `Left` / `Right` | Previous/next tool or thought while hovering the inspector; otherwise move cursor |
| `Up` / `Down` | Scroll hovered transcript; otherwise move input line or recall history |
| `Page Up` / `Page Down` | Page hovered transcript; main transcript by default |
| `Home` / `End` | Top/follow bottom of hovered transcript; otherwise input line start/end |
| `Ctrl-A` / `Ctrl-E` | Input line start/end regardless of pointer |
| `Ctrl-R` | Reverse history search |
| `Ctrl-G` | Open the Agents dialog; cancel reverse history search when active |
| `Ctrl-O` | Toggle the reasoning disclosure |
| `Ctrl-V` | Attach an image from the clipboard |
| `Ctrl-Z` | Suspend. `fg` resumes |

Input submitted mid-turn is queued and marked `(queued)`. Failed or
canceled input returns to the composer as a draft.

Mouse reporting is on, for scrolling and image clicks. Select text with
the terminal's override, usually Shift-drag.

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
/resume                      Select or open a saved workspace
/new                         Open a new tab on a fresh session
/tab [n|name]  (/tabs)       List root workspaces, or switch to one
/close                       Close the visible tab (session stays saved)
/parent                      Go up through the inspector to the caller
/inspect [tools|thoughts]    Open the last view, newest tool, or thoughts
/agents                      Pick an agent to inspect
/spawn <brief>               Start a background agent that reports back here
/tools [list [ns]|show <n>]  List or inspect loaded tools
/skills                      List discovered Agent Skills
/rename <name>               Rename the current context
/reset confirm               Clear durable conversation history
/exit  (/quit)               Leave the TUI
```

Keys set with `/keys` live until polly exits. Never written to the
transcript, history, database, or environment.

### Tool calls and reasoning

Tool activity: one collapsed `▸ N tool calls` row per turn. Click it for
timers, outcomes, and tool-produced images, updating in place. Details
show call labels and outcomes; click a detail row for the full captured result
in the inspector. The model still
gets every result. Durable history keeps the full exchange.

`--thinking`: reasoning shows as a collapsed `▸ thought 2.1s` row whose
elapsed timer ticks up live while thinking. Click it, or
`Ctrl-O`, for a live tail.

Both collapse when the turn ends. Both reopen later, even after a reload.

Small accents acknowledge disclosure toggles, completed agent counts, new
context usage, and queued input. A delivered agent report briefly lights
`← Back to caller`. The empty composer has a slow idle cursor; typing restores
the terminal cursor. Incoming assistant text has a quick typewriter reveal
that catches up immediately when the response finishes. These effects use
your terminal's colors, pause while unfocused, and are omitted in quiet mode
or with `NO_COLOR`.

### Interrupted turns

A failed or canceled turn keeps everything it finished. Each completed
model iteration and tool result is written to durable history. A retry
continues from real state. Lost: only the text streamed by the
interrupted final call.

Labels: `failed · completed work saved`, `canceled · …`, or `not saved`
when nothing durable was produced.

### Images

**In.** Assistant Markdown with `![alt](./path.png)`, or a tool result
with a local image path alone on a line: thumbnail. Relative paths
resolve from polly's working directory. Remote images, and paths inside
prose, JSON, or code blocks: not opened.

Kitty graphics on Kitty, Ghostty, WezTerm. Sixel on Windows Terminal
1.22+ and foot. Everything else, tmux and Zellij included: caption and
path. Override with `POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none`.

**Out.** A local image path in the prompt attaches on submit.
`describe .assets/polly.png` works. `Ctrl-V` takes the clipboard image.
Drag-and-drop attaches a file. `/attach <path>` handles paths with
spaces. Each attachment is an `[image #N]` token at the cursor. Delete
the token, drop the attachment. Bytes are captured on submit, so later
changes to the file do not touch queued turns.

**Limits.** 16 images per prompt, 100 per request. 10 MB per image,
16 MiB per request. Downscaled to 1568px on the long edge. PNG, JPEG,
WebP pass through. GIF (first frame) and BMP become PNG. This is the
portable intersection of the
[OpenAI](https://developers.openai.com/api/docs/guides/images-vision),
[Anthropic](https://platform.claude.com/docs/en/build-with-claude/vision),
and [Gemini](https://ai.google.dev/gemini-api/docs/image-understanding)
image inputs.

## Contexts

A context is a named, persistent conversation. One-shot runs are
stateless without `-c`.

```bash
polly --create project --model openai/gpt-5.6    # create with settings
polly --show project                             # show its configuration
polly -c project -p "What database should I use?"  # continue
polly -c project                                 # continue in the TUI
polly --last -p "Explain the query"              # -L / --last: most recent context
cat notes.txt | polly -c project --add           # add stdin, no API call
polly --reset project                            # clear history, keep settings
polly --list                                     # list all, agents nested under parents
polly --list --flat                              # one line per context, for scripts
polly --delete project                           # delete one
polly --purge                                    # delete all (asks first)
```

Settings used with a context (model, temperature, system prompt, tools)
are saved to it and restored next run. Flags win over stored settings,
and the change sticks. A new system prompt on a context with history
resets the conversation. The stored system prompt holds only your custom
persona. With no custom persona, Polly adds a coding policy: respect the
requested scope, inspect before editing, preserve unrelated work, sequence
dependent changes, verify the result at a depth matched to its risk, and
reply in one short paragraph, with a brief plan and updates for substantial
work and the result, validation, and remaining issues for changes. Display
and conversation-recall guidance is added per request as well. These
defaults are never stored in the transcript. The Go library does not add
the CLI's coding policy.

Default coding turns also load `AGENTS.md` from the nearest Git root through
the working directory, in that order. A `.git` directory or worktree file
marks the root; outside a Git repository, only the working directory's file
is loaded. Instructions apply to their directory and descendants, with
more specific instructions taking precedence. The agent is told to check
for additional instructions before working in deeper directories. Files
are read again each turn under the sandbox read policy. A file that is
unreadable, non-text, or oversized is skipped with a warning, shown once
until the problem changes. Limits are 32 KiB per file and 64 KiB combined.

A non-empty `--system` / `POLLYTOOL_SYSTEM` replaces the coding policy and
automatic `AGENTS.md` loading. Display and recall guidance still applies.
`--schema` runs omit all of these CLI additions so the schema controls the
response. Explicit user requests take precedence over the coding defaults
and repository guidance.

Storage: one SQLite file, `~/.pollytool/polly.db`. Old JSON under
`~/.pollytool/contexts` is ignored. Backup: quit and copy the file, or use
SQLite's [online backup API](https://www.sqlite.org/backup.html) while it
runs. A plain copy of a live database can miss the write-ahead log.

## Models

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

`--baseurl`: a remote Ollama, or any OpenAI-compatible endpoint.

```bash
polly --baseurl http://192.168.1.100:11434 -m ollama/gpt-oss -p "Hello"
polly --baseurl https://api.openrouter.ai/api/v1 -m openai/whatevermodel -p "Hello"
```

- Native OpenAI: Responses API. With `--baseurl`: Chat Completions. Strict tool schemas with optional parameters go non-strict there.
- DeepSeek reasoning models need `reasoning_content` echoed on follow-up turns. Polly does it.
- OpenRouter: the upstream slug after the prefix. `openrouter/openai/gpt-5`.
- Ollama: schema support depends on the model.

## Tools

`-t`/`--tool`, repeatable. Detected by file type:

- `*.sh`: shell script, one tool
- `*.json`: MCP server config, one or more tools
- bare name: built-in (`bash`, `read_file`, ...)

Names are namespaced: `uppercase__to_uppercase`, `filesystem__read_file`.
`--confirm` asks before each call.

New contexts start with `bash` and the built-in file tools. `zvec_grep_search`
loads only when `zg` (zvec-grep) is on `PATH`. Any `--tool` replaces that default.

### Built-in tools

- `bash`: sandboxed shell.
- `read_file`: paged numbered lines, search, raw byte windows.
- `write_file`: create or replace a file. Makes parent directories.
- `edit_file`: replace an exact literal string. Must be unique, or pass `replace_all`.
- `list_dir`: one directory, non-recursive.
- `spawn_agent`: delegate to a child agent. See [Subagents](#subagents).
- `zvec_grep_search`: zg's own agent search request, loaded when `zg` (zvec-grep) is on `PATH`: `query` and `queries` for hybrid groups, `fts` and `vector` for supplemental routes, `fuse`, a per-group `limit` (default 7, maximum 50), rg-style `globs` and `insensitiveGlobs`, `fileTypes`, `preferSymbol` and `symbolTypes`, `modifiedAfter` and `modifiedBefore`, and `path` to narrow within the workspace. Polly creates the workspace's `.zvec-grep/` index on first use and refreshes it inside each query. New indexes use the local `potion-code-16m-v2` model; the first query may download it. Existing local-model indexes retain their model and file-selection settings. Results are ranked snippets, not exhaustive matches. See [SEARCH.md](SEARCH.md).

If zg is missing, Polly omits `zvec_grep_search`, including when restoring a session
that previously used it. An explicit `--tool zvec_grep_search` reports the missing
dependency. Exact lookups are bash's job, with `grep` or `rg`, with or without zg.

Native file operations enforce the sandbox policy in-process; indexed search
runs zg through the process sandbox and checks cached hits against the current
read policy. It keeps its runtime and model cache in `.zvec-grep/polly/`.
The search root uses the closest ancestor index (`.zvec-grep/manifest.json`)
or Git workspace, otherwise the requested directory; discovery stops short of
the home directory and the filesystem root. Subdirectory searches stay scoped
to that directory.
Automatic indexing requires workspace write access and a local embedding model;
an ancestor whose `.zvec-grep` the write policy cannot reach is skipped, so a
launch from a subdirectory under the default sandbox indexes that subdirectory
(`--writepath <repo-root>` shares one index across launches).
If an existing zg daemon owns index writes, Polly searches the existing snapshot
directly and marks it potentially stale. Other index failures are reported so
the agent can use `grep` or `rg` in bash. See [sandbox details](SANDBOX.md#what-gets-sandboxed).

To verify an installed zg with the real sandbox, run
`POLLYTOOL_REQUIRE_ZG_TESTS=1 go test ./tools -run TestZvecGrepSearchLive -count=1`.
This builds a temporary index and downloads a local model into it.

Conversations also provide recall tools for content omitted from model context:

- `list_artifacts`: catalog stored outputs and images from this conversation.
- `read_artifact`: read or search a stored output, or attach a stored image.
- `read_transcript`: read or search the conversation transcript.

Both readers accept `offset`/`limit` for numbered lines and `query` for literal
search. Use `byte_offset` on its own to continue through a long text line when
a page reports a byte continuation.

### Shell tools

Any executable is a tool if it answers two flags:

- `--schema`: print a JSON Schema
- `--execute <json-args>`: do the work, print the result to stdout, exit 0

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

A top-level `"sandbox"` field in the schema customizes the tool's policy.
See [Sandboxing](#sandboxing).

### MCP servers

Claude Desktop-format JSON. `-t mcp.json` loads every server in the file.
`-t mcp.json#filesystem` loads one.

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

## Skills

[Agent Skills](https://agentskills.io/specification): a folder per skill,
a `SKILL.md` manifest, discovered from one or more directories.

```bash
polly --listskills                          # default: ~/.pollytool/skills
polly --skilldir ~/.pollytool/skills --skilldir ./skills --listskills
polly -S ./my-skill -p "..."                # load one directly (dir, git URL,
                                            # or archive URL); auto-activated
polly --noskills -p "summarize this file"   # skills off for a run
```

Discovered skills are advertised in the system prompt beside the
`activate_skill` and `read_skill_file` tools. Activation loads the
skill's `scripts/` as shell tools and its `mcp/` configs as MCP servers,
namespaced by skill name. Its `allowed-tools` globs apply on later turns,
additively across activations.

## Structured output

A JSON schema in, validated JSON out. Images too:
`polly -f receipt.jpg --schema receipt.schema.json`.

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

Tool commands (the builtin `bash` tool, shell tools, stdio MCP servers)
run **sandboxed by default**.

- Filesystem read-only outside the policy's writable paths.
- Credential paths hidden: `~/.ssh`, `~/.aws`, `~/.gnupg`, ...
- Credential-shaped environment stripped: `POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, `SSH_AUTH_SOCK`, ...

`--sandbox <preset>` (`POLLYTOOL_SANDBOX`) picks the base policy. Join
components with `+`.

| Preset | Meaning |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | no writes at all, not even temp; no network |
| `workspace` | working directory writable; Git metadata read-only |
| `git` | with `workspace`: keep `.git` writable, pin only its dangerous leaves (config, hooks, routing pointers) so commit/rebase/fetch work |
| `net` | outbound network allowed |
| `ssh` | agent-based SSH: `SSH_AUTH_SOCK` and its socket pass through, `~/.ssh/config` and `known_hosts` readable; private keys stay masked |
| `sshkeys` | read all of `~/.ssh` including private keys; still not writable |

Default: **`workspace+net+git`**. Tighten with `--sandbox base` or
`--sandbox readonly` when tools only compute or inspect.

On top of any preset: `--writepath <dir>` and `--denypath <path>` (both
repeatable), `--allownet`. `--nosandbox` turns sandboxing off.

Per tool: a `"sandbox"` field in the shell tool schema or MCP server
entry widens or tightens that tool's policy.

**[SANDBOX.md](SANDBOX.md)**: every `"sandbox"` field, the merge rules,
Git metadata protection, platform details, limitations.

## CLI reference

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
   --maxiterations int                                      Maximum agent iterations (LLM calls) before stopping (default: 1024) [$POLLYTOOL_MAXITERATIONS]
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
   --system string, -s string                               Custom persona (replaces coding defaults and AGENTS.md loading; display and recall guidance is added automatically) [$POLLYTOOL_SYSTEM]
   --file string, -f string [ --file string, -f string ]    File, image, or URL to include (can be specified multiple times)
   --schema string                                          Path to JSON schema file for structured output
   --context string, -c string                              Context name for conversation continuity [$POLLYTOOL_CONTEXT]
   --last, -L                                               Use the last active context
   --flat                                                   With --list, print one line per context instead of nesting agents under the context that spawned them
   --maxcontext int                                         Maximum estimated tokens sent to the model, clamped to the model's advertised context window when discoverable; full history is retained (0 = unlimited, never clamped) (default: 256000)
   --confirm                                                Require confirmation before each tool call (default: false)
   --sandbox string                                         Sandbox preset: base, readonly, workspace, git, net, ssh, sshkeys — join with + (e.g. workspace+net+git+ssh); git requires workspace (default: "workspace+net+git") [$POLLYTOOL_SANDBOX]
   --nosandbox                                              Disable sandboxing of tool commands [$POLLYTOOL_NOSANDBOX]
   --denypath string [ --denypath string ]                  Additional path blocked from sandboxed reads (repeatable, supports ~) [$POLLYTOOL_DENYPATHS]
   --writepath string [ --writepath string ]                Additional path sandboxed tools may write to (repeatable, supports ~) [$POLLYTOOL_WRITEPATHS]
   --allownet                                               Allow sandboxed tools outbound network access [$POLLYTOOL_ALLOWNET]
   --theme string                                           TUI appearance: default or halo (default: "default") [$POLLYTOOL_THEME]
   --activity-details                                       Print bounded thought, tool, agent, and image details at turn end (one-shot only; ignored by the REPL) [$POLLYTOOL_ACTIVITY_DETAILS]
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

## See also

- [Soulshack](https://github.com/pkdindustries/soulshack): an IRC chatbot that uses Polly for LLM features.

## License

MIT
