package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

type referenceChoice struct {
	text, description string
	score             int
}
type referenceCompletion struct {
	key                  string
	start, end, selected int
	choices              []referenceChoice
	// args marks a popup opened by Tab for a slash-command argument. Unlike
	// the token popups, which re-derive from a leading / or @ token on every
	// keystroke, it is only ever opened by an explicit Tab; edits re-filter it
	// until the argument completes.
	args bool
}

func completionKey(m *replModel) string {
	return fmt.Sprintf("%d:%d:%s", m.ed.revision, m.ed.cursor, m.ed.text())
}
func fuzzyReferenceScore(value, query string) int {
	value, query = strings.ToLower(value), strings.ToLower(query)
	if value == query {
		return 0
	}
	if strings.HasPrefix(value, query) {
		return 1
	}
	if strings.Contains(value, query) {
		return 2
	}
	q := []rune(query)
	n := 0
	for _, ch := range value {
		if n < len(q) && ch == q[n] {
			n++
		}
	}
	if n == len(q) {
		return 3
	}
	return -1
}
func (r *managedREPL) refreshReferenceCompletionLocked() {
	m := r.model
	if m.pasting || m.hist.searching || m.approval != nil || m.modal != nil || r.inspectorFocused() {
		m.referencesPopup = nil
		return
	}
	key := completionKey(m)
	if key == m.referenceDismissed {
		return
	}
	if m.referencesPopup != nil && m.referencesPopup.key == key {
		return
	}
	if m.referencesPopup != nil && m.referencesPopup.args {
		// An argument popup tracks edits: re-filter it from the current
		// input, closing it once nothing remains to choose.
		start, end, choices, ok := defaultReplCommands.argCompletion(newManagedReplCommandContext(r), m.ed.text())
		if !ok {
			m.referencesPopup = nil
			return
		}
		m.referencesPopup = &referenceCompletion{key: key, args: true, start: start, end: end, choices: choices}
		return
	}
	m.referencesPopup = nil
	var ref composerReference
	found := false
	for _, token := range scanComposerReferenceTokens(m.ed.text()) {
		if m.ed.cursor > token.start && m.ed.cursor <= token.end {
			ref, found = token, true
			break
		}
	}
	if !found {
		return
	}
	if ref.kind == "/" && ref.start != 0 {
		return
	}
	if ref.kind == "@" && (r.state == nil || r.state.session == nil || r.state.effectiveTools() == nil) {
		return
	}
	if ref.kind == "@" && time.Since(m.referenceFilesAt) > 10*time.Second {
		m.referenceFilesLoaded = false
	}
	if ref.kind == "@" && !m.referenceFilesLoaded && !m.referenceFilesLoading {
		m.referenceFilesLoading = true
		registry, root := r.state.effectiveTools(), m.imageBaseDir
		ctx, cancel := context.WithTimeout(r.state.sessionContext(), 10*time.Second)
		r.background(func() {
			defer cancel()
			paths, err := registry.ContextFilePaths(ctx, root)
			r.postUI(r.work.ctx, func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				m.referenceFilesLoading = false
				m.referenceFilesLoaded = true
				m.referenceFilesAt = time.Now()
				if err == nil {
					m.referenceFiles = paths
				}
				// Enumeration is query-independent. Recompute from the current
				// cursor/draft rather than publishing the original query's results.
				if err != nil {
					m.appendNoticeLine("File completion unavailable · " + err.Error())
				}
				if r.model == m {
					m.referencesPopup = nil
					r.refreshReferenceCompletionLocked()
				}
			})
		})
	}
	prefix := string(m.ed.buf[ref.start:m.ed.cursor])
	query := strings.TrimPrefix(prefix, ref.kind)
	if ref.kind == "@" {
		query = strings.Trim(query, "\"'")
	}
	if ref.kind == "skill" {
		query = strings.TrimSpace(strings.TrimPrefix(prefix, "/skill"))
	}
	var choices []referenceChoice
	add := func(value, text, desc string) {
		if score := fuzzyReferenceScore(value, query); score >= 0 {
			choices = append(choices, referenceChoice{text, desc, score})
		}
	}
	switch ref.kind {
	case "@":
		for _, path := range m.referenceFiles {
			add(path, fileReference(path), "")
		}
	case "/", "skill":
		if ref.kind == "/" && ref.start == 0 {
			add("skill", "/skill", "activate a named skill for this prompt")
			for _, name := range defaultReplCommands.commandNames() {
				cmd, _ := defaultReplCommands.get(name)
				add(strings.TrimPrefix(name, "/"), name, cmd.summary)
			}
		}
		for _, skill := range r.composerCatalog().List() {
			// A skill a command activates is offered as that command, which
			// is already listed: only /skill names it, for reading it.
			if skill.Command != "" && ref.kind == "/" {
				continue
			}
			spelling := "/" + skill.Name
			if _, reserved := defaultReplCommands.get(spelling); reserved || ref.kind == "skill" {
				spelling = "/skill " + skill.Name
			}
			add(skill.Name, spelling, skill.Description)
		}
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].score != choices[j].score {
			return choices[i].score < choices[j].score
		}
		return choices[i].text < choices[j].text
	})
	if len(choices) == 0 {
		return
	}
	if choices[0].text == prefix && m.ed.cursor == ref.end {
		return
	}
	if len(choices) > 50 {
		choices = choices[:50]
	}
	m.referencesPopup = &referenceCompletion{key: key, start: ref.start, end: ref.end, choices: choices}
}
func (r *managedREPL) handleReferenceCompletionKey(e ui.Event) bool {
	m := r.model
	p := m.referencesPopup
	if p != nil && p.key != completionKey(m) {
		m.referencesPopup = nil
		return false
	}
	if p == nil || r.inspectorFocused() || m.modal != nil || m.approval != nil || m.hist.searching || e.Type != ui.KeyboardEvent {
		return false
	}
	switch e.ID {
	case "<Up>":
		p.selected = (p.selected + len(p.choices) - 1) % len(p.choices)
		return true
	case "<Down>":
		p.selected = (p.selected + 1) % len(p.choices)
		return true
	case "<Escape>", "<Esc>":
		m.referenceDismissed = completionKey(m)
		m.referencesPopup = nil
		return true
	// Enter accepts the highlighted choice like Tab; a second Enter sends the
	// draft once the popup is gone.
	case "<Enter>", "<Tab>":
		choice := p.choices[p.selected].text
		delete(m.referenceSnapshots, choice)
		m.clearRestoredDraft()
		suffix := " "
		if p.end < len(m.ed.buf) && m.ed.buf[p.end] == ' ' {
			suffix = ""
		}
		m.ed.replace(p.start, p.end, choice+suffix)
		m.referencesPopup = nil
		m.referenceDismissed = completionKey(m)
		return true
	}
	return false
}

