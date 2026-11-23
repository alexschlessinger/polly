package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	ui "github.com/metaspartan/gotui/v5"
)

func agentCall(id, args string) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{ID: id, Name: subagent.ToolName, Arguments: args}
}

// agentDetail renders the agents of a set of records in isolation from chunks.
func (m *replModel) agentDetail(ids []int64, width int, prefix string) (string, []agentLink) {
	var rows []agentRowRef
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil {
			for i := range record.rows {
				if record.rows[i].agent != nil {
					rows = append(rows, agentRowRef{record, i})
				}
			}
		}
	}
	return m.agentRowsDetail(rows, width, prefix)
}

func activityBlocks(m *replModel, width int) []transcriptDisplayBlock {
	// Match the paint boundary before inspecting the laid-out activity.
	m.refreshReasoningRecords(width)
	var out []transcriptDisplayBlock
	for _, b := range m.transcriptDisplayEntries(width) {
		if b.isActivity() {
			out = append(out, b)
		}
	}
	return out
}

func TestAgentsMixedGroupingAndIndependentDisclosures(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("compare the work")
	calls := []messages.ChatMessageToolCall{
		agentCall("a", `{"label":"Trace sessions"}`),
		{ID: "b", Name: "read_file"},
		agentCall("c", `{"task":"Review picker"}`),
		{ID: "d", Name: "mcp__spawn_agent"},
		{ID: "e", Name: "view_image"},
	}
	tui.AppendToolStart(calls)
	for _, c := range calls {
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	path := filepath.Join(t.TempDir(), "frame.png")
	writeImageFixture(t, path, 8, 4)
	tui.AppendToolMedia(calls[4], inspectionTranscriptImages(testToolImageResult(t, path, calls[4].ID), nil))
	blocks := activityBlocks(m, 120)
	if len(blocks) != 1 {
		t.Fatalf("adjacent activity split: %+v", blocks)
	}
	// One parallel batch is one chunk: the launches lead its header, and
	// what was asked beside them is counted after.
	const folded = "▸ 2 agents, read a file, viewed frame.png and used a tool"
	if header := plainStyledText(blocks[0].text); header != folded {
		t.Fatalf("folded header = %q", header)
	}
	ids := blocks[0].toolDisclosureIDs
	if m.turnToolCallCount() != 5 || m.runningTools != 0 {
		t.Fatalf("execution counters changed: %d / %d", m.turnToolCallCount(), m.runningTools)
	}
	m.toggleDisclosureGroup(activityTools, ids, 0)
	record := m.toolDisclosures.get(ids[0])
	if !m.lineOpenOf(record) {
		t.Fatal("the chunk did not open")
	}
	tools := toolRecordText(record, 0, true)
	if strings.Contains(plainStyledText(tools), "Trace sessions") || !strings.Contains(plainStyledText(tools), "mcp__spawn_agent") {
		t.Fatalf("Tools classification = %q", tools)
	}
	detail, _ := m.agentDetail(ids, 120, "  ")
	if got := plainStyledText(detail); got != "  ✓ Trace sessions  done\n  ✓ Review picker   done" {
		t.Fatalf("agent detail = %q", got)
	}
	// The open chunk lists its thought, its ordinary calls and its agents.
	open := activityText(t, m, 120, 0)
	for _, want := range []string{"▾" + folded[len("▸"):] + "\n", thoughtGlyph + " thought", "mcp__spawn_agent", "✓ Trace sessions  done", "✓ Review picker   done"} {
		if !strings.Contains(open, want) {
			t.Fatalf("open chunk lacks %q:\n%s", want, open)
		}
	}
	// Folding the chunk folds its agents with it.
	m.toggleDisclosureGroup(activityAgents, ids, 0)
	if got := activityText(t, m, 120, 0); m.lineOpenOf(record) || got != folded {
		t.Fatalf("the header did not fold the whole chunk: %q", got)
	}
	tui.AppendAssistantText("I have the first results.")
	tui.AppendToolStart([]messages.ChatMessageToolCall{agentCall("f", `{}`)})
	tui.AppendToolEnd(agentCall("f", `{}`), "ok", time.Millisecond, nil)
	blocks = activityBlocks(m, 120)
	if len(blocks) != 2 || !strings.Contains(plainStyledText(blocks[1].text), "1 agent") || strings.Contains(plainStyledText(blocks[1].text), "tool") {
		t.Fatalf("prose did not separate agent-only batch: %+v", blocks)
	}
	m.toggleDisclosureGroup(activityAgents, ids, 0)
	r.endTurn(nil)
	// Settling leaves each chunk as the user left it, and the turn's time
	// rides on the prompt's row, not on a row of its own.
	if !m.lineOpenOf(record) {
		t.Fatalf("settled expansion = %+v", record)
	}
	if trailer := m.turnTrailers.latest(); trailer != nil {
		t.Fatalf("settled turn with a prompt row left a trailer row: %#v", trailer)
	}
	settled := activityBlocks(m, 120)
	if len(settled) != 2 || !strings.HasPrefix(plainStyledText(settled[0].text), "▾"+folded[len("▸"):]+"\n") || plainStyledText(settled[1].text) != "▸ 1 agent" {
		t.Fatalf("settled launch rows = %q / %q", plainStyledText(settled[0].text), plainStyledText(settled[len(settled)-1].text))
	}
	// Folding again leaves just the header.
	m.toggleDisclosureGroup(activityTools, ids, 0)
	if got := plainStyledText(activityBlocks(m, 120)[0].text); got != folded {
		t.Fatalf("folded launch line = %q", got)
	}
}

// The bulk state applies to chunks with agents and viewed images like any
// other: they open and fold with every other chunk, a later batch follows
// the state, and no press opens an output.
func TestBulkStateAppliesToAgentsAndImages(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ShowThinking("compare the work")
	first := []messages.ChatMessageToolCall{agentCall("a", `{"label":"Trace sessions"}`), {ID: "b", Name: "read_file"}}
	tui.AppendToolStart(first)
	for _, c := range first {
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	record := m.currentToolDisclosure()
	if m.depth != depthCollapsed || record == nil || m.lineOpenOf(record) {
		t.Fatalf("a running turn's chunk did not start folded: depth=%v record=%#v", m.depth, record)
	}
	press := func() { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"}) }
	press()
	if m.depth != depthExpanded || !m.lineOpenOf(record) {
		t.Fatalf("Ctrl-O did not expand the chunk: depth=%v", m.depth)
	}
	// A later batch follows the bulk state, its agents and images with it.
	tui.AppendAssistantText("first results.")
	path := filepath.Join(t.TempDir(), "frame.png")
	writeImageFixture(t, path, 8, 4)
	later := []messages.ChatMessageToolCall{agentCall("c", `{"label":"Review picker"}`), {ID: "d", Name: "view_image"}}
	tui.AppendToolStart(later)
	for _, c := range later {
		tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
	}
	tui.AppendToolMedia(later[1], inspectionTranscriptImages(testToolImageResult(t, path, later[1].ID), nil))
	laterRecord := m.currentToolDisclosure()
	if laterRecord == nil || laterRecord.id == record.id || !m.lineOpenOf(laterRecord) {
		t.Fatalf("later batch did not follow the expanded state: %#v", laterRecord)
	}
	if open := activityBlocks(m, 120)[1]; !strings.Contains(plainStyledText(open.text), "Review picker") || len(open.images) != 1 {
		t.Fatalf("the open chunk lost its agent or its image: %q images=%d", plainStyledText(open.text), len(open.images))
	}
	r.endTurn(nil)
	// Settlement keeps the chunks as they are; collapsing folds them and
	// expanding opens them again, and every output stays closed.
	if !m.lineOpenOf(record) || !m.lineOpenOf(laterRecord) {
		t.Fatal("settlement folded a chunk")
	}
	for _, depth := range []disclosureDepth{depthCollapsed, depthExpanded} {
		press()
		if m.depth != depth {
			t.Fatalf("Ctrl-O state = %v, want %v", m.depth.label(), depth.label())
		}
		for _, batch := range []*toolDisclosureRecord{record, laterRecord} {
			if m.lineOpenOf(batch) != (depth == depthExpanded) {
				t.Fatalf("%v left a chunk wrong: %#v", depth.label(), batch)
			}
			for _, row := range batch.rows {
				if row.outputExpanded {
					t.Fatalf("%v opened an output for %q", depth.label(), row.callID)
				}
			}
		}
		if folded := activityBlocks(m, 120)[1]; depth == depthCollapsed && len(folded.images) != 0 {
			t.Fatalf("a folded chunk kept its images: %d", len(folded.images))
		}
	}
}

func TestAgentLaunchFailuresAndFallbacks(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name, args, result, label, status string
		err                               error
	}{
		{"denied", `{"task":"inspect"}`, llm.ToolDeniedContent, "inspect", "denied", nil},
		{"failed", `{}`, "", "agent", "failed", errors.New("launch failed")},
		{"canceled", `{"label":" [literal] "}`, "", "[literal]", "canceled", context.Canceled},
		{"background", `{"task":"look","background":true}`, "started", "look", "unknown", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("go")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			call := agentCall("a", tc.args)
			tui.AppendToolStart([]messages.ChatMessageToolCall{call})
			tui.AppendToolEnd(call, tc.result, time.Millisecond, tc.err)
			record, row := m.toolDisclosureRowForCall("a")
			if row.agent.label != tc.label || row.agent.display() != tc.status || row.agent.busy() {
				t.Fatalf("row = %+v", row.agent)
			}
			m.toggleDisclosureGroup(activityAgents, []int64{record.id}, 0)
			text := plainStyledText(activityBlocks(m, 80)[0].text)
			if strings.Contains(text, "tool") || !strings.Contains(text, tc.label+"  "+tc.status) {
				t.Fatalf("agent-only activity = %q", text)
			}
		})
	}
}

func TestAgentOnlyContinuationInvalidatesRenderedCount(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	first := agentCall("first", `{"label":"first"}`)
	tui.AppendToolStart([]messages.ChatMessageToolCall{first})
	tui.AppendToolEnd(first, "ok", time.Millisecond, nil)
	if got := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n"); !strings.Contains(got, "1 agent") {
		t.Fatalf("first count = %q", got)
	}
	tui.AppendToolStart([]messages.ChatMessageToolCall{agentCall("second", `{"label":"second"}`)})
	if got := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n"); !strings.Contains(got, "1 agent running, 1 completed") {
		t.Fatalf("stale rendered count during launch = %q", got)
	}
}

