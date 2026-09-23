package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
)

// Status bar, frame title, activity ticker, and desktop-notice state.

const turnCancelDetachAfter = 2 * time.Second

// Reserve room for the estimate/overflow markers and both compact counts.
const contextStatusWidth = 14

// sessionStatus is what the status bar shows and what its mouse target
// needs: the model and context names, the tool and skill counts, the last
// context usage report, the recently used models for the picker, and where
// the session field landed on the last render. Context usage describes the
// provider-visible request, not the complete durable transcript or the
// cumulative billed tokens.
type sessionStatus struct {
	modelName    string
	contextName  string
	title        string
	titleSource  sessions.TitleSource
	description  string
	toolCount    int
	skillCount   int
	recentModels []string

	contextUsed   int
	contextLimit  int
	contextBudget *contextBudgetDetails

	parentName   string
	modelField   statusSessionPlacement
	sessionField statusSessionPlacement
	contextField statusSessionPlacement

	// agents is what this workspace's agents are doing, as the status row
	// says it ("Agents · 2 running · 3 finished"), and agentsStyled its
	// colored form; agentsField is where it was painted, for a click that
	// opens the Agents inspector.
	agents       string
	agentsStyled string
	agentsField  statusSessionPlacement

	// changes is the session's cumulative tracked line counts ("+100 −20")
	// and changesStyled its colored form; changesField is where it was
	// painted, for a click that opens the Changes inspector.
	changes       string
	changesStyled string
	changesField  statusSessionPlacement

	// spend is the session's cost since this process opened it, members
	// included; nil for views that do not run the session.
	spend *sessionSpend
}

func newSessionStatus(settings *Settings, contextName string, toolCount, skillCount int) sessionStatus {
	s := sessionStatus{
		modelName:    settings.Model,
		contextName:  contextName,
		toolCount:    toolCount,
		skillCount:   skillCount,
		contextLimit: settings.MaxHistoryTokens,
	}
	if settings.Model != "" {
		s.recentModels = []string{settings.Model}
	}
	return s
}

func (s *sessionStatus) displayLabel() string {
	return sessions.DisplayLabel(&sessions.Metadata{Name: s.contextName, Title: s.title, Parent: s.parentName, Description: s.description})
}

// rememberModel puts a newly chosen model at the front of the picker's
// recent list unless it is already listed.
func (s *sessionStatus) rememberModel(model string) {
	if !slices.Contains(s.recentModels, model) {
		s.recentModels = append([]string{model}, s.recentModels...)
	}
}

func (s *sessionStatus) contextUsageText() string {
	used, limit := s.contextUsageParts()
	return used + limit
}

// contextUsageParts splits the usage readout into the used count and the
// window it is measured against: "12.3k" and "/156k", or "448 tok" and ""
// when no limit is known. The count is whatever is most current — a
// projection estimate until the provider reports measured usage — and it
// only ever means context usage, so no prefix qualifies it.
func (s *sessionStatus) contextUsageParts() (used, limit string) {
	if s.contextUsed <= 0 && s.contextLimit <= 0 {
		return "", ""
	}
	used = humanizeTokens(s.contextUsed)
	if s.contextLimit <= 0 {
		return used + " tok", ""
	}
	return used, "/" + humanizeTokens(s.contextLimit)
}

// contextUsageStyled colors the used count by how full the window is; the
// window itself stays muted so the number carries the signal.
func (s *sessionStatus) contextUsageStyled() string {
	used, limit := s.contextUsageParts()
	if used == "" {
		return ""
	}
	return style.Styled(used, contextUsageColor(s.contextUsed, s.contextLimit), "") + style.Styled(limit, "muted", "")
}

// contextUsageColor is muted without a limit, then ok, active, and err as the
// window passes three quarters and nine tenths full.
func contextUsageColor(used, limit int) string {
	if limit <= 0 {
		return "muted"
	}
	switch {
	case used*10 >= limit*9:
		return "err"
	case used*4 >= limit*3:
		return "active"
	default:
		return "ok"
	}
}

func (s *sessionStatus) clearContextUsage(limit int) {
	s.contextUsed = 0
	s.contextLimit = limit
	s.contextBudget = nil
}

func (s *sessionStatus) recordContextUsage(used, limit int) {
	if used < 0 {
		used = 0
	}
	s.contextUsed = used
	s.contextLimit = limit
}

