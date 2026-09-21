# CLI and TUI guide

Run `polly` for the TUI, or use `-p` / piped stdin for a single turn.
[Quick start](../README.md#quick-start) · [Documentation index](README.md)

## Contents

- [First run](#first-run)
- [Prompt examples](#prompt-examples)
- [One-shot output](#one-shot-output)
- [Sessions and settings](#contexts)
- [TUI](#tui): [keys](#keys), [commands](#slash-commands), [inspector](#inspector)
- [Headless screenshots](#headless-screenshots)
- [Files and skills in prompts](#files-and-skills-in-the-composer)
- [Transcript and changes](#transcript)
- [Images](#images)
- [Models](#models)
- [Tools](#tools)
- [Skills](#skills)
- [Themes](#themes)
- [Sandboxing](#sandboxing)

## First run

Without `~/.pollytool/config`, the first TUI launch opens setup before your first
prompt. Choose a provider, model, key, context limit, endpoint, reasoning effort,
theme, and sandbox default. Apply saves everything except the key. Escape skips
setup and records the skip. Reopen it with `/setup` or `polly --setup`.

Keys come from `POLLYTOOL_<PROVIDER>KEY` or a process-only override in `/keys`.
Polly requires one for the selected provider, except Ollama and OpenAI-compatible
providers with custom `--baseurl` endpoints. Keys are never saved. The provider list and variable names
are in the [README](../README.md#models).

Other defaults use one `POLLYTOOL_*` setting per line:

```text
POLLYTOOL_MODEL=openai/gpt-5.4
POLLYTOOL_EFFORT=high
```

**Precedence: flags → environment → config file.** Environment/config values
seed new sessions; only explicit flags change a resumed session. Setup warns
when an exported variable overrides a saved value.

The sandbox field changes the default for later launches. The current process
keeps the policy it started with.

## Prompt examples

```sh
polly -p "Explain this repository"
polly ask "Hello?"                       # alias for -p
polly --ask "Hello?"                     # another alias
cat notes.txt | polly
polly -f image.jpg -f https://example.com/chart.png -p "Compare these"
polly -p "uppercase this" -t ./uppercase.sh -t filesystem.json
```

`-f` accepts local image paths and image URLs. `-t` selects a tool set and replaces
the defaults; repeat it to include multiple tools.

## One-shot output

The settled answer goes to stdout; live activity goes to stderr and disappears
when the turn ends. Redirected stdout is raw Markdown. Activity is omitted when
stderr is redirected or `TERM=dumb`.

| Flag | Effect |
|---|---|
| `--stream` | Emit text as it arrives |
| `--quiet` | Hide activity |
| `--activity-details` | Print bounded thought/tool/agent/image summaries at turn end |
| `--meta` | Emit a `polly-meta` record |

Token and iteration caps return an incomplete-turn exit status. If swarm
settlement reopens an answer, stdout prints the successive answer blocks in order.
Structured output keeps one validated final document.

### Structured output

```sh
polly --schema person.schema.json -p "Extract a person from this text"
```

The result is validated JSON on stdout. Image attachments also work.

## Contexts

A context is a saved conversation. TUI sessions save automatically; one-shot
runs are stateless unless you use `-c`. Generated handles such as `quiet-otter`
expire after seven idle days. Named sessions never expire; empty sessions are
discarded on exit.

| Command | Effect |
|---|---|
| `polly -c project` | Open a named session |
| `polly -L` | Resume the last active session |
| `polly --create project --model openai/gpt-5.4` | Create with explicit settings |
| `polly -c project -p "Continue"` | Continue with a one-shot prompt |
| `cat notes.txt \| polly -c project --add` | Add context without a model call |
| `polly --show project` | Show settings |
| `polly --reset project` | Clear history |
| `polly --delete project` | Remove the session |
| `polly --list --flat` | List saved sessions |
| `polly --purge` | Delete all sessions after confirmation |

Settings persist with the context. Changing its system prompt resets history.
Without `--system`, the CLI adds coding guidance and loads `AGENTS.md` from the
Git root to the working directory, with limits of 32 KiB per file and 64 KiB total.
Structured output omits that coding guidance; the effective sandbox summary still
reaches the model.

Storage is `~/.pollytool/polly.db`. Use SQLite's
[online backup API](https://www.sqlite.org/backup.html) for a live database backup.

## TUI

The full-screen interface runs without `-p` or piped stdin. `TERM=dumb` and
redirects select the line frontend. Markdown tables wrap to the pane width and
become labeled fields in narrow panes.

### Sessions

Polly titles a session once its purpose is clear. `/title <text>` or F2 in the
picker protects a manual title; `/rename <name>` changes its handle.

Click a status field to inspect it:

| Field | Opens |
|---|---|
| Session name | Saved-session picker |
| Model name | Model form |
| Context usage, such as `41.2k/156k` | Message counts, token estimates, and request budget |
| Changes, such as `+100 −20` | Net workspace diff |
| Agents | Running work, pending decisions, and collapsed finished history |

`~` marks estimated context use. Session estimates include generated guidance,
but exclude tool-definition overhead and describe the untrimmed session.
Cache hit rates use cached input divided by total input; turn rates appear only
when every measured request reports cache usage.

Sessions are leased: another running Polly's session is unavailable in the picker.
A child whose parent is leased elsewhere can show a read-only parent snapshot.

### Tabs

`/new` opens a fresh tab; `/close` closes the visible tab while preserving its
session and refuses during a running turn. `/resume` or Ctrl-G picks a saved root
session. Alt+1…9 jumps to a tab; Alt+] and Alt+[ move next/previous.

Each tab has its own settings. Hidden tabs keep running, queue input, and post
one completion notice. Parent links open the parent conversation.

### Keys

| Key | Action |
|---|---|
| `Enter` | Send; accept a completion first if one is open |
| `Tab` | Accept completion; from an empty composer, focus the inspector |
| `Ctrl-C` | Interrupt the root turn; again, or idle, quit |
| `Esc` | Dismiss completion/dialog/search, close inspector, then interrupt, in that order |
| `Ctrl-R` | Search input history |
| `Ctrl-G` | Pick a saved session |
| `Ctrl-O` | Expand/collapse all inline details |
| `Ctrl-V` | Attach a clipboard image |
| `Ctrl-Z` | Suspend |
| `Shift` + drag | Select text |
| Arrow keys, `PgUp/PgDn`, `Home/End` | Navigate the focused inspector; otherwise edit/history |

Input sent mid-turn queues. Failed input returns as a draft. Left/Right switches
thoughts in the thought inspector; it does not navigate between tools.

### Slash commands

Use `/help [command]` for full syntax.

| Task | Commands |
|---|---|
| Model and defaults | `/model`, `/keys`, `/setup`, `/effort [value]`, `/set [key [value]]` |
| Conversation | `/sessions`, `/resume`, `/new`, `/close`, `/title`, `/rename` |
| Context and files | `/context`, `/attach <path>`, `/add-dir [path]` |
| Display | `/inspect`, `/theme [name]`, `/clear`, `/screenshot [path]` |
| Tools and agents | `/tools`, `/spawn`, `/workflow` |
| Sandbox | `/sandbox`, `/sandbox-init [notes]` |
| Reset/exit | `/reset confirm`, `/exit` |

`/keys` changes only the running process. `/setup` saves non-key defaults.
`/tools list [namespace]`, `/tools show <name>`, and `/tools restart <server>`
inspect tools or restart a stdio MCP server. `/screenshot` writes a PNG of the
screen as polly renders it — the frame painted after the command, so neither the
typed command nor the notice it prints is in the image — and defaults its path
to `polly-screenshot.png` in the system temp directory. Images polly placed are
painted into it at the cells they cover, so a thumbnail appears where the
terminal would have drawn it; what the terminal does with those pixels itself —
its scaling, its palette, the hardware cursor — does not.

### Inspector

Click an expanded tool, thought, or agent row, or use
`/inspect [tools|thoughts|changes|find|maximize]`. Stop cancels the inspected
agent; Review answers its approval. The Agents list preserves its scroll position
when you return from a conversation.

At 120+ columns the inspector starts as a draggable 70/30 split; below that it
uses the full width. Focus follows the pointer. Tab from an empty composer enters
the inspector; Escape returns. A click outside dismisses a dialog or inspector
without activating what is behind it. Agent/Changes fields and inspector links
can retarget an open inspector.

### Subagents

Ask for delegation or use `/spawn [--read-only] [--review] <brief>`. Research
returns findings; editing uses isolated Git snapshots for parent integration.
Use repository-relative paths in briefs. Git 2.40+ is required for snapshots.

Ordinary research finishes on durable delivery. `--review` requires explicit
acceptance. Child labels describe their purpose and become initial titles.
Editing agents make no commits; integration applies their captured revisions.

Defaults are 32 concurrent executions and 256 starts per run, adjustable through
`--swarm-concurrent` and `--swarm-executions`. Children inherit the iteration limit.
Quitting pauses unfinished work. The Agents inspector remains available, but
`/swarm` and its subcommands are currently disabled.

See [workflows](WORKFLOWS.md) for tools, follow-ups, review, integration, and recovery.

## Headless screenshots

`polly --shot-script <file>` runs the same TUI with no terminal at all: it paints
on an off-screen screen of `--shot-size` (default `120x40`) and plays a script of
typed input, keys, and captures. Each `:shot` writes a PNG of the frame polly
rendered — exact theme colors, no terminal capture, no screen-recording
permission, no external converter — and prints its path on stdout, one per line.
A run that cannot take the input it was given fails with the script line that
did, rather than capturing something else.

A scenario that needs no model call (layout, colors, keys, commands) is free and
deterministic:

```text
# scenario.txt
:shot $POLLY_SHOT_DIR/splash.png
/help
:wait "Navigate" 5
:shot $POLLY_SHOT_DIR/help.png
:size 160x50
:settle
:shot $POLLY_SHOT_DIR/help-wide.png
```

```bash
POLLY_SHOT_DIR=/tmp/shots polly --shot-script scenario.txt
```

Script lines, one step each; blank lines and `#` comments are skipped:

| Step | Meaning |
|---|---|
| `<text>` | Type the text into the composer and submit it |
| `:submit <text>` | The same, for text that starts with `:` |
| `:type <text>` | Type text without submitting (multi-line needs `:key c-j`) |
| `:key <name>` | One key: `enter`, `esc`, `tab`, `up`, `down`, `left`, `right`, `pgup`, `pgdn`, `home`, `end`, `insert`, `delete`, `backspace`, `space`, `c-a`…`c-z` |
| `:shot <path>` | Write a PNG of the current frame; the path is `~`- and `$VAR`-expanded |
| `:size WxH` | Resize the virtual terminal and re-lay out the frame |
| `:wait <pattern> [sec]` | Wait until the screen contains the pattern (quote a pattern that ends in a number) |
| `:settle [sec]` | Wait until two reads of the screen agree |
| `:ready [sec]` | Wait until input would run rather than queue |
| `:sleep <ms>` | Wait |
| `:quit` | End the run here |

Headless text and screenshots read decoded terminal output, including the
colors and styles sent by the renderer. Scripted keys keep the same UI behavior
as interactive input.

A capture is the frame painted *after* the step before it, so a `:shot` never
contains the step that asked for it. The first typed line waits for the startup
workspace baseline so it runs instead of queueing; later input queues exactly as
it would for a fast typist, which is what `:wait` and `:settle` are for. A
headless run has native graphics on, so the images a frame places — the masthead
logo, a thumbnail — are painted into the capture at the cells they cover, at one
pixel per screen pixel. What the terminal would then do with those pixels (kitty
scaling, sixel quantization, the hardware cursor) is not reproduced.

## Files and skills in the composer

Type `@` to search files, or start with `/` for commands and skills. Arrow keys
select; Tab/Enter inserts; Escape dismisses. Fully typed references work too:

```text
/polly-tui inspect @cmd/polly/repl_composer.go
compare @"notes/design draft.md" with @README.md
```

| Input | Behavior |
|---|---|
| Bare unresolved `@word` | Literal text |
| Quoted reference or explicit `@./path` | Missing files are errors |
| Plain typed path | Literal text |
| Backticks, fenced code, `\@`, or `\/` | Literal reference syntax |
| `/name` at the beginning | Activate a skill |
| `/skill name` anywhere | Activate a skill, including a command-name collision |
| File drop or paste containing only existing paths | Attach as references |
| Mixed prose/code or unsupported path batch | Keep pasted text; report unsupported attachments |

Sending captures attachment contents before queueing; failure keeps the draft.
Text files must be UTF-8: at most 256 KiB each, 1 MiB combined, and 32 files per prompt.
Directories, PDFs, and other binary files are unsupported. Images have separate
limits below.

Completion honors nested `.gitignore` rules and excludes `.git`. Explicit paths
can include ignored/external files allowed by policy; a reference grants no access.
Drops without terminal paste markers remain ordinary input.

Click a submitted attachment's prompt to inspect saved contents. Restored drafts
retain those bytes even if the source changes; remove and reattach to refresh.
Queued skills activate for their own turn, not the running turn. A skill-only
prompt also starts a turn. CLI and line-frontend prompts treat `@` and `/` literally.

## Transcript

Thoughts, tool batches, agents, and images start as compact disclosure rows.
Click their triangles independently, or Ctrl-O to open/close all. When all are
open, newly arriving blocks open too. Display state survives reload.

### Tools and changes

Tool previews keep status, timing, relative paths, and read ranges visible.
`$` marks Bash commands; `…` marks folded setup or omitted text. The tool inspector
shows calls oldest first, with arguments, diffs, output, and images loaded when
opened. Each call expands independently; scroll position survives resizing.
Leading unambiguous `cd`/`export` setup can fold; ambiguous shell syntax stays visible.

File edits show counts such as `+3 −1` or `new +12`. The Changes inspector compares
the workspace with its tracked-file baseline from first open:

- Pre-existing tracked edits are part of the baseline.
- All non-ignored untracked files appear as additions, even pre-existing ones.
- Repeated edits produce one net diff; reverting removes it.
- Reports include changes by other writers and refresh after tools/turns or when opened.
- Missing bodies and unknown counts are labeled. Outside Git, Bash changes are not tracked.

The TUI accepts typing while the initial baseline loads; work that can change
files waits for it. The baseline and last report survive reopening and transcript
reset.

### Thoughts and agents

Thought rows show live timing; reasoning effort defaults to `high` and can be
changed with `--effort`. Interrupted turns keep completed iterations and results.

Expanded workflow rows emphasize busy, paused, and decision-needed agents, with
finished agents behind a collapsed count. “Finished” means the latest run ended,
not that every task was accepted or integrated.

## Images

Assistant Markdown images and tool-returned image media render inline. A tool
result containing only a path or Markdown syntax stays text.

| Terminal capability | Rendering |
|---|---|
| Kitty graphics | Kitty, Ghostty, WezTerm |
| Sixel | Supported Sixel terminals, including Windows Terminal 1.22+ and foot |
| Neither | Caption |

Set `POLLYTOOL_IMAGE_PROTOCOL=kitty|sixel|none` to override detection.
Ctrl-V and `/attach` insert image tokens. File drops and `@path` use composer
attachments; a bare path stays text for the model to inspect with `view_image`.

Limits: 16 images per prompt, 100 per request, 10 MB each, 16 MiB total, and a
1568-pixel longest edge. GIF uses the first frame; GIF and BMP become PNG.

## Models

Select `provider/model` through `-m`, `POLLYTOOL_MODEL`, or `/model`.
[Provider prefixes and keys](../README.md#models).
`--baseurl` sets inference/metadata endpoints for OpenAI-compatible providers and
Ollama; native Anthropic and Gemini use their own endpoints.

### Model form

Click the status model or use `/model`. `/keys` focuses its masked key field;
`/setup` adds endpoint, effort, theme, and sandbox defaults.

- Up/Down changes fields. Left/Right cycles providers.
- Tab completes a model and cycles matches; Shift-Tab goes backward. Typing starts
  a new completion cycle. Tab does not move between fields.
- Enter advances or applies. Escape discards the draft. Fields also accept clicks.
- Advertised input/output/cached prices are per million tokens. Pinned hosts use
  host prices; unknown prices appear as dashes or remain hidden.

### Discovery and completion

Suggestions require advertised text and tool support. Manual names always work,
even if discovery fails. Ctrl-R refreshes the catalog. Provider catalogs load on
use; details load when needed. Cached results appear first, refresh after an hour,
and survive failed refreshes. Failed automatic refreshes pause for one minute.

Caches are scoped to provider, endpoint, credential, model, and host; credentials
are not stored in the cache. Disk sessions use SQLite; memory stores stay in memory.

### Host pinning

For OpenRouter and Hugging Face, choose a discovered `model:host` to pin a route.
A bare model uses Automatic. Exact catalog IDs take precedence over route syntax;
unknown suffixes stay literal, and Ollama `model:tag` names stay intact.

OpenRouter also accepts `--modelhost` or `/set modelhost`; use
`/set modelhost automatic` to clear it. Sessions preserve pins; children inherit
them unless selecting another model/route.

### Key overrides

A key override applies to that provider across tabs in this process. It is never
saved to disk, the environment, or history. An unchanged field preserves the key;
Ctrl-U clears the draft so Apply restores the environment key. Discovery may use
a draft key without installing it; Escape leaves the active key unchanged.

### Context limit

The Context field accepts a number, `auto`, or `0` for unlimited. The status bar
shows the resolved limit before output headroom. Click it after a request for
input budget, response reserve, and safety margin.

New sessions use detected capacity with output headroom, falling back to 256,000
tokens. Positive explicit/saved/inherited budgets are clamped to the detected
window. `/set maxcontext auto` restores detection; `--maxcontext 0` opts out.
Automatic routing uses a conservative window only when every eligible host
advertises one. Ollama's model capacity and runtime context are separate limits.

### Thinking on OpenRouter

`/effort` (or `/set effort`) shows preference and effective setting, such as `off → low (required)`.
Use `/effort <value>` to save a new session preference; completion offers values supported by the current model.
The saved preference survives adaptations:

| Situation | Effective behavior |
|---|---|
| Required thinking with a known minimum | Use that minimum |
| Required minimum unknown | Use provider default and report the fallback |
| Optional thinking, preference `off` | Disable thinking |
| Thinking policy unknown, preference `off` | Use provider default and label it unknown |
| `dynamic` | Provider default |
| Model cannot reason | Omit effort; refuse a named effort when editing |
| Unsupported newly selected effort | Reject it with valid choices |
| Saved effort unsupported after a model switch | Use provider default and report adaptation |

OpenRouter's accepted vocabulary comes from model metadata; native providers may
clamp levels differently. Reasoning details replay only to the gateway and model
that produced them. An Automatic host change preserves that origin; an unrelated
model/gateway change does not. Originless saved reasoning remains visible only.

### Capability adaptation

Explicit metadata can omit unsupported optional tools/temperature, replace images
with explanatory text, or turn completed tool exchanges into text. Originals and
saved settings remain. An incompatible required response tool/schema errors.
Missing metadata does not impose those adaptations. Polly does not automatically
retry or batch images to satisfy provider-specific limits.

Provider-specific wire behavior belongs in the [Go API guide](API.md#providers).

## Tools

### Built-in tools

The [README](../README.md#built-in-tools) lists the default set. Parents also get
coordination tools; setup tools exist only during `/sandbox-init`. Use `--confirm`
for per-call approval. Any `--tool` selection replaces the defaults.

`read_artifact` pages/searches conversation artifacts and explicitly published
swarm evidence, including stored images. Unpublished artifacts stay private.
`swarm_help()` and `workflow_help()` provide the embedded coordination/API guides.

### Bash

Bash runs `bash -c` in a fresh shell and reports its final exit status. Pipelines
use the final command's status. `cd`, exports, variables, and shell options do
not persist across calls. Workflow `exec` adds `pipefail`, so a failed pipeline
stage fails the pipeline; ordinary parent/worker Bash calls do not.

Repeat required setup in each call. Use `set -e -o pipefail` when every command and
pipeline stage must pass, and handle expected failures explicitly. Conditional-list
exceptions still apply: `false && printf unreachable; printf later` succeeds.
Keep required validations separate or propagate failures yourself. Use writable
scratch for disposable caches/output; treat denied access as an environment issue.

Bash/file edits can show TUI diffs without changing model result text. Bash uses
private Git snapshots; it never stages into the repository's own index. Outside
Git or when capture exceeds its limits, no Bash diff is available.

### Shell tools

An executable must return JSON Schema from `--schema` and its result from
`--execute <json-args>` with exit 0:

```bash
#!/bin/bash
case "$1" in
  --schema) echo '{"title":"uppercase","description":"Uppercase text","type":"object","properties":{"text":{"type":"string"}},"required":["text"]}' ;;
  --execute) jq -r .text <<<"$2" | tr a-z A-Z ;;
esac
```

Load it with `-t ./uppercase.sh`. A top-level `sandbox` schema field customizes
its process policy. Schema discovery runs under its own restricted policy.

### MCP servers

Load Claude Desktop-format JSON with `-t mcp.json` or `-t mcp.json#server`.
Tools are namespaced as `server__tool`. Stdio processes follow the base sandbox
and server declaration; workspace-profile layers do not reach them. SSE and
streamable HTTP configs use `transport`, `url`, `headers`, and `timeout`.

## Skills

A skill is a folder containing `SKILL.md`, following the
[Agent Skills specification](https://agentskills.io/specification).

| Option | Effect |
|---|---|
| `--skilldir` | Search directory; default `~/.pollytool/skills` |
| `--listskills` | List available skills |
| `-S <dir\|git\|url>` | Load and activate a skill |
| `--noskills` | Disable skills |

Activation loads `mcp/` JSON servers and exposes `scripts/` paths for Bash.
Active skill instructions remain in the session. A `command: /sandbox-init`
frontmatter entry hides unsafe bare activation from completion; use the named
setup command. `/skill <name>` can still load its instructions for reading.

Built-ins sync to `~/.pollytool/builtin-skills`; a same-named user skill wins.
They include `feature-workflow`, `simplify`, `theme-designer`, and `sandbox-setup`.
See [feature workflows](WORKFLOWS.md#running-javascript-workflows) and
[build setup](SANDBOX.md#build-setup).

## Themes

`/theme` previews and selects; `/theme <name>` switches directly. Escape cancels a
preview. A committed selection saves `POLLYTOOL_THEME` in `~/.pollytool/config`.
`--theme` / `POLLYTOOL_THEME` also select at launch.

### Theme files

```json
{
  "name": "solarized-ish",
  "colors": { "accent": "#268bd2", "muted": "palette:8" }
}
```

Save user themes as `~/.pollytool/themes/<name>.json`. A name containing `/` or
ending in `.json` is a path; otherwise a user file shadows the built-in preset.
`default` is reserved. Write separate themes for light and dark appearances.

Precedence is flag → environment → config → `default`. The default follows ANSI
palette slots and terminal surface colors, with fixed bird colors. Four complete
presets ship: `amber-parrot`, `azure-parrot`, `midnight-parrot`, and `verdant-parrot`.
Missing copies are written to the themes directory at startup; existing files stay.

### Roles

| Group | Names | Unset behavior |
|---|---|---|
| Semantic | `ok`, `err`, `run`, `accent`, `active`, `muted`, `code` | Built-in color |
| Syntax | `syn-comment`, `syn-keyword`, `syn-string`, `syn-number`, `syn-func`, `syn-add`, `syn-del` | Corresponding semantic color |
| Bird | `polly-green`, `polly-light`, `polly-wing`, `polly-crown`, `polly-beak`, `polly-mouth`, `polly-face`, `polly-eye`, `polly-foot` | Built-in bird color |
| Surface | `background`, `foreground` | Terminal default |

Syntax fallbacks, in table order, are `muted`, `accent`, `ok`, `active`, `code`,
`ok`, and `err`. Set surface foreground/background together. Surface painting
applies only to the full-screen TUI. The amber approval border and image content
are fixed.

### Values

Use `#rrggbb`, `#rgb`, `palette:0`…`palette:255`, a recognized color name, or
`inherit`. Omission uses the built-in fallback. `inherit` is refused for `accent`
and `muted`; role names are not color aliases. Palette slot 0 is a real color,
not an omitted value.

Use the keys and roles above. Invalid values are errors. Load failure falls back
to `default` with a notice unless quiet. During live reload, an invalid or
half-written file retains the previous theme and retries.

### Applying a theme

The TUI reloads an edited active theme/config at most once a second, on the next
frame. The line frontend cannot switch or reload themes interactively.

The model's `set_theme` tool previews in memory. Persistence requires two calls:
first `persist:true` returns the exact path/colors for review; then
`persist:true, confirm:true` writes them. Replacing a file also needs
`overwrite:true`. `theme-designer` follows this protocol. Sandboxed tools cannot
write the theme directory; `set_theme` is the supported model-facing writer.

### Color in the line frontend

ANSI slots 0–15 retain terminal-remappable SGR codes; higher slots use 256-color
codes. True color is used when advertised, otherwise reduced to a palette color.
`NO_COLOR`, `TERM=dumb`, or redirected stdout suppress line-frontend color.
`NO_COLOR` also makes the TUI monochrome.

## Sandboxing

Sandboxing is opt-in. Use `polly --sandbox default` to start with workspace,
network, and protected Git writes. Add `private-home` to hide ungranted home paths.
A saved workspace profile or `--add-dir` alone does not enable a sandbox.

| Command | Purpose |
|---|---|
| `/sandbox-init [notes]` | Prepare builds, verify commands, and update `AGENTS.md` |
| `/sandbox [show]` | Inspect profile settings |
| `/sandbox try [command]` | Diagnose access failures and review grants |
| `/sandbox allow …`, `/sandbox forget …` | Manage explicit exceptions |
| `/sandbox storage` | Inspect owned storage |
| `/sandbox clean caches` | Clear tracked caches |
| `/sandbox reset environment` | Also clear restorable state; preserve configuration |

See [Sandboxing](SANDBOX.md) for defaults, permission rules, platform differences,
and exactly what cleanup preserves.

## CLI reference

`polly --help` lists all flags and their `POLLYTOOL_*` environment equivalents.
`--shot-script <file|->` and `--shot-size <WxH>` configure the headless capture
run described in [Headless screenshots](#headless-screenshots).
