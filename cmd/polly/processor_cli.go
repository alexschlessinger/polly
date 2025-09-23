package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// CLIEventProcessor handles event processing for command-line mode
type CLIEventProcessor struct {
	config             *Config
	session            sessions.Session
	registry           *tools.ToolRegistry
	statusLine         StatusHandler
	schema             *llm.Schema
	response           messages.ChatMessage
	responseText       strings.Builder
	reasoningLength    int
	firstByteReceived  bool
	messageCommitted   bool
	ctx                context.Context
}

// NewCLIEventProcessor creates a new CLI event processor
func NewCLIEventProcessor(
	ctx context.Context,
	config *Config,
	session sessions.Session,
	registry *tools.ToolRegistry,
	statusLine StatusHandler,
	schema *llm.Schema,
) *CLIEventProcessor {
	return &CLIEventProcessor{
		ctx:        ctx,
		config:     config,
		session:    session,
		registry:   registry,
		statusLine: statusLine,
		schema:     schema,
	}
}

// OnReasoning handles reasoning content
func (p *CLIEventProcessor) OnReasoning(content string, totalLength int) {
	p.reasoningLength = totalLength
	if p.statusLine != nil {
		p.statusLine.UpdateThinkingProgress(totalLength)
	}
}

// OnContent handles regular content streaming
func (p *CLIEventProcessor) OnContent(content string, firstChunk bool) {
	// Clear status on first content
	if firstChunk && !p.firstByteReceived && p.statusLine != nil {
		p.firstByteReceived = true
		p.statusLine.ClearForContent()
	}

	// Accumulate content for the session
	p.responseText.WriteString(content)

	// Print content as it arrives (unless using schema/structured output)
	if p.config.SchemaPath == "" && content != "" {
		fmt.Print(content)
	}

	// Update streaming progress for status line
	if p.statusLine != nil && p.config.SchemaPath == "" {
		p.statusLine.UpdateStreamingProgress(p.responseText.Len())
	}
}

// OnToolCall handles tool call events (not used in current implementation)
func (p *CLIEventProcessor) OnToolCall(toolCall messages.ChatMessageToolCall) {
	// Tool calls are handled in OnComplete in the current implementation
}

// OnComplete handles the complete message
func (p *CLIEventProcessor) OnComplete(message *messages.ChatMessage) {
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
		if p.response.Content != "" && p.config.SchemaPath != "" {
			// In JSON mode, we'll output everything at the end
		} else if p.response.Content != "" && p.responseText.Len() == 0 {
			// Content wasn't streamed (responseText is empty), print it now
			fmt.Print(p.response.Content)
		} else if p.responseText.Len() > 0 && p.config.SchemaPath == "" {
			// Content was streamed, add a newline before tool output
			fmt.Println()
		}

		// Execute tool calls
		for _, toolCall := range p.response.ToolCalls {
			_ = executeToolCall(p.ctx, toolCall, p.registry, p.session, p.statusLine)
		}
	}

	// Output final response if no tool calls
	if len(p.response.ToolCalls) == 0 {
		if p.config.SchemaPath != "" {
			outputStructured(p.response.Content, p.schema)
		} else {
			fmt.Println()
		}
	}
}

// OnError handles errors during streaming
func (p *CLIEventProcessor) OnError(err error) {
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
func (p *CLIEventProcessor) GetResponse() messages.ChatMessage {
	return p.response
}

// IsMessageCommitted returns whether a message has been committed to the session
func (p *CLIEventProcessor) IsMessageCommitted() bool {
	return p.messageCommitted
}