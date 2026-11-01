package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

func liveMessage(role, id, text string) messages.ChatMessage {
	return messages.ChatMessage{Role: role, Content: text, Metadata: map[string]any{messages.MetadataKeyDisplayID: id}}
}

func newTranscriptTest() (*Runtime, *invocation, *llm.AgentCallbacks, *transcriptBinding) {
	i := &invocation{id: "execution", member: "member", generation: 2}
	r := &Runtime{active: map[string]*invocation{i.member: i}, notify: make(chan struct{})}
	cb := &llm.AgentCallbacks{}
	return r, i, cb, i.bindTranscript(cb, "session")
}

func transcriptSnapshot(t *testing.T, r *Runtime) LiveTranscript {
	t.Helper()
	snapshot, ok := r.LiveTranscript("member")
	if !ok {
		t.Fatal("missing transcript")
	}
	return snapshot
}

func TestLiveTranscriptOrderingRetryAndCopies(t *testing.T) {
	t.Parallel()
	r, _, cb, binding := newTranscriptTest()
	if got := transcriptSnapshot(t, r); got.Ready || got.Execution != "execution" || got.Generation != 2 || got.SessionID != "session" {
		t.Fatalf("initial snapshot: %+v", got)
	}
	if err := binding.checkpoint(llm.AgentCheckpoint{}, func() (int64, error) { return 3, nil }); err != nil {
		t.Fatal(err)
	}
	if got := transcriptSnapshot(t, r); got.Ready {
		t.Fatal("base ready before first request checkpoint")
	}
	if err := binding.checkpoint(llm.AgentCheckpoint{Request: true}, func() (int64, error) { return 4, nil }); err != nil {
		t.Fatal(err)
	}
	cb.OnModelRequest(0, 0)
	cb.OnMessageStart("assistant")
	cb.OnReasoning("thinking")
	cb.OnCommentary(messages.AssistantText{ID: "comment", Text: "working"}, true)
	cb.OnContent("partial")
	streamed := transcriptSnapshot(t, r)
	if len(streamed.Messages) != 1 || streamed.Messages[0].Content != "partial" || streamed.Messages[0].Reasoning != "thinking" || !streamed.Messages[0].HasTextBlocks() || streamed.ProvisionalDisplayID != "assistant" || streamed.BaseSequence != 4 {
		t.Fatalf("streamed message: %+v", streamed)
	}
	calls := []messages.ChatMessageToolCall{{ID: "a", DisplayID: "tool-a", Name: "read_file", Arguments: `{"path":"a"}`}, {ID: "b", DisplayID: "tool-b", Name: "read_file"}}
	assistant := liveMessage(messages.MessageRoleAssistant, "assistant", "accepted")
	assistant.ToolCalls = calls
	cb.OnMessage(0, assistant)
	ref := &artifacts.Ref{ID: "sha256:abc", Kind: artifacts.KindText, Name: "output.txt"}
	resultB := liveMessage(messages.MessageRoleTool, "tool-b", "second")
	resultB.ToolCallID, resultB.ToolName = "b", "read_file"
	resultB.Parts = []messages.ContentPart{{Type: "artifact", Artifact: ref}}
	resultA := liveMessage(messages.MessageRoleTool, "tool-a", "first")
	resultA.ToolCallID = "a"
	cb.OnToolResult(calls[1], resultB) // finishes before the first declared call
	cb.OnToolResult(calls[0], resultA)
	pending := transcriptSnapshot(t, r)
	if len(pending.Messages) != 3 || pending.Messages[1].ToolCallID != "a" || pending.Messages[2].ToolCallID != "b" || pending.ProvisionalDisplayID != "" {
		t.Fatalf("parallel results not in declaration order: %+v", pending)
	}
	cb.OnMessage(1, resultA)
	cb.OnMessage(2, resultB)
	// Mutating inputs and independent snapshots must never mutate the buffer.
	ref.Name = "input mutation"
	assistant.ToolCalls[0].Name = "input mutation"
	pending.Messages[0].ToolCalls[0].Arguments = "snapshot mutation"
	pending.Messages[2].Parts[0].Artifact.Name = "snapshot mutation"
	pending.Messages[2].Metadata[messages.MetadataKeyDisplayID] = "snapshot mutation"
	fresh := transcriptSnapshot(t, r)
	if len(fresh.Messages) != 3 || fresh.Messages[0].ToolCalls[0].Name != "read_file" || fresh.Messages[0].ToolCalls[0].Arguments != `{"path":"a"}` || fresh.Messages[2].Parts[0].Artifact.Name != "output.txt" || fresh.Messages[2].DisplayID() != "tool-b" {
		t.Fatalf("snapshot alias or canonical duplicate: %+v", fresh)
	}
	cb.OnModelRequest(1, 0)
	cb.OnMessageStart("failed-attempt")
	cb.OnContent("discard only this")
	cb.OnAttemptAborted(1, 0, errors.New("retry"))
	after := transcriptSnapshot(t, r)
	if len(after.Messages) != 3 || after.ProvisionalDisplayID != "" {
		t.Fatalf("abort removed accepted work: %+v", after)
	}
	cb.OnModelRequest(1, 1)
	cb.OnMessageStart("retry")
	cb.OnContent("retry partial")
	cb.OnAttemptAborted(1, 0, errors.New("late abort"))
	if got := transcriptSnapshot(t, r); got.ProvisionalDisplayID != "retry" || len(got.Messages) != 4 {
		t.Fatalf("stale abort: %+v", got)
	}
}

