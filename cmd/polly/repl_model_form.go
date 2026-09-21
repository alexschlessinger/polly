package main

import (
	"cmp"
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
	initialRoute    *modelFormRoute
	selectedRoute   *modelFormRoute
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
	fieldRows       [9]int
	pasting         bool

	// setup marks the setup form: the endpoint, thinking, theme, and sandbox
	// fields join the four above, and Apply also saves the draft to
	// ~/.pollytool/config as the process defaults.
	setup           bool
	endpoint        lineEditor
	endpointChanged bool
	initialEndpoint string
	thinking        string
	initialThinking string

	// theme is the theme the setup form offers, by the name /theme takes.
	// Cycling the field previews a theme on the whole screen; the theme in
	// effect comes back on Escape, and only Apply saves the choice.
	// initialTheme is the name the launch resolved, and themeNames the
	// names to cycle through.
	theme        string
	initialTheme string
	themeNames   []string

	// sandbox is whether later launches sandbox tool calls; the field cycles
	// between none and sandboxPreset, the policy the on state saves.
	// Sandboxing is a launch-time wiring (the tools are built with it), so
	// the choice is a default for the next launch, never a change to this
	// one. initialSandbox is what this launch got.
	sandbox        bool
	initialSandbox bool
	sandboxPreset  string
}

// Field order; the Apply button follows the last field the form shows.
const (
	formFieldProvider = iota
	formFieldModel
	formFieldKey
	formFieldContext
	formFieldEndpoint // setup only
	formFieldThinking // setup only
	formFieldTheme    // setup only
	formFieldSandbox  // setup only
)

func (f *modelForm) applyIndex() int {
	if f.setup {
		return formFieldSandbox + 1
	}
	return formFieldContext + 1
}

