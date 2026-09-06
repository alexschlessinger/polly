package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
	"golang.org/x/term"
)

// Status capabilities belong to stderr, independently of the answer on stdout.
// Color is optional; NO_COLOR does not disable cursor updates.
type lineStatusCapabilities struct {
	live    bool
	color   bool
	columns int
}

func resolveLineStatusCapabilities(tty bool, columns int, getenv func(string) string) lineStatusCapabilities {
	live := tty && !strings.EqualFold(strings.TrimSpace(getenv("TERM")), "dumb")
	return lineStatusCapabilities{live: live, color: live && getenv("NO_COLOR") == "", columns: max(1, columns)}
}

type lineActivityItem struct {
	scope, id, label string
	started          time.Time
	agent            bool
}

// All state and writes are protected by lineTurnUI.toolMu, including the timer.
// The live area owns only the current line, never prior scrollback or images.
type lineActivity struct {
	caps                     lineStatusCapabilities
	started                  time.Time
	phase, outcome           string
	lastText                 string
	imageCaps                outputCapabilities
	active                   []lineActivityItem
	tools, agents, images    int
	failed, denied           int
	in, out, used, limit     int
	estimated                bool
	visible, paused, stopped bool
	nextScope                int
	ticks                    int
	cancel, done             chan struct{}
}

func cleanActivityText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, stripTranscriptImageMarkers(s))
}

func (ui *lineTurnUI) activityEnabled() bool { return !ui.config.Quiet }

func (ui *lineTurnUI) startActivityLocked() {
	if !ui.activityEnabled() {
		return
	}
	columns := 80
	if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 {
		columns = width
	}
	ui.activity = &lineActivity{
		caps:    resolveLineStatusCapabilities(ui.stderrTTY, columns, os.Getenv),
		started: time.Now(), phase: "Working",
		imageCaps: resolveOutputCapabilities(conversationModeOneShot, false, ui.stderrTTY, columns, os.Getenv),
	}
	a := ui.activity
	ui.renderActivityLocked()
	if !a.caps.live {
		ui.activityLineLocked("Working")
		return
	}
	a.cancel, a.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(a.done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-a.cancel:
				return
			case <-ticker.C:
				ui.toolMu.Lock()
				if !a.stopped {
					a.ticks++
					ui.renderActivityLocked()
				}
				ui.toolMu.Unlock()
			}
		}
	}()
}

func (ui *lineTurnUI) clearActivityLocked() {
	if a := ui.activity; a != nil && a.visible {
		fmt.Fprint(ui.errWriter, "\r\x1b[2K")
		a.visible = false
	}
}

func (ui *lineTurnUI) renderActivityLocked() {
	a := ui.activity
	if a == nil || a.stopped || a.paused || ui.prompting || !a.caps.live || (ui.stdoutTTY && ui.contentPrinted && !ui.endsWithNewline) {
		return
	}
	columns := a.caps.columns
	if ui.stderrTTY {
		if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 {
			columns = width
		}
	}
	parts := []string{a.phase, formatElapsed(time.Since(a.started))}
	if a.tools > 0 {
		parts = append(parts, turnToolLabel(a.tools))
	}
	if a.agents > 0 {
		running := 0
		for _, item := range a.active {
			if item.agent {
				running++
			}
		}
		parts = append(parts, fmt.Sprintf("%d/%d agents running", running, a.agents))
	}
	if a.images > 0 {
		parts = append(parts, turnImageLabel(a.images))
	}
	if len(a.active) > 0 {
		// Rotate through concurrent work slowly enough to read each label.
		item := a.active[(a.ticks/10)%len(a.active)]
		parts = append(parts, item.label+" · "+formatElapsed(time.Since(item.started)))
	}
	text := truncate(cleanActivityText(strings.Join(parts, " · ")), max(0, columns-1))
	if columns < 5 {
		text = ""
	}
	if a.visible && text == a.lastText {
		return
	}
	fmt.Fprintf(ui.errWriter, "\r\x1b[2K%s", ui.activityColorLocked(text))
	a.visible = true
	a.lastText = text
}

