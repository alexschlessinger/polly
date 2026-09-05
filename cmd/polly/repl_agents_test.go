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

func activityBlocks(m *replModel, width int) []transcriptDisplayBlock {
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
	header := plainStyledText(blocks[0].text)
	if !strings.Contains(header, "3 tools · ▸ 2 agents · ▸ 1 image viewed") || strings.Contains(header, "Trace sessions") {
		t.Fatalf("collapsed header = %q", header)
	}
	ids := blocks[0].toolDisclosureIDs
	if m.turnToolCallCount() != 5 || m.runningTools != 0 {
		t.Fatalf("execution counters changed: %d / %d", m.turnToolCallCount(), m.runningTools)
	}
	m.toggleToolDisclosureGroup(ids)
	m.toggleAgentDisclosureGroup(ids)
	m.toggleImageDisclosureGroup(ids)
	record := m.toolDisclosures[ids[0]]
	if !record.expanded || !record.agentsExpanded || !record.imagesExpanded {
		t.Fatal("controls did not expand independently")
	}
	tools, _ := toolDisclosureText(record)
	if strings.Contains(plainStyledText(tools), "Trace sessions") || !strings.Contains(plainStyledText(tools), "mcp__spawn_agent") {
		t.Fatalf("Tools classification = %q", tools)
	}
	detail, _ := m.agentDetail(ids, 120)
	if got := plainStyledText(detail); got != "  ✓ Trace sessions · done\n  ✓ Review picker · done" {
		t.Fatalf("agent detail = %q", got)
	}
	m.toggleAgentDisclosureGroup(ids)
	if !record.expanded || record.agentsExpanded || !record.imagesExpanded {
		t.Fatal("Agents collapsed another disclosure")
	}
	tui.AppendAssistantText("I have the first results.")
	tui.AppendToolStart([]messages.ChatMessageToolCall{agentCall("f", `{}`)})
	tui.AppendToolEnd(agentCall("f", `{}`), "ok", time.Millisecond, nil)
	blocks = activityBlocks(m, 120)
	if len(blocks) != 2 || !strings.Contains(plainStyledText(blocks[1].text), "1 agent") || strings.Contains(plainStyledText(blocks[1].text), "tool") {
		t.Fatalf("prose did not separate agent-only batch: %+v", blocks)
	}
	m.toggleAgentDisclosureGroup(ids)
	r.endTurn(nil)
	trailer := m.turnTrailers[m.turnTrailerSeq]
	header = plainStyledText(m.transcript[trailer.transcriptIndex].text)
	if !strings.Contains(header, "3 tools · ▸ 3 agents · ▸ 1 image viewed") || record.agentsExpanded || record.imagesExpanded || record.expanded {
		t.Fatalf("settled trailer / expansion = %q / %+v", header, record)
	}
	if !m.toggleTurnTrailerOverlay(trailer, turnDockOverlayAgents) {
		t.Fatal("trailer Agents did not expand")
	}
	if got := plainStyledText(m.transcript[trailer.transcriptIndex].text); !strings.Contains(got, "agent · done") || !strings.Contains(got, "Trace sessions") {
		t.Fatalf("trailer omitted launches: %q", got)
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
			if row.agent.label != tc.label || row.agent.status != tc.status || row.agent.active {
				t.Fatalf("row = %+v", row.agent)
			}
			m.toggleAgentDisclosureGroup([]int64{record.id})
			text := plainStyledText(activityBlocks(m, 80)[0].text)
			if strings.Contains(text, "tool") || !strings.Contains(text, tc.label+" · "+tc.status) {
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
	if got := strings.Join(transcriptRowsText(m.transcriptRows(100)), "\n"); !strings.Contains(got, "2 agents") {
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
	m.toggleAgentDisclosureGroup([]int64{record.id})
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
	m.toggleAgentDisclosureGroup([]int64{record.id})
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
				tui.AppendToolStart([]messages.ChatMessageToolCall{call, agentCall("a", `{"label":"linked agent task"}`)})
				tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
				images := inspectionTranscriptImages(testToolImageResult(t, path, call.ID), nil)
				tui.AppendToolMedia(call, images)
				record, row := m.toolDisclosureRowForCall(call.ID)
				row.images = images // ordinary result media before the agent detail
				_, a := m.toolDisclosureRowForCall("a")
				a.agent.session = "child"
				ids := []int64{record.id}
				m.toggleToolDisclosureGroup(ids)
				m.toggleAgentDisclosureGroup(ids)
				m.toggleImageDisclosureGroup(ids)
				check := func() {
					t.Helper()
					rows := m.transcriptRows(width)
					links := m.visibleAgentLinks(fullViewport(len(rows), width))
					if len(links) != 1 {
						t.Fatalf("links = %+v", links)
					}
					link := links[0]
					text := transcriptRowsText(rows)[link.Y]
					if text != "  ↻ linked agent task ·" && !strings.Contains(text, "linked agent task") {
						t.Fatalf("link shifted into image: %+v / %q", link, text)
					}
					for _, image := range m.visibleImagePlacements(fullViewport(len(rows), width)) {
						if link.Y >= image.Y && link.Y < image.Y+image.Rows {
							t.Fatalf("link overlaps image: %+v / %+v", link, image)
						}
					}
				}
				check()
				m.toggleImageDisclosureGroup(ids)
				check()
				if !record.agentsExpanded || !record.expanded {
					t.Fatal("image toggle changed Agents or Tools")
				}
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
		{"done", "renamed-child", "done"}, {"legacy", "", "unknown"}, {"blocking", "", "done"}, {"ambiguous", "", "done"},
	} {
		var row *toolDisclosureRow
		for _, record := range m.toolDisclosures {
			for i := range record.rows {
				if record.rows[i].callID == tc.id {
					row = &record.rows[i]
				}
			}
		}
		if row == nil || row.agent.session != tc.session || row.agent.status != tc.status || row.agent.active {
			t.Fatalf("%s: %+v", tc.id, row)
		}
	}
	if m.turnDockToolRowCount(m.turnTrailers[m.turnTrailerSeq].dock) != 0 {
		t.Fatal("hydrated agents counted as Tools")
	}
}

func TestBackgroundAgentUpdatesOriginalTurnAndFreezesAfterFollowup(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "slow", Background: true}, "call-1")
	runUITask(t, r)
	if rep := awaitReport(t, result); rep.err != nil {
		t.Fatal(rep.err)
	}
	child := r.tabs[1]
	child.keepOpen = true
	record, row := parent.model.toolDisclosureRowForCall("call-1")
	initial := row.agent
	parent.model.mu.Lock()
	parent.model.takeActiveTool("call-1")
	parent.model.mu.Unlock()
	close(runs.release)
	settleUntil(t, r, settled(parent))
	if !initial.active || !r.needsTick() || record.agentsExpanded {
		t.Fatal("parent settlement lost background activity")
	}
	child.model.mu.Lock()
	child.model.approval = &approvalState{}
	child.model.mu.Unlock()
	r.refreshAgentActivities()
	if initial.status != "approval needed" {
		t.Fatalf("approval status = %q", initial.status)
	}
	child.model.mu.Lock()
	child.model.approval = nil
	child.model.mu.Unlock()
	parent.model.mu.Lock()
	parent.model.beginTurn("new parent turn")
	parent.model.appendToolCallStart(agentCall("new-call", `{"label":"new task"}`))
	parent.model.mu.Unlock()
	close(runs.slow)
	settleUntil(t, r, settled(child))
	r.refreshAgentActivities()
	if initial.active || initial.status != "done" {
		t.Fatalf("original = %+v", initial)
	}
	_, latest := parent.model.toolDisclosureRowForCall("new-call")
	if latest.agent.status != "starting" || !latest.agent.active {
		t.Fatalf("new run overwritten: %+v", latest.agent)
	}
	<-child.agentWriteDone
	md, err := child.state.session.GetMetadata(context.Background())
	if err != nil || md.SpawnCallID != "call-1" || md.SpawnOutcome != sessions.ReportFinished {
		t.Fatalf("metadata = %+v / %v", md, err)
	}
	if err := recordSpawnOutcome(child.state.session, sessions.ReportFailed); err != nil {
		t.Fatal(err)
	}
	md, _ = child.state.session.GetMetadata(context.Background())
	if md.SpawnOutcome != sessions.ReportFinished {
		t.Fatal("first outcome overwritten")
	}
	child.model.mu.Lock()
	child.model.beginTurn("wait")
	child.model.mu.Unlock()
	r.startManagedTurn(context.Background(), child, managedTurnInput{displayText: "wait"}, runs.run)
	r.refreshAgentActivities()
	if initial.active || initial.status != "done" {
		t.Fatal("follow-up reopened original activity")
	}
	child.turnCancel()
	settleUntil(t, r, settled(child))
	if initial.status != "done" {
		t.Fatal("follow-up cancellation changed first outcome")
	}
}

