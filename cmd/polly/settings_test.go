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
	pin("postReplSet hooks", settingKeysWhere(func(s settingSpec) bool { return s.postReplSet != nil }),
		[]string{"tooltimeout"})
	pin("setWords completions", settingKeysWhere(func(s settingSpec) bool { return s.setWords != nil }),
		[]string{"thinking"})

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

// TestSettingSpecMetadataRoundTrip catches a mispointed, duplicated, or
// cross-row-swapped cfg/metadata accessor pair. Each row's closures run in
// isolation against a fresh Metadata, and the field pairing below is an
// independent statement of which config and metadata field every row owns —
// so a row whose closures touch the wrong field, an extra field, or another
// row's field fails here even when the union over all rows still copies
// every value (which the call sites' per-flag gating does not guarantee).
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

	owns := map[string]struct {
		cfg  func(*Config) any
		meta func(*sessions.Metadata) any
	}{
		"model":         {func(c *Config) any { return c.Model }, func(m *sessions.Metadata) any { return m.Model }},
		"temp":          {func(c *Config) any { return c.Temperature }, func(m *sessions.Metadata) any { return m.Temperature }},
		"maxtokens":     {func(c *Config) any { return c.MaxTokens }, func(m *sessions.Metadata) any { return m.MaxTokens }},
		"maxcontext":    {func(c *Config) any { return c.MaxHistoryTokens }, func(m *sessions.Metadata) any { return m.MaxHistoryTokens }},
		"thinking":      {func(c *Config) any { return c.ThinkingEffort }, func(m *sessions.Metadata) any { return m.ThinkingEffort }},
		"system":        {func(c *Config) any { return c.SystemPrompt }, func(m *sessions.Metadata) any { return m.SystemPrompt }},
		"tooltimeout":   {func(c *Config) any { return c.ToolTimeout }, func(m *sessions.Metadata) any { return m.ToolTimeout }},
		"skilldir":      {func(c *Config) any { return c.SkillDirs }, func(m *sessions.Metadata) any { return m.SkillDirs }},
		"maxiterations": {func(c *Config) any { return c.MaxIterations }, func(m *sessions.Metadata) any { return m.MaxIterations }},
	}

	nonZeroFields := func(v reflect.Value) int {
		count := 0
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if !reflect.DeepEqual(f.Interface(), reflect.Zero(f.Type()).Interface()) {
				count++
			}
		}
		return count
	}

	for _, spec := range settingSpecs {
		if spec.toMeta == nil {
			continue
		}
		own, ok := owns[spec.key]
		if !ok {
			t.Errorf("row %q has metadata closures but no field pairing in this test — add one", spec.key)
			continue
		}
		md := &sessions.Metadata{}
		spec.toMeta(src, md)
		if !reflect.DeepEqual(own.meta(md), own.cfg(src)) {
			t.Errorf("row %q toMeta did not write its own metadata field: md has %v, want %v", spec.key, own.meta(md), own.cfg(src))
		}
		if n := nonZeroFields(reflect.ValueOf(md).Elem()); n != 1 {
			t.Errorf("row %q toMeta touched %d metadata fields, want exactly 1", spec.key, n)
		}
		dst := &Config{}
		spec.fromMeta(dst, md)
		if !reflect.DeepEqual(own.cfg(dst), own.cfg(src)) {
			t.Errorf("row %q fromMeta did not restore its own config field: got %v, want %v", spec.key, own.cfg(dst), own.cfg(src))
		}
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
