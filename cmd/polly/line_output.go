package main

import (
	"io"
)

// Output errors participate in the turn outcome just like provider and save
// errors. All access is under the renderer lock; the first failure is enough.
type lineCheckedWriter struct {
	writer io.Writer
	err    error
}

func (w *lineCheckedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func checkedLineWriter(w io.Writer) *lineCheckedWriter {
	if previous, ok := w.(*lineCheckedWriter); ok {
		w = previous.writer
	}
	return &lineCheckedWriter{writer: w}
}

// FlushOutputError commits any interrupted rich tail without adding bytes to
// a raw partial answer. It must run before classifying the final outcome.
func (ui *lineTurnUI) FlushOutputError() error {
	ui.toolMu.Lock()
	defer ui.toolMu.Unlock()
	ui.clearActivityLocked()
	if ui.config.SchemaPath == "" {
		ui.flushBufferedMarkdown()
	}
	return ui.outputErrorLocked()
}

// outputErrorLocked reports a failure delivering the answer. Status rows and
// the settled summary on stderr are chrome, not the contract: a redirected or
// closed stderr must not turn a fully delivered stdout answer into exit 1.
func (ui *lineTurnUI) outputErrorLocked() error {
	if checked, ok := ui.writer.(*lineCheckedWriter); ok {
		return checked.err
	}
	return nil
}

func flushTurnOutputError(turnUI TurnUI) error {
	if output, ok := turnUI.(interface{ FlushOutputError() error }); ok {
		return output.FlushOutputError()
	}
	return nil
}
