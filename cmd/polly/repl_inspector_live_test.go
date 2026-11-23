package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

type inspectorLiveReadFunc func(string) (swarm.LiveTranscript, bool)

func (f inspectorLiveReadFunc) LiveTranscript(id string) (swarm.LiveTranscript, bool) { return f(id) }

type inspectorViewReadFunc func(context.Context, sessions.ViewTarget, string) (*sessions.SessionView, error)

func (f inspectorViewReadFunc) ReadView(ctx context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	return f(ctx, target, known)
}

func TestInspectorLiveSnapshotPairing(t *testing.T) {
	for _, name := range []string{"matched", "unchanged", "checkpoint raced", "generation checkpoint raced", "settlement checkpoint raced", "invalid saved boundary", "wrong session", "wrong execution", "wrong generation", "not ready", "absent", "settled", "incomplete", "truncated"} {
		t.Run(name, func(t *testing.T) {
			identity := inspectorLiveIdentity{"execution", 3}
			live := swarm.LiveTranscript{SessionID: "member", Execution: identity.execution, Generation: identity.generation, Epoch: 4, Revision: 9, Ready: true, BaseSequence: 7, Messages: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "not saved yet"}}}
			info := &sessions.SessionView{ID: "member", Revision: "saved", Sequence: 7}
			available := true
			wantLive := name == "matched" || name == "unchanged" || name == "truncated"
			switch name {
			case "unchanged":
				info.Unchanged = true
			case "checkpoint raced":
				info.Sequence++
			case "generation checkpoint raced", "settlement checkpoint raced":
				info.Metadata = &sessions.Metadata{Transcript: &sessions.TranscriptIdentity{Execution: identity.execution, Generation: identity.generation + 1, Sequence: info.Sequence}}
				live.Settled = name == "settlement checkpoint raced"
			case "invalid saved boundary":
				info.Metadata = &sessions.Metadata{Transcript: &sessions.TranscriptIdentity{Execution: identity.execution, Generation: identity.generation, Sequence: info.Sequence - 1}}
			case "wrong session":
				live.SessionID = "replacement"
			case "wrong execution":
				live.Execution = "previous"
			case "wrong generation":
				live.Generation--
			case "not ready":
				live.Ready = false
			case "absent":
				available = false
			case "settled":
				live.Settled = true
			case "incomplete":
				live.Settled, live.Incomplete = true, true
			case "truncated":
				live.Truncated = true
			}
			var order []string
			provider := inspectorLiveReadFunc(func(id string) (swarm.LiveTranscript, bool) {
				if id != "member" {
					t.Fatalf("copied an unselected member: %q", id)
				}
				order = append(order, "live")
				return live, available
			})
			store := inspectorViewReadFunc(func(_ context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
				order = append(order, "saved")
				if target.ID != "member" || known != "saved" {
					t.Fatalf("unexpected saved request: %+v %q", target, known)
				}
				return info, nil
			})
			got, tail, status, err := readInspectorConversation(context.Background(), store, sessions.ViewTarget{ID: "member"}, "saved", provider, identity)
			if err != nil || got != info || strings.Join(order, ",") != "live,saved" {
				t.Fatalf("read pair: info=%+v order=%v err=%v", got, order, err)
			}
			if status.active != wantLive || (len(tail.Messages) > 0) != wantLive {
				t.Fatalf("unsafe/missing overlay: status=%+v tail=%+v", status, tail)
			}
			if status.incomplete != (name == "incomplete") || status.truncated != (name == "truncated") {
				t.Fatalf("missing source limit notice: %+v", status)
			}
		})
	}
}

func TestInspectorLiveAbsentStillReadsSavedAndInvalidatesRevision(t *testing.T) {
	info := &sessions.SessionView{ID: "member", Revision: "saved", Sequence: 4, Unchanged: true}
	old := inspectorConversationRevision(info, swarm.LiveTranscript{Execution: "e", Generation: 1, Epoch: 2, Revision: 3}, inspectorLiveStatus{active: true})
	reads := 0
	store := inspectorViewReadFunc(func(context.Context, sessions.ViewTarget, string) (*sessions.SessionView, error) {
		reads++
		return info, nil
	})
	absent := inspectorLiveReadFunc(func(string) (swarm.LiveTranscript, bool) { return swarm.LiveTranscript{}, false })
	got, live, status, err := readInspectorConversation(context.Background(), store, sessions.ViewTarget{ID: info.ID}, "saved", absent, inspectorLiveIdentity{"e", 1})
	if err != nil || reads != 1 || status.active || inspectorConversationRevision(got, live, status) == old {
		t.Fatalf("absent snapshot reused provisional render: reads=%d status=%+v err=%v", reads, status, err)
	}
	store = inspectorViewReadFunc(func(context.Context, sessions.ViewTarget, string) (*sessions.SessionView, error) {
		return nil, errors.New("read failed")
	})
	if _, _, _, err := readInspectorConversation(context.Background(), store, sessions.ViewTarget{}, "", absent, inspectorLiveIdentity{}); err == nil {
		t.Fatal("storage error silently retained provisional output")
	}
}

