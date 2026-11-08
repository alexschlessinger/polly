package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func nativeDisplayFixture(t *testing.T, history []messages.ChatMessage) (*sessions.SQLiteStore, sessions.Session, sessions.ViewTarget) {
	t.Helper()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session, err := store.Acquire(t.Context(), "display-window", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := session.AddMessages(t.Context(), history); err != nil {
		t.Fatal(err)
	}
	return store, session, sessions.ViewTarget{ID: session.(sessions.ViewIdentity).ViewID()}
}

func TestNativeDisplayWindowPayloadIndependentOfOldProse(t *testing.T) {
	var small, large runtimemetrics.Snapshot
	for _, old := range []int{2, 2000} {
		var history []messages.ChatMessage
		for n := 0; n < old; n++ {
			history = append(history, messages.ChatMessage{Role: "user", Content: "old prompt"}, messages.ChatMessage{Role: "assistant", Content: strings.Repeat("unrelated old answer ", 100)})
		}
		for n := 0; n < 6; n++ {
			history = append(history, messages.ChatMessage{Role: "user", Content: fmt.Sprintf("visible %d", n)}, messages.ChatMessage{Role: "assistant", Content: "answer"})
		}
		store, _, target := nativeDisplayFixture(t, history)
		recorder := runtimemetrics.New()
		ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
		var header *sessions.SessionViewHeader
		info, err := readSessionDisplay(ctx, store, target, "", &header)
		if err != nil {
			t.Fatal(err)
		}
		if info.Display == nil || len(info.History) != 10 || len(info.Display.Prefix) != 0 || info.Display.Previous == nil || info.Display.Previous.Message.Content != "visible 0" {
			t.Fatalf("window %+v history %d", info.Display, len(info.History))
		}
		if old == 2 {
			small = recorder.Snapshot()
		} else {
			large = recorder.Snapshot()
		}
		recorder = runtimemetrics.New()
		ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
		unchanged, err := readSessionDisplay(ctx, store, target, info.Revision, &header)
		if err != nil || !unchanged.Unchanged || recorder.Snapshot().Count(runtimemetrics.TranscriptMessagesRead) != 0 {
			t.Fatalf("unchanged %+v %v", unchanged, err)
		}
		recorder = runtimemetrics.New()
		ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
		progress, _, _, err := readInspectorProgressCached(ctx, store, target, "", nil, inspectorLiveIdentity{}, nil)
		if err != nil || len(progress.History) != 2 || len(progress.Display.Prefix) != 0 || progress.Display.Previous != nil {
			t.Fatalf("progress %+v %v", progress, err)
		}
		if got := recorder.Snapshot().Count(runtimemetrics.TranscriptMessagesRead); got != 2 {
			t.Fatalf("progress read %d messages", got)
		}
	}
	for _, counter := range []runtimemetrics.Counter{runtimemetrics.TranscriptMessagesRead, runtimemetrics.RowsRead, runtimemetrics.RecordsDecoded, runtimemetrics.PayloadBytesRead} {
		// Identity/revision digit lengths may vary slightly; historical payload
		// work must not grow with unrelated old turns or their body sizes.
		if large.Count(counter) > small.Count(counter)+32 {
			t.Fatalf("old prose grew counter %d: %d -> %d", counter, small.Count(counter), large.Count(counter))
		}
	}
}

func TestNativeDisplayWindowMatchesFullHydration(t *testing.T) {
	call := messages.ChatMessageToolCall{ID: "repeat", Name: "bash", Arguments: `{"command":"old"}`}
	first := messages.ChatMessage{Role: "assistant", Reasoning: strings.Repeat("old thought", 100), ToolCalls: []messages.ChatMessageToolCall{call}}
	result := messages.ChatMessage{Role: "tool", ToolCallID: call.ID, ToolName: call.Name, Content: strings.Repeat("old tool output", 1000), Metadata: map[string]any{"tool_succeeded": true, "tool_data": map[string]any{"changes": map[string]any{"tracked": true, "changes": []any{map[string]any{"path": "a.go", "additions": 3, "deletions": 1, "diff": strings.Repeat("old diff", 1000)}}}}}}
	image := func(id string) messages.ContentPart {
		return messages.ContentPart{Type: "image_artifact", Reference: "[image #7]", Artifact: &artifacts.Ref{ID: id, Kind: artifacts.KindImage, Name: "old", ImageToken: "[image #7]"}}
	}
	history := []messages.ChatMessage{{Role: "user", Content: "Original assignment", Parts: []messages.ContentPart{image(strings.Repeat("a", 64))}}, first, result, {Role: "assistant", Content: "prose barrier"}, {Role: "assistant", Reasoning: "separate thought"}, agentLaunchMarker(agentLaunch{CallID: "old-launch", Label: "older agent"}), {Role: "user", Content: "Prior assignment", Parts: []messages.ContentPart{image(strings.Repeat("b", 64))}}, {Role: "assistant", Content: "done"}}
	// This report and the five context imports are real user turns, but must
	// not overwrite the previous assignment. Imported bodies never enter prefix.
	history = append(history, messages.ChatMessage{Role: "user", Content: "agent old finished", Metadata: map[string]any{messages.MetadataKeyAgentReport: true}}, messages.ChatMessage{Role: "assistant", Content: "notice done"})
	for n := 0; n < 5; n++ {
		history = append(history, messages.ChatMessage{Role: "user", Content: "context", Metadata: map[string]any{messages.MetadataKeyContextImport: true}}, messages.ChatMessage{Role: "assistant", Content: fmt.Sprintf("visible answer %d", n)})
	}
	history = append(history, (messages.Compaction{Summary: "saved compaction", KeepsTurn: true}).Message())
	// Synthetic admissions must not shrink the selected five-turn window.
	history = append(history, messages.ChatMessage{Role: "user", Content: "mail", Metadata: map[string]any{messages.MetadataKeySwarmMessages: []string{"receipt"}}}, messages.ChatMessage{Role: "assistant", Reasoning: "latest thought", ToolCalls: []messages.ChatMessageToolCall{{ID: "repeat", DisplayID: "latest", Name: "bash", Arguments: `{"command":"new"}`}}})
	store, _, target := nativeDisplayFixture(t, history)
	full, err := store.ReadView(t.Context(), target, "")
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := readSessionDisplay(t.Context(), store, target, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := prepareChildDisplay(full, &Config{}, 100)
	got := prepareChildDisplay(bounded, &Config{}, 100)
	if a, b := continuationText(got), continuationText(want); a != b {
		t.Fatalf("visible mismatch\ngot %s\nwant %s", a, b)
	}
	if got.assignment != want.assignment || got.assignment != "Prior assignment [attached: attachment]" || got.attachmentSeq != want.attachmentSeq || !reflect.DeepEqual(got.ambiguousAttachments, want.ambiguousAttachments) || !reflect.DeepEqual(got.unshownCallIDs, want.unshownCallIDs) {
		t.Fatalf("prefix state differs: assignment %q/%q attachments %v/%v calls %v/%v", got.assignment, want.assignment, got.ambiguousAttachments, want.ambiguousAttachments, got.unshownCallIDs, want.unshownCallIDs)
	}
	if !reflect.DeepEqual(got.displayCatalog.thoughts, want.displayCatalog.thoughts) || len(got.displayCatalog.tools) != len(want.displayCatalog.tools) {
		t.Fatalf("catalog differs: thoughts %v/%v tools %d/%d", got.displayCatalog.thoughts, want.displayCatalog.thoughts, len(got.displayCatalog.tools), len(want.displayCatalog.tools))
	}
	for i, tool := range got.displayCatalog.tools {
		expected := want.displayCatalog.tools[i]
		if tool.key != expected.key || tool.call != expected.call || tool.pres.outcome != expected.pres.outcome || tool.pres.counts != expected.pres.counts {
			t.Fatalf("tool %d differs: %+v/%+v", i, tool, expected)
		}
	}
	ga, gd, gf := sessionChangeStats(got.displayCatalog.tools)
	wa, wd, wf := sessionChangeStats(want.displayCatalog.tools)
	if ga != wa || gd != wd || gf != wf {
		t.Fatal("historical change totals lost")
	}
	progress, _, _, err := readInspectorProgressCached(t.Context(), store, target, "", nil, inspectorLiveIdentity{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	progressModel := prepareChildDisplay(progress, &Config{}, 100)
	if !reflect.DeepEqual(progressModel.agentProgressSummary(100), want.agentProgressSummary(100)) {
		t.Fatal("latest-turn progress differs from complete hydration")
	}
	// Continuation must settle exactly the saved latest call, not the repeated
	// provider ID in the omitted prefix, and must keep reasoning keys stable.
	tail := []messages.ChatMessage{continuationMessage("latest", "tool", llm.ToolDeniedContent), continuationMessage("new-thought", "assistant", "The answer")}
	tail[0].ToolCallID, tail[0].ToolName = "repeat", "bash"
	if a, b := continuationText(prepareLiveDisplay(got, tail)), continuationText(prepareLiveDisplay(want, tail)); a != b {
		t.Fatalf("continuation differs\n%s\n%s", a, b)
	}
}

func TestNativeDisplayPrefixPreservesCommentaryBoundaries(t *testing.T) {
	history := append(messages.User("old"), messages.ChatMessage{Role: "assistant", Reasoning: "first"}, messages.ChatMessage{Role: "assistant", TextBlocks: []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "interim"}}}, messages.ChatMessage{Role: "assistant", Reasoning: "second"})
	for n := 0; n < 5; n++ {
		history = append(history, messages.ChatMessage{Role: "user", Content: "new"}, messages.ChatMessage{Role: "assistant", Content: "done"})
	}
	store, _, target := nativeDisplayFixture(t, history)
	full, err := store.ReadView(t.Context(), target, "")
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := readSessionDisplay(t.Context(), store, target, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, commentary := range []bool{false, true} {
		want, got := newReplModel(), newReplModel()
		want.showCommentary, got.showCommentary = commentary, commentary
		want.hydrateHistory(full.History, "test")
		got.hydrateHistoryWindow(bounded.History, "test", bounded.Display)
		if !reflect.DeepEqual(got.displayCatalog.thoughts, want.displayCatalog.thoughts) {
			t.Fatalf("commentary=%v thoughts %v/%v", commentary, got.displayCatalog.thoughts, want.displayCatalog.thoughts)
		}
	}
}

type startupHistoryCounter struct {
	sessions.Session
	histories int
}

func (s *startupHistoryCounter) ViewID() string { return s.Session.(sessions.ViewIdentity).ViewID() }
func (s *startupHistoryCounter) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	s.histories++
	return s.Session.GetHistory(ctx)
}

func TestNativeTabStartupCountedWindowAndExactEstimateFallback(t *testing.T) {
	for _, counted := range []bool{false, true} {
		t.Run(fmt.Sprint(counted), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "startup")
			var history []messages.ChatMessage
			for n := 0; n < 20; n++ {
				history = append(history, messages.ChatMessage{Role: "user", Content: "question"}, messages.ChatMessage{Role: "assistant", Content: "answer"})
			}
			if counted {
				history[len(history)-1].Metadata = map[string]any{messages.MetadataKeyInputTokens: 12000, messages.MetadataKeyOutputTokens: 300}
			}
			if err := r.state.session.AddMessages(t.Context(), history); err != nil {
				t.Fatal(err)
			}
			counter := &startupHistoryCounter{Session: r.state.session}
			r.state.session = counter
			_, model, err := r.newTabModelContext(t.Context(), r.state)
			if err != nil {
				t.Fatal(err)
			}
			wantReads := 1
			if counted {
				wantReads = 0
			}
			if counter.histories != wantReads {
				t.Fatalf("counted=%v full history reads %d", counted, counter.histories)
			}
			want := newReplModel()
			want.masthead = model.masthead
			want.hydrateHistory(history, "startup")
			if continuationText(model) != continuationText(want) {
				t.Fatalf("startup visible transcript differs\ngot %s\nwant %s", continuationText(model), continuationText(want))
			}
		})
	}
}

