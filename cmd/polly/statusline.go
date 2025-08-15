package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Status manages terminal title updates for status display
type Status struct {
	mainWriter    io.Writer
	mu            sync.Mutex
	currentText   string
	spinnerActive bool
	spinnerStop   chan bool
	spinnerIndex  int
	startTime     time.Time
}

// NewStatus creates a new terminal title status line manager
func NewStatus() *Status {
	return &Status{
		mainWriter:  os.Stdout,
		spinnerStop: make(chan bool, 1),
	}
}

// setTerminalTitle sets the terminal title using ANSI escape codes
func setTerminalTitle(title string) {
	// OSC 2 sets window title, OSC 0 sets both window and icon title
	// Using OSC 0 for broader compatibility
	fmt.Fprintf(os.Stderr, "\033]0;%s\007", title)
}

// saveTerminalTitle saves the current terminal title (limited support)
func saveTerminalTitle() {
	// OSC 22 pushes title to stack (not universally supported)
	fmt.Fprint(os.Stderr, "\033[22;0t")
}

// restoreTerminalTitle restores the saved terminal title
func restoreTerminalTitle() {
	// OSC 23 pops title from stack (not universally supported)
	// Fallback: just clear the title
	fmt.Fprint(os.Stderr, "\033[23;0t")
	// Additional fallback to reset to empty
	setTerminalTitle("")
}

// Start begins the status line updates
func (s *Status) Start() {
	// Initialize start time
	s.mu.Lock()
	s.startTime = time.Now()
	s.mu.Unlock()

	// Save current title when starting
	saveTerminalTitle()
}

// Stop stops the status line updates and restores the terminal title
func (s *Status) Stop() {
	// Stop spinner and restore original title
	s.stopSpinner()
	restoreTerminalTitle()
}

// SetStatus updates the terminal title with status text
func (s *Status) SetStatus(format string, args ...any) {
	statusText := fmt.Sprintf(format, args...)

	// Stop any spinner and update terminal title with elapsed time
	s.stopSpinner()
	s.mu.Lock()
	elapsed := time.Since(s.startTime).Seconds()
	s.mu.Unlock()
	setTerminalTitle(fmt.Sprintf("%s [%.1fs]", statusText, elapsed))
}

// ShowSpinner shows a spinner in the terminal title
func (s *Status) ShowSpinner(text string) {
	s.startSpinnerWithText(text)
}

// stopSpinner stops the terminal title spinner if active
func (s *Status) stopSpinner() {
	s.mu.Lock()
	wasActive := s.spinnerActive
	s.spinnerActive = false
	s.mu.Unlock()

	if wasActive {
		// Send stop signal (non-blocking)
		select {
		case s.spinnerStop <- true:
		default:
		}
	}
}

// startSpinnerWithText starts or updates the spinner with new text
func (s *Status) startSpinnerWithText(text string) {
	s.mu.Lock()
	s.currentText = text
	wasActive := s.spinnerActive
	s.spinnerActive = true
	s.mu.Unlock()

	// Only start a new goroutine if spinner wasn't already active
	if !wasActive {
		go s.runSpinner()
	}
}

// runSpinner runs the spinner animation goroutine
func (s *Status) runSpinner() {
	// Using simple ASCII spinner for broader compatibility in titles
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	// Alternative ASCII: []string{"|", "/", "-", "\\"}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.spinnerStop:
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.spinnerActive {
				elapsed := time.Since(s.startTime).Seconds()
				setTerminalTitle(fmt.Sprintf("%s %s [%.1fs]", spinnerChars[s.spinnerIndex%len(spinnerChars)], s.currentText, elapsed))
				s.spinnerIndex++
			}
			s.mu.Unlock()
		}
	}
}

// ShowToolExecution shows tool execution information in terminal title
func (s *Status) ShowToolExecution(toolName string, args map[string]any) {
	s.startSpinnerWithText(toolName)
}

// Print writes to the main output (stdout), preserving the status line
func (s *Status) Print(text string) {
	// Write directly to stdout
	fmt.Fprint(s.mainWriter, text)
}

// Clear clears the terminal title
func (s *Status) Clear() {
	// Stop spinner and clear the title
	s.stopSpinner()
	setTerminalTitle("")
}

// ClearForContent prepares for content streaming
func (s *Status) ClearForContent() {
	// Start spinner with streaming status
	s.startSpinnerWithText("streaming (0 chars)")
}

// UpdateStreamingProgress updates the terminal title with streaming progress
func (s *Status) UpdateStreamingProgress(charCount int) {
	// Update spinner text without stopping it
	s.mu.Lock()
	s.currentText = fmt.Sprintf("streaming (%d chars)", charCount)
	s.mu.Unlock()
}

// UpdateThinkingProgress updates the terminal title to show thinking state
func (s *Status) UpdateThinkingProgress(charCount int) {
	// Update spinner text to show thinking
	s.mu.Lock()
	s.currentText = fmt.Sprintf("thinking (%d chars)", charCount)
	s.mu.Unlock()
}
