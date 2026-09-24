---
name: feature-workflow
description: End-to-end feature development — grill the user into an approved spec, fan out research into an implementation plan, then implement in parallel dependency waves with review and integration
---

# Feature workflow

Three phases with a hard human gate before implementation. All artifacts live
in `docs/features/<name>.md` (`<name>` is kebab-case) inside the project, so
they survive context trimming and can be committed. A program built from
scratch keeps its program spec there and one file per milestone beside it,
and a tree with nothing to build or test takes the greenfield path below
before any research. The two workflow scripts
ship in this skill directory; run each with `workflow_run({"skill":
"feature-workflow", "path": "<script>.js", "input": "<JSON>"})` and the host
reads the file. Never pass a script's text as `source` — a retyped copy
differs from the file — and never spawn agents to execute one.

## Phase 1 — Brainstorm and grill (interactive, you and the user)

This phase is a conversation, not a form. One topic per message; follow up on
vague answers instead of moving on. Cover, in roughly this order:

- **Problem**: what hurts today, who feels it, what breaks if we do nothing.
- **Scope**: goals and explicit non-goals. Push for non-goals — "everything
  else" is not an answer.
- **Design**: proposed shape, alternatives considered and rejected (and why),
  compatibility with existing behavior, new configuration surface.
- **Edge cases and failure modes**: empty inputs, errors, concurrency,
  platform-specific variants, security or sandboxing implications.
- **Verification**: how the user will know it works; which tests must exist.

Grilling means challenging: ask "why not the existing X?", "what's the
cheapest version of this?", "what breaks if we skip it?". Don't accept the
first answer to scope questions.

Keep a list of facts about this host you learn while probing (a command that
never exits, a runtime or browser feature that is missing, a tool that needs
a flag here). They become `hostNotes` for both workflows, so no researcher or
editor burns a timeout rediscovering them. Workers publish the host facts they
learn (`swarm_publish` with kind host): those reach you as peer messages, and
both workflows read them themselves, so `hostNotes` carries what you learned.

When converged, write `docs/features/<name>.md` with sections: Problem,
Goals, Non-goals, Design, Edge cases, Acceptance criteria, Open questions,
and Environment notes (the host facts so far).
Get explicit user approval of the spec before phase 2.
On a greenfield tree, run the foundation grill below before writing the
spec; the spec then also gets Foundation and Milestones sections, and phase
2 starts only once the foundation has landed.

## Greenfield — decide the foundation before research

The tree is greenfield when nothing in it can be built, tested or imitated:
`git ls-files -co --exclude-standard` lists only a spec, a README, a license
or ignore files. An unborn repository captures fine, but research over an
empty tree finds nothing: a lens with no subject reports its absence and
stops, or, worse, fills the gap with design opinions. A greenfield tree is
short on decisions, not information, so the decisions come first, with the
user, and research runs afterwards against real code.

**Foundation grill.** After the feature topics, one topic per message:

- **Stack and runtime**: language, version, what runs the code and the tests.
- **Layout**: where source, tests, docs and tooling live; one unit or several.
- **Commands**: build, test, lint, format; which run on every change and
  which once at the end; what a sandboxed copy cannot run.
- **Dependency policy**: what may be pulled in, how it is pinned, what is off
  limits.
- **License and CI**: which commands CI runs, on which platforms.
- **Milestones**: the smallest sequence where each milestone leaves the
  program runnable and verified. Milestone one is the architecture, its
  contracts and one end-to-end path.

Propose defaults from the spec; the user approves or edits them.

**Write it down, then land it.** Record the decisions in a contributor doc at
the project root, `AGENTS.md` or `CONTRIBUTING.md` (the conventions lens looks
for exactly those): stack, layout, commands, policies, and the milestone list.
Then write the smallest skeleton whose commands pass: the build configuration,
a placeholder entry point, one trivial test, and a stub proving the hardest
part of verification runs on this host (the probe from phase 1: a browser
launch, a device, a service). The skeleton proves the commands, not the
design: no domain code; everything else is a milestone. Run every command
yourself in the user's tree. A command that hangs, needs a flag, or cannot
run here is a host fact for `hostNotes`, and a command a plan will list as a
check must exit 0 here. Show the user what you wrote, get explicit approval,
and ask the user to commit the foundation before milestone one: a commit
gives every capture a real baseline and keeps `git status` after
implementation showing only the milestone. After that the tree is no longer
greenfield, and phases 2 and 3 run per milestone against real code.

