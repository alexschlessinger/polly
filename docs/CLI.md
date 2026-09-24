# CLI and TUI guide

This guide covers running `polly` from a terminal: setup, one-shot prompts,
sessions, the full-screen TUI, models, tools, skills, and themes. New to Polly?
Start with the [quick start](../README.md#quick-start). The other guides are in
the [documentation index](README.md).

## Contents

- [First run](#first-run)
- [Prompt examples](#prompt-examples)
- [One-shot output](#one-shot-output)
- [Sessions](#sessions)
- [TUI](#tui): [keys](#keys), [commands](#slash-commands), [inspector](#inspector)
- [Files and skills in prompts](#files-and-skills-in-the-composer)
- [Transcript and changes](#transcript)
- [Images](#images)
- [Models](#models)
- [Tools](#tools)
- [Skills](#skills)
- [Themes](#themes)
- [Sandboxing](#sandboxing)

## First run

The first time you launch Polly interactively without a `~/.pollytool/config`,
it opens setup before your first prompt. Setup asks for a provider, model, key,
context limit, endpoint, reasoning effort, theme, and default sandbox. Apply
saves all of it except the key; Escape skips setup and remembers that you did.
Reopen it any time with `/setup` or `polly --setup`.

Without the TUI (a dumb terminal, or `TERM=dumb`), setup asks the same questions
one line at a time. Press Enter to keep the value shown, and end the input to
skip setup.

To skip the questions entirely, name every default on the command line:

```sh
polly --setup --model openai/gpt-5.4 --effort high --theme default --nosandbox
```

That saves the defaults exactly as setup would and exits. Use
`--sandbox <preset>` in place of `--nosandbox` for a sandboxed default. On a
first run, the same flags without `--setup` save the defaults and carry on into
the session.

### API keys

Keys come from `POLLYTOOL_<PROVIDER>KEY`, or from a process-only override in
`/keys`, and they're never saved. Polly needs a key for the selected provider,
except for Ollama and for OpenAI-compatible providers on a custom `--baseurl`.
The [README](../README.md#models) lists the providers and their variable names.

### Configuration

Every other default lives in `~/.pollytool/config`, one `POLLYTOOL_*` setting per
line:

```text
POLLYTOOL_MODEL=openai/gpt-5.4
POLLYTOOL_EFFORT=high
```

**Precedence runs flags → environment → config file.** Environment and config
values seed new sessions, but a resumed session changes only when you pass an
explicit flag. Setup warns you when an exported variable is overriding a saved
value.

Changing the sandbox in setup affects later launches. The running process keeps
the policy it started with.

## Prompt examples

```sh
polly -p "Explain this repository"
polly ask "Hello?"                       # alias for -p
polly --ask "Hello?"                     # another alias
cat notes.txt | polly
git diff | polly ask "Explain this change" # piped input attached to the prompt
polly -f image.jpg -f https://example.com/chart.png -p "Compare these"
polly -p "uppercase this" -t ./uppercase.sh -t filesystem.json
```

Piped stdin is the prompt when none is given. With `-p` or `ask` it is attached
after the prompt as `stdin`, so the model reads the prompt as the instruction.
`-f` takes local image paths and image URLs. `-t` picks a tool set and replaces
the defaults, so repeat it for each tool you want.

## One-shot output

The settled answer goes to stdout. Live activity goes to stderr and clears
itself when the turn ends. Redirected stdout gets raw Markdown, and activity is
left out entirely when stderr is redirected or `TERM=dumb`.

Activity is plain text and ignores the theme. The live line names what the
turn is doing, counts tools and agents, and ticks its elapsed time once a
second; when the terminal is narrow it drops fields rather than wrapping. The
settled summary lists thought time, tool and agent counts, elapsed time,
tokens, cache hits, and cost. Only failed tool calls, warnings, and a failed
turn are marked with `✗`, and those are the only lines colored (red);
`NO_COLOR` turns that off too.

| Flag | Effect |
|---|---|
| `--stream` | Emit text as it arrives |
| `--quiet` | Hide activity |
| `--activity-details` | Print bounded thought, tool, agent, and image summaries at turn end |
| `--meta` | Emit a `polly-meta` record |

A turn that hits a token or iteration cap exits with an incomplete-turn status.

### Structured output

Pass a JSON Schema and stdout becomes validated JSON. Image attachments work
too.

```sh
polly --schema person.schema.json -p "Extract a person from this text"
```

## Sessions

A session is a saved conversation; flags call it a *context*
(`-c`, `--context`). TUI sessions save automatically, while one-shot runs are
stateless unless you pass `-c`. Sessions with generated handles like
`quiet-otter` expire after seven idle days. Named sessions never expire, and
empty ones are discarded on exit.

| Command | Effect |
|---|---|
| `polly -c project` | Open a named session |
| `polly -L` | Resume the last active session |
| `polly --create project --model openai/gpt-5.4` | Create a session with explicit settings |
| `polly -c project -p "Continue"` | Continue it with a one-shot prompt |
| `cat notes.txt \| polly -c project --add` | Add context without a model call |
| `polly --show project` | Show its settings |
| `polly --reset project` | Clear its history |
| `polly --delete project` | Remove it |
| `polly --export project > fixture.json` | Export it and its agents as a [shot fixture](SCREENSHOTS.md#fixtures-seeded-sessions-and-scripted-turns) |
| `polly --list --flat` | List saved sessions |
| `polly --purge` | Delete every session, after confirmation |

A session keeps its own settings, and changing its system prompt resets its
history. Unless you pass `--system`, the CLI adds coding guidance and loads the
`AGENTS.md` files on the path from the Git root to the working directory, up to
32 KiB per file and 64 KiB in total.

Sessions are stored in `~/.pollytool/polly.db`. To back up the database while
Polly is running, use SQLite's [online backup API](https://www.sqlite.org/backup.html).

## TUI

Run `polly` without `-p` or piped stdin to get the full-screen interface;
`TERM=dumb` or a redirect selects the line frontend instead. Markdown tables
wrap to fit the pane, and in narrow panes they turn into labeled fields.

### Titles and the status bar

Polly titles a session on its own once its purpose is clear. Set a title
yourself with `/title <text>` (or F2 in the session picker) and Polly will leave
it alone. `/rename <name>` changes the session's handle.

The status bar doubles as a row of shortcuts. Click a field to open it:

| Field | Opens |
|---|---|
| Session name | Saved-session picker |
| Model name | Model form |
| Context usage, such as `41.2k/156k` | Message counts, token estimates, and request budget |
| Changes, such as `+100 −20` | Net workspace diff |
| Agents | Running work, pending decisions, and collapsed finished history |

A `~` anywhere means an estimate. Each turn's row counts tokens as the response
streams in and, when the cost is knowable, ends with it: exact when the provider
bills it (OpenRouter), estimated from the model's advertised rates otherwise.
The status bar totals what the session has spent since this Polly opened it,
swarm members included. `--meta` adds `cost_usd`, plus `cost_estimated=true`
when the cost wasn't billed.

A session open in another running Polly is unavailable in the picker.

### Open sessions

Several sessions can be open at once, but only one fills the screen. `/new`
opens a fresh session, and `/close` closes the visible one while keeping it
saved (though not during a running turn). `/resume` or Ctrl-G picks a saved
root session. Alt+1…9 jumps straight to an open session, and Alt+] and Alt+[
step to the next and previous one.

Every open session has its own settings. Hidden sessions keep running and queue
your input, then post a single notice when they finish. Parent links open the
parent conversation.

### Keys

| Key | Action |
|---|---|
| `Enter` | Send; accept a completion first if one is open |
| `Tab` | Switch focus when an inspector is open; otherwise accept a completion |
| `Ctrl-C` | Warn, then press again to cancel; again while canceling, quit |
| `Esc` | Dismiss a completion, dialog, search, or inspector; otherwise press twice to cancel |
| `Ctrl-R` | Search input history |
| `Ctrl-G` | Pick a saved session |
| `Ctrl-O` | Expand or collapse all inline details |
| `Ctrl-V` | Attach a clipboard image |
| `Ctrl-Z` | Suspend |
| `Shift` + drag | Select text |
| Arrow keys, `PgUp/PgDn`, `Home/End` | Navigate the focused inspector; otherwise edit and browse history |

Canceling a turn takes two presses of the same key in a row, so a stray `Esc`
only shows a warning. Anything you send mid-turn queues up for later, and input
that fails to send comes back as a draft.

#### Inspector navigation

In the Tools and Changes inspectors, `Tab` moves focus between the composer and
the list. `Up/Down` picks a tool or file, `Enter` toggles its details, and
`Left/Right` collapse and expand it. `PgUp/PgDn` scrolls long output, and
`Ctrl-O` toggles every row.

In Agents, `Up/Down` picks an agent and `Enter` or `Right` opens it; `Left` takes
you from an agent's conversation back to the list. In Thoughts, `Left/Right`
steps between thought blocks (not between tools), and in Swarm it switches
sections. Conversation, Thought, and Swarm views scroll with `Up/Down`. Every
inspector supports `PgUp/PgDn` and `Home/End`, and `Backspace` goes back to its
parent.

Buttons and links are reachable from the keyboard too. In a focused inspector,
`Shift-Tab` selects a visible one and underlines it. `Left/Right` or another
`Shift-Tab` moves between actions, and `Enter` activates the selection.
`Up/Down` drops back to normal navigation, and `Tab` returns to the composer.

### Slash commands

`/help [command]` has the full syntax for everything. In the TUI it opens a
compact reference dialog: type to filter, scroll with the arrows or Page
Up/Down, and press Esc to close it.

| Task | Commands |
|---|---|
| Model and defaults | `/model`, `/keys`, `/setup`, `/effort [value]`, `/set [key [value]]` |
| Conversation | `/sessions`, `/resume`, `/new`, `/close`, `/title`, `/rename` |
| Context and files | `/context`, `/attach <path>`, `/add-dir [path]` |
| Display | `/inspect`, `/theme [name]`, `/clear`, `/screenshot [path]` |
| Tools and agents | `/tools`, `/spawn`, `/workflow` |
| Sandbox | `/sandbox`, `/sandbox-init [notes]` |
| Reset and exit | `/reset confirm`, `/exit` |

`/keys` changes only the running process, while `/setup` saves your non-key
defaults. `/tools list [namespace]` and `/tools show <name>` inspect tools, and
`/tools restart <server>` restarts a stdio MCP server.

`/screenshot` saves a PNG of the screen as Polly renders it, images included,
to `polly-screenshot.png` in the system temp directory unless you give a path.

### Inspector

Click an expanded tool, thought, or agent row to open the inspector, or use
`/inspect [tools|thoughts|changes|find|maximize]`. For an inspected agent,
**Stop agent** cancels it and keeps it stopped until you choose **Resume agent**.
Parent follow-ups cannot clear your stop.
**Review** answers its approval request.

At 120 columns or wider, the inspector opens as a draggable 70/30 split;
anything narrower gets the full width. Focus follows the pointer, Tab switches
between the composer and the inspector without touching your draft, and Escape
or a click outside closes it.

### Subagents

Ask Polly to delegate, or use `/spawn [--read-only] [--review] <brief>`.
Research agents return findings, and editing agents work in isolated Git
snapshots (Git 2.40 or later) that the parent integrates; they never commit
anything themselves. Research is done once it's delivered, unless `--review`
makes it wait for your acceptance. Write briefs with repository-relative paths.

Live agent rows show the current phase, elapsed time, and time since provider
data last arrived. Open the agent to see the current model request's silence
timeout and total deadline, remaining time, and output count. `≈` marks a rough
estimate from streamed text and reasoning; provider-reported counts replace it
when available. The inspector also identifies who last resumed the agent.
Activity is available while this Polly process owns the running agent.

A resumed session redraws only its last five prompts, so reach agents launched
earlier through `/sessions`. By default, a run allows 32 concurrent executions
and 256 starts; change these with `--swarm-concurrent` and
`--swarm-executions`. Quitting pauses unfinished work.

[Swarms and workflows](WORKFLOWS.md) covers tools, follow-ups, review,
integration, and recovery.

## Files and skills in the composer

Type `@` to search for files, or start with `/` for commands and skills. Arrow
keys select, Tab or Enter inserts, and Escape dismisses. Fully typed references
work too:

```text
/polly-tui inspect @cmd/polly/repl_composer.go
compare @"notes/design draft.md" with @README.md
```

Here's how the composer reads what you type:

| Input | Behavior |
|---|---|
| Bare unresolved `@word` | Literal text |
| Quoted reference or explicit `@./path` | Missing files are errors |
| Plain typed path | Literal text |
| Backticks, fenced code, `\@`, or `\/` | Literal reference syntax |
| `/name` at the beginning | Activate a skill |
| `/skill name` anywhere | Activate a skill, even when a command has the same name |
| File drop or paste containing only existing paths | Attach as references |
| Mixed prose and code, or an unsupported batch of paths | Keep the pasted text and report unsupported attachments |

Attachment contents are captured when you send, before the prompt queues; if
that fails, your draft stays put. Text files must be UTF-8, with at most 256 KiB
each, 1 MiB combined, and 32 files per prompt. Directories, PDFs, and other
binary files aren't supported. Images have their own limits, described
[below](#images).

Completion honors nested `.gitignore` rules and skips `.git`. An explicit path
can reach ignored or external files the policy allows, but a reference never
grants access on its own. Click a submitted prompt to see the attachment
contents it saved.

CLI and line-frontend prompts treat `@` and `/` literally.

## Transcript

Thoughts, tool batches, agents, and images start out as compact disclosure
rows. Click a row's triangle to open it, or press Ctrl-O to open or close them
all; while everything is open, new blocks arrive open too.

### Tools and changes

Tool previews keep the status, timing, relative paths, and read ranges in view.
`$` marks a Bash command, and `…` marks folded setup (such as a leading `cd`) or
omitted text. The tool inspector lists calls oldest first, with their arguments,
diffs, output, and images.

File edits show counts like `+3 −1` or `new +12`. The Changes inspector compares
the workspace against a baseline of tracked files taken when it first opens:

- Tracked edits that were already there are part of the baseline.
- Every non-ignored untracked file shows up as an addition, even one that
  existed before.
- Repeated edits collapse into one net diff, and reverting an edit removes it.
- Changes by other writers are included. The report refreshes after tools and
  turns, and whenever you open it.
- Outside Git, Bash changes aren't tracked.

The baseline survives reopening the session and resetting the transcript.

### Thoughts and agents

Thought rows show live timing. An interrupted turn keeps the iterations and
results it had completed.

Expanded workflow rows highlight agents that are busy, paused, or waiting on a
decision, and tuck finished ones behind a collapsed count. "Finished" means an
agent's latest run ended, not that every task was accepted or integrated.

## Images

Images in the assistant's Markdown and image media returned by tools render
inline. A tool result containing only a path or Markdown image syntax stays
text.

| Protocol | Terminals |
|---|---|
| Kitty graphics | Kitty, Ghostty, WezTerm |
| Sixel | Terminals with Sixel support, including Windows Terminal 1.22+ and foot |
| Neither | Images appear as a caption |

Set `POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none` to override detection.

Ctrl-V and `/attach` insert image tokens into the composer. Dropped files and
`@path` references become composer attachments, while a bare path stays text for
the model to open with `view_image`.

A prompt can carry 16 images and a request 100, at up to 10 MB each and 16 MiB
in total. Images are scaled to at most 1568 pixels on the longest edge. A GIF
uses its first frame, and GIF and BMP images are converted to PNG.

## Models

Choose a model as `provider/model` with `-m`, `POLLYTOOL_MODEL`, or `/model`; the
[README](../README.md#models) lists provider prefixes and keys. `--baseurl` sets
the inference and metadata endpoint for OpenAI-compatible providers and Ollama.
Native Anthropic and Gemini use their own endpoints.

### Model form

Click the model in the status bar or run `/model` to open the model form.
`/keys` opens it with the masked key field focused, and `/setup` adds fields for
the endpoint, effort, theme, and sandbox defaults.

- Up/Down moves between fields, and Left/Right cycles providers.
- Tab completes a model name and cycles through matches; Shift-Tab goes
  backward, and typing starts a fresh cycle. Tab doesn't move between fields.
- Enter advances or applies, and Escape discards the draft. Fields take clicks
  too.
- Prices are the advertised input, output, and cached rates per million tokens.
  A pinned host shows that host's prices, and unknown prices show as dashes or
  stay hidden.

### Discovery and completion

Suggestions include only models that advertise text and tool support, but a
name you type by hand always works, even when discovery fails. Catalogs are
cached for an hour (keys are never stored in the cache), and Ctrl-R refreshes
them.

### Host pinning

For OpenRouter and Hugging Face, choosing a discovered `model:host` pins
requests to that host, while a bare model name uses Automatic routing. An exact
catalog ID wins over route syntax, unknown suffixes stay literal, and Ollama's
`model:tag` names are left intact.

OpenRouter also takes `--modelhost` or `/set modelhost`, and
`/set modelhost automatic` clears it. Sessions keep their pins, and children
inherit them unless they select a different model or route.

### Key overrides

A key override applies to its provider in every open session of the running process, and
is never written anywhere. Ctrl-U clears the field so that Apply goes back to
the environment key, and Escape leaves the active key as it was.

### Context limit

The Context field takes a number, `auto`, or `0` for unlimited. The status bar
shows the resolved limit before output headroom; click it after a request to see
the input budget, response reserve, and safety margin.

New sessions use the detected capacity, leaving headroom for output, and fall
back to 256,000 tokens. Any positive budget, whether explicit, saved, or
inherited, is clamped to the detected window. `/set maxcontext auto` goes back
to detection, and `--maxcontext 0` opts out. For Ollama, the model's capacity
and the runtime context are separate limits.

### Thinking on OpenRouter

`/effort` (or `/set effort`) shows both your preference and the setting in
effect, such as `off → low (required)`. `/effort <value>` saves a new
preference for the session, and completion offers only the values the current
model supports. Your saved preference survives when Polly has to adapt it:

| Situation | Effective behavior |
|---|---|
| Required thinking with a known minimum | Use that minimum |
| Required minimum unknown | Use the provider default and report the fallback |
| Optional thinking, preference `off` | Disable thinking |
| Thinking policy unknown, preference `off` | Use the provider default and label it unknown |
| `dynamic` | Provider default |
| Model cannot reason | Omit effort; refuse a named effort when editing |
| Unsupported newly selected effort | Reject it and list the valid choices |
| Saved effort unsupported after a model switch | Use the provider default and report the adaptation |

OpenRouter's accepted values come from model metadata, and native providers may
clamp levels differently. Reasoning is replayed only to the gateway and model
that produced it.

### Capability adaptation

When a model's metadata explicitly says it lacks a feature, Polly adapts the
outgoing request. It can leave out unsupported optional tools or temperature,
replace images with explanatory text, or turn completed tool exchanges into
text. Your original messages and saved settings are untouched. An incompatible
*required* response tool or schema is an error instead. The
[Go API guide](API.md#providers) covers the details.

## Tools

### Built-in tools

The [README](../README.md#built-in-tools) lists the default set. A parent session
also gets coordination tools, and setup tools exist only during `/sandbox-init`.
`--confirm` asks you to approve each call, and any `--tool` selection replaces
the defaults.

`read_artifact` pages through and searches conversation artifacts, stored images
included, along with swarm evidence that was explicitly published.

### Bash

Each Bash call runs `bash -c` in a fresh shell, so `cd`, exports, and shell
options don't carry over between calls. A pipeline reports its last command's
status; workflow `exec` adds `pipefail`, but ordinary Bash calls don't. Bash
diffs in the TUI come from private Git snapshots and never touch the
repository's own index.

### Shell tools

A shell tool is any executable that prints its JSON Schema for `--schema` and
its result for `--execute <json-args>`, exiting 0:

```bash
#!/bin/bash
case "$1" in
  --schema) echo '{"title":"uppercase","description":"Uppercase text","type":"object","properties":{"text":{"type":"string"}},"required":["text"]}' ;;
  --execute) jq -r .text <<<"$2" | tr a-z A-Z ;;
esac
```

Load it with `-t ./uppercase.sh`. A top-level `sandbox` field in the schema
customizes the policy its process runs under. Schema discovery itself runs
under a separate, restricted policy.

### MCP servers

Load a Claude Desktop-format JSON config with `-t mcp.json`, or a single server
from it with `-t mcp.json#server`. Tools are namespaced as `server__tool`.

Stdio servers run under the base sandbox plus the server's own declaration;
workspace-profile layers don't reach them. SSE and streamable HTTP configs take
`transport`, `url`, `headers`, and `timeout`.

## Skills

A skill is a folder with a `SKILL.md`, following the
[Agent Skills specification](https://agentskills.io/specification).

| Option | Effect |
|---|---|
| `--skilldir` | Search directory; default `~/.pollytool/skills` |
| `--listskills` | List available skills |
| `-S <dir\|git\|url>` | Load and activate a skill |
| `--noskills` | Disable skills |

Activating a skill loads the JSON servers in its `mcp/` directory and exposes
its `scripts/` paths to Bash. Once active, a skill's instructions stay in the
session. A skill that names a `command:` in its frontmatter, as `sandbox-setup`
does with `/sandbox-init`, is offered through that command instead.

Built-in skills sync to `~/.pollytool/builtin-skills`, and a user skill with the
same name wins. See [feature workflows](WORKFLOWS.md#running-javascript-workflows)
and [build setup](SANDBOX.md#build-setup) for the two larger ones.

## Themes

`/theme` opens a preview picker, and `/theme <name>` switches straight away.
Escape cancels a preview; committing a choice saves `POLLYTOOL_THEME` to
`~/.pollytool/config`. At launch, `--theme` or `POLLYTOOL_THEME` picks the theme.

### Theme files

```json
{
  "name": "solarized-ish",
  "colors": { "accent": "#268bd2", "muted": "palette:8" }
}
```

Save your themes as `~/.pollytool/themes/<name>.json`. A name containing `/` or
ending in `.json` is treated as a path. Any other name looks for a user file
first, which shadows a built-in preset of the same name. `default` is reserved.
If you want different light and dark looks, write two themes.

The default theme follows your terminal's ANSI palette and surface colors. The
four parrot presets (`amber-parrot`, `azure-parrot`, `midnight-parrot`, and
`verdant-parrot`) are written into the themes directory at startup if they're
missing, so you can copy one as a starting point.

### Roles

| Group | Names | Unset behavior |
|---|---|---|
| Semantic | `ok`, `err`, `run`, `accent`, `active`, `muted`, `code` | Built-in color |
| Syntax | `syn-comment`, `syn-keyword`, `syn-string`, `syn-number`, `syn-func`, `syn-add`, `syn-del` | Corresponding semantic color |
| Bird | `polly-green`, `polly-light`, `polly-wing`, `polly-crown`, `polly-beak`, `polly-mouth`, `polly-face`, `polly-eye`, `polly-foot` | Built-in bird color |
| Surface | `background`, `foreground` | Terminal default |

The syntax roles fall back, in table order, to `muted`, `accent`, `ok`, `active`,
`code`, `ok`, and `err`. Set the surface foreground and background together;
they're painted only in the full-screen TUI.

### Values

A color can be `#rrggbb`, `#rgb`, `palette:0` through `palette:255`, a
recognized color name, or `inherit` (except for `accent` and `muted`). Leave a
role out to get its built-in fallback. Invalid values are errors, and a theme
that fails to load falls back to `default` with a notice.

### Applying a theme

The TUI reloads the active theme within a second of an edit; a half-written
file keeps the previous theme until it's valid again. The line frontend can't
switch or reload themes interactively.

When the model designs a theme, its `set_theme` tool previews it in memory.
Saving takes a second, confirming call after the first returns the exact path
and colors for review. Sandboxed tools can't write the theme directory, so
`set_theme` is the only way for the model to save a theme.

`NO_COLOR`, `TERM=dumb`, or redirected stdout turn off color in the line
frontend's answer, and `NO_COLOR` also makes the TUI monochrome. One-shot
activity on stderr never uses the theme.

## Sandboxing

Sandboxing is opt-in. `polly --sandbox default` starts you with workspace
access, network, and protected Git writes, and adding `private-home` hides the
home paths you haven't granted. A saved workspace profile or `--add-dir` on its
own doesn't turn a sandbox on.

| Command | Purpose |
|---|---|
| `/sandbox-init [notes]` | Prepare builds, verify commands, and update `AGENTS.md` |
| `/sandbox [show]` | Inspect profile settings |
| `/sandbox try [command]` | Diagnose access failures and review grants |
| `/sandbox allow …`, `/sandbox forget …` | Manage explicit exceptions |
| `/sandbox storage` | Inspect owned storage |
| `/sandbox clean caches` | Clear tracked caches |
| `/sandbox reset environment` | Also clear restorable state, keeping configuration |

[Sandboxing](SANDBOX.md) covers defaults, permission rules, platform
differences, and exactly what cleanup keeps.

## CLI reference

`polly --help` lists every flag along with its `POLLYTOOL_*` environment
equivalent. The headless capture flags, `--shot-script <file|->`,
`--shot-size <WxH>`, and `--shot-fixture <file>`, are covered in
[Headless screenshots](SCREENSHOTS.md).
