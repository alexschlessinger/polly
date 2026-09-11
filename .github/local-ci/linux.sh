#!/bin/bash
# Portable Docker entry point: no Tart, GitHub account, host mounts, or daemon socket in the container.
set -euo pipefail
mode=${1:-all}
case "$mode" in test|race|cross|all) ;; *) echo 'usage: linux.sh [test|race|cross|all] [revision]' >&2; exit 2 ;; esac
script_dir=$(cd "$(dirname "$0")" && pwd)
repo=$(git -C "$script_dir" rev-parse --show-toplevel)
revision=$(git -C "$repo" rev-parse --verify --end-of-options "${2:-HEAD}^{commit}")
git -C "$repo" cat-file -e "$revision:.github/ci.sh"
temporary=$(mktemp -d)
cleanup() {
  if [[ -s "$temporary/container-id" ]]; then
    docker rm -f "$(cat "$temporary/container-id")" >/dev/null 2>&1 || true
  fi
  rm -rf "$temporary"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$temporary/build"
cp "$script_dir/Dockerfile" "$script_dir/entrypoint.sh" "$temporary/build/"
git -C "$repo" show "$revision:go.mod" > "$temporary/build/go.mod"
git -C "$repo" show "$revision:go.sum" > "$temporary/build/go.sum"
git -C "$repo" archive --format=tar "$revision" > "$temporary/tree.tar"
# The tag is only a local convenience; Docker's layer cache checks all inputs.
image=polly-local-ci:go1.27
docker build --label com.polly.ci=true --iidfile "$temporary/image-id" -t "$image" "$temporary/build"
image=$(cat "$temporary/image-id")
echo "Running $mode in Docker at $revision"
docker run --rm --init --cidfile "$temporary/container-id" --cpus 6 --memory 8g \
  --network none --cap-drop ALL --user 1000:1000 \
  --security-opt no-new-privileges --security-opt seccomp=unconfined \
  --security-opt systempaths=unconfined -i "$image" "$mode" < "$temporary/tree.tar"
