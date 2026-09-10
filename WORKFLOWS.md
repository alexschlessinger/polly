# Shared swarms and JavaScript workflows

Polly automatically registers a parent's direct children in a local swarm.
Sharing belongs to that family: no nested swarms, remote machines, or separately
started agent tools in this version. A workflow is an optional program over the
same scheduler, tasks, mailboxes, and isolated files that model tools use.
There is no separate "workflow swarm". A parent may coordinate directly, run a
script, or combine both. `/spawn` supplies one brief directly; asking the model
to delegate lets it choose the assignments. Neither requires JavaScript.

## Running a workflow

In the REPL or TUI:

```text
/workflow /absolute/path/to/workflow.js /absolute/path/to/input.json
/swarm workflows
/swarm cancel-workflow REPORT_ID
```

`workflow_run` runs script source and JSON input as a blocking model tool;
`workflow_start` starts it in the background, and `workflow_read` explicitly
loads the full saved report. These tools also work in one-shot CLI prompts.
The blocking result includes only report ID, status, output, and step count.
Full source, inputs, intermediate results, typed failures, and artifact
references stay available in the inspector and SQLite.
Foreground and background launches share durable registration, member reservation,
and cleanup. Foreground work honors caller cancellation and waits for active host
effects and their receipts to finish. Background work outlives the launching call;
explicit cancellation and runtime shutdown still stop it. Restart always runs an
explicit new attempt; saved JavaScript is never replayed automatically.

A parent that started a background workflow parks in `swarm_wait`: agent progress
inside a running workflow does not wake it, and the workflow posts one
informational message when its report turns terminal (completed, failed, or
interrupted), naming the report for `workflow_read` and `workflow_acknowledge`.
Members of a running workflow post no per-agent completion mail; directly spawned
children still do.

Use [fix-review-findings.js](examples/workflows/fix-review-findings.js) with a
copy of [input.json](examples/workflows/input.json). Replace the absolute source,
evidence filename, findings, and check commands. The example makes an isolated
fix, snapshots it, independently reviews that candidate, and runs checks in
another copy of the same snapshot. It allows one repair in the original worker
session, followed by a fresh reviewer checking every original finding and all
commands again. Workers make no commits. The parent must review the returned
task IDs, accept current revisions, preview, and apply editing results.

For documentation-only work, use
[doc-drift-audit.js](examples/workflows/doc-drift-audit.js) with an input file
like `{"source":"/repo","docs":[{"path":"README.md","focus":"models"}],"repair":true}`.
A read-only enumerator extracts checkable claims (tools, commands, flags,
defaults, env vars) from each doc, an independent verifier checks every claim
against the code, and one optional editor pass repairs drifted claims in an
isolated copy before a fresh verifier re-checks the original set. `repair:false`
limits the run to a structured drift report. Editors make no commits; the parent
accepts and applies the editing result.

The audit rejects empty claim lists and blank evidence. Claims cite document
locations; verdicts cite implementation locations, and drifted/stale verdicts
require an actionable change. Unverifiable findings fail the area as incomplete
without starting an editor. Reverification reads the editor's immutable snapshot.
These checks reject missing evidence, not fabricated citations; parent review
is still required.

## JavaScript contract

Scripts define exactly one workflow. No Node.js APIs, modules, filesystem,
network, timers, or process globals are exposed directly.

```js
const s = polly.schema;
polly.defineWorkflow({
  name: "research",
  inputSchema: s.object({question: s.string()}),
  async run({question}) {
    const agent = await polly.agent({
      label: "researcher", task: question, readOnly: true,
      schema: s.object({answer: s.string()}),
    });
    return {task: agent.task, answer: agent.value.answer};
  },
});
```

