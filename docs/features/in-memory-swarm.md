# In-memory swarm state with a write-behind store

Status: design sketch, 2026-11-09. Steps 1 to 4 of the sequence and the
writer of step 5 are implemented, and the step 7 benchmark exists; nothing
else is.

Target: 5000 concurrent agents in one process, about 750 of them writing
files, on one workstation. Everything else is negotiable. Sizing figures below
are estimates from stated assumptions, not measurements.

## Problem

Coordination state is stored and read as whole families. Answering a small
question (should this parent wake, is this mail delivered) means loading and
decoding the family, and noticing a change means rereading it. Making that
cheap inside the database needs derived tables, indexes and a revision poller
kept in step with every write. At thousands of agents the storage layer ends
up owning a second copy of the swarm's rules.

A swarm runtime already lives in one process, which holds the parent's
session lease exclusively. The database does not need to be the place where
live state is queried.

## Shape

Five parts.

1. **Memory is the truth.** Each family's records are held in memory by the
   store, below `CoordinationSession`: the parent's handle and every member's
   handle write the family through that interface, so that is where one owner
   can see every mutation. Above it the runtime keeps the decoded structs and
   the maps it asks about (undelivered mail by recipient, open tasks by owner,
   dependents by task). All reads and all wake checks hit memory.
2. **The database is a write-behind copy.** The existing tables stay:
   `swarm_records(parent, kind, id, payload)` and
   `messages(session, sequence, payload)`. One writer goroutine for the whole
   file collects dirty records and new messages and commits them every few
   milliseconds in one transaction. Recovery loads the family's records. There
   is no log, replay, compaction or derived table, and no schema change.
3. **Wakes are a poke.** Every agent has a one-slot channel. After a mutation
   the owner pokes only the agents named in the records it touched; each
   rechecks its condition in memory. Idle cost is zero.
4. **Two limiters.** One per provider for model calls (concurrency plus
   backoff on rate-limit responses), one for tool execution. A parked agent
   holds neither, so "concurrent agent" means "has a live turn".
5. **One seam for later.** Tools run through `Executor.Run(ctx, Job)`, where a
   `Job` is a command, a workspace and a sandbox policy. The only
   implementation is local: take a tool slot, call the sandbox factory. A job
   pool is a second implementation; nothing else needs to know.

## Store interface

```go
type Store interface {
    Load(ctx context.Context, family ID) ([]Record, error)
    Flush(ctx context.Context, b Batch) error // atomic
    ReadMessages(ctx context.Context, session ID, from, limit int) ([]Message, error)
    ListSessions(ctx context.Context, after ID, limit int) ([]Summary, error)
}

type Batch struct {
    Records  []Record // nil payload deletes
    Messages []Append
}
```

Transcripts and the session list are paged from disk and never held whole in
memory; the two read methods exist for them.

## Semantics

- **Crash window.** A hard crash can lose the last few milliseconds of state.
  This is accepted. The one rule: `Sync()` on the writer, which returns once
  everything enqueued so far is committed, is called before any irreversible
  host effect (applying a workspace, integrating a result). The runtime
  calls it before it writes a result into the source tree.
- **Atomic delivery.** A mail receipt and the transcript message it produced
  travel in the same `Batch`, so one transaction commits both.
- **Backpressure.** The writer's queue is bounded. If disk falls behind, owner
  goroutines block on enqueue; the system slows down without losing state.
- **Eviction.** A settled family leaves memory and is reloaded on demand, so
  memory tracks live work.
- **Viewers in another process.** Until the server process exists
  ([sketch](server-process.md)), a second `polly` viewing a family reads the
  database copy, which trails memory by one flush. The writer bumps a
  per-family revision on each flush; a viewer polls that one integer, and only
  while such a view is open. This is the only polling left.

## Workspaces

Read-only agents share the source tree and need no workspace. Writers keep the
worktree manager, with per-source locking and a higher cap (the default is
512). Consequences:

- Disk is linear in checkout size: a 74 MB tree costs about 55 GB at 750
  writers. A multi-gigabyte tree is where copy-on-write overlays
  (`bwrap --overlay` on Linux, clones on APFS) would be worth a second
  mechanism.
- Applying results into one source tree is serial per source, and conflicts
  grow with the number of writers. That is a property of the work's fan-in,
  not of this design.

## Sizing

Assumptions: 10 s average model round trip, 50k-token average context, 5 KB
average tool result.

| Load | Estimate |
|---|---|
| Model requests | 500/s, about 100 MB/s of request bodies |
| Stream events to parse | about 250k/s |
| Coordination mutations | a few thousand/s |
| Transcript writes | about 1,500 rows/s, 10 MB/s |

Provider quota is the real ceiling and is the operator's to supply. Tool
execution is bounded by cores through the tool limiter, not by agent count.

## Deliberately left out

- The server process. Parts 1 to 5 work inside today's `polly`.
- Overlay workspaces, until checkout size or writer count demands them.
- Sharded transcript files, unless one writer cannot hold the write rate.
- Shared MCP servers and aggregated UI views. Both are needed at 5000 agents;
  neither shapes the core.

