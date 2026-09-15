# Writing JavaScript workflows

This reference and its examples ship inside Polly; they need no Polly source checkout. Use scripts when branches, structured results, or repeated steps make them useful. Direct spawn_agent/wait_agent calls remain sufficient for simple delegation.

Pass JavaScript source text in the `source` argument to workflow_run, and a JSON-encoded string in `input` (for example, `"{\"question\":\"Explain the cache\"}"`). The tool does not accept script paths. For a saved script, read it with an available file tool first. The human `/workflow SCRIPT.js INPUT.json` command reads files directly.

Define exactly one workflow with `polly.workflow(name, inputSchema, async run(input) {...})`; `polly.defineWorkflow({name, inputSchema, run})` is the same definition as one object. All effects belong inside run. workflow_run waits for the result by default. Set background:true to return an ID immediately, then wait with wait_agent. Use swarm_read({view:"workflows", id}) for saved source, input, steps, errors, or output. Cancel with swarm_control({action:"cancel_workflow", id}); acknowledge a terminal failure with action:"acknowledge_workflow" (defer:true requires a nonblank note). Source and results are saved, but interrupted JavaScript is never automatically replayed.

Foreground workflow_run returns `{id, status, output, steps, next, error?}` once in the tool result, with full large results attached. Background workflows deliver their terminal result in a completion notice. If a foreground result could not be saved, its notice remains available for recovery. Delivery acknowledges completed workflows only; a failed/interrupted result still requires acknowledgment or deferral after handling its unresolved work. Direct inspection never acknowledges delivery.

The runtime supplies `polly`; do not import it. There are no Node.js, module, filesystem, process, network, or timer APIs. Use host tools through explicit execution contexts. Await all host operations, including log; use polly.parallel for independent work. Returning with pending operations fails the workflow. Schemas validate input before effects; return JSON-compatible output.

## API reference

All methods below are on `polly`. A `?` marks an optional argument, not literal JavaScript syntax.

| Method | Contract |
| --- | --- |
| `workflow(name, inputSchema, run)` | Define the workflow. |
| `defineWorkflow({name, inputSchema, run})` | The same definition as one object. |
| `agent(label, task, options?)` or `agent({task, label?, input?, schema?, tools?, model?, modelHost?, readOnly?, review?, source?, commit?, context?, session?, taskID?})` | New agents require a 1–80 character label; task is the brief. `agent(label, task, options)` is equivalent to `agent({label, task, ...options})`. `taskID` selects a precreated task for scheduler assignment. Returns `{value, session, context, task, execution, revision, usage}`. `value` is validated against schema when supplied. |
| `research(label, task, options?)` | Agent with `readOnly` forced to true; accepts the object form too, and refuses an explicit `readOnly: false`. |
| `editor(source, label, task, options?)` | Agent with `source` forced to the given nonblank path; accepts the object form too, and refuses a conflicting `source` option. |
| `followup({task, question, commit?, label?})` | Runs a linked follow-up on a completed task's member and returns an agent result. |
| `tasks.create({description, criteria?, dependencies?, owner?, review?, requirement?})` | Returns a task. Completion requirement is fixed at creation: delivered (default for unowned research), reviewed, or applied. Use applied for unowned editing work. |
| `tasks.update({task, revision, owner, dependencies})` | Returns the updated task. Replace owner and dependencies; empty owner lets the scheduler assign a new agent. An active owner must finish or its owning workflow must be canceled first. Stale revisions, dependency cycles and incompatible owners are refused. |
| `tasks.read(task)` / `tasks.get(task)` | Current task, including `id`, `revision`, `status`, `requirement`, `result`, and `baseCommit`/`resultCommit` when their captures are retained. |
| `tasks.review({task, revision, accept, feedback?})` | Accept the current revision of research requested with review:true, or request changes with feedback. Editing is accepted through integrate. |
| `context({source?, commit?, context?, readOnly?})` | Returns an opaque ID for a fresh isolated workspace. |
| `scope({context, label?}, async work => ...)` | Supplies context defaults to scoped work methods. `cwd` is refused; authority is unchanged. |
| `tool(name, args, {context})` | Runs an allowed tool in that context; returns `{text, data, artifacts, step}`. |
| `exec(command, {context, check?})` | Runs `bash -o pipefail -c` under context policy; returns a tool result plus `exitCode`. `check` defaults to true and rejects a nonzero final shell exit status. A pipeline fails when any stage fails; errexit is off. |
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

