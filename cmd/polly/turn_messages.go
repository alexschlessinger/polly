package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// answerBlocks returns every answer the run produced, in order: each
// assistant message without tool calls. Swarm settlement can reopen a
// provisional answer with a synthetic user message, so a turn may hold
// several; the coordination messages and tool exchanges between them are not
// answers. A response without a generated transcript falls back to Message.
func answerBlocks(resp *llm.AgentResponse) []string {
	if resp == nil {
		return nil
	}
	var blocks []string
	add := func(m messages.ChatMessage) {
		if m.Role == messages.MessageRoleAssistant && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != "" {
			blocks = append(blocks, strings.Trim(m.Content, "\r\n"))
		}
	}
	for _, m := range resp.AllMessages {
		add(m)
	}
	if len(blocks) == 0 && resp.Message != nil {
		add(*resp.Message)
	}
	return blocks
}

// answerText joins the answer blocks with a blank line, mirroring the
// separator the streaming path prints between text bursts.
func answerText(resp *llm.AgentResponse) string { return strings.Join(answerBlocks(resp), "\n\n") }

// settledAnswer is what a failed one-shot run still prints on stdout: every
// answer the model produced, so a consumer keeps it and reads the failure from
// stderr, or a blocker report naming the session when there is no answer.
func settledAnswer(resp *llm.AgentResponse, runErr error, session string) string {
	if text := answerText(resp); text != "" {
		return text
	}
	return "Blocked: " + runErr.Error() + "\n\nSession: " + session + "\n"
}

// externalizeMessageImages replaces prepared base64 image parts with private
// content-addressed references. Artifact storage is authoritative, so a write
// failure is returned instead of silently persisting a second inline format.
func externalizeMessageImages(ctx context.Context, msg messages.ChatMessage, store artifacts.Store) (messages.ChatMessage, error) {
	msg = msg.Clone()
	for i, part := range msg.Parts {
		if part.Type != "image_base64" || part.ImageData == "" {
			continue
		}
		if store == nil {
			return messages.ChatMessage{}, fmt.Errorf("artifact store is unavailable")
		}
		// A nonportable part (legacy GIF/BMP bytes, mismatched MIME) must be
		// normalized before its bytes become an immutable artifact: once
		// externalized, the base64-only portability validation never sees it
		// again and hydration would replay the bad MIME to providers forever.
		part, err := messages.PortableImagePart(part)
		if err != nil {
			continue
		}
		if part.Type != "image_base64" {
			msg.Parts[i] = part
			continue
		}
		ref, err := storeImagePart(ctx, store, part, part.Reference)
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("store image artifact %d: %w", i+1, err)
		}
		msg.Parts[i] = messages.ContentPart{
			Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken, Artifact: &ref,
		}
	}
	return msg, nil
}

// interruptedTurnMarker records why a partially persisted turn ended. The
// internal role never reaches a provider; hydration uses it to settle the
// turn and label it interrupted instead of leaving it looking abandoned.
func interruptedTurnMarker(cause error) messages.ChatMessage {
	// StopReason is already persisted on ChatMessage. Keep an explicit cap
	// reason on this display-only marker so hydration need not parse errors.
	reason := messages.StopReason("")
	if onlyIterationLimit(cause) {
		reason = messages.StopReasonMaxIterations
	}
	return messages.ChatMessage{
		Role:       messages.MessageRoleInternal,
		StopReason: reason,
		Metadata: map[string]any{
			messages.MetadataKeyTurnStatus: messages.TurnStatusInterrupted,
			messages.MetadataKeyError:      cause.Error(),
		},
	}
}

// durableTurnMessages removes provider-protocol denial exchanges while
// retaining their safe display projection. The internal marker is never sent
// to a model; hydration uses it to restore disclosure order and to keep an
// all-denied turn from looking like an incomplete composer draft.
func durableTurnMessages(generated []messages.ChatMessage) []messages.ChatMessage {
	stripped := llm.StripDeniedExchanges(generated)
	deniedIDs := deniedToolCallIDs(generated)
	allDenied := terminalToolBatchAllDenied(generated)
	displayToolCalls := deniedDisplayToolCalls(generated, deniedIDs)
	if allDenied || displayToolCalls != "" {
		metadata := make(map[string]any)
		if allDenied {
			metadata[messages.MetadataKeyTurnStatus] = messages.TurnStatusToolDenied
		}
		if reasoning, thinking := deniedDisplayReasoning(generated, deniedIDs); reasoning != "" {
			metadata[messages.MetadataKeyDisplayReasoning] = reasoning
			if thinking > 0 {
				metadata[messages.MetadataKeyThinkingMillis] = int(max(thinking.Milliseconds(), 1))
			}
		}
		if displayToolCalls != "" {
			metadata[messages.MetadataKeyDisplayToolCalls] = displayToolCalls
		}
		stripped = append(stripped, messages.ChatMessage{
			Role:     messages.MessageRoleInternal,
			Metadata: metadata,
		})
	}
	return stripped
}

// durableDisplayToolCall is the safe, UI-only subset needed to restore tool
// disclosure order after denied provider-protocol calls have been stripped.
// Arguments and result bodies are intentionally absent.
type durableDisplayToolCall struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Denied bool   `json:"denied,omitempty"`
}

