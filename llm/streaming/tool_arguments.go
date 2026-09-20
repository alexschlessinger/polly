package streaming

import "strings"

// Each call keeps a separate buffer because providers may interleave argument
// deltas. Pointers keep nonempty Builders stationary when the map grows.
type ToolArgumentBuffers map[int]*strings.Builder

func (buffers *ToolArgumentBuffers) Append(index int, current, delta string) string {
	if *buffers == nil {
		*buffers = make(ToolArgumentBuffers)
	}
	builder := (*buffers)[index]
	if builder == nil {
		builder = &strings.Builder{}
		(*buffers)[index] = builder
	}
	// "{}" is the state API's placeholder for an unstarted argument stream.
	if current == "{}" {
		current = ""
	}
	// A completed Responses event can replace the accumulated arguments, so
	// reseeding the buffer from current is what makes the next delta correct.
	return appendStreamText(builder, current, delta)
}
