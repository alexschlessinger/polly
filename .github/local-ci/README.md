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

On the configured Mac, use the installed supervisor to share its single worker
slot with GitHub jobs:

```bash
python3 "$HOME/Library/Application Support/PollyCI/supervisor.py" --local all --platform linux
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

A LaunchAgent polls the repository queue every 30 seconds. It creates one Linux
container or macOS VM when a job requests `polly-local-linux` or
`polly-local-macos`. Each worker gets a just-in-time runner configuration, handles
one job, and is deleted. The host's GitHub OAuth credential stays in its Keychain;
only the one-job configuration crosses into the worker, on stdin.

Supervised jobs run serially with 6 CPUs and 8 GiB RAM per worker. When idle, no CI
container or VM runs. The Mac must be awake, logged in and online; OrbStack must be
running for Linux jobs. Existing containers are not stopped or reconfigured.

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
limit. Logs live under `~/Library/Application Support/PollyCI/logs`: the latest
`runner.log` and `vm.log`, plus a rotating supervisor log.

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
queued against local labels. To stop the service:

```bash
launchctl bootout "gui/$(id -u)/com.polly.ci.local"
```

Stopping interrupts active work and triggers cleanup. After a hard crash, the next
supervisor/manual invocation recovers the journal. The image, macOS base and host
helper remain installed when the service is stopped.
