package main

import (
	"fmt"
	"image"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

type replModalItem struct {
	label           string
	value           string
	identity        string // Stable session identity when its display changes.
	searchText      string // full text, independent of truncated display columns
	display         string
	selectedDisplay string
	// parent is the value of the item this one nests under; such an item
	// is listed only while its parent is expanded or a filter matches it.
	parent string
	// children counts the items nesting under this one.
	children   int
	nestDetail string
}

// replModal is shared by provider/model selection and masked credential input.
// It is intentionally display-only state: none of its text enters the composer,
// transcript, input history, or durable session metadata.
type replModal struct {
	title      string
	items      []replModalItem
	selected   int
	top        int
	visible    int
	listBounds image.Rectangle
	width      int
	maxRows    int
	showCount  bool
	input      lineEditor
	inputMode  bool
	masked     bool
	helper     string
	// body is styled rows shown above the list (an approval's call block);
	// bodyRows is how many rows it took in the last text, list rows follow.
	body     []string
	bodyRows int
	// expanded holds the values of parent items whose children are listed.
	// Sharing the map across openings keeps the choice for the process.
	expanded    map[string]bool
	refresh     func()
	picker      *sessionsPicker
	onSubmit    func(string)
	onClear     func()
	onEditTitle func(string)
	onCancel    func()
	onDraft     func(string)
}

func (m *replModal) wipe() {
	for i := range m.input.buf {
		m.input.buf[i] = 0
	}
	m.input.clear()
}

func (m *replModal) filteredItems() []replModalItem {
	if m == nil || m.inputMode {
		return m.items
	}
	needle := strings.ToLower(strings.TrimSpace(m.input.text()))
	if needle == "" {
		if !m.nested() {
			return m.items
		}
		out := make([]replModalItem, 0, len(m.items))
		for _, item := range m.items {
			if item.parent == "" || m.expanded[item.parent] {
				out = append(out, item)
			}
		}
		return out
	}
	// A filter reaches collapsed children too: it is how a name from a
	// spawn reply is found without knowing which parent to expand.
	out := make([]replModalItem, 0, len(m.items))
	for _, item := range m.items {
		if strings.Contains(strings.ToLower(item.label), needle) || strings.Contains(strings.ToLower(item.value), needle) || strings.Contains(strings.ToLower(item.searchText), needle) {
			out = append(out, item)
		}
	}
	return out
}

// nested reports whether any item nests under another.
func (m *replModal) nested() bool {
	for _, item := range m.items {
		if item.parent != "" {
			return true
		}
	}
	return false
}

// toggle expands or collapses the parent of the selected row. Right expands
// a parent; Left collapses a parent or, on a child, its parent, which then
// takes the selection. It reports whether the list changed.
func (m *replModal) toggle(expand bool) bool {
	items := m.filteredItems()
	if len(items) == 0 {
		return false
	}
	m.selected = min(m.selected, len(items)-1)
	item := items[m.selected]
	target := item.value
	if item.parent != "" {
		if expand {
			return false
		}
		target = item.parent
	} else if item.children == 0 {
		return false
	}
	if m.expanded[target] == expand {
		return false
	}
	if m.expanded == nil {
		m.expanded = make(map[string]bool)
	}
	m.expanded[target] = expand
	for i, item := range m.filteredItems() {
		if item.value == target {
			m.selected = i
			break
		}
	}
	return true
}

// nestMarker names an item's collapsed or expanded children.
func (m *replModal) nestMarker(item replModalItem) string {
	if item.children == 0 {
		return ""
	}
	glyph := "▸"
	if m.expanded[item.value] {
		glyph = "▾"
	}
	noun := "agents"
	if item.children == 1 {
		noun = "agent"
	}
	marker := fmt.Sprintf("%s %d %s", glyph, item.children, noun)
	if item.nestDetail != "" {
		marker += " · " + item.nestDetail
	}
	return marker
}

func (m *replModal) text(maxRows, modalWidth int) string {
	if m.inputMode {
		value := m.input.text()
		if m.masked {
			value = strings.Repeat("•", len([]rune(value)))
		}
		if value == "" {
			return userGutter() + styled("type a value", "muted", "") + "\n\n" + centeredModalHelper(m.helper, modalWidth)
		}
		return userGutter() + styleEscape(value) + "\n\n" + centeredModalHelper(m.helper, modalWidth)
	}
	items := m.filteredItems()
	if len(items) == 0 {
		m.top, m.selected, m.visible = 0, 0, 0
		return styled("No matches", "muted", "") + "\n\n" + centeredModalHelper("type to filter · Esc back", modalWidth)
	}
	m.selected = max(0, min(m.selected, len(items)-1))
	start := 0
	visibleRows := maxRows
	if m.maxRows > 0 {
		visibleRows = min(visibleRows, m.maxRows)
	}
	// The list is a window over the items: it scrolls only as far as the
	// selection needs, so paging and the scrollbar keep a stable origin.
	m.top = max(0, min(m.top, len(items)-visibleRows))
	if m.selected < m.top {
		m.top = m.selected
	}
	if m.selected >= m.top+visibleRows {
		m.top = m.selected - visibleRows + 1
	}
	start = m.top
	end := min(len(items), start+visibleRows)
	m.visible = end - start
	lines := make([]string, 0, end-start+2)
	for i := start; i < end; i++ {
		prefix := "  "
		line := styleEscape(items[i].label)
		if items[i].display != "" {
			line = items[i].display
		}
		if i == m.selected {
			prefix = styled("›", "accent", "bold") + " "
			if items[i].selectedDisplay != "" {
				line = items[i].selectedDisplay
			} else {
				line = styled(items[i].label, "accent", "bold")
			}
		}
		if marker := m.nestMarker(items[i]); marker != "" {
			line += "  " + styled(marker, "muted", "")
		}
		lines = append(lines, prefix+line)
	}
	filter := m.input.text()
	footer := ""
	if m.showCount {
		count := ""
		if filter != "" {
			count = fmt.Sprintf("%d matches", len(items))
			if len(items) == 1 {
				count = "1 match"
			}
			filter = rw.Truncate("/"+filter, 10, "…")
		} else {
			if m.onEditTitle != nil {
				count = fmt.Sprintf("%d–%d/%d", start+1, end, len(items))
				filter = "filter"
			} else {
				count = fmt.Sprintf("%d–%d of %d", start+1, end, len(items))
				filter = "type filter"
			}
		}
		footer = count + " · " + filter + " · ↑↓ · Enter open"
		if m.onEditTitle != nil {
			footer += " · F2 edit title"
		}
		if m.nested() {
			footer += " · → agents"
		}
		footer += " · Esc"
	} else {
		if filter == "" {
			filter = "type to filter"
		}
		footer = filter + " · ↑/↓ select · Enter choose · Esc close"
	}
	lines = append(lines, "", centeredModalHelper(footer, modalWidth))
	m.bodyRows = 0
	if len(m.body) > 0 {
		m.bodyRows = len(m.body) + 1
		lines = append(append(append([]string(nil), m.body...), ""), lines...)
	}
	return strings.Join(lines, "\n")
}

func centeredModalHelper(text string, modalWidth int) string {
	padding := max(0, (modalWidth-2-rw.StringWidth(text))/2)
	return strings.Repeat(" ", padding) + styled(text, "muted", "")
}

// modalParagraph clears its complete rectangle before drawing. This makes a
// ColorClear modal opaque while still honoring the terminal's own background.
type modalParagraph struct {
	*widgets.Paragraph
	scrollbar scrollbar
}

func newModalParagraph() *modalParagraph {
	p := widgets.NewParagraph()
	p.TextStyle = ui.NewStyle(ui.ColorClear)
	p.WrapText = false
	p.BorderRounded = true
	p.BorderStyle = ui.NewStyle(ui.ColorGrey)
	p.TitleStyle = ui.NewStyle(ui.ColorBlue, ui.ColorClear, ui.ModifierBold)
	return &modalParagraph{Paragraph: p}
}

func (p *modalParagraph) Draw(buf *ui.Buffer) {
	for y := p.Min.Y; y < p.Max.Y; y++ {
		for x := p.Min.X; x < p.Max.X; x++ {
			buf.SetCell(ui.Cell{Rune: ' ', Style: ui.StyleClear}, image.Pt(x, y))
		}
	}
	p.Paragraph.Draw(buf)
	restoreStyledLiterals(buf, p.Inner)
	p.scrollbar.draw(buf)
}

var modelPresets = map[string][]string{
	"openai":      {"gpt-5.4", "gpt-5.4-mini"},
	"anthropic":   {"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5"},
	"gemini":      {"gemini-3.1-pro-preview", "gemini-3.1-flash-preview"},
	"ollama":      {"gpt-oss", "llama3.2"},
	"deepseek":    {"deepseek-v4-pro", "deepseek-v4-flash"},
	"openrouter":  {"anthropic/claude-sonnet-4-6", "openai/gpt-5.4"},
	"huggingface": {},
}

func (r *managedREPL) closeModal() {
	if r.model.modal != nil {
		r.model.modal.wipe()
	}
	r.model.modal = nil
	r.modalScrollbar = scrollbar{}
	r.scrollDrag = scrollDragState{}
}

func (r *managedREPL) openModal(modal *replModal) {
	// The startup mark owns a separate text band and may also be a native
	// Kitty/Sixel placement. Release both before drawing an interactive layer;
	// otherwise the native image can be composited over the modal afterward.
	r.startupLogoVisible = false
	r.model.modal = modal
}

func (r *managedREPL) openModelPicker() {
	items := make([]replModalItem, 0, len(validModelProviders))
	currentProvider, _, _ := strings.Cut(r.currentModel(), "/")
	selected := 0
	for i, provider := range validModelProviders {
		detail := r.providerCredentialDetail(provider)
		if provider == currentProvider {
			detail += " · current"
			selected = i
		}
		items = append(items, replModalItem{label: fmt.Sprintf("%-12s %s", provider, detail), value: provider})
	}
	r.openModal(&replModal{
		title: "Select provider", items: items, selected: selected,
		onSubmit: r.openProviderModels,
	})
}

func (r *managedREPL) providerCredentialDetail(provider string) string {
	if provider == "ollama" {
		return "local / key optional"
	}
	if r.state != nil && r.state.agent != nil {
		if source := r.state.agent.ProviderAPIKeySource(provider); source != "" {
			return "key: " + source
		}
	}
	return "no key"
}

func (r *managedREPL) openProviderModels(provider string) {
	seen := make(map[string]bool)
	var models []string
	add := func(model string) {
		if model == "" || seen[model] {
			return
		}
		seen[model] = true
		models = append(models, model)
	}
	current := r.currentModel()
	if strings.HasPrefix(current, provider+"/") {
		add(current)
	}
	for _, recent := range r.model.status.recentModels {
		if strings.HasPrefix(recent, provider+"/") {
			add(recent)
		}
	}
	for _, name := range modelPresets[provider] {
		add(provider + "/" + name)
	}
	items := make([]replModalItem, 0, len(models)+1)
	for _, model := range models {
		label := strings.TrimPrefix(model, provider+"/")
		if model == current {
			label += "  current"
		}
		items = append(items, replModalItem{label: label, value: model})
	}
	items = append(items, replModalItem{label: "Enter model manually…", value: ""})
	r.openModal(&replModal{
		title: "Select " + provider + " model", items: items,
		onSubmit: func(model string) {
			if model == "" {
				r.openManualModel(provider)
				return
			}
			r.applySelectedModel(model)
		},
	})
}

func (r *managedREPL) openManualModel(provider string) {
	r.openModal(&replModal{
		title: "Enter " + provider + " model", inputMode: true,
		helper: "Enter save · Esc cancel",
		onSubmit: func(name string) {
			name = strings.TrimSpace(name)
			if !strings.Contains(name, "/") || !strings.HasPrefix(name, provider+"/") {
				name = provider + "/" + strings.TrimPrefix(name, "/")
			}
			r.applySelectedModel(name)
		},
	})
}

func (r *managedREPL) applySelectedModel(model string) {
	_, name, ok := strings.Cut(model, "/")
	if !ok || strings.TrimSpace(name) == "" {
		r.model.appendNoticeLine("Enter a model name")
		return
	}
	line, err := applyAndPersistSetting(newManagedReplCommandContext(r), "model", model)
	if err != nil {
		r.model.appendNoticeLine("Model change failed · " + err.Error())
		return
	}
	r.model.appendNoticeLine(line)
}

func (r *managedREPL) openKeyManager() {
	items := make([]replModalItem, 0, len(validModelProviders))
	currentProvider, _, _ := strings.Cut(r.currentModel(), "/")
	selected := 0
	for i, provider := range validModelProviders {
		if provider == currentProvider {
			selected = i
		}
		items = append(items, replModalItem{
			label: fmt.Sprintf("%-12s %s", provider, r.providerCredentialDetail(provider)), value: provider,
		})
	}
	r.openModal(&replModal{
		title: "Provider keys", items: items, selected: selected,
		onSubmit: r.openProviderKeyInput,
	})
}

// expandPickerParent lists name's agents in the session picker from now on.
func (r *managedREPL) expandPickerParent(name string) {
	if r.pickerExpanded == nil {
		r.pickerExpanded = make(map[string]bool)
	}
	r.pickerExpanded[name] = true
}

func formatSessionMessageCount(count int) string {
	unit := "msgs"
	if count == 1 {
		unit = "msg"
	}
	return humanizeTokens(count) + " " + unit
}

func formatCompactDuration(d time.Duration) string {
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (r *managedREPL) openProviderKeyInput(provider string) {
	r.openModal(&replModal{
		title: provider + " session key", inputMode: true, masked: true,
		helper: "Enter save · Ctrl-D clear session key · Esc cancel",
		onSubmit: func(key string) {
			if key == "" {
				return
			}
			if r.state == nil || r.state.agent == nil || !r.state.agent.SetProviderAPIKey(provider, key) {
				r.model.appendNoticeLine("Key manager unavailable · no provider router")
				return
			}
			// A missing key may have made model-metadata discovery fail earlier
			// in this process. Let models for this provider retry on next use.
			for model, window := range r.state.contextWindows {
				if window == 0 && strings.HasPrefix(model, provider+"/") {
					delete(r.state.contextWindows, model)
				}
			}
			r.model.appendNoticeLine("Configured the " + provider + " key for this run")
		},
		onClear: func() {
			if r.state != nil && r.state.agent != nil && r.state.agent.ClearProviderAPIKey(provider) {
				r.model.appendNoticeLine("Cleared the " + provider + " key for this run")
			}
		},
	})
}

func (r *managedREPL) handleModalEvent(e ui.Event) bool {
	m := r.model.modal
	if m == nil {
		return false
	}
	if e.ID == "<C-z>" {
		r.requestSuspend()
		return true
	}
	if e.ID == "<C-c>" {
		r.closeModal()
		return true
	}
	if e.Type == ui.MouseEvent {
		mouse, ok := e.Payload.(ui.Mouse)
		if !ok || m.inputMode {
			return true
		}
		switch e.ID {
		case "<MouseWheelUp>":
			m.selected = max(0, m.selected-3)
		case "<MouseWheelDown>":
			m.selected = min(len(m.filteredItems())-1, m.selected+3)
		case "<MouseLeft>":
			if !image.Pt(mouse.X, mouse.Y).In(m.listBounds) {
				return true
			}
			index := m.top + mouse.Y - m.listBounds.Min.Y
			if index >= 0 && index < len(m.filteredItems()) {
				m.selected = index
				return r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			}
		}
		return true
	}
	switch e.ID {
	case "<PageUp>", "<PageDown>", "<Home>", "<End>":
		if !m.inputMode {
			switch e.ID {
			case "<PageUp>":
				m.selected -= max(1, m.visible-1)
			case "<PageDown>":
				m.selected += max(1, m.visible-1)
			case "<Home>":
				m.selected = 0
			case "<End>":
				m.selected = len(m.filteredItems()) - 1
			}
			m.selected = max(0, min(m.selected, len(m.filteredItems())-1))
		}
	case "<Escape>":
		cancel := m.onCancel
		if m.onDraft != nil {
			m.onDraft(m.input.text())
		}
		r.closeModal()
		if cancel != nil {
			cancel()
		}
	case "<Up>":
		if !m.inputMode {
			m.selected = max(0, m.selected-1)
		}
	case "<Down>":
		if !m.inputMode {
			m.selected = min(len(m.filteredItems())-1, m.selected+1)
		}
	case "<Enter>":
		value := m.input.text()
		if !m.inputMode {
			items := m.filteredItems()
			if len(items) == 0 {
				return true
			}
			m.selected = min(m.selected, len(items)-1)
			value = items[m.selected].value
		}
		submit := m.onSubmit
		r.closeModal()
		if submit != nil {
			submit(value)
		}
	case "<Right>", "<Left>":
		if !m.inputMode {
			m.toggle(e.ID == "<Right>")
		}
	case "<F2>":
		if m.inputMode || m.onEditTitle == nil {
			break
		}
		items := m.filteredItems()
		if len(items) == 0 {
			return true
		}
		m.selected = min(m.selected, len(items)-1)
		editTitle := m.onEditTitle
		value := items[m.selected].value
		r.closeModal()
		editTitle(value)
	case "<C-d>":
		if m.inputMode && m.onClear != nil {
			clear := m.onClear
			r.closeModal()
			clear()
		}
	case "<C-j>":
		if m.inputMode && m.onDraft != nil {
			m.input.insert('\n')
		}
	case "<Backspace>", "<C-h>":
		m.input.backspace()
		m.selected = 0
	default:
		if ch, ok := printableRune(e); ok {
			m.input.insert(ch)
			m.selected = 0
		}
	}
	return true
}
