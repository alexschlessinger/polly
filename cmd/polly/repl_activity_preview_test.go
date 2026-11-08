package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestActivityDepthBoundsAgentsAndImages(t *testing.T) {
	withDisplayTTY(t)
	for _, native := range []bool{false, true} {
		for _, width := range []int{32, 120} {
			t.Run(fmt.Sprintf("native-%v-width-%d", native, width), func(t *testing.T) {
				r := newManagedREPL(&Config{}, "ctx", 0, 0)
				m := r.model
				m.nativeImages, m.imageCellWidth, m.imageCellHeight = native, 10, 20
				m.beginTurn("busy turn")
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				var calls []messages.ChatMessageToolCall
				for i := range 13 {
					calls = append(calls, agentCall(fmt.Sprintf("agent-%02d", i), fmt.Sprintf(`{"label":"worker-%02d"}`, i)))
				}
				for i := range 16 {
					calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprintf("image-%02d", i), Name: "view_image"})
				}
				tui.AppendToolStart(calls)
				path := filepath.Join(t.TempDir(), "frame.png")
				writeImageFixture(t, path, 8, 4)
				for i, call := range calls {
					tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
					_, row := m.toolDisclosureRowForCall(call.ID)
					if i < 13 {
						row.agent.session = call.ID
						continue
					}
					tui.AppendToolMedia(call, []style.Image{{
						Path: path, Alt: call.ID + ".png", Width: 8, Height: 4, Inspection: true,
						MaxCols: style.InspectionThumbnailCols, MaxRows: style.InspectionThumbnailRows,
					}})
				}
				// Hold prose below the activity while depth changes resize it.
				m.appendLine("held below activity")
				m.reasoningWidth = width
				for i, row := range transcriptRowsText(m.transcriptRows(width)) {
					if strings.Contains(row, "held below activity") {
						m.scrollAnchor, m.followBottom = i, false
						break
					}
				}
				for _, step := range []struct {
					depth          disclosureDepth
					agents, images int
				}{
					{depthAbridged, 5, 3},
					{depthFull, 13, 16},
					{depthCalls, 5, 3},
					{depthCollapsed, 0, 0},
					{depthAbridged, 5, 3},
				} {
					if m.depth != step.depth {
						m.toggleAllDisclosures(width)
					}
					block := activityBlocks(m, width)[0]
					text := plainStyledText(style.StripImageMarkers(block.text))
					field, images, ok := m.inlineImageField(block.toolDisclosureIDs, true)
					if !ok || field.raw != "16 images viewed" || len(images) != 16 || m.agentCounts(block.toolDisclosureIDs).Total != 13 {
						t.Fatalf("%s lost total counts: %q", step.depth.label(), text)
					}
					if width >= 80 && (!strings.Contains(text, "13 agents") || !strings.Contains(text, "16 images viewed")) {
						t.Fatalf("%s header lost total counts: %q", step.depth.label(), text)
					}
					if len(block.agentLinks) != step.agents || len(block.images) != step.images {
						t.Fatalf("%s shows %d agents / %d images, want %d / %d", step.depth.label(), len(block.agentLinks), len(block.images), step.agents, step.images)
					}
					if strings.Contains(text, "Ctrl-O") {
						t.Fatalf("%s previews include shortcut text: %q", step.depth.label(), text)
					}
					// A cut depth marks the call run, the agents and the
					// images each on its first shown row; nothing else is
					// marked, and the full depth cuts none of them.
					cuts := step.depth == depthAbridged || step.depth == depthCalls
					var marked []string
					for _, line := range strings.Split(text, "\n") {
						if rest, ok := strings.CutPrefix(line, reasoningCutIndent); ok {
							marked = append(marked, rest)
						}
					}
					if !cuts && len(marked) != 0 {
						t.Fatalf("%s marks a cut: %q", step.depth.label(), text)
					}
					if cuts && (len(marked) != 3 || !strings.HasPrefix(marked[0], "✓ view_image") || !strings.HasPrefix(marked[1], "✓ worker-08") || !strings.Contains(marked[2], "image-13.png")) {
						t.Fatalf("%s wants the calls, the agents and the images each marked: %q", step.depth.label(), text)
					}
					if strings.Contains(text, "more agent") || strings.Contains(text, "more image") {
						t.Fatalf("%s kept an omission hint row: %q", step.depth.label(), text)
					}
					for i, link := range block.agentLinks {
						index := 13 - step.agents + i
						row := m.toolDisclosures.get(link.recordID).rows[link.rowIndex]
						if link.rowIndex != index || row.agent.label != fmt.Sprintf("worker-%02d", index) {
							t.Fatalf("%s agent link points at the wrong row: %+v", step.depth.label(), link)
						}
					}
					for i, img := range block.images {
						if want := fmt.Sprintf("image-%02d.png", 16-step.images+i); img.Alt != want {
							t.Fatalf("%s image %d = %q, want %q", step.depth.label(), i, img.Alt, want)
						}
					}
					_, spans := transcriptBlockRowsWithImages(block.text, false, width, block.images, native, 10, 20)
					if native && len(spans) != step.images {
						t.Fatalf("%s image spans = %+v", step.depth.label(), spans)
					}
					for i, span := range spans {
						if span.imageIndex != i {
							t.Fatalf("%s image marker kept its old index: %+v", step.depth.label(), span)
						}
					}
					rows := transcriptRowsText(m.transcriptRows(width))
					if !strings.Contains(rows[m.scrollAnchor], "held below activity") {
						t.Fatalf("%s moved held viewport: %q", step.depth.label(), rows[m.scrollAnchor])
					}
					for _, link := range m.visibleAgentLinks(fullViewport(len(rows), width)) {
						a := m.toolDisclosures.get(link.recordID).rows[link.rowIndex].agent
						if !strings.Contains(rows[link.Y], a.label) {
							t.Fatalf("%s agent link is off its label: %+v", step.depth.label(), link)
						}
					}
				}
			})
		}
	}
}