func TestAgentLinksWrapAndScroll(t *testing.T) {
	withDisplayTTY(t)
	m := newReplModel()
	m.beginTurn("agents")
	for i := 0; i < 8; i++ {
		m.appendToolCallStart(agentCall(fmt.Sprint(i), fmt.Sprintf(`{"label":"task %d [brackets] 界 wrapped label"}`, i)))
		_, row := m.toolDisclosureRowForCall(fmt.Sprint(i))
		row.agent.session = fmt.Sprintf("child-%d", i)
	}
	record := m.currentToolDisclosure()
	m.toggleDisclosureGroup(activityAgents, []int64{record.id}, 0)
	for _, width := range []int{20, 40, 120} {
		rows := m.transcriptRows(width)
		links := m.visibleAgentLinks(fullViewport(len(rows), width))
		seen := map[int]bool{}
		for _, link := range links {
			seen[link.rowIndex] = true
			if link.Y < 0 || link.Y >= len(rows) || link.X < 0 || link.X+link.Cols > width {
				t.Fatalf("invalid link at width %d: %+v", width, link)
			}
			if !strings.Contains(transcriptRowsText(rows)[link.Y], "task") && !strings.Contains(transcriptRowsText(rows)[link.Y], "label") && width == 120 {
				t.Fatalf("link does not cover label: %+v / %q", link, transcriptRowsText(rows)[link.Y])
			}
		}
		if len(seen) != 8 || width == 20 && len(links) <= 8 {
			t.Fatalf("agents truncated or wrapped links missing at %d: %+v", width, links)
		}
	}
	m.reasoningWidth = 40
	m.appendLine("anchor below the agents")
	rows := transcriptRowsText(m.transcriptRows(40))
	m.followBottom = false
	for i, row := range rows {
		if strings.Contains(row, "anchor below") {
			m.scrollAnchor = i
			break
		}
	}
	m.toggleDisclosureGroup(activityAgents, []int64{record.id}, 0)
	rows = transcriptRowsText(m.transcriptRows(40))
	if !strings.Contains(rows[m.scrollAnchor], "anchor below") {
		t.Fatalf("collapse moved scroll anchor to %q", rows[m.scrollAnchor])
	}
}