// shortModelName trims a provider-qualified model to its display form:
// everything after the last slash ("openai/gpt-5.4" → "gpt-5.4"). A model
// with no slash passes through unchanged.
func shortModelName(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// statusRow renders stable session context. Per-turn activity and completion
// metrics live in the fixed turn dock immediately above the composer.
func (m *replModel) statusRow(width int) string {
	m.status.modelField = statusSessionPlacement{}
	m.status.sessionField = statusSessionPlacement{}
	m.status.agentsField = statusSessionPlacement{}
	m.status.contextField = statusSessionPlacement{}
	m.status.changesField = statusSessionPlacement{}
	if m.quiet || width <= 0 {
		return ""
	}
	const sep = " · "

	leftRaw, leftStyled := "", ""
	if m.busy && !m.turnStarted.IsZero() {
		leftRaw = formatElapsed(time.Since(m.turnStarted))
		leftStyled = style.Styled(leftRaw, "accent", "")
	} else if m.hoverHint != "" {
		// A wordless target under the pointer names its action here while
		// no turn owns the slot.
		leftRaw, leftStyled = m.hoverHint, style.Styled(m.hoverHint, "muted", "")
	}
	// A field drops in order of drop when the row is too narrow (0 never
	// drops), and records where it landed in place for its mouse target,
	// when it has one.
	type field struct {
		drop     int
		text     string
		rendered string // styled form when the field carries its own colors
		color    string
		place    *statusSessionPlacement
	}
	fields := []field{}
	if m.status.modelName != "" {
		// Show the bare model name; the provider prefix is redundant once
		// you know which model you're talking to ("gpt-5.4", not
		// "openai/gpt-5.4").
		fields = append(fields, field{drop: 3, text: shortModelName(m.status.modelName), color: "muted", place: &m.status.modelField})
	}
	fields = append(fields, field{text: m.status.displayLabel(), color: "accent", place: &m.status.sessionField})
	if m.status.agents != "" {
		fields = append(fields, field{drop: 2, text: m.status.agents, rendered: m.status.agentsStyled, color: "muted", place: &m.status.agentsField})
	}
	// The session's cumulative diff, summed from every tool result that
	// reported one; a click opens the Changes inspector.
	additions, deletions, files := m.changeStats()
	m.status.changes, m.status.changesStyled = changeTotalsText(additions, deletions)
	if m.status.changes == "" && files > 0 {
		m.status.changes = fmt.Sprintf("%d files", files)
		if files == 1 {
			m.status.changes = "1 file"
		}
		m.status.changesStyled = style.Styled(m.status.changes, "muted", "")
	}
	if m.workspaceChanges != nil {
		partial := m.workspaceChanges.omitted > 0
		for _, c := range m.workspaceChanges.changes {
			partial = partial || c.countsUnknown
		}
		if partial {
			m.status.changes += " · partial"
			m.status.changesStyled += style.Styled(" · partial", "muted", "")
		}
		if !m.workspaceChanges.tracked {
			m.status.changes = "changes unavailable"
			m.status.changesStyled = style.Styled(m.status.changes, "muted", "")
		}
	}
	if m.status.changes != "" {
		fields = append(fields, field{drop: 1, text: m.status.changes, rendered: m.status.changesStyled, color: "muted", place: &m.status.changesField})
	}
	if cost := m.status.spend.total(); cost.known {
		text := estimateMark(cost.estimated) + formatCostUSD(cost.usd)
		fields = append(fields, field{drop: 2, text: text, color: "muted"})
	}
	if context := m.status.contextUsageText(); context != "" {
		padding := strings.Repeat(" ", max(0, contextStatusWidth-rw.StringWidth(context)))
		fields = append(fields, field{drop: 1, text: padding + context, rendered: padding + m.status.contextUsageStyled(), color: "muted", place: &m.status.contextField})
	}

	fieldWidth := func(fs []field) int {
		parts := make([]string, len(fs))
		for i, f := range fs {
			parts[i] = f.text
		}
		return rw.StringWidth(strings.Join(parts, sep))
	}
	needed := func() int {
		n := rw.StringWidth(leftRaw) + fieldWidth(fields)
		if leftRaw != "" && len(fields) > 0 {
			n++
		}
		return n
	}
	for needed() > width && len(fields) > 1 {
		idx := -1
		best := 0
		for i, f := range fields {
			if f.drop > best {
				best = f.drop
				idx = i
			}
		}
		if idx < 0 {
			break
		}
		fields = slices.Delete(fields, idx, idx+1)
	}

	// Truncate the session name when it is the only remaining field.
	if needed() > width && len(fields) == 1 {
		budget := width - rw.StringWidth(leftRaw)
		if leftRaw != "" {
			budget--
		}
		if budget <= 0 {
			fields[0].text = ""
		} else {
			fields[0].text = rw.Truncate(fields[0].text, budget, "…")
		}
	}

	rightRawParts := make([]string, len(fields))
	rightStyledParts := make([]string, len(fields))
	for i, f := range fields {
		rightRawParts[i] = f.text
		if f.rendered != "" && !strings.HasPrefix(f.text, "…") {
			rightStyledParts[i] = f.rendered
			continue
		}
		rightStyledParts[i] = style.Styled(f.text, f.color, "")
	}
	rightRaw := strings.Join(rightRawParts, sep)
	rightStyled := strings.Join(rightStyledParts, style.Styled(sep, "muted", ""))

	rightWidth := rw.StringWidth(rightRaw)
	leftBudget := width - rightWidth
	if leftRaw != "" && rightRaw != "" {
		leftBudget--
	}
	if leftBudget < 0 {
		leftBudget = 0
	}
	if rw.StringWidth(leftRaw) > leftBudget {
		if leftBudget == 0 {
			leftRaw, leftStyled = "", ""
		} else {
			leftRaw = rw.Truncate(leftRaw, leftBudget, "…")
			leftStyled = style.Styled(leftRaw, "muted", "")
		}
	}

	x := width - rightWidth
	sepWidth := rw.StringWidth(sep)
	for i, f := range fields {
		fieldCols := rw.StringWidth(f.text)
		if fieldCols > 0 && f.place != nil {
			*f.place = statusSessionPlacement{X: x, Cols: fieldCols}
		}
		x += fieldCols
		if i < len(fields)-1 {
			x += sepWidth
		}
	}
	// Right-align: pad the left edge so the field block ends at the
	// terminal's right margin.
	pad := width - rw.StringWidth(leftRaw) - rightWidth
	if pad < 0 {
		pad = 0
	}
	return leftStyled + strings.Repeat(" ", pad) + rightStyled
}

// activityTicker is the pinned bottom-row notice shown while the user is
// scrolled up: how much transcript lies below the viewport, what the agent is
// doing right now, and the way back. Empty when following or nothing is below.
// Caller must hold m.mu.
func (m *replModel) activityTicker(totalRows, topRow, height int) string {
	if m.followBottom || height <= 0 {
		return ""
	}
	below := totalRows - (topRow + height)
	if below <= 0 {
		return ""
	}
	word := "rows"
	if below == 1 {
		word = "row"
	}
	raw := fmt.Sprintf("↓ %d %s below", below, word)
	if m.busy {
		raw += " · " + m.busyLabel()
	}
	return style.Styled(raw, "accent", "bold") + style.Styled(" · End to follow", "muted", "")
}

func compactQueuePreview(text string) string {
	return style.Truncate(strings.Join(strings.Fields(text), " "), 36)
}

func humanizeTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 100_000:
		whole := n / 1000
		frac := (n % 1000) / 100
		return fmt.Sprintf("%d.%dk", whole, frac)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		whole := n / 1_000_000
		frac := (n % 1_000_000) / 100_000
		return fmt.Sprintf("%d.%dM", whole, frac)
	}
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", m, s)
}

