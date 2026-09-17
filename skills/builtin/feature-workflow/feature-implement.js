// Ships inside polly as part of the builtin feature-workflow skill; the skill
// passes this file's contents to workflow_run.
// Input: {"name":"kebab-case-feature","spec":"...","plan":{...},"source":"/repo",
//         "checks":["make test"]}
// Implements an approved feature plan (from feature-research.js, gated by the
// user). Editing workers run in dependency waves — parallel within a wave —
// and each wave is merged, reviewed by a single reviewer, checked, repaired
// within bounds, and integrated before the next wave starts. Checks come from
// the plan's checks (discovered during research); pass checks in the input to
// override. With no checks at all, validation is reviewer-only. Each checks
// entry is one required command. Parent authority comes from the host.
// Integration ends at working files: no commits, staging, or publishing.
const {obj, str, arr, int, bool, enum: senum, keyed} = polly.schema;
const nonblank = str({minLength: 1, pattern: "\\S"});
const {integration, context: ctx, exec, research, agent, parallel, fail, tasks, release, log} = polly;

const planTask = obj({
  id: nonblank,
  title: nonblank,
  brief: nonblank,
  paths: arr(str()),
  dependsOn: arr(str()),
  acceptance: arr(nonblank, {minItems: 1}),
});
const planRequired = ["summary", "tasks", "docsUpdates", "risks", "openQuestions"];
const planSchema = obj({
  summary: nonblank,
  checks: arr(nonblank),
  tasks: arr(planTask, {minItems: 1}),
  docsUpdates: arr(str()),
  risks: arr(str()),
  openQuestions: arr(str()),
}, {required: planRequired});
const editorSchema = obj({summary: nonblank, filesChanged: arr(str()), notes: arr(str())});
const reviewSchema = obj({approved: bool(), feedback: str()});

// Group plan tasks into dependency waves: a task starts only after every
// task it depends on has been integrated into the parent files.
function waves(planTasks) {
  keyed(planTasks.map(t => t.id), str()); // duplicate ids reject before any effect
  const known = new Set(planTasks.map(t => t.id));
  for (const t of planTasks) for (const d of t.dependsOn || []) {
    if (!known.has(d)) fail("Plan task depends on an unknown task", {task: t.id, dependsOn: d});
  }
  const done = new Set();
  const result = [];
  let remaining = [...planTasks];
  while (remaining.length) {
    const wave = remaining.filter(t => (t.dependsOn || []).every(d => done.has(d)));
    if (!wave.length) fail("Plan task dependencies have a cycle", remaining.map(t => t.id));
    result.push(wave);
    wave.forEach(t => done.add(t.id));
    remaining = remaining.filter(t => !done.has(t.id));
  }
  return result;
}

