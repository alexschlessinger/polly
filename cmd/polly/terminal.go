package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"golang.org/x/term"
)

var terminalFD = term.IsTerminal

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
	if response == "" {
		return defaultValue
	}
	return response == "y" || response == "yes"
}

func promptYesNoAllWithReader(out io.Writer, prompt string, reader *bufio.Reader) byte {
	fmt.Fprintf(out, "%s (Y/n/a): ", prompt)
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

// The prompt shares stderr with the status line, so out is the same stream
// the line UI writes to; tests capture both to check their ordering.
type toolApprover struct {
	approveAll bool
	reader     *bufio.Reader
	out        io.Writer
}

func newToolApprover(reader *bufio.Reader) *toolApprover {
	return &toolApprover{reader: reader, out: os.Stderr}
}

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
		fmt.Fprintf(ta.out, "  %s\n", label)

		switch promptYesNoAllWithReader(ta.out, "  allow?", ta.reader) {
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

func isTerminal() bool {
	return terminalFD(int(os.Stdout.Fd())) && terminalFD(int(os.Stderr.Fd()))
}

func canPromptOnStdin() bool {
	return terminalFD(int(os.Stdin.Fd())) && terminalFD(int(os.Stderr.Fd()))
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
