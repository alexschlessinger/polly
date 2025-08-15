package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pkdindustries/pollytool/llm"
	"golang.org/x/term"
)

// outputStructured formats and outputs structured response
func outputStructured(content string, schema *llm.Schema) {
	// If content is already JSON, pretty-print it
	var data any
	if err := json.Unmarshal([]byte(content), &data); err == nil {
		// Validate against schema if provided
		if schema != nil {
			if err := validateJSONAgainstSchema(data, schema); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Output doesn't match schema: %v\n", err)
			}
		}

		jsonBytes, _ := json.MarshalIndent(data, "", "  ")
		fmt.Println(string(jsonBytes))
	} else {
		// Fallback to raw output if not valid JSON
		fmt.Println(content)
	}
}

// outputText adds a newline at the end for non-JSON output
func outputText() {
	fmt.Println()
}

// isTerminal checks if output is going to a terminal
func isTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// createStatusLine creates a status line if appropriate
func createStatusLine(config *Config) *Status {
	// Use terminal title for status updates when in a terminal
	// Status line works fine with schema since it outputs to stderr
	if !config.Quiet && isTerminal() {
		return NewStatus()
	}

	// Return nil when status updates are not appropriate
	return nil
}
