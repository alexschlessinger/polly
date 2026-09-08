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
	scope, id string
	agent     bool
	name      string
	launch    *lineAgentProgress
	detail    *lineToolDetail
}

type lineAgentProgress struct {
	label, status string
	active        bool
	tools, images int
	thought       time.Duration
	in, out       int
}

// All state and writes are protected by lineTurnUI.toolMu, including the timer.
// Cursor ownership lives in lineTerminalFrame, separately from these metrics.
type lineActivity struct {
	caps    lineStatusCapabilities
	started time.Time
	// state and toolName mirror the TUI's live turn state; loggedLabel is the
	// last state line written when the status is not live.
	state         turnState
	toolName      string
	loggedLabel   string
	outcome       turnOutcome
	lastText      string
	imageCaps     outputCapabilities
	active        []lineActivityItem
	tools, images int
	launches      []*lineAgentProgress
	scopes        map[string]*lineChildActivity
	elapsed       time.Duration
	completed     bool
	details       *lineActivityDetails
	// thought banks the parent's reasoning time by phase, the way the TUI
	// banks it by segment; reasoned records that any reasoning arrived.
	thought                  time.Duration
	thinkingSince            time.Time
	reasoned                 bool
	in, out                  int
	visible, paused, stopped bool
	nextScope                int
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
		started: time.Now(), state: turnStateWaiting,
		imageCaps: resolveOutputCapabilities(conversationModeOneShot, false, ui.stderrTTY, columns, os.Getenv),
	}
	if ui.config.ActivityDetails {
		ui.activity.details = &lineActivityDetails{}
	}
	a := ui.activity
	ui.renderActivityLocked()
	if !a.caps.live {
		ui.logStateLocked()
		return
	}
}

func (ui *lineTurnUI) clearActivityLocked() {
	if ui.stream != nil && ui.sameTerminal {
		columns, rows := ui.answerSizeLocked()
		ui.stream.resync(ui, columns, rows)
	}
	if a := ui.activity; a != nil && a.visible {
		ui.statusFrame.clear(ui.errWriter, ui.statusColumnsLocked(), 0)
		a.visible = false
	}
}

func (ui *lineTurnUI) renderActivityLocked() {
	if ui.completed || ui.prompting {
		return
	}
	if ui.stream != nil {
		ui.stream.draw(ui, false)
		if ui.sameTerminal {
			return
		}
	}
	a := ui.activity
	if a == nil || a.stopped || a.paused || !a.caps.live || (ui.sameTerminal && ui.contentPrinted && !ui.endsWithNewline) {
		return
	}
	// The line is overwritten in place and never terminated, so it stops
	// short of the last column to avoid a wrap.
	var rows []string
	if columns := ui.statusColumnsLocked(); columns >= 5 {
		rows = ui.activityRowsLocked(false)
	}
	text := strings.Join(rows, "\n")
	if a.visible && text == a.lastText {
		return
	}
	ui.statusFrame.clear(ui.errWriter, ui.statusColumnsLocked(), 0)
	ui.statusFrame.write(ui.errWriter, rows)
	a.visible = true
	a.lastText = text
}

// liveFields is the activity row. Elapsed time and usage are fitted separately.
func (a *lineActivity) liveFields() []turnDockField {
	role, modifier := "run", "bold"
	switch a.state {
	case turnStateThinking:
		role, modifier = "active", "bold"
	case turnStateStreaming:
		role, modifier = "accent", ""
	}
	label := a.busyLabel()
	fields := []turnDockField{
		{raw: label, rendered: styled(label, role, modifier)},
	}
	if a.reasoned {
		fields = append(fields, accentField(reasoningDisclosureLabel(false, false, a.thoughtDuration())))
	}
	return append(fields, a.countFields()...)
}

func (a *lineActivity) busyLabel() string {
	if a.state != turnStateThinking && a.state != turnStateStreaming {
		for _, item := range a.active {
			if item.scope == "" && !item.agent {
				return turnBusyLabel(turnStateTool, item.name, false)
			}
		}
		for _, launch := range a.launches {
			if launch.active {
				return "waiting for agents"
			}
		}
	}
	return turnBusyLabel(a.state, a.toolName, false)
}

func (a *lineActivity) thoughtDuration() time.Duration {
	elapsed := a.thought
	if !a.thinkingSince.IsZero() {
		elapsed += time.Since(a.thinkingSince)
	}
	return elapsed
}

func (a *lineActivity) summary() turnActivitySummary {
	s := turnActivitySummary{Reasoned: a.reasoned, Thought: a.thoughtDuration(), Tools: a.tools, Images: a.images, Outcome: a.outcome, Elapsed: a.elapsed, In: a.in, Out: a.out}
	for _, launch := range a.launches {
		s.Agents.add(launch.status, launch.active)
	}
	return s
}

