package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/llm"
	tcell "github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// modelForm is the disposable /setup draft: Apply updates the session and saves
// launch defaults to ~/.pollytool/config (never the key or context).
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
	// keyOrigin is where the provider's key comes from when none is typed:
	// "environment", "session" (an earlier override), or "" for none.
	keyOrigin string
	// login marks a provider served on a sign-in: the key row shows the
	// account instead and takes no typing.
	login           bool
	signedIn        bool
	account, plan   string
	focus           int
	completing      bool
	completionQuery string
	hasKey          bool
	hasEnvKey       bool
	// selected is the Tab completion's place in suggestions (-1 before completing).
	selected    int
	suggestions []string
	// hits are the last paint's click targets, in form coordinates.
	hits        []formHit
	catalog     llm.ModelCatalog
	infos       map[string]llm.ModelInfo
	details     map[string]llm.ModelInfo
	requested   map[string]bool
	status, err string
	revision    uint64
	cancel      context.CancelFunc
	ctx         context.Context
	pasting     bool

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

const (
	formFieldProvider = iota
	formFieldModel
	formFieldKey
	formFieldContext
	formFieldEndpoint
	formFieldThinking
	formFieldTheme
	formFieldSandbox
)

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

// openSetupForm opens the setup form: the first run with nothing configured,
// --setup, /setup, or clicking the model in the status bar.
func (r *managedREPL) openSetupForm() { r.openSetupFormAt(formFieldProvider) }

func (r *managedREPL) openKeyManager() { r.openSetupFormAt(formFieldKey) }

func (r *managedREPL) openSetupFormAt(focus int) {
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
	f := &modelForm{
		provider:        provider,
		initialProvider: provider,
		initialModel:    name,
		focus:           max(formFieldProvider, min(formFieldSandbox, focus)),
		selected:        -1,
		infos:           map[string]llm.ModelInfo{},
		requested:       map[string]bool{},
	}
	if model != "" {
		f.initialRoute = &modelFormRoute{provider: provider, text: name, model: model, host: host}
	}
	if r.state != nil {
		f.contextSettings = r.state.settings.clone()
	} else if r.config != nil {
		f.contextSettings = r.config.Launch.clone()
	}
	f.model.setText(name)
	if r.config != nil {
		f.endpoint.setText(r.config.BaseURL)
	}
	f.initialEndpoint = f.endpoint.text()
	baseline := cmp.Or(f.contextSettings.ThinkingEffort, defaultThinkingEffort)
	f.thinking, f.initialThinking = baseline, baseline
	r.initSetupDefaults(f)
	f.modal = &replModal{
		title:     "Setup",
		modelForm: f,
		onCancel: func() {
			r.restoreFormTheme(f)
			r.skipSetup()
		},
	}
	r.modelFormKeySource(f)
	r.openModal(f.modal)
	r.fetchFormCatalog(f, false)
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
	f.keyOrigin, f.hasKey, f.hasEnvKey = "", false, false
	f.signedIn, f.account, f.plan = false, "", ""
	f.login = llm.ProviderRequiresLogin(f.provider + "/model")
	if r.state == nil || r.state.agent == nil {
		return
	}
	if f.login {
		if acct, ok := r.state.agent.ProviderAccount(f.provider); ok {
			f.signedIn, f.account, f.plan = true, cmp.Or(acct.Email, acct.ID), acct.Plan
		}
		return
	}
	f.hasEnvKey = r.state.agent.HasConfiguredProviderAPIKey(f.provider)
	switch source := r.state.agent.ProviderAPIKeySource(f.provider); source {
	case "session", "environment":
		f.keyOrigin, f.hasKey = source, true
	}
}

