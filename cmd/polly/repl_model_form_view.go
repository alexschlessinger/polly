package main

import (
	"cmp"
	"fmt"
	"image"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	rw "github.com/mattn/go-runewidth"
)

// The model form docks in the composer's place under the rule, without a
// border: a title row with the form's scope and its key hints, then one row
// per field — caret, label, value, and a muted note in the right-hand
// column. The /model form is a picker: provider tabs, a filter line, and
// the provider's models with their context and prices, Key and Context
// folded into one footer line until Tab opens them.

const (
	formPad           = 1  // cells of padding at each side
	formCaretW        = 2  // "› " on the focused row
	formLabelW        = 10 // label column, its gap included
	formNoteGap       = 3  // least space between a value and its note
	formMinNote       = 8  // a note with less room is dropped, not stubbed
	formMinTranscript = 3  // transcript rows a docked form leaves above it
	formListRows      = 8  // models the picker lists at once
	formContextW      = 9  // the picker's context column
	formPriceW        = 10 // each of the picker's price columns
)

// formValueX is the cell where field values start.
const formValueX = formPad + formCaretW + formLabelW

// dockedFormRows is how many rows the open model form takes in the
// composer's place in a w×h terminal, 0 when none is open. Caller holds m.mu.
func (m *replModel) dockedFormRows(w, h, statusRows int) int {
	if m.modal == nil || m.modal.modelForm == nil {
		return 0
	}
	return strings.Count(m.modal.modelForm.text(dockedFormMaxRows(h, statusRows), w), "\n") + 1
}

// dockedFormMaxRows leaves the status bar, the rule, and a few transcript
// rows above the form.
func dockedFormMaxRows(h, statusRows int) int {
	return max(1, h-statusRows-1-formMinTranscript)
}

type formHitKind int

const (
	hitField    formHitKind = iota // focus field n
	hitCycle                       // step the focused field back (n < 0) or on
	hitProvider                    // switch to validModelProviders[n]
	hitPick                        // highlight suggestions[n]
	hitApply
)

// formHit is a click target in form coordinates: x from the form's left
// edge, y its row.
type formHit struct {
	rect image.Rectangle
	kind formHitKind
	n    int
}

// hitAt is the target under p. Targets added later sit on earlier ones, so
// the last match wins.
func (f *modelForm) hitAt(p image.Point) (formHit, bool) {
	for i := len(f.hits) - 1; i >= 0; i-- {
		if p.In(f.hits[i].rect) {
			return f.hits[i], true
		}
	}
	return formHit{}, false
}

func (f *modelForm) hit(kind formHitKind, n, x0, y, x1 int) {
	if x1 > x0 {
		f.hits = append(f.hits, formHit{rect: image.Rect(x0, y, x1, y+1), kind: kind, n: n})
	}
}

// formSpan is one styled piece of a form row.
type formSpan struct{ text, role, mod string }

func spansWidth(spans []formSpan) int {
	n := 0
	for _, s := range spans {
		n += rw.StringWidth(metadataDisplayText(s.text, false))
	}
	return n
}

func mutedSpans(spans []formSpan) []formSpan {
	out := make([]formSpan, len(spans))
	for i, s := range spans {
		out[i] = formSpan{s.text, "muted", ""}
	}
	return out
}

func mutedNote(text string) []formSpan {
	if text == "" {
		return nil
	}
	return []formSpan{{text, "muted", ""}}
}

// fitSpans renders spans in at most limit cells. The span that crosses the
// limit ends in an ellipsis and the rest are dropped.
func fitSpans(spans []formSpan, limit int) (string, int) {
	var b strings.Builder
	used := 0
	for _, s := range spans {
		text := metadataDisplayText(s.text, false)
		w := rw.StringWidth(text)
		if used+w > limit {
			text = rw.Truncate(text, max(0, limit-used), "…")
			b.WriteString(style.Styled(text, s.role, s.mod))
			return b.String(), used + rw.StringWidth(text)
		}
		b.WriteString(style.Styled(text, s.role, s.mod))
		used += w
	}
	return b.String(), used
}

