---
name: simplify
description: Clean up recently changed code without changing its behavior — parallel read-only reviewers look for missed reuse, needless complexity, wasted work, and fixes at the wrong depth, then you apply what survives. Quality only, not a bug hunt. Use when the user asks to simplify, tidy, or clean up their changes, a diff, a branch, or specific files.
---

# Simplify

Make the changed code smaller, clearer, and cheaper while keeping what it does.
This is not a correctness review: do not go looking for bugs, and do not
"fix" behavior that looks odd but may be intended. If you trip over a real
bug, mention it in the summary and leave it alone.

## 1. Pin down the change

Work from a concrete diff, never from memory of the conversation.

- If the user named files, a commit, a range, or a branch, review exactly that.
- Otherwise, in a Git repository: take `git diff HEAD` (staged plus unstaged).
  If that is empty, take the branch against its upstream
  (`git diff @{upstream}...HEAD`), falling back to the default branch
  (`git diff main...HEAD` or `master...HEAD`), then to `git diff HEAD~1`.
- Outside Git, ask which files to review.

Include untracked files the user clearly means (`git status --short` lists
them). If the diff is empty, say so and stop. If it is very large (thousands of
lines, dozens of files), tell the user and offer to narrow it before spending
the fan-out.

Also note any project instruction files (`AGENTS.md`, `CLAUDE.md`, and
similar at the root or in directories above changed files). Reviewers should
respect their stated conventions and must not suggest changes that break them.

## 2. Fan out four reviewers

If `spawn_agent` is available, start four workers in parallel, all with
`read_only: true`, one per lens below. Each brief must be self-contained,
because workers do not see this conversation. Include:

- the diff itself (paste it; do not describe it), and the project root,
- the one lens it owns, copied from below,
- the instruction files it should read for conventions,
- the reporting format: one finding per item with `file`, `line`, a one-line
  `summary`, the `cost` (what is duplicated, wasted, or harder to maintain),
  and the concrete `fix`,
- the limits: stay inside the lens, report only issues the diff introduces or
  touches, no correctness bugs, no style nits a formatter or linter would own,
  and "no findings" is a fine answer.

Spawns return immediately and the reviewers run in the background; their
reports reach you at each step. Use the time:

- Run the project's usual checks (build, lint, format, tests) on the
  unchanged code, so that in step 4 you can tell a failure your fix caused
  from one that was already there.
- Read the changed files in full, so triage can judge findings against the
  surrounding code without another round of reads.

Do not start editing yet: a later report may cover the same lines. Once your
own work is done and reports are still outstanding, wait with `wait_agent`
rather than polling or sleeping.

If `spawn_agent` is not available, work through the four lenses yourself, one
after another, over the same diff, and give each a real pass; run the baseline
checks described below before you start editing. Say in the final
summary that this was a single-context review, not the parallel one.

### Lens: reuse

Code the diff writes that the project, its dependencies, or the standard
library already provides — helpers, types, constants, validation, formatting,
error wrapping. Search shared and utility packages and the neighbors of each
changed file before claiming something is new. Name the existing thing to call
instead and where it lives.

### Lens: simplification

Complexity the diff adds without earning it: state that can be derived, flags
that duplicate other state, near-identical blocks that differ in one value,
deep nesting that early returns would flatten, one-caller abstractions and
options nobody passes, dead code and stale comments left behind. Name the
simpler form that does the same job.

### Lens: efficiency

Work the diff makes the program do for nothing: repeated computation or I/O
that could be done once, lookups in loops that a map would answer, independent
operations run one after another, allocations or copies on hot paths, blocking
work added to startup, and long-lived closures or objects that hold on to more
than they need. Name the cheaper form and why it is cheaper. Skip
micro-optimizations on cold paths.

### Lens: altitude

Whether each change sits at the right depth. Look for symptoms patched at the
call site when the cause is in a shared mechanism, special cases bolted onto
general code, logic duplicated across callers that belongs in the callee, and
details leaking across a layer boundary. Prefer one general change to the
underlying mechanism over several special cases, and name where the change
belongs.

## 3. Triage

Merge the reports. Collapse findings that point at the same line or the same
mechanism, keeping the most concrete one. Drop a finding when:

- the fix would change observable behavior, output, error text, or a public
  interface,
- the fix reaches well outside the reviewed change,
- it contradicts a project instruction file, or
- reading the code shows it is wrong.

Keep a short note of each drop and the reason. Do not argue with the
reviewers; just record the call.

## 4. Apply

Fix what survives, directly and in the project's own style. Keep edits minimal
and local to each finding. After editing, run the same checks as the baseline
in step 2 and compare. If a fix introduces a new failure, revert that fix and
move it to the dropped list rather than chasing it. Failures that were already
there are not yours to fix; report them.

Do not commit unless the user asked you to.

## 5. Report

Keep it short:

- what changed, one line per applied fix (`file:line` and what it became),
- what was dropped and why,
- which checks ran, their result, and any failures already present before,
- whether this was the parallel review or a single-context pass.

If nothing was worth changing, say the change is already clean.
