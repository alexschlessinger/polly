# Local CI

Run Linux checks in Docker and macOS checks in disposable Tart VMs. The configured
Apple Silicon host uses OrbStack with three Linux workers and one macOS worker.
Windows is cross-compiled; there are no Windows runtime tests.

## Contents

- [Run checks locally](#run-checks-locally)
- [Runner isolation](#runner-isolation)
- [Recovery and logs](#recovery-and-logs)
- [Install the Mac runner](#install-the-mac-runner)
- [Configure three Linux workers](#configure-three-linux-workers)
- [Add another Linux host](#add-another-linux-host)
- [Start, stop, and fall back](#start-stop-and-fall-back)

[Documentation index](../../docs/README.md) · [Sandbox validation](../../docs/SANDBOX_ENVIRONMENT_VALIDATION.md)

## Run checks locally

From a checkout with Docker running:

```bash
.github/local-ci/linux.sh all
.github/local-ci/linux.sh test HEAD
```

This builds the pinned image and tests the selected **committed revision**.
Uncommitted edits are excluded. It works on Linux/macOS with ARM64 or amd64 Docker;
no Tart or GitHub credentials are needed.

Source arrives through `git archive` on stdin. The test container has no host
mounts, Docker socket, or external network. Dependencies and warmed compile caches
are baked into the image; each job runs its own tests and cannot update the image.

Both platforms call [`.github/ci.sh`](../ci.sh), which also runs in a prepared shell:

| Argument | Checks |
|---|---|
| `test` | Python supervisor tests, CGO-free build, vet, full Go suite with required sandbox tests |
| `race` | Race checks for tools, sessions, CLI, LLM, subagent, swarm, workflow, and worktree packages |
| `cross` | CGO-free CLI builds for Linux/macOS amd64/arm64 and Windows amd64 |
| `warm` | Compile-only pass that fills the caches the other modes read; the Docker image build runs it |
| `all` | `test`, `race`, and `cross`, stopping on failure |

The workflow runs `race` for about one run in five (run ids ending in 0 or 5)
and on every manual dispatch; `ci.sh all` always includes it.

To share an installed worker slot with GitHub jobs:

```bash
export POLLY_CI_LINUX_ROOT="$HOME/Library/Application Support/PollyCI/linux-1"
python3 "$POLLY_CI_LINUX_ROOT/supervisor.py" --root "$POLLY_CI_LINUX_ROOT" --local all --platform linux
python3 "$HOME/Library/Application Support/PollyCI/supervisor.py" --local test --platform macos
```

`--revision <ref>` and `--repository-dir <path>` select another tree. Supervised
Linux runs reuse the installed image; rebuild with `linux.sh` when dependencies
change. Standalone `linux.sh` runs independently of supervisor slots.

## Runner isolation

Each LaunchAgent polls the repository queue every 30 seconds. A job labeled
`polly-local-linux` or `polly-local-macos` gets a fresh one-job runner, deleted on
completion. The host's GitHub OAuth credential stays in Keychain; only the
just-in-time configuration reaches the worker, on stdin.

| Worker | Configured resources | Boundary |
|---|---|---|
| Linux × 3 | 3 CPUs / 3 GiB each | OrbStack containers with independent supervisors, journals, locks, and logs |
| macOS × 1 | 6 CPUs / 8 GiB | Disposable clone of a trusted, warmed Tart base |
| Standalone/default Linux | 6 CPUs / 8 GiB | Docker container |

Linux Go runtime/build parallelism follows the CPU cap. Size the pool against the
Docker VM's memory limit. No worker runs while idle. The Mac must be awake, logged
in, online, and running OrbStack for Linux jobs.

### Linux

Workers use UID 1000, dropped capabilities, and `no-new-privileges`. There are no
host mounts, host PID namespace, Docker socket, or privileged mode.
`seccomp=unconfined` and `systempaths=unconfined` let bubblewrap create nested
namespaces/proc mounts; Polly's own required sandbox tests still run.

GitHub workers need outbound networking and use Docker's bridge. They share the
OrbStack kernel/network environment with other containers, without a separate VM
boundary per worker.

### macOS

Jobs cannot update the base. VMs have no host shares, clipboard, audio, or SSH
forwarding. Softnet blocks host/private-network access; guests use public DNS.
Tart's control socket runs guest commands without SSH.

Fork PRs use GitHub-hosted runners in the workflow. That routing is a convenience,
not an isolation guarantee against modified workflow files.

## Recovery and logs

The supervisor journals worker/runner identity. Recovery reclaims compute first,
then removes only the matching GitHub runner. Container cleanup checks the exact
recorded name and repository label. API outages retain the journal for retry.

| Limit/check | Behavior |
|---|---|
| Workflow / supervisor | 30-minute job timeout / 35-minute worker limit |
| Linux inspection | Every 10 seconds; restarted containers are replaced |
| Initial stdin | Empty configuration is refused; read times out after 30 seconds |
| Runner health | Every 30 seconds; two minutes offline or online without a job releases the slot |
| Normal shutdown | Up to 10 seconds for the attached Docker client to return its status |

API outages and quiet test output do not count as worker failure.
**Do not restart a job container:** its registration arrives only once and the
job cannot resume. Let the supervisor replace it, then rerun the interrupted
GitHub job after it reaches a terminal state.

Logs contain the latest `runner.log`, macOS `vm.log`, and rotating supervisor log:

- macOS: `~/Library/Application Support/PollyCI/logs`
- Linux: `~/Library/Application Support/PollyCI/linux-{1,2,3}/logs`

## Install the Mac runner

Needs Python 3, GitHub CLI with repository runner administration access,
OrbStack/Docker, Tart 2.37.0 or later, and Softnet. This setup pins Tart 2.37.0 in
its own tools directory and Softnet 0.19.0.

```bash
export POLLY_CI_ROOT="$HOME/Library/Application Support/PollyCI"
export TART_HOME="$POLLY_CI_ROOT/tart"
export PATH="$POLLY_CI_ROOT/bin:/opt/homebrew/bin:$PATH"

sudo install -o root -g wheel -m 4755 \
  /opt/homebrew/Cellar/softnet/0.19.0/bin/softnet \
  /Library/PrivilegedHelperTools/com.polly.ci.softnet
python3 .github/local-ci/install.py
.github/local-ci/linux.sh all
```

The installer verifies the helper owner/mode, creates `bin/softnet`, and writes
configuration and a LaunchAgent without starting it. It refuses differing existing
configuration. State is mode 0700; Docker context is explicitly `orbstack`.

### Prepare the macOS base

Clone this image as `polly-ci-base` and set 6 CPUs / 8192 MiB with `tart set`:

`ghcr.io/cirruslabs/macos-tahoe-base@sha256:1b093499716409d29e8b5336844528e1cae375db97d2ad8e5aeff78cf0da201e`

```bash
tart run --no-graphics --no-audio --no-clipboard --net-softnet-block=@host polly-ci-base
```

Stage `prepare-macos.sh` before executing it so child processes cannot consume its
input. Supply Go version/hash, Actions runner version/hash, and a trusted source
commit. Obtain hashes from official release metadata.

```bash
tart exec -i polly-ci-base /bin/bash -c \
  'cat > /tmp/prepare-ci.sh && /bin/bash /tmp/prepare-ci.sh <go-version> <go-sha256> <runner-version> <runner-sha256> <trusted-commit> </dev/null' \
  < .github/local-ci/prepare-macos.sh
```

The script verifies archives, configures DNS, disables SSH, installs the
toolchains and Homebrew's git (it starts several times faster than the
`/usr/bin/git` stub, and the sandbox trusts its Cellar route), and warms caches
by running `ci.sh test` at the trusted commit, which also proves the git route. Shut down with
`tart exec polly-ci-base sudo /sbin/shutdown -h now`; wait for `tart list` to show
it stopped.

To refresh, stop the supervisor and prepare a candidate base. Select it only after
checks pass. Never promote a VM that ran untrusted work or overwrite a base while
a job clones it.

## Configure three Linux workers

From an existing installation:

```bash
python3 .github/local-ci/install_linux_workers.py --workers 3 --cpus 3 --memory 3g
```

This creates independent Linux roots/LaunchAgents without starting them. It
inherits repository, Docker context, GitHub CLI, and image settings. Differing
existing configuration is refused before writing worker files.

Before starting the pool, stop or drain the main supervisor. Remove `linux` from
its `config.json` `platforms`, leaving `macos`, to avoid a fourth Linux worker.
Copy the current `supervisor.py` to the stopped main installation and restart it.
Then start the pool:

```bash
for worker in 1 2 3; do
  launchctl bootstrap "gui/$(id -u)" \
    "$HOME/Library/LaunchAgents/com.polly.ci.linux-$worker.plist"
done
```

Stop/drain the pool before rerunning its installer to refresh code. Reducing
`--workers` does not remove services; explicitly boot out retired slots. Never
share roots between active supervisors or copy active journals between slots.
`install.py` is for initial setup and refuses the pool's customized main config.

## Add another Linux host

Automatic Linux jobs require **ARM64** in both workflow and supervisor labels.
Use an Apple Silicon Mac or ARM64 Linux host with Docker. Standalone `linux.sh`
also supports amd64; an amd64 automatic worker needs matching changes to both
registration labels and workflow architecture. Never label it ARM64.

Install Git, Python 3, Docker, and GitHub CLI; authenticate `gh` for repository
runner administration and clone the repository. Allow 6 CPUs / 8 GiB plus image
and cache space. Build and verify before enabling the worker:

```bash
.github/local-ci/linux.sh test HEAD
docker image inspect polly-local-ci:go1.27 --format '{{.Architecture}}'
```

The image must report `arm64`, and native sandbox tests must pass. Create a
Linux-only configuration:

```bash
export POLLY_CI_ROOT="$HOME/.local/state/polly-ci"
python3 - <<'PY'
import json, os, shutil, subprocess
from pathlib import Path

root = Path(os.environ["POLLY_CI_ROOT"])
root.mkdir(parents=True, exist_ok=True, mode=0o700)
root.chmod(0o700)
(root / "logs").mkdir(exist_ok=True)
config = {
    "repository": "alexschlessinger/polly",
    "gh": shutil.which("gh"),
    "docker": shutil.which("docker"),
    "docker_context": subprocess.check_output(["docker", "context", "show"], text=True).strip(),
    "platforms": {"linux": {
        "engine": "docker", "image": "polly-local-ci:go1.27",
        "label": "polly-local-linux", "os": "Linux"
    }}
}
assert config["gh"] and config["docker"], "GitHub CLI and Docker are required"
path = root / "config.json"
if path.exists() and json.loads(path.read_text()) != config:
    raise SystemExit("Existing configuration differs; inspect it before replacing")
path.write_text(json.dumps(config, indent=2) + "\n")
path.chmod(0o600)
shutil.copyfile(".github/local-ci/supervisor.py", root / "supervisor.py")
PY
python3 "$POLLY_CI_ROOT/supervisor.py" --root "$POLLY_CI_ROOT"
```

Run the last command under the host's service manager. Keep state local; do not
copy `active.json`, locks, or runner credentials from another installation.
Linux-only hosts need no Tart or Softnet. Keep `POLLY_LOCAL_LINUX=true` in the
repository so hosts share the job label. If hosts race for one queued job, the
unused runner times out and releases its slot.

## Start, stop, and fall back

```bash
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.polly.ci.local.plist"
launchctl print "gui/$(id -u)/com.polly.ci.local"
gh variable set POLLY_LOCAL_MACOS --repo alexschlessinger/polly --body true
gh variable set POLLY_LOCAL_LINUX --repo alexschlessinger/polly --body true
```

Set both variables to `false` for new runs on GitHub-hosted machines. Cancel and
rerun jobs already queued on local labels. Stop all four configured services with:

```bash
launchctl bootout "gui/$(id -u)/com.polly.ci.local"
for worker in 1 2 3; do
  launchctl bootout "gui/$(id -u)/com.polly.ci.linux-$worker"
done
```

Stopping interrupts active work and triggers cleanup. After a hard crash, the
next supervisor/manual invocation recovers the journal. Images, the macOS base,
and the host helper remain installed.
