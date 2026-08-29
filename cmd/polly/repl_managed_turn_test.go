package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestEnterWaitsForClipboardCapture(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.ed.setText("describe the clipboard image")
	r.model.clipboardCapture = true

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if got := r.model.ed.text(); got != "describe the clipboard image" {
		t.Fatalf("draft changed while clipboard capture was pending: %q", got)
	}
	if r.model.busy {
		t.Fatal("turn started before clipboard capture completed")
	}
	if _, ok := r.takePending(); ok {
		t.Fatal("turn reached pending before clipboard capture completed")
	}
	if got := r.model.transcript[len(r.model.transcript)-1]; !strings.Contains(got, "waiting for image capture") {
		t.Fatalf("missing clipboard wait notice: %q", got)
	}

	r.model.clipboardCapture = false
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	turn, ok := r.takePending()
	if !ok || turn.displayText != "describe the clipboard image" {
		t.Fatalf("completed clipboard draft did not start: %#v, ok=%t", turn, ok)
	}
}

func TestBusyAttachAppliesBeforeFollowingQueuedPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queued-attach.png")
	writeImageFixture(t, path, 8, 8)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("current turn")

	r.model.ed.setText("/attach " + path)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if len(r.model.queue) != 0 {
		t.Fatalf("busy /attach was queued instead of applied: %#v", r.model.queue)
	}
	if got := r.model.ed.text(); got != "[image #1] " {
		t.Fatalf("busy /attach composer = %q, want attachment token", got)
	}

	r.model.ed.setText("describe it " + r.model.ed.text())
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if len(r.model.queue) != 1 || r.model.queue[0].turn == nil {
		t.Fatalf("following prompt was not queued as a prepared turn: %#v", r.model.queue)
	}
	if got := managedImageData(t, r.model.queue[0].turn.userMessage); got == "" {
		t.Fatal("following queued prompt did not receive the busy /attach image")
	}
}

func TestQueuedAttachmentTurnKeepsPreparedBytesAfterSourceMutation(t *testing.T) {
	for _, mutation := range []string{"delete", "overwrite"} {
		t.Run(mutation, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "queued.png")
			writeImageFixture(t, path, 8, 8)
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "queued-"+mutation)
			artifactStore := session.ArtifactStore()

			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			r.state = &conversationState{session: session, artifactStore: artifactStore}
			r.model.artifactStore = artifactStore
			r.model.beginTurn("current turn")
			token := r.model.registerAttachment(path, "queued.png")
			r.model.ed.setText("inspect " + token)
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})

			if len(r.model.queue) != 1 || r.model.queue[0].turn == nil {
				t.Fatalf("queued prompt was not prepared: %#v", r.model.queue)
			}
			prepared := cloneChatMessage(r.model.queue[0].turn.userMessage)
			queuedTranscriptIndex := r.model.queue[0].transcriptIndex
			if len(managedImageBytes(t, prepared, artifactStore)) == 0 {
				t.Fatal("prepared turn has no persisted image bytes")
			}

			mutateAttachmentSource(t, path, mutation)
			r.endTurn(nil)

			seen := make(chan messages.ChatMessage, 1)
			done := r.startNextQueued(context.Background(), func(_ context.Context, _ string, turnUI TurnUI) error {
				seen <- cloneChatMessage(turnUI.(*gotuiTurnUI).turn.userMessage)
				return nil
			})
			if done == nil {
				t.Fatal("prepared queued turn did not start")
			}
			select {
			case got := <-seen:
				if !reflect.DeepEqual(got, prepared) {
					t.Fatalf("queued payload changed after %s:\n got %#v\nwant %#v", mutation, got, prepared)
				}
				if err := persistUserMessageForTurn(context.Background(), session, got, false); err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("queued turn did not reach runner")
			}
			images := r.model.transcriptImages[queuedTranscriptIndex]
			if len(images) != 1 {
				t.Fatalf("queued prepared preview = %+v", images)
			}
			previewBytes, err := os.ReadFile(images[0].Path)
			if err != nil {
				t.Fatal(err)
			}
			preparedBytes := managedImageBytes(t, prepared, artifactStore)
			if !bytes.Equal(previewBytes, preparedBytes) {
				t.Fatalf("queued preview changed after source %s", mutation)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if history := testSessionHistory(t, session); len(history) != 1 || !reflect.DeepEqual(history[0], prepared) {
				t.Fatalf("queued attachment produced non-durable or duplicate user turns: %#v", history)
			}
		})
	}
}