type nonNativeDisplay struct{ *sessions.SQLiteStore }

func (s nonNativeDisplay) TranscriptCapabilities() sessions.TranscriptCapabilities {
	return sessions.TranscriptCapabilities{NativeIndexed: true, StableEpoch: true}
}
func (s nonNativeDisplay) ReadTranscriptWindow(context.Context, sessions.ViewTarget, sessions.TranscriptWindowQuery) (sessions.TranscriptWindow, error) {
	panic("unadvertised native display used")
}
func TestDisplayWindowRequiresExplicitNativeCapability(t *testing.T) {
	var history []messages.ChatMessage
	for n := 0; n < 10; n++ {
		history = append(history, messages.ChatMessage{Role: "user", Content: "q"}, messages.ChatMessage{Role: "assistant", Content: "a"})
	}
	store, _, target := nativeDisplayFixture(t, history)
	info, err := readSessionDisplay(t.Context(), nonNativeDisplay{store}, target, "", nil)
	if err != nil || info.Display != nil || len(info.History) != len(history) {
		t.Fatalf("legacy fallback %+v %v", info, err)
	}
}

type racedNativeDisplay struct {
	*sessions.SQLiteStore
	beforeWindow func()
	beforePage   func()
}

func (s *racedNativeDisplay) ReadTranscriptWindow(ctx context.Context, target sessions.ViewTarget, q sessions.TranscriptWindowQuery) (sessions.TranscriptWindow, error) {
	if s.beforeWindow != nil {
		s.beforeWindow()
		s.beforeWindow = nil
	}
	return s.SQLiteStore.ReadTranscriptWindow(ctx, target, q)
}
func (s *racedNativeDisplay) ReadTranscriptPage(ctx context.Context, target sessions.ViewTarget, q sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
	if s.beforePage != nil {
		s.beforePage()
		s.beforePage = nil
	}
	return s.SQLiteStore.ReadTranscriptPage(ctx, target, q)
}

