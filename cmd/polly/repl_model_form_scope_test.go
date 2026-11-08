package main

import (
	"strings"
	"testing"
)

func TestModelFormOmitsPersistenceProse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, _ := newFormREPL(t)
	for _, setup := range []bool{false, true} {
		if setup {
			r.closeModal()
			r.openSetupForm()
		}
		f := r.model.modal.modelForm
		text := plainStyledText(f.text(30, f.modal.width))
		for _, unwanted := range []string{
			"Key override",
			"saved as launch defaults",
			"saved to the current session",
			"apply now",
			"unchanged now",
			"/keys",
		} {
			if strings.Contains(text, unwanted) {
				t.Fatalf("setup=%v: form shows %q:\n%s", setup, unwanted, text)
			}
		}
	}
}