`polly.schema` provides `string(options?)`, `number(options?)`, `integer(options?)`, `boolean()`, `enum(...values)`, `array(itemSchema, options?)`, `object(properties, options?)`, and `keyed(ids, valueSchema)`. `str`, `num`, `int`, `bool`, `arr`, and `obj` are the same functions under short names, and `polly.keyed` is `schema.keyed`. Options are JSON Schema fields. Objects require every declared key and reject extra keys by default; override `required` explicitly for optional keys. keyed requires unique string IDs and exactly those result keys.

Scoped `work` exposes agent, research, editor, followup, integrate, context, tool, exec, snapshot, release, and log. followup/integrate use explicit arguments rather than scope defaults. schema, keyed, parallel, tasks, integration, fail, workflow, and defineWorkflow remain on polly.

Captured code is identified by full Git commit IDs. New agents, contexts, and follow-up tasks accept a full commit ID in the source repository or a retained capture. Explicit local commits are validated and retained without changing their SHA or ancestry; omission captures eligible current files, including dirty and untracked files. Use `commit: candidate.merged.commit` to start a reviewer or repair; use the candidate's `id` only for integration operations. Branch names, abbreviated hashes, revision expressions, and old `snapshot` arguments are rejected. Task results expose `baseCommit` and `resultCommit`; absent or forgotten captures omit these fields. `polly.snapshot(context)` still captures files automatically into an immutable commit. Automatic captures are parentless; explicitly selected repository commits preserve their history. Publications still reference retained captures.

Use optional `readOnly` in JavaScript, versus required boolean `read_only` in managed spawn_agent arguments. JavaScript and Go defaults are unchanged. Agent results are wrappers: read `result.value` for the answer and `result.task` for the task ID. Parallel results add another wrapper: `row.value` is the callback's result. Read the current task before supplying its revision to review or integration; the agent result's revision describes its execution result.

For a new member, source selects snapshot input and context selects the workspace to copy; commit selects an immutable Git commit from that repository or a retained capture. Agents run in their assigned directories. Continue an existing active obligation with `agent({session: prior.session, task: "follow-up brief"})`; it preserves the saved conversation and authority. Do not override source, commit, context, model, or tools on continuation. Use followup for new work after a task is complete. To run a task created beforehand, use agent({taskID: task.id, label:"Worker", task:task.description, readOnly:true}) with a compatible role, after its dependencies are done. The scheduler assigns it; no claim or submit call is needed. Successful final results are captured automatically; swarm_block leaves the task unresolved. JavaScript cannot set maxIterations or grant budgets.

`exec(..., {check:false})` converts an ordinary nonzero process exit into data; it does not change shell options. Check exitCode explicitly. Approval denial, sandbox setup failure, timeout, cancellation, and iteration exhaustion still reject. Errors can carry `code`, `message`, `result`, `session`, and `usage`; branch on `code` (see the table below) and rethrow anything you do not handle. Preserve partial work and receipts; do not rerun an entire script to recover an uncertain apply. Inspect saved workflow evidence with swarm_read view=workflows. Use polly.integration.read/reconcile in a separate workflow to observe an uncertain apply; reconciliation never writes patches.

Rejections carry a stable `code`. Handle the expected ones explicitly, for example `try { await polly.integrate({candidate}) } catch (error) { if (error.code !== "parent_changed") throw error; ... }`, and let everything else propagate.