// formLine lays out a row width cells wide: left from the padding, note
// right-aligned against the far padding. The note gives way to the left
// side, and is dropped when it would get fewer than formMinNote cells.
// noteX is the note's first cell, -1 when dropped.
func formLine(width int, left, note []formSpan) (string, int) {
	inner := max(1, width-2*formPad)
	noteW := spansWidth(note)
	if noteW > 0 {
		room := inner - spansWidth(left) - formNoteGap
		if room < min(noteW, formMinNote) {
			noteW = 0
		} else {
			noteW = min(noteW, room)
		}
	}
	limit := inner
	if noteW > 0 {
		limit = inner - noteW - formNoteGap
	}
	text, used := fitSpans(left, limit)
	row := strings.Repeat(" ", formPad) + text
	if noteW == 0 {
		return row, -1
	}
	noteText, w := fitSpans(note, noteW)
	gap := inner - used - w
	return row + strings.Repeat(" ", gap) + noteText, formPad + used + gap
}

func (f *modelForm) text(maxRows, width int) string {
	f.hits = f.hits[:0]
	if f.modal != nil {
		// The form owns its click targets and says everything on its rows.
		f.modal.titleNotice = ""
		f.modal.visible = 0
	}
	f.syncContextLimit()
	var rows []string
	if f.picker {
		rows = f.pickerRows(maxRows, width)
	} else {
		rows = f.setupRows(maxRows, width)
	}
	return strings.Join(rows, "\n")
}

// titleRow names the form and what Apply changes, or the error that stopped
// it, with the key hints on the right; "apply" is a link.
func (f *modelForm) titleRow(width, y int) string {
	title, scope := "Model", "this session"
	if f.setup {
		title, scope = "Setup", "launch defaults"
	}
	left := []formSpan{{title, "", "bold"}, {"  " + scope, "muted", ""}}
	if f.err != "" {
		left[1] = formSpan{"  " + f.err, "err", ""}
	}
	room := max(1, width-2*formPad) - spansWidth(left) - formNoteGap
	for _, pairs := range hintVariants(f.keyHints()) {
		hints, applyAt, applyW := hintSpans(pairs, f.changed())
		if spansWidth(hints) > room {
			continue
		}
		row, x := formLine(width, left, hints)
		if x >= 0 && applyAt >= 0 {
			f.hit(hitApply, 0, x+applyAt, y, x+applyAt+applyW)
		}
		return row
	}
	row, _ := formLine(width, left, nil)
	return row
}

func (f *modelForm) keyHints() [][2]string {
	hints := [][2]string{{"↑↓", "move"}}
	switch {
	case f.picker && f.focus == formFieldModel:
		hints = [][2]string{{"←→", "provider"}, {"↑↓", "choose"}, {"Tab", "key"}}
	case f.picker:
		hints = append(hints, [2]string{"Tab", "next"})
	case f.focus == formFieldModel:
		hints = append(hints, [2]string{"Tab", "complete"})
	case f.cycles(f.focus):
		hints = append(hints, [2]string{"←→", "change"})
	}
	return append(hints, [2]string{"Enter", "apply"}, [2]string{"Esc", "cancel"})
}

// hintVariants are the hint lists to try, longest first: the leading hints
// give way one by one, and Enter apply outlasts Esc cancel.
func hintVariants(hints [][2]string) [][][2]string {
	var out [][][2]string
	for k := 0; k+1 < len(hints); k++ {
		out = append(out, hints[k:])
	}
	return append(out, hints[len(hints)-2:len(hints)-1])
}