func TestLiveTranscriptToolResultOrderUsesHostIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		calls []messages.ChatMessageToolCall
	}{
		{"repeated provider IDs", []messages.ChatMessageToolCall{{ID: "repeat", DisplayID: "first"}, {ID: "repeat", DisplayID: "second"}}},
		{"missing provider IDs", []messages.ChatMessageToolCall{{DisplayID: "first"}, {DisplayID: "second"}}},
		{"legacy provider IDs", []messages.ChatMessageToolCall{{ID: "first"}, {ID: "second"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, cb, _ := newTranscriptTest()
			declaration := liveMessage(messages.MessageRoleAssistant, "batch", "")
			declaration.ToolCalls = tc.calls
			cb.OnMessage(0, declaration)
			for n := len(tc.calls) - 1; n >= 0; n-- {
				result := liveMessage(messages.MessageRoleTool, tc.calls[n].DisplayID, fmt.Sprint(n))
				result.ToolCallID = tc.calls[n].ID
				cb.OnToolResult(tc.calls[n], result)
			}
			got := transcriptSnapshot(t, r)
			if len(got.Messages) != 3 || got.Messages[1].Content != "0" || got.Messages[2].Content != "1" {
				t.Fatalf("parallel results lost declaration order: %+v", got.Messages)
			}
		})
	}
}

