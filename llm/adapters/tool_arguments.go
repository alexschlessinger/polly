package adapters

import "strings"

// Each call keeps a separate buffer because providers may interleave argument
// deltas. Pointers keep nonempty Builders stationary when the map grows.
type toolArgumentBuffers map[int]*strings.Builder

func (buffers *toolArgumentBuffers) append(index int, current, delta string) string {
	if *buffers == nil {
		*buffers = make(toolArgumentBuffers)
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
	// Completed Responses events can replace the accumulated arguments. Seed
	// from the current state if a subsequent delta follows that replacement.
	if current != builder.String() {
		builder.Reset()
		builder.WriteString(current)
	}
	builder.WriteString(delta)
	return builder.String()
}
