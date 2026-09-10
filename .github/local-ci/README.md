# Local CI on an Apple silicon Mac

The Mac hosts disposable macOS and Ubuntu VMs using Tart. A small LaunchAgent
polls this repository's GitHub Actions queue and starts one VM when a job requests
`polly-local-macos` or `polly-local-linux`. Each VM receives a just-in-time runner
configuration, handles one job, and is deleted. The warmed base is never updated
from a job. Nothing runs in the primary working checkout.

Only one VM runs at a time, with 6 CPUs and 8 GiB RAM. When idle, the supervisor
polls every 30 seconds and both VMs are stopped. The Mac must be awake, logged in,
and online to serve GitHub jobs. Turning off a routing variable affects new runs;
cancel and rerun already-queued runs after changing it.

## Run CI without GitHub

After installation, from a repository checkout:

```bash
python3 "$HOME/Library/Application Support/PollyCI/supervisor.py" \
  --local all --platform linux
python3 "$HOME/Library/Application Support/PollyCI/supervisor.py" \
  --local test --platform macos
```

Use `--revision <commit-or-ref>` and `--repository-dir <path>` to select a committed
tree. Uncommitted edits are not included. The supervisor copies `git archive`
through the guest control channel; it does not mount the checkout or copy host Git
credentials. Manual runs work without the GitHub API, share the one-VM slot with
GitHub jobs, print test output, and return a failure status when a check fails.

Both modes call `.github/ci.sh`, which can also run directly in an already prepared
environment:

| Command | Checks |
| --- | --- |
| `test` | Supervisor tests, CGO-free build, vet, full Go suite including required sandbox tests |
| `race` | Race-sensitive tools, storage, conversation, coordination, workflow and worktree packages |
| `cross` | CGO-free CLI builds for Linux amd64/arm64, macOS amd64/arm64, Windows amd64 |
| `all` | All three, stopping at the first failure |

Windows runtime tests are disabled. Local Linux runtime tests run on ARM64;
Linux amd64 is cross-compiled but is not runtime-tested by the local runner.
Fork PRs keep GitHub-hosted runners. This routing is convenience, not an isolation
boundary: PRs can change workflow files, so every locally dispatched job must be
treated as untrusted.

## Isolation and lifecycle

VMs have no host directory shares, clipboard, audio, SSH forwarding, or host
GitHub credential. Softnet blocks `@host` and rejects private-network destinations;
guests use public DNS because the host DNS gateway is blocked. Tart's guest agent
uses the VM control socket, independent of IP networking. SSH is disabled in the
bases. The runner's short-lived job configuration is passed on stdin.

The supervisor uses a journal and an exclusive lock. Recovery stops and deletes
the recorded disposable VM before contacting GitHub, then removes only the runner
whose name matches that VM. An API outage retains the journal for another cleanup
attempt. A crashed job is bounded by the 30-minute workflow timeout and 35-minute
supervisor timeout. Logs live under `~/Library/Application Support/PollyCI/logs`;
`runner.log` and `vm.log` contain the latest job, and the supervisor log rotates.

## Installation and base refresh

Prerequisites: Apple silicon macOS, Python 3, GitHub CLI authenticated for repository
runner administration, Tart **2.37.0 or later**, and Softnet. Earlier Tart releases
have control-socket bugs with paths containing spaces. The installation on this
Mac pins Tart 2.37.0 in the task's `tools` directory and Softnet 0.19.0.

The state directory is `~/Library/Application Support/PollyCI`, mode 0700.
Set these variables for maintenance commands:

```bash
export POLLY_CI_ROOT="$HOME/Library/Application Support/PollyCI"
export TART_HOME="$POLLY_CI_ROOT/tart"
export PATH="$POLLY_CI_ROOT/bin:/opt/homebrew/bin:$PATH"
```

Softnet needs its documented privileged networking helper. Install the reviewed
binary once using administrator access, without granting the runner host sudo:

```bash
sudo install -o root -g wheel -m 4755 \
  /opt/homebrew/Cellar/softnet/0.19.0/bin/softnet \
  /Library/PrivilegedHelperTools/com.polly.ci.softnet
```

`install.py` checks the helper owner/mode and creates the task-owned `bin/softnet`
link. It copies the supervisor/configuration and writes, but does not start, the
LaunchAgent. It refuses to overwrite unrelated configuration.

```bash
python3 .github/local-ci/install.py
```

Prepare fresh bases, never a previously used job VM. These are the source image
pins used for this installation:

| Base | Image |
| --- | --- |
| `polly-ci-base` | `ghcr.io/cirruslabs/macos-tahoe-base@sha256:1b093499716409d29e8b5336844528e1cae375db97d2ad8e5aeff78cf0da201e` |
| `polly-ci-linux-base` | `ghcr.io/cirruslabs/ubuntu@sha256:e1814edfeddabeaed5e6bdc646445c96701618d6a2a3d0fe993b0c2914417c07` |

Clone with `tart clone <image> <base>`, then `tart set <base> --cpu 6 --memory 8192`.
Give Linux a 50 GB disk with `--disk-size 50`. Boot one base at a time:

```bash
tart run --no-graphics --no-audio --no-clipboard --net-softnet-block=@host <base>
```

Run the matching `prepare-macos.sh` or `prepare-linux.sh` inside that VM. The five
arguments are Go version, Go archive SHA256, runner version, runner archive SHA256,
and a trusted source commit used to warm caches. Obtain archive hashes from the
official Go download metadata and GitHub Actions runner release assets. Scripts
verify both archives before extracting. They install the toolchain, configure the
sandbox backend, and run build, vet and the full sandbox-enabled suite. Linux also
warms the race suite and all five cross-builds. Stage the script in the guest before
running it, so subprocesses cannot consume the script's input stream:

```bash
tart exec -i <base> /bin/bash -c \
  'cat > /tmp/prepare-ci.sh && /bin/bash /tmp/prepare-ci.sh <go-version> <go-sha256> <runner-version> <runner-sha256> <trusted-commit> </dev/null' \
  < .github/local-ci/prepare-linux.sh
```

Use the macOS script for the macOS base. Stop the VM cleanly before using it as a
base: `tart exec <base> sudo /sbin/shutdown -h now`, then wait until `tart list`
reports it stopped.

When refreshing bases, stop the supervisor first and prepare new candidate names.
Only change the configured base names after their checks pass. Do not overwrite a
base while a job is cloning it, or promote a VM that ran untrusted work.

## Start, stop, and fall back

```bash
launchctl bootstrap "gui/$(id -u)" \
  "$HOME/Library/LaunchAgents/com.polly.ci.local.plist"
launchctl print "gui/$(id -u)/com.polly.ci.local"
```

Enable routing after both bases are ready and the supervisor is running:

```bash
gh variable set POLLY_LOCAL_MACOS --repo alexschlessinger/polly --body true
gh variable set POLLY_LOCAL_LINUX --repo alexschlessinger/polly --body true
```

Only revisions containing the updated workflow use those variables. To return new
runs to GitHub-hosted machines, set both variables to `false`. Then stop the service:

```bash
launchctl bootout "gui/$(id -u)/com.polly.ci.local"
```

Stopping interrupts an active local job and triggers disposable-VM cleanup. If a
hard crash prevents cleanup, the next supervisor or manual invocation recovers the
journal. The helper, bases and state remain installed when the service is stopped.