func TestLiveTranscriptCheckpointFenceAndAdmittedObserver(t *testing.T) {
	t.Parallel()
	r, i, cb, binding := newTranscriptTest()
	accepted := liveMessage(messages.MessageRoleAssistant, "accepted", "not yet committed")
	cb.OnMessage(0, accepted)
	before := transcriptSnapshot(t, r)
	failed := errors.New("rolled back")
	if err := binding.checkpoint(llm.AgentCheckpoint{Generated: []messages.ChatMessage{accepted}, Request: true}, func() (int64, error) { return 20, failed }); err != failed {
		t.Fatal(err)
	}
	if after := transcriptSnapshot(t, r); !reflect.DeepEqual(after, before) {
		t.Fatalf("rollback changed live state: %+v => %+v", before, after)
	}
	admitted := liveMessage(messages.MessageRoleUser, "mail", "admitted")
	generated := []messages.ChatMessage{accepted, admitted}
	if err := binding.checkpoint(llm.AgentCheckpoint{Generated: generated, Request: true}, func() (int64, error) { return 8, nil }); err != nil {
		t.Fatal(err)
	}
	cb.OnMessage(1, admitted) // Agent appends admitted input after its checkpoint.
	if got := transcriptSnapshot(t, r); len(got.Messages) != 0 || !got.Ready || got.BaseSequence != 8 {
		t.Fatalf("admitted input duplicated: %+v", got)
	}
	next := &llm.AgentCallbacks{}
	i.bindTranscript(next, "session")
	next.OnMessageStart("current")
	next.OnContent("current")
	before = transcriptSnapshot(t, r)
	cb.OnMessageStart("stale")
	cb.OnContent("stale")
	cb.OnMessage(2, liveMessage(messages.MessageRoleAssistant, "stale", "stale"))
	cb.OnAttemptAborted(0, 0, failed)
	if err := binding.checkpoint(llm.AgentCheckpoint{}, func() (int64, error) { t.Error("stale checkpoint committed"); return 0, nil }); err == nil {
		t.Fatal("stale checkpoint was not fenced")
	}
	if after := transcriptSnapshot(t, r); !reflect.DeepEqual(after, before) {
		t.Fatalf("old epoch changed transcript: %+v => %+v", before, after)
	}
}

func TestLiveTranscriptRetryOptInAndNoWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cb   llm.AgentCallbacks
		want bool
	}{
		{"none", llm.AgentCallbacks{}, true},
		{"content", llm.AgentCallbacks{OnContent: func(string) {}}, false},
		{"reasoning", llm.AgentCallbacks{OnReasoning: func(string) {}}, false},
		{"commentary", llm.AgentCallbacks{OnCommentary: func(messages.AssistantText, bool) {}}, false},
		{"abort", llm.AgentCallbacks{OnContent: func(string) {}, OnAttemptAborted: func(int, int, error) {}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, i, _, _ := newTranscriptTest()
			i.bindTranscript(&tc.cb, "session")
			i.bindActivity(&tc.cb, 0, 0)
			if (tc.cb.OnAttemptAborted != nil) != tc.want {
				t.Fatalf("abort permission changed: %+v", tc.cb)
			}
			tc.cb.OnModelRequest(0, 0)
			tc.cb.OnMessageStart("text")
			tc.cb.OnContent("does not wake parked workers")
			select {
			case <-r.notify:
				t.Fatal("text changed runtime notification")
			default:
			}
		})
	}
}

func TestLiveTranscriptBoundsPreserveToolIdentity(t *testing.T) {
	t.Parallel()
	r, _, cb, _ := newTranscriptTest()
	start := liveMessage(messages.MessageRoleAssistant, "tools", "")
	start.ToolCalls = []messages.ChatMessageToolCall{{ID: "active-call", DisplayID: "active-display", Name: "read_file"}}
	cb.OnMessage(0, start)
	largeText, binary, metadata := strings.Repeat("x", 20<<10), strings.Repeat("b", 1<<20), strings.Repeat("m", 1<<20)
	for n := 1; n < 3*liveTranscriptMessages; n++ {
		msg := liveMessage(messages.MessageRoleTool, fmt.Sprintf("result-%d", n), largeText)
		msg.Parts = []messages.ContentPart{{Type: "image_base64", ImageData: binary, Artifact: &artifacts.Ref{ID: "sha256:abc", Kind: artifacts.KindImage}}}
		msg.Metadata["nested"] = map[string]any{"huge": metadata}
		cb.OnMessage(n, msg)
	}
	cb.OnMessageStart("partial")
	cb.OnContent(strings.Repeat("p", 1<<20))
	got := transcriptSnapshot(t, r)
	if !got.Truncated || len(got.Messages) > liveTranscriptMessages || got.Messages[0].DisplayID() != "tools" || got.Messages[0].ToolCalls[0].DisplayID != "active-display" {
		t.Fatalf("bounds lost tool start or marker: count=%d first=%+v truncated=%v", len(got.Messages), got.Messages[0], got.Truncated)
	}
	bytes := 0
	for _, msg := range got.Messages {
		_, count, _ := boundedLiveMessage(msg)
		bytes += count
		if msg.Metadata["nested"] != nil {
			t.Fatal("retained nested metadata")
		}
		for _, part := range msg.Parts {
			if part.ImageData != "" || part.ImageURL != "" {
				t.Fatal("retained binary data")
			}
			if part.Artifact == nil || part.Artifact.ID != "sha256:abc" {
				t.Fatal("lost artifact reference")
			}
		}
	}
	if bytes > liveTranscriptBytes {
		t.Fatalf("retained %d text bytes", bytes)
	}
}

