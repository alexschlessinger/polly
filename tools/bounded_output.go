package tools

import (
	"bytes"
	"fmt"
	"strings"
)

// capturedOutputLimit bounds how much of a command's stdout or stderr is held
// in memory while it runs. The agent spills large results to artifacts only
// after the command exits, so without a bound a runaway producer such as
// `yes` exhausts memory before any timeout fires. Output past the limit is
// discarded — the pipe stays drained so the process never blocks on it — and
// the truncation is reported in the result.
const capturedOutputLimit = 4 << 20

// boundedBuffer keeps the first limit bytes written to it and counts the rest.
type boundedBuffer struct {
	limit   int
	buf     bytes.Buffer
	dropped int64
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		b.buf.Write(p[:room])
	}
	b.dropped += int64(len(p) - room)
	return len(p), nil
}

// Len reports how many bytes were kept.
func (b *boundedBuffer) Len() int { return b.buf.Len() }

// Truncated reports whether any bytes were dropped.
func (b *boundedBuffer) Truncated() bool { return b.dropped > 0 }

// String returns the kept output, followed by a truncation notice when bytes
// were dropped.
func (b *boundedBuffer) String() string {
	out := b.buf.String()
	if b.dropped == 0 {
		return out
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + fmt.Sprintf("[output truncated: %d more bytes were dropped after the %d byte capture limit]", b.dropped, b.limit)
}
