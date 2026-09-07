package main

import (
	"fmt"
	"strings"

	rw "github.com/mattn/go-runewidth"
)

type sampleLine struct{ text, tone string }

// Fixed sample data keeps scroll position separate from animation time and
// gives every style the same content to protect, wrap, and clip.
func sampleRows(pane, width int) []sampleLine {
	var source []sampleLine
	if pane == 0 {
		source = []sampleLine{
			{"YOU", "heading"},
			{"Let's make room for a different kind of workspace.", "text"},
			{"", ""},
			{"POLLY", "heading"},
			{"I have opened Scout alongside this conversation. The frame can carry the activity while the words stay still.", "text"},
			{"", ""},
			{"› agents  1 running · 2 complete", "accent"},
			{"  scout      exploring window chrome", "text"},
			{"  maker      sample transcript ready", "muted"},
			{"  reviewer   checking small sizes", "muted"},
			{"", ""},
			{"A FEW THINGS TO TRY", "heading"},
			{"Scroll this pane without moving the other one. Drag the divider until the long title has to shorten. Pull the bottom edge up to make the scroll thumb smaller.", "text"},
			{"", ""},
			{"Use Tab to move focus, or click inside a pane. The inactive frame should feel quieter without making the transcript difficult to read.", "text"},
			{"", ""},
		}
	} else {
		source = []sampleLine{
			{"SCOUT", "heading"},
			{"Tracing the frame, one detail at a time.", "text"},
			{"", ""},
			{"› read_file  repl_inspector_frame.go", "accent"},
			{"  A thin divider separates the transcripts.", "muted"},
			{"  The main composer stays below the split.", "muted"},
			{"", ""},
			{"› read_file  repl_inspector_header.go", "accent"},
			{"  polly › scout › tool 2/8", "text"},
			{"  A breadcrumb gives the window a place.", "muted"},
			{"", ""},
			{"DESIGN NOTES", "heading"},
			{"Let the title carry identity. Let the border carry focus. Give movement a home outside the reading surface.", "text"},
			{"", ""},
			{"The scroll thumb represents the visible fraction of this transcript. Grab it, or click above and below it to page.", "text"},
			{"", ""},
		}
	}
	notes := []string{
		"Keep the close and maximize controls visible even when a title becomes long.",
		"Resize grips can be structural: a folded corner, a caliper, a bevel, a mooring point.",
		"A quiet inactive frame helps the focused pane stand out.",
		"Attention should read as a state, even with motion paused or a limited color palette.",
		"Scrollbars should describe position, not animation time. Keep their thumbs steady.",
		"Animation can travel along an edge without moving a single line of conversation.",
		"At narrow widths, show one pane. Keep its own scroll position when switching back.",
		"When work completes, let the motion settle and leave a clear receipt.",
	}
	for i, note := range notes {
		source = append(source, sampleLine{fmt.Sprintf("%02d / OBSERVATION", i+1), "accent"}, sampleLine{note, "text"}, sampleLine{"", ""})
	}
	source = append(source, sampleLine{"END OF SAMPLE", "heading"}, sampleLine{"The experiment is ready for another direction.", "muted"})
	var rows []sampleLine
	for _, line := range source {
		for _, text := range wrapWords(line.text, max(1, width)) {
			rows = append(rows, sampleLine{text, line.tone})
		}
	}
	return rows
}

func wrapWords(text string, width int) []string {
	if text == "" {
		return []string{""}
	}
	indent := text[:len(text)-len(strings.TrimLeft(text, " "))]
	indent = indent[:min(len(indent), max(0, width-1))]
	var rows []string
	line := indent
	for _, word := range strings.Fields(text) {
		if line != indent && rw.StringWidth(line)+1+rw.StringWidth(word) > width {
			rows = append(rows, line)
			line = indent
		}
		if line != indent {
			line += " "
		}
		parts := strings.Split(rw.Wrap(word, max(1, width-len(indent))), "\n")
		for n, part := range parts {
			if n > 0 {
				rows = append(rows, line)
				line = indent
			}
			line += part
		}
	}
	return append(rows, line)
}
