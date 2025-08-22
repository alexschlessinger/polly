package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

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
	var promptStr string
	if defaultValue {
		promptStr = fmt.Sprintf("%s (Y/n): ", prompt)
	} else {
		promptStr = fmt.Sprintf("%s (y/N): ", prompt)
	}

	fmt.Fprint(os.Stderr, promptStr)
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
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

// isTerminal checks if output is going to a terminal
func isTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}
