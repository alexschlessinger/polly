package main

import (
	"bufio"
	"bytes"
	"context"
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
	// ApproveToolCalls asks for each call's approval. ctx ends a pending
	// request when the execution behind it ends; requester names the swarm
	// member asking, or is "" for this turn's own calls.
	ApproveToolCalls(ctx context.Context, requester string, calls []messages.ChatMessageToolCall) []bool
	AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error)
	// AppendToolResult surfaces the exact tool result the model saw, for
	// surfaces that inspect it.
	AppendToolResult(call messages.ChatMessageToolCall, result messages.ChatMessage)
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
	CompleteTurn(turnCompletion)
	// UserMessagePersistenceStarted and UserMessagePersistenceFinished bracket
	// the durable write of the user message, so a UI that projects history
	// concurrently can serialize against it.
	UserMessagePersistenceStarted()
	UserMessagePersistenceFinished(persisted bool)
	// TurnPersistenceAllowed reports whether the settled turn may still be
	// written to the session; a detached REPL turn vetoes the write.
	TurnPersistenceAllowed() bool
}

// approveToolCalls asks ui for approval unless the execution asking has
// already ended.
func approveToolCalls(ctx context.Context, ui TurnUI, requester string, calls []messages.ChatMessageToolCall) []bool {
	if ctx.Err() != nil {
		return denyToolCalls(calls)
	}
	return ui.ApproveToolCalls(ctx, requester, calls)
}

func denyToolCalls(calls []messages.ChatMessageToolCall) []bool {
	return make([]bool, len(calls))
}

func approveAllToolCalls(calls []messages.ChatMessageToolCall) []bool {
	approved := make([]bool, len(calls))
	for i := range approved {
		approved[i] = true
	}
	return approved
}

// turnUIBase is the no-op behaviour a TurnUI embeds for the events it has
// no use for: lifecycle brackets, TUI-only meters, and persistence gates
// that always allow.
type turnUIBase struct{}

func (turnUIBase) Start()                                                              {}
func (turnUIBase) Stop()                                                               {}
func (turnUIBase) AppendToolResult(messages.ChatMessageToolCall, messages.ChatMessage) {}
func (turnUIBase) RecordContextUsage(int, int, bool)                                   {}
func (turnUIBase) FinishTextTurn()                                                     {}
func (turnUIBase) UserMessagePersistenceStarted()                                      {}
func (turnUIBase) UserMessagePersistenceFinished(bool)                                 {}
func (turnUIBase) TurnPersistenceAllowed() bool                                        { return true }

// lineTurnUI streams raw output or owns a terminal Markdown tail and footer,
// according to each stream's capabilities. It also serves fallback REPL turns.
type lineTurnUI struct {
	turnUIBase
	settledOutput bool
	// interactive marks a REPL turn: the answer streams as it arrives. Settled
	// output, one answer after the run, is for one-shot and piped runs only.
	interactive              bool
	config                   *Config
	writer                   io.Writer
	errWriter                io.Writer
	approver                 *toolApprover
	capabilities             outputCapabilities
	imageBaseDir             string
	markdownBuffer           strings.Builder
	bufferSeparator          bool
	needsSeparator           bool
	contentPrinted           bool
	endsWithNewline          bool
	finished                 bool
	toolMu                   sync.Mutex
	stderrTTY                bool
	stdoutTTY                bool
	activity                 *lineActivity
	stream                   *lineStream
	statusFrame              lineTerminalFrame
	sameTerminal             bool
	renderCancel, renderDone chan struct{}
	completed                bool
	size                     func(stdout bool) (columns, rows int)
	// promptMu serializes approval prompts on the shared stdin reader; it is
	// always taken before toolMu, never while holding it. prompting and
	// pending are guarded by toolMu.
	promptMu  sync.Mutex
	prompting bool
	pending   bytes.Buffer
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
		stdoutTTY:    terminalFD(int(os.Stdout.Fd())),
	}
	// Two TTYs are one screen: /dev/tty and /dev/ttysNNN stat differently
	// yet paint the same terminal, so an inode comparison would split the
	// frame into two cursor owners that overwrite each other.
	ui.sameTerminal = ui.stdoutTTY && ui.stderrTTY
	// Only prompt for confirmation when stdin can actually answer. A piped
	// prompt or `< /dev/null` leaves the approval reader at EOF, which would
	// otherwise deny every tool call.
	if config.Confirm && inputReader != nil && canPromptOnStdin() {
		ui.approver = newToolApprover(inputReader)
	}
	return ui
}

func (ui *lineTurnUI) Start() {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.writer, ui.errWriter = checkedLineWriter(ui.writer), checkedLineWriter(ui.errWriter)
	ui.markdownBuffer.Reset()
	ui.bufferSeparator = false
	ui.needsSeparator = false
	ui.contentPrinted = false
	ui.endsWithNewline = false
	ui.finished = false
	ui.completed = false
	ui.activity, ui.stream = nil, nil
	ui.statusFrame = lineTerminalFrame{}
	if ui.capabilities.rendersLineANSI() && ui.stdoutTTY && ui.config.SchemaPath == "" {
		ui.stream = &lineStream{displayedImages: make(map[int]bool)}
	}
	ui.startActivityLocked()
	ui.startRendererLocked()
}

