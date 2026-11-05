package openai

import (
	"log/slog"
	"strings"
)

// responsesReasoningStream selects the first nonempty representation of each
// item. Some hosts send both raw reasoning and a summary: switching after text
// was displayed would duplicate or rewrite it. Parts of the selected form are
// accumulated separately so done/final snapshots can fill gaps without replay.
type responsesReasoningStream struct {
	items map[int]*responseReasoningItem
	byID  map[string]*responseReasoningItem
	last  *strings.Builder
	emit  func(string)
}

type responseReasoningItem struct {
	id    string
	form  string
	parts map[int]*strings.Builder
}

func newResponsesReasoningStream(emit func(string)) *responsesReasoningStream {
	return &responsesReasoningStream{
		items: map[int]*responseReasoningItem{},
		byID:  map[string]*responseReasoningItem{},
		emit:  emit,
	}
}

func (r *responsesReasoningStream) item(index int, id string) *responseReasoningItem {
	if id != "" && r.byID[id] != nil {
		return r.byID[id]
	}
	item := r.items[index]
	if item == nil || (id != "" && item.id != "" && item.id != id) {
		item = &responseReasoningItem{parts: map[int]*strings.Builder{}}
		r.items[index] = item
	}
	if id != "" {
		item.id = id
		r.byID[id] = item
	}
	return item
}

func (r *responsesReasoningStream) text(item *responseReasoningItem, form string, index int, text string, snapshot bool) {
	if text == "" || (item.form != "" && item.form != form) {
		return
	}
	item.form = form
	part := item.parts[index]
	if part == nil {
		part = new(strings.Builder)
		item.parts[index] = part
	}
	if snapshot {
		prior := part.String()
		switch {
		case strings.HasPrefix(text, prior):
			text = text[len(prior):]
		case strings.HasPrefix(prior, text):
			return // An older snapshot must not shorten the streamed text.
		default:
			// Display is append-only. Never concatenate a revised snapshot to
			// an incompatible prefix, or log the potentially sensitive text.
			slog.Warn("openai_reasoning_snapshot_mismatch", "item_id", item.id, "form", form, "part_index", index)
			return
		}
	}
	if text == "" {
		return
	}
	if r.last != nil && r.last != part {
		// A late suffix for an earlier part cannot be inserted into the
		// append-only display. Keep it separate from the intervening part.
		r.emit("\n\n")
	}
	r.emit(text)
	part.WriteString(text)
	r.last = part
}

func (r *responsesReasoningStream) snapshot(index int, item *ResponseOutputItem) {
	if item == nil || item.Type != "reasoning" {
		return
	}
	state := r.item(index, item.ID)
	// With no earlier text, prefer a nonempty summary. Empty summary parts
	// don't hide raw content, and don't create blank display paragraphs.
	for i, part := range item.Summary {
		r.text(state, "summary", i, part.Text, true)
	}
	for i, part := range item.Content {
		r.text(state, "content", i, part.Text, true)
	}
}

func (r *responsesReasoningStream) process(event *ResponseStreamEvent) {
	switch event.Type {
	case "response.reasoning_summary_text.delta":
		r.text(r.item(int(event.OutputIndex), event.ItemID), "summary", int(event.SummaryIndex), string(event.Delta), false)
	case "response.reasoning_summary_text.done":
		r.text(r.item(int(event.OutputIndex), event.ItemID), "summary", int(event.SummaryIndex), event.Text, true)
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if event.Part != nil {
			r.text(r.item(int(event.OutputIndex), event.ItemID), "summary", int(event.SummaryIndex), event.Part.Text, true)
		}
	case "response.reasoning_text.delta":
		r.text(r.item(int(event.OutputIndex), event.ItemID), "content", int(event.ContentIndex), string(event.Delta), false)
	case "response.reasoning_text.done":
		r.text(r.item(int(event.OutputIndex), event.ItemID), "content", int(event.ContentIndex), event.Text, true)
	case "response.content_part.added", "response.content_part.done":
		if event.Part != nil && event.Part.Type == "reasoning_text" {
			r.text(r.item(int(event.OutputIndex), event.ItemID), "content", int(event.ContentIndex), event.Part.Text, true)
		}
	case "response.output_item.added", "response.output_item.done":
		r.snapshot(int(event.OutputIndex), event.Item)
	case "response.completed", "response.incomplete", "response.failed":
		if event.Response != nil {
			for i := range event.Response.Output {
				r.snapshot(i, &event.Response.Output[i])
			}
		}
	}
}