func TestAgentLabelHitboxesWithToolAndViewedImages(t *testing.T) {
	withDisplayTTY(t)
	for _, native := range []bool{false, true} {
		for _, width := range []int{25, 100} {
			t.Run(fmt.Sprintf("native-%v-width-%d", native, width), func(t *testing.T) {
				r := newManagedREPL(&Config{}, "ctx", 0, 0)
				m := r.model
				m.beginTurn("images and agents")
				m.nativeImages, m.imageCellWidth, m.imageCellHeight = native, 9, 18
				m.reasoningWidth = width
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				path := filepath.Join(t.TempDir(), "frame.png")
				writeImageFixture(t, path, 800, 400)
				call := messages.ChatMessageToolCall{ID: "image", Name: "view_image"}
				m.appendLine("prose above chunks")
				tui.AppendToolStart([]messages.ChatMessageToolCall{call})
				tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
				images := inspectionTranscriptImages(testToolImageResult(t, path, call.ID), nil)
				tui.AppendToolMedia(call, images)
				// A separate agent batch follows the image slots on the same line.
				launch, read := agentCall("a", `{"label":"linked agent task"}`), readCall("target")
				launch.DisplayID = "host-agent"
				tui.AppendToolStart([]messages.ChatMessageToolCall{launch, read})
				tui.AppendToolEnd(read, "ok", time.Millisecond, nil)
				record, _ := m.toolDisclosureRowForCall(call.ID)
				_, a := m.toolDisclosureRowForCall("a")
				a.agent.session = "child"
				a.agent.inputTokens, a.agent.outputTokens = 12000, 2400
				ids := []int64{record.id}
				m.toggleDisclosureGroup(activityTools, ids, 0)
				check := func() {
					t.Helper()
					rows := m.transcriptRows(width)
					text := transcriptRowsText(rows)
					v := fullViewport(len(rows), width)
					links := m.visibleAgentLinks(v)
					if len(links) != 1 || !strings.Contains(text[links[0].Y], "linked agent task") {
						t.Fatalf("agent link missed its row: %+v / %q", links, text)
					}
					heads := m.visibleDisclosurePlacements(v, activityAll)
					if len(heads) != 2 || heads[1].key != "call:host-agent" || !strings.HasPrefix(text[heads[1].Y], "▾") || heads[1].Y >= links[0].Y {
						t.Fatalf("second chunk header missed its row: %+v / %q", heads, text)
					}
					toolY := -1
					for _, link := range m.visibleToolOutputLinks(v, 0) {
						if _, row := m.toolOutputRow(link); row != nil && row.callID == read.ID {
							toolY = link.Y
						}
					}
					if toolY <= heads[1].Y || !strings.Contains(text[toolY], "read target.go") {
						t.Fatalf("post-image tool control missed its row: %d / %q", toolY, text)
					}
					slots := m.visibleImagePlacements(v)
					if (len(slots) != 0) != native || len(slots) > 0 && heads[1].Y < slots[0].Y+slots[0].Rows {
						t.Fatalf("image slots crossed into the second chunk: %+v / %+v", slots, heads)
					}
				}
				check()
			})
		}
	}
}

