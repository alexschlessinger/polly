package main

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

type replModalItem struct {
	label           string
	value           string
	display         string
	selectedDisplay string
	// parent is the value of the item this one nests under; such an item
	// is listed only while its parent is expanded or a filter matches it.
	parent string
	// children counts the items nesting under this one.
	children int
}

// replModal is shared by provider/model selection and masked credential input.
// It is intentionally display-only state: none of its text enters the composer,
// transcript, input history, or durable session metadata.
type replModal struct {
	title     string
	items     []replModalItem
	selected  int
	width     int
	maxRows   int
	showCount bool
	input     lineEditor
	inputMode bool
	masked    bool
	helper    string
	// expanded holds the values of parent items whose children are listed.
	// Sharing the map across openings keeps the choice for the process.
	expanded map[string]bool
	onSubmit func(string)
	onClear  func()
	onRename func(string)
	onCancel func()
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
		if strings.Contains(strings.ToLower(item.label), needle) || strings.Contains(strings.ToLower(item.value), needle) {
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
	return fmt.Sprintf("%s %d %s", glyph, item.children, noun)
}

func (m *replModal) text(maxRows, modalWidth int) string {
	if m.inputMode {
		value := m.input.text()
		if m.masked {
			value = strings.Repeat("•", len([]rune(value)))
		}
		if value == "" {
			return "> " + styled("type a value", "muted", "") + "\n\n" + centeredModalHelper(m.helper, modalWidth)
		}
		return "> " + styleEscape(value) + "\n\n" + centeredModalHelper(m.helper, modalWidth)
	}
	items := m.filteredItems()
	if len(items) == 0 {
		return styled("No matches", "muted", "") + "\n\n" + centeredModalHelper("type to filter · Esc back", modalWidth)
	}
	if m.selected >= len(items) {
		m.selected = len(items) - 1
	}
	start := 0
	visibleRows := maxRows
	if m.maxRows > 0 {
		visibleRows = min(visibleRows, m.maxRows)
	}
	if len(items) > visibleRows && m.selected >= visibleRows {
		start = m.selected - visibleRows + 1
	}
	end := min(len(items), start+visibleRows)
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
			if m.onRename != nil {
				count = fmt.Sprintf("%d–%d/%d", start+1, end, len(items))
				filter = "filter"
			} else {
				count = fmt.Sprintf("%d–%d of %d", start+1, end, len(items))
				filter = "type filter"
			}
		}
		footer = count + " · " + filter + " · ↑↓ · Enter resume"
		if m.onRename != nil {
			footer += " · F2 rename"
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
	return strings.Join(lines, "\n")
}

func centeredModalHelper(text string, modalWidth int) string {
	padding := max(0, (modalWidth-2-rw.StringWidth(text))/2)
	return strings.Repeat(" ", padding) + styled(text, "muted", "")
}

// modalParagraph clears its complete rectangle before drawing. This makes a
// ColorClear modal opaque while still honoring the terminal's own background.
type modalParagraph struct{ *widgets.Paragraph }

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
		r.model.appendNoticeLine("model: enter a model name")
		return
	}
	line, err := applyAndPersistSetting(newManagedReplCommandContext(r), "model", model)
	if err != nil {
		r.model.appendNoticeLine("model: " + err.Error())
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

func (r *managedREPL) openResumePicker() {
	r.openResumePickerSelected("")
}

func (r *managedREPL) openResumePickerSelected(preferred string) {
	if r.state == nil || r.state.sessionStore == nil || r.state.session == nil {
		r.model.appendNoticeLine("session picker unavailable")
		return
	}
	ctx := r.state.session.Context()
	summaries, err := r.state.sessionStore.ListSummaries(ctx)
	if err != nil {
		r.model.appendNoticeLine("session picker: " + err.Error())
		return
	}
	current, err := r.state.session.GetName(ctx)
	if err != nil {
		r.model.appendNoticeLine("session picker: " + err.Error())
		return
	}
	infos := make([]sessions.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.Metadata != nil {
			infos = append(infos, summary)
		}
	}
	// Agents nest under the session that spawned them, collapsed until the
	// parent is expanded with Right, so a fan-out never buries the list.
	metas := make([]*sessions.Metadata, len(infos))
	for i, summary := range infos {
		metas[i] = summary.Metadata
	}
	nodes := sessionTree(metas)
	nameWidth := 0
	lengthWidth := 0
	for _, node := range nodes {
		summary := infos[node.Index]
		nameWidth = max(nameWidth, rw.StringWidth(sessionTreeName(summary.Metadata, node.Depth)))
		lengthWidth = max(lengthWidth, rw.StringWidth(formatSessionMessageCount(summary.MessageCount)))
	}
	nameWidth = min(nameWidth, 24)
	target := current
	if preferred != "" {
		target = preferred
	}
	// A session open in one of this polly's tabs is reached by showing that
	// tab. One leased by another polly cannot be opened here: Acquire would
	// wait out the lease timeout and then fail. Mark it and refuse up front.
	inUseElsewhere := make(map[string]bool)
	items := make([]replModalItem, 0, len(nodes))
	for _, node := range nodes {
		summary := infos[node.Index]
		info := summary.Metadata
		name := truncate(sessionTreeName(info, node.Depth), nameWidth)
		age := formatCompactDuration(time.Since(info.LastUsed))
		length := formatSessionMessageCount(summary.MessageCount)
		nameColumn := fmt.Sprintf("%-*s", nameWidth, name)
		ageColumn := fmt.Sprintf("%4s", age)
		lengthColumn := fmt.Sprintf("%*s", lengthWidth, length)
		label := nameColumn + "  " + ageColumn + "  " + lengthColumn
		display := styleEscape(nameColumn) + "  " + styled(ageColumn, "muted", "") + "  " + styled(lengthColumn, "muted", "")
		selectedDisplay := styled(nameColumn, "accent", "bold") + "  " + styled(ageColumn, "muted", "") + "  " + styled(lengthColumn, "muted", "")
		switch tab := r.tabIndexOf(info.Name); {
		case info.Name == current:
			label += "  current"
			display += "  " + styled("current", "accent", "")
			selectedDisplay += "  " + styled("current", "accent", "")
		case tab >= 0:
			mark := fmt.Sprintf("tab %d", tab+1)
			label += "  " + mark
			display += "  " + styled(mark, "ok", "")
			selectedDisplay += "  " + styled(mark, "ok", "")
		case summary.InUse:
			inUseElsewhere[info.Name] = true
			label += "  in use"
			display += "  " + styled("in use", "active", "")
			selectedDisplay += "  " + styled("in use", "active", "")
		}
		item := replModalItem{
			label: label, value: info.Name, display: display, selectedDisplay: selectedDisplay,
			children: node.Children,
		}
		if node.Depth > 0 {
			item.parent = info.Parent
		}
		if info.Name == target && item.parent != "" {
			r.expandPickerParent(item.parent)
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		r.model.appendNoticeLine("no saved sessions")
		return
	}
	m := &replModal{
		title: "Resume session", items: items, expanded: r.pickerExpanded,
		width: 64, maxRows: 14, showCount: true,
		onSubmit: func(name string) {
			if name == "" || name == current {
				return
			}
			if inUseElsewhere[name] {
				r.model.appendErrorLine(name + " is open in another polly")
				return
			}
			r.requestOpenLocked(name)
		},
		onRename: r.openSessionRenameInput,
	}
	if m.nested() {
		// Room for the agent count after a row's marks.
		m.width = 72
	}
	for i, item := range m.filteredItems() {
		if item.value == target {
			m.selected = i
			break
		}
	}
	r.openModal(m)
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

func (r *managedREPL) openSessionRenameInput(name string) {
	m := &replModal{
		title: "Rename session", inputMode: true, width: 64,
		helper:   "Enter save · Esc back",
		onCancel: func() { r.openResumePickerSelected(name) },
		onSubmit: func(newName string) { r.renameSession(name, newName) },
	}
	m.input.setText(name)
	r.openModal(m)
}

func (r *managedREPL) renameSession(oldName, newName string) {
	if oldName == "" || newName == "" || r.state == nil || r.state.sessionStore == nil || r.state.session == nil {
		r.model.appendNoticeLine("rename failed: session unavailable")
		return
	}
	if oldName == newName {
		r.openResumePickerSelected(oldName)
		return
	}
	ctx := r.state.session.Context()
	current, err := r.state.session.GetName(ctx)
	if err != nil {
		r.model.appendNoticeLine("rename failed: " + err.Error())
		return
	}
	target := r.state.session
	closeTarget := false
	// A session open in another tab is renamed through that tab's lease.
	tab := r.tabIndexOf(oldName)
	switch {
	case oldName == current:
	case tab >= 0:
		target = r.tabs[tab].state.session
	default:
		target, err = r.state.sessionStore.Acquire(ctx, oldName, sessions.AcquireOptions{})
		if err != nil {
			r.model.appendNoticeLine("rename failed: " + err.Error())
			r.openResumePickerSelected(oldName)
			return
		}
		closeTarget = true
	}
	if err := target.Rename(ctx, newName); err != nil {
		if closeTarget {
			_ = target.Close()
		}
		r.model.appendNoticeLine("rename failed: " + err.Error())
		r.openResumePickerSelected(oldName)
		return
	}
	switch {
	case closeTarget:
		if err := target.Close(); err != nil {
			r.model.appendNoticeLine("renamed session; releasing it failed: " + err.Error())
		}
	case oldName == current:
		r.model.status.contextName = newName
		if i := r.visibleTabIndex(); i >= 0 {
			r.tabs[i].name = newName
		}
	default:
		hidden := r.tabs[tab]
		hidden.name = newName
		hidden.model.mu.Lock()
		hidden.model.status.contextName = newName
		hidden.model.mu.Unlock()
	}
	r.model.appendNoticeLine("renamed session '" + oldName + "' to '" + newName + "'")
	r.openResumePickerSelected(newName)
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
				r.model.appendNoticeLine("key manager: provider router unavailable")
				return
			}
			// A missing key may have made model-metadata discovery fail earlier
			// in this process. Let models for this provider retry on next use.
			for model, window := range r.state.contextWindows {
				if window == 0 && strings.HasPrefix(model, provider+"/") {
					delete(r.state.contextWindows, model)
				}
			}
			r.model.appendNoticeLine(provider + " key configured for this run")
		},
		onClear: func() {
			if r.state != nil && r.state.agent != nil && r.state.agent.ClearProviderAPIKey(provider) {
				r.model.appendNoticeLine(provider + " session key cleared")
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
		switch e.ID {
		case "<MouseWheelUp>":
			m.selected = max(0, m.selected-1)
		case "<MouseWheelDown>":
			m.selected++
		}
		return true
	}
	switch e.ID {
	case "<Escape>":
		cancel := m.onCancel
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
		if m.inputMode || m.onRename == nil {
			break
		}
		items := m.filteredItems()
		if len(items) == 0 {
			return true
		}
		m.selected = min(m.selected, len(items)-1)
		rename := m.onRename
		value := items[m.selected].value
		r.closeModal()
		rename(value)
	case "<C-d>":
		if m.inputMode && m.onClear != nil {
			clear := m.onClear
			r.closeModal()
			clear()
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
