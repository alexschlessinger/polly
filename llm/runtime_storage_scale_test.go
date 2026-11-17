package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func testListArtifactsTool(refs []artifacts.Ref) *listArtifactsTool {
	a := &Agent{artifactRefs: make(map[string]artifacts.Ref)}
	for _, ref := range refs {
		a.indexArtifact(ref)
	}
	return &listArtifactsTool{page: a.listArtifactPage}
}

func scaleTranscript(count int) []messages.ChatMessage {
	history := make([]messages.ChatMessage, count)
	for i := range history {
		history[i] = messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("transcript content ", 64)}
	}
	return history
}

func TestRuntimeStorageScaleTranscriptPageAllocations(t *testing.T) {
	// Required durable/model history is allocated before measurement. A page
	// must not render/cache that history, even on the first read or replacement.
	a := &Agent{}
	a.setTranscript(scaleTranscript(16_000))
	tool := &readTranscriptTool{open: a.openTranscript}
	args := map[string]any{"limit": 2}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 4 {
		if _, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("four tiny pages allocated %d bytes for a >18MiB history", allocated)
	}
	t.Logf("four tiny pages: %d allocated bytes; authoritative history payload: %d bytes", after.TotalAlloc-before.TotalAlloc, len(a.transcript)*len(a.transcript[0].Content))
	if len(a.transcript) != 16_000 || a.transcript[15_999].Content != strings.Repeat("transcript content ", 64) {
		t.Fatal("recall truncated the authoritative model/durable history")
	}
}

func TestRuntimeStorageScaleTranscriptStructuredPageAllocations(t *testing.T) {
	large := strings.Repeat("x", 8<<20)
	for _, msg := range []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "first\n"}, {Type: "text", Text: large}, {Type: "text", Text: large}}},
		{Role: messages.MessageRoleAssistant, Content: large + "\n\n" + large, TextBlocks: []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "first\n"}, {Text: large}, {Text: large}}},
	} {
		tool := testReadTranscriptTool([]messages.ChatMessage{msg})
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		page, err := tool.Execute(context.Background(), map[string]any{"limit": 2})
		runtime.ReadMemStats(&after)
		if err != nil || !strings.Contains(page, "2: first") {
			t.Fatalf("structured page: %q, %v", page, err)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
			t.Fatalf("tiny structured page allocated %d bytes by joining unrequested content", allocated)
		}
	}
}

func TestRuntimeStorageScaleArtifactPageOwnershipAndReset(t *testing.T) {
	a := &Agent{artifactRefs: make(map[string]artifacts.Ref)}
	for i := range 10_000 {
		a.indexArtifact(artifacts.Ref{ID: fmt.Sprintf("sha256:%064x", i), Kind: artifacts.KindBinary, Name: "original"})
	}
	refs, total := a.listArtifactPage(51, artifactListPageEntries)
	if total != 10_000 || len(refs) != 50 || cap(refs) != 50 || refs[0].ID != fmt.Sprintf("sha256:%064x", 50) {
		t.Fatalf("page is not bounded or ordered: len=%d cap=%d total=%d", len(refs), cap(refs), total)
	}
	upgrade := refs[0]
	upgrade.Kind, upgrade.Name = artifacts.KindText, "upgraded"
	a.indexArtifact(upgrade)
	if refs[0].Name != "original" {
		t.Fatal("upgrading index mutated owned page")
	}
	refs[1].Name = "caller mutation"
	fresh, _ := a.listArtifactPage(51, 2)
	if fresh[0] != upgrade || fresh[1].Name != "original" {
		t.Fatalf("upgrade moved reference or caller mutated index: %+v", fresh)
	}
	a.resetArtifactIndex(nil)
	if _, ok := a.lookupArtifact(upgrade.ID); ok {
		t.Fatal("reset retained prior authorization")
	}
	if refs, total := a.listArtifactPage(1, 50); len(refs) != 0 || total != 0 {
		t.Fatal("reset retained prior catalog")
	}
}

