package llm

import (
	"context"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

// readTranscriptTool pages and searches the run's durable conversation record
// — the history the caller supplied plus everything generated so far — so
// content compaction left out of requests stays reachable. Its results are
// recall results: reproducible on demand, so compaction clears them to a stub.
type readTranscriptTool struct {
	tools.NativeTool
	open func(context.Context) *transcriptReader
}

func (t *readTranscriptTool) GetName() string { return "read_transcript" }
func (t *readTranscriptTool) RecallStub() string {
	return "[read_transcript result elided to save space; the call above shows its arguments. Call read_transcript again to re-read.]"
}

func (t *readTranscriptTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"read_transcript",
		"Page or search the durable conversation transcript as numbered lines or raw byte windows, including the conversation compaction summarized and the full content of cleared tool results.",
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
		r := t.open(ctx)
		defer r.Close()
		size, err := r.size()
		if err != nil {
			return "", err
		}
		if byteOffset >= size {
			return fmt.Sprintf("Transcript has no content at or after byte %d.", byteOffset), nil
		}
		return tools.PageByteWindow(ctx, r, "transcript", "conversation", size, byteOffset)
	}
	offset := args.Int("offset", 1)
	if offset < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	limit := args.Int("limit", artifactReadDefaultLines)
	if limit < 1 || limit > artifactReadMaxLines {
		return "", fmt.Errorf("limit must be between 1 and %d", artifactReadMaxLines)
	}
	r := t.open(ctx)
	defer r.Close()
	text, err := tools.PageLines(ctx, r, "transcript", offset, limit, args.String("query"))
	if err != nil {
		return "", err
	}
	return tools.CapPageText(ctx, text), nil
}

// transcriptReader borrows the authoritative history until Close. It retains
// only the current string fragment, never a rendered transcript or a copied
// message slice. The pager owns its bounded line/window buffers. Searches and
// deep offsets can still traverse history; byte pages also count its size.
type transcriptReader struct {
	ctx     context.Context
	history []messages.ChatMessage
	recall  recallStubs
	next    func() (string, bool)
	stop    func()
	pending string
	release func()
}

func newTranscriptReader(ctx context.Context, history []messages.ChatMessage, recall recallStubs) *transcriptReader {
	return &transcriptReader{ctx: ctx, history: history, recall: recall}
}

