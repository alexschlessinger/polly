package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
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
	"github.com/alexschlessinger/pollytool/tools"
)

func TestPersistUserMessageForTurnReusesMatchingFinalUser(t *testing.T) {
	session := newTurnPersistenceTestSession(t)
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "try this"}
	if err := session.AddMessage(context.Background(), userMsg); err != nil {
		t.Fatalf("AddMessage() error = %v", err)
	}

	if err := persistUserMessageForTurn(context.Background(), session, userMsg, true, nil); err != nil {
		t.Fatalf("persistUserMessageForTurn() error = %v", err)
	}
	if got := testSessionHistory(t, session); !slices.EqualFunc(got, []messages.ChatMessage{userMsg}, equalTurnTestMessage) {
		t.Fatalf("matching retry should reuse final user message, history = %#v", got)
	}
}

func TestPersistUserMessageForTurnConsumesReportsOnRetry(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "parent")
	ctx := context.Background()
	if err := store.PostReport(ctx, "parent", sessions.Report{Child: "helper", Status: sessions.ReportFinished, Text: "reply"}); err != nil {
		t.Fatalf("PostReport() error = %v", err)
	}
	reports, err := session.PeekReports(ctx)
	if err != nil || len(reports) != 1 {
		t.Fatalf("PeekReports() = %v, %v", reports, err)
	}
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "agent helper finished"}
	// A restored report draft retries with reuseUser set, but its first
	// persist never happened, so the reports must still be consumed.
	if err := persistUserMessageForTurn(ctx, session, userMsg, true, []int64{reports[0].ID}); err != nil {
		t.Fatalf("persistUserMessageForTurn() error = %v", err)
	}
	if got := testSessionHistory(t, session); !slices.EqualFunc(got, []messages.ChatMessage{userMsg}, equalTurnTestMessage) {
		t.Fatalf("report input was not persisted, history = %#v", got)
	}
	if reports, err := session.PeekReports(ctx); err != nil || len(reports) != 0 {
		t.Fatalf("retry left its reports unconsumed: %v, %v", reports, err)
	}
	if err := persistUserMessageForTurn(ctx, session, userMsg, true, []int64{reports[0].ID}); err != nil {
		t.Fatalf("persistUserMessageForTurn() retry error = %v", err)
	}
	if got := testSessionHistory(t, session); len(got) != 1 {
		t.Fatalf("matching retry should reuse the persisted report input, history = %#v", got)
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
			if err := session.AddMessages(context.Background(), tt.history); err != nil {
				t.Fatalf("AddMessages() error = %v", err)
			}
			if err := persistUserMessageForTurn(context.Background(), session, userMsg, tt.reuse, nil); err != nil {
				t.Fatalf("persistUserMessageForTurn() error = %v", err)
			}
			got := testSessionHistory(t, session)
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
		if err := session.AddMessage(context.Background(), userMsg); err != nil {
			t.Fatalf("AddMessage() error = %v", err)
		}
		if err := persistUserMessageForTurn(context.Background(), session, userMsg, true, nil); err != nil {
			t.Fatalf("persistUserMessageForTurn() error = %v", err)
		}
		if got := len(testSessionHistory(t, session)); got != 1 {
			t.Fatalf("same attached payload should be reused, history length = %d", got)
		}
	})

	t.Run("changed payload is persisted", func(t *testing.T) {
		session := newTurnPersistenceTestSession(t)
		oldMsg := userMsg
		oldMsg.Parts = slices.Clone(userMsg.Parts)
		oldMsg.Parts[1].Text = "older contents"
		if err := session.AddMessage(context.Background(), oldMsg); err != nil {
			t.Fatalf("AddMessage() error = %v", err)
		}
		if err := persistUserMessageForTurn(context.Background(), session, userMsg, true, nil); err != nil {
			t.Fatalf("persistUserMessageForTurn() error = %v", err)
		}
		if got := len(testSessionHistory(t, session)); got != 2 {
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
	if err := session.AddMessage(context.Background(), userMsg); err != nil {
		t.Fatal(err)
	}

	retry, err := prepareSessionImageRequest(context.Background(), session, userMsg, true)
	if err != nil || len(retry) != 1 {
		t.Fatalf("exact retry projection = (%d, %v), want one message", len(retry), err)
	}
	ordinary, err := prepareSessionImageRequest(context.Background(), session, userMsg, false)
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
	store := testOpenMemoryStore(t, &sessions.Metadata{MaxHistoryTokens: 2000})
	session := testAcquireSession(t, store, "trimmed-budget")
	oldImage := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{{
			Type: "image_base64", ImageData: strings.Repeat("A", maxEncodedImageHistoryBytes), MimeType: "image/png",
		}},
	}
	if err := session.AddMessages(context.Background(), []messages.ChatMessage{
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

	prepared, err := prepareSessionImageRequest(context.Background(), session, candidate, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 3 || prepared[0].Parts[0].ImageData != oldImage.Parts[0].ImageData {
		t.Fatalf("durable history was trimmed before agent projection: %#v", prepared)
	}
}

func TestPrepareSessionImageRequestNormalizesLegacyImageWithoutRewriting(t *testing.T) {
	session := newTurnPersistenceTestSession(t)
	if err := session.AddMessages(context.Background(), []messages.ChatMessage{
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

	prepared, err := prepareSessionImageRequest(context.Background(), session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "next"}, false)
	if err != nil {
		t.Fatalf("legacy GIF poisoned the upgraded request: %v", err)
	}
	if len(prepared) != 3 || len(prepared[0].Parts) != 1 || prepared[0].Parts[0].MimeType != "image/png" {
		t.Fatalf("legacy image was not normalized for agent projection: %#v", prepared)
	}
	if got := testSessionHistory(t, session)[0].Parts[0].MimeType; got != "image/gif" {
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
		err := persistUserMessageForTurn(context.Background(), session, userMsg, true, nil)
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
		err := persistUserMessageForTurn(context.Background(), session, userMsg, true, nil)
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
	if history := testSessionHistory(t, session); len(history) != 1 || !equivalentUserMessage(history[0], userMsg) {
		t.Fatalf("serialized exact retry history = %#v", history)
	}
}

type blockingFirstAddSession struct {
	sessions.Session
	addCalls     atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (s *blockingFirstAddSession) AddMessage(ctx context.Context, msg messages.ChatMessage) error {
	if s.addCalls.Add(1) == 1 {
		close(s.firstStarted)
		<-s.releaseFirst
	}
	return s.Session.AddMessage(ctx, msg)
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
	proposal := messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		Reasoning: "I should inspect the requested file before answering.",
		ToolCalls: []messages.ChatMessageToolCall{
			{ID: "1", Name: "bash", Arguments: `{}`},
		},
	}
	proposal.SetThinkingDuration(3 * time.Second)
	generated := []messages.ChatMessage{
		proposal,
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
	if reasoning, _ := durable[0].Metadata[messages.MetadataKeyDisplayReasoning].(string); reasoning != generated[0].Reasoning {
		t.Fatalf("durable display reasoning = %q, want %q", reasoning, generated[0].Reasoning)
	}
	if durable[0].Reasoning != "" {
		t.Fatalf("internal display reasoning must not be model reasoning: %#v", durable[0])
	}
	if got := durable[0].ThinkingDuration(); got != 3*time.Second {
		t.Fatalf("stripped proposal's thinking time on marker = %v, want 3s", got)
	}

	history := append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "do it"}}, durable...)
	visible := modelVisibleHistory(history)
	if len(visible) != 1 || visible[0].Role != messages.MessageRoleUser {
		t.Fatalf("model-visible history leaked internal marker: %#v", visible)
	}

	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	if len(m.reasoningOrder) != 1 {
		t.Fatalf("denied reasoning disclosures = %d, want 1", len(m.reasoningOrder))
	}
	record := m.reasoningRecords[m.reasoningOrder[0]]
	if record == nil || !record.complete || record.unsaved || !strings.Contains(string(record.tail), "inspect the requested file") {
		t.Fatalf("hydrated denied reasoning = %#v", record)
	}
	if record.elapsed != 3*time.Second {
		t.Fatalf("hydrated denied reasoning elapsed = %v, want 3s", record.elapsed)
	}
	if len(m.toolDisclosures) != 1 {
		t.Fatalf("denied tool disclosures = %d, want 1", len(m.toolDisclosures))
	}
	var tools *toolDisclosureRecord
	for _, candidate := range m.toolDisclosures {
		tools = candidate
	}
	if tools == nil || !tools.complete || len(tools.rows) != 1 || record.transcriptIndex >= tools.transcriptIndex {
		t.Fatalf("hydrated denied tool disclosure/order = reasoning %#v tools %#v", record, tools)
	}
	if !m.toggleToolDisclosure(tools.id) || !strings.Contains(plainStyledText(m.transcript[tools.transcriptIndex].text), "✗ denied bash") {
		t.Fatalf("hydrated denied tool disclosure did not expand: %#v", tools)
	}

	dbPath := filepath.Join(t.TempDir(), "denied-reasoning.db")
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "denied-reasoning")
	testAddMessages(t, session, history)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore := testOpenDiskStore(t, dbPath, nil)
	reopened := testAcquireSession(t, reopenedStore, "denied-reasoning")
	reloadedHistory := testSessionHistory(t, reopened)
	if visible := modelVisibleHistory(reloadedHistory); len(visible) != 1 || visible[0].Role != messages.MessageRoleUser {
		t.Fatalf("reloaded model history leaked internal reasoning: %#v", visible)
	}
	reloaded := newReplModel()
	reloaded.hydrateHistory(reloadedHistory, "denied-reasoning")
	if len(reloaded.reasoningOrder) != 1 || !strings.Contains(string(reloaded.reasoningRecords[reloaded.reasoningOrder[0]].tail), "inspect the requested file") {
		t.Fatalf("reloaded denied reasoning disclosure = %#v", reloaded.reasoningRecords)
	}
	var reloadedTools *toolDisclosureRecord
	for _, candidate := range reloaded.toolDisclosures {
		reloadedTools = candidate
	}
	reloadedReasoning := reloaded.reasoningRecords[reloaded.reasoningOrder[0]]
	if reloadedTools == nil || len(reloadedTools.rows) != 1 || reloadedReasoning.transcriptIndex >= reloadedTools.transcriptIndex {
		t.Fatalf("reloaded disclosure order = reasoning %#v tools %#v", reloadedReasoning, reloadedTools)
	}
}

func TestDurableTurnMessagesKeepsProseAndDeniedOutcome(t *testing.T) {
	generated := []messages.ChatMessage{
		{
			Role:      messages.MessageRoleAssistant,
			Content:   "I will inspect that.",
			Reasoning: "This reasoning stays on the surviving assistant message.",
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
	if display, _ := durable[1].Metadata[messages.MetadataKeyDisplayReasoning].(string); display != "" {
		t.Fatalf("surviving assistant reasoning was duplicated on marker: %q", display)
	}

	m := newReplModel()
	m.hydrateHistory(append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}}, durable...), "ctx")
	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "I will inspect that.") || !strings.Contains(plain, "▸ 1 tool") ||
		strings.Contains(plain, "tool request denied") || strings.Contains(plain, "incomplete") {
		t.Fatalf("prose + denial hydration = %q", plain)
	}
	var tools *toolDisclosureRecord
	for _, candidate := range m.toolDisclosures {
		tools = candidate
	}
	if tools == nil || !m.toggleToolDisclosure(tools.id) || !strings.Contains(plainStyledText(m.transcript[tools.transcriptIndex].text), "✗ denied bash") {
		t.Fatalf("prose + denial disclosure = %#v", tools)
	}
}

func TestDurableMixedToolBatchReloadsOneOrderedDisclosure(t *testing.T) {
	const (
		deniedName  = "denied_a"
		successName = "successful_b"
		rawBody     = "RAW SUCCESS RESULT BODY"
	)
	success := messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		Content:    rawBody,
		ToolCallID: "b",
		ToolName:   successName,
	}
	success.SetToolSucceeded(true)
	generated := []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "a", Name: deniedName, Arguments: `{}`},
				{ID: "b", Name: successName, Arguments: `{}`},
			},
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    llm.ToolDeniedContent,
			ToolCallID: "a",
			ToolName:   deniedName,
		},
		success,
		{Role: messages.MessageRoleAssistant, Content: "done"},
	}
	durable := durableTurnMessages(generated)
	history := append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "run mixed batch"}}, durable...)

	dbPath := filepath.Join(t.TempDir(), "mixed-tools.db")
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "mixed-tools")
	testAddMessages(t, session, history)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore := testOpenDiskStore(t, dbPath, nil)
	reopened := testAcquireSession(t, reopenedStore, "mixed-tools")
	m := newReplModel()
	m.hydrateHistory(testSessionHistory(t, reopened), "mixed-tools")
	if len(m.toolDisclosures) != 1 {
		t.Fatalf("reloaded tool disclosures = %d, want 1", len(m.toolDisclosures))
	}
	var record *toolDisclosureRecord
	for _, candidate := range m.toolDisclosures {
		record = candidate
	}
	if record == nil || !record.complete || record.expanded || len(record.rows) != 2 {
		t.Fatalf("reloaded mixed disclosure = %#v, want two completed collapsed rows", record)
	}
	if record.rows[0].label != deniedName || record.rows[1].label != successName {
		t.Fatalf("reloaded mixed row order = %#v, want %s then %s", record.rows, deniedName, successName)
	}

	collapsed := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(collapsed, "▸ 2 tools") {
		t.Fatalf("reloaded collapsed header = %q", collapsed)
	}
	for _, hidden := range []string{deniedName, successName, rawBody, llm.ToolDeniedContent} {
		if strings.Contains(collapsed, hidden) {
			t.Fatalf("collapsed mixed disclosure leaked %q: %q", hidden, collapsed)
		}
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("reloaded mixed disclosure did not expand")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	deniedDetail := "✗ denied " + deniedName
	successDetail := "✓ " + successName
	deniedAt := strings.Index(expanded, deniedDetail)
	successAt := strings.Index(expanded, successDetail)
	if deniedAt < 0 || successAt <= deniedAt {
		t.Fatalf("expanded mixed disclosure lost status or start order: %q", expanded)
	}
	if strings.Contains(expanded, rawBody) || strings.Contains(expanded, llm.ToolDeniedContent) {
		t.Fatalf("expanded mixed disclosure leaked raw result content: %q", expanded)
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
	return testAcquireSession(t, testOpenMemoryStore(t, nil), "test")
}

func equalTurnTestMessage(a, b messages.ChatMessage) bool {
	return a.Role == b.Role && a.Content == b.Content && slices.Equal(a.Parts, b.Parts)
}

// scriptedStreamLLM plays fixed responses, then fails the stream with failErr.
type scriptedStreamLLM struct {
	responses []messages.ChatMessage
	failErr   error
	calls     int
}

func (s *scriptedStreamLLM) ChatCompletionStream(_ context.Context, _ *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	idx := s.calls
	s.calls++
	if idx < len(s.responses) {
		input := make(chan messages.ChatMessage, 1)
		input <- s.responses[idx]
		close(input)
		return processor.ProcessMessagesToEvents(input)
	}
	events := make(chan *messages.StreamEvent, 1)
	events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: s.failErr}
	close(events)
	return events
}

