package main

import (
	"context"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func formKey(r *managedREPL, key string) {
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: key})
}
func newFormREPL(t *testing.T) (*managedREPL, *llm.MultiPass) {
	t.Helper()
	mp := llm.NewMultiPass(map[string]string{"openai": "environment-key"})
	a := llm.NewAgent(mp, nil, llm.AgentConfig{})
	t.Cleanup(func() { a.Close() })
	r := newManagedREPL(&Config{}, "form", 0, 0)
	r.work.close()
	r.work = nil
	r.state = &conversationState{agent: a, settings: Settings{Model: "openai/current"}}
	r.openModelPicker()
	return r, mp
}
func TestModelFormConfirmedTextToolsSuggestions(t *testing.T) {
	yes, no := true, false
	caps := llm.ModelCapabilities{Chat: &yes, Tools: &yes, InputModalities: []string{"text"}, OutputModalities: []string{"text"}}
	noTools := caps
	noTools.Tools = &no
	nonText := caps
	nonText.OutputModalities = []string{"image"}
	f := &modelForm{provider: "openrouter", selected: -1, infos: map[string]llm.ModelInfo{
		"good":      {ID: "good", ModelCapabilities: caps},
		"unknown":   {ID: "unknown"},
		"text-only": {ID: "text-only", ModelCapabilities: noTools},
		"image":     {ID: "image", ModelCapabilities: nonText},
		"route": {ID: "route", Routed: true, ModelCapabilities: noTools, Endpoints: []llm.ModelEndpointInfo{
			{ID: "works", ModelCapabilities: caps}, {ID: "no-tools", ModelCapabilities: noTools}, {ID: "offline", Status: "offline", ModelCapabilities: caps},
		}},
	}}
	f.updateSuggestions()
	if !slices.Equal(f.suggestions, []string{"good", "route:works"}) {
		t.Fatalf("suggestions: %v", f.suggestions)
	}
	f.model.setText("route:w")
	f.updateSuggestions()
	if !slices.Equal(f.suggestions, []string{"route:works"}) {
		t.Fatalf("hosts: %v", f.suggestions)
	}
	f.selected = 0
	f.updateSuggestions()
	if f.selected != 0 {
		t.Fatal("refresh lost selection")
	}
}
func TestModelFormApplyCancelKeysAndPersistence(t *testing.T) {
	r, mp := newFormREPL(t)
	store := testOpenMemoryStore(t, nil)
	r.state.session = testAcquireSession(t, store, "form")
	f := r.model.modal.modelForm
	f.provider = "openrouter"
	f.model.setText("org/manual:host")
	f.infos["org/manual"] = llm.ModelInfo{ID: "org/manual", Endpoints: []llm.ModelEndpointInfo{{ID: "host"}}}
	f.key.setText("draft-secret")
	f.keyChanged = true
	if mp.APIKeySource("openrouter") != "" || r.state.settings.Model != "openai/current" {
		t.Fatal("draft applied early")
	}
	formKey(r, "<Escape>")
	if f.key.text() != "" || mp.APIKeySource("openrouter") != "" {
		t.Fatal("cancel retained/installed secret")
	}
	r.openModelPicker()
	f = r.model.modal.modelForm
	f.provider = "openrouter"
	f.model.setText("org/manual:host")
	f.infos["org/manual"] = llm.ModelInfo{ID: "org/manual", Endpoints: []llm.ModelEndpointInfo{{ID: "host"}}}
	f.key.setText("draft-secret")
	f.keyChanged = true
	f.focus = 4
	formKey(r, "<Enter>")
	if r.model.modal != nil || mp.APIKeySource("openrouter") != "session" {
		t.Fatal("Apply failed")
	}
	md, err := r.state.session.GetMetadata(context.Background())
	if err != nil || md.Model != "openrouter/org/manual" || md.ModelHost != "host" {
		t.Fatalf("route: %+v %v", md, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", md), "draft-secret") || strings.Contains(r.model.ed.text(), "draft-secret") {
		t.Fatal("key escaped form")
	}
	r.openModelPicker()
	f = r.model.modal.modelForm
	if f.model.text() != "org/manual:host" {
		t.Fatal("pin not restored into name")
	}
	f.model.setText("org/next")
	f.focus = 4
	formKey(r, "<Enter>")
	if r.state.settings.ModelHost != "" {
		t.Fatal("old host retained")
	}
	r.openKeyManager()
	f = r.model.modal.modelForm
	formKey(r, "<C-u>")
	formKey(r, "<Down>")
	formKey(r, "<Enter>")
	formKey(r, "<Enter>")
	if mp.APIKeySource("openrouter") != "" {
		t.Fatal("clear did not remove override")
	}
	mp.SetAPIKey("openai", "old-override")
	r.state.settings.Model = "openai/current"
	r.openKeyManager()
	formKey(r, "<C-u>")
	formKey(r, "<Enter>")
	formKey(r, "<Enter>")
	formKey(r, "<Enter>")
	if mp.APIKeySource("openai") != "environment" {
		t.Fatal("clear did not restore environment")
	}
}
func TestModelFormKeyboardProviderAutocompleteAndPaste(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	formKey(r, "<Up>")
	for range len(validModelProviders) {
		if f.provider == "huggingface" {
			break
		}
		formKey(r, "<Right>")
	}
	if f.provider != "huggingface" || f.focus != 0 || r.state.settings.Model != "openai/current" {
		t.Fatal("provider selection applied early")
	}
	formKey(r, "<Down>")
	yes := true
	caps := llm.ModelCapabilities{Chat: &yes, Tools: &yes}
	f.infos = map[string]llm.ModelInfo{"org/model": {ID: "org/model", Routed: true, ModelCapabilities: caps, Endpoints: []llm.ModelEndpointInfo{{ID: "host", ModelCapabilities: caps}}}}
	f.model.setText("org/")
	f.updateSuggestions()
	if f.model.text() != "org/" {
		t.Fatal("completed without Tab")
	}
	formKey(r, "<Tab>")
	formKey(r, "<Tab>")
	if f.model.text() != "org/model:host" || f.focus != 1 {
		t.Fatal("Tab did not cycle host completion")
	}
	formKey(r, "<Down>")
	formKey(r, pasteStartID)
	for _, ch := range "secret" {
		formKey(r, string(ch))
	}
	formKey(r, "<Enter>")
	formKey(r, "<Tab>")
	formKey(r, pasteEndID)
	if f.focus != 2 || f.key.text() != "secret" {
		t.Fatal("paste triggered form actions")
	}
	formKey(r, "<Tab>")
	if f.focus != 2 {
		t.Fatal("Tab moved key focus")
	}
	formKey(r, "<Down>")
	formKey(r, "<Enter>")
	formKey(r, "<Enter>")
	if r.state.settings.Model != "huggingface/org/model:host" || r.state.settings.ModelHost != "" {
		t.Fatal("HF route lost")
	}
}
func TestModelFormTabCyclesOriginalQuery(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	yes := true
	f.infos = map[string]llm.ModelInfo{}
	for _, id := range []string{"alpha-one", "alpha-two", "beta"} {
		f.infos[id] = llm.ModelInfo{ID: id, ModelCapabilities: llm.ModelCapabilities{Chat: &yes, Tools: &yes}}
	}
	f.model.setText("alpha")
	f.updateSuggestions()
	text := plainStyledText(f.text(20, 70))
	if !strings.Contains(text, "alpha│-one") || f.model.text() != "alpha" {
		t.Fatal("completion shadow missing or changed the draft")
	}
	formKey(r, "<Left>")
	if strings.Contains(plainStyledText(f.text(20, 70)), "-one") {
		t.Fatal("shadow shown while editing inside the name")
	}
	formKey(r, "<End>")
	for _, want := range []string{"alpha-one", "alpha-two", "alpha-one"} {
		formKey(r, "<Tab>")
		if f.model.text() != want || f.focus != 1 {
			t.Fatalf("got %s, want %s", f.model.text(), want)
		}
	}
	if f.completionShadow() != " → alpha-two" {
		t.Fatal("shadow did not follow the next Tab result")
	}
	key := tcell.NewEventKey(tcell.KeyBacktab, "", tcell.ModNone)
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: fmt.Sprintf("<Key:%v>", key.Key()), Payload: key})
	if f.model.text() != "alpha-two" || f.focus != 1 {
		t.Fatal("Shift-Tab did not reverse completion")
	}
	formKey(r, "<C-u>")
	formKey(r, "b")
	formKey(r, "<Tab>")
	if f.model.text() != "beta" {
		t.Fatal("editing did not reset completion query")
	}
}

