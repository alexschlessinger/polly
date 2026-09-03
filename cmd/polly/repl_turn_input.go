package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// Managed turn inputs: queued prompts, restored drafts, and their image parts.

// managedTurnInput is the immutable boundary between accepting composer input
// and running a model turn. displayText is UI-only; userMessage is the exact
// normalized payload carried through queues and restored composer drafts.
type managedTurnInput struct {
	displayText string
	userMessage messages.ChatMessage
}

// turnPersistenceAck belongs to one logical user turn. Provider goroutines can
// mark it without taking the model lock, so queue projection never observes a
// persisted session message while its acknowledgement is blocked behind the
// event loop. Keeping the pointer turn-owned makes late callbacks harmless: an
// old callback can only update the old turn (or an unchanged restored-draft
// resubmission), never a newer prompt.
type turnPersistenceAck struct {
	mu        sync.Mutex
	active    int
	persisted bool
	settled   chan struct{}
}

func newTurnPersistenceAck(persisted bool) *turnPersistenceAck {
	return &turnPersistenceAck{persisted: persisted}
}

func (a *turnPersistenceAck) beginPersistence() {
	if a == nil {
		return
	}
	for {
		a.mu.Lock()
		if a.active > 0 {
			settled := a.settled
			a.mu.Unlock()
			<-settled
			continue
		}
		a.settled = make(chan struct{})
		a.active = 1
		a.mu.Unlock()
		return
	}
}

func (a *turnPersistenceAck) finishPersistence(persisted bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if persisted {
		a.persisted = true
	}
	if a.active > 0 {
		a.active--
		if a.active == 0 {
			close(a.settled)
		}
	}
	a.mu.Unlock()
}

type queuedREPLInput struct {
	text            string
	turn            *managedTurnInput
	transcriptIndex int
	transcriptShown bool
}

// materializeQueuedImagesForReset snapshots prepared queued images before the
// session namespace is cleared. The returned queue is self-contained: callers
// may safely remove every artifact and then externalize these exact bytes into
// the fresh namespace. Caller must hold m.mu.
func (m *replModel) materializeQueuedImagesForReset(ctx context.Context) ([]queuedREPLInput, error) {
	queue := make([]queuedREPLInput, len(m.queue))
	copy(queue, m.queue)
	for i := range queue {
		if queue[i].turn == nil {
			continue
		}
		turn := cloneManagedTurn(*queue[i].turn)
		message, err := materializeArtifactImageParts(ctx, turn.userMessage, m.artifactStore)
		if err != nil {
			return nil, err
		}
		turn.userMessage = message
		queue[i].turn = &turn
	}
	return queue, nil
}

func materializeArtifactImageParts(ctx context.Context, msg messages.ChatMessage, store artifacts.Store) (messages.ChatMessage, error) {
	msg = cloneChatMessage(msg)
	for i, part := range msg.Parts {
		if part.Artifact == nil || part.Artifact.Kind != artifacts.KindImage {
			continue
		}
		ref := part.Artifact
		if store == nil {
			return messages.ChatMessage{}, fmt.Errorf("image artifact %s has no session store", ref.ID)
		}
		if !artifacts.ValidID(ref.ID) || ref.Bytes < 0 || ref.Bytes > int64(maxLocalImageBytes) {
			return messages.ChatMessage{}, fmt.Errorf("image artifact %s has invalid metadata", ref.ID)
		}
		r, err := store.Open(ctx, ref.ID)
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("read queued image artifact %s: %w", ref.ID, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(r, ref.Bytes+1))
		closeErr := r.Close()
		if readErr != nil {
			return messages.ChatMessage{}, fmt.Errorf("read queued image artifact %s: %w", ref.ID, readErr)
		}
		if closeErr != nil {
			return messages.ChatMessage{}, fmt.Errorf("close queued image artifact %s: %w", ref.ID, closeErr)
		}
		if int64(len(data)) != ref.Bytes {
			return messages.ChatMessage{}, fmt.Errorf("queued image artifact %s size changed", ref.ID)
		}
		reference := part.Reference
		if reference == "" {
			reference = ref.ImageToken
		}
		if reference == "" {
			reference = ref.Reference
		}
		msg.Parts[i] = messages.ContentPart{
			Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data),
			MimeType: ref.MIMEType, FileName: ref.Name, Reference: reference,
		}
	}
	return msg, nil
}

