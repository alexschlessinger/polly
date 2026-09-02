# Sandboxing

Polly runs LLM-driven commands — the builtin `bash` tool, shell tools, and
stdio MCP servers — inside an OS-level sandbox by default. This is the
reference: threat model, every configuration field, the workspace Git
protection, and how the Linux and macOS backends differ. The CLI summary is
in [README.md](README.md#sandboxing); library wiring in
[API.md](API.md#sandboxing-in-the-library).

**Contents:** [Intent](#intent) ·
[What gets sandboxed](#what-gets-sandboxed) ·
[Configuration](#configuration) ·
[Workspace Git protection](#workspace-git-protection) ·
[Platform implementations](#platform-implementations) ·
[Observing decisions](#observing-decisions) · [Limitations](#limitations)

## Intent

The commands polly runs are chosen by a language model, often steered by
untrusted input. The sandbox limits the blast radius of a hallucinated,
prompt-injected, or buggy command. By default a sandboxed process cannot:

- **Read credentials** — `~/.ssh`, `~/.aws`, `~/.gnupg`, and the rest of
  the [deny list](#credential-paths-denied-by-default), plus anything added
  with `--denypath`, unless a `readPaths` grant exempts an entry.
- **See secrets in the environment** — credential-shaped variables
  (`POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, ...) and agent sockets
  (`SSH_AUTH_SOCK`, `GPG_AGENT_INFO`) are stripped unless explicitly passed.
  Agent sockets matter: with `SSH_AUTH_SOCK` intact a process can use your
  SSH keys without ever reading `~/.ssh`.
- **Write beyond the writable set** — the filesystem is read-only except the
  OS temp dir and configured `writablePaths`. The CLI default adds the
  workspace with its Git metadata carved back out; the library
  `DefaultConfig` does not.
- **Reach the network unless allowed** — the library denies it; the CLI
  default `workspace+net+git` allows TCP/UDP. Host Unix sockets stay
  blocked either way, except sockets an `allowUnixSockets` grant names.

The sandbox is **default-on** (tool metadata cannot opt out; only the
caller's `--nosandbox` / `WithUnsafeNoSandbox` can), **fails closed** (no
backend, or a sandbox that fails to construct, is an error, never a silent
unsandboxed run), **tracks the live filesystem** (deny lists are rebuilt
every command, so a credential dir created mid-session is masked by the
next one), and is **observable** under `--debug`. It is containment for
accidents and injection, not a hostile-binary jail: the process runs as your
user on a shared kernel. See [Limitations](#limitations).

## What gets sandboxed

| Execution path | Sandboxed | Opt-out |
|---|---|---|
| Builtin `bash` tool | yes | `--nosandbox` |
| Shell tools (`-t ./tool.sh`) | yes | global `--nosandbox` only |
| Stdio MCP servers | yes (the whole server process) | global `--nosandbox` only |
| Remote MCP servers (HTTP/SSE) | no — the process runs elsewhere | n/a |
| Skill helper / function tools | no — in-process, nothing to wrap | n/a |
| Builtin file tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `search_files`, `view_image`) | policy-checked in-process | `--nosandbox` |

The builtin file tools have no child process to wrap, so they check every
path against the base config instead: reads against the deny list minus
`readPaths`, writes against the writable paths minus `denyWritePaths` and
the deny list. `write_file` and `edit_file` refuse to load when sandboxing
is unavailable unless the registry opts out. Shell-tool `--schema`
discovery is stricter than execution: private-temp writes only, no
network, workspace, read-path, or environment grants.

## Configuration

### CLI presets

`--sandbox <spec>` (`POLLYTOOL_SANDBOX`) picks the base policy. A spec is
one or more preset names joined with `+`:

| Preset | Policy |
|---|---|
| `base` | temp-dir writes only, no network |
| `readonly` | `denyWrite` — nothing writable, not even temp |
| `workspace` | working directory writable; discovered Git routing entries and metadata trees pinned read-only |
| `git` | with `workspace`: pin only the dangerous Git metadata leaves instead of whole trees, so commit/rebase/fetch work |
| `net` | `allowNetwork` |
| `ssh` | agent-based SSH: pass `SSH_AUTH_SOCK`, allow that socket, read `~/.ssh/config` and `~/.ssh/known_hosts`; private keys stay masked |
| `sshkeys` | read all of `~/.ssh`, private keys included; `~/.ssh` writes stay denied |

The default is **`workspace+net+git`**. `git` does nothing on its own;
`--sandbox git` is rejected with a pointer to `workspace+git`. `workspace`
canonicalizes the working directory at startup and refuses roots it cannot
safely protect (the filesystem root, your home directory, mounted-volume
roots); change into a project directory or select `--sandbox base`.

### Global flags

| Flag | Env | Effect |
|---|---|---|
| `--sandbox <spec>` | `POLLYTOOL_SANDBOX` | select the base preset |
| `--writepath <dir>` (repeatable) | `POLLYTOOL_WRITEPATHS` | extra writable paths for all sandboxed tools |
| `--denypath <path>` (repeatable) | `POLLYTOOL_DENYPATHS` | extra read-blocked paths for all sandboxed tools |
| `--allownet` | `POLLYTOOL_ALLOWNET` | allow outbound network for all sandboxed tools |
| `--nosandbox` | `POLLYTOOL_NOSANDBOX` | disable sandboxing entirely |

When no-sandbox mode is effective, an explicitly supplied `--sandbox`,
`--denypath`, `--writepath`, or `--allownet` is rejected rather than
silently ignored; `--nosandbox=false` overrides an ambient
`POLLYTOOL_NOSANDBOX=true`. Polly warns once for each home directory or
filesystem root left broadly writable after policies merge.

### The per-tool `"sandbox"` object

A shell tool schema or MCP server entry may carry a `"sandbox"` field:
`true` means the base policy, an object customizes it, and `false` is
refused unless the caller chose `--nosandbox` / `WithUnsafeNoSandbox`.

| Field | Type | Effect |
|---|---|---|
| `allowNetwork` | bool | allow outbound network access |
| `denyDNS` | bool | with `allowNetwork`: block DNS on macOS; suppress the default resolver on Linux (best effort) |
| `writablePaths` | string[] | directories where writes are allowed |
| `readPaths` | string[] | paths exempted from the read deny list |
| `denyPaths` | string[] | extra read-blocked paths |
| `denyWritePaths` | string[] | paths kept read-only even inside a `writablePaths` entry |
| `allowEnv` | string[] | strict allowlist: if set, *only* these env vars pass through |
| `passEnv` | string[] | additive exemptions from sensitive-var stripping (ignored when `allowEnv` is set) |
| `allowUnixSockets` | string[] | absolute Unix-socket paths the process may connect to |
| `denyWrite` | bool | deny all writes, even temp; overrides `writablePaths` |

Path fields support `~`. The **base policy** (`sandbox.DefaultConfig()`,
preset `base`) denies writes everywhere except the sandbox temp dir, denies
network, hides the credential deny list, and strips sensitive env vars. On
Linux it also gives the process a private `/tmp` and `/run`, its own PID
and IPC namespaces, dropped capabilities, and no filesystem Unix sockets.

### How policies merge

The effective config is the base (a preset, or `DefaultConfig`) merged with
the global overlays and the tool's own `"sandbox"` object. Merging is
monotonic: booleans OR and path lists append, so an overlay can add grants
or restrictions but never remove one. Details:

- `denyWrite: true` overrides `writablePaths`. `denyDNS` only matters with
  `allowNetwork`.
- `allowEnv` is a mode switch: when set, only the listed variables flow and
  `passEnv` is ignored. Prefer `passEnv` to add one variable.
- `~` expands to your home and relative entries resolve against polly's
  working directory. Existing `writablePaths` and `readPaths` grants are
  canonicalized and **frozen to their filesystem identities** when
  `WithSandboxFactory` is created; missing grants are dropped rather than
  activating on a later creation, and a grant later replaced or rerouted
  fails closed.
- A `readPaths` entry may name a child of a denied directory
  (`~/.ssh/config`): only that child is restored, and the route is
  revalidated before each command.
- `denyWritePaths` entries carve read-only islands out of writable trees
  and must exist on disk; `denyPaths` blocks reads *and* writes. Deny paths
  are rechecked every command with symlinks resolved. Writable ancestors of
  a protected entry are pinned against relocation.
- An `allowUnixSockets` entry that isn't a live socket at command time is
  dropped rather than failing the command, and never lifts a credential
  deny that covers it.

### Environment filtering

Sensitive variables are always stripped, matched case-insensitively:

- the prefixes `POLLYTOOL_*` and `AWS_*`;
- names ending in `_API_KEY`, `_APIKEY`, `_TOKEN`, `_SECRET`, `_SECRET_KEY`,
  `_ACCESS_KEY`, `_PASSWORD`, `_PASSPHRASE`, `_CREDENTIALS`, `_PRIVATE_KEY`,
  and those names bare (`TOKEN`, `PASSWORD`, `API_KEY`, ...);
- database credentials: `PGPASSWORD`, `PGPASSFILE`, `MYSQL_PWD`,
  `REDISCLI_AUTH`, `DATABASE_URL`;
- agent sockets and host runtime handles: `SSH_AUTH_SOCK`, `SSH_AGENT_PID`,
  `GPG_AGENT_INFO`, `DBUS_SESSION_BUS_ADDRESS`, `DBUS_SYSTEM_BUS_ADDRESS`,
  `DOCKER_HOST`, `CONTAINER_HOST`, `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`,
  `PULSE_SERVER`.

Pass one through with `passEnv` (additive) or `allowEnv` (strict). The
heuristics are heuristics: a secret in a variable named `CREDS` passes, so
use `allowEnv` for tools that should see almost nothing.

### Credential paths denied by default

`~/.ssh`, `~/.gnupg`, `~/.gpg`, `~/.aws`, `~/.azure`, `~/.config/gcloud`,
`~/.kube`, `~/.docker/config.json`, `~/.npmrc`, `~/.pypirc`,
`~/.gem/credentials`, `~/.cargo/credentials`, `~/.config/gh`, `~/.netrc`,
`~/.git-credentials`, `~/.local/share/keyrings`, `~/Library/Keychains`

Credentials outside this list — a `.env` in your project, a token in some
config file — are readable unless you add them with `--denypath` or
`denyPaths`.

### Examples

In a shell tool's `--schema` output, or an MCP server entry:

```jsonc
// Base policy
"sandbox": true

// A fetcher: network plus a cache dir (added to whatever base is selected)
"sandbox": { "allowNetwork": true, "writablePaths": ["~/.cache/fetcher"] }

// A deploy tool: AWS credentials, a strict env allowlist, little else
"sandbox": {
  "allowNetwork": true,
  "writablePaths": ["/tmp/deploy"],
  "readPaths": ["~/.aws"],
  "allowEnv": ["AWS_PROFILE", "AWS_REGION", "HOME", "PATH"]
}

// One variable and one agent socket, everything else still filtered
"sandbox": { "passEnv": ["SSH_AUTH_SOCK"], "allowUnixSockets": ["~/.ssh/agent.sock"] }

// Read-only analysis that may read your SSH config
"sandbox": { "denyWrite": true, "readPaths": ["~/.ssh/config"] }
```

## Workspace Git protection

Git metadata is an execution vector: a command that can write `.git/hooks/`
or `.git/config` (`core.hooksPath`, `core.fsmonitor`) runs code on the
*host* the next time you type `git status`. So the `workspace` preset
carves Git metadata back out of the writable tree. Discovery is recursive —
nested repositories, submodules, and linked worktrees are found by
following `.git` routing files and `commondir` pointers — and runs once,
when the preset is parsed.

**Whole-tree mode** (`workspace` without `git`) pins every discovered
`.git` routing entry, gitdir, and common gitdir read-only. Working-tree
files stay editable, but `git commit`, rebase, and fetch fail with `EPERM`;
the LLM-facing bash description says `.git is read-only` so the model
doesn't mistake that for a transient error.

**Leaf mode** (`workspace+git`, the default) keeps `.git` writable and pins
only the metadata that can select host-executed code or reroute the
repository: `config` and `config.worktree`, `hooks/`, and the `.git`
routing file plus `commondir`/`gitdir` pointers — per repository, submodule,
and linked worktree. `index`, `objects/`, `refs/`, and `logs/` stay
writable, so commit/rebase/fetch work while hook-planting and
`core.hooksPath` rewrites stay blocked. Because pinned entries must exist,
leaf mode creates the inert leaves it pins when absent (an empty
`.git/config` and `.git/hooks/`, exactly what `git init` leaves behind); if
it can't, that repository falls back to the whole-tree pin. Metadata the
walk never enters — dormant submodule gitdirs, stale `worktrees/<id>`
entries, bare repositories — stays whole-tree pinned.

Leaf mode's residuals: `modules/` and `worktrees/` stay writable so
`git worktree add` and `git submodule update` work, which means a sandboxed
command can create *new* metadata subtrees there — don't run Git inside a
directory a sandboxed tool created without inspecting it. Data paths
(`objects/`, `refs/`, `HEAD`, `index`) are unpinned; tampering there
corrupts content but cannot execute host code. Config-writing commands
(`git config`, `git remote add`) are blocked by the `config` pin, by design.

**Refused layouts.** Some shapes cannot be pinned portably, so the preset
refuses them up front: bare-repository working directories; symlinked Git
metadata (`.git`, config, hooks directory, or a hook file); hard-linked
routing, config, or hook files; and repository-local `core.hooksPath` or
config includes, whose effective target cannot be pinned without evaluating
Git's full configuration (`/dev/null` remains a supported `core.hooksPath`).

**The trusted Git and the config audit.** To evaluate hooks and config,
polly only runs a Git it trusts — the fixed `/usr/bin/git`, or on macOS the
standard Homebrew symlinks when they resolve to a non-writable
`Cellar/git/<version>/bin/git` — with repository-routing variables removed
so its answers match your next host invocation. The audit checks effective
and overridden global/system `core.hooksPath` values and recursively
inspects config includes (even inactive `includeIf` branches), rejecting
hook, config, and include targets that land in host-visible writable
content outside protected Git metadata, config sources with hard-link
aliases, and symlinked or hard-linked entries in hook directories. It
**runs again when each sandbox is constructed**, against the final merged
writable roots, so a later `--writepath` or per-tool `writablePaths` cannot
quietly make an external config or hook target plantable.

## Platform implementations

Both backends share the policy surface (the `Config` fields) and the
environment filtering, done in Go. The pre-containment wrapper receives an
empty environment; the filtered target environment is installed only after
containment is active. The enforcement mechanisms differ.

### Linux: bubblewrap (`bwrap`)

Each command runs under [bubblewrap](https://github.com/containers/bubblewrap)
in a fresh mount + PID namespace. The default config renders roughly:

```
bwrap \
  --ro-bind / /                          # entire filesystem read-only
  --tmpfs /tmp                           # private writable temp
  --tmpfs /run --remount-ro /run         # hide host runtime sockets
  --tmpfs /home/you/.ssh                 # denied dirs masked with empty tmpfs
  --ro-bind /dev/null /home/you/.netrc   # denied files masked with /dev/null
  ...                                    # one mask per denied path
  --dev /dev --proc /proc
  --unshare-pid --unshare-ipc
  --unshare-net                          # omitted when allowNetwork
  --seccomp FD                           # deny AF_UNIX sockets + io_uring setup
  --cap-drop ALL
  --die-with-parent --new-session
  -- /proc/self/fd/BOOTSTRAP_FD ...      # pinned post-containment bootstrap
```

- **Fixed launcher.** Only the root-owned, non-user-writable
  `/usr/bin/bwrap` is executed; construction fails closed if it's
  unavailable or mutable.
- **Environment after containment.** bwrap gets an empty environment. A
  pinned bootstrap reads a sealed anonymous env descriptor after
  namespaces, mounts, and seccomp are active, then `exec`s the target with
  its exact argv. Target values never appear in bwrap's environment or argv.
- **Writes are physically impossible** outside private temp and writable
  binds: the root is a read-only mount, not a policy check, and
  capabilities are dropped so a root launcher cannot remount.
- **Host runtime state is private.** `/tmp` and `/run` are fresh mounts, so
  D-Bus, Docker, SSH-agent, and Wayland sockets are absent, and seccomp
  denies `socket(AF_UNIX)` for sockets elsewhere.
- **Denied paths read as empty**, not as errors. Missing deny paths are
  reserved first (the nearest existing parent gets a private snapshot view
  with the entry omitted) so a later host-side creation cannot appear
  inside the running sandbox. Child read exemptions such as
  `~/.ssh/config` are bind-backed; sibling keys stay hidden.
- **`--unshare-pid`** hides other processes' `/proc/<pid>/environ`,
  including polly's own API keys; **`--new-session`** detaches the
  controlling terminal, closing the TIOCSTI keystroke-injection escape.
- `denyDNS` masks `/etc/resolv.conf` and is best-effort only: bubblewrap
  has no port-level filtering, so a hardcoded resolver (`dig @8.8.8.8`)
  still works.

### macOS: Seatbelt (`sandbox-exec`)

Each command runs under `sandbox-exec` with a generated profile, launched
through a fixed root-owned `/usr/bin/perl` bootstrap that reads the
filtered environment from anonymous pipes (over 1 MiB is rejected) and
`exec`s the command, so target values never appear in any intermediate
process. The default config renders:

```scheme
(version 1)
(allow default)                          ; allow-by-default policy
(deny file-write*)                       ; ...except writes are denied
(allow file-write* (literal "/dev/null")); char devices re-allowed (+ zero, random, stdout, stderr)
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/var/folders/.../T"))  ; your real $TMPDIR
(deny file-read* (subpath "/Users/you/.ssh"))
; ... one deny rule per denied path, 17 built-in ...
(deny signal)                            ; can't signal unrelated processes...
(allow signal (target self))             ; ...but a script can manage its own
(allow signal (target same-sandbox))     ;    descendants
(deny network*)
```

The command also runs under `setsid()`: its own session, no controlling
terminal.

- **Allow-by-default.** Writes, credential reads, network, and
  cross-process signaling are denied; spawning processes, enumerating other
  processes, and Mach services are permitted. The file *read* surface
  matches Linux, which also exposes the whole filesystem read-only.
- **Denied reads fail loudly**: `cat ~/.ssh/config` returns
  `Operation not permitted`, where Linux returns empty.
- **Denied paths are also write-blocked**, so a broad `writablePaths` such
  as `["~"]` cannot re-open write access to `~/.ssh`. `readPaths` re-allows
  reads, never writes. Rules cover both the literal path and its
  symlink-resolved target, since Seatbelt matches resolved vnode paths.
- **Writable roots are frozen at construction**, including the
  construction-time `TMPDIR`; a missing `writablePaths` entry is dropped.
- `allowNetwork` enables TCP/UDP but still denies outbound Unix-domain
  sockets, except macOS's fixed `mDNSResponder` socket when DNS is enabled;
  `denyDNS` removes that exception and blocks port 53.
- `sandbox-exec` is deprecated by Apple but remains functional (Bazel, Nix,
  and Chromium ride on it); there is no replacement API for ad-hoc
  profiles. Only the fixed root-owned `/usr/bin/sandbox-exec` and
  `/usr/bin/perl` are executed; construction fails closed otherwise.

### Differences at a glance

| | Linux (bwrap) | macOS (Seatbelt) | Unified? |
|---|---|---|---|
| File reads / credentials | whole fs readable, 17-path deny list | same | ✅ |
| Writes | read-only root + temp binds | `deny file-write*` + temp allows | ✅ |
| Network | `--unshare-net` | `deny network*` | ✅ |
| Env handling | sealed env FD read by in-namespace bootstrap | anonymous pipes read by in-profile bootstrap | ✅ |
| Cross-process env read | blocked by PID namespace | blocked by the OS (`KERN_PROCARGS2` truncates) | ✅ |
| Signal other processes | invisible | `deny signal` + self/same-sandbox re-allow | ✅ |
| Controlling terminal | detached (`--new-session`) | detached (`setsid()`) | ✅ |
| Granted Unix socket | seccomp admits `AF_UNIX`; sockets outside private roots also become reachable | exact socket path only | ⚠️ Linux is broader |
| **Denied-read failure mode** | reads as empty | `Operation not permitted` | ❌ inherent |
| **Process enumeration** | invisible (PID namespace) | visible (`KERN_PROC` sysctl, ungated) | ❌ inherent |
| **Host IPC** | private IPC namespace; host Unix sockets blocked | host Unix sockets blocked; Mach services allowed | ❌ platform gap |

The bold rows are inherent to the platform. The denied-read failure mode
is the one that bites portability: a tool doing `[ -f ~/.aws/credentials ]`
sees *absent* on Linux but *present-but-unreadable* on macOS. Flipping
Seatbelt to `(deny default)` would break most tools, since every syscall,
file read, and Mach service would need an allowlist.

## Observing decisions

A masked directory reads as empty and a stripped variable surfaces as a
mystifying auth failure deep inside a tool, so the sandbox logs what it
does. With `--debug`: one `sandbox_config` line when a tool is loaded and
one `sandbox_wrap` line per command, **names only, never values**:

```
... DBG sandbox_config tool=bash network=false deny_dns=false deny_write=false
        writable_paths="[/tmp]" read_paths=[] deny_paths=[] allow_env=[]
        pass_env=[] allow_unix_sockets=[]
... DBG sandbox_wrap command="bash -c" network=false deny_write=false
        writable_paths="[/tmp]" env_stripped="[OPENAI_API_KEY SSH_AUTH_SOCK]"
        denied_paths=17 unix_sockets=0
```

In the REPL, startup prints a posture line, `/get sandbox` shows the live
state, and `/tools list` marks each sandboxed tool with a policy summary
such as `[sandboxed: net off, temp writes, env filtered]`. The model sees
`[sandboxed]` appended to each sandboxed tool's description.

## Limitations

- **Same user, same kernel.** Not a VM or a user-namespaced container.
  Anything your user can do that isn't explicitly denied, the sandbox can
  do.
- **macOS is allow-by-default.** A sandboxed process can read every
  non-denied file, enumerate your processes, and talk to Mach services,
  though it can no longer signal them, read their environments, or hold
  your terminal. `/private/tmp` also stays shared; Seatbelt has no mount
  namespace.
- **No resource limits.** A sandboxed fork bomb is still a fork bomb.
- **A granted agent socket is a signing oracle.** `allowUnixSockets` and the
  `ssh` preset let a prompt-injected command sign with your SSH agent while
  it runs. Prefer `ssh-add -c` for per-use confirmation.
- **Linux socket grants are broader than macOS.** `connect()` cannot be
  path-filtered in seccomp, so once any grant is active, Unix sockets
  outside the private `/tmp`/`/run` roots (a Docker socket under
  `~/.docker/run`, say) also become connectable. Grant sockets
  deliberately.
- **The `ssh` preset does not grant `known_hosts` writes.** First contact
  with an unknown host fails inside the sandbox; connect once outside, or
  manage `known_hosts` on the host.
- **`git commit` needs the `git` component.** Under plain `workspace` the
  whole `.git` tree is read-only. GPG-signed commits still fail (no
  gpg-agent socket is granted); disable signing in the sandbox or sign on
  the host.
