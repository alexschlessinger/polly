package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/muesli/termenv"
)

// StatusHandler is the interface for different status display implementations
type StatusHandler interface {
	// Lifecycle methods
	Start()
	Stop()
	Clear()

	// Status display methods
	ShowSpinner(text string)
	ShowThinking(tokens int)
	ShowToolCall(name string)

	// Progress updates
	ClearForContent()
	UpdateStreamingProgress(bytes int)
	UpdateThinkingProgress(tokens int)
}

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

// ShowThinking shows thinking status with token count
func (s *Status) ShowThinking(tokens int) {
	s.SetStatus("thinking (%d tokens)", tokens)
}

// ShowToolCall shows tool execution status
func (s *Status) ShowToolCall(name string) {
	s.SetStatus("running tool: %s", name)
}

// ClearForContent clears status before content streaming
func (s *Status) ClearForContent() {
	// Start spinner with streaming status
	s.startSpinnerWithText("streaming...")
}

// UpdateStreamingProgress updates streaming progress
func (s *Status) UpdateStreamingProgress(bytes int) {
	// Update spinner text with character count
	s.mu.Lock()
	s.currentText = fmt.Sprintf("streaming (%d chars)", bytes)
	s.mu.Unlock()
}

// UpdateThinkingProgress updates thinking progress
func (s *Status) UpdateThinkingProgress(tokens int) {
	// Update spinner text with token count
	s.mu.Lock()
	s.currentText = fmt.Sprintf("thinking (%d tokens)", tokens)
	s.mu.Unlock()
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
				// Only show elapsed time for waiting and tool execution
				if s.currentText == "waiting" || strings.HasPrefix(s.currentText, "running tool:") {
					elapsed := time.Since(s.startTime).Seconds()
					setTerminalTitle(fmt.Sprintf("%s %s [%.1fs]", spinnerChars[s.spinnerIndex%len(spinnerChars)], s.currentText, elapsed))
				} else {
					setTerminalTitle(fmt.Sprintf("%s %s", spinnerChars[s.spinnerIndex%len(spinnerChars)], s.currentText))
				}
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

// Clear clears the terminal title
func (s *Status) Clear() {
	// Stop spinner and clear the title
	s.stopSpinner()
	setTerminalTitle("")
}

// InteractiveStatus handles status display for interactive mode
type InteractiveStatus struct {
	output         *termenv.Output
	mu             sync.Mutex
	active         bool
	spinner        []string
	spinIdx        int
	stopChan       chan struct{}
	currentMessage string // Current status message being displayed
}

// NewInteractiveStatus creates a new interactive status handler
func NewInteractiveStatus() *InteractiveStatus {
	return &InteractiveStatus{
		output:  termenv.NewOutput(os.Stderr),
		spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
}

// ShowStatus shows a status message in place of the prompt
func (s *InteractiveStatus) ShowStatus(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update the current message
	s.currentMessage = message

	if !s.active {
		s.active = true
		// Move cursor to beginning of line and clear it
		fmt.Fprint(s.output, "\r")
		s.output.ClearLine()
	}

	// Show spinner and message
	fmt.Fprintf(s.output, "%s %s", s.spinner[s.spinIdx], message)
}

// ShowSpinner starts the spinner animation
func (s *InteractiveStatus) ShowSpinner(message string) {
	s.mu.Lock()
	s.currentMessage = message
	
	if s.active {
		// Spinner already running, just update the message
		s.mu.Unlock()
		return
	}
	s.active = true
	s.stopChan = make(chan struct{})
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
				s.mu.Lock()
				s.spinIdx = (s.spinIdx + 1) % len(s.spinner)
				// Move to start of line, clear, and redraw with current message
				fmt.Fprint(s.output, "\r")
				s.output.ClearLine()
				fmt.Fprintf(s.output, "%s %s", s.spinner[s.spinIdx], s.currentMessage)
				s.mu.Unlock()
			}
		}
	}()
}

// StopSpinner stops the spinner and clears the line
func (s *InteractiveStatus) StopSpinner() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active && s.stopChan != nil {
		close(s.stopChan)
		s.stopChan = nil
	}
	s.active = false

	// Clear the status line - don't print prompt, readline will do that
	fmt.Fprint(s.output, "\r")
	s.output.ClearLine()
}

// Clear clears any status
func (s *InteractiveStatus) Clear() {
	s.StopSpinner()
}

// ShowToolCall shows that a tool is being called
func (s *InteractiveStatus) ShowToolCall(toolName string) {
	s.Clear()
	// Print tool call as regular output
	fmt.Printf("→ Running tool: %s", toolName)
}

// ShowThinking shows thinking status
func (s *InteractiveStatus) ShowThinking(tokens int) {
	message := fmt.Sprintf("thinking (%d tokens)", tokens)
	
	s.mu.Lock()
	s.currentMessage = message
	
	if !s.active {
		// Start the spinner if not already running
		s.mu.Unlock()
		s.ShowSpinner(message)
	} else {
		// Just update the message, spinner will pick it up on next tick
		s.mu.Unlock()
	}
}

// Start begins status display (no-op for interactive)
func (s *InteractiveStatus) Start() {
	// No-op for interactive status
}

// Stop ends status display
func (s *InteractiveStatus) Stop() {
	s.Clear()
}

// ClearForContent clears status before content streaming
func (s *InteractiveStatus) ClearForContent() {
	s.Clear()
}

// UpdateStreamingProgress updates streaming progress (no-op for interactive)
func (s *InteractiveStatus) UpdateStreamingProgress(bytes int) {
	// No-op - we show content directly in interactive mode
}

// UpdateThinkingProgress updates thinking progress
func (s *InteractiveStatus) UpdateThinkingProgress(tokens int) {
	s.ShowThinking(tokens)
}