func newInterruptedTurnState(t *testing.T, model llm.LLM, tool tools.Tool) (*conversationState, sessions.Session) {
	t.Helper()
	session := testAcquireSession(t, testOpenMemoryStore(t, nil), "interrupted-turn")
	var toolList []tools.Tool
	if tool != nil {
		toolList = append(toolList, tool)
	}
	registry := tools.NewToolRegistry(toolList)
	artifactStore := session.ArtifactStore()
	return &conversationState{
		session: session, artifactStore: artifactStore, toolRegistry: registry,
		agent:    llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
		settings: Settings{Model: "test/model", MaxTokens: 128},
	}, session
}

func assertInterruptedMarker(t *testing.T, marker messages.ChatMessage, wantErr string) {
	t.Helper()
	if marker.Role != messages.MessageRoleInternal {
		t.Fatalf("turn did not close with an internal marker: %#v", marker)
	}
	if status, _ := marker.Metadata[messages.MetadataKeyTurnStatus].(string); status != messages.TurnStatusInterrupted {
		t.Fatalf("marker status = %q, want %q", status, messages.TurnStatusInterrupted)
	}
	if detail, _ := marker.Metadata[messages.MetadataKeyError].(string); !strings.Contains(detail, wantErr) {
		t.Fatalf("marker error = %q, want it to mention %q", detail, wantErr)
	}
}

