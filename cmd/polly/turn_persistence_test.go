package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestPersistUserMessageForTurnReusesMatchingFinalUser(t *testing.T) {
	session := newTurnPersistenceTestSession(t)
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "try this"}
	if err := session.AddMessage(userMsg); err != nil {
		t.Fatalf("AddMessage() error = %v", err)
	}

	if err := persistUserMessageForTurn(session, userMsg, true); err != nil {
		t.Fatalf("persistUserMessageForTurn() error = %v", err)
	}
	if got := session.GetHistory(); !slices.EqualFunc(got, []messages.ChatMessage{userMsg}, equalTurnTestMessage) {
		t.Fatalf("matching retry should reuse final user message, history = %#v", got)
	}
}

func TestPersistUserMessageForTurnOnlyReusesOnExplicitMatchingRetry(t *testing.T) {
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "try this"}

	tests := []struct {
		name     string
		reuse    bool
		history  []messages.ChatMessage
		wantSize int
	}{
		{
			name:     "ordinary turn persists duplicate text",
			history:  []messages.ChatMessage{userMsg},
			wantSize: 2,
		},
		{
			name:     "empty history",
			reuse:    true,
			wantSize: 1,
		},
		{
			name:     "different final user",
			reuse:    true,
			history:  []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "something else"}},
			wantSize: 2,
		},
		{
			name:  "matching user is not final",
			reuse: true,
			history: []messages.ChatMessage{
				userMsg,
				{Role: messages.MessageRoleAssistant, Content: "partial"},
			},
			wantSize: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := newTurnPersistenceTestSession(t)
			if err := session.AddMessages(tt.history); err != nil {
				t.Fatalf("AddMessages() error = %v", err)
			}
			if err := persistUserMessageForTurn(session, userMsg, tt.reuse); err != nil {
				t.Fatalf("persistUserMessageForTurn() error = %v", err)
			}
			got := session.GetHistory()
			if len(got) != tt.wantSize {
				t.Fatalf("history length = %d, want %d; history = %#v", len(got), tt.wantSize, got)
			}
			if last := got[len(got)-1]; !equalTurnTestMessage(last, userMsg) {
				t.Fatalf("last message = %#v, want persisted user %#v", last, userMsg)
			}
		})
	}
}

func TestPersistUserMessageForTurnComparesAttachedParts(t *testing.T) {
	userMsg := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect this"},
			{Type: "file", FileName: "notes.txt", MimeType: "text/plain", Text: "alpha"},
		},
	}

	t.Run("same payload is reused", func(t *testing.T) {
		session := newTurnPersistenceTestSession(t)
		if err := session.AddMessage(userMsg); err != nil {
			t.Fatalf("AddMessage() error = %v", err)
		}
		if err := persistUserMessageForTurn(session, userMsg, true); err != nil {
			t.Fatalf("persistUserMessageForTurn() error = %v", err)
		}
		if got := len(session.GetHistory()); got != 1 {
			t.Fatalf("same attached payload should be reused, history length = %d", got)
		}
	})

	t.Run("changed payload is persisted", func(t *testing.T) {
		session := newTurnPersistenceTestSession(t)
		oldMsg := userMsg
		oldMsg.Parts = slices.Clone(userMsg.Parts)
		oldMsg.Parts[1].Text = "older contents"
		if err := session.AddMessage(oldMsg); err != nil {
			t.Fatalf("AddMessage() error = %v", err)
		}
		if err := persistUserMessageForTurn(session, userMsg, true); err != nil {
			t.Fatalf("persistUserMessageForTurn() error = %v", err)
		}
		if got := len(session.GetHistory()); got != 2 {
			t.Fatalf("changed attached payload must be persisted, history length = %d", got)
		}
	})
}