func TestModelFormInvalidApplyKeepsDraftAndKey(t *testing.T) {
	r, mp := newFormREPL(t)
	f := r.model.modal.modelForm
	f.model.setText("")
	f.key.setText("secret")
	f.keyChanged = true
	f.focus = 4
	formKey(r, "<Enter>")
	if r.model.modal == nil || f.err == "" || mp.APIKeySource("openai") != "environment" {
		t.Fatal("invalid Apply mutated settings/key")
	}
}
func TestModelFormNarrowWideMasksKey(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	f.key.setText("never-show-this-secret")
	f.keyChanged = true
	f.focus = 2
	for _, width := range []int{40, 60, 100} {
		text := plainStyledText(f.modal.text(24, width))
		if strings.Contains(text, "never-show") || !strings.Contains(text, "Apply") || !strings.Contains(text, "***") || strings.Contains(text, "•") {
			t.Fatalf("render: %s", text)
		}
		for _, line := range strings.Split(text, "\n") {
			if len([]rune(line)) > width-2 {
				t.Fatalf("overflow width %d: %s", width, line)
			}
		}
	}
}
func TestModelFormDisregardsLateDiscovery(t *testing.T) {
	for _, change := range []string{"key", "navigation", "revision", "draft"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				close(entered)
				<-release
				fmt.Fprint(w, `{"data":[{"id":"new-model"}]}`)
			}))
			defer server.Close()
			r, mp := newFormREPL(t)
			r.closeModal()
			r.config.BaseURL = server.URL
			r.work = newREPLWork()
			defer r.work.close()
			r.uiTasks = make(chan func(), 4)
			r.openModelPicker()
			f := r.model.modal.modelForm
			<-entered
			switch change {
			case "key":
				mp.SetAPIKey("openai", "new-key")
			case "navigation":
				r.openModal(&replModal{title: "elsewhere"})
			case "revision":
				f.revision++
			case "draft":
				f.focus = 2
				formKey(r, "x")
			}
			close(release)
			select {
			case apply := <-r.uiTasks:
				apply()
			case <-time.After(100 * time.Millisecond):
			}
			if len(f.infos) != 0 {
				t.Fatal("stale discovery applied")
			}
		})
	}
}
func TestModelFormDiscoveryUsesDraftKeyWithoutInstalling(t *testing.T) {
	auth := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth <- req.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()
	r, mp := newFormREPL(t)
	r.config.BaseURL = server.URL
	r.work = newREPLWork()
	defer r.work.close()
	r.uiTasks = make(chan func(), 8)
	f := r.model.modal.modelForm
	f.key.setText("preview")
	f.keyChanged = true
	r.fetchFormCatalog(f, false)
	select {
	case got := <-auth:
		if got != "Bearer preview" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("no preview")
	}
	if mp.APIKeySource("openai") != "environment" {
		t.Fatal("preview installed key")
	}
	mp.SetAPIKey("openai", "runtime")
	f.key.clear()
	r.fetchFormCatalog(f, false)
	select {
	case got := <-auth:
		if got != "Bearer environment-key" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("no clear preview")
	}
}
func TestModelFormRoutesKeepNativeModelIDs(t *testing.T) {
	for _, tc := range []struct{ provider, name, want, host string }{
		{"openrouter", "org/model:free", "openrouter/org/model:free", ""},
		{"openrouter", "org/model:free:host", "openrouter/org/model:free", "host"},
		{"openrouter", "org/model:free:unknown", "openrouter/org/model:free:unknown", ""},
		{"openrouter", "org/manual:variant", "openrouter/org/manual:variant", ""},
		{"openrouter", "org/manual:", "openrouter/org/manual:", ""},
		{"ollama", "model:latest", "ollama/model:latest", ""},
	} {
		f := &modelForm{provider: tc.provider, infos: map[string]llm.ModelInfo{"org/model:free": {ID: "org/model:free", Endpoints: []llm.ModelEndpointInfo{{ID: "host"}}}}}
		f.model.setText(tc.name)
		model, host, err := f.route()
		if model != tc.want || host != tc.host || err != nil {
			t.Fatalf("%+v: %s %s %v", tc, model, host, err)
		}
	}
}