| Code | Meaning and next step |
| --- | --- |
| `command_failed` | exec with `check:true`, or a command tool, exited nonzero; `result.exitCode` holds the status. `tool_failed` covers other tool errors. |
| `tool_denied` | The tool is unavailable in that context or its approval was denied. |
| `conflicts` | integrate halted on merge conflicts; `result` is the candidate. Repair from `merged.commit`, then `integration.revise`. |
| `parent_changed` | Parent files moved since the candidate was prepared. `integration.refresh`, revalidate when `changed`, then integrate again. |
| `recovery_required` | An earlier apply has an unconfirmed outcome. `integration.reconcile` it before any further integration. |
| `stale_task` | The supplied revision is not the task's current submitted revision, or the task is already done. Read the task again. |
| `superseded`, `already_applied`, `not_accepted` | The candidate was revised, refreshed or applied already, or stepwise apply ran before accept. Use the current candidate. |
| `invalid_repair` | A revise repair did not start from the exact candidate commit or reused a task. |
| `session_busy`, `context_busy` | The member or workspace still has an active workflow, agent, tool, refresh or release. Wait for it to settle. |
| `unintegrated_changes`, `workspace_retained` | Release or refresh refused because the workspace holds edits that were never integrated; it stays retained. Integrate or accept them first. |
| `requirement_mismatch` | The task's completion requirement does not fit the agent's role, or a follow-up tried to change it. |
| `blocked` | The task has an unresolved integration; reconcile it first. |
| `invalid_commit`, `unknown_commit` | Not a full retained or repository commit ID. |
| `unknown_task`, `unknown_candidate`, `unknown_context`, `unknown_member`, `invalid_args` | A bad ID or argument; nothing ran. |
| `execution_budget` | The JavaScript or host-call budget is exhausted; the workflow ends. |
| `canceled`, `timeout`, `host_failed` | Cancellation, a deadline, or any other host error. `workflow_failed` is `polly.fail` or an untyped throw. |

Each exec call starts a fresh shell, even when reusing the same context. Changes made by `cd`, exports, shell variables, and shell options do not persist between calls. Repeat required directory and environment setup in each command, or source a setup file within that call. Use supplied writable scratch or temporary paths for tool caches and disposable build output. Treat sandbox permission failures as environment limits; do not change ownership, persistent user configuration, or project code to bypass them.

Candidates always include `receipt`: null means no apply attempt is recorded for that candidate; an object records `status` and the apply plan. `applied` records success; `not_applied` means reconciliation found the patch unapplied; `applying` and `recovery_required` require inspection/reconciliation before a retry. A null receipt does not prove parent files are unchanged: inspect them separately. `integrate` returns its receipt as `result.receipt`; unchanged work may return `status:"done"` without a receipt because no application was needed. Do not invent `applyReceipt` or use fallback property names.

Validation evidence names the exact merged commit it passed on; a revised or refreshed candidate is a new commit and needs fresh review and checks. Assert file contents with `cmp` against exact bytes instead of trusting editor reports: an incomplete repair must fail that assertion, and only a successor that passes is integrated. Before integrating, re-read the candidate and confirm `merged.commit` is the validated commit and `receipt` is still null; afterwards confirm `receipt.status` is `applied`. Never let an apply attempt precede successful validation.

Run each required check in a separate awaited exec call. Exec enables `pipefail`, so `go test ./... | tail` fails when `go test` fails, and `yes | head` exits 141; use `set +o pipefail` for intentional early-reader termination. Errexit stays off, so checking sees the final shell status: `false; printf later` succeeds. Enable `set -e` when every command must succeed; conditional-list exceptions remain: `false && printf unreachable; printf later` succeeds. Propagate required failures explicitly (`required_check || exit $?`) in compound scripts. Handle expected failures with `if`. External shell tools and separately launched scripts retain their own shell options. Use `go test -count=1` for mutation tests to avoid cached results.

Prefer portable `cmp` and `od` for exact content/byte checks; do not assume GNU flags such as cat -A or grep -P. For grep, exit 1 means no matches, while other nonzero exits are errors; simply negating grep also hides those errors.

## API tour example

Input: `{"files":["README.md","go.mod"]}`. One pass touches most of the API: scoped exec, tool and snapshot; parallel readers on one captured commit; a reviewed task with dependencies and a keyed schema; a follow-up; optional integration; and release. Snapshot requires a Git repository. Adding `"edit"` applies that change to parent files, so try it in a disposable checkout. The focused examples below cover review before apply, reassignment and recovery.