// hintSpans renders key hints like style.KeyHints: the key in the text
// color, its verb muted, middle dots between. The apply verb is a link,
// bold while Apply would change something. applyAt is the offset of the
// apply pair, -1 when absent.
func hintSpans(pairs [][2]string, changed bool) (spans []formSpan, applyAt, applyW int) {
	applyAt, at := -1, 0
	for i, p := range pairs {
		if i > 0 {
			spans = append(spans, formSpan{" · ", "muted", ""})
			at += 3
		}
		verb := formSpan{" " + p[1], "muted", ""}
		w := rw.StringWidth(p[0]) + 1 + rw.StringWidth(p[1])
		if p[1] == "apply" {
			verb.role = "accent"
			if changed {
				verb.mod = "bold"
			}
			applyAt, applyW = at, w
		}
		spans = append(spans, formSpan{p[0], "", ""}, verb)
		at += w
	}
	return spans, applyAt, applyW
}

// cycles reports whether Left and Right step field n through its values.
func (f *modelForm) cycles(n int) bool {
	switch n {
	case formFieldProvider, formFieldThinking, formFieldTheme, formFieldSandbox:
		return true
	}
	return false
}

// setupRows is the setup form: the title, then a row per field. A short
// terminal drops the blank rows first, then shows the fields around focus.
func (f *modelForm) setupRows(maxRows, width int) []string {
	fields := []int{formFieldProvider, formFieldModel, formFieldKey, formFieldContext, formFieldEndpoint, formFieldThinking, formFieldTheme, formFieldSandbox}
	rows := []string{f.titleRow(width, 0)}
	room := max(1, maxRows) - 1
	roomy := room >= len(fields)+2
	if roomy {
		rows = append(rows, "")
		room -= 2
	}
	start := 0
	if room < len(fields) {
		start = max(0, min(f.focus-room/2, len(fields)-room))
	}
	for _, n := range fields[start:min(len(fields), start+room)] {
		rows = append(rows, f.fieldRow(width, len(rows), n))
	}
	if roomy {
		rows = append(rows, "")
	}
	return rows
}

// fieldRow is one field: caret, label, value, note. A focused cycler shows
// its arrows, and they step it when clicked.
func (f *modelForm) fieldRow(width, y, n int) string {
	label, value, note := f.field(n, max(1, width-formValueX-formPad))
	caret, name := formSpan{"  ", "", ""}, formSpan{rw.FillRight(label, formLabelW), "", ""}
	focused := f.focus == n
	if focused {
		caret = formSpan{"› ", "accent", "bold"}
		name.role, name.mod = "accent", "bold"
	}
	row, _ := formLine(width, append([]formSpan{caret, name}, value...), note)
	f.hit(hitField, n, 0, y, width)
	if focused && f.cycles(n) {
		end := formValueX + spansWidth(value)
		f.hit(hitCycle, -1, formValueX, y, formValueX+2)
		f.hit(hitCycle, 1, end-2, y, end)
	}
	return row
}

func cycler(value string, focused bool) []formSpan {
	if !focused {
		return []formSpan{{value, "", ""}}
	}
	return []formSpan{{"‹ ", "accent", ""}, {value, "", ""}, {" ›", "accent", ""}}
}

// position is a focused cycler's note: where its value sits in the list.
func position(values []string, value string, focused bool) []formSpan {
	i := slices.Index(values, value)
	if !focused || i < 0 || len(values) < 2 {
		return nil
	}
	return mutedNote(fmt.Sprintf("%d of %d", i+1, len(values)))
}