func TestModelFormSavedRouteSurvivesCatalogChanges(t *testing.T) {
	for _, route := range []struct{ model, host string }{
		{"org/model:variant", ""},
		{"org/model", "upstream"},
		{"org/model:variant", "upstream/turbo"},
	} {
		for _, state := range []string{"loading", "failed", "refresh", "key-edit", "restored-text"} {
			t.Run(route.model+":"+route.host+"/"+state, func(t *testing.T) {
				r, _ := newFormREPL(t)
				r.state.settings.Model, r.state.settings.ModelHost = "openrouter/"+route.model, route.host
				r.openModelForm(3)
				f := r.model.modal.modelForm
				switch state {
				case "loading":
					f.status = "Loading suggestions…"
				case "failed":
					f.setCatalog(llm.ModelCatalog{Partial: true, Error: "discovery unavailable"})
				case "refresh":
					// Even a conflicting exact catalog ID cannot reinterpret a saved pin.
					f.setCatalog(llm.ModelCatalog{Models: []llm.ModelInfo{{ID: f.model.text()}}})
				case "key-edit":
					f.focus = 2
					formKey(r, "x")
					if !f.keyChanged || len(f.infos) != 0 {
						t.Fatal("credential edit did not clear discovery")
					}
				case "restored-text":
					f.infos["other"] = llm.ModelInfo{ID: "other"}
					f.model.setText("other")
					_, _, _ = f.route()
					f.setCatalog(llm.ModelCatalog{})
					f.model.setText(f.initialModel)
				}
				f.contextLimit.setText("32000")
				f.contextChanged = true
				r.applyModelForm(f)
				if r.model.modal != nil || r.state.settings.Model != "openrouter/"+route.model || r.state.settings.ModelHost != route.host || r.state.settings.MaxHistoryTokens != 32000 {
					t.Fatalf("context Apply changed route: model=%q host=%q error=%q", r.state.settings.Model, r.state.settings.ModelHost, f.err)
				}
			})
		}
	}
}

