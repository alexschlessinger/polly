package main

// openThemePicker is /theme with no argument: every theme /theme accepts by
// name, the active one selected. Moving the selection previews a theme on the
// whole screen without following it; Enter switches the session to it exactly
// as "/theme name" does, and Escape puts the theme in effect back.
//
// It runs where /theme does — on the event loop with the model lock held — so
// the preview may apply directly and rides the frame the key already paints.
func (r *managedREPL) openThemePicker() {
	active := r.activeThemeName()
	names := allThemeNames()
	items := make([]replModalItem, 0, len(names))
	selected := 0
	for i, name := range names {
		item := replModalItem{label: name, value: name}
		if name == active {
			item.label += " (active)"
			selected = i
		}
		items = append(items, item)
	}
	r.openModal(&replModal{
		title:    "Theme",
		width:    48,
		hideHelp: true,
		items:    items,
		selected: selected,
		onSelect: func(name string) {
			// A file that does not load previews as nothing; choosing it
			// reports why.
			if selection, err := resolveThemeSelection(name); err == nil {
				r.applyTheme(selection.theme)
			}
		},
		onSubmit: func(name string) {
			lines := r.switchTheme(name)
			if lines == nil {
				r.restoreActiveTheme()
			}
			for _, line := range lines {
				r.model.appendNoticeLine(line)
			}
		},
		onCancel: r.restoreActiveTheme,
	})
}

// switchTheme is a deliberate choice of theme — "/theme name" or Enter in the
// picker: it switches the session and saves the name as the launch default in
// ~/.pollytool/config, the line set_theme's persist writes, so the choice
// survives a restart. It returns the lines to report, nil when the theme did
// not load (applyThemeByName already said why).
func (r *managedREPL) switchTheme(name string) []string {
	if _, err := r.applyThemeByName(name); err != nil {
		return nil
	}
	lines := []string{r.activeThemeLine()}
	if err := saveThemeSelection(name); err != nil {
		return append(lines, "Warning: theme not saved for later launches: "+err.Error())
	}
	if warning := themeShadowedWarning(name); warning != "" {
		lines = append(lines, "Warning: "+warning)
	}
	return lines
}