func TestRestoredAttachmentDraftReusesExactPreparedMessageAfterSourceMutation(t *testing.T) {
	for _, mutation := range []string{"delete", "overwrite"} {
		for _, outcome := range []struct {
			name string
			err  error
		}{
			{name: "failed", err: errors.New("provider failed")},
			{name: "canceled", err: context.Canceled},
		} {
			t.Run(mutation+"/"+outcome.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "retry.png")
				writeImageFixture(t, path, 8, 8)
				store := testOpenMemoryStore(t, nil)
				session := testAcquireSession(t, store, "retry")
				artifactStore := session.ArtifactStore()

				r := newManagedREPL(&Config{}, "ctx", 0, 0)
				r.state = &conversationState{session: session, artifactStore: artifactStore}
				r.model.artifactStore = artifactStore
				token := r.model.registerAttachment(path, "retry.png")
				r.model.ed.setText("inspect " + token)
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
				prepared, ok := r.takePending()
				if !ok {
					t.Fatal("accepted attachment turn was not pending")
				}
				if err := persistUserMessageForTurn(context.Background(), session, prepared.userMessage, false); err != nil {
					t.Fatal(err)
				}
				r.endTurn(outcome.err)

				mutateAttachmentSource(t, path, mutation)
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
				retried, ok := r.takePending()
				if !ok {
					t.Fatal("restored draft did not enqueue an exact turn")
				}
				if !reflect.DeepEqual(retried.userMessage, prepared.userMessage) {
					t.Fatalf("restored payload changed after %s", mutation)
				}
				if !r.model.restoreDraftNext {
					t.Fatal("unchanged restored draft did not request persisted-user reuse")
				}
				if err := persistUserMessageForTurn(context.Background(), session, retried.userMessage, r.model.restoreDraftNext); err != nil {
					t.Fatal(err)
				}
				if got := testSessionHistory(t, session); len(got) != 1 || !reflect.DeepEqual(got[0], prepared.userMessage) {
					t.Fatalf("restored submit created non-durable or duplicate user turns: %#v", got)
				}
			})
		}
	}
}

func TestFailedBarePathTurnRestoresAsPlainText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bare.png")
	writeImageFixture(t, path, 8, 8)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "bare-restore")

	// A bare typed path is prose now: no attachment, so the failed draft
	// restores byte-for-byte with the path still in it, and the model is
	// expected to call view_image on it.
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{session: session, artifactStore: session.ArtifactStore()}
	r.model.artifactStore = session.ArtifactStore()
	r.model.ed.setText("inspect " + path)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	prepared, ok := r.takePending()
	if !ok {
		t.Fatal("turn was not accepted")
	}
	for _, part := range prepared.userMessage.Parts {
		if part.Type == "image_base64" || part.Type == "image_artifact" {
			t.Fatalf("bare path produced an image part: %#v", part)
		}
	}
	r.endTurn(errors.New("provider failed"))

	draft := r.model.ed.text()
	if draft != "inspect "+path {
		t.Fatalf("restored draft = %q, want the original text unchanged", draft)
	}
}

func TestDiskSessionReloadRestoresPersistedImageWithoutSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reload.png")
	writeImageFixture(t, path, 8, 8)
	prepared, err := buildREPLUserMessage("inspect reload", []composerAttachment{{Path: path, Label: "reload.png"}})
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dir, "polly.db")
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "image-reload")
	prepared, err = externalizeMessageImages(context.Background(), prepared, session.ArtifactStore())
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
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	reopenedStore := testOpenDiskStore(t, dbPath, nil)
	reopened := testAcquireSession(t, reopenedStore, "image-reload")
	r := newManagedREPL(&Config{}, "image-reload", 0, 0)
	r.state = &conversationState{session: reopened, artifactStore: reopened.ArtifactStore()}
	r.model.artifactStore = reopened.ArtifactStore()
	r.model.hydrateHistory(testSessionHistory(t, reopened), "image-reload")
	if r.model.restoredDraft == nil {
		t.Fatal("reloaded incomplete image turn did not restore exact draft")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	retried, ok := r.takePending()
	if !ok || !reflect.DeepEqual(retried.userMessage, prepared) {
		t.Fatalf("reloaded restored payload = %#v, want exact persisted message", retried.userMessage)
	}
	if !r.model.restoreDraftNext {
		t.Fatal("reloaded restored draft did not reuse persisted user")
	}
	if err := persistUserMessageForTurn(context.Background(), reopened, retried.userMessage, r.model.restoreDraftNext); err != nil {
		t.Fatal(err)
	}
	if got := testSessionHistory(t, reopened); len(got) != 1 {
		t.Fatalf("reloaded restored draft duplicated durable user turn: %#v", got)
	}
}

