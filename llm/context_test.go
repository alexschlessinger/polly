package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

func TestProjectKeepsBoundedReadArtifactResultInline(t *testing.T) {
	store := newTestArtifactStore()
	content := strings.Repeat("x", artifactReadMaxBytes)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read it"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_artifact", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "read", ToolName: "read_artifact", Content: content},
	}
	projected, _, err := projectMessages(context.Background(), history, store, false)
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
	projected, _, err := projectMessages(context.Background(), history, failingArtifactStore{}, false)
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

	projected, stats, err := projectMessages(context.Background(), history, store, false)
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
		_, _, err := projectMessages(context.Background(), history, store, false)
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("error = %v, want unavailable image reference", err)
		}
	})

	t.Run("ambiguous exact filename", func(t *testing.T) {
		history := append(cloneMessages(base), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "compare same.png"})
		_, _, err := projectMessages(context.Background(), history, store, false)
		if err == nil || !strings.Contains(err.Error(), "matches multiple stored images") {
			t.Fatalf("error = %v, want filename ambiguity", err)
		}
	})

	t.Run("missing artifact bytes", func(t *testing.T) {
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "inspect"}, imageArtifactPart(first)}}}
		missingStore := newTestArtifactStore()
		_, _, err := projectMessages(context.Background(), history, missingStore, false)
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
		projected, _, err := projectMessages(context.Background(), history, store, false)
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
	projected, _, err := projectMessages(context.Background(), firstRequest, store, false)
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
	projected, _, err = projectMessages(context.Background(), secondRequest, store, false)
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

	projected, stats, err := projectMessages(context.Background(), history, store, false)
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

func TestAgentRejectsBudgetSmallerThanItsFirstRequest(t *testing.T) {
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	_, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("hi"), MaxContextTokens: 300,
	}, nil)
	var limit *ContextLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("Run error = %v, want a context limit error", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("provider was called despite schema overflow")
	}
}
