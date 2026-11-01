package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestAgentOverviewExcerptCacheMatchesStyledRows(t *testing.T) {
	text := style.Styled("Styled words with literal "+style.Escape("[brackets]")+" and 中 e\u0301 text.\n", "accent", "bold") +
		style.Styled("│ ", "muted", "") + strings.Repeat("code", 20) + "\n" +
		style.Styled("a long final line with words ", "code", "") + strings.Repeat("tail ", 20)
	var cache agentOverviewExcerptCache
	for _, width := range []int{4, 17, 80} {
		for _, limit := range []int{3, 6, 0, 3} {
			want, _ := transcriptBlockRowsWithImages(text, false, width, nil, false, 0, 0)
			truncated := limit > 0 && len(want) > limit
			if truncated {
				want = want[:limit]
			}
			for range 2 {
				got, clipped := cache.render(text, nil, width, limit)
				if clipped != truncated || !reflect.DeepEqual(got, want) {
					t.Fatalf("width %d, limit %d: cached excerpt differs in text, styles, or clipping", width, limit)
				}
			}
			if limit == 0 && cache.valid {
				t.Fatal("expanded prose retained an unbounded cell cache")
			}
		}
	}
}

func TestAgentOverviewExcerptCacheInvalidation(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	var cache agentOverviewExcerptCache
	text := style.Styled("one two three four five six seven eight", "accent", "")
	first, _ := cache.render(text, nil, 20, 3)
	again, _ := cache.render(text, nil, 20, 3)
	if &first[0][0] != &again[0][0] {
		t.Fatal("unchanged excerpt was rebuilt")
	}
	changed, _ := cache.render(text+" new", nil, 20, 3)
	if &first[0][0] == &changed[0][0] || !strings.Contains(strings.Join(transcriptRowsText(changed), "\n"), "new") {
		t.Fatal("changed source reused the cached excerpt")
	}
	style.Apply(style.Theme{Name: "overview-cache-test", Colors: map[string]string{"accent": "#123abc"}})
	recolored, _ := cache.render(text, nil, 20, 3)
	if recolored[0][0].Style.Fg == first[0][0].Style.Fg || cache.epoch != style.Epoch() {
		t.Fatal("theme epoch did not invalidate resolved cells")
	}
	// Image presence affects fallback row removal even when text is unchanged.
	text = "before\n" + string(style.ImageMarker(0)) + "\ncaption\nafter"
	without, _ := cache.render(text, nil, 80, 6)
	images := []style.Image{{Path: "chart.png", Width: 8, Height: 4}}
	with, _ := cache.render(text, images, 80, 6)
	if len(without) != len(with)+1 || strings.ContainsRune(strings.Join(transcriptRowsText(with), "\n"), style.ImageMarker(0)) {
		t.Fatal("changed image metadata reused stale marker/fallback rows")
	}
	images[0].Version = "changed"
	if cache.images[0].Version == images[0].Version {
		t.Fatal("cache key aliases mutable source images")
	}
	cache.render(text, images, 80, 6)
	if cache.images[0].Version != "changed" {
		t.Fatal("image metadata change did not refresh cache key")
	}
}

func TestAgentOverviewCachePreservesChoicesAndCurrentWork(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt(strings.Repeat("Actual assignment detail.\n", 8))
	m.appendAssistant(strings.Repeat("**Actual update.**\n", 10))
	m.finishAssistantBlock("")
	var first messages.ChatMessageToolCall
	for i := range 5 {
		call := overviewToolStart(m, fmt.Sprint(i), "read_file", fmt.Sprintf(`{"path":"file-%d.go"}`, i))
		if i == 0 {
			first = call
		}
		overviewToolEnd(m, call, fmt.Sprintf("result-%d", i), nil)
	}
	key := m.displayCatalog.toolForCall(first.ID).key
	var cache agentOverviewCache
	for _, state := range []agentOverviewState{
		{},
		{assignmentExpanded: true},
		{updateExpanded: true},
		{tools: map[string]bool{key: true}},
		{tools: map[string]bool{key: false}, disclosures: disclosuresDetailed},
		{assignment: "Current execution assignment", completed: true},
	} {
		for _, width := range []int{17, 80} {
			got := renderAgentOverviewCached(m, width, state, &cache)
			want := renderAgentOverview(m, width, state)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("cached render changed choices or row actions at width %d: %+v", width, state)
			}
		}
	}
	call := overviewToolStart(m, "new", "bash", `{"command":"new-command"}`)
	got := overviewText(renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache))
	if !strings.Contains(got, "new-command") || !strings.Contains(got, "Output pending") {
		t.Fatalf("prose cache hid current work: %s", got)
	}
	overviewToolEnd(m, call, "new-result", nil)
	key = m.displayCatalog.toolForCall(call.ID).key
	got = overviewText(renderAgentOverviewCached(m, 80, agentOverviewState{tools: map[string]bool{key: true}}, &cache))
	if !strings.Contains(got, "new-result") || strings.Contains(got, "Output pending") {
		t.Fatalf("prose cache hid settlement/manual expansion: %s", got)
	}
}

