package main

import (
	"fmt"
	"slices"

	"github.com/alexschlessinger/pollytool/llm"
)

func (ui *lineTurnUI) SupportsAttemptRecovery() bool {
	// Raw/piped streaming is machine output and cannot retract bytes. Human
	// terminals can keep them only with an explicit discarded-attempt boundary.
	return ui.settledOutput || ui.config.SchemaPath != "" ||
		(ui.stdoutTTY && (ui.interactive || ui.capabilities.rendersLineANSI()))
}

func (ui *lineTurnUI) ModelAttemptStarted(_, _ int) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.attemptOutput = false
	ui.attemptThought = reasoningRecord{}
	if a := ui.activity; a != nil {
		a.retry = modelRetryStatus{}
		if a.details != nil {
			ui.attemptThought = a.details.thought
			ui.attemptThought.tail = slices.Clone(a.details.thought.tail)
		}
	}
}

func (ui *lineTurnUI) ModelAttemptAborted(_, attempt int, _ error) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed {
		return
	}
	ui.clearActivityLocked()
	// Commit the failed tail once, including images already emitted to the
	// terminal. flush resets the stream's offsets and image ownership.
	ui.flushBufferedMarkdown()
	if ui.attemptOutput && !ui.settledOutput && ui.config.SchemaPath == "" {
		if ui.contentPrinted && !ui.endsWithNewline {
			fmt.Fprintln(ui.writer)
			ui.endsWithNewline = true
		}
		ui.statusLineLocked(discardedAttemptLabel(attempt))
		ui.needsSeparator = true
	}
	if a := ui.activity; a != nil {
		a.bankThinking()
		a.state, a.toolName = turnStateWaiting, ""
		if a.details != nil {
			// Failed reasoning must not appear in the accepted turn's details.
			a.details.thought = ui.attemptThought
		}
	}
	ui.attemptOutput = false
	ui.renderActivityLocked()
}

func (ui *lineTurnUI) ModelRetry(event llm.RetryEvent) {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	if ui.completed || ui.activity == nil {
		return
	}
	ui.activity.retry.update(event)
	if event.Waiting {
		ui.activity.bankThinking()
		ui.activity.state, ui.activity.toolName = turnStateWaiting, ""
	}
	ui.renderActivityLocked()
}
