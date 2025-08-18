package llm

import "strings"

// ThinkBlockFilter filters out <think>...</think> blocks from streaming content
type ThinkBlockFilter struct {
	inThinkBlock bool
	buffer       strings.Builder
	thinkDepth   int
}

// ProcessChunk processes a chunk of streaming content, filtering think blocks
func (f *ThinkBlockFilter) ProcessChunk(chunk string) (filtered string, isThinking bool) {
	var output strings.Builder

	for _, char := range chunk {
		f.buffer.WriteRune(char)
		bufStr := f.buffer.String()

		// Check for <think> opening tag
		if !f.inThinkBlock && strings.HasSuffix(bufStr, "<think>") {
			f.inThinkBlock = true
			f.thinkDepth++
			// Remove the <think> tag from output
			if f.buffer.Len() > 7 {
				output.WriteString(bufStr[:len(bufStr)-7])
			}
			f.buffer.Reset()
		} else if f.inThinkBlock && strings.HasSuffix(bufStr, "</think>") {
			// Check for </think> closing tag
			f.thinkDepth--
			if f.thinkDepth <= 0 {
				f.inThinkBlock = false
				f.thinkDepth = 0
			}
			f.buffer.Reset()
		} else if !f.inThinkBlock && (char == '<' || f.buffer.Len() > 10) {
			// Not in think block and either starting a new potential tag or buffer is getting long
			if char != '<' {
				// Flush buffer if it's not a tag start
				output.WriteString(bufStr)
				f.buffer.Reset()
			}
		}
	}

	// If we're not in a think block and buffer doesn't look like a partial tag, flush it
	if !f.inThinkBlock && f.buffer.Len() > 0 && !strings.HasPrefix(f.buffer.String(), "<") {
		output.WriteString(f.buffer.String())
		f.buffer.Reset()
	}

	return output.String(), f.inThinkBlock
}