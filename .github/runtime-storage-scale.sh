#!/bin/bash
# Small observational foundation only; the combined 1m/10k/1k stress runner is
# intentionally not implemented here. Reuse configured Go caches and test temp
# cleanup. The event stream is consumed without creating fixture/output files.
set -euo pipefail
cd "$(dirname "$0")/.."

case "${1:-quick}" in
  quick)
    if [ "$#" -gt 1 ]; then
      printf '%s\n' 'quick takes no additional arguments' >&2
      exit 2
    fi
    ;;
  stress)
    printf '%s\n' 'Full runtime/storage mixed-workload stress is not implemented yet; quick is observational foundation only.' >&2
    exit 2
    ;;
  *)
    printf '%s\n' 'usage: bash .github/runtime-storage-scale.sh quick' >&2
    exit 2
    ;;
esac

packages=(./internal/runtimemetrics ./internal/runtimescale ./sessions ./swarm ./llm ./workflow ./tools ./worktree)
# Enable the small 1/4/8-worker Git curves; this is not the combined stress case.
# Native sandbox cancellation checks remain explicitly opt-in via the existing
# POLLYTOOL_REQUIRE_SANDBOX_TESTS setting.
# pipefail preserves a failing go test even if the reporter exits successfully.
# The reporter also rejects packages without a passing named test.
CGO_ENABLED=0 POLLYTOOL_WORKTREE_MEASURE=1 go test -count=1 -run '^(TestRuntimeStorageScale|TestRuntimeStorageMCP)' -json "${packages[@]}" | python3 -c '
import json
import sys

prefix = "github.com/alexschlessinger/pollytool/"
required = {prefix + name for name in ("internal/runtimemetrics", "internal/runtimescale", "sessions", "swarm", "llm", "workflow", "tools", "worktree")}
passed = set()
failed = False
for line in sys.stdin:
    event = json.loads(line)
    if event.get("Output"):
        print(event["Output"], end="", flush=True)
    test = event.get("Test", "")
    if event.get("Action") == "fail":
        failed = True
    if event.get("Action") == "pass" and test.startswith("TestRuntimeStorageScale") and "/" not in test:
        passed.add(event.get("Package"))
missing = required - passed
if missing:
    print("Missing passing named TestRuntimeStorageScale tests: " + ", ".join(sorted(missing)), file=sys.stderr)
sys.exit(1 if failed or missing else 0)
'
