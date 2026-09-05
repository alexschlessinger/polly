#!/usr/bin/env bash
# driver.sh — drive the polly TUI and capture its screen for agent review.
#
# Two backends:
#   tmux    - headless text-grid capture. Fast, no GUI, but polly disables
#             image protocols under tmux (terminal_images.go), so images
#             render as caption fallbacks.
#   wezterm - real terminal window. WezTerm renders polly's kitty-graphics
#             images and `wezterm cli` drives input over IPC, so pixel
#             screenshots (screencapture) show real image output.
#
# All runs use an isolated $HOME under the output dir so they never touch
# ~/.pollytool. Screenshots and captures land in $POLLY_SHOT_DIR
# (default /tmp/polly-shots).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SESSION="${POLLY_SHOT_SESSION:-pollyshot}"
COLS="${POLLY_SHOT_COLS:-120}"
ROWS="${POLLY_SHOT_ROWS:-40}"
OUTDIR="${POLLY_SHOT_DIR:-/tmp/polly-shots}"
# Isolated HOME so runs never touch ~/.pollytool. Must NOT live under /tmp:
# polly's sandbox refuses a gitconfig inside a writable sandbox path.
SANDBOX_HOME="${POLLY_SHOT_HOME:-$HOME/.cache/polly-tui-home}"
# Explicit PATH for spawned polly: `open`-launched WezTerm only has the
# bare launchd PATH, and the sandbox preset needs git on PATH at startup.
SPAWN_PATH="/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
WEZ="/Applications/WezTerm.app/Contents/MacOS/wezterm"
STATE="$OUTDIR/state"

die() { echo "driver.sh: $*" >&2; exit 1; }

build() { (cd "$REPO" && go build -o polly ./cmd/polly/); }

prep() { mkdir -p "$OUTDIR" "$SANDBOX_HOME"; }

# ---------- tmux backend ----------

start() {
  prep; build
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  local cmd="env HOME=$(printf %q "$SANDBOX_HOME") PATH=$(printf %q "$SPAWN_PATH") $(printf %q "$REPO/polly")"
  local arg; for arg in "$@"; do cmd+=" $(printf %q "$arg")"; done
  tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" "$cmd"
  wait_for '>' 15 || die "polly prompt did not appear"
  echo "started tmux session '$SESSION' (${COLS}x${ROWS})"
}

keys() { tmux send-keys -t "$SESSION" "$@"; }          # tmux key names: Enter, C-c, Down...
type_text() { tmux send-keys -t "$SESSION" -l "$*"; }  # literal text

wait_for() {  # wait_for <grep-pattern> [timeout-sec]
  local pat="$1" deadline=$((SECONDS + ${2:-10}))
  while ((SECONDS < deadline)); do
    tmux capture-pane -pt "$SESSION" | grep -q -- "$pat" && return 0
    sleep 0.3
  done
  return 1
}

settle() {  # wait until two consecutive captures are identical
  local deadline=$((SECONDS + ${1:-10})) prev="" cur
  while ((SECONDS < deadline)); do
    cur="$(tmux capture-pane -pt "$SESSION")"
    [[ -n "$cur" && "$cur" == "$prev" ]] && return 0
    prev="$cur"; sleep 0.4
  done
  return 0
}

text() { tmux capture-pane -pt "$SESSION"; }

shot() {  # shot [name] -> PNG of the ANSI capture (needs freeze), else .ansi
  prep
  local name="${1:-shot-$(date +%H%M%S)}"
  tmux capture-pane -pet "$SESSION" > "$OUTDIR/$name.ansi"
  if command -v freeze >/dev/null; then
    freeze --language ansi "$OUTDIR/$name.ansi" -o "$OUTDIR/$name.png" >/dev/null
    echo "$OUTDIR/$name.png"
  else
    echo "$OUTDIR/$name.ansi (install charmbracelet/tap/freeze for PNG)"
  fi
}

stop() { tmux kill-session -t "$SESSION" 2>/dev/null || true; echo stopped; }

