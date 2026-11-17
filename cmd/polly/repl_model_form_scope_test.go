package main

import (
	"strings"
	"testing"
)

func TestModelFormOmitsPersistenceProse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	text := plainStyledText(f.text(30, 80))
	for _, unwanted := range []string{
		"Key override",
		"saved as launch defaults",
		"saved to the current session",
		"apply now",
		"unchanged now",
		"/keys",
	} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("form shows %q:\n%s", unwanted, text)
		}
	}
}