// openArgChoices fills the reference popup with the choices for the slash
// command argument at the cursor. Tab opens it when inline completion cannot
// extend; from there Up/Down select, Tab or Enter inserts the selected value,
// and typing re-filters the list until the argument completes.
func (r *managedREPL) openArgChoices() {
	m := r.model
	start, end, choices, ok := defaultReplCommands.argCompletion(newManagedReplCommandContext(r), m.ed.text())
	if !ok {
		return
	}
	m.referencesPopup = &referenceCompletion{key: completionKey(m), args: true, start: start, end: end, choices: choices}
}

// referencePopupWidget lays the completion popup out above the cursor, as
// wide as its widest row up to referencePopupMaxWidth. Native images give way
// to the whole box, which the returned widget's rectangle bounds.
func (m *replModel) referencePopupWidget(width, cursorX, cursorY int) *style.LiteralParagraph {
	p := m.referencesPopup
	if p == nil || m.modal != nil || m.approval != nil || cursorY < 2 {
		return nil
	}
	rows := min(6, len(p.choices), cursorY-1)
	if rows < 1 {
		return nil
	}
	start := max(0, p.selected-rows+1)
	maxWidth := min(referencePopupMaxWidth, width)
	var texts []string
	boxWidth := 1
	for _, choice := range p.choices[start:min(start+rows, len(p.choices))] {
		text := choice.text
		if choice.description != "" {
			text += "  " + choice.description
		}
		text = rw.Truncate(text, maxWidth-2, "…")
		if choice.text == p.choices[p.selected].text {
			text = "› " + text
		} else {
			text = "  " + text
		}
		texts = append(texts, text)
		boxWidth = max(boxWidth, rw.StringWidth(text))
	}
	boxWidth = min(boxWidth, maxWidth)
	x := min(max(0, cursorX), max(0, width-boxWidth))
	var lines []string
	for i, text := range texts {
		color := "muted"
		if start+i == p.selected {
			color = "accent"
		}
		text += strings.Repeat(" ", max(0, boxWidth-rw.StringWidth(text)))
		lines = append(lines, style.Styled(style.Escape(text), color, ""))
	}
	w := style.NewLiteralParagraph()
	noBorder(&w.Block)
	w.Text = strings.Join(lines, "\n")
	w.SetRect(x, cursorY-rows, x+boxWidth, cursorY)
	return w
}

// referencePopupMaxWidth caps the completion popup; longer rows truncate.
const referencePopupMaxWidth = 76
