package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelFormExplainsPersistenceBeforeApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, _ := newFormREPL(t)
	for _, setup := range []bool{false, true} {
		if setup {
			r.closeModal()
			r.openSetupForm()
		}
		f := r.model.modal.modelForm
		text := plainStyledText(f.text(30, f.modal.width))
		wants := []string{"Key override: applies now, this process only, never saved."}
		if setup {
			wants = append(wants,
				"Model/host/effort/endpoint/theme/sandbox: saved as launch defaults.",
				"Model/host/context/effort: saved to the current session.",
				"Model, effort, theme and endpoint apply now.",
				"Sandbox default: later launches only; unchanged now.")
		} else {
			wants = append(wants, "Apply saves model/host/context to the current session.")
		}
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatalf("setup=%v: form lacks %q before Apply:\n%s", setup, want, text)
			}
		}
		if strings.Contains(text, "/keys") {
			t.Fatalf("setup=%v: form promotes /keys: %s", setup, text)
		}
	}
}

func TestModelFormScopeMatchesPersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, userConfigDirName, userConfigFileName)
	writeFile(t, path, "POLLYTOOL_MODEL=openai/launch-default\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, mp := newFormREPL(t)
	store := testOpenMemoryStore(t, nil)
	r.state.session = testAcquireSession(t, store, "form-scope")
	f := r.model.modal.modelForm
	f.model.setText("session-model")
	f.contextLimit.setText("32000")
	f.contextChanged = true
	f.key.setText("scope-test-secret")
	f.keyChanged = true
	r.applyModelForm(f)
	if r.model.modal != nil {
		t.Fatalf("Apply left the form open: %q", f.err)
	}
	md, err := r.state.session.GetMetadata(context.Background())
	if err != nil || md.Model != "openai/session-model" || md.MaxHistoryTokens != 32000 {
		t.Fatalf("session metadata: %+v, %v", md, err)
	}
	if mp.APIKeySource("openai") != "session" {
		t.Fatal("key override was not installed")
	}
	got := strings.Join(transcriptTexts(r.model), "\n")
	for _, want := range []string{"key override applied to this process only", "never saved", "export POLLYTOOL_OPENAIKEY for later launches"} {
		if !strings.Contains(got, want) {
			t.Fatalf("confirmation lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "scope-test-secret") {
		t.Fatal("confirmation exposed the key")
	}
	r.openModelPicker()
	f = r.model.modal.modelForm
	f.keyChanged = true // Empty draft restores the configured source.
	r.applyModelForm(f)
	if mp.APIKeySource("openai") != "environment" {
		t.Fatal("clearing the override did not restore the configured source")
	}
	got = strings.Join(transcriptTexts(r.model), "\n")
	if !strings.Contains(got, "key override cleared for this process only · nothing saved") {
		t.Fatalf("clear confirmation omits scope: %s", got)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("/model changed launch defaults: %q, %v", after, err)
	}
}
