# Writing JavaScript workflows

This reference and its examples ship inside Polly; they need no Polly source checkout. Use scripts when branches, structured results, or repeated steps make them useful. Direct spawn_agent/wait_agent calls remain sufficient for simple delegation.

Pass JavaScript source text in the `source` argument to workflow_run, and a JSON-encoded string in `input` (for example, `"{\"question\":\"Explain the cache\"}"`). The tool does not accept script paths. For a saved script, read it with an available file tool first. The human `/workflow SCRIPT.js INPUT.json` command reads files directly.

Define exactly one `polly.defineWorkflow({name, inputSchema, async run(input) {...}})`. All effects belong inside run. workflow_run waits for the result by default. Set background:true to return an ID immediately, then wait with wait_agent. Use swarm_read({view:"workflows", id}) for saved source, input, steps, errors, or output. Cancel with swarm_control({action:"cancel_workflow", id}); acknowledge a terminal failure with action:"acknowledge_workflow" (defer:true requires a nonblank note). Source and results are saved, but interrupted JavaScript is never automatically replayed.

Foreground workflow_run returns `{id, status, output, steps, next, error?}` once in the tool result, with full large results attached. Background workflows deliver their terminal result in a completion notice. If a foreground result could not be saved, its notice remains available for recovery. Delivery acknowledges completed workflows only; a failed/interrupted result still requires acknowledgment or deferral after handling its unresolved work. Direct inspection never acknowledges delivery.

The runtime supplies `polly`; do not import it. There are no Node.js, module, filesystem, process, network, or timer APIs. Use host tools through explicit execution contexts. Await all host operations, including log; use polly.parallel for independent work. Returning with pending operations fails the workflow. Schemas validate input before effects; return JSON-compatible output.

## API reference

All methods below are on `polly`. A `?` marks an optional argument, not literal JavaScript syntax.

| Method | Contract |
| --- | --- |
| `agent({task, label?, input?, schema?, tools?, model?, modelHost?, readOnly?, review?, source?, commit?, context?, session?, taskID?})` | New agents require a 1–80 character label; task is the brief; taskID selects a precreated task for scheduler assignment. Returns `{value, session, context, task, execution, revision, usage}`. `value` is validated against schema when supplied. |
| `followup({task, question, commit?, label?})` | Runs a linked follow-up on a completed task's member and returns an agent result. |
| `tasks.create({description, criteria?, dependencies?, owner?, review?, requirement?})` | Returns a task. Completion requirement is fixed at creation: delivered (default for unowned research), reviewed, or applied. Use applied for unowned editing work. |
| `tasks.update({task, revision, owner, dependencies})` | Returns the updated task. Replace owner and dependencies; empty owner lets the scheduler assign a new agent. An active owner must finish or its owning workflow must be canceled first. Stale revisions, dependency cycles and incompatible owners are refused. |
| `tasks.read(task)` | Current task, including `id`, `revision`, `status`, `requirement`, `result`, and `baseCommit`/`resultCommit` when their captures are retained. |
| `tasks.review({task, revision, accept, feedback?})` | Accept the current revision of research requested with review:true, or request changes with feedback. Editing is accepted through integrate. |
| `context({source?, commit?, context?, readOnly?})` | Returns an opaque ID for a fresh isolated workspace. |
| `scope({context, label?}, async work => ...)` | Supplies context defaults to scoped work methods. `cwd` is refused; authority is unchanged. |
| `tool(name, args, {context})` | Runs an allowed tool in that context; returns `{text, data, artifacts, step}`. |
| `exec(command, {context, check?})` | Runs `bash -e -o pipefail -c` under context policy; returns a tool result plus `exitCode`. `check` defaults to true and rejects a nonzero final shell exit status. |
| `snapshot(context)` | Captures an immutable `{commit, tree, source}`. Pass `.commit` as another agent/context's commit. |
| `release(context)` | Releases an eligible inactive workspace you own. Dirty or still-needed copies remain retained. |
| `integrate({tasks?, candidate?, drift?})` | Supply either exact `tasks:[{task,revision}]` or an existing candidate ID. Returns `{status, candidate?, tasks, unchanged?, receipt?, next}`; success is `applied` or `done` for unchanged work. Drift (`paths` default or `tree`) applies only with tasks. |
| `integration.prepare({tasks:[{task,revision}], drift?})` | Builds a candidate with `id`, `merged.commit`, `conflicts`, `pending`, and `receipt`. Review and check that exact merged commit before integrating. |
| `integration.read(id)` | Returns the candidate, including `id`, `status`, `accepted`, `merged`, `plan`, and `receipt`. Read `candidate.receipt` directly. |
| `integration.revise(id, {task,revision})` | Creates a successor from a repair made against the candidate's merged commit. Validate the successor. |
| `integration.refresh(id)` | Refreshes against parent drift; returns candidate fields plus `changed`. Revalidate whenever changed is true. |
| `integration.reconcile(id)` | Observes an interrupted apply and returns its receipt; never reapplies a patch. Inspect the outcome before choosing an explicit retry or repair. |
| `integration.accept(id)`, `integration.apply(id)` | Advanced stepwise operations; normal completion uses integrate. |
| `parallel(items, async (item, index) => ..., {concurrency?, errors?})` | Ordered `{ok:true,value}` / `{ok:false,error}` rows. Default concurrency 8 (1–256); errors defaults to `collect`. `throw_after_all` finishes all branches, then rejects if any failed. |
| `log(message)` | Awaitable progress entry saved in the report. |
| `fail(message, result?)` | Throws a workflow failure with optional structured partial results. |

