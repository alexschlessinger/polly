# Theme engine

Status: implemented — all 11 plan tasks integrated; parent verification recorded in `## Outcome` at the end of this file

## Problem

polly's appearance is hardcoded. Every color the TUI and the line frontend emit
resolves through seven semantic role names (`ok`, `err`, `run`, `accent`,
`active`, `muted`, `code`) registered in a single `init()` in
`cmd/polly/internal/style/style.go`, plus nine fixed true-color `polly-*` names
for the masthead bird. Each role maps to a terminal palette slot (XTerm 0–15),
so the terminal's own theme decides the RGB.

Two consequences hurt:

- **No customization.** A user who wants different accents, a calmer muted
  gray, or a restyled bird has to patch Go constants and rebuild. The model
  cannot restyle polly either, even though it can already change a session
  title through a tool.
- **No adaptation.** The role assignment assumes a dark background. On a light
  terminal, `code`/`ColorWhite` and `muted`/bright-black land badly, and there
  is no way to say "use these colors on light backgrounds".

Who feels it: users who live in polly all day and want it to match their
terminal, and users whose terminal palette makes some roles unreadable.

## Goals

1. A theme is a named set of colors for polly's fixed, semantic role
   vocabulary. Call sites (`style.Styled(text, "muted", "bold")`,
   `chromeColor("accent")`) do not change — only the resolver behind them.
2. Themes load from user-editable files under `~/.pollytool/themes/`, selected
   by `--theme` / `POLLYTOOL_THEME` / `~/.pollytool/config` (the existing
   three-tier flag → env → file precedence) with compiled-in presets as the
   built-in default.
3. Syntax highlighting becomes themable: chroma token categories get their own
   roles instead of borrowing semantic ones, and highlighting extends to the
   tool/bash output surfaces that render as plain `code` today.
4. A theme can declare light and dark variants; polly picks between them by
   config or by detection.
5. The model can author a theme: a `set_theme` tool applies one live, and a
   builtin skill teaches it how. Persisting to disk requires an explicit
   confirmation step.
6. Editing a theme file takes effect without restarting polly (hot reload).
7. The line frontend (`-p`, piped output) honors themes too, with truecolor SGR
   when the terminal advertises it and a 256-color fallback when it does not.

## Non-goals

- No theme registry, download, or importing foreign formats (base16, Ghostty,
  iTerm, VSCode themes).
- No colorscheme beyond foreground roles: no `background` field, no per-widget
  backgrounds. polly keeps painting `ui.ColorClear` and defers to the
  terminal's background.
- No theming of modifiers: bold/dim/italic stay at the call sites. A theme sets
  colors only.
- No theming surface for the fixed approval-border amber documented in
  `README.md` (the framed-inspector note), and none for image rendering.
- No per-project themes, no theme stacks, no theme inheritance.
- No OSC 11 background probe (see Design: detection).
- No `/theme save`; the model's `set_theme` tool and hand-editing are the
  persistence paths.
- No hot reload in the line frontend (it has no tick).

## Design

### Role vocabulary

The theme's namespace is the role name. Three groups, 23 roles total:

- **Semantic (7).** `ok`, `err`, `run`, `accent`, `active`, `muted`, `code` —
  unchanged names and meaning.
- **Token (7).** `syn-comment`, `syn-keyword`, `syn-string`, `syn-number`,
  `syn-func`, `syn-add`, `syn-del`. `markdown.chromaStyle` returns these
  instead of borrowing semantic roles — today a comment is literally `muted`
  and a string literally `ok`. Each token role falls back to the semantic role
  it uses now (`syn-comment`→`muted`, `syn-keyword`→`accent`,
  `syn-string`→`ok`, `syn-number`→`active`, `syn-func`→`code` bold,
  `syn-add`→`ok`, `syn-del`→`err`), so an unset token role renders exactly as
  today. `renderDiffLines` keeps its own `+`/`-`/`@@` tones but moves them onto
  `syn-add`/`syn-del`/`muted`.
- **Bird (9).** The existing `polly-green`, `polly-light`, `polly-wing`,
  `polly-crown`, `polly-beak`, `polly-mouth`, `polly-face`, `polly-eye`,
  `polly-foot` names become ordinary themable roles, still true-color by
  default.

### Color values

A role's value in a theme file is one of:

| Value | Meaning |
| --- | --- |
| `"#rrggbb"` (also `"#rgb"`) | true color |
| `"palette:N"`, `N` 0–255 | explicit ANSI slot |
| any name in gotui's `StyleParserColorMap` (`"green"`, `"darkred"`, …) | that parser color |
| `"inherit"` | drop styling; the terminal's default foreground |
| key omitted | polly's built-in mapping for that role (today's behavior) |

`"inherit"` resolves to `ui.ColorClear` (`tcell.ColorDefault`), which the
parser already understands as "no color" — `"clear"` is in gotui's own map.

### Theme files

`~/.pollytool/themes/<name>.json`:

```json
{
  "name": "solar-polly",
  "variant": "dark",
  "colors": { "accent": "#8ab4f8", "muted": "palette:8", "code": "inherit" },
  "light":  { "accent": "#0b5cad" },
  "dark":   { "accent": "#8ab4f8" }
}
```

- `colors` is the base layer. `light` and `dark` are optional override layers
  applied when the resolved variant is light or dark respectively. A theme
  with only `colors` renders identically in both variants.
- `variant` is the theme's own declaration (`"dark"` default). It is advisory:
  the resolved variant comes from config/detection, and the file's `variant` is
  used only to report mismatches.
- `name` defaults to the file's base name.
- Selection: an absolute or relative path containing `/` (or ending in `.json`)
  is read as a path; otherwise a user file `~/.pollytool/themes/<name>.json`
  wins and the builtin presets are the fallback. `default` is reserved: a user
  file of that name is ignored, because `default` is what every load failure
  falls back to.
- Compiled-in presets: `default` (today's ANSI-slot mapping, the built-in
  default and the fallback for every load failure), `dark`, `light` (pinned
  RGB).
- JSON, not YAML: no new behavior to explain, and `encoding/json` is stdlib.
- Unknown role names, unknown top-level keys, and unparseable color values are
  load errors. A load error prints a notice and falls back to `default`; it
  never prevents polly from starting.

### Selection and detection

- `--theme` / `POLLYTOOL_THEME` (default `default`).
- `--theme-variant` / `POLLYTOOL_THEME_VARIANT` — `auto` (default), `dark`,
  `light`.
- `auto` reads `COLORFGBG` (`fg;bg`, last field is the background index): light
  when the index is `7` or `>= 9`, dark otherwise (including `8`, bright
  black). Unset, unparseable, or `TERM`-less environments resolve to `dark`.
- No OSC 11 probe: polly's TUI hands the tty to tcell, tcell v3 has no
  background-color API, and querying before screen init costs a startup
  timeout and garbles output on terminals that do not answer.
- `/theme` lists builtin and user themes with the active one marked;
  `/theme <name>` switches for the session (no persistence); `/theme default`
  returns to the built-in. The switch is immediate, through the same reload
  path as the file watcher.

### Applying a theme

Two projections, one source of truth:

- **Markup/text surfaces.** All 23 role names are (re)registered into
  `ui.StyleParserColorMap`, which `style.Styled` output and gotui's parser
  already read. Unset token roles are registered with their fallback's
  resolved color, so the table is always complete and never depends on
  gotui's unknown-name path.
- **Direct cell painting.** `chromeColor(name)` already reads the same map, so
  the orbit frame and scrollbar follow with no change.

The role table, value syntax, variant overlays, and the epoch live in
`cmd/polly/internal/style` (`theme.go`); `markdown` imports only the `syn-*`
*names*. File discovery, flag/env/config wiring, `COLORFGBG`, the reload
watcher, cache invalidation, and the tool live in `cmd/polly` (`theme.go`,
`repl_theme.go`).

### Hot reload and the style epoch

- The theme file (and the selection in `~/.pollytool/config`) is stat-polled
  (mtime + size) with the watcher's own ~1 s throttle. There is no ready-made
  tick to hang it on: the TUI's 50 ms ticker only repaints while the turn is
  busy (`repl_loop.go` calls `render()` only when `needsTick()`), so an idle
  TUI would never pick up an edit. A reload must force its own repaint. The
  config selection must be re-read through `readUserConfig`, not
  `userConfigValue`, which caches the parsed file for the process lifetime.
- On change, `style` bumps a process-wide **style epoch**. Everything that
  caches *resolved* cells or styles must invalidate on an epoch change — a
  cache holding role *names* is safe, a cache holding `ui.Cell`/`ui.Style`/
  `ui.Color` is not:
  - `transcriptParagraph.Rows`/`OverlayBottom` and the row caches in
    `repl_transcript.go`/`repl_model.go`: the epoch must enter the reuse
    predicate, because `transcriptVisualCache.invalidate()` alone only clears
    `valid` and the row rebuild reuses any block whose key/text/cells are
    unchanged.
  - `assistantTypewriter.cells`, which re-parses only when the source changes,
    so a mid-stream theme change leaves the visible prefix stale.
  - `markdown.CodeCache` (already resettable — the line frontend resets it the
    same way), the cached image spans in `repl_images.go`, and cached child
    views in `repl_child_cache.go`/`repl_main_view_cache.go`.
  - `frameOrbit.cells` (its `base` cells hold resolved colors).
  - Two `sync.OnceValue` caches found during research:
    `markdown/table.go`'s `paletteColorNames` (a stale inverse map silently
    re-encodes a color under a name that now means something else) and
    `repl_agents.go`'s `agentLinkStyle` (used for exact-equality click hit
    testing, so agent links stop being clickable after any theme change).
- Applying a theme writes the process-global `ui.StyleParserColorMap`, which
  gotui reads lock-free on the draw path while other goroutines parse cells.
  Every apply therefore happens on the event loop; the `set_theme` tool posts
  to it rather than writing the map from its own goroutine.

### `set_theme` tool

Registered in-process the way `set_session_title` is
(`cmd/polly/session_title.go:27`), only when a live frontend can repaint.

- Args: `name` (string, required), `colors` (object, optional), `light`/`dark`
  (objects, optional), `persist` (bool, default false), `confirm` (bool,
  default false), `overwrite` (bool, default false).