func TestAgentOverviewCurrentAssignmentDoesNotReuseOldAnswer(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt("old assignment")
	m.appendAssistant("old answer")
	m.finishAssistantBlock("")
	var cache agentOverviewCache
	renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
	state := agentOverviewState{assignment: "new assignment", completed: true}
	text := overviewText(renderAgentOverviewCached(m, 80, state, &cache))
	if !strings.Contains(text, "new assignment") || strings.Contains(text, "old answer") || strings.Contains(text, "Latest update") || strings.Contains(text, "Result") {
		t.Fatalf("current assignment inherited old display output: %s", text)
	}
	if cache.update.valid || cache.update.text != "" {
		t.Fatal("omitted update retained its previous task's cache")
	}
	m.appendUserPrompt("new assignment")
	m.appendAssistant("new answer")
	m.finishAssistantBlock("")
	text = overviewText(renderAgentOverviewCached(m, 80, state, &cache))
	if !strings.Contains(text, "new answer") || !strings.Contains(text, "Result") || strings.Contains(text, "old answer") {
		t.Fatalf("matching durable assignment did not restore its answer: %s", text)
	}
}

func overviewLargeUpdateFixture(repeats int) *replModel {
	m := newReplModel()
	m.assignment = "Check the implementation"
	m.transcript = []transcriptEntry{{assistant: true, text: style.Styled(strings.Repeat("An unchanged update with enough repeated words to wrap.\n", repeats), "accent", "bold")}}
	return m
}

func overviewToolCatalogFixture(count int) *replModel {
	m := newReplModel()
	m.assignment = "Check the implementation"
	for i := range count {
		call := messages.ChatMessageToolCall{ID: fmt.Sprint(i), Name: "bash", Arguments: `{"command":"` + strings.Repeat("hidden-argument ", 256) + `"}`}
		if i >= count-agentOverviewRecentTools {
			call.Arguments = fmt.Sprintf(`{"command":"visible-%d"}`, i)
		}
		m.displayCatalog.tools = append(m.displayCatalog.tools, displayTool{key: "tool:" + call.ID, call: call, complete: true, pres: toolPresentation{outcome: toolOutcomeOK}})
	}
	return m
}

func TestAgentOverviewCachedPaintAllocationsIgnoreHiddenUpdate(t *testing.T) {
	allocs := func(repeats int) float64 {
		m := overviewLargeUpdateFixture(repeats)
		var cache agentOverviewCache
		renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
		if len(cache.update.rows) != agentOverviewUpdateRows || cap(cache.update.rows) != agentOverviewUpdateRows {
			t.Fatal("collapsed cache retained hidden rows")
		}
		capacity := 0
		for _, row := range cache.update.rows {
			capacity += cap(row)
		}
		if capacity > 2*80*agentOverviewUpdateRows {
			t.Fatalf("collapsed cache retained hidden cell backing storage: %d cells", capacity)
		}
		return testing.AllocsPerRun(10, func() {
			renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
		})
	}
	short, long := allocs(8), allocs(8000)
	if long > short+10 {
		t.Fatalf("unchanged paint allocations scale with hidden update: short %.0f, long %.0f", short, long)
	}
}

func TestAgentOverviewFormatsOnlySelectedCatalogTools(t *testing.T) {
	allocs := func(count int) float64 {
		m := overviewToolCatalogFixture(count)
		var cache agentOverviewCache
		return testing.AllocsPerRun(5, func() {
			renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
		})
	}
	short, long := allocs(3), allocs(1000)
	// Lightweight identity indexes may grow with history; decoding and
	// formatting every hidden call must not. Leave room for map growth.
	if long > short+128 {
		t.Fatalf("hidden tool arguments formatted during paint: short %.0f, long %.0f", short, long)
	}
	m := overviewToolCatalogFixture(1000)
	tools := agentOverviewTools(m)
	if len(tools) != 1000 || tools[0].row != nil || tools[0].source != &m.displayCatalog.tools[0] {
		t.Fatal("catalog-only descriptors were eagerly materialized")
	}
}

func BenchmarkAgentOverviewCollapsedUpdate(b *testing.B) {
	for _, repeats := range []int{8, 8000} {
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("lines=%d/cached=%v", repeats, cached), func(b *testing.B) {
				m := overviewLargeUpdateFixture(repeats)
				var cache *agentOverviewCache
				if cached {
					cache = &agentOverviewCache{}
				}
				renderAgentOverviewCached(m, 80, agentOverviewState{}, cache)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					renderAgentOverviewCached(m, 80, agentOverviewState{}, cache)
				}
			})
		}
	}
}

func BenchmarkAgentOverviewToolCatalog(b *testing.B) {
	for _, count := range []int{3, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			m := overviewToolCatalogFixture(count)
			var cache agentOverviewCache
			renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
			}
		})
	}
}