# ---------- wezterm backend (real pixels, images work) ----------

wstart() {
  prep; build
  [[ -x "$WEZ" ]] || die "WezTerm not installed (brew install --cask wezterm)"
  # Launch via `open` so the GUI is parented to launchd and survives the
  # (short-lived) shell that ran this script.
  local sockdir="$HOME/.local/share/wezterm" s
  mkdir -p "$sockdir"
  for s in "$sockdir"/gui-sock-*; do          # reap sockets of dead GUIs
    [[ -S "$s" ]] && ! kill -0 "${s##*-}" 2>/dev/null && rm -f "$s"
  done
  local before=" $(ls "$sockdir" 2>/dev/null | grep '^gui-sock-' | tr '\n' ' ') "
  # `open`-launched WezTerm has no shell env, so provider keys would be
  # missing and live-response shots impossible. Pass selected vars via a
  # chmod-600 file (not the command line, which `ps` would expose).
  local envfile="$SANDBOX_HOME/env.sh" v
  : > "$envfile"; chmod 600 "$envfile"
  for v in ${POLLY_SHOT_ENV:-POLLYTOOL_ANTHROPICKEY POLLYTOOL_OPENAIKEY POLLYTOOL_GEMINIKEY POLLYTOOL_OPENROUTERKEY POLLYTOOL_DEEPSEEKKEY POLLYTOOL_HUGGINGFACEKEY POLLYTOOL_OLLAMAKEY POLLYTOOL_MODEL POLLYTOOL_BASEURL}; do
    [[ -n "${!v:-}" ]] && printf 'export %s=%q\n' "$v" "${!v}" >> "$envfile"
  done
  # Wrap in sh so a startup failure holds the window open with the error
  # text (readable via wtext) instead of instantly closing the GUI.
  local cmdstr="HOME=$(printf %q "$SANDBOX_HOME") PATH=$(printf %q "$SPAWN_PATH") $(printf %q "$REPO/polly")"
  local arg; for arg in "$@"; do cmdstr+=" $(printf %q "$arg")"; done
  open -na WezTerm.app --args --config enable_tab_bar=false \
    --config initial_cols="$COLS" --config initial_rows="$ROWS" \
    --config 'exit_behavior="Close"' \
    start --always-new-process --cwd "$REPO" -- \
    /bin/sh -c ". $(printf %q "$envfile") 2>/dev/null || true; $cmdstr; ec=\$?; [ \$ec -eq 0 ] || { echo POLLY_EXIT=\$ec; sleep 300; }"
  local deadline=$((SECONDS + 30)) sock=""
  while ((SECONDS < deadline)) && [[ -z "$sock" ]]; do
    for s in "$sockdir"/gui-sock-*; do
      [[ -S "$s" ]] || continue
      [[ "$before" == *" ${s##*/} "* ]] && continue
      kill -0 "${s##*-}" 2>/dev/null && sock="$s"
    done
    [[ -z "$sock" ]] && sleep 0.5
  done
  [[ -n "$sock" ]] || die "wezterm gui socket did not appear"
  echo "${sock##*-}" > "$STATE.wezpid"
  echo "$sock" > "$STATE.sock"
  local pane=""
  deadline=$((SECONDS + 15))
  while ((SECONDS < deadline)); do
    pane="$(wcli list --format json 2>/dev/null |
      /usr/bin/python3 -c 'import json,sys; p=json.load(sys.stdin); print(p[-1]["pane_id"] if p else "")' 2>/dev/null || true)"
    [[ -n "$pane" ]] && break
    sleep 0.5
  done
  [[ -n "$pane" ]] || die "wezterm pane did not appear"
  echo "$pane" > "$STATE.pane"
  wwait '>' 15 || die "polly prompt did not appear"
  echo "started wezterm pane $pane (${COLS}x${ROWS})"
}

# Pin to this GUI's socket; --no-auto-start stops the cli from spawning a
# headless mux server (which then answers with the wrong panes).
wcli() { WEZTERM_UNIX_SOCKET="$(cat "$STATE.sock")" "$WEZ" cli --no-auto-start "$@"; }

