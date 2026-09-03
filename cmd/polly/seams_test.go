package main

// Test seams: default-argument wrappers and transcript oracles that only tests
// call. They live here so production carries no surface reachable only from
// tests, which keeps `deadcode ./cmd/polly` a meaningful signal.

import (
	"bufio"
	"context"
	"io"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

var slashCommands = defaultReplCommands.commandNames()

func renderMarkdown(src string) string {
	return renderMarkdownDocument(src, nil)
}

func sandboxRegistryOptions(config *Config) ([]tools.RegistryOption, error) {
	return sandboxRegistryOptionsWithWarnings(config, newBroadWritablePathWarner())
}

func signalExitCode(err error) (int, bool) {
	code, _, ok := splitSignalError(err)
	return code, ok
}

func preparedMessageTranscriptImages(msg messages.ChatMessage) []transcriptImage {
	return preparedMessageTranscriptImagesWithStore(msg, nil)
}

func (m *replModel) appendHelp() {
	for _, line := range defaultReplCommands.helpLines() {
		m.appendNoticeLine(line)
	}
}

func completeSlash(input string) (completed string, matches []string, ok bool) {
	return defaultReplCommands.complete(input, nil)
}

func (m *replModel) turnTrailerDetailText(dock turnDockState, width int) string {
	text, _ := m.turnTrailerDetail(dock, width)
	return text
}

func textManagedTurn(prompt string) managedTurnInput {
	return managedTurnInput{
		displayText: prompt,
		userMessage: messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt},
	}
}

func (m *replModel) appendToolStartLine(id, label string) {
	record := m.appendToolStartRow(id, label)
	m.refreshToolDisclosure(record)
}

func (m *replModel) beginTurn(prompt string) {
	m.beginManagedTurn(textManagedTurn(prompt))
}

func (m *replModel) renderInput() (text string, rows, curRow, curCol int, editable bool) {
	return m.renderInputWithMaxRows(maxInputRows)
}

// renderInputWithMaxRows also reports the row count the region occupies,
// derived as frameLayoutFor derives it, for the layout assertions.
func (m *replModel) renderInputWithMaxRows(maxRows int) (text string, rows, curRow, curCol int, editable bool) {
	text, curRow, curCol, editable = m.renderInputForTerminal(maxRows, 0)
	return text, min(max(maxRows, 1), m.inputRows()), curRow, curCol, editable
}

// inputDisplay returns just the input region's text, for assertions on the
// busy/approval overlays.
func (m *replModel) inputDisplay() string {
	text, _, _, _, _ := m.renderInput()
	return text
}

func (r *managedREPL) startTurn(ctx context.Context, prompt string, runTurn func(context.Context, string, TurnUI) error) chan error {
	return r.startManagedTurn(ctx, textManagedTurn(prompt), runTurn)
}

// transcriptTexts returns the transcript entries' text, for the tests that
// assert against the joined transcript.
func transcriptTexts(m *replModel) []string {
	out := make([]string, len(m.transcript))
	for i, entry := range m.transcript {
		out[i] = entry.text
	}
	return out
}

// flattenTranscript expands embedded "\n" within entries into separate lines,
// the logical-line view tests assert against.
func (m *replModel) flattenTranscript() []string {
	out := make([]string, 0, len(m.transcript))
	for i, entry := range m.transcript {
		e := entry.text
		if i == m.currentAssistant {
			// A provider often streams its final newline as its own chunk. Keep it
			// provisional until more text arrives so completion does not create and
			// then remove a visible blank row.
			e = strings.TrimRight(e, "\r\n")
			if e == "" {
				continue
			}
		}
		if strings.Contains(e, "\n") {
			out = append(out, strings.Split(e, "\n")...)
		} else {
			out = append(out, e)
		}
	}
	return out
}

// visibleTranscript returns the slice of transcript lines that would fill the
// pane, honoring scroll state on logical lines.
func (m *replModel) visibleTranscript(maxLines int) string {
	lines := m.flattenTranscript()
	if m.slashHints != "" {
		withHints := make([]string, 0, len(lines)+1)
		withHints = append(withHints, lines...)
		withHints = append(withHints, styled(m.slashHints, "muted", ""))
		lines = withHints
	}
	total := len(lines)
	if total == 0 {
		return ""
	}

	if m.followBottom {
		if total <= maxLines {
			return strings.Join(lines, "\n")
		}
		return strings.Join(lines[total-maxLines:], "\n")
	}

	top := m.scrollAnchor
	if top < 0 {
		top = 0
	}
	if top+maxLines >= total {
		// User scrolled all the way down; re-engage follow.
		m.followBottom = true
		top = total - maxLines
		if top < 0 {
			top = 0
		}
	}
	end := top + maxLines
	if end > total {
		end = total
	}
	return strings.Join(lines[top:end], "\n")
}

// fullTranscript returns every semantic block plus transient slash hints,
// joined the way the pre-cache renderer saw them.
func (m *replModel) fullTranscript() string {
	lines := m.flattenTranscript()
	if m.slashHints != "" {
		lines = append(append([]string(nil), lines...), styled(m.slashHints, "muted", ""))
	}
	return strings.Join(lines, "\n")
}

func (m *replModel) transcriptDisplayBlocks() []string {
	entries := m.transcriptDisplayEntries(m.disclosureLayoutWidth(0))
	blocks := make([]string, len(entries))
	for i, entry := range entries {
		blocks[i] = entry.text
	}
	return blocks
}

func transcriptBlockRows(text string, followed bool, width int) [][]ui.Cell {
	rows, _ := transcriptBlockRowsWithImages(text, followed, width, nil, false, 0, 0)
	return rows
}

func (m *replModel) scrollBy(delta, viewportHeight int) {
	width, _ := ui.TerminalDimensions()
	if width < 1 {
		width = 80
	}
	m.scrollByWidth(delta, viewportHeight, width)
}

func runREPLLoop(ctx context.Context, reader *bufio.Reader, promptWriter io.Writer, runTurn func(string) error) error {
	return runREPLLoopWithCommands(ctx, reader, promptWriter, newWriterReplCommandContext(nil, nil, promptWriter), runTurn)
}

func newLineTurnUI(config *Config, inputReader *bufio.Reader) *lineTurnUI {
	return newLineTurnUIWithCapabilities(
		config,
		inputReader,
		outputCapabilities{surface: outputSurfaceLineRaw, columns: 80},
	)
}

// fullViewport is the window that shows every transcript row at width, the
// common case for placement assertions.
func fullViewport(totalRows, width int) transcriptViewport {
	return frameLayout{width: width, transcriptHeight: totalRows}.transcriptViewport(totalRows, 0, false, 0)
}