// field is field n's label, value, and note; width bounds an editor's text.
func (f *modelForm) field(n, width int) (string, []formSpan, []formSpan) {
	focused := f.focus == n
	switch n {
	case formFieldProvider:
		return "Provider", cycler(f.provider, focused), position(validModelProviders, f.provider, focused)
	case formFieldModel:
		var value []formSpan
		switch {
		case focused:
			value = []formSpan{{formEditorText(&f.model, false, width), "", ""}}
			if f.model.cursor == len(f.model.buf) {
				value = append(value, formSpan{f.completionShadow(), "muted", "dim"})
			}
		case strings.TrimSpace(f.model.text()) == "":
			value = []formSpan{{"not set", "muted", ""}}
		default:
			value = []formSpan{{f.model.text(), "", ""}}
		}
		return "Model", value, mutedNote(cmp.Or(f.modelPricing(), f.status))
	case formFieldKey:
		return f.keyField(focused, width)
	case formFieldContext:
		value, note := f.contextField(focused, width)
		return "Context", value, note
	case formFieldEndpoint:
		value := []formSpan{{f.endpoint.text(), "", ""}}
		if focused {
			value = []formSpan{{formEditorText(&f.endpoint, false, width), "", ""}}
		} else if strings.TrimSpace(f.endpoint.text()) == "" {
			value = []formSpan{{"provider default", "muted", ""}}
		}
		return "Endpoint", value, nil
	case formFieldThinking:
		return "Effort", cycler(f.thinking, focused), nil
	case formFieldTheme:
		return "Theme", cycler(f.theme, focused), position(f.themeNames, f.theme, focused)
	case formFieldSandbox:
		// The values name the --sandbox vocabulary: the preset that
		// applies, or none at all.
		value, note := f.sandboxPreset, ""
		if !f.sandbox {
			value, note = "none", "tool calls run unsandboxed"
		}
		if f.sandbox != f.initialSandbox {
			note = "from the next launch"
		}
		return "Sandbox", cycler(value, focused), mutedNote(note)
	}
	return "", nil, nil
}

// keyField is the Key row: where the provider's key comes from, or the
// sign-in for a provider served on one. Focused, a typed key shows masked;
// before any typing the source sits muted after the cursor, since typing
// replaces it.
func (f *modelForm) keyField(focused bool, width int) (string, []formSpan, []formSpan) {
	if f.login {
		if f.signedIn {
			plan := ""
			if f.plan != "" {
				plan = f.plan + " plan"
			}
			return "Account", []formSpan{{f.account, "", ""}}, mutedNote(plan)
		}
		return "Account", []formSpan{{"not signed in", "", ""}}, mutedNote("/login " + f.provider)
	}
	envVar := llm.ProviderKeyEnvVar(f.provider)
	var value []formSpan
	note := "this process only"
	switch {
	case f.keyChanged && len(f.key.buf) > 0:
		value = []formSpan{{strings.Repeat("*", len(f.key.buf)), "", ""}}
	case f.keyChanged:
		value = []formSpan{{"cleared", "muted", ""}}
	case f.keyOrigin == "environment":
		value, note = []formSpan{{"environment", "", ""}}, envVar
	case f.keyOrigin == "session":
		value = []formSpan{{"override", "", ""}}
	case f.provider == "ollama":
		value, note = []formSpan{{"none", "muted", ""}}, "optional"
	default:
		value, note = []formSpan{{"not set", "muted", ""}}, "type one or export "+envVar
	}
	if focused {
		if f.keyChanged {
			value = []formSpan{{formEditorText(&f.key, true, width), "", ""}}
		} else {
			value = append([]formSpan{{"│", "", ""}}, mutedSpans(value)...)
		}
	}
	return "Key", value, mutedNote(note)
}

// contextField is the Context row: the limit in tokens, digits while it is
// edited, and the model's window when the catalog gives one.
func (f *modelForm) contextField(focused bool, width int) ([]formSpan, []formSpan) {
	text := strings.TrimSpace(f.contextLimit.text())
	value := []formSpan{{text, "", ""}}
	if focused {
		value = []formSpan{{formEditorText(&f.contextLimit, false, width), "", ""}}
	} else if n, err := strconv.Atoi(text); err == nil && n > 0 {
		value = []formSpan{{humanizeTokens(n), "", ""}}
	}
	// The window is worth a note while the limit is edited or falls short.
	var note []formSpan
	if window := f.modelContextWindow(); window > 0 && (focused || text != strconv.Itoa(window)) {
		note = mutedNote("of " + humanizeTokens(window))
	}
	return value, note
}

