package llm

import "testing"

func TestParseThinkingEffort(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    ThinkingEffort
		wantErr bool
	}{
		{"empty is off", "", EffortOff(), false},
		{"off", "off", EffortOff(), false},
		{"off uppercase", "OFF", EffortOff(), false},
		{"off padded", "  off  ", EffortOff(), false},
		{"dynamic", "dynamic", EffortDynamic(), false},
		{"auto alias", "auto", EffortDynamic(), false},
		{"minimal", "minimal", EffortLevel(LevelMinimal), false},
		{"low", "low", EffortLevel(LevelLow), false},
		{"medium", "medium", EffortLevel(LevelMedium), false},
		{"high", "high", EffortLevel(LevelHigh), false},
		{"high uppercase", "High", EffortLevel(LevelHigh), false},
		{"xhigh", "xhigh", EffortLevel(LevelXHigh), false},
		{"max", "max", EffortLevel(LevelMax), false},
		{"raw budget", "12000", EffortBudget(12000), false},
		{"raw budget padded", " 2048 ", EffortBudget(2048), false},
		{"zero budget invalid", "0", ThinkingEffort{}, true},
		{"negative budget invalid", "-5", ThinkingEffort{}, true},
		{"garbage invalid", "bogus", ThinkingEffort{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseThinkingEffort(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseThinkingEffort(%q): want error, got %+v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseThinkingEffort(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseThinkingEffort(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

func TestThinkingEffortStringRoundTrip(t *testing.T) {
	cases := []ThinkingEffort{
		EffortOff(),
		EffortDynamic(),
		EffortLevel(LevelMinimal),
		EffortLevel(LevelLow),
		EffortLevel(LevelMedium),
		EffortLevel(LevelHigh),
		EffortLevel(LevelXHigh),
		EffortLevel(LevelMax),
		EffortBudget(12000),
	}
	for _, e := range cases {
		got, err := ParseThinkingEffort(e.String())
		if err != nil {
			t.Fatalf("re-parsing %q: %v", e.String(), err)
		}
		if got != e {
			t.Fatalf("round-trip of %+v via %q = %+v", e, e.String(), got)
		}
	}
}

func TestThinkingEffortIsEnabledIsDynamic(t *testing.T) {
	tests := []struct {
		effort    ThinkingEffort
		enabled   bool
		isDynamic bool
	}{
		{EffortOff(), false, false},
		{ThinkingEffort{}, false, false}, // zero value == off
		{EffortDynamic(), true, true},
		{EffortLevel(LevelLow), true, false},
		{EffortBudget(5000), true, false},
	}
	for _, tc := range tests {
		if got := tc.effort.IsEnabled(); got != tc.enabled {
			t.Errorf("%+v IsEnabled() = %v, want %v", tc.effort, got, tc.enabled)
		}
		if got := tc.effort.IsDynamic(); got != tc.isDynamic {
			t.Errorf("%+v IsDynamic() = %v, want %v", tc.effort, got, tc.isDynamic)
		}
	}
}

func TestThinkingEffortAsLevel(t *testing.T) {
	const fb = LevelMedium
	tests := []struct {
		name   string
		effort ThinkingEffort
		want   ThinkingLevel
	}{
		{"named level passes through", EffortLevel(LevelXHigh), LevelXHigh},
		{"dynamic uses fallback", EffortDynamic(), fb},
		{"off uses fallback", EffortOff(), fb},
		{"budget below minimal floors to minimal", EffortBudget(500), LevelMinimal},
		{"budget at minimal threshold", EffortBudget(1024), LevelMinimal},
		{"budget between low and medium maps to low", EffortBudget(5000), LevelLow},
		{"budget at high threshold", EffortBudget(16384), LevelHigh},
		{"budget above max maps to max", EffortBudget(100000), LevelMax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.effort.AsLevel(fb); got != tc.want {
				t.Fatalf("AsLevel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestThinkingEffortAsBudget(t *testing.T) {
	tests := []struct {
		name   string
		effort ThinkingEffort
		want   int
		wantOK bool
	}{
		{"level maps to canonical budget", EffortLevel(LevelHigh), 16384, true},
		{"minimal level budget", EffortLevel(LevelMinimal), 1024, true},
		{"raw budget passes through", EffortBudget(12000), 12000, true},
		{"dynamic has no budget", EffortDynamic(), 0, false},
		{"off has no budget", EffortOff(), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.effort.AsBudget()
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("AsBudget = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Every advertised word parses and renders back as itself, so completions,
// usage text, and the parser cannot drift apart.
func TestThinkingEffortWordsRoundTrip(t *testing.T) {
	words := ThinkingEffortWords()
	if len(words) != 2+len(ThinkingLevelNames()) {
		t.Fatalf("words = %v", words)
	}
	for _, word := range words {
		effort, err := ParseThinkingEffort(word)
		if err != nil {
			t.Fatalf("ParseThinkingEffort(%q) error = %v", word, err)
		}
		if got := effort.String(); got != word {
			t.Fatalf("ParseThinkingEffort(%q).String() = %q", word, got)
		}
	}
	if got := ThinkingLevel(200).String(); got != "unknown" {
		t.Fatalf("out-of-range level String() = %q", got)
	}
}
