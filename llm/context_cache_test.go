package llm

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

type countingProjectionStore struct {
	*testArtifactStore
	opens, puts int
}

func (s *countingProjectionStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	s.opens++
	return s.testArtifactStore.Open(ctx, id)
}

func (s *countingProjectionStore) Put(ctx context.Context, blob artifacts.Blob) (artifacts.Ref, error) {
	s.puts++
	return s.testArtifactStore.Put(ctx, blob)
}

func TestProjectionCacheMatchesFreshProjectionAsHistoryGrows(t *testing.T) {
	ctx := context.Background()
	for _, budget := range []int{0, 250, 700, 3000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			cache := &projectionCache{}
			store := newTestArtifactStore()
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "instructions"},
				{Role: messages.MessageRoleInternal, Content: "not projected"},
			}
			for turn := 0; turn < 5; turn++ {
				history = append(history,
					messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("question %d", turn)},
					messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: fmt.Sprint(turn), Name: "lookup", Arguments: `{}`}}},
					messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: fmt.Sprint(turn), ToolName: "lookup", Content: strings.Repeat("payload\n", 400)},
				)
				before := cloneMessages(history)
				got, stats, err := projectMessagesCached(ctx, history, budget, store, true, cache)
				want, wantStats, wantErr := projectMessages(ctx, history, budget, store, true)
				if fmt.Sprint(err) != fmt.Sprint(wantErr) || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(stats, wantStats) {
					t.Fatalf("turn %d cache differs from fresh projection: stats=%+v want=%+v errors=%v / %v", turn, stats, wantStats, err, wantErr)
				}
				if !reflect.DeepEqual(history, before) {
					t.Fatal("projection mutated input history")
				}
				if err == nil && stats.EstimatedTokens != estimateProjectedTokens(got) {
					t.Fatalf("running total=%d, actual=%d", stats.EstimatedTokens, estimateProjectedTokens(got))
				}
				if len(stats.toolSpills) > 0 {
					(&Agent{}).applyDurableToolSpills(history, stats.toolSpills)
					cache.invalidateMessages()
				}
				history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "finished"})
			}
		})
	}
}

func TestProjectionCachesStoredFormsAndSelectedImages(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		content string
		budget  int
	}{
		{"birth preview", strings.Repeat("large\n", 10000), 0},
		{"demoted receipt", strings.Repeat("line\n", 1000), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &countingProjectionStore{testArtifactStore: newTestArtifactStore()}
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "previous"},
				{Role: messages.MessageRoleTool, ToolName: "lookup", ToolCallID: "old", Content: tc.content},
				{Role: messages.MessageRoleUser, Content: "next"},
			}
			cache := &projectionCache{}
			for range 3 {
				got, stats, err := projectMessagesCached(ctx, history, tc.budget, store, false, cache)
				if err != nil {
					t.Fatal(err)
				}
				if stats.CompactedToolResults != 1 || stats.EstimatedTokens != estimateProjectedTokens(got) {
					t.Fatalf("unexpected projection stats: %+v", stats)
				}
			}
			if store.puts != 1 {
				t.Fatalf("stored immutable result %d times, want 1", store.puts)
			}
		})
	}
	store := &countingProjectionStore{testArtifactStore: newTestArtifactStore()}
	ref, err := store.Put(ctx, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "photo.png", Data: []byte("immutable image bytes")})
	if err != nil {
		t.Fatal(err)
	}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref}}}}
	cache := &projectionCache{}
	for range 3 {
		got, stats, err := projectMessagesCached(ctx, history, 0, store, false, cache)
		if err != nil || stats.HydratedImages != 1 || stats.EstimatedTokens != estimateProjectedTokens(got) {
			t.Fatalf("image projection: %+v, %v", stats, err)
		}
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "still looking"})
	}
	if store.opens != 1 {
		t.Fatalf("opened immutable image %d times, want 1", store.opens)
	}
	changed := ref
	changed.Bytes++
	if _, err := cache.hydrateImage(ctx, messages.ContentPart{Type: "image_artifact", Artifact: &changed}, store); err == nil {
		t.Fatal("cached image bypassed mismatched byte-count validation")
	}
}

func TestProjectionMarkerDeltaPreservesRoundingAndSystemPlacement(t *testing.T) {
	for length := 0; length < 9; length++ {
		for _, withSystem := range []bool{false, true} {
			history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "user"}}
			if withSystem {
				history = append(history, messages.ChatMessage{Role: messages.MessageRoleSystem, Content: strings.Repeat("s", length)})
			}
			for markerLen := 1; markerLen < 9; markerLen++ {
				marker := strings.Repeat("m", markerLen)
				_, delta := projectionMarkerTokenDelta(history, marker)
				want := estimateProjectedTokens(addProjectionMarker(cloneMessages(history), marker)) - estimateProjectedTokens(history)
				if delta != want {
					t.Fatalf("system=%v length=%d marker=%d: delta=%d want=%d", withSystem, length, markerLen, delta, want)
				}
			}
		}
	}
}

func TestProjectionPartsCopyOnWriteWithSpareCapacity(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "photo.png", Data: []byte("image")})
	parts := make([]messages.ContentPart, 1, 4)
	parts[0] = messages.ContentPart{Type: "text", Text: "describe photo.png"}
	parts[:cap(parts)][1] = messages.ContentPart{Type: "text", Text: "outside durable length"}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref}}},
		{Role: messages.MessageRoleUser, Parts: parts},
	}
	if _, _, err := projectMessages(ctx, history, 0, store, false); err != nil {
		t.Fatal(err)
	}
	if parts[:cap(parts)][1].Text != "outside durable length" || len(history[1].Parts) != 1 {
		t.Fatal("adding a referenced image wrote into durable slice capacity")
	}
}

func BenchmarkProjectionCachedHistory(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}
			for range n {
				history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", ToolCalls: []messages.ChatMessageToolCall{{ID: "id", Name: "tool", Arguments: `{}`}}})
			}
			cache := &projectionCache{}
			projectMessagesCached(context.Background(), history, 0, nil, false, cache)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := projectMessagesCached(context.Background(), history, 0, nil, false, cache); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSpillActiveUnshrinkableHistory(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}
			tokens := &projectionTokens{}
			tokens.append(history[0])
			for range n {
				msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: "tool", Content: "tiny"}
				history = append(history, msg)
				tokens.append(msg)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				spillActiveToolResults(context.Background(), history, 1, nil, nil, tokens)
			}
		})
	}
}
