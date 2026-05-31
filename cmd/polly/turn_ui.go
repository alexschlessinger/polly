package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// TurnUI is the semantic output surface for a single assistant turn.
// Implementations are free to render these events however they want.
type TurnUI interface {
	Start()
	Stop()
	ShowThinking(tokens int)
	AppendAssistantText(content string)
	AppendToolStart(calls []messages.ChatMessageToolCall)
	ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool
	AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error)
	AppendWarning(text string)
	RecordTurnTokens(in, out int)
	FinishTextTurn()
}

// lineTurnUI prints turn output to an io.Writer one line at a time.
// Used for one-shot mode and as the input loop for the fallback REPL.
type lineTurnUI struct {
	config         *Config
	writer         io.Writer
	errWriter      io.Writer
	approver       *toolApprover
	needsSeparator bool
	contentPrinted bool
}

func newLineTurnUI(config *Config, inputReader *bufio.Reader) *lineTurnUI {
	ui := &lineTurnUI{
		config:    config,
		writer:    os.Stdout,
		errWriter: os.Stderr,
	}
	// Only prompt for confirmation when stdin can actually answer. A piped
	// prompt or `< /dev/null` leaves the approval reader at EOF, which would
	// otherwise deny every tool call.
	if config.Confirm && inputReader != nil && canPromptOnStdin() {
		ui.approver = newToolApprover(inputReader)
	}
	return ui
}

func (ui *lineTurnUI) Start() {}
func (ui *lineTurnUI) Stop()  {}

func (ui *lineTurnUI) ShowThinking(tokens int) {}

func (ui *lineTurnUI) AppendAssistantText(content string) {
	if ui.config.SchemaPath != "" {
		return
	}
	if ui.needsSeparator {
		fmt.Fprintln(ui.writer)
		ui.needsSeparator = false
	}
	fmt.Fprint(ui.writer, content)
	ui.contentPrinted = true
}

func (ui *lineTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	ui.needsSeparator = true
	if !toolDisplayEnabled(ui.config) {
		return
	}
	if ui.contentPrinted {
		fmt.Fprintln(ui.writer)
		ui.contentPrinted = false
	}
	for _, tc := range calls {
		fmt.Fprintf(ui.errWriter, "  → %s\n", toolLabel(tc))
	}
}

func (ui *lineTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if ui.approver == nil {
		approved := make([]bool, len(calls))
		for i := range approved {
			approved[i] = true
		}
		return approved
	}
	return ui.approver.approveToolCalls(calls)
}

func (ui *lineTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	if !toolDisplayEnabled(ui.config) {
		return
	}
	label := toolLabel(call)
	if toolWasDenied(result) {
		fmt.Fprintf(ui.errWriter, "  ✗ denied %s\n", label)
		return
	}
	dur := formatElapsed(duration)
	if err != nil {
		// Tool output/error text is intentionally omitted; the model still
		// receives the full output. The ✗ alone marks the failure.
		fmt.Fprintf(ui.errWriter, "  ✗ %s %s\n", dur, label)
		return
	}
	fmt.Fprintf(ui.errWriter, "  ✓ %s %s\n", dur, label)
}

func (ui *lineTurnUI) AppendWarning(text string) {
	if ui.config.SchemaPath != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", text)
		return
	}
	fmt.Fprintf(ui.writer, "\nWarning: %s\n", text)
}

func (ui *lineTurnUI) RecordTurnTokens(in, out int) {}

func (ui *lineTurnUI) FinishTextTurn() {
	if ui.config.SchemaPath == "" {
		fmt.Fprintln(ui.writer)
	}
}

func trimLeadingResponseNewlines(content string) string {
	return strings.TrimLeft(content, "\r\n")
}