func TestMidTurnFailurePersistsCompletedWork(t *testing.T) {
	model := &scriptedStreamLLM{
		responses: []messages.ChatMessage{{
			Role: messages.MessageRoleAssistant, Content: "let me check", StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{ID: "call-1", Name: "probe", Arguments: `{}`}},
		}},
		failErr: errors.New("provider exploded"),
	}
	probe := &tools.Func{Name: "probe", Run: func(context.Context, tools.Args) (string, error) {
		return "tool ran ok", nil
	}}
	state, session := newInterruptedTurnState(t, model, probe)
	config := &Config{}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr

	code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{
		Role: messages.MessageRoleUser, Content: "run the probe",
	}, nil, nil, ui, false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "provider exploded") {
		t.Fatalf("turn = code %d, err %v; want the provider failure", code, err)
	}
	if !turnProgressSaved(err) {
		t.Fatalf("turn error does not report saved progress: %v", err)
	}

	history := testSessionHistory(t, session)
	if len(history) != 4 {
		t.Fatalf("history = %d messages, want user+assistant+tool+marker: %#v", len(history), history)
	}
	if history[1].Role != messages.MessageRoleAssistant || len(history[1].ToolCalls) != 1 {
		t.Fatalf("completed assistant iteration was not persisted: %#v", history[1])
	}
	if history[2].Role != messages.MessageRoleTool || history[2].Content != "tool ran ok" {
		t.Fatalf("completed tool result was not persisted: %#v", history[2])
	}
	assertInterruptedMarker(t, history[3], "provider exploded")
}

func TestCanceledTurnPersistsCompletedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &scriptedStreamLLM{
		responses: []messages.ChatMessage{{
			Role: messages.MessageRoleAssistant, Content: "starting", StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{ID: "call-1", Name: "probe", Arguments: `{}`}},
		}},
		failErr: errors.New("must not be reached"),
	}
	probe := &tools.Func{Name: "probe", Run: func(context.Context, tools.Args) (string, error) {
		cancel() // the user hits cancel while the tool is finishing
		return "did work before cancel", nil
	}}
	state, session := newInterruptedTurnState(t, model, probe)
	config := &Config{}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr

	code, err := executeTurnWithUserMessage(ctx, config, state, messages.ChatMessage{
		Role: messages.MessageRoleUser, Content: "run the probe",
	}, nil, nil, ui, false)
	if code != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("turn = code %d, err %v; want cancellation", code, err)
	}
	if !turnProgressSaved(err) {
		t.Fatalf("canceled turn error does not report saved progress: %v", err)
	}

	history := testSessionHistory(t, session)
	if len(history) != 4 {
		t.Fatalf("history = %d messages, want user+assistant+tool+marker: %#v", len(history), history)
	}
	if history[2].Role != messages.MessageRoleTool || history[2].Content != "did work before cancel" {
		t.Fatalf("tool result completed before cancel was not persisted: %#v", history[2])
	}
	assertInterruptedMarker(t, history[3], context.Canceled.Error())
}

