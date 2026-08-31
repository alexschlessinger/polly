package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// artifactBirthPreview is the provider-visible form a stored tool result is
// born with when it carries no media descriptors: what production computes
// via artifactPreviewWithDescriptors on a descriptor-free message.
func artifactBirthPreview(ref artifacts.Ref, data []byte) string {
	head, tail := previewWindows(data)
	return artifactPreview(ref, head, tail, toolPreviewTokenLimit*4)
}

func TestProjectToolResultThreshold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		preview bool
	}{
		{name: "ten thousand tokens stays inline", size: toolInlineTokenLimit * 4},
		{name: "over threshold becomes preview", size: toolInlineTokenLimit*4 + 1, preview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestArtifactStore()
			data := strings.Repeat("x", tc.size)
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "run"},
				{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "tool", Arguments: `{}`}}},
				{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "tool", Content: data},
			}

			projected, stats, err := projectMessages(context.Background(), history, 0, store, false)
			if err != nil {
				t.Fatal(err)
			}
			toolMessage := projected[len(projected)-1]
			if tc.preview {
				if !strings.Contains(toolMessage.Content, "Head/tail preview follows") || estimatedStringTokens(toolMessage.Content) > toolPreviewTokenLimit {
					t.Fatalf("preview is not bounded: tokens=%d content=%q", estimatedStringTokens(toolMessage.Content), toolMessage.Content[:min(200, len(toolMessage.Content))])
				}
				if stats.CompactedToolResults != 1 {
					t.Fatalf("compacted results = %d, want 1", stats.CompactedToolResults)
				}
			} else if toolMessage.Content != data {
				t.Fatalf("threshold-sized result was not kept inline: got %d bytes", len(toolMessage.Content))
			}
			if len(toolMessage.Parts) != 0 {
				t.Fatalf("provider projection retained artifact parts: %#v", toolMessage.Parts)
			}
			if history[len(history)-1].Content != data {
				t.Fatalf("projection mutated durable inline result")
			}
		})
	}
}

func TestProjectKeepsBoundedReadArtifactResultInline(t *testing.T) {
	store := newTestArtifactStore()
	content := strings.Repeat("x", artifactReadMaxBytes)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read it"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_artifact", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: content},
	}
	projected, _, err := projectMessages(context.Background(), history, 0, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := projected[len(projected)-1].Content; got != content {
		t.Fatalf("bounded read_artifact result was recursively compacted: got %d bytes", len(got))
	}
}

func TestProjectToolResultsPassesThroughDurableFormsWithoutStoreReads(t *testing.T) {
	mintStore := newTestArtifactStore()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "run all"}}
	contents := make([]string, 4)
	for i, label := range []string{"one", "two", "three", "four"} {
		data := "HEAD-" + label + "\n" + strings.Repeat(label+" body line\n", 5000) + "TAIL-" + label
		ref := putTestArtifact(t, mintStore, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: label + ".txt", Data: []byte(data)})
		if i%2 == 0 {
			contents[i] = artifactReceipt(ref)
		} else {
			contents[i] = artifactBirthPreview(ref, []byte(data))
		}
		id := "call-" + label
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "tool_" + label, Arguments: `{}`}}},
			messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "tool_" + label, Content: contents[i], Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
		)
	}

	// The projection store fails every Open and Put: receipts and born previews
	// must project byte-identically with zero store I/O.
	projected, stats, err := projectMessages(context.Background(), history, 0, failingArtifactStore{}, false)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if len(toolMessages) != 4 {
		t.Fatalf("tool messages = %d, want 4", len(toolMessages))
	}
	for i, msg := range toolMessages {
		if msg.Content != contents[i] {
			t.Fatalf("durable form %d was rewritten: %q", i, msg.Content[:min(200, len(msg.Content))])
		}
	}
	if stats.CompactedToolResults != 0 {
		t.Fatalf("compacted tool results = %d, want 0", stats.CompactedToolResults)
	}
}

