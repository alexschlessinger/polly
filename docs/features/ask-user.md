# Asking the user from a turn

Status: implemented, 2026-11-18.

## Problem

Mid-turn the model sometimes needs a decision only the user can make: which of
two designs to take, which environment to migrate, whether to keep a
half-finished change. Today it can only guess, or end its turn with a prose
question — which drops the work in progress and makes the user restate the
context they are answering about.

The composer is the right place for an open-ended question. A question with
enumerated answers is not: those deserve to be choices, answered in one
keystroke, with the answer in the model's context when the turn resumes.

## Shape

A TUI-only tool, `ask_user`, registered the way `set_theme` is: only when
`state.outputCapabilities.surface == outputSurfaceManagedTUI`, and marked
built-in, so no tool view or allow-list hides the affordance the surface itself
provides — a skill that needs a decision, and a member whose bound registry
restricts its tools, keep the tool they ask with. Line, one-shot and raw runs
never see the tool; a session resumed there keeps its `ask_user` history as
ordinary past tool rows.

```text
ask_user(question, options[], header?, other?)
  question  Markdown shown above the choices; at most 8 KiB
  options   2–6 × {label ≤ 60 chars, description ≤ 240 bytes}, labels unique
  header    optional one-line title; at most 48 chars
  other     bool, default true: adds an "Other…" row that takes a typed answer
```

The tool is `Exclusive` — the agent aborts a batch that mixes it with any other
call before starting anything, so no tool mutates while the user reads — and
`LongRunning`, so the per-tool timeout never fires; the turn's context still
ends it. It is deliberately not `ReadOnly`: the answer is not reproducible, and
asking again is not free. It is not a recall tool.

The result is one compact JSON object the model reads:

```json
{"outcome":"answered","choice":"PostgreSQL","index":0}
{"outcome":"answered","choice":"Other…","index":3,"text":"use the prod DSN"}
{"outcome":"dismissed","why":"the user closed the question"}
```

`dismissed` is a successful result, not an error: the description tells the
model to stop and report what it needed, never to pick an option anyway.
Errors are `*tools.ToolError` with `USER_UNREACHABLE` (the context carries no
TUI capability), `ASK_BUSY` (a dialog is already open on that tab) and
`INVALID_QUESTION` (limits, duplicate labels).

## The dialog

A panel docked in the composer's place, the way the setup form is: it rises
from the status bar and grows upward instead of floating over the transcript,
so the question belongs to the session the same way its settings do.

```text
 Question  Which database should the migration target?    ↑↓ select · Enter choose · Esc dismiss
 Target PostgreSQL 16, which prod already runs.
 
 • no schema port
 • one week of work
 
 › 1 PostgreSQL
   2 SQLite
   3 Ask me later
   4 Other…
 
 Postgres 16 is already deployed; the port is a week of work.
```

- The body renders through `markdown.RenderMutedRows(question, inner, cache)`:
  the existing text-only preview path, so structure and emphasis survive, the
  foreground stays muted under the choices, and image references stay links —
  nothing composites under a dialog, so a resolved slot would be a dead glyph.
- The body is windowed to what the title row, choices and description leave,
  and scrolls with PgUp/PgDn, or with the wheel while the pointer is over it.
  The panel's height comes from its text, so an uncapped body would fill the
  screen.
- The title row carries the panel's name, who is asking, and the key hints on
  the right, the way the setup form's title row does; the hints drop from the
  left until they fit, `Esc` last. There is no footer row.
- The selected row's description sits in a pane padded to the tallest
  description, so rows never jump as the selection moves. Rows are one line —
  number and label, truncated to the panel — with the explanation only in the
  pane, which keeps six choices readable at a narrow width.
- Keys: `↑↓` select, `Home`/`End` jump to the ends, `1`–`9` pick directly,
  `Enter` choose, `Esc`/`Ctrl-C` dismiss, `PgUp`/`PgDn` scroll the body; the
  wheel scrolls over the body and moves the selection below it, a click on a
  row chooses it, and a click outside the panel dismisses it as `Esc` does. In
  the `Other…` editor, `Esc` returns to the list, `Enter` with text answers,
  and empty text does nothing.
- The free-text editor replaces the list and its explanation with one input
  row under the body, so the question stays visible while the user types.
- Keys count only once the first frame is painted, and pasted text never
  answers: a user typing in the composer when a member asks must not have their
  next Enter decide the question.
- A question on a hidden tab calls `signalHiddenLocked(signalApprovalNeeded,
  …)`: the notice interrupts even a running turn, because the turn behind it is
  stalled.

## The bridge

Nothing in `llm/` changes. The tool resolves a capability from the context,
exactly as `set_theme` resolves `themeUI` and `sandbox_propose` resolves
`sandboxReviewer`:

