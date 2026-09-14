package openrouter

import (
	"context"
	"net/http"
	"strconv"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// ListModels reads the OpenRouter catalog, or one model's endpoints (its
// routes, with per-route capabilities and prices) when t.Model is set. An
// endpoint response omits model-wide reasoning policy; the router merges it
// with the catalog record.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	if t.Model == "" {
		return openai.ListCompatibleModels(ctx, client, t, decodeModel, false)
	}
	raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, t.BaseURL+"/models/"+catalog.EscapeModelPath(t.Model)+"/endpoints", nil, catalog.Bearer(t.APIKey))
	if err != nil {
		return contract.ModelCatalog{}, err
	}
	data := catalog.Obj(raw["data"])
	if data == nil {
		return contract.ModelCatalog{}, contract.ErrModelMetadataUnknown
	}
	info := decodeModel(data)
	if info.ID == "" {
		info.ID = t.Model
	}
	endpoints := catalog.Array(data["endpoints"])
	info.EndpointsComplete = endpoints != nil
	for _, value := range endpoints {
		info.Endpoints = append(info.Endpoints, decodeEndpoint(catalog.Obj(value)))
	}
	return contract.ModelCatalog{Models: []contract.ModelInfo{info}, Partial: !info.EndpointsComplete}, nil
}

func decodeModel(r map[string]any) contract.ModelInfo {
	info := catalog.RoutedModel(r)
	decodeReasoningPolicy(&info.ModelCapabilities, catalog.Obj(r["reasoning"]))
	info.PricingUnit = "USD per token"
	info.Prices = catalog.Prices(info.Pricing, r, priceUnit)
	return info
}

// decodeReasoningPolicy reads the gateway's model-wide reasoning object. An
// explicit null supported_efforts means unrestricted and is still complete.
func decodeReasoningPolicy(c *contract.ModelCapabilities, policy map[string]any) {
	if policy == nil {
		return
	}
	c.ReasoningPolicy = true
	c.ReasoningMandatory = catalog.BoolPtr(policy["mandatory"])
	c.ReasoningDefaultEnabled = catalog.BoolPtr(policy["default_enabled"])
	c.ReasoningMaxTokens = catalog.BoolPtr(policy["supports_max_tokens"])
	if effort, ok := policy["default_effort"].(string); ok {
		c.ReasoningDefaultEffort = &effort
	}
	if value, present := policy["supported_efforts"]; present {
		efforts := catalog.Strings(policy, "supported_efforts")
		if value == nil || efforts != nil {
			c.ReasoningEfforts = efforts
			c.ReasoningEffortsComplete = true
		}
	}
}

func decodeEndpoint(r map[string]any) contract.ModelEndpointInfo {
	e := catalog.Endpoint(r)
	e.ID = catalog.Str(r["tag"])
	e.Name = catalog.Str(r["provider_name"])
	e.PricingUnit = "USD per token"
	if status, ok := r["status"].(float64); ok {
		e.Status = strconv.Itoa(int(status))
	}
	e.InputTokens = catalog.IntPtr(r["max_prompt_tokens"])
	e.OutputTokens = catalog.IntPtr(r["max_completion_tokens"])
	catalog.SupportedParameters(&e.ModelCapabilities, r)
	decodeReasoningPolicy(&e.ModelCapabilities, catalog.Obj(r["reasoning"]))
	e.Prices = catalog.Prices(e.Pricing, r, priceUnit)
	return e
}

func priceUnit(item string) string {
	switch item {
	case "prompt", "completion", "input_cache_read", "input_cache_write":
		return "token"
	case "request":
		return "request"
	case "image":
		return "image"
	}
	return ""
}