// joinedTranscript writes the canonical form the plain way, joining each
// message's ModelText, as an oracle independent of transcriptCursor.
func joinedTranscript(history []messages.ChatMessage, recall recallStubs) string {
	var b strings.Builder
	index := 0
	for _, msg := range history {
		if msg.Role == messages.MessageRoleInternal {
			continue
		}
		index++
		fmt.Fprintf(&b, "=== message %d: %s", index, msg.Role)
		if msg.ToolName != "" {
			b.WriteString(" " + msg.ToolName)
		}
		b.WriteString(" ===\n")
		if _, isRecall := recall.stub(msg.ToolName); msg.Role == messages.MessageRoleTool && isRecall {
			b.WriteString("[" + msg.ToolName + " result not rendered; reproducible by calling it again]\n")
			continue
		}
		if content := msg.ModelText(); content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteByte('\n')
			}
		}
		for _, call := range msg.ToolCalls {
			fmt.Fprintf(&b, "[tool call %s %s]\n", call.Name, call.Arguments)
		}
	}
	return b.String()
}

func TestTranscriptStreamingCanonicalParity(t *testing.T) {
	blocks := []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "thinking\n"}, {Text: "answer"}, {Text: ""}, {Text: "last"}}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleInternal, Content: "private"},
		{Role: messages.MessageRoleUser, Content: "hello\r\n世界\n"},
		{Role: messages.MessageRoleAssistant, Content: messages.AnswerText(blocks), TextBlocks: blocks},
		{Role: messages.MessageRoleAssistant, Content: "edited", TextBlocks: blocks},
		{Role: messages.MessageRoleAssistant, TextBlocks: []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "only commentary"}}},
		{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: "a"}, {Type: "image_url"}, {Type: "text", Text: "b\n"}}},
		{Role: messages.MessageRoleAssistant, Content: messages.AnswerText(blocks), TextBlocks: blocks, Parts: []messages.ContentPart{{Type: "text", Text: "ignored"}}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{Name: "bash", Arguments: "{\n  \"command\": \"true\"\n}"}}},
		{Role: messages.MessageRoleTool, ToolName: "read_transcript", Content: "do not nest", ToolCalls: []messages.ChatMessageToolCall{{Name: "also elided"}}},
		{Role: messages.MessageRoleTool, ToolName: "bash", Content: "result"},
	}
	// Cross two message-index digit boundaries, including empty messages.
	for range 110 {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant})
	}
	recall := builtinProjectionTools(false).recall
	want := joinedTranscript(history, recall) // independent full-render oracle
	if got := renderTranscript(history, recall); got != want {
		t.Fatalf("canonical render: %q; want %q", got, want)
	}
	r := newTranscriptReader(context.Background(), history, recall)
	defer r.Close()
	size, err := r.size()
	if err != nil || size != int64(len(want)) {
		t.Fatalf("stream size=%d want=%d err=%v", size, len(want), err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != want {
		t.Fatalf("stream differs from canonical render: %q, %v; want %q", got, err, want)
	}
	tool := testReadTranscriptTool(history)
	for _, offset := range []int{1, 3, 10, 90, 1000} {
		for _, query := range []string{"", "answer", "no match", "世界", "message 10"} {
			got, err := tool.Execute(context.Background(), map[string]any{"offset": offset, "limit": 3, "query": query})
			want, wantErr := tools.PageLines(context.Background(), strings.NewReader(want), "transcript", offset, 3, query)
			if err != wantErr || got != want {
				t.Fatalf("page offset=%d query=%q: %q, %v; want %q, %v", offset, query, got, err, want, wantErr)
			}
		}
	}
}

func TestTranscriptStreamingModelTextParity(t *testing.T) {
	for _, role := range []string{messages.MessageRoleUser, messages.MessageRoleAssistant, messages.MessageRoleTool} {
		for _, blocks := range [][]messages.AssistantText{
			nil, {{Text: ""}}, {{Phase: messages.PhaseCommentary, Text: "comment"}},
			{{Text: "a"}, {Phase: messages.PhaseCommentary, Text: "comment"}, {Text: "b\n"}},
		} {
			for _, content := range []string{"", "replacement", messages.AnswerText(blocks)} {
				for _, partType := range []string{"", "text", "image_url", "image_base64", "image_artifact", "artifact"} {
					msg := messages.ChatMessage{Role: role, Content: content, TextBlocks: blocks, Parts: []messages.ContentPart{{Type: partType, Text: "part"}, {Type: partType, Text: "suffix"}}}
					if got := strings.Join(appendModelText(nil, &msg), ""); got != msg.ModelText() {
						t.Fatalf("streamed model text differs for %+v: %q != %q", msg, got, msg.ModelText())
					}
				}
			}
		}
	}
}

func BenchmarkRuntimeStorageScaleTranscript(b *testing.B) {
	for _, count := range []int{100, 10_000} {
		history := scaleTranscript(count)
		a := &Agent{}
		a.setTranscript(history)
		tool := &readTranscriptTool{open: a.openTranscript}
		for _, mode := range []string{"page", "byte-page", "search-miss", "full-render", "model-serialization"} {
			b.Run(fmt.Sprintf("messages=%d/%s", count, mode), func(b *testing.B) {
				args := map[string]any{"limit": 2}
				switch mode {
				case "byte-page":
					args = map[string]any{"byte_offset": 0}
				case "search-miss":
					args = map[string]any{"query": "absent-needle"}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					switch mode {
					case "full-render":
						_ = renderTranscript(history, nil)
					case "model-serialization":
						if _, err := json.Marshal(&CompletionRequest{Messages: history}); err != nil {
							b.Fatal(err)
						}
					default:
						if _, err := tool.Execute(context.Background(), args); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func BenchmarkRuntimeStorageScaleOversizedRecall(b *testing.B) {
	for _, size := range []int{2 << 20, 16 << 20} {
		text := strings.Repeat("x", size)
		// Parts exercise streaming concatenation too, not just plain Content.
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "text", Text: text}, {Type: "text", Text: "NEEDLE"}}}}
		tool := testReadTranscriptTool(history)
		for _, query := range []string{"", "NEEDLE"} {
			b.Run(fmt.Sprintf("bytes=%d/search=%t", size, query != ""), func(b *testing.B) {
				args := map[string]any{"query": query}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := tool.Execute(context.Background(), args); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkRuntimeStorageScaleArtifactPage(b *testing.B) {
	for _, count := range []int{100, 10_000, 100_000} {
		a := &Agent{artifactRefs: make(map[string]artifacts.Ref)}
		for i := range count {
			a.indexArtifact(artifacts.Ref{ID: fmt.Sprintf("sha256:%064x", i), Kind: artifacts.KindText})
		}
		b.Run(fmt.Sprintf("refs=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				refs, total := a.listArtifactPage(1, artifactListPageEntries)
				if len(refs) != artifactListPageEntries || total != count {
					b.Fatal("unexpected page")
				}
			}
		})
	}
}

func BenchmarkRuntimeStorageScaleWholeAttachment(b *testing.B) {
	for _, size := range []int{1 << 20, 16 << 20} {
		// A zero-copy backing reader isolates requested attachment loading from
		// the in-memory test store's defensive full copy on Open.
		payload := strings.Repeat("x", size)
		ref := artifacts.Ref{ID: "sha256:" + strings.Repeat("b", 64), Kind: artifacts.KindBinary, Bytes: int64(size), MIMEType: "application/pdf"}
		tool := &readArtifactTool{
			lookup: func(string) (artifacts.Ref, bool) { return artifacts.Ref{}, false },
			open: func(context.Context, string) (artifacts.Ref, io.ReadCloser, error) {
				return ref, io.NopCloser(strings.NewReader(payload)), nil
			},
		}
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			args := map[string]any{"id": ref.ID}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := tool.ExecuteOutput(context.Background(), args)
				if err != nil || len(out.Media) != 1 || len(out.Media[0].Data) != size {
					b.Fatalf("attachment: %v", err)
				}
			}
		})
	}
}
