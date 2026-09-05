package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// readTranscriptTool pages and searches the run's durable conversation record
// — the history the caller supplied plus everything generated so far — so
// content the projection has omitted or demoted stays reachable. Its results
// are recall results: reproducible on demand, so the projection elides them
// once their exchange completes.
type readTranscriptTool struct {
	tools.NativeTool
	snapshot func() []messages.ChatMessage
	rendered func() string
}

func (t *readTranscriptTool) text() string {
	if t.rendered != nil {
		return t.rendered()
	}
	return renderTranscript(t.snapshot())
}

func (t *readTranscriptTool) GetName() string { return "read_transcript" }

func (t *readTranscriptTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"read_transcript",
		"Page or search the durable conversation transcript as numbered lines or raw byte windows, including exchanges omitted from the visible context and the full content of demoted tool results.",
		schema.Params{
			"offset":      schema.Int("1-based starting line (default 1)"),
			"limit":       schema.Int("Maximum lines or matches (default 200, maximum 500)"),
			"query":       schema.S("Optional case-sensitive literal search"),
			"byte_offset": schema.Int("0-based byte position; returns a raw byte window instead of numbered lines (for transcripts with very long lines)"),
		},
	)
}

func (t *readTranscriptTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	args := tools.Args(raw)
	if _, hasByteOffset := raw["byte_offset"]; hasByteOffset {
		for _, key := range []string{"offset", "limit", "query"} {
			if _, conflict := raw[key]; conflict {
				return "", fmt.Errorf("byte_offset cannot be combined with offset, limit, or query")
			}
		}
		byteOffset := int64(args.Int("byte_offset", -1))
		if byteOffset < 0 {
			return "", fmt.Errorf("byte_offset must be at least 0")
		}
		rendered := t.text()
		if byteOffset >= int64(len(rendered)) {
			return fmt.Sprintf("Transcript has no content at or after byte %d.", byteOffset), nil
		}
		return tools.PageByteWindow(ctx, strings.NewReader(rendered), "transcript", "conversation", int64(len(rendered)), byteOffset)
	}
	offset := args.Int("offset", 1)
	if offset < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	limit := args.Int("limit", artifactReadDefaultLines)
	if limit < 1 || limit > artifactReadMaxLines {
		return "", fmt.Errorf("limit must be between 1 and %d", artifactReadMaxLines)
	}
	rendered := t.text()
	text, err := tools.PageLines(ctx, strings.NewReader(rendered), "transcript", offset, limit, args.String("query"))
	if err != nil {
		return "", err
	}
	return tools.CapPageText(text), nil
}

// renderTranscript is the durable transcript's canonical text form. Message
// indexes count non-internal messages only and the transcript is append-only,
// so a line found in one call pages the same in the next. Recall-tool results
// are elided from the rendering: they are reproducible by calling the tool
// again, and inlining them would nest prior read_transcript output inside
// later ones.
func renderTranscript(history []messages.ChatMessage) string {
	var b strings.Builder
	appendTranscriptText(&b, history, 0)
	return b.String()
}

func appendTranscriptText(b *strings.Builder, history []messages.ChatMessage, index int) int {
	for _, msg := range history {
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		index++
		b.WriteString(fmt.Sprintf("=== message %d: %s", index, msg.Role))
		if msg.ToolName != "" {
			b.WriteString(" " + msg.ToolName)
		}
		b.WriteString(" ===\n")
		if msg.Role == messages.MessageRoleTool && isRecallToolName(msg.ToolName) {
			b.WriteString("[" + msg.ToolName + " result not rendered; reproducible by calling it again]\n")
			continue
		}
		if content := msg.GetContent(); content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteByte('\n')
			}
		}
		for _, call := range msg.ToolCalls {
			b.WriteString(fmt.Sprintf("[tool call %s %s]\n", call.Name, call.Arguments))
		}
	}
	return index
}