// frameTitle is the desired terminal window title: app · context, then the
// live turn state so progress is readable from another window or the tab bar.
// Caller must hold m.mu.
func (m *replModel) frameTitle() string {
	title := "polly"
	if label := m.status.displayLabel(); label != "" && label != "-" {
		title += " · " + label
	}
	switch {
	case m.approval != nil:
		return title + " — approval needed"
	case m.busy:
		s := title + " — " + m.busyLabel()
		if !m.turnStarted.IsZero() {
			s += " · " + coarseElapsed(time.Since(m.turnStarted))
		}
		return s
	}
	switch m.lastOutcome {
	case turnOutcomeDone:
		return title + " — done · " + formatElapsed(m.lastElapsed)
	case turnOutcomeFailed:
		return title + " — failed"
	case turnOutcomeCanceled:
		return title + " — canceled"
	case turnOutcomeIncomplete:
		return title + " — incomplete"
	}
	return title
}

// frameProgress is the desired taskbar progress payload (see terminalFX): an
// indeterminate bar while a turn runs, an error badge while a failure is the
// settled outcome, nothing otherwise. Caller must hold m.mu.
func (m *replModel) frameProgress() string {
	if m.busy {
		return progressBusy
	}
	if m.lastOutcome == turnOutcomeFailed {
		return progressFail
	}
	return progressNone
}

// coarseElapsed formats a duration at whole-second granularity for surfaces
// that shouldn't churn ten times a second (window title, notifications).
func coarseElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return formatElapsed(d)
}

// notifyMinTurn is the shortest turn whose completion is worth a desktop
// notification; anything quicker, the user never had time to look away.
const notifyMinTurn = 10 * time.Second

// pushNotice queues a desktop-notification body. Caller must hold m.mu.
func (m *replModel) pushNotice(body string) {
	m.notificationMu.Lock()
	defer m.notificationMu.Unlock()
	m.notices = append(m.notices, body)
}

// takeNotices drains queued notifications, returning them only when the
// terminal has explicitly reported itself unfocused — a watching user needs no
// ping. Drained-but-dropped notices are gone for good; going unfocused later
// must not replay stale news. Caller must hold m.mu.
func (m *replModel) takeNotices() []string {
	m.notificationMu.Lock()
	defer m.notificationMu.Unlock()
	out := m.notices
	m.notices = nil
	if !m.focusKnown || m.focused {
		return nil
	}
	return out
}