```js
const {obj, str, arr, int, enum: senum, keyed} = polly.schema;
const quote = text => "'" + text.replace(/'/g, "'\\''") + "'";
polly.workflow("api-tour", obj({
  files: arr(str({minLength: 1}), {minItems: 1, uniqueItems: true}),
  edit: str(),
}, {required: ["files"]}), async ({files, edit}) => {
  await polly.log("Touring " + files.join(", "));
  const context = await polly.context({readOnly: true});
  const baseline = await polly.scope({context, label: "Baseline"}, async work => {
    const sizes = await work.exec("wc -l -- " + files.map(quote).join(" "), {check: false});
    if (sizes.exitCode !== 0) polly.fail("Missing input files", sizes);
    const excerpt = await work.tool("read_file", {path: files[0], limit: 20});
    return {capture: await work.snapshot(), sizes: sizes.text, excerpt: excerpt.text};
  });
  // The capture outlives its workspace, so release the checkout before any agent runs.
  await polly.release(context);
  const rows = await polly.parallel(files, path => polly.research("Read " + path, "Summarize " + path, {
    commit: baseline.capture.commit,
    schema: obj({summary: str(), risk: senum("low", "medium", "high"), lines: int({minimum: 0})}),
  }), {concurrency: 4});
  if (rows.some(row => !row.ok)) polly.fail("A reader failed", rows);
  const readers = rows.map(row => row.value);
  const rank = await polly.tasks.create({
    description: "Rank the files by risk", criteria: "One verdict per file", review: true,
    dependencies: readers.map(reader => reader.task),
  });
  const ranking = await polly.research("Rank", rank.description, {
    taskID: rank.id, schema: keyed(files, str()),
    input: {sizes: baseline.sizes, excerpt: baseline.excerpt, summaries: readers.map(reader => reader.value)},
  });
  const submitted = await polly.tasks.get(ranking.task);
  const accepted = await polly.tasks.review({task: submitted.id, revision: submitted.revision, accept: true});
  // A follow-up keeps its original's requirement: delivered research needs no review.
  const detail = await polly.followup({task: readers[0].task, question: "Quote the riskiest line."});
  let integration = null;
  if (edit) {
    const editor = await polly.agent({label: "Editor", task: edit, input: ranking.value});
    const edited = await polly.tasks.read(editor.task);
    integration = await polly.integrate({tasks: [{task: edited.id, revision: edited.revision}]});
    if (integration.status !== "applied" && integration.status !== "done") polly.fail("Edit not integrated", integration);
  }
  return {commit: baseline.capture.commit, ranking: ranking.value, status: accepted.status, detail: detail.value, integration};
});
```

## Parallel research example

Input: `{"questions":["How is the cache keyed?","How are failures retried?"]}`. Read-only research completes on delivery; no manual acceptance is needed.

```js
const {obj, str, arr} = polly.schema;
polly.workflow("parallel-research", obj({questions: arr(str(), {minItems: 1})}), async ({questions}) => {
  const rows = await polly.parallel(questions, async (question, i) => {
    const result = await polly.research("Research " + (i + 1), question, {
      schema: obj({answer: str(), evidence: arr(str())}),
    });
    return result.value;
  }, {concurrency: 4, errors: "throw_after_all"});
  return rows.map(row => row.value);
});
```

## Fix, verify, one repair pass example

Input: `{"source":"/repo","findings":[{"id":"cache-key","summary":"Cache key ignores the host"}],"checks":["go test ./..."]}`. One editor fixes every finding in an isolated copy; an independent verifier and the checks run in parallel against the exact candidate commit. A failed verification gets one repair pass that continues the same session, then verification repeats. The parent gets a structured summary either way.