// Keep a selected destination independent of asynchronously replaced catalogs.
// The text includes the optional :host suffix shown in the combined model field.
type modelFormRoute struct {
	provider, text, model, host string
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
	model, host := name, ""
	if provider == "openrouter" && r.state != nil {
		host = r.state.settings.ModelHost
		if host != "" {
			name += ":" + host
		}
	}
	f := &modelForm{provider: provider, initialProvider: provider, initialModel: name, focus: focus, selected: -1, infos: map[string]llm.ModelInfo{}, requested: map[string]bool{}}
	if model != "" {
		f.initialRoute = &modelFormRoute{provider: provider, text: name, model: model, host: host}
	}
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

// openSetupForm opens the model form in setup mode: the first run with
// nothing configured, --setup, or /setup.
func (r *managedREPL) openSetupForm() {
	r.openModelForm(formFieldProvider)
	f := r.model.modal.modelForm
	f.setup = true
	f.modal.title = "Setup"
	if r.config != nil {
		f.endpoint.setText(r.config.BaseURL)
	}
	f.initialEndpoint = f.endpoint.text()
	baseline := cmp.Or(f.contextSettings.ThinkingEffort, defaultThinkingEffort)
	f.thinking, f.initialThinking = baseline, baseline
	r.initSetupDefaults(f)
}

// initSetupDefaults loads the two defaults that are not model settings: the
// theme the launch is running, and whether tool calls are sandboxed.
func (r *managedREPL) initSetupDefaults(f *modelForm) {
	f.theme = r.activeThemeName()
	f.themeNames = allThemeNames()
	// A --theme value that is not a name (a path, or a name no longer
	// present) still leads the cycle, so an untouched field leaves it alone.
	if !slices.Contains(f.themeNames, f.theme) {
		f.themeNames = append([]string{f.theme}, f.themeNames...)
	}
	f.initialTheme = f.theme
	// Sandboxing is opt-in, so the field is on when a policy asks for one.
	// The saved preset leads it, both as the value the on state keeps and so
	// a custom policy survives an untouched field; this launch's own preset,
	// then the standard one, stand in. Reading the file matters because
	// saving the default never changes this launch.
	f.sandbox = r.config == nil || !r.config.NoSandbox
	f.sandboxPreset = defaultSandboxPreset
	if r.config != nil && r.config.SandboxPreset != "" {
		f.sandboxPreset = r.config.SandboxPreset
	}
	if saved, ok := userConfigValue(envVarSandbox); ok {
		if preset := strings.TrimSpace(saved); preset != "" {
			f.sandbox, f.sandboxPreset = true, preset
		} else {
			f.sandbox = false
		}
	}
	f.initialSandbox = f.sandbox
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
	field(formFieldProvider, "Provider", "‹ "+f.provider+" ›")
	f.providerBounds[0] = image.Rect(11, f.fieldRows[formFieldProvider], min(inner, 13), f.fieldRows[formFieldProvider]+1)
	right := 13 + rw.StringWidth(f.provider)
	f.providerBounds[1] = image.Rect(right, f.fieldRows[formFieldProvider], min(inner, right+2), f.fieldRows[formFieldProvider]+1)
	value := f.model.text()
	if f.focus == formFieldModel {
		value = formEditorText(&f.model, false, max(1, inner-12))
	}
	field(formFieldModel, "Model", value)
	if f.focus == formFieldModel && f.model.cursor == len(f.model.buf) {
		available := max(0, inner-11-rw.StringWidth(value))
		if available > 0 {
			shadow := rw.Truncate(metadataDisplayText(f.completionShadow(), false), available, "…")
			rows[f.fieldRows[formFieldModel]] += style.Styled(shadow, "muted", "dim")
		}
	}

	key := ""
	if f.hasKey {
		key = "********"
	}
	if f.keyChanged {
		key = strings.Repeat("*", len(f.key.buf))
	}
	if f.focus == formFieldKey {
		if !f.keyChanged && f.hasKey {
			key += "│"
		} else {
			key = formEditorText(&f.key, true, max(1, inner-12))
		}
	}
	field(formFieldKey, "Key", key)
	f.syncContextLimit()
	contextSize := f.contextLimit.text()
	if f.focus == formFieldContext {
		contextSize = formEditorText(&f.contextLimit, false, max(1, inner-12))
	}
	field(formFieldContext, "Context", contextSize)
	if f.setup {
		endpoint := f.endpoint.text()
		if f.focus == formFieldEndpoint {
			endpoint = formEditorText(&f.endpoint, false, max(1, inner-12))
		} else if endpoint == "" {
			endpoint = "default"
		}
		field(formFieldEndpoint, "Endpoint", endpoint)
		field(formFieldThinking, "Effort", "‹ "+f.thinking+" ›")
		field(formFieldTheme, "Theme", "‹ "+f.theme+" ›")
		// The values name the --sandbox vocabulary: the preset that applies,
		// or none at all.
		sandbox := f.sandboxPreset
		if !f.sandbox {
			sandbox = "none"
		}
		field(formFieldSandbox, "Sandbox", "‹ "+sandbox+" ›")
	}
	status, role := f.keySource, "muted"
	if status == "No key configured" || status == "Using environment key" {
		status = ""
	}
	if f.status != "" {
		status = f.status
	}

	if f.err != "" {
		status, role = f.err, "err"
	}
	f.modal.titleNotice = ""
	if status != "" {
		f.modal.titleNotice = "(" + rw.Truncate(metadataDisplayText(status, false), max(1, width-12), "…") + ")"
	}
	f.modal.titleNoticeRole = role
	rows = append(rows, "")
	apply := f.applyIndex()
	f.fieldRows[apply] = len(rows)
	button := "[ Apply ]"
	if f.focus == apply {
		button = "› " + button
	}
	x := max(0, inner-rw.StringWidth(button))
	f.applyBounds = image.Rect(x, len(rows), inner, len(rows)+1)
	buttonColor, modifier := "", ""
	if f.focus == apply {
		buttonColor, modifier = "accent", "bold"
	}
	if f.contextChanged || f.keyChanged || f.provider != f.initialProvider || strings.TrimSpace(f.model.text()) != f.initialModel || f.setupChanged() {
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

// setupChanged reports whether a setup-only field differs from its opening value.
func (f *modelForm) setupChanged() bool {
	return f.setup && (strings.TrimSpace(f.endpoint.text()) != f.initialEndpoint || f.thinking != f.initialThinking || f.theme != f.initialTheme || f.sandbox != f.initialSandbox)
}

// arrowDelta is the step an arrow key takes through an ordered list.
func arrowDelta(id string) int {
	if id == "<Left>" || id == "<Up>" {
		return -1
	}
	return 1
}

// cycleAmong steps current through words, landing on the first word when it
// is not one of them (a saved value the list no longer offers).
func cycleAmong(words []string, current string, delta int) string {
	if len(words) == 0 {
		return current
	}
	i := slices.Index(words, current)
	if i < 0 {
		i, delta = 0, 0
	}
	return words[(i+delta+len(words))%len(words)]
}

// cycleFormTheme steps the theme field through the known theme names, previewing
// each one on screen like the theme picker's selection. A preview applies no
// setting: Escape puts the theme in effect back and Apply is what saves the
// choice as the launch default.
func (r *managedREPL) cycleFormTheme(f *modelForm, delta int) {
	if len(f.themeNames) == 0 {
		return
	}
	f.theme = cycleAmong(f.themeNames, f.theme, delta)
	f.err = ""
	// A name that no longer resolves previews as nothing, so the field says
	// why instead of naming a theme the screen is not showing.
	if selection, err := resolveThemeSelection(f.theme); err == nil {
		r.applyTheme(selection.theme)
	} else {
		f.err = err.Error()
	}
}

// restoreFormTheme puts the theme the session follows back after a preview.
// The selection the startup apply recorded is the one to restore, because the
// preview never touches it.
func (r *managedREPL) restoreFormTheme(f *modelForm) {
	if !f.setup || f.theme == f.initialTheme {
		return
	}
	f.theme = f.initialTheme
	r.restoreActiveTheme()
}

// thinkingCapabilities returns what the form already knows about the drafted
// model and route, without fetching anything: the zero value when the catalog
// has not named it, which offers the provider's whole vocabulary.
func (f *modelForm) thinkingCapabilities() llm.ModelCapabilities {
	info, host, ok := f.selectedModelInfo()
	if !ok {
		return llm.ModelCapabilities{}
	}
	return info.EffectiveCapabilities(host)
}

// cycleThinking steps the thinking field through the effort words the drafted
// model accepts, so the form cannot save an effort its provider would reject:
// OpenRouter has no "max". A value outside that vocabulary is not in the
// cycle, so the first press lands on its first word.
func (f *modelForm) cycleThinking(delta int) {
	words := llm.ThinkingEffortWordsFor(f.provider+"/"+strings.TrimSpace(f.model.text()), f.thinkingCapabilities())
	f.thinking = cycleAmong(words, f.thinking, delta)
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
	_, _, _ = f.route()
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
	if f.setup {
		t.BaseURL = modelMetadataBaseURL(f.provider, strings.TrimSpace(f.endpoint.text()))
	}
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
			// Like Escape, a click outside the painted form closes it.
			if e.ID == "<MouseLeft>" && !f.modal.bounds.Empty() {
				r.restoreFormTheme(f)
				r.closeModal()
			}
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
				r.focusModelForm(f, f.applyIndex())
				r.applyModelForm(f)
				return true
			}
			for n, y := range f.fieldRows[:f.applyIndex()] {
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
		if f.setup {
			r.restoreFormTheme(f)
			r.skipSetup()
		}
		r.closeModal()
	case "<Tab>", "<Backtab>", "<S-Tab>":
		if f.focus == formFieldModel {
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
				_, _, _ = f.route()
			}
			r.fetchFormDetails(f, false)
		}
	case "<C-r>":
		r.fetchFormCatalog(f, true)
	case "<Up>", "<Down>":
		r.focusModelForm(f, max(0, min(f.applyIndex(), f.focus+arrowDelta(e.ID))))
	case "<Left>", "<Right>":
		delta := arrowDelta(e.ID)
		if f.focus == formFieldProvider {
			index := (slices.Index(validModelProviders, f.provider) + delta + len(validModelProviders)) % len(validModelProviders)
			r.selectFormProvider(f, validModelProviders[index])
		} else if f.focus == formFieldModel {
			handleModalInputKey(&f.model, e.ID)
		} else if f.focus == formFieldKey {
			handleModalInputKey(&f.key, e.ID)
		} else if f.focus == formFieldContext {
			f.syncContextLimit()
			handleModalInputKey(&f.contextLimit, e.ID)
		} else if f.focus == formFieldEndpoint {
			handleModalInputKey(&f.endpoint, e.ID)
		} else if f.focus == formFieldThinking {
			f.cycleThinking(delta)
			f.err = ""
		} else if f.focus == formFieldTheme {
			r.cycleFormTheme(f, delta)
		} else if f.focus == formFieldSandbox {
			// Either arrow flips it: the field has two values.
			f.sandbox = !f.sandbox
			f.err = ""
		}
	case "<Enter>":
		if f.focus == f.applyIndex() {
			r.applyModelForm(f)
		} else {
			r.focusModelForm(f, f.focus+1)
		}
	default:
		var ed *lineEditor
		if f.focus == formFieldModel {
			ed = &f.model
		}
		if f.focus == formFieldKey {
			ed = &f.key
		}
		if f.focus == formFieldContext {
			f.syncContextLimit()
			ed = &f.contextLimit
		}
		if f.focus == formFieldEndpoint {
			ed = &f.endpoint
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
		if before != ed.text() || f.focus == formFieldKey && (e.ID == "<C-u>" || !f.keyChanged && (e.ID == "<Backspace>" || e.ID == "<Delete>" || e.ID == "<C-h>")) {
			f.err = ""
			f.completing = false
			if f.focus == formFieldContext {
				f.contextChanged = true
			} else if f.focus == formFieldEndpoint {
				f.endpointChanged = true
			} else if f.focus == formFieldKey {
				f.keyChanged = true
				_, _, _ = f.route()
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
	if f.focus == formFieldKey && f.keyChanged && n != formFieldKey {
		r.fetchFormCatalog(f, false)
	}
	// Suggestions come from the endpoint being configured, so a changed
	// endpoint refreshes them once the field is left.
	if f.focus == formFieldEndpoint && f.endpointChanged && n != formFieldEndpoint {
		f.endpointChanged = false
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
		for _, route := range []*modelFormRoute{f.initialRoute, f.selectedRoute} {
			if route != nil && route.provider == f.provider && route.text == name {
				return f.provider + "/" + route.model, route.host, nil
			}
		}
		text := name
		_, known := f.infos[name]
		// Exact IDs take precedence over discovered host suffixes. An unknown
		// colon suffix remains part of the literal model ID.
		if !known {
			if i := strings.LastIndex(name, ":"); i > 0 {
				if info, ok := f.infos[name[:i]]; ok {
					for _, endpoint := range info.Endpoints {
						if endpoint.ID != "" && endpoint.ID == name[i+1:] {
							name, host, known = name[:i], endpoint.ID, true
							break
						}
					}
				}
			}
		}
		if known {
			f.selectedRoute = &modelFormRoute{provider: f.provider, text: text, model: name, host: host}
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
	if f.setup && f.keyMissing(model) {
		f.err = "Enter a key for " + f.provider
		r.focusModelForm(f, formFieldKey)
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
	if f.setup {
		if err := r.saveSetup(f, model, host); err != nil {
			f.err = "Saving defaults failed · " + err.Error()
			return
		}
		r.saveSetupTheme(f)
	}
	r.closeModal()
	r.prefetchSelectedModel(model, host)
}

// saveSetup makes the setup draft the process defaults: the thinking effort
// lands on the session like /set effort, the endpoint replaces the
// process base URL, launch settings for later sessions follow, and the
// configuration file records them for the next launch. The model and key
// were applied by applyModelForm already; the key is never written.
func (r *managedREPL) saveSetup(f *modelForm, model, host string) error {
	// Polly refuses to start with a sandbox policy and no sandbox, so the
	// pair is refused here rather than at the next launch, which could not be
	// talked out of it — and before anything is applied or written. Only the
	// environment can hold the other half: a saved line this writes over.
	if f.sandbox && noSandboxExported() {
		return fmt.Errorf("%s is set in your environment, and a launch refuses to start with a sandbox policy and no sandbox; unset it or choose none", envVarNoSandbox)
	}
	endpoint := strings.TrimSpace(f.endpoint.text())
	thinking := f.thinking
	ctx := newManagedReplCommandContext(r)
	// The model may have changed under an untouched field, so what Apply
	// saves is checked against the model it is saved for, not only what the
	// field last cycled through.
	if err := validateThinkingEffort(ctx, thinking); err != nil {
		return err
	}
	if ctx.settings != nil && ctx.settings.ThinkingEffort != thinking {
		if _, err := applyAndPersistSetting(ctx, "effort", thinking); err != nil {
			return err
		}
	}
	if r.config != nil {
		r.config.BaseURL = endpoint
		r.config.Launch.Model, r.config.Launch.ModelHost, r.config.Launch.ThinkingEffort = model, host, thinking
	}
	if r.state != nil {
		r.state.metadataBaseURL = endpoint
	}
	// Empty values drop the line; the built-in defaults need none. The effort
	// is always written: every one of its words is a choice, and off is not
	// what polly does without being told. The sandbox is the reverse — none
	// is what polly does untold, so only a policy is written, and the former
	// spelling of its off state goes with it.
	sandboxPreset := ""
	if f.sandbox {
		sandboxPreset = f.sandboxPreset
	}
	updates := map[string]string{
		envVarModel:     model,
		envVarModelHost: host,
		envVarBaseURL:   endpoint,
		envVarEffort:    thinking,
		envVarSandbox:   sandboxPreset,
		envVarNoSandbox: "",
		// The former spelling of the effort line would outlive the line that
		// replaces it, so saving removes it.
		envVarThinking: "",
	}
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	if err := writeUserConfig(path, updates); err != nil {
		return err
	}
	r.model.appendNoticeLine("defaults saved to " + userConfigDisplayPath)
	// The saved effort line outranks an exported former spelling, so that
	// variable shadows nothing and stays out of the report.
	delete(updates, envVarThinking)
	if shadowed := shadowedByEnvironment(updates); len(shadowed) > 0 {
		r.model.appendNoticeLine("set in your environment and overriding the file on the next launch: " + strings.Join(shadowed, ", "))
	}
	if f.keyChanged && strings.TrimSpace(f.key.text()) != "" {
		// The key is a process override like /keys; the next launch needs
		// it in the environment.
		r.model.appendNoticeLine("key kept for this process only · export " + llm.ProviderKeyEnvVar(f.provider) + " for the next launch")
	}
	if f.sandbox != f.initialSandbox {
		// The tools this launch runs were wired for its own posture, so the
		// default can only take hold on the next launch.
		line := "default saved · later launches sandbox tool calls · this one is already running without the sandbox"
		if !f.sandbox {
			line = "default saved · later launches run without the sandbox · this one keeps the sandbox it started with"
		}
		r.model.appendNoticeLine(line)
	}
	return nil
}

// saveSetupTheme keeps a theme the form chose as the launch default, the way
// /theme and set_theme's persist do. An untouched field writes nothing: the
// theme in effect is already what the launch resolved. A name that no longer
// loads is reported in the transcript by applyThemeByName and the previous
// theme stays on screen.
func (r *managedREPL) saveSetupTheme(f *modelForm) {
	if f.theme == f.initialTheme {
		return
	}
	lines := r.switchTheme(f.theme)
	if lines == nil {
		// The name no longer loads, and applyThemeByName has said so. Put
		// the theme the session follows back, as the picker does, instead of
		// leaving the last preview on screen.
		r.restoreFormTheme(f)
		return
	}
	for _, line := range lines {
		r.model.appendNoticeLine(line)
	}
}

// keyMissing reports whether applying the draft would leave model without
// the credential its provider needs: no configured key and no draft key.
func (f *modelForm) keyMissing(model string) bool {
	if f.keyChanged && strings.TrimSpace(f.key.text()) != "" {
		return false
	}
	if f.hasKey && !f.keyChanged {
		return false
	}
	return llm.ProviderRequiresKey(model, strings.TrimSpace(f.endpoint.text()))
}

// skipSetup records a dismissed setup form as an empty configuration when
// none exists yet, so the next launch starts straight into the conversation.
// The startup key gate stood aside for the form, so a session left on a
// provider without a key is told here what it would otherwise have been
// told at launch.
func (r *managedREPL) skipSetup() {
	defer r.noticeMissingKey()
	if userConfigExists() {
		return
	}
	path, err := userConfigPath()
	if err == nil {
		err = writeUserConfig(path, nil)
	}
	if err != nil {
		r.model.appendNoticeLine("Setup skipped · could not record it · " + err.Error())
		return
	}
	r.model.appendNoticeLine("Setup skipped · /setup or polly --setup reopens it")
}

// noticeMissingKey posts the startup key refusal as a notice when the
// session's provider needs a credential and none is configured.
func (r *managedREPL) noticeMissingKey() {
	if r.state == nil || r.state.agent == nil {
		return
	}
	model := r.state.settings.Model
	provider, _, _ := strings.Cut(model, "/")
	if r.state.agent.ProviderAPIKeySource(provider) != "" || !llm.ProviderRequiresKey(model, r.config.BaseURL) {
		return
	}
	r.model.appendNoticeLine(missingKeyNotice(model))
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
