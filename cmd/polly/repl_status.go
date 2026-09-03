package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
)

// Status bar, frame title, activity ticker, and desktop-notice state.

const turnCancelDetachAfter = 2 * time.Second

// sessionStatus is what the status bar shows and what its mouse target
// needs: the model and context names, the tool and skill counts, the last
// context usage report, the recently used models for the picker, and where
// the session field landed on the last render. Context usage describes the
// provider-visible request, not the complete durable transcript or the
// cumulative billed tokens.
type sessionStatus struct {
	modelName    string
	contextName  string
	toolCount    int
	skillCount   int
	recentModels []string

	contextUsed      int
	contextLimit     int
	contextEstimated bool

	sessionField statusSessionPlacement
}

func newSessionStatus(config *Config, contextName string, toolCount, skillCount int) sessionStatus {
	s := sessionStatus{
		modelName:    config.Model,
		contextName:  contextName,
		toolCount:    toolCount,
		skillCount:   skillCount,
		contextLimit: config.MaxHistoryTokens,
	}
	if config.Model != "" {
		s.recentModels = []string{config.Model}
	}
	return s
}

// rememberModel puts a newly chosen model at the front of the picker's
// recent list unless it is already listed.
func (s *sessionStatus) rememberModel(model string) {
	if !slices.Contains(s.recentModels, model) {
		s.recentModels = append([]string{model}, s.recentModels...)
	}
}

func (s *sessionStatus) contextUsageText() string {
	if s.contextUsed <= 0 && s.contextLimit <= 0 {
		return ""
	}
	prefix := "ctx "
	if s.contextEstimated {
		prefix += "~"
	}
	used := humanizeTokens(s.contextUsed)
	if s.contextLimit <= 0 {
		return prefix + used
	}
	if s.contextUsed > s.contextLimit && s.contextEstimated {
		used = ">" + humanizeTokens(s.contextLimit)
	}
	return prefix + used + "/" + humanizeTokens(s.contextLimit)
}

func (s *sessionStatus) clearContextUsage(limit int) {
	s.contextUsed = 0
	s.contextLimit = limit
	s.contextEstimated = false
}

func (s *sessionStatus) recordContextUsage(used, limit int, estimated bool) {
	if used < 0 {
		used = 0
	}
	s.contextUsed = used
	s.contextLimit = limit
	s.contextEstimated = estimated
}

// statusRow renders stable session context. Per-turn activity and completion
// metrics live in the fixed turn dock immediately above the composer.
func (m *replModel) statusRow(width int) string {
	m.status.sessionField = statusSessionPlacement{}
	if m.quiet || width <= 0 {
		return ""
	}
	const sep = " · "

	leftRaw, leftStyled := "", ""
	if m.busy && !m.turnStarted.IsZero() {
		leftRaw = formatElapsed(time.Since(m.turnStarted))
		leftStyled = styled(leftRaw, "accent", "")
	}
	type field struct {
		drop    int
		text    string
		session bool
	}
	fields := []field{}
	if m.status.modelName != "" {
		fields = append(fields, field{drop: 3, text: m.status.modelName})
	}
	fields = append(fields, field{drop: 0, text: m.status.contextName, session: true})
	if context := m.status.contextUsageText(); context != "" {
		fields = append(fields, field{drop: 1, text: context})
	}
	if m.status.toolCount > 0 {
		fields = append(fields, field{drop: 2, text: fmt.Sprintf("tools:%d", m.status.toolCount)})
	}
	if m.status.skillCount > 0 {
		fields = append(fields, field{drop: 4, text: fmt.Sprintf("skills:%d", m.status.skillCount)})
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
		fields = append(fields[:idx], fields[idx+1:]...)
	}

	// Context is the one non-droppable field. Truncate it rather than letting a
	// wide context name push operational state off-screen.
	if needed() > width && len(fields) == 1 {
		budget := width - rw.StringWidth(leftRaw)
		if leftRaw != "" {
			budget--
		}
		if budget < 0 {
			budget = 0
		}
		if budget == 0 {
			fields[0].text = ""
		} else {
			fields[0].text = rw.Truncate(fields[0].text, budget, "…")
		}
	}

	rightRawParts := make([]string, len(fields))
	rightStyledParts := make([]string, len(fields))
	for i, f := range fields {
		rightRawParts[i] = f.text
		color := "muted"
		if f.session {
			color = "accent"
		}
		rightStyledParts[i] = styled(f.text, color, "")
	}
	rightRaw := strings.Join(rightRawParts, sep)
	rightStyled := strings.Join(rightStyledParts, styled(sep, "muted", ""))

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
			leftStyled = styled(leftRaw, "muted", "")
		}
	}

	if rightRaw == "" {
		return leftStyled
	}
	gap := width - rw.StringWidth(leftRaw) - rightWidth
	if leftRaw != "" && gap < 1 {
		gap = 1
	}
	if gap < 0 {
		gap = 0
	}
	x := width - rightWidth
	sepWidth := rw.StringWidth(sep)
	for i, f := range fields {
		fieldCols := rw.StringWidth(f.text)
		if f.session && fieldCols > 0 {
			m.status.sessionField = statusSessionPlacement{X: x, Cols: fieldCols}
		}
		x += fieldCols
		if i < len(fields)-1 {
			x += sepWidth
		}
	}
	return leftStyled + strings.Repeat(" ", gap) + rightStyled
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
	return styled(raw, "accent", "bold") + styled(" · End to follow", "muted", "")
}

func compactQueuePreview(text string) string {
	return truncate(strings.Join(strings.Fields(text), " "), 36)
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
	if m.status.contextName != "" && m.status.contextName != "-" {
		title += " · " + m.status.contextName
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
	m.notices = append(m.notices, body)
}

// takeNotices drains queued notifications, returning them only when the
// terminal has explicitly reported itself unfocused — a watching user needs no
// ping. Drained-but-dropped notices are gone for good; going unfocused later
// must not replay stale news. Caller must hold m.mu.
func (m *replModel) takeNotices() []string {
	out := m.notices
	m.notices = nil
	if !m.focusKnown || m.focused {
		return nil
	}
	return out
}
