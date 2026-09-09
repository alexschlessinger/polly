// /workflow /absolute/path/doc-drift-audit.js /absolute/path/input.json
// Input: {"source":"/repo","docs":[{"path":"README.md","label":"readme","focus":"models"}],"repair":true}
// Auditors are read-only. One editor pass repairs drift in an isolated copy;
// workers never commit, and the parent accepts the editing result.
const s = polly.schema;
const nonblank = s.string({minLength: 1, pattern: "\\S"});
const claim = s.object({id: nonblank, claim: nonblank, evidence: nonblank});
const enumeration = s.object({claims: s.array(claim, {minItems: 1})});
const verdict = s.object({
  verdict: s.enum("holds", "drifted", "stale", "unverifiable"),
  evidence: nonblank,
  suggestedChange: s.string(),
}, {allOf: [{
  if: {properties: {verdict: {enum: ["drifted", "stale"]}}},
  then: {properties: {suggestedChange: nonblank}},
}]});
const fix = s.object({status: s.enum("fixed", "skipped"), what: s.string()});
const ids = claims => claims.map(c => c.id);

// Acceptance records consumption of research, including negative findings.
// It does not certify a clean audit or accept an editing candidate.
async function acceptResearch(result) {
  const task = await polly.tasks.read(result.task);
  await polly.tasks.review({task: task.id, revision: task.revision, accept: true});
}

async function auditArea(input, area) {
  const label = area.label || area.path;
  const found = await polly.agent({
    label: label + " claim enumerator", readOnly: true, source: input.source,
    task: "Read the documentation file named in the input. Extract every claim a reader could act on: tool names, commands, flags, config fields, defaults, environment variables, and platform behavior. Give each claim a stable kebab-case id and document path:line evidence. Omit vague statements that cannot be checked against the code. An empty enumeration is incomplete, not a clean audit. You cannot edit anything.",
    input: {path: area.path, focus: area.focus || ""},
    schema: enumeration,
  });
  const claims = found.value.claims;
  s.keyed(ids(claims), verdict); // duplicate claim ids reject before any effect
  if (!claims.length) polly.fail("No claims extracted", {label, path: area.path, status: "incomplete", session: found.session, task: found.task});
  await acceptResearch(found);

  async function verify(previous, snapshot) {
    const review = await polly.agent({
      label: label + " verifier", readOnly: true, ...(snapshot ? {snapshot} : {source: input.source}),
      task: "Independently verify EVERY claim in the input against the documentation, actual code, tests, and repository instructions in your assigned copy. Trace each command, flag, and default to its implementation. Treat prior verdicts and editor reports as claims, not facts. A claim is drifted when the documentation disagrees with the code, stale when it describes removed behavior, and unverifiable when it cannot be established. Cite documentation and implementation path:line references as evidence, describe what was checked for unverifiable claims, and give an actionable suggestedChange for drifted/stale verdicts. You cannot edit or commit." + (snapshot ? " This is a repaired candidate: use original claim IDs to track each issue, but inspect the revised documentation. Mark holds when the current documentation correctly describes the code or removes an obsolete claim; do not require the old incorrect wording to remain true." : ""),
      input: {doc: area.path, claims, previous: previous || []},
      schema: s.keyed(ids(claims), verdict),
    });
    await acceptResearch(review);
    const unverifiable = Object.entries(review.value).filter(([, v]) => v.verdict === "unverifiable");
    if (unverifiable.length) polly.fail("Claims could not be verified", {
      label, path: area.path, status: "incomplete", unverifiable,
      session: review.session, task: review.task,
    });
    const drifted = Object.entries(review.value)
      .filter(([, v]) => v.verdict !== "holds")
      .map(([id, v]) => ({id, ...v}));
    return {review, drifted};
  }

  let {drifted} = await verify([]);
  let editor;
  if (drifted.length && input.repair !== false) {
    await polly.log("One editor pass for " + label + ": " + drifted.length + " drifted claims");
    editor = await polly.agent({
      source: input.source, label: label + " doc editor",
      task: "Repair documentation drift in your assigned isolated copy. Read the repository instructions, then update only the documentation file named in the input so every drifted claim matches the code. Preserve unrelated prose and files. Do not commit or push. Report every claim id, including anything skipped.",
      input: {doc: area.path, drifted},
      schema: s.keyed(ids(drifted), fix),
    });
    // A fresh verifier checks the complete claim set in the edited candidate.
    const candidate = await polly.snapshot(editor.context);
    ({drifted} = await verify(editor.value, candidate.id));
  }
  const summary = {
    label, path: area.path, claims: claims.length, drifted,
    task: editor && editor.task, session: editor && editor.session,
    status: drifted.length ? (editor ? "unresolved" : "drifted") : (editor ? "verified" : "clean"),
  };
  if (summary.status === "unresolved") polly.fail("Documentation drift remains unresolved", summary);
  return summary;
}

polly.defineWorkflow({
  name: "doc-drift-audit",
  inputSchema: s.object({
    source: s.string({minLength: 1}),
    docs: s.array(s.object({
      path: s.string({minLength: 1}),
      label: s.string({minLength: 1}),
      focus: s.string(),
    }, {required: ["path"]}), {minItems: 1}),
    repair: s.boolean(),
  }, {required: ["source", "docs"]}),
  run(input) {
    const paths = new Set();
    for (const doc of input.docs) {
      if (paths.has(doc.path)) polly.fail("Duplicate doc path", {path: doc.path});
      paths.add(doc.path);
    }
    return polly.parallel(input.docs, area => auditArea(input, area), {concurrency: 4, errors: "throw_after_all"});
  },
});
