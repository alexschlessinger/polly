package main

import (
	"strings"
	"testing"

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

func TestPollyBirdRowsUseThePaletteAtThirteenColumns(t *testing.T) {
	rows := pollyBirdRows()
	if len(rows) != 6 {
		t.Fatalf("bird has %d rows, want 6", len(rows))
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

// With room and no native graphics the masthead is the bird with the title,
// the sandbox posture, and the invitation beside it; narrower terminals and
// image-capable ones get the text rows alone.
func TestMastheadLayoutByWidth(t *testing.T) {
	m := mastheadTestModel()
	for _, width := range []int{120, 80, 60} {
		rows := rowsText(m.transcriptRows(width))
		if got := m.mastheadRowCount(width); got != 7 || len(rows) != 6 {
			t.Fatalf("width %d: masthead rows = %d, transcript rows = %d, want 7 and 6", width, got, len(rows))
		}
		if !hasBlockRunes(rows[0]) || !hasBlockRunes(rows[5]) {
			t.Fatalf("width %d: bird missing from rows %q", width, rows)
		}
		for i, want := range []string{"polly · spec-demo · gpt-5.4", "Sandbox active · workspace, net, git", mastheadInvitation} {
			if row := rows[i+1]; strings.Index(row, want) < 0 || rw.StringWidth(row[:strings.Index(row, want)]) != mastheadTextCol {
				t.Fatalf("width %d: row %d = %q, want %q at column %d", width, i+1, row, want, mastheadTextCol)
			}
		}
	}
	for _, width := range []int{59, 40, 20, 1} {
		rows := rowsText(m.transcriptRows(width))
		if hasBlockRunes(strings.Join(rows, "\n")) {
			t.Fatalf("width %d: bird drawn without room: %q", width, rows)
		}
		if width >= len("polly") && !strings.HasPrefix(rows[0], "polly") {
			t.Fatalf("width %d: first row = %q", width, rows[0])
		}
	}
	if rows := rowsText(m.transcriptRows(40)); rows[0] != "polly · spec-demo · gpt-5.4" || rows[1] != "Sandbox active · workspace, net, git" || rows[2] != mastheadInvitation {
		t.Fatalf("width 40 rows = %q", rows)
	}
	// The title drops its model, then its session; muted rows clip instead.
	if rows := rowsText(m.transcriptRows(20)); rows[0] != "polly · spec-demo" || rows[1] != "Sandbox active · wo…" || rows[2] != "Type a message, or …" {
		t.Fatalf("width 20 rows = %q", rows)
	}
	if rows := rowsText(m.transcriptRows(8)); rows[0] != "polly" || rows[1] != "Sandbox…" {
		t.Fatalf("width 8 rows = %q", rows)
	}

	m.nativeImages = true
	if rows := rowsText(m.transcriptRows(80)); hasBlockRunes(strings.Join(rows, "\n")) || rows[0] != "polly · spec-demo · gpt-5.4" {
		t.Fatalf("image-capable terminal rows = %q, want text only", rows)
	}
	m.nativeImages = false
	m.quiet = true
	if got := m.mastheadRowCount(80); got != 0 {
		t.Fatalf("quiet masthead rows = %d", got)
	}
}

func TestMastheadInvitationLeavesWithTheFirstPrompt(t *testing.T) {
	m := mastheadTestModel()
	if rows := rowsText(m.transcriptRows(39)); len(rows) != 3 {
		t.Fatalf("rows before the first prompt = %q", rows)
	}
	m.appendUserPrompt("hello")
	rows := rowsText(m.transcriptRows(39))
	if strings.Contains(strings.Join(rows, "\n"), mastheadInvitation) {
		t.Fatalf("invitation stayed after the first prompt: %q", rows)
	}
	// Title, sandbox, the blank row the masthead owns, then the prompt.
	if len(rows) != 4 || rows[2] != "" || rows[3] != "▎ hello" {
		t.Fatalf("rows after the first prompt = %q", rows)
	}
	if got := m.entryVisualStart(0, 39); got != m.mastheadRowCount(39) || got != 3 {
		t.Fatalf("entry 0 starts at row %d, masthead rows %d", got, m.mastheadRowCount(39))
	}
	if got := m.mastheadRowCount(80); got != 7 {
		t.Fatalf("bird masthead rows after the first prompt = %d", got)
	}
}

func TestMastheadFollowsIdentityAndSurvivesClear(t *testing.T) {
	m := mastheadTestModel()
	m.transcriptRows(80)
	m.setContextName("renamed")
	if m.visual.valid {
		t.Fatal("renaming did not invalidate the visual cache")
	}
	if rows := rowsText(m.transcriptRows(80)); !strings.Contains(rows[1], "polly · renamed · gpt-5.4") {
		t.Fatalf("renamed masthead rows = %q", rows)
	}
	m.setModelName("anthropic/claude-sonnet-5")
	if rows := rowsText(m.transcriptRows(80)); !strings.Contains(rows[1], "polly · renamed · claude-sonnet-5") {
		t.Fatalf("model change masthead rows = %q", rows)
	}
	m.setModelName("anthropic/claude-sonnet-5")
	if !m.visual.valid {
		t.Fatal("an unchanged model invalidated the cache")
	}

	m.appendUserPrompt("hello")
	m.appendLine("reply")
	m.clearDisplay()
	rows := rowsText(m.transcriptRows(80))
	if len(m.visual.blocks) != 1 || m.visual.blocks[0].key != "masthead" || !hasBlockRunes(rows[0]) {
		t.Fatalf("masthead did not survive /clear: blocks=%d rows=%q", len(m.visual.blocks), rows)
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
