package contract

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"
)

// ModelTarget identifies an inference destination. APIKey is never serialized.
type ModelTarget struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"baseURL,omitempty"`
	Model    string `json:"model,omitempty"`
	Host     string `json:"host,omitempty"`
	APIKey   string `json:"-"`
	// UseConfiguredKey bypasses the process override for a credential-clear preview.
	UseConfiguredKey bool `json:"-"`
}

// ModelPrice preserves the provider's amount and billing basis. An empty unit
// is unknown; zero is a valid advertised free price.
type ModelPrice struct {
	Item       string         `json:"item"`
	Amount     any            `json:"amount"`
	Currency   string         `json:"currency,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	Conditions map[string]any `json:"conditions,omitempty"`
}

type ModelEndpointInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	ModelCapabilities
	Prices      []ModelPrice    `json:"prices,omitempty"`
	Pricing     map[string]any  `json:"pricing,omitempty"`
	PricingUnit string          `json:"pricingUnit,omitempty"`
	Performance map[string]any  `json:"performance,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	ModelCapabilities
	Endpoints []ModelEndpointInfo `json:"endpoints,omitempty"`
	// Routed prevents a catalog's aggregate limits being mistaken for host guarantees.
	// LimitsApplyToAllRoutes marks an explicit model-wide limit, not a catalog maximum.
	LimitsApplyToAllRoutes bool            `json:"limitsApplyToAllRoutes,omitempty"`
	EndpointsComplete      bool            `json:"endpointsComplete,omitempty"`
	Routed                 bool            `json:"routed,omitempty"`
	Prices                 []ModelPrice    `json:"prices,omitempty"`
	Pricing                map[string]any  `json:"pricing,omitempty"`
	PricingUnit            string          `json:"pricingUnit,omitempty"`
	Lifecycle              map[string]any  `json:"lifecycle,omitempty"`
	Raw                    json.RawMessage `json:"raw,omitempty"`
}

type ModelCatalog struct {
	Models    []ModelInfo `json:"models"`
	Source    string      `json:"source"`
	FetchedAt time.Time   `json:"fetchedAt"`
	Partial   bool        `json:"partial,omitempty"`
	Stale     bool        `json:"-"`
	Error     string      `json:"-"`
}

// ErrModelMetadataUnknown reports that no catalog describes the target.
var ErrModelMetadataUnknown = errors.New("model metadata is unavailable")

// MergeReasoningPolicy fills unknown reasoning policy facts in dst from src.
// Catalog policy provides defaults; explicit endpoint policy wins.
func MergeReasoningPolicy(dst *ModelCapabilities, src ModelCapabilities) {
	if !src.ReasoningPolicy {
		return
	}
	dst.ReasoningPolicy = true
	if dst.ReasoningMandatory == nil {
		dst.ReasoningMandatory = src.ReasoningMandatory
	}
	if dst.ReasoningDefaultEnabled == nil {
		dst.ReasoningDefaultEnabled = src.ReasoningDefaultEnabled
	}
	if dst.ReasoningDefaultEffort == nil {
		dst.ReasoningDefaultEffort = src.ReasoningDefaultEffort
	}
	if dst.ReasoningMaxTokens == nil {
		dst.ReasoningMaxTokens = src.ReasoningMaxTokens
	}
	if !dst.ReasoningEffortsComplete {
		dst.ReasoningEfforts, dst.ReasoningEffortsComplete = src.ReasoningEfforts, src.ReasoningEffortsComplete
	}
}

// EffectiveCapabilities resolves only facts valid for the selected route.
func (m ModelInfo) EffectiveCapabilities(host string) ModelCapabilities {
	if !m.Routed {
		return m.ModelCapabilities
	}
	var candidates []ModelCapabilities
	for _, e := range m.Endpoints {
		if host != "" {
			if e.ID == host {
				return overlayCapabilities(m.ModelCapabilities, e.ModelCapabilities, m.LimitsApplyToAllRoutes)
			}
			continue
		}
		if e.Status != "" && e.Status != "live" && e.Status != "0" {
			continue
		}
		candidates = append(candidates, overlayCapabilities(m.ModelCapabilities, e.ModelCapabilities, m.LimitsApplyToAllRoutes))
	}
	if host != "" {
		out := ModelCapabilities{}
		MergeReasoningPolicy(&out, m.ModelCapabilities)
		return out
	}
	if !m.EndpointsComplete || len(candidates) == 0 {
		// Architecture is a model fact. Aggregate route capabilities and limits
		// are not guarantees when the eligible endpoint set is unknown.
		out := ModelCapabilities{Chat: m.Chat, InputModalities: m.InputModalities, OutputModalities: m.OutputModalities}
		MergeReasoningPolicy(&out, m.ModelCapabilities)
		if m.LimitsApplyToAllRoutes {
			out.ContextTokens = m.ContextTokens
			out.InputTokens = m.InputTokens
			out.OutputTokens = m.OutputTokens
			out.UnlimitedLimits = m.UnlimitedLimits
		}
		return out
	}
	out := candidates[0]
	effortsAgree := true
	out.UnlimitedLimits = maps.Clone(out.UnlimitedLimits)
	for _, c := range candidates[1:] {
		effortsAgree = effortsAgree && reflect.DeepEqual(out.ReasoningEfforts, c.ReasoningEfforts) && out.ReasoningEffortsComplete == c.ReasoningEffortsComplete
		a, b := reflect.ValueOf(&out).Elem(), reflect.ValueOf(c)
		for i := 0; i < a.NumField(); i++ {
			x, y := a.Field(i), b.Field(i)
			key := strings.Split(a.Type().Field(i).Tag.Get("json"), ",")[0]
			if key == "unlimitedLimits" {
				continue
			}
			if x.Type() == reflect.TypeFor[*int]() {
				n, unlimited := commonModelLimit(x.Interface().(*int), y.Interface().(*int), out.UnlimitedLimits[key], c.UnlimitedLimits[key])
				x.Set(reflect.ValueOf(n))
				if unlimited {
					if out.UnlimitedLimits == nil {
						out.UnlimitedLimits = map[string]bool{}
					}
					out.UnlimitedLimits[key] = true
				} else {
					delete(out.UnlimitedLimits, key)
				}
			} else if !reflect.DeepEqual(x.Interface(), y.Interface()) {
				x.SetZero()
			}
		}
	}
	// Different complete parameter sets are uncertain, not an empty supported set.
	if out.Parameters == nil {
		out.ParametersComplete = false
	}
	if !effortsAgree {
		out.ReasoningEfforts = nil
		out.ReasoningEffortsComplete = false
	}
	return out
}
func overlayCapabilities(model, endpoint ModelCapabilities, sharedLimits bool) ModelCapabilities {
	// Routed catalog limits and parameter unions describe available options,
	// not every endpoint. Route-specific declarations must establish them.
	if !sharedLimits {
		model.UnlimitedLimits = nil
		model.ContextTokens = nil
		model.InputTokens = nil
		model.OutputTokens = nil
		model.RuntimeContextTokens = nil
	}
	model.Tools = nil
	model.StructuredOutput = nil
	model.Reasoning = nil
	model.Parameters = nil
	model.ParametersComplete = false
	if !model.ReasoningPolicy {
		model.ReasoningEfforts = nil
		model.ReasoningEffortsComplete = false
	}
	a, b := reflect.ValueOf(&model).Elem(), reflect.ValueOf(endpoint)
	for i := 0; i < a.NumField(); i++ {
		x := b.Field(i)
		if !x.IsZero() {
			a.Field(i).Set(x)
		}
	}
	if endpoint.ReasoningEffortsComplete {
		model.ReasoningEfforts = endpoint.ReasoningEfforts
		model.ReasoningEffortsComplete = true
	}
	return model
}

func commonModelLimit(a, b *int, au, bu bool) (*int, bool) {
	if au && bu {
		return nil, true
	}
	if au {
		return b, false
	}
	if bu {
		return a, false
	}
	if a == nil || b == nil {
		return nil, false
	}
	n := min(*a, *b)
	return &n, false
}