func TestFinalAgentSnapshotSurvivesAutomaticTabRemoval(t *testing.T) {
	r, _ := newChildTestREPL(t)
	m := r.model
	m.appendToolCallStart(agentCall("a", `{}`))
	_, row := m.toolDisclosureRowForCall("a")
	child := &replTab{name: "child", model: newReplModel(), agentActivity: row.agent, spawnCallID: "a", agentStatus: "done"}
	m.mu.Lock()
	if r.updateChildAgent(child) {
		t.Fatal("update should defer while locked")
	}
	r.pendingAgentUpdates = append(r.pendingAgentUpdates, child)
	m.mu.Unlock()
	r.refreshAgentActivities()
	if len(r.pendingAgentUpdates) != 0 || row.agent.active || row.agent.status != "done" {
		t.Fatalf("snapshot lost: %+v", row.agent)
	}
}

func TestAgentLabelClickPrecedesTrailerDismissal(t *testing.T) {
	r, _ := newChildTestREPL(t)
	m := r.model
	m.beginTurn("go")
	m.appendToolCallStart(agentCall("a", `{"label":"child label"}`))
	record, row := m.toolDisclosureRowForCall("a")
	child := &replTab{name: "child", model: newReplModel(), agentActivity: row.agent}
	row.agent.session = "child"
	r.tabs = append(r.tabs, child)
	r.endTurn(nil)
	trailer := m.turnTrailers[m.turnTrailerSeq]
	m.toggleTurnTrailerOverlay(trailer, turnDockOverlayAgents)
	rows := m.transcriptRows(80)
	m.agentLinkPlacements = m.visibleAgentLinks(fullViewport(len(rows), 80))
	if len(m.agentLinkPlacements) != 1 {
		t.Fatalf("trailer links = %+v", m.agentLinkPlacements)
	}
	link := m.agentLinkPlacements[0]
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
	if r.showTabRequest != 1 || m.openTurnTrailerID == 0 {
		t.Fatalf("label click dismissed overlay: request %d, record %d", r.showTabRequest, record.id)
	}
}

