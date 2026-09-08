# Window lab

A standalone gotui/tcell experiment branching from Polly's split inspector:
breadcrumb title, contextual chrome, close control, thin divider, and a shared
composer below the panes. Twelve studies explore borders, title bars, animation,
scrollbars, and resizing. All content is sample data; no providers or sessions.

Run from the repository root:

~~~sh
go run ./experiments/windowfx
go run ./experiments/windowfx --view split --style aurora
~~~

Start with a terminal around **132 columns × 48 rows** for six studies at once.
The gallery adapts to smaller sizes. Split preview switches to one pane below
92 columns; Tab selects the other pane. The minimum is 72 × 28.

| Style slug | Border / title | Scrollbar |
| --- | --- | --- |
| split | Current divider and breadcrumb vocabulary | Plain rail |
| halo | Quiet etched frame with an orbiting glint | Fine dotted track |
| aurora | Rounded violet / ice / mint ribbon | Slim luminous thumb |
| phosphor | Green double-line CRT with scanning caption | Shaded phosphor blocks |
| workbench | Raised bevel, blue caption, chunky controls | Elevator with grip marks |
| paper | Warm paper, an index tab, moving underline | Red bookmark |
| circuit | Routed packets that turn the frame's corners | Dotted signal rail |
| tide | Open horizon and rippling lower edge | Floating buoy |
| blueprint | Drafting ticks and live dimension labels | Caliper |
| marquee | Amber chase lights and a ticket title | Perforated rail |
| deco | Gold inlay and centered nameplate | Twin-line brass thumb |
| radar | Open brackets with sweeping edge telemetry | Range-finder ticks |

## Try the interactions

In the gallery, click a study or select it with Left/Right and press Enter.
Brackets [ / ] move by a gallery page; 1–9 and 0 select the first ten studies.
Left/Right also cycle all twelve styles in split preview.

In split preview:

- **Scroll:** wheel over either pane, or Up/Down, Page Up/Down, Home/End on the
  focused pane. Grab the scrollbar thumb; click its track to page.
- **Resize:** drag the middle divider horizontally. Drag a bottom edge or its
  corner grip vertically. [ / ] resize the split; { / } resize height.
  The Blueprint title reports the resulting cell dimensions.
- **Focus:** Tab or click inside a pane. Each pane keeps its own scroll position.
- **Window controls:** click [+] to maximize/restore and [x] to close a pane.
  z and x do the same for the focused pane; i restores both.
- **Compare:** b toggles the original border/title vocabulary; g or Enter
  returns to the gallery. The reference uses the lab's sample layout and added
  controls, not a pixel-exact rendering of the production inspector.

Everywhere: s cycles **working → attention → complete → idle**, Space pauses,
+ / - change speed, c toggles RGB / ANSI colors, r resets time, scroll and
geometry, and q / Escape exits. Complete and idle settle the animated accents.
Transcript text and scrollbar positions never move with the animation clock.

## Export previews

PNG and GIF exports render the actual cell buffer without opening a terminal:

~~~sh
go run ./experiments/windowfx --snapshot /tmp/windowfx-gallery-1.png
go run ./experiments/windowfx --style circuit --snapshot /tmp/windowfx-gallery-2.png
go run ./experiments/windowfx --style aurora --view split --gif /tmp/windowfx-aurora.gif
go run ./experiments/windowfx --style paper --view split --snapshot /tmp/windowfx-paper.png
go run ./experiments/windowfx --style radar --state attention --gif /tmp/windowfx-radar.gif
~~~

--width / --height set export dimensions; --at selects the PNG animation
time; --seconds sets GIF duration (default 6). --still starts paused,
--palette uses 16 ANSI colors, and --list lists the style slugs.

Exports use local Menlo / Apple Symbols when available, with embedded Go Mono
fallback. Fonts, glyph coverage, bold rendering, and ANSI colors depend on the
terminal; the live terminal remains the visual authority. The experiment draws
at 30 fps during activity and waits for input when paused or settled.

The code stays under this directory. A production integration would also need
to connect real inspector state, search/header wrapping, transcript scroll
anchors, image placement, and its existing input routing.
