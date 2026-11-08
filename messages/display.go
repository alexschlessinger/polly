package messages

// IsAgentSynthetic recognizes authenticated coordination admissions, including
// legacy messages written before the explicit synthetic flag. Envelope text is
// deliberately not an identity signal.
func (m ChatMessage) IsAgentSynthetic() bool {
	if synthetic, _ := m.Metadata[MetadataKeyAgentSynthetic].(bool); synthetic {
		return true
	}
	switch ids := m.Metadata[MetadataKeySwarmMessages].(type) {
	case []string:
		for _, id := range ids {
			if id == "" {
				return false
			}
		}
		return len(ids) > 0
	case []any:
		for _, value := range ids {
			if id, ok := value.(string); !ok || id == "" {
				return false
			}
		}
		return len(ids) > 0
	}
	return false
}
