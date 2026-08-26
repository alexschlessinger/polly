# Sandboxing

Polly runs LLM-driven commands — the builtin `bash` tool, schema-defined shell
tools, and stdio MCP servers — inside an OS-level sandbox by default. This
document explains what the sandbox is for, how the Linux and macOS
implementations differ, and how to configure and observe it. For the JSON
config field reference, see [API.md](API.md#sandbox-spec-reference); for the
shell-tool walkthrough, see [README.md](README.md#sandboxing).

## Intent

The commands polly executes are chosen by a language model, often steered by
untrusted input (web pages, file contents, MCP tool output). The sandbox limits
the blast radius of a hallucinated, prompt-injected, or simply buggy command.
By default, it enforces these boundaries; explicit policy grants can widen
them as described below:

- **Read credentials.** `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.kube`, `~/.netrc`,
  `~/.config/gh`, `~/Library/Keychains`, and the rest of the built-in deny
  list (17 paths) are blocked from reads, plus anything added with
  `--denypath`, unless an effective `readPaths` grant exempts an entry.
- **See secrets in the environment.** Credential-shaped variables
  (`POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`,
  ...) and agent sockets (`SSH_AUTH_SOCK`, `GPG_AGENT_INFO`) are stripped
  before exec unless explicitly delivered or allowlisted. Agent sockets matter:
  with `SSH_AUTH_SOCK` intact, a process can use your SSH keys without ever
  reading `~/.ssh`. The `ssh` preset re-admits exactly `SSH_AUTH_SOCK` (via
  `passEnv`) and the one agent socket it names (via `allowUnixSockets`), so
  agent-based SSH works while private keys stay masked.
- **Write beyond the effective writable set.** The filesystem is read-only
  except the OS temp dir and configured `writablePaths`. The CLI default adds
  the current workspace; the library `DefaultConfig` does not. Git metadata
  under a writable workspace is carved back out: whole trees read-only by
  default, or only the dangerous leaves with the `git` component (see the
  workspace/Git section).
- **Reach TCP/UDP unless the effective policy allows it.** Library
  `DefaultConfig` denies network access. The CLI deliberately defaults to
  `workspace+net+git`, so CLI tools can use outbound TCP/UDP unless the
  operator selects a tighter preset. Host filesystem Unix sockets remain
  blocked even when TCP/UDP is enabled, except any socket an
  `allowUnixSockets` grant names.

Design principles:

- **Default-on.** Tools don't opt in. Tool metadata cannot opt out by itself;
  the caller must explicitly select `--nosandbox` / `WithUnsafeNoSandbox`.
- **Fail closed.** If no backend exists, or a sandbox fails to construct, the
  tool errors instead of silently running unsandboxed. At startup polly probes
  the backend with a trivial command and aborts with a pointer to
  `--nosandbox` if it can't actually start processes.
- **Track the live filesystem.** Deny lists and profiles are rebuilt on every
  command, so a credential dir created mid-session (`aws configure` in another
  terminal) is masked by the next command. On Linux, denied directory entries
  are also reserved when a long-lived process starts, so later host-side
  creation or replacement cannot appear inside that existing namespace.
- **Observable.** Every decision is visible under `--debug` (variable and path
  names only — never values), and the REPL reports posture at startup and via
  `/get sandbox`.

This is containment for accidents and injection, not a hostile-binary jail:
the process runs as your user on a shared kernel. See
[Limitations](#limitations).

## What gets sandboxed

| Execution path | Sandboxed | Opt-out |
|---|---|---|
| Builtin `bash` tool | yes | `--nosandbox` |
| Shell tools (`-t ./tool.sh`) | yes | global `--nosandbox` only |
| Stdio MCP servers | yes (the whole server process) | global `--nosandbox` only |
| Remote MCP servers (HTTP/SSE) | no — the process runs elsewhere | n/a |
| Skill helper / function tools | no — in-process, nothing to wrap | n/a |

The effective config for a tool is the caller's **base config** merged with
global overlays and the tool's own `sandbox` object. Library callers commonly
start with `DefaultConfig` (temp writes, no network); the CLI starts with its
selected preset (`workspace+net+git` by default), then adds `--denypath`,
`--writepath`, and `--allownet` overlays. Merging is monotonic: booleans OR and
path lists append, so an overlay cannot remove an earlier choice. Grant fields
such as `allowNetwork`, `writablePaths`, and `readPaths` can widen access, while
`denyWrite`, `denyDNS`, `denyPaths`, `denyWritePaths`, and a strict `allowEnv`
can add restrictions.

Shell-tool `--schema` discovery is deliberately stricter than execution. Before
the schema can be trusted, discovery runs with private-temp writes only and no
network, workspace, read-path, or environment grants. Base `denyPaths`,
`denyWritePaths`, and `denyWrite` restrictions are retained.

## Platform implementations

Both platforms share the same policy surface (the `Config` fields) and ambient
environment filtering in Go. The pre-containment wrapper receives an empty
environment; the filtered target environment, including explicitly configured
values, is installed only after containment is active. The enforcement
mechanisms are otherwise very different.

Both built-in backends also require the managed wrapping API. Use
`WrapCmdManaged` or `WrapCmdWithEnvManaged`, then call the returned idempotent
cleanup after the process start/run method returns. This deterministically
closes backend-owned bootstrap, policy, mount-source, and environment
descriptors without touching caller-owned `ExtraFiles`. The legacy
cleanup-less `Sandbox.Wrap`, `WrapCmd`, and `WrapCmdWithEnv` calls fail before
mutating the command with `ErrManagedWrapRequired`; they cannot safely infer
when a caller later invokes `exec.Cmd.Start`. Custom descriptor-free sandbox
implementations remain compatible with those legacy helpers.

### Linux: bubblewrap (`bwrap`)

Each command is re-written to run under [bubblewrap](https://github.com/containers/bubblewrap)
in a fresh mount + PID namespace. The default config renders roughly:

```
bwrap \
  --ro-bind / /                          # entire filesystem read-only
  --tmpfs /tmp                           # private writable temp
  --tmpfs /run --remount-ro /run         # hide host runtime sockets
  --tmpfs /home/you/.ssh                 # denied dirs masked with empty tmpfs
  --ro-bind /dev/null /home/you/.netrc   # denied files masked with /dev/null
  ...                                    # one mask per denied path
  --dev /dev                             # fresh devtmpfs
  --proc /proc                           # proc scoped to the new PID namespace
  --unshare-pid                          # own PID namespace
  --unshare-ipc                          # own SysV/POSIX IPC namespace
  --unshare-net                          # no network (omitted when allowNetwork)
  --seccomp FD                           # deny AF_UNIX sockets + io_uring setup
  --cap-drop ALL                         # discard inherited launcher capabilities
  --die-with-parent
  --new-session                          # detach from the controlling tty
  -- /proc/self/fd/BOOTSTRAP_FD ...      # pinned post-containment bootstrap
```

Properties worth knowing:

- **The launcher has a fixed trusted route.** Polly executes only the regular,
  root-owned, non-user-writable `/usr/bin/bwrap`; a workspace or temp-directory
  `bwrap` earlier on `PATH` is never selected. Construction fails closed if the
  fixed backend is unavailable or mutable.
- **Target environment is installed only after containment.** bwrap itself gets
  a non-nil empty environment. The parent passes a pinned descriptor for its
  own executable plus a sealed anonymous NUL-delimited environment descriptor;
  after namespaces, mounts, and seccomp are active, the internal bootstrap
  closes those descriptors and directly `exec`s the original target with its
  exact argument vector and filtered environment. Target environment values
  never appear in bwrap's environment or argv; the original target argv is
  forwarded.
- **Writes are physically impossible** outside private temp and writable binds — the root
  is a read-only mount, not a policy check. Inherited capabilities are dropped,
  so a capability-bearing root launcher cannot remount those binds writable.
- **Host runtime state is private.** `/tmp` and `/run` are fresh mounts, so
  D-Bus, Docker/Podman, SSH-agent, Wayland, and similar host sockets are absent.
  Private roots are installed after writable ancestor binds, so a workspace or
  explicit write grant cannot cover them and reveal the host temp/runtime tree.
  A seccomp rule also denies `socket(AF_UNIX)` so sockets elsewhere in the broad
  read-only filesystem view cannot be reached. Private AF_UNIX stream
  `socketpair()` remains available for descendant IPC; reconnectable datagram
  and seqpacket pairs are denied. With networking disabled, socket creation is
  denied for every address family except that private stream pair.
- **Denied paths read as empty**, not as errors: a masked directory lists
  nothing (tmpfs), a masked file reads zero bytes (`/dev/null`). Tools probing
  for credentials see "not configured" rather than "blocked".
- **`--unshare-pid`** means `/proc` only shows the sandboxed process tree.
  Without it, any same-UID process's `/proc/<pid>/environ` is readable —
  including polly's own `POLLYTOOL_*` API keys, which would defeat the env
  stripping.
- **`--new-session`** detaches the controlling terminal, closing the classic
  TIOCSTI escape (a sandboxed process injecting keystrokes into your shell).
- **Symlinked deny paths are resolved** to their real targets before masking
  (bwrap can't mount over the link itself, and masking the link wouldn't
  cover the target). Resolved duplicates are masked once.
- **Child read exemptions are bind-backed read-only.** For example,
  `readPaths: ["~/.ssh/config"]` keeps the parent `~/.ssh` mask and restores
  only `config`; sibling keys remain hidden.
- **Missing deny paths are reserved before they are masked.** bwrap cannot
  create a mount destination below the read-only host root, so Polly first gives
  the nearest existing parent a private, snapshotted directory view with the
  denied entry omitted, then creates the empty-tmpfs or `/dev/null` mask inside
  that private view. A host process cannot make a later creation, replacement,
  or symlink retarget appear inside the already-running sandbox. Existing denied
  leaves receive the same masks directly. Unexpected resolution or snapshot
  errors fail the command closed. When the reserved parent is itself
  writable, existing allowed children remain host-backed, but new direct
  siblings are created in the private snapshot rather than on the host.
- A `writablePaths` entry that doesn't exist is dropped when the sandbox is
  constructed, rather than bound — bwrap aborts on a missing bind source, and
  failing construction would brick session restore over one stale path. A path
  created later does not acquire authority on a subsequent command.
- `denyDNS` (with `allowNetwork`) is implemented by masking
  `/etc/resolv.conf` with `/dev/null`. **This is weaker than the macOS
  equivalent and is best-effort only:** bubblewrap has no port-level network
  filtering (the net namespace is all-or-nothing, and seccomp-BPF can't inspect
  the destination port behind the `connect()` sockaddr pointer), so a process
  that hardcodes a resolver (`dig @8.8.8.8`) still resolves names. It stops the
  libc default resolver, not a determined one. True DNS egress control on Linux
  would need a userspace network proxy (netns + slirp/pasta), which is out of
  scope. On macOS the port-53 block below is enforced by the kernel policy.

### macOS: Seatbelt (`sandbox-exec`)

Each command runs under `sandbox-exec` with a generated profile. A fixed,
root-owned `/usr/bin/perl` bootstrap reads the filtered target environment from
anonymous, prefilled pipe descriptors, clears its own environment, closes the
descriptors, and directly `exec`s the original command. Large environments are
split across nonblocking pipe shards whose writers are closed before wrapping
returns, so no named filesystem object or background writer exists. Target
values therefore never appear in the pre-containment wrapper environment or
any intermediate process argv. Framed target environments larger than 1 MiB
are rejected before pipe allocation. The default config renders:

```scheme
(version 1)
(allow default)                          ; allow-by-default policy
(deny file-write*)                       ; ...except writes are denied
(allow file-write* (literal "/dev/null"));  char devices re-allowed
(allow file-write* (literal "/dev/zero")); (+ random, urandom, stdout, stderr)
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/var/folders/.../T"))  ; your real $TMPDIR
(deny file-read* (subpath "/Users/you/.ssh"))
; ... one deny rule per denied path, 17 built-in ...
(deny signal)                            ; can't signal unrelated processes...
(allow signal (target self))             ; ...but a script can manage its own
(allow signal (target same-sandbox))     ;    descendants (anything in this sandbox)
(deny network*)
```

The command also runs with `setsid()` (via `SysProcAttr.Setsid`), giving it its
own session and detached terminal — see the terminal note below.

Properties worth knowing:

- **Allow-by-default.** Unlike the Linux side, Seatbelt here is a targeted
  deny: writes, credential reads, network, and cross-process signaling are
  blocked; the rest (spawning processes, *enumerating* other processes, mach
  services) is permitted. Note this mostly affects process and IPC operations,
  **not** file reads — Linux's read-only root bind also exposes the whole
  filesystem readable, and both platforms deny the same credential list, so the
  read surface matches.
- **Signaling is denied for unrelated processes.** `(deny signal)` plus a
  self/`same-sandbox` re-allow lets a script manage its own descendants
  (timeouts, background workers, even children it detaches into their own
  session) but blocks it from `kill`-ing or `SIGSTOP`-ing your other processes.
  `same-sandbox` scopes by sandbox membership, so it covers descendants
  regardless of process group and doesn't depend on the `setsid()` below. It's
  the macOS approximation of the isolation Linux gets for free from the PID
  namespace (where other processes are simply invisible).
- **Own session, no controlling terminal.** `setsid()` is the macOS counterpart
  to bwrap's `--new-session`: it detaches the controlling tty, closing
  terminal-injection vectors.
- **Denied reads fail loudly**: `cat ~/.ssh/config` returns
  `Operation not permitted`, where Linux would return empty.
- **Denied paths are also write-blocked.** Each credential path gets a
  `deny file-write*` rule after the `writablePaths` allows, so a broad
  `writablePaths` (e.g. `["~"]`) can't re-open write access to `~/.ssh` or
  `~/.aws` — a sandboxed process can neither read nor plant credentials there.
  On Linux this is structural instead: the tmpfs/`/dev/null` mask sits over the
  writable bind, so writes land on the ephemeral overlay, not the real file.
  `readPaths` re-allows reads for exempted paths but never writes.
- **Rules are emitted for both the literal path and its symlink-resolved
  target.** Seatbelt matches resolved vnode paths, so a rule on a
  dotfiles-managed symlink (`~/.npmrc -> ~/dotfiles/npmrc`) would never fire
  for the real file; naming both closes the hole.
- **Last-match-wins** is how `readPaths` exemptions work: they're emitted as
  `allow file-read*` rules after the denies.
- Deny rules are emitted whether or not the path exists (harmless, unlike
  bwrap), so no existence filtering is needed.
- A missing `writablePaths` entry is dropped when the sandbox is constructed,
  so it is tolerated without leaving an inert grant that could activate after
  a later creation.
- Automatic writable roots, including the construction-time `TMPDIR`, are also
  frozen; changing the parent process environment before a later command does
  not add a new write grant.
- `allowNetwork` enables TCP/UDP but still denies outbound Unix-domain sockets,
  preventing access to host Docker, VM, agent, and service endpoints. The one
  exception is macOS's fixed `mDNSResponder` socket, re-allowed only when DNS
  is enabled; `denyDNS` removes that exception and also blocks direct port 53.
- `denyDNS` blocks the system resolver socket (`mDNSResponder`) plus direct
  port-53 UDP/TCP.
- `sandbox-exec` is deprecated by Apple but remains functional and is what
  Bazel, Nix, and Chromium-family tooling ride on; there is no supported
  replacement API for ad-hoc profiles.
- The Darwin backend executes only fixed `/usr/bin/sandbox-exec` and
  `/usr/bin/perl` routes. Both must be regular, root-owned executables that
  non-root users cannot modify. Construction fails closed if either trusted
  component is unavailable.

### Differences at a glance

| | Linux (bwrap) | macOS (Seatbelt) | Unified? |
|---|---|---|---|
| File reads / credentials | whole fs readable, 17-path deny list | whole fs readable, same deny list | ✅ effect matches |
| Writes | read-only root + temp binds | `deny file-write*` + temp allows | ✅ effect matches |
| Network | denied (`--unshare-net`) | denied (`deny network*`) | ✅ effect matches |
| Env handling | shared filtering; pinned in-namespace bootstrap reads a sealed anonymous env FD and directly `exec`s the target | shared filtering; fixed in-profile bootstrap reads anonymous pipe shards and directly `exec`s the target | ✅ effect matches |
| Cross-process env read | blocked by PID namespace | blocked by the OS (`KERN_PROCARGS2` truncates) | ✅ effect matches |
| Signal other processes | invisible, can't signal | `deny signal` + self/same-sandbox re-allow | ✅ effect matches |
| Controlling terminal | detached (`--new-session`) | detached (`setsid()`) | ✅ effect matches |
| Missing `writablePaths` | dropped at construction | dropped at construction | ✅ tolerated, cannot activate later |
| Granted Unix socket (`allowUnixSockets`) | seccomp admits `AF_UNIX` stream + bind into private root; sockets outside private roots also become reachable | profile authorizes the exact socket path only | ⚠️ effect matches for the grant; Linux is broader (see below) |
| **Denied-read failure mode** | reads as empty | `Operation not permitted` | ❌ inherent (masking vs policy) |
| **Process enumeration** | invisible (PID namespace) | visible (`KERN_PROC` sysctl, ungated) | ❌ inherent |
| **Host IPC** | private IPC namespace; host Unix sockets blocked | host Unix sockets blocked; Mach services allowed (allow-default) | ❌ platform gap |
| Mechanism / backend | namespaces, maintained | kernel policy, `sandbox-exec` deprecated | ❌ inherent |

The bold rows are the only genuinely operator-visible gaps that remain, and each
is inherent to the platform rather than a config choice:

- **Denied-read failure mode** is the one that can actually bite portability: a
  tool doing `[ -f ~/.aws/credentials ]` sees *absent* on Linux but
  *present-but-unreadable* on macOS. Unifying it would mean changing one
  platform's masking mechanism.
- **Process enumeration** isn't gateable on macOS — `KERN_PROC` sysctl isn't
  covered by `process-info*`. A sandboxed process can *list* your processes
  (but no longer signal or read the env of them).
- **Host IPC** Unix sockets are blocked on both platforms, including when
  TCP/UDP is enabled. Mach services remain visible on macOS. Flipping Seatbelt
  to `(deny default)` would break most tools because every syscall, file read,
  and Mach service would need an allowlist, so it remains allow-default.

## Configuration

### CLI presets

`--sandbox <spec>` (`POLLYTOOL_SANDBOX`) picks the base policy every sandboxed
tool starts from. A spec is one or more preset names joined with `+`:

| Preset | Policy |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | `denyWrite` — nothing writable, not even temp |
| `workspace` | working directory writable; discovered Git routing entries and metadata trees pinned read-only |
| `git` | with `workspace`: pin only the dangerous Git metadata leaves instead of whole trees, so commit/rebase/fetch work; requires `workspace` |
| `net` | `allowNetwork` |
| `ssh` | agent-based SSH: pass `SSH_AUTH_SOCK`, allow connecting to that socket, read `~/.ssh/config` and `~/.ssh/known_hosts`; private keys stay masked |
| `sshkeys` | read all of `~/.ssh`, private keys included (agentless setups); `~/.ssh` writes stay denied |

The default is **`workspace+net+git`** — agentic work on the current project
with network access and working Git, while credentials stay masked and the
rest of the filesystem stays read-only. `workspace` canonicalizes the working
directory *at startup*, then follows `.git` and `commondir` pointer files to
pin the resolved Git metadata (whole trees on its own; only the dangerous
leaves when combined with `git` — see the workspace/Git section). Because that
protection cannot safely use a partial recursive scan, `workspace` refuses the
filesystem root, the user's home directory, and exact mounted-volume roots on
Linux and macOS. Linux also refuses its exact private temp/runtime sandbox
roots. Descendants of those mounts remain valid bounded workspaces; otherwise
change into a project directory or select `--sandbox base`.

`git` selects *how* the workspace protects Git metadata and does nothing on its
own — `--sandbox git` is rejected with a pointer to `workspace+git`.

Per-tool knobs (schema `"sandbox"` object, MCP server entry) merge on top of
the base policy. Entries are monotonic: overlays may add grants or restrictions
but cannot remove an earlier entry:

| Field | Default | Effect |
|---|---|---|
| `writablePaths` | `[]` (temp only) | extra write-allowed directories (`~` ok) |
| `allowNetwork` | `false` | permit outbound network |
| `denyDNS` | `false` | with `allowNetwork`: block DNS on macOS; suppress the default resolver on Linux (best effort) |
| `readPaths` | `[]` | exempt entries from the deny list |
| `denyPaths` | `[]` | extra read-blocked paths (`~` ok) |
| `denyWritePaths` | `[]` | read-only islands inside writable trees (`~` ok) |
| `allowEnv` | `[]` | strict env allowlist — replaces the heuristic stripping |
| `passEnv` | `[]` | additive exemption from heuristic stripping (ignored under `allowEnv`) |
| `allowUnixSockets` | `[]` | absolute Unix-socket paths the process may connect to (`~` ok) |
| `denyWrite` | `false` | no writes anywhere, not even temp |

Global flags: `--nosandbox` (`POLLYTOOL_NOSANDBOX`) disables everything;
`--denypath` (`POLLYTOOL_DENYPATHS`, repeatable) adds read-blocked paths,
`--writepath` (`POLLYTOOL_WRITEPATHS`, repeatable) adds writable paths, and
`--allownet` (`POLLYTOOL_ALLOWNET`) enables network for all sandboxed tools.
For a conversation run, when no-sandbox mode is effective, Polly rejects an
explicitly supplied `--sandbox`, `--denypath`, `--writepath`, or `--allownet`
instead of silently ignoring that policy. Pass `--nosandbox=false` to override
an ambient `POLLYTOOL_NOSANDBOX=true` and restore sandboxing.

After global and per-tool policies are merged, Polly visibly warns once for
each home directory or filesystem root that remains a broad writable grant. The
warning points to the global and per-tool settings to inspect; read-only policies
and matching `denyWritePaths` do not trigger it.

Three interactions to keep in mind:

- `denyDNS` only matters when `allowNetwork` is true (no network ⊃ no DNS).
  macOS blocks the resolver service and direct port 53; Linux masks the default
  resolver configuration, but a process can still contact a hard-coded resolver.
- `allowEnv` is a mode switch, not an addition: when set, *only* those
  variables pass through. Use it to hand a tool one specific token —
  `"allowEnv": ["GITHUB_TOKEN"]` — that the heuristics would otherwise strip.
  `passEnv` is the additive counterpart: it exempts named variables from the
  credential-shaped stripping while everything else still flows through, and
  it is ignored when `allowEnv` is set so a strict allowlist stays strict.
  Prefer `passEnv` when you only want to add one variable (e.g. `passEnv:
  ["SSH_AUTH_SOCK"]`) rather than re-enumerating `HOME`/`PATH` under `allowEnv`.
- `allowUnixSockets` grants outbound access to specific Unix-domain sockets by
  path, even while broad Unix-socket access stays blocked. A grant is dropped
  for any command where its path is no longer a live socket (a dead agent
  degrades to "cannot reach the agent", not a broken sandbox) and never lifts
  a credential deny that covers it. On macOS the profile authorizes exactly
  that socket path; on Linux the socket is bound into the private mount view
  when it lives under a private root, and a seccomp relaxation admits
  `socket(AF_UNIX, SOCK_STREAM)` — which, because `connect()` cannot be
  path-filtered, also makes other host sockets *outside* the private roots
  reachable while any grant is active (see Limitations).
- `denyWritePaths` blocks writes but leaves reads alone (unlike `denyPaths`,
  which blocks both). Mutable ancestors inside a writable tree are pinned so a
  process cannot rename an ancestor and rebuild a replacement at the guarded
  pathname. A missing or unresolvable entry fails sandbox construction (and is
  checked again before every command) because neither backend can reliably
  reserve a nonexistent protected object. The `workspace` preset (without
  `git`) avoids missing Git leaves by protecting whole metadata directories,
  including a not-yet-created `config.worktree`; the `git` component instead
  creates the inert leaves it pins (see the workspace/Git section).

The `workspace` preset recursively discovers regular and nested repositories,
submodules, and linked worktrees. Without the `git` component it makes each
`.git` routing entry, resolved per-worktree gitdir, `commondir` pointer, and
common gitdir read-only. This closes replacement and alternate-config paths at
the cost of blocking all Git metadata-tree writes inside the sandbox
(working-tree content remains writable), so `git commit` and friends fail with
`EPERM` — the LLM-facing bash description says so, and `--sandbox
workspace+net+git` (the default) is the way to get working Git.

The `git` component switches this to **leaf mode**: instead of the whole `.git`
tree, it pins only the metadata that can select host-executed code or reroute
the repository — `config`, `config.worktree`, `hooks/`, the `.git` routing
file, and the `commondir`/`gitdir` pointers — per repository, submodule gitdir,
and linked worktree. `.git` itself stays writable, so `index`, `objects/`,
`refs/`, `logs/`, and `COMMIT_EDITMSG` are writable and commit/rebase/fetch
work; the pinned leaves keep an injected command from planting a hook or
rewriting `core.hooksPath`/`core.fsmonitor`. Because `denyWritePaths` entries
must exist on disk, leaf mode **creates the inert leaves it pins when they are
absent**: an empty `.git/config` and `.git/hooks/` (exactly what `git init`
leaves), and an empty `.git/config.worktree` only when
`extensions.worktreeConfig` is effectively enabled (git ignores that file
otherwise). If a leaf cannot be created — a read-only `.git`, for instance —
that one repository falls back to the whole-tree pin and logs
`sandbox_git_leaf_fallback`; the sandbox still starts and is never weaker than
whole-tree mode. Metadata the workspace walk never enters — dormant (registered
but not checked-out) submodule gitdirs under `modules/`, stale or external
`worktrees/<id>` entries, and a common gitdir outside the workspace — stays
whole-tree pinned, and bare repositories inside the workspace are always
whole-tree pinned. Ancestor pinning makes the pinned leaves freeze `.git`
itself against rename or recreation on both platforms.

Leaf mode's honest residuals: the `modules/` and `worktrees/` container
directories stay writable so `git worktree add` and `git submodule update`
work, so a sandboxed command can create *new* metadata subtrees there (as it
already can create a fake `anydir/.git/` in the writable worktree) — a host
user should not run Git inside a directory a sandboxed tool created without
inspecting it. Data-level paths (`objects/`, `refs/`, `packed-refs`, `logs/`,
`HEAD`, `index`, `info/attributes`, `objects/info/alternates`) are unpinned:
tampering there corrupts history or content but cannot execute host code, and
the equivalent is already reachable through writable worktree files such as
`.gitattributes`. The reverse `worktrees/<id>/gitdir` pointer *is* pinned
because a legitimate `git worktree repair` writes through it; a retargeted
pointer does not yield an arbitrary-file clobber in current Git (repair
validates the pointer and refuses), so the pin is defense-in-depth against
routing corruption rather than a known write primitive. Config-writing commands
(`git config`, `git remote add`, `git submodule init/deinit`, `git maintenance
start`) are blocked by the `config` pin **by design**. And an existing
effective `core.hooksPath` or config include that targets an unpinned location
*inside* `.git` fails closed under leaf mode where whole-tree mode accepted it
(the target is no longer covered) — drop the `git` component to fall back to
`workspace+net`.

The rest of this section describes the discovery and audit that both modes
share.
Bare-repository working directories are rejected, as are symlinked `.git`,
config, `config.worktree`, hooks directories, and hook files: neither backend
can portably pin those links if a merged policy later makes their targets
writable. Protected routing/config/hook files with hard-link aliases are also
rejected. Repository-local `core.hooksPath` and config includes fail closed
because the effective redirected target cannot be pinned without evaluating
Git's full configuration environment. Polly accepts PATH-selected `git` when
it reaches fixed `/usr/bin/git` through a stable non-symlink route outside
writable paths. On Darwin it also accepts the standard `/opt/homebrew/bin/git`
and `/usr/local/bin/git` leaf symlinks when they resolve directly into the
matching `Cellar/git/<version>/bin/git`; the resolved target must be
non-writable, single-linked, and outside every sandbox-writable path. Polly
executes the resolved selected target with repository-routing variables
removed, so compiled system-config paths and prefix expansion match the user's
next host Git invocation. Relevant global/system config selectors and
ordinary-command overrides remain present; the `git config`-only `GIT_CONFIG`
override is removed. It
checks effective and overridden global/system `core.hooksPath` values and
recursively inspects nested includes regardless of current `includeIf`
conditions. Hook targets, selected config sources, and include targets in
host-visible writable content outside protected Git metadata are rejected,
including absent targets and the host temp trees writable under Seatbelt on
macOS. Existing config sources with hard-link aliases are rejected as well.
Configured hook directories are scanned, and symlinked or hard-linked hook
entries are rejected before containment. `/dev/null` is accepted as Git's
immutable hook-disabling target. Repository discovery runs once when the
workspace preset is parsed; its private policy record survives `Config.Merge`,
and the config/include/hook audit runs again at backend construction against
all final host-visible writable roots. Consequently, CLI `--writepath` and
per-tool `writablePaths` overlays cannot reopen an external hook-planting
route. Linux's exact private `/tmp` and `/run` mounts are excluded from that
host-persistence check, while explicitly rebound descendants and wider roots
remain covered.

Policy paths are normalized when the sandbox is constructed. `~` expands to
the user's home directory, relative paths resolve against the process working
directory at that moment, and empty entries are rejected. Existing
`writablePaths` and `readPaths` grants are frozen to their canonical targets;
their mutable routing entries are pinned for each command, and missing grants
are dropped rather than becoming active after later creation. A `readPaths`
entry that traverses a symlink keeps its approved lexical route usable, but the
route and target are revalidated before each command so replacement or
retargeting fails closed. `PrepareConfig` also carries the approved filesystem
identities privately through
`Config.Merge`. A tool registry prepares its base before any process-backed
tool or schema sandbox starts (`WithSandboxFactory` snapshots it when the
option is created) and reuses those identities for later lazy tool
construction, so replacing or rerouting an approved object fails before the
sandbox factory is called.

### Examples

Shell tool that fetches URLs and adds a cache directory to the writable paths
it inherits from the caller's base policy:

```json
{
  "title": "fetcher",
  "command": "./fetch.sh",
  "sandbox": {
    "allowNetwork": true,
    "writablePaths": ["~/.cache/fetcher"]
  }
}
```

The overlay does not remove base permissions. With CLI `--sandbox base`, the
effective writable set is temp plus this cache; with the CLI default it also
includes the workspace.

Stdio MCP server with one credential explicitly passed through:

```json
{
  "mcpServers": {
    "github": {
      "command": "github-mcp-server",
      "sandbox": {
        "allowNetwork": true,
        "allowEnv": ["GITHUB_TOKEN", "HOME", "PATH"]
      }
    }
  }
}
```

Read-only analysis tool that may read your SSH config but nothing else
sensitive, and can't write at all:

```json
"sandbox": {
  "denyWrite": true,
  "readPaths": ["~/.ssh/config"]
}
```

Block a project's secrets from every sandboxed tool:

```sh
polly --denypath ~/work/secrets --denypath ~/.config/myapp ...
```

## Observing decisions

A masked directory reads as empty and a stripped variable surfaces as a
mystifying auth failure deep inside a tool, so the sandbox logs what it does.
With `--debug`, one `sandbox_config` line when a tool is loaded (the effective
merged config) and one `sandbox_wrap` line per command:

```
... DBG sandbox_config tool=bash network=false deny_dns=false deny_write=false
        writable_paths="[/tmp]" read_paths=[] deny_paths=[] allow_env=[]
        pass_env=[] allow_unix_sockets=[]
... DBG sandbox_wrap command="bash -c" network=false deny_write=false
        writable_paths="[/tmp]" env_stripped="[OPENAI_API_KEY SSH_AUTH_SOCK]"
        denied_paths=17 unix_sockets=0
```

A dropped socket grant (dead or replaced agent) logs a
`sandbox_unix_socket_grant_dropped` line naming the path and reason.

`env_stripped` and all path fields log **names only, never values**. Linux also
strips host-runtime hints such as `DBUS_SESSION_BUS_ADDRESS`, `DOCKER_HOST`, and
`XDG_RUNTIME_DIR`.
`command` is truncated to the first two argv entries.

In the REPL:

- Startup prints a posture line:
  `sandbox: active (base; 3 tools sandboxed; not sandboxed: weather)` — or
  `sandbox: disabled (--nosandbox)`.
- `/get sandbox` shows the live state:
  `active (preset: base; denypaths: 2; tools: 3 sandboxed, 1 not: weather)`.
- `/tools list` marks each sandboxed tool with a policy summary such as
  `[sandboxed: net off, temp writes, env filtered]`, and
  `/tools show <name>` includes a `sandboxed: true/false` line.

The LLM also sees it: sandboxed shell tools get `[sandboxed]` appended to
their description, so the model knows writes and network may be restricted.

## Limitations

Know what this does *not* do:

- **Same user, same kernel.** This is not a VM or a container with its own
  user namespace. The kernel attack surface is fully exposed, and anything
  your user can do that isn't explicitly denied, the sandbox can do.
- **macOS is allow-by-default.** A sandboxed process can still read every
  non-denied file you can, *enumerate* your other processes, and talk to mach
  services — though it can no longer signal them, read their environments, or
  hold your terminal. Treat the macOS sandbox as credential/write/network
  containment with partial process isolation, not the full PID-namespace
  isolation Linux provides.
- **macOS temp is shared.** Linux uses a private tmpfs for `/tmp`; Seatbelt
  cannot provide a mount namespace, so `/private/tmp` remains shared on macOS.
- **The deny list is a list.** Credentials living outside the 17 built-in
  paths (a `.env` in your project, a token in a config the list doesn't know)
  are readable unless you add them with `--denypath` or `denyPaths`.
- **No resource limits.** CPU, memory, disk-in-temp, and process count are
  unconstrained; a sandboxed fork bomb is still a fork bomb.
- **Env heuristics are heuristics.** A secret in a variable named `CREDS`
  passes the suffix patterns. Use `allowEnv` for tools that should see almost
  nothing.
- **A granted agent socket is a signing oracle.** `allowUnixSockets` (and the
  `ssh` preset) let a sandboxed process *use* your SSH agent while it runs — it
  never reads the key, but a prompt-injected command can sign with it. Prefer
  `ssh-add -c` (per-use confirmation) if that matters, and remember the grant
  only exists while an agent is running.
- **Linux socket grants are broader than macOS.** `connect()` cannot be
  path-filtered in seccomp, so once any `allowUnixSockets` grant is active,
  Unix sockets that sit *outside* the private `/tmp`//`run` roots (e.g. a
  Docker socket under `~/.docker/run`) also become connectable. macOS
  authorizes only the exact granted path. Grant sockets deliberately.
- **The `ssh` preset does not grant `known_hosts` writes.** First contact with
  an unknown host fails host-key verification inside the sandbox; connect once
  outside, or manage `known_hosts` on the host. `ControlMaster` sockets and
  other `~/.ssh` writes are likewise denied.
- **`git commit` needs the `git` component.** Under plain `workspace` the whole
  `.git` tree is read-only and commits fail with `EPERM`; the default
  `workspace+net+git` fixes this. GPG-signed commits still fail (no gpg-agent
  socket is granted) — disable signing in the sandbox or sign on the host.