func TestNativeDisplayWindowCheckpointFences(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			store, session, target := nativeDisplayFixture(t, append(messages.User("task"), messages.ChatMessage{Role: "assistant", Content: "saved"}))
			reader := &racedNativeDisplay{SQLiteStore: store}
			reader.beforeWindow = func() {
				if replace {
					if err := session.Clear(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if err := session.AddMessages(t.Context(), append(messages.User("new"), messages.ChatMessage{Role: "assistant", Content: "next"})); err != nil {
					t.Fatal(err)
				}
			}
			info, err := readSessionDisplay(t.Context(), reader, target, "", nil)
			if replace {
				if !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
					t.Fatalf("replacement accepted: %+v %v", info, err)
				}
				return
			}
			if err != nil || info.Sequence != 2 || len(info.History) != 2 || info.History[1].Content != "saved" {
				t.Fatalf("append crossed captured boundary: %+v %v", info, err)
			}
			reader.beforePage = func() {
				if err := session.Clear(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = readSessionDisplay(t.Context(), reader, target, "", nil); !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
				t.Fatalf("replacement after window accepted: %v", err)
			}
		})
	}
}

// The native window is still paired with the live checkpoint's exact sequence;
// sampling live first never licenses merging an older tail into a newer save.
func TestNativeDisplayWindowRejectsOldLiveTail(t *testing.T) {
	store, session, target := nativeDisplayFixture(t, messages.User("task"))
	reader := &racedNativeDisplay{SQLiteStore: store}
	live := inspectorLiveReadFunc(func(string) (swarm.LiveTranscript, bool) {
		if err := session.AddMessage(t.Context(), messages.ChatMessage{Role: "assistant", Content: "checkpoint"}); err != nil {
			t.Fatal(err)
		}
		return swarm.LiveTranscript{Ready: true, Execution: "e", Generation: 1, BaseSequence: 1}, true
	})
	_, _, status, err := readInspectorConversation(t.Context(), reader, target, "", live, inspectorLiveIdentity{"e", 1})
	if err != nil || status.active {
		t.Fatalf("stale tail paired: %+v %v", status, err)
	}
}
