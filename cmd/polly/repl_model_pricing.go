package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
)

// modelPricing returns input/output/cached prices per million tokens. Pinned
// routes use endpoint prices; Automatic uses advertised model catalog prices.
// Missing endpoint prices are never filled from aggregate model prices.
func (f *modelForm) modelPricing() string {
	info, host, exact := f.selectedModelInfo()
	if !exact {
		return ""
	}
	prices := info.Prices
	if host != "" {
		prices = nil
		for _, endpoint := range info.Endpoints {
			if endpoint.ID == host {
				prices = endpoint.Prices
				break
			}
		}
	}
	fields := map[string]string{}
	for _, price := range prices {
		field := ""
		switch price.Item {
		case "input", "prompt":
			field = "In"
		case "output", "completion":
			field = "Out"
		case "input_cache_read", "cache_read", "cached_input":
			field = "Cached"
		default:
			continue
		}
		amount, ok := pricePerMillion(price)
		if !ok {
			continue
		}
		fields[field] = amount
	}
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, field := range []string{"In", "Out", "Cached"} {
		price, ok := fields[field]
		if !ok {
			price = "—"
		}
		parts = append(parts, price)
	}
	return strings.Join(parts, "/")
}

func pricePerMillion(price llm.ModelPrice) (string, bool) {
	factor := float64(0)
	switch price.Unit {
	case "token":
		factor = 1_000_000
	case "million tokens":
		factor = 1
	default:
		return "", false
	}
	value, err := strconv.ParseFloat(fmt.Sprint(price.Amount), 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || price.Currency == "" {
		return "", false
	}
	value *= factor
	if math.IsInf(value, 0) {
		return "", false
	}
	currency := price.Currency + " "
	if price.Currency == "USD" {
		currency = "$"
	}
	return currency + strconv.FormatFloat(value, 'g', 6, 64), true
}

func (f *modelForm) selectedModelInfo() (llm.ModelInfo, string, bool) {
	name := strings.TrimPrefix(strings.TrimSpace(f.model.text()), f.provider+"/")
	info, exact := f.infos[name]
	host := ""
	if !exact && (f.provider == "openrouter" || f.provider == "huggingface") {
		if i := strings.LastIndex(name, ":"); i >= 0 {
			host = name[i+1:]
			info, exact = f.infos[name[:i]]
		}
	}
	if !exact {
		return llm.ModelInfo{}, "", false
	}
	return info, host, true
}

func (f *modelForm) modelContextWindow() int {
	info, host, ok := f.selectedModelInfo()
	if !ok {
		return 0
	}
	return info.EffectiveCapabilities(host).ContextWindow()
}
