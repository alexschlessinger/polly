package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThemePickerExplainsPersistenceBeforeSelection(t *testing.T) {
	themeTestRestoreDefault(t)
	themeTestHome(t)
	r, _ := chromeTestREPL(t)
	r.openThemePicker()
	modal := r.model.modal
	text := modal.title + "\n" + plainStyledText(modal.text(20, modal.width))
	for _, want := range []string{"UI + launch default", "apply to the UI now and save as the default", "for later launches", "previews", "Esc cancels"} {
		if !strings.Contains(text, want) {
			t.Fatalf("picker lacks %q before selection: %s", want, text)
		}
	}
	modal.onSelect("amber-parrot")
	if _, saved := themeSelectionFromConfig(); saved {
		t.Fatal("preview saved a launch default")
	}
	formKey(r, "<Escape>")
	if r.activeThemeName() != themeNameDefault {
		t.Fatal("cancel changed the active theme")
	}
	if _, saved := themeSelectionFromConfig(); saved {
		t.Fatal("cancel saved a launch default")
	}
	r.openThemePicker()
	for i := 0; i < len(r.model.modal.items) && r.model.modal.selectedValue() != "amber-parrot"; i++ {
		formKey(r, "<Up>")
	}
	if r.model.modal.selectedValue() != "amber-parrot" {
		t.Fatal("could not select amber-parrot")
	}
	formKey(r, "<Enter>")
	got := strings.Join(transcriptTexts(r.model), "\n")
	for _, want := range []string{"active theme: amber-parrot", "applied to UI now", "saved as default for later launches"} {
		if !strings.Contains(got, want) {
			t.Fatalf("picker confirmation lacks %q: %s", want, got)
		}
	}
	if saved, _ := themeSelectionFromConfig(); saved != "amber-parrot" || r.activeThemeName() != saved {
		t.Fatalf("theme not applied and saved: active=%q saved=%q", r.activeThemeName(), saved)
	}
}

func TestThemePickerSaveFailureDistinguishesAppliedFromSaved(t *testing.T) {
	themeTestRestoreDefault(t)
	home := themeTestHome(t)
	r, _ := chromeTestREPL(t)
	// A directory at the config path refuses persistence without blocking the UI switch.
	if err := os.MkdirAll(filepath.Join(home, userConfigDirName, userConfigFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.switchTheme("amber-parrot"), "\n")
	if !strings.Contains(got, "applied to UI now") || !strings.Contains(got, "Warning: theme not saved for later launches:") || strings.Contains(got, "saved as default") {
		t.Fatalf("save failure confirmation confuses scopes: %s", got)
	}
	if r.activeThemeName() != "amber-parrot" {
		t.Fatal("save failure reverted the UI switch")
	}
}
