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
// entry is one required command. The plan's finalChecks are the suites too
// slow or too environment-bound for every wave: no wave runs them, and an
// applied result hands them back for the parent to run once. A failing check
// is re-run on the commit the wave merged onto: one that already failed there
// the same way is a limit of the environment or the repository, not a
// regression this wave caused, and does not block. Parent authority comes
// from the host.
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
  finalChecks: arr(nonblank),
  tasks: arr(planTask, {minItems: 1}),
  docsUpdates: arr(str()),
  risks: arr(str()),
  openQuestions: arr(str()),
}, {required: planRequired});
const editorSchema = obj({summary: nonblank, filesChanged: arr(str()), notes: arr(str())});
const reviewSchema = obj({approved: bool(), feedback: str()});

// Names a test runner prints for one failing case, across the common
// formats: Go's "--- FAIL: Name (0.01s)", pytest's "FAILED path::case", TAP's
// "not ok N - case", the bullets Jest, Vitest and Mocha put before a failed
// title, and Vitest's " FAIL  file > case" summary. The name is the rest of
// the line less a trailing duration, so a title with spaces survives whole.
// Go's package line closes the cases above it and qualifies them, so one
// name in two packages stays two failures; a package line with no case above
// it (a panic, a timeout, a TestMain exit) or a build failure names the
// package itself. The set is a heuristic and can only make validation
// stricter: a name seen at the candidate but not at the baseline blocks, and
// a failure whose output names nothing recognisable is blocked as well,
// because nothing ties it to the baseline.
const failureMarker = /^\s*(?:---\s*FAIL:\s*|FAIL:\s*|FAIL\s{2,}|FAILED\s+|not ok\s+\d+\s*-?\s*|[\u2717\u2715\u00d7\u25cf\u276f]\s+)(.+?)\s*(?:\([^()]*\))?\s*$/;
const packageMarker = /^FAIL\t(\S+)(?:\s+(\[[^\]]+\]))?/;

function failureNames(text) {
  const names = new Set();
  let pending = [];
  for (const line of String(text || "").split("\n")) {
    const pkg = packageMarker.exec(line);
    if (pkg) {
      if (pkg[2]) names.add(pkg[1] + " " + pkg[2]);
      else if (!pending.length) names.add(pkg[1]);
      for (const name of pending) names.add(pkg[1] + ": " + name);
      pending = [];
      continue;
    }
    const match = failureMarker.exec(line);
    if (match) pending.push(match[1]);
  }
  for (const name of pending) names.add(name);
  return names;
}

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
    const result = await agent("integration repair " + repairs, "Repair this exact integration candidate. Resolve the supplied conflicts or validation failures while preserving all intended contributions. Use the structured conflicts and their base/ours/theirs commits and blob references (read-only git show) for binary and rename conflicts; do not rely on textual markers. Make source changes only in your assigned copy. A check carrying preexisting:true failed the same way on the commit this wave merged onto; it is not yours to fix, and changing project code to satisfy it is wrong. Do not commit or publish. Report what changed.", {
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
      // Names come from the whole output; the stored tail is for readers.
      return {command: checks[item], exitCode: result.exitCode, output: result.text.slice(-6000),
        failures: result.exitCode === 0 ? [] : [...failureNames(result.text)]};
    }, {concurrency: 4, errors: "collect"});
    const errors = rows.filter(row => !row.ok);
    if (errors.length) fail("Wave validation could not run", {candidate: candidate.id, commit: candidate.merged.commit, errors});
    const review = rows[0].value;
    const checkResults = rows.slice(1).map(row => row.value);
    await classify(checkResults);
    const result = {candidate: candidate.id, commit: candidate.merged.commit, review, checks: checkResults};
    validations.push(result);
    return {...result, passed: review.approved && checkResults.every(check => check.exitCode === 0 || check.preexisting)};
  }

  // Re-run only the failing commands on the commit this wave merged onto, and
  // mark the ones that fail there the same way as pre-existing. A wave whose
  // checks all pass never reaches this, so the comparison costs nothing until
  // something is already wrong.
  async function classify(checkResults) {
    const failed = checkResults.filter(check => check.exitCode !== 0);
    const baselineCommit = candidate.parent && candidate.parent.commit;
    if (!failed.length || !baselineCommit) return;
    const baseline = await ctx({commit: baselineCommit});
    temporary.push(baseline);
    const rows = await parallel(failed, async check => {
      const result = await exec(check.command, {context: baseline, check: false});
      return {exitCode: result.exitCode, failures: failureNames(result.text)};
    }, {concurrency: 4, errors: "collect"});
    rows.forEach((row, i) => {
      const check = failed[i];
      if (!row.ok) {
        check.baseline = {commit: baselineCommit, error: row.error};
        return;
      }
      check.baseline = {commit: baselineCommit, exitCode: row.value.exitCode};
      if (row.value.exitCode === 0) return;
      // Only a failure the output names, and names again at the baseline, is
      // pre-existing. Output naming nothing (a compile error, an unfamiliar
      // runner) cannot be attributed and blocks like any regression.
      const names = check.failures || [];
      check.baseline.newFailures = names.filter(name => !row.value.failures.has(name));
      check.preexisting = names.length > 0 && check.baseline.newFailures.length === 0;
    });
    const carried = failed.filter(check => check.preexisting).map(check => check.command);
    if (carried.length) await log("checks failing at the wave baseline too, not blocking: " + carried.join("; "));
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
  const applied = new Set();
  for (let w = 0; w < ordered.length; w++) {
    const wave = ordered[w];
    await log("wave " + (w + 1) + "/" + ordered.length + ": " + wave.map(t => t.id).join(", "));
    try {
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
      wave.forEach(t => applied.add(t.id));
    } catch (error) {
      // Earlier waves are already in the parent's files. Failing the run
      // would report landed work as lost and leave the caller to reconstruct
      // what is left by hand, so stop and hand back the remaining task ids
      // instead. A first wave that fails has nothing to protect and still
      // fails outright.
      if (!completed.length) throw error;
      const stopped = {wave: w + 1, tasks: wave.map(t => t.id), reason: error.message, code: error.code, details: error.result};
      if (error.result && error.result.code === "recovery_required") {
        // The wave's integrate began and its outcome is unknown: the parent
        // files may hold part of it, so its tasks are neither applied nor
        // safe to implement again. Only a reconciled candidate says which.
        await log("wave " + (w + 1) + " integrate needs recovery after " + completed.length + " applied wave(s)");
        return {status: "recovery_required", name: input.name, waves: completed, candidate: error.result.candidate, stopped};
      }
      const remaining = input.plan.tasks.filter(t => !applied.has(t.id));
      // The tasks left depend on tasks already applied, which a relaunch
      // would refuse as unknown, so the plan handed back keeps only the
      // dependencies among the remaining tasks.
      const plan = {...input.plan, tasks: remaining.map(t => ({...t, dependsOn: (t.dependsOn || []).filter(d => !applied.has(d))}))};
      await log("wave " + (w + 1) + " stopped after " + completed.length + " applied wave(s); " + remaining.length + " task(s) remain");
      return {status: "incomplete", name: input.name, waves: completed, remaining: remaining.map(t => t.id), plan, stopped};
    }
  }
  return {status: "applied", name: input.name, waves: completed, finalChecks: input.plan.finalChecks || []};
});