// countFields shares parent tallies between the live and settled rows.
func (a *lineActivity) countFields() []turnDockField {
	var fields []turnDockField
	s := a.summary()
	if s.Tools > 0 {
		fields = append(fields, accentField(turnToolLabel(s.Tools)))
	}
	if c := s.Agents; c.Total > 0 {
		fields = append(fields, accentField(turnAgentSummaryLabel(c.Total, c.Running, c.Failed, c.Canceled, c.Paused)))
	}
	if s.Images > 0 {
		fields = append(fields, accentField(turnImageLabel(s.Images)))
	}
	return fields
}

func accentField(label string) turnDockField {
	return turnDockField{raw: label, rendered: styled(label, "accent", "")}
}

func mutedField(label string, optional bool) turnDockField {
	return turnDockField{raw: label, rendered: styled(label, "muted", ""), optional: optional}
}

// Use the same theme-relative palette and style conversion as the TUI.
// activityColorLocked styles a settled notice with the TUI palette: failures
// and warnings stand out, everything else reads as metadata.
func (u *lineTurnUI) activityColorLocked(text string) string {
	if u.activity == nil || !u.activity.caps.color {
		return text
	}
	parts := strings.Split(text, " · ")
	for i, part := range parts {
		role, modifier := "muted", ""
		switch {
		case strings.Contains(part, "✗"):
			role, modifier = "err", "bold"
		case strings.HasPrefix(part, "Warning:"):
			role, modifier = "active", "bold"
		}
		parts[i] = styled(part, role, modifier)
	}
	return styledMarkupToLine(strings.Join(parts, " · "), true)
}

// styledMarkupToLine flattens gotui style markup for one stderr line: the
// TUI palette as ANSI when color is on, plain text otherwise.
func styledMarkupToLine(markup string, color bool) string {
	cells := parseStyledCells(markup, ui.StyleClear)
	var out bytes.Buffer
	if color {
		appendANSIStyledCells(&out, cells)
		return out.String()
	}
	for _, cell := range cells {
		if !unicode.IsControl(cell.Rune) {
			out.WriteRune(cell.Rune)
		}
	}
	return out.String()
}

// statusColumnsLocked is the current stderr width, falling back to the width
// resolved at start when the terminal cannot be measured.
func (ui *lineTurnUI) statusColumnsLocked() int {
	if ui.size != nil {
		columns, _ := ui.size(false)
		return columns
	}
	columns := ui.activity.caps.columns
	if ui.stderrTTY {
		if width, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && width > 0 {
			columns = width
		}
	}
	return columns
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
	ui.statusLineLocked(ui.activityColorLocked(cleanActivityText(text)))
}

