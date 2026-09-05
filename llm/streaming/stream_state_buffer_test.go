package streaming

import (
	"strconv"
	"strings"
	"testing"
)

func TestStreamStateBufferedSnapshots(t *testing.T) {
	state := &StreamState{ResponseContent: "seed", ReasoningContent: "why"}
	state.AppendContent(" text")
	state.AppendReasoning(" now")
	content, reasoning := state.ResponseContent, state.ReasoningContent
	clone := state.Clone()
	state.AppendContent(strings.Repeat("x", 10000))
	state.AppendReasoning(strings.Repeat("y", 10000))
	clone.AppendContent(" clone")
	clone.AppendReasoning(" clone")
	if content != "seed text" || reasoning != "why now" {
		t.Fatalf("saved snapshots changed: %q / %q", content, reasoning)
	}
	if clone.ResponseContent != "seed text clone" || clone.ReasoningContent != "why now clone" {
		t.Fatalf("clone append lost its prefix: %q / %q", clone.ResponseContent, clone.ReasoningContent)
	}
	if state.ResponseContent != content+strings.Repeat("x", 10000) || state.ReasoningContent != reasoning+strings.Repeat("y", 10000) {
		t.Fatal("appending to the clone changed the source")
	}
	state.ResponseContent, state.ReasoningContent = "replacement", "different"
	state.AppendContent(" content")
	state.AppendReasoning(" reasoning")
	if state.ResponseContent != "replacement content" || state.ReasoningContent != "different reasoning" {
		t.Fatalf("appending ignored public field replacement: %q / %q", state.ResponseContent, state.ReasoningContent)
	}
}

func TestStreamStateFragmentedOutputAllocationBound(t *testing.T) {
	const chunk = "12345678901234567890"
	allocations := testing.AllocsPerRun(1, func() {
		state := NewStreamState()
		for range 5000 {
			state.AppendContent(chunk)
			state.AppendReasoning(chunk)
		}
		if len(state.ResponseContent) != 100000 || len(state.ReasoningContent) != 100000 {
			t.Fatal("fragmented stream lost content")
		}
	})
	// Allow ample room for buffer growth while rejecting one prefix allocation
	// per delta, which made fragmented long responses quadratic.
	if allocations > 200 {
		t.Fatalf("100KB fragmented text and reasoning allocated %.0f times, want <= 200", allocations)
	}
}

func BenchmarkStreamStateAppendContent(b *testing.B) {
	const chunk = "12345678901234567890"
	for _, size := range []int{100000, 1000000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				state := NewStreamState()
				for range size / len(chunk) {
					state.AppendContent(chunk)
				}
				if len(state.ResponseContent) != size {
					b.Fatal("incorrect content length")
				}
			}
		})
	}
}
