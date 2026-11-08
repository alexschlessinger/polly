package main

import (
	"image"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

func TestLatestAgentMessages(t *testing.T) {
	posted := time.Unix(100, 0)
	s := &swarm.State{
		Members: map[string]*swarm.Member{
			"reviewer": {ID: "reviewer", Name: "review-session", Label: "Reviewer"},
			"builder":  {ID: "builder", Name: "build-session", Label: "Builder"},
			"quiet":    {ID: "quiet", Name: "Quiet"},
		},
		Messages: map[string]*swarm.Mail{
			"old":  {ID: "old", From: "root-id", To: "reviewer", Text: "original brief", Posted: posted},
			"peer": {ID: "peer", From: "reviewer", To: "builder", Text: "check the parser", Posted: posted.Add(time.Second), Delivered: true},
			"new":  {ID: "new", From: "builder", To: "root-id", Text: "ready for review", Posted: posted.Add(2 * time.Second)},
			"else": {ID: "else", From: "outside", To: "root-id", Text: "unrelated", Posted: posted.Add(3 * time.Second)},
		},
	}
	previews := latestAgentMessages(s, "root-id")
	if len(previews) != 2 {
		t.Fatalf("previews include quiet/nonmembers: %+v", previews)
	}
	if got := *previews["reviewer"]; got != (agentMessagePreview{peer: "Builder", text: "check the parser", outgoing: true}) {
		t.Fatalf("latest sent peer message: %+v", got)
	}
	if got := *previews["builder"]; got != (agentMessagePreview{peer: "parent", text: "ready for review", outgoing: true, queued: true}) {
		t.Fatalf("latest outgoing message: %+v", got)
	}
	// Equal posting times use the same deterministic ID order as the inbox.
	s.Messages["tie"] = &swarm.Mail{ID: "tie", From: "root-id", To: "builder", Text: "check cancellation", Posted: posted.Add(2 * time.Second)}
	for range 20 {
		if got := *latestAgentMessages(s, "root-id")["builder"]; got != (agentMessagePreview{peer: "parent", text: "check cancellation", queued: true}) {
			t.Fatalf("latest incoming message/tie order: %+v", got)
		}
	}
	// Merely displaying the queue never admits messages. Delivery of older
	// mail must not displace the latest posted message either.
	if s.Messages["tie"].Delivered || s.Messages["new"].Delivered {
		t.Fatal("preview consumed queued mail")
	}
	s.Messages["old"].Delivered = true
	if got := latestAgentMessages(s, "root-id")["reviewer"].text; got != "check the parser" {
		t.Fatalf("older delivery displaced latest message: %q", got)
	}
	s.Messages["tie"].Delivered = true
	s.Members["builder"].Label = "Renamed builder"
	previews = latestAgentMessages(s, "root-id")
	if previews["builder"].queued || previews["reviewer"].peer != "Renamed builder" {
		t.Fatalf("delivery/peer rename not refreshed: %+v / %+v", previews["builder"], previews["reviewer"])
	}
	// The same peer-to-peer message has opposite directions in the two rows.
	s.Messages["exchange"] = &swarm.Mail{ID: "exchange", From: "builder", To: "reviewer", Text: "Parser checked", Posted: posted.Add(4 * time.Second), Delivered: true}
	previews = latestAgentMessages(s, "root-id")
	for id, want := range map[string]string{
		"reviewer": "    from Renamed builder: Parser checked",
		"builder":  "    to Reviewer: Parser checked",
	} {
		if got := plainStyledText(previews[id].row(80)); got != want {
			t.Fatalf("%s preview: %q, want %q", id, got, want)
		}
	}
}

func TestAgentMessagePreviewTextAndNames(t *testing.T) {
	s := &swarm.State{
		Members:  map[string]*swarm.Member{"worker": {ID: "worker", Name: "Worker\nname"}},
		Messages: map[string]*swarm.Mail{"m": {ID: "m", From: "unknown-id", To: "worker", Text: "Check\t[parser](fg:err)\n\x00界   please"}},
	}
	p := latestAgentMessages(s, "root")["worker"]
	if p.peer != "unknown-id" || p.outgoing || p.text != "Check [parser](fg:err) 界 please" {
		t.Fatalf("name fallback or whitespace: %+v", p)
	}
	cells := style.VisualRows(p.row(80), ui.StyleClear, 80)
	if len(cells) != 1 || strings.TrimSpace(plainCells(cells[0])) != "from unknown-id: "+p.text+" · queued" {
		t.Fatalf("message markup not literal: %v", cells)
	}
	for _, cell := range cells[0] {
		if cell.Rune != ' ' && cell.Style.Fg != chromeColor("muted") {
			t.Fatal("message text injected a style")
		}
	}
	for _, text := range []string{strings.Repeat("界", agentMessageLimit), strings.Repeat("界", agentMessageLimit+1)} {
		s.Messages["m"].Text = text
		p = latestAgentMessages(s, "root")["worker"]
		if !utf8.ValidString(p.text) || utf8.RuneCountInString(p.text) != agentMessageLimit {
			t.Fatalf("Unicode character limit: %q", p.text)
		}
		if strings.HasSuffix(p.text, "…") != (utf8.RuneCountInString(text) > agentMessageLimit) {
			t.Fatalf("ellipsis at character limit: %q", p.text)
		}
	}
}

func TestAgentMessagePreviewRowFits(t *testing.T) {
	for _, outgoing := range []bool{false, true} {
		for _, queued := range []bool{false, true} {
			p := &agentMessagePreview{peer: strings.Repeat("界", 40), text: strings.Repeat("語", agentMessageLimit), outgoing: outgoing, queued: queued}
			direction := "from "
			if outgoing {
				direction = "to "
			}
			for _, width := range []int{0, 8, 18, 24, 36, 80, 400} {
				row := p.row(width)
				if width >= 24 && row == "" {
					t.Fatalf("width %d: preview missing", width)
				}
				if row == "" {
					continue
				}
				cells := style.VisualRows(row, ui.StyleClear, width)
				if len(cells) != 1 || style.CellsWidth(cells[0]) > width {
					t.Fatalf("width %d: row wrapped/overflowed: %q", width, row)
				}
				text := plainCells(cells[0])
				if !strings.HasPrefix(text, agentDetailIndent+direction) || !strings.Contains(text, ": ") || strings.Contains(text, "queued") != queued {
					t.Fatalf("width %d: direction or queued marker lost: %q", width, text)
				}
				if width >= 36 && (!strings.Contains(text, "界") || !strings.Contains(text, "語")) {
					t.Fatalf("width %d: peer or text crowded out: %q", width, text)
				}
			}
		}
	}
	var absent *agentMessagePreview
	if row := absent.row(80); row != "" {
		t.Fatalf("absent message takes space: %q", row)
	}
}

func TestAgentsInspectorMessageRowsRefreshAndNavigate(t *testing.T) {
	r, ids := agentsInspectorFixture(t)
	s := r.visibleTab().swarmSnapshot
	mail := &swarm.Mail{ID: "mail", From: r.visibleTab().viewID(), To: ids[0], Text: "Check cancellation\nhandling"}
	s.Messages = map[string]*swarm.Mail{mail.ID: mail}
	r.openAgentsInspector()
	r.refreshAgentsInspector(viewGeometry{width: 64})
	state := r.workspace().viewState(r.workspace().inspector.target)
	// At full depth the latest call and the message preview take their own
	// rows under the agent's row; the turn's counts stay in the transcript.
	state.agents.full = true
	call := activityField("✓ $ go test ./...", activityTools, false)
	call.latest = true
	state.agents.progress[ids[0]] = &agentProgress{agentProgressSummary: agentProgressSummary{fields: []turnDockField{{raw: "4 tool calls"}, call}}}
	r.refreshAgentsInspector(viewGeometry{width: 64})
	v := r.workspace().inspector.current
	open := "agents-open:" + ids[0]
	first := slices.Index(v.agentsActions, open)
	if first < 0 || !slices.Equal(v.agentsActions[first:first+3], []string{open, open, open}) || v.agentsActions[first+3] == open {
		t.Fatalf("message/activity hit targets: %v", v.agentsActions)
	}
	order := agentActionOrder(v.agentsActions)
	rows := v.view.Rows(v.model, 64)
	if len(rows) != len(v.agentsActions) {
		t.Fatalf("rendered rows/hit targets: %d/%d", len(rows), len(v.agentsActions))
	}
	for n, text := range []string{"✓ $ go test ./...", "from parent: Check cancellation handling · queued"} {
		if got := strings.TrimSpace(plainCells(rows[first+1+n])); got != text {
			t.Fatalf("row %d: %q, want %q", first+1+n, got, text)
		}
	}
	if mail.Delivered {
		t.Fatal("inspector admitted queued mail")
	}
	before := v.revision
	mail.Delivered = true
	r.refreshAgentsInspector(viewGeometry{width: 64})
	v = r.workspace().inspector.current
	if v.revision == before || strings.Contains(inspectorText(v), "queued") {
		t.Fatal("delivery change did not repaint preview")
	}
	s.Messages["new"] = &swarm.Mail{ID: "new", From: ids[0], To: r.visibleTab().viewID(), Text: "Cancellation checked", Posted: time.Now(), Delivered: true}
	r.refreshAgentsInspector(viewGeometry{width: 64})
	v = r.workspace().inspector.current
	if text := inspectorText(v); !strings.Contains(text, "to parent: Cancellation checked") || strings.Contains(text, "Check cancellation handling") {
		t.Fatalf("new outgoing message did not replace incoming preview: %s", text)
	}
	if !slices.Equal(agentActionOrder(v.agentsActions), order) {
		t.Fatal("new message changed agent order")
	}
	// Preview rows are part of one keyboard selection, not extra agents.
	r.chrome.inner = image.Rect(0, 0, 64, 8)
	r.inspectorHeaderRows = 1
	r.navigateAgentsInspector("<Home>")
	r.navigateAgentsInspector("<Down>")
	if state.agents.selected != ids[1] {
		t.Fatal("Down selected another line of the same agent")
	}
	r.navigateAgentsInspector("<Up>")
	if state.agents.selected != ids[0] {
		t.Fatal("Up skipped the agent with message rows")
	}
	if !r.agentsInspectorAction(v.agentsActions[first+2]) || r.workspace().inspector.target.session.ID != ids[0] {
		t.Fatal("message text did not open its agent")
	}
}
