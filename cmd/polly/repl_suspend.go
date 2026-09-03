package main

import (
	"errors"
	"fmt"

	"github.com/gdamore/tcell/v3"
)

// suspendTerminal temporarily releases tcell's raw/alternate-screen terminal,
// suspends the process, and reacquires the terminal after the shell sends
// SIGCONT (normally via `fg`). Resume is attempted even when signaling fails.
func suspendTerminal(screen tcell.Screen, suspendProcess func() error) error {
	if screen == nil {
		return errors.New("terminal screen is unavailable")
	}
	if suspendProcess == nil {
		return errors.New("process suspension is unavailable")
	}
	if err := screen.Suspend(); err != nil {
		return fmt.Errorf("restore terminal: %w", err)
	}

	suspendErr := suspendProcess()
	resumeErr := screen.Resume()
	if suspendErr != nil {
		suspendErr = fmt.Errorf("stop process group: %w", suspendErr)
	}
	if resumeErr != nil {
		resumeErr = fmt.Errorf("resume terminal: %w", resumeErr)
	}
	return errors.Join(suspendErr, resumeErr)
}

// suspendUI clears terminal-only effects before handing the terminal back to
// the shell, then recreates their state so the next render repaints cleanly.
func (r *managedREPL) suspendUI(screen tcell.Screen) error {
	if r.images != nil {
		r.images.shutdown()
		r.images = nil
	}
	if r.fx != nil {
		r.fx.shutdown()
	}

	err := suspendTerminal(screen, r.suspendProcess)

	r.fx = newTerminalFX(screen)
	r.images = newTerminalImageManager(screen)
	r.model.mu.Lock()
	r.model.nativeImages = r.images != nil
	r.model.visual.invalidate()
	r.model.mu.Unlock()
	return err
}
