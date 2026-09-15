# Container-backed tools

**Status: implemented (2026-09-15) with the decisions recorded at the end.**
Builds on [runtime-backends.md](runtime-backends.md), whose first three
stages landed alongside it. A container is not another
`sandbox.Sandbox`; it is an `OpenTools` implementation whose tools all execute
inside a container that the binding owns. The agent loop, registry dispatch,
sessions, and Git integration are unchanged.

## Goals

- One confinement model on macOS and Linux: the container is the whole
  filesystem a tool can see. Nothing from the host home directory exists inside
  it, so the credential masks, private roots, and grant freezing of the native
  backends have nothing to protect.
- The workspace is either the host worktree bind-mounted at its own path, or a
  copy kept in sync with the host worktree for a remote Docker daemon.
- The container lives as long as the agent loop's binding and keeps state
  (installed packages, caches, background processes). It survives parking. A
  restart reconnects to it or destroys it; it is never trusted as the durable
  copy of anything.
- Bash, shell tools, file tools, `view_image`, stdio MCP servers, and skill
  scripts all run inside. Host-side execution of any of them is not offered by
  this binding.
- It is the intended default backend. The native backends remain for hosts
  without a reachable daemon and a configured image.

## Shape

```go
// tools/docker (proposed)
type Options struct {
    Image     string        // required; image reference, resolved to a digest at open
    Docker    string        // CLI path, default "docker"; podman/nerdctl accepted if compatible
    Mode      Mode          // ModeBind (default when the daemon is local) or ModeCopy
    Memory    string        // "--memory"; empty = unlimited
    CPUs      string        // "--cpus"
    PIDs      int           // "--pids-limit"; default 4096
    Network   NetworkPolicy // from the sandbox policy: allowed, denied, allowed-without-DNS
    GitIdent  GitIdentity   // user.name/user.email copied from the host
    Helper    string        // optional host path to a linux polly binary to copy in
    Labels    map[string]string
}

func OpenTools(opts Options) tools.OpenTools
```

`OpenTools(ctx, scope)` returns a `ToolBinding` whose `Registry` holds proxy
tools and whose `Close` detaches from the container without destroying it. A
separate `Destroy(ctx, scope)` removes it. Both are idempotent.

The binding is the only owner of the connection. Concurrent bindings for one
scope are refused, which is the same rule the coordinator already enforces for
workspace authority.

### The helper

`polly` runs inside the container as a helper process and hosts a real native
`ToolRegistry` bound to the container's worktree. The host registry holds one
proxy per remote tool. `ToolRegistry.ExecuteTool` on the host performs
approval, timeout, gate, exclusivity, and result processing exactly as today,
then the proxy forwards the approved arguments to the helper, which runs the
same tool code against the container filesystem. The loop never sees anything
but ordinary tools.

Consequences:

- File tool semantics are identical on both sides because it is the same code.
  `edit_file`'s matching rules, `read_file`'s limits, and `view_image`'s file
  and HTTP rules do not fork.
- Shell tools, MCP servers, and skills load inside the helper's registry from
  paths inside the container. The helper returns their schemas; the host
  mirrors them as proxies and applies `AllowedTools` and `Omitted` to the
  mirrored set. Staged skill activation commits on both sides in one round
  trip so `CommitPendingChanges` keeps its whole-batch behavior.
- Inside the container the helper's registry uses a `containerSandbox`
  factory: it applies environment filtering, `setsid`, and the scratch
  environment but no filesystem or network rules, since the container already
  provides those. The factory is constructible only in helper mode. This is the
  one sanctioned "runs a child outside bwrap/Seatbelt" path and `SANDBOX.md`
  must say so.

The helper protocol is JSON lines over the stdio of one `docker exec -i`. It
carries: hello (protocol and polly versions, worktree path, mode), load
(tool specs, skill roots, MCP specs), list (schemas and metadata including
`OutputTool`, `UntimedTool`, `ExclusiveTool`), execute (tool, arguments, deadline)
with streamed progress and a final result including media, cancel, sync
(described below), and a heartbeat. Cancellation on the host closes the
execution's deadline and the helper kills the process group; the exec stream
itself stays open. Losing the exec stream kills in-flight tool processes, which
is acceptable: the host turn is already gone.

Where the binary comes from: the image ships a `polly` and the hello exchange
checks protocol compatibility. `Options.Helper` names a host-side linux binary
to `docker cp` in before the first exec, for development and for images that
predate a protocol change. Either way, mismatch fails the open.

## Workspace transport

The host worktree is always the durable truth. In both modes the container can
be destroyed and rebuilt from it at any time; only container-local state
(packages, caches, ignored build outputs) is lost.

### Bind mode

The worktree is mounted read-write at its host absolute path, so host tools,
the TUI, `@file` references, and Git capture all agree on what a path means.
The worktree's `.git` is a file pointing into the owning repository, so the
owning Git directory is also mounted at its own path. The mount set is derived
from the same discovery the workspace preset uses (`RuntimeGitReadConfig` and
the Git workspace policy): the owning repository read-only, the worktree's own
entry under `.git/worktrees/<id>` read-write, hooks denied. Scratch is mounted
read-write at its host path and exported as `TMPDIR`, `GOCACHE`, and
`GOTMPDIR`, matching `scratchEnv`. Read-only members get a read-only worktree
mount and a writable scratch.

