package llm

import (
	"fmt"
	"strconv"
	"strings"
)

// ThinkingLevel is an ordered, provider-agnostic reasoning level. Higher values
// mean deeper reasoning. Providers clamp to whatever subset they support.
type ThinkingLevel uint8

const (
	LevelMinimal ThinkingLevel = iota
	LevelLow
	LevelMedium
	LevelHigh
	LevelXHigh
	LevelMax
)

// levelBudgets is the canonical level<->budget table: the single source of
// truth for converting a named level to an approximate token budget and back.
var levelBudgets = [...]int{
	LevelMinimal: 1024,
	LevelLow:     4096,
	LevelMedium:  8192,
	LevelHigh:    16384,
	LevelXHigh:   32768,
	LevelMax:     65536,
}

// thinkingKind discriminates the ThinkingEffort tagged union. The zero value is
// kindOff so that a zero ThinkingEffort means "no thinking".
type thinkingKind uint8

const (
	kindOff thinkingKind = iota // zero value
	kindLevel
	kindBudget
	kindDynamic
)

// ThinkingEffort is a tagged union describing how much reasoning effort to spend.
// Exactly one of: Off (zero value), a named Level, a raw token Budget, or
// Dynamic (let the model decide). It is a comparable value type, so equality and
// map keys work as expected.
type ThinkingEffort struct {
	kind   thinkingKind
	level  ThinkingLevel
	budget int
}

// EffortOff returns the zero/disabled effort.
func EffortOff() ThinkingEffort { return ThinkingEffort{kind: kindOff} }

// EffortLevel returns an effort pinned to a named level.
func EffortLevel(l ThinkingLevel) ThinkingEffort {
	return ThinkingEffort{kind: kindLevel, level: l}
}

// EffortBudget returns an effort expressed as a raw token budget.
func EffortBudget(tokens int) ThinkingEffort {
	return ThinkingEffort{kind: kindBudget, budget: tokens}
}

// EffortDynamic returns an effort that defers depth to the model ("let the model
// decide" / Gemini -1 / Anthropic adaptive-with-no-effort).
func EffortDynamic() ThinkingEffort { return ThinkingEffort{kind: kindDynamic} }

// IsEnabled reports whether any thinking is requested (not off).
func (e ThinkingEffort) IsEnabled() bool { return e.kind != kindOff }

// IsDynamic reports whether the model should choose depth itself.
func (e ThinkingEffort) IsDynamic() bool { return e.kind == kindDynamic }

// Level returns the named level when the effort is a level.
func (e ThinkingEffort) Level() (ThinkingLevel, bool) {
	return e.level, e.kind == kindLevel
}

// Budget returns the raw token budget when the effort is a budget.
func (e ThinkingEffort) Budget() (int, bool) {
	return e.budget, e.kind == kindBudget
}

// AsLevel reduces the effort to a named level for providers that only accept
// levels (OpenAI, adaptive Anthropic, Gemini 3.x). A Budget maps to the nearest
// level via the canonical table; Dynamic and Off return dynamicFallback.
func (e ThinkingEffort) AsLevel(dynamicFallback ThinkingLevel) ThinkingLevel {
	switch e.kind {
	case kindLevel:
		return e.level
	case kindBudget:
		return budgetToLevel(e.budget)
	default: // kindOff, kindDynamic
		return dynamicFallback
	}
}

// AsBudget reduces the effort to a token budget for providers that accept one
// (legacy Anthropic, Gemini 2.5). A Level maps via the canonical table. Dynamic
// and Off return (0, false), meaning "use the provider's own dynamic/default".
func (e ThinkingEffort) AsBudget() (int, bool) {
	switch e.kind {
	case kindLevel:
		return levelBudgets[e.level], true
	case kindBudget:
		return e.budget, true
	default: // kindOff, kindDynamic
		return 0, false
	}
}

// budgetToLevel maps a token budget to the highest level whose canonical
// threshold is <= n, flooring at LevelMinimal.
func budgetToLevel(n int) ThinkingLevel {
	level := LevelMinimal
	for l := LevelMinimal; l <= LevelMax; l++ {
		if n >= levelBudgets[l] {
			level = l
		}
	}
	return level
}

// levelName maps a level to its canonical parse string.
func (l ThinkingLevel) String() string {
	switch l {
	case LevelMinimal:
		return "minimal"
	case LevelLow:
		return "low"
	case LevelMedium:
		return "medium"
	case LevelHigh:
		return "high"
	case LevelXHigh:
		return "xhigh"
	case LevelMax:
		return "max"
	default:
		return "unknown"
	}
}

// String renders the effort back into the form ParseThinkingEffort accepts, so
// it round-trips. Used for display/debug; persistence stores the raw input.
func (e ThinkingEffort) String() string {
	switch e.kind {
	case kindLevel:
		return e.level.String()
	case kindBudget:
		return strconv.Itoa(e.budget)
	case kindDynamic:
		return "dynamic"
	default:
		return "off"
	}
}

// ParseThinkingEffort converts a string to a ThinkingEffort. Accepts: off (or
// empty), dynamic/auto, the named levels minimal/low/medium/high/xhigh/max, or a
// positive integer token budget. Old values (off/low/medium/high) are preserved.
func ParseThinkingEffort(s string) (ThinkingEffort, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "", "off":
		return EffortOff(), nil
	case "dynamic", "auto":
		return EffortDynamic(), nil
	case "minimal":
		return EffortLevel(LevelMinimal), nil
	case "low":
		return EffortLevel(LevelLow), nil
	case "medium":
		return EffortLevel(LevelMedium), nil
	case "high":
		return EffortLevel(LevelHigh), nil
	case "xhigh":
		return EffortLevel(LevelXHigh), nil
	case "max":
		return EffortLevel(LevelMax), nil
	default:
		if n, err := strconv.Atoi(v); err == nil {
			if n <= 0 {
				return ThinkingEffort{}, fmt.Errorf("invalid thinking effort %q: token budget must be positive", s)
			}
			return EffortBudget(n), nil
		}
		return ThinkingEffort{}, fmt.Errorf("invalid thinking effort %q: must be off, dynamic, a level (minimal, low, medium, high, xhigh, max), or a positive token budget", s)
	}
}