`polly.schema` provides `string(options?)`, `number(options?)`, `integer(options?)`, `boolean()`, `enum(...values)`, `array(itemSchema, options?)`, `object(properties, options?)`, and `keyed(ids, valueSchema)`. Options are JSON Schema fields. Objects require every declared key and reject extra keys by default; override `required` explicitly for optional keys. keyed requires unique string IDs and exactly those result keys.

Scoped `work` exposes agent, followup, integrate, context, tool, exec, snapshot, release, and log. followup/integrate use explicit arguments rather than scope defaults. schema, parallel, tasks, integration, fail, and defineWorkflow remain on polly.

Captured code is identified by full Git commit IDs retained in this runtime. Use `commit: candidate.merged.commit` to start a reviewer or repair; use the candidate's `id` only for integration operations. Branch names, abbreviated hashes, revision expressions, arbitrary local commits, and old `snapshot` arguments are rejected. Task results expose `baseCommit` and `resultCommit`; absent or forgotten captures omit these fields. `polly.snapshot(context)` still captures files automatically into an immutable commit. Captures are parentless; the assigned baseline commit identifies contents, not the original repository history.

Use optional `readOnly` in JavaScript, versus required boolean `read_only` in managed spawn_agent arguments. JavaScript and Go defaults are unchanged. Agent results are wrappers: read `result.value` for the answer and `result.task` for the task ID. Parallel results add another wrapper: `row.value` is the callback's result. Read the current task before supplying its revision to review or integration; the agent result's revision describes its execution result.

For a new member, source selects snapshot input and context selects the workspace to copy; commit selects a retained immutable Git commit. Agents run in their assigned directories. Continue an existing active obligation with `agent({session: prior.session, task: "follow-up brief"})`; it preserves the saved conversation and authority. Do not override source, commit, context, model, or tools on continuation. Use followup for new work after a task is complete. To run a task created beforehand, use agent({taskID: task.id, label:"Worker", task:task.description, readOnly:true}) with a compatible role, after its dependencies are done. The scheduler assigns it; no claim or submit call is needed. Successful final results are captured automatically; swarm_block leaves the task unresolved. JavaScript cannot set maxIterations or grant budgets.