func TestPrepareSessionImageRequestDoesNotDuplicateExactRetry(t *testing.T) {
	session := newTurnPersistenceTestSession(t)
	userMsg := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type:      "image_base64",
			ImageData: portablePNGBase64Size(t, 400),
			MimeType:  "image/png",
		}},
	}
	if err := session.AddMessage(userMsg); err != nil {
		t.Fatal(err)
	}

	retry, err := prepareSessionImageRequest(session, userMsg, true)
	if err != nil || len(retry) != 1 {
		t.Fatalf("exact retry projection = (%d, %v), want one message", len(retry), err)
	}
	ordinary, err := prepareSessionImageRequest(session, userMsg, false)
	if err != nil || len(ordinary) != 2 {
		t.Fatalf("ordinary duplicate projection = (%d, %v), want two messages", len(ordinary), err)
	}
}

func TestValidateEncodedImageBudgetIncludesDataURLs(t *testing.T) {
	history := []messages.ChatMessage{{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "image_base64", ImageData: strings.Repeat("A", 8<<20)},
			{Type: "image_url", ImageURL: "data:image/png;base64," + strings.Repeat("B", (8<<20)+1)},
		},
	}}
	if err := validateEncodedImageBudget(history); err == nil || !strings.Contains(err.Error(), "portable limit is 16 MiB") {
		t.Fatalf("aggregate encoded-image overflow = %v", err)
	}
}

func TestPrepareSessionImageRequestRetainsHistoryBeyondModelBudget(t *testing.T) {
	store := sessions.NewSyncMapSessionStore(&sessions.Metadata{MaxHistoryTokens: 2000})
	session, err := store.Get("trimmed-budget")
	if err != nil {
		t.Fatal(err)
	}
	oldImage := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: strings.Repeat("A", maxEncodedImageHistoryBytes), MimeType: "image/png",
		}},
	}
	if err := session.AddMessages([]messages.ChatMessage{
		oldImage,
		{Role: messages.MessageRoleAssistant, Content: "completed old turn"},
	}); err != nil {
		t.Fatal(err)
	}
	candidate := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: portablePNGBase64Size(t, 400), MimeType: "image/png",
		}},
	}

	prepared, err := prepareSessionImageRequest(session, candidate, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 3 || prepared[0].Parts[0].ImageData != oldImage.Parts[0].ImageData {
		t.Fatalf("durable history was trimmed before agent projection: %#v", prepared)
	}
}

func TestPrepareSessionImageRequestNormalizesLegacyImageWithoutRewriting(t *testing.T) {
	session := newTurnPersistenceTestSession(t)
	if err := session.AddMessages([]messages.ChatMessage{
		{
			Role: messages.MessageRoleUser,
			Parts: []messages.ContentPart{{
				Type: "image_base64", ImageData: "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==", MimeType: "image/gif",
			}},
		},
		{Role: messages.MessageRoleAssistant, Content: "legacy response"},
	}); err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareSessionImageRequest(session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "next"}, false)
	if err != nil {
		t.Fatalf("legacy GIF poisoned the upgraded request: %v", err)
	}
	if len(prepared) != 3 || len(prepared[0].Parts) != 1 || prepared[0].Parts[0].MimeType != "image/png" {
		t.Fatalf("legacy image was not normalized for agent projection: %#v", prepared)
	}
	if got := session.GetHistory()[0].Parts[0].MimeType; got != "image/gif" {
		t.Fatalf("request-only migration rewrote durable history MIME to %q", got)
	}
}

func TestValidatePortableImageRequestEnforcesRequestWideImageLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.png")
	writeImageFixture(t, path, 1, 1)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	part := messages.ContentPart{Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data), MimeType: "image/png"}
	makeHistory := func(count int) []messages.ChatMessage {
		var history []messages.ChatMessage
		for count > 0 {
			n := min(count, maxPromptAttachments)
			parts := make([]messages.ContentPart, n)
			for i := range parts {
				parts[i] = part
			}
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Parts: parts})
			count -= n
		}
		return history
	}
	if err := validatePortableImageRequest(makeHistory(maxPortableRequestImages)); err != nil {
		t.Fatalf("request-wide image limit rejected: %v", err)
	}
	err = validatePortableImageRequest(makeHistory(maxPortableRequestImages + 1))
	if err == nil || !strings.Contains(err.Error(), "portable request maximum is 100") {
		t.Fatalf("request-wide image overflow = %v", err)
	}
}