func TestHydratedAgentsUseVerifiedIdentityAndFirstOutcome(t *testing.T) {
	m := newReplModel()
	calls := []messages.ChatMessageToolCall{
		agentCall("done", `{"label":"original label","background":true}`),
		agentCall("legacy", `{"background":true}`),
		agentCall("blocking", `{"task":"old blocking"}`),
		agentCall("ambiguous", `{}`),
	}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "delegate"}, {Role: messages.MessageRoleAssistant, ToolCalls: calls}}
	for _, call := range calls {
		msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "success"}
		msg.SetToolSucceeded(true)
		history = append(history, msg)
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "launched"})
	m.hydrateHistory(history, "parent")
	m.hydrateAgentSessions("parent", []sessions.SessionSummary{
		{Metadata: &sessions.Metadata{Name: "renamed-child", Parent: "parent", SpawnCallID: "done", SpawnOutcome: sessions.ReportFinished}},
		{Metadata: &sessions.Metadata{Name: "wrong-parent", Parent: "other", SpawnCallID: "legacy", SpawnOutcome: sessions.ReportFinished}},
		{Metadata: &sessions.Metadata{Name: "duplicate-1", Parent: "parent", SpawnCallID: "ambiguous"}},
		{Metadata: &sessions.Metadata{Name: "duplicate-2", Parent: "parent", SpawnCallID: "ambiguous"}},
	})
	for _, tc := range []struct{ id, session, status string }{
		{"done", "renamed-child", "idle · done"}, {"legacy", "", "unknown"}, {"blocking", "", "done"}, {"ambiguous", "", "done"},
	} {
		var row *toolDisclosureRow
		for _, record := range m.toolDisclosures.all() {
			for i := range record.rows {
				if record.rows[i].callID == tc.id {
					row = &record.rows[i]
				}
			}
		}
		if row == nil || row.agent.session != tc.session || row.agent.display() != tc.status || row.agent.busy() {
			t.Fatalf("%s: %+v", tc.id, row)
		}
	}
	var ids []int64
	for id := range m.toolDisclosures.all() {
		ids = append(ids, id)
	}
	if m.toolRowCount(ids) != 0 {
		t.Fatal("hydrated agents counted as Tools")
	}
}

