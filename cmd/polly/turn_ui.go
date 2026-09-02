package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// TurnUI is the semantic output surface for a single assistant turn.
// Implementations are free to render these events however they want.
type TurnUI interface {
	Start()
	Stop()
	// ShowThinking reports one streamed reasoning chunk verbatim;
	// implementations accumulate for running totals or live excerpts.
	ShowThinking(chunk string)
	AppendAssistantText(content string)
	AppendToolStart(calls []messages.ChatMessageToolCall)
	ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool
	AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error)
	// AppendToolMedia surfaces exact typed image parts that entered the model's
	// tool result. It is separate from text/path discovery so UIs can pin a
	// trustworthy inspection receipt without exposing arbitrary tool output.
	AppendToolMedia(call messages.ChatMessageToolCall, images []transcriptImage)
	AppendWarning(text string)
	RecordTurnTokens(in, out int)
	// RecordContextUsage reports the turn's context consumption against the
	// resolved budget; estimated marks a pre-response projection.
	RecordContextUsage(used, limit int, estimated bool)
	FinishTextTurn()
	// UserMessagePersistenceStarted and UserMessagePersistenceFinished bracket
	// the durable write of the user message, so a UI that projects history
	// concurrently can serialize against it.
	UserMessagePersistenceStarted()
	UserMessagePersistenceFinished(persisted bool)
	// TurnPersistenceAllowed reports whether the settled turn may still be
	// written to the session; a detached REPL turn vetoes the write.
	TurnPersistenceAllowed() bool
}

// lineTurnUI writes raw streamed output or buffered ANSI Markdown according to
// the resolved stdout capabilities. It serves one-shot and fallback REPL turns.
type lineTurnUI struct {
	config          *Config
	writer          io.Writer
	errWriter       io.Writer
	approver        *toolApprover
	capabilities    outputCapabilities
	imageBaseDir    string
	markdownBuffer  strings.Builder
	bufferSeparator bool
	needsSeparator  bool
	contentPrinted  bool
	endsWithNewline bool
	finished        bool
	toolMu          sync.Mutex
	stderrTTY       bool
}

func newLineTurnUIWithCapabilities(config *Config, inputReader *bufio.Reader, capabilities outputCapabilities) *lineTurnUI {
	baseDir, _ := os.Getwd()
	ui := &lineTurnUI{
		config:       config,
		writer:       os.Stdout,
		errWriter:    os.Stderr,
		capabilities: capabilities,
		imageBaseDir: baseDir,
		stderrTTY:    terminalFD(int(os.Stderr.Fd())),
	}
	// Only prompt for confirmation when stdin can actually answer. A piped
	// prompt or `< /dev/null` leaves the approval reader at EOF, which would
	// otherwise deny every tool call.
	if config.Confirm && inputReader != nil && canPromptOnStdin() {
		ui.approver = newToolApprover(inputReader)
	}
	return ui
}

func (ui *lineTurnUI) Start() {
	ui.markdownBuffer.Reset()
	ui.bufferSeparator = false
	ui.needsSeparator = false
	ui.contentPrinted = false
	ui.endsWithNewline = false
	ui.finished = false
}

func (ui *lineTurnUI) Stop() {
	if ui.finished || ui.config.SchemaPath != "" || !ui.capabilities.rendersLineANSI() {
		return
	}
	ui.flushBufferedMarkdown()
	if ui.contentPrinted && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
}

func (ui *lineTurnUI) ShowThinking(chunk string) {}

func (ui *lineTurnUI) AppendAssistantText(content string) {
	if ui.config.SchemaPath != "" {
		return
	}
	if ui.capabilities.rendersLineANSI() {
		if content == "" {
			return
		}
		if ui.needsSeparator {
			ui.bufferSeparator = true
			ui.needsSeparator = false
		}
		ui.markdownBuffer.WriteString(content)
		return
	}
	if ui.needsSeparator {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
		ui.needsSeparator = false
	}
	fmt.Fprint(ui.writer, content)
	if content != "" {
		ui.contentPrinted = true
		ui.endsWithNewline = strings.HasSuffix(content, "\n")
	}
}

func (ui *lineTurnUI) flushBufferedMarkdown() {
	if !ui.capabilities.rendersLineANSI() || ui.markdownBuffer.Len() == 0 {
		return
	}
	if ui.bufferSeparator {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
		ui.bufferSeparator = false
	}
	rendered := renderLineMarkdown(ui.markdownBuffer.String(), ui.imageBaseDir, ui.capabilities)
	ui.markdownBuffer.Reset()
	if len(rendered) == 0 {
		return
	}
	_, _ = ui.writer.Write(rendered)
	ui.contentPrinted = true
	ui.endsWithNewline = rendered[len(rendered)-1] == '\n'
}

func (ui *lineTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	ui.flushBufferedMarkdown()
	ui.needsSeparator = true
	if !toolDisplayEnabled(ui.config) {
		return
	}
	if ui.contentPrinted {
		if !ui.endsWithNewline {
			fmt.Fprintln(ui.writer)
			ui.endsWithNewline = true
		}
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
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
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
		// receives the full output. The ✗ and exit code alone mark the failure.
		fmt.Fprintf(ui.errWriter, "  ✗ %s\n", toolLineBody(dur, label, toolFailureMeta(err)))
		return
	}
	fmt.Fprintf(ui.errWriter, "  ✓ %s\n", toolLineBody(dur, label, resultLineMeta(result)))
}

func (ui *lineTurnUI) AppendToolMedia(_ messages.ChatMessageToolCall, images []transcriptImage) {
	if len(images) == 0 || ui.config.SchemaPath != "" || ui.config.Quiet {
		return
	}
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	for _, img := range images {
		fmt.Fprintf(ui.errWriter, "    %s\n", transcriptImageCaptionText(img))
		if !ui.capabilities.rendersLineANSI() || !ui.stderrTTY {
			continue
		}
		if payload := lineImagePayload(img, ui.capabilities, 4); len(payload) > 0 {
			_, _ = ui.errWriter.Write(payload)
		}
	}
}

func (ui *lineTurnUI) AppendWarning(text string) {
	ui.flushBufferedMarkdown()
	// Warnings ride stderr so a captured stdout answer stays clean. Terminate
	// any unfinished stdout line first so a shared terminal doesn't glue the
	// warning onto the tail of the streamed answer.
	if ui.contentPrinted && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
	fmt.Fprintf(ui.errWriter, "Warning: %s\n", text)
}

func (ui *lineTurnUI) RecordTurnTokens(in, out int) {}

func (ui *lineTurnUI) RecordContextUsage(used, limit int, estimated bool) {}

func (ui *lineTurnUI) UserMessagePersistenceStarted() {}

func (ui *lineTurnUI) UserMessagePersistenceFinished(persisted bool) {}

func (ui *lineTurnUI) TurnPersistenceAllowed() bool { return true }

func (ui *lineTurnUI) FinishTextTurn() {
	ui.flushBufferedMarkdown()
	if ui.config.SchemaPath == "" && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
	ui.finished = true
}

func trimLeadingResponseNewlines(content string) string {
	return strings.TrimLeft(content, "\r\n")
}