func TestInspectorLiveRevisionFencesRebind(t *testing.T) {
	info := &sessions.SessionView{Revision: "saved"}
	live := swarm.LiveTranscript{Execution: "execution", Generation: 1, Epoch: 1, Revision: 2}
	status := inspectorLiveStatus{active: true}
	before := inspectorConversationRevision(info, live, status)
	live.Epoch++
	if after := inspectorConversationRevision(info, live, status); after == before {
		t.Fatal("rebind reused previous render despite equal live revision")
	}
}

func TestInspectorLiveMetadataToolsUseOnlyHostIdentity(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt("task")
	call := messages.ChatMessageToolCall{ID: "provider-reused", DisplayID: "host-first", Name: "bash", Arguments: `{"command":"pwd"}`}
	testToolStart(m, call)
	testToolEnd(m, call, "done", nil)
	settled := m.currentTurnToolRecord()
	second, legacy := call, call
	second.DisplayID, legacy.DisplayID = "host-second", ""
	m.adoptLiveTools([]swarm.LiveToolActivity{{Call: call, Started: time.Now()}, {Call: second, Started: time.Now()}, {Call: legacy, Started: time.Now()}})
	record := m.currentTurnToolRecord()
	if record == nil || record == settled || len(settled.rows) != 1 || !settled.rows[0].settled || len(record.rows) != 2 || record.rows[0].settled || record.rows[1].settled {
		t.Fatalf("metadata merged provider-ID siblings or repeated the completed row: settled=%+v running=%+v", settled, record)
	}
	if record.rows[0].displayID != "host-second" || record.rows[1].displayID != "" || len(m.activeTools) != 2 {
		t.Fatalf("live rows lost their identities: %+v active=%+v", record.rows, m.activeTools)
	}
	if a, b := record.rows[0], record.rows[1]; !a.batchStart || b.batchStart || a.step != settled.rows[0].step+1 || b.step != a.step {
		t.Fatalf("runtime calls did not open one new step/batch: %+v", record.rows)
	}
}

func TestInspectorRuntimeBatchKeepsFoldWhenProposalAddsLeadingCall(t *testing.T) {
	const width = 100
	prior, missed, running := readCall("prior"), readCall("missed"), editCall("running")
	prior.DisplayID, missed.DisplayID, running.DisplayID = "host-prior", "host-missed", "host-running"
	result := continuationMessage(prior.DisplayID, messages.MessageRoleTool, "read complete")
	result.ToolCallID, result.ToolName = prior.ID, prior.Name
	result.SetToolSucceeded(true)
	history := append(messages.User("Task"), messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{prior}}, result)
	saved := newReplModel()
	saved.hydrateHistory(history, "worker")
	live := []swarm.LiveToolActivity{{Call: running, Started: time.Now()}}
	fallback := childDisplayCopy(saved)
	fallback.adoptLiveTools(live)
	choice := viewState{depth: depthExpanded}
	applyViewSections(fallback, choice)
	if keys, open := foldStates(fallback, width); !slices.Equal(keys, []string{"call:host-prior", "call:host-running"}) || !slices.Equal(open, []bool{true, true}) {
		t.Fatalf("runtime edit took over the older read batch: keys=%v open=%v", keys, open)
	}
	clickHeader(t, fallback, width, "call:host-running")
	rememberViewSections(fallback, &choice)

	// The batch was known only through the runtime's still-active subset;
	// the real proposal includes a leading sibling and therefore a new key.
	proposal := messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{missed, running}}
	projected := prepareLiveDisplay(saved, []messages.ChatMessage{proposal})
	projected.adoptLiveTools(live)
	applyViewSections(projected, choice)
	if keys, open := foldStates(projected, width); !slices.Equal(keys, []string{"call:host-prior", "call:host-missed"}) || !slices.Equal(open, []bool{true, false}) {
		t.Fatalf("manual fold did not follow the same runtime call to its proposal: keys=%v open=%v", keys, open)
	}
	batch := projected.currentTurnToolRecord()
	if len(batch.rows) != 3 || len(projected.activeTools) != 1 || batch.rows[2].step != 1 || batch.rows[2].runtimeOnly {
		t.Fatalf("proposal did not replace the synthetic batch: %+v", batch.rows)
	}
	if !projected.toggleAllDisclosures(width) {
		t.Fatal("bulk disclosure did not change")
	}
	choice.depth = projected.depth
	rememberViewSections(projected, &choice)
	rebuilt := prepareLiveDisplay(saved, []messages.ChatMessage{proposal})
	applyViewSections(rebuilt, choice)
	if _, open := foldStates(rebuilt, width); len(choice.runtimeFolds) != 0 || !slices.Equal(open, []bool{false, false}) {
		t.Fatalf("bulk state did not clear the provisional choice: aliases=%v open=%v", choice.runtimeFolds, open)
	}
}

