// /workflow /absolute/path/fix-review-findings.js /absolute/path/input.json
// Every source is copied; research completes on saved step delivery.
// Workers never commit. The parent integrates the returned editing task revisions.
// Each checks entry is a separate required command. exec enables pipefail;
// propagate compound failures explicitly.
const {obj, str, arr, enum: senum, keyed} = polly.schema;
const finding = obj({ id: str({minLength: 1}), summary: str() });
const fix = obj({ status: senum("fixed", "skipped"), what: str() });
const verdict = obj({
  verdict: senum("fixed", "not_fixed", "regression"),
  reasoning: str(), requiredChange: str(),
});
const ids = findings => findings.map(f => f.id);

async function verify(group, worker, reports) {
  const candidate = await polly.snapshot(worker.context);
  const checkContext = await polly.context({commit: candidate.commit});
  const results = await polly.parallel(["review", "checks"], async kind => {
    if (kind === "review") {
      return polly.research(group.label + " reviewer",
        "Independently verify EVERY finding in the candidate. Read the evidence file. Treat fix reports as claims. Trace the failure scenario and whether the regression test detects it. Return an actionable verdict for every key. You cannot edit or commit.", {
        commit: candidate.commit,
        input: {findings: group.findings, evidence: group.evidence, reports},
        schema: keyed(ids(group.findings), verdict),
      });
    }
    return polly.scope({context: checkContext, label: "checks"}, async work => {
      const failures = [];
      for (const command of group.checks) {
        const result = await work.exec(command, {check: false});
        if (result.exitCode !== 0) failures.push({command, exitCode: result.exitCode, step: result.step, outputTail: result.text.slice(-6000)});
      }
      return failures;
    });
  }, {concurrency: 2, errors: "throw_after_all"});
  const review = results[0].value;
  const rejected = Object.entries(review.value)
    .filter(([, item]) => item.verdict !== "fixed")
    .map(([id, item]) => ({id, ...item}));
  const failedChecks = results[1].value;
  return {candidate, reviewTask: review.task, rejected, failedChecks,
    passed: rejected.length === 0 && failedChecks.length === 0};
}

async function fixGroup(group) {
  const worker = await polly.editor(group.source, group.label + " fixer",
    "Fix the supplied findings in your assigned isolated files. Read the evidence file and repository instructions. Add focused regression tests and format changes. Preserve unrelated work. Do not commit or push. Report every finding key, including anything skipped.", {
    input: {findings: group.findings, evidence: group.evidence},
    schema: keyed(ids(group.findings), fix),
  });
  const reports = [worker.value];
  const reviewTasks = [];
  let verification = await verify(group, worker, reports);
  reviewTasks.push(verification.reviewTask);
  if (!verification.passed) {
    const repairFindings = verification.failedChecks.length ? group.findings :
      group.findings.filter(f => verification.rejected.some(r => r.id === f.id));
    await polly.log("One repair pass for " + group.label);
    const repaired = await polly.agent({
      session: worker.session,
      task: "Repair the rejected findings and attributable check failures. Preserve accepted fixes and unrelated files. Do not commit or push. This is the only repair pass.",
      input: {repairFindings, rejected: verification.rejected, failedChecks: verification.failedChecks},
      schema: keyed(ids(repairFindings), fix),
    });
    reports.push(repaired.value);
    verification = await verify(group, worker, reports);
    reviewTasks.push(verification.reviewTask);
  }
  const summary = {
    label: group.label, status: verification.passed ? "verified" : "unresolved",
    task: worker.task, session: worker.session, commit: verification.candidate.commit,
    reviewTasks, repairAttempted: reports.length > 1,
    rejected: verification.rejected, failedChecks: verification.failedChecks,
  };
  if (!verification.passed) polly.fail("Findings or checks remain unresolved", summary);
  return summary;
}

polly.workflow("fix-review-findings", obj({groups: arr(obj({
  label: str({minLength: 1}), source: str({minLength: 1}),
  evidence: str({minLength: 1}),
  findings: arr(finding, {minItems: 1}),
  checks: arr(str({minLength: 1}), {minItems: 1}),
}), {minItems: 1})}), ({groups}) => {
  const sources = new Set();
  for (const group of groups) {
    if (sources.has(group.source)) polly.fail("Duplicate source", {source: group.source});
    sources.add(group.source);
    keyed(ids(group.findings), fix); // duplicate finding ids reject before any editor launches
  }
  return polly.parallel(groups, fixGroup, {concurrency: 8, errors: "throw_after_all"});
});