// Merge one wave's editing tasks, then review, check, repair (at most two
// repairs and one refresh), and integrate. Adapted from integrate-results.js.
async function integrateWave(input, refs, submissions, checks) {
  let candidate;
  let repairs = 0;
  let refreshes = 0;
  let validated = false;
  const temporary = [];
  const validations = [];

  async function repair(reason, evidence) {
    if (repairs >= 2) fail("Wave integration needs further repair", {candidate: candidate.id, reason, evidence});
    repairs += 1;
    const result = await agent("integration repair " + repairs, "Repair this exact integration candidate. Resolve the supplied conflicts or validation failures while preserving all intended contributions. Use the structured conflicts and their base/ours/theirs commits and blob references (read-only git show) for binary and rename conflicts; do not rely on textual markers. Make source changes only in your assigned copy. Do not commit or publish. Report what changed.", {
      commit: candidate.merged.commit,
      input: {candidate: candidate.id, reason, evidence, conflicts: candidate.conflicts},
      schema: obj({summary: str()}),
    });
    temporary.push(result.context);
    const task = await tasks.get(result.task);
    candidate = await integration.revise(candidate.id, {task: task.id, revision: task.revision});
    validated = false;
  }

  async function validate() {
    const rows = await parallel(["review", ...checks.map((_, i) => i)], async item => {
      if (item === "review") {
        const review = await research("wave reviewer", "Independently review this merged implementation candidate, including every contribution and repair. Your input contains the feature spec, the plan tasks being implemented, and each editor's own report; treat those reports as claims and verify against the code. Check the tasks' acceptance criteria and look for regressions, convention violations, and missed project requirements (docs, platform variants, tests). Return approved only when no required changes remain. You cannot edit or commit.", {
          commit: candidate.merged.commit,
          input: {candidate: candidate.id, spec: input.spec || "", submissions, instructions: input.reviewInstructions || ""},
          schema: reviewSchema,
        });
        temporary.push(review.context);
        return review.value;
      }
      const context = await ctx({commit: candidate.merged.commit});
      temporary.push(context);
      const result = await exec(checks[item], {context, check: false});
      return {command: checks[item], exitCode: result.exitCode, output: result.text.slice(-6000)};
    }, {concurrency: 4, errors: "collect"});
    const errors = rows.filter(row => !row.ok);
    if (errors.length) fail("Wave validation could not run", {candidate: candidate.id, commit: candidate.merged.commit, errors});
    const review = rows[0].value;
    const checkResults = rows.slice(1).map(row => row.value);
    const result = {candidate: candidate.id, commit: candidate.merged.commit, review, checks: checkResults};
    validations.push(result);
    return {...result, passed: review.approved && checkResults.every(check => check.exitCode === 0)};
  }

  try {
    candidate = await integration.prepare({tasks: refs, drift: input.drift || "paths"});
    for (;;) {
      if (candidate.conflicts && candidate.conflicts.length) {
        await repair("merge conflicts", candidate.conflicts);
        continue;
      }
      if (!validated) {
        const result = await validate();
        if (!result.passed) {
          await repair("review or check failures", result);
          continue;
        }
        validated = true;
      }
      let outcome;
      try {
        outcome = await polly.integrate({candidate: candidate.id});
      } catch (error) {
        if (error.code !== "parent_changed" || refreshes >= 1) throw error;
        refreshes += 1;
        const refreshed = await integration.refresh(candidate.id);
        candidate = refreshed;
        if (refreshed.changed) validated = false;
        continue;
      }
      const cleanup = await parallel([...new Set(temporary)], async context => {
        try { return await release(context); }
        catch (error) { return {retained: context, code: error.code, reason: error.message, details: error.result}; }
      }, {concurrency: 4, errors: "collect"});
      return {status: outcome.status, candidate: candidate.id, receipt: outcome.receipt, repairs, refreshes, validations,
        retained: cleanup.filter(row => !row.ok || row.value.retained).map(row => row.ok ? row.value : row.error)};
    }
  } catch (error) {
    fail("Wave integration blocked: " + error.message, {
      status: "blocked", candidate: candidate && candidate.id,
      code: error.code, details: error.result, repairs, refreshes, validations,
      retainedContexts: [...new Set(temporary)],
    });
  }
}

polly.workflow("feature-implement", obj({
  name: nonblank,
  spec: str(),
  plan: planSchema,
  source: str({minLength: 1}),
  checks: arr(nonblank),
  concurrency: int({minimum: 1}),
  drift: senum("paths", "tree"),
  reviewInstructions: str(),
}, {required: ["name", "plan"]}), async (input) => {
  const source = input.source ? {source: input.source} : {};
  const checks = input.checks && input.checks.length ? input.checks : (input.plan.checks || []);
  if (!checks.length) await log("no checks configured; validation is reviewer-only");
  const ordered = waves(input.plan.tasks);
  const completed = [];
  for (let w = 0; w < ordered.length; w++) {
    const wave = ordered[w];
    await log("wave " + (w + 1) + "/" + ordered.length + ": " + wave.map(t => t.id).join(", "));
    const rows = await parallel(wave, t => agent(
      ("implement " + t.id).slice(0, 80),
      "Implement your assigned task from the approved feature plan for '" + input.name + "'. Your input contains the plan summary, the feature spec, and your exact task with its brief, expected paths, and acceptance criteria. Work only in your assigned copy; sibling tasks are being implemented concurrently in their own copies and merged afterwards, so stay within your task's scope and do not fix unrelated issues. Follow the project's contributor documentation and imitate existing conventions. Run the relevant build and test commands yourself before finishing. Report a summary, the files you changed, and anything the reviewer should know. Do not commit or publish.",
      {...source, input: {name: input.name, spec: input.spec || "", planSummary: input.plan.summary, task: t}, schema: editorSchema},
    ), {concurrency: input.concurrency || 4, errors: "throw_after_all"});
    const refs = [];
    const submissions = [];
    for (let i = 0; i < rows.length; i++) {
      const task = await tasks.get(rows[i].value.task);
      refs.push({task: task.id, revision: task.revision});
      submissions.push({task: task.id, planTask: wave[i], report: rows[i].value.value});
    }
    const outcome = await integrateWave(input, refs, submissions, checks);
    completed.push({wave: w + 1, tasks: wave.map(t => t.id), submissions, integration: outcome});
  }
  return {status: "applied", name: input.name, waves: completed};
});