**Milestone files.** The program spec stays at `docs/features/<name>.md`
with `## Foundation` (the decisions and the alternatives rejected) and
`## Milestones`. Each milestone gets `docs/features/<name>-<n>-<slug>.md`
(kebab-case, at most 64 characters, so keep `<name>` short) holding the
phase 1 sections scoped to it, and runs phases 2 and 3 with `"name":
"<name>-<n>-<slug>"` and `"specFiles": ["docs/features/<name>-<n>-<slug>.md",
"docs/features/<name>.md"]`. The milestone file takes the `## Plan` section and the
status line; the program spec gets one status line per milestone as it
lands. Finish a milestone — final checks run, result approved — before
grilling the next: the next one's research reads what this one landed.

**Milestone one.** Run research with the default lenses plus
`{"id": "architecture"}` and with this planning note:

```json
{"lenses": [{"id": "codebase"}, {"id": "conventions"}, {"id": "verification"}, {"id": "architecture"}, {"id": "external"}, {"id": "docs-config"}],
 "planningNotes": ["Make wave one a single task that lands the contracts every other task builds on: the shared types, interfaces and signatures from the architecture report, with a stub for each component in the file the task implementing it will own, so that the build and the existing checks pass on the stubs alone; make every other task depend on it, and give no two later tasks the same path."]}
```

Later milestones use the default lenses, unless one lays down new structure:
then the same lens and note apply, as they do for a feature that adds a
subsystem to an existing project.

## Phase 2 — Research fan-out (workflow, read-only)

Run `{baseDir}/feature-research.js` with input `{"name": "<name>",
"specFiles": ["docs/features/<name>.md"], "source": "<project root>",
"hostNotes": [...], "lenses": [...], "planningNotes": [...]}`, with the host
facts from phase 1; `lenses` and `planningNotes` are optional. `specFiles`
are paths relative to the project root, read in order from the same capture
the researchers get, each up to its `## Plan` heading (a plan left in the
file by an earlier run is not spec) and joined with a blank line; for a
milestone give `["docs/features/<name>-<n>-<slug>.md",
"docs/features/<name>.md"]`. `spec` still takes the text inline; give
exactly one of the two. It captures the project
once, so every agent reads the same code even if files change meanwhile, fans
out read-only researchers (codebase, conventions, verification — build,
checks, and testing — external prior art via curl, docs/config) and
synthesizes a wave-ordered implementation plan, including the `checks`
commands implementation must pass. Researchers inherit the default sandbox
preset (`workspace+net+git`), so curl works.

`lenses` picks the researchers; five run by default. Name a subset, or add
one, with `{"id": "<id>"}` entries from the catalog — `codebase`,
`conventions`, `verification`, `external`, `docs-config`, and
`architecture` (module decomposition with concrete interfaces and which
parts are independent; required when named, not a default) — optionally with
your own `focus` or `required`. A catalog id keeps its required flag unless
you set one, and an id outside the catalog needs a `focus`. Drop a lens
whose subject this project does not have. `planningNotes` are sentences for
the synthesizer alone about the shape of the plan; researchers never see
them, and they are not in the plan.

It returns `plan`, `commit` (the capture the plan was verified against;
absent outside Git), `research` (per lens: the task id, a summary, and its
unknowns), `gaps`, `retries`, and `repairs`. The plan's `environmentNotes` are the host
facts research settled: your `hostNotes` plus what the researchers found,
which implementation passes to every editor, reviewer and repairer. The full
reports are not in the output: a report is Markdown (findings with their
paths and evidence, host facts under `## Host`, then recommendations); read
one with `swarm_read({view:"tasks", id:"<task>", section:"result",
pointer:"/report"})` when the plan leaves a question its summary does not
answer. A lens in `gaps`
failed and the plan was made without it — tell the user which, and why. Its
researcher keeps the investigation in its session (the gap names its
`session` and `task`); the user can resume it after the run. A required lens
whose researcher returned no valid report was retried once before that:
`retries` lists each (`{lens, reason, outcome}`: `continued` when its own
session answered, `fresh` when a new researcher did, `failed` when the gap
stands). A
plan the workflow could not execute as written (duplicate ids, unknown or
cyclic dependencies, concurrent tasks sharing a path) was sent back to the
synthesizer up to twice; `repairs` counts that. Every check was also run
once on the captured commit: `preflight` lists each with its exit code and
the number of tests already failing there. A check whose command names a
file a plan task creates (a smoke harness the feature adds) is not run
there: its `preflight` entry carries `createdBy` and `missing` instead, and
implementation skips that check until the creating task's wave, then
requires it. A check that failed naming no
test, whose packages could not set up or build, or that left files behind
went back too; one still like that after the repairs is in `checkProblems`
beside the plan — present each at the gate, because implementation would
block every wave on it or verify nothing with it. A failed run carries the
same `research` and `gaps` in its error result, plus the last `plan` and its
`problems` when validation was what failed, so nothing needs re-running to
see what was learned.

When it returns:

1. Append the plan to `docs/features/<name>.md` (the milestone file, for a
   milestone) as a `## Plan` section
   (summary, the `commit` it was verified against, checks, final checks,
   tasks with ids/briefs/paths/dependencies/acceptance, docs updates, risks,
   open questions, environment notes). Copy each task's brief whole: it is
   written to stand on its own, and an editor sees nothing else.