func TestLiveTranscriptLargePayloadPreservesBatchAndArtifactIdentities(t *testing.T) {
	t.Parallel()
	msg := liveMessage(messages.MessageRoleAssistant, "batch", "")
	for n := range liveMessageItems {
		msg.ToolCalls = append(msg.ToolCalls, messages.ChatMessageToolCall{ID: fmt.Sprint(n), DisplayID: fmt.Sprintf("call-%d", n), Name: "read_file", Arguments: strings.Repeat("a", 10<<10)})
	}
	msg.Parts = []messages.ContentPart{{Type: "artifact", Text: strings.Repeat("t", 10<<10), Artifact: &artifacts.Ref{ID: "sha256:abc", Kind: artifacts.KindText, Name: "output"}}}
	bounded, size, truncated := boundedLiveMessage(msg)
	if !truncated || size > liveMessageBytes || len(bounded.ToolCalls) != liveMessageItems || bounded.Parts[0].Artifact.ID != "sha256:abc" {
		t.Fatalf("bounded payload: %+v bytes=%d truncated=%v", bounded, size, truncated)
	}
	for n, call := range bounded.ToolCalls {
		if call.ID != fmt.Sprint(n) || call.DisplayID != fmt.Sprintf("call-%d", n) || call.Name != "read_file" {
			t.Fatalf("large arguments erased active call %d: %+v", n, call)
		}
	}
}

func TestLiveTranscriptBoundsReserveEveryToolIdentity(t *testing.T) {
	t.Parallel()
	for _, hostIdentity := range []bool{true, false} {
		t.Run(fmt.Sprintf("host=%v", hostIdentity), func(t *testing.T) {
			r, _, cb, _ := newTranscriptTest()
			msg := liveMessage(messages.MessageRoleAssistant, "batch", "")
			for n := range liveMessageItems {
				call := messages.ChatMessageToolCall{ID: fmt.Sprintf("%0256d", n), Name: strings.Repeat("n", 256)}
				if hostIdentity {
					call.DisplayID = fmt.Sprintf("%032d", n)
				}
				msg.ToolCalls = append(msg.ToolCalls, call)
			}
			bounded, size, truncated := boundedLiveMessage(msg)
			cb.OnMessage(0, msg)
			if got := transcriptSnapshot(t, r); !got.Truncated {
				t.Fatal("snapshot omitted truncation marker")
			}
			if !truncated || len(bounded.ToolCalls) != liveMessageItems {
				t.Fatalf("bounded call count=%d truncated=%v", len(bounded.ToolCalls), truncated)
			}
			bytes := len(bounded.Role) + len(bounded.DisplayID())
			for n, call := range bounded.ToolCalls {
				if hostIdentity && call.DisplayID != msg.ToolCalls[n].DisplayID || !hostIdentity && call.ID != msg.ToolCalls[n].ID {
					t.Fatalf("call %d lost its primary identity: %+v", n, call)
				}
				bytes += len(call.ID) + len(call.DisplayID) + len(call.Name) + len(call.Arguments)
			}
			if bytes != size || bytes > liveMessageBytes {
				t.Fatalf("text budget counted=%d reported=%d limit=%d", bytes, size, liveMessageBytes)
			}
		})
	}
}