```js
const {obj, str, arr, enum: senum, keyed} = polly.schema;
const fix = obj({status: senum("fixed", "skipped"), what: str()});
const verdict = obj({verdict: senum("fixed", "not_fixed", "regression"), reasoning: str()});
polly.workflow("fix-and-verify", obj({
  source: str({minLength: 1}),
  findings: arr(obj({id: str({minLength: 1}), summary: str()}), {minItems: 1}),
  checks: arr(str({minLength: 1}), {minItems: 1}),
}), async ({source, findings, checks}) => {
  const ids = findings.map(f => f.id);
  keyed(ids, fix); // duplicate finding ids reject before any editor launches
  const worker = await polly.editor(source, "Fixer", "Fix every supplied finding in your assigned isolated copy. Read the repository instructions, add focused regression tests, and preserve unrelated work. Do not commit. Report every finding id, including anything skipped.", {
    input: {findings}, schema: keyed(ids, fix),
  });
  async function verify(reports) {
    // Evidence names the exact candidate commit; a later commit needs fresh verification.
    const candidate = await polly.snapshot(worker.context);
    const checkContext = await polly.context({commit: candidate.commit});
    const rows = await polly.parallel(["review", "checks"], async kind => {
      if (kind === "review") return polly.research("Verifier", "Independently verify EVERY finding in the candidate. Treat fix reports as claims: trace each failure scenario and whether its regression test detects it. Return a verdict for every key. You cannot edit or commit.", {
        commit: candidate.commit, input: {findings, reports}, schema: keyed(ids, verdict),
      });
      return polly.scope({context: checkContext, label: "checks"}, async work => {
        const failed = [];
        for (const command of checks) {
          const result = await work.exec(command, {check: false});
          if (result.exitCode !== 0) failed.push({command, exitCode: result.exitCode, step: result.step, output: result.text.slice(-6000)});
        }
        return failed;
      });
    }, {concurrency: 2, errors: "throw_after_all"});
    try { await polly.release(checkContext); } catch (error) { await polly.log("Check workspace retained: " + error.message); }
    const review = rows[0].value;
    const rejected = Object.entries(review.value).filter(([, v]) => v.verdict !== "fixed").map(([id, v]) => ({id, ...v}));
    const failedChecks = rows[1].value;
    return {commit: candidate.commit, reviewTask: review.task, rejected, failedChecks, passed: !rejected.length && !failedChecks.length};
  }
  const reports = [worker.value];
  let verification = await verify(reports);
  if (!verification.passed) {
    // The repair continues the same session: the worker keeps its conversation and workspace, and its task reopens at a new revision.
    const repaired = await polly.agent({session: worker.session, task: "Repair the rejected findings and attributable check failures. Preserve accepted fixes and unrelated files. Do not commit. This is the only repair pass.",
      input: {rejected: verification.rejected, failedChecks: verification.failedChecks}, schema: keyed(ids, fix)});
    reports.push(repaired.value);
    verification = await verify(reports);
  }
  const summary = {task: worker.task, session: worker.session, reports, ...verification};
  if (!verification.passed) polly.fail("Findings or checks remain unresolved", summary);
  const task = await polly.tasks.get(worker.task);
  return {...summary, integration: await polly.integrate({tasks: [{task: task.id, revision: task.revision}]})};
});
```

## Combine, repair, refresh, integrate example

Input: `{"tasks":[{"task":"ID","revision":3},{"task":"ID2","revision":1}],"checks":["go test ./..."]}`. Several editing results become one candidate. Conflicts and failed validation get up to two repairs, each revised from the exact merged commit. A parent that moved during validation is refreshed once and revalidated when the merge changed. Temporary workspaces are released at the end, and anything retained is reported rather than hidden.

```js
const {obj, str, arr, int, bool, enum: senum} = polly.schema;
polly.workflow("integrate-results", obj({
  tasks: arr(obj({task: str({minLength: 1}), revision: int({minimum: 1})}), {minItems: 1}),
  checks: arr(str({minLength: 1})),
  drift: senum("paths", "tree"),
}, {required: ["tasks", "checks"]}), async ({tasks, checks, drift}) => {
  const temporary = [];
  let candidate = await polly.integration.prepare({tasks, drift: drift || "paths"});
  let repairs = 0;
  let refreshes = 0;
  let validated = false;
  async function repair(reason, evidence) {
    if (repairs >= 2) polly.fail("Integration needs further repair", {candidate: candidate.id, reason, evidence});
    repairs += 1;
    // A repair starts from the exact merged commit; revise adopts its task as the successor candidate.
    const result = await polly.agent("Repair " + repairs, "Repair this exact integration candidate: resolve the supplied conflicts or validation failures while preserving every contribution. Use the structured conflicts and their base/ours/theirs references, not textual markers. Do not commit. Report what changed.", {
      commit: candidate.merged.commit, input: {reason, evidence, conflicts: candidate.conflicts}, schema: obj({summary: str()}),
    });
    temporary.push(result.context);
    const task = await polly.tasks.get(result.task);
    candidate = await polly.integration.revise(candidate.id, {task: task.id, revision: task.revision});
    validated = false;
  }
  async function validate() {
    const review = await polly.research("Reviewer", "Independently review this combined candidate, including every contribution and repair. HEAD is a parentless snapshot, so clean git status alone is not evidence. Return approved only when no required changes remain.", {
      commit: candidate.merged.commit, input: {candidate: candidate.id, inputs: candidate.inputs, repairs: candidate.repairs}, schema: obj({approved: bool(), feedback: str()}),
    });
    temporary.push(review.context);
    const context = await polly.context({commit: candidate.merged.commit});
    temporary.push(context);
    const failed = [];
    for (const command of checks) {
      const result = await polly.exec(command, {context, check: false});
      if (result.exitCode !== 0) failed.push({command, exitCode: result.exitCode, step: result.step, output: result.text.slice(-6000)});
    }
    return {commit: candidate.merged.commit, review: review.value, failed, passed: review.value.approved && !failed.length};
  }
  for (;;) {
    if ((candidate.conflicts || []).length) { await repair("merge conflicts", candidate.conflicts); continue; }
    if (!validated) {
      const result = await validate();
      if (!result.passed) { await repair("review or check failures", result); continue; }
      validated = true;
    }
    let outcome;
    try {
      outcome = await polly.integrate({candidate: candidate.id});
    } catch (error) {
      // Parent files moved since preparation: refresh once, then revalidate when the merge changed.
      if (error.code !== "parent_changed" || refreshes >= 1) throw error;
      refreshes += 1;
      candidate = await polly.integration.refresh(candidate.id);
      if (candidate.changed) validated = false;
      continue;
    }
    const retained = [];
    for (const context of new Set(temporary)) {
      try { await polly.release(context); }
      catch (error) { retained.push({context, code: error.code, reason: error.message}); }
    }
    return {status: outcome.status, candidate: candidate.id, receipt: outcome.receipt, repairs, refreshes, retained};
  }
});
```