| API | Result and behavior |
| --- | --- |
| `agent({task, label?, input?, schema?, tools?, model?, readOnly?, source?, snapshot?, context?, session?})` | `{value, session, context, task, usage}`. Creates a member, or continues `session` with inherited authority and the host's model-call limit. |
| `context({source?, snapshot?, context?, readOnly?})` | Opaque ID for a fresh isolated copy. Paths are sources, never permission to edit an existing checkout. |
| `scope({context, label?}, async work => ...)` | Supplies the context to `work.agent`, `tool`, `exec`, and `snapshot`. `cwd` is refused. |
| `tool(name, args, {context})` | `{text, data, artifacts, step}`. Uses compatible tools with the same sandbox, approval, and timeout rules. |
| `exec(command, {context, check?})` | Tool result plus `exitCode`. Ordinary nonzero exit rejects unless `check: false`. |
| `snapshot(context)` | Immutable `{id, commit, tree, source}`. Use `.id` as the next agent/context's `snapshot`. |
| `integration.prepare({tasks:[{task,revision}], drift?})` | Ordered parent candidate; drift is `"paths"` (default) or `"tree"`. |
| `integration.read(id)` | Candidate, conflicts, provenance, supersession links, acceptance, and receipt. |
| `integration.revise(id, {task,revision})` | New candidate adopting an exact intermediate repair and continuing pending inputs. |
| `integration.refresh(id)` | Candidate fields plus `changed`; unchanged means the same ID and acceptance. |
| `integration.accept(id)` | Accepts that candidate and all contributing task revisions. |
| `integration.apply(id)` | Durable apply receipt; rechecks authority, acceptance, revisions, supersession, and filesystem preconditions. |
| `tasks.read(task)` | Current task description, acceptance criteria, revision, status, feedback, and result. |
| `tasks.review({task,revision,accept,feedback?})` | Accepts a submitted task or requests changes with feedback. |
| `release(context)` | Removes an inactive workflow-owned copy only when unchanged or demonstrably integrated. `work.release()` uses its scoped context. |
| `parallel(items, callback, {concurrency?, errors?})` | Ordered `{ok, value}` / `{ok, error}` entries. Default concurrency 8, bounded 1–256. |
| `log(message)` | Awaitable progress event and saved log step. |
| `fail(message, result?)` | Fails the workflow with structured diagnostic data. |

Schema helpers: `string`, `number`, `integer`, `boolean`, `enum`, `array`,
`object`, and `keyed`. Objects require their declared keys and reject additional
keys by default. `keyed(ids, valueSchema)` rejects duplicate IDs before dispatch
and requires every named key in the result. Agent responses are strictly decoded
and schema-validated; duplicate JSON keys, trailing JSON, or missing verdicts
are failures. Input is validated before any host operation starts.

For agents with tools, `schema` describes the final value rather than constraining
every model response. They investigate normally and finish with the internal
`swarm_complete({value})` tool, alone in its batch. The runtime validates that
value and supplies it both to the workflow and the assigned task for parent
review. `swarm_submit` is omitted for these executions; `swarm_publish` remains
available for progress. Completion does not accept or integrate a task.

Missing or invalid results receive up to two corrective prompts within the same
execution and model-call allowance. A third failure rejects the agent operation
with saved partial work. Successful tool work is not replayed. Tool denials,
provider errors, cancellation, truncation, and mixed completion batches keep
their terminal behavior. Explicit `tools: []` and inherited tool prohibitions
stay tool-free, using validated JSON text with the same correction limit.

`parallel` defaults to `errors: "collect"`. `"throw_after_all"` waits for every
branch and fails with the ordered results if any branch failed. Await all host
operations. Returning with pending operations, or waiting on a promise with no
host operation capable of settling it, fails the run.
Thrown primitives, including `null` and `undefined`, become structured errors
without stopping the other branches. Getters and `toJSON` methods used to
serialize workflow output or failures cannot start host operations.

The VM has one owning goroutine. Host work runs asynchronously and resolves
promises only on that goroutine. Defaults are five seconds per uninterrupted
JavaScript slice, 4,096 host calls per attempt, and a 512-frame JS stack limit.
Host waiting does not spend the JS slice budget. These limits are not a hard
heap quota. Do not treat scripts as an operating-system isolation boundary;
all external effects go through the runtime's authority checks.

`exec(check: false)` recovers only a typed ordinary target-process exit. An
approval denial, sandbox construction/startup failure, timeout, or cancellation
still rejects. A target shell acknowledges startup through a private descriptor,
so a failed sandbox launcher is not mistaken for the command's exit. An OS denial
*inside an already running command* remains that command's ordinary nonzero exit;
Polly does not infer stronger error types from stderr text.

