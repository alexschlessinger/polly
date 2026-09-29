package contract

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// Complete collects a prepared request, retaining metadata and partial text on error.
func Complete(ctx context.Context, client LLM, req *CompletionRequest) (*messages.ChatMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := client.ChatCompletionStream(ctx, req, messages.NewStreamProcessor())
	var content, reasoning strings.Builder
	var blocks []messages.AssistantText
	var texts []*strings.Builder
	indexes := map[string]int{}
	addText := func(event *messages.StreamEvent) {
		if event.Text == nil {
			return
		}
		block := *event.Text
		i, ok := indexes[block.ID]
		if !ok {
			i = len(blocks)
			indexes[block.ID] = i
			blocks = append(blocks, block)
			texts = append(texts, new(strings.Builder))
		}
		texts[i].WriteString(block.Text)
	}
	partial := func() *messages.ChatMessage {
		if content.Len() == 0 && reasoning.Len() == 0 && len(blocks) == 0 {
			return nil
		}
		for i := range blocks {
			blocks[i].Text = texts[i].String()
		}
		return &messages.ChatMessage{TextBlocks: blocks, Role: messages.MessageRoleAssistant, Content: content.String(), Reasoning: reasoning.String()}
	}
	for {
		select {
		case <-ctx.Done():
			return partial(), ctx.Err()
		case event, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return partial(), err
				}
				return partial(), fmt.Errorf("no final response from LLM")
			}
			switch event.Type {
			case messages.EventTypeCommentary:
				addText(event)
			case messages.EventTypeContent:
				addText(event)
				content.WriteString(event.Content)
			case messages.EventTypeReasoning:
				reasoning.WriteString(event.Content)
			case messages.EventTypeComplete:
				if err := ctx.Err(); err != nil {
					return partial(), err
				}
				if event.Message == nil {
					return partial(), fmt.Errorf("completion event has no message")
				}
				return event.Message, nil
			case messages.EventTypeError:
				return partial(), event.Error
			}
		}
	}
}