`exec(..., {check:false})` converts an ordinary nonzero process exit into data after strict Bash execution; it does not disable `errexit` or `pipefail`. Check exitCode explicitly. Approval denial, sandbox setup failure, timeout, cancellation, and iteration exhaustion still reject. Errors can carry `code`, `message`, `result`, `session`, and `usage`. Preserve partial work and receipts; do not rerun an entire script to recover an uncertain apply. Inspect saved workflow evidence with swarm_read view=workflows. Use polly.integration.read/reconcile in a separate workflow to observe an uncertain apply; reconciliation never writes patches.

Each exec call starts a fresh shell, even when reusing the same context. Changes made by `cd`, exports, shell variables, and shell options do not persist between calls. Repeat required directory and environment setup in each command, or source a setup file within that call. Use supplied writable scratch or temporary paths for tool caches and disposable build output. Treat sandbox permission failures as environment limits; do not change ownership, persistent user configuration, or project code to bypass them.

Candidates always include `receipt`: null means no apply attempt is recorded for that candidate; an object records `status` and the apply plan. `applied` records success; `not_applied` means reconciliation found the patch unapplied; `applying` and `recovery_required` require inspection/reconciliation before a retry. A null receipt does not prove parent files are unchanged: inspect them separately. `integrate` returns its receipt as `result.receipt`; unchanged work may return `status:"done"` without a receipt because no application was needed. Do not invent `applyReceipt` or use fallback property names.

The runnable `examples/workflows/integration-evidence-exercise.js` deliberately creates conflicting edits, fails an exact content assertion on an incomplete repair, then reviews and checks a successor commit before explicit integration. Run it only in a disposable Git repository with the documented fixture. Validation evidence records the exact candidate commit; changing that commit requires fresh review and checks.

Run each required check in a separate awaited exec call. Strict Bash stops on unhandled command failures and makes a pipeline fail when any stage fails: `false; echo done` and `false | cat; echo done` stop before echo. Checking still sees the final shell status only, and conditional-list exceptions remain: `false && printf unreachable; printf later` succeeds. Propagate required failures explicitly (`required_check || exit $?`) in compound scripts. Handle expected failures with `if`, or use `set +e` to continue while collecting and checking statuses. `set +o pipefail` restores ordinary pipeline status for intentional early-reader termination such as `yes | head`; both overrides together restore the previous defaults for that invocation. External shell tools and separately launched scripts retain their own shell options. Use `go test -count=1` for mutation tests to avoid cached results.

Prefer portable `cmp` and `od` for exact content/byte checks; do not assume GNU flags such as cat -A or grep -P. For grep, exit 1 means no matches, while other nonzero exits are errors; simply negating grep also hides those errors.

## Parallel research example

Input: `{"questions":["How is the cache keyed?","How are failures retried?"]}`. Read-only research completes on delivery; no manual acceptance is needed.

```js
const s = polly.schema;
polly.defineWorkflow({
  name: "parallel-research",
  inputSchema: s.object({questions: s.array(s.string(), {minItems: 1})}),
  async run({questions}) {
    const rows = await polly.parallel(questions, async (question, i) => {
      const result = await polly.agent({
        label: "Research " + (i + 1), task: question, readOnly: true,
        schema: s.object({answer: s.string(), evidence: s.array(s.string())}),
      });
      return result.value;
    }, {concurrency: 4, errors: "throw_after_all"});
    return rows.map(row => row.value);
  },
});
```

## Edit, review, check, integrate example

Input: `{"task":"Fix the reported cache bug","checks":["go test ./..."]}`. Supply the actual repository-required checks. This example stops on conflicts, rejected review, failed checks, or apply uncertainty, leaving evidence for the parent to resolve. It does not silently repair or restart. For multiple editors, prepare all exact task revisions together and validate the combined candidate.