// pickerRows is the /model picker. Its height holds still while the list
// filters and Tab opens Key and Context; a short terminal drops the blank
// rows, then lists fewer models.
func (f *modelForm) pickerRows(maxRows, width int) []string {
	expanded := f.focus != formFieldModel
	roomy := maxRows >= 8+formListRows
	footer := 1
	if roomy || expanded {
		footer = 2
	}
	list := formListRows
	if !roomy {
		list = max(1, maxRows-4-footer)
	}
	rows := []string{f.titleRow(width, 0)}
	if roomy {
		rows = append(rows, "")
	}
	rows = append(rows, f.providerTabs(width, len(rows)))
	rows = append(rows, f.filterRow(width, len(rows)))
	rows = append(rows, f.listRows(width, len(rows), list)...)
	if expanded {
		rows = append(rows, f.fieldRow(width, len(rows), formFieldKey))
		rows = append(rows, f.fieldRow(width, len(rows), formFieldContext))
	} else {
		if roomy {
			rows = append(rows, "")
		}
		rows = append(rows, f.footerRow(width, len(rows)))
	}
	if roomy {
		rows = append(rows, "")
	}
	return rows[:min(len(rows), max(1, maxRows))]
}

// providerTabs lists the providers, the current one highlighted. When they
// do not all fit, the row starts late enough to show the current one.
func (f *modelForm) providerTabs(width, y int) string {
	const gap = 2
	inner := max(1, width-2*formPad-formCaretW)
	current := max(0, slices.Index(validModelProviders, f.provider))
	span := func(from, to int) int {
		w := 0
		for i := from; i <= to; i++ {
			w += rw.StringWidth(validModelProviders[i]) + gap
		}
		return w
	}
	start := 0
	for start < current && span(start, current)+2 > inner {
		start++
	}
	spans := []formSpan{{"  ", "", ""}}
	x := formPad + formCaretW
	if start > 0 {
		spans = append(spans, formSpan{"‹ ", "muted", ""})
		x += 2
	}
	used := x - formPad
	for i := start; i < len(validModelProviders); i++ {
		name := validModelProviders[i]
		w := rw.StringWidth(name)
		if used+w > inner+formCaretW {
			spans = append(spans, formSpan{"›", "muted", ""})
			break
		}
		tab := formSpan{name, "muted", ""}
		if i == current {
			tab = formSpan{name, "accent", "bold"}
		}
		spans = append(spans, tab, formSpan{strings.Repeat(" ", gap), "", ""})
		f.hit(hitProvider, i, x, y, x+w)
		x += w + gap
		used += w + gap
	}
	row, _ := formLine(width, spans, nil)
	return row
}

// filterRow is the picker's model field. It carries the caret while no
// model is highlighted, when Enter applies the name as typed.
func (f *modelForm) filterRow(width, y int) string {
	focused := f.focus == formFieldModel
	caret := formSpan{"  ", "", ""}
	if focused && f.selected < 0 {
		caret = formSpan{"› ", "accent", "bold"}
	}
	value := []formSpan{{f.model.text(), "", ""}}
	if focused {
		value = []formSpan{{formEditorText(&f.model, false, max(1, width-2*formPad-formCaretW)), "", ""}}
	}
	if f.model.text() == "" {
		value = append(value, formSpan{"type to filter", "muted", ""})
	}
	note := f.status
	if note == "" && f.total > 0 {
		note = fmt.Sprintf("%d models", f.total)
		if strings.TrimSpace(f.model.text()) != "" {
			note = fmt.Sprintf("%d of %d", len(f.suggestions), f.total)
		}
	}
	row, _ := formLine(width, append([]formSpan{caret}, value...), mutedNote(note))
	f.hit(hitField, formFieldModel, 0, y, width)
	return row
}

