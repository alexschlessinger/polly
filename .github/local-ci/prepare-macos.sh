#!/bin/bash
# Execute inside a fresh, trusted macOS base VM, never on the host.
set -euo pipefail
[[ "$HOME" == /Users/admin ]] || { echo 'Run only inside the CI VM' >&2; exit 1; }

go_version=$1
go_sha256=$2
runner_version=$3
runner_sha256=$4
source_revision=$5
[[ "$go_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$runner_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$go_sha256" =~ ^[a-f0-9]{64}$ && "$runner_sha256" =~ ^[a-f0-9]{64}$ ]]
[[ "$source_revision" =~ ^[a-f0-9]{40}$ ]]

umask 077
mkdir -p "$HOME/actions-runner"
sudo networksetup -setdnsservers Ethernet 1.1.1.1 8.8.8.8
sudo launchctl disable system/com.openssh.sshd
sudo launchctl bootout system/com.openssh.sshd || true

cd "$HOME/actions-runner"
curl -fL --retry 3 -o runner.tar.gz "https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-osx-arm64-${runner_version}.tar.gz"
printf '%s  %s\n' "$runner_sha256" runner.tar.gz | shasum -a 256 -c -
tar -xzf runner.tar.gz
rm runner.tar.gz
go_dir="$HOME/actions-runner/_work/_tool/go/$go_version/arm64"
mkdir -p "$go_dir"
curl -fL --retry 3 -o go.tar.gz "https://go.dev/dl/go${go_version}.darwin-arm64.tar.gz"
printf '%s  %s\n' "$go_sha256" go.tar.gz | shasum -a 256 -c -
tar -xzf go.tar.gz -C "$go_dir" --strip-components=1
rm go.tar.gz
touch "${go_dir}.complete"
sudo mkdir -p /usr/local/bin
sudo ln -sf "$go_dir/bin/go" /usr/local/bin/go
sudo ln -sf "$go_dir/bin/gofmt" /usr/local/bin/gofmt
export PATH="$go_dir/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"

# Homebrew's git starts in a fraction of the time of the /usr/bin/git stub,
# which locates the Command Line Tools on every launch; git-heavy test packages
# spend most of their time there. The runner PATH puts /opt/homebrew/bin first
# and the sandbox trusts the Cellar route, which the warmup below exercises.
HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_ENV_HINTS=1 HOMEBREW_NO_INSTALL_CLEANUP=1 brew install --quiet git
[[ "$(command -v git)" == /opt/homebrew/bin/git ]]

cat > polly-run.sh <<'SCRIPT'
#!/bin/bash
set -euo pipefail
cd /Users/admin/actions-runner
export PATH="/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export RUNNER_TOOL_CACHE=/Users/admin/actions-runner/_work/_tool
IFS= read -r jit_config
exec ./run.sh --jitconfig "$jit_config"
SCRIPT
chmod 700 polly-run.sh

# Warm immutable, disposable copies of the Go caches using trusted main only.
# No job ever writes its state back into this base image.
git init -q "$HOME/polly-warmup"
cd "$HOME/polly-warmup"
git fetch -q --depth 1 https://github.com/alexschlessinger/polly.git "$source_revision"
git checkout -q --detach FETCH_HEAD
/bin/bash .github/ci.sh test
cd "$HOME"
rm -rf "$HOME/polly-warmup"
printf 'Prepared base at %s with Go %s, runner %s and %s\n' "$source_revision" "$go_version" "$runner_version" "$(git --version)"