// restoreQueuedImagesAfterReset repopulates only artifacts referenced by
// future queued turns and rebuilds their stable-token registry. On failure the
// remaining prepared bytes stay inline until those turns are marked not sent.
// Caller must hold m.mu.
func (m *replModel) restoreQueuedImagesAfterReset(ctx context.Context, queue []queuedREPLInput) error {
	m.queue = queue
	m.attachments = make(map[int]composerAttachment)
	m.ambiguousAttachments = make(map[int]bool)
	// attachmentSeq stays monotonic across the reset: input-recall history and
	// unsubmitted drafts survive it carrying old tokens, and reusing a number
	// would silently rebind such a token to a different file. A cleared token
	// now fails as unknown instead.
	for i := range m.queue {
		if m.queue[i].turn == nil {
			continue
		}
		turn := cloneManagedTurn(*m.queue[i].turn)
		externalized, err := externalizeMessageImages(ctx, turn.userMessage, m.artifactStore)
		if err != nil {
			m.queue[i].turn = &turn
			return err
		}
		turn.userMessage = externalized
		m.rememberArtifactAttachments(turn.userMessage)
		m.queue[i].turn = &turn
	}
	return nil
}

func cloneManagedTurn(turn managedTurnInput) managedTurnInput {
	turn.userMessage = cloneChatMessage(turn.userMessage)
	return turn
}

func cloneChatMessage(msg messages.ChatMessage) messages.ChatMessage {
	msg.Parts = append([]messages.ContentPart(nil), msg.Parts...)
	for i := range msg.Parts {
		if msg.Parts[i].Artifact != nil {
			ref := *msg.Parts[i].Artifact
			msg.Parts[i].Artifact = &ref
		}
	}
	msg.ToolCalls = append([]messages.ChatMessageToolCall(nil), msg.ToolCalls...)
	if msg.Metadata != nil {
		metadata := make(map[string]any, len(msg.Metadata))
		for key, value := range msg.Metadata {
			metadata[key] = value
		}
		msg.Metadata = metadata
	}
	return msg
}

// beginManagedTurn echoes a user prompt and marks a turn in flight. Shared by
// the idle submit path and the queued-prompt drain; neither records history
// here (callers do that when the text is first accepted). Caller must hold
// m.mu.
func (m *replModel) beginManagedTurn(turn managedTurnInput) {
	prompt := turn.displayText
	m.appendTurnSeparator()
	m.appendUserPrompt(prompt)
	m.decorateUserPrompt(len(m.transcript)-1, turn)
	m.beginManagedTurnState(turn)
}

func (m *replModel) decorateUserPrompt(index int, turn managedTurnInput) {
	if images := preparedMessageTranscriptImagesWithStore(turn.userMessage, m.artifactStore); len(images) > 0 {
		// The echoed prompt gains thumbnail slots for its attachments. Pasted
		// private-use runes are stripped first so they cannot pose as slot
		// anchors in an entry that now carries real ones.
		m.setTranscriptEntry(index,
			stripTranscriptImageMarkers(m.transcript[index].text)+"\n"+renderTranscriptImages(images, "  "),
			images)
	}
}

// appendQueuedInput echoes accepted input without settling the assistant block
// that may still be streaming. The transcript, rather than the status bar, is
// the visible acknowledgement that Polly retained the input.
func (m *replModel) appendQueuedInput(item *queuedREPLInput) {
	if len(m.transcript) > 0 && m.transcript[len(m.transcript)-1].text != "" {
		m.appendTranscriptEntry("")
	}
	entry := formattedUserPrompt(item.text) + "\n  " + styled("(queued)", "muted", "")
	item.transcriptIndex = m.appendTranscriptEntry(entry)
	item.transcriptShown = true
	if item.turn != nil {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
	}
	m.followBottom = true
}

func (m *replModel) activateQueuedInput(item queuedREPLInput) {
	if item.transcriptShown && item.transcriptIndex >= 0 && item.transcriptIndex < len(m.transcript) {
		m.setTranscriptText(item.transcriptIndex, formattedUserPrompt(item.text))
		if item.turn != nil {
			m.decorateUserPrompt(item.transcriptIndex, *item.turn)
		}
		return
	}
	m.appendTurnSeparator()
	m.appendUserPrompt(item.text)
	if item.turn != nil {
		m.decorateUserPrompt(len(m.transcript)-1, *item.turn)
	}
}

func (m *replModel) markQueuedInputNotSent(item queuedREPLInput) {
	if !item.transcriptShown || item.transcriptIndex < 0 || item.transcriptIndex >= len(m.transcript) {
		return
	}
	m.setTranscriptText(item.transcriptIndex, formattedUserPrompt(item.text)+"\n  "+styled("(not sent)", "muted", ""))
	if item.turn != nil {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
	}
}

func (m *replModel) discardQueuedInputs() int {
	count := len(m.queue)
	for _, item := range m.queue {
		m.markQueuedInputNotSent(item)
	}
	m.queue = nil
	return count
}