// statusLineLocked prints one settled, already rendered line to stderr.
func (ui *lineTurnUI) statusLineLocked(line string) {
	ui.clearActivityLocked()
	ui.flushBufferedMarkdown()
	// Only terminate stdout when it shares the terminal. Redirected answers
	// must not acquire newlines from asynchronous status events.
	if ui.sameTerminal && ui.contentPrinted && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
	fmt.Fprintln(ui.statusWriterLocked(), line)
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

// activityStateLocked moves the turn to its next TUI busy state.
func (ui *lineTurnUI) activityStateLocked(state turnState, toolName string) {
	a := ui.activity
	if a == nil || a.stopped || (a.state == state && a.toolName == toolName) {
		return
	}
	ui.clearActivityLocked()
	a.bankThinking()
	a.state, a.toolName = state, toolName
	if state == turnStateThinking {
		a.reasoned = true
		a.thinkingSince = time.Now()
	}
	if !a.caps.live {
		ui.logStateLocked()
	}
}

// logStateLocked writes the state as a plain line when the status is not
// live. A log records what happened rather than the waits between events,
// and never names individual tools: one line per stretch of activity.
func (ui *lineTurnUI) logStateLocked() {
	a := ui.activity
	label := turnBusyLabel(a.state, "", false)
	if label == a.loggedLabel || (a.state == turnStateWaiting && a.loggedLabel != "") {
		return
	}
	a.loggedLabel = label
	ui.activityLineLocked(label)
}

// bankThinking folds an open reasoning stretch into the turn's thought time.
func (a *lineActivity) bankThinking() {
	if !a.thinkingSince.IsZero() {
		a.thought += time.Since(a.thinkingSince)
		a.thinkingSince = time.Time{}
	}
}

func (ui *lineTurnUI) activityToolsLocked(scope string, calls []messages.ChatMessageToolCall) {
	a := ui.activity
	if a == nil || a.stopped {
		return
	}
	for _, call := range calls {
		agent := call.Name == "spawn_agent"
		item := lineActivityItem{scope: scope, id: call.ID, agent: agent, name: call.Name}
		if scope == "" {
			if agent {
				item.launch = &lineAgentProgress{label: lineAgentName(call, fmt.Sprintf("agent %d", len(a.launches)+1)), status: "starting", active: true}
				a.launches = append(a.launches, item.launch)
			} else {
				a.tools++
				if a.details != nil {
					item.detail = a.details.startTool(call)
				}
			}
		} else if child := a.scopes[scope]; child != nil {
			if !agent {
				child.launch.tools++
			}
		}
		a.active = append(a.active, item)
	}
}

func (ui *lineTurnUI) activityToolEndLocked(scope, prefix string, call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	if a := ui.activity; a != nil {
		for i, item := range a.active {
			if item.scope == scope && item.id == call.ID {
				if item.launch != nil {
					item.launch.active = false
					if item.launch.status == "starting" || err != nil || toolWasDenied(result) {
						item.launch.status = toolActivityOutcome(toolWasDenied(result), err)
					}
				}
				if item.detail != nil {
					item.detail.finish(call, result, duration, err)
				}
				a.active = append(a.active[:i], a.active[i+1:]...)
				break
			}
		}
		// Like the TUI, the turn returns to waiting only once the whole batch
		// has finished, so parallel tools keep the running name.
		if scope == "" {
			if a.runningIn(scope) == 0 {
				ui.activityStateLocked(turnStateWaiting, "")
			} else {
				for _, item := range a.active {
					if item.scope == scope {
						a.toolName = item.name
						break
					}
				}
			}
		}
	}
	// Successful work only updates counts. Never leave a per-call transcript
	// (including child calls or result-line metadata) in normal scrollback/logs.
	label := prefix + toolDisplayName(call.Name)
	if toolWasDenied(result) {
		ui.activityLineLocked("  ✗ " + label + " · denied")
	} else if err != nil {
		meta := toolFailureMeta(err)
		if meta == "" {
			meta = toolActivityOutcome(false, err)
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
	ui.CompleteTurn(turnCompletion{Reason: reason, Err: err})
}

func (ui *lineTurnUI) CompleteTurn(completion turnCompletion) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	ui.clearActivityLocked()
	if ui.config.SchemaPath == "" {
		ui.flushBufferedMarkdown()
	}
	ui.completed = true
	ui.stopRendererLocked()
	if a := ui.activity; a != nil {
		if a.completed {
			return
		}
		a.completed = true
		ui.clearActivityLocked()
		if ui.config.SchemaPath == "" {
			ui.flushBufferedMarkdown()
		}
		a.outcome, a.elapsed = completion.outcome(), completion.Elapsed
		if a.elapsed == 0 {
			a.elapsed = time.Since(a.started)
		}
		ui.finishDetailsLocked(completion)
		ui.finishActivityLocked()
	}
}

// Children keep their replies private while forwarding attributed activity to
// the same serialized renderer. Scope IDs distinguish reused provider call IDs.
type lineChildActivity struct {
	ui                   *lineTurnUI
	scope, prefix, label string
	launch               *lineAgentProgress
	state                turnState
	toolName             string
	thinkingSince        time.Time
	in, out              int
	completed            bool
	direct               bool
}

type lineChildActivityHost interface {
	childActivity(messages.ChatMessageToolCall) *lineChildActivity
}

func (ui *lineTurnUI) childActivity(call messages.ChatMessageToolCall) *lineChildActivity {
	return ui.newChildActivity("", call)
}

func (ui *lineTurnUI) newChildActivity(parentScope string, call messages.ChatMessageToolCall) *lineChildActivity {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.activity == nil || ui.activity.stopped {
		return nil
	}
	ui.activity.nextScope++
	var launch *lineAgentProgress
	prefix := ""
	if parent := ui.activity.scopes[parentScope]; parent != nil {
		launch, prefix = parent.launch, parent.prefix
	} else {
		for _, item := range ui.activity.active {
			if item.scope == parentScope && item.id == call.ID {
				launch = item.launch
				break
			}
		}
	}
	if launch == nil {
		return nil
	}
	// A direct child is named by its launch row so notices, captions, and
	// the Agents summary agree; nextScope also counts nested children, so
	// it only numbers those. Either way nextScope stays the scope identity.
	label := launch.label
	if parentScope != "" {
		label = prefix + lineAgentName(call, fmt.Sprintf("agent %d", ui.activity.nextScope))
	}
	c := &lineChildActivity{ui: ui, scope: fmt.Sprint(ui.activity.nextScope), prefix: label + ": ", label: label, launch: launch, state: turnStateWaiting, direct: parentScope == ""}
	if ui.activity.scopes == nil {
		ui.activity.scopes = make(map[string]*lineChildActivity)
	}
	ui.activity.scopes[c.scope] = c
	return c
}

func (c *lineChildActivity) stateLocked(state turnState, name string) {
	if c.completed {
		return
	}
	if !c.thinkingSince.IsZero() && state != turnStateThinking {
		c.launch.thought += time.Since(c.thinkingSince)
		c.thinkingSince = time.Time{}
	}
	if state == turnStateThinking && c.thinkingSince.IsZero() {
		c.thinkingSince = time.Now()
	}
	c.state, c.toolName = state, name
}

func (c *lineChildActivity) phase(state turnState) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.stateLocked(state, "")
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) warning(text string) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped || c.completed {
		return
	}
	c.ui.activityLineLocked("Warning: " + c.prefix + text)
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) usage(in, out int) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped || c.completed {
		return
	}
	// Each child's totals replace its preceding sample, so its final
	// reconciliation cannot be counted a second time under Agents.
	c.launch.in += in - c.in
	c.launch.out += out - c.out
	c.in, c.out = in, out
}

