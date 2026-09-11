#!/bin/bash
set -euo pipefail
case "${1:-all}" in
  runner)
    cd "$HOME/actions-runner"
    IFS= read -r jit_config
    exec ./run.sh --jitconfig "$jit_config"
    ;;
  test|race|cross|all)
    mkdir -p "$HOME/polly"
    tar -xf - -C "$HOME/polly"
    cd "$HOME/polly"
    exec /bin/bash .github/ci.sh "$1"
    ;;
  *) echo 'expected test, race, cross, all, or runner' >&2; exit 2 ;;
esac