Patterns that scale these examples up:

- Many parallel reviewers that report ids: namespace each id by its source (`diff + "/" + lens + "/" + id`) before merging, build the keyed schema once from the merged list so duplicates reject before the next agent starts, and use `obj({})` when the list is empty. A review pipeline is independent reviewers, then one judge that treats every finding as a claim and accounts for every id exactly once, then one auditor of the judge; each is a separate research agent with its own schema.
- Restrict a worker with `tools: ["read_file", "write_file"]`. `source` points a worker or context at another repository; `context({context})` copies an existing workspace.
- Editing work that no agent owns yet: `tasks.create({description, requirement: "applied"})`, then `agent({taskID: task.id, label, task})` with an editing role once its dependencies are done.
- Group work by source, refuse duplicate sources or paths up front, and run groups with `parallel(groups, fn, {errors: "throw_after_all"})` so every group finishes before the failure surfaces.
- Every `polly.fail` carries the task, session, and commit the parent needs to accept, integrate, or continue the work without rerunning the script.

## Dependencies and reassignment example

Input: `{"question":"How does the cache work?","feedback":"Verify the failure paths independently."}`. This example creates two tasks, completes the dependency, then requests a revision and assigns the reviewed task to a replacement. Acceptance remains explicit.

```js
// Input: {"question":"How does the cache work?","feedback":"Verify the failure paths independently."}
const {obj, str} = polly.schema;
polly.workflow("task-dependencies", obj({question: str(), feedback: str()}), async ({question, feedback}) => {
  const evidence = await polly.tasks.create({description: question});
  const assessment = await polly.tasks.create({
    description: "Assess the evidence and its limitations", review: true,
    dependencies: [evidence.id],
  });
  const research = await polly.research("Gather evidence", question, {taskID: evidence.id});
  // Durable delivery completes the dependency before this launch.
  const first = await polly.research("Assess evidence", assessment.description, {
    taskID: assessment.id, input: research.value,
  });
  let {id, revision} = await polly.tasks.get(first.task);
  await polly.tasks.review({task: id, revision, accept: false, feedback});
  let task = await polly.tasks.get(id);
  // Clear the inactive owner; the scheduler assigns the replacement agent.
  // An active owner must finish or its owning workflow must be canceled first.
  task = await polly.tasks.update({
    task: task.id, revision: task.revision, owner: "", dependencies: [evidence.id],
  });
  const replacement = await polly.research("Verify assessment", feedback, {
    taskID: task.id, input: {evidence: research.value, assessment: first.value},
  });
  task = await polly.tasks.get(replacement.task);
  await polly.tasks.review({task: task.id, revision: task.revision, accept: true});
  return {research, first, replacement, task: await polly.tasks.get(task.id)};
});
```