func (c *lineChildActivity) complete(completion turnCompletion) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped || c.completed {
		return
	}
	c.stateLocked(turnStateIdle, "")
	c.completed = true
	delete(c.ui.activity.scopes, c.scope)
	// A nested run updates attributed work, never its direct ancestor's
	// lifecycle. Only the direct launch call can settle that agent.
	if c.direct {
		c.launch.status = toolActivityOutcome(false, completion.Err)
		c.launch.active = false
	}
	c.ui.renderActivityLocked()
}

// runningIn counts the scope's tool calls still in flight.
func (a *lineActivity) runningIn(scope string) int {
	running := 0
	for _, item := range a.active {
		if item.scope == scope {
			running++
		}
	}
	return running
}

func (c *lineChildActivity) tools(calls []messages.ChatMessageToolCall) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.ui.clearActivityLocked()
	if len(calls) > 0 {
		c.stateLocked(turnStateTool, calls[0].Name)
	}
	c.ui.activityToolsLocked(c.scope, calls)
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) toolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.ui.activityToolEndLocked(c.scope, c.prefix, call, result, duration, err)
	if c.ui.activity.runningIn(c.scope) == 0 {
		c.stateLocked(turnStateWaiting, "")
	}
	c.ui.renderActivityLocked()
}

func (c *lineChildActivity) media(images []transcriptImage) {
	c.ui.toolMu.Lock()
	defer c.ui.toolMu.Unlock()
	if c.ui.activity.stopped {
		return
	}
	c.launch.images += len(images)
	for _, img := range images {
		c.ui.activityLineLocked("    " + c.prefix + transcriptImageCaptionText(img))
	}
	c.ui.renderActivityLocked()
}

// finishActivityLocked prints the settled summary: the TUI's turn trailer,
// same fields in the same order, minus its click glyphs (nothing on stderr is
// clickable).
func (ui *lineTurnUI) finishActivityLocked() {
	a := ui.activity
	if a == nil || a.stopped {
		return
	}
	ui.clearActivityLocked()
	a.stopped = true
	a.bankThinking()
	if a.elapsed == 0 {
		a.elapsed = time.Since(a.started)
	}
	for _, launch := range a.launches {
		if launch.active {
			launch.status, launch.active = "canceled", false
		}
	}
	// A tool whose end never arrived may have half-changed the world. The
	// trailer stays at parity with the TUI, so the fact goes to scrollback
	// the way a failed or denied call does.
	for _, item := range a.active {
		if item.agent {
			continue
		}
		prefix := ""
		if child := a.scopes[item.scope]; child != nil {
			prefix = child.prefix
		}
		ui.activityLineLocked("  ✗ " + prefix + toolDisplayName(item.name) + " · unfinished")
	}
	if a.outcome == turnOutcomeNone {
		a.outcome = turnOutcomeFailed
		if ui.finished {
			a.outcome = turnOutcomeDone
		}
	}
	for _, row := range ui.activityRowsLocked(true) {
		ui.statusLineLocked(row)
	}
}

// unboundedStatusWidth keeps a logged summary whole: only a live terminal
// line is fitted to its columns the way the TUI fits its trailer.
const unboundedStatusWidth = 1 << 30