func TestLiveTranscriptBoundsReserveEveryArtifactHandle(t *testing.T) {
	t.Parallel()
	r, _, cb, _ := newTranscriptTest()
	msg := liveMessage(messages.MessageRoleAssistant, "batch", strings.Repeat("c", 8<<10))
	descriptor := strings.Repeat("d", 256)
	for n := range liveMessageItems {
		msg.ToolCalls = append(msg.ToolCalls, messages.ChatMessageToolCall{ID: descriptor, DisplayID: fmt.Sprintf("%0256d", n), Name: descriptor, Arguments: descriptor})
		msg.Parts = append(msg.Parts, messages.ContentPart{
			Type: "artifact", Text: descriptor, MimeType: descriptor, FileName: descriptor, Reference: descriptor,
			Artifact: &artifacts.Ref{ID: fmt.Sprintf("sha256:%064x", n), Kind: artifacts.KindText, MIMEType: descriptor, Name: descriptor, ImageToken: descriptor, Reference: descriptor, Bytes: int64(n + 1), Lines: n + 1},
		})
	}
	bounded, size, truncated := boundedLiveMessage(msg)
	cb.OnMessage(0, msg)
	if got := transcriptSnapshot(t, r); !got.Truncated {
		t.Fatal("snapshot omitted truncation marker")
	}
	if !truncated || len(bounded.ToolCalls) != liveMessageItems || len(bounded.Parts) != liveMessageItems {
		t.Fatalf("calls=%d parts=%d truncated=%v", len(bounded.ToolCalls), len(bounded.Parts), truncated)
	}
	for n, part := range bounded.Parts {
		if bounded.ToolCalls[n].DisplayID != msg.ToolCalls[n].DisplayID {
			t.Fatalf("lost primary call identity %d", n)
		}
		if part.Artifact == nil || !artifacts.ValidID(part.Artifact.ID) || part.Artifact.ID != msg.Parts[n].Artifact.ID {
			t.Fatalf("lost primary artifact handle %d: %+v", n, part.Artifact)
		}
		if part.Type != "artifact" || part.Artifact.Kind != artifacts.KindText || part.Artifact.Bytes != int64(n+1) || part.Artifact.Lines != n+1 {
			t.Fatalf("lost artifact type or size %d: %+v", n, part)
		}
	}
	if bytes := liveMessageStringBytes(bounded); bytes != size || bytes > liveMessageBytes {
		t.Fatalf("text budget counted=%d reported=%d limit=%d", bytes, size, liveMessageBytes)
	}
}

func TestLiveTranscriptBoundsOmitUnrepresentableArtifactParts(t *testing.T) {
	t.Parallel()
	ref := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindText, Data: []byte("saved")})
	msg := liveMessage(messages.MessageRoleTool, "result", "")
	msg.Parts = []messages.ContentPart{
		{Type: "text", Text: "before"},
		{Type: "artifact", Text: "oversized", Artifact: &artifacts.Ref{ID: strings.Repeat("x", 257)}},
		{Type: "artifact", Text: "after", Artifact: &ref},
		{Type: "artifact", Text: "empty", Artifact: &artifacts.Ref{}},
	}
	bounded, size, truncated := boundedLiveMessage(msg)
	if !truncated || len(bounded.Parts) != 2 {
		t.Fatalf("unrepresentable parts not omitted: %+v truncated=%v", bounded.Parts, truncated)
	}
	if bounded.Parts[0].Text != "before" || bounded.Parts[1].Text != "after" || bounded.Parts[1].Artifact == nil || bounded.Parts[1].Artifact.ID != ref.ID {
		t.Fatalf("omission mismatched part payloads: %+v", bounded.Parts)
	}
	if bytes := liveMessageStringBytes(bounded); bytes != size || bytes > liveMessageBytes {
		t.Fatalf("text budget counted=%d reported=%d limit=%d", bytes, size, liveMessageBytes)
	}
}

