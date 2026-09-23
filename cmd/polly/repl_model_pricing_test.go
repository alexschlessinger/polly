package main

import (
	"math"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func TestModelPricingUsesSelectedRouteAndAdvertisedUnits(t *testing.T) {
	f := &modelForm{provider: "openrouter", infos: map[string]llm.ModelInfo{"org/model": {
		ID: "org/model", Routed: true,
		Prices: []llm.ModelPrice{
			{Item: "prompt", Amount: "0.000002", Currency: "USD", Unit: "token"},
			{Item: "completion", Amount: "0.000008", Currency: "USD", Unit: "token"},
			{Item: "input_cache_read", Amount: "0.0000005", Currency: "USD", Unit: "token"},
		},
		Endpoints: []llm.ModelEndpointInfo{
			{ID: "host", Prices: []llm.ModelPrice{{Item: "prompt", Amount: 3.5, Currency: "USD", Unit: "million tokens"}}},
			{ID: "unknown"},
		},
	}}}
	for _, tc := range []struct{ name, want string }{
		{"org/model", "$2/$8/$0.5"},
		{"org/model:host", "$3.5/—/—"},
		{"org/model:unknown", ""}, {"org/model:missing", ""}, {"org/", ""},
	} {
		f.model.setText(tc.name)
		if got := f.modelPricing(); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
func TestModelPricingPreservesFreeAndOmitsUnspecifiedOrInvalidPrices(t *testing.T) {
	f := &modelForm{provider: "huggingface", infos: map[string]llm.ModelInfo{"org/model": {
		ID: "org/model", Prices: []llm.ModelPrice{
			{Item: "input", Amount: 0, Currency: "USD", Unit: "million tokens"},
			{Item: "output", Amount: 4, Currency: "USD"},
			{Item: "cached_input", Amount: -1, Currency: "USD", Unit: "token"},
		},
	}}}
	f.model.setText("org/model")
	if got := f.modelPricing(); got != "$0/—/—" {
		t.Fatal(got)
	}
	info := f.infos["org/model"]
	info.Prices = []llm.ModelPrice{{Item: "input", Amount: "NaN", Currency: "USD", Unit: "token"}, {Item: "output", Amount: "Inf", Currency: "USD", Unit: "token"}}
	f.infos[info.ID] = info
	if f.modelPricing() != "" {
		t.Fatal("invalid prices displayed")
	}
}
func TestModelPricingLowerLeftInsideForm(t *testing.T) {
	r, _ := newFormREPL(t)
	f := r.model.modal.modelForm
	f.infos = map[string]llm.ModelInfo{"current": {ID: "current", Prices: []llm.ModelPrice{
		{Item: "input", Amount: "12.75", Currency: "USD", Unit: "million tokens"},
		{Item: "output", Amount: "45.25", Currency: "USD", Unit: "million tokens"},
		{Item: "cached_input", Amount: "1.25", Currency: "USD", Unit: "million tokens"},
	}}}
	for _, width := range []int{40, 60, 100} {
		text := plainStyledText(f.text(20, width))
		bottom := strings.Split(text, "\n")[f.fieldRows[4]]
		if !strings.HasPrefix(bottom, "  $12.75/$45.25/$1.25") || !strings.HasSuffix(bottom, "[ Apply ]") {
			t.Fatalf("footer: %q", bottom)
		}
		for _, line := range strings.Split(text, "\n") {
			if len([]rune(line)) > width-2 {
				t.Fatalf("overflow at %d: %s", width, line)
			}
		}
	}
}

func TestModelRatesForRoute(t *testing.T) {
	info := llm.ModelInfo{
		ID: "org/model",
		Prices: []llm.ModelPrice{
			{Item: "prompt", Amount: "0.000002", Currency: "USD", Unit: "token"},
			{Item: "completion", Amount: "0.000008", Currency: "USD", Unit: "token"},
			{Item: "input_cache_read", Amount: "0.0000005", Currency: "USD", Unit: "token"},
			{Item: "input_cache_write", Amount: 2.5, Currency: "USD", Unit: "million tokens"},
		},
		Endpoints: []llm.ModelEndpointInfo{
			{ID: "host", Prices: []llm.ModelPrice{
				{Item: "input", Amount: 3, Currency: "USD", Unit: "million tokens"},
				{Item: "output", Amount: 9, Currency: "USD", Unit: "million tokens"},
			}},
			{ID: "half", Prices: []llm.ModelPrice{{Item: "input", Amount: 3, Currency: "USD", Unit: "million tokens"}}},
			{ID: "euro", Prices: []llm.ModelPrice{
				{Item: "input", Amount: 3, Currency: "EUR", Unit: "million tokens"},
				{Item: "output", Amount: 9, Currency: "EUR", Unit: "million tokens"},
			}},
		},
	}
	close := func(a, b float64) bool { return math.Abs(a-b) < 1e-15 }
	aggregate := modelRatesFor(info, "")
	if !aggregate.known || !close(aggregate.in, 2e-6) || !close(aggregate.out, 8e-6) ||
		!aggregate.hasCacheRead || !close(aggregate.cacheRead, 0.5e-6) ||
		!aggregate.hasCacheWrite || !close(aggregate.cacheWrite, 2.5e-6) {
		t.Fatalf("aggregate rates = %+v", aggregate)
	}
	pinned := modelRatesFor(info, "host")
	if !pinned.known || !close(pinned.in, 3e-6) || !close(pinned.out, 9e-6) || pinned.hasCacheRead || pinned.hasCacheWrite {
		t.Fatalf("pinned rates = %+v; endpoint prices must not borrow aggregate cache rates", pinned)
	}
	for _, host := range []string{"half", "euro", "missing"} {
		if rates := modelRatesFor(info, host); rates.known {
			t.Fatalf("%s: rates known = %+v", host, rates)
		}
	}
}
