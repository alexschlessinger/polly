---
name: feature-workflow
description: End-to-end feature development — grill the user into an approved spec, fan out research into an implementation plan, then implement in parallel dependency waves with review and integration
---

# Feature workflow

Three phases with a hard human gate before implementation. All artifacts live
in `docs/features/<name>.md` (`<name>` is kebab-case) inside the project, so
they survive context trimming and can be committed. The two workflow scripts
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

When converged, write `docs/features/<name>.md` with sections: Problem,
Goals, Non-goals, Design, Edge cases, Acceptance criteria, Open questions.
Get explicit user approval of the spec before phase 2.

## Phase 2 — Research fan-out (workflow, read-only)

Run `{baseDir}/feature-research.js` with input `{"name": "<name>", "spec":
"<full spec text>", "source": "<project root>"}`. It captures the project
once, so every agent reads the same code even if files change meanwhile, fans
out read-only researchers (codebase, conventions, verification — build,
checks, and testing — external prior art via curl, docs/config) and
synthesizes a wave-ordered implementation plan, including the `checks`
commands implementation must pass. Researchers inherit the default sandbox
preset (`workspace+net+git`), so curl works.

It returns `plan`, `commit` (the capture the plan was verified against;
absent outside Git), `research` (per lens: the task id, a summary, and its
unknowns), `gaps`, and `repairs`. The full reports are not in the output:
read one with `swarm_read({view:"tasks", id:"<task>", section:"result"})`
when the plan leaves a question its summary does not answer. A lens in `gaps`
failed and the plan was made without it — tell the user which, and why. Its
researcher is paused with the investigation intact (the gap names its
`session` and `task`), and only the user can resume it. A
plan the workflow could not execute as written (duplicate ids, unknown or
cyclic dependencies, concurrent tasks sharing a path) was sent back to the
synthesizer up to twice; `repairs` counts that. A failed run carries the
same `research` and `gaps` in its error result, plus the last `plan` and its
`problems` when validation was what failed, so nothing needs re-running to
see what was learned.

When it returns:

1. Append the plan to `docs/features/<name>.md` as a `## Plan` section
   (summary, the `commit` it was verified against, checks, final checks,
   tasks with ids/briefs/paths/dependencies/acceptance, docs updates, risks,
   open questions). Copy each task's brief whole: it is written to stand on
   its own, and an editor sees nothing else.
2. Present the user a short summary plus every open question, risk, and gap.
3. **GATE: stop.** Wait for explicit approval. The user may edit the plan
   file directly; re-read it after they do. Only then phase 3.

## Phase 3 — Implementation (workflow, editing)

Run `{baseDir}/feature-implement.js` with input `{"name": "<name>", "spec":
"<spec text>", "plan": <plan object>, "source": "<project root>"}`. Read the
plan back from the file rather than trusting conversation memory. The
workflow runs editing workers in dependency waves (parallel within a wave),
then per wave: merge → single reviewer + the plan's `checks` → bounded repair
→ integrate. The next wave starts only after integration. Pass `"checks":
[...]` in the input to override the plan's commands; with no checks at all,
validation is reviewer-only — confirm that with the user first. A failing
check is re-run on the commit the wave merged onto, and one whose named
failures all failed there too does not block: a check list that a worker's
environment cannot satisfy costs the wave nothing, so prefer the project's
real commands over a list narrowed to what you expect to pass. A failure
that names no test (a compile error, a panic, an unfamiliar runner) always
blocks, because nothing ties it to the baseline.

When it returns:

- `status: "applied"`: update `docs/features/<name>.md` with a status line,
  run the returned `finalChecks` (the plan's suites too slow or too
  environment-bound for every wave, which no wave ran) and the project's own
  verification commands yourself, and summarize what landed per wave.
- `status: "incomplete"`: earlier waves are applied and a later one stopped.
  `waves` is what landed, `stopped` says which wave failed and why,
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
