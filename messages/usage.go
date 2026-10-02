package messages

// UsageRecord keeps a response's reported usage as internal application
// state, without any content or provider replay metadata. Missing counts
// stay missing, so consumers can distinguish unknown usage from zero.
func (m ChatMessage) UsageRecord() ChatMessage {
	record := ChatMessage{Role: MessageRoleInternal, Metadata: map[string]any{MetadataKeyUsageOnly: true}}
	for _, key := range []string{
		MetadataKeyInputTokens, MetadataKeyOutputTokens,
		MetadataKeyCacheReadInputTokens, MetadataKeyCacheWriteInputTokens, MetadataKeyCostUSD,
	} {
		if value, ok := m.Metadata[key]; ok {
			record.Metadata[key] = value
		}
	}
	return record
}

// ReportsUsage reports whether m accounts for a model response's usage: an
// assistant message, or a usage record.
func (m ChatMessage) ReportsUsage() bool {
	return m.Role == MessageRoleAssistant || m.IsUsageRecord()
}

// IsUsageRecord reports whether this internal message accounts for a model
// response whose content is not retained in the conversation.
func (m ChatMessage) IsUsageRecord() bool {
	only, _ := m.Metadata[MetadataKeyUsageOnly].(bool)
	return m.Role == MessageRoleInternal && only
}
