#!/usr/bin/env bash
# fanout.sh <tasks-dir>
#
# Launch one sub-agent per file in <tasks-dir>, in parallel. The file name
# (with any .txt extension stripped) is the agent id; the contents are the
# prompt, streamed to run-agent.sh over stdin so huge task files work (no
# ARG_MAX limit). A stripped id that would be empty or collide with another
# task's id keeps the full file name instead.
# Writes under the run directory POLLY_AGENT_DIR (default ${TMPDIR:-/tmp}/polly-agent).
# Starts a fresh status set, runs all agents, waits, then prints status.tsv:
#   <id> <TAB> <exit code> <TAB> ok|fail
# An agent that died before recording a row (e.g. its launch failed) still gets
# a fail row. Exits 0 when every agent succeeded, 1 when any failed, 64 on
# usage errors. Per-agent output is in <run-dir>/out/<id>.txt and err/<id>.log.
set -uo pipefail

if [ $# -lt 1 ]; then
  echo "usage: fanout.sh <tasks-dir>" >&2
  exit 64
fi

tasks_dir=$1
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# Pin + export the run dir so every run-agent.sh child resolves the same location.
export POLLY_AGENT_DIR=${POLLY_AGENT_DIR:-${TMPDIR:-/tmp}/polly-agent}
mkdir -p "$POLLY_AGENT_DIR/status"
rm -f "$POLLY_AGENT_DIR/status/"*.tsv "$POLLY_AGENT_DIR/status.tsv"   # fresh batch

ids=
for task in "$tasks_dir"/*; do
  [ -f "$task" ] || continue        # any regular file is a task (.txt optional)
  base=$(basename "$task")
  id=${base%.txt}
  case ",$ids," in
  *",$id,"*) id=$base ;;            # "report" and "report.txt" must not share a row
  esac
  [ -n "$id" ] || id=$base          # a file named exactly ".txt"
  ids=${ids:+$ids,}$id
  "$here/run-agent.sh" "$id" - < "$task" &
done
if [ -z "$ids" ]; then
  echo "no task files in $tasks_dir" >&2
  exit 64
fi
wait

# An agent whose launch died before run-agent.sh could record anything would
# otherwise vanish from the batch; give it an explicit fail row.
IFS=,
for id in $ids; do
  [ -f "$POLLY_AGENT_DIR/status/$id.tsv" ] ||
    printf '%s\t%s\t%s\n' "$id" 1 fail > "$POLLY_AGENT_DIR/status/$id.tsv"
done
unset IFS

sort "$POLLY_AGENT_DIR/status/"*.tsv > "$POLLY_AGENT_DIR/status.tsv"
echo "results in $POLLY_AGENT_DIR (out/<id>.txt, err/<id>.log)" >&2
cat "$POLLY_AGENT_DIR/status.tsv"
! grep -q "	fail$" "$POLLY_AGENT_DIR/status.tsv"