func TestAbridgedImageWindowBoundary(t *testing.T) {
	withDisplayTTY(t)
	for count := range 5 {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m := newReplModel()
			m.beginTurn("images")
			record := m.appendToolCallStart(messages.ChatMessageToolCall{ID: "images", Name: "view_image"})
			for i := range count {
				record.rows[0].inspectionImages = append(record.rows[0].inspectionImages, style.Image{Alt: fmt.Sprintf("frame-%d.png", i), Inspection: true})
			}
			block := transcriptDisplayBlock{toolDisclosureIDs: []int64{record.id}}
			m.layoutInlineActivityBlock(&block, 80)
			if len(block.images) != min(count, 3) {
				t.Fatalf("%d images produced %d previews", count, len(block.images))
			}
			for i, img := range block.images {
				if want := fmt.Sprintf("frame-%d.png", max(0, count-3)+i); img.Alt != want {
					t.Fatalf("image %d = %q, want %q", i, img.Alt, want)
				}
			}
			// Past the window the first image row carries the cut mark.
			text := plainStyledText(block.text)
			if strings.Contains(text, "more image") || strings.Contains(text, "\n"+reasoningCutIndent+"viewed · frame-1.png") != (count == 4) || strings.Contains(text, reasoningCutIndent) != (count > 3) {
				t.Fatalf("%d images produced the wrong cut mark: %q", count, text)
			}
			if controls := len(block.activitySectionTargets); controls != min(1, max(0, count-3)) {
				t.Fatalf("%d images want %d cut controls, got %+v", count, min(1, max(0, count-3)), block.activitySectionTargets)
			}
		})
	}
}

