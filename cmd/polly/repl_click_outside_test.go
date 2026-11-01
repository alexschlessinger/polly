package main

import (
	"image"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A click outside a painted dialog is Escape: the dialog keeps its typed draft,
// closes, and runs its cancel hook. Its own frame still swallows clicks.
func TestClickOutsideDismissesModal(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	canceled := false
	m := &replModal{title: "pick", bounds: image.Rect(10, 10, 40, 20), listBounds: image.Rect(11, 12, 39, 19), onCancel: func() { canceled = true }}
	m.items = []replModalItem{{label: "one", value: "1"}}
	r.model.modal = m
	r.handleModalEvent(mouseEvent("<MouseLeft>", image.Pt(10, 10)))
	if r.model.modal != m || canceled {
		t.Fatal("a click on the dialog frame dismissed it")
	}
	r.handleModalEvent(mouseEvent("<MouseWheelDown>", image.Pt(5, 5)))
	if r.model.modal != m {
		t.Fatal("the wheel outside the dialog dismissed it")
	}
	r.handleModalEvent(mouseEvent("<MouseLeft>", image.Pt(5, 5)))
	if r.model.modal != nil || !canceled {
		t.Fatal("a click outside did not dismiss the dialog like Escape")
	}

	draft := ""
	input := &replModal{title: "Message", inputMode: true, bounds: image.Rect(10, 10, 40, 20), onDraft: func(text string) { draft = text }}
	input.input.setText("half typed")
	r.model.modal = input
	r.handleModalEvent(mouseEvent("<MouseLeft>", image.Pt(5, 5)))
	if r.model.modal != nil || draft != "half typed" {
		t.Fatalf("a click outside an input dialog lost its draft: modal=%v draft=%q", r.model.modal != nil, draft)
	}

	// A dialog that was never painted has no outside yet.
	unpainted := &replModal{title: "pick", items: []replModalItem{{label: "one", value: "1"}}}
	r.model.modal = unpainted
	r.handleModalEvent(mouseEvent("<MouseLeft>", image.Pt(5, 5)))
	if r.model.modal != unpainted {
		t.Fatal("a click dismissed a dialog before it was painted")
	}
}

func TestClickOutsideClosesModelForm(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	f.modal.bounds = image.Rect(2, 2, 62, 26)
	r.handleModalEvent(mouseEvent("<MouseWheelDown>", image.Pt(70, 30)))
	if r.model.modal == nil {
		t.Fatal("the wheel outside the form closed it")
	}
	r.handleModalEvent(mouseEvent("<MouseLeft>", image.Pt(70, 30)))
	if r.model.modal != nil {
		t.Fatal("a click outside the form did not close it")
	}
}

// The Agents status field retargets an open inspector like a link beside it;
// other outside clicks leave the pane open and reach the surrounding UI.
func TestClickOutsideKeepsInspectorAndAgentsFieldRetargets(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	m := r.model
	m.ed.setText("draft stays here")
	call := messages.ChatMessageToolCall{ID: "one", Name: "read_file"}
	m.appendToolCallStart(call)
	m.displayCatalog.setResult(call, messages.ChatMessage{Content: "result line"})
	tab := r.visibleTab()
	tab.viewTarget.ID = "parent"
	tab.swarmSnapshot = decisionSnapshot()
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	r.render()
	f := m.status.agentsField
	if f.Cols == 0 {
		t.Fatalf("no Agents status field: %q", m.status.agents)
	}
	_, height := screen.Size()
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(f.X, height-1)))
	w := r.workspace()
	if !w.inspector.open || w.inspector.target.kind != agentsViewKind {
		t.Fatalf("the Agents status field did not retarget the inspector: open=%v %+v", w.inspector.open, w.inspector.target)
	}
	r.render()
	r.handleEvent(mouseEvent("<MouseLeft>", r.inputW.Inner.Min))
	if !w.inspector.open || w.inspector.focused {
		t.Fatal("a click on the composer closed the inspector or kept its focus")
	}
	if m.ed.text() != "draft stays here" || m.modal != nil {
		t.Fatal("the outside click changed the draft or opened a dialog")
	}
	if r.handleInspectorEvent(mouseEvent("<MouseLeft>", r.inputW.Inner.Min)) {
		t.Fatal("the inspector consumed a click intended for the surrounding UI")
	}
}

func TestClickOutsideKeepsAgentInspectionAndWatch(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	r.model.ed.setText("draft stays here")
	r.tabs[1].model.appendUserPrompt("Agent assignment")
	r.inspect(tabViewTarget(r.tabs[1]))
	screen.SetSize(140, 40)
	for _, watch := range []bool{false, true} {
		if watch {
			r.inspectorAction("watch")
		}
		waitInspector(t, r, 140)
		r.render()
		i := &r.workspace().inspector
		target := i.target
		i.focused = true
		point := r.inputW.Inner.Min
		if point.In(r.chrome.frame) {
			t.Fatal("composer click is not outside the inspector")
		}
		r.handleEvent(mouseEvent("<MouseLeft>", point))
		if !i.open || i.target != target || i.focused || r.workspace().viewState(target).watch != watch {
			t.Fatalf("outside click changed agent view: watch=%v inspector=%+v", watch, i)
		}
		if r.model.ed.text() != "draft stays here" {
			t.Fatal("outside click changed the composer draft")
		}
	}
	r.handleEvent(mouseEvent("<MouseLeft>", r.chrome.close.Min))
	if r.workspace().inspector.open {
		t.Fatal("explicit close button stopped dismissing the agent view")
	}
}
