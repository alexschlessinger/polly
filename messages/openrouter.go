package messages

import (
	"bytes"
	"encoding/json"
)

// UnmarshalJSON keeps opaque numbers in newly attributed OpenRouter replay
// blocks exact across SQLite and JSON round-trips. All other message fields,
// metadata namespaces, and historical unattributed data use the usual decoder.
// Marshaling and the stored schema are unchanged.
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	type plain ChatMessage
	if err := json.Unmarshal(data, (*plain)(m)); err != nil {
		return err
	}
	meta, ok := m.Metadata["openrouter"].(map[string]any)
	if !ok {
		return nil
	}
	endpoint, _ := meta["endpoint"].(string)
	model, _ := meta["requested_model"].(string)
	if endpoint == "" || model == "" {
		return nil
	}
	var envelope struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	raw, present := envelope.Metadata["openrouter"]
	if !present {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var exact map[string]any
	if err := decoder.Decode(&exact); err != nil {
		return err
	}
	m.Metadata["openrouter"] = exact
	return nil
}
