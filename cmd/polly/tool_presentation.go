package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// Tool presentation: what every frontend knows about a tool call's result,
// derived once from the durable call and result pair. The transcript row,
// the tool inspector and the line frontend lay this value out with their
// own width and detail policy; none of them reads the raw result again.

type toolOutcome string

const (
	toolOutcomeUnknown  toolOutcome = ""
	toolOutcomeRunning  toolOutcome = "running"
	toolOutcomeOK       toolOutcome = "ok"
	toolOutcomeFailed   toolOutcome = "failed"
	toolOutcomeDenied   toolOutcome = "denied"
	toolOutcomeCanceled toolOutcome = "canceled"
	toolOutcomePaused   toolOutcome = "paused · iteration limit"
)

// toolPresentation is plain, width-independent data. Frontends choose the
// glyph, colour and budget; producers fill it and never style it.
type toolPresentation struct {
	outcome toolOutcome
	// failure is the compact failure descriptor ("exit 1"), from the live
	// error chain or from the exit code a command stored in its result.
	failure string
	// lines counts the stored result text ("3 lines"); the inspector counts
	// its expanded artifact body separately, which can be longer.
	lines    string
	duration time.Duration
	// counts summarizes tracked file changes ("+3 −1") for a successful call.
	counts  string
	changes *fileChanges
	hasText bool
}

// toolPresentationInput is the pair every producer holds. Live callbacks
// add the error and the measured duration; hydration reconstructs both from
// the result's metadata. A zero duration falls back to the stored one.
type toolPresentationInput struct {
	call     messages.ChatMessageToolCall
	result   messages.ChatMessage
	err      error
	duration time.Duration
	complete bool
}

// liveToolResult is the result message a live callback holds before the
// durable one arrives: the text and the outcome the agent loop will record.
func liveToolResult(call messages.ChatMessageToolCall, result string, err error) messages.ChatMessage {
	msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: result}
	msg.SetToolSucceeded(err == nil)
	return msg
}

func newToolPresentation(in toolPresentationInput) toolPresentation {
	content := in.result.Content
	p := toolPresentation{duration: in.duration, changes: fileChangesFromResult(in.result)}
	if p.duration == 0 {
		p.duration = in.result.ToolDuration()
	}
	p.lines = resultLineMeta(content)
	p.hasText = strings.TrimSpace(content) != ""
	switch {
	case !in.complete:
		p.outcome = toolOutcomeRunning
	case toolWasDenied(content):
		p.outcome = toolOutcomeDenied
	case in.err != nil:
		p.outcome = toolOutcome(toolActivityOutcome(false, in.err))
		p.failure = toolFailureMeta(in.err)
	default:
		// A result that recorded no outcome (history from before outcomes
		// were stored) stays neutral rather than claiming success.
		succeeded, known := in.result.ToolSucceeded()
		switch {
		case in.result.IsError() || (known && !succeeded):
			p.outcome = toolOutcomeFailed
			if code := exitCodeFromResult(in.result); code > 0 {
				p.failure = fmt.Sprintf("exit %d", code)
			}
		case known:
			p.outcome = toolOutcomeOK
		}
	}
	if p.changes != nil && !p.changes.tracked {
		p.changes = nil
	}
	if p.changes != nil {
		p.counts = p.changes.countText()
	}
	return p
}

func (p toolPresentation) glyph() string {
	switch p.outcome {
	case toolOutcomeOK:
		return "✓"
	case toolOutcomeRunning:
		return "→"
	case toolOutcomeUnknown:
		return "·"
	}
	return "✗"
}

func (p toolPresentation) tone() string {
	switch p.outcome {
	case toolOutcomeOK:
		return "ok"
	case toolOutcomeRunning:
		return "run"
	case toolOutcomeUnknown:
		return "muted"
	}
	return "err"
}

// meta is the settled row's annotation: the output size for success, the
// failure descriptor otherwise.
func (p toolPresentation) meta() string {
	switch p.outcome {
	case toolOutcomeOK:
		return p.lines
	case toolOutcomeFailed:
		if p.failure != "" {
			return p.failure
		}
		return string(p.outcome)
	case toolOutcomeRunning, toolOutcomeUnknown:
		return ""
	}
	return string(p.outcome)
}

// detail is the line frontend's fuller annotation for a settled call that
// did not succeed: "failed · exit 1 · 3 lines".
func (p toolPresentation) detail() string {
	switch p.outcome {
	case toolOutcomeOK:
		return p.lines
	case toolOutcomeRunning, toolOutcomeUnknown:
		return ""
	}
	parts := []string{string(p.outcome)}
	if p.failure != "" {
		parts = append(parts, p.failure)
	}
	if p.lines != "" {
		parts = append(parts, p.lines)
	}
	return strings.Join(parts, " · ")
}

// elapsed is the row's duration. A denial ran nothing, so it shows none.
func (p toolPresentation) elapsed() string {
	if p.outcome == toolOutcomeDenied || p.duration <= 0 {
		return ""
	}
	return formatElapsed(p.duration)
}

// inline is the transcript row's width-budget carrier for this result.
func (p toolPresentation) inline() inlineToolLine {
	return inlineToolLine{glyph: p.glyph(), tone: p.tone(), modifier: "bold", meta: p.meta(), duration: p.elapsed(), counts: p.counts}
}
