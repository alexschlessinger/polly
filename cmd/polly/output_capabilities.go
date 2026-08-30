package main

import (
	"os"
	"strings"

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
	imageProtocol terminalImageProtocol
	columns       int
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
	if mode == conversationModeREPL && managedREPL {
		return outputCapabilities{
			surface:       outputSurfaceManagedTUI,
			imageProtocol: detectTerminalImageProtocol(getenv),
			columns:       columns,
		}
	}

	termName := strings.TrimSpace(getenv("TERM"))
	if !stdoutTTY || strings.EqualFold(termName, "dumb") || getenv("NO_COLOR") != "" {
		return outputCapabilities{surface: outputSurfaceLineRaw, columns: columns}
	}

	return outputCapabilities{
		surface:       outputSurfaceLineANSI,
		imageProtocol: detectTerminalImageProtocol(getenv),
		columns:       columns,
	}
}
