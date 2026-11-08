package sessions

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// displayFact consumes a JSON-decoded persisted message, so metadata has the
// same types as ordinary transcript reads. Never retain arbitrary tool_data:
// command output and file diffs can be much larger than the visible window.
func displayFact(msg messages.ChatMessage) (TranscriptDisplayFact, bool) {
	out := messages.ChatMessage{Role: msg.Role, ToolCallID: msg.ToolCallID, ToolName: msg.ToolName, ToolCalls: msg.ToolCalls, Metadata: map[string]any{}}
	for _, key := range []string{messages.MetadataKeyDisplayID, messages.MetadataKeyDisplayToolCalls, messages.MetadataKeyDisplayAgentLaunch, messages.MetadataKeyToolSucceeded, messages.MetadataKeyIsError, messages.MetadataKeyToolMillis, messages.MetadataKeyAgentSynthetic, messages.MetadataKeySwarmMessages} {
		if value, ok := msg.Metadata[key]; ok {
			out.Metadata[key] = value
		}
	}
	if msg.Reasoning != "" {
		out.Reasoning = "x"
	}
	if text, _ := msg.Metadata[messages.MetadataKeyDisplayReasoning].(string); text != "" {
		out.Metadata[messages.MetadataKeyDisplayReasoning] = "x"
	}
	if msg.Role == messages.MessageRoleAssistant {
		if msg.GetContent() != "" {
			out.Content = "x"
		} else if msg.ModelText() != "" {
			out.TextBlocks = []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "x"}}
		}
	}
	if msg.Role == messages.MessageRoleTool && strings.TrimSpace(msg.Content) != "" {
		out.Content = "x"
	}
	for _, part := range msg.Parts {
		if part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage || part.Type == "image_base64" && part.ImageData != "" {
			out.Parts = append(out.Parts, part)
		}
	}
	if data, ok := msg.Metadata["tool_data"].(map[string]any); ok {
		compact := map[string]any{}
		if code, ok := data["exitCode"]; ok {
			compact["exitCode"] = code
		}
		if nested, ok := data["changes"].(map[string]any); ok {
			compact["changes"] = displayChanges(nested)
		} else if _, ok := data["tracked"]; ok {
			compact = displayChanges(data)
		}
		out.Metadata["tool_data"] = compact
	}
	fact := TranscriptDisplayFact{Message: out}
	if msg.Role == messages.MessageRoleTool {
		fact.ContentHash = fmt.Sprintf("%x", sha256.Sum256([]byte(msg.Content)))
	}
	_, launch := out.Metadata[messages.MetadataKeyDisplayAgentLaunch]
	_, calls := out.Metadata[messages.MetadataKeyDisplayToolCalls]
	_, thought := out.Metadata[messages.MetadataKeyDisplayReasoning]
	relevant := len(out.Parts) > 0 || len(out.ToolCalls) > 0 || out.ToolCallID != "" || msg.Role == messages.MessageRoleTool || out.Reasoning != "" || launch || calls || thought
	return fact, relevant
}

func displayChanges(data map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"root", "tracked", "reason", "truncated", "omitted"} {
		if value, ok := data[key]; ok {
			out[key] = value
		}
	}
	if list, ok := data["changes"].([]any); ok {
		var changes []any
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			change := map[string]any{}
			for _, key := range []string{"path", "kind", "additions", "deletions", "truncated", "binary", "counts_unknown"} {
				if value, ok := entry[key]; ok {
					change[key] = value
				}
			}
			changes = append(changes, change)
		}
		out["changes"] = changes
	}
	return out
}

func displayUser(msg messages.ChatMessage) messages.ChatMessage {
	out := messages.ChatMessage{Role: msg.Role, Content: msg.Content, Metadata: map[string]any{}}
	for _, key := range []string{"polly_composer_v1", messages.MetadataKeyContextImport, messages.MetadataKeyAgentReport} {
		if value, ok := msg.Metadata[key]; ok {
			out.Metadata[key] = value
		}
	}
	for _, part := range msg.Parts {
		p := messages.ContentPart{Type: part.Type, FileName: part.FileName}
		if part.Type == "text" && part.FileName == "" {
			p.Text = part.Text
		}
		out.Parts = append(out.Parts, p)
	}
	return out
}
