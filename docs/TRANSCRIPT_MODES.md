# Main transcript: Collapsed and Expanded

> **Status, 2026-11-21: built in the working tree**, with the decisions below where this spec left a question open. [CLI.md § Transcript](CLI.md#transcript) describes the controls as built. The "Current state" section and its code links describe the tree before the change: `repl_activity_story.go`, `repl_story_header.go` and `repl_story_evidence.go` are gone, and `repl_activity_chunks.go` and `repl_chunk_header.go` hold what replaced them.
>
> - **Runs.** An edit, a test and an agent launch end a run. Reads, commands, viewed images and other tools share one, counted by kind; a run of one call names it. A failure marks its run and never splits it.
> - **Batches.** Calls asked together stay in one run, which counts all of them: a batch joins the run of its weightiest call (an agent launch, else an edit), and a test asked beside other commands is one of the commands. This replaces the neutral `N calls` row that "Keep counts honest" below asks for. On real turns that row hid what the batch did and split the runs on either side of it, so a turn of three parts folded to seven rows.
> - **Commands.** A command is named without its setup (a leading `cd`, leading shell options such as `set -e`). A script too long for its row shows its first commands, each with room to be read, then `…` for the rest, not a stub of every command.
> - **Thoughts.** A thought is a row among its step's calls (`∴ thought · 4s`). One that led no call is the last row of the run before it, and stands alone only where there is no run to join. While the model thinks between calls the newest run's header says so (`Thinking… 4s`), once the thought is half a second old, as it would name a call in flight. A row of its own, which is what "leave one standalone `▸ Thought` row" below asks for, came and went with every response that thought before it called: 77 times in one real turn of 100 responses, 41 of them for under half a second. A triangle only ever means a fold; a thought's text opens by a click on its row.
> - **Row budget.** A collapsed view gives a turn's activity five rows. While the turn adds to it, the newest four runs keep their headers and one summary header stands for the earlier ones (`24 earlier runs · 4 agents, edited 11 files +377 −141, …`); once the answer or a notice follows, or the turn ends, the summary stands for all of them. It counts as a run counts, what changed first, carries the cue for every failed call under it, and opens onto the run headers, which open as usual. Expanded has no budget, and Ctrl-O still has two states. This goes past "Do not replace ordinary runs of different actions with one generic turn-wide `N calls` recap" below for long turns only: an edit ends a run, so a real turn of 147 calls that looked, edited and looked again folded to 28 rows.
> - **Turn time.** A settled turn's time and outcome sit on its prompt's row, not on an activity row.
> - **Ctrl-O.** A press applies the state opposite the last one set and clears folds made by hand, unless that would change nothing on screen; then it applies the other.
> - **Following a thought.** Opening the thought that is streaming follows the turn's thinking until a thought is closed or the turn ends.
> - **Smaller points.** No blank row separates batches; the step start in the right column marks them. The status bar names `collapsed` or `expanded` after a press. An inspected agent view opens expanded.

**Design spec, not a description of a shipped change.** The destination is one chronological transcript with two states: **Collapsed** (the default) and **Expanded**. A fold changes how much of the work you see, not the order in which you see it. There is no summary strip above the work.

Scope: activity in the main TUI conversation. Prompts, assistant prose, the composer, the agent panel, the line frontend, and what gets recorded in session history are not being redesigned. The code references below describe the working tree based on `4209fb5b`, including its in-progress activity-story renderer.

## What the destination looks like

Suppose Polly reads six different files in three batches, thinks between two of the batches, edits two files, and runs a test that fails. After the turn, the **entire folded activity** is three rows:

```text
▸ Read 6 files
▸ Edited 2 files
▸ Tests failed
```

Open only the reads. This is the **same transcript**, not a separate detail view. The times below are illustrative step starts, measured from the prompt:

```text
▾ Read 6 files
  read README.md                         0:02
  read docs/CLI.md

  ▸ Thought                              0:05
  read cmd/polly/repl_model.go
  read cmd/polly/repl_view.go

  read cmd/polly/repl_keys.go            0:08
  read cmd/polly/repl_status.go
▸ Edited 2 files
▸ Tests failed
```

The two reads at `0:02` were requested together. `0:05` marks the next step, not the duration of the thought. Open `Thought` to see that step’s retained text **there**, between the batches. Or leave the reads closed and open `Tests failed`: the test command and its status are visible immediately; its output still takes one more click.

## Current state: what the working tree actually does

### Four depths, plus local choices

[`disclosureDepth`, its labels, and `next`](../cmd/polly/repl_placements.go#L10-L56) define a per-view cycle. With activity present, `Ctrl-O` goes **Full → Calls only → Collapsed → Abridged → Full** from the fresh-view default. With no activity records it does nothing. The depth is not the same thing as whether an individual activity block is open.

| Depth | Ordinary behavior, before a local override |
|---|---|
| **Full** (default) | Blocks start open. Show each step’s retained thought text and all ordinary call rows. This is not “open every output.” |
| **Calls only** | Omit thoughts. The unfiltered body also takes the newest-three-call window; it is not an exhaustive call log. |
| **Collapsed** | Entering it folds every activity block. A block can still be opened by hand. |
| **Abridged** | Leaving Collapsed opens the blocks. Show up to the newest three text rows per thought and the newest three calls of the block; earlier steps outside the call window can disappear entirely. |

[`toggleAllDisclosures`, `applyDepth`, and `applyOpening`](../cmd/polly/repl_placements.go#L298-L353) explain the exceptions: only entering or leaving Collapsed resets all block folds. Every depth change clears phrase selections and resets per-thought fullness; it does **not** change `outputExpanded` on calls. A fresh record inherits the state of the line it joins, or the view’s depth if it starts a new line. Hydration applies the depth after replay ([`newRecordOpen` and `openHydratedLines`](../cmd/polly/repl_placements.go#L376-L422)).

### The folded row and the arrow strip are different render paths

1. **Collect an adjacent run.** [`transcriptDisplayEntries`](../cmd/polly/repl_transcript.go#L548-L610) projects transcript entries. [`layoutInlineActivityBlocks`](../cmd/polly/repl_inline_activity.go#L213-L243) merges neighboring activity into **one block**, even if it contains reads, edits, thoughts, and tests. Intervening visible prose or a notice separates blocks. This block—not a read or edit chunk—is the unit of a whole-line click today.
2. **Build phrases from records.** [`activityStory`](../cmd/polly/repl_activity_story.go#L55-L153) collects reasoning segments and tool batches, sorts by step, and puts a step’s thought before its calls. It does not sort by result arrival. A thought flushes the pending batch. Only adjacent successful single-kind batches of the same kind merge; a mixed batch stays one phrase ([`storyBatchOf`, `mergeable`, and `phrase`](../cmd/polly/repl_activity_story.go#L218-L292)). Thus a thought between two read batches currently *splits* the read phrases.
3. **If folded, paint one row.** [`layoutInlineActivityBlock`](../cmd/polly/repl_inline_activity.go#L245-L286) calls [`renderCollapsedStoryHeader`](../cmd/polly/repl_story_header.go#L64-L179). The newest block of a running turn shows its latest action. Other folded blocks recap the run as `N calls`, a reasoning-step count, or a lone thought label. Clicking the live label selects that action; clicking the settled recap opens the whole block. A failure count and the turn’s outcome or elapsed time appear when space permits. For a concrete live-to-settled example, see [`TestCollapsedActivityKeepsFailureAfterAdvancing`](../cmd/polly/repl_activity_handoff_test.go#L106-L146): `Reading next.go … 1 failed` becomes `2 calls … 1 failed · 34.0s`.
4. **If open, paint the strip, then the body.** [`renderStoryHeader`](../cmd/polly/repl_story_header.go#L283-L371) lays out selectable phrases joined with `→`, wrapping to at most two rows. [`selectParts`](../cmd/polly/repl_story_header.go#L247-L281) fits a recent suffix, an `… earlier` control, and a failure when possible. The triangle opens/folds the whole block; `… earlier` explicitly exposes the whole block; a phrase filters its body to just that evidence ([click dispatch](../cmd/polly/repl_placements.go#L117-L159)). The first record owns a **single** selection for the merged block ([`blockStoryFocus` and `selectedStoryEvidence`](../cmd/polly/repl_story_evidence.go#L51-L105)). That is not independent folding of several sibling chunks.

This distinction matters: deleting only the folded recap would remove something useful and leave the unwanted strip intact. The [current CLI guide’s story illustration](CLI.md#transcript) and [three-stage key-help string](../cmd/polly/repl_keys.go#L401-L419) are not the source of truth for this checkout.

### Chronology, details, and view state worth keeping

- **Steps already exist.** A step is one model response; a batch is a group of calls requested together. [Step starts and the no-clock fallback](../cmd/polly/repl_activity_steps.go#L16-L68) and [the live tool-start loop](../cmd/polly/repl_turn_ui.go#L139-L169) capture step order and batch starts. A single reasoning record can span multiple steps. On replay, [`historyHydrator.assistant`](../cmd/polly/repl_hydrate.go#L220-L251) assigns steps; [the turn audit and durable call order](../cmd/polly/repl_hydrate.go#L346-L360) restore known times and call positions. A step start is distinct from a tool’s own duration and from total turn elapsed.
- **The body already interleaves steps.** [`activitySteps`](../cmd/polly/repl_activity_steps.go#L253-L313) groups open records by step. [`activityStepBody`](../cmd/polly/repl_activity_steps.go#L324-L433) emits each thought before that step’s calls, places the step time once, and applies the three-call window only to unfiltered non-Full bodies. Explicitly opening the whole block bypasses that window. Today a long thought toggles between abridged and full text, not between a marker and text. The [reasoning display tail](../cmd/polly/repl_reasoning.go#L78-L125) is bounded; “full” never promises unavailable text.
- **Results already have their own controls.** [`inlineToolRowChunk`](../cmd/polly/repl_inline_tool.go#L24-L60) adds command detail, output, or recorded changes only when the call’s output is open. [`activateToolOutput`](../cmd/polly/repl_tool_output.go#L455-L518) handles an individual toggle or page action. Text output uses [500-line / 256 KiB page limits](../cmd/polly/repl_tool_output.go#L23-L49); [viewport-aware artifact loading](../cmd/polly/repl_tool_output.go#L619-L671) avoids fetching closed or offscreen results. Agents and inspected images currently have [additional body sections](../cmd/polly/repl_inline_activity.go#L287-L341).
- **Input is view-local.** [`toggleSelectedViewDisclosures`](../cmd/polly/repl_keys.go#L428-L454) sends `Ctrl-O` to the focused pane, never through an inspector to the conversation behind it. [Inline keyboard actions](../cmd/polly/repl_inline_keyboard.go#L10-L41) are made from visible hit targets. [`viewState`](../cmd/polly/repl_view.go#L57-L74) and [`rememberViewSections` / `applyViewSections`](../cmd/polly/repl_view_state.go#L87-L175) keep choices across view projections, not as new session records.

## Destination: behavior contract

The two states, calls-first chronology, and one-level toggle are the design decisions. The rules below for mixed batches, thought-only steps, and partially open views are proposed defaults so those cases don’t require another mode.

### 1. Fold a chunk, not a turn or a batch

A **chunk** is a contiguous run of related work in the transcript. A **step** tells us when something happened; a **chunk** tells us what folds together. They need not have the same boundaries.

- Give each chunk one short, clickable header. Consecutive reads of six distinct files across three batches get **one** `Read 6 files` row. A thought between those reads belongs inside the fold; it does not force another read row. An edit between two reads *does* end the run. Never join across visible prose, a notice, or a turn boundary.
- Keep counts honest: file counts refer to distinct known paths, call counts to calls. A repeated read of one file is not two files. A mixed parallel batch must not be presented as serial actions; one neutral row such as `3 calls` is sufficient when no single action describes it. Do not replace ordinary runs of different actions with one generic turn-wide `N calls` recap.
- While working, show the current action (`Thinking…`, `Reading…`, `Running tests…`). On settlement, leave a compact action or outcome. A failed call must still be visible **on its folded chunk** after later work succeeds. Keep denied, canceled, and unknown distinct from success. On narrow terminals, shorten the label before sacrificing the failure cue; don’t wrap a breadcrumb onto a second row.
- Neither state has a `Read → Thought → Ran tests` strip, an `… earlier` phrase picker, or a duplicate row listing the chunks. The headers themselves are the navigation. Turns with no activity need no fake chunk.

### 2. Expanded means calls visible, not thoughts reordered

- Show **one row per recorded call in the chunk**. There is no newest-*N* cutoff. A hundred calls may make a hundred rows; that is the cost of asking to see them. This is not a change to session retention, the [resumed history window](../cmd/polly/repl_hydrate.go#L18-L39), or output paging.
- Preserve step order. A thought goes with the batch requested by **its response**: render `▸ Thought` first, then those calls in request order. If a thought follows reads and leads to an edit, put it at the start of the edit chunk, not at the end of the reads. While the next action is unknown, `Thinking…` is sufficient. Keep a thought-only step between matching batches inside their fold. If there is no call chunk to hold it, leave one standalone `▸ Thought` row; bulk expansion still does not open its text.
- Parallel siblings share one step-start marker; do not give each a timestamp that suggests serial execution. Print the offset once when known; don’t invent `0:00` when it is unknown.
- An explicit action on `Thought` reveals only that step’s retained provider-supplied text, in place. Another action folds it. No automatic three-line preview and no turn-wide thought dump. Preserve the existing notices for trimmed or unsaved text. A call opens its own recorded output/diff; large text results stay paged. A neighboring call, thought, or other chunk is unaffected.
- Keep existing useful evidence reachable: an agent launch still leads to its agent, a viewed image to its image, and an edit to its diff. Do not duplicate them as extra calls or spill another chunk’s evidence under the one being inspected.

### 3. Two bulk states; local inspection is not another mode

| Action | Effect |
|---|---|
| Open a fresh main view | **Collapsed**. Activity headers are visible; inner details are closed. |
| Activate a chunk’s header or triangle | Toggle **only that chunk**. More than one sibling can be open. |
| `Ctrl-O` | Alternate **Collapsed ↔ Expanded** for all chunks in the addressed main view, one level only. Remove **Calls only** and **Abridged** from this cycle. |
| Activate a thought or call | Toggle **only that item’s detail**. A page control changes the page, not the fold. |

For mixed local folds, use the **last bulk state** to decide the next `Ctrl-O`, not the first visible row or a majority vote. If the bulk state is Collapsed and two chunks were opened by hand, the next press expands all; the following press collapses all. New chunks follow the bulk state. New calls joining an existing chunk inherit that chunk’s fold.

Keep inner choices independent: a parent fold hides the contents; it does not set inner flags. By default reopening shows markers and calls, not bodies. A body the user *explicitly* opened reappears when that same chunk reopens; no bulk action turns a closed thought, output, or diff on. Keep the held scroll position and keyboard focus on the same evidence where it survives; never redirect Enter to a different call because the screen reflowed. No new shortcut, persistent setting, or display mode is needed.

## Implementation map

1. **Group before painting.** In [`activityStory`](../cmd/polly/repl_activity_story.go#L63-L153), separate ordered steps and batches from the phrases used to draw the strip. Reuse [action classification](../cmd/polly/repl_activity_story.go#L178-L208) and [file/count labels](../cmd/polly/repl_activity_story.go#L344-L431). Build chunks from records **before** width fitting or visibility filtering. Don’t split a rendered string or use already-windowed rows as history.
2. **Paint the chunk and its body.** Replace the two header branches in [`layoutInlineActivityBlock`](../cmd/polly/repl_inline_activity.go#L265-L306) with vertical chunk headers and an optional body for each. Scope [`activityStepBody`](../cmd/polly/repl_activity_steps.go#L324-L433) to a chunk, replace its preview/full choice with a folded thought marker, and remove the call window. Keep the [settled turn trailer](../cmd/polly/repl_turn_dock.go#L504-L532) once, not once per chunk.
3. **Key choices to evidence.** A [tool record’s `expanded` flag](../cmd/polly/repl_model.go#L463-L476) or one [`storyFocus`](../cmd/polly/repl_story_evidence.go#L8-L49) per run cannot represent independently open siblings. Track the bulk default, chunk folds, and explicit thought/call choices separately. Anchor chunks to their first stable call or thought section and step, not a label, screen row, or phrase index. Use [display keys and their legacy fallback](../cmd/polly/repl_display_catalog.go#L38-L50); two provider calls can reuse an ID. Derive the same chunks from live and replayed records without persisting UI state in history.
4. **Route input to the item painted.** Replace [phrase hit targets](../cmd/polly/repl_placements.go#L72-L159) with chunk targets. Keep mouse, hover, and Shift-Tab / Left / Right / Enter aligned. In particular, [thought part keys](../cmd/polly/repl_activity_steps.go#L447-L469) use a local index; the [keyboard action list](../cmd/polly/repl_inline_keyboard.go#L13-L46) deduplicates keys. Include the owning section and step so two visible `Thought` markers are two stops. Update per-view section restore and [anchored toggles](../cmd/polly/repl_placements.go#L287-L318) together.
5. **Update what describes the controls.** When implemented, revise the [key help](../cmd/polly/repl_keys.go#L401-L419), [CLI transcript guide](CLI.md#transcript), [README description](../README.md#tui), and [headless activity-story scenario](../.agents/skills/polly-tui/fixtures/activity-story.txt). Its key sequence, phrase clicks, and screenshots exercise the old behavior. The [disclosure benchmark](../cmd/polly/repl_disclosure_perf_test.go#L49-L81) also counts a three-press round trip; measure the new two-press cycle for both dense batches and many small chunks.

## Acceptance scenarios

1. **Default and bulk toggle.** A fresh and a freshly resumed main transcript start collapsed. From the worked example, open Reads and Tests by hand: Edits stays closed. From that mixed state, two `Ctrl-O` presses expand all chunks, then fold all. Neither press opens a previously closed body. Replace the four-depth expectations in [`TestCtrlOCyclesDepthsAndCollapsedFoldsTheLine`](../cmd/polly/repl_disclosure_cycle_test.go#L16-L69); retain its unopened-output and held-viewport checks.
2. **Chronology and parallelism.** Six reads in three batches with an intervening thought yield one folded row and six expanded call rows. Complete parallel reads in reverse order: the visible order, thought position, and shared step offsets do not change. Add visible prose between reads: now there are two chunks with the prose between them. Exercise both live and saved variants using [the live/replay story fixture](../cmd/polly/repl_activity_story_test.go#L13-L105) and [audited step times](../cmd/polly/repl_activity_steps_test.go#L194-L262).
3. **No hidden calls, no unsolicited bodies.** With 100 calls, a long thought, a large text result, and an edit, the oldest and newest calls are both reachable in Expanded. Initially none of the thought text, result, or diff is shown. Opening one does not reveal the others; paging reaches the last recorded byte. Replace the [old row-window expectations](../cmd/polly/repl_row_window_test.go#L102-L166); keep [output paging](../cmd/polly/repl_inline_history_test.go#L193-L274) and [click-to-see-diff](../cmd/polly/repl_tool_changes_test.go#L158-L222) coverage.
4. **Live status, failure, narrow width.** Run a failing test followed by a successful read. The test row still says it failed while the read row shows current work, and after settlement. Resize from 100 to 32 columns: no arrow strip, no wrapped chain of labels, and each visible control operates on the row it names. A historical call without an outcome must not say “passed.” Adapt the [failure handoff](../cmd/polly/repl_activity_handoff_test.go#L106-L146) and [width/hit tests](../cmd/polly/repl_story_header_test.go#L13-L71).
5. **Stable, reachable evidence.** Open one thought, page an output, and leave a sibling folded. Append another batch, resize, and rebuild the view in memory: no focus or detail jumps to a different item. Mouse and keyboard can both reach chunk headers, thought markers, calls, and page controls. A focused inspector must not toggle the main transcript behind it. Preserve the invariants in the [evidence identity/replay tests](../cmd/polly/repl_story_evidence_test.go#L169-L202) and [focused-inspector test](../cmd/polly/repl_inspector_test.go#L380-L417), not their old mode names.