Usage components absent from provider metadata are `null`, not invented zeros.
Execution usage accumulates across waits; a continuation is a new logical
execution, even when it reuses the same member conversation.

## Parent integration recipe

Use [integrate-results.js](examples/workflows/integrate-results.js) with an explicit
input file, using the same `/workflow`, `workflow_run`, and `workflow_start` launch
commands as other workflows:

```json
{"tasks":[{"task":"TASK_ID","revision":3}],"checks":["go test ./..."],"drift":"paths"}
```

The recipe prepares, resolves conflicts, reviews and checks the combined result,
repairs if needed, accepts, and applies. Reviewers receive the contributing tasks'
descriptions and acceptance criteria. `reviewInstructions` is optional; the parent
chooses the commands in `checks` (an empty array deliberately omits command checks).
Defaults are two repair executions across the entire attempt and one refresh after
applicable parent drift. Each repair inherits the normal model-call allowance;
neither repair nor refresh resets any consumed runtime budget. Every changed
candidate gets fresh review and every check; unchanged refresh skips revalidation.

Sandbox/setup failures, cancellation, budget exhaustion, and uncertain apply
outcomes produce blockers. Only ordinary check exits or negative review verdicts
enter the validation repair loop. Conflict repair receives the complete structured
conflict list, including binary/base/ours/theirs references.

Each check uses its own writable copy of the immutable candidate. Check-side
changes are never implicitly integrated. To adopt intended source changes, submit
a distinct editing repair based on the candidate, explicitly reproduce the intended
changes there, then call `integration.revise` and validate the new candidate.
Successful attempts release safe temporary copies and report dirty copies with
paths and context IDs. Failed attempts retain their copies for inspection.
`release` uses per-context activity checks, so finished check copies can be released
while the workflow continues. It preserves candidate snapshots and publications,
and refuses unintegrated edits. Workflow release and ordinary swarm cleanup share
the same Go checks and retirement routine. Both save submission provenance and
record retirement before deleting files, so interrupted cleanup cannot expose a
reused directory as an old execution context. After retirement is recorded, cleanup
finishes under a bounded parent-lease context even if its caller is canceled.
Ordinary cleanup still requires all members and workflows to stop; workflow release
checks only the selected copy.

Authority is inherited from the parent host, never supplied in JavaScript
arguments. Generic `tool` calls remain confined to isolated contexts and cannot
reach the parent integration tools. Children retain their existing permissions.
An apply that has started finishes under the parent lease even if its workflow is
canceled. The final interrupted report retains the completed apply step and receipt;
it does not claim rollback. The CLI write timeout is `--swarm-apply-timeout`, default
`2m` (`POLLYTOOL_SWARM_APPLY_TIMEOUT`); receipt recording has a separate bound.

Requesting changes through `tasks.review` does not wake a workflow-reserved member.
Continue it explicitly with `agent({session, task: feedback})`. Restarting JavaScript
is always an explicit new attempt. Integration ends at working-file changes;
staging, commits, and publishing are outside these operations.

## Sharing and task ownership

Agent calls inherit the host's configured iteration limit; `maxIterations` is
not a workflow option and is rejected, including through scope defaults.
An exhausted call rejects with `code: "iteration_limit"`, the saved member
`session`, partial `result`, and `usage`. It pauses the execution
(`paused · iteration limit`) and retains
the task, publications and worktree; it does not establish a stall or a code
failure. A script cannot grant itself more calls by retrying a paused member.

Every member receives stable identity, an assignment and its bound directory.
`list_agents` discovers the parent and teammates. `send_message` sends `info`,
`request`, or `reply` with an addressed request ID; a request accepts one reply.
`read_messages` reads only the caller's mailbox. Runtime-authenticated provenance
does not make peer text a user instruction or grant extra tools or filesystem
authority. Information waits until the next active turn. Requests/replies may
wake an idle member. A failed (`paused · failed`), stopped (`paused · stopped`),
or workflow-reserved member does not silently restart on peer traffic, and
informational mail never starts anyone.

Admitted mail remains in the model's saved history with its delivery receipt.
The TUI omits these internal envelopes from user turns and the restored composer,
including mail saved by older versions; `/swarm` still exposes the messages.
Completion reports preserve plain-text results as text and structured results
as JSON.