func TestModelFormDiscoveredSelectionSurvivesRefreshAndKeyEdit(t *testing.T) {
	for _, change := range []string{"refresh", "key-edit"} {
		t.Run(change, func(t *testing.T) {
			r, _ := newFormREPL(t)
			f := r.model.modal.modelForm
			f.provider = "openrouter"
			f.infos = map[string]llm.ModelInfo{"org/model:variant": {ID: "org/model:variant", Endpoints: []llm.ModelEndpointInfo{{ID: "host"}}}}
			f.model.setText("org/model:variant:h")
			f.suggestions = []string{"org/model:variant:host"}
			f.focus, f.completing, f.selected = 1, true, -1
			formKey(r, "<Tab>")
			if change == "refresh" {
				f.setCatalog(llm.ModelCatalog{Models: []llm.ModelInfo{{ID: "org/model:variant:host"}}})
			} else {
				f.focus = 2
				formKey(r, "x")
			}
			r.applyModelForm(f)
			if r.model.modal != nil || r.state.settings.Model != "openrouter/org/model:variant" || r.state.settings.ModelHost != "host" {
				t.Fatalf("selected route lost: model=%q host=%q error=%q", r.state.settings.Model, r.state.settings.ModelHost, f.err)
			}
		})
	}
}

func TestModelFormExactIDPrecedesDiscoveredRoute(t *testing.T) {
	f := &modelForm{provider: "openrouter", infos: map[string]llm.ModelInfo{
		"org/model":         {ID: "org/model", Endpoints: []llm.ModelEndpointInfo{{ID: "variant"}}},
		"org/model:variant": {ID: "org/model:variant"},
	}}
	f.model.setText("org/model:variant")
	model, host, err := f.route()
	if err != nil || model != "openrouter/org/model:variant" || host != "" {
		t.Fatalf("exact ID was split: %q %q %v", model, host, err)
	}
	f.setCatalog(llm.ModelCatalog{})
	model, host, err = f.route()
	if err != nil || model != "openrouter/org/model:variant" || host != "" {
		t.Fatal("refresh lost exact ID provenance")
	}
}

