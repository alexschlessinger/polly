package messages

// MetadataKeyCompaction marks an internal message as a context compaction:
// durable state that changes what later requests carry of the history before
// it, without rewriting that history.
const MetadataKeyCompaction = "compaction"

// Compaction is what a compaction marker records. A marker with a Summary
// replaces the conversation before it with the summary; with KeepsTurn, the
// turn in progress (from its last real user message on) stays verbatim after
// the summary. A marker with ClearThrough clears tool results up to and
// including the result of that tool call, which the marker follows, to short
// notes. The history itself stays whole, so the transcript keeps every
// message the markers leave out of requests.
type Compaction struct {
	Summary      string
	KeepsTurn    bool
	ClearThrough string
}

// Message is the internal message that records c.
func (c Compaction) Message() ChatMessage {
	value := map[string]any{}
	if c.Summary != "" {
		value["summary"] = c.Summary
	}
	if c.KeepsTurn {
		value["keeps_turn"] = true
	}
	if c.ClearThrough != "" {
		value["clear_through"] = c.ClearThrough
	}
	return ChatMessage{Role: MessageRoleInternal, Metadata: map[string]any{MetadataKeyCompaction: value}}
}

// Compaction returns what m records when it is a compaction marker.
func (m ChatMessage) Compaction() (Compaction, bool) {
	if m.Role != MessageRoleInternal {
		return Compaction{}, false
	}
	value, ok := m.Metadata[MetadataKeyCompaction].(map[string]any)
	if !ok {
		return Compaction{}, false
	}
	var c Compaction
	c.Summary, _ = value["summary"].(string)
	c.KeepsTurn, _ = value["keeps_turn"].(bool)
	c.ClearThrough, _ = value["clear_through"].(string)
	return c, c.Summary != "" || c.ClearThrough != ""
}
