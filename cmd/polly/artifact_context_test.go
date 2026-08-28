package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/gif"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestExternalizeMessageImagesStoresExactBytesAndSurfacesFailure(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(portablePNGBase64Size(t, 400))
	if err != nil {
		t.Fatal(err)
	}
	message := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect [image #3]"},
			{Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data), MimeType: "image/png", FileName: "pixel.png", Reference: "[image #3]"},
		},
	}
	store := testArtifactStore(t)
	externalized, err := externalizeMessageImages(context.Background(), message, store)
	if err != nil {
		t.Fatal(err)
	}
	part := externalized.Parts[1]
	if part.Type != "image_artifact" || part.Artifact == nil || part.ImageData != "" || part.Artifact.ImageToken != "[image #3]" {
		t.Fatalf("externalized part = %#v", part)
	}
	r, err := store.Open(context.Background(), part.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !reflect.DeepEqual(stored, data) {
		t.Fatalf("stored bytes = %q, %v", stored, err)
	}
	if message.Parts[1].Type != "image_base64" || message.Parts[1].ImageData == "" {
		t.Fatalf("externalization mutated caller message: %#v", message)
	}

	if _, err := externalizeMessageImages(context.Background(), message, failingContextArtifactStore{}); err == nil || !strings.Contains(err.Error(), "artifact storage failed") {
		t.Fatalf("storage failure = %v, want propagated artifact error", err)
	}
}

func TestManagedTurnCarriesArtifactAfterSourceDisappears(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "queued.png")
	writeImageFixture(t, path, 8, 8)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "artifact-queue")
	artifactStore := session.ArtifactStore()
	r := newManagedREPL(&Config{}, "artifact-queue", 0, 0)
	r.state = &conversationState{session: session, artifactStore: artifactStore}
	r.model.artifactStore = artifactStore
	token := r.model.registerAttachment(path, "queued.png")

	turn, err := r.prepareManagedTurnLocked("inspect " + token)
	if err != nil {
		t.Fatal(err)
	}
	var ref *artifacts.Ref
	for _, part := range turn.userMessage.Parts {
		if part.Artifact != nil {
			copyRef := *part.Artifact
			ref = &copyRef
		}
		if part.Type == "image_base64" || part.ImageData != "" {
			t.Fatalf("managed turn retained inline image data: %#v", turn.userMessage)
		}
	}
	if ref == nil || ref.ImageToken != token {
		t.Fatalf("managed turn artifact = %#v", ref)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if images := preparedMessageTranscriptImagesWithStore(turn.userMessage, artifactStore); len(images) != 1 {
		t.Fatalf("artifact-backed preview after source removal = %#v", images)
	}
	if _, err := artifactStore.Open(context.Background(), ref.ID); err != nil {
		t.Fatalf("prepared bytes did not survive source removal: %v", err)
	}
}

func TestReloadRestoresStableImageTokenAndExactRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "polly.db")
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "reload")
	artifactStore := session.ArtifactStore()
	path := filepath.Join(t.TempDir(), "reload.png")
	writeImageFixture(t, path, 4, 4)
	preparedPart, err := prepareImageForUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	preparedPart.Reference = "[image #1]"
	prepared, err := externalizeMessageImages(context.Background(), messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect [image #1]"},
			*preparedPart,
		},
	}, artifactStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore := testOpenDiskStore(t, dbPath, nil)
	reopened := testAcquireSession(t, reopenedStore, "reload")
	reopenedArtifacts := reopened.ArtifactStore()
	r := newManagedREPL(&Config{}, "reload", 0, 0)
	r.state = &conversationState{session: reopened, artifactStore: reopenedArtifacts}
	r.model.artifactStore = reopenedArtifacts
	r.model.hydrateHistory(testSessionHistory(t, reopened), "reload")

	if r.model.restoredDraft == nil || !reflect.DeepEqual(r.model.restoredDraft.userMessage, prepared) {
		t.Fatalf("reloaded exact draft = %#v, want %#v", r.model.restoredDraft, prepared)
	}
	attachments, err := r.model.promptAttachments("compare [image #1]")
	if err != nil || len(attachments) != 1 || attachments[0].Artifact == nil || attachments[0].Artifact.ID != prepared.Parts[1].Artifact.ID {
		t.Fatalf("restored stable token = %#v, %v", attachments, err)
	}

	newPath := filepath.Join(t.TempDir(), "new.png")
	writeImageFixture(t, newPath, 2, 2)
	if token := r.model.registerAttachment(newPath, "new.png"); token != "[image #2]" {
		t.Fatalf("post-restart token = %q, want [image #2]", token)
	}
	reopened.Close()
}

