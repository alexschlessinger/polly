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
- [Provider outages and retries](#provider-outages-and-retries)
- [TUI](#tui): [keys](#keys), [commands](#slash-commands), [agent panel](#agent-panel), [inside Rex](#inside-rex)
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
endpoint, reasoning effort, theme, and default sandbox. Apply saves the non-key
defaults; Escape skips setup and remembers that you did. The TUI form also has
a Context field, saved with the current session rather than as a launch default.
Reopen setup any time with `/setup` or `polly --setup`. A provider that signs in
instead of taking a key, such as `codex`, shows the account in place of the key
and asks you to sign in first.

Without the TUI (a dumb terminal, or `TERM=dumb`), setup asks for the defaults
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
`/setup`, and they're never saved. Polly needs a key for the selected
provider, except for Ollama, for OpenAI-compatible providers on a custom
`--baseurl`, and for providers you sign in to instead.
The [README](../README.md#models) lists the providers and their variable names.

### Signing in with ChatGPT

The `codex` provider runs OpenAI's Codex models on a ChatGPT plan instead of an
API key. Sign in once:

```sh
polly --login codex
```

That opens your browser on OpenAI's sign-in page. When you finish, the browser
comes back to Polly on `localhost:1455`, a loopback address only this machine
can reach. If the browser is somewhere else (an SSH session, say), paste the
address it lands on back into the terminal, or use `polly --login codex --device`
to get a short code to enter on a web page instead. In the TUI, `/login`
does the same in a dialog, and `/setup` signs you in when you pick `codex`.

The sign-in is the one credential Polly keeps on disk: `~/.pollytool/auth.json`,
readable by you alone, refreshed as its token expires, and hidden from
sandboxed tools like the rest of Polly's runtime storage. `polly --logout codex`
or `/logout` forgets it. Models are then `codex/gpt-5.5`, `codex/gpt-6-sol`,
and whatever else your plan lists; `/setup` discovers them once you are
signed in. Requests identify themselves as `polly`, and your plan's usage
limits apply as they do in the Codex app.

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

Assistant commentary is retained in session history for provider replay and
is hidden from the main chat and line output, which show only final or unphased
answer text. The agent panel exposes interim commentary as progress updates.

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
32 KiB per file and 64 KiB in total. Commits Polly creates carry a
`Co-authored-by: polly/<model> <polly@pollytool>` trailer naming the model that
wrote them; the trailer follows the session's current model.

Sessions are stored in `~/.pollytool/polly.db`. To back up the database while
Polly is running, use SQLite's [online backup API](https://www.sqlite.org/backup.html).

The database trails a running swarm by a fraction of a second, and by a few
seconds while a large swarm records faster than the database takes it. What a
swarm records goes first to a log in `~/.pollytool/polly.db-log/` and into the
database at the next checkpoint, so a backup taken while a swarm runs holds it
as of its last checkpoint. Whether an agent is queued or running at the moment
is kept in memory only, so another Polly that reads the database sees each
agent's status and counts as of the last thing the agent saved, and sees an
agent's session as in use for up to twenty seconds after the agent last ran.
If Polly crashes, the log keeps what the database lacks, and the next Polly to
open the database writes it there. Neither the
log nor a database commit waits for the disk: if the machine loses power or
the system crashes, the most recent turns and swarm records can be lost. The
database stays consistent either way, and Polly does wait for the disk before
it applies a swarm result to your source tree.

## Provider outages and retries

Each model call can recover from transient connection failures, rate limits,
server overload, and silent stalls. HTTP and stream failures share up to
**12 retries within five minutes of the first failure**. Backoff grows from
500 ms to at most 30 seconds, with jitter; server `Retry-After` hints take
precedence. The recovery window does not interrupt a healthy generation.
Authentication errors, known exhausted quota, invalid requests, hard request
deadlines, and user cancellation stop instead of retrying.

| Flag | Environment | Default |
|---|---|---|
| `--retries` | `POLLYTOOL_RETRIES` | `12`; `0` disables |
| `--retry-budget` | `POLLYTOOL_RETRY_BUDGET` | `5m` |

These are process settings, like `--timeout` and `--deadline`. The latter
still bound provider silence and total request duration separately. Retry
waiting is visible in activity status and remains immediately cancelable.

The TUI can separate a failed partial response as a **discarded attempt** and
start replacement output. Completed tool results remain saved; replay applies
only to the failed model request, never to earlier tool actions. Failed text
and incomplete tool arguments do not enter model history. Raw/piped streaming
output cannot be retracted, so it still stops after a failure once output has
been emitted. Providers may charge for discarded attempts; Polly retains usage
that was reported but cannot know unreported charges.

When recovery is exhausted, a successfully saved session keeps a recoverable
pause with its reason, attempt count, and earliest retry time. Reopen the
session and explicitly ask to continue. A paused worker can be continued with
its existing resume/follow-up action after that time, retaining its execution
and remaining iteration allowance. Opening a session or receiving peer mail
does not restart it automatically. User-stopped agents still require the user
to resume them; uncertain tool effects are never blindly replayed.

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
| Context usage, such as `41.2k/156k` | How requests are sized, what they carry, and the compactions so far |
| Agents | Running work, pending decisions, and collapsed finished history |

Click context usage or run `/context` to open the live Context popup in the TUI.
It stays above the status bar and updates while open, including during a turn.
Its input capacity gauge marks the compaction threshold; message-role bars show
estimated composition, not an exact reconciliation of the provider's count. A
chart of the last 32 reported request input counts uses absolute tokens, marks
compactions, and scales to its own peak (not the current model's window).
Kitty terminals and Sixel terminals with an explicit theme background show a
native chart. Other terminals, including Sixel with an inherited background,
use a Unicode fallback.
Session usage shows cumulative input (↑) and output (↓) tokens for the parent,
all its subagents, and their combined total, across saved turns and follow-ups.
It counts recorded requests, including retries and compaction summaries, not peak
context size; input includes cached tokens. Worker totals include workflow agents
and completed, failed, and released executions. Totals refresh at saved request
boundaries, without estimating an in-flight response. Missing provider counts
show `—`, or `≥` for a partial total; a failed worker-usage read is reported rather
than shown as zero. The capacity gauge and history chart remain parent-only.
Click the **Subagents ›** row, or press Tab then Enter, to open a live usage
breakdown in the same-sized popup. Each agent shows its name and cumulative input
and output bars, scaled separately to its share of recorded subagent input and
output. Completed and failed workers remain included even when hidden from the
agent panel. Scroll with the arrow keys, mouse wheel, or scrollbar when the list
overflows. Use the Context link, Left, Backspace, or Esc to return; the overview's
scroll position and live snapshot are retained. Ctrl-C closes the popup, and
clicking elsewhere leaves it up without the keys.
The footer shows a cache-hit meter, limits, and stored history; zero compactions
and the default summary model are omitted. The chart's compaction legend appears
only when the displayed history contains a compaction.
Short reveal and gauge transitions animate data changes, then settle. Scroll on
small terminals; Escape, Enter, or a click outside closes the popup without
changing the conversation.
`/context text` prints the full report instead, including session identity, model,
display status, and available last-turn usage. The line frontend prints that
report for both `/context` and `/context text`.

The Changes count, such as `+100 −20`, is informational. Open a tool row for its
recorded diff; use Git to inspect the net workspace diff.

A `~` anywhere means an estimate. The status bar totals what the session has
spent since this Polly opened it, swarm members included. `/context text` reports
the last turn on its own when available: its time, tokens in and out, cache rate,
and cost, exact when the provider bills it (OpenRouter) and estimated from the
model's advertised rates otherwise. `--meta` adds `cost_usd`, plus `cost_estimated=true`
when the cost wasn't billed.

A session open in another running Polly is unavailable in the picker. [Inside
Rex](#inside-rex), picking it brings that Polly's tab to the front instead.

The composer rule has a short accent and a steady approval cue.
Quiet mode hides the composer rule.

### Open sessions

Several sessions can be open at once, but only one fills the screen. `/new`
opens a fresh session, and `/close` closes the visible one while keeping it
saved (though not during a running turn). `/resume` or Ctrl-G picks a saved
root session. Alt+1…9 jumps straight to an open session, and Alt+] and Alt+[
step to the next and previous one.

Every open session has its own settings. Hidden sessions keep running and queue
your input, then post a single notice when they finish. Parent links open the
parent conversation.

### Windows and lists

The agent panel, the TODO panel, and the context popover from `/context` open
in windows that stand beside the conversation while you keep working, and
`/help`, `/title` and `/model` ask in a list. Inside [Rex](#inside-rex) the
windows and the list are Rex's, and inside [tmux](#inside-tmux) or
[Zellij](#inside-zellij) the windows are their panes. Everywhere else Polly
draws them on its own screen, placed and behaving as near to Rex's as a
terminal's cells allow:

- **The windows share one column at the right of the conversation** and never
  cover each other: the TODO panel on top, the agent panel under it, the
  context popover at the bottom, above the composer. Several can be open at
  once, all as wide as the widest, so their edges line up. They make room as one opens and take it back as one closes; when the
  column is short the tallest gives way first and scrolls, and the agent
  panel keeps at least half.
- **A window stays up while you type.** The TODO panel and the context
  popover take the keys when they open and when you click or scroll them:
  the arrows, Enter and Tab are theirs, and Esc closes the one that has
  them. An approval or a history search that is waiting takes that Esc
  first, and the window stays. Typing gives the keys back to the composer,
  where the letter lands, and so does a click anywhere else; the window
  stays where it is. Opening a panel again closes its window, as does the
  `×` on its border.
- **Tab goes round the open windows.** From the composer it gives the keys
  to the topmost one: the TODO panel, then the agent panel, then the context
  popover. In the TODO panel or the context popover, Tab first selects the
  window's action when it has one that is not selected, and the next Tab
  moves on to the window under it; from the last one the keys go back to the
  composer. One of those two open with no other window keeps Tab for its
  action, as before. While you are typing a `/command`, Tab completes it
  unless the agent panel is open.
- **The agent panel floats over the conversation**, which keeps its full
  width underneath. A terminal narrower than 120 columns gives the panel
  the whole width instead.
- **Any of the three docks beside the conversation.** The `◨` on a window's
  border, beside the `×`, makes it a pane of a column at the right, and the
  conversation that much narrower; the `❐` on a docked pane floats it again.
  Hovering either one names what it does in the status row, and Alt-P does
  the same for the window that has the keys.
  Docked panes stack in the same order, TODO on top and the context popover
  at the bottom, and fill the column: the agent panel takes what the others
  leave, in the split it used to open in, and without it the last pane does.
  Windows that still float sit over the conversation, beside the column,
  in one column of their own with one left edge. Where the terminal is not
  wide enough for both, the floating ones are narrowed so that 32 columns of
  the conversation stay uncovered.
  Each panel is remembered on its own, across launches, in the same
  `POLLYTOOL_PANES` setting Rex's docking uses (`agents`, `todo`,
  `context`). Below 120 columns, or with fewer than six rows of
  conversation, nothing docks and the marks are not shown on any of them.
- **`/help` lists the commands and skills**: type to filter, Enter to choose.
  A command that needs nothing more runs; one that needs an argument, or a
  skill, is typed into the composer for you to finish. The last row opens
  the full help page with the keys, and `/help <command>` opens that
  command's help.
- **`/title`** with nothing after it asks for the session's title on a line
  that starts from the one it has.
- **`/model`** lists the models of the session's provider and switches this
  session to the one chosen, as `/set model` does. The last row opens the
  setup form, which is also what opens when the models cannot be listed.

### Inside Rex

When Polly runs in a Rex terminal on macOS or Linux, it hands some of its own
chrome to Rex. If Rex refuses a request, Polly does the same thing on its own
screen, as it does outside Rex (see [Windows and lists](#windows-and-lists)),
and says so once. A window Rex opens whose terminal has not connected back to
Polly within three seconds counts as refused: Polly has Rex close it and draws
the panel itself. Until the agent panel's window has connected, the panel
stays in Polly's own frame.

- **Panels open in floating Rex windows** over Polly's tab instead of inside
  its screen: the agent panel, the TODO panel, and the context popover from
  `/context`. A window does not take the keys from the composer and does not
  resize Polly; click into it to scroll, navigate, or act on what it shows.
  It follows the visible session, and is Rex's to move, resize or minimize.
  Opening a panel again closes its window, as do Escape in the window and
  closing it in Rex.
  - **The agent panel** takes the right of the tab, down to the composer, and
    closes with the panel.
  - **The TODO panel** sits at the top right and follows the checklist.
  - **The context popover** sits at the right, above the status row. Its
    request history is drawn in text there, not as an image.

  With several open, the windows share one column at the right, all as wide
  as the widest, and never cover each other: the TODO panel on top, the agent panel under it, the
  context popover at the bottom. They make room as one opens and take it
  back as one closes; when the column is short the tallest gives way first
  and scrolls, and the agent panel keeps at least half. They sit over
  Polly's own pane, not over panes beside it, and above the composer however
  many lines it has grown to, or the form docked in its place. The TODO
  panel's window grows and shrinks with its checklist. A window you move or
  resize stays where you put it. If Rex fails to move a window, Polly asks
  again a couple of seconds later, and after three failures in a row leaves
  that window where it is.

  **Tab and Alt-P reach the windows from the keyboard.** Tab in the composer
  has Rex bring the first open panel's window forward with the keys: the TODO
  panel, then the agent panel, then the context popover. In a window, Tab
  first selects the window's action when it has one that is not selected,
  and the next Tab moves on to the window after it; from the last one the
  keys go back to Polly's own terminal. While you are typing a `/command`,
  Tab completes it. Alt-P in a window docks its panel beside Polly, or floats
  it again: Polly closes the window, opens it the other way with the keys,
  and remembers which, as below.

  **A panel you dock opens docked.** Dock a panel's window into the tab as an
  ordinary pane, with Rex's Float Pane action, and Polly keeps it up to date
  there and opens that panel as a pane from then on: beside Polly, or under
  a panel that is docked already. Float the pane again and the panel goes
  back to opening as a floating window. Closing a docked pane closes the
  panel and leaves it docked for the next time. Each panel is remembered on
  its own, across launches, as `POLLYTOOL_PANES` in `~/.pollytool/config`
  (any of `agents`, `todo`, `context`); setting that variable in the
  environment overrides the file.
- **Each session is a Rex tab.** `/new` and a pick in `/sessions` start another
  Polly in a tab of its own, with the same options and environment. Picking a
  session that another Polly in Rex already has open brings that Polly's tab
  to the front.
- **Polly asks in Rex's command palette** what it would otherwise ask in a
  dialog of its own. Rex shows a question only for an action its configuration
  file defines, so this needs the action below in Rex's `init.lua`, followed by
  `rex config reload`. Without it, or while Rex is showing another question,
  Polly asks in a list of its own. Without the action Polly tries Rex again
  only every half minute, so its own list opens with no wait in between.
  Polly stays usable while the palette is open,
  and the question appears in every Rex app connected to the server, so it can
  be answered from another device.

  ```lua
  rex.action{
    name  = "polly_ask",
    title = "Polly: Ask",
    args  = { ask = "object" },
    run   = function(ctx, args)
      local ask = args.ask
      if type(ask) ~= "table" then return { why = "failed", detail = "only Polly runs this" } end
      local answer, why, detail
      if ask.items then
        answer, why, detail = rex.ui.pick(ask)
      else
        answer, why, detail = rex.ui.input(ask)
      end
      if answer == nil then return { why = why or "failed", detail = detail } end
      if type(answer) == "table" then return { text = answer.text } end
      if ask.items then return { id = answer } end
      return { text = answer }
    end,
  }
  ```

  - **`/sessions`, `/resume` and Ctrl-G** list the sessions. Each row shows
    the session's title, its name and length, whether it is open here or in
    another Polly, and how long ago it was used. The session on screen is left
    out. Editing another session's title with F2 is only in Polly's own
    picker.
  - **`/help`** lists the commands and skills. Choosing a command that needs
    nothing more runs it; one that needs an argument, or a skill, is typed
    into the composer for you to finish. The last row opens the full help
    page with the keys. `/help <command>` still opens that command's help.
  - **`/title`** with nothing after it asks for the session's title, starting
    from the one it has.
  - **`/model`** lists the models of the session's provider and switches this
    session to the one chosen, as `/set model` does. The last row opens the
    setup form.
  - **An approval or a question from the model** is asked in Polly first. It
    is asked in the palette too when Polly's terminal does not have the keys,
    or after fifteen seconds with no answer and no key pressed, and the first
    answer settles it. Keys and clicks in one of Polly's own windows or panes
    count as keys in Polly: working in the agent panel's window does not send
    the question to the palette. The
    palette's first row decides nothing, so a key meant for something else
    cannot approve a tool. A question keeps only its first line there; the
    whole of it is in Polly.
- **A tab Polly opens comes to the front**: a new session, a session's
  existing tab, and the agent wall. Rex's app only switches tabs for another
  program when Remote Control is turned on in its settings. With it off, the
  tab still opens, behind the one you are in, and Polly tells you so and
  passes on Rex's reason the first time.
- **The tab is named after the session**: its title, or its name until it has
  one. A tab you named yourself keeps your name, and Rex's own name comes back
  when Polly exits.
- **`/plan edit` runs `$VISUAL` or `$EDITOR`** and gives it a pane beside
  Polly if it turns out to be a terminal program. A graphical editor opens its
  own window and gets no pane.
- **`/wall` opens the agent wall**: a tab tiled with one live pane per agent
  that is working or waiting on you, up to nine. Each pane is that agent's
  panel, so you can scroll, search, follow links, and stop or resume the agent
  there. An agent that starts takes the pane of one that has finished, unless
  you scrolled or navigated in it, or else a new pane. Escape in a pane, or
  closing it in Rex, takes that agent off the wall; `/wall` again closes the
  whole wall. Outside Rex, tmux and Zellij, `/wall` opens the agent panel.

### Inside tmux

Inside tmux, Polly asks tmux for what it asks Rex for. Nothing has to be
configured. With one host inside another (tmux in a Rex terminal, Zellij in
a tmux pane), Polly uses the innermost, since the pane it is in is that
one's.

- **Panels open in floating panes** over Polly's own pane, where tmux has
  them (`new-pane`, in tmux 3.8): the agent panel, the TODO panel and the
  context popover, in the same column and order as in Rex, without taking
  the keys from the composer. Click one, or select it, to scroll and act in
  it; Escape in it, or opening the panel again, closes it. An older tmux has
  no floating panes, and the panels float on Polly's own screen instead (see
  [Windows and lists](#windows-and-lists)).
- **Tab and Alt-P work as they do in Rex**: Tab goes round the panels' panes
  and back to Polly's own, and Alt-P in a panel's pane docks it or floats it
  again.
- **A panel you dock opens docked.** Put a floating pane into the window's
  layout (`join-pane`), and Polly opens that panel as a split from then on:
  beside its own pane, or under a panel already docked. Float it again
  (`break-pane -W`) and it goes back to floating. This is the same
  `POLLYTOOL_PANES` setting as everywhere else, and it works in a tmux
  without floating panes too.
- **A floating pane you move or resize stays where you put it.** One that
  tmux itself moves, to keep it inside a window that got smaller, is still
  Polly's to place.
- **Each session is a tmux window.** `/new` and a pick in `/sessions` start
  another Polly in a window of the same tmux session, and picking a session
  another Polly on the same tmux server has open selects that Polly's
  window.
- **The window is named after the session**, unless you named it yourself;
  tmux's automatic name comes back when Polly exits.
- **`/plan edit`** gives a terminal editor a split beside Polly. The editor
  starts in a window in the background and is moved beside Polly once it
  takes the screen; a graphical editor's window stays in the background
  until it exits.
- **`/wall`** tiles one pane per agent in a window of its own.
- **Questions stay on Polly's own screen.** tmux has no list to ask in, so
  `/sessions`, `/help`, `/title`, `/model`, approvals and the model's
  questions are asked in Polly, as outside tmux.

### Inside Zellij

Inside Zellij, Polly asks Zellij for the same things. Nothing has to be
configured, but it needs a Zellij recent enough to list its panes
(`zellij action list-panes`); with an older one Polly keeps to its own
screen.

- **Panels open in pinned floating panes** over Polly's own pane: the agent
  panel, the TODO panel and the context popover, in the same column and
  order as in Rex. Pinned, they stay in sight while the keys are in the
  composer. Focus one to scroll and act in it; Escape in it, or opening the
  panel again, closes it.
- **Tab and Alt-P work as they do in Rex**: Tab goes round the panels' panes
  and back to Polly's own, and Alt-P in a panel's pane embeds it or floats it
  again. A key Zellij binds itself never reaches Polly: if your Zellij binds
  Alt-P, use its embed-or-float key instead.
- **A panel you embed opens embedded.** Embed a floating pane in the tab's
  layout, with Zellij's embed-or-float key, and Polly opens that panel as a
  split at its right from then on; float it again and it goes back to
  floating. This is the same `POLLYTOOL_PANES` setting as everywhere else.
  Zellij splits a pane in half and only beside the pane that asks, so panels
  docked together stand side by side, not stacked.
- **A floating pane you move or resize stays where you put it.**
- **Each session is a Zellij tab.** `/new` and a pick in `/sessions` start
  another Polly in a tab of the same Zellij session, and picking a session
  another Polly in that session has open goes to that Polly's tab.
- **The tab is named after the session**, unless you named it yourself.
- **`/plan edit`** gives the editor a pane at Polly's right. Zellij does not
  say whether a program has taken the screen, so a graphical editor gets a
  pane too, which stays empty until the editor exits.
- **`/wall`** opens a tab with one pane per agent, laid out by Zellij.
- **Questions stay on Polly's own screen**, as inside tmux.

### Keys

| Key | Action |
|---|---|
| `Enter` | Send; accept a completion first if one is open |
| `Tab` | Move the keys to the next open window (TODO, agent panel, context) and back; with none open, accept a completion |
| `Alt-P` | Dock the window that has the keys beside the conversation, or float it again |
| `Ctrl-C` | Warn, then press again to cancel; again while canceling, quit |
| `Esc` | Dismiss a completion, dialog, search, or inspector; otherwise press twice to cancel |
| `Ctrl-R` | Search input history |
| `Ctrl-G` | Pick a saved session |
| `Ctrl-O` | Collapse or expand all activity |
| `Ctrl-V` | Attach a clipboard image |
| `Ctrl-Z` | Suspend |
| `Shift` + drag | Select text |
| Arrow keys, `PgUp/PgDn`, `Home/End` | Navigate the focused inspector; otherwise edit and browse history |

Canceling a turn takes two presses of the same key in a row, so a stray `Esc`
only shows a warning. Anything you send mid-turn queues up for later, and input
that fails to send comes back as a draft.

#### Agent panel navigation

The agent list shows each agent's status and, under it, the activity summary of
its latest turn, such as `thought 4s · 4 tool calls`. The latest command or tool
call gets its own line beneath the counts, such as `✓ $ go test ./llm/...`,
fitted to the panel's width. A turn that ran nothing shows the first line of its
answer instead.
These rows are projected from saved checkpoints in the background, a few agents
at a time, and follow running agents the way the view does. A blank line separates
agent groups; navigation skips that spacing. Finished and idle agents stay visible
for five minutes after Polly observes them becoming idle, then move into a
collapsed **Finished** section. Click its heading, or select it and press Enter
or Right, to show those agents and open their transcripts. The idle timer survives
closing and reopening the panel; resuming work resets it and returns the agent to
the main list. Agents awaiting review, integration, delivery, or approval, and
paused or failed agents, remain outside Finished.

Each agent also shows its most recently posted coordination message on one line:
`from Name: text` for incoming messages or `to Name: text` for outgoing messages.
The name is the other agent's display label (`parent` for the root). Undelivered
messages are marked `queued`. Whitespace is collapsed and text is capped at 160
characters, then ellipsized to fit the panel. Agents without messages show no
preview. These previews use cached coordination state, not assistant output or
tool results, and clicking the preview opens that agent's transcript.

Opening an agent shows its transcript with every activity line open, so the
view reads like the main buffer: the task, then each turn's activity line and
answer. Calls the agent's runtime reports as running join the activity line
with their own clocks as they start, before the transcript records them. Tool
output shows only what has been received so far, not a live subprocess stream.
The title row ends with the agent's status, right-aligned, as its row in the
list shows it, such as `idle · done` or `thinking 6m`; a panel too narrow for
both gives the status its own row. The clock measures the current phase or model
request, not total assignment duration; during retry backoff it counts down to
the next attempt. A highlighted clock flags retry or timeout details shown
below it, and a paused or failed execution shows its reason there.

The view opens following new output and expanded, so every run lists its calls
(the main conversation opens collapsed). `Ctrl-O` collapses or expands the view
that has the keys and leaves the other as it is. Folds made by hand, opened
thoughts and opened outputs are kept across updates and reopening the agent. For agents
running in this Polly process, the view includes live assistant text,
reasoning, and completed tool results before the next saved checkpoint. Updates
are coalesced about every 100 ms, not rendered per token; this is not a
subprocess stdout stream. The status ends in **saved** when the view updates
at checkpoints; normal live updates need no extra label. Agents
running elsewhere remain saved-checkpoint views, though their available status
metadata can still update.

The live continuation is bounded to 128 messages and 256 KiB of retained text,
with individual messages limited to 32 KiB and individual strings to 8 KiB.
A visible buffer-limit notice means some live output was clipped or omitted;
saved history is not truncated by these live limits. Large output and binary
media remain available through their artifact references when supplied. Retry
attempts replace provisional text rather than appending duplicates. When an
execution ends, the view returns to its saved checkpoint; unsaved failed/canceled
output is not presented as a committed answer, and an incomplete-output notice
identifies discarded live content. Explicit tool expansion choices and reading
positions carry across the live-to-saved handoff.

Scrolling up pauses following; `End` follows again. The title row holds the
applicable Stop/Resume/Review approval actions; long titles are ellipsized to
leave room for the controls. Follow-ups to an agent go through the conversation
that launched it.
Clicking outside the panel leaves it open; use its close button or `Esc` to
dismiss it.

`Tab` moves focus between the composer and the agent panel, by way of the TODO
panel and the context popover when those are open on Polly's own screen (see
[Windows and lists](#windows-and-lists)). In the agent list,
`Up/Down` selects an agent and `Enter` or `Right` opens its inspection. `Left`
returns to the list. Agent and Swarm views scroll with `Up/Down`;
in Swarm, `Left/Right` switches sections. `PgUp/PgDn` and `Home/End` scroll, and
`Backspace` returns to the parent.

In a focused panel, `Shift-Tab` selects a visible action and underlines it.
`Left/Right` or another `Shift-Tab` moves between actions, and `Enter` activates
the selection. `Up/Down` returns to normal navigation, and `Tab` moves on to
the next open window, or back to the composer. `Alt-P` docks the panel or
floats it again, and the divider's width controls apply only while it is docked.

### Slash commands

`/help [command]` has the full syntax for everything. In the TUI `/help` lists
the commands to filter and choose from, and the list's last row opens a
compact reference dialog with the keys: type to filter, scroll with the arrows
or Page Up/Down, and press Esc to close it.

| Task | Commands |
|---|---|
| Preferences and defaults | `/set [key [value]]`, `/speed`, `/setup` |
| Sign-in | `/login [provider] [--device]`, `/logout [provider]` |
| Conversation | `/sessions`, `/resume`, `/new`, `/close`, `/title`, `/rename` |
| Planning and progress | `/plan [goal]`, `/todo` |
| Context and files | `/context [text]`, `/compact`, `/attach <path>`, `/add-dir [path]` |
| Display | `/theme [name]`, `/clear`, `/copy`, `/screenshot [path]` |
| Tools and agents | `/tools`, `/spawn`, `/workflow`, `/wall` |
| Sandbox | `/sandbox`, `/sandbox-init [notes]` |
| Reset and exit | `/reset confirm`, `/exit` |

`/title <text>` sets the displayed title, and `/title` alone asks for it;
`/rename <name>` changes the handle used to reopen the session. `/clear` clears only the display, keeping the saved
conversation and the model's context. `/reset confirm` clears durable
conversation history.

`/tools list [namespace]` and `/tools show <name>` inspect tools, and
`/tools restart <server>` restarts a stdio MCP server.

`/copy` copies the last completed answer in the current conversation as its
original text, preserving Markdown but excluding reasoning and tool output.
While a response is streaming it copies the previous completed answer; it also
works after `/clear` and when viewing saved agent conversations. Clipboard
writes use the terminal's OSC 52 support, including over SSH; your terminal
must allow clipboard writes. Redirected output and `TERM=dumb` do not support
clipboard copying.

`/screenshot` saves a PNG of the screen as Polly renders it, images included,
to `polly-screenshot.png` in the system temp directory unless you give a path.

#### Track work without a plan

For a multi-step request, Polly can keep a checklist without entering `/plan`.
For example, ask: **“Check the import format, update the parser, and verify the
old fixtures still pass. Track those outcomes.”** Open `/todo` to see what's
active, what's done, and the reason for anything blocked. Ask in chat to add
or rename an ordinary item; `/todo` itself is read-only.

The checklist lives in the session, not in chat history. Only the
`update_todos` tool records changes. A Markdown checkbox, a successful command,
or a worker's reply does not mark an outcome done. Resuming a saved session
restores its checklist without starting work.

#### Plan first, then implement

`/plan <goal>` starts an investigation without authorizing project changes;
`/plan` without a goal enters or shows the current plan. While `PLAN` is
visible, you can ask questions or revise the proposal. Open the advertised
Markdown file in your editor (`/plan edit`), or revise it in chat. Put the
outcomes you want tracked in one `## Steps` section, using one top-level list
item per outcome. For example:

```markdown
## Steps
- Include title-only matches in session search
- Verify ranking and deduplication
```

Use `/plan review` to see what will become todos. Accept the displayed
revision with `/plan accept` to start work, keep discussing to make changes, or `/plan exit` to
leave without implementing. A save after review requires a fresh review.
Acceptance freezes the reviewed outcomes. On the implementation turn, Polly
adds them to the checklist with `update_todos`, keeping any ordinary items.
Later saves to the draft are ignored, and checkmarks never count as progress.

The divider above the TUI input shows `✓` for completed tasks and `☐` for unfinished ones.
Click the marks, select them with Shift-Tab and Enter, or type `/todo` to see
the checklist and any blocked reason. Once all tasks are done, the marks disappear;
`/todo` still shows the completed checklist. In line mode the checklist is plain text.
`/plan view` displays the accepted plan from the session database; there's no
need to keep the draft file to track or resume execution. Reopening
a session doesn't start an implementation turn: use `/plan continue` when
you want to continue. Reopen from the original project directory to work on
the plan. For a different plan, start a new session.

#### Preferences and scope

Use `/set` for session preferences: `/set` lists them, `/set <key>` inspects
one, and `/set <key> <value>` changes it. Editable preferences come first,
followed by the launch-only `system` and `skilldir` values. The editable keys
are `model`, `modelhost`, `temp`, `maxtokens`, `maxcontext`, `compactmodel`,
`effort`, `speed`, and `tooltimeout`. For example:

```text
/set model openai/gpt-5.4
/set effort high
/speed fast
/set maxcontext auto
```

Use `/context text` for context and display status, and `/sandbox` for live sandbox
posture and workspace profile settings. These are status views, not session
preferences.

| Interface | Where changes apply |
|---|---|
| `/set <key> <value>` | Current session, saved with it for resume |
| API key override in `/setup` | Running process, shared by its open sessions; never saved |
| `/sandbox allow …`, `/sandbox forget …` | Workspace profile; forgetting session-only items is not saved |
| `/theme` picker or `/theme <name>` | Current UI and the saved default for later launches |
| `/setup` | Saved launch defaults, with the immediate effects below |

Use `/set model provider/model` to change only the current session's model; a
failed session save leaves session preferences unchanged. Clicking the model in
the status bar or running `/setup` opens the setup form, which applies the
model, host, context limit, and effort to the current session, the endpoint and
key override to the process, and a changed theme to the UI. It saves the model,
host, endpoint, effort, theme, and sandbox defaults to `~/.pollytool/config`;
the context limit stays session-only and the key stays process-only. Once that
file exists, Apply saves only the fields you changed: a value the form shows
because this launch was started with it (`--baseurl`, `--model`, `--sandbox`)
applies here and leaves the file as it was. Cycle the sandbox field away and
back to save a policy the launch is already running. Its sandbox
default affects later launches, not the sandbox already running.

### Agent panel

Click agents in the status bar or an inline agent name to open the agent panel.
**Stop** cancels it and keeps it stopped until you choose **Resume agent**.
Parent follow-ups cannot clear your stop. **Review** answers its approval request.

At 120 columns or wider, the panel opens as a draggable 60/40 split; anything
narrower gets the full width. Focus follows the pointer, Tab switches between
the composer and the open windows without touching your draft, and Escape
closes it.

### Subagents

Ask Polly to delegate, or use `/spawn [--read-only] [--review] <brief>`.
Research agents return findings, and editing agents work in isolated Git
snapshots (Git 2.40 or later) that the parent integrates; they never commit
anything themselves. Research is done once it's delivered, unless `--review`
makes it wait for your acceptance. Write briefs with repository-relative paths.

Live agent rows and the progress header share a compact phase and clock.
A highlighted clock signals retries, additional request attempts, or an
approaching timeout; open the agent for the warning details. The panel also
identifies who last resumed the agent. Activity is available while this Polly
process owns the running agent.

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

A turn's work shows as one row per run of related calls: a triangle, then what
the run did. A transcript opens collapsed.

```text
▎ check the release                                                    5.3s
▸ Read 4 files
▸ Tests passed
▸ Viewed logo.png
I checked the release materials.
```

[Collapsed](../.assets/activity-story.png) · [One run open](../.assets/activity-read-evidence.png)

Adjacent calls share a row until the kind of work changes. An edit, a test run
and an agent launch each end a run. Reads, commands, viewed images and other
tools share one, counted by kind in a fixed order: `Read 4 files, ran 3
commands and used a tool`. A run of one call names it: `Ran go build ./...`.
Edit rows keep their change counts (`Edited 2 files +2 −6`). Assistant prose
between calls starts a new row.

Calls requested together stay in one row, which counts all of them. A search
asked beside an edit joins the edit run (`Edited 3 files +9 −2 and ran a
command`). A call asked beside an agent launch is counted after the agents
(`1 agent and read picker.go`). A test run asked beside other commands is one
of the commands (`Ran 4 commands`).

A command is named without its setup: a leading `cd <dir> &&` and leading
shell options such as `set -euo pipefail` are left out. A script too long for
its row shows its first commands, each with room to be read, and `…` for the
rest: `Ran git diff --check ; git ls-files -z | xargs -0 gofmt -l ; …`. Open
the call to see the whole command.

A call that failed, was denied or was canceled marks its row and stays in it:
`Read 6 files · 1 failed`. A lone call that did not succeed is named as it was
asked (`Edit main.go · denied`), and a failed test run is the whole label
(`Tests failed`). A saved call with no recorded outcome is counted and not
called passed. While a call runs its row names it, such as `Running go test
./...`. While the model thinks between calls, the newest row says so
(`Thinking… 4s`) once the thought is half a second old; a shorter one never
shows there, and no row is added or removed for it. On a folded run, an
in-flight label stays readable for half a second before the same row changes,
without slowing down the work. Failures and the final summary are not held
back. A row too narrow for its label shortens the label and keeps the mark; no
row wraps.

A collapsed view gives a turn's activity five rows. While the turn is still
adding to it, the newest four runs keep their rows and one row above them
stands for everything earlier, counted the way a run is with what changed
first: `24 earlier runs · 4 agents, edited 11 files +377 −141, read 29 files,
ran 23 commands and used 24 tools`. Once the answer or a notice follows the
activity, or the turn ends, all of it is that one row, without the count of
runs. Its mark counts every call under it that did not succeed. Activity of
five runs or fewer is left as it is, and an expanded view has no limit.

```text
▎ fix the review findings                                            12m59s
▸ 4 agents, edited 11 files +382 −142, read 29 files and ran 38 commands
Fixed the findings. Nothing is committed.
```

Click that row to list the runs it stands for beneath it. It then says how
many they are (`28 runs · 4 agents, …`), and each run opens as usual. Click it
again to fold them away; the folds made under it are kept.

Click a row to open that run: one row per call in the order asked, each step's
thought above the calls it made. Click it again to fold it; several runs can be
open at once. **Ctrl-O** puts every run in the view in one state. Collapsed
shows the rows above; expanded lists every call and thought row. A press
applies the state opposite the last one set and forgets folds made by hand,
unless that would change nothing on screen, in which case it applies the
other. Runs that begin later follow the last press, and a call that joins a
run takes that run's fold. The status bar names the state at its left for a
moment after each press.

A thought is a row among its step's calls (`∴ thought · 4s`). One that led to
no call is the last row of the run before it, so no thought stands among a
turn's run rows. A thought has a row of its own only where there is no run for
it to join, as in a turn that made no call (`∴ Thought for 4s`, and
`∴ Thinking… 2s` while it streams), and while it streams after a run whose
agents are still at work, where the next call's row takes its place. Click the
row to show that step's thinking under it, and
again to hide it. Opening the thought that is streaming follows the turn's
thinking: the run that takes the thought in opens so the text stays in view,
and each later thought of the turn opens as it starts, until you close one or
the turn ends. A call's output and recorded diff open by a click on the call.
No Ctrl-O press opens a thought's text, an output or a diff, and what you
opened stays open through every fold.

Shift-Tab selects a visible control; use Left/Right to move between them and
Enter to activate the focused one.

In an open run the right column gives each step's start as time since the
prompt (`1:13`), live and after a reload, where it comes from the saved turn
audit. A call's own duration appears inline only from one second up, so quick
calls carry no time; a running call shows its clock. A settled turn's elapsed
time sits at the right edge of its prompt's row, after its outcome when it did
not finish (`✗ failed · 34.0s`). A prompt row without room for both keeps the
time alone, and one without room for that is left as you typed it. A turn that
echoed no prompt leaves its outcome and time as a row of its own.

Thought text is what the provider exposes; OpenAI supplies reasoning summaries,
not its raw internal reasoning. Summary updates may arrive after the work they
describe. Polly forwards incoming updates without waiting for the full response.
The thought timer measures observed reasoning-stream intervals, excluding
intervening commentary and tool execution, not the model's hidden compute time.
Failed or canceled attempts are separated and marked **not saved**, even when
earlier rounds of the same turn were saved.

Reasoning uses the same Markdown layout as responses, including paragraphs,
lists, code, links, and tables, with muted colors. An opened thought shows the
retained tail of its step's text and notes when earlier text has been trimmed.
The line frontend's `--activity-details` prints a short thought tail at
settlement rather than streaming a live thought preview.

### Tools and changes

Tool previews keep the status, timing, relative paths, and read ranges in view.
`$` marks a Bash command, and `…` marks folded setup (such as a leading `cd`) or
omitted text. File edits show counts like `+3 −1` or `new +12`.

An open run lists every recorded call, with a blank row after an opened
detail and between its calls, its agents and its viewed images. Click a
tool row to show its recorded output beneath it, two columns in, under one
label row (`output · bytes 1–19,498`) and above the page controls. Click the
row again or anywhere in its expanded command, output, or diff body to hide
the details. Page controls keep their navigation action. The row's label is
the call, so no row dumps its arguments.

A Bash row whose label folded its setup, shortened a path, or
dropped lines beyond the first shows the command as written above its output,
wrapping to the pane width. Output that is a unified diff is colored like a
recorded edit. Edit and write rows show the full recorded diff, with long
lines wrapped and change counts always visible in the row. Any limits applied
when recording changes are labeled; the display does not impose a second
diff cutoff.

Text keeps its spacing, JSON is indented when it fits, and file contents use
syntax highlighting. JSON stays in its complete raw form when indentation or
string unescaping would exceed the formatting budget. Large outputs use pages
of up to 500 source lines or 256 KiB, with **Previous output page** and
**Next output page** controls. Byte ranges refer to the recorded output before
display formatting. Stored pages load on demand when an expanded output enters
the visible pane, so opening all details does not fetch offscreen results.
**Retry output page** retries a failed read. This also works in saved sessions
and agent conversations.

For keyboard access, press **Shift-Tab** to select a visible inline control,
then **Left/Right** to choose and **Enter** to activate it. **Esc** leaves inline
selection; **PgUp/PgDn** scrolls to offscreen controls. The same action keys
work in the agent panel.

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
| Kitty graphics | Kitty, Ghostty, WezTerm, Rex |
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

Choose a model as `provider/model` with `-m` or `POLLYTOOL_MODEL` at launch,
or `/set model provider/model` in a session; the [README](../README.md#models)
lists provider prefixes and keys. `--baseurl` sets the inference and metadata
endpoint for OpenAI-compatible providers and Ollama. Native Anthropic and Gemini
use their own endpoints.

### Setup form

Click the model in the status bar or run `/setup` with no argument to open the
setup form. It opens in the composer's place, under the rule, with the
conversation still visible above it. The title row names its scope
(`launch defaults`) and lists the keys; an error that stops Apply takes the
place of the scope.

`/setup` lists one row per field: provider, model, key, context, endpoint,
effort, theme, and sandbox. For a provider you sign in to, the key row shows
the account instead; `/login` signs in. Up/Down moves between rows, Left/Right
steps the provider, effort, theme, and sandbox, and Tab completes a model name
and cycles through matches (Shift-Tab goes backward; typing starts a fresh
cycle). Enter applies from any row, and Escape, Ctrl-C, or a click outside the
form discards the draft. Notes on the right say where the key comes from, the
model's prices and context window, and what a sandbox choice means; a sandbox
change takes effect from the next launch.

### Discovery and completion

Completions leave out models whose catalog entry rules out text
chat with tools, and include models it says nothing about. A name you type by
hand always works, even when discovery fails; the form says when a catalog
can't be loaded. Catalogs are cached for an hour (keys are never stored in
the cache), and Ctrl-R refreshes them.

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
shows the latest request's size against the input budget it was sized to: the
limit, at most the model's window less the room kept for the reply. The size
is in the provider's count (what the model last reported, moved by an
estimate of what changed since), with a `~` while no count covers it, such as
after a model change, or on a reopened session whose last reply is not the
last thing its requests carry. It
turns amber from three quarters of the budget and red past the point where
compaction starts, when a compaction could not shrink a request. A provider's
rejection lowers the budget shown. Click it, or run `/context`, to see the
window, the input budget, where compaction starts, the output limit, the
compaction model, the compactions so far, and what requests carry beside the
stored transcript.

New sessions use the detected capacity, leaving headroom for output, and fall
back to 256,000 tokens. Any positive budget, whether explicit, saved, or
inherited, is clamped to the detected window. `/set maxcontext auto` goes back
to detection, and `--maxcontext 0` opts out of compacting ahead of time: the
conversation compacts only when a provider rejects a request as too long. For
Ollama, the model's capacity
and the runtime context are separate limits.

### Compaction

`/compact` summarizes the current session's context on demand, without waiting
for the automatic threshold or continuing the agent's work. It takes no arguments
and adds no user message. The summary is saved with the session for subsequent
requests and resume; original messages remain in the transcript and available
through `read_transcript`. It uses the compaction model configured below and
makes a billable model call. Empty or already-compacted context with nothing
new is a no-op. A failed save or a summary that does not shrink context leaves
the session's context unchanged.

In the TUI, `/compact` queues behind an active turn, runs in the originating
session even if you switch tabs, and can be canceled like a normal turn.
`/clear` only clears the display; `/reset confirm` clears saved history.

When a request would take more than 90% of the input budget, Polly compacts
the conversation before sending it. It first clears tool output the model has
already read, keeping the newest. When that is not enough, it summarizes the
conversation and keeps the turn in progress verbatim if it is small. It
compacts only when that brings the request well under the budget, or, for a
request that does not fit, within it, so a conversation whose system prompt
and tools fill most of a small budget is not summarized on every turn. A
prompt that no compaction can make room for is refused before it is saved. A
provider's rejection of a request as too long teaches Polly the real window
while the session stays open, and the request is compacted to fit and sent
again; when the response reserve is what did not fit, Polly lowers it and
sends the request again as it is. If a compaction fails, a request that still
fits is sent as it is, and Polly makes no more summaries that turn unless
the provider rejects a request. The session keeps every message, and the
transcript notes each compaction and what it did; as it happens, the note
also says how far it shrank the request, and failures show as warnings. The
model can still page and search the whole conversation with
`read_transcript`.

The session's model writes the summary unless `--compactmodel`,
`/set compactmodel`, or `POLLYTOOL_COMPACTMODEL` names another, such as a
cheaper one; `auto` goes back to the session's model. When the request
still fits during automatic compaction, the session's model is sent the
conversation as its requests carry it, so the provider's prompt cache covers
most of what it reads; otherwise, when another model writes the summary, and
for `/compact`, it reads a transcript, uncached. `/set compactmodel` refuses
a model whose provider has no key or sign-in. If the compaction model fails,
the session's model writes the summary. A summary on another model is priced
at that model's rates; when that model has no known prices, the turn's cost
shows the rest as an estimate.

### Thinking on OpenRouter

`/set effort` shows both your preference and the setting in effect, such as
`off → low (required)`. `/set effort <value>` saves a new preference for the
session, and completion offers only the values the current model supports.
Your saved preference survives when Polly has to adapt it:

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

### Processing speed

Use `/speed` to see the current speed, or `/speed <mode>` to save a choice for
this session. At launch, use `--speed <mode>`. Set `POLLYTOOL_SPEED` in the
environment or `~/.pollytool/config` to set the default for new sessions;
resuming a session keeps its saved choice unless you pass `--speed` explicitly.

| Mode | What Polly requests |
|---|---|
| `default` | No tier override; the provider uses its default |
| `fast` | OpenAI Priority or Codex Fast on supported models |
| `ultrafast` | Ultrafast on supported OpenAI Responses or Codex models |

Tab completion uses the model information Polly already has. The TUI shows
`Fast` or `Ultrafast` beside the model when Polly expects to request it. When
the saved tier can't be used for this model or endpoint, `/speed` shows
Default and the saved choice. Polly omits the tier with a warning and never
substitutes a different paid tier. The saved choice applies again when you
return to a compatible model; `/speed default` clears it. If Codex support for
Ultrafast hasn't been confirmed yet, `/speed` says so.

For example, `polly --model openai/gpt-6.2-astra --speed ultrafast` requests
Ultrafast on the native OpenAI Responses API. `gpt-6.2-sol`, `gpt-6-astra`, and
`gpt-6.1-sol` are also supported. Leave `--baseurl` unset: a custom endpoint
uses Chat Completions instead. For `codex/`, the signed-in account's model
catalog must explicitly list Ultrafast for the selected model.

Paid tiers may cost more or draw more on your plan. Ultrafast has higher
pricing and separate rate limits; see [OpenAI’s Ultrafast guide](https://developers.openai.com/api/docs/guides/ultrafast-mode).
Polly’s cost estimates use model rates, not tier-specific rates.

Existing `/fast on` and `/fast off` commands, `--fast` and `--fast=false`
flags, and `POLLYTOOL_FAST` defaults still work.

### Capability adaptation

When a model's metadata explicitly says it lacks a feature, Polly adapts the
outgoing request. It can leave out unsupported optional tools, temperature,
or processing tiers, replace images with explanatory text, or turn completed tool
exchanges into text. Each adaptation is reported once per model for the
session. Your original messages and saved settings are untouched. An incompatible
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
`restore_artifact` creates a byte-exact local copy under the destination's write
policy, refusing existing files and never executing the content. See
[durable file handoff](WORKFLOWS.md#durable-file-handoff) for capture/restore limits
and verification guidance.

New default sessions include `restore_artifact`. An explicit `--tool` list must
name it to enable it; selections persist across resume. Older sessions without
this selection do not acquire it automatically.

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
| `--skilldir` | Replace automatic search directories; repeatable |
| `--listskills` | List available skills and their source paths |
| `-S <dir\|git\|url>` | Add and activate a skill |
| `--noskills` | Disable skills |

By default, Polly discovers `.agents/skills` in every directory from the nearest
Git root down to the launch directory, plus `~/.pollytool/skills`. Git worktrees
and nested repositories stop discovery at their own root. Outside Git, only the
launch directory contributes project skills; siblings and descendants are not
scanned. Missing search directories are ignored. Automatic paths are rediscovered
on launch and resume, not saved as explicit session search paths.

For matching names, an explicit `-S` skill wins, then the nearest project
skill, ancestor project skills, personal skills, and finally built-ins.
`--skilldir` (or `POLLYTOOL_SKILLDIR`) replaces project and personal discovery;
`-S` remains additive and built-ins remain available. Duplicate names across
explicit `--skilldir` entries or across `-S` sources are errors, as are malformed
skills in searched directories.

Discovery makes skills available; it does not activate them or start scripts or
MCP servers. Automatic project discovery skips sandbox-masked paths and symlinks
outside the project boundary; a `SKILL.md` must also stay within its skill root
and be at most 1 MiB. Use an explicit skill source for an external directory.

Activating a skill loads the JSON servers in its `mcp/` directory and exposes
its `scripts/` paths to Bash. Once active, a skill's instructions stay in the
session. A skill that names a `command:` in its frontmatter, as `sandbox-setup`
does with `/sandbox-init`, is offered through that command instead.

Built-in skills sync to `~/.pollytool/builtin-skills`, and a user skill with the
same name wins. See [feature workflows](WORKFLOWS.md#running-javascript-workflows)
and [build setup](SANDBOX.md#build-setup) for the two larger ones.

## Themes

`/theme` opens a preview picker, and `/theme <name>` switches straight away.
Escape cancels a preview. Pressing Enter in the picker or naming a theme directly
applies it to the current UI and saves `POLLYTOOL_THEME` to `~/.pollytool/config`
as the default for later launches. At launch, `--theme` or `POLLYTOOL_THEME`
picks the theme.

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

To sandbox only children, use `--agent-sandbox workspace+private-home` or set
`POLLYTOOL_AGENT_SANDBOX` in the environment or `~/.pollytool/config`. It works
with an unsandboxed parent, including `--nosandbox`. Per-agent `sandbox` options
can tighten this child policy but cannot loosen it or the parent's restrictions.
See [child-only policies](SANDBOX.md#child-only-sandbox-policies).

| Command | Purpose |
|---|---|
| `/sandbox-init [notes]` | Prepare builds, verify commands, and update `AGENTS.md` |
| `/sandbox [show]` | Inspect live posture and workspace profile settings |
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
[Headless screenshots](SCREENSHOTS.md). `--login <provider>` and
`--logout <provider>` run a sign-in or forget one instead of a conversation, and
`--device` makes `--login` use a code instead of a browser; see
[Signing in with ChatGPT](#signing-in-with-chatgpt).
