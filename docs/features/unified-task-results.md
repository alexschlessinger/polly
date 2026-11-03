# Task output handoff

Status: implemented on the artifact store. Supersedes the
"unified retained task results" plan, whose implementation was stopped and
removed on 2026-11-02 in favour of this design.

## Problem

Workers lose their scratch when their workspace is released, so a parent that
needed a worker's reproduction, fixture or generated file had only what the
worker pasted into its result. `swarm_publish({files})` already captured named
files as durable artifacts on request, but relied on the worker remembering to
call it before completing.

## Design

One mechanism carries files out of a worker: the content-addressed artifact
store that already holds every conversation artifact (SHA-256 identity,
deduplicated chunks, per-session ownership, swarm pins, references passed in
messages and publications).

- **Output directory.** Every execution context with a scratch has
  `scratch/output`, created with the context and exported to its processes as
  `POLLYTOOL_OUTPUT_DIR`. It is inside the scratch grant, so no new sandbox
  grant, mask or cleanup path exists for it.
- **Capture at completion.** When a member completes, or fails for good, the runtime walks the
  directory, stores each regular file as an artifact of the member's session
  under the policy-checked read path `swarm_publish({files})` uses, names it by
  its slash-separated relative path, and pins the artifacts to the swarm.
  The references are recorded on the task revision (`Task.Outputs`) and on the
  `AgentResult`, and the completion notice lists them. Capture happens before
  the result is recorded, so the files are durable before anything can release
  the scratch. The directory is emptied only after the terminal result commits,
  under the same context lock as capture; a cleanup error does not invalidate
  the durable result.
- **Fail closed.** The limits are the existing ones: 16 files, 16 MiB per file,
  64 MiB in total. A symlink, a non-regular file, an oversized set, or a file
  whose size or modification time changes while it is read fails the
  completion with the reason; the task is blocked with that feedback and no
  partial or torn set is delivered. Failed pin or finalization transactions,
  pauses, cancellations, and pending follow-ups leave the originals for the
  resumed execution, which captures the current files rather than stale refs.
- **Read and restore.** The parent opens outputs with `read_artifact` and
  copies them with `restore_artifact({id,path})`, which refuses overwrites,
  verifies size and digest, and never executes files. Workflows see the
  outputs on the task record their step receipts return.
- **Before completion.** `swarm_publish({files})` remains for files a teammate
  needs mid-task or files outside the output directory.

Project changes are unchanged: editing members submit retained Git snapshots
that integration merges. Outputs are never merged into the project.

## Removed plan

The earlier plan built a private Git object database partitioned by hash
format, a transactional catalog with owners and leases, an owned-writer
quiescence barrier across five platform files, a capture journal with a
recovery engine, a retirement engine and a measurement harness, about 18,000
lines. Its storage win was delta compression of repeated project trees, which
stay in Git under both designs; outputs are raw bytes that gain nothing from
Git. Its isolation requirement (private outputs must not live in the shared
project object store) is met by the artifact store, which is already private
to the runtime. The removed tree is kept at `refs/backup/unified-results-full`.

## Limits

- Outputs are bounded regular files. Directories, permissions, links and
  larger payloads are out of scope.
- Durability is that of the SQLite store: a crash between capture and the
  recording of the result leaves the artifacts stored but unreferenced.
- A follow-up after a committed terminal result normally starts with an empty
  output directory. A pause or provisional final keeps the originals instead.