// listRows is the column header and n rows of models scrolled to keep the
// highlighted one in view; rows past the end stay blank, so the form's
// height holds while the filter narrows the list.
func (f *modelForm) listRows(width, y, n int) []string {
	inner := max(1, width-2*formPad)
	// A column shows when some listed model fills it and the row has room.
	type facts struct{ context, in, out string }
	known := make([]facts, len(f.suggestions))
	var anyContext, anyPrice bool
	for i, name := range f.suggestions {
		if info, host, ok := f.suggestionInfo(name); ok {
			if window := info.EffectiveCapabilities(host).ContextWindow(); window > 0 {
				known[i].context = humanizeTokens(window)
			}
			known[i].in, known[i].out, _ = routeRates(info, host)
		}
		anyContext = anyContext || known[i].context != ""
		anyPrice = anyPrice || known[i].in != "" || known[i].out != ""
	}
	showContext, showPrices := anyContext && inner >= 40, anyPrice && inner >= 64
	columns := func(context, in, out string, role string) []formSpan {
		var spans []formSpan
		if showContext {
			spans = append(spans, formSpan{rw.FillLeft(context, formContextW), role, ""})
		}
		if showPrices {
			spans = append(spans, formSpan{rw.FillLeft(in, formPriceW), role, ""}, formSpan{rw.FillLeft(out, formPriceW), role, ""})
		}
		return spans
	}
	rows := make([]string, 0, n+1)
	if len(f.suggestions) == 0 {
		message := cmp.Or(f.status, "no models listed")
		if strings.TrimSpace(f.model.text()) != "" && f.status == "" {
			message = "no model matches · Enter uses the name as typed"
		}
		row, _ := formLine(width, []formSpan{{"  ", "", ""}, {message, "muted", ""}}, nil)
		rows = append(rows, "", row)
		for len(rows) < n+1 {
			rows = append(rows, "")
		}
		return rows
	}
	header, _ := formLine(width, nil, columns("context", "in /1M", "out /1M", "muted"))
	if !showContext && !showPrices {
		header = ""
	}
	rows = append(rows, header)
	f.listTop = max(0, min(f.listTop, len(f.suggestions)-n))
	if f.selected >= 0 {
		f.listTop = min(max(f.listTop, f.selected-n+1), f.selected)
	}
	nameW := inner - formCaretW - spansWidth(columns("", "", "", "")) - formNoteGap
	for i := f.listTop; i < f.listTop+n; i++ {
		if i >= len(f.suggestions) {
			rows = append(rows, "")
			continue
		}
		name := f.suggestions[i]
		caret, label := formSpan{"  ", "", ""}, formSpan{rw.Truncate(metadataDisplayText(name, false), max(1, nameW), "…"), "", ""}
		// The chosen model stays marked while Key and Context have focus,
		// but the caret goes with focus.
		if i == f.selected && f.focus == formFieldModel {
			caret = formSpan{"› ", "accent", "bold"}
			label.role, label.mod = "accent", "bold"
		} else if i == f.selected {
			label.role = "accent"
		}
		row, _ := formLine(width, []formSpan{caret, label}, columns(known[i].context, known[i].in, known[i].out, ""))
		rows = append(rows, row)
		f.hit(hitPick, i, 0, y+len(rows)-1, width)
	}
	return rows
}

// suggestionInfo is the catalog entry behind a listed name, and the host a
// "model:host" route pins.
func (f *modelForm) suggestionInfo(name string) (llm.ModelInfo, string, bool) {
	if info, ok := f.infos[name]; ok {
		return info, "", true
	}
	if i := strings.LastIndex(name, ":"); i > 0 {
		if info, ok := f.infos[name[:i]]; ok {
			return info, name[i+1:], true
		}
	}
	return llm.ModelInfo{}, "", false
}

// footerRow folds Key and Context into one line while the list has focus.
func (f *modelForm) footerRow(width, y int) string {
	label, key, keyNote := f.keyField(false, width)
	context, contextNote := f.contextField(false, width)
	spans := []formSpan{{"  ", "", ""}, {label + " ", "muted", ""}}
	spans = append(spans, key...)
	if f.login && !f.signedIn {
		spans = append(spans, formSpan{" " + keyNote[0].text, "muted", ""})
	}
	spans = append(spans, formSpan{" · Context ", "muted", ""})
	spans = append(spans, context...)
	if len(contextNote) > 0 {
		spans = append(spans, formSpan{" " + contextNote[0].text, "muted", ""})
	}
	row, _ := formLine(width, spans, nil)
	f.hit(hitField, formFieldKey, 0, y, width)
	return row
}
