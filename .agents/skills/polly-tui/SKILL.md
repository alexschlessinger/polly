---
name: polly-tui
description: Drive the polly TUI and capture its screen for review — headless tmux text/PNG captures, or WezTerm pixel screenshots when kitty-graphics image rendering must be verified
---

# Driving and screenshotting the polly TUI

All commands go through `driver.sh` in this directory. It builds
`./polly` from source on every `start`/`wstart`, runs it with an
isolated `HOME` (`~/.cache/polly-tui-home`) so the user's real
`~/.pollytool` sessions are never touched, and drops captures in
`/tmp/polly-shots/` (override with `POLLY_SHOT_DIR`; size with
`POLLY_SHOT_COLS`/`POLLY_SHOT_ROWS`, default 120x40).

```bash
D=.agents/skills/polly-tui/driver.sh   # run from repo root
```

## Choosing a backend

**tmux (default)** — headless, fast, no GUI flashes on the user's
screen. polly deliberately disables image protocols under tmux
(`cmd/polly/terminal_images.go`), so attachments render as text
captions. Use for layout, content, keybinding, color, and slash-command
work — everything except real image rendering.

**wezterm** — opens a real WezTerm window (kitty-graphics capable;
polly auto-detects it) and screenshots actual pixels with
`screencapture`. The ONLY way to verify image/thumbnail rendering.
Requires the one-time macOS grant: System Settings → Privacy &
Security → Screen & System Audio Recording → enable the terminal app
hosting the agent (e.g. Ghostty). A window briefly appears on the
user's screen — that's expected.

## tmux backend

```bash
$D start                 # launch (extra args go to polly)
$D type "/help"          # literal text into the input
$D keys Enter            # tmux key names: Enter, C-c, Down, PPage...
$D wait "keys:" 10       # poll screen for a pattern (grep)
$D settle 5              # wait until the screen stops changing
$D text                  # plain-text screen dump (cheapest check)
$D shot name             # ANSI capture -> /tmp/polly-shots/name.png (freeze)
$D stop                  # kill the session
```

Read the PNG with the Read tool to see rendered colors/layout. `text`
is cheaper when plain content is enough.

## wezterm backend (pixel shots, images render)

```bash
$D wstart                                  # opens a WezTerm window
$D wtype "/attach /abs/path/img.png"       # send text over IPC
$D wkey enter                              # enter|esc|up|down|left|right|tab|bs|c-<x>
$D wwait "attached" 10                     # poll pane text
$D wtext                                   # pane text dump
$D wshot name                              # REAL pixel PNG of the window
$D wstop                                   # close the window
```

Provider keys (`POLLYTOOL_*KEY`) are passed through a chmod-600 env
file, so live streamed responses work too — but remember a submitted
prompt makes a real API call; slash commands and /attach are free.

## Key reference (from /help)

| Key | Action |
|---|---|
| Enter | send message |
| Ctrl-J | newline (multi-line input) |
| / then Tab | list/complete slash commands |
| Ctrl-C / Esc | interrupt turn (Ctrl-C twice to quit) |
| Up / Down | move line; recall history at top/bottom |
| Ctrl-R | reverse-search history |
| Ctrl-V | attach image from clipboard |
| PgUp / PgDn | scroll transcript |
| Ctrl-L | clear display |

Slash commands (all local, no API call): /attach /clear /context /exit
/get /help /queue /rename /reset /retry /skills /tools

Quick protocol check: the startup splash doubles as a diagnostic. When
the image protocol is active you see the dotted macaw image splash
(`splash_image.go`, drawn through the same placement pipeline as
thumbnails); when it's off (e.g. under tmux) you see the small
half-block text bird instead.

## Gotchas learned the hard way

- Never launch the WezTerm GUI directly from an agent shell — the
  process group is killed when the tool call ends. `wstart` uses
  `open -na` (parents to launchd) for this reason.
- A bare `wezterm cli` auto-spawns a headless mux server and answers
  from the wrong pane set. The driver pins `WEZTERM_UNIX_SOCKET` to
  its own GUI's socket and passes `--no-auto-start`.
- polly's sandbox needs `git` on PATH and refuses to start when
  cwd is the real `$HOME` (TCC blocks scanning `~/.Trash`) or when
  `HOME` is under `/tmp` (gitconfig inside a writable sandbox path).
  The driver pins cwd to the repo and HOME to `~/.cache/polly-tui-home`.
- If `wstart` says the prompt did not appear, run `$D wtext` — the
  window is held open with the startup error on screen.
