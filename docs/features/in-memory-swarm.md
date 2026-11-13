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
  calls it before it writes a result into the source tree, and before it
  removes the files of a workspace whose release it has recorded.
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
   (`BenchmarkStateCacheOneChanged`). A shared state is read-only: a test
   fails when a reader edits one.
3. **Done.** An update pays for the records it touches. `edit` hands its
   callback the family's own state; the callback takes each record it changes
   through an `own` method, which gives it a private copy, and adds and
   deletes records with `put` and `drop`. Those calls write the family's maps
   where they are and note what each record was, so the edit ends by encoding
   the noted records alone and a failed edit puts them back. The store writes
   them (`UpdateCoordinationFrom`). A runtime's edits take turns, and nothing
   reads the family's state during one. A reader has two ways in. `State`
   returns a state to keep: it shares the family's maps, so the next edit
   copies each map it writes before writing it. `look` runs a function on the
   family's own state between edits and costs later edits nothing; the reads
   a member makes on every model call go through it. The family's state also
   indexes mail by recipient, which the edits keep current, so finding a
   member's mail does not look at everyone's. Nothing else
   may write a `State` map: a test reads the package source for such writes,
   and after every edit a test also compares the whole family with its
   stored records and fails on a change the edit was not told about. A
   running process makes no such comparison: a change made around `own`,
   `put` and `drop` is not written. `update` remains as the whole-family form, for
   tests, for the two recovery passes that run when a runtime takes over a
   family, and as what `edit` falls back to against a store that cannot write
   a set of changes.
4. **Done for members.** An edit pokes the members its records concern: a
   mail's recipient, a member whose own record changed, the parked members
   whose task or one of its dependencies changed, and every active member
   when a publication changed. A parked member rechecks its wake conditions
   only then. The pokes are sent once the edit has let go of the family's
   state. The parent's waits still hear every change and then compare the
   whole family with what they saw before; that comparison is one pass over
   the records.
5. **Writer done; the interface is unchanged.** The SQLite store answers an
   `UpdateCoordinationFrom` from memory: it checks the session's lease by its
   expiry, applies the changed records to the family it holds, queues the
   records and any appended transcript messages, and returns. One goroutine
   commits everything queued in one transaction, in order. Every other
   operation that runs in a transaction first waits for the queue to empty,
   so it sees the rows the accepted updates produced: every other write to a
   session or a family, and every read made through a session or a view. The
   reads the store answers without a lease do not wait (the session list and
   summaries, a session's metadata by name, the last session used), nor do
   lease renewals and artifact reads. Of what those read, a queued update
   writes only a session's message count, last-used time and transcript
   identity, so the lease-free reads can report those as they were before
   the writer's next commit. The queue is bounded, and callers wait when it
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
| memory | 100 | 2371 | 1.4 ms |
| memory | 1000 | 2440 | 1.4 ms |
| memory | 5000 | 2143 | 1.5 ms |
| disk | 100 | 1719 | 2.5 ms |
| disk | 1000 | 1570 | 2.8 ms |
| disk | 5000 | 1065 | 3.2 ms |

With one transaction per edit and a flush to disk on every commit
(`synchronous=FULL`), 100 agents on disk managed 44.

The store's one SQLite connection sets the ceiling: a model call puts
roughly half a millisecond of work on it, so at the target of 500 calls a
second it is busy about a quarter of the time. Most of that is the queued
writes, about five updates a call at some 50 µs each, a quarter of it
rereading and rewriting the session's settings to record the transcript's
identity. The rest is what a slice does directly. A slice reads the member's
whole transcript: the connection is held for the read alone and the
messages are decoded after it is let go. It acquires and releases the member's session unless
it follows a yield within a second: a parked member keeps its session that
long, which is shorter than a lease heartbeat, so a prompt wake reuses it
and a long wait costs no renewal. Acquiring a parked member's session from
elsewhere waits out that second. With 5000 agents in step a member is parked
for longer than that, which is why the hold helps the smaller runs only.

Those transcripts are a few kilobytes. `SCALE_CONTEXT` makes each agent
take that many bytes of tool output in its first round and carry them from
then on. With 1000 agents in memory over ten rounds, 200 KB of transcript
each brings 2584 calls a second down to 1808: every slice still reads and
decodes its transcript, which costs about a core and keeps the connection
some 0.1 ms a read. Transcripts are not cached between slices; that would
hold every live transcript in memory a second time to save that.

What still grows with the family: a first slice counts every member for
its prompt, though it reads only the 32 it lists in full, the workspace
release pass visits every context and every task once, and a kept state
(`State`) makes the next edit copy the maps it writes. The family's state
indexes the roots its contexts use, as it does mail, so a slice names its
siblings' roots without looking at every context.

`SCALE_PARENT` adds a parent that waits on the swarm throughout, as
`wait_agent` does. With 5000 agents it takes about half a core rechecking
after each change and costs the members nothing measurable (2155 calls a
second); settling over the finished swarm takes 5 ms. Both find a task's
result notice through an index of mail by task, which the family's state
keeps and a kept state builds once for itself.

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
