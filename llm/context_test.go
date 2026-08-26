package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

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
			store := artifacts.NewMemoryStore()
			data := strings.Repeat("x", tc.size)
			ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(data)})
			history := testToolHistory(ref)

			projected, stats, err := projectMessages(context.Background(), history, 0, store)
			if err != nil {
				t.Fatal(err)
			}
			toolMessage := projected[len(projected)-1]
			if tc.preview {
				if !strings.Contains(toolMessage.Content, "head/tail preview") || estimatedStringTokens(toolMessage.Content) > toolPreviewTokenLimit {
					t.Fatalf("preview is not bounded: tokens=%d content=%q", estimatedStringTokens(toolMessage.Content), toolMessage.Content[:min(200, len(toolMessage.Content))])
				}
				if stats.CompactedToolResults != 1 {
					t.Fatalf("compacted results = %d, want 1", stats.CompactedToolResults)
				}
			} else if toolMessage.Content != data {
				t.Fatalf("threshold-sized result was not restored inline: got %d bytes", len(toolMessage.Content))
			}
			if len(toolMessage.Parts) != 0 {
				t.Fatalf("provider projection retained artifact parts: %#v", toolMessage.Parts)
			}
		})
	}
}

func TestProjectKeepsBoundedReadArtifactResultInline(t *testing.T) {
	store := artifacts.NewMemoryStore()
	content := strings.Repeat("x", artifactReadMaxBytes)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read it"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_artifact", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: content},
	}
	projected, _, err := projectMessages(context.Background(), history, 0, store)
	if err != nil {
		t.Fatal(err)
	}
	if got := projected[len(projected)-1].Content; got != content {
		t.Fatalf("bounded read_artifact result was recursively compacted: got %d bytes", len(got))
	}
}

func TestProjectToolResultsKeepsThreeNewestBatchPreviews(t *testing.T) {
	store := artifacts.NewMemoryStore()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "run all"}}
	refs := make([]artifacts.Ref, 4)
	for i, label := range []string{"one", "two", "three", "four"} {
		data := "HEAD-" + label + "\n" + strings.Repeat(label+" body line\n", 5000) + "TAIL-" + label
		refs[i] = putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: label + ".txt", Data: []byte(data)})
		id := "call-" + label
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "tool_" + label, Arguments: `{}`}}},
			messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: "tool_" + label, Content: artifactReceipt(refs[i]), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &refs[i]}}},
		)
	}

	projected, stats, err := projectMessages(context.Background(), history, 0, store)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if len(toolMessages) != 4 {
		t.Fatalf("tool messages = %d, want 4", len(toolMessages))
	}
	if toolMessages[0].Content != artifactReceipt(refs[0]) {
		t.Fatalf("oldest batch was not collapsed to receipt: %q", toolMessages[0].Content)
	}
	for i := 1; i < len(toolMessages); i++ {
		if !strings.Contains(toolMessages[i].Content, "head/tail preview") || !strings.Contains(toolMessages[i].Content, refs[i].ID) {
			t.Fatalf("batch %d preview = %q", i+1, toolMessages[i].Content[:min(200, len(toolMessages[i].Content))])
		}
		if estimatedStringTokens(toolMessages[i].Content) > toolPreviewTokenLimit {
			t.Fatalf("batch %d preview exceeded %d tokens", i+1, toolPreviewTokenLimit)
		}
	}
	if stats.CompactedToolResults != 4 {
		t.Fatalf("compacted tool results = %d, want 4", stats.CompactedToolResults)
	}

	// Projection is copy-on-write and every durable artifact still holds the
	// complete result bytes.
	if history[2].Content != artifactReceipt(refs[0]) || len(history[2].Parts) != 1 {
		t.Fatalf("projection mutated durable history: %#v", history[2])
	}
	for i, ref := range refs {
		r, err := store.Open(context.Background(), ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !strings.Contains(string(data), "TAIL-"+[]string{"one", "two", "three", "four"}[i]) {
			t.Fatalf("artifact %d was incomplete: bytes=%d err=%v", i, len(data), err)
		}
	}
}

func TestLargeTextPreviewRetainsTypedMediaDescriptorWithinBound(t *testing.T) {
	store := artifacts.NewMemoryStore()
	textRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, Data: []byte(strings.Repeat("large text\n", 5_000))})
	imageRef := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("image")})
	history := testToolHistory(textRef)
	history[len(history)-1].Parts = append(history[len(history)-1].Parts, imageArtifactPart(imageRef))

	projected, _, err := projectMessages(context.Background(), history, 0, store)
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
	store := artifacts.NewMemoryStore()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "active turn"}}
	refs := make([]artifacts.Ref, 3)
	for i, label := range []string{"first", "second", "third"} {
		refs[i] = putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, Data: []byte(strings.Repeat(label, 20_000))})
		id := "active-" + label
		history = append(history,
			messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: label, Arguments: `{}`}}},
			messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: id, ToolName: label, Content: artifactReceipt(refs[i]), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &refs[i]}}},
		)
	}

	projected, stats, err := projectMessages(context.Background(), history, 5_300, store)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if toolMessages[0].Content != artifactReceipt(refs[0]) {
		t.Fatalf("oldest active result was not spilled first: %q", toolMessages[0].Content[:min(200, len(toolMessages[0].Content))])
	}
	if !strings.Contains(toolMessages[len(toolMessages)-1].Content, "head/tail preview") {
		t.Fatalf("newest active result lost its preview: %q", toolMessages[len(toolMessages)-1].Content)
	}
	if stats.EstimatedTokens > 5_300 {
		t.Fatalf("projected tokens = %d, want <= 5300", stats.EstimatedTokens)
	}
}