func TestSavedAgentNavigationResolvesRenamesAndRefusesMissingOrLeased(t *testing.T) {
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
			runUITask(t, r)
			if kind == "renamed" {
				select {
				case res := <-r.openDone:
					r.finishOpen(res)
				case <-time.After(5 * time.Second):
					t.Fatal("reopen did not finish")
				}
				if r.visibleTab().name != "new-child-name" {
					t.Fatalf("opened %s", r.visibleTab().name)
				}
			} else {
				want := "missing or ambiguous"
				if kind == "leased" {
					want = "open in another polly"
				}
				if r.opening != "" || !strings.Contains(plainStyledText(m.fullTranscript()), want) {
					t.Fatalf("refusal = %q, opening %q", m.fullTranscript(), r.opening)
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
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if !strings.Contains(plainStyledText(m.transcript[trailer.transcriptIndex].text), "2 agents") {
		t.Fatal("trailer lost earlier group")
	}
}

func TestAgentInitialFailureAndCancellationSurviveTabClosure(t *testing.T) {
	for _, outcome := range []sessions.ReportStatus{sessions.ReportFailed, sessions.ReportCanceled} {
		t.Run(string(outcome), func(t *testing.T) {
			r, runs := newChildTestREPL(t)
			parent := r.visibleTab()
			beginParentToolCall(t, r, runs, "a")
			gate := make(chan struct{})
			r.runTurn = func(ctx context.Context, prompt string, ui TurnUI) error {
				<-gate
				if outcome == sessions.ReportCanceled {
					return context.Canceled
				}
				return errors.New("deterministic child failure")
			}
			result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "fixture"}, "a")
			runUITask(t, r)
			child := r.tabs[1]
			if err := child.state.session.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "fixture"}); err != nil {
				t.Fatal(err)
			}
			_, row := parent.model.toolDisclosureRowForCall("a")
			initial := row.agent
			close(gate)
			settleUntil(t, r, settled(child))
			if rep := awaitReport(t, result); rep.err == nil {
				t.Fatal("fixture unexpectedly succeeded")
			}
			close(runs.release)
			settleUntil(t, r, settled(parent))
			settleUntil(t, r, func() bool { return len(r.tabs) == 1 })
			if initial.active || initial.status != spawnOutcomeStatus(outcome) {
				t.Fatalf("closed child row = %+v", initial)
			}
			<-child.agentWriteDone
			md, err := parent.state.sessionStore.GetAllMetadata(context.Background())
			if err != nil || md[child.name].SpawnOutcome != outcome {
				t.Fatalf("saved outcome = %+v / %v", md, err)
			}
		})
	}
}