`swarm_publish` records attributed findings, optional source/artifact references,
and an optional immutable snapshot. Authors may supersede their own findings;
history is retained. `swarm_search` performs case-insensitive literal search of
explicit publications across retained runs. `swarm_read_artifact` opens only
family-pinned bytes. Private transcripts and unpublished artifacts are not
discoverable through these tools. Human inspection through `/sessions` remains
separate from model-visible publications.

Task states are `pending`, `running`, `blocked`, `changes_requested`,
`awaiting_review`, `done`, and `canceled`. Displays derive from them: a member
reads `idle · <disposition>` once its execution completes (`awaiting review`,
`integration pending`, `done`); see the lifecycle diagram in
[API.md](API.md#swarm-lifecycle). Claims require the current revision,
unassigned pending work, and accepted dependencies. Owners submit or record a
blocker; the parent accepts, requests changes with feedback, cancels, or updates
assignment/dependencies. Changes requested sends a request back to the owner.
Reassignment requires stopping an active owner. Cycles and cross-run dependencies
are refused. Canceling a dependency blocks its dependents until the parent
updates them. Accepting an editing snapshot with the same immutable tree as its
original starting snapshot completes the task immediately. Changed snapshots
become done after their accepted integration is applied. Explicit integration
still supports accepted unchanged inputs, including a mixture of unchanged and
changed tasks. A late execution cannot overwrite reassigned or canceled work.

The first assignment creates a run. All assignments created before its successful
settlement share its persisted execution budget. A completed run leaves findings
available but does not reenter the scheduling queue. A subsequent assignment
starts a new run. Defaults are 32 executing children and 256 logical starts.
`swarm_wait` parks after the current tool batch, releases its slot, registry and
session lease, and preserves its execution and remaining iteration allowance.
The member shows `waiting`; a wake re-queues the same execution. Addressed
requests/replies and relevant task/dependency changes resume it. A parent parked
in its own `swarm_wait` shows `waiting` too; it returns on mail addressed to the
parent, on a directly spawned child's launch, outcome or task change, when a
workflow reaches a terminal status or is acknowledged, or when nothing is active.
Transitions of members and tasks that a running workflow controls do not end the
parent's wait. Blocking spawns can return `yielded` so a parent can answer;
background plus `swarm_wait` is the coordination pattern, never sleeping or
re-reading reports. Several simultaneous blocking spawns are released together
when a member yields.

Workflow calls and ordinary spawns use this same pool and budget. A workflow
reserves its members across its steps. Concurrent calls to a reserved/busy
member fail; idle peer traffic cannot create an extra workflow-controlled turn.
Successful workflows release reservations. A workflow's agents report to the
parent through the workflow: no per-agent completion mail is posted while the
attempt runs, and one informational message arrives with the terminal report;
afterwards its members are ordinary members again and explicit resumes notify
the parent as usual. Failed or canceled attempts leave
their interrupted executions `paused · interrupted` for explicit parent/user
recovery. Completed and failed executions retain their actual outcomes; a failed workflow does not pause completed agents.
Failed and interrupted reports block settlement until the parent inspects and
acknowledges them with `workflow_acknowledge` or `/swarm acknowledge-workflow ID`;
for those reports acknowledgment retains the report and does not accept tasks or
discard changes. Acknowledging a completed report accepts, in the same
transaction, the read-only research its script consumed without reviewing and
reports the count; results the script already reviewed and editing candidates
with snapshots are untouched. A completed report blocks settlement only while it
still owns such research, and settlement names that step first, before per-task
blockers; a task blocker reports how many tasks are open.
Workflows belong to the same current run even when they never start an agent.

## Worktrees and integration

An editing member gets a detached linked worktree outside the source checkout.
Read-only members inside Git also receive snapshots. A parent running in an
existing linked checkout is supported with `workspace` and the default
`workspace+git` sandbox:
runtime Git administration has its own bounded policy, while agent tools keep
Git metadata read-only. `source` selects a checkout root in that same Git
repository; a standalone copy or a package subdirectory is not a valid source.
Use repository-relative paths in agent briefs and run commands in the member's
assigned directory. `source` selects snapshot input, not the member's working
directory or permission to enter the parent's checkout. Ordinary `git log`,
`git show`, `git diff`, and `git status` work there with either a main or linked
parent checkout; Git writes remain blocked. Read-only members cannot modify
their checkout; each member has a private scratch directory (`$TMPDIR`, also the
Go build cache) that siblings cannot read and that is removed with the context.
Return findings in messages, not as scratch files.
`HEAD` is a parentless snapshot commit. For history reviews, include the source
commit ID in the brief and use `git log <source-commit>` or
`git diff <older-commit> <source-commit>` from the member's worktree.
Live read-only files are used only outside Git, never as a fallback for a denied
or broken Git setup. Report setup errors without modifying `.git`, ignore rules,
or sandbox settings to work around them.
The runtime seeds a private Git index, preserving the original index timestamp
so Git still detects rapid same-size edits, and captures
tracked edits and non-ignored new files. It preserves the original index and
HEAD, excludes Git-ignored build/dependency data, and checks for source drift.
There is no global `chdir`. Native tools, shell/MCP processes, image paths,
repository instructions, and skills bind to the member's registry root.
Local MCP and shell tools are context-dependent by default. Required incompatible
tools fail launch; optional ones are omitted and reported. Context-independent
custom Go tools explicitly declare that property; remote MCP configuration uses
`"contextIndependent": true`. Member registries omit indexed semantic search.

Git 2.40+ is required for the explicit merge-base integration path. The runtime
refuses conflicted indexes, submodules, sparse/split indexes, paths whose
attributes apply a content filter (LFS), symlinked top-level Git metadata, and
special files. Untracked files follow the repository's and the user's global
ignore rules. Runtime-private paths such as session databases are excluded before
snapshot staging, including tracked files and private directories; existing Git
history is unchanged. New files have a default 32 MiB individual / 256 MiB total
capture guard, enforced on the tree that is actually published. External writers cannot
be locked by Polly; a detected inconsistent snapshot is refused. Stop external
writes when a consistent repository-wide snapshot is required.

Process sandboxes are supported on macOS and Linux. Editing requires an active
sandbox or explicit `--nosandbox` / `WithUnsafeNoSandbox`. Member policies deny
parent/sibling files and all writes to repository Git metadata. The common Git
object store stays readable for Git operations: this provides file-write
isolation, not source-code secrecy. An explicitly unsandboxed process has ambient
host authority; native tools still check their context policy.

The default capacity is 512 retained worktrees per family, including verification
and integration copies. Their directory slots are reserved before any member
sandbox starts so future siblings are already denied. This bounds OS policy
size; `worktree.Config.MaxWorktrees` and `swarm.Config.MaxWorktrees` allow a host
to choose a smaller/different capacity. Large policies may exceed platform limits.
Explicitly clean integrated/unchanged contexts to reuse slots.

`swarm_integration` exposes parent-only `prepare`, `read`, `revise`, `refresh`,
`accept`, `apply`, and recovery-only `reconcile` operations. For example:

```json
{"op":"prepare","tasks":[{"task":"worker-task","revision":3}],"drift":"paths"}
```

Preparation combines ordered submissions using each task's own starting snapshot.
It stops at the first conflict and retains the remaining inputs. Candidates and
merged snapshots are persisted immediately; preparation allocates no checkout.
Create reviewer or resolver copies on demand from `candidate.merged.id`.
Conflicts include their type, affected paths, file stages, and base/ours/theirs
snapshots, including binary and directory conflicts without textual markers.

`revise` takes an `id` and `repair:{task,revision}`. The repair must be a distinct
editing task based on that exact intermediate snapshot. It becomes a contribution
and remaining inputs are merged afterward. `refresh` merges the completed result,
including repairs, against the latest parent. A changed revision or refresh creates
a new ID and atomically supersedes its predecessor. Superseded candidates cannot
be accepted or applied. An unchanged refresh returns the same ID with
`changed:false`, retaining acceptance and allocating no snapshot or checkout.

`accept` binds the exact candidate and all contributing task revisions; `apply`
rechecks them before changing files. All editing contributions, including repairs,
become done only after confirmed application. `read` includes source references,
conflicts, predecessor/successor links, acceptance, drift policy, and any receipt.
Inspect them in `/swarm integrations`. Existing legacy previews without task
revision provenance require fresh preparation. Single-task and multi-task
integration both use `swarm_integration`; the former `swarm_preview` and
`swarm_apply` model tools are removed. Prepare with `tasks:[{task,revision}]`,
then explicitly `accept` and `apply` the returned candidate ID. Task review
alone does not accept an integration candidate.

The default `paths` policy checks every changed path's existence, file type,
Git mode and content identity, including both rename endpoints. Relevant ancestors
must be real directories; symlinks are not followed. Ignored existing destinations
are checked too. Unrelated parent edits survive. The final checkout is therefore
not necessarily identical to the tested snapshot. Optional `tree` policy also
requires full parent-tree equality. Every changed candidate recomputes its
preconditions. Receipts distinguish the validated snapshot from the observed
parent state at application. Parent branch and index remain unchanged.

A runtime-owned execution gate excludes parent tools during application, including
shell, custom, MCP, and long-running writers. Coordination tools release this
resource while waiting; orchestration acquires exclusivity inside apply.
Task mutations serialize with apply. A durable coordination intent and an
`apply-*.json` manifest record the patch identity, task revisions, and before/after
path states, including empty deltas. Confirmed duplicate calls return without writing.
After preflight, writing ignores turn cancellation (reported as "finishing apply")
with a two-minute default `swarm.Config.ApplyTimeout`; parent lease loss still
fences the write. A separate ten-second phase records the outcome, and runtime
shutdown waits for active writes and receipts. Timeouts and I/O failures require
inspection. On restart, matching after-states complete the original revisions;
matching before-states permit an explicit retry; mixed states require recovery.
Use `swarm_integration` with `op:"reconcile", id:CANDIDATE_ID`
(or `Runtime.ReconcileApply`) to
inspect an interrupted intent before an explicit retry. Later task revisions are
never overwritten. No automatic patch replay or rollback
is attempted. Parent branch and index stay unchanged.
Publishing commits or PRs remains subject to the task's existing authorization.

## Persistence, shutdown, and recovery

The parent session lease owns scheduling. Child leases plus execution generations
fence restored conversations. Generated output, mail receipts, and checkpoints
commit atomically. In-flight tool intents are journaled separately; recovery
appends interrupted receipts for uncertain calls rather than reexecuting them.
Provider context projection and the first-input persistence gate remain in effect.

Typed completion values are committed with their successful receipts. Correction
counts and pending feedback survive waits, budget exhaustion, and recovery.
Explicitly resuming an execution with a
durably accepted completion finishes the handoff without another model call,
even when its call allowance is exhausted. An uncertain completion intent is not
an accepted result. Historical workflow reports are not automatically rerun.

One-shot memory sessions promote into `~/.pollytool/polly.db` before coordination
mutates shared state. Live handles, IDs, prompt cache identity, and artifacts
survive promotion. Library memory mode is deliberately ephemeral unless the host
supplies a promotion callback. SQLite stores domain-keyed coordination
records with family-level membership and artifact pins. Published bytes survive
child retirement. Rows and pins cascade when the parent is deleted or expires.
Schema v6 adds paused child reports, preserving existing report IDs and delivery
receipts when upgrading earlier databases. Each root's swarm records carry a
format record written with its first coordination mutation. Roots written
before it do not open: `swarm.ErrUnsupportedFormat` names the cause, no
migration exists, and delegated work continues in a new session. Their records,
transcripts, artifacts, and worktrees stay in place.

Shutdown pauses members (`paused · interrupted`), leaves the parent
`paused · interrupted`, and marks interrupted workflow attempts. Reopen the
parent, inspect `/swarm`, and explicitly resume a member. A waiting/interrupted
execution retains its consumed starts and remaining iterations; an explicitly
retried failed execution spends another start. `/swarm grant N` adds an explicit
user-directed execution allowance. A workflow is restarted by submitting source
and input again as a new attempt. There is no durable JS heap or automatic replay.

For iteration exhaustion, `/swarm resume ID N` grants N additional model calls
to the **same** execution. Its consumed calls, task and files survive restart;
this continuation does not spend another logical start. A plain resume preserves
the existing allowance and refuses an exhausted one unless a completion is
already durably accepted and only finalization remains. Active workflow reservations
must settle or be canceled before taking over a member. Resuming its saved agent
does not replay or resume JavaScript; inspect the workflow report and explicitly
arrange any remaining workflow steps. `/swarm grant N` only extends the separate
logical-start budget and cannot replenish an agent's model-call allowance.
A resumed member reads `active · queued`, then `active`; `/swarm stop ID`
reads `paused · stopped`, and cleanup `idle · retired`.

Filesystem worktrees and Git references require explicit cleanup; parent TTL
expiry does not delete source changes. Cleanup refuses unintegrated current
edits. Whole-family cleanup also retires published Git snapshots, while SQLite
findings and published artifact bytes follow parent retention. Retired members
must be replaced with new members rather than executed in a missing directory.

All one-shot stdout is settled by default, with stderr progress and explicit
`--stream` for immediate output. Both CLI and TUI keep a parent's answer
provisional until coordination settles. Unresolved review, dependency, failure,
or reply work produces a blocker after one corrective prompt for unchanged
state. Structured output is emitted only after successful validation. When
settlement reopens a provisional answer, piped stdout prints every answer block
in order, separated by a blank line; the coordination prompts and tool exchanges
between them never print. `--schema` output stays the single final document.

### Inspecting and deferring retained work

`workflow_read({id})` returns a bounded summary of the outcome, acknowledgment,
step counts, agent/task references, failures, and deferred items. Use
`section: "steps"` to page step summaries, then `section: "step", step: "<id>"`
for one saved operation. Sections `source`, `input`, and `output` select the
other report fields. `pointer` is a JSON Pointer within the selected section,
for example `/value/value/claims/0` within an agent step. Inspection never
resumes JavaScript or reads the current worktree instead of captured results.

`swarm_tasks` lists summaries; select `task: "<id>", section: "details"` for
criteria and feedback, or `section: "result"` for the result (including retained
partial results). `list_agents` returns `items` with caller `self` and `parent`
identity, each item's `state` (lifecycle, busy, raw outcome, control, task
disposition, deferral, attention, display), context IDs, and the parent's own
`parentState`. Listings use
1-based `offset`, default `limit: 50`, maximum 100, and a 16 KiB response budget;
pass `next` back as `offset`. Oversized selections have bounded previews and
complete pretty-printed text artifacts. Use the receipt's `read_artifact` ID
with `offset`/`limit`, literal `query`, or exact `byte_offset` paging.

