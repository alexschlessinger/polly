package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

var (

	// termenv output for consistent terminal styling
	output = termenv.NewOutput(os.Stdout)

	// dimStyle is the only generic style still in use (approveToolCalls).
	dimStyle termenv.Style

	// Role-specific styles - initialized in initColors()
	promptStyle    termenv.Style // the "> " REPL prompt
	toolStartStyle termenv.Style // the "→ " tool-start arrow
	toolOkStyle    termenv.Style // the "✓" tool-success checkmark
	toolErrStyle   termenv.Style // the "✗" tool-failure cross
	toolLabelStyle termenv.Style // the dim text after the marker glyph
	assistantOut   termenv.Style // streamed assistant text in the REPL
	barFieldStyle  termenv.Style // status bar fields (separators + non-active text)
	barActiveStyle termenv.Style // status bar's active turn-state token
	barErrorStyle  termenv.Style // status bar when turnState=="error"
)

// initColors initializes color styles based on terminal background.
// Palette assumes a 256-color terminal; degrades cleanly via termenv when not.
func initColors() {
	if termenv.HasDarkBackground() {
		// Dark background — soft, low-saturation accents that read on grey.
		dimStyle = output.String().Foreground(output.Color("244"))

		promptStyle = output.String().Foreground(output.Color("110")).Bold()
		toolStartStyle = output.String().Foreground(output.Color("109"))
		toolOkStyle = output.String().Foreground(output.Color("114")).Bold()
		toolErrStyle = output.String().Foreground(output.Color("174")).Bold()
		toolLabelStyle = output.String().Foreground(output.Color("244"))
		assistantOut = output.String().Foreground(output.Color("252"))
		barFieldStyle = output.String().Foreground(output.Color("244"))
		barActiveStyle = output.String().Foreground(output.Color("179")).Bold()
		barErrorStyle = output.String().Foreground(output.Color("174")).Bold()
	} else {
		// Light background — saturated darks for contrast on white.
		dimStyle = output.String().Foreground(output.Color("240"))

		promptStyle = output.String().Foreground(output.Color("24")).Bold()
		toolStartStyle = output.String().Foreground(output.Color("66"))
		toolOkStyle = output.String().Foreground(output.Color("28")).Bold()
		toolErrStyle = output.String().Foreground(output.Color("124")).Bold()
		toolLabelStyle = output.String().Foreground(output.Color("240"))
		assistantOut = output.String().Foreground(output.Color("235"))
		barFieldStyle = output.String().Foreground(output.Color("240"))
		barActiveStyle = output.String().Foreground(output.Color("136")).Bold()
		barErrorStyle = output.String().Foreground(output.Color("124")).Bold()
	}
}

// promptYesNo prompts the user for a yes/no response
func promptYesNo(prompt string, defaultValue bool) bool {
	return promptYesNoWithReader(prompt, defaultValue, bufio.NewReader(os.Stdin))
}

func promptYesNoWithReader(prompt string, defaultValue bool, reader *bufio.Reader) bool {
	var promptStr string
	if defaultValue {
		promptStr = fmt.Sprintf("%s (Y/n): ", prompt)
	} else {
		promptStr = fmt.Sprintf("%s (y/N): ", prompt)
	}

	fmt.Fprint(os.Stderr, promptStr)
	response, err := readLine(reader)
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	// Return default if user just presses enter
	if response == "" {
		return defaultValue
	}
	return response == "y" || response == "yes"
}

// promptYesNoAll prompts for y/n/a (yes, no, approve all). Returns 'y', 'n', or 'a'.
func promptYesNoAll(prompt string) byte {
	return promptYesNoAllWithReader(prompt, bufio.NewReader(os.Stdin))
}

func promptYesNoAllWithReader(prompt string, reader *bufio.Reader) byte {
	fmt.Fprintf(os.Stderr, "%s (Y/n/a): ", prompt)
	response, err := readLine(reader)
	if err != nil {
		return 'n'
	}
	response = strings.TrimSpace(strings.ToLower(response))
	switch response {
	case "", "y", "yes":
		return 'y'
	case "a", "all":
		return 'a'
	default:
		return 'n'
	}
}

// toolApprover manages tool call approval state across a session.
type toolApprover struct {
	approveAll bool
	reader     *bufio.Reader
}

func newToolApprover(reader *bufio.Reader) *toolApprover {
	return &toolApprover{reader: reader}
}

// approveToolCalls prompts the user to approve each tool in a batch.
func (ta *toolApprover) approveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	approved := make([]bool, len(calls))
	if ta.approveAll {
		for i := range approved {
			approved[i] = true
		}
		return approved
	}

	for i, tc := range calls {
		summary := summarizeToolArgs(tc.Name, tc.Arguments)
		label := tc.Name
		if summary != "" {
			label += ": " + truncate(summary, 80)
		}
		fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled(label))

		switch promptYesNoAllWithReader("  allow?", ta.reader) {
		case 'y':
			approved[i] = true
		case 'a':
			ta.approveAll = true
			for j := i; j < len(calls); j++ {
				approved[j] = true
			}
			return approved
		default:
			approved[i] = false
		}
	}
	return approved
}

// isTerminal checks if output is going to a terminal
func isTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err == nil {
		return strings.TrimRight(line, "\r\n"), nil
	}
	if err == io.EOF && line != "" {
		return strings.TrimRight(line, "\r\n"), nil
	}
	return "", err
}