func TestBackgroundAgentLivenessHandsOffToTrailer(t *testing.T) {
	r, runs := newChildTestREPL(t)
	parent := r.visibleTab()
	beginParentToolCall(t, r, runs, "call-1")
	result := spawnFromTool(context.Background(), r, parent, subagent.Request{Task: "slow", Background: true}, "call-1")
	runUITask(t, r)
	if rep := awaitReport(t, result); rep.err != nil {
		t.Fatal(rep.err)
	}
	child := r.tabs[1]
	child.keepOpen = true
	headers := func() (launch, trailer string) {
		parent.model.mu.Lock()
		defer parent.model.mu.Unlock()
		for _, block := range parent.model.transcriptDisplayEntries(100) {
			header := strings.SplitN(block.text, "\n", 2)[0]
			switch {
			case block.turnTrailerID != 0:
				trailer = header
			case block.isActivity():
				launch = header
			}
		}
		return launch, trailer
	}
	// The running count is the live cue, shown in one control at a time: the
	// launch row until its turn has a trailer, then the trailer.
	// Brightness follows the same rules as the thought and tools controls.
	if launch, trailer := headers(); !strings.Contains(plainStyledText(launch), "▸ 1 agent running") || trailer != "" {
		t.Fatalf("running turn: launch %q / trailer %q", launch, trailer)
	}
	parent.model.mu.Lock()
	parent.model.takeActiveTool("call-1")
	parent.model.mu.Unlock()
	close(runs.release)
	settleUntil(t, r, settled(parent))
	r.refreshAgentActivities()
	launchIdle, trailerLive := inlineActivityControl("▸", "1 agent", true), turnActivityControl("▸", "1 agent running")
	if launch, trailer := headers(); !strings.Contains(launch, launchIdle) || !strings.Contains(trailer, trailerLive) {
		t.Fatalf("settled turn: launch %q / trailer %q", launch, trailer)
	}
	// A new parent turn does not hand liveness back to the earlier launch row.
	parent.model.mu.Lock()
	parent.model.beginTurn("again")
	parent.model.mu.Unlock()
	if launch, trailer := headers(); !strings.Contains(launch, launchIdle) || !strings.Contains(trailer, trailerLive) {
		t.Fatalf("next turn: launch %q / trailer %q", launch, trailer)
	}
	close(runs.slow)
	settleUntil(t, r, settled(child))
	r.refreshAgentActivities()
	// A finished group stays as bright as its neighbours; only the running phrase goes.
	if launch, trailer := headers(); !strings.Contains(launch, launchIdle) || !strings.Contains(trailer, turnActivityControl("▸", "1 agent")) {
		t.Fatalf("finished child: launch %q / trailer %q", launch, trailer)
	}
	<-child.agentWriteDone
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