func TestValidatePortableImageRequestPerImageEncodedLimit(t *testing.T) {
	atLimit := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: portablePNGBase64Size(t, maxPortableEncodedImageBytes), MimeType: "image/png",
		}},
	}
	if err := validatePortableImageRequest([]messages.ChatMessage{atLimit}); err != nil {
		t.Fatalf("10,000,000-byte image rejected: %v", err)
	}

	overLimit := atLimit
	overLimit.Parts = slices.Clone(atLimit.Parts)
	overLimit.Parts[0].ImageData = portablePNGBase64Size(t, maxPortableEncodedImageBytes+4)
	err := validatePortableImageRequest([]messages.ChatMessage{overLimit})
	if err == nil || !strings.Contains(err.Error(), "per-image portable limit is 10,000,000 bytes") {
		t.Fatalf("10,000,004-byte image limit error = %v", err)
	}
}

func TestTurnPersistenceAckSerializesDetachedExactRetry(t *testing.T) {
	base := newTurnPersistenceTestSession(t)
	session := &blockingFirstAddSession{
		Session:      base,
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	ack := newTurnPersistenceAck(false)
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "same detached turn"}

	runAttempt := func(done chan<- error) {
		ack.beginPersistence()
		err := persistUserMessageForTurn(session, userMsg, true)
		ack.finishPersistence(err == nil)
		done <- err
	}
	firstDone := make(chan error, 1)
	go runAttempt(firstDone)
	<-session.firstStarted

	secondAcquired := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		ack.beginPersistence()
		close(secondAcquired)
		err := persistUserMessageForTurn(session, userMsg, true)
		ack.finishPersistence(err == nil)
		secondDone <- err
	}()
	select {
	case <-secondAcquired:
		close(session.releaseFirst)
		t.Fatal("exact retry acquired persistence while detached attempt was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(session.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	<-secondAcquired
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if calls := session.addCalls.Load(); calls != 1 {
		t.Fatalf("serialized exact retry called AddMessage %d times, want 1", calls)
	}
	if history := session.GetHistory(); len(history) != 1 || !equivalentUserMessage(history[0], userMsg) {
		t.Fatalf("serialized exact retry history = %#v", history)
	}
}

type blockingFirstAddSession struct {
	sessions.Session
	addCalls     atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (s *blockingFirstAddSession) AddMessage(msg messages.ChatMessage) error {
	if s.addCalls.Add(1) == 1 {
		close(s.firstStarted)
		<-s.releaseFirst
	}
	return s.Session.AddMessage(msg)
}

// portablePNGBase64Size returns a fully decodable 2x2 PNG whose encoded text
// has exactly encodedSize bytes. A private ancillary chunk supplies inert
// padding without changing the pixels or prepared-image dimensions.
func portablePNGBase64Size(t *testing.T, encodedSize int) string {
	t.Helper()
	if encodedSize%4 != 0 {
		t.Fatalf("encoded PNG fixture size %d is not a base64 quantum", encodedSize)
	}
	path := filepath.Join(t.TempDir(), "portable.png")
	writeImageFixture(t, path, 2, 2)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const chunkOverhead = 12
	rawSize := encodedSize / 4 * 3
	payloadSize := rawSize - len(original) - chunkOverhead
	if payloadSize < 0 || len(original) < 12 {
		t.Fatalf("encoded PNG fixture size %d is too small", encodedSize)
	}

	chunk := make([]byte, chunkOverhead+payloadSize)
	binary.BigEndian.PutUint32(chunk[:4], uint32(payloadSize))
	copy(chunk[4:8], "ppLy")
	binary.BigEndian.PutUint32(chunk[8+payloadSize:], crc32.ChecksumIEEE(chunk[4:8+payloadSize]))

	data := make([]byte, 0, rawSize)
	data = append(data, original[:len(original)-12]...)
	data = append(data, chunk...)
	data = append(data, original[len(original)-12:]...)
	encoded := base64.StdEncoding.EncodeToString(data)
	if len(encoded) != encodedSize {
		t.Fatalf("encoded PNG fixture length = %d, want %d", len(encoded), encodedSize)
	}
	return encoded
}

func TestDurableTurnMessagesMarksDeniedCompletionAndFiltersItFromModels(t *testing.T) {
	generated := []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "1", Name: "bash", Arguments: `{}`},
			},
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    llm.ToolDeniedContent,
			ToolCallID: "1",
			ToolName:   "bash",
		},
	}
	durable := durableTurnMessages(generated)
	if len(durable) != 1 || durable[0].Role != messages.MessageRoleInternal {
		t.Fatalf("durable denied turn = %#v, want one internal marker", durable)
	}
	if status, _ := durable[0].Metadata[messages.MetadataKeyTurnStatus].(string); status != messages.TurnStatusToolDenied {
		t.Fatalf("durable denied status = %q", status)
	}

	history := append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "do it"}}, durable...)
	visible := modelVisibleHistory(history)
	if len(visible) != 1 || visible[0].Role != messages.MessageRoleUser {
		t.Fatalf("model-visible history leaked internal marker: %#v", visible)
	}
}