// The abridged agents keep their newest activityAgentWindow and every busy
// agent; the first shown after hidden ones carries the cut mark and the
// cut's control, and no hint row counts what is hidden.
func TestAbridgedAgentCutMark(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name            string
		hidden, running int
		marked          string
	}{
		{"nothing hidden", 0, 0, ""},
		{"one settled", 1, 0, "worker-1"},
		{"one running", 1, 1, ""},
		{"mixed", 5, 2, "worker-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("agents")
			for i := range tc.hidden + activityAgentWindow {
				id := fmt.Sprint(i)
				m.appendToolCallStart(agentCall(id, fmt.Sprintf(`{"label":"worker-%s"}`, id)))
				_, row := m.toolDisclosureRowForCall(id)
				row.agent.session = id
				// The window's agents run too; of the older ones only the
				// first tc.running do, and those stay in view.
				if i >= tc.running && i < tc.hidden {
					row.agent.setLocal("done", false)
				}
			}
			record := m.currentToolDisclosure()
			detail, links, controls := m.agentDetailCut([]int64{record.id}, 32, style.Rail)
			text := plainStyledText(detail)
			if len(links) != activityAgentWindow+tc.running || strings.Contains(text, "more agent") {
				t.Fatalf("want the newest %d and %d busy agents, no hint row: %q / %+v", activityAgentWindow, tc.running, text, links)
			}
			lines := strings.Split(text, "\n")
			var marks []int
			for i, line := range lines {
				if strings.HasPrefix(line, reasoningCutIndent) {
					marks = append(marks, i)
				}
			}
			if tc.marked == "" {
				if len(marks) != 0 || len(controls) != 0 {
					t.Fatalf("nothing hidden wants no mark or control: %q / %v", text, controls)
				}
			} else if len(marks) != 1 || !strings.Contains(lines[marks[0]], tc.marked+" ") || len(controls) != 1 || controls[0] != marks[0] {
				t.Fatalf("want %s marked and its line the control: %q / %v", tc.marked, text, controls)
			}
			rows := style.VisualRows(detail, ui.StyleClear, 32)
			for _, link := range links {
				if !strings.Contains(ui.CellsToString(rows[link.Y]), record.rows[link.rowIndex].agent.label) {
					t.Fatalf("the mark moved an agent link off its row: %+v", link)
				}
			}
			// Full depth shows every agent and cuts none.
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
			detail, links, controls = m.agentDetailCut([]int64{record.id}, 80, style.Rail)
			if m.depth != depthFull || len(links) != tc.hidden+activityAgentWindow || strings.Contains(plainStyledText(detail), reasoningCutIndent) || len(controls) != 0 {
				t.Fatalf("Ctrl-O did not reveal the full list: %q / %+v", plainStyledText(detail), links)
			}
		})
	}
}

func TestAbridgedAgentsWindowBeforeWorkflowGrouping(t *testing.T) {
	withDisplayTTY(t)
	m := newReplModel()
	m.beginTurn("interleaved agents")
	for i, workflow := range []string{"old", "first", "second", "first", "second", "", "first", "second"} {
		id := fmt.Sprint(i)
		m.appendToolCallStart(agentCall(id, fmt.Sprintf(`{"label":"worker-%s"}`, id)))
		_, row := m.toolDisclosureRowForCall(id)
		row.agent.session = id
		row.agent.workflowID, row.agent.workflowName = workflow, workflow
		// Settled, so only the window keeps them in view.
		row.agent.setLocal("done", false)
		if i == 1 {
			row.agent.decisions = 1
		}
	}
	record := m.currentToolDisclosure()
	detail, links := m.agentDetail([]int64{record.id}, 120, "  ")
	text := plainStyledText(detail)
	if strings.Contains(text, "Workflow · old") || !strings.Contains(text, "Workflow · first · 1 needs decision") || !strings.Contains(text, "Workflow · second") {
		t.Fatalf("window lost workflow decisions or kept an empty heading: %q", text)
	}
	// The first shown agent follows hidden ones; the others follow shown
	// ones whichever group they sit in.
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, reasoningCutIndent) != strings.Contains(line, "worker-3 ") {
			t.Fatalf("want worker-3 alone marked: %q", text)
		}
	}
	wantRows := []int{3, 6, 4, 7, 5}
	if len(links) != len(wantRows) {
		t.Fatalf("window links = %+v", links)
	}
	lines := strings.Split(text, "\n")
	for i, link := range links {
		if link.recordID != record.id || link.rowIndex != wantRows[i] || !strings.Contains(lines[link.Y], fmt.Sprintf("worker-%d", wantRows[i])) {
			t.Fatalf("grouping changed the window or its link: %+v / %q", link, text)
		}
	}
}