func TestFirstCallFailurePersistsNoGeneratedMessages(t *testing.T) {
	model := &scriptedStreamLLM{failErr: errors.New("immediate failure")}
	state, session := newInterruptedTurnState(t, model, nil)
	config := &Config{}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr

	code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{
		Role: messages.MessageRoleUser, Content: "hello",
	}, nil, nil, ui, false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "immediate failure") {
		t.Fatalf("turn = code %d, err %v; want the provider failure", code, err)
	}
	if turnProgressSaved(err) {
		t.Fatalf("turn with no generated work claims saved progress: %v", err)
	}
	if history := testSessionHistory(t, session); len(history) != 1 || history[0].Role != messages.MessageRoleUser {
		t.Fatalf("history = %#v, want only the persisted user message", history)
	}
}

func TestDetachedTurnRefusesLatePersistence(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.turnID = 4
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: 4}
	if !tui.TurnPersistenceAllowed() {
		t.Fatal("live turn should be allowed to persist")
	}
	r.model.turnID++ // detaching a stuck turn advances the generation
	if tui.TurnPersistenceAllowed() {
		t.Fatal("detached turn must not append after newer turns")
	}
	if !newLineTurnUI(&Config{}, nil).TurnPersistenceAllowed() {
		t.Fatal("the line UI must default to persisting")
	}
}
