package main

import (
	"os"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"golang.org/x/term"
)

type outputSurface uint8

const (
	outputSurfaceLineRaw outputSurface = iota
	outputSurfaceLineANSI
	outputSurfaceManagedTUI
)

// outputCapabilities describes only how this process can present assistant
// output. The assistant's persisted Markdown source is independent of this
// display choice.
type outputCapabilities struct {
	surface       outputSurface
	imageProtocol termimg.Protocol
	columns       int
	noColor       bool
	// truecolor and color256 are the SGR color depth of the surface. They stay
	// advisory on the raw and managed-TUI surfaces, which never emit
	// renderer-owned SGR. Emission branches only on truecolor: 38;5;N degrades
	// safely on a terminal that does not advertise the 256 palette, while 38;2
	// needs the truecolor probe.
	truecolor bool
	color256  bool
}

// lineColorCapabilities is the emission decision for one line-frontend surface:
// whether SGR may be written at all, and how deep the color may go. The line
// writers take this instead of reading the environment a second time.
type lineColorCapabilities struct {
	enabled   bool
	truecolor bool
	color256  bool
}

// lineColors derives the line-frontend emission decision: raw output (piped or
// TERM=dumb) and NO_COLOR never carry SGR, whatever the terminal advertises.
func (c outputCapabilities) lineColors() lineColorCapabilities {
	return lineColorCapabilities{
		enabled:   c.rendersLineANSI() && !c.noColor,
		truecolor: c.truecolor,
		color256:  c.color256,
	}
}

// detectColorDepth mirrors tcell's own probe (tscreen.go newTerminfoScreen):
// COLORTERM advertises the capability directly and the terminfo name carries it
// as a suffix. A truecolor terminal is always 256-color capable.
func detectColorDepth(getenv func(string) string) (truecolor, color256 bool) {
	colorTerm := strings.TrimSpace(getenv("COLORTERM"))
	termName := strings.TrimSpace(getenv("TERM"))
	if slices.Contains([]string{"truecolor", "direct", "24bit"}, colorTerm) ||
		strings.HasSuffix(termName, "-direct") || strings.HasSuffix(termName, "-truecolor") {
		return true, true
	}
	if strings.HasSuffix(termName, "-256color") || strings.Contains(colorTerm, "256") {
		return false, true
	}
	return false, false
}

func (c outputCapabilities) rendersMarkdown() bool {
	return c.surface == outputSurfaceLineANSI || c.surface == outputSurfaceManagedTUI
}

func (c outputCapabilities) rendersLineANSI() bool {
	return c.surface == outputSurfaceLineANSI
}

func (c outputCapabilities) interpretsLocalImages() bool {
	return c.rendersMarkdown()
}

func outputCapabilitiesForRun(mode conversationMode, managedREPL bool) outputCapabilities {
	stdoutFD := int(os.Stdout.Fd())
	columns := 80
	if width, _, err := term.GetSize(stdoutFD); err == nil && width > 0 {
		columns = width
	}
	return resolveOutputCapabilities(mode, managedREPL, terminalFD(stdoutFD), columns, os.Getenv)
}

func resolveOutputCapabilities(
	mode conversationMode,
	managedREPL bool,
	stdoutTTY bool,
	columns int,
	getenv func(string) string,
) outputCapabilities {
	if columns <= 0 {
		columns = 80
	}
	truecolor, color256 := detectColorDepth(getenv)
	if mode == conversationModeREPL && managedREPL {
		return outputCapabilities{
			surface:       outputSurfaceManagedTUI,
			imageProtocol: termimg.DetectProtocol(getenv),
			columns:       columns,
			truecolor:     truecolor,
			color256:      color256,
		}
	}

	termName := strings.TrimSpace(getenv("TERM"))
	if !stdoutTTY || strings.EqualFold(termName, "dumb") {
		return outputCapabilities{surface: outputSurfaceLineRaw, columns: columns, truecolor: truecolor, color256: color256}
	}

	caps := outputCapabilities{
		surface:       outputSurfaceLineANSI,
		imageProtocol: termimg.DetectProtocol(getenv),
		columns:       columns,
		noColor:       getenv("NO_COLOR") != "",
		truecolor:     truecolor,
		color256:      color256,
	}
	if caps.noColor {
		caps.imageProtocol = termimg.ProtocolNone
	}
	return caps
}
