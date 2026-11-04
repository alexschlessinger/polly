package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func TestInlineToolOutputCompletesOnRetiredReplacement(t *testing.T) {
	withDisplayTTY(t)
	for _, navigation := range []string{"close", "panel", "workspace"} {
		t.Run(navigation, func(t *testing.T) {
			r, screen := affordanceTestREPL(t)
			t.Cleanup(func() { _ = r.work.close() })
			store := &gatedInlineOutputStore{Store: testArtifactStore(t), gate: make(chan struct{}), started: make(chan struct{})}
			ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("full-agent-output")})
			if err != nil {
				t.Fatal(err)
			}
			agent := newReplModel()
			agent.artifactStore = store
			call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"a.go"}`}
			agent.hydrateHistory([]messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "read"},
				{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
				{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
			}, "agent")
			watchTestAgent(r, agent)
			r.tabs[len(r.tabs)-1].viewTarget.ID = "output-agent"
			v := waitInspector(t, r, 160)
			screen.SetSize(160, 40)
			r.render()
			link := v.model.toolOutputLinks[0]
			r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.X, Y: link.Y}})
			select {
			case <-store.started:
			case <-time.After(time.Second):
				t.Fatal("output did not start loading")
			}
			original := v.model
			agent.appendLine("new agent message")
			// A paint may have queued a projection before the agent changed.
			v = waitInspector(t, r, 160)
			v = waitInspector(t, r, 160)
			if v.model == original {
				t.Fatal("projection was not replaced")
			}
			_, row := v.model.toolOutputRow(link)
			if row == nil || !row.outputLoading {
				t.Fatal("replacement did not inherit the pending read")
			}
			source := row.output
			target := r.workspace().inspector.target
			var other *replModel
			switch navigation {
			case "close":
				r.closeInspector()
			case "panel", "workspace":
				// This different conversation deliberately shares the immutable
				// source snapshot and section key, but does not own this request.
				other = childDisplayCopy(agent)
				otherTab := &replTab{name: "other", model: other, viewTarget: sessions.ViewTarget{ID: "other"}, workspaceRoot: navigation == "workspace"}
				r.tabs = append(r.tabs, otherTab)
				if navigation == "workspace" {
					r.showTab(len(r.tabs) - 1)
				}
				r.inspect(tabViewTarget(otherTab))
				other = waitInspector(t, r, 160).model
			}
			cached := r.childViews.entries["inspector:"+target.key()]
			if cached == nil || cached.model != v.model {
				t.Fatal("replacement was not retired to the cache")
			}
			close(store.gate)
			runInlineOutputTask(t, r)
			if row.outputLoading || !row.output.loaded || !strings.Contains(plainStyledText(row.output.text), "full-agent-output") {
				t.Fatalf("retired replacement lost completion: loading=%v output=%+v", row.outputLoading, row.output)
			}
			if other != nil {
				for _, record := range other.toolDisclosures.all() {
					for _, row := range record.rows {
						if row.output != source || row.outputLoading {
							t.Fatal("completion leaked to another conversation")
						}
					}
				}
			}
			if navigation == "workspace" {
				r.showTab(0)
			}
			r.inspect(target)
			v = waitInspector(t, r, 160)
			r.render()
			_, row = v.model.toolOutputRow(link)
			if row == nil || row.outputLoading || !row.output.loaded || !strings.Contains(plainStyledText(row.output.text), "full-agent-output") {
				t.Fatalf("reopened view lost completion: %+v", row)
			}
			if store.opens.Load() != 1 {
				t.Fatalf("reopening duplicated the read: %d", store.opens.Load())
			}
		})
	}
}

func TestInlineToolOutputFollowsCacheTransferWithoutChangingOldWorkspace(t *testing.T) {
	withDisplayTTY(t)
	f := newViewportOutputFixture(t, "watch", 1, false, false, "preview", "transferred output")
	child := f.r.tabs[len(f.r.tabs)-1]
	child.viewTarget.ID = "transferred-agent"
	child.model.appendLine("identity resolved")
	v := waitInspector(t, f.r, 160)
	f.r.render()
	link := v.model.toolOutputLinks[0]
	if !f.r.activateToolOutput(v.model, link, &v.target) {
		t.Fatal("could not open output")
	}
	waitViewportOutputReads(t, f.store, 1)
	v = waitInspector(t, f.r, 160)
	target := f.r.workspace().inspector.target
	f.r.closeInspector()
	if cached := f.r.childViews.entries["inspector:"+target.key()]; cached == nil || cached.view != v {
		t.Fatal("view was not cached")
	}
	f.r.tabs = append(f.r.tabs, &replTab{name: "another workspace", model: newReplModel(), workspaceRoot: true})
	f.r.showTab(len(f.r.tabs) - 1)
	f.r.inspect(target)
	owner := f.r.workspace().viewState(target)
	owner.disclosures = disclosuresDetailed
	if current := waitInspector(t, f.r, 160); current != v {
		t.Fatal("another workspace did not acquire the cached instance")
	}
	before, oldRevision, oldTop := owner.revision, f.state.revision, f.state.top
	close(f.store.gate)
	runInlineOutputTask(t, f.r)
	if owner.revision != before+1 {
		t.Fatal("completion did not fence the new owner's pending projections")
	}
	if f.state.revision != oldRevision || f.state.top != oldTop {
		t.Fatal("completion changed the old workspace's navigation")
	}
	_, row := v.model.toolOutputRow(link)
	if row == nil || row.outputLoading || !row.output.loaded || plainStyledText(row.output.text) != "transferred output" {
		t.Fatalf("cache transfer lost completion: %+v", row)
	}
}

func TestInlineToolOutputReplacementRejectsSupersededSnapshot(t *testing.T) {
	withDisplayTTY(t)
	for _, superseded := range []string{"page", "execution"} {
		t.Run(superseded, func(t *testing.T) {
			f := newViewportOutputFixture(t, "watch", 1, false, false, "preview", "obsolete output")
			f.r.render()
			v := f.r.workspace().inspector.current
			link := v.model.toolOutputLinks[0]
			if !f.r.activateToolOutput(v.model, link, &v.target) {
				t.Fatal("could not open output")
			}
			waitViewportOutputReads(t, f.store, 1)
			original := v.model
			v.model = childDisplayCopy(original)
			_, row := v.model.toolOutputRow(link)
			if row == nil || !row.outputLoading {
				t.Fatal("replacement did not inherit load")
			}
			next := *row.output
			if superseded == "page" {
				next.offset, next.end = 100, 200
			} else {
				// A new execution may reuse both call and artifact identities.
				// Value equality is not proof that it owns this in-flight read.
				next.artifact = &artifacts.Ref{ID: row.output.artifact.ID, Kind: artifacts.KindText}
			}
			next.text, next.loaded = "newer output", true
			row.output, row.outputLoading = &next, false
			close(f.store.gate)
			runInlineOutputTask(t, f.r)
			if row.output != &next || row.outputLoading || row.output.text != "newer output" {
				t.Fatalf("stale %s read replaced the newer snapshot: %+v", superseded, row.output)
			}
		})
	}
}