// changed reports whether Apply would change anything.
func (f *modelForm) changed() bool {
	return f.contextChanged || f.keyChanged ||
		f.provider != f.initialProvider ||
		strings.TrimSpace(f.model.text()) != f.initialModel ||
		strings.TrimSpace(f.endpoint.text()) != f.initialEndpoint ||
		f.thinking != f.initialThinking ||
		f.theme != f.initialTheme ||
		f.sandbox != f.initialSandbox
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
	if f.theme == f.initialTheme {
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

// textToolsRuledOut reports whether capabilities say a model cannot chat in
// text with tools. Unstated capabilities rule nothing out: many catalogs
// list names alone.
func textToolsRuledOut(c llm.ModelCapabilities) bool {
	if c.Tools != nil && !*c.Tools || c.Chat != nil && !*c.Chat {
		return true
	}
	return c.InputModalities != nil && !slices.Contains(c.InputModalities, "text") || c.OutputModalities != nil && !slices.Contains(c.OutputModalities, "text")
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
	// Models the catalog confirms for text and tools are listed, and so are
	// models it says nothing about; models it rules out are not.
	var out []string
	for _, info := range f.infos {
		base, effective := info.ModelCapabilities, info.EffectiveCapabilities("")
		if textTools(base) || textTools(effective) || !textToolsRuledOut(base) && !textToolsRuledOut(effective) {
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
	t.BaseURL = modelMetadataBaseURL(f.provider, strings.TrimSpace(f.endpoint.text()))
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
				if err != nil && !errors.Is(err, llm.ErrModelMetadataUnknown) {
					f.status = "catalog unavailable · " + err.Error()
				} else if cat.Error != "" {
					f.status = "catalog unavailable · " + cat.Error
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
				r.dismissModal()
			}
			return true
		}
		// The docked form has no border: its cells start at its bounds.
		local := p.Sub(f.modal.bounds.Min)
		if e.ID == "<MouseLeft>" {
			if hit, ok := f.hitAt(local); ok {
				r.clickModelForm(f, hit)
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
		r.dismissModal()
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
		r.focusModelForm(f, max(formFieldProvider, min(formFieldSandbox, f.focus+arrowDelta(e.ID))))
	case "<Left>", "<Right>":
		delta := arrowDelta(e.ID)
		if f.focus == formFieldProvider {
			index := (slices.Index(validModelProviders, f.provider) + delta + len(validModelProviders)) % len(validModelProviders)
			r.selectFormProvider(f, validModelProviders[index])
		} else if f.focus == formFieldModel {
			handleModalInputKey(&f.model, e.ID)
		} else if f.focus == formFieldKey && !f.login {
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
		// Enter applies from any field; Up and Down move between them.
		r.applyModelForm(f)
	default:
		var ed *lineEditor
		if f.focus == formFieldModel {
			ed = &f.model
		}
		if f.focus == formFieldKey && !f.login {
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

// clickModelForm acts on a click target from the last paint.
func (r *managedREPL) clickModelForm(f *modelForm, hit formHit) {
	switch hit.kind {
	case hitField:
		r.focusModelForm(f, hit.n)
	case hitCycle:
		key := "<Left>"
		if hit.n > 0 {
			key = "<Right>"
		}
		r.handleModelFormEvent(f, ui.Event{Type: ui.KeyboardEvent, ID: key})
	case hitApply:
		r.applyModelForm(f)
	}
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

func (f *modelForm) setupDefaults(model, host string) setupDefaults {
	defaults := setupDefaults{
		model:    model,
		host:     host,
		endpoint: strings.TrimSpace(f.endpoint.text()),
		thinking: f.thinking,
	}
	if f.sandbox {
		defaults.sandboxPreset = f.sandboxPreset
	}
	// An untouched theme writes nothing: the theme in effect is already
	// what the launch resolved.
	if f.theme != f.initialTheme {
		defaults.theme = f.theme
	}
	return defaults
}

func (r *managedREPL) applyModelForm(f *modelForm) {
	model, host, err := f.route()
	if err != nil {
		f.err = err.Error()
		r.focusModelForm(f, formFieldModel)
		return
	}
	if f.keyChanged && (r.state == nil || r.state.agent == nil || !r.state.agent.HasProviderKeyOverrides()) {
		f.err = "Key overrides require a provider router"
		return
	}
	if r.state != nil && r.state.agent != nil && f.keyMissing(model) {
		f.err = "Enter a key for " + f.provider
		r.focusModelForm(f, formFieldKey)
		return
	}
	if f.login && r.state != nil && r.state.agent != nil && r.state.agent.LoginRequired(model) {
		f.err = "Sign in first: /login " + f.provider
		r.focusModelForm(f, formFieldKey)
		return
	}
	defaults := f.setupDefaults(model, host)
	if err := defaults.check(); err != nil {
		f.err = "Saving defaults failed · " + err.Error()
		return
	}
	if err := r.applySetupSessionSettings(model, host, f.thinking, f.modelContextWindow(), f.contextOverride()); err != nil {
		f.err = err.Error()
		return
	}
	notices, err := defaults.write()
	if err != nil {
		f.err = "Saving defaults failed · " + err.Error()
		return
	}
	if f.keyChanged {
		if key := strings.TrimSpace(f.key.text()); key != "" {
			r.state.agent.SetProviderAPIKey(f.provider, key)
		} else {
			r.state.agent.ClearProviderAPIKey(f.provider)
		}
	}
	if r.config != nil {
		r.config.BaseURL = defaults.endpoint
		r.config.Launch.Model, r.config.Launch.ModelHost, r.config.Launch.ThinkingEffort = model, host, f.thinking
	}
	if r.state != nil {
		r.state.metadataBaseURL = defaults.endpoint
	}
	for _, notice := range notices {
		r.model.appendNoticeLine(notice)
	}
	if f.sandbox != f.initialSandbox {
		// The tools this launch runs were wired for its own posture, so the
		// default can only take hold on the next launch.
		line := "sandbox default saved · later launches sandbox tool calls · this launch's sandbox is unchanged"
		if !f.sandbox {
			line = "sandbox default saved · later launches run without the sandbox · this launch's sandbox is unchanged"
		}
		r.model.appendNoticeLine(line)
	}
	r.saveSetupTheme(f)
	r.closeModal()
	r.prefetchSelectedModel(model, host)
}

// saveSetupTheme makes a theme the form chose the session's, the way /theme
// does; applyModelForm has already written it as the launch default. A name
// that no longer loads is reported in the transcript by applyThemeByName and
// the previous theme stays on screen.
func (r *managedREPL) saveSetupTheme(f *modelForm) {
	if f.theme == f.initialTheme {
		return
	}
	if _, err := r.applyThemeByName(f.theme); err != nil {
		// Put the theme the session follows back, as the picker does,
		// instead of leaving the last preview on screen.
		r.restoreFormTheme(f)
		return
	}
	r.model.appendNoticeLine(r.activeThemeLine() + " · applied to UI now · saved as default for later launches")
}

// keyMissing reports whether applying the draft would leave model without
// the credential its provider needs: no configured key and no draft key.
func (f *modelForm) keyMissing(model string) bool {
	if f.keyChanged {
		if strings.TrimSpace(f.key.text()) != "" || f.hasEnvKey {
			return false
		}
	} else if f.hasKey {
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
	if notice := recordSetupSkip(); notice != "" {
		r.model.appendNoticeLine(notice)
	}
	r.noticeMissingKey()
}

// noticeMissingKey posts the startup key refusal as a notice when the
// session's provider needs a credential and none is configured.
func (r *managedREPL) noticeMissingKey() {
	if r.state == nil || r.state.agent == nil {
		return
	}
	model := r.state.settings.Model
	if envVar, missing := r.state.agent.MissingAPIKey(model, r.config.BaseURL); missing {
		r.model.appendNoticeLine(missingKeyNotice(model, envVar))
	}
	if r.state.agent.LoginRequired(model) {
		r.model.appendNoticeLine(missingLoginNotice(model))
	}
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
