package anthropic

import (
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

// ReplayThinkingBlocks restores the signed or redacted thinking blocks that
// requests replay from an assistant message. Plain or unsigned thinking is not
// replayable.
func ReplayThinkingBlocks(msg messages.ChatMessage) []*ContentBlock {
	if msg.Role != messages.MessageRoleAssistant {
		return nil
	}
	var blocks []*ContentBlock
	for _, block := range contract.MetadataMapList(msg.Metadata[ThinkingBlocksKey]) {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "thinking":
			thinking, _ := block["thinking"].(string)
			signature, _ := block["signature"].(string)
			if signature != "" && thinking != "" {
				blocks = append(blocks, &ContentBlock{
					Type: "thinking", Thinking: thinking, Signature: signature,
				})
			}
		case "redacted_thinking":
			if data, _ := block["data"].(string); data != "" {
				blocks = append(blocks, &ContentBlock{Type: "redacted_thinking", Data: data})
			}
		}
	}
	return blocks
}
