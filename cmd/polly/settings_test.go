package main

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// TestSettingSpecGateMembership pins the three copy gates and the derived key
// lists. The exclusions are load-bearing: flipping startupWriteBack or
// persistOnSet on a row changes what reaches persisted session metadata.
func TestSettingSpecGateMembership(t *testing.T) {
	pin := func(name string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	pin("replSettingKeys", replSettingKeys,
		[]string{"model", "temp", "maxtokens", "maxcontext", "thinking", "system", "display", "tooltimeout", "skilldir", "sandbox"})
	pin("replSettableKeys", replSettableKeys,
		[]string{"model", "temp", "maxtokens", "maxcontext", "thinking", "tooltimeout"})
	pin("flagged rows", settingKeysWhere(func(s settingSpec) bool { return s.flag != "" }),
		[]string{"model", "temp", "maxtokens", "maxcontext", "thinking", "system", "tooltimeout", "skilldir", "maxiterations"})
	pin("startupWriteBack gate", settingKeysWhere(func(s settingSpec) bool { return s.startupWriteBack }),
		[]string{"model", "temp", "maxtokens", "tooltimeout", "skilldir", "maxiterations"})
	pin("persistOnSet gate", settingKeysWhere(func(s settingSpec) bool { return s.persistOnSet }),
		[]string{"model", "temp", "maxtokens", "maxcontext", "thinking", "tooltimeout"})

	for _, spec := range settingSpecs {
		if spec.flag != "" {
			if spec.fromCmd == nil || spec.fromMeta == nil || spec.toMeta == nil {
				t.Errorf("flagged setting %q must carry fromCmd, fromMeta, and toMeta", spec.key)
			}
		} else {
			if spec.fromCmd != nil || spec.fromMeta != nil || spec.toMeta != nil ||
				spec.startupWriteBack || spec.persistOnSet || spec.parse != nil {
				t.Errorf("derived setting %q must be show-only", spec.key)
			}
		}
		if (spec.startupWriteBack || spec.persistOnSet) && spec.toMeta == nil {
			t.Errorf("setting %q joins a copy gate without a toMeta", spec.key)
		}
		if spec.parse != nil && spec.show == nil {
			t.Errorf("settable setting %q must also be gettable", spec.key)
		}
	}
}

// TestSettingSpecMetadataRoundTrip catches a mispointed or duplicated
// cfg/metadata accessor pair: every value written through toMeta must come
// back identical through fromMeta.
func TestSettingSpecMetadataRoundTrip(t *testing.T) {
	src := &Config{MaxIterations: 17}
	src.Model = "anthropic/claude-test"
	src.Temperature = 0.42
	src.MaxTokens = 1234
	src.MaxHistoryTokens = 5678
	src.ThinkingEffort = "high"
	src.SystemPrompt = "be terse"
	src.ToolTimeout = 45 * time.Second
	src.SkillDirs = []string{"/a", "/b"}

	md := &sessions.Metadata{}
	for _, spec := range settingSpecs {
		if spec.toMeta != nil {
			spec.toMeta(src, md)
		}
	}
	dst := &Config{}
	for _, spec := range settingSpecs {
		if spec.fromMeta != nil {
			spec.fromMeta(dst, md)
		}
	}
	if !reflect.DeepEqual(src.Settings, dst.Settings) {
		t.Errorf("settings round trip = %+v, want %+v", dst.Settings, src.Settings)
	}
	if src.MaxIterations != dst.MaxIterations {
		t.Errorf("maxiterations round trip = %d, want %d", dst.MaxIterations, src.MaxIterations)
	}
}

// TestSettingSpecFlagsExist catches a row naming a CLI flag that was never
// defined (or a renamed flag leaving a stale row behind).
func TestSettingSpecFlagsExist(t *testing.T) {
	flags, _ := defineFlagsWithGroups()
	names := map[string]bool{}
	for _, f := range flags {
		for _, n := range f.Names() {
			names[n] = true
		}
	}
	for _, spec := range settingSpecs {
		if spec.flag != "" && !names[spec.flag] {
			t.Errorf("setting %q references flag %q that is not defined", spec.key, spec.flag)
		}
	}
}