## Interrupted integration example

Input: `{"id":"candidate-or-apply-id"}`. Inspect the failed workflow with swarm_read first. Run this separately; it records observed outcomes and never retries the patch.

```js
// Input: {"id":"candidate-or-apply-id"}
// Run this separately after inspecting an interrupted workflow's saved report.
const {obj, str} = polly.schema;
polly.workflow("reconcile-integration", obj({id: str()}), async ({id}) => {
  await polly.integration.reconcile(id);
  const integration = await polly.integration.read(id);
  const receipt = integration.receipt;
  // Reconciliation observes the recorded plan's before/after states. It
  // never applies a patch. Inspect not_applied or recovery_required before
  // choosing an explicit retry or repair in a separate action.
  return {receipt, integration};
});
```

Ordinary delegation uses named asynchronous spawn_agent({task_name,message,read_only}), send_message({target,message}) without idle startup, followup_task({target,message}) for explicit work, wait_agent({timeout_ms}), list_agents({path_prefix}), and interrupt_agent({target}). These follow Codex b979d4f1 with Polly workspace and task authority. Review feedback does not start an idle worker: explicitly continue with polly.agent({session,taskID,task}) inside its controlling workflow, reassign with polly.tasks.update, or use parent followup_task after the workflow settles. Existing polly.followup remains a completed-task convenience with optional commit refresh.

The parent model can use `followup_task({target,message,refresh:true})` after a worker's assignment is done and its execution is idle. Default follow-ups preserve its previous code; refresh captures current parent files and replaces only an eligible workspace, preserving conversation and authority. Non-Git research retains its saved live root with fresh scratch. Paused or pending work, live workflow reservations, uncertain integrations, retained workspaces and extra worker edits are refused. No acceptance or budget grant is implied. `send_message` changes information only; a new worker provides independent review.

The model tool returns `{member,message,operation,task,execution,baseOrigin,baseCommit?,source?,note}`; `message` is an ID, not an echo of the brief. `baseOrigin` is `parent`, `previous_result`, `original_baseline`, `existing_workspace` or `live_source`. Existing workspace contents can include edits beyond its baseline. Missing historical provenance is omitted. This is an additive model argument and a changed model result shape: JavaScript still uses `polly.followup({task,question,commit?})` to select an explicit repository commit or retained capture and returns its existing agent result. It has no new `refresh` argument.


## Inspection and recovery details

`list_agents({path_prefix,details:true})` returns full worker state, budgets and context/task/execution IDs. Default listings summarize lifecycle and task disposition; an idle worker can still require acceptance or recovery. To release an eligible workspace, pass its `items[].context` to `swarm_control({action:"release",id})`. Release preserves the worker and its results. Use `swarm_read({view:"tasks",id,section:"details"})` for task lineage, delivery receipts, accepted revision and retained `baseCommit`/`resultCommit`; task summaries keep the current revision and completion requirement. JavaScript `polly.tasks.read` retains the full task view. All listings are bounded and paged; large selections attach their full content for read_artifact.

Tasks are created and successful final results are captured automatically. Ordinary research finishes through durable delivery; reviewed research requires acceptance of the submitted revision; editing requires integration or acceptance of immutable unchanged evidence. A failed execution or explicit blocker leaves unresolved work. Review rejection records feedback without starting an idle worker. Continuing an unaccepted submission reopens that task and invalidates its prior acceptance; settled work gets a linked task. Active workflow reservations must settle or be canceled before parent takeover.

`swarm_control({action:"cancel_task",id})` cancels an open assignment. Completed workflow reports need no manual acknowledgment. After reporting a terminal workflow failure, `swarm_control({action:"acknowledge_workflow",id,defer:true,note:"Reason for deferring"})` records deferral of its exact unresolved work without accepting, applying or canceling it. Preserve partial results and follow the report's next action.

Inside Git, source chooses snapshot input; workers use repository-relative paths in their assigned worktrees. Automatic captures are parentless: include the original source commit in history-review briefs, or select that full commit explicitly. Workers cannot write repository Git metadata. If Git setup is denied, report it; do not copy or repoint Git metadata, change ignore rules, or disable sandboxing to bypass the failure. Model calls inherit the host's allowance; keep findings on exhaustion and report the needed user-directed client grant.