func TestDurableTurnMessagesKeepsProseAndDeniedOutcome(t *testing.T) {
	generated := []messages.ChatMessage{
		{
			Role:    messages.MessageRoleAssistant,
			Content: "I will inspect that.",
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "1", Name: "bash", Arguments: `{}`},
			},
		},
		{Role: messages.MessageRoleTool, Content: llm.ToolDeniedContent, ToolCallID: "1"},
	}
	durable := durableTurnMessages(generated)
	if len(durable) != 2 || durable[0].Content != "I will inspect that." || durable[1].Role != messages.MessageRoleInternal {
		t.Fatalf("prose + denial durable messages = %#v", durable)
	}

	m := newReplModel()
	m.hydrateHistory(append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}}, durable...), "ctx")
	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "I will inspect that.") || !strings.Contains(plain, "tool request denied") || strings.Contains(plain, "incomplete") {
		t.Fatalf("prose + denial hydration = %q", plain)
	}
}

func TestLineTurnUIFinishOwnsOneTerminalNewline(t *testing.T) {
	tests := []struct {
		name    string
		content []string
		want    string
	}{
		{name: "unterminated content", content: []string{"answer"}, want: "answer\n"},
		{name: "terminated content", content: []string{"answer\n"}, want: "answer\n"},
		{name: "newline in separate chunk", content: []string{"answer", "\n"}, want: "answer\n"},
		{name: "no content", want: "\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			ui := newLineTurnUI(&Config{}, nil)
			ui.writer = &out
			for _, content := range tt.content {
				ui.AppendAssistantText(content)
			}
			ui.FinishTextTurn()
			if got := out.String(); got != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLineTurnUIToolBoundaryPreservesSeparatorWithoutExtraFinalNewline(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	for _, before := range []string{"before", "before\n"} {
		t.Run("before "+before, func(t *testing.T) {
			var out bytes.Buffer
			var errOut bytes.Buffer
			ui := newLineTurnUI(&Config{}, nil)
			ui.writer = &out
			ui.errWriter = &errOut
			ui.AppendAssistantText(before)
			ui.AppendToolStart([]messages.ChatMessageToolCall{{Name: "read"}})
			ui.AppendAssistantText("after\n")
			ui.FinishTextTurn()

			if got, want := out.String(), "before\n\nafter\n"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestLineTurnUIWarningAlreadyTerminatesOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	ui := newLineTurnUI(&Config{}, nil)
	ui.writer = &out
	ui.errWriter = &errOut
	ui.AppendAssistantText("answer")
	ui.AppendWarning("truncated")
	ui.FinishTextTurn()

	if got, want := out.String(), "answer\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := errOut.String(), "Warning: truncated\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func newTurnPersistenceTestSession(t *testing.T) sessions.Session {
	t.Helper()
	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get(t.Name())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

func equalTurnTestMessage(a, b messages.ChatMessage) bool {
	return a.Role == b.Role && a.Content == b.Content && slices.Equal(a.Parts, b.Parts)
}
