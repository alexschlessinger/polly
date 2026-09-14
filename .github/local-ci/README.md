# Local CI

Linux checks run in a portable Docker image. On this Mac, both manual checks and
GitHub jobs use OrbStack directly. macOS checks use disposable Tart VMs.
Windows runtime tests are disabled; Windows amd64 cross-compilation remains.

## Run checks without GitHub

From a checkout, with Docker running:

```bash
.github/local-ci/linux.sh all
.github/local-ci/linux.sh test HEAD
```

This builds the pinned Linux image and tests the selected committed revision.
It works with Docker on Linux or macOS and needs no Tart or GitHub credentials.
Uncommitted edits are excluded. The source arrives as `git archive` through stdin,
with no host mounts or Docker socket inside the container. Dependencies are baked
into the image; manual test containers have no external network. The image supports
ARM64 and amd64; the installed OrbStack engine runs ARM64.
Compile caches are warmed from a pinned trusted commit during image construction;
the actual test suites run in each job. No job writes caches back to the image.

On the configured Mac, use a worker's installed supervisor to share that slot
with GitHub jobs. Linux has three independent slots; this example uses the first:

```bash
export POLLY_CI_LINUX_ROOT="$HOME/Library/Application Support/PollyCI/linux-1"
python3 "$POLLY_CI_LINUX_ROOT/supervisor.py" --root "$POLLY_CI_LINUX_ROOT" --local all --platform linux
python3 "$HOME/Library/Application Support/PollyCI/supervisor.py" --local test --platform macos
```

`--revision <ref>` and `--repository-dir <path>` select another committed tree.
The supervised Linux command uses the already-built image; rerun `linux.sh` when
Go dependencies change to refresh it. Standalone `linux.sh` runs independently of
that supervisor slot and builds dependencies for its selected revision.

Both environments call `.github/ci.sh`, which also works directly in a prepared
shell:

| Command | Checks |
| --- | --- |
| `test` | Supervisor tests, CGO-free build, vet, full Go suite including required sandbox tests |
| `race` | Race-sensitive tools, storage, conversation, coordination, workflow and worktree packages |
| `cross` | CGO-free CLI builds for Linux amd64/arm64, macOS amd64/arm64, Windows amd64 |
| `all` | All three, stopping on failure |

## GitHub runners and isolation

A LaunchAgent per slot polls the repository queue every 30 seconds. It creates one Linux
container or macOS VM when a job requests `polly-local-linux` or
`polly-local-macos`. Each worker gets a just-in-time runner configuration, handles
one job, and is deleted. The host's GitHub OAuth credential stays in its Keychain;
only the one-job configuration crosses into the worker, on stdin.

This Mac runs three Linux slots, each capped at 3 CPUs and 3 GiB RAM, plus one
independent macOS slot with 6 CPUs and 8 GiB RAM. Go runtime and build parallelism
are also capped at each Linux slot's configured CPU count. Every slot has its own
supervisor process, journal, lock and logs; a slow job holds only its own slot.
The pool's Linux memory caps total 9 GiB within OrbStack's roughly 12 GiB limit.
The default single-worker and standalone Linux configuration remains 6 CPUs / 8 GiB.

When idle, no CI container or VM runs. The Mac must be awake, logged in and online;
OrbStack must be running for Linux jobs. Existing containers are not reconfigured.
A legacy supervisor configured for both platforms alternates them when both have
queued work; the independent pool does not share the macOS slot.

Linux containers run as UID 1000 with all capabilities dropped, no-new-privileges,
and no host filesystem, Docker socket, host PID namespace, or privileged mode.
Bubblewrap's nested namespaces and proc mount require `seccomp=unconfined` and
`systempaths=unconfined`. These relax Docker's outer restrictions; Polly's own
sandbox policy remains enabled and its required tests run. Automatic GitHub
runners need outbound networking and use Docker's normal bridge network. They
share OrbStack's kernel/network environment with other containers; they do not
have a separate VM isolation boundary. This is the selected deployment tradeoff.

