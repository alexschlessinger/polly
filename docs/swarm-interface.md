# Swarm model-tool interface

Coordination uses a fixed tool set per role. Parents have 14 tools; children have six. Typed children add `swarm_complete`.
Ordinary file, session, transcript, and artifact tools are separate.

| Available to | Tools |
| --- | --- |
| Parents and children | `swarm_read`, `send_message`, `wait_agent`, `list_agents`, `swarm_publish` |
| Parents only | `spawn_agent`, `followup_task`, `interrupt_agent`, `swarm_review`, `swarm_integrate`, `swarm_control`, `workflow_run`, `swarm_help`, `workflow_help` |
| Children only | `swarm_block` |

For ordinary research, spawn with `task_name`, `message` and `read_only:true`, wait with
`wait_agent`, then answer from the delivered result. Add `review:true` when explicit
parent acceptance is required. For editing, spawn, wait, validate the captured
candidate and finish with `swarm_integrate`. The runtime creates assignments,
captures snapshots and submits successful final results automatically. A blocker
keeps the assignment unresolved. Publication shares evidence without completing work.

## Explicit assignment and compact inspection

Managed `spawn_agent` requires `task_name`, `message`, and a boolean `read_only`:

```javascript
spawn_agent({task_name:"cache_audit", message:"Explain cache invalidation.", read_only:true})
spawn_agent({task_name:"cache_fix", message:"Fix cache invalidation and test it.", read_only:false})
```

Missing, null, string or numeric `read_only` values fail before capture or assignment. Research completes on durable delivery; add `review:true` for explicit acceptance. Editing requires integration, including acceptance of unchanged evidence. `review:true` with `read_only:false` remains invalid. Go `AgentRequest.ReadOnly`, JavaScript `readOnly` and CLI defaults are unchanged.

`list_agents({path_prefix?,offset?,limit?,details?})` returns compact worker entries by default: `id`, `agent_name`, `label`, `readOnly`, and `state`. Compact state contains `lifecycle`, optional `taskStatus`, `attention`, `deferred`, and an optional `detail` such as failure or iteration exhaustion. Idle execution does not imply completed work. The `self`, `parent` and `parentState` envelope remains; parent state uses the same compact projection.

Use `details:true` for the previous full entries, including `agent_status`, `name`, context/task/execution IDs, budgets and full state. Filtering and bounded pagination work in both modes. Workspace release uses the detailed entry's context:

```javascript
list_agents({path_prefix:"/root/cache_fix", details:true})
// Pass the returned items[].context as swarm_control's release id.
```

`swarm_read({view:"tasks"})` summaries retain `id`, `owner`, `description`, `status`, `displayStatus`, `revision`, `requirement`, `deferred`, and the result-reading reference. Use `swarm_read({view:"tasks",id,section:"details"})` for execution/run IDs, delivery receipts, lineage, accepted revision and retained `baseCommit`/`resultCommit`. JSON pointers select fields within the requested section. JavaScript `polly.tasks.read` continues returning the full task view.

These are breaking model-tool schema/default-response changes. Stored coordination records, historical transcripts, Go methods, workflow result shapes, automatic completion, and acceptance/recovery checks remain unchanged. Current descriptions and help identify the new contract when resuming older conversations.

## Breaking model-tool changes

Old tool names are removed from schemas and dispatch, with no compatibility aliases.

| Removed name | Replacement |
| --- | --- |
| `swarm_status` | `swarm_read({})` |
| `swarm_read({view:"agents"})` | `list_agents({path_prefix?, offset?, limit?, details?})`, including all available idle workers |
| `swarm_wait` | `wait_agent({timeout_ms?})`, returning an update summary rather than status |
| `swarm_followup` | `followup_task({target, message})`, supporting active and idle workers |
| `swarm_control` actions `stop` / `resume` | `interrupt_agent({target})` / `followup_task({target, message})` |
| `spawn_agent` arguments `task`, `session`, `task_id`, `background` | `task_name`, `message`, and required `read_only`; always asynchronous. Continue existing workers with `followup_task`; precreated tasks use `polly.agent({taskID,...})`. |
| `send_message` arguments `to`, `kind`, `reply_to`, `text` | `target` and `message`; never starts an idle worker |
| `swarm_tasks` | `swarm_read({view:"tasks", id, section, pointer})`; omit `id` to list |
| `read_messages` | `swarm_read({view:"messages", id})`; omit `id` to list your inbox |
| `swarm_search` | `swarm_read({view:"publications", query, offset, limit})` |
| `workflow_read` | `swarm_read({view:"workflows", id, section, step, pointer})`; parent-only, ID required |
| `workflow_start` | `workflow_run({source, input, background:true})`; default foreground behavior is unchanged |
| `workflow_cancel` | `swarm_control({action:"cancel_workflow", id})` |
| `workflow_acknowledge` | `swarm_control({action:"acknowledge_workflow", id, defer, note})`; deferral requires a nonblank note |
| `swarm_create_task` | `polly.tasks.create({...})` inside `workflow_run`; returns the task |
| `swarm_update_task` | `polly.tasks.update({task, revision, owner, dependencies})` inside `workflow_run`; returns the task |
| `swarm_claim` | Scheduler assignment through `polly.agent({taskID, ...})` |
| `swarm_submit` | Automatic final-result handling; typed children use `swarm_complete` |
| `swarm_snapshot` | Automatic final capture or explicit `polly.snapshot(context)` inside a workflow |
| `swarm_integration` | `polly.integration.prepare/read/revise/refresh/accept/apply/reconcile` inside a workflow |
| `swarm_read_artifact` | Unified `read_artifact` for authorized conversation and published artifacts |

