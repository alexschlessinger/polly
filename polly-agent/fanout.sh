#!/usr/bin/env bash
# fanout.sh <tasks-dir>
#
# Launch one sub-agent per file in <tasks-dir>, in parallel. The file name
# (with any .txt extension stripped) is the agent id; the contents are the prompt.
# Writes under the run directory POLLY_AGENT_DIR (default ${TMPDIR:-/tmp}/polly-agent).
# Starts a fresh status.tsv, runs all agents, waits, then prints status.tsv:
#   <id> <TAB> <exit code> <TAB> ok|fail
# Per-agent output is in <run-dir>/out/<id>.txt and <run-dir>/err/<id>.log.
set -uo pipefail

if [ $# -lt 1 ]; then
  echo "usage: fanout.sh <tasks-dir>" >&2
  exit 2
fi

tasks_dir=$1
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Pin + export the run dir so every run-agent.sh child resolves the same location.
export POLLY_AGENT_DIR=${POLLY_AGENT_DIR:-${TMPDIR:-/tmp}/polly-agent}
mkdir -p "$POLLY_AGENT_DIR"
: > "$POLLY_AGENT_DIR/status.tsv"     # fresh batch; children upsert one row each under a lock

found=0
for task in "$tasks_dir"/*; do
  [ -f "$task" ] || continue        # any regular file is a task (.txt optional)
  found=1
  id=$(basename "$task"); id=${id%.txt}
  "$here/run-agent.sh" "$id" "$(cat "$task")" &
done
if [ "$found" -eq 0 ]; then
  echo "no task files in $tasks_dir" >&2
  exit 2
fi
wait

sort -o "$POLLY_AGENT_DIR/status.tsv" "$POLLY_AGENT_DIR/status.tsv" 2>/dev/null || true
echo "results in $POLLY_AGENT_DIR (out/<id>.txt, err/<id>.log)" >&2
cat "$POLLY_AGENT_DIR/status.tsv"
