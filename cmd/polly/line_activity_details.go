package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

// Only requested details retain text. The normal activity model holds counters
// and short live labels, never tool results or child answer bodies.
type lineActivityDetails struct {
	thought       reasoningRecord
	tools         []*lineToolDetail
	earlierTools  int
	images        []string
	earlierImages int
	emitted       bool
}

type lineToolDetail struct{ label, line string }

func (d *lineActivityDetails) appendThought(chunk string, segmentBreak bool) {
	// The helper has no model dependencies; use the same bounded tail and
	// segment rules as the TUI, then the same five-line preview at settlement.
	appendReasoningTail(&d.thought, chunk, segmentBreak)
}

// lineActivityToolRows bounds the tool rows a one-shot activity summary keeps.
const lineActivityToolRows = 6

func (d *lineActivityDetails) startTool(call messages.ChatMessageToolCall) *lineToolDetail {
	t := &lineToolDetail{label: cleanActivityText(toolLabel(call))}
	if len(d.tools) == lineActivityToolRows {
		copy(d.tools, d.tools[1:])
		d.tools = d.tools[:len(d.tools)-1]
		d.earlierTools++
	}
	d.tools = append(d.tools, t)
	return t
}

func (t *lineToolDetail) finish(_ messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	meta := resultLineMeta(result)
	outcome := toolActivityOutcome(toolWasDenied(result), err)
	if outcome != "done" {
		parts := []string{outcome}
		if exit := toolFailureMeta(err); exit != "" {
			parts = append(parts, exit)
		}
		if meta != "" {
			parts = append(parts, meta)
		}
		meta = strings.Join(parts, " · ")
		t.line = toolErrorLine(t.label, formatElapsed(duration), meta)
	} else {
		t.line = toolOKLine(t.label, formatElapsed(duration), meta)
	}
}

const lineImageDetailLimit = 64

func (d *lineActivityDetails) addImages(images []style.Image) {
	for _, img := range images {
		if len(d.images) == lineImageDetailLimit {
			d.earlierImages++
			continue
		}
		d.images = append(d.images, style.Truncate(cleanActivityText(style.ImageCaptionText(img)), 512))
	}
}

func (ui *lineTurnUI) finishDetailsLocked(completion turnCompletion) {
	a := ui.activity
	if a == nil || a.details == nil || a.details.emitted || ui.config.Quiet {
		return
	}
	d := a.details
	d.emitted = true
	width := unboundedStatusWidth
	if a.caps.live {
		width = max(1, ui.statusColumnsLocked()-4)
	}
	print := func(markup string) { ui.statusLineLocked(styledMarkupToLine(markup, a.caps.color)) }
	heading := func(text string) { print("  " + style.Styled(text, "accent", "")) }
	if len(d.thought.tail) > 0 {
		heading("Thought")
		for _, line := range reasoningTailLines(string(d.thought.tail), width, reasoningPreviewLines) {
			print(reasoningBlockIndent + style.Styled(cleanActivityText(line), "muted", "italic"))
		}
	}
	if len(d.tools) > 0 {
		heading("Tools")
		earlier, rows := d.earlierTools, d.tools
		if earlier > 0 && len(rows) == lineActivityToolRows {
			rows, earlier = rows[1:], earlier+1
		}
		if earlier > 0 {
			print("  " + style.Styled(fmt.Sprintf("… %d earlier", earlier), "muted", ""))
		}
		for _, row := range rows {
			line := row.line
			if line == "" {
				line = toolErrorLine(row.label, "", turnOutcomeLabel(completion.outcome()))
			}
			print(line)
		}
	}
	if len(a.launches) > 0 {
		heading("Agents")
		for _, launch := range a.launches {
			status := launch.status
			if launch.active {
				status = "canceled"
			}
			parts := []string{launch.label, status}
			if launch.thought > 0 {
				parts = append(parts, reasoningDisclosureLabel(false, false, launch.thought))
			}
			if launch.tools > 0 {
				parts = append(parts, turnToolLabel(launch.tools))
			}
			if launch.images > 0 {
				parts = append(parts, turnImageLabel(launch.images))
			}
			if f, ok := turnTokenField(launch.in, launch.out); ok {
				parts = append(parts, f.raw)
			}
			print("    " + style.Styled(strings.Join(parts, " · "), "muted", ""))
		}
	}
	if len(d.images) > 0 {
		heading("Images")
		for _, caption := range d.images {
			print("    " + style.Styled(caption, "muted", ""))
		}
		if d.earlierImages > 0 {
			print("    " + style.Styled(fmt.Sprintf("… %d more receipts", d.earlierImages), "muted", ""))
		}
	}
}
