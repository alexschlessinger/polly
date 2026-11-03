---
name: session-eval
description: Evaluate Polly sessions and their workers for harness reliability, development process, and the practical value of coordination. Use for requests such as evaluating the latest session, the last few sessions, or a named session. This is a session audit, not a general code review or permission to fix findings.
---

# Session Eval

Evaluate what happened, why it mattered, and whether the result was supported by
the work. Default to both harness and development-process evaluation; honor a
narrower requested focus. Judge communications by their effect on decisions,
defects, verification, or rework, not their volume.

## Establish the evidence

- Identify the requested root session(s) and their worker families. For "latest,"
  select roots by last activity, not whichever child wrote most recently. State
  the session name, time range, and whether the evidence is still in progress.
  Use stable IDs: titles and names can change. For multiple sessions, keep each
  verdict attributable before drawing cross-session conclusions.
- Inspect without resuming sessions, acquiring a Polly write lease, changing
  runtime state, or running another model turn. Preserve shared checkout work.
  An evaluation alone does not authorize code changes, commits, or publication;
  carry forward any explicit authorization already present in the conversation.
- For SQLite, take a consistent online backup through a read-only connection,
  then inspect the backup. Do not copy just the database file from a live WAL
  store, use `immutable=1` on it, or checkpoint/repair the live store.

Use the bundled standard-library helper when the store has Polly's SQLite schema:

```sh
python3 <skill-dir>/scripts/capture.py --latest 1
python3 <skill-dir>/scripts/capture.py --latest 3
python3 <skill-dir>/scripts/capture.py --session glad-raven
python3 <skill-dir>/scripts/capture.py --db /actual/path/polly.db --session SESSION_ID
```

The default database is `~/.pollytool/polly.db`; verify configuration if a different
store is in use. The helper writes a private temporary capture, checks database
integrity, and exports selected transcripts, settings, and coordination records.
It does not evaluate them. See [evidence.md](references/evidence.md) for the file
layout and interpretation. For another backend or schema, use its read-only
inspection path; do not migrate it to fit the helper.

Keep captures local and outside repositories. The snapshot contains the whole
store, including unselected sessions; transcripts and artifacts may be sensitive.
Read bounded selections instead of dumping provider payloads or all messages.
Re-check the source watermark once before reporting if the session is active;
label the cutoff and any unexamined continuation rather than silently mixing
snapshots. A lease or a missing final audit alone does not establish activity.

## Reconstruct the work

Start with actual user requests, parent updates/finals, worker assignments,
completion records, integrations, and validation commands. Build a compact
timeline with session IDs and message sequences. Read full tool output or
artifact bytes when a conclusion depends on content outside a preview.

Distinguish the harness running the session from code defects that its agents
were deliberately reproducing or fixing. A failing regression probe is not an
operational harness failure. Distinguish initial failures, successful recovery,
and unresolved problems; count the recovery's real cost when evidenced.

## Harness evaluation

Follow the evidence rather than mechanically running every possible check:

- Pair tool calls/results within their session and execution context. Investigate
  missing, duplicated, mismatched, or reordered records with replay/checkpoint
  boundaries in mind. Respect text-block phases and display IDs before calling
  repeated text duplicate output.
- Trace failures to their layer: provider response, retry policy, tool timeout,
  sandbox, storage, scheduler, or user cancellation. Separate request-local
  deadlines from whole-turn cancellation. A loaded test timing out is not proof
  of a production race; deterministic reproduction or a concrete path is stronger.
- Check task/execution transitions, follow-up launches, delivery receipts,
  integration outcomes, and release timing when relevant. A completed execution,
  delivered result, accepted task, and applied edit are different events.
- For suspected lost or truncated evidence, follow artifact references to owned
  bytes; verify length, chunk order, and digest as needed. Check published access
  as well as existence. A scratch path is not a durable handoff.
- Compare available turn audits with saved attempts and timing. Report absent
  coverage honestly: root-turn audits do not necessarily cover member runs,
  standalone commands, or abrupt termination. Never infer "no retries" merely
  because one audit is missing.

Verify current source only for a concrete mechanism or proposed fix. Use the
repository's retrieval rules and distinguish the session's captured revision
from today's checkout. Do not run a full suite as part of every session audit.
If a focused reproduction is necessary, keep it isolated from active work and
state precisely what it establishes.

## Development process and communications

- Compare the requested scope with the actual work and final claims. Account for
  user steering and authorization already present; do not treat a legitimately
  expanded assignment as scope drift.
- Evaluate task boundaries, ownership, dependency order, integration, and review
  by outcomes. Look for edits colliding, stale baselines, findings independently
  reproduced, or first-pass fixes corrected after review. Agent count alone says
  nothing about effectiveness.
- Trace useful exchanges as **question/evidence → response → changed action or
  verified result**. Negative answers that eliminate false findings can help.
  Separate demonstrated follow-through from a plausible benefit; do not credit
  a message with originating a discovery already underway.
- Treat undelivered mail according to intent and consequence. A late FYI with no
  missed decision is weak evidence for building acknowledgments or more status
  tooling. Ask what would actually have improved if it had arrived.
- Examine evidence handoffs, repeated probes, redundant validation, and time
  spent recovering missing artifacts. Identify the demonstrated avoidable work,
  not just an aesthetically untidy transcript.
- Match validation claims to exact commands, outputs, revision, and context.
  Distinguish focused tests, full suites, race/sandbox runs, browser/TUI checks,
  and CI. A parent passing a suite does not erase a worker-environment failure;
  a failing worker fixture does not establish a production security defect.

## Report and prioritize

Lead with the outcome. Present a small set of actionable findings with concrete
impact and pointers to session/message/tool evidence; include source locations
only when verified. Separate harness issues from process issues. Mention what
worked when supported, and give the important coverage limits.

For each recommended change, explain the observed failure or wasted work it
would prevent. Consider an existing mechanism or workflow adjustment before new
harness machinery. "No change warranted" is a valid conclusion. Avoid arbitrary
scores or productivity percentages unless requested and their basis is explicit.

Do not repeat old findings as current without checking recurrence or present
code. When asked to show conversations, quote actual assignments, messages and
completion notices with direction and sequence/time, preserving queued versus
admitted status. Do not substitute private model reasoning for communications.
Use a browsable artifact only if it materially helps the requested inspection;
a concise text evaluation is the default.