`swarm_read` uses common `id` selection instead of the old `task`/`message`
arguments. Status retains `section:"decisions"` and `"working"`; task/workflow
selection retains sections, steps and JSON pointers. Publication queries are literal
and case-insensitive. Lists use 1-based `offset`, `limit` (default 50, maximum 100),
and `next`. Oversized selections attach complete content for `read_artifact`.
Reads never acknowledge delivery, accept work or change task state. Child workflow
inspection is refused at dispatch as well as excluded from schemas.

`workflow_run` still takes JavaScript source text and a JSON-encoded input string.
Background attempts direct the parent to `wait_agent`, which returns `{message, timed_out}`. Read saved decisions through `swarm_read`. Cancellation and acknowledgment retain
their coordination and timeout behavior. Budget grants remain client-controlled.

Go task, submission, snapshot and integration methods remain available. Historical
transcripts and saved coordination records remain intact; old records retain display
support. There is no database migration, automatic workflow replay or advanced mode.

The runnable [dependency/reassignment](../examples/workflows/task-dependencies.js)
and [interrupted integration](../examples/workflows/reconcile-integration.js)
examples are also bundled in `workflow_help`. Reconciliation observes the recorded
apply outcome and never reapplies a patch. Existing context ownership and parent
authority checks apply to every workflow operation.

## Pinned ordinary delegation contract

