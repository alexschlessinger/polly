package llm

import (
	"github.com/alexschlessinger/pollytool/llm/internal/contract"

	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// Prepare adapts a request to its model before it is sent. It resolves the
// model's capabilities through client unless the request already carries
// them, applies PrepareCapabilities, records the capabilities on the copy so
// providers resolve reasoning from the same facts, and injects the skill
// prompt. Agent.Run prepares every iteration; callers that stream through
// MultiPass directly prepare once themselves. The caller's request is never
// modified.
func Prepare(ctx context.Context, client LLM, req *CompletionRequest, requireTools bool) (*CompletionRequest, []RequestAdaptation, error) {
	out := *req
	var notes []RequestAdaptation
	if caps := resolveRequestCapabilities(ctx, client, req); caps != nil {
		prepared, adapted, err := PrepareCapabilities(req, *caps, requireTools)
		if err != nil {
			return nil, nil, err
		}
		prepared.Capabilities = caps
		out, notes = *prepared, adapted
	}
	if out.Skills != nil && !out.Skills.IsEmpty() {
		out.Messages = out.ResolvedMessages()
		out.Skills = nil
	}
	return &out, notes, nil
}

// PrepareCapabilities copies a request and omits only explicitly unsupported
// optional features. Durable messages, tools and caller settings are untouched.
func PrepareCapabilities(req *CompletionRequest, c ModelCapabilities, requireTools bool) (*CompletionRequest, []RequestAdaptation, error) {
	out := *req
	out.MaxContextTokens = ClampContextBudget(req.MaxContextTokens, c.ContextWindow(), req.MaxTokens)
	unsupportedTools := c.Tools != nil && !*c.Tools
	if unsupportedTools && requireTools {
		return nil, nil, fmt.Errorf("model %s does not support required tool calling", req.Model)
	}
	// Anthropic implements response schemas with a tool, independently of
	// native structured-output support advertised by model discovery.
	schemaTool := strings.HasPrefix(strings.ToLower(req.Model), "anthropic/") && !unsupportedTools
	if c.StructuredOutput != nil && !*c.StructuredOutput && req.ResponseSchema != nil && !schemaTool {
		return nil, nil, fmt.Errorf("model %s does not support the requested structured output", req.Model)
	}
	var notes []RequestAdaptation
	add := func(feature string, count int, description string) {
		notes = append(notes, RequestAdaptation{Feature: feature, Count: count, Message: description})
	}
	toolExchanges := 0
	noImages := omitsImages(c)
	if noImages || unsupportedTools {
		out.Messages = cloneMessages(req.Messages)
		images := 0
		for i := range out.Messages {
			m := &out.Messages[i]
			for j, p := range m.Parts {
				if noImages && isImagePart(p) {
					label := p.Reference
					if label == "" {
						label = p.FileName
					}
					if p.Artifact != nil {
						if p.Artifact.ImageToken != "" {
							label = p.Artifact.ImageToken
						} else if label == "" {
							label = p.Artifact.Name
						}
					}
					if label == "" {
						label = "image"
					}
					m.Parts[j] = messages.ContentPart{Type: "text", Text: fmt.Sprintf("[%s omitted: this model cannot view images.]", label)}
					images++
				}
			}
			if unsupportedTools {
				for _, call := range m.ToolCalls {
					toolExchanges++
					m.Parts = append(m.Parts, messages.ContentPart{Type: "text", Text: fmt.Sprintf("Tool call %s (%s): %s", call.ID, call.Name, call.Arguments)})
				}
				m.ToolCalls = nil
				if m.Role == messages.MessageRoleTool {
					m.Role = messages.MessageRoleUser
					if m.Metadata == nil {
						m.Metadata = map[string]any{}
					}
					m.Metadata[messages.MetadataKeyAgentSynthetic] = true
					m.Parts = append([]messages.ContentPart{{Type: "text", Text: "Result of tool call " + m.ToolCallID + " (" + m.ToolName + "):"}}, m.Parts...)
					m.ToolCallID = ""
					m.ToolName = ""
				}
				if len(m.Parts) > 0 {
					promoteMessageContentToTextPart(m)
				}
			}
		}
		if images > 0 {
			add("images", images, fmt.Sprintf("%d images omitted: %s cannot view images; originals retained", images, req.Model))
		}
	}
	if unsupportedTools && (len(out.Tools) > 0 || toolExchanges > 0) {
		add("tools", len(out.Tools)+toolExchanges, "Tool calling omitted; completed calls retained as text: unsupported by this model")
		out.Tools = nil
	}
	parameterUnsupported := func(k string) bool { v, ok := c.Parameters[k]; return (ok && !v) || (!ok && c.ParametersComplete) }
	if out.Temperature != nil && parameterUnsupported("temperature") {
		out.Temperature = nil
		add("temperature", 1, "Temperature omitted: unsupported by this model")
	}
	if req.IsOpenRouter() {
		if resolved := contract.ResolveOpenRouterRequestThinking(req.ThinkingEffort, c); resolved.Notice != "" {
			add("reasoning", 1, resolved.Notice)
		}
	} else if out.ThinkingEffort.IsEnabled() {
		unsupported := c.Reasoning != nil && !*c.Reasoning
		if c.ReasoningEffortsComplete && c.ReasoningEfforts != nil && out.ThinkingEffort.IsLevel() && !slices.Contains(c.ReasoningEfforts, out.ThinkingEffort.String()) {
			unsupported = true
		}
		if unsupported {
			out.ThinkingEffort = EffortOff()
			add("reasoning", 1, "Requested reasoning setting omitted: unsupported by this model")
		}
	}
	return &out, notes, nil
}

// omitsImages reports whether a model declares it cannot view images, in
// which case preparation replaces media with text and projection must not
// resolve image references.
func omitsImages(c ModelCapabilities) bool {
	return c.InputModalities != nil && !slices.Contains(c.InputModalities, "image")
}
func targetForRequest(req *CompletionRequest) ModelTarget {
	p, name, ok := strings.Cut(req.Model, "/")
	if !ok {
		name = req.Model
		p = ""
	}
	return ModelTarget{Provider: p, Model: name, BaseURL: requestBaseURL(p, req.BaseURL), APIKey: req.APIKey, Host: req.ModelHost}
}

// routeHost names the endpoint whose capabilities apply to a target: the
// explicit host, or for Hugging Face the ":provider" suffix of the model id.
func routeHost(t ModelTarget) string {
	if t.Provider == "huggingface" {
		if _, host, ok := strings.Cut(t.Model, ":"); ok {
			return host
		}
	}
	return t.Host
}

func resolveRequestCapabilities(ctx context.Context, client LLM, req *CompletionRequest) *ModelCapabilities {
	if caps := lookupRequestCapabilities(ctx, client, req); caps != nil {
		return caps
	}
	// Unknown OpenRouter policy still needs an explicit resolution and a
	// turn-scoped adaptation notice, including when metadata is offline.
	if req.IsOpenRouter() {
		return &ModelCapabilities{}
	}
	return nil
}

func lookupRequestCapabilities(ctx context.Context, client LLM, req *CompletionRequest) *ModelCapabilities {
	if req.Capabilities != nil {
		return req.Capabilities
	}
	provider, ok := client.(ModelMetadataProvider)
	if !ok {
		return nil
	}
	t := targetForRequest(req)
	info, err := provider.GetModelInfo(ctx, t)
	if err != nil || info == nil {
		return nil
	}
	c := info.EffectiveCapabilities(routeHost(t))
	return &c
}
