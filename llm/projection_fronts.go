package llm

import (
	"hash/fnv"
	"io"

	"github.com/alexschlessinger/pollytool/messages"
)

// projectionFronts is how far a projection has compacted and omitted, as
// counts of leading real user exchanges, with a fingerprint of the message
// structure those counts index. Every advance rewrites the provider-visible
// prefix, so a front never retreats: not within a run, where the budget the
// calibration sizes by each provider count widens again after a compacted
// request counted lower, and not across runs whose histories extend the last
// one's.
type projectionFronts struct {
	// compacted is the number of leading exchanges whose tool results
	// demote; omitted is the number omitted outright.
	compacted, omitted int
	// shape fingerprints the history, as the run holds it, up to the further
	// front's user message: the roles and tool call ids the counts index,
	// and what the user said. A history that no longer matches it starts
	// from no fronts.
	shape uint64
}

// end is the number of exchanges the fronts index.
func (f projectionFronts) end() int { return max(f.compacted, f.omitted) }

// indexes reports whether f still indexes projected, whose real user
// messages are at users.
func (f projectionFronts) indexes(projected []messages.ChatMessage, users []int) bool {
	end := f.end()
	if end == 0 {
		return true
	}
	return end < len(users) && frontShape(projected[:users[end]]) == f.shape
}

// advance moves f to compacted exchanges demoted and omitted exchanges
// omitted, never back, and fingerprints the prefix of history it then
// indexes. history is the run's, not the projection's, whose rewrites the
// next run's check does not see.
func (f *projectionFronts) advance(history []messages.ChatMessage, users []int, compacted, omitted int) {
	if compacted <= f.compacted && omitted <= f.omitted {
		return
	}
	f.compacted = max(f.compacted, compacted)
	f.omitted = max(f.omitted, omitted)
	f.shape = frontShape(history[:users[f.end()]])
}

// heldFronts is where a projection of projected starts from: the cache's
// fronts, checked once per run against the history they index. The history
// is append-only from then on.
func (c *projectionCache) heldFronts(projected []messages.ChatMessage, users []int) projectionFronts {
	if !c.frontsChecked {
		c.frontsChecked = true
		if !c.fronts.indexes(projected, users) {
			c.fronts = projectionFronts{}
		}
	}
	return c.fronts
}

// frontShape fingerprints the structure of a history prefix: the roles and
// tool call ids in order, and the real user messages' text, which tells one
// conversation of a given shape from another. Other content is left out:
// durable spills rewrite tool results in place.
func frontShape(prefix []messages.ChatMessage) uint64 {
	h := fnv.New64a()
	for _, msg := range prefix {
		io.WriteString(h, msg.Role)
		h.Write([]byte{0})
		io.WriteString(h, msg.ToolCallID)
		for _, call := range msg.ToolCalls {
			h.Write([]byte{0})
			io.WriteString(h, call.ID)
		}
		if isRealUser(msg) {
			h.Write([]byte{0})
			io.WriteString(h, msg.GetContent())
		}
		h.Write([]byte{1})
	}
	return h.Sum64()
}