## Sequence

1. **Done.** The store holds each family's records in memory. A read or an
   update no longer scans the table, and an update still writes its changes
   before it returns. SQLite's `data_version` tells the store when another
   connection committed, in this process or another, and it then rereads.
   Callers still get a private copy of the records, as the storage contract
   requires. With 30,000 one-kilobyte records a read went from 84 ms to 4 ms
   and a one-record update from 87 ms to 17 ms
   (`BenchmarkCoordinationFamily`); what remains is that copy and the
   comparison that finds the changed records.
2. **Done for reads.** The runtime's reads share one decoded state. The store
   gives each family's records a revision; while it stands a read costs one
   lease check, and after a change the runtime decodes only the records whose
   bytes changed. With 40 members carrying tens of kilobytes each, a read
   after a one-record change went from 2.8 ms to 0.13 ms
   (`BenchmarkStateCacheOneChanged`). A shared state is read-only: tests fail
   when a reader edits one, and a running process checks about once a second
   and returns to the stored records with a warning.
3. **Done.** An update pays for the records it touches. `edit` hands its
   callback the shared state; the callback takes each record it changes
   through an `own` method, which gives it a private copy, and adds and
   deletes records with `put` and `drop`. Those calls copy a map the first
   time the edit writes it and note the record, so the edit ends by encoding
   the noted records alone. The store writes them (`UpdateCoordinationFrom`)
   and the shared state moves forward by the same set. A runtime's edits take
   turns, so they never find each other's changes unexpected. Nothing else
   may write a `State` map: a test reads the package source for such writes,
   and every edit in a test also compares the whole family and fails on a
   change it was not told about. A running process makes that comparison
   about once a second while the family has at most 4,096 records, writes
   what it finds and warns; between comparisons an unannounced change to a
   record is not written. `update` remains as the whole-family form, for
   tests, for the two recovery passes that run when a runtime takes over a
   family, and as what `edit` falls back to against a store that cannot write
   a set of changes.
4. **Done for members.** An edit pokes the members its records concern: a
   mail's recipient, a member whose own record changed, and every active
   member when a task or publication changed. A parked member rechecks its
   wake conditions only then. The parent's waits still hear every change.
5. **Writer done; the interface is unchanged.** The SQLite store answers an
   `UpdateCoordinationFrom` from memory: it checks the session's lease by its
   expiry, applies the changed records to the family it holds, queues the
   records and any appended transcript messages, and returns. One goroutine
   commits everything queued in one transaction, in order. Every other store
   operation first waits for the queue to empty, so it sees the rows the
   accepted updates produced. The queue is bounded, and callers wait when it
   is full. `Sync` waits for the queue and then for the disk. An update that
   pins artifacts or registers members is written before it returns. About
   every tenth of a second one operation per family goes to the database, to
   notice a commit made through another connection. If a queued batch cannot
   be written, twice, the store stops accepting coordination updates.
6. `Executor` and the two limiters; raise the 32-member default.
7. Benchmark at 5000 agents with a scripted model, with budgets that fail the
   run: commit under 100 µs in memory, wake under 1 ms, open to first paint
   independent of transcript length. Then decide on overlays and sharding.
   `BenchmarkSwarmAgents` is the first part: read-only agents in a ring, each
   round a model call, a mail, a park, a wake and a delivery for every agent,
   against a model that answers at once. It has no budgets yet.

## Measurements

`BenchmarkSwarmAgents`, three rounds, on a 16-core workstation:

| Store | Agents | Model calls/s | Spawn per agent |
|---|---|---|---|
| memory | 100 | 1803 | 1.8 ms |
| memory | 1000 | 1344 | 1.5 ms |
| memory | 5000 | 860 | 2.0 ms |
| disk | 100 | 1182 | 2.3 ms |
| disk | 1000 | 910 | 2.7 ms |
| disk | 5000 | 573 | 3.7 ms |

Before the writer, with one transaction per edit, the same runs gave 493
calls a second at 5000 agents in memory and about 360 on disk, where most
agents then failed: their final edits queued for longer than the five seconds
each allows itself. With a flush to disk on every commit
(`synchronous=FULL`), 100 agents on disk managed 44.

Each model call costs about six edits and a runtime's edits are one at a
time, so edits per second is the ceiling. What still grows with the family is
that every edit copies each map it writes; removing that needs maps that
share structure between versions, or smaller families.

Every agent, read-only or not, claims a scratch directory, and those share
the worktree cap (512 by default), so a larger swarm has to raise
`MaxWorktrees`.

## Open questions

- One family of 5000 members or many small ones? One large family makes the
  single owner goroutine and the load time the first things to benchmark.
- Per-agent cache of the serialized request prefix: it avoids re-encoding the
  context every round trip and keeps provider prompt caching intact, at about
  1 GB for 5000 agents. Build with step 3 or wait for the benchmark?
- Databases already migrated to schema 10 by a build of the storage stack need
  either a forward migration that drops the added tables or to be treated as
  disposable.