The six core operations follow [Codex b979d4f1](https://github.com/openai/codex/blob/b979d4f1f04538ba5a5fcc434d499c007bfe1b8c/codex-rs/core/src/tools/handlers/multi_agents_spec.rs): spawn_agent, send_message, followup_task, wait_agent, list_agents, interrupt_agent. Polly extensions include bounded listing pages, durable tasks, reviewed results, exact-revision integration, workspace snapshots and host-controlled budgets. This is not complete Codex API compatibility: no nested children, fork_turns, or copied permission policy.

Worker names are durable `/root/<task_name>` paths. Names use lowercase letters, digits and underscores, starting with a letter (1–64 characters). They are separate from assignment IDs and session titles. Targets accept canonical names, relative sibling names or stable member IDs. Old members receive deterministic `/root/agent_<memberID>` names. Duplicate or foreign names are rejected. Released workers remain available in list_agents; execution completion does not imply acceptance or integration.

Messages are delivered at safe input boundaries. They never start idle workers, including Go request/reply messages and review feedback. Follow-ups carry a separate durable execution intent. An active worker consumes the follow-up in the same execution, including input arriving during tool execution or finalization. A paused worker keeps its remaining model-call allowance. Review rejection or a submission awaiting acceptance reuses the assignment with a new revision; settled tasks get linked new assignments with preserved source provenance. Explicit follow-up cannot take a member from a running workflow. An interrupted turn stays unresolved, retains its worker and can be continued explicitly.

wait_agent defaults to 30 seconds, accepts 10 seconds through one hour, and returns an update summary. Results remain in durable addressed delivery and bounded readers. Children yield their slots and resume on addressed input, relevant task changes or timeout. User cancellation interrupts waiting. A running workflow reports once when terminal rather than waking its parent for each internal worker.

Go Spawn, Agent, Followup, Resume and StopMember signatures remain available. Go Followup and polly.followup retain their completed-task behavior, including explicit captured-code refresh; Go Send deliberately loses idle-start behavior. FollowupTask and InterruptAgent expose the new runtime operations. Legacy request records remain answerable: send_message answers the recipient's oldest outstanding addressed request before sending new informational mail. Restored executions remain paused until explicit continuation; saved follow-up input survives without replaying uncertain tools or JavaScript. Current model requests receive current contract guidance while historical transcripts remain unchanged.

## Public captured-code references

### Explicit model follow-up refresh

`followup_task({target, message, refresh?})` adds a strictly boolean argument,
default false. Ordinary continuation preserves its previous source: editing uses
the previous result, research uses its original baseline, and active or unresolved
work keeps its workspace. `send_message` changes information only.

With `refresh:true`, an idle worker whose previous assignment is done receives a
linked task against a capture of current parent files, including eligible dirty
and untracked files. Its conversation, role, model, tools and completion requirement
survive. Live non-Git research keeps its saved root and source kind with fresh
scratch. Active or paused executions, pending follow-ups, open assignments, live
workflow reservations and uncertain integrations are refused. Retained workspaces
or additional worker edits are preserved. Resolve them before retrying. Refresh
grants no budget and accepts no work. Start a new worker for independent review.

The model result changes from a message echo to compact provenance:
`{member,message,operation,task,execution,baseOrigin,baseCommit?,source?,note}`.
`message` is an ID. Operations are `steer`, `resume`, `new_task`; origins are
`parent`, `previous_result`, `original_baseline`, `existing_workspace`, `live_source`.
The baseline describes the selected capture; an existing workspace may contain
worker edits beyond it. Omit unavailable commits and historical launch fields.
Runtime-generated worker guidance names the same baseline and explains that a
refresh supersedes earlier file descriptions in the saved conversation.

Separate optional follow-up records pin selection before workspace release and
bind assignment/execution at launch. Retries with the same normalized arguments
and call ID reuse that record; changed arguments fail. Interrupted preparation
does not start on unrelated input. Recorded failures disable startup intent;
committed executions keep normal paused recovery. Reading provenance never
acknowledges delivery or settles work. Exported Go `FollowupTask` still returns
`*Mail` with its default behavior. `polly.followup({task,question,commit?,...})`
is unchanged. No tools, JavaScript operations, migration or history rewrite are added.

### Retained commits

Model tools and JavaScript use full retained Git commits to identify captured code. `spawn_agent`, `polly.agent`, `polly.context`, `polly.followup`, and `swarm_publish` accept `commit` instead of `snapshot`. `polly.snapshot(context)` returns `{commit, tree, source}`; pass `.commit`, never a capture record ID. Detailed task views and JavaScript task reads expose `baseCommit` and `resultCommit`, and publication views expose `commit`. Integration candidates keep their own IDs; nested versions in inputs, conflicts, plans, and receipts expose commits without snapshot IDs.

Only retained captures in this runtime are accepted. Branch names, abbreviated hashes, revision expressions, and arbitrary local commits are refused. Duplicate captures of one commit are compatible baselines, but ownership, submission records, task revisions, and acceptance remain separate checks. Captures and their Git ancestry are unchanged. Current public views omit commit fields when a capture is absent or forgotten, including nested integration receipts; recorded outcomes and raw historical records remain available.

## Explicit receipts and foreground delivery

Public integration candidates now always include `receipt`, with `null` meaning
no recorded apply attempt. Prepare, read, revise, refresh, and structured errors
share this contract. Read `candidate.receipt` directly; do not use `applyReceipt`
or fallbacks. `integrate(...).receipt` remains optional for unchanged work that
needs no application. Go structs/storage serialization and historical or
user-authored workflow values keep their existing shape.

Foreground `workflow_run` summaries add `next` and, on failure, structured `error`
alongside `id`, `status`, `output`, and `steps`. New host delivery metadata lets the
parent checkpoint save the result and consume its durable terminal notice in one
transaction. An unsaved or projected-away result retains its recovery notice.
Background results and direct Go launches retain notice delivery. Delivery
acknowledges successful workflows only; failures still need explicit handling.
Existing transcripts without trusted delivery metadata are unchanged. There are
no new tools, JavaScript operations, database migrations, or automatic replays.

This breaks current model/JavaScript snapshot arguments and fields without aliases. Replace `snapshot: candidate.merged.id` with `commit: candidate.merged.commit`, task `startingSnapshot` with `baseCommit`, and task `snapshot` with `resultCommit`. Exported Go snapshot APIs and their serialization remain unchanged, as do persisted coordination records, raw state views, historical transcripts, saved workflow results, and user-authored payloads. Old scripts must be updated before an explicit rerun; interrupted JavaScript is never replayed. No database migration is required.
