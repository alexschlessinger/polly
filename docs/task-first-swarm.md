# Task-first swarm: completed design

Status: complete for the implementation in this revision, format 2 (2026-09-10).
Merge and release status belong to the associated pull requests; this page records
the implemented contract. The [workflow guide](../WORKFLOWS.md),
[API reference](../API.md#swarms-and-workflows), and
[state model](swarm-state-model.md) are the maintained repository documentation.

## Principle

Preserve acceptance and integration rigor while making resource reclamation
independent of permanent member lifetime. The task is the durable unit of work;
the member is a reusable identity and conversation. An execution is one budgeted
turn. A workspace is a resource that can be released and restored.

The parent should normally start work, wait for its result, and answer. Editing
adds one integration call. Explicit decisions remain where they carry meaning:
review requested by the assignment, feedback, integration conflicts, uncertain
writes, failures, dependencies, replies, and budget extensions.

## Decisions that define the contract

| Decision | Implemented behavior |
| --- | --- |
| Completion belongs to the task | Each task fixes `delivered`, `reviewed`, or `applied` at creation. Authority-incompatible reassignment is refused. |
| Ordinary research finishes on delivery | Exact parent-input or workflow-step receipts finish the task. Negative findings can be delivered successfully. |
| Review is opt-in for research | `review:true` requires explicit acceptance of the submitted revision. |
| Editing finishes through integration | `swarm_integrate` / `polly.integrate` accepts exact revisions and applies changed work; immutable unchanged proof avoids an empty apply. |
| Identity survives release | The runtime reclaims safe workspaces independently of the member's conversation and task history. |
| Follow-ups reuse the same member | A linked task retains the requirement and authority; the old task and result are unchanged. |
| Source defaults are explicit | Research follows its starting snapshot; editing follows its submitted snapshot. A known snapshot explicitly refreshes the new task. Missing proof fails closed. |
| Parent tools lead with decisions | Needs decision, working, and done describe normal coordination; detailed axes remain available for inspection. |
| Presentation is derived | One set of facts feeds separate settlement and presentation consumers. Display folding cannot suppress obligations. |
| Waiting releases execution capacity | Event waits retain the logical execution and allowance, while releasing the slot, registry, and session lease. |
| Format changes once | Completion and restoration use format 2 together; older swarm records have no automatic migration. |

## Why retain the explicit records

Threads, branches, PRs, and containers are useful durable objects, but Polly also
supports local integration without a remote. It needs to know which exact submitted
revision was accepted, what tree was validated, what parent state was observed at
application, and whether a write completed. Git history alone does not answer all
of those questions when the parent has uncommitted changes.

The solution is to keep that evidence while reducing routine coordination. A
research receipt can satisfy delivery without asking the model to accept a report.
One integration operation can own acceptance and application without removing the
candidate, preconditions, or receipt. Release can reclaim disk and bindings without
turning the member into an unusable historical record.

The design keeps two especially useful properties: derived presentation cannot
drift from persisted facts, and a parked child can wait for an event without
occupying execution capacity. Neither requires exposing every underlying axis in
the parent's normal tool output. These are properties of Polly's implementation,
not claims about undocumented competitor internals.

## Delivery, review, and integration remain distinct

Delivery means that the exact result reached a durable consumer checkpoint. It
makes no claim about correctness. For large results it can mean a bounded preview
and retrieval reference, with `inline:false` recorded in the receipt. An inspector
read alone cannot establish delivery. A workflow step receipt proves delivery to
the script; a later failed report must identify that already-consumed research.

Review accepts the result of a task that explicitly asked for review. Editing
acceptance belongs to integration: changed contributions finish after confirmed
application, and unchanged contributions finish from immutable starting/submitted
proof. An accepted revision in a conflicted candidate remains an obligation.

A conflict retains the candidate and exact repair source. A distinct editing task
repairs that source; revision creates a successor and records the repair as a
contribution. Parent drift requires explicit refresh and validation when the merged
tree changes. Uncertain writes require reconciliation; cancellation never implies
rollback after writing begins. The
[integration sequence](../WORKFLOWS.md#integrating-editing-results) shows the normal
repair path and the [API](../API.md#integration-reference) defines the operations.

## Release and restoration are resource operations

Eligibility and filesystem proof are separate checks. Settled tasks, inactive
execution, no reservation, and no uncertain apply make a member eligible; unchanged
or integrated contents make removal safe. The runtime marks the release before
removing files and records retained contexts with reasons. A cleanup failure never
reopens a done task.

Task/execution snapshots retain the original source independently of the live member
pointer. Follow-ups restore it automatically when possible. Editing uses the completed
submitted snapshot so follow-up work builds on the previous result. Read-only work
keeps the original starting snapshot so earlier findings retain their meaning.
Non-Git live roots cannot supply historical contents. Forgotten or pruned snapshots
require an explicit alternative, with no silent fallback to current parent code.

Workflow validation copies remain explicitly owned resources. A workflow may release
its own idle copies and settled member workspaces without losing the conversation
or reservation. Automatic passes leave active workflow reservations alone. Retained
workspaces require inspection and explicit cleanup.

## Settlement owns correctness; presentation owns grouping

Derive coordination facts once. Settlement consumes all obligations in priority
order. Presentation groups those facts for the parent: one running workflow instead
of each internal member, one conflicted candidate instead of every contributor,
and delivery as work in progress rather than manual acceptance.

The harness handles its own bookkeeping: checkpointing mail, acknowledging delivered
successful reports, completing previously accepted unchanged work, and scheduling
release. Failed reports still need handling; deferral explicitly records the exact
work being set aside. No-op notice checks must remain read-only so a waiting parent
does not trigger its own wake on every scan.

Lock discipline is part of this contract. Application acquires the execution gate,
then task mutation serialization, then runtime Git. It never waits for the gate
while holding the launch mutex. Release requests schedule a coalesced worker;
callbacks do not run a release pass while holding scheduler locks. See
[AGENTS.md](../AGENTS.md#swarm-invariants) for the implementation rules.

## Completed stages

| Stage | Result in this revision |
| --- | --- |
| 1: Parent decisions | Shared facts, independent settlement/presentation, bounded status, decision-first tools and TUI. |
| 2: Completion and restoration | Immutable requirements, exact delivery receipts, linked follow-ups, independent workspace release, and format 2. |
| 3: Integration | One acceptance/integration operation, unchanged completion, retained halts, and idempotent receipt replays. |
| 4: Documentation | Task-first guide, API reference, lifecycle diagrams, agent guidance, example comments, and these repository pages. |

The operational checks for this design cover delivery in direct and scripted
research, explicit review, same-member follow-ups, unchanged and changed editing,
conflict repair, parent drift, uncertain apply recovery, and cleanup/restoration.
Implementation tests live with `swarm`, `workflow`, and `worktree`; documentation
validation must also check tool arguments, status strings, links, and examples
against the actual revision. A documentation change does not itself rerun or claim
new evidence from live model replays.

## Deferred proposals

These are proposals with no delivery promise and no additional current API:

- **Flatter JavaScript surface.** Consider reducing nested namespaces only if it
  makes normal coordination simpler while preserving explicit repair decisions and
  parent authority. The documented `polly.integration` and `polly.tasks` remain.
- **`polly.status`.** Consider a script-level decision view only with a concrete use
  case and defined consistency semantics. The current model tool is `swarm_read`.
- **PR delivery.** A remote destination needs a defined completion requirement,
  reviewed revision, and proof of integration. Creating a PR is not equivalent to
  applying a task to the local parent, and this design does not add automatic publishing.
- **Go workspace naming.** Renaming `ExecutionContext` can improve vocabulary but
  changes no lifecycle; the current record kind remains `context`.
- **Additional human integration controls.** A `/swarm integrate`
  command would need its own contract.
  The current normal surface is the parent integration tool or workflow API.
