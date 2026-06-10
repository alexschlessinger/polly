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
Concretely, a sandboxed process cannot:

- **Read credentials.** `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.kube`, `~/.netrc`,
  `~/.config/gh`, `~/Library/Keychains`, and the rest of the built-in deny
  list (17 paths) are blocked from reads, plus anything added with
  `--denypath`.
- **See secrets in the environment.** Credential-shaped variables
  (`POLLYTOOL_*`, `AWS_*`, `*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`,
  ...) and agent sockets (`SSH_AUTH_SOCK`, `GPG_AGENT_INFO`) are stripped
  before exec. Agent sockets matter: with `SSH_AUTH_SOCK` intact, a process
  can use your SSH keys without ever reading `~/.ssh`.
- **Write outside temp directories.** The filesystem is read-only except the
  OS temp dir and any configured `writablePaths`.
- **Reach the network.** All network access is denied unless the tool's config
  opts into it (`allowNetwork`, optionally narrowed with `denyDNS`).

Design principles:

- **Default-on.** Tools don't opt in; they may opt out (`"sandbox": false`).
- **Fail closed.** If no backend exists, or a sandbox fails to construct, the
  tool errors instead of silently running unsandboxed. At startup polly probes
  the backend with a trivial command and aborts with a pointer to
  `--nosandbox` if it can't actually start processes.
- **Track the live filesystem.** Deny lists and profiles are rebuilt on every
  command, so a credential dir created mid-session (`aws configure` in another
  terminal) is masked by the next command.
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
| Shell tools (`-t ./tool.sh`) | yes | `"sandbox": false` in the schema |
| Stdio MCP servers | yes (the whole server process) | `"sandbox": false` on the server entry |
| Remote MCP servers (HTTP/SSE) | no — the process runs elsewhere | n/a |
| Skill helper / function tools | no — in-process, nothing to wrap | n/a |

The effective config for a tool is the **base config** (temp-dir writes, plus
any `--denypath` entries) **merged** with the tool's own `sandbox` object.
Merging only widens: booleans OR, path lists append. A tool can grant itself
network access; nothing can un-deny a credential path except an explicit
`readPaths` exemption in its own config.

## Platform implementations

Both platforms share the same policy surface (the `Config` fields) and the
same env filtering, which happens in Go before exec. The enforcement
mechanisms are very different.

### Linux: bubblewrap (`bwrap`)

Each command is re-written to run under [bubblewrap](https://github.com/containers/bubblewrap)
in a fresh mount + PID namespace. The default config renders roughly:

```
bwrap \
  --ro-bind / /                          # entire filesystem read-only
  --bind /tmp /tmp                       # writable temp (+ any writablePaths)
  --tmpfs /home/you/.ssh                 # denied dirs masked with empty tmpfs
  --ro-bind /dev/null /home/you/.netrc   # denied files masked with /dev/null
  ...                                    # one mask per existing denied path
  --dev /dev                             # fresh devtmpfs
  --proc /proc                           # proc scoped to the new PID namespace
  --unshare-pid                          # own PID namespace
  --unshare-net                          # no network (omitted when allowNetwork)
  --die-with-parent
  --new-session                          # detach from the controlling tty
  -- original command...
```

Properties worth knowing:

- **Writes are physically impossible** outside the writable binds — the root
  is a read-only mount, not a policy check.
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
- **Missing deny paths are dropped** — bwrap aborts the whole command if asked
  to mask a nonexistent path. Only a confirmed does-not-exist drops a mask;
  any other resolution failure (permissions, I/O) keeps the path, so the
  command fails rather than running with that path readable.
- A `writablePaths` entry that doesn't exist is skipped per-command (with a
  debug log), not bound — bwrap aborts on a missing bind source, and failing
  construction would brick session restore over one stale path. A path created
  later becomes writable on the next command; writes to a missing path just
  fail at runtime (fail-closed).
- `denyDNS` (with `allowNetwork`) is implemented by masking
  `/etc/resolv.conf` with `/dev/null`.

### macOS: Seatbelt (`sandbox-exec`)

Each command runs as `/usr/bin/sandbox-exec -p <profile> original command...`
with a profile generated per command. The default config renders:

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
- **Rules are emitted for both the literal path and its symlink-resolved
  target.** Seatbelt matches resolved vnode paths, so a rule on a
  dotfiles-managed symlink (`~/.npmrc -> ~/dotfiles/npmrc`) would never fire
  for the real file; naming both closes the hole.
- **Last-match-wins** is how `readPaths` exemptions work: they're emitted as
  `allow file-read*` rules after the denies.
- Deny rules are emitted whether or not the path exists (harmless, unlike
  bwrap), so no existence filtering is needed.
- A missing `writablePaths` entry renders an inert profile rule (Seatbelt
  ignores it), so it's tolerated the same as on Linux — a stale path never
  fails tool loading.
