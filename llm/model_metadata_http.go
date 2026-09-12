package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type metadataFetcher func(context.Context, *http.Client, ModelTarget) (ModelCatalog, error)

func fetchProviderMetadata(ctx context.Context, client *http.Client, t ModelTarget) (ModelCatalog, error) {
	cat := ModelCatalog{}

	if t.Provider == "openrouter" && t.Model != "" {
		raw, err := readMetadataJSON(ctx, client, t, http.MethodGet, t.BaseURL+"/models/"+escapeModelPath(t.Model)+"/endpoints", nil)
		if err != nil {
			return cat, err
		}
		data := obj(raw["data"])
		if data == nil {
			return cat, ErrModelMetadataUnknown
		}
		info := decodeModel(t.Provider, data)
		if info.ID == "" {
			info.ID = t.Model
		}
		info.EndpointsComplete = array(data["endpoints"]) != nil
		for _, value := range array(data["endpoints"]) {
			info.Endpoints = append(info.Endpoints, decodeEndpoint(t.Provider, obj(value)))
		}
		cat.Models = []ModelInfo{info}
		cat.Partial = !info.EndpointsComplete
		return cat, nil
	}
	path := "/models"
	method := http.MethodGet
	var body any
	switch t.Provider {
	case "gemini":
		path = "/models"
	case "ollama":
		path = "/api/tags"
		if t.Model != "" {
			path = "/api/show"
			method = http.MethodPost
			body = map[string]string{"model": t.Model}
		}
	}
	if t.Model != "" && t.Provider != "ollama" && t.Provider != "openrouter" && t.Provider != "deepseek" {
		path += "/" + escapeModelPath(strings.TrimPrefix(t.Model, "models/"))
	}
	next := ""
	visited := map[string]bool{}
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		uri := t.BaseURL + path
		if next != "" {
			q := url.Values{}
			if t.Provider == "anthropic" {
				q.Set("after_id", next)
			} else {
				q.Set("pageToken", next)
			}
			uri += "?" + q.Encode()
		}
		raw, err := readMetadataJSON(ctx, client, t, method, uri, body)
		if err != nil {
			cat.Partial = len(cat.Models) > 0
			return cat, err
		}
		if t.Provider == "ollama" && t.Model != "" {
			cat.Models = []ModelInfo{decodeModel(t.Provider, raw)}
			cat.Models[0].ID = t.Model
			return cat, nil
		}
		rows := array(raw["data"])
		if t.Provider == "gemini" || t.Provider == "ollama" {
			rows = array(raw["models"])
		}
		if rows == nil && t.Model == "" {
			return cat, ErrModelMetadataUnknown
		}
		if rows == nil && t.Model != "" {
			rows = []any{raw}
		}
		for _, value := range rows {
			row, ok := value.(map[string]any)
			if !ok {
				cat.Partial = true
				continue
			}
			info := decodeModel(t.Provider, row)
			if info.ID == "" || seen[info.ID] {
				continue
			}
			seen[info.ID] = true
			if t.Model == "" || info.ID == strings.TrimPrefix(t.Model, "models/") {
				cat.Models = append(cat.Models, info)
			}
		}
		next = str(raw["nextPageToken"])
		if b(raw["has_more"]) {
			next = str(raw["last_id"])
			if next == "" {
				cat.Partial = true
				return cat, fmt.Errorf("model catalog pagination omitted last_id")
			}
		}
		if next == "" {
			break
		}
		if visited[next] || page == 99 {
			cat.Partial = true
			return cat, fmt.Errorf("model catalog pagination did not terminate")
		}
		visited[next] = true
	}
	if t.Model != "" && len(cat.Models) == 0 {
		return cat, ErrModelMetadataUnknown
	}
	slices.SortFunc(cat.Models, func(a, b ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	return cat, nil
}
func escapeModelPath(s string) string {
	parts := strings.Split(s, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func readMetadataJSON(ctx context.Context, client *http.Client, t ModelTarget, method, uri string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, uri, reader)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if t.APIKey != "" {
		switch t.Provider {
		case "anthropic":
			req.Header.Set("x-api-key", t.APIKey)
		case "gemini":
			req.Header.Set("x-goog-api-key", t.APIKey)
		default:
			req.Header.Set("Authorization", "Bearer "+t.APIKey)
		}
	}
	if t.Provider == "anthropic" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	// Credentials belong to this endpoint, never a redirect target.
	scoped := *client
	scoped.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := scoped.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("metadata request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("metadata endpoint returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 16<<20 {
		return nil, fmt.Errorf("model metadata exceeds 16 MiB")
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("invalid model metadata JSON: %w", err)
	}
	return out, nil
}
func str(v any) string         { s, _ := v.(string); return s }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any        { a, _ := v.([]any); return a }
func b(v any) bool             { x, _ := v.(bool); return x }
func bp(v any) *bool {
	x, ok := v.(bool)
	if !ok {
		return nil
	}
	return &x
}
func ip(v any) *int {
	x, ok := v.(float64)
	if !ok || x < 0 {
		return nil
	}
	n := int(x)
	return &n
}
func stringsField(m map[string]any, k string) []string {
	v, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}
func truth(v bool) *bool { return &v }
func decodeModel(provider string, r map[string]any) ModelInfo {
	raw := boundedMetadata(r)
	info := ModelInfo{ID: str(r["id"]), Name: str(r["name"]), Description: str(r["description"]), Raw: raw, Lifecycle: map[string]any{}}
	for _, k := range []string{"created", "created_at", "version", "shutdown_date", "expiration_date", "deprecated", "preview", "owned_by"} {
		if v, ok := r[k]; ok {
			info.Lifecycle[k] = v
		}
	}
	arch := obj(r["architecture"])
	info.InputModalities = stringsField(arch, "input_modalities")
	info.OutputModalities = stringsField(arch, "output_modalities")
	info.ContextTokens = ip(r["context_length"])
	info.InputTokens = ip(r["max_input_tokens"])
	info.OutputTokens = ip(r["max_tokens"])
	switch provider {
	case "anthropic":
		info.Name = str(r["display_name"])
		info.Chat = truth(true)
		caps := obj(r["capabilities"])
		info.Tools = bp(obj(caps["tool_use"])["supported"])
		info.StructuredOutput = bp(obj(caps["structured_outputs"])["supported"])
		info.Reasoning = bp(obj(caps["thinking"])["supported"])
		info.ReasoningOptions = obj(caps["thinking"])
		if vision := bp(obj(caps["image_input"])["supported"]); vision != nil {
			info.InputModalities = []string{"text"}
			if *vision {
				info.InputModalities = append(info.InputModalities, "image")
			}
		}
		effort := obj(caps["effort"])
		for k, v := range effort {
			if k != "supported" && b(obj(v)["supported"]) {
				info.ReasoningEfforts = append(info.ReasoningEfforts, k)
			}
		}
		slices.Sort(info.ReasoningEfforts)
		info.ReasoningEffortsComplete = bp(effort["supported"]) != nil
		for _, k := range []string{"low", "medium", "high", "xhigh", "max"} {
			if bp(obj(effort[k])["supported"]) == nil {
				info.ReasoningEffortsComplete = false
			}
		}
		if info.ReasoningOptions == nil {
			info.ReasoningOptions = map[string]any{}
		}
		if effort != nil {
			info.ReasoningOptions["effort"] = effort
		}
	case "gemini":
		info.ID = strings.TrimPrefix(str(r["name"]), "models/")
		info.Name = str(r["displayName"])
		info.InputTokens = ip(r["inputTokenLimit"])
		info.OutputTokens = ip(r["outputTokenLimit"])
		info.Reasoning = bp(r["thinking"])
		info.Sampling = advertisedFields(r, "temperature", "maxTemperature", "topP", "topK")
		if methods := stringsField(r, "supportedGenerationMethods"); methods != nil {
			info.Chat = truth(slices.Contains(methods, "generateContent"))
		}
	case "ollama":
		info.ID = str(r["name"])
		if info.ID == "" {
			info.ID = str(r["model"])
		}
		if caps := stringsField(r, "capabilities"); caps != nil {
			info.Chat = truth(slices.Contains(caps, "completion"))
			info.Tools = truth(slices.Contains(caps, "tools"))
			info.Reasoning = truth(slices.Contains(caps, "thinking"))
			info.InputModalities = []string{"text"}
			if slices.Contains(caps, "vision") {
				info.InputModalities = append(info.InputModalities, "image")
			}
		}
		model := obj(r["model_info"])
		info.ContextTokens = ip(model[str(model["general.architecture"])+".context_length"])
		for _, line := range strings.Split(str(r["parameters"]), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "num_ctx" {
				if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
					info.RuntimeContextTokens = &n
				}
			}
		}
	case "huggingface", "openrouter":
		info.Routed = true
		info.Chat = truth(true)
		if info.OutputModalities != nil && !slices.Contains(info.OutputModalities, "text") {
			info.Chat = truth(false)
		}
		if provider == "huggingface" {
			info.EndpointsComplete = array(r["providers"]) != nil
			for _, v := range array(r["providers"]) {
				info.Endpoints = append(info.Endpoints, decodeEndpoint(provider, obj(v)))
			}
		}
		info.Pricing = obj(r["pricing"])
		info.PricingUnit = "USD per token"
		if provider == "huggingface" {
			info.PricingUnit = "USD per million tokens"
		}
		decodeParameters(&info.ModelCapabilities, r)
		info.OutputTokens = ip(obj(r["top_provider"])["max_completion_tokens"])
	}
	info.Prices = normalizedPrices(provider, info.Pricing, r)
	return info
}
func decodeParameters(c *ModelCapabilities, r map[string]any) {
	if p := stringsField(r, "supported_parameters"); p != nil {
		c.Parameters = map[string]bool{}
		c.ParametersComplete = true
		for _, s := range p {
			c.Parameters[s] = true
		}
		c.Tools = truth(slices.Contains(p, "tools"))
		c.StructuredOutput = truth(slices.Contains(p, "structured_outputs") || slices.Contains(p, "response_format"))
		c.Reasoning = truth(slices.Contains(p, "reasoning") || slices.Contains(p, "reasoning_effort") || slices.Contains(p, "include_reasoning"))
	}
	if efforts := stringsField(obj(r["reasoning"]), "supported_efforts"); efforts != nil {
		c.ReasoningEfforts = efforts
		c.ReasoningEffortsComplete = true
	}
}
func decodeEndpoint(provider string, r map[string]any) ModelEndpointInfo {
	raw := boundedMetadata(r)
	e := ModelEndpointInfo{ID: str(r["provider"]), Name: str(r["provider"]), Status: str(r["status"]), Raw: raw, Pricing: obj(r["pricing"]), PricingUnit: "USD per million tokens", Performance: map[string]any{}}
	e.ContextTokens = ip(r["context_length"])
	e.Tools = bp(r["supports_tools"])
	e.StructuredOutput = bp(r["supports_structured_output"])
	e.InputModalities = stringsField(obj(r["architecture"]), "input_modalities")
	if provider == "openrouter" {
		e.ID = str(r["tag"])
		e.Name = str(r["provider_name"])
		e.PricingUnit = "USD per token"
		if status, ok := r["status"].(float64); ok {
			e.Status = strconv.Itoa(int(status))
		}
		e.InputTokens = ip(r["max_prompt_tokens"])
		e.OutputTokens = ip(r["max_completion_tokens"])
		decodeParameters(&e.ModelCapabilities, r)
	}
	for _, k := range []string{"first_token_latency_ms", "throughput", "latency_last_30m", "throughput_last_30m", "uptime_last_30m", "uptime_last_1d"} {
		if v, ok := r[k]; ok {
			e.Performance[k] = v
		}
	}
	e.Prices = normalizedPrices(provider, e.Pricing, r)
	return e
}

// Keep provider additions inspectable without retaining unbounded model cards.
func boundedMetadata(r map[string]any) json.RawMessage {
	raw, _ := json.Marshal(r)
	if len(raw) > 64<<10 {
		return json.RawMessage(`{"truncated":true,"reason":"provider record exceeded 64 KiB"}`)
	}
	return raw
}
func advertisedFields(r map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}
	return out
}

func normalizedPrices(provider string, prices map[string]any, record map[string]any) []ModelPrice {
	var out []ModelPrice
	for item, amount := range prices {
		unit := ""
		if provider == "huggingface" {
			if item == "input" || item == "output" {
				unit = "million tokens"
			}
		} else if provider == "openrouter" {
			switch item {
			case "prompt", "completion", "input_cache_read", "input_cache_write":
				unit = "token"
			case "request":
				unit = "request"
			case "image":
				unit = "image"
			}
		}
		out = append(out, ModelPrice{Item: item, Amount: amount, Currency: "USD", Unit: unit, Conditions: advertisedFields(record, "is_free", "pricing_conditions")})
	}
	slices.SortFunc(out, func(a, b ModelPrice) int { return strings.Compare(a.Item, b.Item) })
	return out
}
