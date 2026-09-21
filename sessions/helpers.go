package sessions

import (
	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// EstimateTokens returns the token count of a single message as it would be
// replayed to a provider, using the shared heuristics defined in the messages
// package: 1 token ≈ 4 characters of prose, 3 bytes per token of dense JSON
// tool arguments, and a flat per-image cost.
// Provider-reported counts are deliberately not used: input_tokens is
// cumulative (the entire request prompt), and output_tokens includes
// reasoning tokens that are not replayed from history, so both misstate the
// message's retained size.
func EstimateTokens(msg messages.ChatMessage) int {
	count := messages.EstimateMessageTokens(msg)
	// Unlike the projection's estimate, stored artifacts count here: the
	// session keeps their bytes even though a request replays receipts.
	for _, part := range msg.Parts {
		if part.Artifact == nil {
			continue
		}
		switch part.Artifact.Kind {
		case artifacts.KindImage:
			count += messages.EstimatedImageTokens
		case artifacts.KindText:
			// A text artifact replaces a tool result's externalized
			// content. On any other role it only references stored
			// content that is counted where it lives (the projection
			// records the artifacts it mints for older inline results on
			// the assistant reply), so it adds nothing here.
			if msg.Role == messages.MessageRoleTool {
				count += int(part.Artifact.Bytes / 4)
			}
		}
	}
	return count
}
