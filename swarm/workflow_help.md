# Writing JavaScript workflows

This reference and its examples ship inside Polly; they need no Polly source checkout. Use scripts when branches, structured results, or repeated steps make them useful. Direct spawn_agent/swarm_wait calls remain sufficient for simple delegation.

Pass JavaScript source text in the `source` argument to workflow_start or workflow_run, and a JSON-encoded string in `input` (for example, `"{\"question\":\"Explain the cache\"}"`). These tools do not accept script paths. For a saved script, read it with an available file tool first. The human `/workflow SCRIPT.js INPUT.json` command reads files directly.

Define exactly one `polly.defineWorkflow({name, inputSchema, async run(input) {...}})`. All effects belong inside run. workflow_start returns an ID immediately; wait with swarm_wait and use workflow_read for saved source, input, steps, errors, or output. workflow_run waits for the result. Source and results are saved, but interrupted JavaScript is never automatically replayed.

The runtime supplies `polly`; do not import it. There are no Node.js, module, filesystem, process, network, or timer APIs. Use host tools through explicit execution contexts. Await all host operations, including log; use polly.parallel for independent work. Returning with pending operations fails the workflow. Schemas validate input before effects; return JSON-compatible output.

## API reference

All methods below are on `polly`. A `?` marks an optional argument, not literal JavaScript syntax.

| Method | Contract |
| --- | --- |
| `agent({task, label?, input?, schema?, tools?, model?, modelHost?, readOnly?, review?, source?, snapshot?, context?, session?})` | New agents require a 1–80 character label; task is the brief. Returns `{value, session, context, task, execution, revision, usage}`. `value` is validated against schema when supplied. |
| `followup({task, question, snapshot?, label?})` | Runs a linked follow-up on a completed task's member and returns an agent result. |
| `tasks.read(task)` | Current task, including `id`, `revision`, `status`, `requirement`, `result`, and submitted `snapshot` when present. |
| `tasks.review({task, revision, accept, feedback?})` | Accept the current revision of research requested with review:true, or request changes with feedback. Editing is accepted through integrate. |
| `context({source?, snapshot?, context?, readOnly?})` | Returns an opaque ID for a fresh isolated workspace. |
| `scope({context, label?}, async work => ...)` | Supplies context defaults to scoped work methods. `cwd` is refused; authority is unchanged. |
| `tool(name, args, {context})` | Runs an allowed tool in that context; returns `{text, data, artifacts, step}`. |
| `exec(command, {context, check?})` | Runs bash under context policy; returns a tool result plus `exitCode`. `check` defaults to true. |
| `snapshot(context)` | Captures an immutable `{id, commit, tree, source}`. Pass `.id` as another agent/context's snapshot. |
| `release(context)` | Releases an eligible inactive workspace you own. Dirty or still-needed copies remain retained. |
| `integrate({tasks?, candidate?, drift?})` | Supply either exact `tasks:[{task,revision}]` or an existing candidate ID. Returns `{status, candidate?, tasks, unchanged?, receipt?, next}`; success is `applied` or `done` for unchanged work. Drift (`paths` default or `tree`) applies only with tasks. |
| `integration.prepare({tasks:[{task,revision}], drift?})` | Builds a candidate with `id`, `merged.id`, `conflicts`, and `pending`. Review and check that exact merged snapshot before integrating. |
| `integration.read(id)` | Reads candidate details and the apply receipt. |
| `integration.revise(id, {task,revision})` | Creates a successor from a repair made against the candidate's merged snapshot. Validate the successor. |
| `integration.refresh(id)` | Refreshes against parent drift; returns candidate fields plus `changed`. Revalidate whenever changed is true. |
| `integration.accept(id)`, `integration.apply(id)` | Advanced stepwise operations; normal completion uses integrate. |
| `parallel(items, async (item, index) => ..., {concurrency?, errors?})` | Ordered `{ok:true,value}` / `{ok:false,error}` rows. Default concurrency 8 (1–256); errors defaults to `collect`. `throw_after_all` finishes all branches, then rejects if any failed. |
| `log(message)` | Awaitable progress entry saved in the report. |
| `fail(message, result?)` | Throws a workflow failure with optional structured partial results. |

`polly.schema` provides `string(options?)`, `number(options?)`, `integer(options?)`, `boolean()`, `enum(...values)`, `array(itemSchema, options?)`, `object(properties, options?)`, and `keyed(ids, valueSchema)`. Options are JSON Schema fields. Objects require every declared key and reject extra keys by default; override `required` explicitly for optional keys. keyed requires unique string IDs and exactly those result keys.

Scoped `work` exposes agent, followup, integrate, context, tool, exec, snapshot, release, and log. followup/integrate use explicit arguments rather than scope defaults. schema, parallel, tasks, integration, fail, and defineWorkflow remain on polly.

Use `readOnly` in JavaScript, versus `read_only` in spawn_agent arguments. Agent results are wrappers: read `result.value` for the answer and `result.task` for the task ID. Parallel results add another wrapper: `row.value` is the callback's result. Read the current task before supplying its revision to review or integration; the agent result's revision describes its execution result.

For a new member, source selects snapshot input and context selects the workspace to copy; snapshot selects an immutable snapshot. Agents run in their assigned directories. Continue an existing active obligation with `agent({session: prior.session, task: "follow-up brief"})`; it preserves the saved conversation and authority. Do not override source, snapshot, context, model, or tools on continuation. Use followup for new work after a task is complete. JavaScript cannot set maxIterations or grant budgets.

`exec(..., {check:false})` converts an ordinary nonzero process exit into data; check exitCode explicitly. Approval denial, sandbox setup failure, timeout, cancellation, and iteration exhaustion still reject. Errors can carry `code`, `message`, `result`, `session`, and `usage`. Preserve partial work and receipts; do not rerun an entire script to recover an uncertain apply. Inspect with workflow_read and swarm_integration. Reconciliation is a parent tool operation, not a JavaScript method.

Each exec call starts a fresh shell, even when reusing the same context. Changes made by `cd`, exports, shell variables, and shell options do not persist between calls. Repeat required directory and environment setup in each command, or source a setup file within that call. Use supplied writable scratch or temporary paths for tool caches and disposable build output. Treat sandbox permission failures as environment limits; do not change ownership, persistent user configuration, or project code to bypass them.

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
      label: "Reviewer", snapshot: candidate.merged.id, readOnly: true,
      task: "Review this exact candidate for the requested change and regressions.",
      input: {request: input.task},
      schema: s.object({approved: s.boolean(), feedback: s.string()}),
    });
    if (!review.value.approved) {
      polly.fail("Review needs changes", {candidate: candidate.id, review: review.value});
    }
    const context = await polly.context({snapshot: candidate.merged.id});
    for (const command of input.checks) {
      const result = await polly.exec(command, {context, check: false});
      if (result.exitCode !== 0) {
        polly.fail("Check failed", {candidate: candidate.id, context, command, result});
      }
    }
    const integration = await polly.integrate({candidate: candidate.id});
    return {integration, checksContext: context};
  },
});
```

Keep the check context ID for inspection or release it with polly.release when eligible. After a repair or changed refresh, review and run checks on the new merged snapshot before integration. Never assume previous validation covers a changed candidate.
