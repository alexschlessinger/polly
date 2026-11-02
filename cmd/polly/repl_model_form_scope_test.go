package main

import (
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
		}
		if strings.Contains(text, "Apply saves model/host/context to the current session.") {
			t.Fatalf("setup=%v: form shows redundant Apply hint:\n%s", setup, text)
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