Workflow scripts explicitly accept research when they consume it:

```js
const task = await polly.tasks.read(research.task);
await polly.tasks.review({task: task.id, revision: task.revision, accept: true});
```

Accepting research acknowledges the completed investigation, including negative
or unverifiable findings. It does not certify a clean verdict or accept an
editing candidate. The audit and fix-review examples perform this bookkeeping
before branching on findings or allowing parallel failures to abort the scope.

Research the script consumed without reviewing is accepted when the parent
acknowledges the completed workflow: `workflow_acknowledge({id})` on a
`completed` report accepts every read-only, snapshot-less task it left awaiting
review and returns `acknowledged; accepted N research results`. Use
`swarm_review` on individual results first when a finding needs changes.

After reporting a failed, canceled, or interrupted workflow, the parent can use
`workflow_acknowledge({id, defer: true, note: "reason for retaining work"})`.
Acknowledgment and deferral commit together. Deferral retains unresolved tasks,
results, snapshots, and worktrees without accepting, applying, or canceling them.
It lets the parent finish without repeatedly prompting about those exact task
revisions. Acknowledgment without `defer` on a failed, canceled, or interrupted
report records the acknowledgment only.
Active work, unanswered requests, approvals, uncertain integrations, and
unrelated tasks still require attention.

Reads never reactivate deferred work. Explicit review, task changes, or recovery
clear the affected deferral. Changed task revisions or execution generations
invalidate it. Recover retained work only after any newer run in the workspace
settles. Paused executions retain their original remaining budget; an exhausted
allowance needs an explicit user grant. Failed executions use the existing
explicit restart and launch accounting. Nothing automatically replays a workflow
or deletes retained work. Older reports remain historical. Labels are derived
read-only: `paused · <reason>` describes the execution and `idle · <disposition>`
the task; reading them never accepts tasks.
