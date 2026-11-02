package gemini

import (
	"encoding/base64"

	"github.com/alexschlessinger/pollytool/messages"
)

// ReplayThoughtSignature restores the signature replayed with an assistant
// tool call. In-process signatures are map[string]string; a JSON session reload
// produces map[string]any. Invalid encodings are not replayed.
func ReplayThoughtSignature(msg messages.ChatMessage, callID string) []byte {
	if msg.Role != messages.MessageRoleAssistant {
		return nil
	}
	var encoded string
	switch signatures := msg.Metadata[ThoughtSignaturesKey].(type) {
	case map[string]string:
		encoded = signatures[callID]
	case map[string]any:
		encoded, _ = signatures[callID].(string)
	}
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return signature
}