- `denyDNS` blocks the system resolver socket (`mDNSResponder`) plus direct
  port-53 UDP/TCP.
- `sandbox-exec` is deprecated by Apple but remains functional and is what
  Bazel, Nix, and Chromium-family tooling ride on; there is no supported
  replacement API for ad-hoc profiles.

### Differences at a glance

| | Linux (bwrap) | macOS (Seatbelt) | Unified? |
|---|---|---|---|
| File reads / credentials | whole fs readable, 17-path deny list | whole fs readable, same deny list | ✅ effect matches |
| Writes | read-only root + temp binds | `deny file-write*` + temp allows | ✅ effect matches |
| Network | denied (`--unshare-net`) | denied (`deny network*`) | ✅ effect matches |
| Env stripping | shared Go-side filtering | shared Go-side filtering | ✅ identical code |
| Cross-process env read | blocked by PID namespace | blocked by the OS (`KERN_PROCARGS2` truncates) | ✅ effect matches |
| Signal other processes | invisible, can't signal | `deny signal` + self/same-sandbox re-allow | ✅ effect matches |
| Controlling terminal | detached (`--new-session`) | detached (`setsid()`) | ✅ effect matches |
| Missing `writablePaths` | skipped per-command | inert profile rule | ✅ tolerated, not fatal |
| **Denied-read failure mode** | reads as empty | `Operation not permitted` | ❌ inherent (masking vs policy) |
| **Process enumeration** | invisible (PID namespace) | visible (`KERN_PROC` sysctl, ungated) | ❌ inherent |
| **Mach / IPC** | n/a | allowed (allow-default) | ❌ inherent (no Linux analogue) |
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
- **Mach/IPC** has no Linux counterpart; flipping macOS to `(deny default)` to
  match Linux's posture would break most tools (you'd have to allowlist every
  syscall, file read, and mach service), so it stays allow-default.

## Configuration

Seven knobs, settable per tool (schema `"sandbox"` object, MCP server entry) on
top of the global flags:

| Field | Default | Effect |
|---|---|---|
| `writablePaths` | `[]` (temp only) | extra write-allowed directories (`~` ok) |
| `allowNetwork` | `false` | permit outbound network |
| `denyDNS` | `false` | with `allowNetwork`: block name resolution |
| `readPaths` | `[]` | exempt entries from the deny list |
| `denyPaths` | `[]` | extra read-blocked paths (`~` ok) |
| `allowEnv` | `[]` | strict env allowlist — replaces the heuristic stripping |
| `denyWrite` | `false` | no writes anywhere, not even temp |

Global flags: `--nosandbox` (`POLLYTOOL_NOSANDBOX`) disables everything;
`--denypath` (`POLLYTOOL_DENYPATHS`, repeatable) adds read-blocked paths for
all sandboxed tools.

Two interactions to keep in mind:

- `denyDNS` only matters when `allowNetwork` is true (no network ⊃ no DNS).
- `allowEnv` is a mode switch, not an addition: when set, *only* those
  variables pass through. Use it to hand a tool one specific token —
  `"allowEnv": ["GITHUB_TOKEN"]` — that the heuristics would otherwise strip.

### Examples

Shell tool that fetches URLs but can't resolve arbitrary hostnames or write
outside its cache:

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
... DBG sandbox_wrap command="bash -c" network=false deny_write=false
        writable_paths="[/tmp]" env_stripped="[OPENAI_API_KEY SSH_AUTH_SOCK]"
        denied_paths=17
```

`env_stripped` and all path fields log **names only, never values**.
`command` is truncated to the first two argv entries.

In the REPL:

- Startup prints a posture line:
  `sandbox: active (3 tools sandboxed; not sandboxed: weather)` — or
  `sandbox: disabled (--nosandbox)`.
- `/get sandbox` shows the live state:
  `active (denypaths: 2; tools: 3 sandboxed, 1 not: weather)`.
- `/tools list` marks each sandboxed tool with `[sandboxed]`, and
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
- **Temp is shared.** `/tmp` is bind-mounted from the host on Linux (and
  `/private/tmp` is writable on macOS), so sandboxed processes share temp
  with each other and with the host — they can read and interfere with other
  processes' temp files, and stage data there for a later non-sandboxed
  exfiltration path.
- **The deny list is a list.** Credentials living outside the 17 built-in
  paths (a `.env` in your project, a token in a config the list doesn't know)
  are readable unless you add them with `--denypath` or `denyPaths`.
- **No resource limits.** CPU, memory, disk-in-temp, and process count are
  unconstrained; a sandboxed fork bomb is still a fork bomb.
- **Env heuristics are heuristics.** A secret in a variable named `CREDS`
  passes the suffix patterns. Use `allowEnv` for tools that should see almost
  nothing.