// deniedToolCallIDs collects the IDs of the tool calls in generated whose
// results are user denials.
func deniedToolCallIDs(generated []messages.ChatMessage) map[string]struct{} {
	deniedIDs := make(map[string]struct{})
	for _, msg := range generated {
		if msg.Role == messages.MessageRoleTool && toolWasDenied(msg.Content) {
			deniedIDs[msg.ToolCallID] = struct{}{}
		}
	}
	return deniedIDs
}

func deniedDisplayToolCalls(generated []messages.ChatMessage, deniedIDs map[string]struct{}) string {
	if len(deniedIDs) == 0 {
		return ""
	}
	var calls []durableDisplayToolCall
	for _, msg := range generated {
		if msg.Role != messages.MessageRoleAssistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			_, denied := deniedIDs[call.ID]
			calls = append(calls, durableDisplayToolCall{ID: call.ID, Name: toolDisplayName(call.Name), Denied: denied})
		}
	}
	if len(calls) == 0 {
		return ""
	}
	encoded, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func decodeDisplayToolCalls(value any) []durableDisplayToolCall {
	encoded, ok := value.(string)
	if !ok || encoded == "" {
		return nil
	}
	var calls []durableDisplayToolCall
	if err := json.Unmarshal([]byte(encoded), &calls); err != nil {
		return nil
	}
	return calls
}

// deniedDisplayReasoning keeps the human-visible reasoning that
// StripDeniedExchanges necessarily drops with a reasoning-only assistant tool
// proposal. Storing it on the internal completion marker avoids both an orphan
// provider message and double-counting it as durable model reasoning. The
// summed thinking time of those stripped messages rides along so the resumed
// disclosure keeps its elapsed label.
func deniedDisplayReasoning(generated []messages.ChatMessage, deniedIDs map[string]struct{}) (string, time.Duration) {
	var segments []string
	var thinking time.Duration
	for _, msg := range generated {
		if msg.Role != messages.MessageRoleAssistant || msg.Content != "" || len(msg.ToolCalls) == 0 {
			continue
		}
		allDenied := true
		for _, call := range msg.ToolCalls {
			if _, denied := deniedIDs[call.ID]; !denied {
				allDenied = false
				break
			}
		}
		if allDenied && strings.TrimSpace(msg.Reasoning) != "" {
			segments = append(segments, msg.Reasoning)
			thinking += msg.ThinkingDuration()
		}
	}
	return strings.Join(segments, "\n"), thinking
}

func terminalToolBatchAllDenied(generated []messages.ChatMessage) bool {
	proposal := -1
	for i, msg := range generated {
		if msg.Role == messages.MessageRoleAssistant && len(msg.ToolCalls) > 0 {
			proposal = i
		}
	}
	if proposal < 0 {
		return false
	}
	seen := false
	for _, msg := range generated[proposal+1:] {
		if msg.Role != messages.MessageRoleTool {
			continue
		}
		seen = true
		if !toolWasDenied(msg.Content) {
			return false
		}
	}
	return seen
}

// persistUserMessageForTurn appends the turn's user message unless a matching
// retry already persisted it. Report input consumes its reports in the same
// write, including a restored report draft whose first persist failed.
func persistUserMessageForTurn(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage, reuseUser bool, reportIDs []int64) error {
	if reuseUser {
		equivalent, err := sessionEndsWithEquivalentUserMessage(ctx, session, userMsg)
		if err != nil {
			return err
		}
		if equivalent {
			return nil
		}
	}
	if len(reportIDs) > 0 {
		return session.AddReportMessage(ctx, userMsg, reportIDs)
	}
	return session.AddMessage(ctx, userMsg)
}

func sessionEndsWithEquivalentUserMessage(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage) (bool, error) {
	history, err := session.GetHistory(ctx)
	if err != nil {
		return false, err
	}
	return historyEndsWithEquivalentUserMessage(history, userMsg), nil
}

func historyEndsWithEquivalentUserMessage(history []messages.ChatMessage, userMsg messages.ChatMessage) bool {
	if len(history) == 0 {
		return false
	}
	return equivalentUserMessage(history[len(history)-1], userMsg)
}

func equivalentUserMessage(left, right messages.ChatMessage) bool {
	return left.Role == messages.MessageRoleUser &&
		right.Role == messages.MessageRoleUser &&
		left.Content == right.Content &&
		equalContentParts(left.Parts, right.Parts)
}

func equalContentParts(left, right []messages.ContentPart) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		l, r := left[i], right[i]
		lRef, rRef := l.Artifact, r.Artifact
		l.Artifact, r.Artifact = nil, nil
		if l != r {
			return false
		}
		if (lRef == nil) != (rRef == nil) {
			return false
		}
		if lRef != nil && *lRef != *rRef {
			return false
		}
	}
	return true
}

// prepareSessionImageRequest projects the exact history that AddMessage will
// expose to llm.Agent, given the session's current history. Image hydration
// and context budgeting happen inside the agent; this boundary only avoids
// duplicating an unchanged persisted draft.
func prepareSessionImageRequest(history []messages.ChatMessage, userMsg messages.ChatMessage, reuseUser bool) ([]messages.ChatMessage, error) {
	if err := messages.ValidateImageMessage(userMsg); err != nil {
		return nil, err
	}
	reusingTerminalUser := reuseUser && historyEndsWithEquivalentUserMessage(history, userMsg)
	if !reusingTerminalUser {
		history = append(history[:len(history):len(history)], userMsg)
	}
	// llm.Agent now owns provider-visible image selection and context
	// projection. The canonical transcript remains complete here.
	return messages.NormalizeImages(messages.ModelVisible(history)), nil
}
