package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

var ErrEmptyResult = errors.New("member returned an empty final result")

// EmptyResultError identifies incomplete work without a meaningful final
// answer. Saved findings remain available.
type EmptyResultError struct {
	Session   string
	Execution string
	Reason    string
}

func (e *EmptyResultError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "final response is empty after one retry"
	}
	return fmt.Sprintf("member %s incomplete: %s; published findings and completed work are retained (execution %s)", e.Session, reason, e.Execution)
}

func (e *EmptyResultError) Unwrap() error { return ErrEmptyResult }

const memberFinalNudge = "Your final response was empty. Return a concise, meaningful final answer describing the result or blocker. If you already published the findings or submitted a task result, summarize it and cite its publication or task ID. Do not repeat completed tool work."

func memberFinalInput() []messages.ChatMessage {
	return []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: memberFinalNudge, Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}}}
}

// bindMemberFinal preserves host continuations while bounding this runtime's
// repair to one nudge per logical execution, including yield and restore.
func (r *Runtime) bindMemberFinal(session sessions.CoordinationSession, execution string, generation, remaining int, responseTool string, cb *llm.AgentCallbacks) {
	prior := cb.ContinueAfterFinal
	priorUsage := cb.OnIterationUsage
	priorResult := cb.OnToolResult
	var responseToolSucceeded atomic.Bool
	used := 0
	cb.OnIterationUsage = func(iteration, input, output int) {
		used = iteration + 1
		responseToolSucceeded.Store(false)
		if priorUsage != nil {
			priorUsage(iteration, input, output)
		}
	}
	cb.OnToolResult = func(call messages.ChatMessageToolCall, result messages.ChatMessage) {
		if succeeded, known := result.ToolSucceeded(); known && succeeded && responseTool != "" && call.Name == responseTool {
			responseToolSucceeded.Store(true)
		}
		if priorResult != nil {
			priorResult(call, result)
		}
	}
	cb.ContinueAfterFinal = func(ctx context.Context, response *messages.ChatMessage) ([]messages.ChatMessage, error) {
		if prior != nil {
			input, err := prior(ctx, response)
			if err != nil || len(input) != 0 {
				return input, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if meaningfulMemberFinal(response) || responseToolSucceeded.Load() {
			return nil, nil
		}
		err := session.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
			s, err := decodeState(raw)
			if err != nil {
				return err
			}
			e := s.Executions[execution]
			if e == nil || e.Member != raw.ActorID || e.Generation != generation || e.Status != "running" {
				return errors.New("member final response was fenced")
			}
			if response != nil && len(response.ToolCalls) != 0 {
				// Preserve the agent's terminal denial/response-tool behavior:
				// another turn could repeat an approval the user just denied.
				return &EmptyResultError{Session: e.Member, Execution: e.ID, Reason: "tool calls ended without a successful final response"}
			}
			if e.EmptyFinalRetried {
				return &EmptyResultError{Session: e.Member, Execution: e.ID}
			}
			if used >= remaining {
				// Let the agent's normal continuation limit stamp and pause the
				// final. No retry was issued, so a later grant can still use it.
				return nil
			}
			// Reserve before returning the nudge. A canceled or crashed slice
			// cannot issue another repair after it is restored.
			e.EmptyFinalRetried = true
			return encodeState(raw, s)
		})
		if err != nil {
			return nil, err
		}
		return memberFinalInput(), nil
	}
}

func meaningfulMemberFinal(message *messages.ChatMessage) bool {
	if message == nil {
		return false
	}
	if strings.TrimSpace(memberFinalText(message)) != "" {
		return true
	}
	return len(memberFinalMedia(message)) != 0
}

func memberFinalText(message *messages.ChatMessage) string {
	if strings.TrimSpace(message.Content) != "" {
		return message.Content
	}
	var text []string
	for _, part := range message.Parts {
		switch part.Type {
		case "text", "file":
			if strings.TrimSpace(part.Text) != "" {
				text = append(text, part.Text)
			}
		}
	}
	return strings.Join(text, "\n")
}

func memberFinalMedia(message *messages.ChatMessage) []string {
	var media []string
	for _, part := range message.Parts {
		present := false
		switch part.Type {
		case "image_url":
			present = strings.TrimSpace(part.ImageURL) != ""
		case "image_base64":
			present = strings.TrimSpace(part.ImageData) != ""
		case "image_artifact":
			present = part.Artifact != nil && part.Artifact.ID != ""
		}
		// Generic artifact refs may have been minted from older tool output
		// during request projection. They do not establish a current answer.
		if !present {
			continue
		}
		kind := part.MimeType
		if kind == "" && part.Artifact != nil {
			kind = part.Artifact.MIMEType
		}
		if kind == "" {
			kind = "image"
		}
		if part.Artifact != nil && part.Artifact.ID != "" {
			kind += " (artifact " + part.Artifact.ID + ")"
		}
		media = append(media, kind)
	}
	return media
}

// memberFinalValue keeps coordination text useful without copying binary data
// or treating a historic projection artifact as the current result.
func memberFinalValue(message *messages.ChatMessage, session, responseTool string) string {
	if text := memberFinalText(message); strings.TrimSpace(text) != "" {
		return text
	}
	if media := memberFinalMedia(message); len(media) != 0 {
		return fmt.Sprintf("Final media saved in member session %s: %s.", session, strings.Join(media, "; "))
	}
	for _, call := range message.ToolCalls {
		if responseTool != "" && call.Name == responseTool {
			return fmt.Sprintf("Final result delivered through %s; see member session %s.", responseTool, session)
		}
	}
	return message.Content
}
