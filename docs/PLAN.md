# Session todos and plan mode

For commands and examples, see the [CLI guide](CLI.md#track-work-without-a-plan).

A **todo** records an outcome and its current state. A **plan** is an optional, reviewed agreement about which outcomes to implement. `/todo` shows progress recorded separately in the session database. Ordinary todos can coexist with approved outcomes.

## Track work without a plan

For a multi-step request or an explicit request for a checklist, Polly uses `update_todos` directly. A quick answer doesn't need an empty list, and each investigation call doesn't need its own item. Ask in chat to add or rename an ordinary item. `/todo` itself is read-only: assistant prose, Markdown checkboxes, worker activity, and opening or resuming a session never change an item's status.

## Draft and review

- `/plan` enters planning mode; `/plan <goal>` also starts the investigation. A persistent `PLAN` indicator distinguishes planning from execution between turns. A follow-up such as “what if we rank exact matches first?” revises the draft; it is not permission to implement.
- Polly reads the relevant project context and proposes outcome-level steps and how to verify them. “Verify ordering and deduplication” is a step; “read the search source file” usually isn't. Small changes can have short plans. No required interview, worker fan-out, or minimum step count.
- Show a compact `PLAN` label in the divider above the input. The full draft path is available in `/plan` and the checklist opened by clicking `PLAN`. Before acceptance the user can edit that file or ask Polly to revise it in chat. Do not create a tracked project document by default.
- The harness enforces the read-only promise. Apart from normal session bookkeeping, the draft is the only document Polly may write while planning. No todo writer, project edits, unrestricted shell, or write-capable delegation. This restriction applies even without a process sandbox; planning never widens an existing sandbox grant. Reopening a draft loads only selected native readers and defers shell tools, MCP, skills, and swarm startup until you explicitly exit or accept. A failed tool startup does not undo that transition or start an implementation turn.
- Both editing routes operate on the same document. Before writing a chat revision, re-read the file. Polly keeps the previous file beside the draft so an editor still saving to that file does not lose its changes. If Polly reports a conflict, close or reload the editor, compare the named file with the draft, and merge the edits you want into the draft. Once you’ve checked the backups, move them out of the plans directory. Polly retains up to eight prior revisions per draft and never deletes one for you. If an interrupted write leaves the draft path missing, Polly restores the previous file when it can identify it unambiguously.
- Review shows the steps that will become todos. **Keep discussing** stays in plan mode. **Exit plan mode** leaves without starting work. **Implement this plan** accepts the reviewed on-disk revision and starts work, with no second approval. If the file changed since review, show the change and request acceptance of the new revision instead. Repeated acceptance of the same review must not start a second run.

## Execute the accepted plan

At acceptance, store the exact reviewed plan in the session database. Close the draft for editing. **View plan** shows a read-only copy of that snapshot. User edits to the Markdown after acceptance are not imported, even from an editor left open before approval or after a session resumes. The prompt includes only short plans in full; for longer plans, Polly reads the approved text in pages with `read_accepted_plan`.

Acceptance does **not** create todos. On the implementation turn, Polly calls `update_todos` to initialize the approved outcomes from the frozen snapshot. This preserves their order and wording, doesn't replace ordinary todos, and is safe to retry. Progress belongs to separate session database records; Polly never writes status or checkmarks into the Markdown or infers completion from it.

Polly marks a pending item active when work starts and done only when its stated outcome is verified. A failed check leaves its item blocked with a concrete reason. A blocked item can become active again when the blocker is addressed. Progress updates appear automatically; the user does not need to open `/todo` or ask for a report.

Restore the accepted revision, checklist, last known progress, and any blocked reason on session resume. Reopening a session is not permission to restart interrupted work or replay a possibly completed action.

## TUI and line frontend

The divider above the input shows a compact strip such as `✓☐☐`: one checkmark per completed task and an empty box for every other task. Long lists are shortened with an ellipsis when space allows. When all tasks are done, the marks disappear, but the checklist stays available with `/todo`. There is no separate progress row.

Click the marks, select them with Shift-Tab and Enter, or enter `/todo` to open the checklist. The header shows how many tasks are done. The marks mean done (`✓`), active (`▶`), blocked (`!`), and pending (`○`). A blocker’s reason appears below its task.

```text
TODO · 1/4 done
✓ #1  Check the import format
▶ #2  Update the parser
! #3  Verify old fixtures
      Missing a fixture for quoted fields
○ #4  Update the examples
```

Standalone lists have no plan controls. If approved outcomes and ordinary todos coexist, each gets a section. A draft shows its path and a **Review draft** link; an accepted plan offers **View plan**. Click the link, select it with Tab and Enter, or press `v`.

Background updates must not steal focus or disturb a partially typed prompt. If the divider above the input is hidden or too narrow for the marks, `/todo` still opens the checklist. In the line frontend, `/todo` prints a plain-text checklist.

## Plan guarantees

- One accepted plan per session. Both the user and Polly can change a draft; neither can change the approved outcomes. Polly can change implementation details. If an outcome needs revision, report the blocker and use a new session for a different plan.
- Only top-level list items in `## Steps` can be imported as linked todos. Checkboxes are not progress. The review must show parsing errors rather than silently approving a malformed list.
- Progress is session-owned and reported by the executing agent; a completed tool call is not proof that an outcome was achieved. `/todo` is an inspection control, not a standalone manual task manager.
- After interruption, `/plan continue` is an explicit request to continue the accepted plan, not a replay of the last tool call. Preserve the approval even if starting the first implementation turn fails.

## Acceptance scenarios

1. **Track ordinary work.** In a fresh session, ask for a checklist. Tool calls create, rename, and update ordinary items. `/todo` shows them without `/plan`. A checkbox in an assistant reply changes nothing.
2. **Draft without side effects.** Enter `/plan <goal>`, inspect relevant files, and revise twice. The plan path is visible. Attempts to change project files or todos directly or via another tool are denied. Exit; nothing starts.
3. **Approve what was reviewed.** Edit the file externally, then request a chat revision. Both edits survive or a conflict is shown. Save again after review; stale acceptance does not start work. Accept the new review twice; exactly one run starts. Initializing the accepted outcomes twice yields one set of linked todos and preserves ordinary items.
4. **See truthful progress.** Finish one outcome and fail a later check. The divider above the input checks off only the finished task. Open the checklist by keyboard or mouse to see the count and blocker reason. In line mode the same details are readable without decoration.
5. **Verify the user-edit cutoff.** Leave the draft open in an editor, accept it, then save a changed step from that editor. Linked `/todo` items still show the approved wording and their recorded status. The user-facing plan action is view-only; marking a step done updates the database, not the Markdown.
6. **Recover without replay.** Quit after a failed check and resume. The accepted plan, completed count, and failure reason are available from the session database without the draft file or transcript reconstruction. No tool or worker is restarted solely by reopening the session.
