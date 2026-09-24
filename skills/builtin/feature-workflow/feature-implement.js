// Ships inside polly as part of the builtin feature-workflow skill; the skill
// names it by skill and path, and the host reads the file.
// Input: {"name":"kebab-case-feature","specFiles":["docs/features/x.md"],"planFile":"docs/features/x.md",
//         "source":"/repo","checks":["make test"],"hostNotes":["..."]}
// The spec may also come inline as "spec" and the plan as "plan"; give one of
// each pair.
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
// regression this wave caused, and does not block. When that shared failure is
// a package that could not set up or build, none of its tests ran on either
// commit, so the result hands the check back as unverified for the parent to
// run where it can. Parent authority comes from the host.
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
// environmentNotes is what research learned about the host; a plan written
// before that field existed still validates.
const planSchema = obj({
  summary: nonblank,
  checks: arr(nonblank),
  finalChecks: arr(nonblank),
  tasks: arr(planTask, {minItems: 1}),
  docsUpdates: arr(str()),
  risks: arr(str()),
  openQuestions: arr(str()),
  environmentNotes: arr(str()),
}, {required: planRequired});
const editorSchema = obj({summary: nonblank, filesChanged: arr(str()), notes: arr(str())});
// A review names each change it requires under a stable id, so the review
// after a repair can close or carry every one of them instead of starting
// over.
const requiredChange = obj({id: nonblank, summary: nonblank, paths: arr(str())}, {required: ["id", "summary"]});
const reviewSchema = obj({
  approved: bool(),
  feedback: str(),
  requiredChanges: arr(requiredChange),
  closed: arr(obj({id: nonblank, evidence: nonblank})),
});
const repairSchema = obj({summary: nonblank, closed: arr(str()), filesChanged: arr(str())}, {required: ["summary"]});

// The sentence every agent gets when the run carries facts about the host.
const notesSentence = notes => notes.length ? " Your input's hostNotes are verified facts about this host, from earlier phases or published by teammates: trust them, do not re-verify them, and do not attempt what they rule out." : "";

// failure names: begin (feature-implement.js and feature-research.js each carry this block; a test keeps them identical)
// Names a test runner prints for one failing case, across the common
// formats: Go's "--- FAIL: Name (0.01s)", pytest's "FAILED path::case", TAP's
// "not ok N - case", the bullets Jest, Vitest and Mocha put before a failed
// title, and Vitest's " FAIL  file > case" summary. The name is the rest of
// the line less a trailing duration, so a title with spaces survives whole.
// Go's package line closes the cases above it and qualifies them, so one
// name in two packages stays two failures; a package line with no case above
// it (a panic, a timeout, a TestMain exit) or a setup or build failure names
// the package itself, and those names are also returned as packages, since
// none of that package's tests ran to the end. The set is a heuristic and can
// only make validation stricter: a name seen at the candidate but not at the
// baseline blocks, and a failure whose output names nothing recognisable is
// blocked as well, because nothing ties it to the baseline.
const failureMarker = /^\s*(?:---\s*FAIL:\s*|FAIL:\s*|FAIL\s{2,}|FAILED\s+|not ok\s+\d+\s*-?\s*|[\u2717\u2715\u00d7\u25cf\u276f]\s+)(.+?)\s*(?:\([^()]*\))?\s*$/;
const packageMarker = /^FAIL\t(\S+)(?:\s+(\[[^\]]+\]))?/;

