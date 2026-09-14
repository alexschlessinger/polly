package catalog

import (
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// RoutedModel decodes what gateway catalogs (OpenRouter, Hugging Face) share
// for a model served by several hosts: the routed flag, chat support from
// the output modalities, the pricing object, the supported parameter list,
// and the top provider's completion limit.
func RoutedModel(r map[string]any) contract.ModelInfo {
	info := BaseModel(r)
	info.Routed = true
	info.Chat = Truth(info.OutputModalities == nil || slices.Contains(info.OutputModalities, "text"))
	info.Pricing = Obj(r["pricing"])
	SupportedParameters(&info.ModelCapabilities, r)
	info.OutputTokens = IntPtr(Obj(r["top_provider"])["max_completion_tokens"])
	return info
}

// SupportedParameters reads a gateway's supported_parameters list, deriving
// tool, structured-output and reasoning support from it.
func SupportedParameters(c *contract.ModelCapabilities, r map[string]any) {
	p := Strings(r, "supported_parameters")
	if p == nil {
		return
	}
	c.Parameters = map[string]bool{}
	c.ParametersComplete = true
	for _, s := range p {
		c.Parameters[s] = true
	}
	c.Tools = Truth(slices.Contains(p, "tools"))
	c.StructuredOutput = Truth(slices.Contains(p, "structured_outputs") || slices.Contains(p, "response_format"))
	c.Reasoning = Truth(slices.Contains(p, "reasoning") || slices.Contains(p, "reasoning_effort") || slices.Contains(p, "include_reasoning"))
}

// Endpoint decodes the route record shape gateways share: the serving
// provider, its status, limits, capabilities, pricing and performance keys.
func Endpoint(r map[string]any) contract.ModelEndpointInfo {
	e := contract.ModelEndpointInfo{ID: Str(r["provider"]), Name: Str(r["provider"]), Status: Str(r["status"]), Raw: BoundedRaw(r), Pricing: Obj(r["pricing"]), Performance: map[string]any{}}
	e.ContextTokens = IntPtr(r["context_length"])
	e.Tools = BoolPtr(r["supports_tools"])
	e.StructuredOutput = BoolPtr(r["supports_structured_output"])
	e.InputModalities = Strings(Obj(r["architecture"]), "input_modalities")
	for _, k := range []string{"first_token_latency_ms", "throughput", "latency_last_30m", "throughput_last_30m", "uptime_last_30m", "uptime_last_1d"} {
		if v, ok := r[k]; ok {
			e.Performance[k] = v
		}
	}
	return e
}

// Prices normalizes a pricing object into sorted prices with the unit each
// item is billed in, keeping the record's free and conditional pricing hints.
func Prices(pricing map[string]any, record map[string]any, unit func(item string) string) []contract.ModelPrice {
	var out []contract.ModelPrice
	for item, amount := range pricing {
		out = append(out, contract.ModelPrice{Item: item, Amount: amount, Currency: "USD", Unit: unit(item), Conditions: AdvertisedFields(record, "is_free", "pricing_conditions")})
	}
	slices.SortFunc(out, func(a, b contract.ModelPrice) int { return strings.Compare(a.Item, b.Item) })
	return out
}