// Count retained string values independently of the recorder's accounting.
func liveMessageStringBytes(msg messages.ChatMessage) int {
	size := len(msg.Role) + len(msg.Content) + len(msg.ToolCallID) + len(msg.ToolName) + len(msg.Reasoning) + len(msg.StopReason)
	for _, value := range msg.Metadata {
		if text, ok := value.(string); ok {
			size += len(text)
		}
	}
	for _, call := range msg.ToolCalls {
		size += len(call.ID) + len(call.DisplayID) + len(call.Name) + len(call.Arguments)
	}
	for _, block := range msg.TextBlocks {
		size += len(block.ID) + len(block.Phase) + len(block.Text)
	}
	for _, part := range msg.Parts {
		size += len(part.Type) + len(part.Text) + len(part.ImageURL) + len(part.ImageData) + len(part.MimeType) + len(part.FileName) + len(part.Reference)
		if ref := part.Artifact; ref != nil {
			size += len(ref.ID) + len(ref.Kind) + len(ref.MIMEType) + len(ref.Name) + len(ref.ImageToken) + len(ref.Reference)
		}
	}
	return size
}

func TestLiveTranscriptSettledCancellationAndEviction(t *testing.T) {
	t.Parallel()
	r, i, cb, _ := newTranscriptTest()
	cb.OnMessageStart("canceled")
	cb.OnContent("unfinished output") // terminal errors need not call abort
	r.mu.Lock()
	r.settleTranscript(i)
	delete(r.active, i.member)
	r.mu.Unlock()
	if r.settledTranscripts[i.member] == &i.transcript {
		t.Fatal("settled cache retains the invocation allocation")
	}
	got := transcriptSnapshot(t, r)
	if !got.Settled || !got.Incomplete || got.ProvisionalDisplayID != "canceled" || got.Messages[0].Content != "unfinished output" {
		t.Fatalf("partial masquerades as committed: %+v", got)
	}
	cb.OnContent("late")
	cb.OnMessage(0, liveMessage(messages.MessageRoleAssistant, "late", "late"))
	if after := transcriptSnapshot(t, r); !reflect.DeepEqual(got, after) {
		t.Fatal("settled callbacks not fenced")
	}
	for n := range liveSettledTranscripts {
		other := &invocation{id: fmt.Sprint(n), member: fmt.Sprint(n)}
		other.bindTranscript(&llm.AgentCallbacks{}, fmt.Sprint(n))
		r.mu.Lock()
		r.settleTranscript(other)
		r.mu.Unlock()
	}
	if _, ok := r.LiveTranscript("member"); ok || len(r.settledTranscripts) != liveSettledTranscripts {
		t.Fatal("settled cache not bounded")
	}
	if got.Messages[0].Content != "unfinished output" {
		t.Fatal("eviction mutated caller snapshot")
	}
}

type transcriptCheckpointSession struct {
	sessions.CoordinationSession
	afterCommit func()
}

