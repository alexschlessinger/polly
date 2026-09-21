#!/usr/bin/env bash
# driver.sh — capture the polly TUI off-screen.
#
# hshot builds ./polly and plays a scenario through polly's own off-screen
# renderer (`polly --shot-script`, documented in docs/CLI.md), leaving the PNGs
# the scenario asks for in the shot directory. No terminal, pty host, window or
# screen-recording permission is involved. Runs use an isolated HOME so the
# user's real ~/.pollytool is never touched, and stay in the repo: polly's
# sandbox needs git on PATH and refuses to start on the real $HOME.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUTDIR="${POLLY_SHOT_DIR:-/tmp/polly-shots}"
# Must NOT live under /tmp: polly's sandbox refuses a gitconfig inside a
# writable sandbox path.
SANDBOX_HOME="${POLLY_SHOT_HOME:-$HOME/.cache/polly-tui-home}"

die() { echo "driver.sh: $*" >&2; exit 1; }

build() { (cd "$REPO" && go build -o polly ./cmd/polly/); }

prep() { mkdir -p "$OUTDIR" "$SANDBOX_HOME"; }

# hshot <scenario> [WxH] — play a scenario and print each PNG path it writes.
# The scenario names its own outputs, usually through $POLLY_SHOT_DIR (which
# points at $OUTDIR here). Provider keys are inherited, so a scenario that types
# a prompt makes a real API call — unless POLLY_SHOT_FIXTURE names a fixture,
# which seeds the store and plays the model's turns itself.
hshot() {
  prep; build
  local scenario="${1:-}" size="${2:-}"
  [[ -n "$scenario" ]] || die "usage: driver.sh hshot <scenario-file> [WxH]"
  [[ -f "$scenario" ]] || die "no scenario file: $scenario"
  local args=(--shot-script "$scenario")
  [[ -z "$size" ]] || args+=(--shot-size "$size")
  [[ -z "${POLLY_SHOT_FIXTURE:-}" ]] || args+=(--shot-fixture "$POLLY_SHOT_FIXTURE")
  HOME="$SANDBOX_HOME" POLLY_SHOT_DIR="$OUTDIR" "$REPO/polly" "${args[@]}" < /dev/null
}

case "${1:-}" in
  hshot|build) cmd="$1"; shift; "$cmd" "$@" ;;
  *) cat >&2 <<'EOF'
usage: driver.sh hshot <scenario-file> [WxH]
  Play a scenario off-screen and print each PNG path it writes.
  Scenarios and gotchas: .agents/skills/polly-tui/SKILL.md
EOF
    exit 1 ;;
esac