func TestProjectSpillsOlderInlineActiveToolResultUnderPressure(t *testing.T) {
	store := artifacts.NewMemoryStore()
	first := strings.Repeat("first-inline-", 1_600)   // below 10k estimated tokens
	second := strings.Repeat("second-inline-", 1_500) // below 10k estimated tokens
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "active"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "first", Name: "first", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "first", ToolName: "first", Content: first},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "second", Name: "second", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "second", ToolName: "second", Content: second},
	}

	projected, stats, err := projectMessages(context.Background(), history, 6_000, store)
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

	projected, stats, err := projectMessages(context.Background(), history, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OmittedExchanges != 1 {
		t.Fatalf("omitted exchanges = %d, want 1", stats.OmittedExchanges)
	}
	joined := projectedText(projected)
	for _, want := range []string{"primary system", "later system invariant", "current question", "1 earlier completed exchange"} {
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
	projected, stats, err := projectMessages(context.Background(), history, 300, nil)
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
	}}, 100, nil)
	var limitErr *ContextLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want ContextLimitError", err)
	}
	if limitErr.Limit != 100 || limitErr.EstimatedTokens != stats.EstimatedTokens || limitErr.EstimatedTokens <= limitErr.Limit {
		t.Fatalf("context error = %+v, stats=%+v", limitErr, stats)
	}
}

func TestProjectHydratesOnlyExplicitImageSelection(t *testing.T) {
	store := artifacts.NewMemoryStore()
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

	projected, stats, err := projectMessages(context.Background(), history, 0, store)
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
	store := artifacts.NewMemoryStore()
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
		_, _, err := projectMessages(context.Background(), history, 0, store)
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("error = %v, want unavailable image reference", err)
		}
	})

	t.Run("ambiguous exact filename", func(t *testing.T) {
		history := append(cloneMessages(base), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "compare same.png"})
		_, _, err := projectMessages(context.Background(), history, 0, store)
		if err == nil || !strings.Contains(err.Error(), "matches multiple stored images") {
			t.Fatalf("error = %v, want filename ambiguity", err)
		}
	})

	t.Run("missing artifact bytes", func(t *testing.T) {
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "inspect"}, imageArtifactPart(first)}}}
		missingStore := artifacts.NewMemoryStore()
		_, _, err := projectMessages(context.Background(), history, 0, missingStore)
		if err == nil || !strings.Contains(err.Error(), "read image artifact") {
			t.Fatalf("error = %v, want missing artifact failure", err)
		}
	})
}

func TestProjectImageFilenameMatchingIsCaseSensitive(t *testing.T) {
	store := artifacts.NewMemoryStore()
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
		projected, _, err := projectMessages(context.Background(), history, 0, store)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(projectedImageParts(projected)); got != tc.want {
			t.Fatalf("prompt %q hydrated %d images, want %d", tc.prompt, got, tc.want)
		}
	}
}

func TestToolImageIsAttachedToExactlyFollowingRequest(t *testing.T) {
	store := artifacts.NewMemoryStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "tool.png", Data: []byte("tool-image")})
	firstRequest := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "make an image"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "one", Name: "render", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "one", ToolName: "render", Content: "rendered", Parts: []messages.ContentPart{imageArtifactPart(ref)}},
	}
	projected, _, err := projectMessages(context.Background(), firstRequest, 0, store)
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
	projected, _, err = projectMessages(context.Background(), secondRequest, 0, store)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(projectedImageParts(projected)); got != 0 {
		t.Fatalf("later request reattached prior tool image %d time(s): %#v", got, projected)
	}
}

func TestProjectDeduplicatesReadArtifactImageAlreadyReferencedByUser(t *testing.T) {
	store := artifacts.NewMemoryStore()
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

	projected, stats, err := projectMessages(context.Background(), history, 0, store)
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

func TestSpillActiveToolResultsExemptsReadArtifact(t *testing.T) {
	store := artifacts.NewMemoryStore()
	readContent := strings.Repeat("r", 20_000)
	bashContent := strings.Repeat("b", 36_000)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "go"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{
			{ID: "read", Name: "read_artifact", Arguments: `{}`},
			{ID: "bash", Name: "bash", Arguments: `{}`},
		}},
		{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: readContent},
		{Role: messages.MessageRoleTool, ToolCallID: "bash", ToolName: "bash", Content: bashContent},
	}
	projected, _, err := projectMessages(context.Background(), history, 9_000, store)
	if err != nil {
		t.Fatal(err)
	}
	toolMessages := messagesWithRole(projected, messages.MessageRoleTool)
	if toolMessages[0].Content != readContent {
		t.Fatalf("read_artifact result was spilled: %q", toolMessages[0].Content[:min(200, len(toolMessages[0].Content))])
	}
	if !strings.Contains(toolMessages[1].Content, "stored as artifact") {
		t.Fatalf("other tool result was not spilled: %q", toolMessages[1].Content[:min(200, len(toolMessages[1].Content))])
	}
}

func TestProjectToolResultsDegradesMissingArtifactToNote(t *testing.T) {
	mintStore := artifacts.NewMemoryStore()
	ref := putTestArtifact(t, mintStore, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "bash.txt", Data: []byte("lost output")})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "bash", Content: artifactReceipt(ref), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
	}
	projected, _, err := projectMessages(context.Background(), history, 0, artifacts.NewMemoryStore())
	if err != nil {
		t.Fatalf("missing blob failed the turn instead of degrading: %v", err)
	}
	last := projected[len(projected)-1]
	if !strings.Contains(last.Content, "no longer available") || !strings.Contains(last.Content, ref.ID) {
		t.Fatalf("missing blob note = %q", last.Content)
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
