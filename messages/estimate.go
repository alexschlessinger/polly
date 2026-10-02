package messages

// EstimatedStringTokens rates prose at the conventional 4 bytes per token,
// rounding up so a partial trailing token still counts.
func EstimatedStringTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// EstimatedJSONTokens rates dense JSON at 3 bytes per token; the prose
// heuristic's 4 bytes per token systematically undercounts it.
func EstimatedJSONTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 2) / 3
}

// EstimatedImageTokens is the flat per-image cost estimate. Polly caps
// uploads at 1568px on the long edge, and provider vision billing on an
// image that size commonly lands near 2000 tokens.
const EstimatedImageTokens = 2000

// EstimateMessageTokens estimates the provider-visible token cost of a single
// message with the heuristic compaction uses where no provider count covers a
// request: a small per-message base plus the string fields, text and image
// parts, and tool calls. Artifact-backed parts add nothing: requests carry
// their receipts, not their stored bytes.
func EstimateMessageTokens(msg ChatMessage) int {
	text := msg.Content
	if msg.HasTextBlocks() {
		text = msg.ModelText()
	}
	total := 4 + EstimatedStringTokens(text) + EstimatedStringTokens(msg.Reasoning) + EstimatedStringTokens(msg.ToolCallID)
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			total += EstimatedStringTokens(part.Text)
		case "image_base64", "image_url":
			total += EstimatedImageTokens
		}
	}
	for _, call := range msg.ToolCalls {
		total += EstimatedStringTokens(call.Name) + EstimatedJSONTokens(call.Arguments)
	}
	return total
}