func (s *transcriptCheckpointSession) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	return nil, errors.New("post-commit read must not be required")
}
func (s *transcriptCheckpointSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	if err := s.CoordinationSession.UpdateCoordination(ctx, fn); err != nil {
		return err
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

func TestLiveTranscriptCheckpointProjectionAndInterleaving(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 2)
	i := &invocation{id: "test-execution", member: "member"}
	r.mu.Lock()
	r.active[i.member] = i
	r.mu.Unlock()
	t.Cleanup(func() { r.mu.Lock(); delete(r.active, i.member); r.mu.Unlock() })
	cb := &llm.AgentCallbacks{}
	binding := i.bindTranscript(cb, r.parent.ViewID())
	initial, err := r.parent.ReadCoordination(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed, release := make(chan struct{}), make(chan struct{})
	wrapper := &transcriptCheckpointSession{CoordinationSession: r.parent, afterCommit: func() { close(committed); <-release }}
	r.config.DurableMessages = func(input []messages.ChatMessage) []messages.ChatMessage {
		if len(input) == 0 {
			return nil
		}
		return input[len(input)-1:]
	}
	r.bindCheckpoint(wrapper, "", 0, 0, cb, nil, binding)
	generated := []messages.ChatMessage{liveMessage(messages.MessageRoleAssistant, "denied", "denied"), liveMessage(messages.MessageRoleTool, "result", "omitted"), liveMessage(messages.MessageRoleUser, "mail", "accepted")}
	cb.OnMessage(0, generated[0])
	cb.OnToolResult(messages.ChatMessageToolCall{}, generated[1])
	cb.OnMessage(1, generated[1])
	checkpointDone := make(chan error, 1)
	go func() { checkpointDone <- cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated, Request: true}) }()
	<-committed // durable state is newer, but acknowledgement is still locked
	viewStore := r.config.Store.(sessions.ViewStore)
	view, err := viewStore.ReadView(ctx, sessions.ViewTarget{ID: r.parent.ViewID()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Sequence != initial.Sequence+1 {
		t.Fatalf("projected sequence=%d, initial=%d", view.Sequence, initial.Sequence)
	}
	readStarted, readDone := make(chan struct{}), make(chan LiveTranscript, 1)
	go func() { close(readStarted); snapshot, _ := r.LiveTranscript("member"); readDone <- snapshot }()
	<-readStarted
	select {
	case <-readDone:
		t.Fatal("snapshot escaped between commit and acknowledgement")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-checkpointDone; err != nil {
		t.Fatal(err)
	}
	got := <-readDone
	if got.BaseSequence != view.Sequence || !got.Ready || len(got.Messages) != 0 || binding.live.acked != len(generated) {
		t.Fatalf("projection cursor/ack: %+v ack=%d", got, binding.live.acked)
	}
	cb.OnMessage(2, generated[2]) // admitted input callback after committed append
	if got := transcriptSnapshot(t, r); len(got.Messages) != 0 {
		t.Fatalf("late admitted input: %+v", got)
	}
	wrapper.afterCommit = nil
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated, Final: true}); err != nil {
		t.Fatal(err)
	}
	view, err = viewStore.ReadView(ctx, sessions.ViewTarget{ID: view.ID}, "")
	if err != nil || view.Sequence != initial.Sequence+1 {
		t.Fatalf("repeated checkpoint duplicated projection: %+v %v", view, err)
	}
}

func TestLiveTranscriptFirstCheckpointUncertainCommitDoesNotDuplicate(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{true, false} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			ctx := context.Background()
			r := runtimeTest(t, nilModel(), 1, 2)
			i := &invocation{}
			cb := &llm.AgentCallbacks{}
			binding := i.bindTranscript(cb, r.parent.ViewID())
			failure := errors.New("checkpoint reply lost")
			wrapper := &refreshStoreFault{CoordinationSession: r.parent, match: func(*State, *State) bool { return true }}
			if committed {
				wrapper.after = func() error { return failure }
			} else {
				wrapper.before = func(*State) error { return failure }
			}
			r.bindCheckpoint(wrapper, "", 0, 0, cb, nil, binding)
			initial, err := r.parent.ReadCoordination(ctx)
			if err != nil {
				t.Fatal(err)
			}
			marker := liveMessage(messages.MessageRoleInternal, "initial-marker", "compaction boundary")
			generated := []messages.ChatMessage{marker}
			cb.OnMessage(0, marker)
			before := binding.live.snapshot
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Request: true, Generated: generated}); !errors.Is(err, failure) {
				t.Fatalf("first checkpoint error=%v", err)
			}
			if binding.live.acked != 0 || !reflect.DeepEqual(binding.live.snapshot, before) || len(binding.live.entries) != 1 {
				t.Fatal("uncertain checkpoint acknowledged live output")
			}
			err = cb.Checkpoint(ctx, llm.AgentCheckpoint{Final: true, Generated: generated})
			if committed && (err == nil || !strings.Contains(err.Error(), "sequence changed")) {
				t.Fatalf("uncertain first append was not fenced: %v", err)
			}
			if !committed && err != nil {
				t.Fatalf("rolled-back first append could not retry: %v", err)
			}
			if committed && (binding.live.acked != 0 || len(binding.live.entries) != 1) {
				t.Fatal("fenced retry acknowledged live output")
			}
			if !committed && (binding.live.acked != len(generated) || len(binding.live.entries) != 0) {
				t.Fatal("successful retry did not acknowledge output")
			}
			view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: r.parent.ViewID()}, "")
			if err != nil {
				t.Fatal(err)
			}
			if view.Sequence != initial.Sequence+1 || view.History[len(view.History)-1].DisplayID() != marker.DisplayID() {
				t.Fatalf("initial checkpoint duplicated or lost: sequence=%d initial=%d history=%+v", view.Sequence, initial.Sequence, view.History)
			}
		})
	}
}