```js
const s = polly.schema;
polly.defineWorkflow({
  name: "edit-review-integrate",
  inputSchema: s.object({
    task: s.string(), checks: s.array(s.string(), {minItems: 1}),
  }),
  async run(input) {
    const editor = await polly.agent({label: "Editor", task: input.task});
    const task = await polly.tasks.read(editor.task);
    const candidate = await polly.integration.prepare({
      tasks: [{task: task.id, revision: task.revision}],
    });
    if ((candidate.conflicts || []).length || (candidate.pending || []).length) {
      polly.fail("Candidate needs repair", {candidate: candidate.id});
    }
    const review = await polly.agent({
      label: "Reviewer", commit: candidate.merged.commit, readOnly: true,
      task: "Compare this candidate against the supplied baseline commit and apply paths; inspect the requested change and regressions. HEAD is a parentless snapshot, so clean git status alone is not evidence of no changes.",
      input: {request: input.task, baseline: candidate.parent, candidate: candidate.merged, paths: candidate.plan.paths},
      schema: s.object({approved: s.boolean(), feedback: s.string()}),
    });
    if (!review.value.approved) {
      polly.fail("Review needs changes", {candidate: candidate.id, review: review.value});
    }
    const context = await polly.context({commit: candidate.merged.commit});
    const checks = [];
    for (const command of input.checks) {
      const result = await polly.exec(command, {context});
      checks.push({command, exitCode: result.exitCode, step: result.step});
    }
    const integration = await polly.integrate({candidate: candidate.id});
    return {integration, checksContext: context,
      validation: {commit: candidate.merged.commit, review: review.value, checks}};
  },
});
```

Keep the check context ID for inspection or release it with polly.release when eligible. Record the candidate commit with validation evidence. After a repair or changed refresh, review and run checks on the new merged commit before integration. Never assume previous validation covers a changed candidate.

## Dependencies and reassignment example

Input: `{"question":"How does the cache work?","feedback":"Verify the failure paths independently."}`. This example creates two tasks, completes the dependency, then requests a revision and assigns the reviewed task to a replacement. Acceptance remains explicit.

```js
// Input: {"question":"How does the cache work?","feedback":"Verify the failure paths independently."}
polly.defineWorkflow({
  name: "task-dependencies",
  inputSchema: polly.schema.object({question: polly.schema.string(), feedback: polly.schema.string()}),
  async run({question, feedback}) {
    const evidence = await polly.tasks.create({description: question});
    const assessment = await polly.tasks.create({
      description: "Assess the evidence and its limitations", review: true,
      dependencies: [evidence.id],
    });
    const research = await polly.agent({
      taskID: evidence.id, label: "Gather evidence", task: question, readOnly: true,
    });
    // Durable delivery completes the dependency before this launch.
    const first = await polly.agent({
      taskID: assessment.id, label: "Assess evidence", task: assessment.description,
      input: research.value, readOnly: true,
    });
    let task = await polly.tasks.read(first.task);
    await polly.tasks.review({task: task.id, revision: task.revision, accept: false, feedback});
    task = await polly.tasks.read(task.id);
    // Clear the inactive owner; the scheduler assigns the replacement agent.
    // An active owner must finish or its owning workflow must be canceled first.
    task = await polly.tasks.update({
      task: task.id, revision: task.revision, owner: "", dependencies: [evidence.id],
    });
    const replacement = await polly.agent({
      taskID: task.id, label: "Verify assessment", task: feedback,
      input: {evidence: research.value, assessment: first.value}, readOnly: true,
    });
    task = await polly.tasks.read(replacement.task);
    await polly.tasks.review({task: task.id, revision: task.revision, accept: true});
    return {research, first, replacement, task: await polly.tasks.read(task.id)};
  },
});
```

## Interrupted integration example

Input: `{"id":"candidate-or-apply-id"}`. Inspect the failed workflow with swarm_read first. Run this separately; it records observed outcomes and never retries the patch.

