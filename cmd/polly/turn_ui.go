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

type lineTurnUI struct {
	config         *Config
	statusLine     StatusHandler
	writer         io.Writer
	approver       *toolApprover
	needsSeparator bool
	contentPrinted bool
}

func newLineTurnUI(config *Config, statusLine StatusHandler, inputReader *bufio.Reader) *lineTurnUI {
	ui := &lineTurnUI{
		config:     config,
		statusLine: statusLine,
		writer:     os.Stdout,
	}
	if statusLine != nil {
		ui.writer = statusLine.ContentWriter()
	}
	if config.Confirm {
		ui.approver = newToolApprover(inputReader)
	}
	return ui
}

func (ui *lineTurnUI) Start() {
	if ui.statusLine == nil {
		return
	}
	ui.statusLine.Start()
	ui.statusLine.ShowSpinner("waiting")
}

func (ui *lineTurnUI) Stop() {
	if ui.statusLine != nil {
		ui.statusLine.Stop()
	}
}

func (ui *lineTurnUI) ShowThinking(tokens int) {
	if ui.statusLine != nil {
		ui.statusLine.UpdateThinkingProgress(tokens)
	}
}

func (ui *lineTurnUI) AppendAssistantText(content string) {
	if ui.statusLine != nil {
		ui.statusLine.ClearForContent()
	}
	if ui.config.SchemaPath != "" {
		return
	}
	if ui.needsSeparator {
		fmt.Fprintln(ui.writer)
		ui.needsSeparator = false
	}
	fmt.Fprint(ui.writer, assistantOut.Styled(content))
	ui.contentPrinted = true
}

func (ui *lineTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	ui.needsSeparator = true
	if !toolDisplayEnabled(ui.config) {
		if ui.statusLine != nil && len(calls) > 0 && ui.approver == nil {
			ui.statusLine.ShowToolCall(calls[0].Name)
		}
		return
	}
	if ui.contentPrinted {
		fmt.Fprintln(ui.writer)
		ui.contentPrinted = false
	}
	for _, tc := range calls {
		printToolStart(ui.writer, tc)
	}
	if ui.statusLine != nil && len(calls) > 0 && ui.approver == nil {
		ui.statusLine.ShowToolCall(calls[0].Name)
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
	if ui.statusLine != nil {
		ui.statusLine.Clear()
	}
	return ui.approver.approveToolCalls(calls)
}

func (ui *lineTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	if ui.statusLine != nil {
		ui.statusLine.Clear()
	}
	if !toolDisplayEnabled(ui.config) {
		return
	}
	printToolEnd(ui.writer, call, duration, err, result)
}

func (ui *lineTurnUI) AppendWarning(text string) {
	if ui.config.SchemaPath != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", text)
		return
	}
	fmt.Fprintf(ui.writer, "\nWarning: %s\n", text)
}

func (ui *lineTurnUI) RecordTurnTokens(in, out int) {
	if ui.statusLine != nil {
		ui.statusLine.RecordTurnTokens(in, out)
	}
}

func (ui *lineTurnUI) FinishTextTurn() {
	if ui.config.SchemaPath == "" {
		fmt.Fprintln(ui.writer)
	}
}

func trimLeadingResponseNewlines(content string) string {
	return strings.TrimLeft(content, "\r\n")
}
