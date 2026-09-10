#!/bin/bash
# Execute inside a fresh, trusted Ubuntu base VM, never on the host.
set -euo pipefail
[[ "$HOME" == /home/admin && "$(uname -s)" == Linux ]] || { echo 'Run only inside the CI VM' >&2; exit 1; }
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
sudo systemctl disable --now ssh.service
if systemctl cat ssh.socket >/dev/null 2>&1; then
  sudo systemctl disable --now ssh.socket
fi
sudo mkdir -p /etc/systemd/resolved.conf.d
printf '%s\n' '[Resolve]' 'DNS=1.1.1.1 8.8.8.8' 'Domains=~.' | sudo tee /etc/systemd/resolved.conf.d/polly-ci.conf >/dev/null
sudo systemctl restart systemd-resolved
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y bubblewrap build-essential git curl ca-certificates libicu-dev python3
if [[ -e /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]]; then
  printf '%s\n' 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/90-polly-ci.conf >/dev/null
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
fi
/usr/bin/bwrap --ro-bind / / --dev /dev --proc /proc --unshare-pid --die-with-parent /usr/bin/true

cd "$HOME/actions-runner"
curl -fL --retry 3 -o runner.tar.gz "https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-linux-arm64-${runner_version}.tar.gz"
printf '%s  %s\n' "$runner_sha256" runner.tar.gz | sha256sum -c -
tar -xzf runner.tar.gz
rm runner.tar.gz
go_dir="$HOME/actions-runner/_work/_tool/go/$go_version/arm64"
mkdir -p "$go_dir"
curl -fL --retry 3 -o go.tar.gz "https://go.dev/dl/go${go_version}.linux-arm64.tar.gz"
printf '%s  %s\n' "$go_sha256" go.tar.gz | sha256sum -c -
tar -xzf go.tar.gz -C "$go_dir" --strip-components=1
rm go.tar.gz
touch "${go_dir}.complete"
sudo ln -sf "$go_dir/bin/go" /usr/local/bin/go
sudo ln -sf "$go_dir/bin/gofmt" /usr/local/bin/gofmt
export PATH="$go_dir/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

cat > polly-run.sh <<'SCRIPT'
#!/bin/bash
set -euo pipefail
cd /home/admin/actions-runner
export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export RUNNER_TOOL_CACHE=/home/admin/actions-runner/_work/_tool
IFS= read -r jit_config
exec ./run.sh --jitconfig "$jit_config"
SCRIPT
chmod 700 polly-run.sh

git clone --no-checkout https://github.com/alexschlessinger/polly.git "$HOME/polly-warmup"
cd "$HOME/polly-warmup"
git checkout --detach "$source_revision"
go mod download
CGO_ENABLED=0 go build ./...
go vet ./...
POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 CGO_ENABLED=0 go test ./...
go test -race ./tools ./sessions ./cmd/polly ./llm ./subagent ./swarm ./workflow ./worktree
output=$(mktemp -d)
trap 'rm -rf "$output"' EXIT
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -o "$output/${target//\//-}" ./cmd/polly
done
cd "$HOME"
rm -rf "$HOME/polly-warmup"
printf 'Prepared base at %s with Go %s and runner %s\n' "$source_revision" "$go_version" "$runner_version"
