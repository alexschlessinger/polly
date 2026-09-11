package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestBashInspectorSeparatesOnlyLeadingSetup(t *testing.T) {
	for _, tc := range []struct {
		command string
		fold    bool
		vars    int
	}{
		{`cd /tmp && export GOCACHE=$TMPDIR/gocache && go test ./swarm -count=1 2>&1 | tail -8`, true, 1},
		{`export FOO="a b" BAR= && echo "$FOO"`, true, 2},
		{`cd "a b" && cd sub && go test; echo done`, true, 0},
		{`cd /tmp && echo ok && cd elsewhere && echo done`, true, 0},
		{`cd /tmp && (echo a; echo b)`, true, 0},
		{`cd /tmp && cat <<'EOF'` + "\nhello && goodbye\nEOF", true, 0},
		{`cd /tmp`, false, 0},
		{`export FOO=bar`, false, 0},
		{`cd /tmp || echo failed`, false, 0},
		{`cd /tmp && echo ok || echo failed`, false, 0},
		{`cd /tmp && echo ok &`, false, 0},
		{`{ cd /tmp && echo ok; }`, false, 0},
		{`! cd /tmp && echo ok`, false, 0},
		{`cd /tmp 2>/dev/null && echo ok`, false, 0},
		{`cd - && echo ok`, false, 0},
		{`export -n FOO && echo ok`, false, 0},
		{`export FOO && echo ok`, false, 0},
		{`export FOO=$(build) && echo ok`, false, 0},
		{`export FOO=${BAR:=changed} && echo ok`, false, 0},
		{`cd "$(mktemp -d)" && echo ok`, false, 0},
		{`echo setup && cd /tmp && echo ok`, false, 0},
	} {
		t.Run(tc.command, func(t *testing.T) {
			b := newBashInspectorCommand(tc.command)
			if (b.setup != "") != tc.fold || b.variables != tc.vars {
				t.Fatalf("setup = %q, variables = %d", b.setup, b.variables)
			}
			// Rejoin the disclosed setup and formatted command. Every shell
			// operator, redirection, quoted value and scope must still agree.
			lines := strings.Split(plainStyledText(b.formatted), "\n")[1:]
			for i := range lines {
				lines[i] = strings.TrimPrefix(lines[i], "│ ")
			}
			joined := b.setup + "\n" + strings.Join(lines, "\n")
			if canonicalBash(t, joined) != canonicalBash(t, tc.command) {
				t.Fatalf("setup split changed shell structure: %s", joined)
			}
		})
	}
	broken := newBashInspectorCommand(`cd /tmp && echo "unterminated`)
	if broken.setup != "" {
		t.Fatal("invalid shell was treated as setup")
	}
}

func TestBashInspectorSetupClickResizeAndReopen(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.setupWidgets()
	r.showTab(0)
	command := `cd /Users/alex/.pollytool/worktrees/56fb08bea51eafe6fc1f07e792ca978a/slot-0002/tree && export GOCACHE=$TMPDIR/gocache && go test ./swarm -count=1 2>&1 | tail -8`
	args, _ := json.Marshal(map[string]string{"command": command})
	call := messages.ChatMessageToolCall{ID: "setup", Name: "bash", Arguments: string(args)}
	r.model.appendToolCallStart(call)
	result := "ok  \tgithub.com/alexschlessinger/pollytool/swarm\t80.696s"
	r.model.inspections.setResult(call, messages.ChatMessage{Content: result})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 200)
	rowsText := func(width int) string {
		var lines []string
		for _, row := range v.view.Rows(v.model, width) {
			if style.CellsWidth(row) > width {
				t.Fatalf("width %d overflow: %q", width, plainCells(row))
			}
			lines = append(lines, plainCells(row))
		}
		return strings.Join(lines, "\n")
	}
	for _, width := range []int{32, 50, 80, 140, 50} {
		text := rowsText(width)
		if !strings.HasPrefix(text, "▸ setup") || strings.Contains(text, "/Users/") || strings.Contains(text, "export ") {
			t.Fatalf("setup should start collapsed: %s", text)
		}
		if !strings.Contains(text, "go test ./swarm") || !strings.Contains(text, "tail -8") || !strings.Contains(text, "2>&1") {
			t.Fatalf("lost main command: %s", text)
		}
		if width >= 50 && !strings.Contains(text, "│ go test ./swarm -count=1 2>&1 | tail -8\n") {
			t.Fatalf("short pipeline should fit one row: %s", text)
		}
		if strings.Contains(text, "swarm80.696s") || strings.ContainsRune(text, '\t') {
			t.Fatalf("output tabs lost their spacing: %s", text)
		}
	}
	screen.SetSize(200, 38)
	r.render()
	button := headerButton(r.inspectorButtons, "bash-setup")
	if button.Empty() {
		t.Fatal("missing clickable setup disclosure")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", button.Min))
	r.render()
	if !v.model.bashSetupExpanded || !strings.Contains(rowsText(160), "/Users/alex/.pollytool/worktrees/") || !strings.Contains(rowsText(160), "export GOCACHE=$TMPDIR/gocache &&") {
		t.Fatalf("click did not reveal full setup: %s", rowsText(160))
	}
	if len(r.inspectorW.OverlayBottom) != 0 {
		t.Fatal("opening setup was mistaken for new output")
	}
	for _, width := range []int{140, 80, 200} {
		screen.SetSize(width, 38)
		v = waitInspector(t, r, width)
		r.render()
		if !v.model.bashSetupExpanded || headerButton(r.inspectorButtons, "bash-setup").Empty() {
			t.Fatal("resize lost setup expansion or its click target")
		}
	}
	r.closeInspector()
	r.inspectCommand("tools")
	v = waitInspector(t, r, 200)
	if !v.model.bashSetupExpanded {
		t.Fatal("reopening lost setup expansion")
	}
	r.render()
	button = headerButton(r.inspectorButtons, "bash-setup")
	r.handleEvent(mouseEvent("<MouseLeft>", button.Min))
	r.render()
	if v.model.bashSetupExpanded || strings.Contains(rowsText(80), "export ") {
		t.Fatal("second click did not collapse setup")
	}
	if got := r.model.inspections.toolForCall(call.ID).call.Arguments; got != call.Arguments {
		t.Fatal("display changed original call")
	}
	if got := r.model.inspections.toolForCall(call.ID).result.Content; got != result {
		t.Fatal("display changed original output")
	}
}