func TestProjectStubsCompletedExchangeRecallResults(t *testing.T) {
	recalled := strings.Repeat("recalled line\n", 500)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read it"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old-read", Name: "read_artifact", Arguments: `{"id":"sha256:abc"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old-read", ToolName: "read_artifact", Content: recalled},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old-tiny", Name: "read_artifact", Arguments: `{"id":"sha256:def"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old-tiny", ToolName: "read_artifact", Content: "No matches."},
		{Role: messages.MessageRoleAssistant, Content: "summarized"},
		{Role: messages.MessageRoleUser, Content: "next question"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "new-read", Name: "read_artifact", Arguments: `{"id":"sha256:abc"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "new-read", ToolName: "read_artifact", Content: recalled},
	}

	// Over budget, so the compaction front reaches the completed exchange.
	projected, stats, err := projectMessages(context.Background(), history, 3_000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if toolMessages[0].Content != recallResultStub("read_artifact") {
		t.Fatalf("completed-exchange recall was not stubbed: %q", toolMessages[0].Content[:min(200, len(toolMessages[0].Content))])
	}
	if toolMessages[1].Content != "No matches." {
		t.Fatalf("tiny recall result was inflated: %q", toolMessages[1].Content)
	}
	if toolMessages[2].Content != recalled {
		t.Fatalf("active-exchange recall was stubbed: %q", toolMessages[2].Content[:min(200, len(toolMessages[2].Content))])
	}
	if stats.CompactedToolResults != 1 {
		t.Fatalf("compacted tool results = %d, want 1", stats.CompactedToolResults)
	}
	if history[2].Content != recalled {
		t.Fatalf("projection mutated durable recall result")
	}
}

func TestLargeTextPreviewRetainsTypedMediaDescriptorWithinBound(t *testing.T) {
	store := newTestArtifactStore()
	imageRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("image")})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "tool", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "tool",
			Content: strings.Repeat("large text\n", 5_000),
			Parts:   []messages.ContentPart{imageArtifactPart(imageRef)}},
	}

	projected, _, err := projectMessages(context.Background(), history, 0, store, false)
	if err != nil {
		t.Fatal(err)
	}
	tool := messagesWithRole(projected, messages.MessageRoleTool)[0]
	if !strings.Contains(tool.Content, imageRef.ID) || !strings.Contains(tool.Content, imageRef.ImageToken) {
		t.Fatalf("preview dropped typed media descriptor: %q", tool.Content)
	}
	if estimatedStringTokens(tool.Content) > toolPreviewTokenLimit {
		t.Fatalf("preview plus descriptors uses %d tokens, max %d", estimatedStringTokens(tool.Content), toolPreviewTokenLimit)
	}
}

func TestProjectSpillsOlderActiveToolPreviewsUnderPressure(t *testing.T) {
	store := newTestArtifactStore()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "active turn"}}
	refs := make([]artifacts.Ref, 3)
	for i, label := range []string{"first", "second", "third"} {
		data := []byte(strings.Repeat(label, 20_000))
		refs[i] = putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, Data: data})
		id := "active-" + label
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: label, Arguments: `{}`}}},
			messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: label, Content: artifactBirthPreview(refs[i], data), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &refs[i]}}},
		)
	}

	projected, stats, err := projectMessages(context.Background(), history, 1_200, store, false)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if toolMessages[0].Content != artifactReceipt(refs[0]) {
		t.Fatalf("oldest active preview was not demoted first: %q", toolMessages[0].Content[:min(200, len(toolMessages[0].Content))])
	}
	if !strings.Contains(toolMessages[len(toolMessages)-1].Content, "Head/tail preview follows") {
		t.Fatalf("newest active result lost its preview: %q", toolMessages[len(toolMessages)-1].Content)
	}
	if stats.EstimatedTokens > 1_200 {
		t.Fatalf("projected tokens = %d, want <= 1200", stats.EstimatedTokens)
	}
	if stats.CompactedToolResults == 0 {
		t.Fatal("preview demotion was not counted")
	}
}

func TestProjectSpillsOlderInlineActiveToolResultUnderPressure(t *testing.T) {
	store := newTestArtifactStore()
	first := strings.Repeat("first-inline-", 1_600)   // below 10k estimated tokens
	second := strings.Repeat("second-inline-", 1_500) // below 10k estimated tokens
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "active"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "first", Name: "first", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "first", ToolName: "first", Content: first},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "second", Name: "second", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "second", ToolName: "second", Content: second},
	}

	projected, stats, err := projectMessages(context.Background(), history, 6_000, store, false)
	if err != nil {
		t.Fatal(err)
	}
	tools := messagesWithRole(projected, messages.MessageRoleTool)
	if len(tools) != 2 || !strings.Contains(tools[0].Content, "stored as artifact") || tools[1].Content != second {
		t.Fatalf("pressure spill = %#v", tools)
	}
	if stats.CompactedToolResults != 1 || len(stats.artifactRefs) != 1 {
		t.Fatalf("pressure spill stats = %+v refs=%#v", stats, stats.artifactRefs)
	}
	r, err := store.Open(context.Background(), stats.artifactRefs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(stored) != first {
		t.Fatalf("spilled inline artifact = %d bytes, %v", len(stored), err)
	}
	if history[2].Content != first || len(history[2].Parts) != 0 {
		t.Fatalf("pressure projection mutated durable inline result: %#v", history[2])
	}
}

func TestAppendArtifactRefUpgradesKindInFirstReferenceOrder(t *testing.T) {
	sharedData := []byte("shared artifact bytes")
	binary := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindBinary, MIMEType: "application/octet-stream", Name: "shared.bin", Data: sharedData})
	image := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "shared.png", Data: sharedData})
	text := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "shared.txt", Data: sharedData})
	other := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindBinary, Data: []byte("other")})

	refs := []artifacts.Ref{binary, other}
	refs = appendArtifactRef(refs, image)
	refs = appendArtifactRef(refs, text)
	refs = appendArtifactRef(refs, binary)

	if len(refs) != 2 {
		t.Fatalf("refs = %#v, want two unique IDs", refs)
	}
	if refs[0] != text {
		t.Fatalf("shared ref = %#v, want richer text ref %#v", refs[0], text)
	}
	if refs[1] != other {
		t.Fatalf("first-reference order changed: %#v", refs)
	}
}

func TestAgentPressureSpillUpgradesEqualBinaryRefForImmediateRead(t *testing.T) {
	content := "FIRST LINE\n" + strings.Repeat("shared pressure-spill bytes\n", 1_000)
	if estimatedStringTokens(content) > toolInlineTokenLimit {
		t.Fatalf("pressure-spill fixture uses %d tokens, want <= %d", estimatedStringTokens(content), toolInlineTokenLimit)
	}
	testAgentEqualBinaryRefImmediateRead(t, content, 1_200)
}

func TestAgentLegacyCompactionUpgradesEqualBinaryRefForImmediateRead(t *testing.T) {
	content := "FIRST LINE\n" + strings.Repeat("shared legacy-compaction bytes\n", 2_000)
	if estimatedStringTokens(content) <= toolInlineTokenLimit {
		t.Fatalf("legacy-compaction fixture uses %d tokens, want > %d", estimatedStringTokens(content), toolInlineTokenLimit)
	}
	testAgentEqualBinaryRefImmediateRead(t, content, 0)
}

func testAgentEqualBinaryRefImmediateRead(t *testing.T, content string, maxContextTokens int) {
	t.Helper()
	store := newTestArtifactStore()
	binaryRef := putTestArtifact(t, store, artifacts.Blob{
		Kind: artifacts.KindBinary, MIMEType: "application/octet-stream", Name: "shared.bin", Data: []byte(content),
	})
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{
			Role:       messages.MessageRoleAssistant,
			StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{
				ID: "read", Name: "read_artifact", Arguments: fmt.Sprintf(`{"id":%q,"limit":1}`, binaryRef.ID),
			}},
		},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "inspect this result"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "shared", Name: "shared", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "shared", ToolName: "shared", Content: content, Parts: []messages.ContentPart{
			{Type: "artifact", Artifact: &binaryRef},
		}},
	}

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: maxContextTokens}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 || !strings.Contains(projectedText(model.requests[0]), binaryRef.ID) {
		t.Fatalf("compacted ref was not exposed to the model: %#v", model.requests)
	}
	var readResult messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "read_artifact" {
			readResult = msg
			break
		}
	}
	if !strings.HasPrefix(readResult.Content, "1: FIRST LINE") {
		t.Fatalf("same-run read_artifact used the binary ref instead of spilled text: %#v", readResult)
	}
	refs := agent.listArtifacts()
	if len(refs) != 1 || refs[0].ID != binaryRef.ID || refs[0].Kind != artifacts.KindText {
		t.Fatalf("indexed refs = %#v, want one upgraded text ref", refs)
	}
}

func TestProjectSurfacesArtifactPutFailures(t *testing.T) {
	t.Run("initial large result", func(t *testing.T) {
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "run"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "large", Name: "large", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "large", ToolName: "large", Content: strings.Repeat("large", toolInlineTokenLimit)},
		}
		_, _, err := projectMessages(context.Background(), history, 0, failingArtifactStore{}, false)
		if !errors.Is(err, errFailingArtifactStore) {
			t.Fatalf("projectMessages() error = %v, want artifact storage failure", err)
		}
	})

	t.Run("active pressure spill", func(t *testing.T) {
		first := strings.Repeat("first-inline-", 1_600)
		second := strings.Repeat("second-inline-", 1_500)
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "active"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "first", Name: "first", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "first", ToolName: "first", Content: first},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "second", Name: "second", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "second", ToolName: "second", Content: second},
		}
		_, _, err := projectMessages(context.Background(), history, 6_000, failingArtifactStore{}, false)
		if !errors.Is(err, errFailingArtifactStore) {
			t.Fatalf("projectMessages() error = %v, want pressure-spill storage failure", err)
		}
	})
}

func TestProjectOmitsCompleteExchangesAndPreservesSystemContext(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "primary system"},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old-call", Name: "lookup", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old-call", ToolName: "lookup", Content: "old result"},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleSystem, Content: "later system invariant"},
		{Role: messages.MessageRoleUser, Content: "current question"},
	}

	projected, stats, err := projectMessages(context.Background(), history, 200, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OmittedExchanges != 1 {
		t.Fatalf("omitted exchanges = %d, want 1", stats.OmittedExchanges)
	}
	joined := projectedText(projected)
	for _, want := range []string{"primary system", "later system invariant", "current question", "earlier completed exchanges omitted"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("projection missing %q: %#v", want, projected)
		}
	}
	for _, unwanted := range []string{"old result", "old answer", "old-call"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("projection retained partial old exchange %q: %#v", unwanted, projected)
		}
	}
	if stats.EstimatedTokens > 200 {
		t.Fatalf("projected tokens = %d, want <= 200", stats.EstimatedTokens)
	}
}

func TestProjectionOmissionPreservesCompleteToolPairing(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old-call", Name: "lookup", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old-call", ToolName: "lookup", Content: "old result"},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{
			{ID: "current-a", Name: "first", Arguments: `{}`},
			{ID: "current-b", Name: "second", Arguments: `{}`},
		}},
		{Role: messages.MessageRoleTool, ToolCallID: "current-a", ToolName: "first", Content: "a"},
		{Role: messages.MessageRoleTool, ToolCallID: "current-b", ToolName: "second", Content: "b"},
	}
	projected, stats, err := projectMessages(context.Background(), history, 300, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OmittedExchanges != 1 {
		t.Fatalf("omitted exchanges = %d, want 1", stats.OmittedExchanges)
	}
	paired := make(map[string]bool)
	for _, msg := range projected {
		if msg.Role == messages.MessageRoleAssistant {
			for _, call := range msg.ToolCalls {
				paired[call.ID] = false
			}
		}
		if msg.Role == messages.MessageRoleTool {
			if _, ok := paired[msg.ToolCallID]; !ok {
				t.Fatalf("orphaned tool result %q in %#v", msg.ToolCallID, projected)
			}
			paired[msg.ToolCallID] = true
		}
	}
	if len(paired) != 2 || !paired["current-a"] || !paired["current-b"] {
		t.Fatalf("projected tool pairing = %#v; messages=%#v", paired, projected)
	}
}

func TestProjectReturnsTypedContextLimitErrorForActiveExchange(t *testing.T) {
	_, stats, err := projectMessages(context.Background(), []messages.ChatMessage{{
		Role: messages.MessageRoleUser, Content: strings.Repeat("active", 2_000),
	}}, 100, nil, false)
	var limitErr *ContextLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want ContextLimitError", err)
	}
	if limitErr.Limit != 100 || limitErr.EstimatedTokens != stats.EstimatedTokens || limitErr.EstimatedTokens <= limitErr.Limit {
		t.Fatalf("context error = %+v, stats=%+v", limitErr, stats)
	}
}

func TestProjectHydratesOnlyExplicitImageSelection(t *testing.T) {
	store := newTestArtifactStore()
	one := []byte("image-one")
	two := []byte("image-two")
	refOne := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "one.png", Reference: "[image #1]", Data: one})
	refTwo := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "two.png", Reference: "[image #2]", Data: two})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "first"}, imageArtifactPart(refOne)}},
		{Role: messages.MessageRoleAssistant, Content: "seen"},
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "second"}, imageArtifactPart(refTwo)}},
		{Role: messages.MessageRoleAssistant, Content: "seen too"},
		{Role: messages.MessageRoleUser, Content: "look again at [image #1]"},
	}

	projected, stats, err := projectMessages(context.Background(), history, 0, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.HydratedImages != 1 {
		t.Fatalf("hydrated images = %d, want 1", stats.HydratedImages)
	}
	images := projectedImageParts(projected)
	if len(images) != 1 {
		t.Fatalf("provider image parts = %d, want 1: %#v", len(images), projected)
	}
	decoded, err := base64.StdEncoding.DecodeString(images[0].ImageData)
	if err != nil || string(decoded) != string(one) {
		t.Fatalf("hydrated bytes = %q, %v", decoded, err)
	}
	latest := projected[len(projected)-1]
	if latest.Content != "" || len(latest.Parts) != 2 || latest.Parts[0].Type != "text" || latest.Parts[0].Text != "look again at [image #1]" {
		t.Fatalf("text-only follow-up was not promoted with its image: %#v", latest)
	}
}

func TestProjectImageReferenceFailuresAreClear(t *testing.T) {
	store := newTestArtifactStore()
	first := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "same.png", Reference: "[image #1]", Data: []byte("first")})
	second := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "same.png", Reference: "[image #2]", Data: []byte("second")})
	base := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "one"}, imageArtifactPart(first)}},
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "two"}, imageArtifactPart(second)}},
		{Role: messages.MessageRoleAssistant, Content: "done"},
	}

	t.Run("missing stable token", func(t *testing.T) {
		history := append(cloneMessages(base), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "show [image #99]"})
		_, _, err := projectMessages(context.Background(), history, 0, store, false)
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("error = %v, want unavailable image reference", err)
		}
	})

	t.Run("ambiguous exact filename", func(t *testing.T) {
		history := append(cloneMessages(base), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "compare same.png"})
		_, _, err := projectMessages(context.Background(), history, 0, store, false)
		if err == nil || !strings.Contains(err.Error(), "matches multiple stored images") {
			t.Fatalf("error = %v, want filename ambiguity", err)
		}
	})

	t.Run("missing artifact bytes", func(t *testing.T) {
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "inspect"}, imageArtifactPart(first)}}}
		missingStore := newTestArtifactStore()
		_, _, err := projectMessages(context.Background(), history, 0, missingStore, false)
		if err == nil || !strings.Contains(err.Error(), "read image artifact") {
			t.Fatalf("error = %v, want missing artifact failure", err)
		}
	})
}

func TestProjectImageFilenameMatchingIsCaseSensitive(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "Cat.PNG", Data: []byte("cat")})
	base := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "old"}, imageArtifactPart(ref)}},
		{Role: messages.MessageRoleAssistant, Content: "done"},
	}
	for _, tc := range []struct {
		prompt string
		want   int
	}{{prompt: "show Cat.PNG", want: 1}, {prompt: "show cat.png", want: 0}} {
		history := append(cloneMessages(base), messages.ChatMessage{Role: messages.MessageRoleUser, Content: tc.prompt})
		projected, _, err := projectMessages(context.Background(), history, 0, store, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(projectedImageParts(projected)); got != tc.want {
			t.Fatalf("prompt %q hydrated %d images, want %d", tc.prompt, got, tc.want)
		}
	}
}

func TestToolImageIsAttachedToExactlyFollowingRequest(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "tool.png", Data: []byte("tool-image")})
	firstRequest := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "make an image"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "one", Name: "render", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "one", ToolName: "render", Content: "rendered", Parts: []messages.ContentPart{imageArtifactPart(ref)}},
	}
	projected, _, err := projectMessages(context.Background(), firstRequest, 0, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(projectedImageParts(projected)); got != 1 {
		t.Fatalf("following request has %d tool images, want 1: %#v", got, projected)
	}
	if projected[len(projected)-1].Role != messages.MessageRoleUser {
		t.Fatalf("tool image was not attached in a synthetic user message: %#v", projected[len(projected)-1])
	}

	secondRequest := append(cloneMessages(firstRequest),
		messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "two", Name: "inspect", Arguments: `{}`}}},
		messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "two", ToolName: "inspect", Content: "no image"},
	)
	projected, _, err = projectMessages(context.Background(), secondRequest, 0, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(projectedImageParts(projected)); got != 0 {
		t.Fatalf("later request reattached prior tool image %d time(s): %#v", got, projected)
	}
}

func TestProjectDeduplicatesReadArtifactImageAlreadyReferencedByUser(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{
		Kind: artifacts.KindImage, MIMEType: "image/png", Name: "again.png", Reference: "[image #1]", Data: []byte("same image"),
	})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "old"}, imageArtifactPart(ref)}},
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "read [image #1] again"}}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_artifact", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: "attached", Parts: []messages.ContentPart{imageArtifactPart(ref)}},
	}

	projected, stats, err := projectMessages(context.Background(), history, 0, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(projectedImageParts(projected)); got != 1 || stats.HydratedImages != 1 {
		t.Fatalf("same artifact was hydrated %d time(s), stats=%+v: %#v", got, stats, projected)
	}
}

func putTestArtifact(t *testing.T, store artifacts.Store, blob artifacts.Blob) artifacts.Ref {
	t.Helper()
	ref, err := store.Put(context.Background(), blob)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func testToolHistory(ref artifacts.Ref) []messages.ChatMessage {
	return []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "tool", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "tool", Content: artifactReceipt(ref), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
	}
}

func imageArtifactPart(ref artifacts.Ref) messages.ContentPart {
	copyRef := ref
	return messages.ContentPart{Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken, Artifact: &copyRef}
}

func messagesWithRole(history []messages.ChatMessage, role string) []messages.ChatMessage {
	var out []messages.ChatMessage
	for _, msg := range history {
		if msg.Role == role {
			out = append(out, msg)
		}
	}
	return out
}

func projectedImageParts(history []messages.ChatMessage) []messages.ContentPart {
	var out []messages.ContentPart
	for _, msg := range history {
		for _, part := range msg.Parts {
			if part.Type == "image_base64" || part.Type == "image_url" {
				out = append(out, part)
			}
		}
	}
	return out
}

func projectedText(history []messages.ChatMessage) string {
	var b strings.Builder
	for _, msg := range history {
		b.WriteString(msg.Content)
		for _, part := range msg.Parts {
			b.WriteString(part.Text)
		}
		for _, call := range msg.ToolCalls {
			b.WriteString(call.ID)
			b.WriteString(call.Name)
		}
	}
	return b.String()
}

func TestSpillActiveToolResultsRecallHandling(t *testing.T) {
	readContent := strings.Repeat("r", 20_000)
	bashContent := strings.Repeat("b", 36_000)

	t.Run("newest unseen recall survives", func(t *testing.T) {
		store := newTestArtifactStore()
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "go"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{
				{ID: "read", Name: "read_artifact", Arguments: `{}`},
				{ID: "bash", Name: "bash", Arguments: `{}`},
			}},
			{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: readContent},
			{Role: messages.MessageRoleTool, ToolCallID: "bash", ToolName: "bash", Content: bashContent},
		}
		projected, _, err := projectMessages(context.Background(), history, 9_000, store, false)
		if err != nil {
			t.Fatal(err)
		}
		toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
		if toolMessages[0].Content != readContent {
			t.Fatalf("unseen read_artifact result was stubbed: %q", toolMessages[0].Content[:min(200, len(toolMessages[0].Content))])
		}
		if !strings.Contains(toolMessages[1].Content, "stored as artifact") {
			t.Fatalf("other tool result was not spilled: %q", toolMessages[1].Content[:min(200, len(toolMessages[1].Content))])
		}
	})

	t.Run("acted-on recall is stubbed without minting", func(t *testing.T) {
		store := newTestArtifactStore()
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "go"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_artifact", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: readContent},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "bash", Name: "bash", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "bash", ToolName: "bash", Content: bashContent},
		}
		projected, _, err := projectMessages(context.Background(), history, 9_000, store, false)
		if err != nil {
			t.Fatal(err)
		}
		toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
		if toolMessages[0].Content != recallResultStub("read_artifact") {
			t.Fatalf("acted-on read_artifact result was not stubbed: %q", toolMessages[0].Content[:min(200, len(toolMessages[0].Content))])
		}
		if len(toolMessages[0].Parts) != 0 {
			t.Fatalf("stubbing a recall minted an artifact: %#v", toolMessages[0].Parts)
		}
		if !strings.Contains(toolMessages[1].Content, "stored as artifact") {
			t.Fatalf("other tool result was not spilled: %q", toolMessages[1].Content[:min(200, len(toolMessages[1].Content))])
		}
	})
}

func TestProjectToolResultsRequireStoreForArtifactRefs(t *testing.T) {
	mintStore := newTestArtifactStore()
	ref := putTestArtifact(t, mintStore, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "bash.txt", Data: []byte("lost output")})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "bash", Content: artifactReceipt(ref), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
	}

	// A transcript with artifact refs but no configured store is a hard error.
	_, _, err := projectMessages(context.Background(), history, 0, nil, false)
	if err == nil || !strings.Contains(err.Error(), ref.ID) {
		t.Fatalf("projectMessages() error = %v, want failure for artifact %s", err, ref.ID)
	}

	// A store missing the blob is fine at projection time: the receipt passes
	// through without any store read, and read_artifact reports the miss.
	projected, stats, err := projectMessages(context.Background(), history, 0, newTestArtifactStore(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := projected[len(projected)-1].Content; got != artifactReceipt(ref) {
		t.Fatalf("receipt was rewritten: %q", got)
	}
	if stats.CompactedToolResults != 0 {
		t.Fatalf("compacted tool results = %d, want 0", stats.CompactedToolResults)
	}
}

func TestValidateImageProjectionEnforcesAggregateImageCaps(t *testing.T) {
	t.Run("request image count", func(t *testing.T) {
		parts := []messages.ContentPart{{Type: "text", Text: "compare everything"}}
		for i := 0; i <= maxProjectedRequestImages; i++ {
			ref := artifacts.Ref{ID: fmt.Sprintf("img-%03d", i), Kind: artifacts.KindImage, MIMEType: "image/png", Bytes: 10}
			parts = append(parts, messages.ContentPart{Type: "image_artifact", Artifact: &ref})
		}
		err := ValidateImageProjection([]messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: parts}})
		if err == nil || !strings.Contains(err.Error(), "portable maximum") {
			t.Fatalf("error = %v, want request image cap", err)
		}
	})
	t.Run("aggregate encoded bytes", func(t *testing.T) {
		parts := []messages.ContentPart{{Type: "text", Text: "compare both"}}
		for i := 0; i < 2; i++ {
			ref := artifacts.Ref{ID: fmt.Sprintf("big-%d", i), Kind: artifacts.KindImage, MIMEType: "image/png", Bytes: 8 << 20}
			parts = append(parts, messages.ContentPart{Type: "image_artifact", Artifact: &ref})
		}
		err := ValidateImageProjection([]messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: parts}})
		if err == nil || !strings.Contains(err.Error(), "portable limit") {
			t.Fatalf("error = %v, want encoded byte cap", err)
		}
	})
	t.Run("unresolvable reference without a store", func(t *testing.T) {
		err := ValidateImageProjection([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "explain [image #4]"}})
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("error = %v, want unavailable image reference", err)
		}
	})
}

func TestProjectionMarkerAdvertisesArtifactRecallOnlyWithStore(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current"},
	}

	withStore, stats, err := projectMessages(context.Background(), cloneMessages(history), 250, newTestArtifactStore(), false)
	if err != nil || stats.OmittedExchanges == 0 {
		t.Fatalf("omission projection failed: stats=%+v err=%v", stats, err)
	}
	if !strings.Contains(projectedText(withStore), "call list_artifacts to enumerate them") {
		t.Fatalf("marker does not advertise recall: %q", projectedText(withStore))
	}

	without, stats, err := projectMessages(context.Background(), cloneMessages(history), 250, nil, false)
	if err != nil || stats.OmittedExchanges == 0 {
		t.Fatalf("nil-store omission projection failed: stats=%+v err=%v", stats, err)
	}
	if strings.Contains(projectedText(without), "list_artifacts") {
		t.Fatalf("nil-store marker advertises an unavailable tool: %q", projectedText(without))
	}
}

func TestProjectionCapturesRefsFromOmittedExchanges(t *testing.T) {
	store := newTestArtifactStore()
	legacy := strings.Repeat("legacy line\n", 4_000)
	// The legacy tool result demotes to a receipt before omission is
	// considered, so the old exchange's bulk must be conversational text for
	// omission to trigger.
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "legacy_tool", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old", ToolName: "legacy_tool", Content: legacy},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current"},
	}

	projected, stats, err := projectMessages(context.Background(), history, 300, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OmittedExchanges == 0 {
		t.Fatalf("legacy exchange was not omitted: %+v", stats)
	}
	var captured *artifacts.Ref
	for i := range stats.artifactRefs {
		if stats.artifactRefs[i].Bytes == int64(len(legacy)) {
			captured = &stats.artifactRefs[i]
		}
	}
	if captured == nil {
		t.Fatalf("ref minted inside the omitted exchange was not captured: %#v", stats.artifactRefs)
	}
	if strings.Contains(projectedText(projected), captured.ID) {
		t.Fatalf("omitted exchange leaked into the projection: %#v", projected)
	}
}

func TestSpilledMediaResultKeepsOneFinalFormAcrossProjections(t *testing.T) {
	store := newTestArtifactStore()
	imageRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "shot.png", Reference: "[image #1]", Data: []byte("imgbytes")})
	screenshot := strings.Repeat("pixel row\n", 2_000)
	other := strings.Repeat("other-inline-", 1_500)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "capture"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "shot", Name: "screenshot", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "shot", ToolName: "screenshot", Content: screenshot, Parts: []messages.ContentPart{imageArtifactPart(imageRef)}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "other", Name: "other", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "other", ToolName: "other", Content: other},
	}

	shotContent := func(projected []messages.ChatMessage) string {
		for _, msg := range projected {
			if msg.Role == messages.MessageRoleTool && msg.ToolCallID == "shot" {
				return msg.Content
			}
		}
		t.Fatal("projected history lost the screenshot result")
		return ""
	}

	first, stats, err := projectMessages(context.Background(), history, 6_000, store, false)
	if err != nil {
		t.Fatal(err)
	}
	sent := shotContent(first)
	if !strings.Contains(sent, "stored as artifact") || !strings.Contains(sent, "[image artifact "+imageRef.ID) {
		t.Fatalf("spilled media form lost its descriptor: %q", sent)
	}
	if len(stats.toolSpills) != 1 || stats.toolSpills[0].Receipt != sent {
		t.Fatalf("spill record form = %+v, sent %q", stats.toolSpills, sent)
	}

	(&Agent{}).applyDurableToolSpills(history, stats.toolSpills)
	if history[2].Content != sent {
		t.Fatalf("durable final form %q != sent form %q", history[2].Content[:min(200, len(history[2].Content))], sent[:min(200, len(sent))])
	}
	second, _, err := projectMessages(context.Background(), history, 6_000, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := shotContent(second); got != sent {
		t.Fatalf("second projection diverged:\nfirst:  %q\nsecond: %q", sent, got)
	}
	if resent, _, err := projectMessages(context.Background(), history, 6_000, store, false); err != nil || shotContent(resent) != sent {
		t.Fatalf("third projection diverged: %v", err)
	}
}

func TestSpillSkipsByteIdenticalRewritesInStats(t *testing.T) {
	store := newTestArtifactStore()
	data := strings.Repeat("spilled once\n", 2_000)
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(data)})
	binaryRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindBinary, MIMEType: "application/zip", Data: []byte("zip")})
	spilledForm := withDescriptorList(artifactReceipt(ref), []string{artifactMediaDescriptor(binaryRef)})
	fresh := strings.Repeat("fresh-inline-", 1_500)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "old", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old", ToolName: "old", Content: spilledForm, Parts: []messages.ContentPart{
			{Type: "artifact", Artifact: &ref},
			{Type: "artifact", Artifact: &binaryRef},
		}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "new", Name: "new", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "new", ToolName: "new", Content: fresh},
	}

	projected, stats, err := projectMessages(context.Background(), history, 4_500, store, false)
	if err != nil {
		t.Fatal(err)
	}
	tools := messagesWithRole(projected, messages.MessageRoleTool)
	if tools[0].Content != spilledForm {
		t.Fatalf("already-spilled media form was rewritten: %q", tools[0].Content)
	}
	if !strings.Contains(tools[1].Content, "stored as artifact") {
		t.Fatalf("fresh result was not spilled: %q", tools[1].Content[:min(120, len(tools[1].Content))])
	}
	if stats.CompactedToolResults != 1 {
		t.Fatalf("compacted results = %d, want 1 (byte-identical rewrite must not count)", stats.CompactedToolResults)
	}
}

func TestSpillSkipsResultsSmallerThanReceipts(t *testing.T) {
	t.Run("small results stay inline while larger ones spill", func(t *testing.T) {
		store := newTestArtifactStore()
		big := strings.Repeat("big-inline-", 1_800)
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "active"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "tiny", Name: "tiny", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "tiny", ToolName: "tiny", Content: "ok"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "big", Name: "big", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "big", ToolName: "big", Content: big},
		}
		projected, stats, err := projectMessages(context.Background(), history, 3_000, store, false)
		if err != nil {
			t.Fatal(err)
		}
		toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
		if toolMessages[0].Content != "ok" || len(toolMessages[0].Parts) != 0 {
			t.Fatalf("small result was spilled into a larger receipt: %#v", toolMessages[0])
		}
		if !strings.Contains(toolMessages[1].Content, "stored as artifact") {
			t.Fatalf("large result was not spilled: %q", toolMessages[1].Content[:min(120, len(toolMessages[1].Content))])
		}
		if len(stats.toolSpills) != 1 || stats.CompactedToolResults != 1 {
			t.Fatalf("spill stats = %+v", stats)
		}
	})

	t.Run("irreducibly small results never mint artifacts", func(t *testing.T) {
		history := []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 4_000)},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "tiny", Name: "tiny", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "tiny", ToolName: "tiny", Content: "ok"},
		}
		// The failing store proves no Put is attempted for results a receipt
		// cannot shrink; the projection fails over to ContextLimitError.
		_, _, err := projectMessages(context.Background(), history, 500, failingArtifactStore{}, false)
		var limitErr *ContextLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("error = %v, want ContextLimitError without store writes", err)
		}
	})
}

func TestOmissionFrontBatchesAndHoldsAcrossGrowth(t *testing.T) {
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "sys"}}
	for j := 0; j < 40; j++ {
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("u%02d %s", j, strings.Repeat("q", 120))},
			messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: strings.Repeat("a", 80)},
		)
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "current question"})

	const budget = 600
	first, firstStats, err := projectMessages(context.Background(), cloneMessages(history), budget, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if firstStats.OmittedExchanges == 0 || firstStats.EstimatedTokens > budget {
		t.Fatalf("omission projection = %+v", firstStats)
	}

	// Sub-quantum growth must not move the omission front: the next
	// projection is the previous one plus the appended exchange, byte for
	// byte, so the provider's cached prefix survives the turn.
	appendix := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "next"},
		{Role: messages.MessageRoleAssistant, Content: "ok"},
	}
	if firstStats.EstimatedTokens+estimateProjectedTokens(appendix) > budget {
		t.Fatalf("fixture growth exceeds the headroom this test depends on: %+v", firstStats)
	}
	grown := append(cloneMessages(history), appendix...)
	second, secondStats, err := projectMessages(context.Background(), cloneMessages(grown), budget, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if secondStats.OmittedExchanges != firstStats.OmittedExchanges {
		t.Fatalf("front moved on sub-quantum growth: %d -> %d", firstStats.OmittedExchanges, secondStats.OmittedExchanges)
	}
	if len(second) != len(first)+len(appendix) || !reflect.DeepEqual(second[:len(first)], first) {
		t.Fatalf("prefix not byte-stable across growth:\nfirst %d msgs\nsecond %d msgs", len(first), len(second))
	}

	// Growth past the quantum advances the front, and in a batch rather than
	// one exchange at a time.
	big := append(cloneMessages(grown),
		messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("z", 800)},
		messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "noted"},
	)
	_, bigStats, err := projectMessages(context.Background(), big, budget, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if bigStats.OmittedExchanges <= secondStats.OmittedExchanges {
		t.Fatalf("front did not advance past quantum growth: %+v", bigStats)
	}
	if bigStats.OmittedExchanges-secondStats.OmittedExchanges < 2 {
		t.Fatalf("front advanced one exchange at a time: %d -> %d", secondStats.OmittedExchanges, bigStats.OmittedExchanges)
	}
}

func TestProjectDemotesCompletedToolResultsToReceipts(t *testing.T) {
	completedContent := strings.Repeat("completed output line\n", 200)
	smallContent := strings.Repeat("short line\n", 30)
	base := func() []messages.ChatMessage {
		return []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "old request"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{
				{ID: "old-big", Name: "bash", Arguments: `{}`},
				{ID: "old-small", Name: "bash", Arguments: `{}`},
			}},
			{Role: messages.MessageRoleTool, ToolCallID: "old-big", ToolName: "bash", Content: completedContent},
			{Role: messages.MessageRoleTool, ToolCallID: "old-small", ToolName: "bash", Content: smallContent},
			{Role: messages.MessageRoleAssistant, Content: "old answer"},
			{Role: messages.MessageRoleUser, Content: "current"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "new", Name: "bash", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: "new", ToolName: "bash", Content: completedContent},
		}
	}

	store := newTestArtifactStore()
	history := base()
	// Over budget, so the compaction front reaches the completed exchange.
	projected, stats, err := projectMessages(context.Background(), history, 2_000, store, false)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if !strings.Contains(toolMessages[0].Content, "stored as artifact") {
		t.Fatalf("completed result was not demoted: %q", toolMessages[0].Content[:min(120, len(toolMessages[0].Content))])
	}
	if toolMessages[1].Content != smallContent {
		t.Fatalf("small completed result was demoted: %q", toolMessages[1].Content)
	}
	if toolMessages[2].Content != completedContent {
		t.Fatalf("active result was demoted: %q", toolMessages[2].Content[:min(120, len(toolMessages[2].Content))])
	}
	if stats.CompactedToolResults != 1 {
		t.Fatalf("compacted results = %d, want 1", stats.CompactedToolResults)
	}
	if history[2].Content != completedContent || len(history[2].Parts) != 0 {
		t.Fatalf("demotion mutated the durable result: %#v", history[2])
	}
	var ref *artifacts.Ref
	for i := range stats.artifactRefs {
		if stats.artifactRefs[i].Bytes == int64(len(completedContent)) {
			ref = &stats.artifactRefs[i]
		}
	}
	if ref == nil || !strings.Contains(toolMessages[0].Content, ref.ID) {
		t.Fatalf("demoted receipt lacks a captured recall ref: refs=%#v", stats.artifactRefs)
	}
	r, err := store.Open(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(stored) != completedContent {
		t.Fatalf("demoted artifact = %d bytes, %v", len(stored), err)
	}

	again, _, err := projectMessages(context.Background(), base(), 2_000, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := messagesWithRole(again, messages.MessageRoleTool)[0].Content; got != toolMessages[0].Content {
		t.Fatalf("demoted form is not byte-stable:\nfirst:  %q\nsecond: %q", toolMessages[0].Content, got)
	}
}

func TestProjectDemotesCompletedPreviewsWithoutStoreReads(t *testing.T) {
	mintStore := newTestArtifactStore()
	data := "HEAD\n" + strings.Repeat("preview body line\n", 3_000) + "TAIL"
	ref := putTestArtifact(t, mintStore, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "bash.txt", Data: []byte(data)})
	preview := artifactBirthPreview(ref, []byte(data))
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "bash", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old", ToolName: "bash", Content: preview, Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Content: "current"},
	}
	// Over budget, so the front reaches the completed exchange; the failing
	// store proves demoting an artifact-backed preview to its receipt costs
	// zero store I/O.
	projected, stats, err := projectMessages(context.Background(), history, 300, failingArtifactStore{}, false)
	if err != nil {
		t.Fatal(err)
	}
	tool := messagesWithRole(projected, messages.MessageRoleTool)[0]
	if tool.Content != artifactReceipt(ref) {
		t.Fatalf("completed preview did not demote to its receipt: %q", tool.Content[:min(200, len(tool.Content))])
	}
	if stats.CompactedToolResults != 1 {
		t.Fatalf("compacted results = %d, want 1", stats.CompactedToolResults)
	}
	if history[2].Content != preview {
		t.Fatalf("demotion mutated the durable preview")
	}
}

func TestCompactionKeepsBirthFormsUnderBudget(t *testing.T) {
	recalled := strings.Repeat("recalled line\n", 500)
	completed := strings.Repeat("completed output line\n", 200)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old-read", Name: "read_artifact", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old-read", ToolName: "read_artifact", Content: recalled},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old-big", Name: "bash", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old-big", ToolName: "bash", Content: completed},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current"},
	}

	for _, tc := range []struct {
		name   string
		budget int
	}{
		{name: "fits its budget", budget: 50_000},
		{name: "no budget configured", budget: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projected, stats, err := projectMessages(context.Background(), cloneMessages(history), tc.budget, newTestArtifactStore(), false)
			if err != nil {
				t.Fatal(err)
			}
			toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
			if toolMessages[0].Content != recalled || toolMessages[1].Content != completed {
				t.Fatalf("completed results were demoted without budget pressure: %q / %q",
					toolMessages[0].Content[:min(80, len(toolMessages[0].Content))],
					toolMessages[1].Content[:min(80, len(toolMessages[1].Content))])
			}
			if stats.CompactedToolResults != 0 {
				t.Fatalf("compacted results = %d, want 0", stats.CompactedToolResults)
			}
		})
	}
}

func TestCompactionHoldsProviderPrefixAcrossTurnBoundary(t *testing.T) {
	store := newTestArtifactStore()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work on it"}}
	for _, label := range []string{"one", "two", "three"} {
		id := "call-" + label
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "bash", Arguments: `{}`}}},
			messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "bash", Content: strings.Repeat(label+" output\n", 300)},
		)
	}

	const budget = 50_000
	during, _, err := projectMessages(context.Background(), cloneMessages(history), budget, store, false)
	if err != nil {
		t.Fatal(err)
	}

	// A new user turn completes the exchange. Under budget the completed
	// results must keep their birth forms: the next request is the previous
	// one plus the appended messages, byte for byte, so the provider's cached
	// prefix survives the turn boundary.
	next := append(cloneMessages(history),
		messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"},
		messages.ChatMessage{Role: messages.MessageRoleUser, Content: "diffstat"},
	)
	after, stats, err := projectMessages(context.Background(), next, budget, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CompactedToolResults != 0 {
		t.Fatalf("turn boundary compacted %d results under budget", stats.CompactedToolResults)
	}
	if len(after) != len(during)+2 || !reflect.DeepEqual(after[:len(during)], during) {
		t.Fatalf("prefix not byte-stable across the turn boundary:\nduring %d msgs\nafter %d msgs", len(during), len(after))
	}
}

func TestCompactionFrontBatchesAndHoldsAcrossGrowth(t *testing.T) {
	store := newTestArtifactStore()
	exchange := func(j int) []messages.ChatMessage {
		id := fmt.Sprintf("call-%02d", j)
		return []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: fmt.Sprintf("u%02d", j)},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "bash", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "bash", Content: fmt.Sprintf("%02d ", j) + strings.Repeat("r", 2_400)},
			{Role: messages.MessageRoleAssistant, Content: "noted"},
		}
	}
	var history []messages.ChatMessage
	for j := 0; j < 10; j++ {
		history = append(history, exchange(j)...)
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "current question"})

	const budget = 3_000
	first, firstStats, err := projectMessages(context.Background(), cloneMessages(history), budget, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if firstStats.CompactedToolResults == 0 || firstStats.CompactedToolResults == 10 {
		t.Fatalf("front is not partial: %+v", firstStats)
	}
	for i, msg := range history {
		if msg.Role != messages.MessageRoleTool {
			continue
		}
		form, _, ok := demotedToolResultForm(msg, true)
		if !ok {
			t.Fatalf("fixture result %d is not demotable", i)
		}
		if got := first[i].Content; got != form && got != msg.Content {
			t.Fatalf("projected form %d matches neither birth nor demotion: %q", i, got[:min(120, len(got))])
		}
	}

	// Sub-quantum growth must not move the front: the next projection is the
	// previous one plus the appended messages, byte for byte.
	grown := append(cloneMessages(history),
		messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "ok"},
		messages.ChatMessage{Role: messages.MessageRoleUser, Content: "again"},
	)
	second, secondStats, err := projectMessages(context.Background(), grown, budget, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if secondStats.CompactedToolResults != firstStats.CompactedToolResults {
		t.Fatalf("front moved on sub-quantum growth: %d -> %d", firstStats.CompactedToolResults, secondStats.CompactedToolResults)
	}
	if !reflect.DeepEqual(second[:len(first)], first) {
		t.Fatalf("prefix not byte-stable across growth")
	}

	// Growth past the quantum advances the front, and in a batch rather than
	// one exchange at a time.
	big := cloneMessages(grown)
	for j := 10; j < 13; j++ {
		big = append(big, exchange(j)...)
	}
	big = append(big, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "still going"})
	_, bigStats, err := projectMessages(context.Background(), big, budget, store, false)
	if err != nil {
		t.Fatal(err)
	}
	if bigStats.CompactedToolResults <= secondStats.CompactedToolResults {
		t.Fatalf("front did not advance past quantum growth: %+v", bigStats)
	}
	if bigStats.CompactedToolResults-secondStats.CompactedToolResults < 2 {
		t.Fatalf("front advanced one exchange at a time: %d -> %d", secondStats.CompactedToolResults, bigStats.CompactedToolResults)
	}
}

func TestProjectionMarkerAdvertisesTranscriptRecallOnlyWhenReadable(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "sys"},
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "current"},
	}

	readable, stats, err := projectMessages(context.Background(), cloneMessages(history), 250, nil, true)
	if err != nil || stats.OmittedExchanges == 0 {
		t.Fatalf("omission projection failed: stats=%+v err=%v", stats, err)
	}
	if !strings.Contains(projectedText(readable), "call read_transcript to page or search it") {
		t.Fatalf("marker does not advertise transcript recall: %q", projectedText(readable))
	}

	unreadable, stats, err := projectMessages(context.Background(), cloneMessages(history), 250, nil, false)
	if err != nil || stats.OmittedExchanges == 0 {
		t.Fatalf("omission projection failed: stats=%+v err=%v", stats, err)
	}
	if strings.Contains(projectedText(unreadable), "read_transcript") {
		t.Fatalf("marker advertises an unavailable tool: %q", projectedText(unreadable))
	}
}

func TestAgentRejectsBudgetSmallerThanToolSchemas(t *testing.T) {
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	_, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("hi"), MaxContextTokens: 300,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "tool schemas") {
		t.Fatalf("Run error = %v, want tool-schema budget failure", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("provider was called despite schema overflow")
	}
}