func TestModelFormProviderMouseAndRightAlignedApply(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	text := plainStyledText(f.modal.text(20, 60))
	f.modal.bounds = image.Rect(2, 2, 62, 26)
	if !strings.HasSuffix(strings.Split(text, "\n")[f.fieldRows[4]], "[ Apply ]") || f.applyBounds.Max.X != 58 {
		t.Fatal("Apply is not right aligned")
	}
	if !strings.Contains(text, "‹ openai ›") || strings.Contains(text, "anthropic") {
		t.Fatal("selector should show only the current provider")
	}
	at := f.providerBounds[1].Min.Add(image.Pt(3, 3))
	r.handleModalEvent(mouseEvent("<MouseLeft>", at))
	if f.provider != validModelProviders[1] || f.focus != 0 {
		t.Fatal("mouse provider selection failed")
	}
	f.model.setText("manual")
	f.modal.text(20, 60)
	r.handleModalEvent(mouseEvent("<MouseLeft>", f.applyBounds.Min.Add(image.Pt(3, 3))))
	if r.model.modal != nil || !strings.HasSuffix(r.state.settings.Model, "/manual") {
		t.Fatal("mouse Apply failed")
	}
}

func TestModelFormPersistenceFailureDoesNotInstallKey(t *testing.T) {
	r, mp := newFormREPL(t)
	store := testOpenMemoryStore(t, nil)
	r.state.session = testAcquireSession(t, store, "closed-form")
	r.state.session.Close()
	f := r.model.modal.modelForm
	f.model.setText("new")
	f.key.setText("new-secret")
	f.keyChanged = true
	r.applyModelForm(f)
	if f.err == "" || r.state.settings.Model != "openai/current" || mp.APIKeySource("openai") != "environment" {
		t.Fatal("failed persistence applied draft")
	}
}

func TestModelFormArrowNavigation(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	if f.status != "" {
		t.Fatal("unavailable catalog showed a notice")
	}
	f.suggestions = []string{"first", "second"}
	for _, step := range []struct {
		key   string
		focus int
	}{
		{"<Up>", 0}, {"<Up>", 0}, {"<Down>", 1}, {"<Down>", 2}, {"<Down>", 3},
		{"<Down>", 4}, {"<Down>", 4}, {"<Up>", 3}, {"<Up>", 2}, {"<Up>", 1},
	} {
		formKey(r, step.key)
		if f.focus != step.focus {
			t.Fatalf("%s: focus=%d", step.key, f.focus)
		}
	}
	if f.model.text() != "current" {
		t.Fatal("arrows changed model")
	}
	formKey(r, "<Up>")
	formKey(r, "<Tab>")
	if f.focus != 0 {
		t.Fatal("Tab changed provider focus")
	}
	previous := f.provider
	formKey(r, "<Right>")
	formKey(r, "<Left>")
	if f.provider != previous || f.focus != 0 {
		t.Fatal("horizontal provider navigation failed")
	}
}

func TestModelFormCatalogRefreshKeepsLoadedHostCompletions(t *testing.T) {
	yes := true
	caps := llm.ModelCapabilities{Chat: &yes, Tools: &yes}
	info := llm.ModelInfo{ID: "org/model", Routed: true, ModelCapabilities: caps, Endpoints: []llm.ModelEndpointInfo{{ID: "host", ModelCapabilities: caps}}}
	f := &modelForm{provider: "openrouter", selected: 0, completing: true, completionQuery: "org/", suggestions: []string{"org/model", "org/model:host"}, details: map[string]llm.ModelInfo{"org/model": info}}
	f.model.setText("org/model")
	f.setCatalog(llm.ModelCatalog{Models: []llm.ModelInfo{{ID: "org/model", Routed: true, ModelCapabilities: caps}}})
	f.updateSuggestions()
	if !slices.Equal(f.suggestions, []string{"org/model", "org/model:host"}) || f.selected != 0 || f.model.text() != "org/model" {
		t.Fatal("refresh lost host completion or changed the input")
	}
}

