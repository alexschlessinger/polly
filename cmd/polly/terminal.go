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

	// Style helpers - initialized in initColors()
	highlightStyle termenv.Style
	errorStyle     termenv.Style
	successStyle   termenv.Style
	dimStyle       termenv.Style
	boldStyle      termenv.Style
	userStyle      termenv.Style
	assistantStyle termenv.Style
	systemStyle    termenv.Style
)

// initColors initializes color styles based on terminal background
func initColors() {
	if termenv.HasDarkBackground() {
		// Dark background - use lighter/brighter colors
		highlightStyle = output.String().Foreground(output.Color("179")).Bold() // Muted yellow
		errorStyle = output.String().Foreground(output.Color("124"))            // Muted red
		successStyle = output.String().Foreground(output.Color("65"))           // Muted green
		dimStyle = output.String().Faint()                                      // Dimmed text
		boldStyle = output.String().Bold()                                      // Bold text
		userStyle = output.String().Foreground(output.Color("32")).Bold()       // Muted blue for user
		assistantStyle = output.String().Foreground(output.Color("141"))        // Muted purple for assistant
		systemStyle = output.String().Foreground(output.Color("244"))           // Gray for system
	} else {
		// Light background - use darker/more saturated colors
		highlightStyle = output.String().Foreground(output.Color("136")).Bold() // Dark orange/brown
		errorStyle = output.String().Foreground(output.Color("160"))            // Dark red
		successStyle = output.String().Foreground(output.Color("28"))           // Dark green
		dimStyle = output.String().Foreground(output.Color("240"))              // Dark gray
		boldStyle = output.String().Bold()                                      // Bold text
		userStyle = output.String().Foreground(output.Color("26")).Bold()       // Dark blue for user
		assistantStyle = output.String().Foreground(output.Color("90"))         // Dark purple for assistant
		systemStyle = output.String().Foreground(output.Color("238"))           // Darker gray for system
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
