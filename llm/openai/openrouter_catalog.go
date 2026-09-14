package openai

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ListOpenRouterModels reads the OpenRouter catalog, or one model's endpoints
// (its routes, with per-route capabilities and prices) when t.Model is set.
// An endpoint response omits model-wide reasoning policy; the router merges
// it with the catalog record.
func ListOpenRouterModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	if t.Model == "" {
		return ListCompatibleModels(ctx, client, t, decodeOpenRouterModel, false)
	}
	raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, t.BaseURL+"/models/"+catalog.EscapeModelPath(t.Model)+"/endpoints", nil, catalog.Bearer(t.APIKey))
	if err != nil {
		return contract.ModelCatalog{}, err
	}
	data := catalog.Obj(raw["data"])
	if data == nil {
		return contract.ModelCatalog{}, contract.ErrModelMetadataUnknown
	}
	info := decodeOpenRouterModel(data)
	if info.ID == "" {
		info.ID = t.Model
	}
	endpoints := catalog.Array(data["endpoints"])
	info.EndpointsComplete = endpoints != nil
	for _, value := range endpoints {
		info.Endpoints = append(info.Endpoints, decodeOpenRouterEndpoint(catalog.Obj(value)))
	}
	return contract.ModelCatalog{Models: []contract.ModelInfo{info}, Partial: !info.EndpointsComplete}, nil
}

// ListHuggingFaceModels reads the Hugging Face router catalog, whose records
// carry each serving provider as an endpoint, or one model's record when
// t.Model is set.
func ListHuggingFaceModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	return ListCompatibleModels(ctx, client, t, decodeHuggingFaceModel, true)
}

// decodeRoutedModel decodes what OpenRouter and Hugging Face records share
// for a model served by several hosts.
func decodeRoutedModel(r map[string]any) contract.ModelInfo {
	info := catalog.BaseModel(r)
	info.Routed = true
	info.Chat = catalog.Truth(info.OutputModalities == nil || slices.Contains(info.OutputModalities, "text"))
	info.Pricing = catalog.Obj(r["pricing"])
	decodeParameters(&info.ModelCapabilities, r)
	info.OutputTokens = catalog.IntPtr(catalog.Obj(r["top_provider"])["max_completion_tokens"])
	return info
}

func decodeOpenRouterModel(r map[string]any) contract.ModelInfo {
	info := decodeRoutedModel(r)
	info.PricingUnit = "USD per token"
	info.Prices = prices(info.Pricing, r, openRouterPriceUnit)
	return info
}

func decodeHuggingFaceModel(r map[string]any) contract.ModelInfo {
	info := decodeRoutedModel(r)
	info.PricingUnit = "USD per million tokens"
	providers := catalog.Array(r["providers"])
	info.EndpointsComplete = providers != nil
	for _, value := range providers {
		info.Endpoints = append(info.Endpoints, decodeHuggingFaceEndpoint(catalog.Obj(value)))
	}
	info.Prices = prices(info.Pricing, r, huggingFacePriceUnit)
	return info
}

// decodeParameters reads the supported_parameters list both routers
// advertise, deriving tool, structured-output and reasoning support from it,
// then the gateway's reasoning policy.
func decodeParameters(c *contract.ModelCapabilities, r map[string]any) {
	if p := catalog.Strings(r, "supported_parameters"); p != nil {
		c.Parameters = map[string]bool{}
		c.ParametersComplete = true
		for _, s := range p {
			c.Parameters[s] = true
		}
		c.Tools = catalog.Truth(slices.Contains(p, "tools"))
		c.StructuredOutput = catalog.Truth(slices.Contains(p, "structured_outputs") || slices.Contains(p, "response_format"))
		c.Reasoning = catalog.Truth(slices.Contains(p, "reasoning") || slices.Contains(p, "reasoning_effort") || slices.Contains(p, "include_reasoning"))
	}
	decodeReasoningPolicy(c, catalog.Obj(r["reasoning"]))
}

// decodeReasoningPolicy reads OpenRouter's model-wide reasoning object. An
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

// decodeEndpoint decodes the route record shape both routers share.
func decodeEndpoint(r map[string]any) contract.ModelEndpointInfo {
	e := contract.ModelEndpointInfo{ID: catalog.Str(r["provider"]), Name: catalog.Str(r["provider"]), Status: catalog.Str(r["status"]), Raw: catalog.BoundedRaw(r), Pricing: catalog.Obj(r["pricing"]), Performance: map[string]any{}}
	e.ContextTokens = catalog.IntPtr(r["context_length"])
	e.Tools = catalog.BoolPtr(r["supports_tools"])
	e.StructuredOutput = catalog.BoolPtr(r["supports_structured_output"])
	e.InputModalities = catalog.Strings(catalog.Obj(r["architecture"]), "input_modalities")
	for _, k := range []string{"first_token_latency_ms", "throughput", "latency_last_30m", "throughput_last_30m", "uptime_last_30m", "uptime_last_1d"} {
		if v, ok := r[k]; ok {
			e.Performance[k] = v
		}
	}
	return e
}

func decodeHuggingFaceEndpoint(r map[string]any) contract.ModelEndpointInfo {
	e := decodeEndpoint(r)
	e.PricingUnit = "USD per million tokens"
	e.Prices = prices(e.Pricing, r, huggingFacePriceUnit)
	return e
}

func decodeOpenRouterEndpoint(r map[string]any) contract.ModelEndpointInfo {
	e := decodeEndpoint(r)
	e.ID = catalog.Str(r["tag"])
	e.Name = catalog.Str(r["provider_name"])
	e.PricingUnit = "USD per token"
	if status, ok := r["status"].(float64); ok {
		e.Status = strconv.Itoa(int(status))
	}
	e.InputTokens = catalog.IntPtr(r["max_prompt_tokens"])
	e.OutputTokens = catalog.IntPtr(r["max_completion_tokens"])
	decodeParameters(&e.ModelCapabilities, r)
	e.Prices = prices(e.Pricing, r, openRouterPriceUnit)
	return e
}

func openRouterPriceUnit(item string) string {
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

func huggingFacePriceUnit(item string) string {
	if item == "input" || item == "output" {
		return "million tokens"
	}
	return ""
}

// prices normalizes a pricing object into sorted prices, keeping the
// record's free and conditional pricing hints.
func prices(pricing map[string]any, record map[string]any, unit func(item string) string) []contract.ModelPrice {
	var out []contract.ModelPrice
	for item, amount := range pricing {
		out = append(out, contract.ModelPrice{Item: item, Amount: amount, Currency: "USD", Unit: unit(item), Conditions: catalog.AdvertisedFields(record, "is_free", "pricing_conditions")})
	}
	slices.SortFunc(out, func(a, b contract.ModelPrice) int { return strings.Compare(a.Item, b.Item) })
	return out
}
