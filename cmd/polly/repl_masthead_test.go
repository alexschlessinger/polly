package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	rw "github.com/mattn/go-runewidth"
)

func mastheadTestModel() *replModel {
	m := newReplModel()
	m.status.contextName = "spec-demo"
	m.status.modelName = "openai/gpt-5.4"
	m.masthead = mastheadState{enabled: true, sandbox: "Sandbox active · workspace, net, git"}
	return m
}

func hasBlockRunes(s string) bool {
	return strings.ContainsAny(s, "▀▄█")
}

func TestPollyBirdRowsUseThePaletteAtNineColumns(t *testing.T) {
	rows := pollyBirdRows()
	if len(rows) != 4 {
		t.Fatalf("bird has %d rows, want 4", len(rows))
	}
	joined := strings.Join(rows, "\n")
	for _, row := range rows {
		if w := rw.StringWidth(plainStyledText(row)); w != pollyBirdWidth {
			t.Fatalf("bird row %q is %d cells wide, want %d", plainStyledText(row), w, pollyBirdWidth)
		}
	}
	for _, name := range []string{"polly-green", "polly-wing", "polly-crown", "polly-beak", "polly-face", "polly-foot"} {
		if !strings.Contains(joined, "fg:"+name) {
			t.Fatalf("bird does not paint %s", name)
		}
	}
	if !strings.Contains(joined, "bg:polly-") {
		t.Fatal("bird has no split half-block cell")
	}
}

// With room and no native graphics the masthead is the bird with the
// sandbox posture and invitation beside it; narrow terminals get
// text alone, and native graphics use an equally compact embedded bird.
func TestMastheadLayoutByWidth(t *testing.T) {
	m := mastheadTestModel()
	for _, width := range []int{120, 80, 60} {
		rows := rowsText(m.transcriptRows(width))
		if got := m.mastheadRowCount(width); got != 5 || len(rows) != 4 {
			t.Fatalf("width %d: masthead rows = %d, transcript rows = %d, want 5 and 4", width, got, len(rows))
		}
		if !hasBlockRunes(rows[0]) || !hasBlockRunes(rows[3]) {
			t.Fatalf("width %d: bird missing from rows %q", width, rows)
		}
		for i, want := range []string{"polly build " + pollyBuildRevision(), "Sandbox active · workspace, net, git", mastheadInvitation} {
			if row := rows[i]; strings.Index(row, want) < 0 || rw.StringWidth(row[:strings.Index(row, want)]) != mastheadTextCol {
				t.Fatalf("width %d: row %d = %q, want %q at column %d", width, i+1, row, want, mastheadTextCol)
			}
		}
	}
	for _, width := range []int{29, 20, 1} {
		rows := rowsText(m.transcriptRows(width))
		if hasBlockRunes(strings.Join(rows, "\n")) {
			t.Fatalf("width %d: bird drawn without room: %q", width, rows)
		}
		if width >= len("polly") && !strings.HasPrefix(rows[0], "polly") {
			t.Fatalf("width %d: first row = %q", width, rows[0])
		}
	}
	if rows := rowsText(m.transcriptRows(29)); rows[1] != "Sandbox active · workspace, …" || rows[2] != "Type a message, or / for com…" {
		t.Fatalf("width 29 rows = %q", rows)
	}
	// Posture and invitation clip on narrow terminals.
	if rows := rowsText(m.transcriptRows(20)); rows[1] != "Sandbox active · wo…" || rows[2] != "Type a message, or …" {
		t.Fatalf("width 20 rows = %q", rows)
	}
	if rows := rowsText(m.transcriptRows(8)); rows[1] != "Sandbox…" {
		t.Fatalf("width 8 rows = %q", rows)
	}

	m.nativeImages = true
	rows := rowsText(m.transcriptRows(80))
	if hasBlockRunes(strings.Join(rows, "\n")) {
		t.Fatalf("image-capable terminal drew half-block art: %q", rows)
	}
	if len(m.visual.blocks) == 0 || m.visual.blocks[0].key != "masthead" || len(m.visual.blocks[0].images) != 1 {
		t.Fatalf("image masthead block = %#v", m.visual.blocks)
	}
	if got := m.mastheadRowCount(80); got != termimg.LogoArtRows+1 {
		t.Fatalf("image masthead rows = %d, want %d", got, termimg.LogoArtRows+1)
	}
	// The marker slot rows stay blank in the text layer; the manager paints
	// the image, and the identity text is indented to its right.
	logo := termimg.LogoImage()
	_, maxRows := style.ImageBounds(logo)
	cols, slotRows, _ := termimg.CellGeometry(logo, 80, maxRows, m.imageCellWidth, m.imageCellHeight)
	if slotRows != len(rows) {
		t.Fatalf("image masthead reserved %d rows, want %d", len(rows), slotRows)
	}
	textCol := cols + 2
	textStart := (slotRows - 3) / 2
	for i, want := range []string{"polly build " + pollyBuildRevision(), "Sandbox active · workspace, net, git", mastheadInvitation} {
		row := rows[textStart+i]
		at := strings.Index(row, want)
		if at < 0 || rw.StringWidth(row[:at]) != textCol {
			t.Fatalf("image masthead row %d = %q, want %q at column %d", textStart+i, row, want, textCol)
		}
	}
	// The logo rides the thumbnail pipeline: the slot projects as an embedded
	// placement with no backing file, at the left edge of the block.
	viewport := (frameLayout{width: 80, transcriptHeight: slotRows + 1}).transcriptViewport(slotRows+1, 0, false, 0)
	placements := m.visibleImagePlacements(viewport)
	if len(placements) != 1 {
		t.Fatalf("image masthead placements = %#v", placements)
	}
	if p := placements[0]; p.Key != "masthead:image:0" || p.Embedded != "logo" || p.Path != "" || p.X != 0 || p.Cols != cols {
		t.Fatalf("image masthead placement = %#v, want embedded logo at column 0, %d cols", p, cols)
	}
	m.nativeImages = false
	m.quiet = true
	if got := m.mastheadRowCount(80); got != 0 {
		t.Fatalf("quiet masthead rows = %d", got)
	}
}