func TestAttachmentProjectionDoesNotChargeHistoricalImages(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "budget")
	if err := session.AddMessage(context.Background(), messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: strings.Repeat("A", maxEncodedImageHistoryBytes), MimeType: "image/png",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "candidate.png")
	writeImageFixture(t, path, 8, 8)
	r := newManagedREPL(&Config{}, "budget", 0, 0)
	r.state = &conversationState{session: session, artifactStore: session.ArtifactStore()}
	r.model.artifactStore = session.ArtifactStore()
	token := r.model.registerAttachment(path, "candidate.png")
	draft := "inspect " + token
	r.model.ed.setText(draft)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})

	if got := r.model.ed.text(); got != "" {
		t.Fatalf("accepted image prompt left draft behind: %q", got)
	}
	turn, ok := r.takePending()
	if !ok || turn.displayText != draft {
		t.Fatalf("historical image suppressed candidate turn: %#v, ok=%t", turn, ok)
	}
	if transcript := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n")); strings.Contains(transcript, "portable limit") {
		t.Fatalf("historical image caused a local budget error: %q", transcript)
	}
}

func TestAttachmentPreparationFailuresLeaveComposerDraft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *managedREPL) string
		want  string
	}{
		{
			name: "unknown token",
			setup: func(_ *testing.T, _ *managedREPL) string {
				return "inspect [image #42]"
			},
			want: "unknown attachment token",
		},
		{
			name: "missing source",
			setup: func(t *testing.T, r *managedREPL) string {
				path := filepath.Join(t.TempDir(), "missing.png")
				writeImageFixture(t, path, 8, 8)
				token := r.model.registerAttachment(path, "missing.png")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				return "inspect " + token
			},
			want: "missing.png",
		},
		{
			name: "too many images",
			setup: func(t *testing.T, r *managedREPL) string {
				dir := t.TempDir()
				var prompt strings.Builder
				for i := 0; i <= maxPromptAttachments; i++ {
					path := filepath.Join(dir, "image-"+string(rune('a'+i))+".png")
					writeImageFixture(t, path, 2, 2)
					if i > 0 {
						prompt.WriteByte(' ')
					}
					prompt.WriteString(r.model.registerAttachment(path, filepath.Base(path)))
				}
				return prompt.String()
			},
			want: "maximum is 16",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			draft := tc.setup(t, r)
			r.model.ed.setText(draft)
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})

			if got := r.model.ed.text(); got != draft {
				t.Fatalf("failed preparation cleared draft: %q", got)
			}
			if r.model.busy || len(r.model.queue) != 0 {
				t.Fatalf("failed preparation was accepted: busy=%v queue=%#v", r.model.busy, r.model.queue)
			}
			if _, ok := r.takePending(); ok {
				t.Fatal("failed preparation entered pending queue")
			}
			plain := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
			if !strings.Contains(plain, tc.want) {
				t.Fatalf("local error %q does not contain %q", plain, tc.want)
			}
		})
	}
}