- Applying without `persist` is immediate and session-only.
- `persist: true` without `confirm: true` writes nothing and returns
  `{"status": "confirmation_required", "path": "...", "theme": {...}}`, so the
  model must surface the exact file and colors to the user and call again with
  `confirm: true`.
- `persist` + `confirm` writes `~/.pollytool/themes/<name>.json` and sets
  `POLLYTOOL_THEME=<name>` in `~/.pollytool/config` via the existing writer
  (`cmd/polly/userconfig.go:149`), then reloads immediately.
- Replacing an existing user theme file requires `overwrite: true`.
- Errors are `*tools.ToolError` with codes `UNKNOWN_ROLE`, `INVALID_COLOR`,
  `INVALID_THEME`, `THEME_EXISTS`, `THEME_WRITE_FAILED`.
- The write happens in-process, like polly's own config writer. The builtin
  file tools would be sandbox-denied because `~/.pollytool` is outside the
  workspace; the model probing that path from bash stays denied, and the tool
  is the only sanctioned writer.

### Skill

`skills/builtin/theme-designer/SKILL.md`, embedded like `feature-workflow` (see
`skills/builtin.go`). It teaches the model to interview the user for intent
(light or dark, match the terminal or pin RGB), pick from polly's role
vocabulary, apply with `set_theme`, and only persist after the user agrees.
It must not reference this repository.

### Line frontend

`outputCapabilities` gains true-color and 256-color capability fields derived
from `COLORTERM` (`truecolor`/`24bit`) and `TERM` (`*-256color`,
`*-truecolor`). The SGR writer grows: palette 0–15 keeps the existing
`30–37`/`90–97` codes, palette 16–255 and non-truecolor RGB emit `38;5;N`,
truecolor RGB emits `38;2;r;g;b`. Today `ansiPaletteCode`
(`cmd/polly/line_markdown.go:126`) only maps 0–15 and silently drops *all*
styling for anything else — a themed RGB role would lose its color in `-p`
output. Rejected alternative: dropping unsupported colors (that is the current
bug); nearest-XTerm-256 is deterministic and testable.

## Edge cases

- **Missing/broken theme file.** Notice + `default`; polly always starts.
- **Typo'd role name.** Load error, never a silently ignored key.
- **`TERM=dumb`, `NO_COLOR`, piped output.** Unchanged: no styling, and no
  color sequences leak into non-ANSI surfaces. (There is no `--no-color` flag
  today, only the variable and `TERM=dumb`.)
- **Terminal without truecolor.** RGB roles degrade to nearest 256 in line
  output; the TUI keeps whatever tcell negotiates, as today.
- **Light variant on a light terminal with a dark-only theme.** No `light`
  layer exists, so `colors` applies unchanged, and polly reports the mismatch
  as a notice rather than guessing.
- **`COLORFGBG` disagreeing with reality.** Accepted: the user can pin
  `--theme-variant`.
- **An unrecognized `--theme-variant` value.** Coerced to `auto` with the
  startup notice, matching "never prevent polly from starting"; no flag
  Validator.
- **Theme change mid-turn.** The watcher forces a repaint even when idle;
  streaming rows and the typewriter prefix recompute on the same epoch bump.
  Cached image spans must survive a theme change without repositioning.
- **Reload while the user edits a file.** A half-written file is a load error →
  notice + keep the previous theme; the next poll picks up the finished file.