func TestReloadKeepsCollidingStableImageTokenAmbiguous(t *testing.T) {
	store := testArtifactStore(t)
	first, err := store.Put(context.Background(), artifacts.Blob{
		Kind: artifacts.KindImage, MIMEType: "image/png", ImageToken: "[image #1]", Data: []byte("first"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), artifacts.Blob{
		Kind: artifacts.KindImage, MIMEType: "image/png", ImageToken: "[image #1]", Data: []byte("second"),
	})
	if err != nil {
		t.Fatal(err)
	}
	m := newReplModel()
	m.artifactStore = store
	for _, ref := range []artifacts.Ref{first, second, first} {
		copyRef := ref
		m.rememberArtifactAttachments(messages.ChatMessage{Parts: []messages.ContentPart{{
			Type: "image_artifact", Reference: "[image #1]", Artifact: &copyRef,
		}}})
	}
	if _, err := m.promptAttachments("compare [image #1]"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("colliding stable token error = %v", err)
	}
}

func TestQueuedImageSurvivesResetWhileClearedArtifactsDoNot(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "queued-reset-artifact")
	artifactStore := session.ArtifactStore()
	oldRef, err := artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("old history")})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AddMessage(context.Background(), messages.ChatMessage{
		Role: messages.MessageRoleTool, Content: "old", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &oldRef}},
	}); err != nil {
		t.Fatal(err)
	}
	queuedBytes, err := base64.StdEncoding.DecodeString(portablePNGBase64Size(t, 400))
	if err != nil {
		t.Fatal(err)
	}
	queuedRef, err := artifactStore.Put(context.Background(), artifacts.Blob{
		Kind: artifacts.KindImage, MIMEType: "image/png", Name: "queued.png", Reference: "[image #2]", Data: queuedBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	queuedTurn := managedTurnInput{displayText: "inspect [image #2]", userMessage: messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect [image #2]"},
			{Type: "image_artifact", Reference: "[image #2]", Artifact: &queuedRef},
		},
	}}
	r := newManagedREPL(&Config{}, "queued-reset-artifact", 0, 0)
	r.state = &conversationState{session: session, artifactStore: artifactStore}
	r.model.artifactStore = artifactStore
	r.model.queue = []queuedREPLInput{{text: queuedTurn.displayText, turn: &queuedTurn}}

	r.model.mu.Lock()
	handled, quit := r.runCommand("/reset confirm")
	r.model.mu.Unlock()
	if !handled || quit {
		t.Fatalf("reset handled=%v quit=%v", handled, quit)
	}
	if history := testSessionHistory(t, session); len(history) != 0 {
		t.Fatalf("reset retained durable history: %#v", history)
	}
	if _, err := artifactStore.Open(context.Background(), oldRef.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleared artifact still exists: %v", err)
	}
	if len(r.model.queue) != 1 || r.model.queue[0].turn == nil {
		t.Fatalf("queued turn was lost: %#v", r.model.queue)
	}
	restored := r.model.queue[0].turn.userMessage.Parts[1].Artifact
	if restored == nil || restored.ID != queuedRef.ID {
		t.Fatalf("queued image was not re-externalized: %#v", r.model.queue[0].turn.userMessage)
	}
	reader, err := artifactStore.Open(context.Background(), restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !reflect.DeepEqual(data, queuedBytes) {
		t.Fatalf("restored queued bytes = %q, %v", data, err)
	}
	if attachment := r.model.attachments[2]; attachment.Artifact == nil || attachment.Artifact.ID != queuedRef.ID {
		t.Fatalf("stable queued token was not restored: %#v", r.model.attachments)
	}
}

func TestExactInlineRetryDoesNotChangeRepresentationWhenStoreRecovers(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "inline-retry")
	inline := messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{
		Type: "image_base64", ImageData: portablePNGBase64Size(t, 400), MimeType: "image/png",
	}}}
	if err := session.AddMessage(context.Background(), inline); err != nil {
		t.Fatal(err)
	}
	artifactStore := session.ArtifactStore()
	model := &captureCompletionLLM{response: messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}
	registry := tools.NewToolRegistry(nil)
	state := &conversationState{
		session: session, artifactStore: artifactStore, toolRegistry: registry,
		agent: llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
	}
	config := &Config{Settings: Settings{Model: "test/model", MaxTokens: 128}}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr

	code, err := executeTurnWithUserMessage(context.Background(), config, state, inline, nil, nil, ui, true)
	if err != nil || code != 0 {
		t.Fatalf("inline retry = code %d, err %v", code, err)
	}
	history := testSessionHistory(t, session)
	if len(history) != 2 || history[0].Parts[0].Type != "image_base64" || history[0].Parts[0].Artifact != nil {
		t.Fatalf("exact retry duplicated or rewrote persisted user: %#v", history)
	}
}