```js
// Input: {"id":"candidate-or-apply-id"}
// Run this separately after inspecting an interrupted workflow's saved report.
polly.defineWorkflow({
  name: "reconcile-integration",
  inputSchema: polly.schema.object({id: polly.schema.string()}),
  async run({id}) {
    await polly.integration.reconcile(id);
    const integration = await polly.integration.read(id);
    const receipt = integration.receipt;
    // Reconciliation observes the recorded plan's before/after states. It
    // never applies a patch. Inspect not_applied or recovery_required before
    // choosing an explicit retry or repair in a separate action.
    return {receipt, integration};
  },
});
```

Ordinary delegation uses named asynchronous spawn_agent({task_name,message,read_only}), send_message({target,message}) without idle startup, followup_task({target,message}) for explicit work, wait_agent({timeout_ms}), list_agents({path_prefix}), and interrupt_agent({target}). These follow Codex b979d4f1 with Polly workspace and task authority. Review feedback does not start an idle worker: explicitly continue with polly.agent({session,taskID,task}) inside its controlling workflow, reassign with polly.tasks.update, or use parent followup_task after the workflow settles. Existing polly.followup remains a completed-task convenience with optional commit refresh.

The parent model can use `followup_task({target,message,refresh:true})` after a worker's assignment is done and its execution is idle. Default follow-ups preserve its previous code; refresh captures current parent files and replaces only an eligible workspace, preserving conversation and authority. Non-Git research retains its saved live root with fresh scratch. Paused or pending work, live workflow reservations, uncertain integrations, retained workspaces and extra worker edits are refused. No acceptance or budget grant is implied. `send_message` changes information only; a new worker provides independent review.

The model tool returns `{member,message,operation,task,execution,baseOrigin,baseCommit?,source?,note}`; `message` is an ID, not an echo of the brief. `baseOrigin` is `parent`, `previous_result`, `original_baseline`, `existing_workspace` or `live_source`. Existing workspace contents can include edits beyond its baseline. Missing historical provenance is omitted. This is an additive model argument and a changed model result shape: JavaScript still uses `polly.followup({task,question,commit?})` to select an explicit retained commit and returns its existing agent result. It has no new `refresh` argument.


## Inspection and recovery details

`list_agents({path_prefix,details:true})` returns full worker state, budgets and context/task/execution IDs. Default listings summarize lifecycle and task disposition; an idle worker can still require acceptance or recovery. To release an eligible workspace, pass its `items[].context` to `swarm_control({action:"release",id})`. Release preserves the worker and its results. Use `swarm_read({view:"tasks",id,section:"details"})` for task lineage, delivery receipts, accepted revision and retained `baseCommit`/`resultCommit`; task summaries keep the current revision and completion requirement. JavaScript `polly.tasks.read` retains the full task view. All listings are bounded and paged; large selections attach their full content for read_artifact.

Tasks are created and successful final results are captured automatically. Ordinary research finishes through durable delivery; reviewed research requires acceptance of the submitted revision; editing requires integration or acceptance of immutable unchanged evidence. A failed execution or explicit blocker leaves unresolved work. Review rejection records feedback without starting an idle worker. Continuing an unaccepted submission reopens that task and invalidates its prior acceptance; settled work gets a linked task. Active workflow reservations must settle or be canceled before parent takeover.

`swarm_control({action:"cancel_task",id})` cancels an open assignment. Completed workflow reports need no manual acknowledgment. After reporting a terminal workflow failure, `swarm_control({action:"acknowledge_workflow",id,defer:true,note:"Reason for deferring"})` records deferral of its exact unresolved work without accepting, applying or canceling it. Preserve partial results and follow the report's next action.

Inside Git, source chooses snapshot input; workers use repository-relative paths in their assigned worktrees. Captures are parentless: include the original source commit in history-review briefs. Workers cannot write repository Git metadata. If Git setup is denied, report it; do not copy or repoint Git metadata, change ignore rules, or disable sandboxing to bypass the failure. Model calls inherit the host's allowance; keep findings on exhaustion and report the needed user-directed client grant.