func TestManagedTurnPreparationDoesNotReplayUnpersistedCurrentImages(t *testing.T) {
	data := strings.Repeat("A", (maxEncodedImageHistoryBytes/2)+1)
	imageMessage := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: data, MimeType: "image/png",
		}},
	}
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "identical-current")
	if err := session.AddMessages(context.Background(), []messages.ChatMessage{
		imageMessage,
		{Role: messages.MessageRoleAssistant, Content: "completed earlier"},
	}); err != nil {
		t.Fatal(err)
	}

	r := newManagedREPL(&Config{}, "identical-current", 0, 0)
	r.state = &conversationState{session: session, artifactStore: session.ArtifactStore()}
	r.model.artifactStore = session.ArtifactStore()
	r.model.beginManagedTurn(managedTurnInput{displayText: "same image again", userMessage: imageMessage})
	path := filepath.Join(t.TempDir(), "candidate.png")
	writeImageFixture(t, path, 2, 2)
	token := r.model.registerAttachment(path, "candidate.png")

	if _, err := r.prepareManagedTurnLocked("inspect " + token); err != nil {
		t.Fatalf("candidate was incorrectly charged for a prior current-turn image: %v", err)
	}
}

func TestProjectedBudgetHonorsQueuedResetBarrier(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "reset-budget")
	if err := session.AddMessage(context.Background(), messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: strings.Repeat("A", maxEncodedImageHistoryBytes), MimeType: "image/png",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	r := newManagedREPL(&Config{}, "reset-budget", 0, 0)
	r.state = &conversationState{session: session, artifactStore: session.ArtifactStore()}
	r.model.artifactStore = session.ArtifactStore()
	r.model.beginTurn("current")
	r.model.queue = []queuedREPLInput{{text: "/reset confirm"}}
	path := filepath.Join(t.TempDir(), "after-reset.png")
	writeImageFixture(t, path, 2, 2)
	token := r.model.registerAttachment(path, "after-reset.png")

	if _, err := r.prepareManagedTurnLocked("inspect " + token); err != nil {
		t.Fatalf("post-reset candidate counted cleared image history: %v", err)
	}
}

func TestManagedTurnPreparationDoesNotWaitForPersistenceAck(t *testing.T) {
	imageMessage := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: portablePNGBase64Size(t, 9<<20), MimeType: "image/png",
		}},
	}
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "persistence-ack")
	r := newManagedREPL(&Config{}, "persistence-ack", 0, 0)
	r.state = &conversationState{session: session, artifactStore: session.ArtifactStore()}
	r.model.artifactStore = session.ArtifactStore()
	r.model.beginManagedTurn(managedTurnInput{displayText: "current image", userMessage: imageMessage})
	tui := &gotuiTurnUI{repl: r, persistence: r.model.currentPersistence}
	tui.UserMessagePersistenceStarted()
	if err := session.AddMessage(context.Background(), imageMessage); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "candidate.png")
	writeImageFixture(t, path, 2, 2)
	token := r.model.registerAttachment(path, "candidate.png")

	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		r.model.mu.Lock()
		close(started)
		_, err := r.prepareManagedTurnLocked("inspect " + token)
		r.model.mu.Unlock()
		result <- err
	}()
	<-started
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("managed turn preparation failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("managed turn preparation waited on unrelated persistence")
	}
	tui.UserMessagePersistenceFinished(true)
}

func managedImageData(t *testing.T, msg messages.ChatMessage) string {
	t.Helper()
	for _, part := range msg.Parts {
		if part.Type == "image_base64" {
			return part.ImageData
		}
	}
	t.Fatal("message has no image_base64 part")
	return ""
}

func managedImageBytes(t *testing.T, msg messages.ChatMessage, store interface {
	Open(context.Context, string) (io.ReadCloser, error)
}) []byte {
	t.Helper()
	for _, part := range msg.Parts {
		if part.Type == "image_base64" {
			data, err := base64.StdEncoding.DecodeString(part.ImageData)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		if part.Artifact != nil {
			reader, err := store.Open(context.Background(), part.Artifact.ID)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			closeErr := reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			return data
		}
	}
	return nil
}

func mutateAttachmentSource(t *testing.T, path, mutation string) {
	t.Helper()
	switch mutation {
	case "delete":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "overwrite":
		writeImageFixture(t, path, 13, 11)
	default:
		t.Fatalf("unknown mutation %q", mutation)
	}
}

func queuedTextInputs(inputs ...string) []queuedREPLInput {
	queue := make([]queuedREPLInput, 0, len(inputs))
	for _, input := range inputs {
		item := queuedREPLInput{text: input}
		if strings.Contains(input, "\n") || !strings.HasPrefix(input, "/") {
			turn := textManagedTurn(input)
			item.turn = &turn
		}
		queue = append(queue, item)
	}
	return queue
}