func (ui *lineTurnUI) Stop() {
	ui.toolMu.Lock()
	ui.clearActivityLocked()
	if !ui.finished && ui.config.SchemaPath == "" && ui.capabilities.rendersLineANSI() {
		ui.flushBufferedMarkdown()
	}
	if ui.config.SchemaPath == "" && (ui.stdoutTTY || ui.capabilities.rendersLineANSI()) && ui.contentPrinted && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
	ui.finishActivityLocked()
	ui.completed = true
	ui.stopRendererLocked()
	done := ui.renderDone
	ui.toolMu.Unlock()
	if done != nil {
		<-done
	}
}

func (ui *lineTurnUI) ShowThinking(chunk string) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	if chunk != "" {
		if a := ui.activity; a != nil && a.details != nil && !a.stopped {
			a.details.appendThought(chunk, a.state != turnStateThinking)
		}
		ui.activityStateLocked(turnStateThinking, "")
	}
	ui.renderActivityLocked()
}

func (ui *lineTurnUI) AppendAssistantText(content string) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	if !ui.capabilities.rendersLineANSI() {
		ui.clearActivityLocked()
	}
	defer ui.renderActivityLocked()
	if content != "" && !ui.settledOutput {
		ui.activityStateLocked(turnStateStreaming, "")
	}
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
	if ui.prompting || !ui.capabilities.rendersLineANSI() || ui.markdownBuffer.Len() == 0 {
		return
	}
	if ui.stream != nil {
		ui.stream.flush(ui)
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
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	ui.clearActivityLocked()
	defer ui.renderActivityLocked()
	ui.flushBufferedMarkdown()
	ui.needsSeparator = !ui.settledOutput
	if !ui.activityEnabled() {
		return
	}
	if ui.contentPrinted && (ui.stdoutTTY || ui.capabilities.rendersLineANSI()) {
		if !ui.endsWithNewline {
			fmt.Fprintln(ui.writer)
			ui.endsWithNewline = true
		}
		ui.contentPrinted = false
	}
	if len(calls) > 0 {
		ui.activityStateLocked(turnStateTool, calls[0].Name)
	}
	ui.activityToolsLocked("", calls)
}

func (ui *lineTurnUI) ApproveToolCalls(_ context.Context, _ string, calls []messages.ChatMessageToolCall) []bool {
	if ui.approver == nil {
		return approveAllToolCalls(calls)
	}
	// Children forward approvals from inside the parent's tool goroutines, so
	// the stdin read must not hold toolMu: siblings keep consuming their
	// streams (and touching the stall watchdog) while the user decides. The
	// prompting flag parks repaints and notices instead, so nothing lands on
	// the prompt line; promptMu keeps concurrent prompts off the same reader.
	ui.promptMu.Lock()
	defer ui.promptMu.Unlock()
	ui.toolMu.Lock()
	ui.clearActivityLocked()
	ui.flushBufferedMarkdown()
	ui.prompting = true
	ui.toolMu.Unlock()
	approved := ui.approver.approveToolCalls(calls)
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.prompting = false
	ui.flushBufferedMarkdown()
	if ui.pending.Len() > 0 {
		_, _ = ui.errWriter.Write(ui.pending.Bytes())
		ui.pending.Reset()
	}
	ui.renderActivityLocked()
	return approved
}

func (ui *lineTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed || !ui.activityEnabled() {
		return
	}
	ui.activityToolEndLocked("", "", call, result, duration, err)
	ui.renderActivityLocked()
}

func (ui *lineTurnUI) AppendToolMedia(_ messages.ChatMessageToolCall, images []transcriptImage) {
	if len(images) == 0 || ui.config.Quiet {
		return
	}
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	ui.clearActivityLocked()
	ui.flushBufferedMarkdown()
	defer ui.renderActivityLocked()
	if ui.activity != nil {
		ui.activity.images += len(images)
		if ui.activity.details != nil {
			ui.activity.details.addImages(images)
		}
	}
	caps := ui.capabilities
	if ui.activity != nil {
		caps = ui.activity.imageCaps
	}
	for _, img := range images {
		ui.activityLineLocked("    " + transcriptImageCaptionText(img))
		if !caps.rendersLineANSI() || !ui.stderrTTY {
			continue
		}
		if payload := lineImagePayload(img, caps, 4); len(payload) > 0 {
			_, _ = ui.statusWriterLocked().Write(payload)
		}
	}
}

func (ui *lineTurnUI) AppendWarning(text string) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.clearActivityLocked()
	defer ui.renderActivityLocked()
	ui.flushBufferedMarkdown()
	// Warnings ride stderr so a captured stdout answer stays clean. Terminate
	// any unfinished stdout line first so a shared terminal doesn't glue the
	// warning onto the tail of the streamed answer.
	if ui.sameTerminal && ui.contentPrinted && !ui.endsWithNewline {
		fmt.Fprintln(ui.writer)
		ui.endsWithNewline = true
	}
	ui.activityLineLocked("Warning: " + text)
}

func (ui *lineTurnUI) RecordTurnTokens(in, out int) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	if ui.activity != nil {
		ui.activity.in, ui.activity.out = in, out
	}
}

func (ui *lineTurnUI) FinishTextTurn() {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.clearActivityLocked()
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