func TestMastheadInvitationLeavesWithTheFirstPrompt(t *testing.T) {
	m := mastheadTestModel()
	if rows := rowsText(m.transcriptRows(29)); len(rows) != 3 {
		t.Fatalf("rows before the first prompt = %q", rows)
	}
	m.appendUserPrompt("hello")
	rows := rowsText(m.transcriptRows(29))
	if strings.Contains(strings.Join(rows, "\n"), mastheadInvitation) {
		t.Fatalf("invitation stayed after the first prompt: %q", rows)
	}
	// Version, sandbox, the blank row the masthead owns, then the prompt.
	if len(rows) != 4 || rows[2] != "" || rows[3] != "▎ hello" {
		t.Fatalf("rows after the first prompt = %q", rows)
	}
	if got := m.entryVisualStart(0, 29); got != m.mastheadRowCount(29) || got != 3 {
		t.Fatalf("entry 0 starts at row %d, masthead rows %d", got, m.mastheadRowCount(29))
	}
	if got := m.mastheadRowCount(80); got != 5 {
		t.Fatalf("bird masthead rows after the first prompt = %d", got)
	}
}

func TestMastheadOmitsIdentityAndLeavesOnClear(t *testing.T) {
	m := mastheadTestModel()
	before := strings.Join(rowsText(m.transcriptRows(80)), "\n")
	if strings.Contains(before, "spec-demo") || strings.Contains(before, "gpt-5.4") {
		t.Fatalf("masthead repeats status identity: %q", before)
	}
	m.setContextName("renamed")
	m.setModelName("anthropic/claude-sonnet-5")
	if after := strings.Join(rowsText(m.transcriptRows(80)), "\n"); after != before {
		t.Fatalf("identity change altered masthead: %q", after)
	}

	m.appendUserPrompt("hello")
	m.appendLine("reply")
	m.clearDisplay()
	rows := rowsText(m.transcriptRows(80))
	if len(m.visual.blocks) != 1 || m.visual.blocks[0].key != "masthead" || !hasBlockRunes(rows[0]) {
		t.Fatalf("masthead did not survive a history reset: blocks=%d rows=%q", len(m.visual.blocks), rows)
	}
	m.appendLine("reply")
	m.clearScreen()
	if rows := rowsText(m.transcriptRows(80)); len(rows) != 0 || m.mastheadRowCount(80) != 0 {
		t.Fatalf("/clear kept the masthead: rows=%q", rows)
	}
}

func TestMastheadStaysOutOfInspectorSnapshots(t *testing.T) {
	m := mastheadTestModel()
	m.appendUserPrompt("hello")
	m.transcriptRows(80)
	m.mu.Lock()
	snapshot := childDisplayCopy(m)
	m.mu.Unlock()
	if snapshot.masthead.enabled || snapshot.mastheadRowCount(80) != 0 {
		t.Fatal("snapshot copied the masthead")
	}
	if rows := rowsText(snapshot.transcriptRows(80)); len(rows) != 1 || rows[0] != "▎ hello" {
		t.Fatalf("snapshot rows = %q, want the prompt alone", rows)
	}
}

func TestMastheadIsOffUntilTheTUIRuns(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	if r.model.masthead.enabled || r.model.mastheadRowCount(80) != 0 {
		t.Fatal("a unit-test REPL grew a masthead")
	}
	if got := rowsText(r.model.transcriptRows(80)); len(got) != 0 {
		t.Fatalf("empty transcript rows = %q", got)
	}
}