func TestTurnSurfacesOneOmissionNoticeAndRetainsDurableTranscript(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "omission-notice")
	if err := session.AddMessages(context.Background(), []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
	}); err != nil {
		t.Fatal(err)
	}
	model := &captureCompletionLLM{response: messages.ChatMessage{
		Role: messages.MessageRoleAssistant, Content: "new answer", StopReason: messages.StopReasonEndTurn,
	}}
	registry := tools.NewToolRegistry(nil)
	artifactStore := session.ArtifactStore()
	state := &conversationState{
		session:       session,
		agent:         llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
		toolRegistry:  registry,
		artifactStore: artifactStore,
	}
	config := &Config{Settings: Settings{Model: "test/model", MaxTokens: 128, MaxHistoryTokens: 200}}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer = &stdout
	ui.errWriter = &stderr

	code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{
		Role: messages.MessageRoleUser, Content: "current question",
	}, nil, nil, ui, false)
	if err != nil || code != 0 {
		t.Fatalf("execute turn = code %d, err %v", code, err)
	}
	if got := strings.Count(stderr.String(), "model context omitted 1 earlier exchange; full transcript retained"); got != 1 {
		t.Fatalf("omission notices = %d, stderr=%q", got, stderr.String())
	}
	if history := testSessionHistory(t, session); len(history) != 4 {
		t.Fatalf("durable transcript was trimmed: %#v", history)
	}
	joinedRequest := projectedRequestText(model.request)
	if strings.Contains(joinedRequest, "old answer") || !strings.Contains(joinedRequest, "current question") || !strings.Contains(joinedRequest, "earlier completed exchange") {
		t.Fatalf("provider request projection = %#v", model.request)
	}
}

type failingContextArtifactStore struct{}

func (failingContextArtifactStore) Put(context.Context, artifacts.Blob) (artifacts.Ref, error) {
	return artifacts.Ref{}, errors.New("artifact storage failed")
}

func (failingContextArtifactStore) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("artifact storage failed")
}

func (failingContextArtifactStore) RemoveAll(context.Context) error { return nil }

type captureCompletionLLM struct {
	request  []messages.ChatMessage
	response messages.ChatMessage
}

func (c *captureCompletionLLM) ChatCompletionStream(_ context.Context, req *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	c.request = sessions.CopyHistory(req.Messages)
	input := make(chan messages.ChatMessage, 1)
	input <- c.response
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func projectedRequestText(history []messages.ChatMessage) string {
	var b strings.Builder
	for _, msg := range history {
		b.WriteString(msg.Content)
		for _, part := range msg.Parts {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func TestTurnRejectsPoisonPromptsBeforePersist(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "pre-persist")
	registry := tools.NewToolRegistry(nil)
	artifactStore := session.ArtifactStore()
	model := &captureCompletionLLM{response: messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}
	state := &conversationState{
		session: session, artifactStore: artifactStore, toolRegistry: registry,
		agent: llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
	}
	config := &Config{Settings: Settings{Model: "test/model", MaxTokens: 128, MaxHistoryTokens: 200}}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr

	t.Run("unresolvable image reference", func(t *testing.T) {
		code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{
			Role: messages.MessageRoleUser, Content: "the docs mention [image #4]; what is it?",
		}, nil, nil, ui, false)
		if err == nil || !strings.Contains(err.Error(), "not available") || code != 1 {
			t.Fatalf("turn = code %d, err %v; want pre-persist rejection", code, err)
		}
		if history := testSessionHistory(t, session); len(history) != 0 {
			t.Fatalf("orphaned user message persisted: %#v", history)
		}
	})

	t.Run("prompt exceeding the context budget", func(t *testing.T) {
		code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{
			Role: messages.MessageRoleUser, Content: strings.Repeat("x", 4_000),
		}, nil, nil, ui, false)
		if err == nil || !strings.Contains(err.Error(), "context budget") || code != 1 {
			t.Fatalf("turn = code %d, err %v; want pre-persist rejection", code, err)
		}
		if history := testSessionHistory(t, session); len(history) != 0 {
			t.Fatalf("oversized prompt was persisted: %#v", history)
		}
	})
}

func TestExternalizeMessageImagesNormalizesLegacyGIF(t *testing.T) {
	var encoded bytes.Buffer
	if err := gif.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	message := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(encoded.Bytes()),
			MimeType: "image/gif", FileName: "legacy.gif", Reference: "[image #1]",
		}},
	}
	store := testArtifactStore(t)
	externalized, err := externalizeMessageImages(context.Background(), message, store)
	if err != nil {
		t.Fatal(err)
	}
	part := externalized.Parts[0]
	if part.Type != "image_artifact" || part.Artifact == nil || part.MimeType != "image/png" || part.Artifact.MIMEType != "image/png" {
		t.Fatalf("legacy GIF was not normalized before storage: %#v", part)
	}
	r, err := store.Open(context.Background(), part.Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, format, err := image.Decode(bytes.NewReader(stored)); err != nil || format != "png" {
		t.Fatalf("stored artifact bytes are %q (%v), want png", format, err)
	}
}

func TestResetPreservesAttachmentTokenMonotonicity(t *testing.T) {
	m := newReplModel()
	if token := m.registerAttachment("/tmp/a.png", "a.png"); token != "[image #1]" {
		t.Fatalf("first token = %q", token)
	}
	if token := m.registerAttachment("/tmp/b.png", "b.png"); token != "[image #2]" {
		t.Fatalf("second token = %q", token)
	}
	m.restoreQueuedImagesAfterReset(context.Background(), nil)
	if len(m.attachments) != 0 {
		t.Fatalf("reset retained attachment bindings: %#v", m.attachments)
	}
	if token := m.registerAttachment("/tmp/c.png", "c.png"); token != "[image #3]" {
		t.Fatalf("post-reset token = %q; reusing a number would rebind surviving history/draft tokens", token)
	}
}