Nothing else from the host is mounted. `DeniedReads` and `DeniedWrites` in the
grant are satisfied structurally and the binding asserts that no mount falls
inside a denied read.

### Copy mode

For a daemon reached over `DOCKER_HOST`, or when selected explicitly. The
container holds a self-contained repository, not a worktree link:

1. Open streams a bundle of the base snapshot commit and checks it out at the
   scope's root path inside the container. Uncommitted host changes are then
   applied from a status-based diff (below). Git identity is written from
   values, never by copying the host `.gitconfig`.
2. After every proxied call of a tool that can write, the helper runs
   `git status --porcelain -z` in the container and streams the modified,
   added, untracked, and deleted non-ignored paths as a tar; the host applies
   it, deleting what was deleted. File tools report whether they wrote;
   bash, shell tools, and MCP tools are assumed to have written. This is what
   the runtime-backends contract calls synchronizing effects before returning,
   and it means host Git capture always sees the current tree. Ignored files
   never cross; capture would not see them either.
3. Host to container runs after any host-side write to the checkout: workflow
   integration, apply, snapshot restore, or a follow-up starting from a new
   snapshot. The coordinator calls the binding's `Resync` with the new base
   commit; the helper fetches a bundle for it, resets, and reapplies the host
   status diff. A binding with an in-flight execution refuses `Resync`; the
   coordinator already serializes these around the integration gate.

Sync failures fail the tool call with a `ToolError` that says the container
still holds the edit. The next successful sync or `Resync` reconciles it.

## Lifetime

One container per agent loop: each swarm member, the parent, and each
standalone or subagent run. Containers are named and labelled with
`polly.session`, `polly.root`, `polly.mode`, `polly.image` (digest), and
`polly.protocol`. Those labels are the only record of the container. Nothing
is written to session records, which keeps the runtime-backends rule of no
persisted backend identity.

- **Open** looks up the container by session label. If it exists and every
  label matches the scope computed now, it is started if stopped and
  reconnected. Any mismatch (different root, mode, image digest, or protocol)
  destroys it and creates a fresh one. A missing container is created.
- **Close** on park or normal completion detaches: the exec stream ends, the
  container keeps running. Background processes the model started survive
  under the container's init.
- **Destroy** happens when the coordinator releases the workspace, when a
  task completes and its member is done, on label mismatch at open, and from
  an explicit prune command. Standalone runs destroy on close.
- **Restart** of polly reconnects members that are resumed and leaves the rest
  until their next open or release decides. A prune command lists containers
  with polly labels whose session no longer exists and removes them; startup
  does not reap on its own.

The container's init is a minimal sleep so `docker exec` sessions can come and
go. Resource limits from `Options` apply at create time and are part of the
label set, so changing them recreates the container.

## Swarm placement

- Members: one container each over the slot's worktree, bind or copy. Sibling
  invisibility holds because no sibling path is mounted. The concurrency limit
  bounds the container count.
- Parent: a container over the live tree. Integration, apply, capture, and
  cleanup remain host Git operations run by the worktree manager through its
  native registry, as runtime-backends specifies. In copy mode the parent's
  binding receives `Resync` after each integration.
- Recovery reconstructs the scope from records and calls the same `OpenTools`;
  reconnect-or-destroy handles whatever container state exists.

## Environment and identity

The container environment is the image's environment plus the scratch
variables, the policy's explicit `env`, and the values of names listed in
`passEnv` or `allowEnv` copied from the host at open. The host's ambient
environment is never projected wholesale, and sensitive-name stripping still
applies to the copied names. Values are passed through a sealed file handed to
the helper over the exec stream, not through `docker run -e`, so they do not
appear in `docker inspect`. The Docker socket is still root-equivalent on the
host; the boundary is documented, not defended against.

Git identity inside the container is `user.name`, `user.email`, and
`commit.gpgsign=false`, written from host `git config` values. Credential
helpers, includes, and signing keys are not copied. `HOME` inside the container
is a per-container directory under a tmpfs.

Containers run as the host uid and gid with a read-only root filesystem, a
tmpfs `/tmp`, and dropped capabilities. Images must work as a non-root user.

Network follows the policy: `--network none` when denied; when allowed with
DNS denied, an empty resolver. `allowUnixSockets` and the `ssh` preset are not
supported by this binding in the first version; a policy that needs them fails
the open with a clear error.

## Image configuration

An image is named, never built implicitly. Precedence: `--sandbox-image`,
`POLLYTOOL_SANDBOX_IMAGE`, then a repository file `.polly/image` containing one
reference. A digest is resolved at open and recorded in the labels. A missing
image fails closed; polly does not pull silently.

The repository ships reference Dockerfiles under `docker/`: a base with git,
bash, coreutils, and polly, and variants adding Go, Node, and Python
toolchains. A `polly sandbox build` command builds a named variant explicitly.

## Backend selection

