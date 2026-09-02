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

// levelNames is the canonical level<->name table: the single source of truth
// for parsing, display, and the names offered in usage text and completions.
var levelNames = [...]string{
	LevelMinimal: "minimal",
	LevelLow:     "low",
	LevelMedium:  "medium",
	LevelHigh:    "high",
	LevelXHigh:   "xhigh",
	LevelMax:     "max",
}

// effortWord is one row of effortWords: a word ParseThinkingEffort accepts and
// the effort it names. An alias parses but is never advertised: it is absent
// from completions and usage text, and String renders the canonical word.
type effortWord struct {
	word   string
	effort ThinkingEffort
	alias  bool
}

// effortWords is the canonical word<->effort table: the single source of
// truth for parsing, display, completions, and usage text. The non-level
// words come first, then the levels in ascending order.
var effortWords = func() []effortWord {
	words := []effortWord{
		{word: "off", effort: EffortOff()},
		{word: "dynamic", effort: EffortDynamic()},
		{word: "auto", effort: EffortDynamic(), alias: true},
	}
	for level, name := range levelNames {
		words = append(words, effortWord{word: name, effort: EffortLevel(ThinkingLevel(level))})
	}
	return words
}()

// ThinkingEffortWords lists every advertised word ParseThinkingEffort
// accepts, from off through the levels, for completions. Aliases are not
// listed; token budgets are free-form.
func ThinkingEffortWords() []string {
	var words []string
	for _, w := range effortWords {
		if !w.alias {
			words = append(words, w.word)
		}
	}
	return words
}

// ThinkingEffortForms spells out every accepted effort form for usage and
// error text: the advertised non-level words, the levels, and a budget.
func ThinkingEffortForms() string {
	var named []string
	for _, w := range effortWords {
		if !w.alias && w.effort.kind != kindLevel {
			named = append(named, w.word)
		}
	}
	return strings.Join(named, ", ") + ", a level (" + strings.Join(levelNames[:], ", ") + "), or a positive token budget (e.g. 12000)"
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

// String renders the level as its canonical parse name.
func (l ThinkingLevel) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return "unknown"
}

// String renders the effort back into the form ParseThinkingEffort accepts, so
// it round-trips. Used for display/debug; persistence stores the raw input.
func (e ThinkingEffort) String() string {
	switch e.kind {
	case kindLevel:
		return e.level.String()
	case kindBudget:
		return strconv.Itoa(e.budget)
	}
	for _, w := range effortWords {
		if !w.alias && w.effort.kind == e.kind {
			return w.word
		}
	}
	return "off"
}

// ParseThinkingEffort converts a string to a ThinkingEffort: any word in
// effortWords, aliases included (empty means off), or a positive integer
// token budget. Old values (off/low/medium/high) are preserved.
func ParseThinkingEffort(s string) (ThinkingEffort, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return EffortOff(), nil
	}
	for _, w := range effortWords {
		if v == w.word {
			return w.effort, nil
		}
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return ThinkingEffort{}, fmt.Errorf("invalid thinking effort %q: token budget must be positive", s)
		}
		return EffortBudget(n), nil
	}
	return ThinkingEffort{}, fmt.Errorf("invalid thinking effort %q: must be %s", s, ThinkingEffortForms())
}