func TestLiveTranscriptConcurrentSnapshotsAndCallbacks(t *testing.T) {
	t.Parallel()
	r, i, cb, binding := newTranscriptTest()
	cb.OnModelRequest(0, 0)
	cb.OnMessageStart("stream")
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 100 {
				switch worker {
				case 0:
					cb.OnContent("chunk")
				case 1:
					cb.OnReasoning("thought")
				case 2:
					cb.OnToolResult(messages.ChatMessageToolCall{}, liveMessage(messages.MessageRoleTool, fmt.Sprint(n), "result"))
				case 3:
					_, _ = r.LiveTranscript("member")
				}
			}
		}()
	}
	wg.Wait()
	if err := binding.checkpoint(llm.AgentCheckpoint{Request: true}, func() (int64, error) { return 5, nil }); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.settleTranscript(i)
	delete(r.active, i.member)
	r.mu.Unlock()
	if got := transcriptSnapshot(t, r); !got.Settled || !got.Incomplete {
		t.Fatalf("concurrent terminal snapshot: %+v", got)
	}
}

func TestLiveTranscriptRuntimeHandoff(t *testing.T) {
	t.Parallel()
	model := activityModel{make(chan struct{}), make(chan struct{})}
	r := runtimeTest(t, model, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed := make(chan struct{}, 1)
	r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
		return &llm.AgentCallbacks{OnReasoning: func(string) { observed <- struct{}{} }}
	}
	result, err := r.Spawn(ctx, subagent.Request{Label: "Live", Task: "work", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	live, ok := r.LiveTranscript(result.Session)
	if !ok || !live.Ready || len(live.Messages) != 1 || live.Messages[0].Reasoning != "eight bytes of thinking" {
		t.Fatalf("runtime stream: %+v ok=%v", live, ok)
	}
	view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: live.SessionID}, "")
	if err != nil || view.Sequence != live.BaseSequence {
		t.Fatalf("initial boundary: %+v %v live=%+v", view, err, live)
	}
	close(model.release)
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	settled, ok := r.LiveTranscript(result.Session)
	if !ok || !settled.Settled || settled.Incomplete || len(settled.Messages) != 0 || settled.Revision <= live.Revision {
		t.Fatalf("terminal handoff: %+v ok=%v", settled, ok)
	}
	view, err = r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: live.SessionID}, view.Revision)
	if err != nil || view.Sequence != settled.BaseSequence || view.Unchanged || view.History[len(view.History)-1].Content != "complete" {
		t.Fatalf("final durable boundary: %+v %v", view, err)
	}
}