// Use the same theme-relative palette and style conversion as the TUI.
func (u *lineTurnUI) activityColorLocked(text string) string {
	if u.activity == nil || !u.activity.caps.color {
		return text
	}
	parts := strings.Split(text, " · ")
	var out bytes.Buffer
	for i, part := range parts {
		if i > 0 {
			out.WriteString(" · ")
		}
		role, modifier := "muted", ""
		switch {
		case strings.Contains(part, "✗"), part == "Stopped", strings.HasSuffix(part, " failed"), strings.HasSuffix(part, " denied"), strings.HasSuffix(part, " unfinished"):
			role, modifier = "err", "bold"
		case part == "Done":
			role, modifier = "ok", "bold"
		case part == "Thinking", part == "Incomplete", strings.HasPrefix(part, "Warning:"):
			role, modifier = "active", "bold"
		case part == "Working":
			role, modifier = "run", "bold"
		case part == "Writing", strings.Contains(part, "tool"), strings.Contains(part, "agent"), strings.Contains(part, "image"):
			role = "accent"
		}
		appendANSIStyledCells(&out, parseStyledCells(styled(part, role, modifier), ui.StyleClear))
	}
	return out.String()
}

func (ui *lineTurnUI) pauseActivity() {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.clearActivityLocked()
	if ui.activity != nil {
		ui.activity.paused = true
	}
}

func (ui *lineTurnUI) activityLineLocked(text string) {
	ui.clearActivityLocked()
	// Only terminate stdout when it shares the terminal. Redirected answers
	// must not acquire newlines from asynchronous status events.
	if ui.stdoutTTY && ui.contentPrinted && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
	fmt.Fprintln(ui.statusWriterLocked(), ui.activityColorLocked(cleanActivityText(text)))
}

// An open approval prompt owns the terminal: notices raised meanwhile (a
// sibling's failure, an image caption) queue here and replay once it is
// answered, so they never land on the prompt line.
func (ui *lineTurnUI) statusWriterLocked() io.Writer {
	if ui.prompting {
		return &ui.pending
	}
	return ui.errWriter
}

func (ui *lineTurnUI) activityPhaseLocked(phase string) {
	if a := ui.activity; a != nil && !a.stopped && a.phase != phase {
		ui.clearActivityLocked()
		a.phase = phase
		if !a.caps.live {
			ui.activityLineLocked(phase)
		}
	}
}

func (ui *lineTurnUI) activityToolsLocked(scope, prefix string, calls []messages.ChatMessageToolCall) {
	a := ui.activity
	for _, call := range calls {
		label := prefix + toolLabel(call)
		if call.Name == "spawn_agent" {
			label = prefix + lineAgentName(call, "agent")
		}
		if a != nil && !a.stopped {
			agent := call.Name == "spawn_agent"
			if agent {
				a.agents++
			} else {
				a.tools++
			}
			a.active = append(a.active, lineActivityItem{scope: scope, id: call.ID, label: label, started: time.Now(), agent: agent})
		}
	}
}

func (ui *lineTurnUI) activityToolEndLocked(scope, prefix string, call messages.ChatMessageToolCall, result string, _ time.Duration, err error) {
	if a := ui.activity; a != nil {
		for i, item := range a.active {
			if item.scope == scope && item.id == call.ID {
				a.active = append(a.active[:i], a.active[i+1:]...)
				break
			}
		}
	}
	// Successful work only updates counts. Never leave a per-call transcript
	// (including child calls or result-line metadata) in normal scrollback/logs.
	label := prefix + toolDisplayName(call.Name)
	if toolWasDenied(result) {
		if ui.activity != nil {
			ui.activity.denied++
		}
		ui.activityLineLocked("  ✗ " + label + " · denied")
	} else if err != nil {
		if ui.activity != nil {
			ui.activity.failed++
		}
		meta := toolFailureMeta(err)
		if meta == "" {
			meta = "failed"
		}
		ui.activityLineLocked("  ✗ " + label + " · " + meta)
	}
}