macOS VMs clone a trusted, warmed base. No job updates the base. They have no host
shares, clipboard, audio, or SSH forwarding. Softnet blocks host/private-network
access; public DNS is configured inside the guest. Tart's guest agent uses the VM
control socket without needing SSH. Fork PRs retain GitHub-hosted runners in the
workflow; this routing is convenience, not an isolation guarantee against modified
workflow files.

The supervisor journals its worker and runner identity. Recovery reclaims compute
before contacting GitHub, then removes only the matching runner. Container cleanup
matches the exact recorded name and repository label. API outages retain the
journal for retry. Jobs have a 30-minute workflow timeout and a 35-minute supervisor
limit. Linux workers are inspected every 10 seconds; a container restart causes
replacement instead of waiting on a configuration that was already consumed.
The entrypoint also rejects empty configuration and times out its initial stdin
read after 30 seconds. GitHub runner health is checked every 30 seconds: two minutes
offline or online without a job releases the slot. API outages do not count as
worker failure, and quiet test output does not trigger recovery. Normal container
shutdown gets up to 10 seconds for the attached Docker client to return its exit
status, so finishing between health polls does not trigger failure backoff.
Each slot's `logs` directory contains its latest `runner.log` (and `vm.log` for
macOS), plus a rotating supervisor log. macOS uses
`~/Library/Application Support/PollyCI/logs`; Linux uses
`~/Library/Application Support/PollyCI/linux-{1,2,3}/logs`.

Do not restart an individual job container: its registration is supplied only once
on stdin, and its job cannot resume after a restart. The supervisor replaces a
failed worker; rerun the interrupted GitHub job after GitHub marks it completed.

## Three Linux workers on this Mac

From an existing installation, this creates three independent Linux worker roots
and LaunchAgents without starting them:

```bash
python3 .github/local-ci/install_linux_workers.py --workers 3 --cpus 3 --memory 3g
```

It inherits the repository, Docker context, GitHub CLI and Linux image from the
main configuration. Each worker receives a Linux-only configuration and a copy
of the supervisor. Existing different configuration or LaunchAgents are refused
before any worker files are written. Review resource limits against the Docker
VM's memory capacity, not just the Mac's total RAM.

Before starting the pool, stop or drain the main supervisor and remove the `linux`
entry from its `config.json` `platforms` object, leaving `macos`. This prevents an
unintended fourth Linux worker. Copy the current `supervisor.py` to the stopped
main installation, then restart its LaunchAgent. Start the Linux services with:

```bash
for worker in 1 2 3; do
  launchctl bootstrap "gui/$(id -u)" \
    "$HOME/Library/LaunchAgents/com.polly.ci.linux-$worker.plist"
done
```

To refresh pool code, stop or drain its three services before rerunning the
installer, then bootstrap them again. Decreasing `--workers` does not remove
existing services: explicitly boot out any slots being retired. Never share a
worker root between active supervisors, and do not copy an active journal between
slots. The original `install.py` remains the initial single-supervisor installer;
it deliberately refuses the pool's customized main configuration on later runs.

## Add Linux capacity on another host

Each worker has its own slot and recovery journal. GitHub assigns queued jobs
to an available runner with matching labels; the hosts do not need shared storage
or a connection to one another. If two hosts observe the same queued job, the
unused runner times out and releases its slot.

The current workflow and supervisor require **ARM64** for automatic Linux jobs.
Another Apple Silicon Mac or an ARM64 Linux machine with Docker can add capacity.
The standalone `linux.sh` also supports amd64, but an amd64 GitHub worker requires
updating both the supervisor's registration labels and the workflow's `runs-on`
architecture requirement. Do not label an amd64 worker ARM64.

On the additional host, install Git, Python 3, Docker and GitHub CLI, authenticate
`gh` with repository runner administration access, and clone this repository.
Allow at least 6 CPUs and 8 GiB RAM for its worker, plus room for the Docker image
and Go caches. First build the image and verify the host can run the sandbox tests:

```bash
.github/local-ci/linux.sh test HEAD
docker image inspect polly-local-ci:go1.27 --format '{{.Architecture}}'
```

The architecture must be `arm64`. Linux must allow bubblewrap's unprivileged user
namespaces; the required sandbox tests above must pass before enabling the worker.
Use a separate Linux-only configuration instead of the Mac-specific `install.py`:

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

