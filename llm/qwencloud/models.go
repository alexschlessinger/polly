package qwencloud

import "strings"

// These wire options follow QwenCloud's model-specific parameter support:
// https://docs.qwencloud.com/api-reference/chat/openai-chat
// Keep unknown models on max_tokens and omit the opt-in preservation flag.
func supportsMaxCompletionTokens(model string) bool {
	for _, family := range []string{
		"qwen3.7-max", "qwen3.8-max",
		"qwen3.5-plus", "qwen3.6-plus", "qwen3.7-plus", "qwen3.8-plus",
		"qwen3.5-flash", "qwen3.6-flash", "qwen3.7-flash", "qwen3.8-flash",
		"deepseek-v3", "deepseek-v4", "deepseek-r1",
	} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return true
		}
		// DeepSeek minor versions (v3.1, v3.2, v4.1) share this parameter.
		if strings.HasPrefix(family, "deepseek-v") && strings.HasPrefix(model, family+".") {
			return true
		}
	}
	return false
}

func supportsPreserveThinking(model string) bool {
	switch model {
	case "qwen3.8-max", "qwen3.8-max-0902", "qwen3.8-flash", "qwen3.8-omni-flash",
		"qwen3.7-max", "qwen3.7-max-2026-05-20", "qwen3.7-max-2026-06-08",
		"qwen3.6-max-preview",
		"qwen3.7-plus", "qwen3.7-plus-2026-05-26",
		"qwen3.6-plus", "qwen3.6-plus-2026-04-02",
		"qwen3.7-flash", "qwen3.7-flash-2026-07-15",
		"qwen3.6-flash", "qwen3.6-flash-2026-04-16",
		"kimi-k2.6", "kimi-k2.7-code", "kimi/kimi-k2.7-code-highspeed", "kimi/kimi-k2.7-code":
		return true
	}
	return false
}
