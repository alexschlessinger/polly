# One Polly server process for all clients

Status: design sketch, 2026-11-04. Nothing implemented.

## Problem

Every `polly` launch is a whole runtime: it opens `~/.pollytool/polly.db`,
takes exclusive session leases, builds a model router, loads skills, the tool
registry and MCP servers, probes the sandbox, and runs the agent loop, swarm
members, workflows and every sandboxed child inside that one process. The
frontend (TUI, line, one-shot) lives in the same process, so:

- a turn dies with its terminal; closing a tab mid-turn vetoes persistence
  ("turn detached; not saved");
- a session can be driven from one terminal only, and a second `polly` on the
  same session waits on the lease;
- one-shot `-p` pays the full open cost (router, catalog prefetch, skills, MCP
  servers, sandbox probe) on every call;
- a swarm parent must stay in the foreground for its members to run;
- stdio MCP servers and sandboxed children are duplicated per launch and torn
  down with the terminal.

## Shape

One long-lived `pollyd` per user owns everything that holds a lease or spawns
a process. Clients are thin: they parse flags, own the terminal, render, and
answer approvals. A client attaches over a Unix socket; the same binary serves
both roles (`polly serve`, autostarted by the first client).

```
 polly (TUI)  polly -p  polly --last   editor/web later
     │           │          │              │
     └───────────┴──────────┴──────────────┘  Unix socket, JSON frames
                        pollyd
   sessions.Store (sole lease holder) · llm.Agent per session
   swarm.Runtime per parent · workflows · MCP servers · sandbox children
```

Embedded mode stays: without a daemon (tests, `--shot-script`, `--shot-fixture`,
CI, `POLLYTOOL_EMBEDDED=1`) the client links the server core and the transport
is a direct call. The seam is the same code either way.

## What already lines up

The codebase has most of the server half already; the missing piece is the
wire, not the model.

| Need | Exists today |
|---|---|
| Client boundary for a running turn | `TurnUI` (cmd/polly/turn_ui.go): 18 methods, one request/response path (`ApproveToolCalls`) |
| Agent boundary | `llm.AgentCallbacks`; `AdmitInput` already lets peer input enter a running turn |
| Lease-free reads with change detection | `sessions.ViewStore.ReadView(target, knownRevision)`, used by child-view and parent tabs |
| Exclusive writer with crash recovery | session leases (5 s heartbeat, 20 s stale), `recoverParent`, `recoverApplies` |
| Parked decisions with no UI present | swarm members without a host UI already park approvals; `hostTurnUI` fallback |
| Child processes outliving a turn | `adoptProcessGroups` on the swarm runtime |
| Short socket-safe path | scratch root `/private/tmp/polly-<uid>/` was chosen for exactly this |
| Durable file transfer between processes | content-addressed artifact store |
| Scripted event source | `llm/replay` bus and provider |

## Protocol

Newline-delimited JSON over a Unix socket at `<scratch root>/pollyd.sock`
(mode 0600, directory 0700). No new dependency: `messages.ChatMessage`,
`StreamEvent` and tool calls already serialize. First frame each way is a
`hello` with protocol and build version.

Client to server:

| Request | Maps to |
|---|---|
| `open {root, context, flags, env subset, capabilities}` | `conversationOpener.openNew`; returns session id and current `ReadView` revision |
| `attach {session, since}` | subscribe to live events from sequence `since` |
| `input {session, message, images}` | `executeTurnWithUserMessage`, or `AdmitInput` when a turn is running |
| `approve {session, request id, decisions}` | the pending `ApproveToolCalls` |
| `cancel {session}` | turn context cancel |
| `command {session, name, args}` | `/set`, `/compact`, `/spawn`, `/sandbox`, `/add-dir` ... |
| `list`, `view {target, revision}` | `sessionStore` listing, `ReadView` |
| `close {session}` / `detach` | release lease / drop subscription |

Server to client: one event type per `TurnUI` method (`thinking`, `text`,
`tool_start`, `tool_end`, `tool_result`, `tool_media`, `warning`, `notice`,
`tokens`, `cost`, `context_usage`, `turn_complete`, `persistence_*`) plus
`approval_request`, `view_changed {session, revision}`, `member_*` for swarm
rows, and client-capability requests (below). Every event carries
`(session, turn, seq)`.

The server keeps each in-flight turn's events in memory until the turn
persists, then drops them: history comes from `ReadView`, the live tail from
the buffer. A reconnecting client sends its revision and last `seq`; the
server answers with `view_changed` and replays the tail. Nothing new is
persisted for this.

Client capabilities are the handful of things that must run where the
terminal is: open a URL or image in the OS viewer (`browser.go`,
`repl_images.go`), read a clipboard image (`clipboard_image.go`), pick files
for `@` and `/attach`. These become server-to-client requests; in v1 both ends
share a filesystem, so paths travel as paths. Remote attach later uploads
through the artifact store.

## Semantics that change

- **Turns belong to the session, not the UI.** The persistence veto on a
  detached turn goes away; a turn runs to completion whether or not anyone is
  attached, and the audit's `interactive_stop` origin means a client asked for
  cancel, not that it went away.
- **Many viewers, serialized input.** Any number of clients may attach to one
  session. Input is queued by the server; a second client's input during a
  turn enters through `AdmitInput`, as peer mail does today.
