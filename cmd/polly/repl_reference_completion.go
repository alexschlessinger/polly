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
	m.referencesPopup = nil
	var current *composerReference
	for _, ref := range scanComposerReferenceTokens(m.ed.text()) {
		if m.ed.cursor > ref.start && m.ed.cursor <= ref.end {
			copy := ref
			current = &copy
			break
		}
	}
	if current == nil {
		return
	}
	ref := *current
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
			add(path, fileReference(path), "file")
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
		m.slashHintsHidden = true
		m.setSlashHintLine("")
		m.referencesPopup = nil
		return true
	case "<Tab>", "<Enter>":
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
	boxWidth := min(76, width)
	x := min(max(0, cursorX), max(0, width-boxWidth))
	var lines []string
	for _, choice := range p.choices[start:min(start+rows, len(p.choices))] {
		text := rw.Truncate(choice.text+"  "+choice.description, boxWidth-2, "…")
		color := "muted"
		if choice.text == p.choices[p.selected].text {
			color = "accent"
			text = "› " + text
		} else {
			text = "  " + text
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
