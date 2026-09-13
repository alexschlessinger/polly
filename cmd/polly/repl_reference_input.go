package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
)

func (r *managedREPL) composerCatalog() *skills.Catalog {
	if r.state == nil {
		return nil
	}
	return r.state.skillCatalog
}

// prepareReferenceTurn receives copies only; all filesystem work is off the UI
// thread. The caller decides whether its draft revision is still current.
func prepareReferenceTurn(ctx context.Context, state *conversationState, root, prompt string, attachments []composerAttachment, snapshots map[string]messages.ContentPart) (managedTurnInput, error) {
	var msg messages.ChatMessage
	var err error
	if state != nil && state.effectiveTools() != nil && len(attachments) > 0 {
		msg = messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: prompt}}}
		for _, att := range attachments {
			if att.Artifact != nil {
				one, e := buildREPLUserMessage("", []composerAttachment{att})
				if e != nil {
					return managedTurnInput{}, e
				}
				msg.Parts = append(msg.Parts, one.Parts...)
				continue
			}
			_, data, e := state.effectiveTools().ReadContextFile(ctx, att.Path, maxContextImageBytes)
			if e != nil {
				return managedTurnInput{}, e
			}
			part, e := prepareImageBytesForUpload(data, att.Label)
			if e != nil {
				return managedTurnInput{}, e
			}
			part.Reference = att.Reference
			msg.Parts = append(msg.Parts, *part)
		}
	} else {
		msg, err = buildREPLUserMessage(prompt, attachments)
	}
	if err != nil {
		return managedTurnInput{}, err
	}
	var catalog *skills.Catalog
	if state != nil {
		catalog = state.skillCatalog
	}
	if state == nil {
		if len(scanComposerReferences(prompt)) > 0 {
			return managedTurnInput{}, fmt.Errorf("references require a session")
		}
	} else {
		msg, err = prepareComposerReferences(ctx, prompt, root, state.effectiveTools(), catalog, msg, snapshots)
		if err != nil {
			return managedTurnInput{}, err
		}
		msg, err = externalizeMessageImages(ctx, msg, state.artifactStore)
		if err != nil {
			return managedTurnInput{}, err
		}
	}
	if err := messages.ValidateImageMessage(msg); err != nil {
		return managedTurnInput{}, err
	}
	return cloneManagedTurn(managedTurnInput{displayText: prompt, userMessage: msg}), nil
}

func (m *replModel) referenceSnapshotCopy() map[string]messages.ContentPart {
	out := map[string]messages.ContentPart{}
	for token, part := range m.referenceSnapshots {
		out[token] = part
	}
	return out
}
func (m *replModel) rememberReferenceSnapshots(msg messages.ChatMessage) {
	md, ok := readComposerMetadata(msg)
	if !ok {
		return
	}
	m.referenceSnapshots = map[string]messages.ContentPart{}
	for _, part := range msg.Clone().Parts {
		if strings.HasPrefix(part.Reference, "@") {
			m.referenceSnapshots[part.Reference] = part
		}
	}
	for _, binding := range md.Files {
		if binding.Part < 0 || binding.Part >= len(msg.Parts) || !strings.HasPrefix(binding.Reference, "@") {
			continue
		}
		part := msg.Clone().Parts[binding.Part]
		part.Reference = binding.Reference
		m.referenceSnapshots[binding.Reference] = part
	}
}
func (m *replModel) pruneReferenceSnapshots() {
	present := map[string]bool{}
	rs := []rune(m.ed.text())
	for _, ref := range scanComposerReferences(m.ed.text()) {
		if ref.kind == "@" {
			present[string(rs[ref.start:ref.end])] = true
		}
	}
	for token := range m.referenceSnapshots {
		if !present[token] {
			delete(m.referenceSnapshots, token)
			m.clearRestoredDraft()
		}
	}
}

func (r *managedREPL) prepareComposerAsyncLocked(prompt string) {
	m, state := r.model, r.state
	if m.referencePreparing {
		return
	}
	attachments, err := m.promptAttachments(prompt)
	if err != nil {
		m.appendErrorLine(err.Error())
		return
	}
	snapshots := m.referenceSnapshotCopy()
	revision := m.ed.revision
	root := m.imageBaseDir
	m.referencePreparing = true
	ctx := state.sessionContext()
	r.background(func() {
		turn, err := prepareReferenceTurn(ctx, state, root, prompt, attachments, snapshots)
		r.postUI(r.work.ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.referencePreparing = false
			if m.ed.revision != revision || r.quitting || ctx.Err() != nil {
				return
			}
			if err != nil {
				m.appendErrorLine(err.Error())
				return
			}
			m.rememberArtifactAttachments(turn.userMessage)
			// Post-acceptance edits start a fresh attachment selection. Restored
			// drafts explicitly rebuild their snapshot bindings.
			if m.busy {
				m.referenceSnapshots = nil
				m.ed.clear()
				m.hist.record(prompt)
				r.appendHistory(prompt)
				m.queue = append(m.queue, queuedREPLInput{text: prompt, turn: &turn})
				m.appendQueuedInput(&m.queue[len(m.queue)-1])
			} else {
				select {
				case r.pending <- pendingTurn{model: m, turn: turn}:
					m.referenceSnapshots = nil
					m.ed.clear()
					m.hist.record(prompt)
					r.appendHistory(prompt)
					m.currentPersistence = nil
					m.clearRestoredDraft()
					m.beginManagedTurn(turn)
				default:
					m.appendErrorLine("turn queue is unavailable")
				}
			}
		})
	})
}