- **`set_theme` during a turn.** Applied immediately, like `/theme`, posted to
  the event loop (never written to the parser map from the tool's goroutine).
- **`~/.pollytool/themes/` missing.** Created (0700) on first persist only.
- **Named builtin shadowing.** A user theme in the themes dir wins over a
  same-named preset; `default` is reserved and `/theme` shows which file won.
- **A persisted selection shadowed by the environment.** `POLLYTOOL_THEME`
  exported in the shell beats `~/.pollytool/config`, so `set_theme persist`
  warns when it detects that, the way the setup form does.
- **Bird roles.** Overridable and `"inherit"`-able: the bird is drawn from block
  glyphs, so inheriting the foreground renders it monochrome rather than
  invisible. `"inherit"` is rejected for `accent` and `muted`, where two
  existing heuristics depend on the resolved color being real and distinct (the
  agent-link hit test compares cell styles for exact equality, and the wrap
  gutter detector compares the first cell's color to accent/muted).
- **A theme value naming one of polly's own roles** (e.g. `"accent": "muted"`)
  is `INVALID_COLOR`: values resolve against a snapshot of the parser map taken
  before polly registers its roles, so resolution is deterministic and
  cycle-free.
- **`palette:N` out of range** is a load error; `palette:0` is the terminal's
  black slot and is distinct from an omitted key (polly's built-in mapping).
- **Concurrency.** The parser map and epoch are process-wide, and gotui reads
  the map lock-free on the draw path; apply, reload, and repaint all happen on
  the event loop.

## Acceptance criteria

1. `--theme`, `POLLYTOOL_THEME`, and `~/.pollytool/config` select a theme with
   the existing flag → env → file → built-in precedence, and `polly --help`
   lists both new flags with their variables.
2. A user themes dir file overriding one role (say `accent`) changes only the
   accent-colored text in the TUI; an omitted role keeps today's appearance; an
   `"inherit"` role renders in the terminal's default foreground.
3. `syn-*` roles are themable independently of semantic roles; under the
   default theme, fenced-code and diff output resolves to exactly today's
   colors (the markup names change to `syn-*`, so the existing assertions that
   pin `fg:accent`/`fg:muted`/`fg:ok`/`fg:err` must be updated by design, not
   weakened), and tool output stays byte-identical until criterion 4's lexer
   selection engages.
4. Syntax highlighting appears in bash tool output and generic tool output,
   with the same token→role mapping as fences.
5. Light/dark: a theme with a `light` layer renders those colors when
   `COLORFGBG` says light and `--theme-variant auto` is in effect; a missing
   `light` layer renders `colors` in both.
6. Editing the active theme file while the TUI runs applies on the next tick
   without a restart and without stale colors anywhere (transcript, orbit
   frame, scrollbar, bird, code blocks).
7. `set_theme` without `persist` changes the running session only; with
   `persist` but no `confirm` it writes nothing and reports the target path;
   with both it writes `~/.pollytool/themes/<name>.json`, sets
   `POLLYTOOL_THEME`, requires `overwrite` to replace an existing file, and
   survives a restart.
8. The `theme` builtin skill exists, is listed, activates, and references no
   repository paths.
9. Line output: a true-color role emits `38;2;r;g;b` when `COLORTERM` says
   truecolor, `38;5;N` otherwise, and `/theme`-independent `-p` output with a
   palette-only theme is unchanged.
10. `CGO_ENABLED=0 go build ./...`, `go vet ./...`, `gofmt -l .`, and
    `go test ./...` pass; no new module dependency; `tmux`-driven TUI
    screenshots via the `polly-tui` skill for before/after.

## Open questions

1. **Confirmation shape for `persist`.** The spec uses a two-call protocol
   (`confirm: true`), which the model can satisfy on its own behalf after
   telling the user what it will write. The alternative is a dedicated TUI
   modal (a real keystroke, reusing the pending-approval UI in
   `cmd/polly/repl_approval.go`) — more honest, more machinery. Recommendation:
   two-call protocol for v1.
2. **`syn-func` bold.** Today function/class names are `code` + bold. Modifiers
   are out of theme scope, so `syn-func` keeps bold. If a user wants function
   names unbold, that needs a modifier field later.
3. **Preset set.** `default`, `dark`, `light` are proposed. A `high-contrast`
   preset is cheap but unrequested.

---

## Plan

Phase-2 output: workflow `feature-research` run `dc2738235ae003d8ca91650bde8784f7`
(6 lens researchers + synthesizer, read-only, baseline `a6a393b3`); the
machine-readable plan object is retained in that workflow's saved output
(artifact `sha256:d6ac47feea934f9d12392897336d7e869ace5345786a8834beb3864be42d958a`).
The parent corrected the spec above where research contradicted it: the
builtin-first/shadowing contradiction, the non-existent `--no-color` flag, the
`~1 s tick` that does not exist, the invalidation list (epoch must enter the
transcript reuse predicate; two `sync.OnceValue` caches were missing), the
`inherit`-on-bird claim, and the scope of acceptance 3's byte-identical claim.

### Summary

Implementation plan for theme-engine, synthesised from the approved spec
(docs/features/theme-engine.md, read in full) plus the five research lenses,
with every decomposition-critical claim re-verified in the assigned copy at
a6a393b3. Verified directly: style.go's init registers 16 names into the
process-global ui.StyleParserColorMap; repl_loop.go uses a 50ms ticker whose
arm repaints only when needsTick(); transcriptRows returns c.rows on `c.valid
&& fits` and its per-block reuse predicate ignores `valid`, so invalidate()
alone cannot re-resolve colors; assistantTypewriter.cells re-parses only on
source change; markdown/table.go's paletteColorNames is sync.OnceValue over
StyleParserColorMap; repl_agents.go's agentLinkStyle is sync.OnceValue and
agentDetail hit-tests cells by exact style equality; subagent.ChildRegistry
already denies set_session_title and friends;
session_title.go/conversation.go is the tool-registration pattern;
output_capabilities.go has an injected-getenv seam; line_markdown.go's
ansiPaletteCode maps only 0-15 and returns false (silent styling loss) for
everything else; outputConfigFlags + envDefault is the flag wiring and the
source of `--help` variable listing; userConfigValue caches
~/.pollytool/config for the process lifetime; skills/builtin.go embeds the
skill tree. 11 tasks in 5 dependency waves, file-disjoint per wave, with the
cross-task Go signatures pinned in the briefs.

### Checks

Implementation must pass, in this order (fast to slow):

1. `gofmt -l .` prints nothing — in this sandbox scope it with
   `gofmt -l $(git ls-files '*.go')`, because the repo-local module caches
   (`.gopath/`, `.tmp-gopath/`) otherwise report vendored files.
2. `CGO_ENABLED=0 go build ./...`
3. `go vet ./...`
4. `CGO_ENABLED=0 go test -count=1 ./cmd/polly/internal/style/... ./cmd/polly/internal/markdown/...` (~0.5 s)
5. `CGO_ENABLED=0 go test -count=1 ./cmd/polly/...` (~45 s with the caveat below)
6. `CGO_ENABLED=0 go test -count=1 ./...` (~2 min)
7. `POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 CGO_ENABLED=0 go test -count=1 ./...`
8. `python3 -B -m unittest discover -s .github/local-ci -p 'test_*.py'`
9. `.github/ci.sh test` and `.github/ci.sh cross`
10. `CGO_ENABLED=1 go test -race ./cmd/polly ./cmd/polly/internal/style ./cmd/polly/internal/markdown` — load-bearing, the parser map and epoch are process-wide.
11. TUI before/after captures via `.agents/skills/polly-tui/driver.sh`.

Environment note for these runs: the sandbox denies the default module cache
and the real home, so prefix with
`GOMODCACHE=$PWD/.gopath/pkg/mod GOCACHE=$TMPDIR/polly-gocache GOFLAGS=-mod=mod GOPROXY=off`
and a writable `HOME` (`cmd/polly`'s TestMain creates its test home under the
real home). Parent-observed baseline: build, vet, style+markdown tests and the
33 local-ci Python tests are green; `./cmd/polly` has **two pre-existing
sandbox-limited failures** unrelated to this feature —
`TestOneShotTerminalContract` fails with `OSError: out of pty devices` and
`TestCLISignalExitStatusAndStderr` fails on `open /Users/alex: operation not
permitted` while loading repository instructions. Both must be re-checked, and
neither counts as a regression.

### Waves

| Wave | Tasks (parallel, file-disjoint) |
| --- | --- |
| 1 | `style-theme-core`, `tool-output-highlighting`, `line-color-capabilities`, `theme-builtin-skill` |
| 2 | `markdown-token-roles`, `theme-selection-and-flags`, `theme-epoch-invalidation` |
| 3 | `theme-apply-reload-command` |
| 4 | `set-theme-tool` |
| 5 | `readme-themes-docs` |
| 6 | `acceptance-verification` |

### Tasks

#### `style-theme-core` — Theme core in cmd/polly/internal/style: 23 roles, value syntax, presets, variants, epoch

- dependsOn: none
- paths: `cmd/polly/internal/style/theme.go`, `cmd/polly/internal/style/style.go`, `cmd/polly/internal/style/theme_test.go`
- acceptance:
  - All 23 role names are present in ui.StyleParserColorMap after Apply(DefaultTheme(), "dark"); no role resolves to an unknown name.
  - With DefaultTheme active each unset syn-* role resolves to exactly its fallback role's color (syn-comment==muted, syn-keyword==accent, syn-string==ok, syn-number==active, syn-add==ok, syn-del==err, syn-func==code).
  - ParseColorValue accepts `#8ab4f8`, `#rgb` expansion, `palette:0`, `palette:255`, `inherit` (== ui.ColorClear), and gotui names such as green/grey/darkred; it rejects `#gggggg`, `palette:256`, `palette:-1`, empty, and an unknown name.
  - ParseTheme rejects an unknown role name, an unknown top-level key, and a bad value with a wrapped error, and accepts colors-only, light/dark overlays, and a variant declaration.
  - Apply returns a strictly increasing epoch on every call and is safe under CGO_ENABLED=1 go test -race ./cmd/polly/internal/style.
  - gofmt -l . prints nothing; CGO_ENABLED=0 go build ./..., go vet ./..., and CGO_ENABLED=0 go test -count=1 ./cmd/polly/internal/style/... pass.
- brief: Build the resolver the whole feature rides on. Today every TUI color is a role NAME resolved through gotui's process-global `ui.StyleParserColorMap`: `cmd/polly/internal/style/style.go:33-52` registers 16 names in `init()` (7 semantic ok=ui.ColorGreen, err=ui.ColorRed, run=ui.ColorTeal, accent=ui.ColorBlue, active=ui.ColorYellow, muted=ui.ColorGrey, code=ui.ColorWhite; 9 `polly-*` bird RGB values held in package vars pollyGreen..pollyFoot just above). `style.Styled` emits `[text](fg:NAME)` and gotui's parser looks NAME up in that map, falling back to `tcell.GetColor`, which understands only W3C names and exactly-7-char `#rrggbb` and otherwise silently returns ColorDefault. So polly must resolve values itself and always register a complete table.

  Create `cmd/polly/internal/style/theme.go` with:
  - The 23 role names: semantic `ok, err, run, accent, active, muted, code`; token `syn-comment, syn-keyword, syn-string, syn-number, syn-func, syn-add, syn-del` with fallbacks `muted, accent, ok, active, code, ok, err`; bird `polly-green, polly-light, polly-wing, polly-crown, polly-beak, polly-mouth, polly-face, polly-eye, polly-foot`.
  - `type Theme struct { Name, Variant string; Colors, Light, Dark map[string]string }` and `DefaultTheme() Theme` reproducing today's mapping exactly (reuse the existing bird vars; do not duplicate RGB literals).
  - `ParseTheme(name string, data []byte) (Theme, error)` with stdlib encoding/json: unknown role name, unknown top-level key (only name/variant/colors/light/dark), or unparseable value is a wrapped error; `name` defaults to the supplied name.
  - `ParseColorValue(v string) (ui.Color, error)` and `(Theme) Validate() error`. Forms: `#rrggbb` and `#rgb` (expand to 6 digits yourself - tcell will NOT); `palette:N` for 0<=N<=255 -> `tcell.PaletteColor(N)` (tcell.PaletteColor has no bounds check, so reject out-of-range); `inherit` -> `ui.ColorClear`; a plain name resolved against a SNAPSHOT of `ui.StyleParserColorMap` captured at the top of init() BEFORE polly registers its own roles, so a value like "muted" cannot be self-referential. Document that a polly role name used as another role's value is INVALID_COLOR (deterministic, cycle-free).
  - `Apply(t Theme, variant string) uint64`: merge base + the light or dark overlay for the variant (a theme with only `colors` renders identically in both), derive each role's color, register ALL 23 names into `ui.StyleParserColorMap`, registering unset token roles with their fallback role's resolved color, then bump a mutex-guarded process-wide epoch and return it. `Epoch() uint64` reads it. Note in a comment that the resulting map is read lock-free by gotui on the draw path, so Apply is event-loop only.
  - Move init()'s registration onto this path so a no-theme start is byte-identical to today (same 16 names and values, plus the 7 syn-* names).

  Conventions: fmt.Errorf with %w; load errors are returned, never fatal. Stdlib testing only, table-driven with t.Run, t.Fatalf, no testify/mocks/goldens. `ui.StyleParserColorMap` is process-global and read by other packages, so tests must restore it (re-Apply(DefaultTheme(), "dark") in t.Cleanup). No new module dependency.

  Pinned interface other tasks call (do not rename): style.Theme, style.DefaultTheme(), style.ParseTheme(name, data), style.ParseColorValue(v), style.Apply(theme, variant) uint64, style.Epoch() uint64.

#### `markdown-token-roles` — Give syntax highlighting its own roles and make the table color-name map epoch-aware

- dependsOn: `style-theme-core`
- paths: `cmd/polly/internal/markdown/markdown.go`, `cmd/polly/internal/markdown/table.go`, `cmd/polly/internal/markdown/markdown_test.go`, `cmd/polly/internal/markdown/table_test.go`, `cmd/polly/repl_tool_changes.go`
- acceptance:
  - With the default theme a fenced Go/JSON block emits fg:syn-comment / fg:syn-keyword / fg:syn-string / fg:syn-number / fg:syn-func markup, and a diff emits fg:syn-add / fg:syn-del for +/- with hunk headers and context roles unchanged.
  - A test asserts each unset syn-* role resolves to exactly its fallback role's ui.Color under the default theme.
  - A regression test renders a wrapped table under theme A, applies theme B (bumping the epoch), renders again, and asserts the re-encoded roles resolve to B's colors - the ColorBlue-as-accent staleness repro no longer reproduces.
  - Default-theme table markup is byte-identical to the pre-change output (asserted on the emitted markup string).
  - HighlightCodeLines(code, "") still returns styledLines(code, "code", "") byte-for-byte.
  - gofmt -l . prints nothing; build, vet, and CGO_ENABLED=0 go test -count=1 ./cmd/polly/internal/markdown/... pass.
- brief: Two coupled changes in cmd/polly/internal/markdown plus the diff tones in cmd/polly.

  (a) Token roles. `chromaStyle` (cmd/polly/internal/markdown/markdown.go:626-645) currently borrows semantic roles: comment->muted, keyword->accent, LiteralString->ok, LiteralNumber->active, NameFunction/Class/Namespace->code+bold, GenericInserted->ok, GenericDeleted->err, default->code. Return the token roles instead: syn-comment, syn-keyword, syn-string, syn-number, syn-func (keep the bold modifier), syn-add, syn-del, default code. Do NOT change `HighlightCodeLines`' behaviour when lang == "": it must keep short-circuiting to `styledLines(code, "code", "")`, because the token roles' colors are registered in internal/style with their fallback values (task style-theme-core). `renderDiffLines` (cmd/polly/repl_tool_changes.go:218-252) keeps its own line classification but moves tones: `+`->syn-add, `-`->syn-del, `@@` and `\\`->muted, context stays code, and the truncated/tail lines stay muted.

  (b) The inverse color-name map. `cmd/polly/internal/markdown/table.go:189-203` caches `paletteColorNames = sync.OnceValue(func() map[ui.Color]string {...})` inverting ui.StyleParserColorMap, and `tableCellMarkup` (:225) re-encodes wrapped table cells by name with a `fmt.Sprintf("#%06x", c.Hex())` fallback on a miss. That cache is built once per process and never rebuilt, so after a theme apply it re-encodes a color as a name whose meaning has changed (verified repro: with the default map it stores ColorBlue -> "accent"; after an accent change that cell renders as the new accent instead of blue). Replace the sync.OnceValue with a value+epoch pair keyed on style.Epoch(), rebuild when the epoch differs, keep the existing lowest-name-wins tie-break, and note in a comment that with 23 roles ties are now the common case because unset token roles share their fallback's color. Output for the default theme must not change: the semantic names sort before the token ones (accent<syn-keyword, code<syn-func, err<syn-del, muted<syn-comment, ok<syn-add/syn-string, active<syn-number), so default-theme table markup must be byte-identical to today.

  Conventions: stdlib testing only, table-driven, t.Fatalf, no golden framework. Byte-identical claims are about RESOLVED COLORS, not markup literals - the existing assertions that pin the old names (markdown_test.go asserting `[func](fg:accent)`, `[// done](fg:muted)`, and TestHighlightDiffUsesOkAndErr asserting fg:ok/fg:err) legitimately change to syn-* and must be updated and renamed, not deleted. The fence-gutter assertion `[| ](fg:muted)` (wrap_test.go, markdown_test.go) is unaffected. Do not edit cmd/polly/internal/style/* (owned by style-theme-core) or cmd/polly/repl_view.go / repl_tools_inspector.go (owned by tool-output-highlighting).

#### `tool-output-highlighting` — Highlight the bash and generic tool result bodies through the fence token map

- dependsOn: none
- paths: `cmd/polly/tool_output_lang.go`, `cmd/polly/tool_output_lang_test.go`, `cmd/polly/repl_view.go`
- acceptance:
  - A bash tool result containing a unified diff renders diff highlighting (syn-add/syn-del markup) in the tool output body; a generic tool result chroma cannot confidently lex renders exactly as today (code role, byte-identical markup and runes).
  - The language helper never returns a lexer name for content chroma analyses as lexers.Fallback.
  - markdown.HighlightCodeLines(code, "") behaviour is untouched (no edit to the markdown package in this task).
  - No new module dependency (go.mod unchanged).
  - gofmt -l . prints nothing; build, vet, and CGO_ENABLED=0 go test -count=1 ./cmd/polly -run 'ToolOutput|Highlight' pass in a workspace with a writable home.
- brief: Acceptance criterion 4: syntax highlighting must appear in bash and generic tool output with the same token->role map as fenced code. There is exactly one tool-result-body renderer: `appendInspectedToolOutput` in cmd/polly/repl_view.go:169, whose body branch is repl_view.go:219-232 - it strips image markers, calls `markdown.HighlightCodeLines(text, "")`, and wraps the result in `markdown.RenderFence(title, raw)`. It is called for every tool result (inline transcript and the tool inspector, cmd/polly/repl_tools_inspector.go:99). The empty lang argument is what makes today's output plain code-colored.

  Add cmd/polly/tool_output_lang.go (do NOT touch cmd/polly/internal/markdown/markdown.go - another task owns it) with a language-selection helper, e.g. `func toolOutputLanguage(body string) string`, and pass its result at the repl_view.go:222 call site. Chroma's `lexers.Analyse(text)` is available for content sniffing; return "" (today's plain code render) when the analyser yields nil or lexers.Fallback, so nothing regresses when no lexer is confident. Rationale for a comment: bash tool OUTPUT is usually not shell source, so a unified diff body should be lexed as a diff rather than as shell; the bash COMMAND is already highlighted explicitly with "bash" in cmd/polly/bash_inspector.go:150 and cmd/polly/repl_tools_inspector.go:78 and neither changes. Do not change the arguments-fence path (repl_tools_inspector.go:88, which already passes json when the arguments are JSON).

  github.com/alecthomas/chroma/v2 is already a direct module dependency and lexers is a subpackage of it, so this adds no new module dependency (cmd/polly does not import chroma today; importing it there is fine).

  Tests in cmd/polly/tool_output_lang_test.go: table-driven with t.Fatalf; assert a unified diff body selects the diff lexer, plain prose/unknown input selects "" (and therefore that the render is byte-identical to today's styledLines(code, "code", "") output), and a bash-output-like body containing a diff hunk produces syn-add/syn-del markup end to end through `markdown.HighlightCodeLines(text, toolOutputLanguage(text))`. This task does not write the parser map, so no global-style restore is needed.

#### `line-color-capabilities` — Truecolor and 256-color SGR for the line frontend

- dependsOn: none
- paths: `cmd/polly/output_capabilities.go`, `cmd/polly/output_capabilities_test.go`, `cmd/polly/line_markdown.go`, `cmd/polly/line_markdown_test.go`, `cmd/polly/line_activity.go`, `cmd/polly/line_stream.go`
- acceptance:
  - With COLORTERM=truecolor a truecolor RGB cell emits 38;2;r;g;b; with it unset (or only TERM=xterm-256color) the same cell emits 38;5;N for a deterministic nearest N; palette slots 0-15 emit exactly today's 30-37/90-97; ui.ColorClear emits no color code.
  - resolveOutputCapabilities populates the new fields from the injected getenv, and NO_COLOR, TERM=dumb, and non-TTY output still emit no SGR at all (existing assertions keep passing).
  - The stderr status path (styledMarkupToLine) uses the same truecolor/256 decision as stdout answer output.
  - Table-driven tests cover all branches, including the 0-15-in-nearest-search policy, documented in a comment.
  - No new module dependency; gofmt -l . prints nothing; build, vet, and CGO_ENABLED=0 go test -count=1 ./cmd/polly -run 'LineMarkdown|OutputCapabilities' pass.
- brief: Goal 7 / acceptance 9. Today the SGR writer maps only palette slots 0-15 and silently emits no color for anything else: `ansiPaletteCode` (cmd/polly/line_markdown.go:126-145) loops `for index := range 16` and returns `0, false` on a miss, so a themed truecolor or palette:200 role loses all styling in -p output. `ansiStyleSequence` (line_markdown.go:97-125) joins integer codes with ';', so the helper's signature must change to carry multi-parameter codes like 38;5;N.

  (a) Capabilities. `outputCapabilities` (cmd/polly/output_capabilities.go:22-28) gains true-color and 256-color fields (e.g. truecolor, color256 bool) derived inside `resolveOutputCapabilities` from the injected getenv - never from a second os.Getenv at the writer, or the fields become untestable. Detection mirroring tcell: truecolor when COLORTERM is truecolor, direct, or 24bit, or TERM ends in -direct/-truecolor; 256 when TERM ends in -256color or COLORTERM contains 256. The managed-TUI branch and the raw branch (!stdoutTTY || TERM=dumb) each return early before reading TERM/COLORTERM, so set the fields in all three returns if anything downstream reads them. Everything stays subordinate to noColor (NO_COLOR non-empty) and to the raw surface: no SGR may leak into piped, TERM=dumb, or NO_COLOR output.

  (b) Emission. Grow ansiStyleSequence/ansiPaletteCode so they emit SGR params: palette 0-15 keeps exactly today's 30-37/90-97 (and 40-47/100-107 for background); palette 16-255 emits 38;5;N with N = int(color) & 0xffffff (tcell's own extraction rule); RGB emits 38;2;r;g;b when truecolor and otherwise the nearest of the 256-color palette; ui.ColorClear emits no code (this is what makes inherit round-trip). For nearest-256 use `github.com/gdamore/tcell/v3/color.Find(c, palette)` over a package-level slice built from PaletteColor(0..255) - tcell already degrades RGB that way in the TUI, and this adds no dependency (termenv's algorithm would need go-colorful, currently only an indirect require). Document and test whether indices 0-15 participate in the nearest search (they are terminal-remappable; tcell's own palette includes them, termenv excludes them).

  (c) Plumbing. `appendANSIStyledCells(out *bytes.Buffer, cells []ui.Cell)` (line_markdown.go:78) takes no capabilities today; thread the truecolor/256 decision (or the whole outputCapabilities) into it and into ansiStyleSequence. Callers: renderLineMarkdown (already has capabilities in scope), lineCellsOutput (cmd/polly/line_stream.go:143-155, called at :203 and :257 with !ui.capabilities.noColor), and styledMarkupToLine(markup string, color bool) (cmd/polly/line_activity.go:248-262), which activityColorLocked calls with a hardcoded true while only checking lineStatusCapabilities.color. Fix that stderr status path too, either by reusing the imageCaps value line_activity.go:101 already resolves for the stderr surface or by adding the fields to lineStatusCapabilities, so status lines and answer output agree.

  Tests: extend the TestResolveOutputCapabilities table (cmd/polly/output_capabilities_test.go:9; mapGetenv helper at :96) with COLORTERM/TERM cases for all three surfaces, and add table-driven raw-escape assertions in cmd/polly/line_markdown_test.go for the branches (palette 0-15 unchanged, palette 16-255 and non-truecolor RGB emit 38;5;N, truecolor RGB emits 38;2;r;g;b, ui.ColorClear emits none). Existing outputCapabilities struct literals in tests stay source-compatible (zero value = false). Keep the no-color/dumb/raw expectations in the one-shot PTY fixture (cmd/polly/testdata/oneshot_terminal.py, TERM=xterm-256color) passing. No new module dependency; do not edit cmd/polly/polly.go (owned by theme-selection-and-flags).

#### `theme-selection-and-flags` — Theme selection: flags, env, ~/.pollytool/config, discovery, COLORFGBG, startup apply

- dependsOn: `style-theme-core`
- paths: `cmd/polly/theme.go`, `cmd/polly/theme_test.go`, `cmd/polly/config.go`, `cmd/polly/types.go`, `cmd/polly/polly.go`, `cmd/polly/config_test.go`, `cmd/polly/storage_runtime_test.go`
- acceptance:
  - polly --help lists --theme and --theme-variant each with its [$POLLYTOOL_*] variable (asserted in a test, not by eye).
  - Precedence is flag > POLLYTOOL_THEME > ~/.pollytool/config > built-in default, asserted with the existing parseEnvTestConfig helper.
  - A path containing '/' or ending in .json is read as a path; otherwise `<themes dir>/<name>.json` wins, with the built-in presets as fallback (and `default` reserved, so a user file of that name is ignored).
  - COLORFGBG 15;0, 0;7, 0;9 -> light; 0;15, 0;8, empty, junk, 0;08 -> dark; --theme-variant dark|light overrides auto.
  - An unknown theme name, a missing file, or malformed JSON prints a startup stderr notice and the run continues with the built-in default (never a fatal error, never a flag Validator failure).
  - The apply happens before runManagedREPL, and no settingSpecs row or session-metadata write is added.
  - gofmt -l . prints nothing; build, vet, and the targeted theme/config tests pass.
- brief: Acceptance criterion 1 plus goals 2 and 4. Everything here lives in cmd/polly, and the apply must happen exactly once, before either frontend takes the terminal.

  (a) Flags and Config. Add `func themeConfigFlags() []cli.Flag` and concatenate it in defineFlagsWithGroups (cmd/polly/config.go:129-141), modelled on outputConfigFlags() (config.go:401): &cli.StringFlag{Name: "theme", Value: "default", Usage: "...", Sources: envDefault("POLLYTOOL_THEME")} and &cli.StringFlag{Name: "theme-variant", Value: "auto", Usage: "...", Sources: envDefault("POLLYTOOL_THEME_VARIANT")}. envDefault (cmd/polly/flag_sources.go:74-78) chains the environment then ~/.pollytool/config, and envDefaultSource.Key()/IsFromEnv() (:52-55) are what make urfave list the variable in --help - that is how criterion 1's [$POLLYTOOL_THEME] is satisfied. Do NOT attach a Validator to --theme (an unknown name must be a load-time notice plus fallback, never a flag-parse failure). Read both into new Config fields in parseConfig (config.go:53); Config is documented as process-wide (cmd/polly/types.go:44-48). Do NOT add a settingSpecs row (cmd/polly/settings.go:69): those rows are /set-able and persist into session metadata, and cmd/polly/settings_test.go:22-31 pins the key lists.

  (b) cmd/polly/theme.go (new). Provide, and do not rename (other tasks call these):
  - `type themeSelection struct { theme style.Theme; path string; builtin bool }`
  - `themeDir() (string, error)` = ~/.pollytool/themes, built from the existing userConfigDirName constant (cmd/polly/userconfig.go:25), not a hardcoded string.
  - `resolveThemeSelection(name string) (themeSelection, error)`: a value containing '/' or ending in .json is a path; otherwise look up `<themes dir>/<name>.json` first, then the compiled-in presets (default, dark, light) - with `default` reserved, so a user file of that name is ignored (parent correction: the research draft had preset-first, which contradicts the spec's shadowing rule). Load with style.ParseTheme; wrap read/parse failures with fmt.Errorf("...: %w", err).
  - `resolveThemeVariant(requested string, getenv func(string) string) string` and `variantFromColorFGBG(v string) string`: auto reads COLORFGBG (fg;bg, last ';'-separated field is the background index) - light when the index is 7 or >= 9, dark otherwise including 8; unset, unparseable, or a TERM-less environment resolves to dark. No OSC 11 probe. Take getenv as a parameter so the table test needs no terminal (mirror resolveOutputCapabilities' injected-getenv signature and the mapGetenv helper at output_capabilities_test.go:96).
  - `themeSelectionFromConfig() (string, bool)`: re-reads ~/.pollytool/config for POLLYTOOL_THEME bypassing userConfigValue - that cache is keyed on path only and is cleared only by writeUserConfig (userconfig.go:117-144, 180-184), so a hand edit is otherwise invisible.
  - Presets dark and light with pinned RGB values (see the resolution recorded in Open questions below).

  (c) Startup apply. In cmd/polly/polly.go, inside commandRunner.runConversation, resolve and apply right where r.outputCapabilities = outputCapabilitiesForRun(...) happens (polly.go:180-186), which is before runManagedREPL hands the tty to tcell. On success store the active themeSelection on the runner (later tasks read the active file path for the reload watcher). On a load error print the startup stderr notice form `polly: <display path>: <detail>` (userconfig.go:134,138), fall back to the default preset, and keep going; after the TUI owns the screen a raw stderr write is wiped by the alternate screen, so any error discovered later must go through m.appendNoticeLine instead.

  Tests (cmd/polly/theme_test.go, new; plus cmd/polly/config_test.go): flag > env > file > builtin precedence through parseEnvTestConfig (flag_sources_test.go:14); a --help capture via getCommand() with cmd.Writer = &bytes.Buffer asserting both flags and both [$POLLYTOOL_...] variables appear (no such test exists today); COLORFGBG table cases 15;0, 0;15, 0;7, 0;8, 0;9, 0;08, 15;default;0, empty, junk; unknown theme name and broken JSON -> notice plus default, never a startup failure. Use t.Setenv("HOME", t.TempDir()) plus t.Setenv("USERPROFILE", ...) for os.UserHomeDir (config_test.go:273-274); tests are serial, so t.Setenv is fine and t.Parallel must not be used. Add POLLYTOOL_THEME and POLLYTOOL_THEME_VARIANT to the env-unset list in parseStorageTestConfig (cmd/polly/storage_runtime_test.go:525) so a developer's own theme cannot leak into unrelated tests.

#### `theme-epoch-invalidation` — Make every resolved-cell cache follow the style epoch

- dependsOn: `style-theme-core`
- paths: `cmd/polly/repl_theme_epoch.go`, `cmd/polly/repl_theme_epoch_test.go`, `cmd/polly/repl_transcript.go`, `cmd/polly/repl_typewriter.go`, `cmd/polly/repl_child_cache.go`, `cmd/polly/repl_main_view_cache.go`, `cmd/polly/repl_agents.go`
- acceptance:
  - A test applies a new theme with only style.Apply + applyStyleEpoch (no explicit invalidate) and observes the new colors via a simulation screen in transcript rows, the orbit frame, the scrollbar thumb, the bird, and a code block.
  - transcriptRows re-parses when the epoch changed even though the geometry still fits and every block's key/text/cells are unchanged.
  - A mid-stream change re-parses assistantTypewriter.cells; the visible prefix renders in the new theme.
  - Agent links remain clickable (exact-style hit test still matches) after a theme change.
  - Cached image spans and native image placements do not reposition across a theme change.
  - CGO_ENABLED=1 go test -race ./cmd/polly passes; gofmt -l . prints nothing; build and vet pass.
- brief: This is the correction the spec under-specifies: invalidate() alone does NOT re-resolve colors. Verified in the assigned copy: transcriptVisualCache.invalidate() (cmd/polly/repl_transcript.go:346) only sets valid = false, while transcriptRows returns the cached rows when c.valid && fits (:382) and, when it does rebuild, reuses each old block's rows whenever key/text/cells/images/etc. are unchanged (`changed := !fits || ...`, :402-404). So with valid=false and fits=true nothing is re-parsed and the screen keeps the old colors. The fix is to make the epoch part of the reuse decision.

  Create cmd/polly/repl_theme_epoch.go exporting exactly this one entry point (the reload/apply task calls it; do not rename):

      func (r *managedREPL) applyStyleEpoch(epoch uint64)

  It runs on the TUI event loop with the model lock held. For every tab (r.tabs, including background/child tabs), every workspace view and cached child view, and the main view cache: invalidate visual caches, reset the streaming typewriter, and nil/zero the per-entry markdown code caches (markdown.CodeCache has no Reset; the established reset is a zero-value assignment - see line_stream.go:284 and repl_main_view_cache.go:17). Concretely:
  - transcriptVisualCache (repl_transcript.go:327-346): add an epoch uint64 field and require c.epoch == style.Epoch() inside fits (or an equivalent guard) so both the `if c.valid && fits` return and `changed := !fits || ...` see the change; set c.epoch wherever the cache is stored. Without this the whole feature silently fails after a theme apply.
  - assistantTypewriter (cmd/polly/repl_typewriter.go:95-125): s.cells is re-parsed only when the source string changes and s.prefix() is handed to the current-assistant block, so a mid-stream theme change leaves the visible prefix stale even after the visual cache is dropped. Add an epoch field and force the re-parse when the epoch differs (clear source/cells or key the parse on the epoch).
  - Cached child views (cmd/polly/repl_child_cache.go:128-133) clone visual.rows/visual.blocks, and retireMainProjection/restoreMainProjection (repl_main_view_cache.go) reuse a projection when visual.revision matches: a full invalidate (revision++) forces the rebuild, so make sure the epoch bump reaches every model's visual, including models that are not the visible tab.
  - cmd/polly/repl_agents.go:294-298: agentLinkStyle = sync.OnceValue(func() ui.Style { return style.ParseCells(style.Link("x"), ui.StyleClear)[0].Style }) caches the resolved accent style, and agentDetail finds clickable cells by exact equality (cell.Style == linkStyle, :346). After any theme change the cached style no longer matches freshly parsed link cells, so agent links stop being clickable until restart - a behavioural regression, and the cache is not in the spec's list. Drop the cache or key it on the epoch.
  - cmd/polly/repl_images.go: the cached transcriptImageSpans live inside transcriptVisualBlock, so they follow the block cache; assert in a test that image spans do not reposition (and native image placements stay put) across a theme change - the spec's edge case.
  - chromeColor/refreshChrome/the scrollbar re-read the map on every render, and pollyBirdRows (repl_masthead.go:91-93) caches MARKUP carrying role names, so those need only the repaint the apply path forces. State the general rule in a comment: caches holding markup that names roles are epoch-safe; caches holding resolved ui.Cell/ui.Style/ui.Color are not.
  - The parser map itself is an unsynchronized global read by gotui on the draw path, so applyStyleEpoch must never be called off the event loop; note it in the doc comment.

  Tests in the new cmd/polly/repl_theme_epoch_test.go: build a REPL with the existing harness (affordanceTestREPL/chromeTestREPL install a tcell.SimulationScreen into ui.DefaultBackend.Screen; screen.Get(x,y) returns (glyph, ui.Style, width); see repl_chrome_test.go:22,336-346). Render with theme A, apply theme B through style.Apply + r.applyStyleEpoch(style.Epoch()) with NO other invalidation call, re-render, and assert that a transcript row cell, the orbit frame perimeter, the scrollbar thumb, the bird glyph, and a fenced code block cell all carry B's colors. Add a test that the agent-link style matches freshly parsed link cells after a theme change, and a test that a mid-stream typewriter prefix re-parses. Stdlib testing only, t.Fatalf, t.TempDir(); restore the global parser map in t.Cleanup. Run CGO_ENABLED=1 go test -race ./cmd/polly as part of this task.

#### `theme-apply-reload-command` — One event-loop apply path, a stat-poll hot reload, and the /theme command

- dependsOn: `style-theme-core`, `theme-selection-and-flags`, `theme-epoch-invalidation`
- paths: `cmd/polly/repl_theme.go`, `cmd/polly/repl_theme_test.go`, `cmd/polly/repl_loop.go`, `cmd/polly/repl_commands.go`, `cmd/polly/repl_test.go`
- acceptance:
  - Editing the active theme file while the TUI runs applies on the next tick with no restart and no stale colors in the transcript, orbit frame, scrollbar, bird, or code blocks (acceptance 6).
  - With no other activity the reload still repaints (proves the poll is not gated behind needsTick()).
  - A half-written/invalid file leaves the previous theme on screen, prints a notice, and the finished file is picked up on a later tick.
  - A change to POLLYTOOL_THEME inside ~/.pollytool/config is detected and applied (the watcher does not go through the process-lifetime config cache).
  - /theme lists builtin and user themes with the active one marked; /theme <name> switches for the session; /theme default restores the builtin; the fallback REPL reports the command unavailable instead of applying.
  - Slash-command completion tests are updated for /theme and pass; gofmt/build/vet/targeted cmd/polly tests pass.
- brief: Goal 6, the /theme half of goal 4, and acceptance 6. Create cmd/polly/repl_theme.go with the single apply path and the command, and wire the watcher into the existing tick.

  (a) Apply path (event loop only). Provide, and do not rename:
  - `func (r *managedREPL) applyTheme(theme style.Theme, variant string) uint64`: style.Apply(theme, variant), then r.applyStyleEpoch(epoch) (from task theme-epoch-invalidation), then r.render(); return the epoch.
  - `func (r *managedREPL) applyThemeByName(name string) (uint64, error)`: resolveThemeSelection + resolveThemeVariant (task theme-selection-and-flags), wrapping load errors; on error print r.model.appendNoticeLine("Warning: " + body) and keep the previous theme. The TUI owns the terminal after runManagedREPL, so a direct stderr write here is wiped by the alternate screen.
  - `func (r *managedREPL) pollTheme(now time.Time) bool`: the watcher body, directly callable so tests need no ui.Init()/Run(). It stat-polls (mtime + size) the active theme file when it is a file (a builtin preset has none) and the theme selection inside ~/.pollytool/config, throttled to about 1s via a stored time.Time. On a change it re-resolves the selection, re-applies, and reports true; on a load error it notices and keeps the previous theme so a half-written file is retried on the next tick.
  - `func (r *managedREPL) activeThemeSource() (path string, ok bool)` for tests and introspection.

  (b) Watcher wiring. cmd/polly/repl_loop.go creates `ticker := time.NewTicker(50 * time.Millisecond)` (:90) and its arm is `case <-ticker.C: if r.needsTick() { r.render() } else { r.tickAffordances(time.Now()) }` (:148-153). needsTick() is true only for a busy model, a live picker, swarm activity, or an inspector retry, so an idle TUI never repaints by itself. Call r.pollTheme(now) in that arm and force r.render() yourself when it reports a change, even if needsTick() is false. The '~1s' in the spec does not exist as a tick - it is an inspector refresh throttle (repl_loop.go:234, repl_inspector.go:285) - so the 1s gate must be your own.
  Do not read the config selection through userConfigValue: it caches the parsed file per path for the lifetime of the process and is cleared only by writeUserConfig (cmd/polly/userconfig.go:117-144, 180-184), so a user's manual edit would never be seen. Use the readUserConfig-based re-read provided by task theme-selection-and-flags.

  (c) /theme. Register in newDefaultReplCommandRegistry (cmd/polly/repl_commands.go:73-78) as busySafe: true with usage `/theme [name]`. Interactive-only work goes through a new replCommandContext callback (pattern at repl_commands.go:230-255), set by newManagedReplCommandContext and left nil by the fallback/writer context so the handler reports unavailability the way replClearCommand does (repl_commands.go:400-411); the line frontend has no hot reload, so it must not silently apply. /theme lists builtin and user themes with the active one marked and shows which file won; /theme <name> switches for the session through the same reload path (no persistence); /theme default returns to the built-in.

  (d) Tests (cmd/polly/repl_theme_test.go, new) plus the existing pinned assertions. Update cmd/polly/repl_test.go:496-503: {"/t", true, "/t", []string{"/title", "/tools"}} must include /theme, the bare-'/' case uses slashCommands, and add a /th case. repl_commands_test.go's help assertions forbid /get, /skills, /stats and are otherwise unaffected. Use os.Chtimes to force a distinct mtime on the fixture theme file instead of relying on same-second granularity; put fixture themes under t.TempDir() with t.Setenv("HOME", ...) plus USERPROFILE. Assert: an idle REPL repaints on a theme-file edit with no other activity and each of transcript/orbit/scrollbar/bird/code cells changes color; a truncated file keeps the previous colors and prints a notice; pollTheme returns false when nothing changed.

#### `set-theme-tool` — set_theme tool: live apply, two-call persist protocol, and child denial

- dependsOn: `style-theme-core`, `theme-selection-and-flags`, `theme-apply-reload-command`
- paths: `cmd/polly/theme_tool.go`, `cmd/polly/theme_tool_test.go`, `cmd/polly/conversation.go`, `subagent/subagent.go`, `docs/API.md`
- acceptance:
  - With the managed-TUI surface set on the state, set_theme is in the registry and always-allowed; with a line/raw surface it is absent.
  - name plus colors without persist changes the running session immediately and writes nothing.
  - persist without confirm writes nothing and returns a JSON object with status: confirmation_required and the exact absolute target path.
  - persist+confirm writes ~/.pollytool/themes/<name>.json, sets POLLYTOOL_THEME in ~/.pollytool/config, and the theme survives a restart; an existing file requires overwrite: true and otherwise returns THEME_EXISTS; the themes directory is created only on the first persist with 0700 dir / 0644 file.
  - All five error codes are produced as *tools.ToolError and asserted with errors.As; the tool never mutates the parser map from its own goroutine (verified by CGO_ENABLED=1 go test -race ./cmd/polly).
  - subagent.ChildRegistry denies set_theme and docs/API.md names it; gofmt/build/vet pass.
- brief: Goal 5 and acceptance 7. Register an in-process tool the way registerSessionTitleTool does (cmd/polly/session_title.go:18-55: state.toolRegistry.Register(&tools.Func{...}) then MarkAlwaysAllowed), called from cmd/polly/conversation.go next to registerSessionTitleTool(state) (:347). Gate registration on state.outputCapabilities.surface == outputSurfaceManagedTUI (cmd/polly/output_capabilities.go returns that surface only for REPL + managed tty) so the tool is absent in one-shot/line/raw runs; a bare &conversationState{} in tests has the zero value, which is not that surface.

  Args (schema.Params, Strict: true, Required: []string{"name"}): name via schema.S; persist, confirm, overwrite via schema.Bool; colors, light, dark as objects. schema/tool.go has S/Int/Bool/Enum/Strings/Array and NO object helper, so hand-write {"type":"object", "additionalProperties":{"type":"string"}} for the three object args.

  Behaviour:
  - No persist: apply immediately, session-only.
  - persist: true without confirm: true: write NOTHING; return a json.Marshal'd {"status":"confirmation_required","path":"<absolute target>","theme":{...}} so the model can show the exact file and colors, then call again.
  - persist + confirm: write <themes dir>/<name>.json (os.MkdirAll(dir, 0o700) / os.WriteFile(..., 0o644), created on first persist only) and set POLLYTOOL_THEME=<name> through writeUserConfig(userConfigPath(), map[string]string{"POLLYTOOL_THEME": name}) (cmd/polly/userconfig.go:149, merge-based so it cannot clobber the setup form's four keys, and it clears the config cache), then reload immediately. Mirror repl_model_form.go's environment-shadowing warning (shadowedByEnvironment) when POLLYTOOL_THEME is already exported, or acceptance 7's 'survives a restart' is false for that user.
  - Replacing an existing user theme file requires overwrite: true.
  - Validate role names and color values by assembling the theme JSON from the args and calling style.ParseTheme(name, data) from task style-theme-core, so the tool and the file loader share exactly one validator.
  - Errors: *tools.ToolError via tools.NewToolError(msg, CODE) with codes UNKNOWN_ROLE, INVALID_COLOR, INVALID_THEME, THEME_EXISTS, THEME_WRITE_FAILED (pattern: session_title.go:37-44 switches on errors.Is to pick a code).

  Concurrency (highest risk). tools.Func.Run executes on a worker goroutine, and ui.StyleParserColorMap is a plain map read lock-free by gotui's parser on the draw path - polly also parses cells off the event loop (retired-child-view backgrounds call display.transcriptRows, and tool/status paths call style.ParseCells under toolMu). Writing the map or bumping the epoch from the tool goroutine is a fatal concurrent-map-write, not a lost update. So type-assert the parent TurnUI exactly as SessionTitleChanged does (parentTurnUIFrom(ctx) -> interface{ ThemeChanged(style.Theme) }, session_title.go:46-48), implement `func (t *gotuiTurnUI) ThemeChanged(theme style.Theme)` in this task's new file, and have it post to the event loop with t.repl.postUI(t.repl.work.ctx, func(){ t.repl.applyTheme(theme, variant) }) (pattern: cmd/polly/repl_session_title.go:20-30, postUI at repl_work.go:64). applyTheme comes from task theme-apply-reload-command.

  Children. Add "set_theme" to the tools.DenyTools(...) list in subagent.ChildRegistry (subagent/subagent.go:309, which already denies set_session_title, swarm_*, workflow_*, the coordination tools) and add set_theme to the docs/API.md:705-708 sentence - otherwise agent-tab children inherit the tool.

  Tests (cmd/polly/theme_tool_test.go, new): call registerThemeTool(state) directly (pattern session_title_test.go:18-22), assert present/absent for the two surfaces, walk the two-call persist protocol, assert the five codes with errors.As into *tools.ToolError, assert ~/.pollytool/themes/ is not created before the first persist, and assert a second persist without overwrite fails with THEME_EXISTS. Isolate the home with t.Setenv("HOME", t.TempDir()) plus t.Setenv("USERPROFILE", ...). Run CGO_ENABLED=1 go test -race ./cmd/polly.

#### `theme-builtin-skill` — Builtin theme skill that interviews the user and uses set_theme

- dependsOn: none
- paths: `skills/builtin/theme-designer/SKILL.md`, `skills/builtin_test.go`
- acceptance:
  - LoadBuiltinCatalog() returns the theme skill and catalog.Get("theme-designer") succeeds after materialization into $HOME/.pollytool/builtin-skills/theme-designer.
  - Frontmatter validates (name == directory, lowercase/hyphen name, description present and < 1024 chars) with no allowed-tools field.
  - The body references no repository paths (checked by an explicit assertion over github.com/alexschlessinger / cmd/polly / docs/features, not the substring pollytool).
  - The body documents the 23 roles, the five value forms, the two-call set_theme persist protocol, and the five error codes.
  - CGO_ENABLED=0 go test -count=1 ./skills/... passes and gofmt -l . prints nothing.
- brief: Acceptance criterion 8. Add skills/builtin/theme-designer/SKILL.md so it is embedded by //go:embed all:builtin (skills/builtin.go:15-18), materialized to ~/.pollytool/builtin-skills/theme-designer at startup, and selectable through the same catalog as feature-workflow (a same-named user skill shadows a builtin via Catalog.Merge).

  Frontmatter contract (enforced by skills/catalog.go's validateFrontmatter): name: theme must equal the directory name and use only lowercase letters/digits/hyphens with no leading/trailing/consecutive hyphens; description is required and must be under 1024 characters - have it state both what the skill does and when to trigger it. Do not add allowed-tools: the registry widens access on activation and set_theme is marked always-allowed the way set_session_title is.

  Body: teach the model to (1) interview the user for intent - light or dark, match the terminal's own palette (palette slots / parser names) or pin RGB; (2) choose from the 23 roles - semantic ok, err, run, accent, active, muted, code; token syn-comment, syn-keyword, syn-string, syn-number, syn-func, syn-add, syn-del (falling back to muted, accent, ok, active, code, ok, err); bird polly-green, polly-light, polly-wing, polly-crown, polly-beak, polly-mouth, polly-face, polly-eye, polly-foot; (3) know the five value forms - #rrggbb, #rgb, palette:N (0-255), a name from the parser's map, inherit, or the key omitted for polly's built-in value; (4) apply with set_theme without persist first and show the user; (5) treat a {"status":"confirmation_required","path":...} reply as an instruction to surface that exact path and colors to the user, and only then call again with confirm: true (and overwrite: true when replacing an existing file); (6) mention the error codes UNKNOWN_ROLE, INVALID_COLOR, INVALID_THEME, THEME_EXISTS, THEME_WRITE_FAILED so it can correct itself. Keep it short and imperative.

  Constraint (AGENTS.md): builtin skills are embedded in the binary, materialized outside the repository, and run against arbitrary projects, so the SKILL.md must not reference this repository. Caveat for the reviewer: the check must look for repository references (github.com/alexschlessinger, cmd/polly, docs/features, a pollytool module path) - NOT the bare substring 'pollytool', because the skill legitimately names set_theme and the runtime path ~/.pollytool/themes/.

  Test: extend skills/builtin_test.go (existing shape: home := t.TempDir(); t.Setenv("HOME", home); catalog, err := LoadBuiltinCatalog(); ... catalog.Get("feature-workflow"), plus the materialized path filepath.Join(home, ".pollytool", "builtin-skills", <name>)) to assert catalog.Get("theme-designer") succeeds, SKILL.md is materialized under builtin-skills/theme-designer, and the frontmatter name matches the directory. Optionally add, as new coverage, a loop over catalog.List() reading each builtin SKILL.md and failing on a repository reference - that AGENTS.md rule is currently untested anywhere.

  This task touches no Go production code, so it can run concurrently with the rest; it depends on the spec's pinned set_theme argument names, and if the tool task changes them the integration step must reconcile the skill body.

#### `readme-themes-docs` — README themes documentation and stale-claim fixes

- dependsOn: `theme-selection-and-flags`, `theme-apply-reload-command`, `set-theme-tool`
- paths: `README.md`, `AGENTS.md`
- acceptance:
  - README.md contains no claim that polly has no theme flag or environment variable; the framed-inspector note describes only the fixed amber approval border.
  - The slash-command block lists /theme [name] and still renders as an aligned fixed-width block; the built-in tools list includes set_theme; the builtin-skills paragraph names theme.
  - A `## Themes` section documents all 23 roles, the five value forms, the theme file shape, selection precedence and the path rule, variant detection, notice-plus-fallback on load errors, hot reload, /theme vs set_theme persistence, the line-frontend SGR rules, and NO_COLOR/TERM=dumb behaviour.
  - polly --help output and the documented flag/env/config names agree with the implementation (spot-checked, not assumed).
  - gofmt -l . prints nothing and the build/tests are unaffected (docs-only change).
- brief: The docs research found four README places this feature invalidates plus one absent section; do all of them as one change to README.md, plus one optional AGENTS.md line.

  1. README.md:84-86 asserts 'The framed inspector (scrollbar, draggable split, running glint, amber border on pending approval) is always on; there is no theme flag or environment variable for it.' That becomes false. Reword so only the fixed amber approval border (and image rendering) is described as not themable, and point at the new Themes section.
  2. README.md:256-262 is a hand-maintained fixed-width slash-command block; add /theme [name] and reflow it so the columns still line up.
  3. README.md:553-558 lists the Built-in tools default set; add set_theme with a one-line note that persisting requires an explicit confirmation call.
  4. README.md:626-632 says 'Currently that is feature-workflow, an end-to-end feature pipeline'; add the theme builtin skill and what it does.
  5. Add a new `## Themes` section (no color/theme section exists today; place it after the TUI section, before the contexts discussion) covering: the 23-role table with token-role fallbacks; the five value forms (#rrggbb, #rgb, palette:N, a parser map name, inherit, omitted); the file ~/.pollytool/themes/<name>.json with the name/variant/colors/light/dark shape and a worked example; selection precedence --theme > POLLYTOOL_THEME > ~/.pollytool/config > built-in default with the path rule (contains '/' or ends in .json); --theme-variant auto|dark|light with the COLORFGBG rule (light when the background index is 7 or >= 9, unset/unparseable resolves to dark); that a bad value or missing file prints a notice and falls back to default instead of blocking startup; that editing the file hot-reloads in the TUI (and not in the line frontend); the /theme vs set_theme distinction (session-only vs persisted, with the two-call confirm); the line-frontend rule (palette 0-15 keep 30-37/90-97, palette 16-255 and non-truecolor RGB emit 38;5;N, truecolor RGB emits 38;2;r;g;b); and that NO_COLOR / TERM=dumb / piped output stay unstyled. NO_COLOR is implemented today (output_capabilities.go:77, line_activity.go:29) and documented nowhere - mention it here.
  6. Also add one sentence on sandbox reach: POLLYTOOL_* is stripped from tool environments (docs/SANDBOX.md:364-366) and ~/.pollytool/themes is not a home read grant (cmd/polly/sandbox_home_grants.go:15-35), so set_theme is the sanctioned writer and a user who wants to cat a theme from bash must add --readpath.
  7. Optional AGENTS.md: mention ~/.pollytool/themes alongside the existing runtime-state gotcha (~/.pollytool/ holds polly.db, skills/, worktrees/), and note that the builtin theme skill must not reference the repository.

  Do NOT touch docs/API.md (its only theme-relevant line, the ChildRegistry deny list at :705, belongs to the set-theme-tool task) and do not invent a docs/THEMES.md - the README header enumerates its companion docs and there is no precedent for a theme-specific file. Read the final behaviour from the merged code, not the spec, wherever the two differ (for example the resolved-variant wording and the chosen preset values).

#### `acceptance-verification` — End-to-end acceptance verification and screenshot evidence

- dependsOn: every implementation task
- paths: none (read-only; screenshots to `$POLLY_SHOT_DIR`, default `/tmp/polly-shots`)
- acceptance:
  - Every command in the checks list is executed and its exit status reported individually; any failure is quoted with the command and output.
  - Each of the 10 numbered acceptance criteria has a PASS/FAIL with concrete evidence (command output, test name, or screenshot path), and unexercisable criteria are named with the reason.
  - Before/after TUI screenshots exist for at least one theme change and one --theme-variant light run, produced with the sanctioned polly-tui driver, and no polly binary or binary artifact is left staged for commit.
  - No repository file is modified by this task; the report is returned as the task result.
- brief: Read-only verification task; make no repository edits (captures go to $POLLY_SHOT_DIR, default /tmp/polly-shots). Run after every other task is merged. This is the acceptance-criteria matrix for the parent's integration step.

  1. Run the full gate: gofmt -l . (must print nothing), CGO_ENABLED=0 go build ./..., go vet ./..., CGO_ENABLED=0 go test ./..., python3 -B -m unittest discover -s .github/local-ci -p 'test_*.py', POLLYTOOL_REQUIRE_SANDBOX_TESTS=1 CGO_ENABLED=0 go test ./..., CGO_ENABLED=1 go test -race ./cmd/polly ./cmd/polly/internal/style ./cmd/polly/internal/markdown, .github/ci.sh cross. Record exit codes and quote any failure with its command.
  2. Walk acceptance criteria 1-10 of docs/features/theme-engine.md one at a time with concrete evidence: (1) polly --help shows both flags with their variables; (2) a user ~/.pollytool/themes/<name>.json overriding only accent changes only accent-colored text, an omitted role keeps today's appearance, inherit renders in the terminal default foreground; (3) with no theme active, fenced-code and diff rendering is byte-identical to the pre-change baseline for RESOLVED COLORS (markup names legitimately changed to syn-*); (4) bash and generic tool output are highlighted with the fence token map; (5) a light layer renders under COLORFGBG + --theme-variant auto, and a theme with no light layer renders colors in both; (6) editing the active theme file while the TUI runs applies on the next tick with no restart and no stale colors anywhere (transcript, orbit frame, scrollbar, bird, code blocks); (7) the set_theme persist matrix (session-only / confirmation_required / written and survives restart / overwrite required); (8) the theme skill exists, is listed, activates, and references no repository paths; (9) a true-color role emits 38;2;r;g;b under COLORTERM=truecolor and 38;5;N otherwise, and a palette-only theme's -p output is unchanged; (10) this gate plus before/after TUI screenshots.
  3. Screenshots: D=.agents/skills/polly-tui/driver.sh, then $D start / $D settle 5 / $D shot before / $D stop, then $D start --theme-variant light (more reliable than tmux's COLORFGBG) / $D settle 5 / $D shot after / $D stop. Arguments after start are appended to the polly command line, so --theme <abs path> works. Put fixture themes under $POLLY_SHOT_HOME/.pollytool/themes/ (default $HOME/.cache/polly-tui-home) - the driver's env-forward list applies only to the wezterm backend and does not include POLLYTOOL_THEME. The driver builds ./polly at the repo root (gitignored); delete it before any commit and never commit it.
  4. Report per-criterion PASS/FAIL with evidence, and explicitly list any criterion that could not be exercised and why. Prefer a simulation screen (affordanceTestREPL/chromeTestREPL) for cache assertions and reserve the tmux driver for the visual before/after.

  Environment caveats to record: this worktree's sandbox denies the default module cache and the real home, so the checks need GOMODCACHE=$TMPDIR/gomodcache GOPROXY=https://proxy.golang.org,direct GOSUMDB=off and a writable home - cmd/polly's TestMain (main_test.go:13-26) creates .polly-test-home-* under the real home and panics without one. gofmt -l ., CGO_ENABLED=0 go build ./..., go vet ./..., and go test -count=1 ./cmd/polly/internal/style/... ./cmd/polly/internal/markdown/... all ran green on the baseline snapshot with that redirect.

### Docs updates

- `README.md:84-86` — drop the false "no theme flag or environment variable" claim; leave only the fixed amber approval border (and image rendering) as not themable.
- `README.md:256-262` — add `/theme [name]` to the fixed-width slash-command block and reflow.
- `README.md:553-558` — add `set_theme` to the built-in tools list, noting persistence needs a separate confirmation call.
- `README.md:626-632` — name the theme builtin skill alongside feature-workflow.
- `README.md` — new `## Themes` section (roles, value forms, file shape, precedence, detection, failure behaviour, hot reload, `/theme` vs `set_theme`, line-frontend SGR rules, `NO_COLOR`, sandbox reach).
- `docs/API.md:705-708` — add `set_theme` to the ChildRegistry deny-list sentence.
- `AGENTS.md` — optional: `~/.pollytool/themes` next to the runtime-state gotcha; the builtin theme skill must not reference the repository.
- `docs/SANDBOX.md` — optional cross-reference that `~/.pollytool/themes` is not a home read grant.
- This file — status line updated and this plan appended.

### Risks

- **Concurrency (highest).** `ui.StyleParserColorMap` is an unsynchronized map read by gotui's parser on the draw path, and polly parses cells off the event loop too. Writing the map or bumping the epoch from a tool goroutine is a fatal concurrent-map-write. Every apply happens on the event loop; the race job for `./cmd/polly` is load-bearing.
- **`invalidate()` alone is insufficient.** `transcriptRows` returns cached rows when `valid && fits` and reuses per-block rows when its changed predicate says nothing changed, so the epoch must enter that predicate or colors stay stale after every apply.
- **Two resolved-cell caches were missing from the spec, and both are real bugs:** `markdown/table.go`'s `paletteColorNames` (stale inverse map re-encodes a color under a name that now means something else) and `repl_agents.go`'s `agentLinkStyle` (exact-equality click hit testing, so agent links stop being clickable after any theme change).
- **`inherit` interacts with existing heuristics.** With `accent == ui.ColorClear` the link hit test degenerates; `wrap.go:159-181` identifies the user/code gutters by comparing the first cell's Fg to accent/muted. Resolved by rejecting `inherit` on `accent` and `muted` (see Open questions).
- **The idle TUI does not repaint.** The 50 ms ticker only calls `render()` when `needsTick()`; the watcher must carry its own throttle and force a repaint.
- **`userConfigValue` caches the config file for the process lifetime**, cleared only by `writeUserConfig`, so a naive config-tier watcher never sees a manual edit; a `readUserConfig`-based re-read is required.
- **Names in the same map as values.** A theme value naming a polly role would be self-referential; resolution against a pre-polly snapshot makes such values `INVALID_COLOR`.
- **Existing tests pin the semantic markup literals** (`fg:accent`, `fg:muted`, `fg:ok`, `fg:err`) that the `syn-*` rename changes; roughly 24 assertions must be updated by design, not weakened.
- **`ui.StyleParserColorMap` is global to the test binary,** so any `cmd/polly` test that applies a theme can break unrelated tests; tests must restore the map.
- **Verification environment.** The implementation slots deny the default module cache and the real home, so `./cmd/polly` and `./...` cannot be fully exercised there; the parent must run the full gate (done for the baseline: build/vet/fast tests/local-ci green, with the two sandbox-limited `cmd/polly` failures noted above).
- **Cross-build.** `.github/ci.sh cross` includes windows/amd64, so theme file discovery and stat-polling must stay portable; no platform-split files should be needed.
- **Do not bump tcell.** `go.mod` pins v3.0.5 while gotui v5.0.3 requests v3.5.0, which removes `tcell.NewSimulationScreen` — the entire `cmd/polly` TUI test harness.
- **Preset RGB values** must be pinned before the README can document them (resolved below).

### Open questions (with parent resolutions for the gate)

1. **Shadowing.** Resolved: a user theme in the themes dir wins over a same-named preset, except `default`, which is reserved because every load failure falls back to it. `/theme` shows which file won and refuses to persist `default`.
2. **Preset colors.** Proposed (veto-able, and the implementer must record the final values in `cmd/polly/theme.go`): `dark` — accent `#8ab4f8`, muted `#9aa0a6`, ok `#81c995`, err `#f28b82`, run `#78d9ec`, active `#fdd663`, code `#e8eaed`; `light` — accent `#1a73e8`, muted `#5f6368`, ok `#188038`, err `#d93025`, run `#007b83`, active `#b06000`, code `#202124`; bird roles unchanged in both; unset `syn-*` roles fall back.
3. **Unknown `--theme-variant` value.** Resolved: coerce to `auto` with the startup notice; no flag Validator (matches criterion 5's "never a fatal error").
4. **Language selection for generic tool output.** Resolved: `lexers.Analyse` with a plain-`code` fallback when the result is nil or `lexers.Fallback`; acceptance 3's byte-identical claim is scoped to fences/diffs under the default theme.
5. **A theme value naming a polly role.** Resolved: `INVALID_COLOR` via the pre-registration snapshot.
6. **`inherit` on `accent`/`muted`/bird.** Resolved: rejected for `accent` and `muted` (the link hit test and gutter heuristic depend on them); allowed for the bird, which renders monochrome in the terminal foreground rather than invisible.
7. **Load-error notice and `--quiet`.** Resolved: respect `--quiet`; before the TUI starts it goes to stderr, after that through `appendNoticeLine`.
8. **`/theme` in the line frontend.** Resolved: reports unavailable (no hot reload there); completion enumerates builtin and user theme names.
9. **`palette:N` bounds.** Resolved: out of range is a load error; `palette:0` (the terminal's black slot) is distinct from an omitted key (polly's built-in mapping).
10. **Docs location.** Resolved: a README `## Themes` section, no new `docs/THEMES.md`.
11. **This file.** Resolved: keep it, status updated, plan appended (it is the feature's durable memory).

Remaining open question for the gate: items 2 and 6 change visible behavior (preset identity, and rejecting some themes as invalid) — approve or amend them before phase 3.

---

## Outcome

All 11 tasks are integrated into the working tree (working files only — nothing
committed, staged or published). Waves as planned: 1) `style-theme-core`,
`tool-output-highlighting`, `line-color-capabilities`, `theme-builtin-skill`;
2) `markdown-token-roles`, `theme-selection-and-flags`,
`theme-epoch-invalidation`; 3) `theme-apply-reload-command`;
4) `set-theme-tool`; 5) `readme-themes-docs`; 6) `acceptance-verification`.

New files: `cmd/polly/internal/style/theme.go` (+test), `cmd/polly/theme.go`
(+test), `cmd/polly/repl_theme.go` (+test), `cmd/polly/repl_theme_epoch.go`
(+test), `cmd/polly/theme_tool.go` (+test), `cmd/polly/tool_output_lang.go`
(+test), `skills/builtin/theme-designer/SKILL.md`. Modified: the style/markdown/line
packages, `config.go`, `types.go`, `polly.go`, `repl_loop.go`,
`repl_commands.go`, `repl_transcript.go`, `repl_typewriter.go`,
`repl_child_views.go`, `repl_agents.go`, `repl_ui.go`, `repl_view.go`,
`subagent/subagent.go`, `docs/API.md`, `README.md`, `AGENTS.md`.

Parent verification (macOS, repo-local module cache, `HOME` under `$TMPDIR`):

- `gofmt -l $(git ls-files '*.go')` clean; `CGO_ENABLED=0 go build ./...` and
  `go vet ./...` exit 0.
- `go test ./cmd/polly/internal/style/... ./cmd/polly/internal/markdown/...
  ./skills/... ./subagent/...` all ok.
- `go test ./cmd/polly/...` shows only the two pre-existing environment
  failures (`TestOneShotTerminalContract`: PTY allocation denied;
  `TestCLISignalExitStatusAndStderr`: reading `/Users/alex` denied) — identical
  to the pre-change baseline.
- `CGO_ENABLED=1 go test -race ./cmd/polly/...` → zero data races (the
  load-bearing check for the process-global parser map).
- `python3 -B -m unittest discover -s .github/local-ci -p 'test_*.py'` → 33
  tests OK.
- `polly --help` lists `--theme` and `--theme-variant` with their
  `[$POLLYTOOL_*]` variables.
- End-to-end load path: `--theme /nonexistent/theme.json` prints
  `polly: /nonexistent/theme.json: read theme file: …` and the run continues.
- Spot checks in the merged code: `set_theme` posts through
  `ThemeChanged`/`postUI` (never writes the parser map off-loop); the
  transcript reuse predicate compares `c.epoch == style.Epoch()`; the 50 ms
  ticker arm repaints when `pollTheme` reports a change even while idle;
  `set_theme` is in the `ChildRegistry` deny list; line output emits
  `38;5;N` and `38;2;r;g;b` with a nearest-256 degradation.

Environment limits, unexercised here and not features' faults:

- **TUI before/after screenshots.** tmux cannot start in this sandbox
  (`error connecting to /private/tmp/tmux-501/default: Operation not
  permitted`) and PTY allocation is denied (`out of pty devices`), so the
  `polly-tui` driver cannot run. Acceptance criterion 10's visual half is
  unverified by execution; its mechanical half (the gate above) passed.
- **`./tools/sandbox`** fails with `sandbox-exec: sandbox_apply: Operation not
  permitted` (nested sandboxing denied). No file under `tools/` was touched by
  this feature.
- The worker sandbox has a much larger version of the same problem: 35
  `./cmd/polly` tests fail there at baseline because slot paths live under the
  real home and `safefile` opens every ancestor directory.

Process findings worth keeping: the first implementation run blocked because a
wave's checks demanded a green `./cmd/polly` in an environment that cannot be
green — fixed by comparing the failure set against the recorded baseline list;
a second run died on a transient provider `connection reset by peer`. Both were
recovered by validating the retained integration candidate in the parent and
applying it, rather than redoing the wave.