2. Present the user a short summary plus every open question, risk, gap,
   and check problem.
3. **GATE: stop.** Wait for explicit approval. The user may edit the plan
   file directly; re-read it after they do. Only then phase 3.

## Phase 3 — Implementation (workflow, editing)

Run `{baseDir}/feature-implement.js` with input `{"name": "<name>",
"specFiles": [...], "plan": <plan object>, "source": "<project root>",
"hostNotes": [...]}`: the same `specFiles` as phase 2 (`spec` still takes the
text inline), the plan object, and the phase-1 host facts (the plan's own
`environmentNotes` are read from the plan). Read the
plan back from the file rather than trusting conversation memory. The
workflow runs editing workers in dependency waves (parallel within a wave),
then per wave: merge → the plan's `checks` → a single reviewer who reads
their results → bounded repair → integrate. A review names each required
change under an id, and the review after a repair receives the previous
verdict, each repair's report and the paths that changed, and must close or
carry every id. The next wave starts only after integration. Pass `"checks":
[...]` in the input to override the plan's commands; with no checks at all,
validation is reviewer-only — confirm that with the user first. A failing
check is re-run on the commit the wave merged onto, and one whose named
failures all failed there too does not block: a check list that a worker's
environment cannot satisfy costs the wave nothing, so prefer the project's
real commands over a list narrowed to what you expect to pass. A failure
whose output names nothing (an unfamiliar runner, a formatter's list) always
blocks, because nothing ties it to the baseline, and so does a compile error
or panic in a package that was fine on that commit. A package that failed to
set up or build there too does not block, but none of its tests ran on
either commit: the result returns that check in `unverified`. A check whose
harness a later wave's task creates is skipped until that wave (the wave's
check entry says `skipped`, with `createdBy`), and required from then on.

When it returns:

- `status: "applied"`: update `docs/features/<name>.md` with a status line,
  run the returned `finalChecks` (the plan's suites too slow or too
  environment-bound for every wave, which no wave ran) and the project's own
  verification commands yourself, and summarize what landed per wave: each
  entry in `waves` holds the editors' reports, the review (with its
  `requiredChanges` and `closed` ids), the repairs (each with its summary)
  and checks the wave passed (a check that failed on both commits shows its
  failure count), and a task's full record is readable with `swarm_read`. Run
  every `unverified` check too (`{wave, command, packages}`): its packages
  failed to set up or build in the workers' environment, so fix the
  environment the command needs and run it where it can reach them. All of
  these run in the user's own tree: afterwards `git status` must show only
  the feature's changes, so delete anything a command left behind.
- `status: "incomplete"`: earlier waves are applied and a later one stopped.
  `waves` is what landed and `unverified` names its checks to run yourself,
  as above; `stopped` says which wave failed and why,
  `remaining` lists the task ids still to do, and `plan` is the plan to
  relaunch with: the remaining tasks, with their dependencies on applied
  tasks already removed (a relaunch refuses a dependency it cannot see).
  Verify what landed, then relaunch with that `plan` — do not re-run the
  whole plan, and do not rebuild the task list by hand.
- `status: "recovery_required"`: earlier waves are applied and the stopped
  wave's integrate began without a confirmed outcome, so the parent files
  may hold part of it. Do not relaunch its tasks. Reconcile the `candidate`
  first (`workflow_run` with `polly.integration.reconcile(id)`), read the
  receipt, and only then decide between an explicit retry and repair.
- `status: "blocked"`: the first wave failed, so nothing was applied. Report
  the failure evidence (validations, candidate, retained contexts) and stop.
  Do not retry blindly — the blocker needs a human decision or a follow-up
  repair assignment.

## Gotchas

- Workflows are non-interactive once launched. All user interaction happens
  in phase 1 and the gate; never promise the workflow will "ask" anything.
- A failed editor fails the whole wave (`throw_after_all`); the failure
  details include the sibling editors' completed work, which can be
  integrated manually or continued with `followup_task`. It stops the run,
  but never discards a wave already integrated — that is `incomplete`, not
  `blocked`.
- Keep `docs/features/<name>.md` current after every phase transition — it
  is the durable memory of the feature.
- Never wrap a check in `test ! -f ... ||` or `|| true` to make it pass while
  its harness does not exist yet: name the harness path in the command and
  list it in the creating task's `paths`, and the workflows recognise it.
- Never run phase 2 on a tree with nothing to build: land the foundation
  first. A researcher whose subject does not exist reports the absence and
  stops, so the plan would rest on the architecture lens alone.
- `## Plan` is a reserved heading: both workflows read a spec file only up to
  it, so keep the spec sections above it and the plan block below.
