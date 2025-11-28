package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/muesli/termenv"
)

// InteractiveEventProcessor handles event processing for interactive mode
type InteractiveEventProcessor struct {
	session            sessions.Session
	registry           *tools.ToolRegistry
	statusLine         *InteractiveStatus
	response           messages.ChatMessage
	responseText       strings.Builder
	reasoningLength    int
	firstByteReceived  bool
	messageCommitted   bool
	ctx                context.Context
}

// NewInteractiveEventProcessor creates a new interactive event processor
func NewInteractiveEventProcessor(
	ctx context.Context,
	session sessions.Session,
	registry *tools.ToolRegistry,
	statusLine *InteractiveStatus,
) *InteractiveEventProcessor {
	return &InteractiveEventProcessor{
		ctx:        ctx,
		session:    session,
		registry:   registry,
		statusLine: statusLine,
	}
}

// OnReasoning handles reasoning content
func (p *InteractiveEventProcessor) OnReasoning(content string, totalLength int) {
	p.reasoningLength = totalLength
	if p.statusLine != nil {
		p.statusLine.UpdateThinkingProgress(totalLength)
	}
}

// OnContent handles regular content streaming
func (p *InteractiveEventProcessor) OnContent(content string, firstChunk bool) {
	// Clear status on first content
	if firstChunk && !p.firstByteReceived && p.statusLine != nil {
		p.firstByteReceived = true
		p.statusLine.ClearForContent()
	}

	// Accumulate content for the session
	p.responseText.WriteString(content)

	// Print content as it arrives in interactive mode
	if content != "" {
		fmt.Print(content)
	}
}

// OnToolCall handles tool call events (not used in current implementation)
func (p *InteractiveEventProcessor) OnToolCall(toolCall messages.ChatMessageToolCall) {
	// Tool calls are handled in OnComplete in the current implementation
}

// OnComplete handles the complete message
func (p *InteractiveEventProcessor) OnComplete(message *messages.ChatMessage) {
	p.response = *message

	// Use streamed content if available, otherwise use message content
	if p.responseText.Len() > 0 {
		p.response.Content = strings.TrimLeft(p.responseText.String(), "")
	}

	// Add assistant response to session
	p.session.AddMessage(p.response)
	p.messageCommitted = true

	// Process tool calls if any
	if len(p.response.ToolCalls) > 0 {
		if p.statusLine != nil {
			p.statusLine.Clear()
		}

		// If we have content, ensure proper formatting before tool execution
		if p.response.Content != "" && p.responseText.Len() == 0 {
			// Content wasn't streamed (responseText is empty), print it now
			fmt.Print(p.response.Content)
		} else if p.responseText.Len() > 0 {
			// Content was streamed, add a newline before tool output
			fmt.Println()
		}

		// Interactive mode shows tool calls with spinner and completion status
		termOutput := termenv.NewOutput(os.Stdout)
		var successStyle, errorStyle termenv.Style

		// Adapt colors based on terminal background
		if termenv.HasDarkBackground() {
			successStyle = termOutput.String().Foreground(termOutput.Color("65")) // Muted green for dark
			errorStyle = termOutput.String().Foreground(termOutput.Color("124"))  // Muted red for dark
		} else {
			successStyle = termOutput.String().Foreground(termOutput.Color("28")) // Dark green for light
			errorStyle = termOutput.String().Foreground(termOutput.Color("160"))  // Dark red for light
		}

		// Create executor with timeout from session metadata
		executor := llm.NewToolExecutor(p.registry).
			WithTimeout(p.session.GetMetadata().ToolTimeout)

		for _, toolCall := range p.response.ToolCalls {
			// Show spinner for this tool
			if p.statusLine != nil {
				p.statusLine.ShowToolCall(toolCall.Name)
			}

			start := time.Now()
			success := executor.ExecuteToolCall(p.ctx, toolCall, p.session)
			dur := time.Since(start).Truncate(time.Millisecond)

			// Clear spinner and show completion message with duration
			if p.statusLine != nil {
				p.statusLine.Clear()
			}
			if success {
				fmt.Printf("%s Completed: %s (%s)\n", successStyle.Styled("✓"), toolCall.Name, dur)
			} else {
				fmt.Printf("%s Failed: %s (%s)\n", errorStyle.Styled("✗"), toolCall.Name, dur)
			}
		}
	} else {
		// No tool calls, just add final newline
		fmt.Println()
	}
}

// OnError handles errors during streaming
func (p *InteractiveEventProcessor) OnError(err error) {
	if p.statusLine != nil {
		p.statusLine.Clear()
	}
	p.response = messages.ChatMessage{
		Role:    messages.MessageRoleAssistant,
		Content: fmt.Sprintf("Error: %v", err),
	}
	p.session.AddMessage(p.response)
}

// GetResponse returns the accumulated response
func (p *InteractiveEventProcessor) GetResponse() messages.ChatMessage {
	return p.response
}

// IsMessageCommitted returns whether a message has been committed to the session
func (p *InteractiveEventProcessor) IsMessageCommitted() bool {
	return p.messageCommitted
}