func (r *transcriptReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.next == nil {
		r.next, r.stop = iter.Pull(transcriptFragments(r.ctx, r.history, r.recall))
	}
	for r.pending == "" {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		var ok bool
		r.pending, ok = r.next()
		if !ok {
			if err := r.ctx.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *transcriptReader) Close() {
	if r.stop != nil {
		r.stop()
	}
	if r.release != nil {
		r.release()
		r.release = nil
	}
	r.history, r.recall, r.pending = nil, nil, ""
	r.next, r.stop = nil, nil
}

// size counts canonical bytes without joining content, even for a message with
// many parts or phased text blocks. It deliberately has no retained index to
// invalidate on replacement, withdrawal or changes to registered recall tools.
func (r *transcriptReader) size() (int64, error) {
	var size int64
	index := 0
	for _, msg := range r.history {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		index++
		digits := 1
		for n := index; n >= 10; n /= 10 {
			digits++
		}
		size += int64(len("=== message :  ===\n") + digits + len(msg.Role))
		if msg.ToolName != "" {
			size += int64(1 + len(msg.ToolName))
		}
		if _, recall := r.recall.stub(msg.ToolName); msg.Role == messages.MessageRoleTool && recall {
			size += int64(len("[ result not rendered; reproducible by calling it again]\n") + len(msg.ToolName))
			continue
		}
		last := ""
		for text := range transcriptModelText(msg) {
			if err := r.ctx.Err(); err != nil {
				return 0, err
			}
			size += int64(len(text))
			last = text
		}
		if last != "" && !strings.HasSuffix(last, "\n") {
			size++
		}
		for _, call := range msg.ToolCalls {
			size += int64(len("[tool call  ]\n") + len(call.Name) + len(call.Arguments))
		}
	}
	return size, nil
}

func transcriptFragments(ctx context.Context, history []messages.ChatMessage, recall recallStubs) iter.Seq[string] {
	return func(yield func(string) bool) {
		index := 0
		for _, msg := range history {
			if ctx.Err() != nil {
				return
			}
			if msg.Role == messages.MessageRoleInternal {
				continue
			}
			index++
			if !yield("=== message ") || !yield(strconv.Itoa(index)) || !yield(": ") || !yield(msg.Role) {
				return
			}
			if msg.ToolName != "" && (!yield(" ") || !yield(msg.ToolName)) {
				return
			}
			if !yield(" ===\n") {
				return
			}
			if _, isRecall := recall.stub(msg.ToolName); msg.Role == messages.MessageRoleTool && isRecall {
				if !yield("[") || !yield(msg.ToolName) || !yield(" result not rendered; reproducible by calling it again]\n") {
					return
				}
				continue
			}
			last := ""
			for text := range transcriptModelText(msg) {
				if !yield(text) {
					return
				}
				last = text
			}
			if last != "" && !strings.HasSuffix(last, "\n") && !yield("\n") {
				return
			}
			for _, call := range msg.ToolCalls {
				if !yield("[tool call ") || !yield(call.Name) || !yield(" ") || !yield(call.Arguments) || !yield("]\n") {
					return
				}
			}
		}
	}
}

// transcriptModelText streams ModelText's exact projection without joining
// parts/blocks. In particular, stale blocks must not override edited Content.
func transcriptModelText(msg messages.ChatMessage) iter.Seq[string] {
	return func(yield func(string) bool) {
		if transcriptHasTextBlocks(msg) {
			first := true
			for _, block := range msg.TextBlocks {
				if block.Text == "" {
					continue
				}
				if !first && !yield("\n\n") {
					return
				}
				if !yield(block.Text) {
					return
				}
				first = false
			}
		} else if msg.Content != "" {
			yield(msg.Content)
		} else {
			for _, part := range msg.Parts {
				if part.Type == "text" && part.Text != "" && !yield(part.Text) {
					return
				}
			}
		}
	}
}

// Equivalent to HasTextBlocks, but compare the answer projection in place
// instead of allocating another potentially oversized answer string.
func transcriptHasTextBlocks(msg messages.ChatMessage) bool {
	if msg.Role != messages.MessageRoleAssistant || len(msg.TextBlocks) == 0 {
		return false
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text", "image_url", "image_base64", "image_artifact":
			return false
		}
	}
	remaining, first := msg.Content, true
	for _, block := range msg.TextBlocks {
		if block.Phase == messages.PhaseCommentary || block.Text == "" {
			continue
		}
		if !first {
			if !strings.HasPrefix(remaining, "\n\n") {
				return false
			}
			remaining = remaining[2:]
		}
		if !strings.HasPrefix(remaining, block.Text) {
			return false
		}
		remaining = remaining[len(block.Text):]
		first = false
	}
	return remaining == ""
}

// renderTranscript is the durable transcript's canonical text form. Message
// indexes count non-internal messages only and the transcript is append-only,
// so a line found in one call pages the same in the next. Recall-tool results
// are elided from the rendering: they are reproducible by calling the tool
// again, and inlining them would nest prior read_transcript output inside
// later ones.
func renderTranscript(history []messages.ChatMessage, recall recallStubs) string {
	var b strings.Builder
	index := 0
	for _, msg := range history {
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		index++
		fmt.Fprintf(&b, "=== message %d: %s", index, msg.Role)
		if msg.ToolName != "" {
			b.WriteString(" " + msg.ToolName)
		}
		b.WriteString(" ===\n")
		if _, isRecall := recall.stub(msg.ToolName); msg.Role == messages.MessageRoleTool && isRecall {
			b.WriteString("[" + msg.ToolName + " result not rendered; reproducible by calling it again]\n")
			continue
		}
		if content := msg.ModelText(); content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteByte('\n')
			}
		}
		for _, call := range msg.ToolCalls {
			fmt.Fprintf(&b, "[tool call %s %s]\n", call.Name, call.Arguments)
		}
	}
	return b.String()
}
