// /workflow /absolute/path/integrate-results.js /absolute/path/input.json
// Input: {tasks:[{task:"ID",revision:3}],checks:["go test ./..."],drift:"paths"}
// Parent authority comes from the host. No commits, publishing, or JS replay.
const s = polly.schema;
const taskRef = s.object({task: s.string({minLength: 1}), revision: s.integer({minimum: 1})});
const reviewSchema = s.object({approved: s.boolean(), feedback: s.string()});

polly.defineWorkflow({
  name: "integrate-results",
  inputSchema: s.object({
    tasks: s.array(taskRef, {minItems: 1}),
    checks: s.array(s.string({minLength: 1})),
    drift: s.enum("paths", "tree"),
    reviewInstructions: s.string(),
  }, {required: ["tasks", "checks"]}),
  async run(input) {
    let candidate;
    let repairs = 0;
    let refreshes = 0;
    let validated = false;
    const temporary = [];
    const validations = [];

    async function repair(reason, evidence) {
      if (repairs >= 2) polly.fail("Integration needs further repair", {candidate: candidate.id, reason, evidence});
      repairs += 1;
      const result = await polly.agent({
        label: "integration repair " + repairs,
        snapshot: candidate.merged.id,
        task: "Repair this exact integration candidate. Resolve the supplied conflicts or validation failures while preserving all intended contributions. Use the structured conflicts and their base/ours/theirs snapshots and blob references (read-only git show) for binary and rename conflicts; do not rely on textual markers. Make source changes only in your assigned copy. Do not commit or publish. Report what changed.",
        input: {candidate: candidate.id, reason, evidence, conflicts: candidate.conflicts},
        schema: s.object({summary: s.string()}),
      });
      temporary.push(result.context);
      const task = await polly.tasks.read(result.task);
      candidate = await polly.integration.revise(candidate.id, {task: task.id, revision: task.revision});
      validated = false;
    }

    async function validate() {
      const taskRows = await polly.parallel([...candidate.inputs, ...(candidate.repairs || [])],
        input => polly.tasks.read(input.task), {concurrency: 4, errors: "throw_after_all"});
      const submissions = taskRows.map(row => row.value);
      const rows = await polly.parallel(["review", ...input.checks.map((_, i) => i)], async item => {
        if (item === "review") {
          const review = await polly.agent({
            label: "integration reviewer", snapshot: candidate.merged.id, readOnly: true,
            task: "Independently review this combined candidate, including every contribution and repair. Check the requested acceptance criteria and regressions. Treat previous reports as claims. Return approved only when no required changes remain. You cannot edit or commit.",
            input: {candidate: candidate.id, submissions, inputs: candidate.inputs, repairs: candidate.repairs, instructions: input.reviewInstructions || ""},
            schema: reviewSchema,
          });
          temporary.push(review.context);
          return review.value;
        }
        const context = await polly.context({snapshot: candidate.merged.id});
        temporary.push(context);
        const command = input.checks[item];
        const result = await polly.exec(command, {context, check: false});
        return {command, context, exitCode: result.exitCode, output: result.text.slice(-6000)};
      }, {concurrency: 4, errors: "collect"});
      const errors = rows.filter(row => !row.ok);
      // Ordinary command exits are data. Sandbox/setup failures, cancellation,
      // budget exhaustion, and other errors are blockers, not repair requests.
      if (errors.length) polly.fail("Validation could not run", {candidate: candidate.id, errors});
      const review = rows[0].value;
      const checks = rows.slice(1).map(row => row.value);
      const result = {candidate: candidate.id, review, checks};
      validations.push(result);
      return {...result, passed: review.approved && checks.every(check => check.exitCode === 0)};
    }

    try {
      candidate = await polly.integration.prepare({tasks: input.tasks, drift: input.drift || "paths"});
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
        await polly.integration.accept(candidate.id);
        let receipt;
        try {
          receipt = await polly.integration.apply(candidate.id);
        } catch (error) {
          if (error.code !== "parent_changed" || refreshes >= 1) throw error;
          refreshes += 1;
          const refreshed = await polly.integration.refresh(candidate.id);
          candidate = refreshed;
          if (refreshed.changed) validated = false;
          // An unchanged refresh retains validation and acceptance. Both
          // counters remain consumed across all changed candidates.
          continue;
        }
        const cleanup = await polly.parallel([...new Set(temporary)], async context => {
          try { return await polly.release(context); }
          catch (error) { return {retained: context, code: error.code, reason: error.message, details: error.result}; }
        }, {concurrency: 4, errors: "collect"});
        return {status: "applied", candidate: candidate.id, receipt, repairs, refreshes, validations,
          retained: cleanup.filter(row => !row.ok || row.value.retained).map(row => row.ok ? row.value : row.error)};
      }
    } catch (error) {
      polly.fail("Integration blocked: " + error.message, {
        status: "blocked", candidate: candidate && candidate.id,
        code: error.code, details: error.result, repairs, refreshes, validations,
        retainedContexts: [...new Set(temporary)],
      });
    }
  },
});