func (m *replModel) beginManagedTurnState(turn managedTurnInput) {
	prompt := turn.displayText
	m.startTurnDock()
	m.busy = true
	m.canceling = false
	m.state = turnStateWaiting
	m.runningTools = 0
	m.activeToolsPhase = -1
	m.resetToolDisclosure()
	m.turnToolDisclosureIDs = nil
	m.resetCurrentThinking()
	m.turnStarted = time.Now()
	m.currentPrompt = prompt
	m.currentTurn = cloneManagedTurn(turn)
	if m.currentPersistence == nil {
		m.currentPersistence = newTurnPersistenceAck(false)
	}
	m.turnHasOutput = false
	m.outcomeLabeled = false
	m.lastOutcome = turnOutcomeNone
	// Token counts are per-turn and appear in the dock once reported.
	m.lastIn = 0
	m.lastOut = 0
	m.followBottom = true
}

func editableTurnPrompt(turn managedTurnInput) string {
	if turn.userMessage.Content != "" {
		return turn.userMessage.Content
	}
	var prompt strings.Builder
	for _, part := range turn.userMessage.Parts {
		if part.Type == "text" && part.FileName == "" {
			prompt.WriteString(part.Text)
		}
	}
	if prompt.Len() > 0 {
		return prompt.String()
	}
	return turn.displayText
}

// restoreTurnDraft puts failed/canceled input back in the composer without
// overwriting anything the user typed while the turn was running. The original
// remains available through input history in that case.
func (m *replModel) restoreTurnDraft(turn managedTurnInput, persistence *turnPersistenceAck) bool {
	turn = cloneManagedTurn(turn)
	originalDisplay := turn.displayText
	turn.displayText = editableTurnPrompt(turn)
	m.rememberArtifactAttachments(turn.userMessage)
	for _, part := range turn.userMessage.Parts {
		if part.Type != "image_base64" && part.Type != "image_artifact" {
			continue
		}
		token := strings.TrimSpace(part.Reference)
		match := attachmentTokenPattern.FindStringSubmatch(token)
		validToken := len(match) == 2 && match[0] == token
		if validToken {
			attachments, err := m.promptAttachments(token)
			validToken = err == nil && len(attachments) == 1
		}
		if !validToken {
			token = m.bindRestoredImageAttachment(part)
		}
		if token != "" && !strings.Contains(turn.displayText, token) {
			if replaced, ok := replaceRestoredImagePath(turn.displayText, part.FileName, token); ok {
				turn.displayText = replaced
			} else {
				turn.displayText = strings.TrimSpace(turn.displayText + " " + token)
			}
		}
	}
	if turn.displayText != originalDisplay {
		m.hist.rewrite(originalDisplay, turn.displayText)
	}
	m.restoredDraft = &turn
	m.restoredPersistence = persistence
	if !m.ed.empty() {
		return false
	}
	m.ed.setText(turn.displayText)
	return true
}

func replaceRestoredImagePath(prompt, fileName, token string) (string, bool) {
	if fileName == "" {
		return prompt, false
	}
	for _, word := range splitPromptWords(prompt) {
		path := trimPromptPathPunctuation(word.text)
		if filepath.Base(path) != fileName {
			continue
		}
		replacement := strings.Replace(word.text, path, token, 1)
		return prompt[:word.pos] + replacement + prompt[word.pos+len(word.text):], true
	}
	return prompt, false
}

func (m *replModel) bindRestoredImageAttachment(part messages.ContentPart) string {
	var ref *artifacts.Ref
	if part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage {
		copy := *part.Artifact
		ref = &copy
	} else if part.Type == "image_base64" && part.ImageData != "" && m.artifactStore != nil {
		data, err := base64.StdEncoding.DecodeString(part.ImageData)
		if err != nil || len(data) == 0 {
			return ""
		}
		stored, err := m.artifactStore.Put(context.Background(), artifacts.Blob{
			Kind: artifacts.KindImage, MIMEType: part.MimeType, Name: part.FileName, Data: data,
		})
		if err != nil {
			return ""
		}
		ref = &stored
	}
	if ref == nil {
		return ""
	}
	m.attachmentSeq++
	token := attachmentToken(m.attachmentSeq)
	ref.ImageToken = token
	m.attachments[m.attachmentSeq] = composerAttachment{Label: ref.Name, Reference: token, Artifact: ref}
	return token
}

func (m *replModel) acceptedRestoredTurn(prompt string) (managedTurnInput, *turnPersistenceAck, bool) {
	if m.restoredDraft == nil || prompt != m.restoredDraft.displayText {
		return managedTurnInput{}, nil, false
	}
	return cloneManagedTurn(*m.restoredDraft), m.restoredPersistence, true
}

func (m *replModel) clearRestoredDraft() {
	m.restoredDraft = nil
	m.restoredPersistence = nil
}