```go
type askUI interface{ AskUser(ctx context.Context, req askRequest) askAnswer }
```

`gotuiTurnUI.AskUser` posts a task to the event loop with `postUI`, opens the
dialog on the turn's tab model under that model's lock, and blocks on a
one-slot reply channel. The tool goroutine never touches the model or the
terminal. The tool is registered on `state.toolRegistry` beside
`registerThemeTool`, after `llm.NewAgent` has derived its registry, which keeps
later parent changes visible.

## Lifecycle

The invariant is that an open question is always answered exactly once, or the
turn behind it hangs. The modal layer already has the shape for this — a turn
waits on the sandbox review — and three pieces must be extended to it:

- `replModal.awaited` must report an unanswered ask dialog, so opening `/help`
  or the sessions picker over it parks it in `pendingModal` and restores it on
  close instead of silently replacing it.
- `replModal.wipe` must answer the question (`dismissed`), so a tab close,
  session switch or quit cannot orphan the tool goroutine.
- The turn's context ending must withdraw the dialog from either `modal` or
  `pendingModal` on the event loop and reply `dismissed`, mirroring
  `withdrawSandboxProposal`.

One question per tab at a time. `AskUser` refuses with `ASK_BUSY` when the tab
already holds a modal or a pending one, rather than queueing: a question that
appears after an unrelated dialog closes is a surprise, and the model can retry.
Other tabs are independent — each has its own model and its own dialog.

## Members and subagents

Swarm members and spawned agents share the session registry and reach the
parent tab's UI through `parentTurnUIFrom`, so they can ask. The question is
labeled with the asking member, the way approvals carry a requester:
`memberUICallbacks` already sets `BeforeToolExecute`, and adds
`withAskingMember(ctx, m.ID)` beside `withToolCall`; the tool copies it into
`askRequest.Requester`, and the dialog titles itself `agent <name> asks`. The
signal detail names the member too.

The tool value declares `ContextIndependent`: a member's bound registry keeps
only tools that rebind themselves or say they do not need to, and a plain
`tools.Func` is omitted as a local tool the binding cannot carry. Without the
declaration the tool is registered for the parent's own turn and silently
missing from every member's schema — which is how the first live probe found
it.

The question lives in the member's transcript, not the parent's: the parent tab
shows the dialog and nothing else. The member's own row records the exchange
when it resumes.

## Transcript

The transcript is the durable record of the decision, so the row reads as one:

- The collapsed row is `ask_user "Which database should the migration target?"`,
  summarized from `question`'s first line in `summarizeToolArgs`; the chosen
  label rides the row's meta (`· PostgreSQL`) so the answer is visible without
  expanding.
- The expanded row renders the question through the markdown renderer at the
  current width, then `→ PostgreSQL` or `→ dismissed`, under a `question` label
  instead of `output`. The row body knows its width (`inlineOutputSection` takes
  it) and caches the rendered rows per width, since transcript rows reflow on
  resize. The raw JSON is not repeated: it is the same decision in a less
  readable form.

## Non-goals

- Open-ended questions: end the turn and let the user type.
- Multi-select, forms, multi-field input, and images inside the dialog.
- A durable pending question across restarts: a turn killed while asking
  records `dismissed`.
- A line or one-shot fallback. Those surfaces have no event loop to open a
  dialog on, and the tool is not registered there.

## Tests

- Dialog, in the `sandboxCommandREPL`/`handleEvent`/`plainStyledText` idiom:
  body rows render, the description follows the selection, `1`–`9`/`Enter`/`Esc`,
  body scroll, no answer before the first paint, paste ignored, `Other…` editing
  and its empty-`Enter` no-op.
- Bridge: context cancellation withdraws and replies `dismissed`; `ASK_BUSY`
  while a dialog is open; `awaited` survives a `/help` opening over it; a hidden
  tab gets the approval signal.
- Surface and agent: schema absent on line and one-shot registries; the tool is
  exclusive, untimed, not a recall tool, and survives an allow-list view.
- `.agents/skills/polly-tui/fixtures/ask-user.{json,txt}` plays the whole thing
  off-screen with no provider key: a fixture turn calls the real tool, the
  scenario waits on the dialog, chooses, and captures the question, the
  selection and the answered row. The turn keeps running behind the dialog, so
  the scenario waits on text rather than `:settle`, whose two reads never
  agree while the elapsed timer repaints.

## Docs

`README.md`'s built-in tools table gains `ask_user` (TUI only) beside
`set_theme`. The tool description carries the "when to use it" guidance — a
decision with enumerated answers that the model cannot resolve from the
workspace — since the coding contract already asks for questions when missing
information materially affects the result.