func TestStatusModelClickOpensFormWithoutChangingDraft(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	r.state = &conversationState{settings: Settings{Model: "openai/test-model"}}
	m := r.model
	m.status.modelName = r.state.settings.Model
	m.ed.setText("unfinished prompt")
	m.busy = true
	r.render()
	_, height := screen.Size()
	placement := m.status.modelField
	if placement.Cols == 0 {
		t.Fatal("model has no click target")
	}
	point := image.Pt(placement.X, height-1)
	hoverAt(t, r, point)
	if m.modal != nil || underlinedRun(screen, height-1) != "test-model" {
		t.Fatal("model hover did not underline its target")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", point))
	if m.modal == nil || m.modal.modelForm == nil || m.modal.modelForm.model.text() != "test-model" {
		t.Fatal("model click did not open the current model form")
	}
	if m.ed.text() != "unfinished prompt" || !m.busy {
		t.Fatal("model click disturbed the conversation")
	}
	r.closeModal()
	m.statusRow(8)
	if m.status.modelField.Cols != 0 {
		t.Fatal("hidden model retained a click target")
	}
	m.statusRow(100)
	m.quiet = true
	m.statusRow(100)
	if m.status.modelField.Cols != 0 {
		t.Fatal("quiet status retained model target")
	}
}

func TestSetupFormFieldsAndSkipRecordsFirstRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	r, _ := newFormREPL(t)
	r.config.Setup = true
	r.config.BaseURL = "http://localhost:11434/v1"
	r.closeModal()
	r.openSetupForm()
	f := r.model.modal.modelForm
	if !f.setup || f.modal.title != "Setup" || f.applyIndex() != formFieldThinking+1 {
		t.Fatalf("setup form state: %+v", f)
	}
	text := plainStyledText(f.modal.text(30, 76))
	for _, want := range []string{"Endpoint http://localhost:11434/v1", "Thinking ‹ off ›", "[ Apply ]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("form lacks %q:\n%s", want, text)
		}
	}
	// Down walks through both extra fields to Apply; the thinking field
	// cycles with the arrows.
	for f.focus != formFieldThinking {
		formKey(r, "<Down>")
	}
	formKey(r, "<Right>")
	if f.thinking != "dynamic" {
		t.Fatalf("thinking after Right = %q", f.thinking)
	}
	formKey(r, "<Left>")
	formKey(r, "<Left>")
	if f.thinking == "off" || f.thinking == "dynamic" {
		t.Fatalf("thinking did not wrap: %q", f.thinking)
	}
	formKey(r, "<Down>")
	if f.focus != f.applyIndex() {
		t.Fatalf("focus = %d, want Apply", f.focus)
	}
	formKey(r, "<Escape>")
	if r.model.modal != nil {
		t.Fatal("Escape did not close the first-run form")
	}
	path := filepath.Join(home, userConfigDirName, userConfigFileName)
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != userConfigHeader {
		t.Fatalf("skip should write a header-only file: %q %v", raw, err)
	}
	if !strings.Contains(strings.Join(transcriptTexts(r.model), "\n"), "Setup skipped") {
		t.Fatalf("notices: %v", transcriptTexts(r.model))
	}
}

func TestSetupFormApplySavesDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"POLLYTOOL_MODEL", "POLLYTOOL_BASEURL", "POLLYTOOL_THINKING", "POLLYTOOL_MODELHOST"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	r, mp := newFormREPL(t)
	store := testOpenMemoryStore(t, nil)
	r.state.session = testAcquireSession(t, store, "form")
	r.closeModal()
	r.openSetupForm()
	f := r.model.modal.modelForm
	f.model.setText("gpt-5.4")
	f.key.setText("sk-saved")
	f.keyChanged = true
	f.endpoint.setText(" http://localhost:8080/v1 ")
	f.thinking = "high"
	f.focus = f.applyIndex()
	formKey(r, "<Enter>")
	if r.model.modal != nil {
		t.Fatalf("Apply left the form open: %q", f.err)
	}
	if mp.APIKeySource("openai") != "session" {
		t.Fatal("key not installed for the process")
	}
	if r.state.settings.Model != "openai/gpt-5.4" || r.state.settings.ThinkingEffort != "high" {
		t.Fatalf("session settings: %+v", r.state.settings)
	}
	md, err := r.state.session.GetMetadata(context.Background())
	if err != nil || md.Model != "openai/gpt-5.4" || md.ThinkingEffort != "high" {
		t.Fatalf("metadata: %+v %v", md, err)
	}
	if r.config.BaseURL != "http://localhost:8080/v1" || r.config.Launch.Model != "openai/gpt-5.4" || r.config.Launch.ThinkingEffort != "high" || r.state.metadataBaseURL != "http://localhost:8080/v1" {
		t.Fatalf("process config: %+v", r.config)
	}
	raw, err := os.ReadFile(filepath.Join(home, userConfigDirName, userConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	want := userConfigHeader + "POLLYTOOL_BASEURL=http://localhost:8080/v1\nPOLLYTOOL_MODEL=openai/gpt-5.4\nPOLLYTOOL_THINKING=high\n"
	if string(raw) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", raw, want)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "defaults saved") || strings.Contains(got, "your environment") {
		t.Fatalf("notices: %v", transcriptTexts(r.model))
	}
	// Re-running setup with thinking off and no endpoint drops those lines
	// and keeps the key.
	r.openSetupForm()
	f = r.model.modal.modelForm
	f.endpoint.clear()
	f.thinking = "off"
	f.focus = f.applyIndex()
	formKey(r, "<Enter>")
	raw, err = os.ReadFile(filepath.Join(home, userConfigDirName, userConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	want = userConfigHeader + "POLLYTOOL_MODEL=openai/gpt-5.4\n"
	if string(raw) != want {
		t.Fatalf("file after second setup:\n%s\nwant:\n%s", raw, want)
	}
	if r.config.BaseURL != "" || r.config.Launch.ThinkingEffort != "off" {
		t.Fatalf("cleared values still in the process config: %+v", r.config)
	}
	// A model exported by the shell beats the file; Apply says so.
	t.Setenv("POLLYTOOL_MODEL", "openai/shell")
	r.openSetupForm()
	f = r.model.modal.modelForm
	f.focus = f.applyIndex()
	formKey(r, "<Enter>")
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "your environment") || !strings.Contains(got, "POLLYTOOL_MODEL") {
		t.Fatalf("no shadowing notice: %q", got)
	}
}

func TestSetupCommandOpensSetupForm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, _ := newFormREPL(t)
	r.closeModal()
	if handled, quit := r.runCommand("/setup"); !handled || quit || r.model.modal == nil || !r.model.modal.modelForm.setup {
		t.Fatal("/setup should open the setup form")
	}
}

func TestSetupFormRefusesProviderWithoutKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, mp := newFormREPL(t)
	r.closeModal()
	r.openSetupForm()
	f := r.model.modal.modelForm
	// openai has an environment key; anthropic has none, so Apply must ask
	// for one rather than save an unusable default.
	f.provider = "anthropic"
	f.hasKey = false
	f.model.setText("claude-sonnet-4-6")
	f.focus = f.applyIndex()
	formKey(r, "<Enter>")
	if r.model.modal == nil || !strings.Contains(f.err, "key") || f.focus != formFieldKey {
		t.Fatalf("Apply without a key: modal=%v err=%q focus=%d", r.model.modal != nil, f.err, f.focus)
	}
	f.key.setText("sk-new")
	f.keyChanged = true
	f.focus = f.applyIndex()
	formKey(r, "<Enter>")
	if r.model.modal != nil || mp.APIKeySource("anthropic") != "session" {
		t.Fatalf("Apply with a key: modal=%v err=%q", r.model.modal != nil, f.err)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "POLLYTOOL_ANTHROPICKEY") {
		t.Fatalf("no reminder that the key is process-only: %q", got)
	}
}