func TestAgentLabelClickKeepsTheAgentsDisclosureOpen(t *testing.T) {
	r, _ := newChildTestREPL(t)
	m := r.model
	m.beginTurn("go")
	m.appendToolCallStart(agentCall("a", `{"label":"child label"}`))
	record, row := m.toolDisclosureRowForCall("a")
	child := &replTab{name: "child", model: newReplModel(), agentActivity: row.agent}
	row.agent.session = "child"
	r.tabs = append(r.tabs, child)
	r.endTurn(nil)
	m.toggleDisclosureGroup(activityAgents, []int64{record.id}, 0)
	rows := m.transcriptRows(80)
	m.agentLinkPlacements = m.visibleAgentLinks(fullViewport(len(rows), 80))
	if len(m.agentLinkPlacements) != 1 {
		t.Fatalf("agent links = %+v", m.agentLinkPlacements)
	}
	link := m.agentLinkPlacements[0]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	if !r.workspace().inspector.open || r.workspace().inspector.target.session.Name != "child" || r.model != m || !m.lineOpenOf(record) {
		t.Fatalf("label click collapsed the disclosure: request %d, record %+v", r.showTabRequest, record)
	}
}

func TestSavedAgentNavigationResolvesRenamesAndReadsLeasedChildren(t *testing.T) {
	for _, kind := range []string{"renamed", "missing", "leased", "reused"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "parent")
			child, err := store.Acquire(ctx, "child", sessions.AcquireOptions{Parent: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			if err := updateMetadata(ctx, child, func(md *sessions.Metadata) { md.SpawnCallID = "a"; md.SpawnOutcome = sessions.ReportFinished }); err != nil {
				t.Fatal(err)
			}
			m := r.model
			m.appendToolCallStart(agentCall("a", `{"label":"saved child"}`))
			record, row := m.toolDisclosureRowForCall("a")
			row.agent.session = "child"
			m.agentLinkPlacements = []agentLink{{recordID: record.id, rowIndex: 0, X: 4, Y: 3, Cols: 11}}
			switch kind {
			case "renamed":
				if err := child.Rename(ctx, "new-child-name"); err != nil {
					t.Fatal(err)
				}
				if err := child.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing", "reused":
				if err := child.Close(); err != nil {
					t.Fatal(err)
				}
				if err := store.Delete(ctx, "child"); err != nil {
					t.Fatal(err)
				}
				if kind == "reused" {
					replacement := testAcquireSession(t, store, "child")
					_ = replacement.Close()
				}
			}
			m.mu.Lock()
			handled := r.openAgentAt(4, 3)
			m.mu.Unlock()
			if !handled {
				t.Fatal("label not handled")
			}
			r.applyTabRequests()
			view := waitInspector(t, r, 140)
			if kind == "renamed" {
				if view.info == nil || view.info.Metadata.Name != "new-child-name" {
					t.Fatalf("opened %#v", view.info)
				}
			} else if kind == "leased" {
				if len(r.tabs) != 1 || r.visibleTab().name != "parent" || view.info == nil || !view.info.InUse {
					t.Fatal("inspection acquired a runtime")
				}
			} else {
				if r.opening != "" || !strings.Contains(inspectorText(view), "session not found") {
					t.Fatalf("refusal = %q, opening %q", inspectorText(view), r.opening)
				}
			}
		})
	}
}

