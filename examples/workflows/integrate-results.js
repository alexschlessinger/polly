// /workflow /absolute/path/integrate-results.js /absolute/path/input.json
// Input: {tasks:[{task:"ID",revision:3}],checks:["go test ./..."],drift:"paths"}
// Review/check each changed candidate, then finish with polly.integrate.
// Each checks entry is one required command. exec enables pipefail; propagate
// compound failures explicitly. Do not assume GNU flags.
// Parent authority comes from the host. No commits, publishing, or JS replay.
const {obj, str, arr, int, bool, enum: senum} = polly.schema;
const taskRef = obj({task: str({minLength: 1}), revision: int({minimum: 1})});
const reviewSchema = obj({approved: bool(), feedback: str()});

const {integrate, integration, context: ctx, exec, research, parallel, fail, tasks: t, release, agent} = polly;

polly.workflow("integrate-results", obj({
  tasks: arr(taskRef, {minItems: 1}),
  checks: arr(str({minLength: 1})),
  drift: senum("paths", "tree"),
  reviewInstructions: str(),
}, {required: ["tasks", "checks"]}), async (input) => {
  let candidate;
  let repairs = 0;
  let refreshes = 0;
  let validated = false;
  const temporary = [];
  const validations = [];

  async function repair(reason, evidence) {
    if (repairs >= 2) fail("Integration needs further repair", {candidate: candidate.id, reason, evidence});
    repairs += 1;
    const result = await agent("integration repair " + repairs, "Repair this exact integration candidate. Resolve the supplied conflicts or validation failures while preserving all intended contributions. Use the structured conflicts and their base/ours/theirs commits and blob references (read-only git show) for binary and rename conflicts; do not rely on textual markers. Make source changes only in your assigned copy. Do not commit or publish. Report what changed.", {
      commit: candidate.merged.commit,
      input: {candidate: candidate.id, reason, evidence, conflicts: candidate.conflicts},
      schema: obj({summary: str()}),
    });
    temporary.push(result.context);
    const task = await t.get(result.task);
    candidate = await integration.revise(candidate.id, {task: task.id, revision: task.revision});
    validated = false;
  }

  async function validate() {
    const taskRows = await parallel([...candidate.inputs, ...(candidate.repairs || [])],
      input => t.get(input.task), {concurrency: 4, errors: "throw_after_all"});
    const submissions = taskRows.map(row => row.value);
    const rows = await parallel(["review", ...input.checks.map((_, i) => i)], async item => {
      if (item === "review") {
        const review = await research("integration reviewer", "Independently review this combined candidate, including every contribution and repair. Check the requested acceptance criteria and regressions. Treat previous reports as claims. Return approved only when no required changes remain. You cannot edit or commit.", {
          commit: candidate.merged.commit,
          input: {candidate: candidate.id, submissions, inputs: candidate.inputs, repairs: candidate.repairs, instructions: input.reviewInstructions || ""},
          schema: reviewSchema,
        });
        temporary.push(review.context);
        return review.value;
      }
      const context = await ctx({commit: candidate.merged.commit});
      temporary.push(context);
      const command = input.checks[item];
      const result = await exec(command, {context, check: false});
      return {command, context, exitCode: result.exitCode, output: result.text.slice(-6000)};
    }, {concurrency: 4, errors: "collect"});
    const errors = rows.filter(row => !row.ok);
    if (errors.length) fail("Validation could not run", {candidate: candidate.id, commit: candidate.merged.commit, errors});
    const review = rows[0].value;
    const checks = rows.slice(1).map(row => row.value);
    const result = {candidate: candidate.id, commit: candidate.merged.commit, review, checks};
    validations.push(result);
    return {...result, passed: review.approved && checks.every(check => check.exitCode === 0)};
  }

  try {
    candidate = await integration.prepare({tasks: input.tasks, drift: input.drift || "paths"});
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
        outcome = await integrate({candidate: candidate.id});
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
    fail("Integration blocked: " + error.message, {
      status: "blocked", candidate: candidate && candidate.id,
      code: error.code, details: error.result, repairs, refreshes, validations,
      retainedContexts: [...new Set(temporary)],
    });
  }
});
