#!/bin/bash
# Shared entry point for GitHub Actions and disposable local CI VMs.
set -euo pipefail
cd "$(dirname "$0")/.."

case "${1:-all}" in
  test)
    python3 -B -m unittest discover -s .github/local-ci -p 'test_*.py'
    CGO_ENABLED=0 go build ./...
    go vet ./...
    POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 CGO_ENABLED=0 go test ./...
    ;;
  race)
    CGO_ENABLED=1 go test -race ./tools ./sessions ./cmd/polly ./llm ./subagent ./swarm ./workflow ./worktree
    ;;
  cross)
    output=$(mktemp -d)
    trap 'rm -rf "$output"' EXIT
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
      echo "Building $target without CGO"
      CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -o "$output/${target//\//-}" ./cmd/polly
    done
    ;;
  all)
    "$0" test
    "$0" race
    "$0" cross
    ;;
  *)
    echo 'usage: .github/ci.sh [test|race|cross|all]' >&2
    exit 2
    ;;
esac