Run that final command under the host's service manager for persistent operation.
Keep the root local to that host; never copy `active.json`, lock files or runner
credentials from another installation. No Tart or Softnet is needed for Linux-only
workers. Leave `POLLY_LOCAL_LINUX=true` in the repository so jobs retain the label
shared by both hosts.

## Install or refresh

Requirements on this Mac: Python 3, GitHub CLI authenticated for repository runner
administration, OrbStack/Docker, Tart 2.37.0 or later, and Softnet. Tart versions
before 2.37.0 have control-socket bugs with paths containing spaces.

```bash
export POLLY_CI_ROOT="$HOME/Library/Application Support/PollyCI"
export TART_HOME="$POLLY_CI_ROOT/tart"
export PATH="$POLLY_CI_ROOT/bin:/opt/homebrew/bin:$PATH"
```

This installation pins Tart 2.37.0 in its own `tools` directory and Softnet 0.19.0.
Install the reviewed Softnet networking helper once with administrator access:

```bash
sudo install -o root -g wheel -m 4755 \
  /opt/homebrew/Cellar/softnet/0.19.0/bin/softnet \
  /Library/PrivilegedHelperTools/com.polly.ci.softnet
python3 .github/local-ci/install.py
.github/local-ci/linux.sh all
```

The installer verifies the helper owner/mode, creates `bin/softnet`, copies the
supervisor and configuration, and writes but does not start the LaunchAgent.
It refuses to replace different existing configuration. The state directory is
mode 0700. Docker's context is explicitly configured as `orbstack` so changing the
interactive Docker context cannot redirect automatic jobs elsewhere.

For macOS, clone this pinned image into `polly-ci-base`:

`ghcr.io/cirruslabs/macos-tahoe-base@sha256:1b093499716409d29e8b5336844528e1cae375db97d2ad8e5aeff78cf0da201e`

Set it to 6 CPUs / 8192 MiB with `tart set`, then boot it:

```bash
tart run --no-graphics --no-audio --no-clipboard --net-softnet-block=@host polly-ci-base
```

Stage `prepare-macos.sh` in the guest, then execute it with five arguments: Go
version, Go archive SHA256, runner version, runner archive SHA256, and a trusted
source commit. Obtain hashes from official Go download metadata and the Actions
runner release assets. The script verifies both archives, configures DNS, disables
SSH, installs toolchains and warms caches with build, vet and required sandbox
tests. Stage the script before running it so subprocesses cannot consume its input:

```bash
tart exec -i polly-ci-base /bin/bash -c \
  'cat > /tmp/prepare-ci.sh && /bin/bash /tmp/prepare-ci.sh <go-version> <go-sha256> <runner-version> <runner-sha256> <trusted-commit> </dev/null' \
  < .github/local-ci/prepare-macos.sh
```

Shut it down with `tart exec polly-ci-base sudo /sbin/shutdown -h now` and wait until
`tart list` reports it stopped. To refresh, stop the supervisor and prepare a new
candidate base. Change the configured base only after its checks pass. Never
promote a VM that ran untrusted work or overwrite a base being cloned by a job.

## Start, stop, and fall back

```bash
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.polly.ci.local.plist"
launchctl print "gui/$(id -u)/com.polly.ci.local"
gh variable set POLLY_LOCAL_MACOS --repo alexschlessinger/polly --body true
gh variable set POLLY_LOCAL_LINUX --repo alexschlessinger/polly --body true
```

Only revisions containing the updated workflow use these variables. Set both to
`false` to return new runs to GitHub-hosted machines. Cancel and rerun jobs already
queued against local labels. To stop all four services on the configured Mac:

```bash
launchctl bootout "gui/$(id -u)/com.polly.ci.local"
for worker in 1 2 3; do
  launchctl bootout "gui/$(id -u)/com.polly.ci.linux-$worker"
done
```

Stopping interrupts active work and triggers cleanup. After a hard crash, the next
supervisor/manual invocation recovers the journal. The image, macOS base and host
helper remain installed when the service is stopped.
