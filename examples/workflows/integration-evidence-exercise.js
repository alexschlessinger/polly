// Run only in a disposable Git repository. Before launching, create a tracked
// file containing exactly "features=base\n". Input: {path:"features.txt"}.
// This exercise deliberately creates conflicting edits and an incomplete repair.
// It applies only after a real failing content assertion passes on a successor.
const s = polly.schema;
const quote = text => "'" + text.replace(/'/g, "'\\''") + "'";

polly.defineWorkflow({
  name: "integration-evidence-exercise",
  inputSchema: s.object({path: s.string({minLength: 1})}),
  async run(input) {
    if (input.path.startsWith("/") || input.path.split("/").some(part => !part || part === "." || part === "..")) {
      polly.fail("path must name a file relative to the disposable repository");
    }
    const contexts = [];
    const validations = [];
    const expected = "features=base,alpha,beta\n";
    const command = content => "set -o pipefail; printf '%s' " + quote(content) + " | cmp - " + quote("./" + input.path);

    async function edit(label, commit, content) {
      const result = await polly.agent({
        label, commit, tools: ["read_file", "write_file"],
        task: "Integration evidence exercise: " + label + ". Set only " + JSON.stringify(input.path) +
          " to exactly " + JSON.stringify(content) + ". This is a deliberate test fixture, including when an incomplete repair is requested. Preserve all other files. Do not commit. Finish with a plain summary.",
      });
      contexts.push(result.context);
      const task = await polly.tasks.read(result.task);
      return {task: task.id, revision: task.revision, commit: task.resultCommit, session: result.session};
    }

    const baselineContext = await polly.context({readOnly: true});
    contexts.push(baselineContext);
    await polly.exec(command("features=base\n"), {context: baselineContext});
    const baseline = await polly.snapshot(baselineContext);
    const rows = await polly.parallel(["alpha", "beta"],
      name => edit("exercise " + name, baseline.commit, "features=base," + name + "\n"),
      {concurrency: 2, errors: "throw_after_all"});
    const edits = rows.map(row => row.value);
    let candidate = await polly.integration.prepare({tasks: edits.map(({task, revision}) => ({task, revision}))});
    if (!(candidate.conflicts || []).length || candidate.receipt !== null) polly.fail("Expected an unapplied conflicting candidate", candidate);
    const conflicted = {id: candidate.id, commit: candidate.merged.commit};

    const incomplete = await edit("exercise incomplete repair", candidate.merged.commit, "features=base,alpha\n");
    candidate = await polly.integration.revise(candidate.id, {task: incomplete.task, revision: incomplete.revision});
    if ((candidate.conflicts || []).length) polly.fail("Incomplete fixture still has merge conflicts", candidate);
    const failedContext = await polly.context({commit: candidate.merged.commit, readOnly: true});
    contexts.push(failedContext);
    let failed = false;
    try {
      await polly.exec(command(expected), {context: failedContext});
    } catch (error) {
      // Sandbox/setup failures, cancellation and timeout are blockers, not the
      // deliberate content mismatch this exercise is intended to repair.
      if (error.code !== "command_failed" || error.result.exitCode !== 1) throw error;
      failed = true;
      validations.push({candidate: candidate.id, commit: candidate.merged.commit, passed: false, error: error.result});
    }
    if (!failed) polly.fail("Expected the incomplete repair to fail its content assertion", candidate);
    if ((await polly.integration.read(candidate.id)).receipt !== null) polly.fail("An apply attempt preceded successful validation");

    const repair = await edit("exercise complete repair", candidate.merged.commit, expected);
    candidate = await polly.integration.revise(candidate.id, {task: repair.task, revision: repair.revision});
    if ((candidate.conflicts || []).length || candidate.receipt !== null) polly.fail("Expected an unapplied repaired candidate", candidate);
    const validatedCommit = candidate.merged.commit;
    if (validatedCommit === validations[0].commit) polly.fail("Repair did not produce a successor commit");
    const review = await polly.agent({
      label: "exercise reviewer", commit: validatedCommit, readOnly: true, tools: ["read_file"],
      task: "Review the integration evidence exercise. Read the fixture and confirm its exact contents, with both alpha and beta and no conflict markers. Return approved only when it matches the supplied expected contents. Do not edit.",
      input: {path: input.path, expected, commit: validatedCommit},
      schema: s.object({approved: s.boolean(), feedback: s.string()}),
    });
    contexts.push(review.context);
    if (!review.value.approved) polly.fail("Repaired candidate needs further review", {commit: validatedCommit, review: review.value});
    const checkContext = await polly.context({commit: validatedCommit, readOnly: true});
    contexts.push(checkContext);
    const check = await polly.exec(command(expected), {context: checkContext});
    // Independent required validations are separate awaited calls. This one
    // also records the actual bytes using POSIX flags supported on macOS.
    const bytes = await polly.exec("od -An -tx1 " + quote("./" + input.path), {context: checkContext});
    validations.push({candidate: candidate.id, commit: validatedCommit, passed: true, review: review.value, check, bytes});
    const current = await polly.integration.read(candidate.id);
    if (current.merged.commit !== validatedCommit || current.receipt !== null) polly.fail("Candidate changed after validation", current);

    const applied = await polly.integrate({candidate: candidate.id});
    if (applied.status !== "applied" || applied.receipt.status !== "applied") polly.fail("Application was not confirmed", applied);
    return {baseline: baseline.commit, edits, conflicted, incomplete, repair, validations,
      candidate: candidate.id, commit: validatedCommit, receipt: applied.receipt, contexts};
  },
});
