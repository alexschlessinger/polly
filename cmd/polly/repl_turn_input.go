package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

// Managed turn inputs: queued prompts, restored drafts, and their image parts.

// managedTurnInput is the immutable boundary between accepting composer input
// and running a model turn. displayText is UI-only; userMessage is the exact
// normalized payload carried through queues and restored composer drafts.
type managedTurnInput struct {
	displayText string
	userMessage messages.ChatMessage
	reportIDs   []int64
	// notice marks input the REPL composed itself (agent reports): the
	// transcript shows it as a muted notice, not as a prompt the user typed.
	notice bool
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
	msg = msg.Clone()
	for i, part := range msg.Parts {
		if part.Artifact == nil || part.Artifact.Kind != artifacts.KindImage {
			continue
		}
		ref := part.Artifact
		data, err := readImageArtifact(ctx, store, ref)
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("read queued image artifact %s: %w", ref.ID, err)
		}
		reference := part.Reference
		if reference == "" {
			reference = ref.ImageToken
		}
		if reference == "" {
			reference = ref.Reference
		}
		msg.Parts[i] = artifactImagePart(ref, data, reference)
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
	turn.userMessage = turn.userMessage.Clone()
	turn.reportIDs = append([]int64(nil), turn.reportIDs...)
	return turn
}

// beginManagedTurn echoes a user prompt and marks a turn in flight. Shared by
// the idle submit path and the queued-prompt drain; neither records history
// here (callers do that when the text is first accepted). Caller must hold
// m.mu.
func (m *replModel) beginManagedTurn(turn managedTurnInput) {
	prompt := turn.displayText
	m.appendTurnSeparator()
	if turn.notice {
		m.appendNoticePrompt(prompt)
	} else {
		m.appendUserPrompt(prompt)
		m.decorateUserPrompt(len(m.transcript)-1, turn)
	}
	m.beginManagedTurnState(turn)
}

// appendNoticePrompt echoes input the REPL composed itself. It counts as a
// prompt for the masthead's invitation, but carries no gutter: nobody typed it.
func (m *replModel) appendNoticePrompt(text string) {
	m.appendNoticeLine(text)
	m.userPromptSeen = true
}

// queuedEcho is the transcript form of queued input: the prompt as it will
// be echoed, then the marker on its own row.
func queuedEcho(item *queuedREPLInput, marker string) (entry, prefix string) {
	if item.turn != nil && item.turn.notice {
		prefix = style.Styled(item.text, "muted", "") + "\n"
	} else {
		prefix = formattedUserPrompt(item.text) + "\n" + userGutter()
	}
	return prefix + style.Styled(marker, "muted", ""), prefix
}

func (m *replModel) decorateUserPrompt(index int, turn managedTurnInput) {
	if images := preparedMessageTranscriptImagesWithStore(turn.userMessage, m.artifactStore); len(images) > 0 {
		// The echoed prompt gains thumbnail slots for its attachments. Pasted
		// private-use runes are stripped first so they cannot pose as slot
		// anchors in an entry that now carries real ones.
		m.setTranscriptEntry(index,
			style.StripImageMarkers(m.transcript[index].text)+"\n"+style.RenderImages(images, userGutter()),
			images)
	}
}

// appendQueuedInput echoes accepted input without settling the assistant block
// that may still be streaming. The transcript, rather than the status bar, is
// the visible acknowledgement that Polly retained the input.
func (m *replModel) appendQueuedInput(item *queuedREPLInput) {
	if len(m.transcript) > 0 && m.transcriptEntryHasContent(len(m.transcript)-1) {
		m.appendTranscriptEntry("")
	}
	entry, prefix := queuedEcho(item, "(queued)")
	item.transcriptIndex = m.appendTranscriptEntry(entry)
	m.noteQueuedInput(item.transcriptIndex, prefix)
	item.transcriptShown = true
	if item.turn != nil && !item.turn.notice {
		m.decorateUserPrompt(item.transcriptIndex, *item.turn)
	}
	m.followBottom = true
}

func (m *replModel) activateQueuedInput(item queuedREPLInput) {
	notice := item.turn != nil && item.turn.notice
	if item.transcriptShown && item.transcriptIndex >= 0 && item.transcriptIndex < len(m.transcript) {
		_, prefix := queuedEcho(&item, "")
		m.fadeQueuedInput(item.transcriptIndex, prefix)
		if notice {
			m.setTranscriptText(item.transcriptIndex, style.Styled(item.text, "muted", ""))
			m.userPromptSeen = true
		} else {
			m.setTranscriptText(item.transcriptIndex, formattedUserPrompt(item.text))
			if item.turn != nil {
				m.decorateUserPrompt(item.transcriptIndex, *item.turn)
			}
		}
		return
	}
	m.appendTurnSeparator()
	if notice {
		m.appendNoticePrompt(item.text)
		return
	}
	m.appendUserPrompt(item.text)
	if item.turn != nil {
		m.decorateUserPrompt(len(m.transcript)-1, *item.turn)
	}
}

func (m *replModel) markQueuedInputNotSent(item queuedREPLInput) {
	delete(m.affordances.queued, item.transcriptIndex)
	if !item.transcriptShown || item.transcriptIndex < 0 || item.transcriptIndex >= len(m.transcript) {
		return
	}
	entry, _ := queuedEcho(&item, "(not sent)")
	m.setTranscriptText(item.transcriptIndex, entry)
	if item.turn != nil && !item.turn.notice {
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
	m.completion = nil
	m.unseenOutcome = turnOutcomeNone
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
		// The restored draft mints its own token below, so the blob is stored
		// without one.
		stored, err := storeImagePart(context.Background(), m.artifactStore, part, "")
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

// adoptRestoredDraft carries the unanswered prompt hydrateHistory restored
// into next over to m when next's display replaces m's. The prompt is saved
// history rather than typing: it fills an empty composer, never overwrites
// input, and an untouched earlier restore follows the history it came from
// instead of lingering as a stale draft. Resending it unchanged then reuses
// the stored user message rather than persisting a duplicate.
func (m *replModel) adoptRestoredDraft(next *replModel) {
	if m.restoredDraft == next.restoredDraft {
		return
	}
	if m.restoredDraft != nil && m.ed.text() == m.restoredDraft.displayText {
		m.ed.clear()
	}
	m.restoredDraft, m.restoredPersistence = next.restoredDraft, next.restoredPersistence
	if m.restoredDraft != nil && m.ed.empty() {
		m.ed.setText(m.restoredDraft.displayText)
	}
}