function failureNames(text) {
  const names = new Set();
  const packages = new Set();
  let pending = [];
  for (const line of String(text || "").split("\n")) {
    const pkg = packageMarker.exec(line);
    if (pkg) {
      const own = pkg[2] ? pkg[1] + " " + pkg[2] : pending.length ? "" : pkg[1];
      if (own) {
        names.add(own);
        packages.add(own);
      }
      for (const name of pending) names.add(pkg[1] + ": " + name);
      pending = [];
      continue;
    }
    const match = failureMarker.exec(line);
    if (match) pending.push(match[1]);
  }
  for (const name of pending) names.add(name);
  return {names, packages};
}
// failure names: end
// check origins: begin (feature-implement.js and feature-research.js each carry this block; a test keeps them identical)
// A check may run a harness that a plan task creates (a script or fixture
// absent from the unchanged code). Such a check cannot run before that task
// lands, so both scripts recognise it the same way: the command names a path
// that overlaps one of the task's paths, and that path does not exist in the
// tree at hand. Existence is the tree's answer, never the plan's, so a task
// that only edits an existing harness leaves its check running as usual.
const clean = path => path.replace(/^(\.\/)+/, "").replace(/\/+$/, "").replace(/^\.$/, "");
// A path overlaps another when they are equal or one contains the other; the
// repository root contains everything.
function overlap(a, b) {
  a = clean(a);
  b = clean(b);
  return a === "" || b === "" || a === b || a.startsWith(b + "/") || b.startsWith(a + "/");
}
// The repository paths a command names: its slash-bearing words less flags,
// absolute paths, variables, quotes, and a trailing /... or glob. "node
// tools/smoke.mjs" names tools/smoke.mjs, "go test ./pkg/..." names pkg,
// "make test" names nothing.
function pathsNamed(command) {
  const named = new Set();
  for (const word of String(command || "").split(/[\s;|&<>()`"']+/)) {
    if (!word.includes("/") || word.startsWith("-") || word.startsWith("/") || word.includes("$")) continue;
    const path = clean(word.replace(/\/(\.\.\.|\*.*)$/, ""));
    if (path && !path.startsWith("..")) named.add(path);
  }
  return [...named];
}
// The first task whose paths overlap a path the command names, with the
// named paths concerned; null when no task does. A task path of "." would
// claim every check, so it does not count.
function checkOrigin(command, tasks) {
  const named = pathsNamed(command);
  for (const task of tasks || []) {
    const paths = named.filter(p => (task.paths || []).some(q => clean(q) && overlap(p, q)));
    if (paths.length) return {task: task.id, paths};
  }
  return null;
}
const quoteShell = text => "'" + String(text).replace(/'/g, "'\\''") + "'";
// A shell command printing each absent path on its own line; exits 0 either way.
const missingPaths = paths => "for p in " + paths.map(quoteShell).join(" ") + "; do test -e \"$p\" || printf '%s\\n' \"$p\"; done";
const lines = text => String(text || "").split("\n").map(s => s.trim()).filter(Boolean);
// check origins: end
// host facts: begin (feature-implement.js and feature-research.js each carry this block; a test keeps them identical)
// Facts about this host that agents published with swarm_publish (kind host)
// stay in the swarm across runs, so every later agent gets them as hostNotes
// instead of rediscovering them. A failed read costs the notes, never the run.
async function publishedHostFacts() {
  try {
    return (await polly.publications({kind: "host"})).map(p => p.text);
  } catch (error) {
    await polly.log("published host facts were not read: " + error.message);
    return [];
  }
}
const withFacts = (notes, facts) => [...new Set([...notes, ...facts])];
// host facts: end
// spec files: begin (feature-implement.js and feature-research.js each carry this block; a test keeps them identical)
// The parent hands the spec over as files rather than pasted text. Each is
// read with cat from the read-only context at hand (a capture of the source,
// or the live tree outside Git), in the order given, and the texts are joined
// with a blank line. A file is taken up to its "## Plan" heading: the plan
// phase 2 appends there is derived from the spec, not part of it, so a plan
// file among the spec files never reaches an agent twice. A file that cannot
// be read fails the run, naming the file. One command per file keeps the
// attribution exact and the separator ours: a single cat would glue a file
// lacking a final newline to the next.
const planHeading = /^## Plan\b/;
async function readFile(context, path, what) {
  let read;
  try {
    read = await polly.exec("cat -- " + quoteShell(path), {context, check: false});
  } catch (error) {
    polly.fail(what + " " + path + " could not be read: " + error.message, {path, code: error.code});
  }
  if (read.exitCode !== 0) polly.fail(what + " " + path + " could not be read (exit " + read.exitCode + "): " + String(read.text || "").trim(), {path, exitCode: read.exitCode});
  if (/\[output truncated/.test(String(read.text || ""))) polly.fail(what + " " + path + " is larger than a command can return", {path});
  return String(read.text || "");
}
async function readSpecFiles(context, paths) {
  const texts = [];
  for (const path of paths) {
    const rows = (await readFile(context, path, "spec file")).split(/\r?\n/);
    const plan = rows.findIndex(row => planHeading.test(row));
    texts.push((plan < 0 ? rows : rows.slice(0, plan)).join("\n").trim());
  }
  const spec = texts.filter(Boolean).join("\n\n");
  if (!spec) polly.fail("spec files hold no text: " + paths.join(", "), {paths});
  return spec;
}
// spec files: end

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

// What a reader gets of a check: a count for a passing one; the names, the
// baseline comparison and the tail of the output for a failing one; and, for
// a check waiting on a file a later wave creates, why it was skipped. The
// caller's summary keeps the compact form, agents read the detailed one.
const compactCheck = check => check.skipped ? {command: check.command, skipped: check.skipped}
  : check.exitCode === 0 ? {command: check.command, exitCode: 0}
  : {command: check.command, exitCode: check.exitCode, failures: check.failures.length, preexisting: !!check.preexisting,
    ...(check.unverified ? {unverified: check.unverified} : {})};
const detailCheck = check => check.skipped || check.exitCode === 0 ? compactCheck(check)
  : {...compactCheck(check), failures: check.failures, ...(check.baseline ? {baseline: check.baseline} : {}), output: check.output.slice(-2000)};

// How a candidate is described to an agent: its commits, and what its id is
// not. Left unexplained, a 32-hex id beside 40-hex commits gets passed to
// git.
const describe = c => ({
  id: c.id, commit: c.merged.commit, baseCommit: c.parent && c.parent.commit,
  note: "id is Polly's integration record for this merged result, not a git object: never pass it to git. commit is the snapshot your worktree holds; baseCommit is the commit this wave merged onto, so git diff <baseCommit> <commit> shows exactly what the wave changed.",
});

// The paths whose content differs between two candidates of the same wave: a
// repair's edits, plus anything it put back to the parent's content. Both
// candidates list every path that differs from the same parent.
function changedBetween(previous, current) {
  const state = c => new Map(((c.plan && c.plan.paths) || []).map(p => [p.path, p.after || {}]));
  const before = state(previous), after = state(current);
  const changed = new Set();
  for (const [path, s] of after) {
    const o = before.get(path);
    if (!o || o.object !== s.object || o.exists !== s.exists || o.mode !== s.mode) changed.add(path);
  }
  for (const path of before.keys()) if (!after.has(path)) changed.add(path);
  return [...changed].sort();
}

// What the caller gets for a landed wave: what each editor reported and how
// the passing validation came out. The plan tasks are the caller's own, and a
// passing check's output and failure names add nothing a count does not; the
// full records stay on the tasks, and a wave that fails keeps its evidence in
// the error, where it is the failure.
function landed(number, wave, submissions, outcome) {
  const passed = outcome.validations[outcome.validations.length - 1];
  return {
    wave: number, tasks: wave.map(t => t.id),
    submissions: submissions.map(s => ({task: s.task, report: s.report})),
    integration: {
      status: outcome.status, candidate: outcome.candidate,
      receipt: outcome.receipt && {id: outcome.receipt.id, status: outcome.receipt.status},
      repairs: outcome.repairs, refreshes: outcome.refreshes,
      review: passed && passed.review, checks: passed ? passed.checks.map(compactCheck) : [],
      retained: outcome.retained,
    },
  };
}

// Merge one wave's editing tasks, then check, review, repair (at most two
// repairs and one refresh), and integrate. Adapted from integrate-results.js.
// The checks run before the reviewer, who reads their classified results
// instead of re-running the suite; the review after a repair also gets the
// previous verdict, each repair's report and the paths that changed, and
// must close or carry every required change.
async function integrateWave(input, {refs, submissions, checks, deferrable, notes}) {
  let candidate;
  const repairLog = [];
  let refreshes = 0;
  let validated = false;
  // The last review and the candidate it judged. A refresh that changed the
  // merge keeps the verdict but drops the candidate, so every path counts as
  // changed.
  let reviewed = null;
  const temporary = [];
  const validations = [];
  // The package-level names of each failing check, kept beside the check
  // rather than in it, so the evidence agents read stays as it was.
  const packageFailures = new Map();
  const hostNotes = notes.length ? {hostNotes: notes} : {};

  async function repair(reason, evidence) {
    if (repairLog.length >= 2) fail("Wave integration needs further repair", {candidate: candidate.id, reason, evidence});
    const review = evidence && evidence.review;
    const failing = ((evidence && evidence.checks) || []).filter(check => !check.skipped && check.exitCode !== 0);
    const conflicts = candidate.conflicts || [];
    const open = (review && review.requiredChanges) || [];
    // Each sentence below applies only when its evidence exists.
    const brief = "Repair this exact integration candidate for the feature '" + input.name + "'. Your input holds the feature spec, each submission pairing a plan task with its editor's report, the review with its required changes, every check that failed with its output, and what earlier repairs in this wave reported; candidate.commit is the merged snapshot your worktree holds and candidate.id is Polly's record of it, not a git object. Preserve every intended contribution and make source changes only in your assigned copy."
      + (conflicts.length ? " Resolve the structured conflicts using their base/ours/theirs commits and blob references (read-only git show) for binary and rename conflicts; do not rely on textual markers." : "")
      + (open.length ? " Address every entry in review.requiredChanges and report each id you fixed in closed." : "")
      + (failing.some(check => !check.preexisting) ? " Make the failing checks pass and run each yourself before finishing." : "")
      + (failing.some(check => check.preexisting) ? " A check carrying preexisting:true failed the same way on the commit this wave merged onto; it is not yours to fix, and changing project code to satisfy it is wrong." : "")
      + notesSentence(notes) + " Do not commit or push. Report what changed.";
    const result = await agent("integration repair " + (repairLog.length + 1), brief, {
      commit: candidate.merged.commit,
      input: {candidate: describe(candidate), reason, spec: input.spec || "", submissions,
        ...(review ? {review} : {}), checks: failing.map(check => ({...detailCheck(check), output: check.output})),
        conflicts, repairs: repairLog, ...hostNotes},
      schema: repairSchema,
    });
    temporary.push(result.context);
    const task = await tasks.get(result.task);
    candidate = await integration.revise(candidate.id, {task: task.id, revision: task.revision});
    repairLog.push({repair: repairLog.length + 1, task: task.id, revision: task.revision, reason,
      summary: result.value.summary, closed: result.value.closed || [], filesChanged: result.value.filesChanged || []});
    validated = false;
  }

  async function reviewCandidate(checkResults) {
    const again = reviewed !== null;
    let brief = "Independently review this merged implementation candidate, including every contribution and repair. Your input contains the feature spec, each submission pairing a plan task (planTask, with its brief and acceptance criteria) with its editor's report, and the result of every check already run on this exact commit, classified against the commit the wave merged onto: a check marked preexisting failed the same way there and is not this wave's regression, and a skipped check waits for a file a later wave creates. Do not re-run the checks; read their output and verify the code. Treat the reports as claims and verify against the code. Check the tasks' acceptance criteria and look for regressions, convention violations, and missed project requirements (docs, platform variants, tests). List every change that must be made before approval in requiredChanges, one entry per finding with a stable id (R1, R2, ...) and the paths concerned; approved is true only when requiredChanges is empty. closed is empty on a first review." + notesSentence(notes) + " You cannot edit or commit.";
    const extra = {};
    if (again) {
      brief += " This candidate was reviewed before and repaired since: previousReview holds the verdict, repairs what each repair reports, and changedSince the paths whose content differs from the candidate you reviewed (null when the parent files moved and the candidate was re-merged, so treat every path as changed). For every entry in previousReview.requiredChanges, verify in the code that it is fixed and list it under closed with the evidence you checked, or carry it into requiredChanges under the same id; an entry you neither close nor carry is a review that did not happen. Then re-check the files in changedSince for regressions the repair introduced. Do not repeat verified findings as new ids.";
      extra.previousReview = reviewed.review;
      extra.repairs = repairLog;
      extra.changedSince = reviewed.candidate ? changedBetween(reviewed.candidate, candidate) : null;
      if (reviewed.refreshed) extra.refreshed = true;
    }
    const review = await research("wave reviewer", brief, {
      commit: candidate.merged.commit,
      input: {candidate: describe(candidate), spec: input.spec || "", submissions, checks: checkResults.map(detailCheck), ...extra,
        ...hostNotes, instructions: input.reviewInstructions || ""},
      schema: reviewSchema,
    });
    temporary.push(review.context);
    let value = review.value;
    if (again) {
      // A required change the re-review neither closed nor carried was not
      // reviewed: one continuation of the same session asks for exactly those.
      const ids = xs => new Set((xs || []).map(x => x.id));
      const previous = (reviewed.review.requiredChanges || []).map(rc => rc.id);
      const missing = value => {
        const accounted = new Set([...ids(value.closed), ...ids(value.requiredChanges)]);
        return previous.filter(id => !accounted.has(id));
      };
      const unaccounted = missing(value);
      if (unaccounted.length) {
        await log("re-review did not account for " + unaccounted.join(", ") + "; asking once more");
        const retry = await agent({session: review.session,
          task: "Your review did not account for required changes " + unaccounted.join(", ") + " from the previous review. Verify each in the code now and return the complete review again: each id under closed with the evidence you checked, or under requiredChanges if it still stands.",
          input: {unaccounted: (reviewed.review.requiredChanges || []).filter(rc => unaccounted.includes(rc.id))}, schema: reviewSchema});
        value = retry.value;
      }
      const remaining = missing(value);
      if (remaining.length) fail("Re-review still did not account for required changes: " + remaining.join(", "), {
        previousReview: reviewed.review, review: value, unaccounted: remaining,
      });
      await log("re-review: closed " + [...ids(value.closed)].join(", ") + "; open " + [...ids(value.requiredChanges)].join(", "));
    }
    reviewed = {candidate, review: value};
    return value;
  }

  async function validate() {
    const rows = await parallel(checks.map((_, i) => i), async item => {
      const command = checks[item];
      // A check may leave build outputs behind; a disposable copy is released
      // whatever it holds, where an ordinary changed copy would be retained.
      const context = await ctx({commit: candidate.merged.commit, disposable: true});
      temporary.push(context);
      const origin = deferrable[command];
      if (origin) {
        const probe = await exec(missingPaths(origin.paths), {context, check: false});
        const missing = lines(probe.text);
        if (missing.length) return {command, skipped: {paths: missing, createdBy: origin.task, wave: origin.wave}};
      }
      const result = await exec(command, {context, check: false});
      // Names come from the whole output; the stored tail is for readers.
      const found = result.exitCode === 0 ? {names: new Set(), packages: new Set()} : failureNames(result.text);
      const check = {command, exitCode: result.exitCode, output: result.text.slice(-6000), failures: [...found.names]};
      packageFailures.set(check, found.packages);
      return check;
    }, {concurrency: 4, errors: "collect"});
    const errors = rows.filter(row => !row.ok);
    if (errors.length) fail("Wave validation could not run", {candidate: candidate.id, commit: candidate.merged.commit, errors});
    const checkResults = rows.map(row => row.value);
    const skipped = checkResults.filter(check => check.skipped);
    if (skipped.length) await log("checks skipped until their files exist: " + skipped.map(check => check.command + " (" + check.skipped.paths.join(", ") + ", created by " + check.skipped.createdBy + " in wave " + check.skipped.wave + ")").join("; "));
    await classify(checkResults);
    const review = await reviewCandidate(checkResults);
    const result = {candidate: candidate.id, commit: candidate.merged.commit, review, checks: checkResults};
    validations.push(result);
    return {...result, passed: review.approved && !(review.requiredChanges || []).length && checkResults.every(check => check.skipped || check.exitCode === 0 || check.preexisting)};
  }

  // Re-run only the failing commands on the commit this wave merged onto, and
  // mark the ones that fail there the same way as pre-existing. A wave whose
  // checks all pass never reaches this, so the comparison costs nothing until
  // something is already wrong.
  async function classify(checkResults) {
    const failed = checkResults.filter(check => !check.skipped && check.exitCode !== 0);
    const baselineCommit = candidate.parent && candidate.parent.commit;
    if (!failed.length || !baselineCommit) return;
    const baseline = await ctx({commit: baselineCommit, disposable: true});
    temporary.push(baseline);
    const rows = await parallel(failed, async check => {
      const result = await exec(check.command, {context: baseline, check: false});
      return {exitCode: result.exitCode, failures: failureNames(result.text).names};
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
      // A package that could not set up, build, or finish on either commit
      // ran none of its tests there: it does not block, but it verified
      // nothing, so it is named for the caller to run where it can.
      const packages = packageFailures.get(check);
      const unverified = check.preexisting && packages ? names.filter(name => packages.has(name)) : [];
      if (unverified.length) check.unverified = unverified;
    });
    const carried = failed.filter(check => check.preexisting).map(check => check.command);
    if (carried.length) await log("checks failing at the wave baseline too, not blocking: " + carried.join("; "));
    const unverified = failed.filter(check => check.unverified).map(check => check.command + " (" + check.unverified.join(", ") + ")");
    if (unverified.length) await log("checks whose packages did not run at the wave baseline either, unverified: " + unverified.join("; "));
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
        if (refreshed.changed) {
          validated = false;
          if (reviewed) reviewed = {...reviewed, candidate: null, refreshed: true};
        }
        continue;
      }
      const cleanup = await parallel([...new Set(temporary)], async context => {
        try { return await release(context); }
        catch (error) { return {retained: context, code: error.code, reason: error.message, details: error.result}; }
      }, {concurrency: 4, errors: "collect"});
      return {status: outcome.status, candidate: candidate.id, receipt: outcome.receipt, repairs: repairLog, refreshes, validations,
        retained: cleanup.filter(row => !row.ok || row.value.retained).map(row => row.ok ? row.value : row.error)};
    }
  } catch (error) {
    fail("Wave integration blocked: " + error.message, {
      status: "blocked", candidate: candidate && candidate.id,
      code: error.code, details: error.result, repairs: repairLog, refreshes, validations,
      retainedContexts: [...new Set(temporary)],
    });
  }
}

// The plan a feature file carries: the first fenced block a ```json line
// opens after its "## Plan" heading, or the first in the file when none
// follows the heading; null when the file has none. The block runs to the
// next ``` line, or to the end of the file when it is never closed, and
// JSON.parse then says what is wrong with it.
function planBlock(text) {
  const rows = String(text || "").split(/\r?\n/);
  const heading = rows.findIndex(row => planHeading.test(row));
  for (const from of heading < 0 ? [0] : [heading + 1, 0]) {
    const open = rows.findIndex((row, i) => i >= from && /^\s*```json\s*$/.test(row));
    if (open < 0) continue;
    const close = rows.findIndex((row, i) => i > open && /^\s*```\s*$/.test(row));
    return rows.slice(open + 1, close < 0 ? rows.length : close).join("\n");
  }
  return null;
}
// No host operation validates a value against a schema, so a plan read from
// a file is checked here against the planSchema that input validation
// applies to an inline plan: one rule, two doors. The walker covers the
// keywords polly.schema emits for that schema and reports every problem, so
// one edit of the block can fix them all.
function shapeProblems(schema, value, at) {
  const kind = Array.isArray(value) ? "array" : value === null ? "null" : typeof value;
  if (schema.type && kind !== schema.type) return [at + " must be " + (schema.type === "object" ? "an object" : schema.type === "array" ? "an array" : "a " + schema.type) + ", not " + kind];
  const problems = [];
  if (kind === "string") {
    if (schema.minLength && value.length < schema.minLength) problems.push(at + " must be at least " + schema.minLength + " character(s)");
    if (schema.pattern && !new RegExp(schema.pattern).test(value)) problems.push(at + (schema.pattern === "\\S" ? " must not be blank" : " must match " + schema.pattern));
  }
  if (kind === "array") {
    if (schema.minItems && value.length < schema.minItems) problems.push(at + " must hold at least " + schema.minItems + " item(s)");
    if (schema.items) value.forEach((item, i) => problems.push(...shapeProblems(schema.items, item, at + "[" + i + "]")));
  }
  if (kind === "object") {
    for (const key of schema.required || []) if (!(key in value)) problems.push(at + " is missing " + key);
    for (const key of Object.keys(value)) {
      if (schema.properties && key in schema.properties) problems.push(...shapeProblems(schema.properties[key], value[key], at + "." + key));
      else if (schema.additionalProperties === false) problems.push(at + " has an unknown key " + key);
    }
  }
  return problems;
}
const planShapeProblems = value => shapeProblems(planSchema, value, "plan");
async function readPlanFile(context, file) {
  const block = planBlock(await readFile(context, file, "plan file"));
  if (block === null) fail("plan block in " + file + " is missing: no ```json line opens a fenced block after a ## Plan heading, nor anywhere in the file", {file});
  let plan;
  try {
    plan = JSON.parse(block);
  } catch (error) {
    fail("plan block in " + file + " is not valid JSON: " + error.message, {file});
  }
  const problems = planShapeProblems(plan);
  if (problems.length) fail("plan block in " + file + " does not fit the plan shape: " + problems.join("; "), {file, problems});
  return plan;
}
// What the parent hands over by file: the spec files' text and the plan
// file's block, read from one read-only capture of the source (the live
// tree outside Git) that is released before any agent starts.
async function handoff(source, given) {
  if (!given.specFiles && !given.planFile) return {};
  const context = await ctx({...source, readOnly: true});
  try {
    const spec = given.specFiles ? {spec: await readSpecFiles(context, given.specFiles)} : {};
    const plan = given.planFile ? {plan: await readPlanFile(context, given.planFile)} : {};
    return {...spec, ...plan};
  } finally {
    try { await release(context); }
    catch (error) { await log("handoff context retained: " + error.message); }
  }
}

polly.workflow("feature-implement", obj({
  name: nonblank,
  spec: str(),
  specFiles: arr(nonblank, {minItems: 1}),
  plan: planSchema,
  planFile: nonblank,
  source: str({minLength: 1}),
  checks: arr(nonblank),
  concurrency: int({minimum: 1}),
  drift: senum("paths", "tree"),
  reviewInstructions: str(),
  hostNotes: arr(nonblank),
}, {required: ["name"]}), async (given) => {
  // The pairs are settled before any effect: a plan comes inline or from a
  // file, never both or neither; the spec inline or from files, not both.
  if (!!given.plan === !!given.planFile) throw new Error("input needs exactly one of plan and planFile");
  if (given.spec !== undefined && given.specFiles) throw new Error("input takes spec or specFiles, not both");
  const source = given.source ? {source: given.source} : {};
  const input = {...given, ...(await handoff(source, given))};
  const checks = input.checks && input.checks.length ? input.checks : (input.plan.checks || []);
  if (!checks.length) await log("no checks configured; validation is reviewer-only");
  // Facts about the host, from the parent and from research, reach every
  // agent, joined at each wave by what agents published since.
  const baseNotes = [...new Set([...(input.hostNotes || []), ...(input.plan.environmentNotes || [])])];
  const ordered = waves(input.plan.tasks);
  // A check whose harness a task creates is skipped, the probe deciding, in
  // the waves before that task's; from its wave on the check must run, so an
  // editor that never created the harness fails it.
  const waveOf = new Map();
  ordered.forEach((wave, i) => wave.forEach(t => waveOf.set(t.id, i + 1)));
  const origins = new Map(checks.map(command => [command, checkOrigin(command, ordered.flat())]));
  const completed = [];
  const applied = new Set();
  // Checks a landed wave passed only because their packages ran at neither
  // commit; the caller runs them where the environment allows.
  const unverified = [];
  for (let w = 0; w < ordered.length; w++) {
    const wave = ordered[w];
    await log("wave " + (w + 1) + "/" + ordered.length + ": " + wave.map(t => t.id).join(", "));
    const deferrable = {};
    for (const [command, origin] of origins) {
      if (origin && waveOf.get(origin.task) > w + 1) deferrable[command] = {...origin, wave: waveOf.get(origin.task)};
    }
    const notes = withFacts(baseNotes, await publishedHostFacts());
    const hostNotes = notes.length ? {hostNotes: notes} : {};
    try {
      const rows = await parallel(wave, t => agent(
        ("implement " + t.id).slice(0, 80),
        "Implement your assigned task from the approved feature plan for '" + input.name + "'. Your input contains the plan summary, the feature spec, and your exact task with its brief, expected paths, and acceptance criteria. Work only in your assigned copy; sibling tasks are being implemented concurrently in their own copies and merged afterwards, so stay within your task's scope and do not fix unrelated issues. Follow the project's contributor documentation and imitate existing conventions. Run the relevant build and test commands yourself before finishing. A fact about this host that costs you time (a command that hangs, a runtime that is missing) is worth publishing with swarm_publish (kind host) for the other editors. Report a summary, the files you changed, and anything the reviewer should know. Do not commit or push." + notesSentence(notes),
        {...source, input: {name: input.name, spec: input.spec || "", planSummary: input.plan.summary, task: t, ...hostNotes}, schema: editorSchema},
      ), {concurrency: input.concurrency || 4, errors: "throw_after_all"});
      const refs = [];
      const submissions = [];
      for (let i = 0; i < rows.length; i++) {
        const task = await tasks.get(rows[i].value.task);
        refs.push({task: task.id, revision: task.revision});
        submissions.push({task: task.id, planTask: wave[i], report: rows[i].value.value});
      }
      const summary = landed(w + 1, wave, submissions, await integrateWave(input, {refs, submissions, checks, deferrable, notes}));
      completed.push(summary);
      wave.forEach(t => applied.add(t.id));
      for (const check of summary.integration.checks) {
        if (check.unverified) unverified.push({wave: w + 1, command: check.command, packages: check.unverified});
      }
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
        return {status: "recovery_required", name: input.name, waves: completed, unverified, candidate: error.result.candidate, stopped};
      }
      const remaining = input.plan.tasks.filter(t => !applied.has(t.id));
      // The tasks left depend on tasks already applied, which a relaunch
      // would refuse as unknown, so the plan handed back keeps only the
      // dependencies among the remaining tasks.
      const plan = {...input.plan, tasks: remaining.map(t => ({...t, dependsOn: (t.dependsOn || []).filter(d => !applied.has(d))}))};
      await log("wave " + (w + 1) + " stopped after " + completed.length + " applied wave(s); " + remaining.length + " task(s) remain");
      return {status: "incomplete", name: input.name, waves: completed, unverified, remaining: remaining.map(t => t.id), plan, stopped};
    }
  }
  return {status: "applied", name: input.name, waves: completed, unverified, finalChecks: input.plan.finalChecks || []};
});
