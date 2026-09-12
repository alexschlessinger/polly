package main

import (
	"context"
	"fmt"
	"image"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	tcell "github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// modelForm is a disposable draft. Only Apply writes settings or installs a key.
// The key editor is never copied into messages, config, history, or metadata.
type modelForm struct {
	modal           *replModal
	provider        string
	initialProvider string
	initialModel    string
	model, key      lineEditor
	contextLimit    lineEditor
	contextSettings Settings
	contextChanged  bool
	keyChanged      bool
	keySource       string
	focus           int // provider, model, key, context, Apply
	completing      bool
	completionQuery string
	hasKey          bool
	providerBounds  [2]image.Rectangle
	applyBounds     image.Rectangle
	selected        int
	suggestions     []string
	catalog         llm.ModelCatalog
	infos           map[string]llm.ModelInfo
	details         map[string]llm.ModelInfo
	requested       map[string]bool
	status, err     string
	revision        uint64
	cancel          context.CancelFunc
	ctx             context.Context
	fieldRows       [5]int
	pasting         bool
}

func (f *modelForm) wipe() {
	if f.cancel != nil {
		f.cancel()
	}
	f.revision++
	for i := range f.key.buf {
		f.key.buf[i] = 0
	}
	f.key.clear()
}
func (r *managedREPL) openModelForm(focus int) {
	provider, name, _ := strings.Cut(r.currentModel(), "/")
	if !slices.Contains(validModelProviders, provider) {
		provider = validModelProviders[0]
		name = ""
	}
	if provider == "openrouter" && r.state != nil && r.state.settings.ModelHost != "" {
		name += ":" + r.state.settings.ModelHost
	}
	f := &modelForm{provider: provider, initialProvider: provider, initialModel: name, focus: focus, selected: -1, infos: map[string]llm.ModelInfo{}, requested: map[string]bool{}}
	if r.state != nil {
		f.contextSettings = r.state.settings.clone()
	} else if r.config != nil {
		f.contextSettings = r.config.Launch.clone()
	}
	f.model.setText(name)
	f.modal = &replModal{title: "Model", width: 76, modelForm: f}
	r.modelFormKeySource(f)
	r.openModal(f.modal)
	r.fetchFormCatalog(f, false)
}
func (r *managedREPL) modelFormKeySource(f *modelForm) {
	f.keySource = "No key configured"
	f.hasKey = false
	if r.state != nil && r.state.agent != nil {
		switch r.state.agent.ProviderAPIKeySource(f.provider) {
		case "session":
			f.keySource = "Using process override"
			f.hasKey = true
		case "environment":
			f.keySource = "Using environment key"
			f.hasKey = true
		}
	}
	if f.provider == "ollama" && f.keySource == "No key configured" {
		f.keySource = "Local provider · key optional"
	}
}
func (f *modelForm) text(maxRows, width int) string {
	var rows []string
	inner := max(1, width-2)
	add := func(s string) {
		rows = append(rows, style.Escape(rw.Truncate(metadataDisplayText(s, false), inner, "…")))
	}
	field := func(n int, label, value string) {
		f.fieldRows[n] = len(rows)
		prefix := "  "
		if f.focus == n {
			prefix = "› "
		}
		line := prefix + fmt.Sprintf("%-9s", label) + value
		if f.focus == n {
			rows = append(rows, style.Styled(rw.Truncate(metadataDisplayText(line, false), inner, "…"), "accent", "bold"))
		} else {
			add(line)
		}
	}
	field(0, "Provider", "‹ "+f.provider+" ›")
	f.providerBounds[0] = image.Rect(11, f.fieldRows[0], min(inner, 13), f.fieldRows[0]+1)
	right := 13 + rw.StringWidth(f.provider)
	f.providerBounds[1] = image.Rect(right, f.fieldRows[0], min(inner, right+2), f.fieldRows[0]+1)
	value := f.model.text()
	if f.focus == 1 {
		value = formEditorText(&f.model, false, max(1, inner-12))
	}
	field(1, "Model", value)
	if f.focus == 1 && f.model.cursor == len(f.model.buf) {
		available := max(0, inner-11-rw.StringWidth(value))
		if available > 0 {
			shadow := rw.Truncate(metadataDisplayText(f.completionShadow(), false), available, "…")
			rows[f.fieldRows[1]] += style.Styled(shadow, "muted", "dim")
		}
	}

	key := ""
	if f.hasKey {
		key = "********"
	}
	if f.keyChanged {
		key = strings.Repeat("*", len(f.key.buf))
	}
	if f.focus == 2 {
		if !f.keyChanged && f.hasKey {
			key += "│"
		} else {
			key = formEditorText(&f.key, true, max(1, inner-12))
		}
	}
	field(2, "Key", key)
	f.syncContextLimit()
	contextSize := f.contextLimit.text()
	if f.focus == 3 {
		contextSize = formEditorText(&f.contextLimit, false, max(1, inner-12))
	}
	field(3, "Context", contextSize)
	status, color := f.keySource, ui.ColorGrey
	if status == "No key configured" || status == "Using environment key" {
		status = ""
	}
	if f.status != "" {
		status = f.status
	}

	if f.err != "" {
		status, color = f.err, ui.ColorRed
	}
	f.modal.titleNotice = ""
	if status != "" {
		f.modal.titleNotice = "(" + rw.Truncate(metadataDisplayText(status, false), max(1, width-12), "…") + ")"
	}
	f.modal.titleNoticeColor = color
	rows = append(rows, "")
	f.fieldRows[4] = len(rows)
	button := "[ Apply ]"
	if f.focus == 4 {
		button = "› " + button
	}
	x := max(0, inner-rw.StringWidth(button))
	f.applyBounds = image.Rect(x, len(rows), inner, len(rows)+1)
	buttonColor, modifier := "", ""
	if f.focus == 4 {
		buttonColor, modifier = "accent", "bold"
	}
	if f.contextChanged || f.keyChanged || f.provider != f.initialProvider || strings.TrimSpace(f.model.text()) != f.initialModel {
		buttonColor = "active"
	}
	left := ""
	if pricing := f.modelPricing(); pricing != "" && x > 3 {
		left = "  " + rw.Truncate(metadataDisplayText(pricing, false), x-3, "…")
	}
	rows = append(rows, style.Escape(left)+strings.Repeat(" ", max(0, x-rw.StringWidth(left)))+style.Styled(button, buttonColor, modifier))
	f.modal.visible = 0 // the form owns its field and provider mouse bounds
	return strings.Join(rows, "\n")
}

// completionShadow previews the next Tab result without modifying the editor.
func (f *modelForm) completionShadow() string {
	if len(f.suggestions) == 0 {
		return ""
	}
	next := 0
	if f.completing {
		next = (f.selected + 1) % len(f.suggestions)
	}
	candidate := f.suggestions[next]
	value := f.model.text()
	if candidate == value {
		return ""
	}
	prefix, full := []rune(value), []rune(candidate)
	if len(prefix) <= len(full) && strings.EqualFold(value, string(full[:len(prefix)])) {
		return string(full[len(prefix):])
	}
	return " → " + candidate
}

func formEditorText(ed *lineEditor, masked bool, width int) string {
	chars := append([]rune(nil), ed.buf...)
	if masked {
		for i := range chars {
			chars[i] = '*'
		}
	}
	pos := min(ed.cursor, len(chars))
	before, after := string(chars[:pos]), string(chars[pos:])
	// Keep the insertion point visible while editing long identifiers or keys.
	for rw.StringWidth(before) > max(0, width-2) {
		_, size := utf8.DecodeRuneInString(before)
		before = before[size:]
	}
	return rw.Truncate(before+"│"+after, width, "…")
}

func textTools(c llm.ModelCapabilities) bool {
	if c.Tools == nil || !*c.Tools || c.Chat != nil && !*c.Chat {
		return false
	}
	if c.InputModalities != nil && !slices.Contains(c.InputModalities, "text") {
		return false
	}
	if c.OutputModalities != nil && !slices.Contains(c.OutputModalities, "text") {
		return false
	}
	return c.Chat != nil && *c.Chat || slices.Contains(c.InputModalities, "text") && slices.Contains(c.OutputModalities, "text")
}
func liveModelHost(e llm.ModelEndpointInfo) bool {
	return e.Status == "" || e.Status == "live" || e.Status == "0"
}
func (f *modelForm) updateSuggestions() {
	previous := ""
	if f.selected >= 0 && f.selected < len(f.suggestions) {
		previous = f.suggestions[f.selected]
	}
	query := f.model.text()
	if f.completing {
		query = f.completionQuery
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var out []string
	for _, info := range f.infos {
		if textTools(info.ModelCapabilities) || textTools(info.EffectiveCapabilities("")) {
			out = append(out, info.ID)
		}
		if f.provider == "openrouter" || f.provider == "huggingface" {
			for _, e := range info.Endpoints {
				if liveModelHost(e) && textTools(info.EffectiveCapabilities(e.ID)) {
					out = append(out, info.ID+":"+e.ID)
				}
			}
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	f.suggestions = nil
	for _, name := range out {
		if strings.Contains(strings.ToLower(name), query) {
			f.suggestions = append(f.suggestions, name)
		}
	}
	f.selected = -1
	if previous != "" {
		f.selected = slices.Index(f.suggestions, previous)
	}
}

// A stale catalog refresh can arrive after its lazy details. Keep the details
// already fetched under this credential and refresh revision.
func (f *modelForm) setCatalog(cat llm.ModelCatalog) {
	f.catalog = cat
	f.infos = make(map[string]llm.ModelInfo, len(cat.Models))
	for _, info := range cat.Models {
		if detail, ok := f.details[info.ID]; ok {
			info = detail
		}
		f.infos[info.ID] = info
	}
}
func (f *modelForm) target(r *managedREPL) llm.ModelTarget {
	t := r.browserTarget(f.provider, "")
	if f.keyChanged {
		t.APIKey = strings.TrimSpace(f.key.text())
		t.UseConfiguredKey = t.APIKey == ""
	}
	return t
}
func (r *managedREPL) fetchFormCatalog(f *modelForm, force bool) {
	if f.cancel != nil {
		f.cancel()
	}
	f.revision++
	f.requested = map[string]bool{}
	f.details = map[string]llm.ModelInfo{}
	if r.state == nil || r.state.agent == nil || r.work == nil {
		f.status = ""
		return
	}
	f.ctx, f.cancel = context.WithCancel(r.work.ctx)
	ctx := f.ctx
	state := r.state
	target := f.target(r)
	revision := f.revision
	identity := state.agent.ModelMetadataIdentity(target)
	f.status = "Loading suggestions…"
	r.background(func() {
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		apply := func(cat llm.ModelCatalog, err error) {
			r.postUI(ctx, func() {
				r.model.mu.Lock()
				defer r.model.mu.Unlock()
				if ctx.Err() != nil || r.state != state || r.model.modal != f.modal || f.revision != revision || state.agent.ModelMetadataIdentity(target) != identity {
					return
				}
				f.setCatalog(cat)
				f.status = ""
				if cat.Partial {
					f.status = "Partial catalog"
				}
				if cat.Stale {
					f.status = "Cached catalog · refreshing…"
				}
				if err != nil || cat.Error != "" {
					f.status = ""
				}
				f.updateSuggestions()
				r.fetchFormDetails(f, force)
			})
		}
		cat, err := state.agent.ListModels(callCtx, target, force)
		apply(cat, err)
		if cat.Stale && cat.Error == "" && !force {
			cat, err = state.agent.ListModels(callCtx, target, true)
			apply(cat, err)
		}
	})
}

// Detail-only catalogs (notably Ollama) are resolved lazily for the typed prefix.
// Routed model details also provide eligible :host completions.
func (r *managedREPL) fetchFormDetails(f *modelForm, force bool) {
	if f.ctx == nil || f.ctx.Err() != nil || r.state == nil || r.state.agent == nil {
		return
	}
	query := strings.ToLower(strings.TrimSpace(f.model.text()))
	if len(query) < 2 {
		return
	}
	var names []string
	for id, info := range f.infos {
		if f.requested[id] {
			continue
		}
		if (query == strings.ToLower(id) && info.ContextWindow() == 0) || (f.provider == "ollama" && strings.Contains(strings.ToLower(id), query)) || (info.Routed && (query == strings.ToLower(id) || strings.HasPrefix(query, strings.ToLower(id)+":"))) {
			names = append(names, id)
		}
	}
	slices.Sort(names)
	names = names[:min(len(names), 8)]
	ctx := f.ctx
	state := r.state
	target := f.target(r)
	revision := f.revision
	identity := state.agent.ModelMetadataIdentity(target)
	for _, name := range names {
		f.requested[name] = true
		t := target
		t.Model = name
		r.background(func() {
			callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			cat, err := state.agent.LookupModel(callCtx, t, force)
			r.postUI(ctx, func() {
				r.model.mu.Lock()
				defer r.model.mu.Unlock()
				if ctx.Err() != nil || r.state != state || r.model.modal != f.modal || revision != f.revision || state.agent.ModelMetadataIdentity(target) != identity {
					return
				}
				if err == nil && len(cat.Models) > 0 {
					f.infos[name] = cat.Models[0]
					f.details[name] = cat.Models[0]
					f.updateSuggestions()
				}
			})
		})
	}
}

func (r *managedREPL) handleModelFormEvent(f *modelForm, e ui.Event) bool {
	// gotui does not name legacy Backtab events; advanced terminals report Shift-Tab.
	if key, ok := e.Payload.(*tcell.EventKey); ok && (key.Key() == tcell.KeyBacktab || key.Key() == tcell.KeyTab && key.Modifiers()&tcell.ModShift != 0) {
		e.ID = "<S-Tab>"
	}

	if e.ID == pasteStartID {
		f.pasting = true
		return true
	}
	if e.ID == pasteEndID {
		f.pasting = false
		return true
	}
	if f.pasting && (e.ID == "<Enter>" || e.ID == "<Tab>" || e.ID == "<C-j>") {
		return true
	}
	if e.Type == ui.MouseEvent {
		mouse, ok := e.Payload.(ui.Mouse)
		if !ok {
			return true
		}
		p := image.Pt(mouse.X, mouse.Y)
		if !p.In(f.modal.bounds) {
			return true
		}
		local := p.Sub(f.modal.bounds.Min.Add(image.Pt(1, 1)))
		if e.ID == "<MouseLeft>" {
			for i, bounds := range f.providerBounds {
				if local.In(bounds) {
					r.focusModelForm(f, 0)
					key := "<Left>"
					if i == 1 {
						key = "<Right>"
					}
					r.handleModelFormEvent(f, ui.Event{Type: ui.KeyboardEvent, ID: key})
					return true
				}
			}
			if local.In(f.applyBounds) {
				r.focusModelForm(f, 4)
				r.applyModelForm(f)
				return true
			}
			for n, y := range f.fieldRows[:4] {
				if local.Y == y {
					r.focusModelForm(f, n)
					break
				}
			}
		} else if e.ID == "<MouseWheelDown>" || e.ID == "<MouseWheelUp>" {
			id := "<Down>"
			if e.ID == "<MouseWheelUp>" {
				id = "<Up>"
			}
			return r.handleModelFormEvent(f, ui.Event{Type: ui.KeyboardEvent, ID: id})
		}
		return true
	}
	switch e.ID {
	case "<Escape>":
		r.closeModal()
	case "<Tab>", "<Backtab>", "<S-Tab>":
		if f.focus == 1 {
			if !f.completing {
				f.completionQuery = f.model.text()
				f.completing = true
				f.selected = -1
				f.updateSuggestions()
			}
			if len(f.suggestions) > 0 {
				delta := 1
				if e.ID != "<Tab>" {
					delta = -1
				}
				if f.selected < 0 && delta < 0 {
					f.selected = 0
				}
				f.selected = (f.selected + delta + len(f.suggestions)) % len(f.suggestions)
				f.model.setText(f.suggestions[f.selected])
			}
			r.fetchFormDetails(f, false)
		}
	case "<C-r>":
		r.fetchFormCatalog(f, true)
	case "<Up>", "<Down>":
		delta := 1
		if e.ID == "<Up>" {
			delta = -1
		}
		r.focusModelForm(f, max(0, min(4, f.focus+delta)))
	case "<Left>", "<Right>":
		if f.focus == 0 {
			delta := 1
			if e.ID == "<Left>" {
				delta = -1
			}
			index := (slices.Index(validModelProviders, f.provider) + delta + len(validModelProviders)) % len(validModelProviders)
			r.selectFormProvider(f, validModelProviders[index])
		} else if f.focus == 1 {
			handleModalInputKey(&f.model, e.ID)
		} else if f.focus == 2 {
			handleModalInputKey(&f.key, e.ID)
		} else if f.focus == 3 {
			f.syncContextLimit()
			handleModalInputKey(&f.contextLimit, e.ID)
		}
	case "<Enter>":
		if f.focus == 4 {
			r.applyModelForm(f)
		} else {
			r.focusModelForm(f, f.focus+1)
		}
	default:
		var ed *lineEditor
		if f.focus == 1 {
			ed = &f.model
		}
		if f.focus == 2 {
			ed = &f.key
		}
		if f.focus == 3 {
			f.syncContextLimit()
			ed = &f.contextLimit
		}
		if ed == nil {
			return true
		}
		before := ed.text()
		if !handleModalInputKey(ed, e.ID) {
			if e.ID == "<Backspace>" || e.ID == "<C-h>" {
				ed.backspace()
			} else if ch, ok := printableRune(e); ok {
				ed.insert(ch)
			}
		}
		if before != ed.text() || f.focus == 2 && (e.ID == "<C-u>" || !f.keyChanged && (e.ID == "<Backspace>" || e.ID == "<Delete>" || e.ID == "<C-h>")) {
			f.err = ""
			f.completing = false
			if f.focus == 3 {
				f.contextChanged = true
			} else if f.focus == 2 {
				f.keyChanged = true
				if f.cancel != nil {
					f.cancel()
				}
				f.revision++
				f.infos = map[string]llm.ModelInfo{}
				f.suggestions = nil
			} else {
				f.selected = -1
				f.updateSuggestions()
				r.fetchFormDetails(f, false)
			}
		}
	}
	return true
}
func (r *managedREPL) focusModelForm(f *modelForm, n int) {
	if f.focus == 2 && f.keyChanged && n != 2 {
		r.fetchFormCatalog(f, false)
	}
	if f.focus != n {
		f.completing = false
		f.selected = -1
	}
	f.focus = n
}
func (r *managedREPL) selectFormProvider(f *modelForm, provider string) {
	if provider != f.provider {
		f.wipe()
		f.keyChanged = false
		f.provider = provider
		f.model.clear()
		f.infos = map[string]llm.ModelInfo{}
		f.suggestions = nil
		f.selected = -1
		f.completing = false
		f.err = ""
		r.modelFormKeySource(f)
		r.fetchFormCatalog(f, false)
	}
}
func (f *modelForm) route() (string, string, error) {
	name := strings.TrimSpace(f.model.text())
	name = strings.TrimPrefix(name, f.provider+"/")
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		return "", "", fmt.Errorf("Enter a model name without spaces")
	}
	host := ""
	if f.provider == "openrouter" {
		// Exact catalog IDs (including provider-native variants) take precedence.
		if _, exact := f.infos[name]; !exact {
			if i := strings.LastIndex(name, ":"); i >= 0 {
				host = name[i+1:]
				name = name[:i]
				if host == "" || name == "" {
					return "", "", fmt.Errorf("Enter a host after the colon")
				}
			}
		}
	}
	if f.provider == "huggingface" && strings.HasSuffix(name, ":") {
		return "", "", fmt.Errorf("Enter a host after the colon")
	}
	return f.provider + "/" + name, host, nil
}
func (r *managedREPL) applyModelForm(f *modelForm) {
	model, host, err := f.route()
	if err != nil {
		f.err = err.Error()
		return
	}
	if f.keyChanged && (r.state == nil || r.state.agent == nil || !r.state.agent.HasProviderKeyOverrides()) {
		f.err = "Key overrides require a provider router"
		return
	}
	if err := r.applySelectedModelHost(model, host, f.modelContextWindow(), f.contextOverride()); err != nil {
		f.err = err.Error()
		return
	}
	if f.keyChanged {
		if key := strings.TrimSpace(f.key.text()); key != "" {
			r.state.agent.SetProviderAPIKey(f.provider, key)
		} else {
			r.state.agent.ClearProviderAPIKey(f.provider)
		}
	}
	r.closeModal()
	r.prefetchSelectedModel(model, host)
}

// Discovery may update an untouched field, but never overwrite a user's draft.
func (f *modelForm) syncContextLimit() {
	if f.contextChanged {
		return
	}
	value := fmt.Sprint(f.contextSettings.contextLimit(f.modelContextWindow()))
	if f.contextLimit.text() != value {
		f.contextLimit.setText(value)
	}
}

func (f *modelForm) contextOverride() *string {
	if !f.contextChanged {
		return nil
	}
	value := strings.TrimSpace(f.contextLimit.text())
	return &value
}