func TestInspectorLiveSourceAndLimitsInHeader(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "child")
	child := r.visibleTab()
	r.showTab(0)
	r.inspect(tabViewTarget(child))
	v := waitInspector(t, r, 140)
	for _, status := range []inspectorLiveStatus{{}, {active: true}, {active: true, truncated: true}, {incomplete: true}} {
		v.live = status
		header := r.inspectorHeader(100, 20, 0, 0)
		text := plainStyledText(header.text)
		if strings.Contains(text, "Live transcript") || strings.Contains(text, "same process") || strings.Contains(text, "\n\n") {
			t.Fatalf("routine live label or blank row remains: %q", text)
		}
		if headerSaved(header.text) == status.active || strings.Contains(text, "buffer limit") != status.truncated || strings.Contains(text, "incomplete") != status.incomplete {
			t.Fatalf("status=%+v header=%q", status, text)
		}
	}
}

func TestInspectorLiveContinuationKeepsReadingAndDisclosureAtCheckpoint(t *testing.T) {
	withDisplayTTY(t)
	user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "assignment"}
	call := messages.ChatMessageToolCall{ID: "call", DisplayID: "host-call", Name: "bash", Arguments: `{"command":"pwd"}`}
	assistant := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: strings.Repeat("live line with stable anchor\n", 30), ToolCalls: []messages.ChatMessageToolCall{call}, Metadata: map[string]any{messages.MetadataKeyDisplayID: "host-message"}}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "tool output", Metadata: map[string]any{messages.MetadataKeyDisplayID: "host-result"}}
	result.SetToolSucceeded(true)
	info := &sessions.SessionView{Metadata: &sessions.Metadata{Name: "child"}, History: []messages.ChatMessage{user}}
	base := prepareChildDisplay(info, &Config{}, 70)
	before := fmt.Sprint(transcriptTexts(base))
	live := prepareLiveDisplay(base, []messages.ChatMessage{assistant, result})
	live.renderPendingMarkdown()
	live.transcriptRows(70)
	state := &viewState{top: 5, follow: false, depth: depthExpanded, sections: map[string]viewSection{"tool:host-call": {outputExpanded: true}}}
	applyViewSections(live, *state)
	live.transcriptRows(70)
	rememberViewPosition(live, state)
	anchor := state.anchor
	if anchor.block == "" {
		t.Fatal("live display has no reading anchor")
	}
	info.History = []messages.ChatMessage{user, assistant, result}
	durable := prepareChildDisplay(info, &Config{}, 70)
	applyViewSections(durable, *state)
	durable.transcriptRows(70)
	restoreViewPosition(durable, state)
	rememberViewPosition(durable, state)
	if state.follow || state.anchor != anchor {
		t.Fatalf("checkpoint moved reading position: before=%+v after=%+v", anchor, state.anchor)
	}
	row := swarmTestRow(t, durable, call.ID)
	if !row.outputExpanded || row.sectionKey != "tool:host-call" {
		t.Fatalf("checkpoint lost manual tool disclosure: %+v", row)
	}
	if got := fmt.Sprint(transcriptTexts(base)); got != before {
		t.Fatal("live projection mutated immutable saved display")
	}
}
