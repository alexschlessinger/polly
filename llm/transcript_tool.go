package llm

import (
	"context"
	"fmt"
	"io"
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
// only the current message's fragments, never a rendered transcript or a copied
// message slice. The pager owns its bounded line/window buffers. Searches and
// deep offsets can still traverse history; byte pages also count its size.
type transcriptReader struct {
	ctx     context.Context
	cursor  transcriptCursor
	pending []string // the current message's fragments not yet read
	release func()
}

func newTranscriptReader(ctx context.Context, history []messages.ChatMessage, recall recallStubs) *transcriptReader {
	return &transcriptReader{ctx: ctx, cursor: transcriptCursor{history: history, recall: recall}}
}

func (r *transcriptReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n := 0
	for n < len(p) {
		if len(r.pending) == 0 {
			if r.pending = r.cursor.next(); r.pending == nil {
				break
			}
		}
		copied := copy(p[n:], r.pending[0])
		n += copied
		if r.pending[0] = r.pending[0][copied:]; r.pending[0] == "" {
			r.pending = r.pending[1:]
		}
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (r *transcriptReader) Close() {
	if r.release != nil {
		r.release()
		r.release = nil
	}
}

// size counts the view's canonical bytes, wherever the reader is. It
// deliberately has no retained index to invalidate on replacement, withdrawal
// or changes to registered recall tools.
func (r *transcriptReader) size() (int64, error) {
	var size int64
	count := transcriptCursor{history: r.cursor.history, recall: r.cursor.recall, counting: true}
	for fragments := count.next(); fragments != nil; fragments = count.next() {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		for _, fragment := range fragments {
			size += int64(len(fragment))
		}
	}
	return size, nil
}

// transcriptCursor walks the durable transcript's canonical text form one
// message at a time. It is the only definition of that form: the reader, the
// byte count and renderTranscript all take its fragments, which reference
// message content instead of joining or copying it.
//
// Message indexes count non-internal messages only and the transcript is
// append-only, so a line found in one call pages the same in the next.
// Recall-tool results are elided: they are reproducible by calling the tool
// again, and inlining them would nest prior read_transcript output inside
// later ones.
type transcriptCursor struct {
	history []messages.ChatMessage
	recall  recallStubs
	pos     int      // the next message in history
	index   int      // the last rendered message's number
	buf     []string // fragment storage, reused for each message
	// counting is set by a caller that only adds up fragment lengths.
	counting bool
}

// next returns the next rendered message's fragments, which stay valid until
// the following call, or nil after the last message.
func (c *transcriptCursor) next() []string {
	for c.pos < len(c.history) {
		msg := &c.history[c.pos]
		c.pos++
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		c.index++
		f := append(c.buf[:0], "=== message ", c.number(), ": ", msg.Role)
		if msg.ToolName != "" {
			f = append(f, " ", msg.ToolName)
		}
		f = append(f, " ===\n")
		if _, isRecall := c.recall.stub(msg.ToolName); msg.Role == messages.MessageRoleTool && isRecall {
			f = append(f, "[", msg.ToolName, " result not rendered; reproducible by calling it again]\n")
		} else {
			text := len(f)
			f = appendModelText(f, msg)
			if len(f) > text && !strings.HasSuffix(f[len(f)-1], "\n") {
				f = append(f, "\n")
			}
			for _, call := range msg.ToolCalls {
				f = append(f, "[tool call ", call.Name, " ", call.Arguments, "]\n")
			}
		}
		c.buf = f
		return f
	}
	return nil
}

// number is the current message's number as text. A counting cursor takes a
// stand-in of the same length, so a byte count allocates nothing per message.
func (c *transcriptCursor) number() string {
	if !c.counting {
		return strconv.Itoa(c.index)
	}
	digits := 1
	for n := c.index; n >= 10; n /= 10 {
		digits++
	}
	return "00000000000000000000"[:digits]
}

// appendModelText appends the non-empty pieces of ModelText's projection
// without joining them.
func appendModelText(dst []string, msg *messages.ChatMessage) []string {
	switch {
	case msg.HasTextBlocks():
		first := true
		for _, block := range msg.TextBlocks {
			if block.Text == "" {
				continue
			}
			if !first {
				dst = append(dst, "\n\n")
			}
			dst = append(dst, block.Text)
			first = false
		}
	case msg.Content != "":
		dst = append(dst, msg.Content)
	default:
		for _, part := range msg.Parts {
			if part.Type == "text" && part.Text != "" {
				dst = append(dst, part.Text)
			}
		}
	}
	return dst
}

// renderTranscript is the canonical text form in one string.
func renderTranscript(history []messages.ChatMessage, recall recallStubs) string {
	var b strings.Builder
	cursor := transcriptCursor{history: history, recall: recall}
	for fragments := cursor.next(); fragments != nil; fragments = cursor.next() {
		for _, fragment := range fragments {
			b.WriteString(fragment)
		}
	}
	return b.String()
}