func lineAgentName(call messages.ChatMessageToolCall, fallback string) string {
	var args struct {
		Label string `json:"label"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) == nil && strings.TrimSpace(args.Label) != "" {
		return truncate(cleanActivityText(args.Label), 40)
	}
	return fallback
}

// Called before machine metadata is written, so no repaint can corrupt it.
func (ui *lineTurnUI) SetTurnOutcome(reason messages.StopReason, err error) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if a := ui.activity; a != nil {
		ui.clearActivityLocked()
		if ui.config.SchemaPath == "" {
			ui.flushBufferedMarkdown()
		}
		a.outcome = "Done"
		if reason == messages.StopReasonMaxTokens || reason == messages.StopReasonMaxIterations {
			a.outcome = "Incomplete"
		} else if err != nil {
			a.outcome = "Stopped"
		}
		ui.finishActivityLocked()
	}
}

// Children keep their replies private while forwarding attributed activity to
// the same serialized renderer. Scope IDs distinguish reused provider call IDs.
type lineChildActivity struct {
	ui                                        *lineTurnUI
	scope, parentScope, callID, prefix, label string
}

type lineChildActivityHost interface {
	childActivity(messages.ChatMessageToolCall) *lineChildActivity
}

func (ui *lineTurnUI) childActivity(call messages.ChatMessageToolCall) *lineChildActivity {
	return ui.newChildActivity("", "", call)
}

func (ui *lineTurnUI) newChildActivity(parentScope, prefix string, call messages.ChatMessageToolCall) *lineChildActivity {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.activity == nil || ui.activity.stopped {
		return nil
	}
	ui.activity.nextScope++
	label := prefix + lineAgentName(call, fmt.Sprintf("agent %d", ui.activity.nextScope))
	return &lineChildActivity{ui: ui, scope: fmt.Sprint(ui.activity.nextScope), parentScope: parentScope, callID: call.ID, prefix: label + ": ", label: label}
}

func (c *lineChildActivity) phase(phase string) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	for i := range c.ui.activity.active {
		item := &c.ui.activity.active[i]
		if item.scope == c.parentScope && item.id == c.callID {
			item.label = c.label + " · " + phase
		}
	}
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) tools(calls []messages.ChatMessageToolCall) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.ui.clearActivityLocked()
	c.ui.activityToolsLocked(c.scope, c.prefix, calls)
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) toolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.ui.activityToolEndLocked(c.scope, c.prefix, call, result, duration, err)
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) media(images []transcriptImage) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.ui.activity.images += len(images)
	for _, img := range images {
		c.ui.activityLineLocked("    " + c.prefix + transcriptImageCaptionText(img))
	}
	c.ui.renderActivityLocked()
}

func (ui *lineTurnUI) finishActivityLocked() {
	a := ui.activity
	if a == nil || a.stopped {
		return
	}
	ui.clearActivityLocked()
	a.stopped = true
	if a.cancel != nil {
		close(a.cancel)
	}
	if a.outcome == "" {
		a.outcome = "Stopped"
		if ui.finished {
			a.outcome = "Done"
		}
	}
	parts := []string{a.outcome, formatElapsed(time.Since(a.started))}
	if a.tools > 0 {
		parts = append(parts, turnToolLabel(a.tools))
	}
	if a.agents > 0 {
		parts = append(parts, fmt.Sprintf("%d agents", a.agents))
	}
	if a.images > 0 {
		parts = append(parts, turnImageLabel(a.images))
	}
	if a.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", a.failed))
	}
	if a.denied > 0 {
		parts = append(parts, fmt.Sprintf("%d denied", a.denied))
	}
	if len(a.active) > 0 {
		parts = append(parts, fmt.Sprintf("%d unfinished", len(a.active)))
	}
	if a.in > 0 || a.out > 0 {
		parts = append(parts, fmt.Sprintf("%d in / %d out", a.in, a.out))
	}
	if a.limit > 0 {
		prefix := ""
		if a.estimated {
			prefix = "~"
		}
		parts = append(parts, fmt.Sprintf("ctx %s%d/%d", prefix, a.used, a.limit))
	}
	ui.activityLineLocked(strings.Join(parts, " · "))
}