wpane() { cat "$STATE.pane" 2>/dev/null || die "no wezterm session (run wstart)"; }

wtype() { wcli send-text --no-paste --pane-id "$(wpane)" "$*"; }

wkey() {  # wkey enter|esc|up|down|left|right|tab|bs|c-<x>
  local seq
  case "$1" in
    enter) seq=$'\r' ;; esc) seq=$'\x1b' ;; tab) seq=$'\t' ;; bs) seq=$'\x7f' ;;
    up) seq=$'\x1b[A' ;; down) seq=$'\x1b[B' ;; right) seq=$'\x1b[C' ;; left) seq=$'\x1b[D' ;;
    c-?) printf -v seq '%b' "\\x$(printf %02x "$(( $(printf %d "'${1#c-}") & 31 ))")" ;;
    *) die "unknown key: $1" ;;
  esac
  wcli send-text --no-paste --pane-id "$(wpane)" "$seq"
}

wtext() { wcli get-text --pane-id "$(wpane)"; }

wwait() {  # wwait <grep-pattern> [timeout-sec]
  local pat="$1" deadline=$((SECONDS + ${2:-10}))
  while ((SECONDS < deadline)); do
    wtext | grep -q -- "$pat" && return 0
    sleep 0.3
  done
  return 1
}

wshot() {  # wshot [name] -> real pixel PNG of the WezTerm window
  prep
  local name="${1:-wshot-$(date +%H%M%S)}" id list
  # polly titles its window "polly · <session>", but macOS hides window
  # titles from processes without Screen Recording permission — fall
  # back to the largest WezTerm window (aux windows are small).
  list="$(GetWindowID WezTerm --list 2>/dev/null)"
  id="$(printf '%s\n' "$list" | grep '^"polly' | head -1 | sed 's/.*id=//')"
  [[ -n "$id" ]] || id="$(printf '%s\n' "$list" |
    sed -nE 's/.*size=([0-9]+)x([0-9]+) id=([0-9]+).*/\1 \2 \3/p' |
    awk '{a=$1*$2; if(a>b){b=a;i=$3}} END{print i}')"
  [[ -n "$id" ]] || die "no wezterm window found (GetWindowID WezTerm --list)"
  screencapture -o -x -l "$id" "$OUTDIR/$name.png" ||
    die "screencapture failed — grant Screen Recording to your terminal in System Settings > Privacy"
  echo "$OUTDIR/$name.png"
}

wstop() {
  if [[ -f "$STATE.wezpid" ]]; then kill "$(cat "$STATE.wezpid")" 2>/dev/null || true; rm -f "$STATE.wezpid"; fi
  rm -f "$STATE.pane" "$STATE.sock"
  echo stopped
}

# ---------- dispatch ----------

case "${1:-}" in
  start|keys|wait_for|settle|text|shot|stop|wstart|wtype|wkey|wtext|wwait|wshot|wstop|build) cmd="$1"; shift; "$cmd" "$@" ;;
  type) shift; type_text "$@" ;;
  wait) shift; wait_for "$@" ;;
  *) cat >&2 <<'EOF'
usage: driver.sh <command> [args]
  tmux (headless text; images become captions):
    start [polly-args]   launch TUI in tmux, isolated HOME
    type <text>          send literal text
    keys <keys...>       tmux key names (Enter, C-c, Down, ...)
    wait <pat> [sec]     poll screen for pattern
    settle [sec]         wait for screen to stop changing
    text                 print screen as plain text
    shot [name]          ANSI capture -> PNG via freeze
    stop                 kill session
  wezterm (real window; kitty-graphics images render):
    wstart [polly-args]  launch TUI in a WezTerm window
    wtype <text>         send text over IPC
    wkey <name>          enter|esc|up|down|left|right|tab|bs|c-<x>
    wtext                print pane text
    wwait <pat> [sec]    poll pane text for pattern
    wshot [name]         pixel screenshot of the window
    wstop                close the window
EOF
    exit 1 ;;
esac