`--sandbox-backend {auto,native,docker}` and `POLLYTOOL_SANDBOX_BACKEND`.
`auto` is the default: docker when an image is configured and the daemon
answers, otherwise native with a startup notice naming the reason. Explicit
`docker` fails closed when unavailable, the same way `sandbox requested but
unavailable` does today. `--nosandbox` keeps its existing meaning and never
selects a container.

Host-side consumers that still need native tools after this lands are named in
the documentation rather than made to work by accident: the worktree manager's
administrative Git, and any host MCP configured for the parent's own registry.

## Implementation order

Prerequisite: runtime-backends stages 1 to 3, so `OpenTools`, registry
derivation without native effects, and `view_image` outside `NewAgent` exist.

1. **Helper and protocol.** Hidden `polly` subcommand, `containerSandbox`
   factory constructible only in helper mode, protocol types, proxy tool
   implementing `Tool` and `OutputTool`, and an in-process test transport that
   runs the helper as a goroutine with no Docker.
2. **Bind mode, standalone.** `tools/docker.OpenTools`, mount derivation from
   the Git workspace policy, labels, reconnect-or-destroy, resource limits,
   environment sealing. Prove it with the runtime-backends direct-agent path.
3. **Copy mode.** Bundle bootstrap, status-based sync-back, `Resync`, failure
   reporting.
4. **Swarm.** Parent and member placement, park survival, release destroys,
   recovery, `Resync` after integration, prune command.
5. **Assembly and docs.** `auto` selection in `cmd/polly`, image
   configuration, reference Dockerfiles and `polly sandbox build`, `SANDBOX.md`
   section, `README.md` flags, `WORKFLOWS.md` note on container placement.

## Tests

Unit tests use a scripted `docker` on `PATH` that records invocations and a
fake helper over the in-process transport: create, reconnect, mismatch
destroys, close detaches, destroy is idempotent, mounts never include a denied
read, sealed environment never appears in argv.

Integration tests are behind `POLLYTOOL_REQUIRE_DOCKER_TESTS=1` and run on the
local CI Docker workers. They cover: exec and file tools through the loop, no
host home visible, network denied and allowed, bind and copy modes producing
identical Git captures after the same edits, deletions synchronized, `Resync`
after a host commit, park then reconnect keeps installed state, restart with a
changed image destroys, two members cannot see each other's worktree or
scratch, and workflow integration of a container-made edit.

The runtime-backends shared contract suite runs against this toolset as well
as the native and in-memory ones.

## Limitations

- Host `allowUnixSockets` and SSH agent forwarding are unsupported.
- Ignored files exist only in the container in copy mode.
- Anything the model starts in the background dies when the container is
  destroyed, and the container is destroyed whenever the scope changes.
- Docker socket access on the host is host access.
- Bind mode requires a local daemon whose file sharing covers the runtime
  directory and the worktree.

## Decisions taken in the implementation

- **Engine API, not the docker CLI.** The daemon is reached over its socket
  (`DOCKER_HOST`, the active docker context, or the default socket; `unix://`
  and `tcp://` with TLS, no `ssh://`), so no host child process runs outside
  the sandbox factory and nothing sensitive appears in an argv. `Options.Docker`
  became `Options.Host`. The one CLI use is the explicit `polly sandbox build`
  command. Unit tests use an `httptest` fake daemon that runs the real helper
  over a hijacked stream instead of a scripted `docker` on `PATH`.
- **`.polly/image` is a hint only.** The flag or the variable selects the
  image; the file produces a startup hint naming it.
- **`auto` is silent when nothing is configured** and prints a notice when an
  image is configured but the daemon does not answer or the policy is
  unsupported. A missing image fails closed in both `auto` and `docker`.
- **Standalone runs destroy on close.** Swarm members and the parent keep
  their containers across parking (`OpenOptions.KeepOnClose`); workspace
  release destroys them. Containers are looked up by session and root, and
  bind mode admits several bindings over one root (a workflow's cached
  binding beside the member's own), each its own helper exec; copy mode
  admits one because it holds one synchronisation state.
- **Scope identity.** `tools.ToolScope.Session` carries the loop's session
  identity for the labels, and `tools.WorkspaceBackend` (`Resync`, `Destroy`,
  keyed by root) is how the coordinator reaches a backend's state; `swarm.Config`
  gained `Workspaces` and `OpenWorktrees`.
- **Helper delivery.** `Options.Helper` names a host Linux binary mounted
  read-only at `/run/polly/bin/polly` (bind mode) rather than copied in; copy
  mode relies on the image's own polly.
- **Copy mode staging** lives in the copy's volume (`<parent>/.polly-sync`),
  because the daemon's archive API cannot read a container's tmpfs, and the
  base commit travels under `refs/polly/copy-base/<commit>` because a bundle
  needs a reference. A later skill activation's MCP tools stay bounded by the
  binding's view, as they are natively; skills active at load are served.
- **Not in this version:** skill roots in copy mode (bind mode mounts them),
  `/tool add` style runtime reloads under docker (the protocol allows an
  additive load later), persisting skills activated inside the container, and
  `ssh://` daemons.
