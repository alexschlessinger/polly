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
	fields := map[string]string{}
	for _, price := range routePrices(info, host) {
		field := ""
		switch priceItemKind(price.Item) {
		case priceInput:
			field = "In"
		case priceOutput:
			field = "Out"
		case priceCacheRead:
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

// routePrices returns the prices for a route: a pinned host's endpoint
// prices, or the model's advertised prices. Missing endpoint prices are never
// filled from aggregate model prices.
func routePrices(info llm.ModelInfo, host string) []llm.ModelPrice {
	if host == "" {
		return info.Prices
	}
	for _, endpoint := range info.Endpoints {
		if endpoint.ID == host {
			return endpoint.Prices
		}
	}
	return nil
}

type priceKind int

const (
	priceOther priceKind = iota
	priceInput
	priceOutput
	priceCacheRead
	priceCacheWrite
)

func priceItemKind(item string) priceKind {
	switch item {
	case "input", "prompt":
		return priceInput
	case "output", "completion":
		return priceOutput
	case "input_cache_read", "cache_read", "cached_input":
		return priceCacheRead
	case "input_cache_write", "cache_write", "cache_creation":
		return priceCacheWrite
	}
	return priceOther
}

// modelRatesFor returns a route's US dollar rates per token. Rates are known
// only when both input and output prices are.
func modelRatesFor(info llm.ModelInfo, host string) turnRates {
	var rates turnRates
	var hasIn, hasOut bool
	for _, price := range routePrices(info, host) {
		if price.Currency != "USD" {
			continue
		}
		perMillion, ok := pricePerMillionValue(price)
		if !ok {
			continue
		}
		perToken := perMillion / 1_000_000
		switch priceItemKind(price.Item) {
		case priceInput:
			rates.in, hasIn = perToken, true
		case priceOutput:
			rates.out, hasOut = perToken, true
		case priceCacheRead:
			rates.cacheRead, rates.hasCacheRead = perToken, true
		case priceCacheWrite:
			rates.cacheWrite, rates.hasCacheWrite = perToken, true
		}
	}
	rates.known = hasIn && hasOut
	return rates
}

// pricePerMillionValue converts a price to its amount per million tokens.
func pricePerMillionValue(price llm.ModelPrice) (float64, bool) {
	factor := float64(0)
	switch price.Unit {
	case "token":
		factor = 1_000_000
	case "million tokens":
		factor = 1
	default:
		return 0, false
	}
	value, err := strconv.ParseFloat(fmt.Sprint(price.Amount), 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || price.Currency == "" {
		return 0, false
	}
	value *= factor
	if math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func pricePerMillion(price llm.ModelPrice) (string, bool) {
	value, ok := pricePerMillionValue(price)
	if !ok {
		return "", false
	}
	currency := price.Currency + " "
	if price.Currency == "USD" {
		currency = "$"
	}
	return currency + strconv.FormatFloat(value, 'g', 6, 64), true
}

func (f *modelForm) selectedModelInfo() (llm.ModelInfo, string, bool) {
	model, host, err := f.route()
	if err != nil {
		return llm.ModelInfo{}, "", false
	}
	name := strings.TrimPrefix(model, f.provider+"/")
	info, exact := f.infos[name]
	if !exact && f.provider == "huggingface" {
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