// Path-only bracketed pastes are recognized before conversion. A mixed batch
// is atomic: unsupported members leave the original paste available to edit.
func (r *managedREPL) flushReferencePasteLocked() {
	m, state := r.model, r.state
	text := string(m.pasteBuf)
	m.pasteBuf = nil
	if text == "" || m.approval != nil || m.hist.searching {
		return
	}
	// Keep image-only behavior for unbound test/fallback models.
	if state == nil {
		m.pasteBuf = []rune(text)
		m.flushPasteBuffer()
		return
	}
	m.insertEditorText(text)
	if state.session == nil || state.effectiveTools() == nil {
		return
	}
	paths := splitDroppedPaths(text)
	if len(paths) == 0 {
		return
	}
	revision, end := m.ed.revision, m.ed.cursor
	root := m.imageBaseDir
	ctx := state.sessionContext()
	m.referencePasting = true
	m.referencePasteSeq++
	pasteSeq := m.referencePasteSeq
	r.background(func() {
		replacement, recognized, err := preparePathPaste(ctx, state, root, paths)
		r.postUI(r.work.ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.referencePasteSeq != pasteSeq {
				return
			}
			m.referencePasting = false
			if m.ed.revision != revision || ctx.Err() != nil || r.quitting || !recognized {
				return
			}
			if err != nil {
				m.appendErrorLine(err.Error())
				return
			}
			m.ed.replace(end-len([]rune(text)), end, replacement)
			for _, ref := range scanComposerReferences(replacement) {
				delete(m.referenceSnapshots, string([]rune(replacement)[ref.start:ref.end]))
			}
			m.referencesPopup = nil
		})
	})
}
func preparePathPaste(ctx context.Context, state *conversationState, root string, paths []string) (string, bool, error) {
	for _, path := range paths {
		if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
			path = filepath.Join(root, path)
		}
		abs, err := state.effectiveTools().ResolvePath(path)
		if err != nil {
			return "", false, nil
		}
		if _, err := os.Stat(abs); err != nil {
			return "", false, nil
		}
	}
	if len(paths) > maxContextFiles {
		return "", true, fmt.Errorf("maximum is %d file attachments", maxContextFiles)
	}
	var tokens []string
	for _, path := range paths {
		tokens = append(tokens, fileReference(path))
	}
	replacement := strings.Join(tokens, " ") + " "
	_, err := prepareReferenceTurn(ctx, state, root, replacement, nil, nil)
	return replacement, true, err
}

// Attachment summaries keep the submitted payload inspectable without
// expanding large file bodies into normal conversation scrollback.
func (m *replModel) decorateReferencePrompt(index int, msg messages.ChatMessage) {
	if _, ok := readComposerMetadata(msg); !ok {
		return
	}
	var parts []messages.ContentPart
	var labels []string
	for _, part := range msg.Clone().Parts {
		if part.Reference != "" && part.Type == "text" {
			parts = append(parts, part)
			labels = append(labels, filepath.Base(part.FileName)+" · text")
		}
	}
	if len(parts) == 0 {
		return
	}
	m.transcript[index].contextFiles = parts
	m.setTranscriptText(index, m.transcript[index].text+"\n  "+style.Styled(style.Escape("Files: "+strings.Join(labels, ", ")+" · click to inspect"), "muted", ""))
}
func (m *replModel) placeReferenceFiles(v transcriptViewport) {
	m.referenceFilePlacements = nil
	row := 0
	for _, block := range m.visual.blocks {
		var index int
		if _, err := fmt.Sscanf(block.key, "transcript:%d", &index); err == nil && index >= 0 && index < len(m.transcript) && len(m.transcript[index].contextFiles) > 0 {
			for j := range block.rows {
				if v.contains(row + j) {
					m.referenceFilePlacements = append(m.referenceFilePlacements, referenceFilePlacement{index: index, y: v.screenY(row + j), width: v.width})
				}
			}
		}
		row += len(block.rows)
	}
}

type referenceFilePlacement struct{ index, y, width int }

func (r *managedREPL) openReferenceFilesAt(x, y int) bool {
	// Mixed attachments retain the existing image-viewer click target.
	for _, image := range r.model.imagePlacements {
		if x >= image.X && x < image.X+image.Cols && y >= image.Y && y < image.Y+image.Rows {
			return false
		}
	}
	for _, p := range r.model.referenceFilePlacements {
		if p.y == y && x >= 0 && x < p.width {
			var details []string
			for _, part := range r.model.transcript[p.index].contextFiles {
				details = append(details, strings.Split(part.Text, "\n")...)
			}
			r.openModal(&replModal{title: "Attached files", width: 90, details: details})
			return true
		}
	}
	return false
}