func TestAgentOpenLeaseRechecksDeletedOrReusedNames(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.WithValue(context.Background(), agentSessionTargetKey{}, agentSessionTarget{"parent", "a"})
	if _, err := getOrCreateSession(ctx, store, "missing", true, false); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if exists, _ := store.Exists(ctx, "missing"); exists {
		t.Fatal("agent link created missing session")
	}
	replacement := testAcquireSession(t, store, "reused")
	_ = replacement.Close()
	if _, err := getOrCreateSession(ctx, store, "reused", true, false); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("reused: %v", err)
	}
}

func TestHydratedAgentGroupsStopAtAssistantProse(t *testing.T) {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{agentCall("a", `{"label":"first"}`)}},
		{Role: messages.MessageRoleTool, ToolCallID: "a", ToolName: subagent.ToolName},
		{Role: messages.MessageRoleAssistant, Content: "some prose"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{agentCall("b", `{"label":"second"}`)}},
		{Role: messages.MessageRoleTool, ToolCallID: "b", ToolName: subagent.ToolName},
		{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyDisplayToolCalls: `[{"id":"a","name":"spawn_agent"},{"id":"b","name":"spawn_agent"}]`}},
		{Role: messages.MessageRoleAssistant, Content: "all done"},
	}, "parent")
	blocks := activityBlocks(m, 100)
	if len(blocks) != 2 {
		t.Fatalf("history merged across prose: %+v", blocks)
	}
	for _, block := range blocks {
		if !strings.Contains(plainStyledText(block.text), "1 agent") {
			t.Fatalf("wrong group count: %q", block.text)
		}
	}
	// Each group keeps its own row; a usage-free history turn adds no trailer.
	if m.turnTrailers.count() != 0 {
		t.Fatalf("hydrated agent turn attached a trailer: %+v", m.turnTrailers)
	}
}

func TestAgentLabelCountsRunningAndOutcomes(t *testing.T) {
	withDisplayTTY(t)
	header := func(m *replModel) string {
		return strings.SplitN(plainStyledText(activityBlocks(m, 100)[0].text), "\n", 2)[0]
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	calls := []messages.ChatMessageToolCall{agentCall("a", `{}`), agentCall("b", `{}`), agentCall("c", `{}`), agentCall("d", `{}`)}
	tui.AppendToolStart(calls)
	if got := header(m); !strings.Contains(got, "▸ 4 agents running") {
		t.Fatalf("all running = %q", got)
	}
	tui.AppendToolEnd(calls[0], "ok", time.Millisecond, nil)
	if got := header(m); !strings.Contains(got, "▸ 3 agents running, 1 completed") {
		t.Fatalf("one completed = %q", got)
	}
	tui.AppendToolEnd(calls[1], llm.ToolDeniedContent, time.Millisecond, nil)
	tui.AppendToolEnd(calls[2], "", time.Millisecond, errors.New("launch failed"))
	// Denied and failed count together; canceled is listed on its own so the
	// counts always add up to the total.
	if got := header(m); !strings.Contains(got, "▸ 1 agent running, 1 completed, 2 failed") {
		t.Fatalf("mixed outcomes while running = %q", got)
	}
	tui.AppendToolEnd(calls[3], "", time.Millisecond, context.Canceled)
	if got := header(m); !strings.Contains(got, "▸ 4 agents, 2 failed, 1 canceled") || strings.Contains(got, "running") {
		t.Fatalf("settled = %q", got)
	}

	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	m = r.model
	m.beginTurn("delegate")
	tui = &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	only := agentCall("only", `{}`)
	tui.AppendToolStart([]messages.ChatMessageToolCall{only})
	if got := header(m); !strings.Contains(got, "▸ 1 agent running") {
		t.Fatalf("single running = %q", got)
	}
	tui.AppendToolEnd(only, "", time.Millisecond, context.Canceled)
	if got := header(m); !strings.Contains(got, "▸ 1 agent, 1 canceled") {
		t.Fatalf("single canceled = %q", got)
	}
}
