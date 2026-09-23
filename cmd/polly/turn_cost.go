package main

import (
	"math"
	"strconv"
)

// turnRates are a model's advertised prices in US dollars per token. A zero
// cache rate means the catalog advertised none.
type turnRates struct {
	in, out, cacheRead, cacheWrite float64
	hasCacheRead, hasCacheWrite    bool
	known                          bool
}

// cost prices a request's usage. Cache counts are subsets of in: a cache rate
// replaces the input rate for those tokens, and a missing one leaves them at
// the input rate.
func (r turnRates) cost(in, out, cacheRead, cacheWrite int) float64 {
	usd := float64(in)*r.in + float64(out)*r.out
	if r.hasCacheRead {
		usd += float64(cacheRead) * (r.cacheRead - r.in)
	}
	if r.hasCacheWrite {
		usd += float64(cacheWrite) * (r.cacheWrite - r.in)
	}
	return max(usd, 0)
}

// turnCost is a turn's cost as a status row shows it: billed by the provider,
// or estimated from advertised rates.
type turnCost struct {
	usd       float64
	known     bool
	estimated bool
}

// formatCostAmount renders a US dollar amount to four significant digits
// without exponent notation, trimming trailing zeros.
func formatCostAmount(usd float64) string {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return "0"
	}
	decimals := min(max(3-int(math.Floor(math.Log10(usd))), 0), 12)
	s := strconv.FormatFloat(usd, 'f', decimals, 64)
	if decimals > 0 {
		for s[len(s)-1] == '0' {
			s = s[:len(s)-1]
		}
		if s[len(s)-1] == '.' {
			s = s[:len(s)-1]
		}
	}
	return s
}

// formatCostUSD is formatCostAmount with the dollar sign.
func formatCostUSD(usd float64) string {
	return "$" + formatCostAmount(usd)
}
