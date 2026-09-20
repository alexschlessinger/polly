package main

// The style epoch: what a theme change must drop, and what it must leave alone.
//
// style.Apply re-registers every role in ui.StyleParserColorMap and bumps the
// process-wide epoch (cmd/polly/internal/style/theme.go). Caches built from the
// previous table keep painting the previous theme until they are dropped or
// re-validated, and the invalidate() calls that already exist are not enough:
// the transcript's row rebuild reuses any block whose key, text, cells, and
// images are unchanged (repl_transcript.go), so a cache that was merely marked
// stale comes straight back. The epoch therefore enters each cache's *reuse*
// decision, and the caches that hold markup naming roles rather than resolved
// cells are left alone on purpose.

// applyStyleEpoch drops the resolved-color caches of the model on screen after
// style.Apply bumped the style epoch. It is the single entry point for a theme
// change (the reload watcher, /theme, and the set_theme tool all reach it
// through applyTheme in repl_theme.go).
//
// It must run on the TUI event loop with r.model.mu held.
// ui.StyleParserColorMap is an unsynchronized global that gotui reads lock-free
// on the draw path while other goroutines parse cells, so an apply — and
// therefore this call — may never run off the loop.
//
// Only r.model and this REPL's image layer are written here. Every other
// model that holds resolved cells (the tabs in the background, the inspector's
// open view, the cached child and main projections) is painted under its own
// lock and must not have its caches written from this one: a background tab's
// turn is appending to its model at the same time. Those need no write,
// because each re-parses on its own next paint:
//
//   - transcriptVisualCache.fits compares c.epoch against style.Epoch()
//     (repl_transcript.go), which covers every model that reaches
//     transcriptRows — a promoted child or main projection included.
//   - assistantTypewriter.update re-parses its cells when the epoch it recorded
//     changed (repl_typewriter.go), so a mid-stream theme change shows on the
//     next frame instead of waiting for another provider chunk.
//   - agentLinkStyle resolves on every call (repl_agents.go): agentDetail finds
//     clickable cells by exact style equality, so a cached accent would silently
//     stop matching freshly parsed links.
//   - The orbit frame needs nothing either: refreshChrome re-resolves every
//     perimeter cell's base style from chromeColor on each paint
//     (repl_chrome.go), and the scrollbar thumb does the same in its own Draw.
//
// epoch is the value style.Apply returned, the pinned cross-task signature of
// the apply path. The caches compare it against style.Epoch() themselves, so it
// is not stored here.
func (r *managedREPL) applyStyleEpoch(epoch uint64) {
	if r == nil || r.model == nil {
		return
	}
	m := r.model
	// invalidate() also advances the revision, so a retired main projection
	// cannot be restored onto this tab as an unchanged revision.
	m.visual.invalidate()
	// Code caches hold highlighted lines, which are role-named markup and
	// therefore epoch-safe; dropping them is cheap and keeps every derived
	// artifact in step with the table.
	for i := range m.transcript {
		m.transcript[i].codeCache = nil
	}
	m.streamCodeCache = nil
	// Terminal images are drawn over cells the manager locks for as long as
	// they stay put, so an unchanged frame never repaints the background a
	// transparent image shows through: the masthead logo would keep the
	// previous theme's background around it. Dropping the placements makes
	// the next frame repaint those cells and draw the images over them again.
	r.images.Invalidate()
}