- **Approvals broadcast and park.** A pending approval goes to every attached
  client; the first answer wins and the others see it resolved. With nobody
  attached it parks indefinitely, the session shows "needs decision" (the
  swarm status badge already draws this), and `polly --last` or the next
  attach presents it. No timeout.
- **Display contract per session.** `outputCapabilities` is resolved once per
  process today and composed into the system message. It becomes a session
  property set by the client that opened the session; a later client with
  narrower capabilities gets degraded rendering, not a changed contract.
  Changing it means `/set`, which already re-prepares the next request.
- **Sandbox policy is fixed at open.** Flags, environment, config file and
  workspace profile resolve on the server exactly as today, against the
  session's root rather than the daemon's cwd (the daemon has none; the
  "never run in `$HOME`" check applies to the root). An attach with
  conflicting sandbox flags is refused with the session's actual posture.
- **Credentials stay in the daemon.** The first client's environment seeds the
  daemon (keys, `POLLYTOOL_*`). Later clients pass per-session overrides, never
  keys. Nothing sensitive crosses the socket after spawn.

## Lifecycle

- **Autostart.** A client that finds no live socket takes a `flock` on
  `pollyd.lock`, forks `polly serve` detached from the terminal, and connects.
  Losing the race means another client started it; connect anyway.
- **Idle exit.** The daemon exits when no client is attached, no turn or
  member is running, and an idle timer (default 30 min, `--keep` disables)
  elapses. It releases leases and stops MCP servers first. Idle sessions with
  no attached client drop their registries (MCP servers, sandbox probe) after
  a shorter timer and reopen lazily on the next input, so the daemon does not
  accumulate one MCP fleet per workspace forever.
- **Crash.** Daemon death is today's process death: leases expire in 20 s,
  in-flight turns are lost, recovery runs on the next open. Clients see EOF,
  reconnect (autostarting), and re-project from `ReadView`.
- **Version skew.** `hello` carries the build. A client newer than the daemon
  asks it to exit when idle and, until then, runs embedded; it never speaks a
  protocol it does not know.

## Security

- The socket is same-uid only, under the 0700 scratch root.
- **Every sandbox masks the socket.** A sandboxed bash that can open the
  socket could open sessions with the user's full authority. The socket path
  joins the credential masks in `tools/sandbox` on every platform, and
  `docs/SANDBOX.md` records it. Nested members already get a scratch root
  inside their own grant, so the mask is a path rule, not a special case.
- Clients cannot widen a session's grants after open; `/add-dir` and
  `/sandbox` keep running through the existing commands on the server with
  the same checks.

## Code movement

`cmd/polly` splits along a line that mostly exists:

- **Server core** (new package, say `host`): `conversationState`, `turn*.go`,
  `conversation.go`, `storage.go`, `swarm.go`, `subagent.go`, `sandbox_*.go`,
  `skills.go`, `settings.go`, `agent_tools.go`, `theme_tool.go`'s writer side.
  It exposes `Open`, `Run`, `Admit`, `Approve`, `Cancel`, `Command` and an
  event sink that is the `TurnUI` of record. Flag parsing and `Config` stay
  in the client, which sends the resolved `Config` subset over the wire.
- **Client**: `repl_*.go`, `line_*.go`, `terminal.go`, `output_capabilities.go`,
  `browser.go`, `clipboard_image.go`, `process.go`, `polly.go`. `gotuiTurnUI`
  and `lineTurnUI` stay as they are; a decoder feeds them from events instead
  of direct calls. The tab model is unchanged: a tab is a session the client
  is attached to.
- **Transport**: `eventTurnUI` on the server encodes `TurnUI` calls into
  frames; `remoteTurnUI` on the client decodes them. In embedded mode the two
  are skipped and the client's `TurnUI` is passed straight in.

`swarm`, `workflow`, `worktree`, `subagent`, `sessions`, `tools` and `llm` are
untouched: they already run inside whatever process holds the parent, and the
daemon is simply that process.

## Sequence

1. **Event-shape the seam in place.** Introduce the frame types and
   `eventTurnUI` → decoder → `gotuiTurnUI` inside one process, so the TUI is
   already driven by the serialized stream. Headless shots and fixtures must
   render identically; that is the regression test. Remove the detached-turn
   veto here.
2. **`polly serve` and the line client.** One-shot `-p` and `--last` attach
   over the socket, with autostart and idle exit. Measures the open-cost win.
3. **TUI attaches.** Tabs over one connection, reconnect with tail replay,
   approvals broadcast and park, `view_changed` driving the existing
   `ReadView` projections. Client-capability requests for browser, clipboard
   and file pickers.
4. **Sandbox mask, idle registry release, version skew.** Docs: `CLI.md`
   (serve, embedded, `--keep`), `SANDBOX.md` (socket mask), `API.md` (host
   package).
5. **Later.** Remote attach over an SSH-forwarded socket with artifact-store
   uploads; web and editor clients speak the same frames.

## Open questions

- Should `list` and read-only views bypass the daemon and read the database
  directly, as `polly -l` does today? Cheaper, but two code paths; the lease
  design permits it.
- Does a parked approval need an operator-visible notification channel
  (bell, `polly --pending`), or is the status badge on next attach enough?
- Per-session idle release of MCP servers changes tool availability timing on
  the next input; is a reopen notice acceptable, or should release wait for
  explicit `close`?
- One daemon per user, or one per scratch root (`POLLYTOOL_SCRATCH_ROOT`
  already varies it)? Per root keeps test isolation trivially correct.
